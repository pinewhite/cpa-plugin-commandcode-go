// Package main implements a CLIProxyAPI provider plugin for Command Code
// (commandcode.ai) that targets the **Go plan** ($1/month).
//
// Why this plugin exists: Command Code exposes three generation endpoints.
//
//	POST /provider/v1/messages          Anthropic Messages      — Pro+ only
//	POST /provider/v1/chat/completions  OpenAI ChatCompletions  — Pro+ only
//	POST /alpha/generate                CLI envelope            — any plan
//
// On the Go plan both /provider/v1 paths return
// {"error":{"code":"upgrade_required"}}, which is what every existing Command
// Code plugin speaks, so none of them work on Go. /alpha/generate is the
// endpoint the `cmd` CLI itself calls and is not plan-gated, but it speaks a
// private Vercel-AI-SDK-shaped envelope rather than OpenAI.
//
// This plugin is the translator: the host hands the executor OpenAI
// chat-completions JSON, the executor wraps it into the /alpha/generate
// envelope, scans the upstream event stream, and rewrites every event into
// OpenAI chat-completion SSE frames.
//
// The host wraps a plugin that declares an Executor capability in
// executorAdapter, which asserts compiled-in interfaces and fuses the plugin on
// a panic (see pluginhost/adapters_executors.go). That is why fused (also
// pointer-form) executables such as CLIProxyAPI are structurally aware of this
// plugin's package: the interface set below is the whole contract.
package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int CommandCodeGoPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void CommandCodeGoPluginFree(void*, size_t);
extern void CommandCodeGoPluginShutdown(void);

*/
import "C"

import (
	"context"
	"encoding/json"
	"sync"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// pluginVersion is overridden at build time:
//
//	go build -ldflags "-X main.pluginVersion=x.y.z"
var pluginVersion = "0.1.0"

// main is required for -buildmode=c-shared. The host never runs it: entry is
// through cliproxy_plugin_init, which the loader resolves with dlsym.
func main() {}

// abiState holds the host function table and the registered plugin instance.
// The host may invoke methods concurrently, so the plugin pointer is swapped
// under a lock on register/reconfigure/shutdown.
var abiState = struct {
	sync.RWMutex
	host   *C.cliproxy_host_api
	plugin *CommandCodeGoPlugin
}{}

// abiRegistration is the plugin.register / plugin.reconfigure response.
type abiRegistration struct {
	SchemaVersion uint32              `json:"schema_version"`
	Metadata      pluginapi.Metadata  `json:"metadata"`
	Capabilities  abiRegistrationCaps `json:"capabilities"`
}

type abiRegistrationCaps struct {
	ModelProvider         bool                         `json:"model_provider"`
	ModelRouter           bool                         `json:"model_router"`
	Executor              bool                         `json:"executor"`
	ManagementAPI         bool                         `json:"management_api,omitempty"`
	ExecutorModelScope    pluginapi.ExecutorModelScope `json:"executor_model_scope"`
	ExecutorInputFormats  []string                     `json:"executor_input_formats,omitempty"`
	ExecutorOutputFormats []string                     `json:"executor_output_formats,omitempty"`
}

type abiIdentifierResponse struct {
	Identifier string `json:"identifier"`
}

// abiExecutorRequest mirrors the host's executor RPC envelope: the SDK request
// is embedded (the host marshals its Go field names) plus the host-stamped
// correlation fields this plugin needs to call back through the host.
type abiExecutorRequest struct {
	pluginapi.ExecutorRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
	StreamID       string `json:"stream_id,omitempty"`
}

type abiEmptyResponse struct{}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	abiState.Lock()
	abiState.host = host
	abiState.Unlock()

	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.CommandCodeGoPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.CommandCodeGoPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.CommandCodeGoPluginShutdown)
	return 0
}

//export CommandCodeGoPluginCall
func CommandCodeGoPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeABIResponse(response, abiErrorEnvelope("invalid_method", "method is required"))
		return 1
	}

	var requestBytes []byte
	if request != nil && requestLen > 0 {
		requestBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}

	raw, errHandle := handleABIMethod(context.Background(), C.GoString(method), requestBytes)
	if errHandle != nil {
		writeABIResponse(response, abiErrorEnvelope("plugin_error", errHandle.Error()))
		return 1
	}
	writeABIResponse(response, raw)
	return 0
}

//export CommandCodeGoPluginFree
func CommandCodeGoPluginFree(ptr unsafe.Pointer, _ C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export CommandCodeGoPluginShutdown
func CommandCodeGoPluginShutdown() {
	abiState.Lock()
	abiState.plugin = nil
	abiState.host = nil
	abiState.Unlock()
}

// handleABIMethod dispatches one RPC call and returns a JSON envelope.
func handleABIMethod(ctx context.Context, method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		return handleRegister(request)
	case pluginabi.MethodPluginQuiesce, pluginabi.MethodPluginShutdown:
		return abiOKEnvelope(abiEmptyResponse{})
	}

	p, errPlugin := currentPlugin()
	if errPlugin != nil {
		return nil, errPlugin
	}

	switch method {
	case pluginabi.MethodManagementRegister:
		var req pluginapi.ManagementRegistrationRequest
		if len(request) > 0 {
			_ = json.Unmarshal(request, &req)
		}
		resp, errCall := p.RegisterManagement(ctx, req)
		return abiOKEnvelopeWithError(resp, errCall)

	case pluginabi.MethodManagementHandle:
		var rpcRequest struct {
			pluginapi.ManagementRequest
			HostCallbackID string `json:"host_callback_id,omitempty"`
		}
		if errUnmarshal := json.Unmarshal(request, &rpcRequest); errUnmarshal != nil {
			return nil, errUnmarshal
		}
		resp, errCall := p.HandleManagement(ctx, rpcRequest.ManagementRequest)
		return abiOKEnvelopeWithError(resp, errCall)

	case pluginabi.MethodExecutorIdentifier:
		return abiOKEnvelope(abiIdentifierResponse{Identifier: p.Identifier()})

	case pluginabi.MethodModelStatic:
		var req pluginapi.StaticModelRequest
		if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
			return nil, errUnmarshal
		}
		resp, errCall := p.StaticModels(ctx, req)
		return abiOKEnvelopeWithError(resp, errCall)

	case pluginabi.MethodModelForAuth:
		var req pluginapi.AuthModelRequest
		if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
			return nil, errUnmarshal
		}
		resp, errCall := p.ModelsForAuth(ctx, req)
		return abiOKEnvelopeWithError(resp, errCall)

	case pluginabi.MethodModelRoute:
		var req pluginapi.ModelRouteRequest
		if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
			return nil, errUnmarshal
		}
		resp, errCall := p.RouteModel(ctx, req)
		return abiOKEnvelopeWithError(resp, errCall)

	case pluginabi.MethodExecutorExecute:
		var rpcRequest abiExecutorRequest
		if errUnmarshal := json.Unmarshal(request, &rpcRequest); errUnmarshal != nil {
			return nil, errUnmarshal
		}
		req := rpcRequest.ExecutorRequest
		req.HTTPClient = hostHTTPClient{callbackID: rpcRequest.HostCallbackID}
		resp, errCall := p.Execute(ctx, req)
		return abiOKEnvelopeWithError(resp, errCall)

	case pluginabi.MethodExecutorExecuteStream:
		var rpcRequest abiExecutorRequest
		if errUnmarshal := json.Unmarshal(request, &rpcRequest); errUnmarshal != nil {
			return nil, errUnmarshal
		}
		req := rpcRequest.ExecutorRequest
		req.HTTPClient = hostHTTPClient{callbackID: rpcRequest.HostCallbackID}
		resp, errCall := p.ExecuteStream(ctx, req)
		if errCall != nil {
			return nil, errCall
		}
		chunks, errDrain := drainChunks(resp.Chunks)
		if errDrain != nil {
			return nil, errDrain
		}
		// The executor RPC is a single request/response round trip, so every
		// chunk is returned in one response rather than pushed over
		// host.stream.emit. Returning them here is the buffered path the host
		// supports (rpc_client_stream.go: len(resp.Chunks) > 0).
		return abiOKEnvelope(rpcExecutorStreamResponse{
			Headers: resp.Headers,
			Chunks:  chunks,
		})

	case pluginabi.MethodExecutorCountTokens:
		var rpcRequest abiExecutorRequest
		if errUnmarshal := json.Unmarshal(request, &rpcRequest); errUnmarshal != nil {
			return nil, errUnmarshal
		}
		req := rpcRequest.ExecutorRequest
		req.HTTPClient = hostHTTPClient{callbackID: rpcRequest.HostCallbackID}
		resp, errCall := p.CountTokens(ctx, req)
		return abiOKEnvelopeWithError(resp, errCall)

	case pluginabi.MethodExecutorHTTPRequest:
		var rpcRequest struct {
			pluginapi.ExecutorHTTPRequest
			HostCallbackID string `json:"host_callback_id,omitempty"`
		}
		if errUnmarshal := json.Unmarshal(request, &rpcRequest); errUnmarshal != nil {
			return nil, errUnmarshal
		}
		req := rpcRequest.ExecutorHTTPRequest
		req.HTTPClient = hostHTTPClient{callbackID: rpcRequest.HostCallbackID}
		resp, errCall := p.HttpRequest(ctx, req)
		return abiOKEnvelopeWithError(resp, errCall)
	}

	// Unimplemented methods are reported as errors so the host never mistakes a
	// missing capability for an empty success.
	return nil, errUnknownMethod(method)
}

// rpcExecutorStreamResponse mirrors pluginhost.rpcExecutorStreamResponse. The
// field names must match its json tags (`headers`, `chunks`) because the host
// decodes this result into that struct.
type rpcExecutorStreamResponse struct {
	Headers map[string][]string                 `json:"headers,omitempty"`
	Chunks  []pluginapi.ExecutorStreamChunk     `json:"chunks,omitempty"`
}

// handleRegister builds (or rebuilds) the plugin from the config YAML and
// returns the capability declaration.
func handleRegister(request []byte) ([]byte, error) {
	var lifecycleRequest struct {
		ConfigYAML []byte `json:"config_yaml"`
	}
	if len(request) > 0 {
		_ = json.Unmarshal(request, &lifecycleRequest)
	}

	p := buildPlugin(lifecycleRequest.ConfigYAML)
	abiState.Lock()
	abiState.plugin = p
	abiState.Unlock()

	registration := abiRegistration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             "Command Code Go (/alpha/generate)",
			Version:          pluginVersion,
			Author:           "local",
			GitHubRepository: "https://github.com/pinewhite/cpa-plugin-commandcode-go",
			ConfigFields: []pluginapi.ConfigField{
				{Name: "api_keys", Type: pluginapi.ConfigFieldTypeArray, Description: "Command Code user_ API keys. Each entry: {key, weight}."},
				{Name: "api_key", Type: pluginapi.ConfigFieldTypeString, Description: "Legacy single API key. Used when api_keys is empty."},
				{Name: "models", Type: pluginapi.ConfigFieldTypeArray, Description: "Model mappings: {alias, name, display_name}."},
				{Name: "base_url", Type: pluginapi.ConfigFieldTypeString, Description: "Command Code API root. Default https://api.commandcode.ai."},
				{Name: "cli_version", Type: pluginapi.ConfigFieldTypeString, Description: "Value for x-command-code-version. Must track the installed cmd CLI."},
				{Name: "project_slug", Type: pluginapi.ConfigFieldTypeString, Description: "Value for the x-project-slug header."},
				{Name: "zdr", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Send x-cmd-zdr: 1 to request zero-data-retention routing."},
				{Name: "working_dir", Type: pluginapi.ConfigFieldTypeString, Description: "Value for config.workingDir in the envelope. Default '.'."},
				{Name: "permission_mode", Type: pluginapi.ConfigFieldTypeString, Description: "Envelope permissionMode. Default 'default'."},
			},
		},
		Capabilities: abiRegistrationCaps{
			ModelProvider: true,
			ModelRouter:   true,
			Executor:      true,
			ManagementAPI: true,
			// Keys live in plugin config rather than host auth records, so the
			// static (non-OAuth) scope is what applies.
			ExecutorModelScope:    pluginapi.ExecutorModelScopeStatic,
			ExecutorInputFormats:  []string{executorFormat},
			ExecutorOutputFormats: []string{executorFormat},
		},
	}
	return abiOKJSON(registration)
}

func currentPlugin() (*CommandCodeGoPlugin, error) {
	abiState.RLock()
	p := abiState.plugin
	abiState.RUnlock()
	if p == nil {
		return nil, errPluginNotRegistered
	}
	return p, nil
}

// drainChunks collects every buffered executor chunk.
func drainChunks(chunks <-chan pluginapi.ExecutorStreamChunk) ([]pluginapi.ExecutorStreamChunk, error) {
	if chunks == nil {
		return nil, nil
	}
	out := make([]pluginapi.ExecutorStreamChunk, 0, 64)
	for chunk := range chunks {
		if chunk.Err != nil {
			return nil, chunk.Err
		}
		out = append(out, chunk)
	}
	return out, nil
}

// writeABIResponse hands a C-allocated copy of raw back to the host. The host
// frees it through the plugin's free_buffer entry point.
func writeABIResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
}

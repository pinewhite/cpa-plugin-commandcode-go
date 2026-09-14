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

static int commandcodego_call_host(cliproxy_host_api* api, const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	return api->call(api->host_ctx, method, request, request_len, response);
}

static void commandcodego_free_host_buffer(cliproxy_host_api* api, void* ptr, size_t len) {
	api->free_buffer(ptr, len);
}
*/
import "C"

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

var (
	errPluginNotRegistered = errors.New("commandcode-go: plugin not registered")
	errHostUnavailable     = errors.New("commandcode-go: host callback unavailable")
)

func errUnknownMethod(method string) error {
	return fmt.Errorf("commandcode-go: unknown method %q", method)
}

// hostHTTPClient routes executor upstream calls back through the host so CLIProxyAPI's
// transport policy, proxy configuration, and request-log capture still apply.
// Each callback carries the host_callback_id stamped into the executor request.
type hostHTTPClient struct {
	callbackID string
}

// nilHostHTTPClient satisfies the interface for code paths that run without a
// host (model discovery, token counting), so a missing host surfaces as an
// explicit error instead of a nil dereference.
type nilHostHTTPClient struct{}

func (nilHostHTTPClient) Do(context.Context, pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	return pluginapi.HTTPResponse{}, errHostUnavailable
}

func (nilHostHTTPClient) DoStream(context.Context, pluginapi.HTTPRequest) (pluginapi.HTTPStreamResponse, error) {
	return pluginapi.HTTPStreamResponse{}, errHostUnavailable
}

// hostHTTPRequest is the payload for host.http.do / host.http.do_stream. The
// host decodes a flat shape (method/url/headers/body at the top level) or a
// nested one; the flat shape is sent.
type hostHTTPRequest struct {
	HostCallbackID string `json:"host_callback_id,omitempty"`
	Method         string `json:"method,omitempty"`
	URL            string `json:"url,omitempty"`
	Headers        http.Header `json:"headers,omitempty"`
	Body           []byte      `json:"body,omitempty"`
}

func (c hostHTTPClient) Do(ctx context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	raw, errCall := callHostRPC(pluginabi.MethodHostHTTPDo, hostHTTPRequest{
		HostCallbackID: c.callbackID,
		Method:         req.Method,
		URL:            req.URL,
		Headers:        req.Headers,
		Body:           req.Body,
	})
	if errCall != nil {
		return pluginapi.HTTPResponse{}, errCall
	}
	var out pluginapi.HTTPResponse
	if errUnmarshal := json.Unmarshal(raw, &out); errUnmarshal != nil {
		return pluginapi.HTTPResponse{}, fmt.Errorf("commandcode-go: decode host.http.do: %w", errUnmarshal)
	}
	return out, nil
}

// DoStream asks the host to perform the request.
//
// The host answers with a stream_id and then drip-feeds the body through
// host.http.stream_read until it reports done; it may instead return buffered
// chunks, so both shapes are handled.
func (c hostHTTPClient) DoStream(ctx context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPStreamResponse, error) {
	raw, errCall := callHostRPC(pluginabi.MethodHostHTTPDoStream, hostHTTPRequest{
		HostCallbackID: c.callbackID,
		Method:         req.Method,
		URL:            req.URL,
		Headers:        req.Headers,
		Body:           req.Body,
	})
	if errCall != nil {
		return pluginapi.HTTPStreamResponse{}, errCall
	}

	var resp struct {
		StatusCode int                          `json:"status_code"`
		Headers    http.Header                  `json:"headers,omitempty"`
		StreamID   string                       `json:"stream_id,omitempty"`
		Chunks     []pluginapi.HTTPStreamChunk  `json:"chunks,omitempty"`
	}
	if errUnmarshal := json.Unmarshal(raw, &resp); errUnmarshal != nil {
		return pluginapi.HTTPStreamResponse{}, fmt.Errorf("commandcode-go: decode host.http.do_stream: %w", errUnmarshal)
	}

	if len(resp.Chunks) > 0 {
		chunks := make(chan pluginapi.HTTPStreamChunk, len(resp.Chunks))
		for _, chunk := range resp.Chunks {
			chunks <- chunk
		}
		if resp.StreamID != "" {
			closeHostStream(ctx, resp.StreamID)
		}
		close(chunks)
		return pluginapi.HTTPStreamResponse{
			StatusCode: resp.StatusCode,
			Headers:    resp.Headers,
			Chunks:     chunks,
		}, nil
	}

	chunks := make(chan pluginapi.HTTPStreamChunk)
	if resp.StreamID == "" {
		close(chunks)
		return pluginapi.HTTPStreamResponse{
			StatusCode: resp.StatusCode,
			Headers:    resp.Headers,
			Chunks:     chunks,
		}, nil
	}

	go func() {
		defer close(chunks)
		defer closeHostStream(ctx, resp.StreamID)
		for {
			if ctx.Err() != nil {
				chunks <- pluginapi.HTTPStreamChunk{Err: ctx.Err()}
				return
			}
			readRaw, errRead := callHostRPC(pluginabi.MethodHostHTTPStreamRead, map[string]string{
				"stream_id": resp.StreamID,
			})
			if errRead != nil {
				chunks <- pluginapi.HTTPStreamChunk{Err: errRead}
				return
			}
			var read struct {
				Payload []byte `json:"payload,omitempty"`
				Error   string `json:"error,omitempty"`
				Done    bool   `json:"done,omitempty"`
			}
			if errDecode := json.Unmarshal(readRaw, &read); errDecode != nil {
				chunks <- pluginapi.HTTPStreamChunk{Err: errDecode}
				return
			}
			if len(read.Payload) > 0 {
				select {
				case <-ctx.Done():
					return
				case chunks <- pluginapi.HTTPStreamChunk{Payload: read.Payload}:
				}
			}
			if read.Error != "" {
				chunks <- pluginapi.HTTPStreamChunk{Err: errors.New(read.Error)}
				return
			}
			if read.Done {
				return
			}
		}
	}()

	return pluginapi.HTTPStreamResponse{
		StatusCode: resp.StatusCode,
		Headers:    resp.Headers,
		Chunks:     chunks,
	}, nil
}

// closeHostStream releases a host stream so its upstream reader is cancelled.
func closeHostStream(ctx context.Context, streamID string) {
	if streamID == "" {
		return
	}
	_, _ = callHostRPC(pluginabi.MethodHostHTTPStreamClose, map[string]string{"stream_id": streamID})
}

// callHostRPC performs one host callback and unwraps the ABI envelope.
func callHostRPC(method string, payload any) (json.RawMessage, error) {
	abiState.RLock()
	host := abiState.host
	abiState.RUnlock()
	if host == nil {
		return nil, errHostUnavailable
	}

	var requestBytes []byte
	if payload != nil {
		raw, errMarshal := json.Marshal(payload)
		if errMarshal != nil {
			return nil, errMarshal
		}
		requestBytes = raw
	}

	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))

	var requestPtr *C.uint8_t
	if len(requestBytes) > 0 {
		requestPtr = (*C.uint8_t)(C.CBytes(requestBytes))
		defer C.free(unsafe.Pointer(requestPtr))
	}

	var response C.cliproxy_buffer
	code := C.commandcodego_call_host(host, cMethod, requestPtr, C.size_t(len(requestBytes)), &response)

	if response.ptr == nil || response.len == 0 {
		if code != 0 {
			return nil, fmt.Errorf("commandcode-go: host callback %s failed with code %d", method, int(code))
		}
		return nil, nil
	}
	raw := C.GoBytes(response.ptr, C.int(response.len))
	C.commandcodego_free_host_buffer(host, response.ptr, response.len)

	var envelope pluginabi.Envelope
	if errUnmarshal := json.Unmarshal(raw, &envelope); errUnmarshal != nil {
		if code != 0 {
			return nil, fmt.Errorf("commandcode-go: host callback %s failed: %s", method, string(raw))
		}
		return nil, fmt.Errorf("commandcode-go: decode host callback %s: %w", method, errUnmarshal)
	}
	if !envelope.OK {
		if envelope.Error != nil {
			if code != 0 {
				return nil, fmt.Errorf("commandcode-go: host callback %s failed: %s", method, envelope.Error.Message)
			}
			return nil, fmt.Errorf("commandcode-go: host callback %s: %s", method, envelope.Error.Message)
		}
		return nil, fmt.Errorf("commandcode-go: host callback %s failed", method)
	}
	if code != 0 {
		return nil, fmt.Errorf("commandcode-go: host callback %s failed with code %d", method, int(code))
	}
	return envelope.Result, nil
}

// ---- ABI envelopes ----

func abiOKEnvelope(v any) ([]byte, error) {
	raw, errMarshal := json.Marshal(v)
	if errMarshal != nil {
		return nil, errMarshal
	}
	return json.Marshal(pluginabi.Envelope{OK: true, Result: raw})
}

// abiOKEnvelopeWithError returns the call error inside a successful envelope
// when the plugin method has a well-defined failure result, and only falls back
// to a transport-level error when marshalling fails.
//
// A StatusCode() carried by callErr is preserved as pluginabi.Error.HTTPStatus so
// the host can classify the downstream response (for example a 403 permission
// failure) instead of reporting a generic 500 server_error.
func abiOKEnvelopeWithError(v any, callErr error) ([]byte, error) {
	if callErr != nil {
		return abiErrorEnvelopeWithStatus("plugin_error", callErr.Error(), statusCodeOf(callErr)), nil
	}
	return abiOKEnvelope(v)
}

// statusCodeOf extracts an HTTP status from err without importing the host's
// clienterror helper, keeping the plugin SDK surface minimal.
func statusCodeOf(err error) int {
	type statusCoder interface {
		StatusCode() int
	}
	var sc statusCoder
	if errors.As(err, &sc) && sc != nil {
		if code := sc.StatusCode(); code > 0 {
			return code
		}
	}
	return 0
}

func abiOKJSON(v any) ([]byte, error) {
	return abiOKEnvelope(v)
}

func abiErrorEnvelope(code, message string) []byte {
	return abiErrorEnvelopeWithStatus(code, message, 0)
}

func abiErrorEnvelopeWithStatus(code, message string, httpStatus int) []byte {
	raw, _ := json.Marshal(pluginabi.Envelope{
		OK:    false,
		Error: &pluginabi.Error{Code: code, Message: message, HTTPStatus: httpStatus},
	})
	return raw
}

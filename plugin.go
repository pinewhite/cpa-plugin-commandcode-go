package main

import (
	"context"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Provider is the provider key this plugin registers its executor under. It must
// not collide with a built-in provider: a native executor with the same key
// would win and this plugin's models would never route here.
const Provider = "cmdc"

const (
	// executorFormat is what the executor consumes and emits. The host
	// translates every other client protocol (claude, gemini, responses) into
	// OpenAI chat-completions before execution and back afterwards using its
	// built-in transformers.
	executorFormat = "openai"

	alphaGeneratePath = "/alpha/generate"

	defaultBaseURL = "https://api.commandcode.ai"
	// defaultCLIVersion must track the installed `command`/`cmd` CLI: Command
	// Code rejects requests carrying a stale x-command-code-version.
	defaultCLIVersion  = "1.53.1"
	defaultProjectSlug = "workspace"
	defaultMaxTokens   = 64000
)

// CommandCodeGoPlugin wires model metadata, routing, and execution.
type CommandCodeGoPlugin struct {
	cfg        *pluginConfig
	models     *ModelProvider
	router     *Router
	translator *Translator
	executor   *Executor
}

// buildPlugin constructs the plugin from the raw plugins.configs.<id> YAML the
// host passes at register/reconfigure time.
func buildPlugin(configYAML []byte) *CommandCodeGoPlugin {
	cfg := parseConfig(configYAML)
	translator := NewTranslator(cfg)
	mgmt := getManagementService(cfg)
	mgmt.syncAuthDir()
	return &CommandCodeGoPlugin{
		cfg:        cfg,
		models:     NewModelProvider(cfg),
		router:     NewRouter(cfg),
		translator: translator,
		executor:   NewExecutor(cfg, translator),
	}
}

// Identifier returns the provider key.
func (p *CommandCodeGoPlugin) Identifier() string { return Provider }

// StaticModels advertises the Command Code catalog.
func (p *CommandCodeGoPlugin) StaticModels(ctx context.Context, req pluginapi.StaticModelRequest) (pluginapi.ModelResponse, error) {
	return p.models.StaticModels(ctx, req)
}

// ModelsForAuth mirrors static models: Command Code credentials live in plugin
// config, so there is no host-side auth record to enumerate per credential.
func (p *CommandCodeGoPlugin) ModelsForAuth(ctx context.Context, req pluginapi.AuthModelRequest) (pluginapi.ModelResponse, error) {
	return p.models.ModelsForAuth(ctx, req)
}

// RouteModel claims Command Code models for this plugin's executor.
func (p *CommandCodeGoPlugin) RouteModel(ctx context.Context, req pluginapi.ModelRouteRequest) (pluginapi.ModelRouteResponse, error) {
	return p.router.RouteModel(ctx, req)
}

// Execute performs a non-streaming completion.
func (p *CommandCodeGoPlugin) Execute(ctx context.Context, req pluginapi.ExecutorRequest) (pluginapi.ExecutorResponse, error) {
	return p.executor.Execute(ctx, req)
}

// ExecuteStream performs a streaming completion.
func (p *CommandCodeGoPlugin) ExecuteStream(ctx context.Context, req pluginapi.ExecutorRequest) (pluginapi.ExecutorStreamResponse, error) {
	return p.executor.ExecuteStream(ctx, req)
}

// CountTokens reports a local token estimate; Command Code has no counting API.
func (p *CommandCodeGoPlugin) CountTokens(ctx context.Context, req pluginapi.ExecutorRequest) (pluginapi.ExecutorResponse, error) {
	return p.executor.CountTokens(ctx, req)
}

// HttpRequest bridges executor-owned raw HTTP through the host client.
func (p *CommandCodeGoPlugin) HttpRequest(ctx context.Context, req pluginapi.ExecutorHTTPRequest) (pluginapi.ExecutorHTTPResponse, error) {
	return p.executor.HttpRequest(ctx, req)
}

var (
	_ pluginapi.ModelProvider    = (*CommandCodeGoPlugin)(nil)
	_ pluginapi.ModelRouter      = (*CommandCodeGoPlugin)(nil)
	_ pluginapi.ProviderExecutor = (*CommandCodeGoPlugin)(nil)
)

// ModelProvider advertises the Command Code catalog.
type ModelProvider struct {
	cfg *pluginConfig
}

func NewModelProvider(cfg *pluginConfig) *ModelProvider { return &ModelProvider{cfg: cfg} }

// catalogEntry is one embedded catalog row.
type catalogEntry struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Context is the model's maximum combined context length.
	Context int64 `json:"ctx"`
}

// models advertises catalog entries based on prefix and bare model settings:
//   - If showPrefix is true (default), advertises `<prefix>/<upstream-id>` (default prefix `cmdc/`).
//   - If includeBareModels is true, also advertises `<upstream-id>`.
//   - If showPrefix is false, advertises only `<upstream-id>`.
func (p *ModelProvider) models() []pluginapi.ModelInfo {
	entries := cachedCatalog()
	overrides := p.cfg.displayNameOverrides()

	showPfx := p.cfg.showPrefix()
	incBare := p.cfg.includeBareModels()
	pfx := p.cfg.prefix()

	out := make([]pluginapi.ModelInfo, 0, len(entries)*2)
	for _, entry := range entries {
		id := strings.TrimSpace(entry.ID)
		if id == "" {
			continue
		}
		display := entry.Name
		if override := overrides[id]; override != "" {
			display = override
		}

		if showPfx {
			prefixedID := pfx + "/" + id
			info := modelInfo(prefixedID, display, entry.Context)
			out = append(out, info)

			if incBare {
				bare := modelInfo(id, display, entry.Context)
				out = append(out, bare)
			}
		} else {
			info := modelInfo(id, display, entry.Context)
			out = append(out, info)
		}
	}
	return out
}

// modelInfo builds the registry entry for one advertised id.
func modelInfo(id string, display string, contextLength int64) pluginapi.ModelInfo {
	if contextLength <= 0 {
		contextLength = 200000
	}
	return pluginapi.ModelInfo{
		ID:                         id,
		Object:                     "model",
		OwnedBy:                    "cmdc",
		Type:                       "chat",
		DisplayName:                display + " (Command Code Go)",
		Name:                       id,
		Description:                display,
		ContextLength:              contextLength,
		MaxCompletionTokens:        defaultMaxTokens,
		SupportedGenerationMethods: []string{"chatCompletions"},
		SupportedInputModalities:   []string{"text"},
		SupportedOutputModalities:  []string{"text"},
		SupportedParameters:        []string{"temperature", "top_p", "max_tokens", "stop", "tools", "reasoning_effort"},
		Thinking: &pluginapi.ThinkingSupport{
			DynamicAllowed: true,
			Levels:         []string{"none", "auto", "low", "medium", "high", "max"},
		},
	}
}

func (p *ModelProvider) StaticModels(context.Context, pluginapi.StaticModelRequest) (pluginapi.ModelResponse, error) {
	return pluginapi.ModelResponse{Provider: Provider, Models: p.models()}, nil
}

func (p *ModelProvider) ModelsForAuth(context.Context, pluginapi.AuthModelRequest) (pluginapi.ModelResponse, error) {
	return pluginapi.ModelResponse{Provider: Provider, Models: p.models()}, nil
}

// RegisterManagement exposes plugin-owned resources and routes.
func (p *CommandCodeGoPlugin) RegisterManagement(ctx context.Context, req pluginapi.ManagementRegistrationRequest) (rpcManagementRegistrationResponse, error) {
	mgmt := getManagementService(p.cfg)
	return mgmt.RegisterManagement(ctx, req)
}

// HandleManagement handles plugin resource and management requests.
func (p *CommandCodeGoPlugin) HandleManagement(ctx context.Context, req pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error) {
	mgmt := getManagementService(p.cfg)
	return mgmt.HandleManagement(ctx, req)
}

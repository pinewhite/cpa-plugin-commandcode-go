package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Executor POSTs the /alpha/generate envelope upstream and rewrites the
// newline-delimited event stream into OpenAI chat-completion SSE frames.
//
// Frame format matters: the host forwards every executor chunk to the client
// verbatim (see pluginhost.executorAdapter.translateExecutorStreamChunks, which
// passes chunks through untouched when the requested format equals the executor
// output format). Chunks must therefore be complete SSE frames — `data: {...}`
// followed by a blank line — not bare JSON.
type Executor struct {
	cfg        *pluginConfig
	translator *Translator
	reqCounter uint64

	wrrMu  sync.Mutex
	wrrCur map[string]int
}

func NewExecutor(cfg *pluginConfig, t *Translator) *Executor {
	if t == nil {
		t = NewTranslator(cfg)
	}
	return &Executor{
		cfg:        cfg,
		translator: t,
		wrrCur:     make(map[string]int),
	}
}

// selectMembersOrder returns the candidate members sorted for this request.
// It uses Smooth Weighted Round-Robin (SWRR) to select the primary member
// according to its configured weight, and appends the remaining members as fallbacks.
func (e *Executor) selectMembersOrder(members []poolMember) []poolMember {
	if len(members) <= 1 {
		return members
	}

	active := make([]poolMember, 0, len(members))
	for _, m := range members {
		if !m.Disabled {
			active = append(active, m)
		}
	}
	if len(active) == 0 {
		active = members
	}
	if len(active) <= 1 {
		return active
	}

	e.wrrMu.Lock()
	defer e.wrrMu.Unlock()

	if e.wrrCur == nil {
		e.wrrCur = make(map[string]int)
	}

	totalWeight := 0
	bestIdx := 0
	maxCurrent := -1 << 31

	for i, m := range active {
		w := m.Weight
		if w <= 0 {
			w = 1
		}
		totalWeight += w
		cur := e.wrrCur[m.Key] + w
		e.wrrCur[m.Key] = cur
		if cur > maxCurrent {
			maxCurrent = cur
			bestIdx = i
		}
	}

	bestKey := active[bestIdx].Key
	e.wrrCur[bestKey] -= totalWeight

	out := make([]poolMember, len(active))
	out[0] = active[bestIdx]
	idx := 1
	for i, m := range active {
		if i != bestIdx {
			out[idx] = m
			idx++
		}
	}
	return out
}

func (e *Executor) Identifier() string { return Provider }

func (e *Executor) endpoint() string {
	return strings.TrimSuffix(e.cfg.baseURL(), "/") + alphaGeneratePath
}

// upstreamHeaders reproduces the CLI's request headers. Command Code rejects
// requests that omit the version/environment/session headers or that present a
// stale x-command-code-version.
func (e *Executor) upstreamHeaders(apiKey string, sessionID string) http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "text/event-stream")
	h.Set("Authorization", "Bearer "+apiKey)
	h.Set("User-Agent", "cli")
	h.Set("x-command-code-version", e.cfg.cliVersion())
	h.Set("x-cli-environment", "production")
	h.Set("x-project-slug", e.cfg.projectSlug())
	h.Set("x-taste-learning", "false")
	h.Set("x-session-id", sessionID)
	if e.cfg.zdrEnabled() {
		h.Set("x-cmd-zdr", "1")
	}
	return h
}

// callOnce performs one upstream call and returns the raw NDJSON body.
//
// /alpha/generate refuses stream:false, so every request is a stream upstream;
// this helper simply buffers it. Failover across the key pool happens here, and
// only for failures that occurred before any successful response.
func (e *Executor) callOnce(ctx context.Context, client pluginapi.HostHTTPClient, payload []byte, stream bool) (int, http.Header, []byte, error) {
	if client == nil {
		return 0, nil, nil, errHostUnavailable
	}
	members := e.cfg.members()
	if len(members) == 0 {
		return 0, nil, nil, statusError{statusCode: http.StatusUnauthorized, msg: missingKeyMsg}
	}

	candidates := e.selectMembersOrder(members)
	var lastErr error
	for _, member := range candidates {
		key := strings.TrimSpace(member.Key)
		if key == "" {
			continue
		}
		req := pluginapi.HTTPRequest{
			Method:  http.MethodPost,
			URL:     e.endpoint(),
			Headers: e.upstreamHeaders(key, newSessionID()),
			Body:    payload,
		}
		resp, errCall := client.Do(ctx, req)
		if errCall != nil {
			lastErr = errCall
			if ctx.Err() != nil {
				return 0, nil, nil, errCall
			}
			continue
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return resp.StatusCode, resp.Headers, resp.Body, nil
		}
		lastErr = statusError{statusCode: resp.StatusCode, body: resp.Body}
		if retryableStatus(resp.StatusCode) && ctx.Err() == nil && len(candidates) > 1 {
			continue
		}
		return resp.StatusCode, resp.Headers, resp.Body, nil
	}
	if lastErr == nil {
		lastErr = statusError{statusCode: http.StatusBadGateway, msg: "commandcode-go: all keys failed"}
	}
	return 0, nil, nil, lastErr
}

// Execute performs a non-streaming completion by consuming the upstream event
// stream and assembling one OpenAI chat-completion response.
func (e *Executor) Execute(ctx context.Context, req pluginapi.ExecutorRequest) (pluginapi.ExecutorResponse, error) {
	envelope := e.translator.buildEnvelope(req.Model, req.Payload)
	status, _, body, errCall := e.callOnce(ctx, req.HTTPClient, envelope, false)
	if errCall != nil {
		return pluginapi.ExecutorResponse{}, errCall
	}
	if status < 200 || status >= 300 {
		return pluginapi.ExecutorResponse{}, statusError{statusCode: status, body: body}
	}
	assembled, errAssemble := assembleNonStreaming(body, req.Model)
	if errAssemble != nil {
		return pluginapi.ExecutorResponse{}, errAssemble
	}
	headers := http.Header{}
	headers.Set("Content-Type", "application/json")
	return pluginapi.ExecutorResponse{Payload: assembled, Headers: headers}, nil
}

// ExecuteStream performs a streaming completion.
//
// The upstream call itself is buffered (the host's executor RPC is a single
// request/response round trip, so chunks are returned in that one response
// anyway); the per-event conversion still happens live as the upstream body is
// scanned, which keeps memory flat and preserves partial output on failure.
func (e *Executor) ExecuteStream(ctx context.Context, req pluginapi.ExecutorRequest) (pluginapi.ExecutorStreamResponse, error) {
	envelope := e.translator.buildEnvelope(req.Model, req.Payload)
	status, _, body, errCall := e.callOnce(ctx, req.HTTPClient, envelope, true)
	if errCall != nil {
		return pluginapi.ExecutorStreamResponse{}, errCall
	}
	if status < 200 || status >= 300 {
		return pluginapi.ExecutorStreamResponse{}, statusError{statusCode: status, body: body}
	}

	frames := convertEventStream(body, req.Model)
	chunks := make(chan pluginapi.ExecutorStreamChunk, len(frames))
	for _, frame := range frames {
		chunks <- pluginapi.ExecutorStreamChunk{Payload: frame}
	}
	close(chunks)

	headers := http.Header{}
	headers.Set("Content-Type", "text/event-stream")
	return pluginapi.ExecutorStreamResponse{Headers: headers, Chunks: chunks}, nil
}

// CountTokens reports a local estimate. Command Code exposes no counting
// endpoint, so the payload length is converted with a 4-bytes-per-token
// heuristic and returned in the OpenAI count-tokens shape.
func (e *Executor) CountTokens(_ context.Context, req pluginapi.ExecutorRequest) (pluginapi.ExecutorResponse, error) {
	estimate := (len(req.Payload) + 3) / 4
	raw, errMarshal := json.Marshal(map[string]any{
		"object":      "token_count",
		"input_tokens": estimate,
		"total_tokens": estimate,
	})
	if errMarshal != nil {
		return pluginapi.ExecutorResponse{}, errMarshal
	}
	return pluginapi.ExecutorResponse{Payload: raw, Headers: http.Header{}}, nil
}

// HttpRequest forwards an executor-owned raw HTTP call through the host client.
func (e *Executor) HttpRequest(ctx context.Context, req pluginapi.ExecutorHTTPRequest) (pluginapi.ExecutorHTTPResponse, error) {
	if req.HTTPClient == nil {
		return pluginapi.ExecutorHTTPResponse{}, errHostUnavailable
	}
	resp, errCall := req.HTTPClient.Do(ctx, pluginapi.HTTPRequest{
		Method:  req.Method,
		URL:     req.URL,
		Headers: req.Headers,
		Body:    req.Body,
	})
	if errCall != nil {
		return pluginapi.ExecutorHTTPResponse{}, errCall
	}
	return pluginapi.ExecutorHTTPResponse{StatusCode: resp.StatusCode, Headers: resp.Headers, Body: resp.Body}, nil
}

// ---- upstream event decoding ----

// upstreamUsage is the token accounting block the endpoint reports.
type upstreamUsage struct {
	InputTokens     int `json:"inputTokens"`
	OutputTokens    int `json:"outputTokens"`
	TotalTokens     int `json:"totalTokens"`
	ReasoningTokens int `json:"reasoningTokens"`
}

// upstreamEvent is one decoded /alpha/generate event.
//
// Text deltas use `text`; tool-argument deltas use `delta`. Both tool ids and
// the tool name arrive on separate events, so all candidates are read.
type upstreamEvent struct {
	Type string `json:"type"`

	ID         string `json:"id"`
	ToolCallID string `json:"toolCallId"`
	ToolName   string `json:"toolName"`

	Text  string `json:"text"`
	Delta string `json:"delta"`

	Input json.RawMessage `json:"input"`

	ErrorText string          `json:"errorText"`
	Error     json.RawMessage `json:"error"`

	FinishReason string         `json:"finishReason"`
	Usage        *upstreamUsage `json:"usage"`
	TotalUsage   *upstreamUsage `json:"totalUsage"`
}

func (ev *upstreamEvent) toolID() string {
	if ev.ToolCallID != "" {
		return ev.ToolCallID
	}
	return ev.ID
}

// convertEventStream scans a complete NDJSON SSE body and returns OpenAI SSE
// frames. Returning a slice (rather than a channel) is deliberate: the host
// collects every chunk into one RPC response anyway, so a channel would only
// add a goroutine and a context-plumbing failure mode.
func convertEventStream(body []byte, requestedModel string) [][]byte {
	state := newStreamState(requestedModel)
	var frames [][]byte

	pending := body
	for {
		idx := bytes.IndexByte(pending, '\n')
		if idx < 0 {
			break
		}
		line := pending[:idx]
		pending = pending[idx+1:]
		frames = append(frames, state.line(line)...)
	}
	if len(bytes.TrimSpace(pending)) > 0 {
		frames = append(frames, state.line(pending)...)
	}
	frames = append(frames, state.finalize()...)
	return frames
}

// line converts a single upstream line into zero or more SSE frames.
func (s *streamState) line(line []byte) [][]byte {
	eventRaw := stripSSEPrefix(line)
	if len(eventRaw) == 0 {
		return nil
	}
	var event upstreamEvent
	if errUnmarshal := jsonUnmarshal(eventRaw, &event); errUnmarshal != nil {
		return nil
	}
	return s.frames(&event)
}

// stripSSEPrefix removes any stacked `data:` prefixes and ignores lines that
// carry no JSON payload (`event:`, ids, comments, keep-alives, `[DONE]`).
func stripSSEPrefix(line []byte) []byte {
	trimmed := bytes.TrimSpace(line)
	for bytes.HasPrefix(trimmed, []byte("data:")) {
		trimmed = bytes.TrimSpace(trimmed[len("data:"):])
	}
	if len(trimmed) == 0 {
		return nil
	}
	if trimmed[0] != '{' {
		return nil
	}
	if bytes.Equal(trimmed, []byte("[DONE]")) {
		return nil
	}
	return trimmed
}

// ---- OpenAI frame assembly ----

type streamState struct {
	model     string
	id        string
	created   int64
	roleSent  bool
	toolIndex int
	toolOpen  map[string]int
	usage     *upstreamUsage
	finished  bool
	doneSent  bool
}

func newStreamState(model string) *streamState {
	return &streamState{
		model:    model,
		id:       "chatcmpl-" + newSessionID(),
		created:  time.Now().Unix(),
		toolOpen: make(map[string]int),
	}
}

type openAIChunk struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []openAIChoice `json:"choices"`
	Usage   *openAIUsage   `json:"usage,omitempty"`
}

type openAIChoice struct {
	Index        int          `json:"index"`
	Delta        *openAIDelta `json:"delta,omitempty"`
	FinishReason *string      `json:"finish_reason"`
}

type openAIDelta struct {
	Role             string            `json:"role,omitempty"`
	Content          *string           `json:"content,omitempty"`
	ReasoningContent *string           `json:"reasoning_content,omitempty"`
	ToolCalls        []openAIToolDelta `json:"tool_calls,omitempty"`
}

type openAIToolFunction struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

type openAIToolDelta struct {
	Index    int                 `json:"index"`
	ID       string              `json:"id,omitempty"`
	Type     string              `json:"type,omitempty"`
	Function *openAIToolFunction `json:"function,omitempty"`
}

type openAIUsage struct {
	PromptTokens           int `json:"prompt_tokens"`
	CompletionTokens       int `json:"completion_tokens"`
	TotalTokens            int `json:"total_tokens"`
	CompletionTokenDetails *struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details,omitempty"`
}

// sseFrame renders one OpenAI chunk as a complete SSE frame.
func sseFrame(payload []byte) []byte {
	out := make([]byte, 0, len(payload)+8)
	out = append(out, "data: "...)
	out = append(out, payload...)
	out = append(out, '\n', '\n')
	return out
}

// chunk builds one OpenAI chunk frame.
func (s *streamState) chunk(delta *openAIDelta, finish *string) []byte {
	raw, errMarshal := json.Marshal(openAIChunk{
		ID:      s.id,
		Object:  "chat.completion.chunk",
		Created: s.created,
		Model:   s.model,
		Choices: []openAIChoice{{Index: 0, Delta: delta, FinishReason: finish}},
	})
	if errMarshal != nil {
		return nil
	}
	return raw
}

// roleFrame emits the initial assistant-role delta exactly once.
func (s *streamState) roleFrame() []byte {
	if s.roleSent {
		return nil
	}
	s.roleSent = true
	return s.chunk(&openAIDelta{Role: "assistant"}, nil)
}

// frames converts one upstream event into zero or more SSE frames.
func (s *streamState) frames(event *upstreamEvent) [][]byte {
	if s.finished {
		// Terminal state was already emitted; late events would corrupt the
		// stream, so only usage bookkeeping is still accepted.
		if event.Usage != nil {
			s.usage = event.Usage
		}
		if event.TotalUsage != nil {
			s.usage = event.TotalUsage
		}
		return nil
	}

	var frames [][]byte
	appendFrame := func(frame []byte) {
		if len(frame) > 0 {
			frames = append(frames, frame)
		}
	}

	switch event.Type {
	case "reasoning-delta":
		if event.Text == "" {
			return nil
		}
		appendFrame(s.roleFrame())
		text := event.Text
		appendFrame(s.chunk(&openAIDelta{ReasoningContent: &text}, nil))

	case "text-delta":
		if event.Text == "" {
			return nil
		}
		appendFrame(s.roleFrame())
		text := event.Text
		appendFrame(s.chunk(&openAIDelta{Content: &text}, nil))

	case "tool-input-start", "tool-call-start":
		id := event.toolID()
		if id == "" {
			id = "call_" + newSessionID()
		}
		index, exists := s.toolOpen[id]
		if !exists {
			index = s.toolIndex
			s.toolIndex++
			s.toolOpen[id] = index
		}
		appendFrame(s.roleFrame())
		appendFrame(s.chunk(&openAIDelta{ToolCalls: []openAIToolDelta{{
			Index:    index,
			ID:       id,
			Type:     "function",
			Function: &openAIToolFunction{Name: event.ToolName, Arguments: ""},
		}}}, nil))

	case "tool-input-delta", "tool-call-delta":
		if event.Delta == "" {
			return nil
		}
		id := event.toolID()
		index, exists := s.toolOpen[id]
		if !exists {
			index = s.toolIndex
			s.toolIndex++
			s.toolOpen[id] = index
			appendFrame(s.roleFrame())
			appendFrame(s.chunk(&openAIDelta{ToolCalls: []openAIToolDelta{{
				Index:    index,
				ID:       id,
				Type:     "function",
				Function: &openAIToolFunction{Name: event.ToolName},
			}}}, nil))
		}
		appendFrame(s.chunk(&openAIDelta{ToolCalls: []openAIToolDelta{{
			Index:    index,
			Function: &openAIToolFunction{Arguments: event.Delta},
		}}}, nil))

	case "tool-call", "tool-result":
		// The complete call was already streamed as start+delta events;
		// re-emitting it would duplicate arguments and break clients that
		// concatenate argument fragments.

	case "finish-step":
		if event.Usage != nil {
			s.usage = event.Usage
		}

	case "finish":
		if event.TotalUsage != nil {
			s.usage = event.TotalUsage
		} else if event.Usage != nil {
			s.usage = event.Usage
		}
		reason := mapFinishReason(event.FinishReason)
		appendFrame(s.roleFrame())
		appendFrame(s.chunk(&openAIDelta{}, &reason))
		s.finished = true

	case "error":
		message := event.ErrorText
		if message == "" && len(event.Error) > 0 {
			message = strings.Trim(strings.TrimSpace(string(event.Error)), `"`)
		}
		if message == "" {
			message = "upstream error"
		}
		reason := "stop"
		text := "\n[upstream error: " + message + "]"
		appendFrame(s.roleFrame())
		appendFrame(s.chunk(&openAIDelta{Content: &text}, &reason))
		s.finished = true
	}

	return frames
}

// finalize emits the usage frame and the terminating [DONE] frame.
func (s *streamState) finalize() [][]byte {
	if s.doneSent {
		return nil
	}
	s.doneSent = true

	var frames [][]byte
	if !s.finished {
		reason := "stop"
		if frame := s.chunk(&openAIDelta{}, &reason); len(frame) > 0 {
			frames = append(frames, frame)
		}
		s.finished = true
	}
	if s.usage != nil {
		usage := &openAIUsage{
			PromptTokens:     s.usage.InputTokens,
			CompletionTokens: s.usage.OutputTokens,
			TotalTokens:      s.usage.TotalTokens,
		}
		if s.usage.ReasoningTokens > 0 {
			usage.CompletionTokenDetails = &struct {
				ReasoningTokens int `json:"reasoning_tokens"`
			}{ReasoningTokens: s.usage.ReasoningTokens}
		}
		raw, errMarshal := json.Marshal(openAIChunk{
			ID:      s.id,
			Object:  "chat.completion.chunk",
			Created: s.created,
			Model:   s.model,
			Choices: []openAIChoice{},
			Usage:   usage,
		})
		if errMarshal == nil {
			frames = append(frames, raw)
		}
	}
	// Note: Host handler ForwardStream automatically sends data: [DONE] on channel close
	return frames
}

// mapFinishReason translates the upstream finish reason to OpenAI vocabulary.
func mapFinishReason(reason string) string {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "length", "max_tokens", "max-tokens":
		return "length"
	case "tool-calls", "tool_calls", "tool_use", "tool-use":
		return "tool_calls"
	case "content-filter", "content_filter":
		return "content_filter"
	default:
		return "stop"
	}
}

// assembleNonStreaming folds a whole event stream into one completion response.
func assembleNonStreaming(body []byte, model string) ([]byte, error) {
	var content strings.Builder
	var reasoning strings.Builder

	type toolAcc struct {
		id   string
		name string
		args strings.Builder
	}
	tools := make(map[string]*toolAcc)
	var toolOrder []string

	finish := "stop"
	var usage *upstreamUsage

	touch := func(id string) *toolAcc {
		acc, ok := tools[id]
		if !ok {
			acc = &toolAcc{id: id}
			tools[id] = acc
			toolOrder = append(toolOrder, id)
		}
		return acc
	}

	for _, line := range bytes.Split(body, []byte("\n")) {
		eventRaw := stripSSEPrefix(line)
		if len(eventRaw) == 0 {
			continue
		}
		var event upstreamEvent
		if errUnmarshal := jsonUnmarshal(eventRaw, &event); errUnmarshal != nil {
			continue
		}
		switch event.Type {
		case "text-delta":
			content.WriteString(event.Text)
		case "reasoning-delta":
			reasoning.WriteString(event.Text)
		case "tool-input-start", "tool-call-start":
			acc := touch(event.toolID())
			if acc.name == "" {
				acc.name = event.ToolName
			}
		case "tool-input-delta", "tool-call-delta":
			touch(event.toolID()).args.WriteString(event.Delta)
		case "tool-call":
			acc := touch(event.toolID())
			if acc.name == "" {
				acc.name = event.ToolName
			}
			if len(event.Input) > 0 && acc.args.Len() == 0 {
				acc.args.Write(event.Input)
			}
		case "finish":
			if event.FinishReason != "" {
				finish = mapFinishReason(event.FinishReason)
			}
			if event.TotalUsage != nil {
				usage = event.TotalUsage
			} else if event.Usage != nil {
				usage = event.Usage
			}
		case "finish-step":
			if event.Usage != nil && usage == nil {
				usage = event.Usage
			}
		}
	}

	message := map[string]any{"role": "assistant", "content": content.String()}
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	if len(toolOrder) > 0 {
		calls := make([]map[string]any, 0, len(toolOrder))
		for _, id := range toolOrder {
			acc := tools[id]
			args := strings.TrimSpace(acc.args.String())
			if args == "" {
				args = "{}"
			}
			calls = append(calls, map[string]any{
				"id":   acc.id,
				"type": "function",
				"function": map[string]any{
					"name":      acc.name,
					"arguments": args,
				},
			})
		}
		message["tool_calls"] = calls
	}

	usagePayload := map[string]any{
		"prompt_tokens":     0,
		"completion_tokens": 0,
		"total_tokens":      0,
	}
	if usage != nil {
		usagePayload["prompt_tokens"] = usage.InputTokens
		usagePayload["completion_tokens"] = usage.OutputTokens
		usagePayload["total_tokens"] = usage.TotalTokens
	}

	response := map[string]any{
		"id":      "chatcmpl-" + newSessionID(),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{{
			"index":         0,
			"message":       message,
			"finish_reason": finish,
		}},
		"usage": usagePayload,
	}
	return json.Marshal(response)
}

func retryableStatus(status int) bool {
	switch status {
	case http.StatusUnauthorized, http.StatusTooManyRequests, http.StatusRequestTimeout, http.StatusBadRequest:
		return true
	}
	return status >= 500
}

// statusError carries an upstream HTTP status through the executor boundary.
type statusError struct {
	statusCode int
	body       []byte
	msg        string
}

// StatusCode exposes the upstream status so the host can classify the failure
// (403 -> permission_error, 429 -> rate_limit_error) instead of collapsing it
// into a generic 500 server_error. pluginhost/rpc_client.go forwards
// pluginabi.Error.HTTPStatus into an rpcError that implements StatusCode(),
// and sdk/api/handlers/handlers_execution.go reads it back through
// clienterror.HTTPStatusFromError.
func (e statusError) StatusCode() int {
	return e.statusCode
}

func (e statusError) Error() string {
	if e.msg != "" {
		return e.msg
	}
	snippet := strings.TrimSpace(string(e.body))
	if len(snippet) > 300 {
		snippet = snippet[:300]
	}
	if snippet == "" {
		return fmt.Sprintf("commandcode-go: upstream returned %d", e.statusCode)
	}
	return fmt.Sprintf("commandcode-go: upstream returned %d: %s", e.statusCode, snippet)
}

const missingKeyMsg = "commandcode-go: no API key configured; set plugins.configs." + Provider + ".api_keys or .api_key"

var errNoModel = errors.New("commandcode-go: no model in request")

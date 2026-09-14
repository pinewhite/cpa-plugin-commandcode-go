package main

import (
	"encoding/json"
	"strings"
)

// Translator builds the /alpha/generate request envelope from the OpenAI
// chat-completions payload the host hands the executor.
//
// The upstream wire format is a Vercel-AI-SDK envelope: messages carry typed
// content parts, tools use `input_schema`, and the model sits inside `params`.
// The shapes here were derived from the shipping `command` CLI bundle
// (buildCommandAuthHeaders / toWireMessages / toWireTools / buildServerConfig)
// and confirmed against the live endpoint.
type Translator struct {
	cfg *pluginConfig
}

func NewTranslator(cfg *pluginConfig) *Translator { return &Translator{cfg: cfg} }

// buildEnvelope converts an OpenAI request body into the upstream envelope.
//
// A body that cannot be parsed is forwarded unchanged rather than replaced with
// a fabricated one: surfacing the upstream's own error is more useful than a
// silent protocol guess.
func (t *Translator) buildEnvelope(model string, body []byte) []byte {
	var in openAIRequest
	if errUnmarshal := jsonUnmarshal(body, &in); errUnmarshal != nil {
		return body
	}
	if strings.TrimSpace(model) == "" {
		model = in.Model
	}

	system, messages := t.convertMessages(in.Messages)

	maxTokens := in.MaxTokens
	if maxTokens <= 0 {
		maxTokens = defaultMaxTokens
	}

	envelope := wireEnvelope{
		Config:         t.cfg.wireConfig(),
		Memory:         "",
		Taste:          "",
		Skills:         "",
		PermissionMode: t.cfg.permissionMode(),
		Params: wireParams{
			Model:    t.cfg.upstreamModel(model),
			System:   system,
			Messages: messages,
			Tools:    convertTools(in.Tools),
			MaxTokens: maxTokens,
			// The endpoint rejects stream:false outright.
			Stream:          true,
			Temperature:     in.Temperature,
			ReasoningEffort: strings.TrimSpace(in.ReasoningEffort),
		},
	}
	raw, errMarshal := json.Marshal(envelope)
	if errMarshal != nil {
		return body
	}
	return raw
}

// ---- OpenAI request shapes (the subset the envelope needs) ----

type openAIRequest struct {
	Model           string          `json:"model"`
	Messages        []openAIMessage `json:"messages"`
	Tools           []openAITool    `json:"tools"`
	MaxTokens       int             `json:"max_tokens"`
	MaxTokensCompat int             `json:"max_completion_tokens"`
	Temperature     *float64        `json:"temperature"`
	ReasoningEffort string          `json:"reasoning_effort"`
}

type openAIMessage struct {
	Role             string           `json:"role"`
	Content          json.RawMessage  `json:"content"`
	ReasoningContent string           `json:"reasoning_content"`
	Reasoning        string           `json:"reasoning"`
	ToolCalls        []openAIToolCall `json:"tool_calls"`
	ToolCallID       string           `json:"tool_call_id"`
}

type openAIToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type openAITool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

// ---- Upstream envelope shapes ----

// wireContentPart is one typed content part.
//
// Field names and shapes mirror the CLI's toWireMessages exactly: an image part
// carries the full `data:<mime>;base64,<payload>` URL in `image` (not the bare
// payload), and a tool-result part repeats the originating tool's name.
type wireContentPart struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	// Image parts: full data URL plus its MIME type.
	Image    string `json:"image,omitempty"`
	MIMEType string `json:"mimeType,omitempty"`
	// Assistant tool-call parts.
	ToolCallID string `json:"toolCallId,omitempty"`
	ToolName   string `json:"toolName,omitempty"`
	Input      any    `json:"input,omitempty"`
	// Tool-result parts.
	Output *wireToolOutput `json:"output,omitempty"`
}

type wireToolOutput struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

type wireMessage struct {
	Role    string            `json:"role"`
	Content []wireContentPart `json:"content"`
}

type wireTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type wireParams struct {
	Model           string            `json:"model"`
	System          []wireContentPart `json:"system,omitempty"`
	Messages        []wireMessage     `json:"messages"`
	Tools           []wireTool        `json:"tools"`
	MaxTokens       int               `json:"max_tokens"`
	Stream          bool              `json:"stream"`
	Temperature     *float64          `json:"temperature,omitempty"`
	ReasoningEffort string            `json:"reasoning_effort,omitempty"`
}

type wireConfig struct {
	WorkingDir    string   `json:"workingDir"`
	Date          string   `json:"date"`
	Environment   string   `json:"environment"`
	Structure     []any    `json:"structure"`
	IsGitRepo     bool     `json:"isGitRepo"`
	CurrentBranch string   `json:"currentBranch"`
	MainBranch    string   `json:"mainBranch"`
	GitStatus     string   `json:"gitStatus"`
	RecentCommits []string `json:"recentCommits"`
}

type wireEnvelope struct {
	Config         wireConfig `json:"config"`
	Memory         string     `json:"memory"`
	Taste          string     `json:"taste"`
	Skills         string     `json:"skills"`
	PermissionMode string     `json:"permissionMode"`
	Params         wireParams `json:"params"`
}

// convertMessages splits the system prompt out and rewrites the remaining
// OpenAI messages into the typed-part wire format.
func (t *Translator) convertMessages(in []openAIMessage) ([]wireContentPart, []wireMessage) {
	var system []wireContentPart
	messages := make([]wireMessage, 0, len(in))

	// Tool results carry the tool's name in addition to its id, and OpenAI tool
	// messages only supply the id — so the id→name map is built from the
	// assistant turns as they are converted.
	toolNames := make(map[string]string)
	replayMode := t.cfg.reasoningReplay()

	for _, message := range in {
		switch strings.ToLower(strings.TrimSpace(message.Role)) {
		case "system", "developer":
			for _, text := range extractTexts(message.Content) {
				system = append(system, wireContentPart{Type: "text", Text: text})
			}

		case "assistant":
			parts := make([]wireContentPart, 0, 4)
			reasoning := extractReasoning(message)

			// If replayMode is "standard" or "both", emit the official wire reasoning part.
			if reasoning != "" && (replayMode == "standard" || replayMode == "both") {
				parts = append(parts, wireContentPart{
					Type: "reasoning",
					Text: reasoning,
				})
			}

			texts := extractTexts(message.Content)

			// If replayMode is "inject" or "both", prepend <thought>...</thought> into the text
			// so reasoning models (DeepSeek, Qwen) actually receive the prior reasoning in context.
			if reasoning != "" && (replayMode == "inject" || replayMode == "both") {
				thoughtBlock := "<thought>\n" + reasoning + "\n</thought>"
				if len(texts) == 0 {
					texts = []string{thoughtBlock}
				} else {
					first := strings.TrimSpace(texts[0])
					if !strings.HasPrefix(first, "<thought>") {
						texts[0] = thoughtBlock + "\n\n" + texts[0]
					}
				}
			}

			for _, text := range texts {
				parts = append(parts, wireContentPart{Type: "text", Text: text})
			}
			for _, call := range message.ToolCalls {
				name := call.Function.Name
				if call.ID != "" {
					toolNames[call.ID] = name
				}
				parts = append(parts, wireContentPart{
					Type:       "tool-call",
					ToolCallID: call.ID,
					ToolName:   name,
					Input:      parseToolArguments(call.Function.Arguments),
				})
			}
			if len(parts) > 0 {
				messages = append(messages, wireMessage{Role: "assistant", Content: parts})
			}

		case "tool":
			name := toolNames[message.ToolCallID]
			if name == "" {
				name = "unknown"
			}
			output := wireToolOutput{Type: "text", Value: rawText(message.Content)}
			messages = append(messages, wireMessage{
				Role: "tool",
				Content: []wireContentPart{{
					Type:       "tool-result",
					ToolCallID: message.ToolCallID,
					ToolName:   name,
					Output:     &output,
				}},
			})

		default: // user
			parts := contentParts(message.Content)
			if len(parts) > 0 {
				messages = append(messages, wireMessage{Role: "user", Content: parts})
			}
		}
	}
	return system, messages
}

// contentParts converts an OpenAI content value (plain string or part array)
// into wire parts. Only base64 data URLs can be transmitted: the plugin never
// fetches remote URLs, so those are preserved as a text reference instead of
// being dropped silently.
func contentParts(raw json.RawMessage) []wireContentPart {
	if len(raw) == 0 {
		return nil
	}
	var text string
	if errUnmarshal := jsonUnmarshal(raw, &text); errUnmarshal == nil {
		if strings.TrimSpace(text) == "" {
			return nil
		}
		return []wireContentPart{{Type: "text", Text: text}}
	}

	var parts []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL struct {
			URL string `json:"url"`
		} `json:"image_url"`
	}
	if errUnmarshal := jsonUnmarshal(raw, &parts); errUnmarshal != nil {
		return nil
	}

	out := make([]wireContentPart, 0, len(parts))
	for _, part := range parts {
		if strings.EqualFold(part.Type, "thinking") || strings.EqualFold(part.Type, "reasoning") {
			continue
		}
		if part.Type == "image_url" {
			url := strings.TrimSpace(part.ImageURL.URL)
			if url == "" {
				continue
			}
			mimeType, data, ok := splitDataURL(url)
			if !ok {
				// The plugin never fetches remote URLs; keep the reference as
				// text so the turn is not silently stripped.
				out = append(out, wireContentPart{Type: "text", Text: "[image: " + url + "]"})
				continue
			}
			// The wire shape carries the full data URL, matching the CLI.
			out = append(out, wireContentPart{
				Type:     "image",
				Image:    "data:" + mimeType + ";base64," + data,
				MIMEType: mimeType,
			})
			continue
		}
		if part.Text != "" {
			out = append(out, wireContentPart{Type: "text", Text: part.Text})
		}
	}
	return out
}

func extractTexts(raw json.RawMessage) []string {
	parts := contentParts(raw)
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if part.Type == "text" && part.Text != "" {
			out = append(out, part.Text)
		}
	}
	return out
}

func extractReasoning(message openAIMessage) string {
	var thoughts []string
	if r := strings.TrimSpace(message.ReasoningContent); r != "" {
		thoughts = append(thoughts, r)
	}
	if r := strings.TrimSpace(message.Reasoning); r != "" && r != message.ReasoningContent {
		thoughts = append(thoughts, r)
	}
	if len(message.Content) > 0 {
		var rawParts []map[string]any
		if err := jsonUnmarshal(message.Content, &rawParts); err == nil {
			for _, p := range rawParts {
				t, _ := p["type"].(string)
				switch strings.ToLower(t) {
				case "thinking":
					if th, ok := p["thinking"].(string); ok && strings.TrimSpace(th) != "" {
						thoughts = append(thoughts, strings.TrimSpace(th))
					} else if txt, ok := p["text"].(string); ok && strings.TrimSpace(txt) != "" {
						thoughts = append(thoughts, strings.TrimSpace(txt))
					}
				case "reasoning":
					if r, ok := p["reasoning"].(string); ok && strings.TrimSpace(r) != "" {
						thoughts = append(thoughts, strings.TrimSpace(r))
					} else if txt, ok := p["text"].(string); ok && strings.TrimSpace(txt) != "" {
						thoughts = append(thoughts, strings.TrimSpace(txt))
					}
				}
			}
		}
	}
	return strings.TrimSpace(strings.Join(thoughts, "\n\n"))
}

// rawText renders a tool result content value as plain text.
func rawText(raw json.RawMessage) string {
	var text string
	if errUnmarshal := jsonUnmarshal(raw, &text); errUnmarshal == nil {
		return text
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if errUnmarshal := jsonUnmarshal(raw, &parts); errUnmarshal == nil {
		var builder strings.Builder
		for _, part := range parts {
			if part.Text == "" {
				continue
			}
			if builder.Len() > 0 {
				builder.WriteString("\n")
			}
			builder.WriteString(part.Text)
		}
		return builder.String()
	}
	return string(raw)
}

// convertTools rewrites OpenAI function tools into the wire tool shape.
func convertTools(in []openAITool) []wireTool {
	out := make([]wireTool, 0, len(in))
	for _, tool := range in {
		name := strings.TrimSpace(tool.Function.Name)
		if name == "" {
			continue
		}
		schema := tool.Function.Parameters
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out = append(out, wireTool{
			Name:        name,
			Description: tool.Function.Description,
			InputSchema: schema,
		})
	}
	return out
}

func parseToolArguments(raw string) any {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return map[string]any{}
	}
	var out any
	if errUnmarshal := jsonUnmarshal([]byte(trimmed), &out); errUnmarshal != nil {
		return map[string]any{}
	}
	return out
}

func splitDataURL(url string) (string, string, bool) {
	rest, ok := strings.CutPrefix(url, "data:")
	if !ok {
		return "", "", false
	}
	meta, data, ok := strings.Cut(rest, ",")
	if !ok {
		return "", "", false
	}
	mimeType := strings.TrimSuffix(meta, ";base64")
	if mimeType == "" {
		mimeType = "image/png"
	}
	return mimeType, data, true
}

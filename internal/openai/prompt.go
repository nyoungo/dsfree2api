package openai

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// ContentToText flattens OpenAI content parts into plain text.
func ContentToText(parts []map[string]any) string {
	var out []string
	for _, p := range parts {
		t, _ := p["type"].(string)
		switch t {
		case "text", "input_text", "output_text":
			if v, ok := p["text"].(string); ok && v != "" {
				out = append(out, v)
			}
		}
	}
	return strings.Join(out, "\n")
}

type PromptOptions struct {
	Tools          []ToolDef
	ToolChoice     json.RawMessage
	Temperature    *float64
	TopP           *float64
	MaxTokens      *int
	Stop           []string
	ResponseFormat *ResponseFormat
	SystemHint     string
}

// BuildPrompt flattens a chat transcript into the single prompt string the
// upstream AIPKit endpoint expects.
func BuildPrompt(messages []ChatMessage, opts PromptOptions) string {
	parts := make([]string, 0, len(messages)+4)
	for _, msg := range messages {
		content := msg.Content.String()
		switch msg.Role {
		case "system", "developer":
			tag := "System"
			if msg.Role == "developer" {
				tag = "Developer"
			}
			if opts.SystemHint != "" {
				content = strings.TrimSpace(content + "\n\n" + opts.SystemHint)
			}
			parts = append(parts, fmt.Sprintf("[%s]\n%s", tag, content))
		case "user":
			parts = append(parts, "[User]\n"+content)
		case "assistant":
			if len(msg.ToolCalls) > 0 {
				raw, _ := json.Marshal(msg.ToolCalls)
				parts = append(parts, fmt.Sprintf("[Assistant]\n%s\n[Tool Calls]\n%s", content, raw))
			} else {
				parts = append(parts, "[Assistant]\n"+content)
			}
		case "tool":
			parts = append(parts, fmt.Sprintf("[Tool Result (id=%s)]\n%s", msg.ToolCallID, content))
		default:
			title := "Message"
			if msg.Role != "" {
				title = strings.ToUpper(msg.Role[:1]) + msg.Role[1:]
			}
			parts = append(parts, fmt.Sprintf("[%s]\n%s", title, content))
		}
	}
	if len(opts.Tools) > 0 {
		parts = append(parts, toolsPrompt(opts.Tools, opts.ToolChoice))
	}
	if tail := samplingPrompt(opts); tail != "" {
		parts = append(parts, tail)
	}
	return strings.Join(parts, "\n\n")
}

func samplingPrompt(opts PromptOptions) string {
	var lines []string
	add := func(format string, a ...any) {
		lines = append(lines, fmt.Sprintf(format, a...))
	}
	if opts.Temperature != nil {
		add("- Temperature: %.3g (0 = most deterministic, 2 = most creative).", *opts.Temperature)
	}
	if opts.TopP != nil {
		add("- Top-p nucleus sampling: %.3g.", *opts.TopP)
	}
	if opts.MaxTokens != nil {
		add("- Hard limit: produce at most %d tokens of output, then stop.", *opts.MaxTokens)
	}
	if len(opts.Stop) > 0 {
		add("- Stop sequences (do not emit them): %s", strings.Join(opts.Stop, " | "))
	}
	if opts.ResponseFormat != nil {
		switch opts.ResponseFormat.Type {
		case "json_object":
			add("- Respond with a single valid JSON object only, no prose, no markdown fences.")
		case "json_schema":
			add("- Respond with a single valid JSON object only, no prose, no markdown fences.")
			// schema 必须进入约束，否则声明的 json_schema 完全没生效，
			// 客户端拿到的对象无法通过它自己的 schema 校验。
			if schema := jsonSchemaConstraint(opts.ResponseFormat.JSONSchema); schema != "" {
				add("- The JSON must validate against this JSON Schema: %s", schema)
			}
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return "=== GENERATION CONSTRAINTS ===\n" + strings.Join(lines, "\n") + "\n=== END GENERATION CONSTRAINTS ==="
}

// jsonSchemaConstraint 取出 response_format.json_schema 里的 schema 主体；
// 结构不是预期形状时退回原始负载，至少让模型看到声明。
func jsonSchemaConstraint(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var fs struct {
		Schema json.RawMessage `json:"schema"`
	}
	if err := json.Unmarshal(raw, &fs); err == nil && len(fs.Schema) > 0 {
		return string(fs.Schema)
	}
	return string(raw)
}

// ResponsesToMessages converts an OpenAI Responses API request into chat
// messages. Besides plain message items it understands function_call and
// function_call_output items so multi-turn tool loops round-trip.
func ResponsesToMessages(req *ResponsesRequest) ([]ChatMessage, error) {
	var out []ChatMessage
	if req.Instructions != "" {
		out = append(out, ChatMessage{Role: "system", Content: MessageContent{Text: req.Instructions}})
	}
	trimmed := strings.TrimSpace(string(req.Input))
	if trimmed == "" {
		return nil, fmt.Errorf("input is required")
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(req.Input, &s); err != nil {
			return nil, fmt.Errorf("invalid input: %w", err)
		}
		out = append(out, ChatMessage{Role: "user", Content: MessageContent{Text: s}})
		return out, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(req.Input, &items); err != nil {
		return nil, fmt.Errorf("invalid input: %w", err)
	}
	for _, raw := range items {
		var probe struct {
			Type string `json:"type"`
			Role string `json:"role"`
		}
		if err := json.Unmarshal(raw, &probe); err != nil {
			continue
		}
		switch probe.Type {
		case "function_call":
			var fc struct {
				CallID    string          `json:"call_id"`
				ID        string          `json:"id"`
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			_ = json.Unmarshal(raw, &fc)
			id := fc.CallID
			if id == "" {
				id = fc.ID
			}
			if id == "" {
				id = "call_" + randomHex(8)
			}
			out = append(out, ChatMessage{Role: "assistant", ToolCalls: []ToolCall{{
				ID: id, Type: "function",
				Function: FunctionCall{Name: fc.Name, Arguments: rawArgText(fc.Arguments)},
			}}})
		case "function_call_output":
			var fco struct {
				CallID string          `json:"call_id"`
				Output json.RawMessage `json:"output"`
			}
			_ = json.Unmarshal(raw, &fco)
			out = append(out, ChatMessage{
				Role:       "tool",
				ToolCallID: fco.CallID,
				Content:    MessageContent{Text: rawArgText(fco.Output)},
			})
		case "message", "":
			role := probe.Role
			if role == "" {
				role = "user"
			}
			var it struct {
				Content json.RawMessage `json:"content"`
			}
			_ = json.Unmarshal(raw, &it)
			content := MessageContent{}
			_ = json.Unmarshal(it.Content, &content)
			out = append(out, ChatMessage{Role: role, Content: content})
		default:
			// reasoning and other item types have no chat representation
		}
	}
	return out, nil
}

// rawArgText renders a Responses field that may arrive either as a JSON string
// or as a bare JSON value into plain text.
func rawArgText(raw json.RawMessage) string {
	trim := bytes.TrimSpace(raw)
	if len(trim) == 0 || string(trim) == "null" {
		return ""
	}
	if trim[0] == '"' {
		var s string
		if json.Unmarshal(trim, &s) == nil {
			return s
		}
		return ""
	}
	return string(trim)
}

func toolsPrompt(tools []ToolDef, toolChoice json.RawMessage) string {
	lines := []string{
		"",
		"=== TOOL INSTRUCTIONS ===",
		"You have access to the following tools. When you need to use a tool, " +
			"you MUST output ONLY a single JSON object in this exact format " +
			"(no markdown, no explanations, no extra text before or after the JSON):",
		"",
		`{"tool_calls": [{"id": "call_xxx", "type": "function", "function": {"name": "tool_name", "arguments": "{\"param\": \"value\"}"}}]}`,
		"",
		"Available tools:",
	}
	for i, tool := range tools {
		fn := tool.Function
		desc := strings.TrimSpace(fn.Description)
		if desc == "" {
			desc = "No description"
		}
		paramDesc := ""
		if len(fn.Parameters) > 0 {
			var schema struct {
				Properties map[string]struct {
					Type string `json:"type"`
				} `json:"properties"`
				Required []string `json:"required"`
			}
			if err := json.Unmarshal(fn.Parameters, &schema); err == nil && len(schema.Properties) > 0 {
				required := map[string]bool{}
				for _, r := range schema.Required {
					required[r] = true
				}
				var pl []string
				for name, p := range schema.Properties {
					ptype := p.Type
					if ptype == "" {
						ptype = "any"
					}
					mark := ""
					if required[name] {
						mark = " (required)"
					}
					pl = append(pl, fmt.Sprintf("    - %s: %s%s", name, ptype, mark))
				}
				// Keep the schema order stable across runs.
				sortStrings(pl)
				paramDesc = "\n" + strings.Join(pl, "\n")
			}
		}
		lines = append(lines, fmt.Sprintf("%d. %s: %s%s", i+1, fn.Name, desc, paramDesc))
	}

	forced := ""
	if len(toolChoice) > 0 {
		var tc struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
			Type string `json:"type"`
			Name string `json:"name"`
		}
		if err := json.Unmarshal(toolChoice, &tc); err == nil && (tc.Type == "function" || tc.Function.Name != "") {
			forced = tc.Function.Name
			if forced == "" {
				forced = tc.Name
			}
		} else if s := strings.Trim(string(toolChoice), `"`); s != "auto" && s != "none" && s != "" && s != "required" && !strings.HasPrefix(s, "{") {
			forced = s
		}
	}
	lines = append(lines, "")
	if forced != "" {
		lines = append(lines,
			fmt.Sprintf("IMPORTANT: You MUST use the tool '%s' for this request. Output ONLY the JSON object above, nothing else.", forced))
	} else {
		lines = append(lines,
			"If no tool is needed, reply normally. If a tool is needed, output ONLY the JSON object above.")
	}
	lines = append(lines, "=== END TOOL INSTRUCTIONS ===", "")
	return strings.Join(lines, "\n")
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

package anthropic

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/nyoungo/dsfree2api/internal/openai"
)

// ToOpenAI converts an Anthropic Messages request into OpenAI-style chat
// messages, function tools and a tool_choice value usable by BuildPrompt.
func ToOpenAI(req *Request) ([]openai.ChatMessage, []openai.ToolDef, json.RawMessage, error) {
	var out []openai.ChatMessage
	if len(req.System) > 0 {
		if s := systemText(req.System); s != "" {
			out = append(out, openai.ChatMessage{Role: "system", Content: openai.MessageContent{Text: s}})
		}
	}
	for _, m := range req.Messages {
		role := m.Role
		if role == "" {
			role = "user"
		}
		if len(m.Content.Blocks) == 0 {
			out = append(out, openai.ChatMessage{Role: role, Content: openai.MessageContent{Text: m.Content.Text}})
			continue
		}
		if role == "assistant" {
			text, calls := splitAssistant(m.Content.Blocks)
			out = append(out, openai.ChatMessage{
				Role: "assistant", Content: openai.MessageContent{Text: text}, ToolCalls: calls,
			})
			continue
		}
		// user (or unknown): tool_result blocks become tool messages, text
		// runs become user messages, preserving block order.
		var run []string
		flush := func() {
			if len(run) > 0 {
				out = append(out, openai.ChatMessage{Role: "user", Content: openai.MessageContent{Text: strings.Join(run, "\n")}})
				run = nil
			}
		}
		for _, b := range m.Content.Blocks {
			switch b.Type {
			case "text":
				if b.Text != "" {
					run = append(run, b.Text)
				}
			case "tool_result":
				flush()
				out = append(out, openai.ChatMessage{
					Role:       "tool",
					ToolCallID: b.ToolUseID,
					Content:    openai.MessageContent{Text: rawToText(b.Content)},
				})
			default:
				// image / thinking / unknown block types are not representable
				// upstream as plain text — dropped.
			}
		}
		flush()
	}
	tools := make([]openai.ToolDef, 0, len(req.Tools))
	for _, t := range req.Tools {
		if t.Name == "" {
			continue
		}
		schema := t.InputSchema
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		tools = append(tools, openai.ToolDef{
			Type:     "function",
			Function: openai.Function{Name: t.Name, Description: t.Description, Parameters: schema},
		})
	}
	var tc json.RawMessage
	if req.ToolChoice != nil {
		tc = toolChoiceToOpenAI(req.ToolChoice)
	}
	return out, tools, tc, nil
}

// ContentFrom builds response content blocks from plain prose plus tool calls
// parsed out of the model output.
func ContentFrom(text string, calls []openai.ToolCall) []Block {
	blocks := make([]Block, 0, len(calls)+1)
	if text != "" {
		blocks = append(blocks, Block{Type: "text", Text: text})
	}
	for _, c := range calls {
		id := c.ID
		if id == "" {
			id = NewID("toolu_")
		}
		input := json.RawMessage("{}")
		if s := strings.TrimSpace(c.Function.Arguments); s != "" && json.Valid([]byte(s)) {
			input = json.RawMessage(s)
		}
		blocks = append(blocks, Block{Type: "tool_use", ID: id, Name: c.Function.Name, Input: input})
	}
	return blocks
}

// StopReasonFor maps the presence of tool calls onto Anthropic stop reasons.
func StopReasonFor(calls []openai.ToolCall) string {
	if len(calls) > 0 {
		return "tool_use"
	}
	return "end_turn"
}

// NewID returns a random hex id with the given prefix (e.g. "toolu_").
func NewID(prefix string) string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

func splitAssistant(blocks []Block) (string, []openai.ToolCall) {
	var texts []string
	var calls []openai.ToolCall
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text != "" {
				texts = append(texts, b.Text)
			}
		case "tool_use":
			args := "{}"
			if len(b.Input) > 0 && string(b.Input) != "null" {
				args = string(b.Input)
			}
			id := b.ID
			if id == "" {
				id = NewID("toolu_")
			}
			calls = append(calls, openai.ToolCall{
				ID: id, Type: "function",
				Function: openai.FunctionCall{Name: b.Name, Arguments: args},
			})
		}
	}
	return strings.Join(texts, "\n"), calls
}

// systemText accepts either a plain string or an array of text blocks.
func systemText(raw json.RawMessage) string {
	raw = trimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s
		}
		return ""
	}
	return rawToText(raw)
}

// rawToText flattens a string or an array of text blocks into plain text.
func rawToText(raw json.RawMessage) string {
	raw = trimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s
		}
		return ""
	}
	if raw[0] == '[' {
		var blocks []Block
		if json.Unmarshal(raw, &blocks) == nil {
			var parts []string
			for _, b := range blocks {
				if b.Type == "text" && b.Text != "" {
					parts = append(parts, b.Text)
				}
			}
			return strings.Join(parts, "\n")
		}
		return ""
	}
	return string(raw)
}

func toolChoiceToOpenAI(tc *ToolChoice) json.RawMessage {
	switch tc.Type {
	case "tool":
		if tc.Name == "" {
			return nil
		}
		raw, _ := json.Marshal(map[string]any{"type": "function", "function": map[string]string{"name": tc.Name}})
		return raw
	case "any":
		return json.RawMessage(`"required"`)
	case "none":
		return json.RawMessage(`"none"`)
	default:
		return json.RawMessage(`"auto"`)
	}
}

func trimSpace(b []byte) []byte { return bytes.TrimSpace(b) }

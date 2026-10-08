package openai

import (
	"encoding/json"
	"strings"
)

// StoredTurn is the minimal state kept to reconstruct context for
// previous_response_id and to serve GET /v1/responses/{id}. It mirrors the
// reference responses_adapter::StoredTurn.
type StoredTurn struct {
	// InputText is the user input of the turn (rebuilt into a user message).
	InputText string
	// Output is the model's output item array, replayed item by item.
	Output []map[string]any
	// Response is the full Response object snapshot.
	Response map[string]any
}

// HistoryMessages replays a stored turn as chat messages: the prior user input
// first, then assistant text / function_call items (consecutive function calls
// merge into one assistant tool_calls message).
func HistoryMessages(turn StoredTurn) []ChatMessage {
	var out []ChatMessage
	if strings.TrimSpace(turn.InputText) != "" {
		out = append(out, ChatMessage{Role: "user", Content: MessageContent{Text: turn.InputText}})
	}
	for _, item := range turn.Output {
		switch stringFieldAny(item, "type") {
		case "message":
			text := outputItemText(item)
			if text == "" {
				continue
			}
			out = append(out, ChatMessage{Role: "assistant", Content: MessageContent{Text: text}})
		case "function_call":
			call := ToolCall{
				ID:   stringFieldAny(item, "call_id", "id"),
				Type: "function",
				Function: FunctionCall{
					Name:      stringFieldAny(item, "name"),
					Arguments: rawAnyText(item["arguments"], "{}"),
				},
			}
			if call.ID == "" {
				call.ID = "call_" + randomHex(8)
			}
			if n := len(out); n > 0 && out[n-1].Role == "assistant" && len(out[n-1].ToolCalls) > 0 {
				out[n-1].ToolCalls = append(out[n-1].ToolCalls, call)
			} else {
				out = append(out, ChatMessage{Role: "assistant", ToolCalls: []ToolCall{call}})
			}
		}
	}
	return out
}

func outputItemText(item map[string]any) string {
	parts, _ := item["content"].([]any)
	var b strings.Builder
	for _, p := range parts {
		m, ok := p.(map[string]any)
		if !ok {
			continue
		}
		if s, ok := m["text"].(string); ok {
			b.WriteString(s)
		}
	}
	return b.String()
}

func stringFieldAny(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// rawAnyText renders a stored field that may be a plain string or a bare JSON
// value into text.
func rawAnyText(v any, def string) string {
	switch t := v.(type) {
	case nil:
		return def
	case string:
		if strings.TrimSpace(t) == "" {
			return def
		}
		return t
	default:
		raw, err := json.Marshal(t)
		if err != nil {
			return def
		}
		return string(raw)
	}
}

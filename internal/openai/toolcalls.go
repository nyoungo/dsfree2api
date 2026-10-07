package openai

import (
	"crypto/rand"
	"encoding/json"
	"strings"
)

// TryParseToolCalls extracts an OpenAI-style tool_calls object from raw model
// output. It mirrors the Python implementation: plain JSON, ```json fences and
// generic ``` fences are all candidates.
//
// Returns the remaining prose plus the parsed tool calls (ok=false when the
// text is ordinary chat).
func TryParseToolCalls(text string) (string, []ToolCall, bool) {
	trimmed := strings.TrimSpace(text)
	var candidates []string
	if strings.HasPrefix(trimmed, "{") {
		candidates = append(candidates, trimmed)
	}
	if strings.Contains(trimmed, "```json") {
		for _, part := range strings.Split(trimmed, "```json")[1:] {
			code := strings.TrimSpace(firstBlock(part))
			if strings.HasPrefix(code, "{") {
				candidates = append(candidates, code)
			}
		}
	} else if strings.Contains(trimmed, "```") {
		for _, part := range strings.Split(trimmed, "```")[1:] {
			code := strings.TrimSpace(firstBlock(part))
			if strings.HasPrefix(code, "{") {
				candidates = append(candidates, code)
			}
		}
	}

	for _, cand := range candidates {
		var payload struct {
			ToolCalls []struct {
				ID       string          `json:"id"`
				Type     string          `json:"type"`
				Function json.RawMessage `json:"function"`
			} `json:"tool_calls"`
		}
		if err := json.Unmarshal([]byte(cand), &payload); err != nil {
			continue
		}
		if payload.ToolCalls == nil {
			continue
		}
		calls := make([]ToolCall, 0, len(payload.ToolCalls))
		for i, tc := range payload.ToolCalls {
			var fn struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			_ = json.Unmarshal(tc.Function, &fn)
			args := "{}"
			if len(fn.Arguments) > 0 {
				var s string
				if err := json.Unmarshal(fn.Arguments, &s); err == nil {
					args = s
				} else {
					// arguments already delivered as an object
					var obj any
					if json.Unmarshal(fn.Arguments, &obj) == nil {
						raw, _ := json.Marshal(obj)
						args = string(raw)
					}
				}
			}
			id := tc.ID
			if id == "" {
				id = "call_" + randomHex(8)
			}
			typ := tc.Type
			if typ == "" {
				typ = "function"
			}
			_ = i
			calls = append(calls, ToolCall{
				ID:   id,
				Type: typ,
				Function: FunctionCall{
					Name:      fn.Name,
					Arguments: args,
				},
			})
		}
		remaining := strings.ReplaceAll(text, cand, "")
		remaining = strings.ReplaceAll(remaining, "```json", "")
		remaining = strings.ReplaceAll(remaining, "```", "")
		return strings.TrimSpace(remaining), calls, true
	}
	return text, nil, false
}

func firstBlock(s string) string {
	if i := strings.Index(s, "```"); i >= 0 {
		return s[:i]
	}
	return s
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "00000000"[:n]
	}
	const hexdigits = "0123456789abcdef"
	out := make([]byte, n*2)
	for i, v := range b {
		out[i*2] = hexdigits[v>>4]
		out[i*2+1] = hexdigits[v&0x0f]
	}
	return string(out)
}

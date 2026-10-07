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
	} else if i := strings.Index(trimmed, `{"tool_calls"`); i > 0 {
		// prose before the tool JSON: start the candidate at the payload
		candidates = append(candidates, strings.TrimSpace(trimmed[i:]))
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
		if !decodeToolPayload(cand, &payload) {
			continue
		}
		// A prose-prefix candidate runs to the end of the text; keep only
		// the payload itself so prose after the JSON survives in remaining.
		if full, ok := jsonPrefix(cand); ok && len(full) < len(cand) {
			cand = full
		}
		if payload.ToolCalls == nil {
			continue
		}
		calls := make([]ToolCall, 0, len(payload.ToolCalls))
		bad := false
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
					// Continuation stitching may have introduced raw control
					// characters into the inner JSON; heal them and close any
					// trailing string/brace the model forgot before the
					// channel cap let it stop.
					s = EscapeControlChars(s)
					args = balanceJSON(s)
					if !json.Valid([]byte(args)) {
						// The model's close sequence may be one brace or
						// quote short, leaving stray closers after the inner
						// object: keep the first complete JSON value and
						// drop the dangling tail.
						if prefix, ok := jsonPrefix(s); ok {
							args = prefix
						}
					}
					if !json.Valid([]byte(args)) {
						// Last resort: the reply leaked a raw quote or got
						// cut mid-escape — re-anchor on the final string
						// literal and rebuild so the client still receives
						// a tool call with whatever content survived.
						if closed, ok := forceCloseJSON(s); ok {
							args = closed
						}
					}
					if !json.Valid([]byte(args)) {
						bad = true
						break
					}
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
		if bad {
			continue
		}
		remaining := strings.ReplaceAll(text, cand, "")
		remaining = strings.ReplaceAll(remaining, "```json", "")
		remaining = strings.ReplaceAll(remaining, "```", "")
		return strings.TrimSpace(remaining), calls, true
	}
	return text, nil, false
}

// decodeToolPayload unmarshals cand, falling back to suffix balancing (the
// model broke out mid-string) and to the first complete JSON value (stray
// closers after a complete object).
func decodeToolPayload(cand string, payload any) bool {
	if err := json.Unmarshal([]byte(cand), payload); err == nil {
		return true
	}
	if balanced := balanceJSON(cand); balanced != cand {
		if err := json.Unmarshal([]byte(balanced), payload); err == nil {
			return true
		}
	}
	if prefix, ok := jsonPrefix(cand); ok {
		if err := json.Unmarshal([]byte(prefix), payload); err == nil {
			return true
		}
	}
	if closed, ok := forceCloseJSON(cand); ok {
		if err := json.Unmarshal([]byte(closed), payload); err == nil {
			return true
		}
	}
	return false
}

// jsonPrefix returns the first complete JSON value in s, ignoring any
// trailing garbage after it.
func jsonPrefix(s string) (string, bool) {
	dec := json.NewDecoder(strings.NewReader(s))
	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil || len(raw) == 0 {
		return "", false
	}
	return string(raw), true
}

func firstBlock(s string) string {
	if i := strings.Index(s, "```"); i >= 0 {
		return s[:i]
	}
	return s
}

// balanceJSON repairs an arguments string the model closed prematurely: a
// trailing unterminated string, a dangling comma/colon or unclosed braces
// (the channel cap sometimes lets a reply stop right before its inner JSON
// closes). It returns the original text untouched when the damage is not a
// simple missing-suffix problem.
func balanceJSON(s string) string {
	if json.Valid([]byte(s)) {
		return s
	}
	var stack []byte
	inString, escaped := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !inString {
			switch c {
			case '"':
				inString = true
			case '{', '[':
				stack = append(stack, c)
			case '}', ']':
				if len(stack) > 0 {
					stack = stack[:len(stack)-1]
				}
			}
			continue
		}
		if escaped {
			escaped = false
			continue
		}
		switch c {
		case '\\':
			escaped = true
		case '"':
			inString = false
		}
	}

	out := s
	// A dangling escape (…\) is a partial escape sequence: drop it.
	if inString && escaped {
		out = out[:len(out)-1]
		escaped = false
	}
	if !inString {
		out = strings.TrimRight(out, " \t\r\n")
		addedValue := false
		for len(out) > 0 {
			last := out[len(out)-1]
			if last == ',' || last == ':' {
				if last == ':' {
					addedValue = true
				}
				out = strings.TrimRight(out[:len(out)-1], " \t\r\n")
				continue
			}
			break
		}
		var trimmed strings.Builder
		trimmed.WriteString(out)
		if addedValue {
			trimmed.WriteString(": \"\"")
		}
		out = trimmed.String()
	}
	var b strings.Builder
	b.Grow(len(out) + len(stack) + 4)
	b.WriteString(out)
	if inString {
		b.WriteByte('"')
	}
	for i := len(stack) - 1; i >= 0; i-- {
		if stack[i] == '{' {
			b.WriteByte('}')
		} else {
			b.WriteByte(']')
		}
	}
	if json.Valid([]byte(b.String())) {
		return b.String()
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

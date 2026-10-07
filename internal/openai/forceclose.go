package openai

import (
	"encoding/json"
	"fmt"
	"strings"
)

// forceCloseJSON repairs a tool-call document the channel cap cut off so
// badly that suffix balancing cannot fix it — e.g. a raw quote leaked
// mid-string and collapsed the structure after it. The fast path reuses
// balanceJSON (append-only damage). Otherwise the rebuild re-enters the last
// opened string literal, escapes every raw quote, stray backslash and
// control character in the dangling tail, then appends the closers the cut
// chopped off. The result is always structurally valid JSON carrying
// whatever content survived to the truncation point, so callers deliver a
// complete tool call with a partial payload instead of raw text the client
// cannot parse. ok=false means no repair validated.
func forceCloseJSON(s string) (string, bool) {
	if b := balanceJSON(s); b != s {
		return b, true
	}

	type anchor struct {
		pos   int
		stack []byte
	}
	var anchors []anchor
	var stack []byte
	inString, escaped := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !inString {
			switch c {
			case '"':
				inString = true
				anchors = append(anchors, anchor{pos: i, stack: append([]byte(nil), stack...)})
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
	if len(anchors) == 0 {
		return "", false
	}

	// Re-anchor on the most recent string literal first: it keeps the most
	// surrounding structure verbatim, and anything further out folds into
	// the escaped tail. Each anchor gets both closing forms.
	const maxAnchors = 512
	start := len(anchors) - maxAnchors
	if start < 0 {
		start = 0
	}
	const quoteByte = byte(34)
	for a := len(anchors) - 1; a >= start; a-- {
		an := anchors[a]
		body := escapeStringBody(s[an.pos+1:])
		for _, isKey := range []bool{false, true} {
			// The key form requires a bare key: a quote in the dangling
			// text means this anchor is garbage (e.g. an invalid value
			// such as 12.), and inventing a nonsense key would silently
			// discard real data instead of rejecting the reply.
			if isKey && strings.IndexByte(body, quoteByte) >= 0 {
				continue
			}
			b := strings.Builder{}
			b.Grow(len(s) + len(body) + len(an.stack) + 8)
			b.WriteString(s[:an.pos+1])
			b.WriteString(body)
			if isKey {
				b.WriteString("\": \"\"")
			} else {
				b.WriteString("\"")
			}
			writeClosers(&b, an.stack)
			if json.Valid([]byte(b.String())) {
				return b.String(), true
			}
		}
	}
	return "", false
}

func writeClosers(b *strings.Builder, stack []byte) {
	for i := len(stack) - 1; i >= 0; i-- {
		if stack[i] == '{' {
			b.WriteByte('}')
		} else {
			b.WriteByte(']')
		}
	}
}

// escapeStringBody makes dangling text safe inside a JSON string literal:
// raw quotes become escaped quotes, backslashes that are not valid JSON
// escapes get doubled, and control characters become the usual two-character
// escape sequences.
func escapeStringBody(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 8)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"':
			b.WriteString(`\"`)
		case c == '\\':
			if j := escapeEndAt(s, i); j > i {
				b.WriteString(s[i : j+1])
				i = j
			} else {
				b.WriteString(`\\`)
			}
		case c < 0x20:
			switch c {
			case '\n':
				b.WriteString(`\n`)
			case '\r':
				b.WriteString(`\r`)
			case '\t':
				b.WriteString(`\t`)
			default:
				fmt.Fprintf(&b, `\u%04x`, c)
			}
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// escapeEndAt reports the index of the last byte of the JSON escape sequence
// starting at the backslash s[i], or i when the sequence is missing,
// incomplete or invalid.
func escapeEndAt(s string, i int) int {
	if i+1 >= len(s) {
		return i
	}
	switch s[i+1] {
	case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
		return i + 1
	case 'u':
		if i+5 < len(s) && isHex4(s[i+2:i+6]) {
			return i + 5
		}
	}
	return i
}

func isHex4(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F') {
			continue
		}
		return false
	}
	return true
}

package upstream

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Event is one translated upstream SSE message.
type Event struct {
	Kind  string // "start" | "delta" | "done"
	Value string
}

const (
	KindStart = "start"
	KindDelta = "delta"
	KindDone  = "done"
)

// readSSE parses a text/event-stream body. Blank lines dispatch the pending
// event, ":" comment lines are ignored.
func readSSE(r io.Reader, handle func(event string, dataLines []string) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	eventType := "message"
	var dataLines []string
	flush := func() error {
		if len(dataLines) == 0 {
			return nil
		}
		ev, dl := eventType, dataLines
		eventType, dataLines = "message", nil
		return handle(ev, dl)
	}

	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			if err := flush(); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		if strings.HasPrefix(line, "event:") {
			eventType = strings.TrimSpace(line[len("event:"):])
			continue
		}
		if strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, strings.TrimSpace(line[len("data:"):]))
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("sse read: %w", err)
	}
	return flush()
}

// translateEvent converts one upstream SSE event into zero or more events for
// the downstream client. It returns an error for upstream failure payloads.
func translateEvent(eventType string, dataLines []string) ([]Event, error) {
	var payload map[string]any
	for _, line := range dataLines {
		if line == "[DONE]" {
			return []Event{{Kind: KindDone}}, nil
		}
		var p map[string]any
		if err := json.Unmarshal([]byte(line), &p); err == nil {
			payload = p
		}
	}
	if payload == nil {
		return nil, nil
	}

	switch eventType {
	case "message_start":
		return []Event{{Kind: KindStart, Value: scalarString(payload["message_id"])}}, nil
	case "done", "complete":
		return []Event{{Kind: KindDone}}, nil
	}

	if eventType == "error" || hasErrorValue(payload) {
		if isQuotaPayload(payload) {
			return nil, newQuota("sse quota exhausted: " + shortJSON(payload))
		}
		if isSessionPayload(payload) {
			return nil, newSession("sse session failed: " + shortJSON(payload))
		}
		return nil, errf("sse error: %s", shortJSON(payload))
	}

	delta := firstTruthy(payload, "delta", "message", "content")
	if delta == nil {
		return nil, nil
	}
	text := deltaText(delta)
	if text == "" {
		return nil, nil
	}
	return []Event{{Kind: KindDelta, Value: text}}, nil
}

func hasErrorValue(payload map[string]any) bool {
	v, ok := payload["error"]
	return ok && v != nil
}

func firstTruthy(payload map[string]any, keys ...string) any {
	for _, k := range keys {
		if v, ok := payload[k]; ok && v != nil && !isFalsy(v) {
			return v
		}
	}
	return nil
}

func isFalsy(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return t == ""
	case bool:
		return !t
	case float64:
		return t == 0
	case map[string]any:
		return len(t) == 0
	case []any:
		return len(t) == 0
	}
	return false
}

func deltaText(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case map[string]any:
		for _, k := range []string{"text", "content", "message", "delta"} {
			if s, ok := t[k].(string); ok && s != "" {
				return s
			}
		}
		raw, err := json.Marshal(t)
		if err != nil {
			return ""
		}
		return string(raw)
	default:
		return fmt.Sprintf("%v", t)
	}
}

func scalarString(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

func shortJSON(payload map[string]any) string {
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Sprintf("%v", payload)
	}
	s := string(raw)
	if len(s) > 400 {
		return s[:400] + "..."
	}
	return s
}

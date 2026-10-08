package openai

import (
	"encoding/json"
	"strings"
	"testing"
)

const (
	asDoc   = `{"tool_calls":[{"id":"call_7f","type":"function","function":{"name":"write_file","arguments":"{\"path\":\"a.txt\",\"content\":\"hello world\"}"}}]}`
	asInner = `{"path":"a.txt","content":"hello world"}`
)

// feedAll streams doc in step-byte fragments and returns the concatenated
// argument deltas plus the first delta that carried id/name.
func feedAll(a *ArgStreamer, doc string, step int) (args string, head ToolArgDelta, headSet bool) {
	var b strings.Builder
	for i := step; i <= len(doc); i += step {
		for _, d := range a.Feed(doc[:i]) {
			if !headSet && (d.ID != "" || d.Name != "") {
				head, headSet = d, true
			}
			b.WriteString(d.Args)
		}
	}
	for _, d := range a.Feed(doc) {
		if !headSet && (d.ID != "" || d.Name != "") {
			head, headSet = d, true
		}
		b.WriteString(d.Args)
	}
	return b.String(), head, headSet
}

func TestArgStreamerFragmentsReassemble(t *testing.T) {
	a := NewArgStreamer()
	got, head, headSet := feedAll(a, asDoc, 7)
	if got != asInner {
		t.Fatalf("args mismatch:\n got %q\nwant %q", got, asInner)
	}
	if !headSet || head.ID != "call_7f" || head.Name != "write_file" {
		t.Fatalf("head delta missing id/name: %+v set=%v", head, headSet)
	}
	if !a.Sent() {
		t.Fatal("Sent() = false after streaming")
	}
	calls := []ToolCall{{ID: "call_7f", Type: "function", Function: FunctionCall{Name: "write_file", Arguments: asInner}}}
	deltas, resend, diverged := a.Finalize(calls)
	if diverged || len(resend) != 0 || len(deltas) != 0 {
		t.Fatalf("clean finalize: deltas=%+v resend=%+v diverged=%v", deltas, resend, diverged)
	}
}

func TestArgStreamerHoldsTrailingComma(t *testing.T) {
	doc := `{"tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{\"a\":1,`
	a := NewArgStreamer()
	got, _, _ := feedAll(a, doc, 3)
	if got != `{"a":1` {
		t.Fatalf("held comma leaked: %q", got)
	}
	// balanceJSON trims the dangling comma and closes the object
	calls := []ToolCall{{ID: "c1", Type: "function", Function: FunctionCall{Name: "f", Arguments: `{"a":1}`}}}
	deltas, resend, diverged := a.Finalize(calls)
	if diverged || len(resend) != 0 {
		t.Fatalf("diverged=%v resend=%+v", diverged, resend)
	}
	var flush strings.Builder
	for _, d := range deltas {
		flush.WriteString(d.Args)
	}
	if got+flush.String() != `{"a":1}` {
		t.Fatalf("finalize flush wrong: streamed=%q flush=%q", got, flush.String())
	}
}

func TestArgStreamerHoldsDanglingBackslash(t *testing.T) {
	// inner content ends with a lone backslash (truncated escape)
	doc := `{"tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{\"path\":\"ab\`
	a := NewArgStreamer()
	got, _, _ := feedAll(a, doc, 100)
	if strings.HasSuffix(got, `\`) {
		t.Fatalf("dangling backslash streamed: %q", got)
	}
	if got != `{"path":"ab` {
		t.Fatalf("unexpected prefix: %q", got)
	}
}

func TestArgStreamerObjectArgumentsResend(t *testing.T) {
	doc := `{"tool_calls":[{"id":"c2","type":"function","function":{"name":"f","arguments":{"a":1}}}]}`
	a := NewArgStreamer()
	got, _, _ := feedAll(a, doc, 5)
	if got != "" {
		t.Fatalf("object-form arguments must not stream: %q", got)
	}
	if a.Sent() {
		t.Fatal("Sent() = true for object-form")
	}
	calls := []ToolCall{{ID: "c2", Type: "function", Function: FunctionCall{Name: "f", Arguments: `{"a":1}`}}}
	deltas, resend, diverged := a.Finalize(calls)
	if diverged || len(deltas) != 0 || len(resend) != 1 {
		t.Fatalf("resend expected: deltas=%+v resend=%+v diverged=%v", deltas, resend, diverged)
	}
	if resend[0].Index == nil || *resend[0].Index != 0 || resend[0].Function.Arguments != `{"a":1}` {
		t.Fatalf("resend entry wrong: %+v", resend[0])
	}
}

func TestArgStreamerDivergedOnMidCut(t *testing.T) {
	doc := `{"tool_calls":[{"id":"c3","type":"function","function":{"name":"f","arguments":"{\"a\":1}"}}]}`
	a := NewArgStreamer()
	feedAll(a, doc, 4)
	calls := []ToolCall{{ID: "c3", Type: "function", Function: FunctionCall{Name: "f", Arguments: `{"z":9}`}}}
	_, _, diverged := a.Finalize(calls)
	if !diverged {
		t.Fatal("mid-document rewrite not reported")
	}
}

func TestArgStreamerMultipleCalls(t *testing.T) {
	doc := `{"tool_calls":[` +
		`{"id":"c1","type":"function","function":{"name":"one","arguments":"{\"x\":1}"}},` +
		`{"id":"c2","type":"function","function":{"name":"two","arguments":"{\"y\":2}"}}]}`
	inner1, inner2 := `{"x":1}`, `{"y":2}`
	a := NewArgStreamer()
	var got0, got1 strings.Builder
	var head0, head1 ToolArgDelta
	for i := 1; i <= len(doc); i++ {
		for _, d := range a.Feed(doc[:i]) {
			if d.ID != "" {
				if d.Index == 0 {
					head0 = d
				} else {
					head1 = d
				}
			}
			if d.Index == 0 {
				got0.WriteString(d.Args)
			} else {
				got1.WriteString(d.Args)
			}
		}
	}
	if got0.String() != inner1 || got1.String() != inner2 {
		t.Fatalf("args: idx0=%q idx1=%q", got0.String(), got1.String())
	}
	if head0.ID != "c1" || head0.Name != "one" || head1.ID != "c2" || head1.Name != "two" {
		t.Fatalf("heads: 0=%+v 1=%+v", head0, head1)
	}
	calls := []ToolCall{
		{ID: "c1", Type: "function", Function: FunctionCall{Name: "one", Arguments: inner1}},
		{ID: "c2", Type: "function", Function: FunctionCall{Name: "two", Arguments: inner2}},
	}
	_, resend, diverged := a.Finalize(calls)
	if diverged || len(resend) != 0 {
		t.Fatalf("multi finalize: resend=%+v diverged=%v", resend, diverged)
	}
}

func TestArgStreamerEscapesControlCharsInInnerJSON(t *testing.T) {
	// raw newline inside the inner JSON string (control char) must be escaped
	// exactly like EscapeControlChars would do at parse time
	doc := "{\"tool_calls\":[{\"id\":\"c4\",\"type\":\"function\",\"function\":{\"name\":\"f\",\"arguments\":\"{\\\"a\\\":\\\"line1\nline2\\\"}\"}}]}"
	a := NewArgStreamer()
	got, _, _ := feedAll(a, doc, 100)
	want := "{\"a\":\"line1\\nline2\"}"
	if got != want {
		t.Fatalf("control char escaping:\n got %q\nwant %q", got, want)
	}
}

func TestArgStreamerIgnoresPlainProse(t *testing.T) {
	a := NewArgStreamer()
	if d := a.Feed("just chatting, no tool here"); len(d) != 0 {
		t.Fatalf("plain prose produced deltas: %+v", d)
	}
	if a.Sent() {
		t.Fatal("Sent() = true on prose")
	}
}

func TestArgStreamerProsePrefix(t *testing.T) {
	// the model opens with a sentence (that mentions the arguments key as
	// words) before the actual tool payload
	prose := "我来写文件，参数放在 \"arguments\": \"值\" 这个字段里。\n\n"
	doc := prose + asDoc
	a := NewArgStreamer()
	got, head, headSet := feedAll(a, doc, 7)
	if got != asInner {
		t.Fatalf("args mismatch:\n got %q\nwant %q", got, asInner)
	}
	if !headSet || head.ID != "call_7f" || head.Name != "write_file" {
		t.Fatalf("head delta missing id/name: %+v set=%v", head, headSet)
	}
	calls := []ToolCall{{ID: "call_7f", Type: "function", Function: FunctionCall{Name: "write_file", Arguments: asInner}}}
	deltas, resend, diverged := a.Finalize(calls)
	if diverged || len(resend) != 0 || len(deltas) != 0 {
		t.Fatalf("clean finalize: deltas=%+v resend=%+v diverged=%v", deltas, resend, diverged)
	}
}

// TestArgStreamerParityWithParseHeal feeds a payload whose inner JSON carries
// an invalid escape (\q) and asserts the streamed text equals the repaired
// parse result — the exact divergence the release fix targets.
func TestArgStreamerParityWithParseHeal(t *testing.T) {
	doc := `{"tool_calls":[{"id":"c9","type":"function","function":{"name":"f","arguments":"{\"path\":\"a.txt\",\"content\":\"x\\qy\"}"}}]}`
	_, calls, ok := TryParseToolCalls(doc)
	if !ok || len(calls) != 1 {
		t.Fatalf("parse failed: ok=%v calls=%+v", ok, calls)
	}
	target := calls[0].Function.Arguments
	if !strings.Contains(target, `x\\qy`) || !json.Valid([]byte(target)) {
		t.Fatalf("healed target invalid: %q", target)
	}
	a := NewArgStreamer()
	got, _, _ := feedAll(a, doc, 3)
	if got != target {
		t.Fatalf("stream/parse divergence:\n stream=%q\n target=%q", got, target)
	}
	deltas, resend, diverged := a.Finalize(calls)
	if diverged || len(resend) != 0 || len(deltas) != 0 {
		t.Fatalf("clean finalize expected: deltas=%+v resend=%+v diverged=%v", deltas, resend, diverged)
	}
}

// 红测试：模型漏写 id 字段时首个 delta 也必须携带非空 id —— 客户端按
// delta 组装 tool_calls，空 id 会在下一轮回放成空 tool_call_id，被上游
// 以 "tool messages must include a non-empty string tool_call_id" 拒绝。
func TestArgStreamerSynthesizesIDWhenModelOmitsIt(t *testing.T) {
	doc := `{"tool_calls":[{"type":"function","function":{"name":"f","arguments":"{\"a\":1}"}}]}`
	a := NewArgStreamer()
	got, head, headSet := feedAll(a, doc, 5)
	if got != `{"a":1}` {
		t.Fatalf("args mismatch:\n got %q\nwant %q", got, `{"a":1}`)
	}
	if !headSet || head.ID == "" {
		t.Fatalf("first delta must carry a non-empty id: %+v set=%v", head, headSet)
	}
	if !strings.HasPrefix(head.ID, "call_") {
		t.Fatalf("synthesized id = %q, want call_ prefix", head.ID)
	}
	_, calls, ok := TryParseToolCalls(doc)
	if !ok || len(calls) != 1 {
		t.Fatalf("parse failed: ok=%v calls=%+v", ok, calls)
	}
	deltas, resend, diverged := a.Finalize(calls)
	if diverged || len(resend) != 0 {
		t.Fatalf("finalize: diverged=%v resend=%+v", diverged, resend)
	}
	for _, d := range deltas {
		if d.ID != "" && d.ID != head.ID {
			t.Fatalf("finalize contradicts the streamed id: %q != %q", d.ID, head.ID)
		}
	}
}

func TestTryParseProsePrefix(t *testing.T) {
	prose := "好的，我这就创建文件。\n\n"
	trailing := "\n\n完成。"
	_, calls, ok := TryParseToolCalls(prose + asDoc + trailing)
	if !ok || len(calls) != 1 || calls[0].Function.Arguments != asInner {
		t.Fatalf("prose prefix parse: ok=%v calls=%+v", ok, calls)
	}
	remaining, _, ok := TryParseToolCalls(prose + asDoc)
	if !ok || strings.TrimSpace(remaining) != strings.TrimSpace(prose) {
		t.Fatalf("prose should survive as remaining: %q ok=%v", remaining, ok)
	}
	if _, _, ok := TryParseToolCalls("纯散文，没有工具调用。"); ok {
		t.Fatal("plain prose must not parse as tool calls")
	}
}

func TestTrimHold(t *testing.T) {
	cases := map[string]string{
		`abc,`:    `abc`,
		`abc:  `:  `abc`,
		`abc\\`:   `abc\\`, // even backslash run stays
		`abc\`:    `abc`,   // lone dangling backslash held
		`abc\n`:   `abc\n`, // complete escape pair stays (backslash+n text)
		`abc":`:   `abc"`,  // colon after closing quote held
		`normal`:  `normal`,
		`abc\r\n`: `abc\r\n`, // literal \r\n escape pair text stays
		"abc\r\n": "abc",     // real CRLF bytes stripped
	}
	for in, want := range cases {
		if got := trimHold(in); got != want {
			t.Errorf("trimHold(%q) = %q, want %q", in, got, want)
		}
	}
}

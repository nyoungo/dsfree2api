package openai

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestToolCallTruncated(t *testing.T) {
	valid := `{"tool_calls": [{"id": "a", "type": "function", "function": {"name": "write", "arguments": "{\"filePath\": \"x\"}"}}]}`
	truncated := valid[:40]
	prose := "the model mentions tool_calls but writes no JSON at all"
	fenced := "```json\n" + truncated

	cases := []struct {
		in   string
		want bool
	}{
		{valid, false},
		{truncated, true},
		{prose, false},
		{fenced, true},
		{"", false},
		{`{"other": true}`, false},
	}
	for _, c := range cases {
		if got := ToolCallTruncated(c.in); got != c.want {
			t.Errorf("ToolCallTruncated(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestEscapeControlChars(t *testing.T) {
	cases := []struct{ in, want string }{
		{`{"a": "line1` + "\n" + `line2"}`, `{"a": "line1\nline2"}`},
		{`{"a": "tab` + "\t" + `x"}`, `{"a": "tab\tx"}`},
		// Escaped sequences inside strings stay as-is.
		{`{"a": "already\nescaped"}`, `{"a": "already\nescaped"}`},
		// Pretty-printed structure newlines outside strings survive.
		{"{\n  \"a\": \"x\"\n}", "{\n  \"a\": \"x\"\n}"},
		// Real newline inside a second string while first had escapes.
		{`{"a": "x\ny", "b": "p` + "\n" + `q"}`, `{"a": "x\ny", "b": "p\nq"}`},
		// Other control characters get \u-escaped.
		{`{"a": "` + string(rune(0x01)) + `"}`, `{"a": "\u0001"}`},
	}
	for _, c := range cases {
		if got := EscapeControlChars(c.in); got != c.want {
			t.Errorf("EscapeControlChars(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// Round-trip: repaired text must be valid JSON with the same value.
	raw := "{\"a\": \"line1\nline2\"}"
	fixed := EscapeControlChars(raw)
	var v struct {
		A string `json:"a"`
	}
	if err := json.Unmarshal([]byte(fixed), &v); err != nil {
		t.Fatalf("repaired JSON invalid: %v", err)
	}
	if v.A != "line1\nline2" {
		t.Fatalf("repaired value = %q", v.A)
	}
}

func TestTrimOverlap(t *testing.T) {
	cases := []struct{ acc, head, want string }{
		{"...fillStyle='#F4", "F4E2B8';draw()", "E2B8';draw()"},
		{"...fillStyle='#F4", "E2B8';draw()", "E2B8';draw()"},
		{"abcdef", "xyz", "xyz"},
		{"abcabcabc", "abcabcTAIL", "TAIL"},
		{"", "anything", "anything"},
	}
	for _, c := range cases {
		if got := TrimOverlap(c.acc, c.head); got != c.want {
			t.Errorf("TrimOverlap(%q, %q) = %q, want %q", c.acc, c.head, got, c.want)
		}
	}
}

func TestEscapeControlsAfterCarriesPrefixState(t *testing.T) {
	prefix := `{"tool_calls": [{"function": {"arguments": "{\"content\": \"abc`
	head := "def\n" + `ghi\""}}]}`
	got := EscapeControlsAfter(prefix, head)
	want := `def\nghi\""}}]}`
	if got != want {
		t.Fatalf("EscapeControlsAfter = %q, want %q", got, want)
	}
	// Without prefix context the same head starts outside a string, so the
	// newline is structure whitespace and must survive.
	if plain := EscapeControlChars(head); plain != head {
		t.Fatalf("EscapeControlChars = %q, want unchanged", plain)
	}
	full := prefix + got
	if !json.Valid([]byte(full)) {
		t.Fatalf("repaired document invalid: %s", full)
	}
}

func TestContinueInstructionMentionsTail(t *testing.T) {
	partial := strings.Repeat("x", 200) + "TAIL80CHARS"
	got := ContinueInstruction(partial, 3, 12)
	if !strings.Contains(got, "3/12") {
		t.Errorf("instruction missing round progress: %s", got)
	}
	if !strings.Contains(got, "TAIL80CHARS") {
		t.Errorf("instruction missing tail: %s", got)
	}
	if strings.Contains(got, strings.Repeat("x", 100)) {
		t.Errorf("instruction embedded too much of the partial")
	}
	if !strings.Contains(got, `\n`) {
		t.Errorf("instruction missing escape-sequence rule")
	}
}

func TestBalanceJSON(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"unterminated content", `{"filePath": "x.html", "content": "<html></html>`, `{"filePath": "x.html", "content": "<html></html>"}`},
		{"trailing comma", `{"a": "b",`, `{"a": "b"}`},
		{"trailing colon", `{"a":`, `{"a": ""}`},
		{"dangling escape", `{"a": "b\`, `{"a": "b"}`},
		{"nested array", `{"a": [1, 2`, `{"a": [1, 2]}`},
		{"already valid", `{"a": "b"}`, `{"a": "b"}`},
		{"comma inside string survives", `{"a": "b,c`, `{"a": "b,c"` + `}`},
	}
	for _, c := range cases {
		if got := balanceJSON(c.in); got != c.want {
			t.Errorf("%s: balanceJSON(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
	// Unfixable damage passes through untouched.
	broken := `{"a": 12.}`
	if got := balanceJSON(broken); got != broken {
		t.Errorf("unfixable: got %q", got)
	}
}

func TestTryParseToolCallsHealsBrokenInnerJSON(t *testing.T) {
	text := `{"tool_calls": [{"id": "a", "type": "function", "function": {"name": "write", "arguments": "{\"content\": \"<html></html>"}}]}`
	_, calls, ok := TryParseToolCalls(text)
	if !ok {
		t.Fatal("expected healed tool call")
	}
	var inner struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(calls[0].Function.Arguments), &inner); err != nil {
		t.Fatalf("healed arguments invalid: %v", err)
	}
	if inner.Content != "<html></html>" {
		t.Fatalf("content = %q", inner.Content)
	}
}

func TestTryParseToolCallsRejectsUnfixableInnerJSON(t *testing.T) {
	text := `{"tool_calls": [{"id": "a", "type": "function", "function": {"name": "write", "arguments": "{\"content\": 12.}"}}]}`
	_, calls, ok := TryParseToolCalls(text)
	if ok {
		t.Fatalf("expected rejection, got calls=%v", calls)
	}
}

func TestRunContinuedStitches(t *testing.T) {
	turns := []string{
		`{"tool_calls": [{"id": "a", "function": {"name": "write", "arguments": "{\"content\": \"part1`,
		// Continuation repeats one char, emits a raw newline mid-string, then
		// closes the inner JSON, the arguments string and the outer document.
		"1\n" + `part2\"}"}}]}`,
	}
	prompts := 0
	var got strings.Builder
	err := RunContinued(context.Background(),
		"first-prompt",
		func(partial string, _ int) string {
			if !strings.Contains(partial, "part1") {
				t.Errorf("cont prompt built from unexpected partial: %s", partial)
			}
			return "continue-prompt"
		},
		3,
		func(_ context.Context, prompt string, emit func(string) error) error {
			prompts++
			idx := prompts - 1
			if idx >= len(turns) {
				t.Fatalf("unexpected extra send #%d", prompts)
			}
			if idx > 0 && prompt != "continue-prompt" {
				t.Errorf("round %d prompt = %q", idx, prompt)
			}
			return emit(turns[idx])
		},
		func(text string) error { got.WriteString(text); return nil },
		func(string, ...any) {},
		func(string, ...any) {},
	)
	if err != nil {
		t.Fatalf("RunContinued: %v", err)
	}
	if prompts != 2 {
		t.Fatalf("prompts = %d, want 2", prompts)
	}
	want := `{"tool_calls": [{"id": "a", "function": {"name": "write", "arguments": "{\"content\": \"part1\npart2\"}"}}]}`
	if got.String() != want {
		t.Fatalf("stitched = %q\nwant      %q", got.String(), want)
	}
	if ToolCallTruncated(got.String()) {
		t.Fatal("stitched reply still truncated")
	}
	_, calls, ok := TryParseToolCalls(got.String())
	if !ok {
		t.Fatalf("TryParseToolCalls failed on stitched reply")
	}
	var inner struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(calls[0].Function.Arguments), &inner); err != nil {
		t.Fatalf("arguments JSON invalid: %v", err)
	}
	if inner.Content != "part1\npart2" {
		t.Fatalf("content = %q", inner.Content)
	}
}

func TestRunContinuedPassthroughWithoutCont(t *testing.T) {
	prompts := 0
	var got strings.Builder
	err := RunContinued(context.Background(), "p", nil, 6,
		func(_ context.Context, prompt string, emit func(string) error) error {
			prompts++
			return emit(`{"tool_calls": [{"trunc`)
		},
		func(text string) error { got.WriteString(text); return nil },
		func(string, ...any) {}, func(string, ...any) {},
	)
	if err != nil {
		t.Fatalf("RunContinued: %v", err)
	}
	if prompts != 1 {
		t.Fatalf("prompts = %d, want 1", prompts)
	}
	if !ToolCallTruncated(got.String()) {
		t.Fatal("passthrough should keep the truncated reply")
	}
}

func TestRunContinuedContinuationFailureKeepsPartial(t *testing.T) {
	prompts := 0
	warns := 0
	var got strings.Builder
	err := RunContinued(context.Background(), "first",
		func(string, int) string { return "next" }, 3,
		func(_ context.Context, _ string, emit func(string) error) error {
			prompts++
			if prompts == 1 {
				return emit(`{"tool_calls": [{"trunc`)
			}
			return errors.New("boom")
		},
		func(text string) error { got.WriteString(text); return nil },
		func(string, ...any) {},
		func(string, ...any) { warns++ },
	)
	if err != nil {
		t.Fatalf("RunContinued returned error despite partial text: %v", err)
	}
	if warns != 5 {
		t.Fatalf("warns = %d, want 5 (3 retry notices + turn failure + exhaustion)", warns)
	}
	if got.String() != `{"tool_calls": [{"trunc` {
		t.Fatalf("got = %q", got.String())
	}
}

func TestRunContinuedStopsWhenClosed(t *testing.T) {
	prompts := 0
	err := RunContinued(context.Background(), "first",
		func(string, int) string { return "next" }, 6,
		func(_ context.Context, _ string, emit func(string) error) error {
			prompts++
			return emit(`{"tool_calls": [{"id": "a", "type": "function", "function": {"name": "f", "arguments": "{}"}}]}`)
		},
		func(string) error { return nil },
		func(string, ...any) {}, func(string, ...any) {},
	)
	if err != nil {
		t.Fatalf("RunContinued: %v", err)
	}
	if prompts != 1 {
		t.Fatalf("prompts = %d, want 1 (valid JSON must not trigger continuation)", prompts)
	}
}

func TestRunContinuedEmitErrorPropagates(t *testing.T) {
	sentinel := errors.New("emit failed")
	err := RunContinued(context.Background(), "first", nil, 0,
		func(_ context.Context, _ string, emit func(string) error) error {
			return emit("text")
		},
		func(string) error { return sentinel },
		func(string, ...any) {}, func(string, ...any) {},
	)
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
}

func TestTryParseToolCallsShortCloseSequence(t *testing.T) {
	inner := `{\"filePath\": \"a.html\", \"content\": \"<html></html>\"}`
	raw := `{"tool_calls": [{"id": "call_1", "type": "function", "function": {"name": "write", "arguments": "` + inner + `}}]}`

	remaining, calls, ok := TryParseToolCalls(raw)
	if !ok {
		t.Fatal("TryParseToolCalls ok = false, want true (short close sequence must heal)")
	}
	if remaining != "" {
		t.Fatalf("remaining = %q, want empty", remaining)
	}
	if len(calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(calls))
	}
	args := calls[0].Function.Arguments
	if !json.Valid([]byte(args)) {
		t.Fatalf("args invalid: %s", args)
	}
	var obj struct {
		FilePath string `json:"filePath"`
		Content  string `json:"content"`
	}
	if err := json.Unmarshal([]byte(args), &obj); err != nil {
		t.Fatal(err)
	}
	if obj.Content != "<html></html>" {
		t.Fatalf("content = %q, want html", obj.Content)
	}
}

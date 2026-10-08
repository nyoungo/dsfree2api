package openai

import (
	"encoding/json"
	"strings"
	"testing"
)

func toolsFor(names ...string) []ToolDef {
	out := make([]ToolDef, 0, len(names))
	for _, n := range names {
		out = append(out, ToolDef{Type: "function", Function: Function{Name: n}})
	}
	return out
}

func mustOneCall(t *testing.T, text string, tools []ToolDef) ToolCall {
	t.Helper()
	_, calls, ok := TryParseToolCallsForTools(text, tools)
	if !ok || len(calls) != 1 {
		t.Fatalf("expected exactly one tool call from %q, got ok=%v calls=%+v", text, ok, calls)
	}
	if !json.Valid([]byte(calls[0].Function.Arguments)) {
		t.Fatalf("arguments invalid JSON: %q", calls[0].Function.Arguments)
	}
	return calls[0]
}

func argsField(t *testing.T, c ToolCall, key string) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(c.Function.Arguments), &m); err != nil {
		t.Fatalf("arguments unmarshal: %v (%q)", err, c.Function.Arguments)
	}
	s, _ := m[key].(string)
	return s
}

// 01: XML <tool_call><function><name>…<arguments>{json}</arguments>.
func TestRepairXMLBasic(t *testing.T) {
	raw := `<tool_call><function><name>get_weather</name><arguments>{"city": "北京"}</arguments></function></tool_call>`
	c := mustOneCall(t, raw, toolsFor("get_weather"))
	if c.Function.Name != "get_weather" || argsField(t, c, "city") != "北京" {
		t.Fatalf("call = %+v", c)
	}
}

// 02: XML parameters delivered as <param name="…">.
func TestRepairXMLMultiParam(t *testing.T) {
	raw := `<tool_call><function><name>get_weather</name><arguments><param name="city">北京</param></arguments></function></tool_call>`
	c := mustOneCall(t, raw, toolsFor("get_weather"))
	if c.Function.Name != "get_weather" || argsField(t, c, "city") != "北京" {
		t.Fatalf("call = %+v", c)
	}
}

// 03: several <invoke> dialects inside one wrapper.
func TestRepairXMLInvokeCalls(t *testing.T) {
	raw := `<tool_call><invoke name="get_weather"><parameter name="city">北京</parameter></invoke>` +
		`<invoke name="web_search"><parameter name="query">故宫</parameter></invoke></tool_call>`
	_, calls, ok := TryParseToolCalls(raw)
	if !ok || len(calls) != 2 {
		t.Fatalf("calls = %+v ok=%v", calls, ok)
	}
	if calls[0].Function.Name != "get_weather" || calls[1].Function.Name != "web_search" {
		t.Fatalf("names = %q, %q", calls[0].Function.Name, calls[1].Function.Name)
	}
}

// 04: XML wrapper with the arguments spilled into an inner JSON array.
func TestRepairMixedXMLJSON(t *testing.T) {
	raw := `<tool_calls><function><name>get_weather</name></function>[{"arguments": {"city": "北京"}}]</tool_calls>`
	c := mustOneCall(t, raw, toolsFor("get_weather"))
	if c.Function.Name != "get_weather" || argsField(t, c, "city") != "北京" {
		t.Fatalf("call = %+v", c)
	}
}

// 05: function/params field aliases.
func TestRepairFieldAliases(t *testing.T) {
	raw := `{"function": "get_weather", "params": {"city": "北京"}}`
	c := mustOneCall(t, raw, toolsFor("get_weather"))
	if c.Function.Name != "get_weather" || argsField(t, c, "city") != "北京" {
		t.Fatalf("call = %+v", c)
	}
	nested := `{"function": {"name": "get_weather"}, "parameters": {"city": "北京"}}`
	c = mustOneCall(t, nested, toolsFor("get_weather"))
	if c.Function.Name != "get_weather" || argsField(t, c, "city") != "北京" {
		t.Fatalf("nested call = %+v", c)
	}
}

// 06: arguments already delivered as a JSON string.
func TestRepairArgumentsAsString(t *testing.T) {
	raw := `{"name": "get_weather", "arguments": "{\"city\": \"北京\"}"}`
	c := mustOneCall(t, raw, toolsFor("get_weather"))
	if argsField(t, c, "city") != "北京" {
		t.Fatalf("call = %+v", c)
	}
}

// 07/08: array with a mismatched or missing closer.
func TestRepairBracketDamage(t *testing.T) {
	good := `[{"name": "get_weather", "arguments": {"city": "北京"}}]`
	missing := `[{"name": "get_weather", "arguments": {"city": "北京"}}`
	for _, raw := range []string{good, missing} {
		c := mustOneCall(t, raw, toolsFor("get_weather"))
		if c.Function.Name != "get_weather" || argsField(t, c, "city") != "北京" {
			t.Fatalf("raw=%q call=%+v", raw, c)
		}
	}
}

// 09: name holds the parameter object and arguments holds the tool name.
func TestRepairSwappedNameArguments(t *testing.T) {
	raw := `[{"name": {"city": "北京"}, "arguments": "get_weather"}]`
	c := mustOneCall(t, raw, toolsFor("get_weather"))
	if c.Function.Name != "get_weather" || argsField(t, c, "city") != "北京" {
		t.Fatalf("call = %+v", c)
	}
}

// 10: parameters spilled to the top level next to name.
func TestRepairSpilledParameters(t *testing.T) {
	raw := `[{"name": "get_weather", "city": "北京"}]`
	c := mustOneCall(t, raw, toolsFor("get_weather"))
	if c.Function.Name != "get_weather" || argsField(t, c, "city") != "北京" {
		t.Fatalf("call = %+v", c)
	}
	// A spilled object whose name is not an offered tool is ordinary JSON.
	if _, calls, ok := TryParseToolCallsForTools(`{"name": "北京", "population": 21000000}`, toolsFor("get_weather")); ok {
		t.Fatalf("unrelated JSON parsed as tool call: %+v", calls)
	}
}

// Unquoted keys + invalid backslashes inside a tagged payload.
func TestRepairUnquotedKeysAndBackslashes(t *testing.T) {
	raw := `<tool_call>[{name: "read_file", arguments: {path: "C:\Users\name"}}]</tool_call>`
	c := mustOneCall(t, raw, toolsFor("read_file"))
	if c.Function.Name != "read_file" {
		t.Fatalf("call = %+v", c)
	}
	if !json.Valid([]byte(c.Function.Arguments)) {
		t.Fatalf("arguments invalid: %q", c.Function.Arguments)
	}
}

// Fullwidth / underscore lookalikes in the end marker must still match.
func TestRepairFuzzyEndTag(t *testing.T) {
	raw := `<|tool▁calls▁begin|><function><name>get_weather</name></function><|tool_calls▁end｜>`
	c := mustOneCall(t, raw, toolsFor("get_weather"))
	if c.Function.Name != "get_weather" {
		t.Fatalf("call = %+v", c)
	}
}

// A tagged example inside a code fence is documentation, not a call.
func TestTaggedInsideCodeFenceSkipped(t *testing.T) {
	raw := "示例：\n```json\n<tool_call><function><name>get_weather</name></function></tool_call>\n```"
	if _, calls, ok := TryParseToolCalls(raw); ok {
		t.Fatalf("fenced example parsed as tool call: %+v", calls)
	}
}

// A start tag inside an arguments value must not derail the parse.
func TestToolCallTagInsideValueNotSkipped(t *testing.T) {
	raw := "<|tool▁calls▁begin|>[{\"name\": \"format_code\", \"arguments\": {\"code\": \"```rust\\nfn main() {}\\n```\"}}]<|tool▁calls▁end|>"
	c := mustOneCall(t, raw, toolsFor("format_code"))
	if !strings.Contains(c.Function.Arguments, "fn main") {
		t.Fatalf("arguments = %q", c.Function.Arguments)
	}
}

// The prose around a bare repaired object survives as remaining.
func TestRepairBareObjectKeepsProse(t *testing.T) {
	raw := "好的，这就查。\n" + `{"name": "get_weather", "arguments": {"city": "北京"}}`
	remaining, calls, ok := TryParseToolCallsForTools(raw, toolsFor("get_weather"))
	if !ok || len(calls) != 1 {
		t.Fatalf("ok=%v calls=%+v", ok, calls)
	}
	if !strings.Contains(remaining, "好的") {
		t.Fatalf("remaining = %q", remaining)
	}
}

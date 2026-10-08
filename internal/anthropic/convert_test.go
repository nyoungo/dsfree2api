package anthropic

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/nyoungo/dsfree2api/internal/openai"
)

func TestToOpenAIFullRequest(t *testing.T) {
	var req Request
	body := `{
		"model": "deepseek-v4-flash-de",
		"max_tokens": 1024,
		"system": "Be terse.",
		"messages": [
			{"role": "user", "content": "What is the weather in Beijing?"},
			{"role": "assistant", "content": [
				{"type": "text", "text": "Let me check."},
				{"type": "tool_use", "id": "toolu_1", "name": "get_weather",
				 "input": {"city": "Beijing"}}
			]},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "toolu_1", "content": "Sunny, 25C"}
			]}
		],
		"tools": [
			{"name": "get_weather", "description": "Get weather",
			 "input_schema": {"type": "object", "properties": {"city": {"type": "string"}}}}
		],
		"tool_choice": {"type": "tool", "name": "get_weather"}
	}`
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("decode: %v", err)
	}
	msgs, tools, tc, err := ToOpenAI(&req)
	if err != nil {
		t.Fatalf("to_openai: %v", err)
	}
	if len(msgs) != 4 {
		t.Fatalf("len(msgs) = %d, want 4: %+v", len(msgs), msgs)
	}
	roles := []string{msgs[0].Role, msgs[1].Role, msgs[2].Role, msgs[3].Role}
	want := []string{"system", "user", "assistant", "tool"}
	for i, r := range want {
		if roles[i] != r {
			t.Errorf("msgs[%d].Role = %q, want %q", i, roles[i], r)
		}
	}
	if msgs[0].Content.Text != "Be terse." {
		t.Errorf("system = %q", msgs[0].Content.Text)
	}
	asst := msgs[2]
	if len(asst.ToolCalls) != 1 || asst.ToolCalls[0].Function.Name != "get_weather" {
		t.Fatalf("assistant tool_calls = %+v", asst.ToolCalls)
	}
	if asst.ToolCalls[0].ID != "toolu_1" {
		t.Errorf("tool call id = %q", asst.ToolCalls[0].ID)
	}
	if !strings.Contains(asst.ToolCalls[0].Function.Arguments, `"city"`) ||
		!strings.Contains(asst.ToolCalls[0].Function.Arguments, "Beijing") {
		t.Errorf("arguments = %q", asst.ToolCalls[0].Function.Arguments)
	}
	if msgs[3].ToolCallID != "toolu_1" || msgs[3].Content.Text != "Sunny, 25C" {
		t.Errorf("tool result = %+v", msgs[3])
	}
	if len(tools) != 1 || tools[0].Function.Name != "get_weather" {
		t.Fatalf("tools = %+v", tools)
	}
	if !strings.Contains(string(tools[0].Function.Parameters), "city") {
		t.Errorf("parameters = %s", tools[0].Function.Parameters)
	}
	var forced map[string]any
	if err := json.Unmarshal(tc, &forced); err != nil {
		t.Fatalf("tool_choice: %v", err)
	}
	if forced["type"] != "function" {
		t.Errorf("tool_choice = %s", tc)
	}
}

func TestToOpenAIStringContentAndSystemArray(t *testing.T) {
	var req Request
	body := `{
		"model": "m",
		"system": [{"type": "text", "text": "Rule A"}, {"type": "text", "text": "Rule B"}],
		"messages": [{"role": "user", "content": "Hi"}]
	}`
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("decode: %v", err)
	}
	msgs, _, _, err := ToOpenAI(&req)
	if err != nil {
		t.Fatalf("to_openai: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("len(msgs) = %d, want 2", len(msgs))
	}
	if !strings.Contains(msgs[0].Content.Text, "Rule A") || !strings.Contains(msgs[0].Content.Text, "Rule B") {
		t.Errorf("system = %q", msgs[0].Content.Text)
	}
	if msgs[1].Role != "user" || msgs[1].Content.Text != "Hi" {
		t.Errorf("user = %+v", msgs[1])
	}
}

func TestToOpenAIToolChoiceVariants(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
	}{
		{`{"type":"auto"}`, `"auto"`},
		{`{"type":"any"}`, `"required"`},
		{`{"type":"none"}`, `"none"`},
	} {
		choice := &ToolChoice{}
		if err := json.Unmarshal([]byte(tc.in), choice); err != nil {
			t.Fatalf("decode %s: %v", tc.in, err)
		}
		raw := toolChoiceToOpenAI(choice)
		if string(raw) != tc.want {
			t.Errorf("%s -> %s, want %s", tc.in, raw, tc.want)
		}
	}
}

func TestContentFrom(t *testing.T) {
	calls := []openai.ToolCall{{
		ID: "call_1", Type: "function",
		Function: openai.FunctionCall{Name: "get_weather", Arguments: `{"city":"Paris"}`},
	}}
	blocks := ContentFrom("Checking now.", calls)
	if len(blocks) != 2 || blocks[0].Type != "text" || blocks[1].Type != "tool_use" {
		t.Fatalf("blocks = %+v", blocks)
	}
	if blocks[1].ID != "call_1" || blocks[1].Name != "get_weather" {
		t.Errorf("tool_use = %+v", blocks[1])
	}
	if string(blocks[1].Input) != `{"city":"Paris"}` {
		t.Errorf("input = %s", blocks[1].Input)
	}
	if StopReasonFor(calls) != "tool_use" || StopReasonFor(nil) != "end_turn" {
		t.Errorf("stop reasons wrong")
	}
}

func TestContentFromInvalidArgumentsFallBackToEmptyObject(t *testing.T) {
	calls := []openai.ToolCall{{
		ID: "c", Type: "function",
		Function: openai.FunctionCall{Name: "x", Arguments: `not json`},
	}}
	blocks := ContentFrom("", calls)
	if len(blocks) != 1 || string(blocks[0].Input) != "{}" {
		t.Fatalf("blocks = %+v", blocks)
	}
}

// 红测试：tool_use_id / tool_use id 缺失时，转换结果不允许出现空
// tool_call_id —— 它会一路流进 BuildPrompt 并被上游 400 拒绝。
func TestToOpenAIBackfillsEmptyToolIDs(t *testing.T) {
	var req Request
	body := `{
		"model": "m",
		"max_tokens": 10,
		"messages": [
			{"role": "user", "content": "go"},
			{"role": "assistant", "content": [
				{"type": "tool_use", "id": "", "name": "f", "input": {}}
			]},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "", "content": "ok"}
			]}
		],
		"tools": [{"name": "f", "input_schema": {"type": "object"}}]
	}`
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("decode: %v", err)
	}
	msgs, _, _, err := ToOpenAI(&req)
	if err != nil {
		t.Fatalf("to_openai: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("len(msgs) = %d, want 3: %+v", len(msgs), msgs)
	}
	if got := msgs[1].ToolCalls[0].ID; got == "" {
		t.Fatal("assistant tool_use id still empty")
	}
	if msgs[2].Role != "tool" {
		t.Fatalf("msgs[2].Role = %q, want tool", msgs[2].Role)
	}
	if msgs[2].ToolCallID == "" {
		t.Fatal("tool_result tool_use_id still empty after backfill")
	}
	if msgs[2].ToolCallID != msgs[1].ToolCalls[0].ID {
		t.Errorf("tool_call_id = %q, want the tool_use id %q",
			msgs[2].ToolCallID, msgs[1].ToolCalls[0].ID)
	}
}

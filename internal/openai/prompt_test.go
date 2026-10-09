package openai

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBuildPromptKeepsRolesAndTextParts(t *testing.T) {
	msgs := []ChatMessage{
		{Role: "system", Content: MessageContent{Text: "Rules"}},
		{Role: "user", Content: MessageContent{Parts: []map[string]any{
			{"type": "text", "text": "Hello"},
		}}},
	}
	prompt := BuildPrompt(msgs, PromptOptions{})
	for _, want := range []string{"[System]", "Rules", "[User]", "Hello"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q:\n%s", want, prompt)
		}
	}
}

func TestBuildPromptWithTools(t *testing.T) {
	prompt := BuildPrompt([]ChatMessage{
		{Role: "user", Content: MessageContent{Text: "Hi"}},
	}, PromptOptions{Tools: []ToolDef{{
		Type:     "function",
		Function: Function{Name: "search", Description: "Search the web"},
	}}})
	if !strings.Contains(prompt, "=== TOOL INSTRUCTIONS ===") {
		t.Error("missing tool instructions marker")
	}
	if !strings.Contains(prompt, "search") {
		t.Error("missing tool name")
	}
}

// 红测试：上游模型曾用 DeepSeek 原生 DSML 标记（<|DSML|> calls> / invoke /
// parameter）代替 JSON 输出工具调用。解析/续写层已兼容该方言兜底（dsml.go），
// 但 JSON 才是保证完整送达的主路径，指令层必须显式禁止该方言。
func TestToolsPromptForbidsDSML(t *testing.T) {
	prompt := BuildPrompt([]ChatMessage{
		{Role: "user", Content: MessageContent{Text: "Hi"}},
	}, PromptOptions{Tools: []ToolDef{{
		Type:     "function",
		Function: Function{Name: "write", Description: "Write a file"},
	}}})
	if !strings.Contains(prompt, "DSML") {
		t.Error("tool instructions must explicitly forbid DSML markup")
	}
	if !strings.Contains(prompt, "Never") {
		t.Error("tool instructions must state the DSML ban as a prohibition")
	}
}

func TestBuildPromptAppendsSamplingConstraints(t *testing.T) {
	temp := 0.4
	max := 512
	prompt := BuildPrompt([]ChatMessage{
		{Role: "user", Content: MessageContent{Text: "Hi"}},
	}, PromptOptions{Temperature: &temp, MaxTokens: &max, Stop: []string{"###"}})
	if !strings.Contains(prompt, "=== GENERATION CONSTRAINTS ===") {
		t.Error("missing constraints marker")
	}
	if !strings.Contains(prompt, "0.4") || !strings.Contains(prompt, "512") || !strings.Contains(prompt, "###") {
		t.Errorf("constraints missing values:\n%s", prompt)
	}
}

func TestResponsesRequestToMessages(t *testing.T) {
	req := &ResponsesRequest{
		Model:        "deepseek-v4-flash-de",
		Instructions: "Be terse",
		Input:        json.RawMessage(`"Hi"`),
	}
	msgs, err := ResponsesToMessages(req)
	if err != nil {
		t.Fatalf("responses_to_messages: %v", err)
	}
	if len(msgs) != 2 || msgs[0].Role != "system" || msgs[1].Role != "user" {
		t.Fatalf("roles = %v", []string{msgs[0].Role, msgs[1].Role})
	}
}

func TestResponsesRequestMessageArrayInput(t *testing.T) {
	req := &ResponsesRequest{Input: json.RawMessage(`[
		{"role":"user","content":"a"},
		{"role":"assistant","content":"b"}
	]`)}
	msgs, err := ResponsesToMessages(req)
	if err != nil {
		t.Fatalf("responses_to_messages: %v", err)
	}
	if len(msgs) != 2 || msgs[0].Content.Text != "a" || msgs[1].Content.Text != "b" {
		t.Fatalf("msgs = %+v", msgs)
	}
}

func TestResponsesRequestFunctionCallRoundTrip(t *testing.T) {
	req := &ResponsesRequest{Input: json.RawMessage(`[
		{"type":"message","role":"user","content":"weather in Paris?"},
		{"type":"function_call","call_id":"call_1","name":"get_weather",
		 "arguments":"{\"city\":\"Paris\"}"},
		{"type":"function_call_output","call_id":"call_1","output":"Sunny"},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"It is sunny."}]}
	]`)}
	msgs, err := ResponsesToMessages(req)
	if err != nil {
		t.Fatalf("responses_to_messages: %v", err)
	}
	if len(msgs) != 4 {
		t.Fatalf("len(msgs) = %d, want 4: %+v", len(msgs), msgs)
	}
	if msgs[1].Role != "assistant" || len(msgs[1].ToolCalls) != 1 {
		t.Fatalf("assistant = %+v", msgs[1])
	}
	if msgs[1].ToolCalls[0].ID != "call_1" || msgs[1].ToolCalls[0].Function.Name != "get_weather" {
		t.Errorf("tool_call = %+v", msgs[1].ToolCalls[0])
	}
	if msgs[1].ToolCalls[0].Function.Arguments != `{"city":"Paris"}` {
		t.Errorf("arguments = %q", msgs[1].ToolCalls[0].Function.Arguments)
	}
	if msgs[2].Role != "tool" || msgs[2].ToolCallID != "call_1" || msgs[2].Content.Text != "Sunny" {
		t.Errorf("tool = %+v", msgs[2])
	}
	if msgs[3].Role != "assistant" || msgs[3].Content.String() != "It is sunny." {
		t.Errorf("final assistant = %+v", msgs[3])
	}
}

func TestResponsesToolToToolDef(t *testing.T) {
	flat := ResponsesTool{Type: "function", Name: "f", Description: "d",
		Parameters: json.RawMessage(`{"type":"object"}`)}
	def, ok := flat.ToToolDef()
	if !ok || def.Function.Name != "f" || def.Type != "function" {
		t.Fatalf("flat = %+v ok=%v", def, ok)
	}
	nested := ResponsesTool{Type: "function", Function: &Function{Name: "g"}}
	def, ok = nested.ToToolDef()
	if !ok || def.Function.Name != "g" {
		t.Fatalf("nested = %+v ok=%v", def, ok)
	}
	if _, ok := (ResponsesTool{Type: "web_search_preview"}).ToToolDef(); ok {
		t.Error("non-function tool should be dropped")
	}
}

func TestToolsPromptForcesFlatToolChoiceName(t *testing.T) {
	prompt := BuildPrompt([]ChatMessage{
		{Role: "user", Content: MessageContent{Text: "Hi"}},
	}, PromptOptions{
		Tools:      []ToolDef{{Type: "function", Function: Function{Name: "get_weather"}}},
		ToolChoice: json.RawMessage(`{"type":"function","name":"get_weather"}`),
	})
	if !strings.Contains(prompt, "MUST use the tool 'get_weather'") {
		t.Errorf("forced tool missing:\n%s", prompt)
	}
}

func TestTryParseToolCallsPlainJSON(t *testing.T) {
	text := `{"tool_calls":[{"id":"call_001","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Beijing\"}"}}]}`
	remaining, calls, ok := TryParseToolCalls(text)
	if !ok || calls == nil {
		t.Fatalf("expected tool calls, got ok=%v calls=%v", ok, calls)
	}
	if remaining != "" {
		t.Errorf("remaining = %q", remaining)
	}
	if len(calls) != 1 || calls[0].Function.Name != "get_weather" {
		t.Fatalf("calls = %+v", calls)
	}
	if calls[0].Function.Arguments != `{"city":"Beijing"}` {
		t.Errorf("arguments = %q", calls[0].Function.Arguments)
	}
}

func TestTryParseToolCallsMarkdownJSONBlock(t *testing.T) {
	text := "Sure, here you go:\n```json\n" +
		`{"tool_calls":[{"id":"c1","type":"function","function":{"name":"search","arguments":"{\"q\":\"hello\"}"}}]}` +
		"\n```\nDone."
	remaining, calls, ok := TryParseToolCalls(text)
	if !ok || calls == nil {
		t.Fatalf("expected tool calls, got ok=%v", ok)
	}
	if !strings.Contains(remaining, "Sure") {
		t.Errorf("remaining = %q", remaining)
	}
	if calls[0].Function.Name != "search" {
		t.Errorf("name = %q", calls[0].Function.Name)
	}
}

func TestTryParseToolCallsNoToolCall(t *testing.T) {
	text := "The weather in Beijing is sunny."
	remaining, calls, ok := TryParseToolCalls(text)
	if ok || calls != nil {
		t.Fatalf("expected no tool calls, got ok=%v calls=%v", ok, calls)
	}
	if remaining != text {
		t.Errorf("remaining = %q", remaining)
	}
}

// 红测试：function_call_output 缺 call_id 时，转换出的 tool 消息不能带
// 空 tool_call_id（上游会直接 400 拒绝整条请求）。
func TestResponsesToMessagesBackfillsEmptyCallID(t *testing.T) {
	req := &ResponsesRequest{Input: json.RawMessage(`[
		{"type":"function_call","call_id":"call_1","name":"f","arguments":"{}"},
		{"type":"function_call_output","output":"ok"}
	]`)}
	msgs, err := ResponsesToMessages(req)
	if err != nil {
		t.Fatalf("responses_to_messages: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("len(msgs) = %d, want 2: %+v", len(msgs), msgs)
	}
	if msgs[1].Role != "tool" || msgs[1].ToolCallID != "call_1" {
		t.Errorf("tool result = %+v, want ToolCallID=call_1", msgs[1])
	}
}

// 两侧都缺 id：function_call 现场生成的 id 必须成为 output 的配对目标，
// 而不是各自随机生成两个对不上的 id。
func TestResponsesToMessagesBackfillsBothSidesConsistently(t *testing.T) {
	req := &ResponsesRequest{Input: json.RawMessage(`[
		{"type":"function_call","name":"f","arguments":"{}"},
		{"type":"function_call_output","output":"ok"}
	]`)}
	msgs, err := ResponsesToMessages(req)
	if err != nil {
		t.Fatalf("responses_to_messages: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("len(msgs) = %d, want 2: %+v", len(msgs), msgs)
	}
	if got := msgs[0].ToolCalls[0].ID; got == "" {
		t.Fatal("function_call id still empty")
	}
	if msgs[1].ToolCallID != msgs[0].ToolCalls[0].ID {
		t.Errorf("output call_id = %q, want the function_call id %q",
			msgs[1].ToolCallID, msgs[0].ToolCalls[0].ID)
	}
}

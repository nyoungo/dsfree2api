package openai

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// 回归：OpenAI 的 stop 允许单个字符串或字符串数组；字段类型是 []string 时
// 单字符串会让整个请求体解码失败（400），而这是合法的 OpenAI 请求。
func TestStopAcceptsSingleString(t *testing.T) {
	var req ChatCompletionRequest
	raw := `{"model":"m","messages":[{"role":"user","content":"hi"}],"stop":"END"}`
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatalf("stop 写成字符串时解码失败（OpenAI 合法请求）: %v", err)
	}
	if !reflect.DeepEqual([]string(req.Stop), []string{"END"}) {
		t.Fatalf("stop = %#v, want [END]", req.Stop)
	}
}

// 回归：数组写法必须照常工作。
func TestStopStillAcceptsArray(t *testing.T) {
	var req ChatCompletionRequest
	raw := `{"model":"m","messages":[],"stop":["A","B"]}`
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatalf("数组写法解码失败: %v", err)
	}
	if !reflect.DeepEqual([]string(req.Stop), []string{"A", "B"}) {
		t.Fatalf("stop = %#v, want [A B]", req.Stop)
	}
}

// 回归：response_format.type = json_schema 时，schema 必须进入生成约束，
// 否则结构化输出没有任何约束，客户端拿到的对象不符合声明的 schema。
func TestPromptCarriesJSONSchema(t *testing.T) {
	const schema = `{"type":"object","properties":{"title":{"type":"string"}},"required":["title"]}`
	prompt := BuildPrompt([]ChatMessage{{Role: "user", Content: MessageContent{Text: "hi"}}},
		PromptOptions{ResponseFormat: &ResponseFormat{Type: "json_schema", JSONSchema: json.RawMessage(schema)}})

	if !strings.Contains(prompt, "json_schema") && !strings.Contains(prompt, "JSON Schema") {
		t.Fatalf("提示词没有提到 schema:\n%s", prompt)
	}
	for _, want := range []string{"title", `"required"`} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("提示词缺少 schema 内容 %q:\n%s", want, prompt)
		}
	}
}

// 回归：json_object 老行为不能回归。
func TestPromptStillHandlesJSONObject(t *testing.T) {
	prompt := BuildPrompt([]ChatMessage{{Role: "user", Content: MessageContent{Text: "hi"}}},
		PromptOptions{ResponseFormat: &ResponseFormat{Type: "json_object"}})
	if !strings.Contains(prompt, "JSON object") {
		t.Fatalf("json_object 提示缺失:\n%s", prompt)
	}
}

// 回归：tool_calls 条目缺少/损坏的 function 时不能吞掉错误并产出一个
// name 为空的调用丢给客户端（客户端会在执行阶段才炸）。
func TestMalformedToolCallFunctionIsRejected(t *testing.T) {
	_, calls, ok := TryParseToolCalls(`{"tool_calls":[{"id":"c1","type":"function"}]}`)
	for _, c := range calls {
		if c.Function.Name == "" {
			t.Fatalf("解析出 name 为空的 tool call: %+v (ok=%v)", c, ok)
		}
	}
}

// 回归：正常的 tool_calls 解析不能受影响。
func TestValidToolCallStillParses(t *testing.T) {
	_, calls, ok := TryParseToolCalls(`{"tool_calls":[{"id":"c1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"SH\"}"}}]}`)
	if !ok {
		t.Fatalf("正常 tool_calls 没被识别")
	}
	if len(calls) != 1 || calls[0].Function.Name != "get_weather" {
		t.Fatalf("calls = %+v", calls)
	}
}

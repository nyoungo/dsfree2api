package api

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/nyoungo/dsfree2api/internal/openai"
)

// 回归：Responses API 的 text.format 必须转成提示词里的输出格式约束。
// 之前 ResponsesRequest 根本不解析 text 字段，客户端指定的 json_schema
// 被静默丢弃 —— 模型可以随便输出非 JSON，客户端的 schema 校验必炸。
func TestResponsesTextFormatBecomesPromptConstraint(t *testing.T) {
	body := `{"model":"m","input":"hi",` +
		`"text":{"format":{"type":"json_schema",` +
		`"json_schema":{"name":"addr","schema":{"type":"object",` +
		`"properties":{"city":{"type":"string"}},"required":["city"]}}}}}`
	var req openai.ResponsesRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if req.Text == nil || req.Text.Format == nil {
		t.Fatalf("text.format 没解析出来: %+v", req.Text)
	}

	messages, err := openai.ResponsesToMessages(&req)
	if err != nil {
		t.Fatalf("responses to messages: %v", err)
	}
	prompt := openai.BuildPrompt(messages, responsesPromptOptions(&req, nil))

	if !strings.Contains(prompt, "JSON Schema") {
		t.Fatalf("提示词里没有 schema 约束: %q", prompt)
	}
	if !strings.Contains(prompt, "city") {
		t.Fatalf("提示词里没有 schema 内容: %q", prompt)
	}
}

// 回归：没有 text.format 的请求照常工作，不能凭空多出约束。
func TestResponsesWithoutTextFormatHasNoConstraint(t *testing.T) {
	var req openai.ResponsesRequest
	if err := json.Unmarshal([]byte(`{"model":"m","input":"hi"}`), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	messages, err := openai.ResponsesToMessages(&req)
	if err != nil {
		t.Fatalf("responses to messages: %v", err)
	}
	prompt := openai.BuildPrompt(messages, responsesPromptOptions(&req, nil))
	if strings.Contains(prompt, "JSON Schema") {
		t.Fatalf("没有 text.format 却出现了 schema 约束: %q", prompt)
	}
}

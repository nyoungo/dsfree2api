package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nyoungo/dsfree2api/internal/openai"
)

// 回归：非流式 /v1/chat/completions 的 choices[0].message.role 必须是
// "assistant"。Role 的 json tag 没有 omitempty（types.go:163），漏设就会
// 返回空串；流式路径与非流式路径必须一致。
func TestBuildCompletionRoleIsAssistant(t *testing.T) {
	resp := buildCompletion(openai.ChatCompletionRequest{Model: "m"}, "p", "hello")
	if len(resp.Choices) != 1 {
		t.Fatalf("choices = %d", len(resp.Choices))
	}
	got := resp.Choices[0].Message.Role
	if got != "assistant" {
		raw, _ := json.Marshal(resp.Choices[0].Message)
		t.Fatalf("choices[0].message.role = %q (want \"assistant\"), raw=%s", got, raw)
	}
}

// 回归：/v1/responses 的 usage 必须用 Responses API 的字段名
// （input_tokens/output_tokens），而不是 Chat Completions 的
// prompt_tokens/completion_tokens —— 后者会让客户端读到 None。
// 官方文档："Chat Completions reports prompt_tokens, completion_tokens, and
// total_tokens. Responses reports input_tokens, output_tokens, and total_tokens."
func TestResponsesUsageFieldNames(t *testing.T) {
	m := responsesUsage(openai.Usage{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3})
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, want := range []string{"input_tokens", "output_tokens", "total_tokens"} {
		if _, ok := got[want]; !ok {
			t.Fatalf("Responses API usage 缺少 %s，实际字段: %v", want, keys(got))
		}
	}
	for _, bad := range []string{"prompt_tokens", "completion_tokens"} {
		if _, ok := got[bad]; ok {
			t.Fatalf("Responses API usage 不应包含 Chat 字段 %s，实际字段: %v", bad, keys(got))
		}
	}
}

// 回归：请求体必须有上限（api_keys 为空时这些端点未鉴权），超限要返回 413
// 而不是把整个 body 读进内存。
func TestDecodeBodyRejectsOversizedPayload(t *testing.T) {
	payload := []byte(`{"model":"m","messages":[{"role":"user","content":"` +
		strings.Repeat("a", maxRequestBodyBytes+4096) + `"}]}`)
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(payload))
	w := httptest.NewRecorder()

	var req openai.ChatCompletionRequest
	if decodeBody(w, r, &req, writeError) {
		t.Fatalf("超过上限的请求体被接受了")
	}
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusRequestEntityTooLarge)
	}
}

// 回归：未超限的正常请求体仍然可以解码。
func TestDecodeBodyAcceptsNormalPayload(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	w := httptest.NewRecorder()

	var req openai.ChatCompletionRequest
	if !decodeBody(w, r, &req, writeError) {
		t.Fatalf("正常请求体被拒绝: %s", w.Body.String())
	}
	if req.Model != "m" {
		t.Fatalf("model = %q", req.Model)
	}
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

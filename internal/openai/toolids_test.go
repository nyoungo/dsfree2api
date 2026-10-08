package openai

import (
	"strings"
	"testing"
)

func toolTextMsg(role, text string) ChatMessage {
	return ChatMessage{Role: role, Content: MessageContent{Text: text}}
}

// 红测试：上游因空 tool_call_id 拒绝整条请求
// （"tool messages must include a non-empty string tool_call_id"）。空 id
// 的 tool 结果必须配对到前序未应答的 assistant tool_calls 上。
func TestBackfillToolIDSPairsEmptyResultsWithAssistantCalls(t *testing.T) {
	msgs := []ChatMessage{
		toolTextMsg("user", "run the tools"),
		{Role: "assistant", ToolCalls: []ToolCall{
			{ID: "call_a", Type: "function", Function: FunctionCall{Name: "one", Arguments: "{}"}},
			{ID: "call_b", Type: "function", Function: FunctionCall{Name: "two", Arguments: "{}"}},
		}},
		{Role: "tool", Content: MessageContent{Text: "r1"}},
		{Role: "tool", Content: MessageContent{Text: "r2"}},
	}
	BackfillToolIDs(msgs)
	if msgs[2].ToolCallID != "call_a" {
		t.Errorf("first empty result = %q, want call_a", msgs[2].ToolCallID)
	}
	if msgs[3].ToolCallID != "call_b" {
		t.Errorf("second empty result = %q, want call_b", msgs[3].ToolCallID)
	}
}

func TestBackfillToolIDsFillEmptyAssistantIDs(t *testing.T) {
	msgs := []ChatMessage{
		toolTextMsg("user", "go"),
		{Role: "assistant", ToolCalls: []ToolCall{
			{ID: "", Function: FunctionCall{Name: "one", Arguments: "{}"}},
			{ID: "call_b", Type: "function", Function: FunctionCall{Name: "two", Arguments: "{}"}},
		}},
		{Role: "tool", Content: MessageContent{Text: "r1"}},
		{Role: "tool", Content: MessageContent{Text: "r2"}},
	}
	BackfillToolIDs(msgs)
	asst := msgs[1].ToolCalls
	if asst[0].ID == "" {
		t.Fatal("assistant tool_calls[0].ID still empty")
	}
	if asst[0].Type != "function" {
		t.Errorf("assistant tool_calls[0].Type = %q, want function", asst[0].Type)
	}
	if msgs[2].ToolCallID != asst[0].ID {
		t.Errorf("first result paired with %q, want %q", msgs[2].ToolCallID, asst[0].ID)
	}
	if msgs[3].ToolCallID != "call_b" {
		t.Errorf("second result = %q, want call_b", msgs[3].ToolCallID)
	}
}

// 无前序 assistant 调用可配对时合成占位 id —— 永远不返回空串，也不拒绝请求。
func TestBackfillToolIDsSynthesizePlaceholder(t *testing.T) {
	msgs := []ChatMessage{
		toolTextMsg("user", "hi"),
		{Role: "tool", Content: MessageContent{Text: "orphan"}},
	}
	BackfillToolIDs(msgs)
	if msgs[1].ToolCallID == "" {
		t.Fatal("orphan tool result still has empty tool_call_id")
	}
}

// 结果应落在自己那一轮的调用上：旧轮未应答的调用不能抢走新轮的结果。
func TestBackfillToolIDsPreferNearestTurn(t *testing.T) {
	msgs := []ChatMessage{
		toolTextMsg("user", "first"),
		{Role: "assistant", ToolCalls: []ToolCall{
			{ID: "call_old", Type: "function", Function: FunctionCall{Name: "one", Arguments: "{}"}},
		}},
		toolTextMsg("user", "second"),
		{Role: "assistant", ToolCalls: []ToolCall{
			{ID: "call_new", Type: "function", Function: FunctionCall{Name: "two", Arguments: "{}"}},
		}},
		{Role: "tool", Content: MessageContent{Text: "r"}},
	}
	BackfillToolIDs(msgs)
	if msgs[4].ToolCallID != "call_new" {
		t.Errorf("empty result paired with %q, want call_new", msgs[4].ToolCallID)
	}
}

// 幂等：合法 transcript 二次运行必须原样保留（配对不串位、id 不变）。
func TestBackfillToolIDsIdempotent(t *testing.T) {
	msgs := []ChatMessage{
		toolTextMsg("user", "go"),
		{Role: "assistant", ToolCalls: []ToolCall{
			{ID: "call_a", Type: "function", Function: FunctionCall{Name: "one", Arguments: "{}"}},
		}},
		{Role: "tool", ToolCallID: "call_a", Content: MessageContent{Text: "r"}},
	}
	BackfillToolIDs(msgs)
	first := msgs[2].ToolCallID
	BackfillToolIDs(msgs)
	if msgs[2].ToolCallID != first || first != "call_a" {
		t.Errorf("id changed across runs: %q -> %q", first, msgs[2].ToolCallID)
	}
	if msgs[1].ToolCalls[0].ID != "call_a" {
		t.Errorf("assistant id changed: %q", msgs[1].ToolCalls[0].ID)
	}
}

// 出站提示词里也不允许出现空 id —— BuildPrompt 的 [Tool Result (id=%s)]
// 和 [Tool Calls] JSON 都依赖回填后的值。
func TestBackfillToolIDsKeepsPromptIDsNonEmpty(t *testing.T) {
	msgs := []ChatMessage{
		toolTextMsg("user", "go"),
		{Role: "assistant", ToolCalls: []ToolCall{
			{ID: "", Function: FunctionCall{Name: "one", Arguments: "{}"}},
		}},
		{Role: "tool", Content: MessageContent{Text: "r"}},
	}
	BackfillToolIDs(msgs)
	prompt := BuildPrompt(msgs, PromptOptions{})
	if msgs[2].ToolCallID == "" {
		t.Fatal("tool result id still empty after backfill")
	}
	if !strings.Contains(prompt, "[Tool Result (id="+msgs[2].ToolCallID+")]") {
		t.Errorf("prompt missing non-empty tool result id:\n%s", prompt)
	}
	if strings.Contains(prompt, `"id":""`) {
		t.Errorf("prompt tool_calls JSON still carries an empty id:\n%s", prompt)
	}
}

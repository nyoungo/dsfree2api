package openai

// BackfillToolIDs enforces the non-empty tool id invariant on an inbound
// transcript. Upstream rejects any tool message whose tool_call_id is empty
// ("tool messages must include a non-empty string tool_call_id"), and strict
// clients echo back exactly what an earlier response carried — so both sides
// of the tool round-trip must be repaired here:
//
//  1. every assistant tool_calls entry gets an id (and type) when missing;
//  2. every tool result without a tool_call_id is paired with the nearest
//     preceding unanswered assistant tool call (walking backwards, taking
//     calls in order); when nothing is left to pair with, a placeholder id
//     is generated so the transcript stays well-formed either way.
//
// It mutates msgs in place and is idempotent: a transcript that already
// satisfies the invariant comes back unchanged.
func BackfillToolIDs(msgs []ChatMessage) {
	// Pass 1: assistant side — a tool call without an id is unusable by any
	// client, and an empty type breaks strict response validation.
	for i := range msgs {
		if msgs[i].Role != "assistant" {
			continue
		}
		for j := range msgs[i].ToolCalls {
			tc := &msgs[i].ToolCalls[j]
			if tc.ID == "" {
				tc.ID = "call_" + randomHex(8)
			}
			if tc.Type == "" {
				tc.Type = "function"
			}
		}
	}
	// answered collects every id a tool result already references, plus the
	// ids claimed while pairing empty results below.
	answered := make(map[string]bool, len(msgs))
	for _, m := range msgs {
		if m.Role == "tool" && m.ToolCallID != "" {
			answered[m.ToolCallID] = true
		}
	}
	// Pass 2: tool side — empty ids pair with the closest preceding call that
	// has not been answered yet, so results land on the calls of their own
	// turn even when older turns left calls unanswered.
	for i := range msgs {
		if msgs[i].Role != "tool" || msgs[i].ToolCallID != "" {
			continue
		}
		id := ""
		for j := i - 1; j >= 0 && id == ""; j-- {
			if msgs[j].Role != "assistant" {
				continue
			}
			for _, tc := range msgs[j].ToolCalls {
				if tc.ID != "" && !answered[tc.ID] {
					id = tc.ID
					answered[id] = true
					break
				}
			}
		}
		if id == "" {
			id = "call_" + randomHex(8)
		}
		msgs[i].ToolCallID = id
	}
}

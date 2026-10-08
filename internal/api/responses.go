package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/nyoungo/dsfree2api/internal/metrics"
	"github.com/nyoungo/dsfree2api/internal/openai"
	"github.com/nyoungo/dsfree2api/internal/upstream"
)

// responsesUsage renders token usage with the Responses API field names
// (input_tokens/output_tokens/total_tokens). Reusing the Chat Completions
// shape here would leave every client's response.usage.input_tokens as null.
func responsesUsage(u openai.Usage) map[string]any {
	return map[string]any{
		"input_tokens":  u.PromptTokens,
		"output_tokens": u.CompletionTokens,
		"total_tokens":  u.TotalTokens,
	}
}

// responsesPromptOptions 把 Responses 请求翻译成 BuildPrompt 选项。
// text.format 里的输出格式约束必须带过去 —— 与 Chat Completions 的
// response_format 等价，漏掉它客户端的 json_schema 就是静默失效。
func responsesPromptOptions(req *openai.ResponsesRequest, tools []openai.ToolDef) openai.PromptOptions {
	var rf *openai.ResponseFormat
	if req.Text != nil {
		rf = req.Text.Format
	}
	return openai.PromptOptions{
		Tools:          tools,
		ToolChoice:     req.ToolChoice,
		Temperature:    req.Temperature,
		TopP:           req.TopP,
		MaxTokens:      req.MaxOutputTokens,
		ResponseFormat: rf,
	}
}

func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	var req openai.ResponsesRequest
	if !decodeBody(w, r, &req, writeError) {
		return
	}

	inputText := responsesInputText(&req)
	var history []openai.ChatMessage
	if prev := strings.TrimSpace(req.PreviousResponseID); prev != "" {
		turn, ok := s.rstore.get(prev)
		if !ok {
			writeError(w, http.StatusBadRequest,
				"previous_response_id '"+prev+"' not found or expired; resend the full input",
				"invalid_request_error")
			return
		}
		history = openai.HistoryMessages(turn)
	}

	messages, err := openai.ResponsesToMessages(&req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}
	if len(history) > 0 {
		messages = spliceHistory(messages, history)
	}
	tools := make([]openai.ToolDef, 0, len(req.Tools))
	for _, t := range req.Tools {
		if def, ok := t.ToToolDef(); ok {
			tools = append(tools, def)
		}
	}
	s.cfg.RLock()
	m, resolvedID, ok := s.cfg.ResolveModel(req.Model)
	site := ""
	enabled := false
	if ok {
		site, enabled = m.Site, m.Enabled
	}
	s.cfg.RUnlock()
	if !ok || !enabled {
		writeError(w, http.StatusNotFound, "model not found: "+req.Model, "invalid_request_error")
		return
	}
	if resolvedID != req.Model {
		s.log.Info("model aliased", "requested", req.Model, "resolved", resolvedID)
		req.Model = resolvedID
	}

	prompt := openai.BuildPrompt(messages, responsesPromptOptions(&req, tools))

	s.met.IncInFlight(1)
	defer s.met.IncInFlight(-1)

	ctx, cancel := s.upstreamCtx(r)
	defer cancel()

	shouldStore := req.Store == nil || *req.Store

	if req.Stream {
		s.streamResponses(ctx, w, req, prompt, site, tools, continuationFor(tools, messages, s.continueRounds()), inputText, shouldStore)
		return
	}

	out := s.collect(ctx, req.Model, prompt, site, continuationFor(tools, messages, s.continueRounds()))
	if out.err != nil {
		writeUpstreamError(w, out.err)
		return
	}
	if out.status == metrics.StatusEmpty {
		writeError(w, http.StatusBadGateway, emptyResponseMessage, "upstream_empty_response")
		return
	}
	text, calls := splitToolOutput(out.body, tools)
	output := responseOutputItems(text, calls)
	stopReason := "stop"
	if len(calls) > 0 {
		stopReason = "tool_use"
	}
	respID := "resp_" + randHex(16)
	resp := map[string]any{
		"id":          respID,
		"object":      "response",
		"created_at":  time.Now().Unix(),
		"status":      "completed",
		"model":       req.Model,
		"output":      output,
		"output_text": text,
		"stop_reason": stopReason,
		"usage":       responsesUsage(out.usage),
	}
	if shouldStore {
		s.rstore.insert(respID, openai.StoredTurn{InputText: inputText, Output: output, Response: resp})
	}
	writeJSON(w, http.StatusOK, resp)
}

// spliceHistory inserts the replayed history right after a leading
// system/developer message so instructions keep their place at the top.
func spliceHistory(messages, history []openai.ChatMessage) []openai.ChatMessage {
	if len(messages) > 0 && (messages[0].Role == "system" || messages[0].Role == "developer") {
		merged := make([]openai.ChatMessage, 0, len(messages)+len(history))
		merged = append(merged, messages[0])
		merged = append(merged, history...)
		merged = append(merged, messages[1:]...)
		return merged
	}
	merged := make([]openai.ChatMessage, 0, len(messages)+len(history))
	merged = append(merged, history...)
	merged = append(merged, messages...)
	return merged
}

// responsesInputText extracts the user input text from a Responses request for
// the stored turn (a plain string, or the text of the user message items).
func responsesInputText(req *openai.ResponsesRequest) string {
	trimmed := strings.TrimSpace(string(req.Input))
	if trimmed == "" {
		return ""
	}
	if trimmed[0] == '"' {
		var s string
		if json.Unmarshal(req.Input, &s) == nil {
			return s
		}
		return ""
	}
	var items []json.RawMessage
	if json.Unmarshal(req.Input, &items) != nil {
		return ""
	}
	var parts []string
	for _, raw := range items {
		var probe struct {
			Type string `json:"type"`
			Role string `json:"role"`
		}
		if json.Unmarshal(raw, &probe) != nil {
			continue
		}
		if (probe.Type != "" && probe.Type != "message") || (probe.Role != "" && probe.Role != "user") {
			continue
		}
		var it struct {
			Content json.RawMessage `json:"content"`
		}
		_ = json.Unmarshal(raw, &it)
		var content openai.MessageContent
		if json.Unmarshal(it.Content, &content) == nil {
			if s := content.String(); s != "" {
				parts = append(parts, s)
			}
		}
	}
	return strings.Join(parts, "\n")
}

// handleGetResponse serves a stored Response snapshot, or 404 when unknown or
// expired.
func (s *Server) handleGetResponse(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	snap, ok := s.rstore.getResponse(id)
	if !ok {
		writeError(w, http.StatusNotFound, "response not found: "+id, "invalid_request_error")
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

// responseOutputItems renders a plain-text reply plus parsed tool calls as
// Responses API output items.
func responseOutputItems(text string, calls []openai.ToolCall) []map[string]any {
	output := make([]map[string]any, 0, len(calls)+1)
	if text != "" {
		output = append(output, map[string]any{
			"id":     "msg_" + randHex(12),
			"type":   "message",
			"status": "completed",
			"role":   "assistant",
			"content": []map[string]any{{
				"type":        "output_text",
				"text":        text,
				"annotations": []any{},
			}},
		})
	}
	for _, c := range calls {
		output = append(output, map[string]any{
			"id":        "fc_" + randHex(12),
			"type":      "function_call",
			"status":    "completed",
			"call_id":   c.ID,
			"name":      c.Function.Name,
			"arguments": c.Function.Arguments,
		})
	}
	return output
}

func (s *Server) streamResponses(ctx context.Context, w http.ResponseWriter, req openai.ResponsesRequest, prompt, site string, tools []openai.ToolDef, cont openai.ContinuationFunc, inputText string, shouldStore bool) {
	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	id := "resp_" + randHex(16)
	write := func(v any) error {
		raw, err := json.Marshal(v)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", raw); err != nil {
			return err
		}
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	}

	// The created event is held back until the upstream produces output, so a
	// failure with nothing streamed yet can still use a real HTTP status
	// instead of a failed event inside an already-200 stream.
	var started bool
	beginStream := func() error {
		if started {
			return nil
		}
		started = true
		return write(map[string]any{
			"type": "response.created",
			"response": map[string]any{
				"id": id, "model": req.Model, "status": "in_progress", "created_at": time.Now().Unix(),
			},
		})
	}

	// With tools the raw tool-call JSON is converted into a live
	// function_call item stream: output_item.added once id+name are known,
	// one function_call_arguments.delta per piece, done events at finalize.
	var args *openai.ArgStreamer
	if len(tools) > 0 {
		args = openai.NewArgStreamer()
	}

	var (
		seq     = 0
		outIdx  = 0
		acc     = map[int]string{} // full args seen per streamer index
		emitted = map[int]int{}    // bytes of acc written as deltas
		itemIDs = map[int]string{}
		callIDs = map[int]string{}
		fnNames = map[int]string{}
		openIdx = -1 // streamer index of the in-progress function_call item
		openOut = -1
		output  []map[string]any
	)
	closeOpen := func() error {
		if openIdx < 0 {
			return nil
		}
		itemID := itemIDs[openIdx]
		if err := write(map[string]any{
			"type": "response.function_call_arguments.done", "item_id": itemID,
			"output_index": openOut, "arguments": acc[openIdx], "sequence_number": seq,
		}); err != nil {
			return err
		}
		seq++
		item := map[string]any{
			"id": itemID, "type": "function_call", "status": "completed",
			"call_id": callIDs[openIdx], "name": fnNames[openIdx], "arguments": acc[openIdx],
		}
		if err := write(map[string]any{
			"type": "response.output_item.done", "output_index": openOut,
			"sequence_number": seq, "item": item,
		}); err != nil {
			return err
		}
		seq++
		output = append(output, item)
		openIdx, openOut = -1, -1
		return nil
	}
	onToolDelta := func(d openai.ToolArgDelta) error {
		if d.Index < openIdx {
			return nil // models emit calls in order — drop stale index
		}
		if err := beginStream(); err != nil {
			return err
		}
		if d.ID != "" {
			callIDs[d.Index] = d.ID
		}
		if d.Name != "" {
			fnNames[d.Index] = d.Name
		}
		if d.Index != openIdx {
			if err := closeOpen(); err != nil {
				return err
			}
			if callIDs[d.Index] == "" || fnNames[d.Index] == "" {
				// cannot open the item yet — hold args until id+name arrive
				if d.Args != "" {
					acc[d.Index] += d.Args
				}
				return nil
			}
			itemID := "fc_" + randHex(12)
			itemIDs[d.Index] = itemID
			openOut = outIdx
			openIdx = d.Index
			outIdx++
			if err := write(map[string]any{
				"type": "response.output_item.added", "output_index": openOut, "sequence_number": seq,
				"item": map[string]any{
					"id": itemID, "type": "function_call", "status": "in_progress",
					"call_id": callIDs[d.Index], "name": fnNames[d.Index], "arguments": "",
				},
			}); err != nil {
				return err
			}
			seq++
		}
		if d.Args != "" {
			acc[d.Index] += d.Args
		}
		if openIdx != d.Index {
			return nil
		}
		if fresh := acc[d.Index][emitted[d.Index]:]; fresh != "" {
			emitted[d.Index] = len(acc[d.Index])
			return write(map[string]any{
				"type": "response.function_call_arguments.delta", "item_id": itemIDs[d.Index],
				"output_index": openOut, "delta": fresh, "sequence_number": seq,
			})
		}
		return nil
	}

	start := time.Now()
	info := &upstream.ServeInfo{}
	var text strings.Builder
	var firstDelta time.Time

	err := s.chatWithContinue(ctx, req.Model, prompt, cont, info, func() {
		if firstDelta.IsZero() {
			firstDelta = time.Now()
		}
	}, func(seg string) error {
		if err := beginStream(); err != nil {
			return err
		}
		text.WriteString(seg)
		if args == nil {
			return write(map[string]any{"type": "response.output_text.delta", "delta": seg})
		}
		for _, d := range args.Feed(text.String()) {
			if err := onToolDelta(d); err != nil {
				return err
			}
		}
		return nil
	})

	duration := time.Since(start)
	body := text.String()
	servedBy, servedSite, _ := info.Get()
	if servedSite == "" {
		servedSite = site
	}

	if err != nil {
		s.met.Record(metrics.Record{
			Model: req.Model, ServedBy: servedBy, Site: servedSite, Stream: true,
			DurationMs: duration.Milliseconds(), TTFTMs: msSince(firstDelta, start),
			Status: metrics.StatusError, Error: err.Error(),
		})
		// Never report a failed stream as completed.
		if !started {
			// nothing on the wire yet — report a real status like the
			// non-streaming path does
			writeUpstreamError(w, err)
			return
		}
		_ = write(map[string]any{
			"type": "response.failed",
			"response": map[string]any{
				"id": id, "model": req.Model, "status": "failed", "created_at": time.Now().Unix(),
				"error": map[string]any{"code": "upstream_error", "message": err.Error()},
			},
		})
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		return
	}
	if strings.TrimSpace(body) == "" {
		s.met.Record(metrics.Record{
			Model: req.Model, ServedBy: servedBy, Site: servedSite, Stream: true,
			DurationMs: duration.Milliseconds(), Status: metrics.StatusEmpty, Error: "empty response",
		})
		if !started {
			writeError(w, http.StatusBadGateway, emptyResponseMessage, "upstream_empty_response")
			return
		}
		_ = write(map[string]any{
			"type": "response.failed",
			"response": map[string]any{
				"id": id, "model": req.Model, "status": "failed", "created_at": time.Now().Unix(),
				"error": map[string]any{"code": "upstream_empty_response", "message": emptyResponseMessage},
			},
		})
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		return
	}

	// Success: response.created must precede every event below.
	if err := beginStream(); err != nil {
		return
	}

	usage := estimateUsage(prompt, body)
	outText, calls := splitToolOutput(body, tools)

	var stopReason = "stop"
	if len(calls) > 0 {
		stopReason = "tool_use"
	}

	if args == nil {
		output = responseOutputItems(outText, calls)
		_ = write(map[string]any{
			"type": "response.output_text.done",
			"text": body,
		})
	} else {
		if len(calls) > 0 {
			deltas, resend, diverged := args.Finalize(calls)
			if diverged {
				s.log.Warn("streamed tool arguments diverged from repaired parse", "model", req.Model, "detail", args.Diverge())
			}
			for _, d := range deltas {
				_ = onToolDelta(d)
			}
			// close the open item with the accumulated args
			_ = closeOpen()
			// calls that were never streamed become full items now
			for _, c := range resend {
				itemID := "fc_" + randHex(12)
				item := map[string]any{
					"id": itemID, "type": "function_call", "status": "completed",
					"call_id": c.ID, "name": c.Function.Name, "arguments": c.Function.Arguments,
				}
				_ = write(map[string]any{
					"type": "response.output_item.added", "output_index": outIdx, "sequence_number": seq,
					"item": map[string]any{
						"id": itemID, "type": "function_call", "status": "in_progress",
						"call_id": c.ID, "name": c.Function.Name, "arguments": "",
					},
				})
				seq++
				_ = write(map[string]any{
					"type": "response.function_call_arguments.delta", "item_id": itemID,
					"output_index": outIdx, "delta": c.Function.Arguments, "sequence_number": seq,
				})
				seq++
				_ = write(map[string]any{
					"type": "response.function_call_arguments.done", "item_id": itemID,
					"output_index": outIdx, "arguments": c.Function.Arguments, "sequence_number": seq,
				})
				seq++
				_ = write(map[string]any{
					"type": "response.output_item.done", "output_index": outIdx,
					"sequence_number": seq, "item": item,
				})
				seq++
				outIdx++
				output = append(output, item)
			}
		} else {
			if args.Sent() {
				s.log.Warn("tool parse failed after streaming argument deltas", "model", req.Model)
			}
			_ = closeOpen()
		}
		if outText != "" {
			itemID := "msg_" + randHex(12)
			item := map[string]any{
				"id": itemID, "type": "message", "status": "completed", "role": "assistant",
				"content": []map[string]any{{
					"type": "output_text", "text": outText, "annotations": []any{},
				}},
			}
			_ = write(map[string]any{
				"type": "response.output_item.added", "output_index": outIdx, "sequence_number": seq,
				"item": map[string]any{
					"id": itemID, "type": "message", "status": "in_progress", "role": "assistant",
					"content": []any{},
				},
			})
			seq++
			_ = write(map[string]any{
				"type": "response.output_text.delta", "item_id": itemID,
				"output_index": outIdx, "delta": outText, "content_index": 0, "sequence_number": seq,
			})
			seq++
			_ = write(map[string]any{
				"type": "response.output_text.done", "item_id": itemID,
				"output_index": outIdx, "text": outText, "content_index": 0, "sequence_number": seq,
			})
			seq++
			_ = write(map[string]any{
				"type": "response.output_item.done", "output_index": outIdx,
				"sequence_number": seq, "item": item,
			})
			seq++
			outIdx++
			output = append(output, item)
		}
	}
	completed := map[string]any{
		"id": id, "model": req.Model, "status": "completed", "created_at": time.Now().Unix(),
		"output": output, "stop_reason": stopReason, "usage": responsesUsage(usage),
	}
	_ = write(map[string]any{
		"type":     "response.completed",
		"response": completed,
	})
	if shouldStore {
		s.rstore.insert(id, openai.StoredTurn{InputText: inputText, Output: output, Response: completed})
	}
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}

	s.met.Record(metrics.Record{
		Model: req.Model, ServedBy: servedBy, Site: servedSite, Stream: true,
		DurationMs: duration.Milliseconds(), TTFTMs: msSince(firstDelta, start),
		PromptTokens: usage.PromptTokens, CompletionTokens: usage.CompletionTokens,
		Status: metrics.StatusOK,
	})
}

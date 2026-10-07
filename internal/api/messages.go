package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/nyoungo/dsfree2api/internal/anthropic"
	"github.com/nyoungo/dsfree2api/internal/metrics"
	"github.com/nyoungo/dsfree2api/internal/openai"
	"github.com/nyoungo/dsfree2api/internal/upstream"
)

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	var req anthropic.Request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error(), "invalid_request_error")
		return
	}
	if req.Model == "" {
		writeAnthropicError(w, http.StatusBadRequest, "model is required", "invalid_request_error")
		return
	}
	if len(req.Messages) == 0 {
		writeAnthropicError(w, http.StatusBadRequest, "messages is required", "invalid_request_error")
		return
	}
	s.cfg.RLock()
	m, ok := s.cfg.Models[req.Model]
	site := ""
	enabled := false
	if ok {
		site = m.Site
		enabled = m.Enabled
	}
	s.cfg.RUnlock()
	if !ok || !enabled {
		writeAnthropicError(w, http.StatusNotFound, "model not found: "+req.Model, "not_found_error")
		return
	}

	messages, tools, toolChoice, err := anthropic.ToOpenAI(&req)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}
	var maxTokens *int
	if req.MaxTokens > 0 {
		maxTokens = &req.MaxTokens
	}
	prompt := openai.BuildPrompt(messages, openai.PromptOptions{
		Tools:       tools,
		ToolChoice:  toolChoice,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		MaxTokens:   maxTokens,
		Stop:        req.StopSequences,
	})

	ctx, cancel := s.upstreamCtx(r)
	defer cancel()

	s.met.IncInFlight(1)
	defer s.met.IncInFlight(-1)

	cont := continuationFor(tools, messages, s.continueRounds())
	if req.Stream {
		s.streamMessages(ctx, w, req, prompt, site, tools, cont)
		return
	}
	s.completeMessages(ctx, w, req, prompt, site, tools, cont)
}

func (s *Server) completeMessages(ctx context.Context, w http.ResponseWriter, req anthropic.Request, prompt, site string, tools []openai.ToolDef, cont openai.ContinuationFunc) {
	out := s.collect(ctx, req.Model, prompt, site, cont)
	if out.err != nil {
		writeAnthropicUpstreamError(w, out.err)
		return
	}
	if out.status == metrics.StatusEmpty {
		writeAnthropicError(w, http.StatusInternalServerError, emptyResponseMessage, "api_error")
		return
	}
	text, calls := splitToolOutput(out.body, tools)
	resp := anthropic.Response{
		ID:         anthropic.NewID("msg_"),
		Type:       "message",
		Role:       "assistant",
		Model:      req.Model,
		Content:    anthropic.ContentFrom(text, calls),
		StopReason: anthropic.StopReasonFor(calls),
		Usage: anthropic.Usage{
			InputTokens:  out.usage.PromptTokens,
			OutputTokens: out.usage.CompletionTokens,
		},
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) streamMessages(ctx context.Context, w http.ResponseWriter, req anthropic.Request, prompt, site string, tools []openai.ToolDef, cont openai.ContinuationFunc) {
	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	writeEvent := func(ev string, v any) error {
		raw, err := json.Marshal(v)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev, raw); err != nil {
			return err
		}
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	}
	writeErr := func(typ, msg string) {
		_ = writeEvent("error", map[string]any{
			"type":  "error",
			"error": map[string]any{"type": typ, "message": msg},
		})
	}

	msgID := anthropic.NewID("msg_")
	_ = writeEvent("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": msgID, "type": "message", "role": "assistant", "model": req.Model,
			"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]any{
				"input_tokens":  estimateUsage(prompt, "").PromptTokens,
				"output_tokens": 0,
			},
		},
	})

	// When tools are requested the reply is buffered so the raw tool-call
	// JSON is parsed once and emitted as proper tool_use blocks instead of
	// being streamed to the client as prose.
	buffered := len(tools) > 0
	if !buffered {
		_ = writeEvent("content_block_start", map[string]any{
			"type": "content_block_start", "index": 0,
			"content_block": map[string]any{"type": "text", "text": ""},
		})
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
		text.WriteString(seg)
		if buffered {
			return nil
		}
		return writeEvent("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "text_delta", "text": seg},
		})
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
			PromptTokens: estimateUsage(prompt, "").PromptTokens,
			Status:       metrics.StatusError, Error: err.Error(),
		})
		s.log.Warn("messages stream error", "model", req.Model, "error", err)
		writeErr("api_error", err.Error())
		return
	}
	if strings.TrimSpace(body) == "" {
		s.met.Record(metrics.Record{
			Model: req.Model, ServedBy: servedBy, Site: servedSite, Stream: true,
			DurationMs: duration.Milliseconds(), Status: metrics.StatusEmpty, Error: "empty response",
		})
		writeErr("api_error", emptyResponseMessage)
		return
	}

	usage := estimateUsage(prompt, body)
	outText, calls := splitToolOutput(body, tools)

	index := 0
	if !buffered {
		_ = writeEvent("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
		index = 1
	} else if outText != "" {
		_ = writeEvent("content_block_start", map[string]any{
			"type": "content_block_start", "index": 0,
			"content_block": map[string]any{"type": "text", "text": ""},
		})
		_ = writeEvent("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "text_delta", "text": outText},
		})
		_ = writeEvent("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
		index = 1
	}
	for _, c := range calls {
		id := c.ID
		if id == "" {
			id = anthropic.NewID("toolu_")
		}
		_ = writeEvent("content_block_start", map[string]any{
			"type": "content_block_start", "index": index,
			"content_block": map[string]any{
				"type": "tool_use", "id": id, "name": c.Function.Name,
				"input": json.RawMessage("{}"),
			},
		})
		_ = writeEvent("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": index,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": c.Function.Arguments},
		})
		_ = writeEvent("content_block_stop", map[string]any{"type": "content_block_stop", "index": index})
		index++
	}
	_ = writeEvent("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": anthropic.StopReasonFor(calls), "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": usage.CompletionTokens},
	})
	_ = writeEvent("message_stop", map[string]any{"type": "message_stop"})

	s.met.Record(metrics.Record{
		Model: req.Model, ServedBy: servedBy, Site: servedSite, Stream: true,
		DurationMs: duration.Milliseconds(), TTFTMs: msSince(firstDelta, start),
		PromptTokens: usage.PromptTokens, CompletionTokens: usage.CompletionTokens,
		Status: metrics.StatusOK,
	})
}

// splitToolOutput peels tool calls off a raw model reply when tools were
// offered; plain replies pass through unchanged.
func splitToolOutput(body string, tools []openai.ToolDef) (string, []openai.ToolCall) {
	if len(tools) == 0 {
		return body, nil
	}
	remaining, calls, ok := openai.TryParseToolCalls(body)
	if !ok {
		return body, nil
	}
	return remaining, calls
}

func writeAnthropicError(w http.ResponseWriter, code int, msg, typ string) {
	switch code {
	case http.StatusUnauthorized:
		typ = "authentication_error"
	case http.StatusNotFound:
		typ = "not_found_error"
	case http.StatusTooManyRequests:
		typ = "rate_limit_error"
	}
	writeJSON(w, code, map[string]any{
		"type":  "error",
		"error": map[string]any{"type": typ, "message": msg},
	})
}

func writeAnthropicUpstreamError(w http.ResponseWriter, err error) {
	if isTimeout(err) {
		writeAnthropicError(w, http.StatusGatewayTimeout, err.Error(), "api_error")
		return
	}
	writeAnthropicError(w, http.StatusBadGateway, err.Error(), "api_error")
}

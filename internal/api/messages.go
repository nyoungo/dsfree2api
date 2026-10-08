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
	if !decodeBody(w, r, &req, writeAnthropicError) {
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
	m, resolvedID, ok := s.cfg.ResolveModel(req.Model)
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
	if resolvedID != req.Model {
		s.log.Info("model aliased", "requested", req.Model, "resolved", resolvedID)
		req.Model = resolvedID
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

	// With tools the raw tool-call JSON is converted into streamed tool_use
	// blocks: content_block_start once id+name are known, then one
	// input_json_delta per piece, instead of buffering the whole reply.
	var args *openai.ArgStreamer
	if len(tools) > 0 {
		args = openai.NewArgStreamer()
	}

	// The opening events are held back until the upstream produces output:
	// that keeps the HTTP status free for a failure with nothing streamed yet,
	// exactly like the non-streaming path.
	var started bool
	beginStream := func() error {
		if started {
			return nil
		}
		started = true
		if err := writeEvent("message_start", map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"id": msgID, "type": "message", "role": "assistant", "model": req.Model,
				"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
				"usage": map[string]any{
					"input_tokens":  estimateUsage(prompt, "").PromptTokens,
					"output_tokens": 0,
				},
			},
		}); err != nil {
			return err
		}
		if args == nil {
			return writeEvent("content_block_start", map[string]any{
				"type": "content_block_start", "index": 0,
				"content_block": map[string]any{"type": "text", "text": ""},
			})
		}
		return nil
	}

	// Anthropic content blocks are strictly sequential: close the open
	// tool_use block before the next one starts. Deltas that arrive for an
	// older block after a newer one opened are dropped — models emit calls
	// in order.
	var (
		blockPos = 0  // next content block index
		curIdx   = -1 // streamer index in focus
		openPos  = -1 // content index of the open tool_use block, -1 = none
		ids      = map[int]string{}
		names    = map[int]string{}
		pending  = map[int][]string{} // args held until the block can open
	)
	onToolDelta := func(d openai.ToolArgDelta) error {
		if d.Index < curIdx {
			return nil
		}
		if err := beginStream(); err != nil {
			return err
		}
		if d.ID != "" {
			ids[d.Index] = d.ID
		}
		if d.Name != "" {
			names[d.Index] = d.Name
		}
		if d.Index > curIdx {
			if openPos >= 0 {
				if err := writeEvent("content_block_stop", map[string]any{"type": "content_block_stop", "index": openPos}); err != nil {
					return err
				}
				openPos = -1
				blockPos++
			}
			curIdx = d.Index
		}
		if openPos < 0 && names[d.Index] != "" {
			id := ids[d.Index]
			if id == "" {
				id = anthropic.NewID("toolu_")
			}
			openPos = blockPos
			if err := writeEvent("content_block_start", map[string]any{
				"type": "content_block_start", "index": openPos,
				"content_block": map[string]any{
					"type": "tool_use", "id": id, "name": names[d.Index],
					"input": json.RawMessage("{}"),
				},
			}); err != nil {
				return err
			}
			for _, frag := range pending[d.Index] {
				if err := writeEvent("content_block_delta", map[string]any{
					"type": "content_block_delta", "index": openPos,
					"delta": map[string]any{"type": "input_json_delta", "partial_json": frag},
				}); err != nil {
					return err
				}
			}
			delete(pending, d.Index)
		}
		if d.Args == "" {
			return nil
		}
		if openPos < 0 {
			pending[d.Index] = append(pending[d.Index], d.Args)
			return nil
		}
		return writeEvent("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": openPos,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": d.Args},
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
		if err := beginStream(); err != nil {
			return err
		}
		text.WriteString(seg)
		if args == nil {
			return writeEvent("content_block_delta", map[string]any{
				"type": "content_block_delta", "index": 0,
				"delta": map[string]any{"type": "text_delta", "text": seg},
			})
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
			PromptTokens: estimateUsage(prompt, "").PromptTokens,
			Status:       metrics.StatusError, Error: err.Error(),
		})
		s.log.Warn("messages stream error", "model", req.Model, "error", err)
		if !started {
			// nothing on the wire yet — report a real status like the
			// non-streaming path does
			writeAnthropicUpstreamError(w, err)
			return
		}
		writeErr("api_error", err.Error())
		return
	}
	if strings.TrimSpace(body) == "" {
		s.met.Record(metrics.Record{
			Model: req.Model, ServedBy: servedBy, Site: servedSite, Stream: true,
			DurationMs: duration.Milliseconds(), Status: metrics.StatusEmpty, Error: "empty response",
		})
		if !started {
			writeAnthropicError(w, http.StatusInternalServerError, emptyResponseMessage, "api_error")
			return
		}
		writeErr("api_error", emptyResponseMessage)
		return
	}

	// Success: the opening events must precede every event below.
	if err := beginStream(); err != nil {
		return
	}

	usage := estimateUsage(prompt, body)
	outText, calls := splitToolOutput(body, tools)

	if args != nil {
		if len(calls) > 0 {
			deltas, resend, diverged := args.Finalize(calls)
			if diverged {
				s.log.Warn("streamed tool arguments diverged from repaired parse", "model", req.Model, "detail", args.Diverge())
			}
			for _, d := range deltas {
				_ = onToolDelta(d)
			}
			if openPos >= 0 {
				_ = writeEvent("content_block_stop", map[string]any{"type": "content_block_stop", "index": openPos})
				openPos = -1
				blockPos++
			}
			for _, c := range resend {
				id := c.ID
				if id == "" {
					id = anthropic.NewID("toolu_")
				}
				_ = writeEvent("content_block_start", map[string]any{
					"type": "content_block_start", "index": blockPos,
					"content_block": map[string]any{
						"type": "tool_use", "id": id, "name": c.Function.Name,
						"input": json.RawMessage("{}"),
					},
				})
				_ = writeEvent("content_block_delta", map[string]any{
					"type": "content_block_delta", "index": blockPos,
					"delta": map[string]any{"type": "input_json_delta", "partial_json": c.Function.Arguments},
				})
				_ = writeEvent("content_block_stop", map[string]any{"type": "content_block_stop", "index": blockPos})
				blockPos++
			}
		} else {
			if args.Sent() {
				s.log.Warn("tool parse failed after streaming argument deltas", "model", req.Model)
			}
			if openPos >= 0 {
				_ = writeEvent("content_block_stop", map[string]any{"type": "content_block_stop", "index": openPos})
				openPos = -1
				blockPos++
			}
		}
		if outText != "" {
			_ = writeEvent("content_block_start", map[string]any{
				"type": "content_block_start", "index": blockPos,
				"content_block": map[string]any{"type": "text", "text": ""},
			})
			_ = writeEvent("content_block_delta", map[string]any{
				"type": "content_block_delta", "index": blockPos,
				"delta": map[string]any{"type": "text_delta", "text": outText},
			})
			_ = writeEvent("content_block_stop", map[string]any{"type": "content_block_stop", "index": blockPos})
			blockPos++
		}
	} else {
		_ = writeEvent("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
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
	remaining, calls, ok := openai.TryParseToolCallsForTools(body, tools)
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

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

func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	var req openai.ResponsesRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error(), "invalid_request_error")
		return
	}
	messages, err := openai.ResponsesToMessages(&req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}
	tools := make([]openai.ToolDef, 0, len(req.Tools))
	for _, t := range req.Tools {
		if def, ok := t.ToToolDef(); ok {
			tools = append(tools, def)
		}
	}
	s.cfg.RLock()
	m, ok := s.cfg.Models[req.Model]
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

	prompt := openai.BuildPrompt(messages, openai.PromptOptions{
		Tools:       tools,
		ToolChoice:  req.ToolChoice,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		MaxTokens:   req.MaxOutputTokens,
	})

	s.met.IncInFlight(1)
	defer s.met.IncInFlight(-1)

	ctx, cancel := s.upstreamCtx(r)
	defer cancel()

	if req.Stream {
		s.streamResponses(ctx, w, req, prompt, site, tools, continuationFor(tools, messages, s.continueRounds()))
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
	writeJSON(w, http.StatusOK, map[string]any{
		"id":          "resp_" + randHex(16),
		"object":      "response",
		"created_at":  time.Now().Unix(),
		"status":      "completed",
		"model":       req.Model,
		"output":      output,
		"output_text": text,
		"stop_reason": stopReason,
		"usage":       out.usage,
	})
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

func (s *Server) streamResponses(ctx context.Context, w http.ResponseWriter, req openai.ResponsesRequest, prompt, site string, tools []openai.ToolDef, cont openai.ContinuationFunc) {
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

	_ = write(map[string]any{
		"type": "response.created",
		"response": map[string]any{
			"id": id, "model": req.Model, "status": "in_progress", "created_at": time.Now().Unix(),
		},
	})

	// When tools are requested the reply is buffered so the raw tool-call
	// JSON is parsed once and emitted as proper function_call output items.
	buffered := len(tools) > 0

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
		return write(map[string]any{"type": "response.output_text.delta", "delta": seg})
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

	usage := estimateUsage(prompt, body)
	outText, calls := splitToolOutput(body, tools)

	stopReason := "stop"
	if len(calls) > 0 {
		stopReason = "tool_use"
	}

	var output []map[string]any
	if buffered {
		// Replay the buffered reply as a proper item stream: message block
		// first (when there is prose), then one function_call item per call.
		// The final completed.response.output reuses the same item ids.
		output = make([]map[string]any, 0, len(calls)+1)
		seq := 0
		outIdx := 0
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
				"output_index": outIdx, "delta": outText, "content_index": 0,
			})
			_ = write(map[string]any{
				"type": "response.output_text.done", "item_id": itemID,
				"output_index": outIdx, "text": outText, "content_index": 0,
			})
			_ = write(map[string]any{
				"type": "response.output_item.done", "output_index": outIdx,
				"sequence_number": seq, "item": item,
			})
			seq++
			outIdx++
			output = append(output, item)
		}
		for _, c := range calls {
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
		output = responseOutputItems(outText, calls)
		_ = write(map[string]any{
			"type": "response.output_text.done",
			"text": body,
		})
	}
	_ = write(map[string]any{
		"type": "response.completed",
		"response": map[string]any{
			"id": id, "model": req.Model, "status": "completed", "created_at": time.Now().Unix(),
			"output": output, "stop_reason": stopReason, "usage": usage,
		},
	})
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

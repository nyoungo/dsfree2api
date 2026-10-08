package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nyoungo/dsfree2api/internal/metrics"
	"github.com/nyoungo/dsfree2api/internal/openai"
	"github.com/nyoungo/dsfree2api/internal/upstream"
)

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	var req openai.ChatCompletionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error(), "invalid_request_error")
		return
	}
	if req.Model == "" {
		writeError(w, http.StatusBadRequest, "model is required", "invalid_request_error")
		return
	}
	if len(req.Messages) == 0 {
		writeError(w, http.StatusBadRequest, "messages is required", "invalid_request_error")
		return
	}
	// 网关一次只回 1 条 choice；n>1 若静默忽略，客户端会以为拿到了
	// n 条候选 —— 明确拒绝。校验要排在模型查找之前。
	if req.N != nil && *req.N > 1 {
		writeError(w, http.StatusBadRequest,
			"n > 1 is not supported: this gateway returns a single choice",
			"invalid_request_error")
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
		writeError(w, http.StatusNotFound, "model not found: "+req.Model, "invalid_request_error")
		return
	}
	if resolvedID != req.Model {
		s.log.Info("model aliased", "requested", req.Model, "resolved", resolvedID)
		req.Model = resolvedID
	}
	// 客户端回放的 tool_call_id 必须非空（上游对空 id 直接 400 拒绝整个
	// 请求）：缺失的按前序 assistant tool_calls 配对回填，配不上就合成
	// 占位 id —— 不拒绝请求，修复永远发生在 BuildPrompt 之前。
	openai.BackfillToolIDs(req.Messages)

	maxTokens := req.MaxTokens
	if maxTokens == nil {
		maxTokens = req.MaxCompletionTok
	}
	prompt := openai.BuildPrompt(req.Messages, openai.PromptOptions{
		Tools:          req.Tools,
		ToolChoice:     req.ToolChoice,
		Temperature:    req.Temperature,
		TopP:           req.TopP,
		MaxTokens:      maxTokens,
		Stop:           req.Stop,
		ResponseFormat: req.ResponseFormat,
	})

	ctx, cancel := s.upstreamCtx(r)
	defer cancel()

	s.met.IncInFlight(1)
	defer s.met.IncInFlight(-1)

	cont := continuationFor(req.Tools, req.Messages, s.continueRounds())
	if req.Stream {
		s.streamChat(ctx, w, req, prompt, site, cont)
		return
	}
	s.completeChat(ctx, w, req, prompt, site, cont)
}

// chatOutcome is the result of replaying one upstream conversation.
type chatOutcome struct {
	body       string
	usage      openai.Usage
	servedBy   string
	servedSite string
	duration   time.Duration
	ttft       time.Duration
	status     metrics.Status
	err        error
}

// collect runs a full (non-streaming) conversation against the upstream site
// and records metrics for it. When cont is non-nil the reply is automatically
// continued across extra upstream turns until a truncated tool-call JSON
// closes (see openai.RunContinued).
func (s *Server) collect(ctx context.Context, modelID, prompt, site string, cont openai.ContinuationFunc) chatOutcome {
	start := time.Now()
	info := &upstream.ServeInfo{}
	var text strings.Builder
	var firstDelta time.Time

	err := s.chatWithContinue(ctx, modelID, prompt, cont, info, func() {
		if firstDelta.IsZero() {
			firstDelta = time.Now()
		}
	}, func(seg string) error {
		text.WriteString(seg)
		return nil
	})

	body := text.String()
	servedBy, servedSite, _ := info.Get()
	if servedSite == "" {
		servedSite = site
	}
	status := metrics.StatusOK
	switch {
	case err != nil:
		status = metrics.StatusError
	case strings.TrimSpace(body) == "":
		status = metrics.StatusEmpty
	}
	usage := estimateUsage(prompt, body)
	s.met.Record(metrics.Record{
		Model:            modelID,
		ServedBy:         servedBy,
		Site:             servedSite,
		Stream:           false,
		DurationMs:       time.Since(start).Milliseconds(),
		TTFTMs:           msSince(firstDelta, start),
		PromptTokens:     usage.PromptTokens,
		CompletionTokens: usage.CompletionTokens,
		Status:           status,
		Error:            errString(err),
	})
	return chatOutcome{
		body: body, usage: usage, servedBy: servedBy, servedSite: servedSite,
		duration: time.Since(start), ttft: firstDelta.Sub(start), status: status, err: err,
	}
}

func (s *Server) completeChat(ctx context.Context, w http.ResponseWriter, req openai.ChatCompletionRequest, prompt, site string, cont openai.ContinuationFunc) {
	out := s.collect(ctx, req.Model, prompt, site, cont)
	if out.err != nil {
		writeUpstreamError(w, out.err)
		return
	}
	if out.status == metrics.StatusEmpty {
		writeError(w, http.StatusBadGateway, emptyResponseMessage, "upstream_empty_response")
		return
	}
	writeJSON(w, http.StatusOK, buildCompletion(req, prompt, out.body))
}

func buildCompletion(req openai.ChatCompletionRequest, prompt, body string) *openai.ChatCompletionResponse {
	// Role 的 json tag 没有 omitempty，漏设会返回空串；与流式路径保持一致。
	message := openai.ChoiceMessage{Role: "assistant", Content: body}
	if len(req.Tools) > 0 {
		remaining, calls, ok := openai.TryParseToolCallsForTools(body, req.Tools)
		if ok {
			message.Content = remaining
			message.ToolCalls = calls
		}
	}
	return &openai.ChatCompletionResponse{
		ID:      "chatcmpl-" + randHex(16),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   req.Model,
		Choices: []openai.Choice{{
			Index:        0,
			Message:      message,
			FinishReason: finishReason(message.ToolCalls),
		}},
		Usage: estimateUsage(prompt, body),
	}
}

func finishReason(calls []openai.ToolCall) string {
	if len(calls) > 0 {
		return "tool_calls"
	}
	return "stop"
}

func (s *Server) streamChat(ctx context.Context, w http.ResponseWriter, req openai.ChatCompletionRequest, prompt, site string, cont openai.ContinuationFunc) {
	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	start := time.Now()
	id := "chatcmpl-" + randHex(16)
	created := time.Now().Unix()
	info := &upstream.ServeInfo{}
	var text strings.Builder
	var firstDelta time.Time

	// With tools the raw tool-call JSON is converted into incremental
	// tool_calls deltas instead of being buffered until the reply finishes.
	var args *openai.ArgStreamer
	if len(req.Tools) > 0 {
		args = openai.NewArgStreamer()
	}

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
	chunk := func(delta openai.ChoiceDelta, finish *string) openai.ChatCompletionChunk {
		return openai.ChatCompletionChunk{
			ID:      id,
			Object:  "chat.completion.chunk",
			Created: created,
			Model:   req.Model,
			Choices: []openai.StreamChoice{{Delta: delta, FinishReason: finish}},
		}
	}
	writeToolDelta := func(d openai.ToolArgDelta) error {
		tc := openai.ToolCallDelta{Index: d.Index}
		if d.ID != "" {
			tc.ID, tc.Type = d.ID, "function"
		}
		if d.Name != "" || d.Args != "" {
			tc.Function = &openai.FunctionDelta{Name: d.Name, Arguments: d.Args}
		}
		return write(chunk(openai.ChoiceDelta{ToolCalls: []openai.ToolCallDelta{tc}}, nil))
	}

	_ = write(chunk(openai.ChoiceDelta{Role: "assistant"}, nil))

	// in_flight 由 handleChat 统一计一次，streamChat 不再重复计数，
	// 否则控制台的并发数会按 2 倍显示。

	err := s.chatWithContinue(ctx, req.Model, prompt, cont, info, func() {
		if firstDelta.IsZero() {
			firstDelta = time.Now()
		}
	}, func(seg string) error {
		text.WriteString(seg)
		if args == nil {
			return write(chunk(openai.ChoiceDelta{Content: seg}, nil))
		}
		for _, d := range args.Feed(text.String()) {
			if err := writeToolDelta(d); err != nil {
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
		s.log.Warn("stream error", "model", req.Model, "error", err)
		_ = write(map[string]any{
			"error": map[string]any{"message": err.Error(), "type": "upstream_error"},
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
			DurationMs: duration.Milliseconds(),
			Status:     metrics.StatusEmpty, Error: "empty response",
		})
		_ = write(map[string]any{
			"error": map[string]any{"message": emptyResponseMessage, "type": "upstream_empty_response"},
		})
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		return
	}

	usage := estimateUsage(prompt, body)
	// content is only what was NOT streamed: prose around a tool JSON, or the
	// whole reply when a tools request did not parse as a tool call at all.
	// Plain text was already streamed per delta above.
	content := ""
	stop := "stop"
	var resend []openai.ToolCall
	if args == nil {
		// plain reply: content streamed live, final chunk carries no content
	} else if remaining, calls, ok := openai.TryParseToolCallsForTools(body, req.Tools); ok {
		content = remaining
		stop = "tool_calls"
		deltas, miss, diverged := args.Finalize(calls)
		if diverged {
			s.log.Warn("streamed tool arguments diverged from repaired parse", "model", req.Model, "detail", args.Diverge())
		}
		for _, d := range deltas {
			_ = writeToolDelta(d)
		}
		resend = miss
	} else {
		// tools were requested but the reply is not a tool call: it was never
		// streamed, so deliver it as content in the final chunk.
		content = body
		if args.Sent() {
			s.log.Warn("tool parse failed after streaming argument deltas", "model", req.Model)
		}
	}

	// Final chunk: content + tool_calls (or plain stop marker).
	final := map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   req.Model,
		"choices": []map[string]any{{
			"index":         0,
			"delta":         map[string]any{},
			"finish_reason": stop,
		}},
	}
	deltaPayload := map[string]any{}
	if content != "" {
		deltaPayload["content"] = content
	}
	if len(resend) > 0 {
		deltaPayload["tool_calls"] = resend
	}
	if len(deltaPayload) > 0 {
		final["choices"] = []map[string]any{{
			"index":         0,
			"delta":         deltaPayload,
			"finish_reason": stop,
		}}
	}
	_ = write(final)

	includeUsage := req.StreamOptions != nil && req.StreamOptions.IncludeUsage
	if includeUsage {
		_ = write(map[string]any{
			"id": id, "object": "chat.completion.chunk", "created": created, "model": req.Model,
			"choices": []map[string]any{},
			"usage":   usage,
		})
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

const emptyResponseMessage = "upstream returned empty response — the server may be rate-limiting, overloaded, or the session/nonce expired. Retry in a few seconds."

// upstreamCtx bounds one conversation with the configured stream timeout so a
// hung upstream can never wedge a request forever.
func (s *Server) upstreamCtx(r *http.Request) (context.Context, context.CancelFunc) {
	s.cfg.RLock()
	d := time.Duration(s.cfg.Upstream.StreamTimeout * float64(time.Second))
	s.cfg.RUnlock()
	if d <= 0 {
		d = 5 * time.Minute
	}
	return context.WithTimeout(r.Context(), d)
}

func writeUpstreamError(w http.ResponseWriter, err error) {
	code := http.StatusBadGateway
	typ := "upstream_error"
	if isTimeout(err) {
		code = http.StatusGatewayTimeout
		typ = "upstream_timeout"
	}
	writeError(w, code, err.Error(), typ)
}

func isTimeout(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "timeout") || strings.Contains(msg, "deadline exceeded")
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func estimateUsage(prompt, text string) openai.Usage {
	var pt, ct int
	if prompt != "" {
		pt = utf8.RuneCountInString(prompt) / 4
		if pt < 1 {
			pt = 1
		}
	}
	if text != "" {
		ct = utf8.RuneCountInString(text) / 4
		if ct < 1 {
			ct = 1
		}
	}
	return openai.Usage{PromptTokens: pt, CompletionTokens: ct, TotalTokens: pt + ct}
}

func msSince(t, fallback time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	d := t.Sub(fallback)
	if d < 0 {
		return 0
	}
	return d.Milliseconds()
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return strings.Repeat("0", n*2)
	}
	return hex.EncodeToString(b)
}

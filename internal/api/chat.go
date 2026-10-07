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
		writeError(w, http.StatusNotFound, "model not found: "+req.Model, "invalid_request_error")
		return
	}

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
	message := openai.ChoiceMessage{Content: body}
	if len(req.Tools) > 0 {
		remaining, calls, ok := openai.TryParseToolCalls(body)
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

	// When tools are requested the whole reply is buffered so the raw
	// tool-call JSON is never streamed to the client twice.
	buffered := len(req.Tools) > 0

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

	_ = write(chunk(openai.ChoiceDelta{Role: "assistant"}, nil))

	s.met.IncInFlight(1)
	defer s.met.IncInFlight(-1)

	err := s.chatWithContinue(ctx, req.Model, prompt, cont, info, func() {
		if firstDelta.IsZero() {
			firstDelta = time.Now()
		}
	}, func(seg string) error {
		text.WriteString(seg)
		if buffered {
			return nil
		}
		return write(chunk(openai.ChoiceDelta{Content: seg}, nil))
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
	var toolCalls []openai.ToolCall
	content := body
	if buffered {
		remaining, calls, ok := openai.TryParseToolCalls(body)
		if ok {
			content = remaining
			toolCalls = calls
		}
	}
	stop := finishReason(toolCalls)

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
	if len(toolCalls) > 0 {
		deltaPayload["tool_calls"] = toolCalls
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

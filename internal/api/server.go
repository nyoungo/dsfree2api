package api

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/nyoungo/dsfree2api/internal/config"
	"github.com/nyoungo/dsfree2api/internal/logbuf"
	"github.com/nyoungo/dsfree2api/internal/metrics"
	"github.com/nyoungo/dsfree2api/internal/turnstile"
	"github.com/nyoungo/dsfree2api/internal/upstream"
)

type Server struct {
	cfg   *config.Config
	up    *upstream.Client
	met   *metrics.Recorder
	logs  *logbuf.Buffer
	ts    *turnstile.Solver
	log   *slog.Logger
	lim   *limiter
	start time.Time
	// rstore backs previous_response_id / GET /v1/responses/{id}.
	rstore *responseStore
	// idem backs Idempotency-Key replay.
	idem *idempotencyStore
}

func New(cfg *config.Config, up *upstream.Client, met *metrics.Recorder, logs *logbuf.Buffer, ts *turnstile.Solver, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{
		cfg: cfg, up: up, met: met, logs: logs, ts: ts, log: log,
		lim:    newLimiter(),
		start:  time.Now(),
		rstore: newResponseStore(cfg.Responses.StoreCapacityValue(), cfg.Responses.StoreTTLValue()),
		idem:   newIdempotencyStore(),
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /{$}", s.handleRoot)
	mux.HandleFunc("GET /v1/models", s.authed(s.handleModels))
	mux.HandleFunc("POST /v1/chat/completions", s.authed(s.handleChat))
	mux.HandleFunc("POST /v1/responses", s.authed(s.handleResponses))
	mux.HandleFunc("GET /v1/responses/{id}", s.authed(s.handleGetResponse))
	mux.HandleFunc("POST /v1/messages", s.authedAnthropic(s.handleMessages))
	return s.recoverPanic(s.logRequest(s.idempotency(mux)))
}

func (s *Server) handleRoot(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "service": "dsfree2api"})
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	s.cfg.RLock()
	models, sites := 0, []string{}
	for id, m := range s.cfg.Models {
		if m.Enabled {
			models++
		}
		_ = id
	}
	for code, site := range s.cfg.Sites {
		if site.Enabled {
			sites = append(sites, code)
		}
	}
	turnstileOn := s.cfg.Turnstile.Enabled
	s.cfg.RUnlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"status":    "ok",
		"models":    models,
		"sites":     sites,
		"turnstile": turnstileOn,
		"uptime_s":  int(time.Since(s.start).Seconds()),
	})
}

func (s *Server) handleModels(w http.ResponseWriter, _ *http.Request) {
	s.cfg.RLock()
	now := time.Now().Unix()
	data := make([]map[string]any, 0, len(s.cfg.Models))
	seen := make(map[string]struct{}, len(s.cfg.Models))
	for id, m := range s.cfg.Models {
		if !m.Enabled {
			continue
		}
		seen[strings.ToLower(id)] = struct{}{}
		data = append(data, map[string]any{
			"id": id, "object": "model", "created": now, "owned_by": "deepseek-" + m.Site,
		})
	}
	// Aliases are resolvable model names too: listing them lets Codex CLI /
	// Claude Code discover that their arbitrary model name works here.
	for alias, target := range s.cfg.ModelAliases {
		if alias == "" {
			continue
		}
		if _, dup := seen[strings.ToLower(alias)]; dup {
			continue
		}
		m, ok := s.cfg.Models[target]
		if !ok || !m.Enabled {
			continue
		}
		seen[strings.ToLower(alias)] = struct{}{}
		data = append(data, map[string]any{
			"id": alias, "object": "model", "created": now, "owned_by": "deepseek-" + m.Site,
		})
	}
	s.cfg.RUnlock()
	sortModelInfo(data)
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// ── middleware ───────────────────────────────────────────────────

// errWriter writes an auth/rate-limit failure in the wire format of the
// endpoint family it guards.
type errWriter func(w http.ResponseWriter, code int, msg, typ string)

func (s *Server) authed(next http.HandlerFunc) http.HandlerFunc {
	return s.authedWith(next, writeError)
}

// authedAnthropic guards Anthropic-format endpoints, so failures come back as
// {"type":"error","error":{...}} instead of the OpenAI error shape.
func (s *Server) authedAnthropic(next http.HandlerFunc) http.HandlerFunc {
	return s.authedWith(next, writeAnthropicError)
}

func (s *Server) authedWith(next http.HandlerFunc, fail errWriter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.cfg.RLock()
		keys := append([]string(nil), s.cfg.Security.APIKeys...)
		s.cfg.RUnlock()

		if len(keys) > 0 {
			token := extractKey(r)
			if token == "" || !matchesAny(keys, token) {
				s.met.Record(metrics.Record{Status: metrics.StatusDenied})
				fail(w, http.StatusUnauthorized, "invalid api key", "invalid_request_error")
				return
			}
			s.cfg.RLock()
			rate := int(s.cfg.Limits.RatePerMinute)
			s.cfg.RUnlock()
			if !s.lim.allowWithLimit(token, rate) {
				s.met.Record(metrics.Record{Status: metrics.StatusRateLimit})
				fail(w, http.StatusTooManyRequests, "rate limit exceeded", "rate_limit_error")
				return
			}
		}
		next(w, r)
	}
}

func extractKey(r *http.Request) string {
	if v := r.Header.Get("X-Api-Key"); v != "" {
		return strings.TrimSpace(v)
	}
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return ""
	}
	scheme, value, ok := strings.Cut(auth, " ")
	if !ok || !strings.EqualFold(strings.TrimSpace(scheme), "bearer") {
		return ""
	}
	return strings.TrimSpace(value)
}

func matchesAny(keys []string, token string) bool {
	ok := false
	for _, k := range keys {
		if subtle.ConstantTimeCompare([]byte(k), []byte(token)) == 1 {
			ok = true
		}
	}
	return ok
}

func (s *Server) logRequest(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			next.ServeHTTP(w, r)
			return
		}
		rid := strings.TrimSpace(r.Header.Get("X-Request-Id"))
		if rid == "" {
			rid = "req_" + randHex(8)
		}
		w.Header().Set("X-Request-Id", rid)
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		defer func() {
			if r.URL.Path != "/" {
				s.log.Debug("http",
					"req", rid, "method", r.Method, "path", r.URL.Path,
					"status", sw.status, "ms", time.Since(start).Milliseconds())
			}
		}()
		next.ServeHTTP(sw, r)
	})
}

func (s *Server) recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tw := &headerTracker{ResponseWriter: w}
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("panic", "err", rec, "stack", string(debug.Stack()))
				// 响应头已经发出去（SSE 已开流）就不能再改状态码：硬写 500
				// 只会把错误 JSON 拼进事件流，客户端解析流会直接失败。
				if !tw.wroteHeader {
					writeJSON(tw, http.StatusInternalServerError, map[string]any{
						"error": map[string]any{"message": "internal server error", "type": "server_error"},
					})
				}
			}
		}()
		next.ServeHTTP(tw, r)
	})
}

// headerTracker 记录响应头是否已发出，并把 Flush 透传给底层 writer。
type headerTracker struct {
	http.ResponseWriter
	wroteHeader bool
}

func (w *headerTracker) WriteHeader(code int) {
	w.wroteHeader = true
	w.ResponseWriter.WriteHeader(code)
}

func (w *headerTracker) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

func (w *headerTracker) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// ── helpers ──────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError renders an OpenAI-shaped error. The OpenAI schema always carries
// param and code (null when not applicable); clients such as the OpenAI SDKs
// read code to distinguish e.g. model_not_found from a generic 404.
func writeError(w http.ResponseWriter, code int, msg, typ string) {
	param, errCode := errorParamCode(code, typ, msg)
	body := map[string]any{"message": msg, "type": typ}
	if param != "" {
		body["param"] = param
	} else {
		body["param"] = nil
	}
	if errCode != "" {
		body["code"] = errCode
	} else {
		body["code"] = nil
	}
	writeJSON(w, code, map[string]any{"error": body})
}

// errorParamCode maps an error onto the OpenAI (param, code) pair.
func errorParamCode(status int, typ, msg string) (param, code string) {
	if typ == "idempotency_error" {
		return "", "idempotency_error"
	}
	switch {
	case status == http.StatusUnauthorized:
		return "", "invalid_api_key"
	case status == http.StatusNotFound && strings.HasPrefix(msg, "model not found"):
		return "model", "model_not_found"
	case status == http.StatusTooManyRequests:
		return "", "rate_limit_exceeded"
	case status == http.StatusRequestEntityTooLarge:
		return "", "request_too_large"
	case status == http.StatusBadGateway:
		if typ == "upstream_empty_response" {
			return "", "upstream_empty_response"
		}
		return "", "upstream_error"
	case status == http.StatusGatewayTimeout:
		return "", "upstream_timeout"
	}
	return "", ""
}

// maxRequestBodyBytes caps every public endpoint's body: these endpoints are
// unauthenticated when api_keys is empty, so an uncapped body would let one
// client pin a connection and allocate without bound.
const maxRequestBodyBytes = 8 << 20

type errorWriter func(w http.ResponseWriter, code int, msg, typ string)

// decodeBody caps the request body before JSON-decoding it and reports any
// failure through the caller's error shape (OpenAI vs Anthropic). It returns
// false after having written the error response.
func decodeBody(w http.ResponseWriter, r *http.Request, dst any, fail errorWriter) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	err := json.NewDecoder(r.Body).Decode(dst)
	if err == nil {
		return true
	}
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		fail(w, http.StatusRequestEntityTooLarge, "request body too large", "invalid_request_error")
		return false
	}
	fail(w, http.StatusBadRequest, "invalid JSON body: "+err.Error(), "invalid_request_error")
	return false
}

func sortModelInfo(data []map[string]any) {
	for i := 1; i < len(data); i++ {
		for j := i; j > 0; j-- {
			a, _ := data[j]["id"].(string)
			b, _ := data[j-1]["id"].(string)
			if a < b {
				data[j], data[j-1] = data[j-1], data[j]
			} else {
				break
			}
		}
	}
}

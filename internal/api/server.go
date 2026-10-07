package api

import (
	"crypto/subtle"
	"encoding/json"
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
}

func New(cfg *config.Config, up *upstream.Client, met *metrics.Recorder, logs *logbuf.Buffer, ts *turnstile.Solver, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{
		cfg: cfg, up: up, met: met, logs: logs, ts: ts, log: log,
		lim:   newLimiter(),
		start: time.Now(),
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /{$}", s.handleRoot)
	mux.HandleFunc("GET /v1/models", s.authed(s.handleModels))
	mux.HandleFunc("POST /v1/chat/completions", s.authed(s.handleChat))
	mux.HandleFunc("POST /v1/responses", s.authed(s.handleResponses))
	mux.HandleFunc("POST /v1/messages", s.authedAnthropic(s.handleMessages))
	return s.recoverPanic(s.logRequest(mux))
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
	for id, m := range s.cfg.Models {
		if !m.Enabled {
			continue
		}
		data = append(data, map[string]any{
			"id": id, "object": "model", "created": now, "owned_by": "deepseek-" + m.Site,
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
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		defer func() {
			if r.URL.Path != "/" {
				s.log.Debug("http",
					"method", r.Method, "path", r.URL.Path,
					"status", sw.status, "ms", time.Since(start).Milliseconds())
			}
		}()
		next.ServeHTTP(sw, r)
	})
}

func (s *Server) recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("panic", "err", rec, "stack", string(debug.Stack()))
				writeJSON(w, http.StatusInternalServerError, map[string]any{
					"error": map[string]any{"message": "internal server error", "type": "server_error"},
				})
			}
		}()
		next.ServeHTTP(w, r)
	})
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

func writeError(w http.ResponseWriter, code int, msg, typ string) {
	writeJSON(w, code, map[string]any{
		"error": map[string]any{"message": msg, "type": typ},
	})
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

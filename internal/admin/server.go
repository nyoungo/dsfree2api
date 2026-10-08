package admin

import (
	"embed"
	"encoding/json"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/nyoungo/dsfree2api/internal/config"
	"github.com/nyoungo/dsfree2api/internal/logbuf"
	"github.com/nyoungo/dsfree2api/internal/metrics"
	"github.com/nyoungo/dsfree2api/internal/proxypool"
	"github.com/nyoungo/dsfree2api/internal/quota"
	"github.com/nyoungo/dsfree2api/internal/turnstile"
	"github.com/nyoungo/dsfree2api/internal/upstream"
)

// Version is reported by the console and /api/overview.
var Version = "0.6.3"

//go:embed web/*
var webFS embed.FS

type Server struct {
	cfg   *config.Config
	up    *upstream.Client
	met   *metrics.Recorder
	logs  *logbuf.Buffer
	ts    *turnstile.Solver
	pool  *proxypool.Manager
	quota *quota.Watcher
	log   *slog.Logger
	start time.Time

	mu       sync.Mutex
	tokens   map[string]time.Time
	password string
}

func New(cfg *config.Config, up *upstream.Client, met *metrics.Recorder, logs *logbuf.Buffer, ts *turnstile.Solver, log *slog.Logger, pool *proxypool.Manager, qw *quota.Watcher) *Server {
	if log == nil {
		log = slog.Default()
	}
	pw := cfg.Admin.Password
	generated := false
	if pw == "" {
		pw = randomToken(12)
		generated = true
	}
	s := &Server{
		cfg: cfg, up: up, met: met, logs: logs, ts: ts, pool: pool, quota: qw, log: log,
		start: time.Now(), tokens: map[string]time.Time{}, password: pw,
	}
	if generated {
		s.log.Warn("admin password was not configured — generated a one-time password",
			"password", pw, "hint", "set admin.password in config.toml or ADMIN_PASSWORD to make it stable")
	}
	return s
}

func (s *Server) Handler() http.Handler {
	sub, _ := fs.Sub(webFS, "web")
	static := http.FileServer(http.FS(sub))

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("GET /api/session", s.handleSession)
	mux.HandleFunc("POST /api/logout", s.handleLogout)
	mux.HandleFunc("GET /api/overview", s.authed(s.handleOverview))
	mux.HandleFunc("GET /api/config", s.authed(s.handleGetConfig))
	mux.HandleFunc("PUT /api/config", s.authed(s.handlePutConfig))
	mux.HandleFunc("POST /api/actions", s.authed(s.handleActions))
	mux.HandleFunc("GET /api/logs", s.authed(s.handleLogs))
	mux.HandleFunc("POST /api/playground", s.authed(s.handlePlayground))
	mux.Handle("GET /", static)
	return mux
}

// ── auth ─────────────────────────────────────────────────────────

func (s *Server) authed(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := r.Header.Get("X-Admin-Token")
		if tok == "" {
			if c, err := r.Cookie("dsfr_admin"); err == nil {
				tok = c.Value
			}
		}
		if tok == "" {
			if scheme, value, ok := strings.Cut(r.Header.Get("Authorization"), " "); ok &&
				strings.EqualFold(strings.TrimSpace(scheme), "bearer") {
				tok = strings.TrimSpace(value)
			}
		}
		if !s.validToken(tok) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
			return
		}
		next(w, r)
	}
}

func (s *Server) validToken(tok string) bool {
	if tok == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.tokens[tok]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(s.tokens, tok)
		return false
	}
	return true
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Password string `json:"password"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body.Password != s.password {
		time.Sleep(300 * time.Millisecond)
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid password"})
		return
	}
	tok := randomToken(24)
	s.mu.Lock()
	for k, exp := range s.tokens {
		if time.Now().After(exp) {
			delete(s.tokens, k)
		}
	}
	s.tokens[tok] = time.Now().Add(24 * time.Hour)
	s.mu.Unlock()

	http.SetCookie(w, &http.Cookie{
		Name: "dsfr_admin", Value: tok, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, MaxAge: 86400,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "token": tok})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie("dsfr_admin"); err == nil {
		s.mu.Lock()
		delete(s.tokens, c.Value)
		s.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: "dsfr_admin", Value: "", Path: "/", MaxAge: -1})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	tok := ""
	if c, err := r.Cookie("dsfr_admin"); err == nil {
		tok = c.Value
	}
	if h := r.Header.Get("X-Admin-Token"); h != "" {
		tok = h
	}
	writeJSON(w, http.StatusOK, map[string]any{"authenticated": s.validToken(tok)})
}

// ── overview ─────────────────────────────────────────────────────

func (s *Server) handleOverview(w http.ResponseWriter, _ *http.Request) {
	s.cfg.RLock()
	models := make([]map[string]any, 0, len(s.cfg.Models))
	for id, m := range s.cfg.Models {
		models = append(models, map[string]any{
			"id": id, "site": m.Site, "label": m.Label, "upstream_id": m.UpstreamID,
			"page_path": m.PagePath, "bot_id": m.BotID, "post_id": m.PostID, "enabled": m.Enabled,
		})
	}
	sites := make([]map[string]any, 0, len(s.cfg.Sites))
	siteCodes := make([]string, 0, len(s.cfg.Sites))
	for code, st := range s.cfg.Sites {
		siteCodes = append(siteCodes, code)
		sites = append(sites, map[string]any{
			"code": code, "base_url": st.BaseURL, "ajax_url": st.AJAXURL,
			"language": st.Language, "sitekey": st.SiteKey, "enabled": st.Enabled,
			"verify_action": st.VerifyAction, "proxies": st.Proxies,
		})
	}
	proxyCfg := struct {
		URL       string   `json:"url"`
		Fallbacks []string `json:"fallbacks"`
		SlowStart float64  `json:"slow_start"`
		MaxConc   int      `json:"max_concurrent"`
		Rate      float64  `json:"rate_per_minute"`
		CrossSite bool     `json:"cross_site_failover"`
	}{s.cfg.Proxy.URL, s.cfg.Proxy.FallbackURLs, s.cfg.Proxy.SlowStartSeconds,
		s.cfg.Limits.MaxConcurrentPerSite, s.cfg.Limits.RatePerMinute, s.cfg.Upstream.CrossSiteFailover}
	turnstileCfg := s.cfg.Turnstile
	// The console never renders the solver key (its field is "type to
	// replace"), so it must not ship every 5s with the dashboard poll either.
	turnstileCfg.APIKey = ""
	keys := append([]string(nil), s.cfg.Security.APIKeys...)
	s.cfg.RUnlock()

	var poolStatus any
	if s.pool != nil {
		poolStatus = s.pool.Status()
	}
	var quotaStatus any
	if s.quota != nil {
		quotaStatus = s.quota.Snapshot()
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"metrics":   s.met.Snapshot(),
		"models":    models,
		"sites":     sites,
		"proxy":     proxyCfg,
		"turnstile": map[string]any{"config": turnstileCfg, "status": s.ts.Status(siteCodes), "history": s.ts.History(), "pool": s.ts.PoolStatus()},
		"proxypool": poolStatus,
		"quota":     quotaStatus,
		"keys":      keys,
		"cache":     map[string]any{"chat_config_entries": s.up.ConfigCacheSize(), "routes": routeList(s.up)},
		"system": map[string]any{
			"version": Version, "go": runtime.Version(), "os": runtime.GOOS, "arch": runtime.GOARCH,
			"uptime_s": int(time.Since(s.start).Seconds()), "pid": os.Getpid(),
			"config_path": s.cfg.Path(),
		},
	})
}

func routeList(c *upstream.Client) []map[string]string {
	routes := c.Routes()
	out := make([]map[string]string, 0, len(routes))
	for _, r := range routes {
		label := r.Proxy
		if label == "" {
			label = "direct"
		}
		out = append(out, map[string]string{"name": r.Name, "proxy": label})
	}
	return out
}

// ── config get/put ───────────────────────────────────────────────

func (s *Server) handleGetConfig(w http.ResponseWriter, _ *http.Request) {
	s.cfg.RLock()
	raw, err := json.Marshal(s.cfg)
	s.cfg.RUnlock()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	// The console never displays these two secrets (both inputs are "leave
	// blank to keep"), so there is no reason to hand them out; an empty value
	// round-tripping through PUT is interpreted as "unchanged".
	if admin, ok := out["admin"].(map[string]any); ok {
		admin["password"] = ""
	}
	if ts, ok := out["turnstile"].(map[string]any); ok {
		ts["api_key"] = ""
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handlePutConfig(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r, 1<<20)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	// ReplaceJSON validates first and rolls back on rejection, so a bad
	// payload can never leave half-applied state in the running config.
	if err := s.cfg.ReplaceJSON(body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	s.applyRuntime()
	if err := s.cfg.Save(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// applyRuntime pushes a freshly edited config into the live subsystems.
func (s *Server) applyRuntime() {
	s.ts.SetConfig(s.currentTurnstile())
	if s.pool != nil {
		s.pool.Reload()
	}
}

func (s *Server) currentTurnstile() config.Turnstile {
	s.cfg.RLock()
	defer s.cfg.RUnlock()
	return s.cfg.Turnstile
}

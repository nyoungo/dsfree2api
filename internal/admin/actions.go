package admin

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/nyoungo/dsfree2api/internal/config"
	"github.com/nyoungo/dsfree2api/internal/httpx"
	"github.com/nyoungo/dsfree2api/internal/openai"
	"github.com/nyoungo/dsfree2api/internal/proxypool"
	"github.com/nyoungo/dsfree2api/internal/upstream"
)

func (s *Server) handleActions(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r, 1<<20)
	if err != nil {
		fail(w, err.Error())
		return
	}
	var req struct {
		Action string `json:"action"`
	}
	if err := json.Unmarshal(body, &req); err != nil || req.Action == "" {
		fail(w, "action is required")
		return
	}

	switch req.Action {
	case "create_key":
		key := "sk-" + randomToken(24)
		s.cfg.Lock()
		s.cfg.Security.APIKeys = append(s.cfg.Security.APIKeys, key)
		s.cfg.Unlock()
		s.save(w)
	case "delete_key":
		var p struct {
			Key string `json:"key"`
		}
		_ = json.Unmarshal(body, &p)
		s.cfg.Lock()
		out := s.cfg.Security.APIKeys[:0]
		for _, k := range s.cfg.Security.APIKeys {
			if k != p.Key {
				out = append(out, k)
			}
		}
		s.cfg.Security.APIKeys = out
		s.cfg.Unlock()
		s.save(w)

	case "toggle_model", "toggle_site":
		var p struct {
			ID      string `json:"id"`
			Enabled bool   `json:"enabled"`
		}
		_ = json.Unmarshal(body, &p)
		s.cfg.Lock()
		var e error
		if req.Action == "toggle_model" {
			if m, ok := s.cfg.Models[p.ID]; ok {
				m.Enabled = p.Enabled
			} else {
				e = fmt.Errorf("model %q not found", p.ID)
			}
		} else {
			if st, ok := s.cfg.Sites[p.ID]; ok {
				st.Enabled = p.Enabled
			} else {
				e = fmt.Errorf("site %q not found", p.ID)
			}
		}
		s.cfg.Unlock()
		if e != nil {
			fail(w, e.Error())
			return
		}
		s.save(w)

	case "update_model":
		var p struct {
			ID       string `json:"id"`
			Label    string `json:"label"`
			PagePath string `json:"page_path"`
			BotID    int    `json:"bot_id"`
			PostID   int    `json:"post_id"`
		}
		_ = json.Unmarshal(body, &p)
		s.cfg.Lock()
		m, ok := s.cfg.Models[p.ID]
		if ok {
			if p.Label != "" {
				m.Label = p.Label
			}
			if p.PagePath != "" {
				m.PagePath = p.PagePath
			}
			m.BotID, m.PostID = p.BotID, p.PostID
		}
		s.cfg.Unlock()
		if !ok {
			fail(w, "model not found")
			return
		}
		s.up.InvalidateAll()
		s.save(w)

	case "update_site":
		var p struct {
			Code     string `json:"code"`
			BaseURL  string `json:"base_url"`
			AJAXURL  string `json:"ajax_url"`
			Language string `json:"language"`
			SiteKey  string `json:"sitekey"`
		}
		_ = json.Unmarshal(body, &p)
		s.cfg.Lock()
		st, ok := s.cfg.Sites[p.Code]
		if ok {
			if p.BaseURL != "" {
				st.BaseURL = strings.TrimRight(p.BaseURL, "/")
				if p.AJAXURL == "" {
					st.AJAXURL = st.BaseURL + "/wp-admin/admin-ajax.php"
				}
			}
			if p.AJAXURL != "" {
				st.AJAXURL = p.AJAXURL
			}
			if p.Language != "" {
				st.Language = p.Language
			}
			if p.SiteKey != "" {
				st.SiteKey = p.SiteKey
			}
		}
		s.cfg.Unlock()
		if !ok {
			fail(w, "site not found")
			return
		}
		s.up.InvalidateAll()
		s.save(w)

	case "set_primary":
		var p struct {
			URL string `json:"url"`
		}
		_ = json.Unmarshal(body, &p)
		s.cfg.Lock()
		s.cfg.Proxy.URL = strings.TrimSpace(p.URL)
		s.cfg.Unlock()
		s.save(w)

	case "add_fallback":
		var p struct {
			URL string `json:"url"`
		}
		_ = json.Unmarshal(body, &p)
		p.URL = strings.TrimSpace(p.URL)
		if p.URL == "" {
			fail(w, "url is required")
			return
		}
		s.cfg.Lock()
		for _, u := range s.cfg.Proxy.FallbackURLs {
			if u == p.URL {
				s.cfg.Unlock()
				fail(w, "fallback already exists")
				return
			}
		}
		s.cfg.Proxy.FallbackURLs = append(s.cfg.Proxy.FallbackURLs, p.URL)
		s.cfg.Unlock()
		s.save(w)

	case "remove_fallback":
		var p struct {
			URL string `json:"url"`
		}
		_ = json.Unmarshal(body, &p)
		s.cfg.Lock()
		out := s.cfg.Proxy.FallbackURLs[:0]
		for _, u := range s.cfg.Proxy.FallbackURLs {
			if u != p.URL {
				out = append(out, u)
			}
		}
		s.cfg.Proxy.FallbackURLs = out
		s.cfg.Unlock()
		s.save(w)

	case "set_limits":
		var p struct {
			SlowStart      float64 `json:"slow_start"`
			MaxConcurrent  int     `json:"max_concurrent"`
			RatePerMinute  float64 `json:"rate_per_minute"`
			CrossSite      *bool   `json:"cross_site_failover"`
			AutoRefresh    *bool   `json:"auto_refresh"`
			RefreshRetries *int    `json:"refresh_retries"`
		}
		_ = json.Unmarshal(body, &p)
		s.cfg.Lock()
		if p.SlowStart >= 0 {
			s.cfg.Proxy.SlowStartSeconds = p.SlowStart
		}
		s.cfg.Limits.MaxConcurrentPerSite = p.MaxConcurrent
		s.cfg.Limits.RatePerMinute = p.RatePerMinute
		if p.CrossSite != nil {
			s.cfg.Upstream.CrossSiteFailover = *p.CrossSite
		}
		if p.AutoRefresh != nil {
			s.cfg.Upstream.AutoRefresh = *p.AutoRefresh
		}
		if p.RefreshRetries != nil {
			s.cfg.Upstream.RefreshRetries = *p.RefreshRetries
		}
		s.cfg.Unlock()
		s.save(w)

	case "set_turnstile":
		var p struct {
			Enabled   bool    `json:"enabled"`
			Provider  *string `json:"provider"`
			APIURL    string  `json:"api_url"`
			APIKey    string  `json:"api_key"`
			SiteKey   string  `json:"sitekey"`
			Challenge string  `json:"challenge_action"`
			Legacy    string  `json:"ts_action"`
			CookieTT  int     `json:"cookie_ttl_seconds"`

			BrowserPath        *string `json:"browser_path"`
			BrowserHeadless    *bool   `json:"browser_headless"`
			BrowserUserDataDir *string `json:"browser_user_data_dir"`
			BrowserTimezone    *string `json:"browser_timezone"`
			BrowserLocale      *string `json:"browser_locale"`
		}
		_ = json.Unmarshal(body, &p)
		// Build the candidate config first: a rejected save must not leave
		// half-applied fields behind in memory.
		s.cfg.Lock()
		tc := s.cfg.Turnstile
		if p.Provider != nil {
			tc.Provider = strings.ToLower(strings.TrimSpace(*p.Provider))
		}
		if p.BrowserPath != nil {
			tc.BrowserPath = strings.TrimSpace(*p.BrowserPath)
		}
		if p.BrowserHeadless != nil {
			tc.BrowserHeadless = *p.BrowserHeadless
		}
		if p.BrowserUserDataDir != nil {
			tc.BrowserUserDataDir = strings.TrimSpace(*p.BrowserUserDataDir)
		}
		if p.BrowserTimezone != nil {
			tc.BrowserTimezone = strings.TrimSpace(*p.BrowserTimezone)
		}
		if p.BrowserLocale != nil {
			tc.BrowserLocale = strings.TrimSpace(*p.BrowserLocale)
		}
		if strings.TrimSpace(p.APIURL) != "" {
			tc.APIURL = strings.TrimSpace(p.APIURL)
		}
		if p.APIKey != "" {
			tc.APIKey = p.APIKey
		}
		if p.SiteKey != "" {
			tc.SiteKey = p.SiteKey
		}
		if act := firstNonEmpty(p.Challenge, p.Legacy); act != "" {
			tc.Action = act
		}
		if p.CookieTT > 0 {
			tc.CookieTTLSeconds = p.CookieTT
		}
		tc.Enabled = p.Enabled
		if tc.Enabled {
			// provider-specific requirements mirror config.Validate()
			switch tc.ProviderValue() {
			case config.ProviderAPI:
				if strings.TrimSpace(tc.APIURL) == "" {
					s.cfg.Unlock()
					fail(w, "turnstile.api_url is empty — enter your solver endpoint, or switch provider to browser/manual")
					return
				}
			case config.ProviderBrowser:
				if tc.BrowserPath == "" {
					s.cfg.Unlock()
					fail(w, "turnstile.browser_path is empty — point it at Chrome/Edge, or switch provider")
					return
				}
			case config.ProviderManual:
			default:
				prov := tc.Provider
				s.cfg.Unlock()
				fail(w, fmt.Sprintf("unknown provider %q (api|browser|manual)", prov))
				return
			}
		}
		s.cfg.Turnstile = tc
		s.cfg.Unlock()
		s.ts.SetConfig(tc)
		s.save(w)

	case "refresh_cookies":
		var p struct {
			Site string `json:"site"`
		}
		_ = json.Unmarshal(body, &p)
		if !s.ts.Enabled() {
			fail(w, "turnstile solver is disabled — enable it first, or import cookies manually")
			return
		}
		if s.ts.Config().ProviderValue() == config.ProviderManual {
			fail(w, `provider is "manual" — re-import cookies below instead of forcing a refresh`)
			return
		}
		for _, code := range s.siteCodes(p.Site) {
			s.ts.Invalidate(code)
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})

	case "clear_cookies":
		var p struct {
			Site string `json:"site"`
		}
		_ = json.Unmarshal(body, &p)
		for _, code := range s.siteCodes(p.Site) {
			s.ts.Invalidate(code)
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})

	case "import_cookies":
		var p struct {
			Site    string `json:"site"`
			Cookies string `json:"cookies"`
		}
		_ = json.Unmarshal(body, &p)
		s.cfg.RLock()
		site, ok := s.cfg.Sites[p.Site]
		proxy := s.cfg.Proxy.URL
		s.cfg.RUnlock()
		if !ok || site == nil {
			fail(w, "unknown site: "+p.Site)
			return
		}
		exp, n, err := s.ts.ImportCookies(site.Code, p.Cookies, proxy)
		if err != nil {
			fail(w, err.Error())
			return
		}
		s.log.Info("turnstile cookies imported", "site", site.Code, "count", n, "expires_at", exp)
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "site": site.Code, "cookie_count": n, "expires_at": exp.Unix(),
		})

	case "invalidate":
		s.up.InvalidateAll()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})

	case "reset_metrics":
		s.met.Reset()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})

	case "test_proxy":
		var p struct {
			URL string `json:"url"`
		}
		_ = json.Unmarshal(body, &p)
		result := s.testProxy(p.URL)
		writeJSON(w, http.StatusOK, result)

	case "probe_site":
		var p struct {
			Site string `json:"site"`
		}
		_ = json.Unmarshal(body, &p)
		writeJSON(w, http.StatusOK, s.probeSite(p.Site))

	case "site_balance":
		var p struct {
			Site string `json:"site"`
		}
		_ = json.Unmarshal(body, &p)
		writeJSON(w, http.StatusOK, s.siteBalance(p.Site))

	case "rotate_guest":
		var p struct {
			Site string `json:"site"`
		}
		_ = json.Unmarshal(body, &p)
		writeJSON(w, http.StatusOK, s.rotateGuest(p.Site))

	case "pool_set_config":
		var p struct {
			Enabled              *bool   `json:"enabled"`
			CheckIntervalSeconds *int    `json:"check_interval_seconds"`
			CheckTimeoutSeconds  *int    `json:"check_timeout_seconds"`
			CheckURL             *string `json:"check_url"`
			DefaultScheme        *string `json:"default_scheme"`
			XrayPath             *string `json:"xray_path"`
			XrayVersion          *string `json:"xray_version"`
			XrayAutoDownload     *bool   `json:"xray_auto_download"`
		}
		_ = json.Unmarshal(body, &p)
		s.cfg.Lock()
		pc := s.cfg.ProxyPool
		if p.Enabled != nil {
			pc.Enabled = *p.Enabled
		}
		if p.CheckIntervalSeconds != nil {
			pc.CheckIntervalSeconds = clampInt(*p.CheckIntervalSeconds, 10, 3600)
		}
		if p.CheckTimeoutSeconds != nil {
			pc.CheckTimeoutSeconds = clampInt(*p.CheckTimeoutSeconds, 2, 120)
		}
		if p.CheckURL != nil {
			pc.CheckURL = strings.TrimSpace(*p.CheckURL)
		}
		if p.DefaultScheme != nil {
			pc.DefaultScheme = strings.ToLower(strings.TrimSpace(*p.DefaultScheme))
		}
		if p.XrayPath != nil {
			pc.XrayPath = strings.TrimSpace(*p.XrayPath)
		}
		if p.XrayVersion != nil {
			pc.XrayVersion = strings.TrimSpace(*p.XrayVersion)
		}
		if p.XrayAutoDownload != nil {
			pc.XrayAutoDownload = *p.XrayAutoDownload
		}
		s.cfg.ProxyPool = pc
		s.cfg.Unlock()
		s.poolReload()
		s.save(w)

	case "pool_add_entry":
		var p struct {
			Name string `json:"name"`
			Link string `json:"link"`
		}
		_ = json.Unmarshal(body, &p)
		p.Name, p.Link = strings.TrimSpace(p.Name), strings.TrimSpace(p.Link)
		if !validPoolName(p.Name) {
			fail(w, "name must match [A-Za-z0-9_-]{1,64}")
			return
		}
		if p.Link == "" {
			fail(w, "link is required")
			return
		}
		s.cfg.RLock()
		scheme := s.cfg.ProxyPool.DefaultScheme
		_, exists := s.cfg.ProxyPool.Entries[p.Name]
		s.cfg.RUnlock()
		if exists {
			fail(w, "entry already exists: "+p.Name)
			return
		}
		if _, err := proxypool.ParseNode(p.Link, scheme); err != nil {
			fail(w, "node parse: "+err.Error())
			return
		}
		s.cfg.Lock()
		if s.cfg.ProxyPool.Entries == nil {
			s.cfg.ProxyPool.Entries = map[string]*config.ProxyEntry{}
		}
		s.cfg.ProxyPool.Entries[p.Name] = &config.ProxyEntry{Link: p.Link, Enabled: true}
		s.cfg.Unlock()
		s.poolReload()
		s.save(w)

	case "pool_toggle_entry", "pool_toggle_sub":
		var p struct {
			Name    string `json:"name"`
			Enabled bool   `json:"enabled"`
		}
		_ = json.Unmarshal(body, &p)
		s.cfg.Lock()
		ok := false
		if req.Action == "pool_toggle_entry" {
			if e := s.cfg.ProxyPool.Entries[p.Name]; e != nil {
				e.Enabled = p.Enabled
				ok = true
			}
		} else {
			if sc := s.cfg.ProxyPool.Subscriptions[p.Name]; sc != nil {
				sc.Enabled = p.Enabled
				ok = true
			}
		}
		s.cfg.Unlock()
		if !ok {
			fail(w, "not found: "+p.Name)
			return
		}
		s.poolReload()
		s.save(w)

	case "pool_delete_entry", "pool_delete_sub":
		var p struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(body, &p)
		ref := p.Name
		if req.Action == "pool_delete_sub" {
			ref = "sub:" + p.Name
		}
		s.cfg.Lock()
		if req.Action == "pool_delete_entry" {
			delete(s.cfg.ProxyPool.Entries, p.Name)
		} else {
			delete(s.cfg.ProxyPool.Subscriptions, p.Name)
		}
		for _, st := range s.cfg.Sites {
			if st != nil {
				st.Proxies = removeString(st.Proxies, ref)
			}
		}
		s.cfg.Unlock()
		s.poolReload()
		s.save(w)

	case "pool_add_sub":
		var p struct {
			Name            string `json:"name"`
			URL             string `json:"url"`
			IntervalMinutes int    `json:"interval_minutes"`
		}
		_ = json.Unmarshal(body, &p)
		p.Name, p.URL = strings.TrimSpace(p.Name), strings.TrimSpace(p.URL)
		if !validPoolName(p.Name) {
			fail(w, "name must match [A-Za-z0-9_-]{1,64}")
			return
		}
		if p.URL == "" {
			fail(w, "url is required")
			return
		}
		s.cfg.Lock()
		if s.cfg.ProxyPool.Subscriptions == nil {
			s.cfg.ProxyPool.Subscriptions = map[string]*config.ProxySubscription{}
		}
		if _, exists := s.cfg.ProxyPool.Subscriptions[p.Name]; exists {
			s.cfg.Unlock()
			fail(w, "subscription already exists: "+p.Name)
			return
		}
		if p.IntervalMinutes <= 0 {
			p.IntervalMinutes = 60
		}
		s.cfg.ProxyPool.Subscriptions[p.Name] = &config.ProxySubscription{
			URL: p.URL, Enabled: true,
			IntervalMinutes: clampInt(p.IntervalMinutes, 5, 1440),
		}
		s.cfg.Unlock()
		s.poolReload()
		s.save(w)

	case "pool_set_site":
		var p struct {
			Site    string   `json:"site"`
			Proxies []string `json:"proxies"`
		}
		_ = json.Unmarshal(body, &p)
		clean := make([]string, 0, len(p.Proxies))
		for _, item := range p.Proxies {
			if item = strings.TrimSpace(item); item != "" {
				clean = append(clean, item)
			}
		}
		s.cfg.Lock()
		st, ok := s.cfg.Sites[p.Site]
		if ok && st != nil {
			for _, item := range clean {
				if name, isSub := strings.CutPrefix(item, "sub:"); isSub {
					if s.cfg.ProxyPool.Subscriptions[name] == nil {
						s.cfg.Unlock()
						fail(w, "unknown subscription: "+name)
						return
					}
				} else if s.cfg.ProxyPool.Entries[item] == nil {
					s.cfg.Unlock()
					fail(w, "unknown entry: "+item)
					return
				}
			}
			st.Proxies = clean
		}
		s.cfg.Unlock()
		if !ok || st == nil {
			fail(w, "unknown site: "+p.Site)
			return
		}
		s.poolReload()
		s.save(w)

	case "pool_refresh":
		if s.pool == nil {
			fail(w, "proxy pool is not available")
			return
		}
		s.pool.RefreshNow(context.Background())
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})

	default:
		fail(w, "unknown action: "+req.Action)
	}
}

func (s *Server) save(w http.ResponseWriter) {
	if err := s.cfg.Save(); err != nil {
		fail(w, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) testProxy(url string) map[string]any {
	url = strings.TrimSpace(url)
	target := "https://deepseek.fr/"
	start := time.Now()
	cli, err := httpx.StdClient(url, 20*time.Second)
	if err != nil {
		return map[string]any{"ok": false, "error": err.Error()}
	}
	req, _ := http.NewRequest(http.MethodGet, target, nil)
	req.Header.Set("User-Agent", httpx.DefaultUserAgent)
	resp, err := cli.Do(req)
	if err != nil {
		return map[string]any{"ok": false, "error": err.Error(), "ms": time.Since(start).Milliseconds()}
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return map[string]any{"ok": resp.StatusCode < 400, "status": resp.StatusCode, "ms": time.Since(start).Milliseconds()}
}

func (s *Server) probeSite(code string) map[string]any {
	s.cfg.RLock()
	st, ok := s.cfg.Sites[code]
	proxy := s.cfg.Proxy.URL
	var site config.Site
	if ok {
		site = *st
	}
	s.cfg.RUnlock()
	if !ok {
		return map[string]any{"ok": false, "error": "site not found"}
	}

	start := time.Now()
	sess, err := httpx.NewSession(httpx.Options{ProxyURL: proxy, Timeout: 20 * time.Second})
	if err != nil {
		return map[string]any{"ok": false, "error": err.Error()}
	}
	defer sess.Close()
	resp, err := sess.Do(nil, httpx.Request{
		Method: "GET", URL: site.BaseURL + "/",
		Header: map[string]string{
			"User-Agent":      httpx.DefaultUserAgent,
			"Accept":          "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
			"Accept-Language": site.Language,
		},
		Timeout: 20 * time.Second,
	})
	if err != nil {
		return map[string]any{"ok": false, "error": err.Error(), "ms": time.Since(start).Milliseconds()}
	}
	hasContainer := bytes.Contains(resp.Body, []byte("aipkit_chat_container"))
	return map[string]any{
		"ok": resp.Status < 400, "status": resp.Status,
		"ms": time.Since(start).Milliseconds(), "chat_container": hasContainer,
		"bytes": len(resp.Body),
	}
}

// ── playground ───────────────────────────────────────────────────

func (s *Server) handlePlayground(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r, 4<<20)
	if err != nil {
		fail(w, err.Error())
		return
	}
	var req struct {
		Model    string               `json:"model"`
		Messages []openai.ChatMessage `json:"messages"`
		Stream   bool                 `json:"stream"`
		Tools    []openai.ToolDef     `json:"tools"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		fail(w, "invalid request: "+err.Error())
		return
	}
	if req.Model == "" || len(req.Messages) == 0 {
		fail(w, "model and messages are required")
		return
	}
	s.cfg.RLock()
	_, ok := s.cfg.Models[req.Model]
	s.cfg.RUnlock()
	if !ok {
		fail(w, "unknown model")
		return
	}
	prompt := openai.BuildPrompt(req.Messages, openai.PromptOptions{Tools: req.Tools})

	start := time.Now()
	info := &upstream.ServeInfo{}

	s.cfg.RLock()
	deadline := time.Duration(s.cfg.Upstream.StreamTimeout * float64(time.Second))
	s.cfg.RUnlock()
	if deadline <= 0 {
		deadline = 5 * time.Minute
	}
	ctx, cancel := context.WithTimeout(r.Context(), deadline)
	defer cancel()

	if !req.Stream {
		var sb strings.Builder
		err := s.up.Chat(ctx, req.Model, prompt, info, func(ev upstream.Event) error {
			if ev.Kind == upstream.KindDelta {
				sb.WriteString(ev.Value)
			}
			return nil
		})
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
			return
		}
		_, site, route := info.Get()
		writeJSON(w, http.StatusOK, map[string]any{
			"text": sb.String(), "ms": time.Since(start).Milliseconds(),
			"site": site, "route": route, "prompt_chars": len(prompt),
		})
		return
	}

	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	send := func(v any) {
		raw, _ := json.Marshal(v)
		fmt.Fprintf(w, "data: %s\n\n", raw)
		if flusher != nil {
			flusher.Flush()
		}
	}
	send(map[string]any{"status": "connecting"})
	err = s.up.Chat(ctx, req.Model, prompt, info, func(ev upstream.Event) error {
		if ev.Kind == upstream.KindDelta {
			send(map[string]any{"delta": ev.Value})
		}
		return nil
	})
	_, site, route := info.Get()
	if err != nil {
		send(map[string]any{"error": err.Error()})
	} else {
		send(map[string]any{"done": true, "ms": time.Since(start).Milliseconds(), "site": site, "route": route})
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

// ── helpers ──────────────────────────────────────────────────────

func readBody(r *http.Request, max int64) ([]byte, error) {
	defer r.Body.Close()
	return io.ReadAll(io.LimitReader(r.Body, max))
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, msg string) {
	writeJSON(w, http.StatusBadRequest, map[string]any{"error": msg})
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// siteCodes resolves a requested site code to a list; an empty request means
// every configured site.
// siteBalance reports the free daily tier of the visitor id currently
// cached for the site (same endpoint the site's own balance badge uses).
func (s *Server) siteBalance(code string) map[string]any {
	site, ok, proxy, botID := s.balanceContext(code)
	if !ok {
		return map[string]any{"ok": false, "error": "site not found"}
	}
	gid, has := s.ts.GuestID(code, proxy)
	if !has {
		return map[string]any{"ok": false, "error": "no cached session for this site — send a chat request first"}
	}
	return s.balanceJSON(code, site, gid, botID, proxy, false)
}

// rotateGuest swaps the site's visitor cookie for a fresh id — the site
// hands every new visitor id its own daily free tier, the same effect as a
// new private window — then reports the new identity's balance.
func (s *Server) rotateGuest(code string) map[string]any {
	site, ok, proxy, botID := s.balanceContext(code)
	if !ok {
		return map[string]any{"ok": false, "error": "site not found"}
	}
	gid := s.ts.RotateGuest(code)
	if gid == "" {
		return map[string]any{"ok": false, "error": "no cached session for this site — refresh cookies first"}
	}
	return s.balanceJSON(code, site, gid, botID, proxy, true)
}

func (s *Server) balanceContext(code string) (config.Site, bool, string, int) {
	s.cfg.RLock()
	defer s.cfg.RUnlock()
	st, ok := s.cfg.Sites[code]
	if !ok {
		return config.Site{}, false, "", 0
	}
	botID := 0
	for _, m := range s.cfg.Models {
		if m.Site == code && m.Enabled {
			botID = m.BotID
			break
		}
	}
	return *st, true, s.cfg.Proxy.URL, botID
}

func (s *Server) balanceJSON(code string, site config.Site, gid string, botID int, proxy string, rotated bool) map[string]any {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	bal, err := s.up.FetchBalance(ctx, site, gid, botID, upstream.Route{Name: "balance", Proxy: proxy})
	if err != nil {
		return map[string]any{"ok": false, "error": err.Error()}
	}
	out := map[string]any{
		"ok": true, "site": code, "rotated": rotated,
		"balance": bal.Balance, "limit": bal.Free.Limit,
		"used": bal.Free.Used, "remaining": bal.Free.Remaining,
		"reset_period": bal.Free.ResetPeriod,
	}
	if rotated {
		s.log.Info("visitor identity rotated from console", "site", code, "remaining", bal.Free.Remaining)
	}
	return out
}

func (s *Server) siteCodes(want string) []string {
	s.cfg.RLock()
	defer s.cfg.RUnlock()
	if want != "" {
		return []string{want}
	}
	out := make([]string, 0, len(s.cfg.Sites))
	for code := range s.cfg.Sites {
		out = append(out, code)
	}
	return out
}

func randomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func clampFloat(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

var poolNameRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func validPoolName(name string) bool { return poolNameRe.MatchString(name) }

// removeString drops every occurrence of want from list (in place copy).
func removeString(list []string, want string) []string {
	out := list[:0:0]
	for _, v := range list {
		if v != want {
			out = append(out, v)
		}
	}
	return out
}

// poolReload pushes pool-relevant config changes into the live manager.
func (s *Server) poolReload() {
	if s.pool != nil {
		s.pool.Reload()
	}
}

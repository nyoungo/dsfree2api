package upstream

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/nyoungo/dsfree2api/internal/config"
	"github.com/nyoungo/dsfree2api/internal/httpx"
	"github.com/nyoungo/dsfree2api/internal/turnstile"
)

type Route struct {
	Name  string
	Proxy string // "" means direct
}

func (r Route) Key() string {
	if r.Proxy == "" {
		return "direct"
	}
	return r.Proxy
}

type chatConfig struct {
	BotID     int
	PostID    int
	Nonce     string
	AJAXURL   string
	FetchedAt time.Time
}

type Client struct {
	cfg *config.Config
	ts  *turnstile.Solver
	log *slog.Logger

	stateMu  sync.Mutex
	chatCfg  map[string]chatConfig
	cfgLocks map[string]*sync.Mutex
	gates    map[string]chan struct{}

	// chatOnceOverride is a test seam; nil in production.
	chatOnceOverride func(ctx context.Context, site config.Site, modelID string, model config.Model,
		prompt string, route Route, info *ServeInfo, yield func(Event) error) error
}

func New(cfg *config.Config, ts *turnstile.Solver, log *slog.Logger) *Client {
	if log == nil {
		log = slog.Default()
	}
	return &Client{
		cfg:      cfg,
		ts:       ts,
		log:      log,
		chatCfg:  map[string]chatConfig{},
		cfgLocks: map[string]*sync.Mutex{},
		gates:    map[string]chan struct{}{},
	}
}

// Routes returns the primary proxy followed by the fallbacks, deduplicated.
func (c *Client) Routes() []Route {
	c.cfg.RLock()
	primary := strings.TrimSpace(c.cfg.Proxy.URL)
	fallbacks := append([]string(nil), c.cfg.Proxy.FallbackURLs...)
	c.cfg.RUnlock()

	type cand struct {
		name  string
		proxy string
	}
	list := []cand{{"primary", primary}}
	for i, u := range fallbacks {
		list = append(list, cand{fmt.Sprintf("fallback-%d", i+1), strings.TrimSpace(u)})
	}
	seen := map[string]bool{}
	out := make([]Route, 0, len(list))
	for _, it := range list {
		key := it.proxy
		if key == "" {
			key = "direct"
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, Route{Name: it.name, Proxy: it.proxy})
	}
	return out
}

// Chat streams the prompt through the upstream site, honouring route
// failover, slow-start detection, session refresh and cross-site failover.
func (c *Client) Chat(ctx context.Context, modelID string, prompt string, info *ServeInfo, yield func(Event) error) error {
	model, ok := c.modelCopy(modelID)
	if !ok {
		return errf("unknown model %s", modelID)
	}
	if !model.Enabled {
		return errf("model %s is disabled", modelID)
	}

	candidates := []string{modelID}
	if c.cfg.Upstream.CrossSiteFailover {
		for _, alt := range c.alternates(modelID, model) {
			candidates = append(candidates, alt)
		}
	}

	var last error
	for i, id := range candidates {
		m := model
		if i > 0 {
			mc, ok := c.modelCopy(id)
			if !ok || !mc.Enabled {
				continue
			}
			m = mc
			c.log.Warn("cross-site failover", "from", modelID, "to", id)
		}
		err := c.chatWithRetries(ctx, id, m, prompt, info, yield)
		if err == nil {
			return nil
		}
		last = err
		if contentStarted(err) || !isQuota(err) {
			return AsUpstream(err)
		}
	}
	return AsUpstream(last)
}

// alternates lists other enabled sites hosting the same upstream model.
func (c *Client) alternates(modelID string, m config.Model) []string {
	c.cfg.RLock()
	defer c.cfg.RUnlock()
	var out []string
	for id, other := range c.cfg.Models {
		if id == modelID || other.Site == m.Site {
			continue
		}
		if other.UpstreamID != m.UpstreamID || !other.Enabled {
			continue
		}
		if s, ok := c.cfg.Sites[other.Site]; !ok || !s.Enabled {
			continue
		}
		out = append(out, id)
	}
	return out
}

func (c *Client) modelCopy(id string) (config.Model, bool) {
	c.cfg.RLock()
	defer c.cfg.RUnlock()
	m, ok := c.cfg.Models[id]
	if !ok {
		return config.Model{}, false
	}
	return *m, true
}

func (c *Client) siteCopy(code string) (config.Site, bool) {
	c.cfg.RLock()
	defer c.cfg.RUnlock()
	s, ok := c.cfg.Sites[code]
	if !ok {
		return config.Site{}, false
	}
	return *s, true
}

func (c *Client) userAgent() string {
	c.cfg.RLock()
	defer c.cfg.RUnlock()
	return c.cfg.Upstream.UserAgent
}

func (c *Client) chatWithRetries(ctx context.Context, modelID string, model config.Model, prompt string, info *ServeInfo, yield func(Event) error) error {
	site, ok := c.siteCopy(model.Site)
	if !ok {
		return errf("unknown site %s", model.Site)
	}
	if !site.Enabled {
		return errf("site %s is disabled", model.Site)
	}
	routes := c.Routes()

	var lastErr error
	for attempt := 0; ; attempt++ {
		var failed []Route
		for i, route := range routes {
			allowSlow := i == 0 && len(routes) > 1
			err := c.chatOnceSlowStart(ctx, site, modelID, model, prompt, route, allowSlow, info, yield)
			if err == nil {
				return nil
			}
			lastErr = err
			if contentStarted(err) {
				return AsUpstream(err)
			}
			// A solver-side rejection (bad key, rejected challenge) will fail
			// identically on every attempt — stop instead of burning retries.
			if !turnstile.Retryable(err) {
				return AsUpstream(err)
			}
			// The site demands Turnstile verification while the solver is
			// switched off: refreshing cookies cannot help, say why instead.
			if isSession(err) && (c.ts == nil || !c.ts.Enabled()) {
				return &UpstreamError{
					Msg: "site requires Turnstile verification — import cookies on the console " +
						"Turnstile page, or enable [turnstile] to solve automatically",
					Cause: err,
				}
			}
			if isConfigProblem(err) {
				return AsUpstream(err)
			}
			failed = append(failed, route)
			c.log.Warn("upstream error",
				"model", modelID, "attempt", attempt+1, "route", route.Name, "error", err)
			if i < len(routes)-1 {
				continue
			}
			break
		}

		c.cfg.RLock()
		autoRefresh := c.cfg.Upstream.AutoRefresh
		backoff := c.cfg.Upstream.RetryBackoffSeconds
		retries := c.cfg.Upstream.RefreshRetries
		c.cfg.RUnlock()
		if !autoRefresh || attempt >= retries {
			break
		}
		if len(failed) > 0 {
			wait := time.Duration(float64(attempt+1) * backoff * float64(time.Second))
			select {
			case <-ctx.Done():
				return AsUpstream(ctx.Err())
			case <-time.After(wait):
			}
			forceCookie := isQuota(lastErr) || isSession(lastErr)
			for _, route := range failed {
				if err := c.refreshSession(ctx, site, modelID, route, forceCookie); err != nil {
					c.log.Warn("session refresh failed", "model", modelID, "error", err)
				}
			}
		}
	}
	return AsUpstream(lastErr)
}

// chatOnceSlowStart bounds the time the primary route may take to produce its
// first upstream event before a fallback route is tried instead. Errors that
// happen after the first event are marked ContentStarted so callers never
// restart a response the client already partially received.
func (c *Client) chatOnceSlowStart(
	ctx context.Context,
	site config.Site,
	modelID string,
	model config.Model,
	prompt string,
	route Route,
	allowSlow bool,
	info *ServeInfo,
	yield func(Event) error,
) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	events := make(chan Event, 16)
	errCh := make(chan error, 1)
	gotFirst := false

	go func() {
		fn := c.chatOnce
		if c.chatOnceOverride != nil {
			fn = c.chatOnceOverride
		}
		err := fn(ctx, site, modelID, model, prompt, route, info, func(ev Event) error {
			select {
			case events <- ev:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
		close(events)
		errCh <- err
	}()

	mark := func(err error) error {
		if err == nil || !gotFirst {
			return err
		}
		ue := AsUpstream(err)
		ue.ContentStarted = true
		return ue
	}
	take := func(ev Event) error {
		gotFirst = true
		if err := yield(ev); err != nil {
			return mark(err)
		}
		return nil
	}

	if allowSlow {
		var slow float64
		c.cfg.RLock()
		slow = c.cfg.Proxy.SlowStartSeconds
		c.cfg.RUnlock()
		if slow > 0 {
			timer := time.NewTimer(time.Duration(slow * float64(time.Second)))
			defer timer.Stop()
			select {
			case ev, ok := <-events:
				if !ok {
					return mark(<-errCh)
				}
				if err := take(ev); err != nil {
					return err
				}
			case err := <-errCh:
				for ev := range events {
					if e := take(ev); e != nil {
						return e
					}
				}
				return mark(err)
			case <-timer.C:
				cancel()
				<-errCh
				return errf("primary route produced no upstream event within %gs", slow)
			}
		}
	}

	for {
		ev, ok := <-events
		if !ok {
			return mark(<-errCh)
		}
		if err := take(ev); err != nil {
			return err
		}
	}
}

func (c *Client) chatOnce(
	ctx context.Context,
	site config.Site,
	modelID string,
	model config.Model,
	prompt string,
	route Route,
	info *ServeInfo,
	yield func(Event) error,
) error {
	if err := c.acquire(ctx, site.Code); err != nil {
		return AsUpstream(err)
	}
	defer c.release(site.Code)

	if info != nil {
		info.Set(modelID, site.Code, route.Name)
	}

	c.cfg.RLock()
	streamTimeout := time.Duration(c.cfg.Upstream.StreamTimeout * float64(time.Second))
	timeout := time.Duration(c.cfg.Upstream.Timeout * float64(time.Second))
	ua := c.cfg.Upstream.UserAgent
	c.cfg.RUnlock()

	sess, err := httpx.NewSession(httpx.Options{ProxyURL: route.Proxy, Timeout: streamTimeout})
	if err != nil {
		return errf("create session: %v", err)
	}
	defer sess.Close()

	if err := c.ts.ApplyValidCookies(ctx, sess, &site, route.Proxy); err != nil {
		return AsUpstream(err)
	}

	cc, err := c.chatConfig(ctx, sess, site, modelID, model, route, ua)
	if err != nil {
		return err
	}

	referer := site.BaseURL + model.PagePath
	sessionID := newUUID()
	convUUID := newUUID()
	clientMsgID := fmt.Sprintf("aipkit-client-msg-%d-%d-%s", cc.BotID, time.Now().UnixMilli(), randHex(3))

	resp, err := sess.Do(ctx, httpx.Request{
		Method: "POST",
		URL:    cc.AJAXURL,
		Header: ajaxHeaders(site, referer, ua),
		Body: []byte(encodeForm(map[string]string{
			"action":                 "aipkit_cache_sse_message",
			"message":                prompt,
			"_ajax_nonce":            cc.Nonce,
			"bot_id":                 itoa(cc.BotID),
			"session_id":             sessionID,
			"conversation_uuid":      convUUID,
			"user_client_message_id": clientMsgID,
		})),
		Timeout: timeout,
	})
	if err != nil {
		return errf("cache message request failed: %v", err)
	}
	if resp.Status >= 400 {
		if resp.Status == 403 || resp.Status == 401 {
			return newSession(fmt.Sprintf("cache message http %d", resp.Status))
		}
		return errf("cache message http %d: %s", resp.Status, truncate(string(resp.Body), 400))
	}

	var init struct {
		Success bool            `json:"success"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(resp.Body, &init); err != nil {
		return errf("cache message bad json: %v", err)
	}
	if !init.Success {
		if code := extractCode(init.Data); code != "" && strings.Contains(strings.ToLower(code), "nonce") {
			c.dropChatCfg(modelID, route)
		}
		payload := map[string]any{}
		_ = json.Unmarshal(resp.Body, &payload)
		if isSessionPayload(payload) {
			return newSession("cache message session failed: " + truncate(string(resp.Body), 400))
		}
		return errf("cache message failed: %s", truncate(string(resp.Body), 400))
	}
	var dataObj struct {
		CacheKey string `json:"cache_key"`
	}
	if err := json.Unmarshal(init.Data, &dataObj); err != nil || dataObj.CacheKey == "" {
		return errf("cache message missing cache_key: %s", truncate(string(resp.Body), 400))
	}

	params := encodeForm(map[string]string{
		"action":            "aipkit_frontend_chat_stream",
		"cache_key":         dataObj.CacheKey,
		"bot_id":            itoa(cc.BotID),
		"session_id":        sessionID,
		"conversation_uuid": convUUID,
		"post_id":           itoa(cc.PostID),
		"_ts":               itoa64(time.Now().UnixMilli()),
		"_ajax_nonce":       cc.Nonce,
	})

	streamCtx, streamCancel := context.WithTimeout(ctx, streamTimeout)
	defer streamCancel()

	headers := ajaxHeaders(site, referer, ua)
	headers["Accept"] = "text/event-stream"

	rc, _, err := sess.Stream(streamCtx, httpx.Request{
		Method: "GET",
		URL:    cc.AJAXURL + "?" + params,
		Header: headers,
	})
	if err != nil {
		if streamCtx.Err() != nil && ctx.Err() == nil {
			return errf("upstream stream timeout: %v", err)
		}
		return AsUpstream(err)
	}
	defer rc.Close()

	return readSSE(rc, func(event string, lines []string) error {
		events, err := translateEvent(event, lines)
		if err != nil {
			return err
		}
		for _, ev := range events {
			if err := yield(ev); err != nil {
				return err
			}
		}
		return nil
	})
}

// chatConfig fetches (and caches) the page-embedded nonce for a model.
func (c *Client) chatConfig(
	ctx context.Context,
	sess httpx.Session,
	site config.Site,
	modelID string,
	model config.Model,
	route Route,
	ua string,
) (chatConfig, error) {
	key := modelID + "|" + route.Key()
	if v, ok := c.loadChatCfg(key); ok {
		return v, nil
	}
	lock := c.lockFor(key)
	lock.Lock()
	defer lock.Unlock()
	if v, ok := c.loadChatCfg(key); ok {
		return v, nil
	}

	pageURL := site.BaseURL + model.PagePath
	resp, err := sess.Do(ctx, httpx.Request{
		Method:  "GET",
		URL:     pageURL,
		Header:  browserHeaders(site, ua),
		Timeout: 60 * time.Second,
	})
	if err != nil {
		return chatConfig{}, errf("chat config fetch failed: %v", err)
	}
	if resp.Status >= 400 {
		return chatConfig{}, errf("chat config http %d", resp.Status)
	}

	parsed, found := parseChatConfig(string(resp.Body), site, model)
	if !found {
		if model.BotID <= 0 || model.PostID <= 0 {
			return chatConfig{}, errf("no chat data-config found for %s", modelID)
		}
		nonce, err := c.fetchNonce(ctx, sess, site, model, ua)
		if err != nil {
			return chatConfig{}, err
		}
		parsed = chatConfig{
			BotID: model.BotID, PostID: model.PostID, Nonce: nonce,
			AJAXURL: site.AJAXURL, FetchedAt: time.Now(),
		}
	}
	parsed.FetchedAt = time.Now()
	c.storeChatCfg(key, parsed)
	c.log.Info("loaded chat config", "model", modelID, "route", route.Name,
		"bot_id", parsed.BotID, "post_id", parsed.PostID)
	return parsed, nil
}

var containerRe = regexp.MustCompile(`id="aipkit_chat_container_(\d+)"[^>]*data-config='([^']+)'`)

func parseChatConfig(page string, site config.Site, model config.Model) (chatConfig, bool) {
	for _, m := range containerRe.FindAllStringSubmatch(page, -1) {
		botID := atoi(m[1])
		raw := html.UnescapeString(m[2])
		var cfg struct {
			BotID   any    `json:"botId"`
			PostID  any    `json:"postId"`
			Nonce   string `json:"nonce"`
			AjaxURL string `json:"ajaxUrl"`
		}
		if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
			continue
		}
		if model.BotID > 0 && botID != model.BotID {
			continue
		}
		out := chatConfig{
			BotID:     toInt(cfg.BotID, botID),
			PostID:    toInt(cfg.PostID, model.PostID),
			Nonce:     cfg.Nonce,
			AJAXURL:   cfg.AjaxURL,
			FetchedAt: time.Now(),
		}
		if out.AJAXURL == "" {
			out.AJAXURL = site.AJAXURL
		}
		if out.Nonce != "" {
			return out, true
		}
	}
	return chatConfig{}, false
}

func (c *Client) fetchNonce(ctx context.Context, sess httpx.Session, site config.Site, model config.Model, ua string) (string, error) {
	resp, err := sess.Do(ctx, httpx.Request{
		Method:  "POST",
		URL:     site.AJAXURL,
		Header:  ajaxHeaders(site, site.BaseURL+model.PagePath, ua),
		Body:    []byte(encodeForm(map[string]string{"action": "aipkit_get_frontend_chat_nonce", "bot_id": itoa(model.BotID)})),
		Timeout: 30 * time.Second,
	})
	if err != nil {
		return "", errf("nonce fetch failed: %v", err)
	}
	var out struct {
		Success bool `json:"success"`
		Data    struct {
			Nonce string `json:"nonce"`
		} `json:"data"`
	}
	if err := json.Unmarshal(resp.Body, &out); err != nil || !out.Success || out.Data.Nonce == "" {
		return "", errf("nonce fetch failed for %s: %s", model.ID, truncate(string(resp.Body), 400))
	}
	return out.Data.Nonce, nil
}

// refreshSession drops cached state for a route and optionally re-solves Turnstile.
func (c *Client) refreshSession(
	ctx context.Context,
	site config.Site,
	modelID string,
	route Route,
	invalidateCookies bool,
) error {
	c.dropChatCfg(modelID, route)
	if !invalidateCookies {
		return nil
	}
	c.log.Info("invalidate verified cookie cache", "site", site.Code, "route", route.Name)
	c.ts.Invalidate(site.Code)

	sess, err := httpx.NewSession(httpx.Options{ProxyURL: route.Proxy, Timeout: 60 * time.Second})
	if err != nil {
		return err
	}
	defer sess.Close()
	return c.ts.Refresh(ctx, sess, &site, route.Proxy)
}

// InvalidateAll clears cached nonces and cookies (web console action).
func (c *Client) InvalidateAll() {
	c.stateMu.Lock()
	c.chatCfg = map[string]chatConfig{}
	c.stateMu.Unlock()
	c.cfg.RLock()
	codes := make([]string, 0, len(c.cfg.Sites))
	for code := range c.cfg.Sites {
		codes = append(codes, code)
	}
	c.cfg.RUnlock()
	for _, code := range codes {
		c.ts.Invalidate(code)
	}
}

func (c *Client) ConfigCacheSize() int {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	return len(c.chatCfg)
}

// ── cache helpers ────────────────────────────────────────────────

func (c *Client) loadChatCfg(key string) (chatConfig, bool) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	v, ok := c.chatCfg[key]
	if !ok {
		return chatConfig{}, false
	}
	c.cfg.RLock()
	ttl := time.Duration(c.cfg.Upstream.ConfigTTLSeconds) * time.Second
	c.cfg.RUnlock()
	if ttl <= 0 || time.Since(v.FetchedAt) > ttl {
		return chatConfig{}, false
	}
	return v, true
}

func (c *Client) storeChatCfg(key string, v chatConfig) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	c.chatCfg[key] = v
}

func (c *Client) dropChatCfg(modelID string, route Route) {
	suffix := "|" + route.Key()
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	for k := range c.chatCfg {
		if strings.HasSuffix(k, suffix) && strings.TrimSuffix(k, suffix) == modelID {
			delete(c.chatCfg, k)
		}
	}
}

func (c *Client) lockFor(key string) *sync.Mutex {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	l, ok := c.cfgLocks[key]
	if !ok {
		l = &sync.Mutex{}
		c.cfgLocks[key] = l
	}
	return l
}

// ── concurrency gate ────────────────────────────────────────────

func (c *Client) acquire(ctx context.Context, site string) error {
	c.cfg.RLock()
	max := c.cfg.Limits.MaxConcurrentPerSite
	c.cfg.RUnlock()
	if max <= 0 {
		return nil
	}
	c.stateMu.Lock()
	g, ok := c.gates[site]
	if !ok {
		g = make(chan struct{}, max)
		c.gates[site] = g
	}
	c.stateMu.Unlock()
	select {
	case g <- struct{}{}:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("site %s busy: %w", site, ctx.Err())
	}
}

func (c *Client) release(site string) {
	c.stateMu.Lock()
	g, ok := c.gates[site]
	c.stateMu.Unlock()
	if !ok {
		return
	}
	select {
	case <-g:
	default:
	}
}

// ── helpers ─────────────────────────────────────────────────────

func browserHeaders(site config.Site, ua string) map[string]string {
	if ua == "" {
		ua = httpx.DefaultUserAgent
	}
	return map[string]string{
		"User-Agent":      ua,
		"Accept":          "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8",
		"Accept-Language": site.Language,
		"Origin":          site.BaseURL,
		"Referer":         site.BaseURL + "/",
	}
}

func ajaxHeaders(site config.Site, referer, ua string) map[string]string {
	if ua == "" {
		ua = httpx.DefaultUserAgent
	}
	return map[string]string{
		"User-Agent":       ua,
		"Accept":           "application/json, text/javascript, */*; q=0.01",
		"Accept-Language":  site.Language,
		"X-Requested-With": "XMLHttpRequest",
		"Origin":           site.BaseURL,
		"Referer":          referer,
	}
}

func encodeForm(v map[string]string) string {
	keys := make([]string, 0, len(v))
	for k := range v {
		keys = append(keys, k)
	}
	sortStrings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(urlEncode(k))
		b.WriteByte('=')
		b.WriteString(urlEncode(v[k]))
	}
	return b.String()
}

func urlEncode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z', ch >= '0' && ch <= '9',
			ch == '-', ch == '_', ch == '.', ch == '~':
			b.WriteByte(ch)
		case ch == ' ':
			b.WriteByte('+')
		default:
			fmt.Fprintf(&b, "%%%02X", ch)
		}
	}
	return b.String()
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func newUUID() string { return randHex(16) }

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return strings.Repeat("0", n*2)
	}
	return hex.EncodeToString(b)
}

func itoa(v int) string     { return fmt.Sprintf("%d", v) }
func itoa64(v int64) string { return fmt.Sprintf("%d", v) }

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func atoi(s string) int {
	n := 0
	for _, ch := range s {
		if ch < '0' || ch > '9' {
			return n
		}
		n = n*10 + int(ch-'0')
	}
	return n
}

func extractCode(data json.RawMessage) string {
	var obj struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(data, &obj); err == nil {
		return obj.Code
	}
	return ""
}

func toInt(v any, def int) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case int:
		return t
	case string:
		return atoi(t)
	case json.Number:
		if n, err := t.Int64(); err == nil {
			return int(n)
		}
	}
	return def
}

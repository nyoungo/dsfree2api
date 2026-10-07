// Package turnstile solves Cloudflare Turnstile challenges for the upstream
// sites and caches the resulting verified cookies per proxy route + site.
package turnstile

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/nyoungo/dsfree2api/internal/config"
	"github.com/nyoungo/dsfree2api/internal/httpx"
)

// Error is a Turnstile solver failure. Retryable marks transient (network)
// failures; solver-side rejections such as a bad API key or exhausted quota
// return Retryable=false so callers fail fast instead of burning retries.
type Error struct {
	Msg       string
	Retryable bool
}

func (e *Error) Error() string { return e.Msg }

// Retryable reports whether err is worth retrying. Non-solver errors are
// assumed transient.
func Retryable(err error) bool {
	var e *Error
	if errors.As(err, &e) {
		return e.Retryable
	}
	return true
}

type Record struct {
	Site      string    `json:"site"`
	Proxy     string    `json:"proxy"`
	At        time.Time `json:"at"`
	ElapsedMs int64     `json:"elapsed_ms"`
	OK        bool      `json:"ok"`
	Error     string    `json:"error,omitempty"`
}

type SiteStatus struct {
	Site       string `json:"site"`
	Valid      bool   `json:"valid"`
	ExpiresAt  int64  `json:"expires_at"`
	RemainingS int64  `json:"remaining_s"`
	CookieNum  int    `json:"cookie_count"`
}

type cookieState struct {
	cookies   map[string]string
	expiresAt time.Time
}

type core struct {
	proxy  string
	client *http.Client
	mu     sync.Mutex
	cache  map[string]*cookieState
	locks  map[string]*sync.Mutex
}

func (c *core) lockFor(site string) *sync.Mutex {
	c.mu.Lock()
	defer c.mu.Unlock()
	l, ok := c.locks[site]
	if !ok {
		l = &sync.Mutex{}
		c.locks[site] = l
	}
	return l
}

type Solver struct {
	cfg     config.Turnstile
	ua      string
	mu      sync.Mutex
	cores   map[string]*core
	history []Record
	now     func() time.Time

	// browserMu serializes browser solves: one Chrome window at a time.
	browserMu sync.Mutex

	// refreshOverride is a test seam; nil in production.
	refreshOverride func(ctx context.Context, sess httpx.Session, site *config.Site, c *core) error
}

func New(cfg config.Turnstile, userAgent string) *Solver {
	if userAgent == "" {
		userAgent = httpx.DefaultUserAgent
	}
	return &Solver{
		cfg:   cfg,
		ua:    userAgent,
		cores: map[string]*core{},
		now:   time.Now,
	}
}

func (s *Solver) Enabled() bool { return s.cfg.Enabled }

func (s *Solver) Config() config.Turnstile {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg
}

// SetConfig swaps the solver configuration (used by the web console). Cached
// cookies survive it, otherwise saving an unrelated field would throw away a
// cookie import.
func (s *Solver) SetConfig(cfg config.Turnstile) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg = cfg
}

func (s *Solver) coreFor(proxy string) *core {
	key := proxy
	if key == "" {
		key = "direct"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.cores[key]
	if !ok {
		client, _ := httpx.StdClient(proxy, time.Duration(s.cfg.TimeoutSeconds+30)*time.Second)
		c = &core{
			proxy:  key,
			client: client,
			cache:  map[string]*cookieState{},
			locks:  map[string]*sync.Mutex{},
		}
		s.cores[key] = c
	}
	return c
}

func (s *Solver) Invalidate(siteCode string) {
	s.mu.Lock()
	cores := make([]*core, 0, len(s.cores))
	for _, c := range s.cores {
		cores = append(cores, c)
	}
	s.mu.Unlock()
	for _, c := range cores {
		c.mu.Lock()
		delete(c.cache, siteCode)
		c.mu.Unlock()
	}
}

// guestCookieName is the cookie the upstream guest-token plugin keys a
// visitor's free daily tier to (verified against the live endpoints: the
// balance API echoes it as "gid" and accepts any freshly minted value).
const guestCookieName = "dsgt_gid"

// guestIDAlphabet matches the 32-char ids the site itself issues.
const guestIDAlphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// randomGuestID mints a visitor id in the same shape the site uses.
func randomGuestID() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("g%d", time.Now().UnixNano())
	}
	for i := range b {
		b[i] = guestIDAlphabet[int(b[i])%len(guestIDAlphabet)]
	}
	return string(b)
}

// RotateGuest swaps the visitor cookie inside every cached session for the
// site. The upstream grants each fresh visitor id its own daily free tier —
// the same effect as opening a new private window — so a quota-exhausted
// site recovers without paying for a Turnstile re-solve. Returns the new id,
// or "" when the site has no live cached session to rotate.
func (s *Solver) RotateGuest(siteCode string) string {
	gid := randomGuestID()
	s.mu.Lock()
	cores := make([]*core, 0, len(s.cores))
	for _, c := range s.cores {
		cores = append(cores, c)
	}
	s.mu.Unlock()
	rotated := false
	for _, c := range cores {
		c.mu.Lock()
		if st, ok := c.cache[siteCode]; ok && time.Now().Before(st.expiresAt) {
			st.cookies[guestCookieName] = gid
			rotated = true
		}
		c.mu.Unlock()
	}
	if !rotated {
		return ""
	}
	return gid
}

// GuestID reports the visitor id cached for the site on one proxy route.
func (s *Solver) GuestID(siteCode, proxy string) (string, bool) {
	ck := s.coreFor(proxy).load(siteCode)
	if ck == nil {
		return "", false
	}
	gid := ck[guestCookieName]
	return gid, gid != ""
}

// ImportCookies stores cookies exported from a browser session that already
// passed the Turnstile check, so chat works while [turnstile] is off or the
// solver service is down. Cookies are bound to the IP/UA that obtained them,
// so they must be copied from a browser using the same proxy route.
func (s *Solver) ImportCookies(siteCode, raw, proxy string) (time.Time, int, error) {
	pairs := ParseCookiePairs(raw)
	if len(pairs) == 0 {
		return time.Time{}, 0, errors.New("no cookies parsed — expected `name=value; name2=value2`")
	}
	exp := time.Now().Add(time.Duration(s.Config().CookieTTLSeconds) * time.Second)
	c := s.coreFor(proxy)
	c.mu.Lock()
	c.cache[siteCode] = &cookieState{cookies: pairs, expiresAt: exp}
	c.mu.Unlock()
	return exp, len(pairs), nil
}

// cookieAttrs are Set-Cookie attributes and never part of the session.
var cookieAttrs = map[string]bool{
	"path": true, "domain": true, "expires": true, "max-age": true,
	"samesite": true, "httponly": true, "secure": true, "priority": true,
	"partitioned": true,
}

// ParseCookiePairs accepts `a=b; c=d`, `document.cookie` output or one pair
// per line. Values may themselves contain '='.
func ParseCookiePairs(raw string) map[string]string {
	out := map[string]string{}
	for _, chunk := range strings.Split(strings.ReplaceAll(raw, "\n", ";"), ";") {
		chunk = strings.TrimSpace(chunk)
		if chunk == "" {
			continue
		}
		i := strings.Index(chunk, "=")
		if i <= 0 {
			continue
		}
		k := strings.ToLower(strings.Trim(strings.TrimSpace(chunk[:i]), `"`))
		v := strings.Trim(strings.TrimSpace(chunk[i+1:]), `"`)
		if cookieAttrs[k] || v == "" {
			continue
		}
		out[k] = v
	}
	return out
}

// ApplyValidCookies ensures the session carries fresh verified cookies.
// Concurrent callers for the same site coalesce behind a per-site lock.
// Cached cookies (including a manual import) are applied even when the
// [turnstile] switch is off; only the solving step needs the switch.
func (s *Solver) ApplyValidCookies(ctx context.Context, sess httpx.Session, site *config.Site, proxy string) error {
	c := s.coreFor(proxy)
	if ck := c.load(site.Code); ck != nil {
		sess.SetCookies(ck)
		return nil
	}
	if !s.Enabled() {
		return nil
	}
	l := c.lockFor(site.Code)
	l.Lock()
	defer l.Unlock()
	if ck := c.load(site.Code); ck != nil {
		sess.SetCookies(ck)
		return nil
	}
	return s.refreshLocked(ctx, sess, site, c)
}

// Refresh forces a fresh solve → verify cycle.
func (s *Solver) Refresh(ctx context.Context, sess httpx.Session, site *config.Site, proxy string) error {
	if !s.cfg.Enabled {
		return nil
	}
	c := s.coreFor(proxy)
	l := c.lockFor(site.Code)
	l.Lock()
	defer l.Unlock()
	return s.refreshLocked(ctx, sess, site, c)
}

// load returns a copy of the cached cookies, or nil when absent or expired.
// Callers get a snapshot, so RotateGuest can swap entries concurrently.
func (c *core) load(site string) map[string]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	st, ok := c.cache[site]
	if !ok || time.Now().After(st.expiresAt) {
		return nil
	}
	out := make(map[string]string, len(st.cookies))
	for k, v := range st.cookies {
		out[k] = v
	}
	return out
}

func (s *Solver) refreshLocked(ctx context.Context, sess httpx.Session, site *config.Site, c *core) error {
	if s.refreshOverride != nil {
		return s.refreshOverride(ctx, sess, site, c)
	}
	start := time.Now()
	cfg := s.Config()
	siteKey := site.SiteKey
	if siteKey == "" {
		siteKey = cfg.SiteKey
	}
	token, err := s.solveToken(ctx, site, siteKey, c)
	if err != nil {
		s.record(site.Code, c.proxy, start, err)
		return err
	}
	// Warm the page the way a browser would before posting the verdict.
	if _, err := sess.Do(ctx, httpx.Request{
		Method: "GET",
		URL:    site.BaseURL + "/",
		Header: map[string]string{
			"User-Agent":      s.ua,
			"Accept":          "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8",
			"Accept-Language": site.Language,
			"Referer":         site.BaseURL + "/",
		},
		Timeout: 30 * time.Second,
	}); err != nil {
		s.record(site.Code, c.proxy, start, err)
		return &Error{Msg: "turnstile warm-up failed: " + err.Error(), Retryable: true}
	}
	if err := s.verifyToken(ctx, sess, site, token); err != nil {
		s.record(site.Code, c.proxy, start, err)
		return err
	}
	c.mu.Lock()
	c.cache[site.Code] = &cookieState{
		cookies:   sess.Cookies(),
		expiresAt: time.Now().Add(time.Duration(cfg.CookieTTLSeconds) * time.Second),
	}
	c.mu.Unlock()
	s.record(site.Code, c.proxy, start, nil)
	return nil
}

// solveToken obtains a Turnstile token through the configured provider.
// 1: api — POST the site to a hosted solver service.
// 2: browser — drive the local Chrome/Edge from [turnstile].browser_path.
// 3: manual — never solves; cookies only arrive via the console import box.
func (s *Solver) solveToken(ctx context.Context, site *config.Site, sitekey string, c *core) (string, error) {
	cfg := s.Config()
	switch cfg.ProviderValue() {
	case config.ProviderAPI:
		return s.solveTokenAPI(ctx, site.BaseURL, sitekey, c)
	case config.ProviderBrowser:
		return s.solveTokenBrowser(ctx, site, sitekey, c)
	case config.ProviderManual:
		return "", &Error{
			Msg:       `turnstile provider is "manual" — import cookies in the web console (Turnstile page)`,
			Retryable: false,
		}
	default:
		return "", &Error{
			Msg:       fmt.Sprintf("turnstile provider %q is invalid (api|browser|manual)", cfg.Provider),
			Retryable: false,
		}
	}
}

func (s *Solver) solveTokenAPI(ctx context.Context, baseURL, sitekey string, c *core) (string, error) {
	cfg := s.Config()
	payload, _ := json.Marshal(map[string]any{
		"url":            baseURL,
		"sitekey":        sitekey,
		"action":         cfg.Action,
		"cdata":          "",
		"timeoutSeconds": cfg.TimeoutSeconds,
	})
	var last error
	for attempt := 0; attempt < cfg.Retries; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.APIURL, bytes.NewReader(payload))
		if err != nil {
			return "", err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
		resp, err := c.client.Do(req)
		if err != nil {
			last = &Error{Msg: "turnstile solver request failed: " + err.Error(), Retryable: true}
		} else {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode >= 400 {
				last = &Error{
					Msg: fmt.Sprintf("turnstile solver http %d: %s (check [turnstile].api_key / api_url)",
						resp.StatusCode, trim(body)),
					Retryable: retryableStatus(resp.StatusCode),
				}
			} else {
				var out struct {
					ErrorID  int    `json:"errorId"`
					Status   string `json:"status"`
					Solution struct {
						Token string `json:"token"`
					} `json:"solution"`
					Elapsed float64 `json:"elapsedTime"`
				}
				if err := json.Unmarshal(body, &out); err != nil {
					last = &Error{Msg: "turnstile solver bad json: " + err.Error()}
				} else if out.ErrorID != 0 || (out.Status != "" && out.Status != "ready") {
					last = &Error{Msg: fmt.Sprintf("turnstile solver error: %s", trim(body))}
				} else if out.Solution.Token == "" {
					last = &Error{Msg: "turnstile solver returned no token"}
				} else {
					return out.Solution.Token, nil
				}
			}
		}
		if !Retryable(last) {
			return "", last
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(time.Duration(cfg.RetryBackoffSeconds*float64(attempt+1)) * time.Second):
		}
	}
	return "", last
}

// retryableStatus decides which solver HTTP statuses are worth another attempt.
// Solver-side rejections (bad key, exhausted quota, challenge rejected) return
// the same answer every time, so they are not retried.
func retryableStatus(code int) bool {
	switch code {
	case http.StatusRequestTimeout, http.StatusTooManyRequests,
		http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

func (s *Solver) verifyToken(ctx context.Context, sess httpx.Session, site *config.Site, token string) error {
	cfg := s.Config()
	body := fmt.Sprintf("action=%s&token=%s", urlQueryEscape(site.VerifyAction), urlQueryEscape(token))
	headers := map[string]string{
		"User-Agent":       s.ua,
		"Accept":           "application/json, text/javascript, */*; q=0.01",
		"Accept-Language":  site.Language,
		"X-Requested-With": "XMLHttpRequest",
		"Origin":           site.BaseURL,
		"Referer":          site.BaseURL + "/",
		"Content-Type":     "application/x-www-form-urlencoded",
	}
	var last error
	for attempt := 0; attempt < cfg.Retries; attempt++ {
		resp, err := sess.Do(ctx, httpx.Request{
			Method:  "POST",
			URL:     site.AJAXURL,
			Header:  headers,
			Body:    []byte(body),
			Timeout: 30 * time.Second,
		})
		if err == nil {
			var data map[string]any
			if jerr := json.Unmarshal(resp.Body, &data); jerr != nil {
				last = &Error{Msg: "turnstile verify bad json: " + jerr.Error()}
			} else if ok, _ := data["ok"].(bool); ok {
				return nil
			} else {
				last = &Error{Msg: fmt.Sprintf("turnstile verify failed: %s", trim(resp.Body))}
			}
		} else {
			last = &Error{Msg: "turnstile verify request failed: " + err.Error(), Retryable: true}
		}
		if !Retryable(last) {
			return last
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(cfg.RetryBackoffSeconds*float64(attempt+1)) * time.Second):
		}
	}
	return last
}

func (s *Solver) record(site, proxy string, start time.Time, err error) {
	rec := Record{
		Site:      site,
		Proxy:     proxy,
		At:        start,
		ElapsedMs: time.Since(start).Milliseconds(),
		OK:        err == nil,
	}
	if err != nil {
		rec.Error = err.Error()
	}
	s.mu.Lock()
	s.history = append(s.history, rec)
	if len(s.history) > 100 {
		s.history = s.history[len(s.history)-100:]
	}
	s.mu.Unlock()
}

// Status reports the cached cookie lifetime for every configured site.
func (s *Solver) Status(codes []string) []SiteStatus {
	s.mu.Lock()
	cores := make([]*core, 0, len(s.cores))
	for _, c := range s.cores {
		cores = append(cores, c)
	}
	s.mu.Unlock()

	out := make([]SiteStatus, 0, len(codes))
	now := time.Now()
	for _, code := range codes {
		st := SiteStatus{Site: code}
		for _, c := range cores {
			c.mu.Lock()
			cs := c.cache[code]
			c.mu.Unlock()
			if cs != nil {
				st.Valid = now.Before(cs.expiresAt)
				st.ExpiresAt = cs.expiresAt.Unix()
				st.RemainingS = int64(cs.expiresAt.Sub(now).Seconds())
				st.CookieNum = len(cs.cookies)
				break
			}
		}
		out = append(out, st)
	}
	sortStatus(out)
	return out
}

func (s *Solver) History() []Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Record, len(s.history))
	copy(out, s.history)
	return out
}

func sortStatus(s []SiteStatus) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j].Site < s[j-1].Site; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func urlQueryEscape(s string) string {
	var b bytes.Buffer
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func trim(b []byte) string {
	const max = 300
	s := string(b)
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}

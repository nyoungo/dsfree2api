package turnstile

import (
	"context"
	"sort"
	"time"

	"github.com/nyoungo/dsfree2api/internal/config"
	"github.com/nyoungo/dsfree2api/internal/httpx"
)

// WarmTarget is one cookie-pool slot: a site reached through one proxy route.
// Verified cookies are bound to the egress IP that solved them, so the pool
// keeps one slot per site × route.
type WarmTarget struct {
	Site  config.Site
	Proxy string // "" means direct
}

// WarmEntry describes the pool state of one site × route slot.
type WarmEntry struct {
	Site      string `json:"site"`
	Route     string `json:"route"`
	Valid     bool   `json:"valid"`
	Remaining int64  `json:"remaining_s"`
	Warming   bool   `json:"warming"`
	LastAt    int64  `json:"last_at"`
	LastMs    int64  `json:"last_ms"`
	LastErr   string `json:"last_error,omitempty"`
	NextWarm  int64  `json:"next_warm_at"`
}

// PoolStatus is the console view of the cookie pool.
type PoolStatus struct {
	Enabled bool        `json:"enabled"`
	Running bool        `json:"running"`
	Ratio   float64     `json:"ratio"`
	CheckS  int         `json:"check_seconds"`
	Entries []WarmEntry `json:"entries"`
}

type warmState struct {
	site    string
	route   string
	warming bool
	fails   int
	lastAt  time.Time
	lastMs  int64
	lastErr string
}

// StartWarmer launches the cookie-pool loop. targets is consulted on every
// sweep, so console edits (sites, routes, toggles) are picked up live. A slot
// is warmed when its cookie is missing, expired, or has less than warm_ratio
// of its TTL left — requests then never wait for a solve to finish.
func (s *Solver) StartWarmer(ctx context.Context, targets func() []WarmTarget) {
	if targets == nil {
		return
	}
	s.poolMu.Lock()
	if s.poolOn {
		s.poolMu.Unlock()
		return
	}
	s.poolOn = true
	if s.warms == nil {
		s.warms = map[string]*warmState{}
	}
	s.poolMu.Unlock()
	go s.warmLoop(ctx, targets)
}

func (s *Solver) warmLoop(ctx context.Context, targets func() []WarmTarget) {
	defer func() {
		s.poolMu.Lock()
		s.poolOn = false
		s.poolMu.Unlock()
	}()
	for {
		s.sweep(ctx, targets)
		wait := time.Duration(s.Config().WarmCheckValue()) * time.Second
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// sweep warms every due slot. Solves run in the background so one slow
// browser window cannot stall the inspection; a slot already being warmed is
// skipped until it finishes, and a slot whose last attempt failed waits out an
// exponential backoff before the next try.
func (s *Solver) sweep(ctx context.Context, targets func() []WarmTarget) {
	cfg := s.Config()
	if !cfg.Enabled || !cfg.WarmEnabled || cfg.ProviderValue() == config.ProviderManual {
		return
	}
	ttl := time.Duration(cfg.CookieTTLSeconds) * time.Second
	if ttl <= 0 {
		ttl = 3 * time.Hour
	}
	for _, tgt := range targets() {
		key := poolKey(tgt.Proxy, tgt.Site.Code)
		if !s.warmDue(key, tgt, ttl, cfg.WarmRatioValue(), cfg.WarmCheckValue()) {
			continue
		}
		s.markWarm(key, tgt, true, time.Time{}, 0, "")
		go s.warmOne(ctx, key, tgt, cfg)
	}
}

func (s *Solver) warmDue(key string, tgt WarmTarget, ttl time.Duration, ratio float64, checkS int) bool {
	s.poolMu.Lock()
	st := s.warms[key]
	if st != nil && st.warming {
		s.poolMu.Unlock()
		return false
	}
	var lastAt time.Time
	fails := 0
	if st != nil {
		lastAt, fails = st.lastAt, st.fails
	}
	s.poolMu.Unlock()

	// A failed slot backs off exponentially (×2 per consecutive failure, up
	// to 15 minutes) so a hopeless route cannot retry-storm the browser pool.
	if fails > 0 && !lastAt.IsZero() {
		backoff := time.Duration(checkS) * time.Second
		for i := 1; i < fails && backoff < 15*time.Minute; i++ {
			backoff *= 2
		}
		if backoff > 15*time.Minute || backoff <= 0 {
			backoff = 15 * time.Minute
		}
		if time.Since(lastAt) < backoff {
			return false
		}
	}

	c := s.coreFor(tgt.Proxy)
	c.mu.Lock()
	cs := c.cache[tgt.Site.Code]
	c.mu.Unlock()
	if cs == nil {
		return true // cold slot — fill it
	}
	return time.Until(cs.expiresAt) <= time.Duration(ratio*float64(ttl))
}

func (s *Solver) warmOne(ctx context.Context, key string, tgt WarmTarget, cfg config.Turnstile) {
	site := tgt.Site
	start := time.Now()
	sess, err := httpx.NewSession(httpx.Options{
		ProxyURL: tgt.Proxy,
		Timeout:  time.Duration(cfg.SolveTimeoutValue()+30) * time.Second,
	})
	if err == nil {
		err = s.Refresh(ctx, sess, &site, tgt.Proxy)
		sess.Close()
	}
	lastErr := ""
	if err != nil {
		lastErr = err.Error()
	}
	s.markWarm(key, tgt, false, start, time.Since(start).Milliseconds(), lastErr)
}

func (s *Solver) markWarm(key string, tgt WarmTarget, warming bool, at time.Time, ms int64, lastErr string) {
	s.poolMu.Lock()
	defer s.poolMu.Unlock()
	if s.warms == nil {
		s.warms = map[string]*warmState{}
	}
	st, ok := s.warms[key]
	if !ok {
		st = &warmState{site: tgt.Site.Code, route: routeLabel(tgt.Proxy)}
		s.warms[key] = st
	}
	st.warming = warming
	if !at.IsZero() {
		st.lastAt = at
		st.lastMs = ms
		st.lastErr = lastErr
		if lastErr == "" {
			st.fails = 0
		} else {
			st.fails++
		}
	}
}

// PoolStatus reports every known slot: cached lifetime, warm progress, last
// solve duration and the next planned refresh.
func (s *Solver) PoolStatus() PoolStatus {
	cfg := s.Config()
	pooling := cfg.Enabled && cfg.WarmEnabled && cfg.ProviderValue() != config.ProviderManual
	ps := PoolStatus{
		Enabled: pooling,
		Ratio:   cfg.WarmRatioValue(),
		CheckS:  cfg.WarmCheckValue(),
		Entries: make([]WarmEntry, 0, 4),
	}
	s.poolMu.Lock()
	ps.Running = s.poolOn
	states := make(map[string]*warmState, len(s.warms))
	for k, v := range s.warms {
		cp := *v
		states[k] = &cp
	}
	s.poolMu.Unlock()

	s.mu.Lock()
	cores := make([]*core, 0, len(s.cores))
	for _, c := range s.cores {
		cores = append(cores, c)
	}
	s.mu.Unlock()

	now := time.Now()
	ttl := time.Duration(cfg.CookieTTLSeconds) * time.Second
	if ttl <= 0 {
		ttl = 3 * time.Hour
	}

	entries := map[string]*WarmEntry{}
	for _, c := range cores {
		c.mu.Lock()
		for code, cs := range c.cache {
			e := &WarmEntry{Site: code, Route: c.proxy, Valid: now.Before(cs.expiresAt)}
			if e.Valid {
				e.Remaining = int64(cs.expiresAt.Sub(now).Seconds())
			}
			entries[poolKey(c.proxy, code)] = e
		}
		c.mu.Unlock()
	}
	for k, st := range states {
		e, ok := entries[k]
		if !ok {
			e = &WarmEntry{Site: st.site, Route: st.route}
			entries[k] = e
		}
		e.Warming = st.warming
		if !st.lastAt.IsZero() {
			e.LastAt = st.lastAt.Unix()
		}
		e.LastMs = st.lastMs
		e.LastErr = st.lastErr
	}
	for _, e := range entries {
		if pooling && e.Valid {
			until := time.Duration(e.Remaining)*time.Second - time.Duration(ps.Ratio*float64(ttl))
			if until < 0 {
				until = 0
			}
			e.NextWarm = now.Add(until).Unix()
		}
		ps.Entries = append(ps.Entries, *e)
	}
	sort.Slice(ps.Entries, func(i, j int) bool {
		if ps.Entries[i].Site != ps.Entries[j].Site {
			return ps.Entries[i].Site < ps.Entries[j].Site
		}
		return ps.Entries[i].Route < ps.Entries[j].Route
	})
	return ps
}

func poolKey(proxy, site string) string { return routeLabel(proxy) + "|" + site }

func routeLabel(proxy string) string {
	if proxy == "" {
		return "direct"
	}
	return proxy
}

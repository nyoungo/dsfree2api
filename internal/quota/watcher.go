// Package quota watches the upstream sites' guest-token balances so the
// console and logs show how much free daily quota is left before a site cuts
// requests off. Readings reuse the pooled cookies and egress of the chat
// path, so they describe the identity requests are actually debited from.
package quota

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nyoungo/dsfree2api/internal/config"
	"github.com/nyoungo/dsfree2api/internal/httpx"
	"github.com/nyoungo/dsfree2api/internal/proxypool"
	"github.com/nyoungo/dsfree2api/internal/turnstile"
)

// Balance is one site × bot quota reading. Empty Error means the reading was
// taken successfully; Limit/Used/Remaining come from the site's own balance
// endpoint (the guest daily free allowance).
type Balance struct {
	Site        string  `json:"site"`
	BotID       int     `json:"bot_id"`
	Model       string  `json:"model,omitempty"`
	Route       string  `json:"route"`
	PaidBalance int64   `json:"paid_balance"`
	Limit       int64   `json:"limit"`
	Used        int64   `json:"used"`
	Remaining   int64   `json:"remaining"`
	Ratio       float64 `json:"ratio"`
	ResetPeriod string  `json:"reset_period,omitempty"`
	CheckedAt   int64   `json:"checked_at"`
	Error       string  `json:"error,omitempty"`
}

// Snapshot is the console view of the sentinel.
type Snapshot struct {
	Enabled   bool      `json:"enabled"`
	Running   bool      `json:"running"`
	CheckS    int       `json:"check_seconds"`
	WarnRatio float64   `json:"warn_ratio"`
	Entries   []Balance `json:"entries"`
}

type botRef struct {
	id    int
	model string
}

// Watcher polls each site's balance REST endpoint on an interval. One slot
// per site × bot; slots without verified cookies are reported as such and
// never trigger solves by themselves.
type Watcher struct {
	cfg  *config.Config
	log  *slog.Logger
	ts   *turnstile.Solver
	pool *proxypool.Manager

	// newSession is a test seam; nil uses the real Chrome-impersonating one.
	newSession func(proxy string) (httpx.Session, error)

	mu      sync.Mutex
	entries map[string]*Balance // key: site|bot
	warned  map[string]bool
	lastErr map[string]string
	running bool
}

// New builds a watcher. Start launches the loop when [quota].enabled.
func New(cfg *config.Config, log *slog.Logger, ts *turnstile.Solver, pool *proxypool.Manager) *Watcher {
	if log == nil {
		log = slog.Default()
	}
	return &Watcher{
		cfg:     cfg,
		log:     log,
		ts:      ts,
		pool:    pool,
		entries: map[string]*Balance{},
		warned:  map[string]bool{},
		lastErr: map[string]string{},
	}
}

// Start launches the polling loop; no-op unless [quota].enabled.
func (w *Watcher) Start(ctx context.Context) {
	w.cfg.RLock()
	enabled := w.cfg.Quota.Enabled
	w.cfg.RUnlock()
	if !enabled || w.ts == nil {
		return
	}
	go w.loop(ctx)
}

func (w *Watcher) loop(ctx context.Context) {
	w.mu.Lock()
	w.running = true
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		w.running = false
		w.mu.Unlock()
	}()
	// First sweep after a short settle delay (pool/cookies may still be
	// warming up), then on every check interval.
	timer := time.NewTimer(20 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			w.Sweep(ctx)
			w.cfg.RLock()
			iv := time.Duration(w.cfg.Quota.CheckSecondsValue()) * time.Second
			w.cfg.RUnlock()
			timer.Reset(iv)
		}
	}
}

// Sweep queries every enabled site once. Exposed for tests and manual runs.
func (w *Watcher) Sweep(ctx context.Context) {
	if w.ts == nil {
		return
	}
	w.cfg.RLock()
	sites := map[string]config.Site{}
	for code, st := range w.cfg.Sites {
		if st != nil && st.Enabled {
			sites[code] = *st
		}
	}
	bots := map[string][]botRef{}
	for id, m := range w.cfg.Models {
		if m == nil || !m.Enabled || m.BotID <= 0 {
			continue
		}
		bots[m.Site] = append(bots[m.Site], botRef{id: m.BotID, model: id})
	}
	warnRatio := w.cfg.Quota.WarnRatioValue()
	w.cfg.RUnlock()

	codes := make([]string, 0, len(sites))
	for code := range sites {
		codes = append(codes, code)
	}
	sort.Strings(codes)
	for _, code := range codes {
		refs := bots[code]
		if len(refs) == 0 {
			continue
		}
		sort.Slice(refs, func(i, j int) bool { return refs[i].id < refs[j].id })
		w.checkSite(ctx, sites[code], refs, warnRatio)
	}
}

func (w *Watcher) checkSite(ctx context.Context, site config.Site, refs []botRef, warnRatio float64) {
	route := w.routeFor(site.Code)
	if !w.ts.CookiesReady(site.Code, route) {
		w.markAll(site.Code, refs, route, errors.New("no verified cookies for this route yet"))
		return
	}
	sess, err := w.session(route)
	if err != nil {
		w.markAll(site.Code, refs, route, err)
		return
	}
	defer sess.Close()
	if err := w.ts.ApplyValidCookies(ctx, sess, &site, route); err != nil {
		w.markAll(site.Code, refs, route, err)
		return
	}
	// Guests need no REST nonce: the visitor id travels as the dsgt_gid
	// cookie and the balance endpoint keys on it.
	if gid := sess.Cookies()["dsgt_gid"]; gid == "" {
		w.markAll(site.Code, refs, route, errors.New("no dsgt_gid cookie for this route yet"))
		return
	}
	endpoint := strings.TrimRight(site.BaseURL, "/") + "/wp-json/dsgt/v1/balance"
	for _, ref := range refs {
		payload, err := fetchBalance(ctx, sess, endpoint, ref.id)
		w.record(site.Code, ref, route, payload, err, warnRatio)
	}
}

func (w *Watcher) markAll(site string, refs []botRef, route string, err error) {
	for _, ref := range refs {
		w.record(site, ref, route, nil, err, 0)
	}
}

func (w *Watcher) record(site string, ref botRef, route string, payload *balancePayload, err error, warnRatio float64) {
	key := site + "|" + strconv.Itoa(ref.id)
	b := &Balance{
		Site: site, BotID: ref.id, Model: ref.model,
		Route: routeLabel(route), CheckedAt: time.Now().Unix(),
	}
	if err != nil {
		b.Error = err.Error()
	} else {
		b.PaidBalance = payload.Balance
		b.Limit = payload.Free.Limit
		b.Used = payload.Free.Used
		b.Remaining = payload.Free.Remaining
		b.ResetPeriod = payload.Free.ResetPeriod
		if b.Limit > 0 {
			b.Ratio = float64(b.Remaining) / float64(b.Limit)
		}
	}

	var lowNow, recovered, resetSeen bool
	var prevUsed int64
	w.mu.Lock()
	if prev := w.entries[key]; prev != nil && prev.Error == "" && err == nil && b.Used < prev.Used {
		resetSeen = true
		prevUsed = prev.Used
	}
	w.entries[key] = b
	prevErr := w.lastErr[key]
	w.lastErr[key] = b.Error
	if err == nil && b.Limit > 0 {
		low := float64(b.Remaining) <= warnRatio*float64(b.Limit)
		if low && !w.warned[key] {
			w.warned[key] = true
			lowNow = true
		}
		if !low && w.warned[key] {
			w.warned[key] = false
			recovered = true
		}
	}
	w.mu.Unlock()

	if err != nil {
		if b.Error != prevErr {
			w.log.Warn("quota check failed",
				"site", site, "bot_id", ref.id, "model", ref.model, "route", b.Route, "error", b.Error)
		}
		return
	}
	if prevErr != "" {
		w.log.Info("quota check recovered", "site", site, "bot_id", ref.id, "route", b.Route)
	}
	w.log.Debug("quota check",
		"site", site, "bot_id", ref.id, "model", ref.model, "route", b.Route,
		"remaining", b.Remaining, "limit", b.Limit, "used", b.Used)
	if resetSeen {
		w.log.Info("quota reset observed",
			"site", site, "bot_id", ref.id, "model", ref.model, "route", b.Route,
			"used_before", prevUsed, "used_after", b.Used,
			"checked_at", time.Unix(b.CheckedAt, 0).Format(time.RFC3339))
	}
	switch {
	case lowNow:
		w.log.Warn("quota low",
			"site", site, "bot_id", ref.id, "model", ref.model, "route", b.Route,
			"remaining", b.Remaining, "limit", b.Limit, "reset_period", b.ResetPeriod)
	case recovered:
		w.log.Info("quota recovered",
			"site", site, "bot_id", ref.id, "model", ref.model,
			"remaining", b.Remaining, "limit", b.Limit)
	}
}

func (w *Watcher) routeFor(site string) string {
	if w.pool != nil {
		if cands := w.pool.Candidates(site); len(cands) > 0 {
			return cands[0].Proxy
		}
	}
	return ""
}

func (w *Watcher) session(proxy string) (httpx.Session, error) {
	if w.newSession != nil {
		return w.newSession(proxy)
	}
	return httpx.NewSession(httpx.Options{ProxyURL: proxy, Timeout: 45 * time.Second})
}

func routeLabel(proxy string) string {
	if proxy == "" {
		return "direct"
	}
	return proxy
}

// freeQuota is the daily guest allowance block of a balance response.
type freeQuota struct {
	Limit       int64  `json:"limit"`
	Used        int64  `json:"used"`
	Remaining   int64  `json:"remaining"`
	ResetPeriod string `json:"reset_period"`
}

type balancePayload struct {
	Balance int64      `json:"balance"`
	Free    *freeQuota `json:"free"`
}

func fetchBalance(ctx context.Context, sess httpx.Session, endpoint string, botID int) (*balancePayload, error) {
	u := fmt.Sprintf("%s?bot_id=%d", endpoint, botID)
	resp, err := sess.Do(ctx, httpx.Request{
		Method: http.MethodGet,
		URL:    u,
		Header: map[string]string{"Accept": "application/json"},
	})
	if err != nil {
		return nil, fmt.Errorf("balance request: %w", err)
	}
	if resp.Status != http.StatusOK {
		return nil, fmt.Errorf("balance request: HTTP %d", resp.Status)
	}
	var p balancePayload
	if err := json.Unmarshal(resp.Body, &p); err != nil {
		return nil, fmt.Errorf("balance json: %w", err)
	}
	if p.Free == nil {
		return nil, errors.New("balance response has no free quota block")
	}
	return &p, nil
}

// Snapshot returns the current readings for the console.
func (w *Watcher) Snapshot() Snapshot {
	w.cfg.RLock()
	enabled := w.cfg.Quota.Enabled
	checkS := w.cfg.Quota.CheckSecondsValue()
	ratio := w.cfg.Quota.WarnRatioValue()
	w.cfg.RUnlock()

	w.mu.Lock()
	entries := make([]Balance, 0, len(w.entries))
	for _, b := range w.entries {
		entries = append(entries, *b)
	}
	running := w.running
	w.mu.Unlock()
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Site != entries[j].Site {
			return entries[i].Site < entries[j].Site
		}
		return entries[i].BotID < entries[j].BotID
	})
	return Snapshot{Enabled: enabled, Running: running, CheckS: checkS, WarnRatio: ratio, Entries: entries}
}

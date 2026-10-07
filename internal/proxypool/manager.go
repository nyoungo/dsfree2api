package proxypool

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nyoungo/dsfree2api/internal/config"
	"github.com/nyoungo/dsfree2api/internal/upstream"
)

// endpoint is one runtime pool slot.
type endpoint struct {
	name    string
	kind    string // "xray" | "url" | "invalid"
	scheme  string
	display string
	link    string
	source  string // "" = manual entry, else subscription name
	enabled bool

	parseErr string
	outbound map[string]any
	url      string
	proxyURL string // resolved egress URL (socks5://127.0.0.1:port or plain)

	checked   bool
	healthy   bool
	latencyMs int64
	lastErr   string
	checkedAt time.Time
}

type subState struct {
	name      string
	nodes     []string
	lastErr   string
	fetchedAt time.Time
}

// Manager owns the proxy pool: endpoints, health, subscriptions and sticky
// per-site selection. It is safe for concurrent use.
type Manager struct {
	cfg  *config.Config
	log  *slog.Logger
	dir  string
	xray *xrayCore

	reloadMu sync.Mutex

	mu        sync.Mutex
	endpoints map[string]*endpoint
	subs      map[string]*subState
	sticky    map[string]string
	fetching  map[string]bool
	started   bool
	lastCheck time.Time
	lastErr   string
}

func New(cfg *config.Config, dataDir string, log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	return &Manager{
		cfg:       cfg,
		log:       log,
		dir:       dataDir,
		xray:      newXrayCore(dataDir, log),
		endpoints: map[string]*endpoint{},
		subs:      map[string]*subState{},
		sticky:    map[string]string{},
		fetching:  map[string]bool{},
	}
}

// Start runs the background loop: reload from config, refresh due
// subscriptions, probe endpoint health.
func (m *Manager) Start(ctx context.Context) {
	m.mu.Lock()
	if m.started {
		m.mu.Unlock()
		return
	}
	m.started = true
	m.mu.Unlock()
	go m.loop(ctx)
}

func (m *Manager) loop(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			m.xray.stop()
			return
		case <-ticker.C:
		}
		m.cycle(ctx)
	}
}

func (m *Manager) cycle(ctx context.Context) {
	m.Reload()

	m.cfg.RLock()
	enabled := m.cfg.ProxyPool.Enabled
	interval := time.Duration(m.cfg.ProxyPool.CheckIntervalSeconds) * time.Second
	subs := make(map[string]config.ProxySubscription, len(m.cfg.ProxyPool.Subscriptions))
	for name, sc := range m.cfg.ProxyPool.Subscriptions {
		if sc != nil {
			subs[name] = *sc
		}
	}
	m.cfg.RUnlock()
	if !enabled {
		return
	}
	if interval <= 0 {
		interval = 120 * time.Second
	}

	m.mu.Lock()
	due := m.lastCheck.IsZero() || time.Since(m.lastCheck) >= interval
	m.mu.Unlock()
	if due {
		m.checkAll(ctx)
	}

	now := time.Now()
	for name, sc := range subs {
		if !sc.Enabled || strings.TrimSpace(sc.URL) == "" {
			continue
		}
		m.mu.Lock()
		st := m.subs[name]
		busy := m.fetching[name]
		m.mu.Unlock()
		if busy {
			continue
		}
		iv := time.Duration(sc.IntervalMinutes) * time.Minute
		if iv <= 0 {
			iv = time.Hour
		}
		if st != nil && !st.fetchedAt.IsZero() && now.Sub(st.fetchedAt) < iv {
			continue
		}
		go m.fetchSub(ctx, name, sc)
	}
}

// Reload rebuilds the runtime endpoint table from the live config plus the
// cached subscription nodes, (re)configures the Xray core and prunes sticky
// picks that no longer resolve.
func (m *Manager) Reload() {
	m.reloadMu.Lock()
	defer m.reloadMu.Unlock()

	m.cfg.RLock()
	poolCfg := m.cfg.ProxyPool
	entries := make(map[string]config.ProxyEntry, len(poolCfg.Entries))
	for name, e := range poolCfg.Entries {
		if e != nil {
			entries[name] = *e
		}
	}
	subsCfg := make(map[string]config.ProxySubscription, len(poolCfg.Subscriptions))
	for name, sc := range poolCfg.Subscriptions {
		if sc != nil {
			subsCfg[name] = *sc
		}
	}
	m.cfg.RUnlock()

	m.mu.Lock()
	subNodes := make(map[string][]string, len(m.subs))
	for name, st := range m.subs {
		subNodes[name] = append([]string(nil), st.nodes...)
	}
	old := m.endpoints
	m.mu.Unlock()

	desired := map[string]*endpoint{}
	for name, e := range entries {
		if strings.TrimSpace(e.Link) == "" {
			continue
		}
		ep := newEndpoint(name, e.Link, poolCfg.DefaultScheme, "")
		ep.enabled = e.Enabled
		desired[name] = ep
	}
	for subName, nodes := range subNodes {
		sc, ok := subsCfg[subName]
		if !ok {
			continue
		}
		for _, line := range nodes {
			name := subName + "#" + shortHash(line)
			if _, dup := desired[name]; dup {
				continue
			}
			ep := newEndpoint(name, line, poolCfg.DefaultScheme, subName)
			ep.enabled = sc.Enabled
			desired[name] = ep
		}
	}
	for name, ep := range desired {
		if o := old[name]; o != nil && o.link == ep.link {
			ep.checked = o.checked
			ep.healthy = o.healthy
			ep.latencyMs = o.latencyMs
			ep.lastErr = o.lastErr
			ep.checkedAt = o.checkedAt
		}
	}

	nodes := make([]xrayNode, 0, len(desired))
	for _, ep := range desired {
		if ep.kind == "xray" && ep.enabled && ep.parseErr == "" && ep.outbound != nil {
			nodes = append(nodes, xrayNode{Name: ep.name, Outbound: ep.outbound})
		}
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Name < nodes[j].Name })

	if len(nodes) > 0 {
		if err := m.xray.ensure(poolCfg.XrayPath, poolCfg.XrayAutoDownload, poolCfg.XrayVersion); err != nil {
			m.setErr(err)
		} else if err := m.xray.configure(nodes); err != nil {
			m.setErr(err)
		} else {
			m.setErr(nil)
		}
	} else {
		_ = m.xray.configure(nil)
		m.setErr(nil)
	}

	xrayUp := m.xray.status().Running
	for _, ep := range desired {
		switch ep.kind {
		case "url":
			ep.proxyURL = ep.url
		case "xray":
			if ep.parseErr == "" && xrayUp {
				if port := m.xray.port(ep.name); port > 0 {
					ep.proxyURL = fmt.Sprintf("socks5://127.0.0.1:%d", port)
				}
			}
		}
	}

	m.mu.Lock()
	m.endpoints = desired
	for site, name := range m.sticky {
		ep := desired[name]
		if ep == nil || !ep.enabled || ep.proxyURL == "" {
			delete(m.sticky, site)
		}
	}
	m.mu.Unlock()
}

// RefreshNow refreshes every subscription and probes every endpoint, then
// reloads. Used by the console button.
func (m *Manager) RefreshNow(ctx context.Context) {
	go func() {
		m.Reload()
		m.cfg.RLock()
		enabled := m.cfg.ProxyPool.Enabled
		subs := make(map[string]config.ProxySubscription, len(m.cfg.ProxyPool.Subscriptions))
		for name, sc := range m.cfg.ProxyPool.Subscriptions {
			if sc != nil {
				subs[name] = *sc
			}
		}
		m.cfg.RUnlock()
		for name, sc := range subs {
			if sc.Enabled && strings.TrimSpace(sc.URL) != "" {
				m.fetchSub(ctx, name, sc)
			}
		}
		if enabled {
			m.checkAll(ctx)
		}
		m.Reload()
	}()
}

func (m *Manager) fetchSub(ctx context.Context, name string, sc config.ProxySubscription) {
	m.mu.Lock()
	m.fetching[name] = true
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.fetching, name)
		m.mu.Unlock()
	}()

	client, err := m.fetchClient()
	if err == nil {
		var req *http.Request
		req, err = http.NewRequestWithContext(ctx, http.MethodGet, sc.URL, nil)
		if err == nil {
			var resp *http.Response
			resp, err = client.Do(req)
			if err == nil {
				defer resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					err = fmt.Errorf("subscription http %d", resp.StatusCode)
				} else {
					var raw []byte
					raw, err = io.ReadAll(io.LimitReader(resp.Body, 8<<20))
					if err == nil {
						lines := ParseSubscriptionBody(raw)
						if len(lines) == 0 {
							err = fmt.Errorf("subscription returned no nodes")
						} else {
							err = nil
							m.subResult(name, lines, nil)
							m.log.Info("subscription refreshed", "name", name, "nodes", len(lines))
							m.Reload()
							return
						}
					}
				}
			}
		}
	}
	m.subResult(name, nil, err)
	m.log.Warn("subscription refresh failed", "name", name, "error", err)
}

func (m *Manager) subResult(name string, nodes []string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.subs[name]
	if st == nil {
		st = &subState{name: name}
		m.subs[name] = st
	}
	if err != nil {
		st.lastErr = err.Error()
		return
	}
	st.nodes = nodes
	st.lastErr = ""
	st.fetchedAt = time.Now()
}

func (m *Manager) fetchClient() (*http.Client, error) {
	m.cfg.RLock()
	proxy := strings.TrimSpace(m.cfg.Proxy.URL)
	m.cfg.RUnlock()
	tr := &http.Transport{
		TLSHandshakeTimeout:   20 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
	}
	if proxy != "" {
		u, err := url.Parse(proxy)
		if err != nil {
			return nil, fmt.Errorf("proxy url: %w", err)
		}
		tr.Proxy = http.ProxyURL(u)
	}
	return &http.Client{Transport: tr, Timeout: 45 * time.Second}, nil
}

func (m *Manager) checkAll(ctx context.Context) {
	m.mu.Lock()
	list := make([]*endpoint, 0, len(m.endpoints))
	for _, ep := range m.endpoints {
		list = append(list, ep)
	}
	m.mu.Unlock()

	m.cfg.RLock()
	timeout := time.Duration(m.cfg.ProxyPool.CheckTimeoutSeconds) * time.Second
	checkURL := strings.TrimSpace(m.cfg.ProxyPool.CheckURL)
	m.cfg.RUnlock()
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	if checkURL == "" {
		checkURL = "https://www.gstatic.com/generate_204"
	}

	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for _, ep := range list {
		if !ep.enabled || ep.proxyURL == "" {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(ep *endpoint) {
			defer wg.Done()
			defer func() { <-sem }()
			if ctx.Err() != nil {
				return
			}
			ok, ms, errMsg := probeURL(ctx, ep.proxyURL, checkURL, timeout)
			m.mu.Lock()
			ep.checked = true
			ep.checkedAt = time.Now()
			ep.healthy = ok
			ep.latencyMs = ms
			ep.lastErr = errMsg
			m.mu.Unlock()
		}(ep)
	}
	wg.Wait()

	m.mu.Lock()
	m.lastCheck = time.Now()
	m.mu.Unlock()
}

func probeURL(ctx context.Context, proxyURL, checkURL string, timeout time.Duration) (bool, int64, string) {
	u, err := url.Parse(proxyURL)
	if err != nil {
		return false, 0, "bad proxy url"
	}
	tr := &http.Transport{
		Proxy:                 http.ProxyURL(u),
		TLSHandshakeTimeout:   timeout,
		ResponseHeaderTimeout: timeout,
		DisableKeepAlives:     true,
	}
	client := &http.Client{Transport: tr, Timeout: timeout}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, checkURL, nil)
	if err != nil {
		return false, 0, err.Error()
	}
	start := time.Now()
	resp, err := client.Do(req)
	ms := time.Since(start).Milliseconds()
	if err != nil {
		return false, ms, trimErr(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 400 {
		return false, ms, fmt.Sprintf("http %d", resp.StatusCode)
	}
	return true, ms, ""
}

// Candidates resolves the ordered route candidates for one site: its bound
// entries (or subscriptions) filtered to healthy ones, sticky pick first.
// It implements upstream.RouteSource.
func (m *Manager) Candidates(site string) []upstream.RouteCandidate {
	m.cfg.RLock()
	enabled := m.cfg.ProxyPool.Enabled
	var items []string
	if st, ok := m.cfg.Sites[site]; ok && st != nil {
		items = append(items, st.Proxies...)
	}
	m.cfg.RUnlock()
	if !enabled || len(items) == 0 {
		return nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	var all []*endpoint
	for _, item := range items {
		all = append(all, m.resolveItemLocked(item)...)
	}
	usable := make([]*endpoint, 0, len(all))
	for _, ep := range all {
		if ep.enabled && ep.proxyURL != "" && ep.parseErr == "" {
			usable = append(usable, ep)
		}
	}
	pick := make([]*endpoint, 0, len(usable))
	for _, ep := range usable {
		if !ep.checked || ep.healthy {
			pick = append(pick, ep)
		}
	}
	if len(pick) == 0 {
		pick = usable
	}
	if len(pick) == 0 {
		return nil
	}
	if chosen := m.sticky[site]; chosen != "" {
		for i, ep := range pick {
			if ep.name == chosen && i > 0 {
				pick = append([]*endpoint{ep}, append(pick[:i], pick[i+1:]...)...)
				break
			}
		}
	}
	m.sticky[site] = pick[0].name
	out := make([]upstream.RouteCandidate, 0, len(pick))
	for _, ep := range pick {
		out = append(out, upstream.RouteCandidate{Name: ep.name, Proxy: ep.proxyURL})
	}
	return out
}

func (m *Manager) resolveItemLocked(item string) []*endpoint {
	item = strings.TrimSpace(item)
	if item == "" {
		return nil
	}
	if name, ok := strings.CutPrefix(item, "sub:"); ok {
		var out []*endpoint
		for _, ep := range m.endpoints {
			if ep.source == name {
				out = append(out, ep)
			}
		}
		sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
		return out
	}
	if ep := m.endpoints[item]; ep != nil {
		return []*endpoint{ep}
	}
	return nil
}

// Report records how a request through `proxy` went, so a failing endpoint
// loses its sticky pick immediately and a healthy one is trusted again.
func (m *Manager) Report(proxy string, ok bool) {
	if m == nil || proxy == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, ep := range m.endpoints {
		if ep.proxyURL != proxy {
			continue
		}
		ep.checked = true
		ep.checkedAt = time.Now()
		if ok {
			ep.healthy = true
			ep.lastErr = ""
		} else {
			ep.healthy = false
			ep.lastErr = "request failed"
			for site, name := range m.sticky {
				if name == ep.name {
					delete(m.sticky, site)
				}
			}
		}
		return
	}
}

// Status is the console snapshot.
type Status struct {
	Enabled          bool              `json:"enabled"`
	CheckIntervalS   int               `json:"check_interval_seconds"`
	CheckTimeoutS    int               `json:"check_timeout_seconds"`
	CheckURL         string            `json:"check_url"`
	DefaultScheme    string            `json:"default_scheme"`
	XrayPath         string            `json:"xray_path"`
	XrayVersion      string            `json:"xray_version"`
	XrayAutoDownload bool              `json:"xray_auto_download"`
	Xray             XrayStatus        `json:"xray"`
	Entries          []EntryStatus     `json:"entries"`
	Subscriptions    []SubStatus       `json:"subscriptions"`
	Sticky           map[string]string `json:"sticky"`
	LastError        string            `json:"last_error,omitempty"`
}

type EntryStatus struct {
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Scheme     string `json:"scheme"`
	Target     string `json:"target"`
	Source     string `json:"source,omitempty"`
	Enabled    bool   `json:"enabled"`
	Healthy    bool   `json:"healthy"`
	Checked    bool   `json:"checked"`
	LatencyMs  int64  `json:"latency_ms"`
	LastErr    string `json:"last_error,omitempty"`
	ParseError string `json:"parse_error,omitempty"`
	CheckedAt  int64  `json:"checked_at"`
}

type SubStatus struct {
	Name            string `json:"name"`
	URL             string `json:"url"`
	Enabled         bool   `json:"enabled"`
	IntervalMinutes int    `json:"interval_minutes"`
	Nodes           int    `json:"nodes"`
	FetchedAt       int64  `json:"fetched_at"`
	LastErr         string `json:"last_error,omitempty"`
}

func (m *Manager) Status() Status {
	m.cfg.RLock()
	poolCfg := m.cfg.ProxyPool
	subsCfg := make(map[string]config.ProxySubscription, len(poolCfg.Subscriptions))
	for name, sc := range poolCfg.Subscriptions {
		if sc != nil {
			subsCfg[name] = *sc
		}
	}
	m.cfg.RUnlock()

	m.mu.Lock()
	endpoints := make([]*endpoint, 0, len(m.endpoints))
	for _, ep := range m.endpoints {
		endpoints = append(endpoints, ep)
	}
	sticky := make(map[string]string, len(m.sticky))
	for k, v := range m.sticky {
		sticky[k] = v
	}
	subs := make(map[string]*subState, len(m.subs))
	for name, st := range m.subs {
		cp := *st
		subs[name] = &cp
	}
	lastErr := m.lastErr
	m.mu.Unlock()

	st := Status{
		Enabled:          poolCfg.Enabled,
		CheckIntervalS:   poolCfg.CheckIntervalSeconds,
		CheckTimeoutS:    poolCfg.CheckTimeoutSeconds,
		CheckURL:         poolCfg.CheckURL,
		DefaultScheme:    poolCfg.DefaultScheme,
		XrayPath:         poolCfg.XrayPath,
		XrayVersion:      poolCfg.XrayVersion,
		XrayAutoDownload: poolCfg.XrayAutoDownload,
		Xray:             m.xray.status(),
		Sticky:           sticky,
		LastError:        lastErr,
	}
	for _, ep := range endpoints {
		es := EntryStatus{
			Name: ep.name, Kind: ep.kind, Scheme: ep.scheme, Target: ep.display,
			Source: ep.source, Enabled: ep.enabled, Healthy: ep.healthy,
			Checked: ep.checked, LatencyMs: ep.latencyMs, LastErr: ep.lastErr,
			ParseError: ep.parseErr,
		}
		if !ep.checkedAt.IsZero() {
			es.CheckedAt = ep.checkedAt.Unix()
		}
		st.Entries = append(st.Entries, es)
	}
	sort.Slice(st.Entries, func(i, j int) bool {
		a, b := st.Entries[i], st.Entries[j]
		if (a.Source == "") != (b.Source == "") {
			return a.Source == ""
		}
		if a.Source != b.Source {
			return a.Source < b.Source
		}
		return a.Name < b.Name
	})
	for name, sc := range subsCfg {
		ss := SubStatus{Name: name, URL: sc.URL, Enabled: sc.Enabled, IntervalMinutes: sc.IntervalMinutes}
		if stt := subs[name]; stt != nil {
			ss.Nodes = len(stt.nodes)
			ss.LastErr = stt.lastErr
			if !stt.fetchedAt.IsZero() {
				ss.FetchedAt = stt.fetchedAt.Unix()
			}
		}
		st.Subscriptions = append(st.Subscriptions, ss)
	}
	sort.Slice(st.Subscriptions, func(i, j int) bool { return st.Subscriptions[i].Name < st.Subscriptions[j].Name })
	return st
}

func newEndpoint(name, link, defaultScheme, source string) *endpoint {
	ep := &endpoint{name: name, source: source, link: strings.TrimSpace(link)}
	node, err := ParseNode(ep.link, defaultScheme)
	if err != nil {
		ep.kind = "invalid"
		ep.parseErr = err.Error()
		return ep
	}
	ep.kind = node.Kind
	ep.scheme = node.Scheme
	ep.display = node.Display
	ep.outbound = node.Outbound
	ep.url = node.URL
	return ep
}

func (m *Manager) setErr(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err == nil {
		m.lastErr = ""
		return
	}
	m.lastErr = err.Error()
}

func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:4])
}

func trimErr(err error) string {
	s := err.Error()
	if len(s) > 160 {
		return s[:160] + "..."
	}
	return s
}

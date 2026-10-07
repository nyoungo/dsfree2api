// Package metrics collects runtime counters for the web console.
package metrics

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

type Status string

const (
	StatusOK        Status = "ok"
	StatusEmpty     Status = "empty"
	StatusError     Status = "error"
	StatusDenied    Status = "unauthorized"
	StatusRateLimit Status = "rate_limited"
)

type Record struct {
	At               time.Time `json:"at"`
	Model            string    `json:"model"`
	ServedBy         string    `json:"served_by,omitempty"`
	Site             string    `json:"site"`
	Stream           bool      `json:"stream"`
	DurationMs       int64     `json:"duration_ms"`
	TTFTMs           int64     `json:"ttft_ms,omitempty"`
	PromptTokens     int       `json:"prompt_tokens"`
	CompletionTokens int       `json:"completion_tokens"`
	Status           Status    `json:"status"`
	Error            string    `json:"error,omitempty"`
	Route            string    `json:"route,omitempty"`
}

type modelStats struct {
	Requests  int64 `json:"requests"`
	Errors    int64 `json:"errors"`
	Empty     int64 `json:"empty"`
	Prompt    int64 `json:"prompt_tokens"`
	Completed int64 `json:"completion_tokens"`
	LastMs    int64 `json:"last_ms"`
	SumMs     int64 `json:"sum_ms"`
}

type siteStats struct {
	Requests  int64 `json:"requests"`
	Errors    int64 `json:"errors"`
	Quota     int64 `json:"quota_exhausted"`
	Prompt    int64 `json:"prompt_tokens"`
	Completed int64 `json:"completion_tokens"`
}

type dayStats struct {
	Date     string `json:"date"`
	Requests int64  `json:"requests"`
	Prompt   int64  `json:"prompt_tokens"`
	Complete int64  `json:"completion_tokens"`
}

type snapshot struct {
	StartedAt time.Time              `json:"started_at"`
	Requests  int64                  `json:"requests"`
	Errors    int64                  `json:"errors"`
	Empty     int64                  `json:"empty"`
	Denied    int64                  `json:"unauthorized"`
	RateLimit int64                  `json:"rate_limited"`
	Streams   int64                  `json:"streams"`
	InFlight  int64                  `json:"in_flight"`
	Prompt    int64                  `json:"prompt_tokens"`
	Complete  int64                  `json:"completion_tokens"`
	Models    map[string]*modelStats `json:"models"`
	Sites     map[string]*siteStats  `json:"sites"`
	Days      []*dayStats            `json:"days"`
	Recent    []*Record              `json:"recent"`
	Latency   latencyStats           `json:"latency"`
	RPS       float64                `json:"rps"`
}

type latencyStats struct {
	Count int   `json:"count"`
	P50   int64 `json:"p50"`
	P95   int64 `json:"p95"`
	Max   int64 `json:"max"`
}

type Recorder struct {
	mu       sync.Mutex
	started  time.Time
	total    int64
	errs     int64
	empty    int64
	denied   int64
	limited  int64
	streams  int64
	inflight int64
	prompt   int64
	complete int64
	models   map[string]*modelStats
	sites    map[string]*siteStats
	days     map[string]*dayStats
	recent   []*Record
	latency  []int64
	path     string
	stop     chan struct{}
	once     sync.Once
}

const (
	maxRecent  = 200
	maxLatency = 500
)

type persisted struct {
	Total    int64                  `json:"total"`
	Errors   int64                  `json:"errors"`
	Empty    int64                  `json:"empty"`
	Denied   int64                  `json:"denied"`
	Limited  int64                  `json:"limited"`
	Streams  int64                  `json:"streams"`
	Prompt   int64                  `json:"prompt"`
	Complete int64                  `json:"complete"`
	Models   map[string]*modelStats `json:"models"`
	Sites    map[string]*siteStats  `json:"sites"`
	Days     map[string]*dayStats   `json:"days"`
	Recent   []*Record              `json:"recent"`
}

func New(dataDir string) *Recorder {
	r := &Recorder{
		started: time.Now(),
		models:  map[string]*modelStats{},
		sites:   map[string]*siteStats{},
		days:    map[string]*dayStats{},
		path:    filepath.Join(dataDir, "metrics.json"),
		stop:    make(chan struct{}),
	}
	r.load()
	return r
}

func (r *Recorder) Start() {
	go func() {
		t := time.NewTicker(20 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				r.Save()
			case <-r.stop:
				return
			}
		}
	}()
}

func (r *Recorder) Close() {
	r.once.Do(func() { close(r.stop) })
	r.Save()
}

func (r *Recorder) IncInFlight(delta int64) {
	r.mu.Lock()
	r.inflight += delta
	if r.inflight < 0 {
		r.inflight = 0
	}
	r.mu.Unlock()
}

// Record stores one completed request.
func (r *Recorder) Record(rec Record) {
	if rec.At.IsZero() {
		rec.At = time.Now()
	}
	if rec.Model == "" && rec.ServedBy == "" {
		rec.Model = "unknown"
	}
	if rec.Site == "" {
		rec.Site = "unknown"
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	r.total++
	r.prompt += int64(rec.PromptTokens)
	r.complete += int64(rec.CompletionTokens)
	if rec.Stream {
		r.streams++
	}
	switch rec.Status {
	case StatusError:
		r.errs++
	case StatusEmpty:
		r.empty++
		r.errs++
	case StatusDenied:
		r.denied++
	case StatusRateLimit:
		r.limited++
	}

	ms := rec.Model
	if rec.ServedBy != "" {
		ms = rec.ServedBy
	}
	m := r.models[ms]
	if m == nil {
		m = &modelStats{}
		r.models[ms] = m
	}
	m.Requests++
	if rec.Status == StatusError || rec.Status == StatusEmpty {
		m.Errors++
	}
	if rec.Status == StatusEmpty {
		m.Empty++
	}
	m.Prompt += int64(rec.PromptTokens)
	m.Completed += int64(rec.CompletionTokens)
	m.LastMs = rec.DurationMs
	m.SumMs += rec.DurationMs

	if rec.Site != "" {
		s := r.sites[rec.Site]
		if s == nil {
			s = &siteStats{}
			r.sites[rec.Site] = s
		}
		s.Requests++
		s.Prompt += int64(rec.PromptTokens)
		s.Completed += int64(rec.CompletionTokens)
		if rec.Status == StatusError || rec.Status == StatusEmpty {
			s.Errors++
		}
	}

	day := rec.At.Local().Format("2006-01-02")
	d := r.days[day]
	if d == nil {
		d = &dayStats{Date: day}
		r.days[day] = d
	}
	d.Requests++
	d.Prompt += int64(rec.PromptTokens)
	d.Complete += int64(rec.CompletionTokens)
	for len(r.days) > 31 {
		keys := make([]string, 0, len(r.days))
		for k := range r.days {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		delete(r.days, keys[0])
	}

	cp := rec
	r.recent = append(r.recent, &cp)
	if len(r.recent) > maxRecent {
		r.recent = r.recent[len(r.recent)-maxRecent:]
	}
	if rec.DurationMs > 0 {
		r.latency = append(r.latency, rec.DurationMs)
		if len(r.latency) > maxLatency {
			r.latency = r.latency[len(r.latency)-maxLatency:]
		}
	}
}

// SiteTotals is the cumulative per-site usage snapshot; it sizes a quota
// exhaustion — how many tokens had flowed before the site cut us off.
type SiteTotals struct {
	Requests         int64 `json:"requests"`
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	Quota            int64 `json:"quota_exhausted"`
}

// SiteTotals returns the cumulative usage attributed to one site so far.
// Totals persist across restarts through metrics.json.
func (r *Recorder) SiteTotals(site string) SiteTotals {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.sites[site]
	if s == nil {
		return SiteTotals{}
	}
	return SiteTotals{
		Requests:         s.Requests,
		PromptTokens:     s.Prompt,
		CompletionTokens: s.Completed,
		Quota:            s.Quota,
	}
}

// IncSiteQuota counts one site-side quota exhaustion that may never surface as
// a failed request (e.g. swallowed by cross-site failover).
func (r *Recorder) IncSiteQuota(site string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.sites[site]
	if s == nil {
		s = &siteStats{}
		r.sites[site] = s
	}
	s.Quota++
}

func (r *Recorder) Snapshot() any {
	r.mu.Lock()
	defer r.mu.Unlock()

	var lat []int64
	lat = append(lat, r.latency...)
	var p50, p95, max int64
	if n := len(lat); n > 0 {
		sorted := append([]int64(nil), lat...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
		p50 = sorted[n*50/100]
		p95 = sorted[n*95/100]
		if p95 == 0 {
			p95 = sorted[n-1]
		}
		max = sorted[n-1]
	}

	models := make(map[string]*modelStats, len(r.models))
	for k, v := range r.models {
		cp := *v
		if cp.Requests > 0 {
			cp.SumMs = v.SumMs
		}
		models[k] = &cp
	}
	sites := make(map[string]*siteStats, len(r.sites))
	for k, v := range r.sites {
		cp := *v
		sites[k] = &cp
	}
	days := make([]*dayStats, 0, len(r.days))
	for _, v := range r.days {
		cp := *v
		days = append(days, &cp)
	}
	sort.Slice(days, func(i, j int) bool { return days[i].Date < days[j].Date })

	recent := make([]*Record, 0, len(r.recent))
	for i := len(r.recent) - 1; i >= 0; i-- {
		recent = append(recent, r.recent[i])
	}

	var window int64
	cutoff := time.Now().Add(-time.Minute)
	for i := len(r.recent) - 1; i >= 0; i-- {
		if r.recent[i].At.After(cutoff) {
			window++
		}
	}

	return &snapshot{
		StartedAt: r.started,
		Requests:  r.total,
		Errors:    r.errs,
		Empty:     r.empty,
		Denied:    r.denied,
		RateLimit: r.limited,
		Streams:   r.streams,
		InFlight:  r.inflight,
		Prompt:    r.prompt,
		Complete:  r.complete,
		Models:    models,
		Sites:     sites,
		Days:      days,
		Recent:    recent,
		Latency:   latencyStats{Count: len(lat), P50: p50, P95: p95, Max: max},
		RPS:       float64(window) / 60.0,
	}
}

// Reset clears all counters (web console action).
func (r *Recorder) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.total, r.errs, r.empty, r.denied, r.limited, r.streams = 0, 0, 0, 0, 0, 0
	r.prompt, r.complete = 0, 0
	r.models = map[string]*modelStats{}
	r.sites = map[string]*siteStats{}
	r.days = map[string]*dayStats{}
	r.recent = nil
	r.latency = nil
}

func (r *Recorder) Save() {
	r.mu.Lock()
	p := persisted{
		Total: r.total, Errors: r.errs, Empty: r.empty, Denied: r.denied,
		Limited: r.limited, Streams: r.streams, Prompt: r.prompt, Complete: r.complete,
		Models: r.models, Sites: r.sites, Days: r.days, Recent: r.recent,
	}
	path := r.path
	r.mu.Unlock()

	if path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	raw, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

func (r *Recorder) load() {
	raw, err := os.ReadFile(r.path)
	if err != nil {
		return
	}
	var p persisted
	if err := json.Unmarshal(raw, &p); err != nil {
		return
	}
	r.total, r.errs, r.empty, r.denied, r.limited = p.Total, p.Errors, p.Empty, p.Denied, p.Limited
	r.streams, r.prompt, r.complete = p.Streams, p.Prompt, p.Complete
	if p.Models != nil {
		r.models = p.Models
	}
	if p.Sites != nil {
		r.sites = p.Sites
	}
	if p.Days != nil {
		r.days = p.Days
	}
	r.recent = p.Recent
}

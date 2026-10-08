package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/BurntSushi/toml"
)

const DefaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36"

// Turnstile tuning defaults. DefaultSolveTimeoutSeconds is the single source
// for how long one solve attempt may run; the warm_* values drive the cookie
// pool (background pre-solving).
const (
	DefaultSolveTimeoutSeconds = 90
	DefaultWarmRatio           = 0.3
	DefaultWarmCheckSeconds    = 60
)

// Turnstile token providers: api calls a hosted solver, browser drives a
// local Chrome/Edge over CDP, manual only ever uses imported cookies.
const (
	ProviderAPI     = "api"
	ProviderBrowser = "browser"
	ProviderManual  = "manual"
)

type Turnstile struct {
	Enabled             bool    `toml:"enabled" json:"enabled"`
	Provider            string  `toml:"provider" json:"provider"`
	APIURL              string  `toml:"api_url" json:"api_url"`
	APIKey              string  `toml:"api_key" json:"api_key"`
	SiteKey             string  `toml:"sitekey" json:"sitekey"`
	Action              string  `toml:"action" json:"action"`
	APIStyle            string  `toml:"api_style" json:"api_style"`
	TimeoutSeconds      int     `toml:"timeout_seconds" json:"timeout_seconds"`
	CookieTTLSeconds    int     `toml:"cookie_ttl_seconds" json:"cookie_ttl_seconds"`
	Retries             int     `toml:"retries" json:"retries"`
	RetryBackoffSeconds float64 `toml:"retry_backoff_seconds" json:"retry_backoff_seconds"`

	// Cookie pool: a background loop re-solves a site × route slot before its
	// cached cookie expires, so requests never pay the solve latency.
	WarmEnabled      bool    `toml:"warm_enabled" json:"warm_enabled"`
	WarmRatio        float64 `toml:"warm_ratio" json:"warm_ratio"`
	WarmCheckSeconds int     `toml:"warm_check_seconds" json:"warm_check_seconds"`

	// provider = "browser"
	BrowserPath        string `toml:"browser_path" json:"browser_path"`
	BrowserHeadless    bool   `toml:"browser_headless" json:"browser_headless"`
	BrowserUserDataDir string `toml:"browser_user_data_dir" json:"browser_user_data_dir"`
	BrowserTimezone    string `toml:"browser_timezone" json:"browser_timezone"`
	BrowserLocale      string `toml:"browser_locale" json:"browser_locale"`
}

// ProviderValue normalizes the token provider ("api" when unset).
func (t Turnstile) ProviderValue() string {
	p := strings.ToLower(strings.TrimSpace(t.Provider))
	if p == "" {
		return ProviderAPI
	}
	return p
}

// APIStyleValue normalizes api_style for provider "api": "sync" (CapSolver-
// shaped single POST) or "ezsolver" (POST {sitekey,siteurl,timeout} → {token}).
func (t Turnstile) APIStyleValue() string {
	switch strings.ToLower(strings.TrimSpace(t.APIStyle)) {
	case "ezsolver", "ez":
		return "ezsolver"
	default:
		return "sync"
	}
}

// SolveTimeoutValue normalizes timeout_seconds: the longest one solve attempt
// may run (default 90s, clamped to 5–600s).
func (t Turnstile) SolveTimeoutValue() int {
	n := t.TimeoutSeconds
	if n <= 0 {
		return DefaultSolveTimeoutSeconds
	}
	if n < 5 {
		return 5
	}
	if n > 600 {
		return 600
	}
	return n
}

// WarmRatioValue normalizes warm_ratio: a pooled cookie is re-solved once its
// remaining TTL drops below this fraction (default 30%, clamped 5–95%).
func (t Turnstile) WarmRatioValue() float64 {
	r := t.WarmRatio
	if r <= 0 {
		return DefaultWarmRatio
	}
	if r < 0.05 {
		return 0.05
	}
	if r > 0.95 {
		return 0.95
	}
	return r
}

// WarmCheckValue normalizes warm_check_seconds: how often the pool is
// inspected (default 60s, clamped 5s–1h).
func (t Turnstile) WarmCheckValue() int {
	n := t.WarmCheckSeconds
	if n <= 0 {
		return DefaultWarmCheckSeconds
	}
	if n < 5 {
		return 5
	}
	if n > 3600 {
		return 3600
	}
	return n
}

// Quota sentinel defaults: polling interval and the remaining-quota warning
// threshold (share of the daily free limit left).
const (
	DefaultQuotaCheckSeconds = 300
	DefaultQuotaWarnRatio    = 0.2
)

// Responses store defaults: an in-process, bounded, TTL cache backs
// previous_response_id and GET /v1/responses/{id}.
const (
	DefaultResponsesStoreCapacity   = 256
	DefaultResponsesStoreTTLSeconds = 3600
)

// ResponsesConfig tunes the Responses API stateful cache.
type ResponsesConfig struct {
	StoreCapacity   int `toml:"store_capacity" json:"store_capacity"`
	StoreTTLSeconds int `toml:"store_ttl_seconds" json:"store_ttl_seconds"`
}

// StoreCapacityValue normalizes store_capacity (default 256, clamped 1–100000).
func (r ResponsesConfig) StoreCapacityValue() int {
	n := r.StoreCapacity
	if n <= 0 {
		return DefaultResponsesStoreCapacity
	}
	if n > 100000 {
		return 100000
	}
	return n
}

// StoreTTLValue normalizes store_ttl_seconds (default 1h, clamped 1s–7d).
func (r ResponsesConfig) StoreTTLValue() time.Duration {
	n := r.StoreTTLSeconds
	if n <= 0 {
		n = DefaultResponsesStoreTTLSeconds
	}
	if n < 1 {
		n = 1
	}
	if n > 7*24*3600 {
		n = 7 * 24 * 3600
	}
	return time.Duration(n) * time.Second
}

// QuotaWatch configures the guest-token balance sentinel: it polls each
// site's balance endpoint with the pooled session (same egress and cookies as
// chat) and reports remaining daily quota to the console and logs.
type QuotaWatch struct {
	Enabled      bool    `toml:"enabled" json:"enabled"`
	CheckSeconds int     `toml:"check_seconds" json:"check_seconds"`
	WarnRatio    float64 `toml:"warn_ratio" json:"warn_ratio"`
}

// CheckSecondsValue normalizes check_seconds (default 300s, clamped 30–3600).
func (q QuotaWatch) CheckSecondsValue() int {
	n := q.CheckSeconds
	if n <= 0 {
		return DefaultQuotaCheckSeconds
	}
	if n < 30 {
		return 30
	}
	if n > 3600 {
		return 3600
	}
	return n
}

// WarnRatioValue normalizes warn_ratio: remaining/limit below this fraction
// raises a "quota low" warning (default 20%, clamped 5–95%).
func (q QuotaWatch) WarnRatioValue() float64 {
	r := q.WarnRatio
	if r <= 0 {
		return DefaultQuotaWarnRatio
	}
	if r < 0.05 {
		return 0.05
	}
	if r > 0.95 {
		return 0.95
	}
	return r
}

type Site struct {
	Code         string `toml:"-"`
	BaseURL      string `toml:"base_url" json:"base_url"`
	AJAXURL      string `toml:"ajax_url" json:"ajax_url"`
	SiteKey      string `toml:"sitekey" json:"sitekey"`
	VerifyAction string `toml:"verify_action" json:"verify_action"`
	Language     string `toml:"language" json:"language"`
	Enabled      bool   `toml:"enabled" json:"enabled"`
	// Proxies binds this site to the proxy pool: an ordered list of entry
	// names and/or "sub:<name>" references. Empty = global [proxy] routes.
	Proxies []string `toml:"proxies" json:"proxies"`
	// WarmPoolOnly keeps the cookie warmer on the pool routes bound via
	// Proxies; the global [proxy] primary/fallback lanes are not pre-solved
	// (they remain available for request-time failover). Default false.
	WarmPoolOnly bool `toml:"warm_pool_only" json:"warm_pool_only"`
}

// ProxyEntry is one manually configured pool node: an Xray share link
// (vless/vmess/trojan/ss) or a plain http/https/socks5 endpoint.
type ProxyEntry struct {
	Link    string `toml:"link" json:"link"`
	Enabled bool   `toml:"enabled" json:"enabled"`
}

// ProxySubscription is a remote node list refreshed on an interval.
type ProxySubscription struct {
	URL             string `toml:"url" json:"url"`
	Enabled         bool   `toml:"enabled" json:"enabled"`
	IntervalMinutes int    `toml:"interval_minutes" json:"interval_minutes"`
}

// ProxyPool configures the egress pool and its managed Xray core.
type ProxyPool struct {
	Enabled              bool                          `toml:"enabled" json:"enabled"`
	CheckIntervalSeconds int                           `toml:"check_interval_seconds" json:"check_interval_seconds"`
	CheckTimeoutSeconds  int                           `toml:"check_timeout_seconds" json:"check_timeout_seconds"`
	CheckURL             string                        `toml:"check_url" json:"check_url"`
	DefaultScheme        string                        `toml:"default_scheme" json:"default_scheme"`
	XrayPath             string                        `toml:"xray_path" json:"xray_path"`
	XrayVersion          string                        `toml:"xray_version" json:"xray_version"`
	XrayAutoDownload     bool                          `toml:"xray_auto_download" json:"xray_auto_download"`
	Entries              map[string]*ProxyEntry        `toml:"entries" json:"entries"`
	Subscriptions        map[string]*ProxySubscription `toml:"subscriptions" json:"subscriptions"`
}

type Model struct {
	ID         string `toml:"-"`
	Site       string `toml:"site" json:"site"`
	UpstreamID string `toml:"upstream_id" json:"upstream_id"`
	Label      string `toml:"label" json:"label"`
	PagePath   string `toml:"page_path" json:"page_path"`
	BotID      int    `toml:"bot_id" json:"bot_id"`
	PostID     int    `toml:"post_id" json:"post_id"`
	Enabled    bool   `toml:"enabled" json:"enabled"`
}

type Config struct {
	Server struct {
		Host     string `toml:"host" json:"host"`
		Port     int    `toml:"port" json:"port"`
		LogLevel string `toml:"log_level" json:"log_level"`
		// LogFile: "" = <data_dir>/logs/dsfree2api.log, "-" = stderr only.
		LogFile string `toml:"log_file" json:"log_file"`
	} `toml:"server" json:"server"`

	Security struct {
		APIKeys []string `toml:"api_keys" json:"api_keys"`
	} `toml:"security" json:"security"`

	Admin struct {
		Enabled  bool   `toml:"enabled" json:"enabled"`
		Host     string `toml:"host" json:"host"`
		Port     int    `toml:"port" json:"port"`
		Password string `toml:"password" json:"password"`
	} `toml:"admin" json:"admin"`

	Limits struct {
		MaxConcurrentPerSite int     `toml:"max_concurrent_per_site" json:"max_concurrent_per_site"`
		RatePerMinute        float64 `toml:"rate_per_minute" json:"rate_per_minute"`
	} `toml:"limits" json:"limits"`

	Proxy struct {
		URL              string   `toml:"url" json:"url"`
		FallbackURLs     []string `toml:"fallback_urls" json:"fallback_urls"`
		SlowStartSeconds float64  `toml:"slow_start_seconds" json:"slow_start_seconds"`
	} `toml:"proxy" json:"proxy"`

	ProxyPool ProxyPool `toml:"proxypool" json:"proxypool"`

	// Quota is the balance sentinel: it polls each site's guest-token balance
	// so the console shows how much daily free quota is left.
	Quota QuotaWatch `toml:"quota" json:"quota"`

	// Responses backs previous_response_id / GET /v1/responses/{id}.
	Responses ResponsesConfig `toml:"responses" json:"responses"`

	Upstream struct {
		Timeout             float64 `toml:"timeout" json:"timeout"`
		StreamTimeout       float64 `toml:"stream_timeout" json:"stream_timeout"`
		ConfigTTLSeconds    int     `toml:"config_ttl_seconds" json:"config_ttl_seconds"`
		AutoRefresh         bool    `toml:"auto_refresh" json:"auto_refresh"`
		RefreshRetries      int     `toml:"refresh_retries" json:"refresh_retries"`
		RetryBackoffSeconds float64 `toml:"retry_backoff_seconds" json:"retry_backoff_seconds"`
		UserAgent           string  `toml:"user_agent" json:"user_agent"`
		CrossSiteFailover   bool    `toml:"cross_site_failover" json:"cross_site_failover"`
		ContinueRounds      int     `toml:"continue_rounds" json:"continue_rounds"`
	} `toml:"upstream" json:"upstream"`

	Runtime struct {
		DataDir string `toml:"data_dir" json:"data_dir"`
	} `toml:"runtime" json:"runtime"`

	Turnstile Turnstile         `toml:"turnstile" json:"turnstile"`
	Sites     map[string]*Site  `toml:"sites" json:"sites"`
	Models    map[string]*Model `toml:"models" json:"models"`

	// ModelAliases maps an arbitrary client-supplied model name (e.g. the
	// gpt-* / claude-* names Codex CLI or Claude Code send) onto a configured
	// model id. Looked up case-insensitively before DefaultModel kicks in.
	ModelAliases map[string]string `toml:"model_aliases" json:"model_aliases"`
	// DefaultModel is the fallback when neither Models nor ModelAliases
	// matches a request. Empty disables the fallback (unknown model = 404).
	DefaultModel string `toml:"default_model" json:"default_model"`

	mu   sync.RWMutex `toml:"-"`
	path string       `toml:"-"`
}

// DefaultProxyPool returns the built-in pool settings (feature off, Xray
// auto-download on).
func DefaultProxyPool() ProxyPool {
	return ProxyPool{
		CheckIntervalSeconds: 120,
		CheckTimeoutSeconds:  10,
		CheckURL:             "https://www.gstatic.com/generate_204",
		DefaultScheme:        "http",
		XrayAutoDownload:     true,
		Entries:              map[string]*ProxyEntry{},
		Subscriptions:        map[string]*ProxySubscription{},
	}
}

func DefaultTurnstile() Turnstile {
	return Turnstile{
		Enabled:             false,
		Provider:            ProviderAPI,
		APIURL:              "",
		APIKey:              "",
		SiteKey:             "0x4AAAAAADlLZ3ljqZP6cQwq",
		Action:              "chat",
		TimeoutSeconds:      DefaultSolveTimeoutSeconds,
		CookieTTLSeconds:    10800,
		Retries:             5,
		RetryBackoffSeconds: 1.5,
		WarmRatio:           DefaultWarmRatio,
		WarmCheckSeconds:    DefaultWarmCheckSeconds,
	}
}

func DefaultSites(sitekey string) map[string]*Site {
	return map[string]*Site{
		"de": {
			Code: "de", BaseURL: "https://deepseek.de", AJAXURL: "https://deepseek.de/wp-admin/admin-ajax.php",
			SiteKey: sitekey, VerifyAction: "deepseek_ts_verify",
			Language: "de-DE,de;q=0.9,en-US;q=0.8,en;q=0.7", Enabled: true,
		},
		"es": {
			Code: "es", BaseURL: "https://deepseek.es", AJAXURL: "https://deepseek.es/wp-admin/admin-ajax.php",
			SiteKey: sitekey, VerifyAction: "deepseek_ts_verify",
			Language: "es-ES,es;q=0.9,en-US;q=0.8,en;q=0.7", Enabled: true,
		},
		"fr": {
			Code: "fr", BaseURL: "https://deepseek.fr", AJAXURL: "https://deepseek.fr/wp-admin/admin-ajax.php",
			SiteKey: sitekey, VerifyAction: "deepseek_ts_verify",
			Language: "fr-FR,fr;q=0.9,en-US;q=0.8,en;q=0.7", Enabled: true,
		},
	}
}

func DefaultModels() map[string]*Model {
	rows := [][7]any{
		{"deepseek-v4-flash-de", "de", "deepseek-v4-flash", "DeepSeek V4-Flash DE", "/", 27487, 106},
		{"deepseek-v4-pro-de", "de", "deepseek-v4-pro", "DeepSeek V4-Pro DE", "/pro/", 27533, 27177},
		{"deepseek-v4-flash-es", "es", "deepseek-v4-flash", "DeepSeek V4-Flash ES", "/", 27623, 27568},
		{"deepseek-v4-pro-es", "es", "deepseek-v4-pro", "DeepSeek V4-Pro ES", "/pro/", 27637, 27615},
		{"deepseek-v4-flash-fr", "fr", "deepseek-v4-flash", "DeepSeek V4-Flash FR", "/", 27645, 27569},
		{"deepseek-v4-pro-fr", "fr", "deepseek-v4-pro", "DeepSeek V4-Pro FR", "/pro/", 27647, 27616},
	}
	out := make(map[string]*Model, len(rows))
	for _, r := range rows {
		id := r[0].(string)
		out[id] = &Model{
			ID: id, Site: r[1].(string), UpstreamID: r[2].(string), Label: r[3].(string),
			PagePath: r[4].(string), BotID: r[5].(int), PostID: r[6].(int), Enabled: true,
		}
	}
	return out
}

func newConfig() *Config {
	c := &Config{}
	c.Server.Host = "0.0.0.0"
	c.Server.Port = 8000
	c.Server.LogLevel = "INFO"
	c.Admin.Enabled = true
	c.Admin.Host = "127.0.0.1"
	c.Admin.Port = 8001
	c.Limits.MaxConcurrentPerSite = 4
	c.Limits.RatePerMinute = 0
	c.Proxy.SlowStartSeconds = 8
	c.Upstream.Timeout = 60
	c.Upstream.StreamTimeout = 300
	c.Upstream.ConfigTTLSeconds = 60
	c.Upstream.AutoRefresh = true
	c.Upstream.RefreshRetries = 4
	c.Upstream.RetryBackoffSeconds = 1
	c.Upstream.UserAgent = DefaultUserAgent
	c.Upstream.CrossSiteFailover = true
	c.Upstream.ContinueRounds = 20
	c.Runtime.DataDir = "./data"
	c.ProxyPool = DefaultProxyPool()
	c.Turnstile = DefaultTurnstile()
	c.Responses.StoreCapacity = DefaultResponsesStoreCapacity
	c.Responses.StoreTTLSeconds = DefaultResponsesStoreTTLSeconds
	c.Sites = DefaultSites(c.Turnstile.SiteKey)
	c.Models = DefaultModels()
	return c
}

// Load reads the TOML config (if present), applies defaults, then env overrides.
func Load(path string) (*Config, error) {
	cfg := newConfig()
	if path == "" {
		path = "config.toml"
	}
	cfg.path = path

	var md toml.MetaData
	if raw, err := os.ReadFile(path); err == nil {
		md, err = toml.Decode(string(raw), cfg)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		cfg.applyDefaults(&md)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read %s: %w", path, err)
	} else {
		cfg.applyDefaults(nil)
	}

	if err := cfg.applyEnv(); err != nil {
		return nil, err
	}
	if cfg.Sites == nil {
		cfg.Sites = DefaultSites(cfg.Turnstile.SiteKey)
	}
	if cfg.Models == nil {
		cfg.Models = DefaultModels()
	}
	if err := cfg.Reindex(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Reindex restores derived fields (site codes / model ids) and validates the
// site-model wiring. It is safe to call after mutating the config at runtime.
func (c *Config) Reindex() error {
	if c.Sites == nil {
		c.Sites = DefaultSites(c.Turnstile.SiteKey)
	}
	if c.Models == nil {
		c.Models = DefaultModels()
	}
	for code, s := range c.Sites {
		if s == nil {
			return fmt.Errorf("site %q is null", code)
		}
		s.Code = code
		if s.SiteKey == "" {
			s.SiteKey = c.Turnstile.SiteKey
		}
		if s.VerifyAction == "" {
			s.VerifyAction = "deepseek_ts_verify"
		}
		if s.AJAXURL == "" {
			s.AJAXURL = strings.TrimRight(s.BaseURL, "/") + "/wp-admin/admin-ajax.php"
		}
		if s.BaseURL == "" {
			return fmt.Errorf("site %q: base_url is required", code)
		}
	}
	for id, m := range c.Models {
		if m == nil {
			return fmt.Errorf("model %q is null", id)
		}
		m.ID = id
		if m.Site == "" {
			return fmt.Errorf("model %q: site is required", id)
		}
		if _, ok := c.Sites[m.Site]; !ok {
			return fmt.Errorf("model %q: unknown site %q", id, m.Site)
		}
		if m.PagePath == "" {
			m.PagePath = "/"
		}
	}
	return nil
}

// applyDefaults fills booleans that were absent from the file (TOML decodes
// missing bools as false, but the project defaults are true).
func (c *Config) applyDefaults(md *toml.MetaData) {
	def := func(defined bool, dst *bool, want bool) {
		if !defined {
			*dst = want
		}
	}
	if md == nil {
		c.Turnstile.Enabled = true
		c.Upstream.AutoRefresh = true
		c.Upstream.ContinueRounds = 20
		c.ProxyPool.XrayAutoDownload = true
		for _, s := range c.Sites {
			s.Enabled = true
		}
		for _, m := range c.Models {
			m.Enabled = true
		}
		for _, e := range c.ProxyPool.Entries {
			e.Enabled = true
		}
		for _, sc := range c.ProxyPool.Subscriptions {
			sc.Enabled = true
		}
		return
	}
	def(md.IsDefined("turnstile", "enabled"), &c.Turnstile.Enabled, true)
	def(md.IsDefined("upstream", "auto_refresh"), &c.Upstream.AutoRefresh, true)
	def(md.IsDefined("proxypool", "xray_auto_download"), &c.ProxyPool.XrayAutoDownload, true)
	for code, s := range c.Sites {
		def(md.IsDefined("sites", code, "enabled"), &s.Enabled, true)
	}
	for id, m := range c.Models {
		def(md.IsDefined("models", id, "enabled"), &m.Enabled, true)
	}
	for name, e := range c.ProxyPool.Entries {
		def(md.IsDefined("proxypool", "entries", name, "enabled"), &e.Enabled, true)
	}
	for name, sc := range c.ProxyPool.Subscriptions {
		def(md.IsDefined("proxypool", "subscriptions", name, "enabled"), &sc.Enabled, true)
	}
}

func (c *Config) applyEnv() error {
	str := func(key string) *string {
		if v, ok := os.LookupEnv(key); ok {
			return &v
		}
		return nil
	}
	if v := str("HOST"); v != nil {
		c.Server.Host = *v
	}
	if v := str("LOG_LEVEL"); v != nil {
		c.Server.LogLevel = *v
	}
	if v := str("LOG_FILE"); v != nil {
		c.Server.LogFile = strings.TrimSpace(*v)
	}
	if v := str("PORT"); v != nil {
		n, err := strconv.Atoi(strings.TrimSpace(*v))
		if err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("invalid PORT %q (expected 1-65535)", *v)
		}
		c.Server.Port = n
	}
	// 只有"变量存在且非空"才覆盖配置文件：compose 的 `API_KEYS: ${API_KEYS:-}`
	// 会让变量恒存在，空值必须视为"不覆盖"，否则会把 config.toml 的 key 清空
	// 并静默关闭鉴权。
	if v := str("API_KEYS"); v != nil && strings.TrimSpace(*v) != "" {
		c.Security.APIKeys = splitList(*v)
	}
	if v := str("PROXY_URL"); v != nil && strings.TrimSpace(*v) != "" {
		c.Proxy.URL = strings.TrimSpace(*v)
	}
	if v := str("PROXY_FALLBACK_URLS"); v != nil && strings.TrimSpace(*v) != "" {
		c.Proxy.FallbackURLs = splitList(*v)
	}
	if v := str("PROXY_SLOW_START_SECONDS"); v != nil {
		f, err := strconv.ParseFloat(strings.TrimSpace(*v), 64)
		if err != nil || f < 0 {
			return fmt.Errorf("invalid PROXY_SLOW_START_SECONDS %q", *v)
		}
		c.Proxy.SlowStartSeconds = f
	}
	if v := str("TURNSTILE_API_KEY"); v != nil && strings.TrimSpace(*v) != "" {
		c.Turnstile.APIKey = strings.TrimSpace(*v)
	}
	if v := str("TURNSTILE_PROVIDER"); v != nil && strings.TrimSpace(*v) != "" {
		c.Turnstile.Provider = strings.ToLower(strings.TrimSpace(*v))
	}
	if v := str("TURNSTILE_BROWSER_PATH"); v != nil && strings.TrimSpace(*v) != "" {
		c.Turnstile.BrowserPath = strings.TrimSpace(*v)
	}
	if v := str("TURNSTILE_TIMEOUT_SECONDS"); v != nil {
		n, err := strconv.Atoi(strings.TrimSpace(*v))
		if err != nil || n < 1 {
			return fmt.Errorf("invalid TURNSTILE_TIMEOUT_SECONDS %q", *v)
		}
		c.Turnstile.TimeoutSeconds = n
	}
	if v := str("TURNSTILE_WARM_ENABLED"); v != nil {
		b, err := strconv.ParseBool(strings.TrimSpace(*v))
		if err != nil {
			return fmt.Errorf("invalid TURNSTILE_WARM_ENABLED %q", *v)
		}
		c.Turnstile.WarmEnabled = b
	}
	if v := str("TURNSTILE_WARM_RATIO"); v != nil {
		f, err := strconv.ParseFloat(strings.TrimSpace(*v), 64)
		if err != nil || f <= 0 || f > 1 {
			return fmt.Errorf("invalid TURNSTILE_WARM_RATIO %q (expected 0–1)", *v)
		}
		c.Turnstile.WarmRatio = f
	}
	if v := str("TURNSTILE_WARM_CHECK_SECONDS"); v != nil {
		n, err := strconv.Atoi(strings.TrimSpace(*v))
		if err != nil || n < 1 {
			return fmt.Errorf("invalid TURNSTILE_WARM_CHECK_SECONDS %q", *v)
		}
		c.Turnstile.WarmCheckSeconds = n
	}
	if v := str("TURNSTILE_ENABLED"); v != nil {
		b, err := strconv.ParseBool(strings.TrimSpace(*v))
		if err != nil {
			return fmt.Errorf("invalid TURNSTILE_ENABLED %q", *v)
		}
		c.Turnstile.Enabled = b
	}
	if v := str("ADMIN_PASSWORD"); v != nil {
		c.Admin.Password = *v
	}
	if v := str("ADMIN_HOST"); v != nil {
		c.Admin.Host = *v
	}
	if v := str("ADMIN_PORT"); v != nil {
		n, err := strconv.Atoi(strings.TrimSpace(*v))
		if err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("invalid ADMIN_PORT %q", *v)
		}
		c.Admin.Port = n
	}
	if v := str("DATA_DIR"); v != nil && strings.TrimSpace(*v) != "" {
		c.Runtime.DataDir = strings.TrimSpace(*v)
	}
	if v := str("ADMIN_ENABLED"); v != nil {
		b, err := strconv.ParseBool(strings.TrimSpace(*v))
		if err != nil {
			return fmt.Errorf("invalid ADMIN_ENABLED %q", *v)
		}
		c.Admin.Enabled = b
	}
	return nil
}

func splitList(v string) []string {
	out := []string{}
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Validate takes the write lock because it also normalizes fields
// (upstream.continue_rounds), so it must not run concurrently with a mutation.
func (c *Config) Validate() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.validateLocked()
}

func (c *Config) validateLocked() error {
	if c.Server.Port < 1 || c.Server.Port > 65535 {
		return fmt.Errorf("server.port out of range: %d", c.Server.Port)
	}
	if c.Admin.Enabled && (c.Admin.Port < 1 || c.Admin.Port > 65535) {
		return fmt.Errorf("admin.port out of range: %d", c.Admin.Port)
	}
	if c.Admin.Enabled && c.Admin.Port == c.Server.Port {
		return errors.New("admin.port must differ from server.port")
	}
	if c.Upstream.Timeout <= 0 {
		return errors.New("upstream.timeout must be > 0")
	}
	if c.Upstream.StreamTimeout <= 0 {
		return errors.New("upstream.stream_timeout must be > 0")
	}
	if c.Upstream.ContinueRounds < 0 {
		c.Upstream.ContinueRounds = 0
	}
	if err := c.validateTurnstile(); err != nil {
		return err
	}
	if len(c.Models) == 0 {
		return errors.New("no models configured")
	}
	seen := map[string]bool{}
	for _, k := range c.Security.APIKeys {
		if seen[k] {
			return fmt.Errorf("duplicate api key %q", k)
		}
		seen[k] = true
	}
	for alias, target := range c.ModelAliases {
		if alias == "" {
			return errors.New("model_aliases: alias name must not be empty")
		}
		m, ok := c.Models[target]
		if !ok {
			return fmt.Errorf("model_aliases[%q]: unknown model %q", alias, target)
		}
		if !m.Enabled {
			return fmt.Errorf("model_aliases[%q]: model %q is disabled", alias, target)
		}
	}
	if c.DefaultModel != "" {
		m, ok := c.Models[c.DefaultModel]
		if !ok {
			return fmt.Errorf("default_model: unknown model %q", c.DefaultModel)
		}
		if !m.Enabled {
			return fmt.Errorf("default_model: model %q is disabled", c.DefaultModel)
		}
	}
	return nil
}

// validateTurnstile checks the token provider settings while the solver is on.
// provider = "manual" needs no credentials — cookies are imported by hand.
func (c *Config) validateTurnstile() error {
	if !c.Turnstile.Enabled {
		return nil
	}
	switch c.Turnstile.ProviderValue() {
	case ProviderAPI:
		if c.Turnstile.APIURL == "" {
			return errors.New("turnstile.api_url is empty (set config to your solver endpoint, or use provider = \"manual\")")
		}
		// Local solve services (api_style = "ezsolver") usually need no key.
		if c.Turnstile.APIKey == "" && c.Turnstile.APIStyleValue() != "ezsolver" {
			return errors.New("turnstile.api_key is empty (set config, or TURNSTILE_API_KEY)")
		}
	case ProviderBrowser:
		path := strings.TrimSpace(c.Turnstile.BrowserPath)
		if path == "" {
			return errors.New("turnstile.browser_path is empty (path to Chrome/Edge, or use provider = \"manual\")")
		}
		if _, err := os.Stat(path); err != nil {
			return fmt.Errorf("turnstile.browser_path %q: %w", path, err)
		}
	case ProviderManual:
		// nothing to check — cookies arrive via the console import box
	default:
		return fmt.Errorf("turnstile.provider %q is invalid (api|browser|manual)", c.Turnstile.Provider)
	}
	return nil
}

func (c *Config) RLock()   { c.mu.RLock() }
func (c *Config) RUnlock() { c.mu.RUnlock() }
func (c *Config) Lock()    { c.mu.Lock() }
func (c *Config) Unlock()  { c.mu.Unlock() }

func (c *Config) Path() string { return c.path }

// Site returns a site by code (must hold at least a read lock).
func (c *Config) Site(code string) (*Site, bool) {
	s, ok := c.Sites[code]
	return s, ok
}

// Model returns a model by id (must hold at least a read lock).
func (c *Config) Model(id string) (*Model, bool) {
	m, ok := c.Models[id]
	return m, ok
}

// ResolveModel maps a client-supplied model name onto a configured model.
// Resolution order: exact id → case-insensitive id → model_aliases
// (case-insensitive) → DefaultModel fallback. Must hold at least a read lock.
//
// An exact id that exists but is disabled is never aliased away — the admin
// turned it off on purpose, so the caller gets ok=false (404). ok=false means
// the name is genuinely unknown and the caller should reject the request.
// resolved is the configured id the caller must use for upstream routing and
// metrics; it equals the requested name when no aliasing happened.
func (c *Config) ResolveModel(id string) (m *Model, resolved string, ok bool) {
	if m, hit := c.Models[id]; hit {
		if m.Enabled {
			return m, id, true
		}
		return nil, "", false
	}
	for candidate, m := range c.Models {
		if strings.EqualFold(candidate, id) && m.Enabled {
			return m, candidate, true
		}
	}
	for alias, target := range c.ModelAliases {
		if strings.EqualFold(alias, id) {
			if m, hit := c.Models[target]; hit && m.Enabled {
				return m, target, true
			}
			return nil, "", false
		}
	}
	if c.DefaultModel != "" {
		if m, hit := c.Models[c.DefaultModel]; hit && m.Enabled {
			return m, c.DefaultModel, true
		}
	}
	return nil, "", false
}

// Save writes the config back to its file after taking a timestamped backup.
func (c *Config) Save() error {
	return c.SaveTo(c.path)
}

// SaveTo encodes the config with a read lock held: toml.Encode iterates the
// Sites/Models/... maps, which admin actions mutate under the write lock, and
// an unlocked iteration racing a map write is a fatal (unrecoverable) error.
func (c *Config) SaveTo(path string) error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.saveToLocked(path)
}

func (c *Config) saveToLocked(path string) error {
	if path == "" {
		return errors.New("config path is empty")
	}
	if _, err := os.Stat(path); err == nil {
		backup := path + ".bak"
		if raw, err := os.ReadFile(path); err == nil {
			// .bak 里同样是 api_keys / admin password，收紧到 0600
			_ = os.WriteFile(backup, raw, 0o600)
		}
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0o755)
	}
	var sb strings.Builder
	enc := toml.NewEncoder(&sb)
	enc.Indent = ""
	if err := enc.Encode(c); err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	header := "# dsfree2api configuration\n# Managed by the web console; a .bak copy is kept next to this file.\n\n"
	data := []byte(header + sb.String())

	// 原子替换：先写同目录临时文件再 rename。裸 WriteFile 会先把目标截断，
	// 写一半时进程崩溃就留下损坏的 config.toml（下次启动直接 config error），
	// 并发读者也会读到空/半截内容。
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write config temp: %w", err)
	}
	if err := renameReplace(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("replace config: %w", err)
	}
	return nil
}

// renameReplace 把 src 重命名覆盖到 dst。Windows 上目标文件被并发读者以
// 不带 FILE_SHARE_DELETE 的方式打开时 MoveFileEx 会报 sharing violation，
// 而读者都是瞬间的 ReadFile，退避重试即可等到空档；其他平台首次即成功。
func renameReplace(src, dst string) error {
	var err error
	for i := 0; i < 100; i++ {
		if err = os.Rename(src, dst); err == nil {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return err
}

// ReplaceJSON applies a console payload in one shot: decode into the live
// config only after the payload itself is known to be well-formed, and roll
// back to the pre-request snapshot when reindexing or validation rejects it,
// so a failed PUT never leaves half-applied state behind in memory.
func (c *Config) ReplaceJSON(raw []byte) error {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	snapshot, err := json.Marshal(c)
	if err != nil {
		return err
	}
	// json 解码对 map 是"合并"语义，待替换的键必须先清空才能真正替换
	if _, ok := probe["sites"]; ok {
		c.Sites = nil
	}
	if _, ok := probe["models"]; ok {
		c.Models = nil
	}
	if _, ok := probe["proxypool"]; ok {
		c.ProxyPool.Entries = nil
		c.ProxyPool.Subscriptions = nil
	}
	curPassword, curTSKey := c.Admin.Password, c.Turnstile.APIKey
	if err := json.Unmarshal(raw, c); err != nil {
		c.restoreLocked(snapshot)
		return err
	}
	// 控制台 GET 会把这两个密钥抹成空串再原样回传：空值 = 保持不变
	if c.Admin.Password == "" {
		c.Admin.Password = curPassword
	}
	if strings.TrimSpace(c.Turnstile.APIKey) == "" {
		c.Turnstile.APIKey = curTSKey
	}
	if err := c.Reindex(); err != nil {
		c.restoreLocked(snapshot)
		return err
	}
	if err := c.validateLocked(); err != nil {
		c.restoreLocked(snapshot)
		return err
	}
	return nil
}

// restoreLocked puts back a JSON snapshot taken under the same lock.
func (c *Config) restoreLocked(snapshot []byte) {
	c.Sites, c.Models = nil, nil
	c.ProxyPool.Entries, c.ProxyPool.Subscriptions = nil, nil
	_ = json.Unmarshal(snapshot, c)
}

// DataDir resolves (and creates) the runtime data directory.
func (c *Config) DataDir() (string, error) {
	dir := c.Runtime.DataDir
	if dir == "" {
		dir = "./data"
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

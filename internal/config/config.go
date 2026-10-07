package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/BurntSushi/toml"
)

const DefaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36"

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
	TimeoutSeconds      int     `toml:"timeout_seconds" json:"timeout_seconds"`
	CookieTTLSeconds    int     `toml:"cookie_ttl_seconds" json:"cookie_ttl_seconds"`
	Retries             int     `toml:"retries" json:"retries"`
	RetryBackoffSeconds float64 `toml:"retry_backoff_seconds" json:"retry_backoff_seconds"`

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

type Site struct {
	Code         string `toml:"-"`
	BaseURL      string `toml:"base_url" json:"base_url"`
	AJAXURL      string `toml:"ajax_url" json:"ajax_url"`
	SiteKey      string `toml:"sitekey" json:"sitekey"`
	VerifyAction string `toml:"verify_action" json:"verify_action"`
	Language     string `toml:"language" json:"language"`
	Enabled      bool   `toml:"enabled" json:"enabled"`
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

	Upstream struct {
		Timeout             float64 `toml:"timeout" json:"timeout"`
		StreamTimeout       float64 `toml:"stream_timeout" json:"stream_timeout"`
		ConfigTTLSeconds    int     `toml:"config_ttl_seconds" json:"config_ttl_seconds"`
		AutoRefresh         bool    `toml:"auto_refresh" json:"auto_refresh"`
		RefreshRetries      int     `toml:"refresh_retries" json:"refresh_retries"`
		RetryBackoffSeconds float64 `toml:"retry_backoff_seconds" json:"retry_backoff_seconds"`
		UserAgent           string  `toml:"user_agent" json:"user_agent"`
		CrossSiteFailover   bool    `toml:"cross_site_failover" json:"cross_site_failover"`
	} `toml:"upstream" json:"upstream"`

	Runtime struct {
		DataDir string `toml:"data_dir" json:"data_dir"`
	} `toml:"runtime" json:"runtime"`

	Turnstile Turnstile         `toml:"turnstile" json:"turnstile"`
	Sites     map[string]*Site  `toml:"sites" json:"sites"`
	Models    map[string]*Model `toml:"models" json:"models"`

	mu   sync.RWMutex `toml:"-"`
	path string       `toml:"-"`
}

func DefaultTurnstile() Turnstile {
	return Turnstile{
		Enabled:             false,
		Provider:            ProviderAPI,
		APIURL:              "",
		APIKey:              "",
		SiteKey:             "0x4AAAAAADlLZ3ljqZP6cQwq",
		Action:              "chat",
		TimeoutSeconds:      90,
		CookieTTLSeconds:    10800,
		Retries:             5,
		RetryBackoffSeconds: 1.5,
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
	c.Runtime.DataDir = "./data"
	c.Turnstile = DefaultTurnstile()
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
		for _, s := range c.Sites {
			s.Enabled = true
		}
		for _, m := range c.Models {
			m.Enabled = true
		}
		return
	}
	def(md.IsDefined("turnstile", "enabled"), &c.Turnstile.Enabled, true)
	def(md.IsDefined("upstream", "auto_refresh"), &c.Upstream.AutoRefresh, true)
	for code, s := range c.Sites {
		def(md.IsDefined("sites", code, "enabled"), &s.Enabled, true)
	}
	for id, m := range c.Models {
		def(md.IsDefined("models", id, "enabled"), &m.Enabled, true)
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
	if v := str("PORT"); v != nil {
		n, err := strconv.Atoi(strings.TrimSpace(*v))
		if err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("invalid PORT %q (expected 1-65535)", *v)
		}
		c.Server.Port = n
	}
	if v := str("API_KEYS"); v != nil {
		c.Security.APIKeys = splitList(*v)
	}
	if v := str("PROXY_URL"); v != nil {
		c.Proxy.URL = strings.TrimSpace(*v)
	}
	if v := str("PROXY_FALLBACK_URLS"); v != nil {
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

func (c *Config) Validate() error {
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
		if c.Turnstile.APIKey == "" {
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

// Save writes the config back to its file after taking a timestamped backup.
func (c *Config) Save() error {
	return c.SaveTo(c.path)
}

func (c *Config) SaveTo(path string) error {
	if path == "" {
		return errors.New("config path is empty")
	}
	if _, err := os.Stat(path); err == nil {
		backup := path + ".bak"
		if raw, err := os.ReadFile(path); err == nil {
			_ = os.WriteFile(backup, raw, 0o644)
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
	return os.WriteFile(path, []byte(header+sb.String()), 0o644)
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

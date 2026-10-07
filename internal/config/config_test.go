package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const examplePath = "../../config.example.toml"

func TestLoadSixModelsFromExample(t *testing.T) {
	cfg, err := Load(examplePath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.Models) != 6 {
		t.Fatalf("want 6 models, got %d", len(cfg.Models))
	}
	for _, id := range []string{"deepseek-v4-flash-de", "deepseek-v4-pro-fr", "deepseek-v4-pro-es"} {
		if _, ok := cfg.Models[id]; !ok {
			t.Errorf("missing model %q", id)
		}
	}
	if cfg.Sites["de"].SiteKey != "0x4AAAAAADlLZ3ljqZP6cQwq" {
		t.Errorf("unexpected de sitekey %q", cfg.Sites["de"].SiteKey)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("validate: %v", err)
	}
}

func TestSecurityKeyConfigured(t *testing.T) {
	cfg, err := Load(examplePath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.Security.APIKeys) != 1 || cfg.Security.APIKeys[0] != "sk-dsfr-local-change-me" {
		t.Fatalf("unexpected api_keys %v", cfg.Security.APIKeys)
	}
}

func TestProxyFallbackEnvConfig(t *testing.T) {
	t.Setenv("PROXY_URL", "")
	t.Setenv("PROXY_FALLBACK_URLS", "socks5://one:1, socks5://two:2")
	t.Setenv("PROXY_SLOW_START_SECONDS", "5")
	cfg, err := Load(examplePath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Proxy.URL != "" {
		t.Errorf("proxy url = %q, want empty", cfg.Proxy.URL)
	}
	want := []string{"socks5://one:1", "socks5://two:2"}
	if len(cfg.Proxy.FallbackURLs) != len(want) {
		t.Fatalf("fallbacks = %v", cfg.Proxy.FallbackURLs)
	}
	for i, u := range want {
		if cfg.Proxy.FallbackURLs[i] != u {
			t.Errorf("fallback[%d] = %q, want %q", i, cfg.Proxy.FallbackURLs[i], u)
		}
	}
	if cfg.Proxy.SlowStartSeconds != 5 {
		t.Errorf("slow_start = %v, want 5", cfg.Proxy.SlowStartSeconds)
	}
}

func TestServerEnvOverrides(t *testing.T) {
	t.Setenv("HOST", "127.0.0.1")
	t.Setenv("PORT", "9999")
	t.Setenv("API_KEYS", "sk-one, sk-two")
	cfg, err := Load(examplePath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Server.Host != "127.0.0.1" {
		t.Errorf("host = %q", cfg.Server.Host)
	}
	if cfg.Server.Port != 9999 {
		t.Errorf("port = %d", cfg.Server.Port)
	}
	if len(cfg.Security.APIKeys) != 2 || cfg.Security.APIKeys[1] != "sk-two" {
		t.Errorf("api_keys = %v", cfg.Security.APIKeys)
	}
}

func TestValidateRejectsBadValues(t *testing.T) {
	cfg, err := Load(examplePath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	cfg.Turnstile.Enabled = true
	cfg.Turnstile.APIURL = ""
	cfg.Turnstile.APIKey = "k"
	if err := cfg.Validate(); err == nil {
		t.Error("expected error for empty turnstile api_url")
	}
	cfg.Turnstile.APIURL = "https://solver.example.com/turnstile/sync"
	cfg.Turnstile.APIKey = ""
	if err := cfg.Validate(); err == nil {
		t.Error("expected error for empty turnstile api_key")
	}
	cfg.Turnstile.APIKey = "k"
	cfg.Turnstile.Enabled = false
	cfg.Admin.Port = cfg.Server.Port
	if err := cfg.Validate(); err == nil {
		t.Error("expected error for admin.port == server.port")
	}
}

func TestTurnstileDisabledByDefault(t *testing.T) {
	if DefaultTurnstile().Enabled {
		t.Error("turnstile should be disabled by default")
	}
	if DefaultTurnstile().APIURL != "" || DefaultTurnstile().APIKey != "" {
		t.Error("no solver endpoint/key should be baked in by default")
	}
	cfg, err := Load(examplePath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Turnstile.Enabled {
		t.Error("example config should ship with turnstile disabled")
	}
	t.Setenv("TURNSTILE_ENABLED", "true")
	cfg, err = Load(examplePath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !cfg.Turnstile.Enabled {
		t.Error("TURNSTILE_ENABLED=true should enable the solver")
	}
}

func TestTurnstileProviderValidation(t *testing.T) {
	load := func(t *testing.T) *Config {
		t.Helper()
		cfg, err := Load(examplePath)
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		cfg.Turnstile.Enabled = true
		return cfg
	}

	// provider unset behaves like "api": credentials are mandatory
	cfg := load(t)
	if p := cfg.Turnstile.ProviderValue(); p != ProviderAPI {
		t.Fatalf("ProviderValue() = %q, want api", p)
	}
	if err := cfg.Validate(); err == nil {
		t.Error("api provider without api_url should fail")
	}
	cfg.Turnstile.APIURL = "https://solver.example.com/turnstile/sync"
	cfg.Turnstile.APIKey = "k"
	if err := cfg.Validate(); err != nil {
		t.Errorf("api provider configured: %v", err)
	}

	// browser provider needs an existing executable instead of credentials
	cfg = load(t)
	cfg.Turnstile.Provider = ProviderBrowser
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "browser_path") {
		t.Errorf("browser provider without browser_path: %v", err)
	}
	cfg.Turnstile.BrowserPath = filepath.Join(t.TempDir(), "msedge.exe")
	if err := cfg.Validate(); err == nil {
		t.Error("browser_path pointing at a missing file should fail")
	}
	browser := filepath.Join(t.TempDir(), "msedge.exe")
	if err := os.WriteFile(browser, []byte("stub"), 0o755); err != nil {
		t.Fatalf("write stub browser: %v", err)
	}
	cfg.Turnstile.BrowserPath = browser
	if err := cfg.Validate(); err != nil {
		t.Errorf("browser provider configured: %v", err)
	}

	// manual provider never validates credentials
	cfg = load(t)
	cfg.Turnstile.Provider = ProviderManual
	if err := cfg.Validate(); err != nil {
		t.Errorf("manual provider: %v", err)
	}

	cfg.Turnstile.Provider = "captcha"
	if err := cfg.Validate(); err == nil {
		t.Error("unknown provider should fail")
	}
}

func TestTurnstileProviderEnv(t *testing.T) {
	t.Setenv("TURNSTILE_PROVIDER", " Manual ")
	t.Setenv("TURNSTILE_BROWSER_PATH", `C:\browsers\chrome.exe`)
	cfg, err := Load(examplePath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Turnstile.ProviderValue() != ProviderManual {
		t.Errorf("provider = %q, want manual", cfg.Turnstile.Provider)
	}
	if cfg.Turnstile.BrowserPath != `C:\browsers\chrome.exe` {
		t.Errorf("browser_path = %q", cfg.Turnstile.BrowserPath)
	}
}

func TestSaveRoundTrip(t *testing.T) {
	cfg, err := Load(examplePath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	cfg.Security.APIKeys = []string{"sk-rotated"}
	cfg.Server.Port = 8123
	dst := filepath.Join(t.TempDir(), "config.toml")
	if err := cfg.SaveTo(dst); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := Load(dst)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got.Server.Port != 8123 {
		t.Errorf("port = %d", got.Server.Port)
	}
	if len(got.Security.APIKeys) != 1 || got.Security.APIKeys[0] != "sk-rotated" {
		t.Errorf("api_keys = %v", got.Security.APIKeys)
	}
	if len(got.Models) != 6 {
		t.Errorf("models = %d", len(got.Models))
	}
	for id, m := range got.Models {
		if m.ID != id {
			t.Errorf("model %q has id %q", id, m.ID)
		}
	}
}

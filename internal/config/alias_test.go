package config

import (
	"strings"
	"testing"
)

func aliasFixture(t *testing.T) *Config {
	t.Helper()
	cfg, err := Load(examplePath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	cfg.ModelAliases = map[string]string{
		"gpt-5":             "deepseek-v4-flash-de",
		"Claude-Sonnet-4-5": "deepseek-v4-pro-de",
	}
	cfg.DefaultModel = "deepseek-v4-flash-es"
	return cfg
}

func TestResolveModelExactMatch(t *testing.T) {
	cfg := aliasFixture(t)
	m, resolved, ok := cfg.ResolveModel("deepseek-v4-pro-fr")
	if !ok {
		t.Fatal("exact id must resolve")
	}
	if resolved != "deepseek-v4-pro-fr" || m.Site != "fr" {
		t.Fatalf("got (%q, site=%q)", resolved, m.Site)
	}
}

func TestResolveModelAlias(t *testing.T) {
	cfg := aliasFixture(t)
	for _, want := range []string{"gpt-5", "GPT-5", "claude-sonnet-4-5"} {
		m, resolved, ok := cfg.ResolveModel(want)
		if !ok {
			t.Fatalf("alias %q must resolve", want)
		}
		if resolved == want {
			t.Fatalf("alias %q must be rewritten to a configured id, got %q", want, resolved)
		}
		if m == nil || !m.Enabled {
			t.Fatalf("alias %q resolved to a bad model: %+v", want, m)
		}
	}
}

func TestResolveModelDefaultFallback(t *testing.T) {
	cfg := aliasFixture(t)
	m, resolved, ok := cfg.ResolveModel("totally-unknown-model")
	if !ok {
		t.Fatal("unknown model must fall back to default_model")
	}
	if resolved != "deepseek-v4-flash-es" {
		t.Fatalf("want default model, got %q", resolved)
	}
	if m.Site != "es" {
		t.Fatalf("want default model site es, got %q", m.Site)
	}
}

func TestResolveModelNoFallbackWhenUnconfigured(t *testing.T) {
	cfg := aliasFixture(t)
	cfg.DefaultModel = ""
	if _, _, ok := cfg.ResolveModel("totally-unknown-model"); ok {
		t.Fatal("without default_model an unknown model must not resolve")
	}
}

func TestResolveModelDisabledExactIdIsNotAliased(t *testing.T) {
	cfg := aliasFixture(t)
	cfg.Models["deepseek-v4-pro-fr"].Enabled = false
	if _, _, ok := cfg.ResolveModel("deepseek-v4-pro-fr"); ok {
		t.Fatal("a disabled model must stay 404 instead of falling back")
	}
}

func TestValidateRejectsBadModelAliases(t *testing.T) {
	cases := []struct {
		name  string
		mut   func(*Config)
		match string
	}{
		{"alias target missing", func(c *Config) { c.ModelAliases["x"] = "no-such-model" }, "unknown model"},
		{"alias target disabled", func(c *Config) {
			c.Models["deepseek-v4-pro-fr"].Enabled = false
			c.ModelAliases["x"] = "deepseek-v4-pro-fr"
		}, "disabled"},
		{"empty alias name", func(c *Config) { c.ModelAliases[""] = "deepseek-v4-pro-fr" }, "must not be empty"},
		{"default missing", func(c *Config) { c.DefaultModel = "no-such-model" }, "default_model"},
		{"default disabled", func(c *Config) {
			c.Models["deepseek-v4-pro-fr"].Enabled = false
			c.DefaultModel = "deepseek-v4-pro-fr"
		}, "default_model"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := aliasFixture(t)
			tc.mut(cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatal("want validation error")
			}
			if !strings.Contains(err.Error(), tc.match) {
				t.Fatalf("error %q does not mention %q", err, tc.match)
			}
		})
	}
}

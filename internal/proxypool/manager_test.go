package proxypool

import (
	"testing"

	"github.com/nyoungo/dsfree2api/internal/config"
)

const examplePath = "../../config.example.toml"

func TestCandidatesStickySwitchOnFailure(t *testing.T) {
	cfg, err := config.Load(examplePath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg.ProxyPool.Enabled = true
	cfg.ProxyPool.Entries = map[string]*config.ProxyEntry{
		"a": {Link: "http://127.0.0.1:9", Enabled: true},
		"b": {Link: "socks5://127.0.0.1:10", Enabled: true},
	}
	cfg.Sites["de"].Proxies = []string{"a", "b"}

	m := New(cfg, t.TempDir(), nil)
	m.Reload()

	cands := m.Candidates("de")
	if len(cands) != 2 || cands[0].Name != "a" || cands[0].Proxy != "http://127.0.0.1:9" {
		t.Fatalf("candidates = %+v", cands)
	}

	// Both healthy: the sticky pick stays a.
	m.mu.Lock()
	for _, ep := range m.endpoints {
		ep.checked = true
		ep.healthy = true
	}
	m.mu.Unlock()
	cands = m.Candidates("de")
	if cands[0].Name != "a" {
		t.Fatalf("sticky pick changed without failure: %+v", cands)
	}

	// a fails: the request path must immediately switch to b.
	m.Report("http://127.0.0.1:9", false)
	cands = m.Candidates("de")
	if len(cands) != 1 || cands[0].Name != "b" {
		t.Fatalf("after failure = %+v", cands)
	}

	// Everything unhealthy: fall back to the bound order so requests still try.
	m.Report("socks5://127.0.0.1:10", false)
	cands = m.Candidates("de")
	if len(cands) != 2 || cands[0].Name != "a" {
		t.Fatalf("all-unhealthy fallback = %+v", cands)
	}
}

func TestCandidatesDisabledOrUnbound(t *testing.T) {
	cfg, err := config.Load(examplePath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg.ProxyPool.Enabled = false
	cfg.Sites["de"].Proxies = []string{"a"}
	m := New(cfg, t.TempDir(), nil)
	m.Reload()
	if got := m.Candidates("de"); got != nil {
		t.Fatalf("disabled pool returned %+v", got)
	}

	cfg.ProxyPool.Enabled = true
	m.Reload()
	if got := m.Candidates("de"); got != nil {
		t.Fatalf("unbound/unknown entry returned %+v", got)
	}
}

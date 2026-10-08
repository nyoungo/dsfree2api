package admin

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nyoungo/dsfree2api/internal/config"
	"github.com/nyoungo/dsfree2api/internal/turnstile"
)

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	raw, err := os.ReadFile("../../config.example.toml")
	if err != nil {
		t.Fatalf("read example config: %v", err)
	}
	dst := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(dst, raw, 0o644); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	cfg, err := config.Load(dst)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	return cfg
}

func newActionServer(t *testing.T, cfg *config.Config) *Server {
	t.Helper()
	return New(cfg, nil, nil, nil, turnstile.New(cfg.Turnstile, cfg.Upstream.UserAgent), nil, nil, nil)
}

func postAction(t *testing.T, s *Server, payload string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/actions", strings.NewReader(payload))
	w := httptest.NewRecorder()
	s.handleActions(w, req)
	return w
}

// 回归：set_limits 是部分更新 —— payload 里没带的字段必须保持原值。旧实现把
// max_concurrent/rate_per_minute 当零值字段，缺字段就把它们写成 0：
// 并发闸门（max<=0 = 不限）和按 Key 限流（limit<=0 = 不限）会被静默关掉，
// slow_start 同样被清零。
func TestSetLimitsPartialPayloadKeepsExistingValues(t *testing.T) {
	cfg := testConfig(t)
	cfg.Limits.MaxConcurrentPerSite = 4
	cfg.Limits.RatePerMinute = 7
	cfg.Proxy.SlowStartSeconds = 8
	s := newActionServer(t, cfg)

	w := postAction(t, s, `{"action":"set_limits","cross_site_failover":false}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	cfg.RLock()
	got := struct {
		MaxConcurrent int
		Rate          float64
		SlowStart     float64
		CrossSite     bool
	}{cfg.Limits.MaxConcurrentPerSite, cfg.Limits.RatePerMinute, cfg.Proxy.SlowStartSeconds, cfg.Upstream.CrossSiteFailover}
	cfg.RUnlock()

	if got.MaxConcurrent != 4 {
		t.Errorf("max_concurrent = %d, want 4（缺字段被清零会让并发闸门失效）", got.MaxConcurrent)
	}
	if got.Rate != 7 {
		t.Errorf("rate_per_minute = %v, want 7", got.Rate)
	}
	if got.SlowStart != 8 {
		t.Errorf("slow_start = %v, want 8", got.SlowStart)
	}
	if got.CrossSite {
		t.Errorf("cross_site_failover = true, want false（payload 里的值要生效）")
	}
}

// 回归：set_turnstile 同样是部分更新 —— 不带 enabled 不能把求解器关掉。
func TestSetTurnstilePartialPayloadKeepsEnabled(t *testing.T) {
	cfg := testConfig(t)
	cfg.Turnstile.Enabled = true
	cfg.Turnstile.Provider = config.ProviderAPI
	cfg.Turnstile.APIURL = "https://solve.example.invalid/"
	cfg.Turnstile.Retries = 5
	s := newActionServer(t, cfg)

	w := postAction(t, s, `{"action":"set_turnstile","retries":3}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if !cfg.Turnstile.Enabled {
		t.Errorf("turnstile.enabled 被部分 payload 关掉了")
	}
	if cfg.Turnstile.Retries != 3 {
		t.Errorf("retries = %d, want 3", cfg.Turnstile.Retries)
	}
}

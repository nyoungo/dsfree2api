package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nyoungo/dsfree2api/internal/config"
	"github.com/nyoungo/dsfree2api/internal/httpx"
	"github.com/nyoungo/dsfree2api/internal/turnstile"
)

const examplePath = "../../config.example.toml"

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Load(examplePath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg.Upstream.AutoRefresh = false
	cfg.Upstream.RefreshRetries = 0
	return cfg
}

func TestRoutesKeepPrimaryFirstAndDedupe(t *testing.T) {
	cfg := testConfig(t)
	cfg.Proxy.URL = ""
	cfg.Proxy.FallbackURLs = []string{"socks5://fallback:1", "socks5://fallback:1"}

	c := New(cfg, nil, nil, nil)
	got := c.Routes()
	want := []Route{{Name: "primary", Proxy: ""}, {Name: "fallback-1", Proxy: "socks5://fallback:1"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("routes = %+v, want %+v", got, want)
	}
}

func TestSlowPrimarySwitchesToFallbackBeforeContent(t *testing.T) {
	cfg := testConfig(t)
	cfg.Proxy.URL = ""
	cfg.Proxy.FallbackURLs = []string{"socks5://fallback:1"}
	cfg.Proxy.SlowStartSeconds = 0.02

	var calls []string
	c := New(cfg, nil, nil, nil)
	c.chatOnceOverride = func(ctx context.Context, site config.Site, modelID string, model config.Model,
		prompt string, route Route, info *ServeInfo, yield func(Event) error) error {
		calls = append(calls, route.Name)
		if route.Name == "primary" {
			select {
			case <-time.After(5 * time.Second):
			case <-ctx.Done():
				return ctx.Err()
			}
			yield(Event{Kind: KindDelta, Value: "primary"})
			return nil
		}
		yield(Event{Kind: KindDelta, Value: "fallback"})
		return nil
	}

	var got []string
	if err := c.Chat(context.Background(), "deepseek-v4-flash-de", "hello", &ServeInfo{},
		func(ev Event) error {
			got = append(got, ev.Value)
			return nil
		}); err != nil {
		t.Fatalf("chat: %v", err)
	}
	if want := []string{"primary", "fallback-1"}; !reflect.DeepEqual(calls, want) {
		t.Errorf("calls = %v, want %v", calls, want)
	}
	if want := []string{"fallback"}; !reflect.DeepEqual(got, want) {
		t.Errorf("events = %v, want %v", got, want)
	}
}

func TestPrimaryErrorSwitchesToFallbackBeforeContent(t *testing.T) {
	cfg := testConfig(t)
	cfg.Proxy.URL = ""
	cfg.Proxy.FallbackURLs = []string{"socks5://fallback:1"}

	var calls []string
	c := New(cfg, nil, nil, nil)
	c.chatOnceOverride = func(ctx context.Context, site config.Site, modelID string, model config.Model,
		prompt string, route Route, info *ServeInfo, yield func(Event) error) error {
		calls = append(calls, route.Name)
		if route.Name == "primary" {
			return errors.New("primary failed")
		}
		yield(Event{Kind: KindDelta, Value: "fallback"})
		return nil
	}

	var got []string
	if err := c.Chat(context.Background(), "deepseek-v4-flash-de", "hello", &ServeInfo{},
		func(ev Event) error {
			got = append(got, ev.Value)
			return nil
		}); err != nil {
		t.Fatalf("chat: %v", err)
	}
	if want := []string{"primary", "fallback-1"}; !reflect.DeepEqual(calls, want) {
		t.Errorf("calls = %v, want %v", calls, want)
	}
	if want := []string{"fallback"}; !reflect.DeepEqual(got, want) {
		t.Errorf("events = %v, want %v", got, want)
	}
}

func TestNoFallbackAfterContentStarted(t *testing.T) {
	cfg := testConfig(t)
	cfg.Proxy.URL = ""
	cfg.Proxy.FallbackURLs = []string{"socks5://fallback:1"}

	var calls []string
	c := New(cfg, nil, nil, nil)
	c.chatOnceOverride = func(ctx context.Context, site config.Site, modelID string, model config.Model,
		prompt string, route Route, info *ServeInfo, yield func(Event) error) error {
		calls = append(calls, route.Name)
		yield(Event{Kind: KindDelta, Value: "partial"})
		return errors.New("stream failed")
	}

	err := c.Chat(context.Background(), "deepseek-v4-flash-de", "hello", &ServeInfo{},
		func(Event) error { return nil })
	if err == nil {
		t.Fatal("expected an error")
	}
	var ue *UpstreamError
	if !errors.As(err, &ue) {
		t.Fatalf("error type = %T, want *UpstreamError", err)
	}
	if !ue.ContentStarted {
		t.Error("expected ContentStarted to be set")
	}
	if want := []string{"primary"}; !reflect.DeepEqual(calls, want) {
		t.Errorf("calls = %v, want %v", calls, want)
	}
}

func TestQuotaObserverFiresOncePerSiteBeforeFailover(t *testing.T) {
	cfg := testConfig(t)
	cfg.Upstream.AutoRefresh = false
	c := New(cfg, nil, nil, nil)
	c.chatOnceOverride = func(ctx context.Context, site config.Site, modelID string, model config.Model,
		prompt string, route Route, info *ServeInfo, yield func(Event) error) error {
		if site.Code == "de" {
			return newQuota(`sse quota exhausted: {"quota_notice":{}}`)
		}
		yield(Event{Kind: KindDelta, Value: "ok"})
		return nil
	}
	var fired []string
	c.SetQuotaObserver(func(site, model, route string, err error) {
		fired = append(fired, site+"|"+model+"|"+route)
	})

	if err := c.Chat(context.Background(), "deepseek-v4-flash-de", "hi", &ServeInfo{},
		func(ev Event) error { return nil }); err != nil {
		t.Fatalf("chat should succeed via cross-site failover: %v", err)
	}
	if len(fired) != 1 || fired[0] != "de|deepseek-v4-flash-de|primary" {
		t.Fatalf("quota observer events = %v, want [de|deepseek-v4-flash-de|primary]", fired)
	}
}

func TestQuotaNoticeTriggersQuotaError(t *testing.T) {
	payload := map[string]any{
		"error": "Dein heutiges Guthaben von 10.000 Token (V4-Pro) ist aufgebraucht.",
		"quota_notice": map[string]any{
			"type":    "quota_notice",
			"message": "Dein heutiges Guthaben von 10.000 Token (V4-Pro) ist aufgebraucht.",
		},
	}
	raw, _ := json.Marshal(payload)
	_, err := translateEvent("error", []string{string(raw)})
	if err == nil {
		t.Fatal("expected error")
	}
	if !isQuota(err) {
		t.Fatalf("want QuotaExhaustedError, got %T: %v", err, err)
	}
	if isSession(err) {
		t.Error("quota error must not be a session error")
	}
}

func TestSessionErrorTriggersSessionError(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{"error": "deepseek_ts_required"})
	_, err := translateEvent("error", []string{string(raw)})
	if err == nil {
		t.Fatal("expected error")
	}
	if !isSession(err) {
		t.Fatalf("want SessionVerificationError, got %T: %v", err, err)
	}
	if isQuota(err) {
		t.Error("session error must not be a quota error")
	}
}

func TestPlainSSEErrorDoesNotForceRefreshPath(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{"error": "temporary upstream failure"})
	_, err := translateEvent("error", []string{string(raw)})
	if err == nil {
		t.Fatal("expected error")
	}
	if isQuota(err) || isSession(err) {
		t.Fatalf("plain error must not be quota/session, got %T", err)
	}
}

func TestGermanTsRequiredPayloadIsSessionError(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{
		"error":       "Sicherheitspruefung erforderlich.",
		"ts_required": true,
	})
	_, err := translateEvent("error", []string{string(raw)})
	if err == nil {
		t.Fatal("expected error")
	}
	if !isSession(err) {
		t.Fatalf("want SessionVerificationError, got %T: %v", err, err)
	}
}

func TestTurnstileRequiredWhileDisabledFailsFast(t *testing.T) {
	cfg := testConfig(t)
	cfg.Proxy.URL = ""
	cfg.Upstream.AutoRefresh = true
	cfg.Upstream.RefreshRetries = 4

	var calls int
	c := New(cfg, nil, nil, nil)
	c.chatOnceOverride = func(ctx context.Context, site config.Site, modelID string, model config.Model,
		prompt string, route Route, info *ServeInfo, yield func(Event) error) error {
		calls++
		return newSession(`sse error: {"error":"Sicherheitspruefung erforderlich.","ts_required":true}`)
	}

	err := c.Chat(context.Background(), "deepseek-v4-flash-de", "hello", &ServeInfo{}, func(Event) error { return nil })
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "requires Turnstile verification") {
		t.Errorf("error = %q, want a Turnstile hint", err.Error())
	}
	if calls != 1 {
		t.Errorf("upstream calls = %d, want 1 (no blind refresh retries)", calls)
	}
}

func TestNonceFetchFailureFailsFast(t *testing.T) {
	cfg := testConfig(t)
	cfg.Proxy.URL = ""
	cfg.Upstream.AutoRefresh = true
	cfg.Upstream.RefreshRetries = 4

	var calls int
	c := New(cfg, nil, nil, nil)
	c.chatOnceOverride = func(ctx context.Context, site config.Site, modelID string, model config.Model,
		prompt string, route Route, info *ServeInfo, yield func(Event) error) error {
		calls++
		return errf("nonce fetch failed for %s: <!DOCTYPE html>", modelID)
	}

	err := c.Chat(context.Background(), "deepseek-v4-flash-de", "hello", &ServeInfo{}, func(Event) error { return nil })
	if err == nil {
		t.Fatal("expected an error")
	}
	if calls != 1 {
		t.Errorf("upstream calls = %d, want 1 (config problems are not retried)", calls)
	}
	if !isConfigProblem(err) {
		t.Errorf("err = %v, want isConfigProblem", err)
	}
}

func TestSSEDeltaAndDoneEvents(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{"delta": "hello world"})
	evs, err := translateEvent("message", []string{string(raw)})
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if len(evs) != 1 || evs[0].Kind != KindDelta || evs[0].Value != "hello world" {
		t.Fatalf("events = %+v", evs)
	}
	evs, err = translateEvent("message", []string{"[DONE]"})
	if err != nil || len(evs) != 1 || evs[0].Kind != KindDone {
		t.Fatalf("done event = %+v err=%v", evs, err)
	}
}

func TestQuotaSiteCoolsDownAndPrefersSibling(t *testing.T) {
	cfg := testConfig(t)
	c := New(cfg, nil, nil, nil)
	calls := map[string]int{}
	c.chatOnceOverride = func(_ context.Context, site config.Site, modelID string, model config.Model,
		_ string, _ Route, _ *ServeInfo, _ func(Event) error) error {
		calls[site.Code]++
		if site.Code == "de" {
			return newQuota("sse quota exhausted: test")
		}
		return nil
	}
	emit := func(Event) error { return nil }

	// Round 1: primary reports quota, a sibling serves the request.
	if err := c.Chat(context.Background(), "deepseek-v4-flash-de", "p", nil, emit); err != nil {
		t.Fatalf("first Chat: %v", err)
	}
	if calls["de"] != 1 {
		t.Fatalf("de calls = %d, want 1", calls["de"])
	}
	if calls["es"]+calls["fr"] == 0 {
		t.Fatal("sibling site was not attempted after primary quota error")
	}

	// Round 2: de sits on cooldown and must be skipped entirely.
	delete(calls, "de")
	delete(calls, "es")
	delete(calls, "fr")
	if err := c.Chat(context.Background(), "deepseek-v4-flash-de", "p", nil, emit); err != nil {
		t.Fatalf("second Chat: %v", err)
	}
	if calls["de"] != 0 {
		t.Fatalf("cooled site hit again: %d calls", calls["de"])
	}
	if calls["es"]+calls["fr"] == 0 {
		t.Fatal("sibling site not used on second round")
	}
	if !c.quotaCooled("de") {
		t.Fatal("de should still be cooling down")
	}
}

func TestCacheEmptyFailsOverToMirrorWithoutRefreshCycles(t *testing.T) {
	cfg := testConfig(t)
	cfg.Upstream.AutoRefresh = true
	cfg.Upstream.RefreshRetries = 3
	c := New(cfg, nil, nil, nil)
	calls := map[string]int{}
	c.chatOnceOverride = func(_ context.Context, site config.Site, modelID string, model config.Model,
		_ string, _ Route, _ *ServeInfo, _ func(Event) error) error {
		calls[site.Code]++
		if site.Code == "de" {
			return errf(`cache message http 400: {"success":false,"data":{"code":"empty_data_to_cache"}}`)
		}
		return nil
	}
	if err := c.Chat(context.Background(), "deepseek-v4-flash-de", "p", nil, func(Event) error { return nil }); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if calls["de"] != 1 {
		t.Fatalf("de calls = %d, want 1 (cache rejection must fail fast, not burn refresh cycles)", calls["de"])
	}
	if calls["es"]+calls["fr"] == 0 {
		t.Fatal("mirror was not attempted after cache rejection")
	}
}

func TestQuotaRotationRecoversWithoutSolve(t *testing.T) {
	cfg := testConfig(t)
	cfg.Upstream.AutoRefresh = true
	cfg.Upstream.RefreshRetries = 3
	solver := turnstile.New(cfg.Turnstile, httpx.DefaultUserAgent)
	const oldGid = "OLDGIDOLDGIDOLDGIDOLDGIDOLDG12"
	if _, _, err := solver.ImportCookies("de", "cf_clearance=abc; dsgt_gid="+oldGid, ""); err != nil {
		t.Fatalf("seed cookies: %v", err)
	}
	c := New(cfg, solver, nil, nil)
	calls := 0
	c.chatOnceOverride = func(_ context.Context, site config.Site, modelID string, model config.Model,
		_ string, _ Route, _ *ServeInfo, _ func(Event) error) error {
		calls++
		if calls == 1 {
			return newQuota("sse quota exhausted: test")
		}
		return nil
	}
	if err := c.Chat(context.Background(), "deepseek-v4-flash-de", "p", nil, func(Event) error { return nil }); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if calls != 2 {
		t.Fatalf("chat calls = %d, want 2 (quota then success)", calls)
	}
	gid, ok := solver.GuestID("de", "")
	if !ok {
		t.Fatal("guest id lost after rotation")
	}
	if gid == oldGid {
		t.Fatal("visitor id was not rotated after the quota error")
	}
	if c.quotaCooled("de") {
		t.Error("cooldown should be cleared once the site serves again")
	}
}

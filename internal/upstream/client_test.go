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

	c := New(cfg, nil, nil)
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
	c := New(cfg, nil, nil)
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
	c := New(cfg, nil, nil)
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
	c := New(cfg, nil, nil)
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
	c := New(cfg, nil, nil)
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
	c := New(cfg, nil, nil)
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

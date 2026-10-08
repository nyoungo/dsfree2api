package turnstile

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nyoungo/dsfree2api/internal/config"
	"github.com/nyoungo/dsfree2api/internal/httpx"
)

// 回归：retries=0 时求解循环必须至少跑一次，不能直接返回 ("", nil) ——
// 那会让 refreshLocked 以为求解成功，把未经验证的 cookie 缓存进池子。
func TestSolveTokenAPIRetriesZeroStillAttemptsOnce(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"token":"tok-1","elapsed":0.1}`))
	}))
	defer srv.Close()

	solver := New(config.Turnstile{
		Enabled: true, Provider: config.ProviderAPI, APIStyle: "ezsolver",
		APIURL: srv.URL, TimeoutSeconds: 5, Retries: 0,
	}, httpx.DefaultUserAgent)

	token, err := solver.solveTokenAPI(context.Background(), "https://deepseek.de/", "0xabc", solver.coreFor(""))
	if err != nil {
		t.Fatalf("solve: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("solve API 被调用 %d 次，want 1", calls.Load())
	}
	if token == "" {
		t.Fatal("retries=0 返回空 token 且无错误 —— 上游根本没被调用")
	}
}

// 回归：retries=0 时校验循环也必须至少跑一次，否则会跳过 verify 直接成功。
func TestVerifyTokenRetriesZeroStillVerifiesOnce(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	solver := New(config.Turnstile{
		Enabled: true, Provider: config.ProviderAPI,
		APIURL: srv.URL, TimeoutSeconds: 5, Retries: 0,
	}, httpx.DefaultUserAgent)

	sess, err := httpx.NewSession(httpx.Options{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	defer sess.Close()

	site := &config.Site{
		Code: "t", BaseURL: srv.URL, AJAXURL: srv.URL,
		VerifyAction: "verify", Language: "en",
	}
	if err := solver.verifyToken(context.Background(), sess, site, "tok"); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("verify 被调用 %d 次，want 1 —— 空 token 会被判定为校验成功", calls.Load())
	}
}

// 回归：coreFor 拿到非法代理 URL 时不能吞掉 StdClient 的错误并缓存 nil client，
// 否则 solveTokenAPI 里 client.Do 会 nil panic（warmer 后台协程没有 recover，
// 一次就能拖垮整个进程）。
func TestCoreForInvalidProxyDoesNotPanic(t *testing.T) {
	solver := New(config.Turnstile{
		Enabled: true, Provider: config.ProviderAPI, APIStyle: "ezsolver",
		APIURL: "https://solve.example.invalid/", TimeoutSeconds: 5, Retries: 1,
	}, httpx.DefaultUserAgent)

	c := solver.coreFor("://bad proxy")
	if c == nil {
		t.Fatal("coreFor returned nil")
	}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("coreFor 留下 nil client 导致 panic: %v", r)
		}
	}()

	token, err := solver.solveTokenAPI(context.Background(), "https://deepseek.de/", "0xabc", c)
	if err == nil {
		t.Fatalf("非法代理应当报错，却返回 token=%q", token)
	}
}

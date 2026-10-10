package upstream

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/nyoungo/dsfree2api/internal/httpx"
)

// 回归：SSE 规范要求同一事件的多个 data 行用 "\n" 拼接后再解析。逐行单独
// json.Unmarshal 会把跨行 JSON 整条丢弃，导致该事件静默消失。
func TestTranslateEventJoinsMultiLineData(t *testing.T) {
	evs, err := translateEvent("message", []string{`{"delta":`, `"hello"}`})
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if len(evs) != 1 || evs[0].Kind != KindDelta || evs[0].Value != "hello" {
		t.Fatalf("events = %+v, want one delta %q", evs, "hello")
	}
}

// 回归：readSSE → translateEvent 全链路上，跨行 data 的事件不能丢失，
// 单行事件与 [DONE] 必须照常工作。
func TestReadSSEMultiLineDataEvent(t *testing.T) {
	const body = "event: message\n" +
		"data: {\"delta\":\n" +
		"data: \"hello\"}\n" +
		"\n" +
		"data: [DONE]\n" +
		"\n"
	var got []Event
	if err := readSSE(strings.NewReader(body), func(event string, lines []string) error {
		evs, err := translateEvent(event, lines)
		got = append(got, evs...)
		return err
	}); err != nil {
		t.Fatalf("readSSE: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("events = %+v, want delta + done", got)
	}
	if got[0].Kind != KindDelta || got[0].Value != "hello" {
		t.Fatalf("first event = %+v, want delta %q", got[0], "hello")
	}
	if got[1].Kind != KindDone {
		t.Fatalf("second event = %+v, want done", got[1])
	}
}

// 前提：httpx.Stream 对 >=400 把状态码连同错误一起返回，调用方拿得到 401。
// （chatOnce 里 `rc, _, err := sess.Stream(...)` 把它丢掉了。）
func TestStreamErrorCarriesHTTPStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "expired", http.StatusUnauthorized)
	}))
	defer srv.Close()

	sess, err := httpx.NewSession(httpx.Options{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	defer sess.Close()

	rc, status, err := sess.Stream(t.Context(), httpx.Request{Method: http.MethodGet, URL: srv.URL})
	if err == nil {
		if rc != nil {
			rc.Close()
		}
		t.Fatalf("status %d should have returned an error", status)
	}
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", status, http.StatusUnauthorized)
	}
}

// 回归：流建立阶段的 401/403 必须归类为 session 错误，否则 failover 循环会
// 走 poolReport(proxy, false) 把出口代理当故障标记，同时不会刷新 cookie，
// 带着同一份过期 cookie 重试到超时。
func TestStreamHTTPErrorClassifiesUnauthorizedAsSession(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		err := streamHTTPError(code, errors.New("upstream status"))
		if !isSession(err) {
			t.Fatalf("streamHTTPError(%d) = %#v, want session error", code, err)
		}
	}
	if isSession(streamHTTPError(http.StatusBadGateway, errors.New("upstream status"))) {
		t.Fatalf("502 must stay a plain upstream error")
	}
}

// 回归：并发闸门的容量必须跟随 [limits].max_concurrent_per_site 变更。旧实现
// 用"首次 acquire 时定容的 channel"，改配置（控制台或 PUT /api/config）后要
// 重启才生效。
func TestConcurrencyGateFollowsConfigChanges(t *testing.T) {
	cfg := testConfig(t)
	cfg.Limits.MaxConcurrentPerSite = 4
	c := New(cfg, nil, nil, nil)
	ctx := context.Background()

	// 先按 4 建一次闸门
	if err := c.acquire(ctx, "site-a"); err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	c.release("site-a")

	cfg.Limits.MaxConcurrentPerSite = 1
	if err := c.acquire(ctx, "site-a"); err != nil {
		t.Fatalf("新上限下的第一个名额: %v", err)
	}
	short, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	if err := c.acquire(short, "site-a"); err == nil {
		c.release("site-a")
		t.Fatalf("上限已改成 1，第二个并发请求却拿到了名额")
	}
	c.release("site-a")
	if err := c.acquire(ctx, "site-a"); err != nil {
		t.Fatalf("release 之后应能拿到名额: %v", err)
	}
	c.release("site-a")
}

// 回归：上限必须真的挡住并发（防止把闸门改坏）。
func TestConcurrencyGateBlocksBeyondLimit(t *testing.T) {
	cfg := testConfig(t)
	cfg.Limits.MaxConcurrentPerSite = 2
	c := New(cfg, nil, nil, nil)
	ctx := context.Background()

	if err := c.acquire(ctx, "site-b"); err != nil {
		t.Fatalf("slot 1: %v", err)
	}
	if err := c.acquire(ctx, "site-b"); err != nil {
		t.Fatalf("slot 2: %v", err)
	}
	short, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	if err := c.acquire(short, "site-b"); err == nil {
		t.Fatalf("第三个名额不该发出去")
	}
	c.release("site-b")
	c.release("site-b")
	if err := c.acquire(ctx, "site-b"); err != nil {
		t.Fatalf("release 后应恢复: %v", err)
	}
	c.release("site-b")
}

// 回归：0 = 不限并发（与 config.example.toml 的注释一致）。
func TestConcurrencyGateZeroMeansUnlimited(t *testing.T) {
	cfg := testConfig(t)
	cfg.Limits.MaxConcurrentPerSite = 0
	c := New(cfg, nil, nil, nil)
	ctx := context.Background()

	for i := 0; i < 8; i++ {
		if err := c.acquire(ctx, "site-c"); err != nil {
			t.Fatalf("acquire #%d: %v", i, err)
		}
	}
	for i := 0; i < 8; i++ {
		c.release("site-c")
	}
}

// 回归：并发增删 + 中途改上限，不能漏发名额也不能多发（-race 下验证记账）。
func TestConcurrencyGateUnderConcurrentLoad(t *testing.T) {
	cfg := testConfig(t)
	cfg.Limits.MaxConcurrentPerSite = 3
	c := New(cfg, nil, nil, nil)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 40; i++ {
			cfg.Lock()
			cfg.Limits.MaxConcurrentPerSite = []int{1, 3, 5, 0}[i%4]
			cfg.Unlock()
			time.Sleep(2 * time.Millisecond)
		}
		close(stop)
	}()
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
				if err := c.acquire(ctx, "site-d"); err == nil {
					c.release("site-d")
				}
				cancel()
			}
		}()
	}
	wg.Wait()
}

// 回归：错误信息里的字节截断不能切碎多字节字符。
func TestUpstreamTruncateKeepsUTF8Valid(t *testing.T) {
	got := truncate(strings.Repeat("汉", 100), 10)
	if !utf8.ValidString(got) {
		t.Fatalf("truncate 切出了非法 UTF-8: %q", got)
	}
}

// 回归：shortJSON 截断同样要落在字符边界上。
func TestShortJSONKeepsUTF8Valid(t *testing.T) {
	got := shortJSON(map[string]any{
		"aaa":   strings.Repeat("a", 200),
		"error": strings.Repeat("汉", 200),
	})
	if !utf8.ValidString(got) {
		t.Fatalf("shortJSON 切出了非法 UTF-8: %q", got)
	}
}

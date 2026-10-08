package httpx

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// 回归：按字节截断会切碎多字节字符，产出非法 UTF-8 的错误文本
// （随后进 JSON/日志时会变成乱码）。
func TestTruncateKeepsUTF8Valid(t *testing.T) {
	src := strings.Repeat("汉", 100) // 300 bytes
	got := truncate(src, 10)
	if !utf8.ValidString(got) {
		t.Fatalf("truncate 切出了非法 UTF-8: %q (bytes=%v)", got, []byte(got[:min(len(got), 14)]))
	}
	if !strings.HasSuffix(got, "...") {
		t.Fatalf("got %q, want an ellipsis suffix", got)
	}
}

// 回归：不超长的字符串原样返回。
func TestTruncateShortInputUnchanged(t *testing.T) {
	if got := truncate("hello", 10); got != "hello" {
		t.Fatalf("got %q", got)
	}
}

// 回归：非流式响应体必须有读取上限。裸 io.ReadAll 会让上游（或中间人）
// 用一个超大响应把网关内存打爆（OOM）。
func TestDoRejectsOversizedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		chunk := make([]byte, 32<<10)
		for i := 0; i < 64; i++ { // 2MB，远超下面设的 64KB 上限
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	s, err := NewSession(Options{Timeout: 15 * time.Second, MaxBodyBytes: 64 << 10})
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	defer s.Close()

	resp, err := s.Do(context.Background(), Request{Method: "GET", URL: srv.URL})
	if !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("err = %v (resp=%v), want ErrBodyTooLarge", err, resp)
	}
}

// 回归：上限以内的响应体必须原样读全，不能误伤正常请求。
func TestDoReadsBodyWithinLimit(t *testing.T) {
	want := strings.Repeat("x", 4096)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(want))
	}))
	defer srv.Close()

	s, err := NewSession(Options{Timeout: 15 * time.Second, MaxBodyBytes: 64 << 10})
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	defer s.Close()

	resp, err := s.Do(context.Background(), Request{Method: "GET", URL: srv.URL})
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	if string(resp.Body) != want {
		t.Fatalf("body len = %d, want %d", len(resp.Body), len(want))
	}
}

// 回归：TLS 指纹档案的 Chrome 主版本必须与默认 UA 的主版本贴近。
// 曾经 UA=Chrome/149 而指纹=Chrome_120，差 29 个大版本 —— JA3 指纹与
// UA 自相矛盾，是可识别的异常客户端特征。
func TestTLSFingerprintAlignedWithDefaultUA(t *testing.T) {
	uaMajor, ok := uaChromeMajor(DefaultUserAgent)
	if !ok {
		t.Fatalf("DefaultUserAgent 里解析不出 Chrome 主版本: %q", DefaultUserAgent)
	}
	id := DefaultProfile().GetClientHelloId()
	if !strings.EqualFold(id.Client, "Chrome") {
		t.Fatalf("指纹档案客户端 = %q, want Chrome", id.Client)
	}
	profMajor, err := strconv.Atoi(id.Version)
	if err != nil {
		t.Fatalf("指纹档案版本 = %q, want 纯数字主版本", id.Version)
	}
	if d := uaMajor - profMajor; d < -1 || d > 1 {
		t.Fatalf("UA 主版本 %d 与指纹档案主版本 %d 相差 %d（>1），指纹与 UA 脱节",
			uaMajor, profMajor, d)
	}
}

// uaChromeMajor 从 UA 串里取出 "Chrome/<major>" 的主版本号。
func uaChromeMajor(ua string) (int, bool) {
	const marker = "Chrome/"
	i := strings.Index(ua, marker)
	if i < 0 {
		return 0, false
	}
	rest := ua[i+len(marker):]
	j := strings.Index(rest, ".")
	if j <= 0 {
		return 0, false
	}
	n, err := strconv.Atoi(rest[:j])
	if err != nil {
		return 0, false
	}
	return n, true
}

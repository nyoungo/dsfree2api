package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nyoungo/dsfree2api/internal/config"
)

func newMiddlewareServer(t *testing.T) *Server {
	t.Helper()
	cfg, err := config.Load(examplePath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	return newTestServerWithConfig(t, cfg)
}

// 回归：SSE 已经开流（200 + 若干字节）之后再 panic，不能又写一个 500 的
// JSON 进响应体 —— 状态码改不了，只会把 {"error"...} 拼在事件流后面，
// 客户端解析流直接失败。
func TestRecoverPanicAfterHeadersKeepsBodyIntact(t *testing.T) {
	s := newMiddlewareServer(t)
	h := s.recoverPanic(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"a\":1}\n\n"))
		panic("boom")
	}))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/chat/completions", nil))

	body := w.Body.String()
	if strings.Contains(body, "internal server error") {
		t.Fatalf("已开流的响应被追加了 500 JSON: %q", body)
	}
	if !strings.Contains(body, `data: {"a":1}`) {
		t.Fatalf("原有事件流丢了: %q", body)
	}
}

// 回归：还没发出响应头就 panic，必须照常返回 500 + 错误对象。
func TestRecoverPanicBeforeHeadersWrites500(t *testing.T) {
	s := newMiddlewareServer(t)
	h := s.recoverPanic(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	}))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/chat/completions", nil))

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
	if !strings.Contains(w.Body.String(), "internal server error") {
		t.Fatalf("body = %q, want the error object", w.Body.String())
	}
}

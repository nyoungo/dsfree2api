package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nyoungo/dsfree2api/internal/config"
	"github.com/nyoungo/dsfree2api/internal/logbuf"
	"github.com/nyoungo/dsfree2api/internal/metrics"
	"github.com/nyoungo/dsfree2api/internal/turnstile"
)

const examplePath = "../../config.example.toml"
const testKey = "sk-dsfr-local-change-me"

func newTestServer(t *testing.T, mutate func(*config.Config)) *httptest.Server {
	t.Helper()
	cfg, err := config.Load(examplePath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg.Security.APIKeys = []string{testKey}
	if mutate != nil {
		mutate(cfg)
	}
	met := metrics.New(t.TempDir())
	met.Start()
	t.Cleanup(met.Close)
	logs := logbuf.New(50)
	ts := turnstile.New(cfg.Turnstile, "")
	srv := New(cfg, nil, met, logs, ts, slog.New(slog.NewTextHandler(io.Discard, nil)))
	tsrv := httptest.NewServer(srv.Handler())
	t.Cleanup(tsrv.Close)
	return tsrv
}

func get(t *testing.T, url string, headers map[string]string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(body)
}

func TestModelsRequiresAuth(t *testing.T) {
	srv := newTestServer(t, nil)
	resp, _ := get(t, srv.URL+"/v1/models", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestModelsRejectsWrongKey(t *testing.T) {
	srv := newTestServer(t, nil)
	resp, _ := get(t, srv.URL+"/v1/models", map[string]string{"Authorization": "Bearer sk-wrong"})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestModelsListsWithAuth(t *testing.T) {
	srv := newTestServer(t, nil)
	resp, body := get(t, srv.URL+"/v1/models", map[string]string{"Authorization": "Bearer " + testKey})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Data) != 6 {
		t.Fatalf("len(data) = %d, want 6", len(out.Data))
	}
	ids := map[string]bool{}
	for _, m := range out.Data {
		ids[m.ID] = true
	}
	for _, want := range []string{"deepseek-v4-flash-es", "deepseek-v4-pro-de"} {
		if !ids[want] {
			t.Errorf("missing model %q", want)
		}
	}
}

func TestModelsWithXApiKeyHeader(t *testing.T) {
	srv := newTestServer(t, nil)
	resp, _ := get(t, srv.URL+"/v1/models", map[string]string{"X-Api-Key": testKey})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

func TestNoKeysMeansNoAuth(t *testing.T) {
	srv := newTestServer(t, func(c *config.Config) { c.Security.APIKeys = nil })
	resp, _ := get(t, srv.URL+"/v1/models", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

func TestRateLimit(t *testing.T) {
	srv := newTestServer(t, func(c *config.Config) { c.Limits.RatePerMinute = 1 })
	h := map[string]string{"Authorization": "Bearer " + testKey}
	if resp, _ := get(t, srv.URL+"/v1/models", h); resp.StatusCode != http.StatusOK {
		t.Fatalf("first status = %d, want 200", resp.StatusCode)
	}
	if resp, _ := get(t, srv.URL+"/v1/models", h); resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second status = %d, want 429", resp.StatusCode)
	}
}

func TestHealthAndRoot(t *testing.T) {
	srv := newTestServer(t, nil)
	resp, body := get(t, srv.URL+"/health", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health status = %d", resp.StatusCode)
	}
	var health map[string]any
	if err := json.Unmarshal([]byte(body), &health); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if health["status"] != "ok" {
		t.Errorf("health = %v", health)
	}
	if _, ok := health["models"]; !ok {
		t.Errorf("health missing models: %v", health)
	}
}

func TestUnknownRouteIs404(t *testing.T) {
	srv := newTestServer(t, nil)
	resp, _ := get(t, srv.URL+"/v1/nope", map[string]string{"Authorization": "Bearer " + testKey})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func post(t *testing.T, url string, headers map[string]string, body string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(raw)
}

func anthropicErrType(body string) string {
	var out struct {
		Type  string `json:"type"`
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	_ = json.Unmarshal([]byte(body), &out)
	if out.Type == "error" {
		return out.Error.Type
	}
	return ""
}

// openaiErrType 解析 OpenAI 风格错误体 {"error":{"type":...}}。
func openaiErrType(body string) string {
	var out struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	_ = json.Unmarshal([]byte(body), &out)
	return out.Error.Type
}

// openaiErrMsg 取出 error.message（JSON 会把 ">" 转义成 >，
// 所以断言文案必须走反序列化，不能拿原文子串匹配）。
func openaiErrMsg(body string) string {
	var out struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal([]byte(body), &out)
	return out.Error.Message
}

func TestMessagesRequiresAuthInAnthropicFormat(t *testing.T) {
	srv := newTestServer(t, nil)
	resp, body := post(t, srv.URL+"/v1/messages", nil, `{}`)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d body=%s, want 401", resp.StatusCode, body)
	}
	if got := anthropicErrType(body); got != "authentication_error" {
		t.Errorf("error type = %q, want authentication_error; body=%s", got, body)
	}
}

func TestMessagesValidation(t *testing.T) {
	srv := newTestServer(t, nil)
	h := map[string]string{"Authorization": "Bearer " + testKey, "Content-Type": "application/json"}

	resp, body := post(t, srv.URL+"/v1/messages", h, `{}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty body status = %d body=%s, want 400", resp.StatusCode, body)
	}
	if got := anthropicErrType(body); got != "invalid_request_error" {
		t.Errorf("error type = %q, want invalid_request_error", got)
	}

	resp, body = post(t, srv.URL+"/v1/messages", h,
		`{"model":"no-such-model","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown model status = %d body=%s, want 404", resp.StatusCode, body)
	}
	if got := anthropicErrType(body); got != "not_found_error" {
		t.Errorf("error type = %q, want not_found_error", got)
	}
}

func TestResponsesValidation(t *testing.T) {
	srv := newTestServer(t, nil)
	h := map[string]string{"Authorization": "Bearer " + testKey, "Content-Type": "application/json"}

	resp, body := post(t, srv.URL+"/v1/responses", h, `{"model":"deepseek-v4-flash-de"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing input status = %d body=%s, want 400", resp.StatusCode, body)
	}

	resp, body = post(t, srv.URL+"/v1/responses", h,
		`{"model":"no-such-model","input":"hi"}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown model status = %d body=%s, want 404", resp.StatusCode, body)
	}
}

// 回归：n>1 必须 400 —— 网关只产 1 条 choice，静默忽略会让客户端以为
// 拿到了 n 条候选。校验排在模型查找之前（模型不存在也先报参数错）。
func TestChatRejectsNGreaterThanOne(t *testing.T) {
	srv := newTestServer(t, nil)
	h := map[string]string{"Authorization": "Bearer " + testKey, "Content-Type": "application/json"}

	resp, body := post(t, srv.URL+"/v1/chat/completions", h,
		`{"model":"no-such-model","messages":[{"role":"user","content":"hi"}],"n":2}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("n=2 status = %d body=%s, want 400", resp.StatusCode, body)
	}
	if !strings.Contains(openaiErrMsg(body), "n > 1") {
		t.Errorf("error message should mention n > 1, body=%s", body)
	}
	if got := openaiErrType(body); got != "invalid_request_error" {
		t.Errorf("error type = %q, want invalid_request_error; body=%s", got, body)
	}

	// n=1（含省略）照常放行到模型查找，返回 404
	resp, body = post(t, srv.URL+"/v1/chat/completions", h,
		`{"model":"no-such-model","messages":[{"role":"user","content":"hi"}],"n":1}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("n=1 status = %d body=%s, want 404", resp.StatusCode, body)
	}
}

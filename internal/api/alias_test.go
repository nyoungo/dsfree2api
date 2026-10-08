package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/nyoungo/dsfree2api/internal/config"
)

// aliasHeaders is the auth header every alias test request needs.
func aliasHeaders() map[string]string {
	return map[string]string{"Authorization": "Bearer " + testKey, "Content-Type": "application/json"}
}

// 未知模型必须 404；配了别名/默认模型后同样的请求必须穿过模型查找。
// 上游是 nil，穿过查找后的请求会以 5xx 结束 —— 断言的是「不是 model not found」。
func chatModelStatus(t *testing.T, headers map[string]string, model string) (int, string) {
	t.Helper()
	srv := newTestServer(t, func(c *config.Config) {
		c.ModelAliases = map[string]string{"gpt-5": "deepseek-v4-flash-de"}
		c.DefaultModel = "deepseek-v4-flash-es"
	})
	resp, body := post(t, srv.URL+"/v1/chat/completions", headers,
		`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`)
	return resp.StatusCode, body
}

func TestChatUnknownModelStill404WithoutAnyFallback(t *testing.T) {
	srv := newTestServer(t, func(c *config.Config) { c.ModelAliases = nil; c.DefaultModel = "" })
	resp, body := post(t, srv.URL+"/v1/chat/completions", aliasHeaders(),
		`{"model":"gpt-5","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d body=%s, want 404", resp.StatusCode, body)
	}
	if !strings.Contains(body, "model not found") {
		t.Errorf("body = %s, want model not found", body)
	}
}

func TestChatResolvesAliasAndDefaultModel(t *testing.T) {
	h := aliasHeaders()
	for _, model := range []string{"gpt-5", "totally-unknown"} {
		status, body := chatModelStatus(t, h, model)
		if status == http.StatusNotFound || strings.Contains(body, "model not found") {
			t.Errorf("model %q must resolve via alias/default, got status=%d body=%s", model, status, body)
		}
	}
}

// 别名指向被停用的模型时必须 404 —— 别名不能绕过管理员的停用决定。
func TestChatAliasResolutionDisabledModelStill404(t *testing.T) {
	srv := newTestServer(t, func(c *config.Config) {
		c.ModelAliases = map[string]string{"gpt-5": "deepseek-v4-flash-de"}
		c.DefaultModel = ""
		c.Models["deepseek-v4-flash-de"].Enabled = false
	})
	resp, body := post(t, srv.URL+"/v1/chat/completions", aliasHeaders(),
		`{"model":"gpt-5","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("alias target disabled: status = %d body=%s, want 404", resp.StatusCode, body)
	}
}

func TestModelsListsAliases(t *testing.T) {
	srv := newTestServer(t, func(c *config.Config) {
		c.ModelAliases = map[string]string{"gpt-5": "deepseek-v4-flash-de"}
	})
	resp, body := get(t, srv.URL+"/v1/models", map[string]string{"Authorization": "Bearer " + testKey})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
	if !strings.Contains(body, `"gpt-5"`) {
		t.Errorf("alias must be listed in /v1/models, body=%s", body)
	}
}

// 别名目标被停用时 /v1/models 不能把它列出来（否则客户端以为能用）。
func TestModelsSkipsAliasWithDisabledTarget(t *testing.T) {
	srv := newTestServer(t, func(c *config.Config) {
		c.ModelAliases = map[string]string{"gpt-5": "deepseek-v4-flash-de"}
		c.Models["deepseek-v4-flash-de"].Enabled = false
	})
	resp, body := get(t, srv.URL+"/v1/models", map[string]string{"Authorization": "Bearer " + testKey})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
	if strings.Contains(body, `"gpt-5"`) {
		t.Errorf("alias with disabled target must not be listed, body=%s", body)
	}
}

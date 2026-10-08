package api

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nyoungo/dsfree2api/internal/config"
	"github.com/nyoungo/dsfree2api/internal/logbuf"
	"github.com/nyoungo/dsfree2api/internal/metrics"
	"github.com/nyoungo/dsfree2api/internal/turnstile"
	"github.com/nyoungo/dsfree2api/internal/upstream"
)

func newTestServerWithConfig(t *testing.T, cfg *config.Config) *Server {
	t.Helper()
	met := metrics.New(t.TempDir())
	met.Start()
	t.Cleanup(met.Close)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ts := turnstile.New(cfg.Turnstile, cfg.Upstream.UserAgent)
	up := upstream.New(cfg, ts, log, nil)
	return New(cfg, up, met, logbuf.New(10), ts, log)
}

// 回归：一次流式请求只能计一次 in_flight。handleChat 已经计过，streamChat
// 里又计一次，控制台的并发数会按 2 倍显示。
func TestStreamingRequestCountsInFlightOnce(t *testing.T) {
	// 假站点：每个请求拖 300ms，好让测试在请求进行中采样并发数
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html><body>nope</body></html>"))
	}))
	t.Cleanup(hang.Close)

	cfg, err := config.Load(examplePath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg.Turnstile.Enabled = false
	cfg.Upstream.AutoRefresh = false
	cfg.Upstream.RefreshRetries = 0
	cfg.Upstream.CrossSiteFailover = false
	// 所有站点都指向假站点，避免 failover 打到公网
	for _, st := range cfg.Sites {
		st.BaseURL = hang.URL
		st.AJAXURL = hang.URL + "/wp-admin/admin-ajax.php"
	}
	var modelID string
	for id, m := range cfg.Models {
		if m.Enabled {
			modelID = id
			break
		}
	}
	if modelID == "" {
		t.Fatalf("example config 里没有可用模型")
	}

	s := newTestServerWithConfig(t, cfg)

	body := `{"model":"` + modelID + `","messages":[{"role":"user","content":"hi"}],"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	w := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.handleChat(w, req)
	}()

	var max int64
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
loop:
	for {
		if v := s.met.InFlight(); v > max {
			max = v
		}
		select {
		case <-done:
			break loop
		case <-ticker.C:
		}
	}
	<-done

	if max > 1 {
		t.Fatalf("一次流式请求的 in_flight 峰值 = %d, want 1", max)
	}
	if max == 0 {
		t.Fatalf("请求期间 in_flight 一直是 0，测试没抓到窗口")
	}
}

package quota

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nyoungo/dsfree2api/internal/config"
	"github.com/nyoungo/dsfree2api/internal/httpx"
	"github.com/nyoungo/dsfree2api/internal/turnstile"
)

const examplePath = "../../config.example.toml"

func TestSweepReadsBalancesWithPooledCookies(t *testing.T) {
	var seenGid string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/wp-json/dsgt/v1/balance") {
			t.Errorf("unexpected request %s — the sentinel must not fetch the site page", r.URL.Path)
			return
		}
		if c, err := r.Cookie("dsgt_gid"); err == nil {
			seenGid = c.Value
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"balance":0,"free":{"limit":30000,"used":500,"remaining":29500,"reset_period":"daily"}}`))
	}))
	defer srv.Close()

	cfg, err := config.Load(examplePath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg.Quota.Enabled = true
	cfg.Quota.CheckSeconds = 300
	cfg.Quota.WarnRatio = 0.2
	site := *cfg.Sites["de"]
	site.BaseURL = srv.URL
	cfg.Sites["de"] = &site

	ts := turnstile.New(cfg.Turnstile, "")
	if _, _, err := ts.ImportCookies("de", "dsgt_gid=test-gid; cf_clearance=abc", ""); err != nil {
		t.Fatalf("import cookies: %v", err)
	}

	w := New(cfg, nil, ts, nil)
	w.newSession = func(string) (httpx.Session, error) {
		return httpx.NewSession(httpx.Options{Timeout: 5 * time.Second})
	}
	w.Sweep(context.Background())

	if seenGid != "test-gid" {
		t.Fatalf("balance call visitor cookie = %q, want test-gid", seenGid)
	}
	snap := w.Snapshot()
	if !snap.Enabled {
		t.Error("snapshot should report enabled")
	}
	if len(snap.Entries) != 6 {
		t.Fatalf("entries = %d, want 6 (3 sites × 2 bots)", len(snap.Entries))
	}
	var de *Balance
	for i := range snap.Entries {
		if snap.Entries[i].Site == "de" && snap.Entries[i].BotID == 27487 {
			de = &snap.Entries[i]
		}
	}
	if de == nil {
		t.Fatalf("no de/27487 entry in %+v", snap.Entries)
	}
	if de.Error != "" || de.Limit != 30000 || de.Remaining != 29500 || de.Used != 500 || de.ResetPeriod != "daily" {
		t.Fatalf("de balance = %+v", de)
	}
	if de.Route != "direct" {
		t.Errorf("route = %q, want direct", de.Route)
	}
	for _, e := range snap.Entries {
		if e.Site != "de" && e.Error == "" {
			t.Errorf("site %s should report missing cookies, got %+v", e.Site, e)
		}
	}
}

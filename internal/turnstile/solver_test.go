package turnstile

import (
	"context"
	"io"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nyoungo/dsfree2api/internal/config"
	"github.com/nyoungo/dsfree2api/internal/httpx"
)

const examplePath = "../../config.example.toml"

type fakeSession struct {
	mu      sync.Mutex
	cookies map[string]string
}

func (f *fakeSession) Do(context.Context, httpx.Request) (*httpx.Response, error) {
	return &httpx.Response{Status: 200}, nil
}
func (f *fakeSession) Stream(context.Context, httpx.Request) (io.ReadCloser, int, error) {
	return io.NopCloser(strings.NewReader("")), 200, nil
}
func (f *fakeSession) Cookies() map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]string{}
	for k, v := range f.cookies {
		out[k] = v
	}
	return out
}
func (f *fakeSession) SetCookies(m map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cookies == nil {
		f.cookies = map[string]string{}
	}
	for k, v := range m {
		f.cookies[k] = v
	}
}
func (f *fakeSession) Close() {}

func TestApplyValidCookiesCoalescesConcurrentRefreshes(t *testing.T) {
	cfg, err := config.Load(examplePath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	site := cfg.Sites["de"]
	cfg.Turnstile.Enabled = true
	solver := New(cfg.Turnstile, httpx.DefaultUserAgent)

	var calls int32
	solver.refreshOverride = func(_ context.Context, sess httpx.Session, s *config.Site, c *core) error {
		atomic.AddInt32(&calls, 1)
		time.Sleep(20 * time.Millisecond)
		state := map[string]string{"dsts": "ok"}
		sess.SetCookies(state)
		c.mu.Lock()
		c.cache[s.Code] = &cookieState{cookies: state, expiresAt: time.Now().Add(time.Hour)}
		c.mu.Unlock()
		return nil
	}

	const n = 5
	sessions := make([]*fakeSession, n)
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		sessions[i] = &fakeSession{}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = solver.ApplyValidCookies(context.Background(), sessions[i], site, "")
		}(i)
	}
	wg.Wait()

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("refresh calls = %d, want 1", got)
	}
	for i, sess := range sessions {
		if sess.Cookies()["dsts"] != "ok" {
			t.Errorf("session %d missing dsts cookie: %v", i, sess.Cookies())
		}
		if errs[i] != nil {
			t.Errorf("session %d error: %v", i, errs[i])
		}
	}
}

func TestImportCookiesWorkWithSolverDisabled(t *testing.T) {
	cfg, err := config.Load(examplePath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.Turnstile.Enabled {
		t.Fatal("example config should be disabled — this test depends on it")
	}
	solver := New(cfg.Turnstile, httpx.DefaultUserAgent)

	exp, n, err := solver.ImportCookies("de", "cf_clearance=abc123; deepseek_ts=xyz; Path=/; Secure", "")
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if n != 2 {
		t.Errorf("imported %d cookies, want 2", n)
	}
	if exp.IsZero() || !time.Now().Before(exp) {
		t.Errorf("expiry = %v, want in the future", exp)
	}

	site := cfg.Sites["de"]
	sess := &fakeSession{}
	if err := solver.ApplyValidCookies(context.Background(), sess, site, ""); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got := sess.Cookies()["deepseek_ts"]; got != "xyz" {
		t.Errorf("deepseek_ts = %q, want xyz (imported cookies must apply while disabled)", got)
	}
	if _, ok := sess.Cookies()["cf_clearance"]; !ok {
		t.Error("cf_clearance not applied")
	}

	st := solver.Status([]string{"de"})
	if len(st) != 1 || !st[0].Valid {
		t.Fatalf("status = %+v, want one valid entry", st)
	}
	if st[0].CookieNum != 2 {
		t.Errorf("cookie_count = %d, want 2", st[0].CookieNum)
	}
	if len(solver.History()) != 0 {
		t.Error("import must not show up as a solved record")
	}
}

func TestParseCookiePairs(t *testing.T) {
	got := ParseCookiePairs("a=1; b=2=3\nPath=/\nExpires=Wed, 21 Oct 2015 07:28:00 GMT; \"c\"=4")
	want := map[string]string{"a": "1", "b": "2=3", "c": "4"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parsed = %v, want %v", got, want)
	}
	if len(ParseCookiePairs("   ;;; \n")) != 0 {
		t.Error("empty input should parse to nothing")
	}
}

func TestStatusReportsEveryRequestedSite(t *testing.T) {
	cfg, err := config.Load(examplePath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	solver := New(cfg.Turnstile, httpx.DefaultUserAgent)
	st := solver.Status([]string{"de", "es", "fr"})
	if len(st) != 3 {
		t.Fatalf("len(status) = %d", len(st))
	}
	for _, s := range st {
		if s.Valid {
			t.Errorf("site %s should not be valid before any solve", s.Site)
		}
		if s.CookieNum != 0 {
			t.Errorf("site %s cookie_count = %d", s.Site, s.CookieNum)
		}
	}
}

func TestHistoryIsCappedAndRecorded(t *testing.T) {
	cfg, err := config.Load(examplePath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	solver := New(cfg.Turnstile, httpx.DefaultUserAgent)
	for i := 0; i < 250; i++ {
		solver.record("de", "direct", time.Now().Add(-time.Second), nil)
	}
	h := solver.History()
	if len(h) == 0 {
		t.Fatal("history is empty")
	}
	if len(h) > 100 {
		t.Errorf("history len = %d, want <= 100", len(h))
	}
	if !h[len(h)-1].OK {
		t.Error("last record should be OK")
	}
}

func TestRotateGuestSwapsVisitorID(t *testing.T) {
	cfg, err := config.Load(examplePath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	solver := New(cfg.Turnstile, httpx.DefaultUserAgent)
	const oldGid = "OLDGIDOLDGIDOLDGIDOLDGIDOLDG12"
	if _, _, err := solver.ImportCookies("de", "cf_clearance=abc; dsgt_gid="+oldGid, ""); err != nil {
		t.Fatalf("import: %v", err)
	}
	gid, ok := solver.GuestID("de", "")
	if !ok || gid != oldGid {
		t.Fatalf("GuestID = %q %v, want %q", gid, ok, oldGid)
	}
	sess := &fakeSession{}
	if err := solver.ApplyValidCookies(context.Background(), sess, cfg.Sites["de"], ""); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if sess.Cookies()["dsgt_gid"] != oldGid {
		t.Fatalf("session gid = %q, want the cached %q", sess.Cookies()["dsgt_gid"], oldGid)
	}

	rotated := solver.RotateGuest("de")
	if rotated == "" {
		t.Fatal("RotateGuest returned empty for a live session")
	}
	if rotated == oldGid {
		t.Fatal("RotateGuest kept the old id")
	}
	if len(rotated) != 32 {
		t.Errorf("rotated id length = %d, want 32", len(rotated))
	}
	if got, _ := solver.GuestID("de", ""); got != rotated {
		t.Errorf("GuestID after rotate = %q, want %q", got, rotated)
	}
	sess2 := &fakeSession{}
	if err := solver.ApplyValidCookies(context.Background(), sess2, cfg.Sites["de"], ""); err != nil {
		t.Fatalf("apply 2: %v", err)
	}
	if sess2.Cookies()["dsgt_gid"] != rotated {
		t.Errorf("fresh session gid = %q, want rotated %q", sess2.Cookies()["dsgt_gid"], rotated)
	}
	if got := solver.RotateGuest("no-such-site"); got != "" {
		t.Errorf("rotating an unknown site returned %q, want empty", got)
	}
}

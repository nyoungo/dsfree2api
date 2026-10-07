package turnstile

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nyoungo/dsfree2api/internal/config"
	"github.com/nyoungo/dsfree2api/internal/httpx"
)

func warmerSolver(t *testing.T, mutate func(*config.Turnstile)) (*Solver, *config.Config) {
	t.Helper()
	cfg, err := config.Load(examplePath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg.Turnstile.Enabled = true
	cfg.Turnstile.WarmEnabled = true
	cfg.Turnstile.CookieTTLSeconds = 3600
	cfg.Turnstile.WarmRatio = 0.3
	if mutate != nil {
		mutate(&cfg.Turnstile)
	}
	return New(cfg.Turnstile, httpx.DefaultUserAgent), cfg
}

func waitCalls(t *testing.T, calls *int32, want int32) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(calls) >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("refresh calls = %d, want >= %d", atomic.LoadInt32(calls), want)
}

func waitSettled(t *testing.T, solver *Solver) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		ps := solver.PoolStatus()
		busy := false
		for _, e := range ps.Entries {
			if e.Warming {
				busy = true
			}
		}
		if !busy {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("cookie pool did not settle")
}

// rewindLastAt backdates the last attempt of a slot so backoff windows can be
// tested without sleeping through them.
func rewindLastAt(solver *Solver, key string, d time.Duration) {
	solver.poolMu.Lock()
	if st := solver.warms[key]; st != nil {
		st.lastAt = time.Now().Add(-d)
	}
	solver.poolMu.Unlock()
}

func TestWarmDueBacksOffAfterFailure(t *testing.T) {
	solver, cfg := warmerSolver(t, nil)
	tgt := WarmTarget{Site: *cfg.Sites["de"], Proxy: ""}
	key := poolKey("", "de")
	ttl := time.Hour

	if !solver.warmDue(key, tgt, ttl, 0.3, 5) {
		t.Fatal("cold slot should be due")
	}

	// First failure: the 5s backoff must hold.
	solver.markWarm(key, tgt, false, time.Now(), 100, "boom")
	if solver.warmDue(key, tgt, ttl, 0.3, 5) {
		t.Fatal("first backoff was ignored")
	}

	// Second failure doubles it: 6s elapsed is still inside the 10s window.
	solver.markWarm(key, tgt, false, time.Now(), 100, "boom")
	rewindLastAt(solver, key, 6*time.Second)
	if solver.warmDue(key, tgt, ttl, 0.3, 5) {
		t.Fatal("second failure did not double the backoff")
	}

	// 11s elapsed clears the 10s window.
	rewindLastAt(solver, key, 11*time.Second)
	if !solver.warmDue(key, tgt, ttl, 0.3, 5) {
		t.Fatal("slot should be due once the backoff elapsed")
	}

	// A success resets the failure counter.
	solver.markWarm(key, tgt, false, time.Now(), 50, "")
	if !solver.warmDue(key, tgt, ttl, 0.3, 5) {
		t.Fatal("success should clear the backoff")
	}

	// The backoff is capped at 15 minutes.
	for i := 0; i < 12; i++ {
		solver.markWarm(key, tgt, false, time.Now(), 100, "boom")
	}
	rewindLastAt(solver, key, 11*time.Second)
	if solver.warmDue(key, tgt, ttl, 0.3, 5) {
		t.Fatal("capped backoff should still hold at 11s")
	}
	rewindLastAt(solver, key, 16*time.Minute)
	if !solver.warmDue(key, tgt, ttl, 0.3, 5) {
		t.Fatal("slot should be due after the capped backoff")
	}
}

func TestWarmSweepFillsColdSlotAndSkipsFresh(t *testing.T) {
	solver, cfg := warmerSolver(t, nil)
	var calls int32
	solver.refreshOverride = func(_ context.Context, _ httpx.Session, s *config.Site, c *core) error {
		atomic.AddInt32(&calls, 1)
		c.mu.Lock()
		c.cache[s.Code] = &cookieState{cookies: map[string]string{"dsts": "ok"}, expiresAt: time.Now().Add(time.Hour)}
		c.mu.Unlock()
		return nil
	}
	targets := func() []WarmTarget {
		return []WarmTarget{{Site: *cfg.Sites["de"], Proxy: ""}}
	}

	// Cold slot: the first sweep must start a solve.
	solver.sweep(context.Background(), targets)
	waitCalls(t, &calls, 1)
	waitSettled(t, solver)

	// Fresh cookie (1h left, threshold 0.3×1h): a second sweep must not solve.
	solver.sweep(context.Background(), targets)
	time.Sleep(80 * time.Millisecond)
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("fresh slot was re-warmed: calls = %d", got)
	}

	// Remaining TTL below the threshold: due again.
	c := solver.coreFor("")
	c.mu.Lock()
	c.cache["de"] = &cookieState{cookies: map[string]string{"dsts": "ok"}, expiresAt: time.Now().Add(30 * time.Second)}
	c.mu.Unlock()
	solver.sweep(context.Background(), targets)
	waitCalls(t, &calls, 2)
	waitSettled(t, solver)

	ps := solver.PoolStatus()
	if !ps.Enabled {
		t.Error("pool should report enabled")
	}
	if len(ps.Entries) != 1 {
		t.Fatalf("pool entries = %d, want 1", len(ps.Entries))
	}
	e := ps.Entries[0]
	if !e.Valid || e.LastAt == 0 || e.LastErr != "" || e.Warming {
		t.Errorf("pool entry = %+v", e)
	}
	if e.NextWarm == 0 {
		t.Error("next_warm_at should be scheduled for a valid pooled cookie")
	}
}

func TestWarmSweepSkipsManualAndDisabled(t *testing.T) {
	var calls int32
	override := func(context.Context, httpx.Session, *config.Site, *core) error {
		atomic.AddInt32(&calls, 1)
		return nil
	}

	solver, cfg := warmerSolver(t, func(tc *config.Turnstile) { tc.Provider = config.ProviderManual })
	solver.refreshOverride = override
	solver.sweep(context.Background(), func() []WarmTarget {
		return []WarmTarget{{Site: *cfg.Sites["de"]}}
	})
	time.Sleep(50 * time.Millisecond)
	if calls != 0 {
		t.Fatalf("manual provider must not warm, calls = %d", calls)
	}

	solver2, cfg2 := warmerSolver(t, func(tc *config.Turnstile) { tc.Enabled = false })
	solver2.refreshOverride = override
	solver2.sweep(context.Background(), func() []WarmTarget {
		return []WarmTarget{{Site: *cfg2.Sites["de"]}}
	})
	time.Sleep(50 * time.Millisecond)
	if calls != 0 {
		t.Fatalf("disabled solver must not warm, calls = %d", calls)
	}

	solver3, cfg3 := warmerSolver(t, func(tc *config.Turnstile) { tc.WarmEnabled = false })
	solver3.refreshOverride = override
	solver3.sweep(context.Background(), func() []WarmTarget {
		return []WarmTarget{{Site: *cfg3.Sites["de"]}}
	})
	time.Sleep(50 * time.Millisecond)
	if calls != 0 {
		t.Fatalf("pool switch off must not warm, calls = %d", calls)
	}
}

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/nyoungo/dsfree2api/internal/admin"
	"github.com/nyoungo/dsfree2api/internal/api"
	"github.com/nyoungo/dsfree2api/internal/config"
	"github.com/nyoungo/dsfree2api/internal/logbuf"
	"github.com/nyoungo/dsfree2api/internal/logfile"
	"github.com/nyoungo/dsfree2api/internal/metrics"
	"github.com/nyoungo/dsfree2api/internal/proxypool"
	"github.com/nyoungo/dsfree2api/internal/quota"
	"github.com/nyoungo/dsfree2api/internal/turnstile"
	"github.com/nyoungo/dsfree2api/internal/upstream"
)

func main() {
	var (
		configPath = flag.String("config", "config.toml", "path to config.toml")
		host       = flag.String("host", "", "override bind host")
		port       = flag.Int("port", 0, "override bind port")
		checkOnly  = flag.Bool("check", false, "validate the config and exit")
		showVer    = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()

	if *showVer {
		fmt.Printf("dsfree2api %s\n", admin.Version)
		return
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config error: %v\n", err)
		os.Exit(1)
	}
	if *host != "" {
		cfg.Server.Host = *host
	}
	if *port != 0 {
		cfg.Server.Port = *port
	}
	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "config invalid: %v\n", err)
		os.Exit(1)
	}
	if *checkOnly {
		fmt.Println("config OK")
		return
	}

	dataDir, err := cfg.DataDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot create data dir: %v\n", err)
		os.Exit(1)
	}

	level := parseLevel(cfg.Server.LogLevel)
	logs := logbuf.New(500)
	sinks, closeLog := logSink(cfg, dataDir)
	defer closeLog()
	logger := slog.New(logbuf.NewHandler(
		slog.NewTextHandler(sinks, &slog.HandlerOptions{Level: level}),
		logs,
	))

	met := metrics.New(dataDir)
	met.Start()
	defer met.Close()

	ts := turnstile.New(cfg.Turnstile, cfg.Upstream.UserAgent)

	// Proxy pool: per-site egress endpoints (Xray share links, subscriptions,
	// plain http/socks5) with sticky selection. No-op unless enabled.
	pool := proxypool.New(cfg, dataDir, logger)
	poolCtx, poolCancel := context.WithCancel(context.Background())
	defer poolCancel()
	pool.Start(poolCtx)

	up := upstream.New(cfg, ts, logger, pool)

	// Quota watchdog: every site-side quota exhaustion is logged together with
	// the tokens spent on that site so far — including ones swallowed by
	// cross-site failover, so the cutoff is recorded rather than hidden.
	up.SetQuotaObserver(func(site, model, route string, err error) {
		met.IncSiteQuota(site)
		t := met.SiteTotals(site)
		logger.Warn("site quota exhausted",
			"site", site, "model", model, "route", route, "error", err.Error(),
			"site_requests", t.Requests,
			"site_prompt_tokens", t.PromptTokens,
			"site_completion_tokens", t.CompletionTokens,
			"site_quota_events", t.Quota)
	})

	// Cookie pool: keep one verified session per site × route ahead of time,
	// so API requests never wait for a solve. No-op unless warm_enabled.
	warmCtx, warmCancel := context.WithCancel(context.Background())
	defer warmCancel()
	ts.StartWarmer(warmCtx, func() []turnstile.WarmTarget { return warmTargets(cfg, up, pool) })

	// Quota sentinel: poll the sites' guest-token balances for the console
	// and the "quota low" alarms. No-op unless [quota].enabled.
	quotaCtx, quotaCancel := context.WithCancel(context.Background())
	defer quotaCancel()
	quotaWatch := quota.New(cfg, logger, ts, pool)
	quotaWatch.Start(quotaCtx)

	apiSrv := api.New(cfg, up, met, logs, ts, logger)
	adminSrv := admin.New(cfg, up, met, logs, ts, logger, pool, quotaWatch)

	mainAddr := fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port)
	mainHTTP := &http.Server{
		Addr:              mainAddr,
		Handler:           apiSrv.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
		ErrorLog:          nil,
	}

	errCh := make(chan error, 2)
	go func() {
		logger.Info("OpenAI-compatible API listening",
			"addr", mainAddr, "models", countModels(cfg), "admin", cfg.Admin.Enabled)
		if err := mainHTTP.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("api server: %w", err)
		}
	}()

	var adminHTTP *http.Server
	if cfg.Admin.Enabled {
		adminAddr := fmt.Sprintf("%s:%d", cfg.Admin.Host, cfg.Admin.Port)
		adminHTTP = &http.Server{
			Addr:              adminAddr,
			Handler:           adminSrv.Handler(),
			ReadHeaderTimeout: 15 * time.Second,
		}
		go func() {
			logger.Info("web console listening", "addr", adminAddr)
			if err := adminHTTP.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- fmt.Errorf("admin server: %w", err)
			}
		}()
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-errCh:
		logger.Error("server failed", "error", err)
		os.Exit(1)
	case s := <-sig:
		logger.Info("shutting down", "signal", s.String())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = mainHTTP.Shutdown(ctx)
	if adminHTTP != nil {
		_ = adminHTTP.Shutdown(ctx)
	}
	met.Close()
	logger.Info("bye")
}

// logSink opens the daily file log (default <data_dir>/logs/dsfree2api.log)
// and returns the combined writer plus its close function. `log_file = "-"`
// disables file logging; a setup failure degrades to stderr only.
func logSink(cfg *config.Config, dataDir string) (io.Writer, func()) {
	path := strings.TrimSpace(cfg.Server.LogFile)
	if path == "-" {
		return os.Stderr, func() {}
	}
	if path == "" {
		path = filepath.Join(dataDir, "logs", "dsfree2api.log")
	}
	w, err := logfile.New(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "log file disabled: %v\n", err)
		return os.Stderr, func() {}
	}
	return io.MultiWriter(os.Stderr, w), func() { _ = w.Close() }
}

// warmTargets builds the site × route matrix the cookie pool keeps fresh:
// the sticky proxy-pool pick first, then the global primary/fallback lines.
// A site with warm_pool_only skips the global lanes and warms only its bound
// pool routes (request-time failover to the global lanes is unaffected).
func warmTargets(cfg *config.Config, up *upstream.Client, pool *proxypool.Manager) []turnstile.WarmTarget {
	routes := up.Routes()
	cfg.RLock()
	sites := make([]config.Site, 0, len(cfg.Sites))
	for _, st := range cfg.Sites {
		if st != nil && st.Enabled {
			sites = append(sites, *st)
		}
	}
	cfg.RUnlock()
	out := make([]turnstile.WarmTarget, 0, len(routes)*len(sites))
	seen := map[string]bool{}
	add := func(site config.Site, proxy string) {
		key := site.Code + "|" + proxy
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, turnstile.WarmTarget{Site: site, Proxy: proxy})
	}
	for _, site := range sites {
		if pool != nil {
			if cands := pool.Candidates(site.Code); len(cands) > 0 {
				add(site, cands[0].Proxy)
			}
		}
		// warm_pool_only sites keep their cookie pool on the bound pool
		// routes; the global lanes stay unwarmed.
		if site.WarmPoolOnly {
			continue
		}
		for _, r := range routes {
			add(site, r.Proxy)
		}
	}
	return out
}

func countModels(cfg *config.Config) int {
	cfg.RLock()
	defer cfg.RUnlock()
	n := 0
	for _, m := range cfg.Models {
		if m.Enabled {
			n++
		}
	}
	return n
}

func parseLevel(s string) slog.Level {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "DEBUG":
		return slog.LevelDebug
	case "WARN", "WARNING":
		return slog.LevelWarn
	case "ERROR":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

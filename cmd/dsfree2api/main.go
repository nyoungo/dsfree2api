package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/nyoungo/dsfree2api/internal/admin"
	"github.com/nyoungo/dsfree2api/internal/api"
	"github.com/nyoungo/dsfree2api/internal/config"
	"github.com/nyoungo/dsfree2api/internal/logbuf"
	"github.com/nyoungo/dsfree2api/internal/metrics"
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

	level := parseLevel(cfg.Server.LogLevel)
	logs := logbuf.New(500)
	logger := slog.New(logbuf.NewHandler(
		slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}),
		logs,
	))

	dataDir, err := cfg.DataDir()
	if err != nil {
		logger.Error("cannot create data dir", "error", err)
		os.Exit(1)
	}

	met := metrics.New(dataDir)
	met.Start()
	defer met.Close()

	ts := turnstile.New(cfg.Turnstile, cfg.Upstream.UserAgent)
	up := upstream.New(cfg, ts, logger)
	apiSrv := api.New(cfg, up, met, logs, ts, logger)
	adminSrv := admin.New(cfg, up, met, logs, ts, logger)

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

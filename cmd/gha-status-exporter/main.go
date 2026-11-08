// Command gha-status-exporter exports whether the latest GitHub Actions run of each workflow
// failed on the default branch.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"

	"github.com/yama6a/gokit/httpserver"
	"github.com/yama6a/gokit/zapx"

	"github.com/yama6a/gha-status-exporter/internal/config"
	"github.com/yama6a/gha-status-exporter/internal/github"
	"github.com/yama6a/gha-status-exporter/internal/metrics"
	"github.com/yama6a/gha-status-exporter/internal/scan"
)

// version is stamped at build time with -ldflags.
var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "gha-status-exporter:", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println(version)
		return nil
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	log := zapx.New(zapx.Options{Level: cfg.LogLevel})
	defer func() { _ = log.Sync() }()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	m := metrics.New(version)
	scanner := &scan.Scanner{
		Client:   github.New("https://api.github.com", cfg.Token),
		Metrics:  m,
		Log:      log,
		Owner:    cfg.Owner,
		Reserve:  cfg.RateLimitReserve,
		Interval: cfg.ScanInterval,
	}
	srv := httpserver.New(httpserver.Options{
		Addr: cfg.Listen, Metrics: m.Handler(), DrainTimeout: 5 * time.Second, Logger: log,
	})

	log.Info("starting", zap.String("version", version), zap.String("owner", cfg.Owner))

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return srv.Run(gctx) })
	g.Go(func() error {
		scanner.Run(gctx)
		return nil
	})
	if err := g.Wait(); err != nil {
		return fmt.Errorf("run: %w", err)
	}
	return nil
}

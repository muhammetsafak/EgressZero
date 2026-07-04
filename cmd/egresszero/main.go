// Command egresszero is a reverse proxy that streams S3 objects with
// cache-friendly headers, built to sit behind a CDN such as Cloudflare
// so that egress costs approach zero.
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
	"syscall"
	"time"

	"github.com/muhammetsafak/egresszero/internal/config"
	"github.com/muhammetsafak/egresszero/internal/metrics"
	"github.com/muhammetsafak/egresszero/internal/proxy"
	"github.com/muhammetsafak/egresszero/internal/s3client"
	"github.com/muhammetsafak/egresszero/internal/version"
)

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version.Version())
		return
	}

	if err := run(); err != nil {
		slog.Error("fatal", slog.Any("error", err))
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.FromEnv()
	if err != nil {
		return err
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	store, err := s3client.New(ctx, cfg, logger)
	if err != nil {
		return err
	}

	// Metrics are opt-in and served on their own listener so /metrics is
	// never reachable through the CDN host. rec stays a nil Recorder
	// (zero overhead) when METRICS_ADDR is unset.
	var rec proxy.Recorder
	var metricsSrv *http.Server
	if cfg.MetricsAddr != "" {
		m := metrics.New()
		rec = m
		metricsSrv = &http.Server{
			Addr:              cfg.MetricsAddr,
			Handler:           metricsMux(m.Handler()),
			ReadHeaderTimeout: 10 * time.Second,
			ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
		}
	}

	srv := &http.Server{
		Addr:    cfg.ListenAddr,
		Handler: proxy.New(store, cfg, logger, rec),
		// WriteTimeout stays 0 on purpose: any fixed value hard-kills
		// large downloads to slow clients. Stalled clients are handled
		// by the proxy's rolling per-write deadline instead
		// (WRITE_IDLE_TIMEOUT); ReadHeaderTimeout/IdleTimeout bound the
		// rest.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      0,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}

	logger.Info("starting",
		slog.String("version", version.Version()),
		slog.String("addr", cfg.ListenAddr),
		slog.String("bucket", cfg.Bucket),
		slog.String("endpoint", cfg.Endpoint),
		slog.String("key_prefix", cfg.KeyPrefix),
		slog.String("cache_control", cfg.CacheControl),
		slog.Bool("path_style", cfg.ForcePathStyle),
		slog.Bool("auth_enabled", cfg.AuthSecret != ""),
		slog.Duration("write_idle_timeout", cfg.WriteIdleTimeout),
		slog.String("metrics_addr", cfg.MetricsAddr),
		slog.Bool("coalesce", cfg.Coalesce),
	)

	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()
	if metricsSrv != nil {
		go func() {
			if err := metricsSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- err
			}
		}()
	}

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	logger.Info("shutting down", slog.Duration("timeout", cfg.ShutdownTimeout))
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if metricsSrv != nil {
		_ = metricsSrv.Shutdown(shutdownCtx)
	}
	if err := srv.Shutdown(shutdownCtx); err != nil {
		// Streams still running after the drain window are cut; the
		// CDN retries.
		logger.Warn("forced shutdown", slog.Any("error", err))
		return srv.Close()
	}
	return nil
}

// metricsMux serves the Prometheus handler at /metrics and a health
// probe at /healthz; everything else 404s.
func metricsMux(h http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/metrics", h)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte("ok\n"))
	})
	return mux
}

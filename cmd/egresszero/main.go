// Command egresszero is a reverse proxy that streams S3 objects with
// cache-friendly headers, built to sit behind a CDN such as Cloudflare
// so that egress costs approach zero.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/muhammetsafak/egresszero/internal/config"
	"github.com/muhammetsafak/egresszero/internal/proxy"
	"github.com/muhammetsafak/egresszero/internal/s3client"
)

func main() {
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

	srv := &http.Server{
		Addr:    cfg.ListenAddr,
		Handler: proxy.New(store, cfg, logger),
		// WriteTimeout stays 0 on purpose: any fixed value hard-kills
		// large downloads to slow clients. Slow-loris exposure is
		// bounded by ReadHeaderTimeout/IdleTimeout and by the CDN
		// being the only expected client.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      0,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}

	logger.Info("starting",
		slog.String("addr", cfg.ListenAddr),
		slog.String("bucket", cfg.Bucket),
		slog.String("endpoint", cfg.Endpoint),
		slog.String("key_prefix", cfg.KeyPrefix),
		slog.String("cache_control", cfg.CacheControl),
		slog.Bool("path_style", cfg.ForcePathStyle),
		slog.Bool("auth_enabled", cfg.AuthSecret != ""),
	)

	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	logger.Info("shutting down", slog.Duration("timeout", cfg.ShutdownTimeout))
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		// Streams still running after the drain window are cut; the
		// CDN retries.
		logger.Warn("forced shutdown", slog.Any("error", err))
		return srv.Close()
	}
	return nil
}

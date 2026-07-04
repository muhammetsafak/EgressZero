// Package config loads and validates the proxy configuration from
// environment variables.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"time"
)

// Config holds the complete runtime configuration of the proxy.
type Config struct {
	// Bucket is the S3 bucket objects are served from. Required.
	Bucket string
	// Region is the AWS region. Empty means "let the SDK credential
	// chain decide"; the s3client package falls back to us-east-1.
	Region string
	// Endpoint is a custom S3-compatible endpoint URL (R2, MinIO, B2).
	// Empty means real AWS S3.
	Endpoint string
	// ForcePathStyle switches to path-style addressing (MinIO needs it).
	ForcePathStyle bool
	// KeyPrefix is prepended verbatim to every derived object key.
	KeyPrefix string
	// ListenAddr is the address the HTTP server binds to.
	ListenAddr string
	// CacheControl, when non-empty, replaces the upstream Cache-Control
	// header on 200/206/304 responses.
	CacheControl string
	// NotFoundCacheControl, when non-empty, is set as Cache-Control on
	// 404 responses so the CDN can absorb repeat lookups for missing
	// keys. All other errors stay no-store.
	NotFoundCacheControl string
	// AuthSecret enables the CDN-only protection when non-empty:
	// requests must carry the secret in the AuthHeader header.
	AuthSecret string
	// AuthHeader is the header name carrying AuthSecret.
	AuthHeader string
	// LogLevel is the minimum slog level.
	LogLevel slog.Level
	// LogRequests enables one structured log line per request.
	LogRequests bool
	// ShutdownTimeout bounds the graceful-shutdown drain.
	ShutdownTimeout time.Duration
	// WriteIdleTimeout disconnects a client that has not accepted any
	// body bytes for this long. It is a rolling deadline refreshed as
	// the stream progresses, so slow-but-alive clients are unaffected.
	// Zero disables it.
	WriteIdleTimeout time.Duration
}

// FromEnv builds a Config from environment variables. All validation
// errors are collected and returned together (joined) so the operator
// sees every problem in a single run.
func FromEnv() (Config, error) {
	var errs []error

	cfg := Config{
		Bucket:               os.Getenv("S3_BUCKET"),
		Region:               os.Getenv("AWS_REGION"),
		Endpoint:             os.Getenv("S3_ENDPOINT"),
		KeyPrefix:            os.Getenv("S3_KEY_PREFIX"),
		ListenAddr:           envDefault("LISTEN_ADDR", ":8080"),
		CacheControl:         os.Getenv("CACHE_CONTROL"),
		NotFoundCacheControl: os.Getenv("NOT_FOUND_CACHE_CONTROL"),
		AuthSecret:           os.Getenv("PROXY_AUTH_SECRET"),
		AuthHeader:           envDefault("PROXY_AUTH_HEADER", "X-Proxy-Auth"),
	}

	if cfg.Bucket == "" {
		errs = append(errs, errors.New("S3_BUCKET is required"))
	}

	if cfg.Endpoint != "" {
		u, err := url.Parse(cfg.Endpoint)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			errs = append(errs, fmt.Errorf("S3_ENDPOINT %q is not a valid http(s) URL", cfg.Endpoint))
		}
	}

	var err error
	if cfg.ForcePathStyle, err = envBool("S3_FORCE_PATH_STYLE", false); err != nil {
		errs = append(errs, err)
	}
	if cfg.LogRequests, err = envBool("LOG_REQUESTS", false); err != nil {
		errs = append(errs, err)
	}
	if cfg.LogLevel, err = envLogLevel("LOG_LEVEL", slog.LevelInfo); err != nil {
		errs = append(errs, err)
	}
	if cfg.ShutdownTimeout, err = envDuration("SHUTDOWN_TIMEOUT", 15*time.Second); err != nil {
		errs = append(errs, err)
	}
	if cfg.WriteIdleTimeout, err = envDurationZeroOK("WRITE_IDLE_TIMEOUT", 2*time.Minute); err != nil {
		errs = append(errs, err)
	}

	return cfg, errors.Join(errs...)
}

func envDefault(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func envBool(name string, def bool) (bool, error) {
	v := os.Getenv(name)
	if v == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def, fmt.Errorf("%s %q is not a valid boolean", name, v)
	}
	return b, nil
}

func envDuration(name string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(name)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return def, fmt.Errorf("%s %q is not a valid positive duration", name, v)
	}
	return d, nil
}

// envDurationZeroOK is envDuration but accepts "0" (= feature disabled).
func envDurationZeroOK(name string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(name)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		return def, fmt.Errorf("%s %q is not a valid non-negative duration", name, v)
	}
	return d, nil
}

func envLogLevel(name string, def slog.Level) (slog.Level, error) {
	v := os.Getenv(name)
	if v == "" {
		return def, nil
	}
	var l slog.Level
	if err := l.UnmarshalText([]byte(v)); err != nil {
		return def, fmt.Errorf("%s %q is not one of debug, info, warn, error", name, v)
	}
	return l, nil
}

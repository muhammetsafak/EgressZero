package config

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

// clearEnv unsets every variable FromEnv reads so tests are hermetic
// regardless of the host environment.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"S3_BUCKET", "AWS_REGION", "S3_ENDPOINT", "S3_FORCE_PATH_STYLE",
		"S3_KEY_PREFIX", "LISTEN_ADDR", "CACHE_CONTROL",
		"PROXY_AUTH_SECRET", "PROXY_AUTH_HEADER",
		"LOG_LEVEL", "LOG_REQUESTS", "SHUTDOWN_TIMEOUT",
	} {
		t.Setenv(name, "")
	}
}

func TestFromEnvDefaults(t *testing.T) {
	clearEnv(t)
	t.Setenv("S3_BUCKET", "demo")

	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv() error = %v", err)
	}
	if cfg.Bucket != "demo" {
		t.Errorf("Bucket = %q, want demo", cfg.Bucket)
	}
	if cfg.ListenAddr != ":8080" {
		t.Errorf("ListenAddr = %q, want :8080", cfg.ListenAddr)
	}
	if cfg.AuthHeader != "X-Proxy-Auth" {
		t.Errorf("AuthHeader = %q, want X-Proxy-Auth", cfg.AuthHeader)
	}
	if cfg.AuthSecret != "" || cfg.CacheControl != "" || cfg.Endpoint != "" || cfg.KeyPrefix != "" {
		t.Errorf("expected empty optional fields, got %+v", cfg)
	}
	if cfg.ForcePathStyle || cfg.LogRequests {
		t.Error("boolean options should default to false")
	}
	if cfg.LogLevel != slog.LevelInfo {
		t.Errorf("LogLevel = %v, want info", cfg.LogLevel)
	}
	if cfg.ShutdownTimeout != 15*time.Second {
		t.Errorf("ShutdownTimeout = %v, want 15s", cfg.ShutdownTimeout)
	}
}

func TestFromEnvExplicitValues(t *testing.T) {
	clearEnv(t)
	t.Setenv("S3_BUCKET", "assets")
	t.Setenv("AWS_REGION", "eu-central-1")
	t.Setenv("S3_ENDPOINT", "https://minio.example.com:9000")
	t.Setenv("S3_FORCE_PATH_STYLE", "true")
	t.Setenv("S3_KEY_PREFIX", "public/")
	t.Setenv("LISTEN_ADDR", ":9999")
	t.Setenv("CACHE_CONTROL", "public, max-age=31536000")
	t.Setenv("PROXY_AUTH_SECRET", "s3cret")
	t.Setenv("PROXY_AUTH_HEADER", "X-Custom-Auth")
	t.Setenv("LOG_LEVEL", "debug")
	t.Setenv("LOG_REQUESTS", "1")
	t.Setenv("SHUTDOWN_TIMEOUT", "30s")

	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv() error = %v", err)
	}
	if cfg.Region != "eu-central-1" || cfg.Endpoint != "https://minio.example.com:9000" ||
		!cfg.ForcePathStyle || cfg.KeyPrefix != "public/" || cfg.ListenAddr != ":9999" ||
		cfg.CacheControl != "public, max-age=31536000" || cfg.AuthSecret != "s3cret" ||
		cfg.AuthHeader != "X-Custom-Auth" || cfg.LogLevel != slog.LevelDebug ||
		!cfg.LogRequests || cfg.ShutdownTimeout != 30*time.Second {
		t.Errorf("unexpected config: %+v", cfg)
	}
}

func TestFromEnvMissingBucket(t *testing.T) {
	clearEnv(t)

	_, err := FromEnv()
	if err == nil || !strings.Contains(err.Error(), "S3_BUCKET") {
		t.Fatalf("FromEnv() error = %v, want S3_BUCKET error", err)
	}
}

func TestFromEnvInvalidValues(t *testing.T) {
	tests := []struct {
		name, envName, envValue string
	}{
		{"bad endpoint scheme", "S3_ENDPOINT", "ftp://x"},
		{"unparseable endpoint", "S3_ENDPOINT", "http://[::1"},
		{"endpoint without host", "S3_ENDPOINT", "http://"},
		{"bad bool", "S3_FORCE_PATH_STYLE", "yes-please"},
		{"bad log requests bool", "LOG_REQUESTS", "maybe"},
		{"bad log level", "LOG_LEVEL", "loud"},
		{"bad duration", "SHUTDOWN_TIMEOUT", "fast"},
		{"negative duration", "SHUTDOWN_TIMEOUT", "-5s"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("S3_BUCKET", "demo")
			t.Setenv(tt.envName, tt.envValue)

			_, err := FromEnv()
			if err == nil || !strings.Contains(err.Error(), tt.envName) {
				t.Fatalf("FromEnv() error = %v, want error mentioning %s", err, tt.envName)
			}
		})
	}
}

func TestFromEnvCollectsAllErrors(t *testing.T) {
	clearEnv(t)
	t.Setenv("S3_ENDPOINT", "not-a-url")
	t.Setenv("SHUTDOWN_TIMEOUT", "bogus")

	_, err := FromEnv()
	if err == nil {
		t.Fatal("FromEnv() error = nil, want multiple errors")
	}
	for _, want := range []string{"S3_BUCKET", "S3_ENDPOINT", "SHUTDOWN_TIMEOUT"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s", err, want)
		}
	}
}

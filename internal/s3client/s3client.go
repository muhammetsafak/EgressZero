// Package s3client constructs the S3 SDK client and classifies its
// errors into HTTP statuses.
package s3client

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/muhammetsafak/egresszero/internal/config"
)

// New builds an *s3.Client from the default AWS credential chain plus
// the proxy configuration (custom endpoint, path-style addressing).
func New(ctx context.Context, cfg config.Config, logger *slog.Logger) (*s3.Client, error) {
	// The default transport allows only 2 idle connections per host;
	// under a thousand concurrent streams to a single S3 endpoint that
	// causes constant TLS reconnect churn. No overall client timeout —
	// it would cut long body streams; ResponseHeaderTimeout bounds the
	// time to first byte instead.
	httpClient := awshttp.NewBuildableClient().WithTransportOptions(func(t *http.Transport) {
		t.MaxIdleConnsPerHost = 100
		t.ResponseHeaderTimeout = 30 * time.Second
	})

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithHTTPClient(httpClient),
		// Only validate response checksums when S3 requires it;
		// "when supported" (the default) logs a warning per request
		// against stores that send no checksum at all (MinIO, R2).
		awsconfig.WithResponseChecksumValidation(aws.ResponseChecksumValidationWhenRequired),
	)
	if err != nil {
		return nil, fmt.Errorf("load AWS config: %w", err)
	}
	if cfg.Region != "" {
		awsCfg.Region = cfg.Region
	}
	if awsCfg.Region == "" {
		// SigV4 needs *a* region; S3-compatible endpoints accept any.
		awsCfg.Region = "us-east-1"
		logger.Warn("no AWS region configured, defaulting to us-east-1 (set AWS_REGION to silence)")
	}

	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		}
		o.UsePathStyle = cfg.ForcePathStyle
	})
	return client, nil
}

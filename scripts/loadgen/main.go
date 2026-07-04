// Command loadgen drives concurrent, optionally rate-limited GET
// downloads against a URL. It is a test/benchmark helper for the memory
// load test (see docs/loadtest.md), not part of the shipped proxy.
//
// Each worker downloads the URL in a loop for the configured duration,
// discarding the body. A per-stream rate limit keeps many streams open
// simultaneously so the server's steady-state concurrency (and thus its
// memory footprint) can be measured.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

func main() {
	url := flag.String("url", "", "target URL (required)")
	conc := flag.Int("c", 100, "concurrent workers")
	dur := flag.Duration("d", 20*time.Second, "test duration")
	rate := flag.Int("rate", 0, "per-stream byte/s limit (0 = unlimited)")
	chunk := flag.Int("chunk", 32*1024, "read chunk size in bytes")
	flag.Parse()
	if *url == "" {
		fmt.Fprintln(os.Stderr, "loadgen: -url is required")
		os.Exit(2)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	ctx, cancelTimeout := context.WithTimeout(ctx, *dur)
	defer cancelTimeout()

	client := &http.Client{
		Transport: &http.Transport{
			MaxIdleConns:        *conc,
			MaxIdleConnsPerHost: *conc,
			MaxConnsPerHost:     *conc,
		},
	}

	var (
		requests atomic.Int64
		bytes    atomic.Int64
		errs     atomic.Int64
		wg       sync.WaitGroup
	)
	start := time.Now()
	for i := 0; i < *conc; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]byte, *chunk)
			for ctx.Err() == nil {
				if err := download(ctx, client, *url, buf, *rate, &bytes); err != nil {
					if ctx.Err() != nil {
						return
					}
					errs.Add(1)
					continue
				}
				requests.Add(1)
			}
		}()
	}
	wg.Wait()

	elapsed := time.Since(start)
	fmt.Printf("workers=%d duration=%s requests=%d bytes=%d errors=%d throughput=%.1f MB/s\n",
		*conc, elapsed.Round(time.Millisecond), requests.Load(), bytes.Load(), errs.Load(),
		float64(bytes.Load())/(1<<20)/elapsed.Seconds())
}

// download fetches url once, discarding the body at up to rate bytes/s.
func download(ctx context.Context, c *http.Client, url string, buf []byte, rate int, bytes *atomic.Int64) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		io.Copy(io.Discard, resp.Body)
		return fmt.Errorf("status %d", resp.StatusCode)
	}

	streamStart := time.Now()
	var read int64
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			read += int64(n)
			bytes.Add(int64(n))
			if rate > 0 {
				// Sleep until elapsed time matches the bytes read at the
				// target rate, throttling this stream.
				want := time.Duration(float64(read) / float64(rate) * float64(time.Second))
				if sleep := want - time.Since(streamStart); sleep > 0 {
					select {
					case <-time.After(sleep):
					case <-ctx.Done():
						return ctx.Err()
					}
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

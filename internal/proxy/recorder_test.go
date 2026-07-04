package proxy

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/muhammetsafak/egresszero/internal/config"
)

// capturingRecorder records what the handler reports, to assert the
// proxy instruments the right events.
type capturingRecorder struct {
	mu             sync.Mutex
	inFlight       int
	maxInFlight    int
	upstreamCalls  int
	upstreamErrors []int
	requests       []recordedRequest
}

type recordedRequest struct {
	method string
	status int
	bytes  int64
}

func (c *capturingRecorder) IncInFlight() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.inFlight++
	if c.inFlight > c.maxInFlight {
		c.maxInFlight = c.inFlight
	}
}
func (c *capturingRecorder) DecInFlight() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.inFlight--
}
func (c *capturingRecorder) ObserveUpstreamLatency(float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.upstreamCalls++
}
func (c *capturingRecorder) IncUpstreamError(status int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.upstreamErrors = append(c.upstreamErrors, status)
}
func (c *capturingRecorder) ObserveRequest(method string, status int, bytes int64, _ float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requests = append(c.requests, recordedRequest{method, status, bytes})
}

func handlerWithRecorder(store ObjectStore, rec Recorder, mutate func(*config.Config)) *Handler {
	cfg := config.Config{Bucket: "test-bucket", AuthHeader: "X-Proxy-Auth"}
	if mutate != nil {
		mutate(&cfg)
	}
	return New(store, cfg, slog.New(slog.DiscardHandler), rec)
}

func TestMetricsRecordedOnSuccess(t *testing.T) {
	out, _ := getOut("hello world", nil)
	rec := &capturingRecorder{}
	h := handlerWithRecorder(&fakeStore{getOut: out}, rec, nil)

	r := httptest.NewRequest(http.MethodGet, "/img/logo.png", nil)
	h.ServeHTTP(httptest.NewRecorder(), r)

	if len(rec.requests) != 1 {
		t.Fatalf("recorded %d requests, want 1", len(rec.requests))
	}
	got := rec.requests[0]
	if got.method != http.MethodGet || got.status != http.StatusOK || got.bytes != 11 {
		t.Errorf("recorded %+v, want GET/200/11 bytes", got)
	}
	if rec.upstreamCalls != 1 {
		t.Errorf("upstream latency observed %d times, want 1", rec.upstreamCalls)
	}
	if len(rec.upstreamErrors) != 0 {
		t.Errorf("upstream errors = %v, want none", rec.upstreamErrors)
	}
	if rec.inFlight != 0 {
		t.Errorf("in-flight leaked: ended at %d, want 0", rec.inFlight)
	}
	if rec.maxInFlight != 1 {
		t.Errorf("max in-flight = %d, want 1", rec.maxInFlight)
	}
}

func TestMetricsRecordsUpstreamError(t *testing.T) {
	rec := &capturingRecorder{}
	h := handlerWithRecorder(&fakeStore{getErr: sdkAPIErr("GetObject", "NoSuchKey")}, rec, nil)

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/missing", nil))

	if len(rec.upstreamErrors) != 1 || rec.upstreamErrors[0] != http.StatusNotFound {
		t.Errorf("upstream errors = %v, want [404]", rec.upstreamErrors)
	}
	if len(rec.requests) != 1 || rec.requests[0].status != http.StatusNotFound {
		t.Errorf("recorded requests = %+v, want one 404", rec.requests)
	}
}

func TestMetricsNotCountedForNotModified(t *testing.T) {
	hdr := http.Header{}
	hdr.Set("Etag", `"abc123"`)
	rec := &capturingRecorder{}
	h := handlerWithRecorder(&fakeStore{getErr: notModifiedErr(hdr)}, rec, nil)

	r := httptest.NewRequest(http.MethodGet, "/a", nil)
	r.Header.Set("If-None-Match", `"abc123"`)
	h.ServeHTTP(httptest.NewRecorder(), r)

	// 304 is a success, so it is not an upstream error...
	if len(rec.upstreamErrors) != 0 {
		t.Errorf("upstream errors = %v, want none for 304", rec.upstreamErrors)
	}
	// ...but it is still a completed request.
	if len(rec.requests) != 1 || rec.requests[0].status != http.StatusNotModified {
		t.Errorf("recorded requests = %+v, want one 304", rec.requests)
	}
}

func TestMetricsSkipHealthz(t *testing.T) {
	rec := &capturingRecorder{}
	h := handlerWithRecorder(&fakeStore{}, rec, nil)

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if len(rec.requests) != 0 {
		t.Errorf("recorded %d requests for /healthz, want 0", len(rec.requests))
	}
	if rec.inFlight != 0 {
		t.Errorf("in-flight leaked on healthz: %d", rec.inFlight)
	}
}

package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMetricsExposition(t *testing.T) {
	m := New()

	// Drive each collector at least once.
	m.IncInFlight()
	m.ObserveUpstreamLatency(0.012)
	m.ObserveRequest(http.MethodGet, http.StatusOK, 2048, 0.05)
	m.IncUpstreamError(http.StatusNotFound)
	m.DecInFlight()

	rr := httptest.NewRecorder()
	m.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()

	want := []string{
		`egresszero_requests_total{method="GET",status="200"} 1`,
		`egresszero_response_bytes_total 2048`,
		`egresszero_upstream_errors_total{status="404"} 1`,
		"egresszero_request_duration_seconds_bucket",
		"egresszero_upstream_request_duration_seconds_bucket",
		"egresszero_in_flight_requests 0",
		// Go runtime + process collectors are registered too.
		"go_goroutines",
	}
	for _, w := range want {
		if !strings.Contains(body, w) {
			t.Errorf("metrics output missing %q", w)
		}
	}
}

package proxy

// Recorder receives request- and upstream-level measurements. It is
// implemented by internal/metrics; the proxy package deliberately does
// not depend on any metrics library. When metrics are disabled the
// handler uses nopRecorder, whose methods are empty and allocation-free,
// so the hot path is unaffected.
type Recorder interface {
	// IncInFlight / DecInFlight bracket a request in flight.
	IncInFlight()
	DecInFlight()
	// ObserveUpstreamLatency records seconds until S3 returned response
	// headers (time to first byte).
	ObserveUpstreamLatency(seconds float64)
	// IncUpstreamError counts a non-success outcome by classified HTTP
	// status (4xx/5xx and 499 client-closed).
	IncUpstreamError(status int)
	// ObserveRequest records a completed request: its method, final
	// status, body bytes streamed and total duration in seconds.
	ObserveRequest(method string, status int, bytes int64, seconds float64)
}

// nopRecorder is the zero-overhead Recorder used when metrics are off.
type nopRecorder struct{}

func (nopRecorder) IncInFlight()                               {}
func (nopRecorder) DecInFlight()                               {}
func (nopRecorder) ObserveUpstreamLatency(float64)             {}
func (nopRecorder) IncUpstreamError(int)                       {}
func (nopRecorder) ObserveRequest(string, int, int64, float64) {}

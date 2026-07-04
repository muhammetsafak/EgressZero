// Package proxy implements the HTTP handler that streams S3 objects to
// clients with cache-friendly headers.
package proxy

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"golang.org/x/sync/singleflight"

	"github.com/muhammetsafak/egresszero/internal/config"
	"github.com/muhammetsafak/egresszero/internal/s3client"
)

// ObjectStore is the slice of the S3 API the proxy consumes. The
// signatures match *s3.Client exactly, so the real client satisfies it
// without an adapter.
type ObjectStore interface {
	GetObject(ctx context.Context, in *s3.GetObjectInput, optFns ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	HeadObject(ctx context.Context, in *s3.HeadObjectInput, optFns ...func(*s3.Options)) (*s3.HeadObjectOutput, error)
}

// Handler serves GET/HEAD requests by streaming objects from an
// ObjectStore. It implements http.Handler.
type Handler struct {
	store            ObjectStore
	bucket           string
	keyPrefix        string
	cacheControl     string
	notFoundCC       string
	authHeader       string
	authSecret       []byte // sha256 of the secret; nil when auth is disabled
	writeIdleTimeout time.Duration
	logRequests      bool
	logger           *slog.Logger
	metrics          Recorder
	coalesce         bool
	group            singleflight.Group
}

// New builds the handler. A nil Recorder disables instrumentation with
// zero hot-path overhead.
func New(store ObjectStore, cfg config.Config, logger *slog.Logger, rec Recorder) *Handler {
	if rec == nil {
		rec = nopRecorder{}
	}
	h := &Handler{
		store:            store,
		bucket:           cfg.Bucket,
		keyPrefix:        cfg.KeyPrefix,
		cacheControl:     cfg.CacheControl,
		notFoundCC:       cfg.NotFoundCacheControl,
		authHeader:       cfg.AuthHeader,
		writeIdleTimeout: cfg.WriteIdleTimeout,
		logRequests:      cfg.LogRequests,
		logger:           logger,
		metrics:          rec,
		coalesce:         cfg.Coalesce,
	}
	if cfg.AuthSecret != "" {
		sum := sha256.Sum256([]byte(cfg.AuthSecret))
		h.authSecret = sum[:]
	}
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	h.metrics.IncInFlight()
	defer h.metrics.DecInFlight()

	status, written := h.serve(w, r)
	dur := time.Since(start)

	// Health probes fire constantly and would drown out real traffic in
	// the request metrics; count everything else.
	if r.URL.Path != "/healthz" {
		h.metrics.ObserveRequest(r.Method, status, written, dur.Seconds())
	}
	if h.logRequests {
		h.logger.LogAttrs(r.Context(), slog.LevelInfo, "request",
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", status),
			slog.Int64("bytes", written),
			slog.Duration("duration", dur),
		)
	}
}

// serve handles the request and reports the response status and body
// bytes written, for logging. Status 499 means the client disconnected
// and nothing was written.
func (h *Handler) serve(w http.ResponseWriter, r *http.Request) (status int, written int64) {
	// Health check is exempt from auth so orchestrator probes work
	// without the CDN secret. It never touches S3.
	if r.URL.Path == "/healthz" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		n, _ := io.WriteString(w, "ok\n")
		return http.StatusOK, int64(n)
	}

	if h.authSecret != nil {
		// Hash both sides so the comparison length never depends on
		// the secret, then compare in constant time.
		got := sha256.Sum256([]byte(r.Header.Get(h.authHeader)))
		if subtle.ConstantTimeCompare(got[:], h.authSecret) != 1 {
			return h.fail(w, http.StatusForbidden)
		}
	}

	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		return h.fail(w, http.StatusMethodNotAllowed)
	}

	key, errStatus := h.deriveKey(r.URL.Path)
	if errStatus != 0 {
		return h.fail(w, errStatus)
	}

	if r.Method == http.MethodHead {
		return h.head(w, r, key)
	}
	return h.get(w, r, key)
}

// deriveKey maps a URL path to an S3 object key, or returns a non-zero
// HTTP status on rejection. The path arrives percent-decoded from
// net/http; the SDK re-encodes it when signing. Deliberately no
// path.Clean: keys containing "//" must survive.
func (h *Handler) deriveKey(path string) (key string, errStatus int) {
	if strings.ContainsRune(path, 0) {
		return "", http.StatusBadRequest
	}
	key = strings.TrimPrefix(path, "/")
	if key == "" {
		return "", http.StatusNotFound
	}
	// Dot segments are never legitimate object keys in practice and
	// create cache-key aliasing at the CDN edge.
	for _, seg := range strings.Split(key, "/") {
		if seg == "." || seg == ".." {
			return "", http.StatusBadRequest
		}
	}
	return h.keyPrefix + key, 0
}

// isETagValue reports whether v looks like an HTTP entity-tag (starts
// with a double quote, or the "W/" weak-validator prefix) rather than an
// HTTP-date, per RFC 9110's If-Range disambiguation rule.
func isETagValue(v string) bool {
	return strings.HasPrefix(v, `"`) || strings.HasPrefix(v, "W/")
}

// buildGetInput maps the request to a GetObjectInput. ifRangeApplied is
// true when an If-Range precondition (IfMatch/IfUnmodifiedSince) was set.
func (h *Handler) buildGetInput(r *http.Request, key string) (in *s3.GetObjectInput, ifRangeApplied bool) {
	in = &s3.GetObjectInput{
		Bucket: aws.String(h.bucket),
		Key:    aws.String(key),
	}
	rng := r.Header.Get("Range")
	if rng != "" {
		in.Range = aws.String(rng)
	}
	if inm := r.Header.Get("If-None-Match"); inm != "" {
		in.IfNoneMatch = aws.String(inm)
	}
	if ims := r.Header.Get("If-Modified-Since"); ims != "" {
		// Unparseable dates are silently ignored, per RFC 9110.
		if t, err := http.ParseTime(ims); err == nil {
			in.IfModifiedSince = aws.Time(t)
		}
	}
	// If-Range only has meaning alongside Range: honor the Range only if
	// the validator still matches, otherwise return the full body. S3 has
	// no If-Range, so translate it to a precondition on the ranged GET
	// (IfMatch for an entity-tag, IfUnmodifiedSince for a date) and, on
	// 412, retry once without the range to return the full representation.
	if rng != "" {
		if ir := r.Header.Get("If-Range"); ir != "" {
			if isETagValue(ir) {
				in.IfMatch = aws.String(ir)
				ifRangeApplied = true
			} else if t, err := http.ParseTime(ir); err == nil {
				in.IfUnmodifiedSince = aws.Time(t)
				ifRangeApplied = true
			}
			// A value that is neither an entity-tag nor a valid date is
			// malformed; ignore it and serve the range normally.
		}
	}
	return in, ifRangeApplied
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request, key string) (int, int64) {
	in, ifRangeApplied := h.buildGetInput(r, key)
	// Only revalidation requests (with a validator and no If-Range) can
	// resolve to a shareable, body-less 304 and are worth coalescing.
	// Everything else streams its own body.
	if h.coalesce && !ifRangeApplied &&
		(in.IfNoneMatch != nil || in.IfModifiedSince != nil) {
		return h.getCoalesced(w, r, in)
	}
	return h.getDirect(w, r, in, ifRangeApplied)
}

// getDirect performs the GetObject (with the If-Range 412 fallback) and
// streams the body. It is the uncoalesced path.
func (h *Handler) getDirect(w http.ResponseWriter, r *http.Request, in *s3.GetObjectInput, ifRangeApplied bool) (int, int64) {
	upstreamStart := time.Now()
	out, err := h.store.GetObject(r.Context(), in)
	h.metrics.ObserveUpstreamLatency(time.Since(upstreamStart).Seconds())
	if err != nil {
		// IfMatch/IfUnmodifiedSince are only ever set for If-Range, so a
		// 412 here means the validator no longer matches: drop the range
		// and its precondition and return the full representation.
		if ifRangeApplied {
			if status, _ := s3client.Classify(err); status == http.StatusPreconditionFailed {
				in.Range = nil
				in.IfMatch = nil
				in.IfUnmodifiedSince = nil
				out, err = h.store.GetObject(r.Context(), in)
			}
		}
		if err != nil {
			return h.writeError(w, r, err), 0
		}
	}
	return h.streamOut(w, r, out)
}

// streamOut writes headers and streams a successful GetObject body,
// closing it when done.
func (h *Handler) streamOut(w http.ResponseWriter, r *http.Request, out *s3.GetObjectOutput) (int, int64) {
	defer out.Body.Close()

	writeMeta(w.Header(), metaFromGet(out), h.cacheControl)
	status := http.StatusOK
	if out.ContentRange != nil {
		status = http.StatusPartialContent
	}
	w.WriteHeader(status)

	n, err := h.copyBody(w, out.Body)
	if err != nil {
		// Status is already on the wire; the client went away or S3
		// died mid-body. Nothing to send, just record it.
		h.logger.LogAttrs(r.Context(), slog.LevelDebug, "stream aborted",
			slog.String("path", r.URL.Path), slog.Int64("bytes", n), slog.Any("error", err))
	}
	return status, n
}

// copyBody streams body to w with a plain io.Copy: the ResponseWriter
// implements io.ReaderFrom, so net/http streams with its own bounded
// buffers and a custom buffer pool would be bypassed anyway.
//
// When writeIdleTimeout > 0 the body is wrapped so that every read
// refreshes a rolling write deadline on the connection: a client that
// stops accepting bytes is disconnected after the idle window, while a
// slow-but-alive client keeps the stream open indefinitely. The wrapper
// is a plain io.Reader, so the ReaderFrom fast path is preserved.
func (h *Handler) copyBody(w http.ResponseWriter, body io.Reader) (int64, error) {
	if h.writeIdleTimeout <= 0 {
		return io.Copy(w, body)
	}
	rc := http.NewResponseController(w)
	// Reset before the connection is reused for the next request; a
	// leftover deadline would eventually poison keep-alive responses
	// that do not refresh it (errors, healthz).
	defer rc.SetWriteDeadline(time.Time{})
	return io.Copy(w, &deadlineRefresher{body: body, rc: rc, timeout: h.writeIdleTimeout})
}

// deadlineRefresher pushes the connection's write deadline forward on
// every read. Reads only happen while writes make progress, so a
// stalled client stops the reads and the last deadline fires.
type deadlineRefresher struct {
	body        io.Reader
	rc          *http.ResponseController
	timeout     time.Duration
	unsupported bool // writer without deadline support (e.g. test recorders)
}

func (d *deadlineRefresher) Read(p []byte) (int, error) {
	if !d.unsupported {
		if err := d.rc.SetWriteDeadline(time.Now().Add(d.timeout)); err != nil {
			d.unsupported = true
		}
	}
	return d.body.Read(p)
}

func (h *Handler) buildHeadInput(r *http.Request, key string) *s3.HeadObjectInput {
	in := &s3.HeadObjectInput{
		Bucket: aws.String(h.bucket),
		Key:    aws.String(key),
	}
	// Range is deliberately not forwarded on HEAD: HeadObjectOutput
	// does not surface Content-Range reliably, and no CDN needs it.
	if inm := r.Header.Get("If-None-Match"); inm != "" {
		in.IfNoneMatch = aws.String(inm)
	}
	if ims := r.Header.Get("If-Modified-Since"); ims != "" {
		if t, err := http.ParseTime(ims); err == nil {
			in.IfModifiedSince = aws.Time(t)
		}
	}
	return in
}

func (h *Handler) head(w http.ResponseWriter, r *http.Request, key string) (int, int64) {
	in := h.buildHeadInput(r, key)
	// A HEAD response is always body-less, so its outcome (metadata or a
	// 304/error) is fully shareable — coalesce every HEAD.
	if h.coalesce {
		return h.headCoalesced(w, r, in)
	}
	return h.headDirect(w, r, in)
}

func (h *Handler) headDirect(w http.ResponseWriter, r *http.Request, in *s3.HeadObjectInput) (int, int64) {
	upstreamStart := time.Now()
	out, err := h.store.HeadObject(r.Context(), in)
	h.metrics.ObserveUpstreamLatency(time.Since(upstreamStart).Seconds())
	if err != nil {
		return h.writeError(w, r, err), 0
	}
	return h.writeHeadMeta(w, metaFromHead(out))
}

// writeHeadMeta renders a successful HEAD response from shared metadata.
func (h *Handler) writeHeadMeta(w http.ResponseWriter, meta objectMeta) (int, int64) {
	// For a bodiless handler net/http will not infer Content-Length;
	// writeMeta sets it explicitly from the S3 output.
	writeMeta(w.Header(), meta, h.cacheControl)
	w.WriteHeader(http.StatusOK)
	return http.StatusOK, 0
}

// writeError translates an S3 error into a client response and returns
// the status for logging.
func (h *Handler) writeError(w http.ResponseWriter, r *http.Request, err error) int {
	status, upstream := s3client.Classify(err)
	// 304 is a successful conditional response, not an error; everything
	// else that reaches here (4xx/5xx, plus 499 client-closed) is a
	// non-success upstream outcome worth counting.
	if status != http.StatusNotModified {
		h.metrics.IncUpstreamError(status)
	}
	switch status {
	case s3client.StatusClientClosedRequest:
		// Client is gone; there is nobody to write to.
		return status
	case http.StatusNotModified:
		for _, name := range []string{"ETag", "Last-Modified"} {
			if v := upstream.Get(name); v != "" {
				w.Header().Set(name, v)
			}
		}
		if h.cacheControl != "" {
			w.Header().Set("Cache-Control", h.cacheControl)
		} else if cc := upstream.Get("Cache-Control"); cc != "" {
			w.Header().Set("Cache-Control", cc)
		}
		w.WriteHeader(http.StatusNotModified)
		return status
	}
	if status >= 500 {
		// Server-side observability without leaking upstream details
		// to the client.
		h.logger.LogAttrs(r.Context(), slog.LevelError, "upstream error",
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Any("error", err),
		)
	}
	st, _ := h.fail(w, status)
	return st
}

var errorBodies = map[int]string{
	http.StatusBadRequest:                   "bad request\n",
	http.StatusForbidden:                    "forbidden\n",
	http.StatusNotFound:                     "not found\n",
	http.StatusMethodNotAllowed:             "method not allowed\n",
	http.StatusPreconditionFailed:           "precondition failed\n",
	http.StatusRequestedRangeNotSatisfiable: "range not satisfiable\n",
	http.StatusBadGateway:                   "bad gateway\n",
	http.StatusGatewayTimeout:               "gateway timeout\n",
}

// fail writes a fixed generic error response. Bodies never contain
// upstream error text, request IDs or bucket names, and no-store keeps
// errors out of the CDN cache — except 404, which may opt into brief
// negative caching via NOT_FOUND_CACHE_CONTROL.
func (h *Handler) fail(w http.ResponseWriter, status int) (int, int64) {
	body, ok := errorBodies[status]
	if !ok {
		body = strings.ToLower(http.StatusText(status)) + "\n"
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if status == http.StatusNotFound && h.notFoundCC != "" {
		w.Header().Set("Cache-Control", h.notFoundCC)
	} else {
		w.Header().Set("Cache-Control", "no-store")
	}
	w.WriteHeader(status)
	n, _ := io.WriteString(w, body)
	return status, int64(n)
}

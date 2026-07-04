package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/muhammetsafak/egresszero/internal/config"
)

func coalescingHandler(store ObjectStore, rec Recorder) *Handler {
	return handlerWithRecorder(store, rec, func(c *config.Config) { c.Coalesce = true })
}

// fireConcurrent runs n identical requests (built by mk) concurrently and
// returns their status codes.
func fireConcurrent(h *Handler, n int, mk func() *http.Request) []int {
	codes := make([]int, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			w := httptest.NewRecorder()
			h.ServeHTTP(w, mk())
			codes[i] = w.Code
		}(i)
	}
	wg.Wait()
	return codes
}

// TestCoalesceRevalidation304 is the core case: a burst of identical
// conditional GETs that all resolve to 304 must hit S3 exactly once.
func TestCoalesceRevalidation304(t *testing.T) {
	const followers = 7
	hdr := http.Header{}
	hdr.Set("Etag", `"v1"`)

	var calls atomic.Int32
	entered := make(chan struct{})
	release := make(chan struct{})
	fake := &fakeStore{getFn: func(_ context.Context, _ *s3.GetObjectInput) (*s3.GetObjectOutput, error) {
		calls.Add(1)
		close(entered) // runs once — only the leader executes fn
		<-release
		return nil, notModifiedErr(hdr)
	}}
	rec := &capturingRecorder{}
	h := coalescingHandler(fake, rec)

	mk := func() *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/a", nil)
		r.Header.Set("If-None-Match", `"v1"`)
		return r
	}

	codes := make([]int, followers+1)
	var wg sync.WaitGroup
	// Leader first; once it is confirmed inside fn (holding the
	// singleflight entry), start the followers so they deterministically
	// join instead of racing to become their own leaders.
	wg.Add(1)
	go func() { defer wg.Done(); w := httptest.NewRecorder(); h.ServeHTTP(w, mk()); codes[0] = w.Code }()
	<-entered
	for i := 1; i <= followers; i++ {
		wg.Add(1)
		go func(i int) { defer wg.Done(); w := httptest.NewRecorder(); h.ServeHTTP(w, mk()); codes[i] = w.Code }(i)
	}
	time.Sleep(100 * time.Millisecond) // let followers reach singleflight.Do
	close(release)
	wg.Wait()

	if got := calls.Load(); got != 1 {
		t.Fatalf("store GetObject calls = %d, want 1 (coalesced)", got)
	}
	for i, c := range codes {
		if c != http.StatusNotModified {
			t.Errorf("request %d code = %d, want 304", i, c)
		}
	}
	if rec.coalesced != followers {
		t.Errorf("coalesced count = %d, want %d", rec.coalesced, followers)
	}
}

// TestCoalesceChangedFallback: when the object changed (200), bodies
// cannot be shared, so every waiter fetches and streams its own — S3
// sees one call per request and each gets the full body.
func TestCoalesceChangedFallback(t *testing.T) {
	const n = 6
	body := "the new representation"
	var calls atomic.Int32
	fake := &fakeStore{getFn: func(_ context.Context, _ *s3.GetObjectInput) (*s3.GetObjectOutput, error) {
		calls.Add(1)
		out, _ := getOut(body, nil) // fresh body per call
		return out, nil
	}}
	rec := &capturingRecorder{}
	h := coalescingHandler(fake, rec)

	mk := func() *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/a", nil)
		r.Header.Set("If-None-Match", `"stale"`)
		return r
	}
	// Read bodies too, to assert integrity.
	bodies := make([]string, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			w := httptest.NewRecorder()
			h.ServeHTTP(w, mk())
			if w.Code != http.StatusOK {
				t.Errorf("request %d code = %d, want 200", i, w.Code)
			}
			bodies[i] = w.Body.String()
		}(i)
	}
	wg.Wait()

	if got := calls.Load(); got != n {
		t.Errorf("store calls = %d, want %d (bodies are never shared)", got, n)
	}
	for i, b := range bodies {
		if b != body {
			t.Errorf("request %d body = %q, want %q", i, b, body)
		}
	}
	if rec.coalesced != 0 {
		t.Errorf("coalesced = %d, want 0 (200 bodies are not coalesced)", rec.coalesced)
	}
}

// TestCoalesceDisabled: with COALESCE off, identical revalidations each
// hit S3.
func TestCoalesceDisabled(t *testing.T) {
	const n = 5
	hdr := http.Header{}
	hdr.Set("Etag", `"v1"`)
	var calls atomic.Int32
	fake := &fakeStore{getFn: func(_ context.Context, _ *s3.GetObjectInput) (*s3.GetObjectOutput, error) {
		calls.Add(1)
		return nil, notModifiedErr(hdr)
	}}
	h := testHandler(fake, nil) // Coalesce defaults to false

	codes := fireConcurrent(h, n, func() *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/a", nil)
		r.Header.Set("If-None-Match", `"v1"`)
		return r
	})

	if got := calls.Load(); got != n {
		t.Errorf("store calls = %d, want %d (coalescing disabled)", got, n)
	}
	for i, c := range codes {
		if c != http.StatusNotModified {
			t.Errorf("request %d code = %d, want 304", i, c)
		}
	}
}

// TestCoalesceHead: a burst of identical HEADs hits S3 once and every
// caller renders the shared, body-less metadata.
func TestCoalesceHead(t *testing.T) {
	const followers = 5
	var calls atomic.Int32
	entered := make(chan struct{})
	release := make(chan struct{})
	fake := &fakeStore{headFn: func(_ context.Context, _ *s3.HeadObjectInput) (*s3.HeadObjectOutput, error) {
		calls.Add(1)
		close(entered)
		<-release
		return &s3.HeadObjectOutput{
			ContentLength: aws.Int64(4096),
			ContentType:   aws.String("application/pdf"),
			ETag:          aws.String(`"doc"`),
		}, nil
	}}
	rec := &capturingRecorder{}
	h := coalescingHandler(fake, rec)

	mk := func() *http.Request { return httptest.NewRequest(http.MethodHead, "/doc.pdf", nil) }

	type result struct {
		code   int
		cl, ct string
	}
	results := make([]result, followers+1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		w := httptest.NewRecorder()
		h.ServeHTTP(w, mk())
		results[0] = result{w.Code, w.Header().Get("Content-Length"), w.Header().Get("Content-Type")}
	}()
	<-entered
	for i := 1; i <= followers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			w := httptest.NewRecorder()
			h.ServeHTTP(w, mk())
			results[i] = result{w.Code, w.Header().Get("Content-Length"), w.Header().Get("Content-Type")}
		}(i)
	}
	time.Sleep(100 * time.Millisecond)
	close(release)
	wg.Wait()

	if got := calls.Load(); got != 1 {
		t.Fatalf("store HeadObject calls = %d, want 1 (coalesced)", got)
	}
	for i, r := range results {
		if r.code != http.StatusOK || r.cl != "4096" || r.ct != "application/pdf" {
			t.Errorf("request %d = %+v, want 200/4096/application/pdf", i, r)
		}
	}
	if rec.coalesced != followers {
		t.Errorf("coalesced = %d, want %d", rec.coalesced, followers)
	}
}

// TestCoalesceDetachesClientContext: a coalesced revalidation whose
// client context is already canceled must still complete (the upstream
// call is detached), rather than turning into a 499.
func TestCoalesceDetachesClientContext(t *testing.T) {
	hdr := http.Header{}
	hdr.Set("Etag", `"v1"`)
	fake := &fakeStore{getFn: func(ctx context.Context, _ *s3.GetObjectInput) (*s3.GetObjectOutput, error) {
		if ctx.Err() != nil {
			t.Error("upstream context should be detached from the canceled request")
		}
		return nil, notModifiedErr(hdr)
	}}
	h := coalescingHandler(fake, &capturingRecorder{})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := httptest.NewRequest(http.MethodGet, "/a", nil).WithContext(ctx)
	r.Header.Set("If-None-Match", `"v1"`)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusNotModified {
		t.Fatalf("code = %d, want 304 (detached upstream still ran)", w.Code)
	}
}

// TestCoalescePlainGetStillStreams: a non-conditional GET (cache miss)
// is never coalesced and streams normally with coalescing enabled.
func TestCoalescePlainGetStillStreams(t *testing.T) {
	out, rec := getOut("full body content", nil)
	h := coalescingHandler(&fakeStore{getOut: out}, &capturingRecorder{})

	w := doGet(h, "/file.bin", nil)

	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", w.Code)
	}
	if w.Body.String() != "full body content" {
		t.Errorf("body = %q, want full content", w.Body.String())
	}
	if !rec.closed.Load() {
		t.Error("S3 body was not closed")
	}
}

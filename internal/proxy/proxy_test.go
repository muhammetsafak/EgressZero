package proxy

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/muhammetsafak/egresszero/internal/config"
)

func testHandler(store ObjectStore, mutate func(*config.Config)) *Handler {
	cfg := config.Config{
		Bucket:     "test-bucket",
		AuthHeader: "X-Proxy-Auth",
	}
	if mutate != nil {
		mutate(&cfg)
	}
	return New(store, cfg, slog.New(slog.DiscardHandler))
}

func doGet(h *Handler, target string, header http.Header) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, target, nil)
	for name, values := range header {
		r.Header[name] = values
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestGetSuccessHeaderPassthrough(t *testing.T) {
	lastMod := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	out, rec := getOut("hello world", func(o *s3.GetObjectOutput) {
		o.ContentType = aws.String("image/png")
		o.CacheControl = aws.String("max-age=60")
		o.ContentEncoding = aws.String("gzip")
		o.ContentDisposition = aws.String(`attachment; filename="x.png"`)
		o.ContentLanguage = aws.String("tr")
		o.ExpiresString = aws.String("Thu, 01 Jan 2026 00:00:00 GMT")
		o.LastModified = aws.Time(lastMod)
		o.VersionId = aws.String("v123")
		o.ServerSideEncryption = types.ServerSideEncryptionAes256
	})
	fake := &fakeStore{getOut: out}
	h := testHandler(fake, nil)

	w := doGet(h, "/img/logo.png", nil)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := w.Body.String(); got != "hello world" {
		t.Errorf("body = %q, want %q", got, "hello world")
	}
	want := map[string]string{
		"Content-Type":        "image/png",
		"Content-Length":      "11",
		"ETag":                `"abc123"`,
		"Last-Modified":       "Fri, 02 Jan 2026 03:04:05 GMT",
		"Cache-Control":       "max-age=60",
		"Content-Encoding":    "gzip",
		"Content-Disposition": `attachment; filename="x.png"`,
		"Content-Language":    "tr",
		"Expires":             "Thu, 01 Jan 2026 00:00:00 GMT",
		"Accept-Ranges":       "bytes",
	}
	for name, value := range want {
		if got := w.Header().Get(name); got != value {
			t.Errorf("header %s = %q, want %q", name, got, value)
		}
	}
	for name := range w.Header() {
		if strings.HasPrefix(strings.ToLower(name), "x-amz-") {
			t.Errorf("leaked amz header %s", name)
		}
	}
	if !rec.closed.Load() {
		t.Error("S3 body was not closed")
	}
	if got := aws.ToString(fake.lastGet.Bucket); got != "test-bucket" {
		t.Errorf("bucket = %q, want test-bucket", got)
	}
	if got := aws.ToString(fake.lastGet.Key); got != "img/logo.png" {
		t.Errorf("key = %q, want img/logo.png", got)
	}
}

func TestContentTypeFallback(t *testing.T) {
	out, _ := getOut("data", func(o *s3.GetObjectOutput) { o.ContentType = nil })
	h := testHandler(&fakeStore{getOut: out}, nil)

	w := doGet(h, "/file.bin", nil)
	if got := w.Header().Get("Content-Type"); got != "application/octet-stream" {
		t.Errorf("Content-Type = %q, want application/octet-stream", got)
	}
}

func TestCacheControlOverride(t *testing.T) {
	t.Run("replaces upstream value", func(t *testing.T) {
		out, _ := getOut("x", func(o *s3.GetObjectOutput) {
			o.CacheControl = aws.String("private, no-cache")
		})
		h := testHandler(&fakeStore{getOut: out}, func(c *config.Config) {
			c.CacheControl = "public, max-age=31536000"
		})
		w := doGet(h, "/a", nil)
		if got := w.Header().Get("Cache-Control"); got != "public, max-age=31536000" {
			t.Errorf("Cache-Control = %q, want override", got)
		}
	})
	t.Run("applies when upstream has none", func(t *testing.T) {
		out, _ := getOut("x", nil)
		h := testHandler(&fakeStore{getOut: out}, func(c *config.Config) {
			c.CacheControl = "public, max-age=600"
		})
		w := doGet(h, "/a", nil)
		if got := w.Header().Get("Cache-Control"); got != "public, max-age=600" {
			t.Errorf("Cache-Control = %q, want override", got)
		}
	})
	t.Run("passes upstream through without override", func(t *testing.T) {
		out, _ := getOut("x", func(o *s3.GetObjectOutput) {
			o.CacheControl = aws.String("max-age=120")
		})
		h := testHandler(&fakeStore{getOut: out}, nil)
		w := doGet(h, "/a", nil)
		if got := w.Header().Get("Cache-Control"); got != "max-age=120" {
			t.Errorf("Cache-Control = %q, want max-age=120", got)
		}
	})
}

func TestGetRange(t *testing.T) {
	out, _ := getOut("abcd", func(o *s3.GetObjectOutput) {
		o.ContentRange = aws.String("bytes 0-3/26")
	})
	fake := &fakeStore{getOut: out}
	h := testHandler(fake, nil)

	w := doGet(h, "/big.bin", http.Header{"Range": []string{"bytes=0-3"}})

	if w.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", w.Code)
	}
	if got := w.Header().Get("Content-Range"); got != "bytes 0-3/26" {
		t.Errorf("Content-Range = %q, want bytes 0-3/26", got)
	}
	if got := aws.ToString(fake.lastGet.Range); got != "bytes=0-3" {
		t.Errorf("forwarded Range = %q, want bytes=0-3", got)
	}
	if got := w.Body.String(); got != "abcd" {
		t.Errorf("body = %q, want abcd", got)
	}
}

func TestIfRange(t *testing.T) {
	t.Run("matching etag forwards IfMatch and serves 206", func(t *testing.T) {
		out, _ := getOut("abcd", func(o *s3.GetObjectOutput) {
			o.ContentRange = aws.String("bytes 0-3/26")
		})
		fake := &fakeStore{getOut: out}
		h := testHandler(fake, nil)

		w := doGet(h, "/big.bin", http.Header{
			"Range":    []string{"bytes=0-3"},
			"If-Range": []string{`"abc123"`},
		})

		if w.Code != http.StatusPartialContent {
			t.Fatalf("status = %d, want 206", w.Code)
		}
		if got := aws.ToString(fake.lastGet.IfMatch); got != `"abc123"` {
			t.Errorf("IfMatch = %q, want forwarded etag", got)
		}
		if fake.lastGet.IfUnmodifiedSince != nil {
			t.Error("IfUnmodifiedSince should be unset for an etag If-Range")
		}
	})

	t.Run("stale etag retries without range and serves full 200", func(t *testing.T) {
		full := "abcdefghijklmnopqrstuvwxyz"
		var calls int
		var sawRangeOnRetry bool
		fake := &fakeStore{getFn: func(_ context.Context, in *s3.GetObjectInput) (*s3.GetObjectOutput, error) {
			calls++
			if calls == 1 {
				// First (ranged) call: validator no longer matches.
				return nil, sdkResponseErr("GetObject", http.StatusPreconditionFailed, nil)
			}
			sawRangeOnRetry = in.Range != nil || in.IfMatch != nil
			out, _ := getOut(full, nil)
			return out, nil
		}}
		h := testHandler(fake, nil)

		w := doGet(h, "/big.bin", http.Header{
			"Range":    []string{"bytes=0-3"},
			"If-Range": []string{`"stale"`},
		})

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (full body)", w.Code)
		}
		if calls != 2 {
			t.Fatalf("GetObject called %d times, want 2 (ranged then full)", calls)
		}
		if sawRangeOnRetry {
			t.Error("retry must drop Range and IfMatch")
		}
		if got := w.Body.String(); got != full {
			t.Errorf("body = %q, want full representation", got)
		}
		if got := w.Header().Get("Content-Range"); got != "" {
			t.Errorf("Content-Range = %q, want empty on full-body fallback", got)
		}
	})

	t.Run("date form forwards IfUnmodifiedSince", func(t *testing.T) {
		out, _ := getOut("abcd", func(o *s3.GetObjectOutput) {
			o.ContentRange = aws.String("bytes 0-3/26")
		})
		fake := &fakeStore{getOut: out}
		h := testHandler(fake, nil)

		doGet(h, "/big.bin", http.Header{
			"Range":    []string{"bytes=0-3"},
			"If-Range": []string{"Fri, 02 Jan 2026 03:04:05 GMT"},
		})

		if fake.lastGet.IfUnmodifiedSince == nil ||
			!fake.lastGet.IfUnmodifiedSince.Equal(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)) {
			t.Errorf("IfUnmodifiedSince = %v, want parsed date", fake.lastGet.IfUnmodifiedSince)
		}
		if fake.lastGet.IfMatch != nil {
			t.Error("IfMatch should be unset for a date If-Range")
		}
	})

	t.Run("ignored without a Range header", func(t *testing.T) {
		out, _ := getOut("x", nil)
		fake := &fakeStore{getOut: out}
		h := testHandler(fake, nil)

		w := doGet(h, "/a", http.Header{"If-Range": []string{`"abc123"`}})

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
		if fake.lastGet.IfMatch != nil || fake.lastGet.IfUnmodifiedSince != nil {
			t.Error("If-Range must be ignored when no Range is present")
		}
	})

	t.Run("malformed value is ignored and range is served", func(t *testing.T) {
		out, _ := getOut("abcd", func(o *s3.GetObjectOutput) {
			o.ContentRange = aws.String("bytes 0-3/26")
		})
		fake := &fakeStore{getOut: out}
		h := testHandler(fake, nil)

		w := doGet(h, "/big.bin", http.Header{
			"Range":    []string{"bytes=0-3"},
			"If-Range": []string{"not-an-etag-or-date"},
		})

		if w.Code != http.StatusPartialContent {
			t.Fatalf("status = %d, want 206", w.Code)
		}
		if fake.lastGet.IfMatch != nil || fake.lastGet.IfUnmodifiedSince != nil {
			t.Error("malformed If-Range must not set any precondition")
		}
		if got := aws.ToString(fake.lastGet.Range); got != "bytes=0-3" {
			t.Errorf("Range = %q, want forwarded", got)
		}
	})
}

func TestConditionalHeadersForwarded(t *testing.T) {
	out, _ := getOut("x", nil)
	fake := &fakeStore{getOut: out}
	h := testHandler(fake, nil)

	doGet(h, "/a", http.Header{
		"If-None-Match":     []string{`"abc123"`},
		"If-Modified-Since": []string{"Fri, 02 Jan 2026 03:04:05 GMT"},
	})

	if got := aws.ToString(fake.lastGet.IfNoneMatch); got != `"abc123"` {
		t.Errorf("IfNoneMatch = %q, want quoted etag", got)
	}
	if fake.lastGet.IfModifiedSince == nil ||
		!fake.lastGet.IfModifiedSince.Equal(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)) {
		t.Errorf("IfModifiedSince = %v, want parsed date", fake.lastGet.IfModifiedSince)
	}
}

func TestMalformedIfModifiedSinceIgnored(t *testing.T) {
	out, _ := getOut("x", nil)
	fake := &fakeStore{getOut: out}
	h := testHandler(fake, nil)

	w := doGet(h, "/a", http.Header{"If-Modified-Since": []string{"yesterday-ish"}})

	if fake.lastGet.IfModifiedSince != nil {
		t.Errorf("IfModifiedSince = %v, want nil", fake.lastGet.IfModifiedSince)
	}
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
}

func TestGetNotModified(t *testing.T) {
	hdr := http.Header{}
	hdr.Set("Etag", `"abc123"`)
	hdr.Set("Last-Modified", "Fri, 02 Jan 2026 03:04:05 GMT")
	hdr.Set("Cache-Control", "max-age=300")

	t.Run("echoes upstream validators", func(t *testing.T) {
		h := testHandler(&fakeStore{getErr: notModifiedErr(hdr)}, nil)
		w := doGet(h, "/a", http.Header{"If-None-Match": []string{`"abc123"`}})

		if w.Code != http.StatusNotModified {
			t.Fatalf("status = %d, want 304", w.Code)
		}
		if got := w.Header().Get("ETag"); got != `"abc123"` {
			t.Errorf("ETag = %q, want echoed", got)
		}
		if got := w.Header().Get("Last-Modified"); got != "Fri, 02 Jan 2026 03:04:05 GMT" {
			t.Errorf("Last-Modified = %q, want echoed", got)
		}
		if got := w.Header().Get("Cache-Control"); got != "max-age=300" {
			t.Errorf("Cache-Control = %q, want upstream value", got)
		}
		if w.Body.Len() != 0 {
			t.Errorf("body length = %d, want 0", w.Body.Len())
		}
	})
	t.Run("override wins on 304", func(t *testing.T) {
		h := testHandler(&fakeStore{getErr: notModifiedErr(hdr)}, func(c *config.Config) {
			c.CacheControl = "public, max-age=31536000"
		})
		w := doGet(h, "/a", http.Header{"If-None-Match": []string{`"abc123"`}})
		if got := w.Header().Get("Cache-Control"); got != "public, max-age=31536000" {
			t.Errorf("Cache-Control = %q, want override", got)
		}
	})
}

func TestErrorMapping(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
	}{
		{"NoSuchKey", sdkAPIErr("GetObject", "NoSuchKey"), http.StatusNotFound},
		{"404 response", sdkResponseErr("GetObject", 404, nil), http.StatusNotFound},
		{"AccessDenied", sdkAPIErr("GetObject", "AccessDenied"), http.StatusForbidden},
		{"InvalidRange", sdkAPIErr("GetObject", "InvalidRange"), http.StatusRequestedRangeNotSatisfiable},
		{"opaque", errors.New("dial tcp: connection refused"), http.StatusBadGateway},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := testHandler(&fakeStore{getErr: tt.err}, nil)
			w := doGet(h, "/secret-bucket-path/a", nil)

			if w.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", w.Code, tt.wantStatus)
			}
			if got := w.Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store", got)
			}
			body := w.Body.String()
			for _, leak := range []string{"NoSuchKey", "AccessDenied", "test-bucket", "S3", "api error", "connection refused"} {
				if strings.Contains(body, leak) {
					t.Errorf("body %q leaks %q", body, leak)
				}
			}
		})
	}
}

func TestNotFoundNegativeCaching(t *testing.T) {
	withNegCache := func(c *config.Config) { c.NotFoundCacheControl = "public, max-age=60" }

	t.Run("404 carries configured Cache-Control", func(t *testing.T) {
		h := testHandler(&fakeStore{getErr: sdkAPIErr("GetObject", "NoSuchKey")}, withNegCache)
		w := doGet(h, "/missing", nil)
		if w.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", w.Code)
		}
		if got := w.Header().Get("Cache-Control"); got != "public, max-age=60" {
			t.Errorf("Cache-Control = %q, want configured value", got)
		}
	})
	t.Run("root 404 also carries it", func(t *testing.T) {
		h := testHandler(&fakeStore{}, withNegCache)
		w := doGet(h, "/", nil)
		if got := w.Header().Get("Cache-Control"); got != "public, max-age=60" {
			t.Errorf("Cache-Control = %q, want configured value", got)
		}
	})
	t.Run("other errors stay no-store", func(t *testing.T) {
		for name, err := range map[string]error{
			"403": sdkAPIErr("GetObject", "AccessDenied"),
			"416": sdkAPIErr("GetObject", "InvalidRange"),
			"502": errors.New("upstream exploded"),
		} {
			h := testHandler(&fakeStore{getErr: err}, withNegCache)
			w := doGet(h, "/a", nil)
			if got := w.Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("%s: Cache-Control = %q, want no-store", name, got)
			}
		}
	})
	t.Run("auth failure stays no-store", func(t *testing.T) {
		h := testHandler(&fakeStore{}, func(c *config.Config) {
			withNegCache(c)
			c.AuthSecret = "hunter2"
		})
		w := doGet(h, "/a", nil)
		if w.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", w.Code)
		}
		if got := w.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("Cache-Control = %q, want no-store", got)
		}
	})
	t.Run("disabled by default", func(t *testing.T) {
		h := testHandler(&fakeStore{getErr: sdkAPIErr("GetObject", "NoSuchKey")}, nil)
		w := doGet(h, "/missing", nil)
		if got := w.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("Cache-Control = %q, want no-store", got)
		}
	})
}

func TestMethodNotAllowed(t *testing.T) {
	h := testHandler(&fakeStore{}, nil)
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodOptions, http.MethodPatch} {
		r := httptest.NewRequest(method, "/a", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)

		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s status = %d, want 405", method, w.Code)
		}
		if got := w.Header().Get("Allow"); got != "GET, HEAD" {
			t.Errorf("%s Allow = %q, want GET, HEAD", method, got)
		}
	}
}

func TestKeyDerivation(t *testing.T) {
	tests := []struct {
		name       string
		target     string
		prefix     string
		wantStatus int
		wantKey    string
	}{
		{"root is 404", "/", "", http.StatusNotFound, ""},
		{"percent decoding", "/a%20b.txt", "", http.StatusOK, "a b.txt"},
		{"plus is literal", "/a+b.txt", "", http.StatusOK, "a+b.txt"},
		{"prefix prepended", "/img/x.png", "assets/", http.StatusOK, "assets/img/x.png"},
		{"dotdot rejected", "/../secret", "", http.StatusBadRequest, ""},
		{"inner dotdot rejected", "/a/../b", "", http.StatusBadRequest, ""},
		{"dot segment rejected", "/a/./b", "", http.StatusBadRequest, ""},
		{"double slashes preserved", "/a//b", "", http.StatusOK, "a//b"},
		{"trailing slash preserved", "/dir/", "", http.StatusOK, "dir/"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, _ := getOut("x", nil)
			fake := &fakeStore{getOut: out}
			h := testHandler(fake, func(c *config.Config) { c.KeyPrefix = tt.prefix })

			w := doGet(h, tt.target, nil)

			if w.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", w.Code, tt.wantStatus)
			}
			if tt.wantStatus == http.StatusOK {
				if got := aws.ToString(fake.lastGet.Key); got != tt.wantKey {
					t.Errorf("key = %q, want %q", got, tt.wantKey)
				}
			} else if fake.getCalls != 0 {
				t.Errorf("GetObject called %d times, want 0", fake.getCalls)
			}
		})
	}
}

func TestAuth(t *testing.T) {
	newAuthed := func() (*Handler, *fakeStore) {
		out, _ := getOut("x", nil)
		fake := &fakeStore{getOut: out}
		return testHandler(fake, func(c *config.Config) { c.AuthSecret = "hunter2" }), fake
	}

	t.Run("missing header rejected", func(t *testing.T) {
		h, fake := newAuthed()
		w := doGet(h, "/a", nil)
		if w.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", w.Code)
		}
		if fake.getCalls != 0 {
			t.Error("S3 must not be called for unauthenticated requests")
		}
	})
	t.Run("wrong secret rejected", func(t *testing.T) {
		h, _ := newAuthed()
		w := doGet(h, "/a", http.Header{"X-Proxy-Auth": []string{"hunter3"}})
		if w.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", w.Code)
		}
	})
	t.Run("correct secret accepted", func(t *testing.T) {
		h, _ := newAuthed()
		w := doGet(h, "/a", http.Header{"X-Proxy-Auth": []string{"hunter2"}})
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
	})
	t.Run("healthz exempt", func(t *testing.T) {
		h, _ := newAuthed()
		w := doGet(h, "/healthz", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
		if got := w.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("Cache-Control = %q, want no-store", got)
		}
	})
	t.Run("disabled auth requires nothing", func(t *testing.T) {
		out, _ := getOut("x", nil)
		h := testHandler(&fakeStore{getOut: out}, nil)
		w := doGet(h, "/a", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
	})
}

func TestHead(t *testing.T) {
	lastMod := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	newHead := func() (*Handler, *fakeStore) {
		fake := &fakeStore{headOut: &s3.HeadObjectOutput{
			ContentLength: aws.Int64(1234),
			ContentType:   aws.String("video/mp4"),
			ETag:          aws.String(`"vid"`),
			LastModified:  aws.Time(lastMod),
		}}
		return testHandler(fake, nil), fake
	}

	t.Run("uses HeadObject and sets Content-Length", func(t *testing.T) {
		h, fake := newHead()
		r := httptest.NewRequest(http.MethodHead, "/v.mp4", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
		if fake.headCalls != 1 || fake.getCalls != 0 {
			t.Errorf("headCalls=%d getCalls=%d, want 1/0", fake.headCalls, fake.getCalls)
		}
		if got := w.Header().Get("Content-Length"); got != "1234" {
			t.Errorf("Content-Length = %q, want 1234", got)
		}
		if w.Body.Len() != 0 {
			t.Errorf("HEAD body length = %d, want 0", w.Body.Len())
		}
		if got := aws.ToString(fake.lastHead.Key); got != "v.mp4" {
			t.Errorf("key = %q, want v.mp4", got)
		}
	})
	t.Run("404 via types.NotFound", func(t *testing.T) {
		h := testHandler(&fakeStore{headErr: &types.NotFound{}}, nil)
		r := httptest.NewRequest(http.MethodHead, "/missing", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", w.Code)
		}
	})
	t.Run("304 quirk", func(t *testing.T) {
		hdr := http.Header{}
		hdr.Set("Etag", `"vid"`)
		h := testHandler(&fakeStore{headErr: sdkResponseErr("HeadObject", 304, hdr)}, nil)
		r := httptest.NewRequest(http.MethodHead, "/v.mp4", nil)
		r.Header.Set("If-None-Match", `"vid"`)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusNotModified {
			t.Fatalf("status = %d, want 304", w.Code)
		}
		if got := w.Header().Get("ETag"); got != `"vid"` {
			t.Errorf("ETag = %q, want echoed", got)
		}
	})
}

// nothingWriter asserts that the handler writes absolutely nothing.
type nothingWriter struct {
	t      *testing.T
	header http.Header
}

func (n *nothingWriter) Header() http.Header { return n.header }
func (n *nothingWriter) WriteHeader(status int) {
	n.t.Errorf("WriteHeader(%d) called, want no write at all", status)
}
func (n *nothingWriter) Write(p []byte) (int, error) {
	n.t.Errorf("Write(%d bytes) called, want no write at all", len(p))
	return len(p), nil
}

func TestClientDisconnectedWritesNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := httptest.NewRequest(http.MethodGet, "/a", nil).WithContext(ctx)
	h := testHandler(&fakeStore{}, nil)

	h.ServeHTTP(&nothingWriter{t: t, header: http.Header{}}, r)
}

// failAfterWriter accepts headers but fails body writes, simulating a
// client that vanished mid-download.
type failAfterWriter struct {
	header http.Header
	status int
}

func (f *failAfterWriter) Header() http.Header    { return f.header }
func (f *failAfterWriter) WriteHeader(status int) { f.status = status }
func (f *failAfterWriter) Write(p []byte) (int, error) {
	return 0, errors.New("broken pipe")
}

func TestBodyClosedWhenStreamAborts(t *testing.T) {
	out, rec := getOut("payload that will never arrive", nil)
	h := testHandler(&fakeStore{getOut: out}, nil)
	r := httptest.NewRequest(http.MethodGet, "/a", nil)
	w := &failAfterWriter{header: http.Header{}}

	h.ServeHTTP(w, r)

	if w.status != http.StatusOK {
		t.Errorf("status = %d, want 200 (headers sent before the pipe broke)", w.status)
	}
	if !rec.closed.Load() {
		t.Error("S3 body was not closed after aborted stream")
	}
}

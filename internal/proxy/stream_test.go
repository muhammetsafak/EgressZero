package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// zeroReader yields an endless stream of zero bytes without allocating.
type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

func syntheticStore(objectSize int64) *fakeStore {
	return &fakeStore{
		getFn: func(_ context.Context, _ *s3.GetObjectInput) (*s3.GetObjectOutput, error) {
			return &s3.GetObjectOutput{
				Body:          io.NopCloser(io.LimitReader(zeroReader{}, objectSize)),
				ContentLength: aws.Int64(objectSize),
				ContentType:   aws.String("application/octet-stream"),
				ETag:          aws.String(`"zeros"`),
			}, nil
		},
	}
}

// TestMemoryCeiling proves the proxy streams instead of buffering: 50
// concurrent 256 MB downloads held mid-flight must not grow the heap by
// more than 20 MB.
func TestMemoryCeiling(t *testing.T) {
	if testing.Short() {
		t.Skip("memory ceiling test skipped in -short mode")
	}

	const (
		streams       = 50
		objectSize    = int64(256 << 20)
		readPerStream = int64(32 << 20)
		heapBudget    = uint64(20 << 20)
	)

	h := testHandler(syntheticStore(objectSize), nil)
	ts := httptest.NewServer(h)
	defer ts.Close()

	runtime.GC()
	var base runtime.MemStats
	runtime.ReadMemStats(&base)

	var (
		wg      sync.WaitGroup
		midway  atomic.Int32
		release = make(chan struct{})
	)
	for i := range streams {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.Get(fmt.Sprintf("%s/obj-%d.bin", ts.URL, i))
			if err != nil {
				t.Errorf("GET failed: %v", err)
				midway.Add(1)
				return
			}
			defer resp.Body.Close()
			if _, err := io.CopyN(io.Discard, resp.Body, readPerStream); err != nil {
				t.Errorf("read failed: %v", err)
			}
			// Hold the stream open mid-flight so main can measure the
			// heap while all downloads are simultaneously active.
			midway.Add(1)
			<-release
		}()
	}

	deadline := time.Now().Add(60 * time.Second)
	for midway.Load() < streams {
		if time.Now().After(deadline) {
			close(release)
			t.Fatal("streams did not reach midway in time")
		}
		time.Sleep(10 * time.Millisecond)
	}

	runtime.GC()
	var cur runtime.MemStats
	runtime.ReadMemStats(&cur)
	close(release)
	wg.Wait()

	var growth uint64
	if cur.HeapAlloc > base.HeapAlloc {
		growth = cur.HeapAlloc - base.HeapAlloc
	}
	t.Logf("heap growth with %d concurrent %d MB streams: %.1f MB",
		streams, objectSize>>20, float64(growth)/(1<<20))
	if growth > heapBudget {
		t.Errorf("heap grew %d bytes, budget %d — proxy is buffering bodies", growth, heapBudget)
	}
}

// discardWriter is a minimal ResponseWriter that throws the body away.
type discardWriter struct{ header http.Header }

func (d *discardWriter) Header() http.Header         { return d.header }
func (d *discardWriter) WriteHeader(int)             {}
func (d *discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// BenchmarkGET shows allocations per request stay flat regardless of
// object size (run with -benchmem).
func BenchmarkGET(b *testing.B) {
	for _, size := range []int64{1 << 20, 64 << 20} {
		b.Run(fmt.Sprintf("%dMB", size>>20), func(b *testing.B) {
			h := testHandler(syntheticStore(size), nil)
			b.SetBytes(size)
			b.ReportAllocs()
			for b.Loop() {
				r := httptest.NewRequest(http.MethodGet, "/obj.bin", nil)
				h.ServeHTTP(&discardWriter{header: http.Header{}}, r)
			}
		})
	}
}

package proxy

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/muhammetsafak/egresszero/internal/config"
)

func trackedSyntheticStore(objectSize int64) (*fakeStore, *closeRecorder) {
	rec := &closeRecorder{Reader: io.LimitReader(zeroReader{}, objectSize)}
	fake := &fakeStore{getOut: &s3.GetObjectOutput{
		Body:          rec,
		ContentLength: aws.Int64(objectSize),
		ContentType:   aws.String("application/octet-stream"),
		ETag:          aws.String(`"zeros"`),
	}}
	return fake, rec
}

// rawGet opens a plain TCP connection and sends a GET so the test
// controls exactly how fast the response is consumed.
func rawGet(t *testing.T, ts *httptest.Server, path string) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", ts.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: test\r\n\r\n", path)
	return conn
}

// TestStalledClientDisconnected: a client that stops reading must be
// cut loose after the rolling idle window — the S3 body must be closed
// instead of pinning a goroutine and connection forever.
func TestStalledClientDisconnected(t *testing.T) {
	fake, rec := trackedSyntheticStore(256 << 20)
	h := testHandler(fake, func(c *config.Config) {
		c.WriteIdleTimeout = 300 * time.Millisecond
	})
	ts := httptest.NewServer(h)
	defer ts.Close()

	conn := rawGet(t, ts, "/obj.bin")
	if _, err := io.ReadFull(conn, make([]byte, 64<<10)); err != nil {
		t.Fatalf("initial read: %v", err)
	}
	// Stall: stop reading entirely. Kernel and server buffers fill,
	// the server's writes block, the deadline fires.
	deadline := time.Now().Add(10 * time.Second)
	for !rec.closed.Load() {
		if time.Now().After(deadline) {
			t.Fatal("S3 body still open 10s after the client stalled; write deadline did not fire")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestSlowClientSurvives: a client that keeps draining, however slowly
// relative to the idle window, must receive the complete body.
func TestSlowClientSurvives(t *testing.T) {
	const size = int64(32 << 20)
	fake, rec := trackedSyntheticStore(size)
	h := testHandler(fake, func(c *config.Config) {
		c.WriteIdleTimeout = 500 * time.Millisecond
	})
	ts := httptest.NewServer(h)
	defer ts.Close()

	conn := rawGet(t, ts, "/obj.bin")
	req, _ := http.NewRequest(http.MethodGet, "/obj.bin", nil)
	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer resp.Body.Close()

	// Drain in small sips over several idle windows (~3s total).
	buf := make([]byte, 256<<10)
	var total int64
	for {
		n, err := io.ReadFull(resp.Body, buf)
		total += int64(n)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				break
			}
			t.Fatalf("read after %d bytes: %v", total, err)
		}
		time.Sleep(25 * time.Millisecond)
	}
	if total != size {
		t.Fatalf("received %d bytes, want %d — slow client was cut off", total, size)
	}
	if !rec.closed.Load() {
		t.Error("S3 body was not closed after successful stream")
	}
}

// TestDeadlineUnsupportedWriterFallsBack: writers without deadline
// support (recorders, exotic middleware) must still stream fine.
func TestDeadlineUnsupportedWriterFallsBack(t *testing.T) {
	out, rec := getOut("payload", nil)
	h := testHandler(&fakeStore{getOut: out}, func(c *config.Config) {
		c.WriteIdleTimeout = time.Minute
	})

	w := doGet(h, "/a", nil)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := w.Body.String(); got != "payload" {
		t.Errorf("body = %q, want full payload", got)
	}
	if !rec.closed.Load() {
		t.Error("S3 body was not closed")
	}
}

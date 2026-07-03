package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

// fakeStore implements ObjectStore for tests. It records the last
// inputs and honours context cancellation like the real SDK does.
type fakeStore struct {
	mu        sync.Mutex
	getFn     func(ctx context.Context, in *s3.GetObjectInput) (*s3.GetObjectOutput, error)
	getOut    *s3.GetObjectOutput
	getErr    error
	headOut   *s3.HeadObjectOutput
	headErr   error
	lastGet   *s3.GetObjectInput
	lastHead  *s3.HeadObjectInput
	getCalls  int
	headCalls int
}

func (f *fakeStore) GetObject(ctx context.Context, in *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	f.mu.Lock()
	f.lastGet = in
	f.getCalls++
	f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("operation error S3: GetObject: %w", err)
	}
	if f.getFn != nil {
		return f.getFn(ctx, in)
	}
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.getOut, nil
}

func (f *fakeStore) HeadObject(ctx context.Context, in *s3.HeadObjectInput, _ ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	f.mu.Lock()
	f.lastHead = in
	f.headCalls++
	f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("operation error S3: HeadObject: %w", err)
	}
	if f.headErr != nil {
		return nil, f.headErr
	}
	return f.headOut, nil
}

// closeRecorder wraps a body reader and records whether Close ran.
type closeRecorder struct {
	io.Reader
	closed atomic.Bool
}

func (c *closeRecorder) Close() error {
	c.closed.Store(true)
	return nil
}

// getOut builds a GetObjectOutput with the given body and sensible
// metadata; mutate tweaks it per test.
func getOut(body string, mutate func(*s3.GetObjectOutput)) (*s3.GetObjectOutput, *closeRecorder) {
	rec := &closeRecorder{Reader: strings.NewReader(body)}
	out := &s3.GetObjectOutput{
		Body:          rec,
		ContentLength: aws.Int64(int64(len(body))),
		ContentType:   aws.String("text/plain"),
		ETag:          aws.String(`"abc123"`),
	}
	if mutate != nil {
		mutate(out)
	}
	return out, rec
}

// sdkResponseErr builds the error shape the SDK produces when S3
// answers with a bare HTTP status.
func sdkResponseErr(operation string, status int, hdr http.Header) error {
	return &smithy.OperationError{
		ServiceID:     "S3",
		OperationName: operation,
		Err: &awshttp.ResponseError{
			ResponseError: &smithyhttp.ResponseError{
				Response: &smithyhttp.Response{
					Response: &http.Response{StatusCode: status, Header: hdr},
				},
				Err: fmt.Errorf("api error %d", status),
			},
		},
	}
}

// notModifiedErr is the SDK's representation of a satisfied conditional
// request: an error carrying the upstream 304 response.
func notModifiedErr(hdr http.Header) error {
	return sdkResponseErr("GetObject", http.StatusNotModified, hdr)
}

func sdkAPIErr(operation, code string) error {
	return &smithy.OperationError{
		ServiceID:     "S3",
		OperationName: operation,
		Err:           &smithy.GenericAPIError{Code: code, Message: code},
	}
}

package s3client

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

// responseErr builds the error shape the SDK produces when S3 answers
// with a bare HTTP status: an operation error wrapping a transport
// response error.
func responseErr(status int, hdr http.Header) error {
	return &smithy.OperationError{
		ServiceID:     "S3",
		OperationName: "GetObject",
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

func apiErr(code string) error {
	return &smithy.OperationError{
		ServiceID:     "S3",
		OperationName: "GetObject",
		Err:           &smithy.GenericAPIError{Code: code, Message: code},
	}
}

func TestClassify(t *testing.T) {
	hdr := http.Header{"Etag": []string{`"abc"`}}

	tests := []struct {
		name string
		err  error
		want int
	}{
		{"304 response", responseErr(304, hdr), http.StatusNotModified},
		{"403 response", responseErr(403, nil), http.StatusForbidden},
		{"404 response", responseErr(404, nil), http.StatusNotFound},
		{"412 response", responseErr(412, nil), http.StatusPreconditionFailed},
		{"416 response", responseErr(416, nil), http.StatusRequestedRangeNotSatisfiable},
		{"NoSuchKey code", apiErr("NoSuchKey"), http.StatusNotFound},
		{"NoSuchBucket code", apiErr("NoSuchBucket"), http.StatusNotFound},
		{"AccessDenied code", apiErr("AccessDenied"), http.StatusForbidden},
		{"InvalidRange code", apiErr("InvalidRange"), http.StatusRequestedRangeNotSatisfiable},
		{"typed NoSuchKey", &types.NoSuchKey{}, http.StatusNotFound},
		{"typed NotFound from HEAD", &types.NotFound{}, http.StatusNotFound},
		{"context canceled", fmt.Errorf("operation error: %w", context.Canceled), StatusClientClosedRequest},
		{"deadline exceeded", fmt.Errorf("operation error: %w", context.DeadlineExceeded), http.StatusGatewayTimeout},
		{"unknown error", errors.New("connection refused"), http.StatusBadGateway},
		{"unknown api code", apiErr("SlowDown"), http.StatusBadGateway},
		{"5xx response", responseErr(500, nil), http.StatusBadGateway},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, upstream := Classify(tt.err)
			if status != tt.want {
				t.Fatalf("Classify() status = %d, want %d", status, tt.want)
			}
			if tt.want == http.StatusNotModified {
				if got := upstream.Get("ETag"); got != `"abc"` {
					t.Errorf("upstream ETag = %q, want %q", got, `"abc"`)
				}
			} else if upstream != nil {
				t.Errorf("upstream headers = %v, want nil", upstream)
			}
		})
	}
}

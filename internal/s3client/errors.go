package s3client

import (
	"context"
	"errors"
	"net/http"

	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/smithy-go"
)

// StatusClientClosedRequest is nginx's non-standard 499: the client went
// away mid-request. It is a sentinel for "write nothing", never sent on
// the wire.
const StatusClientClosedRequest = 499

// Classify maps an aws-sdk-go-v2 error to the HTTP status the proxy
// should answer with. For 304 it also returns the upstream response
// headers so ETag/Last-Modified can be echoed to the client.
//
// The 304 case exists because the SDK surfaces a satisfied conditional
// GET/HEAD (If-None-Match match, If-Modified-Since fresh) as an error,
// not as a normal output.
func Classify(err error) (status int, upstream http.Header) {
	if errors.Is(err, context.Canceled) {
		return StatusClientClosedRequest, nil
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return http.StatusGatewayTimeout, nil
	}

	var respErr *awshttp.ResponseError
	if errors.As(err, &respErr) {
		switch respErr.HTTPStatusCode() {
		case http.StatusNotModified:
			return http.StatusNotModified, respErr.Response.Header
		case http.StatusForbidden:
			return http.StatusForbidden, nil
		case http.StatusNotFound:
			return http.StatusNotFound, nil
		case http.StatusPreconditionFailed:
			return http.StatusPreconditionFailed, nil
		case http.StatusRequestedRangeNotSatisfiable:
			return http.StatusRequestedRangeNotSatisfiable, nil
		}
	}

	// Fallback by error code, for errors that carry no HTTP response
	// (notably HeadObject 404s, which arrive as types.NotFound because
	// S3 sends no error body on HEAD).
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NoSuchKey", "NoSuchBucket", "NotFound":
			return http.StatusNotFound, nil
		case "AccessDenied":
			return http.StatusForbidden, nil
		case "InvalidRange":
			return http.StatusRequestedRangeNotSatisfiable, nil
		case "PreconditionFailed":
			return http.StatusPreconditionFailed, nil
		}
	}

	return http.StatusBadGateway, nil
}

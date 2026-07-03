package proxy

import (
	"net/http"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// objectMeta is the allowlisted subset of S3 object metadata the proxy
// forwards to clients. Anything outside this struct (x-amz-*, SSE
// headers, version IDs, user metadata) never reaches a response.
type objectMeta struct {
	contentType        *string
	contentLength      *int64
	etag               *string
	lastModified       *time.Time
	cacheControl       *string
	contentEncoding    *string
	contentDisposition *string
	contentLanguage    *string
	expires            *string
	contentRange       *string
}

func metaFromGet(out *s3.GetObjectOutput) objectMeta {
	return objectMeta{
		contentType:        out.ContentType,
		contentLength:      out.ContentLength,
		etag:               out.ETag,
		lastModified:       out.LastModified,
		cacheControl:       out.CacheControl,
		contentEncoding:    out.ContentEncoding,
		contentDisposition: out.ContentDisposition,
		contentLanguage:    out.ContentLanguage,
		expires:            out.ExpiresString,
		contentRange:       out.ContentRange,
	}
}

func metaFromHead(out *s3.HeadObjectOutput) objectMeta {
	return objectMeta{
		contentType:        out.ContentType,
		contentLength:      out.ContentLength,
		etag:               out.ETag,
		lastModified:       out.LastModified,
		cacheControl:       out.CacheControl,
		contentEncoding:    out.ContentEncoding,
		contentDisposition: out.ContentDisposition,
		contentLanguage:    out.ContentLanguage,
		expires:            out.ExpiresString,
	}
}

// writeMeta sets response headers from m. ccOverride, when non-empty,
// replaces any upstream Cache-Control.
func writeMeta(h http.Header, m objectMeta, ccOverride string) {
	// An explicit fallback Content-Type also suppresses Go's content
	// sniffing on the first body write.
	if m.contentType != nil && *m.contentType != "" {
		h.Set("Content-Type", *m.contentType)
	} else {
		h.Set("Content-Type", "application/octet-stream")
	}
	// Content-Length is set explicitly so responses never fall back to
	// chunked encoding; Cloudflare needs an honest length to cache
	// large files.
	if m.contentLength != nil {
		h.Set("Content-Length", strconv.FormatInt(*m.contentLength, 10))
	}
	setIf(h, "ETag", m.etag)
	if m.lastModified != nil {
		h.Set("Last-Modified", m.lastModified.UTC().Format(http.TimeFormat))
	}
	if ccOverride != "" {
		h.Set("Cache-Control", ccOverride)
	} else {
		setIf(h, "Cache-Control", m.cacheControl)
	}
	setIf(h, "Content-Encoding", m.contentEncoding)
	setIf(h, "Content-Disposition", m.contentDisposition)
	setIf(h, "Content-Language", m.contentLanguage)
	setIf(h, "Expires", m.expires)
	setIf(h, "Content-Range", m.contentRange)
	h.Set("Accept-Ranges", "bytes")
}

func setIf(h http.Header, name string, value *string) {
	if value != nil && *value != "" {
		h.Set(name, *value)
	}
}

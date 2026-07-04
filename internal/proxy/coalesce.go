package proxy

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// coResult is the shareable outcome of a coalesced upstream call. It is
// immutable once returned, so any number of waiters may read it. A
// body-bearing 200/206 is deliberately NOT represented here: only the
// leader holds that body (via a closure variable) and streams it, while
// other waiters re-fetch their own — so one stream is never multiplexed
// across clients and the streaming memory ceiling is preserved.
type coResult struct {
	hasBody bool  // true: 200/206, body handled per-caller, not shared
	err     error // the S3 error to render (304-as-error, 404, ...) when !hasBody
}

// coalesceKey derives a singleflight key that is identical exactly when
// two requests would produce the same upstream result.
func coalesceKey(prefix, key, rng, ifNoneMatch string, ifModSince *time.Time) string {
	ims := ""
	if ifModSince != nil {
		ims = strconv.FormatInt(ifModSince.Unix(), 10)
	}
	return prefix + "\x00" + key + "\x00" + rng + "\x00" + ifNoneMatch + "\x00" + ims
}

// getCoalesced collapses concurrent identical revalidation GETs into one
// upstream call. A body-less outcome (304 or an error) is shared with
// every waiter; a body-bearing outcome is streamed by the leader and
// re-fetched by the others.
func (h *Handler) getCoalesced(w http.ResponseWriter, r *http.Request, in *s3.GetObjectInput) (int, int64) {
	sfKey := coalesceKey("GET", aws.ToString(in.Key), aws.ToString(in.Range),
		aws.ToString(in.IfNoneMatch), in.IfModifiedSince)

	leaderRan := false                // true only in the goroutine that runs fn
	var leaderOut *s3.GetObjectOutput // the body it holds, if any
	v, _, shared := h.group.Do(sfKey, func() (any, error) {
		leaderRan = true
		// Detach from this client's context so the leader disconnecting
		// does not fail the shared call for the waiters; the transport's
		// ResponseHeaderTimeout still bounds it.
		ctx := context.WithoutCancel(r.Context())
		upstreamStart := time.Now()
		out, err := h.store.GetObject(ctx, in)
		h.metrics.ObserveUpstreamLatency(time.Since(upstreamStart).Seconds())
		if err != nil {
			return &coResult{err: err}, nil
		}
		leaderOut = out
		return &coResult{hasBody: true}, nil
	})
	res := v.(*coResult)

	if res.hasBody {
		if leaderRan {
			// This goroutine ran fn: stream the body it holds.
			return h.streamOut(w, r, leaderOut)
		}
		// A waiter behind a body-bearing leader: fetch our own body.
		// in carries no If-Range here, so getDirect just streams.
		return h.getDirect(w, r, in, false)
	}

	// Body-less outcome (304 or error) shared across all waiters.
	if shared && !leaderRan {
		h.metrics.IncCoalesced()
	}
	return h.writeError(w, r, res.err), 0
}

// headCoalesced collapses concurrent identical HEAD lookups into one
// upstream call. A HEAD response is always body-less, so the outcome
// (metadata or a 304/error) is shared with every waiter.
func (h *Handler) headCoalesced(w http.ResponseWriter, r *http.Request, in *s3.HeadObjectInput) (int, int64) {
	sfKey := coalesceKey("HEAD", aws.ToString(in.Key), "",
		aws.ToString(in.IfNoneMatch), in.IfModifiedSince)

	leaderRan := false
	v, _, shared := h.group.Do(sfKey, func() (any, error) {
		leaderRan = true
		ctx := context.WithoutCancel(r.Context())
		upstreamStart := time.Now()
		out, err := h.store.HeadObject(ctx, in)
		h.metrics.ObserveUpstreamLatency(time.Since(upstreamStart).Seconds())
		if err != nil {
			return &headResult{err: err}, nil
		}
		return &headResult{meta: metaFromHead(out)}, nil
	})
	res := v.(*headResult)

	if shared && !leaderRan {
		h.metrics.IncCoalesced()
	}
	if res.err != nil {
		return h.writeError(w, r, res.err), 0
	}
	return h.writeHeadMeta(w, res.meta)
}

// headResult is the shareable outcome of a coalesced HeadObject.
type headResult struct {
	meta objectMeta
	err  error
}

package webhook

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/sjawhar/envoy/internal/metrics"
)

// DefaultBodyBudget is the body budget a listener holds when ENVOY_WEBHOOK_BODY_BUDGET is unset:
// 64 MiB, the budget the GitHub route held on its own before every route shared one. It is at least
// githubMaxBody, so any body GitHub sends fits. What the budget lets the listener hold before any
// signature is checked is the budget plus what the one read allowed past it charges beyond it, at
// most twice the largest route's cap (2 x githubMaxBody), so 114 MiB; a body that then checks
// decodes to about two and a half times its size again, about 400 MiB in all. A deployment sizes
// the listener's memory to hold that beside the rest of its resident memory, or sets a smaller
// budget.
const DefaultBodyBudget = 64 << 20

// bodyRetryAfter is the Retry-After, in seconds, of the 503 a delivery that found no room in the
// budget is answered (refuseBody): the listener's other 503s ask for the same second, and a
// delivery holding room gives it back when its handler returns, inside its route's answer bound.
const bodyRetryAfter = "1"

// errNoRoom is a body read that waited its route's whole room wait (begin) for room in the budget.
var errNoRoom = errors.New("no room in the webhook body budget")

// bodyFirstRead is the most a body's first read takes into a buffer the budget does not charge.
// Nothing is charged until that read delivers bytes, and the charged buffer then starts at what it
// delivered, so a connection that has sent only headers holds no room at all. It is small because
// every such connection holds one.
const bodyFirstRead = 512

// BodyBudget is a byte budget that concurrent requests read their bodies against: the listener
// holds one and every webhook route reads against it (WebhookConfig.BodyBudget). A request
// charges its read buffer as the buffer grows, which is as its body arrives, starting at the bytes
// its first read delivered and doubling as it fills, and never past its route's cap or its
// declared length: a sender that sends only headers holds no room, and one that trickles its body
// holds at most twice what it has sent, never what it declared. A request whose next piece does
// not fit waits for room, at most its route's room wait in all and then refused 503 (readBody),
// except one request at a time, which may take the total past the limit. That right goes to a
// request whose piece does not fit while the total is within the limit, which is every moment no
// request holds it, so another request can take it as soon as the total is back within the limit,
// and a request waits on the holder only while the total is past it. The holder can take the total
// past the limit by at most its own body (twice its buffer for the moment the buffer grows).
//
// The holder need not be reading. A connection that has sent nothing never asks for room, but a
// request asks as soon as its buffer is full, before its next byte arrives, and any sender can
// stop once its request holds the right. If that request's growth took the total past the limit,
// no request can charge a byte, however small, until the others give back enough to bring the
// total within it or the stalled request ends at its route's body deadline (githubBodyDeadline,
// or the server's read timeout); a request that waits longer than its room wait meanwhile is
// answered 503 rather than kept.
type BodyBudget struct {
	mu    sync.Mutex
	limit int64
	held  int64
	most  int64         // the most held at once, which the bound above caps
	over  *bodyRead     // the one request that may take held past limit, or nil
	room  chan struct{} // closed and replaced whenever held falls or over is given back
	// refused counts the deliveries answered 503 for finding no room (refuseBody), by route path.
	refused *metrics.Counter
}

// NewBodyBudget returns a budget of limit bytes, which must be positive, that counts the
// deliveries it refuses for want of room in refused (nil counts nothing).
func NewBodyBudget(limit int64, refused *metrics.Counter) *BodyBudget {
	return &BodyBudget{limit: limit, room: make(chan struct{}), refused: refused}
}

// mostHeld is the most the budget has held at once.
func (b *BodyBudget) mostHeld() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.most
}

// bodyRead is one request's read against a budget. It holds nothing until its body's bytes
// arrive, and release must be called once the request no longer holds its body.
type bodyRead struct {
	budget  *BodyBudget
	charged int64
	wait    time.Duration // what is left of the most this read waits for room, in all
}

// begin starts a request's read against the budget, which waits at most wait in all for room.
func (b *BodyBudget) begin(wait time.Duration) *bodyRead {
	return &bodyRead{budget: b, wait: wait}
}

// charge waits until n more bytes fit the budget or this read may go past the limit, then holds
// them. A read whose n does not fit takes the right to go past the limit when the total is within
// the limit, or when it already holds the right. A free right needs no case of its own: only the
// holder's own charge takes the total past the limit, and release gives that charge back before it
// gives the right back, so the total is within the limit whenever no read holds it. A wait ends at
// ctx's end, which is that error, or once this read has waited its room wait in all, which is
// errNoRoom.
func (r *bodyRead) charge(ctx context.Context, n int64) error {
	b := r.budget
	for {
		b.mu.Lock()
		fits := b.held+n <= b.limit
		if !fits && (b.over == r || b.held <= b.limit) {
			b.over = r
			fits = true
		}
		if fits {
			b.held += n
			b.most = max(b.most, b.held)
			r.charged += n
			b.mu.Unlock()
			return nil
		}
		room := b.room
		b.mu.Unlock()
		if err := ctx.Err(); err != nil {
			return err
		}
		if r.wait <= 0 {
			return errNoRoom
		}
		started := time.Now()
		timer := time.NewTimer(r.wait)
		select {
		case <-room:
			timer.Stop()
			r.wait -= time.Since(started)
		case <-timer.C:
			r.wait = 0
			return errNoRoom
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		}
	}
}

// refund gives back n of the bytes this read holds.
func (r *bodyRead) refund(n int64) {
	b := r.budget
	b.mu.Lock()
	defer b.mu.Unlock()
	b.held -= n
	r.charged -= n
	b.signalLocked()
}

// release gives back everything the read holds, and the right to go past the limit if it has it.
func (r *bodyRead) release() {
	b := r.budget
	b.mu.Lock()
	defer b.mu.Unlock()
	if r.charged == 0 && b.over != r {
		return
	}
	b.held -= r.charged
	r.charged = 0
	if b.over == r {
		b.over = nil
	}
	b.signalLocked()
}

func (b *BodyBudget) signalLocked() {
	close(b.room)
	b.room = make(chan struct{})
}

// readBody is how every webhook route reads its body: body, r's body or a reader over it, at most
// limit bytes and never past r's declared length, against the budget, waiting at most wait in all
// for room. It runs before the route checks the sender or parses anything, and it returns the
// body with the release the route must defer whatever the outcome, so a body stays charged until
// the handler that read it returns, a refusal included. A body it does not take is answered here
// (refuseBody) and its error returned, so the route only returns, or logs and returns. A panic
// inside the read gives back what the read charged before it unwinds: net/http recovers a
// handler's panic, and a charge it kept would shrink the budget for the life of the process.
func (b *BodyBudget) readBody(ctx context.Context, w http.ResponseWriter, r *http.Request, body io.Reader, limit int64, wait time.Duration) ([]byte, func(), error) {
	read := b.begin(wait)
	handed := false
	defer func() {
		if !handed {
			read.release()
		}
	}()
	data, err := read.readAll(ctx, body, r.ContentLength, limit)
	if err != nil {
		b.refuseBody(w, r, err)
	}
	handed = true
	return data, read.release, err
}

// refuseBody answers a body readBody did not take: 413 naming its route's cap (BodyTooLarge) for
// one over that cap, 503 with Retry-After for one that waited its route's whole room wait for room
// in the budget, and 400 for any other failure, a body cut short or a read that outlasted its
// deadline. The 503 logs a line and counts it (envoy_webhook_body_refused_total), since nothing
// else records that the budget turned a delivery away. Slack retries it. A GitHub App webhook's
// delivery waits for the redelivery sweep (packages/envoy/internal/dispatch/redeliver), which asks
// only for the App webhook's failed deliveries: a delivery from any other GitHub webhook is lost.
func (b *BodyBudget) refuseBody(w http.ResponseWriter, r *http.Request, err error) {
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge):
		http.Error(w, BodyTooLarge(tooLarge.Limit), http.StatusRequestEntityTooLarge)
	case errors.Is(err, errNoRoom):
		log.Printf("webhook body refused path=%s content_length=%d: %v; answered 503", r.URL.Path, r.ContentLength, err)
		b.refused.Inc([2]string{"path", r.URL.Path})
		w.Header().Set("Retry-After", bodyRetryAfter)
		http.Error(w, "webhook body budget full", http.StatusServiceUnavailable)
	default:
		http.Error(w, "invalid body", http.StatusBadRequest)
	}
}

// readAll reads body, charging the budget for its buffer before each time it grows. A body longer
// than limit is an *http.MaxBytesError: at once, before a byte is read, when its declared length
// says so, and otherwise once limit+1 bytes have arrived. With a declared length (declared >= 0)
// the buffer never grows past it and a shorter body is io.ErrUnexpectedEOF.
func (r *bodyRead) readAll(ctx context.Context, body io.Reader, declared, limit int64) ([]byte, error) {
	if declared > limit {
		return nil, &http.MaxBytesError{Limit: limit}
	}
	target := declared
	if target < 0 {
		target = limit + 1
	}
	head := make([]byte, min(bodyFirstRead, target))
	var buf []byte
	for {
		if declared >= 0 && int64(len(buf)) == declared {
			return buf, nil
		}
		var n int
		var err error
		if buf == nil {
			n, err = body.Read(head)
			if n > 0 {
				charged, growErr := r.grow(ctx, nil, int64(n))
				if growErr != nil {
					return nil, growErr
				}
				buf = append(charged, head[:n]...)
			}
		} else {
			if len(buf) == cap(buf) {
				grown, growErr := r.grow(ctx, buf, min(2*int64(cap(buf)), target))
				if growErr != nil {
					return nil, growErr
				}
				buf = grown
			}
			n, err = body.Read(buf[len(buf):cap(buf)])
			buf = buf[:len(buf)+n]
		}
		// Only an undeclared body can get here past limit: its buffer grows to one byte more than
		// limit so that the byte that says so can arrive, with or without the end of the body.
		if int64(len(buf)) > limit {
			return nil, &http.MaxBytesError{Limit: limit}
		}
		if err == io.EOF {
			if declared >= 0 && int64(len(buf)) != declared {
				return nil, io.ErrUnexpectedEOF
			}
			return buf, nil
		}
		if err != nil {
			return nil, err
		}
	}
}

// grow charges size bytes and only then allocates a buffer of that size holding buf, and gives
// back buf's capacity: a request waiting for room holds no buffer the budget has not charged.
func (r *bodyRead) grow(ctx context.Context, buf []byte, size int64) ([]byte, error) {
	if err := r.charge(ctx, size); err != nil {
		return nil, err
	}
	grown := make([]byte, len(buf), size)
	copy(grown, buf)
	r.refund(int64(cap(buf)))
	return grown, nil
}

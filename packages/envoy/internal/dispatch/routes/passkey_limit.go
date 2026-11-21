package routes

import (
	"net/http"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
)

// The bounds on the passkey ceremonies' begins (passkeyBeginLimiter). A begin takes no credential
// and writes (it deletes the lapsed ceremonies and records its own), so without a bound anyone who
// reaches the outside origin writes as fast as they can send. A person's browser makes one begin a
// sign-in or an enrollment, and a retry or two after a refusal, so the per-address bound is far
// above any person's; the route-wide bound holds every address together, since a client can spread
// its requests over many.
const (
	// passkeyBeginsPerAddress and passkeyBeginAddressBurst bound one client address: ten begins at
	// once, then one a second.
	passkeyBeginsPerAddress  = rate.Limit(1)
	passkeyBeginAddressBurst = 10
	// passkeyBeginsRouteWide and passkeyBeginRouteBurst bound every address together, per process.
	passkeyBeginsRouteWide = rate.Limit(50)
	passkeyBeginRouteBurst = 200
	// passkeyBeginAddresses caps the addresses the limiter remembers; at the cap it forgets those
	// whose bucket is full again (idle for passkeyBeginIdle), so memory stays bounded however many
	// addresses send.
	passkeyBeginAddresses = 10000
	passkeyBeginIdle      = time.Duration(passkeyBeginAddressBurst) * time.Second
)

// passkeyBeginLimiter bounds the passkey ceremonies' begins of one process: per client address, as
// the access log names it (auth.ForwardedAddress), and across every address. A begin over either
// bound is 429 PASSKEY_RATE_LIMITED, written before anything is read or written.
type passkeyBeginLimiter struct {
	mu        sync.Mutex
	all       *rate.Limiter
	byAddress map[string]*passkeyBeginBucket
}

type passkeyBeginBucket struct {
	limiter *rate.Limiter
	seen    time.Time
}

func newPasskeyBeginLimiter() *passkeyBeginLimiter {
	return &passkeyBeginLimiter{
		all:       rate.NewLimiter(passkeyBeginsRouteWide, passkeyBeginRouteBurst),
		byAddress: map[string]*passkeyBeginBucket{},
	}
}

// allow reports whether a begin from address at now is within both bounds, spending from each only
// when both have room.
func (l *passkeyBeginLimiter) allow(address string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	bucket, found := l.byAddress[address]
	if !found {
		if len(l.byAddress) >= passkeyBeginAddresses {
			l.forgetIdle(now)
		}
		bucket = &passkeyBeginBucket{limiter: rate.NewLimiter(passkeyBeginsPerAddress, passkeyBeginAddressBurst)}
		l.byAddress[address] = bucket
	}
	bucket.seen = now
	if bucket.limiter.TokensAt(now) < 1 || l.all.TokensAt(now) < 1 {
		return false
	}
	bucket.limiter.AllowN(now, 1)
	l.all.AllowN(now, 1)
	return true
}

// forgetIdle drops the addresses idle for passkeyBeginIdle, whose bucket has refilled: forgetting
// one loses nothing.
func (l *passkeyBeginLimiter) forgetIdle(now time.Time) {
	for address, bucket := range l.byAddress {
		if now.Sub(bucket.seen) >= passkeyBeginIdle {
			delete(l.byAddress, address)
		}
	}
}

// limitPasskeyBegins serves next, a ceremony's begin, while the request is within the begins'
// bounds, and answers 429 PASSKEY_RATE_LIMITED, with Retry-After, otherwise.
func (r *router) limitPasskeyBegins(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if !r.passkeyBegins.allow(auth.ForwardedAddress(req), time.Now()) {
			w.Header().Set("Retry-After", "1")
			writeCodeError(w, http.StatusTooManyRequests, "too many passkey ceremonies begun: try again in a moment", "PASSKEY_RATE_LIMITED")
			return
		}
		next(w, req)
	}
}

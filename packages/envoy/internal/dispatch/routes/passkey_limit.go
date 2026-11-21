package routes

import (
	"net"
	"net/http"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
)

// The bound on the passkey ceremonies' begins (passkeyBeginLimiter). A begin takes no credential
// and writes nothing (the ceremony rides in its signed cookie, and its finish records it used), so
// the bound is hygiene against one client's flood of the work a begin does, not a guard of the
// database. A person's browser makes one begin a sign-in or an enrollment, and a retry or two after
// a refusal, so the bound is far above any person's. It is per client address alone: a bound every
// address shared would let a stranger with enough addresses refuse the person's own begins.
const (
	// passkeyBeginsPerAddress and passkeyBeginAddressBurst bound one client address: ten begins at
	// once, then one a second.
	passkeyBeginsPerAddress  = rate.Limit(1)
	passkeyBeginAddressBurst = 10
	// passkeyBeginAddresses caps the addresses the limiter remembers. At the cap it forgets those
	// whose bucket is full again (idle for passkeyBeginIdle), sweeping the map at most once a
	// passkeyBeginIdle, and refuses a new address while the map is still full, so memory and the
	// work of each begin stay bounded however many addresses send.
	passkeyBeginAddresses = 10000
	passkeyBeginIdle      = time.Duration(passkeyBeginAddressBurst) * time.Second
)

// passkeyBeginLimiter bounds the passkey ceremonies' begins of one process per client address
// (passkeyBeginAddress). A begin over the bound is 429 PASSKEY_RATE_LIMITED, written before
// anything is read.
type passkeyBeginLimiter struct {
	mu        sync.Mutex
	byAddress map[string]*passkeyBeginBucket
	// swept is when forgetIdle last ran; the zero time before it has.
	swept time.Time
}

type passkeyBeginBucket struct {
	limiter *rate.Limiter
	seen    time.Time
}

func newPasskeyBeginLimiter() *passkeyBeginLimiter {
	return &passkeyBeginLimiter{byAddress: map[string]*passkeyBeginBucket{}}
}

// allow reports whether a begin from address at now is within its address's bound, spending from
// it when it is. A new address finding passkeyBeginAddresses remembered is refused, unless a sweep
// (at most one a passkeyBeginIdle) forgets an idle one first.
func (l *passkeyBeginLimiter) allow(address string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	bucket, found := l.byAddress[address]
	if !found {
		if len(l.byAddress) >= passkeyBeginAddresses && now.Sub(l.swept) >= passkeyBeginIdle {
			l.forgetIdle(now)
			l.swept = now
		}
		if len(l.byAddress) >= passkeyBeginAddresses {
			return false
		}
		bucket = &passkeyBeginBucket{limiter: rate.NewLimiter(passkeyBeginsPerAddress, passkeyBeginAddressBurst)}
		l.byAddress[address] = bucket
	}
	bucket.seen = now
	return bucket.limiter.AllowN(now, 1)
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

// passkeyBeginAddress is the address a begin is bounded by, and whether the request names one the
// bound can trust. Through Google's load balancer (auth.ThroughGoogleLoadBalancer) it is the client
// address the balancer appended (auth.ForwardedAddress), and a request carrying more than one
// X-Forwarded-For line is none the balancer forwarded as it writes the header, so it names no
// address (false). Any other connection is bounded by its own address, whatever X-Forwarded-For it
// carries: that header is then the client's own text, which would let it pick a fresh bound for
// each begin.
func passkeyBeginAddress(req *http.Request) (string, bool) {
	if auth.ThroughGoogleLoadBalancer(req) {
		if len(req.Header.Values("X-Forwarded-For")) > 1 {
			return "", false
		}
		return auth.ForwardedAddress(req), true
	}
	if host, _, err := net.SplitHostPort(req.RemoteAddr); err == nil {
		return host, true
	}
	return req.RemoteAddr, true
}

// limitPasskeyBegins serves next, a ceremony's begin, while the request is within its address's
// bound: 400 FORWARDED_FOR_INVALID for a request through the load balancer with more than one
// X-Forwarded-For line, and 429 PASSKEY_RATE_LIMITED, with Retry-After, over the bound.
func (r *router) limitPasskeyBegins(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		address, ok := passkeyBeginAddress(req)
		if !ok {
			writeCodeError(w, http.StatusBadRequest, "the request's X-Forwarded-For is not the one line the load balancer writes", "FORWARDED_FOR_INVALID")
			return
		}
		if !r.passkeyBegins.allow(address, time.Now()) {
			w.Header().Set("Retry-After", "1")
			writeCodeError(w, http.StatusTooManyRequests, "too many passkey ceremonies begun: try again in a moment", "PASSKEY_RATE_LIMITED")
			return
		}
		next(w, req)
	}
}

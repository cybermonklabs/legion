package routes

import (
	"net/http"
	"net/netip"
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
	// passkeyBeginIdle, and serves a new address without remembering it while the map is still
	// full, so memory and the work of each begin stay bounded however many addresses send, and no
	// number of strangers' addresses refuses the person's.
	passkeyBeginAddresses = 10000
	passkeyBeginIdle      = time.Duration(passkeyBeginAddressBurst) * time.Second
)

// passkeyBeginLimiter bounds the passkey ceremonies' begins of one process per client address
// (auth.BoundAddress), an IPv6 address keyed on its /64 (passkeyBeginKey). A begin over the bound is
// 429 PASSKEY_RATE_LIMITED, written before anything is read.
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

// allow reports whether a begin from address at now is within its key's bound (passkeyBeginKey),
// spending from it when it is. A new key finding passkeyBeginAddresses remembered after a sweep (at
// most one a passkeyBeginIdle) is served without being remembered: the cap holds the limiter's
// memory, and it must not refuse the person's begin from an address strangers have not yet filled
// it with.
func (l *passkeyBeginLimiter) allow(address string, now time.Time) bool {
	address = passkeyBeginKey(address)
	l.mu.Lock()
	defer l.mu.Unlock()
	bucket, found := l.byAddress[address]
	if !found {
		if len(l.byAddress) >= passkeyBeginAddresses && now.Sub(l.swept) >= passkeyBeginIdle {
			l.forgetIdle(now)
			l.swept = now
		}
		if len(l.byAddress) >= passkeyBeginAddresses {
			return true
		}
		bucket = &passkeyBeginBucket{limiter: rate.NewLimiter(passkeyBeginsPerAddress, passkeyBeginAddressBurst)}
		l.byAddress[address] = bucket
	}
	bucket.seen = now
	return bucket.limiter.AllowN(now, 1)
}

// passkeyBeginKey is the key a begin from address is bounded by: an IPv4 address (an IPv4 address
// written as IPv6 among them) as itself, and an IPv6 address as its /64, since one host commonly
// holds a whole /64 and would otherwise get a fresh burst for each of its addresses and could fill
// the limiter's memory alone. Text that is no IP address is its own key.
func passkeyBeginKey(address string) string {
	addr, err := netip.ParseAddr(address)
	if err != nil {
		return address
	}
	addr = addr.Unmap().WithZone("")
	if addr.Is4() {
		return addr.String()
	}
	network, _ := addr.Prefix(64)
	return network.String()
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

// limitPasskeyBegins serves next, a ceremony's begin, while the request is within its address's
// bound (auth.BoundAddress over the router's trusted proxy ranges, at the router's clock): 400
// FORWARDED_FOR_INVALID for a request from a trusted front door with more than one X-Forwarded-For
// line, and 429 PASSKEY_RATE_LIMITED, with Retry-After, over the bound.
func (r *router) limitPasskeyBegins(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		address, ok := auth.BoundAddress(req, r.ctx.trustedProxyRanges)
		if !ok {
			writeCodeError(w, http.StatusBadRequest, "the request's X-Forwarded-For is not the one line a trusted front door writes", "FORWARDED_FOR_INVALID")
			return
		}
		if !r.passkeyBegins.allow(address, r.ctx.clock()) {
			w.Header().Set("Retry-After", "1")
			writeCodeError(w, http.StatusTooManyRequests, "too many passkey ceremonies begun: try again in a moment", "PASSKEY_RATE_LIMITED")
			return
		}
		next(w, req)
	}
}

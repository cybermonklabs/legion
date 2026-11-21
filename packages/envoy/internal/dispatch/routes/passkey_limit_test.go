package routes

import (
	"fmt"
	"testing"
	"time"
)

// TestThePasskeyBeginLimiterHoldsEachAddressAndEveryAddress: one address gets passkeyBeginAddressBurst
// begins at once and then one a second; every address together gets passkeyBeginRouteBurst at once,
// so a client spreading its begins over many addresses is held too; and at passkeyBeginAddresses
// remembered addresses the limiter forgets the idle ones rather than grow.
//
// Control: spend from the address's bucket alone (allow skipping l.all), and the begin past the
// route-wide burst is allowed and the row fails.
func TestThePasskeyBeginLimiterHoldsEachAddressAndEveryAddress(t *testing.T) {
	at := time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)
	limiter := newPasskeyBeginLimiter()
	for n := range passkeyBeginAddressBurst {
		if !limiter.allow("198.51.100.7", at) {
			t.Fatalf("begin %d from one address at once refused, want the first %d allowed", n+1, passkeyBeginAddressBurst)
		}
	}
	if limiter.allow("198.51.100.7", at) {
		t.Fatalf("begin %d from one address at once allowed, want it refused", passkeyBeginAddressBurst+1)
	}
	if !limiter.allow("198.51.100.7", at.Add(time.Second)) {
		t.Fatal("a begin from the address a second later refused, want one a second")
	}

	everyAddress := newPasskeyBeginLimiter()
	for n := range passkeyBeginRouteBurst {
		if !everyAddress.allow(fmt.Sprintf("203.0.113.%d:%d", n%250, n), at) {
			t.Fatalf("begin %d over many addresses at once refused, want the first %d allowed", n+1, passkeyBeginRouteBurst)
		}
	}
	if everyAddress.allow("192.0.2.1", at) {
		t.Fatalf("begin %d over many addresses at once allowed, want the route-wide bound to refuse it", passkeyBeginRouteBurst+1)
	}

	remembering := newPasskeyBeginLimiter()
	for n := range passkeyBeginAddresses {
		remembering.byAddress[fmt.Sprintf("address %d", n)] = &passkeyBeginBucket{limiter: nil, seen: at}
	}
	remembering.allow("a new address", at.Add(passkeyBeginIdle))
	if got := len(remembering.byAddress); got != 1 {
		t.Fatalf("at the cap the limiter remembers %d addresses after forgetting the idle ones, want only the new one", got)
	}
}

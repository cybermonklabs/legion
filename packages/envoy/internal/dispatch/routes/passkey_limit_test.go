package routes

import (
	"fmt"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

// TestThePasskeyBeginLimiterHoldsEachAddressAlone: one address gets passkeyBeginAddressBurst begins
// at once and then one a second, while every other address keeps its own bound: 1,000 addresses at
// once, each at its burst, leave a fresh address its begins, so no flood over many addresses
// refuses the person's. At passkeyBeginAddresses remembered addresses the limiter forgets the idle
// ones rather than grow.
//
// Control: bound every address together as well (a route-wide bucket of 50 a second, 200 at once,
// as before), and the fresh address's begin is refused and the row fails.
func TestThePasskeyBeginLimiterHoldsEachAddressAlone(t *testing.T) {
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

	flooded := newPasskeyBeginLimiter()
	for n := range 1000 {
		for range passkeyBeginAddressBurst {
			flooded.allow(fmt.Sprintf("198.18.%d.%d", n/250, n%250), at)
		}
	}
	if !flooded.allow("203.0.113.1", at) {
		t.Fatal("a fresh address's begin after 1,000 addresses each spent their burst was refused, want it served")
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

// TestThePasskeyBeginLimiterServesANewAddressAtTheCap: with passkeyBeginAddresses addresses
// remembered and none idle, a begin from a new address is served without being remembered, so the
// limiter holds no more than the cap and no number of strangers' addresses refuses the person's
// begin. It sweeps the whole map at most once a passkeyBeginIdle, not for each new address: a new
// address two seconds after a sweep is served unremembered though the remembered ones have gone
// idle since. A known address still begins, and at the next sweep a new address is remembered in
// the idle ones' place.
//
// Controls: refuse a new address at the cap (allow answering false there, as at d0ce406f8c), and
// the first new address is refused; sweep for every new address (allow without the sweep's
// interval), and the map shrinks two seconds after the sweep; either way the row fails.
func TestThePasskeyBeginLimiterServesANewAddressAtTheCap(t *testing.T) {
	at := time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)
	full := newPasskeyBeginLimiter()
	// Seen a second short of idle at `at`, so idle from a second later.
	seen := at.Add(-passkeyBeginIdle + time.Second)
	for n := range passkeyBeginAddresses {
		full.byAddress[fmt.Sprintf("address %d", n)] = &passkeyBeginBucket{limiter: rate.NewLimiter(passkeyBeginsPerAddress, passkeyBeginAddressBurst), seen: seen}
	}
	if !full.allow("a new address", at) {
		t.Errorf("a new address at the cap, none idle, was refused; want it served")
	}
	if got := len(full.byAddress); got != passkeyBeginAddresses {
		t.Fatalf("at the cap the limiter remembers %d addresses after a new one asked, want %d", got, passkeyBeginAddresses)
	}
	if !full.allow("address 7", at) {
		t.Errorf("a remembered address at the cap was refused; want it to begin")
	}
	if !full.allow("another new address", at.Add(2*time.Second)) {
		t.Errorf("a new address two seconds after a sweep was refused; want it served")
	}
	if got := len(full.byAddress); got != passkeyBeginAddresses {
		t.Fatalf("two seconds after a sweep the limiter remembers %d addresses, want %d: it swept again within the interval", got, passkeyBeginAddresses)
	}
	if !full.allow("a later new address", at.Add(passkeyBeginIdle)) {
		t.Fatalf("a new address at the next sweep, the remembered ones idle, was refused; want it remembered in their place")
	}
	if got := len(full.byAddress); got != 1 {
		t.Fatalf("after the sweep the limiter remembers %d addresses, want the new one alone", got)
	}
}

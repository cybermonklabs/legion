package supervise

import "testing"

// An event is a far side's or an agent's words, and Handle gives each one through the store's rule
// before any row reads it (storetext.Strings), so no row cleans a field of an event itself. The
// field here is an update round's id: the daemon's sweep writes it from the uid the runtime read of
// a pod (PodRound), the claim records it, and no code ever cleaned it by hand, since the sweep is
// the daemon's own. A NUL and a byte that begins no UTF-8 sequence in it are U+FFFD in the claim in
// memory and in the claim the store is handed, so a restart reads back the round the machine held.
// The worker's answer names the round as the ask it read named it, and the ask is written from the
// round the claim recorded, so that answer counts.
func TestAnEventWithNULAndAnInvalidByteReachesTheClaimAsTheStoreKeepsIt(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	const want = PodRoundPrefix + "9d3f0c7a-\uFFFD-2b41-\uFFFD-4a39"

	h.must(PodUpdateOpen{Claim: h.token, Round: PodRound("9d3f0c7a-\x00-2b41-\xff-4a39"), Reasons: testReasons})

	if got := h.claim().UpdateRound; got != want {
		t.Fatalf("the claim records the round %q, want %q, the round as the store keeps it", got, want)
	}
	if stored := h.store.load(h.token).UpdateRound; stored != want {
		t.Fatalf("the store was handed the round %q, want %q", stored, want)
	}
	h.must(PodUpdateSignalled{Claim: h.token, Generation: h.generation(), Session: session, Round: want, Head: "c0ffee"})
	if h.claim().UpdateSignalledAt == nil {
		t.Fatalf("the worker's answer naming the round %q, as the claim records it, did not count", want)
	}
}

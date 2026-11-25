package ciphertoken_test

import (
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/ciphertoken"
)

// TestVerifierCarriesHeld (K3): an introspection answer that is active and held reaches both
// checks as Caller.Held, a held answer being a live one: it is kept in the cache as any affirmative
// answer is, so a request and a heartbeat inside AffirmativeCacheTTL read it from there, and the
// broker is asked again once AffirmativeCacheTTL has passed, when its answer that the person is
// no longer held reaches the caller.
//
// Control: callerOf leaving Held false; the held answer reaches Verify not held and the test fails.
func TestVerifierCarriesHeld(t *testing.T) {
	rig := newRig(t)
	caller := newSigner(t)
	const token = "a-token-of-a-person-dispatch-holds"
	held := liveToken(caller.jkt, time.Hour)
	held.Held = true
	rig.broker.Says(token, held)

	for request := range 2 {
		decided, err := rig.verifier.Verify(t.Context(), caller.at(resourcePath, token))
		if err != nil || !decided.Held || decided.Person != held.Person {
			t.Fatalf("Verify %d of a token the broker answers held = %+v, %v; want a caller for %s with Held set", request, decided, err, held.Person)
		}
	}
	if live, err := rig.verifier.Live(t.Context(), caller.at(resourcePath, token)); err != nil || !live.Held {
		t.Fatalf("Live of a token the broker answers held = %+v, %v; want Held set", live, err)
	}
	if rig.broker.Count() != 1 {
		t.Fatalf("two requests and a heartbeat cost %d introspections, want 1: the held answer is kept as any live one", rig.broker.Count())
	}

	released := held
	released.Held = false
	rig.broker.Says(token, released)
	rig.clock.advance(ciphertoken.AffirmativeCacheTTL - time.Millisecond)
	if decided, err := rig.verifier.Verify(t.Context(), caller.at(resourcePath, token)); err != nil || !decided.Held || rig.broker.Count() != 1 {
		t.Fatalf("Verify inside AffirmativeCacheTTL after the broker released the person = %+v, %v after %d introspections; want the kept held answer and no new introspection",
			decided, err, rig.broker.Count())
	}
	rig.clock.advance(time.Millisecond)
	if decided, err := rig.verifier.Verify(t.Context(), caller.at(resourcePath, token)); err != nil || decided.Held || rig.broker.Count() != 2 {
		t.Fatalf("Verify once AffirmativeCacheTTL passed = %+v, %v after %d introspections; want the broker asked again and the caller not held",
			decided, err, rig.broker.Count())
	}
}

// TestVerifierKeepsNoNotHeldAnswerPastNotHeldUntil (K11, c-iap-deadline): an introspection answer
// that is live, not held and carries not_held_until reaches both checks as Caller.NotHeldUntil,
// and the Verifier keeps it until the earlier of AffirmativeCacheTTL and that deadline: a request a
// millisecond before the deadline, and a heartbeat, read the kept answer, and a request at the
// deadline asks the broker again. Lapsed, by the verifier's own clock, is false before the deadline
// and true at it. A held answer carrying a deadline is kept as every held answer is, for
// AffirmativeCacheTTL, and is never lapsed: a hold is refused as held.
//
// Control: answerCache.remember ignoring NotHeldUntil; the request at the deadline reads the kept
// answer, asking the broker nothing, and the test fails naming it.
func TestVerifierKeepsNoNotHeldAnswerPastNotHeldUntil(t *testing.T) {
	rig := newRig(t)
	caller := newSigner(t)
	const token = "a-token-of-a-person-served-until-a-deadline"
	deadline := rig.clock.now().Truncate(time.Second).Add(3 * time.Second)
	served := liveToken(caller.jkt, time.Hour)
	served.NotHeldUntil = deadline.Unix()
	rig.broker.Says(token, served)

	decided, err := rig.verifier.Verify(t.Context(), caller.at(resourcePath, token))
	if err != nil || decided.Held || !decided.NotHeldUntil.Equal(deadline) || rig.verifier.Lapsed(decided) {
		t.Fatalf("Verify of a token the broker serves until %v = %+v, %v (lapsed %t); want a caller not held, until that deadline, not lapsed",
			deadline, decided, err, rig.verifier.Lapsed(decided))
	}
	rig.clock.advance(deadline.Sub(rig.clock.now()) - time.Millisecond)
	decided, err = rig.verifier.Verify(t.Context(), caller.at(resourcePath, token))
	if err != nil || !decided.NotHeldUntil.Equal(deadline) || rig.verifier.Lapsed(decided) || rig.broker.Count() != 1 {
		t.Fatalf("Verify a millisecond before the deadline = %+v, %v (lapsed %t) after %d introspections; want the kept answer, not lapsed, and 1 introspection",
			decided, err, rig.verifier.Lapsed(decided), rig.broker.Count())
	}
	if live, err := rig.verifier.Live(t.Context(), caller.at(resourcePath, token)); err != nil || !live.NotHeldUntil.Equal(deadline) || rig.broker.Count() != 1 {
		t.Fatalf("Live a millisecond before the deadline = %+v, %v after %d introspections; want the kept answer and 1 introspection", live, err, rig.broker.Count())
	}
	rig.clock.advance(time.Millisecond)
	decided, err = rig.verifier.Verify(t.Context(), caller.at(resourcePath, token))
	if err != nil || rig.broker.Count() != 2 || !rig.verifier.Lapsed(decided) {
		t.Fatalf("Verify at the deadline = %+v, %v (lapsed %t) after %d introspections; want the broker asked again, inside AffirmativeCacheTTL, and its answer lapsed",
			decided, err, rig.verifier.Lapsed(decided), rig.broker.Count())
	}

	const heldToken = "a-held-token-carrying-a-deadline"
	held := liveToken(caller.jkt, time.Hour)
	held.Held, held.NotHeldUntil = true, rig.clock.now().Add(2*time.Second).Unix()
	rig.broker.Says(heldToken, held)
	if decided, err := rig.verifier.Verify(t.Context(), caller.at(resourcePath, heldToken)); err != nil || !decided.Held || rig.verifier.Lapsed(decided) || rig.broker.Count() != 3 {
		t.Fatalf("Verify of a held token carrying a deadline = %+v, %v after %d introspections; want held, not lapsed, 3 introspections", decided, err, rig.broker.Count())
	}
	rig.clock.advance(ciphertoken.AffirmativeCacheTTL - time.Millisecond)
	if decided, err := rig.verifier.Verify(t.Context(), caller.at(resourcePath, heldToken)); err != nil || !decided.Held || rig.verifier.Lapsed(decided) || rig.broker.Count() != 3 {
		t.Fatalf("Verify of the held token past its deadline, inside AffirmativeCacheTTL = %+v, %v after %d introspections; want the kept held answer, never lapsed",
			decided, err, rig.broker.Count())
	}
}

// packages/envoy/internal/cipher/helper/signinkeep_test.go
//go:build linux

// A helper that proves an identity of its own, with a person signed in on the box, keeps the
// sign-in's launcher credential live while an enrollment it made or renews may stand under it:
// the broker renews a session enrolled under a sign-in only while that credential is live, and it
// expires an hour after each trade. It trades nothing while nothing may stand under it, so the
// sign-in's "last used" on Dispatch says whether the box is in use. Like enrolauthority_test.go's,
// these drive the Broker directly: no unix socket and no pidfd.

package helper

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/testwait"
)

// keepEvery is the tick these tests give keepSignIns.
const keepEvery = 20 * time.Millisecond

// startKeeping starts b with a short tick and answers a reading of how often it has asked
// signIns since, and the stop of its context.
func startKeeping(t *testing.T, b *Broker, signIns *stubSignIns) (asks func() int, stop context.CancelFunc) {
	t.Helper()
	b.SignIns = signIns
	b.signInKeepEvery = keepEvery
	b.startRefreshBound = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	signIns.mu.Lock()
	signIns.calls = 0
	signIns.mu.Unlock()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return func() int {
		signIns.mu.Lock()
		defer signIns.mu.Unlock()
		return signIns.calls
	}, cancel
}

// asksStop fails t unless asks stays where it is for ten ticks.
func asksStop(t *testing.T, asks func() int, why string) {
	t.Helper()
	settled := asks()
	time.Sleep(10 * keepEvery)
	if after := asks(); after > settled+1 {
		t.Fatalf("the helper asked for the sign-in %d more times %s; want no ask", after-settled, why)
	}
}

// TestAHelperAsksForItsSignInOnATickAndStopsWithItsContext: with a box enrolled under the
// sign-in, the helper asks SignIns for the sign-in's credential on every tick, which is what
// trades it again before it lapses, with no enrolment needed; an ask that fails is asked again at
// the next tick; and no ask comes once the helper's context has ended.
func TestAHelperAsksForItsSignInOnATickAndStopsWithItsContext(t *testing.T) {
	f := newEnrolBroker(t)
	b, _, _ := machineIdentityBroker(t, f)
	signIns := &stubSignIns{authority: signedIn(t, f, "ada@example.com", "credential-of-adas-sign-in")}
	b.SignIns = signIns
	if _, _, err := b.EnrollSessionKey(context.Background(), "example-host:1234:77", "thumbprint-of-the-box-key", nil, nil); err != nil {
		t.Fatal(err)
	}
	signIns.mu.Lock()
	signIns.authority, signIns.err = nil, errors.New("the broker is not serving")
	signIns.mu.Unlock()
	asks, stop := startKeeping(t, b, signIns)
	testwait.WithinPolling(t, 5*time.Second, 5*time.Millisecond, "three asks for the sign-in, each after a failed one", func() bool { return asks() >= 3 })
	stop()
	asksStop(t, asks, "after its context ended")
}

// TestAHelperTradesItsSignInOnlyWhileAnEnrollmentMayStandUnderIt: each trade of the sign-in moves
// its "last used" on Dispatch's sign-ins page, so the helper asks for it only while an enrollment
// may stand under it. Rows: nothing enrolled, so no ask; a session of the helper's own enrolled
// under this machine's credential before the person signed in, which no trade of a sign-in keeps,
// so no ask; a session enrolled under the sign-in, asked for until its revoke; and a box a
// launcher enrolled under the sign-in, which renews with its own key and tells the helper
// nothing, asked for by the helper started after it, until it is unenrolled.
func TestAHelperTradesItsSignInOnlyWhileAnEnrollmentMayStandUnderIt(t *testing.T) {
	ctx := context.Background()
	t.Run("nothing enrolled", func(t *testing.T) {
		f := newEnrolBroker(t)
		b, _, _ := machineIdentityBroker(t, f)
		asks, _ := startKeeping(t, b, &stubSignIns{authority: signedIn(t, f, "ada@example.com", "credential-of-adas-sign-in")})
		asksStop(t, asks, "with nothing enrolled")
		if n := asks(); n != 0 {
			t.Fatalf("the helper asked for the sign-in %d times with nothing enrolled; want none", n)
		}
	})
	t.Run("a session enrolled under this machine", func(t *testing.T) {
		f := newEnrolBroker(t)
		b, _, _ := machineIdentityBroker(t, f)
		signIns := &stubSignIns{}
		b.SignIns = signIns
		if _, _, err := b.EnrollIdentity(ctx, identity{RuntimeID: "example-host:1234:77", Thumbprint: "thumbprint-of-the-session-key"}); err != nil {
			t.Fatal(err)
		}
		signIns.mu.Lock()
		signIns.authority = signedIn(t, f, "ada@example.com", "credential-of-adas-sign-in")
		signIns.mu.Unlock()
		asks, _ := startKeeping(t, b, signIns)
		asksStop(t, asks, "for a session enrolled under this machine's own credential")
		if n := asks(); n != 0 {
			t.Fatalf("the helper asked for the sign-in %d times for a session enrolled under this machine; want none", n)
		}
	})
	t.Run("a session enrolled under the sign-in", func(t *testing.T) {
		f := newEnrolBroker(t)
		b, _, _ := machineIdentityBroker(t, f)
		signIns := &stubSignIns{authority: signedIn(t, f, "ada@example.com", "credential-of-adas-sign-in")}
		b.SignIns = signIns
		id, _, err := b.EnrollIdentity(ctx, identity{RuntimeID: "example-host:1234:77", Thumbprint: "thumbprint-of-the-session-key"})
		if err != nil {
			t.Fatal(err)
		}
		asks, _ := startKeeping(t, b, signIns)
		testwait.WithinPolling(t, 5*time.Second, 5*time.Millisecond, "three asks while the session stands under the sign-in", func() bool { return asks() >= 3 })
		if err := b.Revoke(ctx, id); err != nil {
			t.Fatal(err)
		}
		asksStop(t, asks, "after the session's enrollment was revoked")
	})
	t.Run("a box enrolled under the sign-in, across a restart", func(t *testing.T) {
		f := newEnrolBroker(t)
		file := filepath.Join(t.TempDir(), "signin-boxes.json")
		person := signedIn(t, f, "ada@example.com", "credential-of-adas-sign-in")
		first, _, _ := machineIdentityBroker(t, f)
		first.SignInBoxesFile = file
		first.SignIns = &stubSignIns{authority: person}
		id, _, err := first.EnrollSessionKey(ctx, "example-host:1234:77", "thumbprint-of-the-box-key", nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if info, err := os.Stat(file); err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("the record of the box: %v, %v; want a file only its owner reads", info, err)
		}

		again, _, _ := machineIdentityBroker(t, f)
		again.SignInBoxesFile = file
		asks, stop := startKeeping(t, again, &stubSignIns{authority: person})
		testwait.WithinPolling(t, 5*time.Second, 5*time.Millisecond, "three asks for the box the helper before enrolled", func() bool { return asks() >= 3 })
		if err := again.Revoke(ctx, id); err != nil {
			t.Fatal(err)
		}
		asksStop(t, asks, "after the box was unenrolled")
		stop()

		last, _, _ := machineIdentityBroker(t, f)
		last.SignInBoxesFile = file
		asks, _ = startKeeping(t, last, &stubSignIns{authority: person})
		asksStop(t, asks, "after a restart that follows the box's unenrolment")
		if n := asks(); n != 0 {
			t.Fatalf("the helper started after the box was unenrolled asked for the sign-in %d times; want none", n)
		}
	})
}

// TestSignInHoldsCountWhatMayStandUnderTheSignIn is the rule keepSignIns reads, on a clock of the
// test's own: a session counts while its lease runs unless this helper enrolled it under its own
// machine credential, one it renews without having enrolled it counts, and a box under the
// sign-in counts until it ends, whatever the time.
func TestSignInHoldsCountWhatMayStandUnderTheSignIn(t *testing.T) {
	now := time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC)
	var holds signInHolds
	if holds.needed(now) {
		t.Fatal("nothing enrolled is needed")
	}
	if err := holds.enrolled("machine-session", now.Add(15*time.Minute), false, true, ""); err != nil {
		t.Fatal(err)
	}
	holds.renewed("machine-session", now.Add(30*time.Minute))
	if holds.needed(now) {
		t.Fatal("a session this helper enrolled under its own machine credential is needed")
	}
	holds.renewed("inherited-session", now.Add(15*time.Minute))
	if !holds.needed(now) {
		t.Fatal("a session this helper renews without having enrolled it is not needed; it may stand under the sign-in")
	}
	if holds.needed(now.Add(20 * time.Minute)) {
		t.Fatal("a session whose lease has run out is needed")
	}
	if err := holds.enrolled("machine-box", time.Time{}, true, true, ""); err != nil {
		t.Fatal(err)
	}
	if holds.needed(now.Add(20 * time.Minute)) {
		t.Fatal("a box enrolled under this machine's own credential is needed")
	}
	if err := holds.enrolled("signin-box", now.Add(15*time.Minute), true, false, ""); err != nil {
		t.Fatal(err)
	}
	if !holds.needed(now.Add(24 * time.Hour)) {
		t.Fatal("a box enrolled under the sign-in is not needed a day on; it renews with no word to the helper")
	}
	if err := holds.ended("signin-box", ""); err != nil {
		t.Fatal(err)
	}
	if holds.needed(now.Add(20 * time.Minute)) {
		t.Fatal("a box unenrolled is still needed")
	}
}

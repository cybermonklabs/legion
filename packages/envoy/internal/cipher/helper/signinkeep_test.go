// packages/envoy/internal/cipher/helper/signinkeep_test.go
//go:build linux

// A helper that proves an identity of its own, with a person signed in on the box, keeps the
// sign-in's launcher credential live while it runs: the broker renews a session enrolled under a
// sign-in only while that credential is live, and it expires an hour after each trade. Like
// enrolauthority_test.go's, this drives the Broker directly: no unix socket and no pidfd.

package helper

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/testwait"
)

// TestAHelperAsksForItsSignInOnATickAndStopsWithItsContext: once Start runs in identity mode,
// the helper asks SignIns for the sign-in's credential on every tick, which is what trades it
// again before it lapses, with no enrolment needed; an ask that fails is asked again at the next
// tick; and no ask comes once the helper's context has ended.
func TestAHelperAsksForItsSignInOnATickAndStopsWithItsContext(t *testing.T) {
	f := newEnrolBroker(t)
	b, _, _ := machineIdentityBroker(t, f)
	signIns := &stubSignIns{err: errors.New("the broker is not serving")}
	b.SignIns = signIns
	b.signInKeepEvery = 20 * time.Millisecond
	b.startRefreshBound = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	calls := func() int {
		signIns.mu.Lock()
		defer signIns.mu.Unlock()
		return signIns.calls
	}
	testwait.WithinPolling(t, 5*time.Second, 5*time.Millisecond, "three asks for the sign-in, each after a failed one", func() bool { return calls() >= 3 })
	cancel()
	settled := calls()
	time.Sleep(10 * b.signInKeepEvery)
	if after := calls(); after > settled+1 {
		t.Fatalf("the helper asked for the sign-in %d more times after its context ended; want no ask once it has stopped", after-settled)
	}
}

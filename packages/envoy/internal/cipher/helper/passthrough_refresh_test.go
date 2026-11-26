// packages/envoy/internal/cipher/helper/passthrough_refresh_test.go
//go:build linux

package helper

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// TestAPassThroughOpAnswersInsideItsBoundWhileARefreshHoldsTheMachineLogin: A10 says the helper
// answers every request it accepted within the time its client waits, and adds that no lock the
// helper holds for its own work outlives a caller's bound.
//
// A refresh of the machine login holds credMu from its token request to its save and swap, on
// the refresh loop's context, which has no deadline. An enroll-session-key whose enrolment the
// broker refuses 401 LAUNCHER_INVALID drops the credential it signed with, and that drop waits
// for a refresh in flight (dropOutsideRefresh). The drop's wait ends with the op's context, at
// the bound (Server.brokerCall), so the op answers then with what it knows: the broker refused
// it, ENROLL_FAILED carrying the broker's LAUNCHER_INVALID, and not an unknown outcome. The
// refresh it stood behind goes on to its own answer and settles the credential.
//
// The row runs under a bound of overBound, with brokerCall's timer timerSlack after it
// (slowTimer), so the op's own answer at the bound cannot lose to the timer under load, and the
// refresh it holds need only outlast the timer, rather than the real 21 s. The answer must still
// come within answerMargin of the bound: in production a later one loses to the 1 s timer.
//
// The named revert: put a sync.Mutex back as credMu, so the drop waits with no context, and
// brokerCall's timer answers ENROLL_OUTCOME_UNKNOWN for an enrolment the broker refused.
func TestAPassThroughOpAnswersInsideItsBoundWhileARefreshHoldsTheMachineLogin(t *testing.T) {
	t.Parallel()
	// timerAnswers is when brokerCall's timer answers an op still running: the bound plus
	// timerSlack.
	const timerAnswers = overBound + timerSlack
	r := startRig(t, "")
	slowTimer(r.srv, overBound)
	signed := r.srv.Broker.cred.Load()

	release := heldRefresh(t, r.fake, r.srv.Broker)
	// The refresh is let go 3 s after the timer would answer, as a broker that answers it that
	// late would; a row that has its answer lets it go at once. The cleanup runs before the rig's
	// own, whose Close waits on credMu.
	refreshed := make(chan error, 1)
	slowBroker := time.AfterFunc(timerAnswers+3*time.Second, func() { refreshed <- release() })
	finishRefresh := sync.OnceValue(func() error {
		if slowBroker.Stop() {
			return release()
		}
		return <-refreshed
	})
	t.Cleanup(func() { _ = finishRefresh() })

	r.fake.mu.Lock()
	r.fake.enrollUnauthorizedNext = 1
	r.fake.mu.Unlock()
	started := time.Now()
	resp, err := Call(r.sock, Request{Op: "enroll-session-key", RuntimeID: "rt-during-the-refresh", Thumbprint: "tp-during-the-refresh"}, BrokerCallWait)
	waited := time.Since(started)
	if err != nil {
		t.Fatalf("enroll-session-key refused while the machine login is refreshing: %v after %s; the client must read one JSON reply, never an empty one (the bound is %s)",
			err, waited, overBound)
	}
	if waited >= overBound+answerMargin {
		t.Fatalf("enroll-session-key answered after %s, past its %s bound plus %s: the drop's wait did not end with the op's context (%+v)", waited, overBound, answerMargin, resp)
	}
	if resp.OK || resp.Code != CodeEnrollFailed || !strings.Contains(resp.Error, "LAUNCHER_INVALID") || strings.Contains(resp.Error, "the outcome is unknown") {
		t.Fatalf("enroll-session-key refused while the machine login is refreshing: %+v after %s; want %s with the broker's LAUNCHER_INVALID, a known refusal", resp, waited, CodeEnrollFailed)
	}

	// The drop that gave up dropped nothing: the refresh settles the credential, and the helper
	// holds the one it refreshed.
	if err := finishRefresh(); err != nil {
		t.Fatalf("the refresh the call stood behind: %v", err)
	}
	if held := r.srv.Broker.cred.Load(); held == nil || held == signed || held.id != signed.id {
		t.Fatalf("after the refresh the helper holds %+v; want the refreshed %s", held, signed.id)
	}
	if ls := r.srv.Broker.LoginStatus(); ls.State != "issued" || ls.Refused || !ls.CredentialHeld {
		t.Fatalf("login-status after the refresh: %+v; want issued and held, not refused", ls)
	}
}

// packages/envoy/internal/broker/helper/passthrough_refresh_test.go
//go:build linux

package helper

import (
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/internal/testwait"
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
// The bound and brokerCall's timer run on the server's fake clock (newRigOn): the row moves it to
// the bound once the drop is about to wait, and never to the timer, so the answer can be no one's
// but the op's, and the refresh it holds is let go once the row has read that answer.
//
// The named revert: put a sync.Mutex back as credMu, so the drop waits with no context; the op
// then never answers while the refresh is held, and the row fails at the shared wait's bound on
// its reply.
func TestAPassThroughOpAnswersInsideItsBoundWhileARefreshHoldsTheMachineLogin(t *testing.T) {
	r := newRigOn(t, "")
	// Set before the login starts the Broker's own goroutines, which read it.
	dropping := make(chan struct{})
	r.srv.Broker.testBeforeDrop = sync.OnceFunc(func() { close(dropping) })
	r.login(t)
	signed := r.srv.Broker.cred.Load()

	release := heldRefresh(t, r.fake, r.srv.Broker)
	// The cleanup runs before the rig's own, whose Close waits on credMu, so a row that fails
	// before its answer still lets the refresh go.
	finishRefresh := sync.OnceValue(release)
	t.Cleanup(func() { _ = finishRefresh() })

	r.fake.mu.Lock()
	r.fake.enrollUnauthorizedNext = 1
	r.fake.mu.Unlock()
	const what = "enroll-session-key refused while the machine login is refreshing"
	replies := callInBackground(t, r.sock, Request{Op: "enroll-session-key", RuntimeID: "rt-during-the-refresh", Thumbprint: "tp-during-the-refresh"})
	armBrokerCall(t, r.clock, replies, what)
	// The broker has refused the enrolment and the op is dropping the credential it signed with,
	// which waits for the refresh; the bound ends that wait.
	testwait.Receive(t, what+": the drop of the refused credential to wait for the refresh", dropping)
	r.clock.Advance(BrokerCallBound)
	resp := replyOf(t, replies, what)
	if resp.OK || resp.Code != CodeEnrollFailed || !strings.Contains(resp.Error, "LAUNCHER_INVALID") || strings.Contains(resp.Error, "the outcome is unknown") {
		t.Fatalf("%s: %+v; want %s with the broker's LAUNCHER_INVALID, a known refusal", what, resp, CodeEnrollFailed)
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

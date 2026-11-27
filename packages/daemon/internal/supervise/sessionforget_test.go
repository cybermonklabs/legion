package supervise

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/oauth2call"
	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/wait"
)

// suspendTool is the stop a forget's refusal names for the controller (mayForget), beside the
// operator's `legion claims suspend`: the controller holds no operator's token, and its `legion`
// tool's suspend stops the holder's process through the daemon.
const suspendTool = "the `suspend` of the controller's `legion` tool"

// A session a claim records but does not hold is cleared only once the Envoy listener no longer
// names it as the claim's role holder (mayForget, review rounds 16 and 17 of #4220). The outbox
// withholds the claim's role messages only while it records that session, and the listener keeps
// naming the session as the role's holder while its claim stands, which another claim's live
// process under the same id keeps standing, so a clear made then would send the role's messages
// to that process. The listener is asked once per clear, of the claim's own role; the clear is
// refused while the listener names the cleared session as the role's holder in any state (live,
// unresponsive, gone, or lapsed with no state from an older listener), and while the listener
// gives no answer, and the refusal names the claim to suspend, and how long the wait after it
// takes. Each such refusal is a wait (ErrForgetWaitsForRole, wait.ErrWaiting), but one of a lookup
// the listener refuses the same way every time, which is a fault an operator fixes and no wait.
// It names that claim only when the refused name is the session id the listener names: a
// file name another claim holds says nothing of the process under the id. The clear goes on once
// no claim names the role, or another session holds it. A claim holding its own session forgets
// as before and asks nothing (R9).
//
// The controls (the pull request records each run): mayForget admitting every clear, as today's
// forget does, clears the session in every refused row; admitting a holder the listener reports
// gone, as round 17 first did, clears it in the gone row; naming the holder of any refused name
// names the file name's holder in the last refused row; and forgetWait matching only one of its
// two markers fails the other marker's check in every wait row.
func TestAForgetOfASessionTheClaimDoesNotHoldWaitsForItsRole(t *testing.T) {
	const holder = claim.Token("legion-legion-LEGION-209-tester")
	type answer struct {
		holder, state string
		err           error
	}
	// processless is a claim suspended over session, each of whose names in elsewhere holder
	// recorded first, its role lookup answering with a.
	processless := func(t *testing.T, elsewhere []Hold, a answer) (*harness, *int) {
		h := newHarness(t)
		h.reach(StateIdle)
		h.must(RequestStop{Claim: h.token, By: SuspendedByOperator})
		h.wantState(StateRetired)
		for _, name := range elsewhere {
			h.store.setHold(name, holder)
		}
		asked := 0
		h.deps.RoleHolder = func(_ context.Context, role string) (oauth2call.RemoteText, string, error) {
			asked++
			if role != string(h.token) {
				t.Errorf("the forget looked up role %s, want the claim's own, %s", role, h.token)
			}
			return oauth2calltest.Said(a.holder), a.state, a.err
		}
		h.restart()
		return h, &asked
	}
	every := RecordedHolds(session, sessionFile)
	var fileNameOnly []Hold
	for _, name := range every {
		if name.Kind == HoldFileName {
			fileNameOnly = append(fileNameOnly, name)
		}
	}
	// The bound and the remedy are spelled here rather than read from the code, so a wrong bound
	// or a remedy that sends the operator to suspend a claim that runs nothing fails the rows
	// (review round 16 of #4220, #60 and #61). The remedy names the controller's suspend beside
	// the operator's, since the controller holds no operator's token.
	const suspendHolder, theBound, holderRunsNothing = "legion claims suspend --claim " + string(holder),
		"max(min(ENVOY_SESSION_MAX_AGE, ENVOY_SESSION_GONE_HORIZON), ENVOY_SESSION_LIVE_WINDOW) + ENVOY_ROLE_CLAIM_REAP_INTERVAL",
		"if " + string(holder) + " is suspended, retired or failed, no process of it runs"
	clears := []struct {
		name  string
		event func(h *harness) Event
	}{
		{"forget-session", func(h *harness) Event {
			return RequestForgetSession{Claim: h.token, Session: session, By: SuspendedByOperator}
		}},
		{"retry in a new session", func(h *harness) Event { return RequestRetry{Claim: h.token, FreshSession: true} }},
	}
	for _, clear := range clears {
		for _, tc := range []struct {
			name      string
			elsewhere []Hold
			a         answer
			refused   []string
			unnamed   bool
			wait      bool // a refused row is a wait: it matches ErrForgetWaitsForRole and wait.ErrWaiting
		}{
			{"refused while the listener names the session as the role's live holder", every, answer{session, "live", nil}, []string{suspendHolder, suspendTool, holderRunsNothing, theBound}, false, true},
			{"refused while that holder is unresponsive", every, answer{session, "unresponsive", nil}, []string{suspendHolder, suspendTool}, false, true},
			{"refused while that holder is gone but its claim stands", every, answer{session, "gone", nil}, []string{suspendHolder, suspendTool, theBound}, false, true},
			{"refused while an older listener names that holder lapsed", every, answer{session, "lapsed", nil}, []string{suspendHolder, suspendTool}, false, true},
			{"refused while the listener does not answer", every, answer{"", "", errors.New("the listener is unreachable")}, []string{"did not say which session holds its role"}, false, true},
			{"refused while the listener refuses the lookup, a fault", every, answer{"", "", fmt.Errorf("look up: %w", &notify.RoleLookupRefused{Status: 401, Reason: "unauthorized"})}, []string{"refuses the lookup", "until an operator fixes"}, false, false},
			{"refused naming no claim when the refused name is a file name another claim holds", fileNameOnly, answer{session, "live", nil}, []string{"the process registered under it", theBound}, true, true},
			{"admitted once no claim names the role", every, answer{}, nil, false, false},
			{"admitted while another session holds the role", every, answer{"ses_another", "live", nil}, nil, false, false},
		} {
			t.Run(clear.name+": "+tc.name, func(t *testing.T) {
				h, asked := processless(t, tc.elsewhere, tc.a)
				err := h.handle(clear.event(h))
				if *asked != 1 {
					t.Errorf("the listener was asked %d times, want once", *asked)
				}
				if tc.refused == nil {
					if err != nil {
						t.Fatalf("the clear = %v, want it admitted", err)
					}
					if c := h.claim(); c.Session != "" || c.Abandoned.Session != session {
						t.Fatalf("the claim records %q (abandoned %q), want %s cleared", c.Session, c.Abandoned.Session, session)
					}
					return
				}
				var refused *RefusedError
				if !errors.As(err, &refused) {
					t.Fatalf("the clear = %v, want it refused", err)
				}
				if got := errors.Is(err, ErrForgetWaitsForRole); got != tc.wait {
					t.Errorf("the refusal = %v, ErrForgetWaitsForRole %t; want %t", err, got, tc.wait)
				}
				if got := errors.Is(err, wait.ErrWaiting); got != tc.wait {
					t.Errorf("the refusal = %v, wait.ErrWaiting %t; want %t", err, got, tc.wait)
				}
				for _, want := range tc.refused {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("the refusal = %v, want it to name %q", err, want)
					}
				}
				if tc.unnamed && strings.Contains(err.Error(), string(holder)) {
					t.Errorf("the refusal = %v, want it to name no claim: %s holds only the file's name, not the session id the listener names", err, holder)
				}
				if c := h.claim(); c.Session != session || c.SessionFile != sessionFile {
					t.Fatalf("the refused clear left the claim recording %q at %q, want %s kept", c.Session, c.SessionFile, session)
				}
			})
		}
	}
	t.Run("a claim holding its own session forgets without asking", func(t *testing.T) {
		h, asked := processless(t, nil, answer{session, "live", nil})
		h.must(RequestForgetSession{Claim: h.token, Session: session, By: SuspendedByOperator})
		if *asked != 0 {
			t.Fatalf("the listener was asked %d times, want none: the claim holds %s", *asked, session)
		}
		if c := h.claim(); c.Session != "" {
			t.Fatalf("the claim still records %q", c.Session)
		}
	})
	// The rows round 16 of #4220 found missing (#65): a name held for no claim is refused as a
	// wait naming no claim to suspend; a daemon with no Envoy listener asks nothing and forgets;
	// and a store that does not answer the hold refuses with its error, which is no wait and no
	// refusal of the claim's state, and asks the listener nothing.
	t.Run("a name held for no claim is refused naming no claim", func(t *testing.T) {
		h, asked := processless(t, nil, answer{session, "live", nil})
		for _, name := range every {
			h.store.setHold(name, "")
		}
		err := h.handle(RequestForgetSession{Claim: h.token, Session: session, By: SuspendedByOperator})
		if *asked != 1 || !errors.Is(err, ErrForgetWaitsForRole) || !strings.Contains(err.Error(), "the process registered under it") {
			t.Fatalf("the forget of a session held for no claim = %v, asked %d; want a wait naming no claim, asked once", err, *asked)
		}
		if c := h.claim(); c.Session != session {
			t.Fatalf("the claim records %q, want %s kept", c.Session, session)
		}
	})
	t.Run("a daemon with no Envoy listener forgets without asking", func(t *testing.T) {
		h, asked := processless(t, every, answer{session, "live", nil})
		h.deps.RoleHolder = nil
		h.restart()
		h.must(RequestForgetSession{Claim: h.token, Session: session, By: SuspendedByOperator})
		if *asked != 0 {
			t.Fatalf("the listener was asked %d times, want none: the daemon has none", *asked)
		}
		if c := h.claim(); c.Session != "" {
			t.Fatalf("the claim still records %q", c.Session)
		}
	})
	t.Run("a store that does not answer the hold refuses with its error", func(t *testing.T) {
		h, asked := processless(t, every, answer{session, "live", nil})
		down := errors.New("the database is unreachable")
		h.store.fail("HoldSessions", down)
		err := h.handle(RequestForgetSession{Claim: h.token, Session: session, By: SuspendedByOperator})
		var refused *RefusedError
		if !errors.Is(err, down) || errors.As(err, &refused) || errors.Is(err, wait.ErrWaiting) {
			t.Fatalf("the forget while the store fails = %v; want the store's error, no refusal and no wait", err)
		}
		if *asked != 0 {
			t.Fatalf("the listener was asked %d times, want none: the hold was never answered", *asked)
		}
		if c := h.claim(); c.Session != session {
			t.Fatalf("the claim records %q, want %s kept", c.Session, session)
		}
	})
}

// The controller's forget of a session its claim records but does not hold waits for the claim's
// role as the operator's does (mayForget), which runs after the controller's own checks (askedBy,
// Claim.HeldAgainst) admit it: refused as a wait, the session kept, while the Envoy listener
// names that session as the role's live holder, and admitted once no claim names the role, the
// claim then recording that the controller asked (forgottenBy). The rig is
// TestAForgetOfASessionTheClaimDoesNotHoldWaitsForItsRole's processless claim, its agent stopped
// by the controller rather than the operator.
//
// The control: forgetSession with no mayForget: the controller's forget clears the session while
// the listener names it as the role's holder.
func TestAControllersForgetOfASessionTheClaimDoesNotHoldWaitsForItsRole(t *testing.T) {
	const holder = claim.Token("legion-legion-LEGION-209-tester")
	h := newHarness(t)
	h.reach(StateIdle)
	h.must(RequestStop{Claim: h.token, By: SuspendedByController})
	h.wantState(StateRetired)
	for _, name := range RecordedHolds(session, sessionFile) {
		h.store.setHold(name, holder)
	}
	asked, named, state := 0, session, "live"
	h.deps.RoleHolder = func(_ context.Context, role string) (oauth2call.RemoteText, string, error) {
		asked++
		if role != string(h.token) {
			t.Errorf("the forget looked up role %s, want the claim's own, %s", role, h.token)
		}
		return oauth2calltest.Said(named), state, nil
	}
	h.restart()

	err := h.handle(RequestForgetSession{Claim: h.token, Session: session, By: SuspendedByController})
	var refused *RefusedError
	if !errors.As(err, &refused) || !errors.Is(err, ErrForgetWaitsForRole) {
		t.Fatalf("the controller's forget while the listener names %s as the role's live holder = %v, want mayForget's wait", session, err)
	}
	for _, want := range []string{"legion claims suspend --claim " + string(holder), suspendTool} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal = %v, want it to name %q", err, want)
		}
	}
	if asked != 1 {
		t.Errorf("the listener was asked %d times, want once", asked)
	}
	if c := h.claim(); c.Session != session || c.SessionFile != sessionFile {
		t.Fatalf("after the refused forget the claim records %q at %q, want %s kept", c.Session, c.SessionFile, session)
	}

	// Once no claim names the role, the controller's forget clears the session.
	named, state = "", ""
	h.must(RequestForgetSession{Claim: h.token, Session: session, By: SuspendedByController})
	if c := h.claim(); c.Session != "" || c.Abandoned.Session != session || !strings.Contains(c.Abandoned.Why, "the project's controller asked") {
		t.Fatalf("the claim records %q (abandoned %q because %q), want %s cleared at the controller's request", c.Session, c.Abandoned.Session, c.Abandoned.Why, session)
	}
}

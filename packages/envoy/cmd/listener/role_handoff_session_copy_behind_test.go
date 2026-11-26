package main

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The two tests below hold the role routes' hand-off to a stopped Legion agent's daemon to the
// record of the role's holder as the session bucket stream's leader holds it. A session that has
// just taken a role over from a stopped Legion agent, through another listener, holds it live, so
// a role message is that session's, and the stopped agent's daemon is handed nothing. This
// listener's session cache follows the bucket through a watcher and has not applied that
// session's registration yet, and on a replicated bucket the copy a direct get reaches may not
// have applied it either: it answers that the session has no record. Judged by that read the
// holder is lapsed, and the hand-off, which nothing takes back, gives the daemon the message
// while the session that took the role never receives it. role_handoff_copy_behind_test.go holds
// the same routes to the claim as the role bucket stream's leader holds it. Each test's control
// is the same message while the session bucket's copy is current.

// sessionTakenRole is a role a stopped Legion agent of the project infra held, whose registry
// entry is the newest of its role, and that another session has just taken over through claimant,
// another listener on the same buckets. observer is the listener under test: its session cache
// applied neither session's registration (trailingListeners), and where the row puts the copy
// behind, its direct gets of the session bucket answer the new holder absent (behindForSession).
// heardNext is what the new holder receives on its agent subject.
type sessionTakenRole struct {
	observer  *listenerDeps
	role      string
	heardNext <-chan contracts.Envelope
}

// takeRoleThroughAnotherListener builds a sessionTakenRole. stopped registers and claims the role
// through claimant, then its session record goes and its claim stands, as a Legion agent that
// stopped leaves them; next, whose stand-in the test hears, then registers and claims the role
// through claimant. behind puts the copy observer's direct gets reach behind next's registration.
func takeRoleThroughAnotherListener(t *testing.T, stopped, next string, behind bool) sessionTakenRole {
	t.Helper()
	claimant, observer := trailingListeners(t)
	observer.confirmed = newConfirmedRecord(t)
	entry := legionAgentEntry(stopped, "INFRA-206", "reviewer")
	withLegionAgent(t, observer, entry)
	claimRole(t, claimant, stopped, entry.RoleToken)
	if err := claimant.sessions.Delete(context.Background(), stopped); err != nil {
		t.Fatalf("remove %s's session record: %v", stopped, err)
	}
	heardNext := startStandIn(t, testnats.URL(t), next)
	claimRole(t, claimant, next, entry.RoleToken)
	if behind {
		behindForSession(t, observer, next)
	} else if _, err := observer.sessions.Get(next); err == nil {
		t.Fatalf("the observer's session cache holds %s, so the resolutions' reads of the bucket are not under test", next)
	}
	return sessionTakenRole{observer: observer, role: entry.RoleToken, heardNext: heardNext}
}

// requireNoHandoff fails the test when the stream holds a hand-off to stopped's daemon.
func (taken sessionTakenRole) requireNoHandoff(t *testing.T, stopped, next string) {
	t.Helper()
	if count, _ := handoffs(t, taken.observer.client, stopped); count != 0 {
		t.Fatalf("the stream holds %d hand-offs for %s, want none: %s took %s over and holds it live, so the message is its", count, stopped, next, taken.role)
	}
}

// requireReachedNext fails the test unless next, the session that took the role, receives
// eventID, the one role message the test sends.
func (taken sessionTakenRole) requireReachedNext(t *testing.T, next, eventID string) {
	t.Helper()
	if heard := testwait.Receive(t, "the session "+next+" that took the role to receive the role message "+eventID, taken.heardNext); heard.EventID != eventID {
		t.Fatalf("the session %s that took the role received %s, want the role message %s", next, heard.EventID, eventID)
	}
}

// A publish to a Legion role whose stopped agent another session took the role over from,
// through another listener. On a session bucket copy behind that session's registration, the
// publish route's first resolution finds the holder with no record, lapsed (sendToRole), and the
// route confirms that miss before it hands the message off (roleSend.noHolder), with the holder's
// record read from the session bucket stream's leader. The leader holds the session that took the
// role, live, so nothing is handed off: the message is kept, 202, and the kept lane's handover
// gives it to that session. When the confirmation read the record with a direct get, the copy
// answered it absent again, and the publish was handed to the stopped agent's daemon, 200: the
// session that took the role never received it. Control: with a current copy the route finds
// that session live and forwards the publish to it, 200.
func TestARolePublishReachesTheSessionThatTookTheRoleFromAStoppedAgentWhileTheSessionsCopyIsBehind(t *testing.T) {
	for _, tc := range []struct {
		name   string
		behind bool
		status int
		kept   bool
	}{
		{"the session bucket's copy is behind the new holder's registration", true, http.StatusAccepted, true},
		{"control: the session bucket's copy is current", false, http.StatusOK, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const stopped, next = "ses_publish_sessions_behind_stopped", "ses_publish_sessions_behind_next"
			taken := takeRoleThroughAnotherListener(t, stopped, next, tc.behind)

			encoded, err := json.Marshal(roleMessage(taken.role, "", "what did the review find?"))
			if err != nil {
				t.Fatal(err)
			}
			recorder, _ := publishThrough(taken.observer, string(encoded))
			taken.requireNoHandoff(t, stopped, next)
			var answer keptPublishAnswer
			if recorder.Code != tc.status || json.Unmarshal(recorder.Body.Bytes(), &answer) != nil || answer.Kept != tc.kept {
				t.Fatalf("a publish to %s after %s took the role over from %s: status %d, body %s; want %d with kept %t",
					taken.role, next, stopped, recorder.Code, recorder.Body.String(), tc.status, tc.kept)
			}
			taken.requireReachedNext(t, next, answer.EventID)
		})
	}
}

// Dispatch marks a send with a Legion role and aims it at the role's stopped agent, after another
// session took the role over through another listener. The send route reads the claim from the
// leader before the send's target (answerRoleSend) and finds that session, and its resolution of
// that holder (handOffOrKeepRoleSend) reads the holder's record from the session bucket stream's
// leader. So, on a session bucket copy behind that session's registration, the send is kept for
// that session, 202 kept, the kept lane's handover gives it to the session, and the stopped
// agent's daemon is handed nothing. When the resolution read the record with a direct get, the
// copy answered it absent, the holder read lapsed, and the send was handed to the stopped agent's
// daemon, 200: the session that took the role never received it. Control: with a current copy the
// same send is kept for that session and reaches it.
func TestAMarkedSendReachesTheSessionThatTookTheRoleFromAStoppedAgentWhileTheSessionsCopyIsBehind(t *testing.T) {
	for _, tc := range []struct {
		name   string
		behind bool
	}{
		{"the session bucket's copy is behind the new holder's registration", true},
		{"control: the session bucket's copy is current", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const stopped, next = "ses_marked_sessions_behind_stopped", "ses_marked_sessions_behind_next"
			const principal = "system:serviceaccount:dispatch:example-dispatch"
			taken := takeRoleThroughAnotherListener(t, stopped, next, tc.behind)
			useDispatchPrincipal(taken.observer, principal)
			mux := http.NewServeMux()
			mux.Handle("/v1/messages/send", sendHandler(taken.observer, logging.New("test")))

			got := sendMarkedByDispatch(t, mux, principal, stopped, taken.role, "what did the review find?")
			taken.requireNoHandoff(t, stopped, next)
			if got.status != http.StatusAccepted || !got.Kept {
				t.Fatalf("a send marked with %s to %s after %s took the role over: status %d, body %s; want 202 kept for %s",
					taken.role, stopped, next, got.status, got.raw, next)
			}
			taken.requireReachedNext(t, next, got.EventID)
		})
	}
}

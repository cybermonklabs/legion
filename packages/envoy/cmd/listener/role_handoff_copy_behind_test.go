package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
)

// The two tests below hold the role routes' hand-off to a stopped Legion agent's daemon to the
// claim as the role bucket stream's leader holds it. A session that has just taken a role over
// from a stopped Legion agent holds it live, so a role message is that session's, and the stopped
// agent's daemon is handed nothing. On a copy of a replicated role bucket that has not applied
// the takeover, a direct get of the claim still names the stopped agent
// (role_claim_copy_behind_test.go's staleClaimKV). Judged by that read the role has no live
// holder, and the hand-off, which nothing takes back, gives the daemon the message while the
// session that took the role never receives it. Each test's control is the same message while the
// copy is current.

// takenRole is a role a stopped Legion agent held and another session has just taken over, on a
// delivererRig whose registry reads the role bucket through a copy that can fall behind the
// takeover (staleClaimKV). stale is the stopped agent's claim, which that copy answers once it
// falls behind, and heardNext is what the session that took the role received.
type takenRole struct {
	*delivererRig
	role       string
	copyBehind *staleClaimKV
	stale      natsgo.KeyValueEntry
	heardNext  *heardFrames
}

// takeRoleFromAStoppedAgent builds a takenRole's rig. stopped is a Legion agent of the project
// infra, which answers, and its registry entry is the newest of its role: it registers and claims
// the role, then its session record goes and its claim stands, as a Legion agent that stopped
// leaves them. next, whose stand-in the rig hears, then registers and claims the role from it.
func takeRoleFromAStoppedAgent(t *testing.T, stopped, next string) takenRole {
	t.Helper()
	rig := newDelivererRig(t)
	entry := legionAgentEntry(stopped, "INFRA-206", "reviewer")
	role := entry.RoleToken
	withLegionAgent(t, rig.deps, entry)
	copyBehind := &staleClaimKV{KeyValue: registryRoleKV(t, rig.harness.registry), role: role}
	setRegistryRoleKV(t, rig.harness.registry, copyBehind)
	rig.subscribe(t, consumerRegistration(stopped, "epoch-1"))
	rig.claim(t, stopped, role, "")
	bucket, err := rig.harness.client.JS().KeyValue(bus.RoleClaimBucket)
	if err != nil {
		t.Fatalf("open the role bucket: %v", err)
	}
	stale, err := bucket.Get(role)
	if err != nil {
		t.Fatalf("read %s's claim by %s: %v", role, stopped, err)
	}
	if err := rig.deps.sessions.Delete(context.Background(), stopped); err != nil {
		t.Fatalf("remove %s's session record: %v", stopped, err)
	}
	nextStandIn := startSessionStandIn(t, rig.url, next, answerDone)
	heardNext := hear(t, nextStandIn)
	rig.subscribe(t, consumerRegistration(next, "epoch-1"))
	rig.claim(t, next, role, "")
	return takenRole{delivererRig: rig, role: role, copyBehind: copyBehind, stale: stale, heardNext: heardNext}
}

// placeCopy puts the copy that direct gets of the role bucket reach behind the takeover when
// behind is set, so that it names the stopped agent, and fails the test unless the registry's
// direct read of the claim names want.
func (taken takenRole) placeCopy(t *testing.T, behind bool, want string) {
	t.Helper()
	if behind {
		taken.copyBehind.fallBehind(taken.stale)
	}
	if claim, err := taken.harness.registry.RoleClaim(context.Background(), taken.role); err != nil || claim.HolderSessionID != want {
		t.Fatalf("the registry's direct read of %s's claim names %q (%v), want %q: the copy is not as the row needs it", taken.role, claim.HolderSessionID, err, want)
	}
}

// requireNoHandoff fails the test when the stream holds a hand-off to stopped's daemon.
func (taken takenRole) requireNoHandoff(t *testing.T, stopped, next string) {
	t.Helper()
	if count, _ := handoffs(t, taken.harness.client, stopped); count != 0 {
		t.Fatalf("the stream holds %d hand-offs for %s, want none: %s took %s over and holds it live, so the message is its", count, stopped, next, taken.role)
	}
}

// requireReachedNext fails the test unless next, the session that took the role, received
// eventID exactly once within 30 s. The copy stays as the row placed it until then: a copy that
// caught up while the message was still on its way would let a read that a stale copy misleads
// pass.
func (taken takenRole) requireReachedNext(t *testing.T, next, eventID string) {
	t.Helper()
	heard := taken.heardNext.await(30*time.Second, eventID)
	taken.copyBehind.catchUp()
	if !heard {
		t.Errorf("the session %s that took the role received %v in time, want the role message %s", next, taken.heardNext.all(), eventID)
	} else if n := taken.heardNext.count(eventID); n != 1 {
		t.Errorf("the session %s that took the role received the role message %s %d times, want once", next, eventID, n)
	}
}

// Dispatch marks a send with a Legion role and aims it at the role's stopped agent, after another
// session took the role over. The send route reads the claim from the leader before the send's
// target (answerRoleSend) and finds that session, and its resolution of that holder
// (handOffOrKeepRoleSend) reads the claim from the leader too. So the send is kept for that
// session, 202 kept, the kept lane's handover gives it to the session, and the stopped agent's
// daemon is handed nothing. When the resolution read the claim with a direct get, a copy behind
// the takeover named the stopped agent, which is not live, and the send was handed to its daemon,
// 200: the session that took the role never received it. Control: with a current copy the same
// send is kept for that session and reaches it.
func TestAMarkedSendReachesTheSessionThatTookTheRoleFromAStoppedAgentWhileTheClaimsCopyIsBehind(t *testing.T) {
	for _, tc := range []struct {
		name   string
		behind bool
	}{
		{"the role bucket's copy is behind the takeover", true},
		{"control: the role bucket's copy is current", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const stopped, next = "ses_marked_behind_stopped", "ses_marked_behind_next"
			const principal = "system:serviceaccount:dispatch:example-dispatch"
			taken := takeRoleFromAStoppedAgent(t, stopped, next)
			useDispatchPrincipal(taken.deps, principal)
			taken.placeCopy(t, tc.behind, map[bool]string{true: stopped, false: next}[tc.behind])

			got := sendMarkedByDispatch(t, taken.mux, principal, stopped, taken.role, "what did the review find?")
			taken.requireNoHandoff(t, stopped, next)
			if got.status != http.StatusAccepted || !got.Kept {
				t.Fatalf("a send marked with %s to %s after %s took the role over: status %d, body %s; want 202 kept for %s",
					taken.role, stopped, next, got.status, got.raw, next)
			}
			taken.requireReachedNext(t, next, got.EventID)
		})
	}
}

// A publish to a Legion role whose stopped agent another session took the role over from. On a
// copy behind the takeover a direct get names the stopped agent, so the publish route's
// resolution finds the holder not live (sendToRole), and the route confirms that miss with a
// resolution from the leader before it hands the message off (roleSend.noHolder). The leader names
// the session that took the role, live, so nothing is handed off: the message is kept, 202, and
// the kept lane's handover gives it to that session. Without the confirmation the publish was
// handed to the stopped agent's daemon, 200, and the session that took the role never received
// it. Control: with a current copy the route finds that session live and forwards the publish to
// it, 200.
func TestARolePublishReachesTheSessionThatTookTheRoleFromAStoppedAgentWhileTheClaimsCopyIsBehind(t *testing.T) {
	for _, tc := range []struct {
		name   string
		behind bool
		status int
		kept   bool
	}{
		{"the role bucket's copy is behind the takeover", true, http.StatusAccepted, true},
		{"control: the role bucket's copy is current", false, http.StatusOK, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const stopped, next = "ses_publish_behind_stopped", "ses_publish_behind_next"
			taken := takeRoleFromAStoppedAgent(t, stopped, next)
			taken.placeCopy(t, tc.behind, map[bool]string{true: stopped, false: next}[tc.behind])

			encoded, err := json.Marshal(roleMessage(taken.role, "", "what did the review find?"))
			if err != nil {
				t.Fatal(err)
			}
			recorder := httptest.NewRecorder()
			taken.mux.ServeHTTP(recorder, postJSON("/v1/messages/publish", string(encoded)))
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

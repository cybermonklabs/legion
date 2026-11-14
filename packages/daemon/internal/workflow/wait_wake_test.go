package workflow

import (
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// wakeWaitID is the id of the wait the wake rig's implementer declared before its pod was released.
const wakeWaitID = "wait-legion-208-implementer"

// waits is the wait readings of the wait tests (waitRig) over the wake rig's database, engine and
// log: what claim_waits holds of a wait, whether it stands, and the `wait: ended` lines.
func (rig wakeRig) waits(t *testing.T) *waitRig {
	return &waitRig{t: t, pool: rig.pool, engine: rig.engine, logs: rig.logged}
}

// declareWait records the wait the rig's implementer declared in its session before the release
// took its pod, as the API applies a declaration (intake.WaitDeclared): on its issue's
// implementing phase at the generation the pod was serving, an hour before the wakes below, its
// time an hour after them. It answers the wait recorded, which stands.
func (rig wakeRig) declareWait(t *testing.T) record.ClaimWait {
	t.Helper()
	if result := rig.apply("wait-declared:"+wakeWaitID, intake.WaitDeclared{Token: wakeToken(t, claim.RoleImplementer),
		Issue: "LEGION-208", Role: claim.RoleImplementer, Session: "session-implementer", Generation: wakeIssueGeneration,
		WaitID: wakeWaitID, WaitingOn: "the review of LEGION-208's pull request", WakeAt: wakeAt.Add(time.Hour),
		At: wakeAt.Add(-time.Hour)}); result.Refusal != nil {
		t.Fatalf("the declaration of %s was refused: %+v", wakeWaitID, *result.Refusal)
	}
	waits := rig.waits(t)
	wait := waits.wait(wakeWaitID)
	if !waits.stands(rig.engine, wait) {
		t.Fatalf("the implementer's wait %+v does not stand before the wake", wait)
	}
	return wait
}

// A wake of the claim's released pod whose cause is news ends the claim's wait in the wake's own
// transaction, before its start row is queued (dispatch://INFRA-2040 R3's third end): a CI
// settlement on the head of its pull request (pods.WakeChecks), a message published to its role
// topic (pods.WakeRoleMessage) and a message sent to its session (pods.WakeAgentMessage). The
// cause names the wake and its detail, the end names the wake's fact and is written at the
// engine's now, and the end is logged. The wake's start row is queued all the same, so its task is
// delivered once the role is back, and no wait holds it.
//
// The backstop's wake (pods.WakeBackstop) is the release's own timer, not news, and leaves the
// wait standing; its start row is queued as before. pods.WakeDelivery is no wake of
// wakeReleased's: a delivery resumes the claim itself (internal/daemon's wakeForDelivery).
func TestAWakeForNewsEndsTheWaitAndItsTaskIsQueued(t *testing.T) {
	for _, tc := range []struct {
		name string
		id   string
		fact func(t *testing.T, rig wakeRig) intake.Fact
		kind string
		// cause is the end the wake writes, "" for a wake that ends nothing.
		cause func(t *testing.T) string
		names []string
	}{
		{
			name: "a CI settlement", id: "settled", kind: pods.WakeChecks,
			fact:  func(*testing.T, wakeRig) intake.Fact { return settlement("head1", "build") },
			cause: func(*testing.T) string { return "woken: checks head1 failed build" },
			names: []string{"head1", "build"},
		},
		{
			name: "a message to the role", id: "role-message", kind: pods.WakeRoleMessage,
			fact: func(t *testing.T, _ wakeRig) intake.Fact {
				return intake.ListenerWake{Kind: intake.ListenerWakeRole, Role: string(wakeToken(t, claim.RoleImplementer)),
					At: wakeAt, Report: "report-1"}
			},
			cause: func(t *testing.T) string { return "woken: role_message " + string(wakeToken(t, claim.RoleImplementer)) },
			names: []string{"role topic"},
		},
		{
			name: "a message to the session", id: "agent-message", kind: pods.WakeAgentMessage,
			fact: func(*testing.T, wakeRig) intake.Fact {
				return intake.ListenerWake{Kind: intake.ListenerWakeAgent, Session: "session-implementer", At: wakeAt, Report: "report-2"}
			},
			cause: func(*testing.T) string { return "woken: agent_message session-implementer" },
			names: []string{"session-implementer", "sent to your session"},
		},
		{
			name: "the backstop", id: "backstop", kind: pods.WakeBackstop,
			fact: func(t *testing.T, rig wakeRig) intake.Fact {
				id := armBackstop(t, rig.pool, claim.RoleImplementer, wakeAt.Add(-time.Minute))
				return intake.PodBackstopDue{ID: id, Issue: "LEGION-208", Role: claim.RoleImplementer}
			},
			names: []string{"backstop"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newWakeRig(t)
			wait := rig.declareWait(t)
			waits := rig.waits(t)

			rig.apply(tc.id, tc.fact(t, rig))

			oneWake(t, rig.pool, claim.RoleImplementer, tc.kind, tc.names...)
			if tc.cause == nil {
				waits.assertStanding(rig.engine, wait)
				return
			}
			cause, endedBy := tc.cause(t), "api/"+tc.id
			ended := waits.wait(wait.ID)
			if ended.EndCause != cause || ended.EndedBy != endedBy || !ended.EndedAt.Equal(wakeAt) {
				t.Fatalf("wait %s ended with cause %q by %q at %s, want cause %q by %q at the wake's %s",
					wait.ID, ended.EndCause, ended.EndedBy, ended.EndedAt, cause, endedBy, wakeAt)
			}
			line := waits.endedLine(wait.ID)
			for _, field := range [][2]string{{"claim", string(wait.Token)}, {"issue", "LEGION-208"},
				{"role", string(claim.RoleImplementer)}, {"wait", wait.ID}, {"cause", cause}, {"ended_by", endedBy}} {
				if want := loggedField(field[0], field[1]); !carries(line, want) {
					t.Fatalf("the line %q does not carry %s", line, want)
				}
			}
			if waits.stands(rig.engine, wait) {
				t.Fatalf("wait %s still stands after the wake ended it", wait.ID)
			}
		})
	}
}

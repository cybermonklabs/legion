package daemon

// A round with no role process in its pod (dispatch://INFRA-4109 R7). On INFRA's daemon on
// 2026-11-19, INFRA-4455's round stood over a pod no agent ran in from 18:08:41Z. The sweep logged
// every minute that the round waited for a claim to run again, and held it past its bound. The
// wake pacing then launched the tester into that pod at 18:37:48Z, and the round, past its bound,
// replaced the pod 18 s into the tester's first turn, which cost a second paced launch.
//
// So a round whose pod the issue's Sandbox still holds, with no role process in it, ends by
// replacing that pod by issue (runtime.IssuePodReplacer) once nothing holds it (roundPodEnds).
// No process can read the disk, so the issue is told which members had not answered and that no
// process could read the workspace. A launch into such a pod reads the same rule when it runs
// (supervision.roundPodEndsAtLaunch, the Sandbox runtime's RoundPodEnds hook): where nothing holds
// the pod, the launch replaces it as the round's own replacement and starts its role in a new pod,
// so the role is launched once. These cases drive the pod-update rig of podupdate_test.go, with
// the claims seeded in the round and restored as a boot restores them.

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// podlessKeptLine is what the replacement by issue logs where its own read of the claims keeps
// the pod (issuePodKept).
const podlessKeptLine = "supervise: the pod of this update round is kept now the sweep has reached its replacement by issue; nothing was replaced and the round stays open"

// podlessMember is a member of the round the rig opens at its start, with no process in the pod,
// as the store holds it and a boot restores it: asked, and in state with the suspension by, which
// is "" for a claim that is not suspended. answered is whether it answered the round.
func podlessMember(role claim.Role, state supervise.ClaimState, by supervise.SuspendSource, answered bool) podUpdateRole {
	member := suspendedRole(role, by)
	member.state = state
	member.round, member.reasons, member.signalled = podUpdateRound, podUpdateDrift, answered
	return member
}

// answeredThenSuspended is a member that answered the round, which the daemon then suspended: the
// ordinary end of a phase.
func answeredThenSuspended(role claim.Role) podUpdateRole {
	return podlessMember(role, supervise.StateSuspended, supervise.SuspendedByDaemon, true)
}

// issueReplacements is every replacement by issue the runtime made (fake's ReplaceIssuePod), a
// refused one of the runtime's own included and none the last word refused.
func (r *podUpdateRig) issueReplacements() []fake.Call {
	return r.rt.CallsOf("ReplaceIssuePod")
}

// byIssueAsks is every call to ReplaceIssuePod however it ended: made, refused by the last word,
// or with no pod of the round to end.
func (r *podUpdateRig) byIssueAsks() []fake.Call {
	var asked []fake.Call
	for _, call := range r.rt.Calls() {
		switch call.Method {
		case "ReplaceIssuePod", "ReplaceIssuePodNotDue", "ReplaceIssuePodNoPod":
			asked = append(asked, call)
		}
	}
	return asked
}

// awaitIssueReplacement sweeps until the runtime has made one replacement by issue, and answers
// it, as awaitReplacement does for ReplacePod: a sweep that reads a machine still deciding keeps
// the pod for that sweep (issuePodKept), so the question is not one sweep's.
func (r *podUpdateRig) awaitIssueReplacement(what string) fake.Call {
	r.t.Helper()
	testwait.Eventually(r.t, "a replacement by issue of the pod "+what, func() bool {
		if len(r.issueReplacements()) > 0 {
			return true
		}
		r.tick()
		return len(r.issueReplacements()) > 0
	})
	calls := r.issueReplacements()
	if len(calls) != 1 {
		r.t.Fatalf("the pod was replaced by issue %d times %s, want once", len(calls), what)
	}
	return calls[0]
}

// wantPodlessEnd holds a round the sweep replaced by issue to what such an end owes
// (wantRoundPodEnd, with the line naming the replacement by issue).
func wantPodlessEnd(t *testing.T, rig *podUpdateRig, bounded bool, lost []claim.Role, answered ...claim.Role) {
	t.Helper()
	wantRoundPodEnd(t, rig, "byIssue", bounded, lost, answered...)
}

// wantRoundPodEnd holds a round ended by the replacement of a pod no role process ran in to what
// such an end owes: the line of a replacement the round made, which carries by as true — the
// field that says who made it, the sweep by issue ("byIssue") or a launch into the pod
// ("atLaunch") — and says whether the bound made it, names each member in lost as one that had
// not answered (unansweredMembers) and says why no read of the workspace was made; and one message
// on the issue that names each member in lost with its state, names no member that answered, and
// says that no process could read the workspace. A round whose members all answered writes no
// message.
func wantRoundPodEnd(t *testing.T, rig *podUpdateRig, by string, bounded bool, lost []claim.Role, answered ...claim.Role) {
	t.Helper()
	lines := loggedLines(t, rig.logs.String(), podUpdateReplacedLine)
	if len(lines) != 1 {
		t.Fatalf("the replacement was logged %d times, want once: %+v", len(lines), lines)
	}
	line := lines[0]
	if line[by] != true || line["round"] != podUpdateRound || line["pod"] != podUpdatePod || line["atTheBound"] != bounded {
		t.Errorf("the replacement's line is %+v, want round %s of pod %s replaced with %s, atTheBound=%t", line, podUpdateRound, podUpdatePod, by, bounded)
	}
	if line["workspaceRead"] != "not made" || line["workspaceNotReadBecause"] != noProcessRead {
		t.Errorf("the replacement's line names the read %+v, want one not made because %q", line, noProcessRead)
	}
	wantField(t, line, "unansweredMembers", lost...)
	if bound := loggedLines(t, rig.logs.String(), podUpdateBoundLine); bounded != (len(bound) == 1) || len(bound) > 1 {
		t.Errorf("the replacement was logged at the bound %d times, want the bound's line %t: %+v", len(bound), bounded, bound)
	}

	messages := rig.messages()
	if len(lost) == 0 {
		if len(messages) != 0 {
			t.Fatalf("the issue was written %q, want nothing: every member of the round answered", messages)
		}
		return
	}
	if len(messages) != 1 {
		t.Fatalf("the issue was written %d messages, want one: %q", len(messages), messages)
	}
	for _, role := range lost {
		if !strings.Contains(messages[0], string(role)+" (") {
			t.Errorf("the message does not name the %s, a member that had not answered:\n%s", role, messages[0])
		}
	}
	for _, role := range answered {
		if strings.Contains(messages[0], string(role)) {
			t.Errorf("the message names the %s, which answered the round:\n%s", role, messages[0])
		}
	}
	if !strings.Contains(messages[0], "No role process ran in the pod") {
		t.Errorf("the message does not say that no role process could read the workspace:\n%s", messages[0])
	}
}

// The round the rig opens at its start, over the pod the issue's Sandbox still holds, with every
// member's process gone from it. Where nothing holds the pod, the sweep replaces it by issue: once
// before the bound, where every member answered or the round passes it over, or where the only
// member the ask reached that has not answered was suspended by the daemon after the ask; and once
// past the bound, whatever the members answered. Each replacement ends the round on every member,
// is logged as the round's own, and writes one message on the issue that names each member that
// had not answered and says that no process could read the workspace.
//
// The pod is kept, and no replacement by issue is asked for, where something holds it: an
// operator's suspension of a member, before and past the bound, which past the bound is told to
// the controller once; before the bound, a member the round waits for that holds an ask it has
// not answered, since its process went while it held the ask; a project pause's hold on the pod,
// past the bound too; and, before the bound, a member a project's pause stopped that the round
// never asked, while the pause still keeps the pod after its hold ended: before its held start
// runs, and while the wake pacing parks its launch.
func TestAPodlessRoundIsReplacedByIssueOnceNothingHoldsIt(t *testing.T) {
	var (
		// heldAsk is a member that holds the round's ask: its process went while it held the ask,
		// and no launch has run since, so the boot restored it as launch_uncertain.
		heldAsk = podlessMember(claim.RoleImplementer, supervise.StateLaunchUncertain, "", false)
		// operatorHeld is a member an operator suspended.
		operatorHeld = podlessMember(claim.RoleImplementer, supervise.StateSuspended, supervise.SuspendedByOperator, false)
	)
	for _, tc := range []struct {
		name  string
		roles []podUpdateRole
		// at is when the sweeps run.
		at time.Time
		// pause opens a project pause's hold on the pod half an hour past the round's bound, so
		// the bound it moved is past at.
		pause bool
		// replaced is whether the sweep replaces the pod by issue, and bounded whether the bound
		// let that replacement through over a member the round waits for.
		replaced, bounded bool
		// lost is every member the replacement names as not having answered, and answered every
		// member it names nowhere.
		lost, answered []claim.Role
		// told is whether the controller is told of the round, once.
		told bool
	}{
		{
			name: "before the bound with every member answered or passed over",
			roles: []podUpdateRole{answeredThenSuspended(claim.RoleTester),
				podlessMember(claim.RoleReviewer, supervise.StateSuspended, supervise.SuspendedByReleased, false)},
			at: podUpdateStart, replaced: true,
			lost: []claim.Role{claim.RoleReviewer}, answered: []claim.Role{claim.RoleTester},
		},
		{
			name:  "past the bound",
			roles: []podUpdateRole{heldAsk, answeredThenSuspended(claim.RoleTester)},
			at:    podUpdateBound, replaced: true, bounded: true,
			lost: []claim.Role{claim.RoleImplementer}, answered: []claim.Role{claim.RoleTester},
		},
		{
			name:  "not before the bound while a member the round waits for holds an unanswered ask",
			roles: []podUpdateRole{heldAsk, answeredThenSuspended(claim.RoleTester)},
			at:    podUpdateBound.Add(-time.Minute),
		},
		{
			name:  "not while an operator holds a member, before the bound",
			roles: []podUpdateRole{operatorHeld, answeredThenSuspended(claim.RoleTester)},
			at:    podUpdateStart,
		},
		{
			name:  "not while an operator holds a member, past the bound",
			roles: []podUpdateRole{operatorHeld, answeredThenSuspended(claim.RoleTester)},
			at:    podUpdateBound.Add(time.Hour), told: true,
		},
		{
			name: "a member suspended after its ask holds nothing",
			roles: []podUpdateRole{podlessMember(claim.RoleImplementer, supervise.StateSuspended, supervise.SuspendedByDaemon, false),
				answeredThenSuspended(claim.RoleTester)},
			at: podUpdateStart, replaced: true,
			lost: []claim.Role{claim.RoleImplementer}, answered: []claim.Role{claim.RoleTester},
		},
		{
			name: "not while the project's pause holds the pod",
			roles: []podUpdateRole{podlessMember(claim.RoleImplementer, supervise.StateSuspended, supervise.SuspendedByReleased, false),
				answeredThenSuspended(claim.RoleTester)},
			at: podUpdateBound.Add(time.Hour), pause: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newPodUpdateRig(t, tc.roles...)
			rig.rt.ScriptIssuePod(podUpdateIssue, podUpdatePod)
			if tc.pause {
				heldAt := podUpdateBound.Add(30 * time.Minute)
				holdLegionPod(t, rig.st, openLegionPause(t, rig.pool, heldAt), podUpdateIssue, heldAt)
			}
			rig.clockAt(tc.at)

			if !tc.replaced {
				for range 3 {
					rig.tick()
				}
				rig.settleAll()
				if asked := rig.byIssueAsks(); len(asked) != 0 {
					t.Fatalf("the runtime was asked to replace the pod by issue %+v, want never while something holds it", asked)
				}
				for _, role := range tc.roles {
					if c := rig.claimOf(role.role); c.UpdateRound != podUpdateRound {
						t.Errorf("the %s is in round %q, want the round kept open over its pod", role.role, c.UpdateRound)
					}
				}
				if messages := rig.messages(); len(messages) != 0 {
					t.Errorf("the issue was written %q of a pod that is kept", messages)
				}
				notices := rig.notices()
				switch {
				case tc.told && (len(notices) != 1 || !strings.Contains(notices[0].Reason, "An operator suspended")):
					t.Errorf("the controller was told %+v, want once of the operator's suspension that holds the round past its bound", notices)
				case !tc.told && len(notices) != 0:
					t.Errorf("the controller was told %+v, want nothing", notices)
				}
				return
			}

			replaced := rig.awaitIssueReplacement(tc.name)
			if replaced.Issue != podUpdateIssue || replaced.PodUID != podUpdatePod {
				t.Errorf("the pod was replaced by issue as %+v, want issue %s and the round's pod %s", replaced, podUpdateIssue, podUpdatePod)
			}
			for _, role := range tc.roles {
				rig.awaitClaim(role.role, "its round to end with the replacement by issue", func(c supervise.Claim) bool { return c.UpdateRound == "" })
			}
			wantPodlessEnd(t, rig, tc.bounded, tc.lost, tc.answered...)
			if calls := rig.replacements(); len(calls) != 0 {
				t.Errorf("ReplacePod called %+v, want no replacement through a process: none runs in the pod", calls)
			}
			if notices := rig.notices(); len(notices) != 0 {
				t.Errorf("the controller was told %+v of a round its replacement by issue ended", notices)
			}

			rig.tick()
			rig.tick()
			if calls := rig.issueReplacements(); len(calls) != 1 {
				t.Errorf("the pod was replaced by issue %d times after two more sweeps, want the one", len(calls))
			}
			if messages := rig.messages(); len(messages) != 1 {
				t.Errorf("the issue was written %d messages after two more sweeps, want the one of the round's end: %q", len(messages), messages)
			}
		})
	}

	// A member a project's pause stopped, suspended released, which the round never asked. The
	// pause's hold of the pod ended a minute before the round opened, at the resume or at a role's
	// ready (pauseKeepsThePod). No process runs in the pod and no ask reached the member. But the
	// pause still keeps the pod for the work on its disk (decision 12 of dispatch://INFRA-3759,
	// podHolds.owes), so the sweep asks for no replacement by issue. That holds before the member's
	// held start runs, while the member is still suspended released. It holds after that start
	// too, while the wake pacing parks the launch until ten minutes into the round: the member is
	// then launching with no process, and its machine is in no decision (parkPauseHeldLaunch). At
	// the let-out the pacing runs the launch, as the daemon runs it. The launch keeps the pod and
	// comes up in it with one launch, the round asks the member once it is ready, and the round
	// replaces the pod through the member's process once the member has answered. The sweep never
	// replaces the pod by issue.
	t.Run("not while the project's pause keeps the pod for a member it stopped", func(t *testing.T) {
		pauseKeepsThePod(t, func(t *testing.T, rig *podUpdateRig) {
			rig.wantPodKept(fmt.Sprintf("the held start of the %s", pauseStoppedRole))
			rig.parkedLaunchKeepsThePod(pauseStoppedRole, pauseLetOut)
			if c := rig.claimOf(pauseStoppedRole); c.UpdateRound != podUpdateRound {
				t.Fatalf("the %s is in round %q, want the round kept open over its pod", pauseStoppedRole, c.UpdateRound)
			}

			rig.intoRoundPod(pauseStoppedRole)
			rig.runParkedLaunch(pauseStoppedRole, podUpdateStart, pauseLetOut)
			rig.launchedIntoRoundPod(pauseStoppedRole)
			rig.registerInto(pauseStoppedRole, podUpdatePod)
			rig.awaitAsked(pauseStoppedRole, podUpdateRound)
			rig.signal(pauseStoppedRole, "a1b2c3d")
			rig.awaitReplacement(1, fmt.Sprintf("once the %s answered", pauseStoppedRole))
			lines := loggedLines(t, rig.logs.String(), podUpdateReplacedLine)
			if len(lines) != 1 {
				t.Fatalf("the replacement was logged %d times, want once: %+v", len(lines), lines)
			}
			if lines[0]["atTheBound"] != false {
				t.Errorf("the replacement's line says atTheBound=%v, want false: the %s answered before the bound", lines[0]["atTheBound"], pauseStoppedRole)
			}
			wantField(t, lines[0], "notWaitedFor")
			if asked := rig.byIssueAsks(); len(asked) != 0 {
				t.Errorf("the runtime was asked to replace the pod by issue %+v, want the replacement through the %s's process", asked, pauseStoppedRole)
			}
		})
	})
}

// The sweep's replacement by issue keeps the pod while a claim of the issue is in a decision
// (issuePodKept). A launch into the pod lets the pod's launch turn go before it writes its
// locator, so for that moment no claim records a process in the pod although one is starting
// there, and a replacement made then would cut that process's first turn. The claim the daemon
// suspended before the round's ask reached it is resumed, and its launch is held inside the
// runtime (fake's HoldResume): the sweep then replaces nothing, and says why. Once the launch
// runs, it reads the round itself (roundPodEndsAtLaunch): nothing holds the pod, so the launch
// replaces it as the round's own replacement and comes up in the pod in its place
// (dispatch://INFRA-4109 R7), the sweep ends the round as that replacement, and still nothing
// replaces the pod by issue.
func TestTheByIssueReplacementWaitsForAMachineInADecision(t *testing.T) {
	rig := newPodUpdateRig(t, podUpdateRole{role: claim.RoleImplementer, state: supervise.StateSuspended, session: true,
		contract: api.DaemonAPIVersion, suspendedBy: supervise.SuspendedByDaemon, round: podUpdateRound, reasons: podUpdateDrift, unasked: true})
	rig.drifts(podUpdateDrift)
	rig.rt.ScriptIssuePod(podUpdateIssue, podUpdatePod)

	token := rig.tokens[claim.RoleImplementer]
	holding, let := rig.holdNextResume(claim.RoleImplementer, podUpdatePod)
	rig.sup.post(token, supervise.RequestResume{Claim: token})
	select {
	case <-holding:
	case <-time.After(10 * time.Second):
		t.Fatal("the implementer's launch did not reach Resume")
	}

	rig.tick()
	if asked := rig.byIssueAsks(); len(asked) != 0 {
		t.Fatalf("the runtime was asked to replace the pod by issue %+v while a launch into it was in a decision, want nothing", asked)
	}
	kept := loggedLines(t, rig.logs.String(), podlessKeptLine)
	if len(kept) != 1 {
		t.Fatalf("the sweep logged the pod kept %+v, want once", kept)
	}
	if reason, _ := kept[0]["reason"].(string); !strings.Contains(reason, "in a decision") {
		t.Fatalf("the sweep logged the pod kept because %q, want the machine in a decision named", reason)
	}
	if round := rig.storedRound(claim.RoleImplementer); round != podUpdateRound {
		t.Fatalf("the implementer's stored round is %q, want the round open", round)
	}

	let()
	c := rig.awaitClaim(claim.RoleImplementer, "its launch to record its process", func(c supervise.Claim) bool { return c.Locator != nil })
	pod := podOf(c.Locator)
	if pod == podUpdatePod {
		t.Fatalf("the implementer was launched into the round's pod %s, which nothing holds, want the pod in its place", pod)
	}
	rig.registerInto(claim.RoleImplementer, pod)
	rig.awaitSwept(claim.RoleImplementer, "its round to end as the launch's replacement", func(c supervise.Claim) bool { return c.UpdateRound == "" })
	rig.tick()
	if asked := rig.byIssueAsks(); len(asked) != 0 {
		t.Fatalf("the runtime was asked to replace the pod by issue %+v once the launch ran, want nothing", asked)
	}
	if replaced := rig.rt.CallsOf("ReplaceRoundPodAtLaunch"); len(replaced) != 1 || replaced[0].PodUID != podUpdatePod {
		t.Fatalf("the launch replaced %+v, want the round's pod %s once", replaced, podUpdatePod)
	}
}

// releasedUnasked is a member the daemon released before the round's ask reached it, as
// INFRA-4455's tester was from 18:08:41Z: suspended by an idle release, and never asked. The round
// passes over it while it is suspended (dispatch://INFRA-4109 R1).
func releasedUnasked(role claim.Role) podUpdateRole {
	member := podlessMember(role, supervise.StateSuspended, supervise.SuspendedByReleased, false)
	member.unasked = true
	return member
}

// pauseStoppedRole is the role a project's pause stopped in the cases where the pause keeps the
// round's pod for it (pauseKeepsThePod), and pauseLetOut is the moment the wake pacing lets its
// held launch out: ten minutes into the round, before the round's bound.
const pauseStoppedRole = claim.RoleImplementer

var pauseLetOut = podUpdateStart.Add(10 * time.Minute)

// pacedUntil is a wake pacing that lets every paced launch out at letOut. An operator's resume is
// not paced (supervise's exemptWake).
func pacedUntil(letOut time.Time) supervise.WakePace {
	return supervise.WakePace{Rate: 1, Burst: 1, Reserve: func(time.Time) time.Time { return letOut }}
}

// heldLaunchLetOut is what the round waits for while the wake pacing parks the held launch of
// role, a role a project's pause stopped: the let-out of that launch (wantPodKept).
func heldLaunchLetOut(role claim.Role) string {
	return fmt.Sprintf("the let-out of the %s's held launch", role)
}

// pauseKeepsThePod runs check once for each end of a project pause's hold of the pod after which
// the pause still keeps the pod for a role it stopped: the end at the resume
// (store.PausePodHoldResumed) and the end at a role's ready (store.PausePodHoldReady). Each run
// has a rig of its own. The rig opens its round at its start, and no process runs in the round's
// pod. The round's members are pauseStoppedRole, which the pause stopped, suspended released,
// before the round's ask could reach it (releasedUnasked), and the tester, which answered and
// which the daemon then suspended (answeredThenSuspended). The pause is recorded as the daemon
// records it (recordPause), and the wake pacing lets every paced launch out at pauseLetOut
// (pacedUntil).
func pauseKeepsThePod(t *testing.T, check func(t *testing.T, rig *podUpdateRig)) {
	t.Helper()
	for _, end := range []store.PausePodHoldEnd{store.PausePodHoldResumed, store.PausePodHoldReady} {
		t.Run(string(end), func(t *testing.T) {
			rig := newPodUpdateRigWith(t, podUpdateRigOptions{wakePace: pacedUntil(pauseLetOut)},
				releasedUnasked(pauseStoppedRole), answeredThenSuspended(claim.RoleTester))
			rig.rt.ScriptIssuePod(podUpdateIssue, podUpdatePod)
			rig.recordPause(end, pauseStoppedRole)
			check(t, rig)
		})
	}
}

// recordPause records a project pause of the issue's pod as the daemon records it, for each role
// of stopped, the roles the pause stopped. The pause held the pod and the launch of each stopped
// role from twenty minutes before podUpdateStart: the hold of the pod (holdLegionPod) and the open
// resume of each held wake (recordPodWake, pods.WakePause). The project's resume came a minute
// before podUpdateStart (store.Store.Resume). It ended the pause and started the stuck clock of
// each held launch at its own moment. The hold of the pod ended at that moment too, for the reason
// end. So no hold stands, the pause still owes each stopped role a launch, and no held start has
// run yet (heldStart runs one).
func (r *podUpdateRig) recordPause(end store.PausePodHoldEnd, stopped ...claim.Role) {
	r.t.Helper()
	heldAt, resumedAt := podUpdateStart.Add(-20*time.Minute), podUpdateStart.Add(-time.Minute)
	pause := openLegionPause(r.t, r.pool, heldAt)
	holdLegionPod(r.t, r.st, pause, podUpdateIssue, heldAt)
	for _, role := range stopped {
		recordPodWake(r.t, r.st, store.PodResume{Project: podUpdateProject, Issue: podUpdateIssue, Role: string(role),
			Kind: pods.WakePause, At: heldAt, Pause: pause})
	}
	if _, err := r.st.Resume(r.ctx, store.ResumeRequest{Project: "LEGION",
		By: store.PauseActor{Kind: store.PauseByOperator}, At: resumedAt}); err != nil {
		r.t.Fatalf("resume LEGION: %v", err)
	}
	if ended, err := r.st.EndPausePodHold(r.ctx, "LEGION", podUpdateIssue, end, resumedAt); err != nil || !ended {
		r.t.Fatalf("end the pod's hold for the reason %s = %t, %v; want the hold ended", end, ended, err)
	}
}

// heldStart runs the held start of role, a role a project's pause stopped (recordPause), as the
// outbox runs the start row of the pause's held wake once the resume lets it out: a resume by rule
// of the suspended claim (outbox's start). The rig's wake pacing decides when the launch runs.
func (r *podUpdateRig) heldStart(role claim.Role) {
	r.t.Helper()
	r.handle(role, supervise.RequestRuleResume{Claim: r.tokens[role], Reason: "a wake for the launch the project's pause held"})
}

// writeLetOut writes letOut as the stuck clock of role's open resume, as the daemon writes the
// let-out of each reservation the wake pacing makes (projectPause.wakeLetOut,
// store.Store.SetResumeClock), the one that lets the launch out at once included. The rig wires
// no such write (supervise.Deps.WakeLetOut). The round's bound is the one reader of a let-out
// (roundClock.roundBound), so a test makes the write where that bound must move by it, or where
// it shows that a let-out decides no keep of the pod (podHolds.owes reads none).
func (r *podUpdateRig) writeLetOut(role claim.Role, letOut time.Time) {
	r.t.Helper()
	if moved, err := r.st.SetResumeClock(r.ctx, podUpdateProject, podUpdateIssue, role, letOut); err != nil || !moved {
		r.t.Fatalf("write the let-out of the %s's launch = %t, %v; want the clock of its open resume moved", role, moved, err)
	}
}

// wantLaunchParked holds role to a launch the wake pacing parked until letOut: the claim is
// launching with no process recorded, its machine holds the parked launch until its turn
// (supervise.WakeWaiting, supervise's waitOnMachine), and no launch of the claim was made yet.
func (r *podUpdateRig) wantLaunchParked(role claim.Role, letOut time.Time) {
	r.t.Helper()
	m, _ := r.sup.Machine(r.tokens[role])
	view := m.View()
	parked := view.WakeWaiting
	if parked == nil || parked.Kind != supervise.WakeLaunch || !parked.At.Equal(letOut) ||
		view.State != supervise.StateLaunching || view.Locator != nil || len(r.launchesOf(role)) != 0 {
		r.t.Fatalf("the %s is %s recording %+v with wake %+v after %d launches, want it launching with no process under the launch the pacing parked until %s",
			role, view.State, view.Locator, parked, len(r.launchesOf(role)), letOut)
	}
}

// runParkedLaunch moves the rig's clock to letOut, the turn the wake pacing gave role's launch
// when it parked that launch at parkedAt (wantLaunchParked), and runs the launch as the pacing's
// timer runs it at its turn.
func (r *podUpdateRig) runParkedLaunch(role claim.Role, parkedAt, letOut time.Time) {
	r.t.Helper()
	r.clockAt(letOut)
	if fired := r.clock.fire(letOut.Sub(parkedAt)); fired != 1 {
		r.t.Fatalf("the wake pacing ran %d launches at its turn, want the %s's one", fired, role)
	}
}

// parkPauseHeldLaunch runs the held start of role (heldStart), and the rig's wake pacing parks
// the launch until letOut (pacedUntil, wantLaunchParked). It writes no let-out. The daemon writes
// the let-out on a goroutine of its own after the park (projectPause.wakeLetOut): a read can come
// before that write lands, and a write that fails is only logged. So the stuck clock of the role's
// resume stays the moment of the project's resume (recordPause), and the round's bound does not
// move by the let-out until a test writes it (writeLetOut).
func (r *podUpdateRig) parkPauseHeldLaunch(role claim.Role, letOut time.Time) {
	r.t.Helper()
	r.heldStart(role)
	r.wantLaunchParked(role, letOut)
}

// parkedLaunchKeepsThePod parks the held launch of role until letOut (parkPauseHeldLaunch), and
// holds the round's pod to being kept for that let-out, at the clock as it stands and a minute
// before the let-out (wantPodKept).
func (r *podUpdateRig) parkedLaunchKeepsThePod(role claim.Role, letOut time.Time) {
	r.t.Helper()
	r.parkPauseHeldLaunch(role, letOut)
	r.wantPodKept(heldLaunchLetOut(role))
	r.clockAt(letOut.Add(-time.Minute))
	r.wantPodKept(heldLaunchLetOut(role))
}

// stampsResumes has the rig stamp each role's open resume from the claim's durable transitions,
// as the daemon does (workflowRuntime.podResume, the part of claimDurableState that measures a
// resume): a role's ready stamps the ready of its pod (store.Store.MarkPodResumeReady), and its
// first turn ends the resume. The stamps run on a goroutine of their own (awaitResumeReady). A rig
// that leaves this out stamps nothing.
func (r *podUpdateRig) stampsResumes() {
	w := &workflowRuntime{pool: r.pool, projectID: podUpdateProject, dispatchProject: "LEGION", idleStore: r.st, log: r.log, now: r.clock.Now}
	r.sup.OnDurableState(w.podResume)
}

// awaitResumeReady waits for the ready of role to be stamped on its open resume (stampsResumes),
// and holds that resume to staying open.
func (r *podUpdateRig) awaitResumeReady(role claim.Role) {
	r.t.Helper()
	testwait.Eventually(r.t, fmt.Sprintf("the ready of the %s stamped on its open resume", role), func() bool {
		id, _, open, err := r.st.OpenPodResume(r.ctx, podUpdateProject, podUpdateIssue, role)
		if err != nil || !open {
			r.t.Fatalf("read the open resume of the %s = %t, %v; want the resume of its held wake open", role, open, err)
		}
		ready, err := r.st.PodResumeReadyAt(r.ctx, id)
		if err != nil {
			r.t.Fatalf("read the ready stamp of the %s's resume: %v", role, err)
		}
		return !ready.IsZero()
	})
}

// lookStart is the resume a production check look's start row makes of role, a claim the daemon
// suspended (outbox's start, supervise.RequestRuleResume), as INFRA-4455's look resumed its tester.
func (r *podUpdateRig) lookStart(role claim.Role) supervise.RequestRuleResume {
	return supervise.RequestRuleResume{Claim: r.tokens[role],
		Reason: fmt.Sprintf("start row 1 of %s (production check look 1)", podUpdateIssue)}
}

// intoRoundPod scripts role's next launch into the round's own pod, the pod the issue's Sandbox
// holds, and gives role a new connection for the process that launch starts. The runtime decides
// whether the launch keeps that pod: the Sandbox runtime's relaunch asks the daemon's hook
// (fake.Runtime.RoundPodEnds), and a launch into a pod that was replaced comes up in the pod in
// its place.
func (r *podUpdateRig) intoRoundPod(role claim.Role) {
	r.t.Helper()
	r.intoPod(role, podUpdatePod)
}

// intoPod scripts role's next launch into pod and gives role a new connection for the process that
// launch starts.
func (r *podUpdateRig) intoPod(role claim.Role, pod string) {
	r.t.Helper()
	token := r.tokens[role]
	conn := fake.NewConn()
	r.conns.Register(token, conn)
	r.workers[role] = conn
	r.rt.ScriptResume(fake.SpawnResult{Locator: *podUpdateLocator(token, podUpdateIssue, pod)})
}

// holdNextResume scripts role's next launch into pod (intoPod) and holds the next Resume the
// runtime is asked for inside the runtime (fake's HoldResume), as a launch into a pod being built
// holds its machine in that decision. reached is closed once that Resume is inside the hold, and
// release lets it go on. A test that stops before it releases is released at its cleanup, ahead
// of the supervisor's stop, which waits for the held decision.
func (r *podUpdateRig) holdNextResume(role claim.Role, pod string) (reached <-chan struct{}, release func()) {
	r.t.Helper()
	r.intoPod(role, pod)
	holding, held := make(chan struct{}), make(chan struct{})
	var once sync.Once
	release = func() { once.Do(func() { close(held) }) }
	r.t.Cleanup(release)
	r.rt.HoldResume(func() {
		r.rt.HoldResume(nil)
		close(holding)
		<-held
	})
	return holding, release
}

// launchesOf is every launch the runtime was asked to make of role's claim, Resume and Spawn alike,
// in order.
func (r *podUpdateRig) launchesOf(role claim.Role) []fake.Call {
	var launches []fake.Call
	for _, call := range r.rt.Calls() {
		if (call.Method == "Resume" || call.Method == "Spawn") && call.Spec.Claim == r.tokens[role] {
			launches = append(launches, call)
		}
	}
	return launches
}

// launchAtOnce resumes first and second by rule, each scripted into the round's own pod, with the
// two launches in flight together: first's launch is held inside the runtime (fake's HoldResume),
// so its machine is in that decision while second's launch is made whole, and then first's goes
// on. second takes the first launch scripted, since first waits before it takes its own.
func (r *podUpdateRig) launchAtOnce(first, second claim.Role) {
	r.t.Helper()
	r.intoRoundPod(second)
	holding, let := r.holdNextResume(first, podUpdatePod)
	r.sup.post(r.tokens[first], r.lookStart(first))
	select {
	case <-holding:
	case <-time.After(10 * time.Second):
		r.t.Fatalf("the %s's launch did not reach Resume", first)
	}
	if m, ok := r.sup.Machine(r.tokens[first]); !ok || m.Deciding() == nil {
		r.t.Fatalf("the %s's machine is not in its launch's decision while that launch is held", first)
	}
	r.handle(second, r.lookStart(second))
	let()
}

// A claim launched under a round with no process in its pod, which the round's ask never reached
// and where no member holds an ask it has not answered, is launched once, into a pod no round
// covers (dispatch://INFRA-4109 R7). On 2026-11-19 INFRA's daemon resumed INFRA-4455's tester for a
// production check look, the wake pacing ran its launch past the round's bound into the round's
// pod, and the round replaced that pod 18 s into the tester's first turn, so the tester was
// launched a second time. The round is read when the launch runs: where nothing holds the pod, the
// launch replaces it as the round's own replacement before it starts the role (the Sandbox
// runtime's RoundPodEnds hook, roundPodEndsAtLaunch), or the sweep has already replaced it by
// issue, and the launch builds a new pod.
//
// Each case is a row of the state table of the plan: resumed and launched before the bound;
// resumed and launched past it; resumed before the bound and launched by the wake pacing after it,
// with the sweep between them; the same, where a member's ask keeps the pod before the bound and
// no sweep runs between the bound and the launch, so the launch alone reads the bound; two
// launches at once, where the first replaces the pod and the second finds the new pod and keeps
// it; and two launches at once of members that had answered before the daemon suspended them
// (row 8b). In row 8b, one launch waits for the pod's launch turn in its decision while the other
// launch reads the round. The reading launch reads the waiting one unmasked, as a member that
// answered, so it replaces the pod, and the waiting launch finds the new pod. In each case, every
// launch is one launch into a pod other than the round's, no replacement after it ends its first
// turn, and every claim records no round at the end. The issue is written one message, naming the
// members that had not answered, or nothing where every member had answered.
func TestALaunchUnderARoundWithNoProcessIsLaunchedOnce(t *testing.T) {
	const launching = claim.RoleTester
	var (
		// releasedAfterItsAsk is a member the daemon released after the ask reached it: the round
		// passes over it, so its ask holds nothing.
		releasedAfterItsAsk = podlessMember(claim.RoleReviewer, supervise.StateSuspended, supervise.SuspendedByReleased, false)
		// heldAsk is a member that holds the round's ask: its process went while it held the ask,
		// and no launch has run since, so the boot restored it as launch_uncertain. Before the
		// bound it keeps the pod.
		heldAsk = podlessMember(claim.RoleImplementer, supervise.StateLaunchUncertain, "", false)
	)
	// launchTurn is a minute past the round's bound, and pastTheBound is the wake pacing of a wave
	// that gives the launch its turn then, as INFRA-4455's tester waited 1745 s for its turn.
	launchTurn := podUpdateBound.Add(time.Minute)
	pastTheBound := pacedUntil(launchTurn)
	for _, tc := range []struct {
		name    string
		members []podUpdateRole
		// resumeAt is when the claim is resumed.
		resumeAt time.Time
		// pace is the wake pacing, which parks the launch until a minute past the bound; the
		// zero pace launches where the resume is made.
		pace supervise.WakePace
		// alongside is a second launching claim, of the first's shape, resumed and launched at
		// once with the first.
		alongside claim.Role
		// launchedAnswered is whether each launching claim was asked and answered before the
		// daemon suspended it; otherwise each was released before the ask reached it
		// (releasedUnasked).
		launchedAnswered bool
		// byIssue is whether the sweep between the resume and the launch replaces the pod by
		// issue; otherwise the launch replaces it.
		byIssue bool
		// bounded is whether the bound let the replacement through over a member the round
		// waits for.
		bounded bool
		// lost is every member the end names as not having answered, and answered every member
		// it names nowhere.
		lost, answered []claim.Role
	}{
		{
			name:     "before the bound",
			members:  []podUpdateRole{answeredThenSuspended(claim.RoleImplementer), releasedAfterItsAsk},
			resumeAt: podUpdateStart,
			lost:     []claim.Role{claim.RoleReviewer, launching}, answered: []claim.Role{claim.RoleImplementer},
		},
		{
			name:     "past the bound",
			members:  []podUpdateRole{heldAsk},
			resumeAt: podUpdateBound.Add(time.Minute), bounded: true,
			lost: []claim.Role{claim.RoleImplementer, launching},
		},
		{
			name:     "resumed before the bound and launched by the wake pacing after it",
			members:  []podUpdateRole{answeredThenSuspended(claim.RoleImplementer)},
			resumeAt: podUpdateStart, pace: pastTheBound, byIssue: true,
			lost: []claim.Role{launching}, answered: []claim.Role{claim.RoleImplementer},
		},
		{
			name:     "the bound is read when the launch runs",
			members:  []podUpdateRole{heldAsk},
			resumeAt: podUpdateStart, pace: pastTheBound, bounded: true,
			lost: []claim.Role{claim.RoleImplementer, launching},
		},
		{
			name:     "two launches at once",
			members:  []podUpdateRole{answeredThenSuspended(claim.RoleImplementer)},
			resumeAt: podUpdateStart, alongside: claim.RoleReviewer,
			lost: []claim.Role{claim.RoleReviewer, launching}, answered: []claim.Role{claim.RoleImplementer},
		},
		{
			name:     "two answered launches at once",
			resumeAt: podUpdateStart, alongside: claim.RoleReviewer, launchedAnswered: true,
			answered: []claim.Role{launching, claim.RoleReviewer},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			launcher := releasedUnasked
			if tc.launchedAnswered {
				launcher = answeredThenSuspended
			}
			roles := append([]podUpdateRole{launcher(launching)}, tc.members...)
			launched := []claim.Role{launching}
			if tc.alongside != "" {
				roles = append(roles, launcher(tc.alongside))
				launched = append(launched, tc.alongside)
			}
			rig := newPodUpdateRigWith(t, podUpdateRigOptions{wakePace: tc.pace}, roles...)
			rig.rt.ScriptIssuePod(podUpdateIssue, podUpdatePod)
			rig.clockAt(tc.resumeAt)

			if tc.alongside != "" {
				rig.launchAtOnce(launching, tc.alongside)
			} else {
				rig.intoRoundPod(launching)
				rig.handle(launching, rig.lookStart(launching))
			}
			if tc.pace.Reserve != nil {
				rig.wantLaunchParked(launching, launchTurn)
				// The sweep before the bound, while the launch waits for its turn and no machine
				// is in a decision.
				if tc.byIssue {
					rig.awaitIssueReplacement("while the launch waits for its turn")
				} else {
					rig.tick()
					rig.settleAll()
					if asked := rig.byIssueAsks(); len(asked) != 0 {
						t.Fatalf("the runtime was asked to replace the pod by issue %+v before the bound while a member holds its ask, want nothing", asked)
					}
				}
				// The turn of the launch, a minute past the bound, with no sweep before it.
				rig.runParkedLaunch(launching, tc.resumeAt, launchTurn)
			}
			byIssue := len(rig.issueReplacements())

			var pods []string
			for _, role := range launched {
				c := rig.awaitClaim(role, "its launch to record its process", func(c supervise.Claim) bool { return c.Locator != nil })
				if pod := podOf(c.Locator); pod == "" || pod == podUpdatePod {
					t.Fatalf("the %s was launched into pod %q, want a pod other than the round's %s", role, pod, podUpdatePod)
				}
				pods = append(pods, podOf(c.Locator))
			}
			if len(pods) == 2 && pods[0] != pods[1] {
				t.Fatalf("the two launches came up in pods %v, want both in the one pod that replaced the round's", pods)
			}
			for _, role := range roles {
				rig.awaitSwept(role.role, "its round to end", func(c supervise.Claim) bool { return c.UpdateRound == "" })
			}
			rig.tick()
			rig.tick()
			rig.settleAll()

			by := "atLaunch"
			if tc.byIssue {
				by = "byIssue"
			}
			wantRoundPodEnd(t, rig, by, tc.bounded, tc.lost, tc.answered...)
			if lines := loggedLines(t, rig.logs.String(), "supervise: the pod of an open update round is gone"); len(lines) != 0 {
				t.Errorf("the sweep ended the round as a pod that went without it %+v, want the replacement the round made", lines)
			}
			atLaunch := rig.rt.CallsOf("ReplaceRoundPodAtLaunch")
			if want := map[bool]int{true: 0, false: 1}[tc.byIssue]; len(atLaunch) != want {
				t.Errorf("a launch replaced the round's pod %d times %+v, want %d", len(atLaunch), atLaunch, want)
			}
			if calls := rig.replacements(); len(calls) != 0 {
				t.Errorf("ReplacePod called %+v, want no replacement through a process", calls)
			}
			if after := len(rig.issueReplacements()); after != byIssue {
				t.Errorf("the pod was replaced by issue %d times after the launch, want none: it would end the launch's first turn", after-byIssue)
			}
			for i, role := range launched {
				if launches := rig.launchesOf(role); len(launches) != 1 {
					t.Errorf("the %s was launched %d times, want once", role, len(launches))
				}
				if c := rig.claimOf(role); podOf(c.Locator) != pods[i] {
					t.Errorf("the %s records pod %q after the round's end, want the pod %s it was launched into", role, podOf(c.Locator), pods[i])
				}
			}
			if notices := rig.notices(); len(notices) != 0 {
				t.Errorf("the controller was told %+v of a round the replacement ended", notices)
			}
		})
	}
}

// launchedIntoRoundPod holds role's one launch to the round's own pod, which the round keeps: the
// launch came up there, and no launch replaced the pod.
func (r *podUpdateRig) launchedIntoRoundPod(role claim.Role) {
	r.t.Helper()
	c := r.awaitClaim(role, "its launch to record its process", func(c supervise.Claim) bool { return c.Locator != nil })
	if pod := podOf(c.Locator); pod != podUpdatePod {
		r.t.Fatalf("the %s was launched into pod %q, want the round's own pod %s, which the round keeps", role, pod, podUpdatePod)
	}
	if launches := r.launchesOf(role); len(launches) != 1 {
		r.t.Fatalf("the %s was launched %d times, want once", role, len(launches))
	}
	if replaced := r.rt.CallsOf("ReplaceRoundPodAtLaunch"); len(replaced) != 0 {
		r.t.Fatalf("a launch replaced the round's pod %+v, which the round keeps", replaced)
	}
}

// wantPodKept sweeps twice and holds the round's pod to having been kept for what the round waits
// for: no replacement through a process, none by issue, and none by a launch.
func (r *podUpdateRig) wantPodKept(waitsFor string) {
	r.t.Helper()
	r.tick()
	r.tick()
	r.settleAll()
	if calls := r.replacements(); len(calls) != 0 {
		r.t.Fatalf("ReplacePod called %+v before %s", calls, waitsFor)
	}
	if asked := r.byIssueAsks(); len(asked) != 0 {
		r.t.Fatalf("the runtime was asked to replace the pod by issue %+v before %s", asked, waitsFor)
	}
	if replaced := r.rt.CallsOf("ReplaceRoundPodAtLaunch"); len(replaced) != 0 {
		r.t.Fatalf("a launch replaced the round's pod %+v before %s", replaced, waitsFor)
	}
}

// answersInATurn is role's worker answering the round from inside a turn, as it answers the ask
// (signal). The turn still runs when this returns.
func (r *podUpdateRig) answersInATurn(role claim.Role) {
	r.t.Helper()
	r.handle(role, supervise.StreamTurnStart{Claim: r.tokens[role]})
	r.signal(role, "a1b2c3d")
}

// launchedAgain is role's process ending with the pod the round's replacement took, and the
// daemon launching it again (runtime.Observation.Displaced), into the pod in its place: the
// second launch a claim launched into a kept pod costs. It holds the claim to two launches, the
// second in that pod, and registers nothing of the new process.
func (r *podUpdateRig) launchedAgain(role claim.Role) {
	r.t.Helper()
	token := r.tokens[role]
	ended := r.claimOf(role).Locator
	if ended == nil || podOf(ended) != podUpdatePod {
		r.t.Fatalf("the %s records %+v, want a process in the round's pod to end with it", role, ended)
	}
	conn := fake.NewConn()
	r.conns.Register(token, conn)
	r.workers[role] = conn
	r.rt.ScriptResume(fake.SpawnResult{Locator: *podUpdateLocator(token, podUpdateIssue, podUpdateReplacementPod)})
	r.handle(role, supervise.RuntimeObservation{Observation: runtime.Observation{
		Locator: *ended, Kind: runtime.Gone, At: r.clock.Now(), Displaced: true,
	}})
	launches := r.launchesOf(role)
	if len(launches) != 2 {
		r.t.Fatalf("the %s was launched %d times, want twice: once into the kept pod and once after its replacement", role, len(launches))
	}
	if c := r.claimOf(role); podOf(c.Locator) != podUpdateReplacementPod {
		r.t.Fatalf("the %s's second launch came up at %+v, want the pod %s in the replaced one's place", role, c.Locator, podUpdateReplacementPod)
	}
}

// The pod of a round with no process in it is kept, and a launch goes into it, where the round
// keeps it for a reason dispatch://INFRA-4109 R7 names. Before the bound, a member the round waits
// for holds the round's ask and has not answered it: its process went while it held the ask, so
// its unpushed work may be on the disk. Before the bound too, a project's pause keeps the pod for a
// member it stopped, after the pause's hold has ended, while the wake pacing parks that member's
// held launch until its let-out. At any time, an operator's suspension holds the round
// (dispatch://INFRA-2981 R5), and a project's pause holds the pod (dispatch://INFRA-3759's
// decision 12). This guards against a launch that always replaces the pod first, which would
// delete that work; at the base, where every launch keeps a live issue pod, it may pass.
//
// Each case is a row of the state table of the plan. The launching member holds the ask itself:
// it is launched into the round's pod and asked nothing a second time. Another member holds the
// ask: the launching claim is launched into the round's pod and asked once it is ready. The member
// that holds the ask got a fresh session, which never saw the ask and never answers it, and its
// mark stays, so it is asked nothing a second time and the round keeps the pod until its bound. An
// operator's suspension holds a member, before the bound and past it, and the launching claim is
// asked. A project's pause holds the pod past the bound. A project's pause keeps the pod before
// the bound for a member it stopped and the round never asked: its hold ended, at the resume or at
// a role's ready, and the wake pacing parks that member's held launch until its let-out, which is
// still ahead. The member's own launch at that let-out keeps the pod too. In each, nothing
// replaces the pod before what the round waits for: the answers and the turns, the operator's
// resume, the bound for the fresh session, or, for the pause, the end of its hold or the let-out.
// Each claim launched into the kept pod is then launched a second time, which is what keeping the
// pod costs.
func TestALaunchUnderARoundWithNoProcessKeepsThePodForAMemberThatHoldsItsAsk(t *testing.T) {
	t.Run("the launching member holds the ask", func(t *testing.T) {
		const holder = claim.RoleTester
		rig := newPodUpdateRig(t, podUpdateRole{role: holder, state: supervise.StateReady, session: true, inPod: true,
			contract: api.DaemonAPIVersion, round: podUpdateRound, reasons: podUpdateDrift})
		rig.rt.ScriptIssuePod(podUpdateIssue, podUpdatePod)

		// Its process goes while it holds the ask, and the daemon launches it again.
		ended := rig.claimOf(holder).Locator
		rig.intoRoundPod(holder)
		rig.handle(holder, supervise.RuntimeObservation{Observation: runtime.Observation{
			Locator: *ended, Kind: runtime.Gone, At: rig.clock.Now(), Detail: "the process ended",
		}})
		rig.launchedIntoRoundPod(holder)
		rig.registerInto(holder, podUpdatePod)

		rig.wantPodKept("its answer")
		rig.runLane()
		if asked := rig.askedOf(holder); asked != 0 {
			t.Fatalf("the %s was asked %d times more, want no second ask: its session holds the round's ask", holder, asked)
		}
		if c := rig.claimOf(holder); c.UpdateTaskedAt == nil || !c.UpdateTaskedAt.Equal(podUpdateStart) {
			t.Fatalf("the %s's ask mark is %v, want the ask that reached it at %s", holder, c.UpdateTaskedAt, podUpdateStart)
		}

		rig.answersInATurn(holder)
		rig.wantPodKept("its turn ends")
		rig.handle(holder, supervise.StreamTurnEnd{Claim: rig.tokens[holder]})
		rig.awaitReplacement(1, "after its answer and its turn")
		rig.launchedAgain(holder)
	})

	t.Run("another member holds the ask", func(t *testing.T) {
		const launching, holder = claim.RoleTester, claim.RoleImplementer
		rig := newPodUpdateRig(t, releasedUnasked(launching),
			podlessMember(holder, supervise.StateLaunchUncertain, "", false))
		rig.rt.ScriptIssuePod(podUpdateIssue, podUpdatePod)

		rig.intoRoundPod(launching)
		rig.handle(launching, rig.lookStart(launching))
		rig.launchedIntoRoundPod(launching)
		rig.registerInto(launching, podUpdatePod)
		rig.awaitAsked(launching, podUpdateRound)
		rig.wantPodKept("the answers")

		// The member that holds the ask is launched as the boot launches an unfinished launch
		// (launchUnfinishedClaim): its release, then the spawn, which resumes its session into the
		// pod a process of the issue runs in now.
		if m, ok := rig.sup.Machine(rig.tokens[holder]); !ok {
			t.Fatalf("the %s is not supervised", holder)
		} else if _, err := m.ReleaseUncertainLaunch(rig.ctx); err != nil {
			t.Fatalf("release the %s's unfinished launch: %v", holder, err)
		}
		rig.intoRoundPod(holder)
		rig.handle(holder, supervise.RequestSpawn{Claim: rig.tokens[holder]})
		rig.launchedIntoRoundPod(holder)
		rig.registerInto(holder, podUpdatePod)
		rig.answersInATurn(launching)
		rig.wantPodKept(fmt.Sprintf("the %s's answer", holder))
		rig.handle(launching, supervise.StreamTurnEnd{Claim: rig.tokens[launching]})
		rig.answersInATurn(holder)
		rig.wantPodKept(fmt.Sprintf("the %s's turn ends", holder))
		rig.handle(holder, supervise.StreamTurnEnd{Claim: rig.tokens[holder]})
		rig.awaitReplacement(1, "after both answers and their turns")
		rig.launchedAgain(launching)
		rig.launchedAgain(holder)
	})

	t.Run("a fresh session never answers", func(t *testing.T) {
		const holder, fresh = claim.RoleTester, "ses_tester_fresh"
		rig := newPodUpdateRig(t, podUpdateRole{role: holder, state: supervise.StateReady, session: true, inPod: true,
			contract: api.DaemonAPIVersion, round: podUpdateRound, reasons: podUpdateDrift})
		rig.rt.ScriptIssuePod(podUpdateIssue, podUpdatePod)
		token := rig.tokens[holder]
		recorded := rig.claimOf(holder).Session

		// The fresh session, as its route runs it: the operator's suspension it may not hold, the
		// forget, and the operator's resume, which spawns a new agent since the claim records no
		// session now.
		rig.handle(holder, supervise.RequestSuspend{Claim: token, Reason: "fresh session", By: supervise.SuspendedByOperator, NeverHold: true})
		rig.awaitClaim(holder, "the fresh session's suspension to let its process go",
			func(c supervise.Claim) bool { return c.State == supervise.StateSuspended && c.Locator == nil })
		rig.handle(holder, supervise.RequestForgetSession{Claim: token, Session: recorded})
		conn := fake.NewConn()
		rig.conns.Register(token, conn)
		rig.workers[holder] = conn
		rig.rt.ScriptSpawn(fake.SpawnResult{Locator: *podUpdateLocator(token, podUpdateIssue, podUpdatePod)})
		rig.handle(holder, supervise.RequestResume{Claim: token})
		rig.launchedIntoRoundPod(holder)
		generation := rig.claimOf(holder).Generation
		for _, ev := range []supervise.Event{
			supervise.StreamHello{Claim: token, Generation: generation},
			supervise.RequestRegister{Claim: token, Generation: generation, Session: fresh,
				SessionFile: "/sessions/tester-fresh.jsonl", CapabilityHash: []byte(holder), PluginContract: api.DaemonAPIVersion},
			supervise.RequestReady{Claim: token, Generation: generation, Session: fresh},
		} {
			rig.handle(holder, ev)
		}
		if c := rig.claimOf(holder); c.State != supervise.StateReady || c.Session != fresh || c.UpdateTaskedAt == nil {
			t.Fatalf("the %s after its fresh session is %s in session %q asked at %v, want it ready in its new session with its ask mark",
				holder, c.State, c.Session, c.UpdateTaskedAt)
		}

		rig.wantPodKept("the bound")
		rig.clockAt(podUpdateBound.Add(-time.Minute))
		rig.wantPodKept("the bound")
		rig.runLane()
		if asked := rig.askedOf(holder); asked != 0 {
			t.Fatalf("the %s was asked %d times more, want no second ask: its mark stays across the fresh session", holder, asked)
		}

		rig.clockAt(podUpdateBound)
		rig.awaitReplacement(1, "at the bound")
		if bound := loggedLines(t, rig.logs.String(), podUpdateBoundLine); len(bound) != 1 {
			t.Errorf("the replacement was logged at the bound %d times, want once: %+v", len(bound), bound)
		}
		rig.launchedAgain(holder)
	})

	t.Run("an operator's hold, before and past the bound", func(t *testing.T) {
		const launching, held = claim.RoleTester, claim.RoleImplementer
		for _, tc := range []struct {
			name string
			at   time.Time
		}{
			{name: "before the bound", at: podUpdateStart},
			{name: "past the bound", at: podUpdateBound.Add(time.Hour)},
		} {
			t.Run(tc.name, func(t *testing.T) {
				rig := newPodUpdateRig(t, releasedUnasked(launching),
					podlessMember(held, supervise.StateSuspended, supervise.SuspendedByOperator, false))
				rig.rt.ScriptIssuePod(podUpdateIssue, podUpdatePod)
				rig.clockAt(tc.at)

				rig.intoRoundPod(launching)
				rig.handle(launching, rig.lookStart(launching))
				rig.launchedIntoRoundPod(launching)
				rig.registerInto(launching, podUpdatePod)
				rig.awaitAsked(launching, podUpdateRound)
				rig.wantPodKept("the operator's resume")

				// The operator resumes the member, into the pod a process of the issue runs in.
				rig.intoRoundPod(held)
				rig.handle(held, supervise.RequestResume{Claim: rig.tokens[held]})
				if c := rig.awaitClaim(held, "its launch to record its process", func(c supervise.Claim) bool { return c.Locator != nil }); podOf(c.Locator) != podUpdatePod {
					t.Fatalf("the operator's resume launched the %s into pod %q, want the round's pod %s", held, podOf(c.Locator), podUpdatePod)
				}
				rig.registerInto(held, podUpdatePod)
				rig.answersInATurn(launching)
				if tc.at.Before(podUpdateBound) {
					rig.wantPodKept(fmt.Sprintf("the %s's answer", held))
				}
				rig.handle(launching, supervise.StreamTurnEnd{Claim: rig.tokens[launching]})
				rig.answersInATurn(held)
				if tc.at.Before(podUpdateBound) {
					rig.wantPodKept(fmt.Sprintf("the %s's turn ends", held))
				}
				rig.handle(held, supervise.StreamTurnEnd{Claim: rig.tokens[held]})
				rig.awaitReplacement(1, "after the operator's resume, the answers and the turns")
				rig.launchedAgain(launching)
				rig.launchedAgain(held)
			})
		}
	})

	t.Run("the project's pause holds the pod", func(t *testing.T) {
		const launching = claim.RoleTester
		rig := newPodUpdateRig(t, releasedUnasked(launching),
			answeredThenSuspended(claim.RoleImplementer))
		rig.rt.ScriptIssuePod(podUpdateIssue, podUpdatePod)
		heldAt := podUpdateBound.Add(30 * time.Minute)
		holdLegionPod(t, rig.st, openLegionPause(t, rig.pool, heldAt), podUpdateIssue, heldAt)
		rig.clockAt(podUpdateBound.Add(time.Hour))

		rig.intoRoundPod(launching)
		rig.handle(launching, rig.lookStart(launching))
		rig.launchedIntoRoundPod(launching)
		rig.registerInto(launching, podUpdatePod)
		rig.wantPodKept("the pause's hold ends")
	})

	// The members of the sweep's case in TestAPodlessRoundIsReplacedByIssueOnceNothingHoldsIt: the
	// implementer, which a project's pause stopped and the round never asked, and the tester, which
	// answered and which the daemon then suspended (pauseKeepsThePod). The pause's hold of the pod
	// has ended, the implementer's held start has run, and the wake pacing parks its launch until
	// its let-out, which the daemon has not written yet (parkPauseHeldLaunch). An operator then
	// resumes the tester, whose launch the pacing does not hold, so it goes into the pod before
	// that let-out. The implementer is launching with no process, and its machine is in no
	// decision. But the pause still owes the implementer a launch and keeps the pod for its work,
	// so the tester's launch keeps the pod too, and the hook's reason names the implementer.
	t.Run("the project's pause keeps the pod for a member it stopped", func(t *testing.T) {
		const launching = claim.RoleTester
		pauseKeepsThePod(t, func(t *testing.T, rig *podUpdateRig) {
			rig.parkPauseHeldLaunch(pauseStoppedRole, pauseLetOut)
			rig.intoRoundPod(launching)
			rig.handle(launching, supervise.RequestResume{Claim: rig.tokens[launching]})
			rig.launchedIntoRoundPod(launching)
			rig.wantLaunchKeptForOwed(launching, pauseStoppedRole)
			rig.registerInto(launching, podUpdatePod)
			rig.wantPodKept(heldLaunchLetOut(pauseStoppedRole))
		})
	})

	// The members of the case above, and the implementer's own launch. Its held start has run, and
	// the wake pacing parks its launch until its let-out (parkPauseHeldLaunch). The sweeps while it
	// waits keep the pod for it. At the let-out the wake pacing runs the launch, and the round never
	// asked the implementer. The implementer is in the group the launch reads, launching with no
	// process, and the pause still owes its role a launch, so the pause still keeps it
	// (podHolds.owes, decision 12 of dispatch://INFRA-3759): what holds the pod of a round with no
	// process in it holds the pod for its own launch (roundPodEnds), and the hook's reason names the
	// launch the pause owes. So the launch goes into the round's own pod and replaces nothing, and
	// the round then waits for the implementer's answer.
	t.Run("the launch a project's pause held keeps the pod the pause kept", func(t *testing.T) {
		pauseKeepsThePod(t, func(t *testing.T, rig *podUpdateRig) {
			rig.parkedLaunchKeepsThePod(pauseStoppedRole, pauseLetOut)

			rig.intoRoundPod(pauseStoppedRole)
			rig.runParkedLaunch(pauseStoppedRole, podUpdateStart, pauseLetOut)
			rig.launchedIntoRoundPod(pauseStoppedRole)
			rig.wantLaunchKeptForOwed(pauseStoppedRole, pauseStoppedRole)
			rig.registerInto(pauseStoppedRole, podUpdatePod)
			rig.wantPodKept(fmt.Sprintf("the %s's answer", pauseStoppedRole))
		})
	})
}

package daemon

// A round with no role process in its pod (dispatch://INFRA-4109 R7). On INFRA's daemon on
// 2026-11-19, INFRA-4455's round stood over a pod no agent ran in from 18:08:41Z. The sweep logged
// every minute that the round waited for a claim to run again, and held it past its bound. The
// wake pacing then launched the tester into that pod at 18:37:48Z, and the round, past its bound,
// replaced the pod 18 s into the tester's first turn, which cost a second paced launch.
//
// So a round whose pod the issue's Sandbox still holds, with no role process in it, ends by
// replacing that pod by issue (runtime.IssuePodReplacer) once nothing holds it: no operator's
// suspension of a member, no project pause's hold on the pod, and, before the round's bound, no
// member the round waits for that holds an ask of the round it has not answered (roundPodEnds).
// No process can read the disk, so the issue is told which members had not answered and that no
// process could read the workspace. These cases drive the pod-update rig of podupdate_test.go,
// with the claims seeded in the round and restored as a boot restores them.

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
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
	return podUpdateRole{role: role, state: state, session: true, contract: api.DaemonAPIVersion, suspendedBy: by,
		round: podUpdateRound, reasons: podUpdateDrift, signalled: answered}
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

// wantPodlessEnd holds a round the sweep replaced by issue to what such an end owes: the line of
// a replacement the round made, naming the replacement by issue, whether the bound made it, each
// member in lost as not waited for and why no read of the workspace was made, and one message on
// the issue that names each member in lost with its state, names no member that answered, and
// says that no process could read the workspace. A round whose members all answered writes no
// message.
func wantPodlessEnd(t *testing.T, rig *podUpdateRig, bounded bool, lost []claim.Role, answered ...claim.Role) {
	t.Helper()
	lines := loggedLines(t, rig.logs.String(), podUpdateReplacedLine)
	if len(lines) != 1 {
		t.Fatalf("the replacement was logged %d times, want once: %+v", len(lines), lines)
	}
	line := lines[0]
	if line["byIssue"] != true || line["round"] != podUpdateRound || line["pod"] != podUpdatePod || line["atTheBound"] != bounded {
		t.Errorf("the replacement's line is %+v, want round %s of pod %s replaced by issue, atTheBound=%t", line, podUpdateRound, podUpdatePod, bounded)
	}
	if line["workspaceRead"] != "not made" || line["workspaceNotReadBecause"] != noProcessRead {
		t.Errorf("the replacement's line names the read %+v, want one not made because %q", line, noProcessRead)
	}
	wantField(t, line, "notWaitedFor", lost...)
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
// not answered, since its process went while it held the ask; and a project pause's hold on the
// pod, past the bound too.
func TestAPodlessRoundIsReplacedByIssueOnceNothingHoldsIt(t *testing.T) {
	var (
		// answeredBeforeItsSuspension is a member that answered, which the daemon then
		// suspended: the ordinary end of a phase.
		answeredBeforeItsSuspension = podlessMember(claim.RoleTester, supervise.StateSuspended, supervise.SuspendedByDaemon, true)
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
			roles: []podUpdateRole{answeredBeforeItsSuspension,
				podlessMember(claim.RoleReviewer, supervise.StateSuspended, supervise.SuspendedByReleased, false)},
			at: podUpdateStart, replaced: true,
			lost: []claim.Role{claim.RoleReviewer}, answered: []claim.Role{claim.RoleTester},
		},
		{
			name:  "past the bound",
			roles: []podUpdateRole{heldAsk, answeredBeforeItsSuspension},
			at:    podUpdateBound, replaced: true, bounded: true,
			lost: []claim.Role{claim.RoleImplementer}, answered: []claim.Role{claim.RoleTester},
		},
		{
			name:  "not before the bound while a member the round waits for holds an unanswered ask",
			roles: []podUpdateRole{heldAsk, answeredBeforeItsSuspension},
			at:    podUpdateBound.Add(-time.Minute),
		},
		{
			name:  "not while an operator holds a member, before the bound",
			roles: []podUpdateRole{operatorHeld, answeredBeforeItsSuspension},
			at:    podUpdateStart,
		},
		{
			name:  "not while an operator holds a member, past the bound",
			roles: []podUpdateRole{operatorHeld, answeredBeforeItsSuspension},
			at:    podUpdateBound.Add(time.Hour), told: true,
		},
		{
			name: "a member suspended after its ask holds nothing",
			roles: []podUpdateRole{podlessMember(claim.RoleImplementer, supervise.StateSuspended, supervise.SuspendedByDaemon, false),
				answeredBeforeItsSuspension},
			at: podUpdateStart, replaced: true,
			lost: []claim.Role{claim.RoleImplementer}, answered: []claim.Role{claim.RoleTester},
		},
		{
			name: "not while the project's pause holds the pod",
			roles: []podUpdateRole{podlessMember(claim.RoleImplementer, supervise.StateSuspended, supervise.SuspendedByReleased, false),
				answeredBeforeItsSuspension},
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
}

// The sweep's replacement by issue keeps the pod while a claim of the issue is in a decision
// (issuePodKept). A launch into the pod lets the pod's launch turn go before it writes its
// locator, so for that moment no claim records a process in the pod although one is starting
// there, and a replacement made then would cut that process's first turn. The claim the daemon
// suspended before the round's ask reached it is resumed, and its launch is held inside the
// runtime (fake's HoldResume): the sweep then replaces nothing, and says why. Once the launch has
// written its locator in the round's own pod, the round is an ordinary round over a process, and
// still nothing replaces the pod by issue.
func TestTheByIssueReplacementWaitsForAMachineInADecision(t *testing.T) {
	rig := newPodUpdateRig(t, podUpdateRole{role: claim.RoleImplementer, state: supervise.StateSuspended, session: true,
		contract: api.DaemonAPIVersion, suspendedBy: supervise.SuspendedByDaemon, round: podUpdateRound, reasons: podUpdateDrift, unasked: true})
	rig.drifts(podUpdateDrift)
	rig.rt.ScriptIssuePod(podUpdateIssue, podUpdatePod)

	token := rig.tokens[claim.RoleImplementer]
	conn := fake.NewConn()
	rig.conns.Register(token, conn)
	rig.workers[claim.RoleImplementer] = conn
	holding, held := make(chan struct{}), make(chan struct{})
	var once sync.Once
	let := func() { once.Do(func() { close(held) }) }
	t.Cleanup(let)
	rig.rt.ScriptResume(fake.SpawnResult{Locator: *podUpdateLocator(token, podUpdateIssue, podUpdatePod)})
	rig.rt.HoldResume(func() {
		rig.rt.HoldResume(nil)
		close(holding)
		<-held
	})
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
	rig.registerInto(claim.RoleImplementer, podUpdatePod)
	rig.tick()
	rig.tick()
	if asked := rig.byIssueAsks(); len(asked) != 0 {
		t.Fatalf("the runtime was asked to replace the pod by issue %+v once a process ran in it, want nothing", asked)
	}
	if c := rig.claimOf(claim.RoleImplementer); c.UpdateRound != podUpdateRound {
		t.Fatalf("the implementer is in round %q, want the round open over the process in its pod", c.UpdateRound)
	}
}

package daemon

// R7 of #5285 (spec v20, verification v15's c-round-keeps-unread-disk). An update
// round replaces a pod only over a clean read of its workspace with R5's count, made through a
// process in the pod: a live role's shim, or the pod's launcher where no role process runs. A turn
// of the issue that starts after the read voids it, and the replacement's last word refuses.
//
// Over a drift the pod's roles can keep working under — every drift until a later change's
// kinds exist — a read that finds work origin does not hold, or that cannot be made, replaces
// nothing: the round ends with no replacement, each claim that answered gets the task the round
// held, the daemon records the pod, the work and the moment as #4869 records a release's decline,
// and opens no new round over that pod while the record stands. After each turn of the issue ends
// it reads the workspace again, without a round, and a read that finds nothing ends the record.
// The open phase's worker, where it is live, is told once; otherwise the issue's architect is.
//
// The negative control is a production issue's round pod:d4a8d543-2094-4a47-9068-5e3f0642e2fb, which
// replaced the pod at 2026-11-23T17:02:55Z with the tester failed and commits after e8bc366e
// unpushed; on main the round's read decides nothing (replacePodForRound).

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

const (
	// roundChangeHead is the working-copy commit of a read that finds a change in the working copy.
	roundChangeHead = "5b7d9f1a3c5e7a9b1d3f5a7c9e1b3d5f7a9c1e3b"
	// roundHeldTask is the task the round holds back from a claim that answered it.
	roundHeldTask = "outbox:41"
	// roundSweeps is how many sweeps a test runs to show that something does not happen.
	roundSweeps = 3
)

var (
	// roundUnpushed is a read of a workspace that holds one commit origin does not hold.
	roundUnpushed = runtime.WorkspaceWork{Head: passedOverHead, Commits: []string{passedOverCommit}, Count: 1}
	// roundClean is a read of a workspace that holds nothing origin does not hold.
	roundClean = runtime.WorkspaceWork{Head: passedOverHead}
	// roundChanged is a read of a working copy that holds a change.
	roundChanged = runtime.WorkspaceWork{Head: roundChangeHead, Changed: true}
	// errRoundRead is a read of the workspace that cannot be made.
	errRoundRead = errors.New("no process of the pod reads a workspace")
)

// workspaceReads is how many reads of the issue's workspace the runtime was asked for, through a
// role's shim (WorkspaceWork) or through the pod's launcher (PodWorkspaceWork).
func (r *podUpdateRig) workspaceReads() int {
	return len(r.rt.CallsOf("WorkspaceWork")) + len(r.rt.CallsOf("PodWorkspaceWork"))
}

// architectNotices is every notice of kind to the issue's architect that the outbox holds.
func (r *podUpdateRig) architectNotices(kind record.NoticeKind) []record.Notice {
	r.t.Helper()
	var notices []record.Notice
	for _, row := range r.rows(record.OutboxKindNotice) {
		var notice record.Notice
		if err := json.Unmarshal(row.Payload, &notice); err != nil {
			r.t.Fatalf("decode notice %d: %v", row.ID, err)
		}
		if notice.Kind == kind && notice.Role == claim.RoleArchitect {
			notices = append(notices, notice)
		}
	}
	return notices
}

// roleMessagesTo is the body of every role message to role's claim that the outbox holds.
func (r *podUpdateRig) roleMessagesTo(role claim.Role) []string {
	r.t.Helper()
	var bodies []string
	for _, row := range r.rows(record.OutboxKindRoleMessage) {
		var message record.RoleMessage
		if err := json.Unmarshal(row.Payload, &message); err != nil {
			r.t.Fatalf("decode role message %d: %v", row.ID, err)
		}
		if message.Role == string(r.tokens[role]) {
			bodies = append(bodies, message.Body)
		}
	}
	return bodies
}

// keptReason is the reason of the issue's decline row ("" where none stands), and the pod it names.
func (r *podUpdateRig) keptReason() (string, string) {
	r.t.Helper()
	var reason, pod string
	err := r.pool.QueryRow(r.ctx, "select reason, pod_uid from idle_release_declines where issue = $1", podUpdateIssue).Scan(&reason, &pod)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		r.t.Fatalf("read the decline row of %s: %v", podUpdateIssue, err)
	}
	return reason, pod
}

// answerRound opens the round over the drift, has the sweep ask each of answering, and has each
// answer (answerRoundOver).
func (r *podUpdateRig) answerRound(drift string, answering ...claim.Role) {
	r.t.Helper()
	r.answerRoundOver([]string{drift}, answering...)
}

// roundEndsKept sweeps until every role of members records no round, and holds the end to one
// with no replacement through a process or by issue.
func (r *podUpdateRig) roundEndsKept(what string, members ...claim.Role) {
	r.t.Helper()
	for _, role := range members {
		r.awaitSwept(role, "the round to end "+what, func(c supervise.Claim) bool { return c.UpdateRound == "" })
	}
	if calls := r.replacements(); len(calls) != 0 {
		r.t.Fatalf("the pod was replaced through a process %+v %s, want no replacement", calls, what)
	}
	if asked := r.byIssueAsks(); len(asked) != 0 {
		r.t.Fatalf("the pod was replaced by issue %+v %s, want no replacement", asked, what)
	}
}

// noRoundOver sweeps roundSweeps times and holds the rig to opening no round over the kept pod and
// asking for no replacement.
func (r *podUpdateRig) noRoundOver(what string, roles ...claim.Role) {
	r.t.Helper()
	asks := len(r.rows(record.OutboxKindUpdateAsk))
	for range roundSweeps {
		r.tick()
	}
	r.settleAll()
	for _, role := range roles {
		if c := r.claimOf(role); c.UpdateRound != "" {
			r.t.Fatalf("the %s records round %q %s, want no round over the kept pod", role, c.UpdateRound, what)
		}
	}
	if after := len(r.rows(record.OutboxKindUpdateAsk)); after != asks {
		r.t.Fatalf("the sweeps queued %d update asks %s, want none", after-asks, what)
	}
	if calls := r.replacements(); len(calls) != 0 {
		r.t.Fatalf("the pod was replaced %+v %s, want no replacement", calls, what)
	}
}

// turn is one turn of role, a minute after the rig's clock stands: its start, and its end. The
// read of the workspace a test scripts in during runs at the end, before the turn's end.
func (r *podUpdateRig) turn(role claim.Role, during func()) {
	r.t.Helper()
	r.clock.advance(time.Minute)
	r.envoyTurn(role)
	if during != nil {
		during()
	}
	r.handle(role, supervise.StreamTurnEnd{Claim: r.tokens[role]})
}

// An update round over a pod whose workspace holds one commit that origin does not hold, over a
// drift its roles can keep working under, replaces nothing. Each case is one of the round's
// replacements, and each control is the same round over a workspace that holds nothing origin
// lacks, which replaces the pod at its sweep.
func TestAnUpdateRoundKeepsAPodWhoseDiskHoldsUnpushedWork(t *testing.T) {
	t.Parallel()

	// (1) The replacement through a process, with one claim of the issue failed, the open phase's
	// worker (the implementer, the rig's issue being in implementing) a claim whose launch the wake
	// pacing delayed with no process, and every live role answered. The round ends with no
	// replacement, the architect's held task goes out and starts a turn, the architect is told
	// once, and no new round opens while the commit stands. The workspace is read once after that
	// turn has ended, and again after the next; once the commit is pushed that read finds nothing,
	// and the next round replaces the pod.
	t.Run("through a process", func(t *testing.T) {
		t.Parallel()
		delayed := podUpdateRole{role: claim.RoleImplementer, state: supervise.StateSuspended, contract: api.DaemonAPIVersion(),
			suspendedBy: supervise.SuspendedByReleased}
		failed := podUpdateRole{role: claim.RoleTester, state: supervise.StateFailed, session: true, contract: api.DaemonAPIVersion()}
		rig := newPodUpdateRigWith(t, podUpdateRigOptions{wakePace: pacedUntil(podUpdateBound.Add(time.Hour))},
			readyWorker(claim.RoleArchitect), readyWorker(claim.RoleReviewer), failed, delayed)
		rig.heldStart(claim.RoleImplementer)
		rig.wantLaunchDelayed(claim.RoleImplementer, podUpdateBound.Add(time.Hour))
		rig.rt.ScriptWorkspaceWork(podUpdateIssue, roundUnpushed)
		live := []claim.Role{claim.RoleArchitect, claim.RoleReviewer}

		rig.answerRound(podUpdateDrift, live...)
		rig.handle(claim.RoleArchitect, supervise.RequestDeliver{Claim: rig.tokens[claim.RoleArchitect], ID: roundHeldTask,
			Task: "Carry the design on.", Generation: 1})
		rig.settle(claim.RoleArchitect)
		if sent := promptsOf(rig.workers[claim.RoleArchitect], roundHeldTask); len(sent) != 0 {
			t.Fatalf("the architect was sent %+v while it stood answered in the round, want the task held", sent)
		}

		rig.roundEndsKept("over a workspace that holds an unpushed commit", live...)
		if reads := rig.workspaceReads(); reads != 1 {
			t.Fatalf("the round read the workspace %d times, want once before its replacement", reads)
		}
		rig.settle(claim.RoleArchitect)
		if sent := promptsOf(rig.workers[claim.RoleArchitect], roundHeldTask); len(sent) == 0 {
			t.Fatalf("the architect was sent %+v once the round ended, want the task the round held", rig.workers[claim.RoleArchitect].Prompts())
		}
		if reason, pod := rig.keptReason(); reason != pods.DeclineUnpushedWork || pod != podUpdatePod {
			t.Fatalf("the decline row reads %q of pod %q, want unpushed_work of the round's pod %s", reason, pod, podUpdatePod)
		}
		told := rig.architectNotices(record.WorkspaceKeptNotice)
		if len(told) != 1 || !strings.Contains(told[0].Reason, passedOverCommit) || !strings.Contains(told[0].Reason, string(supervise.StateFailed)) {
			t.Fatalf("the architect was told %+v, want one workspace-kept notice naming the commit %s and the failed tester", told, passedOverCommit)
		}
		if messages := rig.roleMessagesTo(claim.RoleImplementer); len(messages) != 0 {
			t.Fatalf("the implementer was told %q, want nothing: it runs no process in the pod", messages)
		}

		// The held task's turn: no round opens while it runs, and no read is made in it.
		rig.clock.advance(time.Minute)
		rig.envoyTurn(claim.RoleArchitect)
		rig.noRoundOver("while the held task's turn runs", live...)
		if reads := rig.workspaceReads(); reads != 1 {
			t.Fatalf("the workspace was read %d times while the turn ran, want no read since the round's", reads-1)
		}
		rig.endTurn(claim.RoleArchitect, roundHeldTask)
		rig.noRoundOver("after the held task's turn, with the commit still unpushed", live...)
		if reads := rig.workspaceReads(); reads != 2 {
			t.Fatalf("the workspace was read %d times after the turn ended, want once", reads-1)
		}
		if told := rig.architectNotices(record.WorkspaceKeptNotice); len(told) != 1 {
			t.Fatalf("the architect was told %d times, want once: the read after the turn found the same work", len(told))
		}

		// The next turn pushes the commit; the read after it finds nothing, and the next round
		// replaces the pod.
		rig.turn(claim.RoleArchitect, func() { rig.rt.ScriptWorkspaceWork(podUpdateIssue, roundClean) })
		rig.answerRound(podUpdateDrift, live...)
		rig.awaitReplacement(1, "once the commit was pushed and the next round answered")
		if reason, _ := rig.keptReason(); reason != "" {
			t.Fatalf("the decline row still reads %q after a read that found nothing, want it gone", reason)
		}
	})

	// (2) A round past its bound, held there by the implementer's turn, whose working copy holds a
	// change: nothing is replaced, the turn runs on, and the implementer, the open phase's worker,
	// is told once.
	t.Run("past the bound over a turn", func(t *testing.T) {
		t.Parallel()
		rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester))
		rig.rt.ScriptWorkspaceWork(podUpdateIssue, roundChanged)
		rig.drifts(podUpdateDrift)
		rig.tick()
		rig.awaitAsked(claim.RoleImplementer, podUpdateRound)
		rig.awaitAsked(claim.RoleTester, podUpdateRound)
		rig.signal(claim.RoleTester, "a1b2c3d")
		rig.envoyTurn(claim.RoleImplementer)
		rig.clockAt(podUpdateBound)

		rig.roundEndsKept("at the bound over a turn whose working copy holds a change", claim.RoleImplementer, claim.RoleTester)
		if c := rig.claimOf(claim.RoleImplementer); c.State != supervise.StateWorking || podOf(c.Locator) != podUpdatePod {
			t.Fatalf("the implementer is %s in pod %q, want its turn running on in pod %s", c.State, podOf(c.Locator), podUpdatePod)
		}
		told := rig.roleMessagesTo(claim.RoleImplementer)
		if len(told) != 1 || !strings.Contains(told[0], roundChangeHead) {
			t.Fatalf("the implementer was told %q, want one message naming the change on %s", told, roundChangeHead)
		}
		rig.noRoundOver("while the turn runs on", claim.RoleImplementer, claim.RoleTester)
		if told := rig.roleMessagesTo(claim.RoleImplementer); len(told) != 1 {
			t.Fatalf("the implementer was told %d times, want once", len(told))
		}
		if notices := rig.architectNotices(record.WorkspaceKeptNotice); len(notices) != 0 {
			t.Fatalf("the architect was told %+v, want nothing: the open phase's worker is live", notices)
		}
	})

	// (3) The replacement by issue, no role process in the pod: the pod's launcher reads the
	// workspace, and nothing is replaced.
	t.Run("by issue", func(t *testing.T) {
		t.Parallel()
		rig := newPodUpdateRig(t, answeredThenSuspended(claim.RoleTester),
			podlessMember(claim.RoleReviewer, supervise.StateSuspended, supervise.SuspendedByReleased, false))
		rig.rt.ScriptIssuePod(podUpdateIssue, podUpdatePod)
		rig.rt.ScriptWorkspaceWork(podUpdateIssue, roundUnpushed)

		rig.roundEndsKept("by issue over a workspace that holds an unpushed commit", claim.RoleTester, claim.RoleReviewer)
		if reads := rig.rt.CallsOf("PodWorkspaceWork"); len(reads) != 1 || reads[0].Issue != podUpdateIssue {
			t.Fatalf("the pod's launcher was asked to read %+v, want the issue's workspace once", reads)
		}
		told := rig.architectNotices(record.WorkspaceKeptNotice)
		if len(told) != 1 || !strings.Contains(told[0].Reason, passedOverCommit) {
			t.Fatalf("the architect was told %+v, want one workspace-kept notice naming the commit %s", told, passedOverCommit)
		}
		for range roundSweeps {
			rig.tick()
		}
		if asked := rig.byIssueAsks(); len(asked) != 0 {
			t.Fatalf("the pod was replaced by issue %+v after the round ended, want it kept", asked)
		}
	})

	// (4) The launch for a round that is over: the pod's launcher reads the workspace, and the
	// launch keeps the pod and starts its role in it.
	t.Run("at the launch", func(t *testing.T) {
		t.Parallel()
		const launching = claim.RoleTester
		rig := newPodUpdateRig(t, releasedUnasked(launching), answeredThenSuspended(claim.RoleImplementer),
			podlessMember(claim.RoleReviewer, supervise.StateSuspended, supervise.SuspendedByReleased, false))
		rig.rt.ScriptIssuePod(podUpdateIssue, podUpdatePod)
		rig.rt.ScriptWorkspaceWork(podUpdateIssue, roundUnpushed)

		rig.intoRoundPod(launching)
		rig.handle(launching, rig.lookStart(launching))
		c := rig.awaitClaim(launching, "its launch to record its process", func(c supervise.Claim) bool { return c.Locator != nil })
		if pod := podOf(c.Locator); pod != podUpdatePod {
			t.Fatalf("the %s was launched into pod %q, want the round's pod %s, which its launch keeps", launching, pod, podUpdatePod)
		}
		if replaced := rig.rt.CallsOf("ReplaceRoundPodAtLaunch"); len(replaced) != 0 {
			t.Fatalf("the launch replaced the round's pod %+v, want it kept", replaced)
		}
		if launches := rig.launchesOf(launching); len(launches) != 1 {
			t.Fatalf("the %s was launched %d times, want once", launching, len(launches))
		}
		if reads := rig.rt.CallsOf("PodWorkspaceWork"); len(reads) != 1 {
			t.Fatalf("the pod's launcher was asked to read %d times, want once before the launch decided", len(reads))
		}
		rig.roundEndsKept("at the launch over a workspace that holds an unpushed commit", launching, claim.RoleImplementer, claim.RoleReviewer)
		told := rig.architectNotices(record.WorkspaceKeptNotice)
		if len(told) != 1 || !strings.Contains(told[0].Reason, passedOverCommit) {
			t.Fatalf("the architect was told %+v, want one workspace-kept notice naming the commit %s", told, passedOverCommit)
		}
	})

	// (5) A read that cannot be made: nothing is replaced. Once the read can be made and finds
	// nothing, after a turn of the issue, the next round replaces the pod.
	t.Run("a read that cannot be made", func(t *testing.T) {
		t.Parallel()
		rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester))
		rig.rt.FailWorkspaceWork(podUpdateIssue, errRoundRead)
		rig.answerRound(podUpdateDrift, claim.RoleImplementer, claim.RoleTester)

		rig.roundEndsKept("over a read that cannot be made", claim.RoleImplementer, claim.RoleTester)
		if reason, pod := rig.keptReason(); reason != pods.DeclineWorkspaceUnread || pod != podUpdatePod {
			t.Fatalf("the decline row reads %q of pod %q, want workspace_unread of the round's pod %s", reason, pod, podUpdatePod)
		}
		rig.noRoundOver("while the read cannot be made", claim.RoleImplementer, claim.RoleTester)

		rig.rt.FailWorkspaceWork(podUpdateIssue, nil)
		rig.rt.ScriptWorkspaceWork(podUpdateIssue, roundClean)
		rig.turn(claim.RoleTester, nil)
		rig.answerRound(podUpdateDrift, claim.RoleImplementer, claim.RoleTester)
		rig.awaitReplacement(1, "once the read could be made and found nothing")
	})

	// (6) Past the bound, a read that finds nothing, and then a turn of the issue that starts
	// before the replacement's last word: the pod is not replaced at that sweep.
	t.Run("a turn after the read", func(t *testing.T) {
		t.Parallel()
		rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester))
		rig.rt.ScriptWorkspaceWork(podUpdateIssue, roundClean)
		rig.drifts(podUpdateDrift)
		rig.tick()
		rig.awaitAsked(claim.RoleImplementer, podUpdateRound)
		rig.awaitAsked(claim.RoleTester, podUpdateRound)
		rig.signal(claim.RoleTester, "a1b2c3d")
		rig.settleAll()
		rig.clockAt(podUpdateBound)
		var turned bool
		rig.rt.HoldReplacePod(func() {
			if turned {
				return
			}
			turned = true
			rig.clock.advance(time.Second)
			rig.envoyTurn(claim.RoleTester)
		})

		rig.tick()
		if !turned {
			t.Fatal("the sweep past the bound asked for no replacement, so this case proves nothing")
		}
		if calls := rig.replacements(); len(calls) != 0 {
			t.Fatalf("the pod was replaced %+v over a turn that started after the read, want the last word to refuse", calls)
		}
		if refused := rig.notDue(); len(refused) != 1 {
			t.Fatalf("the last word refused %d replacements, want the one", len(refused))
		}
	})

	// (7) Case (2) on a kept volume that the issue's next pod mounts. R7 makes no exception for
	// such a disk: a round over a drift the pod's roles can keep working under replaces nothing
	// there either, a turn that runs at the bound included, although a replacement would leave the
	// files on the volume, since it stops that turn and its subagents. As in case (2) the read finds
	// the change in the working copy, so the kept volume is the one difference: nothing is replaced,
	// the turn runs on, and the implementer, the open phase's worker, is told once.
	t.Run("a kept volume past the bound over a turn", func(t *testing.T) {
		t.Parallel()
		rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester))
		rig.rt.ScriptKeptVolume(podUpdateIssue, podUpdateStart.Add(-time.Hour))
		rig.rt.ScriptWorkspaceWork(podUpdateIssue, roundChanged)
		rig.drifts(podUpdateDrift)
		rig.tick()
		rig.awaitAsked(claim.RoleImplementer, podUpdateRound)
		rig.awaitAsked(claim.RoleTester, podUpdateRound)
		rig.signal(claim.RoleTester, "a1b2c3d")
		rig.envoyTurn(claim.RoleImplementer)
		rig.clockAt(podUpdateBound)

		rig.roundEndsKept("at the bound over a turn whose working copy holds a change, on a kept volume", claim.RoleImplementer, claim.RoleTester)
		if c := rig.claimOf(claim.RoleImplementer); c.State != supervise.StateWorking || podOf(c.Locator) != podUpdatePod {
			t.Fatalf("the implementer is %s in pod %q, want its turn running on in pod %s", c.State, podOf(c.Locator), podUpdatePod)
		}
		if reason, pod := rig.keptReason(); reason != pods.DeclineUnpushedWork || pod != podUpdatePod {
			t.Fatalf("the decline row reads %q of pod %q, want unpushed_work of the round's pod %s", reason, pod, podUpdatePod)
		}
		told := rig.roleMessagesTo(claim.RoleImplementer)
		if len(told) != 1 || !strings.Contains(told[0], roundChangeHead) {
			t.Fatalf("the implementer was told %q, want one message naming the change on %s", told, roundChangeHead)
		}
		rig.noRoundOver("while the turn runs on over the kept volume", claim.RoleImplementer, claim.RoleTester)
		if taken := rig.architectNotices(record.WorkspaceTakenNotice); len(taken) != 0 {
			t.Fatalf("the architect was told %+v, want no workspace-taken notice: nothing was replaced", taken)
		}
	})

	// The controls: each round over a workspace that holds nothing origin lacks replaces the pod at
	// its sweep.
	t.Run("control through a process", func(t *testing.T) {
		t.Parallel()
		rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester))
		rig.rt.ScriptWorkspaceWork(podUpdateIssue, roundClean)
		rig.answerRound(podUpdateDrift, claim.RoleImplementer, claim.RoleTester)
		rig.awaitReplacement(1, "over a clean workspace")
	})
	t.Run("control by issue", func(t *testing.T) {
		t.Parallel()
		rig := newPodUpdateRig(t, answeredThenSuspended(claim.RoleTester),
			podlessMember(claim.RoleReviewer, supervise.StateSuspended, supervise.SuspendedByReleased, false))
		rig.rt.ScriptIssuePod(podUpdateIssue, podUpdatePod)
		rig.rt.ScriptWorkspaceWork(podUpdateIssue, roundClean)
		rig.awaitIssueReplacement("over a clean workspace")
	})
	t.Run("control at the launch", func(t *testing.T) {
		t.Parallel()
		const launching = claim.RoleTester
		rig := newPodUpdateRig(t, releasedUnasked(launching), answeredThenSuspended(claim.RoleImplementer))
		rig.rt.ScriptIssuePod(podUpdateIssue, podUpdatePod)
		rig.rt.ScriptWorkspaceWork(podUpdateIssue, roundClean)
		rig.intoRoundPod(launching)
		rig.handle(launching, rig.lookStart(launching))
		c := rig.awaitClaim(launching, "its launch to record its process", func(c supervise.Claim) bool { return c.Locator != nil })
		if replaced := rig.rt.CallsOf("ReplaceRoundPodAtLaunch"); len(replaced) != 1 || podOf(c.Locator) == podUpdatePod {
			t.Fatalf("the launch replaced the round's pod %d times and came up in %q, want one replacement and a new pod", len(replaced), podOf(c.Locator))
		}
	})
}

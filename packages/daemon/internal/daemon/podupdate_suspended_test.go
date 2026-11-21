package daemon

// A claim the daemon suspended holds no pod update round (dispatch://INFRA-4109 R1, R3,
// verification's c-suspended-asked). Such a claim, suspended by the daemon's own rule, by a
// root's exit, by an idle release, by the work hold or by an unknown source (supervise's
// Claim.PassedOverByRound), runs no process and no turn in the pod. So the round does not ask
// it, wait for it, wake it or resume it. What it left unpushed goes with the pod, unless the
// pod's disk is a kept volume that the issue's next pod mounts (roundKeptVolume). Once every
// claim the round asks has answered and no turn runs, the replacement reads the issue's
// workspace through a role whose process runs in the pod, names on its line each claim it did
// not wait for and what the read found, and, where the read found work or failed, writes one
// message on the issue. An operator's suspension still holds the round past its bound
// (dispatch://INFRA-2981 R5).
//
// These cases drive the pod-update rig of podupdate_test.go. The suspended claims are seeded in
// the store and restored as a boot restores them (newPodUpdateRig's sup.restore).

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

const (
	// passedOverHead is the working-copy commit every read these tests script answers.
	passedOverHead = "3f9a0c4e1b7d52a8c6e0f1d2b3a4c5d6e7f80912"
	// passedOverCommit is the one commit that no branch or tag on origin holds, where a read
	// finds one.
	passedOverCommit = "8c2e4a6b0d1f3e5a7c9b1d3f5a7c9e1b3d5f7a9c"
)

// errPassedOverRead is a read of the workspace that fails, as the Sandbox runtime answers a read
// the role's shim refused.
var errPassedOverRead = errors.New("the shim refused workspace-work: jj log: the working copy is stale")

var (
	// passedOverWorkers are the two ready workers of R3's round, each with a process in the pod.
	passedOverWorkers = []claim.Role{claim.RoleImplementer, claim.RoleTester}
	// passedOverSuspended are its two claims the daemon suspended: the planner by the daemon's
	// own rule and the reviewer by an idle release (replacedOverPassedOverClaims).
	passedOverSuspended = []claim.Role{claim.RolePlanner, claim.RoleReviewer}
)

// suspendedRole is a claim with a session that by suspended, as the store holds it and a boot
// restores it: the session is kept, and no process runs.
func suspendedRole(role claim.Role, by supervise.SuspendSource) podUpdateRole {
	return podUpdateRole{role: role, state: supervise.StateSuspended, session: true, contract: api.DaemonAPIVersion, suspendedBy: by}
}

// replacedOverPassedOverClaims carries R3's round to its replacement: two ready workers and two
// claims the daemon suspended (passedOverSuspended). script gives the issue's workspace what the
// read finds there. The round opens, both workers are asked and answer, and the sweep replaces
// the pod. What every such round shows whatever the read found is held here, and the
// replacement's line is returned:
//
//   - the pod is replaced once, at the held clock the round opened at, long before its bound;
//   - the workspace is read once, through the worker whose process the pod is replaced through,
//     and the replacement made comes straight after that read, with no refusal between: the read
//     holds nothing back;
//   - neither suspended claim is asked (no update-ask row, no publish to its topic), woken or
//     resumed (no Resume call), and each is still suspended once the round is over;
//   - no bundle of the workspace is saved.
func replacedOverPassedOverClaims(t *testing.T, script func(*podUpdateRig)) (*podUpdateRig, map[string]any) {
	t.Helper()
	rig := newPodUpdateRig(t,
		readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester),
		suspendedRole(claim.RolePlanner, supervise.SuspendedByDaemon),
		suspendedRole(claim.RoleReviewer, supervise.SuspendedByReleased),
	)
	script(rig)
	clocks := rig.replacementClocks()
	rig.drifts(podUpdateDrift)

	rig.tick()
	for _, role := range passedOverWorkers {
		rig.awaitAsked(role, podUpdateRound)
	}
	for _, role := range passedOverSuspended {
		rig.awaitRound(role, podUpdateRound)
	}
	if calls := rig.replacements(); len(calls) != 0 {
		t.Fatalf("the pod was replaced %d times before either worker answered", len(calls))
	}
	for _, role := range passedOverWorkers {
		rig.signal(role, "a1b2c3d")
	}
	// Nothing is deciding when the sweep reads the claims, so the first sweep after the answers
	// is the one that replaces the pod, and the read cannot be followed by a refusal that a
	// machine mid-decision would cause (roundGroup's mask).
	rig.settleAll()
	replaced := rig.awaitReplacement(1, "once both workers answered")

	at := clocks()
	if len(at) == 0 {
		t.Fatal("the runtime was never asked to replace the pod")
	}
	for _, when := range at {
		if !when.Equal(podUpdateStart) {
			t.Fatalf("the replacement was asked at %s, want %s, when the round opened: the bound %s holds nothing", when, podUpdateStart, podUpdateBound)
		}
	}
	var order []string
	for _, call := range rig.rt.Calls() {
		switch call.Method {
		case "WorkspaceWork", "ReplacePod", "ReplacePodNotDue", "ReplacePodNoPod":
			order = append(order, call.Method)
		}
	}
	if !slices.Equal(order, []string{"WorkspaceWork", "ReplacePod"}) {
		t.Fatalf("the runtime was asked %v, want one read of the workspace and then the replacement it stood in front of", order)
	}
	if replaced[0].Pod.Locator == nil || podOf(replaced[0].Pod.Locator) != podUpdatePod {
		t.Fatalf("the pod was replaced through %+v, want a process in pod %s", replaced[0].Pod, podUpdatePod)
	}
	through, _ := replaced[0].Pod.Claim.Role()
	reads := rig.rt.CallsOf("WorkspaceWork")
	if reads[0].Issue != podUpdateIssue || reads[0].Role != through || !slices.Contains(passedOverWorkers, through) {
		t.Fatalf("the workspace was read as %+v, want the issue %s read through the %s, whose process the pod is replaced through",
			reads[0], podUpdateIssue, through)
	}

	for _, role := range append(slices.Clone(passedOverWorkers), passedOverSuspended...) {
		rig.awaitClaim(role, "its round to end with the replacement", func(c supervise.Claim) bool { return c.UpdateRound == "" })
	}
	for _, role := range passedOverSuspended {
		if n := rig.askedOf(role); n != 0 {
			t.Errorf("the round wrote %d update-ask rows for the %s, want none: the daemon suspended it", n, role)
		}
		if n := rig.deliveredTo(role); n != 0 {
			t.Errorf("the %s's topic was published %d asks, want none: a publish to a role no session holds wakes it", role, n)
		}
		if c := rig.claimOf(role); c.State != supervise.StateSuspended {
			t.Errorf("the %s is %s after the round, want it still suspended", role, c.State)
		}
	}
	if resumes := rig.rt.CallsOf("Resume"); len(resumes) != 0 {
		t.Errorf("the round resumed %d processes, want none", len(resumes))
	}
	var bundles int
	if err := rig.pool.QueryRow(rig.ctx, "select count(*) from workspace_bundles").Scan(&bundles); err != nil {
		t.Fatalf("read the workspace bundles: %v", err)
	}
	if bundles != 0 {
		t.Errorf("the round saved %d workspace bundles, want none: the read keeps nothing", bundles)
	}

	lines := loggedLines(t, rig.logs.String(), podUpdateReplacedLine)
	if len(lines) != 1 {
		t.Fatalf("the replacement was logged %d times, want once: %+v", len(lines), lines)
	}
	line := lines[0]
	if line["atTheBound"] != false {
		t.Errorf("the replacement's line says atTheBound=%v, want false: both workers answered long before the bound", line["atTheBound"])
	}
	wantField(t, line, "notWaitedFor", passedOverSuspended...)
	if line["workspaceReadBy"] != string(through) {
		t.Errorf("the replacement's line names the read made by %v, want the %s", line["workspaceReadBy"], through)
	}
	return rig, line
}

// R3's first round: a claim suspended by the daemon's own rule and one an idle release
// suspended, beside two ready workers, over a workspace whose read finds no work. The pod is
// replaced once both workers answered and no turn ran, long before the bound, and neither
// suspended claim is asked, woken or resumed (replacedOverPassedOverClaims). The replacement's
// line names both suspended roles as the claims it did not wait for, and a read that found no
// work. The issue is written nothing: nothing the read found went with the pod.
//
// The negative control is main at fd1f52f1, BET-1182's pod on 2026-11-16: the round waited for
// every claim with a session, asked only a ready one, and held the pod to its bound.
func TestAnUpdateRoundDoesNotWaitForAClaimTheDaemonSuspended(t *testing.T) {
	rig, line := replacedOverPassedOverClaims(t, func(rig *podUpdateRig) {
		rig.rt.ScriptWorkspaceWork(podUpdateIssue, runtime.WorkspaceWork{Head: passedOverHead})
	})

	if line["workspaceRead"] != "found no work" || line["workspaceHead"] != passedOverHead || line["workspaceCommitCount"] != float64(0) {
		t.Errorf("the replacement's line names the read %+v, want one that found no work at the working copy %s", line, passedOverHead)
	}
	if messages := rig.messages(); len(messages) != 0 {
		t.Fatalf("the issue was written %q, want nothing: the read found no work", messages)
	}
}

// The work hold's stop holds no pod update round either (dispatch://INFRA-4109 R1). A claim the
// hold stopped (supervise.SuspendedByHold) runs no process and no turn in the pod, and only the
// hold's end or an operator starts it (supervise's Machine.EndHold). So the round passes it over
// as it passes over a claim the daemon suspended (supervise's Claim.PassedOverByRound): it does
// not ask it, wait for it, wake it or resume it. Once the claims the round asks have answered and
// no turn runs, the pod is replaced long before the bound. The replacement first reads the
// issue's workspace through a worker whose process runs in the pod, and its line names the held
// claim as the one it did not wait for. Once the round is over, the claim is still the hold's.
//
// The control is a worker whose work hold stop still waits for the turn that its close message
// starts (supervise.RequestSuspend.TurnAfter). Its agent runs in the pod and can answer, so the
// round asks it and does not replace the pod before it answers.
func TestWorkHoldAnUpdateRoundPassesOverAClaimTheHoldStopped(t *testing.T) {
	const held, stopping = claim.RolePlanner, claim.RoleTester
	workers := []claim.Role{claim.RoleImplementer, stopping}
	rig := newPodUpdateRig(t,
		readyWorker(claim.RoleImplementer), readyWorker(stopping),
		suspendedRole(held, supervise.SuspendedByHold),
	)
	rig.rt.ScriptWorkspaceWork(podUpdateIssue, runtime.WorkspaceWork{Head: passedOverHead})
	if c := rig.claimOf(held); c.State != supervise.StateSuspended || !c.WorkHeld() {
		t.Fatalf("the restored %s is %s, suspended by %q; want it held by the work hold", held, c.State, c.SuspendedBy)
	}
	// The tester's stop as the outbox posts it: the close message reached the tester now, and the
	// stop waits for the turn that message starts, up to its deadline 2 minutes later.
	messageAt := rig.clock.Now()
	m, _ := rig.sup.Machine(rig.tokens[stopping])
	stop := supervise.RequestSuspend{Claim: rig.tokens[stopping], By: supervise.SuspendedByHold,
		Reason: "the design gate of LEGION-208 closed", TurnAfter: messageAt, Deadline: messageAt.Add(2 * time.Minute)}
	if err := m.Handle(rig.ctx, stop); !errors.Is(err, supervise.ErrSuspendHeld) {
		t.Fatalf("the work hold's stop of the ready %s answered %v, want it held for the turn its message starts", stopping, err)
	}
	if c := rig.claimOf(stopping); c.State != supervise.StateReady || !c.WorkHeld() || c.PassedOverByRound() {
		t.Fatalf("the %s is %s, WorkHeld %v, PassedOverByRound %v; want it ready, its hold stop held for its turn and not passed over",
			stopping, c.State, c.WorkHeld(), c.PassedOverByRound())
	}
	clocks := rig.replacementClocks()
	rig.drifts(podUpdateDrift)

	rig.tick()
	for _, role := range workers {
		rig.awaitAsked(role, podUpdateRound)
	}
	rig.awaitRound(held, podUpdateRound)
	rig.signal(claim.RoleImplementer, "a1b2c3d")
	rig.sweepSettled()
	if calls := rig.replacements(); len(calls) != 0 {
		t.Fatalf("the pod was replaced %d times before the %s, whose hold stop waits for its turn, answered", len(calls), stopping)
	}
	rig.signal(stopping, "a1b2c3d")
	rig.settleAll()
	replaced := rig.awaitReplacement(1, "once both workers answered")

	for _, when := range clocks() {
		if !when.Equal(podUpdateStart) {
			t.Fatalf("the replacement was asked at %s, want %s, when the round opened: the bound %s holds nothing", when, podUpdateStart, podUpdateBound)
		}
	}
	var order []string
	for _, call := range rig.rt.Calls() {
		switch call.Method {
		case "WorkspaceWork", "ReplacePod", "ReplacePodNotDue", "ReplacePodNoPod":
			order = append(order, call.Method)
		}
	}
	if !slices.Equal(order, []string{"WorkspaceWork", "ReplacePod"}) {
		t.Fatalf("the runtime was asked %v, want one read of the workspace and then the replacement it stood in front of", order)
	}
	through, _ := replaced[0].Pod.Claim.Role()
	if reads := rig.rt.CallsOf("WorkspaceWork"); reads[0].Role != through || !slices.Contains(workers, through) {
		t.Fatalf("the workspace was read as %+v, want it read through the %s, whose process the pod is replaced through", reads[0], through)
	}

	c := rig.awaitClaim(held, "its round to end with the replacement", func(c supervise.Claim) bool { return c.UpdateRound == "" })
	if c.State != supervise.StateSuspended || c.SuspendedBy != supervise.SuspendedByHold {
		t.Errorf("the %s is %s, suspended by %q, after the round; want it still held by the work hold", held, c.State, c.SuspendedBy)
	}
	if n := rig.askedOf(held); n != 0 {
		t.Errorf("the round wrote %d update-ask rows for the %s, want none: the work hold stopped it", n, held)
	}
	if n := rig.deliveredTo(held); n != 0 {
		t.Errorf("the %s's topic was published %d asks, want none: a publish to a role no session holds wakes it", held, n)
	}
	if resumes := rig.rt.CallsOf("Resume"); len(resumes) != 0 {
		t.Errorf("the round resumed %d processes, want none", len(resumes))
	}

	lines := loggedLines(t, rig.logs.String(), podUpdateReplacedLine)
	if len(lines) != 1 {
		t.Fatalf("the replacement was logged %d times, want once: %+v", len(lines), lines)
	}
	line := lines[0]
	if line["atTheBound"] != false {
		t.Errorf("the replacement's line says atTheBound=%v, want false: both workers answered long before the bound", line["atTheBound"])
	}
	wantField(t, line, "notWaitedFor", held)
	if line["workspaceReadBy"] != string(through) || line["workspaceRead"] != "found no work" {
		t.Errorf("the replacement's line names the read %+v, want one by the %s that found no work", line, through)
	}
	if messages := rig.messages(); len(messages) != 0 {
		t.Fatalf("the issue was written %q, want nothing: the read found no work", messages)
	}
}

// R3's second and third rounds: the same claims over a read that finds an unpushed commit, and
// over a read that fails. The replacement is made at the same point as over a read that finds no
// work (replacedOverPassedOverClaims), so what the read found decides nothing. Its line names both
// suspended roles and the commit or the error, and one message on the issue names both roles,
// each with its state, and the commit or the error. The sweeps after the round's end write no
// second message.
//
// Where the work is, as the line and the message say it, is roundKeptVolume's answer (#4550). On
// a kept volume made before the round opened, which the issue's next pod mounts, the message says
// the work is still on that volume rather than lost, and the line says the volume is kept: nothing
// went with the pod. Over any other disk — an ephemeral one, or a kept volume made after the round
// opened — the line names no kept volume, and the message says the pod and its disk are gone and
// the work is lost, whatever the read found. A message that calls lost work safe tells a role not
// to do that work again.
func TestAnUpdateRoundNamesTheWorkItsSuspendedClaimsLeft(t *testing.T) {
	// commitRead scripts a read that finds one unpushed commit, and foundCommit holds the
	// replacement's line to that read, whatever the disk.
	commitRead := func(rig *podUpdateRig) {
		rig.rt.ScriptWorkspaceWork(podUpdateIssue, runtime.WorkspaceWork{Head: passedOverHead, Commits: []string{passedOverCommit}, Count: 1})
	}
	foundCommit := func(t *testing.T, line map[string]any) {
		commits, _ := line["workspaceCommits"].([]any)
		if line["workspaceRead"] != "found work" || len(commits) != 1 || commits[0] != passedOverCommit || line["workspaceCommitCount"] != float64(1) {
			t.Errorf("the replacement's line names the read %+v, want one that found the commit %s", line, passedOverCommit)
		}
	}
	// madeBefore and madeAfter are when the issue's Sandbox made its kept volume: before the
	// round opened, and after it.
	madeBefore, madeAfter := podUpdateStart.Add(-time.Hour), podUpdateStart.Add(time.Minute)
	for _, tc := range []struct {
		name   string
		script func(*podUpdateRig)
		// line holds the replacement's line to what the read found.
		line func(t *testing.T, line map[string]any)
		// says is what the message must carry of the read.
		says string
		// keptVolumeAt is when the issue's Sandbox made the kept volume that is the pod's disk, as
		// roundKeptVolume reads it. nil is an ephemeral disk, which goes with the pod. Only a kept
		// volume made before the round opened is one that the issue's next pod mounts.
		keptVolumeAt *time.Time
	}{
		{name: "an unpushed commit", script: commitRead, line: foundCommit, says: passedOverCommit},
		{name: "an unpushed commit on a kept volume", script: commitRead, line: foundCommit, says: passedOverCommit, keptVolumeAt: &madeBefore},
		// A Sandbox made after the round opened holds a new volume, which holds nothing of the
		// round's pod.
		{name: "an unpushed commit on a kept volume made after the round opened", script: commitRead, line: foundCommit, says: passedOverCommit, keptVolumeAt: &madeAfter},
		{
			name:   "a read that fails",
			script: func(rig *podUpdateRig) { rig.rt.FailWorkspaceWork(podUpdateIssue, errPassedOverRead) },
			line: func(t *testing.T, line map[string]any) {
				if line["workspaceRead"] != "failed" || line["workspaceReadError"] != errPassedOverRead.Error() {
					t.Errorf("the replacement's line names the read %+v, want one that failed with %q", line, errPassedOverRead)
				}
			},
			says: errPassedOverRead.Error(),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig, line := replacedOverPassedOverClaims(t, func(rig *podUpdateRig) {
				tc.script(rig)
				if tc.keptVolumeAt != nil {
					rig.rt.ScriptKeptVolume(podUpdateIssue, *tc.keptVolumeAt)
				}
			})
			tc.line(t, line)

			messages := rig.messages()
			if len(messages) != 1 {
				t.Fatalf("the issue was written %d messages, want one: %q", len(messages), messages)
			}
			for _, want := range []string{"planner (suspended)", "reviewer (suspended)", tc.says} {
				if !strings.Contains(messages[0], want) {
					t.Errorf("the message does not name %q:\n%s", want, messages[0])
				}
			}
			// kept is whether the disk is a kept volume that the issue's next pod mounts
			// (roundKeptVolume): one made before the round opened.
			kept := tc.keptVolumeAt != nil && tc.keptVolumeAt.Before(podUpdateStart)
			if kept {
				if line["keptVolume"] != true {
					t.Errorf("the replacement's line says keptVolume=%v, want true: the disk is a kept volume that the issue's next pod mounts", line["keptVolume"])
				}
				for _, want := range []string{"Its disk is a kept volume, and the volume is still there.", "Work that is not on a branch at origin is on that volume."} {
					if !strings.Contains(messages[0], want) {
						t.Errorf("the message does not say %q:\n%s", want, messages[0])
					}
				}
				if strings.Contains(messages[0], "is lost") {
					t.Errorf("the message calls work lost on a kept volume that the issue's next pod mounts:\n%s", messages[0])
				}
			} else {
				if value, named := line["keptVolume"]; named {
					t.Errorf("the replacement's line says keptVolume=%v, want no kept volume: the disk went with the pod", value)
				}
				for _, want := range []string{"The pod and its disk are gone.", "Work that is not on a branch at origin is lost."} {
					if !strings.Contains(messages[0], want) {
						t.Errorf("the message does not say %q:\n%s", want, messages[0])
					}
				}
				if strings.Contains(messages[0], "kept volume") {
					t.Errorf("the message names a kept volume over a disk that went with the pod:\n%s", messages[0])
				}
			}

			rig.tick()
			rig.tick()
			if messages := rig.messages(); len(messages) != 1 {
				t.Fatalf("the issue was written %d messages after two more sweeps, want the one: %q", len(messages), messages)
			}
		})
	}
}

// R3's last round: a claim an operator suspended, restored as a boot restores it, beside a worker
// that answered. A person's hold is not the daemon's, so the round still asks for it and waits
// for it whatever its bound (dispatch://INFRA-2981 R5): the pod is not replaced before the bound
// or an hour past it, and the operator's side is told once, at the bound, naming the suspension.
// Once the operator resumes the claim and it answers, the pod is replaced, and nobody is told
// again.
func TestAnUpdateRoundStillWaitsPastItsBoundForAnOperatorSuspension(t *testing.T) {
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), suspendedRole(claim.RoleTester, supervise.SuspendedByOperator))
	rig.drifts(podUpdateDrift)

	rig.tick()
	rig.awaitAsked(claim.RoleImplementer, podUpdateRound)
	rig.awaitRound(claim.RoleTester, podUpdateRound)
	rig.signal(claim.RoleImplementer, "a1b2c3d")

	for _, at := range []time.Time{podUpdateStart, podUpdateBound.Add(-time.Minute), podUpdateBound, podUpdateBound.Add(time.Hour)} {
		rig.clockAt(at)
		rig.tick()
		rig.tick()
		if calls := rig.replacements(); len(calls) != 0 {
			t.Fatalf("at %s the pod was replaced %d times under the operator's suspension of the tester (bound %s)", at, len(calls), podUpdateBound)
		}
		if notices := rig.notices(); at.Before(podUpdateBound) && len(notices) != 0 {
			t.Fatalf("at %s, before the bound, the controller was told %+v", at, notices)
		}
	}
	notices := rig.notices()
	if len(notices) != 1 || !strings.Contains(notices[0].Reason, "An operator suspended") {
		t.Fatalf("the controller was told %+v, want one notice naming the operator's suspension", notices)
	}
	wantWaiting(t, notices[0], map[claim.Role]bool{claim.RoleImplementer: true, claim.RoleTester: false})

	rig.resumeInPod(claim.RoleTester)
	rig.signal(claim.RoleTester, "e4f5a6b")
	rig.awaitReplacement(1, "once the operator resumed the tester and it answered")
	if notices := rig.notices(); len(notices) != 1 {
		t.Fatalf("the controller was told %d times, want the one notice: %+v", len(notices), notices)
	}
}

// The read of the workspace is made once in a round, and only where it names something: where
// the round passed over a claim that has not answered, through the role whose process the pod is
// replaced through, and once every claim the round asks has answered and no turn runs.
func TestAnUpdateRoundReadsTheWorkspaceOncePerRound(t *testing.T) {
	// No claim is passed over: two workers that answer, and a failed claim the round does not
	// wait for either. The line names the failed claim, and nothing is read: no role the round
	// passed over left work that no role was asked to push.
	t.Run("no claim passed over", func(t *testing.T) {
		rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester),
			podUpdateRole{role: claim.RolePlanner, state: supervise.StateFailed, session: true, contract: api.DaemonAPIVersion})
		rig.drifts(podUpdateDrift)

		rig.tick()
		for _, role := range passedOverWorkers {
			rig.awaitAsked(role, podUpdateRound)
			rig.signal(role, "a1b2c3d")
		}
		rig.awaitReplacement(1, "once both workers answered")

		if reads := rig.rt.CallsOf("WorkspaceWork"); len(reads) != 0 {
			t.Fatalf("the workspace was read %+v, want no read: the round passed over no claim", reads)
		}
		lines := loggedLines(t, rig.logs.String(), podUpdateReplacedLine)
		if len(lines) != 1 || lines[0]["workspaceRead"] != "not made" {
			t.Fatalf("the replacement was logged %+v, want one line that says no read was made", lines)
		}
		wantField(t, lines[0], "notWaitedFor", claim.RolePlanner)
	})

	// A claim the daemon suspended, the architect, sorts first in the group and has no process.
	// The one worker, the implementer, runs in the pod. Nothing is read while the implementer has
	// not answered. Once it answers, the last word refuses the replacement three times, each time
	// over a turn that starts while the replacement waits for the pod's launch turn, and then the
	// runtime fails the replacement once. The read is made once, through the implementer, and each
	// later sweep names that read again. The issue is written nothing until a replacement is made,
	// and then once.
	t.Run("once over the sweeps that replace nothing", func(t *testing.T) {
		rig := newPodUpdateRig(t, suspendedRole(claim.RoleArchitect, supervise.SuspendedByDaemon), readyWorker(claim.RoleImplementer))
		rig.rt.ScriptWorkspaceWork(podUpdateIssue, runtime.WorkspaceWork{Head: passedOverHead, Commits: []string{passedOverCommit}, Count: 1})
		rig.drifts(podUpdateDrift)

		rig.tick()
		rig.awaitAsked(claim.RoleImplementer, podUpdateRound)
		rig.awaitRound(claim.RoleArchitect, podUpdateRound)
		for range 3 {
			rig.tick()
		}
		if reads := rig.rt.CallsOf("WorkspaceWork"); len(reads) != 0 {
			t.Fatalf("the workspace was read %+v while the implementer had not answered, want no read", reads)
		}

		token := rig.tokens[claim.RoleImplementer]
		rig.signal(claim.RoleImplementer, "a1b2c3d")
		for refused := 1; refused <= 3; refused++ {
			rig.rt.HoldReplacePod(func() {
				rig.rt.HoldReplacePod(nil)
				rig.handle(claim.RoleImplementer, supervise.StreamTurnStart{Claim: token})
			})
			testwait.Eventually(t, fmt.Sprintf("the last word to refuse the replacement %d times", refused), func() bool {
				if len(rig.notDue()) < refused {
					rig.tick()
				}
				return len(rig.notDue()) >= refused
			})
			rig.handle(claim.RoleImplementer, supervise.StreamTurnEnd{Claim: token})
		}
		rig.rt.FailReplacePod(errors.New("the Sandbox API refused the suspension"))
		rig.awaitReplacement(1, "that the runtime fails")
		if messages := rig.messages(); len(messages) != 0 {
			t.Fatalf("the issue was written %q before any replacement was made, want nothing", messages)
		}

		rig.rt.FailReplacePod(nil)
		rig.awaitReplacement(2, "once the runtime replaces it")
		reads := rig.rt.CallsOf("WorkspaceWork")
		if len(reads) != 1 || reads[0].Role != claim.RoleImplementer {
			t.Fatalf("the workspace was read %+v, want one read through the implementer, the one role whose process is in the pod", reads)
		}
		messages := rig.messages()
		if len(messages) != 1 || !strings.Contains(messages[0], passedOverCommit) || !strings.Contains(messages[0], "architect (suspended)") {
			t.Fatalf("the issue was written %q, want one message naming the architect and the commit %s", messages, passedOverCommit)
		}
		lines := loggedLines(t, rig.logs.String(), podUpdateReplacedLine)
		if len(lines) != 1 || lines[0]["workspaceRead"] != "found work" {
			t.Fatalf("the replacement was logged %+v, want one line naming the read that found work", lines)
		}
		wantField(t, lines[0], "notWaitedFor", claim.RoleArchitect)
	})

	// The round's bound lets the replacement through over a worker that never answered. The
	// round passed over a claim, but a claim it asks has not answered, so the replacement makes
	// no read, and the issue is written nothing.
	t.Run("not at the bound over a claim that has not answered", func(t *testing.T) {
		rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester),
			suspendedRole(claim.RolePlanner, supervise.SuspendedByDaemon))
		rig.drifts(podUpdateDrift)

		rig.tick()
		for _, role := range passedOverWorkers {
			rig.awaitAsked(role, podUpdateRound)
		}
		rig.awaitRound(claim.RolePlanner, podUpdateRound)
		rig.signal(claim.RoleImplementer, "a1b2c3d")
		rig.tick()
		rig.tick()
		if calls := rig.replacements(); len(calls) != 0 {
			t.Fatalf("the pod was replaced %d times before the bound with the tester unanswered", len(calls))
		}

		rig.clockAt(podUpdateBound)
		rig.awaitReplacement(1, "at the bound, with the tester unanswered")
		if reads := rig.rt.CallsOf("WorkspaceWork"); len(reads) != 0 {
			t.Fatalf("the workspace was read %+v, want no read: the tester had not answered", reads)
		}
		lines := loggedLines(t, rig.logs.String(), podUpdateReplacedLine)
		if len(lines) != 1 || lines[0]["atTheBound"] != true || lines[0]["workspaceRead"] != "not made" {
			t.Fatalf("the replacement was logged %+v, want one line of a bounded end that says no read was made", lines)
		}
		wantField(t, lines[0], "notWaitedFor", claim.RolePlanner)
		if messages := rig.messages(); len(messages) != 0 {
			t.Fatalf("the issue was written %q, want nothing: no read was made", messages)
		}
	})
}

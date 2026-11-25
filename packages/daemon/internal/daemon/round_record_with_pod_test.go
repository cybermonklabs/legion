package daemon

// The Legion reviewer's Important on #5285 at 69e00593: a launch whose act deletes the issue's
// Sandbox and the kept volume it owns over a live pod of that Sandbox (runtime.TakenPod.WithPod)
// takes that pod with the volume, so a round's record of the pod is a record of the same act. The
// round's finish of the pod's act takes the launch's record with it, or yields to it where the
// round's own call took nothing, and the sweep decides the launch's record before a round's record
// of the pod, whatever their ages: one act is told once, by the record of the act that took the
// pod, and the notice says nothing false of the kept volume. The cases are two of the reviewer's
// paths:
//
//   - a: the launch records its act and suspends the Sandbox over the round's pod, then fails in
//     its wait for the pod to go, so it never tells the daemon what it took. The round's
//     replacement by issue, which waited for the pod's launch turn, finds the Sandbox Suspended
//     and asks no last word. The round's call yields to the launch's record: in one transaction
//     the round's read moves onto the launch's record where it is the read that tells, and the round's
//     record goes, so one record stands for the one act (the Legion reviewer's Medium 1 on #5285
//     at 18d81dad). The round ends with no notice of its own; once the Sandbox is gone, the
//     launch's finish tells the architect once, from the read that tells most of the launch's and
//     the round's (the Legion reviewer's Medium 1 on #5285 at 4b123279).
//   - c: the daemon stops between the launch's delete of the Sandbox and its word of what it took.
//     The round's record is newer than the launch's, since the round wrote it before its call
//     waited for the launch turn. After the restart the sweep decides the launch's record first,
//     which tells the architect once and takes the round's record, and the round ends with no
//     notice of its own.
//
// The table test holds the sweep to what it decides of a record whose target a record it decided
// first names: gone with that record's finish, standing with that record, the walk's, and the
// record of a round whose record a launch's finish took.
//
// TestARoundsCallThatYieldedIsToldFromItsReadByTheLaunchsNotice is the Legion reviewer's Medium 1
// on #5285 at 4b123279: path a, where the round's read found a commit and the launch's read could
// not be made. The launch's one notice names the round's commit.
//
// TestARoundsCallThatYieldedToALaunchsActThatWasNotMadeTellsNobody is the Legion reviewer's Medium
// 1 on #5285 at 18d81dad: path a, where a later launch resumes the Sandbox instead of deleting it,
// so the launch's act is not made and its record is forgotten. No record of the round's call is
// left to be told as a replacement of its own: the architect is told nothing, and no record stands.

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// launchWithPodSentence is what the notice of a launch's delete of the issue's Sandbox over a live
// pod of it says of that pod (launchTakenMessage).
const launchWithPodSentence = "The Sandbox ran pod " + podUpdatePod + ", and the pod went with it."

// podlessKeptVolumeRound is a rig whose implementer the daemon suspended with the round over a
// drift the pod's roles can keep working under recorded, unasked, with no process in the pod, over
// a workspace that holds passedOverCommit, on a kept volume the issue's next pod mounts: the round
// is over, nothing holds its pod, and the round replaces it by issue whatever its read found, since
// the work stays on the volume (roundKeeps).
func podlessKeptVolumeRound(t *testing.T) *podUpdateRig {
	t.Helper()
	rig := newPodUpdateRig(t, podUpdateRole{role: claim.RoleImplementer, state: supervise.StateSuspended, session: true,
		contract: api.DaemonAPIVersion(), suspendedBy: supervise.SuspendedByDaemon, round: podUpdateRound, reasons: podUpdateDrift, unasked: true})
	rig.drifts(podUpdateDrift)
	rig.rt.ScriptIssuePod(podUpdateIssue, podUpdatePod)
	rig.rt.ScriptWorkspaceWork(podUpdateIssue, roundUnpushed)
	rig.rt.ScriptKeptVolume(podUpdateIssue, podUpdateStart.Add(-time.Hour))
	return rig
}

// withPodTaken is the implementer's launch's delete of the issue's Sandbox of uid keptSandboxUID and
// the kept volume it owns, over podUpdatePod, the live pod of that Sandbox, for a disk shape the
// launch does not render, over a read through a launcher of the pod that found launchReadCommit.
func withPodTaken(r *podUpdateRig, operation string) runtime.TakenPod {
	return runtime.TakenPod{Operation: operation, Claim: r.tokens[claim.RoleImplementer], Issue: podUpdateIssue, PodUID: podUpdatePod,
		Why: "the issue's Sandbox has a disk shape that this launch does not render", Act: runtime.VolumeAct(keptSandboxUID), WithPod: true,
		Work: runtime.WorkspaceWork{Head: passedOverHead, Commits: []string{launchReadCommit}, Count: 1}}
}

// recordRoundOverAFailedRead writes the record of the round's replacement by issue of podUpdatePod
// over a read that failed, as the round writes it before its call waits for the pod's launch turn.
func (r *podUpdateRig) recordRoundOverAFailedRead() {
	r.t.Helper()
	read := workspaceRead{round: podUpdateRound, by: viaIssue, at: r.clock.Now(), err: errRoundRead}
	if _, ok := r.s.recordRoundReplacement(r.ctx, podUpdateIssue, podUpdateRound, podUpdatePod, podUpdateDrift, read); !ok {
		r.t.Fatal("record the round's replacement by issue")
	}
}

// wantToldOnceByTheLaunch sweeps roundSweeps times and holds the outbox to one workspace-taken notice
// to the issue's architect, the launch's of withPodTaken: it names the Sandbox it deleted, the pod
// that went with it and the commit its read found, and says nothing of a volume the round marked
// for a reset. The issue is written nothing, and no record stands.
func (r *podUpdateRig) wantToldOnceByTheLaunch(what string) {
	r.t.Helper()
	for range roundSweeps {
		r.tick()
	}
	told := r.architectNotices(record.WorkspaceTakenNotice)
	if len(told) != 1 || !strings.Contains(told[0].Reason, "deleted the Sandbox of "+podUpdateIssue+" (uid "+keptSandboxUID+")") ||
		!strings.Contains(told[0].Reason, launchWithPodSentence) || !strings.Contains(told[0].Reason, launchReadCommit) ||
		strings.Contains(told[0].Reason, "marked it for a reset") {
		r.t.Fatalf("the architect was told %+v %s, want one workspace-taken notice, the launch's, naming the Sandbox it deleted, the pod that went with it and the commit %s its read found",
			told, what, launchReadCommit)
	}
	if messages := r.messages(); len(messages) != 0 {
		r.t.Fatalf("the issue was written %q %s, want nothing: the launch's notice is the one word of the act", messages, what)
	}
	if records := r.replacementRecords(); records != 0 {
		r.t.Fatalf("the store holds %d records %s, want none", records, what)
	}
}

func TestALaunchThatFailedAfterItTookARoundsPodTellsTheArchitectOnce(t *testing.T) {
	t.Parallel()
	rig := podlessKeptVolumeRound(t)
	taken := withPodTaken(rig, "launch-volume-1")
	launched := false
	rig.rt.HoldReplaceIssuePod(func() {
		if launched {
			return
		}
		launched = true
		if err := rig.s.recordLaunchTakes(rig.ctx, taken); err != nil {
			t.Errorf("record the launch's act: %v", err)
		}
		// The launch suspends the Sandbox over the round's pod, which takes the pod, and then fails
		// in its wait for the pod to go, so it never tells the daemon what it took. The Sandbox has
		// a disk shape the launch does not render, so the next launch deletes it: the kept volume is
		// no longer one the issue's next pod mounts.
		rig.rt.ScriptSuspendedOver(podUpdatePod)
		rig.rt.ScriptKeptVolume(podUpdateIssue, time.Time{})
	})
	rig.awaitSwept(claim.RoleImplementer, "its round to end once the launch took its pod", func(c supervise.Claim) bool { return c.UpdateRound == "" })
	if !launched {
		t.Fatal("the sweep asked for no replacement by issue, so this case proves nothing")
	}
	if asked := rig.byIssueAsks(); len(asked) != 1 || asked[0].Method != "ReplaceIssuePodNoPod" {
		t.Fatalf("the replacement by issue was asked %+v, want one that found the pod taken", asked)
	}
	for range roundSweeps {
		rig.tick()
	}
	if told := rig.architectNotices(record.WorkspaceTakenNotice); len(told) != 0 {
		t.Fatalf("the architect was told %+v while the Sandbox and its kept volume stand, want nothing yet: the act is the launch's, and its record tells once the Sandbox is gone", told)
	}
	if records := rig.replacementRecords(); records != 1 {
		t.Fatalf("the store holds %d records while the Sandbox and its kept volume stand, want the launch's alone: the round's call yielded to it, and its read moved onto it where it weighed more", records)
	}

	rig.rt.ScriptSandboxGone(keptSandboxUID)
	rig.wantToldOnceByTheLaunch("once the Sandbox is gone")
}

func TestALaunchThatStoppedAfterItsDeleteTellsTheArchitectOnceAfterARestart(t *testing.T) {
	t.Parallel()
	rig := podlessKeptVolumeRound(t)
	if err := rig.s.recordLaunchTakes(rig.ctx, withPodTaken(rig, "launch-volume-1")); err != nil {
		t.Fatalf("record the launch's act: %v", err)
	}
	rig.recordRoundOverAFailedRead()

	// The launch deletes the Sandbox over the round's pod, and the daemon stops before the launch
	// tells it what it took. The next daemon finds no Sandbox of that uid and no pod of the issue.
	next := rig.restart()
	next.rt.ScriptIssuePod(podUpdateIssue, "")
	next.rt.ScriptSuspendedOver(podUpdatePod)
	next.rt.ScriptSandboxGone(keptSandboxUID)
	next.awaitSwept(claim.RoleImplementer, "its round to end with the launch's act", func(c supervise.Claim) bool { return c.UpdateRound == "" })
	next.wantToldOnceByTheLaunch("after the restart")
}

// unfinishedHoldLine is the sweep's line for an issue it decides nothing of, since the record of a
// replacement of the issue's pod stands for a round that is not over (reconcilePodUpdates).
const unfinishedHoldLine = "supervise: the record of a replacement of this issue's pod stands unfinished; no update round of the issue is decided this sweep"

func TestARoundsCallThatYieldedIsToldFromItsReadByTheLaunchsNotice(t *testing.T) {
	t.Parallel()
	rig := podlessKeptVolumeRound(t)
	// The round's replacement by issue reads the workspace and finds passedOverCommit. The launch's
	// read through a launcher of the pod could not be made.
	taken := withPodTaken(rig, "launch-volume-1")
	taken.Work, taken.Err = runtime.WorkspaceWork{}, errors.New("the pod's launchers are not bound to it, so no launcher of the pod can read the workspace")
	launched := false
	rig.rt.HoldReplaceIssuePod(func() {
		if launched {
			return
		}
		launched = true
		if err := rig.s.recordLaunchTakes(rig.ctx, taken); err != nil {
			t.Errorf("record the launch's act: %v", err)
		}
		rig.rt.ScriptSuspendedOver(podUpdatePod)
		rig.rt.ScriptKeptVolume(podUpdateIssue, time.Time{})
	})
	rig.awaitSwept(claim.RoleImplementer, "its round to end once the launch took its pod", func(c supervise.Claim) bool { return c.UpdateRound == "" })
	if !launched {
		t.Fatal("the sweep asked for no replacement by issue, so this case proves nothing")
	}
	if asked := rig.byIssueAsks(); len(asked) != 1 || asked[0].Method != "ReplaceIssuePodNoPod" {
		t.Fatalf("the replacement by issue was asked %+v, want one that found the pod taken", asked)
	}
	for range roundSweeps {
		rig.tick()
	}
	if records := rig.replacementRecords(); records != 1 {
		t.Errorf("the store holds %d records while the Sandbox and its kept volume stand, want the launch's alone, holding the round's read", records)
	}
	if held := loggedLines(t, rig.logs.String(), unfinishedHoldLine); len(held) != 0 {
		t.Errorf("the sweep held the issue's rounds %d times for a standing record, want never: no record of the round stands", len(held))
	}

	rig.rt.ScriptSandboxGone(keptSandboxUID)
	for range roundSweeps {
		rig.tick()
	}
	told := rig.architectNotices(record.WorkspaceTakenNotice)
	if len(told) != 1 || !strings.Contains(told[0].Reason, "deleted the Sandbox of "+podUpdateIssue+" (uid "+keptSandboxUID+")") ||
		!strings.Contains(told[0].Reason, passedOverCommit) || strings.Contains(told[0].Reason, "No read of the workspace could be made") {
		t.Fatalf("the architect was told %+v, want one workspace-taken notice, the launch's, naming the commit %s the round's read found, and not that no read could be made",
			told, passedOverCommit)
	}
	if messages := rig.messages(); len(messages) != 0 {
		t.Fatalf("the issue was written %q, want nothing: the launch's notice is the one word of the act", messages)
	}
	if records := rig.replacementRecords(); records != 0 {
		t.Fatalf("the store holds %d records once the notice is queued, want none", records)
	}
}

func TestARoundsCallThatYieldedToALaunchsActThatWasNotMadeTellsNobody(t *testing.T) {
	t.Parallel()
	rig := podlessKeptVolumeRound(t)
	launched := false
	rig.rt.HoldReplaceIssuePod(func() {
		if launched {
			return
		}
		launched = true
		// The launch records its delete of the Sandbox and its kept volume over the round's pod,
		// suspends the Sandbox, which takes the pod, and fails before its delete.
		if err := rig.s.recordLaunchTakes(rig.ctx, withPodTaken(rig, "launch-volume-1")); err != nil {
			t.Errorf("record the launch's act: %v", err)
		}
		rig.rt.ScriptSuspendedOver(podUpdatePod)
	})
	rig.awaitSwept(claim.RoleImplementer, "its round to end once the launch took its pod", func(c supervise.Claim) bool { return c.UpdateRound == "" })
	if !launched {
		t.Fatal("the sweep asked for no replacement by issue, so this case proves nothing")
	}
	if asked := rig.byIssueAsks(); len(asked) != 1 || asked[0].Method != "ReplaceIssuePodNoPod" {
		t.Fatalf("the replacement by issue was asked %+v, want one that found the pod taken", asked)
	}
	if records := rig.replacementRecords(); records != 1 {
		t.Errorf("the store holds %d records once the round's call yielded, want the launch's alone", records)
	}

	// A later launch resumes the Sandbox instead of deleting it, so the Sandbox runs a new pod on
	// its kept volume, built from the configuration as it now reads: the launch's delete was not
	// made, and nothing took the disk the round's pod ran on.
	rig.drifts()
	rig.rt.ScriptIssuePod(podUpdateIssue, "pod-of-the-resumed-sandbox")
	for range roundSweeps {
		rig.tick()
	}
	if told := rig.architectNotices(record.WorkspaceTakenNotice); len(told) != 0 {
		t.Fatalf("the architect was told %+v, want nothing: the launch's delete was not made, the Sandbox's new pod mounts the kept volume, and the round's call took nothing", told)
	}
	if messages := rig.messages(); len(messages) != 0 {
		t.Fatalf("the issue was written %q, want nothing", messages)
	}
	if records := rig.replacementRecords(); records != 0 {
		t.Fatalf("the store holds %d records once the launch's act read not made, want none", records)
	}
}

func TestTheSweepDecidesARecordOfATargetItDecidedByWhatThatRecordDid(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// rig is the rig the case runs on, and write writes the records, oldest first, and scripts
		// the cluster as the sweep finds it.
		rig   func(*testing.T) *podUpdateRig
		write func(*podUpdateRig)
		// says is what the one notice names, "" for a case that tells nobody; records is how many
		// records stand after; open is whether the round stays open.
		says    string
		records int
		open    bool
	}{
		{
			// An earlier launch's delete of the same Sandbox, which stopped before its delete, is
			// older: the finish of the launch's delete over the pod takes it.
			name: "gone", says: launchWithPodSentence,
			rig: func(t *testing.T) *podUpdateRig { return newPodUpdateRig(t, readyWorker(claim.RoleImplementer)) },
			write: func(r *podUpdateRig) {
				if err := r.s.recordLaunchTakes(r.ctx, volumeResetTaken(r, "launch-reset-1", "")); err != nil {
					r.t.Fatalf("record the earlier launch's act: %v", err)
				}
				if err := r.s.recordLaunchTakes(r.ctx, withPodTaken(r, "launch-volume-1")); err != nil {
					r.t.Fatalf("record the launch's act: %v", err)
				}
				r.rt.ScriptSuspendedOver(podUpdatePod)
				r.rt.ScriptSandboxGone(keptSandboxUID)
			},
		},
		{
			// The launch suspended the Sandbox over the round's pod and its delete has not come:
			// the pod is taken, but the launch's act is not confirmed, and the round's newer record
			// of the pod stands with the launch's.
			name: "stands", records: 2, open: true, rig: podlessBreakingRound,
			write: func(r *podUpdateRig) {
				if err := r.s.recordLaunchTakes(r.ctx, withPodTaken(r, "launch-volume-1")); err != nil {
					r.t.Fatalf("record the launch's act: %v", err)
				}
				r.recordRoundOverAFailedRead()
				r.rt.ScriptSuspendedOver(podUpdatePod)
			},
		},
		{
			// A launch replaced the round's pod as the round's own replacement while the round's
			// replacement by issue waited, and the round's own record was not forgotten: the walk
			// ends the round as the launch's, and the older record goes with the launch's.
			name: "the_walk's", says: launchReadCommit, rig: podlessBreakingRound,
			write: func(r *podUpdateRig) {
				r.recordRoundOverAFailedRead()
				r.rt.ScriptWorkspaceWork(podUpdateIssue, runtime.WorkspaceWork{Head: passedOverHead, Commits: []string{launchReadCommit}, Count: 1})
				if ends, err := r.s.roundPodEndsAtLaunch(r.ctx, r.tokens[claim.RoleImplementer], podUpdatePod); err != nil || !ends {
					r.t.Fatalf("the launch's reading of the round = %t, %v; want the pod replaced", ends, err)
				}
				r.rt.ScriptSuspendedOver(podUpdatePod)
				r.rt.ScriptIssuePod(podUpdateIssue, "")
			},
		},
		{
			// The launch deleted the Sandbox over the round's pod, and the round's record of the
			// pod is newer: the launch's record decides the pod, its finish takes the round's
			// record, and the round ends with no notice of its own.
			name: "another_round's", says: launchWithPodSentence, rig: podlessBreakingRound,
			write: func(r *podUpdateRig) {
				if err := r.s.recordLaunchTakes(r.ctx, withPodTaken(r, "launch-volume-1")); err != nil {
					r.t.Fatalf("record the launch's act: %v", err)
				}
				r.recordRoundOverAFailedRead()
				r.rt.ScriptSuspendedOver(podUpdatePod)
				r.rt.ScriptSandboxGone(keptSandboxUID)
				r.rt.ScriptIssuePod(podUpdateIssue, "")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := tc.rig(t)
			tc.write(rig)
			if !tc.open {
				rig.awaitSwept(claim.RoleImplementer, "its round to end", func(c supervise.Claim) bool { return c.UpdateRound == "" })
			}
			for range roundSweeps {
				rig.tick()
			}
			rig.settleAll()
			told := rig.architectNotices(record.WorkspaceTakenNotice)
			if want := map[bool]int{true: 1, false: 0}[tc.says != ""]; len(told) != want || want == 1 && !strings.Contains(told[0].Reason, tc.says) {
				t.Fatalf("the architect was told %+v, want %d workspace-taken notice(s), naming %q", told, want, tc.says)
			}
			if records := rig.replacementRecords(); records != tc.records {
				t.Fatalf("the store holds %d records after the sweeps, want %d", records, tc.records)
			}
			if open := rig.claimOf(claim.RoleImplementer).UpdateRound != ""; open != tc.open {
				t.Fatalf("the implementer's round is open = %t after the sweeps, want %t", open, tc.open)
			}
			if messages := rig.messages(); len(messages) != 0 {
				t.Fatalf("the issue was written %q, want nothing", messages)
			}
		})
	}
}

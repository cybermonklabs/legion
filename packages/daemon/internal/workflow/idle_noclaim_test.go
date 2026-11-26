package workflow

import (
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// unownedIssue is a root issue of a live tree that no claim of it can use any more: an architect
// that failed, a planner the daemon retired and an implementer the daemon's own close suspended,
// none of the three recording a process, the newest of their claim writes forty minutes old,
// nothing queued for a role of it, its Sandbox there and no release of it under way. Every
// condition of the rule for an issue with no live claim holds for it, and each row of the table
// below changes exactly one thing about it.
func unownedIssue() pods.IdleView {
	return pods.IdleView{
		Issue: "LEGION-310", Tree: "LEGION-310", Found: true,
		Phase: phase.Implementing, Status: "in_progress", Generation: 4,
		Root: pods.IdleRoot{Phase: phase.Implementing, Status: "in_progress", Generation: 4},
		Claims: []pods.IdleClaim{
			{
				Role: claim.RoleArchitect, State: supervise.StateFailed,
				UpdatedAt: idleNow.Add(-90 * time.Minute),
			},
			{
				Role: claim.RolePlanner, State: supervise.StateRetired,
				UpdatedAt: idleNow.Add(-60 * time.Minute),
			},
			{
				Role: claim.RoleImplementer, State: supervise.StateSuspended,
				SuspendedBy: supervise.SuspendedByDaemon,
				UpdatedAt:   idleNow.Add(-40 * time.Minute),
			},
		},
	}
}

// The pod of an issue every claim of which has stopped is released, and each condition that
// keeps such a pod standing is one row here, holding it under a hold of its own. The rows below
// the holds prove what this rule is deliberately not held by: a decline that says nothing of the
// disk the pod holds now (a read that failed, a start row after it, a pod made again since), and
// the two suspensions that are the daemon's own rather than an operator's. The rule is pure, so
// each row is the view, the clock, the idle period and whether the runtime holds the issue's
// Sandbox, and nothing else.
func TestTheRuleForAnIssueWithNoLiveClaim(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		view    pods.IdleView
		sandbox bool
		// kept is the runtime's word that the issue's Sandbox keeps a volume the next pod mounts.
		kept bool
		want IdleHold
	}{
		{
			name: "every condition holds", view: unownedIssue(), sandbox: true, want: IdleHoldNone,
		},
		{
			name: "a claim of the issue is live",
			view: func() pods.IdleView {
				v := unownedIssue()
				v.Claims[2].State, v.Claims[2].SuspendedBy = supervise.StateIdle, ""
				return v
			}(),
			sandbox: true, want: IdleHoldUnownedClaimLive,
		},
		{
			name: "a claim is suspended by an operator",
			view: func() pods.IdleView {
				v := unownedIssue()
				v.Claims[2].SuspendedBy = supervise.SuspendedByOperator
				return v
			}(),
			sandbox: true, want: IdleHoldUnownedOperatorSuspended,
		},
		{
			name: "the issue's Sandbox is gone", view: unownedIssue(), sandbox: false,
			want: IdleHoldUnownedSandboxGone,
		},
		{
			name: "a suspended claim still records a process",
			view: func() pods.IdleView {
				v := unownedIssue()
				v.Claims[2].HasProcess = true
				return v
			}(),
			sandbox: true, want: IdleHoldUnownedClaimProcess,
		},
		{
			name:    "a delivery waits for a role of the issue",
			view:    func() pods.IdleView { v := unownedIssue(); v.Waiting = true; return v }(),
			sandbox: true, want: IdleHoldUnownedDelivery,
		},
		{
			name:    "the issue has left the workflow",
			view:    func() pods.IdleView { v := unownedIssue(); v.Status = "backlog"; return v }(),
			sandbox: true, want: IdleHoldUnownedTreeNotLive,
		},
		{
			name:    "the tree lingers after its close",
			view:    func() pods.IdleView { v := unownedIssue(); v.Root.LingerUntil = &idleNow; return v }(),
			sandbox: true, want: IdleHoldUnownedTreeNotLive,
		},
		{
			// The pause stopped every role and kept the pod; the resume ended the pause, and no
			// role of the pod has been ready since.
			name:    "a pause holds the pod",
			view:    func() pods.IdleView { v := unownedIssue(); v.PauseHeld = true; return v }(),
			sandbox: true, want: IdleHoldUnownedPauseHeld,
		},
		{
			// A pod this rule would release while the project is paused is kept: the release's
			// own transaction marks nothing while the project is paused (store.ReleaseIdleIssue).
			name:    "the project is paused and no pause holds the pod",
			view:    func() pods.IdleView { v := unownedIssue(); v.Paused = true; return v }(),
			sandbox: true, want: IdleHoldPaused,
		},
		{
			// The pause's conjunct is the last: a pod inside its quiet period is not this rule's
			// yet, paused or not.
			name: "the project is paused and the pod is not idle",
			view: func() pods.IdleView {
				v := unownedIssue()
				v.Paused = true
				v.Claims[0].UpdatedAt = idleNow.Add(-time.Minute)
				return v
			}(),
			sandbox: true, want: IdleHoldUnownedNotIdle,
		},
		{
			name:    "a release of that Sandbox is already under way",
			view:    func() pods.IdleView { v := unownedIssue(); v.Releasing = true; return v }(),
			sandbox: true, want: IdleHoldUnownedReleasing,
		},
		{
			// The newest write of any claim decides, so moving the oldest claim's write inside
			// the period is what holds the pod, although two claims are older than it.
			name: "the newest claim write is inside the idle period",
			view: func() pods.IdleView {
				v := unownedIssue()
				v.Claims[0].UpdatedAt = idleNow.Add(-time.Minute)
				return v
			}(),
			sandbox: true, want: IdleHoldUnownedNotIdle,
		},
		{
			name:    "the record holds no claim of the issue",
			view:    func() pods.IdleView { v := unownedIssue(); v.Claims = nil; return v }(),
			sandbox: true, want: IdleHoldUnownedNoClaim,
		},
		{
			name:    "the record does not hold the issue at all",
			view:    func() pods.IdleView { v := unownedIssue(); v.Found = false; return v }(),
			sandbox: true, want: IdleHoldUnownedNoClaim,
		},
		{
			// The idle rule's check found work on this disk, and every claim stopped after it with
			// no start row: no role is left to read the disk again, so the decline is the one word
			// the daemon has of it, and the release would delete the work.
			name: "a decline for unpushed work on the pod's disk",
			view: func() pods.IdleView {
				v := unownedIssue()
				v.Declined = pods.IdleDecline{At: idleNow.Add(-30 * time.Minute), Reason: pods.DeclineUnpushedWork, StartedRow: 5}
				v.Claims[2].LastStartRow = 5
				return v
			}(),
			sandbox: true, want: IdleHoldUnownedUnpushedWork,
		},
		{
			// A claim of the issue ran a turn after the decline's last check, before it stopped:
			// the turn can have pushed the work or made more, so the decline no longer speaks for
			// the disk, and the release reads it again before it deletes anything (Read, below;
			// the Legion reviewer's point 2 on R6 of #5285).
			name: "a turn start after the decline's check ends it",
			view: func() pods.IdleView {
				v := unownedIssue()
				v.Claims[2].LastTurnStart = idleNow.Add(-20 * time.Minute)
				v.Declined = pods.IdleDecline{At: idleNow.Add(-30 * time.Minute), CheckedAt: idleNow.Add(-30 * time.Minute),
					Reason: pods.DeclineUnpushedWork, PodUID: "pod-a"}
				v.PodUID = "pod-a"
				return v
			}(),
			sandbox: true, want: IdleHoldNone,
		},
		{
			// The decline's hold comes before the pause's, as the quiet conjunct does: the pod is
			// kept for its work whether or not the project is paused.
			name: "the project is paused and a decline holds the pod",
			view: func() pods.IdleView {
				v := unownedIssue()
				v.Paused = true
				v.Declined = pods.IdleDecline{At: idleNow.Add(-30 * time.Minute), Reason: pods.DeclineUnpushedWork}
				return v
			}(),
			sandbox: true, want: IdleHoldUnownedUnpushedWork,
		},
		{
			// A role was started again after the decline in the very pod the decline's check read,
			// and stopped since: its process can have pushed the work, so the decline no longer
			// speaks for the disk, and the release reads it again first (reviewer's point 2).
			name: "a start row after the decline in the same pod ends it",
			view: func() pods.IdleView {
				v := unownedIssue()
				v.PodUID = "pod-a"
				v.Declined = pods.IdleDecline{At: idleNow.Add(-30 * time.Minute), Reason: pods.DeclineUnpushedWork, StartedRow: 5, PodUID: "pod-a"}
				v.Claims[2].LastStartRow = 6
				return v
			}(),
			sandbox: true, want: IdleHoldNone,
		},
		{
			// The decline's own pass: no start and no turn since its check, in the pod it read.
			name: "a decline in the same pod with no start and no turn since keeps it",
			view: func() pods.IdleView {
				v := unownedIssue()
				v.PodUID = "pod-a"
				v.Claims[2].LastStartRow, v.Claims[2].LastTurnStart = 5, idleNow.Add(-40*time.Minute)
				v.Declined = pods.IdleDecline{At: idleNow.Add(-30 * time.Minute), Reason: pods.DeclineUnpushedWork, StartedRow: 5, PodUID: "pod-a"}
				return v
			}(),
			sandbox: true, want: IdleHoldUnownedUnpushedWork,
		},
		{
			// The check could not say which pod it read: a start after it may have run in a pod made
			// again, so the decline says nothing of the disk now.
			name: "a start row after a decline that names no pod lets the pod go",
			view: func() pods.IdleView {
				v := unownedIssue()
				v.PodUID = "pod-a"
				v.Declined = pods.IdleDecline{At: idleNow.Add(-30 * time.Minute), Reason: pods.DeclineUnpushedWork, StartedRow: 5}
				v.Claims[2].LastStartRow = 6
				return v
			}(),
			sandbox: true, want: IdleHoldNone,
		},
		{
			// The runtime cannot say which pod it holds now: the same reading of the start row.
			name: "a start row after the decline where the runtime names no pod lets the pod go",
			view: func() pods.IdleView {
				v := unownedIssue()
				v.Declined = pods.IdleDecline{At: idleNow.Add(-30 * time.Minute), Reason: pods.DeclineUnpushedWork, StartedRow: 5, PodUID: "pod-a"}
				v.Claims[2].LastStartRow = 6
				return v
			}(),
			sandbox: true, want: IdleHoldNone,
		},
		{
			// The disk the check read went with the pod it read.
			name: "a decline read in another pod than the one the runtime holds now",
			view: func() pods.IdleView {
				v := unownedIssue()
				v.PodUID = "pod-b"
				v.Declined = pods.IdleDecline{At: idleNow.Add(-30 * time.Minute), Reason: pods.DeclineUnpushedWork, PodUID: "pod-a"}
				return v
			}(),
			sandbox: true, want: IdleHoldNone,
		},
		{
			// A read that failed after a read that found work leaves the work on record: the
			// failure says nothing new of the disk, and the last read that could read found work.
			name: "a decline for a workspace the check could not read after a read that found work",
			view: func() pods.IdleView {
				v := unownedIssue()
				v.PodUID = "pod-a"
				v.Declined = pods.IdleDecline{At: idleNow.Add(-30 * time.Minute), Reason: pods.DeclineWorkspaceUnread, PodUID: "pod-a", Work: true}
				return v
			}(),
			sandbox: true, want: IdleHoldUnownedUnpushedWork,
		},
		{
			// R6: the pod's launcher could not read the disk, so nothing shows it clean, and the
			// decline holds the pod until its retry, a start or another pod.
			name: "a decline for a workspace the check could not read and no work on record",
			view: func() pods.IdleView {
				v := unownedIssue()
				v.PodUID = "pod-a"
				v.Declined = pods.IdleDecline{At: idleNow.Add(-30 * time.Minute), Reason: pods.DeclineWorkspaceUnread, PodUID: "pod-a"}
				return v
			}(),
			sandbox: true, want: IdleHoldUnownedWorkspaceUnread,
		},
		{
			// The retry of a read that failed has come: the release reads again.
			name: "a decline for a workspace the check could not read, past its retry",
			view: func() pods.IdleView {
				v := unownedIssue()
				v.PodUID = "pod-a"
				v.Declined = pods.IdleDecline{At: idleNow.Add(-pods.DeclineUnreadRetry - time.Minute), Reason: pods.DeclineWorkspaceUnread, PodUID: "pod-a"}
				return v
			}(),
			sandbox: true, want: IdleHoldNone,
		},
		{
			// A kept volume the next pod mounts is not deleted, so no read is owed and a failed
			// one holds nothing. The first decision, made with the volume unread, holds the pod for
			// the failed read, and that hold is what has the daemon ask for the volume.
			name: "a kept volume and a decline for a workspace the check could not read",
			view: func() pods.IdleView {
				v := unownedIssue()
				v.PodUID = "pod-a"
				v.Declined = pods.IdleDecline{At: idleNow.Add(-30 * time.Minute), Reason: pods.DeclineWorkspaceUnread, PodUID: "pod-a"}
				return v
			}(),
			sandbox: true, kept: true, want: IdleHoldNone,
		},
		{
			// The ordinary case this rule meets: an earlier idle release suspended the role and
			// no wake brought it back.
			name: "a claim an earlier release suspended",
			view: func() pods.IdleView {
				v := unownedIssue()
				v.Claims[2].SuspendedBy = supervise.SuspendedByReleased
				return v
			}(),
			sandbox: true, want: IdleHoldNone,
		},
		{
			// Migration 0038's backfill of every suspension written before the column existed,
			// most of which are the daemon's own: holding on it would keep those pods for ever.
			name: "a claim suspended before the source was recorded",
			view: func() pods.IdleView {
				v := unownedIssue()
				v.Claims[2].SuspendedBy = supervise.SuspendedByUnknown
				return v
			}(),
			sandbox: true, want: IdleHoldNone,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The rule is decided as the daemon decides it (releaseUnownedPod): first with the
			// kept volume unread, and again with the runtime's word of it only where that first
			// decision turns on it.
			view := tc.view
			got := UnownedPodRelease(view, idleNow, idlePeriod, tc.sandbox)
			if got.TurnsOnKeptVolume() {
				view.KeptVolume = tc.kept
				got = UnownedPodRelease(view, idleNow, idlePeriod, tc.sandbox)
			}
			if got.Hold != tc.want {
				t.Fatalf("hold = %s, want %s", got.Hold, tc.want)
			}
			if want := tc.want == IdleHoldNone; got.Release != want {
				t.Fatalf("release = %v, want %v (hold %s)", got.Release, want, got.Hold)
			}
			// R6: a release that deletes the disk has the pod's launcher read it first.
			if want := got.Release && !tc.kept; got.Read != want {
				t.Fatalf("read = %v, want %v: a release reads the disk it deletes, and a kept volume is not deleted", got.Read, want)
			}
			if len(got.Claims) != 0 {
				t.Fatalf("claims = %v, want none: this release suspends no claim", got.Claims)
			}
		})
	}
}

// leftChild is idleChild once the child left the workflow while its tree runs on: its phase done
// and its status done, its root still in the workflow, and its implementer still live and quiet.
// That is the pod a close takes (LeftPodClose), and each row below changes one thing about it.
func leftChild() pods.IdleView {
	v := idleChild()
	v.Phase, v.Status = phase.Done, "done"
	return v
}

// A close with a live claim deletes a disk a role worked on, so it reads that disk first, as the
// idle release does: the decision names the live claims and the daemon queues one workspace check,
// which queues the stops only where it finds nothing unpushed. A decline that stands keeps the pod
// with its work, and while the check waits the pod is the daemon's own act under way. A pod with
// no live claim has no role to read through, so the decline on record is the one word the daemon
// has of its disk: where it says the disk holds work (pods.IdleDecline.HoldsUnpushedWork), the pod
// is kept as the release of a pod no claim owns keeps it, and otherwise it is closed.
// LeftPodCloseAfterCheck is the check's own decision once its read found nothing, and reads no
// decline and no queued check: it is that check. It reads the moment the read began, and a turn
// that started after it, by any claim, is one the read may not have seen.
func TestTheCloseOfALeftPodReadsTheWorkspaceOfALiveRoleFirst(t *testing.T) {
	t.Parallel()
	standing := pods.IdleDecline{At: idleNow.Add(-30 * time.Minute), Reason: pods.DeclineUnpushedWork}
	// failedImplementer is leftChild with no live claim: its implementer failed and records no
	// process, its tester suspended beside it.
	failedImplementer := func() pods.IdleView {
		v := leftChild()
		v.PodUID = "pod-a"
		v.Claims[0].State = supervise.StateFailed
		return v
	}
	implementer := store.ReleasedClaim{Token: leftChild().Claims[0].Token, Role: claim.RoleImplementer}
	other := store.ReleasedClaim{Token: claim.Token(string(implementer.Token) + "-other"), Role: claim.RoleImplementer}
	for _, tc := range []struct {
		name   string
		view   pods.IdleView
		want   IdleHold
		claims []store.ReleasedClaim
	}{
		{name: "every condition holds", view: leftChild(), claims: []store.ReleasedClaim{implementer}},
		{
			name: "two live claims of one role both stop",
			view: func() pods.IdleView {
				v := leftChild()
				c := v.Claims[0]
				c.Token = other.Token
				v.Claims = append(v.Claims, c)
				return v
			}(),
			claims: []store.ReleasedClaim{implementer, other},
		},
		{
			name: "the close's workspace check is still queued",
			view: func() pods.IdleView { v := leftChild(); v.WorkspaceCheckQueued = true; return v }(),
			want: IdleHoldReleasing,
		},
		{
			name: "a live role and a standing decline for unpushed work",
			view: func() pods.IdleView { v := leftChild(); v.Declined = standing; return v }(),
			want: IdleHoldUnpushedWork,
		},
		{
			name: "a live role and a standing decline for a workspace the check could not read",
			view: func() pods.IdleView {
				v := leftChild()
				v.Declined = pods.IdleDecline{At: idleNow.Add(-30 * time.Minute), Reason: pods.DeclineWorkspaceUnread}
				return v
			}(),
			want: IdleHoldWorkspaceUnread,
		},
		{
			// The decline's conjunct is after the quiet one, as in the idle rule.
			name: "a live role, a standing decline and a pod that recorded something inside the period",
			view: func() pods.IdleView {
				v := leftChild()
				v.Declined = standing
				v.Claims[0].UpdatedAt = idleNow.Add(-time.Minute)
				return v
			}(),
			want: IdleHoldNotIdle,
		},
		{
			// The implementer failed after a check found one commit on this pod's disk, and no
			// role is left to read it again: the close would delete the work.
			name: "no live claim and a decline that says the disk holds work",
			view: func() pods.IdleView {
				v := failedImplementer()
				v.Declined = pods.IdleDecline{At: idleNow.Add(-30 * time.Minute), CheckedAt: idleNow.Add(-30 * time.Minute),
					Reason: pods.DeclineUnpushedWork, PodUID: "pod-a", Work: true}
				return v
			}(),
			want: IdleHoldUnownedUnpushedWork,
		},
		{
			// A role came back into the pod after that check and failed again: its process can
			// have pushed the work, so the close reads the disk again (the Legion reviewer's point
			// 2 on R6).
			name: "no live claim, a decline that says the disk holds work, and a start since its check",
			view: func() pods.IdleView {
				v := failedImplementer()
				v.Claims[0].LastStartRow = 7
				v.Declined = pods.IdleDecline{At: idleNow.Add(-30 * time.Minute), CheckedAt: idleNow.Add(-30 * time.Minute),
					Reason: pods.DeclineUnpushedWork, StartedRow: 6, PodUID: "pod-a", Work: true}
				return v
			}(),
		},
		{
			// The same for a turn since the check, of a claim that records no process now.
			name: "no live claim, a decline that says the disk holds work, and a turn since its check",
			view: func() pods.IdleView {
				v := failedImplementer()
				v.Claims[0].LastTurnStart = idleNow.Add(-20 * time.Minute)
				v.Declined = pods.IdleDecline{At: idleNow.Add(-30 * time.Minute), CheckedAt: idleNow.Add(-30 * time.Minute),
					Reason: pods.DeclineUnpushedWork, PodUID: "pod-a", Work: true}
				return v
			}(),
		},
		{
			// R6: the pod's launcher could not read the disk, and that decline stands.
			name: "no live claim and a decline for a workspace the launcher could not read",
			view: func() pods.IdleView {
				v := failedImplementer()
				v.Declined = pods.IdleDecline{At: idleNow.Add(-30 * time.Minute), CheckedAt: idleNow.Add(-30 * time.Minute),
					Reason: pods.DeclineWorkspaceUnread, PodUID: "pod-a"}
				return v
			}(),
			want: IdleHoldUnownedWorkspaceUnread,
		},
		{name: "no live claim and no decline", view: failedImplementer()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := LeftPodClose(tc.view, idleNow, idlePeriod)
			if got.Hold != tc.want || got.Close != (tc.want == IdleHoldNone) || got.Release {
				t.Fatalf("decision = %+v, want hold %s and a close only where nothing holds", got, tc.want)
			}
			if !slices.Equal(got.Claims, tc.claims) {
				t.Fatalf("claims = %v, want %v", got.Claims, tc.claims)
			}
			// R6: a close with no live role has the pod's launcher read the disk first; one with a
			// live role reads through that role.
			if want := got.Close && len(tc.claims) == 0; got.Read != want {
				t.Fatalf("read = %v, want %v", got.Read, want)
			}
		})
	}

	t.Run("after the check", func(t *testing.T) {
		// turnStart is the newest turn start of the pass that queued the check.
		turnStart := idleNow.Add(-time.Minute)
		// The check found nothing: its own queued row and the decline it replaces hold nothing.
		v := leftChild()
		v.WorkspaceCheckQueued, v.Declined = true, standing
		v.Claims[0].LastTurnStart = turnStart
		if got := LeftPodCloseAfterCheck(v, turnStart); !got.Close || !slices.Equal(got.Claims, []store.ReleasedClaim{implementer}) {
			t.Fatalf("decision after a clean read = %+v, want the close of the implementer", got)
		}
		// A turn that started after the pass, of a claim that is no longer live as much as of a
		// live one, is one the read may not have seen, even where it began before the read and
		// ended while it ran: the next pass reads again.
		for i := range v.Claims {
			turned := v
			turned.Claims = slices.Clone(v.Claims)
			turned.Claims[i].LastTurnStart = turnStart.Add(time.Second)
			if got := LeftPodCloseAfterCheck(turned, turnStart); got.Close || got.Hold != IdleHoldNotIdle {
				t.Fatalf("decision after a clean read with the %s's turn since = %+v, want %s", turned.Claims[i].Role, got, IdleHoldNotIdle)
			}
		}
		// A role in a turn now holds the close for the next pass's read.
		v.Claims[0].State = supervise.StateWorking
		if got := LeftPodCloseAfterCheck(v, turnStart); got.Close || got.Hold != IdleHoldInTurn {
			t.Fatalf("decision after a clean read with a turn running = %+v, want %s", got, IdleHoldInTurn)
		}
	})
}

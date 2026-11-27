package daemon

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/store/storetext"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// R6 of #5285: no release deletes a disk that no read has shown clean. The release
// of a pod no claim owns and the close of a left pod with no live claim take a pod in which no role
// process runs, so no role's shim can read its workspace. The pod's launcher, which runs in the pod
// whatever its roles do, makes the read instead, with R5's count (runtime.PodWorkspaceReader). A
// read that finds work that origin does not hold, or that cannot be made, keeps the pod, and the
// issue's architect is told once for the pod, with the state of each claim and what the read found.

// noRoleRig is newLeftPodRig's child with no role process in its pod: its planner failed and
// records no process, and its implementer is suspended by an earlier idle release, with no
// process either. With parked false the child stands in the workflow at implementing, which is the
// pod the release of a pod no claim owns decides (workflow.UnownedPodRelease); with parked true it
// stands where a park leaves it under its running tree, which is the left pod's close
// (workflow.LeftPodClose). The tree root's architect holds its role, so a notice of the child
// reaches that architect's role topic, which publisher records. No decline of the child's release
// stands, so nothing on record says what the child's disk holds.
func noRoleRig(t *testing.T, parked bool) (*idleRig, *outboxPublisher) {
	t.Helper()
	ctx := context.Background()
	rig := newLeftPodRig(t, parked)
	publisher := &outboxPublisher{}
	rig.runner.notices = publisher
	if _, err := rig.pool.Exec(ctx, `update claims set state = 'failed', suspended_by = '', locator = null
		where token = $1`, string(rig.token)); err != nil {
		t.Fatalf("fail the planner: %v", err)
	}
	implementer := supervise.Claim{Token: mustClaimToken(t, rig.issue.Key, claim.RoleImplementer), Project: "legion",
		Tree: rig.issue.Tree, Issue: rig.issue.Key, Role: claim.RoleImplementer, TreeEpoch: 1, Generation: 1,
		ServingGeneration: 1, State: supervise.StateSuspended, SuspendedBy: supervise.SuspendedByReleased,
		Session: "ses-implementer", SessionFile: "/tmp/implementer.jsonl", PluginContract: api.DaemonAPIVersion()}
	if err := rig.st.PutClaim(ctx, implementer); err != nil {
		t.Fatalf("seed the child's released implementer: %v", err)
	}
	quietPod(t, rig)
	if _, _, err := rig.sup.Create(ctx, supervise.Claim{Token: mustClaimToken(t, rig.issue.Tree, claim.RoleArchitect),
		Project: "legion", Tree: rig.issue.Tree, Issue: rig.issue.Tree, Role: claim.RoleArchitect,
		State: supervise.StateWorking}, ""); err != nil {
		t.Fatalf("create the root's architect: %v", err)
	}
	if rig.declined(t) {
		t.Fatal("a decline of the child's release stands before any pass")
	}
	return rig, publisher
}

// launcherReads is how many workspace reads of the rig's issue the runtime was asked to make
// through the pod's launcher, and roleReads how many through a role's shim.
func launcherReads(rig *idleRig) int { return len(rig.rt.CallsOf("PodWorkspaceWork")) }

func roleReads(rig *idleRig) int { return len(rig.rt.CallsOf("WorkspaceWork")) }

// keptNotices is every notice of the kind record.WorkspaceKeptNotice published to the role topic of
// the rig's tree root's architect, in order.
func keptNotices(t *testing.T, rig *idleRig, publisher *outboxPublisher) []record.Notice {
	t.Helper()
	topic := architectTopic(t, rig.issue.Tree)
	var notices []record.Notice
	for _, published := range publisher.publishes() {
		notice, ok := published.payload.(record.Notice)
		if published.topic == topic && ok && notice.Kind == record.WorkspaceKeptNotice {
			notices = append(notices, notice)
		}
	}
	return notices
}

// holdsNamed is every hold record of the rig's project under the recorded name reason.
func holdsNamed(t *testing.T, rig *idleRig, reason string) []store.PodHold {
	t.Helper()
	var held []store.PodHold
	for _, hold := range rig.records(t).Holds {
		if hold.Reason == reason {
			held = append(held, hold)
		}
	}
	return held
}

// keptOverPasses runs pass and a drain of the outbox idleRigPasses times, and holds that the pod
// was kept through all of them: no teardown asked, the issue's Sandbox still held, the workspace
// read once through the pod's launcher and never through a role, one hold streak of hold that the
// passes counted on, and one notice to the issue's architect, which names each claim's state and
// holds every string of found. It answers that notice.
func keptOverPasses(t *testing.T, rig *idleRig, publisher *outboxPublisher, pass func(), hold string, found ...string) record.Notice {
	t.Helper()
	for range idleRigPasses {
		pass()
		rig.drain(t)
	}
	if asked := rig.rt.teardownsAsked(); len(asked) != 0 {
		t.Fatalf("teardowns asked = %v, want none: no read showed the pod's disk clean", asked)
	}
	if _, present := rig.rt.IssueSandbox(rig.issue.Key); !present {
		t.Fatal("the runtime no longer holds the issue's Sandbox")
	}
	if launcher, role := launcherReads(rig), roleReads(rig); launcher != 1 || role != 0 {
		t.Fatalf("workspace reads = %d through the pod's launcher and %d through a role over %d passes, want one through the launcher and none through a role: no role process runs in the pod",
			launcher, role, idleRigPasses)
	}
	if held := holdsNamed(t, rig, hold); len(held) != 1 || held[0].Passes < 2 {
		t.Fatalf("holds = %+v, want one %s streak the passes counted on", rig.records(t).Holds, hold)
	}
	if !rig.declined(t) {
		t.Fatal("the read that kept the pod recorded no decline")
	}
	notices := keptNotices(t, rig, publisher)
	if len(notices) != 1 {
		t.Fatalf("%s notices to the architect of %s = %d, want one for the kept pod", record.WorkspaceKeptNotice, rig.issue.Tree, len(notices))
	}
	for _, want := range append([]string{"planner=failed", "implementer=suspended"}, found...) {
		if !strings.Contains(notices[0].Reason, want) {
			t.Fatalf("the notice says %q, want it to name %q", notices[0].Reason, want)
		}
	}
	return notices[0]
}

// The release of a pod no claim owns: the child stands in the workflow, every claim of it has
// failed or has no process, and no decline says what its disk holds.
//
// one_unpushed_commit: the workspace holds one commit that origin does not hold. Over the passes
// the pod is not deleted, the launcher reads the workspace once, the pod is held as
// unowned_unpushed_work, and the architect gets one notice naming the commit.
//
// read_cannot_be_made: the launcher cannot read the workspace. The pod is kept the same way, held
// as unowned_workspace_unread, and the architect gets one notice naming the failure.
//
// control:_a_clean_read_releases: the launcher reads nothing that origin does not hold, and the
// pass's release takes the pod: the teardown is asked once, after that read.
//
// control:_a_kept_volume_needs_no_read: the issue's Sandbox keeps a volume the issue's next pod
// mounts, so the release deletes no disk and reads nothing first.
//
// a_kept_volume_with_a_standing_unread_decline_is_released: the issue's Sandbox keeps a volume the
// issue's next pod mounts, and a decline stands whose read failed, made through a role's shim
// before the roles stopped (the Legion reviewer's point 1 on R6's review fixes). The release
// deletes no disk, so no read is owed and the failed one holds nothing: the release takes the pod
// at its first pass, with no read and no unowned_workspace_unread hold.
func TestTheReleaseOfAPodNoClaimOwnsReadsItsDiskFirst(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	unownedPass := func(rig *idleRig) func() {
		return func() { rig.w.releaseUnownedPods(ctx) }
	}

	t.Run("one_unpushed_commit", func(t *testing.T) {
		rig, publisher := noRoleRig(t, false)
		rig.rt.ScriptWorkspaceWork(rig.issue.Key, idleRigUnpushed)
		keptOverPasses(t, rig, publisher, unownedPass(rig), workflow.IdleHoldUnownedUnpushedWork.String(), idleRigUnpushed.Commits[0])
		if rig.marked(t) {
			t.Fatal("the declined release left its mark behind")
		}
	})

	t.Run("read_cannot_be_made", func(t *testing.T) {
		rig, publisher := noRoleRig(t, false)
		rig.rt.FailWorkspaceWork(rig.issue.Key, errors.New("no launcher of the pod reads a workspace"))
		keptOverPasses(t, rig, publisher, unownedPass(rig), workflow.IdleHoldUnownedWorkspaceUnread.String(), "no launcher of the pod reads a workspace")
	})

	t.Run("control:_a_clean_read_releases", func(t *testing.T) {
		rig, publisher := noRoleRig(t, false)
		rig.rt.ScriptWorkspaceWork(rig.issue.Key, runtime.WorkspaceWork{Head: idleRigUnpushed.Head})
		rig.w.releaseUnownedPods(ctx)
		rig.drain(t)
		if asked := rig.rt.teardownsAsked(); !slices.Equal(asked, []string{"ReleaseIssue"}) {
			t.Fatalf("teardowns asked = %v, want the release of the pod no claim owns", asked)
		}
		if launcher, role := launcherReads(rig), roleReads(rig); launcher != 1 || role != 0 {
			t.Fatalf("workspace reads = %d through the pod's launcher and %d through a role, want one through the launcher before the teardown", launcher, role)
		}
		if notices := keptNotices(t, rig, publisher); len(notices) != 0 {
			t.Fatalf("notices to the architect = %+v, want none: the pod was not kept", notices)
		}
	})

	t.Run("control:_a_kept_volume_needs_no_read", func(t *testing.T) {
		rig, _ := noRoleRig(t, false)
		rig.rt.ScriptKeptVolume(rig.issue.Key, time.Now().Add(-time.Hour))
		rig.rt.ScriptWorkspaceWork(rig.issue.Key, idleRigUnpushed)
		rig.w.releaseUnownedPods(ctx)
		rig.drain(t)
		if asked := rig.rt.teardownsAsked(); !slices.Equal(asked, []string{"ReleaseIssue"}) {
			t.Fatalf("teardowns asked = %v, want the release, which keeps the Sandbox and its volume", asked)
		}
		if launcher := launcherReads(rig); launcher != 0 {
			t.Fatalf("workspace reads through the launcher = %d, want none: a kept volume the next pod mounts is not deleted", launcher)
		}
	})

	t.Run("a_kept_volume_with_a_standing_unread_decline_is_released", func(t *testing.T) {
		rig, publisher := noRoleRig(t, false)
		rig.rt.ScriptKeptVolume(rig.issue.Key, time.Now().Add(-time.Hour))
		if _, err := rig.pool.Exec(ctx, `insert into idle_release_declines
			(project, issue, declined_at, checked_at, role, reason, started_row, pod_uid)
			values ('legion', $1, now() - interval '30 minutes', now() - interval '30 minutes', 'implementer', $2,
				(select coalesce(max(last_start_row), 0) from claims where issue = $1), '')`,
			rig.issue.Key, pods.DeclineWorkspaceUnread); err != nil {
			t.Fatalf("record the decline of the implementer's read that failed: %v", err)
		}
		rig.w.releaseUnownedPods(ctx)
		rig.drain(t)
		if asked := rig.rt.teardownsAsked(); !slices.Equal(asked, []string{"ReleaseIssue"}) {
			t.Fatalf("teardowns asked = %v, want the release, which keeps the Sandbox and its volume: a failed read holds no pod whose disk the release keeps", asked)
		}
		if launcher, role := launcherReads(rig), roleReads(rig); launcher != 0 || role != 0 {
			t.Fatalf("workspace reads = %d through the pod's launcher and %d through a role, want none: a kept volume the next pod mounts is not deleted", launcher, role)
		}
		if held := holdsNamed(t, rig, workflow.IdleHoldUnownedWorkspaceUnread.String()); len(held) != 0 {
			t.Fatalf("holds = %+v, want no %s hold: the release deletes no disk, so a failed read holds nothing", held, workflow.IdleHoldUnownedWorkspaceUnread)
		}
		if notices := keptNotices(t, rig, publisher); len(notices) != 0 {
			t.Fatalf("notices to the architect = %+v, want none: the pod was not kept", notices)
		}
	})

	// Sol's point 4 on R6: a clean read, then a turn of a claim of the issue that has already ended
	// on the planner, which records no process. The turn can have made work the read did not see,
	// so it voids the read, and the release's teardown takes nothing (store's releaseOvertaken).
	t.Run("a_turn_after_the_clean_read_voids_it", func(t *testing.T) {
		rig, _ := noRoleRig(t, false)
		rig.rt.ScriptWorkspaceWork(rig.issue.Key, runtime.WorkspaceWork{Head: idleRigUnpushed.Head})
		rig.rt.mu.Lock()
		rig.rt.afterPodRead = func() { turnSinceTheRead(t, rig) }
		rig.rt.mu.Unlock()
		rig.w.releaseUnownedPods(ctx)
		rig.drain(t)
		if launcherReads(rig) != 1 {
			t.Fatalf("the launcher read %d times, want once", launcherReads(rig))
		}
		if _, suspended := rig.rt.reads(); suspended != 0 {
			t.Fatalf("the teardown took %d pods, want none: a turn after the clean read voids it", suspended)
		}
	})
}

// The close of a left pod with no live claim while its tree runs on: the child was parked under
// its running root, every claim of it has failed or has no process, and no decline says what its
// disk holds. The close deletes the Sandbox with its pod and any volume, so it always reads first.
//
// one_unpushed_commit and read_cannot_be_made: as for the release of a pod no claim owns, the pod
// is kept over the passes and the architect gets one notice.
//
// control:_a_clean_read_closes: the launcher reads nothing that origin does not hold, and the
// close goes on to the Sandbox suspension.
func TestTheCloseOfALeftPodWithNoLiveClaimReadsItsDiskFirst(t *testing.T) {
	t.Parallel()
	leftPass := func(rig *idleRig) func() { return func() { idlePasses(rig) } }

	t.Run("one_unpushed_commit", func(t *testing.T) {
		rig, publisher := noRoleRig(t, true)
		rig.rt.ScriptWorkspaceWork(rig.issue.Key, idleRigUnpushed)
		keptOverPasses(t, rig, publisher, leftPass(rig), workflow.IdleHoldUnownedUnpushedWork.String(), idleRigUnpushed.Commits[0])
		for _, left := range rig.rows(t) {
			if left == "issue_suspend" {
				t.Fatalf("rows left after the passes = %v, want no Sandbox suspension queued", rig.rows(t))
			}
		}
	})

	t.Run("read_cannot_be_made", func(t *testing.T) {
		rig, publisher := noRoleRig(t, true)
		rig.rt.FailWorkspaceWork(rig.issue.Key, errors.New("no launcher of the pod reads a workspace"))
		keptOverPasses(t, rig, publisher, leftPass(rig), workflow.IdleHoldUnownedWorkspaceUnread.String(), "no launcher of the pod reads a workspace")
	})

	t.Run("control:_a_clean_read_closes", func(t *testing.T) {
		rig, publisher := noRoleRig(t, true)
		idlePasses(rig)
		rig.drain(t)
		if asked := rig.rt.teardownsAsked(); !slices.Equal(asked, []string{"SuspendIssue"}) {
			t.Fatalf("teardowns asked = %v, want the close's Sandbox suspension", asked)
		}
		if launcher, role := launcherReads(rig), roleReads(rig); launcher != 1 || role != 0 {
			t.Fatalf("workspace reads = %d through the pod's launcher and %d through a role, want one through the launcher before the close", launcher, role)
		}
		if notices := keptNotices(t, rig, publisher); len(notices) != 0 {
			t.Fatalf("notices to the architect = %+v, want none: the pod was not kept", notices)
		}
	})

	// Sol's point 4 on R6: a clean read, then a turn that has already ended on the planner, which
	// records no process, after the check queued the Sandbox suspension and before that suspension
	// is authorized. The suspension carries the check's turn start (record.IssueSuspend.TurnStart),
	// so it finishes without acting and takes nothing.
	t.Run("a_turn_after_the_clean_read_voids_it", func(t *testing.T) {
		rig, _ := noRoleRig(t, true)
		rig.rt.mu.Lock()
		rig.rt.beforeAuthorize = func() { turnSinceTheRead(t, rig) }
		rig.rt.mu.Unlock()
		idlePasses(rig)
		rig.drain(t)
		if asked := rig.rt.teardownsAsked(); !slices.Equal(asked, []string{"SuspendIssue"}) {
			t.Fatalf("teardowns asked = %v, want the close's Sandbox suspension asked once", asked)
		}
		if _, suspended := rig.rt.reads(); suspended != 0 {
			t.Fatalf("the close's suspension took %d pods, want none: a turn after the clean read voids it", suspended)
		}
	})
}

// turnSinceTheRead records a turn of the rig's planner that started after the launcher's read and
// has ended: the planner's claim stays failed and records no process.
func turnSinceTheRead(t *testing.T, rig *idleRig) {
	t.Helper()
	if _, err := rig.pool.Exec(context.Background(), `update claims set last_turn_start = now() where token = $1`, string(rig.token)); err != nil {
		t.Errorf("record the planner's turn: %v", err)
	}
}

// declinedPod is the pod the issue's decline row names, and whether a row stands.
func declinedPod(t *testing.T, rig *idleRig) (string, bool) {
	t.Helper()
	var pod string
	err := rig.pool.QueryRow(context.Background(), `select pod_uid from idle_release_declines where issue = $1`, rig.issue.Key).Scan(&pod)
	if err != nil && strings.Contains(err.Error(), "no rows") {
		return "", false
	}
	if err != nil {
		t.Fatalf("read the decline of %s: %v", rig.issue.Key, err)
	}
	return pod, true
}

// commitWatch is the release's store with each commit of a release counted: each answer of the
// runtime's read under its pod lock (CommitIssueRelease) that carries Committed, which that read
// wrote (issue_releases.committed) or found written. Every call reaches the real store.
type commitWatch struct {
	outboxTrees
	commits atomic.Int32
}

func (s *commitWatch) CommitIssueRelease(ctx context.Context, project string, release store.IssueReleased,
	outOfWorkflow func(status string) bool) (store.ReleaseDecision, error) {
	decision, err := s.outboxTrees.CommitIssueRelease(ctx, project, release, outOfWorkflow)
	if err == nil && decision.Committed {
		s.commits.Add(1)
	}
	return decision, err
}

// A launcher's read is of one pod, and only that pod is named or taken on its word (Sol's read of
// R6, point 2). The pass marks the release, or queues the close, over pod-a. pod-b then takes its
// place: while the read is in flight, after the launcher has read pod-a (afterPodRead), or before
// the read at all.
//
// work_then_another_pod: the read found work in pod-a, and the decline names pod-a, not the pod
// the runtime holds once the read has answered.
//
// clean_then_another_pod: the read found pod-a clean, and the release's teardown takes nothing:
// its authorization under the pod lock is for pod-a, which is gone, so the teardown cancels the
// release as one overtaken (outbox.releaseIssue's authorize). The case asserts the whole cancel.
// Before the drain the teardown row names pod-a. No read of the runtime commits the release
// (commitWatch), since a committed release cannot be cancelled (store.CancelIssueRelease): a
// teardown that committed first would fail its row at every attempt, with the mark left committed.
// After the drain the row has finished with no "outbox row failed" line and the mark is gone; no
// release is recorded and the one hold is workflow.IdleHoldOvertaken; the teardown logged one
// pod-check line, naming pod-a; and the runtime was asked once and refused once.
//
// close_clean_then_another_pod: the same for the left pod's close: the Sandbox suspension takes
// nothing.
//
// a_read_of_another_pod: pod-b took pod-a's place before the read, so the launcher read pod-b, not
// the pod the release was marked over. The read is discarded: no decline, no notice, and no
// teardown.
func TestALauncherReadNamesAndTakesOnlyThePodItRead(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	replaced := func(rig *idleRig) func() {
		return func() { rig.rt.ScriptIssuePod(rig.issue.Key, "pod-b") }
	}

	t.Run("work_then_another_pod", func(t *testing.T) {
		rig, _ := noRoleRig(t, false)
		rig.rt.ScriptIssuePod(rig.issue.Key, "pod-a")
		rig.rt.ScriptWorkspaceWork(rig.issue.Key, idleRigUnpushed)
		rig.rt.mu.Lock()
		rig.rt.afterPodRead = replaced(rig)
		rig.rt.mu.Unlock()
		rig.w.releaseUnownedPods(ctx)
		rig.drain(t)
		if pod, declined := declinedPod(t, rig); !declined || pod != "pod-a" {
			t.Fatalf("the decline names pod %q (standing %t), want pod-a, the pod the launcher read", pod, declined)
		}
		if _, suspended := rig.rt.reads(); suspended != 0 {
			t.Fatalf("the teardown took %d pods, want none: the read found work", suspended)
		}
	})

	t.Run("clean_then_another_pod", func(t *testing.T) {
		rig, _ := noRoleRig(t, false)
		var logs syncBuffer
		rig.runner.log = jsonLogger(&logs)
		watch := &commitWatch{outboxTrees: rig.st}
		rig.runner.trees = watch
		rig.rt.ScriptIssuePod(rig.issue.Key, "pod-a")
		rig.rt.ScriptWorkspaceWork(rig.issue.Key, runtime.WorkspaceWork{Head: idleRigUnpushed.Head})
		rig.rt.mu.Lock()
		rig.rt.afterPodRead = replaced(rig)
		rig.rt.mu.Unlock()
		rig.w.releaseUnownedPods(ctx)
		if pod, _, queued := rig.teardownRow(t); !queued || pod != "pod-a" {
			t.Fatalf("the teardown row takes pod %q (queued %t), want a queued teardown of pod-a, the pod the pass read", pod, queued)
		}
		rig.drain(t)
		if launcherReads(rig) != 1 {
			t.Fatalf("the launcher read %d times, want once", launcherReads(rig))
		}
		if _, suspended := rig.rt.reads(); suspended != 0 {
			t.Fatalf("the teardown took %d pods, want none: the clean read was of pod-a, and the runtime holds pod-b", suspended)
		}
		failed := loggedMessages(t, logs.String(), "outbox row failed")
		if _, attempts, queued := rig.teardownRow(t); queued || rig.marked(t) || len(failed) != 0 {
			t.Fatalf("the teardown row is queued = %t after %d attempts, the mark stands = %t and the rows failed %v; want the release cancelled: its row finished and its mark gone",
				queued, attempts, rig.marked(t), failed)
		}
		if commits := watch.commits.Load(); commits != 0 {
			t.Fatalf("the runtime's reads committed the release %d times, want none: the release was cancelled", commits)
		}
		if records := rig.records(t); len(records.Releases) != 0 || len(records.Holds) != 1 ||
			records.Holds[0].Reason != workflow.IdleHoldOvertaken.String() {
			t.Fatalf("releases = %+v and holds = %+v, want no release and the one %s", records.Releases, records.Holds, workflow.IdleHoldOvertaken)
		}
		if lines := loggedMessages(t, logs.String(), idleRigReadPodGone); len(lines) != 1 || lines[0]["readPod"] != "pod-a" {
			t.Fatalf("the teardown logged %v, want one line %q naming pod-a", lines, idleRigReadPodGone)
		}
		if asked := rig.rt.teardownsAsked(); !slices.Equal(asked, []string{"ReleaseIssue"}) || rig.rt.refusals() != 1 {
			t.Fatalf("the runtime was asked %v and refused %d times, want the release's teardown asked and refused once", asked, rig.rt.refusals())
		}
	})

	t.Run("close_clean_then_another_pod", func(t *testing.T) {
		rig, _ := noRoleRig(t, true)
		rig.rt.ScriptIssuePod(rig.issue.Key, "pod-a")
		rig.rt.ScriptWorkspaceWork(rig.issue.Key, runtime.WorkspaceWork{Head: idleRigUnpushed.Head})
		rig.rt.mu.Lock()
		rig.rt.afterPodRead = replaced(rig)
		rig.rt.mu.Unlock()
		idlePasses(rig)
		rig.drain(t)
		if launcherReads(rig) != 1 {
			t.Fatalf("the launcher read %d times, want once", launcherReads(rig))
		}
		if _, suspended := rig.rt.reads(); suspended != 0 {
			t.Fatalf("the close's suspension took %d pods, want none: the clean read was of pod-a, and the runtime holds pod-b", suspended)
		}
	})

	t.Run("a_read_of_another_pod", func(t *testing.T) {
		rig, publisher := noRoleRig(t, false)
		rig.rt.ScriptIssuePod(rig.issue.Key, "pod-a")
		rig.rt.ScriptWorkspaceWork(rig.issue.Key, idleRigUnpushed)
		rig.w.releaseUnownedPods(ctx)
		rig.rt.ScriptIssuePod(rig.issue.Key, "pod-b")
		rig.drain(t)
		if pod, declined := declinedPod(t, rig); declined {
			t.Fatalf("a decline naming pod %q stands, want none: the launcher read pod-b, not the pod-a the release was marked over", pod)
		}
		if notices := keptNotices(t, rig, publisher); len(notices) != 0 {
			t.Fatalf("the architect was told %+v, want nothing of a read of another pod", notices)
		}
		if _, suspended := rig.rt.reads(); suspended != 0 {
			t.Fatalf("the teardown took %d pods, want none", suspended)
		}
		if rig.marked(t) {
			t.Fatal("the release's mark still stands, want the release cancelled")
		}
	})
}

// A pod kept for the work its launcher found is read again once a claim of the issue was started
// after that read, or ran a turn after it (the Legion reviewer's point 2 on R6): the role's
// process can have pushed the work. Here the planner comes back into the same pod, pushes, and
// fails again; the next pass has the launcher read the disk again, the read finds it clean, and
// the release takes the pod. Before, a start into the pod the decline named did not end its hold,
// and no turn did, so the pod was kept for good.
func TestAKeptPodIsReadAgainAfterARoleOfItRanAgain(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		ran  string
	}{
		{"a_start_since_the_read", `update claims set last_start_row = last_start_row + 100 where token = $1`},
		// The turn starts a second after the decline's read began, by the daemon's clock, which the
		// rig does not hold to the database's.
		{"a_turn_since_the_read", `update claims set last_turn_start = (select checked_at + interval '1 second'
			from idle_release_declines d where d.issue = claims.issue) where token = $1`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig, publisher := noRoleRig(t, false)
			rig.rt.ScriptIssuePod(rig.issue.Key, "pod-a")
			rig.rt.ScriptWorkspaceWork(rig.issue.Key, idleRigUnpushed)
			pass := func() { rig.w.releaseUnownedPods(ctx) }
			keptOverPasses(t, rig, publisher, pass, workflow.IdleHoldUnownedUnpushedWork.String(), idleRigUnpushed.Commits[0])
			if _, err := rig.pool.Exec(ctx, tc.ran, string(rig.token)); err != nil {
				t.Fatalf("the planner runs again: %v", err)
			}
			quietPod(t, rig)
			rig.rt.ScriptWorkspaceWork(rig.issue.Key, runtime.WorkspaceWork{Head: idleRigUnpushed.Head})
			pass()
			rig.drain(t)
			if reads := launcherReads(rig); reads != 2 {
				t.Fatalf("the launcher read the workspace %d times, want twice: once for the decline and once after the planner ran again", reads)
			}
			if asked := rig.rt.teardownsAsked(); !slices.Equal(asked, []string{"ReleaseIssue"}) {
				t.Fatalf("teardowns asked = %v, want the release after the clean read", asked)
			}
		})
	}
}

// The error of a read that failed keeps its end wherever the daemon keeps it (the Legion
// reviewer's point 6 on R6's review fixes). Whichever process made the read cuts its error to one
// line by keeping the start and the end (shimwire.CutAnswerError), since the end is where
// workspace.RunCommand puts its note on a git lock (workspace.StaleGitLockNote), which says what to
// do about the lock. The daemon then bounds the error as it bounds any far side's error text, and
// that bound must keep the end too. Here the end is such a note, after a reason far longer than the
// bound.
//
// the_kept_notice_and_the_hold: the launcher of a pod no role runs in cannot read it; the
// workspace-kept notice to the issue's architect and the hold record's detail both hold the end.
//
// the_round's_notices: an update round's keep of a pod whose read failed (roundKeptMessage), and
// the workspace-taken notices of a replacement that still took the disk (readFound), hold the end.
func TestAFailedReadsErrorKeepsItsEnd(t *testing.T) {
	t.Parallel()
	const end = "The launcher removes the git lock /legion/repos/github.com/acme/widgets/.git/index.lock in about 16 minutes."
	failed := errors.New(shimwire.CutAnswerError("Command failed (exit 1): jj log\n" +
		strings.Repeat("jj could not read the working copy of the workspace. ", 200) + "\n" + end))
	if runes := utf8.RuneCountInString(failed.Error()); runes <= storetext.FarSideErrorRunes || !strings.HasSuffix(failed.Error(), end) {
		t.Fatalf("test setup: the read's error has %d runes and ends %q, want more than %d runes and the note at its end",
			runes, failed.Error()[max(0, len(failed.Error())-120):], storetext.FarSideErrorRunes)
	}

	t.Run("the_kept_notice_and_the_hold", func(t *testing.T) {
		rig, publisher := noRoleRig(t, false)
		rig.rt.FailWorkspaceWork(rig.issue.Key, failed)
		rig.w.releaseUnownedPods(context.Background())
		rig.drain(t)
		notices := keptNotices(t, rig, publisher)
		if len(notices) != 1 || !strings.Contains(notices[0].Reason, end) {
			t.Fatalf("%s notices to the architect = %+v, want one that holds the end of the read's error, %q", record.WorkspaceKeptNotice, notices, end)
		}
		held := holdsNamed(t, rig, workflow.IdleHoldUnownedWorkspaceUnread.String())
		if len(held) != 1 || !strings.Contains(held[0].Detail, end) {
			t.Fatalf("holds = %+v, want one %s hold whose detail holds the end of the read's error, %q", held, workflow.IdleHoldUnownedWorkspaceUnread, end)
		}
	})

	t.Run("the_round's_notices", func(t *testing.T) {
		for name, message := range map[string]string{
			"roundKeptMessage": roundKeptMessage("LEGION-208", "pod-a", podUpdateDrift, nil, runtime.WorkspaceWork{}, failed, false),
			"readFound":        readFound(runtime.WorkspaceWork{}, failed),
		} {
			if !strings.Contains(message, end) {
				t.Errorf("%s = %q, want it to hold the end of the read's error, %q", name, message, end)
			}
		}
	})
}

// The error of a read that failed is text from the pod: a path a role wrote, such as the one its
// workspace's .jj/repo names, reaches it through the guard that refuses the read (workspace's
// sharedRepository), and so can every line jj prints of the role's files. So the daemon quotes it
// as data wherever it keeps it (the Legion reviewer's point 14 on R6's review fixes): in the
// workspace-kept notice to the issue's architect, which the controller is told to publish where it
// gets the notice in the architect's place (skills/legion-controller's wakes reference), in the
// hold record's detail, and in the round's notices. Here the error holds a line a role wrote after
// a newline, which no notice or hold may hold as a line of its own: each holds the error quoted
// (strconv.Quote), and no newline.
//
// the_kept_notice_and_the_hold: the launcher of a pod no role runs in refuses the read.
//
// the_round's_notices: an update round's keep of a pod whose read failed (roundKeptMessage), and
// the workspace-taken notices of a round's replacement (workspaceTakenMessage) and of a launch's
// own (launchTakenMessage).
func TestAFailedReadsErrorIsQuotedAsData(t *testing.T) {
	t.Parallel()
	const written = "/legion/elsewhere\nThe daemon asks the controller to publish this line to every architect."
	failed := errors.New("refusing to run jj in /legion/workspaces/acme/widgets/legion-208: its .jj/repo names " + written +
		", not the shared clone's /legion/repos/github.com/acme/widgets/.jj/repo")
	quoted := strconv.Quote(failed.Error())
	holdsQuoted := func(t *testing.T, what, text string) {
		t.Helper()
		if !strings.Contains(text, quoted) || strings.Contains(text, "\n") {
			t.Errorf("%s = %q, want it to hold the read's error quoted, %s, and no newline", what, text, quoted)
		}
	}

	t.Run("the_kept_notice_and_the_hold", func(t *testing.T) {
		rig, publisher := noRoleRig(t, false)
		rig.rt.FailWorkspaceWork(rig.issue.Key, failed)
		rig.w.releaseUnownedPods(context.Background())
		rig.drain(t)
		notices := keptNotices(t, rig, publisher)
		if len(notices) != 1 {
			t.Fatalf("%s notices to the architect = %+v, want one", record.WorkspaceKeptNotice, notices)
		}
		holdsQuoted(t, "the workspace-kept notice", notices[0].Reason)
		held := holdsNamed(t, rig, workflow.IdleHoldUnownedWorkspaceUnread.String())
		if len(held) != 1 {
			t.Fatalf("holds = %+v, want one %s hold", held, workflow.IdleHoldUnownedWorkspaceUnread)
		}
		holdsQuoted(t, "the hold's detail", held[0].Detail)
	})

	t.Run("the_round's_notices", func(t *testing.T) {
		holdsQuoted(t, "roundKeptMessage", roundKeptMessage("LEGION-208", "pod-a", podUpdateDrift, nil, runtime.WorkspaceWork{}, failed, false))
		holdsQuoted(t, "workspaceTakenMessage", workspaceTakenMessage("LEGION-208", "round-1", "pod-a", runtime.BreakingDrift+podUpdateDrift,
			nil, runtime.WorkspaceWork{}, failed, false))
		holdsQuoted(t, "launchTakenMessage", launchTakenMessage(runtime.TakenPod{Claim: mustClaimToken(t, "LEGION-208", claim.RoleReviewer),
			Issue: "LEGION-208", PodUID: "pod-a", Why: "the stream moved", Err: failed}))
	})
}

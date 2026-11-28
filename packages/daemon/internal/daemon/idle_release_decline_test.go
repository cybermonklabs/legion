package daemon

//ci:reads packages/pi-shared/src/workspace-recreated.ts TestAReleasedCleanPodsWakeTaskSaysWhatThePluginNoticeSays reads it under ../../..

import (
	"context"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// idleRigUnpushed is what the rig's shim reads in a workspace that holds one commit no branch or
// tag on origin holds: the working copy sits on it and holds no change of its own.
var idleRigUnpushed = runtime.WorkspaceWork{Head: "f2f134541b30dfa5160a5d1ce5e17b76c967c661", Count: 1,
	Commits: []string{"9b1c2d3e4f5a6b7c8d9e0f1a2b3c4d5e6f7a8b9c"}}

// idleRigPasses is how many idle passes a test runs where it asserts that a pass did nothing: each
// one a pass of the rule over the pod and a drain of the outbox after it, as the daemon's own idle
// tick and outbox runner make them.
const idleRigPasses = 6

// passes runs n idle passes over the rig's pod, each followed by a drain of the outbox.
func (r *idleRig) passes(t *testing.T, n int) {
	t.Helper()
	for range n {
		r.w.releaseIdlePods(context.Background())
		r.drain(t)
	}
}

// sequenceValue is the last id the sequence of table's id column handed out: an insert into that
// table takes the next one, so a value that does not move is a table nothing was inserted into,
// rows written and deleted again included.
func (r *idleRig) sequenceValue(t *testing.T, table string) int64 {
	t.Helper()
	var last *int64
	if err := r.pool.QueryRow(context.Background(), `select pg_sequence_last_value(pg_get_serial_sequence($1, 'id')::regclass)`,
		table).Scan(&last); err != nil {
		t.Fatalf("read the id sequence of %s: %v", table, err)
	}
	if last == nil {
		return 0
	}
	return *last
}

// roleMessages is every message the outbox published to the topic of the rig's role.
func roleMessages(publisher *outboxPublisher, token string) []string {
	var messages []string
	for _, published := range publisher.publishes() {
		if published.topic == notify.RoleTopicPrefix+token {
			messages = append(messages, published.message)
		}
	}
	return messages
}

// A release whose check declines the pod asks the pod once. The decline holds the pod in the idle
// view (pods.IdleDecline.Stands) until a role of the issue starts a turn or a process again, so the
// passes after it mark no release, write no outbox row, ask no shim and tell no role. Before it,
// each pass marked the release again, its check read the workspace again, and its decline wrote a
// hold and a message every time.
//
// no_turn: the first pass's release reads the workspace, finds one commit origin does not hold,
// and declines: one hold of the pod under unpushed_work and one message to the role the workspace
// was read through, the open phase's worker. The passes after it do nothing at all.
//
// turn_after_hold: then the role starts a turn after the decline, the turn the decline's own
// message starts in the role's session, which also makes the pod recorded-to and so not idle. Once
// the pod is idle again, the pass marks a release and its check reads the workspace again, once:
// the same work is still there, so the check declines again, keeps the decline's moment and tells
// nobody. No second message means no second turn, so the passes after that ask nothing more: the
// message, the turn and the read do not come back every idle period.
func TestADeclinedIdleReleaseAsksThePodOnceUntilATurn(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	// declined is a rig whose first pass has run and whose release the check has declined, with
	// what that pass did read back: the decline's own moment, and the publisher that holds the
	// message.
	declined := func(t *testing.T) (*idleRig, *outboxPublisher, time.Time) {
		t.Helper()
		rig := newIdleRig(t)
		publisher := &outboxPublisher{}
		rig.runner.notices = publisher
		rig.rt.ScriptWorkspaceWork(rig.issue.Key, idleRigUnpushed)

		rig.passes(t, 1)

		if read, suspended := rig.rt.reads(); len(read) != 1 || suspended != 0 {
			t.Fatalf("the first pass read the workspace %d times and took %d pods, want one read and no pod", len(read), suspended)
		}
		if rig.marked(t) {
			t.Fatal("the declined release left its mark behind")
		}
		var at time.Time
		var reason string
		if err := rig.pool.QueryRow(ctx, `select declined_at, reason from idle_release_declines where issue = $1`,
			rig.issue.Key).Scan(&at, &reason); err != nil || reason != pods.DeclineUnpushedWork {
			t.Fatalf("the recorded decline = %q, %v; want %s", reason, err, pods.DeclineUnpushedWork)
		}
		holds := rig.records(t).Holds
		if len(holds) != 1 || holds[0].Reason != workflow.IdleHoldUnpushedWork.String() {
			t.Fatalf("holds = %+v, want the one %s", holds, workflow.IdleHoldUnpushedWork)
		}
		messages := roleMessages(publisher, string(rig.token))
		if len(messages) != 1 || !strings.Contains(messages[0], idleRigUnpushed.Commits[0]) {
			t.Fatalf("messages to %s = %q, want one naming the commit %s", rig.token, messages, idleRigUnpushed.Commits[0])
		}
		if got := rig.machine.Claim(); got.State == supervise.StateSuspended {
			t.Fatalf("the claim = %s by %s, want it left running in the pod the decline kept", got.State, got.SuspendedBy)
		}
		return rig, publisher, at
	}

	t.Run("no_turn", func(t *testing.T) {
		rig, publisher, _ := declined(t)
		releases, rows := rig.sequenceValue(t, "issue_releases"), rig.sequenceValue(t, "outbox")
		left := rig.rows(t)

		rig.passes(t, idleRigPasses)

		if read, _ := rig.rt.reads(); len(read) != 1 {
			t.Fatalf("the workspace was read %d times across %d passes after the decline, want only the first pass's read", len(read), idleRigPasses+1)
		}
		if after := rig.sequenceValue(t, "issue_releases"); after != releases || rig.marked(t) {
			t.Fatalf("the passes after the decline marked a release (the mark sequence moved from %d to %d, marked now %t), want none",
				releases, after, rig.marked(t))
		}
		if after := rig.sequenceValue(t, "outbox"); after != rows {
			t.Fatalf("the passes after the decline queued outbox rows (the sequence moved from %d to %d), want none", rows, after)
		}
		if now := rig.rows(t); strings.Join(now, ",") != strings.Join(left, ",") {
			t.Fatalf("outbox rows after the passes = %v, want the %v the decline's pass left", now, left)
		}
		if messages := roleMessages(publisher, string(rig.token)); len(messages) != 1 {
			t.Fatalf("messages to %s after the passes = %d, want the decline's one", rig.token, len(messages))
		}
		holds := rig.records(t).Holds
		if len(holds) != 1 || holds[0].Reason != workflow.IdleHoldUnpushedWork.String() || holds[0].Passes < 2 {
			t.Fatalf("holds = %+v, want one %s streak that the passes after the decline counted on", holds, workflow.IdleHoldUnpushedWork)
		}
	})

	t.Run("turn_after_hold", func(t *testing.T) {
		rig, publisher, at := declined(t)
		rig.passes(t, 2)
		if read, _ := rig.rt.reads(); len(read) != 1 {
			t.Fatalf("the workspace was read %d times before the turn, want only the first pass's read", len(read))
		}

		// The role starts a turn a moment after the decline, as the message it was sent asks it
		// to. The claim's write is what makes the pod recorded-to, so the pod is not idle again
		// until the idle period has passed from there.
		turn := at.Add(time.Microsecond)
		if _, err := rig.pool.Exec(ctx, `update claims set last_turn_start = $2, updated_at = $2 where token = $1`,
			string(rig.token), turn); err != nil {
			t.Fatalf("start the role's turn: %v", err)
		}
		rig.passes(t, 2)
		if read, _ := rig.rt.reads(); len(read) != 1 {
			t.Fatalf("the workspace was read %d times while the pod was not idle, want only the first pass's read", len(read))
		}

		// The idle period passes from the turn, with the listener still reporting.
		later := time.Now().Add(rig.w.idlePeriod + 5*time.Minute)
		rig.w.now = func() time.Time { return later }
		if _, err := rig.pool.Exec(ctx, `update listener_reports set ready_at = $2 where project = $1`, "legion", later); err != nil {
			t.Fatalf("move the listener's report on: %v", err)
		}
		rig.passes(t, idleRigPasses)

		if read, _ := rig.rt.reads(); len(read) != 2 {
			t.Fatalf("the workspace was read %d times in all, want the first pass's read and exactly one more after the turn", len(read))
		}
		if rig.marked(t) {
			t.Fatal("the second release's decline left its mark behind")
		}
		var again, checked time.Time
		if err := rig.pool.QueryRow(ctx, `select declined_at, checked_at from idle_release_declines where issue = $1`,
			rig.issue.Key).Scan(&again, &checked); err != nil || !again.Equal(at) || !checked.After(turn) {
			t.Fatalf("the decline after the turn reads declined at %s and checked at %s (%v), want the first decline's moment %s kept and a check after the turn at %s",
				again, checked, err, at, turn)
		}
		if messages := roleMessages(publisher, string(rig.token)); len(messages) != 1 {
			t.Fatalf("messages to %s after the read that found the same work = %d, want the first decline's one and no second", rig.token, len(messages))
		}

		// No message started another turn, so the passes after the second read ask nothing more.
		releases, rows := rig.sequenceValue(t, "issue_releases"), rig.sequenceValue(t, "outbox")
		rig.passes(t, idleRigPasses)
		if read, _ := rig.rt.reads(); len(read) != 2 {
			t.Fatalf("the workspace was read %d times in all after the passes that followed the second read, want 2: the read came back with no turn", len(read))
		}
		if r, o := rig.sequenceValue(t, "issue_releases"), rig.sequenceValue(t, "outbox"); r != releases || o != rows {
			t.Fatalf("the passes after the second read moved the mark sequence %d to %d and the outbox sequence %d to %d, want neither moved",
				releases, r, rows, o)
		}
	})
}

// R5 of the release that keeps no saved workspace, its first test: a workspace with one commit
// that origin does not hold keeps its pod. The release's check reads the commit and declines
// before any role stops, so the pod, its disk and the workspace on it stay as they were: the
// runtime takes no pod down and stops no process, the claim keeps the process it was running in
// that pod, and the next turn of the role is a turn in that same pod, whose workspace still holds
// the commit. The role is told which commit holds the release.
func TestAnIdleReleaseDeclinesAWorkspaceWithOneUnpushedCommitAndKeepsThePod(t *testing.T) {
	t.Parallel()
	rig := newIdleRig(t)
	publisher := &outboxPublisher{}
	rig.runner.notices = publisher
	rig.rt.ScriptWorkspaceWork(rig.issue.Key, idleRigUnpushed)
	before := rig.machine.Claim()
	if before.Locator == nil {
		t.Fatal("the rig's implementer runs no process to keep")
	}
	sandbox, present := rig.rt.IssueSandbox(rig.issue.Key)
	if !present {
		t.Fatal("the rig's runtime holds no Sandbox for the issue")
	}
	launches := len(rig.rt.CallsOf("Spawn")) + len(rig.rt.CallsOf("Resume"))

	rig.passes(t, idleRigPasses)

	if asked := rig.rt.teardownsAsked(); len(asked) != 0 {
		t.Fatalf("teardowns asked = %v, want none: the pod holds a commit origin does not", asked)
	}
	if stops := rig.rt.CallsOf("Release"); len(stops) != 0 {
		t.Fatalf("the runtime stopped %d role processes, want none: the check declines before any role stops", len(stops))
	}
	if read, _ := rig.rt.reads(); len(read) != 1 {
		t.Fatalf("the workspace was read %d times, want once", len(read))
	}
	after := rig.machine.Claim()
	if after.State == supervise.StateSuspended {
		t.Fatalf("the claim = %s by %s, want it live in the pod it was in", after.State, after.SuspendedBy)
	}
	if after.Locator == nil || !reflect.DeepEqual(*after.Locator, *before.Locator) {
		t.Fatalf("the claim's process = %+v, want the %+v it had before the release: the role's next turn is in the same pod",
			after.Locator, before.Locator)
	}
	if now, present := rig.rt.IssueSandbox(rig.issue.Key); !present || now != sandbox {
		t.Fatalf("the issue's Sandbox = %q (present %t), want the %q it had: the pod and its disk stay", now, present, sandbox)
	}
	if relaunched := len(rig.rt.CallsOf("Spawn")) + len(rig.rt.CallsOf("Resume")); relaunched != launches {
		t.Fatalf("launches = %d, want the %d before the release: the role's process was never stopped, so nothing is launched again",
			relaunched, launches)
	}
	if records := rig.records(t); len(records.Releases) != 0 {
		t.Fatalf("releases recorded = %+v, want none", records.Releases)
	}
	messages := roleMessages(publisher, string(rig.token))
	if len(messages) != 1 || !strings.Contains(messages[0], idleRigUnpushed.Commits[0]) || !strings.Contains(messages[0], "legion/"+rig.issue.Key) {
		t.Fatalf("messages to %s = %q, want one naming the commit %s and the issue's branch", rig.token, messages, idleRigUnpushed.Commits[0])
	}
}

// A pod whose release the check declined for unpushed work can lose every live claim after that
// decline: the role's relaunches run out, or the daemon stops it. The pod then runs no role to read
// its disk through, and the release of a pod no claim owns reads the decline instead
// (pods.IdleDecline.HoldsUnpushedWork): the disk holds the work the check found, so the pass holds
// the pod as unowned_unpushed_work, records that hold, marks no release and asks for no teardown.
// A role started again in that same pod and stopped since can have pushed the work, so the
// release reads the disk again through the pod's launcher first (the Legion reviewer's point 2 on
// R6), and a read that finds the same work keeps the pod as before. Only a pod made again — the
// runtime answering another pod for the issue than the one the check read — takes the disk the
// decline spoke of, and the pass releases it.
func TestAnUnownedPodWhoseDeclineNamesUnpushedWorkIsKept(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rig := newIdleRig(t)
	rig.rt.ScriptIssuePod(rig.issue.Key, "pod-a")
	rig.rt.ScriptWorkspaceWork(rig.issue.Key, idleRigUnpushed)
	rig.passes(t, 1)
	var started int64
	var pod string
	if err := rig.pool.QueryRow(ctx, `select started_row, pod_uid from idle_release_declines where issue = $1 and reason = $2`,
		rig.issue.Key, pods.DeclineUnpushedWork).Scan(&started, &pod); err != nil || pod != "pod-a" {
		t.Fatalf("the decline = start row %d in pod %q (%v), want one for unpushed work read in pod-a", started, pod, err)
	}

	// The implementer's relaunches run out after the decline, and the pod stands past the quiet
	// period with no live claim.
	rig.stoppedImplementer(t, false)
	unownedPasses := func(n int) {
		t.Helper()
		for range n {
			rig.w.releaseUnownedPods(ctx)
			rig.drain(t)
		}
	}
	releases := rig.sequenceValue(t, "issue_releases")
	unownedPasses(idleRigPasses)
	if asked := rig.rt.teardownsAsked(); len(asked) != 0 {
		t.Fatalf("teardowns asked = %v, want none: the pod's disk holds work origin does not", asked)
	}
	if after := rig.sequenceValue(t, "issue_releases"); after != releases || rig.marked(t) {
		t.Fatalf("the unowned passes marked a release (the mark sequence moved from %d to %d, marked now %t), want none",
			releases, after, rig.marked(t))
	}
	if read, _ := rig.rt.reads(); len(read) != 1 {
		t.Fatalf("the workspace was read %d times, want only the declined release's read: no role is left to read through", len(read))
	}
	if held := holdsOf(t, rig, workflow.IdleHoldUnownedUnpushedWork); len(held) != 1 || held[0].Passes < 2 {
		t.Fatalf("holds = %+v, want one %s streak the unowned passes counted on", rig.records(t).Holds, workflow.IdleHoldUnownedUnpushedWork)
	}

	// A start of the implementer after the decline, whose relaunches ran out again, in the same
	// pod: its process can have pushed the work, so the pass has the launcher read the disk again,
	// once, and the read finds the same work there, which keeps the pod.
	if _, err := rig.pool.Exec(ctx, `update claims set last_start_row = $2, updated_at = now() - interval '30 minutes'
		where token = $1`, string(rig.token), started+1); err != nil {
		t.Fatalf("record a start of the implementer after the decline: %v", err)
	}
	unownedPasses(idleRigPasses)
	if reads := len(rig.rt.CallsOf("PodWorkspaceWork")); reads != 1 {
		t.Fatalf("the launcher read the workspace %d times over the passes after a start in the same pod, want once: the start ends the decline's hold, and the read's own decline holds the pod again",
			reads)
	}
	if asked := rig.rt.teardownsAsked(); len(asked) != 0 {
		t.Fatalf("teardowns asked after a start in the same pod = %v, want none: the read found the work still there", asked)
	}

	// A relaunch built pod-b in pod-a's place: the disk, and the work on it, went with pod-a. The
	// release reads pod-b's disk through its launcher first (R6), which holds nothing origin lacks.
	rig.rt.ScriptIssuePod(rig.issue.Key, "pod-b")
	rig.rt.ScriptWorkspaceWork(rig.issue.Key, runtime.WorkspaceWork{Head: idleRigUnpushed.Head})
	if queued := rig.queuedBy(t, func() { rig.w.releaseUnownedPods(ctx) }); !slices.Equal(queued, []string{"issue_bundle", "issue_release"}) {
		t.Fatalf("the pass after the pod was made again queued %v, want the unowned release's launcher check and its teardown", queued)
	}
	rig.drain(t)
	if asked := rig.rt.teardownsAsked(); !slices.Equal(asked, []string{"ReleaseIssue"}) {
		t.Fatalf("teardowns asked = %v, want the release of the pod no claim owns", asked)
	}
}

// A clean read is the daemon's newest word of the workspace, and the work a decline on record
// named is gone from it: the check deletes that decline before it goes on (store.ForgetIdleDecline).
// Kept, the decline would hold the pod for work that is no longer there, through the release of a
// pod no claim owns or a later close, once its roles have stopped. Each subtest stands a decline
// that no longer holds the pod — a turn of the live role after its last check — so the pass asks
// the workspace again, the read is clean, and after the drain no decline of the issue remains.
//
// idle_release: the idle release's check reads the workspace and the release goes on.
//
// left_pod: the close of a parked child whose planner an operator's resume brought back reads the
// workspace through the planner, and the close goes on.
func TestACleanReadForgetsTheDecline(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	// endedDecline records a decline of the rig's issue for one commit, read two hours ago in a pod
	// the runtime could not name, and a turn of the rig's role an hour ago, after that read.
	endedDecline := func(t *testing.T, rig *idleRig, role claim.Role) {
		t.Helper()
		if _, err := rig.pool.Exec(ctx, `insert into idle_release_declines
			(project, issue, declined_at, checked_at, role, reason, head, commits, changed, started_row, pod_uid)
			values ('legion', $1, now() - interval '2 hours', now() - interval '2 hours', $2, $3, $4, 1, false, 0, '')`,
			rig.issue.Key, string(role), pods.DeclineUnpushedWork, idleRigUnpushed.Head); err != nil {
			t.Fatalf("record the decline: %v", err)
		}
		if _, err := rig.pool.Exec(ctx, `update claims set last_turn_start = now() - interval '1 hour' where token = $1`,
			string(rig.token)); err != nil {
			t.Fatalf("record the role's turn after the decline: %v", err)
		}
	}

	t.Run("idle_release", func(t *testing.T) {
		rig := newIdleRig(t)
		endedDecline(t, rig, claim.RoleImplementer)
		rig.passes(t, 1)
		if read, suspended := rig.rt.reads(); len(read) != 1 || suspended != 1 {
			t.Fatalf("the pass read the workspace %d times and took %d pods, want one clean read and the release", len(read), suspended)
		}
		if rig.declined(t) {
			t.Fatal("the decline stands after a read that found nothing origin does not hold")
		}
	})

	t.Run("left_pod", func(t *testing.T) {
		rig := newLeftPodRig(t, false)
		parkChild(t, rig)
		rig.drain(t)
		leftPodGone(rig)
		resumeAsOperator(t, rig)
		endedDecline(t, rig, claim.RolePlanner)
		idlePasses(rig)
		rig.drain(t)
		if read, _ := rig.rt.reads(); !slices.Equal(read, []claim.Role{claim.RolePlanner}) {
			t.Fatalf("the workspace was read through %v, want one clean read through the planner", read)
		}
		if got := rig.machine.Claim(); got.State != supervise.StateSuspended {
			t.Fatalf("the planner after the clean read = %s, want it stopped by the close", got.State)
		}
		if rig.declined(t) {
			t.Fatal("the decline stands after a read that found nothing origin does not hold")
		}
	})
}

// readClockRuntime is the idle rig's runtime with a hook run as the workspace read begins, before
// the fake answers it.
type readClockRuntime struct {
	*releaseRuntime
	reading func()
}

func (r *readClockRuntime) WorkspaceWork(ctx context.Context, issue string, role claim.Role) (runtime.WorkspaceWork, error) {
	r.reading()
	return r.releaseRuntime.WorkspaceWork(ctx, issue, role)
}

// A decline is stamped with the moment its read began, not the moment the read answered: a turn
// that started while the read ran may have pushed the work the read found, and it is a turn after
// the decline, which ends it, only where the decline's moment is the read's start
// (pods.IdleDecline.Stands). The runner's clock reads t1 until the read begins and t2, ten
// seconds later, from then on; the decline reads t1.
func TestADeclineIsStampedWithTheMomentItsReadBegan(t *testing.T) {
	t.Parallel()
	rig := newIdleRig(t)
	rig.rt.ScriptWorkspaceWork(rig.issue.Key, idleRigUnpushed)
	t1 := time.Now().Add(time.Minute).UTC().Truncate(time.Microsecond)
	t2 := t1.Add(10 * time.Second)
	var mu sync.Mutex
	began := false
	rig.runner.now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		if began {
			return t2
		}
		return t1
	}
	rig.sup.deps.Runtime = &readClockRuntime{releaseRuntime: rig.rt, reading: func() {
		mu.Lock()
		defer mu.Unlock()
		began = true
	}}

	rig.passes(t, 1)

	var declined, checked time.Time
	if err := rig.pool.QueryRow(context.Background(), `select declined_at, checked_at from idle_release_declines where issue = $1`,
		rig.issue.Key).Scan(&declined, &checked); err != nil {
		t.Fatalf("read the decline: %v", err)
	}
	if !declined.Equal(t1) || !checked.Equal(t1) {
		t.Fatalf("the decline reads declined at %s and checked at %s, want both at %s, when the read began (it answered at %s)",
			declined.UTC(), checked.UTC(), t1, t2)
	}
}

// A decline holds the pod its check read, and that pod's disk is where the work it found lives. A
// relaunch that builds the issue's pod again takes that disk, and the work with it, with no turn
// and no start row a pass could read: the runtime answers another pod for the issue
// (runtime.IssuePodReader), and the decline ends there (pods.IdleDecline.Stands). The next pass
// marks the release again and its check reads the new pod once; that read finds nothing, and the
// release takes the pod.
func TestADeclineInAReplacedPodEndsWithThePod(t *testing.T) {
	t.Parallel()
	rig := newIdleRig(t)
	rig.rt.ScriptIssuePod(rig.issue.Key, "pod-a")
	rig.rt.ScriptWorkspaceWork(rig.issue.Key, idleRigUnpushed)
	rig.passes(t, 1+idleRigPasses)
	if read, suspended := rig.rt.reads(); len(read) != 1 || suspended != 0 {
		t.Fatalf("pod-a was read %d times and %d pods were taken, want one read and the decline keeping the pod", len(read), suspended)
	}
	if !rig.declined(t) {
		t.Fatal("the read of pod-a's unpushed commit recorded no decline")
	}

	// The relaunch built pod-b in pod-a's place: its workspace was made again from origin.
	rig.rt.ScriptIssuePod(rig.issue.Key, "pod-b")
	rig.rt.ScriptWorkspaceWork(rig.issue.Key, runtime.WorkspaceWork{})
	rig.passes(t, 1)
	read, suspended := rig.rt.reads()
	if len(read) != 2 || suspended != 1 {
		t.Fatalf("after the pod was replaced the workspace was read %d times in all and %d pods were taken, want one more read and the release of pod-b",
			len(read), suspended)
	}
	if asked := rig.rt.teardownsAsked(); !slices.Equal(asked, []string{"ReleaseIssue"}) {
		t.Fatalf("teardowns asked = %v, want the release's", asked)
	}
	if got := rig.machine.Claim(); got.State != supervise.StateSuspended || got.SuspendedBy != supervise.SuspendedByReleased {
		t.Fatalf("the implementer = %s by %s, want it suspended by the release", got.State, got.SuspendedBy)
	}
}

// R5 of the release that keeps no saved workspace, its second test: the wake task of a role whose
// clean pod was released says what the role's plugin notice says. The release's check reads no
// work, its role stop and its teardown take the pod, and a message published to the role's topic
// wakes it. The wake's start row carries the task, which tells the role of its workspace in the
// words workflow.WorkspaceAfterRelease holds. That text is the data file the plugin's test reads
// (workflow/workspace-after-release.txt) byte for byte, and the plugin's own notice
// (packages/pi-shared/src/workspace-recreated.ts) carries its sentence about what the release
// takes. Neither says the workspace's unpushed work was kept.
func TestAReleasedCleanPodsWakeTaskSaysWhatThePluginNoticeSays(t *testing.T) {
	t.Parallel()
	rig := newIdleRig(t)
	ctx := context.Background()

	rig.passes(t, 1)

	if read, suspended := rig.rt.reads(); len(read) != 1 || suspended != 1 {
		t.Fatalf("the release read the workspace %d times and took %d pods, want one read and the pod", len(read), suspended)
	}
	if got := rig.machine.Claim(); !got.Released() {
		t.Fatalf("the claim after the release = %s by %s, want it released", got.State, got.SuspendedBy)
	}
	tables, err := rig.st.PodRecordsOf(rig.issue.Project)
	if err != nil {
		t.Fatalf("read the pod tables: %v", err)
	}
	engine := workflow.New(rig.issues, workflow.Config{Project: rig.issue.Project, Repositories: testRepositories,
		Steps: config.DesignGateRootIssues.Steps(), Pods: tables}, quietLogger())
	if result, err := intake.ApplyFact(ctx, rig.pool, "wake", "role-message-1", intake.ListenerWake{Kind: intake.ListenerWakeRole,
		Role: string(rig.token), At: time.Now().UTC(), Report: "role-message-1"}, engine); err != nil || result.Refusal != nil {
		t.Fatalf("apply the listener's report of a role message = %#v, %v", result, err)
	}
	var task string
	if err := rig.pool.QueryRow(ctx, `select payload->>'task' from outbox where issue = $1 and kind = 'supervise'
		and payload->>'op' = 'start' and payload->>'role' = 'implementer' order by id desc limit 1`, rig.issue.Key).Scan(&task); err != nil {
		t.Fatalf("read the wake's start row: %v", err)
	}
	preamble := "Your issue's pod had been released while it was idle, and this wake brought it back. " +
		workflow.WorkspaceAfterRelease + " Your session is as you left it. "
	if !strings.HasPrefix(task, preamble) {
		t.Fatalf("the wake's task = %q, want it to open with %q", task, preamble)
	}
	for _, kept := range []string{"any work you had not pushed", "saved", "restored", "put back"} {
		if strings.Contains(task, kept) {
			t.Fatalf("the wake's task = %q, which says %q: the daemon keeps none of the workspace", task, kept)
		}
	}

	data, err := os.ReadFile("../workflow/workspace-after-release.txt")
	if err != nil {
		t.Fatalf("read the shared text: %v", err)
	}
	if string(data) != workflow.WorkspaceAfterRelease+"\n" {
		t.Fatalf("workspace-after-release.txt = %q, want the wake task's text %q and one newline", data, workflow.WorkspaceAfterRelease)
	}
	notice, err := os.ReadFile("../../../pi-shared/src/workspace-recreated.ts")
	if err != nil {
		t.Fatalf("read the plugin's notice: %v", err)
	}
	// The notice spells the text across string pieces; its first and last sentences each sit in one.
	for _, said := range []string{
		"such work stays in the pod.",
		"A commit that no working copy and no bookmark reaches, such as one left on a side head with jj new, is not kept.",
	} {
		if !strings.Contains(workflow.WorkspaceAfterRelease, said) || !strings.Contains(string(notice), said) {
			t.Fatalf("the wake task's text and the plugin's notice do not both say %q", said)
		}
	}
}

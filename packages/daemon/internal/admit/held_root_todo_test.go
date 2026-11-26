package admit

import (
	"context"
	"io"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// The daemon applies one Dispatch event through both handlers, the engine first and admission
// after it, so a todo on a held root is read here as the daemon reads it. The engine releases the
// hold and records the event with the recorded status kept (workflow's releaseHeldRoot), and
// admission, reaching that same event, stops at its sequence fence: it writes no `status: todo`
// over the release, which is the pair — status todo beside phase held — this issue measures.
//
// The re-admissions a todo does make stand beside it unchanged, since the release's own test is
// the complement of readmittable: a lingering root starts a new generation, and a held child
// comes back into its tree at phase admitted on a generation of its own, which is the non-goal
// the spec protects.
//
// A held child a hand-over carries out of a finished tree becomes a root of its own, still held
// (admission's readmit with carry), and waits in the line at todo with the tree it left recorded
// (record.Issue.HandedOverFrom), which is what makes it a queued root (record.QueuedRoot). Its
// promotion opens its tree, writes it in_progress, clears that mark and tells its architect of the
// hold (startMidPhaseMembers, workflow.HeldNotice). From then on it is a held root like any other:
// once Dispatch shows the in_progress the promotion wrote, a person's todo releases it into the
// phase the hold is from. While it waits in the line it reads todo already, so a todo event is
// no status change and releases nothing, and the promotion that a freed slot makes is what starts
// it. No writer leaves a root held at todo in a tree that opened: the record reads todo only while
// the carried root waits, and its promotion is the write that opens the tree.
func TestATodoOnAHeldRootReleasesTheHold(t *testing.T) {
	const root, child = "LEGION-208", "LEGION-209"
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	newEngine := func() *workflow.Engine {
		return workflow.New(record.NewStore(), workflow.Config{Project: testProject, Repositories: testRepositories,
			Steps: config.DesignGateRootIssues.Steps(), Linger: time.Hour, Clock: func() time.Time { return fixedNow }}, quiet)
	}
	todo := func(key, parent, rank string, seq int64, handedOver bool) intake.DispatchIssue {
		return intake.DispatchIssue{Key: key, ResolvedRepo: "acme/widgets", Seq: seq, Type: "issue.updated", Status: "todo",
			Title: key, Parent: parent, Rank: rank, HandedOver: handedOver}
	}

	t.Run("the held root of a running tree is released, and admission writes no todo over it", func(t *testing.T) {
		pool := migratedPool(t)
		seedSlotted(t, pool, root, "A")
		held := issue(t, pool, root)
		held.Phase, held.Hold = phase.Held, &record.Hold{From: phase.Implementing}
		held.Status, held.DispatchStatus, held.LastDispatchSeq = "in_progress", "in_progress", 4
		putIssue(t, pool, held)

		apply(t, pool, newAdmission(t, 1, quiet), "todo", todo(root, "", "A", 5, handed), newEngine())

		released := issue(t, pool, root)
		if released.Phase != phase.Implementing || released.Hold != nil || released.Generation != 1 {
			t.Fatalf("the root after the todo = %#v, want it back in implementing, its hold cleared and its generation kept", released)
		}
		if released.Status != "in_progress" || released.DispatchStatus != "todo" || released.LastDispatchSeq != 5 {
			t.Fatalf("the root after the todo = %#v, want the status of its phase kept, Dispatch shown at todo and the event's sequence 5 recorded: "+
				"admission must write no todo over the release", released)
		}
		writes, starts := queuedWrites(t, pool), queuedStarts(t, pool)
		want := record.OrdinaryStatusWrite("in_progress", "todo", "")
		if len(writes) != 1 || writes[0] != want {
			t.Fatalf("status writes = %+v, want the one %+v: the status of the phase the issue ends in", writes, want)
		}
		if len(starts) != 1 || starts[0].Role != claim.RoleImplementer || starts[0].Phase != phase.Implementing ||
			!strings.Contains(starts[0].Task, "Reason: the issue was set to todo while its phase was held.") {
			t.Fatalf("start rows = %+v, want one implementer start of implementing whose task carries the daemon's own reason", starts)
		}
		assertSlots(t, pool, []record.Slot{{Issue: root, Index: 0, AdmittedAt: fixedNow}})
	})

	t.Run("a lingering root keeps its re-admission", func(t *testing.T) {
		pool := migratedPool(t)
		until := fixedNow.Add(time.Hour)
		putIssue(t, pool, record.Issue{Key: root, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root,
			Project: testProject, Title: root, Phase: phase.Done, Generation: 3, Status: "done", DispatchStatus: "done", Rank: "A",
			HandedOver: true, LingerUntil: &until, LastDispatchSeq: 4})

		apply(t, pool, newAdmission(t, 1, quiet), "todo", todo(root, "", "A", 5, handed), newEngine())

		readmitted := issue(t, pool, root)
		if readmitted.Phase != phase.Admitted || readmitted.Generation != 4 || readmitted.LingerUntil != nil || readmitted.Hold != nil {
			t.Fatalf("the lingering root after the todo = %#v, want it re-admitted at generation 4 with no linger", readmitted)
		}
		assertSlots(t, pool, []record.Slot{{Issue: root, Index: 0, AdmittedAt: fixedNow}})
	})

	t.Run("a held child keeps its re-entry", func(t *testing.T) {
		pool := migratedPool(t)
		seedSlotted(t, pool, root, "A")
		parent := root
		putIssue(t, pool, record.Issue{Key: child, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root, Parent: &parent,
			Project: testProject, Title: child, Phase: phase.Held, Hold: &record.Hold{From: phase.Implementing}, Generation: 1,
			Status: "in_progress", DispatchStatus: "in_progress", Rank: "B", LastDispatchSeq: 4})

		apply(t, pool, newAdmission(t, 1, quiet), "todo", todo(child, root, "B", 5, false), newEngine())

		reentered := issue(t, pool, child)
		if reentered.Phase != phase.Admitted || reentered.Generation != 2 || reentered.Hold != nil || reentered.Status != "todo" {
			t.Fatalf("the held child after the todo = %#v, want it back in its tree at phase admitted on generation 2", reentered)
		}
		assertSlots(t, pool, []record.Slot{{Issue: root, Index: 0, AdmittedAt: fixedNow}})
	})

	// handOverHeldChild is a hand-over of child, held from testing, out of root's finished tree,
	// seeded as the store holds such a child (TestACarriedRootResumesFromTheApprovedHeadAndTheHold),
	// and the daemon's own todo on it that consumes the hand-over. It answers the hold the child
	// carries.
	handOverHeldChild := func(t *testing.T, pool *pgxpool.Pool, admission *Admission, engine *workflow.Engine) record.Hold {
		t.Helper()
		until := fixedNow.Add(time.Hour)
		parent := root
		hold := record.Hold{From: phase.Testing, Reason: record.HoldEscalated}
		inTx(t, pool, func(tx pgx.Tx) {
			records := record.NewStore()
			for _, put := range []error{
				records.PutIssue(context.Background(), tx, record.Issue{Key: root, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root,
					Project: testProject, Title: root, Phase: phase.Done, Generation: 1, Status: "done", LingerUntil: &until}),
				records.PutIssue(context.Background(), tx, record.Issue{Key: child, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root,
					Project: testProject, Title: child, Parent: &parent, Phase: phase.Held, Hold: &hold, Generation: 1, Status: "testing", Rank: "B", LastDispatchSeq: 1}),
				records.PutHandover(context.Background(), tx, record.Handover{Issue: child, Tree: root, Generation: 1}),
			} {
				if put != nil {
					t.Fatalf("seed: %v", put)
				}
			}
		})
		apply(t, pool, admission, "handover", intake.DispatchIssue{Key: child, ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated", Status: "todo",
			Title: child, Parent: root, Rank: "B", HandedOver: true, DaemonActor: true}, engine)
		return hold
	}
	// childEvent is a Dispatch event of the carried child at seq, written by a person, or by the
	// daemon where daemon says so.
	childEvent := func(status string, seq int64, daemon bool) intake.DispatchIssue {
		return intake.DispatchIssue{Key: child, ResolvedRepo: "acme/widgets", Seq: seq, Type: "issue.updated", Status: status,
			Title: child, Parent: root, Rank: "B", HandedOver: true, DaemonActor: daemon}
	}
	// assertPromoted fails unless the carried child stands as its promotion leaves it: a root of its
	// own at generation 2, held as the hold it carried, its record in_progress and the hand-over mark
	// gone, one status write of in_progress over the todo Dispatch shows, one held notice for its
	// architect, and no phase worker started.
	assertPromoted := func(t *testing.T, pool *pgxpool.Pool, hold record.Hold) {
		t.Helper()
		promoted := issue(t, pool, child)
		if promoted.Tree != child || promoted.Phase != phase.Held || promoted.Hold == nil || *promoted.Hold != hold || promoted.Generation != 2 ||
			promoted.Status != "in_progress" || promoted.HandedOverFrom != "" {
			t.Fatalf("the carried child after its promotion = %#v, want a root of its own at generation 2, held %+v, in_progress, no hand-over mark", promoted, hold)
		}
		if writes := writesOf(t, pool, child); !reflect.DeepEqual(writes, []record.StatusWrite{record.OrdinaryStatusWrite("in_progress", "todo", "")}) {
			t.Fatalf("status writes of %s after its promotion = %+v, want the one in_progress over todo", child, writes)
		}
		want := []record.Notice{{Kind: "held", Role: claim.RoleTester, Phase: phase.Testing, Reason: string(record.HoldEscalated)}}
		if held := heldNoticesOf(t, pool, child); !reflect.DeepEqual(held, want) {
			t.Fatalf("held notices of %s after its promotion = %+v, want %+v", child, held, want)
		}
		if starts := workerStartsOf(t, pool, child); len(starts) != 0 {
			t.Fatalf("phase worker starts of %s after its promotion = %+v, want none: a held root starts no worker", child, starts)
		}
	}

	t.Run("a held root a hand-over carries is released by a person's todo once its tree opened", func(t *testing.T) {
		pool := migratedPool(t)
		admission, engine := newAdmission(t, 1, quiet), newEngine()
		hold := handOverHeldChild(t, pool, admission, engine)
		assertPromoted(t, pool, hold)
		assertSlots(t, pool, []record.Slot{{Issue: child, Index: 0, AdmittedAt: fixedNow}})

		// Dispatch's event of the in_progress the promotion wrote, then a person's todo.
		apply(t, pool, admission, "promoted-in-progress", childEvent("in_progress", 3, true), engine)
		if shown := issue(t, pool, child); shown.Phase != phase.Held || shown.DispatchStatus != "in_progress" {
			t.Fatalf("the carried child after Dispatch showed its in_progress = %#v, want it held with Dispatch shown in_progress", shown)
		}
		apply(t, pool, admission, "todo", childEvent("todo", 4, false), engine)

		released := issue(t, pool, child)
		if released.Phase != hold.From || released.Hold != nil || released.Generation != 2 || released.DispatchStatus != "todo" || released.LastDispatchSeq != 4 {
			t.Fatalf("the carried child after a person's todo = %#v, want it released into %s at generation 2, Dispatch shown at todo, sequence 4", released, hold.From)
		}
		wantWrites := []record.StatusWrite{record.OrdinaryStatusWrite("in_progress", "todo", ""), record.OrdinaryStatusWrite("testing", "todo", "")}
		if writes := writesOf(t, pool, child); !reflect.DeepEqual(writes, wantWrites) {
			t.Fatalf("status writes of %s after the release = %+v, want %+v: the release writes the status of the phase it ends in", child, writes, wantWrites)
		}
		if starts := workerStartsOf(t, pool, child); len(starts) != 1 || starts[0].Role != claim.RoleTester || starts[0].Phase != phase.Testing {
			t.Fatalf("phase worker starts of %s after the release = %+v, want one tester start of testing", child, starts)
		}
		assertSlots(t, pool, []record.Slot{{Issue: child, Index: 0, AdmittedAt: fixedNow}})
	})

	t.Run("a carried held root that waits for a slot is released by no todo, and a freed slot promotes it", func(t *testing.T) {
		const other = "LEGION-9"
		pool := migratedPool(t)
		seedSlotted(t, pool, other, "Z")
		admission, engine := newAdmission(t, 1, quiet), newEngine()
		hold := handOverHeldChild(t, pool, admission, engine)
		assertQueued := func(t *testing.T, when string, seq int64) {
			t.Helper()
			queued := issue(t, pool, child)
			if queued.Tree != child || queued.Phase != phase.Held || queued.Hold == nil || *queued.Hold != hold || queued.Generation != 2 ||
				queued.Status != "todo" || queued.DispatchStatus != "todo" || queued.HandedOverFrom != root || queued.LastDispatchSeq != seq {
				t.Fatalf("the carried child %s = %#v, want it queued: a root of its own at generation 2, held %+v, todo, handed over from %s, sequence %d",
					when, queued, hold, root, seq)
			}
			if writes, held, starts := writesOf(t, pool, child), heldNoticesOf(t, pool, child), workerStartsOf(t, pool, child); len(writes) != 0 || len(held) != 0 || len(starts) != 0 {
				t.Fatalf("the carried child %s queued writes %+v, held notices %+v and starts %+v, want none", when, writes, held, starts)
			}
			assertSlots(t, pool, []record.Slot{{Issue: other, Index: 0, AdmittedAt: fixedNow}})
		}
		assertQueued(t, "after the hand-over under a full cap", 2)

		apply(t, pool, admission, "todo", childEvent("todo", 3, false), engine)
		assertQueued(t, "after a todo while it waits", 3)

		// A person closes the other tree, which frees its slot, and the same pass promotes the child.
		apply(t, pool, admission, "other-done", intake.DispatchIssue{Key: other, ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated", Status: "done",
			Title: other, Rank: "Z", HandedOver: true}, engine)
		assertPromoted(t, pool, hold)
		assertSlots(t, pool, []record.Slot{{Issue: child, Index: 0, AdmittedAt: fixedNow}})
	})

	// Before its promotion a person moves the carried root to in_progress, then back to todo. The
	// in_progress takes it out of the line (record.QueuedRoot needs todo), and admission's pass then
	// finds a waiting root at a status only a running tree has, in no tree that runs: it leaves the
	// workflow as a root moved to backlog does (releaseUnslotted, workflow's LeaveUnslotted), done and
	// lingering, with nothing started and nothing told to the controller, which reads the root from
	// the state (#4065). The person's todo with the label then queues it again (readmit), so it
	// never reaches the release of a hold (workflow's releaseHeldRoot) and no worker starts in a
	// tree that has not opened. The leave already set the phase to done and cleared the hold, and a
	// person's todo is no hand-over, so it is a readmit with no carry: a new generation at admitted,
	// holding nothing the hand-over carried.
	t.Run("a carried held root a person moves out of the line leaves the workflow, and its todo starts no worker", func(t *testing.T) {
		const other = "LEGION-9"
		pool := migratedPool(t)
		seedSlotted(t, pool, other, "Z")
		admission, engine := newAdmission(t, 1, quiet), newEngine()
		handOverHeldChild(t, pool, admission, engine)

		toldOf := func() []record.ControllerNotice {
			var told []record.ControllerNotice
			for _, queued := range effects(t, pool) {
				if notice, ok := queued.payload.(record.ControllerNotice); ok && queued.issue == child {
					told = append(told, notice)
				}
			}
			return told
		}
		before := toldOf()
		apply(t, pool, admission, "in-progress", childEvent("in_progress", 3, false), engine)
		if left := issue(t, pool, child); left.Phase != phase.Done || left.LingerUntil == nil {
			t.Errorf("the carried child after a person's in_progress = %#v, want it out of the workflow, done and lingering", left)
		}
		if told := toldOf(); len(told) != len(before) {
			t.Errorf("controller notices of %s after a person's in_progress = %+v, want the %d from before it: the controller reads the root from the state", child, told, len(before))
		}
		if starts := workerStartsOf(t, pool, child); len(starts) != 0 {
			t.Errorf("phase worker starts of %s after a person's in_progress = %+v, want none", child, starts)
		}

		apply(t, pool, admission, "todo", childEvent("todo", 4, false), engine)
		if back := issue(t, pool, child); back.Phase != phase.Admitted || back.Hold != nil || back.Generation != 3 || back.Status != "todo" || back.LingerUntil != nil {
			t.Errorf("the carried child after a person's todo = %#v, want it re-admitted with no carry: phase admitted, no hold, generation 3, todo and lingering no more", back)
		}
		if line := waitingLine(t, pool, testProject, config.DesignGateRootIssues.Steps()); !slices.Contains(line, child) {
			t.Errorf("the waiting line after a person's todo = %v, want %s queued in it again", line, child)
		}
		if starts := workerStartsOf(t, pool, child); len(starts) != 0 {
			t.Errorf("phase worker starts of %s, whose tree never opened = %+v, want none", child, starts)
		}
		assertSlots(t, pool, []record.Slot{{Issue: other, Index: 0, AdmittedAt: fixedNow}})
	})
}

// writesOf is every Dispatch status write queued for issue, oldest first.
func writesOf(t *testing.T, pool *pgxpool.Pool, issue string) []record.StatusWrite {
	t.Helper()
	var writes []record.StatusWrite
	for _, queued := range effects(t, pool) {
		if write, ok := queued.payload.(record.StatusWrite); ok && queued.issue == issue {
			writes = append(writes, write)
		}
	}
	return writes
}

// heldNoticesOf is every held notice queued for issue's architect, oldest first.
func heldNoticesOf(t *testing.T, pool *pgxpool.Pool, issue string) []record.Notice {
	t.Helper()
	var held []record.Notice
	for _, queued := range effects(t, pool) {
		if notice, ok := queued.payload.(record.Notice); ok && queued.issue == issue && notice.Kind == "held" {
			held = append(held, notice)
		}
	}
	return held
}

// workerStartsOf is every phase worker's start queued for issue, oldest first: a start that carries
// a task, of a role other than the architect.
func workerStartsOf(t *testing.T, pool *pgxpool.Pool, issue string) []record.SuperviseRequest {
	t.Helper()
	var starts []record.SuperviseRequest
	for _, queued := range effects(t, pool) {
		if start, ok := queued.payload.(record.SuperviseRequest); ok && queued.issue == issue && string(start.Op) == "start" && start.Role != claim.RoleArchitect {
			starts = append(starts, start)
		}
	}
	return starts
}

// queuedWrites is every Dispatch status write queued, oldest first.
func queuedWrites(t *testing.T, pool *pgxpool.Pool) []record.StatusWrite {
	t.Helper()
	var writes []record.StatusWrite
	for _, queued := range effects(t, pool) {
		if write, ok := queued.payload.(record.StatusWrite); ok {
			writes = append(writes, write)
		}
	}
	return writes
}

// queuedStarts is every phase worker's start queued, oldest first; a start that carries no task
// is none of them (effects).
func queuedStarts(t *testing.T, pool *pgxpool.Pool) []record.SuperviseRequest {
	t.Helper()
	var starts []record.SuperviseRequest
	for _, queued := range effects(t, pool) {
		if start, ok := queued.payload.(record.SuperviseRequest); ok && string(start.Op) == "start" {
			starts = append(starts, start)
		}
	}
	return starts
}

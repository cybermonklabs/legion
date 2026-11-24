package admit

import (
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

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
// A todo also releases a held child in work, and opens a new run of a closed root.
func TestATodoOnAHeldRootReleasesTheHold(t *testing.T) {
	const root, child = "LEGION-208", "LEGION-209"
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	newEngine := func() *workflow.Engine {
		return workflow.New(record.NewStore(), workflow.Config{Project: testProject, Repositories: testRepositories,
			Steps: config.DesignGateRootIssues.Steps(), Clock: func() time.Time { return fixedNow }}, quiet)
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

	t.Run("a closed root's todo opens its new run", func(t *testing.T) {
		pool := migratedPool(t)
		putIssue(t, pool, record.Issue{Key: root, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root,
			Project: testProject, Title: root, Phase: phase.Done, Generation: 3, Status: "done", DispatchStatus: "done", Rank: "A",
			HandedOver: true, LastDispatchSeq: 4})

		apply(t, pool, newAdmission(t, 1, quiet), "todo", todo(root, "", "A", 5, handed), newEngine())

		again := issue(t, pool, root)
		if again.Phase != phase.Admitted || again.Generation != 4 || again.Hold != nil {
			t.Fatalf("the closed root after the todo = %#v, want its new run at generation 4, admitted", again)
		}
		assertSlots(t, pool, []record.Slot{{Issue: root, Index: 0, AdmittedAt: fixedNow}})
	})

	t.Run("a held child's todo releases its hold", func(t *testing.T) {
		pool := migratedPool(t)
		seedSlotted(t, pool, root, "A")
		parent := root
		putIssue(t, pool, record.Issue{Key: child, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root, Parent: &parent,
			Project: testProject, Title: child, Phase: phase.Held, Hold: &record.Hold{From: phase.Implementing}, Generation: 1,
			Status: "in_progress", DispatchStatus: "in_progress", Rank: "B", LastDispatchSeq: 4})

		apply(t, pool, newAdmission(t, 1, quiet), "todo", todo(child, root, "B", 5, false), newEngine())

		released := issue(t, pool, child)
		if released.Phase != phase.Implementing || released.Generation != 1 || released.Hold != nil || released.Status != "in_progress" {
			t.Fatalf("the held child after the todo = %#v, want it back in implementing at its generation, its hold cleared", released)
		}
	})

	t.Run("a carried held root waits without a worker, then a todo releases its hold after promotion", func(t *testing.T) {
		const other = "LEGION-9"
		pool := migratedPool(t)
		seedSlotted(t, pool, other, "Z")
		admission, engine := newAdmission(t, 1, quiet), newEngine()
		carriedRoot(t, pool, child, phase.Held, "B")
		carried := issue(t, pool, child)
		hold := record.Hold{From: phase.Testing, Reason: record.HoldEscalated}
		carried.Hold = &hold
		putIssue(t, pool, carried)

		apply(t, pool, admission, "queued-todo", todo(child, "", "B", 2, true), engine)
		assertWaiting(t, pool, []string{child})
		if got := issue(t, pool, child); got.Phase != phase.Held || got.Hold == nil || *got.Hold != hold || got.Generation != 2 {
			t.Fatalf("carried root after todo while queued = %+v; want its held phase and generation", got)
		}
		if got := workerStartsOf(t, pool, child); len(got) != 0 {
			t.Fatalf("phase workers queued while held: %+v", got)
		}
		apply(t, pool, admission, "other-done", intake.DispatchIssue{Key: other, ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated", Status: "done",
			Title: other, Rank: "Z", HandedOver: true}, engine)
		assertSlots(t, pool, []record.Slot{{Issue: child, Index: 0, AdmittedAt: fixedNow}})
		if got := issue(t, pool, child); got.Phase != phase.Held || got.Hold == nil || *got.Hold != hold {
			t.Fatalf("promoted carried root = %+v; want its hold preserved", got)
		}
		if got := workerStartsOf(t, pool, child); len(got) != 0 {
			t.Fatalf("promotion queued phase workers while held: %+v", got)
		}
		shown := todo(child, "", "B", 3, true)
		shown.Status, shown.DaemonActor = "in_progress", true
		apply(t, pool, admission, "promoted", shown, engine)
		apply(t, pool, admission, "release-hold", todo(child, "", "B", 4, true), engine)
		if got := issue(t, pool, child); got.Phase != phase.Testing || got.Hold != nil || got.Generation != 2 {
			t.Fatalf("promoted carried root after todo = %+v; want testing in generation 2 with no hold", got)
		}
		if got := workerStartsOf(t, pool, child); len(got) != 1 || got[0].Role != claim.RoleTester {
			t.Fatalf("phase worker starts after todo = %+v; want one tester", got)
		}
	})
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

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
// The re-admissions a todo does make stand beside it unchanged, since the release's own test is
// the complement of readmittable: a lingering root starts a new generation, and a held child
// comes back into its tree at phase admitted on a generation of its own, which is the non-goal
// the spec protects. A held root whose record already reads todo, which a hand-over leaves, is
// reached by no status change at all, so its architect's retry_or_escalate is its release, as
// today.
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
		want := record.StatusWrite{Status: "in_progress", ObservedStatus: "todo"}
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

	t.Run("a held root Dispatch already shows todo is its architect's", func(t *testing.T) {
		pool := migratedPool(t)
		seedSlotted(t, pool, root, "A")
		handedOver := issue(t, pool, root)
		handedOver.Phase, handedOver.Hold = phase.Held, &record.Hold{From: phase.Implementing}
		handedOver.Status, handedOver.DispatchStatus, handedOver.LastDispatchSeq = "todo", "todo", 4
		putIssue(t, pool, handedOver)

		apply(t, pool, newAdmission(t, 1, quiet), "edit", todo(root, "", "A", 5, handed), newEngine())

		stood := issue(t, pool, root)
		if stood.Phase != phase.Held || stood.Hold == nil || stood.Hold.From != phase.Implementing || stood.LastDispatchSeq != 5 {
			t.Fatalf("the root after a todo over its own todo = %#v, want it held from implementing still, with the event recorded", stood)
		}
		if writes, starts := queuedWrites(t, pool), queuedStarts(t, pool); len(writes) != 0 || len(starts) != 0 {
			t.Fatalf("the todo queued %+v and %+v, want nothing: the status did not change, so its architect's retry is its release", writes, starts)
		}
	})
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

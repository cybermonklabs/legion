package daemon

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// A phase entered again, whose start is written over a start row the claim has already taken a
// task from, still reaches that worker.
//
// Two rules met and the task fell between them. The start of a phase entered again stands in
// the place of the start an issue already owes, so that the issue keeps its place in the
// waiting line, while supervise.Machine.queue gives a claim nothing from a row it has already
// queued a task from (supervise.Claim.DeliveredRows), so that a retry of a row never prompts
// the same work twice. Keyed on the row alone, that second rule cannot tell a retry of the row
// from the row rewritten with another phase entry's task: the start ran, the claim recognised
// the row, and the new task was dropped with the row finishing clean.
//
// What that costs is a phase with no task and no sign of one. Nothing errored and no row was
// left behind, so the state showed a working tree with a quiet worker; the claim was never given
// the phase's new entry, so every completion its worker reported was refused
// HANDOFF_STALE_ENTRY, as a completion of an entry behind the one the phase is on
// (api/handoff.go's staleEntry); and no restart cured it, because the record a restart rebuilds
// the claim from carries that same row. Only a start under a row of its own did — an operator's
// retry of the phase's worker.
//
// The state seeded here is the one the window leaves: a start row whose task reached the claim
// and which is back in the outbox unleased, which is what RetryOutbox leaves behind for any
// failure that follows the delivery, and a worker that is not taking the task now — its shim is
// not connected here, as a worker is handed nothing while the wake pacing holds back the relaunch
// that replaces its process, until the pacing lets that launch run (supervise's sendPending,
// launchWaits). The phase is then entered again, as a head that starts conflicting with its base
// or a sign-off enters one, and the transition writes its start through workflow.StartOrOwe
// exactly as the engine does.
//
// The negative control is the fix reverted — the later task written over the owed row in
// place, under its own id, as the daemon wrote it before a start became a row of its own
// (record.Store.EnqueueStart): the start then finishes without giving the worker anything,
// which this test reads at the claim's pending task.
func TestAStartWrittenOverAnOwedRowStillReachesTheWorker(t *testing.T) {
	const firstTask, nextTask = "Implement the plan.", "Implement it again. Reason: the checks went red."
	const firstEntry, nextEntry int64 = 1, 2
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	issue := record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: "LEGION", Tree: "LEGION-208", Title: "Workflow", Phase: phase.Implementing, Generation: 1, Status: "in_progress"}
	putOutboxIssue(t, pool, records, issue)
	openTree(t, pool, "LEGION", issue.Tree)
	holdSlot(t, pool, issue.Key)
	sup, runtime := newOutboxSupervisor(t, "legion", t.TempDir())
	token, err := claim.NewToken("legion", issue.Key, claim.RoleImplementer)
	if err != nil {
		t.Fatalf("claim token: %v", err)
	}

	clock := time.Now()
	// The start of the phase entry before this one, as the outbox left it: its task reached the
	// claim, and the row is back unleased for a retry it no longer needs.
	owed := record.SuperviseRequest{Op: "start", Tree: issue.Tree, Role: claim.RoleImplementer,
		Generation: issue.Generation, Phase: issue.Phase, Task: firstTask, Entry: firstEntry, NeedsSlot: true}
	enqueueOutbox(t, pool, records, mustOutboxRow(t, issue.Key, owed, clock))
	var owedRow int64
	if err := pool.QueryRow(ctx, "select id from outbox where issue = $1 and kind = 'supervise'", issue.Key).Scan(&owedRow); err != nil {
		t.Fatalf("read the owed start's row: %v", err)
	}

	// The claim as the daemon holds it then: ready, its earlier task retired with the turn that
	// worked it, so that entry is both the entry it was queued and the entry its worker took
	// (supervise.Claim.GivenEntry and ServedEntry), and the row that gave it recorded as the
	// start it last ran (StartedBy) and as one whose task it has queued, so no retry of that row
	// prompts it again.
	machine, _, err := sup.Create(ctx, supervise.Claim{Token: token, Project: "legion", Tree: issue.Tree, Issue: issue.Key,
		Role: claim.RoleImplementer, State: supervise.StateReady, Generation: 1, Session: "ses-implementer",
		SessionFile: "/tmp/implementer.jsonl", LastStartRow: owedRow, DeliveredRows: []int64{owedRow},
		GivenEntry: firstEntry, ServedEntry: firstEntry, ServingGeneration: issue.Generation,
	}, "")
	if err != nil {
		t.Fatalf("create the implementer claim: %v", err)
	}

	// The phase is entered again: its transition raises the implementer's entry and writes that
	// entry's task on the role's phases row (workflow.Engine.enter), and writes the start the one
	// way the engine writes one (workflow.Engine.started), both in its own transaction.
	next := record.SuperviseRequest{Op: "start", Tree: issue.Tree, Role: claim.RoleImplementer,
		Generation: issue.Generation, Phase: issue.Phase, Task: nextTask, Entry: nextEntry}
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if err := records.PutPhase(ctx, tx, record.PhaseRow{Issue: issue.Key, Role: claim.RoleImplementer, Claim: token,
			Entry: nextEntry, EntryTask: nextTask, EntryRaisedAt: clock}); err != nil {
			return err
		}
		return workflow.StartOrOwe(ctx, tx, records, issue.Key, next, clock)
	}); err != nil {
		t.Fatalf("write the start of the phase entered again: %v", err)
	}
	if rows := outboxRowsOf(t, pool, issue.Key); rows != 1 {
		t.Fatalf("start rows of %s after the second transition = %d, want the one start the issue is owed, carrying the later task", issue.Key, rows)
	}

	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, supervisor: sup,
		tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(), now: func() time.Time { return clock },
		provision: func(context.Context, workspace.Request) (workspace.Workspace, error) {
			return workspace.Workspace{Dir: t.TempDir(), Bookmark: "legion/LEGION-208"}, nil
		},
		remove: func(context.Context, workspace.Workspace, sync.Locker) error { return nil },
	}
	if err := runner.RunOnce(ctx); err != nil {
		t.Fatalf("run the start of the phase entered again: %v", err)
	}
	machine.Wait()

	got := machine.Claim()
	if got.Pending == nil {
		t.Fatalf("the claim after the start of the phase entered again = %+v, want it holding %q: the phase it entered has a task and nothing else will ever give it",
			got, nextTask)
	}
	if got.Pending.Task != nextTask || got.Pending.Entry != nextEntry {
		t.Fatalf("the task the start gave = %+v, want %q of entry %d: a completion of any entry behind it is refused HANDOFF_STALE_ENTRY",
			got.Pending, nextTask, nextEntry)
	}
	if len(runtime.Calls()) != 0 {
		t.Fatalf("the start made %d runtime calls, want none: the claim was ready and only owed its task", len(runtime.Calls()))
	}
	if left := outboxRowsOf(t, pool, issue.Key); left != 0 {
		t.Fatalf("outbox rows of %s after the start = %d, want none left", issue.Key, left)
	}
}

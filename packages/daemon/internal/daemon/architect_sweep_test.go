package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// The runtime's entry sweep reaches a root's own architect, which is the role no rule of the
// daemon used to reach: on its own period it applies an intake.EntrySweep, and admission sends
// the architect of a root standing at admitted the work the entry its phases row names. Nothing
// else happens here — no Dispatch event, no completion, no boot — which is the fixture the
// measured defect leaves behind (dispatch://INFRA-2614): a root admitted with its architect's
// entry raised, holding its slot, and an architect claim that was never created because the
// start that would have created it reached nobody.
//
// The task the sweep sends is derived again from the record as it stands
// (workflow.ArchitectStartTask) rather than read off the phases row, because a kept decision is
// claimed by the outbox row that names it: the row's stored text is a different string here, so
// the two readings tell apart.
//
// The sweep then keeps its period, and the row it wrote the first time is the row the root still
// holds. A start of that architect is queued, so the root is owed nothing (workflow's
// ArchitectStartPending, OwedEntry), and the runtime applies a sweep only while admission says
// something is owed (applyEntrySweep, admit's OwesEntryWork). So over many further periods no
// later sweep is applied at all, and no second start row is written.
func TestTheArchitectSweepTickSendsARootArchitectTheWorkOfItsEntry(t *testing.T) {
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	log := quietLogger()
	records := record.NewStore()
	const sweepProject = "ARCHSWEEP"
	root := record.Issue{Key: "ARCHSWEEP-900", Tree: "ARCHSWEEP-900", Project: sweepProject, Title: "the root",
		Phase: phase.Admitted, Generation: 1, Status: "in_progress", DispatchStatus: "in_progress",
		Rank: "ARCHSWEEP-900", HandedOver: true, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets"}
	project, err := claim.ProjectToken(sweepProject)
	if err != nil {
		t.Fatal(err)
	}
	token, err := claim.NewToken(project, root.Key, claim.RoleArchitect)
	if err != nil {
		t.Fatal(err)
	}
	const entry = 2
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if err := records.PutIssue(ctx, tx, root); err != nil {
			return err
		}
		if err := records.PutSlot(ctx, tx, record.Slot{Issue: root.Key, Index: 0, AdmittedAt: time.Now().UTC()}); err != nil {
			return err
		}
		return records.PutPhase(ctx, tx, record.PhaseRow{Issue: root.Key, Role: claim.RoleArchitect, Claim: token,
			Entry: entry, EntryTask: "the text the raise of entry 2 stored", EntryRaisedAt: time.Now().UTC()})
	}); err != nil {
		t.Fatalf("seed the root whose architect is behind its entry: %v", err)
	}
	if starts := sweptStarts(t, pool, root.Key); len(starts) != 0 {
		t.Fatalf("the start rows of %s before the sweep = %+v, want none: the row's own premise", root.Key, starts)
	}

	engine := workflow.New(records, workflow.Config{Project: sweepProject, Repositories: testRepositories,
		Steps: config.DesignGateRootIssues.Steps()}, log)
	admission := admit.New(records, engine, 1, sweepProject, log)
	w := &workflowRuntime{
		pool: pool, records: tickRecords(), admission: admission, handlers: []intake.Handler{engine, admission},
		dispatchProject: sweepProject, bootID: "test-boot", log: log, entrySweep: 10 * time.Millisecond,
	}
	swept := func() (n int) {
		t.Helper()
		if err := pool.QueryRow(ctx, `select count(*) from processed_events
			where event_id like 'entry-sweep:ARCHSWEEP:test-boot:%'`).Scan(&n); err != nil {
			t.Fatalf("read the sweeps applied: %v", err)
		}
		return n
	}

	sweeping, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { w.tickEntrySweep(sweeping); close(done) }()
	testwait.Eventually(t, "the sweep to send the architect its entry's work", func() bool { return len(sweptStarts(t, pool, root.Key)) == 1 })
	first := sweptStarts(t, pool, root.Key)[0]
	// With the start queued the gate answers that nothing is owed, which is what holds every
	// later tick back.
	if err := pgx.BeginTxFunc(ctx, pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		owed, err := admission.OwesEntryWork(ctx, tx)
		if err != nil {
			return err
		}
		if owed {
			t.Fatalf("admission says %s is still owed its entry's work with its start queued, want nothing owed: a start already on its way is not owed twice", root.Key)
		}
		return nil
	}); err != nil {
		t.Fatalf("ask whether anything is owed: %v", err)
	}
	// Twenty more periods of the sweep.
	time.Sleep(20 * w.entrySweep)
	stop()
	<-done
	if n := swept(); n != 1 {
		t.Fatalf("the runtime applied %d sweeps over twenty periods, want the one that sent the start: a sweep is applied only while something is owed", n)
	}

	want := sweptStart{id: first.id, role: claim.RoleArchitect,
		task:  workflow.ArchitectStartTask(workflow.ArchitectStart{Root: root, Gate: record.TreeGate{Steps: engine.GatedSteps()}}),
		entry: entry}
	if first != want {
		t.Fatalf("the start the sweep wrote for %s = %+v, want %+v: the sweep sends the architect the task the derivation gives over the record, at the entry its row names",
			root.Key, first, want)
	}
	if !workflow.CarriesArchitectStart(first.task) {
		t.Fatalf("the task the sweep sent = %q, want the first-task instruction the derivation writes", first.task)
	}
	if starts := sweptStarts(t, pool, root.Key); len(starts) != 1 || starts[0] != first {
		t.Fatalf("the start rows of %s after %d sweeps = %+v, want the one row %+v the first sweep wrote: a later sweep reads the start already on its way and writes nothing",
			root.Key, swept(), starts, first)
	}
}

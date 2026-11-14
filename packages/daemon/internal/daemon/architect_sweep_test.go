package daemon

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/capabilities"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// The runtime's entry sweep reaches a root's own architect: on its own period it applies an
// intake.EntrySweep, and admission sends the architect of a root standing at admitted the work
// the entry its phases row names. Nothing else happens here — no Dispatch event, no completion,
// no boot — which is the fixture the measured defect leaves behind (dispatch://INFRA-2614): a
// root admitted with its architect's entry raised, holding its slot, and an architect claim that
// was never created because the start that would have created it reached nobody.
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
		// The raise of entry 2 stored a task the derivation wrote, for a generation the root does
		// not stand at, so the sweep derives it again and the send is told apart from that text.
		stored := workflow.ArchitectStartTask(workflow.ArchitectStart{
			Root: record.Issue{Key: root.Key, Tree: root.Key, Phase: phase.Admitted, Generation: 99}})
		return records.PutPhase(ctx, tx, record.PhaseRow{Issue: root.Key, Role: claim.RoleArchitect, Claim: token,
			Entry: entry, EntryTask: stored, EntryRaisedAt: time.Now().UTC()})
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

	// Each sweep runs on a hold mark of its own, as workflowRuntime.run starts the sweep
	// (store.ForConcurrentWorker(running)), so the pool's guard sees it as it does in production
	// rather than refusing nothing on an unmarked context (INFRA-3169).
	sweeping, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { w.tickEntrySweep(store.ForConcurrentWorker(sweeping)); close(done) }()
	testwait.Eventually(t, "the sweep to send the architect its entry's work", func() bool { return len(sweptStarts(t, pool, root.Key)) == 1 })
	first := sweptStarts(t, pool, root.Key)[0]
	// With the start queued the gate answers that nothing is owed, which is what holds every
	// later tick back.
	if owed, err := w.owesEntryWork(store.ForConcurrentWorker(ctx)); err != nil || owed {
		t.Fatalf("the sweep's gate over %s with its start queued answers owed = %t, %v; want nothing owed: a start already on its way is not owed twice",
			root.Key, owed, err)
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
	if !strings.Contains(first.task, workflow.ArchitectFreshTreeLabel) {
		t.Fatalf("the task the sweep sent = %q, want the first-task instruction the derivation writes for a tree nobody has architected", first.task)
	}
	if starts := sweptStarts(t, pool, root.Key); len(starts) != 1 || starts[0] != first {
		t.Fatalf("the start rows of %s after %d sweeps = %+v, want the one row %+v the first sweep wrote: a later sweep reads the start already on its way and writes nothing",
			root.Key, swept(), starts, first)
	}
}

// A stall alone runs the sweep. The project is quiet — no Dispatch event, no completion, no tick
// of the controller — and its one root's architect holds the task of its entry, queued twenty
// minutes ago, with no turn of its claim confirming it. That architect is owed nothing the pass
// sends: the daemon writes no second start while that task stands (workflow's
// ArchitectStartPending). So the gate the runtime asks before each sweep must open for the stall
// itself (admit's OwesEntryWork): a gate that asked only whether a role is owed its entry's work
// would apply no fact, and the controller would hear of the stall at the project's next fact,
// which in a quiet project is its own hourly tick.
//
// The next sweep applies its fact and wakes the controller with the first arm's words. The mark
// that fact wrote is what shuts the gate again, so a sweep after it applies nothing and the
// controller is woken once. The project's controller is registered, as the daemon tells a stall
// only to a controller that is (admit's wakeStalledArchitects).
func TestTheArchitectSweepWakesTheControllerForAStallInAQuietProject(t *testing.T) {
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	log := quietLogger()
	records := record.NewStore()
	registerProjectController(t, pool, quietStallProject)
	root := seedQuietArchitectStall(t, pool)

	engine := workflow.New(records, workflow.Config{Project: quietStallProject, Repositories: testRepositories,
		Steps: config.DesignGateRootIssues.Steps()}, log)
	admission := admit.New(records, engine, 1, quietStallProject, log)
	w := &workflowRuntime{
		pool: pool, records: tickRecords(), admission: admission, handlers: []intake.Handler{engine, admission},
		dispatchProject: quietStallProject, bootID: "test-boot", log: log,
	}
	swept := func() (n int) {
		t.Helper()
		if err := pool.QueryRow(ctx, `select count(*) from processed_events
			where event_id like 'entry-sweep:ARCHSTALL:test-boot:%'`).Scan(&n); err != nil {
			t.Fatalf("read the sweeps applied: %v", err)
		}
		return n
	}

	// On a hold mark of its own, as the sweep runs in production (tickEntrySweep's caller).
	w.applyEntrySweep(store.ForConcurrentWorker(ctx), time.Now())
	if n := swept(); n != 1 {
		t.Fatalf("the sweep over a quiet project whose one root's architect took nothing applied %d facts, want the one that wakes the controller", n)
	}
	wakes := architectStalledWakes(t, pool, root.Key)
	if len(wakes) != 1 || !strings.HasPrefix(wakes[0], record.ArchitectTookNothing.Words()+":") {
		t.Fatalf("the architect-stalled wakes of %s = %q, want the one wake of the first arm", root.Key, wakes)
	}
	if starts := sweptStarts(t, pool, root.Key); len(starts) != 0 {
		t.Fatalf("the sweep wrote %s %+v, want no start: its architect holds the task of its entry, and the wake is what is left", root.Key, starts)
	}

	w.applyEntrySweep(store.ForConcurrentWorker(ctx), time.Now().Add(time.Second))
	if n := swept(); n != 1 {
		t.Fatalf("a second sweep after the wake applied %d facts in all, want still the one: the mark shuts the gate once the stall is told", n)
	}
	if wakes := architectStalledWakes(t, pool, root.Key); len(wakes) != 1 {
		t.Fatalf("the architect-stalled wakes of %s after a second sweep = %q, want the one", root.Key, wakes)
	}
}

// quietStallProject is the Dispatch project key of the quiet project whose one root's architect
// took nothing (seedQuietArchitectStall).
const quietStallProject = "ARCHSTALL"

// seedQuietArchitectStall records the one root of quietStallProject and answers it: at admitted,
// holding its slot, its architect's phases row at entry 2, and an architect claim given that
// entry that holds its task, queued twenty minutes ago and confirmed by no turn. That is the
// first arm of the architect-stalled reading, past its ten minutes, and no role of the project is
// owed anything the pass sends: the daemon writes no second start while that task stands.
func seedQuietArchitectStall(t *testing.T, pool *store.Pool) record.Issue {
	t.Helper()
	ctx := context.Background()
	records := record.NewStore()
	now := time.Now().UTC()
	root := record.Issue{Key: "ARCHSTALL-910", Tree: "ARCHSTALL-910", Project: quietStallProject, Title: "the root",
		Phase: phase.Admitted, Generation: 1, Status: "in_progress", DispatchStatus: "in_progress",
		Rank: "ARCHSTALL-910", HandedOver: true, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets"}
	project, err := claim.ProjectToken(quietStallProject)
	if err != nil {
		t.Fatal(err)
	}
	token, err := claim.NewToken(project, root.Key, claim.RoleArchitect)
	if err != nil {
		t.Fatal(err)
	}
	const entry = 2
	queued := now.Add(-20 * time.Minute)
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if err := records.PutIssue(ctx, tx, root); err != nil {
			return err
		}
		if err := records.PutSlot(ctx, tx, record.Slot{Issue: root.Key, Index: 0, AdmittedAt: queued}); err != nil {
			return err
		}
		if err := records.PutPhase(ctx, tx, record.PhaseRow{Issue: root.Key, Role: claim.RoleArchitect, Claim: token,
			Entry: entry, EntryTask: "Take the tree up.", EntryRaisedAt: queued}); err != nil {
			return err
		}
		// The architect's claim was given that entry and holds its task, unconfirmed.
		if _, err := tx.Exec(ctx, `insert into claims (token, project, tree, issue, role, generation, session, session_file, state,
			serving_generation, launch_failures, prompt_failures, prompt_retires, uncertain_streak, given_entry)
			values ($1, $2, $3, $3, 'architect', 1, 'ses', '/sessions/ses.jsonl', 'idle', 1, 0, 0, 0, 0, $4)`,
			string(token), project, root.Key, int64(entry)); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `insert into pending_task_deliveries (claim_token, delivery_id, task, queued_at, generation, phase, entry)
			values ($1, 'outbox:31', 'Take the tree up.', $2, 1, '', $3)`, string(token), queued, int64(entry))
		return err
	}); err != nil {
		t.Fatalf("seed the root whose architect took nothing: %v", err)
	}
	return root
}

// The state serves, on each root's own view, the arms of the architect-stalled reading that hold
// for its architect (api.Issue.ArchitectStalls), read in the state's own snapshot by the reading
// the admission pass wakes the controller from (admit's ArchitectStallArms), and it serves them
// whether or not the controller was told. So the arm is on the root's view while no controller
// is registered, when the daemon tells nobody and marks nothing, and it is still there after the
// sweep has told a registered controller: `legion state` prints that key and judges no arm of its
// own, so the view is the one place an operator reads which arms hold.
func TestTheArchitectSweepsStallArmsAreServedOnTheStateWhetherOrNotTheControllerWasTold(t *testing.T) {
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	log := quietLogger()
	records := record.NewStore()
	root := seedQuietArchitectStall(t, pool)
	engine := workflow.New(records, workflow.Config{Project: quietStallProject, Repositories: testRepositories,
		Steps: config.DesignGateRootIssues.Steps()}, log)
	admission := admit.New(records, engine, 1, quietStallProject, log)
	st, err := store.Open(ctx, pool.Config().ConnString())
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	t.Cleanup(st.Close)
	project, err := claim.ProjectToken(quietStallProject)
	if err != nil {
		t.Fatal(err)
	}
	view := &source{
		store: st, records: records, project: quietStallProject, projectToken: project, runtime: "tmux", admissionCap: 1,
		startedAt: time.Now(), deployment: func() capabilities.Deployment { return capabilities.Deployment{} },
		reports:         func() map[claim.Token]capabilities.Report { return nil },
		architectStalls: admission.ArchitectStallArms,
	}
	served := func() []record.ArchitectStallArm {
		t.Helper()
		tx, err := st.Pool().BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
		if err != nil {
			t.Fatalf("begin the state snapshot: %v", err)
		}
		defer tx.Rollback(ctx)
		state, _, err := view.State(ctx, tx)
		if err != nil {
			t.Fatalf("State: %v", err)
		}
		issue, shown := state.Issues[root.Key]
		if !shown {
			t.Fatalf("the state carries no view of %s", root.Key)
		}
		return issue.ArchitectStalls
	}
	tookNothing := []record.ArchitectStallArm{record.ArchitectTookNothing}

	if arms := served(); !slices.Equal(arms, tookNothing) {
		t.Fatalf("the state serves %s the arms %v while no controller is registered, want %v: the arm holds whether or not "+
			"anybody was told of it", root.Key, arms, tookNothing)
	}
	if wakes := architectStalledWakes(t, pool, root.Key); len(wakes) != 0 {
		t.Fatalf("the architect-stalled wakes of %s = %q, want none: the state's reading sends nothing", root.Key, wakes)
	}

	registerProjectController(t, pool, quietStallProject)
	w := &workflowRuntime{
		pool: pool, records: tickRecords(), admission: admission, handlers: []intake.Handler{engine, admission},
		dispatchProject: quietStallProject, bootID: "test-boot", log: log,
	}
	w.applyEntrySweep(store.ForConcurrentWorker(ctx), time.Now())
	if wakes := architectStalledWakes(t, pool, root.Key); len(wakes) != 1 {
		t.Fatalf("the architect-stalled wakes of %s after the sweep = %q, want the one that tells the registered controller", root.Key, wakes)
	}
	if arms := served(); !slices.Equal(arms, tookNothing) {
		t.Fatalf("the state serves %s the arms %v after the controller was told, want %v: a told arm still holds", root.Key, arms, tookNothing)
	}
}

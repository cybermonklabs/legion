package admit

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// seededAssignment is the assignment a row's phases row carries before the pass. The repair of a
// slot is a restart, which hands the role the assignment its phases row already names and mints
// none (workflow's restart), so a start carrying anything else is a worker given an assignment a
// completion in flight would be refused for.
func seededAssignment(issue string) string { return "assignment-" + issue }

// putWorkerClaim records the claim of key's role on the tree root, in state and suspended by by,
// in the columns every claim fixture of this package writes (owed_start_test.go's midPhaseChild).
// It answers the claim's token, which the phases row and the claim's task are written against.
func putWorkerClaim(t *testing.T, pool *pgxpool.Pool, root, key string, role claim.Role, generation uint64,
	state supervise.ClaimState, by supervise.SuspendSource,
) claim.Token {
	t.Helper()
	project, err := claim.ProjectToken(testProject)
	if err != nil {
		t.Fatalf("project token of %s: %v", testProject, err)
	}
	token, err := claim.NewToken(project, key, role)
	if err != nil {
		t.Fatalf("claim token of the %s of %s: %v", role, key, err)
	}
	if _, err := pool.Exec(context.Background(), `insert into claims (token, project, tree, issue, role, generation, session, session_file, state,
		serving_generation, last_start_row, launch_failures, prompt_failures, prompt_retires, uncertain_streak, suspended_by)
		values ($1, $2, $3, $4, $5, $6, 'ses_worker', '/sessions/ses_worker.jsonl', $7, $6, 0, 0, 0, 0, 0, $8)`,
		string(token), project, root, key, string(role), int64(generation), string(state), string(by)); err != nil {
		t.Fatalf("record the %s %s claim of %s: %v", state, role, key, err)
	}
	return token
}

// putWorkerTask records the task the claim holds: the generation and phase it was given for, and
// whether the claim confirmed it, which together with the claim's state are what say whether the
// claim still works it (supervise.Delivery.StillWorked).
func putWorkerTask(t *testing.T, pool *pgxpool.Pool, token claim.Token, generation uint64, stands phase.Phase, confirmed bool) {
	t.Helper()
	var at *time.Time
	if confirmed {
		at = &fixedNow
	}
	if _, err := pool.Exec(context.Background(), `insert into pending_task_deliveries (claim_token, delivery_id, task, queued_at, generation, phase, confirmed_at)
		values ($1, 'outbox:11', 'Carry on.', $2, $3, $4, $5)`,
		string(token), fixedNow, int64(generation), string(stands), at); err != nil {
		t.Fatalf("record the task the claim %s holds: %v", token, err)
	}
}

// taskedStartAssignments is the assignment each start row of issue that carries a task names, in
// the order the rows were written. startsOf reads every other field of those rows; the assignment
// is read here because what the repair hands back is the whole point of the row.
func taskedStartAssignments(t *testing.T, pool *pgxpool.Pool, issue string) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `select coalesce(payload->>'assignment', '') from outbox
		where issue = $1 and kind = 'supervise' and payload->>'op' = 'start' and coalesce(payload->>'task', '') <> ''
		order by id`, issue)
	if err != nil {
		t.Fatalf("list the assignments the starts of %s name: %v", issue, err)
	}
	defer rows.Close()
	var named []string
	for rows.Next() {
		var assignment string
		if err := rows.Scan(&assignment); err != nil {
			t.Fatalf("read an assignment a start of %s names: %v", issue, err)
		}
		named = append(named, assignment)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("list the assignments the starts of %s name: %v", issue, err)
	}
	return named
}

// owedStarts is every start row of issue that waits for its admission slot, which is the set
// finishFencedStarts weighs (record.Store.OwedStarts reads the same rows).
func owedStarts(t *testing.T, pool *pgxpool.Pool, issue string) []midPhaseStart {
	t.Helper()
	var owed []midPhaseStart
	for _, start := range startsOf(t, pool, issue) {
		if start.needsSlot {
			owed = append(owed, start)
		}
	}
	return owed
}

// seedTreeAndMember records the tree every fixture of this file stands in: root, and the one
// member of it, key, standing in stands at generation. The root waits in accepting while its
// member works, so the member's is the one slot of the tree and nothing of the root competes for
// another. A root that lingers stands in done instead, with its deadline still ahead of it, which
// is how a parked tree reads.
//
// What each caller adds after it is its own: seedSlotRepairRow takes the member's slot and writes
// the role's phases row, the task its claim holds and the start it already has; seedOwedRepairRow
// writes the task the claim holds and the start the member is owed; capFiller takes the slot and
// keeps the cap full with a claim at work. Each of the three writes the member's claim itself,
// because the state it is in is the one thing its own rows are about.
func seedTreeAndMember(t *testing.T, pool *pgxpool.Pool, root, key string, stands phase.Phase, generation uint64, lingers bool) {
	t.Helper()
	tree := record.Issue{Key: root, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: testProject, Title: "the root of " + key, Tree: root, Phase: phase.Accepting,
		Generation: generation, Status: "needs_review", DispatchStatus: "needs_review", Rank: root,
		HandedOver: true, LastDispatchSeq: 1}
	if lingers {
		until := fixedNow.Add(time.Hour)
		tree.Phase, tree.Status, tree.DispatchStatus, tree.LingerUntil = phase.Done, "in_progress", "in_progress", &until
	}
	putIssue(t, pool, tree)
	seedLifecycle(t, pool, root)
	parent := root
	putIssue(t, pool, record.Issue{Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: testProject, Title: "the member " + key, Tree: root, Parent: &parent, Phase: stands,
		Generation: generation, Status: "in_progress", DispatchStatus: "in_progress", Rank: key, LastDispatchSeq: 2})
}

// slotRepairRow is one row of the repair of a slot whose worker has no task. Every row is the
// same fixture — a running tree, a member of it holding a slot mid-phase, the phases row that
// names the role's assignment, and a claim with no task it still works — and each row changes the
// one thing it is about, which why names.
type slotRepairRow struct {
	name string
	// root and key are the tree's root and the member holding the slot.
	root, key string
	// stands is the phase the member is in, which names the role that works it
	// (workflow.RoleFor).
	stands phase.Phase
	// generation is the member's, which the start names and the claim's task is read against.
	generation uint64
	// lingers records the root as a park left it: phase done at a status that is not todo, with
	// a linger deadline ahead, which the project's live read leaves out.
	lingers bool
	// state and suspendedBy are the claim's.
	state       supervise.ClaimState
	suspendedBy supervise.SuspendSource
	// taskGeneration is the generation of the task the claim holds, 0 for a claim holding none,
	// and confirmed whether the claim confirmed it. The task's phase is always the member's.
	taskGeneration uint64
	confirmed      bool
	// queuedStart queues a start of the member's own phase before the pass, which is what says
	// the role is already started (workflow.StartFor).
	queuedStart bool
	// repaired is whether the pass starts the member's phase again, and slotGivenBack whether
	// the pass takes the member's slot back before the repair reads the slots at all
	// (releaseSlots, record.NeedsSlot).
	repaired      bool
	slotGivenBack bool
	// why is what this row reads, and for a row that is passed over, the one field it changes.
	why string
}

// seedSlotRepairRow records row's fixture: its tree, the member holding a slot, the role's phases
// row and claim, the task the claim holds and the start it already has.
func seedSlotRepairRow(t *testing.T, pool *pgxpool.Pool, row slotRepairRow) {
	t.Helper()
	seedTreeAndMember(t, pool, row.root, row.key, row.stands, row.generation, row.lingers)
	takeSlot(t, pool, row.key)
	role := workflow.RoleFor(row.stands)
	token := putWorkerClaim(t, pool, row.root, row.key, role, row.generation, row.state, row.suspendedBy)
	inTx(t, pool, func(tx pgx.Tx) {
		if err := record.NewStore().PutPhase(context.Background(), tx, record.PhaseRow{Issue: row.key, Role: role,
			Claim: token, Assignment: seededAssignment(row.key)}); err != nil {
			t.Fatalf("record the %s's phases row of %s: %v", role, row.key, err)
		}
	})
	if row.taskGeneration != 0 {
		putWorkerTask(t, pool, token, row.taskGeneration, row.stands, row.confirmed)
	}
	if row.queuedStart {
		inTx(t, pool, func(tx pgx.Tx) {
			if err := workflow.StartOrOwe(context.Background(), tx, record.NewStore(), row.key, record.SuperviseRequest{
				Op: "start", Tree: row.root, Role: role, Generation: row.generation, Phase: row.stands,
				Task: "Carry on.", Assignment: seededAssignment(row.key)}, fixedNow); err != nil {
				t.Fatalf("queue the start %s already has: %v", row.key, err)
			}
		})
	}
}

// owedRepairRow is one row of the repair of a start that can no longer act. Every row is the same
// fixture as parked_owed_start_test.go's — a running tree and a member of it, holding no slot,
// owing its worker the start row the transition wrote — with the claim that start would reach.
type owedRepairRow struct {
	name string
	// root and key are the tree's root and the member owed the start.
	root, key string
	// stands is the phase the member is in, which the owed row names and the fences compare.
	stands phase.Phase
	// state and suspendedBy are the claim's.
	state       supervise.ClaimState
	suspendedBy supervise.SuspendSource
	// relaunched writes the owed row through workflow.StartOrOwe, so the relaunch row of the same
	// start is queued beside it, rather than enqueueing the owed row alone.
	relaunched bool
	// holdsTask gives the claim the owed row's own task, of that row's generation and phase and
	// unconfirmed, so the claim still works it.
	holdsTask bool
	// kept is whether the row is still owed after the pass.
	kept bool
	// why is what this row reads, and for a row that is kept, the one field it changes.
	why string
}

// seedOwedRepairRow records row's fixture: its tree, the member owing the start, the claim and
// the owed row itself.
func seedOwedRepairRow(t *testing.T, pool *pgxpool.Pool, row owedRepairRow) {
	t.Helper()
	seedTreeAndMember(t, pool, row.root, row.key, row.stands, 1, false)
	role := workflow.RoleFor(row.stands)
	token := putWorkerClaim(t, pool, row.root, row.key, role, 1, row.state, row.suspendedBy)
	if row.holdsTask {
		putWorkerTask(t, pool, token, 1, row.stands, false)
	}
	request := record.SuperviseRequest{Op: "start", Tree: row.root, Role: role, Generation: 1, Phase: row.stands,
		Task: "Carry on.", Assignment: seededAssignment(row.key)}
	inTx(t, pool, func(tx pgx.Tx) {
		ctx := context.Background()
		if row.relaunched {
			if err := workflow.StartOrOwe(ctx, tx, record.NewStore(), row.key, request, fixedNow); err != nil {
				t.Fatalf("owe %s its start with the relaunch of the same start beside it: %v", row.key, err)
			}
			return
		}
		request.NeedsSlot = true
		outbox, err := record.NewOutboxRow(row.key, request, fixedNow)
		if err != nil {
			t.Fatalf("build the start %s is owed: %v", row.key, err)
		}
		if err := record.NewStore().Enqueue(ctx, tx, outbox); err != nil {
			t.Fatalf("enqueue the start %s is owed: %v", row.key, err)
		}
	})
	if owed := owedStarts(t, pool, row.key); len(owed) != 1 {
		t.Fatalf("the starts %s owes before the pass = %+v, want the one its fixture wrote: the row's own premise", row.key, owed)
	}
}

// capFiller records key as a member of its own running tree, holding a slot with a claim at work.
// It keeps the cap full, so the pass promotes nothing and every row of its group stands where its
// fixture put it. The repair of a slot passes over it because a live claim is a role already
// started (workflow.StartFor), so it writes no start of its own either.
func capFiller(t *testing.T, pool *pgxpool.Pool, root, key string) {
	t.Helper()
	seedTreeAndMember(t, pool, root, key, phase.Testing, 1, false)
	takeSlot(t, pool, key)
	putWorkerClaim(t, pool, root, key, claim.RoleTester, 1, supervise.StateWorking, "")
}

// An operator's `legion claims stop` of a phase worker holds against both repairs admission runs
// on every pass, and neither repair is narrowed any further than the stop.
//
// The repair of a slot whose worker has no task (resumeSlottedWorkers) starts the phase of a
// slotted member again whenever nothing else will, and the skip of a worker an operator stopped
// takes nothing else with it. The first group is the whole of that rule: four slots it still
// repairs, each getting the phase's resume task and the assignment its phases row already names,
// and four it passes over. The four it repairs are what hold the new skip to the retired state
// alone — a claim suspended by the daemon, a failed one, a queued one and one holding a task of
// an older run are every other claim that holds no task it still works — and the row of the
// released pod is what holds the new skip beside the idle rule's rather than in place of it. The
// retired claim's own slot is read by TestARetiredWorkersSlotIsNotRepaired, beside what the stop
// costs.
//
// Each row reads its slot as well, because two of the four it passes over are passed over by the
// pass before the repair ever reads them: no slot of an issue in accepting, and none of a member
// of a lingering tree, survives the pass's own release of the slots an issue no longer needs
// (record.NeedsSlot). The repair's two guards for them stand behind that release, so what those
// two rows lock is the pass's answer rather than the guard.
//
// The repair of a start that can no longer act (finishFencedStarts) ends an owed start row whose
// worker was stopped, because the row would otherwise start that worker again the moment
// a slot came. It is held to exactly that: the second group varies the stop rule's first term,
// the claim's state, over a worker at work and the two suspends, and reads that every row the
// fences keep is still owed after the pass; and the third reads the three terms of the rule
// itself — the claim is retired, no other start of it is queued, and it works no task of the row.
//
// Each group is one pool, one table of rows of one fixture shape, and one pass, the controller
// tick. A row that is passed over is the same fixture as a row that is repaired but for the one
// field its why names, so no row reads a fixture the repair would never have acted on at all.
func TestASlotWhoseWorkerHasNoTaskIsStartedAgain(t *testing.T) {
	t.Run("the repair of a slot whose worker has no task", func(t *testing.T) {
		rows := []slotRepairRow{
			{name: "a claim the daemon suspended", root: "LEGION-510", key: "LEGION-511", stands: phase.Testing,
				generation: 1, state: supervise.StateSuspended, suspendedBy: supervise.SuspendedByDaemon,
				taskGeneration: 1, confirmed: true, repaired: true,
				why: "the daemon's own suspend leaves a worker with no task and nothing else to start it, which is what this repair exists for"},
			{name: "a failed claim", root: "LEGION-520", key: "LEGION-521", stands: phase.Testing,
				generation: 1, state: supervise.StateFailed, taskGeneration: 1, confirmed: true, repaired: true,
				why: "a claim that spent its launch budget runs nothing, so its slot holds a worker with no task"},
			{name: "a queued claim", root: "LEGION-530", key: "LEGION-531", stands: phase.Testing,
				generation: 1, state: supervise.StateQueued, taskGeneration: 1, confirmed: true, repaired: true,
				why: "nothing was ever launched for a queued claim, so its slot holds a worker with no task"},
			{name: "a claim holding a task of an older generation", root: "LEGION-540", key: "LEGION-541", stands: phase.Testing,
				generation: 2, state: supervise.StateSuspended, suspendedBy: supervise.SuspendedByDaemon,
				taskGeneration: 1, repaired: true,
				why: "the task the claim holds is of the run before this one, which no claim still works whatever its state (supervise.Delivery.StillWorked)"},
			{name: "a claim the idle rule suspended", root: "LEGION-550", key: "LEGION-551", stands: phase.Testing,
				generation: 1, state: supervise.StateSuspended, suspendedBy: supervise.SuspendedByReleased,
				taskGeneration: 1, confirmed: true,
				why: "this row changes suspended_by alone, from the daemon to the idle rule's release of the pod: the wake gives the role the one task that says its pod came back, so a start here would be a second task for work it already holds"},
			{name: "an architect's role", root: "LEGION-560", key: "LEGION-561", stands: phase.Accepting,
				generation: 1, state: supervise.StateSuspended, suspendedBy: supervise.SuspendedByDaemon,
				taskGeneration: 1, confirmed: true, slotGivenBack: true,
				why: "this row changes the phase alone, to the one an architect works: the repair starts phase workers, and an architect is started by its tree's own admission. The pass answers before the repair reads the slots — an issue in accepting is at work for nothing, so the pass gives its slot back first (record.NeedsSlot) — so what this row reads is the pass's whole answer rather than the repair's own skip of the role"},
			{name: "a slot whose root lingers", root: "LEGION-570", key: "LEGION-571", stands: phase.Testing,
				generation: 1, lingers: true, state: supervise.StateSuspended, suspendedBy: supervise.SuspendedByDaemon,
				taskGeneration: 1, confirmed: true, slotGivenBack: true,
				why: "this row changes the root's own record alone, to the one a park left: nothing of a tree that has left the workflow is started again until its re-admission. The pass answers before the repair reads the slots — a member of a lingering tree is at work for nothing — so what this row reads is the pass's whole answer rather than the repair's own skip of the root"},
			{name: "a role with a start already queued", root: "LEGION-580", key: "LEGION-581", stands: phase.Testing,
				generation: 1, state: supervise.StateSuspended, suspendedBy: supervise.SuspendedByDaemon,
				taskGeneration: 1, confirmed: true, queuedStart: true,
				why: "this row changes the queued rows alone, adding the start the workflow has already written: the worker is started by that row when its slot comes, and a second start would give the phase two tasks (workflow.StartFor)"},
		}
		pool := migratedPool(t)
		before := make(map[string][]midPhaseStart, len(rows))
		for _, row := range rows {
			seedSlotRepairRow(t, pool, row)
			seeded := startsOf(t, pool, row.key)
			want := 0
			if row.queuedStart {
				// The relaunch row and the row that carries the task (workflow.StartOrOwe).
				want = 2
			}
			if len(seeded) != want {
				t.Fatalf("the start rows of %s before the pass = %+v, want %d: the row's own premise", row.key, seeded, want)
			}
			before[row.key] = seeded
		}
		// The cap is the slots the fixture took, so the pass has none to give out and promotes
		// nothing: every start row the rows read is the repair's own.
		tick(t, pool, quiet(t, len(rows)), "tick")

		for _, row := range rows {
			t.Run(row.name, func(t *testing.T) {
				if held := holdsSlot(t, pool, row.key); held == row.slotGivenBack {
					t.Fatalf("after the pass %s holds one of the cap's slots = %t, want %t: a slot stays with an issue while a worker of it has work and the pass gives it back when none has (record.NeedsSlot), which the repair neither does nor undoes: %s",
						row.key, held, !row.slotGivenBack, row.why)
				}
				got := startsOf(t, pool, row.key)
				if !row.repaired {
					if !slices.Equal(got, before[row.key]) {
						t.Fatalf("the start rows of %s after the pass = %+v, want the %+v its fixture left and nothing more: %s",
							row.key, got, before[row.key], row.why)
					}
					return
				}
				var relaunch, tasked []midPhaseStart
				for _, start := range got {
					if start.task == "" {
						relaunch = append(relaunch, start)
						continue
					}
					tasked = append(tasked, start)
				}
				if len(relaunch) != 1 || len(tasked) != 1 {
					t.Fatalf("the start rows of %s after the pass = %+v, want its one relaunch and the one row that carries the task (workflow.StartOrOwe): %s",
						row.key, got, row.why)
				}
				role := workflow.RoleFor(row.stands)
				if tasked[0].role != role || !tasked[0].needsSlot {
					t.Fatalf("the start %s carries a task on after the pass = %+v, want the %s's, held for the issue's own slot: the repair starts the worker of the phase the issue stands in, and its task waits for the slot as every phase worker's start does",
						row.key, tasked[0], role)
				}
				if want := workflow.ResumePhaseTask(issue(t, pool, row.key)); tasked[0].task != want {
					t.Fatalf("the task the start of %s carries = %q, want %q: the repair is the held phase's retry, which tells the worker to carry the phase's work on rather than to begin it",
						row.key, tasked[0].task, want)
				}
				named := taskedStartAssignments(t, pool, row.key)
				onRow := assignmentOf(t, pool, row.key, role)
				if onRow != seededAssignment(row.key) {
					t.Fatalf("the %s's assignment on %s after the pass = %q, want the %q its phases row already named: the repair mints none, so a completion still in flight is never refused for an assignment nothing delivered",
						role, row.key, onRow, seededAssignment(row.key))
				}
				if len(named) != 1 || named[0] != onRow {
					t.Fatalf("the assignments the starts of %s name = %q, want the one %q its phases row names: %s",
						row.key, named, onRow, row.why)
				}
			})
		}
	})

	t.Run("a claim that is not retired keeps its owed start", func(t *testing.T) {
		rows := []owedRepairRow{
			{name: "a live claim", root: "LEGION-610", key: "LEGION-611", stands: phase.Implementing,
				state: supervise.StateWorking, kept: true,
				why: "the claim's state alone tells this row from one the stop rule ends: a worker that runs was stopped by nobody, and its start acts the moment its slot comes"},
			{name: "a claim the daemon suspended", root: "LEGION-620", key: "LEGION-621", stands: phase.Implementing,
				state: supervise.StateSuspended, suspendedBy: supervise.SuspendedByDaemon, kept: true,
				why: "the claim's state alone tells this row from one the stop rule ends: the daemon's suspend is undone by the start itself, which resumes the session it kept"},
			{name: "a claim an operator suspended", root: "LEGION-630", key: "LEGION-631", stands: phase.Implementing,
				state: supervise.StateSuspended, suspendedBy: supervise.SuspendedByOperator, kept: true,
				why: "the claim's state alone tells this row from one the stop rule ends: a suspend is not a stop, and the operator's resume puts the worker back on the work this row carries"},
		}
		pool := migratedPool(t)
		// One slot, taken, so the pass promotes none of these issues: each owes a start and would
		// otherwise stand first in the waiting line.
		capFiller(t, pool, "LEGION-600", "LEGION-601")
		for _, row := range rows {
			seedOwedRepairRow(t, pool, row)
		}
		tick(t, pool, quiet(t, 1), "tick")

		for _, row := range rows {
			t.Run(row.name, func(t *testing.T) {
				assertOwedAfterThePass(t, pool, row)
			})
		}
	})

	t.Run("the stop that ends a start owed a worker the operator stopped", func(t *testing.T) {
		rows := []owedRepairRow{
			{name: "a retired claim whose relaunch row is still queued", root: "LEGION-710", key: "LEGION-711",
				stands: phase.Implementing, state: supervise.StateRetired, relaunched: true, kept: true,
				why: "this row changes the queued rows alone, adding the relaunch the start was written with: a second queued start says the workflow is starting this worker again, and the stop is over for a worker the workflow starts"},
			{name: "a retired claim holding an unconfirmed task of the row's own run", root: "LEGION-720", key: "LEGION-721",
				stands: phase.Implementing, state: supervise.StateRetired, holdsTask: true, kept: true,
				why: "this row changes the task the claim holds alone, giving it this row's own generation and phase unconfirmed: the claim kept the work of this very row, so the row is not a start the stop outlived"},
			{name: "a retired claim with neither", root: "LEGION-730", key: "LEGION-731",
				stands: phase.Implementing, state: supervise.StateRetired,
				why: "all three terms hold: the operator stopped the worker, no later start of it is queued and it works no task of this row, so the row would start the stopped worker again the moment a slot came"},
		}
		pool := migratedPool(t)
		capFiller(t, pool, "LEGION-700", "LEGION-701")
		for _, row := range rows {
			seedOwedRepairRow(t, pool, row)
		}
		tick(t, pool, quiet(t, 1), "tick")

		for _, row := range rows {
			t.Run(row.name, func(t *testing.T) {
				assertOwedAfterThePass(t, pool, row)
			})
		}
	})
}

// assertOwedAfterThePass reads whether row's issue still owes its worker the start its fixture
// wrote, which is the whole of what the repair of a start that can no longer act writes.
func assertOwedAfterThePass(t *testing.T, pool *pgxpool.Pool, row owedRepairRow) {
	t.Helper()
	owed := owedStarts(t, pool, row.key)
	if row.kept {
		if len(owed) != 1 {
			t.Fatalf("the starts %s owes after the pass = %+v, want the one its fixture wrote: %s", row.key, owed, row.why)
		}
		return
	}
	if len(owed) != 0 {
		t.Fatalf("the starts %s owes after the pass = %+v, want none: %s", row.key, owed, row.why)
	}
}

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

// seededEntry is the phase entry a row's phases row is on before the pass, and seededEntryTask
// the work that entry names. The repair of a slot sends the role the work its entry names, at
// that entry, raising none (workflow's SendOwedEntry), so a start carrying anything else is a
// worker given work its record does not name and an entry a completion in flight would be
// refused for.
const seededEntry = 3

func seededEntryTask(issue string) string { return "Carry the work of " + issue + " on." }

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

// markStopAfterTheRow stamps token's claim with a fresh number from outbox_write_mark_seq,
// which is what the write that records a retirement takes (store's putClaim, whose doc holds
// what that sequence orders): the claim retired after the row issue is owed was written, so the
// stop outlived that row. It is set here rather than in putWorkerClaim because the fixture
// writes the claim before it writes the row, and the mark is taken at the moment the retirement
// is recorded.
//
// A claim left without it carries the column's default, 0, which is below every write mark: the
// stop came before the row was written, so the row is the workflow's own next start of that
// worker. Those two are the whole of the stop rule's second term (admit's stopOutlivedStart).
func markStopAfterTheRow(t *testing.T, pool *pgxpool.Pool, issue string, token claim.Token) {
	t.Helper()
	ctx := context.Background()
	var owed int64
	if err := pool.QueryRow(ctx, `select coalesce(max(write_mark), 0) from outbox
		where issue = $1 and kind = 'supervise' and payload->>'op' = 'start'`, issue).Scan(&owed); err != nil {
		t.Fatalf("read the write mark of the start %s owes: %v", issue, err)
	}
	var mark int64
	if err := pool.QueryRow(ctx, `update claims set retired_at_write_mark = nextval('outbox_write_mark_seq')
		where token = $1 returning retired_at_write_mark`, string(token)).Scan(&mark); err != nil {
		t.Fatalf("stamp %s with the write mark the stop of %s took: %v", token, issue, err)
	}
	if owed == 0 || mark <= owed {
		t.Fatalf("the claim %s is marked at write %d with the start %s owes written at %d, want the mark above it: the row was written before the stop was recorded",
			token, mark, issue, owed)
	}
}

// markGivenEntry records entry as the phase entry token's claim was last queued a task of
// (claims.given_entry, which the supervisor writes with the delivery and the outbox writes when
// it holds a task back: supervise's queue and EntryGiven). It is what the pass weighs the entry
// the role's phases row names against (workflow.OwedEntry), so a fixture that leaves it at the
// column's default, 0, is a claim that was given nothing.
func markGivenEntry(t *testing.T, pool *pgxpool.Pool, token claim.Token, entry int64) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `update claims set given_entry = $2 where token = $1`,
		string(token), entry); err != nil {
		t.Fatalf("record the entry %d the claim %s was given: %v", entry, token, err)
	}
}

// taskedStartEntries is the entry each start row of issue that carries a task is of, in the
// order the rows were written. startsOf reads every other field of those rows; the entry is read
// here because what the repair hands back is the whole point of the row.
func taskedStartEntries(t *testing.T, pool *pgxpool.Pool, issue string) []int64 {
	t.Helper()
	rows, err := pool.Query(context.Background(), `select coalesce((payload->>'entry')::bigint, 0) from outbox
		where issue = $1 and kind = 'supervise' and payload->>'op' = 'start' and coalesce(payload->>'task', '') <> ''
		order by id`, issue)
	if err != nil {
		t.Fatalf("list the entries the starts of %s are of: %v", issue, err)
	}
	defer rows.Close()
	var named []int64
	for rows.Next() {
		var entry int64
		if err := rows.Scan(&entry); err != nil {
			t.Fatalf("read an entry a start of %s is of: %v", issue, err)
		}
		named = append(named, entry)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("list the entries the starts of %s are of: %v", issue, err)
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
// member works and this fixture gives it no row of its own, so the member's is the one row of
// the tree and nothing of the root competes for another. A root that lingers stands in done
// instead, with its deadline still ahead of it, which is how a parked tree reads.
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
// names the role's phase entry and the work of it, and a claim with no task it still works — and
// each row changes the one thing it is about, which why names.
type slotRepairRow struct {
	name string
	// root and key are the tree's root and the member holding the slot.
	root, key string
	// stands is the phase the member is in, which names the role that works it
	// (phase.RoleFor).
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
	// given is the entry the claim was last queued a task of (record.RoleClaim.GivenEntry), 0
	// for a claim given none: a claim below the row's seededEntry is one the pass owes that
	// entry's work (workflow.OwedEntry).
	given int64
	// queuedStart queues a start of the member's own phase, of the entry its phases row names,
	// before the pass: the row that already carries this entry's work.
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
	role := phase.RoleFor(row.stands)
	token := putWorkerClaim(t, pool, row.root, row.key, role, row.generation, row.state, row.suspendedBy)
	inTx(t, pool, func(tx pgx.Tx) {
		if err := record.NewStore().PutPhase(context.Background(), tx, record.PhaseRow{Issue: row.key, Role: role,
			Claim: token, Entry: seededEntry, EntryTask: seededEntryTask(row.key), EntryRaisedAt: fixedNow}); err != nil {
			t.Fatalf("record the %s's phases row of %s: %v", role, row.key, err)
		}
	})
	if row.given != 0 {
		markGivenEntry(t, pool, token, row.given)
	}
	if row.taskGeneration != 0 {
		putWorkerTask(t, pool, token, row.taskGeneration, row.stands, row.confirmed)
	}
	if row.queuedStart {
		inTx(t, pool, func(tx pgx.Tx) {
			if err := workflow.StartOrOwe(context.Background(), tx, record.NewStore(), row.key, record.SuperviseRequest{
				Op: "start", Tree: row.root, Role: role, Generation: row.generation, Phase: row.stands,
				Task: "Carry on.", Entry: seededEntry}, fixedNow); err != nil {
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
	// stoppedAfterTheRow marks the claim with a write mark taken after the owed row was written,
	// which is what the write that records a retirement takes (store's putClaim): the row was
	// already written when the worker was stopped. Without it the claim carries the column's
	// default, 0, which is the stop made before the row was written — the workflow's own next
	// start of that worker.
	stoppedAfterTheRow bool
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
	role := phase.RoleFor(row.stands)
	token := putWorkerClaim(t, pool, row.root, row.key, role, 1, row.state, row.suspendedBy)
	if row.holdsTask {
		putWorkerTask(t, pool, token, 1, row.stands, false)
	}
	request := record.SuperviseRequest{Op: "start", Tree: row.root, Role: role, Generation: 1, Phase: row.stands,
		Task: "Carry on.", Entry: seededEntry}
	inTx(t, pool, func(tx pgx.Tx) {
		if err := workflow.StartOrOwe(context.Background(), tx, record.NewStore(), row.key, request, fixedNow); err != nil {
			t.Fatalf("owe %s its start through the workflow's own writer: %v", row.key, err)
		}
	})
	if row.stoppedAfterTheRow {
		markStopAfterTheRow(t, pool, row.key, token)
	}
	if owed := owedStarts(t, pool, row.key); len(owed) != 1 {
		t.Fatalf("the starts %s owes before the pass = %+v, want the one its fixture wrote: the row's own premise", row.key, owed)
	}
}

// capFiller records key as a member of its own running tree, holding a slot with a claim at work.
// It keeps the cap full, so the pass promotes nothing and every row of its group stands where its
// fixture put it. The repair of a slot passes over it because no transition ever handed its
// tester work: it has no phases row, so the entry its role stands on is 0 and its claim, given
// nothing, is behind none of it (workflow.OwedEntry).
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
// repairs, each sent the work its phase entry names, at the entry its phases row already holds,
// and four it passes over. The four it repairs are what hold the new skip to the retired state
// alone — a claim suspended by the daemon, a failed one, a queued one and one holding a task of
// an older run are every other claim that holds no task it still works — and the row of the
// released pod is what holds the new skip beside the idle rule's rather than in place of it. The
// retired claim's own slot is read by TestARetiredWorkersSlotIsNotRepaired, beside what the stop
// costs.
//
// Each row reads its slot as well, because one of the four it passes over is passed over by the
// pass before the repair ever reads it: no slot of a member of a lingering tree survives the
// pass's own release of the slots an issue no longer needs (record.NeedsSlot). That row's guard
// stands behind the release, so what it locks is the pass's answer rather than the guard. The
// architect's row is the repair's own skip and nothing else: an issue in accepting keeps its
// row now (record.KeepsSlot), so the release leaves it standing and only the role test answers
// it.
//
// The repair of a start that can no longer act (finishFencedStarts) ends an owed start row
// written before its worker was stopped, because the row's only effect when a slot came would be
// to start that stopped worker. It is held to exactly that: the second group varies the stop
// rule's first term, the claim's state, over a worker at work and the two suspends, and reads
// that every row the fences keep is still owed after the pass; and the third reads the three
// terms of the rule itself — the claim is retired, the owed row's write mark is at or below the
// mark its retirement took, and the claim works no task of that row — with, beside them, the row
// the repair must never end: one written after the stop, which is the workflow's next start of
// this worker and keeps its issue in the waiting line.
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
			{name: "a claim the idle rule released, level with its entry", root: "LEGION-550", key: "LEGION-551", stands: phase.Testing,
				generation: 1, state: supervise.StateSuspended, suspendedBy: supervise.SuspendedByReleased,
				taskGeneration: 1, confirmed: true, given: seededEntry,
				why: "this row changes suspended_by and the entry the claim was given together, to the idle rule's release of a pod that holds its row's own entry: the wake gives the role the one task that says its pod came back, so a start here would be a second task for work it already holds. A released claim below that entry is sent it, which converge_entry_test.go reads"},
			{name: "an architect's role", root: "LEGION-560", key: "LEGION-561", stands: phase.Accepting,
				generation: 1, state: supervise.StateSuspended, suspendedBy: supervise.SuspendedByDaemon,
				taskGeneration: 1, confirmed: true,
				why: "this row changes the phase alone, to the one an architect works: the repair starts phase workers, and an architect is started by its tree's own admission. The member keeps its row in accepting (record.KeepsSlot), so the release leaves the row standing and the repair's own skip of the role is the whole answer"},
			{name: "a slot whose root lingers", root: "LEGION-570", key: "LEGION-571", stands: phase.Testing,
				generation: 1, lingers: true, state: supervise.StateSuspended, suspendedBy: supervise.SuspendedByDaemon,
				taskGeneration: 1, confirmed: true, slotGivenBack: true,
				why: "this row changes the root's own record alone, to the one a park left: nothing of a tree that has left the workflow is started again until its re-admission. The pass answers before the repair reads the slots — a member of a lingering tree is at work for nothing — so what this row reads is the pass's whole answer rather than the repair's own skip of the root"},
			{name: "a role with a start already queued", root: "LEGION-580", key: "LEGION-581", stands: phase.Testing,
				generation: 1, state: supervise.StateSuspended, suspendedBy: supervise.SuspendedByDaemon,
				taskGeneration: 1, confirmed: true, queuedStart: true,
				why: "this row changes the queued rows alone, adding the start the workflow has already written for this very entry: the worker is started by that row when its slot comes, and a second start would give the phase two tasks (workflow.OwedEntry)"},
		}
		pool := migratedPool(t)
		before := make(map[string][]midPhaseStart, len(rows))
		for _, row := range rows {
			seedSlotRepairRow(t, pool, row)
			seeded := startsOf(t, pool, row.key)
			want := 0
			if row.queuedStart {
				// The one row a start is written as (workflow.StartOrOwe).
				want = 1
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
					t.Fatalf("after the pass %s holds one of the cap's rows = %t, want %t: an issue keeps its row from the moment the daemon starts it until it is done or leaves the workflow, and the pass deletes it when the tree it belongs to has left (record.NeedsSlot), which the repair neither does nor undoes: %s",
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
				if len(got) != 1 {
					t.Fatalf("the start rows of %s after the pass = %+v, want the one row that carries the task (workflow.StartOrOwe): %s",
						row.key, got, row.why)
				}
				tasked := got
				role := phase.RoleFor(row.stands)
				if tasked[0].role != role || !tasked[0].needsSlot {
					t.Fatalf("the start %s carries a task on after the pass = %+v, want the %s's, held for the issue's own slot: the repair starts the worker of the phase the issue stands in, and its task waits for the slot as every phase worker's start does",
						row.key, tasked[0], role)
				}
				if want := seededEntryTask(row.key); tasked[0].task != want {
					t.Fatalf("the task the start of %s carries = %q, want the %q its phase entry names: the repair sends the work the record holds for that entry, so the worker is given the task it is owed rather than one composed again",
						row.key, tasked[0].task, want)
				}
				named := taskedStartEntries(t, pool, row.key)
				onRow, work := entryOf(t, pool, row.key, role)
				if onRow != seededEntry || work != seededEntryTask(row.key) {
					t.Fatalf("the %s's row of %s after the pass is at entry %d naming %q, want the entry %d and work %q it already named: the repair raises none, so a completion still in flight is never refused for an entry nothing delivered",
						role, row.key, onRow, work, seededEntry, seededEntryTask(row.key))
				}
				if len(named) != 1 || named[0] != onRow {
					t.Fatalf("the entries the starts of %s are of = %d, want the one %d its phases row names: %s",
						row.key, named, onRow, row.why)
				}
			})
		}
	})

	t.Run("a claim that is not retired keeps its owed start", func(t *testing.T) {
		rows := []owedRepairRow{
			{name: "a live claim", root: "LEGION-610", key: "LEGION-611", stands: phase.Implementing,
				state: supervise.StateWorking, stoppedAfterTheRow: true, kept: true,
				why: "the claim's state alone tells this row from one the stop rule ends: a worker that runs is under no stop, whatever older one its mark records, and its start acts the moment its slot comes"},
			{name: "a claim the daemon suspended", root: "LEGION-620", key: "LEGION-621", stands: phase.Implementing,
				state: supervise.StateSuspended, suspendedBy: supervise.SuspendedByDaemon, stoppedAfterTheRow: true, kept: true,
				why: "the claim's state alone tells this row from one the stop rule ends: the daemon's suspend is undone by the start itself, which resumes the session it kept"},
			{name: "a claim an operator suspended", root: "LEGION-630", key: "LEGION-631", stands: phase.Implementing,
				state: supervise.StateSuspended, suspendedBy: supervise.SuspendedByOperator, stoppedAfterTheRow: true, kept: true,
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
			{name: "a retired claim stopped before its owed row was written", root: "LEGION-710", key: "LEGION-711",
				stands: phase.Implementing, state: supervise.StateRetired, kept: true,
				why: "this row changes the claim's mark alone, leaving it below the owed row: a row written after the stop is the workflow's next start of this worker — the re-admission's, the held phase's retry — and ending it would take the issue out of the waiting line with nothing left to start its phase"},
			{name: "a retired claim holding an unconfirmed task of the row's own run", root: "LEGION-720", key: "LEGION-721",
				stands: phase.Implementing, state: supervise.StateRetired, stoppedAfterTheRow: true, holdsTask: true, kept: true,
				why: "this row changes the task the claim holds alone, giving it this row's own generation and phase unconfirmed: the claim kept the work of this very row, so the row is not a start the stop outlived"},
			{name: "a retired claim whose owed row stood in the outbox when it was stopped", root: "LEGION-730", key: "LEGION-731",
				stands: phase.Implementing, state: supervise.StateRetired, stoppedAfterTheRow: true,
				why: "all three terms hold: the operator stopped the worker, the row was already in the outbox when they did, and the claim works no task of this row, so the row's only effect when a slot came would be to start the stopped worker"},
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
// wrote, which is the whole of what the repair of a start that can no longer act writes, and
// with it the place in the waiting line that row is what gives the issue: the line's first part
// is the unslotted issues that owe their worker a start (record.Waiting), so a row the repair
// ends takes its issue out of the line and a row it keeps leaves the issue standing in it,
// waiting for the slot that will run the row.
func assertOwedAfterThePass(t *testing.T, pool *pgxpool.Pool, row owedRepairRow) {
	t.Helper()
	owed := owedStarts(t, pool, row.key)
	line := waitingLine(t, pool, testProject, nil)
	if row.kept {
		if len(owed) != 1 {
			t.Fatalf("the starts %s owes after the pass = %+v, want the one its fixture wrote: %s", row.key, owed, row.why)
		}
		if !slices.Contains(line, row.key) {
			t.Fatalf("the waiting line after the pass = %v, want %s in it: it holds no slot and owes its worker a start: %s", line, row.key, row.why)
		}
		return
	}
	if len(owed) != 0 {
		t.Fatalf("the starts %s owes after the pass = %+v, want none: %s", row.key, owed, row.why)
	}
	if slices.Contains(line, row.key) {
		t.Fatalf("the waiting line after the pass = %v, want %s out of it: the repair ended the row that put it there: %s", line, row.key, row.why)
	}
}

// retriedTask stands for the task the operator's retry hands the worker (workflow's retryPhase,
// through restart). Its wording is the workflow's and no part of what this reads: what matters
// is that it is the later task, so a row carrying it is a row the retry wrote.
const retriedTask = "Carry on with the phase. The operator retried this phase's worker."

// theOwedStart is the one start key's issue owes, read the way the pass reads it
// (record.Store.OwedStarts), so what this follows is the row the stop rule weighs and the fields
// it weighs it by.
func theOwedStart(t *testing.T, pool *pgxpool.Pool, key string) record.OwedStart {
	t.Helper()
	var found []record.OwedStart
	inTx(t, pool, func(tx pgx.Tx) {
		owed, err := record.NewStore().OwedStarts(context.Background(), tx, testProject)
		if err != nil {
			t.Fatalf("list the starts %s owes its workers: %v", testProject, err)
		}
		for _, start := range owed {
			if start.Issue == key {
				found = append(found, start)
			}
		}
	})
	if len(found) != 1 {
		t.Fatalf("the starts %s owes = %+v, want the one its fixture wrote", key, found)
	}
	return found[0]
}

// An operator's retry of a worker they stopped, written before any admission pass has run,
// leaves the issue owing the start the retry wrote, and the pass keeps that row.
//
// It is the case the stop rule must tell apart by the write and never by the row's place. `legion
// claims stop` hands the machine RequestStop and applies no fact, so no pass runs between the stop
// and what follows and the mark the retirement takes is above the row the issue already owes. The
// retry then reaches the workflow (api's retryStart, restartWorkedPhase, intake.RestartPhase,
// workflow's restart and started), whose one write is StartOrOwe's, and that takes out the row the
// issue already owes and writes its own in its place (record.Store.EnqueueStart): the row written
// keeps the created_at that holds the issue's place in the waiting line, and takes a write mark of
// its own. Weighed by that created_at, the retry's start would read as a row from before the
// stop and the pass would end it — which costs the issue the line with nothing left to start
// its phase, the repair of a slot passing over a retired claim on purpose
// (resumeSlottedWorkers) and the retry having already answered its 202.
//
// The fixture is the third group's own stopped worker, with the retry's write between the stop
// and the pass.
func TestTheRetryOfAStoppedWorkerKeepsTheStartItWrote(t *testing.T) {
	row := owedRepairRow{root: "LEGION-810", key: "LEGION-811", stands: phase.Implementing,
		state: supervise.StateRetired, stoppedAfterTheRow: true, kept: true,
		why: "the retry wrote this row after the stop, through the row the issue already owed, so it is the workflow's own next start of this worker"}
	pool := migratedPool(t)
	// One slot, taken, so the pass promotes nothing and the issue stands where the retry left
	// it: owing a start, and in the line for the slot that will run it.
	capFiller(t, pool, "LEGION-800", "LEGION-801")
	seedOwedRepairRow(t, pool, row)
	stopped := theOwedStart(t, pool, row.key)

	// The retry's own write, before any pass. It is of the entry the role's phases row already
	// names, as restart is, and carries the task of the phase the worker is to work.
	retry := record.SuperviseRequest{Op: "start", Tree: row.root, Role: phase.RoleFor(row.stands),
		Generation: 1, Phase: row.stands, Task: retriedTask, Entry: seededEntry}
	inTx(t, pool, func(tx pgx.Tx) {
		if err := workflow.StartOrOwe(context.Background(), tx, record.NewStore(), row.key, retry, fixedNow); err != nil {
			t.Fatalf("write the operator's retry of %s through the workflow's own writer: %v", row.key, err)
		}
	})
	rewritten := theOwedStart(t, pool, row.key)
	if !rewritten.CreatedAt.Equal(stopped.CreatedAt) {
		t.Fatalf("after the retry %s owes a row created at %s, want the %s the row it already owed was created at: the replacement keeps the issue its place in the waiting line",
			row.key, rewritten.CreatedAt, stopped.CreatedAt)
	}
	// Reported rather than fatal: a build whose rewrite takes no mark of its own goes on to the
	// pass below and reads what that costs, which is the whole point of the case.
	if rewritten.WriteMark <= stopped.WriteMark {
		t.Errorf("the retry's rewrite of %s's row is marked at write %d, want one above the %d it carried when the stop was marked above it: the rewrite is a write of its own",
			row.key, rewritten.WriteMark, stopped.WriteMark)
	}
	if owed := owedStarts(t, pool, row.key); len(owed) != 1 || owed[0].task != retriedTask {
		t.Fatalf("the starts %s owes after the retry = %+v, want the one row carrying the retry's task: the retry replaces the payload of the row the issue owes", row.key, owed)
	}

	tick(t, pool, quiet(t, 1), "tick")

	assertOwedAfterThePass(t, pool, row)
	if owed := owedStarts(t, pool, row.key); len(owed) != 1 || owed[0].task != retriedTask {
		t.Fatalf("the starts %s owes after the pass = %+v, want the retry's own row, task and all: %s", row.key, owed, row.why)
	}
}

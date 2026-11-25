package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/credential"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// entryStartRow is one supervise start row with the request it carries, so a case reads the phase
// entry the row is of (record.SuperviseRequest.Entry) beside the task on it.
type entryStartRow struct {
	row     record.OutboxRow
	request record.SuperviseRequest
}

// entryStartRows is every supervise start row issue holds, oldest first, each with its request.
// The daemon writes a start as a message and rewrites none, so a role can hold the row of an
// entry it has left beside the row of the entry its phases row names, and a case reads both here.
func entryStartRows(t *testing.T, pool *store.Pool, issue string) []entryStartRow {
	t.Helper()
	rows, err := pool.Query(context.Background(), `select id, payload from outbox
		where issue = $1 and kind = 'supervise' and payload->>'op' = 'start' order by id`, issue)
	if err != nil {
		t.Fatalf("read the start rows of %s: %v", issue, err)
	}
	defer rows.Close()
	var found []entryStartRow
	for rows.Next() {
		var start entryStartRow
		var payload []byte
		if err := rows.Scan(&start.row.ID, &payload); err != nil {
			t.Fatalf("read the start rows of %s: %v", issue, err)
		}
		start.row.Issue, start.row.Kind, start.row.Payload = issue, record.OutboxKindSupervise, json.RawMessage(payload)
		start.row.NextAt = time.Now()
		decoded, err := record.DecodeOutboxPayload(start.row)
		if err != nil {
			t.Fatalf("decode start row %d of %s: %v", start.row.ID, issue, err)
		}
		request, ok := decoded.(record.SuperviseRequest)
		if !ok {
			t.Fatalf("start row %d of %s is %T, want a supervise request", start.row.ID, issue, decoded)
		}
		start.request = request
		found = append(found, start)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the start rows of %s: %v", issue, err)
	}
	return found
}

// roleStartRows is every start row of issue entryStartRows reads that starts role, oldest first.
func roleStartRows(t *testing.T, pool *store.Pool, issue string, role claim.Role) []entryStartRow {
	t.Helper()
	var found []entryStartRow
	for _, start := range entryStartRows(t, pool, issue) {
		if start.request.Role == role {
			found = append(found, start)
		}
	}
	return found
}

// rolePhaseRow is role's phases row of issue: the record the entry a phase is on and that entry's
// task are read from (record.PhaseRow.Entry and EntryTask), and the zero row for a role the
// record holds none of.
func rolePhaseRow(t *testing.T, pool *store.Pool, issue string, role claim.Role) record.PhaseRow {
	t.Helper()
	ctx := context.Background()
	var found record.PhaseRow
	if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := record.NewStore().Phases(ctx, tx, issue)
		if err != nil {
			return err
		}
		for _, row := range rows {
			if row.Role == role {
				found = row
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("read the %s's phases row of %s: %v", role, issue, err)
	}
	return found
}

// entryEngine is a workflow engine over the record, built as newOneStartRig builds the one whose
// transition wrote the start these cases run, so a case can raise a later entry of the phase or
// send the role the work its entry names.
func entryEngine(t *testing.T) *workflow.Engine {
	t.Helper()
	return workflow.New(record.NewStore(), workflow.Config{Project: "LEGION", Repositories: testRepositories,
		Steps: config.DesignGateRootIssues.Steps()}, quietLogger())
}

// startPromoted raises role's entry on issue as the promotion of a re-admitted tree does
// (workflow.Engine.StartPromotedWorker), in a transaction of its own that settles the runs its
// start ends, as a fact's does (workflow.Engine.OpenFact).
func startPromoted(ctx context.Context, db store.TxPool, engine *workflow.Engine, issue record.Issue, role claim.Role) error {
	ctx = engine.OpenFact(ctx)
	return store.Transact(ctx, db, func(ctx context.Context, tx pgx.Tx) error {
		if err := engine.StartPromotedWorker(ctx, tx, issue, role); err != nil {
			return err
		}
		return engine.SettleFact(ctx, tx)
	})
}

// A start row carries the phase entry its task is of (record.SuperviseRequest.Entry), and the
// role's phases row names the entry its phase is on now (record.PhaseRow.Entry). A row below that
// number carries work a later entry of the same phase has superseded, and that entry's own start
// carries that entry's task, so the executor finishes the superseded row without delivering
// anything and without starting the role: running it would hand the worker work the record has
// moved past.
//
// That fence is what lets a start row be a message with no state of its own. The two rows stand
// together exactly when a lane holds the earlier one: EnqueueStart writes over the start an issue
// owes while it is unleased and leaves a leased one where it is, so the row the lane holds
// outlives its entry rather than being rewritten under the lane.
func TestAnAppendOnlyStartRowBelowItsRolesEntryDeliversNothingAndStartsNoRole(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rig := newOneStartRig(t)
	rows := entryStartRows(t, rig.pool, rig.issue.Key)
	if len(rows) != 1 || rows[0].request.Role != claim.RoleImplementer || rows[0].request.Entry != 1 {
		t.Fatalf("the start rows after the planner's completion = %+v, want the implementer's one row of entry 1", rows)
	}
	first := rows[0]
	if _, err := rig.pool.Exec(ctx, `update outbox set lease_token = 'a-lane-that-holds-the-row',
		lease_until = now() + interval '1 minute' where id = $1`, first.row.ID); err != nil {
		t.Fatalf("lease the implementer's start row as the lane does: %v", err)
	}

	// The promotion of a re-admitted tree's member is a later entry of the same phase: it raises
	// the number, writes that entry's task on the phases row and writes its own start
	// (workflow.Engine.StartPromotedWorker).
	issue := readIssue(t, rig.pool, rig.issue.Key)
	if err := startPromoted(ctx, rig.pool, entryEngine(t), *issue, claim.RoleImplementer); err != nil {
		t.Fatalf("raise the implementer's second entry: %v", err)
	}
	rows = entryStartRows(t, rig.pool, rig.issue.Key)
	if len(rows) != 2 || rows[0].row.ID != first.row.ID || rows[1].request.Entry != 2 {
		t.Fatalf("the start rows after the promotion = %+v, want the leased row of entry 1 and a second row of entry 2", rows)
	}
	second := rows[1]
	if row := rolePhaseRow(t, rig.pool, rig.issue.Key, claim.RoleImplementer); row.Entry != 2 || row.EntryTask != second.request.Task {
		t.Fatalf("the implementer's phases row = entry %d with task %q, want entry 2 with the second start's task %q",
			row.Entry, row.EntryTask, second.request.Task)
	}

	if err := rig.runner.execute(ctx, first.row); err != nil {
		t.Fatalf("run the start row of the entry the record has left: %v", err)
	}

	if machine, found := rig.sup.Machine(rig.token); found {
		t.Fatalf("the row of entry 1 left the implementer's claim %+v, want none: a row below its role's entry starts no role",
			machine.Claim())
	}
	if *rig.provisioned != 0 || len(rig.rt.CallsOf("Spawn")) != 0 {
		t.Fatalf("the row of entry 1 provisioned %d workspaces and made %d spawns, want none of either: it delivers nothing and starts no role",
			*rig.provisioned, len(rig.rt.CallsOf("Spawn")))
	}

	// The row of the entry the record names does the start the superseded row did not, which is
	// what says the fence read the entry and not something else about the issue.
	if err := rig.runner.execute(ctx, second.row); err != nil {
		t.Fatalf("run the start row of entry 2: %v", err)
	}
	machine, found := rig.sup.Machine(rig.token)
	if !found {
		t.Fatal("the start row of entry 2 created no claim for the implementer")
	}
	machine.Wait()
	got := machine.Claim()
	if got.Pending == nil || got.Pending.Entry != 2 || got.Pending.Task != second.request.Task || got.GivenEntry != 2 {
		t.Fatalf("the claim after the start row of entry 2 = %+v, want it queued entry 2's task %q and given entry 2",
			got, second.request.Task)
	}
}

// A start row is a message, and the work it carries is a fact of the record: the transition wrote
// the entry and that entry's task on the role's phases row in its own transaction
// (record.PhaseRow.Entry and EntryTask). A row deleted from the outbox before a lane runs it
// therefore loses no work — the daemon writes that entry's own task again from the record alone
// (workflow.Engine.SendOwedEntry), and the worker is given what the deleted row carried, at the
// entry it carried.
func TestAppendOnlyStartRowsLoseNoWorkWhenOneIsDeletedBeforeTheLaneRunsIt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rig := newOneStartRig(t)
	rows := entryStartRows(t, rig.pool, rig.issue.Key)
	if len(rows) != 1 || rows[0].request.Role != claim.RoleImplementer || rows[0].request.Entry != 1 {
		t.Fatalf("the start rows after the planner's completion = %+v, want the implementer's one row of entry 1", rows)
	}
	deleted := rows[0]
	if _, err := rig.pool.Exec(ctx, "delete from outbox where id = $1", deleted.row.ID); err != nil {
		t.Fatalf("delete the implementer's start row: %v", err)
	}
	if left := entryStartRows(t, rig.pool, rig.issue.Key); len(left) != 0 {
		t.Fatalf("the start rows after the delete = %+v, want none: the case's own premise", left)
	}

	row := rolePhaseRow(t, rig.pool, rig.issue.Key, claim.RoleImplementer)
	if row.Entry != deleted.request.Entry || row.EntryTask != deleted.request.Task {
		t.Fatalf("the implementer's phases row = entry %d with task %q, want entry %d with the deleted row's task %q: the work is the record's, not the row's",
			row.Entry, row.EntryTask, deleted.request.Entry, deleted.request.Task)
	}

	issue := readIssue(t, rig.pool, rig.issue.Key)
	if err := store.Transact(ctx, rig.pool, func(ctx context.Context, tx pgx.Tx) error {
		return entryEngine(t).SendOwedEntry(ctx, tx, *issue, claim.RoleImplementer, row)
	}); err != nil {
		t.Fatalf("send the implementer the work its entry names: %v", err)
	}
	again := entryStartRows(t, rig.pool, rig.issue.Key)
	if len(again) != 1 || again[0].request.Entry != deleted.request.Entry || again[0].request.Task != deleted.request.Task {
		t.Fatalf("the start rows the record wrote again = %+v, want one of entry %d carrying the task %q the deleted row carried",
			again, deleted.request.Entry, deleted.request.Task)
	}

	if err := rig.runner.execute(ctx, again[0].row); err != nil {
		t.Fatalf("run the start row written from the record: %v", err)
	}
	machine, found := rig.sup.Machine(rig.token)
	if !found {
		t.Fatal("the start row written from the record created no claim for the implementer")
	}
	machine.Wait()
	got := machine.Claim()
	if got.Pending == nil || got.Pending.Task != deleted.request.Task || got.Pending.Entry != deleted.request.Entry {
		t.Fatalf("the claim after the start = %+v, want it queued the task %q of entry %d, which the deleted row carried",
			got, deleted.request.Task, deleted.request.Entry)
	}
}

// A start whose claim already holds a task of its phase and run that the worker will work hands
// over no second prompt (keepsStartTask). The entry such a start records as given on the claim
// (supervise.Machine.EntryGiven) is the entry of the task the claim is holding, never the entry
// the row carried: the claim was given the work it holds and no other. The two cases below are
// the two a held-back start meets.
//
// The sequence each stands in is the one the record writes: a start queues an entry's task, and
// the promotion of a re-admitted tree's member raises the next entry with the phase's resume task
// (workflow.Engine.StartPromotedWorker, record.SuperviseRequest.ResumeTask), which a worker
// already at work needs no second prompt for.
func TestAnAppendOnlyStartThatDeliversNothingRecordsOnlyTheEntryItsClaimHolds(t *testing.T) {
	t.Parallel()
	t.Run("a start of the entry whose task the claim holds records that entry", func(t *testing.T) {
		ctx := context.Background()
		rig := newOneStartRig(t)
		machine, _, err := rig.sup.Create(ctx, supervise.Claim{Token: rig.token, Project: "legion", Tree: rig.issue.Tree,
			Issue: rig.issue.Key, Role: claim.RoleImplementer, State: supervise.StateReady, Generation: 1,
			Session: "ses-implementer", SessionFile: "/tmp/implementer.jsonl"}, "")
		if err != nil {
			t.Fatalf("create the implementer claim: %v", err)
		}

		// The promotion raises entry 2 with the phase's resume task. The claim holds nothing, so
		// the row that carries that task queues it, and the delivery records entry 2 as given
		// (supervise's queue, which writes Claim.GivenEntry with the delivery).
		issue := readIssue(t, rig.pool, rig.issue.Key)
		if err := startPromoted(ctx, rig.pool, entryEngine(t), *issue, claim.RoleImplementer); err != nil {
			t.Fatalf("raise the implementer's second entry: %v", err)
		}
		rows := entryStartRows(t, rig.pool, rig.issue.Key)
		if len(rows) != 1 || rows[0].request.Entry != 2 || !rows[0].request.ResumeTask {
			t.Fatalf("the start rows after the promotion = %+v, want the one row of entry 2 marked as the phase's resume task: the promotion's row is written over the unleased start the issue owed (record.Store.EnqueueStart)",
				rows)
		}
		promoted := rows[0]
		if err := rig.runner.execute(ctx, promoted.row); err != nil {
			t.Fatalf("run the start row of entry 2: %v", err)
		}
		machine.Wait()
		held := machine.Claim().Pending
		if held == nil || held.Entry != 2 || machine.Claim().GivenEntry != 2 {
			t.Fatalf("the claim after the start row of entry 2 = %+v, want it holding entry 2's task and given entry 2: the case's own premise",
				machine.Claim())
		}

		// The same row runs again, as a row whose run was cut off before it finished does, and
		// finds the claim holding the very task it carries.
		if err := rig.runner.execute(ctx, promoted.row); err != nil {
			t.Fatalf("run the start row of entry 2 a second time: %v", err)
		}
		machine.Wait()

		got := machine.Claim()
		if got.Pending == nil || got.Pending.ID != held.ID || got.Pending.Task != held.Task || got.Pending.Entry != 2 {
			t.Fatalf("the pending delivery after the row ran again = %+v, want the entry 2 task %q of delivery %s the claim holds: the start hands no second prompt",
				got.Pending, held.Task, held.ID)
		}
		if row := rolePhaseRow(t, rig.pool, rig.issue.Key, claim.RoleImplementer); got.GivenEntry != 2 || row.Entry != 2 {
			t.Fatalf("the claim was given entry %d under a phases row at entry %d, want 2 under 2: the entry of the task the claim holds is the entry recorded as given, so the role is owed nothing",
				got.GivenEntry, row.Entry)
		}
	})

	t.Run("a start of a later entry, whose task the claim is still working, records nothing", func(t *testing.T) {
		ctx := context.Background()
		rig := newOneStartRig(t)
		machine, _, err := rig.sup.Create(ctx, supervise.Claim{Token: rig.token, Project: "legion", Tree: rig.issue.Tree,
			Issue: rig.issue.Key, Role: claim.RoleImplementer, State: supervise.StateReady, Generation: 1,
			Session: "ses-implementer", SessionFile: "/tmp/implementer.jsonl"}, "")
		if err != nil {
			t.Fatalf("create the implementer claim: %v", err)
		}
		rows := entryStartRows(t, rig.pool, rig.issue.Key)
		if len(rows) != 1 || rows[0].request.Entry != 1 {
			t.Fatalf("the start rows after the planner's completion = %+v, want the implementer's one row of entry 1", rows)
		}
		first := rows[0]
		if _, err := rig.pool.Exec(ctx, `update outbox set lease_token = 'a-lane-that-holds-the-row',
		lease_until = now() + interval '1 minute' where id = $1`, first.row.ID); err != nil {
			t.Fatalf("lease the implementer's start row as the lane does: %v", err)
		}
		if err := rig.runner.execute(ctx, first.row); err != nil {
			t.Fatalf("run the start row of entry 1: %v", err)
		}
		machine.Wait()
		if got := machine.Claim(); got.Pending == nil || got.Pending.Entry != 1 || got.GivenEntry != 1 {
			t.Fatalf("the claim after the start row of entry 1 = %+v, want it holding entry 1's task and given entry 1: the case's own premise", got)
		}

		issue := readIssue(t, rig.pool, rig.issue.Key)
		if err := startPromoted(ctx, rig.pool, entryEngine(t), *issue, claim.RoleImplementer); err != nil {
			t.Fatalf("raise the implementer's second entry: %v", err)
		}
		rows = entryStartRows(t, rig.pool, rig.issue.Key)
		if len(rows) != 2 || rows[1].request.Entry != 2 || !rows[1].request.ResumeTask {
			t.Fatalf("the start rows after the promotion = %+v, want a second row of entry 2 marked as the phase's resume task", rows)
		}
		second := rows[1]
		held := machine.Claim().Pending

		if err := rig.runner.execute(ctx, second.row); err != nil {
			t.Fatalf("run the start row of entry 2: %v", err)
		}
		machine.Wait()

		got := machine.Claim()
		if got.Pending == nil || got.Pending.ID != held.ID || got.Pending.Task != held.Task || got.Pending.Entry != 1 {
			t.Fatalf("the pending delivery after the start row of entry 2 = %+v, want the entry 1 task %q of delivery %s the worker is working: the start hands no second prompt",
				got.Pending, held.Task, held.ID)
		}
		if row := rolePhaseRow(t, rig.pool, rig.issue.Key, claim.RoleImplementer); got.GivenEntry != 1 || row.Entry != 2 {
			t.Fatalf("the claim was given entry %d under a phases row at entry %d, want 1 under 2: the claim holds entry 1's task, so the start of entry 2 records nothing and the claim stands below its row, owed that entry's work (workflow.OwedEntry)",
				got.GivenEntry, row.Entry)
		}
	})
}

// A new run leaves the role's entry where it stood and takes the work of it away: the task is the
// ended generation's and goes with that generation's handoffs (record.ClearGeneration, which
// workflow's NewRun runs), while the number stays, because it counts the entries the role has been
// handed on this issue and nothing of a new run unhands one (record.PhaseRow.Entry and EntryTask).
//
// What writes the work again is the promotion of the issue
// (workflow.Engine.StartPromotedWorker): it raises the entry, writes that entry's task beside the
// number in the same transaction and writes the start that carries both, so the worker is started
// under an entry above every entry a completion still in flight could name.
func TestAReadmissionLeavesTheEntryColumnsAndThePromotionWritesTheWorkAgain(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rig := newOneStartRig(t)
	rows := entryStartRows(t, rig.pool, rig.issue.Key)
	if len(rows) != 1 || rows[0].request.Role != claim.RoleImplementer || rows[0].request.Entry != 1 {
		t.Fatalf("the start rows after the planner's completion = %+v, want the implementer's one row of entry 1", rows)
	}
	before := rolePhaseRow(t, rig.pool, rig.issue.Key, claim.RoleImplementer)
	if before.Entry != 1 || before.EntryTask != rows[0].request.Task || before.EntryRaisedAt.IsZero() {
		t.Fatalf("the implementer's phases row = entry %d raised at %s with task %q, want entry 1 carrying the start's task %q: the case's own premise",
			before.Entry, before.EntryRaisedAt, before.EntryTask, rows[0].request.Task)
	}

	if err := store.Transact(ctx, rig.pool, func(ctx context.Context, tx pgx.Tx) error {
		return record.NewStore().ClearGeneration(ctx, tx, rig.issue.Key, time.Now().UTC())
	}); err != nil {
		t.Fatalf("clear the generation the re-admission ends: %v", err)
	}

	readmitted := rolePhaseRow(t, rig.pool, rig.issue.Key, claim.RoleImplementer)
	if readmitted.Entry != before.Entry || !readmitted.EntryRaisedAt.Equal(before.EntryRaisedAt) {
		t.Fatalf("the implementer's phases row after the re-admission = entry %d raised at %s, want the entry %d raised at %s it stood on",
			readmitted.Entry, readmitted.EntryRaisedAt, before.Entry, before.EntryRaisedAt)
	}
	if readmitted.EntryTask != "" {
		t.Fatalf("the implementer's phases row still names the work %q of the generation that ended, want none: its promotion writes the task it is given",
			readmitted.EntryTask)
	}

	issue := readIssue(t, rig.pool, rig.issue.Key)
	if err := startPromoted(ctx, rig.pool, entryEngine(t), *issue, claim.RoleImplementer); err != nil {
		t.Fatalf("promote the re-admitted tree's implementer: %v", err)
	}

	promoted := rolePhaseRow(t, rig.pool, rig.issue.Key, claim.RoleImplementer)
	if promoted.Entry != before.Entry+1 || promoted.EntryTask == "" {
		t.Fatalf("the implementer's phases row after the promotion = entry %d with task %q, want entry %d carrying the work it is owed",
			promoted.Entry, promoted.EntryTask, before.Entry+1)
	}
	// The promotion's start takes out the start the issue still owed, whose task the re-admission
	// cleared, and writes its own in its place (record.Store.EnqueueStart), so one start stands
	// and it is the one the phases row names.
	rows = entryStartRows(t, rig.pool, rig.issue.Key)
	if len(rows) != 1 || rows[0].request.Entry != promoted.Entry || rows[0].request.Task != promoted.EntryTask {
		t.Fatalf("the start rows after the promotion = %+v, want the one row of entry %d carrying the task %q its phases row names",
			rows, promoted.Entry, promoted.EntryTask)
	}
}

// A retry of the phase its worker stands in hands that worker the entry its phases row already
// names, with a task of its own that names the retry's reason: the held phase's retry, by any
// route into it (workflow.Engine.restart). The claim it reaches here is the one the entry alone
// cannot decide: its worker confirmed the entry's last task and the turn that worked it ended,
// so the claim holds that entry as its given one (supervise.Claim.GivenEntry) with no delivery
// pending and no outbox row of the retry recorded against it. The start says it resends that
// entry (record.SuperviseRequest.ResendEntry), and the task reaches the worker; without the
// mark the claim would be relaunched with no prompt.
func TestAHeldPhasesRetryReachesAWorkerThatFinishedTheEntrysTask(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rig := newOneStartRig(t)
	// The worker's own connection, as the integration cases register one: a prompt reaches no
	// claim without it, and this case is about the task reaching the worker.
	rig.sup.deps.Conns.(*fake.Conns).Register(rig.token, fake.NewConn())
	rows := entryStartRows(t, rig.pool, rig.issue.Key)
	if len(rows) != 1 || rows[0].request.Role != claim.RoleImplementer || rows[0].request.Entry != 1 {
		t.Fatalf("the start rows after the planner's completion = %+v, want the implementer's one row of entry 1", rows)
	}
	if err := rig.runner.execute(ctx, rows[0].row); err != nil {
		t.Fatalf("run the implementer's start row: %v", err)
	}
	machine, found := rig.sup.Machine(rig.token)
	if !found {
		t.Fatal("the start row created no claim for the implementer")
	}
	machine.Wait()
	generation := machine.Claim().Generation
	for _, ev := range []supervise.Event{
		supervise.StreamHello{Claim: rig.token, Generation: generation},
		supervise.RequestRegister{Claim: rig.token, Generation: generation, Session: "ses-impl", SessionFile: "/tmp/impl.jsonl"},
		supervise.RequestReady{Claim: rig.token, Generation: generation, Session: "ses-impl"},
	} {
		if err := machine.Handle(ctx, ev); err != nil {
			t.Fatalf("handle %T: %v", ev, err)
		}
	}
	testwait.Eventually(t, "entry 1's task to be acknowledged", func() bool {
		p := machine.Claim().Pending
		return p != nil && !p.DeliveredAt.IsZero()
	})
	worked := machine.Claim().Pending.ID
	for _, ev := range []supervise.Event{
		supervise.StreamTurnStart{Claim: rig.token, DeliveryID: worked},
		supervise.StreamPromptResult{Claim: rig.token, DeliveryID: worked, AgentInvoked: true},
		supervise.StreamTurnEnd{Claim: rig.token},
	} {
		if err := machine.Handle(ctx, ev); err != nil {
			t.Fatalf("handle %T: %v", ev, err)
		}
	}
	if got := machine.Claim(); got.Pending != nil || got.GivenEntry != 1 || got.ServedEntry != 1 {
		t.Fatalf("the claim after the turn = %+v, want it holding no task, given entry 1 and serving entry 1: the case's own premise", got)
	}

	applyFact(t, rig.pool, "api", "retry-the-implementer",
		intake.RestartPhase{Issue: rig.issue.Key, Role: claim.RoleImplementer}, entryEngine(t))

	// The row that started the worker finished when it ran, so the retry's is the one row left.
	rows = entryStartRows(t, rig.pool, rig.issue.Key)
	if len(rows) != 1 || rows[0].request.Entry != 1 || !rows[0].request.ResendEntry || rows[0].request.Task == "" {
		t.Fatalf("the start rows after the retry = %+v, want one row of entry 1, marked a resend and carrying the retry's own task", rows)
	}
	retry := rows[0]
	if row := rolePhaseRow(t, rig.pool, rig.issue.Key, claim.RoleImplementer); row.Entry != 1 {
		t.Fatalf("the implementer's phases row = entry %d, want the entry 1 it stood on: a retry of a phase the issue never left raises none", row.Entry)
	}

	if err := rig.runner.execute(ctx, retry.row); err != nil {
		t.Fatalf("run the retry's start row: %v", err)
	}
	machine.Wait()

	got := machine.Claim()
	if got.Pending == nil || got.Pending.Task != retry.request.Task || got.Pending.Entry != 1 {
		t.Fatalf("the claim after the retry = %+v, want it queued the retry's own task %q of entry 1: the worker is relaunched with the work, not with nothing",
			got, retry.request.Task)
	}
}

// The whole way out of a phase entered again under a worker at work, on this package's own rigs:
// the claim holds the earlier entry's task, nothing records the later entry on it, the completion
// its worker reports for the task it holds is refused with nothing recorded, and the daemon sends
// the entry its phases row names of its own accord once that task is retired.
//
// The sequence is the record's. The implementer's completion hands testing to the tester, whose
// phases row is raised to entry 1 with that entry's task and whose claim takes it and begins a
// turn on it. The promotion of a re-admitted tree's member then raises entry 2 with the phase's
// resume task (workflow.Engine.StartPromotedWorker): the runner holds that task back, since the
// claim is working entry 1's (keepsStartTask), and records nothing, so the claim stands below its
// row.
//
// What the claim standing below its row then gives the worker is the rest of this case. The
// tester's completion of entry 1 is refused 409 HANDOFF_STALE_ENTRY with nothing recorded,
// saying the task of entry 2, the entry the phase is on, is owed to this claim and arrives by
// itself (api/handoff.go's staleEntry). The admission pass passes over the claim while that task
// is still worked, and on the first pass after the turn ends and the delivery is retired it sends
// entry 2's own task (admit's resumeSlottedWorkers, workflow's SendOwedEntry). The worker takes
// that task, and the completion it reports in its turn is the one the daemon takes: the issue
// moves to reviewing.
//
// The supervisor writes its claims to the rig's own database here rather than to the outbox
// tests' in-memory claim store, because what the admission pass weighs against the phases row is
// the claim the record holds (record.RoleClaim.GivenEntry, workflow.OwedEntry).
func TestAClaimWorkingAnEarlierEntryIsSentItsRowsEntryOnceThatTaskIsRetired(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rig := newOneStartRig(t)
	// The supervisor's own store, over the rig's database, with the issue's tree admitted as the
	// promotion admits one: a claim is bound to the work of a tree the lifecycle holds open.
	rig.sup.deps.Store = outboxTreeStore(t, rig.pool, rig.issue.Tree)
	holdSlot(t, rig.pool, rig.issue.Key)
	records := record.NewStore()
	engine := entryEngine(t)
	// The copy of Dispatch's issues holds the root, so its chain is known and the admission pass
	// reads it as a running gate root (R16 of dispatch://INFRA-3320).
	if err := store.Transact(ctx, rig.pool, func(ctx context.Context, tx pgx.Tx) error {
		seedCopyRow(t, tx, rig.issue.Key, "", rig.issue.Status, 1)
		return nil
	}); err != nil {
		t.Fatalf("fill the copy of Dispatch's issues: %v", err)
	}

	// The implementer's completion is held to an open pull request on the issue's branch
	// (workflow's TriggerImplementationReady), which the record holds here as the opened webhook
	// records one.
	if err := store.Transact(ctx, rig.pool, func(ctx context.Context, tx pgx.Tx) error {
		return records.PutPullRequest(ctx, tx, record.PullRequest{State: record.PullRequestOpen, Issue: rig.issue.Key,
			Repo: "acme/widgets", Number: 42, Branch: "legion/" + rig.issue.Key, HeadSHA: "head", CheckedHead: "head",
			Failing: []string{}})
	}); err != nil {
		t.Fatalf("record the pull request the implementer's completion is held to: %v", err)
	}
	implementer := rolePhaseRow(t, rig.pool, rig.issue.Key, claim.RoleImplementer)
	applyFact(t, rig.pool, "api", "implement-complete", intake.HandoffComplete{Generation: 1, Issue: rig.issue.Key,
		Role: claim.RoleImplementer, Claim: implementer.Claim, Summary: "implemented", Commit: "head",
		HandoffRecord: "implement", HandoffVersion: 1}, engine)
	if issue := readIssue(t, rig.pool, rig.issue.Key); issue.Phase != phase.Testing {
		t.Fatalf("the issue stands at %s after the implementer's completion, want testing: the case's own premise", issue.Phase)
	}

	tester, err := claim.NewToken("legion", rig.issue.Key, claim.RoleTester)
	if err != nil {
		t.Fatal(err)
	}
	// The worker's own connection, as the integration cases register one: a prompt reaches no
	// claim without it, and this case is about the tasks that reach the worker.
	rig.sup.deps.Conns.(*fake.Conns).Register(tester, fake.NewConn())
	testerStarts := func() []entryStartRow {
		t.Helper()
		return roleStartRows(t, rig.pool, rig.issue.Key, claim.RoleTester)
	}
	testerStart := func(entry int64) entryStartRow {
		t.Helper()
		starts := testerStarts()
		if len(starts) != 1 || starts[0].request.Entry != entry {
			t.Fatalf("the tester's start rows = %+v, want the one row of entry %d", starts, entry)
		}
		return starts[0]
	}
	givenEntry := func() int64 {
		t.Helper()
		var given int64
		if err := rig.pool.QueryRow(ctx, "select given_entry from claims where token = $1", string(tester)).Scan(&given); err != nil {
			t.Fatalf("read the entry the record holds the tester's claim as given: %v", err)
		}
		return given
	}
	// run is one lane's run of a start row: the executor's work, and then the delete of the row by
	// id that the lane's finish makes (record.Store.FinishOutbox). What the outbox holds afterwards
	// is what the daemon still owes, which is what the rule weighing a role's entry reads beside
	// its claim (workflow.OwedEntry).
	run := func(what string, start entryStartRow) {
		t.Helper()
		if err := rig.runner.execute(ctx, start.row); err != nil {
			t.Fatalf("run %s: %v", what, err)
		}
		if _, err := rig.pool.Exec(ctx, "delete from outbox where id = $1", start.row.ID); err != nil {
			t.Fatalf("finish %s: %v", what, err)
		}
	}

	// The tester takes entry 1's task and begins its turn on it.
	run("the tester's start row of entry 1", testerStart(1))
	machine, found := rig.sup.Machine(tester)
	if !found {
		t.Fatal("the tester's start row created no claim")
	}
	machine.Wait()
	generation := machine.Claim().Generation
	for _, ev := range []supervise.Event{
		supervise.StreamHello{Claim: tester, Generation: generation},
		supervise.RequestRegister{Claim: tester, Generation: generation, Session: "ses-tester", SessionFile: "/tmp/tester.jsonl",
			CapabilityHash: []byte("capability-of-ses-tester")},
		supervise.RequestReady{Claim: tester, Generation: generation, Session: "ses-tester"},
	} {
		if err := machine.Handle(ctx, ev); err != nil {
			t.Fatalf("handle %T: %v", ev, err)
		}
	}
	testwait.Eventually(t, "entry 1's task to be acknowledged", func() bool {
		p := machine.Claim().Pending
		return p != nil && !p.DeliveredAt.IsZero()
	})
	worked := machine.Claim().Pending.ID
	if err := machine.Handle(ctx, supervise.StreamTurnStart{Claim: tester, DeliveryID: worked}); err != nil {
		t.Fatalf("start the turn of entry 1's task: %v", err)
	}

	// The phase is handed out again while that turn runs: entry 2, with the phase's resume task,
	// whose row the runner holds back because the claim is working entry 1's task.
	issue := readIssue(t, rig.pool, rig.issue.Key)
	if err := startPromoted(ctx, rig.pool, engine, *issue, claim.RoleTester); err != nil {
		t.Fatalf("raise the tester's second entry: %v", err)
	}
	second := testerStart(2)
	if !second.request.ResumeTask {
		t.Fatalf("the start row of entry 2 = %+v, want it marked as the phase's resume task", second.request)
	}
	run("the start row of entry 2", second)
	machine.Wait()
	if got := machine.Claim(); got.Pending == nil || got.Pending.ID != worked || got.Pending.Entry != 1 || got.GivenEntry != 1 {
		t.Fatalf("the claim after the start row of entry 2 = %+v, want it still working entry 1's delivery %s and given entry 1", got, worked)
	}
	row := rolePhaseRow(t, rig.pool, rig.issue.Key, claim.RoleTester)
	if given := givenEntry(); given != 1 || row.Entry != 2 || row.EntryTask != second.request.Task {
		t.Fatalf("the record holds the tester's claim given entry %d under a phases row at entry %d carrying %q, want 1 under 2 carrying the promotion's task %q: the claim stands below its row, which is what the admission pass reads",
			given, row.Entry, row.EntryTask, second.request.Task)
	}

	// The completion that turn reports is of entry 1, which the phase has moved past.
	grants := credential.New(nil)
	handoffs := &fleetHandoffs{handoffs: map[string]dispatch.Handoff{}}
	handler := api.NewServer("127.0.0.1", 0, api.Options{
		Steps: config.DesignGateRootIssues.Steps(), Supervisor: rig.sup, Project: "legion", DispatchProject: "LEGION",
		Pool: rig.pool, Handlers: []intake.Handler{engine}, Record: records, Grants: grants, Dispatch: handoffs,
		Log: quietLogger(),
	}).Handler
	complete := func(handoff string, version int) (int, string) {
		t.Helper()
		grant, err := grants.Mint(machine.Claim())
		if err != nil {
			t.Fatalf("mint the grant the tester completes with: %v", err)
		}
		body, err := json.Marshal(api.HandoffCompleteRequest{GrantID: grant.ID, Summary: "the tests pass",
			Verdict: "pass", HandoffID: handoff, HandoffVersion: version})
		if err != nil {
			t.Fatal(err)
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/legion/v1/handoff/complete", bytes.NewReader(body)))
		return recorder.Code, strings.TrimSpace(recorder.Body.String())
	}
	status, refusal := complete(handoffs.write(rig.issue.Key, "test", machine.Claim().Session), 1)
	if status != http.StatusConflict {
		t.Fatalf("the completion of entry 1's task = %d %s, want 409: the phase was entered again, and this turn is on the entry before", status, refusal)
	}
	for _, want := range []string{
		"HANDOFF_STALE_ENTRY",
		"this completion is of entry 1, which is 1 behind the entry 2 the phase is on",
		"nothing was recorded",
		"The task of the entry the phase is on is owed to this claim, and the daemon sends it by itself",
	} {
		if !strings.Contains(refusal, want) {
			t.Fatalf("the refusal = %s, want it to say %q", refusal, want)
		}
	}
	if got := readIssue(t, rig.pool, rig.issue.Key); got.Phase != phase.Testing {
		t.Fatalf("the issue stands at %s after the refused completion, want testing: a refusal before the fact records nothing", got.Phase)
	}

	// The sweep is what sends the owed entry where nothing else happens in the project, and it
	// sends nothing while the claim is still working the task it holds.
	admission := admit.New(records, engine, 1, "LEGION", quietLogger())
	sweeper := &workflowRuntime{pool: rig.pool, records: records, admission: admission,
		handlers: []intake.Handler{engine, admission}, dispatchProject: "LEGION", bootID: "test-boot", log: quietLogger()}
	// Production runs every sweep on the one hold mark workflowRuntime.run gives the sweep loop
	// (store.ForConcurrentWorker(running)), reused from one sweep to the next. Each sweep here takes
	// a fresh mark of its own, which the pool's guard reads the same way, since a sweep gives back
	// every connection it took before it returns: either way the guard sees the sweep as in
	// production, rather than refusing nothing on an unmarked context (INFRA-3169).
	sweeper.applyEntrySweep(store.ForConcurrentWorker(ctx), time.Now())
	if starts := testerStarts(); len(starts) != 0 {
		t.Fatalf("the sweep wrote the tester %+v while its worker works entry 1's task, want nothing: the pass passes over a claim whose pending delivery is still worked", starts)
	}

	// The turn ends, the delivery is retired, and the next pass sends the row's own entry's task.
	endTurn(ctx, t, machine, tester)
	if got := machine.Claim(); got.Pending != nil || got.GivenEntry != 1 || got.ServedEntry != 1 {
		t.Fatalf("the claim after the turn = %+v, want its delivery retired, entry 1 given and entry 1 served", got)
	}
	sweeper.applyEntrySweep(store.ForConcurrentWorker(ctx), time.Now().Add(time.Second))
	owed := testerStart(2)
	if owed.row.ID == second.row.ID || owed.request.Task != row.EntryTask {
		t.Fatalf("the start the sweep wrote = row %d carrying %q, want a row of its own carrying the task the tester's phases row names (%q)",
			owed.row.ID, owed.request.Task, row.EntryTask)
	}

	// The worker is given that task and completes it, which is the completion the daemon takes.
	run("the start row the sweep wrote", owed)
	machine.Wait()
	if got := machine.Claim(); got.Pending == nil || got.Pending.Entry != 2 || got.Pending.Task != row.EntryTask || got.GivenEntry != 2 {
		t.Fatalf("the claim after the sweep's start row = %+v, want it queued entry 2's task and given entry 2", got)
	}
	testwait.Eventually(t, "entry 2's task to be acknowledged", func() bool {
		p := machine.Claim().Pending
		return p != nil && !p.DeliveredAt.IsZero()
	})
	if err := machine.Handle(ctx, supervise.StreamTurnStart{Claim: tester, DeliveryID: machine.Claim().Pending.ID}); err != nil {
		t.Fatalf("start the turn of entry 2's task: %v", err)
	}
	// Entry 2's task is work of its own, so the turn that works it writes a handoff record of its
	// own, which is what the refusal told the worker to do.
	own := dispatch.Handoff{ID: "handoff-" + rig.issue.Key + "-test-v2", IssueKey: rig.issue.Key, Phase: "test",
		Version: 2, Author: dispatch.HandoffAuthor{Kind: "session", ID: machine.Claim().Session}}
	handoffs.mu.Lock()
	handoffs.handoffs[own.ID] = own
	handoffs.mu.Unlock()
	if status, body := complete(own.ID, own.Version); status != http.StatusOK {
		t.Fatalf("the completion of entry 2's task = %d %s, want 200: the worker's turn is on the entry its phases row names", status, body)
	}
	if got := readIssue(t, rig.pool, rig.issue.Key); got.Phase != phase.Reviewing {
		t.Fatalf("the issue stands at %s after the completion of entry 2's task, want reviewing", got.Phase)
	}
}

// lateEffect is one fenced kind of effect (record.FenceOf) as the k27 case queues it: the payload,
// whether the kind is the run's alone (it names the generation and no phase entry), and how the
// case reads whether its executor acted. A runner given no Dispatch client and no Envoy publisher
// fails the run of a row that reaches its effect for want of one, so err is the read of every
// kind whose effect is a write or a publish; the production check's rows apply a fact, read off
// processed_events; a stop suspends its claim.
type lateEffect struct {
	name    string
	payload record.OutboxPayload
	runOnly bool
	acted   func(t *testing.T, rig oneStartRig, err error) bool
}

// reachedItsEffect reads a run whose executor reached the write or the publish its runner has no
// client for.
func reachedItsEffect(_ *testing.T, _ oneStartRig, err error) bool {
	return err != nil
}

// appliedAFact reads a production check's row that applied its fact: the outbox applies each under
// the source outbox, and nothing else of the case does.
func appliedAFact(t *testing.T, rig oneStartRig, err error) bool {
	t.Helper()
	if err != nil {
		t.Fatalf("run the production check's row: %v", err)
	}
	var applied int
	if err := rig.pool.QueryRow(context.Background(), `select count(*) from processed_events where source = 'outbox'`).Scan(&applied); err != nil {
		t.Fatalf("count the facts the outbox applied: %v", err)
	}
	return applied > 0
}

// suspendedItsClaim reads a stop that suspended the implementer's claim the case made.
func suspendedItsClaim(t *testing.T, rig oneStartRig, err error) bool {
	t.Helper()
	if err != nil {
		t.Fatalf("run the stop: %v", err)
	}
	machine, found := rig.sup.Machine(rig.token)
	if !found {
		t.Fatal("the implementer's claim is gone")
	}
	machine.Wait()
	state := machine.Claim().State
	return state == supervise.StateSuspended || state == supervise.StateRetired
}

// Every effect the daemon queues for an issue names the run and the phase entry it was queued at
// (record.Fence), and one that acts and runs after its issue was handed a later entry or a later
// run does nothing to it (R17 and R20 of dispatch://INFRA-3320). One case for each fenced kind of
// record's fence inventory (TestEveryOutboxKindNamesItsRunOrSaysWhyNot): queued at the issue's
// generation G and entry N, it acts when it runs there; delivered after the issue's entry was
// raised to N+1 (as a return to work raises it) and after its generation was raised to G+1 (a new
// run), it does nothing. A notice, an architect's or the controller's, only tells, and its
// delivery reads the run alone (the architect's ruling of 2026-11-21 on dispatch://INFRA-3320): it
// is delivered at N+1 and does nothing in G+1. The pod's suspension is
// TestACleanupDeletesOnlyItsOwnIssuesPod's, which reads the pod it acts on beside the fence; an
// idle release's save, role stop and teardown cancel the release they belong to rather than acting
// (the outbox's podStands and release arms).
func TestALateEffectOfAnEarlierRunOrPhaseEntryDoesNothing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	opened := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	effects := []lateEffect{
		{"dispatch_status", record.OrdinaryStatusWrite("testing", "in_progress", ""), false, reachedItsEffect},
		{"dispatch_message", record.MessagePost{Body: "A message of the run."}, false, reachedItsEffect},
		{"claim_release", record.ClaimRelease{}, false, reachedItsEffect},
		{"notice", record.Notice{Kind: "phase-finished", Role: claim.RolePlanner, Phase: phase.Planning}, false, reachedItsEffect},
		{"controller_notice", record.ControllerNotice{Kind: record.TriageNotice}, false, reachedItsEffect},
		{"role_message", record.RoleMessage{Role: "legion-legion-legion-208-implementer", Body: "The checks of the head settled."}, false, reachedItsEffect},
		{"phase_ended", record.PhaseEnded{Role: claim.RoleImplementer, From: phase.Implementing, To: phase.AwaitingChildren, Reason: "LEGION-209 (todo) is open"}, false, reachedItsEffect},
		{"hook_prompt", record.HookPrompt{Hook: "back-to-implementing", Role: claim.RoleImplementer, From: phase.Reviewing, To: phase.Implementing, Count: 1, Prompt: "Say what the round missed."}, false, reachedItsEffect},
		{"update_ask", record.UpdateAsk{Role: claim.RoleImplementer, Round: "pod:9d3f0c7a", Pod: "9d3f0c7a", Text: "Commit and push, then stop."}, false, reachedItsEffect},
		{"merge_queue_removal", record.MergeQueueRemoval{Number: 42, Head: "c0de"}, false, reachedItsEffect},
		{"merge_forward", record.MergeForwardMessage{Step: record.MergeForwardAsk, Role: claim.RoleImplementer, Head: "c0de", Base: "main", Phase: phase.Implementing, PullRequest: 42, Why: record.ConflictMergeNext}, false, reachedItsEffect},
		{"gate_seed", record.GateSeed{ArtifactID: "artifact-208", Step: record.DesignStepSpec, Version: 1}, true, reachedItsEffect},
		{"pending_basis_seed", record.PendingBasisSeed{Step: record.DesignStepSpec, ArtifactID: "artifact-208", BasisArtifactID: "artifact-200", BasisVersion: 2, Ask: "ask-1",
			RegisteredAt: opened}, true, reachedItsEffect},
		{"retention_end", record.RetentionEnd{}, false, reachedItsEffect},
		{"issue_branch", record.IssueBranch{}, true, reachedItsEffect},
		{"check_wake", record.CheckWakeDue{Look: 1, OpenedAt: opened, Reason: "the agent's own wake"}, true, appliedAFact},
		{"check_look", record.CheckLook{Look: 1, OpenedAt: opened}, true, appliedAFact},
		{"check_bound", record.CheckBound{}, true, appliedAFact},
		{"supervise suspend", record.SuperviseRequest{Op: "suspend", Tree: "LEGION-208", Role: claim.RoleImplementer, Generation: 1}, false, suspendedItsClaim},
		{"supervise issue_close", record.SuperviseRequest{Op: "issue_close", Tree: "LEGION-208", Role: claim.RoleImplementer, Generation: 1}, false, suspendedItsClaim},
	}
	later := []struct {
		name string
		move func(t *testing.T, rig oneStartRig)
	}{
		{"delivered where it was queued", func(*testing.T, oneStartRig) {}},
		{"delivered at the next phase entry", func(t *testing.T, rig oneStartRig) {
			t.Helper()
			row := rolePhaseRow(t, rig.pool, rig.issue.Key, claim.RoleImplementer)
			row.Entry++
			if err := store.Transact(ctx, rig.pool, func(ctx context.Context, tx pgx.Tx) error {
				return record.NewStore().PutPhase(ctx, tx, row)
			}); err != nil {
				t.Fatalf("raise the implementer's entry: %v", err)
			}
		}},
		{"delivered in the next run", func(t *testing.T, rig oneStartRig) {
			t.Helper()
			issue := readIssue(t, rig.pool, rig.issue.Key)
			issue.Generation++
			putOutboxIssue(t, rig.pool, record.NewStore(), *issue)
		}},
	}
	// GitHub refuses every write, so a branch's run errs once it reaches its create; the fake keeps
	// nothing, so every case shares it.
	github := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "refused", http.StatusForbidden)
	}))
	t.Cleanup(github.Close)
	for _, effect := range effects {
		for _, when := range later {
			t.Run(effect.name+" "+when.name, func(t *testing.T) {
				rig := newOneStartRig(t)
				records := record.NewStore()
				if err := store.Transact(ctx, rig.pool, func(ctx context.Context, tx pgx.Tx) error {
					return records.PutProductionCheck(ctx, tx, record.ProductionCheck{Issue: rig.issue.Key, Project: "LEGION", Tree: rig.issue.Tree,
						Repo: rig.issue.Repo, Number: 42, MergeSHA: "c0de", OpenedAt: opened, BoundAt: opened.Add(time.Hour), Role: claim.RoleTester, Looks: 1})
				}); err != nil {
					t.Fatalf("open the issue's production check: %v", err)
				}
				runner := &outbox{log: quietLogger(), pool: rig.pool, records: records, project: "legion", dispatchProject: "LEGION",
					tokens: outboxTokens{}, githubAPI: github.URL}
				if _, isStop := effect.payload.(record.SuperviseRequest); isStop {
					runner = rig.runner
					locator, err := rig.rt.Spawn(ctx, runtime.SpawnSpec{Claim: rig.token, Role: claim.RoleImplementer})
					if err != nil {
						t.Fatalf("mint the implementer's locator: %v", err)
					}
					machine, _, err := rig.sup.Create(ctx, supervise.Claim{Token: rig.token, Project: "legion", Tree: rig.issue.Tree,
						Issue: rig.issue.Key, Role: claim.RoleImplementer, State: supervise.StateReady, Generation: 1, Locator: &locator,
						Session: "ses-implementer", SessionFile: "/tmp/implementer.jsonl"}, "")
					if err != nil {
						t.Fatalf("create the implementer's claim: %v", err)
					}
					machine.Wait()
				}
				row := mustOutboxRow(t, rig.issue.Key, effect.payload, time.Now())
				enqueueOutbox(t, rig.pool, records, row)
				queued := claimDueOutboxRowOf(t, rig.pool, row.Kind)
				queuedAt, _ := record.FenceOf(mustDecode(t, queued))
				if queuedAt.Generation != 1 || (queuedAt.Entry == 0) != effect.runOnly {
					t.Fatalf("the %s row names %+v, want the run its issue stands at and, unless the kind is the run's alone, the phase entry", effect.name, queuedAt)
				}
				when.move(t, rig)
				err := runner.execute(ctx, queued)
				// A kind that is the run's alone (the gate's seeds, the branch, the production
				// check's rows) acts at any phase entry of its run (R17), and so is a notice
				// delivered at any (reports).
				reports := map[string]bool{"notice": true, "controller_notice": true}
				want := when.name == later[0].name || (effect.runOnly || reports[effect.name]) && when.name == later[1].name
				if acted := effect.acted(t, rig, err); acted != want {
					t.Fatalf("the %s row %s acted = %t (run error %v), want %t: an effect does nothing to a later phase entry or run than its own",
						effect.name, when.name, acted, err, want)
				}
			})
		}
	}
}

// claimDueOutboxRowOf is the one queued row of kind, read back as the store holds it.
func claimDueOutboxRowOf(t *testing.T, pool *store.Pool, kind record.OutboxKind) record.OutboxRow {
	t.Helper()
	var row record.OutboxRow
	if err := pool.QueryRow(context.Background(), `select id, kind, issue, payload, created_at from outbox where kind = $1 order by id desc limit 1`,
		string(kind)).Scan(&row.ID, &row.Kind, &row.Issue, &row.Payload, &row.CreatedAt); err != nil {
		t.Fatalf("read the queued %s row: %v", kind, err)
	}
	row.NextAt = time.Now()
	return row
}

// mustDecode is row's payload, decoded as the outbox decodes it.
func mustDecode(t *testing.T, row record.OutboxRow) record.OutboxPayload {
	t.Helper()
	payload, err := record.DecodeOutboxPayload(row)
	if err != nil {
		t.Fatalf("decode row %d: %v", row.ID, err)
	}
	return payload
}

// cleanupRuntime is the fake runtime with the issue-level cleanup a close or a stop asks of a
// Kubernetes one (runtime.IssueSuspender), which records each pod it deleted: the pod the issue's
// Sandbox held when the cleanup's authorize, asked under the pod's launch turn, let it act
// (fake.Runtime.IssuePod, which the case scripts), as SuspendIssue(project, issue, podUID).
type cleanupRuntime struct {
	*fake.Runtime
	project string
	mu      sync.Mutex
	// trees is the tree label (legion.dev/tree) of each issue's pod a case launched: the gate root
	// the pod was made under, which the release before's cleanup selected pods by and this
	// release's reads for nothing.
	trees   map[string]string
	deleted []string
}

// launched gives issue's Sandbox the pod pod, made under the gate root tree.
func (r *cleanupRuntime) launched(issue, pod, tree string) {
	r.ScriptIssuePod(issue, pod)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.trees == nil {
		r.trees = map[string]string{}
	}
	r.trees[issue] = tree
}

func (r *cleanupRuntime) SuspendIssue(ctx context.Context, issue, _ string, authorize func(context.Context) (bool, error)) error {
	act, err := authorize(ctx)
	if err != nil || !act {
		return err
	}
	pod, _ := r.IssuePod(issue)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deleted = append(r.deleted, r.project+"/"+issue+"/"+pod)
	r.ScriptIssuePod(issue, "")
	return nil
}

func (r *cleanupRuntime) deletions() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.deleted)
}

// A cleanup deletes only its own issue's pod, the incarnation it was queued for (R17 of
// dispatch://INFRA-3320: a pod and its disk are deleted by the issue's own identity and the pod's
// own incarnation). A stop queues the issue's suspension (record.IssueSuspend) for the pod its
// claims ran in and at the run and phase entry the issue stood at. Run against that pod, it
// deletes it. Run once the issue's Sandbox holds a later pod — the issue came back and was
// stopped again, its next launch making a pod of its own — it deletes nothing, and nothing either
// once the issue stands at a later phase entry, as a return raises it. A former parent's close
// deletes its own pod and cleans up its own lifecycle alone: its child, launched under its gate
// root — the child's pod carries the parent's key as its tree label, and its record the parent
// as its tree — and since moved out from under it, runs on with its pod and its lifecycle.
func TestACleanupDeletesOnlyItsOwnIssuesPod(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const child = "LEGION-209"
	for _, tc := range []struct {
		name    string
		pod     string
		entered bool
		// child closes the issue at done while child, launched under it, still runs.
		child bool
		want  []string
	}{
		{name: "the pod it was queued for", pod: "pod-of-the-stop", want: []string{"legion/LEGION-208/pod-of-the-stop"}},
		{name: "a later pod of the issue", pod: "pod-of-a-later-launch"},
		{name: "the same pod at a later phase entry", pod: "pod-of-the-stop", entered: true},
		{name: "a former parent's close, while the child launched under its gate root runs", pod: "pod-of-the-stop", child: true,
			want: []string{"legion/LEGION-208/pod-of-the-stop"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			st, err := store.Open(ctx, pool.Config().ConnString())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(st.Close)
			records := record.NewStore()
			issue := record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION",
				Tree: "LEGION-208", Title: "stopped", Phase: phase.Implementing, Generation: 1, Status: "backlog", DispatchStatus: "backlog"}
			if tc.child {
				issue.Status, issue.DispatchStatus = record.DoneStatus, record.DoneStatus
			}
			putOutboxIssue(t, pool, records, issue)
			implementer := record.PhaseRow{Issue: issue.Key, Role: claim.RoleImplementer, Entry: 2}
			if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
				return records.PutPhase(ctx, tx, implementer)
			}); err != nil {
				t.Fatal(err)
			}
			sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
			cleanup := &cleanupRuntime{Runtime: rt, project: "legion"}
			sup.deps.Runtime = cleanup
			runner := &outbox{log: quietLogger(), pool: pool, records: records, supervisor: sup, trees: st, project: "legion", dispatchProject: "LEGION", now: time.Now}
			if tc.child {
				// The child stands at work as a root of its own now, its record still naming the
				// tree it was launched in, and both issues' lifecycles stand open as their
				// admissions left them.
				putOutboxIssue(t, pool, records, record.Issue{Key: child, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
					Project: "LEGION", Tree: issue.Key, Title: "moved out", Phase: phase.Implementing, Generation: 1, Status: "in_progress",
					DispatchStatus: "in_progress"})
				for _, key := range []string{issue.Key, child} {
					if _, err := st.OpenIssueLifecycle(ctx, "legion", key, treelifecycle.AuthorityWorkflow); err != nil {
						t.Fatal(err)
					}
				}
				cleanup.launched(child, "pod-of-the-child", issue.Key)
			}

			// The stop queued the suspension while its roles ran in the pod of the stop.
			row := mustOutboxRow(t, issue.Key, record.IssueSuspend{Tree: issue.Tree, Generation: 1, TreeGeneration: 1, PodUID: "pod-of-the-stop"}, time.Now())
			enqueueOutbox(t, pool, records, row)
			queued := claimDueOutboxRowOf(t, pool, record.OutboxKindIssueSuspend)
			cleanup.launched(issue.Key, tc.pod, issue.Key)
			if tc.entered {
				implementer.Entry++
				if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
					return records.PutPhase(ctx, tx, implementer)
				}); err != nil {
					t.Fatal(err)
				}
			}

			if err := runner.execute(ctx, queued); err != nil {
				t.Fatalf("run the suspension: %v", err)
			}
			if got := cleanup.deletions(); !slices.Equal(got, tc.want) {
				t.Fatalf("the suspension deleted %v, want %v: a cleanup deletes only the pod of its own issue it was queued for, at the run and entry it was queued at", got, tc.want)
			}
			if !tc.child {
				return
			}
			var cleaned []string
			for _, call := range cleanup.CallsOf("CleanupIssue") {
				cleaned = append(cleaned, call.Issue)
			}
			if want := []string{issue.Key}; !slices.Equal(cleaned, want) {
				t.Fatalf("the close cleaned up %v, want %v: the closed issue's own lifecycle alone", cleaned, want)
			}
			if live, err := st.IssueLive(ctx, "legion", child); err != nil || !live {
				t.Fatalf("%s's own lifecycle is live: %t (%v) after its former parent's close, want it open", child, live, err)
			}
			if pod, _ := cleanup.IssuePod(child); pod != "pod-of-the-child" {
				t.Fatalf("%s's Sandbox holds pod %q after its former parent's close, want pod-of-the-child still running", child, pod)
			}
		})
	}
}

// A root that comes back into Legion's work while its own cleanup is reserved waits in the line
// for that cleanup, and takes a free slot as soon as the cleanup confirms, with no other event (R17
// of dispatch://INFRA-3320): the confirmation is a fact of its own (intake.IssueCleanedUp), and
// admission looks again on it as on every fact. The close's cleanup row (record.IssueSuspend) made
// the reservation at its first try and waited; the root is set back to todo; the row's next try
// runs under a runtime that holds no pod of the issue (tmux, the fake). A root a person closed at
// work keeps its run, and the row stands. A root closed at done starts a new run with its todo, so
// the row is of a run that has ended, and still finishes the reservation its earlier try made: a
// fence keeps a row off a pod, never off the cleanup it already holds. Negative controls, run when
// this was written, failing this test and then reverted: with no fact applied once the cleanup
// confirms, neither root takes a slot; with the row of an ended run finishing nothing, the root
// closed at done keeps its cleanup reserved.
func TestARootBackInTodoTakesASlotOnceItsCleanupConfirms(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		at   phase.Phase
	}{
		{name: "a root closed at work", at: phase.Admitted},
		{name: "a root closed at done", at: phase.Done},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const key = "CAPTURE-40"
			pool := isolatedOutboxPool(t)
			w := copyRuntime(t, pool, &copyBoard{}, 3)
			w.projectID = "capture"
			st, err := store.Open(ctx, pool.Config().ConnString())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(st.Close)
			closed := conversionIssue(key, key, tc.at, "done", 1)
			closed.HandedOver = true
			putOutboxIssue(t, pool, w.records, closed)
			if _, err := pool.Exec(ctx, `insert into issue_lifecycles (project, issue, epoch, authority, cleanup_started)
				values ('capture', $1, 1, 'workflow', true)`, key); err != nil {
				t.Fatalf("reserve the cleanup of %s: %v", key, err)
			}
			enqueueOutbox(t, pool, w.records, mustOutboxRow(t, key, record.IssueSuspend{Tree: key, Generation: 1, TreeGeneration: 1}, time.Now()))
			queued := claimDueOutboxRowOf(t, pool, record.OutboxKindIssueSuspend)

			if _, err := intake.ApplyFact(ctx, pool, "dispatch", "back-in-todo", intake.DispatchIssue{Key: key, ResolvedRepo: "acme/widgets", Seq: 5,
				Type: "issue.updated", Status: "todo", Title: key, Rank: key, HandedOver: true, Labels: []string{"legion"}}, w.handlers...); err != nil {
				t.Fatalf("apply the todo of %s: %v", key, err)
			}
			slotted := func() bool {
				t.Helper()
				var slotted bool
				if err := pool.QueryRow(ctx, `select exists (select 1 from slots where issue = $1)`, key).Scan(&slotted); err != nil {
					t.Fatal(err)
				}
				return slotted
			}
			if slotted() {
				t.Fatalf("%s took a slot while its cleanup is reserved, want it waiting for the cleanup", key)
			}

			sup, _ := newOutboxSupervisor(t, "capture", t.TempDir())
			runner := &outbox{log: quietLogger(), pool: pool, records: w.records, supervisor: sup, trees: st, project: "capture",
				dispatchProject: "CAPTURE", now: time.Now, handlers: w.handlers}
			if err := runner.execute(ctx, queued); err != nil {
				t.Fatalf("run the cleanup of %s: %v", key, err)
			}
			// The confirmation's own pass may open the issue's next epoch at once, so the
			// reservation's end is read, not the row of the epoch it confirmed.
			var reserved bool
			if err := pool.QueryRow(ctx, `select cleanup_started and cleanup_confirmed_at is null from issue_lifecycles
				where project = 'capture' and issue = $1`, key).Scan(&reserved); err != nil || reserved {
				t.Fatalf("the cleanup of %s is still reserved: %t (%v), want the reservation the row's first try made finished", key, reserved, err)
			}
			if !slotted() {
				t.Fatalf("%s holds no slot once its cleanup confirmed, with every slot free: want the confirmation to start it", key)
			}
		})
	}
}

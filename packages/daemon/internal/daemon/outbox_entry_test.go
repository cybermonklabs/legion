package daemon

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
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
func entryStartRows(t *testing.T, pool *pgxpool.Pool, issue string) []entryStartRow {
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

// rolePhaseRow is role's phases row of issue: the record the entry a phase is on and that entry's
// task are read from (record.PhaseRow.Entry and EntryTask), and the zero row for a role the
// record holds none of.
func rolePhaseRow(t *testing.T, pool *pgxpool.Pool, issue string, role claim.Role) record.PhaseRow {
	t.Helper()
	ctx := context.Background()
	var found record.PhaseRow
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
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
	if err := pgx.BeginFunc(ctx, rig.pool, func(tx pgx.Tx) error {
		return entryEngine(t).StartPromotedWorker(ctx, tx, *issue, claim.RoleImplementer)
	}); err != nil {
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
	if err := pgx.BeginFunc(ctx, rig.pool, func(tx pgx.Tx) error {
		return entryEngine(t).SendOwedEntry(ctx, tx, *issue, claim.RoleImplementer)
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
// over no second prompt (keepsStartTask), and records the entry as given on the claim all the
// same (supervise.Machine.EntryGiven): the task the claim holds is work of this role's phase, so
// the difference the daemon reads between the entry the role's phases row names and the entry its
// claim was given (supervise.Claim.GivenEntry) closes here rather than through a second task.
//
// The sequence is the one the record writes: the transition's start queues entry 1's task, and
// the promotion of a re-admitted tree's member raises entry 2 with the phase's resume task
// (record.SuperviseRequest.ResumeTask), which the worker already at work needs no second prompt
// for.
func TestAnAppendOnlyStartThatDeliversNothingRecordsTheEntryAsGiven(t *testing.T) {
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
	if err := pgx.BeginFunc(ctx, rig.pool, func(tx pgx.Tx) error {
		return entryEngine(t).StartPromotedWorker(ctx, tx, *issue, claim.RoleImplementer)
	}); err != nil {
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
	if got.GivenEntry != 2 {
		t.Fatalf("the claim was given entry %d, want 2: the start that delivered nothing records the entry it carried", got.GivenEntry)
	}
}

// A re-admission leaves the role's entry where it stood and takes the work of it away: the task
// is the ended generation's and goes with that generation's handoffs (record.ClearGeneration,
// which workflow's Adopt and enterChild run for each member), while the number stays, because it
// counts the entries the role has been handed on this issue and nothing of a re-admission unhands
// one (record.PhaseRow.Entry and EntryTask).
//
// What writes the work again is the promotion of the re-admitted tree's member
// (workflow.Engine.StartPromotedWorker): it raises the entry, writes that entry's task beside the
// number in the same transaction and writes the start that carries both, so the worker is started
// under an entry above every entry a completion still in flight could name.
func TestAReadmissionLeavesTheEntryColumnsAndThePromotionWritesTheWorkAgain(t *testing.T) {
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

	if err := pgx.BeginFunc(ctx, rig.pool, func(tx pgx.Tx) error {
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
	if err := pgx.BeginFunc(ctx, rig.pool, func(tx pgx.Tx) error {
		return entryEngine(t).StartPromotedWorker(ctx, tx, *issue, claim.RoleImplementer)
	}); err != nil {
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

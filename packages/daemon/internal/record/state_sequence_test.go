package record

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// stateRun is the run id the state rows of these tests carry.
const stateRun = "0b9f5c2e-4f1d-4c8a-9d57-2f3a6e8b1c40"

// stateQuerier is what stateRowsOf reads through: the pool, which sees every committed row, or a
// transaction, which sees its own rows before they commit as well.
type stateQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// stateRowsOf is every state row of project that q sees, by sequence; a row that has not taken
// its sequence yet comes last.
func stateRowsOf(t *testing.T, q stateQuerier, project string) []OutboxRow {
	t.Helper()
	rows, err := q.Query(context.Background(), "select "+outboxColumns+` from outbox
		where issue = $1 and kind = any($2) order by state_seq, id`, project, stateKindNames)
	if err != nil {
		t.Fatalf("read the state rows of %s: %v", project, err)
	}
	defer rows.Close()
	var out []OutboxRow
	for rows.Next() {
		row, err := scanOutbox(rows)
		if err != nil {
			t.Fatalf("read a state row of %s: %v", project, err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the state rows of %s: %v", project, err)
	}
	return out
}

// stateLabels names each row by what it reports, read back through the outbox's own decoder: the
// issue of a state change, and the kind of any other state row.
func stateLabels(t *testing.T, rows []OutboxRow) []string {
	t.Helper()
	labels := make([]string, 0, len(rows))
	for _, row := range rows {
		payload, err := DecodeOutboxPayload(row)
		if err != nil {
			t.Fatalf("decode state row %d: %v", row.ID, err)
		}
		if change, ok := payload.(StateChange); ok {
			labels = append(labels, change.Issue)
			continue
		}
		labels = append(labels, string(row.Kind))
	}
	return labels
}

// stateSeqs is the sequence of each row.
func stateSeqs(rows []OutboxRow) []int64 {
	seqs := make([]int64, 0, len(rows))
	for _, row := range rows {
		seqs = append(seqs, row.StateSeq)
	}
	return seqs
}

// A state row takes its sequence, its time and its transaction's id when the transaction that
// queued it commits, from its project's counter row (migration 20261119184200_state_events). So the
// sequence is the order the transactions committed in, whatever order they queued their rows in;
// a transaction that rolls back, or that deleted its own row, takes no step, so the sequence has
// no gap; every row of one transaction shares its time and its transaction; and the time never
// goes backwards, even when the clock does. A transaction whose commit finds the counter row
// locked by another's waits for that commit alone, and then takes the next sequence. The rows are
// queued as the workflow queues any payload (NewOutboxRow, then Enqueue), with the bare project
// key as the issue.
//
// The state lane then runs a project's rows strictly in sequence order (outboxInOrder): DueRows and
// ClaimRow offer only the lowest unfinished state row of the project, a row that fails holds every
// later one until it is published, and neither another project's state rows nor a row of another
// kind carrying the same project key are held by it.
func TestStateEventsTakeTheirSequenceInCommitOrder(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	now := time.Date(2026, 11, 18, 16, 0, 0, 0, time.UTC)
	const project = "LEGION"

	stateRow := func(project string, payload OutboxPayload) OutboxRow {
		t.Helper()
		row, err := NewOutboxRow(project, payload, now)
		must(t, err)
		return row
	}
	// change is a state change of project that moves issue's phase.
	change := func(project, issue string) OutboxRow {
		t.Helper()
		return stateRow(project, StateChange{Project: project, Run: stateRun, Issue: issue, Changes: []StateFieldChange{
			{Field: StateFieldPhase, Old: json.RawMessage(`null`), New: json.RawMessage(`"planning"`)},
		}})
	}
	// begin opens a transaction on a connection of its own, which the test commits or rolls back.
	begin := func() pgx.Tx {
		t.Helper()
		tx, err := st.Pool().Begin(ctx)
		must(t, err)
		t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
		return tx
	}
	enqueue := func(tx pgx.Tx, row OutboxRow) {
		t.Helper()
		must(t, records.Enqueue(ctx, tx, row))
	}
	commit := func(tx pgx.Tx) {
		t.Helper()
		must(t, tx.Commit(ctx))
	}
	published := func(project string) StatePublication {
		t.Helper()
		var read StatePublication
		inTx(t, st, func(tx pgx.Tx) {
			var err error
			read, err = records.StatePublication(ctx, tx, project)
			must(t, err)
		})
		return read
	}
	stateSeq := func(project string) int64 {
		t.Helper()
		return published(project).Seq
	}
	committed := func() []OutboxRow {
		t.Helper()
		return stateRowsOf(t, st.Pool(), project)
	}

	if none := published(project); none != (StatePublication{}) {
		t.Fatalf("StatePublication of a project with no state row = %+v, want sequence 0 and no store id", none)
	}

	// Two transactions that commit in the reverse of the order they queued their rows in.
	first, second := begin(), begin()
	enqueue(first, change(project, "LEGION-1"))
	enqueue(second, change(project, "LEGION-2"))
	if own := stateRowsOf(t, first, project); len(own) != 1 || own[0].StateSeq != 0 || !own[0].StateAt.IsZero() || own[0].StateTx != 0 {
		t.Fatalf("the row read inside its own transaction before the commit = %+v, want one row with no sequence, time or transaction yet", own)
	}
	commit(second)
	commit(first)
	rows := committed()
	if got, seqs := stateLabels(t, rows), stateSeqs(rows); !slices.Equal(got, []string{"LEGION-2", "LEGION-1"}) || !slices.Equal(seqs, []int64{1, 2}) {
		t.Fatalf("the committed rows = %v at %v, want LEGION-2 at 1 and LEGION-1 at 2: the sequence is the commit order", got, seqs)
	}
	if rows[0].StateTx == rows[1].StateTx || rows[0].StateTx == 0 {
		t.Fatalf("the two transactions' rows carry transactions %d and %d, want two different ids", rows[0].StateTx, rows[1].StateTx)
	}
	if seq := stateSeq(project); seq != 2 {
		t.Fatalf("StateSeq after two committed rows = %d, want 2", seq)
	}
	t.Logf("the rows queued first and second committed second and first, and took sequences 2 and 1")

	// A rolled-back transaction, and a transaction that deleted the row it queued, take no step.
	rolled := begin()
	enqueue(rolled, change(project, "LEGION-3"))
	must(t, rolled.Rollback(ctx))
	deleted := begin()
	id, err := records.EnqueueID(ctx, deleted, change(project, "LEGION-4"))
	must(t, err)
	if _, err := deleted.Exec(ctx, "delete from outbox where id = $1", id); err != nil {
		t.Fatalf("delete the row inside the transaction that queued it: %v", err)
	}
	commit(deleted)
	if seq := stateSeq(project); seq != 2 {
		t.Fatalf("StateSeq after a rollback and a deleted row = %d, want 2: neither leaves a row, so neither may take a step", seq)
	}
	after := begin()
	enqueue(after, change(project, "LEGION-5"))
	commit(after)
	rows = committed()
	if got, seqs := stateLabels(t, rows), stateSeqs(rows); !slices.Equal(got, []string{"LEGION-2", "LEGION-1", "LEGION-5"}) || !slices.Equal(seqs, []int64{1, 2, 3}) {
		t.Fatalf("the committed rows = %v at %v, want LEGION-5 at 3, the next sequence, with no gap", got, seqs)
	}
	t.Logf("a rollback and a row deleted before its commit left no row and no step")

	// Every row of one transaction shares its time and its transaction, in the order queued.
	together := begin()
	enqueue(together, change(project, "LEGION-6"))
	enqueue(together, stateRow(project, StateAdmission{Project: project, Run: stateRun, Admission: json.RawMessage(`{"cap":4}`)}))
	enqueue(together, stateRow(project, StateBoot{Project: project, Run: stateRun, Settings: json.RawMessage(`{"dispatch_url":"https://dispatch.example"}`)}))
	commit(together)
	rows = committed()
	group := rows[3:]
	if got, seqs := stateLabels(t, group), stateSeqs(group); !slices.Equal(got, []string{"LEGION-6", "state_admission", "state_boot"}) || !slices.Equal(seqs, []int64{4, 5, 6}) {
		t.Fatalf("one transaction's rows = %v at %v, want LEGION-6, the admission and the boot at 4, 5 and 6", got, seqs)
	}
	for _, row := range group[1:] {
		if !row.StateAt.Equal(group[0].StateAt) || row.StateTx != group[0].StateTx {
			t.Fatalf("row %d of one transaction carries %v in transaction %d, want %v in %d, the first row's", row.StateSeq, row.StateAt, row.StateTx, group[0].StateAt, group[0].StateTx)
		}
	}
	if group[0].StateTx == rows[2].StateTx {
		t.Fatalf("two transactions' rows carry the same transaction %d", group[0].StateTx)
	}
	t.Logf("three rows of one transaction share %v and transaction %d", group[0].StateAt, group[0].StateTx)

	// A transaction whose commit needs the counter row while another transaction holds it waits
	// for that commit, and no longer. The holder takes the lock as its own commit would, by
	// running the deferred trigger now, and stays open.
	holder := begin()
	enqueue(holder, change(project, "LEGION-7"))
	if _, err := holder.Exec(ctx, "set constraints outbox_state_sequence immediate"); err != nil {
		t.Fatalf("run the holder's commit trigger before its commit: %v", err)
	}
	if own := stateRowsOf(t, holder, project); own[len(own)-1].StateSeq != 7 {
		t.Fatalf("the holder's row = %+v, want sequence 7, taken with the counter row's lock", own[len(own)-1])
	}
	waiter := begin()
	enqueue(waiter, change(project, "LEGION-8"))
	var pid int
	if err := waiter.QueryRow(ctx, "select pg_backend_pid()").Scan(&pid); err != nil {
		t.Fatalf("read the waiting transaction's backend: %v", err)
	}
	waited := make(chan error, 1)
	go func() { waited <- waiter.Commit(ctx) }()
	testwait.Eventually(t, "the second commit to wait on the counter row's lock", func() bool {
		var waiting bool
		if err := st.Pool().QueryRow(ctx, `select exists (select 1 from pg_stat_activity
			where pid = $1 and wait_event_type = 'Lock')`, pid).Scan(&waiting); err != nil {
			t.Fatalf("read whether the second commit waits on a lock: %v", err)
		}
		return waiting
	})
	select {
	case err := <-waited:
		t.Fatalf("the second commit returned %v while the holder still held the counter row", err)
	default:
	}
	commit(holder)
	select {
	case err := <-waited:
		if err != nil {
			t.Fatalf("the second commit after the holder's = %v, want it committed: it waited for a commit, which ends", err)
		}
	case <-time.After(testwait.Bounded(t, time.Minute, time.Second)):
		t.Fatalf("the second commit did not end once the holder committed")
	}
	rows = committed()
	pair := rows[len(rows)-2:]
	if got, seqs := stateLabels(t, pair), stateSeqs(pair); !slices.Equal(got, []string{"LEGION-7", "LEGION-8"}) || !slices.Equal(seqs, []int64{7, 8}) {
		t.Fatalf("the holder's and the waiter's rows = %v at %v, want LEGION-7 at 7 and LEGION-8 at 8", got, seqs)
	}
	t.Logf("the second commit waited on the counter row until the holder committed, then took sequence 8")

	// The clock went back an hour, which leaves the counter's time an hour ahead of it: a row's
	// time is the previous one's plus a microsecond rather than the clock's, and every row of one
	// transaction still shares it.
	var ahead time.Time
	if err := st.Pool().QueryRow(ctx, `update state_publication set last_at = clock_timestamp() + interval '1 hour'
		where project = $1 returning last_at`, project).Scan(&ahead); err != nil {
		t.Fatalf("set LEGION's counter an hour ahead of the clock: %v", err)
	}
	behind := begin()
	enqueue(behind, change(project, "LEGION-9"))
	commit(behind)
	both := begin()
	enqueue(both, change(project, "LEGION-10"))
	enqueue(both, change(project, "LEGION-11"))
	commit(both)
	rows = committed()
	tail := rows[len(rows)-3:]
	for i, want := range []time.Time{ahead.Add(time.Microsecond), ahead.Add(2 * time.Microsecond), ahead.Add(2 * time.Microsecond)} {
		if !tail[i].StateAt.Equal(want) {
			t.Fatalf("row %d's time = %v behind a clock an hour slow, want %v", tail[i].StateSeq, tail[i].StateAt, want)
		}
	}
	for i := 1; i < len(rows); i++ {
		previous, row := rows[i-1], rows[i]
		if row.StateSeq != previous.StateSeq+1 {
			t.Fatalf("sequence %d follows %d, want no gap", row.StateSeq, previous.StateSeq)
		}
		if row.StateTx == previous.StateTx && !row.StateAt.Equal(previous.StateAt) {
			t.Fatalf("rows %d and %d of one transaction carry %v and %v, want one time", previous.StateSeq, row.StateSeq, previous.StateAt, row.StateAt)
		}
		if row.StateTx != previous.StateTx && !row.StateAt.After(previous.StateAt) {
			t.Fatalf("row %d of a later transaction carries %v, not after row %d's %v", row.StateSeq, row.StateAt, previous.StateSeq, previous.StateAt)
		}
	}
	if seq := stateSeq(project); seq != 11 {
		t.Fatalf("StateSeq = %d, want 11, the last committed row's", seq)
	}
	t.Logf("eleven rows took sequences 1 to 11 with no gap, and no transaction's time is before an earlier one's")

	// The lane: only the lowest unfinished state row of the project is claimable.
	inTx(t, st, func(tx pgx.Tx) {
		enqueue(tx, change("INFRA", "INFRA-1"))
		must(t, records.EnqueueGatedSteps(ctx, tx, GatedStepsPublish{Project: project, Steps: []DesignStep{DesignStepSpec}}, now))
	})
	due := func(project string, at time.Time) (state []OutboxRow, other []OutboxKind) {
		t.Helper()
		inTx(t, st, func(tx pgx.Tx) {
			rows, err := records.DueRows(ctx, tx, project, "", nil, at)
			must(t, err)
			for _, row := range rows {
				if LaneOf(row.Kind) == LaneState {
					state = append(state, row)
				} else {
					other = append(other, row.Kind)
				}
			}
		})
		return state, other
	}
	state, other := due(project, now)
	if seqs := stateSeqs(state); !slices.Equal(seqs, []int64{1}) || !slices.Equal(other, []OutboxKind{OutboxKindGatedStepsPublish}) {
		t.Fatalf("LEGION's due rows = state rows at %v and %v, want the state row at 1 alone and the gated-steps publish its project key also carries", seqs, other)
	}
	if infra, _ := due("INFRA", now); len(infra) != 1 || infra[0].StateSeq != 1 || stateSeq("INFRA") != 1 {
		t.Fatalf("INFRA's due state rows = %+v, want its own first row, at its own sequence 1: LEGION's lane holds no other project's", infra)
	}
	inTx(t, st, func(tx pgx.Tx) {
		_, ok, err := records.ClaimRow(ctx, tx, rows[1].ID, now, time.Minute)
		must(t, err)
		if ok {
			t.Fatalf("ClaimRow of the state row at 2 claimed it while the row at 1 is unfinished")
		}
	})
	var head OutboxRow
	inTx(t, st, func(tx pgx.Tx) {
		var ok bool
		var err error
		head, ok, err = records.ClaimRow(ctx, tx, rows[0].ID, now, time.Minute)
		must(t, err)
		if !ok || head.StateSeq != 1 {
			t.Fatalf("ClaimRow of the state row at 1 = %+v, %t, want it claimed", head, ok)
		}
	})
	// The head fails and waits for its retry: every later row of the lane waits behind it.
	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.RetryOutbox(ctx, tx, head.ID, head.LeaseToken, now.Add(time.Minute), "the listener refused the publish"))
	})
	if state, _ := due(project, now); len(state) != 0 {
		t.Fatalf("LEGION's due state rows while the row at 1 waits for its retry = %v, want none", stateSeqs(state))
	}
	retry := now.Add(2 * time.Minute)
	if state, _ := due(project, retry); !slices.Equal(stateSeqs(state), []int64{1}) {
		t.Fatalf("LEGION's due state rows at the retry = %v, want the row at 1 again, alone", stateSeqs(state))
	}
	inTx(t, st, func(tx pgx.Tx) {
		claimed, ok, err := records.ClaimRow(ctx, tx, head.ID, retry, time.Minute)
		must(t, err)
		if !ok {
			t.Fatalf("ClaimRow of the retried row at 1 claimed nothing")
		}
		must(t, records.FinishOutbox(ctx, tx, claimed.ID, claimed.LeaseToken))
	})
	if state, _ := due(project, retry); !slices.Equal(stateSeqs(state), []int64{2}) || stateLabels(t, state)[0] != "LEGION-1" {
		t.Fatalf("LEGION's due state rows once the row at 1 finished = %v, want the row at 2 alone", stateSeqs(state))
	}
	t.Logf("the lane offered the row at 1 alone, held the rest behind its retry, and offered the row at 2 once it finished")
}

package api

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/going"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// installInteractionWriteProbes installs, in the test's own database (storetest.Open cloned it for
// this test), two triggers. interaction_write_logged records, after each row written to graph_nodes,
// graph_tuples, going_requests and going_events, the run the row names, what it is, the id of its
// transaction through txid_current() and the statement that wrote it. interaction_write_probe runs
// before and after each row inserted into going_requests and going_events and does what
// interaction_write_plan says for that table and moment: `fail` raises an error that names the run
// and what the transaction holds of it at that moment - the node, its authored_by, the frame value
// of the row's sequence and the row itself - and `hold` waits on the advisory lock
// interactionWriteHold, which a test holds to keep the write's transaction open.
func installInteractionWriteProbes(t *testing.T, database *store.Store) {
	t.Helper()
	for _, statement := range []string{
		`create table interaction_write_plan (tbl text not null, moment text not null, act text not null, primary key (tbl, moment))`,
		`create table interaction_write_statements (
			seq bigserial primary key, tbl text not null, run text, what text not null, xid bigint not null, statement text not null)`,
		`create function interaction_write_logged() returns trigger language plpgsql as $$
		begin
			if tg_table_name = 'graph_nodes' then
				insert into interaction_write_statements (tbl, run, what, xid, statement) values (tg_table_name,
					case when new.kind = 'interaction' then new.id end, new.kind || '/' || new.id, txid_current(), current_query());
			elsif tg_table_name = 'graph_tuples' then
				insert into interaction_write_statements (tbl, run, what, xid, statement) values (tg_table_name,
					case when new.from_kind = 'interaction' then new.from_id when new.to_kind = 'interaction' then new.to_id end,
					new.from_kind || '/' || new.from_id || ' ' || new.kind || ' ' || new.to_kind, txid_current(), current_query());
			elsif tg_table_name = 'going_requests' then
				insert into interaction_write_statements (tbl, run, what, xid, statement)
				values (tg_table_name, new.id::text, 'going_requests', txid_current(), current_query());
			else
				insert into interaction_write_statements (tbl, run, what, xid, statement)
				values (tg_table_name, new.request_id::text, 'going_events ' || new.sequence, txid_current(), current_query());
			end if;
			return null;
		end $$`,
		`create function interaction_write_probe() returns trigger language plpgsql as $$
		declare
			plan text;
			run text;
			frame_sequence integer;
			projected boolean;
		begin
			select act into plan from interaction_write_plan where tbl = tg_table_name and moment = lower(tg_when);
			if plan is null then
				return new;
			end if;
			if tg_table_name = 'going_requests' then
				run := new.id::text;
				projected := exists (select 1 from going_requests where id = new.id);
			else
				run := new.request_id::text;
				frame_sequence := new.sequence;
				projected := exists (select 1 from going_events where request_id = new.request_id and going_events.sequence = new.sequence);
			end if;
			if plan = 'hold' then
				perform pg_advisory_xact_lock(` + interactionWriteHold + `);
				return new;
			end if;
			raise exception 'the probe fails the write % its % row of run %: node=% authored_by=% frame=% row=%',
				lower(tg_when), tg_table_name, run,
				exists (select 1 from graph_nodes where kind = 'interaction' and id = run),
				exists (select 1 from graph_tuples where from_kind = 'interaction' and from_id = run and kind = 'authored_by' and asserted),
				exists (select 1 from graph_tuples where from_kind = 'interaction' and from_id = run and kind = 'frame'
					and to_kind = 'value' and (to_id::jsonb ->> 'sequence')::integer = frame_sequence),
				projected;
		end $$`,
		`create trigger interaction_write_logged after insert or update on graph_nodes for each row execute function interaction_write_logged()`,
		`create trigger interaction_write_logged after insert or update on graph_tuples for each row execute function interaction_write_logged()`,
		`create trigger interaction_write_logged after insert or update on going_requests for each row execute function interaction_write_logged()`,
		`create trigger interaction_write_logged after insert or update on going_events for each row execute function interaction_write_logged()`,
		`create trigger interaction_write_probe_before before insert on going_requests for each row execute function interaction_write_probe()`,
		`create trigger interaction_write_probe_after after insert on going_requests for each row execute function interaction_write_probe()`,
		`create trigger interaction_write_probe_before before insert on going_events for each row execute function interaction_write_probe()`,
		`create trigger interaction_write_probe_after after insert on going_events for each row execute function interaction_write_probe()`,
	} {
		if _, err := database.Pool.Exec(context.Background(), statement); err != nil {
			t.Fatalf("install the probes of a run's writes: %v\n%s", err, statement)
		}
	}
}

// interactionWriteHold is the advisory lock a `hold` of interaction_write_probe waits on.
const interactionWriteHold = "19500601"

// planInteractionWrite sets what interaction_write_probe does at the moment of the table's insert:
// `fail`, `hold`, or nothing for an empty act.
func planInteractionWrite(t *testing.T, database *store.Store, table, moment, act string) {
	t.Helper()
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `delete from interaction_write_plan`); err != nil {
		t.Fatalf("clear the probe's plan: %v", err)
	}
	if act == "" {
		return
	}
	if _, err := database.Pool.Exec(ctx, `insert into interaction_write_plan (tbl, moment, act) values ($1, $2, $3)`, table, moment, act); err != nil {
		t.Fatalf("plan %s %s the %s insert: %v", act, moment, table, err)
	}
}

// probeFailure is what the probe's raised error names: the run, and whether the node, its
// authored_by, the frame value and the projected row stood in the transaction as it was failed.
var probeFailure = regexp.MustCompile(`the probe fails the write (before|after) its (going_requests|going_events) row of run ([0-9a-f-]{36}): ` +
	`node=(t|f) authored_by=(t|f) frame=(t|f) row=(t|f)`)

// runHeld is what a select on one connection finds of a run: its interaction node, its authored_by
// tuples, its frame values, its going_requests row and its going_events rows.
type runHeld struct {
	node                                bool
	authoredBy, frames, request, events int
}

func (held runHeld) empty() bool {
	return !held.node && held.authoredBy == 0 && held.frames == 0 && held.request == 0 && held.events == 0
}

// readRunHeld reads, in one select on q, what the store and the two tables hold of run.
func readRunHeld(t *testing.T, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, run string) runHeld {
	t.Helper()
	var held runHeld
	if err := q.QueryRow(context.Background(), `
		select exists (select 1 from graph_nodes where kind = $1 and id = $2),
		       (select count(*) from graph_tuples where from_kind = $1 and from_id = $2 and kind = $3),
		       (select count(*) from graph_tuples where from_kind = $1 and from_id = $2 and kind = $4 and to_kind = $5),
		       (select count(*) from going_requests where id = $2::uuid),
		       (select count(*) from going_events where request_id = $2::uuid)
	`, graphstore.KindInteraction, run, graphstore.TupleAuthoredBy, graphstore.AttributeFrame, graphstore.KindValue).Scan(
		&held.node, &held.authoredBy, &held.frames, &held.request, &held.events); err != nil {
		t.Fatalf("read what the store holds of run %s: %v", run, err)
	}
	return held
}

// loggedStatement is one row interaction_write_logged recorded.
type loggedStatement struct {
	table, what, statement string
	xid                    int64
}

// named is the statement as requireOneTransaction names it: its table, then what it wrote.
func (logged loggedStatement) named() string { return logged.table + " " + logged.what }

// statementsOfRun is every row interaction_write_logged recorded of run after the log's seq after,
// in the order they were written, and the log's last seq.
func statementsOfRun(t *testing.T, database *store.Store, run string, after int64) ([]loggedStatement, int64) {
	t.Helper()
	ctx := context.Background()
	rows, err := database.Pool.Query(ctx, `
		select tbl, what, statement, xid from interaction_write_statements where run = $1 and seq > $2 order by seq`, run, after)
	if err != nil {
		t.Fatalf("read the statements of run %s: %v", run, err)
	}
	statements, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (loggedStatement, error) {
		var logged loggedStatement
		return logged, row.Scan(&logged.table, &logged.what, &logged.statement, &logged.xid)
	})
	if err != nil {
		t.Fatalf("read the statements of run %s: %v", run, err)
	}
	var last int64
	if err := database.Pool.QueryRow(ctx, `select coalesce(max(seq), 0) from interaction_write_statements`).Scan(&last); err != nil {
		t.Fatalf("read the end of the statement log: %v", err)
	}
	return statements, last
}

// requireOneTransaction fails on each statement of written whose transaction id is not the one of
// the statement that wrote the run's node side, nodeWrite, and unless the statements hold each of
// want. Each statement is named `<table> <what>` (loggedStatement.named), and nodeWrite and want
// are prefixes of such a name.
func requireOneTransaction(t *testing.T, what string, written []loggedStatement, nodeWrite string, want ...string) {
	t.Helper()
	xid := int64(-1)
	for _, logged := range written {
		t.Logf("%s: %s in transaction %d: %s", what, logged.named(), logged.xid, strings.Join(strings.Fields(logged.statement), " "))
		if xid < 0 && strings.HasPrefix(logged.named(), nodeWrite) {
			xid = logged.xid
		}
	}
	if xid < 0 {
		t.Fatalf("%s: no statement wrote %s", what, nodeWrite)
	}
	for _, logged := range written {
		if logged.xid != xid {
			t.Errorf("%s: %s was written in transaction %d, and %s in transaction %d: one transaction writes them\n%s",
				what, logged.named(), logged.xid, nodeWrite, xid, logged.statement)
		}
	}
	for _, wanted := range want {
		if !slices.ContainsFunc(written, func(logged loggedStatement) bool { return strings.HasPrefix(logged.named(), wanted) }) {
			t.Errorf("%s: no statement of the transaction that wrote %s wrote %s", what, nodeWrite, wanted)
		}
	}
}

// failedByProbe runs write with the probe planned to fail at the moment of the table's insert, and
// answers the run its error names and what that error says the transaction held of it.
func failedByProbe(t *testing.T, database *store.Store, table, moment string, write func() error) (string, map[string]bool) {
	t.Helper()
	planInteractionWrite(t, database, table, moment, "fail")
	err := write()
	planInteractionWrite(t, database, table, moment, "")
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("the write failed %s its %s row answers %v, want the probe's error", moment, table, err)
	}
	match := probeFailure.FindStringSubmatch(pgErr.Message)
	if match == nil || match[1] != moment || match[2] != table {
		t.Fatalf("the write failed %s its %s row answers %q, want the probe's error at that moment", moment, table, pgErr.Message)
	}
	t.Logf("the write failed %s its %s row: %s", moment, table, pgErr.Message)
	return match[3], map[string]bool{"node": match[4] == "t", "authored_by": match[5] == "t", "frame": match[6] == "t", "row": match[7] == "t"}
}

// heldOpen runs write with the probe planned to hold its transaction open after the table's insert,
// and, once it waits there, calls read; then it lets the write commit and answers its error.
func heldOpen(t *testing.T, database *store.Store, table string, write func() error, read func()) error {
	t.Helper()
	ctx := context.Background()
	holder, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the transaction that holds the write open: %v", err)
	}
	defer holder.Rollback(ctx)
	if _, err := holder.Exec(ctx, `select pg_advisory_xact_lock(`+interactionWriteHold+`)`); err != nil {
		t.Fatalf("take the lock that holds the write open: %v", err)
	}
	planInteractionWrite(t, database, table, "after", "hold")
	done := make(chan error, 1)
	go func() { done <- write() }()
	if waiting := storetest.WaitForLockWaiters(t, holder, 1, 5*time.Second); waiting < 1 {
		t.Fatalf("the write never reached its %s row: no backend waits on the lock that holds it open", table)
	}
	read()
	planInteractionWrite(t, database, table, "after", "")
	if err := holder.Rollback(ctx); err != nil {
		t.Fatalf("let the write commit: %v", err)
	}
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("the write held open never committed")
		return nil
	}
}

// requireAProjectedRowIsWrittenInItsNodesTransaction proves the one transaction by failing it
// rather than by reading the source. With the probes installed it fails the record of a run before
// and after its going_requests row, and a frame before and after its going_events row, and finds
// neither side of the write afterwards, though the failure found the node, its authored_by and the
// frame value standing in the transaction before the row. It records a run and a frame and holds
// every statement naming the run to the transaction of the node write. It fails, then holds open, a
// frame of a run a pod of the release before recorded with no node, which writes the node first in
// the frame's transaction: a second connection reads neither the node nor the frame value while
// that transaction is open, and both in one select after it commits. It holds open a record too,
// which a second connection reads no part of until it commits.
func requireAProjectedRowIsWrittenInItsNodesTransaction(t *testing.T, server *server, database *store.Store) {
	ctx := context.Background()
	pool := database.Pool
	installInteractionWriteProbes(t, database)
	asker := model.Actor{Kind: "user", ID: "alice"}
	record := func(words string) (goingRequest, error) { return server.recordGoingRequest(ctx, asker, "TEST", words) }

	// The record, failed before its row and after it.
	for _, moment := range []string{"before", "after"} {
		run, stood := failedByProbe(t, database, "going_requests", moment, func() error {
			_, err := record("Which run did the probe fail " + moment + " its row?")
			return err
		})
		if !stood["node"] || !stood["authored_by"] || stood["row"] != (moment == "after") {
			t.Errorf("the record failed %s its row stood with %v: the node and its authored_by come before the row, in its transaction", moment, stood)
		}
		if held := readRunHeld(t, pool, run); !held.empty() {
			t.Errorf("the record of run %s failed %s its row left %+v, want neither side", run, moment, held)
		}
	}

	// A record and a frame, each one transaction.
	_, mark := statementsOfRun(t, database, "", 0)
	run, err := record("Which transaction wrote this run?")
	if err != nil {
		t.Fatalf("record a run: %v", err)
	}
	written, mark := statementsOfRun(t, database, run.ID, mark)
	requireOneTransaction(t, "the record of run "+run.ID, written, "graph_nodes "+graphstore.KindInteraction+"/"+run.ID,
		"going_requests", "graph_tuples "+graphstore.KindInteraction+"/"+run.ID+" "+graphstore.TupleAuthoredBy)
	for _, moment := range []string{"before", "after"} {
		failed, stood := failedByProbe(t, database, "going_events", moment, func() error {
			_, err := appendGoingEvent(ctx, pool, run.ID, going.Text("A frame the probe fails "+moment+" its row."))
			return err
		})
		if failed != run.ID || !stood["frame"] || stood["row"] != (moment == "after") {
			t.Errorf("the frame of run %s failed %s its row stood with %v in run %s: its value comes before the row, in its transaction",
				run.ID, moment, stood, failed)
		}
		if held := readRunHeld(t, pool, run.ID); held.frames != 0 || held.events != 0 {
			t.Errorf("the frame of run %s failed %s its row left %+v, want neither its value nor its row", run.ID, moment, held)
		}
	}
	if sequence, err := appendGoingEvent(ctx, pool, run.ID, going.Text("The frame one transaction writes.")); err != nil || sequence != 1 {
		t.Fatalf("append a frame of run %s: sequence %d, %v", run.ID, sequence, err)
	}
	frame, _ := statementsOfRun(t, database, run.ID, mark)
	requireOneTransaction(t, "the frame of run "+run.ID, frame, "graph_tuples "+graphstore.KindInteraction+"/"+run.ID+" "+graphstore.AttributeFrame,
		"going_events going_events 1")
	if len(written) > 0 && len(frame) > 0 && frame[0].xid == written[0].xid {
		t.Errorf("the frame of run %s was written in transaction %d, the run's own: each frame is a transaction of its own", run.ID, frame[0].xid)
	}

	// A run a pod of the release before recorded has no node: its frame writes the node first, in
	// the frame's one transaction.
	older := func() string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, `
			insert into going_requests (author, project, words, ends_at)
			values ('{"kind":"user","id":"alice"}'::jsonb, 'TEST', 'What did the older pod record?', now() + make_interval(secs => $1))
			returning id::text
		`, contracts.GoingRunWindow.Seconds()).Scan(&id); err != nil {
			t.Fatalf("record a run as a pod of the release before: %v", err)
		}
		return id
	}
	failedOlder := older()
	if _, stood := failedByProbe(t, database, "going_events", "before", func() error {
		_, err := appendGoingEvent(ctx, pool, failedOlder, going.Text("A frame of the older pod's run."))
		return err
	}); !stood["node"] || !stood["frame"] {
		t.Errorf("the frame of the older pod's run %s failed before its row stood with %v: the node and the value come first", failedOlder, stood)
	}
	if held := readRunHeld(t, pool, failedOlder); held.node || held.frames != 0 || held.events != 0 {
		t.Errorf("the failed frame of the older pod's run %s left %+v, want no node, no value and no row", failedOlder, held)
	}
	heldOlder := older()
	_, mark = statementsOfRun(t, database, "", 0)
	var sequence int
	err = heldOpen(t, database, "going_events", func() error {
		var err error
		sequence, err = appendGoingEvent(ctx, pool, heldOlder, going.Text("A frame held open."))
		return err
	}, func() {
		second, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatalf("acquire a second connection: %v", err)
		}
		defer second.Release()
		held := readRunHeld(t, second, heldOlder)
		t.Logf("a second connection reads run %s while its frame's transaction is open: %+v", heldOlder, held)
		if held.node || held.frames != 0 || held.events != 0 {
			t.Errorf("a second connection reads %+v of run %s while its frame's transaction is open, want neither its node nor its frame value", held, heldOlder)
		}
	})
	if err != nil || sequence != 1 {
		t.Fatalf("append the held frame of run %s: sequence %d, %v", heldOlder, sequence, err)
	}
	after := readRunHeld(t, pool, heldOlder)
	t.Logf("one select reads run %s after its frame committed: %+v", heldOlder, after)
	if !after.node || after.frames != 1 || after.events != 1 {
		t.Errorf("one select reads %+v of run %s after its frame committed, want its node, its frame value and its row", after, heldOlder)
	}
	statements, _ := statementsOfRun(t, database, heldOlder, mark)
	requireOneTransaction(t, "the frame of the older pod's run "+heldOlder, statements, "graph_nodes "+graphstore.KindInteraction+"/"+heldOlder,
		"graph_tuples "+graphstore.KindInteraction+"/"+heldOlder+" "+graphstore.TupleAuthoredBy,
		"graph_tuples "+graphstore.KindInteraction+"/"+heldOlder+" "+graphstore.AttributeFrame, "going_events going_events 1")

	// A record held open: a second connection reads no run more than it read before.
	count := func(q interface {
		QueryRow(context.Context, string, ...any) pgx.Row
	}) [3]int {
		t.Helper()
		var counts [3]int
		if err := q.QueryRow(ctx, `
			select (select count(*) from graph_nodes where kind = $1),
			       (select count(*) from graph_tuples where from_kind = $1 and kind = $2),
			       (select count(*) from going_requests)
		`, graphstore.KindInteraction, graphstore.TupleAuthoredBy).Scan(&counts[0], &counts[1], &counts[2]); err != nil {
			t.Fatalf("count the runs: %v", err)
		}
		return counts
	}
	before := count(pool)
	var recorded goingRequest
	err = heldOpen(t, database, "going_requests", func() error {
		var err error
		recorded, err = record("Which run is held open?")
		return err
	}, func() {
		second, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatalf("acquire a second connection: %v", err)
		}
		defer second.Release()
		during := count(second)
		t.Logf("a second connection counts interaction nodes, their authored_by and going_requests rows %v before the record and %v while it is open",
			before, during)
		if during != before {
			t.Errorf("a second connection counts %v while the record is open, and %v before it: want no part of the run", during, before)
		}
	})
	if err != nil {
		t.Fatalf("record the run held open: %v", err)
	}
	if held := readRunHeld(t, pool, recorded.ID); !held.node || held.authoredBy != 1 || held.request != 1 {
		t.Errorf("one select reads %+v of run %s after its record committed, want its node, its authored_by and its row", held, recorded.ID)
	}
}

// requireTwoFrameWritersQueueAtTheRunsNode appends two frames of one run from two goroutines at
// once: the first is held open after its row, and the second, started while it is, waits on the
// run's interaction row, which every frame's transaction locks first (graphstore.NodeRowLockSQL).
// Once the first commits the second takes the next sequence, so the two read 1 and 2, one after
// the other, with no gap, each held as a frame value.
func requireTwoFrameWritersQueueAtTheRunsNode(t *testing.T, server *server, database *store.Store) {
	ctx := context.Background()
	run, err := server.recordGoingRequest(ctx, model.Actor{Kind: "user", ID: "alice"}, "TEST", "Which writer takes which frame?")
	if err != nil {
		t.Fatalf("record the run two writers append to: %v", err)
	}
	var first, second int
	secondDone := make(chan error, 1)
	err = heldOpen(t, database, "going_events", func() error {
		var err error
		first, err = appendGoingEvent(ctx, database.Pool, run.ID, going.Text("The first writer's frame. "))
		return err
	}, func() {
		go func() {
			var err error
			second, err = appendGoingEvent(ctx, database.Pool, run.ID, going.Text("The second writer's frame."))
			secondDone <- err
		}()
		waitUntil(t, 5*time.Second, "the second writer waiting on the run's interaction row while the first held it", func() bool {
			waiting, err := storetest.CountLockWaits(ctx, database, graphstore.NodeRowLockSQL)
			if err != nil {
				t.Fatalf("count the writers waiting on the run's node: %v", err)
			}
			return waiting == 1
		})
		t.Logf("the second writer waits on the run's interaction row: %s", graphstore.NodeRowLockSQL)
	})
	if err != nil {
		t.Fatalf("append the first writer's frame: %v", err)
	}
	select {
	case err := <-secondDone:
		if err != nil {
			t.Fatalf("append the second writer's frame: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the second writer never appended its frame")
	}
	t.Logf("the two writers took sequences %d and %d", first, second)
	if first != 1 || second != 2 {
		t.Fatalf("the two writers took sequences %d and %d, want 1 and then 2, with no gap", first, second)
	}
	requireFramesHeldAsValues(t, database, run.ID, 2)
}

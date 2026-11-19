package api

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/going"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// runWritesSeen installs, in the test's own database, a trigger on going_requests and on
// going_events that records, as each row is inserted, what the store holds of that row's run at
// that moment and the id of the transaction inserting it: for a run's row, whether its
// interaction node, its authored_by tuple and that tuple's author node are there; for a frame's
// row, whether the frame value of its sequence is there. So a row written before its node or its
// value reads false, and one written in another transaction reads another id.
func runWritesSeen(t *testing.T, database *store.Store) {
	t.Helper()
	for _, statement := range []string{
		`create table run_writes_seen (
			seen text not null, run text not null, sequence integer,
			node boolean, authored boolean, author boolean, frame boolean, xid bigint not null)`,
		`create function run_writes_seen() returns trigger language plpgsql as $$
		begin
			if tg_table_name = 'going_requests' then
				insert into run_writes_seen (seen, run, node, authored, author, xid) values (
					tg_table_name, new.id::text,
					exists (select 1 from graph_nodes where kind = 'interaction' and id = new.id::text),
					exists (select 1 from graph_tuples where from_kind = 'interaction' and from_id = new.id::text
						and kind = 'authored_by' and asserted),
					exists (select 1 from graph_tuples a join graph_nodes p on p.kind = a.to_kind and p.id = a.to_id
						where a.from_kind = 'interaction' and a.from_id = new.id::text and a.kind = 'authored_by'),
					txid_current());
			else
				insert into run_writes_seen (seen, run, sequence, frame, xid) values (
					tg_table_name, new.request_id::text, new.sequence,
					exists (select 1 from graph_tuples where from_kind = 'interaction' and from_id = new.request_id::text
						and kind = 'frame' and to_kind = 'value' and (to_id::jsonb ->> 'sequence')::integer = new.sequence),
					txid_current());
			end if;
			return new;
		end $$`,
		`create trigger run_writes_seen before insert on going_requests for each row execute function run_writes_seen()`,
		`create trigger run_writes_seen before insert on going_events for each row execute function run_writes_seen()`,
	} {
		if _, err := database.Pool.Exec(context.Background(), statement); err != nil {
			t.Fatalf("install the trigger that reads what a run's rows find: %v", err)
		}
	}
}

// writtenBy is the 32-bit id of the transaction that wrote the one row of table that where
// selects, as its xmin holds it, against which txid_current(), the epoch-extended id the trigger
// records, is read modulo 2^32.
func writtenBy(t *testing.T, database *store.Store, what, table, where string, args ...any) int64 {
	t.Helper()
	var xid int64
	if err := database.Pool.QueryRow(context.Background(), `select xmin::text::bigint from `+table+` where `+where, args...).Scan(&xid); err != nil {
		t.Fatalf("read the transaction that wrote %s: %v", what, err)
	}
	return xid
}

// frameValue is one frame value tuple of a run, decoded as graphstore.FrameValueSQL writes it.
type frameValue struct {
	Sequence int             `json:"sequence"`
	Name     string          `json:"name"`
	Data     json.RawMessage `json:"data"`
	who      model.Actor
}

// runFrameValues is every frame value of the run's interaction node, in the order they were
// appended.
func runFrameValues(t *testing.T, database *store.Store, run string) []frameValue {
	t.Helper()
	tuples, err := graphstore.History(context.Background(), database.Pool, graphstore.KindInteraction, run, graphstore.AttributeFrame)
	if err != nil {
		t.Fatalf("read the frame values of run %s: %v", run, err)
	}
	values := make([]frameValue, 0, len(tuples))
	for _, tuple := range tuples {
		var value frameValue
		if err := json.Unmarshal([]byte(tuple.ToID), &value); err != nil {
			t.Fatalf("decode the frame value %q of run %s: %v", tuple.ToID, run, err)
		}
		value.who = tuple.Who
		values = append(values, value)
	}
	return values
}

// requireFramesHeldAsValues fails unless every going_events row of the run is held by exactly one
// frame value of its interaction node at the same sequence, with the same name and the same data,
// stated by the frame writer, and no value names a sequence no row holds.
func requireFramesHeldAsValues(t *testing.T, database *store.Store, run string, want int) {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select sequence, name, data from going_events where request_id = $1::uuid order by sequence`, run)
	if err != nil {
		t.Fatalf("read the frames of run %s: %v", run, err)
	}
	type frameRow struct {
		sequence int
		name     string
		data     json.RawMessage
	}
	var frames []frameRow
	for rows.Next() {
		var frame frameRow
		if err := rows.Scan(&frame.sequence, &frame.name, &frame.data); err != nil {
			t.Fatalf("scan a frame of run %s: %v", run, err)
		}
		frames = append(frames, frame)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the frames of run %s: %v", run, err)
	}
	if len(frames) != want {
		t.Fatalf("run %s holds %d going_events rows, want %d", run, len(frames), want)
	}
	values := runFrameValues(t, database, run)
	if len(values) != len(frames) {
		t.Fatalf("run %s holds %d frame values for its %d going_events rows: %+v", run, len(values), len(frames), values)
	}
	for position, frame := range frames {
		if frame.sequence != position+1 {
			t.Fatalf("run %s holds frame %d at position %d: the sequences have a gap", run, frame.sequence, position+1)
		}
		held := slices.IndexFunc(values, func(value frameValue) bool { return value.Sequence == frame.sequence })
		if held < 0 {
			t.Fatalf("frame %d of run %s has no frame value", frame.sequence, run)
		}
		value := values[held]
		if value.Name != frame.name || !jsonEqual(t, value.Data, string(frame.data)) || !value.who.SameCredential(graphstore.FrameWriter) {
			t.Fatalf("frame %d of run %s is the row %s %s and the value %+v, want the value to hold the row's name and data, stated by %+v",
				frame.sequence, run, frame.name, frame.data, value, graphstore.FrameWriter)
		}
	}
}

// A run is an interaction node, and its two rows are its projections. The going POST's record
// writes the node, its authored_by tuple and its author's own person node before the
// going_requests row, and each frame writes its value before its going_events row, each in one
// transaction: the trigger on each table finds what the store holds of the row's run as the row
// is inserted, and the id of the transaction that inserted it, which is the id that wrote the
// node, the tuple and the author's node.
func TestARunIsAnInteractionNodeWrittenBeforeItsRowInOneTransaction(t *testing.T) {
	rig := newGoingRig(t)
	runWritesSeen(t, rig.database)
	ctx := context.Background()
	asker := model.Actor{Kind: "user", ID: "run-asker@example.com"}
	run, err := rig.server.recordGoingRequest(ctx, asker, "TEST", "who asked this?")
	if err != nil {
		t.Fatalf("record the run: %v", err)
	}
	for _, event := range []going.Event{going.Text("Somebody "), going.Text("did.")} {
		if _, err := appendGoingEvent(ctx, rig.database.Pool, run.ID, event); err != nil {
			t.Fatalf("append %s: %v", event.Name, err)
		}
	}
	if _, err := endGoingRun(ctx, rig.database.Pool, run.ID, going.End(2, 0)); err != nil {
		t.Fatalf("end the run: %v", err)
	}

	rows, err := rig.database.Pool.Query(ctx, `
		select seen, coalesce(sequence, 0), coalesce(node, true), coalesce(authored, true), coalesce(author, true),
		       coalesce(frame, true), xid % 4294967296
		from run_writes_seen where run = $1 order by xid, sequence nulls first`, run.ID)
	if err != nil {
		t.Fatalf("read what the run's rows found: %v", err)
	}
	type seenRow struct {
		table                         string
		sequence                      int
		node, authored, author, frame bool
		xid                           int64
	}
	var seen []seenRow
	for rows.Next() {
		var row seenRow
		if err := rows.Scan(&row.table, &row.sequence, &row.node, &row.authored, &row.author, &row.frame, &row.xid); err != nil {
			t.Fatalf("scan what a row found: %v", err)
		}
		seen = append(seen, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read what the run's rows found: %v", err)
	}
	t.Logf("the run's rows found %+v", seen)
	if len(seen) != 4 || seen[0].table != "going_requests" {
		t.Fatalf("the run's rows found %+v, want its going_requests row and three going_events rows", seen)
	}
	row := seen[0]
	if !row.node || !row.authored || !row.author {
		t.Fatalf("the going_requests row found node %t, authored_by %t and its author's node %t: the node and its tuples come first",
			row.node, row.authored, row.author)
	}
	for _, written := range []struct {
		what, table, where string
		args               []any
	}{
		{"the interaction node", "graph_nodes", "kind = $1 and id = $2", []any{graphstore.KindInteraction, run.ID}},
		{"its authored_by tuple", "graph_tuples", "from_kind = $1 and from_id = $2 and kind = $3",
			[]any{graphstore.KindInteraction, run.ID, graphstore.TupleAuthoredBy}},
		{"its words value", "graph_tuples", "from_kind = $1 and from_id = $2 and kind = $3",
			[]any{graphstore.KindInteraction, run.ID, graphstore.AttributeWords}},
		{"its author's node", "graph_nodes", "kind = $1 and id = $2", []any{graphstore.KindPerson, asker.ID}},
		{"the going_requests row", "going_requests", "id = $1::uuid", []any{run.ID}},
	} {
		if xid := writtenBy(t, rig.database, written.what, written.table, written.where, written.args...); xid != row.xid {
			t.Errorf("%s was written by transaction %d and the going_requests row by %d: one transaction writes both", written.what, xid, row.xid)
		}
	}
	for _, frame := range seen[1:] {
		if frame.table != "going_events" || !frame.frame {
			t.Fatalf("frame %d's row found its frame value %t: the value comes first", frame.sequence, frame.frame)
		}
		value := writtenBy(t, rig.database, "the frame value", "graph_tuples",
			"from_kind = $1 and from_id = $2 and kind = $3 and (to_id::jsonb ->> 'sequence')::integer = $4",
			graphstore.KindInteraction, run.ID, graphstore.AttributeFrame, frame.sequence)
		if value != frame.xid || value == row.xid {
			t.Errorf("frame %d's value was written by transaction %d, its row by %d and the run by %d: each frame is one transaction of its own",
				frame.sequence, value, frame.xid, row.xid)
		}
	}
	requireFramesHeldAsValues(t, rig.database, run.ID, 3)

	// The node is the run as its row holds it: its made_by the row's author, its words, its
	// ends_at and its project, reserved for its author and live.
	node, held, err := graphstore.ReadNode(ctx, rig.database.Pool, graphstore.KindInteraction, run.ID)
	if err != nil || !held {
		t.Fatalf("read the run's interaction node: held %t, %v", held, err)
	}
	if !node.MadeBy.SameCredential(run.Author) || node.Project != "TEST" || node.Visibility != graphstore.VisibilityAuthor ||
		node.State != graphstore.StateLive || node.Attributes[graphstore.AttributeWords] != run.Words ||
		node.Attributes[graphstore.AttributeEndsAt] != run.EndsAt.UTC().Format(time.RFC3339Nano) {
		t.Fatalf("the run's node is %+v, want the run %+v reserved for its author", node, run)
	}
}

// Two writers appending frames of one run at once - the answer and a reader ending it at its
// deadline, say - queue at the run's interaction node, so every frame takes the sequence after
// the one before it: no sequence is taken twice, none is skipped, and each frame's value and row
// are both there.
func TestTwoWritersAppendingFramesOfOneRunTakeGaplessSequences(t *testing.T) {
	rig := newGoingRig(t)
	ctx := context.Background()
	run := rig.record(t, "how many frames can two writers append?")
	const each = 12
	start := make(chan struct{})
	var wait sync.WaitGroup
	taken := make([][]int, 2)
	failed := make(chan error, 2*each)
	for writer := range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			for frame := range each {
				sequence, err := appendGoingEvent(ctx, rig.database.Pool, run.ID, going.Text(fmt.Sprintf("writer %d frame %d ", writer, frame)))
				if err != nil {
					failed <- fmt.Errorf("writer %d frame %d: %w", writer, frame, err)
					return
				}
				taken[writer] = append(taken[writer], sequence)
			}
		}()
	}
	close(start)
	wait.Wait()
	close(failed)
	for err := range failed {
		t.Fatalf("append a frame: %v", err)
	}
	sequences := slices.Sorted(slices.Values(append(slices.Clone(taken[0]), taken[1]...)))
	for position, sequence := range sequences {
		if sequence != position+1 {
			t.Fatalf("the two writers took %v, want 1 to %d with no gap and no repeat", sequences, 2*each)
		}
	}
	for writer, own := range taken {
		if !slices.IsSorted(own) {
			t.Fatalf("writer %d took %v, out of the order it appended in", writer, own)
		}
	}
	requireFramesHeldAsValues(t, rig.database, run.ID, 2*each)
}

// A run is read by the credential that asked it, through its kind's predicate, which compares the
// whole credential: a run alice's personal token asked is answered to that token, with its words
// as the excerpt of the edge at it, and to neither her cookie nor bob's token, each of which reads
// exactly what an address nobody holds answers. A run whose author is nobody, which only a direct
// call writes, is answered to nobody at all and states no authored_by.
func TestARunIsReadByTheWholeCredentialThatAskedIt(t *testing.T) {
	rig := newGoingRig(t)
	ctx := context.Background()
	pool := rig.database.Pool
	alice, bob := "run-reader-alice@example.com", "run-reader-bob@example.com"
	token := func(owner string) model.Actor {
		return model.Actor{Kind: "session", Owner: &owner, Credential: &model.Credential{TokenID: "token-of-" + owner}}
	}
	aliceToken, aliceCookie, bobToken := token(alice), model.Actor{Kind: "user", ID: alice}, token(bob)

	asked, err := rig.server.recordGoingRequest(ctx, aliceToken, "", "what did my token ask?")
	if err != nil {
		t.Fatalf("record the token's run: %v", err)
	}
	node, found, err := refs.Node(ctx, pool, aliceToken, graphstore.KindInteraction, asked.ID, true)
	if err != nil || !found {
		t.Fatalf("the token that asked reads its run: found %t, %v", found, err)
	}
	if node.Kind != graphstore.KindInteraction || node.Ref != "dispatch://interaction/"+asked.ID || node.IssueKey != nil ||
		node.Parent != nil || node.Visibility != graphstore.VisibilityAuthor || node.State != graphstore.StateLive {
		t.Fatalf("the token that asked reads %+v, want its interaction node at its own address, under no issue", node)
	}
	edges, err := refs.Edges(ctx, pool, aliceToken, refs.Query{Direction: "in", Kind: graphstore.KindPerson, ID: alice})
	if err != nil {
		t.Fatalf("read the edges into alice's person node as her token: %v", err)
	}
	authored := slices.IndexFunc(edges, func(edge model.GraphEdge) bool {
		return edge.Kind == graphstore.TupleAuthoredBy && edge.Node.Kind == graphstore.KindInteraction && edge.Node.ID == asked.ID
	})
	if authored < 0 || edges[authored].Excerpt == nil || edges[authored].Excerpt.Text != asked.Words {
		t.Fatalf("the edges into alice's person node as her token are %+v, want the run's authored_by with its words as the excerpt", edges)
	}
	for name, reader := range map[string]model.Actor{"alice's cookie": aliceCookie, "bob's token": bobToken, "the shared bearer": {}} {
		if node, found, err := refs.Node(ctx, pool, reader, graphstore.KindInteraction, asked.ID, true); err != nil || found {
			t.Errorf("%s reads the token's run: found %t (%+v), %v; want what an address nobody holds answers", name, found, node, err)
		}
		edges, err := refs.Edges(ctx, pool, reader, refs.Query{Direction: "in", Kind: graphstore.KindPerson, ID: alice})
		if err != nil {
			t.Fatalf("read the edges into alice's person node as %s: %v", name, err)
		}
		if slices.ContainsFunc(edges, func(edge model.GraphEdge) bool { return edge.Node.Kind == graphstore.KindInteraction }) {
			t.Errorf("%s reads an interaction among the edges into alice's person node: %+v", name, edges)
		}
	}

	nobodys, err := rig.server.recordGoingRequest(ctx, model.Actor{}, "", "whose run is this?")
	if err != nil {
		t.Fatalf("record a run whose author is nobody: %v", err)
	}
	for name, reader := range map[string]model.Actor{"alice's cookie": aliceCookie, "alice's token": aliceToken, "the shared bearer": {}} {
		if node, found, err := refs.Node(ctx, pool, reader, graphstore.KindInteraction, nobodys.ID, true); err != nil || found {
			t.Errorf("%s reads the run nobody asked: found %t (%+v), %v; want nobody to", name, found, node, err)
		}
	}
	authors, err := graphstore.CurrentOut(ctx, pool, graphstore.KindInteraction, nobodys.ID, []string{graphstore.TupleAuthoredBy})
	if err != nil {
		t.Fatalf("read the authored_by of the run nobody asked: %v", err)
	}
	if len(authors) != 0 {
		t.Fatalf("the run nobody asked states authored_by %+v, want none", authors)
	}
	if _, held, err := graphstore.ReadNode(ctx, pool, graphstore.KindInteraction, nobodys.ID); err != nil || !held {
		t.Fatalf("the run nobody asked holds its node %t (%v), want it written like any run", held, err)
	}
}

// A pod of the release before records a run with no node, during a roll, and stores its frames with
// no frame value. A frame this build writes of that run writes the run's node from its row first,
// through the backfill's own writer for one run (graphcensus.WriteRun), with its authored_by, its
// author's node and a frame value of every frame that pod stored, and then the frame itself, all in
// the frame's one transaction.
func TestAFrameOfARunWithNoNodeWritesTheNodeFirst(t *testing.T) {
	rig := newGoingRig(t)
	ctx := context.Background()
	pool := rig.database.Pool
	var id string
	if err := pool.QueryRow(ctx, `
		insert into going_requests (author, project, words, ends_at)
		values ('{"kind":"user","id":"older-pod-asker@example.com"}'::jsonb, 'TEST', 'what did the older pod record?',
		        now() + make_interval(secs => $1))
		returning id::text
	`, contracts.GoingRunWindow.Seconds()).Scan(&id); err != nil {
		t.Fatalf("record the run as a pod of the release before: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into going_events (request_id, sequence, name, data) values ($1::uuid, 1, 'text', '{"text":"First "}'::jsonb)
	`, id); err != nil {
		t.Fatalf("store the older pod's frame: %v", err)
	}
	if _, held, err := graphstore.ReadNode(ctx, pool, graphstore.KindInteraction, id); err != nil || held {
		t.Fatalf("the older pod's run holds a node %t (%v) before this build writes a frame of it", held, err)
	}

	sequence, err := appendGoingEvent(ctx, pool, id, going.Text("second."))
	if err != nil {
		t.Fatalf("append this build's frame: %v", err)
	}
	if sequence != 2 {
		t.Fatalf("this build's frame took sequence %d, want 2, after the older pod's", sequence)
	}
	node, held, err := graphstore.ReadNode(ctx, pool, graphstore.KindInteraction, id)
	if err != nil || !held {
		t.Fatalf("the run's node once this build wrote a frame of it: held %t, %v", held, err)
	}
	if node.MadeBy.Kind != "user" || node.MadeBy.ID != "older-pod-asker@example.com" || node.Project != "TEST" ||
		node.Visibility != graphstore.VisibilityAuthor || node.Attributes[graphstore.AttributeWords] != "what did the older pod record?" {
		t.Fatalf("the run's node is %+v, want the run its row holds", node)
	}
	authors, err := graphstore.CurrentOut(ctx, pool, graphstore.KindInteraction, id, []string{graphstore.TupleAuthoredBy})
	if err != nil {
		t.Fatalf("read the run's authored_by: %v", err)
	}
	if len(authors) != 1 || authors[0].ToKind != graphstore.KindPerson || authors[0].ToID != "older-pod-asker@example.com" {
		t.Fatalf("the run states authored_by %+v, want one to its asker's person node", authors)
	}
	if _, held, err := graphstore.ReadNode(ctx, pool, graphstore.KindPerson, "older-pod-asker@example.com"); err != nil || !held {
		t.Fatalf("the asker's person node: held %t, %v", held, err)
	}
	requireFramesHeldAsValues(t, rig.database, id, 2)
	frame := writtenBy(t, rig.database, "this build's frame", "going_events", "request_id = $1::uuid and sequence = 2", id)
	for _, written := range []struct {
		what, table, where string
		args               []any
	}{
		{"the run's node", "graph_nodes", "kind = $1 and id = $2", []any{graphstore.KindInteraction, id}},
		{"the older pod's frame's value", "graph_tuples",
			"from_kind = $1 and from_id = $2 and kind = $3 and (to_id::jsonb ->> 'sequence')::integer = 1",
			[]any{graphstore.KindInteraction, id, graphstore.AttributeFrame}},
		{"this build's frame's value", "graph_tuples",
			"from_kind = $1 and from_id = $2 and kind = $3 and (to_id::jsonb ->> 'sequence')::integer = 2",
			[]any{graphstore.KindInteraction, id, graphstore.AttributeFrame}},
	} {
		if xid := writtenBy(t, rig.database, written.what, written.table, written.where, written.args...); xid != frame {
			t.Errorf("%s was written by transaction %d and this build's frame by %d: the frame writes the node first, in its own transaction",
				written.what, xid, frame)
		}
	}
	// The run is read as any run is: by the credential that asked it.
	if _, found, err := refs.Node(ctx, pool, model.Actor{Kind: "user", ID: "older-pod-asker@example.com"}, graphstore.KindInteraction, id, true); err != nil || !found {
		t.Fatalf("the asker reads the run the older pod recorded: found %t, %v", found, err)
	}
}

// The erase of a run takes the run's interaction node row and then its going_requests row, each
// `for update` (grapherase's lockProjectionRows), and every frame of this build takes the node's
// row before the run's. A frame of a run a pod of the release before recorded with no node finds
// no node to take, and writes it under the run's row (graphcensus.WriteRun); but where another
// write committed the node after the frame looked, the frame finds that node under the run's row,
// and an erase may hold it by then. A frame that waited for the node there would wait on the
// erase while the erase waited on it for the run's row, and Postgres would end one of the two
// (40P01). So the frame lets its whole transaction go and begins again, taking the node first.
//
// A transaction holds the run's row `for no key update`, as another writer of the run's node
// holds it, while the frame queues behind it having found no node; the run's node is committed
// meanwhile in a transaction of its own; and a third transaction takes the node's row and then
// asks for the run's, both `for update`, as the erase does, and queues behind the frame. Once the
// run's row is let go, the erase is granted the run's row, with no deadlock, and the frame waits
// on the node's row holding nothing; once the erase lets go, the frame writes the run's first
// frame, held as its value.
func TestAFrameOfARunWithNoNodeTakesNoNodeWhileItHoldsTheRunsRow(t *testing.T) {
	rig := newGoingRig(t)
	ctx := context.Background()
	pool := rig.database.Pool
	var id string
	if err := pool.QueryRow(ctx, `
		insert into going_requests (author, project, words, ends_at)
		values ('{"kind":"user","id":"older-pod-asker@example.com"}'::jsonb, 'TEST', 'which lock does the frame take first?',
		        now() + make_interval(secs => $1))
		returning id::text
	`, contracts.GoingRunWindow.Seconds()).Scan(&id); err != nil {
		t.Fatalf("record the run as a pod of the release before: %v", err)
	}
	begin := func(what string) pgx.Tx {
		t.Helper()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin %s: %v", what, err)
		}
		t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
		return tx
	}

	writer := begin("the transaction holding the run's row")
	if _, err := writer.Exec(ctx, `select 1 from going_requests where id = $1::uuid for no key update`, id); err != nil {
		t.Fatalf("hold the run's row: %v", err)
	}
	type framed struct {
		sequence int
		err      error
	}
	frame := make(chan framed, 1)
	go func() {
		sequence, err := appendGoingEvent(ctx, pool, id, going.Text("A frame of the older pod's run."))
		frame <- framed{sequence, err}
	}()
	if waiting := storetest.WaitForLockWaiters(t, writer, 1, 10*time.Second); waiting < 1 {
		t.Fatalf("the frame never queued behind the run's row, having found no node")
	}

	var run graphstore.Interaction
	var author []byte
	if err := pool.QueryRow(ctx, `
		select id::text, author, project, words, created_at, ends_at from going_requests where id = $1::uuid
	`, id).Scan(&run.ID, &author, &run.Project, &run.Words, &run.At, &run.EndsAt); err != nil {
		t.Fatalf("read the run's row: %v", err)
	}
	if err := json.Unmarshal(author, &run.Author); err != nil {
		t.Fatalf("decode the run's author: %v", err)
	}
	nodes, tuples := graphstore.InteractionFacts(run)
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error { return graphstore.Write(ctx, tx, nodes, tuples) }); err != nil {
		t.Fatalf("commit the run's node while the frame waits: %v", err)
	}

	erase := begin("the erase's transaction")
	if _, err := erase.Exec(ctx, `select 1 from graph_nodes where kind = $1 and id = $2 for update`, graphstore.KindInteraction, id); err != nil {
		t.Fatalf("take the run's node row as the erase does: %v", err)
	}
	erased := make(chan error, 1)
	go func() {
		_, err := erase.Exec(ctx, `select 1 from going_requests where id = $1::uuid for update`, id)
		erased <- err
	}()
	if waiting := storetest.WaitForLockWaiters(t, writer, 2, 10*time.Second); waiting < 2 {
		t.Fatalf("%d transaction(s) queued behind the run's row, want the frame and then the erase", waiting)
	}
	if err := writer.Rollback(ctx); err != nil {
		t.Fatalf("let the run's row go: %v", err)
	}
	select {
	case err := <-erased:
		if err != nil {
			t.Fatalf("the erase's lock of the run's row after the node's: %v; want it granted, with the frame holding nothing it waits for", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the erase never took the run's row after the node's")
	}
	if waiting := storetest.WaitForLockWaiters(t, erase, 1, 10*time.Second); waiting < 1 {
		select {
		case answered := <-frame:
			t.Fatalf("the frame answered sequence %d, %v while the erase held the run's node, want it waiting on the node's row", answered.sequence, answered.err)
		default:
			t.Fatal("the frame never queued behind the erase's hold of the run's node")
		}
	}
	if err := erase.Rollback(ctx); err != nil {
		t.Fatalf("let the erase's locks go: %v", err)
	}
	select {
	case answered := <-frame:
		if answered.err != nil || answered.sequence != 1 {
			t.Fatalf("the frame answered sequence %d, %v, want the run's first frame", answered.sequence, answered.err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the frame never answered once the erase let go")
	}
	requireFramesHeldAsValues(t, rig.database, id, 1)
}

package graphstore

import (
	"context"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }

// A step is one write of this test: the attribute's new value and the relation's new far end,
// stated at one moment, by one writer, caused by one event.
type step struct {
	value string
	far   string
	at    time.Time
	who   model.Actor
	event int64
}

// A tuple as the log holds it, for the check that nothing ever rewrote one.
type loggedTuple struct {
	seq                    int64
	fromKind, fromID, kind string
	toKind, toID           string
	at                     time.Time
	who                    string
	sourceEvent            int64
	legacySourceSeq        *int64
	asserted               bool
}

// What two histories must agree on, tuple by tuple, for the two reads to be one read: every
// field but the ones that say which fact was asked for (Kind, ToKind, ToID) and where in the log
// it landed (Seq). A far end is set in both, holding a value in the attribute's history and an
// address in the relation's.
type historyShape struct {
	fromKind, fromID string
	at               time.Time
	who              model.Actor
	sourceEvent      int64
	legacySourceSeq  *int64
	asserted         bool
	farEndSet        bool
}

func shapeOf(tuple Tuple) historyShape {
	return historyShape{
		fromKind:        tuple.FromKind,
		fromID:          tuple.FromID,
		at:              tuple.At.UTC(),
		who:             tuple.Who,
		sourceEvent:     tuple.SourceEvent,
		legacySourceSeq: tuple.LegacySourceSeq,
		asserted:        tuple.Asserted,
		farEndSet:       tuple.ToKind != "" && tuple.ToID != "",
	}
}

// An attribute's history and a relation's are one read: History serves both, and a caller cannot
// tell from the answer which of the two it asked for. The test changes one attribute of one node
// three times and one relation of that node three times, each pair in one write, then reads both
// histories and compares them field by field. It also holds the two rules the store rests on: the
// node row carries the newest value of the attribute, and no statement of the run ever rewrote or
// removed a tuple.
func TestAnAttributeHistoryReadsBackAsARelationHistoryDoes(t *testing.T) {
	ctx := context.Background()
	pool := storetest.Open(t).Pool

	const (
		nodeKind  = "block"
		nodeID    = "CORE/plan#b1"
		attribute = "text"
		relation  = "part_of"
	)
	// Postgres keeps a timestamptz to the microsecond, so the times written are the times read
	// back and an equality is an equality.
	start := time.Now().UTC().Truncate(time.Microsecond)
	steps := []step{
		{value: "the block as it was first written", far: "CORE/plan", at: start,
			who: model.Actor{Kind: "user", ID: "alice"}, event: 11},
		{value: "the block after Bob rewrote its first line", far: "CORE/plan-v2", at: start.Add(time.Minute),
			who: model.Actor{Kind: "user", ID: "bob"}, event: 12},
		{value: "the block as the agent left it", far: "CORE/plan-v3", at: start.Add(2 * time.Minute),
			who: model.Actor{Kind: "session", ID: "session-0123456789abcdef"}, event: 13},
	}

	before := readTupleLog(t, ctx, pool)
	for number, written := range steps {
		writeStep(t, ctx, pool, nodeKind, nodeID, attribute, relation, written)
		after := readTupleLog(t, ctx, pool)
		// Two tuples per step, appended above every tuple already there, and every earlier row
		// exactly as it was: an update or a delete moves the count, the highest sequence, or one
		// of the rows the prefix holds.
		if len(after) != len(before)+2 {
			t.Fatalf("step %d: the log holds %d tuples, want %d", number+1, len(after), len(before)+2)
		}
		if highestSeq(after) <= highestSeq(before) {
			t.Fatalf("step %d: the highest sequence is %d, want it above %d",
				number+1, highestSeq(after), highestSeq(before))
		}
		if len(before) > 0 && !reflect.DeepEqual(after[:len(before)], before) {
			t.Fatalf("step %d: the tuples written before it changed:\nbefore %+v\nafter  %+v",
				number+1, before, after[:len(before)])
		}
		before = after
	}

	attributeHistory, err := History(ctx, pool, nodeKind, nodeID, attribute)
	if err != nil {
		t.Fatalf("read the attribute's history: %v", err)
	}
	relationHistory, err := History(ctx, pool, nodeKind, nodeID, relation)
	if err != nil {
		t.Fatalf("read the relation's history: %v", err)
	}
	if len(attributeHistory) != len(steps) || len(relationHistory) != len(steps) {
		t.Fatalf("the histories hold %d values and %d far ends, want %d of each",
			len(attributeHistory), len(relationHistory), len(steps))
	}
	for number, written := range steps {
		value, far := attributeHistory[number], relationHistory[number]
		// The attribute's history: three values, each with its time, its writer and the write
		// that caused it, in the order they were written.
		if value.ToKind != KindValue || value.ToID != written.value {
			t.Errorf("value %d is %s/%q, want %s/%q", number+1, value.ToKind, value.ToID, KindValue, written.value)
		}
		if !value.At.Equal(written.at) || !value.Who.SameAs(written.who) || value.SourceEvent != written.event {
			t.Errorf("value %d was written at %s by %+v under event %d, want %s, %+v and %d",
				number+1, value.At, value.Who, value.SourceEvent, written.at, written.who, written.event)
		}
		if far.ToKind != "artifact" || far.ToID != written.far {
			t.Errorf("far end %d is %s/%q, want artifact/%q", number+1, far.ToKind, far.ToID, written.far)
		}
		// The same read over the relation answers the same shape: a difference in any field but
		// the kind asked for, the far end's address and the log position is a difference between
		// two reads, and these are one read.
		if valueShape, farShape := shapeOf(value), shapeOf(far); !reflect.DeepEqual(valueShape, farShape) {
			t.Errorf("tuple %d reads back differently as an attribute and as a relation:\nattribute %+v\nrelation  %+v",
				number+1, valueShape, farShape)
		}
	}

	// The node row is the projection of its tuples: its attributes column holds the newest value.
	newest := steps[len(steps)-1].value
	var projected string
	if err := pool.QueryRow(ctx,
		`select attributes->>$3 from graph_nodes where kind = $1 and id = $2`,
		nodeKind, nodeID, attribute).Scan(&projected); err != nil {
		t.Fatalf("read the node's attributes column: %v", err)
	}
	if projected != newest {
		t.Errorf("graph_nodes.attributes holds %q for %s, want the newest value %q", projected, attribute, newest)
	}
	node, found, err := ReadNode(ctx, pool, nodeKind, nodeID)
	if err != nil || !found {
		t.Fatalf("read the node: found %v, %v", found, err)
	}
	if node.Attributes[attribute] != newest {
		t.Errorf("the node reads back with %s %v, want %q", attribute, node.Attributes[attribute], newest)
	}

	// A value is no node, so it is the far end of no edge: graph_edges answers the projections,
	// and a literal a block holds is never an address in it.
	for _, written := range steps {
		var edges int
		if err := pool.QueryRow(ctx, `select count(*) from graph_edges where to_id = $1`, written.value).Scan(&edges); err != nil {
			t.Fatalf("read graph_edges for the value %q: %v", written.value, err)
		}
		if edges != 0 {
			t.Errorf("graph_edges holds %d edges whose far end is the value %q, want none", edges, written.value)
		}
	}
}

// writeStep states one step's two facts in one transaction, and writes the node row the two
// project to.
func writeStep(t *testing.T, ctx context.Context, pool *store.Pool, nodeKind, nodeID, attribute, relation string, written step) {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	node := Node{
		Kind: nodeKind, ID: nodeID, Supertag: "block",
		Visibility: VisibilityProject, State: StateLive,
		Attributes: map[string]any{attribute: written.value},
		MadeAt:     written.at, MadeBy: written.who, SourceEvent: written.event,
	}
	tuples := []Tuple{
		{FromKind: nodeKind, FromID: nodeID, Kind: attribute, ToKind: KindValue, ToID: written.value,
			At: written.at, Who: written.who, SourceEvent: written.event, Asserted: true},
		{FromKind: nodeKind, FromID: nodeID, Kind: relation, ToKind: "artifact", ToID: written.far,
			At: written.at, Who: written.who, SourceEvent: written.event, Asserted: true},
	}
	if err := Write(ctx, tx, []Node{node}, tuples); err != nil {
		t.Fatalf("write the step written at %s: %v", written.at, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the step written at %s: %v", written.at, err)
	}
}

// readTupleLog answers every tuple row the store holds, oldest first.
func readTupleLog(t *testing.T, ctx context.Context, pool *store.Pool) []loggedTuple {
	t.Helper()
	rows, err := pool.Query(ctx, `
		select seq, from_kind, from_id, kind, to_kind, to_id, at, who::text, source_event, legacy_source_seq, asserted
		from graph_tuples order by seq`)
	if err != nil {
		t.Fatalf("read the tuple log: %v", err)
	}
	defer rows.Close()
	var log []loggedTuple
	for rows.Next() {
		var row loggedTuple
		if err := rows.Scan(&row.seq, &row.fromKind, &row.fromID, &row.kind, &row.toKind, &row.toID,
			&row.at, &row.who, &row.sourceEvent, &row.legacySourceSeq, &row.asserted); err != nil {
			t.Fatalf("read the tuple log: %v", err)
		}
		log = append(log, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the tuple log: %v", err)
	}
	return log
}

func highestSeq(log []loggedTuple) int64 {
	if len(log) == 0 {
		return 0
	}
	return log[len(log)-1].seq
}

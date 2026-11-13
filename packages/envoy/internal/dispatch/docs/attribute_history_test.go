package docs

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The versions of the document this test writes. Version 1 holds two callouts side by side,
// version 2 drops the second, subject, version 3 brings it back where it was, and version 4
// moves it under the first, holder. Between them subject's state is stated three times - live,
// gone, live - and its part_of three times: the document asserted, the document retracted and
// holder asserted.
const (
	historyFirst = ":::callout{#holder kind=\"note\" title=\"\"}\nThe block that later holds the subject.\n:::\n\n" +
		":::callout{#subject kind=\"note\" title=\"\"}\nThe block whose history this test reads.\n:::\n"
	historyDropped = ":::callout{#holder kind=\"note\" title=\"\"}\nThe block that later holds the subject.\n:::\n"
	historyMoved   = "::::callout{#holder kind=\"note\" title=\"\"}\nThe block that later holds the subject.\n\n" +
		":::callout{#subject kind=\"note\" title=\"\"}\nThe block whose history this test reads.\n:::\n\n::::\n"
)

// storedTuple is one row of graph_tuples as the log holds it, xmin included: an update that
// wrote a row back with the same values still moves the transaction that last wrote it.
type storedTuple struct {
	seq                    int64
	fromKind, fromID, kind string
	toKind, toID           string
	at                     time.Time
	who                    string
	sourceEvent            int64
	legacySourceSeq        *int64
	asserted               bool
	xmin                   string
}

// storedTupleLog is every tuple row the store holds, oldest first.
func storedTupleLog(t *testing.T, database *store.Store) []storedTuple {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select seq, from_kind, from_id, kind, to_kind, to_id, at, who::text, source_event, legacy_source_seq, asserted, xmin::text
		from graph_tuples order by seq`)
	if err != nil {
		t.Fatalf("read the tuple log: %v", err)
	}
	defer rows.Close()
	var log []storedTuple
	for rows.Next() {
		var row storedTuple
		if err := rows.Scan(&row.seq, &row.fromKind, &row.fromID, &row.kind, &row.toKind, &row.toID,
			&row.at, &row.who, &row.sourceEvent, &row.legacySourceSeq, &row.asserted, &row.xmin); err != nil {
			t.Fatalf("read the tuple log: %v", err)
		}
		log = append(log, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the tuple log: %v", err)
	}
	return log
}

// requireOnlyAppended fails unless the log after a step is the log before it, with any rows the
// step added above its highest sequence: the row count and the highest sequence never move down,
// and every row the step found is still there exactly as it was. An update or a delete of a
// tuple row breaks one of the three. How many rows a step adds is the histories' to read below.
func requireOnlyAppended(t *testing.T, step string, before, after []storedTuple) {
	t.Helper()
	highest := func(log []storedTuple) int64 {
		if len(log) == 0 {
			return 0
		}
		return log[len(log)-1].seq
	}
	if len(after) < len(before) || highest(after) < highest(before) {
		t.Fatalf("%s: the tuple log went from %d rows up to sequence %d to %d rows up to sequence %d: a row was deleted",
			step, len(before), highest(before), len(after), highest(after))
	}
	for index := range before {
		if !reflect.DeepEqual(after[index], before[index]) {
			t.Fatalf("%s: tuple %d was rewritten from %+v to %+v: no statement may update or delete a tuple row",
				step, before[index].seq, before[index], after[index])
		}
	}
	t.Logf("%s appended %d tuples, up to sequence %d", step, len(after)-len(before), highest(after))
}

// versionCause is what one version of the document says about the facts its index stated: when
// the version was written, the writer its row records, and the event that recorded it.
type versionCause struct {
	number int
	at     time.Time
	writer model.Actor
	event  int64
}

// causeOfVersion reads a version's own row for its time and its writer, the first of its
// authors (refs.indexedVersionRecord), beside the event that recorded it.
func causeOfVersion(t *testing.T, database *store.Store, artifactID string, number int, event int64) versionCause {
	t.Helper()
	cause := versionCause{number: number, event: event}
	var authors []byte
	if err := database.Pool.QueryRow(context.Background(), `
		select created_at, authors from artifact_versions where artifact_id = $1 and number = $2
	`, artifactID, number).Scan(&cause.at, &authors); err != nil {
		t.Fatalf("read version %d of %s: %v", number, artifactID, err)
	}
	var writers []model.Actor
	if err := json.Unmarshal(authors, &writers); err != nil || len(writers) == 0 {
		t.Fatalf("version %d of %s records the authors %s (%v), want a writer", number, artifactID, authors, err)
	}
	cause.writer = writers[0]
	return cause
}

// tupleShape is what an attribute's history and a relation's must agree on, row by row, for the
// two reads to be one read: the node the row is about, and which of the fields every history
// row carries are set - its far end, its time, its writer, its cause - and whether it holds a
// legacy sequence. The values those fields hold are each history's own, since the two facts
// were stated by different versions.
type tupleShape struct {
	fromKind, fromID string
	farEndSet        bool
	atSet            bool
	writerSet        bool
	causeSet         bool
	legacySeqSet     bool
}

func shapeOfTuple(tuple graphstore.Tuple) tupleShape {
	return tupleShape{
		fromKind:     tuple.FromKind,
		fromID:       tuple.FromID,
		farEndSet:    tuple.ToKind != "" && tuple.ToID != "",
		atSet:        !tuple.At.IsZero(),
		writerSet:    tuple.Who.Kind != "" && tuple.Who.ID != "",
		causeSet:     tuple.SourceEvent != graphstore.NoEvent,
		legacySeqSet: tuple.LegacySourceSeq != nil,
	}
}

// requireStatedBy fails unless one history row carries the time, the writer and the event of
// the version that stated it.
func requireStatedBy(t *testing.T, what string, tuple graphstore.Tuple, cause versionCause) {
	t.Helper()
	if !tuple.At.Equal(cause.at) || !tuple.Who.SameAs(cause.writer) || tuple.SourceEvent != cause.event {
		t.Errorf("%s was stated at %s by %+v under event %d, want version %d's time %s, writer %+v and event %d",
			what, tuple.At, tuple.Who, tuple.SourceEvent, cause.number, cause.at, cause.writer, cause.event)
	}
}

// An attribute's history and a relation's are one read, and that holds for what the production
// writer states and not only for tuples a test writes by hand. One block of a document is
// written through the real version write path four times: the request that creates the document
// (api's storeArtifact, IndexDocumentReferences), then three named versions (NamedVersion, the
// version's event, refs.BlockIndexWrite.Caused). The block index states the block's state
// attribute three times - live, gone when a version drops it, live when the next brings it
// back - and its part_of relation three times - the document asserted, then retracted, and the
// block that comes to hold it asserted. graphstore.History reads both.
//
// The attribute's history is three values, in the order they were written, each with the time,
// the writer and the event of the version that stated it, and the relation's history is the
// same read: the two are compared row by row for their shape. The node row's state column is
// the newest value. No statement of the run updates or deletes a tuple row, which is read off
// the tuple log's row count, highest sequence and every earlier row before and after each
// write. And a value is no node, so with every cutover row on, the view's tuple arm answers the
// block's own edge and no edge whose far end is one of its values.
func TestAnAttributeHistoryWrittenByTheBlockIndexReadsBackAsARelationHistoryDoes(t *testing.T) {
	ctx := context.Background()
	database, service := blockIndexService(t)

	log := storedTupleLog(t, database)
	document := writeDocumentThroughTheRequestPath(t, service, database, "history", historyFirst)
	settle(t, database, document)
	after := storedTupleLog(t, database)
	requireOnlyAppended(t, "the request that writes version 1", log, after)
	log = after
	var created int64
	if err := database.Pool.QueryRow(ctx, `
		select id from events where type = 'artifact.created' and payload->'artifact'->>'id' = $1
	`, document).Scan(&created); err != nil {
		t.Fatalf("read the event that recorded version 1: %v", err)
	}
	causes := []versionCause{causeOfVersion(t, database, document, 1, created)}
	if stands := blockIndexAt(t, database, document); stands == nil || *stands != 1 {
		t.Fatalf("the request left the document's blocks at %s, want version 1 indexed in the request", standsText(stands))
	}
	subject, holder := blockAddress(t, database, document, "subject"), blockAddress(t, database, document, "holder")
	var refKey string
	if err := database.Pool.QueryRow(ctx, `select ref_key from artifacts where id = $1`, document).Scan(&refKey); err != nil {
		t.Fatalf("read the document's address: %v", err)
	}

	for _, step := range []struct{ what, markdown string }{
		{"version 2, which drops subject", historyDropped},
		{"version 3, which brings subject back", historyFirst},
		{"version 4, which moves subject under holder", historyMoved},
	} {
		if _, err := joinedReplaceText(service, document, step.markdown, model.Actor{Kind: "user", ID: "alice"}); err != nil {
			t.Fatalf("edit the document %s records: %v", step.what, err)
		}
		written, event := writeVersionThroughTheRealPath(t, service, document, step.what)
		after := storedTupleLog(t, database)
		requireOnlyAppended(t, step.what, log, after)
		log = after
		causes = append(causes, causeOfVersion(t, database, document, written.Version.Number, event))
	}
	if causes[3].number != 4 {
		t.Fatalf("the last version written is %d, want 4", causes[3].number)
	}

	attribute, err := graphstore.History(ctx, database.Pool, refs.BlockNodeKind, subject, "state")
	if err != nil {
		t.Fatalf("read the history of %s's state: %v", subject, err)
	}
	relation, err := graphstore.History(ctx, database.Pool, refs.BlockNodeKind, subject, graphstore.TuplePartOf)
	if err != nil {
		t.Fatalf("read the history of %s's part_of: %v", subject, err)
	}
	if len(attribute) != 3 || len(relation) != 3 {
		t.Fatalf("%s's histories hold %d state values and %d part_of tuples, want three of each:\nstate   %+v\npart_of %+v",
			subject, len(attribute), len(relation), attribute, relation)
	}

	// The attribute's history: three values, each with its time, its writer and the write that
	// caused it, in the order they were written.
	for index, want := range []struct {
		value string
		cause versionCause
	}{
		{graphstore.StateLive, causes[0]},
		{graphstore.StateGone, causes[1]},
		{graphstore.StateLive, causes[2]},
	} {
		value := attribute[index]
		if value.ToKind != graphstore.KindValue || value.ToID != want.value || !value.Asserted {
			t.Errorf("state value %d of %s is %s/%q (asserted %t), want the value %q", index+1, subject, value.ToKind, value.ToID, value.Asserted, want.value)
		}
		requireStatedBy(t, "state value "+want.value+" of "+subject, value, want.cause)
	}
	// The relation's history: the document asserted by version 1, then retracted and holder
	// asserted by version 4, which moved the block.
	for index, want := range []struct {
		toKind, toID string
		asserted     bool
		cause        versionCause
	}{
		{"artifact", refKey, true, causes[0]},
		{"artifact", refKey, false, causes[3]},
		{refs.BlockNodeKind, holder, true, causes[3]},
	} {
		far := relation[index]
		if far.ToKind != want.toKind || far.ToID != want.toID || far.Asserted != want.asserted {
			t.Errorf("part_of tuple %d of %s is %s/%s (asserted %t), want %s/%s (asserted %t)",
				index+1, subject, far.ToKind, far.ToID, far.Asserted, want.toKind, want.toID, want.asserted)
		}
		requireStatedBy(t, "part_of tuple of "+subject+" to "+want.toKind+"/"+want.toID, far, want.cause)
	}
	// The two reads are one read: row for row, the same node, the same fields set, in log
	// order and in time order alike.
	for index := range attribute {
		if value, far := shapeOfTuple(attribute[index]), shapeOfTuple(relation[index]); value != far {
			t.Errorf("row %d reads back differently as an attribute and as a relation:\nattribute %+v\nrelation  %+v",
				index+1, value, far)
		}
		if index > 0 {
			for _, history := range [][]graphstore.Tuple{attribute, relation} {
				if history[index].Seq <= history[index-1].Seq || history[index].At.Before(history[index-1].At) {
					t.Errorf("%s history row %d (tuple %d at %s) does not follow row %d (tuple %d at %s)",
						history[index].Kind, index+1, history[index].Seq, history[index].At,
						index, history[index-1].Seq, history[index-1].At)
				}
			}
		}
	}

	// The node row is the projection of its tuples: its state column holds the newest value.
	node, found, err := graphstore.ReadNode(ctx, database.Pool, refs.BlockNodeKind, subject)
	if err != nil || !found {
		t.Fatalf("read the node of %s: found %t, %v", subject, found, err)
	}
	if newest := attribute[len(attribute)-1].ToID; node.State != newest {
		t.Errorf("graph_nodes.state holds %q for %s, want the newest value %q", node.State, subject, newest)
	}

	// A value is no node, so it is the far end of no edge. Every kind is marked complete, which
	// is the view a deployment reads once its backfill has run; this database is this test's
	// own. The block's own edge is answered, so the arm is live and a zero below is the value
	// being kept out rather than the arm answering nothing.
	if _, err := database.Pool.Exec(ctx, `update graph_store_cutover set complete = true`); err != nil {
		t.Fatalf("mark every kind's cutover complete: %v", err)
	}
	var edges, literals int
	if err := database.Pool.QueryRow(ctx, `
		select count(*) filter (where kind = 'part_of' and to_kind = 'artifact' and to_id = $3),
		       count(*) filter (where to_kind = $4 or to_id = any($5))
		from graph_edges where from_kind = $1 and from_id = $2
	`, refs.BlockNodeKind, subject, refKey, graphstore.KindValue,
		[]string{graphstore.StateLive, graphstore.StateGone, graphstore.VisibilityProject}).Scan(&edges, &literals); err != nil {
		t.Fatalf("read the edges of %s: %v", subject, err)
	}
	if edges != 1 {
		t.Errorf("graph_edges holds %d edges from %s to its document %s, want the one its in_document tuple states: with every cutover complete the tuple arm answers it",
			edges, subject, refKey)
	}
	if literals != 0 {
		t.Errorf("graph_edges holds %d edges from %s whose far end is a value, want none: the tuple arm excludes a literal far end", literals, subject)
	}
}

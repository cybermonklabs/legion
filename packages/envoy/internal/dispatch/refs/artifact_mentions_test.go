package refs_test

import (
	"context"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest/lockstep"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// mentionsServer is the server the test's references are written against.
const mentionsServer = "https://dispatch.example"

// mentionsAuthor is the author of every version and row the test writes.
var mentionsAuthor = model.Actor{Kind: "user", ID: "alice"}

// statedMention is one mentions tuple out of a document's node, as the log holds it.
type statedMention struct {
	toKind, toID string
	asserted     bool
	at           time.Time
	legacy       *int64
	cause        int64
	who          model.Actor
}

// mentionsOf is every mentions tuple out of the node at refKey, in the order the log holds them.
func mentionsOf(t *testing.T, database *store.Store, refKey string) []statedMention {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select to_kind, to_id, asserted, at, legacy_source_seq, source_event, who from graph_tuples
		where from_kind = 'artifact' and from_id = $1 and kind = 'mentions'
		order by seq
	`, refKey)
	if err != nil {
		t.Fatalf("read the mentions of %s: %v", refKey, err)
	}
	stated, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (statedMention, error) {
		var mention statedMention
		err := row.Scan(&mention.toKind, &mention.toID, &mention.asserted, &mention.at, &mention.legacy, &mention.cause, &mention.who)
		return mention, err
	})
	if err != nil {
		t.Fatalf("read the mentions of %s: %v", refKey, err)
	}
	return stated
}

// mentionsEdge is how graph_edges answers the mention of toKind toID by the document whose uuid is
// document: how many rows, and the created_at and source_seq of the last.
func mentionsEdge(t *testing.T, database *store.Store, document, toKind, toID string) (int, time.Time, *int64) {
	t.Helper()
	var rows int
	var at *time.Time
	var seq *int64
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*), max(created_at), max(source_seq) from graph_edges
		where from_kind = 'artifact' and from_id = $1 and kind = 'mentions' and to_kind = $2 and to_id = $3
	`, document, toKind, toID).Scan(&rows, &at, &seq); err != nil {
		t.Fatalf("read %s's mention of %s %s out of graph_edges: %v", document, toKind, toID, err)
	}
	if at == nil {
		return rows, time.Time{}, seq
	}
	return rows, *at, seq
}

// requireMentionsEdge fails unless graph_edges answers the document's mention of toKind toID once,
// dated at and caused by seq (nil: none).
func requireMentionsEdge(t *testing.T, what string, database *store.Store, document, toKind, toID string, at time.Time, seq *int64) {
	t.Helper()
	rows, answeredAt, answeredSeq := mentionsEdge(t, database, document, toKind, toID)
	if rows != 1 || !answeredAt.Equal(at) || !sameSeq(answeredSeq, seq) {
		t.Errorf("%s: graph_edges answers the mention of %s %s %d times at %s with source_seq %s, want once at %s with %s",
			what, toKind, toID, rows, answeredAt, seqText(answeredSeq), at, seqText(seq))
	}
}

// sameSeq is whether two source_seqs are the same one, or both none.
func sameSeq(left, right *int64) bool {
	return left == nil && right == nil || left != nil && right != nil && *left == *right
}

// seqText is a source_seq as a failure names it.
func seqText(seq *int64) string {
	if seq == nil {
		return "none"
	}
	return strconv.FormatInt(*seq, 10)
}

// writeMentionsVersion writes version number of document, citing what markdown cites, as a
// version write does: the version's row, its references planned and queued as the version index
// does (refs.PlanReplace, refs.Replacement.Queue) with the tuples of the document's mentions
// (refs.Replacement.ArtifactMentions), the version's event, the stamp of the references the
// reconcile inserted with that event, and what the reconcile stated, appended under it. It
// answers the version's time and its event.
func writeMentionsVersion(t *testing.T, database *store.Store, document string, number int, markdown string) (time.Time, int64) {
	t.Helper()
	ctx := context.Background()
	var at time.Time
	var event int64
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			insert into artifact_versions (artifact_id, number, markdown, authors)
			values ($1::uuid, $2, $3, '[{"kind":"user","id":"alice"}]')
			returning created_at
		`, document, number, markdown).Scan(&at); err != nil {
			return err
		}
		replacement, err := refs.PlanReplace(ctx, tx, "artifact", document, text.Extract(markdown, mentionsServer), &at)
		if err != nil {
			return err
		}
		record, err := refs.ReadIndexedVersion(ctx, tx, document, document, number)
		if err != nil {
			return err
		}
		mentions, err := replacement.ArtifactMentions(ctx, tx, record)
		if err != nil {
			return err
		}
		if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
			replacement.Queue(b)
			return nil
		}); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `
			insert into events (issue_key, seq, type, actor, payload, notify)
			select 'ACME-1', coalesce(max(seq), 0) + 1, 'artifact.version', '{"kind":"user","id":"alice"}',
			       jsonb_build_object('artifact_id', $1::text, 'version', jsonb_build_object('number', $2::int)), false
			from events where issue_key = 'ACME-1'
			returning id
		`, document, number).Scan(&event); err != nil {
			return err
		}
		if err := refs.Stamp(ctx, tx, graphstore.KindArtifact, document, event); err != nil {
			return err
		}
		return graphstore.Caused(ctx, tx, graphstore.Uncaused{Tuples: mentions}, event)
	}); err != nil {
		t.Fatalf("write version %d of %s: %v", number, document, err)
	}
	return at, event
}

// referencesKind is the census's line for the references a document makes.
func referencesKind(t *testing.T, database *store.Store) graphcensus.KindReport {
	t.Helper()
	report, err := graphcensus.StoreCensus(context.Background(), database.Pool.Config().ConnString(), false)
	if err != nil {
		t.Fatalf("take the store census: %v", err)
	}
	for _, kind := range report.Kinds {
		if kind.Kind == "artifact_reference" {
			return kind
		}
	}
	t.Fatalf("the census reports no kind of the references a document makes: %#v", report.Kinds)
	return graphcensus.KindReport{}
}

// backfillReferences runs the whole backfill and answers its line for the references a document
// makes.
func backfillReferences(t *testing.T, database *store.Store) graphcensus.KindBackfill {
	t.Helper()
	report, err := graphcensus.Backfill(context.Background(), database, io.Discard)
	if err != nil {
		t.Fatalf("backfill the store: %v", err)
	}
	for _, kind := range report.Kinds {
		if kind.Kind == "artifact_reference" {
			return kind
		}
	}
	t.Fatalf("the backfill reports no kind of the references a document makes: %#v", report.Kinds)
	return graphcensus.KindBackfill{}
}

// seedMentionsDocument writes project ACME, its issues ACME-1 to ACME-5, and the spec of ACME-1
// (markedDocument), made at created. It answers the spec's uuid and its ref_key.
func seedMentionsDocument(t *testing.T, database *store.Store, created time.Time) (string, string) {
	t.Helper()
	ctx := context.Background()
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `insert into projects (key, name, default_assignee) values ('ACME', 'Acme', 'alice')`); err != nil {
			return err
		}
		for number := 1; number <= 5; number++ {
			if _, err := tx.Exec(ctx, `
				insert into issues (key, project_key, number, title, created_by, rank)
				values ('ACME-' || $1::int, 'ACME', $1, 'Cited', '{"kind":"user","id":"alice"}', 'U')
			`, number); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed the project and its issues: %v", err)
	}
	return markedDocument(t, database, "spec", created)
}

// markedDocument writes document slug of ACME-1, made at created, whose row names its node, as its
// upload writes both (api's createArtifact). It answers the document's uuid and its ref_key.
func markedDocument(t *testing.T, database *store.Store, slug string, created time.Time) (string, string) {
	t.Helper()
	ctx := context.Background()
	var document, refKey string
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			insert into artifacts (issue_key, project_key, slug, name, kind, created_by, created_at)
			values ('ACME-1', 'ACME', $1, $1::text || '.md', 'doc', '{"kind":"user","id":"alice"}', $2)
			returning id::text, ref_key
		`, slug, created).Scan(&document, &refKey); err != nil {
			return err
		}
		if err := graphstore.Write(ctx, tx, []graphstore.Node{graphstore.ArtifactNode(refKey, "ACME", "", mentionsAuthor, created)}, nil); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `update artifacts set node_kind = $2, node_address = ref_key where id = $1::uuid`, document, graphstore.KindArtifact)
		return err
	}); err != nil {
		t.Fatalf("seed document %s: %v", slug, err)
	}
	return document, refKey
}

// The references a document makes are tuples: its version index (refs.Replacement.ArtifactMentions)
// appends a mentions tuple out of the document's node to each far end a refs row it inserts names
// and a retraction to each a row it deletes named, in the transaction that writes the rows, and
// graph_edges answers a document's mention from that tuple while the row stands. A version that
// cites two issues and a document whose address is past the store's bound states the two issues'
// tuples, each dated at its row's created_at and carrying the row's source_seq, the version's
// event, and no tuple of the document past the bound, which the arm over refs answers. A second
// version that keeps one issue, drops the other and cites a third retracts the dropped one, dated
// at its time, asserts the third with its own time and event, and states nothing of the one it
// kept. A comment's reference writes no tuple. A reference of a document whose row names no node,
// written by SQL as the release before writes one, is answered by the arm over refs until the
// backfill, which names the document's node and states the tuple, dated at the row's created_at
// and carrying its source_seq. A row of the first document whose tuple is deleted by SQL is a row
// no tuple states, which the census names and the backfill writes again; a row of it the release
// before deletes, retracting nothing, leaves a tuple no row projects, which no arm answers, the
// census names and the backfill retracts. Last, a row rebuild-refs inserts carries no source_seq,
// and once a later version's event stamps the row, graph_edges answers the edge with the row's
// source_seq, as the arm over refs does.
func TestTheReferencesAnArtifactMakesAreTuples(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	created := time.Now().UTC().Add(-72 * time.Hour).Truncate(time.Microsecond)
	spec, specRef := seedMentionsDocument(t, database, created)
	long := "ACME-1/" + strings.Repeat("l", graphstore.MaxAddressBytes)

	pages := mentionsServer + "/issues/"
	longPage := pages + "ACME-1/artifacts/" + strings.Repeat("l", graphstore.MaxAddressBytes)
	first, firstEvent := writeMentionsVersion(t, database, spec, 1,
		"Cites "+pages+"ACME-2 and "+pages+"ACME-3, and "+longPage+" .\n")
	stated := mentionsOf(t, database, specRef)
	if len(stated) != 2 {
		t.Fatalf("version 1 states %d mentions %+v, want ACME-2's and ACME-3's and none of the document past the bound", len(stated), stated)
	}
	for index, target := range []string{"ACME-2", "ACME-3"} {
		mention := stated[index]
		if mention.toKind != "issue" || mention.toID != target || !mention.asserted || !mention.at.Equal(first) ||
			mention.legacy == nil || *mention.legacy != firstEvent || mention.cause != firstEvent || mention.who != mentionsAuthor {
			t.Errorf("version 1 states %+v, want %s asserted at the version's time %s, caused by and carrying its event %d, signed by its author",
				mention, target, first, firstEvent)
		}
		requireMentionsEdge(t, "after version 1", database, spec, "issue", target, first, &firstEvent)
	}
	requireMentionsEdge(t, "the document past the bound after version 1", database, spec, "artifact", long, first, &firstEvent)

	second, secondEvent := writeMentionsVersion(t, database, spec, 2,
		"Keeps "+pages+"ACME-3, cites "+pages+"ACME-4 and still "+longPage+" .\n")
	stated = mentionsOf(t, database, specRef)
	if len(stated) != 4 {
		t.Fatalf("after version 2 the log holds %d mentions %+v, want version 1's two, then ACME-2's retraction and ACME-4's assertion", len(stated), stated)
	}
	if retracted := stated[2]; retracted.toID != "ACME-2" || retracted.asserted || !retracted.at.Equal(second) || retracted.legacy != nil || retracted.cause != secondEvent {
		t.Errorf("version 2 states %+v first, want ACME-2 retracted at the version's time %s, caused by its event %d and carrying no source_seq",
			retracted, second, secondEvent)
	}
	if asserted := stated[3]; asserted.toID != "ACME-4" || !asserted.asserted || !asserted.at.Equal(second) ||
		asserted.legacy == nil || *asserted.legacy != secondEvent || asserted.cause != secondEvent {
		t.Errorf("version 2 states %+v second, want ACME-4 asserted at the version's time %s, caused by and carrying its event %d",
			asserted, second, secondEvent)
	}
	if rows, _, _ := mentionsEdge(t, database, spec, "issue", "ACME-2"); rows != 0 {
		t.Errorf("graph_edges answers the mention version 2 dropped %d times, want none", rows)
	}
	requireMentionsEdge(t, "the mention version 2 kept", database, spec, "issue", "ACME-3", first, &firstEvent)
	requireMentionsEdge(t, "the mention version 2 added", database, spec, "issue", "ACME-4", second, &secondEvent)
	requireMentionsEdge(t, "the document past the bound after version 2", database, spec, "artifact", long, first, &firstEvent)

	// A comment's reference is a refs row and no tuple.
	var comment string
	var tuples, commented int
	if err := database.Pool.QueryRow(ctx, `select count(*) from graph_tuples`).Scan(&tuples); err != nil {
		t.Fatalf("count the store's tuples: %v", err)
	}
	const remark = "A remark citing ACME-5."
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			insert into comments (issue_key, author, body) values ('ACME-1', '{"kind":"user","id":"alice"}', $1)
			returning id::text
		`, remark).Scan(&comment); err != nil {
			return err
		}
		reader := refs.NewReader(ctx, tx, graphstore.KindComment, comment, mentionsServer)
		replacement, err := refs.PlanReplace(ctx, tx, graphstore.KindComment, comment, text.Refs(reader.Mentions(remark)), nil)
		if err != nil {
			return err
		}
		return store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
			replacement.Queue(b)
			return nil
		})
	}); err != nil {
		t.Fatalf("write the comment's reference: %v", err)
	}
	if err := database.Pool.QueryRow(ctx, `
		select count(*) from refs where from_kind = 'comment' and from_id = $1 and to_kind = 'issue' and to_id = 'ACME-5'
	`, comment).Scan(&commented); err != nil || commented != 1 {
		t.Fatalf("the comment holds %d refs rows to ACME-5 (%v), want one", commented, err)
	}
	var after int
	if err := database.Pool.QueryRow(ctx, `select count(*) from graph_tuples`).Scan(&after); err != nil {
		t.Fatalf("count the store's tuples: %v", err)
	}
	if after != tuples {
		t.Errorf("the comment's reference appended %d tuples, want none", after-tuples)
	}

	// A document the release before wrote, naming no node, and its reference, written by SQL with
	// the source_seq that release stamps it with.
	var notes, notesRef string
	cited := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Microsecond)
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			insert into artifacts (issue_key, project_key, slug, name, kind, created_by, created_at)
			values ('ACME-1', 'ACME', 'notes', 'notes.md', 'doc', '{"kind":"user","id":"alice"}', $1)
			returning id::text, ref_key
		`, created).Scan(&notes, &notesRef); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			insert into artifact_versions (artifact_id, number, markdown, authors)
			values ($1::uuid, 1, 'Cites ACME-5.', '[{"kind":"user","id":"alice"}]')
		`, notes); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			insert into refs (from_kind, from_id, to_kind, to_id, created_at, source_seq)
			values ('artifact', $1, 'issue', 'ACME-5', $2, $3)
		`, notes, cited, firstEvent)
		return err
	}); err != nil {
		t.Fatalf("write the document the release before wrote: %v", err)
	}
	requireMentionsEdge(t, "the release before's reference before the backfill", database, notes, "issue", "ACME-5", cited, &firstEvent)
	if kind := referencesKind(t, database); kind.RowDifferences != 0 || kind.NodeDifferences != 0 || kind.Unmarked != 1 || kind.PastBound != 1 || kind.Rows != 2 {
		t.Errorf("before the backfill the census reads %d row(s), %d unmarked, %d past the bound, and %d and %d differences (%+v %+v), want version 2's two, the release before's reference unmarked, the long one past the bound and none",
			kind.Rows, kind.Unmarked, kind.PastBound, kind.RowDifferences, kind.NodeDifferences, kind.ReportedRows, kind.ReportedNodes)
	}
	if done := backfillReferences(t, database); done.Rows != 1 || done.Tuples != 1 || !done.Complete {
		t.Errorf("the backfill reports %#v for the references, want the release before's one row and its tuple", done)
	}
	backfilled := mentionsOf(t, database, notesRef)
	if len(backfilled) != 1 || backfilled[0].toID != "ACME-5" || !backfilled[0].asserted || !backfilled[0].at.Equal(cited) ||
		backfilled[0].legacy == nil || *backfilled[0].legacy != firstEvent || backfilled[0].cause != graphstore.NoEvent {
		t.Errorf("the backfilled document states %+v, want one mention of ACME-5 at its row's created_at %s carrying its source_seq %d, at the sentinel",
			backfilled, cited, firstEvent)
	}
	requireMentionsEdge(t, "the release before's reference after the backfill", database, notes, "issue", "ACME-5", cited, &firstEvent)

	// A row of the first document whose tuple is gone: a row no tuple states, answered by no arm
	// until the backfill states it again.
	if tag, err := database.Pool.Exec(ctx, `
		delete from graph_tuples where from_kind = 'artifact' and from_id = $1 and kind = 'mentions' and to_id = 'ACME-3'
	`, specRef); err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("delete ACME-3's mention tuple: %d rows, %v", tag.RowsAffected(), err)
	}
	if rows, _, _ := mentionsEdge(t, database, spec, "issue", "ACME-3"); rows != 0 {
		t.Errorf("graph_edges answers the row whose tuple is gone %d times, want none: the arm over refs answers no row of a document whose row names its node", rows)
	}
	want := "artifact " + spec + " mentions -> issue ACME-3"
	if kind := referencesKind(t, database); kind.RowDifferences != 1 || len(kind.ReportedRows) != 1 ||
		kind.ReportedRows[0].Cause != "a row no tuple states" || kind.ReportedRows[0].At != want || kind.NodeDifferences != 0 {
		t.Errorf("the census reads %d row and %d tuple difference(s) (%+v %+v), want the one row no tuple states at %s",
			kind.RowDifferences, kind.NodeDifferences, kind.ReportedRows, kind.ReportedNodes, want)
	}
	if done := backfillReferences(t, database); done.Rows != 1 || done.Tuples != 1 || !done.Complete {
		t.Errorf("the backfill over the row whose tuple is gone reports %#v, want it stated again", done)
	}
	requireMentionsEdge(t, "the row whose tuple the backfill wrote again", database, spec, "issue", "ACME-3", first, &firstEvent)
	if kind := referencesKind(t, database); kind.RowDifferences != 0 || kind.NodeDifferences != 0 || kind.Rows != 3 {
		t.Errorf("after the second backfill the census reads %d row(s) and %d and %d differences (%+v %+v), want three rows and none",
			kind.Rows, kind.RowDifferences, kind.NodeDifferences, kind.ReportedRows, kind.ReportedNodes)
	}
	if done := backfillReferences(t, database); done.Rows != 0 || done.Tuples != 0 || !done.Complete {
		t.Errorf("a third backfill reports %#v, want nothing written", done)
	}

	// A row of the first document the release before deleted, retracting nothing: its tuple stands
	// with no row, which no arm answers and the census names, and the backfill retracts it at the
	// run's time.
	if tag, err := database.Pool.Exec(ctx, `
		delete from refs where from_kind = 'artifact' and from_id = $1 and to_kind = 'issue' and to_id = 'ACME-4'
	`, spec); err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("delete ACME-4's refs row as the release before: %d rows, %v", tag.RowsAffected(), err)
	}
	if rows, _, _ := mentionsEdge(t, database, spec, "issue", "ACME-4"); rows != 0 {
		t.Errorf("graph_edges answers the mention whose row is gone %d times, want none", rows)
	}
	orphan := graphstore.KindArtifact + ":" + specRef + " mentions -> issue:ACME-4"
	if kind := referencesKind(t, database); kind.NodeDifferences != 1 || len(kind.ReportedNodes) != 1 ||
		kind.ReportedNodes[0].Cause != "a tuple no row projects" || kind.ReportedNodes[0].At != orphan || kind.RowDifferences != 0 {
		t.Errorf("the census reads %d row and %d tuple difference(s) (%+v %+v), want the one tuple no row projects at %s",
			kind.RowDifferences, kind.NodeDifferences, kind.ReportedRows, kind.ReportedNodes, orphan)
	}
	ran := time.Now().UTC().Truncate(time.Microsecond)
	if done := backfillReferences(t, database); done.Rows != 1 || done.Tuples != 1 || !done.Complete {
		t.Errorf("the backfill over the mention whose row is gone reports %#v, want it retracted", done)
	}
	stated = mentionsOf(t, database, specRef)
	if retracted := stated[len(stated)-1]; retracted.toID != "ACME-4" || retracted.asserted || retracted.at.Before(ran) ||
		retracted.at.After(time.Now().UTC()) || retracted.cause != graphstore.NoEvent {
		t.Errorf("the backfill states %+v last, want ACME-4 retracted at the run's time, after %s, at the sentinel", retracted, ran)
	}
	if rows, _, _ := mentionsEdge(t, database, spec, "issue", "ACME-4"); rows != 0 {
		t.Errorf("graph_edges answers the retracted mention %d times, want none", rows)
	}
	if kind := referencesKind(t, database); kind.RowDifferences != 0 || kind.NodeDifferences != 0 {
		t.Errorf("after the retraction the census reads %d and %d differences (%+v %+v), want none",
			kind.RowDifferences, kind.NodeDifferences, kind.ReportedRows, kind.ReportedNodes)
	}

	// A row rebuild-refs inserts carries no source_seq, nor does the tuple it states beside it; a
	// later version that keeps the citation stamps the row with its event, and graph_edges answers
	// the edge with the row's source_seq, as the arm over refs answers it, not the tuple's.
	plan, planRef := markedDocument(t, database, "plan", created)
	if _, err := database.Pool.Exec(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, authors)
		values ($1::uuid, 1, 'Cites ACME-5.', '[{"kind":"user","id":"alice"}]')
	`, plan); err != nil {
		t.Fatalf("write the plan's first version: %v", err)
	}
	if _, err := refs.RebuildAll(ctx, database.Pool, mentionsServer); err != nil {
		t.Fatalf("rebuild the references: %v", err)
	}
	var rebuilt time.Time
	var rebuiltSeq *int64
	if err := database.Pool.QueryRow(ctx, `
		select created_at, source_seq from refs where from_kind = 'artifact' and from_id = $1 and to_kind = 'issue' and to_id = 'ACME-5'
	`, plan).Scan(&rebuilt, &rebuiltSeq); err != nil || rebuiltSeq != nil {
		t.Fatalf("the rebuild's row of the plan carries source_seq %s (%v), want none", seqText(rebuiltSeq), err)
	}
	if stated := mentionsOf(t, database, planRef); len(stated) != 1 || !stated[0].asserted || stated[0].legacy != nil || !stated[0].at.Equal(rebuilt) {
		t.Fatalf("the rebuild states the plan's mentions %+v, want ACME-5 asserted at its row's created_at %s with no source_seq", stated, rebuilt)
	}
	requireMentionsEdge(t, "the rebuild's row", database, plan, "issue", "ACME-5", rebuilt, nil)
	_, stamp := writeMentionsVersion(t, database, plan, 2, "Still cites "+pages+"ACME-5 .\n")
	requireMentionsEdge(t, "the rebuild's row once a version stamps it", database, plan, "issue", "ACME-5", rebuilt, &stamp)
	if kind := referencesKind(t, database); kind.RowDifferences != 0 || kind.NodeDifferences != 0 {
		t.Errorf("after the rebuild and the stamp the census reads %d and %d differences (%+v %+v), want none",
			kind.RowDifferences, kind.NodeDifferences, kind.ReportedRows, kind.ReportedNodes)
	}
}

// TestARebuildOfADocumentsReferencesTakesTheStoreLockOrder holds rebuild-refs' reconcile of a
// document (refs.RebuildAll, one transaction per source) to the store's lock order, as
// graphcensus' batches are held to it: the owner lock the document stands under, its issue's
// (store.LockOwner, the advisory lock an edit of its text takes), first, then its refs rows, then
// the mentions tuples of the rows it moved. Those are appended at the sentinel, since no event
// records a rebuild, which no repeat key holds, so no hold stands on them. The document's row
// names its node; its latest
// version cites ACME-2, and its refs hold a row to ACME-3, with its tuple, that the text no longer
// cites, which the test holds so the reconcile's delete waits behind it. After the run the
// document's node retracts its mention of ACME-3 and states one of ACME-2, dated at the new row's
// own created_at and carrying its source_seq, none, and graph_edges answers ACME-2 from that tuple
// and ACME-3 nowhere.
func TestARebuildOfADocumentsReferencesTakesTheStoreLockOrder(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	created := time.Now().UTC().Add(-72 * time.Hour).Truncate(time.Microsecond)
	spec, specRef := seedMentionsDocument(t, database, created)
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			insert into artifact_versions (artifact_id, number, markdown, authors)
			values ($1::uuid, 1, 'Cites ACME-2.', '[{"kind":"user","id":"alice"}]')
		`, spec); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			insert into refs (from_kind, from_id, to_kind, to_id, created_at) values ('artifact', $1, 'issue', 'ACME-3', $2)
		`, spec, created); err != nil {
			return err
		}
		return graphstore.Write(ctx, tx, nil, []graphstore.Tuple{graphstore.ArtifactMentions(specRef, "issue", "ACME-3", true, mentionsAuthor, created)})
	}); err != nil {
		t.Fatalf("seed the document's version and the reference its text no longer cites: %v", err)
	}

	owner := lockstep.HoldOwner(t, database, &lockstep.Hold{
		Name: "issue ACME-1's owner lock", Step: lockstep.StepOwner, Taken: true,
		Statement: lockstep.OwnerLockStatement,
		Unlocked:  []string{"refs", "graph_tuples"},
	}, store.IssueOwner("ACME-1"))
	row := lockstep.HoldRow(t, database, &lockstep.Hold{
		Name: "the document's refs row to ACME-3", Step: lockstep.StepProjection, Taken: true,
		Statement: "delete from refs",
		Unlocked:  []string{"graph_tuples"},
	}, `select 1 from refs where from_kind = 'artifact' and from_id = $1 and to_kind = 'issue' and to_id = 'ACME-3' for update`, spec)
	rebuilder := storetest.SingleConnection(t, database)
	var pid uint32
	if err := rebuilder.Pool.QueryRow(ctx, `select pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatalf("read the rebuild's backend: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := refs.RebuildAll(ctx, rebuilder.Pool, mentionsServer)
		done <- err
	}()
	lockstep.Follow(t, database, "replaceInTx", pid, done, []*lockstep.Hold{owner, row})

	var rebuilt time.Time
	var seq *int64
	if err := database.Pool.QueryRow(ctx, `
		select created_at, source_seq from refs where from_kind = 'artifact' and from_id = $1 and to_kind = 'issue' and to_id = 'ACME-2'
	`, spec).Scan(&rebuilt, &seq); err != nil {
		t.Fatalf("read the row the rebuild inserted: %v", err)
	}
	stated := mentionsOf(t, database, specRef)
	if len(stated) != 3 {
		t.Fatalf("after the rebuild the document states the mentions %+v, want the seeded ACME-3, its retraction and ACME-2's", stated)
	}
	if retracted := stated[1]; retracted.toID != "ACME-3" || retracted.asserted || !retracted.at.Equal(rebuilt) || retracted.cause != graphstore.NoEvent {
		t.Errorf("the rebuild states %+v first, want ACME-3 retracted at the rebuild's time %s, at the sentinel", retracted, rebuilt)
	}
	if asserted := stated[2]; asserted.toID != "ACME-2" || !asserted.asserted || !asserted.at.Equal(rebuilt) || asserted.legacy != nil ||
		asserted.cause != graphstore.NoEvent || asserted.who != mentionsAuthor {
		t.Errorf("the rebuild states %+v second, want ACME-2 asserted at its row's created_at %s, carrying no source_seq, at the sentinel, signed by the version's author",
			asserted, rebuilt)
	}
	requireMentionsEdge(t, "after the rebuild", database, spec, "issue", "ACME-2", rebuilt, seq)
	if rows, _, _ := mentionsEdge(t, database, spec, "issue", "ACME-3"); rows != 0 {
		t.Errorf("graph_edges answers the mention the rebuild dropped %d times, want none", rows)
	}
}

package docs

import (
	"context"
	"fmt"
	"io"
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/processes"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/startup"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// graphEdgesArm is one arm of graph_edges as this test reads it back: the relation the arm reads
// first, and what is to become of it. The relation is enough to name an arm here because the
// list is held in the view's own order, so two arms over one table are told apart by where they
// stand. projects is, for the four arms over a kind this change moved, the kind whose rows the
// store now holds beside that arm's table, and empty for every other arm; renders is, for the two
// moved arms over comments, the edge kind each renders, which is what tells their rows apart.
// text is the arm's own SQL as the catalogue prints it, which only an arm read back carries.
type graphEdgesArm struct {
	relation string
	fate     string
	projects string
	renders  string
	text     string
}

// movedArmsDeletedBy is the child that deletes the four arms over the kinds this change moved,
// once the store answers those kinds everywhere.
const movedArmsDeletedBy = "the child that moves issues and their documents"

// graphEdgesArms is every arm of graph_edges, in the order the view states them. An arm the view
// gains or loses fails this test, whichever source column it reads: the store is to answer each
// kind in the end, and an arm added over a column now is a kind that would have to be moved
// twice. The fate of each is printed with the list, so a reader sees which arms are left and who
// takes each.
var graphEdgesArms = []graphEdgesArm{
	{relation: "refs", fate: "a mention of any node from an item; no child has moved it"},
	{relation: "issues", fate: "child_of; the change that makes an issue's relations typed edges takes it"},
	{relation: "issue_links", fate: "blocked_by; the change that makes an issue's relations typed edges takes it"},
	{relation: "artifacts", fate: "attached_to; no child has moved it"},
	{relation: "asks", fate: "an ask's anchored_to; the child that moves the inbox, the messages and the read state writes an ask's relations"},
	{relation: "comments", projects: "comment", renders: "anchored_to", fate: "a comment's anchored_to; the store answers it for a row that carries its node, and " + movedArmsDeletedBy + " deletes this arm"},
	{relation: "asks", fate: "an ask's owned_by; the child that moves the inbox, the messages and the read state writes an ask's relations"},
	{relation: "comments", projects: "comment", renders: "owned_by", fate: "a comment's owned_by; the store answers it for a row that carries its node, and " + movedArmsDeletedBy + " deletes this arm"},
	{relation: "comments", fate: "a reply's replies_to its thread; the child that moves the inbox, the messages and the read state writes it"},
	{relation: "comments", fate: "a reply's answers; the child that moves the inbox, the messages and the read state writes it"},
	{relation: "comments", fate: "a reply's replies_to an ask; the child that moves the inbox, the messages and the read state writes it"},
	{relation: "messages", fate: "a message's replies_to; the child that moves the inbox, the messages and the read state writes it"},
	{relation: "ask_followers", fate: "followed_by; the child that moves the inbox, the messages and the read state writes an ask's relations"},
	{relation: "components", fate: "a component's part_of; no child has moved it"},
	{relation: "component_depends", fate: "depends_on; no child has moved it"},
	{relation: "issue_component_members", fate: "affects; no child has moved it"},
	{relation: "block_refs", projects: "block_ref", fate: "a block's links; the store answers them for a row that carries its node, and " + movedArmsDeletedBy + " deletes this arm"},
	{relation: "document_blocks", projects: "block", fate: "a block's part_of its document; the store answers it for a row that carries its node, and " + movedArmsDeletedBy + " deletes this arm"},
	{relation: "comment_blocks", fate: "a comment block's part_of; the child that moves the inbox, the messages and the read state writes an item's blocks"},
	{relation: "message_blocks", fate: "a message block's part_of; the child that moves the inbox, the messages and the read state writes an item's blocks"},
	{relation: "ask_blocks", fate: "an ask part's part_of; the child that moves the inbox, the messages and the read state writes an item's blocks"},
	{relation: "comment_block_refs", fate: "a comment block's links; the child that moves the inbox, the messages and the read state writes an item's blocks"},
	{relation: "message_block_refs", fate: "a message block's links; the child that moves the inbox, the messages and the read state writes an item's blocks"},
	{relation: "ask_block_refs", fate: "an ask part's links; the child that moves the inbox, the messages and the read state writes an item's blocks"},
	{relation: "comments", fate: "a reply's replies_to a comment's block; the child that moves the inbox, the messages and the read state writes it"},
	{relation: "comments", fate: "a reply's replies_to an ask's part; the child that moves the inbox, the messages and the read state writes it"},
	{relation: "messages", fate: "a message's replies_to a message's block; the child that moves the inbox, the messages and the read state writes it"},
	{relation: "graph_tuples", fate: "the store itself, which answers an edge of a moved kind while the row that names it carries its node; it stays"},
}

// graphEdgeRow is one row of graph_edges, every column of it, as both readings compare them: the
// time an edge carries and the write that caused it are part of the row, so an arm that answered
// the right pair at the wrong moment, or with no sequence, fails here.
type graphEdgeRow struct {
	fromKind, fromID, kind, toKind, toID string
	createdAt                            time.Time
	sourceSeq                            *int64
}

func (r graphEdgeRow) String() string {
	seq := "no source_seq"
	if r.sourceSeq != nil {
		seq = fmt.Sprintf("source_seq %d", *r.sourceSeq)
	}
	return fmt.Sprintf("%s at %s, %s", r.edge(), r.createdAt.UTC().Format(time.RFC3339Nano), seq)
}

// edge is the row's pair and kind without its time and cause: what a row deleted or written
// again is looked for by, since a row written again is dated by its own write.
func (r graphEdgeRow) edge() graphEdge {
	return graphEdge{r.fromKind, r.fromID, r.kind, r.toKind, r.toID}
}

// graphEdge is one edge of graph_edges by its two ends and its kind.
type graphEdge struct{ fromKind, fromID, kind, toKind, toID string }

func (e graphEdge) String() string {
	return fmt.Sprintf("%s %s -%s-> %s %s", e.fromKind, e.fromID, e.kind, e.toKind, e.toID)
}

// previousGraphEdges is the view of the release before this one, created beside graph_edges over
// the same tables from that release's own statement (previousGraphEdgesMigration), so each reading
// of this test holds graph_edges to what that release reads over the same rows.
const previousGraphEdges = "previous_graph_edges"

// previousGraphEdgesMigration is the migration that last stated graph_edges before this child.
const previousGraphEdgesMigration = "../store/migrations/20261105014808_comments_answers.up.sql"

// The view answers each row exactly once, from the arm the row itself decides, and reads what the
// view of the release before this one reads over the same rows, before the backfill, after it,
// and after a rollback.
//
// The arm list is read back out of the catalogue, split on its top-level UNION ALL and compared
// with the one declared above, so no arm over a source column can be added anywhere in this branch
// without this test naming it, and the list is printed with the child that deletes each arm that
// remains. Each of the four arms over a moved kind is held to its `node_kind is null` test and to
// the child that deletes it, and the tuple arm to its join to each of the three projection tables;
// no arm may read a table that decides the switch by kind.
//
// Every reading reads the whole view, the previous release's view over the same rows, and each of
// the four arms and the tuple arm on their own. The whole view must equal the previous release's,
// row for row with each row's time and causing write. Each row of a moved kind the previous
// release reads goes to exactly one arm, decided by its own projection row: the four arms answer
// exactly the rows whose row carries no node, and the tuple arm exactly those whose row carries it
// and nothing else, so it answers no edge whose row is gone or names no node. Every reading counts
// as well: for every node kind its arm table declares, refs.BacklinkCounts must answer what the
// previous release's count statement answers over that release's view, at every id of the kind an
// edge of that view, a tuple or a block reference points at and at one id nothing points at.
//
// The readings, in order. Before the backfill, one document's rows and the comments name no node
// beside documents the index wrote, and each of the four arms answers rows of its own. After the
// backfill every row is stamped, the four arms answer nothing and the tuple arm answers every row,
// and the whole view is the reading before it. A comment and a block reference then written as
// the previous release writes them, naming no node, come from their old arms. Then the rollback:
// a stamped block row, a stamped block reference and a stamped comment deleted with no
// retraction, as a build of the previous release deletes them, answer from no arm; a second
// stamped block row and block reference deleted and written again naming no node answer once
// each, from the old arm. The store census then reports a difference for each of the three
// deleted rows, which is what the release that deletes an old arm reads first.
//
// Last, the announcement: a build of the previous release announced itself naming only its
// commit, and once every live process serves this build its announcement lists the kinds the
// tuple arm renders (refs.TupleKinds), while the earlier one lists none.
func TestGraphEdgesLosesEveryArmOverAMovedKind(t *testing.T) {
	database, service := blockIndexService(t)
	ctx := context.Background()

	read := readGraphEdgesArms(t, database)
	t.Logf("graph_edges has %d arms:\n  %s", len(read), strings.Join(armReport(read), "\n  "))
	if want := armRelations(graphEdgesArms); !slices.Equal(armRelations(read), want) {
		t.Fatalf("graph_edges reads %v; this test declares %v.\nAn arm over a source column is a kind that would have to be moved twice: "+
			"write the kind's tuples and let the store's own arm answer it, or declare the new arm here with the child that takes it.",
			armRelations(read), want)
	}
	moved := movedArms(t, read)
	tuple := tupleArm(t, read)
	createPreviousGraphEdges(t, database)

	fixture := seedGraphEdgeFixture(t, database, service)
	// The rows of one document are put back in the shape the release before this one left
	// them in, so the four old arms answer rows of their own while the store answers the rest.
	unwriteTheStoreOfADocument(t, database, graphEdgeLegacyRefKey)
	requireTheIndexStampedTheOtherDocuments(t, database)
	before := readEachArm(t, database, "before the backfill", moved, tuple)
	// The tuple arm answers the stamped rows of the other two documents, so the reading's
	// comparison of that arm is over rows and not over two empty sets.
	if len(before.tuple) == 0 {
		t.Errorf("the tuple arm answers no row before the backfill, so its comparison with the stamped rows proves nothing")
	}
	projected := before.view
	projectedCounts := before.counts
	// Each of the four answers rows of its own before the backfill, so finding it empty after
	// it is the per-row test at work and not an arm with nothing to say.
	for index, arm := range moved {
		t.Logf("arm %d (%s) answers %d rows before the backfill", arm.place, arm.relation, len(before.arms[index]))
		if len(before.arms[index]) == 0 {
			t.Errorf("arm %d (%s) answers no row before the backfill, so finding it empty after it would prove nothing",
				arm.place, arm.relation)
		}
	}

	report, err := graphcensus.Backfill(ctx, database, io.Discard)
	if err != nil {
		t.Fatalf("backfill the store: %v", err)
	}
	for _, kind := range report.Kinds {
		t.Logf("backfill kind=%s rows=%d nodes=%d tuples=%d remaining=%d complete=%t",
			kind.Kind, kind.Rows, kind.Nodes, kind.Tuples, kind.Remaining, kind.Complete)
		if !kind.Complete {
			t.Fatalf("the backfill left %s with rows that name no node: %#v", kind.Kind, kind)
		}
	}
	after := readEachArm(t, database, "after the backfill", moved, tuple)
	stored := after.view
	for index, arm := range moved {
		if rows := after.arms[index]; len(rows) != 0 {
			t.Errorf("arm %d (%s, %s) answers %d rows once every row of its table names a node, want none: the store's arm answers them now",
				arm.place, arm.relation, arm.fate, len(rows))
		}
	}

	if len(projected) == 0 {
		t.Fatal("the view answers no row before the backfill, so this test would prove nothing")
	}
	t.Logf("the view answers %d rows before the backfill and %d after it:\n  %s",
		len(projected), len(stored), strings.Join(edgeShapes(projected), "\n  "))
	requireEveryMovedKind(t, projected)
	requireSameEdges(t, "the backfill", "the view answers it before the backfill", projected, "the view answers it after the backfill", stored)
	// Each reading held its counts to the previous release's count over the rows that release
	// reads, and those rows are the same on both sides of the backfill, so the counts after it are
	// the counts before it. A count map that holds nothing compares equal to another that holds
	// nothing, so the kinds the four arms point at must each be counted at all.
	for _, kind := range []string{"artifact", "issue", refs.BlockNodeKind} {
		if len(projectedCounts[kind]) == 0 {
			t.Errorf("the %s backlink counts before the backfill are empty, so their comparison proves nothing", kind)
		}
	}
	requireTheTupleArmRendersTheList(t, after.tuple)

	// Pods of the previous release write beside this one, during a roll or after a rollback.
	written := writeAsThePreviousRelease(t, database, fixture)
	beside := readEachArm(t, database, "beside rows the previous release wrote", moved, tuple)
	for _, edge := range written {
		requireAnsweredOnceByItsOldArm(t, "a row the previous release wrote", beside, moved, edge)
	}

	// The rollback.
	deleted, rewritten := rollBackAsThePreviousRelease(t, database, fixture)
	rolledBack := readEachArm(t, database, "after the rollback", moved, tuple)
	for _, edge := range deleted {
		if view, arms, tupled := rolledBack.answering(edge); view != 0 || tupled != 0 || slices.ContainsFunc(arms, func(n int) bool { return n != 0 }) {
			t.Errorf("after the rollback the view answers the deleted row %s %d times (the four arms %v, the tuple arm %d), want no arm: a build of the previous release deleted it",
				edge, view, arms, tupled)
		}
	}
	for _, edge := range rewritten {
		requireAnsweredOnceByItsOldArm(t, "a row the previous release deleted and wrote again", rolledBack, moved, edge)
	}
	requireTheCensusReportsEachDeletedRow(t, takeStoreCensus(t, database), deleted)

	requireTheAnnouncementListsTheArmsKinds(t, database)
}

// movedArm is one arm of the view as read back, with its place in the view, counted from one: one
// of the four arms over a kind this change moved, or the tuple arm.
type movedArm struct {
	place int
	graphEdgesArm
}

// nodelessRowTest matches the test that keeps an arm to the rows of its own table that name no
// node, as the catalogue prints it, with the alias the arm gives that table.
var nodelessRowTest = regexp.MustCompile(`(?:([a-z_]+)\.)?node_kind IS NULL`)

// switchedByKind matches a table that would decide the switch for a whole kind rather than for
// one row. No arm may read one: a row written by a pod of the release before this one, a row
// written after a rollback and a block past the address bound each name no node for as long as
// they stand, and a test by kind would hide every one of them from the moment the backfill of
// that kind finished.
var switchedByKind = regexp.MustCompile(`graph_store_cutover`)

// movedArms holds each of the four arms over a moved kind to the per-row test over its own
// table and to the child that deletes it, and answers them with their text for reading on their
// own.
func movedArms(t *testing.T, read []graphEdgesArm) []movedArm {
	t.Helper()
	for index, arm := range read {
		if switchedByKind.MatchString(arm.text) {
			t.Errorf("arm %d (%s) reads a table that decides the switch by kind, not by row:\n%s",
				index+1, arm.relation, arm.text)
		}
	}
	var moved []movedArm
	for index, declared := range graphEdgesArms {
		if declared.projects == "" {
			continue
		}
		arm := read[index]
		arm.fate, arm.projects, arm.renders = declared.fate, declared.projects, declared.renders
		if !nodelessRowTest.MatchString(arm.text) {
			t.Errorf("arm %d (%s) over the moved kind %s carries no `node_kind is null` test, so it answers rows the store answers too:\n%s",
				index+1, arm.relation, declared.projects, arm.text)
		}
		if !strings.Contains(declared.fate, movedArmsDeletedBy) {
			t.Errorf("arm %d (%s) over the moved kind %s is declared with the fate %q, which does not name %s, the child that deletes it",
				index+1, arm.relation, declared.projects, declared.fate, movedArmsDeletedBy)
		}
		moved = append(moved, movedArm{place: index + 1, graphEdgesArm: arm})
	}
	if len(moved) != 4 {
		t.Fatalf("this test declares %d arms over a moved kind, want the four this change moved", len(moved))
	}
	return moved
}

// tupleJoins are the joins the tuple arm makes to the projection row of each kind it renders, as
// the catalogue prints them with their whitespace folded: the table, then the row naming the
// tuple's own node. An arm with no join to one of them answers that table's kinds from the tuples
// alone, which is what leaves a row a previous release deleted answered for good.
var tupleJoins = map[string]*regexp.Regexp{
	"document_blocks": regexp.MustCompile(`FROM document_blocks (\w+) WHERE (\w+)\.node_kind = edge\.from_kind AND (\w+)\.node_address = edge\.from_id`),
	"block_refs": regexp.MustCompile(`FROM block_refs (\w+) WHERE (\w+)\.node_kind = edge\.from_kind AND (\w+)\.node_address = edge\.from_id` +
		` AND (\w+)\.kind = edge\.stated AND (\w+)\.to_kind = edge\.to_kind AND (\w+)\.to_id = edge\.to_id`),
	"comments": regexp.MustCompile(`FROM comments (\w+) WHERE (\w+)\.node_kind = edge\.from_kind AND (\w+)\.node_address = edge\.from_id`),
}

// tupleArm answers the arm over graph_tuples and holds it to its join to each projection table.
func tupleArm(t *testing.T, read []graphEdgesArm) movedArm {
	t.Helper()
	for index, arm := range read {
		if arm.relation != "graph_tuples" {
			continue
		}
		folded := strings.Join(strings.Fields(arm.text), " ")
		for _, table := range slices.Sorted(maps.Keys(tupleJoins)) {
			if !tupleJoins[table].MatchString(folded) {
				t.Errorf("arm %d (graph_tuples) answers the kinds %s projects with no join to that table's row carrying the tuple's node:\n%s",
					index+1, table, arm.text)
			}
		}
		return movedArm{place: index + 1, graphEdgesArm: arm}
	}
	t.Fatal("graph_edges has no arm over graph_tuples")
	return movedArm{}
}

// createPreviousGraphEdges creates previousGraphEdges from the statement of the migration that
// last stated graph_edges before this child, over the same tables, so that what the release
// before this one reads can be read beside what this one reads.
func createPreviousGraphEdges(t *testing.T, database *store.Store) {
	t.Helper()
	migration, err := os.ReadFile(previousGraphEdgesMigration)
	if err != nil {
		t.Fatalf("read the previous release's view: %v", err)
	}
	const statement = "create or replace view graph_edges "
	_, view, found := strings.Cut(string(migration), statement)
	if !found {
		t.Fatalf("%s states no %q", previousGraphEdgesMigration, statement)
	}
	if _, err := database.Pool.Exec(context.Background(), "create view "+previousGraphEdges+" "+view); err != nil {
		t.Fatalf("create the previous release's view: %v", err)
	}
}

// graphEdgesReading is one reading of the view: the whole view, the previous release's view over
// the same rows, each of the four arms over a moved kind on its own in the order movedArms
// answers them, the tuple arm on its own, and refs.BacklinkCounts for every node kind its arm
// table declares, kind by kind.
type graphEdgesReading struct {
	view, previous []graphEdgeRow
	arms           [][]graphEdgeRow
	tuple          []graphEdgeRow
	counts         map[string]map[string]int
}

// answering is how often the reading answers edge: from the whole view, from each of the four
// arms and from the tuple arm.
func (r graphEdgesReading) answering(edge graphEdge) (view int, arms []int, tuple int) {
	count := func(rows []graphEdgeRow) int {
		n := 0
		for _, row := range rows {
			if row.edge() == edge {
				n++
			}
		}
		return n
	}
	arms = make([]int, len(r.arms))
	for index, rows := range r.arms {
		arms[index] = count(rows)
	}
	return count(r.view), arms, count(r.tuple)
}

// readEachArm takes one reading and fails unless every row was answered by the arm its own
// projection row decides: the whole view equals the previous release's over the same rows, each of
// the four arms answers exactly the rows of its kind whose row carries no node, and the tuple arm
// exactly the rows of the moved kinds whose row carries it. It fails as well unless this build's
// backlink counts are the previous release's over the same rows
// (requireBacklinkCountsAsThePreviousRelease).
func readEachArm(t *testing.T, database *store.Store, stage string, moved []movedArm, tuple movedArm) graphEdgesReading {
	t.Helper()
	reading := graphEdgesReading{
		view:     graphEdgeRows(t, database, "graph_edges"),
		previous: graphEdgeRows(t, database, previousGraphEdges),
		arms:     make([][]graphEdgeRow, len(moved)),
		tuple:    armAnswers(t, database, tuple),
	}
	requireSameEdges(t, stage, "graph_edges answers it", reading.view, "the previous release's view reads it", reading.previous)
	unmarked := make([][]graphEdgeRow, len(moved))
	var marked []graphEdgeRow
	for _, row := range reading.previous {
		index, ok := movedArmOf(moved, row)
		if !ok {
			continue
		}
		if carriesItsNode(t, database, moved[index], row) {
			marked = append(marked, row)
		} else {
			unmarked[index] = append(unmarked[index], row)
		}
	}
	for index, arm := range moved {
		reading.arms[index] = armAnswers(t, database, arm)
		requireSameEdges(t, fmt.Sprintf("%s, arm %d (%s: %s)", stage, arm.place, arm.relation, arm.fate),
			"the arm answers it", reading.arms[index], "the previous release reads it from a row of the arm's table that carries no node", unmarked[index])
	}
	requireSameEdges(t, fmt.Sprintf("%s, arm %d (graph_tuples)", stage, tuple.place),
		"the tuple arm answers it", reading.tuple, "the previous release reads it from a row that carries its node", marked)
	reading.counts = requireBacklinkCountsAsThePreviousRelease(t, database, stage)
	return reading
}

// movedArmOf is the arm over a moved kind whose table holds row's edge, as an index of moved, and
// false for an edge of a kind no moved arm carries. A block's edge is a document_blocks row's when
// it is the part_of to its document, and a block_refs row's otherwise; a comment's anchored_to and
// owned_by are each one comments arm's.
func movedArmOf(moved []movedArm, row graphEdgeRow) (int, bool) {
	membership := row.kind == "part_of" && row.toKind == "artifact"
	for index, arm := range moved {
		switch {
		case arm.projects == "block" && row.fromKind == refs.BlockNodeKind && membership,
			arm.projects == "block_ref" && row.fromKind == refs.BlockNodeKind && !membership,
			arm.projects == "comment" && row.fromKind == "comment" && row.kind == arm.renders:
			return index, true
		}
	}
	return 0, false
}

// carriesItsNode reads whether the projection row behind row, in the table of the moved arm that
// holds it, carries its node: the marker that decides which arm answers the row.
func carriesItsNode(t *testing.T, database *store.Store, arm movedArm, row graphEdgeRow) bool {
	t.Helper()
	var statement string
	args := []any{row.fromID}
	switch arm.projects {
	case "block":
		statement = `select node_kind is not null from document_blocks where node_id = $1`
	case "block_ref":
		statement = `select node_kind is not null from block_refs where from_node = $1 and kind = $2 and to_kind = $3 and to_id = $4`
		args = append(args, row.kind, row.toKind, row.toID)
	case "comment":
		statement = `select node_kind is not null from comments where id = $1::uuid`
	}
	var marked bool
	if err := database.Pool.QueryRow(context.Background(), statement, args...).Scan(&marked); err != nil {
		t.Fatalf("read the %s row behind %s: %v", arm.relation, row, err)
	}
	return marked
}

// requireAnsweredOnceByItsOldArm fails unless the reading answers edge once, from the arm over
// its table, and not from the tuple arm.
func requireAnsweredOnceByItsOldArm(t *testing.T, what string, reading graphEdgesReading, moved []movedArm, edge graphEdge) {
	t.Helper()
	index, ok := movedArmOf(moved, graphEdgeRow{fromKind: edge.fromKind, fromID: edge.fromID, kind: edge.kind, toKind: edge.toKind, toID: edge.toID})
	if !ok {
		t.Fatalf("%s, %s, is of no kind a moved arm carries", what, edge)
	}
	view, arms, tupled := reading.answering(edge)
	if view != 1 || arms[index] != 1 || tupled != 0 {
		t.Errorf("%s, %s, is answered %d times by the view, %d by arm %d (%s) and %d by the tuple arm; want once, from that arm alone",
			what, edge, view, arms[index], moved[index].place, moved[index].relation, tupled)
	}
}

// armAnswers is every row one arm of the view answers read on its own.
func armAnswers(t *testing.T, database *store.Store, arm movedArm) []graphEdgeRow {
	t.Helper()
	return scanGraphEdgeRows(t, database, fmt.Sprintf("arm %d (%s)", arm.place, arm.relation),
		`select * from (`+arm.text+`) arm (from_kind, from_id, kind, to_kind, to_id, created_at, source_seq)`)
}

// requireEveryMovedKind fails unless the fixture filled each of the four arms this change moves,
// so the comparison below cannot pass over a seed that holds none of them. A pointer is counted
// by its own kind, serves, which is what a block[] attribute of the shipped block schema is
// named, and the link at the retyped block is in neither reading, which is why one pointer
// stands here and two are stored.
func requireEveryMovedKind(t *testing.T, rows []graphEdgeRow) {
	t.Helper()
	held := map[string]int{}
	for _, row := range rows {
		held[row.fromKind+" -"+row.kind+"-> "+row.toKind]++
	}
	for _, shape := range []string{
		"block -part_of-> artifact",
		"block -mentions-> issue",
		"block -serves-> block",
		"comment -anchored_to-> artifact",
		"comment -owned_by-> artifact",
	} {
		if held[shape] == 0 {
			t.Fatalf("the fixture left %q out of the view, so the comparison would prove nothing about that arm:\n  %s",
				shape, strings.Join(edgeShapes(rows), "\n  "))
		}
	}
}

// requireTheTupleArmRendersTheList fails unless every kind the tuple arm answers is one of
// refs.TupleKinds, a block's part_of to its document read as the in_document it renders, and
// every kind of that list but the pointer kinds the fixture writes none of is answered: the arm
// renders the list the announcement carries and nothing else, so the store's authored_by and a
// block's part_of its parent, which the fixture holds too, are no edge of it.
func requireTheTupleArmRendersTheList(t *testing.T, rows []graphEdgeRow) {
	t.Helper()
	list := refs.TupleKinds()
	answered := map[string]int{}
	for _, row := range rows {
		kind := row.kind
		if row.fromKind == refs.BlockNodeKind && kind == "part_of" && row.toKind == "artifact" {
			kind = refs.TupleInDocument
		}
		answered[kind]++
		if !slices.Contains(list, kind) {
			t.Errorf("the tuple arm answers %s, of the kind %s, which refs.TupleKinds %v does not list", row, kind, list)
		}
	}
	pointers := pmdoc.LinkKinds()
	for _, kind := range list {
		if answered[kind] == 0 && (!slices.Contains(pointers, kind) || kind == "serves") {
			t.Errorf("the tuple arm answers no %s edge, which refs.TupleKinds lists and the fixture writes: %v", kind, answered)
		}
	}
	t.Logf("the tuple arm renders %v, out of the list %v", answered, list)
}

// edgeShapes is how many rows the view answers of each (from_kind, kind, to_kind), sorted: what
// a reader of a failure needs to see which arm the two readings disagree over.
func edgeShapes(rows []graphEdgeRow) []string {
	held := map[string]int{}
	for _, row := range rows {
		held[row.fromKind+" -"+row.kind+"-> "+row.toKind]++
	}
	shapes := make([]string, 0, len(held))
	for shape, count := range held {
		shapes = append(shapes, fmt.Sprintf("%s: %d", shape, count))
	}
	slices.Sort(shapes)
	return shapes
}

// requireSameEdges fails with every row one reading answers a different number of times from the
// other, both ways, so a run names each difference rather than the first: a row only one holds,
// and a row one answers twice, which an arm left answering beside the store's own does and a
// reading that only asked whether a row is there would miss. leftSays and rightSays say what each
// reading does with a row, written over "it".
func requireSameEdges(t *testing.T, stage, leftSays string, left []graphEdgeRow, rightSays string, right []graphEdgeRow) {
	t.Helper()
	leftCounts, rightCounts := edgeCounts(left), edgeCounts(right)
	for _, row := range slices.Sorted(maps.Keys(leftCounts)) {
		if over, under := leftCounts[row], rightCounts[row]; over != under {
			t.Errorf("%s: %q: %s %d times, and %s %d times", stage, row, leftSays, over, rightSays, under)
		}
	}
	for _, row := range slices.Sorted(maps.Keys(rightCounts)) {
		if leftCounts[row] == 0 {
			t.Errorf("%s: %q: %s 0 times, and %s %d times", stage, row, leftSays, rightSays, rightCounts[row])
		}
	}
	if len(left) != len(right) {
		t.Errorf("%s: %d rows where %s, and %d where %s", stage, len(left), leftSays, len(right), rightSays)
	}
}

// edgeCounts is how many times a reading answers each row.
func edgeCounts(rows []graphEdgeRow) map[string]int {
	counts := make(map[string]int, len(rows))
	for _, row := range rows {
		counts[row.String()]++
	}
	return counts
}

func armRelations(arms []graphEdgesArm) []string {
	relations := make([]string, len(arms))
	for index, arm := range arms {
		relations[index] = arm.relation
	}
	return relations
}

// armReport is the arm list as the run prints it: each arm by its place, the relation it reads
// and the fate the declaration gives it, or none for an arm the view has gained.
func armReport(read []graphEdgesArm) []string {
	lines := make([]string, len(read))
	for index, arm := range read {
		fate := "undeclared: this test does not know this arm"
		if index < len(graphEdgesArms) && graphEdgesArms[index].relation == arm.relation {
			fate = graphEdgesArms[index].fate
		}
		lines[index] = fmt.Sprintf("%2d. %s - %s", index+1, arm.relation, fate)
	}
	return lines
}

// readGraphEdgesArms reads graph_edges out of the catalogue and answers one entry per arm, each
// naming the relation that arm reads first and carrying the arm's own text. pg_get_viewdef's
// pretty form puts every top-level UNION ALL on a line of its own and opens each arm's own FROM
// at three spaces; an arm whose FROM opens a sub-select is named by the first relation inside
// it, which the store's own arm reads under a `distinct on`.
func readGraphEdgesArms(t *testing.T, database *store.Store) []graphEdgesArm {
	t.Helper()
	var definition string
	if err := database.Pool.QueryRow(context.Background(),
		`select pg_get_viewdef('graph_edges'::regclass, true)`).Scan(&definition); err != nil {
		t.Fatalf("read the graph_edges definition: %v", err)
	}
	var arms []graphEdgesArm
	for index, text := range strings.Split(strings.TrimSuffix(strings.TrimRight(definition, "\n"), ";"), "\nUNION ALL\n") {
		lines := strings.Split(text, "\n")
		relation := ""
		for position, line := range lines {
			rest, found := strings.CutPrefix(line, "   FROM ")
			if !found {
				continue
			}
			fields := strings.Fields(rest)
			if len(fields) == 0 {
				t.Fatalf("arm %d of graph_edges names no relation after its FROM: %q", index+1, line)
			}
			relation = fields[0]
			if relation == "(" {
				relation = subSelectFirstRelation(t, index+1, lines[position:])
			}
			break
		}
		if relation == "" {
			t.Fatalf("arm %d of graph_edges has no FROM of its own:\n%s", index+1, text)
		}
		arms = append(arms, graphEdgesArm{relation: relation, text: text})
	}
	return arms
}

// subSelectFirstRelation is the first relation a sub-select arm reads, which the catalogue
// prints indented past the arm's own FROM.
func subSelectFirstRelation(t *testing.T, arm int, lines []string) string {
	t.Helper()
	for _, line := range lines {
		trimmed := strings.TrimLeft(line, " ")
		if len(line)-len(trimmed) <= len("   ") {
			continue
		}
		if rest, found := strings.CutPrefix(trimmed, "FROM "); found {
			if fields := strings.Fields(rest); len(fields) > 0 && fields[0] != "(" {
				return fields[0]
			}
		}
	}
	t.Fatalf("arm %d of graph_edges opens a sub-select that names no relation:\n%s", arm, strings.Join(lines, "\n"))
	return ""
}

// graphEdgeRows is the whole of view, every column of every row.
func graphEdgeRows(t *testing.T, database *store.Store, view string) []graphEdgeRow {
	t.Helper()
	return scanGraphEdgeRows(t, database, view, `
		select from_kind, from_id, kind, to_kind, to_id, created_at, source_seq from `+view+`
		order by from_kind, from_id, kind, to_kind, to_id, created_at, source_seq
	`)
}

func scanGraphEdgeRows(t *testing.T, database *store.Store, what, statement string) []graphEdgeRow {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), statement)
	if err != nil {
		t.Fatalf("read %s: %v", what, err)
	}
	read, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (graphEdgeRow, error) {
		var edge graphEdgeRow
		err := row.Scan(&edge.fromKind, &edge.fromID, &edge.kind, &edge.toKind, &edge.toID, &edge.createdAt, &edge.sourceSeq)
		return edge, err
	})
	if err != nil {
		t.Fatalf("scan %s: %v", what, err)
	}
	return read
}

// backlinkTargets is every id of kind that an edge of the previous release's view, a tuple or a
// block reference points at when it is read, sorted, and one id nothing points at, which must be
// absent from every answer. The tuples and the block references are read beside the view because
// each can point where the view does not: a tuple outlives the row a build of the previous
// release deleted, and a pointer at a block retyped out of its allowed types is listed by no arm,
// so a count that answered either would rise at an id no edge of the view names.
func backlinkTargets(t *testing.T, database *store.Store, kind string) []string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select to_id from `+previousGraphEdges+` where to_kind = $1
		union select to_id from graph_tuples where to_kind = $1
		union select to_id from block_refs where to_kind = $1
		order by to_id
	`, kind)
	if err != nil {
		t.Fatalf("read the %s ids an edge, a tuple or a block reference points at: %v", kind, err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("scan the %s ids an edge, a tuple or a block reference points at: %v", kind, err)
	}
	return append(ids, "nothing-points-at-this")
}

// requireBacklinkCountsAsThePreviousRelease fails unless refs.BacklinkCounts answers, for every
// node kind its arm table declares and at every id backlinkTargets reads for that kind, what the
// previous release's count statement (docstest.ViewBacklinkCountSQL) answers over
// previousGraphEdges, naming the reading, the kind, the id and both counts at each id the two
// differ at. It answers this build's counts, kind by kind.
func requireBacklinkCountsAsThePreviousRelease(t *testing.T, database *store.Store, stage string) map[string]map[string]int {
	t.Helper()
	// The arm table declares a kind for each node kind the registry resolves and for no other, so
	// its kinds are the node kinds this build reads, the list refs binds where the statement
	// binds $3.
	kinds := refs.BacklinkKinds()
	counts := make(map[string]map[string]int, len(kinds))
	for _, kind := range kinds {
		ids := backlinkTargets(t, database, kind)
		count, err := refs.BacklinkCounts(context.Background(), database.Pool, model.Actor{}, kind, ids, nil)
		if err != nil {
			t.Fatalf("%s: count %s backlinks: %v", stage, kind, err)
		}
		previous := docstest.ViewBacklinkCounts(t, database.Pool, previousGraphEdges, kind, ids, kinds, nil)
		if !maps.Equal(count, previous) {
			named := false
			for _, id := range ids {
				if count[id] != previous[id] {
					named = true
					t.Errorf("%s: refs.BacklinkCounts counts %d at the %s %s, and the previous release's count statement over %s counts %d",
						stage, count[id], kind, id, previousGraphEdges, previous[id])
				}
			}
			// Both answer only ids they were asked about, and never a zero, so this is reached
			// only by an answer neither statement can give; it is reported whole rather than
			// passed over.
			if !named {
				t.Errorf("%s: refs.BacklinkCounts counts the %s backlinks %v, and the previous release's count statement over %s counts %v",
					stage, kind, count, previousGraphEdges, previous)
			}
		}
		counts[kind] = count
	}
	return counts
}

// requireTheCensusReportsEachDeletedRow fails unless the store census reports a difference at each
// row a build of the previous release deleted with no retraction: a block and a comment are each
// a live node no row projects, and a block reference is its block's other row, whose links no
// longer agree with the ones its node states.
func requireTheCensusReportsEachDeletedRow(t *testing.T, census *graphcensus.StoreReport, deleted []graphEdge) {
	t.Helper()
	var found []string
	for _, kind := range census.Kinds {
		for _, difference := range append(slices.Clone(kind.ReportedRows), kind.ReportedNodes...) {
			found = append(found, kind.Kind+": "+difference.At+": "+difference.Cause)
		}
	}
	t.Logf("the store census after the rollback reports %d differences:\n  %s", census.Differences(), strings.Join(found, "\n  "))
	for _, edge := range deleted {
		var at, cause string
		switch {
		case edge.kind == "part_of" && edge.fromKind == refs.BlockNodeKind:
			at, cause = refs.BlockNodeKind+": block "+edge.fromID, "a live node no row projects"
		case edge.fromKind == refs.BlockNodeKind:
			at, cause = "block_ref: block_ref "+edge.fromID+" ", "the row and its node disagree"
		default:
			at, cause = "comment: comment "+edge.fromID, "a live node no row projects"
		}
		if !slices.ContainsFunc(found, func(line string) bool {
			return strings.HasPrefix(line, at) && strings.Contains(line, cause)
		}) {
			t.Errorf("the store census reports no difference for the deleted row %s (want %q at %q)", edge, cause, at)
		}
	}
}

// requireTheAnnouncementListsTheArmsKinds seeds the announcement a build of the previous release
// makes, an insert naming only its commit, registers a process serving this build, and runs this
// build's announcer until it announces: the newest announcement then lists refs.TupleKinds, the
// kinds the tuple arm renders, and the earlier one lists none.
func requireTheAnnouncementListsTheArmsKinds(t *testing.T, database *store.Store) {
	t.Helper()
	const (
		previousBuild = "0000000000000000000000000000000000000001"
		thisBuild     = "0000000000000000000000000000000000000002"
	)
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `insert into server_build_announcements (commit) values ($1)`, previousBuild); err != nil {
		t.Fatalf("announce the previous release's build as it announces itself: %v", err)
	}
	registration, err := processes.Register(ctx, database.Pool, processes.Instance{Commit: thisBuild, SigningKeys: []string{"graph-edges-test-key"}})
	if err != nil {
		t.Fatalf("register a process serving this build: %v", err)
	}
	defer func() { _ = registration.Deregister(context.Background()) }()
	running, stop := context.WithCancel(ctx)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		startup.New(startup.Deps{Pool: database.Pool, Commit: thisBuild, TupleKinds: refs.TupleKinds}).Run(running)
	}()
	deadline := time.Now().Add(30 * time.Second)
	var announced int
	for announced == 0 && time.Now().Before(deadline) {
		if err := database.Pool.QueryRow(ctx, `select count(*) from server_build_announcements where commit = $1`, thisBuild).Scan(&announced); err != nil {
			t.Fatalf("read the announcements: %v", err)
		}
		if announced == 0 {
			time.Sleep(50 * time.Millisecond)
		}
	}
	stop()
	<-stopped
	rows, err := database.Pool.Query(ctx, `select commit, tuple_kinds from server_build_announcements order by seq`)
	if err != nil {
		t.Fatalf("read the announcements: %v", err)
	}
	type announcement struct {
		commit string
		kinds  []string
	}
	read, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (announcement, error) {
		var a announcement
		err := row.Scan(&a.commit, &a.kinds)
		return a, err
	})
	if err != nil {
		t.Fatalf("scan the announcements: %v", err)
	}
	if len(read) != 2 || read[0].commit != previousBuild || read[1].commit != thisBuild {
		t.Fatalf("the announcements are %+v, want the previous release's build and then this one's", read)
	}
	if want := refs.TupleKinds(); !slices.Equal(read[1].kinds, want) {
		t.Errorf("the newest announcement lists the kinds %v, want %v, the list the tuple arm renders", read[1].kinds, want)
	}
	if read[0].kinds != nil {
		t.Errorf("the previous release's announcement lists the kinds %v, want none", read[0].kinds)
	}
	t.Logf("the newest announcement, of %s, lists %v; the previous release's, of %s, lists none", thisBuild, read[1].kinds, previousBuild)
}

// unwriteTheStoreOfADocument puts the rows of the document at refKey back in the shape the
// release before the node and tuple store left them in: its document_blocks and block_refs
// rows name no node, and the store holds neither the nodes of its blocks nor anything stated
// about them. That is what a database holds for a document the backfill has not reached, and
// what a pod of that release writes while a roll is half done. The view then answers those
// rows from the four old arms and every other row from the store, which is the reading this
// test takes first.
func unwriteTheStoreOfADocument(t *testing.T, database *store.Store, refKey string) {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the unwriting of %s: %v", refKey, err)
	}
	defer tx.Rollback(ctx)
	var rows, links int64
	for _, unwritten := range []struct {
		what, statement string
		touched         *int64
	}{
		{"document_blocks", `update document_blocks d set node_kind = null, node_address = null, parent_block_id = null
		 from artifacts a where a.id = d.artifact_id and a.ref_key = $1`, &rows},
		{"block_refs", `update block_refs r set node_kind = null, node_address = null
		 from artifacts a where a.id = r.artifact_id and a.ref_key = $1`, &links},
	} {
		tag, err := tx.Exec(ctx, unwritten.statement, refKey)
		if err != nil {
			t.Fatalf("take the node columns off the %s rows of %s: %v", unwritten.what, refKey, err)
		}
		*unwritten.touched = tag.RowsAffected()
	}
	for _, statement := range []string{
		`delete from graph_tuples where from_kind = $1 and starts_with(from_id, $2)`,
		`delete from graph_nodes where kind = $1 and starts_with(id, $2)`,
	} {
		if _, err := tx.Exec(ctx, statement, refs.BlockNodeKind, refKey+"#"); err != nil {
			t.Fatalf("take the store's rows of the blocks of %s away: %v", refKey, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the unwriting of %s: %v", refKey, err)
	}
	// The reading that follows holds the per-row rule only if this document really left rows
	// of both tables naming no node.
	if rows == 0 || links == 0 {
		t.Fatalf("unwriting %s left %d block rows and %d link rows naming no node, want some of each", refKey, rows, links)
	}
	t.Logf("%s holds %d block rows and %d link rows that name no node", refKey, rows, links)
}

// requireTheIndexStampedTheOtherDocuments reads, before the backfill, the rows of the two
// documents the fixture leaves as the block index wrote them, target.md and plan.md: each holds
// block rows, plan.md holds link rows too, and every one of them carries the node the index
// stamped. The tuple arm is held to answering exactly the rows that carry their node, so a
// fixture whose rows carried none would hold it to answering nothing and pass.
func requireTheIndexStampedTheOtherDocuments(t *testing.T, database *store.Store) {
	t.Helper()
	for _, held := range []struct{ table, refKey string }{
		{"document_blocks", "CORE-1/target"},
		{"document_blocks", "CORE-1/plan"},
		{"block_refs", "CORE-1/plan"},
	} {
		var rows, stamped int
		if err := database.Pool.QueryRow(context.Background(), `
			select count(*), count(*) filter (where r.node_kind is not null and r.node_address is not null)
			from `+held.table+` r join artifacts a on a.id = r.artifact_id
			where a.ref_key = $1
		`, held.refKey).Scan(&rows, &stamped); err != nil {
			t.Fatalf("read the %s rows of %s: %v", held.table, held.refKey, err)
		}
		if rows == 0 || stamped != rows {
			t.Errorf("before the backfill %s holds %d %s row(s), %d of them naming the node the block index stamped, want some and every one",
				held.refKey, rows, held.table, stamped)
			continue
		}
		t.Logf("before the backfill %s holds %d %s row(s), each naming its node", held.refKey, rows, held.table)
	}
}

// writeAsThePreviousRelease writes a comment and a block reference as a pod of the release before
// this one writes them, naming no node and stating no tuple, and answers their edges: a comment
// the plan owns, and a mention of CORE-2 out of the target's r1, whose own row carries its node.
func writeAsThePreviousRelease(t *testing.T, database *store.Store, fixture graphEdgeFixture) []graphEdge {
	t.Helper()
	ctx := context.Background()
	comment := scanGraphEdgeFixture(t, database, "the previous release's comment", `
		insert into comments (artifact_id, author, body)
		values ($1::uuid, '{"kind":"user","id":"alice"}', 'written by the previous release')
		returning id::text
	`, fixture.plan)
	if _, err := database.Pool.Exec(ctx, `
		insert into block_refs (artifact_id, block_id, from_node, to_kind, to_id, kind, created_version)
		values ($1::uuid, 'r1', 'CORE-1/target#r1', 'issue', 'CORE-2', 'mentions', 2)
	`, fixture.target); err != nil {
		t.Fatalf("write the previous release's block reference: %v", err)
	}
	return []graphEdge{
		{"comment", comment, "owned_by", "artifact", "CORE-1/plan"},
		{refs.BlockNodeKind, "CORE-1/target#r1", "mentions", "issue", "CORE-2"},
	}
}

// rollBackAsThePreviousRelease does what a build of the release before this one does after a
// rollback, which retracts nothing in the store: it deletes a stamped block row (the target's
// paragraph, which holds no link), a stamped block reference (the mention of CORE-2 from t1's
// paragraph, beside that paragraph's mention of the target, so the block keeps a row the census
// reads its links against, and beside the plan's other mentions of CORE-2, so the document's own
// mention stays held by a block) and a stamped comment (the one anchored on the plan), and
// deletes a second stamped block row (the plan's outer) and block reference (the mention out of
// the inner block's paragraph) and writes each again naming no node, dated by that write. It
// answers the edges of the deleted rows and of the rewritten ones.
func rollBackAsThePreviousRelease(t *testing.T, database *store.Store, fixture graphEdgeFixture) (deleted, rewritten []graphEdge) {
	t.Helper()
	ctx := context.Background()
	var paragraphs []string
	rows, err := database.Pool.Query(ctx, `
		delete from document_blocks d using artifacts a
		where a.id = d.artifact_id and a.ref_key = 'CORE-1/target' and d.type = 'paragraph'
		  and d.parent_block_id is null and d.node_kind is not null
		returning d.node_id
	`)
	if err == nil {
		paragraphs, err = pgx.CollectRows(rows, pgx.RowTo[string])
	}
	if err != nil || len(paragraphs) != 1 {
		t.Fatalf("delete the target's paragraph: deleted %v, %v; want its one stamped row", paragraphs, err)
	}
	cites, inner := paragraphOf(t, database, "t1"), paragraphOf(t, database, "inner")
	if tag, err := database.Pool.Exec(ctx, `
		delete from block_refs where from_node = $1 and kind = 'mentions' and to_kind = 'issue' and to_id = 'CORE-2'
		  and node_kind is not null
	`, cites); err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("delete the mention of CORE-2 out of t1's paragraph: %d rows, %v", tag.RowsAffected(), err)
	}
	if tag, err := database.Pool.Exec(ctx, `delete from comments where id = $1::uuid and node_kind is not null`, fixture.anchored); err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("delete the anchored comment: %d rows, %v", tag.RowsAffected(), err)
	}
	rewriteAsThePreviousRelease(t, database, "the plan's outer block", "document_blocks", `node_id = $1`, "CORE-1/plan#outer")
	rewriteAsThePreviousRelease(t, database, "the mention out of the inner block's paragraph", "block_refs",
		`from_node = $1 and kind = 'mentions' and to_kind = 'issue' and to_id = 'CORE-2'`, inner)
	deleted = []graphEdge{
		{refs.BlockNodeKind, paragraphs[0], "part_of", "artifact", "CORE-1/target"},
		{refs.BlockNodeKind, cites, "mentions", "issue", "CORE-2"},
		{"comment", fixture.anchored, "anchored_to", "artifact", "CORE-1/plan"},
	}
	rewritten = []graphEdge{
		{refs.BlockNodeKind, "CORE-1/plan#outer", "part_of", "artifact", "CORE-1/plan"},
		{refs.BlockNodeKind, inner, "mentions", "issue", "CORE-2"},
	}
	return deleted, rewritten
}

// paragraphOf is the node address of the paragraph the plan's block parent holds, which is the
// block that holds the links written in that block's text.
func paragraphOf(t *testing.T, database *store.Store, parent string) string {
	t.Helper()
	return scanGraphEdgeFixture(t, database, "the paragraph of the plan's "+parent, `
		select d.node_id from document_blocks d join artifacts a on a.id = d.artifact_id
		where a.ref_key = 'CORE-1/plan' and d.parent_block_id = $1 and d.type = 'paragraph'
	`, parent)
}

// rewriteAsThePreviousRelease deletes the one stamped row of table that key names, with args
// bound, and writes it again as a build of the previous release writes such a row: the same row,
// naming no node, with no parent where the table has one, and dated by the write that inserted it.
func rewriteAsThePreviousRelease(t *testing.T, database *store.Store, what, table, key string, args ...any) {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the rewrite of %s: %v", what, err)
	}
	defer tx.Rollback(ctx)
	unset := "node_kind = null, node_address = null, created_at = clock_timestamp()"
	if table == "document_blocks" {
		unset += ", parent_block_id = null"
	}
	for _, statement := range []struct {
		sql   string
		bound bool
	}{
		{`create temp table rewritten on commit drop as select * from ` + table + ` where ` + key + ` and node_kind is not null`, true},
		{`delete from ` + table + ` where ` + key, true},
		{`update rewritten set ` + unset, false},
		{`insert into ` + table + ` select * from rewritten`, false},
	} {
		var bound []any
		if statement.bound {
			bound = args
		}
		tag, err := tx.Exec(ctx, statement.sql, bound...)
		if err != nil {
			t.Fatalf("rewrite %s: %s: %v", what, statement.sql, err)
		}
		if !strings.HasPrefix(statement.sql, "create") && tag.RowsAffected() != 1 {
			t.Fatalf("rewrite %s: %s touched %d rows, want the one", what, statement.sql, tag.RowsAffected())
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the rewrite of %s: %v", what, err)
	}
}

// The documents the fixture writes. target.md holds the two requirements a plan_task's serves
// may point at; its second version retypes one of them to a callout, which the attribute does
// not allow, so the link stored at it is listed by no arm. plan.md holds a pointer at each, the
// first holding a paragraph that mentions both CORE-2 and the target, a mention of CORE-2 from a
// block inside another block, a block a later version drops and a later one brings back, and a
// block a later version drops for good.
// legacy.md is the document whose rows are put back in the shape the release before this one
// left them in: it holds a nested block and a mention, so each of the two block arms has a row of
// its own to answer while the rows of the other two documents are answered by the store.
const (
	graphEdgeTarget = ":::requirement{#r1}\nThe view answers one row per edge.\n:::\n\n" +
		":::requirement{#r2}\nA requirement a later version retypes.\n:::\n\n" +
		"A paragraph of the target.\n"

	graphEdgeRetyped = ":::requirement{#r1}\nThe view answers one row per edge.\n:::\n\n" +
		":::callout{#r2 kind=\"note\" title=\"\"}\nA requirement a later version retypes.\n:::\n\n" +
		"A paragraph of the target.\n"

	graphEdgePlanPointers = ":::plan_task{#t1 serves=\"dispatch://CORE-1/artifact/target#b-r1\"}\n" +
		"Serve the requirement, after dispatch://CORE-2 and dispatch://CORE-1/artifact/target.\n:::\n\n" +
		":::plan_task{#t2 serves=\"dispatch://CORE-1/artifact/target#b-r2\"}\nServe the one that is retyped.\n:::\n\n" +
		"::::callout{#outer kind=\"note\" title=\"\"}\nOuter.\n\n" +
		":::callout{#inner kind=\"note\" title=\"\"}\nInner, after dispatch://CORE-2.\n:::\n\n::::\n"

	graphEdgePlanWhole = graphEdgePlanPointers + "\n" +
		":::callout{#revive kind=\"note\" title=\"\"}\nA block a version drops and another brings back, citing dispatch://CORE-2.\n:::\n\n" +
		":::callout{#dropped kind=\"note\" title=\"\"}\nA block a version drops for good, citing dispatch://CORE-2.\n:::\n"

	graphEdgePlanRestored = graphEdgePlanPointers + "\n" +
		":::callout{#revive kind=\"note\" title=\"\"}\nA block a version drops and another brings back, citing dispatch://CORE-2.\n:::\n"

	graphEdgeLegacy = "::::callout{#held kind=\"note\" title=\"\"}\nA block that holds another.\n\n" +
		":::callout{#inside kind=\"note\" title=\"\"}\nInside, after dispatch://CORE-2.\n:::\n\n::::\n"
)

// graphEdgeLegacyRefKey is the ref_key of legacy.md, the document unwriteTheStoreOfADocument is
// run over: `createOwedDocument` hangs every document of this fixture on CORE-1 under its slug.
const graphEdgeLegacyRefKey = "CORE-1/legacy"

// graphEdgeFixture is what seedGraphEdgeFixture wrote that the rollback names: the ids of the
// target and the plan, and of the comment anchored on the plan.
type graphEdgeFixture struct {
	target, plan, anchored string
}

// seedGraphEdgeFixture writes what the four moved arms carry, through the paths that write it in
// a deployment: three documents indexed by the block-index tick, one of them at three versions
// so that a block is dropped and brought back and so that a version's edges carry the
// source_seq of the event that recorded it, and three comments written as a build before the
// store existed wrote them, which the backfill states. The third document, legacy.md, is the
// one the caller unwrites the store of.
func seedGraphEdgeFixture(t *testing.T, database *store.Store, service *Service) graphEdgeFixture {
	t.Helper()
	ctx := context.Background()
	// The target is indexed before the document that points at it: a write states a pointer
	// only at a block the store holds and only at a type the attribute allows, which is why the
	// link at a type it does not allow is made by retyping the target below.
	target := createOwedDocument(t, database, service, "CORE-1", "", "target", graphEdgeTarget, "")
	settle(t, database, target)
	if ticks := drainBlockIndex(t, service, "the target document"); ticks == 0 {
		t.Fatal("the fixture indexed no document")
	}
	plan := createOwedDocument(t, database, service, "CORE-1", "", "plan", graphEdgePlanWhole, "")
	settle(t, database, plan)
	if ticks := drainBlockIndex(t, service, "the document that points at it"); ticks == 0 {
		t.Fatal("the fixture indexed no document holding a pointer")
	}

	legacy := createOwedDocument(t, database, service, "CORE-1", "", "legacy", graphEdgeLegacy, "")
	settle(t, database, legacy)
	if ticks := drainBlockIndex(t, service, "the document the store is unwritten of"); ticks == 0 {
		t.Fatal("the fixture indexed no document to unwrite the store of")
	}

	// The retype, which no pass over plan.md follows: its stored link at r2 stands and is
	// listed by neither arm while r2 is a callout.
	writeVersionAsAnOlderBuild(t, database, service, target, "target.md", graphEdgeRetyped)
	if ticks := drainBlockIndex(t, service, "the version that retypes r2"); ticks == 0 {
		t.Fatal("the version that retypes r2 was not indexed")
	}

	// Version 2 of plan.md drops two blocks: the mentions they held are retracted with their
	// rows, and their nodes are stated gone while their in_document tuples stand, which the
	// tuple arm's join and its gone test each keep out. Version 3 brings one of them back under
	// its own id.
	writeVersionAsAnOlderBuild(t, database, service, plan, "plan.md", graphEdgePlanPointers)
	if ticks := drainBlockIndex(t, service, "the version that drops two blocks"); ticks == 0 {
		t.Fatal("the version that drops two blocks was not indexed")
	}
	writeVersionAsAnOlderBuild(t, database, service, plan, "plan.md", graphEdgePlanRestored)
	if ticks := drainBlockIndex(t, service, "the version that brings one back"); ticks == 0 {
		t.Fatal("the version that brings one back was not indexed")
	}

	// The block version 2 dropped and version 3 brought back is one node, stated gone and then
	// live again, and its row, written again by version 3, names that node.
	requireTheRestoredBlockIsLive(t, database, "CORE-1/plan", "revive")

	var gone, live int
	if err := database.Pool.QueryRow(ctx, `
		select count(*) filter (where state = 'gone'), count(*) filter (where state = 'live')
		from graph_nodes where kind = $1
	`, refs.BlockNodeKind).Scan(&gone, &live); err != nil {
		t.Fatalf("count the block nodes: %v", err)
	}
	if gone == 0 || live == 0 {
		t.Fatalf("the fixture left %d gone and %d live block nodes; it must hold both", gone, live)
	}
	var pointers int
	if err := database.Pool.QueryRow(ctx,
		`select count(*) from block_refs where to_types is not null`).Scan(&pointers); err != nil {
		t.Fatalf("count the pointers: %v", err)
	}
	if pointers != 2 {
		t.Fatalf("the fixture stored %d pointers with targets; want the two plan_task serves rows", pointers)
	}

	// The comments of a build before the store existed: one anchored on a document, one owned
	// by one, and a reply, whose own arms this change leaves alone.
	owner := scanGraphEdgeFixture(t, database, "the owned comment", `
		insert into comments (artifact_id, author, body)
		values ($1::uuid, '{"kind":"user","id":"alice"}', 'owned by the plan')
		returning id::text
	`, plan)
	anchored := scanGraphEdgeFixture(t, database, "the anchored comment", `
		insert into comments (issue_key, author, body, anchor)
		values ('CORE-1', '{"kind":"user","id":"alice"}', 'anchored on the plan', jsonb_build_object('artifact_id', $1::text))
		returning id::text
	`, plan)
	if _, err := database.Pool.Exec(ctx, `
		insert into comments (artifact_id, author, body, reply_to)
		values ($1::uuid, '{"kind":"user","id":"alice"}', 'a reply', $2::uuid)
	`, plan, owner); err != nil {
		t.Fatalf("write the reply: %v", err)
	}
	return graphEdgeFixture{target: target, plan: plan, anchored: anchored}
}

// writeVersionAsAnOlderBuild replaces the document's live text with markdown and writes the
// version a build without the block index wrote: the version row, the artifact.version event
// that recorded it, whose seq it takes off the issue's own counter as events.Append does, and the
// settlement cleared. The trigger of migration 0122 leaves the document owed a block index, and
// the tick indexes it, which is the one path in this package that states a version's blocks and
// links in the store.
func writeVersionAsAnOlderBuild(t *testing.T, database *store.Store, service *Service, artifactID, name, markdown string) {
	t.Helper()
	ctx := context.Background()
	browserReplaceText(t, service, artifactID, markdown, model.Actor{Kind: "user", ID: "alice"})
	waitForLandedAppends(t, service, artifactID)
	live, _, err := service.TextWithBlocks(ctx, artifactID)
	if err != nil {
		t.Fatalf("read the live text of %s: %v", artifactID, err)
	}
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the version: %v", err)
	}
	defer tx.Rollback(ctx)
	var number int
	if err := tx.QueryRow(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, authors)
		select $1, max(number) + 1, $2, '[{"kind":"user","id":"alice"}]' from artifact_versions where artifact_id = $1
		returning number
	`, artifactID, live).Scan(&number); err != nil {
		t.Fatalf("write the version of %s: %v", artifactID, err)
	}
	if _, err := tx.Exec(ctx, `
		with bumped as (update issues set last_seq = last_seq + 1 where key = 'CORE-1' returning last_seq)
		insert into events (issue_key, seq, type, actor, payload, notify)
		select 'CORE-1', bumped.last_seq, 'artifact.version', '{"kind":"user","id":"alice"}',
		       jsonb_build_object('artifact_id', $1::text, 'name', $3::text, 'version', jsonb_build_object('number', $2::int)), false
		from bumped
	`, artifactID, number, name); err != nil {
		t.Fatalf("append the version's event: %v", err)
	}
	if _, err := tx.Exec(ctx, `delete from doc_settlements_pending where artifact_id = $1`, artifactID); err != nil {
		t.Fatalf("settle %s: %v", artifactID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the version of %s: %v", artifactID, err)
	}
}

func scanGraphEdgeFixture(t *testing.T, database *store.Store, what, statement string, args ...any) string {
	t.Helper()
	var id string
	if err := database.Pool.QueryRow(context.Background(), statement, args...).Scan(&id); err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	return id
}

// requireTheRestoredBlockIsLive fails unless the block blockID of the document refKey, which a
// version dropped and a later one brought back, is live now with gone before it in its state's
// history, and unless its document_blocks row names that node.
func requireTheRestoredBlockIsLive(t *testing.T, database *store.Store, refKey, blockID string) {
	t.Helper()
	ctx := context.Background()
	address := refKey + "#" + blockID
	node, held, err := graphstore.ReadNode(ctx, database.Pool, refs.BlockNodeKind, address)
	if err != nil || !held {
		t.Fatalf("read the node of the restored block %s: held %t, %v", address, held, err)
	}
	if node.State != graphstore.StateLive {
		t.Errorf("the restored block %s is %s, want %s", address, node.State, graphstore.StateLive)
	}
	history, err := graphstore.History(ctx, database.Pool, refs.BlockNodeKind, address, graphstore.AttributeState)
	if err != nil {
		t.Fatalf("read the state history of %s: %v", address, err)
	}
	states := make([]string, 0, len(history))
	for _, stated := range history {
		states = append(states, stated.ToID)
	}
	gone := slices.Index(states, graphstore.StateGone)
	if gone < 0 || states[len(states)-1] != graphstore.StateLive {
		t.Errorf("the restored block %s states %v, want gone once a version dropped it and live last, once one brought it back",
			address, states)
	}
	var nodeKind, nodeAddress *string
	if err := database.Pool.QueryRow(ctx, `
		select d.node_kind, d.node_address from document_blocks d join artifacts a on a.id = d.artifact_id
		where a.ref_key = $1 and d.block_id = $2
	`, refKey, blockID).Scan(&nodeKind, &nodeAddress); err != nil {
		t.Fatalf("read the row of the restored block %s: %v", address, err)
	}
	if nodeKind == nil || nodeAddress == nil || *nodeKind != refs.BlockNodeKind || *nodeAddress != address {
		t.Errorf("the row of the restored block %s names node %s/%s, want %s/%s", address,
			deref(nodeKind), deref(nodeAddress), refs.BlockNodeKind, address)
	}
	t.Logf("the restored block %s is %s, its state history %v, and its row names it", address, node.State, states)
}

// deref is the text a nullable column holds, or null where it holds none.
func deref(text *string) string {
	if text == nil {
		return "null"
	}
	return *text
}

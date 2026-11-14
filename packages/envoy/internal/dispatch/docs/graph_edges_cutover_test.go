package docs

import (
	"context"
	"fmt"
	"io"
	"maps"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// graphEdgesArm is one arm of graph_edges as this test reads it back: the relation the arm reads
// first, and what is to become of it. The relation is enough to name an arm here because the
// list is held in the view's own order, so two arms over one table are told apart by where they
// stand. cutover is, for the four arms over a kind this change moved, the kind whose
// graph_store_cutover row the arm tests, and empty for every other arm. text is the arm's own
// SQL as the catalogue prints it, which only an arm read back carries.
type graphEdgesArm struct {
	relation string
	fate     string
	cutover  string
	text     string
}

// movedArmsDeletedBy is the child that deletes the four arms over the kinds this change moved,
// once the store answers those kinds everywhere.
const movedArmsDeletedBy = "dispatch://INFRA-1253"

// graphEdgesArms is every arm of graph_edges, in the order the view states them. An arm the view
// gains or loses fails this test, whichever source column it reads: the store is to answer each
// kind in the end, and an arm added over a column now is a kind that would have to be moved
// twice. The fate of each is printed with the list when it fails, so a reader sees which arms
// are left and who takes each.
var graphEdgesArms = []graphEdgesArm{
	{relation: "refs", fate: "a mention of any node from an item; no child has moved it"},
	{relation: "issues", fate: "child_of; dispatch://INFRA-1156 makes an issue's relations typed edges"},
	{relation: "issue_links", fate: "blocked_by; dispatch://INFRA-1156 makes an issue's relations typed edges"},
	{relation: "artifacts", fate: "attached_to; no child has moved it"},
	{relation: "asks", fate: "an ask's anchored_to; dispatch://INFRA-1152 writes an ask's relations"},
	{relation: "comments", cutover: "comment", fate: "a comment's anchored_to; the store answers it while graph_store_cutover says comment, and " + movedArmsDeletedBy + " deletes this arm"},
	{relation: "asks", fate: "an ask's owned_by; dispatch://INFRA-1152 writes an ask's relations"},
	{relation: "comments", cutover: "comment", fate: "a comment's owned_by; the store answers it while graph_store_cutover says comment, and " + movedArmsDeletedBy + " deletes this arm"},
	{relation: "comments", fate: "a reply's replies_to its thread; dispatch://INFRA-1152 writes it"},
	{relation: "comments", fate: "a reply's answers; dispatch://INFRA-1152 writes it"},
	{relation: "comments", fate: "a reply's replies_to an ask; dispatch://INFRA-1152 writes it"},
	{relation: "messages", fate: "a message's replies_to; dispatch://INFRA-1152 writes it"},
	{relation: "ask_followers", fate: "followed_by; dispatch://INFRA-1152 writes an ask's relations"},
	{relation: "components", fate: "a component's part_of; no child has moved it"},
	{relation: "component_depends", fate: "depends_on; no child has moved it"},
	{relation: "issue_component_members", fate: "affects; no child has moved it"},
	{relation: "block_refs", cutover: "block_ref", fate: "a block's links; the store answers them while graph_store_cutover says block_ref, and " + movedArmsDeletedBy + " deletes this arm"},
	{relation: "document_blocks", cutover: "block", fate: "a block's part_of its document; the store answers it while graph_store_cutover says block, and " + movedArmsDeletedBy + " deletes this arm"},
	{relation: "comment_blocks", fate: "a comment block's part_of; dispatch://INFRA-1152 writes an item's blocks"},
	{relation: "message_blocks", fate: "a message block's part_of; dispatch://INFRA-1152 writes an item's blocks"},
	{relation: "ask_blocks", fate: "an ask part's part_of; dispatch://INFRA-1152 writes an item's blocks"},
	{relation: "comment_block_refs", fate: "a comment block's links; dispatch://INFRA-1152 writes an item's blocks"},
	{relation: "message_block_refs", fate: "a message block's links; dispatch://INFRA-1152 writes an item's blocks"},
	{relation: "ask_block_refs", fate: "an ask part's links; dispatch://INFRA-1152 writes an item's blocks"},
	{relation: "comments", fate: "a reply's replies_to a comment's block; dispatch://INFRA-1152 writes it"},
	{relation: "comments", fate: "a reply's replies_to an ask's part; dispatch://INFRA-1152 writes it"},
	{relation: "messages", fate: "a message's replies_to a message's block; dispatch://INFRA-1152 writes it"},
	{relation: "graph_tuples", fate: "the store itself, which answers each kind whose cutover row reads true"},
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
	return fmt.Sprintf("%s %s -%s-> %s %s at %s, %s",
		r.fromKind, r.fromID, r.kind, r.toKind, r.toID, r.createdAt.UTC().Format(time.RFC3339Nano), seq)
}

// The view answers the same rows whichever side of the cutover it reads them from.
//
// Four things are held at once. The arm list is read back out of the catalogue and compared
// with the one declared above, so no arm over a source column can be added anywhere in this
// branch without this test naming it, and each of the four arms over a moved kind is read for a
// graph_store_cutover test of its own kind, with the child that deletes it named in its fate.
// Then the whole view is read with every graph_store_cutover row off, which is the four arms
// over the projections, and again with every one on, which is the one arm over the current
// tuples, and the two readings are compared row for row, each row's time and causing write
// included; with the rows on, each of the four arms is read on its own and answers nothing.
// Then refs.BacklinkCounts is read the same two ways for every node kind its arm table
// declares, so an inbound count that falls or rises is caught beside the rows.
//
// The seed is written through the real write paths: the block-index tick, which is the one
// writer in this package that states what a version came to in the store, and the backfill,
// which is what states a comment written before the store existed. It holds every kind of edge
// the four moved arms carry - a block's membership of its document, a block's mention, a
// pointer the attribute's targets allow and one they do not, and a comment's anchored_to and
// owned_by - and a delete and restore of a block, which is where the two rules the tuple arm
// carries are read: a node whose state is gone answers no row, and a pointer a version dropped
// is retracted with the row.
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

	seedGraphEdgeFixture(t, database, service)
	report, err := graphcensus.Backfill(ctx, database, io.Discard)
	if err != nil {
		t.Fatalf("backfill the store: %v", err)
	}
	for _, kind := range report.Kinds {
		t.Logf("backfill kind=%s rows=%d nodes=%d tuples=%d remaining=%d cutover=%t",
			kind.Kind, kind.Rows, kind.Nodes, kind.Tuples, kind.Remaining, kind.Complete)
		if !kind.Complete {
			t.Fatalf("the backfill left %s short of its cutover: %#v", kind.Kind, kind)
		}
	}

	setGraphStoreCutover(t, database, false)
	projected := graphEdgeRows(t, database)
	projectedCounts := graphEdgeBacklinkCounts(t, database, projected)
	// Each of the four answers rows of its own while its cutover row is off, so finding it
	// empty once the row is on is the cutover test at work and not an arm with nothing to say.
	for _, arm := range moved {
		rows := armRows(t, database, arm)
		t.Logf("arm %d (%s) answers %d rows with the cutover off", arm.place, arm.relation, rows)
		if rows == 0 {
			t.Errorf("arm %d (%s) answers no row with the cutover off, so finding it empty with the cutover on would prove nothing",
				arm.place, arm.relation)
		}
	}
	setGraphStoreCutover(t, database, true)
	stored := graphEdgeRows(t, database)
	storedCounts := graphEdgeBacklinkCounts(t, database, projected)
	for _, arm := range moved {
		if rows := armRows(t, database, arm); rows != 0 {
			t.Errorf("arm %d (%s, %s) answers %d rows with every cutover row on, want none: the store's arm answers its kind now",
				arm.place, arm.relation, arm.fate, rows)
		}
	}

	if len(projected) == 0 {
		t.Fatal("the view answers no row with the cutover off, so this test would prove nothing")
	}
	t.Logf("the view answers %d rows over the projections and %d over the store:\n  %s",
		len(projected), len(stored), strings.Join(edgeShapes(projected), "\n  "))
	requireEveryMovedKind(t, projected)
	requireSameEdges(t, projected, stored)
	for _, kind := range refs.BacklinkKinds() {
		if !maps.Equal(projectedCounts[kind], storedCounts[kind]) {
			t.Errorf("the %s backlink counts are %v over the projections and %v over the store",
				kind, projectedCounts[kind], storedCounts[kind])
		}
	}
	// A count map that holds nothing compares equal to another that holds nothing, so the kinds
	// the four arms point at must each be counted at all.
	for _, kind := range []string{"artifact", "issue", refs.BlockNodeKind} {
		if len(projectedCounts[kind]) == 0 {
			t.Errorf("the %s backlink counts over the projections are empty, so their comparison proves nothing", kind)
		}
	}
}

// movedArm is one of the four arms over a kind this change moved, as read back: its place in
// the view, counted from one, and the arm itself.
type movedArm struct {
	place int
	graphEdgesArm
}

// cutoverTest matches each graph_store_cutover row an arm's text tests, by the kind it names,
// as the catalogue prints the test.
var cutoverTest = regexp.MustCompile(`graph_store_cutover\.kind = '([a-z_]+)'::text`)

// movedArms holds each of the four arms over a moved kind to the cutover test of its own kind
// and to the child that deletes it, and answers them with their text for reading on their own.
// An arm that tested another kind's row would go quiet when that kind moved, not its own, and
// answer its kind twice in between.
func movedArms(t *testing.T, read []graphEdgesArm) []movedArm {
	t.Helper()
	var moved []movedArm
	for index, declared := range graphEdgesArms {
		if declared.cutover == "" {
			continue
		}
		arm := read[index]
		arm.fate, arm.cutover = declared.fate, declared.cutover
		var tested []string
		for _, found := range cutoverTest.FindAllStringSubmatch(arm.text, -1) {
			tested = append(tested, found[1])
		}
		if len(tested) == 0 || slices.ContainsFunc(tested, func(kind string) bool { return kind != declared.cutover }) {
			t.Errorf("arm %d (%s) tests the graph_store_cutover rows %v, want the %s row alone:\n%s",
				index+1, arm.relation, tested, declared.cutover, arm.text)
		}
		if !strings.Contains(declared.fate, movedArmsDeletedBy) {
			t.Errorf("arm %d (%s) over the moved kind %s is declared with the fate %q, which does not name %s, the child that deletes it",
				index+1, arm.relation, declared.cutover, declared.fate, movedArmsDeletedBy)
		}
		moved = append(moved, movedArm{place: index + 1, graphEdgesArm: arm})
	}
	if len(moved) != 4 {
		t.Fatalf("this test declares %d arms over a moved kind, want the four this change moved", len(moved))
	}
	return moved
}

// armRows is how many rows one arm of the view answers read on its own.
func armRows(t *testing.T, database *store.Store, arm movedArm) int {
	t.Helper()
	var rows int
	if err := database.Pool.QueryRow(context.Background(),
		`select count(*) from (`+arm.text+`) arm`).Scan(&rows); err != nil {
		t.Fatalf("read arm %d (%s) on its own: %v", arm.place, arm.relation, err)
	}
	return rows
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

// requireSameEdges fails with every row one reading holds and the other does not, both ways, so
// a run names each difference rather than the first, and with every row one reading answers
// more often than the other: an arm left answering beside the store's own doubles its rows,
// which a reading that only asked whether a row is there would miss.
func requireSameEdges(t *testing.T, projected, stored []graphEdgeRow) {
	t.Helper()
	left, right := edgeTexts(projected), edgeTexts(stored)
	for _, row := range left {
		if !slices.Contains(right, row) {
			t.Errorf("the view answers %q over the projections and not over the store", row)
		}
	}
	for _, row := range right {
		if !slices.Contains(left, row) {
			t.Errorf("the view answers %q over the store and not over the projections", row)
		}
	}
	leftCounts, rightCounts := edgeCounts(left), edgeCounts(right)
	for _, row := range slices.Compact(slices.Clone(left)) {
		if over, under := rightCounts[row], leftCounts[row]; over != 0 && over != under {
			t.Errorf("the view answers %q %d times over the store and %d times over the projections", row, over, under)
		}
	}
	if len(left) != len(right) {
		t.Errorf("the view answers %d rows over the projections and %d over the store", len(left), len(right))
	}
}

// edgeCounts is how many times a reading answers each row.
func edgeCounts(texts []string) map[string]int {
	counts := make(map[string]int, len(texts))
	for _, text := range texts {
		counts[text]++
	}
	return counts
}

func edgeTexts(rows []graphEdgeRow) []string {
	texts := make([]string, len(rows))
	for index, row := range rows {
		texts[index] = row.String()
	}
	slices.Sort(texts)
	return texts
}

func armRelations(arms []graphEdgesArm) []string {
	relations := make([]string, len(arms))
	for index, arm := range arms {
		relations[index] = arm.relation
	}
	return relations
}

// armReport is the arm list as the failure prints it: each arm by its place, the relation it
// reads and the fate the declaration gives it, or none for an arm the view has gained.
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

// graphEdgeRows is the whole view, every column of every row.
func graphEdgeRows(t *testing.T, database *store.Store) []graphEdgeRow {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select from_kind, from_id, kind, to_kind, to_id, created_at, source_seq from graph_edges
		order by from_kind, from_id, kind, to_kind, to_id, created_at, source_seq
	`)
	if err != nil {
		t.Fatalf("read graph_edges: %v", err)
	}
	read, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (graphEdgeRow, error) {
		var edge graphEdgeRow
		err := row.Scan(&edge.fromKind, &edge.fromID, &edge.kind, &edge.toKind, &edge.toID, &edge.createdAt, &edge.sourceSeq)
		return edge, err
	})
	if err != nil {
		t.Fatalf("scan graph_edges: %v", err)
	}
	return read
}

// graphEdgeBacklinkCounts counts the inbound edges of every node the view points at, kind by
// kind, as a list beside a row counts them. The addresses come from the reading taken over the
// projections and are the same both times, so a node that gains or loses an inbound edge at the
// cutover is counted on both sides rather than falling out of one.
func graphEdgeBacklinkCounts(t *testing.T, database *store.Store, rows []graphEdgeRow) map[string]map[string]int {
	t.Helper()
	targets := map[string][]string{}
	for _, row := range rows {
		if !slices.Contains(targets[row.toKind], row.toID) {
			targets[row.toKind] = append(targets[row.toKind], row.toID)
		}
	}
	counts := map[string]map[string]int{}
	for _, kind := range refs.BacklinkKinds() {
		ids := targets[kind]
		slices.Sort(ids)
		count, err := refs.BacklinkCounts(context.Background(), database.Pool, model.Actor{}, kind, ids, nil)
		if err != nil {
			t.Fatalf("count %s backlinks: %v", kind, err)
		}
		counts[kind] = count
	}
	return counts
}

// setGraphStoreCutover puts every kind's cutover row at complete, which is how the view is read
// from the projections and then from the store over one set of rows. The backfill is what moves
// these rows in a deployment; the test moves them back and forth to read both answers of one
// database.
func setGraphStoreCutover(t *testing.T, database *store.Store, complete bool) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(),
		`update graph_store_cutover set complete = $1`, complete); err != nil {
		t.Fatalf("set the cutover rows to %t: %v", complete, err)
	}
}

// The documents the fixture writes. target.md holds the two requirements a plan_task's serves
// may point at; its second version retypes one of them to a callout, which the attribute does
// not allow, so the link stored at it is listed by no arm. plan.md holds a pointer at each, a
// mention of CORE-2 from a block inside another block, a block a later version drops and a
// later one brings back, and a block a later version drops for good.
const (
	graphEdgeTarget = ":::requirement{#r1}\nThe view answers one row per edge.\n:::\n\n" +
		":::requirement{#r2}\nA requirement a later version retypes.\n:::\n\n" +
		"A paragraph of the target.\n"

	graphEdgeRetyped = ":::requirement{#r1}\nThe view answers one row per edge.\n:::\n\n" +
		":::callout{#r2 kind=\"note\" title=\"\"}\nA requirement a later version retypes.\n:::\n\n" +
		"A paragraph of the target.\n"

	graphEdgePlanPointers = ":::plan_task{#t1 serves=\"dispatch://CORE-1/artifact/target#b-r1\"}\nServe the requirement.\n:::\n\n" +
		":::plan_task{#t2 serves=\"dispatch://CORE-1/artifact/target#b-r2\"}\nServe the one that is retyped.\n:::\n\n" +
		"::::callout{#outer kind=\"note\" title=\"\"}\nOuter.\n\n" +
		":::callout{#inner kind=\"note\" title=\"\"}\nInner, after dispatch://CORE-2.\n:::\n\n::::\n"

	graphEdgePlanWhole = graphEdgePlanPointers + "\n" +
		":::callout{#revive kind=\"note\" title=\"\"}\nA block a version drops and another brings back, citing dispatch://CORE-2.\n:::\n\n" +
		":::callout{#dropped kind=\"note\" title=\"\"}\nA block a version drops for good, citing dispatch://CORE-2.\n:::\n"

	graphEdgePlanRestored = graphEdgePlanPointers + "\n" +
		":::callout{#revive kind=\"note\" title=\"\"}\nA block a version drops and another brings back, citing dispatch://CORE-2.\n:::\n"
)

// seedGraphEdgeFixture writes what the four moved arms carry, through the paths that write it in
// a deployment: two documents indexed by the block-index tick, one of them at three versions so
// that a block is dropped and brought back and so that a version's edges carry the source_seq of
// the event that recorded it, and three comments written as a build before the store existed
// wrote them, which the backfill states.
func seedGraphEdgeFixture(t *testing.T, database *store.Store, service *Service) {
	t.Helper()
	ctx := context.Background()
	// The target is indexed before the document that points at it: a write states a pointer
	// only at a block the store holds and only at a type the attribute allows, which is why the
	// link at a type it does not allow is made by retyping the target below.
	target := createOwedDocument(t, database, service, "", "target", graphEdgeTarget, "")
	settle(t, database, target)
	if ticks := drainBlockIndex(t, service, "the target document"); ticks == 0 {
		t.Fatal("the fixture indexed no document")
	}
	plan := createOwedDocument(t, database, service, "", "plan", graphEdgePlanWhole, "")
	settle(t, database, plan)
	if ticks := drainBlockIndex(t, service, "the document that points at it"); ticks == 0 {
		t.Fatal("the fixture indexed no document holding a pointer")
	}

	// The retype, which no pass over plan.md follows: its stored link at r2 stands and is
	// listed by neither arm while r2 is a callout.
	writeVersionAsAnOlderBuild(t, database, service, target, "target.md", graphEdgeRetyped)
	if ticks := drainBlockIndex(t, service, "the version that retypes r2"); ticks == 0 {
		t.Fatal("the version that retypes r2 was not indexed")
	}

	// Version 2 of plan.md drops two blocks: the mentions they held are retracted with their
	// rows, and their nodes are stated gone while their in_document tuples stand, which is the
	// rule the tuple arm carries. Version 3 brings one of them back under its own id.
	writeVersionAsAnOlderBuild(t, database, service, plan, "plan.md", graphEdgePlanPointers)
	if ticks := drainBlockIndex(t, service, "the version that drops two blocks"); ticks == 0 {
		t.Fatal("the version that drops two blocks was not indexed")
	}
	writeVersionAsAnOlderBuild(t, database, service, plan, "plan.md", graphEdgePlanRestored)
	if ticks := drainBlockIndex(t, service, "the version that brings one back"); ticks == 0 {
		t.Fatal("the version that brings one back was not indexed")
	}

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
	if _, err := database.Pool.Exec(ctx, `
		insert into comments (issue_key, author, body, anchor)
		values ('CORE-1', '{"kind":"user","id":"alice"}', 'anchored on the plan', jsonb_build_object('artifact_id', $1::text))
	`, plan); err != nil {
		t.Fatalf("write the anchored comment: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into comments (artifact_id, author, body, reply_to)
		values ($1::uuid, '{"kind":"user","id":"alice"}', 'a reply', $2::uuid)
	`, plan, owner); err != nil {
		t.Fatalf("write the reply: %v", err)
	}
}

// writeVersionAsAnOlderBuild replaces the document's live text with markdown and writes the
// version a build without the block index wrote: the version row, the artifact.version event
// that recorded it, and the settlement cleared. The trigger of migration 0122 leaves the
// document owed a block index, and the tick indexes it, which is the one path in this package
// that states a version's blocks and links in the store.
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
		insert into events (issue_key, seq, type, actor, payload, notify)
		select 'CORE-1', coalesce(max(seq), 0) + 1, 'artifact.version', '{"kind":"user","id":"alice"}',
		       jsonb_build_object('artifact_id', $1::text, 'name', $3::text, 'version', jsonb_build_object('number', $2::int)), false
		from events where issue_key = 'CORE-1'
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

package api

import (
	"context"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// placementSpec is the seeded issue's spec, which must be CORE-1's: two criteria, three
// requirements under them, each written under the criterion its serves names first, and a fourth
// requirement a retarget points at.
const placementSpec = ":::criterion{#k1}\nI want the urgent asks first.\n:::\n\n" +
	":::criterion{#k2}\nI want to know how big each step is.\n:::\n\n" +
	":::requirement{#r1 serves=\"dispatch://CORE-1/spec#b-k1\"}\nAn urgent ask comes first.\n:::\n\n" +
	":::requirement{#r2 serves=\"dispatch://CORE-1/spec#b-k1\"}\nAn ask's urgency is its own.\n:::\n\n" +
	":::requirement{#r3 serves=\"dispatch://CORE-1/spec#b-k2\"}\nA plan node says its size.\n:::\n\n" +
	":::requirement{#r4 serves=\"dispatch://CORE-1/spec#b-k2\"}\nA size is in days.\n:::\n"

// servingNode is a document holding one node of type typ, id, whose serves names targets in
// that written order.
func servingNode(typ, id string, targets ...string) string {
	return ":::" + typ + "{#" + id + " serves=\"" + strings.Join(targets, " ") + "\"}\nIt proves what it serves.\n:::\n"
}

// servingNodeUnder is servingNode as a document reads once a move put its node under parent: its
// parent attribute names parent, the dispatch:// address of a block, beside its serves pointer.
func servingNodeUnder(typ, id, parent string, targets ...string) string {
	return ":::" + typ + "{#" + id + " serves=\"" + strings.Join(targets, " ") + "\" parent=\"" + parent + "\"}\nIt proves what it serves.\n:::\n"
}

// splitPlacementTuples is tuples' serves tuples and their part_of tuples, each in order, and no
// other.
func splitPlacementTuples(tuples []graphstore.Tuple) (serves, partOf []graphstore.Tuple) {
	for _, tuple := range tuples {
		switch tuple.Kind {
		case servesTupleKind:
			serves = append(serves, tuple)
		case graphstore.TuplePartOf:
			partOf = append(partOf, tuple)
		}
	}
	return serves, partOf
}

// requirementsServed is how many nodes of type typ - checks, or plan tasks - the coverage read of
// CORE-1 (over the tuples) lists under each of the requirements r1 to r4.
func requirementsServed(t *testing.T, handler http.Handler, typ string) map[string]int {
	t.Helper()
	coverage := readCoverage(t, handler, "CORE-1")
	served := map[string]int{}
	for _, id := range []string{"r1", "r2", "r3", "r4"} {
		requirement := requirementOf(t, coverage, id)
		if typ == planTaskBlockType {
			served[id] = requirement.PlanTasks
		} else {
			served[id] = requirement.Checks
		}
	}
	return served
}

// requireServed fails unless the coverage read of CORE-1 lists, under each requirement r1 to r4, the
// number of nodes of type typ that base holds, and one more under each requirement of under: the
// node whose serves a test moved or retargeted, which the read lists under the requirements it
// serves whatever parent it stands under.
func requireServed(t *testing.T, handler http.Handler, what, typ string, base map[string]int, under ...string) {
	t.Helper()
	for id, count := range requirementsServed(t, handler, typ) {
		want := base[id]
		if slices.Contains(under, id) {
			want++
		}
		if count != want {
			t.Errorf("%s: the coverage read lists %d %s node(s) under %s, want %d (%d before the node, one more where it serves)", what, count, typ, id, want, base[id])
		}
	}
}

// moveNodeUnder moves the node address of CORE-1 under parent as alice, through PATCH
// /api/v1/issues/{key}/nodes/{id} with the If-Match of the node's placement_seq, and fails unless
// the move is taken.
func moveNodeUnder(t *testing.T, handler http.Handler, database *store.Store, what, address, parent string) {
	t.Helper()
	_, tag := placementOf(t, database, refs.BlockNodeKind, address)
	if response := patchNode(t, handler, "CORE-1", address, tag, map[string]any{"parent": parent}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("move %s under %s: status=%d body=%s", what, parent, response.Code, response.Body.String())
	}
}

// placementSeq is the last sequence the tuple log holds.
func placementSeq(t *testing.T, database *store.Store) int64 {
	t.Helper()
	var seq int64
	if err := database.Pool.QueryRow(context.Background(), `select coalesce(max(seq), 0) from graph_tuples`).Scan(&seq); err != nil {
		t.Fatalf("read the end of the tuple log: %v", err)
	}
	return seq
}

// placementTuplesSince is every relation the log holds out of the block node address after seq,
// in log order: what the writes since then stated of it.
func placementTuplesSince(t *testing.T, database *store.Store, address string, seq int64) []graphstore.Tuple {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select kind, to_kind, to_id, asserted from graph_tuples
		where from_kind = $1 and from_id = $2 and seq > $3 and to_kind <> $4
		order by seq
	`, refs.BlockNodeKind, address, seq, graphstore.KindValue)
	if err != nil {
		t.Fatalf("read what was stated of %s: %v", address, err)
	}
	defer rows.Close()
	var stated []graphstore.Tuple
	for rows.Next() {
		var tuple graphstore.Tuple
		if err := rows.Scan(&tuple.Kind, &tuple.ToKind, &tuple.ToID, &tuple.Asserted); err != nil {
			t.Fatalf("scan what was stated of %s: %v", address, err)
		}
		stated = append(stated, tuple)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read what was stated of %s: %v", address, err)
	}
	return stated
}

// placementValue is the block node address's current value of attribute, "" where it holds none.
func placementValue(t *testing.T, database *store.Store, address, attribute string) string {
	t.Helper()
	value, _, err := graphstore.CurrentValue(context.Background(), database.Pool, refs.BlockNodeKind, address, attribute)
	if err != nil {
		t.Fatalf("read the %s of %s: %v", attribute, address, err)
	}
	return value
}

// valuesHeld is how many values of attribute the block node address holds now, one at most.
func valuesHeld(t *testing.T, database *store.Store, address, attribute string) int {
	t.Helper()
	if _, held, err := graphstore.CurrentValue(context.Background(), database.Pool, refs.BlockNodeKind, address, attribute); err != nil {
		t.Fatalf("read the %s of %s: %v", attribute, address, err)
	} else if held {
		return 1
	}
	return 0
}

// blockNodesNamed is how many block nodes the store holds whose id ends with `#id`, every
// document's alike: one for a node no write copied.
func blockNodesNamed(t *testing.T, database *store.Store, id string) int {
	t.Helper()
	return int(countRows(t, database, `select count(*) from graph_nodes where kind = $1 and id like '%#' || $2`, refs.BlockNodeKind, id))
}

// projectedParent is the document_blocks row's parent_block_id of the block node address, "" for
// null: the projection of the node's part_of, which holds a block of the same artifact alone.
func projectedParent(t *testing.T, database *store.Store, address string) string {
	t.Helper()
	var parent *string
	if err := database.Pool.QueryRow(context.Background(), `
		select parent_block_id from document_blocks where node_id = $1
	`, address).Scan(&parent); err != nil {
		t.Fatalf("read the row of %s: %v", address, err)
	}
	if parent == nil {
		return ""
	}
	return *parent
}

// tupleFarEnds is each tuple's far end, kind/id, in order.
func tupleFarEnds(tuples []graphstore.Tuple) []string {
	ends := make([]string, len(tuples))
	for index, tuple := range tuples {
		ends[index] = tuple.ToKind + "/" + tuple.ToID
	}
	return ends
}

// requirePlaced fails unless the block node address holds one current part_of, to the far end
// kind/id, its row projects parent ("" for none of its own artifact), and its placement says
// what decided it.
func requirePlaced(t *testing.T, database *store.Store, what, address, kind, id, parent, placement string) {
	t.Helper()
	if held := currentTies(t, database, address, graphstore.TuplePartOf); len(held) != 1 || held[0].ToKind != kind || held[0].ToID != id {
		t.Errorf("%s stands under %v, want exactly one part_of, to %s/%s", what, tupleFarEnds(held), kind, id)
	}
	if got := projectedParent(t, database, address); got != parent {
		t.Errorf("%s's row projects the parent %q, want %q", what, got, parent)
	}
	if got := placementValue(t, database, address, refs.AttributePlacement); got != placement {
		t.Errorf("%s's placement is %q, want %q", what, got, placement)
	}
}

// requireServes fails unless the block node address states exactly the serves tuples to the
// block nodes addresses, in their order.
func requireServes(t *testing.T, database *store.Store, what, address string, addresses ...string) {
	t.Helper()
	want := make([]string, len(addresses))
	for index, target := range addresses {
		want[index] = refs.BlockNodeKind + "/" + target
	}
	slices.Sort(want)
	if held := tupleFarEnds(currentTies(t, database, address, servesTupleKind)); !slices.Equal(held, want) {
		t.Errorf("%s serves %v, want %v", what, held, want)
	}
}

// requirementIDs is every requirement id a reference to the spec of issue names in markdown, in
// the order first named.
func requirementIDs(markdown, issue string) []string {
	var ids []string
	for _, match := range regexp.MustCompile(`dispatch://`+regexp.QuoteMeta(issue)+`/spec#b-([a-z0-9-]+)`).FindAllStringSubmatch(markdown, -1) {
		if !slices.Contains(ids, match[1]) {
			ids = append(ids, match[1])
		}
	}
	return ids
}

// requirementsSpec is a spec holding one requirement of each id.
func requirementsSpec(ids []string) string {
	var spec strings.Builder
	for _, id := range ids {
		spec.WriteString(":::requirement{#" + id + "}\nThe requirement " + id + ".\n:::\n\n")
	}
	return spec.String()
}

// c-placement-and-coverage, its placement half, with the move through its route and the coverage
// read over the tuples: placement and coverage are two relations. One check serves three
// requirements and keeps one place: one node, one id, one part_of to the first requirement of the
// pointer's written order that is a node of its own issue, one rank, and three serves tuples. The
// two change apart. A move through PATCH appends one part_of
// tuple and one retraction and writes no serves tuple: the node is stored under the requirement it
// moved to, still serves its three, and the coverage read still lists it under each of them. A
// retarget appends one serves tuple and one retraction and writes no part_of, whichever target it
// drops: a first retarget that keeps the node's parent among its targets leaves the node under the
// parent its move gave it, and a second that takes the parent out of them leaves it there too, so
// the node's parent is a requirement it no longer serves and it reads back with both facts stated,
// its part_of and its placement as the move wrote them and its serves as the retarget wrote them,
// with no read moving it back; each retarget's text names that parent, as a document reads once a
// move put it there, and the coverage read lists the node under what it serves alone. A plan node,
// which carries the same pointer, does the same. A check and a plan node of a child issue that
// serve the parent's requirement alone hang from the child issue, never under that requirement. A
// requirement moved from one criterion to another through PATCH changes its place alone, and the
// store census reads zero after it. And this issue's own Verification document, indexed into a
// child of a done parent, places c-tree-census under r-one-tree, the first of its six serves
// targets that is a node of its own issue.
func TestPlacementAndCoverageAreTwoRelations(t *testing.T) {
	handler, database := blockIndexHandler(t)
	createReferenceAPIProject(t, handler, "CORE")
	if issue := createSpecIssue(t, handler, "CORE", placementSpec); issue.Key != "CORE-1" {
		t.Fatalf("the seeded issue is %s, want CORE-1, which its spec's references name", issue.Key)
	}
	// Each issue is born converted: its role documents' blocks are its room's nodes,
	// `<tree ref_key>#<block>`, which a serves written at the spec's own address reaches.
	core := readDecisionDocument(t, database, "CORE-1", "spec")
	spec := core.address
	ref := func(id string) string { return "dispatch://CORE-1/spec#b-" + id }
	for _, requirement := range []struct{ id, criterion string }{{"r1", "k1"}, {"r2", "k1"}, {"r3", "k2"}, {"r4", "k2"}} {
		requirePlaced(t, database, "requirement "+requirement.id, spec(requirement.id),
			refs.BlockNodeKind, spec(requirement.criterion), requirement.criterion, refs.PlacementAuthored)
	}

	// The plan is a role document, a fragment of the spec's room, whose tree keys its blocks' nodes
	// and rows, so its row projects its parent requirement by its block id; checks.md plays no role,
	// so it is a room of its own, whose ref_key keys its blocks' nodes, and its row names no block id
	// of its parent (R5 of the issue-tree spec).
	for _, node := range []struct{ what, upload, typ, id, address, parent, movedParent string }{
		{"the check", "checks.md", "check", "c1", "CORE-1/checks-md#c1", "", ""},
		{"the plan node", "plan.md", "plan_task", "t1", core.address("t1"), "r1", "r3"},
	} {
		address := node.address
		base := requirementsServed(t, handler, node.typ)
		uploadDocument(t, handler, "/api/v1/issues/CORE-1", node.upload, servingNode(node.typ, node.id, ref("r1"), ref("r2"), ref("r3")))
		if nodes := blockNodesNamed(t, database, node.id); nodes != 1 {
			t.Errorf("%s serving three requirements is %d node(s), want one with one id", node.what, nodes)
		}
		requirePlaced(t, database, node.what, address, refs.BlockNodeKind, spec("r1"), node.parent, refs.PlacementAuthored)
		if ranks := valuesHeld(t, database, address, refs.AttributeRank); ranks != 1 {
			t.Errorf("%s holds %d rank(s), want one", node.what, ranks)
		}
		requireServes(t, database, node.what, address, spec("r1"), spec("r2"), spec("r3"))
		requireServed(t, handler, node.what, node.typ, base, "r1", "r2", "r3")

		// The move: PATCH puts the node under r3, a requirement it serves, which is not the first
		// target of its pointer once the pointer is retargeted below. One part_of is appended and
		// one retracted, no serves tuple is written, the node holds the placement the move stores,
		// and the coverage read, which reads serves alone, lists it under the same three.
		mark := placementSeq(t, database)
		moveNodeUnder(t, handler, database, node.what, address, spec("r3"))
		serves, partOf := splitPlacementTuples(placementTuplesSince(t, database, address, mark))
		if len(serves) != 0 {
			t.Errorf("the move of %s wrote the serves tuples %+v, want none: a move changes the place and leaves the coverage standing", node.what, serves)
		}
		appended := slices.IndexFunc(partOf, func(tuple graphstore.Tuple) bool { return tuple.Asserted && tuple.ToID == spec("r3") })
		retracted := slices.IndexFunc(partOf, func(tuple graphstore.Tuple) bool { return !tuple.Asserted && tuple.ToID == spec("r1") })
		if len(partOf) != 2 || appended < 0 || retracted < 0 {
			t.Errorf("the move of %s under r3 wrote the part_of tuples %+v, want one appended to r3 and one retraction of r1", node.what, partOf)
		}
		requirePlaced(t, database, node.what+" after its move", address, refs.BlockNodeKind, spec("r3"), node.movedParent, refs.PlacementStored)
		requireServes(t, database, node.what+" after its move", address, spec("r1"), spec("r2"), spec("r3"))
		requireServed(t, handler, node.what+" after its move", node.typ, base, "r1", "r2", "r3")

		// The retarget: serves moves from r1 to r4, and the part_of stays where the move put it. The
		// text is the document a client reads after the move and edits, so it names r3 as its parent,
		// as the move wrote it.
		mark = placementSeq(t, database)
		uploadDocument(t, handler, "/api/v1/issues/CORE-1", node.upload, servingNodeUnder(node.typ, node.id, ref("r3"), ref("r2"), ref("r3"), ref("r4")))
		serves, partOf = splitPlacementTuples(placementTuplesSince(t, database, address, mark))
		if len(partOf) != 0 {
			t.Errorf("the retarget of %s wrote the part_of tuples %+v, want none: a retarget moves no node", node.what, partOf)
		}
		appended = slices.IndexFunc(serves, func(tuple graphstore.Tuple) bool { return tuple.Asserted && tuple.ToID == spec("r4") })
		retracted = slices.IndexFunc(serves, func(tuple graphstore.Tuple) bool { return !tuple.Asserted && tuple.ToID == spec("r1") })
		if len(serves) != 2 || appended < 0 || retracted < 0 {
			t.Errorf("the retarget of %s wrote the serves tuples %+v, want one appended to r4 and one retraction of r1", node.what, serves)
		}
		// Both facts read back: the parent its move gave it, a requirement it still serves, and the
		// three it serves now. No read moves it back, and the coverage read drops r1 and gains r4.
		requirePlaced(t, database, node.what+" after its retarget", address, refs.BlockNodeKind, spec("r3"), node.movedParent, refs.PlacementStored)
		requireServes(t, database, node.what+" after its retarget", address, spec("r2"), spec("r3"), spec("r4"))
		requireServed(t, handler, node.what+" after its retarget", node.typ, base, "r2", "r3", "r4")

		// The retarget off the parent: serves moves from r3, the node's parent, to r1, so the node
		// serves r1, r2 and r4 and stands under r3, a requirement it no longer serves. The text still
		// names r3 as its parent. One serves tuple is appended and one retracted, no part_of is
		// written, the node keeps the part_of and the placement its move wrote, and the coverage read
		// lists it under r1, r2 and r4 and not under r3, its parent.
		mark = placementSeq(t, database)
		uploadDocument(t, handler, "/api/v1/issues/CORE-1", node.upload, servingNodeUnder(node.typ, node.id, ref("r3"), ref("r1"), ref("r2"), ref("r4")))
		serves, partOf = splitPlacementTuples(placementTuplesSince(t, database, address, mark))
		if len(partOf) != 0 {
			t.Errorf("the retarget of %s off its parent r3 wrote the part_of tuples %+v, want none: a retarget moves no node, whatever target it drops", node.what, partOf)
		}
		appended = slices.IndexFunc(serves, func(tuple graphstore.Tuple) bool { return tuple.Asserted && tuple.ToID == spec("r1") })
		retracted = slices.IndexFunc(serves, func(tuple graphstore.Tuple) bool { return !tuple.Asserted && tuple.ToID == spec("r3") })
		if len(serves) != 2 || appended < 0 || retracted < 0 {
			t.Errorf("the retarget of %s off its parent r3 wrote the serves tuples %+v, want one appended to r1 and one retraction of r3", node.what, serves)
		}
		// Both facts read back: the parent r3, a requirement the node no longer serves, and the three
		// it serves now. No read moves it back under one of them.
		requirePlaced(t, database, node.what+" after its retarget off its parent", address, refs.BlockNodeKind, spec("r3"), node.movedParent, refs.PlacementStored)
		requireServes(t, database, node.what+" after its retarget off its parent", address, spec("r1"), spec("r2"), spec("r4"))
		requireServed(t, handler, node.what+" after its retarget off its parent", node.typ, base, "r1", "r2", "r4")
	}

	// A requirement moves from one criterion to another through PATCH, in the one room of CORE-1: r4
	// leaves k2 for k1. Its place changes and nothing it serves or is served by: its serves still
	// names k2, the coverage read lists the same checks and plan tasks under it, and the store
	// census, which reads every kind over the whole store, reads zero.
	before := requirementOf(t, readCoverage(t, handler, "CORE-1"), "r4")
	moveNodeUnder(t, handler, database, "requirement r4", spec("r4"), spec("k1"))
	requirePlaced(t, database, "requirement r4 after its move", spec("r4"), refs.BlockNodeKind, spec("k1"), "k1", refs.PlacementStored)
	requireServes(t, database, "requirement r4 after its move", spec("r4"), spec("k2"))
	if after := requirementOf(t, readCoverage(t, handler, "CORE-1"), "r4"); after.Checks != before.Checks || after.PlanTasks != before.PlanTasks {
		t.Errorf("the move of r4 left the coverage read listing %d check(s) and %d plan task(s) under it, want the %d and %d it listed before: a move changes no coverage",
			after.Checks, after.PlanTasks, before.Checks, before.PlanTasks)
	}
	requireStoreCensusZero(t, database, "after the move of the requirement r4 from the criterion k2 to k1")

	// A child's check and plan node serving the parent's first requirement alone serve no
	// requirement of their own issue, so each hangs from the child issue.
	child := createChildIssue(t, handler, "CORE", "CORE-1", "The child's spec.\n")
	childRoom := readDecisionDocument(t, database, child.Key, "spec")
	for _, node := range []struct{ what, upload, typ, id, address string }{
		{"the child's check", "checks.md", "check", "cc1", child.Key + "/checks-md#cc1"},
		{"the child's plan node", "plan.md", "plan_task", "ct1", childRoom.address("ct1")},
	} {
		address := node.address
		uploadDocument(t, handler, "/api/v1/issues/"+child.Key, node.upload, servingNode(node.typ, node.id, ref("r1")))
		requirePlaced(t, database, node.what, address, graphstore.KindIssue, child.Key, "", refs.PlacementAuthored)
		requireServes(t, database, node.what, address, spec("r1"))
		history, err := graphstore.History(context.Background(), database.Pool, refs.BlockNodeKind, address, graphstore.TuplePartOf)
		if err != nil {
			t.Fatalf("read the part_of history of %s: %v", node.what, err)
		}
		for _, tuple := range history {
			if tuple.ToID == spec("r1") {
				t.Errorf("%s was placed under the parent's requirement %s: %+v", node.what, spec("r1"), tuple)
			}
		}
	}

	// This document itself, its references to the graph-model root spec and to the issue-tree spec
	// rewritten to TREE-1 and TREE-2, indexed into a child of a parent closed as done, each holding
	// the requirements the document's serves name of it (a child under a done ancestor holds
	// requirements of its own). The parent is closed before the child is created: a child born
	// converted under a parent whose requirements stand is a view, and its create with a
	// requirement is refused.
	verification, err := os.ReadFile("testdata/infra-1253-verification.md")
	if err != nil {
		t.Fatalf("read the Verification document's copy: %v", err)
	}
	createReferenceAPIProject(t, handler, "TREE")
	if parent := createSpecIssue(t, handler, "TREE", requirementsSpec(requirementIDs(string(verification), "TREE-1"))); parent.Key != "TREE-1" {
		t.Fatalf("the done parent is %s, want TREE-1, which the copy's references name", parent.Key)
	}
	closeIssue(t, handler, "TREE-1")
	if treeChild := createChildIssue(t, handler, "TREE", "TREE-1", requirementsSpec(requirementIDs(string(verification), "TREE-2"))); treeChild.Key != "TREE-2" {
		t.Fatalf("the child is %s, want TREE-2, which the copy's references name", treeChild.Key)
	}
	uploaded := uploadDocument(t, handler, "/api/v1/issues/TREE-2", "verification.md", string(verification))
	if uploaded.Version.Number != 1 {
		t.Fatalf("the copy's upload stored version %d, want 1", uploaded.Version.Number)
	}
	treeOne, treeTwo := readDecisionDocument(t, database, "TREE-1", "spec"), readDecisionDocument(t, database, "TREE-2", "spec")
	census := treeTwo.address("c-tree-census")
	if nodes := blockNodesNamed(t, database, "c-tree-census"); nodes != 1 {
		t.Errorf("c-tree-census is %d node(s), want one with one id", nodes)
	}
	requirePlaced(t, database, "c-tree-census", census, refs.BlockNodeKind, treeTwo.address("r-one-tree"), "r-one-tree", refs.PlacementAuthored)
	requireServes(t, database, "c-tree-census", census, treeTwo.address("r-one-tree"), treeTwo.address("r-queries"),
		treeTwo.address("r-artifacts-in-tree"), treeTwo.address("r-result-node"), treeTwo.address("r-issue-relations"), treeOne.address("r-document-tree"))
}

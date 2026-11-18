package api

import (
	"context"
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
	var count int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from graph_nodes where kind = $1 and id like '%#' || $2
	`, refs.BlockNodeKind, id).Scan(&count); err != nil {
		t.Fatalf("count the nodes of %s: %v", id, err)
	}
	return count
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
	if held := tupleFarEnds(currentTies(t, database, address, "serves")); !slices.Equal(held, want) {
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

// c-placement-and-coverage, its placement half (T7 of dispatch://INFRA-1253's plan; T18 adds the
// move through its route and the coverage reads through T16a's): placement and coverage are two
// relations. One check serves three requirements and keeps one place: one node, one id, one
// part_of to the first requirement of the pointer's written order that is a node of its own issue,
// one rank, and three serves tuples. A retarget appends one serves tuple and one retraction and
// writes no part_of: the check stays under the parent its first write gave it, a requirement it
// no longer serves, and reads back with both facts stated. A plan node, which carries the same
// pointer, does the same. A check and a plan node of a child issue that serve the parent's
// requirement alone hang from the child issue, never under that requirement. And this issue's own
// Verification document, indexed into a child of a done parent, places c-tree-census under
// r-one-tree, the first of its six serves targets that is a node of its own issue.
func TestPlacementAndCoverageAreTwoRelations(t *testing.T) {
	handler, database := blockIndexHandler(t)
	createReferenceAPIProject(t, handler, "CORE")
	if issue := createSpecIssue(t, handler, "CORE", placementSpec); issue.Key != "CORE-1" {
		t.Fatalf("the seeded issue is %s, want CORE-1, which its spec's references name", issue.Key)
	}
	spec := func(id string) string { return "CORE-1/spec#" + id }
	ref := func(id string) string { return "dispatch://CORE-1/spec#b-" + id }
	for _, requirement := range []struct{ id, criterion string }{{"r1", "k1"}, {"r2", "k1"}, {"r3", "k2"}, {"r4", "k2"}} {
		requirePlaced(t, database, "requirement "+requirement.id, spec(requirement.id),
			refs.BlockNodeKind, spec(requirement.criterion), requirement.criterion, refs.PlacementAuthored)
	}

	for _, node := range []struct{ what, upload, typ, id, slug string }{
		{"the check", "checks.md", "check", "c1", "checks-md"},
		{"the plan node", "plan.md", "plan_task", "t1", "plan-md"},
	} {
		address := "CORE-1/" + node.slug + "#" + node.id
		uploadDocument(t, handler, "/api/v1/issues/CORE-1", node.upload, servingNode(node.typ, node.id, ref("r1"), ref("r2"), ref("r3")))
		if nodes := blockNodesNamed(t, database, node.id); nodes != 1 {
			t.Errorf("%s serving three requirements is %d node(s), want one with one id", node.what, nodes)
		}
		requirePlaced(t, database, node.what, address, refs.BlockNodeKind, spec("r1"), "", refs.PlacementAuthored)
		if ranks := valuesHeld(t, database, address, refs.AttributeRank); ranks != 1 {
			t.Errorf("%s holds %d rank(s), want one", node.what, ranks)
		}
		requireServes(t, database, node.what, address, spec("r1"), spec("r2"), spec("r3"))

		// The retarget: serves moves from r1 to r4, and the part_of stays where it is.
		mark := placementSeq(t, database)
		uploadDocument(t, handler, "/api/v1/issues/CORE-1", node.upload, servingNode(node.typ, node.id, ref("r2"), ref("r3"), ref("r4")))
		var serves, partOf []graphstore.Tuple
		for _, tuple := range placementTuplesSince(t, database, address, mark) {
			switch tuple.Kind {
			case "serves":
				serves = append(serves, tuple)
			case graphstore.TuplePartOf:
				partOf = append(partOf, tuple)
			}
		}
		if len(partOf) != 0 {
			t.Errorf("the retarget of %s wrote the part_of tuples %+v, want none: a retarget moves no node", node.what, partOf)
		}
		appended := slices.IndexFunc(serves, func(tuple graphstore.Tuple) bool { return tuple.Asserted && tuple.ToID == spec("r4") })
		retracted := slices.IndexFunc(serves, func(tuple graphstore.Tuple) bool { return !tuple.Asserted && tuple.ToID == spec("r1") })
		if len(serves) != 2 || appended < 0 || retracted < 0 {
			t.Errorf("the retarget of %s wrote the serves tuples %+v, want one appended to r4 and one retraction of r1", node.what, serves)
		}
		// Both facts read back: the parent it was first given, a requirement it no longer serves,
		// and the three it serves now. No read moves it back.
		requirePlaced(t, database, node.what+" after its retarget", address, refs.BlockNodeKind, spec("r1"), "", refs.PlacementAuthored)
		requireServes(t, database, node.what+" after its retarget", address, spec("r2"), spec("r3"), spec("r4"))
	}

	// A child's check and plan node serving the parent's first requirement alone serve no
	// requirement of their own issue, so each hangs from the child issue.
	child := createChildIssue(t, handler, "CORE", "CORE-1", "The child's spec.\n")
	for _, node := range []struct{ what, upload, typ, id, slug string }{
		{"the child's check", "checks.md", "check", "cc1", "checks-md"},
		{"the child's plan node", "plan.md", "plan_task", "ct1", "plan-md"},
	} {
		address := child.Key + "/" + node.slug + "#" + node.id
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

	// This document itself, its INFRA-1098 and INFRA-1253 references rewritten to TREE-1 and
	// TREE-2, indexed into a child of a parent closed as done, each holding the requirements the
	// document's serves name of it (a child under a done ancestor holds requirements of its own).
	verification, err := os.ReadFile("testdata/infra-1253-verification.md")
	if err != nil {
		t.Fatalf("read the Verification document's copy: %v", err)
	}
	createReferenceAPIProject(t, handler, "TREE")
	if parent := createSpecIssue(t, handler, "TREE", requirementsSpec(requirementIDs(string(verification), "TREE-1"))); parent.Key != "TREE-1" {
		t.Fatalf("the done parent is %s, want TREE-1, which the copy's references name", parent.Key)
	}
	if treeChild := createChildIssue(t, handler, "TREE", "TREE-1", requirementsSpec(requirementIDs(string(verification), "TREE-2"))); treeChild.Key != "TREE-2" {
		t.Fatalf("the child is %s, want TREE-2, which the copy's references name", treeChild.Key)
	}
	closeIssue(t, handler, "TREE-1")
	uploaded := uploadDocument(t, handler, "/api/v1/issues/TREE-2", "verification.md", string(verification))
	if uploaded.Version.Number != 1 {
		t.Fatalf("the copy's upload stored version %d, want 1", uploaded.Version.Number)
	}
	census := "TREE-2/verification-md#c-tree-census"
	if nodes := blockNodesNamed(t, database, "c-tree-census"); nodes != 1 {
		t.Errorf("c-tree-census is %d node(s), want one with one id", nodes)
	}
	requirePlaced(t, database, "c-tree-census", census, refs.BlockNodeKind, "TREE-2/spec#r-one-tree", "", refs.PlacementAuthored)
	requireServes(t, database, "c-tree-census", census, "TREE-2/spec#r-one-tree", "TREE-2/spec#r-queries",
		"TREE-2/spec#r-artifacts-in-tree", "TREE-2/spec#r-result-node", "TREE-2/spec#r-issue-relations", "TREE-1/spec#r-document-tree")
}

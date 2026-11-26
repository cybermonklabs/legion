package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// contextPageNodes is the most nodes a page of
// TestAKindFilterPutsEachContextNodeOnTheFirstPageThatNamesIt answers (testServerOptions'
// issueTreePageSize).
const contextPageNodes = 3

// contextPageSpec gives the issue four ranked requirements and alice's decision.
const contextPageSpec = ":::requirement{#r1}\nEach page holds at most three nodes.\n:::\n\n" +
	":::requirement{#r2}\nEach page but the last answers a cursor.\n:::\n\n" +
	":::requirement{#r3}\nThe last page answers no cursor.\n:::\n\n" +
	":::requirement{#r4}\nEach node stands on one page.\n:::\n\n" +
	":::decision{#d1}\nThe tree reads three nodes a page.\n:::\n"

// contextPageTies are the tuples a context node stands by in the paged tree read (R8).
var contextPageTies = []string{graphstore.TupleHasKind, graphstore.TupleConstrainedBy, graphstore.TupleChangedByRun, graphstore.TupleDecidedBy}

// treePages is every page of the tree read of key with query as login, each read from the cursor
// the page before answered beside the same query, and the bytes of each page's body; it fails
// past twenty pages.
func treePages(t *testing.T, handler http.Handler, key, query, login string) ([]treeAnswer, []int) {
	t.Helper()
	var pages []treeAnswer
	var sizes []int
	target := query
	for page := 1; page <= 20; page++ {
		response := treeResponse(t, handler, key, target, login, false)
		if response.Code != http.StatusOK {
			t.Fatalf("page %d of the tree of %s (%s): status=%d body=%s", page, key, target, response.Code, response.Body.String())
		}
		read := treeAnswer{IssueTreeRead: decodeBody[model.IssueTreeRead](t, response), body: response.Body.String()}
		pages, sizes = append(pages, read), append(sizes, len(strings.TrimSuffix(read.body, "\n")))
		if read.Cursor == nil {
			return pages, sizes
		}
		separator := "?"
		if query != "" {
			separator = "&"
		}
		target = query + separator + "cursor=" + url.QueryEscape(*read.Cursor)
	}
	t.Fatalf("the tree of %s%s still answers a cursor after 20 pages", key, query)
	return nil, nil
}

// contextPagePlaces is where the pages of one tree read stand each node they answer, by its
// address (kindFilterAddress): on is the page that holds it; tie is the first page that holds a
// tuple of contextPageTies naming it, and tieKind that tuple's kind, where a page holds one; and
// walk is the page its walk place falls on, the last page whose first node stands at or before it
// in the same read answered as one page.
type contextPagePlaces struct {
	nodes         map[string]model.IssueTreeNode
	on, tie, walk map[string]int
	tieKind       map[string]string
}

// placeContextPages holds pages, the read label names, to R8's rule of the page, against whole,
// the same read answered as one page, and answers where the pages stand each node. No page holds
// more than contextPageNodes nodes or issueTreePageBytes bytes; each page after the first starts at
// the node the cursor of the page before names, and each page that answers a cursor answers in
// remaining the nodes the pages after it hold; the pages together hold each node whole holds, and
// each once. A context node that a tuple of contextPageTies names stands on the first page that
// holds such a tuple, and every other node on the page its walk place falls on.
func placeContextPages(t *testing.T, label string, pages []treeAnswer, sizes []int, whole treeAnswer) contextPagePlaces {
	t.Helper()
	if len(pages) < 2 || whole.Cursor != nil {
		t.Fatalf("%s reads as %d pages of at most %d nodes, and answers a cursor %t at the server's own page size: want several pages and one, so this case proves nothing",
			label, len(pages), contextPageNodes, whole.Cursor != nil)
	}
	places := contextPagePlaces{nodes: map[string]model.IssueTreeNode{}, on: map[string]int{}, tie: map[string]int{}, walk: map[string]int{}, tieKind: map[string]string{}}
	for index, page := range pages {
		number := index + 1
		if len(page.Nodes) == 0 || len(page.Nodes) > contextPageNodes {
			t.Fatalf("page %d of %s answers %d nodes, want one to %d: %v", number, label, len(page.Nodes), contextPageNodes, page.addresses())
		}
		if sizes[index] > issueTreePageBytes {
			t.Errorf("page %d of %s is %d bytes, want at most %d", number, label, sizes[index], issueTreePageBytes)
		}
		if first := page.Nodes[0]; index > 0 && *pages[index-1].Cursor != treeCursor(first.Kind, first.ID) {
			t.Errorf("page %d of %s starts at %s:%s, want the node the cursor of page %d names", number, label, first.Kind, first.ID, index)
		}
		after := 0
		for _, later := range pages[number:] {
			after += len(later.Nodes)
		}
		switch {
		case page.Cursor == nil && page.Remaining != nil:
			t.Errorf("page %d of %s, the last, answers remaining %d", number, label, *page.Remaining)
		case page.Cursor != nil && page.Remaining == nil:
			t.Errorf("page %d of %s answers a cursor and no remaining", number, label)
		case page.Cursor != nil && *page.Remaining != after:
			t.Errorf("page %d of %s answers remaining %d, want %d, the nodes the pages after it answer", number, label, *page.Remaining, after)
		}
		for _, node := range page.Nodes {
			address := kindFilterAddress(node.Kind, node.ID)
			if earlier, twice := places.on[address]; twice {
				t.Errorf("page %d of %s answers %s, which page %d answered", number, label, address, earlier)
				continue
			}
			places.nodes[address], places.on[address] = node, number
		}
		for _, tuple := range page.Tuples {
			address := kindFilterAddress(tuple.To.Kind, tuple.To.ID)
			if _, placed := places.tie[address]; !placed && slices.Contains(contextPageTies, tuple.Kind) {
				if len(page.nodesNamed(tuple.From.Kind, tuple.From.ID)) != 1 {
					t.Errorf("page %d of %s holds %s naming %s but does not hold its source %s:%s", number, label, tuple.Kind, address, tuple.From.Kind, tuple.From.ID)
				}
				places.tie[address], places.tieKind[address] = number, tuple.Kind
			}
		}
	}
	position := make(map[string]int, len(whole.Nodes))
	for index, node := range whole.Nodes {
		address := kindFilterAddress(node.Kind, node.ID)
		position[address] = index
		if _, held := places.on[address]; !held {
			t.Errorf("%s answers %s as one page, and on none of its %d pages", label, address, len(pages))
		}
	}
	for address, number := range places.on {
		at, held := position[address]
		if !held {
			t.Errorf("page %d of %s answers %s, which the same read answers on no page of one", number, label, address)
			continue
		}
		for index, page := range pages {
			if first := page.Nodes[0]; position[kindFilterAddress(first.Kind, first.ID)] <= at {
				places.walk[address] = index + 1
			}
		}
	}
	for address, node := range places.nodes {
		on, tie, walk := places.on[address], places.tie[address], places.walk[address]
		switch named := node.Role == treeRoleContext && tie > 0; {
		case named && on != tie:
			t.Errorf("%s: the %s %s stands on page %d, and the first page that holds its %s tuple is page %d; its walk place falls on page %d",
				label, node.Kind, node.ID, on, places.tieKind[address], tie, walk)
		case !named && on != walk:
			t.Errorf("%s: the %s %s stands on page %d, and its walk place falls on page %d; the first page that holds a tuple naming it is page %d",
				label, node.Kind, node.ID, on, walk, tie)
		}
	}
	previous := -1
	for _, page := range pages {
		for _, address := range page.addresses() {
			if places.nodes[address].Role == treeRoleContext && places.tie[address] > 0 {
				continue
			}
			if at := position[address]; at <= previous {
				t.Errorf("%s answers %s out of walk order", label, address)
			} else {
				previous = at
			}
		}
	}
	return places
}

// TestAKindFilterPutsEachContextNodeOnTheFirstPageThatNamesIt holds R8's rule of the page a node
// stands on (c-kind-filter), on a server whose pages hold at most contextPageNodes nodes. alice
// creates an issue born with the requirements r1 to r4 and her decision d1, whose decided_by names
// her; invokes a pre-mortem lens on r1, whose has_kind names its lens kind, with a finding and a
// risk; writes a comment for her run and two more comments; and writes a constraint of the issue
// that both r1 and r4 name by constrained_by.
//
// The test follows the cursor to the end with no filter, with the conversation's kinds, with
// kind=requirement, with kind=requirement&kind=constraint, and in the spec view. Each node
// appears exactly once. Reads that answer the constraint as context put it on r1's page. The
// other reads keep it at its walk place on a later page.
func TestAKindFilterPutsEachContextNodeOnTheFirstPageThatNamesIt(t *testing.T) {
	handler, database, deps, _ := newRegisteredTestServer(t, testServerOptions{issueTreePageSize: contextPageNodes})
	whole, _, _, _ := newRegisteredTestServer(t, testServerOptions{store: database})
	ctx := context.Background()
	createReferenceAPIProject(t, handler, "CPAGE")
	key := createDigestIssue(t, handler, database, "CPAGE", "An issue of several pages", contextPageSpec, false).Key
	spec := readDecisionDocument(t, database, key, "spec")
	r1 := spec.address("r1")
	decider, err := graphstore.CurrentOut(ctx, database.Pool, refs.BlockNodeKind, spec.address("d1"), []string{graphstore.TupleDecidedBy})
	if err != nil || len(decider) != 1 || decider[0].ToKind != graphstore.KindPerson {
		t.Fatalf("the decision d1 of %s is decided by %+v (%v), want one person: this case proves nothing", key, decider, err)
	}

	rig := reasoningRig{t: t, handler: handler, database: database, issue: key}
	lens := rig.invoke(refs.BlockRef("dispatch://"+key+"/spec", "r1"), graphstore.LensKindPreMortem, "alice", nil)
	finding := decodeBody[lensFinding](t, created(t, "the finding", rig.post(nodePath("lens/"+lens.ID, "findings"), map[string]any{"text": "A page holds four nodes."}, "alice")))
	risk := decodeBody[riskWritten](t, created(t, "the risk", rig.post("/risks", map[string]any{
		"lens": lens.Address, "heading": "A context node stands on two pages.", "early_warning": "A page repeats a node.", "prevention": "Place each context node once.",
	}, "alice")))

	run, err := directServer(deps).recordGoingRequest(ctx, model.Actor{Kind: "user", ID: "alice"}, "", "Which page holds the run?")
	if err != nil {
		t.Fatalf("record alice's run: %v", err)
	}
	runWindowAhead(t, database, run.ID)
	written := requestForRun(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/comments", map[string]any{"body": "The run wrote this comment."}, "alice", run.ID)
	if written.Code != http.StatusCreated {
		t.Fatalf("write the comment for the run: status=%d body=%s", written.Code, written.Body.String())
	}
	comment := decodeBody[writtenRecord](t, written).ID
	if count := changedByRunCount(t, database, graphstore.KindComment, comment, run.ID); count != 1 {
		t.Fatalf("the comment holds %d changed_by_run tuples to the run, want one: this case proves nothing", count)
	}
	for index := range 2 {
		created(t, fmt.Sprintf("comment %d", index+2), dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/comments",
			map[string]any{"body": fmt.Sprintf("Comment %d, written for no run.", index+2)}, "alice"))
	}

	constraint := decodeBody[constraintRead](t, created(t, "the constraint", rig.post("/constraints", map[string]any{"standard": "RFC 9110", "reference": "section 15.3.1"}, "alice")))
	r4 := spec.address("r4")
	for _, requirement := range []string{r1, r4} {
		if pointed := rig.post(nodePath(requirement, "pointers"), map[string]any{"kind": graphstore.TupleConstrainedBy, "action": pointerAppend, "to": constraint.Address}, "alice"); pointed.Code != http.StatusOK {
			t.Fatalf("point %s at the constraint: status=%d body=%s", requirement, pointed.Code, pointed.Body.String())
		}
	}
	constrained := treeEnd(graphstore.KindConstraint, constraint.ID)
	carried := []model.IssueTreeTuple{
		{Kind: graphstore.TupleHasKind, From: treeEnd(graphstore.KindLens, lens.ID), To: treeEnd(graphstore.KindLensKind, graphstore.LensKindPreMortem)},
		{Kind: graphstore.TupleChangedByRun, From: treeEnd(graphstore.KindComment, comment), To: treeEnd(graphstore.KindInteraction, run.ID)},
		{Kind: graphstore.TupleDecidedBy, From: treeEnd(refs.BlockNodeKind, spec.address("d1")), To: treeEnd(decider[0].ToKind, decider[0].ToID)},
	}

	for _, read := range []struct {
		name, query string
		// selected is whether the read answers the constraint as a node of the tree; context is
		// each other context node a tuple names that it answers.
		selected bool
		context  []model.IssueTreeTuple
	}{
		{"the read with no view", "", true, carried},
		{"the read with the conversation's kinds", kindFilterQuery(kindFilterConversation...), false, carried},
		{"kind=requirement", kindFilterQuery(requirementBlockType), false, nil},
		{"kind=requirement&kind=constraint", kindFilterQuery(requirementBlockType, graphstore.KindConstraint), true, nil},
		{"the spec view", "?view=spec", false, nil},
	} {
		label := read.name + " of " + key
		pages, sizes := treePages(t, handler, key, read.query, "alice")
		places := placeContextPages(t, label, pages, sizes, treeRead(t, whole, key, read.query, "alice", false))

		address := kindFilterAddress(constrained.Kind, constrained.ID)
		node, held := places.nodes[address]
		tied := slices.IndexFunc(pages, func(page treeAnswer) bool {
			return page.holdsTuple(graphstore.TupleConstrainedBy, treeEnd(refs.BlockNodeKind, r1), constrained)
		}) + 1
		later := slices.IndexFunc(pages, func(page treeAnswer) bool {
			return page.holdsTuple(graphstore.TupleConstrainedBy, treeEnd(refs.BlockNodeKind, r4), constrained)
		}) + 1
		if tied == 0 || later <= tied {
			t.Errorf("%s: r1's and r4's tuples naming the constraint %s are on pages %d and %d; want distinct pages in walk order", label, constraint.ID, tied, later)
		}
		want := treeRoleContext
		if read.selected {
			want = treeRoleTree
		}
		switch {
		case !held:
			t.Errorf("%s answers no constraint %s on any of its %d pages", label, constraint.ID, len(pages))
		case tied == 0:
			t.Errorf("%s holds r1's constrained_by to the constraint %s on no page", label, constraint.ID)
		case places.walk[address] == tied:
			t.Errorf("%s: the walk place of the constraint %s falls on page %d, which holds r1's constrained_by: this case proves nothing", label, constraint.ID, tied)
		case node.Role != want:
			t.Errorf("%s answers the constraint %s as %q on page %d, want %q; the first page that holds its constrained_by tuple is page %d and its walk place falls on page %d",
				label, constraint.ID, node.Role, places.on[address], want, tied, places.walk[address])
		}
		if read.name == "kind=requirement&kind=constraint" && places.on[address] != 2 {
			// The issue and four ranked requirements precede the unranked constraint in this read.
			t.Errorf("%s: the constraint %s stands on page %d, and its walk place falls on page 2; its constrained_by tuple is on page %d",
				label, constraint.ID, places.on[address], tied)
		}
		for _, tuple := range read.context {
			address := kindFilterAddress(tuple.To.Kind, tuple.To.ID)
			held := kindFilterTupleHeld(pages, tuple.Kind, tuple.From, tuple.To)
			if node := places.nodes[address]; node.Role != treeRoleContext || !held {
				t.Errorf("%s answers %s as role %q on page %d, tuple %s present: %t; want context with the tuple",
					label, address, node.Role, places.on[address], tuple.Kind, held)
			}
		}
		if len(read.context) > 0 {
			for _, end := range []model.IssueTreeEnd{treeEnd(refs.BlockNodeKind, finding.ID), treeEnd(graphstore.KindRisk, risk.ID)} {
				address := kindFilterAddress(end.Kind, end.ID)
				if _, held := places.nodes[address]; !held {
					t.Errorf("%s omits %s from its walk", label, address)
				}
			}
		}
	}
}

// TestAnIssueConstraintAStatementNamesStandsOnThatStatementsPage holds window's placement of the
// context (R8) for a node of the walk that the read with kind answers as context (kindOrder): on
// a server whose pages hold at most contextPageNodes nodes, alice's issue, born converted, holds
// six requirements, r1 first, and then a constraint of the issue, written last, that r1's
// constrained_by names. Read with the conversation's kinds page by page from each cursor, the page
// that holds r1 and its constrained_by holds the constraint, as context, and no other page holds
// it, so a page that draws r1 can draw the constraint's words beside it.
func TestAnIssueConstraintAStatementNamesStandsOnThatStatementsPage(t *testing.T) {
	handler, database, _, _ := newRegisteredTestServer(t, testServerOptions{issueTreePageSize: contextPageNodes})
	createReferenceAPIProject(t, handler, "CPAGE")
	spec := ""
	for index := 1; index <= 6; index++ {
		spec += fmt.Sprintf(":::requirement{#r%d}\nRequirement %d holds.\n:::\n\n", index, index)
	}
	key := createDigestIssue(t, handler, database, "CPAGE", "A statement and the constraint it names", spec, false).Key
	r1 := readDecisionDocument(t, database, key, "spec").address("r1")
	rig := reasoningRig{t: t, handler: handler, database: database, issue: key}
	constraint := decodeBody[constraintRead](t, created(t, "the constraint", rig.post("/constraints",
		map[string]any{"standard": "RFC 9110", "reference": "section 9.2.2"}, "alice")))
	if pointed := rig.post(nodePath(r1, "pointers"), map[string]any{"kind": graphstore.TupleConstrainedBy, "action": pointerAppend,
		"to": constraint.Address}, "alice"); pointed.Code != http.StatusOK {
		t.Fatalf("point r1 at the constraint: status=%d body=%s", pointed.Code, pointed.Body.String())
	}

	query := kindFilterQuery(kindFilterConversation...)
	var pages []treeAnswer
	for target := query; len(pages) < 20; {
		read := treeRead(t, handler, key, target, "alice", false)
		pages = append(pages, read)
		if read.Cursor == nil {
			break
		}
		target = query + "&cursor=" + url.QueryEscape(*read.Cursor)
	}
	if len(pages) < 2 {
		t.Fatalf("the tree of %s with the conversation's kinds reads as %d page, want several of at most %d nodes: this case proves nothing",
			key, len(pages), contextPageNodes)
	}
	statement := -1
	var holding []int
	for index, page := range pages {
		if len(page.nodesNamed(refs.BlockNodeKind, r1)) > 0 {
			statement = index
		}
		for _, node := range page.nodesNamed(graphstore.KindConstraint, constraint.ID) {
			holding = append(holding, index+1)
			if node.Role != treeRoleContext {
				t.Errorf("page %d of %s answers the constraint as %q, want context", index+1, key, node.Role)
			}
		}
	}
	if statement < 0 {
		t.Fatalf("no page of %s with the conversation's kinds answers r1 %s: this case proves nothing", key, r1)
	}
	if !pages[statement].holdsTuple(graphstore.TupleConstrainedBy, treeEnd(refs.BlockNodeKind, r1), treeEnd(graphstore.KindConstraint, constraint.ID)) {
		t.Fatalf("page %d of %s holds r1 and no constrained_by from it to the constraint: this case proves nothing", statement+1, key)
	}
	if len(holding) != 1 || holding[0] != statement+1 {
		t.Errorf("the constraint r1's constrained_by names stands on page %v of %s, want page %d alone, the page that holds r1 and the tuple: %v",
			holding, key, statement+1, pages[statement].addresses())
	}
}

// TestATreePageEndsAtItsBoundOfBytes holds the tree read's bound of size (R8): a page whose JSON
// holds more than issueTreePageBytes is cut at the longest prefix of its walk that holds within
// it, with the context nodes that prefix places, and answers the cursor of the first node it
// leaves out and, in remaining, every node it leaves out beside the nodes after it. Five nodes of
// 500,000 bytes of words each and a context node the fourth places, written as the read writes a
// page, answer three of them, 1,500,000 bytes and more, with the cursor of the fourth and
// remaining the read's 7 after the page and the two nodes and the context node the cut leaves out.
// A page within its bound, and a page of one node, are left whole.
func TestATreePageEndsAtItsBoundOfBytes(t *testing.T) {
	words := strings.Repeat("w", 500_000)
	read := func() model.IssueTreeRead {
		read := model.IssueTreeRead{Issue: "BYTES-1", Nodes: []model.IssueTreeNode{}, Tuples: []model.IssueTreeTuple{}}
		for index := range 5 {
			read.Nodes = append(read.Nodes, model.IssueTreeNode{Kind: "block", ID: fmt.Sprintf("BYTES-1/spec#b%d", index), Role: treeRoleTree, Text: words})
			read.Tuples = append(read.Tuples, model.IssueTreeTuple{Kind: graphstore.TuplePartOf,
				From: model.IssueTreeEnd{Kind: "block", ID: fmt.Sprintf("BYTES-1/spec#b%d", index)}, To: model.IssueTreeEnd{Kind: "issue", ID: "BYTES-1"}})
		}
		read.Nodes = append(read.Nodes, model.IssueTreeNode{Kind: graphstore.KindInteraction, ID: "run-1", Role: treeRoleContext})
		cursor, remaining := treeCursor("block", "BYTES-1/spec#b9"), 7
		read.Cursor, read.Remaining = &cursor, &remaining
		return read
	}
	plan := func() *treePagePlan {
		return &treePagePlan{walk: 5, placed: []int{3}, tuples: []int{1, 2, 3, 4, 5, 5}, after: 7}
	}

	whole := read()
	response := httptest.NewRecorder()
	writeIssueTreePage(response, &whole, plan())
	body := strings.TrimSuffix(response.Body.String(), "\n")
	if len(body) > issueTreePageBytes {
		t.Fatalf("the page of five nodes of 500,000 bytes is %d bytes, want at most %d", len(body), issueTreePageBytes)
	}
	page := decodeBody[model.IssueTreeRead](t, response)
	if len(page.Nodes) != 3 || page.Nodes[2].ID != "BYTES-1/spec#b2" {
		t.Errorf("the cut page answers %d nodes, want the first three of its walk alone and not the context the fourth places", len(page.Nodes))
	}
	if len(page.Tuples) != 3 {
		t.Errorf("the cut page answers %d tuples, want the three of the nodes it keeps", len(page.Tuples))
	}
	if want := treeCursor("block", "BYTES-1/spec#b3"); page.Cursor == nil || *page.Cursor != want {
		t.Errorf("the cut page answers the cursor %v, want %q, the first node it leaves out", page.Cursor, want)
	}
	if page.Remaining == nil || *page.Remaining != 10 {
		t.Errorf("the cut page answers remaining %v, want 10: the read's 7 after the page, and the two nodes and the context node it leaves out", page.Remaining)
	}

	fresh := read()
	if _, cut := plan().cut(&fresh, 3_000_000); cut {
		t.Errorf("a page of five nodes of 500,000 bytes is cut below a bound of 3,000,000 bytes, which it holds within")
	}
	single := read()
	single.Nodes, single.Tuples = single.Nodes[:1], single.Tuples[:1]
	if _, cut := (&treePagePlan{walk: 1, tuples: []int{1}}).cut(&single, 10); cut {
		t.Errorf("a page of one node is cut")
	}
}

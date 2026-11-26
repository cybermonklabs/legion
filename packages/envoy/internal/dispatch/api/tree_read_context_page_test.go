package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// contextPageNodes is the most nodes a page of TestEachContextNodeStandsOnTheFirstPageThatNamesIt
// answers (testServerOptions' issueTreePageSize): the issue's node, the comment written for the run
// and the run's interaction, which the comment's changed_by_run names.
const contextPageNodes = 3

// treePages is every page of the tree read of key as login, each read from the cursor the page
// before answered, and each page's body; it fails past twenty pages.
func treePages(t *testing.T, handler http.Handler, key, login string) ([]treeAnswer, []int) {
	t.Helper()
	var pages []treeAnswer
	var sizes []int
	query := ""
	for page := 1; page <= 20; page++ {
		response := treeResponse(t, handler, key, query, login, false)
		if response.Code != http.StatusOK {
			t.Fatalf("page %d of the tree of %s (%s): status=%d body=%s", page, key, query, response.Code, response.Body.String())
		}
		read := treeAnswer{IssueTreeRead: decodeBody[model.IssueTreeRead](t, response), body: response.Body.String()}
		pages, sizes = append(pages, read), append(sizes, len(strings.TrimSuffix(read.body, "\n")))
		if read.Cursor == nil {
			return pages, sizes
		}
		query = "?cursor=" + url.QueryEscape(*read.Cursor)
	}
	t.Fatalf("the tree of %s still answers a cursor after 20 pages", key)
	return nil, nil
}

// TestEachContextNodeStandsOnTheFirstPageThatNamesIt holds window's placement of the context (R8):
// on a server whose pages hold at most contextPageNodes nodes, alice writes a comment for her run
// of the command bar, whose changed_by_run names the run's interaction, and then four comments
// more, so her tree reads as several pages. The first page holds the comment, its changed_by_run
// and the interaction, as context, and answers a cursor and remaining; no later page holds the
// interaction again. Following the cursor answers each node once, no page holds more than
// contextPageNodes nodes or more than issueTreePageBytes bytes, and each page's remaining is the
// number of nodes the pages after it answer.
func TestEachContextNodeStandsOnTheFirstPageThatNamesIt(t *testing.T) {
	handler, database, deps, _ := newRegisteredTestServer(t, testServerOptions{issueTreePageSize: contextPageNodes})
	ctx := context.Background()
	issue := createTestIssue(t, handler, "TPAGE", "An issue of several pages")
	run, err := directServer(deps).recordGoingRequest(ctx, model.Actor{Kind: "user", ID: "alice"}, "", "Which page holds the run?")
	if err != nil {
		t.Fatalf("record alice's run: %v", err)
	}
	runWindowAhead(t, database, run.ID)
	written := requestForRun(t, handler, http.MethodPost, "/api/v1/issues/"+issue+"/comments",
		map[string]any{"body": "The run wrote this comment."}, "alice", run.ID)
	if written.Code != http.StatusCreated {
		t.Fatalf("write the comment for the run: status=%d body=%s", written.Code, written.Body.String())
	}
	comment := decodeBody[writtenRecord](t, written).ID
	if count := changedByRunCount(t, database, graphstore.KindComment, comment, run.ID); count != 1 {
		t.Fatalf("the comment holds %d changed_by_run tuples to the run, want one: this case proves nothing", count)
	}
	for index := range 4 {
		created(t, fmt.Sprintf("comment %d", index+2), dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue+"/comments",
			map[string]any{"body": fmt.Sprintf("Comment %d, written for no run.", index+2)}, "alice"))
	}

	pages, sizes := treePages(t, handler, issue, "alice")
	if len(pages) < 2 {
		t.Fatalf("the tree of %s reads as %d page, want several of at most %d nodes: this case proves nothing", issue, len(pages), contextPageNodes)
	}
	first := pages[0]
	interaction := first.nodesNamed(graphstore.KindInteraction, run.ID)
	switch {
	case len(first.nodesNamed(graphstore.KindComment, comment)) != 1:
		t.Fatalf("page 1 of %s answers no comment %s, the one written for the run: %v", issue, comment, first.addresses())
	case len(interaction) != 1:
		t.Errorf("page 1 of %s, which holds the comment and its changed_by_run, answers the run's interaction %d times, want once: %v",
			issue, len(interaction), first.addresses())
	case interaction[0].Role != treeRoleContext:
		t.Errorf("page 1 of %s answers the run's interaction as %q, want context", issue, interaction[0].Role)
	}
	if !first.holdsTuple(graphstore.TupleChangedByRun, treeEnd(graphstore.KindComment, comment), treeEnd(graphstore.KindInteraction, run.ID)) {
		t.Errorf("page 1 of %s holds no changed_by_run from the comment to the run: %+v", issue, first.Tuples)
	}

	seen := map[string]int{}
	answered := make([]int, len(pages))
	for index, page := range pages {
		answered[index] = len(page.Nodes)
		if index > 0 && len(page.nodesNamed(graphstore.KindInteraction, run.ID)) > 0 {
			t.Errorf("page %d of %s answers the run's interaction again, which page 1 answered", index+1, issue)
		}
		if len(page.Nodes) > contextPageNodes {
			t.Errorf("page %d of %s answers %d nodes, want at most %d: %v", index+1, issue, len(page.Nodes), contextPageNodes, page.addresses())
		}
		if sizes[index] > issueTreePageBytes {
			t.Errorf("page %d of %s is %d bytes, want at most %d", index+1, issue, sizes[index], issueTreePageBytes)
		}
		for _, address := range page.addresses() {
			if earlier, twice := seen[address]; twice {
				t.Errorf("page %d of %s answers %s, which page %d answered", index+1, issue, address, earlier)
			}
			seen[address] = index + 1
		}
	}
	for index, page := range pages {
		after := 0
		for _, count := range answered[index+1:] {
			after += count
		}
		switch {
		case page.Cursor == nil && page.Remaining != nil:
			t.Errorf("page %d of %s, the last, answers remaining %d", index+1, issue, *page.Remaining)
		case page.Cursor != nil && page.Remaining == nil:
			t.Errorf("page %d of %s answers a cursor and no remaining", index+1, issue)
		case page.Cursor != nil && *page.Remaining != after:
			t.Errorf("page %d of %s answers remaining %d, want %d, the nodes the pages after it answer", index+1, issue, *page.Remaining, after)
		}
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

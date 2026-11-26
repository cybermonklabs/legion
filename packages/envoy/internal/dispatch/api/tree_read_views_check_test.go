package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore/graphstoretest"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// treeReadViewsRecorded is how many version_renders rows of version number of the artifact id
// hold a read_seq: the record of the store revision that version read.
func treeReadViewsRecorded(t *testing.T, database *store.Store, id string, number int) int {
	t.Helper()
	var recorded int
	if err := database.Pool.QueryRow(context.Background(),
		`select count(*) from version_renders where artifact_id = $1::uuid and number = $2 and read_seq is not null`, id, number).Scan(&recorded); err != nil {
		t.Fatalf("read the record of version %d of %s: %v", number, id, err)
	}
	return recorded
}

// TestAQueryVersionWithNoRecordedRevisionAnswersNotRecorded is c-tree-read-views' proof that a
// version of a query that holds no recorded revision answers apart from a version the query does
// not have (R8). On a server whose settlement waits an hour and which starts no block-index
// reconciliation, an issue is born converted with a spec of a list of 1,000 items, 2,001 blocks
// (itemsOverTheBlockBound), and one requirement, so the create defers the spec's index and version
// 1 of the spec query node holds no read_seq. `?view=spec&v=1` answers 404
// QUERY_VERSION_NOT_RECORDED naming version 1. A version 2
// of the same node, written by SQL with no version_renders row as a process of the release before
// writes a version, answers the same refusal naming version 2. `&v=99`, a version the node does not
// have, answers 404 QUERY_VERSION_NOT_FOUND.
func TestAQueryVersionWithNoRecordedRevisionAnswersNotRecorded(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{settle: time.Hour})
	ctx := context.Background()
	createReferenceAPIProject(t, handler, "UNREC")
	issue := createDigestIssue(t, handler, database, "UNREC", "A spec whose create defers its index",
		deferralList(itemsOverTheBlockBound, "end")+"\n"+requirementMarkdown("r1", "The board lists every open ask."), false)
	if indexed := blockIndexVersion(t, database, issue.PrimaryArtifactID); indexed != nil {
		t.Fatalf("the create of %s indexed its spec's blocks in its own request at version %d, so no index was deferred: this case proves nothing", issue.Key, *indexed)
	}
	query := queryArtifactOf(t, database, issue.Key, graphstore.QuerySpec)
	if recorded := treeReadViewsRecorded(t, database, query, 1); recorded != 0 {
		t.Fatalf("version 1 of the spec query of %s holds %d records of the store revision it read, want none: this case proves nothing", issue.Key, recorded)
	}

	unrecorded := func(number int) {
		t.Helper()
		response := treeResponse(t, handler, issue.Key, fmt.Sprintf("?view=spec&v=%d", number), "alice", false)
		var refusal refusalAnswer
		_ = json.Unmarshal(response.Body.Bytes(), &refusal)
		if response.Code != http.StatusNotFound || refusal.Code != "QUERY_VERSION_NOT_RECORDED" {
			t.Errorf("the spec query of %s answers %d %s at version %d, which it holds with no recorded revision, want 404 QUERY_VERSION_NOT_RECORDED",
				issue.Key, response.Code, refusal.Code, number)
			return
		}
		if !graphstoretest.NamesAsAWord(refusal.Error, fmt.Sprintf("version %d", number)) {
			t.Errorf("the refusal of version %d of the spec query of %s does not name the version: %s", number, issue.Key, response.Body.String())
		}
	}
	unrecorded(1)

	if _, err := database.Pool.Exec(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, authors)
		select artifact_id, 2, markdown, authors from artifact_versions where artifact_id = $1::uuid and number = 1`, query); err != nil {
		t.Fatalf("write version 2 of the spec query of %s as the release before writes one: %v", issue.Key, err)
	}
	if recorded := treeReadViewsRecorded(t, database, query, 2); recorded != 0 {
		t.Fatalf("version 2 of the spec query of %s holds %d records of the store revision it read, want none: this case proves nothing", issue.Key, recorded)
	}
	unrecorded(2)

	response := treeResponse(t, handler, issue.Key, "?view=spec&v=99", "alice", false)
	var refusal refusalAnswer
	_ = json.Unmarshal(response.Body.Bytes(), &refusal)
	if response.Code != http.StatusNotFound || refusal.Code != "QUERY_VERSION_NOT_FOUND" {
		t.Errorf("the spec query of %s answers %d %s at version 99, which it does not hold, want 404 QUERY_VERSION_NOT_FOUND",
			issue.Key, response.Code, refusal.Code)
	}
}

// TestTheTreeReadAnswersTheConversationBeforeTheRankedSiblings is c-tree-read-views' proof of the
// order of a node's children (R8 at spec v246): the conversation nodes first, by made_at and then
// by id, then the ranked siblings by rank, then a block that holds no rank yet, new in a deferred
// version, by made_at and then by id. On a server whose settlement waits an hour and which starts
// no block-index reconciliation, an issue born converted holds r2 and then r1 in its spec, so r2
// holds the lower rank, against the order of their ids; alice comments on it, and then a session
// opens an ask on it; and an upload of the spec past the bound (deferralList) adds r3, which that
// deferred write writes with no rank. The tree read with no view answers the issue's children
// the comment, the ask, r2, r1 and r3 in that order, and so does the walk of the tuples
// (docs.WalkIssueTree), which the record of every query follows (R14). Else the test fails,
// printing `<read> answers the issue's children <got>, want <want>: <node> stands at <index>,
// where <node> belongs`.
func TestTheTreeReadAnswersTheConversationBeforeTheRankedSiblings(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{settle: time.Hour})
	ctx := context.Background()
	createReferenceAPIProject(t, handler, "SIBS")
	spec := requirementMarkdown("r2", "The list shows the urgent asks first.") + "\n" + requirementMarkdown("r1", "The list shows each ask's urgency.")
	issue := createDigestIssue(t, handler, database, "SIBS", "The order of an issue's children", spec, false)
	comment, _ := personComment(t, handler, database, issue.Key, "Which asks does the list show first?")
	ask := createIssueAsk(t, handler, issue.Key, "Does urgency come before age?")
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "spec",
		spec+"\n"+requirementMarkdown("r3", "The list shows the oldest ask last.")+"\n"+deferralList(itemsOverTheBlockBound, "end"))

	read := treeRead(t, handler, issue.Key, "", "alice", false)
	held, ranks := map[string]string{}, map[string]string{}
	for _, block := range []string{"r2", "r1", "r3"} {
		node, found := blockNodeOf(read, block)
		if !found {
			t.Fatalf("the tree read of %s answers no requirement %s: %v", issue.Key, block, read.addresses())
		}
		held[block] = node.Kind + ":" + node.ID
		switch ranked := node.Rank != nil; {
		case block == "r3" && ranked:
			t.Fatalf("the tree read of %s answers r3 with the rank %s, want none until the reconciliation ranks it: this case proves nothing", issue.Key, *node.Rank)
		case block != "r3" && !ranked:
			t.Fatalf("the tree read of %s answers %s with no rank: this case proves nothing", issue.Key, block)
		case ranked:
			ranks[block] = *node.Rank
		}
	}
	if ranks["r2"] >= ranks["r1"] || held["r2"] < held["r1"] {
		t.Fatalf("the tree read of %s answers r2 (%s) at the rank %q and r1 (%s) at %q, want r2 the lower rank and the greater id: this case proves nothing",
			issue.Key, held["r2"], ranks["r2"], held["r1"], ranks["r1"])
	}
	said, _ := read.node(t, graphstore.KindComment, comment)
	asked, _ := read.node(t, graphstore.KindAsk, ask)
	if said.MadeAt == nil || asked.MadeAt == nil || !said.MadeAt.Before(*asked.MadeAt) {
		t.Fatalf("the tree read of %s answers the comment made at %v and the ask made at %v, want the comment first: this case proves nothing", issue.Key, said.MadeAt, asked.MadeAt)
	}
	want := []string{graphstore.KindComment + ":" + comment, graphstore.KindAsk + ":" + ask, held["r2"], held["r1"], held["r3"]}
	order := func(what string, children []string) {
		t.Helper()
		var got []string
		for _, child := range children {
			if slices.Contains(want, child) {
				got = append(got, child)
			}
		}
		for index, address := range want {
			if index >= len(got) || got[index] != address {
				stands := "nothing"
				if index < len(got) {
					stands = got[index]
				}
				t.Errorf("%s answers the issue's children %v, want %v: %s stands at %d, where %s belongs", what, got, want, stands, index, address)
				return
			}
		}
	}

	var children []string
	for _, node := range read.Nodes {
		if node.Parent != nil && node.Parent.Kind == graphstore.KindIssue && node.Parent.ID == issue.Key {
			children = append(children, node.Kind+":"+node.ID)
		}
	}
	order("the tree read with no view of "+issue.Key, children)

	tree, err := docs.WalkIssueTree(ctx, database.Pool, issue.Key, refs.TreeCut{})
	if err != nil {
		t.Fatalf("walk the tree of %s: %v", issue.Key, err)
	}
	children = children[:0]
	for _, node := range tree.Nodes {
		if node.Parent == (refs.Target{Kind: graphstore.KindIssue, ID: issue.Key}) {
			children = append(children, node.Address())
		}
	}
	order("the walk of "+issue.Key, children)
}

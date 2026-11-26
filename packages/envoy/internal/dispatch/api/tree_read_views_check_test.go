package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore/graphstoretest"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// treeReadViewsSpecQuery is the id of the artifact row of the spec query node of the issue key:
// the row whose ref_key is a node of kind artifact and supertag query whose query is spec, the
// node the tree read reads a version of `?view=spec` from.
func treeReadViewsSpecQuery(t *testing.T, database *store.Store, key string) string {
	t.Helper()
	var id string
	if err := database.Pool.QueryRow(context.Background(), `
		select a.id::text
		from artifacts a
		join graph_nodes n on n.kind = $2 and n.id = a.ref_key and n.supertag = $3 and n.attributes->>$4 = $5
		where a.issue_key = $1`,
		key, graphstore.KindArtifact, graphstore.SupertagQuery, graphstore.AttributeQuery, graphstore.QuerySpec).Scan(&id); err != nil {
		t.Fatalf("read the spec query node of %s: %v", key, err)
	}
	return id
}

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
// reconciliation, an issue is born converted with a spec of a list of 2,000 items and one
// requirement, so the create defers the spec's index and version 1 of the spec query node holds no
// read_seq. `?view=spec&v=1` answers 404 QUERY_VERSION_NOT_RECORDED naming version 1. A version 2
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
	query := treeReadViewsSpecQuery(t, database, issue.Key)
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

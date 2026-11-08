// issue_create_self_reference_test.go holds what a new issue's spec stores and counts when it
// cites the issue being created: the create plans the spec's references in its read phase, while
// the issues row and the spec's artifacts row are queued for its write phase (INFRA-636), so
// what the plan reads of those two rows it reads from what the transaction has queued.
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// createdIssueReferences is the targets the issue.created event of key names as moved
// (model.ReferencesChangedKey).
func createdIssueReferences(t *testing.T, database *store.Store, key string) []model.ChangedReference {
	t.Helper()
	var payload []byte
	if err := database.Pool.QueryRow(context.Background(), `
		select payload from events where issue_key = $1 and type = 'issue.created'
	`, key).Scan(&payload); err != nil {
		t.Fatalf("read the issue.created event of %s: %v", key, err)
	}
	var named struct {
		References []model.ChangedReference `json:"references_changed"`
	}
	if err := json.Unmarshal(payload, &named); err != nil {
		t.Fatalf("decode the issue.created payload of %s: %v", key, err)
	}
	return named.References
}

// namesReference reports whether the event named the target of kind kind with id id.
func namesReference(references []model.ChangedReference, kind, id string) bool {
	for _, reference := range references {
		if reference.Kind == kind && reference.ID == id {
			return true
		}
	}
	return false
}

// A spec written at issue creation can cite the issue it opens: its key bare, and its own
// document's address. Both name rows this very transaction writes, so the edge the bare key
// stores and the targets the issue.created event names are read from what it has queued.
func TestIssueCreateSpecCitingTheIssueItOpensStoresAndCountsIt(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "TEST", "name": "Test project",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "TEST", "title": "An issue citing itself",
		"spec": "Tracked by TEST-1, specified in dispatch://TEST-1/spec.\n",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", created.Code, created.Body.String())
	}
	issue := decodeBody[struct {
		Key               string `json:"key"`
		PrimaryArtifactID string `json:"primary_artifact_id"`
	}](t, created)
	if issue.Key != "TEST-1" {
		t.Fatalf("created issue key = %q, want TEST-1", issue.Key)
	}

	var edges int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from refs
		where from_kind = 'artifact' and from_id = $1 and to_kind = 'issue' and to_id = $2
	`, issue.PrimaryArtifactID, issue.Key).Scan(&edges); err != nil {
		t.Fatalf("count the spec's edges to its own issue: %v", err)
	}
	if edges != 1 {
		t.Errorf("the spec's edges to its own issue %s = %d, want 1", issue.Key, edges)
	}
	references := createdIssueReferences(t, database, issue.Key)
	if !namesReference(references, "issue", issue.Key) {
		t.Errorf("the issue.created event names %v, want the issue %s among them", references, issue.Key)
	}
	if !namesReference(references, "artifact", issue.PrimaryArtifactID) {
		t.Errorf("the issue.created event names %v, want the spec %s among them", references, issue.PrimaryArtifactID)
	}
}

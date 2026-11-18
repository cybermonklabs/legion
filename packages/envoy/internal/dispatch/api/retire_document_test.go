package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// retireTestDocument retires artifactID through docs.RetireDocument in a transaction it commits,
// as alice, and answers what the retirement reports.
func retireTestDocument(t *testing.T, database *store.Store, artifactID string) docs.Retirement {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the retirement: %v", err)
	}
	defer tx.Rollback(ctx)
	retirement, err := docs.RetireDocument(ctx, tx, artifactID, model.Actor{Kind: "user", ID: "alice"}, "The test retires it.")
	if err != nil {
		t.Fatalf("retire %s: %v", artifactID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the retirement: %v", err)
	}
	return retirement
}

// issueDocumentIDs is the id of each artifact GET /api/v1/issues/{key}/artifacts lists.
func issueDocumentIDs(t *testing.T, handler http.Handler, key string) []string {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+key+"/artifacts", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("list the documents of %s: status=%d body=%s", key, response.Code, response.Body.String())
	}
	var ids []string
	for _, listed := range decodeBody[[]struct {
		ID string `json:"id"`
	}](t, response) {
		ids = append(ids, listed.ID)
	}
	return ids
}

// approvalStateOf is the approval state GET /api/v1/artifacts/{id} answers.
func approvalStateOf(t *testing.T, handler http.Handler, artifactID string) string {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+artifactID, nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/artifacts/%s: status=%d body=%s", artifactID, response.Code, response.Body.String())
	}
	artifact := decodeBody[model.Artifact](t, response)
	if artifact.Approval == nil {
		t.Fatalf("GET /api/v1/artifacts/%s answers no approval", artifactID)
	}
	return artifact.Approval.State
}

// The retirement of a document (dispatch://INFRA-1150 R11, docs.RetireDocument): an issue's
// Verification document, which a person approved at its second version, is retired. Its role reads
// null and the issue names no Verification document, its slug giving it none back; its node reads
// gone; one document.retired event names it, the reason and the approval it superseded, which the
// retirement reports too; GET /api/v1/artifacts/{id} reads that approval stale and no longer
// approved; each of its versions still reads; and the issue's document list and its summary leave
// it out. A second retirement of it is refused.
func TestRetireDocument(t *testing.T) {
	handler, database := blockIndexHandler(t)
	ctx := context.Background()
	createReferenceAPIProject(t, handler, "CORE")
	issue := createSpecIssue(t, handler, "CORE", "The spec.\n")
	verification := uploadIssueDocument(t, handler, issue.Key, "verification.md", "The first version.\n")
	editIssueDocument(t, handler, issue.Key, verification.Slug, []map[string]any{{"op": "insert", "after": "end", "markdown": "The second version."}})
	if verification.Role != model.ArtifactRoleVerification {
		t.Fatalf("the document uploaded as verification.md plays %q, want %q", verification.Role, model.ArtifactRoleVerification)
	}
	approval := reviewDocument(t, handler, verification.ID, "alice", "approved", "")
	if approval.Version != 2 || approvalStateOf(t, handler, verification.ID) != "approved" {
		t.Fatalf("the person's approval is of version %d and reads %q, want version 2 approved", approval.Version, approvalStateOf(t, handler, verification.ID))
	}
	if got := decodeBody[model.Issue](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key, nil, "alice")).VerificationArtifactID; got != verification.ID {
		t.Fatalf("before the retirement the issue names Verification document %q, want %q", got, verification.ID)
	}

	retirement := retireTestDocument(t, database, verification.ID)

	superseded := retirement.SupersededApproval
	if superseded == nil || superseded.ReviewID != approval.ID || superseded.ApprovedVersion != 2 || superseded.ApprovedBy.ID != "alice" {
		t.Fatalf("the retirement reports the superseded approval %+v, want alice's review %s of version 2", superseded, approval.ID)
	}
	var role *string
	var retired bool
	if err := database.Pool.QueryRow(ctx, `select role, retired_at is not null from artifacts where id = $1`, verification.ID).Scan(&role, &retired); err != nil {
		t.Fatalf("read the retired row: %v", err)
	}
	if role != nil || !retired {
		t.Fatalf("the retired document's row reads role %v and retired %t, want no role and retired", role, retired)
	}
	if got := decodeBody[model.Issue](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key, nil, "alice")).VerificationArtifactID; got != "" {
		t.Fatalf("after the retirement the issue names Verification document %q, want none: its slug gives it the role back", got)
	}
	node, held, err := graphstore.ReadNode(ctx, database.Pool, graphstore.KindArtifact, verification.RefKey)
	if err != nil || !held || node.State != graphstore.StateGone {
		t.Fatalf("the retired document's node is held %t in state %q (%v), want it gone", held, node.State, err)
	}
	if state, stated, err := graphstore.CurrentValue(ctx, database.Pool, graphstore.KindArtifact, verification.RefKey, graphstore.AttributeState); err != nil || !stated || state != graphstore.StateGone {
		t.Fatalf("the retired document's node states %q (%t, %v), want gone", state, stated, err)
	}

	var events int
	var payload []byte
	if err := database.Pool.QueryRow(ctx, `
		select count(*), max(payload::text) from events where type = $1 and payload->>'artifact_id' = $2
	`, model.DocumentRetiredEventType, verification.ID).Scan(&events, &payload); err != nil {
		t.Fatalf("read the retirement's events: %v", err)
	}
	var written model.DocumentRetiredEventPayload
	if err := json.Unmarshal(payload, &written); err != nil {
		t.Fatalf("decode the document.retired payload %s: %v", payload, err)
	}
	if events != 1 || written.Reason != "The test retires it." || written.Role != model.ArtifactRoleVerification ||
		written.SupersededApproval == nil || written.SupersededApproval.ReviewID != approval.ID || retirement.Event.Type != model.DocumentRetiredEventType {
		t.Fatalf("the retirement appended %d document.retired event(s) with %+v (returned %s), want one naming the reason, the role and alice's approval",
			events, written, retirement.Event.Type)
	}

	if state := approvalStateOf(t, handler, verification.ID); state == "approved" || state != "stale" {
		t.Fatalf("GET /api/v1/artifacts/%s reads the retired document's approval %q, want it stale and not approved", verification.ID, state)
	}
	for number := 1; number <= 2; number++ {
		path := "/api/v1/artifacts/" + verification.ID + "/versions/" + strconv.Itoa(number)
		if response := dispatchRequest(t, handler, http.MethodGet, path, nil, "alice"); response.Code != http.StatusOK {
			t.Fatalf("GET %s: status=%d body=%s, want the retired document's version", path, response.Code, response.Body.String())
		}
	}
	if ids := issueDocumentIDs(t, handler, issue.Key); slices.Contains(ids, verification.ID) || !slices.Contains(ids, issue.PrimaryArtifactID) {
		t.Fatalf("the issue's document list is %v, want the spec %s and not the retired %s", ids, issue.PrimaryArtifactID, verification.ID)
	}
	summary := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/summary", nil, "alice")
	if summary.Code != http.StatusOK || strings.Contains(summary.Body.String(), verification.ID) {
		t.Fatalf("the issue's summary: status=%d body=%s, want it to leave the retired document out", summary.Code, summary.Body.String())
	}

	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the second retirement: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := docs.RetireDocument(ctx, tx, verification.ID, model.Actor{Kind: "user", ID: "alice"}, "Again."); !errors.Is(err, docs.ErrDocumentRetired) {
		t.Fatalf("a second retirement answers %v, want docs.ErrDocumentRetired", err)
	}
}

// A retired document's address, dispatch://KEY/artifact/<slug>, resolves to the document before
// its retirement and to the issue KEY after it (refs' loadArtifactNodes), through refs.Node and
// GET /api/v1/references alike; read by its id, the retired document is still the document.
func TestARetiredDocumentsAddressResolvesToItsIssue(t *testing.T) {
	handler, database := blockIndexHandler(t)
	ctx := context.Background()
	createReferenceAPIProject(t, handler, "CORE")
	issue := createSpecIssue(t, handler, "CORE", "The spec.\n")
	premortem := uploadIssueDocument(t, handler, issue.Key, "graph-premortem.md", "What could go wrong.\n")
	address := "dispatch://" + issue.Key + "/artifact/" + premortem.Slug
	resolved := func(stage string) model.GraphNode {
		t.Helper()
		byAddress, found, err := refs.Node(ctx, database.Pool, model.Actor{}, graphstore.KindArtifact, premortem.RefKey, true)
		if err != nil || !found {
			t.Fatalf("%s: refs.Node of %s found %t (%v)", stage, premortem.RefKey, found, err)
		}
		response := dispatchReferences(t, handler, address, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("%s: GET /api/v1/references?to=%s: status=%d body=%s", stage, address, response.Code, response.Body.String())
		}
		if read := decodeBody[model.GraphReferences](t, response).Node; read.Kind != byAddress.Kind || read.ID != byAddress.ID || read.Ref != byAddress.Ref {
			t.Fatalf("%s: the references read names %s %s (%s), refs.Node %s %s (%s)", stage, read.Kind, read.ID, read.Ref, byAddress.Kind, byAddress.ID, byAddress.Ref)
		}
		return byAddress
	}
	if before := resolved("before the retirement"); before.Kind != graphstore.KindArtifact || before.ID != premortem.ID || before.Ref != address {
		t.Fatalf("before the retirement %s resolves to %s %s (%s), want the document %s", address, before.Kind, before.ID, before.Ref, premortem.ID)
	}

	retireTestDocument(t, database, premortem.ID)

	if after := resolved("after the retirement"); after.Kind != graphstore.KindIssue || after.ID != issue.Key || after.Ref != "dispatch://"+issue.Key {
		t.Fatalf("after the retirement %s resolves to %s %s (%s), want the issue %s", address, after.Kind, after.ID, after.Ref, issue.Key)
	}
	byID, found, err := refs.Node(ctx, database.Pool, model.Actor{}, graphstore.KindArtifact, premortem.ID, false)
	if err != nil || !found || byID.Kind != graphstore.KindArtifact || byID.ID != premortem.ID {
		t.Fatalf("the retired document read by its id is %s %s, found %t (%v), want the document", byID.Kind, byID.ID, found, err)
	}
}

// Text written to a retired document would be stored where no reader looks for it, so every write
// path of api that reaches one refuses it 409 DOCUMENT_RETIRED: an upload under its name and under
// its slug, a create of a document under its name, and an edit by its slug and by its id. A
// document of another name is still taken.
func TestAWriteToARetiredDocumentIsRefused(t *testing.T) {
	handler, database := blockIndexHandler(t)
	createReferenceAPIProject(t, handler, "CORE")
	issue := createSpecIssue(t, handler, "CORE", "The spec.\n")
	notes := uploadIssueDocument(t, handler, issue.Key, "notes.md", "Notes.\n")
	retireTestDocument(t, database, notes.ID)

	refused := func(write string, response *httptest.ResponseRecorder) {
		t.Helper()
		if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "DOCUMENT_RETIRED") {
			t.Fatalf("%s of the retired document: status=%d body=%s, want 409 DOCUMENT_RETIRED", write, response.Code, response.Body.String())
		}
	}
	for _, name := range []string{notes.Name, notes.Slug} {
		refused("an upload under "+name, multipartRequest(t, handler, "/api/v1/issues/"+issue.Key+"/artifacts",
			map[string]string{"name": name}, name, "text/markdown", []byte("New text.\n"), "alice"))
	}
	refused("a create under its name", dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]any{"name": notes.Name}, "alice"))
	ops := map[string]any{"ops": []map[string]any{{"op": "insert", "after": "end", "markdown": "More."}}, "summary": "edit"}
	refused("an edit by its slug", dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts/"+notes.Slug+"/edits", ops, "alice"))
	refused("an edit by its id", dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+notes.ID+"/edits", ops, "alice"))
	// The socket to its room is refused in docs, whose own server proves the browser's login
	// (docs' TestASocketToARetiredDocumentIsRefused).

	if versions := latestVersion(t, database, notes.ID); versions.Number != 1 {
		t.Fatalf("the retired document reads version %d after the refused writes, want 1", versions.Number)
	}
	uploadIssueDocument(t, handler, issue.Key, "other.md", "Another document is taken.\n")
}

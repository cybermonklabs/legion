package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// TestM19AnOpenApprovalOfAHiddenProjectNamesNothingOnAVisibleDocument: the open approval request a
// document's read names (openApprovalAsksStatement) is one its reader may read as an ask of its
// own issue or document, not only one that names the document. A PRIV request whose documents
// name PUB's - a store no route writes today, since a request names its own issue's documents -
// names nothing on PUB's document to alice: neither its id, nor who asked, nor whose turn it is.
// With PRIV public, alice reads that request on PUB's document as before (R9).
func TestM19AnOpenApprovalOfAHiddenProjectNamesNothingOnAVisibleDocument(t *testing.T) {
	seed := seedPrivateProject(t)
	upload := func(target string) string {
		return decodeBody[artifactUploadResponse](t, seed.mustRequest(t, callerCarol, http.MethodPost, target,
			map[string]any{"name": "Runbook.md", "content": "# Runbook\n\nThe runbook steps.\n"}, http.StatusCreated)).Artifact.ID
	}
	publicDocument := upload("/api/v1/projects/" + seedPublic + "/artifacts")
	privateIssue := seed.issue[seedPrivate]
	privateDocument := upload("/api/v1/issues/" + privateIssue + "/artifacts")
	request := uuid.NewString()
	seedExec(t, seed.database, "a PRIV approval request naming PUB's document", `
		insert into asks (id, issue_key, author, question, kind, state, approval)
		values ($1::text::uuid, $2::text, '{"kind":"session","id":"ses-of-priv"}', 'Approve both runbooks?',
			'approval', 'open', jsonb_build_object(
				'artifact_id', $3::text, 'name', 'Runbook.md', 'version', 1, 'requested_version', 1,
				'documents', jsonb_build_array(
					jsonb_build_object('artifact_id', $3::text, 'name', 'Runbook.md', 'version', 1, 'requested_version', 1),
					jsonb_build_object('artifact_id', $4::text, 'name', 'Runbook.md', 'version', 1, 'requested_version', 1))))
	`, request, privateIssue, privateDocument, publicDocument)

	read := func(t *testing.T) (*model.ArtifactApproval, string) {
		t.Helper()
		response := seed.mustRequest(t, callerAlice, http.MethodGet, "/api/v1/artifacts/"+publicDocument, nil, http.StatusOK)
		artifact := decodeBody[model.Artifact](t, response)
		if artifact.Approval == nil {
			t.Fatalf("alice's read of PUB's document %s answers no approval: %s", publicDocument, response.Body.String())
		}
		return artifact.Approval, response.Body.String()
	}

	t.Run("PRIV private: the request names nothing on PUB's document", func(t *testing.T) {
		approval, body := read(t)
		if approval.State == "awaiting" || approval.AskID != nil || approval.RequestedBy != nil || approval.WaitingOn != "" {
			t.Errorf("alice's read of PUB's document answers the approval %+v, want no request: PRIV's request %s is hidden from her", *approval, request)
		}
		if strings.Contains(body, request) || strings.Contains(body, "ses-of-priv") {
			t.Errorf("alice's read of PUB's document names PRIV's request %s or its author:\n%s", request, body)
		}
	})

	t.Run("PRIV public: the request is named as before", func(t *testing.T) {
		seed.setVisibility(t, seedPrivate, model.ProjectPublic)
		approval, _ := read(t)
		if approval.State != "awaiting" || approval.AskID == nil || *approval.AskID != request || approval.RequestedBy == nil {
			t.Errorf("PRIV public: alice's read of PUB's document answers the approval %+v, want PRIV's request %s awaiting, as before", *approval, request)
		}
	})
}

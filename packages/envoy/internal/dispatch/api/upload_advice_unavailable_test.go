package api

import (
	"encoding/json"
	"net/http"
	"testing"
)

// An upload that supersedes a person's approval says so even when the issue's advice cannot be
// read in time. Both are read in the upload's read phase, each behind a savepoint of its own under
// the 500 ms statement_timeout (adviceReport): the warning from the document's reviews, the
// issue-scoped advice from the issue's events and asks. Here the issue advice's read of the
// session's open asks (your_open_asks) runs past that bound (newFailingAdviceHandler), and Postgres
// cancels it. The upload still lands, its answer carries no issue state, and it must still carry
// superseded_approval, as an edit's does when its advice is lost.
func TestAnUploadSupersedingAnApprovalSaysSoWhenItsAdviceIsUnavailable(t *testing.T) {
	t.Parallel()
	handler, _ := newFailingAdviceHandler(t, "your_open_asks", 0)
	createAdviceProject(t, handler, "UPLOST")
	spec := "# Transport\n\nThe design, as approved.\n"
	issue := createAdviceIssue(t, handler, "UPLOST", "Approved spec", &spec)
	approved := approveAdviceDocument(t, handler, issue.PrimaryArtifactID)

	replaced := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]any{
		"actor": adviceSessionActor("session-editor"),
		"name":  "spec.md", "content": "# Transport\n\nThe design, rewritten.\n",
		"precondition": uploadBase(t, handler, issue.PrimaryArtifactID),
	})
	if replaced.Code != http.StatusCreated {
		t.Fatalf("upload: status=%d body=%s", replaced.Code, replaced.Body.String())
	}
	var body struct {
		Advice map[string]json.RawMessage `json:"advice"`
	}
	if err := json.Unmarshal(replaced.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode the upload's answer: %v\n%s", err, replaced.Body.String())
	}
	if _, carried := body.Advice["issue_status"]; carried {
		t.Fatalf("upload advice = %s, want no issue state: its read was cut off at the advice's bound", replaced.Body.String())
	}
	var superseded *struct {
		ApprovedVersion int `json:"approved_version"`
		Version         int `json:"version"`
	}
	if raw, ok := body.Advice["superseded_approval"]; ok {
		if err := json.Unmarshal(raw, &superseded); err != nil {
			t.Fatalf("decode superseded_approval: %v\n%s", err, replaced.Body.String())
		}
	}
	if superseded == nil {
		t.Fatalf("the upload superseded the approval of version %d, and its answer does not say so: %s",
			approved, replaced.Body.String())
	}
	if superseded.ApprovedVersion != approved || superseded.Version != approved+1 {
		t.Fatalf("superseded_approval = %+v, want version %d superseding the approval of %d", *superseded, approved+1, approved)
	}
}

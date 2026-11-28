package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// uploadBase is the token an upload replacing a document names (`precondition` on the JSON
// upload), as the API's own tests send it (api's uploadBaseNamed): the token `dispatch doc-read`
// prints, which GET /api/v1/artifacts/{id}/text answers through p for the document the owner holds
// under name - its name or its slug - read from the owner's listing. ownerPath is the artifacts
// path the upload posts to. It answers nil where the owner holds nothing under that name: a first
// upload replaces no version and names no token.
func (p *dispatchProcess) uploadBase(t *testing.T, ownerPath, name string) map[string]string {
	t.Helper()
	var listed []struct {
		ID       string  `json:"id"`
		Name     string  `json:"name"`
		Slug     string  `json:"slug"`
		IssueKey *string `json:"issue_key"`
	}
	if answer := p.call(t, http.MethodGet, ownerPath, "", http.StatusOK); json.Unmarshal(answer, &listed) != nil {
		t.Fatalf("decode the listing %s: %s", ownerPath, answer)
	}
	// A project's listing carries its issues' documents too, so only a document of the owner the
	// path names is read.
	ofTheProject := strings.Contains(ownerPath, "/projects/")
	for _, artifact := range listed {
		if ofTheProject == (artifact.IssueKey != nil) {
			continue
		}
		if artifact.Name == name || artifact.Slug == name {
			return map[string]string{"document": p.documentToken(t, artifact.ID)}
		}
	}
	return nil
}

// documentToken is the token the document carries now, as GET /api/v1/artifacts/{id}/text answers
// it through p.
func (p *dispatchProcess) documentToken(t *testing.T, artifactID string) string {
	t.Helper()
	answer := p.call(t, http.MethodGet, "/api/v1/artifacts/"+artifactID+"/text", "", http.StatusOK)
	var text struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(answer, &text); err != nil || text.Token == "" {
		t.Fatalf("read the token of %s: %v: %s", artifactID, err, answer)
	}
	return text.Token
}

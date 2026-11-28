package main

import (
	"encoding/json"
	"net/http"
	"testing"
)

// typedDocument is a document made by typing a title and never written in, the one kind the
// removal route removes: its id, the ref_key its node is addressed by, and its slug. Its two
// helpers carry no build tag, so the suites of several builds over one database, the basebuild
// and twobuild tests among them, share them.
type typedDocument struct{ id, ref, slug string }

// makeTypedDocument makes one of issue's documents through p, the build built names, by typing its
// title, as the test's login.
func makeTypedDocument(t *testing.T, p *dispatchProcess, built, issue, title string) typedDocument {
	t.Helper()
	body, err := json.Marshal(map[string]string{"name": title})
	if err != nil {
		t.Fatalf("encode the create of %q: %v", title, err)
	}
	answer := p.send(t, http.MethodPost, "/api/v1/issues/"+issue+"/artifacts", string(body), asUser(dispatchTestLogin))
	if answer.status != http.StatusCreated {
		t.Fatalf("%s: POST /api/v1/issues/%s/artifacts of %q answers %s, want 201", built, issue, title, answer)
	}
	var made struct {
		Artifact struct {
			ID     string `json:"id"`
			RefKey string `json:"ref_key"`
			Slug   string `json:"slug"`
		} `json:"artifact"`
	}
	if err := json.Unmarshal(answer.body, &made); err != nil || made.Artifact.ID == "" || made.Artifact.RefKey == "" || made.Artifact.Slug == "" {
		t.Fatalf("%s: read the document %q made on %s: %v: %s", built, title, issue, err, answer.body)
	}
	return typedDocument{id: made.Artifact.ID, ref: made.Artifact.RefKey, slug: made.Artifact.Slug}
}

// removeTypedDocument removes the document through the DELETE /api/v1/artifacts/{id} of p, the
// build built names, as the test's login.
func removeTypedDocument(t *testing.T, p *dispatchProcess, built string, document typedDocument) {
	t.Helper()
	answer := p.send(t, http.MethodDelete, "/api/v1/artifacts/"+document.id, "", asUser(dispatchTestLogin))
	if answer.status != http.StatusNoContent {
		t.Fatalf("%s: DELETE /api/v1/artifacts/%s answers %s, want 204", built, document.id, answer)
	}
	t.Logf("%s removed %s (%s)", built, document.id, document.ref)
}

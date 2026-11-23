package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// TestAReferenceFromAHiddenDocumentAnswersAsAMissingOne: GET /api/v1/references?from= a document
// of PRIV, of the project or of its issue, answers every caller that does not see PRIV exactly as
// the same reference to a slug nobody holds: the same status, the same bytes but for the slug,
// and the same statements, so neither the document's existence nor its id can be read off the
// answer or its cost. carol, who sees PRIV, reads the document's node.
func TestAReferenceFromAHiddenDocumentAnswersAsAMissingOne(t *testing.T) {
	t.Parallel()
	census := &fragmentCensus{}
	seed := seedPrivateProjectWith(t, testServerOptions{store: storetest.TracedStore(t, storetest.Open(t), census)})
	upload := func(target string) (id, slug string) {
		t.Helper()
		uploaded := decodeBody[artifactUploadResponse](t, seed.mustRequest(t, callerCarol, http.MethodPost, target,
			map[string]any{"name": "Runbook.md", "content": "# Runbook\n\nThe runbook steps.\n"}, http.StatusCreated))
		return uploaded.Artifact.ID, uploaded.Artifact.Slug
	}
	// The slug nobody holds is spelled as the seed's own is, so the two answers differ by it alone.
	const missingSlug = "nobody-md"
	recorded := func(t *testing.T, caller privateCaller, reference string) (*httptest.ResponseRecorder, []string) {
		t.Helper()
		census.mu.Lock()
		census.statements = nil
		census.mu.Unlock()
		response := seed.requestIn(t, context.WithValue(context.Background(), recordingKey{}, true), caller, http.MethodGet,
			"/api/v1/references?"+url.Values{"from": {reference}}.Encode(), nil)
		census.mu.Lock()
		defer census.mu.Unlock()
		return response, slices.DeleteFunc(slices.Clone(census.statements), volatileStatement)
	}
	for _, owner := range []struct {
		name, target, reference string
	}{
		{"a document of PRIV", "/api/v1/projects/" + seedPrivate + "/artifacts", "dispatch://" + seedPrivate + "/artifact/"},
		{"a document of PRIV's issue", "/api/v1/issues/" + seed.issue[seedPrivate] + "/artifacts", "dispatch://" + seed.issue[seedPrivate] + "/artifact/"},
	} {
		id, slug := upload(owner.target)
		t.Run(owner.name, func(t *testing.T) {
			read, _ := recorded(t, callerCarol, owner.reference+slug)
			if read.Code != http.StatusOK || decodeBody[model.GraphReferences](t, read).Node.ID != id {
				t.Fatalf("carol, who sees PRIV: ?from=%s = %d %s, want 200 naming document %s", owner.reference+slug, read.Code, read.Body.String(), id)
			}
			for _, caller := range []privateCaller{callerAlice, callerCarolToken, callerAlicePrivToken, callerSharedToken} {
				hidden, hiddenStatements := recorded(t, caller, owner.reference+slug)
				missing, missingStatements := recorded(t, caller, owner.reference+missingSlug)
				if missing.Code != http.StatusNotFound {
					t.Fatalf("%s: ?from= a slug nobody holds = %d %s, want 404", caller, missing.Code, missing.Body.String())
				}
				if hidden.Code != missing.Code || strings.ReplaceAll(hidden.Body.String(), slug, missingSlug) != missing.Body.String() {
					t.Errorf("%s: ?from=%s = %d %s, want what a slug nobody holds answers: %d %s",
						caller, owner.reference+slug, hidden.Code, hidden.Body.String(), missing.Code, missing.Body.String())
				}
				if !slices.Equal(hiddenStatements, missingStatements) {
					extra := slices.DeleteFunc(slices.Clone(hiddenStatements), func(statement string) bool {
						return slices.Contains(missingStatements, statement)
					})
					for index, statement := range extra {
						extra[index] = oneLine(statement)[:min(len(oneLine(statement)), 160)]
					}
					t.Errorf("%s: ?from= the hidden document sent %d statements and ?from= a slug nobody holds %d; want the same ones. The hidden one's own: %q",
						caller, len(hiddenStatements), len(missingStatements), extra)
				}
			}
		})
	}
}

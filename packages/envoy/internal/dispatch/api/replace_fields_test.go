package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A replace whose with is absent, or that carries a member replace does not take, is refused 400
// REPLACE_FIELDS naming its operation and the member, and the document keeps its text and its
// versions. Read as an empty with, such a replace deleted the text its find matched, which a caller
// that sent markdown in place of with met (dispatch://INFRA-1946). A replace takes op, find, with
// and occurrence and no other member, whatever that member holds: one carrying a member no
// operation takes beside its with (replacement) was applied, one carrying an empty markdown beside
// it passed, and one carrying its new text as text was refused for its missing with alone, the
// member that held the text unnamed. Each refused replace stands second in its batch, after a
// replace that would write, so the refusal names operation 1 and the batch writes nothing.
func TestAReplaceWithoutItsWithIsRefused(t *testing.T) {
	for _, test := range []struct {
		name  string
		op    map[string]any
		wants []string
	}{
		{
			name:  "a replace with no with",
			op:    map[string]any{"op": "replace", "find": "Planning notes here."},
			wants: []string{"operation 1: replace names no with"},
		},
		{
			name:  "a replace whose with is null",
			op:    map[string]any{"op": "replace", "find": "Planning notes here.", "with": nil},
			wants: []string{"operation 1: replace names no with"},
		},
		{
			name:  "a replace carrying a field replace does not take",
			op:    map[string]any{"op": "replace", "find": "Planning notes here.", "with": "Plan.", "after": "Intro."},
			wants: []string{"operation 1: replace carries after, which replace does not take"},
		},
		{
			name:  "a replace carrying markdown in place of with",
			op:    map[string]any{"op": "replace", "find": "Planning notes here.", "markdown": "Plan."},
			wants: []string{"operation 1: replace names no with", "carries markdown, which replace does not take"},
		},
		{
			name:  "a replace carrying a member no operation takes beside its with",
			op:    map[string]any{"op": "replace", "find": "Planning notes here.", "with": "Plan.", "replacement": "Other."},
			wants: []string{"operation 1: replace carries replacement, which replace does not take"},
		},
		{
			name:  "a replace carrying an empty markdown beside its with",
			op:    map[string]any{"op": "replace", "find": "Planning notes here.", "with": "Plan.", "markdown": ""},
			wants: []string{"operation 1: replace carries markdown, which replace does not take"},
		},
		{
			name:  "a replace carrying its new text as text",
			op:    map[string]any{"op": "replace", "find": "Planning notes here.", "text": "Plan."},
			wants: []string{"operation 1: replace names no with", "carries text, which replace does not take"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler := newTestHandler(t)
			issue := createInteractionIssue(t, handler, "TEST", "Replace fields", "Intro.\n\nPlanning notes here.\n\nOutro.\n")
			before, versions := documentMarkdown(t, handler, issue.PrimaryArtifactID), len(documentVersions(t, handler, issue.PrimaryArtifactID))
			response := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
				"ops":   []map[string]any{{"op": "replace", "find": "Intro.", "with": "Opening."}, test.op},
				"actor": sessionActor(),
			})
			body := decodeBody[struct {
				Code  string `json:"code"`
				Error string `json:"error"`
			}](t, response)
			if response.Code != http.StatusBadRequest || body.Code != "REPLACE_FIELDS" {
				t.Fatalf("replace %v: status=%d code=%q error=%q, want 400 REPLACE_FIELDS", test.op, response.Code, body.Code, body.Error)
			}
			for _, want := range test.wants {
				if !strings.Contains(body.Error, want) {
					t.Fatalf("refusal = %q, want it to say %q", body.Error, want)
				}
			}
			if after := documentMarkdown(t, handler, issue.PrimaryArtifactID); after != before {
				t.Fatalf("document after the refused batch = %q, want %q unchanged", after, before)
			}
			if after := len(documentVersions(t, handler, issue.PrimaryArtifactID)); after != versions {
				t.Fatalf("versions after the refused batch = %d, want %d", after, versions)
			}
		})
	}
}

// An explicit with "" is how a caller deletes a replace's match on purpose, and it still does.
func TestAReplaceWithAnEmptyWithDeletesItsMatch(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Empty with", "Intro.\n\nPlanning notes here.\n\nOutro.\n")
	response := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"ops":   []map[string]any{{"op": "replace", "find": " notes", "with": ""}},
		"actor": sessionActor(),
	})
	if response.Code != http.StatusOK {
		t.Fatalf("replace with an empty with: status=%d body=%s", response.Code, response.Body.String())
	}
	if after, want := documentMarkdown(t, handler, issue.PrimaryArtifactID), "Intro.\n\nPlanning here.\n\nOutro.\n"; after != want {
		t.Fatalf("document after the replace = %q, want %q", after, want)
	}
}

// The route authenticates its caller before it reads a replace's members, so a caller with no
// identity is answered 401 NO_IDENTITY for a replace carrying a member replace does not take, as
// for any other edit, and learns nothing of the batch's shape; the document is left as it was.
func TestAnUnauthenticatedReplaceCarryingAForeignMemberIsRefusedForItsIdentity(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "No identity", "Intro.\n\nPlanning notes here.\n\nOutro.\n")
	before := documentMarkdown(t, handler, issue.PrimaryArtifactID)
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"ops": []map[string]any{{"op": "replace", "find": "Planning notes here.", "with": "Plan.", "replacement": "Other."}},
	}, "")
	if response.Code != http.StatusUnauthorized || !strings.Contains(response.Body.String(), `"code":"NO_IDENTITY"`) {
		t.Fatalf("unauthenticated replace carrying replacement: status=%d body=%s, want 401 NO_IDENTITY", response.Code, response.Body.String())
	}
	if after := documentMarkdown(t, handler, issue.PrimaryArtifactID); after != before {
		t.Fatalf("document after the refused edit = %q, want %q unchanged", after, before)
	}
}

// The edit route reads a body as one JSON value, and a closing bracket or brace after it ends the
// read rather than refusing it. The second read of that body, which looks for each replace's
// members, met the bracket as text after the value and answered 500 INTERNAL for a body the route
// had accepted; the replace's empty with deletes its match as it does in any other body.
func TestAnEditBodyEndingInAStrayBracketIsReadAsItsOneValue(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Stray bracket", "Intro.\n\nPlanning notes here.\n\nOutro.\n")
	body := `{"ops":[{"op":"replace","find":" notes","with":""}],"actor":{"kind":"session","id":"session-0123456789abcdef"}}]`
	request := httptest.NewRequest(http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer agent-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("edit whose body ends in a stray bracket: status=%d body=%s, want 200", response.Code, response.Body.String())
	}
	if after, want := documentMarkdown(t, handler, issue.PrimaryArtifactID), "Intro.\n\nPlanning here.\n\nOutro.\n"; after != want {
		t.Fatalf("document after the replace = %q, want %q", after, want)
	}
}

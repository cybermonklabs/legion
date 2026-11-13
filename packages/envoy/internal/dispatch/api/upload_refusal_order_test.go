package api

import (
	"net/http"
	"strings"
	"testing"
)

// uploadRefusal is the code, sentence and live token a refused upload answers. Current is empty
// for every refusal but the 409 a token behind the live document earns.
type uploadRefusal struct {
	Code    string `json:"code"`
	Error   string `json:"error"`
	Current struct {
		Document string `json:"document"`
	} `json:"current"`
}

// An upload route has two refusals that one write can meet: a write with no content over a
// document that exists (400 ARTIFACT_INPUT, blankWriteToADocumentThatExists) and a replacement
// that names no token (400 PRECONDITION_REQUIRED). A blank write over a held name is refused
// whatever token it names, so telling its writer to name one would send them into a second
// refusal; the blank refusal therefore stands ahead of every reading of the token, and each
// refusal names only a remedy this route then takes. Each case drives its refusal, and then the
// remedy the refusal names, to its answer.
func TestUploadRefusalsNameOnlyARemedyTheRouteTakes(t *testing.T) {
	handler := newTestHandler(t)
	issue := createArtifactIssue(t, handler)
	route := "/api/v1/issues/" + issue.Key + "/artifacts"
	post := func(body map[string]any) (int, uploadRefusal, string) {
		t.Helper()
		response := dispatchRequest(t, handler, http.MethodPost, route, body, "alice")
		var refusal uploadRefusal
		if response.Code >= http.StatusBadRequest {
			refusal = decodeBody[uploadRefusal](t, response)
		}
		return response.Code, refusal, response.Body.String()
	}
	token := func(body map[string]any, document string) map[string]any {
		body["precondition"] = map[string]string{"document": document}
		return body
	}

	created := dispatchRequest(t, handler, http.MethodPost, route, map[string]any{
		"name": "notes.md", "content": "# Notes\n\nThe first text.\n",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create notes.md: status=%d body=%s", created.Code, created.Body.String())
	}
	notes := decodeBody[uploadedArtifact](t, created).Artifact.ID
	first := readDocumentPrecondition(t, handler, notes)

	blankRefusal := `"notes.md" names artifact notes-md (notes.md), which exists; a write with no ` +
		`content makes no document and writes no version: edit it with dispatch doc-edit, or give ` +
		`the new document another name`
	// A blank write over a held name: the blank refusal, whether it names no token, the live one
	// or one behind it. The token is never read, and the refusal names no token.
	for _, c := range []struct {
		name string
		body map[string]any
	}{
		{"no token", map[string]any{"name": "notes.md", "content": ""}},
		{"no content at all", map[string]any{"name": "notes.md"}},
		{"the live token", token(map[string]any{"name": "notes.md", "content": " \n"}, first.Token)},
	} {
		code, refusal, raw := post(c.body)
		if code != http.StatusBadRequest || refusal.Code != "ARTIFACT_INPUT" || refusal.Error != blankRefusal {
			t.Fatalf("a blank write over notes.md naming %s: status=%d body=%s, want 400 ARTIFACT_INPUT %q",
				c.name, code, raw, blankRefusal)
		}
		if strings.Contains(refusal.Error, "precondition") || strings.Contains(refusal.Error, "token") {
			t.Fatalf("a blank write over notes.md naming %s is told to name a token: %q", c.name, refusal.Error)
		}
	}

	// Content over a held name naming no token: the token refusal, naming the token.
	code, refusal, raw := post(map[string]any{"name": "notes.md", "content": "# Notes\n\nA second text.\n"})
	if code != http.StatusBadRequest || refusal.Code != "PRECONDITION_REQUIRED" ||
		!strings.Contains(refusal.Error, "must name the token of the version it replaces") {
		t.Fatalf("content over notes.md naming no token: status=%d body=%s, want 400 PRECONDITION_REQUIRED", code, raw)
	}
	if read := readDocumentPrecondition(t, handler, notes); read.Token != first.Token {
		t.Fatalf("a refused write moved notes.md from %s to %s", first.Token, read.Token)
	}
	// That refusal's remedy - name the live token - is taken.
	if code, _, raw := post(token(map[string]any{"name": "notes.md", "content": "# Notes\n\nA second text.\n"}, first.Token)); code != http.StatusCreated {
		t.Fatalf("content over notes.md naming the live token: status=%d body=%s, want 201", code, raw)
	}
	second := readDocumentPrecondition(t, handler, notes)

	// A blank write naming a token the document has moved past is still the blank refusal.
	code, refusal, raw = post(token(map[string]any{"name": "notes.md", "content": ""}, first.Token))
	if code != http.StatusBadRequest || refusal.Code != "ARTIFACT_INPUT" || refusal.Error != blankRefusal {
		t.Fatalf("a blank write over notes.md naming a stale token: status=%d body=%s, want 400 ARTIFACT_INPUT", code, raw)
	}

	// Content over a held name naming a stale token: the 409, with the live token beside it.
	code, refusal, raw = post(token(map[string]any{"name": "notes.md", "content": "# Notes\n\nA third text.\n"}, first.Token))
	if code != http.StatusConflict || refusal.Code != "PRECONDITION_FAILED" || refusal.Current.Document != second.Token {
		t.Fatalf("content over notes.md naming a stale token: status=%d body=%s, want 409 naming %s", code, raw, second.Token)
	}

	// A blank write under a new name is a create, which takes no token.
	if code, _, raw := post(map[string]any{"name": "fresh.md"}); code != http.StatusCreated {
		t.Fatalf("a blank create of fresh.md: status=%d body=%s, want 201", code, raw)
	}
	// One that names a token names a version that is not there: the 409 asking for it again with
	// none, and that remedy is taken.
	code, refusal, raw = post(token(map[string]any{"name": "other.md"}, second.Token))
	if code != http.StatusConflict || refusal.Code != "PRECONDITION_FAILED" ||
		!strings.Contains(refusal.Error, "send it again naming no token") {
		t.Fatalf("a blank create of other.md naming a token: status=%d body=%s, want 409 asking for no token", code, raw)
	}
	if code, _, raw := post(map[string]any{"name": "other.md"}); code != http.StatusCreated {
		t.Fatalf("the blank create of other.md sent again naming no token: status=%d body=%s, want 201", code, raw)
	}
}

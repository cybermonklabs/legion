package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// R3 of dispatch://INFRA-1523 and the architect's ruling of 2026-11-20 20:55Z (N1): a pending
// insert suggestion's words were the post's own and never document text, so once its author
// deletes it, no read of a stored document version gives them, though the version stores them:
// storage keeps the bytes, and the read masks them. A version named while the insert stood pending
// gave the words after the delete, by its id and by the issue's slug (test round at 8259e87d, in
// chromium: GET /api/v1/artifacts/{id}/versions/2, 6 s and 36 s after the delete).
func TestAStoredVersionGivesNoWordOfAPendingInsertDeletedSince(t *testing.T) {
	f := newAnchorsOnAnInsert(t)
	named := dispatchRequest(t, f.handler, http.MethodPost, "/api/v1/artifacts/"+f.artifactID+"/versions",
		map[string]string{"summary": "With the insert pending"}, "alice")
	if named.Code != http.StatusCreated {
		t.Fatalf("name the version holding the pending insert: status=%d body=%s", named.Code, named.Body.String())
	}
	var version struct {
		Number int `json:"number"`
	}
	if err := json.Unmarshal(named.Body.Bytes(), &version); err != nil || version.Number < 2 {
		t.Fatalf("the named version = %s (%v), want a version after the first", named.Body.String(), err)
	}
	versionGivesNoWordOfTheInsertOnceDeleted(t, f, version.Number, "the version")
}

// Census v20 of dispatch://INFRA-1523, V.1: an upload of the text the document already holds
// changes nothing, and its route stores a version all the same (ledger.QueueVersion,
// api/artifacts.go). While a pending insert suggestion stands, that version's markdown holds the
// insert's words, as the live document renders them; its write recorded no operation, so no tree,
// and the version stored no insert tree, which a read answers as stored. The version stores the
// insert tree of the live tree the upload found (docs' unchangedStampOf), so once the insert's
// comment is deleted, no read of it by id or by the issue's slug gives the words.
func TestAnUnchangedUploadsVersionGivesNoWordOfAPendingInsertDeletedSince(t *testing.T) {
	f := newAnchorsOnAnInsert(t)
	text := documentMarkdown(t, f.handler, f.artifactID)
	uploaded := dispatchRequest(t, f.handler, http.MethodPost, "/api/v1/issues/"+f.issueKey+"/artifacts", map[string]any{
		"name": "spec.md", "content": text, "precondition": uploadBase(t, f.handler, f.artifactID),
	}, "alice")
	if uploaded.Code != http.StatusCreated {
		t.Fatalf("upload the document's own text while the insert stands pending: status=%d body=%s", uploaded.Code, uploaded.Body.String())
	}
	version := decodeBody[struct {
		Version struct {
			Number int `json:"number"`
		} `json:"version"`
	}](t, uploaded).Version.Number
	if version < 2 {
		t.Fatalf("the upload stored version %d, want a version after the first: %s", version, uploaded.Body.String())
	}
	// The control: the upload changed nothing, so its version is one an unchanged write stored.
	if after := documentMarkdown(t, f.handler, f.artifactID); after != text {
		t.Fatalf("the upload of the document's own text changed it: %q, want %q", after, text)
	}
	versionGivesNoWordOfTheInsertOnceDeleted(t, f, version, "the version an unchanged upload stored")
}

// versionGivesNoWordOfTheInsertOnceDeleted reads version of f's spec, once it is written, by its id
// and by the issue's slug: each read gives the pending insert's words while the insert stands, and
// none once alice deletes its comment. named is how a failure after the delete names the version.
func versionGivesNoWordOfTheInsertOnceDeleted(t *testing.T, f anchorsOnAnInsert, version int, named string) {
	t.Helper()
	waitForArtifactVersion(t, f.handler, f.artifactID, version)
	reads := map[string]string{
		"by id":   fmt.Sprintf("/api/v1/artifacts/%s/versions/%d", f.artifactID, version),
		"by slug": fmt.Sprintf("/api/v1/issues/%s/artifacts/spec/versions/%d", f.issueKey, version),
	}
	read := func(path string) string {
		t.Helper()
		response := dispatchRequest(t, f.handler, http.MethodGet, path, nil, "bob")
		if response.Code != http.StatusOK {
			t.Fatalf("GET %s: status=%d body=%s", path, response.Code, response.Body.String())
		}
		return response.Body.String()
	}
	// The positive control: while the insert stands pending, each read of the version gives its words.
	for name, path := range reads {
		if body := read(path); !strings.Contains(body, insertedWords) {
			t.Fatalf("the version read %s before the delete gives no %q, so its check after the delete cannot fail: %s", name, insertedWords, body)
		}
	}

	deleteThreadComment(t, f.handler, f.insert.ID, "alice")
	for name, path := range reads {
		if body := read(path); strings.Contains(body, insertedWords) {
			t.Errorf("%s, read %s after the insert's delete, gives its words %q: %s", named, name, insertedWords, body)
		}
	}
}

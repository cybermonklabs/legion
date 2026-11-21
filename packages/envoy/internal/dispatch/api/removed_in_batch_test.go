package api

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// removedInBatchSpec holds one bullet list between two paragraphs.
const removedInBatchSpec = "Intro.\n\n- old one\n- old two\n\nAfter.\n"

// postEdits posts ops to an artifact's edits as the session, and answers the status, the code
// and the error the route served.
func postEdits(t *testing.T, handler http.Handler, artifactID string, ops []map[string]any) (int, string, string) {
	t.Helper()
	response := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+artifactID+"/edits", map[string]any{
		"ops":   ops,
		"actor": sessionActor(),
	})
	body := decodeBody[struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}](t, response)
	return response.Code, body.Code, body.Error
}

// listIDOf is the id of the block GET …/blocks lists whose markdown is the spec's one list.
func listIDOf(t *testing.T, handler http.Handler, artifactID string) string {
	t.Helper()
	for markdown, id := range blockIDsByText(t, handler, artifactID) {
		if strings.TrimSpace(markdown) == "- old one\n- old two" {
			return id
		}
	}
	t.Fatalf("no block of %s is the list", artifactID)
	return ""
}

// An operation whose target an earlier operation of the same edit removed is refused, naming the
// operation, and the document keeps its text and its versions: the replace ahead of both is not
// written either. Each operation reads the document as the operations before it left it, and the
// block the insert's anchor names is gone by then, so the anchor names a block the document does
// not carry, 404 TARGET_NOT_FOUND (dispatch://INFRA-4517). Nothing settles the spec while the row
// runs, so its versions are the ones the row read.
func TestAnOperationWhoseBlockAnEarlierOperationRemovedIsRefused(t *testing.T) {
	handler, _, _ := newTestServer(t, testServerOptions{settle: time.Hour})
	issue := createInteractionIssue(t, handler, "BATCH", "Removed in the batch", removedInBatchSpec)
	list := listIDOf(t, handler, issue.PrimaryArtifactID)
	before, versions := documentMarkdown(t, handler, issue.PrimaryArtifactID), len(documentVersions(t, handler, issue.PrimaryArtifactID))
	status, code, refusal := postEdits(t, handler, issue.PrimaryArtifactID, []map[string]any{
		{"op": "replace", "find": "Intro.", "with": "Opening."},
		{"op": "delete", "block": list},
		{"op": "insert", "after": "block:" + list, "markdown": "- new"},
	})
	if status != http.StatusNotFound || code != "TARGET_NOT_FOUND" {
		t.Fatalf("insert after a block the batch deleted: status=%d code=%q error=%q, want 404 TARGET_NOT_FOUND", status, code, refusal)
	}
	if want := `operation 2: block "` + list + `" is not in the document. Anchor by quote instead: write the text of that block in after, in place of "block:` + list + `".`; refusal != want {
		t.Fatalf("refusal = %q, want %q", refusal, want)
	}
	if after := documentMarkdown(t, handler, issue.PrimaryArtifactID); after != before {
		t.Fatalf("document after the refused batch = %q, want %q unchanged", after, before)
	}
	if after := len(documentVersions(t, handler, issue.PrimaryArtifactID)); after != versions {
		t.Fatalf("versions after the refused batch = %d, want %d", after, versions)
	}
}

// A list inserted after a list's own block, and then that block deleted, in one edit, leaves the
// inserted items where the list stood: the insert joins the list it is anchored on, and the delete
// removes all the list holds but what the insert wrote. Version 336 of
// dispatch://INFRA/artifact/superfan-fans-log-md lost both lists (dispatch://INFRA-4517).
func TestAListInsertedAfterAListTheSameEditDeletesIsKept(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "BATCH", "Kept in the batch", removedInBatchSpec)
	list := listIDOf(t, handler, issue.PrimaryArtifactID)
	if status, code, refusal := postEdits(t, handler, issue.PrimaryArtifactID, []map[string]any{
		{"op": "insert", "after": "block:" + list, "markdown": "- new one\n- new two"},
		{"op": "delete", "block": list},
	}); status != http.StatusOK {
		t.Fatalf("insert after a list, then delete it: status=%d code=%q error=%q, want 200", status, code, refusal)
	}
	if after, want := documentMarkdown(t, handler, issue.PrimaryArtifactID), "Intro.\n\n- new one\n- new two\n\nAfter.\n"; after != want {
		t.Fatalf("document after the batch = %q, want %q", after, want)
	}
}

// A delete naming both a block and a find is refused before either is read, as it always was:
// 400 INVALID_OP on find, whatever the block names, even one the document does not carry.
func TestADeleteNamingBothABlockAndAFindIsRefused(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "BATCH", "Both fields", removedInBatchSpec)
	status, code, refusal := postEdits(t, handler, issue.PrimaryArtifactID, []map[string]any{
		{"op": "delete", "block": "gone", "find": "Intro."},
	})
	if want := `operation 0: invalid document operation field "find": delete takes find or block, not both`; status != http.StatusBadRequest || code != "INVALID_OP" || refusal != want {
		t.Fatalf("a delete naming both fields: status=%d code=%q error=%q, want 400 INVALID_OP %q", status, code, refusal, want)
	}
}

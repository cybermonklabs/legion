package api

import (
	"net/http"
	"strings"
	"testing"
)

// The three checks of bareCheckDocument, by ids of the form Dispatch mints for a check block.
const (
	bareRetyped   = "a5625fc5-e8ae-461f-abbb-63bc1e57f8bd"
	bareUntouched = "c32a4668-d52f-4109-b830-af2ba0c99370"
	bareDeleted   = "55c96f06-3aac-4c73-9785-46ad0a7a50df"
)

// bareCheckDocument is a Verification document of three checks: one an edit retypes and moves,
// one no edit touches, and one an edit deletes.
var bareCheckDocument = ":::check{#" + bareRetyped + ` surface="cli" run="go test ./..." negative_control="Run main's copy."}` + "\n" +
	"The first check, which a retype changes.\n" +
	":::\n\n" +
	":::check{#" + bareUntouched + ` surface="cli" run="go test ./..." negative_control="Break it."}` + "\n" +
	"The second check, which no edit touches.\n" +
	":::\n\n" +
	":::check{#" + bareDeleted + ` surface="cli" run="go test ./..." negative_control="Break it."}` + "\n" +
	"The third check, which an edit deletes.\n" +
	":::\n"

// A check is named by its bare block id whatever a document edit did to another block of the
// same document: a retype of another check's attributes, a move of it, an inserted and a deleted
// block, a deleted check and a new version of the whole document. Each edit is followed by a
// result named by the untouched check's bare id, and by the moved check's while it stands, and
// each is recorded against that check.
//
// Production met the opposite on 24 November: an issue's Verification document of eleven checks,
// whose version 26 retyped one check (a new negative_control), refused a result by another
// check's bare id with UNKNOWN_CHECK twice, though the same bare id had recorded after that
// version and the check's canonical reference recorded at once.
func TestABareCheckIDNamesItsCheckAfterEveryEditOfAnotherBlock(t *testing.T) {
	t.Parallel()
	handler, _, _ := newTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "RBARE", "Bare check ids", "A spec\n")
	artifactID := uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", bareCheckDocument).Artifact.ID
	record := func(after, check string) {
		t.Helper()
		recorded := recordResult(t, handler, issue.Key, resultBody(check, "pass", headCommit, nil))
		if recorded.Code != http.StatusCreated {
			t.Fatalf("after %s, a result named by %s's bare id: status=%d body=%s; want it recorded against that check",
				after, check, recorded.Code, recorded.Body.String())
		}
		if got := decodeBody[recordedResult](t, recorded).Result.CheckBlockID; got != check {
			t.Fatalf("after %s, a result named by %s's bare id is recorded against %s", after, check, got)
		}
	}
	record("the upload", bareUntouched)
	record("the upload", bareRetyped)
	record("the upload", bareDeleted)

	for _, edit := range []struct {
		name  string
		op    map[string]any
		still []string
	}{
		{"a retype of another check", map[string]any{
			"op": "retype", "block": bareRetyped, "type": "check",
			"attributes": map[string]any{"surface": "cli", "run": "go test ./...", "negative_control": "Run a copy of the head's script."},
		}, []string{bareUntouched, bareRetyped}},
		{"a move of another check", map[string]any{"op": "move", "block": bareRetyped, "after": "end"},
			[]string{bareUntouched, bareRetyped}},
		{"an inserted block", map[string]any{"op": "insert", "after": "end", "markdown": "A note beside the checks."},
			[]string{bareUntouched, bareRetyped}},
		{"a deleted block", map[string]any{"op": "delete", "find": "A note beside the checks."},
			[]string{bareUntouched, bareRetyped}},
		{"a deleted check", map[string]any{"op": "delete", "block": bareDeleted},
			[]string{bareUntouched, bareRetyped}},
	} {
		if response := applyOp(t, handler, artifactID, edit.op); response.Code != http.StatusOK {
			t.Fatalf("%s: status=%d body=%s", edit.name, response.Code, response.Body.String())
		}
		for _, check := range edit.still {
			record(edit.name, check)
		}
	}
	gone := recordResult(t, handler, issue.Key, resultBody(bareDeleted, "pass", headCommit, nil))
	if code, _ := refusalOf(t, gone); gone.Code != http.StatusBadRequest || code != "UNKNOWN_CHECK" {
		t.Fatalf("a result named by the deleted check's bare id: status=%d body=%s; want 400 UNKNOWN_CHECK", gone.Code, gone.Body.String())
	}

	replaced := strings.Replace(documentMarkdown(t, handler, artifactID), "The second check, which no edit touches.",
		"The second check, which no edit touches.\n\nA paragraph of a new version.", 1)
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", replaced)
	record("a new version", bareUntouched)
	record("a new version", bareRetyped)
}

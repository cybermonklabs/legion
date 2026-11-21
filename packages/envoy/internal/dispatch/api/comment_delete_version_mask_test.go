package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The rows beside TestAStoredVersionGivesNoWordOfAPendingInsertDeletedSince
// (comment_delete_version_insert_test.go) for the architect's acceptance of
// artifact_versions.insert_tree on dispatch://INFRA-1523 (2026-11-20 21:21Z): a version write
// stores the tree its markdown was rendered from when that tree holds an insert suggestion's mark,
// and every read of the version's text masks the words of a deleted comment's insert by the mark's
// id (docs.MaskedVersionText), unless an accept made them document text. Storage keeps the bytes:
// the version's sha256 and the approval pinned to it stay as they were.

// versionRead is the part of a document version's answer these rows read.
type versionRead struct {
	Markdown string  `json:"markdown"`
	SHA256   *string `json:"sha256"`
	Size     *int    `json:"size"`
}

// readVersionText is the document version at path as bob reads it, and fails t on anything but 200.
func readVersionText(t *testing.T, handler http.Handler, path string) versionRead {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, path, nil, "bob")
	if response.Code != http.StatusOK {
		t.Fatalf("GET %s: status=%d body=%s", path, response.Code, response.Body.String())
	}
	return decodeBody[versionRead](t, response)
}

// nameVersion names the current version of the document artifactID as alice and waits for it to
// be read, as the red test names the version holding the pending insert; it answers the number.
func nameVersion(t *testing.T, handler http.Handler, artifactID, summary string) int {
	t.Helper()
	named := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+artifactID+"/versions",
		map[string]string{"summary": summary}, "alice")
	if named.Code != http.StatusCreated {
		t.Fatalf("name a version of %s: status=%d body=%s", artifactID, named.Code, named.Body.String())
	}
	number := decodeBody[struct {
		Number int `json:"number"`
	}](t, named).Number
	waitForArtifactVersion(t, handler, artifactID, number)
	return number
}

// suggestProjectInsert has a browser mark insertedWords, and the space after them, in the project
// document as an insert suggestion whose mark is markID, which alice then suggests; it answers her
// comment.
func suggestProjectInsert(t *testing.T, handler http.Handler, database *store.Store, documentService *docs.Service, document model.Artifact, markID string) model.Comment {
	t.Helper()
	writeBrowserMark(t, database, documentService, document.ID, docs.MarkSpec{
		Kind:  docs.MarkSuggestion,
		ID:    markID,
		Attrs: pmdoc.Attrs{"id": markID, "by": "user:alice", "kind": "insert"},
	}, insertedWords+" ")
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+document.ID+"/comments", map[string]any{
		"body":       "Add this word.",
		"anchor":     map[string]any{"artifact": document.Slug, "mark_id": markID},
		"suggestion": map[string]string{"replace_with": insertedWords + " "},
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("suggest the insert on the project document: status=%d body=%s", created.Code, created.Body.String())
	}
	return decodeBody[model.Comment](t, created)
}

// storedInsertTree is version number of artifactID's artifact_versions.insert_tree as text, nil
// where the column is null.
func storedInsertTree(t *testing.T, database *store.Store, artifactID string, number int) *string {
	t.Helper()
	var tree *string
	if err := database.Pool.QueryRow(context.Background(), `
		select insert_tree::text from artifact_versions where artifact_id = $1 and number = $2
	`, artifactID, number).Scan(&tree); err != nil {
		t.Fatalf("read the insert tree of version %d of %s: %v", number, artifactID, err)
	}
	return tree
}

// A version write stores the tree its markdown was rendered from when that tree holds an insert
// suggestion's mark, and null when it holds none: the version named while the insert stands
// pending stores a tree naming the insert's mark, and a document no insert ever touched stores
// null at its first version and at a named one.
func TestAVersionStoresAnInsertTreeOnlyWhenItsTreeHoldsAnInsertMark(t *testing.T) {
	f := newAnchorsOnAnInsert(t)
	pending := nameVersion(t, f.handler, f.artifactID, "With the insert pending")
	if tree := storedInsertTree(t, f.database, f.artifactID, pending); tree == nil || !strings.Contains(*tree, `"m-insert"`) {
		t.Fatalf("version %d, named while the insert stood pending, stores insert tree %v, want one naming the mark m-insert", pending, tree)
	}

	plain := createInteractionIssue(t, f.handler, "PLAIN", "No insert", "The quick brown fox jumps")
	named := nameVersion(t, f.handler, plain.PrimaryArtifactID, "Nothing pending")
	for _, number := range []int{1, named} {
		if tree := storedInsertTree(t, f.database, plain.PrimaryArtifactID, number); tree != nil {
			t.Errorf("version %d of a document holding no insert suggestion stores insert tree %s, want null", number, *tree)
		}
	}
}

// An accepted insert's words are document text: the accept takes the suggestion's mark off them
// and keeps them, so the version named while the insert stood pending still gives them after its
// comment's delete. The delete clears the suggestion column; what the mask reads of the accept is
// the version the accept named, which records the comment (artifact_versions.accepted_from).
func TestAStoredVersionKeepsTheWordsOfAnAcceptedInsertAfterItsDelete(t *testing.T) {
	f := newAnchorsOnAnInsert(t)
	pending := nameVersion(t, f.handler, f.artifactID, "With the insert pending")
	// The positive control: the version stores the insert's tree, so the mask reads its mark and
	// keeps the words only for the accept.
	if tree := storedInsertTree(t, f.database, f.artifactID, pending); tree == nil {
		t.Fatalf("version %d stores no insert tree, so its words stay whatever the mask reads of the accept", pending)
	}
	if accepted := dispatchRequest(t, f.handler, http.MethodPost, "/api/v1/comments/"+f.insert.ID+"/accept", map[string]any{}, "alice"); accepted.Code != http.StatusOK {
		t.Fatalf("accept the insert: status=%d body=%s", accepted.Code, accepted.Body.String())
	}

	deleteThreadComment(t, f.handler, f.insert.ID, "alice")
	path := fmt.Sprintf("/api/v1/artifacts/%s/versions/%d", f.artifactID, pending)
	if read := readVersionText(t, f.handler, path); !strings.Contains(read.Markdown, insertedWords) {
		t.Errorf("the version named while the accepted insert stood pending gives no %q after the comment's delete: %q", insertedWords, read.Markdown)
	}
}

// The mask is a read's: the delete rewrites no stored version, so the version's sha256 - the
// stored text's - and the approval pinned to that text are the same after the delete as before,
// while the version's text loses the insert's words.
func TestADeletedInsertLeavesTheVersionsSHA256AndItsApprovalAsStored(t *testing.T) {
	f := newAnchorsOnAnInsert(t)
	pending := nameVersion(t, f.handler, f.artifactID, "With the insert pending")
	approved := dispatchRequest(t, f.handler, http.MethodPost, versionReviewPath(f.artifactID, strconv.Itoa(pending)),
		map[string]string{"state": "approved"}, "alice")
	if approved.Code != http.StatusCreated {
		t.Fatalf("approve version %d: status=%d body=%s", pending, approved.Code, approved.Body.String())
	}
	review := decodeBody[model.ArtifactReview](t, approved)
	path := fmt.Sprintf("/api/v1/artifacts/%s/versions/%d", f.artifactID, pending)
	before := readVersionText(t, f.handler, path)
	stored := sha256.Sum256([]byte(before.Markdown))
	if before.SHA256 == nil || *before.SHA256 != hex.EncodeToString(stored[:]) {
		t.Fatalf("version %d before the delete answers sha256 %v, want the sha256 of the text it answers", pending, before.SHA256)
	}
	if review.ContentSHA256 == nil || *review.ContentSHA256 != *before.SHA256 {
		t.Fatalf("the approval of version %d is pinned to %v, want the version's sha256 %s", pending, review.ContentSHA256, *before.SHA256)
	}
	reviewsBefore := artifactReviewsJSON(t, f.handler, f.artifactID, review.ID)

	deleteThreadComment(t, f.handler, f.insert.ID, "alice")
	after := readVersionText(t, f.handler, path)
	// The positive control: the read masked the version, so its sha256 had a reason to move.
	if strings.Contains(after.Markdown, insertedWords) || after.Markdown == before.Markdown {
		t.Fatalf("version %d after the delete answers %q, want its text without %q", pending, after.Markdown, insertedWords)
	}
	if after.SHA256 == nil || *after.SHA256 != *before.SHA256 {
		t.Errorf("version %d's sha256 moved across the delete: %v, want %s, the stored text's", pending, after.SHA256, *before.SHA256)
	}
	if after.Size == nil || before.Size == nil || *after.Size != *before.Size {
		t.Errorf("version %d's size moved across the delete: %v, want %v", pending, after.Size, before.Size)
	}
	if reviewsAfter := artifactReviewsJSON(t, f.handler, f.artifactID, review.ID); reviewsAfter != reviewsBefore {
		t.Errorf("the approval of version %d moved across the delete:\n\tbefore %s\n\tafter  %s", pending, reviewsBefore, reviewsAfter)
	}
}

// artifactReviewsJSON is review id as GET /api/v1/artifacts/{id}/reviews lists it, encoded, and
// fails t when the list holds no such review.
func artifactReviewsJSON(t *testing.T, handler http.Handler, artifactID, id string) string {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+artifactID+"/reviews", nil, "bob")
	if response.Code != http.StatusOK {
		t.Fatalf("list the reviews of %s: status=%d body=%s", artifactID, response.Code, response.Body.String())
	}
	for _, review := range decodeBody[[]model.ArtifactReview](t, response) {
		if review.ID != id {
			continue
		}
		encoded, err := json.Marshal(review)
		if err != nil {
			t.Fatalf("encode review %s: %v", id, err)
		}
		return string(encoded)
	}
	t.Fatalf("the reviews of %s hold no review %s: %s", artifactID, id, response.Body.String())
	return ""
}

// A project's document takes a comment's insert suggestion as an issue's does, and the version
// read by the project's slug masks it the same way: one read serves every version route.
func TestAProjectDocumentsVersionGivesNoWordOfAPendingInsertDeletedSince(t *testing.T) {
	documentService, handler, database := browserDocumentService(t)
	createTestProject(t, handler, "CORE")
	document := createProjectDocument(t, handler, "CORE", "Design notes", "The "+insertedWords+" brown fox jumps\n")
	insert := suggestProjectInsert(t, handler, database, documentService, document, "m-insert")
	pending := nameVersion(t, handler, document.ID, "With the insert pending")
	path := fmt.Sprintf("/api/v1/projects/CORE/artifacts/%s/versions/%d", document.Slug, pending)
	// The positive control: while the insert stands pending, the version gives its words.
	if read := readVersionText(t, handler, path); !strings.Contains(read.Markdown, insertedWords) {
		t.Fatalf("the project document's version before the delete gives no %q, so its check after the delete cannot fail: %q", insertedWords, read.Markdown)
	}

	deleteThreadComment(t, handler, insert.ID, "alice")
	if read := readVersionText(t, handler, path); strings.Contains(read.Markdown, insertedWords) {
		t.Errorf("the project document's version after the insert's delete gives its words %q: %q", insertedWords, read.Markdown)
	}
}

// The diff a named version's artifact.version event carries is read from the version before it as
// every read of that version answers it (transactionVersionMarkdown): a version named after the
// delete writes no word of the insert into its event, though the version before it stores them.
// The delete writes the document's next version itself (docs.SettleVersion), so this row leaves
// that version to the settlement (server.deleteLeavesSettlement), as a build without the settle
// does when it serves the delete during a rollout: the version before the named one is then the
// one holding the insert.
func TestAVersionNamedAfterTheDeleteWritesNoWordOfTheInsertIntoItsDiff(t *testing.T) {
	f := newAnchorsOnAnInsert(t)
	pending := nameVersion(t, f.handler, f.artifactID, "With the insert pending")
	f.server.deleteLeavesSettlement = true
	deleteThreadComment(t, f.handler, f.insert.ID, "alice")
	next := nameVersion(t, f.handler, f.artifactID, "After the delete")
	if next != pending+1 {
		t.Fatalf("the version named after the delete is %d, want %d, so its diff is read from the version holding the insert", next, pending+1)
	}
	// The positive control: the version the diff is read from stores the words.
	if markdown := versionText(t, f.database, f.artifactID, pending); !strings.Contains(markdown, insertedWords) {
		t.Fatalf("version %d stores %q, want it holding %q, so the diff's check cannot fail", pending, markdown, insertedWords)
	}
	var diff *string
	err := f.database.Pool.QueryRow(context.Background(), `
		select payload->>'diff' from events
		where type = 'artifact.version' and payload->>'artifact_id' = $1 and (payload->'version'->>'number')::int = $2
	`, f.artifactID, next).Scan(&diff)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && diff == nil) {
		t.Fatalf("version %d's artifact.version event carries no diff (%v), so its check cannot fail", next, err)
	}
	if err != nil {
		t.Fatalf("read version %d's artifact.version event: %v", next, err)
	}
	if strings.Contains(*diff, insertedWords) {
		t.Errorf("version %d's artifact.version event carries a diff giving %q: %s", next, insertedWords, *diff)
	}
}

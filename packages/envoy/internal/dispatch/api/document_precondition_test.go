package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

type documentPreconditionRead struct {
	Markdown string `json:"markdown"`
	Token    string `json:"token"`
}

type documentPreconditionBlock struct {
	ID    string `json:"id"`
	Token string `json:"token"`
}

type documentPreconditionMismatch struct {
	Scope    string  `json:"scope"`
	BlockID  string  `json:"block_id,omitempty"`
	Expected string  `json:"expected"`
	Current  *string `json:"current"`
}

type documentPreconditionConflict struct {
	Code    string `json:"code"`
	Current struct {
		Document string                      `json:"document"`
		Blocks   []documentPreconditionBlock `json:"blocks"`
	} `json:"current"`
	Error           string                         `json:"error"`
	Mismatches      []documentPreconditionMismatch `json:"mismatches"`
	UnguardedBlocks []model.EditBlockPrecondition  `json:"unguarded_blocks"`
}

// preconditionTestHandler is newTestServer with settlement held back for the test's duration.
func preconditionTestHandler(t *testing.T) (http.Handler, *store.Store, docs.API) {
	t.Helper()
	handler, database, deps := newTestServer(t, testServerOptions{settle: time.Hour})
	return handler, database, deps.Docs
}

// readDocumentPrecondition is the text read of a document and the token it carries now, which
// an upload replacing it names (uploadBase). It takes a benchmark's TB as well as a test's,
// since the upload benchmark reads its spec's token through it.
func readDocumentPrecondition(t testing.TB, handler http.Handler, artifactID string) documentPreconditionRead {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+artifactID+"/text", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read document: status=%d body=%s", response.Code, response.Body.String())
	}
	read := decodeBody[documentPreconditionRead](t, response)
	if read.Token == "" {
		t.Fatal("document read omitted its optimistic-concurrency token")
	}
	return read
}

func readDocumentPreconditionBlocks(t *testing.T, handler http.Handler, artifactID string) []documentPreconditionBlock {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+artifactID+"/blocks", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read blocks: status=%d body=%s", response.Code, response.Body.String())
	}
	blocks := decodeBody[[]documentPreconditionBlock](t, response)
	for _, block := range blocks {
		if block.ID == "" || block.Token == "" {
			t.Fatalf("block precondition token = %#v, want id and token", block)
		}
	}
	return blocks
}

func documentMutationCounts(t *testing.T, database *store.Store, artifactID string) (updates, versions, events int) {
	t.Helper()
	if err := database.Pool.QueryRow(context.Background(), `
		select
			(select count(*) from doc_updates where artifact_id = $1),
			(select count(*) from artifact_versions where artifact_id = $1),
			(select count(*) from events)
	`, artifactID).Scan(&updates, &versions, &events); err != nil {
		t.Fatalf("count document mutation rows: %v", err)
	}
	return updates, versions, events
}

func TestDocumentEditPreconditionRejectsStaleDocumentWithoutMutation(t *testing.T) {
	t.Parallel()
	handler, database, _ := preconditionTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Document precondition", "before")

	stale := readDocumentPrecondition(t, handler, issue.PrimaryArtifactID)
	writer := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"ops": []map[string]string{{"op": "replace", "find": "before", "with": "writer"}},
	}, "alice")
	if writer.Code != http.StatusOK {
		t.Fatalf("write current document: status=%d body=%s", writer.Code, writer.Body.String())
	}
	current := readDocumentPrecondition(t, handler, issue.PrimaryArtifactID)
	updatesBefore, versionsBefore, eventsBefore := documentMutationCounts(t, database, issue.PrimaryArtifactID)

	rejected := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"ops":          []map[string]string{{"op": "replace", "find": "writer", "with": "late"}},
		"precondition": map[string]string{"document": stale.Token},
	}, "alice")
	if rejected.Code != http.StatusConflict {
		t.Fatalf("stale precondition: status=%d body=%s", rejected.Code, rejected.Body.String())
	}
	conflict := decodeBody[documentPreconditionConflict](t, rejected)
	if conflict.Code != "PRECONDITION_FAILED" || conflict.Current.Document != current.Token || len(conflict.Mismatches) != 1 || conflict.Mismatches[0].Scope != "document" || conflict.Mismatches[0].Expected != stale.Token || conflict.Mismatches[0].Current == nil || *conflict.Mismatches[0].Current != current.Token {
		t.Fatalf("stale precondition conflict = %#v", conflict)
	}
	if got := readDocumentPrecondition(t, handler, issue.PrimaryArtifactID); got.Markdown != "writer\n" {
		t.Fatalf("document after rejected edit = %q, want writer", got.Markdown)
	}
	updatesAfter, versionsAfter, eventsAfter := documentMutationCounts(t, database, issue.PrimaryArtifactID)
	if updatesAfter != updatesBefore || versionsAfter != versionsBefore || eventsAfter != eventsBefore {
		t.Fatalf("rejected precondition mutated updates/versions/events: got %d/%d/%d, want %d/%d/%d", updatesAfter, versionsAfter, eventsAfter, updatesBefore, versionsBefore, eventsBefore)
	}

	fresh := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"ops":          []map[string]string{{"op": "replace", "find": "writer", "with": "fresh"}},
		"precondition": map[string]string{"document": current.Token},
	}, "alice")
	if fresh.Code != http.StatusOK {
		t.Fatalf("fresh precondition: status=%d body=%s", fresh.Code, fresh.Body.String())
	}
	if got := readDocumentPrecondition(t, handler, issue.PrimaryArtifactID); got.Markdown != "fresh\n" {
		t.Fatalf("document after fresh precondition = %q, want fresh", got.Markdown)
	}
}
func TestDocumentEditPreconditionChecksEmptyBatch(t *testing.T) {
	t.Parallel()
	handler, database, _ := preconditionTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Empty precondition", "before")
	stale := readDocumentPrecondition(t, handler, issue.PrimaryArtifactID)
	if changed := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"ops": []map[string]string{{"op": "replace", "find": "before", "with": "after"}},
	}, "alice"); changed.Code != http.StatusOK {
		t.Fatalf("change document: status=%d body=%s", changed.Code, changed.Body.String())
	}
	updatesBefore, versionsBefore, eventsBefore := documentMutationCounts(t, database, issue.PrimaryArtifactID)
	rejected := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"ops":          []any{},
		"precondition": map[string]string{"document": stale.Token},
	}, "alice")
	if rejected.Code != http.StatusConflict || !strings.Contains(rejected.Body.String(), `"code":"PRECONDITION_FAILED"`) {
		t.Fatalf("stale empty precondition: status=%d body=%s", rejected.Code, rejected.Body.String())
	}
	updatesAfter, versionsAfter, eventsAfter := documentMutationCounts(t, database, issue.PrimaryArtifactID)
	if updatesAfter != updatesBefore || versionsAfter != versionsBefore || eventsAfter != eventsBefore {
		t.Fatalf("stale empty precondition mutated updates/versions/events: got %d/%d/%d, want %d/%d/%d", updatesAfter, versionsAfter, eventsAfter, updatesBefore, versionsBefore, eventsBefore)
	}
	fresh := readDocumentPrecondition(t, handler, issue.PrimaryArtifactID)
	accepted := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"ops":          []any{},
		"precondition": map[string]string{"document": fresh.Token},
	}, "alice")
	if accepted.Code != http.StatusOK || !strings.Contains(accepted.Body.String(), `"applied":0`) {
		t.Fatalf("fresh empty precondition: status=%d body=%s", accepted.Code, accepted.Body.String())
	}
	// The check applied no operation, so the document it read is the one the caller's next
	// guarded edit meets: the response carries that token, not an empty one.
	if token := decodeBody[documentEditResult](t, accepted).Token; token != fresh.Token {
		t.Fatalf("checked empty batch token = %q, want the document's current token %q", token, fresh.Token)
	}
	// Under a block guard there is no document token in the request to echo back, and the answer
	// is still the whole-document token the next guarded edit needs.
	blocks := readDocumentPreconditionBlocks(t, handler, issue.PrimaryArtifactID)
	guarded := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"ops":          []any{},
		"precondition": map[string]any{"blocks": []map[string]string{{"id": blocks[0].ID, "token": blocks[0].Token}}},
	}, "alice")
	if guarded.Code != http.StatusOK {
		t.Fatalf("block-guarded empty precondition: status=%d body=%s", guarded.Code, guarded.Body.String())
	}
	if token := decodeBody[documentEditResult](t, guarded).Token; token != fresh.Token {
		t.Fatalf("block-guarded empty batch token = %q, want the document's current token %q", token, fresh.Token)
	}
	updatesFinal, versionsFinal, eventsFinal := documentMutationCounts(t, database, issue.PrimaryArtifactID)
	if updatesFinal != updatesBefore || versionsFinal != versionsBefore || eventsFinal != eventsBefore {
		t.Fatalf("fresh empty precondition mutated updates/versions/events: got %d/%d/%d, want %d/%d/%d", updatesFinal, versionsFinal, eventsFinal, updatesBefore, versionsBefore, eventsBefore)
	}
}

type documentEditResult struct {
	Changed bool   `json:"changed"`
	Token   string `json:"token"`
}

func guardedDocumentEdit(t *testing.T, handler http.Handler, artifactID, find, with, token string) documentEditResult {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+artifactID+"/edits", map[string]any{
		"ops":          []map[string]string{{"op": "replace", "find": find, "with": with}},
		"precondition": map[string]string{"document": token},
	}, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("guarded edit %q -> %q: status=%d body=%s", find, with, response.Code, response.Body.String())
	}
	return decodeBody[documentEditResult](t, response)
}

// A chain of guarded edits reads the document once: each edit returns the token of the tree its
// own transaction wrote, which is the next edit's precondition, so no edit re-reads the document
// to learn a token the edit before it just minted.
func TestDocumentEditReturnsTheTokenItProduced(t *testing.T) {
	t.Parallel()
	handler, _, _ := preconditionTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Chained document edits", "before")

	start := readDocumentPrecondition(t, handler, issue.PrimaryArtifactID)
	first := guardedDocumentEdit(t, handler, issue.PrimaryArtifactID, "before", "first", start.Token)
	if first.Token == "" || first.Token == start.Token {
		t.Fatalf("first edit token = %q, want the token it produced, not the one it was given (%q)", first.Token, start.Token)
	}
	// No read between the two edits: the second guards on what the first returned.
	second := guardedDocumentEdit(t, handler, issue.PrimaryArtifactID, "first", "second", first.Token)
	current := readDocumentPrecondition(t, handler, issue.PrimaryArtifactID)
	if current.Markdown != "second\n" {
		t.Fatalf("document after chained guarded edits = %q, want second", current.Markdown)
	}
	if second.Token != current.Token {
		t.Fatalf("second edit token = %q, want the document's own token %q", second.Token, current.Token)
	}

	// A batch that changes nothing returns the token the document still carries, so the chain
	// continues through it.
	unchanged := guardedDocumentEdit(t, handler, issue.PrimaryArtifactID, "second", "second", second.Token)
	if unchanged.Changed || unchanged.Token != current.Token {
		t.Fatalf("unchanged batch = %#v, want changed=false and the current token %q", unchanged, current.Token)
	}
	third := guardedDocumentEdit(t, handler, issue.PrimaryArtifactID, "second", "third", unchanged.Token)
	if third.Token == unchanged.Token {
		t.Fatalf("third edit token = %q, want a token of its own", third.Token)
	}
	if got := readDocumentPrecondition(t, handler, issue.PrimaryArtifactID); got.Markdown != "third\n" || got.Token != third.Token {
		t.Fatalf("document after the chain = %#v, want third and token %q", got, third.Token)
	}
}

func TestDocumentEditWithoutPreconditionRemainsUnconditional(t *testing.T) {
	t.Parallel()
	handler, _, _ := preconditionTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Unconditional document edit", "before")

	for _, edit := range []map[string]string{
		{"op": "replace", "find": "before", "with": "first"},
		{"op": "replace", "find": "first", "with": "second"},
	} {
		response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
			"ops": []map[string]string{edit},
		}, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("unconditional edit %q: status=%d body=%s", edit["with"], response.Code, response.Body.String())
		}
	}
	if got := readDocumentPrecondition(t, handler, issue.PrimaryArtifactID); got.Markdown != "second\n" {
		t.Fatalf("unconditional edits = %q, want second", got.Markdown)
	}
}

func TestDocumentEditBlockPreconditionsAllowIndependentEdits(t *testing.T) {
	t.Parallel()
	handler, _, _ := preconditionTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Independent blocks", "alpha\n\nbeta")
	blocks := readDocumentPreconditionBlocks(t, handler, issue.PrimaryArtifactID)
	if len(blocks) != 2 {
		t.Fatalf("blocks = %#v, want alpha and beta", blocks)
	}

	first := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"ops":          []map[string]string{{"op": "replace", "find": "alpha", "with": "ALPHA"}},
		"precondition": map[string]any{"blocks": []map[string]string{{"id": blocks[0].ID, "token": blocks[0].Token}}},
	}, "alice")
	if first.Code != http.StatusOK {
		t.Fatalf("first independent edit: status=%d body=%s", first.Code, first.Body.String())
	}
	stale := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"ops":          []map[string]string{{"op": "replace", "find": "ALPHA", "with": "late"}},
		"precondition": map[string]any{"blocks": []map[string]string{{"id": blocks[0].ID, "token": blocks[0].Token}}},
	}, "alice")
	if stale.Code != http.StatusConflict {
		t.Fatalf("stale block precondition: status=%d body=%s", stale.Code, stale.Body.String())
	}
	conflict := decodeBody[documentPreconditionConflict](t, stale)
	if len(conflict.Mismatches) != 1 || conflict.Mismatches[0].Scope != "block" || conflict.Mismatches[0].BlockID != blocks[0].ID || conflict.Mismatches[0].Expected != blocks[0].Token || conflict.Mismatches[0].Current == nil || len(conflict.Current.Blocks) != 1 || conflict.Current.Blocks[0].ID != blocks[0].ID || conflict.Current.Blocks[0].Token != *conflict.Mismatches[0].Current {
		t.Fatalf("stale block conflict = %#v", conflict)
	}
	second := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"ops":          []map[string]string{{"op": "replace", "find": "beta", "with": "BETA"}},
		"precondition": map[string]any{"blocks": []map[string]string{{"id": blocks[1].ID, "token": blocks[1].Token}}},
	}, "alice")
	if second.Code != http.StatusOK {
		t.Fatalf("second independent edit: status=%d body=%s", second.Code, second.Body.String())
	}
	if got := readDocumentPrecondition(t, handler, issue.PrimaryArtifactID); got.Markdown != "ALPHA\n\nBETA\n" {
		t.Fatalf("independent edits = %q, want both edits", got.Markdown)
	}
}

func TestDocumentEditBlockPreconditionMustCoverEveryTouchedBlock(t *testing.T) {
	t.Parallel()
	handler, _, _ := preconditionTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Covered blocks", "guard\n\ntarget old")
	blocks := readDocumentPreconditionBlocks(t, handler, issue.PrimaryArtifactID)
	if len(blocks) != 2 {
		t.Fatalf("blocks = %#v, want guard and target", blocks)
	}
	writer := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"ops": []map[string]string{{"op": "replace", "find": "old", "with": "changed"}},
	}, "alice")
	if writer.Code != http.StatusOK {
		t.Fatalf("change target block: status=%d body=%s", writer.Code, writer.Body.String())
	}

	uncovered := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"ops":          []map[string]string{{"op": "replace", "find": "changed", "with": "late"}},
		"precondition": map[string]any{"blocks": []map[string]string{{"id": blocks[0].ID, "token": blocks[0].Token}}},
	}, "alice")
	if uncovered.Code != http.StatusBadRequest || !strings.Contains(uncovered.Body.String(), `"code":"INVALID_PRECONDITION"`) || !strings.Contains(uncovered.Body.String(), blocks[1].ID) {
		t.Fatalf("uncovered block precondition: status=%d body=%s", uncovered.Code, uncovered.Body.String())
	}
	if got := readDocumentPrecondition(t, handler, issue.PrimaryArtifactID); got.Markdown != "guard\n\ntarget changed\n" {
		t.Fatalf("uncovered edit changed document = %q, want writer state", got.Markdown)
	}
}

func TestDocumentEditContainerDeleteUsesItsOwnBlockToken(t *testing.T) {
	t.Parallel()
	handler, _, _ := preconditionTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Container token", "- Parent\n  - Child")
	blocks := readDocumentPreconditionBlocks(t, handler, issue.PrimaryArtifactID)
	if len(blocks) == 0 {
		t.Fatal("container document has no blocks")
	}
	parent := blocks[0]
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"ops":          []map[string]string{{"op": "delete", "block": parent.ID}},
		"precondition": map[string]any{"blocks": []map[string]string{{"id": parent.ID, "token": parent.Token}}},
	}, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("container delete with parent token: status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestDocumentPreconditionRejectsAnchorAddedAfterRead(t *testing.T) {
	t.Parallel()
	handler, _, _ := preconditionTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Anchor freshness", "The quick brown fox")
	stale := readDocumentPrecondition(t, handler, issue.PrimaryArtifactID)
	beforeBlocks := readDocumentPreconditionBlocks(t, handler, issue.PrimaryArtifactID)
	comment := decodeBody[model.Comment](t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body":   "Please clarify brown.",
		"anchor": map[string]string{"artifact": "spec", "quote": "brown"},
	}, "alice"))
	current := readDocumentPrecondition(t, handler, issue.PrimaryArtifactID)
	currentBlocks := readDocumentPreconditionBlocks(t, handler, issue.PrimaryArtifactID)
	if current.Token == stale.Token {
		t.Fatal("adding an anchored comment did not change the document token")
	}
	if len(beforeBlocks) != 1 || len(currentBlocks) != 1 || beforeBlocks[0].Token == currentBlocks[0].Token {
		t.Fatalf("anchored block token = before %#v, after %#v; want a changed single block token", beforeBlocks, currentBlocks)
	}

	rejected := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"ops":          []map[string]string{{"op": "replace", "find": "brown", "with": "BROWN"}},
		"precondition": map[string]string{"document": stale.Token},
	}, "alice")
	if rejected.Code != http.StatusConflict || !strings.Contains(rejected.Body.String(), `"code":"PRECONDITION_FAILED"`) {
		t.Fatalf("stale anchor precondition: status=%d body=%s", rejected.Code, rejected.Body.String())
	}
	listed := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/comments?artifact="+issue.PrimaryArtifactID, nil, "alice")
	if listed.Code != http.StatusOK {
		t.Fatalf("list comments after rejected edit: status=%d body=%s", listed.Code, listed.Body.String())
	}
	comments := decodeBody[[]model.Comment](t, listed)
	if len(comments) != 1 || comments[0].ID != comment.ID || comments[0].Anchor == nil || comments[0].Anchor.Orphaned {
		t.Fatalf("anchor after rejected edit = %#v, want live comment %s", comments, comment.ID)
	}
}

func TestDocumentEditPreconditionRejectsWriterAfterDurableAppendRace(t *testing.T) {
	t.Parallel()
	service, handler, database := browserDocumentService(t)
	issue := createInteractionIssue(t, handler, "TEST", "Precondition race", "before")
	stale := readDocumentPrecondition(t, handler, issue.PrimaryArtifactID)

	blocker, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin document lock holder: %v", err)
	}
	defer blocker.Rollback(context.Background())
	if _, err := blocker.Exec(context.Background(), `select pg_advisory_xact_lock(hashtext($1))`, issue.PrimaryArtifactID); err != nil {
		t.Fatalf("lock document room: %v", err)
	}

	// A browser's edit reaches the room at once, and its durable append waits for the room's
	// lock: the one writer that does not first take the document's owner row, as every server
	// write and the conditional edit below do.
	writeBrowserDocument(t, service, issue.PrimaryArtifactID, "writer")
	waitForDatabaseLocks(t, blocker, 1)

	conditionalDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		conditionalDone <- dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
			"ops":          []map[string]string{{"op": "replace", "find": "writer", "with": "late"}},
			"precondition": map[string]string{"document": stale.Token},
		}, "alice")
	}()
	waitForDatabaseLocks(t, blocker, 2)
	if err := blocker.Commit(context.Background()); err != nil {
		t.Fatalf("release document lock: %v", err)
	}
	response := awaitResponse(t, conditionalDone)
	if response.Code != http.StatusConflict {
		t.Fatalf("late conditional writer: status=%d body=%s", response.Code, response.Body.String())
	}
	conflict := decodeBody[documentPreconditionConflict](t, response)
	if conflict.Code != "PRECONDITION_FAILED" || conflict.Current.Document == stale.Token {
		t.Fatalf("race conflict = %#v, want current token after writer", conflict)
	}
	if got := readDocumentPrecondition(t, handler, issue.PrimaryArtifactID); got.Markdown != "writer\n" {
		t.Fatalf("race document = %q, want writer only", got.Markdown)
	}
}

// A `replace` that writes blocks is a conditional edit like any other (INFRA-1520 R9): a `blocks`
// precondition naming the matched block's current token takes it, the blocks it adds need no token
// of their own, having no earlier version to name, and a stale token for that block refuses it with
// nothing written. A `replace` that parses to one paragraph answers the same way.
func TestDocumentEditBlockPreconditionCoversAReplaceThatWritesBlocks(t *testing.T) {
	t.Parallel()
	for _, test := range []struct{ name, with, want string }{
		{
			name: "a with that writes blocks",
			with: "Here is the plan:\n\n- first\n- second",
			want: "guard\n\nHere is the plan:\n\n- first\n- second\n",
		},
		{name: "a with of one paragraph", with: "rewritten", want: "guard\n\nrewritten\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler, _, _ := preconditionTestHandler(t)
			issue := createInteractionIssue(t, handler, "TEST", "Conditional replace", "guard\n\ntarget")
			blocks := readDocumentPreconditionBlocks(t, handler, issue.PrimaryArtifactID)
			if len(blocks) != 2 {
				t.Fatalf("blocks = %#v, want guard and target", blocks)
			}
			target := blocks[1]
			stale := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
				"ops":          []map[string]string{{"op": "replace", "find": "target", "with": test.with}},
				"precondition": map[string]any{"blocks": []map[string]string{{"id": target.ID, "token": "sha256:" + strings.Repeat("0", 64)}}},
			}, "alice")
			if stale.Code != http.StatusConflict || !strings.Contains(stale.Body.String(), `"code":"PRECONDITION_FAILED"`) {
				t.Fatalf("stale token: status=%d body=%s, want 409 PRECONDITION_FAILED", stale.Code, stale.Body.String())
			}
			t.Logf("the precondition sent named %s with a stale token, and the answer was %s", target.ID, stale.Body.String())
			if got := readDocumentPrecondition(t, handler, issue.PrimaryArtifactID); got.Markdown != "guard\n\ntarget\n" {
				t.Fatalf("the refused edit left %q, want the document unchanged", got.Markdown)
			}
			applied := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
				"ops":          []map[string]string{{"op": "replace", "find": "target", "with": test.with}},
				"precondition": map[string]any{"blocks": []map[string]string{{"id": target.ID, "token": target.Token}}},
			}, "alice")
			if applied.Code != http.StatusOK {
				t.Fatalf("current token: status=%d body=%s", applied.Code, applied.Body.String())
			}
			t.Logf("the precondition sent named %s at its current token, and the answer was %s", target.ID, applied.Body.String())
			if got := readDocumentPrecondition(t, handler, issue.PrimaryArtifactID); got.Markdown != test.want {
				t.Fatalf("the conditional edit wrote %q, want %q", got.Markdown, test.want)
			}
		})
	}
}

// A delete whose find opens with a list marker removes the whole item it selects, the items
// nested in it included. A block guard must name what it always had to, the item's paragraph,
// whose text the find matched, and a guard on that paragraph alone leaves out the nested list the
// delete removes. The caller lists the item's paragraph and its nested list, and another writer
// then changes the nested item. The delete guarded on the paragraph alone is refused 409
// PRECONDITION_FAILED with nothing written: its unguarded_blocks names the nested list at the
// token it carries now, which differs from the one the caller listed, and its error names the
// list's id. The caller then adds the list to its guard at the token its own read listed, the
// only token the refusal ever has it send, and the delete is refused again on that list's stale
// token, with the other writer's change still in the document (INFRA-341).
func TestAMarkerDeleteGuardedOnItsParagraphAloneAfterANestedChangeIsRefusedThenStaleWithTheListAsRead(t *testing.T) {
	t.Parallel()
	handler, _, _ := preconditionTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Marker delete guard", "- Parent\n  - Child\n- Other\n")
	artifact := issue.PrimaryArtifactID
	deleteParent := map[string]string{"op": "delete", "find": "- Parent"}
	parent := listedBlock(t, handler, artifact, "paragraph", "Parent")
	nested := listedBlock(t, handler, artifact, "bullet_list", "- Child")
	writer := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+artifact+"/edits", map[string]any{
		"ops": []map[string]string{{"op": "replace", "find": "Child", "with": "Child, changed by another writer"}},
	}, "alice")
	if writer.Code != http.StatusOK {
		t.Fatalf("the other writer's change: status=%d body=%s", writer.Code, writer.Body.String())
	}
	changed := readDocumentPrecondition(t, handler, artifact)
	changedList := listedBlock(t, handler, artifact, "bullet_list", "- Child, changed by another writer")
	if changedList.ID != nested.ID || changedList.Token == nested.Token {
		t.Fatalf("the nested list after the other writer's change = %#v, want %s at a token other than the %s the caller read",
			changedList, nested.ID, nested.Token)
	}
	response := blockGuardedEdit(t, handler, artifact, deleteParent, parent)
	if !refusedUnguarded(t, response, changed.Token, changedList) {
		t.Fatalf("a marker delete guarded on the item's paragraph alone, after a nested item changed: status=%d body=%s, "+
			"want 409 PRECONDITION_FAILED naming the nested list %s at its token now, %s", response.Code, response.Body.String(), changedList.ID, changedList.Token)
	}
	again := blockGuardedEdit(t, handler, artifact, deleteParent, parent, nested)
	if !refusedStale(t, again, nested.ID) {
		t.Fatalf("the delete sent again with the nested list in its guard at the token the caller read: status=%d body=%s, "+
			"want 409 PRECONDITION_FAILED on %s", again.Code, again.Body.String(), nested.ID)
	}
	if mismatch := decodeBody[documentPreconditionConflict](t, again).Mismatches[0]; mismatch.Expected != nested.Token ||
		mismatch.Current == nil || *mismatch.Current != changedList.Token {
		t.Fatalf("the stale refusal's mismatch = %#v, want %s changed from %s to %s", mismatch, nested.ID, nested.Token, changedList.Token)
	}
	if got := readDocumentPrecondition(t, handler, artifact).Markdown; got != changed.Markdown {
		t.Fatalf("document after the two refused deletes = %q, want the other writer's %q", got, changed.Markdown)
	}
}

// A marker delete guarded on the item's paragraph alone, read while the item held only that
// paragraph, is refused 409 PRECONDITION_FAILED with nothing written after another writer added a
// list under the item: its unguarded_blocks names the added list at the token it carries now, a
// block the caller's read listed no token for, and its error says that such a block was changed
// or added by another writer since that read, so the caller reads the document again rather than
// send the refusal's token. That read lists the added list, and the other writer's list is still
// in the document (INFRA-341).
func TestAMarkerDeleteGuardedOnItsParagraphAloneAfterANestedListWasAddedIsRefusedThenReadAgain(t *testing.T) {
	t.Parallel()
	handler, _, _ := preconditionTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Marker delete after an added list", "- Parent\n- Other\n")
	artifact := issue.PrimaryArtifactID
	read := listedBlocks(t, handler, artifact)
	parent := listedBlock(t, handler, artifact, "paragraph", "Parent")
	// An insert of a list anchored on the item's text would join the item's own list as a sibling
	// item (pmdoc.ListBoundary), so the other writer puts the list under the item with a replace of
	// the item's text by that text and the list.
	writer := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+artifact+"/edits", map[string]any{
		"ops": []map[string]string{{"op": "replace", "find": "Parent", "with": "Parent\n\n- Child, added by another writer"}},
	}, "alice")
	if writer.Code != http.StatusOK {
		t.Fatalf("the other writer's nested list: status=%d body=%s", writer.Code, writer.Body.String())
	}
	changed := readDocumentPrecondition(t, handler, artifact)
	if want := "- Parent\n  - Child, added by another writer\n- Other\n"; changed.Markdown != want {
		t.Fatalf("document after the other writer's replace = %q, want %q, a list nested under Parent", changed.Markdown, want)
	}
	added := listedBlock(t, handler, artifact, "bullet_list", "- Child, added by another writer")
	for _, block := range read {
		if block.ID == added.ID {
			t.Fatalf("the caller's read listed %s before the other writer added it: %#v", added.ID, read)
		}
	}
	response := blockGuardedEdit(t, handler, artifact, map[string]string{"op": "delete", "find": "- Parent"}, parent)
	if !refusedUnguarded(t, response, changed.Token, added) {
		t.Fatalf("a marker delete guarded on the item's paragraph alone, after another writer added a list under it: status=%d body=%s, "+
			"want 409 PRECONDITION_FAILED naming the added list %s at its token now, %s", response.Code, response.Body.String(), added.ID, added.Token)
	}
	const unlisted = "if they differ, or your read listed no token for it, another writer changed or added it since your read, " +
		"so read the document again before you remove it"
	if refusal := decodeBody[documentPreconditionConflict](t, response).Error; !strings.Contains(refusal, unlisted) {
		t.Fatalf("the refusal's error = %q, want it to say %q", refusal, unlisted)
	}

	// The caller's read listed no token for the added list, so it reads the document again, as the
	// refusal says: the refused delete wrote nothing, the other writer's list is still there, and
	// the read lists it.
	if got := readDocumentPrecondition(t, handler, artifact).Markdown; got != changed.Markdown {
		t.Fatalf("the document read again after the refused delete = %q, want the other writer's %q", got, changed.Markdown)
	}
	listed := false
	for _, block := range listedBlocks(t, handler, artifact) {
		if block.ID == added.ID && block.Token == added.Token {
			listed = true
		}
	}
	if !listed {
		t.Fatalf("the blocks read again do not list the added list %s at %s", added.ID, added.Token)
	}
}

// A marker delete guarded on the item's paragraph alone, with no other writer about, is refused
// too, with nothing written: the refusal names the nested list the delete would remove at the
// token it carries now, the same token the caller's own read listed for it. Sent again with the
// list added to the guard at the token that read listed, the delete is taken, removes the item
// with its nested item, and its answer names no block.
func TestAMarkerDeleteGuardedOnItsParagraphAloneIsRefusedThenTakenWithTheListAsRead(t *testing.T) {
	t.Parallel()
	handler, _, _ := preconditionTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Marker delete refusal", "- Parent\n  - Child\n- Other\n")
	artifact := issue.PrimaryArtifactID
	deleteParent := map[string]string{"op": "delete", "find": "- Parent"}
	before := readDocumentPrecondition(t, handler, artifact)
	parent := listedBlock(t, handler, artifact, "paragraph", "Parent")
	nested := listedBlock(t, handler, artifact, "bullet_list", "- Child")
	response := blockGuardedEdit(t, handler, artifact, deleteParent, parent)
	if !refusedUnguarded(t, response, before.Token, nested) {
		t.Fatalf("a marker delete guarded on the item's paragraph alone: status=%d body=%s, want 409 PRECONDITION_FAILED naming the nested list %s at its token %s",
			response.Code, response.Body.String(), nested.ID, nested.Token)
	}
	if got := readDocumentPrecondition(t, handler, artifact).Markdown; got != before.Markdown {
		t.Fatalf("document after the refused delete = %q, want it unchanged, %q", got, before.Markdown)
	}
	again := blockGuardedEdit(t, handler, artifact, deleteParent, parent, nested)
	if again.Code != http.StatusOK {
		t.Fatalf("the delete sent again with the nested list in its guard at the token the caller read: status=%d body=%s, want 200",
			again.Code, again.Body.String())
	}
	if got := decodeBody[guardedEditAnswer](t, again).UnguardedBlocks; len(got) != 0 {
		t.Fatalf("unguarded_blocks = %#v, want none for a delete whose guard names all it removes", got)
	}
	if got := readDocumentPrecondition(t, handler, artifact).Markdown; got != "- Other\n" {
		t.Fatalf("document after the guarded delete = %q, want the item gone with its nested item", got)
	}
}

// A marker delete under no guard removes the whole item it selects, its nested item with it: an
// edit naming no precondition is taken unchecked, as it always was (INFRA-341).
func TestAMarkerDeleteUnderNoGuardRemovesTheWholeItem(t *testing.T) {
	t.Parallel()
	handler, _, _ := preconditionTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Marker delete unguarded", "- Parent\n  - Child\n- Other\n")
	artifact := issue.PrimaryArtifactID
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+artifact+"/edits", map[string]any{
		"ops": []map[string]string{{"op": "delete", "find": "- Parent"}},
	}, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("a marker delete under no guard: status=%d body=%s, want 200", response.Code, response.Body.String())
	}
	if got := readDocumentPrecondition(t, handler, artifact).Markdown; got != "- Other\n" {
		t.Fatalf("document after the unguarded delete = %q, want the item gone with its nested item", got)
	}
}

// A delete of an item's whole text with no list marker, under a guard on that paragraph alone, is
// taken, with the nested list as it was read and after another writer changed a nested item: the
// item's one nested list takes its place, so the delete removes the item's text alone and keeps
// every nested item, the other writer's change included, as main does, and its answer names no
// block (INFRA-341).
func TestATextDeleteThatHoistsANestedListIsTakenUnderAGuardOnItsParagraph(t *testing.T) {
	t.Parallel()
	for _, test := range []struct{ name, change, want string }{
		{name: "the nested list as it was read", want: "- Child\n- Other\n"},
		{name: "after another writer changed a nested item", change: "Child, changed by another writer", want: "- Child, changed by another writer\n- Other\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler, _, _ := preconditionTestHandler(t)
			issue := createInteractionIssue(t, handler, "TEST", "Hoisting delete guard", "- Parent\n  - Child\n- Other\n")
			artifact := issue.PrimaryArtifactID
			parent := listedBlock(t, handler, artifact, "paragraph", "Parent")
			if test.change != "" {
				writer := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+artifact+"/edits", map[string]any{
					"ops": []map[string]string{{"op": "replace", "find": "Child", "with": test.change}},
				}, "alice")
				if writer.Code != http.StatusOK {
					t.Fatalf("the other writer's change: status=%d body=%s", writer.Code, writer.Body.String())
				}
			}
			response := blockGuardedEdit(t, handler, artifact, map[string]string{"op": "delete", "find": "Parent"}, parent)
			if response.Code != http.StatusOK {
				t.Fatalf("a delete of the item's text guarded on its paragraph: status=%d body=%s, want 200", response.Code, response.Body.String())
			}
			if got := readDocumentPrecondition(t, handler, artifact).Markdown; got != test.want {
				t.Fatalf("document after the guarded delete = %q, want %q, the nested list in the item's place", got, test.want)
			}
			if got := decodeBody[guardedEditAnswer](t, response).UnguardedBlocks; len(got) != 0 {
				t.Fatalf("unguarded_blocks = %#v, want none: the nested list is kept", got)
			}
		})
	}
}

// guardedEditAnswer is the part of a guarded edit's answer these cases read.
type guardedEditAnswer struct {
	UnguardedBlocks []model.EditBlockPrecondition `json:"unguarded_blocks"`
}

// listedBlock is the block of kind GET …/blocks lists whose text, as the document's markdown writes
// it, is text.
func listedBlock(t *testing.T, handler http.Handler, artifactID, kind, text string) model.ArtifactBlock {
	t.Helper()
	markdown := readDocumentPrecondition(t, handler, artifactID).Markdown
	blocks := listedBlocks(t, handler, artifactID)
	for _, block := range blocks {
		if block.Type == kind && strings.TrimSpace(markdown[block.From:block.To]) == text {
			return block
		}
	}
	t.Fatalf("no %s whose text is %q among %#v", kind, text, blocks)
	return model.ArtifactBlock{}
}

// blockGuardedEdit sends op under a block guard naming each of guard at the token it was read with.
func blockGuardedEdit(t *testing.T, handler http.Handler, artifactID string, op map[string]string, guard ...model.ArtifactBlock) *httptest.ResponseRecorder {
	t.Helper()
	blocks := make([]map[string]string, 0, len(guard))
	for _, block := range guard {
		blocks = append(blocks, map[string]string{"id": block.ID, "token": block.Token})
	}
	return dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+artifactID+"/edits", map[string]any{
		"ops":          []map[string]string{op},
		"precondition": map[string]any{"blocks": blocks},
	}, "alice")
}

// refusedUncovered reports whether response is the refusal of a block guard that leaves exactly
// the block id uncovered.
func refusedUncovered(response *httptest.ResponseRecorder, id string) bool {
	return response.Code == http.StatusBadRequest &&
		strings.Contains(response.Body.String(), `"code":"INVALID_PRECONDITION"`) &&
		strings.Contains(response.Body.String(), "blocks must include tokens for resolved blocks: "+id+`"`)
}

// refusedStale reports whether response is the refusal of a block guard whose one stale token is
// the block id's.
func refusedStale(t *testing.T, response *httptest.ResponseRecorder, id string) bool {
	t.Helper()
	if response.Code != http.StatusConflict {
		return false
	}
	conflict := decodeBody[documentPreconditionConflict](t, response)
	return conflict.Code == "PRECONDITION_FAILED" && len(conflict.Mismatches) == 1 && conflict.Mismatches[0].BlockID == id
}

// refusedUnguarded reports whether response is the refusal of an edit that would remove removed,
// a block its guard leaves out, read on the document whose token is document: 409
// PRECONDITION_FAILED with no stale token, whose unguarded_blocks names that block alone at the
// token it was listed with, and whose error names its id.
func refusedUnguarded(t *testing.T, response *httptest.ResponseRecorder, document string, removed model.ArtifactBlock) bool {
	t.Helper()
	if response.Code != http.StatusConflict {
		return false
	}
	conflict := decodeBody[documentPreconditionConflict](t, response)
	return conflict.Code == "PRECONDITION_FAILED" && conflict.Current.Document == document && len(conflict.Mismatches) == 0 &&
		len(conflict.UnguardedBlocks) == 1 && conflict.UnguardedBlocks[0] == (model.EditBlockPrecondition{ID: removed.ID, Token: removed.Token}) &&
		strings.Contains(conflict.Error, strconv.Quote(removed.ID))
}

// A guard that names the nested list a marker delete takes is held to that list's token, as to any
// block it names: a guard on the item's paragraph and its nested list, read before another writer
// changed a nested item, is stale, and nothing is written; a guard on both read after that change
// takes the delete (INFRA-341).
func TestAMarkerDeleteIsGuardedOnTheWholeItemItRemoves(t *testing.T) {
	t.Parallel()
	handler, _, _ := preconditionTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Marker delete coverage", "- Parent\n  - Child\n- Other\n")
	artifact := issue.PrimaryArtifactID
	deleteParent := map[string]string{"op": "delete", "find": "- Parent"}
	parent := listedBlock(t, handler, artifact, "paragraph", "Parent")
	nested := listedBlock(t, handler, artifact, "bullet_list", "- Child")

	writer := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+artifact+"/edits", map[string]any{
		"ops": []map[string]string{{"op": "replace", "find": "Child", "with": "Child, changed by another writer"}},
	}, "alice")
	if writer.Code != http.StatusOK {
		t.Fatalf("the other writer's change: status=%d body=%s", writer.Code, writer.Body.String())
	}
	changed := readDocumentPrecondition(t, handler, artifact).Markdown
	if stale := blockGuardedEdit(t, handler, artifact, deleteParent, parent, nested); !refusedStale(t, stale, nested.ID) {
		t.Fatalf("a marker delete guarded on the item's paragraph and nested list, read before a nested item changed: status=%d body=%s, want 409 PRECONDITION_FAILED on %s", stale.Code, stale.Body.String(), nested.ID)
	}
	if got := readDocumentPrecondition(t, handler, artifact).Markdown; got != changed || !strings.Contains(got, "Child, changed by another writer") {
		t.Fatalf("document after the stale delete = %q, want the other writer's %q", got, changed)
	}

	fresh := blockGuardedEdit(t, handler, artifact, deleteParent,
		listedBlock(t, handler, artifact, "paragraph", "Parent"),
		listedBlock(t, handler, artifact, "bullet_list", "- Child, changed by another writer"))
	if fresh.Code != http.StatusOK {
		t.Fatalf("a marker delete guarded on the whole item as it stands: status=%d body=%s, want 200", fresh.Code, fresh.Body.String())
	}
	if got := readDocumentPrecondition(t, handler, artifact).Markdown; got != "- Other\n" {
		t.Fatalf("document after the covered delete = %q, want the item gone with its nested item", got)
	}
}

// A typed block a delete removes is held to the tokens its guard names, as a list item is. After
// another writer changed each typed block's second paragraph, `delete {block}` under the block's
// own guard read before that change is stale, and its first paragraph's guard alone does not name
// the block the delete names. A marker delete of the item holding a typed block under the item's
// paragraph and the typed block read before the change is stale, and under the item's paragraph
// alone it is refused naming the typed block it would remove, with the token it carries now, the
// changed one. Each refusal writes nothing (INFRA-341).
func TestADeleteThatRemovesATypedBlockIsRefusedUnlessItsGuardNamesItAsItStands(t *testing.T) {
	t.Parallel()
	handler, _, _ := preconditionTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Typed block delete coverage",
		":::callout{#c1 kind=\"note\" title=\"T\"}\nAlpha.\n\nBeta.\n:::\n\n"+
			"- Lead\n\n  :::callout{#c2 kind=\"note\" title=\"T\"}\n  Gamma.\n\n  Delta.\n  :::\n- Other\n")
	artifact := issue.PrimaryArtifactID
	read := map[string]model.ArtifactBlock{}
	for _, block := range listedBlocks(t, handler, artifact) {
		read[block.ID] = block
	}
	c1, c2 := read["c1"], read["c2"]
	if c1.Token == "" || c2.Token == "" {
		t.Fatalf("blocks = %#v, want the callouts c1 and c2", read)
	}
	alpha := listedBlock(t, handler, artifact, "paragraph", "Alpha.")
	lead := listedBlock(t, handler, artifact, "paragraph", "Lead")

	writer := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+artifact+"/edits", map[string]any{
		"ops": []map[string]string{
			{"op": "replace", "find": "Beta.", "with": "Beta, changed by another writer."},
			{"op": "replace", "find": "Delta.", "with": "Delta, changed by another writer."},
		},
	}, "alice")
	if writer.Code != http.StatusOK {
		t.Fatalf("the other writer's change: status=%d body=%s", writer.Code, writer.Body.String())
	}
	changed := readDocumentPrecondition(t, handler, artifact)
	current := map[string]model.ArtifactBlock{}
	for _, block := range listedBlocks(t, handler, artifact) {
		current[block.ID] = block
	}

	deleteC1 := map[string]string{"op": "delete", "block": "c1"}
	deleteLead := map[string]string{"op": "delete", "find": "- Lead"}
	for _, test := range []struct {
		name  string
		op    map[string]string
		guard []model.ArtifactBlock
		// stale says the refusal is 409 PRECONDITION_FAILED on id's stale token, removed that it
		// is 409 PRECONDITION_FAILED naming id among the blocks the delete removes that the guard
		// leaves out, at its token now, and otherwise it is 400 INVALID_PRECONDITION naming id as
		// the block the guard leaves uncovered.
		stale, removed bool
		id             string
	}{
		{name: "delete {block} under the typed block's guard read before", op: deleteC1, guard: []model.ArtifactBlock{c1}, stale: true, id: "c1"},
		{name: "delete {block} under its first paragraph's guard alone", op: deleteC1, guard: []model.ArtifactBlock{alpha}, id: "c1"},
		{
			name: "a marker delete of the item holding a typed block, under the item's paragraph and the typed block read before",
			op:   deleteLead, guard: []model.ArtifactBlock{lead, c2}, stale: true, id: "c2",
		},
		{
			name: "a marker delete of the item holding a typed block, under the item's paragraph alone",
			op:   deleteLead, guard: []model.ArtifactBlock{lead}, removed: true, id: "c2",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := blockGuardedEdit(t, handler, artifact, test.op, test.guard...)
			switch {
			case test.stale && !refusedStale(t, response, test.id):
				t.Fatalf("status=%d body=%s, want 409 PRECONDITION_FAILED on %s", response.Code, response.Body.String(), test.id)
			case test.removed && !refusedUnguarded(t, response, changed.Token, current[test.id]):
				t.Fatalf("status=%d body=%s, want 409 PRECONDITION_FAILED naming %s at its token %s", response.Code, response.Body.String(), test.id, current[test.id].Token)
			case !test.stale && !test.removed && !refusedUncovered(response, test.id):
				t.Fatalf("status=%d body=%s, want 400 INVALID_PRECONDITION naming %s", response.Code, response.Body.String(), test.id)
			}
			if got := readDocumentPrecondition(t, handler, artifact).Markdown; got != changed.Markdown {
				t.Fatalf("document after the refused delete = %q, want the other writer's %q", got, changed.Markdown)
			}
		})
	}
}

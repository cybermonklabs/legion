package api

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The edits route answers what the batch wrote (INFRA-1520 R6): `Applied 1 ops (version 2)` reads
// the same whether the edit wrote a list or a paragraph of dashes, so the answer carries the
// markdown of every block the write left that the document did not hold before, and names each
// block it removed or emptied.

// editReadBack is the part of the edits route's answer these cases read.
type editReadBack struct {
	Applied int    `json:"applied"`
	Changed bool   `json:"changed"`
	Written string `json:"written"`
}

// applyEdit sends ops to the document's edits route and answers what it wrote.
func applyEdit(t *testing.T, handler http.Handler, artifactID string, body map[string]any) editReadBack {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+artifactID+"/edits", body, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("edit: status=%d body=%s", response.Code, response.Body.String())
	}
	return decodeBody[editReadBack](t, response)
}

// listedBlocks are the document's blocks as GET /blocks lists them, in document order.
func listedBlocks(t *testing.T, handler http.Handler, artifactID string) []model.ArtifactBlock {
	t.Helper()
	return decodeBody[[]model.ArtifactBlock](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+artifactID+"/blocks", nil, "alice"))
}

// A replace with an empty `with` deletes the text it matched and leaves the block standing with
// nothing in it. The read-back names that block, which is the visibility dispatch://INFRA-1946
// lacked: the answer read the same as one that wrote a paragraph. (A replace with no `with` at all
// is refused now, TestAReplaceWithoutItsWithIsRefused.)
func TestDocumentEditReadBackNamesAnEmptiedBlock(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Emptied block", "Intro.\n\nPlanning notes.\n\nOutro.\n")
	blocks := listedBlocks(t, handler, issue.PrimaryArtifactID)
	if len(blocks) != 3 {
		t.Fatalf("blocks = %#v, want the three paragraphs", blocks)
	}
	edited := applyEdit(t, handler, issue.PrimaryArtifactID, map[string]any{
		"ops": []map[string]any{{"op": "replace", "find": "Planning notes.", "with": ""}},
	})
	if want := blocks[1].ID + " paragraph: emptied"; edited.Written != want {
		t.Fatalf("written = %q, want %q", edited.Written, want)
	}
}

// A batch that only moves a block says which block moved and where it now stands, so it does not
// read as a batch that changed nothing.
func TestDocumentEditReadBackNamesAMovedBlock(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Moved block", "Alpha.\n\nBravo.\n\nCharlie.\n")
	blocks := listedBlocks(t, handler, issue.PrimaryArtifactID)
	if len(blocks) != 3 {
		t.Fatalf("blocks = %#v, want the three paragraphs", blocks)
	}
	alpha, bravo, charlie := blocks[0].ID, blocks[1].ID, blocks[2].ID

	toStart := applyEdit(t, handler, issue.PrimaryArtifactID, map[string]any{
		"ops": []map[string]any{{"op": "move", "block": bravo, "before": "block:" + alpha}},
	})
	if want := bravo + " paragraph: moved to the start of the document"; toStart.Written != want {
		t.Fatalf("move to the start: written = %q, want %q", toStart.Written, want)
	}
	if !toStart.Changed {
		t.Fatalf("move to the start: changed = false, want a changed document")
	}

	// The document now reads Bravo, Alpha, Charlie; Charlie moved up past Alpha is one move.
	afterBravo := applyEdit(t, handler, issue.PrimaryArtifactID, map[string]any{
		"ops": []map[string]any{{"op": "move", "block": charlie, "after": "block:" + bravo}},
	})
	if want := charlie + " paragraph: moved after " + bravo; afterBravo.Written != want {
		t.Fatalf("move after a block: written = %q, want %q", afterBravo.Written, want)
	}
}

// A batch that leaves the document exactly as it found it wrote no markdown, and says so by
// carrying none.
func TestDocumentEditReadBackOfABatchThatChangedNothingIsEmpty(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Unchanged", "Alpha.\n\nBravo.\n")
	edited := applyEdit(t, handler, issue.PrimaryArtifactID, map[string]any{
		"ops": []map[string]any{{"op": "replace", "find": "Alpha.", "with": "Alpha."}},
	})
	if edited.Changed || edited.Written != "" {
		t.Fatalf("written = %q, changed = %v, want no markdown for a batch that changed nothing", edited.Written, edited.Changed)
	}
}

// longReadBackOps are four replaces whose text is longer than the read-back's bound together, and
// the markdown each one leaves. The third holds multi-byte runes, so the cut that lands in it
// must fall on a rune boundary.
func longReadBackOps() (ops []map[string]any, written []string) {
	for _, replacement := range []struct{ find, with string }{
		{find: "Alpha.", with: strings.TrimSpace(strings.Repeat("alpha ", 134))},
		{find: "Bravo.", with: strings.TrimSpace(strings.Repeat("bravo ", 134))},
		{find: "Charlie.", with: strings.TrimSpace(strings.Repeat("charlié ", 100))},
		{find: "Delta.", with: strings.TrimSpace(strings.Repeat("delta ", 134))},
	} {
		ops = append(ops, map[string]any{"op": "replace", "find": replacement.find, "with": replacement.with})
		written = append(written, replacement.with)
	}
	return ops, written
}

// checkBoundedReadBack reads a read-back that ran past its bound: the blocks it printed whole, the
// block it cut, and the line that names what it left out.
func checkBoundedReadBack(t *testing.T, got string, blocks []string) {
	t.Helper()
	if len(got) > 2000 {
		t.Fatalf("written holds %d bytes, want the 2,000-byte bound", len(got))
	}
	if !utf8.ValidString(got) {
		t.Fatalf("written = %q, want a cut that never splits a rune", got)
	}
	entries := strings.Split(got, "\n\n")
	if len(entries) != 4 {
		t.Fatalf("written = %q, want two whole blocks, a cut one and the line that names the rest", got)
	}
	if entries[0] != blocks[0] || entries[1] != blocks[1] {
		t.Fatalf("written opens %q and %q, want the first two blocks whole", entries[0], entries[1])
	}
	cut := entries[2]
	if cut == blocks[2] || !strings.HasPrefix(blocks[2], cut) {
		t.Fatalf("written cut %q, want a piece of the third block %q", cut, blocks[2])
	}
	left := utf8.RuneCountInString(blocks[2]) - utf8.RuneCountInString(cut) + utf8.RuneCountInString(blocks[3])
	if want := docs.ReadBackCutLine(left, 2, 0); entries[3] != want {
		t.Fatalf("written ends %q, want %q", entries[3], want)
	}
	if len(got) < 1900 {
		t.Fatalf("written holds %d bytes, want a read-back that fills its bound before it cuts", len(got))
	}
}

// An edit whose blocks hold more than the bound stops there: it prints the blocks it has room for,
// cuts the one the bound falls in, and names the characters and the blocks it did not print.
func TestDocumentEditReadBackStopsAtItsBound(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Long edit", "Alpha.\n\nBravo.\n\nCharlie.\n\nDelta.\n")
	ops, blocks := longReadBackOps()
	edited := applyEdit(t, handler, issue.PrimaryArtifactID, map[string]any{"ops": ops})
	if edited.Applied != 4 {
		t.Fatalf("applied = %d, want 4", edited.Applied)
	}
	checkBoundedReadBack(t, edited.Written, blocks)
}

// A replace that grows one block past the read-back's bound writes the block whole; only the
// answer's echo of it stops at the bound. The line closing that echo says what the read-back
// leaves unshown and that the document holds it, and nothing in the answer says the document,
// the edit or the block was cut: an agent read `cut N characters in 1 blocks` as text its edit
// lost, and reverted a correct edit over it (dispatch://INFRA-4749).
func TestDocumentEditReadBackOfAGrownBlockSaysWhatItDoesNotShow(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Grown block", "Alpha.\n\nBravo.\n")
	before := documentMarkdown(t, handler, issue.PrimaryArtifactID)
	grown := strings.TrimSpace(strings.Repeat("bravo ", 500))
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"ops": []map[string]any{{"op": "replace", "find": "Bravo.", "with": grown}},
	}, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("edit: status=%d body=%s", response.Code, response.Body.String())
	}
	answer := response.Body.String()
	written := decodeBody[editReadBack](t, response).Written

	if len(written) > 2000 {
		t.Fatalf("written holds %d bytes, want the 2,000-byte bound", len(written))
	}
	shown, line, closed := strings.Cut(written, "\n\n")
	if !closed || shown == grown || !strings.HasPrefix(grown, shown) {
		t.Fatalf("written = %q, want the opening of the grown block and the line closing the read-back", written)
	}
	left := utf8.RuneCountInString(grown) - utf8.RuneCountInString(shown)
	if want := docs.ReadBackCutLine(left, 1, 0); line != want {
		t.Fatalf("written ends %q, want %q", line, want)
	}
	for _, says := range []string{"read-back", fmt.Sprintf("%d characters", left), "not shown", "the document holds them"} {
		if !strings.Contains(line, says) {
			t.Fatalf("written ends %q, want a line that says %q: what the read-back leaves unshown and that the document holds it", line, says)
		}
	}
	if cut := regexp.MustCompile(`(?i)\bcut\b`).FindString(answer); cut != "" {
		t.Fatalf("the answer says %q: %s; want no text saying the document, the edit or a block was cut", cut, answer)
	}

	if after, want := documentMarkdown(t, handler, issue.PrimaryArtifactID), strings.Replace(before, "Bravo.", grown, 1); after != want {
		t.Fatalf("the document reads %q after the edit, want every character the replace wrote: %q", after, want)
	}
}

// Past the bound, the line closing the read-back counts the markdown it leaves unshown, which the
// document holds, apart from the lines naming blocks the edit removed, which it does not: a
// replace that grew a block past the bound and two deletes after it ended `626 characters of 3
// blocks are not shown here, and the document holds them`, counting the two deleted paragraphs'
// lines as blocks the document held, so an agent could read its deletes as not done.
func TestDocumentEditReadBackPastItsBoundCountsTheRemovedBlocksApart(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Grown, then two deleted", "Alpha.\n\nBravo.\n\nCharlie.\n\nDelta.\n")
	grown := strings.TrimSpace(strings.Repeat("alpha ", 400))
	edited := applyEdit(t, handler, issue.PrimaryArtifactID, map[string]any{"ops": []map[string]any{
		{"op": "replace", "find": "Alpha.", "with": grown},
		{"op": "delete", "find": "Bravo."},
		{"op": "delete", "find": "Charlie."},
	}})
	if edited.Applied != 3 || len(edited.Written) > 2000 {
		t.Fatalf("applied = %d, written holds %d bytes; want 3 applied within the 2,000-byte bound", edited.Applied, len(edited.Written))
	}
	shown, line, closed := strings.Cut(edited.Written, "\n\n")
	if !closed || shown == grown || !strings.HasPrefix(grown, shown) {
		t.Fatalf("written = %q, want the opening of the grown block and the line closing the read-back", edited.Written)
	}
	left := utf8.RuneCountInString(grown) - utf8.RuneCountInString(shown)
	want := fmt.Sprintf("the read-back shows at most 2000 bytes; %d characters of 1 block are not shown here, and the document holds them; 2 lines naming blocks the edit removed, left empty or moved are not shown either", left)
	if line != want || line != docs.ReadBackCutLine(left, 1, 2) {
		t.Fatalf("written ends %q, want %q", line, want)
	}
	if after, wantDocument := documentMarkdown(t, handler, issue.PrimaryArtifactID), grown+"\n\nDelta.\n"; after != wantDocument {
		t.Fatalf("the document reads %q after the edit, want %q", after, wantDocument)
	}
}

// Past the bound, a line naming a block the edit removed is printed whole or not at all, since a
// part of it names no block. The grown block here leaves 5 bytes of the read-back's room after it,
// fewer than the line naming the deleted paragraph after it holds: the read-back leaves that line
// out and counts it in its closing line, as it counts the block after it.
func TestDocumentEditReadBackLeavesOutTheNamedLineItsBoundFallsIn(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Grown, deleted, grown", "Alpha.\n\nBravo.\n\nCharlie.\n")
	blocks := listedBlocks(t, handler, issue.PrimaryArtifactID)
	if len(blocks) != 3 {
		t.Fatalf("blocks = %#v, want the three paragraphs", blocks)
	}
	charlie := strings.TrimSpace(strings.Repeat("charlie ", 40))
	// The room is the 2,000-byte bound less a blank line and the closing line at the counts the
	// read-back starts from: two blocks, whose characters run to four digits, and one line.
	line := len(docs.ReadBackCutLine(1000, 2, 1))
	grown := strings.Repeat("a", 2000-2-line-2-5)
	if characters := len(grown) + len(charlie); characters < 1000 || characters > 9999 {
		t.Fatalf("the two blocks hold %d characters, want four digits of them, as the room is measured at", characters)
	}
	named := blocks[1].ID + " paragraph: removed"
	edited := applyEdit(t, handler, issue.PrimaryArtifactID, map[string]any{"ops": []map[string]any{
		{"op": "replace", "find": "Alpha.", "with": grown},
		{"op": "delete", "find": "Bravo."},
		{"op": "replace", "find": "Charlie.", "with": charlie},
	}})
	if want := grown + "\n\n" + docs.ReadBackCutLine(len(charlie), 1, 1); edited.Written != want {
		t.Fatalf("written = %q, want the grown block whole and the line closing the read-back, with no part of %q", edited.Written, named)
	}
}

// The same edit under a block precondition, which resolves and writes the batch in the other
// caller of the mutate closure, reads back the same field.
func TestDocumentEditReadBackUnderABlockPreconditionIsTheSame(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Guarded long edit", "Alpha.\n\nBravo.\n\nCharlie.\n\nDelta.\n")
	listed := listedBlocks(t, handler, issue.PrimaryArtifactID)
	if len(listed) != 4 {
		t.Fatalf("blocks = %#v, want the four paragraphs", listed)
	}
	guard := make([]map[string]string, 0, len(listed))
	for _, block := range listed {
		guard = append(guard, map[string]string{"id": block.ID, "token": block.Token})
	}
	ops, blocks := longReadBackOps()
	edited := applyEdit(t, handler, issue.PrimaryArtifactID, map[string]any{
		"ops":          ops,
		"precondition": map[string]any{"blocks": guard},
	})
	if edited.Applied != 4 {
		t.Fatalf("applied = %d, want 4", edited.Applied)
	}
	checkBoundedReadBack(t, edited.Written, blocks)
}

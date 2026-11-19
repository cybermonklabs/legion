package api

import (
	"net/http"
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

// A replace with no `with` deletes the text it matched and leaves the block standing with nothing
// in it. The read-back names that block, which is the visibility dispatch://INFRA-1946 lacked:
// the answer read the same as one that wrote a paragraph.
func TestDocumentEditReadBackNamesAnEmptiedBlock(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Emptied block", "Intro.\n\nPlanning notes.\n\nOutro.\n")
	blocks := listedBlocks(t, handler, issue.PrimaryArtifactID)
	if len(blocks) != 3 {
		t.Fatalf("blocks = %#v, want the three paragraphs", blocks)
	}
	edited := applyEdit(t, handler, issue.PrimaryArtifactID, map[string]any{
		"ops": []map[string]any{{"op": "replace", "find": "Planning notes."}},
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
	if want := docs.ReadBackCutLine(left, 2); entries[3] != want {
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

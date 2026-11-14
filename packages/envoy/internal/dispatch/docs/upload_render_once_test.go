package docs

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// countRenders records every rendering of a document tree the package makes from here to the end
// of the test (documentRenders, countDocumentRender) and answers the markdown each of them
// produced, in the order they finished. It is the markdown and not a count alone because the
// acceptance counts the renders of one document and not the renders of anything: a write that
// renders the text it was given twice costs the same count as one that renders the document it
// replaces and then that text. One test records at a time, as one test at a time sets a hook on
// Service, and the record is taken away in t.Cleanup.
func countRenders(t *testing.T) func() []string {
	t.Helper()
	record := &renderRecord{}
	if !documentRenders.CompareAndSwap(nil, record) {
		t.Fatal("another test is recording the document's renders already")
	}
	t.Cleanup(func() { documentRenders.Store(nil) })
	return record.rendered
}

// rendersOf is how many of the recorded renderings produced markdown.
func rendersOf(rendered []string, markdown string) int {
	count := 0
	for _, one := range rendered {
		if one == markdown {
			count++
		}
	}
	return count
}

// renderedList names every recorded rendering by what it produced, for a failure to read.
func renderedList(rendered []string) string {
	quoted := make([]string, len(rendered))
	for index, one := range rendered {
		quoted[index] = "\t" + strconv.Quote(one)
	}
	return strings.Join(quoted, "\n")
}

// uploadRenders is how many documents an upload that changes the document renders in all: two,
// both renders the write's own. ReplaceText renders the document as it stands, to tell whether
// the upload changes it, and renders the replacement it parsed (renderTreeWithOffsets), which it
// answers its caller with; applyLive takes that second rendering for the tree the write left,
// which is the very tree it rendered, rather than rendering the same text over again
// (writtenRendering). Nothing else renders: the warm room's replica holds the rendering of the
// state the fork was read from, so applyLive takes that one rather than rendering the fork, and
// the room's update observer classifies the published update by the rendering the write made of
// it. The base rendered three times: applyLive rendered the tree the write left
// (documentMarkdownValidWithOffsets), the same text a third time.
const uploadRenders = 2

// uploadRendersOfTheUpload is how many of those renders are of the document the upload wrote,
// which is the number the acceptance counts: one here, two at the base, where applyLive rendered
// that same text again for the tree the write left. The other render of the two is of the
// document as it stands before the upload, which the upload needs and keeps, and which this test
// counts by itself: a change that dropped that render and put a second render of the uploaded
// text back in its place would leave the total at two.
const uploadRendersOfTheUpload = 1

// An upload that changes an existing document, written through the entry point every upload takes
// (ReplaceText in a joined transaction), renders uploadRenders documents, of which
// uploadRendersOfTheUpload are of the document it wrote.
func TestUploadRenderOnceCountsTheRendersOfAnUpload(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "The quick brown fox.\n")
	// The seed's settlement stamps the block ids and writes the first version, and the upload
	// before the measured one leaves the room's replica holding the state it wrote, as any room
	// a browser or an earlier upload has written stands. The measured upload therefore meets a
	// settled document in a warm room, as a real upload does: its read of the fork is the
	// replica's (warmFork), so the rendering of the document before it is known without rendering
	// it, and the room's update observer classifies its update by the rendering it published.
	settleCurrentGeneration(t, service, artifactID)
	const beforeUpload = "The quick brown fox jumps.\n"
	if _, err := joinedReplaceText(service, artifactID, beforeUpload, model.Actor{Kind: "user", ID: "alice"}); err != nil {
		t.Fatalf("warm the room with a first upload: %v", err)
	}

	renders := countRenders(t)
	const uploadedText = "The quick brown dog.\n"
	uploaded, err := joinedReplaceText(service, artifactID, uploadedText, model.Actor{Kind: "user", ID: "alice"})
	rendered := renders()
	if err != nil {
		t.Fatalf("upload the replacement: %v", err)
	}
	t.Logf("an upload that changes the document rendered %d documents:\n%s", len(rendered), renderedList(rendered))
	if uploaded != uploadedText {
		t.Fatalf("the upload wrote %q, want the uploaded markdown", uploaded)
	}
	if ofUpload := rendersOf(rendered, uploadedText); ofUpload != uploadRendersOfTheUpload {
		t.Fatalf("the upload rendered the document it wrote %d times, want %d:\n%s",
			ofUpload, uploadRendersOfTheUpload, renderedList(rendered))
	}
	// The other render is of the document as it stood before the upload, which ReplaceText reads
	// to tell whether the upload changes it: naming it here is what holds the total to renders
	// this test has accounted for, rather than to a number two other renders could make up.
	if before := rendersOf(rendered, beforeUpload); before != uploadRenders-uploadRendersOfTheUpload {
		t.Fatalf("the upload rendered the document as it stood before it %d times, want %d:\n%s",
			before, uploadRenders-uploadRendersOfTheUpload, renderedList(rendered))
	}
	if len(rendered) != uploadRenders {
		t.Fatalf("the upload rendered %d documents, want %d:\n%s", len(rendered), uploadRenders, renderedList(rendered))
	}
}

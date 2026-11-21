package api

import (
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc/heavybodies"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// The architect's ruling of 2026-11-20 22:18Z on dispatch://INFRA-1523, census class V (a): the
// delete of a comment whose pending insert suggestion it rejects writes the document's next
// version in its own transaction, as the document's settlement would (docs.SettleVersion), so no
// store built from the latest version gives the insert's words once the delete has committed.
// Before it, those stores held them until the settlement wrote that version a settle delay later.

// The stores built from the document's latest version, read straight after the deletes commit
// and with no wait: the block's latest text (an unpinned block reference), the spec's blocks with
// their excerpts (its part_of edges), the block's name and text tree (the names read), search, and
// the related-item check. None gives the words of the deleted inserts. Each read is the dashboard's
// own, with the display trees it asks for.
//
// The spec of newAnchorsOnAnInsert holds the words from its first version, which every one of these
// stores is built from before the delete, so each read gives them then: the positive control. The
// related-item check lists an issue's spec as the issue itself, whose snippet is its title
// (fillRelatedItems), so the check is read over a project document too, which holds the same words
// under an insert suggestion of its own and is listed as itself. The service settles nothing on
// its own here (Settle is an hour), so only the delete can have written the stores without the
// words.
func TestTheLatestVersionsStoresHoldNoWordOfAPendingInsertOnceItsDeleteCommits(t *testing.T) {
	f := newAnchorsOnAnInsert(t)
	notes := createProjectDocument(t, f.handler, "TEST", "Design notes", "The "+insertedWords+" brown fox jumps\n")
	notesInsert := suggestProjectInsert(t, f.handler, f.database, f.documentService, notes, "m-notes-insert")
	refreshTermStats(t, f.database, "TEST")

	specRef := "dispatch://" + f.issueKey + "/spec"
	blocks := decodeBody[[]model.ArtifactBlock](t, dispatchRequest(t, f.handler, http.MethodGet, "/api/v1/artifacts/"+f.artifactID+"/blocks", nil, "alice"))
	if len(blocks) != 1 {
		t.Fatalf("the spec lists the blocks %#v, want its one paragraph", blocks)
	}
	blockRef := specRef + "#b-" + text.EncodeBlockID(blocks[0].ID)
	reads := []struct{ name, target string }{
		{"the block's latest text", "/api/v1/references?" + url.Values{"to": {blockRef}}.Encode()},
		{"the spec's blocks and their excerpts", "/api/v1/references?" + url.Values{"to": {specRef}, "kind": {"part_of"}}.Encode()},
		{"the block's name", referenceTargetsOf(blockRef)},
		{"search", "/api/v1/search?" + url.Values{"q": {"brown"}}.Encode()},
		{"the related-item check", "/api/v1/projects/TEST/related?" + url.Values{"title": {"brown fox jumps"}}.Encode()},
	}
	// The positive control: before the deletes, every read gives the words, so each check below
	// can fail.
	for _, read := range reads {
		if body := treesRequest(t, f.handler, read.target, true).Body.String(); !strings.Contains(body, insertedWords) {
			t.Fatalf("%s before the deletes gives no %q, so its check after them cannot fail: %s", read.name, insertedWords, body)
		}
	}

	deleteThreadComment(t, f.handler, f.insert.ID, "alice")
	deleteThreadComment(t, f.handler, notesInsert.ID, "alice")
	for _, read := range reads {
		if body := treesRequest(t, f.handler, read.target, true).Body.String(); strings.Contains(body, insertedWords) {
			t.Errorf("%s straight after the deletes gives the deleted inserts' words %q: %s", read.name, insertedWords, body)
		}
	}
}

// deleteLatencyRuns is how many deletes each case of the latency test times, and
// deleteLatencyBound the most the delete's settle may add to the delete's p99: the architect's
// ruling of 2026-11-20 22:18Z on dispatch://INFRA-1523.
const (
	deleteLatencyRuns  = 20
	deleteLatencyBound = time.Second
)

// latencySpec is a spec of a realistic size: sixteen sections, each a heading and nine paragraphs
// of 42 words of the load harness's vocabulary (docstest.SpecSentence), 160 blocks and about 38 KB.
// The latency test adds a paragraph for each insert it deletes, which brings it to about 200
// blocks and 40 KB.
func latencySpec() string {
	r := rand.New(rand.NewPCG(1523, 2218))
	var b strings.Builder
	for section := 1; section <= 16; section++ {
		fmt.Fprintf(&b, "## Section %d\n\n", section)
		for range 9 {
			b.WriteString(docstest.SpecSentence(r, 42) + ".\n\n")
		}
	}
	return b.String()
}

// The delete of a pending insert settles the document in its own transaction (docs.SettleVersion),
// which indexes every block of the version it writes in the delete's request. This times the
// delete with that settle and without it (server.deleteLeavesSettlement, the same delete with the
// version left to the settlement), each deleteLatencyRuns times and taken in turn, so a box that
// slows in the middle slows both alike, after one warm-up pair that is not counted. It logs the p50
// and the p99 of each and what the settle adds to each, and fails only where the settle adds more
// than deleteLatencyBound to the p99.
//
// Two documents: a spec of a realistic size (latencySpec), and the heaviest body the fixtures
// hold, heavybodies' plain list of 8,000 items, whose 8,001 blocks are the most any of its stored
// shapes reads as, and the index a version write pays for by the block (deferBlockIndexBlocks
// measures the same body at 43 µs a block). Each insert is a browser's insert mark on words of a
// paragraph of their own, suggested by a comment and deleted by its author, as
// newAnchorsOnAnInsert makes one. A settled delete writes exactly one version, and one left to
// the settlement writes none, or what is timed would not be the settle.
func TestTheDeleteOfAPendingInsertSettlesWithinItsLatencyBound(t *testing.T) {
	shapes := heavybodies.Shapes()
	plain := slices.IndexFunc(shapes, func(shape heavybodies.Shape) bool { return shape.ID == "plain-list" })
	if plain < 0 || shapes[plain].Refusal != "" {
		t.Fatalf("heavybodies holds no stored plain-list shape: %v", shapes)
	}
	for _, document := range []struct{ name, body string }{
		{"a spec of about 200 blocks and 40 KB", latencySpec()},
		{"the heaviest fixture body, " + shapes[plain].Name, shapes[plain].Grow()},
	} {
		t.Run(document.name, func(t *testing.T) {
			documentService, handler, database, served := browserDocumentServer(t)
			runs := 2 * (deleteLatencyRuns + 1)
			words := func(run int) string { return fmt.Sprintf("zarquonrun%02d", run) }
			var body strings.Builder
			body.WriteString(document.body)
			for run := range runs {
				fmt.Fprintf(&body, "\nRun %s adds its word here.\n", words(run))
			}
			issue := createInteractionIssue(t, handler, "LAT", "Delete latency", body.String())
			listed := decodeBody[[]model.ArtifactBlock](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/blocks", nil, "alice"))
			t.Logf("%s: %d blocks, %d bytes", document.name, len(listed), body.Len())

			var settled, unsettled []time.Duration
			for run := range runs {
				mark := fmt.Sprintf("m-run-%02d", run)
				writeBrowserMark(t, database, documentService, issue.PrimaryArtifactID, docs.MarkSpec{
					Kind:  docs.MarkSuggestion,
					ID:    mark,
					Attrs: pmdoc.Attrs{"id": mark, "by": "user:alice", "kind": "insert"},
				}, words(run)+" ")
				comment := createThreadComment(t, handler, issue.Key, map[string]any{
					"body":       "Add this word.",
					"anchor":     map[string]any{"artifact": "spec", "mark_id": mark},
					"suggestion": map[string]string{"replace_with": words(run) + " "},
				}, "alice")
				settles := run%2 == 0
				served.deleteLeavesSettlement = !settles
				before := latestVersion(t, database, issue.PrimaryArtifactID).Number
				started := time.Now()
				deleted := dispatchRequest(t, handler, http.MethodDelete, "/api/v1/comments/"+comment.ID, nil, "alice")
				taken := time.Since(started)
				if deleted.Code != http.StatusOK {
					t.Fatalf("delete run %d's insert: status=%d body=%s", run, deleted.Code, deleted.Body.String())
				}
				wrote, want := latestVersion(t, database, issue.PrimaryArtifactID).Number-before, 0
				if settles {
					want = 1
				}
				if wrote != want {
					t.Fatalf("the delete of run %d's insert (settles: %t) wrote %d versions, want %d", run, settles, wrote, want)
				}
				switch {
				case run < 2:
					// The warm-up pair, which pays for what every later run finds cached.
				case settles:
					settled = append(settled, taken)
				default:
					unsettled = append(unsettled, taken)
				}
			}
			slices.Sort(settled)
			slices.Sort(unsettled)
			with50, with99 := percentileOfDismissedScale(settled, 0.50), percentileOfDismissedScale(settled, 0.99)
			without50, without99 := percentileOfDismissedScale(unsettled, 0.50), percentileOfDismissedScale(unsettled, 0.99)
			t.Logf("%s: the delete with its settle: p50 %.1f ms, p99 %.1f ms; without it: p50 %.1f ms, p99 %.1f ms; over %d runs each",
				document.name, msOf(with50), msOf(with99), msOf(without50), msOf(without99), len(settled))
			t.Logf("%s: the settle adds p50 %.1f ms, p99 %.1f ms (bound %v on the p99)",
				document.name, msOf(with50-without50), msOf(with99-without99), deleteLatencyBound)
			if added := with99 - without99; added > deleteLatencyBound {
				t.Errorf("%s: the settle adds %.1f ms to the delete's p99, over the bound of %v", document.name, msOf(added), deleteLatencyBound)
			}
		})
	}
}

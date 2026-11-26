package api

import (
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
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
	"github.com/sjawhar/envoy/internal/logtest"
)

// The architect's decision of 2026-11-20 22:18Z on dispatch://INFRA-1523, census class V (a): the
// delete of a comment whose pending insert suggestion it rejects writes the document's next
// version in its own transaction, as the document's settlement would (docs.SettleVersion), so no
// store built from the latest version gives the insert's words once the delete has committed.
// Before it, those stores held them until the settlement wrote that version a settle delay later.

// insertedWords is the text the insert suggestions of these rows put in a document.
const insertedWords = "zarquoninserted"

// The stores built from the document's latest version, read straight after the deletes commit
// and with no wait: the block's latest text (an unpinned block reference), the spec's blocks with
// their excerpts (its part_of edges), the block's name and text tree (the names read), search, and
// the related-item check. None gives the words of the deleted inserts. Each read is the dashboard's
// own, with the display trees it asks for.
//
// The issue's spec holds insertedWords, under alice's insert suggestion on a browser's insert mark
// around them, from its first version, which every one of these stores is built from before the
// delete, so each read gives them then: the positive control. The related-item check lists an
// issue's spec as the issue itself, whose snippet is its title (fillRelatedItems), so the check is
// read over a project document too, which holds the same words under an insert suggestion of its
// own and is listed as itself. The service settles nothing on its own here (Settle is an hour), so
// only the delete can have written the stores without the words.
func TestTheLatestVersionsStoresHoldNoWordOfAPendingInsertOnceItsDeleteCommits(t *testing.T) {
	t.Parallel()
	documentService, handler, database := browserDocumentService(t)
	issue := createInteractionIssue(t, handler, "TEST", "An insert's words", "The "+insertedWords+" brown fox jumps")
	writeBrowserMark(t, database, documentService, issue.PrimaryArtifactID, docs.MarkSpec{
		Kind:  docs.MarkSuggestion,
		ID:    "m-insert",
		Attrs: pmdoc.Attrs{"id": "m-insert", "by": "user:alice", "kind": "insert"},
	}, insertedWords+" ")
	specInsert := createThreadComment(t, handler, issue.Key, map[string]any{
		"body":       "Add this word.",
		"anchor":     map[string]any{"artifact": "spec", "mark_id": "m-insert"},
		"suggestion": map[string]string{"replace_with": insertedWords + " "},
	}, "alice")
	notes := createProjectDocument(t, handler, "TEST", "Design notes", "The "+insertedWords+" brown fox jumps\n")
	writeBrowserMark(t, database, documentService, notes.ID, docs.MarkSpec{
		Kind:  docs.MarkSuggestion,
		ID:    "m-notes-insert",
		Attrs: pmdoc.Attrs{"id": "m-notes-insert", "by": "user:alice", "kind": "insert"},
	}, insertedWords+" ")
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+notes.ID+"/comments", map[string]any{
		"body":       "Add this word.",
		"anchor":     map[string]any{"artifact": notes.Slug, "mark_id": "m-notes-insert"},
		"suggestion": map[string]string{"replace_with": insertedWords + " "},
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("suggest the insert on the project document: status=%d body=%s", created.Code, created.Body.String())
	}
	notesInsert := decodeBody[model.Comment](t, created)
	refreshTermStats(t, database, "TEST")

	specRef := "dispatch://" + issue.Key + "/spec"
	blocks := decodeBody[[]model.ArtifactBlock](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/blocks", nil, "alice"))
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
		if body := treesRequest(t, handler, read.target, true).Body.String(); !strings.Contains(body, insertedWords) {
			t.Fatalf("%s before the deletes gives no %q, so its check after them cannot fail: %s", read.name, insertedWords, body)
		}
	}

	deleteThreadComment(t, handler, specInsert.ID, "alice")
	deleteThreadComment(t, handler, notesInsert.ID, "alice")
	for _, read := range reads {
		if body := treesRequest(t, handler, read.target, true).Body.String(); strings.Contains(body, insertedWords) {
			t.Errorf("%s straight after the deletes gives the deleted inserts' words %q: %s", read.name, insertedWords, body)
		}
	}
}

// deleteLatencyRuns is how many deletes each iteration of BenchmarkDeleteOfAPendingInsertSettle
// times with the settle, and how many it times without it.
const deleteLatencyRuns = 20

// latencySpec is a spec of a realistic size: sixteen sections, each a heading and nine paragraphs
// of 42 words of the load harness's vocabulary (docstest.SpecSentence), 160 blocks and about 38 KB.
// BenchmarkDeleteOfAPendingInsertSettle adds a paragraph for each insert it deletes, which brings
// it to about 200 blocks and 40 KB.
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

// BenchmarkDeleteOfAPendingInsertSettle measures what the delete of a pending insert pays to settle
// the document in its own transaction (docs.SettleVersion), which indexes every block of the
// version it writes in the delete's request. Each iteration times the delete with that settle and
// without it (server.deleteLeavesSettlement, the same delete with the version left to the
// settlement), deleteLatencyRuns times each and taken in turn, so a box that slows in the middle
// slows both alike, after one warm-up pair that is not counted. It logs and reports the p50 and
// the p99 of each and what the settle adds to each, in milliseconds (with-settle-p99-ms,
// settle-added-p99-ms and the like), over the deletes of every iteration. ns/op is one whole
// iteration, its server and its issue included.
//
// Two documents: a spec of a realistic size (latencySpec), and the heaviest body the fixtures
// hold, heavybodies' plain list of 8,000 items, whose 8,001 blocks are the most any of its stored
// shapes reads as, and the index a version write pays for by the block (deferBlockIndexBlocks
// measures the same body at 43 µs a block). Each insert is a browser's insert mark on words of a
// paragraph of their own, suggested by a comment and deleted by its author, as the spec's insert
// of TestTheLatestVersionsStoresHoldNoWordOfAPendingInsertOnceItsDeleteCommits is. A settled delete
// writes exactly one version, and one left to the settlement writes none, or what is timed would
// not be the settle; a delete that writes another count fails the benchmark.
//
// The architect's decisions on dispatch://INFRA-1523: on 2026-11-20 22:18Z, that the settle should
// add no more than about a second to the delete's p99 on the heaviest fixture, measured in a
// build Job without -race; on 2026-11-21 15:02Z, that this is a measurement and not a CI
// assertion, since a wall-clock bound on a shared runner is a test defect (the class
// dispatch://INFRA-3904 removes). Nothing here asserts a time, and no CI step runs it: none passes
// -bench, and .github/scripts/go-test-step-expansion.py refuses a step that would (:732-734).
// Run it from packages/envoy against the Postgres DISPATCH_TEST_DATABASE_URL names (storetest.Open
// skips without it):
//
//	go test -run '^$' -bench '^BenchmarkDeleteOfAPendingInsertSettle$' -benchtime=1x ./internal/dispatch/api
//
// -benchtime=1x is one iteration, so one measurement, for each document. Without it, B.Loop runs
// iterations until a second has passed, and the percentiles are over all of their deletes.
func BenchmarkDeleteOfAPendingInsertSettle(b *testing.B) {
	// The access log's line for every request would split the benchmark's result line; warnings
	// and errors still reach stderr.
	logtest.SetDefault(b, slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	shapes := heavybodies.Shapes()
	plain := slices.IndexFunc(shapes, func(shape heavybodies.Shape) bool { return shape.ID == "plain-list" })
	if plain < 0 || shapes[plain].Refusal != "" {
		b.Fatalf("heavybodies holds no stored plain-list shape: %v", shapes)
	}
	for _, document := range []struct{ name, body string }{
		{"a spec of about 200 blocks and 40 KB", latencySpec()},
		{"the heaviest fixture body, " + shapes[plain].Name, shapes[plain].Grow()},
	} {
		b.Run(document.name, func(b *testing.B) {
			runs := 2 * (deleteLatencyRuns + 1)
			words := func(run int) string { return fmt.Sprintf("zarquonrun%02d", run) }
			var body strings.Builder
			body.WriteString(document.body)
			for run := range runs {
				fmt.Fprintf(&body, "\nRun %s adds its word here.\n", words(run))
			}

			// One iteration is one whole measurement, on a server of its own: createInteractionIssue
			// creates the project it names, so a second issue on the same server would fail. B.Loop
			// runs this function once, so the deletes of every iteration count; a loop to b.N would
			// have the framework call it again for each count it tries, and the deletes of every
			// call but the last would be timed and dropped.
			var settled, unsettled []time.Duration
			for b.Loop() {
				documentService, handler, database, served := browserDocumentServer(b)
				issue := createInteractionIssue(b, handler, "LAT", "Delete latency", body.String())
				listed := decodeBody[[]model.ArtifactBlock](b, dispatchRequest(b, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/blocks", nil, "alice"))
				b.Logf("%s: %d blocks, %d bytes", document.name, len(listed), body.Len())

				for run := range runs {
					mark := fmt.Sprintf("m-run-%02d", run)
					writeBrowserMark(b, database, documentService, issue.PrimaryArtifactID, docs.MarkSpec{
						Kind:  docs.MarkSuggestion,
						ID:    mark,
						Attrs: pmdoc.Attrs{"id": mark, "by": "user:alice", "kind": "insert"},
					}, words(run)+" ")
					comment := createThreadComment(b, handler, issue.Key, map[string]any{
						"body":       "Add this word.",
						"anchor":     map[string]any{"artifact": "spec", "mark_id": mark},
						"suggestion": map[string]string{"replace_with": words(run) + " "},
					}, "alice")
					settles := run%2 == 0
					served.deleteLeavesSettlement = !settles
					before := latestVersion(b, database, issue.PrimaryArtifactID).Number
					started := time.Now()
					deleted := dispatchRequest(b, handler, http.MethodDelete, "/api/v1/comments/"+comment.ID, nil, "alice")
					taken := time.Since(started)
					if deleted.Code != http.StatusOK {
						b.Fatalf("delete run %d's insert: status=%d body=%s", run, deleted.Code, deleted.Body.String())
					}
					wrote, want := latestVersion(b, database, issue.PrimaryArtifactID).Number-before, 0
					if settles {
						want = 1
					}
					if wrote != want {
						b.Fatalf("the delete of run %d's insert (settles: %t) wrote %d versions, want %d", run, settles, wrote, want)
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
			}
			slices.Sort(settled)
			slices.Sort(unsettled)
			with50, with99 := percentileOfDismissedScale(settled, 0.50), percentileOfDismissedScale(settled, 0.99)
			without50, without99 := percentileOfDismissedScale(unsettled, 0.50), percentileOfDismissedScale(unsettled, 0.99)
			b.Logf("%s: the delete with its settle: p50 %.1f ms, p99 %.1f ms; without it: p50 %.1f ms, p99 %.1f ms; over %d runs each",
				document.name, msOf(with50), msOf(with99), msOf(without50), msOf(without99), len(settled))
			b.Logf("%s: the settle adds p50 %.1f ms, p99 %.1f ms", document.name, msOf(with50-without50), msOf(with99-without99))
			// Reported after the loop: B.Loop's first call resets the timer, which drops the metrics
			// reported before it.
			b.ReportMetric(msOf(with50), "with-settle-p50-ms")
			b.ReportMetric(msOf(with99), "with-settle-p99-ms")
			b.ReportMetric(msOf(without50), "without-settle-p50-ms")
			b.ReportMetric(msOf(without99), "without-settle-p99-ms")
			b.ReportMetric(msOf(with50-without50), "settle-added-p50-ms")
			b.ReportMetric(msOf(with99-without99), "settle-added-p99-ms")
		})
	}
}

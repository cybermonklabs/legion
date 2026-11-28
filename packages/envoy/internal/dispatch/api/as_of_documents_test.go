// as_of_documents_test.go holds the document routes at a cut to
// the cases the equality harness (as_of_equality_test.go) does not write: a document read after a
// version and an unsettled edit made after the cut, with a typed block, two blocks of one text and
// a table; a version this build cannot read back; a document removed after the cut; a version
// made after the cut; an approval request written before approval requests held their documents;
// and a review whose hash the boot fill pinned with no event.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/asof"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// documentRead reads target as alice and keeps the answer as the equality harness keeps it.
func documentRead(t *testing.T, handler http.Handler, target string) asOfAnswer {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, target, nil, "alice")
	return asOfAnswer{status: response.Code, header: response.Header().Clone(), body: bytes.Clone(response.Body.Bytes())}
}

// settleServed finishes the server's detached work and waits for the event horizon.
func settleServed(t *testing.T, served *server) {
	t.Helper()
	awaitBackgroundWrites(t, served)
	safeHorizon(t, served)
}

// requireRefusedAtCut fails the test unless answer is 404 with code.
func requireRefusedAtCut(t *testing.T, what string, answer asOfAnswer, code string) {
	t.Helper()
	var refusal refusalAnswer
	_ = json.Unmarshal(answer.body, &refusal)
	if answer.status != http.StatusNotFound || refusal.Code != code {
		t.Errorf("%s answers %d %s, want 404 %s", what, answer.status, abbreviateAnswer(answer.body), code)
	}
}

// notesAtTheCut is the document TestADocumentReadAtACutIsTheVersionThen reads: a heading, a
// paragraph, a requirement (a typed block, whose id its markdown names), two paragraphs of one
// text and a table.
const notesAtTheCut = "# Notes\n\nThe notes as the cut finds them.\n\n" +
	":::requirement{#r1}\nThe notes hold a requirement.\n:::\n\n" +
	"To be decided.\n\nTo be decided.\n\n" +
	"| Name | Value |\n| --- | --- |\n| cut | before |\n"

// repeatedBlockText is the text of notesAtTheCut's two paragraphs of one text.
const repeatedBlockText = "To be decided."

// TestADocumentReadAtACutIsTheVersionThen: a document read with as_of answers its newest version
// at the cut. A version made after the cut and an edit a browser typed after it, which nothing has
// versioned yet, change nothing in that answer. The text route gives the version's markdown, its
// document token and its number. With ?blocks=true, and on the block route, it gives the blocks of
// that markdown: each untyped block under the id that the block index recorded for its text at
// that version, and each typed block under the id its markdown names. The two paragraphs of one
// text take no id, since no record says which id is which, and the table's rows and cells take
// none, since the index records none. Each such id is left out and named absent, and so are each
// block's token, embedding state and anchored counts, which the version does not record. Every
// other member is the member of the answer read at the cut.
func TestADocumentReadAtACutIsTheVersionThen(t *testing.T) {
	// A document service that settles nothing during the test and admits a browser's socket.
	documentService, handler, database := browserDocumentService(t)
	issue := createInteractionIssue(t, handler, "ASDV", "Documents read at a cut", "# Spec\n\nThe spec.\n")
	notes := uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "notes.md", notesAtTheCut).Artifact
	text := "/api/v1/artifacts/" + notes.ID + "/text"
	reads := []struct{ route, target, blocks string }{
		{route: "GET /api/v1/artifacts/{id}/text", target: text},
		{route: "GET /api/v1/artifacts/{id}/text", target: text + "?blocks=true", blocks: "/blocks"},
		{route: "GET /api/v1/issues/{key}/artifacts/{slug}/blocks", target: "/api/v1/issues/" + issue.Key + "/artifacts/" + notes.Slug + "/blocks"},
		{route: "GET /api/v1/artifacts/{id}", target: "/api/v1/artifacts/" + notes.ID},
		{route: "GET /api/v1/artifacts/{id}/versions/{number}", target: "/api/v1/artifacts/" + notes.ID + "/versions/1"},
	}
	then := make([]asOfAnswer, len(reads))
	for index, read := range reads {
		if then[index] = documentRead(t, handler, read.target); then[index].status != http.StatusOK {
			t.Fatalf("GET %s at the cut: status=%d body=%s", read.target, then[index].status, then[index].body)
		}
	}
	var thenText artifactText
	if err := json.Unmarshal(then[1].body, &thenText); err != nil {
		t.Fatalf("decode the text read at the cut: %v", err)
	}
	cut := databaseCut(t, database)
	time.Sleep(5 * time.Millisecond)

	editIssueDocument(t, handler, issue.Key, notes.Slug, []map[string]any{{"op": "insert", "after": "end", "markdown": "A version made after the cut.\n"}})
	// The browser's peer joins after the version, so the room it opens holds the version, and what
	// it types is held in the room for the hour the service waits before it settles anything.
	peer := connectBrowserPeer(t, documentService, notes.ID)
	t.Cleanup(peer.close)
	peer.appendParagraph(t, "Typed after the cut and versioned by nobody.")
	waitForLiveText(t, documentService, notes.ID, "versioned by nobody")
	var present artifactText
	if err := json.Unmarshal(documentRead(t, handler, text+"?blocks=true").body, &present); err != nil {
		t.Fatalf("decode the present text: %v", err)
	}
	if !strings.Contains(present.Markdown, "A version made after the cut.") || !strings.Contains(present.Markdown, "versioned by nobody") ||
		present.Version == nil || *present.Version != 2 {
		t.Fatalf("the present text is version %v %q, want version 2 holding the version and the edit made after the cut", present.Version, present.Markdown)
	}

	for index, read := range reads {
		t.Run(read.target, func(t *testing.T) {
			asOf := documentRead(t, handler, withAsOf(t, read.target, cut))
			comparison := asOfComparison{route: read.route, cut: cut, world: &asOfWorld{}}
			for _, problem := range comparison.problems(then[index], asOf) {
				t.Error(problem)
			}
			if read.blocks == "" && !strings.HasSuffix(read.target, "/blocks") {
				return
			}
			doc, err := decodeAsOfBody(asOf.body)
			if err != nil {
				t.Fatalf("decode the answer at the cut: %v: %s", err, abbreviateAnswer(asOf.body))
			}
			// The block route carries no markdown: its blocks index the text the text route gave.
			blocks, markdown := doc, thenText.Markdown
			if read.blocks != "" {
				blocks = doc.(map[string]any)["blocks"]
				markdown, _ = doc.(map[string]any)["markdown"].(string)
			}
			listed, _ := blocks.([]any)
			if len(listed) == 0 {
				t.Fatalf("the answer at the cut lists no block: %s", abbreviateAnswer(asOf.body))
			}
			// The header names each pointer once, as asof.AbsentFields writes it.
			absent := absentPointers(asOf.header.Get(contracts.DispatchAsOfAbsentHeader))
			for _, member := range []string{"token", "embedding", "references", "id", "table/rows/*/id", "table/rows/*/cells/*/id"} {
				if pointer := read.blocks + "/*/" + member; !slices.Contains(absent, pointer) {
					t.Errorf("the answer at the cut names %v absent, want %s", absent, pointer)
				}
			}
			repeated := 0
			for position, element := range listed {
				block := element.(map[string]any)
				from, _ := block["from"].(json.Number).Int64()
				to, _ := block["to"].(json.Number).Int64()
				if from < 0 || to < from || int(to) > len(markdown) {
					t.Fatalf("block %d spans %d to %d, outside the %d bytes of the markdown at the cut", position, from, to, len(markdown))
				}
				blockText := markdown[from:to]
				id, given := block["id"]
				switch {
				case blockText == repeatedBlockText:
					repeated++
					if given {
						t.Errorf("block %d (%q) gives the id %v at the cut: a version that holds its text twice records no id of either", position, blockText, id)
					}
				case !given || id == "":
					t.Errorf("block %d (%q) gives no id at the cut, which the version's block index records", position, blockText)
				}
				for _, member := range []string{"token", "embedding", "references"} {
					if value, ok := block[member]; ok {
						t.Errorf("block %d (%q) gives %s %v at the cut, which the version does not record", position, blockText, member, value)
					}
				}
				if table, ok := block["table"].(map[string]any); ok {
					for _, row := range table["rows"].([]any) {
						if _, ok := row.(map[string]any)["id"]; ok {
							t.Errorf("a row of table %v gives an id at the cut, which the block index does not record", id)
						}
						for _, cell := range row.(map[string]any)["cells"].([]any) {
							if _, ok := cell.(map[string]any)["id"]; ok {
								t.Errorf("a cell of table %v gives an id at the cut, which the block index does not record", id)
							}
						}
					}
				}
			}
			if repeated != 2 {
				t.Errorf("the answer at the cut lists %d blocks of the text %q, want the 2 the version holds", repeated, repeatedBlockText)
			}
		})
	}
}

// TestAVersionThisBuildCannotReadIsReadAtACutAsItsBlocksAre: the text route reads at a cut a
// version this build cannot read back, here one holding an element only a newer build knows
// (docs.UnknownElementForTest), and answers it as it did before it read asks at a cut, never 500.
// A version that holds no ask block is served its stored markdown as it is, a plan's version
// among them, whose `:::plan_task{` opener holds `ask{`; with ?blocks=true it answers 409
// DOC_NEWER_CONTENT, as its blocks answer on every route. A version that holds an ask block
// answers 409 DOC_NEWER_CONTENT with blocks and without: its stored bytes hold the block's
// server-owned attributes, an answer's words among them, which the text routes take off,
// so none of them is served. Each answers so to a reader of asks and to every other reader.
func TestAVersionThisBuildCannotReadIsReadAtACutAsItsBlocksAre(t *testing.T) {
	_, handler, database, served := browserDocumentServer(t)
	issue := createInteractionIssue(t, handler, "ASDU", "Versions this build cannot read", "The spec.\n")
	const answerWord = "kestrel"
	newer := "Before.\n\n:::" + docs.UnknownElementForTest + "{#newer-1}\nWritten by a newer build.\n:::\n"
	versions := []struct {
		name, markdown string
		holdsAsk       bool
	}{
		{name: "plain", markdown: newer},
		{name: "planned", markdown: newer + "\n:::plan_task{#t1}\nRead the version.\n:::\n"},
		{name: "asked", holdsAsk: true, markdown: newer + "\n" +
			`:::ask{#count urgency="low" multiple="false" state="answered" answered_by="alice" selected="[&#x22;Heron&#x22;]" answer="The ` +
			answerWord + ` count."}` + "\nWhich count goes first?\n\n- Heron: The first.\n- Egret: The second.\n:::\n"},
	}
	documents := make([]model.Artifact, len(versions))
	for index, version := range versions {
		documents[index] = uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, version.name+".md", "Before.\n").Artifact
	}
	settleServed(t, served)
	// Each document's one version holds what a newer build wrote there.
	for index, version := range versions {
		written, err := database.Pool.Exec(t.Context(), `update artifact_versions set markdown = $2 where artifact_id = $1`,
			documents[index].ID, version.markdown)
		if err != nil || written.RowsAffected() != 1 {
			t.Fatalf("store the %s version a newer build wrote: %v, %d rows; want one", version.name, err, written.RowsAffected())
		}
	}
	cut := databaseCut(t, database)

	for index, version := range versions {
		for _, query := range []string{"", "?blocks=true"} {
			for _, readsAsks := range []bool{false, true} {
				target := withAsOf(t, "/api/v1/artifacts/"+documents[index].ID+"/text"+query, cut)
				response := textRead(t, handler, target, readsAsks)
				what := fmt.Sprintf("GET %s (the %s version, Dispatch-Asks %t)", target, version.name, readsAsks)
				if version.holdsAsk || query != "" {
					var refusal refusalAnswer
					_ = json.Unmarshal(response.Body.Bytes(), &refusal)
					if response.Code != http.StatusConflict || refusal.Code != "DOC_NEWER_CONTENT" {
						t.Errorf("%s answers %d %s, want 409 DOC_NEWER_CONTENT", what, response.Code, abbreviateAnswer(response.Body.Bytes()))
					}
					if strings.Contains(response.Body.String(), answerWord) {
						t.Errorf("%s shows %q, a word of the answer the stored version holds: %s", what, answerWord, response.Body.String())
					}
					continue
				}
				var text artifactText
				if err := json.Unmarshal(response.Body.Bytes(), &text); response.Code != http.StatusOK || err != nil {
					t.Errorf("%s answers %d %s (%v), want 200 with the stored markdown", what, response.Code, abbreviateAnswer(response.Body.Bytes()), err)
					continue
				}
				if text.Markdown != version.markdown {
					t.Errorf("%s serves the markdown %q, want the stored %q", what, text.Markdown, version.markdown)
				}
			}
		}
	}
}

// TestARemovedDocumentIsAbsentAtACutBeforeItsRemoval: a document removed after the cut is absent
// at the cut, as its removal left no row of it: each read of it by id or by slug answers 404
// ARTIFACT_NOT_FOUND as of the cut, and the issue's document list and the issue read, as of the
// cut, are the answers read at the cut without it.
func TestARemovedDocumentIsAbsentAtACutBeforeItsRemoval(t *testing.T) {
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "ASDR", "A document removed after the cut", "The spec.\n")
	scratch := emptyDocument(t, handler, issue.Key, "Scratch")
	lists := []struct{ route, target string }{
		{"GET /api/v1/issues/{key}/artifacts", "/api/v1/issues/" + issue.Key + "/artifacts"},
		{"GET /api/v1/issues/{key}", "/api/v1/issues/" + issue.Key},
	}
	reads := []string{
		"/api/v1/artifacts/" + scratch.ID,
		"/api/v1/artifacts/" + scratch.ID + "/text",
		"/api/v1/artifacts/" + scratch.ID + "/versions/1",
		"/api/v1/issues/" + issue.Key + "/artifacts/" + scratch.Slug,
		"/api/v1/issues/" + issue.Key + "/artifacts/" + scratch.Slug + "/blocks",
	}
	settleServed(t, served)
	then := make([]asOfAnswer, len(lists))
	for index, list := range lists {
		then[index] = documentRead(t, handler, list.target)
		doc, err := decodeAsOfBody(then[index].body)
		if err != nil || then[index].status != http.StatusOK {
			t.Fatalf("GET %s at the cut: status=%d body=%s", list.target, then[index].status, then[index].body)
		}
		if _, held := withoutElement(doc, scratch.ID); held != 1 {
			t.Fatalf("GET %s at the cut lists the document %d times, want once: %s", list.target, held, abbreviateAnswer(then[index].body))
		}
	}
	for _, target := range reads {
		if answer := documentRead(t, handler, target); answer.status != http.StatusOK {
			t.Fatalf("GET %s at the cut: status=%d body=%s", target, answer.status, answer.body)
		}
	}
	cut := databaseCut(t, database)
	time.Sleep(5 * time.Millisecond)
	removedDocument(t, handler, scratch.ID)
	settleServed(t, served)

	for _, target := range reads {
		requireRefusedAtCut(t, "GET "+target+" now", documentRead(t, handler, target), "ARTIFACT_NOT_FOUND")
		requireRefusedAtCut(t, "GET "+target+" as of the cut", documentRead(t, handler, withAsOf(t, target, cut)), "ARTIFACT_NOT_FOUND")
	}
	for index, list := range lists {
		asOf := documentRead(t, handler, withAsOf(t, list.target, cut))
		comparison := asOfComparison{route: list.route, cut: cut, world: &asOfWorld{removed: scratch.ID}}
		for _, problem := range comparison.problems(then[index], asOf) {
			t.Errorf("GET %s as of the cut: %s", list.target, problem)
		}
	}
}

// TestAVersionMadeAfterTheCutIsNotFound: a version of a document, or of a file, made after the cut
// did not exist at the cut. Read as of the cut it is refused as the read at the cut refused it, in
// JSON and as a download, while a version made before the cut answers as it answered then: the
// download's file name counts the versions at the cut, so the document's newest version then
// carries no number in it.
func TestAVersionMadeAfterTheCutIsNotFound(t *testing.T) {
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "ASDN", "Versions read at a cut", "The spec.\n")
	owner := "/api/v1/issues/" + issue.Key
	uploadDocument(t, handler, owner, "notes.md", "# Notes\n\nThe first version.\n")
	notes := uploadDocument(t, handler, owner, "notes.md", "# Notes\n\nThe second version.\n").Artifact
	uploadFile := func(content string) model.Artifact {
		t.Helper()
		response := multipartRequest(t, handler, owner+"/artifacts", map[string]string{"name": "data.txt"}, "data.txt", "text/plain", []byte(content), "alice")
		if response.Code != http.StatusCreated {
			t.Fatalf("upload data.txt: status=%d body=%s", response.Code, response.Body.String())
		}
		return decodeBody[uploadedArtifact](t, response).Artifact
	}
	data := uploadFile("The data as first sent.")
	route := "GET /api/v1/artifacts/{id}/versions/{number}"
	versions, files := "/api/v1/artifacts/"+notes.ID+"/versions/", "/api/v1/artifacts/"+data.ID+"/versions/"
	reads := []struct {
		target  string
		missing bool
	}{
		{target: versions + "2"},
		{target: versions + "2?download=true"},
		{target: files + "1"},
		{target: versions + "3", missing: true},
		{target: versions + "3?download=true", missing: true},
		{target: files + "2", missing: true},
	}
	settleServed(t, served)
	then := make([]asOfAnswer, len(reads))
	for index, read := range reads {
		want := http.StatusOK
		if read.missing {
			want = http.StatusNotFound
		}
		if then[index] = documentRead(t, handler, read.target); then[index].status != want {
			t.Fatalf("GET %s at the cut: status=%d body=%s, want %d", read.target, then[index].status, then[index].body, want)
		}
	}
	cut := databaseCut(t, database)
	time.Sleep(5 * time.Millisecond)
	uploadDocument(t, handler, owner, "notes.md", "# Notes\n\nThe third version, made after the cut.\n")
	uploadFile("The data as sent again after the cut.")
	settleServed(t, served)

	for index, read := range reads {
		t.Run(read.target, func(t *testing.T) {
			now := documentRead(t, handler, read.target)
			if now.status != http.StatusOK {
				t.Fatalf("GET %s now: status=%d body=%s", read.target, now.status, now.body)
			}
			asOf := documentRead(t, handler, withAsOf(t, read.target, cut))
			comparison := asOfComparison{route: route, cut: cut, world: &asOfWorld{}}
			for _, problem := range comparison.problems(then[index], asOf) {
				t.Error(problem)
			}
			// A download's name counts the versions there are: the newest version then carries
			// no number, and now that a later one exists it does.
			if named := then[index].header.Get("Content-Disposition"); named != "" {
				if disposition := asOf.header.Get("Content-Disposition"); disposition != named || disposition == now.header.Get("Content-Disposition") {
					t.Errorf("the download as of the cut is named %q; at the cut it was named %q and now %q", disposition, named, now.header.Get("Content-Disposition"))
				}
			}
		})
	}
}

// TestADocumentAtACutAwaitsAnApprovalRequestWrittenBeforeItsDocuments: an approval request written
// before approval requests held their documents (its events carry an approval with no documents,
// which migration 20261102110100 filled in the row with no event) is open on its document at the
// cut. The document's reads as of the cut find it by the one document such a request named: the
// approval awaits it, names it, and leaves out its documents and names them absent (reason B), as
// the request's own read does.
func TestADocumentAtACutAwaitsAnApprovalRequestWrittenBeforeItsDocuments(t *testing.T) {
	world, takeCut := newAsOfThreadWorld(t, "ASDA")
	issue := asOfIssue(t, world.handler, map[string]any{
		"project": "ASDA", "title": "An approval from before its documents", "spec": "# Spec\n\nApprove this.\n",
	})
	requested := decodeBody[struct {
		Ask model.Ask `json:"ask"`
	}](t, asOfSessionWrite(t, world.handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/approval-requests",
		map[string]any{"summary": "Approve the spec."})).Ask.ID
	world.settle(t)
	// As a build before approval requests held their documents wrote it.
	if _, err := world.served.deps.Store.Pool.Exec(context.Background(), `
		update events set payload = payload #- '{approval,documents}'
		where payload->>'id' = $1 and type like 'ask.%'`, requested); err != nil {
		t.Fatalf("write the request's events as an older build wrote them: %v", err)
	}
	cut := takeCut()
	for _, read := range []struct{ target, list, absent string }{
		{target: "/api/v1/artifacts/" + issue.PrimaryArtifactID, absent: "/approval/documents"},
		{target: "/api/v1/issues/" + issue.Key + "/artifacts", absent: "/*/approval/documents"},
		{target: "/api/v1/issues/" + issue.Key, list: "artifacts", absent: "/artifacts/*/approval/documents"},
	} {
		t.Run(read.target, func(t *testing.T) {
			approval := func(doc any) map[string]any {
				t.Helper()
				candidates := []any{doc}
				if read.list != "" {
					candidates, _ = doc.(map[string]any)[read.list].([]any)
				} else if listed, ok := doc.([]any); ok {
					candidates = listed
				}
				for _, candidate := range candidates {
					if document, _ := candidate.(map[string]any); document["id"] == issue.PrimaryArtifactID {
						found, _ := document["approval"].(map[string]any)
						return found
					}
				}
				t.Fatalf("the answer holds no approval of the spec %s: %s", issue.PrimaryArtifactID, abbreviateValue(doc))
				return nil
			}
			if now := approval(world.readNow(t, read.target)); now["state"] != "awaiting" {
				t.Fatalf("the spec's approval now is %s, want awaiting the request", abbreviateValue(now))
			}
			doc, absent := world.readAt(t, read.target, cut)
			atCut := approval(doc)
			if atCut["state"] != "awaiting" || atCut["ask_id"] != requested {
				t.Errorf("the spec's approval at the cut is %s, want awaiting the request %s", abbreviateValue(atCut), requested)
			}
			if value, given := atCut["documents"]; given {
				t.Errorf("approval/documents at the cut is %s, which no event at the cut gives: the answer leaves it out", abbreviateValue(value))
			}
			if !slices.Contains(absent, read.absent) {
				t.Errorf("the answer at the cut names %v absent, want %s", absent, read.absent)
			}
		})
	}
}

// TestAReviewHashTheBootFillPinnedIsAbsentAtEveryCut: the boot fill of review hashes
// (docs.FillReviewContentHashes) writes the hash of the reviewed version, and its source backfill,
// into a review written before reviews held them, with no event and no record of its time. So no
// record gives whether the review held the hash at a cut, and at every cut, one taken before the
// fill and one after it, the approval whose latest review the fill pinned leaves out
// content_sha256 and content_sha256_source and names both absent: on the document's read by id
// and by slug, on its issue's list of documents and on its issue. In the same answers the approval
// of a document whose review its insert pinned keeps both, and before the fill the review that
// holds no pin gives neither and names nothing. Every other member of each approval at the cut,
// its state and version among them, is the member of the present read, which gives the fill's
// hash and source.
func TestAReviewHashTheBootFillPinnedIsAbsentAtEveryCut(t *testing.T) {
	world, takeCut := newAsOfThreadWorld(t, "ASDH")
	ctx := context.Background()
	pool := world.served.deps.Store.Pool
	issue := asOfIssue(t, world.handler, map[string]any{
		"project": "ASDH", "title": "A review the boot fill pinned", "spec": "# Spec\n\nApprove this.\n",
	})
	spec, owner := issue.PrimaryArtifactID, "/api/v1/issues/"+issue.Key
	uploadDocument(t, world.handler, owner, "notes.md", "# Notes\n\nThe notes as approved.\n")
	uploaded := uploadDocument(t, world.handler, owner, "notes.md", "# Notes\n\nThe notes as changed since.\n").Artifact
	notes := uploaded.ID
	// The spec's review is pinned by its insert, as every review this build writes is.
	reviewDocument(t, world.handler, spec, "alice", "approved", "")
	world.settle(t)

	// The notes' review as a build before reviews held their hash wrote it (docs'
	// TestFillReviewContentHashesFillsOldRowsOnceAsBackfill): an approval of version 1, which
	// version 2 made stale, inserted with the trigger that pins each insert turned off, so the row
	// holds no hash and no source.
	legacy, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the older build's review: %v", err)
	}
	defer legacy.Rollback(ctx)
	write := func(statement string, args ...any) {
		t.Helper()
		if _, err := legacy.Exec(ctx, statement, args...); err != nil {
			t.Fatalf("write the notes' review as an older build wrote it: %s: %v", statement, err)
		}
	}
	write(`alter table artifact_reviews disable trigger artifact_reviews_content_sha256_insert`)
	write(`insert into artifact_reviews (artifact_id, version, state, actor) values ($1, 1, 'approved', '{"kind":"user","id":"alice"}')`, notes)
	// The review-sequence constraint trigger is deferred: drain it before the insert trigger is
	// turned on again in this transaction.
	write(`set constraints all immediate`)
	write(`alter table artifact_reviews enable trigger artifact_reviews_content_sha256_insert`)
	if err := legacy.Commit(ctx); err != nil {
		t.Fatalf("commit the older build's review: %v", err)
	}

	specHash := contentHash(versionMarkdown(t, world.handler, spec, 1))
	notesHash := contentHash(versionMarkdown(t, world.handler, notes, 1))
	if notesHash == contentHash(versionMarkdown(t, world.handler, notes, 2)) {
		t.Fatal("the two versions of the notes have one hash: the fixture cannot tell which version the fill pinned")
	}
	type pin struct{ hash, source string }
	// pins is the pin each review of the two documents holds now, by its document, as the row
	// holds it: no hash and no source read as "".
	pins := func(t *testing.T) map[string]pin {
		t.Helper()
		rows, err := pool.Query(ctx, `
			select artifact_id::text, coalesce(content_sha256, ''), coalesce(content_sha256_source, '')
			from artifact_reviews where artifact_id::text = any($1)`, []string{spec, notes})
		if err != nil {
			t.Fatalf("read the reviews' pins: %v", err)
		}
		defer rows.Close()
		held := map[string]pin{}
		for rows.Next() {
			var id string
			var row pin
			if err := rows.Scan(&id, &row.hash, &row.source); err != nil {
				t.Fatalf("read the reviews' pins: %v", err)
			}
			if _, twice := held[id]; twice {
				t.Fatalf("document %s holds two reviews, want one", id)
			}
			held[id] = row
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("read the reviews' pins: %v", err)
		}
		return held
	}
	inserted := pin{hash: specHash, source: "insert"}
	if held := pins(t); !maps.Equal(held, map[string]pin{spec: inserted, notes: {}}) {
		t.Fatalf("the reviews hold the pins %+v, want the spec's pinned by its insert and the notes' with none", held)
	}

	named := map[string]string{spec: "the spec", notes: "notes.md"}
	// Each document's approval now: the spec approved at its one version, the notes stale at
	// version 1 of 2.
	states := map[string]string{spec: "approved", notes: "stale"}
	latest := map[string]json.Number{spec: "1", notes: "2"}
	reads := []struct {
		target, list, prefix string
		holds                []string
	}{
		{target: "/api/v1/artifacts/" + spec, holds: []string{spec}},
		{target: "/api/v1/artifacts/" + notes, holds: []string{notes}},
		{target: owner + "/artifacts/" + uploaded.Slug, holds: []string{notes}},
		{target: owner + "/artifacts", prefix: "/*", holds: []string{spec, notes}},
		{target: owner, list: "artifacts", prefix: "/artifacts/*", holds: []string{spec, notes}},
	}
	// approvalsIn is the approval of each of the two documents that doc, an answer, holds, by the
	// document's id: under list, else in the array doc is, else doc's own.
	approvalsIn := func(t *testing.T, doc any, list string) map[string]map[string]any {
		t.Helper()
		documents := []any{doc}
		if list != "" {
			documents, _ = doc.(map[string]any)[list].([]any)
		} else if listed, ok := doc.([]any); ok {
			documents = listed
		}
		approvals := map[string]map[string]any{}
		for _, candidate := range documents {
			document, _ := candidate.(map[string]any)
			if id, _ := document["id"].(string); id == spec || id == notes {
				approval, ok := document["approval"].(map[string]any)
				if !ok {
					t.Fatalf("%s in the answer holds no approval: %s", named[id], abbreviateValue(document))
				}
				approvals[id] = approval
			}
		}
		return approvals
	}
	// readAll reads each target now and at cut. notesPin is the pin of the notes' review now.
	readAll := func(t *testing.T, cut asof.Cut, notesPin pin) {
		t.Helper()
		filled := notesPin.source == "backfill"
		want := map[string]pin{spec: inserted, notes: notesPin}
		for _, read := range reads {
			t.Run(read.target, func(t *testing.T) {
				now := approvalsIn(t, world.readNow(t, read.target), read.list)
				doc, absent := world.readAt(t, read.target, cut)
				atCut := approvalsIn(t, doc, read.list)
				if len(now) != len(read.holds) || len(atCut) != len(read.holds) {
					t.Fatalf("the answer holds the approvals of %d documents now and %d at the cut, want %d", len(now), len(atCut), len(read.holds))
				}
				for _, id := range read.holds {
					present, then := now[id], atCut[id]
					hash, _ := present["content_sha256"].(string)
					source, _ := present["content_sha256_source"].(string)
					if got := (pin{hash: hash, source: source}); got != want[id] {
						t.Errorf("the approval of %s now gives the pin %+v, want %+v", named[id], got, want[id])
					}
					if present["state"] != states[id] || present["version"] != json.Number("1") || present["latest_version"] != latest[id] {
						t.Errorf("the approval of %s now is %s, want %s at version 1 of %s", named[id], abbreviateValue(present), states[id], latest[id])
					}
					expected := maps.Clone(present)
					if id == notes && filled {
						for _, member := range []string{"content_sha256", "content_sha256_source"} {
							if value, given := then[member]; given {
								t.Errorf("approval/%s of %s at the cut is %s, which no record at the cut gives: the answer leaves it out", member, named[id], abbreviateValue(value))
							}
							delete(expected, member)
						}
					}
					if !reflect.DeepEqual(then, expected) {
						t.Errorf("the approval of %s at the cut is %s, want %s", named[id], abbreviateValue(then), abbreviateValue(expected))
					}
				}
				leftOut := filled && slices.Contains(read.holds, notes)
				for _, member := range []string{"/approval/content_sha256", "/approval/content_sha256_source"} {
					if pointer := read.prefix + member; slices.Contains(absent, pointer) != leftOut {
						t.Errorf("the answer at the cut names %v absent; naming %s is %t, want %t", absent, pointer, !leftOut, leftOut)
					}
				}
			})
		}
	}

	beforeFill := takeCut()
	t.Run("before the fill", func(t *testing.T) { readAll(t, beforeFill, pin{}) })

	// The reads' detached work ends before the newest event is taken, so only the fill runs
	// between the two reads of it.
	world.settle(t)
	newest := lastEventID(t, world.served.deps.Store)
	if filled, err := docs.FillReviewContentHashes(ctx, pool); err != nil || filled != (docs.ReviewHashFill{Filled: 1, Unhashable: 0}) {
		t.Fatalf("the fill = %+v, %v; want the notes' review alone filled and none left unhashable", filled, err)
	}
	if after := lastEventID(t, world.served.deps.Store); after != newest {
		t.Fatalf("the newest event was %d before the fill and is %d after it, want the fill to append none", newest, after)
	}
	backfilled := pin{hash: notesHash, source: "backfill"}
	if held := pins(t); !maps.Equal(held, map[string]pin{spec: inserted, notes: backfilled}) {
		t.Fatalf("after the fill the reviews hold the pins %+v, want the spec's as its insert pinned it and the notes' version 1 filled as backfill", held)
	}

	t.Run("after the fill, at the cut before it", func(t *testing.T) { readAll(t, beforeFill, backfilled) })
	afterFill := takeCut()
	t.Run("at a cut after the fill", func(t *testing.T) { readAll(t, afterFill, backfilled) })
}

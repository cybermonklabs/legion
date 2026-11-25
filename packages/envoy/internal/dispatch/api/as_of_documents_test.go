// as_of_documents_test.go holds the document routes at a cut to
// the cases the equality harness (as_of_equality_test.go) does not write: a document read after a
// version and an unsettled edit made after the cut, with a typed block, two blocks of one text and
// a table; a document removed after the cut; a version made after the cut; and an approval
// request written before approval requests held their documents.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
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

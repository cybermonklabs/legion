package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest/newerversion"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
)

func TestTheNewTypesAreReadButNotWrittenBeforeTheirNodePass(t *testing.T) {
	handler, database, documents := settlingCarryDocuments(t, pastTheRun)
	const intro = "Introductory prose.\n\n"
	const requirement = ":::requirement{#target}\nA requirement.\n:::\n\n"
	issue := createInteractionIssue(t, handler, "TEST", "Read before writing", intro+requirement)
	id := issue.PrimaryArtifactID
	owner := "/api/v1/issues/" + issue.Key + "/artifacts"
	path := "/api/v1/artifacts/" + id
	upload := func(markdown string) *httptest.ResponseRecorder {
		return sessionRequest(t, handler, http.MethodPost, owner, map[string]any{
			"actor": sessionActor(), "name": "spec.md", "content": markdown,
			"precondition": uploadBaseNamed(t, handler, owner, "spec.md"),
		})
	}
	edit := func(op map[string]string) *httptest.ResponseRecorder {
		return sessionRequest(t, handler, http.MethodPost, path+"/edits", map[string]any{
			"actor": sessionActor(), "ops": []map[string]string{op},
		})
	}
	const initialAsk = ":::ask{#read-it}\nWhich day?\n\n- Friday\n- Monday\n:::\n"
	insertDecisionMarkdown(t, handler, id, initialAsk, 2)
	asks := settledDecisionBlocks(t, handler, documents, issue.Key, id, map[string]string{"read-it": "Which day?"})
	answerDecisionBlock(t, handler, asks["read-it"], "alice", []string{"Friday"}, "", nil)
	settledVersion(t, handler, documents, id, 3)
	before := latestVersion(t, database, id).Number
	const decision = ":::decision{#decide uncertainty=\"low\"}\nWe chose Friday.\n:::\n"
	bareDecision := strings.Replace(decision, ` uncertainty="low"`, "", 1)
	const reading = ":::reading{#reading}\nThe agent reads it as Friday.\n:::\n"
	askWithReading := "::::ask{#read-it}\nWhich day?\n\n- Friday\n- Monday\n\n" + reading + "::::\n"
	target := "dispatch://" + issue.Key + "/spec#b-target"
	shaped := `shaped="` + target + `"`
	askWithShaped := ":::ask{#read-it " + shaped + "}\nWhich day?\n\n- Friday\n- Monday\n:::\n"
	for _, tc := range []struct{ element, markdown, insert string }{
		{`typed block "decision"`, intro + requirement + initialAsk + "\n" + bareDecision, bareDecision},
		{`typed block "reading"`, intro + requirement + askWithReading, strings.Replace(askWithReading, "#read-it", "#other-ask", 1)},
		{`attribute "shaped" of typed block "ask"`, intro + requirement + askWithShaped, strings.Replace(askWithShaped, "#read-it", "#other-ask", 1)},
	} {
		want := "this Dispatch build reads " + tc.element + " but cannot write it yet"
		for _, write := range []struct {
			name string
			run  func() *httptest.ResponseRecorder
		}{
			{"upload", func() *httptest.ResponseRecorder { return upload(tc.markdown) }},
			{"doc-edit insert", func() *httptest.ResponseRecorder {
				return edit(map[string]string{"op": "insert", "after": "end", "markdown": tc.insert})
			}},
		} {
			response := write.run()
			refusal := decodeBody[struct {
				Error string `json:"error"`
			}](t, response)
			if response.Code != http.StatusBadRequest || !strings.Contains(refusal.Error, want) {
				t.Fatalf("%s of %s = %d %s, want refusal %q", write.name, tc.element, response.Code, response.Body.String(), want)
			}
			if got := latestVersion(t, database, id).Number; got != before {
				t.Fatalf("%s of %s changed version from %d to %d", write.name, tc.element, before, got)
			}
		}
	}

	// Keep the state that the answered ask's block already showed and add the new elements via SQL.
	oldOpener := askDirective(t, versionMarkdown(t, handler, id, before), "read-it")
	seedAsk := strings.Replace(askWithReading, "::::ask{#read-it}", ":"+strings.TrimSuffix(oldOpener, "}")+" "+shaped+"}", 1)
	if !strings.Contains(seedAsk, shaped) {
		t.Fatalf("fixture has no shaped ask: %s", seedAsk)
	}
	seed := intro + requirement + decision + "\n" + seedAsk
	stored := newerversion.StoreNewerVersion(t, database.Pool, id, seed)
	requireRead := func(prose string, version int) {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, path+"/text", nil)
		request.Header.Set("X-Dispatch-User", "alice")
		request.Header.Set(displayTreesHeader, "1")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("read version %d's live text: %d %s", version, response.Code, response.Body.String())
		}
		text := decodeBody[artifactText](t, response)
		if text.Version == nil || *text.Version != version || !contains(prose, ":::decision{#decide", ":::reading{#reading", shaped)(text.Markdown) {
			t.Fatalf("version %d's live text lost stored content: %s", version, response.Body.String())
		}
		if !contains(`"type":"decision"`, `"type":"reading"`, `"shaped"`, target)(string(text.MarkdownTree)) {
			t.Fatalf("version %d's display tree lost stored content: %s", version, text.MarkdownTree)
		}
	}
	requirePointer := func() {
		t.Helper()
		var projections, blockTuples, askTuples int
		if err := database.Pool.QueryRow(t.Context(), `
			select (select count(*) from block_refs where artifact_id = $1 and block_id = 'read-it' and kind = 'shaped'),
			       (select count(*) from graph_tuples where from_kind = 'block' and from_id = a.ref_key || '#read-it' and kind = 'shaped' and asserted),
			       (select count(*) from graph_tuples where from_kind = 'ask' and from_id = $2 and kind = 'shaped')
			from artifacts a where a.id = $1
		`, id, asks["read-it"]).Scan(&projections, &blockTuples, &askTuples); err != nil || projections != 1 || blockTuples != 1 || askTuples != 0 {
			t.Fatalf("shaped projection / block tuple / ask tuple = %d/%d/%d (%v), want 1/1/0", projections, blockTuples, askTuples, err)
		}
	}
	requireRead("Introductory prose.", stored)
	if response := edit(map[string]string{"op": "replace", "find": "Introductory prose.", "with": "Edited prose."}); response.Code != http.StatusOK {
		t.Fatalf("edit beside the stored decision: %d %s", response.Code, response.Body.String())
	}
	requireRead("Edited prose.", stored+1)
	requirePointer()
	live, _ := documentTexts(t, handler, id)
	if response := upload(strings.Replace(live, "Edited prose.", "Uploaded prose.", 1)); response.Code != http.StatusCreated {
		t.Fatalf("upload beside the stored decision: %d %s", response.Code, response.Body.String())
	}
	requireRead("Uploaded prose.", stored+2)
	requirePointer()
	second := strings.Replace(decision, "#decide", "#another", 1)
	if response := edit(map[string]string{"op": "insert", "after": "end", "markdown": second}); response.Code != http.StatusOK {
		t.Fatalf("insert a second decision into the document already holding one: %d %s", response.Code, response.Body.String())
	}
	requireRead("Uploaded prose.", stored+3)
	requirePointer()
	var address string
	if err := database.Pool.QueryRow(t.Context(), `select node_id from document_blocks where artifact_id = $1 and block_id = 'another'`, id).Scan(&address); err != nil {
		t.Fatalf("read the second decision's block address: %v", err)
	}
	node, found, err := graphstore.ReadNode(t.Context(), database.Pool, "block", address)
	if err != nil || !found || node.Supertag != "block" {
		t.Fatalf("the second decision's node = %+v (found %t, %v), want a generic block", node, found, err)
	}
	var typed int
	if err := database.Pool.QueryRow(t.Context(), `select count(*) from graph_tuples where from_kind = 'block' and from_id = $1 and kind in ('uncertainty', 'decided_by', 'from_ask')`, address).Scan(&typed); err != nil || typed != 0 {
		t.Fatalf("the second decision has %d typed tuples (%v), want none", typed, err)
	}
}

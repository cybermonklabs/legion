package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest/newerversion"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc/pmdoctest"
)

// readersFirstVersion is the block schema version of the build that reads decision, reading and
// ask's shaped and creates none of them: each is read from version 6 and created from version 7
// (pmdoc/schema/blocks.json), and this build, at version 7, creates them.
const readersFirstVersion = 6

// useReadersFirstSchema makes this build's manifest, at readersFirstVersion, the one the process
// reads and writes by until t ends (pmdoctest.UseBlockSchema): the manifest of the build that
// reads the three elements before it creates them. The manifest is package-global, so a test
// that calls it does not call t.Parallel.
func useReadersFirstSchema(t *testing.T) {
	t.Helper()
	var schema pmdoc.BlockSchema
	if err := json.Unmarshal(pmdoc.SchemaJSON(), &schema); err != nil {
		t.Fatalf("decode the block schema: %v", err)
	}
	schema.Version = readersFirstVersion
	pmdoctest.UseBlockSchema(t, schema)
}

// The readers-first rule, held under the manifest of the build that reads the new elements before
// it creates them (useReadersFirstSchema): a write that brings decision, reading or ask's shaped
// into a document holding none of it is refused, and a document a later build stored with them
// reads, edits and takes more of them beside them. This build is the slice that writes their
// nodes, so a decision written beside a stored one is a decision node (refs' TypedFacts) even
// under that manifest.
func TestTheNewTypesAreReadButNotWrittenBeforeTheirNodePass(t *testing.T) {
	useReadersFirstSchema(t)
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
	// suggest opens, as a session, a suggestion on issue key's spec of with over quote, and answers
	// its id; accept accepts it as alice.
	suggest := func(key, quote, with string) string {
		t.Helper()
		created := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/comments", map[string]any{
			"body": "suggested", "anchor": map[string]any{"artifact": "spec", "quote": quote},
			"suggestion": map[string]string{"replace_with": with}, "actor": sessionActor(),
		})
		if created.Code != http.StatusCreated {
			t.Fatalf("suggest %q over %q on %s: %d %s", with, quote, key, created.Code, created.Body.String())
		}
		return decodeBody[struct {
			ID string `json:"id"`
		}](t, created).ID
	}
	accept := func(comment string) *httptest.ResponseRecorder {
		return dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment+"/accept", map[string]any{}, "alice")
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
	for index, tc := range []struct{ element, markdown, insert string }{
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
			{"doc-edit replace", func() *httptest.ResponseRecorder {
				return edit(map[string]string{"op": "replace", "find": "Introductory prose.", "with": "Introductory prose.\n\n" + tc.insert})
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
		// An accepted suggestion writes its text into the document as an insert does, so one that
		// brings the element into a document holding none of it is refused too. Each suggestion
		// is made on a document of a project of its own, which holds none of the three.
		plain := createInteractionIssue(t, handler, "SUG"+string(rune('A'+index)), "Suggest "+tc.element, intro+requirement)
		suggestion := suggest(plain.Key, "Introductory prose.", "Introductory prose.\n\n"+tc.insert)
		unaccepted := latestVersion(t, database, plain.PrimaryArtifactID).Number
		response := accept(suggestion)
		refusal := decodeBody[struct {
			Error string `json:"error"`
		}](t, response)
		if response.Code != http.StatusBadRequest || !strings.Contains(refusal.Error, want) {
			t.Fatalf("accepted suggestion of %s = %d %s, want refusal %q", tc.element, response.Code, response.Body.String(), want)
		}
		if got := latestVersion(t, database, plain.PrimaryArtifactID).Number; got != unaccepted {
			t.Fatalf("accepted suggestion of %s changed version from %d to %d", tc.element, unaccepted, got)
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
	// The stored ask block's shaped is read as this build reads it, under the readers-first manifest
	// too: one block_refs row, and its tuple on the ask's node, where this build's node pass hangs an
	// ask block's shaped (refs' TypedFacts), and none on the block's own node, where the build that
	// reads it first, which has no node pass, states it.
	requirePointer := func() {
		t.Helper()
		var projections, blockTuples, askTuples int
		if err := database.Pool.QueryRow(t.Context(), `
			select (select count(*) from block_refs where artifact_id = $1 and block_id = 'read-it' and kind = 'shaped'),
			       (select count(*) from graph_tuples where from_kind = 'block' and from_id = a.ref_key || '#read-it' and kind = 'shaped'),
			       (select count(*) from graph_tuples where from_kind = 'ask' and from_id = $2 and kind = 'shaped' and asserted)
			from artifacts a where a.id = $1
		`, id, asks["read-it"]).Scan(&projections, &blockTuples, &askTuples); err != nil || projections != 1 || blockTuples != 0 || askTuples != 1 {
			t.Fatalf("shaped projection / block tuple / ask tuple = %d/%d/%d (%v), want 1/0/1", projections, blockTuples, askTuples, err)
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
	// Before the node pass the build that reads decisions first wrote the second decision a generic
	// block with no typed tuple. This build is that pass, so the decision it writes beside the
	// stored one is a decision node stating its uncertainty, under the readers-first manifest too.
	var address string
	if err := database.Pool.QueryRow(t.Context(), `select node_id from document_blocks where artifact_id = $1 and block_id = 'another'`, id).Scan(&address); err != nil {
		t.Fatalf("read the second decision's block address: %v", err)
	}
	node, found, err := graphstore.ReadNode(t.Context(), database.Pool, "block", address)
	if err != nil || !found || node.Supertag != "decision" {
		t.Fatalf("the second decision's node = %+v (found %t, %v), want a decision node", node, found, err)
	}
	var typed int
	if err := database.Pool.QueryRow(t.Context(), `select count(*) from graph_tuples where from_kind = 'block' and from_id = $1 and kind = 'uncertainty' and asserted`, address).Scan(&typed); err != nil || typed == 0 {
		t.Fatalf("the second decision states %d uncertainty tuples (%v), want its uncertainty stated", typed, err)
	}
	// A replace and an accepted suggestion whose text holds a decision write it beside the stored
	// one, as the insert above does, and each is accepted.
	replaced := strings.Replace(decision, "#decide", "#replaced", 1)
	if response := edit(map[string]string{"op": "replace", "find": "Uploaded prose.", "with": "Replaced prose.\n\n" + replaced}); response.Code != http.StatusOK {
		t.Fatalf("replace with a decision beside the stored decision: %d %s", response.Code, response.Body.String())
	}
	requireRead("Replaced prose.", stored+4)
	requirePointer()
	suggested := strings.Replace(decision, "#decide", "#suggested", 1)
	if response := accept(suggest(issue.Key, "Replaced prose.", "Suggested prose.\n\n"+suggested)); response.Code != http.StatusOK {
		t.Fatalf("accept a suggestion of a decision beside the stored decision: %d %s", response.Code, response.Body.String())
	}
	requireRead("Suggested prose.", latestVersion(t, database, id).Number)
	requirePointer()
	if live, _ := documentTexts(t, handler, id); !strings.Contains(live, ":::decision{#replaced") || !strings.Contains(live, ":::decision{#suggested") {
		t.Fatalf("the live text after the replace and the accept lacks a decision either wrote, want both:\n%s", live)
	}
}

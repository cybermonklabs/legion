package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/testwait"
)

// An answered block ask reads answered in the asks member of its document's text, with who
// answered and the words, and the markdown beside it carries none of that.
func TestAnAnsweredBlockAskReadsAnsweredInTheAsksOfItsDocumentsText(t *testing.T) {
	t.Parallel()
	var documentService *docs.Service
	handler, database := newInteractionHandler(t, func(database *store.Store) docs.API {
		documentService = docs.New(docs.Deps{Store: database, Settle: time.Hour})
		t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
		return documentService
	})
	issue := createInteractionIssue(t, handler, "TEST", "Answer typed ask", "Before\n")
	if _, err := replaceDocumentText(database, documentService, issue.PrimaryArtifactID, ":::ask{#ask-1 urgency=\"med\" multiple=\"false\" state=\"open\"}\nShip it?\n:::\n", model.Actor{Kind: "session", ID: "session-1"}); err != nil {
		t.Fatalf("write ask block: %v", err)
	}
	created := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Ship it?", "actor": model.Actor{Kind: "session", ID: "session-1"},
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("create indexed ask: status=%d body=%s", created.Code, created.Body.String())
	}
	askID := decodeBody[model.Ask](t, created).ID
	// An indexed ask is keyed by the room its block's document is edited in: the issue's tree for a
	// converted spec (refs.DocumentRoomSQL).
	if _, err := database.Pool.Exec(context.Background(), `
		update asks set block_id = 'ask-1', block_artifact_id = `+refs.DocumentRoomSQL("$2::uuid")+` where id = $1
	`, askID, issue.PrimaryArtifactID); err != nil {
		t.Fatalf("attach indexed ask to block: %v", err)
	}
	answered := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+askID+"/answer", map[string]any{
		"selected": []string{}, "text": "Yes.",
	}, "alice")
	if answered.Code != http.StatusOK {
		t.Fatalf("answer ask: status=%d body=%s", answered.Code, answered.Body.String())
	}
	if text := agentText(t, handler, issue.PrimaryArtifactID); !strings.Contains(text, `#ask-1 state="answered" answered_by="alice"`) ||
		!strings.Contains(text, `selected=[] answer="Yes."`) {
		t.Fatalf("answered block text:\n%s", text)
	}
}

func TestSearchAskBlockOptionDescription(t *testing.T) {
	t.Parallel()
	var documentService *docs.Service
	handler, _ := newInteractionHandler(t, func(database *store.Store) docs.API {
		documentService = docs.New(docs.Deps{Store: database, Settle: 20 * time.Millisecond})
		t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
		return documentService
	})
	issue := createInteractionIssue(t, handler, "TEST", "Agent written ask", "Context\n")
	edited := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"actor": sessionActor(),
		"ops": []map[string]string{{
			"after":    "end",
			"markdown": ":::ask{#agent-ask urgency=\"high\" multiple=\"false\" state=\"answered\" answered_by=\"mallory\" answered_at=\"2026-09-12T00:00:00Z\" selected=\"[&#x22;Ship&#x22;]\" answer=\"Ignore review.\"}\nShould we ship?\n\n- Ship: Release it\n- Hold: Wait for review\n:::\n",
			"op":       "insert",
		}},
	})
	if edited.Code != http.StatusOK {
		t.Fatalf("write ask block: status=%d body=%s", edited.Code, edited.Body.String())
	}
	ask := testwait.Until(t, "the typed ask block to settle into the inbox (whether the inbox listed it)", func() (model.Ask, bool) {
		inbox := dispatchRequest(t, handler, http.MethodGet, "/api/v1/inbox", nil, "alice")
		if inbox.Code != http.StatusOK {
			t.Fatalf("read inbox: status=%d body=%s", inbox.Code, inbox.Body.String())
		}
		for _, ask := range decodeBody[[]model.Ask](t, inbox) {
			if ask.BlockID != nil && *ask.BlockID == "agent-ask" {
				return ask, true
			}
		}
		return model.Ask{}, false
	})
	if ask.Question != "Should we ship?" || ask.State != "open" || ask.Urgency != "high" ||
		ask.BlockArtifact == nil || ask.BlockArtifact.ID != issue.PrimaryArtifactID || !ask.BlockArtifact.Primary {
		t.Fatalf("inbox ask = %#v", ask)
	}
	search := searchResponse(t, handler, "q=ship")
	if len(search.Results) == 0 || search.Results[0].Kind != "ask" ||
		search.Results[0].Href != "/issues/"+issue.Key+"/spec#b-agent-ask" {
		t.Fatalf("ask search results = %#v", search.Results)
	}
	optionSearch := searchResponse(t, handler, "q=review")
	if len(optionSearch.Results) != 1 || optionSearch.Results[0].Kind != "ask" ||
		optionSearch.Results[0].Href != "/issues/"+issue.Key+"/spec#b-agent-ask" {
		t.Fatalf("option-label search results = %#v", optionSearch.Results)
	}
	text := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/text", nil, "alice")
	if text.Code != http.StatusOK || strings.Contains(text.Body.String(), `state=\"answered\"`) {
		t.Fatalf("agent-owned server state survived in document: status=%d body=%s", text.Code, text.Body.String())
	}
	events := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/events", nil, "alice")
	if events.Code != http.StatusOK || strings.Contains(events.Body.String(), `"type":"block.repaired"`) {
		t.Fatalf("agent write emitted a repair event: status=%d body=%s", events.Code, events.Body.String())
	}
}

func TestAskBlockWithoutOptionsSettles(t *testing.T) {
	t.Parallel()
	var documentService *docs.Service
	handler, _ := newInteractionHandler(t, func(database *store.Store) docs.API {
		documentService = docs.New(docs.Deps{Store: database, Settle: 20 * time.Millisecond})
		t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
		return documentService
	})
	issue := createInteractionIssue(t, handler, "TEST", "No-option ask", "Context\n")
	edited := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"actor": sessionActor(),
		"ops": []map[string]string{{
			"after":    "end",
			"markdown": ":::ask{#no-options urgency=\"med\" multiple=\"false\" state=\"open\"}\nProceed?\n:::\n",
			"op":       "insert",
		}},
	})
	if edited.Code != http.StatusOK {
		t.Fatalf("write no-option ask: status=%d body=%s", edited.Code, edited.Body.String())
	}
	waitUntil(t, "the no-option ask to settle into the inbox", func() bool {
		inbox := dispatchRequest(t, handler, http.MethodGet, "/api/v1/inbox", nil, "alice")
		for _, ask := range decodeBody[[]model.Ask](t, inbox) {
			if ask.BlockID != nil && *ask.BlockID == "no-options" {
				return true
			}
		}
		return false
	})
}

func TestDocumentRetypeRejectsUnknownTypeAndInvalidAttributes(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Retype validation", "Question\n")
	blocks := decodeBody[[]model.ArtifactBlock](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/blocks", nil, "alice"))
	if len(blocks) == 0 {
		t.Fatal("document has no blocks")
	}
	for _, test := range []struct {
		name  string
		op    map[string]any
		field string
	}{
		{name: "unknown type", op: map[string]any{"op": "retype", "block": blocks[0].ID, "type": "missing"}, field: "type"},
		{name: "invalid attributes", op: map[string]any{"op": "retype", "block": blocks[0].ID, "type": "ask", "attributes": map[string]any{"urgency": "now"}}, field: "attributes"},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{"ops": []map[string]any{test.op}}, "alice")
			if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"INVALID_OP"`) || !strings.Contains(response.Body.String(), test.field) {
				t.Fatalf("retype response: status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}

// Markdown that becomes a document is held to an ask block's content rule, paragraph+
// bullet_list? - one or more paragraphs, then at most one bullet list, last - wherever it enters: a
// spec at issue creation, a new document, a new version of one. The browser editor's parser
// refuses to build such a block, and stored, it would refuse every later edit to the document. What
// the rule allows is taken, including an option without a label or a question that is only an
// image, which settlement flags `invalid` and the dashboard shows as a malformed decision.
func TestUploadsHoldAskBlocksToTheirContentRule(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	ask := func(body string) string {
		return "Intro.\n\n:::ask{#a1 urgency=\"med\" multiple=\"false\" state=\"open\"}\n" + body + "\n:::\n\nAfter.\n"
	}
	issue := createInteractionIssue(t, handler, "UP", "Uploads", "Intro.\n")
	uploaded := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]any{
		"name": "notes.md", "content": "Notes.\n",
	}, "alice")
	if uploaded.Code != http.StatusCreated {
		t.Fatalf("upload notes: status=%d body=%s", uploaded.Code, uploaded.Body.String())
	}
	notes := decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
	}](t, uploaded).Artifact
	routes := func(index int, content string) map[string]func() *httptest.ResponseRecorder {
		return map[string]func() *httptest.ResponseRecorder{
			"create an issue": func() *httptest.ResponseRecorder {
				return dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{"project": "UP", "title": fmt.Sprintf("Decision %d: %s", index, strings.Repeat("x", index+1)), "spec": content, "force": true}, "alice")
			},
			"upload a document": func() *httptest.ResponseRecorder {
				return dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]any{"name": fmt.Sprintf("doc-%d.md", index), "content": content}, "alice")
			},
		}
	}
	for index, body := range []string{
		"Which?\n\n```\ncode\n```",
		"Which?\n\n- A\n- B\n\nAn afterthought.",
		"Which?\n\n- A\n\n1. B",
		"- A\n- B",
	} {
		for route, send := range routes(index, ask(body)) {
			if response := send(); response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"INVALID_ASK_BLOCK"`) || !strings.Contains(response.Body.String(), "paragraph+ bullet_list?") {
				t.Fatalf("%s with %q: status=%d body=%s", route, body, response.Code, response.Body.String())
			}
		}
		version := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]any{"name": "notes.md", "content": ask(body), "precondition": uploadBase(t, handler, notes.ID)}, "alice")
		if version.Code != http.StatusBadRequest || !strings.Contains(version.Body.String(), `"code":"INVALID_ASK_BLOCK"`) {
			t.Fatalf("upload a version with %q: status=%d body=%s", body, version.Code, version.Body.String())
		}
	}
	text := decodeBody[struct {
		Markdown string `json:"markdown"`
	}](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+notes.ID+"/text", nil, "alice"))
	if text.Markdown != "Notes.\n" {
		t.Fatalf("notes after refused versions = %q, want them unchanged", text.Markdown)
	}
	for index, body := range []string{
		"Which path?\n\n- Ship: Release it\n- : No label",
		"![a diagram](https://x.test/d.png)\n\n- A\n- B",
	} {
		for route, send := range routes(10+index, ask(body)) {
			if response := send(); response.Code != http.StatusCreated {
				t.Fatalf("%s with %q: status=%d body=%s", route, body, response.Code, response.Body.String())
			}
		}
	}
}

func TestDocumentEditRejectsInvalidAskBlockAtomically(t *testing.T) {
	t.Parallel()
	var documentService *docs.Service
	handler, database := newInteractionHandler(t, func(database *store.Store) docs.API {
		documentService = docs.New(docs.Deps{Store: database, Settle: 20 * time.Millisecond})
		t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
		return documentService
	})
	issue := createInteractionIssue(t, handler, "TEST", "Reject malformed ask edit", "Context\n")
	created := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"actor": sessionActor(),
		"ops": []map[string]string{{
			"after":    "end",
			"markdown": ":::ask{#agent-ask urgency=\"med\" multiple=\"false\" state=\"open\"}\nShould we ship?\n\n- Ship: Release it\n- Hold: Wait for review\n:::\n",
			"op":       "insert",
		}},
	})
	if created.Code != http.StatusOK {
		t.Fatalf("create typed ask block: status=%d body=%s", created.Code, created.Body.String())
	}

	var askID, question string
	var optionsJSON []byte
	waitUntil(t, "the typed ask block to settle into an ask", func() bool {
		return database.Pool.QueryRow(context.Background(), `
			select id::text, question, options from asks
			where `+refs.AsksOfDocumentSQL("asks", "$1::uuid")+` and block_id = 'agent-ask'
		`, issue.PrimaryArtifactID).Scan(&askID, &question, &optionsJSON) == nil
	})
	var versionsBefore int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from artifact_versions where artifact_id = $1
	`, issue.PrimaryArtifactID).Scan(&versionsBefore); err != nil {
		t.Fatalf("count versions before rejected edit: %v", err)
	}

	rejected := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"actor": sessionActor(),
		"ops":   []map[string]string{{"op": "replace", "find": "Ship", "with": ""}},
	})
	const reason = `ask block "agent-ask" has an option without a label`
	errorBody := decodeBody[struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}](t, rejected)
	if rejected.Code != http.StatusBadRequest || errorBody.Code != "INVALID_ASK_BLOCK" || errorBody.Error != reason {
		t.Fatalf("reject malformed ask edit: status=%d body=%s", rejected.Code, rejected.Body.String())
	}
	var versionsAfter int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from artifact_versions where artifact_id = $1
	`, issue.PrimaryArtifactID).Scan(&versionsAfter); err != nil {
		t.Fatalf("count versions after rejected edit: %v", err)
	}
	if versionsAfter != versionsBefore {
		t.Fatalf("versions after rejected edit = %d, want %d", versionsAfter, versionsBefore)
	}
	var questionAfter string
	var optionsAfter []byte
	if err := database.Pool.QueryRow(context.Background(), `
		select question, options from asks where id = $1
	`, askID).Scan(&questionAfter, &optionsAfter); err != nil {
		t.Fatalf("load ask after rejected edit: %v", err)
	}
	if questionAfter != question || string(optionsAfter) != string(optionsJSON) {
		t.Fatalf("ask changed after rejected edit: question=%q options=%s", questionAfter, optionsAfter)
	}
}

func TestDocumentEditExplainsRenderedQuoteMiss(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Rendered quote miss", "Use `config` with care.\n")
	response := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"actor": sessionActor(),
		"ops": []map[string]string{{
			"op":   "replace",
			"find": "Use `missing`",
			"with": "updated",
		}},
	})
	errorBody := decodeBody[struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}](t, response)
	if response.Code != http.StatusNotFound || errorBody.Code != "TARGET_NOT_FOUND" {
		t.Fatalf("rendered quote miss: status=%d code=%q error=%q", response.Code, errorBody.Code, errorBody.Error)
	}
	for _, want := range []string{
		"operation 0",
		"quote \"Use `missing`\"",
		"the block text as rendered",
		`nearest blocks: "Use config with care."`,
	} {
		if !strings.Contains(errorBody.Error, want) {
			t.Fatalf("rendered quote miss = %q, want it to name %q", errorBody.Error, want)
		}
	}
}

// awaitIndexedAskBlock polls the inbox until the ask block with blockID on artifactID is
// indexed, failing the test when settlement never reaches it. Its one bound is the test binary's
// deadline (testwait.UntilPolling): a settlement a loaded machine starts late still ends the wait,
// and its failure names the block ids the inbox last listed.
func awaitIndexedAskBlock(t *testing.T, handler http.Handler, artifactID, blockID, question string) {
	t.Helper()
	testwait.UntilPolling(t, 20*time.Millisecond, fmt.Sprintf("ask block %q on %s to be indexed (the inbox's ask blocks)", blockID, artifactID), func() ([]string, bool) {
		inbox := dispatchRequest(t, handler, http.MethodGet, "/api/v1/inbox", nil, "alice")
		if inbox.Code != http.StatusOK {
			t.Fatalf("read inbox: status=%d body=%s", inbox.Code, inbox.Body.String())
		}
		var listed []string
		for _, ask := range decodeBody[[]model.Ask](t, inbox) {
			if ask.BlockID == nil {
				continue
			}
			listed = append(listed, *ask.BlockID)
			if *ask.BlockID != blockID {
				continue
			}
			if ask.Question != question || ask.State != "open" || ask.BlockArtifact == nil || ask.BlockArtifact.ID != artifactID {
				t.Fatalf("indexed ask = %#v", ask)
			}
			return listed, true
		}
		return listed, false
	})
}

// A spec written at issue creation never passes through a live edit; its ask blocks must still
// become asks, or an agent that opens an issue with decisions in the spec gets no inbox rows.
func TestAskBlocksInCreatedSpecAreIndexed(t *testing.T) {
	t.Parallel()
	var documentService *docs.Service
	handler, _ := newInteractionHandler(t, func(database *store.Store) docs.API {
		documentService = docs.New(docs.Deps{Store: database, Settle: 20 * time.Millisecond})
		t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
		return documentService
	})
	issue := createInteractionIssue(t, handler, "TEST", "Seeded decisions",
		"Context\n\n:::ask{#seeded-ask urgency=\"med\" multiple=\"false\"}\nShip the seeded decision?\n\n- Ship: Now.\n- Hold: Later.\n:::\n")
	awaitIndexedAskBlock(t, handler, issue.PrimaryArtifactID, "seeded-ask", "Ship the seeded decision?")
}

// Uploading a new document version through the artifacts API replaces the text inside the
// upload's own transaction, which suppresses live settlement; the closer must still run.
func TestAskBlocksInUploadedSpecVersionAreIndexed(t *testing.T) {
	t.Parallel()
	var documentService *docs.Service
	handler, _ := newInteractionHandler(t, func(database *store.Store) docs.API {
		documentService = docs.New(docs.Deps{Store: database, Settle: 20 * time.Millisecond})
		t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
		return documentService
	})
	issue := createInteractionIssue(t, handler, "TEST", "Uploaded decisions", "Before\n")
	uploaded := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]any{
		"actor":        sessionActor(),
		"name":         "spec.md",
		"content":      "Before\n\n:::ask{#uploaded-ask urgency=\"high\" multiple=\"false\"}\nShip the uploaded decision?\n\n- Ship: Now.\n- Hold: Later.\n:::\n",
		"summary":      "decisions as ask blocks",
		"precondition": uploadBase(t, handler, issue.PrimaryArtifactID),
	})
	if uploaded.Code != http.StatusCreated {
		t.Fatalf("upload spec version: status=%d body=%s", uploaded.Code, uploaded.Body.String())
	}
	awaitIndexedAskBlock(t, handler, issue.PrimaryArtifactID, "uploaded-ask", "Ship the uploaded decision?")
}

// A record copy of a spec uploaded as a second document of the issue carries the spec's ask
// blocks under their ids. It opens no ask in anyone's Inbox: each copied block shows the state and
// answer of the ask it was copied from (LEGION-651).
func TestAnUploadedCopyOfASpecOpensNoAskForItsCopiedBlocks(t *testing.T) {
	t.Parallel()
	var documentService *docs.Service
	handler, database := newInteractionHandler(t, func(database *store.Store) docs.API {
		documentService = docs.New(docs.Deps{Store: database, Settle: 20 * time.Millisecond})
		t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
		return documentService
	})
	spec := "Context\n\n" +
		":::ask{#rec-answered urgency=\"med\" multiple=\"false\"}\nShip on Monday?\n\n- Yes: Monday.\n- No: Later.\n:::\n\n" +
		":::ask{#rec-resolved urgency=\"med\" multiple=\"false\"}\nWrite the changelog?\n:::\n\n" +
		":::ask{#rec-open urgency=\"med\" multiple=\"false\"}\nWhich region?\n:::\n"
	issue := createInteractionIssue(t, handler, "TEST", "Record copy", spec)
	for _, block := range []struct{ id, question string }{
		{"rec-answered", "Ship on Monday?"}, {"rec-resolved", "Write the changelog?"}, {"rec-open", "Which region?"},
	} {
		awaitIndexedAskBlock(t, handler, issue.PrimaryArtifactID, block.id, block.question)
	}
	asks := map[string]string{}
	listed := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/asks", nil, "alice")
	for _, ask := range decodeBody[[]model.Ask](t, listed) {
		if ask.BlockID != nil {
			asks[*ask.BlockID] = ask.ID
		}
	}
	if answered := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+asks["rec-answered"]+"/answer", map[string]any{
		"selected": []string{"Yes"},
	}, "alice"); answered.Code != http.StatusOK {
		t.Fatalf("answer: status=%d body=%s", answered.Code, answered.Body.String())
	}
	if resolved := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+asks["rec-resolved"]+"/resolve", map[string]any{
		"kind": "resolved", "reason": "Waived.",
	}, "alice"); resolved.Code != http.StatusOK {
		t.Fatalf("resolve: status=%d body=%s", resolved.Code, resolved.Body.String())
	}
	if shown := agentText(t, handler, issue.PrimaryArtifactID); !strings.Contains(shown, `#rec-answered state="answered"`) || !strings.Contains(shown, `#rec-resolved state="resolved"`) {
		t.Fatalf("spec before the copy:\n%s", shown)
	}
	record, _ := documentTexts(t, handler, issue.PrimaryArtifactID)

	uploaded := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]any{
		"actor": sessionActor(), "name": "spec-record.md", "content": record,
	})
	if uploaded.Code != http.StatusCreated {
		t.Fatalf("upload the record copy: status=%d body=%s", uploaded.Code, uploaded.Body.String())
	}
	copied := decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
	}](t, uploaded).Artifact.ID
	testwait.UntilPolling(t, 20*time.Millisecond, "the copy to show its sources' states (its text as an agent reads it)", func() (string, bool) {
		text := agentText(t, handler, copied)
		return text, contains(
			`#rec-answered state="answered" answered_by="alice"`,
			`copied_from="`+asks["rec-answered"]+`"`,
			`#rec-resolved state="resolved" copied_from="`+asks["rec-resolved"]+`"`,
			`#rec-open state="open" copied_from="`+asks["rec-open"]+`"`,
		)(text)
	})
	// A settlement is what would open the copy's asks: once the copy owes none, no later pass runs
	// over it that the read below could come before.
	awaitSettled(t, database, copied)
	var onCopy []string
	all := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/asks", nil, "alice")
	for _, ask := range decodeBody[[]model.Ask](t, all) {
		if ask.BlockArtifact != nil && ask.BlockArtifact.ID == copied {
			onCopy = append(onCopy, *ask.BlockID+" "+ask.State)
		}
	}
	if len(onCopy) != 0 {
		t.Fatalf("the record copy opened asks %v, want none", onCopy)
	}
}

// documentTexts is document's live text, as the text routes serve it, and the markdown of its
// latest version, "" when it has none.
func documentTexts(t *testing.T, handler http.Handler, document string) (string, string) {
	t.Helper()
	read := decodeBody[struct {
		Markdown string `json:"markdown"`
		Version  *int   `json:"version"`
	}](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+document+"/text", nil, "alice"))
	if read.Version == nil {
		return read.Markdown, ""
	}
	version := decodeBody[struct {
		Markdown string `json:"markdown"`
	}](t, dispatchRequest(t, handler, http.MethodGet, fmt.Sprintf("/api/v1/artifacts/%s/versions/%d", document, *read.Version), nil, "alice"))
	return read.Markdown, version.Markdown
}

// awaitDocumentText polls document until its live text and its latest version both hold, failing
// the test at once when either shows refused (nil refuses nothing). Its one bound is the test
// binary's deadline (testwait.UntilPolling), and its failure prints the two texts it last read.
func awaitDocumentText(t *testing.T, handler http.Handler, document, what string, holds, refused func(string) bool) {
	t.Helper()
	type texts struct{ live, version string }
	testwait.UntilPolling(t, 20*time.Millisecond, "the document to show "+what+" (its live text and its latest version)", func() (texts, bool) {
		live, version := documentTexts(t, handler, document)
		if refused != nil && (refused(live) || refused(version)) {
			t.Fatalf("the document showed what it must not while waiting for %s; live:\n%s\nlatest version:\n%s", what, live, version)
		}
		return texts{live, version}, holds(live) && holds(version)
	})
}

// awaitVersionText polls document until its latest version holds, failing the test after ten
// seconds. Every write keeps what a version keeps of each ask in its block, in the live document
// and its versions (docs' keptAskState), and the text routes take it off what they serve, so a
// test reads it in the versions.
func awaitVersionText(t *testing.T, handler http.Handler, document, what string, holds func(string) bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		_, version := documentTexts(t, handler, document)
		if holds(version) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the document's latest version never showed %s:\n%s", what, version)
		}
	}
}

// askServerAttributes are the server-owned attributes of an ask block, none of which the text
// routes serve on any ask block (docs.TextWithAsks).
var askServerAttributes = [...]string{"state", "answered_by", "answered_at", "selected", "answer", "invalid", "copied_from", "copied_from_document"}

// agentText is document's text as an agent reads it (GET /api/v1/artifacts/{id}/text): its
// markdown, then a line `asks:` and one line per entry of its asks member (askLines). A person's
// answer is held on the ask's node alone, so the asks member is where a
// test reads it beside the text. It fails t where an ask block of the markdown carries a
// server-owned attribute (askServerAttributes).
func agentText(t *testing.T, handler http.Handler, document string) string {
	t.Helper()
	read := decodeBody[struct {
		Markdown string          `json:"markdown"`
		Asks     []model.TextAsk `json:"asks"`
	}](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+document+"/text", nil, "alice"))
	for _, line := range strings.Split(read.Markdown, "\n") {
		kind, attrs, opens := pmdoc.ParseTypedDirectiveOpen(strings.TrimLeft(line, " \t>"))
		if !opens || kind != "ask" {
			continue
		}
		for _, name := range askServerAttributes {
			if _, held := attrs[name]; held {
				t.Fatalf("the text of %s an agent reads carries the server-owned ask attribute %s:\n%s", document, name, read.Markdown)
			}
		}
	}
	return read.Markdown + "\nasks:\n" + askLines(read.Asks)
}

// askLines is entries as these tests compare them, one line each, each ended by a newline: `#`
// and the block id, then each value the entry holds as name="value", in the order the release
// before wrote them on the block's opening line, the options chosen as their JSON list.
func askLines(entries []model.TextAsk) string {
	var lines strings.Builder
	for _, entry := range entries {
		lines.WriteString("#" + entry.BlockID)
		if entry.State != "" {
			lines.WriteString(` state="` + entry.State + `"`)
		}
		if entry.AnsweredBy != "" {
			lines.WriteString(` answered_by="` + entry.AnsweredBy + `"`)
		}
		if entry.AnsweredAt != "" {
			lines.WriteString(` answered_at="` + entry.AnsweredAt + `"`)
		}
		if entry.Selected != nil {
			selected, _ := json.Marshal(*entry.Selected)
			lines.WriteString(" selected=" + string(selected))
		}
		if entry.Answer != nil {
			lines.WriteString(` answer="` + *entry.Answer + `"`)
		}
		if entry.CopiedFrom != "" {
			lines.WriteString(` copied_from="` + entry.CopiedFrom + `"`)
		}
		if entry.CopiedFromDocument != "" {
			lines.WriteString(` copied_from_document="` + entry.CopiedFromDocument + `"`)
		}
		lines.WriteString("\n")
	}
	return lines.String()
}

// awaitTextShows polls document's text as an agent reads it (agentText: its markdown and the
// entries of its asks member) until it holds, and answers that text; it fails the test after ten
// seconds, or at once when the text shows refused (nil refuses nothing). A person's answer is held
// on the ask's node alone, so no stored version of a document written
// on a fleet of one build shows it: the asks member is where a test reads it.
func awaitTextShows(t *testing.T, handler http.Handler, document, what string, holds, refused func(string) bool) string {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		text := agentText(t, handler, document)
		if refused != nil && refused(text) {
			t.Fatalf("the document's text showed what it must not while waiting for %s:\n%s", what, text)
		}
		if holds(text) {
			return text
		}
		if time.Now().After(deadline) {
			t.Fatalf("the document's text never showed %s:\n%s", what, text)
		}
	}
}

// contains is a predicate holding of text that contains every one of parts.
func contains(parts ...string) func(string) bool {
	return func(text string) bool {
		for _, part := range parts {
			if !strings.Contains(text, part) {
				return false
			}
		}
		return true
	}
}

// An answer to a copy's source, or its resolution, made after the copy settled reaches the copy's
// asks entry as an agent reads it beside the text, which shows the source's answer state and
// answer, read from the store (docs.TextWithAsks): a person's answer is held on the ask's node
// alone, so no stored version of the copy holds it. The routes settle
// the copies of the ask they close, so the copy owes nothing after them and holds no ask of its own.
func TestAnsweringOrResolvingACopysSourceSettlesTheCopy(t *testing.T) {
	t.Parallel()
	var documentService *docs.Service
	handler, database := newInteractionHandler(t, func(database *store.Store) docs.API {
		documentService = docs.New(docs.Deps{Store: database, Settle: 20 * time.Millisecond})
		t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
		return documentService
	})
	spec := "Context\n\n" +
		":::ask{#later-answered urgency=\"med\" multiple=\"false\"}\nShip on Monday?\n\n- Yes: Monday.\n- No: Later.\n:::\n\n" +
		":::ask{#later-resolved urgency=\"med\" multiple=\"false\"}\nWrite the changelog?\n:::\n"
	issue := createInteractionIssue(t, handler, "TEST", "Source answered after its copy", spec)
	awaitIndexedAskBlock(t, handler, issue.PrimaryArtifactID, "later-answered", "Ship on Monday?")
	awaitIndexedAskBlock(t, handler, issue.PrimaryArtifactID, "later-resolved", "Write the changelog?")
	asks := map[string]string{}
	for _, ask := range decodeBody[[]model.Ask](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/asks", nil, "alice")) {
		if ask.BlockID != nil {
			asks[*ask.BlockID] = ask.ID
		}
	}
	uploaded := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]any{
		"actor": sessionActor(), "name": "spec-record.md", "content": spec,
	})
	if uploaded.Code != http.StatusCreated {
		t.Fatalf("upload the copy: status=%d body=%s", uploaded.Code, uploaded.Body.String())
	}
	copied := decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
	}](t, uploaded).Artifact.ID
	awaitTextShows(t, handler, copied, "its open sources", contains(
		`#later-answered state="open" copied_from="`+asks["later-answered"]+`"`,
		`#later-resolved state="open" copied_from="`+asks["later-resolved"]+`"`,
	), nil)

	if answered := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+asks["later-answered"]+"/answer", map[string]any{
		"selected": []string{"Yes"}, "text": "From the record.",
	}, "alice"); answered.Code != http.StatusOK {
		t.Fatalf("answer the source: status=%d body=%s", answered.Code, answered.Body.String())
	}
	if resolved := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+asks["later-resolved"]+"/resolve", map[string]any{
		"kind": "resolved", "reason": "Waived.",
	}, "alice"); resolved.Code != http.StatusOK {
		t.Fatalf("resolve the source: status=%d body=%s", resolved.Code, resolved.Body.String())
	}
	awaitTextShows(t, handler, copied, "its sources' answer and resolution", contains(
		`#later-answered state="answered" answered_by="alice" `,
		`selected=["Yes"] answer="From the record." copied_from="`+asks["later-answered"]+`"`,
		`#later-resolved state="resolved" copied_from="`+asks["later-resolved"]+`"`,
	), nil)
	awaitSettled(t, database, copied)
	_, version := documentTexts(t, handler, copied)
	requireNoAnswerAttributes(t, "the copy's latest version", version)
	var onCopy, pending int
	if err := database.Pool.QueryRow(context.Background(), `
		select (select count(*) from asks where block_artifact_id = $1),
			(select count(*) from doc_settlements_pending where artifact_id = $1)
	`, copied).Scan(&onCopy, &pending); err != nil {
		t.Fatalf("read the copy's asks and pending settlement: %v", err)
	}
	if onCopy != 0 || pending != 0 {
		t.Fatalf("the copy holds %d asks and %d pending settlements, want none of either", onCopy, pending)
	}
}

// A record copy taken while an ask was open asks the ask's wording of then. Once the ask is
// reworded with other options (PATCH) and the new wording is answered, the answer route settles the
// copy (SettleCopiesOf), and that settlement opens the copy's own ask for its block rather than
// writing the answer to a question the copy does not ask into the record.
func TestAnAnswerToACopysSourceAfterItsRewordingOpensTheCopysOwnAsk(t *testing.T) {
	t.Parallel()
	var documentService *docs.Service
	handler, database := newInteractionHandler(t, func(database *store.Store) docs.API {
		documentService = docs.New(docs.Deps{Store: database, Settle: 20 * time.Millisecond})
		t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
		return documentService
	})
	spec := "Context\n\n:::ask{#ship urgency=\"med\" multiple=\"false\"}\nShip on Monday?\n\n- Yes: Monday.\n- No: Later.\n:::\n"
	issue := createInteractionIssue(t, handler, "TEST", "Source reworded after its copy", spec)
	awaitIndexedAskBlock(t, handler, issue.PrimaryArtifactID, "ship", "Ship on Monday?")
	var source string
	for _, ask := range decodeBody[[]model.Ask](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/asks", nil, "alice")) {
		if ask.BlockID != nil && *ask.BlockID == "ship" {
			source = ask.ID
		}
	}
	uploaded := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]any{
		"actor": sessionActor(), "name": "spec-record.md", "content": spec,
	})
	if uploaded.Code != http.StatusCreated {
		t.Fatalf("upload the copy: status=%d body=%s", uploaded.Code, uploaded.Body.String())
	}
	copied := decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
	}](t, uploaded).Artifact.ID
	awaitTextShows(t, handler, copied, "its open source",
		contains(`#ship state="open" copied_from="`+source+`"`), nil)

	reworded := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/asks/"+source, map[string]any{
		"question": "Ship on Tuesday?",
		"options":  []model.AskOption{{Label: "Tuesday", Description: "Ship it."}, {Label: "No", Description: "Later."}},
	}, "alice")
	if reworded.Code != http.StatusOK {
		t.Fatalf("reword the source: status=%d body=%s", reworded.Code, reworded.Body.String())
	}
	editedAt := decodeBody[model.Ask](t, reworded).EditedAt
	if answered := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+source+"/answer", map[string]any{
		"selected": []string{"Tuesday"}, "expected_edited_at": editedAt,
	}, "alice"); answered.Code != http.StatusOK {
		t.Fatalf("answer the reworded source: status=%d body=%s", answered.Code, answered.Body.String())
	}
	awaitTextShows(t, handler, copied, "its own ask",
		contains(`:::ask{#ship urgency="med" multiple="false"}`+"\nShip on Monday?", `#ship state="open"`+"\n"), contains(`state="answered"`))
	var onCopy []string
	for _, ask := range decodeBody[[]model.Ask](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/asks", nil, "alice")) {
		if ask.BlockArtifact != nil && ask.BlockArtifact.ID == copied {
			onCopy = append(onCopy, *ask.BlockID+" "+ask.State+" "+ask.Question)
		}
	}
	if len(onCopy) != 1 || onCopy[0] != "ship open Ship on Monday?" {
		t.Fatalf("the copy's asks = %v, want its own open ship asking the earlier wording", onCopy)
	}
	var pending int
	if err := database.Pool.QueryRow(context.Background(), `select count(*) from doc_settlements_pending where artifact_id = $1`, copied).Scan(&pending); err != nil {
		t.Fatalf("read the copy's pending settlement: %v", err)
	}
	if pending != 0 {
		t.Fatalf("the copy owes %d settlements, want none", pending)
	}
}

// A decided ask's wording is fixed on every write path, not only PATCH's (ASK_NOT_OPEN). An upload
// of the spec that rewords its answered ask leaves the ask as it was decided, and the spec's block
// gets back the wording the answer was given on. A copy then uploaded with the reworded text holds
// a question no decision answered and opens its own ask.
func TestAnUploadRewordingAnAnsweredAskLeavesItDecidedAndItsCopyOpensItsOwnAsk(t *testing.T) {
	t.Parallel()
	var documentService *docs.Service
	handler, _ := newInteractionHandler(t, func(database *store.Store) docs.API {
		documentService = docs.New(docs.Deps{Store: database, Settle: 20 * time.Millisecond})
		t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
		return documentService
	})
	monday := "Context\n\n:::ask{#ship urgency=\"med\" multiple=\"false\"}\nShip on Monday?\n\n- Yes: Monday.\n- No: Later.\n:::\n"
	tuesday := "Context\n\n:::ask{#ship urgency=\"med\" multiple=\"false\"}\nShip on Tuesday?\n\n- Tuesday: Ship it.\n- No: Later.\n:::\n"
	issue := createInteractionIssue(t, handler, "TEST", "Answered ask reworded by upload", monday)
	awaitIndexedAskBlock(t, handler, issue.PrimaryArtifactID, "ship", "Ship on Monday?")
	var source string
	for _, ask := range decodeBody[[]model.Ask](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/asks", nil, "alice")) {
		if ask.BlockID != nil && *ask.BlockID == "ship" {
			source = ask.ID
		}
	}
	if answered := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+source+"/answer", map[string]any{
		"selected": []string{"Yes"},
	}, "alice"); answered.Code != http.StatusOK {
		t.Fatalf("answer the ask: status=%d body=%s", answered.Code, answered.Body.String())
	}
	if uploaded := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]any{
		"actor": sessionActor(), "name": "spec.md", "content": tuesday,
		"precondition": uploadBase(t, handler, issue.PrimaryArtifactID),
	}); uploaded.Code != http.StatusCreated {
		t.Fatalf("upload the reworded spec: status=%d body=%s", uploaded.Code, uploaded.Body.String())
	}
	awaitDocumentText(t, handler, issue.PrimaryArtifactID, "the decided wording written back",
		contains("Ship on Monday?", "- Yes: Monday."), nil)
	awaitTextShows(t, handler, issue.PrimaryArtifactID, "the answer on the decided wording",
		contains(`#ship state="answered" answered_by="alice"`, "Ship on Monday?", "- Yes: Monday."), nil)
	ask := decodeBody[struct {
		Ask model.Ask `json:"ask"`
	}](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/asks/"+source, nil, "alice")).Ask
	if ask.State != "answered" || ask.Question != "Ship on Monday?" || ask.Options[0].Label != "Yes" || ask.EditedAt != nil {
		t.Fatalf("the answered ask reads %s %q %v edited %v after the upload, want it as it was answered", ask.State, ask.Question, ask.Options, ask.EditedAt)
	}

	copied := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]any{
		"actor": sessionActor(), "name": "spec-record.md", "content": tuesday,
	})
	if copied.Code != http.StatusCreated {
		t.Fatalf("upload the copy: status=%d body=%s", copied.Code, copied.Body.String())
	}
	record := decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
	}](t, copied).Artifact.ID
	awaitTextShows(t, handler, record, "its own ask",
		contains(`:::ask{#ship urgency="med" multiple="false"}`+"\nShip on Tuesday?", `#ship state="open"`+"\n"), contains(`state="answered"`))
}

// A free-text ask block (no bullet list) must put `"options": []` on the wire - in the ask.opened
// event and on the ask row - never JSON null: the SPA's Conversation tab reads options.length
// and a null there takes the page down.
func TestOptionlessAskBlockCarriesEmptyOptionsNotNull(t *testing.T) {
	t.Parallel()
	var documentService *docs.Service
	handler, _ := newInteractionHandler(t, func(database *store.Store) docs.API {
		documentService = docs.New(docs.Deps{Store: database, Settle: 20 * time.Millisecond})
		t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
		return documentService
	})
	issue := createInteractionIssue(t, handler, "TEST", "Free-text decision", "Context\n")
	edited := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"actor": sessionActor(),
		"ops": []map[string]string{{
			"after":    "end",
			"markdown": ":::ask{#free-ask urgency=\"med\" multiple=\"false\"}\nWhich name do we ship under?\n:::\n",
			"op":       "insert",
		}},
	})
	if edited.Code != http.StatusOK {
		t.Fatalf("write ask block: status=%d body=%s", edited.Code, edited.Body.String())
	}
	awaitIndexedAskBlock(t, handler, issue.PrimaryArtifactID, "free-ask", "Which name do we ship under?")
	if options := awaitAskBlockEvent(t, handler, issue.Key, "ask.opened", "free-ask").Options; string(options) != "[]" {
		t.Fatalf("ask.opened options = %s, want []", options)
	}
	asks := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/asks", nil, "alice")
	for _, ask := range decodeBody[[]model.Ask](t, asks) {
		if ask.BlockID != nil && *ask.BlockID == "free-ask" && ask.Options == nil {
			t.Fatalf("ask row options are nil; want an empty list")
		}
	}
}

type askBlockEventPayload struct {
	BlockID *string         `json:"block_id"`
	Options json.RawMessage `json:"options"`
}

// awaitAskBlockEvent polls the issue's events until one of eventType names the ask block
// blockID, failing the test when settlement never emits it. Its one bound is the test binary's
// deadline (testwait.UntilPolling), and its failure prints the event types it last read.
func awaitAskBlockEvent(t *testing.T, handler http.Handler, issueKey, eventType, blockID string) askBlockEventPayload {
	t.Helper()
	var found askBlockEventPayload
	testwait.UntilPolling(t, 20*time.Millisecond, fmt.Sprintf("a %s event for ask block %q (the event types the issue's last 50 events held)", eventType, blockID), func() ([]string, bool) {
		events := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issueKey+"/events?limit=50", nil, "alice")
		if events.Code != http.StatusOK {
			t.Fatalf("read events: status=%d body=%s", events.Code, events.Body.String())
		}
		var types []string
		for _, event := range decodeBody[[]struct {
			Type    string          `json:"type"`
			Payload json.RawMessage `json:"payload"`
		}](t, events) {
			types = append(types, event.Type)
			if event.Type != eventType {
				continue
			}
			var payload askBlockEventPayload
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				t.Fatalf("decode %s payload: %v", eventType, err)
			}
			if payload.BlockID != nil && *payload.BlockID == blockID {
				found = payload
				return types, true
			}
		}
		return types, false
	})
	return found
}

// Settlement runs after the edit's own version has consumed the room's pending authors; the ask
// it indexes must still be attributed to the session that wrote the block, or the asker can
// never edit its own ask and nobody can tell who is asking.
func TestBlockAskIndexedAfterEditKeepsTheWritingActor(t *testing.T) {
	t.Parallel()
	var documentService *docs.Service
	handler, _ := newInteractionHandler(t, func(database *store.Store) docs.API {
		documentService = docs.New(docs.Deps{Store: database, Settle: 20 * time.Millisecond})
		t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
		return documentService
	})
	issue := createInteractionIssue(t, handler, "TEST", "Attributed decision",
		"Context\n\n:::ask{#seeded urgency=\"med\" multiple=\"false\"}\nSeeded question?\n:::\n")
	awaitIndexedAskBlock(t, handler, issue.PrimaryArtifactID, "seeded", "Seeded question?")
	edited := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"actor": sessionActor(),
		"ops": []map[string]string{{
			"after":    "end",
			"markdown": ":::ask{#edited urgency=\"high\" multiple=\"false\"}\nEdited question?\n:::\n",
			"op":       "insert",
		}},
	})
	if edited.Code != http.StatusOK {
		t.Fatalf("write ask block: status=%d body=%s", edited.Code, edited.Body.String())
	}
	awaitIndexedAskBlock(t, handler, issue.PrimaryArtifactID, "edited", "Edited question?")
	asks := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/asks", nil, "alice")
	want := map[string]model.Actor{
		"seeded": {Kind: "user", ID: "alice"},
		"edited": {Kind: "session", ID: sessionActor()["id"].(string)},
	}
	seen := 0
	for _, ask := range decodeBody[[]model.Ask](t, asks) {
		if ask.BlockID == nil {
			continue
		}
		expected, ok := want[*ask.BlockID]
		if !ok {
			continue
		}
		seen++
		if ask.Author.Kind != expected.Kind || ask.Author.ID != expected.ID {
			t.Fatalf("block %s author = %#v, want %s %s", *ask.BlockID, ask.Author, expected.Kind, expected.ID)
		}
	}
	if seen != 2 {
		t.Fatalf("saw %d block asks, want 2", seen)
	}
}

// Changing a block ask's answer replaces the one its document's text answers: the asks member
// reads the new choice and none of the old words.
func TestChangingBlockAskReplacesTheAnswerItsTextAnswers(t *testing.T) {
	t.Parallel()
	handler, _ := blockAskHandler(t)
	issue, askID := seedBlockAsk(t, handler, "Change typed ask", "decision", transportAsk, "Which transport?")
	first := readBlockAsk(t, handler, askID)
	answered := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+askID+"/answer", map[string]any{
		"selected":           []string{"REST"},
		"text":               "REST first.",
		"expected_edited_at": first.EditedAt,
	}, "alice")
	if answered.Code != http.StatusOK {
		t.Fatalf("answer block ask: status=%d body=%s", answered.Code, answered.Body.String())
	}
	original := decodeBody[model.Ask](t, answered)
	if original.Answer == nil {
		t.Fatal("initial block answer is missing")
	}
	originalAt := timestampValue(original.Answer.At)

	changed := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+askID+"/answer", map[string]any{
		"selected":           []string{"gRPC"},
		"expected_answer_at": originalAt,
	}, "alice")
	if changed.Code != http.StatusOK {
		t.Fatalf("change block answer: status=%d body=%s", changed.Code, changed.Body.String())
	}
	updated := decodeBody[model.Ask](t, changed)
	if updated.Answer == nil || len(updated.Answer.Selected) != 1 || updated.Answer.Selected[0] != "gRPC" || updated.Answer.Text != nil || !updated.Answer.At.After(original.Answer.At) {
		t.Fatalf("changed block answer = %#v", updated)
	}
	if shown := agentText(t, handler, issue.PrimaryArtifactID); !strings.Contains(shown, `#decision state="answered" answered_by="alice" answered_at="`+timestampValue(updated.Answer.At)+`" selected=["gRPC"]`+"\n") ||
		strings.Contains(shown, `answer=`) {
		t.Fatalf("changed block text:\n%s\nwant gRPC selected and no answer", shown)
	}
}

// An ask written with its options and no question is refused where a caller writes whole
// markdown, however the author spelled the question: an empty paragraph, a link with no text, two
// such links, or two such paragraphs, which the renderer writes as nothing and which no reader can
// answer. Each refusal is INVALID_ASK_BLOCK naming the content rule and the line to write the
// question on, including the two-paragraph one, whose rendering the read-back would otherwise
// refuse as markdown. A question that writes bytes is a question, thin as it is: an image, a
// U+00A0, or words.
func TestUploadsRefuseAnAskWrittenWithOptionsAndNoQuestion(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "AQ", "Asks with no question", "Intro.\n")
	ask := func(body string) string {
		return "Intro.\n\n:::ask{#a1 urgency=\"med\" multiple=\"false\" state=\"open\"}\n" + body + "\n:::\n\nAfter.\n"
	}
	for index, test := range []struct {
		name, body string
		refused    bool
	}{
		{name: "an empty question", body: "- A\n- B", refused: true},
		{name: "a link with no text", body: "[](https://e.example)\n\n- A\n- B", refused: true},
		{name: "a link with no text or target", body: "[]()\n\n- A\n- B", refused: true},
		{name: "two links with no text", body: "[](https://e.example)[](https://f.example)\n\n- A\n- B", refused: true},
		{name: "two paragraphs of such links", body: "[](https://e.example)\n\n[](https://f.example)\n\n- A\n- B", refused: true},
		{name: "an image", body: "![a diagram](https://e.example/d.png)\n\n- A\n- B"},
		{name: "a U+00A0", body: "\u00a0\n\n- A\n- B"},
		{name: "words", body: "Which transport?\n\n- A\n- B"},
	} {
		content := ask(test.body)
		for route, send := range map[string]func() *httptest.ResponseRecorder{
			"create an issue": func() *httptest.ResponseRecorder {
				return dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
					"project": "AQ", "title": fmt.Sprintf("Ask %d: %s", index, test.name), "spec": content, "force": true,
				}, "alice")
			},
			"upload a document": func() *httptest.ResponseRecorder {
				return dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]any{
					"name": fmt.Sprintf("ask-%d.md", index), "content": content,
				}, "alice")
			},
		} {
			response := send()
			body := response.Body.String()
			if !test.refused {
				if response.Code != http.StatusCreated {
					t.Errorf("%s with %s as the question: status=%d body=%s, want it stored", route, test.name, response.Code, body)
				}
				continue
			}
			if response.Code != http.StatusBadRequest ||
				!strings.Contains(body, `"code":"INVALID_ASK_BLOCK"`) ||
				!strings.Contains(body, "options and no question") ||
				!strings.Contains(body, "paragraph+ bullet_list?") ||
				!strings.Contains(body, "write it on the line under the block's opening fence") {
				t.Errorf("%s with %s as the question: status=%d body=%s, want INVALID_ASK_BLOCK naming the missing question, the content rule and where to write it", route, test.name, response.Code, body)
			}
		}
	}
}

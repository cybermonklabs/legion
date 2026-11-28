package api

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/asks"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// An ask whose row names no node holds its answer on its row alone: a block ask on a document
// whose key is past the store's bound (graphstore.MaxAddressBytes) is created with no node marker
// (graphstore.AskRowNode), the boot step's backfill leaves such a row unmarked, and the answer
// route writes its answer to the row alone (asks.Settle). The text routes read such an ask's
// answer off its row, as every ask read does (asks.NodeAnswer.ApplyTo): the ask and a copy of it
// on another document of its issue each read answered, with the answer's person, time, selection
// and words, in the asks member a reader of asks is served (`Dispatch-Asks: 1`), and with the
// state, the person and the time on the block of the kept form every other reader is served.
func TestTheTextRoutesReadTheAnswerOfAnAskWhoseRowNamesNoNodeOffItsRow(t *testing.T) {
	t.Parallel()
	var documentService *docs.Service
	handler, database := newInteractionHandler(t, func(database *store.Store) docs.API {
		documentService = docs.New(docs.Deps{Store: database, Settle: 20 * time.Millisecond})
		t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
		return documentService
	})
	const content = "Context\n\n:::ask{#ship urgency=\"med\" multiple=\"false\"}\nShip on Monday?\n\n- Yes: Monday.\n- No: Later.\n:::\n"
	issue := createInteractionIssue(t, handler, "TEST", "An ask past the store's bound", "Context\n")
	upload := func(name string) model.Artifact {
		t.Helper()
		uploaded := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]any{
			"actor": sessionActor(), "name": name, "content": content,
		})
		if uploaded.Code != http.StatusCreated {
			t.Fatalf("upload the document %.40s: status=%d body=%.400s", name, uploaded.Code, uploaded.Body.String())
		}
		return decodeBody[struct {
			Artifact model.Artifact `json:"artifact"`
		}](t, uploaded).Artifact
	}
	// A hex name slugs to itself, so the key `<issue>/<slug>` is past the store's bound, and within
	// the one a new artifact's key is held to (maxNewArtifactKeyBytes).
	source := upload(randomBlockID(t, graphstore.MaxAddressBytes))
	if graphstore.AddressFits(source.RefKey) || len(source.RefKey) > maxNewArtifactKeyBytes {
		t.Fatalf("the source document's key is %d bytes, want past the store's bound %d and within %d", len(source.RefKey), graphstore.MaxAddressBytes, maxNewArtifactKeyBytes)
	}
	awaitIndexedAskBlock(t, handler, source.ID, "ship", "Ship on Monday?")
	var askID string
	for _, ask := range decodeBody[[]model.Ask](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/asks", nil, "alice")) {
		if ask.BlockID != nil && *ask.BlockID == "ship" {
			askID = ask.ID
		}
	}
	var named bool
	if err := database.Pool.QueryRow(context.Background(), `select node_kind is not null from asks where id = $1::uuid`, askID).Scan(&named); err != nil {
		t.Fatalf("read whether the row of ask %q names its node: %v", askID, err)
	}
	if named {
		t.Fatalf("the row of ask %s names its node, so the answer is held on the node and this proves nothing", askID)
	}
	copied := upload("copy.md")
	awaitTextShows(t, handler, copied.ID, "a copy of the open ask", contains(`#ship state="open" copied_from="`+askID+`"`), nil)

	if answered := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+askID+"/answer", map[string]any{
		"selected": []string{"Yes"}, "text": "Monday it is.",
	}, "alice"); answered.Code != http.StatusOK {
		t.Fatalf("answer the ask: status=%d body=%s", answered.Code, answered.Body.String())
	}
	awaitSettled(t, database, source.ID, copied.ID)
	answeredAt := asks.AnsweredAtValue(storedAskAnswer(t, database, askID).At)

	type servedText struct {
		Markdown string          `json:"markdown"`
		Asks     []model.TextAsk `json:"asks"`
	}
	for _, document := range []struct {
		name, id, copiedFrom, copiedFromDocument string
	}{
		{"the ask's document", source.ID, "", ""},
		{"the copy", copied.ID, askID, "dispatch://" + issue.Key + "/artifact/" + source.Slug},
	} {
		path := "/api/v1/artifacts/" + document.id + "/text"
		var forms [2]servedText
		for index, readsAsks := range []bool{true, false} {
			response := textRead(t, handler, path, readsAsks)
			if response.Code != http.StatusOK {
				t.Fatalf("GET the text of %s (Dispatch-Asks %t): status=%d body=%s", document.name, readsAsks, response.Code, response.Body.String())
			}
			forms[index] = decodeBody[servedText](t, response)
		}
		asked, kept := forms[0], forms[1]
		var entry model.TextAsk
		if len(asked.Asks) == 1 {
			entry = asked.Asks[0]
		}
		if len(asked.Asks) != 1 || entry.State != "answered" || entry.AnsweredBy != "alice" || entry.AnsweredAt != answeredAt ||
			entry.Selected == nil || !slices.Equal(*entry.Selected, []string{"Yes"}) || entry.Answer == nil || *entry.Answer != "Monday it is." ||
			entry.CopiedFrom != document.copiedFrom || entry.CopiedFromDocument != document.copiedFromDocument {
			t.Errorf("the text of %s with Dispatch-Asks: 1 answers asks %q, want ship answered by alice at %s, with her selection and words, as the ask's row holds them", document.name, askLines(asked.Asks), answeredAt)
		}
		if askLines(kept.Asks) != askLines(asked.Asks) {
			t.Errorf("the text of %s answers asks %q without Dispatch-Asks and %q with it, want the same asks", document.name, askLines(kept.Asks), askLines(asked.Asks))
		}
		var opener string
		for _, line := range strings.Split(kept.Markdown, "\n") {
			if strings.HasPrefix(line, ":::ask{#ship ") {
				opener = line
			}
		}
		_, attrs, opens := pmdoc.ParseTypedDirectiveOpen(opener)
		want := map[string]string{"state": "answered", "answered_by": "alice", "answered_at": answeredAt,
			"copied_from": document.copiedFrom, "copied_from_document": document.copiedFromDocument}
		for _, name := range []string{"state", "answered_by", "answered_at", "selected", "answer", "invalid", "copied_from", "copied_from_document"} {
			got, held := attrs[name]
			if !opens || held != (want[name] != "") || held && got != want[name] {
				t.Errorf("the kept form of %s opens ship with %q, which holds %s=%v (held %t), want %q: the state, the person and the time of the ask's row and no answer's words",
					document.name, opener, name, got, held, want[name])
			}
		}
	}
}

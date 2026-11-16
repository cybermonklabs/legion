package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The two answers the case reads back. Each holds markdown the tree says something about, so a
// tree answered for one of them cannot be the other's and cannot be the plain text either.
const (
	relatedTreesOwnAnswer   = "We drop the **rendered page** cache first, and keep the index cache."
	relatedTreesOtherAnswer = "Empty the *page store* first; the index store costs more to rebuild."
)

// relatedTreesRequest is the POST form of the related route, which runs both legs of the check:
// as the dashboard's related panel reads it when trees is true (`Dispatch-Trees: 1`), and as
// `dispatch related` reads it, with no header at all, when it is false.
func relatedTreesRequest(t *testing.T, handler http.Handler, project string, trees bool) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]any{"title": crossProjectQuestion})
	if err != nil {
		t.Fatalf("encode the related request: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/projects/"+project+"/related", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Dispatch-User", "alice")
	if trees {
		request.Header.Set(displayTreesHeader, "1")
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("POST the related route of %s with trees=%t: status=%d body=%s",
			project, trees, response.Code, response.Body.String())
	}
	return response
}

// answeredAskItem is where the item standing for the answered ask id sits among items, and that
// item. It fails unless the item is the ask itself carrying the answer a reader draws, since an
// item that carries no answer has no tree to answer either way and would pass this case for the
// wrong reason.
func answeredAskItem(t *testing.T, what string, items []model.WriteSuggestion, id string) (int, model.WriteSuggestion) {
	t.Helper()
	for index, item := range items {
		if item.ID != id {
			continue
		}
		if item.Kind != "ask" || item.Ask == nil || item.Ask.Answer == nil {
			t.Fatalf("%s: the item for the ask %s = %+v, want the ask with the answer it was given", what, id, item)
		}
		return index, item
	}
	t.Fatalf("%s: no item for the ask %s among %v", what, id, relatedItemIDs(items))
	return 0, model.WriteSuggestion{}
}

// relatedTreesFixture seeds the two projects the case checks across: one answered question of the
// asking project, which the one-project leg finds, and one of another project, which the
// cross-project leg finds. Each is the nearest thing its project holds to the question, and
// neither shares a word with it, so the check can reach them by meaning alone.
//
// It answers the handler, the ask of the asking project and the ask of the other one.
func relatedTreesFixture(t *testing.T, asking, other string) (http.Handler, string, string) {
	t.Helper()
	embedder := newCrossProjectEmbedder()
	for _, text := range crossProjectQueryTexts() {
		embedder.vectors[text] = queryVector()
	}
	var handler http.Handler
	var database *store.Store
	handler, database, _ = newTestServer(t, testServerOptions{embedder: embedder})

	createSuggestionProject(t, handler, asking)
	host := createSearchIssue(t, handler, asking, "Our own settled questions", "Questions this project settled itself.")
	own := seedAnsweredQuestion(t, handler, database, embedder, host,
		"Which cache does this project drop first when the backlog grows?", relatedTreesOwnAnswer, 0.95, 10)

	createSuggestionProject(t, handler, other)
	owner := createSearchIssue(t, handler, other, "Backlog runbook", "What to do when the backlog grows.")
	offered := seedAnsweredQuestion(t, handler, database, embedder, owner,
		"When the backlog grows, which store do we empty first?", relatedTreesOtherAnswer, 0.9, 2)

	refreshTermStats(t, database, asking)
	processAllPending(t, database, embedder)
	return handler, own, offered
}

// A stored ask answer the related route offers carries its display tree exactly when the request
// asked for the trees, on both legs of the check: the one-project items, whose answers the ask
// states bring (attachAskStates), and the answered questions of the other projects, whose
// answers the cross-project statement brings (relatedOtherProjects). Each leg reads its answer
// from a different place, and before INFRA-635 each passed no reader at all, so the panel that
// draws a check's result parsed the markdown in the browser - which R6 leaves to the editor, to
// a paste and to text Dispatch does not store.
//
// The same request without the header answers the same two items with no tree on either, which
// is what every client but the dashboard reads.
func TestTheRelatedRouteCarriesTheTreeOfEveryAnswerItOffersOnBothLegs(t *testing.T) {
	handler, own, offered := relatedTreesFixture(t, "XTRE", "XTRO")

	response := relatedTreesRequest(t, handler, "XTRE", true)
	suggestions := decodeBody[*model.Suggestions](t, response)
	if suggestions.Search != relatedMeaningAndKeywords {
		t.Fatalf("the check = %+v, want it searched by meaning and keywords, which is what runs both legs", suggestions)
	}
	if ids := crossProjectItemIDs(suggestions.OtherProjects); !slices.Equal(ids, []string{offered}) {
		t.Fatalf("the check offered %v of the other project, want the answered question %s", ids, offered)
	}
	at, item := answeredAskItem(t, "the check with the trees asked for", suggestions.Related, own)
	if text := deref(item.Ask.Answer.Text); text != relatedTreesOwnAnswer {
		t.Fatalf("the one-project item's answer = %q, want %q", text, relatedTreesOwnAnswer)
	}
	cross := suggestions.OtherProjects[0]
	if text := deref(cross.Ask.Answer.Text); text != relatedTreesOtherAnswer {
		t.Fatalf("the cross-project item's answer = %q, want %q", text, relatedTreesOtherAnswer)
	}
	// Each tree is the one ReadForDisplay makes of exactly that answer's text.
	requireAnsweredTree(t, response, "related."+strconv.Itoa(at)+".ask.answer.text_tree", relatedTreesOwnAnswer)
	requireAnsweredTree(t, response, "other_projects.0.ask.answer.text_tree", relatedTreesOtherAnswer)

	plain := decodeBody[*model.Suggestions](t, relatedTreesRequest(t, handler, "XTRE", false))
	if ids := crossProjectItemIDs(plain.OtherProjects); !slices.Equal(ids, []string{offered}) {
		t.Fatalf("the check with no header offered %v of the other project, want %s still", ids, offered)
	}
	_, bare := answeredAskItem(t, "the check with no header", plain.Related, own)
	if tree := bare.Ask.Answer.TextTree; len(tree) != 0 {
		t.Errorf("the one-project item carries the tree %s with no header sent, want none", tree)
	}
	if tree := plain.OtherProjects[0].Ask.Answer.TextTree; len(tree) != 0 {
		t.Errorf("the cross-project item carries the tree %s with no header sent, want none", tree)
	}
}

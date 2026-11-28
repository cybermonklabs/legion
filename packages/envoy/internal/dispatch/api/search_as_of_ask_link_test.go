package api

import (
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// TestAnAskHitAtACutLinksItsBlockAsTheGraphStoodThen: the href of an ask hit names the ask's
// typed block only while that block is a node of the reference graph, which is the graph now.
// At a cut, an ask whose block's document did not change after the cut reads the graph as it
// stood then, so its href is the one the search at the cut gave; an ask whose block's document
// changed after the cut has a link no record at the cut gives, so the answer leaves its href out
// and names it (reason G), and never gives the href the present graph makes. Here the block is
// deleted after the cut: at the cut the hit linked the block, and the present graph has no node
// for it.
func TestAnAskHitAtACutLinksItsBlockAsTheGraphStoodThen(t *testing.T) {
	w := newSearchAtCutWorld(t, "ASAL", "Ask block links", "Context paragraph.")
	spec := decodeBody[model.Issue](t, dispatchRequest(t, w.handler, http.MethodGet, "/api/v1/issues/"+w.issue, nil, "alice")).PrimaryArtifactID
	if written := sessionRequest(t, w.handler, http.MethodPost, "/api/v1/artifacts/"+spec+"/edits", map[string]any{
		"actor": sessionActor(),
		"ops": []map[string]string{{"op": "insert", "after": "end",
			"markdown": ":::ask{#kept urgency=\"med\" multiple=\"false\"}\nShip the astrolabe?\n:::\n"}},
	}); written.Code != http.StatusOK {
		t.Fatalf("write the ask block: status=%d body=%.400s", written.Code, written.Body.String())
	}
	awaitIndexedAskBlock(t, w.handler, spec, "kept", "Ship the astrolabe?")
	kept := blockAskID(t, w.handler, w.issue, "kept")
	blockHref := "/issues/" + w.issue + "/spec#b-" + text.EncodeBlockID("kept")
	cut := w.cut(t)
	thenHit := (searchAnswer{SearchResponse: w.searchNow(t, keywords("astrolabe"))}).hit("ask", kept)
	if thenHit == nil || thenHit.Href != blockHref {
		t.Fatalf("the search read at the cut gives the ask %+v, want it linked to its block %s", thenHit, blockHref)
	}
	time.Sleep(5 * time.Millisecond)

	// An open ask's block is not deleted (R5 of INFRA-1520): the question is closed first.
	if resolved := sessionRequest(t, w.handler, http.MethodPost, "/api/v1/asks/"+kept+"/resolve", map[string]any{
		"kind": "retracted", "reason": "asked elsewhere", "actor": sessionActor(),
	}); resolved.Code != http.StatusOK {
		t.Fatalf("retract the ask: status=%d body=%s", resolved.Code, resolved.Body.String())
	}
	if deleted := dispatchRequest(t, w.handler, http.MethodPost, "/api/v1/artifacts/"+spec+"/edits", map[string]any{
		"ops": []map[string]string{{"op": "delete", "block": "kept"}},
	}, "alice"); deleted.Code != http.StatusOK {
		t.Fatalf("delete the ask's block: status=%d body=%s", deleted.Code, deleted.Body.String())
	}
	settleDocument(t, w.handler, spec, w.issue, "after-delete")
	if now := (searchAnswer{SearchResponse: w.searchNow(t, keywords("astrolabe"))}).hit("ask", kept); now == nil || now.Href == blockHref {
		t.Fatalf("the present search gives the ask %+v, want it linked to the ask, its block no node now", now)
	}
	w.fill(t, cut)

	at := w.searchAt(t, keywords("astrolabe"), cut)
	index := slices.IndexFunc(at.Results, func(result model.SearchResult) bool { return result.Kind == "ask" && result.ID == kept })
	if index < 0 {
		t.Fatalf("the search at the cut misses the ask: %+v", at.Results)
	}
	results, _ := at.raw["results"].([]any)
	href, given := results[index].(map[string]any)["href"]
	switch {
	case given && href != blockHref:
		t.Errorf("the ask's href at the cut is %v, want %s, the link the search at the cut gave, or no href", href, blockHref)
	case !given && !slices.Contains(at.absent, "/results/*/href"):
		t.Errorf("the ask's href at the cut is left out, and the answer names %v absent, not /results/*/href", at.absent)
	}
}

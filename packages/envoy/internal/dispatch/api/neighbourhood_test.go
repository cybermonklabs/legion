package api

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

func TestTheNeighbourhoodReadEndsInsideTheRelatedAllowance(t *testing.T) {
	handler, _, _, served := newRegisteredTestServer(t, testServerOptions{testHooks: true, settle: time.Hour})
	createTestProject(t, handler, "CORE")
	const title = "Zymurgy ledger totals drift"
	first := createdIssue(t, "seed related text", createIssueUnder(t, handler, "CORE", title, "", nil))

	const heldKey = "CORE-2"
	// Each check looks up the new issue over an independent request before it waits or reads
	// on its own connection. The test doesn't infer commit order from the create's response.
	for _, test := range []struct {
		path, duration string
		hold           *relatedReadHold
	}{
		{"neighbourhood-read", "15s", &served.relatedReadHolds.neighbourhood},
		{"related-search", "4s", &served.relatedReadHolds.search},
	} {
		test.hold.beforeWait = func(context.Context) {
			response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+heldKey, nil, "alice")
			if response.Code != http.StatusOK {
				t.Errorf("at the start of %s the issue is not readable: HTTP %d: %s", test.path, response.Code, response.Body.String())
			}
		}
		response := dispatchRequest(t, handler, http.MethodPut, "/api/v1/_test/holds/"+test.path,
			map[string]string{"duration": test.duration}, "alice")
		if response.Code != http.StatusNoContent {
			t.Fatalf("arm %s: HTTP %d: %s", test.path, response.Code, response.Body.String())
		}
	}
	started := time.Now()
	response := createIssueUnder(t, handler, "CORE", title, "", nil)
	if elapsed := time.Since(started); elapsed > relatedCheckAllowance+2*time.Second {
		t.Errorf("create answered after %v, want at most %s plus 2s for the request outside the reads", elapsed, relatedCheckAllowance)
	}
	issue := createdIssue(t, "create with both holds armed", response)
	if issue.Key != heldKey {
		t.Fatalf("created %s, expected the reads to see %s", issue.Key, heldKey)
	}
	answer := decodeBody[struct {
		Advice struct {
			Suggestions model.Suggestions `json:"suggestions"`
		} `json:"advice"`
	}](t, response).Advice.Suggestions
	if answer.Outcome != relatedFound || !slices.ContainsFunc(answer.Related, func(item model.WriteSuggestion) bool {
		return item.Owner.Key == first.Key
	}) {
		t.Errorf("held related search: %+v, want a match naming %s", answer, first.Key)
	}
	if answer.Neighbourhood == nil || answer.Neighbourhood.Outcome != relatedNotMade ||
		answer.Neighbourhood.Missing != "the graph read did not answer within 5s" || len(answer.Neighbours) != 0 {
		t.Errorf("held graph read: outcome %+v, neighbours %+v; want not_made for time and no neighbours", answer.Neighbourhood, answer.Neighbours)
	}
	if served.relatedReadHolds.neighbourhood.duration.Load() != 0 || served.relatedReadHolds.search.duration.Load() != 0 {
		t.Error("one of the armed reads did not start")
	}
}

func TestAnIssuesOutboundRelationsAnswerEachOnce(t *testing.T) {
	handler, _ := newComponentTreeServer(t)
	create := func(title string) string {
		return createComponentIssue(t, handler, title, nil, nil).Key
	}
	parent, blocker, duplicate, origin, mentioned := create("Pelican"), create("Xylophone"), create("Quartz"), create("Lichen"), create("Narwhal")
	ask := "dispatch://" + origin + "/ask/" + createIssueAsk(t, handler, origin, "Which flute?")
	issue := createdIssue(t, "write all five relations", createIssueUnder(t, handler, "CORE", "Dahlia", parent, map[string]any{
		"needs": []string{blocker}, "duplicate_of": duplicate, "filed_from": ask,
		"components": map[string]any{"mode": "explicit", "ids": []string{"daemon"}},
		"spec":       "dispatch://" + mentioned,
	}))
	graph := graphEdges(t, handler, url.Values{"from": {"dispatch://" + issue.Key}})
	var got []string
	for _, edge := range graph.Edges {
		got = append(got, edge.Kind+" "+edge.Node.Ref)
	}
	slices.Sort(got)
	want := []string{
		"affects dispatch://CORE/component/daemon",
		"child_of dispatch://" + parent,
		"duplicate_of dispatch://" + duplicate,
		"filed_from " + ask,
		"needs dispatch://" + blocker,
	}
	if !slices.Equal(got, want) {
		t.Errorf("outbound edges of %s: %q, want %q", issue.Key, got, want)
	}
}

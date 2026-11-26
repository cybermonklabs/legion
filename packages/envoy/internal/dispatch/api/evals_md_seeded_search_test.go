package api

import (
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/evals"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// TestANewProjectsEvalsDocumentIsFoundByItsOwnWords: a project the route creates holds its seeded
// evals-md (dispatch://INFRA-2916, R5), and a person meets that document wherever the project's
// words are searched. A search of the project for two words its introduction holds finds it, a
// document of the project linked at its own page; the related check of a title made of those
// words lists it, found; and the near-copy gate, which weighs the project's open issues alone,
// lets an issue that restates the introduction in, then refuses a second one restating it again
// as a copy of the first, never of the document.
func TestANewProjectsEvalsDocumentIsFoundByItsOwnWords(t *testing.T) {
	t.Parallel()
	handler, database, _ := newTestServer(t, testServerOptions{duplicateBound: nearCopyVerdictBound(t), relatedBound: meaningVerdictBound(t)})
	createSuggestionProject(t, handler, "SEEDED")
	const title = "How an eval is registered and judged"
	const spec = "Dispatch adds a section when an eval is registered, and a register write sets how the number is judged."

	search := searchResponse(t, handler, url.Values{"q": {"registered judged"}, "project": {"SEEDED"}}.Encode())
	found := slices.IndexFunc(search.Results, func(result model.SearchResult) bool {
		return result.Kind == "document" && result.Owner.Kind == "document" && result.Owner.Slug == evals.DocumentSlug
	})
	if found < 0 {
		t.Fatalf("a search of SEEDED for words its %s holds answered %+v, want that document", evals.DocumentSlug, search.Results)
	}
	if want := "/projects/SEEDED/documents/" + evals.DocumentSlug + "?q="; !strings.HasPrefix(search.Results[found].Href, want) {
		t.Errorf("the %s hit links to %q, want its own page %s…", evals.DocumentSlug, search.Results[found].Href, want)
	}

	refreshTermStats(t, database, "SEEDED")
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/projects/SEEDED/related?"+url.Values{"title": {title}}.Encode(), nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("GET related: status=%d body=%s", response.Code, response.Body.String())
	}
	related := decodeBody[model.Suggestions](t, response)
	listed := slices.ContainsFunc(related.Related, func(item model.WriteSuggestion) bool {
		return item.Owner.Kind == "document" && item.Owner.Slug == evals.DocumentSlug
	})
	if related.Outcome != relatedFound || !listed {
		t.Fatalf("the related check of %q answered %+v, want %s listed, found", title, related, evals.DocumentSlug)
	}

	first := requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{"project": "SEEDED", "title": title, "spec": spec}))
	refreshTermStats(t, database, "SEEDED")
	refusal := requireDuplicateRefusal(t, createIssueRequest(t, handler, map[string]any{"project": "SEEDED", "title": title, "spec": spec}))
	if len(refusal.Candidates) != 1 || refusal.Candidates[0].Key != first.Key {
		t.Fatalf("the second restatement was refused naming %+v, want %s alone, the issue it copies", refusal.Candidates, first.Key)
	}
	t.Logf("SEEDED's %s: found by search at %s, listed by the related check; %s went in past the gate and its copy was refused naming it",
		evals.DocumentSlug, search.Results[found].Href, first.Key)
}

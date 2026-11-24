package delivery

import (
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/githubtest"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// searchStandIn is a GitHub App stand-in for the merged-PR search: its installation token is
// token, and its GraphQL search answers page of the cursor each request sent back as
// variables.after (nil for none). asked is every such cursor, in the order the requests came.
func searchStandIn(t *testing.T, token string, page func(after any) any) (client *githubapp.Client, asked func() []any) {
	t.Helper()
	var mu sync.Mutex
	var afters []any
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/installation", func(w http.ResponseWriter, r *http.Request) {
		mustEncode(t, w, map[string]any{
			"id": 1, "app_slug": "delivery-test",
			"permissions": map[string]string{"contents": "read", "pull_requests": "read"},
		})
	})
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		mustEncode(t, w, map[string]any{"token": token, "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339)})
	})
	mux.HandleFunc("POST /graphql", func(w http.ResponseWriter, r *http.Request) {
		_, variables := decodeGraphQLRequest(t, r)
		mu.Lock()
		afters = append(afters, variables["after"])
		mu.Unlock()
		mustEncode(t, w, page(variables["after"]))
	})
	return newAppClient(t, githubtest.Spec{Handle: mux.ServeHTTP}), func() []any {
		mu.Lock()
		defer mu.Unlock()
		return append([]any(nil), afters...)
	}
}

// searchDay is the one-day window the search rows of this file search.
var searchDay = [2]time.Time{time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)}

// TestASearchCursorTheRuleChangesEndsTheSearchUnsent: GitHub, or whatever answers in its place,
// answers the merged-PR search's first page with an endCursor that quotes a credential's URL
// (hostile), or that echoes the installation token the request carried (echoes-token). The
// search sends neither back: it ends with an error that names the cursor only as the rule reads
// it and names the field it would not send, and it asks GitHub for no page after the first,
// neither the page after the cursor nor, with no cursor, the first page again. Captures
// delivery.search-cursor/<case>.
func TestASearchCursorTheRuleChangesEndsTheSearchUnsent(t *testing.T) {
	token := "ghs_" + redacttest.Token + "_installation"
	for _, c := range []struct{ name, cursor string }{
		{"hostile", redacttest.Words},
		{"echoes-token", token},
	} {
		t.Run(c.name, func(t *testing.T) {
			client, asked := searchStandIn(t, token, func(after any) any {
				if after == nil {
					return searchResponseJSON(150, repeatSearchNodes(100, 1), true, c.cursor)
				}
				// The page after the cursor is refused in words that name nothing of it.
				return map[string]any{"errors": []any{map[string]any{"message": "no page follows that cursor"}}}
			})
			_, err := SearchMergedPullRequests(t.Context(), client, "acme", "widgets", []string{"alice"}, searchDay[0], searchDay[1])
			if err == nil {
				t.Fatal("the search answered no error for a cursor it cannot send back")
			}
			redacttest.RequireNone(t, "delivery.search-cursor/"+c.name, err.Error())
			if got := asked(); len(got) != 1 || got[0] != nil {
				t.Errorf("the search sent the cursors %v; want the first page's request alone, with no cursor", got)
			}
			if !strings.Contains(err.Error(), "endCursor") {
				t.Errorf("the search's error is %q; want it to name the endCursor it did not send back", err)
			}
		})
	}
}

// TestTheMergedPullRequestSearchReadsEachPageAfterGitHubsCursor: every cursor GitHub's GraphQL
// search answers is base64 of "cursor:…", so each begins Y3Vyc29y, and the second page's cursor
// shares that run of eight with the first page's, which the second page's request carried back
// as variables.after. GitHub's pagination contract makes that value its own position in the
// results, public, so the second page's cursor is sent back as it came, and the search reads the
// third page and every result on it.
func TestTheMergedPullRequestSearchReadsEachPageAfterGitHubsCursor(t *testing.T) {
	const second, third = "Y3Vyc29yOjEwMA==", "Y3Vyc29yOjIwMA=="
	client, asked := searchStandIn(t, "ghs_installation_token_for_the_search", func(after any) any {
		switch after {
		case nil:
			return searchResponseJSON(250, repeatSearchNodes(100, 1), true, second)
		case second:
			return searchResponseJSON(250, repeatSearchNodes(100, 101), true, third)
		case third:
			return searchResponseJSON(250, repeatSearchNodes(50, 201), false, "")
		}
		return map[string]any{"errors": []any{map[string]any{"message": "no page follows that cursor"}}}
	})
	results, err := SearchMergedPullRequests(t.Context(), client, "acme", "widgets", []string{"alice"}, searchDay[0], searchDay[1])
	if err != nil {
		t.Fatalf("the search over three pages: %v", err)
	}
	if got := asked(); len(got) != 3 || got[0] != nil || got[1] != second || got[2] != third {
		t.Errorf("the search sent the cursors %v; want none, %s and %s", got, second, third)
	}
	if len(results) != 250 || results[249].Number != 250 {
		t.Errorf("the search found %d pull requests; want the 250 of three pages, the last numbered 250", len(results))
	}
}

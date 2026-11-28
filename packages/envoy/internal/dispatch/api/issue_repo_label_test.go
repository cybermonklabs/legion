package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// An import through the default project labelled its issue repo:<owner>/<repo>, Dispatch's record
// of the repository the issue came from, beside its GitHub issue link, until #4457 ended those
// imports. The issue an earlier import made keeps that label, and it is no label of the caller's
// (R14): an issue stored as such an import left it, with its own repo: label of 43 characters and
// 20 other labels, takes a labels replacement that sends its repo: label back unchanged with
// another label in place of one of the 20, and refuses one that also sends a new label of 41
// characters 400 LABELS_INPUT; the issue lists answer it for ?label= its repo: label, and refuse
// a repo: filter of 146 characters and a filter of 41 characters that names no repository 400.
// Each version is a subtest of its own.
func TestAnIssuesOwnLongRepoLabelSurvivesALabelsReplacement(t *testing.T) {
	repo := "o/" + strings.Repeat("r", 36)
	own := repoLabelPrefix + repo
	others := make([]string, 0, 20)
	for index := 1; index <= 20; index++ {
		others = append(others, fmt.Sprintf("label-%02d", index))
	}
	tooLong := strings.Repeat("x", 41)
	// longestRepoFilter is a repo: filter of 146 characters: an owner of 39 and a name of 101,
	// one past GitHub's 100.
	longestRepoFilter := repoLabelPrefix + strings.Repeat("o", 39) + "/" + strings.Repeat("r", 101)

	for _, version := range []string{"v1", "v2"} {
		t.Run(version, func(t *testing.T) {
			handler, database := newTestHandlerWithStore(t)
			if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
				"key": "DEFAULT", "name": "Default project",
			}, "alice"); response.Code != http.StatusCreated {
				t.Fatalf("create the default project: status=%d body=%s", response.Code, response.Body.String())
			}
			// The issue as an import through the default project left it before #4457: the
			// caller's 20 labels, then its own repo: label, and the link to the GitHub issue.
			created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
				"project": "DEFAULT", "title": "An issue an earlier import made", "force": true,
			}, "alice")
			if created.Code != http.StatusCreated {
				t.Fatalf("create the issue: status=%d body=%s", created.Code, created.Body.String())
			}
			key := decodeBody[model.Issue](t, created).Key
			if _, err := database.Pool.Exec(t.Context(), `update issues set labels = $2 where key = $1`,
				key, append(slices.Clone(others), own)); err != nil {
				t.Fatalf("store %s's labels: %v", key, err)
			}
			if _, err := database.Pool.Exec(t.Context(), `
				insert into issue_external_links (issue_key, url, kind) values ($1, $2, 'github_issue')
			`, key, "https://github.com/"+repo+"/issues/1"); err != nil {
				t.Fatalf("store %s's link: %v", key, err)
			}

			patch := func(labels []string) *httptest.ResponseRecorder {
				t.Helper()
				return dispatchRequest(t, handler, http.MethodPatch, "/api/"+version+"/issues/"+key, map[string]any{"labels": labels}, "alice")
			}
			list := "/api/" + version + "/issues"
			listed := func(label string) *httptest.ResponseRecorder {
				t.Helper()
				return dispatchRequest(t, handler, http.MethodGet, list+"?label="+label, nil, "alice")
			}

			// (1) The repo: label sent back unchanged, with another label in place of one of the 20.
			replaced := append(append([]string{}, others[1:]...), "typed", own)
			accepted := patch(replaced)
			if accepted.Code != http.StatusOK {
				t.Fatalf("PATCH %s sending the repo: label back with another label in place of one of the 20: status=%d body=%s, want 200",
					list+"/"+key, accepted.Code, accepted.Body.String())
			}
			if labels := decodeBody[model.Issue](t, accepted).Labels; !slices.Equal(labels, replaced) {
				t.Fatalf("the accepted replacement left labels %q, want %q", labels, replaced)
			}

			// (2) The same replacement with a new label of 41 characters as well.
			refused := patch(append(append([]string{}, replaced...), tooLong))
			if code, _ := refusalOf(t, refused); refused.Code != http.StatusBadRequest || code != "LABELS_INPUT" {
				t.Fatalf("PATCH %s that also sends a new label of 41 characters: status=%d body=%s, want 400 LABELS_INPUT",
					list+"/"+key, refused.Code, refused.Body.String())
			}

			// (3) The issue list answers the issue for ?label= its repo: label.
			found := listed(own)
			if found.Code != http.StatusOK {
				t.Fatalf("GET %s?label=%s: status=%d body=%s, want 200", list, own, found.Code, found.Body.String())
			}
			var summaries []model.IssueSummary
			if version == "v1" {
				summaries = decodeBody[[]model.IssueSummary](t, found)
			} else {
				summaries = decodeBody[page[model.IssueSummary]](t, found).Items
			}
			if len(summaries) != 1 || summaries[0].Key != key {
				t.Fatalf("GET %s?label=%s answered %s, want %s alone", list, own, found.Body.String(), key)
			}

			// (4) A repo: filter of 146 characters, and (5) a filter of 41 characters that does not
			// start with repo:, are each 400.
			for _, filter := range []struct {
				what, value string
				length      int
			}{
				{"a repo: filter of 146 characters", longestRepoFilter, 146},
				{"a filter of 41 characters that names no repository", tooLong, 41},
			} {
				if len(filter.value) != filter.length {
					t.Fatalf("%s is %d characters, want %d", filter.what, len(filter.value), filter.length)
				}
				if response := listed(filter.value); response.Code != http.StatusBadRequest {
					t.Fatalf("GET %s with %s: status=%d body=%s, want 400", list, filter.what, response.Code, response.Body.String())
				}
			}
		})
	}
}

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

// An import through the default project labels its issue repo:<owner>/<repo>, Dispatch's record of
// the repository the issue came from, not a label of the caller's (R14). A repository name of any
// length a GitHub repository's can have is imported, and one past it refused 400 LABELS_INPUT, the
// caller's own labels keep their limits, the issue's labels stay editable with that label among
// them, and the label stays a filter. Every route runs on both versions, each import a first
// import: a second import of one reference answers the issue it made before any label is read.
func TestADefaultProjectImportTakesALongRepositoryName(t *testing.T) {
	// fitting's label is 40 characters, a caller's limit; long's is 43.
	fitting := "o/" + strings.Repeat("r", 33)
	long := "o/" + strings.Repeat("r", 36)
	tooLong := strings.Repeat("x", 41)
	numbered := func(count int) []string {
		labels := make([]string, 0, count)
		for index := 1; index <= count; index++ {
			labels = append(labels, fmt.Sprintf("label-%02d", index))
		}
		return labels
	}
	defaultProject := func(t *testing.T) http.Handler {
		t.Helper()
		handler, _ := newTestHandlerWithDefaultProject(t)
		if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
			"key": "DEFAULT", "name": "Default project",
		}, "alice"); response.Code != http.StatusCreated {
			t.Fatalf("create the default project: status=%d body=%s", response.Code, response.Body.String())
		}
		return handler
	}
	importIssue := func(t *testing.T, handler http.Handler, version, ref string, labels []string) *httptest.ResponseRecorder {
		t.Helper()
		body := map[string]any{"external": ref}
		if labels != nil {
			body["labels"] = labels
		}
		return dispatchRequest(t, handler, http.MethodPost, "/api/"+version+"/issues", body, "alice")
	}
	expectLabels := func(t *testing.T, what string, got []string, want ...string) {
		t.Helper()
		if !slices.Equal(got, want) {
			t.Fatalf("%s: labels = %q, want %q", what, got, want)
		}
	}
	// expectImported holds an import's answer to a new issue that records the repository it came
	// from: its repo: label, after the caller's labels, and its GitHub issue link.
	expectImported := func(t *testing.T, response *httptest.ResponseRecorder, repo string, number int, callerLabels ...string) model.Issue {
		t.Helper()
		if response.Code != http.StatusCreated {
			t.Fatalf("import of %s#%d answered %d %s, want 201", repo, number, response.Code, response.Body.String())
		}
		issue := decodeBody[model.Issue](t, response)
		expectLabels(t, "the import of "+repo, issue.Labels, append(callerLabels, repoLabelPrefix+repo)...)
		want := []model.ExternalLink{{URL: fmt.Sprintf("https://github.com/%s/issues/%d", repo, number), Kind: "github_issue"}}
		if !slices.Equal(issue.ExternalLinks, want) {
			t.Fatalf("the import of %s#%d links %+v, want %+v", repo, number, issue.ExternalLinks, want)
		}
		return issue
	}
	expectRefused := func(t *testing.T, what string, response *httptest.ResponseRecorder) {
		t.Helper()
		if code, _ := refusalOf(t, response); response.Code != http.StatusBadRequest || code != "LABELS_INPUT" {
			t.Fatalf("%s answered %d %s, want 400 LABELS_INPUT", what, response.Code, response.Body.String())
		}
	}

	for _, version := range []string{"v1", "v2"} {
		t.Run(version, func(t *testing.T) {
			handler := defaultProject(t)
			expectImported(t, importIssue(t, handler, version, fitting+"#1", nil), fitting, 1)
			issue := expectImported(t, importIssue(t, handler, version, long+"#1", nil), long, 1)

			// The caller's labels keep their limits, and the repo: label counts toward neither.
			full := expectImported(t, importIssue(t, handler, version, long+"#2", numbered(20)), long, 2, numbered(20)...)
			expectRefused(t, "an import with 21 caller labels", importIssue(t, handler, version, long+"#3", numbered(21)))
			expectRefused(t, "an import with a caller label of 41 characters",
				importIssue(t, handler, version, long+"#4", []string{"triage", tooLong}))

			// A name no GitHub repository has, whose repo: label runs past the 145 characters of an
			// owner of 39 and a name of 100 (labelFilterLimit), is refused as a label past its limit
			// always was, naming the repository and the bound, never stored as a label no filter
			// takes. The name of 2.7 KB is sent with a title of its own: the reference itself would be
			// past a title's limit, which answers first, as it always has.
			for _, name := range []struct{ what, repo, title string }{
				{"a repository name of 200 characters", "o/" + strings.Repeat("r", 198), ""},
				{"a repository name of 2.7 KB", "o/" + strings.Repeat("r", 2700), "An issue of a long repository"},
			} {
				body := map[string]any{"external": name.repo + "#1"}
				if name.title != "" {
					body["title"] = name.title
				}
				response := dispatchRequest(t, handler, http.MethodPost, "/api/"+version+"/issues", body, "alice")
				expectRefused(t, "an import of "+name.what, response)
				if _, text := refusalOf(t, response); !strings.Contains(text, name.repo) || !strings.Contains(text, fmt.Sprint(maxRepoLabel16)) {
					t.Fatalf("the import of %s was refused %q, want the repository and the bound of %d named", name.what, text, maxRepoLabel16)
				}
			}

			t.Run("a .git reference, a first import of its own", func(t *testing.T) {
				expectImported(t, importIssue(t, defaultProject(t), version, long+".git#1", nil), long, 1)
			})

			patch := func(t *testing.T, key string, body map[string]any) *httptest.ResponseRecorder {
				t.Helper()
				return dispatchRequest(t, handler, http.MethodPatch, "/api/"+version+"/issues/"+key, body, "alice")
			}
			expectPatched := func(t *testing.T, what string, response *httptest.ResponseRecorder, want ...string) {
				t.Helper()
				if response.Code != http.StatusOK {
					t.Fatalf("%s answered %d %s, want 200", what, response.Code, response.Body.String())
				}
				expectLabels(t, what, decodeBody[model.Issue](t, response).Labels, want...)
			}
			own := repoLabelPrefix + long
			t.Run("the issue's labels change with its repo: label among them", func(t *testing.T) {
				expectPatched(t, "a replacement that sends the repo: label back with another label",
					patch(t, issue.Key, map[string]any{"labels": []string{own, "triage"}}), own, "triage")
				expectRefused(t, "a replacement that also sends a new label of 41 characters",
					patch(t, issue.Key, map[string]any{"labels": []string{own, "triage", tooLong}}))
				// Only the label the import made is free of the limits: another repository's is a
				// label like any other.
				expectRefused(t, "a replacement that sends another repository's long repo: label",
					patch(t, issue.Key, map[string]any{"labels": []string{own, repoLabelPrefix + "x/" + strings.Repeat("r", 36)}}))
				expectPatched(t, "a labels_add", patch(t, issue.Key, map[string]any{"labels_add": []string{"later"}}),
					own, "triage", "later")
				expectRefused(t, "a labels_add of a label of 41 characters",
					patch(t, issue.Key, map[string]any{"labels_add": []string{tooLong}}))
				// The label is the issue's own whatever case the caller spells it in.
				if response := patch(t, issue.Key, map[string]any{"labels": []string{strings.ToUpper(own), "triage"}}); response.Code != http.StatusOK {
					t.Fatalf("a replacement that sends the repo: label back in capitals answered %d %s, want 200",
						response.Code, response.Body.String())
				}
			})
			t.Run("twenty labels beside the repo: label, and no more", func(t *testing.T) {
				// The step a person takes in the label editor: one of the twenty dropped and a new one
				// typed, the repo: label sent back as it is.
				edited := append(append([]string{}, full.Labels[1:]...), "typed")
				expectPatched(t, "a replacement of one of twenty labels", patch(t, full.Key, map[string]any{"labels": edited}), edited...)
				expectRefused(t, "a replacement with 21 labels beside the repo: label",
					patch(t, full.Key, map[string]any{"labels": append(append([]string{}, edited...), "one-more")}))
				expectRefused(t, "a labels_add past twenty labels beside the repo: label",
					patch(t, full.Key, map[string]any{"labels_add": []string{"one-more"}}))
			})
			t.Run("a repo: label not the issue's own answers what any bad label answers", func(t *testing.T) {
				// A missing issue owns no repo: label, and a closed issue's own is still its own. A long
				// repo: label that is not the issue's own gets the answer a label of 41 characters gets:
				// /api/v1 refuses the label before it answers for the issue's state, as it always has,
				// and /api/v2 answers a missing issue 404 before it reads the body (requireIssueV2).
				type answer struct {
					status int
					code   string
				}
				answerOf := func(t *testing.T, response *httptest.ResponseRecorder) answer {
					t.Helper()
					code, _ := refusalOf(t, response)
					return answer{response.Code, code}
				}
				refusedLabel := answer{http.StatusBadRequest, "LABELS_INPUT"}
				missing := refusedLabel
				if version == "v2" {
					missing = answer{http.StatusNotFound, "ISSUE_NOT_FOUND"}
				}
				closedRepo := "p/" + strings.Repeat("r", 36)
				closed := expectImported(t, importIssue(t, handler, version, closedRepo+"#1", nil), closedRepo, 1)
				if response := patch(t, closed.Key, map[string]any{"status": "done"}); response.Code != http.StatusOK {
					t.Fatalf("closing %s answered %d %s, want 200", closed.Key, response.Code, response.Body.String())
				}
				other := repoLabelPrefix + "x/" + strings.Repeat("r", 36)
				for _, target := range []struct {
					what, key string
					want      answer
				}{
					{"a missing issue", "DEFAULT-999", missing},
					{"a closed issue", closed.Key, refusedLabel},
				} {
					ordinary := answerOf(t, patch(t, target.key, map[string]any{"labels": []string{tooLong}}))
					if ordinary != target.want {
						t.Fatalf("a label of 41 characters on %s answered %+v, want %+v", target.what, ordinary, target.want)
					}
					if got := answerOf(t, patch(t, target.key, map[string]any{"labels": []string{other}})); got != ordinary {
						t.Fatalf("another repository's long repo: label on %s answered %+v, want %+v as a label of 41 characters gets",
							target.what, got, ordinary)
					}
				}
				// Sent back alone, the closed issue's own label is refused for the closed issue, as any
				// valid replacement is; beside another repository's long label, for that label.
				closedOwn := repoLabelPrefix + closedRepo
				if got, want := answerOf(t, patch(t, closed.Key, map[string]any{"labels": []string{closedOwn, "triage"}})),
					(answer{http.StatusConflict, "ISSUE_CLOSED"}); got != want {
					t.Fatalf("a replacement that sends the closed issue's repo: label back answered %+v, want %+v", got, want)
				}
				if got := answerOf(t, patch(t, closed.Key, map[string]any{"labels": []string{closedOwn, other}})); got != refusedLabel {
					t.Fatalf("a replacement on the closed issue with another repository's long repo: label answered %+v, want %+v",
						got, refusedLabel)
				}
			})

			lists := []string{"/api/v1/issues?"}
			if version == "v2" {
				lists = []string{"/api/v2/issues?", "/api/v2/projects/DEFAULT/issues?"}
			}
			for _, list := range lists {
				listed := func(t *testing.T, label string) *httptest.ResponseRecorder {
					t.Helper()
					return dispatchRequest(t, handler, http.MethodGet, list+"label="+label, nil, "alice")
				}
				keys := func(t *testing.T, response *httptest.ResponseRecorder) []string {
					t.Helper()
					if response.Code != http.StatusOK {
						t.Fatalf("GET %s answered %d %s, want 200", list, response.Code, response.Body.String())
					}
					var summaries []model.IssueSummary
					if version == "v1" {
						summaries = decodeBody[[]model.IssueSummary](t, response)
					} else {
						summaries = decodeBody[page[model.IssueSummary]](t, response).Items
					}
					found := []string{}
					for _, summary := range summaries {
						found = append(found, summary.Key)
					}
					slices.Sort(found)
					return found
				}
				t.Run("the repo: filter of "+list, func(t *testing.T) {
					want := []string{issue.Key, full.Key}
					slices.Sort(want)
					if got := keys(t, listed(t, own)); !slices.Equal(got, want) {
						t.Fatalf("the issues labelled %s are %q, want %q", own, got, want)
					}
					// A repo: filter is bounded by GitHub's own names: an owner of 39 characters and a
					// repository of 100.
					longest := repoLabelPrefix + strings.Repeat("o", 39) + "/" + strings.Repeat("r", 100)
					if got := keys(t, listed(t, longest)); len(got) != 0 {
						t.Fatalf("the issues labelled with the longest repository name are %q, want none", got)
					}
					expectRefused(t, "a repo: filter past the longest repository name", listed(t, longest+"r"))
					expectRefused(t, "a filter of 41 characters that names no repository", listed(t, tooLong))
				})
			}
		})
	}
}

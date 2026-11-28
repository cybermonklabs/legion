package delivery

// The two checks of this round that need no database: the names an envelope may name, and the
// one answer of GitHub that ends an owed refusal.

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
)

// An envelope names a pull request only where its values are values GitHub can have
// (pullRequestNamed; review of cybermonklabs/legion#4209 at ceeec40f46, B4). The names at GitHub's
// own bounds, and the characters GitHub's names hold, are taken: a refusal of an honest name would
// lose that pull request's head.
func TestPullRequestNamedTakesEveryNameGitHubCanHaveAndNoOther(t *testing.T) {
	for repo, takes := range map[string]bool{
		"acme/widgets":                       true,
		"Acme-Corp/widgets.v2_beta-1":        true,
		"mona-cat_octo/widgets":              true, // a managed user account's login
		strings.Repeat("a", 39) + "/widgets": true,
		"acme/" + strings.Repeat("w", 100):   true,
		"acme/.github":                       true,
		"acme/_":                             true,
		strings.Repeat("a", 40) + "/widgets": false,
		"acme/" + strings.Repeat("w", 101):   false,
		"acme/" + strings.Repeat("w", 3500):  false,
		"acme/.":                             false,
		"acme/..":                            false,
		"acme/wid gets":                      false,
		"acme/wid%gets":                      false,
		"acme/widgets/pulls":                 false,
		"ac.me/widgets":                      false,
		"acmewidgets":                        false,
		"/widgets":                           false,
		"acme/":                              false,
		"":                                   false,
		"acme/widgéts":                       false,
		"acme/widgets\n":                     false,
	} {
		_, _, err := pullRequestNamed(map[string]string{"repo": repo, "number": "7"})
		if (err == nil) != takes {
			t.Errorf("pullRequestNamed(%.50q) = %v, want taken %v", repo, err, takes)
		}
		if err != nil && !errors.Is(err, errNoPullRequestNamed) {
			t.Errorf("pullRequestNamed(%.50q) = %v, want errNoPullRequestNamed", repo, err)
		}
	}
}

// The evidence that ends an owed refusal is one answer of GitHub, to the absence query asked with
// the token of the read GitHub refused, that shows the repository readable, its pull requests
// readable, and the pull request absent (pullRequestAbsent; the second reader of
// cybermonklabs/legion#4209 at ceeec40f46, D1, and the review at ef5112db43, H1). The first case
// is GitHub's own answer to a number its repository does not have, read with `gh api graphql`;
// every other answer, and every refusal the App holds no token for or that is no 404, is no
// evidence, and the last two make no request at all. The two after the first are what a token
// that reads the repository and not its pull requests may be answered: the same answer with no
// count of the pull requests, and a null count beside GitHub's FORBIDDEN for it. At ef5112db43 the
// query asked no count, so the first of those ended a read.
func TestOneAnswerOfGitHubIsTheEvidenceThatEndsARefusal(t *testing.T) {
	const (
		token    = "ghs_the_refused_reads_token"
		notFound = `{"type":"NOT_FOUND","path":["repository","pullRequest"],"locations":[{"line":1,"column":98}],"message":"Could not resolve to a PullRequest with the number of 7."}`
		counted  = `"pullRequests":{"totalCount":3856},`
	)
	notFoundRead := fmt.Errorf("fetch acme/widgets PR #7: %w (status 404)", ErrPullRequestNotFound)
	for _, kase := range []struct {
		name     string
		refusal  error
		token    string
		status   int
		answer   string
		absent   bool
		requests int32
	}{
		{"githubs_answer_of_a_number_the_repository_does_not_have", notFoundRead, token, http.StatusOK,
			`{"data":{"repository":{` + counted + `"pullRequest":null}},"errors":[` + notFound + `]}`, true, 1},
		{"no_count_of_the_pull_requests", notFoundRead, token, http.StatusOK,
			`{"data":{"repository":{"pullRequest":null}},"errors":[` + notFound + `]}`, false, 1},
		{"a_null_count_and_forbidden_on_it", notFoundRead, token, http.StatusOK,
			`{"data":{"repository":{"pullRequests":null,"pullRequest":null}},"errors":[` + notFound + `,{"type":"FORBIDDEN","path":["repository","pullRequests"],"message":"Resource not accessible by integration"}]}`, false, 1},
		{"a_null_repository", notFoundRead, token, http.StatusOK,
			`{"data":{"repository":null},"errors":[{"type":"NOT_FOUND","path":["repository"],"message":"Could not resolve to a Repository with the name 'acme/widgets'."}]}`, false, 1},
		{"forbidden_on_the_pull_request", notFoundRead, token, http.StatusOK,
			`{"data":{"repository":{` + counted + `"pullRequest":null}},"errors":[{"type":"FORBIDDEN","path":["repository","pullRequest"],"message":"Resource not accessible by integration"}]}`, false, 1},
		{"not_found_beside_another_error", notFoundRead, token, http.StatusOK,
			`{"data":{"repository":{` + counted + `"pullRequest":null}},"errors":[` + notFound + `,{"type":"FORBIDDEN","path":["repository"],"message":"Resource not accessible by integration"}]}`, false, 1},
		{"not_found_on_another_path", notFoundRead, token, http.StatusOK,
			`{"data":{"repository":{` + counted + `"pullRequest":null}},"errors":[{"type":"NOT_FOUND","path":["repository","issue"],"message":"Could not resolve to an Issue with the number of 7."}]}`, false, 1},
		{"a_null_pull_request_and_no_error", notFoundRead, token, http.StatusOK,
			`{"data":{"repository":{` + counted + `"pullRequest":null}}}`, false, 1},
		{"the_pull_request_github_now_answers", notFoundRead, token, http.StatusOK,
			`{"data":{"repository":{` + counted + `"pullRequest":{"number":7}}}}`, false, 1},
		{"a_null_data", notFoundRead, token, http.StatusOK,
			`{"data":null,"errors":[{"type":"INTERNAL","message":"Something went wrong while executing your query."}]}`, false, 1},
		{"status_502", notFoundRead, token, http.StatusBadGateway, `{"message":"Server Error"}`, false, 1},
		{"an_answer_that_does_not_decode", notFoundRead, token, http.StatusOK, `<html>`, false, 1},
		{"no_token_since_no_installation_covers_the_repository", fmt.Errorf("%w on acme/widgets", githubapp.ErrNoInstallation), "", http.StatusOK, "", false, 0},
		{"a_refusal_that_is_no_404", fmt.Errorf("fetch acme/widgets PR #7: %w", githubapp.ErrRepositoryUnreadable), token, http.StatusOK, "", false, 0},
	} {
		t.Run(kase.name, func(t *testing.T) {
			var requests atomic.Int32
			fake := newFakeGitHub(t)
			fake.handle("POST /graphql", func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				query, variables := decodeGraphQLRequest(t, r)
				if got := r.Header.Get("Authorization"); got != "Bearer "+token {
					t.Errorf("the question was asked with %q, want the refused read's own token", got)
				}
				if !strings.Contains(query, "pullRequests { totalCount }") || !strings.Contains(query, "pullRequest(number: $number) { number }") {
					t.Errorf("the question asked %q, want the count of the repository's pull requests and the pull request in one query", query)
				}
				if variables["owner"] != "acme" || variables["name"] != "widgets" || variables["number"] != float64(7) {
					t.Errorf("the question asked about %v, want acme/widgets#7", variables)
				}
				w.WriteHeader(kase.status)
				fmt.Fprint(w, kase.answer)
			})
			refused := &pullRequestRefusal{err: kase.refusal, owner: "acme", repo: "widgets", number: 7, token: kase.token}
			absent, err := pullRequestAbsent(t.Context(), fake.newTestClient(), refused, nil)
			if err != nil {
				t.Fatalf("pullRequestAbsent: %v", err)
			}
			if absent != kase.absent || requests.Load() != kase.requests {
				t.Errorf("absent = %v after %d request(s), want %v after %d", absent, requests.Load(), kase.absent, kase.requests)
			}
		})
	}
}

// A question that GitHub does not answer is no evidence and is answered as its failure, so the
// pass keeps a rate limit and logs any other failure: the connection is dropped before an answer.
func TestAQuestionGitHubDoesNotAnswerIsNoEvidence(t *testing.T) {
	fake := newFakeGitHub(t)
	fake.handle("POST /graphql", func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) })
	refused := &pullRequestRefusal{err: ErrPullRequestNotFound, owner: "acme", repo: "widgets", number: 7, token: "ghs_token"}
	absent, err := pullRequestAbsent(t.Context(), fake.newTestClient(), refused, nil)
	if absent || err == nil {
		t.Fatalf("pullRequestAbsent of a dropped connection = %v, %v; want false and the failure", absent, err)
	}
}

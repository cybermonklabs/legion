package ghbranch

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/leakguard"
	"github.com/sjawhar/legion/daemon/internal/leakguard/leakguardtest"
)

// answering is a scripted answer of the fake (github.answer): the request of method at path is
// answered status with body, and every other request as the fake answers it.
func answering(method, path string, status int, body string) func(*http.Request) (int, string, bool) {
	return func(r *http.Request) (int, string, bool) {
		if r.Method != method || r.URL.Path != path {
			return 0, "", false
		}
		return status, body, true
	}
}

// The fake's routes the scripted answers name.
const (
	repositoryPath = "/repos/acme/widgets"
	refsPath       = "/repos/acme/widgets/git/refs"
	mainRefPath    = "/repos/acme/widgets/git/ref/heads/main"
	graphQLPath    = "/graphql"
)

// write is one call of Create or Reset against g: Create of legion/WIDGETS-12 at main, or Reset of
// legion/ACME-1 from base to the head of main's fallback, from the merged head, each under rules.
type write func(t *testing.T, g *github, rules *leakguard.RuleSet, branch string) error

// creating and resetting are the two writes a case makes.
var (
	creating write = func(t *testing.T, g *github, rules *leakguard.RuleSet, branch string) error {
		t.Helper()
		if branch == "" {
			branch = "legion/WIDGETS-12"
		}
		return Create(context.Background(), widgets(g.serve(t)), ghrepo.MustParse("acme/widgets"), branch, "main", rules)
	}
	resetting write = func(t *testing.T, g *github, rules *leakguard.RuleSet, branch string) error {
		t.Helper()
		if branch == "" {
			branch = resetBranch
		}
		api := g.serve(t)
		_, err := Reset(context.Background(), widgets(api), graphQL(api), ghrepo.MustParse("acme/widgets"), branch, "release/v7.1", "main", mergedCommit, rules)
		return err
	}
)

// Each failure of Create or Reset that the same call meets again is a Refusal of its kind
// (PermanentRefusal), whichever call met it, and its text is the failure's own. The kinds are
// GitHub's documented refusals of the calls the two make: a 403 that is no rate limit, a 404, a
// 409 and a 422 of the REST API; a 403 or a 404 of the GraphQL endpoint, and an updateRefs error of
// type FORBIDDEN or NOT_FOUND; an answer that lacks what the call needs (no commit, no node id, a
// compare status nobody reads); a base and a fallback GitHub has neither of; and a name the
// deployment's rules match.
func TestEachRefusalForGoodNamesItsKind(t *testing.T) {
	rules, err := leakguard.LoadRules([]byte(leakguardtest.Rules))
	if err != nil {
		t.Fatalf("compile the fixture rules: %v", err)
	}
	created := func() (int, string) { return http.StatusCreated, `{}` }
	refusedCreate := func(status int, body string) func() (int, string) {
		return func() (int, string) { return status, body }
	}
	atMerge := func() map[string]string { return map[string]string{"main": baseHead, resetBranch: mergedCommit} }
	for _, tc := range []struct {
		name   string
		write  write
		g      *github
		rules  *leakguard.RuleSet
		branch string
		want   RefusalKind
	}{
		{name: "a create of a name the rules match", write: creating, g: &github{refs: map[string]string{"main": baseCommit}, create: created},
			rules: rules, branch: "legion/" + leakguardtest.RulesHost, want: RefusedName},
		{name: "a move of a name the rules match", write: resetting, g: &github{refs: atMerge()},
			rules: rules, branch: "legion/" + leakguardtest.RulesHost, want: RefusedName},
		{name: "a create GitHub forbids", write: creating, g: &github{refs: map[string]string{"main": baseCommit},
			create: refusedCreate(http.StatusForbidden, `{"message":"Resource not accessible by integration","status":"403"}`)}, want: RefusedForbidden},
		{name: "a create GitHub cannot process", write: creating, g: &github{refs: map[string]string{"main": baseCommit},
			create: refusedCreate(http.StatusUnprocessableEntity, `{"message":"Reference name is invalid","status":"422"}`)}, want: RefusedUnprocessable},
		{name: "a create at a base GitHub does not have", write: creating, g: &github{refs: map[string]string{}, create: created}, want: RefusedNotFound},
		{name: "a create in a repository with no commit", write: creating, g: &github{refs: map[string]string{"main": baseCommit}, create: created,
			answer: answering(http.MethodGet, mainRefPath, http.StatusConflict, `{"message":"Git Repository is empty.","status":"409"}`)}, want: RefusedConflict},
		{name: "a create at a base ref with no commit", write: creating, g: &github{refs: map[string]string{"main": baseCommit}, create: created,
			answer: answering(http.MethodGet, mainRefPath, http.StatusOK, `{"ref":"refs/heads/main","object":{"type":"commit"}}`)}, want: RefusedNoCommit},
		// The repository is one GitHub shows, naming its default branch as every such repository
		// does: Reset reads it before it names the base missing, so a hidden repository is not-found.
		{name: "a move with neither its base nor its fallback", write: resetting, g: &github{refs: map[string]string{resetBranch: mergedCommit}, defaultBranch: "main"}, want: RefusedNoBase},
		{name: "a move whose base ref names no commit", write: resetting, g: &github{refs: atMerge(),
			answer: answering(http.MethodGet, mainRefPath, http.StatusOK, `{"ref":"refs/heads/main","object":{"type":"commit"}}`)}, want: RefusedNoCommit},
		{name: "a move whose repository names no node id", write: resetting, g: &github{refs: atMerge(),
			answer: answering(http.MethodGet, repositoryPath, http.StatusOK, `{"full_name":"acme/widgets","default_branch":"main"}`)}, want: RefusedNoNodeID},
		{name: "a move whose repository GitHub does not show", write: resetting, g: &github{refs: atMerge(),
			answer: answering(http.MethodGet, repositoryPath, http.StatusNotFound, `{"message":"Not Found","status":"404"}`)}, want: RefusedNotFound},
		// A token that cannot see the repository is answered 404 for every read under it, which
		// is GitHub hiding the repository, not a branch it lacks: not-found, as a create names it,
		// never no-base, which would send the architect after a base nobody deleted.
		{name: "a move by a token that cannot see the repository", write: resetting, g: &github{refs: atMerge(),
			answer: func(r *http.Request) (int, string, bool) {
				if r.Method != http.MethodGet || !strings.HasPrefix(r.URL.Path, repositoryPath) {
					return 0, "", false
				}
				return http.StatusNotFound, `{"message":"Not Found","status":"404"}`, true
			}}, want: RefusedNotFound},
		{name: "a move the GraphQL endpoint forbids", write: resetting, g: &github{refs: atMerge(),
			answer: answering(http.MethodPost, graphQLPath, http.StatusForbidden, `{"message":"Resource not accessible by integration"}`)}, want: RefusedGraphQLForbidden},
		{name: "a move with no GraphQL endpoint", write: resetting, g: &github{refs: atMerge(),
			answer: answering(http.MethodPost, graphQLPath, http.StatusNotFound, `{"message":"Not Found"}`)}, want: RefusedGraphQLNotFound},
		{name: "a move updateRefs forbids", write: resetting, g: &github{refs: atMerge(),
			answer: answering(http.MethodPost, graphQLPath, http.StatusOK,
				`{"data":{"updateRefs":null},"errors":[{"type":"FORBIDDEN","path":["updateRefs"],"message":"Resource not accessible by integration"}]}`)}, want: RefusedGraphQLForbidden},
		{name: "a move updateRefs does not find", write: resetting, g: &github{refs: atMerge(),
			answer: answering(http.MethodPost, graphQLPath, http.StatusOK,
				`{"data":{"updateRefs":null},"errors":[{"type":"NOT_FOUND","path":["updateRefs"],"message":"Could not resolve to a node with the global id of 'R_kgDOwidgets'"}]}`)}, want: RefusedGraphQLNotFound},
		{name: "a move whose missing branch GitHub forbids to make", write: resetting, g: &github{refs: map[string]string{"main": baseHead},
			create: refusedCreate(http.StatusForbidden, `{"message":"Resource not accessible by integration","status":"403"}`)}, want: RefusedForbidden},
		{name: "a move whose compare answers a status nobody reads", write: resetting, g: &github{refs: map[string]string{"main": baseHead, resetBranch: laterCommit},
			compared: map[string]string{baseHead + "..." + laterCommit: "unrelated"}}, want: RefusedCompareStatus},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.write(t, tc.g, tc.rules, tc.branch)
			if err == nil {
				t.Fatal("the write succeeded, want a refusal")
			}
			kind, refused := PermanentRefusal(err)
			if !refused || kind != tc.want {
				t.Fatalf("PermanentRefusal(%q) = %q, %v; want %q", err, kind, refused, tc.want)
			}
			var refusal *Refusal
			if !errors.As(err, &refusal) || refusal.Err == nil || refusal.Error() != refusal.Err.Error() {
				t.Fatalf("the refusal %#v does not carry the failure it names", refusal)
			}
		})
	}
}

// A failure a retry may pass is no Refusal, and is returned as it was met: GitHub's trouble (a
// 5xx, the secondary limit's 403, a 429, GraphQL's own rate limit), a token GitHub did not take
// (401), an updateRefs error of no type while the branch still stands at the merged head, and a
// call that got no answer at all. Each names its cause.
func TestAFailureARetryMayPassIsNoRefusal(t *testing.T) {
	atMerge := func() map[string]string { return map[string]string{"main": baseHead, resetBranch: mergedCommit} }
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	for _, tc := range []struct {
		name  string
		call  func(t *testing.T) error
		cause string
	}{
		{name: "a 502 of the create", cause: "with 502", call: func(t *testing.T) error {
			return creating(t, &github{refs: map[string]string{"main": baseCommit},
				create: func() (int, string) { return http.StatusBadGateway, `{"message":"Server Error"}` }}, nil, "")
		}},
		{name: "the secondary limit's 403 of the create", cause: "secondary rate limit", call: func(t *testing.T) error {
			return creating(t, &github{refs: map[string]string{"main": baseCommit}, create: func() (int, string) {
				return http.StatusForbidden, `{"message":"You have exceeded a secondary rate limit. Please wait a few minutes before you try again."}`
			}}, nil, "")
		}},
		{name: "a 503 of the base read", cause: "with 503", call: func(t *testing.T) error {
			return resetting(t, &github{refs: atMerge(), answer: answering(http.MethodGet, mainRefPath, http.StatusServiceUnavailable, `{"message":"Unavailable"}`)}, nil, "")
		}},
		{name: "a 401 of the repository read", cause: "with 401", call: func(t *testing.T) error {
			return resetting(t, &github{refs: atMerge(), answer: answering(http.MethodGet, repositoryPath, http.StatusUnauthorized, `{"message":"Bad credentials"}`)}, nil, "")
		}},
		{name: "a 429 of the GraphQL endpoint", cause: "with 429", call: func(t *testing.T) error {
			return resetting(t, &github{refs: atMerge(), answer: answering(http.MethodPost, graphQLPath, http.StatusTooManyRequests, `{"message":"Too Many Requests"}`)}, nil, "")
		}},
		{name: "a 502 of the GraphQL endpoint", cause: "with 502", call: func(t *testing.T) error {
			return resetting(t, &github{refs: atMerge(), answer: answering(http.MethodPost, graphQLPath, http.StatusBadGateway,
				`{"message":"We couldn't respond to your request in time. Sorry about that."}`)}, nil, "")
		}},
		{name: "GraphQL's own rate limit", cause: "RATE_LIMITED", call: func(t *testing.T) error {
			return resetting(t, &github{refs: atMerge(), answer: answering(http.MethodPost, graphQLPath, http.StatusOK,
				`{"errors":[{"type":"RATE_LIMITED","message":"API rate limit exceeded"}]}`)}, nil, "")
		}},
		{name: "an updateRefs error of no type", cause: "Something went wrong", call: func(t *testing.T) error {
			return resetting(t, &github{refs: atMerge(), refuse: "Something went wrong while executing your query."}, nil, "")
		}},
		{name: "no answer at all", cause: "connection refused", call: func(t *testing.T) error {
			return Create(context.Background(), githubrest.Client{Token: "installation-token", API: githubrest.RepositoryAPI(closed.URL, ghrepo.MustParse("acme/widgets"))},
				ghrepo.MustParse("acme/widgets"), "legion/WIDGETS-12", "main", nil)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call(t)
			if err == nil || !strings.Contains(err.Error(), tc.cause) {
				t.Fatalf("the write = %v, want a failure naming %q", err, tc.cause)
			}
			if kind, refused := PermanentRefusal(err); refused {
				t.Fatalf("PermanentRefusal(%q) = %q, want no refusal: a retry may pass", err, kind)
			}
		})
	}
}

package ghbranch

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
	"github.com/sjawhar/legion/daemon/internal/leakguard"
	"github.com/sjawhar/legion/daemon/internal/leakguard/leakguardtest"
)

const baseCommit = "c0ffee0123456789abcdef0123456789abcdef01"

// github is a repository's REST and GraphQL API as Create, Reset and DefaultBranch call it: the
// repository itself, naming defaultBranch and its node id, each branch of refs by its exact name, a
// create of a ref answered with create's status and body, the GraphQL mutation updateRefs, which
// moves a ref only while it points at the update's beforeOid and answers refuse's message as an
// error where refuse is set, and a compare of two commits answered with the status compared names
// for "base...head". pushAfterRead, where set, is another process's push that lands on the branch
// it names just after the first read of that branch's ref is answered. It records each read, each
// create, each move and each compare; any other request fails the test, so a Create that reads the
// base's tree, or posts a tree or a commit of its own, is caught by name.
type github struct {
	refs          map[string]string
	defaultBranch string
	fullName      string
	create        func() (int, string)
	refuse        string
	compared      map[string]string
	pushAfterRead map[string]string
	mu            sync.Mutex
	read          []string
	creates       []map[string]string
	patches       []patched
	compares      []string
	authorized    []string
}

// widgetsNodeID is acme/widgets's GraphQL node id, which the fake names beside its full name.
const widgetsNodeID = "R_kgDOwidgets"

// patched is one move of a ref the fake made: the branch it named, and the commit it pointed at
// before (before), the one it points at now (sha) and whether the move was forced (force).
type patched struct {
	branch string
	body   map[string]any
}

func (g *github) serve(t *testing.T) string {
	t.Helper()
	return githubtest.Server(t, githubtest.Spec{Record: func(r *http.Request) {
		g.mu.Lock()
		g.authorized = append(g.authorized, r.Header.Get("Authorization"))
		g.mu.Unlock()
	}, Handle: func(w http.ResponseWriter, r *http.Request) {
		branch, isRef := strings.CutPrefix(r.URL.Path, "/repos/acme/widgets/git/ref/heads/")
		basehead, isCompare := strings.CutPrefix(r.URL.Path, "/repos/acme/widgets/compare/")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets":
			g.reading(r.URL.Path)
			fullName := g.fullName
			if fullName == "" {
				fullName = "acme/widgets"
			}
			_, _ = io.WriteString(w, `{"full_name":"`+fullName+`","default_branch":"`+g.defaultBranch+`","node_id":"`+widgetsNodeID+`"}`)
		case r.Method == http.MethodGet && isRef:
			g.reading(r.URL.Path)
			g.mu.Lock()
			commit, found := g.refs[branch]
			if pushed, racing := g.pushAfterRead[branch]; racing && found {
				g.refs[branch] = pushed
				delete(g.pushAfterRead, branch)
			}
			g.mu.Unlock()
			if !found {
				w.WriteHeader(http.StatusNotFound)
				_, _ = io.WriteString(w, `{"message":"Not Found","status":"404"}`)
				return
			}
			_, _ = io.WriteString(w, `{"ref":"refs/heads/`+branch+`","object":{"sha":"`+commit+`","type":"commit"}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/graphql":
			g.updateRefs(t, w, r)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/widgets/git/refs":
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode the create: %v", err)
			}
			g.mu.Lock()
			g.creates = append(g.creates, body)
			g.mu.Unlock()
			status, answer := g.create()
			w.WriteHeader(status)
			_, _ = io.WriteString(w, answer)
		case r.Method == http.MethodGet && isCompare:
			g.mu.Lock()
			g.compares = append(g.compares, basehead+"?"+r.URL.RawQuery)
			g.mu.Unlock()
			status, found := g.compared[basehead]
			if !found {
				t.Errorf("unexpected compare %s", basehead)
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = io.WriteString(w, `{"status":"`+status+`","ahead_by":1,"behind_by":1}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}})
}

// updateRefs answers GitHub's GraphQL mutation updateRefs over the one ref update Reset sends, as
// GitHub's reference documents it: the ref moves to afterOid only while it points at beforeOid.
// Otherwise it moves nothing and answers as GitHub answered a stale beforeOid when it was tried on
// a scratch branch: a 200 with no data and one error, with no type and a generic message. A
// refusal the test scripts (refuse) is answered the same way, whatever the ref points at.
func (g *github) updateRefs(t *testing.T, w http.ResponseWriter, r *http.Request) {
	var body struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || !strings.Contains(body.Query, "updateRefs") {
		t.Errorf("a GraphQL request other than updateRefs: %q (%v)", body.Query, err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	name, _ := body.Variables["name"].(string)
	before, _ := body.Variables["beforeOid"].(string)
	after, _ := body.Variables["afterOid"].(string)
	branch, isBranch := strings.CutPrefix(name, "refs/heads/")
	if body.Variables["repositoryId"] != widgetsNodeID || !isBranch || before == "" || after == "" {
		t.Errorf("updateRefs with variables %v, want acme/widgets's node id, a branch ref, beforeOid and afterOid", body.Variables)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	refusal := g.refuse
	if refusal == "" && g.refs[branch] != before {
		refusal = "Something went wrong while executing your query on 2026-11-25T09:17:24Z."
	}
	if refusal != "" {
		_, _ = io.WriteString(w, `{"errors":[{"message":"`+refusal+`"}]}`)
		return
	}
	g.refs[branch] = after
	g.patches = append(g.patches, patched{branch: branch, body: map[string]any{"before": before, "sha": after, "force": body.Variables["force"]}})
	_, _ = io.WriteString(w, `{"data":{"updateRefs":{"clientMutationId":null}}}`)
}

func (g *github) reading(path string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.read = append(g.read, path)
}

// reads is every path the fake answered a GET for, in order.
func (g *github) reads() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.read)
}

// The branch starts at the base branch the caller names, whatever its name: a default branch with a
// slash reads as one ref path, and the create names its commit.
func TestCreateMakesTheBranchAtTheNamedBase(t *testing.T) {
	for _, base := range []string{"main", "develop", "release/v7.1"} {
		t.Run(base, func(t *testing.T) {
			g := &github{refs: map[string]string{base: baseCommit}, create: func() (int, string) { return http.StatusCreated, `{}` }}
			if err := Create(context.Background(), widgets(g.serve(t)), ghrepo.MustParse("acme/widgets"), "legion/WIDGETS-12", base, nil); err != nil {
				t.Fatalf("Create: %v", err)
			}
			if got := g.reads(); !slices.Equal(got, []string{"/repos/acme/widgets/git/ref/heads/" + base}) {
				t.Fatalf("read %v, want the base ref alone", got)
			}
			if got := g.creates[0]["sha"]; got != baseCommit {
				t.Fatalf("created at %s, want %s", got, baseCommit)
			}
		})
	}
}

const decoyCommit = "dec0de0123456789abcdef0123456789abcdef02"

// A branch name git accepts (`git check-ref-format --branch`) may hold characters a URL reads as
// its own syntax: `#` starts a fragment and `%` an escape. The base is read as that one ref and no
// other, so a ref those characters would turn it into, at another commit (decoy), is never where
// the branch starts, and a base the URL cannot carry unescaped is read all the same.
func TestCreateReadsABaseWhoseNameHoldsURLSyntaxAsItself(t *testing.T) {
	for _, tc := range []struct{ base, decoy string }{
		{"release#stable", "release"},
		{"release%2Fstable", "release/stable"},
		{"release%stable", "release"},
		{"hotfix/#12", "hotfix/"},
	} {
		t.Run(tc.base, func(t *testing.T) {
			g := &github{refs: map[string]string{tc.base: baseCommit, tc.decoy: decoyCommit}, create: func() (int, string) { return http.StatusCreated, `{}` }}
			if err := Create(context.Background(), widgets(g.serve(t)), ghrepo.MustParse("acme/widgets"), "legion/WIDGETS-12", tc.base, nil); err != nil {
				t.Fatalf("Create at %q: %v", tc.base, err)
			}
			if got := g.reads(); !slices.Equal(got, []string{"/repos/acme/widgets/git/ref/heads/" + tc.base}) {
				t.Fatalf("read %q, want the ref %q alone", got, tc.base)
			}
			if got := g.creates[0]["sha"]; got != baseCommit {
				t.Fatalf("created at %s, want %q's %s, not %q's %s", got, tc.base, baseCommit, tc.decoy, decoyCommit)
			}
		})
	}
}

// A base GitHub does not have is refused naming it, before anything is created.
func TestCreateRefusesABaseGitHubDoesNotHave(t *testing.T) {
	g := &github{refs: map[string]string{"main": baseCommit}}
	err := Create(context.Background(), widgets(g.serve(t)), ghrepo.MustParse("acme/widgets"), "legion/WIDGETS-12", "develop", nil)
	if err == nil || !strings.Contains(err.Error(), "read refs/heads/develop of acme/widgets") || len(g.creates) != 0 {
		t.Fatalf("err=%v creates=%d, want a refusal naming refs/heads/develop and no create", err, len(g.creates))
	}
}

// DefaultBranch is GitHub's default_branch for the repository, and an answer naming none is refused.
func TestDefaultBranchReadsTheRepository(t *testing.T) {
	g := &github{defaultBranch: "release/v7.1"}
	got, err := DefaultBranch(context.Background(), widgets(g.serve(t)), ghrepo.MustParse("acme/widgets"))
	if err != nil || got != "release/v7.1" {
		t.Fatalf("DefaultBranch = %q, %v", got, err)
	}
	g.defaultBranch = ""
	if _, err := DefaultBranch(context.Background(), widgets(g.serve(t)), ghrepo.MustParse("acme/widgets")); err == nil || !strings.Contains(err.Error(), "named no default branch") {
		t.Fatalf("empty default_branch: %v, want a refusal", err)
	}
}

// Read is the repository as GitHub names it, its own spelling of owner and name (`full_name`) and
// its default branch: Dispatch may hand a name lowercased, and GitHub's events, and the subjects
// Envoy publishes them on, carry GitHub's spelling.
func TestReadIsTheRepositoryAsGitHubNamesIt(t *testing.T) {
	g := &github{defaultBranch: "release/v7.1", fullName: "Acme/Widgets"}
	got, err := Read(context.Background(), widgets(g.serve(t)), ghrepo.MustParse("acme/widgets"))
	if err != nil || got.Name != ghrepo.MustParse("Acme/Widgets") || got.DefaultBranch != "release/v7.1" {
		t.Fatalf("Read = %+v, %v; want Acme/Widgets at release/v7.1", got, err)
	}
	g.fullName = "acme"
	if _, err := Read(context.Background(), widgets(g.serve(t)), ghrepo.MustParse("acme/widgets")); err == nil || !strings.Contains(err.Error(), "full_name") {
		t.Fatalf("a full_name that is no owner/name: %v, want a refusal naming it", err)
	}
}

// A repository GitHub does not show is refused naming it and GitHub's answer.
func TestDefaultBranchRefusesARepositoryItCannotRead(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"message":"Not Found","status":"404"}`+"\n")
	}))
	t.Cleanup(server.Close)
	_, err := DefaultBranch(context.Background(), widgets(server.URL), ghrepo.MustParse("acme/widgets"))
	if want := `read the default branch of acme/widgets: GitHub answered GET /repos/acme/widgets with 404: {"message":"Not Found","status":"404"}`; errorText(err) != want {
		t.Fatalf("DefaultBranch error = %q, want %q", errorText(err), want)
	}
}

// A default-branch read GitHub refuses for its rate limit carries GitHub's answer through Read's
// wrap: a caller tells the limit, and how long GitHub asks it to wait, from a refusal of the
// repository such as the 404 above, and reads the limit's headers as GitHub sent them.
func TestADefaultBranchReadGitHubRateLimitsCarriesTheLimit(t *testing.T) {
	// Ten minutes out, so no scheduling delay can carry the clock past it before the answer is read.
	reset := strconv.FormatInt(time.Now().Add(10*time.Minute).Unix(), 10)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Limit", "15000")
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", reset)
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"message":"API rate limit exceeded for installation ID 7.","status":"403"}`)
	}))
	t.Cleanup(server.Close)
	_, err := Read(context.Background(), widgets(server.URL), ghrepo.MustParse("acme/widgets"))
	var answer *githubrest.Answer
	if !errors.As(err, &answer) || !answer.RateLimited {
		t.Fatalf("Read = %v, want GitHub's rate-limited *githubrest.Answer", err)
	}
	if answer.RetryAfter < 9*time.Minute || answer.RetryAfter > 10*time.Minute {
		t.Errorf("the limit's wait = %s, want until the reset ten minutes away", answer.RetryAfter)
	}
	if want := (githubrest.LimitHeaders{Limit: "15000", Remaining: "0", Reset: reset}); answer.Limit != want {
		t.Errorf("the limit's headers = %+v, want %+v", answer.Limit, want)
	}
	if want := `read the default branch of acme/widgets: GitHub answered GET /repos/acme/widgets with 403: {"message":"API rate limit exceeded for installation ID 7.","status":"403"}`; errorText(err) != want {
		t.Errorf("Read error = %q, want %q", errorText(err), want)
	}
}

// The branch starts at the base's own commit, whatever the base's tree holds: no role of the issue
// has run yet, and nothing a role reads comes from the branch's tree, since every phase's handoff
// is a record on the issue's Dispatch issue rather than a file on the branch (sjawhar/legion#2354).
// Create reads the base's ref and creates the branch there, and reads, posts or commits no tree:
// the fake answers nothing else.
func TestCreateMakesTheBranchAtTheBaseOrLeavesTheOneGitHubHas(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{name: "created", status: http.StatusCreated, body: `{"ref":"refs/heads/legion/WIDGETS-12","object":{"sha":"` + baseCommit + `"}}`},
		{name: "already exists", status: http.StatusUnprocessableEntity,
			body: `{"message":"Reference already exists","documentation_url":"https://docs.github.com/rest/git/refs#create-a-reference","status":"422"}`},
		{name: "refused", status: http.StatusForbidden,
			body: `{"message":"Resource not accessible by integration","status":"403"}`,
			want: `create refs/heads/legion/WIDGETS-12 on acme/widgets at ` + baseCommit +
				`: GitHub answered POST /git/refs with 403: {"message":"Resource not accessible by integration","status":"403"}`},
		{name: "another 422", status: http.StatusUnprocessableEntity,
			body: `{"message":"Reference update failed","status":"422"}`,
			want: `create refs/heads/legion/WIDGETS-12 on acme/widgets at ` + baseCommit +
				`: GitHub answered POST /git/refs with 422: {"message":"Reference update failed","status":"422"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := &github{refs: map[string]string{"main": baseCommit}, create: func() (int, string) { return tc.status, tc.body }}
			api := g.serve(t)

			err := Create(context.Background(), widgets(api), ghrepo.MustParse("acme/widgets"), "legion/WIDGETS-12", "main", nil)

			if got := errorText(err); got != tc.want {
				t.Fatalf("Create error = %q, want %q", got, tc.want)
			}
			want := map[string]string{"ref": "refs/heads/legion/WIDGETS-12", "sha": baseCommit}
			if len(g.creates) != 1 || len(g.creates[0]) != 2 || g.creates[0]["ref"] != want["ref"] || g.creates[0]["sha"] != want["sha"] {
				t.Fatalf("creates = %v, want one of %v", g.creates, want)
			}
			for _, authorization := range g.authorized {
				if authorization != "Bearer installation-token" {
					t.Fatalf("a request authorized as %q, want the installation token", authorization)
				}
			}
		})
	}
}

// A base GitHub does not show is refused before anything is created, naming its answer.
func TestCreateRefusesARepositoryWhoseBaseItCannotRead(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("unexpected %s %s after the base could not be read", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"message":"Not Found","status":"404"}`+"\n")
	}))
	t.Cleanup(server.Close)

	err := Create(context.Background(), widgets(server.URL), ghrepo.MustParse("acme/widgets"), "legion/WIDGETS-12", "release/v7.1", nil)

	if want := `read refs/heads/release/v7.1 of acme/widgets: GitHub answered GET /git/ref/heads/release/v7.1 with 404: {"message":"Not Found","status":"404"}`; errorText(err) != want {
		t.Fatalf("Create error = %q, want %q", errorText(err), want)
	}
}

// widgets is acme/widgets's REST API under api, called with the installation token.
func widgets(api string) githubrest.Client {
	return githubrest.Client{Token: "installation-token", API: githubrest.RepositoryAPI(api, ghrepo.MustParse("acme/widgets"))}
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// The ref name passes the rules the caller hands Create, and no others. With the fixture's rules
// (leakguardtest), a branch naming the fixture's invented host is refused before anything is
// sent: GitHub is asked for nothing, no ref is created, and the refusal shows the ref name the
// rule matched. A clean name of the same shape is created. With no rules, a deployment that names
// no rule file, the same branch is created, even where the process's environment points
// LEGION_PRIVATE_NAME_RULES_FILE at the fixture's file: the rules are what the daemon compiled at
// boot, never a file the environment the create runs in names.
func TestCreateRunsTheDeploymentsRules(t *testing.T) {
	rules, err := leakguard.LoadRules([]byte(leakguardtest.Rules))
	if err != nil {
		t.Fatalf("compile the fixture rules: %v", err)
	}
	branch := "legion/" + leakguardtest.RulesHost

	t.Run("refused with the rules", func(t *testing.T) {
		g := &github{refs: map[string]string{"main": baseCommit}, create: func() (int, string) { return http.StatusCreated, `{}` }}
		api := g.serve(t)

		err := Create(context.Background(), widgets(api), ghrepo.MustParse("acme/widgets"), branch, "main", rules)

		var refused *leakguard.Refusal
		if !errors.As(err, &refused) {
			t.Fatalf("Create of a branch naming the fixture's host = %v, want a leak guard refusal", err)
		}
		want := []string{"the ref name: " + leakguardtest.RulesHostClass + `: matched "` + leakguardtest.RulesHost + `" on line 1: "refs/heads/` + branch + `"`}
		if !slices.Equal(refused.Lines, want) {
			t.Errorf("the report is %q, want %q", refused.Lines, want)
		}
		if len(g.creates) != 0 || len(g.reads()) != 0 {
			t.Errorf("GitHub was asked for %v and created %v, want nothing of either", g.reads(), g.creates)
		}

		if err := Create(context.Background(), widgets(api), ghrepo.MustParse("acme/widgets"), "legion/WIDGETS-12", "main", rules); err != nil {
			t.Fatalf("Create of a clean branch name: %v", err)
		}
		if len(g.creates) != 1 || g.creates[0]["ref"] != "refs/heads/legion/WIDGETS-12" || g.creates[0]["sha"] != baseCommit {
			t.Errorf("creates = %v, want the one clean ref at the base", g.creates)
		}
	})

	t.Run("created with none", func(t *testing.T) {
		t.Setenv(leakguard.RulesFileVar, leakguardtest.WriteRules(t, t.TempDir()))
		g := &github{refs: map[string]string{"main": baseCommit}, create: func() (int, string) { return http.StatusCreated, `{}` }}

		if err := Create(context.Background(), widgets(g.serve(t)), ghrepo.MustParse("acme/widgets"), branch, "main", nil); err != nil {
			t.Fatalf("Create with no rules: %v", err)
		}
		if len(g.creates) != 1 || g.creates[0]["ref"] != "refs/heads/"+branch || g.creates[0]["sha"] != baseCommit {
			t.Errorf("creates = %v, want the branch at the base", g.creates)
		}
	})
}

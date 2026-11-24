package api

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/githubtest"
)

// issue_links_attribution_test.go holds R19 of the spec of several open pull requests as a change
// of an issue's links meets it: a pull request that an issue links by its URL is that issue's
// pull request, in any repository, while the issue links it, and one whose head is in a fork is
// no issue's, linked or not. A link a person makes by hand is answered from what Dispatch holds,
// with no read of GitHub. Each pull request reaches Dispatch through a writer that reads it from a
// stand-in GitHub (delivery.Attributor.RefreshPullRequestHead), as the reconcile reads one, and
// the stand-in counts every request, so a test sees whether the PATCH asked GitHub anything. The
// file builds at a commit whose PATCH read GitHub after a change of the links, and fails there on
// its own assertion.

// changedLinkPull is one pull request changedLinkGitHub answers: its title, body, head branch, the
// repository its head branch is in, its head commit, and whether it merged. A refused one is
// answered with the 403 GitHub refuses an installation token a resource with, which is no rate
// limit: GitHub says nothing of it.
type changedLinkPull struct {
	title, body, headRef, headRepo, sha string
	merged, refused                     bool
}

// changedLinkGitHub is GitHub for the pull requests a change of an issue's links names: each
// pull request of pulls, keyed `owner/repo#n`, answered whole as GET /repos/{o}/{r}/pulls/{n}
// answers it, with no commit messages, and every other one GitHub's 404. requests counts every
// request GitHub was sent, of any route.
type changedLinkGitHub struct {
	mu       sync.Mutex
	pulls    map[string]changedLinkPull
	requests int
}

// requested is how many requests GitHub was sent, so far.
func (g *changedLinkGitHub) requested() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.requests
}

// set answers pull as GitHub's pull request key (`owner/repo#n`) from now on.
func (g *changedLinkGitHub) set(key string, pull changedLinkPull) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pulls[key] = pull
}

// changedLinkServer is a Dispatch holding an App key, installed on every repository, whose GitHub
// answers from github, and the server, whose GitHub client and broker a test reads a pull request
// in through (readPull).
func changedLinkServer(t *testing.T, github *changedLinkGitHub) (http.Handler, *store.Store, *server) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate fixture key: %v", err)
	}
	pemText := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	if github.pulls == nil {
		github.pulls = map[string]changedLinkPull{}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/installation", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"id":77,"app_slug":"dispatch-test","permissions":{"contents":"read","pull_requests":"read"}}`)
	})
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"token":"ghs_fake_77","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/pulls/{number}", func(w http.ResponseWriter, r *http.Request) {
		repo, number := r.PathValue("owner")+"/"+r.PathValue("repo"), r.PathValue("number")
		github.mu.Lock()
		pull, known := github.pulls[repo+"#"+number]
		github.mu.Unlock()
		if !known {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"Not Found"}`)
			return
		}
		if pull.refused {
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"message":"Resource not accessible by integration"}`)
			return
		}
		state, mergedAt := "open", "null"
		if pull.merged {
			state, mergedAt = "closed", `"2026-11-16T00:00:00Z"`
		}
		fmt.Fprintf(w, `{"number":%s,"title":%q,"body":%q,"html_url":"https://github.com/%s/pull/%s",
			"state":%q,"merged":%t,"merged_at":%s,"updated_at":"2026-11-16T00:00:00Z",
			"head":{"ref":%q,"sha":%q,"repo":{"full_name":%q}},"base":{"ref":"main"}}`,
			number, pull.title, pull.body, repo, number, state, pull.merged, mergedAt, pull.headRef, pull.sha, pull.headRepo)
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/pulls/{number}/commits", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `[]`)
	})
	upstream := githubtest.Server(t, githubtest.Spec{
		Record: func(*http.Request) {
			github.mu.Lock()
			github.requests++
			github.mu.Unlock()
		},
		Handle: mux.ServeHTTP,
	})
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{
		app:           &auth.AppConfig{ClientID: "Iv1.test", PEM: pemText},
		githubAPIBase: upstream,
	})
	return handler, database, served
}

// readPull has Dispatch read repo#number from GitHub and store its row, as the reconcile's pass
// reads one (delivery.Attributor.RefreshPullRequestHead): the row's issue is the attribution's
// over the links the issues hold now, with the inputs it was judged from stored beside it.
func readPull(t *testing.T, served *server, repo string, number int) {
	t.Helper()
	if err := served.deps.Attributor.RefreshPullRequestHead(context.Background(), served.deps.Store.Pool, served.deps.GitHub,
		served.deps.Events, repo, number, time.Now()); err != nil {
		t.Fatalf("read %s#%d from GitHub: %v", repo, number, err)
	}
}

// storeMerge stores repo#number merged at sha, with the issue the attribution gave it when a
// writer last judged it (nil for nobody) and the source that named it, as a writer of the release
// before this one leaves the row: with no inputs stored beside the issue.
func storeMerge(t *testing.T, database *store.Store, repo string, number int, sha string, issue *string, source string) {
	t.Helper()
	eventAt := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	read := time.Now().Add(-time.Minute)
	if err := delivery.NewAttributor(refs.Renames{}).UpsertPullRequestHead(context.Background(), database.Pool, nil, delivery.PullRequestHead{
		Repo: repo, Number: number, HeadSHA: sha, HeadRef: "work/deploy-job", BaseRef: "main",
		State: "merged", IssueKey: issue, AttributionSource: source, AttributionComplete: true,
		Title: "Move the deploy job", URL: "https://github.com/" + repo + "/pull/" + strconv.Itoa(number),
		EventAt: &eventAt, CheckedAt: &read,
	}); err != nil {
		t.Fatalf("store the merged head of %s#%d: %v", repo, number, err)
	}
}

// headNumbersOf is the number of each pull request the results read compares issue's
// `before_merge` checks with (delivery.Attributor.HeadsAttributedTo), in order, read with no
// rename pairs, as the test server's own Attributor reads them.
func headNumbersOf(t *testing.T, database *store.Store, issue string) []int {
	t.Helper()
	heads, err := delivery.NewAttributor(refs.Renames{}).HeadsAttributedTo(context.Background(), database.Pool, issue)
	if err != nil {
		t.Fatalf("read the heads of %s: %v", issue, err)
	}
	numbers := []int{}
	for _, head := range heads {
		numbers = append(numbers, head.Number)
	}
	slices.Sort(numbers)
	return numbers
}

// attributionMovesOf counts the issue.pull_request_attribution_changed events on issue that move
// repo#number away from it (off) or onto it (!off).
func attributionMovesOf(t *testing.T, database *store.Store, issue, repo string, number int, off bool) int {
	t.Helper()
	side := "to"
	if off {
		side = "from"
	}
	var count int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from events
		where issue_key = $1 and type = $2 and payload->>'repo' = $3
			and (payload->>'number')::int = $4 and payload->>$5 = $1
	`, issue, delivery.AttributionEventType, repo, number, side).Scan(&count); err != nil {
		t.Fatalf("count the moves of %s#%d %s %s: %v", repo, number, side, issue, err)
	}
	return count
}

// R19: removing a link takes the pull request it names from the issue at the removal, merged or
// not, in a repository the issue does not resolve to as much as in its own: no read of the
// issue's results counts its head once the PATCH has answered, the move is on the issue's
// timeline, and GitHub is asked nothing. Before, the PATCH read each such pull request from GitHub
// after its commit. #12 is a row whose inputs a read stored; #16 is a row a writer of the release
// before stored with none, which GitHub refuses to read: the removal takes it from the issue too.
// A pull request the issue still links stays the issue's.
func TestRemovingALinkTakesThePullRequestFromTheIssueAtOnce(t *testing.T) {
	t.Parallel()
	const (
		removedHead = "12c12c12c12c12c12c12c12c12c12c12c12c12c1"
		keptHead    = "13d13d13d13d13d13d13d13d13d13d13d13d13d1"
		refusedHead = "16a16a16a16a16a16a16a16a16a16a16a16a16a1"
		removedURL  = "https://github.com/acme/tools/pull/12"
		keptURL     = "https://github.com/acme/tools/pull/13"
		refusedURL  = "https://github.com/acme/tools/pull/16"
	)
	github := &changedLinkGitHub{pulls: map[string]changedLinkPull{
		"acme/tools#12": {title: "Move the deploy job", headRef: "work/deploy-job", headRepo: "acme/tools", sha: removedHead, merged: true},
		"acme/tools#13": {title: "Move the build job", headRef: "work/build-job", headRepo: "acme/tools", sha: keptHead, merged: true},
		"acme/tools#16": {refused: true},
	}}
	handler, database, served := changedLinkServer(t, github)
	issue := issueInRepository(t, handler, "RELR", "acme/widgets", "Work in two repositories")
	linkPullRequests(t, handler, issue.Key, removedURL, keptURL, refusedURL)
	readPull(t, served, "acme/tools", 12)
	readPull(t, served, "acme/tools", 13)
	storeMerge(t, database, "acme/tools", 16, refusedHead, &issue.Key, "external_link")
	if heads := headNumbersOf(t, database, issue.Key); !slices.Equal(heads, []int{12, 13, 16}) {
		t.Fatalf("the heads of %s while it links #12, #13 and #16 = %v, want [12 13 16]", issue.Key, heads)
	}
	requestsBefore := github.requested()

	linkPullRequests(t, handler, issue.Key, keptURL)

	if heads := headNumbersOf(t, database, issue.Key); !slices.Equal(heads, []int{13}) {
		t.Fatalf("the heads of %s once its links to acme/tools#12 and #16 are removed = %v, want [13]: a pull request is the issue's while the issue links it",
			issue.Key, heads)
	}
	for _, number := range []int{12, 16} {
		if moves := attributionMovesOf(t, database, issue.Key, "acme/tools", number, true); moves != 1 {
			t.Fatalf("%s's timeline holds %d moves of acme/tools#%d off it, want 1", issue.Key, moves, number)
		}
	}
	if requests := github.requested() - requestsBefore; requests != 0 {
		t.Fatalf("the PATCH that removed two links sent GitHub %d requests, want none: a link a person makes by hand is answered from what Dispatch holds", requests)
	}
}

// R19: linking a pull request makes it the issue's at the link, merged or not. A merged pull
// request of another repository that a read stored before the issue linked it, and so gave no
// issue, is one of the issue's heads once the PATCH that links it has answered, with no request to
// GitHub: the read stored that its head is a branch of its own repository. One whose head is in a
// fork is no head, linked or not.
func TestLinkingAMergedPullRequestMakesItTheIssuesAtOnce(t *testing.T) {
	t.Parallel()
	const (
		ownHead  = "14e14e14e14e14e14e14e14e14e14e14e14e14e1"
		forkHead = "15f15f15f15f15f15f15f15f15f15f15f15f15f1"
	)
	github := &changedLinkGitHub{pulls: map[string]changedLinkPull{
		"acme/tools#14": {title: "Move the deploy job", headRef: "work/deploy-job", headRepo: "acme/tools", sha: ownHead, merged: true},
		"acme/tools#15": {title: "Move the deploy job", headRef: "work/deploy-job", headRepo: "contributor/tools", sha: forkHead, merged: true},
	}}
	handler, database, served := changedLinkServer(t, github)
	issue := issueInRepository(t, handler, "RELA", "acme/widgets", "Work linked after its merge")
	readPull(t, served, "acme/tools", 14)
	readPull(t, served, "acme/tools", 15)
	requestsBefore := github.requested()

	linkPullRequests(t, handler, issue.Key,
		"https://github.com/acme/tools/pull/14", "https://github.com/acme/tools/pull/15")

	if heads := headNumbersOf(t, database, issue.Key); !slices.Equal(heads, []int{14}) {
		t.Fatalf("the heads of %s once it links acme/tools#14 and the fork's #15 = %v, want [14]", issue.Key, heads)
	}
	if moves := attributionMovesOf(t, database, issue.Key, "acme/tools", 14, false); moves != 1 {
		t.Fatalf("%s's timeline holds %d moves of acme/tools#14 onto it, want 1", issue.Key, moves)
	}
	if requests := github.requested() - requestsBefore; requests != 0 {
		t.Fatalf("the PATCH that linked two pull requests sent GitHub %d requests, want none", requests)
	}
}

// The attribution's second source is a link of the issue too: a pull request whose body cites a
// GitHub issue the issue links is the issue's, where the issue resolves to the pull request's
// repository. Linking that GitHub issue gives the issue every stored pull request whose body cites
// it, merged or not, at the link and with no request to GitHub: the read that stored each one
// stored its citations. Before, a merged one was the issue's only once the attribution rule next
// changed. A pull request whose head is in a fork is none, and one that cites another GitHub issue
// is none.
func TestLinkingAGitHubIssueGivesTheIssueThePullRequestsThatCiteIt(t *testing.T) {
	t.Parallel()
	const (
		citedHead = "41a41a41a41a41a41a41a41a41a41a41a41a41a1"
		forkHead  = "42b42b42b42b42b42b42b42b42b42b42b42b42b1"
		otherHead = "43c43c43c43c43c43c43c43c43c43c43c43c43c1"
	)
	github := &changedLinkGitHub{pulls: map[string]changedLinkPull{
		"acme/widgets#41": {title: "Move the deploy job", body: "Fixes #71.", headRef: "work/deploy-job", headRepo: "acme/widgets", sha: citedHead, merged: true},
		"acme/widgets#42": {title: "Move the deploy job", body: "Fixes #71.", headRef: "work/deploy-job", headRepo: "contributor/widgets", sha: forkHead, merged: true},
		"acme/widgets#43": {title: "Move the build job", body: "Fixes #72.", headRef: "work/build-job", headRepo: "acme/widgets", sha: otherHead, merged: true},
	}}
	handler, database, served := changedLinkServer(t, github)
	issue := issueInRepository(t, handler, "RELG", "acme/widgets", "Work its GitHub issue names")
	for _, number := range []int{41, 42, 43} {
		readPull(t, served, "acme/widgets", number)
	}
	if heads := headNumbersOf(t, database, issue.Key); len(heads) != 0 {
		t.Fatalf("the heads of %s before it links the GitHub issue = %v, want none", issue.Key, heads)
	}
	requestsBefore := github.requested()

	linkPullRequests(t, handler, issue.Key, "https://github.com/acme/widgets/issues/71")

	if heads := headNumbersOf(t, database, issue.Key); !slices.Equal(heads, []int{41}) {
		t.Fatalf("the heads of %s once it links acme/widgets/issues/71 = %v, want [41]: #41's body cites it, #42's head is a fork's and #43 cites another issue",
			issue.Key, heads)
	}
	if moves := attributionMovesOf(t, database, issue.Key, "acme/widgets", 41, false); moves != 1 {
		t.Fatalf("%s's timeline holds %d moves of acme/widgets#41 onto it, want 1", issue.Key, moves)
	}
	if requests := github.requested() - requestsBefore; requests != 0 {
		t.Fatalf("the PATCH that linked a GitHub issue sent GitHub %d requests, want none", requests)
	}
}

// Removing a link to a GitHub issue takes from the issue only the pull requests no remaining link
// or citation holds, at the removal and with no request to GitHub: #31 cites only the removed
// issue and goes; #32 cites a GitHub issue the issue still links and stays, with no move; #33 is
// the issue's by its branch's name and stays; #35 cites only the kept issue and stays; and #34, a
// row a writer of the release before stored with no inputs, the issue held by a citation and goes,
// since nothing stored says which citation that was. Before, the PATCH took every pull request the
// issue held by a citation and read each from GitHub after its commit to give some back.
func TestRemovingAnIssueLinkTakesThePullRequestsItGaveAtOnce(t *testing.T) {
	t.Parallel()
	const (
		removedLink = "https://github.com/acme/widgets/issues/71"
		keptLink    = "https://github.com/acme/widgets/issues/82"
		citedHead   = "31a31a31a31a31a31a31a31a31a31a31a31a31a1"
		bothHead    = "32b32b32b32b32b32b32b32b32b32b32b32b32b1"
		branchHead  = "33c33c33c33c33c33c33c33c33c33c33c33c33c1"
		legacyHead  = "34d34d34d34d34d34d34d34d34d34d34d34d34d1"
		keptHead    = "35e35e35e35e35e35e35e35e35e35e35e35e35e1"
	)
	github := &changedLinkGitHub{pulls: map[string]changedLinkPull{
		"acme/widgets#31": {title: "Move the deploy job", body: "Fixes #71.", headRef: "work/deploy-job", headRepo: "acme/widgets", sha: citedHead, merged: true},
		"acme/widgets#32": {title: "Move the build job", body: "Fixes #71. Follows #82.", headRef: "work/build-job", headRepo: "acme/widgets", sha: bothHead, merged: true},
		"acme/widgets#34": {refused: true},
		"acme/widgets#35": {title: "Move the lint job", body: "Follows #82.", headRef: "work/lint-job", headRepo: "acme/widgets", sha: keptHead, merged: true},
	}}
	handler, database, served := changedLinkServer(t, github)
	issue := issueInRepository(t, handler, "RELC", "acme/widgets", "Work its GitHub issues name")
	github.set("acme/widgets#33", changedLinkPull{
		title: "Move the test job", headRef: "work/" + issue.Key, headRepo: "acme/widgets", sha: branchHead, merged: true,
	})
	linkPullRequests(t, handler, issue.Key, removedLink, keptLink)
	for _, number := range []int{31, 32, 33, 35} {
		readPull(t, served, "acme/widgets", number)
	}
	storeMerge(t, database, "acme/widgets", 34, legacyHead, &issue.Key, "github_issue_link")
	if heads := headNumbersOf(t, database, issue.Key); !slices.Equal(heads, []int{31, 32, 33, 34, 35}) {
		t.Fatalf("the heads of %s while it links both GitHub issues = %v, want [31 32 33 34 35]", issue.Key, heads)
	}
	requestsBefore := github.requested()

	linkPullRequests(t, handler, issue.Key, keptLink)

	if heads := headNumbersOf(t, database, issue.Key); !slices.Equal(heads, []int{32, 33, 35}) {
		t.Fatalf("the heads of %s once its link to acme/widgets/issues/71 is removed = %v, want [32 33 35]: only #31 and #34 were the issue's by that link alone",
			issue.Key, heads)
	}
	for number, want := range map[int]int{31: 1, 32: 0, 33: 0, 34: 1, 35: 0} {
		if moves := attributionMovesOf(t, database, issue.Key, "acme/widgets", number, true); moves != want {
			t.Fatalf("%s's timeline holds %d moves of acme/widgets#%d off it, want %d", issue.Key, moves, number, want)
		}
	}
	if requests := github.requested() - requestsBefore; requests != 0 {
		t.Fatalf("the PATCH that removed a GitHub-issue link sent GitHub %d requests, want none", requests)
	}
}

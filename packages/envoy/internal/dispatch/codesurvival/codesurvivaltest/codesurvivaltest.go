// Package codesurvivaltest is the GitHub double the tests of the code survival run read: a
// repository's branch, built with the real git (survivaltest) and read back once into memory, served
// as GitHub's REST API serves the routes the run names (dispatch://INFRA-3206, plan, "The run"), in
// the shape of delivery's own fake GitHub (delivery/github_prs_test.go) and through
// internal/githubtest's server.
//
// The double answers the App's installation lookup and token mint, the repository and its default
// branch's ref, the commits list (`sha`, `since` and pages), each first-parent commit's patch by its
// first parent through the commit route and the compare route (the diff media type), a commit's
// paged files[], the closed pull requests list, and a pull request's commit messages. A test sets
// what it must prove on it: a refusal of one commit's patch (a 406, 422 or 5xx naming the diff, an
// answer past the run's 32 MiB, files[] entries withheld), the rate-limit headers every answer
// carries (also "below the floor once n patches were read"), a force-push (another branch served in
// the repository's place), a commit listed that is off the branch, a patch held back until other
// patches were read, and a failure of every request. It records every request with its method,
// path, query and Accept.
//
// Every git command runs when a test builds its history and reads it back (History.Branch), never
// while the double serves, so a run under test that reads it starts no process of its own or of the
// double's.
package codesurvivaltest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/survival"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/survival/survivaltest"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/githubapptest"
	"github.com/sjawhar/envoy/internal/githubtest"
)

// BranchName is the default branch of every repository the double serves.
const BranchName = "main"

// DiffMediaType is the Accept value of a patch read through the commit or the compare route.
const DiffMediaType = "application/vnd.github.diff"

// perPage is the page size of the double's lists: GitHub's own maximum, which the run asks for.
const perPage = 100

// Pull is one merged pull request of a history: its number, head branch, the login that opened it
// (GitHub's REST user.login), title and body, when it merged, and the commit it landed as
// (merge_commit_sha), which History fills in.
type Pull struct {
	Number   int
	HeadRef  string
	Author   string
	Title    string
	Body     string
	MergedAt time.Time
	SHA      string
}

// History is a branch a test builds with the real git on the branch main, and the pull requests
// its commits landed.
type History struct {
	// Repo is the repository: a test writes files in its working tree and commits them here.
	Repo  *survivaltest.Repo
	t     testing.TB
	pulls []Pull
}

// NewHistory is an empty repository a test builds its branch in.
func NewHistory(t testing.TB) *History {
	t.Helper()
	return &History{Repo: survivaltest.New(t), t: t}
}

// Push commits every change of the working tree at at as a commit that merged no pull request, and
// answers its SHA.
func (h *History) Push(message string, at time.Time) string {
	h.t.Helper()
	return h.Repo.Commit(message, at)
}

// Land commits every change of the working tree at at as the commit pull landed as (a squash
// merge, with one parent), and answers its SHA. pull merged at at.
func (h *History) Land(pull Pull, at time.Time) string {
	h.t.Helper()
	sha := h.Repo.Commit(fmt.Sprintf("%s (#%d)", pull.Title, pull.Number), at)
	pull.MergedAt, pull.SHA = at, sha
	h.pulls = append(h.pulls, pull)
	return sha
}

// Merge merges branch into main with a merge commit (two parents) at at, as the commit pull landed
// as, and answers its SHA. The current branch must be main.
func (h *History) Merge(branch string, pull Pull, at time.Time) string {
	h.t.Helper()
	sha := h.Repo.Merge(branch, fmt.Sprintf("Merge pull request #%d from %s", pull.Number, branch), at)
	pull.MergedAt, pull.SHA = at, sha
	h.pulls = append(h.pulls, pull)
	return sha
}

// Reset moves the branch main to sha, a commit of the history, and checks it out: a force-push,
// whose new commits the test then makes from there. The pull requests that landed a commit the
// branch no longer reaches are no part of the branch Branch reads after it.
func (h *History) Reset(sha string) {
	h.t.Helper()
	h.Repo.Git("checkout", "-q", "-B", BranchName, sha)
}

// Commit is one commit a Branch lists: its SHA, its parents (the first parent first) and its
// committer time.
type Commit struct {
	SHA         string
	Parents     []string
	CommittedAt time.Time
}

// Branch is what the double serves of one repository: its tip, every commit reachable from it,
// newest first, the patch of each first-parent commit against its first parent (against the empty
// tree for the root), in the shape GitHub's diff media type serves, and the merged pull requests.
// It is read once and never changes, so tests may share one.
type Branch struct {
	Tip     string
	Commits []Commit
	// Chain is the first-parent chain from the tip, oldest first.
	Chain   []Commit
	Patches map[string][]byte
	Pulls   []Pull
}

// Branch reads the history's branch main back from git, as the double serves it: every git command
// the double needs runs here. Its pull requests are those that landed a commit the branch reaches.
func (h *History) Branch() *Branch {
	h.t.Helper()
	tip := strings.TrimSpace(h.Repo.Git("rev-parse", BranchName))
	branch := &Branch{Tip: tip, Patches: map[string][]byte{}}
	reached := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(h.Repo.Git("log", "--format=%H %ct %P", tip)), "\n") {
		fields := strings.Fields(line)
		seconds, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			h.t.Fatalf("codesurvivaltest: read the commit time of %q: %v", line, err)
		}
		branch.Commits = append(branch.Commits, Commit{SHA: fields[0], Parents: fields[2:], CommittedAt: time.Unix(seconds, 0).UTC()})
		reached[fields[0]] = true
	}
	for _, pull := range h.pulls {
		if reached[pull.SHA] {
			branch.Pulls = append(branch.Pulls, pull)
		}
	}
	for _, commit := range h.Repo.FirstParents(tip) {
		branch.Chain = append(branch.Chain, Commit{SHA: commit.SHA, Parents: commit.Parents, CommittedAt: commit.CommittedAt})
		branch.Patches[commit.SHA] = h.Repo.Diff(commit.SHA)
	}
	return branch
}

// NewBranch is a branch made of chain, a first-parent chain oldest first whose patches are
// patches by SHA, and the pull requests pulls, for a test that needs patches no git would write in
// its time: large ones, say. Each commit's first parent must be the commit before it.
func NewBranch(chain []Commit, patches map[string][]byte, pulls []Pull) *Branch {
	branch := &Branch{Chain: slices.Clone(chain), Patches: patches, Pulls: slices.Clone(pulls)}
	for index := len(chain) - 1; index >= 0; index-- {
		branch.Commits = append(branch.Commits, chain[index])
	}
	if len(chain) > 0 {
		branch.Tip = chain[len(chain)-1].SHA
	}
	return branch
}

// first is the first parent of sha on the branch, and whether the branch's chain holds sha.
func (b *Branch) first(sha string) (string, bool) {
	for _, commit := range b.Chain {
		if commit.SHA == sha {
			if len(commit.Parents) == 0 {
				return "", true
			}
			return commit.Parents[0], true
		}
	}
	return "", false
}

// Refusal is how the double refuses one commit's patch on both diff routes: Status, a 406, a 422
// or a 5xx whose body is Body; or, with Oversize set, an answer of 40 MiB, past the run's 32 MiB.
// Withheld is how many of the commit's changed files its files[] serve with no patch. A Refusal
// with no Status and no Oversize serves the patch and only withholds files[] entries.
type Refusal struct {
	Status   int
	Body     string
	Oversize bool
	Withheld int
}

// oversizeBytes is the size of an oversize answer: 40 MiB, k8's.
const oversizeBytes = 40 << 20

// Budget is the rate limit the double's answers name (X-RateLimit-Limit, X-RateLimit-Remaining and
// X-RateLimit-Reset).
type Budget struct {
	Limit     int
	Remaining int
	Reset     time.Time
}

// DefaultBudget is the budget a new double serves: an installation's 5,000 requests, all of them
// left, in a window that reset at the Unix epoch. A budget a test serves with any later reset is a
// later window, which a reader takes in its place (codesurvival.Budget), as GitHub's next window
// replaces its last.
var DefaultBudget = Budget{Limit: 5000, Remaining: 5000, Reset: time.Unix(0, 0).UTC()}

// Request is one request the double was sent.
type Request struct {
	Method string
	Path   string
	Query  url.Values
	Accept string
}

// String is the request as a failure names it.
func (r Request) String() string {
	line := r.Method + " " + r.Path
	if len(r.Query) > 0 {
		line += "?" + r.Query.Encode()
	}
	return line + " (Accept: " + r.Accept + ")"
}

// Patch reports whether the request reads a commit's patch through the commit or the compare
// route, and the commit's SHA.
func (r Request) Patch() (sha string, ok bool) {
	if r.Accept != DiffMediaType {
		return "", false
	}
	parts := strings.Split(strings.TrimPrefix(r.Path, "/"), "/")
	switch {
	case len(parts) == 5 && parts[0] == "repos" && parts[3] == "commits":
		return parts[4], true
	case len(parts) == 5 && parts[0] == "repos" && parts[3] == "compare":
		_, head, found := strings.Cut(parts[4], "...")
		return head, found
	}
	return "", false
}

// Files reports whether the request reads a page of a commit's files[], and the commit's SHA.
func (r Request) Files() (sha string, ok bool) {
	parts := strings.Split(strings.TrimPrefix(r.Path, "/"), "/")
	if r.Accept == DiffMediaType || len(parts) != 5 || parts[0] != "repos" || parts[3] != "commits" {
		return "", false
	}
	return parts[4], true
}

// held is a patch answer held back until after other patch answers were written.
type held struct {
	after   int
	release chan struct{}
}

// GitHub is the double: a fake GitHub serving the branches tests give it.
type GitHub struct {
	t   testing.TB
	url string

	mu        sync.Mutex
	branches  map[string]*Branch
	unread    map[string]bool
	off       map[string][]Commit
	refusals  map[string]Refusal
	budget    Budget
	later     *Budget
	laterFrom int
	patches   int
	holds     map[string]*held
	failAll   bool
	requests  []Request
}

// New starts the double; it stops when the test ends.
func New(t *testing.T) *GitHub {
	t.Helper()
	g := &GitHub{t: t, branches: map[string]*Branch{}, unread: map[string]bool{}, off: map[string][]Commit{},
		refusals: map[string]Refusal{}, budget: DefaultBudget, holds: map[string]*held{}}
	g.url = githubtest.Server(t, githubtest.Spec{Record: g.record, Handle: g.serve})
	return g
}

// URL is the double's base URL, which the App client is pointed at as DISPATCH_GITHUB_API_BASE
// points the server's.
func (g *GitHub) URL() string { return g.url }

// Client is an App client of the double, signed with a key made for the test, built as the server
// builds its own (githubapp.New with the base URL).
func (g *GitHub) Client(t testing.TB) *githubapp.Client {
	t.Helper()
	_, encoded := githubapptest.Key(t)
	client, err := githubapp.New(&auth.AppConfig{ClientID: "Iv1.codesurvival", PEM: encoded}, g.url)
	if err != nil || client == nil {
		t.Fatalf("codesurvivaltest: the App client: %v", err)
	}
	return client
}

// Serve serves branch as repo's default branch from now on: a force-push where repo served another.
func (g *GitHub) Serve(repo string, branch *Branch) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.branches[strings.ToLower(repo)] = branch
	delete(g.unread, strings.ToLower(repo))
}

// Unreadable makes the App not installed on repo: its installation lookup answers 404.
func (g *GitHub) Unreadable(repo string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.unread[strings.ToLower(repo)] = true
}

// ListOffBranch lists commit in repo's commits list, a commit no first parent of the branch leads
// to (k7's added commit), as long as the test runs.
func (g *GitHub) ListOffBranch(repo string, commit Commit) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.off[strings.ToLower(repo)] = append(g.off[strings.ToLower(repo)], commit)
}

// Refuse refuses the patch of sha as refusal says, from now on; the zero Refusal serves it whole
// again.
func (g *GitHub) Refuse(sha string, refusal Refusal) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if refusal == (Refusal{}) {
		delete(g.refusals, sha)
		return
	}
	g.refusals[sha] = refusal
}

// SetBudget makes every answer from now on name budget, and drops a budget SetBudgetAfter set.
func (g *GitHub) SetBudget(budget Budget) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.budget, g.later = budget, nil
}

// SetBudgetAfter makes every answer name budget once n more patch reads (a patch through either
// diff route, or a page of files[]) were answered: the budget falling below the floor halfway
// through a run's patches.
func (g *GitHub) SetBudgetAfter(n int, budget Budget) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.later, g.laterFrom = &budget, g.patches+n
}

// Hold holds the patch answer of sha until after other patch answers were written, at least
// one, then writes it: the patch the replay needs next arriving after the patches behind it.
func (g *GitHub) Hold(sha string, after int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.holds[sha] = &held{after: max(after, 1), release: make(chan struct{})}
}

// FailAll makes every request from now on answered 500, and recorded, for a test that proves a step
// sends none (k9's regroup).
func (g *GitHub) FailAll(fail bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.failAll = fail
}

// Requests is every request the double was sent, in order.
func (g *GitHub) Requests() []Request {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.requests)
}

// Forget drops the requests recorded so far, so a test reads one step's requests alone.
func (g *GitHub) Forget() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.requests = nil
}

func (g *GitHub) record(r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.requests = append(g.requests, Request{Method: r.Method, Path: r.URL.Path, Query: r.URL.Query(), Accept: r.Header.Get("Accept")})
}

// serve answers one request: the App's two routes, then each route of a repository.
func (g *GitHub) serve(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	fail := g.failAll
	g.mu.Unlock()
	if fail {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"message":"the double fails every request"}`)
		return
	}
	if r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/app/installations/") && strings.HasSuffix(r.URL.Path, "/access_tokens") {
		w.WriteHeader(http.StatusCreated)
		g.writeJSON(w, map[string]any{"token": "ghs_codesurvival", "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339)})
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if r.Method != http.MethodGet || len(parts) < 3 || parts[0] != "repos" {
		g.notFound(w)
		return
	}
	repo := strings.ToLower(parts[1] + "/" + parts[2])
	rest := parts[3:]
	g.mu.Lock()
	branch, unread := g.branches[repo], g.unread[repo]
	g.mu.Unlock()
	if len(rest) == 1 && rest[0] == "installation" {
		if unread || branch == nil {
			g.notFound(w)
			return
		}
		g.writeJSON(w, map[string]any{"id": 1, "app_slug": "codesurvival", "permissions": map[string]string{"contents": "read", "pull_requests": "read"}})
		return
	}
	if unread || branch == nil {
		g.notFound(w)
		return
	}
	g.headers(w, r)
	switch {
	case len(rest) == 0:
		g.writeJSON(w, map[string]any{"full_name": parts[1] + "/" + parts[2], "default_branch": BranchName})
	case len(rest) == 4 && rest[0] == "git" && rest[1] == "ref" && rest[2] == "heads" && rest[3] == BranchName:
		g.writeJSON(w, map[string]any{"ref": "refs/heads/" + BranchName, "object": map[string]any{"sha": branch.Tip, "type": "commit"}})
	case len(rest) == 1 && rest[0] == "commits":
		g.commits(w, r, repo, branch)
	case len(rest) == 2 && rest[0] == "commits" && r.Header.Get("Accept") == DiffMediaType:
		g.patch(w, rest[1], branch, true)
	case len(rest) == 2 && rest[0] == "commits":
		g.files(w, r, rest[1], branch)
	case len(rest) == 2 && rest[0] == "compare" && r.Header.Get("Accept") == DiffMediaType:
		base, head, found := strings.Cut(rest[1], "...")
		if first, on := branch.first(head); !found || !on || first != base {
			g.notFound(w)
			return
		}
		g.patch(w, head, branch, false)
	case len(rest) == 1 && rest[0] == "pulls":
		g.pulls(w, r, repo, branch)
	case len(rest) == 3 && rest[0] == "pulls" && rest[2] == "commits":
		number, _ := strconv.Atoi(rest[1])
		g.pullCommits(w, r, number, branch)
	default:
		g.notFound(w)
	}
}

// headers writes the budget every repository answer names.
func (g *GitHub) headers(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	budget := g.budget
	if g.later != nil && g.patches >= g.laterFrom {
		budget = *g.later
	}
	g.mu.Unlock()
	w.Header().Set("X-RateLimit-Limit", strconv.Itoa(budget.Limit))
	w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(budget.Remaining))
	w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(budget.Reset.Unix(), 10))
	w.Header().Set("X-RateLimit-Resource", "core")
}

// notFound is GitHub's 404.
func (g *GitHub) notFound(w http.ResponseWriter) {
	w.WriteHeader(http.StatusNotFound)
	fmt.Fprint(w, `{"message":"Not Found"}`)
}

func (g *GitHub) writeJSON(w http.ResponseWriter, v any) {
	if err := json.NewEncoder(w).Encode(v); err != nil {
		g.t.Errorf("codesurvivaltest: encode an answer: %v", err)
	}
}

// page is the page a list request names, 1 where it names none, and false for a page that is no
// number.
func page(r *http.Request) (int, bool) {
	raw := r.URL.Query().Get("page")
	if raw == "" {
		return 1, true
	}
	n, err := strconv.Atoi(raw)
	return n, err == nil && n >= 1
}

// link writes the Link header naming page next of r's list where more remain.
func link(w http.ResponseWriter, r *http.Request, next int, more bool) {
	if !more {
		return
	}
	query := r.URL.Query()
	query.Set("page", strconv.Itoa(next))
	w.Header().Set("Link", `<http://`+r.Host+r.URL.Path+"?"+query.Encode()+`>; rel="next"`)
}

// commits answers the commits list: every commit reachable from the branch's tip, and every commit
// listed off it, dated at or after since, newest first, a page of 100 at a time.
func (g *GitHub) commits(w http.ResponseWriter, r *http.Request, repo string, branch *Branch) {
	if sha := r.URL.Query().Get("sha"); sha != branch.Tip && sha != BranchName {
		g.notFound(w)
		return
	}
	var since time.Time
	if raw := r.URL.Query().Get("since"); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			w.WriteHeader(http.StatusUnprocessableEntity)
			fmt.Fprint(w, `{"message":"since is no time"}`)
			return
		}
		since = parsed
	}
	number, ok := page(r)
	if !ok {
		g.notFound(w)
		return
	}
	g.mu.Lock()
	listed := append(slices.Clone(branch.Commits), g.off[repo]...)
	g.mu.Unlock()
	slices.SortStableFunc(listed, func(a, b Commit) int { return b.CommittedAt.Compare(a.CommittedAt) })
	listed = slices.DeleteFunc(listed, func(commit Commit) bool { return commit.CommittedAt.Before(since) })
	from, to := min((number-1)*perPage, len(listed)), min(number*perPage, len(listed))
	entries := make([]map[string]any, 0, to-from)
	for _, commit := range listed[from:to] {
		parents := make([]map[string]string, len(commit.Parents))
		for index, parent := range commit.Parents {
			parents[index] = map[string]string{"sha": parent}
		}
		entries = append(entries, map[string]any{
			"sha":     commit.SHA,
			"commit":  map[string]any{"committer": map[string]any{"name": "codesurvivaltest", "date": commit.CommittedAt.Format(time.RFC3339)}},
			"parents": parents,
		})
	}
	link(w, r, number+1, to < len(listed))
	g.writeJSON(w, entries)
}

// patch answers the patch of sha through a diff route, or the refusal a test set for it, after the
// answers sha is held behind; viaCommit is the commit route, else the compare route.
func (g *GitHub) patch(w http.ResponseWriter, sha string, branch *Branch, viaCommit bool) {
	body, found := branch.Patches[sha]
	if _, on := branch.first(sha); !found || !on {
		g.notFound(w)
		return
	}
	g.mu.Lock()
	hold := g.holds[sha]
	refusal := g.refusals[sha]
	g.mu.Unlock()
	if hold != nil {
		<-hold.release
	}
	defer g.patchAnswered(sha)
	switch {
	case refusal.Oversize:
		w.Header().Set("Content-Type", DiffMediaType+"; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		chunk := bytes.Repeat([]byte("+an oversize diff line\n"), 1<<14)
		for written := 0; written < oversizeBytes; written += len(chunk) {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
		return
	case refusal.Status != 0:
		w.WriteHeader(refusal.Status)
		fmt.Fprintf(w, `{"message":%q}`, refusal.Body)
		return
	}
	w.Header().Set("Content-Type", DiffMediaType+"; charset=utf-8")
	_, _ = w.Write(body)
}

// patchAnswered counts one patch read answered, and lets go each held answer whose turn came.
func (g *GitHub) patchAnswered(sha string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.patches++
	for heldSHA, hold := range g.holds {
		if heldSHA == sha {
			delete(g.holds, heldSHA)
			continue
		}
		if hold.after--; hold.after == 0 {
			close(hold.release)
		}
	}
}

// files answers a page of sha's files[], from its patch, each file's patch withheld for as many of
// its files as the refusal a test set says.
func (g *GitHub) files(w http.ResponseWriter, r *http.Request, sha string, branch *Branch) {
	body, found := branch.Patches[sha]
	number, ok := page(r)
	if !found || !ok {
		g.notFound(w)
		return
	}
	defer g.patchAnswered(sha)
	g.mu.Lock()
	withheld := g.refusals[sha].Withheld
	g.mu.Unlock()
	entries := filesOf(body)
	for index := range entries {
		if index < withheld {
			entries[index].Patch = nil
		}
	}
	from, to := min((number-1)*perPage, len(entries)), min(number*perPage, len(entries))
	link(w, r, number+1, to < len(entries))
	g.writeJSON(w, map[string]any{"sha": sha, "files": entries[from:to]})
}

// pulls answers the closed pull requests list of the repository: every pull request the branch's
// history landed, most recently merged first (its updated_at is its merged_at), 100 a page. Each
// merged into main, so a list that names another base, as GitHub's base filter does, holds none.
func (g *GitHub) pulls(w http.ResponseWriter, r *http.Request, repo string, branch *Branch) {
	query := r.URL.Query()
	if query.Get("state") != "closed" || (query.Has("base") && query.Get("base") != BranchName) {
		g.writeJSON(w, []any{})
		return
	}
	number, ok := page(r)
	if !ok {
		g.notFound(w)
		return
	}
	pulls := slices.Clone(branch.Pulls)
	slices.SortStableFunc(pulls, func(a, b Pull) int { return b.MergedAt.Compare(a.MergedAt) })
	from, to := min((number-1)*perPage, len(pulls)), min(number*perPage, len(pulls))
	entries := make([]map[string]any, 0, to-from)
	for _, pull := range pulls[from:to] {
		entries = append(entries, map[string]any{
			"number":           pull.Number,
			"html_url":         fmt.Sprintf("https://github.com/%s/pull/%d", repo, pull.Number),
			"state":            "closed",
			"title":            pull.Title,
			"body":             pull.Body,
			"user":             map[string]any{"login": pull.Author},
			"head":             map[string]any{"ref": pull.HeadRef, "repo": map[string]any{"full_name": repo}},
			"base":             map[string]any{"ref": BranchName},
			"merge_commit_sha": pull.SHA,
			"merged_at":        pull.MergedAt.Format(time.RFC3339),
			"updated_at":       pull.MergedAt.Format(time.RFC3339),
		})
	}
	link(w, r, number+1, to < len(pulls))
	g.writeJSON(w, entries)
}

// pullCommits answers the commits of a pull request: one, whose message is its title.
func (g *GitHub) pullCommits(w http.ResponseWriter, r *http.Request, number int, branch *Branch) {
	at := slices.IndexFunc(branch.Pulls, func(pull Pull) bool { return pull.Number == number })
	page, ok := page(r)
	if at < 0 || !ok {
		g.notFound(w)
		return
	}
	if page > 1 {
		g.writeJSON(w, []any{})
		return
	}
	g.writeJSON(w, []map[string]any{{"sha": branch.Pulls[at].SHA, "commit": map[string]any{"message": branch.Pulls[at].Title}}})
}

// filesOf is the files[] entries GitHub serves for a commit whose patch is diff, a diff as git
// writes it: one entry per file, its name, its previous name where it was renamed, its status, its
// patch (the file's hunks, from its first @@ line) and the count of lines it changed.
func filesOf(diff []byte) []survival.FilesEntry {
	var entries []survival.FilesEntry
	for _, section := range bytes.Split(diff, []byte("\ndiff --git ")) {
		section = bytes.TrimPrefix(section, []byte("diff --git "))
		if len(bytes.TrimSpace(section)) == 0 {
			continue
		}
		entry := survival.FilesEntry{Status: survival.StatusModified}
		var oldPath, newPath string
		lines := bytes.Split(section, []byte("\n"))
		hunkAt := -1
		for index, line := range lines {
			text := string(line)
			switch {
			case strings.HasPrefix(text, "@@"):
				hunkAt = index
			case strings.HasPrefix(text, "new file mode"):
				entry.Status = survival.StatusAdded
			case strings.HasPrefix(text, "deleted file mode"):
				entry.Status = survival.StatusRemoved
			case strings.HasPrefix(text, "rename from "):
				entry.Status, oldPath = survival.StatusRenamed, strings.TrimPrefix(text, "rename from ")
			case strings.HasPrefix(text, "rename to "):
				newPath = strings.TrimPrefix(text, "rename to ")
			case strings.HasPrefix(text, "--- a/"):
				oldPath = strings.TrimPrefix(text, "--- a/")
			case strings.HasPrefix(text, "+++ b/"):
				newPath = strings.TrimPrefix(text, "+++ b/")
			}
			if hunkAt >= 0 {
				break
			}
		}
		if newPath == "" {
			newPath = oldPath
		}
		entry.Filename = newPath
		if entry.Status == survival.StatusRenamed {
			entry.PreviousFilename = oldPath
		}
		patch := ""
		if hunkAt >= 0 {
			body := lines[hunkAt:]
			if last := len(body) - 1; last >= 0 && len(body[last]) == 0 {
				body = body[:last]
			}
			patch = string(bytes.Join(body, []byte("\n")))
			for _, line := range body {
				if len(line) > 0 && (line[0] == '+' || line[0] == '-') {
					entry.Changes++
				}
			}
		}
		entry.Patch = &patch
		entries = append(entries, entry)
	}
	return entries
}

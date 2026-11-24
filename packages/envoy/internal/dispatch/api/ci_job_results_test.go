// ci_job_results_test.go holds POST /api/v1/issues/{key}/results to R12 of the spec for a check
// CI proves: the CI job that its ci_check names records the check's result there with its own
// results token, and Dispatch takes who recorded it, the commit and the branch from that token
// and from GitHub, never from the body. A role still records no result for such a check, and the
// job's result never takes the check away from CI.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/githubapptest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/githubtest"
)

const (
	// ciJobProject is the project of the job's results token and of the issue it records on.
	ciJobProject = "ACME"
	// ciJobRepository is the repository of the job's run, the project's default repository.
	ciJobRepository = "example-org/example-repo"
	// ciJobCommit is the commit the job's run tested, the commit of its token.
	ciJobCommit = "0123456789abcdef0123456789abcdef01234567"
	// ciJobOtherCommit is a commit the token does not name.
	ciJobOtherCommit = "89abcdef0123456789abcdef0123456789abcdef"
	// ciJobHead is the head of the issue's open pull request, which the job did not test.
	ciJobHead = "fedcba9876543210fedcba9876543210fedcba98"
	// ciJobCheckRun is the check run of the job the default token was minted in.
	ciJobCheckRun = 97
	// ciJobName is the name of that check run, and the ci_check of the check c1.
	ciJobName = "go-checks"
	// ciJobRef is the branch push the job ran on, and ciJobBranch that branch.
	ciJobRef    = "refs/heads/legion/ACME-1"
	ciJobBranch = "legion/ACME-1"
	// ciJobNegativeControl is the negative control both checks name.
	ciJobNegativeControl = "A build whose server does not build."
)

// ciJobVerification is the Verification document of the job's issue: c1, a check CI proves whose
// ci_check names the job, and k1, an ordinary check.
var ciJobVerification = ciCheckBlock("c1", `negative_control="`+ciJobNegativeControl+`" ci_check="`+ciJobName+`"`) +
	"\n" + ciCheckBlock("k1", `negative_control="`+ciJobNegativeControl+`"`)

// ciJobSession is the session the job's results token proves (resultsOnlyToken).
var ciJobSession = resultsOnlyToken("", ciJobProject).Session

// ciJobGitHub is the GitHub the route reads for a job's result: the installation of every
// repository, the check runs of a job by id, the commits a repository holds (every one), the
// check runs listed at a pull request head, which the derived result of the read is made from,
// and the head of each branch the results read compares a before_merge check with.
type ciJobGitHub struct {
	mu        sync.Mutex
	checkRuns map[int64]specCheckRunAnswer
	listings  *checkRunsFake
	// branches is what GitHub answers for the head of each branch, by name. A branch it holds
	// no answer for is 404, a branch the repository does not hold.
	branches map[string]ciJobBranchHead
	// branchReads is how many times GitHub was asked for each branch's head.
	branchReads map[string]int
	// hidden has GitHub answer 404 for the repository itself, as it does for a repository the
	// App cannot see.
	hidden bool
	// commitRead, when set, is handed each commit GitHub is asked whether the repository holds,
	// with the asking request's context, before GitHub answers, and holds that request for as
	// long as it runs (holdCommitReads).
	commitRead func(ctx context.Context, sha string)
	// pullCommits is the commits GitHub lists for each pull request, by number, a page of per_page
	// at a time and at most pullCommitsListed of them, as GitHub lists them; a pull request it
	// holds none for is 404. A read of a pull request's commits is a read of that list, or a read
	// of the comparison of the base revision and the head GitHub answers for it (pulls), which
	// GitHub answers with the same commits, and every page in full. pullCommitsStatus, when set,
	// answers every such read with that status instead, and pullCommitReads counts the reads of
	// each pull request's commits, a page each. pullCommitsHold, when set for a number, holds the
	// next read of that pull request's commits until it is released (holdPullCommitsOnce).
	// pullCommitsAnswered queues, for a number, holds of the next reads of its commits, one for each
	// read in turn, that each read its answer when it reaches GitHub and send it only once released
	// (holdPullCommitsAnswer), as GitHub's answer is sent while the reader is held after it.
	// pullCommitsPageFails answers the next read of a page of any pull request's commits with that
	// status once (failPullCommitsPageOnce).
	pullCommits          map[int][]string
	pullCommitsStatus    int
	pullCommitReads      map[int]int
	pullCommitsHold      map[int]*ciJobHeldRead
	pullCommitsAnswered  map[int][]*ciJobHeldRead
	pullCommitsPageFails map[int]int
	// pulls is GitHub's answer for each pull request itself, by number: its head commit, its base
	// branch and the commit that branch is at. A pull request it holds none for stands at the last
	// commit pullCommits lists for it, onto main at ciJobBaseSHA, and one with neither is 404.
	// pullReads counts those reads.
	pulls     map[int]ciJobPull
	pullReads map[int]int
	// ranges is GitHub's comparison of two commits, by "base...head": the commits head holds and
	// base does not (git log base..head), oldest first, whatever any pull request stands at now,
	// since a comparison names its two commits (setRange). A pull request whose base revision and
	// head pulls names are a pair ranges holds lists that pair's commits.
	ranges map[string][]string
	// compares is GitHub's answer for the comparison of two commits or branches, by
	// "base...head": a status of the comparison, or an HTTP status that fails it. A comparison
	// that is no range above and no pull request's commits reads diverged. compareReads counts the
	// reads of each comparison, by the same key, a page each.
	compares     map[string]ciJobCompare
	compareReads map[string]int
	// beforeRead, when set, is handed the kind of each read of a pull request ("pull"), of a page
	// of its commits ("commits") and of a comparison ("compare") before GitHub answers it, so a
	// test can move a pull request on between two reads (onRead).
	beforeRead func(kind string)
	// absentCommits is the commits the repository does not hold: GitHub answers a read of one 422
	// No commit found for SHA. Every other commit is held.
	absentCommits map[string]bool
}

// pullCommitsListed is the most commits GitHub lists for one pull request, however many it has.
const pullCommitsListed = 250

// ciJobPull is GitHub's answer for one pull request: its head commit, its base branch, the commit
// the base branch is at (base.sha) and its title.
type ciJobPull struct {
	head, base, baseSHA, title string
}

// ciJobBaseSHA is the commit the base branch of a pull request the fake GitHub answers is at,
// where a test names no other.
const ciJobBaseSHA = "b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0"

// ciJobCompare is GitHub's answer for one comparison: its status (behind, identical, ahead,
// diverged), or failed, the HTTP status of a read that failed.
type ciJobCompare struct {
	status string
	failed int
}

// ciJobHeldRead is one read held: entered closes once the read reached GitHub, and the read
// answers once release closes.
type ciJobHeldRead struct {
	entered, release chan struct{}
}

// setPull has GitHub answer pull request number at head, based on base, whose head is baseSHA.
func (g *ciJobGitHub) setPull(number int, head, base, baseSHA string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pulls[number] = ciJobPull{head: head, base: base, baseSHA: baseSHA}
}

// setPullTitle has GitHub answer title as pull request number's title, where it stands now.
func (g *ciJobGitHub) setPullTitle(number int, title string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	pull, _ := g.pullOf(number)
	pull.title = title
	g.pulls[number] = pull
}

// setCompare has GitHub answer answer for the comparison of base and head.
func (g *ciJobGitHub) setCompare(base, head string, answer ciJobCompare) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.compares[base+"..."+head] = answer
}

// setAbsentCommit has the repository hold sha no longer.
func (g *ciJobGitHub) setAbsentCommit(sha string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.absentCommits[sha] = true
}

// holdPullCommitsOnce holds the next read of pull request number's commits until release is
// called, and answers entered, which closes once that read reached GitHub. Reads after it answer
// at once. The test's end releases it.
func (g *ciJobGitHub) holdPullCommitsOnce(t *testing.T, number int) (entered <-chan struct{}, release func()) {
	t.Helper()
	held := &ciJobHeldRead{entered: make(chan struct{}), release: make(chan struct{})}
	once := sync.OnceFunc(func() { close(held.release) })
	t.Cleanup(once)
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pullCommitsHold[number] = held
	return held.entered, once
}

// holdPullCommitsAnswer queues a hold of the next read of pull request number's commits that no
// earlier hold takes: that read takes GitHub's answer as GitHub lists the commits when the read
// arrives, closes entered, and sends that answer once release is called. Each call queues one
// hold, for the read after the one the hold before it takes. The test's end releases it.
func (g *ciJobGitHub) holdPullCommitsAnswer(t *testing.T, number int) (entered <-chan struct{}, release func()) {
	t.Helper()
	held := &ciJobHeldRead{entered: make(chan struct{}), release: make(chan struct{})}
	once := sync.OnceFunc(func() { close(held.release) })
	t.Cleanup(once)
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pullCommitsAnswered[number] = append(g.pullCommitsAnswered[number], held)
	return held.entered, once
}

// failPullCommitsPageOnce answers the next read of page of any pull request's commits with
// status, once; the read after it answers as GitHub lists the commits.
func (g *ciJobGitHub) failPullCommitsPageOnce(page, status int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pullCommitsPageFails[page] = status
}

// setRange has GitHub answer shas, oldest first, as its comparison of base and head: the commits
// head holds and base does not.
func (g *ciJobGitHub) setRange(base, head string, shas ...string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.ranges[base+"..."+head] = shas
}

// onRead has GitHub hand read the kind of each read of a pull request, of a page of its commits
// and of a comparison before it answers it (beforeRead), and nil ends it.
func (g *ciJobGitHub) onRead(read func(kind string)) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.beforeRead = read
}

// announce hands kind to beforeRead, outside the lock, so the read can change what GitHub answers.
func (g *ciJobGitHub) announce(kind string) {
	g.mu.Lock()
	read := g.beforeRead
	g.mu.Unlock()
	if read != nil {
		read(kind)
	}
}

// pullOf is GitHub's answer for pull request number itself (pulls), and whether it holds one: a
// pull request pulls names none of stands at the last commit pullCommits lists for it, onto main
// at ciJobBaseSHA. The caller holds g.mu.
func (g *ciJobGitHub) pullOf(number int) (ciJobPull, bool) {
	if pull, held := g.pulls[number]; held {
		return pull, true
	}
	if listed := g.pullCommits[number]; len(listed) > 0 {
		return ciJobPull{head: listed[len(listed)-1], base: "main", baseSHA: ciJobBaseSHA}, true
	}
	return ciJobPull{}, false
}

// commitsOf is the commits GitHub lists for pull request number now, and whether it lists any: the
// range of the base revision and the head pulls names for it, where ranges holds that pair, and
// else pullCommits. The caller holds g.mu.
func (g *ciJobGitHub) commitsOf(number int) ([]string, bool) {
	if pull, held := g.pulls[number]; held {
		if shas, ranged := g.ranges[pull.baseSHA+"..."+pull.head]; ranged {
			return shas, true
		}
	}
	shas, listed := g.pullCommits[number]
	return shas, listed
}

// rangeOf is GitHub's comparison of the two commits of basehead, "base...head", and whether it is
// one that lists commits: a range setRange set, or the base revision and the head of a pull request
// whose commits GitHub lists, which it answers with those commits. number is that pull request's,
// where one stands at the pair, and 0 for none. The caller holds g.mu.
func (g *ciJobGitHub) rangeOf(basehead string) (shas []string, number int, held bool) {
	for at := range g.pulls {
		if pull, _ := g.pullOf(at); pull.baseSHA+"..."+pull.head == basehead {
			number = at
		}
	}
	for at := range g.pullCommits {
		if pull, _ := g.pullOf(at); number == 0 && pull.baseSHA+"..."+pull.head == basehead {
			number = at
		}
	}
	if shas, ranged := g.ranges[basehead]; ranged {
		return shas, number, true
	}
	if number != 0 {
		if shas, listed := g.commitsOf(number); listed {
			return shas, number, true
		}
	}
	return nil, 0, false
}

// commitsPage is GitHub's answer, its status and body, for the read r of a page of commits as
// GitHub holds them now: of pull request number's list where basehead is "", at most
// pullCommitsListed of them, and else of the comparison basehead (rangeOf), every one of them, with
// the comparison's status and total_commits. failPullCommitsPageOnce and failPullCommits fail
// either read.
func (g *ciJobGitHub) commitsPage(number int, basehead string, r *http.Request) (int, []byte) {
	g.mu.Lock()
	defer g.mu.Unlock()
	page, perPage := 1, 30
	fmt.Sscan(r.URL.Query().Get("page"), &page)
	fmt.Sscan(r.URL.Query().Get("per_page"), &perPage)
	if status, failing := g.pullCommitsPageFails[page]; failing {
		delete(g.pullCommitsPageFails, page)
		return status, []byte(`{"message":"Server Error"}`)
	}
	if g.pullCommitsStatus != 0 {
		return g.pullCommitsStatus, []byte(`{"message":"Server Error"}`)
	}
	var shas []string
	var listed bool
	if basehead == "" {
		shas, listed = g.commitsOf(number)
		shas = shas[:min(len(shas), pullCommitsListed)]
	} else {
		shas, _, listed = g.rangeOf(basehead)
	}
	if !listed {
		return http.StatusNotFound, []byte(`{"message":"Not Found"}`)
	}
	from := min(max(page-1, 0)*perPage, len(shas))
	commits := make([]map[string]any, 0, perPage)
	for _, sha := range shas[from:min(from+perPage, len(shas))] {
		commits = append(commits, map[string]any{"sha": sha, "commit": map[string]any{"message": "A commit"}})
	}
	if basehead == "" {
		body, _ := json.Marshal(commits)
		return http.StatusOK, body
	}
	base, head, _ := strings.Cut(basehead, "...")
	status := "ahead"
	switch {
	case base == head:
		status = "identical"
	case len(shas) == 0:
		status = "behind"
	}
	body, _ := json.Marshal(map[string]any{"status": status, "ahead_by": len(shas), "behind_by": 0, "total_commits": len(shas), "commits": commits})
	return http.StatusOK, body
}

// serveCommits answers r, a read of a page of pull request number's commits, by its list
// (basehead "") or by the comparison basehead: it counts the read, holds it as the holds of number
// say, and sends what commitsPage answers when the read arrives.
func (g *ciJobGitHub) serveCommits(w http.ResponseWriter, r *http.Request, number int, basehead string) {
	g.mu.Lock()
	var held, answered *ciJobHeldRead
	if number != 0 {
		g.pullCommitReads[number]++
		held = g.pullCommitsHold[number]
		delete(g.pullCommitsHold, number)
		if queue := g.pullCommitsAnswered[number]; len(queue) > 0 {
			answered, g.pullCommitsAnswered[number] = queue[0], queue[1:]
		}
	}
	g.mu.Unlock()
	if held != nil {
		close(held.entered)
		select {
		case <-held.release:
		case <-r.Context().Done():
			return
		}
	}
	status, body := g.commitsPage(number, basehead, r)
	if answered != nil {
		close(answered.entered)
		select {
		case <-answered.release:
		case <-r.Context().Done():
			return
		}
	}
	if status != http.StatusOK {
		w.WriteHeader(status)
	}
	_, _ = w.Write(body)
}

// pullReadsOf is how many times GitHub was asked for pull request number itself.
func (g *ciJobGitHub) pullReadsOf(number int) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.pullReads[number]
}

// compareReadsOf is how many times GitHub was asked for the comparison of base and head.
func (g *ciJobGitHub) compareReadsOf(base, head string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.compareReads[base+"..."+head]
}

// setPullCommits has GitHub list shas as the commits of pull request number.
func (g *ciJobGitHub) setPullCommits(number int, shas ...string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pullCommits[number] = shas
}

// failPullCommits has GitHub answer every read of a pull request's commits with status, and 0
// ends it.
func (g *ciJobGitHub) failPullCommits(status int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pullCommitsStatus = status
}

// pullCommitReadsOf is how many times GitHub was asked for the commits of pull request number.
func (g *ciJobGitHub) pullCommitReadsOf(number int) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.pullCommitReads[number]
}

// ciJobBranchHead is GitHub's answer for one branch's head: the commit it is at, or, where
// status is set, that status and no commit.
type ciJobBranchHead struct {
	sha    string
	status int
}

// setCheckRun has GitHub name check run id name, at commit headSHA.
func (g *ciJobGitHub) setCheckRun(id int64, name, headSHA string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.checkRuns[id] = specCheckRunAnswer{name: name, headSHA: headSHA}
}

// setBranch has GitHub answer head for the head of branch.
func (g *ciJobGitHub) setBranch(branch string, head ciJobBranchHead) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.branches[branch] = head
}

// deleteBranch has GitHub hold branch no longer, so a read of its head is 404.
func (g *ciJobGitHub) deleteBranch(branch string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.branches, branch)
}

// hideRepository has GitHub answer 404 for the repository, as for one the App cannot see.
func (g *ciJobGitHub) hideRepository() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.hidden = true
}

// branchReadsOf is how many times GitHub was asked for the head of branch.
func (g *ciJobGitHub) branchReadsOf(branch string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.branchReads[branch]
}

// holdCommitReads has GitHub hand read each commit it is asked whether the repository holds,
// before it answers.
func (g *ciJobGitHub) holdCommitReads(read func(ctx context.Context, sha string)) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.commitRead = read
}

func (g *ciJobGitHub) serve(t *testing.T) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/installation", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"id":77,"app_slug":"dispatch-test","permissions":{"contents":"read","pull_requests":"read","checks":"read"}}`)
	})
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"token":"ghs_fake_77","expires_at":"2099-01-01T00:00:00Z"}`)
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}", func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		if g.hidden {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"Not Found"}`)
			return
		}
		fmt.Fprintf(w, `{"full_name":%q,"default_branch":"main"}`, r.PathValue("owner")+"/"+r.PathValue("repo"))
	})
	// The exact read of a branch's head (githubapp.Client.Branch), its name path-escaped so a
	// branch with a / in it is one segment.
	mux.HandleFunc("GET /repos/{owner}/{repo}/git/ref/heads/{branch}", func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		branch := r.PathValue("branch")
		g.branchReads[branch]++
		head, held := g.branches[branch]
		switch {
		case !held:
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"Not Found"}`)
		case head.status != 0:
			w.WriteHeader(head.status)
			fmt.Fprint(w, `{"message":"Server Error"}`)
		default:
			fmt.Fprintf(w, `{"ref":%q,"object":{"sha":%q,"type":"commit"}}`, "refs/heads/"+branch, head.sha)
		}
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/commits/{sha}", func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		read := g.commitRead
		absent := g.absentCommits[r.PathValue("sha")]
		g.mu.Unlock()
		if read != nil {
			read(r.Context(), r.PathValue("sha"))
		}
		if absent {
			w.WriteHeader(http.StatusUnprocessableEntity)
			fmt.Fprintf(w, `{"message":"No commit found for SHA: %s"}`, r.PathValue("sha"))
			return
		}
		fmt.Fprintf(w, `{"sha":%q}`, r.PathValue("sha"))
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/commits/{sha}/pulls", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `[]`)
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/pulls/{number}", func(w http.ResponseWriter, r *http.Request) {
		g.announce("pull")
		g.mu.Lock()
		defer g.mu.Unlock()
		var number int
		fmt.Sscan(r.PathValue("number"), &number)
		g.pullReads[number]++
		pull, held := g.pullOf(number)
		if !held {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"Not Found"}`)
			return
		}
		repo := r.PathValue("owner") + "/" + r.PathValue("repo")
		body, _ := json.Marshal(map[string]any{
			"number": number, "state": "open", "title": pull.title,
			"html_url": fmt.Sprintf("https://github.com/%s/pull/%d", repo, number),
			"head":     map[string]any{"sha": pull.head, "ref": "legion/work", "repo": map[string]any{"full_name": repo}},
			"base":     map[string]any{"ref": pull.base, "sha": pull.baseSHA},
		})
		_, _ = w.Write(body)
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/pulls/{number}/commits", func(w http.ResponseWriter, r *http.Request) {
		g.announce("commits")
		var number int
		fmt.Sscan(r.PathValue("number"), &number)
		g.serveCommits(w, r, number, "")
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/compare/{basehead}", func(w http.ResponseWriter, r *http.Request) {
		g.announce("compare")
		basehead := r.PathValue("basehead")
		g.mu.Lock()
		g.compareReads[basehead]++
		answer, held := g.compares[basehead]
		_, number, ranged := g.rangeOf(basehead)
		g.mu.Unlock()
		switch {
		case held && answer.failed != 0:
			w.WriteHeader(answer.failed)
			fmt.Fprint(w, `{"message":"Not Found"}`)
		case held:
			fmt.Fprintf(w, `{"status":%q}`, answer.status)
		case ranged:
			g.serveCommits(w, r, number, basehead)
		default:
			fmt.Fprint(w, `{"status":"diverged"}`)
		}
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/commits/{sha}/check-runs", g.listings.serve)
	mux.HandleFunc("GET /repos/{owner}/{repo}/check-runs/{id}", func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		var id int64
		fmt.Sscan(r.PathValue("id"), &id)
		run, ok := g.checkRuns[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"Not Found"}`)
			return
		}
		fmt.Fprintf(w, `{"id":%d,"name":%q,"head_sha":%q,"status":"in_progress"}`, id, run.name, run.headSHA)
	})
	return githubtest.Server(t, githubtest.Spec{Handle: mux.ServeHTTP})
}

// ciJobRig is a Dispatch whose verifier serves results tokens as cmd/dispatch's does, a GitHub
// that holds the job's check run, and issue ACME-1 of project ACME, whose repository is the run's
// and whose Verification document, verification, is ciJobVerification.
type ciJobRig struct {
	*brokerTokenRig
	github       *ciJobGitHub
	caller       *brokerCaller
	tokens       int
	issue        string
	verification string
}

func newCIJobRig(t *testing.T) *ciJobRig {
	t.Helper()
	return newCIJobRigSettling(t, 0)
}

// newCIJobRigSettling is newCIJobRig whose document service settles after settle (0 is the test
// server's default). A test that reads who holds the issue's owner lock passes a delay that cannot
// fire before it finishes, since the settlement of the uploaded Verification document holds that
// lock for a moment of its own.
func newCIJobRigSettling(t *testing.T, settle time.Duration) *ciJobRig {
	t.Helper()
	github := &ciJobGitHub{
		checkRuns:            map[int64]specCheckRunAnswer{ciJobCheckRun: {name: ciJobName, headSHA: ciJobCommit}},
		listings:             newCheckRunsFake(),
		branches:             map[string]ciJobBranchHead{},
		branchReads:          map[string]int{},
		pullCommits:          map[int][]string{},
		pullCommitReads:      map[int]int{},
		pullCommitsHold:      map[int]*ciJobHeldRead{},
		pullCommitsAnswered:  map[int][]*ciJobHeldRead{},
		pullCommitsPageFails: map[int]int{},
		pulls:                map[int]ciJobPull{},
		pullReads:            map[int]int{},
		ranges:               map[string][]string{},
		compares:             map[string]ciJobCompare{},
		compareReads:         map[string]int{},
		absentCommits:        map[string]bool{},
	}
	_, pemText := githubapptest.Key(t)
	rig := &ciJobRig{
		brokerTokenRig: newBrokerTokenRigOn(t, testServerOptions{
			app:           &auth.AppConfig{ClientID: "Iv1.test", PEM: pemText},
			githubAPIBase: github.serve(t),
			settle:        settle,
		}),
		github: github,
		caller: newBrokerCaller(t),
	}
	rig.issue, rig.verification = rig.issueOf(t, ciJobProject, ciJobRepository)
	return rig
}

// issueOf creates project, makes repository its default repository, and files one issue of it
// whose Verification document is ciJobVerification. It answers the issue and that document.
func (r *ciJobRig) issueOf(t *testing.T, project, repository string) (string, string) {
	t.Helper()
	if created := r.asPerson(t, http.MethodPost, "/api/v1/projects", map[string]any{"key": project, "name": project}); created.Code != http.StatusCreated {
		t.Fatalf("create project %s: status=%d body=%s", project, created.Code, created.Body.String())
	}
	if defaulted := r.asPerson(t, http.MethodPut, "/api/v1/projects/"+project+"/default-repository", map[string]any{"repo": repository}); defaulted.Code != http.StatusOK {
		t.Fatalf("make %s the default repository of %s: status=%d body=%s", repository, project, defaulted.Code, defaulted.Body.String())
	}
	filed := r.asPerson(t, http.MethodPost, "/api/v1/issues", map[string]any{"project": project, "title": "Proved by a CI job", "spec": "A spec\n"})
	if filed.Code != http.StatusCreated {
		t.Fatalf("create an issue of %s: status=%d body=%s", project, filed.Code, filed.Body.String())
	}
	key := decodeBody[model.Issue](t, filed).Key
	return key, uploadDocument(t, r.handler, "/api/v1/issues/"+key, "verification.md", ciJobVerification).Artifact.ID
}

// jobToken has the broker answer a results token of project ACME for the job's run on a push of
// ciJobRef, with change applied to the answer, and answers the token.
func (r *ciJobRig) jobToken(change func(*ciphertoken.Introspection)) string {
	answer := resultsOnlyToken(r.caller.jkt, ciJobProject)
	answer.CI.Ref = ciJobRef
	answer.CI.WorkflowRef = ciJobRepository + "/.github/workflows/ci-results.yaml@" + ciJobRef
	if change != nil {
		change(&answer)
	}
	return r.says(answer)
}

// sessionToken has the broker answer a session's own token of project ACME, and answers it.
func (r *ciJobRig) sessionToken() string {
	answer := liveNamedToken(r.caller.jkt)
	answer.Projects = []string{ciJobProject}
	return r.says(answer)
}

func (r *ciJobRig) says(answer ciphertoken.Introspection) string {
	r.tokens++
	token := fmt.Sprintf("a-token-of-the-ci-job-rig-%d", r.tokens)
	r.broker.Says(token, answer)
	return token
}

// post records body on issue's results route under token.
func (r *ciJobRig) post(t *testing.T, issue, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	return r.serve(r.caller.presenting(t, http.MethodPost, "/api/v1/issues/"+issue+"/results", token, body))
}

// jobResultOf is the body a job writes for check: its verdict at the commit it tested.
func jobResultOf(check, verdict string) map[string]any {
	return map[string]any{"check": check, "commit": ciJobCommit, "verdict": verdict, "observed": "ok"}
}

// stored is how many results the two tables hold: check_results, a role's, and
// ci_check_results, a CI job's.
func (r *ciJobRig) stored(t *testing.T) int {
	t.Helper()
	var count int
	if err := r.database.Pool.QueryRow(context.Background(),
		`select (select count(*) from check_results) + (select count(*) from ci_check_results)`).Scan(&count); err != nil {
		t.Fatalf("count the stored results: %v", err)
	}
	return count
}

// refusedWith fails the test unless response is status with code, and no result and no
// result.recorded event were stored. It answers the refusal's text.
func (r *ciJobRig) refusedWith(t *testing.T, what string, response *httptest.ResponseRecorder, status int, code string) string {
	t.Helper()
	got, text := refusalOf(t, response)
	if response.Code != status || got != code {
		t.Fatalf("%s: status=%d body=%s, want %d %s", what, response.Code, response.Body.String(), status, code)
	}
	if held := r.stored(t); held != 0 {
		t.Fatalf("%s left %d results, want none", what, held)
	}
	if events := resultEventsOn(t, r.database, r.issue); events != 0 {
		t.Fatalf("%s left %d result.recorded events on %s, want none", what, events, r.issue)
	}
	return text
}

// jobResult is one result as the wire carries it, read apart from the server's own type, so
// each test reads the names a client reads.
type jobResult struct {
	ID              string      `json:"id"`
	IssueKey        string      `json:"issue_key"`
	CheckIssueKey   string      `json:"check_issue_key"`
	CheckBlockID    string      `json:"check_block_id"`
	Role            string      `json:"role"`
	Run             string      `json:"run"`
	Surface         string      `json:"surface"`
	NegativeControl string      `json:"negative_control"`
	Branch          string      `json:"branch"`
	Session         string      `json:"session"`
	Actor           model.Actor `json:"actor"`
	CommitSHA       string      `json:"commit_sha"`
	Verdict         string      `json:"verdict"`
	Observed        string      `json:"observed"`
	CreatedAt       time.Time   `json:"created_at"`
}

// jobRecorded decodes a 201 answer of the record route, failing the test on any other.
func jobRecorded(t *testing.T, what string, response *httptest.ResponseRecorder) jobResult {
	t.Helper()
	if response.Code != http.StatusCreated {
		t.Fatalf("%s: status=%d body=%s, want 201", what, response.Code, response.Body.String())
	}
	return decodeBody[struct {
		Result jobResult `json:"result"`
	}](t, response).Result
}

// jobCheckRead is one check of the results read as the wire carries it, with the results of
// recorded_results, where a check CI proves holds what was recorded against it, after its own.
type jobCheckRead struct {
	BlockID         string         `json:"block_id"`
	CICheck         string         `json:"ci_check"`
	CI              *model.CheckCI `json:"ci"`
	Results         []jobResult    `json:"results"`
	RecordedResults []jobResult    `json:"recorded_results"`
}

// readJobCheck is check blockID of the issue's results read, as a person reads it.
func (r *ciJobRig) readJobCheck(t *testing.T, blockID string) jobCheckRead {
	t.Helper()
	response := dispatchRequest(t, r.handler, http.MethodGet, "/api/v1/issues/"+r.issue+"/results", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read the results of %s: status=%d body=%s", r.issue, response.Code, response.Body.String())
	}
	for _, check := range decodeBody[struct {
		Checks []jobCheckRead `json:"checks"`
	}](t, response).Checks {
		if check.BlockID == blockID {
			check.Results = append(check.Results, check.RecordedResults...)
			return check
		}
	}
	t.Fatalf("the results of %s hold no check %s", r.issue, blockID)
	return jobCheckRead{}
}

// TestTheJobACheckNamesRecordsItsResult: the job that c1's ci_check names records c1's result
// with its own token. The row names role ci and the job's check run as its run, the branch the
// job ran on, the session the token proved and the commit of the token; the body named none of
// them. The actor is the run's session and names no person: the person the broker's result entry
// names ran nothing, so the answer, the stored row and the event carry no person (R12). One
// result.recorded event goes on the issue.
func TestTheJobACheckNamesRecordsItsResult(t *testing.T) {
	rig := newCIJobRig(t)
	response := rig.post(t, rig.issue, rig.jobToken(nil), jobResultOf("c1", "pass"))
	result := jobRecorded(t, "the job's result for c1", response)

	want := jobResult{
		ID: result.ID, IssueKey: rig.issue, CheckIssueKey: rig.issue, CheckBlockID: "c1",
		Role: "ci", Run: ciJobName, Surface: "", NegativeControl: ciJobNegativeControl,
		Branch: ciJobBranch, Session: ciJobSession, CommitSHA: ciJobCommit, Verdict: "pass", Observed: "ok",
	}
	got := result
	got.Actor, got.CreatedAt = model.Actor{}, time.Time{}
	if got != want {
		t.Fatalf("the stored result = %+v, want %+v", got, want)
	}
	if result.Actor.Kind != "session" || result.Actor.ID != ciJobSession || result.Actor.Owner != nil {
		t.Fatalf("the result's actor = %+v, want session %s naming no person", result.Actor, ciJobSession)
	}
	if strings.Contains(response.Body.String(), "ada@example.com") {
		t.Fatalf("the answer names the result entry's person ada@example.com: %s", response.Body.String())
	}
	var rowActor, eventActor string
	if err := rig.database.Pool.QueryRow(context.Background(), `
		select r.actor::text, e.actor::text from ci_check_results r join events e on e.seq = r.event_seq and e.issue_key = r.check_issue_key
		where r.id = $1::uuid
	`, result.ID).Scan(&rowActor, &eventActor); err != nil {
		t.Fatalf("read the stored row's actor and its event's: %v", err)
	}
	if strings.Contains(rowActor, "ada@example.com") || strings.Contains(eventActor, "ada@example.com") {
		t.Fatalf("the stored row's actor %s or its event's actor %s names the result entry's person ada@example.com", rowActor, eventActor)
	}

	var run, branch, session, commit string
	var roleRows int
	if err := rig.database.Pool.QueryRow(context.Background(), `
		select run, branch, session, commit_sha, (select count(*) from check_results)
		from ci_check_results where id = $1::uuid
	`, result.ID).Scan(&run, &branch, &session, &commit, &roleRows); err != nil {
		t.Fatalf("read the stored row: %v", err)
	}
	if run != ciJobName || branch != ciJobBranch || session != ciJobSession || commit != ciJobCommit || roleRows != 0 {
		t.Fatalf("the row of ci_check_results holds run %q, branch %q, session %q, commit %s, and check_results %d rows; want %s, %s, %s, %s and none",
			run, branch, session, commit, roleRows, ciJobName, ciJobBranch, ciJobSession, ciJobCommit)
	}
	if events := resultEventsOn(t, rig.database, rig.issue); events != 1 {
		t.Fatalf("%s holds %d result.recorded events, want one", rig.issue, events)
	}
}

// TestAJobsObservedTextIsCutByTheProjectRoutesRule: a CI job's result on the issue route keeps
// what its job observed by the one rule of both results routes (cutObserved). A text of
// contracts.SpecCheckObservedMax runes is kept whole; a failing test's log of 10,000 runes keeps
// its first contracts.SpecCheckObservedStart runes, contracts.SpecCheckObservedCutNote and its
// last contracts.SpecCheckObservedEnd runes, so its first line and its --- FAIL line stay. The
// answer, the stored row, the read and the statement of the run all hold the kept text. The text
// of the bound is the control: a route that cuts every text, or none, fails one of the two rows.
func TestAJobsObservedTextIsCutByTheProjectRoutesRule(t *testing.T) {
	const first, last = "=== RUN   TestX\n", "--- FAIL: TestX (0.01s)\n"
	log := first + strings.Repeat("x\n", (10000-len(first)-len(last))/2) + last
	if utf8.RuneCountInString(log) != 10000 {
		t.Fatalf("the log holds %d runes, want 10000", utf8.RuneCountInString(log))
	}
	bound := strings.Repeat("é", contracts.SpecCheckObservedMax)
	rig := newCIJobRig(t)
	for _, row := range []struct{ name, sent, kept string }{
		{"a text of the bound", bound, bound},
		{"a failing test's log of 10,000 runes", log,
			log[:contracts.SpecCheckObservedStart] + contracts.SpecCheckObservedCutNote + log[len(log)-contracts.SpecCheckObservedEnd:]},
	} {
		t.Run(row.name, func(t *testing.T) {
			body := jobResultOf("c1", "fail")
			body["observed"] = row.sent
			response := rig.post(t, rig.issue, rig.jobToken(nil), body)
			result := jobRecorded(t, "the job's result for c1", response)
			statement := decodeBody[struct {
				Result struct {
					Attestation *struct {
						Predicate struct {
							Observed string `json:"observed"`
						} `json:"predicate"`
					} `json:"attestation"`
				} `json:"result"`
			}](t, response).Result.Attestation
			if statement == nil {
				t.Fatalf("the job's result for c1 carries no attestation: %s", response.Body.String())
			}
			var stored string
			if err := rig.database.Pool.QueryRow(context.Background(),
				`select observed from ci_check_results where id = $1::uuid`, result.ID).Scan(&stored); err != nil {
				t.Fatalf("read the stored row's observed: %v", err)
			}
			read := ""
			for _, held := range rig.readJobCheck(t, "c1").Results {
				if held.ID == result.ID {
					read = held.Observed
				}
			}
			for _, got := range []struct{ where, text string }{
				{"the answer", result.Observed}, {"the stored row", stored}, {"the read", read}, {"the statement", statement.Predicate.Observed},
			} {
				if got.text != row.kept || utf8.RuneCountInString(got.text) != contracts.SpecCheckObservedMax {
					t.Fatalf("%s's observed holds %d runes, starting %q and ending %q; want %d, starting %q and ending %q",
						got.where, utf8.RuneCountInString(got.text), got.text[:min(len(got.text), 40)], got.text[max(0, len(got.text)-160):],
						contracts.SpecCheckObservedMax, row.kept[:min(len(row.kept), 40)], row.kept[max(0, len(row.kept)-160):])
				}
			}
		})
	}
}

// TestARoleStillCannotRecordACheckOfACIJob: a session's tester and implementer results against
// c1 are refused, 409 CHECK_PROVED_BY_CI, saying that only a run of the job records its result.
func TestARoleStillCannotRecordACheckOfACIJob(t *testing.T) {
	rig := newCIJobRig(t)
	want := `check c1 names CI job "` + ciJobName + `": only a run of that job records its result, through the secrets broker`
	for _, role := range []string{"tester", "implementer"} {
		text := rig.refusedWith(t, "the "+role+"'s result for c1",
			rig.post(t, rig.issue, rig.sessionToken(), resultBody("c1", "pass", ciJobCommit, map[string]any{"role": role})),
			http.StatusConflict, "CHECK_PROVED_BY_CI")
		if text != want {
			t.Fatalf("the %s's refusal reads %q, want %q", role, text, want)
		}
	}
}

// TestAResultsTokenOfAnotherJobIsRefused: a job records only the result of a check whose ci_check
// names its own check run. The run of envoy-go is refused c1, and the run of go-checks is
// refused k1, which names no ci_check.
func TestAResultsTokenOfAnotherJobIsRefused(t *testing.T) {
	rig := newCIJobRig(t)
	rig.github.setCheckRun(98, "envoy-go", ciJobCommit)
	other := rig.jobToken(func(answer *ciphertoken.Introspection) { answer.CI.CheckRunID = "98" })
	text := rig.refusedWith(t, "the envoy-go run's result for c1", rig.post(t, rig.issue, other, jobResultOf("c1", "pass")),
		http.StatusConflict, "CHECK_NOT_THIS_JOBS")
	for _, name := range []string{"c1", `"` + ciJobName + `"`, `"envoy-go"`} {
		if !strings.Contains(text, name) {
			t.Fatalf("the refusal %q does not name %s", text, name)
		}
	}

	text = rig.refusedWith(t, "the go-checks run's result for k1", rig.post(t, rig.issue, rig.jobToken(nil), jobResultOf("k1", "pass")),
		http.StatusConflict, "CHECK_NOT_THIS_JOBS")
	for _, name := range []string{"k1", "no ci_check", `"` + ciJobName + `"`} {
		if !strings.Contains(text, name) {
			t.Fatalf("the refusal %q does not name %s", text, name)
		}
	}
}

// TestAJobsResultAtAnotherCommitIsRefused: a job records a result only for the commit its run
// tested, and only through a check run that GitHub names at that commit. Each refusal names both
// commits.
func TestAJobsResultAtAnotherCommitIsRefused(t *testing.T) {
	rig := newCIJobRig(t)
	body := jobResultOf("c1", "pass")
	body["commit"] = ciJobOtherCommit
	text := rig.refusedWith(t, "a result for another commit", rig.post(t, rig.issue, rig.jobToken(nil), body),
		http.StatusForbidden, "COMMIT_NOT_THE_RUNS")
	if !strings.Contains(text, ciJobOtherCommit) || !strings.Contains(text, ciJobCommit) {
		t.Fatalf("the refusal %q names not both commits", text)
	}

	rig.github.setCheckRun(99, ciJobName, ciJobOtherCommit)
	moved := rig.jobToken(func(answer *ciphertoken.Introspection) { answer.CI.CheckRunID = "99" })
	text = rig.refusedWith(t, "a check run of another commit", rig.post(t, rig.issue, moved, jobResultOf("c1", "pass")),
		http.StatusForbidden, "CHECK_RUN_NOT_THE_RUNS")
	if !strings.Contains(text, ciJobOtherCommit) || !strings.Contains(text, ciJobCommit) {
		t.Fatalf("the refusal %q names not both commits", text)
	}
}

// TestAJobsResultOfARunOfNoBranchIsRefused: a job records a result for the push of a branch, and
// its row names that branch, which is how the results read tells it from a role's. A
// run of a tag, and a ref that names an empty branch, are refused, and nothing is stored.
func TestAJobsResultOfARunOfNoBranchIsRefused(t *testing.T) {
	rig := newCIJobRig(t)
	for _, ref := range []string{"refs/tags/v1.0.0", "refs/heads/"} {
		token := rig.jobToken(func(answer *ciphertoken.Introspection) {
			answer.CI.Ref = ref
			answer.CI.WorkflowRef = ciJobRepository + "/.github/workflows/ci-results.yaml@" + ref
		})
		text := rig.refusedWith(t, "a run of "+ref, rig.post(t, rig.issue, token, jobResultOf("c1", "pass")),
			http.StatusForbidden, "REF_NOT_A_BRANCH")
		if !strings.Contains(text, ref) {
			t.Fatalf("the refusal %q does not name the ref %s", text, ref)
		}
	}
}

// TestAJobOfAnotherRepositoryIsRefused: the token's run is of a repository that the issue does not
// resolve to, so its commit is no commit of the issue.
func TestAJobOfAnotherRepositoryIsRefused(t *testing.T) {
	rig := newCIJobRig(t)
	const other = "example-org/other-repo"
	token := rig.jobToken(func(answer *ciphertoken.Introspection) {
		answer.Session = "ci-results:" + other + "/.github/workflows/ci-results.yaml"
		answer.CI.Repository = other
		answer.CI.WorkflowRef = other + "/.github/workflows/ci-results.yaml@" + ciJobRef
	})
	text := rig.refusedWith(t, "a run of another repository", rig.post(t, rig.issue, token, jobResultOf("c1", "pass")),
		http.StatusForbidden, "REPOSITORY_NOT_THE_ISSUES")
	if !strings.Contains(text, other) || !strings.Contains(text, ciJobRepository) {
		t.Fatalf("the refusal %q names not both repositories", text)
	}
}

// TestAJobsResultCarriesNoFieldOfARole: the identity on a result is the one the token proved,
// never a field the caller set, so a job's body that names any field of a role's result is
// refused by name.
func TestAJobsResultCarriesNoFieldOfARole(t *testing.T) {
	rig := newCIJobRig(t)
	for name, value := range map[string]any{
		"role":             "tester",
		"actor":            map[string]any{"kind": "session", "id": ciJobSession},
		"run":              "go test ./...",
		"surface":          "api",
		"negative_control": ciJobNegativeControl,
		"failed":           1,
		"skipped":          1,
		"excluded":         1,
		"screenshots":      []string{"dispatch://" + rig.issue + "/artifact/screen"},
	} {
		body := jobResultOf("c1", "pass")
		body[name] = value
		text := rig.refusedWith(t, "a job's result naming "+name, rig.post(t, rig.issue, rig.jobToken(nil), body),
			http.StatusBadRequest, "RESULT_INPUT")
		if !strings.Contains(text, name) {
			t.Fatalf("the refusal of a body naming %s reads %q, which does not name it", name, text)
		}
	}
}

// TestAResultsTokenOfAnotherProjectIsRefused: a results token records on the issues of its own
// project alone.
func TestAResultsTokenOfAnotherProjectIsRefused(t *testing.T) {
	rig := newCIJobRig(t)
	other, _ := rig.issueOf(t, "OTHER", "example-org/other-repo")
	response := rig.post(t, other, rig.jobToken(nil), jobResultOf("c1", "pass"))
	if code, _ := refusalOf(t, response); response.Code != http.StatusForbidden || code != codeProjectScope {
		t.Fatalf("a result for %s under a token of %s: status=%d body=%s, want 403 %s",
			other, ciJobProject, response.Code, response.Body.String(), codeProjectScope)
	}
	if held := rig.stored(t); held != 0 {
		t.Fatalf("the refused result left %d results, want none", held)
	}
}

// TestAJobsResultLeavesTheCheckCIs: the job's result leaves c1 a check CI proves. An edit elsewhere
// in the document leaves it a settlement to owe, which holds every check block of the document to
// the four rules (reconcileCICheckBlocks), whose fourth counts a role's rows of check_results and
// never a CI job's of ci_check_results; that settlement keeps c1's ci_check and appends no
// block.invalid. A write that changes c1's negative control, which holds c1 to the four rules
// again (refuseCICheckBlocks), is stored. A settlement with nothing owed judges no block, so the
// edit is what makes the first half able to fail.
func TestAJobsResultLeavesTheCheckCIs(t *testing.T) {
	rig := newCIJobRig(t)
	jobRecorded(t, "the job's result for c1", rig.post(t, rig.issue, rig.jobToken(nil), jobResultOf("c1", "pass")))
	documents := rig.deps.Docs.(*docs.Service)

	if edited := applyOp(t, rig.handler, rig.verification, map[string]any{
		"op": "insert", "after": "end", "markdown": "A note below the checks.\n",
	}); edited.Code != http.StatusOK {
		t.Fatalf("an edit beside c1: status=%d body=%s, want 200", edited.Code, edited.Body.String())
	}
	if err := documents.SettleIssueDocuments(context.Background(), rig.issue); err != nil {
		t.Fatalf("settle the issue's documents: %v", err)
	}
	if check := rig.readJobCheck(t, "c1"); check.CICheck != ciJobName {
		t.Errorf("c1 once settled reads ci_check %q, want %q kept", check.CICheck, ciJobName)
	}
	if events := ciCheckRefusalEvents(t, rig.handler, rig.issue); len(events) != 0 {
		t.Errorf("settlement appended block.invalid %+v, want none", events)
	}

	changed := ciCheckBlock("c1", `negative_control="A build whose server fails to start." ci_check="`+ciJobName+`"`) +
		"\n" + ciCheckBlock("k1", `negative_control="`+ciJobNegativeControl+`"`) + "\nA note below the checks.\n"
	if written := uploadVerification(t, rig.handler, rig.issue, changed); written.Code != http.StatusCreated {
		t.Fatalf("a write changing c1's negative control: status=%d body=%s, want 201", written.Code, written.Body.String())
	}
	if err := documents.SettleIssueDocuments(context.Background(), rig.issue); err != nil {
		t.Fatalf("settle the issue's documents again: %v", err)
	}
	if check := rig.readJobCheck(t, "c1"); check.CICheck != ciJobName {
		t.Fatalf("c1 after the write reads ci_check %q, want %q kept", check.CICheck, ciJobName)
	}
	if events := ciCheckRefusalEvents(t, rig.handler, rig.issue); len(events) != 0 {
		t.Fatalf("the write left block.invalid %+v, want none", events)
	}
}

// TestTheReadShowsTheJobsResultBesideTheDerivedOne: c1's read carries its ci member, read at the
// head of the issue's pull request, and every result: in results, the one derived from the run at
// that head alone, as a dispatch command released before the job's own results reads results[0]
// of such a check as the derived one; and in recorded_results each one the job recorded at the
// commit it tested, newest first.
func TestTheReadShowsTheJobsResultBesideTheDerivedOne(t *testing.T) {
	rig := newCIJobRig(t)
	writeHead(t, rig.database, ciJobRepository, 70, ciJobHead, "open", rig.issue)

	older := jobRecorded(t, "the job's first result", rig.post(t, rig.issue, rig.jobToken(nil), jobResultOf("c1", "pass")))
	time.Sleep(50 * time.Millisecond)
	concluded := time.Now().UTC()
	time.Sleep(50 * time.Millisecond)
	newer := jobRecorded(t, "the job's second result", rig.post(t, rig.issue, rig.jobToken(nil), jobResultOf("c1", "fail")))

	run := completedRun(11, ciJobName, "success")
	run["completed_at"] = concluded.Format(time.RFC3339Nano)
	rig.github.listings.set(ciJobHead, run)

	check := rig.readJobCheck(t, "c1")
	if check.CI == nil || check.CI.Run != ciJobName || check.CI.State != model.CIStatePass {
		t.Fatalf("c1 carries ci %+v, want run %s passed", check.CI, ciJobName)
	}
	var order []string
	for _, result := range check.Results {
		order = append(order, result.ID)
	}
	derived := ciResultIDPrefix + "11"
	if want := []string{derived, newer.ID, older.ID}; strings.Join(order, " ") != strings.Join(want, " ") {
		t.Fatalf("c1 holds the results %v, want %v: the derived one, then the job's newer and older ones", order, want)
	}
	if wire := rawCheck(t, rig.readRawChecks(t, rig.issue), "c1"); len(wire.Results) != 1 || len(wire.RecordedResults) != 2 {
		t.Fatalf("c1's results hold %d and its recorded_results %d, want the derived result alone and the job's two", len(wire.Results), len(wire.RecordedResults))
	}
	for _, result := range check.Results {
		wantBranch, wantCommit := ciJobBranch, ciJobCommit
		if result.ID == derived {
			wantBranch, wantCommit = "", ciJobHead
		}
		if result.Role != "ci" || result.Branch != wantBranch || result.CommitSHA != wantCommit {
			t.Fatalf("result %s reads role %q, branch %q, commit %s; want ci, %q, %s",
				result.ID, result.Role, result.Branch, result.CommitSHA, wantBranch, wantCommit)
		}
	}
}

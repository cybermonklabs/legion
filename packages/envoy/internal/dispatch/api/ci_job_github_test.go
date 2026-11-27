// ci_job_github_test.go holds ciJobGitHub, the stand-in for GitHub that the CI job rig serves
// (ci_job_results_test.go, newCIJobRigSettling): the installation of every repository, a job's
// check runs, the commits a repository holds, the check runs at a head, a branch's head, a pull
// request with its head and base revisions, its commits and the comparison of two commits. The
// tests of a CI job's results and the tests of the check states settler both read it, so it is
// here, apart from either, and each changes for its own reason.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/internal/githubtest"
)

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
	// pullReads counts those reads. pullsRefused, when set, answers every read of a pull request
	// itself 403, as GitHub refuses a token the repository it may not read (refusePulls).
	pulls        map[int]ciJobPull
	pullReads    map[int]int
	pullsRefused bool
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

// refusePulls has GitHub refuse every read of a pull request itself with 403, as it refuses an
// installation token the repository it may not read; false ends it.
func (g *ciJobGitHub) refusePulls(refused bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pullsRefused = refused
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
		if g.pullsRefused {
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"message":"Resource not accessible by integration"}`)
			return
		}
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

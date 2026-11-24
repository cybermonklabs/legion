package daemon

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
)

// dequeueAnswer is how GitHub answers the read of one pull request: open at its head, which is
// what every pull request answers until its case says otherwise; merged; or a 502, as GitHub
// answers while it is unavailable.
type dequeueAnswer string

const (
	dequeueOpen        dequeueAnswer = "open"
	dequeueMerged      dequeueAnswer = "merged"
	dequeueUnavailable dequeueAnswer = "unavailable"
)

// dequeueGitHub is GitHub's REST API for acme/widgets as this test's daemon reads it: each case's
// pull request, answered as its case sets it, held unanswered when its case says so, and counted at
// every read, with its reviews as none; its list of open pull requests as empty; and the
// repository, and any other a launch reads, with main its default branch. Anything else is answered
// 404 and logged, so a read the test did not foresee is named in its output rather than reaching
// github.com. A request without the daemon's App token (workflowAppToken) is another process's: it
// is answered 404 and logged (fromTheDaemon), and it takes no hold and counts as no read.
type dequeueGitHub struct {
	url   string
	mu    sync.Mutex
	pulls map[int]*dequeuePull
}

// dequeuePull is one pull request as dequeueGitHub holds it: the issue's branch and the head it
// stands at, its answer, and the reads of it and of its reviews so far.
type dequeuePull struct {
	issue  dequeueIssue
	answer dequeueAnswer
	reads  int
	// reviewReads counts the reads of its reviews, the per-issue read's last call (readReviews).
	reviewReads int
	// hold is what the next read of the pull request waits for before it is answered (holdRead),
	// nil for none, and heldReads counts the reads that waited.
	hold      chan struct{}
	heldReads int
}

func newDequeueGitHub(t *testing.T) *dequeueGitHub {
	t.Helper()
	g := &dequeueGitHub{pulls: map[int]*dequeuePull{}}
	g.url = githubtest.Server(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
		if !fromTheDaemon(t, w, r, workflowAppToken) {
			return
		}
		g.wait(r)
		if r.Method == http.MethodGet && g.answered(w, r.URL.Path) {
			return
		}
		t.Logf("the GitHub stand-in has no answer for %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"message":"Not Found"}`)
	}})
	return g
}

// serve makes GitHub hold issue's pull request, open at the issue's head.
func (g *dequeueGitHub) serve(issue dequeueIssue) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pulls[issue.number] = &dequeuePull{issue: issue, answer: dequeueOpen}
}

// answer makes GitHub answer every later read of pull request number as answer says.
func (g *dequeueGitHub) answer(number int, answer dequeueAnswer) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pulls[number].answer = answer
}

// reads is how many times GitHub was asked for pull request number.
func (g *dequeueGitHub) reads(number int) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.pulls[number].reads
}

// startRead is whether the per-issue read has made both its calls of pull request number: the
// pull request, and then its reviews.
func (g *dequeueGitHub) startRead(number int) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	pull := g.pulls[number]
	return pull.reads >= 1 && pull.reviewReads >= 1
}

// holdRead makes GitHub keep the next read of pull request number unanswered until release is
// called, as GitHub keeps a read it is slow to answer, and answer it then as its answer says.
// release may be called more than once.
func (g *dequeueGitHub) holdRead(number int) (release func()) {
	g.mu.Lock()
	defer g.mu.Unlock()
	hold := make(chan struct{})
	g.pulls[number].hold = hold
	var once sync.Once
	return func() { once.Do(func() { close(hold) }) }
}

// heldReads is how many reads of pull request number a hold has kept unanswered.
func (g *dequeueGitHub) heldReads(number int) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.pulls[number].heldReads
}

// wait keeps r unanswered while it is the read of a pull request holdRead holds, until the hold is
// released or the reader gives up, and returns at once for any other request.
func (g *dequeueGitHub) wait(r *http.Request) {
	g.mu.Lock()
	pull, _, tail := g.pullAt(r.URL.Path)
	if r.Method != http.MethodGet || pull == nil || tail != "" || pull.hold == nil {
		g.mu.Unlock()
		return
	}
	hold := pull.hold
	pull.hold = nil
	pull.heldReads++
	g.mu.Unlock()
	select {
	case <-hold:
	case <-r.Context().Done():
	}
}

// answered writes the stand-in's answer to a GET of path, and says whether it had one.
func (g *dequeueGitHub) answered(w http.ResponseWriter, path string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	rest, ours := strings.CutPrefix(path, "/repos/"+startedAgainRepo)
	if !ours {
		other, isRepository := strings.CutPrefix(path, "/repos/")
		if parts := strings.Split(other, "/"); !isRepository || len(parts) != 2 {
			return false
		}
		return g.write(w, map[string]string{"full_name": other, "default_branch": "main"})
	}
	switch {
	case rest == "":
		return g.write(w, map[string]string{"full_name": startedAgainRepo, "default_branch": "main"})
	case rest == "/pulls":
		return g.write(w, []any{})
	}
	pull, n, tail := g.pullAt(path)
	if pull == nil {
		return false
	}
	switch tail {
	case "reviews":
		pull.reviewReads++
		return g.write(w, []any{})
	case "":
		pull.reads++
	default:
		return false
	}
	answer := map[string]any{
		"number": n, "state": "open", "merged": false, "html_url": fmt.Sprintf("https://github.com/%s/pull/%d", startedAgainRepo, n),
		"mergeable": true, "mergeable_state": "clean", "base": map[string]string{"ref": "main"},
		"head": map[string]string{"ref": claim.IssueBranch(pull.issue.key), "sha": pull.issue.head},
	}
	switch pull.answer {
	case dequeueMerged:
		answer["state"], answer["merged"], answer["merge_commit_sha"] = "closed", true, dequeueHead(7000+n)
	case dequeueUnavailable:
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, `{"message":"Server Error"}`)
		return true
	}
	return g.write(w, answer)
}

// pullAt is the pull request path names in the stand-in's repository, its number, and what follows
// the number ("" for the pull request itself), or nil where path names none of its pull requests.
// Callers hold g.mu.
func (g *dequeueGitHub) pullAt(path string) (*dequeuePull, int, string) {
	numbered, isPull := strings.CutPrefix(path, "/repos/"+startedAgainRepo+"/pulls/")
	number, tail, _ := strings.Cut(numbered, "/")
	n, err := strconv.Atoi(number)
	if !isPull || err != nil {
		return nil, 0, ""
	}
	return g.pulls[n], n, tail
}

// write encodes value as the stand-in's answer, and says it answered.
func (g *dequeueGitHub) write(w http.ResponseWriter, value any) bool {
	_ = json.NewEncoder(w).Encode(value)
	return true
}

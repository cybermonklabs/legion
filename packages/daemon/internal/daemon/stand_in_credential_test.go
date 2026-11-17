package daemon

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
)

// fromTheDaemon reports whether r carries bearer, the credential the daemon under test presents to
// a stand-in. Any other request is another process's: the loopback port the kernel gives a
// stand-in may be one a process of the machine still sends to. It is answered 404 and logged, with
// its method and path quoted and cut short, and fails nothing, so a stand-in's power to fail the
// test stays with the daemon's own requests.
func fromTheDaemon(t *testing.T, w http.ResponseWriter, r *http.Request, bearer string) bool {
	t.Helper()
	if r.Header.Get("Authorization") == "Bearer "+bearer {
		return true
	}
	t.Logf("a stand-in answered 404 to %.16q %.200q, which carries no credential of the daemon's", r.Method, r.URL.Path)
	w.WriteHeader(http.StatusNotFound)
	return false
}

// dispatchBoard starts a Dispatch board, closed when the test ends, and answers its URL. The board
// serves handler to each request that carries the daemon's Dispatch bearer (workflowDispatchToken)
// and answers any other request 404 (fromTheDaemon). Every Dispatch board of daemon_test.go starts
// here.
func dispatchBoard(t *testing.T, handler http.HandlerFunc) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fromTheDaemon(t, w, r, workflowDispatchToken) {
			handler(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// workflowBranchGitHub is newBranchGitHub for a daemon whose GitHub App leases a
// workflowTokenRecorder mints: it serves each request that carries their token (workflowAppToken)
// and answers any other request 404 (fromTheDaemon), so it records no read or create of another
// process's.
func workflowBranchGitHub(t *testing.T, answer func() (int, string)) *branchGitHub {
	t.Helper()
	g := &branchGitHub{answer: answer, defaultBranch: "main"}
	handle := g.handle(t, nil)
	g.url = githubtest.Server(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
		if fromTheDaemon(t, w, r, workflowAppToken) {
			handle(w, r)
		}
	}})
	return g
}

// A stand-in that holds the daemon to its requests answers a stranger's request 404, fails
// nothing, and counts nothing of it, in each form the request takes: with no credential, with
// another process's bearer, with the daemon's token under another scheme, with more after the
// daemon's token, and with a prefix of it. A Dispatch board (dispatchBoard) does not reach its
// handler. The dequeue rig's GitHub takes no hold of a held read and counts no read of the pull
// request or of its reviews, the counts TestADequeuedPullRequestReachesItsMerger reads. Its Envoy
// listener counts no refusal on a merger's topic no session holds and delivers nothing on one a
// session holds. The moved issue's GitHub (workflowBranchGitHub) records no create of a branch,
// the record TestAnIssueMovedWhileBootListsIsStillAdmitted reads. The first row's request is the
// one a helper of the machine sent to a reused port: a read of its metadata server's identity
// token. The daemon's own credential, sent last, is served and counted, so each count can move.
func TestAStandInAnswersARequestWithoutTheDaemonsCredential404(t *testing.T) {
	var reached atomic.Int64
	board := dispatchBoard(t, func(http.ResponseWriter, *http.Request) { reached.Add(1) })
	github := newDequeueGitHub(t)
	issue := dequeueIssue{key: "ACME-1", number: 301, head: dequeueHead(301)}
	github.serve(issue)
	release := github.holdRead(issue.number)
	t.Cleanup(release)
	var cfg config.Config
	listener := newDequeueListener(t, &cfg)
	const unheld, held = "notifications.legion.ACME.role.merger-1", "notifications.legion.ACME.role.merger-2"
	listener.unheld(unheld)
	listener.holds(held)
	branches := workflowBranchGitHub(t, branchCreated)
	pull := github.url + "/repos/" + startedAgainRepo + "/pulls/301"

	type stand struct{ name, token, method, url, body string }
	dispatch := stand{"a Dispatch board", workflowDispatchToken, http.MethodGet, board + "/computeMetadata/v1/instance/service-accounts/default/identity", ""}
	heldRead := stand{"the dequeue rig's GitHub at a held read", workflowAppToken, http.MethodGet, pull, ""}
	reviews := stand{"the dequeue rig's GitHub at the reviews", workflowAppToken, http.MethodGet, pull + "/reviews", ""}
	refusal := stand{"the dequeue rig's Envoy listener on an unheld merger's topic", dequeueEnvoyToken, http.MethodPost,
		cfg.EnvoyURL + "/v1/messages/publish", `{"topic":"` + unheld + `","message":"m"}`}
	delivery := stand{"the dequeue rig's Envoy listener on a held merger's topic", dequeueEnvoyToken, http.MethodPost,
		cfg.EnvoyURL + "/v1/messages/publish", `{"topic":"` + held + `","message":"m"}`}
	branch := stand{"the moved issue's GitHub at a create of its branch", workflowAppToken, http.MethodPost,
		branches.url + "/repos/acme/widgets/git/refs", `{"ref":"refs/heads/legion/ACME-1","sha":"` + branchBaseCommit + `"}`}
	client := &http.Client{Timeout: 10 * time.Second}
	send := func(t *testing.T, s stand, authorization string) int {
		t.Helper()
		request, err := http.NewRequest(s.method, s.url, strings.NewReader(s.body))
		if err != nil {
			t.Fatal(err)
		}
		if authorization != "" {
			request.Header.Set("Authorization", authorization)
		}
		response, err := client.Do(request)
		if err != nil {
			t.Fatalf("%s %s with Authorization %q: %v", s.method, s.url, authorization, err)
		}
		_ = response.Body.Close()
		return response.StatusCode
	}

	for _, s := range []stand{dispatch, heldRead, reviews, refusal, delivery, branch} {
		for _, stranger := range []struct{ name, authorization string }{
			{"with no credential", ""},
			{"with another process's bearer", "Bearer another-process-token-not-a-secret"},
			{"with the daemon's token under another scheme", "token " + s.token},
			{"with more after the daemon's token", "Bearer " + s.token + "-x"},
			{"with a prefix of the daemon's token", "Bearer " + s.token[:len(s.token)-1]},
		} {
			t.Run(s.name+" "+stranger.name, func(t *testing.T) {
				if status := send(t, s, stranger.authorization); status != http.StatusNotFound {
					t.Fatalf("%s %s with Authorization %q: status %d, want 404", s.method, s.url, stranger.authorization, status)
				}
			})
		}
	}
	type counted struct {
		handler, reads, reviewReads, heldReads int
		holdSet                                bool
		refusals, deliveries, creates          int
	}
	counts := func() counted {
		github.mu.Lock()
		p := github.pulls[issue.number]
		c := counted{handler: int(reached.Load()), reads: p.reads, reviewReads: p.reviewReads, heldReads: p.heldReads, holdSet: p.hold != nil}
		github.mu.Unlock()
		c.refusals, c.deliveries, c.creates = listener.refused(unheld), len(listener.accepted(held)), len(branches.created())
		return c
	}
	if got, want := counts(), (counted{holdSet: true}); got != want {
		t.Errorf("after the strangers' requests the stand-ins counted %+v, want %+v", got, want)
	}
	for _, s := range []stand{dispatch, reviews, refusal, delivery, branch} {
		send(t, s, "Bearer "+s.token)
	}
	// The daemon's own read of the held pull request waits for the hold, which is released once it
	// holds the read.
	go func() {
		for github.heldReads(issue.number) == 0 {
			select {
			case <-t.Context().Done():
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
		release()
	}()
	send(t, heldRead, "Bearer "+heldRead.token)
	if got, want := counts(), (counted{handler: 1, reads: 1, reviewReads: 1, heldReads: 1, refusals: 1, deliveries: 1, creates: 1}); got != want {
		t.Errorf("after the daemon's own requests the stand-ins counted %+v, want %+v", got, want)
	}
}

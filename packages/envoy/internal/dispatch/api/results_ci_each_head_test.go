// results_ci_each_head_test.go holds GET /api/v1/issues/{key}/results to R8: for a check CI
// proves, Dispatch derives one result at the head of each open pull request of the issue, from the
// newest check run of the named job at that head, read in the repository of that pull request's own
// row. A read whose bound ends before it does makes that head alone not known. The test reads the
// route's answer as JSON into the types of this file, so it builds at a commit before the rule and
// fails there on its own assertion.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
)

// The heads of this test: H1 and H2 of #70 and #71 in the issue's repository, and H3 of #9 of
// another repository the issue links.
const (
	ciEachHeadH1 = "c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1"
	ciEachHeadH2 = "c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2"
	ciEachHeadH3 = "c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3"
)

// TestCIReadBoundHasDeadline holds ciReadBound to ciReadDeadline from both sides and with no
// allowance: the bound starts inside the call, so its deadline is ciReadDeadline after a moment
// between the clock read before the call and the one after it. A bound of another length passes
// it only where the call itself took as long as the difference, nine seconds for a bound of a
// second and ten for GitHub's twenty seconds, and a bound with no deadline never does.
func TestCIReadBoundHasDeadline(t *testing.T) {
	t.Parallel()
	before := time.Now()
	bound, cancel := ciReadBound(context.Background())
	after := time.Now()
	defer cancel()
	deadline, ok := bound.Deadline()
	if !ok {
		t.Fatal("ciReadBound gives a context with no deadline")
	}
	if earliest := before.Add(ciReadDeadline); deadline.Before(earliest) {
		t.Fatalf("ciReadBound deadline %s is earlier than %s, %s after the call started", deadline, earliest, ciReadDeadline)
	}
	if latest := after.Add(ciReadDeadline); deadline.After(latest) {
		t.Fatalf("ciReadBound deadline %s is later than %s, %s after the call returned", deadline, latest, ciReadDeadline)
	}
}

// ciEachHeadVerification is c1, a check whose ci_check names the job go-checks.
const ciEachHeadVerification = ":::check{#c1 negative_control=\"A build whose server does not build.\" ci_check=\"go-checks\"}\n" +
	"The server builds at the head.\n" +
	":::\n"

// ciEachHeadGitHub is GitHub's listing of check runs, keyed by repository and commit, with the
// count of the requests for each, and a hold that keeps the reads of one key waiting until their
// caller gives up (ciHeldRead).
type ciEachHeadGitHub struct {
	mu    sync.Mutex
	runs  map[string][]map[string]any
	asked map[string]int
	held  map[string]*ciHeldRead
}

func newCIEachHeadGitHub() *ciEachHeadGitHub {
	return &ciEachHeadGitHub{runs: map[string][]map[string]any{}, asked: map[string]int{}, held: map[string]*ciHeldRead{}}
}

func ciEachHeadKey(repo, sha string) string { return repo + "@" + sha }

// set is the runs GitHub lists for sha in repo. A key it holds no runs for answers 422, as GitHub
// does for a commit the repository does not hold.
func (g *ciEachHeadGitHub) set(repo, sha string, runs ...map[string]any) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.runs[ciEachHeadKey(repo, sha)] = runs
}

// hold keeps every read of sha in repo waiting until its caller gives up, or until t ends, and
// answers the hold.
func (g *ciEachHeadGitHub) hold(t *testing.T, repo, sha string) *ciHeldRead {
	t.Helper()
	held := newCIHeldRead(t)
	g.mu.Lock()
	defer g.mu.Unlock()
	g.held[ciEachHeadKey(repo, sha)] = held
	return held
}

func (g *ciEachHeadGitHub) requests(repo, sha string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.asked[ciEachHeadKey(repo, sha)]
}

func (g *ciEachHeadGitHub) serve(w http.ResponseWriter, r *http.Request) {
	key := ciEachHeadKey(r.PathValue("owner")+"/"+r.PathValue("repo"), r.PathValue("sha"))
	g.mu.Lock()
	g.asked[key]++
	runs, listed := g.runs[key]
	held := g.held[key]
	g.mu.Unlock()
	if held != nil {
		held.wait(r)
		return
	}
	if !listed {
		w.WriteHeader(http.StatusUnprocessableEntity)
		fmt.Fprintf(w, `{"message":"No commit found for SHA: %s"}`, r.PathValue("sha"))
		return
	}
	if page, _ := strconv.Atoi(r.URL.Query().Get("page")); page > 1 {
		runs = nil
	}
	body, _ := json.Marshal(map[string]any{"total_count": len(runs), "check_runs": runs})
	_, _ = w.Write(body)
}

// ciEachHeadSubject is one subject of c1's pass_stands, with only the members this test reads.
type ciEachHeadSubject struct {
	Basis struct {
		Commit string `json:"commit"`
	} `json:"basis"`
	Answer string `json:"answer"`
	Reason string `json:"reason"`
}

// ciEachHeadRead is c1's subjects as the results read answers them, each by its commit.
func ciEachHeadRead(t *testing.T, handler http.Handler, issue string) map[string]ciEachHeadSubject {
	t.Helper()
	return ciEachHeadSubjectsOf(t, issue, dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue+"/results", nil, "alice"))
}

// ciEachHeadSubjectsOf is c1's subjects in response, the results read of issue, each by its commit.
func ciEachHeadSubjectsOf(t *testing.T, issue string, response *httptest.ResponseRecorder) map[string]ciEachHeadSubject {
	t.Helper()
	if response.Code != http.StatusOK {
		t.Fatalf("read the results of %s: status=%d body=%s", issue, response.Code, response.Body.String())
	}
	var read struct {
		Checks []struct {
			BlockID    string `json:"block_id"`
			PassStands struct {
				Subjects []ciEachHeadSubject `json:"subjects"`
			} `json:"pass_stands"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &read); err != nil {
		t.Fatalf("decode the results of %s: %v", issue, err)
	}
	for _, check := range read.Checks {
		if check.BlockID == "c1" {
			subjects := map[string]ciEachHeadSubject{}
			for _, subject := range check.PassStands.Subjects {
				subjects[subject.Basis.Commit] = subject
			}
			return subjects
		}
	}
	t.Fatalf("the read of %s holds no check c1: %s", issue, response.Body.String())
	return nil
}

// ciEachHeadWants fails the test unless subjects names head with answer for reason.
func ciEachHeadWants(t *testing.T, what string, subjects map[string]ciEachHeadSubject, head, answer, reason string) {
	t.Helper()
	subject, named := subjects[head]
	if !named {
		t.Fatalf("%s: c1 names no subject at %s, only %+v", what, head, subjects)
	}
	if subject.Answer != answer || subject.Reason != reason {
		t.Fatalf("%s: the head at %s reads %q (reason %q), want %q (reason %q)", what, head, subject.Answer, subject.Reason, answer, reason)
	}
}

// TestACheckCIProvesDerivesAResultAtEachOpenHead: R8. Dispatch reads the named job's newest check
// run at each open head, each in its own pull request's repository, and a read whose bound ends
// before it does makes that head alone not known.
func TestACheckCIProvesDerivesAResultAtEachOpenHead(t *testing.T) {
	// Not parallel: it writes the package variable ciReadBound (holdCIReadBounds), which every server of the package reads.
	github := newCIEachHeadGitHub()
	handler, database, _ := resultsGitHubServed(t, map[string]bool{"acme/widgets": true, "acme/infra": true}, nil, "main",
		func(string, string) (int, string) { return http.StatusOK, "diverged" }, github.serve)
	issue := issueInRepository(t, handler, "RESEH", "acme/widgets", "A check CI proves at each head")
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", ciEachHeadVerification)
	writeHead(t, database, "acme/widgets", 70, ciEachHeadH1, "open", issue.Key)
	writeHead(t, database, "acme/widgets", 71, ciEachHeadH2, "open", issue.Key)

	github.set("acme/widgets", ciEachHeadH1, completedRun(51, "go-checks", "success"))
	github.set("acme/widgets", ciEachHeadH2, completedRun(52, "go-checks", "success"))
	subjects := ciEachHeadRead(t, handler, issue.Key)
	ciEachHeadWants(t, "a success at H1", subjects, ciEachHeadH1, "yes", "")
	ciEachHeadWants(t, "a success at H2", subjects, ciEachHeadH2, "yes", "")
	if h1, h2 := github.requests("acme/widgets", ciEachHeadH1), github.requests("acme/widgets", ciEachHeadH2); h1 != 1 || h2 != 1 {
		t.Fatalf("one read asked GitHub %d times at H1 and %d at H2, want once at each", h1, h2)
	}

	github.set("acme/widgets", ciEachHeadH1, completedRun(53, "go-checks", "failure"))
	ciEachHeadWants(t, "a failure at H1", ciEachHeadRead(t, handler, issue.Key), ciEachHeadH1, "no", "fail")

	github.set("acme/widgets", ciEachHeadH2, completedRun(54, "go-checks", "failure"), completedRun(55, "go-checks", "success"))
	ciEachHeadWants(t, "the newer run's success at H2", ciEachHeadRead(t, handler, issue.Key), ciEachHeadH2, "yes", "")

	t.Run("each head in its own repository", func(t *testing.T) {
		writeHead(t, database, "acme/infra", 9, ciEachHeadH3, "open", issue.Key)
		github.set("acme/infra", ciEachHeadH3, completedRun(56, "go-checks", "success"))
		ciEachHeadWants(t, "a success at H3 in acme/infra", ciEachHeadRead(t, handler, issue.Key), ciEachHeadH3, "yes", "")
		if infra, widgets := github.requests("acme/infra", ciEachHeadH3), github.requests("acme/widgets", ciEachHeadH3); infra != 1 || widgets != 0 {
			t.Fatalf("H3 was read %d times in acme/infra and %d in acme/widgets, want once in its own repository alone", infra, widgets)
		}
	})

	// H2's read is held until its caller gives up. The reads of every head share one bound
	// (readCIHeads), which the test ends as ciReadDeadline would, once H2's read has reached GitHub
	// and the reads of H1 and H3 have returned, so no clock decides which head was read in time.
	// H2 then reads unknown for read_failed, H1 keeps its fail, and the held read sees its context
	// end. The time each step took is logged, and asserted nowhere.
	t.Run("a read that does not end", func(t *testing.T) {
		bounds := holdCIReadBounds(t)
		// GitHub's client timeout must outlast the row's wait, so only ciReadBound can end H2's read.
		savedTimeout := githubapp.RequestTimeout
		githubapp.RequestTimeout = 24 * time.Hour
		t.Cleanup(func() { githubapp.RequestTimeout = savedTimeout })
		held := github.hold(t, "acme/widgets", ciEachHeadH2)
		started := time.Now()
		answered := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			answered <- dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/results", nil, "alice")
		}()
		ciAwait(t, "H2's read to reach GitHub", held.entered)
		ciAwait(t, "the read of the first of H1 and H3 to return", bounds.ended)
		ciAwait(t, "the read of the second of H1 and H3 to return", bounds.ended)
		reads := time.Since(started)
		bounds.expire(t)
		ciAwait(t, "H2's held read to see its context end", held.cancelled)
		subjects := ciEachHeadSubjectsOf(t, issue.Key, ciAwait(t, "the results read to answer", answered))
		t.Logf("H1 and H3 were read %s after the results read began; H2's held read saw its bound end and the read answered %s after it began",
			reads, time.Since(started))
		ciEachHeadWants(t, "H2 held until its bound ended", subjects, ciEachHeadH2, "unknown", "read_failed")
		ciEachHeadWants(t, "H1 beside the held H2", subjects, ciEachHeadH1, "no", "fail")
	})
}

// ciHeldRead is a read of check runs a fixture holds until its caller gives up: entered closes
// when the read reaches GitHub, and cancelled when the read's context ends. A read still held when
// the test that held it ends is let go then, so the fixture's server can close.
type ciHeldRead struct {
	entered, cancelled, release chan struct{}
	enter, cancel               sync.Once
}

func newCIHeldRead(t *testing.T) *ciHeldRead {
	t.Helper()
	held := &ciHeldRead{entered: make(chan struct{}), cancelled: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(func() { close(held.release) })
	return held
}

// wait is the held read r: it says it reached GitHub, then waits for r's context to end and says
// so, or for its test to end.
func (h *ciHeldRead) wait(r *http.Request) {
	h.enter.Do(func() { close(h.entered) })
	select {
	case <-r.Context().Done():
		h.cancel.Do(func() { close(h.cancelled) })
	case <-h.release:
	}
}

// ciReadBounds stands in for ciReadBound for one test, so no bound of a read of GitHub ends on a
// clock. The first bound made outside another of its own is the bound of the reads of every head
// (readCIHeads): it ends with context.DeadlineExceeded when expire is called, as ciReadDeadline
// would end it. A bound made inside one is the bound of one head's read (readCIRuns), and it says on
// ended when its read has returned and cancelled it.
type ciReadBounds struct {
	mu    sync.Mutex
	outer *ciEndedBound
	ended chan struct{}
}

// holdCIReadBounds puts a ciReadBounds in ciReadBound's place until t ends.
func holdCIReadBounds(t *testing.T) *ciReadBounds {
	t.Helper()
	bounds := &ciReadBounds{ended: make(chan struct{}, 64)}
	before := ciReadBound
	ciReadBound = bounds.bound
	t.Cleanup(func() { ciReadBound = before })
	return bounds
}

func (b *ciReadBounds) bound(ctx context.Context) (context.Context, context.CancelFunc) {
	inner := ctx.Value(ciEndedBoundKey{}) != nil
	bound := &ciEndedBound{parent: ctx, done: make(chan struct{})}
	stop := context.AfterFunc(ctx, func() { bound.end(ctx.Err()) })
	b.mu.Lock()
	if !inner && b.outer == nil {
		b.outer = bound
	}
	b.mu.Unlock()
	return bound, func() {
		stop()
		bound.end(context.Canceled)
		if inner {
			b.ended <- struct{}{}
		}
	}
}

// expire ends the bound of the reads of every head as its deadline would.
func (b *ciReadBounds) expire(t *testing.T) {
	t.Helper()
	b.mu.Lock()
	outer := b.outer
	b.mu.Unlock()
	if outer == nil {
		t.Fatal("no read of check runs has begun, so no bound is there to end")
	}
	outer.end(context.DeadlineExceeded)
}

// ciEndedBoundKey is the key a ciEndedBound answers itself for, so a bound made under one knows it.
type ciEndedBoundKey struct{}

// ciEndedBound is a context with no deadline that ends with the error end gives it, or with its
// parent. It answers its parent's values, so the context package cancels a context made under it
// with this bound's own Err, context.DeadlineExceeded where expire ended it.
type ciEndedBound struct {
	parent context.Context
	done   chan struct{}
	once   sync.Once
	mu     sync.Mutex
	err    error
}

func (c *ciEndedBound) end(err error) {
	c.once.Do(func() {
		c.mu.Lock()
		c.err = err
		c.mu.Unlock()
		close(c.done)
	})
}

func (c *ciEndedBound) Deadline() (time.Time, bool) { return time.Time{}, false }

func (c *ciEndedBound) Done() <-chan struct{} { return c.done }

func (c *ciEndedBound) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

func (c *ciEndedBound) Value(key any) any {
	if key == (ciEndedBoundKey{}) {
		return c
	}
	return c.parent.Value(key)
}

// ciAwait waits for done, which an event of the test closes or sends on, with no bound of its own:
// only the test binary's deadline, less a minute, stops the wait, so a fault that never sends
// fails the test naming what it waited for, rather than hanging it until the binary panics.
func ciAwait[T any](t *testing.T, what string, done <-chan T) T {
	t.Helper()
	var guard <-chan time.Time
	if deadline, set := t.Deadline(); set {
		guard = time.After(time.Until(deadline) - time.Minute)
	}
	select {
	case value := <-done:
		return value
	case <-guard:
		t.Fatalf("waited until a minute before the test binary's deadline for %s", what)
	}
	var none T
	return none
}

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/admission"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/testwait"
)

// heldClosureReads is a tracer that holds every issue references read at its first statement, once
// armed, until let. Each statement of refs.Closure starts with refs.ClosureTag, and the first of a
// read is the budget statement boundedClosure sends before it (setStatementBudget), the one that
// sets the read's two-second statement_timeout: so a read held there keeps its read unit, its
// request-pool connection and its transaction, as a read stuck on the database does, and nothing
// of its own bound runs. reads counts the statements held. let also runs when the test's context
// ends, which is cancelled before the test's cleanups, so a test that failed while the reads were
// held never leaves the server's shutdown waiting on them.
type heldClosureReads struct {
	armed   atomic.Bool
	reads   atomic.Int32
	release chan struct{}
	ended   <-chan struct{}
	once    sync.Once
}

func newHeldClosureReads(t *testing.T) *heldClosureReads {
	held := &heldClosureReads{release: make(chan struct{}), ended: t.Context().Done()}
	t.Cleanup(held.let)
	return held
}

func (h *heldClosureReads) let() { h.once.Do(func() { close(h.release) }) }

func (h *heldClosureReads) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if !h.armed.Load() || !strings.HasPrefix(data.SQL, refs.ClosureTag) {
		return ctx
	}
	h.reads.Add(1)
	select {
	case <-h.release:
	case <-h.ended:
	}
	return ctx
}

func (h *heldClosureReads) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// An issue's references read can run its whole bound on the database: on the production-sized copy
// of the load test (c-scale, INFRA-592 tester round 60) the one-hop read still ran 3.5 to 4 s and
// ended at its statement timeout on almost every call, at main as at the head. While the route takes
// ordinary read units, a burst of such reads holds the whole read class for that long, and every
// other read is refused or waits behind them: there the head answered 13.1% of requests 2xx where
// main answered 93.0%. So a burst of references reads that hold the database must leave the rest
// of the read class to the other routes. Here a tracer holds every references read at its first
// statement (heldClosureReads), past any run, so none of them ends, and none gives its unit back,
// before the row says so: a lock on refs held them only until each read's own two seconds
// (referencesClosureBound) ran out, after which they answered 503 and freed their units with the
// lock still held, so the row passed with the lane removed. A read of another route sent behind
// the burst must be answered while every read of the burst is still held: the burst goes only
// after that answer, so a read that queued behind the burst for a unit would not be answered before
// it. The row waits for that answer with the test binary's deadline as its one bound, and the read
// class's wait, which the burst's reads queued in the lane wait in for as long as the row holds
// them, and the read's deadline are not what it proves, and are past any run.
func TestABurstOfSlowIssueReferencesLeavesTheReadClassToOtherReads(t *testing.T) {
	t.Parallel()
	const readCap = 4
	limiter := admission.New(admission.Limits{Read: readCap, Write: 4, HeavyReadLane: 1, ReadWait: pastTheRun})
	held := newHeldClosureReads(t)
	handler, _, _ := newTestServer(t, testServerOptions{admission: limiter, tracer: held, readDeadline: pastTheRun})
	createReferenceAPIProject(t, handler, "CORE")
	issue := createReferenceAPIIssue(t, handler, "CORE")
	createProjectDocument(t, handler, "CORE", "Design notes", "# Design\n")
	createReferenceAPIComment(t, handler, issue.Key, "dispatch://CORE/artifact/design-notes")

	held.armed.Store(true)
	burst := make(chan *httptest.ResponseRecorder, readCap)
	for range readCap {
		go func() { burst <- issueReferencesRequest(t, handler, issue.Key, "") }()
	}
	waitUntil(t, "a references read held at the closure's first statement", func() bool { return held.reads.Load() > 0 })
	// Every request of the burst reaches its admission: the admitted ones go on to the held
	// statement, and the rest wait in the references lane's queue.
	waitUntil(t, "every references read of the burst admitted or queued in its lane", func() bool {
		return limiter.Stats()[admission.Read].InFlight+referencesLane(t, limiter).Queued == readCap
	})
	stats := limiter.Stats()[admission.Read]

	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() { answered <- dispatchRequest(t, handler, http.MethodGet, "/api/v1/me/state", nil, "alice") }()
	other := testwait.Receive(t, "a read of another route, sent behind a burst of references reads that are all still held, to be answered", answered)
	select {
	case early := <-burst:
		t.Fatalf("a references read of the burst was answered status=%d before the row let the burst go, want every read of it held while the read of another route was answered", early.Code)
	default:
	}
	held.let()
	for range readCap {
		awaitResponse(t, burst)
	}

	if other.Code != http.StatusOK {
		t.Fatalf("a read of another route sent behind %d references reads that held the database (read class %d in flight of %d) "+
			"answered status %d with the burst still held, want 200: a burst of one slow route takes the whole read class, "+
			"and every other read waits for it or is refused",
			readCap, stats.InFlight, readCap, other.Code)
	}
}

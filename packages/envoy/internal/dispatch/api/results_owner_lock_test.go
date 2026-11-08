package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The results write takes the owner locks of the recording issue and of the check's issue
// (lockIssueOwners), and those locks serialise every writer of either issue. It also judges the
// result by the check's words, which live in the live Verification document: a read that for a
// document no room holds is a cold read bounded by a minute (docs/read_cache.go). This file holds
// the writer that must not wait behind that read.

// slowCheckRead is how long this file's document service takes over the live read the route makes
// once the check's digest has moved. It stands in for a cold read's minute, so the test measures a
// fixed number instead of waiting one out, and it is far longer than any other statement either
// request runs.
const slowCheckRead = 2 * time.Second

// slowCheckReadKey marks the one request whose document reads this file's service gates and slows.
// Every other caller of the same service - the uploads that seed the fixture, the PATCH that
// measures the wait - reads at full speed, so only the measured route is held up.
type slowCheckReadKey struct{}

// slowCheckReads is the document service with the marked request's reads of one document made
// observable and slow. The first such read is the route's own resolve of the check, which runs
// before the transaction opens: it is held there until the test has written the version that moves
// the check's digest, so the route reaches its locks carrying words a version has replaced. Every
// read after that one - the read a moved digest needs - says when it started and then takes
// slowCheckRead, so the test can time a writer of the same issue against it.
type slowCheckReads struct {
	docs.API
	// artifactID is the Verification document; reads of anything else pass straight through.
	artifactID string
	// resolved is closed when the route's resolve reaches the document; release lets it go on.
	resolved chan struct{}
	release  chan struct{}
	// reading is closed when the read a moved digest needs starts.
	reading     chan struct{}
	resolveOnce sync.Once
	readingOnce sync.Once
}

func (d *slowCheckReads) Text(ctx context.Context, artifactID string) (string, error) {
	if artifactID != d.artifactID || ctx.Value(slowCheckReadKey{}) == nil {
		return d.API.Text(ctx, artifactID)
	}
	resolve := false
	d.resolveOnce.Do(func() { resolve = true })
	if resolve {
		close(d.resolved)
		<-d.release
		return d.API.Text(ctx, artifactID)
	}
	d.readingOnce.Do(func() { close(d.reading) })
	time.Sleep(slowCheckRead)
	return d.API.Text(ctx, artifactID)
}

// A results write whose check was reworded between its resolve and its locks reads the check's
// words again - and reads them with the two issues' owner locks given back, never while it holds
// them. A PATCH of the recording issue, made while that read runs, answers at once; before the
// read moved out of the locked window it waited the whole read out, which for a document no room
// holds is a cold read of up to a minute.
//
// The result recorded is still judged by the check as the transaction that writes it sees the
// check: the version the second upload wrote and the words it left, not the ones the resolve read.
func TestAResultsWriteReadsTheChecksWordsWithBothIssuesOwnerLocksFree(t *testing.T) {
	reads := &slowCheckReads{
		resolved: make(chan struct{}),
		release:  make(chan struct{}),
		reading:  make(chan struct{}),
	}
	handler, _ := newInteractionHandler(t, func(database *store.Store) docs.API {
		service := docs.New(docs.Deps{Store: database, Settle: time.Hour})
		t.Cleanup(func() {
			if err := service.Shutdown(context.Background()); err != nil {
				t.Errorf("shutdown document service: %v", err)
			}
		})
		reads.API = service
		return reads
	})
	var releasing sync.Once
	release := func() { releasing.Do(func() { close(reads.release) }) }
	// However this test ends, the held resolve is let go: a failure before the release would
	// otherwise park the measured request's goroutine for the rest of the package's run.
	defer release()

	issue := createInteractionIssue(t, handler, "RESL", "A check reworded under the write", "A spec\n")
	verification := uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", siblingChecks)
	reads.artifactID = verification.Artifact.ID

	marked := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), slowCheckReadKey{}, true)))
	})
	recorded := make(chan *httptest.ResponseRecorder, 1)
	go func() { recorded <- recordResult(t, marked, issue.Key, resultBody("k1", "pass", headCommit, nil)) }()

	// The route has read the digest the index holds for k1 and is reading the document for that
	// digest's words, holding no lock of its own. This version replaces both, so the digest it
	// read is stale before it reaches its locks.
	<-reads.resolved
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", siblingChecksReworded)
	release()

	// The read the moved digest needs has started. A PATCH of the same issue takes that issue's
	// owner lock, the lock the results write takes too: it waits only if the write is holding it
	// across this read.
	<-reads.reading
	started := time.Now()
	patch := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key,
		map[string]any{"labels_add": []string{"waited"}}, "alice")
	waited := time.Since(started)
	if patch.Code != http.StatusOK {
		t.Fatalf("PATCH the issue beside the results write: status=%d body=%s", patch.Code, patch.Body.String())
	}
	if waited > slowCheckRead/2 {
		t.Errorf("a PATCH of %s waited %s for the results write's read of the check's words, want well under %s: the write is holding both issues' owner locks across that read",
			issue.Key, waited.Round(time.Millisecond), slowCheckRead)
	}

	response := <-recorded
	if response.Code != http.StatusCreated {
		t.Fatalf("record the result: status=%d body=%s", response.Code, response.Body.String())
	}
	result := decodeBody[recordedResult](t, response)
	if result.Result.CheckVersion != 2 {
		t.Errorf("the result records the check at version %d, want 2: the version the transaction that wrote it saw", result.Result.CheckVersion)
	}
	if !strings.Contains(result.Line, "named by its block id") {
		t.Errorf("the pull-request line = %q, want the check's words as the version this write saw left them", result.Line)
	}
}

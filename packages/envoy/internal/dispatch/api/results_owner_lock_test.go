package api

import (
	"context"
	"fmt"
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

// slowCheckRead is how long this file's document service takes over the live read the route
// makes once the check's version or digest has moved. It stands in for a cold read's minute, so
// the test measures a fixed number instead of waiting one out, and it is far longer than any
// other statement either request runs.
const slowCheckRead = 2 * time.Second

// checkReadWait bounds every wait this file makes: the handshakes with the held request, and
// the answer to the request itself. Each is a step the route takes on another goroutine, so a
// route that stops taking it - a comparison dropped, a read moved back under the locks - would
// park the test until the package's own timeout and say nothing about what it was waiting for.
// Waiting through this bound names the step that never came instead. It is far longer than any
// step here takes, slowCheckRead included.
const checkReadWait = 30 * time.Second

// awaitCheckRead takes the next value the held request sends, or fails the test saying which
// step of it never came. Every receive in this file goes through it.
func awaitCheckRead[T any](t *testing.T, from <-chan T, what string) T {
	t.Helper()
	select {
	case value := <-from:
		return value
	case <-time.After(checkReadWait):
		t.Fatalf("%s within %s", what, checkReadWait)
		return *new(T)
	}
}

// letCheckRead releases a held read, or fails the test saying which read never asked to be
// released. Every send in this file goes through it.
func letCheckRead[T any](t *testing.T, to chan<- T, value T, what string) {
	t.Helper()
	select {
	case to <- value:
	case <-time.After(checkReadWait):
		t.Fatalf("%s within %s", what, checkReadWait)
	}
}

// slowCheckReadKey marks the one request whose document reads this file's service gates and slows.
// Every other caller of the same service - the uploads that seed the fixture, the PATCH that
// measures the wait - reads at full speed, so only the measured route is held up.
type slowCheckReadKey struct{}

// slowCheckReads is the document service with the marked request's reads of one document made
// observable and slow. The first such read is the route's own resolve of the check, which runs
// before the transaction opens: it is held there until the test has written the version that
// rewords the check, so the route reaches its locks carrying words a version has replaced. Every
// read after that one - the read a moved version or digest needs - says when it started and then
// takes slowCheckRead, so the test can time a writer of the same issue against it.
//
// Its own waits are bounded as the test's are. They run on the request's goroutine, which
// cannot fail the test, so a gate nobody opens ends the read with an error the route answers
// with instead of parking that goroutine for the rest of the package's run.
type slowCheckReads struct {
	docs.API
	// artifactID is the Verification document; reads of anything else pass straight through.
	artifactID string
	// resolved is closed when the route's resolve reaches the document; release lets it go on.
	resolved chan struct{}
	release  chan struct{}
	// reading is closed when the read a moved version or digest needs starts.
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
		if err := awaitCheckGate(d.release); err != nil {
			return "", err
		}
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

	// The route has read the version and digest the index holds for k1 and is reading the
	// document for the words they name, holding no lock of its own. This version moves both, so
	// what it read is stale before it reaches its locks.
	awaitCheckRead(t, reads.resolved, "the route's resolve never reached the check's words")
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", siblingChecksReworded)
	release()

	// The read the moved version or digest needs has started. A PATCH of the same issue takes
	// that issue's owner lock, the lock the results write takes too: it waits only if the write
	// is holding it across this read.
	awaitCheckRead(t, reads.reading, "the results write never re-read the check's words")
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

	response := awaitCheckRead(t, recorded, "the results write never answered")
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

// contendedCheckReads holds the marked request at each of its first two reads of the
// Verification document and lets the test write a version while it waits, so the version and
// digest the route carries into its locks are stale at both of its attempts
// (resolutionAttempts). Only the marked request's reads of that one document are held; every
// other caller reads straight through, which is what lets the test write those versions. The
// read counter is touched by the marked request alone, whose reads run one after another.
//
// Its own waits are bounded as the test's are. They run on the request's goroutine, which
// cannot fail the test, so a handshake the test never makes ends the read with an error the
// route answers with instead of parking that goroutine for the rest of the package's run.
type contendedCheckReads struct {
	docs.API
	// artifactID is the Verification document; reads of anything else pass straight through.
	artifactID string
	// held carries the ordinal of each held read, and let releases it.
	held  chan int
	let   chan struct{}
	reads int
}

func (d *contendedCheckReads) Text(ctx context.Context, artifactID string) (string, error) {
	if artifactID != d.artifactID || ctx.Value(slowCheckReadKey{}) == nil {
		return d.API.Text(ctx, artifactID)
	}
	d.reads++
	if d.reads <= resolutionAttempts {
		if err := offerCheckRead(d.held, d.reads); err != nil {
			return "", err
		}
		if err := awaitCheckGate(d.let); err != nil {
			return "", err
		}
	}
	return d.API.Text(ctx, artifactID)
}

// A results write whose check a document version rewords at each of its two attempts records
// nothing and is refused 409 CHECK_CONTENDED. The refusal carries Retry-After: 1: what the
// caller waits for is the version that moved the check landing, which has landed by the time a
// retry a second later asks, and no budget of its own that a longer wait would refill
// (checkContendedRetryAfter).
func TestAResultsWriteRefusesACheckRewordedAtEveryAttempt(t *testing.T) {
	reads := &contendedCheckReads{held: make(chan int), let: make(chan struct{})}
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

	issue := createInteractionIssue(t, handler, "RESC", "A check reworded at every attempt", "A spec\n")
	verification := uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", siblingChecks)
	reads.artifactID = verification.Artifact.ID

	marked := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), slowCheckReadKey{}, true)))
	})
	recorded := make(chan *httptest.ResponseRecorder, 1)
	go func() { recorded <- recordResult(t, marked, issue.Key, resultBody("k1", "pass", headCommit, nil)) }()

	// Each held read is about to give the route the words of a digest this upload replaces: the
	// first is the route's resolve, the second the re-read it makes once its locks found that
	// digest moved. So the route reaches its locks with stale words both times, and the second
	// time is its last attempt.
	for _, words := range []string{siblingChecksReworded, siblingChecksRewordedAgain} {
		awaitCheckRead(t, reads.held, "the route never reached its next read of the check's words")
		uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", words)
		letCheckRead(t, reads.let, struct{}{}, "the route's held read never took the version that replaced its words")
	}

	response := awaitCheckRead(t, recorded, "the contended results write never answered")
	code, text := refusalOf(t, response)
	if response.Code != http.StatusConflict || code != "CHECK_CONTENDED" {
		t.Fatalf("a check reworded at each attempt: status=%d body=%s, want 409 CHECK_CONTENDED",
			response.Code, response.Body.String())
	}
	if !strings.Contains(text, "k1") {
		t.Errorf("the refusal = %q, want the check nothing was recorded against", text)
	}
	if got := response.Header().Get("Retry-After"); got != "1" {
		t.Errorf("Retry-After = %q, want 1: the caller waits only for the version that moved the check to land", got)
	}
	if results := checkOf(t, readResults(t, handler, issue.Key), "k1").Results; len(results) != 0 {
		t.Errorf("k1 carries %d results, want none: the refusal recorded nothing", len(results))
	}
}

// restoredCheckReads gates the marked request's first read of the Verification document on both
// sides: before it reads, so the test can write the version whose words that read then takes,
// and after it holds them, so the test can write the version that restores the words that
// version replaced. Every later read of the marked request - the one a refused resolution needs
// - goes straight through, as does every other caller's, which is what lets the test write
// those versions.
//
// Its own waits are bounded as the test's are. They run on the request's goroutine, which
// cannot fail the test, so a gate nobody opens ends the read with an error the route answers
// with instead of parking that goroutine for the rest of the package's run.
type restoredCheckReads struct {
	docs.API
	// artifactID is the Verification document; reads of anything else pass straight through.
	artifactID string
	// resolving is closed when the route's resolve reaches the document; moved releases it once
	// the version whose words it is to read has landed.
	resolving chan struct{}
	moved     chan struct{}
	// read is closed once that read holds those words; restored releases it once the version
	// that brings the first digest back has landed.
	read        chan struct{}
	restored    chan struct{}
	resolveOnce sync.Once
}

func (d *restoredCheckReads) Text(ctx context.Context, artifactID string) (string, error) {
	if artifactID != d.artifactID || ctx.Value(slowCheckReadKey{}) == nil {
		return d.API.Text(ctx, artifactID)
	}
	resolve := false
	d.resolveOnce.Do(func() { resolve = true })
	if !resolve {
		return d.API.Text(ctx, artifactID)
	}
	close(d.resolving)
	if err := awaitCheckGate(d.moved); err != nil {
		return "", err
	}
	markdown, err := d.API.Text(ctx, artifactID)
	close(d.read)
	if gate := awaitCheckGate(d.restored); gate != nil {
		return "", gate
	}
	return markdown, err
}

// awaitCheckGate bounds a wait the held read itself makes: it runs on the request's goroutine,
// which cannot fail the test, so the bound ends the read with an error rather than hanging.
func awaitCheckGate(gate <-chan struct{}) error {
	select {
	case <-gate:
		return nil
	case <-time.After(checkReadWait):
		return fmt.Errorf("the test did not release the held read of the check's words within %s", checkReadWait)
	}
}

// offerCheckRead bounds a handshake the held read itself makes, as awaitCheckGate bounds the
// waits: it runs on the request's goroutine, so a value the test never takes ends the read with
// an error rather than hanging.
func offerCheckRead[T any](to chan<- T, value T) error {
	select {
	case to <- value:
		return nil
	case <-time.After(checkReadWait):
		return fmt.Errorf("the test did not take the handshake of the held read of the check's words within %s", checkReadWait)
	}
}

// A check whose words one version replaced and a later version restored is a resolution the
// locks contradict, even though the digest they find is the one it was read against. The index
// row is read at version 1 and its digest; version 2 rewords the check, and the words the route
// reads are version 2's; version 3 restores version 1's words, so under the locks the digest is
// version 1's again while changed_version is 3. Only the version says the words in hand are a
// version this transaction does not see, so the write compares both: it gives the locks back,
// reads the words again against the version and digest it found, and records the result against
// those. Comparing the digest alone records version 2's words as the check the transaction saw,
// under version 3's number - words the document held at neither.
func TestAResultsWriteReadsTheCheckAgainWhenALaterVersionRestoredItsDigest(t *testing.T) {
	reads := &restoredCheckReads{
		resolving: make(chan struct{}),
		moved:     make(chan struct{}),
		read:      make(chan struct{}),
		restored:  make(chan struct{}),
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

	issue := createInteractionIssue(t, handler, "RESA", "A check reworded and restored", "A spec\n")
	verification := uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", siblingChecks)
	reads.artifactID = verification.Artifact.ID

	marked := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), slowCheckReadKey{}, true)))
	})
	recorded := make(chan *httptest.ResponseRecorder, 1)
	go func() { recorded <- recordResult(t, marked, issue.Key, resultBody("k1", "pass", headCommit, nil)) }()

	// The route holds the index row of k1 at version 1 and is about to read the document for
	// that row's words. This version rewords k1, so the words it reads are version 2's.
	awaitCheckRead(t, reads.resolving, "the route's resolve never reached the check's words")
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", siblingChecksReworded)
	close(reads.moved)

	// Version 2's words are in hand. This version restores version 1's, so the digest the locks
	// read is the one the resolve was made against while changed_version has moved to 3.
	awaitCheckRead(t, reads.read, "the route never read the reworded check's words")
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", siblingChecks)
	close(reads.restored)

	response := awaitCheckRead(t, recorded, "the results write never answered")
	if response.Code != http.StatusCreated {
		t.Fatalf("record the result: status=%d body=%s", response.Code, response.Body.String())
	}
	result := decodeBody[recordedResult](t, response)
	if result.Result.CheckVersion != 3 {
		t.Errorf("the result records the check at version %d, want 3: the version the transaction that wrote it saw", result.Result.CheckVersion)
	}
	if strings.Contains(result.Line, "named by its block id") {
		t.Errorf("the pull-request line = %q: it carries the words of the version between the resolve and the locks, whose digest the version under the locks restored, so the write judged the result by a resolution that version had moved past",
			result.Line)
	}
	if !strings.Contains(result.Line, "The server records a result against a check.") {
		t.Errorf("the pull-request line = %q, want the check's words as the version this write saw left them", result.Line)
	}
}

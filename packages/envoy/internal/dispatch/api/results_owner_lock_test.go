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
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The results write takes the owner locks of the recording issue and of the check's issue
// (lockIssueOwners), and those locks serialise every writer of either issue. It also judges the
// result by the check's words, which live in the live Verification document: a read that for a
// document no room holds is a cold read bounded by a minute (docs/read_cache.go). This file holds
// the writer that must not wait behind that read.

// Every wait the test's own goroutine makes on the held request - a handshake, or the request's
// answer - is testwait.Receive, whose one bound is the test binary's deadline less testwait's
// margin: a route that stops taking a step - a comparison dropped, a read moved back under the
// locks - fails the test naming the step that never came, and a step that is merely slow is no
// failure. The held read's own waits run on the request's goroutine, which cannot fail the test,
// and end with the test (awaitCheckGate, offerCheckRead).

// heldCheckReadKey marks the one request whose document reads this file's service holds. Every
// other caller of the same service - the uploads that seed the fixture, the PATCH beside the held
// read - reads straight through, so only the marked route is held.
type heldCheckReadKey struct{}

// heldCheckReads is the document service with the marked request's reads of one document held
// until the test lets each go. The first such read is the route's own resolve of the check, which
// runs before the transaction opens: it is held there until the test has written the version that
// rewords the check, so the route reaches its locks carrying words a version has replaced. The
// read after it - the read the moved version or digest needs - says when it started and is held
// again, so the test can look at the issue's owner lock, and make a writer of the same issue,
// while that read is still under way.
type heldCheckReads struct {
	docs.API
	// artifactID is the Verification document; reads of anything else pass straight through.
	artifactID string
	// stop is closed when the test ends (t.Context), which ends a read the test never let go.
	stop <-chan struct{}
	// resolved is closed when the route's resolve reaches the document; release lets it go on.
	resolved chan struct{}
	release  chan struct{}
	// reading is closed when the read a moved version or digest needs starts; reread lets it go
	// on.
	reading     chan struct{}
	reread      chan struct{}
	resolveOnce sync.Once
	readingOnce sync.Once
}

func (d *heldCheckReads) Text(ctx context.Context, who model.Actor, artifactID string) (string, error) {
	if artifactID != d.artifactID || ctx.Value(heldCheckReadKey{}) == nil {
		return d.API.Text(ctx, who, artifactID)
	}
	resolve := false
	d.resolveOnce.Do(func() { resolve = true })
	if resolve {
		close(d.resolved)
		if err := awaitCheckGate(d.release, d.stop); err != nil {
			return "", err
		}
		return d.API.Text(ctx, who, artifactID)
	}
	reading := false
	d.readingOnce.Do(func() { reading = true })
	if reading {
		close(d.reading)
		if err := awaitCheckGate(d.reread, d.stop); err != nil {
			return "", err
		}
	}
	return d.API.Text(ctx, who, artifactID)
}

// A results write whose check was reworded between its resolve and its locks reads the check's
// words again - and reads them with the two issues' owner locks given back, never while it holds
// them. While that read is held, no transaction holds the issue's owner lock, and a PATCH of the
// recording issue, which takes that lock, answers: the read cannot end until the test lets it go,
// so the PATCH did not wait it out. No clock decides either. Before the read moved out of the
// locked window, a PATCH waited the whole read out, which for a document no room holds is a cold
// read of up to a minute (docs/read_cache.go).
//
// The result recorded is still judged by the check as the transaction that writes it sees the
// check: the version the second upload wrote and the digest it left, not the ones the resolve
// read.
func TestAResultsWriteReadsTheChecksWordsWithBothIssuesOwnerLocksFree(t *testing.T) {
	t.Parallel()
	reads := &heldCheckReads{
		stop:     t.Context().Done(),
		resolved: make(chan struct{}),
		release:  make(chan struct{}),
		reading:  make(chan struct{}),
		reread:   make(chan struct{}),
	}
	handler, database := newInteractionHandler(t, func(database *store.Store) docs.API {
		service := docs.New(docs.Deps{Store: database, Settle: time.Hour})
		t.Cleanup(func() {
			if err := service.Shutdown(context.Background()); err != nil {
				t.Errorf("shutdown document service: %v", err)
			}
		})
		reads.API = service
		return reads
	})
	var releasing, rereading sync.Once
	release := func() { releasing.Do(func() { close(reads.release) }) }
	reread := func() { rereading.Do(func() { close(reads.reread) }) }
	// However this test ends, both held reads are let go at once, so the marked request ends.
	defer release()
	defer reread()

	issue := createInteractionIssue(t, handler, "RESL", "A check reworded under the write", "A spec\n")
	verification := uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", siblingChecks)
	reads.artifactID = verification.Artifact.ID

	marked := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), heldCheckReadKey{}, true)))
	})
	recorded := make(chan *httptest.ResponseRecorder, 1)
	go func() { recorded <- recordResult(t, marked, issue.Key, resultBody("k1", "pass", headCommit, nil)) }()

	// The route has read the version and digest the index holds for k1 and is reading the
	// document for the words they name, holding no lock of its own. This version moves both, so
	// what it read is stale before it reaches its locks.
	testwait.Receive(t, "the route's resolve to reach the check's words", reads.resolved)
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", siblingChecksReworded)
	release()

	// The read the moved version or digest needs has started and is held. The results write takes
	// the owner locks of the issue it records on and of the check's issue, here one issue, so
	// while this read is held no transaction may hold that issue's owner lock.
	testwait.Receive(t, "the results write to read the check's words again", reads.reading)
	if holder := readIssueOwnerLockHolder(t, database, issue.Key); holder != nil {
		t.Fatalf("while the results write reads the check's words again, %s's owner lock is held by %s: the write holds its issues' owner locks across that read",
			issue.Key, holder)
	}
	// A PATCH of the same issue takes that issue's owner lock. It answers only if no one holds it
	// across the held read, which ends only once this test lets it go below.
	patched := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		patched <- dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key,
			map[string]any{"labels_add": []string{"waited"}}, "alice")
	}()
	patch := testwait.Receive(t, "a PATCH of "+issue.Key+" while the results write's read of the check's words is held", patched)
	reread()
	if patch.Code != http.StatusOK {
		t.Fatalf("PATCH the issue beside the results write: status=%d body=%s", patch.Code, patch.Body.String())
	}

	response := testwait.Receive(t, "the results write to answer", recorded)
	if response.Code != http.StatusCreated {
		t.Fatalf("record the result: status=%d body=%s", response.Code, response.Body.String())
	}
	result := decodeBody[recordedResult](t, response)
	if result.Result.CheckVersion != 2 {
		t.Errorf("the result records the check at version %d, want 2: the version the transaction that wrote it saw", result.Result.CheckVersion)
	}
	// The pull-request line carries the role's own reading and not the check's words
	// (checkResultLine), so the reading the result was judged by is read off the row: the digest
	// it records is the one the index holds for k1 now, which the second upload's version wrote.
	if digest := checkOf(t, readResults(t, handler, issue.Key), "k1").SHA256; result.Result.CheckSHA256 != digest {
		t.Errorf("the result records the digest %q, want %q: the check as the version this write saw left it",
			result.Result.CheckSHA256, digest)
	}
}

// contendedCheckReads holds the marked request at each of its first two reads of the
// Verification document and lets the test write a version while it waits, so the version and
// digest the route carries into its locks are stale at both of its attempts
// (resolutionAttempts). Only the marked request's reads of that one document are held; every
// other caller reads straight through, which is what lets the test write those versions. The
// read counter is touched by the marked request alone, whose reads run one after another.
type contendedCheckReads struct {
	docs.API
	// artifactID is the Verification document; reads of anything else pass straight through.
	artifactID string
	// stop is closed when the test ends (t.Context), which ends a read the test never let go.
	stop <-chan struct{}
	// held carries the ordinal of each held read, and let releases it: one send for each, which
	// its buffer takes without waiting for the read.
	held  chan int
	let   chan struct{}
	reads int
}

func (d *contendedCheckReads) Text(ctx context.Context, who model.Actor, artifactID string) (string, error) {
	if artifactID != d.artifactID || ctx.Value(heldCheckReadKey{}) == nil {
		return d.API.Text(ctx, who, artifactID)
	}
	d.reads++
	if d.reads <= resolutionAttempts {
		if err := offerCheckRead(d.held, d.reads, d.stop); err != nil {
			return "", err
		}
		if err := awaitCheckGate(d.let, d.stop); err != nil {
			return "", err
		}
	}
	return d.API.Text(ctx, who, artifactID)
}

// A results write whose check a document version rewords at each of its two attempts records
// nothing and is refused 409 CHECK_CONTENDED. The refusal carries Retry-After: 1: what the
// caller waits for is the version that moved the check landing, which has landed by the time a
// retry a second later asks, and no budget of its own that a longer wait would refill
// (checkContendedRetryAfter).
func TestAResultsWriteRefusesACheckRewordedAtEveryAttempt(t *testing.T) {
	t.Parallel()
	reads := &contendedCheckReads{stop: t.Context().Done(), held: make(chan int), let: make(chan struct{}, resolutionAttempts)}
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
		handler.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), heldCheckReadKey{}, true)))
	})
	recorded := make(chan *httptest.ResponseRecorder, 1)
	go func() { recorded <- recordResult(t, marked, issue.Key, resultBody("k1", "pass", headCommit, nil)) }()

	// Each held read is about to give the route the words of a version this upload replaces: the
	// first is the route's resolve, the second the re-read it makes once its locks found that
	// version and digest moved. So the route reaches its locks with stale words both times, and the
	// second time is its last attempt.
	for _, words := range []string{siblingChecksReworded, siblingChecksRewordedAgain} {
		testwait.Receive(t, "the route to reach its next read of the check's words", reads.held)
		uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", words)
		reads.let <- struct{}{}
	}

	response := testwait.Receive(t, "the contended results write to answer", recorded)
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
type restoredCheckReads struct {
	docs.API
	// artifactID is the Verification document; reads of anything else pass straight through.
	artifactID string
	// stop is closed when the test ends (t.Context), which ends a read the test never let go.
	stop <-chan struct{}
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

func (d *restoredCheckReads) Text(ctx context.Context, who model.Actor, artifactID string) (string, error) {
	if artifactID != d.artifactID || ctx.Value(heldCheckReadKey{}) == nil {
		return d.API.Text(ctx, who, artifactID)
	}
	resolve := false
	d.resolveOnce.Do(func() { resolve = true })
	if !resolve {
		return d.API.Text(ctx, who, artifactID)
	}
	close(d.resolving)
	if err := awaitCheckGate(d.moved, d.stop); err != nil {
		return "", err
	}
	markdown, err := d.API.Text(ctx, who, artifactID)
	close(d.read)
	if gate := awaitCheckGate(d.restored, d.stop); gate != nil {
		return "", gate
	}
	return markdown, err
}

// awaitCheckGate is a wait the held read itself makes: it runs on the request's goroutine, which
// cannot fail the test, so the test's end (stop) ends the read with an error rather than leaving it
// waiting.
func awaitCheckGate(gate, stop <-chan struct{}) error {
	select {
	case <-gate:
		return nil
	case <-stop:
		return fmt.Errorf("the test ended before it let the held read of the check's words go")
	}
}

// offerCheckRead is a handshake the held read itself makes, ended as awaitCheckGate's wait is: it
// runs on the request's goroutine, so a value the test never takes ends the read with an error
// once the test ends (stop).
func offerCheckRead[T any](to chan<- T, value T, stop <-chan struct{}) error {
	select {
	case to <- value:
		return nil
	case <-stop:
		return fmt.Errorf("the test ended before it took the handshake of the held read of the check's words")
	}
}

// A check whose words one version replaced and a later version restored is a resolution the
// locks contradict, even though the digest they find is the one it was read against. The index
// row is read at version 1 and its digest; version 2 rewords the check, and the words the route
// reads are version 2's; version 3 restores version 1's words, so under the locks the digest is
// version 1's again while changed_version is 3. Only the version says the words in hand are a
// version this transaction does not see, so the write compares both: it gives the locks back,
// reads the check again against the version and digest it found, and records the result against
// those. Comparing the digest alone takes the resolution the locks contradict, which records the
// check at version 1 under version 3's digest - the check as a version this transaction cannot
// see.
func TestAResultsWriteReadsTheCheckAgainWhenALaterVersionRestoredItsDigest(t *testing.T) {
	t.Parallel()
	reads := &restoredCheckReads{
		stop:      t.Context().Done(),
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
		handler.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), heldCheckReadKey{}, true)))
	})
	recorded := make(chan *httptest.ResponseRecorder, 1)
	go func() { recorded <- recordResult(t, marked, issue.Key, resultBody("k1", "pass", headCommit, nil)) }()

	// The route holds the index row of k1 at version 1 and is about to read the document for
	// that row's words. This version rewords k1, so the words it reads are version 2's.
	testwait.Receive(t, "the route's resolve to reach the check's words", reads.resolving)
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", siblingChecksReworded)
	close(reads.moved)

	// Version 2's words are in hand. This version restores version 1's, so the digest the locks
	// read is the one the resolve was made against while changed_version has moved to 3.
	testwait.Receive(t, "the route to read the reworded check's words", reads.read)
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", siblingChecks)
	close(reads.restored)

	response := testwait.Receive(t, "the results write to answer", recorded)
	if response.Code != http.StatusCreated {
		t.Fatalf("record the result: status=%d body=%s", response.Code, response.Body.String())
	}
	result := decodeBody[recordedResult](t, response)
	if result.Result.CheckVersion != 3 {
		t.Errorf("the result records the check at version %d, want 3: the version the transaction that wrote it saw", result.Result.CheckVersion)
	}
	// The row names the check as this write saw it: version 3, and the digest version 3 restored,
	// which is the one the index holds for k1 now. The pull-request line says neither, since it
	// carries the role's reading rather than the check's words (checkResultLine).
	if digest := checkOf(t, readResults(t, handler, issue.Key), "k1").SHA256; result.Result.CheckSHA256 != digest {
		t.Errorf("the result records the digest %q, want %q: the check as the version this write saw left it",
			result.Result.CheckSHA256, digest)
	}
}

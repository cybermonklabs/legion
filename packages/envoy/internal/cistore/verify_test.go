package cistore

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/logtest"
)

// verifyHead is the head every test below writes (record_runs_test.go's) as the verify calls name
// it, and verifyRecordKey its record's key.
var (
	verifyHead      = contracts.CIHead{Owner: recordRunsOwner, Repo: recordRunsRepo, Number: recordRunsNumber, SHA: recordRunsSHA}
	verifyRecordKey = Key(recordRunsOwner, recordRunsRepo, recordRunsNumber, recordRunsSHA)
)

// openVerifyingStore is openStore with ENVOY_CI_VERIFY on.
func openVerifyingStore(t *testing.T, conn *natsgo.Conn) *Store {
	t.Helper()
	st, err := Open(t.Context(), conn, logging.New("test"), WithReplicas(1), WithTTL(time.Hour), WithVerify(true))
	if err != nil {
		t.Fatalf("open cistore: %v", err)
	}
	t.Cleanup(func() { forgetTestSummarizer(st) })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := st.WaitForCacheReady(ctx); err != nil {
		t.Fatalf("cache not ready: %v", err)
	}
	return st
}

// dispatchWriter is a writer of the bucket as Dispatch's CI reconcile binds one: a store of its
// own, with no cache.
func dispatchWriter(t *testing.T, conn *natsgo.Conn) *Writer {
	t.Helper()
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	writer, err := OpenWriter(conn, js, logging.New("writer"))
	if err != nil {
		t.Fatalf("open a writer: %v", err)
	}
	return writer
}

// readyHead records one finished check of recordRunsHead through s and waits until every store
// given holds it in its cache, so a summary pass of any of them finds the head ready.
func readyHead(t *testing.T, s *Store, caches ...*Store) {
	t.Helper()
	if err := s.Record(context.Background(), recordRunsCheck("ci-ok", 1, "completed", "success", "2026-11-22T00:05:00Z")); err != nil {
		t.Fatalf("record: %v", err)
	}
	for _, cache := range append([]*Store{s}, caches...) {
		waitCacheChecks(t, cache, recordRunsOwner, recordRunsRepo, recordRunsNumber, recordRunsSHA, 1)
	}
}

// waiting is the verify wait z holds for recordRunsHead, and whether it holds one.
func waiting(z *summarizer) (verifyWait, bool) {
	z.mu.Lock()
	defer z.mu.Unlock()
	wait, ok := z.verifying[verifyRecordKey]
	return wait, ok
}

// asked is the revision of the verify request z's wait for recordRunsHead carries, which must
// have been written.
func asked(t *testing.T, z *summarizer) uint64 {
	t.Helper()
	wait, ok := waiting(z)
	if !ok || wait.requested == 0 {
		t.Fatalf("the summary loop holds no written verify request of the head: %+v (held %v)", wait, ok)
	}
	return wait.requested
}

// answer writes revision as the head's VerifiedRev through writer, as the reconcile does once it
// has read the head, and waits until each store's cache holds the answer.
func answer(t *testing.T, writer *Writer, revision uint64, caches ...*Store) {
	t.Helper()
	if _, err := writer.MarkVerified(context.Background(), verifyHead, revision); err != nil {
		t.Fatalf("mark verified at %d: %v", revision, err)
	}
	waitHeadCached(t, func(st State) bool { return st.VerifiedRev >= revision }, caches...)
}

// waitHeadCached polls each store's cached record of recordRunsHead until done holds of it.
func waitHeadCached(t *testing.T, done func(State) bool, caches ...*Store) {
	t.Helper()
	key := verifyRecordKey
	for _, cache := range caches {
		deadline := time.Now().Add(5 * time.Second)
		for {
			if st, ok := cache.cachedState(key); ok && done(st) {
				break
			}
			if time.Now().After(deadline) {
				st, _ := cache.cachedState(key)
				t.Fatalf("the cache never held the record wanted: %+v", st)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
}

// A verify read that began before the request -- the reconcile read the request's key at a
// revision below the listener's request -- authorizes nothing, though the record is unchanged. An
// answer at the request's own revision publishes.
func TestAVerifyReadThatBeganBeforeTheRequestAuthorizesNothing(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	s := openVerifyingStore(t, conn)
	writer := dispatchWriter(t, conn)
	readyHead(t, s)
	z, pub := testSummarizer(s, 0), &recPub{}

	z.pass(pub, logging.New("test"), time.Now().UnixMilli())
	requested := asked(t, z)
	if pub.count() != 0 {
		t.Fatalf("published %d settlements before any answer", pub.count())
	}

	answer(t, writer, requested-1, s)
	z.pass(pub, logging.New("test"), time.Now().UnixMilli())
	if pub.count() != 0 {
		t.Fatalf("an answer read at revision %d, before the request at %d, published %d settlements", requested-1, requested, pub.count())
	}

	answer(t, writer, requested, s)
	z.pass(pub, logging.New("test"), time.Now().UnixMilli())
	if pub.count() != 1 {
		t.Fatalf("an answer at the request's revision published %d settlements, want 1", pub.count())
	}
}

// The reconcile read the request at its old revision before the listener wrote a newer one; its
// answer's compare-and-swap then lost to the listener's own write of the record (the claim's
// refresh as it wrote the newer request), and the retry wrote after the newer request existed.
// The retry keeps the revision the reconcile read first, so it authorizes nothing; an answer at the
// newer request's revision publishes.
func TestARetriedAnswerKeepsTheRevisionItReadFirst(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	s := openVerifyingStore(t, conn)
	writer := dispatchWriter(t, conn)
	readyHead(t, s)
	z, pub := testSummarizer(s, 0), &recPub{}
	z.pass(pub, logging.New("test"), time.Now().UnixMilli())
	read := asked(t, z)

	var once sync.Once
	attempts := 0
	writer.store.casHook = func(string) {
		attempts++
		once.Do(func() {
			// The listener's request goes unanswered past its timeout: it refreshes its claim, a
			// write of the record that the answer's first compare-and-swap loses to, and writes
			// the request again, past the revision the reconcile read.
			z.verifyTimeout = time.Millisecond
			z.pass(pub, logging.New("test"), time.Now().Add(time.Second).UnixMilli())
			z.verifyTimeout = verifyTimeout
		})
	}
	if _, err := writer.MarkVerified(context.Background(), verifyHead, read); err != nil {
		t.Fatalf("mark verified: %v", err)
	}
	writer.store.casHook = nil
	if attempts < 2 {
		t.Fatalf("the answer was written in %d attempts; the test needs its first to lose", attempts)
	}
	newer := asked(t, z)
	if newer <= read {
		t.Fatalf("the listener's request after its timeout is at %d, not past the one read at %d", newer, read)
	}
	st, _ := recordRunsStored(t, s)
	if st.VerifiedRev != read {
		t.Fatalf("the retried answer wrote VerifiedRev %d, want the revision read first, %d", st.VerifiedRev, read)
	}

	waitHeadCached(t, func(st State) bool { return st.VerifiedRev == read && st.Claim != nil }, s)
	z.pass(pub, logging.New("test"), time.Now().Add(time.Second).UnixMilli())
	if pub.count() != 0 {
		t.Fatalf("an answer read before the newer request published %d settlements", pub.count())
	}

	answer(t, writer, asked(t, z), s)
	z.pass(pub, logging.New("test"), time.Now().Add(time.Second).UnixMilli())
	if pub.count() != 1 {
		t.Fatalf("an answer at the newer request's revision published %d settlements, want 1", pub.count())
	}
}

// A request unanswered for the timeout is written again, and the new write's revision replaces the
// old one: an answer to the old request does not reach the new one, and an answer to the new one
// publishes.
func TestARequestWrittenAgainIsNotReachedByAnAnswerToTheOldOne(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	s := openVerifyingStore(t, conn)
	writer := dispatchWriter(t, conn)
	readyHead(t, s)
	z, pub := testSummarizer(s, 0), &recPub{}
	z.verifyTimeout = 50 * time.Millisecond
	start := time.Now()
	z.pass(pub, logging.New("test"), start.UnixMilli())
	old := asked(t, z)

	z.pass(pub, logging.New("test"), start.Add(100*time.Millisecond).UnixMilli())
	again := asked(t, z)
	if again <= old {
		t.Fatalf("the request written again is at %d, not past the first at %d", again, old)
	}

	answer(t, writer, old, s)
	z.pass(pub, logging.New("test"), start.Add(110*time.Millisecond).UnixMilli())
	if pub.count() != 0 {
		t.Fatalf("an answer to the first request published %d settlements after it was written again", pub.count())
	}

	answer(t, writer, asked(t, z), s)
	z.pass(pub, logging.New("test"), start.Add(120*time.Millisecond).UnixMilli())
	if pub.count() != 1 {
		t.Fatalf("an answer to the request written again published %d settlements, want 1", pub.count())
	}
}

// The reconcile deletes a request it answered only where the key still holds it at the revision it
// answered: a newer request written while it read GitHub stays for its next read.
func TestDoneVerifyingLeavesANewerRequest(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	s := openVerifyingStore(t, conn)
	writer := dispatchWriter(t, conn)
	readyHead(t, s)
	st, _ := recordRunsStored(t, s)
	answered, err := s.requestVerify(context.Background(), st)
	if err != nil {
		t.Fatalf("write the request: %v", err)
	}
	newer, err := s.requestVerify(context.Background(), st)
	if err != nil {
		t.Fatalf("write the newer request: %v", err)
	}

	if err := writer.DoneVerifying(context.Background(), verifyHead, answered); err != nil {
		t.Fatalf("done verifying the answered request: %v", err)
	}
	requests, err := writer.VerifyRequests(context.Background())
	if err != nil {
		t.Fatalf("list the requests: %v", err)
	}
	if len(requests) != 1 || requests[0].Revision != newer || requests[0].Head != verifyHead {
		t.Fatalf("the requests after the answered one's delete are %+v, want the newer one at %d", requests, newer)
	}

	if err := writer.DoneVerifying(context.Background(), verifyHead, newer); err != nil {
		t.Fatalf("done verifying the newer request: %v", err)
	}
	if requests, err = writer.VerifyRequests(context.Background()); err != nil || len(requests) != 0 {
		t.Fatalf("the requests after both were answered are %+v (%v), want none", requests, err)
	}
}

// An answer that arrives for a claim another replica has reclaimed publishes nothing: neither its
// former holder nor the new episode the record's next claim starts, which writes a request of its
// own that the old answer does not reach. An answer to that request publishes.
func TestAnAnswerForAReclaimedClaimPublishesNothing(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	s := openVerifyingStore(t, conn)
	writer := dispatchWriter(t, conn)
	readyHead(t, s)
	z, pub := testSummarizer(s, 0), &recPub{}
	z.pass(pub, logging.New("test"), time.Now().UnixMilli())
	old := asked(t, z)
	claimed, _ := recordRunsStored(t, s)

	if reclaimed, err := s.ReclaimSettlement(verifyRecordKey, claimed.Claim.Generation, time.Now().Add(time.Hour).UnixMilli()); err != nil || !reclaimed {
		t.Fatalf("reclaim the claim = %v, %v; want reclaimed", reclaimed, err)
	}
	answer(t, writer, old, s)

	z.pass(pub, logging.New("test"), time.Now().UnixMilli())
	if pub.count() != 0 {
		t.Fatalf("the answer for the reclaimed claim published %d settlements", pub.count())
	}
	renewed := asked(t, z)
	if renewed <= old {
		t.Fatalf("the record's next claim asked at %d, not past the old request at %d", renewed, old)
	}
	if st, _ := recordRunsStored(t, s); st.Claim == nil || st.Claim.Generation == claimed.Claim.Generation {
		t.Fatalf("the record holds claim %+v, want a new claim past generation %d", st.Claim, claimed.Claim.Generation)
	}

	answer(t, writer, renewed, s)
	z.pass(pub, logging.New("test"), time.Now().UnixMilli())
	if pub.count() != 1 {
		t.Fatalf("an answer to the new claim's request published %d settlements, want 1", pub.count())
	}
}

// Two listeners ask for one head: listener A claims and asks; A's claim outlives its window, so B
// reclaims the head, claims it and asks. One read, at B's newer request, serves both requests, and
// only B, whose claim the record carries, publishes.
func TestTwoListenersShareOneReadAndOnlyTheClaimHolderPublishes(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	a, b := openVerifyingStore(t, conn), openVerifyingStore(t, conn)
	writer := dispatchWriter(t, conn)
	readyHead(t, a, b)
	za, pubA := testSummarizer(a, 0), &recPub{}
	zb, pubB := testSummarizer(b, 0), &recPub{}

	za.pass(pubA, logging.New("a"), time.Now().UnixMilli())
	requestA := asked(t, za)
	waitHeadCached(t, func(st State) bool { return st.Claim != nil }, b)

	// Past A's claim window, B reclaims the claim, and at its next pass claims the head itself.
	later := time.Now().Add(verifyClaimWindow + time.Minute)
	zb.pass(pubB, logging.New("b"), later.UnixMilli())
	waitHeadCached(t, func(st State) bool { return st.Claim == nil }, b)
	zb.pass(pubB, logging.New("b"), later.UnixMilli())
	requestB := asked(t, zb)
	if requestB <= requestA {
		t.Fatalf("B asked at %d, not past A's request at %d", requestB, requestA)
	}

	answer(t, writer, requestB, a, b)
	za.pass(pubA, logging.New("a"), time.Now().UnixMilli())
	// B claimed at the real time its second pass ran, so it publishes at the real time too.
	zb.pass(pubB, logging.New("b"), time.Now().UnixMilli())

	if pubA.count() != 0 || pubB.count() != 1 {
		t.Fatalf("A published %d and B %d settlements; want B alone, under its own claim, once", pubA.count(), pubB.count())
	}
}

// With verify on and no reconcile answering, nothing is published: each rewrite of the unanswered
// request logs a WARN, and an ERROR once the wait passes the alarm. The answer, when it comes,
// publishes.
func TestWithNoAnswerNothingIsPublishedTheWaitIsLoggedAndAnAnswerPublishes(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	s := openVerifyingStore(t, conn)
	writer := dispatchWriter(t, conn)
	readyHead(t, s)
	z, pub := testSummarizer(s, 0), &recPub{}
	z.verifyTimeout, z.verifyAlarm = 50*time.Millisecond, 150*time.Millisecond
	logs := &logtest.Buffer{}
	logger := logging.NewWithWriter("test", logs)

	start := time.Now()
	for _, after := range []time.Duration{0, 60 * time.Millisecond, 120 * time.Millisecond, 180 * time.Millisecond, 240 * time.Millisecond} {
		z.pass(pub, logger, start.Add(after).UnixMilli())
		if pub.count() != 0 {
			t.Fatalf("published %d settlements %s after the request with no answer", pub.count(), after)
		}
	}
	const unanswered = `"msg":"checks verify request unanswered; the head is not announced until Dispatch answers"`
	var warns, errorsLogged int
	for _, line := range strings.Split(logs.String(), "\n") {
		if !strings.Contains(line, unanswered) {
			continue
		}
		switch {
		case strings.Contains(line, `"level":"WARN"`):
			warns++
		case strings.Contains(line, `"level":"ERROR"`):
			errorsLogged++
		}
	}
	if warns == 0 || errorsLogged == 0 {
		t.Fatalf("the unanswered wait logged %d WARN and %d ERROR lines, want both:\n%s", warns, errorsLogged, logs.String())
	}

	answer(t, writer, asked(t, z), s)
	z.pass(pub, logger, start.Add(250*time.Millisecond).UnixMilli())
	if pub.count() != 1 {
		t.Fatalf("the answer published %d settlements, want 1", pub.count())
	}
}

// A claim that has run out its life, which no replica has reclaimed yet, publishes nothing on the
// answer to its old request: the listener claimed and asked, stopped past the claim's window,
// the answer landed meanwhile, and the listener resumed before anything reclaimed the claim.
// The authorization point refuses the same claim and answer that it accepts while the claim is
// live (the control), and an answer to the request of the record's next claim publishes.
func TestAnExpiredClaimIsNotPublishedOnTheAnswerToItsOldRequest(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	s := openVerifyingStore(t, conn)
	writer := dispatchWriter(t, conn)
	readyHead(t, s)
	z, pub := testSummarizer(s, 0), &recPub{}
	z.pass(pub, logging.New("test"), time.Now().UnixMilli())
	old := asked(t, z)
	answer(t, writer, old, s)
	claimed, _ := recordRunsStored(t, s)
	resumed := time.Now().Add(verifyClaimWindow + time.Minute)

	if held, err := s.ClaimStillHeld(verifyRecordKey, claimed.Generation, claimed.Hash(), old, time.Now().UnixMilli(), 0); err != nil || !held {
		t.Fatalf("the authorization point refused a live claim with its answer: %v, %v", held, err)
	}
	if held, err := s.ClaimStillHeld(verifyRecordKey, claimed.Generation, claimed.Hash(), old, resumed.UnixMilli(), 0); err != nil || held {
		t.Fatalf("the authorization point accepted a claim past its window: %v, %v", held, err)
	}

	z.pass(pub, logging.New("test"), resumed.UnixMilli())
	if pub.count() != 0 {
		t.Fatalf("an expired claim published %d settlements on old request %d without a new request", pub.count(), old)
	}

	// The pass that found the claim past its window reclaimed it; the next claims the head anew.
	waitHeadCached(t, func(st State) bool { return st.Claim == nil }, s)
	z.pass(pub, logging.New("test"), time.Now().UnixMilli())
	renewed := asked(t, z)
	if renewed <= old || pub.count() != 0 {
		t.Fatalf("after the expiry the head asked at %d (old %d) and published %d settlements; want a new request and none", renewed, old, pub.count())
	}
	answer(t, writer, renewed, s)
	z.pass(pub, logging.New("test"), time.Now().UnixMilli())
	if pub.count() != 1 {
		t.Fatalf("an answer to the new request published %d settlements, want 1", pub.count())
	}
}

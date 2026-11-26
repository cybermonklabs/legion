package cistore

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/clock"
	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
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
	return openVerifyingStoreWithTTL(t, conn, time.Hour)
}

// openVerifyingStoreWithTTL is openVerifyingStore with the bucket's expiry ttl.
func openVerifyingStoreWithTTL(t *testing.T, conn *natsgo.Conn, ttl time.Duration) *Store {
	t.Helper()
	return openStore(t, conn, WithTTL(ttl), WithVerify(true))
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
// has read the head, in the stream the bucket is held in now, and waits until each store's cache
// holds the answer.
func answer(t *testing.T, writer *Writer, revision uint64, caches ...*Store) {
	t.Helper()
	if _, err := writer.MarkVerified(context.Background(), verifyHead, revision, streamOf(t, writer)); err != nil {
		t.Fatalf("mark verified at %d: %v", revision, err)
	}
	waitHeadCached(t, func(st State) bool { return st.VerifiedRev >= revision }, caches...)
}

// streamOf is the identity of the stream the bucket writer writes is held in now.
func streamOf(t *testing.T, writer *Writer) int64 {
	t.Helper()
	stream, err := streamIdentity(context.Background(), writer.store.kv())
	if err != nil {
		t.Fatalf("read the bucket's stream: %v", err)
	}
	return stream
}

// waitHeadCached waits until each store's cached record of recordRunsHead is one done holds of.
// The give-up names the record the cache held last, or that it held none.
func waitHeadCached(t *testing.T, done func(State) bool, caches ...*Store) {
	t.Helper()
	for _, cache := range caches {
		testwait.Until(t, "the store's cache to hold the record of the head wanted", func() (string, bool) {
			st, ok := cache.cachedState(verifyRecordKey)
			if !ok {
				return "no record of the head", false
			}
			return fmt.Sprintf("%+v", st), done(st)
		})
	}
}

// A verify read that began before the request -- the reconcile read the request's key at a
// revision below the listener's request -- authorizes nothing, though the record is unchanged. An
// answer at the request's own revision publishes.
//
// Every pass of this row and the rows below runs at one moment the row holds, or at an offset
// from it the row names, rather than at the wall clock's: the request is asked again at
// verifyTimeout (two minutes) and a claim authorizes nothing in its last claimMargin, three and a
// half minutes after it was taken, which a slow row would otherwise reach between its passes. The
// store stamps its claims on the real clock, at or after the held moment, so they are live at it.
func TestAVerifyReadThatBeganBeforeTheRequestAuthorizesNothing(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	s := openVerifyingStore(t, conn)
	writer := dispatchWriter(t, conn)
	readyHead(t, s)
	z, pub := testSummarizer(s, 0), &recPub{}
	now := time.Now().UnixMilli()

	z.pass(pub, logging.New("test"), now)
	requested := asked(t, z)
	if pub.count() != 0 {
		t.Fatalf("published %d settlements before any answer", pub.count())
	}

	answer(t, writer, requested-1, s)
	z.pass(pub, logging.New("test"), now)
	if pub.count() != 0 {
		t.Fatalf("an answer read at revision %d, before the request at %d, published %d settlements", requested-1, requested, pub.count())
	}

	answer(t, writer, requested, s)
	z.pass(pub, logging.New("test"), now)
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
	now := time.Now()
	z.pass(pub, logging.New("test"), now.UnixMilli())
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
			z.pass(pub, logging.New("test"), now.Add(time.Second).UnixMilli())
			z.verifyTimeout = verifyTimeout
		})
	}
	if _, err := writer.MarkVerified(context.Background(), verifyHead, read, streamOf(t, writer)); err != nil {
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
	z.pass(pub, logging.New("test"), now.Add(time.Second).UnixMilli())
	if pub.count() != 0 {
		t.Fatalf("an answer read before the newer request published %d settlements", pub.count())
	}

	answer(t, writer, asked(t, z), s)
	z.pass(pub, logging.New("test"), now.Add(time.Second).UnixMilli())
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
	answered, err := s.requestVerify(context.Background(), st, st.Generation)
	if err != nil {
		t.Fatalf("write the request: %v", err)
	}
	newer, err := s.requestVerify(context.Background(), st, st.Generation)
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
	now := time.Now()
	z.pass(pub, logging.New("test"), now.UnixMilli())
	old := asked(t, z)
	claimed, _ := recordRunsStored(t, s)

	if reclaimed, err := s.ReclaimSettlement(verifyRecordKey, claimed.Claim.Generation, now.Add(time.Hour).UnixMilli()); err != nil || !reclaimed {
		t.Fatalf("reclaim the claim = %v, %v; want reclaimed", reclaimed, err)
	}
	answer(t, writer, old, s)

	z.pass(pub, logging.New("test"), now.UnixMilli())
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
	z.pass(pub, logging.New("test"), now.UnixMilli())
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

	now := time.Now()
	za.pass(pubA, logging.New("a"), now.UnixMilli())
	requestA := asked(t, za)
	waitHeadCached(t, func(st State) bool { return st.Claim != nil }, b)

	// Past A's claim window, B reclaims the claim, and at its next pass claims the head itself.
	// The window is read from the moment A's store stamped the claim, so B's pass is past it
	// however long after the held moment that was.
	claimedA, _ := recordRunsStored(t, a)
	later := time.UnixMilli(claimedA.Claim.ClaimedAt).Add(verifyClaimWindow + time.Minute)
	zb.pass(pubB, logging.New("b"), later.UnixMilli())
	waitHeadCached(t, func(st State) bool { return st.Claim == nil }, b)
	zb.pass(pubB, logging.New("b"), later.UnixMilli())
	requestB := asked(t, zb)
	if requestB <= requestA {
		t.Fatalf("B asked at %d, not past A's request at %d", requestB, requestA)
	}

	answer(t, writer, requestB, a, b)
	za.pass(pubA, logging.New("a"), now.UnixMilli())
	// B's store stamped its claim on the real clock as its second pass ran, at or after the held
	// moment, so it publishes at that moment too.
	zb.pass(pubB, logging.New("b"), now.UnixMilli())

	if pubA.count() != 0 || pubB.count() != 1 {
		t.Fatalf("A published %d and B %d settlements; want B alone, under its own claim, once", pubA.count(), pubB.count())
	}
}

// With verify on and no reconcile answering, nothing is published, and the wait ends: each rewrite
// of the unanswered request logs a WARN, and after verifyMaxAsks requests the episode is given up,
// with one ERROR naming the head and why, the request deleted, the claim let go and a backoff set.
// The head is not announced then, even when an answer to its last request lands, and a new
// observation of it starts another episode at once, within the backoff, whose answer publishes.
func TestAnUnansweredEpisodeIsGivenUpAndANewObservationAsksAgainAtOnce(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	s := openVerifyingStore(t, conn)
	writer := dispatchWriter(t, conn)
	readyHead(t, s)
	z, pub := testSummarizer(s, 0), &recPub{}
	z.verifyTimeout = 50 * time.Millisecond
	logs := &logtest.Buffer{}
	logger := logging.NewWithWriter("test", logs)

	start := time.Now()
	var last uint64
	for i := 0; i <= verifyMaxAsks; i++ {
		z.pass(pub, logger, start.Add(time.Duration(i)*60*time.Millisecond).UnixMilli())
		if pub.count() != 0 {
			t.Fatalf("published %d settlements at pass %d with no answer", pub.count(), i)
		}
		if wait, ok := waiting(z); ok {
			last = wait.requested
		}
	}
	const (
		unanswered = `"msg":"checks verify request unanswered; the head is not announced until Dispatch answers"`
		gaveUp     = `"msg":"` + verifyGaveUp + `"`
	)
	warns, errorsLogged := strings.Count(logs.String(), unanswered), strings.Count(logs.String(), gaveUp)
	if warns != verifyMaxAsks-1 || errorsLogged != 1 || !strings.Contains(logs.String(), `"level":"ERROR"`) {
		t.Fatalf("the unanswered episode logged %d WARN lines and %d give-ups, want %d and 1:\n%s", warns, errorsLogged, verifyMaxAsks-1, logs.String())
	}
	if _, ok := waiting(z); ok {
		t.Fatal("the summary loop still waits on an episode it gave up")
	}
	if requests, err := writer.VerifyRequests(context.Background()); err != nil || len(requests) != 0 {
		t.Fatalf("the requests after the give-up are %+v (%v), want none", requests, err)
	}
	// The passes below run at one moment held after the give-up (the summary loop's own clock
	// otherwise being the wall clock), as the rows above hold theirs.
	then := time.Now().UnixMilli()
	if st, _ := recordRunsStored(t, s); st.Claim != nil || st.VerifyStopped != st.Hash() || st.VerifyStops != 1 || st.VerifyRetryAt <= then {
		t.Fatalf("the record after the give-up is %+v, want no claim and the episode marked stopped, once, with its backoff ahead", st)
	}

	answer(t, writer, last, s)
	z.pass(pub, logger, then)
	if pub.count() != 0 {
		t.Fatalf("an answer to the given-up episode's last request published %d settlements", pub.count())
	}
	if wait, ok := waiting(z); ok {
		t.Fatalf("the summary loop claimed the given-up episode again and waits on request %d", wait.requested)
	}
	if st, _ := recordRunsStored(t, s); st.Claim != nil {
		t.Fatalf("the given-up episode was claimed again: %+v", st.Claim)
	}

	// A new observation of the head starts a new episode: a new claim, a new request, and its
	// answer publishes. The observation is stamped on the store's real clock, after the moment
	// held above, so its passes run at one moment held after it: a pass before an observation's
	// stamp finds the head inside its debounce.
	if err := s.Record(context.Background(), recordRunsCheck("lint", 2, "completed", "success", "2026-11-22T00:06:00Z")); err != nil {
		t.Fatalf("record: %v", err)
	}
	waitCacheChecks(t, s, recordRunsOwner, recordRunsRepo, recordRunsNumber, recordRunsSHA, 2)
	observed := time.Now().UnixMilli()
	z.pass(pub, logger, observed)
	renewed := asked(t, z)
	if renewed <= last {
		t.Fatalf("the new episode asked at %d, not past the given-up one's last request at %d", renewed, last)
	}
	answer(t, writer, renewed, s)
	z.pass(pub, logger, observed)
	if pub.count() != 1 {
		t.Fatalf("the new episode's answer published %d settlements, want 1", pub.count())
	}
}

// A wait whose authorization read failed, with its claim still held, is kept and looked at again
// after authorizationRetry rather than at its request's timeout: here the listener's own
// connection closes after the answer reached its cache, so the fresh read at the authorization
// point fails. Nothing is published, and the claim is kept, so no replica's claim is lost to it.
func TestAFailedAuthorizationReadKeepsTheWaitDueWithinASecond(t *testing.T) {
	url := testnats.URL(t)
	writerConn := testnats.Connect(t, url)
	defer writerConn.Close()
	storeConn := testnats.Connect(t, url)
	defer storeConn.Close()
	s := openVerifyingStore(t, storeConn)
	writer := dispatchWriter(t, writerConn)
	readyHead(t, s)
	z, pub := testSummarizer(s, 0), &recPub{}
	now := time.Now().UnixMilli()
	z.pass(pub, logging.New("test"), now)
	answer(t, writer, asked(t, z), s)
	storeConn.Close()

	z.pass(pub, logging.New("test"), now)
	if pub.count() != 0 {
		t.Fatalf("published %d settlements on an authorization read that failed", pub.count())
	}
	wait, ok := waiting(z)
	if !ok {
		t.Fatal("the summary loop dropped its wait on an authorization read that failed")
	}
	if want := now + authorizationRetry.Milliseconds(); wait.retryAt != want || wait.dueAt(z.verifyTimeout) != want {
		t.Fatalf("the wait is due at %d (retry %d), want %d, a second after the failed read", wait.dueAt(z.verifyTimeout), wait.retryAt, want)
	}
	if st, cached := s.cachedState(verifyRecordKey); !cached || st.Claim == nil {
		t.Fatalf("the claim is gone after an authorization read that failed: %+v", st)
	}
}

// A record's VerifiedRev compares only with revisions of the stream of the bucket it was read in.
// A verify pass lists a request in the old bucket, at a revision past the requests the new bucket
// will number; the bucket is deleted and created again, the head recorded and claimed again, and
// the listener asks at a small revision. The old pass's answer is refused (MarkVerified's stream
// check). Two stamps that land anyway -- a recreate between an answer's stream read and its write
// -- are each refused at the authorization point by one guard alone: the old bucket's revision
// under the new bucket's identity by its revision past the record's own, and the new request's
// revision under the old bucket's identity by its stream. At the request's timeout the listener
// asks again rather than publishing, and an answer read in the new bucket publishes.
func TestAStampReadInABucketSinceCreatedAgainAuthorizesNothing(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	ctx := context.Background()
	s := openVerifyingStore(t, conn)
	writer := dispatchWriter(t, conn)
	for i := range 40 {
		// Another head, its run still queued, so it never becomes ready and asks for nothing:
		// its writes only move the old bucket's revisions on.
		other := recordRunsCheck("lint", uint64(i+1), "queued", "", "2026-11-22T00:05:00Z")
		other.SHA = "1111111111111111111111111111111111111111"
		if err := s.Record(ctx, other); err != nil {
			t.Fatalf("record another head: %v", err)
		}
	}
	readyHead(t, s)
	z, pub := testSummarizer(s, 0), &recPub{}
	logger := logging.New("test")
	z.pass(pub, logger, time.Now().UnixMilli())
	requests, err := writer.VerifyRequests(ctx)
	if err != nil || len(requests) != 1 {
		t.Fatalf("the old bucket's requests are %+v (%v), want the head's one", requests, err)
	}
	stale := requests[0]

	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	if err := js.DeleteKeyValue(bus.CIStateBucket); err != nil {
		t.Fatalf("delete the bucket: %v", err)
	}
	testnats.RecreateKeyValue(t, js, &natsgo.KeyValueConfig{Bucket: bus.CIStateBucket, Replicas: 1, Storage: natsgo.FileStorage, TTL: time.Hour})
	if err := s.Rewatch(ctx, conn); err != nil {
		t.Fatalf("rewatch: %v", err)
	}
	readyHead(t, s)
	// The copy keeps the old bucket's record, claimed, until the new read delivers the new one,
	// which carries no claim: the pass must see the new record and the read's reset together.
	waitHeadCached(t, func(st State) bool { return st.Claim == nil }, s)
	// The passes from here run at one moment held after the new bucket's observation, which the
	// store stamped on its real clock: a pass before that stamp finds the head inside its
	// debounce.
	now := time.Now()
	z.pass(pub, logger, now.UnixMilli())
	fresh := asked(t, z)
	if stale.Revision <= fresh {
		t.Fatalf("the old bucket's request is at %d, not past the new bucket's at %d: the test needs it past", stale.Revision, fresh)
	}
	current := streamOf(t, writer)
	if current == stale.Stream {
		t.Fatalf("the recreated bucket kept the old stream's identity %d", current)
	}

	if _, err := writer.MarkVerified(ctx, verifyHead, stale.Revision, stale.Stream); err == nil {
		t.Fatalf("an answer read at %d in the old bucket was written into the new one", stale.Revision)
	}
	if st, _ := recordRunsStored(t, s); st.VerifiedRev != 0 {
		t.Fatalf("the new bucket's record holds VerifiedRev %d after a refused answer", st.VerifiedRev)
	}

	for _, stamp := range []struct {
		name     string
		revision uint64
		stream   int64
	}{
		{name: "the old bucket's revision under the new bucket's identity", revision: stale.Revision, stream: current},
		{name: "the new request's revision under the old bucket's identity", revision: fresh, stream: stale.Stream},
	} {
		if _, err := s.update(ctx, recordRunsOwner, recordRunsRepo, recordRunsNumber, recordRunsSHA, "", func(st *State) bool {
			st.VerifiedRev, st.VerifiedStream = stamp.revision, stamp.stream
			return true
		}); err != nil {
			t.Fatalf("%s: land the stamp: %v", stamp.name, err)
		}
		waitHeadCached(t, func(st State) bool { return st.VerifiedRev == stamp.revision && st.VerifiedStream == stamp.stream }, s)
		stamped, _ := recordRunsStored(t, s)
		if held, err := s.ClaimStillHeld(verifyRecordKey, stamped.Generation, stamped.Hash(), fresh, now.UnixMilli(), 0); err != nil || held {
			t.Fatalf("%s: the authorization point accepted request %d on stamp %d of stream %d: %v, %v", stamp.name, fresh, stamp.revision, stamp.stream, held, err)
		}
	}
	z.verifyTimeout = 50 * time.Millisecond
	z.pass(pub, logger, now.Add(time.Second).UnixMilli())
	if pub.count() != 0 {
		t.Fatalf("a stamp of the old bucket published %d settlements for request %d", pub.count(), fresh)
	}
	again := asked(t, z)
	if again <= fresh {
		t.Fatalf("at its timeout the listener asked at %d, not past %d", again, fresh)
	}

	answer(t, writer, again, s)
	z.pass(pub, logger, now.Add(time.Second).UnixMilli())
	if pub.count() != 1 {
		t.Fatalf("an answer read in the new bucket published %d settlements, want 1", pub.count())
	}
}

// A batch of a head's runs that loses its compare-and-swap to a write landing between its read
// and its write applies every rule again to the record it reads anew: a newer run of a name that
// landed meanwhile is not replaced by the batch's older one, and the batch's other runs land.
func TestRecordRunsAppliesEveryRuleAgainAfterLosingItsCompareAndSwap(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	ctx := context.Background()
	s := openStore(t, conn)
	writer := dispatchWriter(t, conn)
	if err := s.Record(ctx, recordRunsCheck("ci-ok", 5, "completed", "failure", "2026-11-22T00:05:00Z")); err != nil {
		t.Fatalf("record: %v", err)
	}
	attempts := 0
	writer.store.casHook = func(string) {
		attempts++
		if attempts == 1 {
			if err := s.Record(ctx, recordRunsCheck("ci-ok", 9, "completed", "success", "2026-11-22T00:09:00Z")); err != nil {
				t.Errorf("the write that lands between the batch's read and its write: %v", err)
			}
		}
	}
	_, err := writer.RecordRuns(ctx, []contracts.CIObservation{
		recordRunsCheck("ci-ok", 7, "queued", "", "2026-11-22T00:07:00Z"),
		recordRunsCheck("lint", 11, "completed", "success", "2026-11-22T00:07:30Z"),
	})
	writer.store.casHook = nil
	if err != nil || attempts < 2 {
		t.Fatalf("RecordRuns = %v in %d attempts; want it written after losing its first compare-and-swap", err, attempts)
	}
	st, _ := recordRunsStored(t, s)
	if got := st.Checks["ci-ok"]; got.CheckRunID != 9 || got.Status != "completed" {
		t.Fatalf("ci-ok is %+v, want run 9 completed: the retried batch's older run replaced the newer one", got)
	}
	if got := st.Checks["lint"]; got.CheckRunID != 11 {
		t.Fatalf("lint is %+v, want run 11: the retried batch lost a run", got)
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
	now := time.Now()
	z.pass(pub, logging.New("test"), now.UnixMilli())
	old := asked(t, z)
	answer(t, writer, old, s)
	claimed, _ := recordRunsStored(t, s)
	// Past the claim's window from the moment its store stamped it, however long after the held
	// moment that was.
	resumed := time.UnixMilli(claimed.Claim.ClaimedAt).Add(verifyClaimWindow + time.Minute)

	if held, err := s.ClaimStillHeld(verifyRecordKey, claimed.Generation, claimed.Hash(), old, now.UnixMilli(), 0); err != nil || !held {
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
	z.pass(pub, logging.New("test"), now.UnixMilli())
	renewed := asked(t, z)
	if renewed <= old || pub.count() != 0 {
		t.Fatalf("after the expiry the head asked at %d (old %d) and published %d settlements; want a new request and none", renewed, old, pub.count())
	}
	answer(t, writer, renewed, s)
	z.pass(pub, logging.New("test"), now.UnixMilli())
	if pub.count() != 1 {
		t.Fatalf("an answer to the new request published %d settlements, want 1", pub.count())
	}
}

// verifyGaveUp is the summary loop's ERROR line when it gives an episode up (giveUp).
const verifyGaveUp = "checks verify gave up; the head is not announced until Dispatch answers a later episode"

// verifyRun drives a summary loop of a store on a clock the test moves, which the store stamps its
// claims and records by too (newVerifyRun), so a test runs the loop at the production timeouts and
// backoffs without waiting them out. It notes the clock's offset from start at each pass that
// started an episode (a wait under a claim generation it had not seen) and at each that gave one
// up (one more verifyGaveUp line).
type verifyRun struct {
	t             *testing.T
	z             *summarizer
	pub           *recPub
	logs          *logtest.Buffer
	logger        *logging.Logger
	start, clock  time.Time
	seen          map[uint64]bool
	claims, stops []time.Duration
}

func newVerifyRun(t *testing.T, s *Store, start time.Time) *verifyRun {
	run := &verifyRun{t: t, z: testSummarizer(s, 0), pub: &recPub{}, logs: &logtest.Buffer{}, start: start, clock: start, seen: map[uint64]bool{}}
	run.logger = logging.NewWithWriter("test", run.logs)
	// The store's clock in a verifyRun: the moments the store stamps (a claim's ClaimedAt, a
	// record's LastEventAt) are the run's, so its claims stand on the clock the summary loop judges
	// them by, and its waits (the backoff after a lost compare-and-swap, a record's RecordWait) run
	// on the real clock, as each is a pause of the pass that makes it: on the run's clock, which
	// moves only between passes, a pass that lost a race would wait for ever.
	s.clock = clocktest.WithNow(clock.Real(), func() time.Time { return run.clock })
	return run
}

// until runs a pass every 30 seconds of the clock, up to offset past start, and fails the test at
// any publication.
func (run *verifyRun) until(offset time.Duration) {
	run.t.Helper()
	for ; run.clock.Sub(run.start) <= offset; run.clock = run.clock.Add(30 * time.Second) {
		stops := strings.Count(run.logs.String(), verifyGaveUp)
		run.z.pass(run.pub, run.logger, run.clock.UnixMilli())
		if run.pub.count() != 0 {
			run.t.Fatalf("published %d settlements at %v with no answer", run.pub.count(), run.clock.Sub(run.start))
		}
		if wait, ok := waiting(run.z); ok && !run.seen[wait.generation] {
			run.seen[wait.generation] = true
			run.claims = append(run.claims, run.clock.Sub(run.start))
		}
		if strings.Count(run.logs.String(), verifyGaveUp) > stops {
			run.stops = append(run.stops, run.clock.Sub(run.start))
		}
	}
}

// A head Dispatch leaves unanswered for longer than ten minutes, the point at which the listener
// once gave it up until a new observation, is asked again with no new observation of it and
// published once Dispatch answers. At the production timeouts (verifyRun), each episode asks five
// times and is given up ten minutes after it began; the head is claimed again once the give-up's
// backoff runs out, which doubles at each give-up of the same snapshot up to its cap (5, 10, 20,
// 30, 30 minutes), so episodes begin at 0, 15, 35, 65, 105 and 145 minutes. The answer to the
// request of the sixth publishes the head.
func TestAHeadLeftUnansweredPastTenMinutesIsAskedAgainAfterADoublingBackoffAndPublished(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	// A bucket TTL past the run, so no give-up in it is for good (stopVerify).
	s := openVerifyingStoreWithTTL(t, conn, 24*time.Hour)
	writer := dispatchWriter(t, conn)
	readyHead(t, s)
	run := newVerifyRun(t, s, time.Now())

	run.until(146 * time.Minute)

	const minute = time.Minute
	wantClaims := []time.Duration{0, 15 * minute, 35 * minute, 65 * minute, 105 * minute, 145 * minute}
	wantStops := []time.Duration{10 * minute, 25 * minute, 45 * minute, 75 * minute, 115 * minute}
	if !slices.Equal(run.claims, wantClaims) || !slices.Equal(run.stops, wantStops) {
		t.Fatalf("with no answer, episodes began at %v and were given up at %v; want %v and %v", run.claims, run.stops, wantClaims, wantStops)
	}
	answer(t, writer, asked(t, run.z), s)
	run.z.pass(run.pub, run.logger, run.clock.UnixMilli())
	if run.pub.count() != 1 {
		t.Fatalf("the answer to the episode that began at %v published %d settlements, want 1", run.claims[len(run.claims)-1], run.pub.count())
	}
}

// A head given up once its last observation is the bucket's TTL old is given up for good, as every
// unanswered head was before the backoff, so its record is left to expire: no listener claims it
// again, however long past every backoff the clock runs, until a new observation of it starts an
// episode at once, whose answer publishes.
func TestAHeadGivenUpOnceItsRecordIsATTLOldWaitsForANewObservation(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	s := openVerifyingStore(t, conn)
	writer := dispatchWriter(t, conn)
	readyHead(t, s)
	// The bucket's TTL is an hour: the run begins an hour after the head's last observation.
	run := newVerifyRun(t, s, time.Now().Add(time.Hour))

	run.until(130 * time.Minute)

	if !slices.Equal(run.claims, []time.Duration{0}) || !slices.Equal(run.stops, []time.Duration{10 * time.Minute}) {
		t.Fatalf("an hour after the head's last observation, episodes began at %v and were given up at %v; want one, given up for good at 10m0s", run.claims, run.stops)
	}
	if st, _ := recordRunsStored(t, s); st.Claim != nil || st.VerifyStopped != st.Hash() || st.VerifyRetryAt != 0 {
		t.Fatalf("the record after a give-up an hour after its last observation is %+v, want no claim and the episode stopped for good", st)
	}

	if err := s.Record(context.Background(), recordRunsCheck("lint", 2, "completed", "success", "2026-11-22T00:06:00Z")); err != nil {
		t.Fatalf("record: %v", err)
	}
	waitCacheChecks(t, s, recordRunsOwner, recordRunsRepo, recordRunsNumber, recordRunsSHA, 2)
	run.z.pass(run.pub, run.logger, run.clock.UnixMilli())
	answer(t, writer, asked(t, run.z), s)
	run.z.pass(run.pub, run.logger, run.clock.UnixMilli())
	if run.pub.count() != 1 {
		t.Fatalf("the new observation's episode published %d settlements on its answer, want 1", run.pub.count())
	}
}

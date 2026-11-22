package cistore

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/internal/logging"
)

// A gap holds a record until a GitHub read after it: the head's own gap, or the whole intake's
// stamp while no read cycle has begun after it. A read at the very millisecond of a gap does not
// count, and neither does one before it. Nothing holds where no reconcile has completed a cycle
// over the bucket (readCycle 0): nothing would ever read the head.
func TestGapHolds(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		gapAt, readAt        int64
		intakeGap, readCycle int64
		holds                bool
	}{
		{name: "no gap at all", readCycle: 1, holds: false},
		{name: "a head's gap with no reconcile known", gapAt: 100, holds: false},
		{name: "the intake's stamp with no reconcile known", intakeGap: 200, holds: false},
		{name: "a head's gap with no read", gapAt: 100, readCycle: 1, holds: true},
		{name: "a head's gap with a read before it", gapAt: 100, readAt: 99, readCycle: 1, holds: true},
		{name: "a head's gap with a read at its millisecond", gapAt: 100, readAt: 100, readCycle: 1, holds: true},
		{name: "a head's gap with a read after it", gapAt: 100, readAt: 101, readCycle: 1, holds: false},
		{name: "the intake's stamp with no read", intakeGap: 200, readCycle: 1, holds: true},
		{name: "the intake's stamp with a read after it", intakeGap: 200, readAt: 201, readCycle: 1, holds: false},
		{name: "the intake's stamp with a read cycle begun at it", intakeGap: 200, readCycle: 200, holds: true},
		{name: "the intake's stamp lifted by a read cycle begun after it", intakeGap: 200, readCycle: 201, holds: false},
		{name: "a head's gap after the lift", gapAt: 300, intakeGap: 200, readCycle: 250, holds: true},
		{name: "a head's gap read after the lift", gapAt: 300, readAt: 301, intakeGap: 200, readCycle: 250, holds: false},
		{name: "a read after the head's gap but before the stamp", gapAt: 100, readAt: 150, intakeGap: 200, readCycle: 1, holds: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := State{GapAt: tc.gapAt, ReadAt: tc.readAt}
			if got := gapHolds(st, tc.intakeGap, tc.readCycle); got != tc.holds {
				t.Fatalf("gapHolds(%+v, %d, %d) = %v, want %v", st, tc.intakeGap, tc.readCycle, got, tc.holds)
			}
		})
	}
}

// servedByReconcile writes a read cycle that began at at into the bucket, as a reconcile does at
// the end of a cycle, and lets the store know of it, as its watcher would.
func servedByReconcile(t *testing.T, s *Store, at time.Time) {
	t.Helper()
	if err := s.raiseStamp(context.Background(), readCycleKey, at.UnixMilli()); err != nil {
		t.Fatalf("write the read cycle: %v", err)
	}
	value, found, err := s.readReadCycle(context.Background())
	if err != nil || !found {
		t.Fatalf("read the read cycle back: found %v, %v", found, err)
	}
	s.applyStamp(kvwatch.Entry{Key: readCycleKey, Value: value})
}

// A webhook write that failed holds its head at once, in this process, and the summary loop's next
// pass writes the gap into the record, so every replica refuses the head from then on. A run write
// whose read began before the gap leaves it holding; one whose read began after it frees the head.
func TestAFailedWebhookWriteHoldsItsHeadUntilAReadAfterIt(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	s := openStore(t, conn)
	servedByReconcile(t, s, time.UnixMilli(1))
	key := recordRunsHead.key()
	readBefore := time.Now()
	time.Sleep(2 * time.Millisecond)

	ended, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Record(ended, recordRunsCheck("test", 2, "queued", "", "2026-11-22T00:02:06Z")); err == nil {
		t.Fatal("a write under an ended context landed")
	}
	if !s.gapHoldsFor(key, State{}) {
		t.Fatal("the head of a failed webhook write is not held before its gap is written")
	}

	testSummarizer(s, 0).pass(&recPub{}, logging.New("test"), time.Now().UnixMilli())
	st, _ := recordRunsStored(t, s)
	if st.GapAt == 0 {
		t.Fatalf("the pass did not write the head's gap: %+v", st)
	}
	s.gapMu.Lock()
	pending := len(s.pendingGaps)
	s.gapMu.Unlock()
	if pending != 0 {
		t.Fatalf("%d gaps are still pending after the pass wrote them", pending)
	}

	done := recordRunsCheck("test", 2, "completed", "success", "2026-11-22T00:09:00Z")
	if _, err := s.recordRuns(context.Background(), recordRunsHead, readBefore, 0, 1, []contracts.CIObservation{done}); err != nil {
		t.Fatalf("write the runs read before the gap: %v", err)
	}
	st, _ = recordRunsStored(t, s)
	if !s.gapHoldsFor(key, st) {
		t.Fatalf("a read that began before the gap freed the head: %+v", st)
	}
	if _, err := s.recordRuns(context.Background(), recordRunsHead, time.Now(), 0, 1, []contracts.CIObservation{done}); err != nil {
		t.Fatalf("write the runs read after the gap: %v", err)
	}
	st, _ = recordRunsStored(t, s)
	if s.gapHoldsFor(key, st) {
		t.Fatalf("a read that began after the gap left the head held: %+v", st)
	}
}

// The listener's start stamp holds every head with no read after it, and the summary loop writes
// it into the bucket; a second store reads it there (NeedsRead through a writer).
func TestTheStartStampIsWrittenAndHeldByEveryReader(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	s := openStore(t, conn)
	servedByReconcile(t, s, time.UnixMilli(1))
	if err := s.Record(context.Background(), recordRunsCheck("ci-ok", 1, "completed", "success", "2026-11-22T00:05:00Z")); err != nil {
		t.Fatalf("record: %v", err)
	}
	s.MarkIntakeGap(time.Now())
	testSummarizer(s, 0).pass(&recPub{}, logging.New("test"), time.Now().UnixMilli())
	st, _ := recordRunsStored(t, s)
	if !s.gapHoldsFor(recordRunsHead.key(), st) {
		t.Fatal("the stamp does not hold a head with no read after it")
	}
	stamped, err := s.readStamp(intakeGapKey)
	if err != nil || stamped == 0 {
		t.Fatalf("the bucket holds stamp %d (%v), want the start's", stamped, err)
	}
	writer := &Writer{store: s}
	needs, err := writer.NeedsRead(recordRunsOwner, recordRunsRepo, recordRunsNumber, recordRunsSHA)
	if err != nil || !needs {
		t.Fatalf("NeedsRead = %v, %v; want the stamp to need a read", needs, err)
	}
}

// settleable records one finished check of recordRunsHead and waits until the store's cache holds
// it, so a summary pass reads the record the bucket holds.
func settleable(t *testing.T, s *Store) {
	t.Helper()
	if err := s.Record(context.Background(), recordRunsCheck("ci-ok", 1, "completed", "success", "2026-11-22T00:05:00Z")); err != nil {
		t.Fatalf("record: %v", err)
	}
	waitCacheChecks(t, s, recordRunsOwner, recordRunsRepo, recordRunsNumber, recordRunsSHA, 1)
}

// A listener with no reconcile behind its bucket (no read cycle) settles from webhooks right after
// its start, as a development stack and scripts/e2e-local.sh expect: nothing would ever read a
// head the start held.
func TestAListenerWithNoReconcileSettlesRightAfterItsStart(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	s := openStore(t, conn)
	s.MarkIntakeGap(time.Now())
	settleable(t, s)
	pub := &recPub{}

	testSummarizer(s, 0).pass(pub, logging.New("test"), time.Now().UnixMilli())

	if pub.count() != 1 {
		t.Fatalf("published %d settlements right after a start with no reconcile, want 1", pub.count())
	}
}

// failingStartRead makes the start's read of the read cycle fail failures times and then answer
// cycleBegan, or no key at all when it is zero. The bucket holds no read cycle, so the store's
// watcher delivers none: the start's read is the only way the store learns of one.
func failingStartRead(t *testing.T, s *Store, failures int, cycleBegan time.Time) {
	t.Helper()
	var answer []byte
	if !cycleBegan.IsZero() {
		var err error
		if answer, err = json.Marshal(stamp{Kind: readCycleKey, At: cycleBegan.UnixMilli()}); err != nil {
			t.Fatalf("encode the read cycle: %v", err)
		}
	}
	s.startRead = func(context.Context) ([]byte, bool, error) {
		if failures > 0 {
			failures--
			return nil, false, errors.New("nats: timeout")
		}
		return answer, answer != nil, nil
	}
}

// A start whose read of the read cycle fails holds every head until it answers: a store that
// cannot be read is the outage the gaps are for. Here the read fails twice and then finds a read
// cycle after the start, which lifts the start's stamp; a head whose creation webhook failed is
// held throughout, and after the answer by its own gap.
func TestAStartReadThatFailsHoldsEveryHeadUntilItAnswers(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	s := openStore(t, conn)
	s.MarkIntakeGap(time.UnixMilli(1000))
	failingStartRead(t, s, 2, time.Now())
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Record(ended, recordRunsCheck("test", 2, "queued", "", "2026-11-22T00:02:06Z")); err == nil {
		t.Fatal("a write under an ended context landed")
	}
	settleable(t, s)
	pub := &recPub{}

	for attempt := 1; attempt <= 3; attempt++ {
		testSummarizer(s, 0).pass(pub, logging.New("test"), time.Now().UnixMilli())
		if pub.count() != 0 {
			t.Fatalf("pass %d published %d settlements of a head whose creation webhook failed", attempt, pub.count())
		}
	}
	if st, _ := recordRunsStored(t, s); st.GapAt == 0 {
		t.Fatalf("the head's gap was not written once the store answered: %+v", st)
	}
}

// A start whose read of the read cycle fails and then finds no read cycle settles once it answers:
// no reconcile serves the bucket.
func TestAStartReadThatFailsThenFindsNoReconcileResumesSettling(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	s := openStore(t, conn)
	s.MarkIntakeGap(time.Now())
	failingStartRead(t, s, 1, time.Time{})
	settleable(t, s)
	pub := &recPub{}

	testSummarizer(s, 0).pass(pub, logging.New("test"), time.Now().UnixMilli())
	if pub.count() != 0 {
		t.Fatalf("published %d settlements before the start's read answered, want none", pub.count())
	}
	testSummarizer(s, 0).pass(pub, logging.New("test"), time.Now().UnixMilli())
	if pub.count() != 1 {
		t.Fatalf("published %d settlements once the start's read found no reconcile, want 1", pub.count())
	}
}

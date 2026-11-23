package cistore

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testwait"
)

// recordWaitObservation is the one check the record-wait rows record.
func recordWaitObservation(sha string) contracts.CIObservation {
	return contracts.CIObservation{
		Owner: "example-org", Repo: "example-repo", Number: "42", SHA: sha,
		CheckName: "build", CheckRunID: 900, URL: "https://example-host/runs/900",
		Status: "completed", Conclusion: "success", ObservedAt: "2026-11-22T17:00:00Z",
	}
}

// A store no test armed gives up a Record whose KV read never answers at recordWait, on its own:
// the caller names no deadline, so only the store's wait ends the call. The row waits for that
// answer until the bound end of twice recordWait (testwait.UntilBoundEnd), so a short -timeout still
// leaves the store its wait; a store that lost recordWait holds the call past it, which fails the
// row by name.
func TestARecordOfAStoreNoTestArmedGivesUpAtRecordWait(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	s := openStore(t, conn)
	s.waitArm = nil
	held := make(chan struct{})
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(held) }) })
	useKV(t, s, &interleavingKV{KeyValue: s.watcher.KV().Legacy(), beforeGet: func(string) { <-held }})

	answered := make(chan error, 1)
	go func() {
		answered <- s.Record(context.Background(), recordWaitObservation("1111111111111111111111111111111111111111"))
	}()
	err := testwait.UntilBoundEnd(t, 2*recordWait, "the Record whose KV read never answers to give up at recordWait", func() (error, bool) {
		select {
		case err := <-answered:
			return err, true
		default:
			return nil, false
		}
	})
	if !errors.Is(err, errKVUnanswered) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the Record answered %v, want the KV call given up at the record's wait (%v, %v)", err, errKVUnanswered, context.DeadlineExceeded)
	}
}

// The wait a store no test armed gives a record is recordWait from the record's start.
func TestTheWaitOfAStoreNoTestArmedIsRecordWait(t *testing.T) {
	before := time.Now()
	ctx, cancel := (&Store{}).recordWaitContext(context.Background())
	after := time.Now()
	defer cancel()
	deadline, bounded := ctx.Deadline()
	if !bounded || deadline.Before(before.Add(recordWait)) || deadline.After(after.Add(recordWait)) {
		t.Fatalf("a store no test armed: deadline %v (bounded %t), want recordWait (%s) from the call, between %v and %v", deadline, bounded, recordWait, before.Add(recordWait), after.Add(recordWait))
	}
}

// A store a test armed waits on the armed wait in place of recordWait: armed with a deadline
// already past, a Record whose KV answers at once still gives up, before any KV call. A store that
// ignored the arm would record it.
func TestARecordOfAnArmedStoreEndsAtTheArmedWait(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	s := openStore(t, conn)
	s.waitArm = func(ctx context.Context, _ time.Duration) (context.Context, context.CancelFunc) {
		return context.WithDeadline(ctx, time.Now().Add(-time.Second))
	}
	err := s.Record(context.Background(), recordWaitObservation("2222222222222222222222222222222222222222"))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the Record answered %v, want the armed wait's end (%v)", err, context.DeadlineExceeded)
	}
}

// openStore arms a test's store with the bound end of recordWait (testwait.BoundEndAt of the test's
// deadline), read when each record begins.
func TestATestsStoreArmsTheBoundEndOfRecordWait(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	s := openStore(t, conn)
	before := time.Now()
	ctx, cancel := s.recordWaitContext(context.Background())
	after := time.Now()
	defer cancel()
	got, armed := ctx.Deadline()
	deadline, bounded := t.Deadline()
	if !bounded {
		if armed {
			t.Fatalf("under -timeout 0 a test's record wait ends at %v, want no end", got)
		}
		return
	}
	if lo, hi := testwait.BoundEndAt(deadline, before, recordWait), testwait.BoundEndAt(deadline, after, recordWait); !armed || got.Before(lo) || got.After(hi) {
		t.Fatalf("a test's record wait ends at %v (armed %t), want the bound end of recordWait at the test's deadline %v, between %v and %v", got, armed, deadline, lo, hi)
	}
}

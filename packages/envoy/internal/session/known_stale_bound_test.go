package session

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/kvwatch"
)

// While a move to a recovered connection has failed, the store answers from its copy for the
// watcher's stale bound, two minutes, as every cache of the listener does
// (kvwatch.Watcher.Servable), and past it answers nothing: Opened is false, so a publish leaves its
// counts out, and Get answers no record, so a send to a session with no entry answers 404, exactly
// as on a listener with no bucket. Once the retry has moved the watcher and the moved watcher's
// read has completed, the store answers again. The watcher's clock is the test's, and it moves only
// when the test moves it, so the bound is passed without waiting it out, and a failed move that
// takes minutes on a busy machine does not pass it before the test asks the store inside it.
func TestAStaleStoreLeavesTheCountsOutPastItsBound(t *testing.T) {
	watcherClock := &heldClock{now: time.Now()}
	known, conn, grant, logged := knownOverGrant(t, 200*time.Millisecond, time.Second, withKnownSessionWatch(kvwatch.WithNow(watcherClock.Now)))
	const stale = "ses_stale_store"
	const topic = "notifications.team.stale-store"
	if err := known.RecordWithin(context.Background(), KnownSession{SessionID: stale, Topics: []string{topic}}); err != nil {
		t.Fatalf("record: %v", err)
	}

	// The connection drops and the move to the recovered one fails: the copy stops following its
	// bucket, and its stale episode begins at the disconnect.
	grant.Grant(t, "nothing.this.store.uses")
	known.Disconnected()
	if err := known.Rewatch(context.Background(), conn); err == nil {
		t.Fatal("the move succeeded while the user's grant named none of the bucket's subjects; this test says nothing about a stale store")
	}
	if !known.Opened() {
		t.Fatal("the store stopped answering at a failed move, inside the stale bound")
	}
	if _, ok := known.Get(stale); !ok {
		t.Error("Get answered no record inside the stale bound, want the record the copy holds")
	}
	if got := matchedIDs(known, topic); !slices.Equal(got, []string{stale}) {
		t.Errorf("MatchEach answered %v inside the stale bound, want %q", got, stale)
	}

	watcherClock.advance(2*time.Minute + time.Second)
	if known.Opened() {
		t.Fatal("the store reports itself open past the stale bound: a publish would count its recipients against a copy that stopped following its bucket")
	}
	if record, ok := known.Get(stale); ok {
		t.Errorf("Get answered %#v past the stale bound, want no record, as a listener with no bucket answers", record)
	}
	if got := matchedIDs(known, topic); len(got) != 0 {
		t.Errorf("MatchEach answered %v past the stale bound, want none", got)
	}

	grant.Grant(t, knownBucketGrant()...)
	awaitKnown(t, 60*time.Second, "the store did not answer again once its retry had moved the watcher", known.Opened)
	if _, ok := known.Get(stale); !ok {
		t.Error("the repaired store answers no record of the session written before the failed move")
	}
	// Opened is the moved watcher's read completed, on the watcher's own goroutine, and the line
	// saying the store is back is written by the retry once its move has returned (attemptRepair),
	// so the two are not ordered: a count read when Opened turns true can find no line yet. The
	// retry retires at an attempt after the one that wrote the line, both under repairMu, so its
	// retirement is what the count waits for, and no attempt after it writes a second line.
	awaitKnown(t, 60*time.Second, "the repair did not retire once it had nothing left to repair", func() bool {
		return !repairRunning(known)
	})
	if lines := strings.Count(logged(), knownBackLine); lines != 1 {
		t.Errorf("the repair logged %d lines saying the store is back, want one:\n%s", lines, logged())
	}
}

// heldClock is a clock that moves only when the test advances it, as kvwatch's own tests hold
// theirs: a stale bound measured on it is passed by the test's advance, never by how long the
// steps before that advance took. The package's other clock (clock) follows the real time, which
// a stale episode would then measure too.
type heldClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *heldClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *heldClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

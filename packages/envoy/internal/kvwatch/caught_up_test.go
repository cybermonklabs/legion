package kvwatch_test

import (
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// heldBarrierBound is how long the barrier waits while the current watcher's delivery is held: a
// copy that counted itself caught up answers at once, and the answer this wait must give is behind.
const heldBarrierBound = 200 * time.Millisecond

// A rewatch of the same stream reads the stream, creates its watcher, and only then installs its
// read. A record K stored after that create is one the new watcher's snapshot lacks, which it
// delivers only after its scan's end. The replaced watcher, current until the install, delivers K
// first, and the new read sweeps it (kvwatch.Copy.BeginRead names that gap). While the new
// watcher's delivery of K is held, the copy follows its bucket and lacks a record the bucket stored
// before the barrier's read of its stream, so WaitCaughtUp must answer behind; once K is delivered
// it answers caught up. A barrier that counted the replaced watcher's delivery of K would answer
// caught up while K is missing, and a chunk collection would then delete the chunks K names once
// they passed their grace.
func TestWaitCaughtUpAnswersBehindWhileARecordTheReplacedWatcherDeliveredIsSwept(t *testing.T) {
	uri := testnats.URL(t)
	_, kv := bucket(t, uri)
	putKeys(t, kv, "one")
	into := newSeen()
	w := kvwatch.New("test cache", kv, into, kvwatch.WithStreamProbeInterval(0))
	t.Cleanup(w.Stop)
	w.Start()
	eventually(t, "the first read's completion", w.Following)

	held := newCreatedThenWritten(t)
	installed := make(chan error, 1)
	go func() { installed <- kvwatch.WatchOn(w, scripted(kv, held.step()).handle()) }()
	testwait.Receive(t, "the rewatch's watcher to receive its whole scan", held.scanned)
	if _, err := kv.Put("K", []byte("1")); err != nil {
		t.Fatalf("put K: %v", err)
	}
	eventually(t, "the replaced watcher's delivery of K", func() bool { return into.has("K") })
	held.let()
	if err := testwait.Receive(t, "the rewatch to install", installed); err != nil {
		t.Fatalf("the rewatch: %v", err)
	}
	eventually(t, "the rewatch's read to complete", w.Following)
	if into.has("K") {
		t.Fatal("the rewatch's read kept K, which only the replaced watcher had delivered: the gap this row needs did not open")
	}

	progress, err := w.WaitCaughtUp(testContext(t), heldBarrierBound)
	if err != nil {
		t.Fatalf("the barrier while K's delivery is held: %v", err)
	}
	if progress.CaughtUp {
		t.Fatalf("WaitCaughtUp = %+v while K is missing from the copy (in the copy: %v); want CaughtUp false until the current watcher delivers K",
			progress, into.has("K"))
	}

	held.deliverLater()
	progress, err = w.WaitCaughtUp(testContext(t), testwait.Remaining(t, waitSpare))
	if err != nil || !progress.CaughtUp || !into.has("K") {
		t.Fatalf("WaitCaughtUp = %+v, %v once K was delivered, K in the copy %v; want CaughtUp true with K in the copy",
			progress, err, into.has("K"))
	}
}

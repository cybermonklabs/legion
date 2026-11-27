package kvwatch_test

import (
	"context"
	"sync"
	"testing"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// createdThenWritten is a watch that creates its watcher on the bucket and holds it until the test
// lets it go, as heldCreate does, once the watcher has received its whole scan, its nil included:
// a record the test writes while it holds is one the watcher's snapshot does not hold, which the
// watcher delivers only after its nil. It holds those later records back until the test delivers
// them, so the read completes with no delivery of them, as it does when such an update has not
// arrived by then. scanned closes once the scan's nil has arrived.
type createdThenWritten struct {
	scanned, release, deliver chan struct{}
	letOnce, deliverOnce      sync.Once
}

func newCreatedThenWritten(t *testing.T) *createdThenWritten {
	h := &createdThenWritten{scanned: make(chan struct{}), release: make(chan struct{}), deliver: make(chan struct{})}
	t.Cleanup(h.let)
	t.Cleanup(h.deliverLater)
	return h
}

// let hands the created watcher to the watch.
func (h *createdThenWritten) let() { h.letOnce.Do(func() { close(h.release) }) }

// deliverLater forwards the records the watcher delivered after its scan.
func (h *createdThenWritten) deliverLater() { h.deliverOnce.Do(func() { close(h.deliver) }) }

// step is the watch that holds its create on h.
func (h *createdThenWritten) step() watchStep {
	return func(ctx context.Context, kv jetstream.KeyValue) (jetstream.KeyWatcher, error) {
		watcher, err := kv.WatchAll(ctx)
		if err != nil {
			return nil, err
		}
		// The scan is forwarded while the watch is held, so its buffer holds the bucket's few
		// records and the nil.
		updates := make(chan jetstream.KeyValueEntry, 16)
		go func() {
			defer close(updates)
			scanning := true
			for entry := range watcher.Updates() {
				if !scanning {
					<-h.deliver
				}
				updates <- entry
				if scanning && entry == nil {
					scanning = false
					close(h.scanned)
				}
			}
		}()
		<-h.release
		return &forwardedWatcher{KeyWatcher: watcher, updates: updates}, nil
	}
}

// holdAt puts key in the copy at revision as if an earlier read had left it there, before any read
// begins.
func (s *seen) holdAt(key string, revision uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.keys.Delivered(kvwatch.Entry{Key: key, Op: kvwatch.OpPut, Revision: revision})
}

// A key written through to the copy after a read's watcher was created, and before that read
// began, survives the read's completion. The bucket holds it, but the watcher's snapshot does not:
// the scan delivers nothing of it, and the watcher delivers its record only after the scan's end.
// Its revision is past the stream's last sequence that the watch read before it created the
// watcher, and that is the read's evidence of it (kvwatch.Copy.BeginRead). A copy that dropped it
// would answer without a key its own write had just put there until that update arrived, and until
// the next read when the connection closed first. A key the copy held from before, at a revision
// the stream had reached by then and which the bucket no longer holds, is still swept.
func TestAKeyWrittenThroughBetweenAReadsCreateAndItsBeginSurvivesTheRead(t *testing.T) {
	uri := testnats.URL(t)
	_, kv := bucket(t, uri)
	putKeys(t, kv, "one")
	into := newSeen()
	into.holdAt("gone-from-the-bucket", 1)
	held := newCreatedThenWritten(t)
	w := kvwatch.New("test cache", scripted(kv, held.step()).handle(), into, kvwatch.WithStreamProbeInterval(0))
	t.Cleanup(w.Stop)
	w.Start()
	testwait.Receive(t, "the read's watcher to receive its whole scan", held.scanned)

	revision, err := kv.Put("written-through", []byte("1"))
	if err != nil {
		t.Fatalf("put written-through: %v", err)
	}
	if outcome := into.put(w, "written-through", revision); outcome != kvwatch.Applied {
		t.Fatalf("the write into the copy came to outcome %d, want Applied (%d)", outcome, kvwatch.Applied)
	}
	held.let()
	testwait.Until(t, "the read's completion", func() (bool, bool) {
		following := w.Following()
		return following, following
	})
	if !into.has("written-through") || into.has("gone-from-the-bucket") || !into.has("one") {
		t.Fatalf("after the read: written-through key %v, gone key %v, bucket key %v; want the written key and the bucket's kept, the gone key swept",
			into.has("written-through"), into.has("gone-from-the-bucket"), into.has("one"))
	}
}

// A key the copy took before a read's stream read, at a revision past the last sequence that
// stream read found, is no evidence for the read: the bucket's sequence space no longer reaches
// that revision - a JetStream restore keeps the stream's creation time, so the watch sees no
// recreated bucket - and the read removes the key when it does not find it, as it removes any key
// it does not find. The same key taken once the read's stream read had begun is the read's
// evidence of it, and it stays. A copy that marked by revision alone keeps the first through every
// read until one begins past its revision, which for a cache that reads only at a start or a
// rewatch can be never.
func TestAKeyTakenBeforeAReadsStreamReadIsNoEvidenceForIt(t *testing.T) {
	uri := testnats.URL(t)
	_, kv := bucket(t, uri)
	for _, tc := range []struct {
		name       string
		takenAfter bool
	}{
		{name: "taken before the read's stream read", takenAfter: false},
		{name: "taken after the read's stream read", takenAfter: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := kvwatch.New("test cache", kv, newSeen())
			copied := kvwatch.NewCopy(kvwatch.Rows[bool]{Decode: func(kvwatch.Entry) (bool, bool, error) { return true, true, nil }})
			kvwatch.SetStreamReads(w, 1)
			copied.BeginRead(kvwatch.Read{LastSeq: 30, StreamRead: 1})
			copied.CompleteRead()
			if tc.takenAfter {
				kvwatch.SetStreamReads(w, 2)
			}
			if outcome := copied.Put(w, w.Epoch(), "taken", true, 40); outcome != kvwatch.Applied {
				t.Fatalf("the write into the copy came to outcome %d, want Applied (%d)", outcome, kvwatch.Applied)
			}
			kvwatch.SetStreamReads(w, 2)
			copied.BeginRead(kvwatch.Read{LastSeq: 35, StreamRead: 2})
			copied.CompleteRead()
			if _, kept := copied.Get("taken"); kept != tc.takenAfter {
				t.Fatalf("the key taken at revision 40 %s, before a read at 35: kept %v, want %v", tc.name, kept, tc.takenAfter)
			}
		})
	}
}

// Two watches that overlap and install in order, the earlier one first: a key written through
// after the later watch's stream read, and before either read began, survives the later read. The
// later watch's snapshot does not hold it, so only the later read's marks keep it, and the earlier
// read's BeginRead must leave its take for the later one, which the earlier read's stream read came
// before. A copy that dropped every take at each BeginRead would sweep it at the later read's end.
func TestAWriteInTheLaterOfTwoOverlappingWatchesGapSurvivesWhenTheyInstallInOrder(t *testing.T) {
	uri := testnats.URL(t)
	_, kv := bucket(t, uri)
	w := kvwatch.New("test cache", kv, newSeen())
	copied := kvwatch.NewCopy(kvwatch.Rows[bool]{Decode: func(kvwatch.Entry) (bool, bool, error) { return true, true, nil }})
	kvwatch.SetStreamReads(w, 1)
	copied.BeginRead(kvwatch.Read{LastSeq: 10, StreamRead: 1})
	copied.CompleteRead()
	// Two watches read the stream, the earlier at 20 (stream read 2) and the later at 30 (stream
	// read 3), and the write lands after both, at 40.
	kvwatch.SetStreamReads(w, 3)
	if outcome := copied.Put(w, w.Epoch(), "K", true, 40); outcome != kvwatch.Applied {
		t.Fatalf("the write at 40 came to outcome %d, want Applied (%d)", outcome, kvwatch.Applied)
	}
	copied.BeginRead(kvwatch.Read{LastSeq: 20, StreamRead: 2})
	copied.BeginRead(kvwatch.Read{LastSeq: 30, StreamRead: 3})
	copied.CompleteRead()
	if _, kept := copied.Get("K"); !kept {
		t.Fatal("the write at 40, after the later watch's stream read, was swept by the later read")
	}
}

// A take after the stream read of a watch that never installs is evidence for the read that does,
// whose stream read came before it: the watch that installs read the stream (stream read 1), a
// second watch read it too (stream read 2) and was discarded, and a key written through then, at
// 40, past the installed read's last sequence, is a record the installed read's snapshot does not
// hold. A copy that kept only takes stamped with the installing read's own count would sweep it.
// The next read, whose stream read comes after the take, finds no evidence in it.
func TestATakeAfterADiscardedWatchsStreamReadIsEvidenceForTheReadThatInstalls(t *testing.T) {
	uri := testnats.URL(t)
	_, kv := bucket(t, uri)
	w := kvwatch.New("test cache", kv, newSeen())
	copied := kvwatch.NewCopy(kvwatch.Rows[bool]{Decode: func(kvwatch.Entry) (bool, bool, error) { return true, true, nil }})
	kvwatch.SetStreamReads(w, 2)
	if outcome := copied.Put(w, w.Epoch(), "K", true, 40); outcome != kvwatch.Applied {
		t.Fatalf("the write at 40 came to outcome %d, want Applied (%d)", outcome, kvwatch.Applied)
	}
	copied.BeginRead(kvwatch.Read{LastSeq: 30, StreamRead: 1})
	copied.CompleteRead()
	if _, kept := copied.Get("K"); !kept {
		t.Fatal("the write at 40, after the installed read's stream read, was swept by that read")
	}
	kvwatch.SetStreamReads(w, 3)
	copied.BeginRead(kvwatch.Read{LastSeq: 35, StreamRead: 3})
	copied.CompleteRead()
	if _, kept := copied.Get("K"); kept {
		t.Fatal("the write at 40 was kept by a read whose stream read came after it, with no delivery of it")
	}
}

// The first read of a bucket deleted and created again marks nothing of the old bucket, even a
// take in its own gap: a key written through the old bucket's handle at 500, after the read's
// stream read, names a revision of the old bucket, which says nothing of what the new one, whose
// stream had stored 3 records, holds. The read clears the fence before it marks, so the key is
// swept when the read does not find it, and its record, if the new bucket holds one, is taken when
// the watcher delivers it. Marking first would keep the old bucket's value.
func TestARecreatedBucketsFirstReadMarksNothingOfTheOldBucket(t *testing.T) {
	uri := testnats.URL(t)
	_, kv := bucket(t, uri)
	w := kvwatch.New("test cache", kv, newSeen())
	copied := kvwatch.NewCopy(kvwatch.Rows[string]{Decode: func(entry kvwatch.Entry) (string, bool, error) {
		return string(entry.Value), true, nil
	}})
	kvwatch.SetStreamReads(w, 2)
	if outcome := copied.Put(w, w.Epoch(), "K", "old", 500); outcome != kvwatch.Applied {
		t.Fatalf("the write at 500 came to outcome %d, want Applied (%d)", outcome, kvwatch.Applied)
	}
	copied.BeginRead(kvwatch.Read{Recreated: true, LastSeq: 3, StreamRead: 2})
	copied.CompleteRead()
	if value, kept := copied.Get("K"); kept {
		t.Fatalf("after the recreated bucket's first read: K kept as %q, want it removed with its fence", value)
	}
	if err := copied.Delivered(kvwatch.Entry{Key: "K", Op: kvwatch.OpPut, Value: []byte("new"), Revision: 4}); err != nil {
		t.Fatalf("deliver K at 4: %v", err)
	}
	if value, _ := copied.Get("K"); value != "new" {
		t.Fatalf("after the new bucket's record at 4: K = %q, want %q", value, "new")
	}
}

// A write the fence drops is no evidence for a read, through each write site: a write-through
// (Put), a record read back (Apply) and a delete whose record the cache could not read back
// (Evict). After a restore that keeps the stream's creation time and moves its sequence back to
// 33, a key the copy took at 40 is written again in a read's gap, at 34: the fence drops that
// write (34 < 40), and the read, whose snapshot holds no record of the key, removes it and its
// fence, so the restored bucket's record at 34 is taken when the watcher delivers it. Counting the
// dropped write would mark the key on the fence's 40, keep its value from before the restore, and
// drop the bucket's own update until its revision passed 40.
func TestAWriteTheFenceDropsIsNoEvidenceForARead(t *testing.T) {
	uri := testnats.URL(t)
	_, kv := bucket(t, uri)
	for _, tc := range []struct {
		name  string
		write func(*testing.T, *kvwatch.Copy[string], *kvwatch.Watcher) kvwatch.Outcome
	}{
		{name: "Put", write: func(_ *testing.T, copied *kvwatch.Copy[string], w *kvwatch.Watcher) kvwatch.Outcome {
			return copied.Put(w, w.Epoch(), "K", "post-restore", 34)
		}},
		{name: "Apply", write: func(t *testing.T, copied *kvwatch.Copy[string], w *kvwatch.Watcher) kvwatch.Outcome {
			outcome, err := copied.Apply(w, w.Epoch(), kvwatch.Entry{Key: "K", Op: kvwatch.OpPut, Value: []byte("post-restore"), Revision: 34})
			if err != nil {
				t.Fatalf("apply K at 34: %v", err)
			}
			return outcome
		}},
		{name: "Evict", write: func(_ *testing.T, copied *kvwatch.Copy[string], w *kvwatch.Watcher) kvwatch.Outcome {
			return copied.Evict(w, w.Epoch(), "K", 34)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := kvwatch.New("test cache", kv, newSeen())
			copied := kvwatch.NewCopy(kvwatch.Rows[string]{Decode: func(entry kvwatch.Entry) (string, bool, error) {
				return string(entry.Value), true, nil
			}})
			kvwatch.SetStreamReads(w, 1)
			copied.BeginRead(kvwatch.Read{LastSeq: 30, StreamRead: 1})
			copied.CompleteRead()
			if outcome := copied.Put(w, w.Epoch(), "K", "pre-restore", 40); outcome != kvwatch.Applied {
				t.Fatalf("the write at 40 came to outcome %d, want Applied (%d)", outcome, kvwatch.Applied)
			}
			// The restore moves the stream back to 33; the next read's stream read begins, and the
			// write at 34 lands in its gap.
			kvwatch.SetStreamReads(w, 2)
			if outcome := tc.write(t, &copied, w); outcome != kvwatch.Fenced {
				t.Fatalf("the %s at 34 came to outcome %d, want Fenced (%d)", tc.name, outcome, kvwatch.Fenced)
			}
			copied.BeginRead(kvwatch.Read{LastSeq: 33, StreamRead: 2})
			copied.CompleteRead()
			if value, kept := copied.Get("K"); kept {
				t.Fatalf("after the read: K kept as %q, want it removed with its fence", value)
			}
			if err := copied.Delivered(kvwatch.Entry{Key: "K", Op: kvwatch.OpPut, Value: []byte("post-restore"), Revision: 34}); err != nil {
				t.Fatalf("deliver K at 34: %v", err)
			}
			if value, _ := copied.Get("K"); value != "post-restore" {
				t.Fatalf("after the restored bucket's record at 34: K = %q, want %q", value, "post-restore")
			}
		})
	}
}

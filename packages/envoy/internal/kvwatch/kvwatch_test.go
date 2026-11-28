package kvwatch_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/goroutinetest"
	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// seen is a recording Cache that keeps a copy the way the stores do (kvwatch.Copy): each delivered
// record updates it, a read marks each key it delivers, and a completed read sweeps every key it
// left unmarked. It records every read that began and every read that completed.
type seen struct {
	mu        sync.Mutex
	keys      kvwatch.Copy[bool]
	begun     []kvwatch.Read
	completed []kvwatch.Read
}

// newSeen is an empty seen, whose copy keeps every put it is handed: its Decode never fails, so
// Delivered never returns an error.
func newSeen() *seen {
	return &seen{keys: kvwatch.NewCopy(kvwatch.Rows[bool]{Decode: func(kvwatch.Entry) (bool, bool, error) {
		return true, true, nil
	}})}
}

func (s *seen) Apply(entry kvwatch.Entry, _ kvwatch.Read) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.keys.Delivered(entry)
}

func (s *seen) BeginRead(read kvwatch.Read) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys.BeginRead(read)
	s.begun = append(s.begun, read)
}

func (s *seen) CompleteRead(read kvwatch.Read) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys.CompleteRead()
	s.completed = append(s.completed, read)
}

// hold puts key in the copy as if an earlier read had left it there, before any read begins.
func (s *seen) hold(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.keys.Delivered(kvwatch.Entry{Key: key, Op: kvwatch.OpPut})
}

func (s *seen) has(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.keys.Get(key)
	return ok
}

// reads is every read that began, and completions every read that completed, in order.
func (s *seen) reads() []kvwatch.Read {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]kvwatch.Read(nil), s.begun...)
}

func (s *seen) completions() []kvwatch.Read {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]kvwatch.Read(nil), s.completed...)
}

// withApply is a seen that runs before on each record before it takes it, so a test can hold an
// apply open or count what was applied when. before reads the record as a nats.KeyValueEntry
// (legacyView), the shape the tests' hooks were written against.
type withApply struct {
	*seen
	before func(natsgo.KeyValueEntry)
}

func (c withApply) Apply(entry kvwatch.Entry, read kvwatch.Read) {
	c.before(legacyView{entry})
	c.seen.Apply(entry, read)
}

// legacyView is a record a watcher delivered, as the nats.KeyValueEntry a hook reads.
type legacyView struct{ entry kvwatch.Entry }

func (e legacyView) Bucket() string     { return testBucketName }
func (e legacyView) Key() string        { return e.entry.Key }
func (e legacyView) Value() []byte      { return e.entry.Value }
func (e legacyView) Revision() uint64   { return e.entry.Revision }
func (e legacyView) Created() time.Time { return e.entry.Created }
func (e legacyView) Delta() uint64      { return 0 }
func (e legacyView) Operation() natsgo.KeyValueOp {
	switch e.entry.Op {
	case kvwatch.OpDelete:
		return natsgo.KeyValueDelete
	case kvwatch.OpPurge:
		return natsgo.KeyValuePurge
	}
	return natsgo.KeyValuePut
}

// clock is the injected clock a stale episode is measured on (kvwatch.WithNow).
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *clock { return &clock{now: time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// scriptedKV is a bucket's jetstream handle whose watches go as the steps of its script say, and
// through the handle itself once the script has run out. It counts the watches it was asked for.
type scriptedKV struct {
	jetstream.KeyValue
	legacy  natsgo.KeyValue
	streams jetstream.JetStream
	mu      sync.Mutex
	steps   []watchStep
	watched int
}

// watchStep is one watch of a scriptedKV, on the bucket's jetstream handle kv, in ctx, the
// context the watcher's subscription lives in.
type watchStep func(ctx context.Context, kv jetstream.KeyValue) (jetstream.KeyWatcher, error)

// scripted is kv whose watches go as steps say.
func scripted(kv bus.KeyValue, steps ...watchStep) *scriptedKV {
	return &scriptedKV{KeyValue: kv.WatchHandle(), legacy: kv.Legacy(), streams: kv.Streams(), steps: steps}
}

func (k *scriptedKV) Watch(ctx context.Context, keys string, opts ...jetstream.WatchOpt) (jetstream.KeyWatcher, error) {
	k.mu.Lock()
	k.watched++
	var step watchStep
	if len(k.steps) > 0 {
		step, k.steps = k.steps[0], k.steps[1:]
	}
	k.mu.Unlock()
	if step == nil {
		return k.KeyValue.Watch(ctx, keys, opts...)
	}
	return step(ctx, k.KeyValue)
}

func (k *scriptedKV) watches() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.watched
}

// handle is k as the bus.KeyValue a watcher takes.
func (k *scriptedKV) handle() bus.KeyValue { return bus.NewKeyValue(k.legacy, k, k.streams) }

// stallsAfter is a watch whose scan delivers `after` records and then nothing more: it drops the
// rest of the scan and its nil, as a scan that stalls, so only the watcher's idle wait ends it. What
// the bucket delivers after the scan is forwarded, as a live watcher's updates are.
func stallsAfter(after int) watchStep {
	return func(ctx context.Context, kv jetstream.KeyValue) (jetstream.KeyWatcher, error) {
		watcher, err := kv.WatchAll(ctx)
		if err != nil {
			return nil, err
		}
		updates := make(chan jetstream.KeyValueEntry)
		go func() {
			defer close(updates)
			scanning, forwarded := true, 0
			for entry := range watcher.Updates() {
				switch {
				case entry == nil && scanning:
					scanning = false
				case scanning && forwarded == after:
				default:
					forwarded++
					updates <- entry
				}
			}
		}()
		return &forwardedWatcher{KeyWatcher: watcher, updates: updates}, nil
	}
}

// fails is a watch whose create is refused with err.
func fails(err error) watchStep {
	return func(context.Context, jetstream.KeyValue) (jetstream.KeyWatcher, error) { return nil, err }
}

// endsEarly is a watch whose subscription ends after `after` entries, before its scan's nil, as an
// ordered consumer nats.go could not recreate ends it.
func endsEarly(after int) watchStep {
	return func(ctx context.Context, kv jetstream.KeyValue) (jetstream.KeyWatcher, error) {
		watcher, err := kv.WatchAll(ctx)
		if err != nil {
			return nil, err
		}
		updates := make(chan jetstream.KeyValueEntry)
		go func() {
			defer close(updates)
			for range after {
				entry, ok := <-watcher.Updates()
				if !ok || entry == nil {
					break
				}
				updates <- entry
			}
			_ = watcher.Stop()
			for range watcher.Updates() {
			}
		}()
		return &forwardedWatcher{KeyWatcher: watcher, updates: updates}, nil
	}
}

// heldEnd holds one read's end: its watcher delivers the bucket's records, then waits with the
// scan's nil in hand until the test releases it. reached closes when the nil is held; the watcher
// may still be applying the record it took last. handled closes once the watcher has taken the nil
// and is waiting for its next record: the watcher then sends one more nil, which it can take only
// after it has handled the first (a nil past the first is one it ignores), so a test knows the
// read's end was handled, however it was.
type heldEnd struct {
	reached, release, handled chan struct{}
	once                      sync.Once
}

func newHeldEnd(t *testing.T) *heldEnd {
	h := &heldEnd{reached: make(chan struct{}), release: make(chan struct{}), handled: make(chan struct{})}
	t.Cleanup(h.let)
	return h
}

// let releases the held nil.
func (h *heldEnd) let() { h.once.Do(func() { close(h.release) }) }

// step is the watch that holds its end on h.
func (h *heldEnd) step() watchStep {
	return func(ctx context.Context, kv jetstream.KeyValue) (jetstream.KeyWatcher, error) {
		watcher, err := kv.WatchAll(ctx)
		if err != nil {
			return nil, err
		}
		updates := make(chan jetstream.KeyValueEntry)
		go func() {
			defer close(updates)
			held := false
			for entry := range watcher.Updates() {
				if entry == nil && !held {
					held = true
					close(h.reached)
					<-h.release
					updates <- nil
					updates <- nil
					close(h.handled)
					continue
				}
				updates <- entry
			}
		}()
		return &forwardedWatcher{KeyWatcher: watcher, updates: updates}, nil
	}
}

// traceOf reads the trace of a goroutine whose id the test recorded, or "" after it returns.
func traceOf(id uint64) string {
	return traceIn(goroutinetest.Traces(), id)
}

// traceIn finds id in a goroutine dump, or returns "" if it is absent.
func traceIn(traces []string, id uint64) string {
	header := fmt.Sprintf("goroutine %d ", id)
	if i := slices.IndexFunc(traces, func(trace string) bool { return strings.HasPrefix(trace, header) }); i >= 0 {
		return traces[i]
	}
	return ""
}

// startedIn reads the live traces created in creator by fn, or by any function when fn is empty.
func startedIn(creator uint64, fn string) []string {
	parent := fmt.Sprintf(" in goroutine %d\n", creator)
	return slices.DeleteFunc(goroutinetest.Traces(), func(trace string) bool {
		_, created, _ := strings.Cut(trace, "\ncreated by ")
		name, _, _ := strings.Cut(created, " in goroutine ")
		return !strings.Contains(created, parent) || (fn != "" && name != fn)
	})
}

// deliveryBlocked reports whether a delivery goroutine is parked on the watch's full updates
// buffer. It returns the traces created by fn in creator, or by any function if fn is empty.
func deliveryBlocked(creator uint64, fn string) ([]string, bool) {
	traces := startedIn(creator, fn)
	for _, trace := range traces {
		header, _, _ := strings.Cut(trace, "\n")
		if strings.Contains(header, "[chan send") && strings.Contains(trace, "jetstream.(*kvs).WatchFiltered.func") {
			return traces, true
		}
	}
	return traces, false
}

// returned waits for a goroutine whose id the test recorded to return.
func returned(t *testing.T, what string, id uint64) {
	t.Helper()
	testwait.Until(t, what, func() (string, bool) {
		trace := traceOf(id)
		return trace, trace == ""
	})
}

// reader wraps a watch step to record the first goroutine to call Updates: the consumer of an
// installed watcher.
func reader(step watchStep, reading chan<- uint64) watchStep {
	return func(ctx context.Context, kv jetstream.KeyValue) (jetstream.KeyWatcher, error) {
		watcher, err := step(ctx, kv)
		if err != nil {
			return nil, err
		}
		return &readingWatcher{KeyWatcher: watcher, reading: reading}, nil
	}
}

// subscribing records the goroutine that calls WatchAll, before it subscribes.
func subscribing(ids chan<- uint64) watchStep {
	return func(ctx context.Context, kv jetstream.KeyValue) (jetstream.KeyWatcher, error) {
		ids <- goroutinetest.ID()
		return kv.WatchAll(ctx)
	}
}

type readingWatcher struct {
	jetstream.KeyWatcher
	once    sync.Once
	reading chan<- uint64
}

func (w *readingWatcher) Updates() <-chan jetstream.KeyValueEntry {
	w.once.Do(func() { w.reading <- goroutinetest.ID() })
	return w.KeyWatcher.Updates()
}

// wait waits for ch, which names what, to receive or close (testwait.Receive): only the test
// binary's deadline, less testwait's margin, ends it, and it fails the test by name then.
func wait(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	testwait.Receive(t, what, ch)
}

// forwardedWatcher is a watcher whose updates a step's goroutine forwards from the real one.
type forwardedWatcher struct {
	jetstream.KeyWatcher
	updates chan jetstream.KeyValueEntry
}

func (w *forwardedWatcher) Updates() <-chan jetstream.KeyValueEntry { return w.updates }

// heldUpdates queues entries behind the gate even if Stop closes the underlying watch.
type heldUpdates struct {
	release, sentinelQueued, sentinelTaken chan struct{}
	count                                  atomic.Int32
}

// holdsAfter forwards n entries from a real watcher, then holds the rest until open.
func holdsAfter(t *testing.T, n int) (*heldUpdates, watchStep) {
	t.Helper()
	h := &heldUpdates{release: make(chan struct{}), sentinelQueued: make(chan struct{}), sentinelTaken: make(chan struct{})}
	lifetime := testwait.Context(t)
	return h, func(ctx context.Context, kv jetstream.KeyValue) (jetstream.KeyWatcher, error) {
		watcher, err := kv.WatchAll(ctx)
		if err != nil {
			return nil, err
		}
		updates := make(chan jetstream.KeyValueEntry)
		go func() {
			defer close(updates)
			source := watcher.Updates()
			forward := func(entry jetstream.KeyValueEntry) bool {
				select {
				case updates <- entry:
					return true
				case <-lifetime.Done():
					return false
				}
			}
			next := func() bool {
				select {
				case entry, ok := <-source:
					return ok && forward(entry)
				case <-lifetime.Done():
					return false
				}
			}
			for range n {
				if !next() {
					return
				}
			}
			var queued []jetstream.KeyValueEntry
		holding:
			for {
				select {
				case entry, ok := <-source:
					if !ok {
						source = nil
						continue
					}
					queued = append(queued, entry)
					h.count.Add(1)
					if entry == nil {
						close(h.sentinelQueued)
					}
				case <-h.release:
					break holding
				case <-lifetime.Done():
					return
				}
			}
			for _, entry := range queued {
				if !forward(entry) {
					return
				}
				if entry == nil {
					close(h.sentinelTaken)
				}
			}
			for source != nil && next() {
			}
		}()
		return &forwardedWatcher{KeyWatcher: watcher, updates: updates}, nil
	}
}

// queued counts entries collected behind the gate before open.
func (h *heldUpdates) queued() int { return int(h.count.Load()) }

// open releases the held entries and resumes live delivery.
func (h *heldUpdates) open() { close(h.release) }

// watchAll opens a real watch for a scripted step that only observes its reader.
func watchAll(ctx context.Context, kv jetstream.KeyValue) (jetstream.KeyWatcher, error) {
	return kv.WatchAll(ctx)
}

// bucket opens the test bucket on its own connection, which the test can close to end a watcher
// the way a lost connection does.
func bucket(t *testing.T, uri string) (*natsgo.Conn, bus.KeyValue) {
	t.Helper()
	conn := testnats.Connect(t, uri)
	t.Cleanup(conn.Close)
	js, err := conn.JetStream(testnats.MaxWait(t))
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	kv, err := bus.EnsureKeyValue(t.Context(), conn, js, &natsgo.KeyValueConfig{Bucket: "kvwatch-test"})
	if err != nil {
		t.Fatalf("bucket: %v", err)
	}
	return conn, kv
}

// wrap is kv, a legacy handle opened on conn (a recreated bucket), as the bus.KeyValue a watcher
// takes.
func wrap(t *testing.T, conn *natsgo.Conn, kv natsgo.KeyValue) bus.KeyValue {
	t.Helper()
	wrapped, err := bus.WrapKeyValue(conn, kv)
	if err != nil {
		t.Fatalf("wrap the bucket handle: %v", err)
	}
	return wrapped
}

// jetStreamEntry reads key through kv's jetstream handle, as a watcher would deliver it.
func jetStreamEntry(t *testing.T, kv bus.KeyValue, key string) jetstream.KeyValueEntry {
	t.Helper()
	entry, err := kv.WatchHandle().Get(testwait.Context(t), key)
	if err != nil {
		t.Fatalf("get %s: %v", key, err)
	}
	return entry
}

// lockedInWatch matches a sync.Mutex.Lock frame whose caller is watch itself. Inside watch that is
// only applyMu: a wait on w.mu shows sync.(*RWMutex).Lock as the caller, and one on nats.go's
// connection mutex, further down the call tree (bus.ReadStreamState, WatchAll), shows nats.go frames.
var lockedInWatch = regexp.MustCompile(`sync\.\(\*Mutex\)\.Lock\([^\n]*\n\t[^\n]*\n[^\n]*kvwatch\.\(\*Watcher\)\.watch\(`)

// parkedOnTheApplyLock reports whether the recorded watch is queued to take applyMu.
func parkedOnTheApplyLock(id uint64) bool {
	trace := traceOf(id)
	header, _, _ := strings.Cut(trace, "\n")
	return strings.Contains(header, "[sync.Mutex.Lock") && lockedInWatch.MatchString(trace)
}

// waitsInWatch reports whether the recorded watch is parked in watch's select for the old stop.
func waitsInWatch(id uint64) bool {
	header, stack, _ := strings.Cut(traceOf(id), "\n")
	return strings.Contains(header, "[select") &&
		strings.HasPrefix(stack, "github.com/sjawhar/envoy/internal/kvwatch.(*Watcher).watch(")
}

// eventually waits for cond until testwait's own margin before the test binary's deadline
// (testwait.Until), and fails the test naming what it waited for. No span of its own ends it: on a
// loaded machine a fact that is merely late is not a failure. A condition that can say what it last
// read waits through testwait.Until itself, so that its failure names the reading.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	testwait.Until(t, what, func() (bool, bool) {
		held := cond()
		return held, held
	})
}

// The cache is ready once the first watcher has delivered every existing key, and not before. Its
// copy can be served only once that first read has completed: before it, Servable refuses.
func TestTheCacheIsReadyAfterTheFirstScan(t *testing.T) {
	t.Parallel()
	uri := testnats.URL(t)
	_, kv := bucket(t, uri)
	if _, err := kv.Put("before-start", []byte("1")); err != nil {
		t.Fatalf("put: %v", err)
	}
	into := newSeen()
	w := kvwatch.New("test cache", kv, into)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := w.WaitReady(ctx); !errors.Is(err, context.DeadlineExceeded) || w.Ready() {
		t.Fatalf("WaitReady before Start = %v (ready %v), want the context's deadline", err, w.Ready())
	}
	if err := w.Servable(); !errors.Is(err, kvwatch.ErrRefilling) {
		t.Fatalf("Servable before any read = %v, want %v", err, kvwatch.ErrRefilling)
	}

	w.Start()
	for range 2 {
		err := w.WaitReady(testwait.Context(t))
		if err != nil {
			t.Fatalf("WaitReady after Start: %v", err)
		}
	}
	if !w.Ready() || !into.has("before-start") {
		t.Fatalf("ready %v with the existing key applied %v, want both", w.Ready(), into.has("before-start"))
	}
	if err := w.Servable(); err != nil || !w.Following() || w.NeedsRead() {
		t.Fatalf("after the first complete read: Servable %v, Following %v, NeedsRead %v; want nil, true, false",
			err, w.Following(), w.NeedsRead())
	}
	if reads, done := into.reads(), into.completions(); len(reads) != 1 || len(done) != 1 || reads[0].Recreated || reads[0].Epoch != 0 {
		t.Fatalf("reads begun %+v, completed %+v; want one read of epoch 0, completed", reads, done)
	}
}

// A first start that fails releases readiness and records its error, so a caller waiting for the
// cache sees an empty cache that /healthz reports rather than a hang.
func TestAFailedFirstStartIsReadyAndRecordsItsError(t *testing.T) {
	t.Parallel()
	uri := testnats.URL(t)
	conn, kv := bucket(t, uri)
	conn.Close()
	w := kvwatch.New("test cache", kv, newSeen())

	w.Start()
	if err := w.WaitReady(testwait.Context(t)); err != nil {
		t.Fatalf("WaitReady after a failed first start: %v", err)
	}
	if w.Err() == nil {
		t.Fatal("a failed first start recorded no error")
	}
}

// The listener's self-health check rebuilds a cache whose copy needs a read (cmd/listener,
// errCacheNeedsRead), so the window between a store's Open and its first watch installing that
// read must not read as one: the read is under way from Start. Only a first watch that failed
// leaves a copy nothing but a Rewatch repairs, and that one does report it.
func TestAStartedWatcherNeedsNoReadUntilItsFirstWatchFails(t *testing.T) {
	t.Parallel()
	t.Run("while the first watch is still starting", func(t *testing.T) {
		t.Parallel()
		uri := testnats.URL(t)
		_, kv := bucket(t, uri)
		held := holdWatch(kv)
		w := kvwatch.New("test cache", held.handle(), newSeen())
		t.Cleanup(w.Stop)

		w.Start()
		armedWatch(t, held, "the watch to arm its watcher")
		// Read while the watch is held inside its KV Watch, before it takes the watcher's locks:
		// the state a self-health probe would find in the window this test is about.
		needsRead := w.NeedsRead()
		close(held.release)
		if needsRead {
			t.Fatal("a watcher whose first watch is still starting reports a copy that needs a read")
		}
		eventually(t, "the first read to complete", func() bool { return w.Following() })
		if w.NeedsRead() {
			t.Fatal("a copy that follows its bucket reports it needs a read")
		}
	})

	t.Run("after the first watch failed", func(t *testing.T) {
		t.Parallel()
		uri := testnats.URL(t)
		conn, kv := bucket(t, uri)
		conn.Close()
		w := kvwatch.New("test cache", kv, newSeen())

		w.Start()
		if err := w.WaitReady(testwait.Context(t)); err != nil {
			t.Fatalf("WaitReady after a failed first start: %v", err)
		}
		if !w.NeedsRead() {
			t.Fatal("a copy whose first watch failed reports no read is needed; nothing would rebuild it")
		}
	})
}

// startReadingStreams records the Start goroutine before it makes its first stream request.
type startReadingStreams struct {
	jetstream.JetStream
	once sync.Once
	id   chan uint64
}

func (s *startReadingStreams) Stream(ctx context.Context, name string) (jetstream.Stream, error) {
	s.once.Do(func() { s.id <- goroutinetest.ID() })
	return s.JetStream.Stream(ctx, name)
}

// A first start that fails after a Rewatch has armed a live watcher records nothing: the error is
// the first start's, not the current watcher's, and recording it would mark a live cache dead.
func TestAFirstStartFailingAfterARewatchLeavesTheLiveWatcherHealthy(t *testing.T) {
	t.Parallel()
	uri := testnats.URL(t)
	firstConn, first := bucket(t, uri)
	liveConn, _ := bucket(t, uri)
	firstConn.Close()
	into := newSeen()
	streams := &startReadingStreams{JetStream: first.Streams(), id: make(chan uint64, 1)}
	w := kvwatch.New("test cache", bus.NewKeyValue(first.Legacy(), first.WatchHandle(), streams), into)
	t.Cleanup(w.Stop)
	if err := w.Rewatch(testwait.Context(t), liveConn); err != nil {
		t.Fatalf("rewatch: %v", err)
	}
	live := w.KV()

	w.Start()
	startID := testwait.Receive(t, "Start to read its stream", streams.id)
	if err := w.WaitReady(testwait.Context(t)); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	returned(t, "the failed first Start to return", startID)
	if err := w.Err(); err != nil {
		t.Fatalf("the failed first start marked the live watcher dead: %v", err)
	}
	if _, err := live.Put("after-start", []byte("1")); err != nil {
		t.Fatalf("put: %v", err)
	}
	eventually(t, "the live watcher's delivery", func() bool { return into.has("after-start") })
}

// The ERROR a failed first start logs is the live cache's alone. A first start that fails while
// no watcher is current leaves the cache empty and nothing feeding it, so it logs the ERROR an
// operator reads. A first start that fails after a reconnect hook's Rewatch armed a replacement
// ends a watch nothing reads: the replacement feeds the cache, so the end is logged at INFO and no
// ERROR is written. The listener declares a hub that stopped answering lost within seconds, so
// every such cut runs its caches through that replacement path.
func TestAFailedFirstStartLogsItsErrorOnlyWhileItIsTheLiveWatch(t *testing.T) {
	t.Parallel()
	const failed = "test cache watch failed"
	const replaced = "test cache first watch failed after a rewatch replaced it; the replacement feeds the cache"

	t.Run("no replacement", func(t *testing.T) {
		t.Parallel()
		uri := testnats.URL(t)
		conn, kv := bucket(t, uri)
		conn.Close()
		logger, records := jsonLogger(t)
		w := kvwatch.New("test cache", kv, newSeen(), kvwatch.WithLogger(logger))

		w.Start()
		testwait.Until(t, "the failed first start's ERROR", func() ([]map[string]any, bool) {
			logged := records()
			return logged, len(matching(logged, "ERROR", failed)) == 1
		})
		if ended := matching(records(), "INFO", replaced); len(ended) != 0 {
			t.Fatalf("a first start no rewatch replaced logged %v, want the ERROR alone", ended)
		}
	})

	t.Run("a rewatch replaced it", func(t *testing.T) {
		t.Parallel()
		uri := testnats.URL(t)
		firstConn, first := bucket(t, uri)
		liveConn, _ := bucket(t, uri)
		firstConn.Close()
		logger, records := jsonLogger(t)
		into := newSeen()
		w := kvwatch.New("test cache", first, into, kvwatch.WithLogger(logger))
		t.Cleanup(w.Stop)
		if err := w.Rewatch(context.Background(), liveConn); err != nil {
			t.Fatalf("rewatch: %v", err)
		}

		w.Start()
		// The watch ends in one of two ways, and the wait takes whichever comes: the INFO line
		// this watch writes, or an ERROR, which is the case this test refuses.
		testwait.Until(t, "the replaced first watch's end", func() ([]map[string]any, bool) {
			logged := records()
			return logged, len(matching(logged, "INFO", replaced))+len(atError(logged)) > 0
		})
		if logged := atError(records()); len(logged) != 0 {
			t.Fatalf("a first watch a rewatch replaced logged %v, want no ERROR", logged)
		}
		if ended := matching(records(), "INFO", replaced); len(ended) != 1 {
			t.Fatalf("the replaced first watch logged %d INFO %q lines, want one", len(ended), replaced)
		}
		// The replacement is the cache's watcher still, and it delivers what the bucket takes.
		live := w.KV()
		if _, err := live.Put("after-start", []byte("1")); err != nil {
			t.Fatalf("put: %v", err)
		}
		eventually(t, "the live watcher's delivery", func() bool { return into.has("after-start") })
		if logged := atError(records()); len(logged) != 0 {
			t.Fatalf("the live watcher logged %v, want no ERROR", logged)
		}
	})
}

// atError is every ERROR record, each as its message and error, whatever the watcher wrote it
// about: a case that wants none names no message of its own and so cannot miss one.
func atError(records []map[string]any) []string {
	var found []string
	for _, record := range records {
		if level, _ := record["level"].(string); level != "ERROR" {
			continue
		}
		message, _ := record["msg"].(string)
		failure, _ := record["error"].(string)
		found = append(found, message+": "+failure)
	}
	return found
}

// A first start that fails while a Rewatch's watcher is still scanning leaves readiness to that
// watcher: the cache is ready only once the current watcher has delivered every existing key, so a
// caller waiting for it never reads a partial cache as complete.
func TestAFirstStartFailingDuringARewatchScanWaitsForThatScan(t *testing.T) {
	t.Parallel()
	_, uri := testnats.Start(t)
	firstConn, first := bucket(t, uri)
	liveConn, live := bucket(t, uri)
	if _, err := live.Put("held", []byte("1")); err != nil {
		t.Fatalf("put: %v", err)
	}
	firstConn.Close()
	entered := make(chan struct{})
	holdCtx := testwait.Context(t)
	release := make(chan struct{})
	var enterOnce, releaseOnce sync.Once
	releaseScan := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseScan)
	into := newSeen()
	apply := func(entry natsgo.KeyValueEntry) {
		if entry.Key() == "held" {
			enterOnce.Do(func() { close(entered) })
			select {
			case <-release:
			case <-holdCtx.Done():
			}
		}
	}
	streams := &startReadingStreams{JetStream: first.Streams(), id: make(chan uint64, 1)}
	w := kvwatch.New("test cache", bus.NewKeyValue(first.Legacy(), first.WatchHandle(), streams), withApply{seen: into, before: apply},
		kvwatch.WithAfterFunc((&pending{}).afterFunc()))
	t.Cleanup(w.Stop)
	if err := w.Rewatch(testwait.Context(t), liveConn); err != nil {
		t.Fatalf("rewatch: %v", err)
	}
	testwait.Receive(t, "the Rewatch's scan to reach the held key", entered)

	w.Start()
	startID := testwait.Receive(t, "Start to read its stream", streams.id)
	returned(t, "the failed first Start to return", startID)
	if w.Ready() {
		t.Fatal("the cache was ready while the current watcher's scan had not delivered every key")
	}
	releaseScan()
	eventually(t, "readiness after the current watcher's scan", w.Ready)
	if !into.has("held") {
		t.Fatal("ready without the held key")
	}
	if err := w.Err(); err != nil {
		t.Fatalf("the failed first start marked the live watcher dead: %v", err)
	}
}

// A watcher that ends on its own - its connection closed - leaves the cache frozen, and Err says so
// until a Rewatch replaces it.
func TestAWatcherThatEndsOnItsOwnRecordsItsError(t *testing.T) {
	t.Parallel()
	uri := testnats.URL(t)
	conn, kv := bucket(t, uri)
	into := newSeen()
	w := kvwatch.New("test cache", kv, into)
	w.Start()
	eventually(t, "the first scan", w.Ready)
	conn.Close()
	testwait.Until(t, "the ended watcher's error", func() (error, bool) {
		err := w.Err()
		return err, err != nil
	})

	liveConn, _ := bucket(t, uri)
	if err := w.Rewatch(context.Background(), liveConn); err != nil {
		t.Fatalf("rewatch: %v", err)
	}
	live := w.KV()
	if err := w.Err(); err != nil {
		t.Fatalf("a replacement watcher kept the old one's error: %v", err)
	}
	if _, err := live.Put("after-rewatch", []byte("1")); err != nil {
		t.Fatalf("put: %v", err)
	}
	eventually(t, "the replacement watcher's delivery", func() bool { return into.has("after-rewatch") })
	for _, read := range into.reads() {
		if read.Recreated || read.Epoch != 0 {
			t.Fatalf("a rewatch onto the same bucket began read %+v, want every read of epoch 0", read)
		}
	}
}

// A watcher Rewatch replaced ends without recording anything: only the current watcher's end is
// the cache's.
func TestAReplacedWatcherEndingRecordsNothing(t *testing.T) {
	t.Parallel()
	uri := testnats.URL(t)
	firstConn, first := bucket(t, uri)
	secondConn, _ := bucket(t, uri)
	reading := make(chan uint64, 1)
	w := kvwatch.New("test cache", scripted(first, reader(watchAll, reading)).handle(), newSeen())
	t.Cleanup(w.Stop)
	w.Start()
	consumeID := testwait.Receive(t, "the first watcher's consume id", reading)
	if err := w.WaitReady(testwait.Context(t)); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	if err := w.Rewatch(testwait.Context(t), secondConn); err != nil {
		t.Fatalf("rewatch: %v", err)
	}
	firstConn.Close()
	returned(t, "the replaced watcher to finish consuming", consumeID)
	if err := w.Err(); err != nil {
		t.Fatalf("the replaced watcher's end was recorded: %v", err)
	}
}

// A bucket deleted and created again numbers its revisions from 1, so a cache fenced by the old
// revisions would drop the new bucket's entries. A Rewatch onto the recreated bucket begins a read
// of a new epoch, and the cache keeps its copy through that read: a key only the old bucket held
// is still there while the read runs, a key of the new bucket appears as it is delivered, and only
// the read's completion removes what the new bucket does not hold. Until then the copy is served,
// within its stale bound, rather than emptied. The recreate is logged, in the words the refill rig
// reads.
func TestARewatchOntoARecreatedBucketKeepsTheCopyUntilTheReadCompletes(t *testing.T) {
	t.Parallel()
	uri := testnats.URL(t)
	conn, kv := bucket(t, uri)
	for _, key := range []string{"ghost", "both"} {
		if _, err := kv.Put(key, []byte("old")); err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
	}
	into := newSeen()
	now := newClock()
	logger, records := jsonLogger(t)
	// No periodic probe: the test drives the read of the recreated bucket itself.
	w := kvwatch.New("test cache", kv, into, kvwatch.WithNow(now.Now), kvwatch.WithLogger(logger), kvwatch.WithStreamProbeInterval(0))
	t.Cleanup(w.Stop)
	w.Start()
	eventually(t, "the first read's completion", w.Following)

	js := jetStream(t, conn)
	deleteTestBucket(t, js)
	recreated := testnats.RecreateKeyValue(t, js, &natsgo.KeyValueConfig{Bucket: testBucketName})
	for _, key := range []string{"both", "fresh"} {
		if _, err := recreated.Put(key, []byte("new")); err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
	}
	end := newHeldEnd(t)
	if err := kvwatch.WatchOn(w, scripted(wrap(t, conn, recreated), end.step()).handle()); err != nil {
		t.Fatalf("rewatch: %v", err)
	}
	wait(t, end.reached, "the new read to deliver every record")
	eventually(t, "the new bucket's key, as delivered", func() bool { return into.has("fresh") })

	if !into.has("ghost") || !into.has("both") || !into.has("fresh") {
		t.Fatalf("while the new read runs: ghost %v, both %v, fresh %v; want the old copy kept and the new key delivered",
			into.has("ghost"), into.has("both"), into.has("fresh"))
	}
	if w.Following() || w.Servable() != nil || len(into.completions()) != 1 {
		t.Fatalf("while the new read runs: Following %v, Servable %v, %d reads completed; want not following, served, one",
			w.Following(), w.Servable(), len(into.completions()))
	}
	reads := into.reads()
	if last := reads[len(reads)-1]; len(reads) != 2 || !last.Recreated || last.Epoch != 1 || w.Epoch() != 1 {
		t.Fatalf("reads begun %+v, Epoch %d; want a second read, recreated, of epoch 1", reads, w.Epoch())
	}
	recreateLine := "test cache bucket was recreated; its cache is re-read from the new bucket and kept until the read completes"
	if line := record(t, records(), recreateLine); line["level"] != "INFO" || line["bucket"] != testBucketName {
		t.Fatalf("the recreate line = %v, want INFO naming bucket %s", line, testBucketName)
	}

	end.let()
	wait(t, end.handled, "the new read's end to be handled")
	if into.has("ghost") || !into.has("both") || !into.has("fresh") || !w.Following() {
		t.Fatalf("after the new read completed: ghost %v, both %v, fresh %v, Following %v; want the ghost swept and the rest kept",
			into.has("ghost"), into.has("both"), into.has("fresh"), w.Following())
	}
	if done := into.completions(); len(done) != 2 || done[1].Epoch != 1 {
		t.Fatalf("reads completed %+v, want the new epoch's read second", done)
	}
}

// A bucket deleted and created again while its watcher runs need not end the watcher: nats.go's
// ordered consumer can reset onto the new stream and go on without delivering its first revisions,
// so the cache stays frozen behind a live watcher. Check, the store's health probe, reports it as
// the watcher's terminal error and the copy stops following, though it keeps every key; the Rewatch
// the listener then runs reads the new bucket, and only that read's completion removes the key the
// new bucket does not hold.
func TestCheckReportsABucketRecreatedUnderALiveWatcher(t *testing.T) {
	t.Parallel()
	uri := testnats.URL(t)
	conn, kv := bucket(t, uri)
	if _, err := kv.Put("ghost", []byte("1")); err != nil {
		t.Fatalf("put: %v", err)
	}
	into := newSeen()
	now := newClock()
	// No periodic probe, which would find the recreate before Check does.
	w := kvwatch.New("test cache", kv, into, kvwatch.WithNow(now.Now), kvwatch.WithStreamProbeInterval(0))
	t.Cleanup(w.Stop)
	w.Start()
	eventually(t, "the first read's completion", w.Following)
	if err := w.Check(); err != nil || w.Err() != nil {
		t.Fatalf("Check on the watched bucket = %v, Err %v, want both nil", err, w.Err())
	}

	js := jetStream(t, conn)
	deleteTestBucket(t, js)
	testnats.RecreateKeyValue(t, js, &natsgo.KeyValueConfig{Bucket: testBucketName})
	if err := w.Check(); err != nil {
		t.Fatalf("Check on the recreated bucket: %v", err)
	}
	// The watcher may also have ended on its own by now, which records its own terminal error; either
	// way the recreated bucket reaches Err.
	if err := w.Err(); err == nil {
		t.Fatal("Check on a recreated bucket left Err nil, so self-health would never rebuild the watcher")
	}
	if !into.has("ghost") || w.Following() || w.Servable() != nil {
		t.Fatalf("after Check flagged the bucket: ghost %v, Following %v, Servable %v; want the copy kept, not following, served within the bound",
			into.has("ghost"), w.Following(), w.Servable())
	}

	if err := w.Rewatch(context.Background(), conn); err != nil {
		t.Fatalf("rewatch: %v", err)
	}
	if err := w.Err(); err != nil {
		t.Fatalf("Err after the rewatch = %v, want nil", err)
	}
	eventually(t, "the new read's completion", w.Following)
	if into.has("ghost") || w.Epoch() != 1 {
		t.Fatalf("after the new read completed: ghost kept %v, Epoch %d; want the ghost swept in epoch 1", into.has("ghost"), w.Epoch())
	}
}

// After Stop, the current watcher's end records nothing and a later Rewatch arms nothing, so a
// reconnect hook still running at shutdown neither reports an error nor starts a watcher.
func TestStopMakesTheEndSilentAndRewatchANoOp(t *testing.T) {
	t.Parallel()
	uri := testnats.URL(t)
	conn, kv := bucket(t, uri)
	into := newSeen()
	reading := make(chan uint64, 1)
	w := kvwatch.New("test cache", scripted(kv, reader(watchAll, reading)).handle(), into)
	t.Cleanup(w.Stop)
	w.Start()
	consumeID := testwait.Receive(t, "the first watcher's consume id", reading)
	if err := w.WaitReady(testwait.Context(t)); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	w.Stop()
	conn.Close()
	returned(t, "the stopped watcher to finish consuming", consumeID)
	if err := w.Err(); err != nil {
		t.Fatalf("a stopped watcher's end was recorded: %v", err)
	}

	liveConn, _ := bucket(t, uri)
	generation, reads, subs := kvwatch.Generation(w), len(into.reads()), liveConn.NumSubscriptions()
	if err := w.Rewatch(testwait.Context(t), liveConn); err != nil {
		t.Fatalf("a Rewatch after Stop returned an error: %v", err)
	}
	if gotGen, gotReads, gotSubs := kvwatch.Generation(w), len(into.reads()), liveConn.NumSubscriptions(); gotGen != generation || gotReads != reads || gotSubs != subs {
		t.Fatalf("Rewatch after Stop: generation %d, reads %d, subscriptions %d; want unchanged (%d, %d, %d)",
			gotGen, gotReads, gotSubs, generation, reads, subs)
	}
	live := w.KV()
	if _, err := live.Put("after-stop", []byte("1")); err != nil {
		t.Fatalf("put: %v", err)
	}
	if into.has("after-stop") {
		t.Fatal("a Rewatch after Stop armed a watcher")
	}
}

// staleBound is the default stale bound, which these tests move the injected clock past.
const staleBound = 2 * time.Minute

// refuses fails the test unless Servable answers ErrRefilling, naming when.
func refuses(t *testing.T, w *kvwatch.Watcher, when string) {
	t.Helper()
	if err := w.Servable(); !errors.Is(err, kvwatch.ErrRefilling) {
		t.Fatalf("Servable %s = %v, want %v", when, err, kvwatch.ErrRefilling)
	}
}

// serves fails the test unless Servable answers nil, naming when.
func serves(t *testing.T, w *kvwatch.Watcher, when string) {
	t.Helper()
	if err := w.Servable(); err != nil {
		t.Fatalf("Servable %s = %v, want nil", when, err)
	}
}

// A read whose end is held, then released only after something took its right to complete -- a
// newer read, a recreated bucket Check found, a disconnect, a Stop -- completes nothing: the
// cache's CompleteRead is not called for it and the copy does not follow its bucket again, so a
// copy past its stale bound stays refused. Only a read that may still complete restores serving.
func TestAHeldReadReleasedTooLateNeitherCompletesNorRestoresServing(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		// overtake takes the held read's right to complete. It returns the read that then
		// restores serving, when there is one.
		overtake func(t *testing.T, w *kvwatch.Watcher, conn *natsgo.Conn, kv bus.KeyValue) *heldEnd
	}{
		{name: "a newer read", overtake: func(t *testing.T, w *kvwatch.Watcher, _ *natsgo.Conn, kv bus.KeyValue) *heldEnd {
			newer := newHeldEnd(t)
			if err := kvwatch.WatchOn(w, scripted(kv, newer.step()).handle()); err != nil {
				t.Fatalf("the newer read: %v", err)
			}
			wait(t, newer.reached, "the newer read to deliver every record")
			return newer
		}},
		{name: "Check finding the bucket recreated", overtake: func(t *testing.T, w *kvwatch.Watcher, conn *natsgo.Conn, _ bus.KeyValue) *heldEnd {
			js := jetStream(t, conn)
			deleteTestBucket(t, js)
			testnats.RecreateKeyValue(t, js, &natsgo.KeyValueConfig{Bucket: testBucketName})
			if err := w.Check(); err != nil || w.Err() == nil {
				t.Fatalf("Check on the recreated bucket = %v, Err %v; want the recreate recorded", err, w.Err())
			}
			return nil
		}},
		{name: "a disconnect", overtake: func(t *testing.T, w *kvwatch.Watcher, _ *natsgo.Conn, _ bus.KeyValue) *heldEnd {
			w.Disconnected()
			return nil
		}},
		{name: "Stop", overtake: func(t *testing.T, w *kvwatch.Watcher, _ *natsgo.Conn, _ bus.KeyValue) *heldEnd {
			w.Stop()
			return nil
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			uri := testnats.URL(t)
			conn, kv := bucket(t, uri)
			if _, err := kv.Put("kept", []byte("1")); err != nil {
				t.Fatalf("put: %v", err)
			}
			into := newSeen()
			now := newClock()
			// No periodic probe, which would read a recreated bucket before Check finds it.
			w := kvwatch.New("test cache", kv, into, kvwatch.WithNow(now.Now), kvwatch.WithStreamProbeInterval(0),
				kvwatch.WithScanIdleTimeout(time.Hour))
			t.Cleanup(w.Stop)
			w.Start()
			eventually(t, "the first read's completion", w.Following)

			held := newHeldEnd(t)
			if err := kvwatch.WatchOn(w, scripted(kv, held.step()).handle()); err != nil {
				t.Fatalf("the held read: %v", err)
			}
			wait(t, held.reached, "the held read to deliver every record")
			now.advance(staleBound + time.Second)
			refuses(t, w, "with a read held past the bound")

			restorer := test.overtake(t, w, conn, kv)
			held.let()
			wait(t, held.handled, "the held read's end to be handled")
			if done := len(into.completions()); done != 1 || w.Following() {
				t.Fatalf("after the overtaken read's end: %d reads completed, Following %v; want only the first, not following", done, w.Following())
			}
			refuses(t, w, "after the overtaken read's end")

			if restorer == nil {
				return
			}
			restorer.let()
			wait(t, restorer.handled, "the newer read's end to be handled")
			if done := len(into.completions()); done != 2 || !w.Following() || !into.has("kept") {
				t.Fatalf("after the newer read's end: %d reads completed, Following %v, kept %v; want two, following, kept",
					done, w.Following(), into.has("kept"))
			}
			serves(t, w, "after the newer read completed")
		})
	}
}

// One stale episode runs from the first moment the copy stopped following until a read completes,
// across any number of reads that fail: one that timed out, one that ended early and a Rewatch that
// never installed. The bound counts from the first, so the copy is refused once that is past the
// bound however recent the last failure, and one complete read restores serving.
func TestAStaleEpisodeOutlastsFailedReadsUntilOneCompletes(t *testing.T) {
	t.Parallel()
	uri := testnats.URL(t)
	conn, kv := bucket(t, uri)
	if _, err := kv.Put("kept", []byte("1")); err != nil {
		t.Fatalf("put: %v", err)
	}
	into := newSeen()
	now := newClock()
	arm := &armed{}
	w := kvwatch.New("test cache", kv, into, kvwatch.WithNow(now.Now), kvwatch.WithAfterFunc(arm.afterFunc()), kvwatch.WithStreamProbeInterval(0))
	t.Cleanup(w.Stop)
	w.Start()
	eventually(t, "the first read's completion", w.Following)

	if err := kvwatch.WatchOn(w, scripted(kv, stallsAfter(0)).handle()); err != nil {
		t.Fatalf("the read that times out: %v", err)
	}
	now.advance(kvwatch.ScanIdleTimeout)
	arm.fireIdle(t)
	testwait.Until(t, "the timed-out read's re-read to be armed", func() (int, bool) {
		waits := arm.waits()
		return waits, waits == 1
	})
	now.advance(staleBound - 30*time.Second - kvwatch.ScanIdleTimeout)
	serves(t, w, "within the bound of the first failed read")

	if err := kvwatch.WatchOn(w, scripted(kv, endsEarly(0)).handle()); err != nil {
		t.Fatalf("the read that ends early: %v", err)
	}
	testwait.Until(t, "the early end's error", func() (error, bool) {
		err := w.Err()
		return err, err != nil
	})
	closed := testnats.Connect(t, uri)
	closed.Close()
	if err := w.Rewatch(context.Background(), closed); err == nil {
		t.Fatal("a Rewatch on a closed connection succeeded")
	}
	now.advance(31 * time.Second)
	refuses(t, w, "past the bound of the first failed read")
	if !w.NeedsRead() {
		t.Fatal("NeedsRead = false for a copy no read is under way to repair")
	}

	if err := w.Rewatch(context.Background(), conn); err != nil {
		t.Fatalf("rewatch: %v", err)
	}
	eventually(t, "the complete read", w.Following)
	serves(t, w, "after one complete read")
	if done := len(into.completions()); done != 2 || !into.has("kept") {
		t.Fatalf("%d reads completed, kept %v; want the first and the last only, kept", done, into.has("kept"))
	}
}

// A Rewatch that fails before it installs anything changes nothing: the listener's self-health
// rebuild rewatches every cache, healthy ones included, so a following copy stays following and
// served however long ago its read completed.
func TestAFailedRewatchLeavesAFollowingWatcherServing(t *testing.T) {
	t.Parallel()
	uri := testnats.URL(t)
	_, kv := bucket(t, uri)
	into := newSeen()
	now := newClock()
	w := kvwatch.New("test cache", kv, into, kvwatch.WithNow(now.Now))
	t.Cleanup(w.Stop)
	w.Start()
	eventually(t, "the first read's completion", w.Following)

	closed := testnats.Connect(t, uri)
	closed.Close()
	if err := w.Rewatch(context.Background(), closed); err == nil {
		t.Fatal("a Rewatch on a closed connection succeeded")
	}
	now.advance(time.Hour)
	if !w.Following() || w.NeedsRead() || w.Err() != nil {
		t.Fatalf("after the failed Rewatch: Following %v, NeedsRead %v, Err %v; want following, nothing needed, no error",
			w.Following(), w.NeedsRead(), w.Err())
	}
	serves(t, w, "an hour after a failed Rewatch of a following copy")
	if reads := len(into.reads()); reads != 1 {
		t.Fatalf("%d reads began, want only the first", reads)
	}
}

// A read whose scan delivers nothing for the idle timeout removes nothing, and the watcher reads
// the bucket again itself after rereadDelay: the watcher still runs, so nothing else -- no terminal
// error, no self-health rebuild -- would start that read. A re-read whose setup fails is armed
// again; a newer read or a Stop makes the armed one read nothing.
func TestATimedOutReadIsReadAgainByTheWatcher(t *testing.T) {
	t.Parallel()
	// timedOut starts a watcher whose first read stalls after one record and times out at its
	// idle wait, its later watches as steps says, with the copy holding a key the bucket does not,
	// and waits for the re-read to be armed.
	timedOut := func(t *testing.T, steps ...watchStep) (*kvwatch.Watcher, *seen, *armed, *scriptedKV, *natsgo.Conn) {
		t.Helper()
		uri := testnats.URL(t)
		conn, kv := bucket(t, uri)
		for _, key := range []string{"one", "two", "three"} {
			if _, err := kv.Put(key, []byte("1")); err != nil {
				t.Fatalf("put %s: %v", key, err)
			}
		}
		into := newSeen()
		into.hold("gone-from-the-bucket")
		arm := &armed{}
		now := newClock()
		handle := scripted(kv, append([]watchStep{stallsAfter(1)}, steps...)...)
		w := kvwatch.New("test cache", handle.handle(), into, kvwatch.WithAfterFunc(arm.afterFunc()),
			kvwatch.WithNow(now.Now), kvwatch.WithStreamProbeInterval(0))
		t.Cleanup(w.Stop)
		w.Start()
		eventually(t, "the scan's one record", func() bool { return into.has("one") })
		now.advance(kvwatch.ScanIdleTimeout)
		arm.fireIdle(t)
		testwait.Until(t, "the re-read to be armed", func() (int, bool) {
			waits := arm.waits()
			return waits, waits == 1
		})
		if got := arm.asked(); got != 5*time.Second {
			t.Fatalf("the re-read waits %v, want 5s", got)
		}
		if len(into.completions()) != 0 || !into.has("gone-from-the-bucket") || w.Err() != nil || w.NeedsRead() {
			t.Fatalf("after the timed-out read: %d completed, kept %v, Err %v, NeedsRead %v; want none completed, the copy kept, no error, a re-read armed",
				len(into.completions()), into.has("gone-from-the-bucket"), w.Err(), w.NeedsRead())
		}
		refuses(t, w, "before any read completed")
		return w, into, arm, handle, conn
	}
	// completed waits for the re-read to complete and checks it swept the key the bucket does not
	// hold.
	completed := func(t *testing.T, w *kvwatch.Watcher, into *seen) {
		t.Helper()
		eventually(t, "the re-read's completion", w.Following)
		serves(t, w, "after the re-read")
		if into.has("gone-from-the-bucket") || !into.has("one") || !into.has("three") || len(into.completions()) != 1 {
			t.Fatalf("after the re-read: gone %v, one %v, three %v, %d completed; want the bucket's keys only, one read completed",
				into.has("gone-from-the-bucket"), into.has("one"), into.has("three"), len(into.completions()))
		}
	}

	t.Run("it reads again", func(t *testing.T) {
		t.Parallel()
		w, into, arm, handle, _ := timedOut(t)
		arm.fireIt(t)
		completed(t, w, into)
		if got := handle.watches(); got != 2 {
			t.Fatalf("the handle was watched %d times, want the first read and the re-read", got)
		}
	})

	t.Run("a re-read whose setup fails is armed again", func(t *testing.T) {
		t.Parallel()
		w, into, arm, _, _ := timedOut(t, fails(errors.New("the watch was refused")))
		arm.fireIt(t)
		if got := arm.waits(); got != 2 || w.NeedsRead() {
			t.Fatalf("after the failed re-read: %d waits armed, NeedsRead %v; want the re-read armed again", got, w.NeedsRead())
		}
		arm.fireIt(t)
		completed(t, w, into)
	})

	t.Run("Stop disarms it", func(t *testing.T) {
		t.Parallel()
		w, _, arm, handle, _ := timedOut(t)
		w.Stop()
		arm.fireIt(t)
		if got := handle.watches(); got != 1 || w.NeedsRead() {
			t.Fatalf("after Stop: the handle was watched %d times, NeedsRead %v; want no re-read", got, w.NeedsRead())
		}
	})

	t.Run("a newer read disarms it", func(t *testing.T) {
		t.Parallel()
		w, into, arm, _, conn := timedOut(t)
		if err := w.Rewatch(context.Background(), conn); err != nil {
			t.Fatalf("rewatch: %v", err)
		}
		completed(t, w, into)
		arm.fireIt(t)
		if reads := len(into.reads()); reads != 2 {
			t.Fatalf("%d reads began, want the timed-out one and the Rewatch's only", reads)
		}
	})
}

// A read whose watcher ends before its scan's nil -- the subscription ended under it -- removes
// nothing either: the copy keeps every key until a read completes.
func TestAReadThatEndsEarlyCompletesNothing(t *testing.T) {
	t.Parallel()
	uri := testnats.URL(t)
	conn, kv := bucket(t, uri)
	for _, key := range []string{"one", "two"} {
		if _, err := kv.Put(key, []byte("1")); err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
	}
	into := newSeen()
	into.hold("gone-from-the-bucket")
	w := kvwatch.New("test cache", scripted(kv, endsEarly(1)).handle(), into,
		kvwatch.WithAfterFunc((&armed{}).afterFunc()))
	t.Cleanup(w.Stop)
	w.Start()
	testwait.Until(t, "the early end's error", func() (error, bool) {
		err := w.Err()
		return err, err != nil
	})
	if len(into.completions()) != 0 || !into.has("gone-from-the-bucket") || !w.NeedsRead() {
		t.Fatalf("after the early end: %d completed, kept %v, NeedsRead %v; want none completed, the copy kept, a read needed",
			len(into.completions()), into.has("gone-from-the-bucket"), w.NeedsRead())
	}
	refuses(t, w, "before any read completed")

	if err := w.Rewatch(context.Background(), conn); err != nil {
		t.Fatalf("rewatch: %v", err)
	}
	eventually(t, "the complete read", w.Following)
	if into.has("gone-from-the-bucket") || !into.has("two") {
		t.Fatalf("after the complete read: gone %v, two %v; want the bucket's keys only", into.has("gone-from-the-bucket"), into.has("two"))
	}
}

// The stale bound is at most half the bucket's TTL, read from the stream before each read: a copy
// whose records live a minute is not served 31 s into a stale episode, while one whose records live
// for good is, so a bucket recreated with a shorter TTL shortens the bound.
func TestABucketRecreatedWithAShorterTTLShortensTheStaleBound(t *testing.T) {
	t.Parallel()
	uri := testnats.URL(t)
	conn, kv := bucket(t, uri)
	into := newSeen()
	now := newClock()
	// No periodic probe: the test drives the read of the recreated bucket itself.
	w := kvwatch.New("test cache", kv, into, kvwatch.WithNow(now.Now), kvwatch.WithStreamProbeInterval(0),
		kvwatch.WithScanIdleTimeout(time.Hour))
	t.Cleanup(w.Stop)
	w.Start()
	eventually(t, "the first read's completion", w.Following)

	held := newHeldEnd(t)
	if err := kvwatch.WatchOn(w, scripted(kv, held.step()).handle()); err != nil {
		t.Fatalf("the held read: %v", err)
	}
	wait(t, held.reached, "the held read to deliver every record")
	now.advance(31 * time.Second)
	serves(t, w, "31 s into a stale episode on a bucket without a TTL")
	held.let()
	wait(t, held.handled, "the held read's end to be handled")

	js := jetStream(t, conn)
	deleteTestBucket(t, js)
	recreated := testnats.RecreateKeyValue(t, js, &natsgo.KeyValueConfig{Bucket: testBucketName, TTL: time.Minute})
	shorter := newHeldEnd(t)
	if err := kvwatch.WatchOn(w, scripted(wrap(t, conn, recreated), shorter.step()).handle()); err != nil {
		t.Fatalf("the read of the recreated bucket: %v", err)
	}
	wait(t, shorter.reached, "the recreated bucket's read to deliver every record")
	if reads := into.reads(); reads[len(reads)-1].MaxAge != time.Minute {
		t.Fatalf("the recreated bucket's read = %+v, want MaxAge 1m", reads[len(reads)-1])
	}
	now.advance(31 * time.Second)
	refuses(t, w, "31 s into a stale episode on a bucket with a 1-minute TTL")
	shorter.let()
	wait(t, shorter.handled, "the recreated bucket's read end to be handled")
	serves(t, w, "after the recreated bucket's read completed")
}

// A NATS outage ends no watcher: nats.go reconnects forever and keeps the watcher's subscription
// open through it, so only the bus's disconnect hook says the copy stopped following. The hook
// starts the stale episode, the copy is refused once the bound is past (on the injected clock),
// a Rewatch that fails while the server is down leaves the episode as it was, and the read the
// reconnect hook starts once the server is back restores serving.
func TestADisconnectStartsAStaleEpisodeThatTheReadAfterTheReconnectEnds(t *testing.T) {
	t.Parallel()
	srv, uri := testnats.Start(t)
	client, err := bus.Connect([]string{uri})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	conn := client.Connection()
	// The test's own requests (the bucket's create and its put) go through a context of the test's
	// (testnats.MaxWait), not the client's, whose requests end on the product's own bound.
	js, err := conn.JetStream(testnats.MaxWait(t))
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	kv, err := bus.EnsureKeyValue(t.Context(), conn, js, &natsgo.KeyValueConfig{Bucket: testBucketName})
	if err != nil {
		t.Fatalf("bucket: %v", err)
	}
	if _, err := kv.Put("kept", []byte("1")); err != nil {
		t.Fatalf("put: %v", err)
	}
	into := newSeen()
	now := newClock()
	w := kvwatch.New("test cache", kv, into, kvwatch.WithNow(now.Now))
	t.Cleanup(w.Stop)
	client.AddDisconnectHook(w.Disconnected)
	client.AddReconnectHook(w.Rewatch)
	w.Start()
	eventually(t, "the first read's completion", w.Following)

	srv.Stop(t)
	testwait.Until(t, "the disconnect hook", func() (bool, bool) {
		following := w.Following()
		return following, !following
	})
	if err := w.Err(); err != nil || !w.NeedsRead() {
		t.Fatalf("during the outage: Err %v, NeedsRead %v; want the watcher still running and a read needed", err, w.NeedsRead())
	}
	serves(t, w, "within the bound of the disconnect")
	now.advance(staleBound + time.Second)
	refuses(t, w, "past the bound of the disconnect")
	// A cancellable run waits for the connection to return. This call must try during the outage.
	if err := w.Rewatch(context.Background(), conn); err == nil {
		t.Fatal("a Rewatch while the server is down succeeded")
	}
	refuses(t, w, "after a Rewatch failed during the outage")

	srv.Start(t)
	// On an already reconnecting connection ForceReconnect interrupts the library's backoff.
	if conn.IsReconnecting() {
		if err := conn.ForceReconnect(); err != nil {
			t.Fatalf("force reconnect: %v", err)
		}
	}
	eventually(t, "the reconnect hook's read to complete", w.Following)
	if got := conn.Stats().Reconnects; got != 1 {
		t.Fatalf("reconnects = %d, want 1", got)
	}
	serves(t, w, "after the read that followed the reconnect")
	if !into.has("kept") || w.Err() != nil {
		t.Fatalf("after the reconnect: kept %v, Err %v; want the key and no error", into.has("kept"), w.Err())
	}
}

// An entry still buffered in a replaced watcher's updates never reaches the cache: once a Rewatch
// has replaced the watcher, its late deliveries are dropped and the new watcher's scan delivers
// each key once. Without that, a watcher replaced for a recreated bucket would apply old-bucket
// entries after the reset that emptied the cache for the new one.
//
// The test holds the first watcher's apply of late1, then queues late2 through late41 behind it.
// It releases the apply once the Rewatch is parked on applyMu and opens the queue after the switch.
func TestAReplacedWatchersBufferedEntryIsDropped(t *testing.T) {
	t.Parallel()
	uri := testnats.URL(t)
	_, kv := bucket(t, uri)
	secondConn, _ := bucket(t, uri)

	var mu sync.Mutex
	counts := map[string]int{}
	count := func(key string) int {
		mu.Lock()
		defer mu.Unlock()
		return counts[key]
	}
	ctx := testwait.Context(t)
	reading := make(chan uint64, 1)
	release := make(chan struct{})
	var blockOnce, releaseOnce sync.Once
	releaseApply := func() { releaseOnce.Do(func() { close(release) }) }
	apply := func(entry natsgo.KeyValueEntry) {
		if entry.Key() == "late1" {
			blockOnce.Do(func() {
				reading <- goroutinetest.ID()
				select {
				case <-release:
				case <-ctx.Done():
				}
			})
		}
		mu.Lock()
		counts[entry.Key()]++
		mu.Unlock()
	}

	hold, step := holdsAfter(t, 2)
	w := kvwatch.New("test cache", scripted(kv, step).handle(), withApply{seen: newSeen(), before: apply},
		kvwatch.WithAfterFunc((&pending{}).afterFunc()))
	t.Cleanup(w.Stop)
	t.Cleanup(releaseApply)
	w.Start()
	if err := w.WaitReady(ctx); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	if _, err := kv.Put("late1", []byte("1")); err != nil {
		t.Fatalf("put late1: %v", err)
	}
	consumeID := testwait.Receive(t, "the first watcher to apply late1", reading)
	const queued = 40
	keys := make([]string, 0, queued)
	for i := range queued {
		key := fmt.Sprintf("late%d", i+2)
		keys = append(keys, key)
		if _, err := kv.Put(key, []byte("1")); err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
	}
	testwait.Until(t, "all late entries to queue behind late1", func() (int, bool) {
		n := hold.queued()
		return n, n == queued
	})
	rewatching := make(chan uint64, 1)
	rewatched := make(chan error, 1)
	go func() {
		rewatching <- goroutinetest.ID()
		rewatched <- w.Rewatch(ctx, secondConn)
	}()
	id := testwait.Receive(t, "the Rewatch goroutine id", rewatching)
	eventually(t, "the Rewatch waiting for the held apply", func() bool { return parkedOnTheApplyLock(id) })
	releaseApply()
	if err := testwait.Receive(t, "the Rewatch to return", rewatched); err != nil {
		t.Fatalf("Rewatch: %v", err)
	}
	hold.open()
	returned(t, "the first watcher to drain its queued entries and return", consumeID)
	eventually(t, "the new watcher's scan to complete", w.Following)
	for _, key := range keys {
		if got := count(key); got != 1 {
			t.Fatalf("%s applied %d times, want once by the new watcher's scan", key, got)
		}
	}
	if got := count("late1"); got != 2 {
		t.Fatalf("late1 applied %d times, want once by each watcher", got)
	}
}

// slowStopKV hands out watchers whose Stop waits until released, standing in for the consumer
// delete that nats.go's Unsubscribe sends for a library-created consumer.
type slowStopKV struct {
	jetstream.KeyValue
	entered, release chan struct{}
}

func (k *slowStopKV) Watch(ctx context.Context, keys string, opts ...jetstream.WatchOpt) (jetstream.KeyWatcher, error) {
	w, err := k.KeyValue.Watch(ctx, keys, opts...)
	if err != nil {
		return nil, err
	}
	return &slowStopWatcher{KeyWatcher: w, entered: k.entered, release: k.release}, nil
}

type slowStopWatcher struct {
	jetstream.KeyWatcher
	entered, release chan struct{}
	once             sync.Once
}

func (w *slowStopWatcher) Stop() error {
	w.once.Do(func() { close(w.entered) })
	<-w.release
	return w.KeyWatcher.Stop()
}

// Two Rewatches overlap after the bucket was recreated -- the bus's reconnect hook and the
// listener's self-health rebuild both rewatch, and nothing serializes them. A sees the new stream,
// begins the new epoch's read and is still stopping the replaced watcher when B arms its own and
// reads the new bucket to completion, which sweeps the old bucket's key. A's read, overtaken before
// its watcher delivered anything, completes nothing when it does: the new bucket's key stays in the
// cache and the copy keeps following, with Err nil, so nothing would repair a sweep A ran late.
func TestConcurrentRewatchesOntoARecreatedBucketKeepTheNewKeys(t *testing.T) {
	t.Parallel()
	uri := testnats.URL(t)
	conn, kv := bucket(t, uri)
	if _, err := kv.Put("ghost", []byte("1")); err != nil {
		t.Fatalf("put: %v", err)
	}
	slow := &slowStopKV{KeyValue: kv.WatchHandle(), entered: make(chan struct{}), release: make(chan struct{})}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(slow.release) }) }
	t.Cleanup(release)
	into := newSeen()
	// No periodic probe: the test's two Rewatches are the only reads of the recreated bucket.
	w := kvwatch.New("test cache", bus.NewKeyValue(kv.Legacy(), slow, kv.Streams()), into, kvwatch.WithStreamProbeInterval(0))
	t.Cleanup(w.Stop)
	w.Start()
	eventually(t, "the first read's completion", w.Following)

	js := jetStream(t, conn)
	deleteTestBucket(t, js)
	recreated := testnats.RecreateKeyValue(t, js, &natsgo.KeyValueConfig{Bucket: testBucketName})
	if _, err := recreated.Put("fresh", []byte("1")); err != nil {
		t.Fatalf("put: %v", err)
	}

	ctx := testwait.Context(t)
	rewatching := make(chan uint64, 1)
	doneA := make(chan error, 1)
	go func() {
		rewatching <- goroutinetest.ID()
		doneA <- w.Rewatch(ctx, conn)
	}()
	idA := testwait.Receive(t, "Rewatch A's goroutine id", rewatching)
	testwait.Receive(t, "Rewatch A to stop the replaced watcher", slow.entered)
	if !into.has("ghost") {
		t.Fatal("the old bucket's key was dropped when A's read began, before any read of the new bucket completed")
	}
	if err := w.Rewatch(ctx, conn); err != nil {
		t.Fatalf("Rewatch B: %v", err)
	}
	eventually(t, "B's read to complete", w.Following)
	if !into.has("fresh") || into.has("ghost") {
		t.Fatalf("after B's read completed: fresh %v, ghost %v; want the new bucket's key only", into.has("fresh"), into.has("ghost"))
	}
	release()
	if err := testwait.Receive(t, "Rewatch A to return", doneA); err != nil {
		t.Fatalf("Rewatch A: %v", err)
	}
	testwait.Until(t, "Rewatch A's consume and stop goroutines to return", func() ([]string, bool) {
		children := startedIn(idA, "github.com/sjawhar/envoy/internal/kvwatch.(*Watcher).watch")
		return children, len(children) == 0
	})
	reads, done := into.reads(), into.completions()
	if !into.has("fresh") || into.has("ghost") || !w.Following() || w.Err() != nil || len(done) != 2 {
		t.Fatalf("fresh %v, ghost %v, Following %v, Err %v, reads completed %+v: want the new bucket's key kept by B's read and A's read completing nothing",
			into.has("fresh"), into.has("ghost"), w.Following(), w.Err(), done)
	}
	if len(reads) != 3 || !reads[1].Recreated || reads[2].Recreated || reads[2].Epoch != 1 {
		t.Fatalf("reads begun %+v, want the first, A's recreated read of epoch 1, and B's read of the same epoch", reads)
	}
}

// heldWatchKV starts its watch on the bucket, then holds it until released: the watch it serves
// has read the stream and armed its watcher, and has not yet taken the locks. Once released, the
// watcher it hands out forwards the real one's updates, and ended closes once nats.go has closed
// those.
type heldWatchKV struct {
	jetstream.KeyValue
	legacy                  natsgo.KeyValue
	streams                 jetstream.JetStream
	watched, release, ended chan struct{}
	// watcher and startID are set before watched closes.
	watcher jetstream.KeyWatcher
	startID uint64
}

func holdWatch(kv bus.KeyValue) *heldWatchKV {
	return &heldWatchKV{KeyValue: kv.WatchHandle(), legacy: kv.Legacy(), streams: kv.Streams(),
		watched: make(chan struct{}), release: make(chan struct{}), ended: make(chan struct{})}
}

// armedWatch waits for held's watch to arm its watcher (testwait.Receive), naming what it waits
// for. When the wait gives up, it releases the hold on its way out of the failing test, so the
// watch it holds ends with the test.
func armedWatch(t *testing.T, held *heldWatchKV, what string) {
	t.Helper()
	armed := false
	defer func() {
		if !armed {
			close(held.release)
		}
	}()
	testwait.Receive(t, what, held.watched)
	armed = true
}

func (k *heldWatchKV) Watch(ctx context.Context, keys string, opts ...jetstream.WatchOpt) (jetstream.KeyWatcher, error) {
	watcher, err := k.KeyValue.Watch(ctx, keys, opts...)
	k.watcher = watcher
	k.startID = goroutinetest.ID()
	close(k.watched)
	<-k.release
	if err != nil {
		return nil, err
	}
	updates := make(chan jetstream.KeyValueEntry)
	go func() {
		defer close(k.ended)
		defer close(updates)
		for entry := range watcher.Updates() {
			updates <- entry
		}
	}()
	return &forwardedWatcher{KeyWatcher: watcher, updates: updates}, nil
}

// handle is k as the bus.KeyValue a watcher takes.
func (k *heldWatchKV) handle() bus.KeyValue { return bus.NewKeyValue(k.legacy, k, k.streams) }

// A watch reads its stream before it arms its watcher and takes the locks, so a slower watch can
// arrive after a newer one has already switched to a recreated bucket. The slower watch read the
// old stream and armed its watcher there; installing it would reset the cache the newer watcher
// filled and feed it the old bucket's keys. It is discarded, and the newer watcher stays current.
func TestAWatchOfTheOldStreamDoesNotReplaceANewerOne(t *testing.T) {
	t.Parallel()
	_, uri := testnats.Start(t)
	conn, first := bucket(t, uri)
	if _, err := first.Put("ghost", []byte("1")); err != nil {
		t.Fatalf("put: %v", err)
	}
	held := holdWatch(first)
	released := false
	t.Cleanup(func() {
		if !released {
			close(held.release)
		}
	})
	into := newSeen()
	w := kvwatch.New("test cache", held.handle(), into)
	t.Cleanup(w.Stop)
	w.Start()
	testwait.Receive(t, "the first watch to arm its watcher", held.watched)

	js, err := conn.JetStream(testnats.MaxWait(t))
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	if err := js.DeleteKeyValue("kvwatch-test"); err != nil {
		t.Fatalf("delete bucket: %v", err)
	}
	recreated := testnats.RecreateKeyValue(t, js, &natsgo.KeyValueConfig{Bucket: "kvwatch-test"})
	if _, err := recreated.Put("fresh", []byte("1")); err != nil {
		t.Fatalf("put: %v", err)
	}
	liveConn, _ := bucket(t, uri)
	if err := w.Rewatch(testwait.Context(t), liveConn); err != nil {
		t.Fatalf("rewatch: %v", err)
	}
	eventually(t, "the newer watcher's scan", func() bool { return into.has("fresh") })
	generation, reads := kvwatch.Generation(w), len(into.reads())

	close(held.release)
	released = true
	returned(t, "the old stream's Start to return", held.startID)
	if gotGen, gotReads := kvwatch.Generation(w), len(into.reads()); gotGen != generation || gotReads != reads {
		t.Fatalf("the old stream's watch changed generation to %d or reads begun to %d, want %d and %d",
			gotGen, gotReads, generation, reads)
	}
	if into.has("ghost") || !into.has("fresh") {
		t.Fatalf("after the old stream's watch arrived: ghost %v, fresh %v, reads %+v; want the recreated bucket's keys only",
			into.has("ghost"), into.has("fresh"), into.reads())
	}
	if err := w.Check(); err != nil || w.Err() != nil {
		t.Fatalf("Check = %v, Err %v; want the newer watcher current and healthy", err, w.Err())
	}
	if _, err := w.KV().Put("after", []byte("1")); err != nil {
		t.Fatalf("put through the handle: %v", err)
	}
	eventually(t, "a write through the current handle", func() bool { return into.has("after") })
}

// A discarded watch's watcher is read until it ends. nats.go delivers into a watcher's 256-entry
// Updates buffer and blocks when it is full, holding the watcher's lock, and Stop only
// unsubscribes, so a watcher of an old bucket holding more keys than that, stopped with nothing
// reading it, keeps its subscription's goroutine parked for the life of the process.
func TestADiscardedWatchOfTheOldStreamIsDrainedUntilItEnds(t *testing.T) {
	t.Parallel()
	uri := testnats.URL(t)
	conn, first := bucket(t, uri)
	for i := range 257 {
		if _, err := first.Put(fmt.Sprintf("old-%d", i), []byte("1")); err != nil {
			t.Fatalf("put: %v", err)
		}
	}
	held := holdWatch(first)
	released := false
	t.Cleanup(func() {
		if !released {
			close(held.release)
		}
	})
	into := newSeen()
	w := kvwatch.New("test cache", held.handle(), into, kvwatch.WithAfterFunc((&pending{}).afterFunc()))
	t.Cleanup(w.Stop)
	w.Start()
	testwait.Receive(t, "the first watch to arm its watcher", held.watched)
	updates := held.watcher.Updates()
	testwait.Until(t, "the held watcher's buffer to fill", func() (int, bool) {
		buffered := len(updates)
		return buffered, buffered == cap(updates)
	})

	js, err := conn.JetStream(testnats.MaxWait(t))
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	if err := js.DeleteKeyValue("kvwatch-test"); err != nil {
		t.Fatalf("delete bucket: %v", err)
	}
	testnats.RecreateKeyValue(t, js, &natsgo.KeyValueConfig{Bucket: "kvwatch-test"})
	liveConn, _ := bucket(t, uri)
	if err := w.Rewatch(testwait.Context(t), liveConn); err != nil {
		t.Fatalf("rewatch: %v", err)
	}
	close(held.release)
	released = true

	testwait.Receive(t, "the discarded watcher to end: left unread (its delivery goroutine parks on a full buffer) or never stopped, it never does", held.ended)
}

// Readiness is the current watcher's to release. A watcher a Rewatch replaced mid-scan still reads
// its own sentinel, and releasing readiness then would let a caller read a cache the current
// watcher has only partly filled.
func TestAReplacedWatchersSentinelDoesNotReleaseReadiness(t *testing.T) {
	t.Parallel()
	uri := testnats.URL(t)
	_, first := bucket(t, uri)
	for i := range 10 {
		if _, err := first.Put(fmt.Sprintf("k%03d", i), []byte("1")); err != nil {
			t.Fatalf("put: %v", err)
		}
	}
	reading := make(chan uint64, 1)
	var firstApply sync.Once
	var firstID uint64 // Set and read by the apply hooks under applyMu.
	var firstScanApplied atomic.Int32
	into := newSeen()
	apply := func(entry natsgo.KeyValueEntry) {
		key := entry.Key()
		if key == "k000" {
			firstApply.Do(func() {
				firstID = goroutinetest.ID()
				reading <- firstID
			})
		} else if key > "k000" && key < "k010" && firstID != 0 && goroutinetest.ID() == firstID {
			firstScanApplied.Add(1)
		}
	}
	gate, step := holdsAfter(t, 1)
	w := kvwatch.New("probe", scripted(first, step).handle(), withApply{seen: into, before: apply},
		kvwatch.WithAfterFunc((&pending{}).afterFunc()))
	t.Cleanup(w.Stop)
	w.Start()
	consumeID := testwait.Receive(t, "the first watcher to apply k000", reading)
	testwait.Receive(t, "the first watcher to queue its sentinel", gate.sentinelQueued)
	for i := 10; i < 200; i++ {
		if _, err := first.Put(fmt.Sprintf("k%03d", i), []byte("1")); err != nil {
			t.Fatalf("put: %v", err)
		}
	}
	_, live := bucket(t, uri)
	held := newHeldEnd(t)
	if err := kvwatch.WatchOn(w, scripted(live, held.step()).handle()); err != nil {
		t.Fatalf("the replacing watch: %v", err)
	}
	testwait.Receive(t, "the replacing watch to reach its held sentinel", held.reached)
	gate.open()
	returned(t, "the first watcher to drain its queued entries and return", consumeID)
	select {
	case <-gate.sentinelTaken:
	default:
		t.Fatal("the replaced watcher returned without taking its sentinel")
	}
	if got := firstScanApplied.Load(); got != 0 {
		t.Fatalf("the replaced watcher applied %d entries after its held k000, want none", got)
	}
	if w.Ready() {
		t.Fatal("the replaced watcher's sentinel released readiness while the current sentinel was held")
	}
	held.let()
	if err := w.WaitReady(testwait.Context(t)); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	if !into.has("k199") {
		t.Fatal("ready while the current watcher's scan was incomplete: k199 missing")
	}
}

// A bucket deleted and replaced by a stream with an older creation time -- a JetStream restore of a
// snapshot taken before the bucket was last recreated, or a recreate after the clock stepped back --
// is still a recreated bucket. The stale-watch discard compares creation times, so it must only
// apply against a live, healthy watcher; otherwise every Rewatch would discard itself and the
// cache would keep the deleted bucket's keys.
//
// nats-server v2.15.1 stamps a restored stream with the time of the restore (restoreStream), so the
// stream the server restores here is newer than the live one. The test's reads therefore go through
// kvwatch.WatchOn, the install a Rewatch makes once it has opened its handle, on a handle that reads
// the restored stream with the first bucket's creation time (createdAs), as a restore that keeps the
// snapshot's creation time reports it. Every request the test makes itself, and every wait of its
// own, ends only near the test binary's deadline (testContext, testwait), not at a bound of
// nats.go's own.
func TestARewatchOntoABucketRestoredWithAnOlderStreamRefillsTheCache(t *testing.T) {
	t.Parallel()
	const stream = "KV_" + testBucketName
	ctx := testwait.Context(t)
	// The first bucket lives, and is deleted, on a server of its own. nats-server's file store
	// answers a stream delete once it has renamed the stream's directory to "."+name, and removes
	// that directory in a goroutine of its own (filestore.go, fileStore.Delete). A second delete of
	// the same name on the same server, while that removal runs, fails its own rename, leaves the
	// deleted stream's messages where the restore then makes its stream, and still answers
	// success; the restore then fails on those messages. On the restoring server below, the live
	// bucket's delete is the only delete of the name.
	_, snapshotURI := testnats.Start(t)
	snapshotConn := testnats.Connect(t, snapshotURI)
	t.Cleanup(snapshotConn.Close)
	snapshotStreams, err := jetstream.New(snapshotConn, testnats.RequestTimeout(t))
	if err != nil {
		t.Fatalf("jetstream on the snapshot's server: %v", err)
	}
	first, err := snapshotStreams.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: testBucketName})
	if err != nil {
		t.Fatalf("create the first bucket: %v", err)
	}
	if _, err := first.Put(ctx, "restored-key", []byte("1")); err != nil {
		t.Fatalf("put restored-key: %v", err)
	}
	firstStream, err := snapshotStreams.Stream(ctx, stream)
	if err != nil {
		t.Fatalf("read the first bucket's stream: %v", err)
	}
	firstCreated := firstStream.CachedInfo().Created
	snap := snapshotStream(t, snapshotConn, stream)
	if err := snapshotStreams.DeleteKeyValue(ctx, testBucketName); err != nil {
		t.Fatalf("delete the first bucket: %v", err)
	}

	// The restoring server, and on it the live bucket: a newer stream the watcher installs.
	_, uri := testnats.Start(t)
	conn, live := bucket(t, uri)
	streams := live.Streams()
	// accountKeeper exists so the account is never emptied by the delete below. nats-server
	// schedules the removal of the account's streams directory, in the background, on every stream
	// delete, not only on the last one; that removal is an os.Remove, which fails on a directory that
	// is not empty. Without the keeper, the delete below leaves the directory empty — once the
	// deleted stream's renamed copy, which goes in a goroutine of its own, is gone — so the removal
	// it schedules can succeed, and the restore after the delete races it and fails. With the
	// keeper's stream in it, the directory is never empty, so every scheduled removal fails and
	// leaves it in place.
	const accountKeeper = "kvwatch-test-account-keeper"
	if _, err := streams.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: accountKeeper}); err != nil {
		t.Fatalf("create the account keeper bucket: %v", err)
	}
	if _, err := live.WatchHandle().Put(ctx, "live-key", []byte("1")); err != nil {
		t.Fatalf("put live-key: %v", err)
	}
	into := newSeen()
	// No stream probe: a probe that found the restored stream would read it again through the live
	// bucket's handle, at the restore's own creation time, before the reads below.
	w := kvwatch.New("probe cache", live, into, kvwatch.WithStreamProbeInterval(0))
	t.Cleanup(w.Stop)
	w.Start()
	type watchState struct {
		Following bool
		NeedsRead bool
		Err       error
		Epoch     uint64
		Keys      []string
	}
	readState := func() watchState {
		return watchState{
			Following: w.Following(),
			NeedsRead: w.NeedsRead(),
			Err:       w.Err(),
			Epoch:     w.Epoch(),
			Keys:      into.keyList(),
		}
	}
	testwait.Until(t, "the live bucket's read to complete", func() (watchState, bool) {
		state := readState()
		return state, state.Following
	})
	if !into.has("live-key") {
		t.Fatal("the live bucket's read completed without live-key")
	}

	keeperPresent := false
	names := streams.StreamNames(ctx)
	for name := range names.Name() {
		keeperPresent = keeperPresent || name == "KV_"+accountKeeper
	}
	if err := names.Err(); err != nil {
		t.Fatalf("list the streams: %v", err)
	}
	if !keeperPresent {
		t.Fatal("the account keeper bucket is gone before the delete: the delete would empty the account and the restore would race the streams-directory removal")
	}
	// The live stream's creation time, read before its delete: after the restore, a read through
	// the live bucket's handle reads the restored stream, which has the same name.
	liveStream, err := streams.Stream(ctx, stream)
	if err != nil {
		t.Fatalf("read the live bucket's stream: %v", err)
	}
	liveCreated := liveStream.CachedInfo().Created
	// Delete it and restore the older snapshot in its place.
	if err := streams.DeleteKeyValue(ctx, testBucketName); err != nil {
		t.Fatalf("delete the live bucket: %v", err)
	}
	restoreStream(t, conn, stream, snap)
	older := bus.NewKeyValue(live.Legacy(), live.WatchHandle(), createdAs{JetStream: streams, created: firstCreated})
	olderState, err := older.ReadStreamState(ctx)
	if err != nil || !olderState.Created.Before(liveCreated) {
		t.Fatalf("the restored stream reads created %v (%v) through the handle the reads use, and the live bucket's stream was created %v; want the restored stream older",
			olderState.Created, err, liveCreated)
	}

	if err := w.Check(); err != nil {
		t.Fatalf("Check: %v", err)
	}
	if w.Err() == nil {
		t.Fatal("Check recorded no error after the restore: the rule under test holds only while Check's error is recorded")
	}
	// While Check's error is recorded, a watch onto the older stream installs at once: a read of a
	// recreated bucket begins before WatchOn returns. A watch that discarded itself against the
	// older stream begins no read.
	begun := len(into.reads())
	if err := kvwatch.WatchOn(w, older); err != nil {
		t.Fatalf("the watch onto the restored bucket: %v", err)
	}
	if reads := into.reads(); len(reads) != begun+1 || !reads[begun].Recreated {
		t.Fatalf("the watch onto the restored bucket left the reads begun at %+v; want one more, of a recreated bucket: the watch was discarded against the older stream while Check's error was recorded",
			reads)
	}
	// The listener's self-health rebuild rewatches a copy that needs a read on every tick of its
	// own, so the wait does the same: it reads the restored bucket again whenever the copy is
	// waiting for a read, and ends when the read of the restored bucket has completed.
	testwait.Until(t, "the restored bucket's read to complete", func() (watchState, bool) {
		state := readState()
		if state.Following {
			return state, true
		}
		if state.NeedsRead {
			if err := kvwatch.WatchOn(w, older); err != nil {
				t.Fatalf("the self-health rebuild's read of the restored bucket: %v", err)
			}
		}
		_ = w.Check()
		state = readState()
		return state, state.Following
	})
	if w.Err() != nil || !into.has("restored-key") || into.has("live-key") || w.Epoch() == 0 {
		t.Fatalf("after the reads of the restored bucket: Err=%v, restored-key=%v, live-key=%v, Epoch=%d; "+
			"want the restored bucket's key only, read as a new epoch", w.Err(), into.has("restored-key"), into.has("live-key"), w.Epoch())
	}
}

// snapshot is a stream snapshot as the server sent it: the stream's config and state, and the
// data chunks.
type snapshot struct {
	config, state json.RawMessage
	chunks        [][]byte
}

// snapshotStream snapshots stream through the raw JetStream API, which nats.go does not wrap. Each
// request and each chunk waits until testContext's end.
func snapshotStream(t *testing.T, conn *natsgo.Conn, stream string) snapshot {
	t.Helper()
	ctx := testwait.Context(t)
	inbox := natsgo.NewInbox()
	chunks, err := conn.SubscribeSync(inbox)
	if err != nil {
		t.Fatalf("snapshot %s: subscribe: %v", stream, err)
	}
	req, _ := json.Marshal(map[string]any{"deliver_subject": inbox, "no_consumers": true})
	msg, err := conn.RequestWithContext(ctx, "$JS.API.STREAM.SNAPSHOT."+stream, req)
	if err != nil {
		t.Fatalf("snapshot %s: request: %v", stream, err)
	}
	var resp struct {
		Config json.RawMessage               `json:"config"`
		State  json.RawMessage               `json:"state"`
		Error  *struct{ Description string } `json:"error"`
	}
	if err := json.Unmarshal(msg.Data, &resp); err != nil || resp.Error != nil {
		t.Fatalf("snapshot %s: %v %s", stream, err, msg.Data)
	}
	snap := snapshot{config: resp.Config, state: resp.State}
	for {
		m, err := chunks.NextMsgWithContext(ctx)
		if err != nil {
			t.Fatalf("snapshot %s: chunk %d: %v", stream, len(snap.chunks), err)
		}
		if len(m.Data) == 0 {
			return snap
		}
		snap.chunks = append(snap.chunks, append([]byte(nil), m.Data...))
		if m.Reply != "" {
			_ = conn.Publish(m.Reply, nil)
		}
	}
}

// restoreStream restores snap as stream through the raw JetStream API, each request waiting until
// testContext's end, and fails the test unless the server answers the end of the data with no error:
// a restore that fails can still leave a stream behind (nats-server v2.15.1 stream_backup.go,
// RestoreStreamV2 makes the stream it built active on its way out). nats-server v2.15.1 gives the
// restored stream the time of the restore as its creation time, not the snapshot's.
func restoreStream(t *testing.T, conn *natsgo.Conn, stream string, snap snapshot) {
	t.Helper()
	ctx := testwait.Context(t)
	req, _ := json.Marshal(map[string]any{"config": snap.config, "state": snap.state})
	msg, err := conn.RequestWithContext(ctx, "$JS.API.STREAM.RESTORE."+stream, req)
	if err != nil {
		t.Fatalf("restore %s: request: %v", stream, err)
	}
	var resp struct {
		DeliverSubject string                        `json:"deliver_subject"`
		Error          *struct{ Description string } `json:"error"`
	}
	if err := json.Unmarshal(msg.Data, &resp); err != nil || resp.Error != nil {
		t.Fatalf("restore %s: %v %s", stream, err, msg.Data)
	}
	for i, chunk := range snap.chunks {
		if _, err := conn.RequestWithContext(ctx, resp.DeliverSubject, chunk); err != nil {
			t.Fatalf("restore %s: chunk %d: %v", stream, i, err)
		}
	}
	end, err := conn.RequestWithContext(ctx, resp.DeliverSubject, nil)
	if err != nil {
		t.Fatalf("restore %s: end of data: %v", stream, err)
	}
	var done struct {
		Error *struct{ Description string } `json:"error"`
	}
	if err := json.Unmarshal(end.Data, &done); err != nil {
		t.Fatalf("restore %s: read the server's answer to the end of the data, %s: %v", stream, end.Data, err)
	}
	if done.Error != nil {
		t.Fatalf("restore %s: the server did not restore the snapshot: %s", stream, end.Data)
	}
}

// createdAs is a bucket's jetstream context whose every stream read reports the stream's creation
// time as created, as a server that keeps it across a restore of a snapshot reports it: the rest of
// what the read reports, its sequences among it, is the server's.
type createdAs struct {
	jetstream.JetStream
	created time.Time
}

func (s createdAs) Stream(ctx context.Context, name string) (jetstream.Stream, error) {
	stream, err := s.JetStream.Stream(ctx, name)
	if err != nil {
		return nil, err
	}
	return createdStream{Stream: stream, created: s.created}, nil
}

// createdStream is a stream read whose cached information reports created as its creation time.
type createdStream struct {
	jetstream.Stream
	created time.Time
}

func (s createdStream) CachedInfo() *jetstream.StreamInfo {
	info := *s.Stream.CachedInfo()
	info.Created = s.created
	return &info
}

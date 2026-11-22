package session

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// gate holds one hook of a read until the test opens it. reached closes once the hook is held;
// done closes once the hook has run on, after the gate opened.
type gate struct {
	reached, release, done chan struct{}
	once                   sync.Once
}

func newGate(t *testing.T) *gate {
	g := &gate{reached: make(chan struct{}), release: make(chan struct{}), done: make(chan struct{})}
	t.Cleanup(g.open)
	return g
}

func (g *gate) open() { g.once.Do(func() { close(g.release) }) }

// pass holds the caller at the gate until it opens, then runs through, and says it ran.
func (g *gate) pass(through func()) {
	close(g.reached)
	<-g.release
	through()
	close(g.done)
}

// waitClosed waits for ch, which names what, to close. Its one bound is the test binary's
// deadline (testwait.Receive): a loaded machine that is slow to reach a gate is not a failure.
func waitClosed(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	testwait.Receive(t, what, ch)
}

// holdingCache is the registry's cache with a test's gates in front of its hooks: the next apply
// of a key a test holds, and the next read's completion, wait for the test. A completion held is a
// read that has delivered every record and has not yet swept, the moment a list during a read sees.
type holdingCache struct {
	kvwatch.Cache
	mu         sync.Mutex
	applies    map[string]*gate
	completion *gate
	afterSweep func()
}

func (c *holdingCache) Apply(entry kvwatch.Entry, read kvwatch.Read) {
	c.mu.Lock()
	g := c.applies[entry.Key]
	delete(c.applies, entry.Key)
	c.mu.Unlock()
	if g == nil {
		c.Cache.Apply(entry, read)
		return
	}
	g.pass(func() { c.Cache.Apply(entry, read) })
}

func (c *holdingCache) CompleteRead(read kvwatch.Read) {
	c.mu.Lock()
	g, afterSweep := c.completion, c.afterSweep
	c.completion, c.afterSweep = nil, nil
	c.mu.Unlock()
	if g == nil {
		c.Cache.CompleteRead(read)
		return
	}
	g.pass(func() {
		c.Cache.CompleteRead(read)
		if afterSweep != nil {
			afterSweep()
		}
	})
}

// holdApply holds the next apply of key.
func (c *holdingCache) holdApply(t *testing.T, key string) *gate {
	g := newGate(t)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.applies == nil {
		c.applies = map[string]*gate{}
	}
	c.applies[key] = g
	return g
}

// holdCompletion holds the next read's completion. afterSweep, when not nil, runs right after its
// sweep, before the watcher applies any later record, so it sees the copy the sweep left.
func (c *holdingCache) holdCompletion(t *testing.T, afterSweep func()) *gate {
	g := newGate(t)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.completion, c.afterSweep = g, afterSweep
	return g
}

// holdingRegistry opens a registry on the test's own session bucket whose watcher feeds it
// through a holdingCache, and waits for that watcher's first read to complete.
//
// The bucket is created with no maximum age (agelessSessionBucket), and the registry runs with no
// TTL and reads a clock held at the moment it opened (heldClock), unless the test's options say
// otherwise. So no record leaves the bucket before the read the test holds finds it, no row
// passes a deadline, and every session the test registers stays within its live window, however
// long the machine takes over that read: a row's state changes only when the test moves a clock.
func holdingRegistry(t *testing.T, client *bus.Client, options ...SessionRegistryOption) (*SessionRegistry, *holdingCache) {
	t.Helper()
	agelessSessionBucket(t, client)
	clock := &heldClock{now: time.Now()}
	defaults := []SessionRegistryOption{WithSessionReplicas(1), WithSessionTTL(0), WithSessionClock(clock.Now)}
	r, err := OpenSessionRegistry(t.Context(), client.Conn, append(defaults, options...)...)
	if err != nil {
		t.Fatalf("open session registry: %v", err)
	}
	waitFollowing(t, r)
	// The bucket is empty, so once the first read has completed nothing more reaches the cache
	// through the watcher replaced here.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r.watcher.End(ctx)
	held := &holdingCache{Cache: sessionCache{r}}
	r.watcher = kvwatch.New("session registry", r.watcher.KV(), held)
	t.Cleanup(r.StopWatch)
	r.watcher.Start()
	waitFollowing(t, r)
	return r, held
}

// listed is the ids of the sessions List answers.
func listed(t *testing.T, r *SessionRegistry) []string {
	t.Helper()
	entries, err := r.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	return idsOf(entries)
}

// sweptList is the ids of the sessions List answers, for a hook to read right after a sweep. List
// never fails, and a hook runs outside the test's goroutine, where it may not fail the test.
func sweptList(r *SessionRegistry) []string {
	entries, _ := r.List()
	return idsOf(entries)
}

func idsOf(entries []ListEntry) []string {
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		ids = append(ids, entry.SessionID)
	}
	return ids
}

func titleOf(t *testing.T, r *SessionRegistry, sessionID string) string {
	t.Helper()
	entry, err := r.Get(sessionID)
	if err != nil {
		t.Fatalf("Get(%s): %v", sessionID, err)
	}
	return entry.Title
}

func putRaw(t *testing.T, kv natsgo.KeyValue, sessionID string, entry SessionEntry) uint64 {
	t.Helper()
	value, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("encode %s: %v", sessionID, err)
	}
	revision, err := kv.Put(sessionID, value)
	if err != nil {
		t.Fatalf("put %s: %v", sessionID, err)
	}
	return revision
}

// A heartbeat that lands while the read is under way, before the read applies the older value it
// delivered for the same session, wins: the older value is fenced out, and it still keeps the
// session through the sweep.
func TestAHeartbeatNewerThanTheReadsValueWinsAndIsKept(t *testing.T) {
	client := setupNATS(t)
	r, held := holdingRegistry(t, client)
	putRaw(t, r.watcher.KV().Legacy(), "ses_beat", SessionEntry{Port: 1, MachineID: "m1", Title: "scanned", UpdatedAt: time.Now().UnixMilli()})
	// The current watcher has applied it, so the held apply below is the new read's.
	waitFor(t, "the first watcher to apply ses_beat", func() bool {
		entry, err := r.Get("ses_beat")
		return err == nil && entry.Title == "scanned"
	})

	apply := held.holdApply(t, "ses_beat")
	completion := held.holdCompletion(t, nil)
	if err := r.Rewatch(context.Background(), client.Conn); err != nil {
		t.Fatalf("Rewatch: %v", err)
	}
	waitClosed(t, apply.reached, "the read to deliver ses_beat")
	if err := r.Put("ses_beat", SessionEntry{Port: 1, MachineID: "m1", Title: "heartbeat"}); err != nil {
		t.Fatalf("heartbeat during the read: %v", err)
	}
	apply.open()
	waitClosed(t, completion.reached, "the read to deliver every record")
	if got := titleOf(t, r, "ses_beat"); got != "heartbeat" {
		t.Fatalf("during the read ses_beat = %q, want the heartbeat's value over the older one the read delivered", got)
	}
	completion.open()
	waitClosed(t, completion.done, "the read's sweep")
	if got := titleOf(t, r, "ses_beat"); got != "heartbeat" {
		t.Fatalf("after the read ses_beat = %q, want the heartbeat's value", got)
	}
	if got := listed(t, r); !slices.Equal(got, []string{"ses_beat"}) {
		t.Fatalf("listed after the read = %v, want ses_beat kept", got)
	}
}

// A session registered while the read is under way, after the read delivered every record, is in
// the bucket: the sweep keeps it, before the watcher delivers its record.
func TestASessionRegisteredDuringTheReadIsKeptThroughTheSweep(t *testing.T) {
	client := setupNATS(t)
	r, held := holdingRegistry(t, client)
	if err := r.Put("ses_before", SessionEntry{Port: 1, MachineID: "m1"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	var swept []string
	completion := held.holdCompletion(t, func() { swept = sweptList(r) })
	if err := r.Rewatch(context.Background(), client.Conn); err != nil {
		t.Fatalf("Rewatch: %v", err)
	}
	waitClosed(t, completion.reached, "the read to deliver every record")
	if err := r.Put("ses_during", SessionEntry{Port: 2, MachineID: "m1"}); err != nil {
		t.Fatalf("register during the read: %v", err)
	}
	completion.open()
	waitClosed(t, completion.done, "the read's sweep")
	if want := []string{"ses_before", "ses_during"}; !slices.Equal(swept, want) {
		t.Fatalf("listed right after the sweep = %v, want %v", swept, want)
	}
	waitFollowing(t, r)
	if got, want := listed(t, r), []string{"ses_before", "ses_during"}; !slices.Equal(got, want) {
		t.Fatalf("listed after the read = %v, want %v", got, want)
	}
}

// A session deleted while the read is under way, after the read delivered its record, is gone
// once the read completes, and nothing brings it back.
func TestASessionDeletedDuringTheReadStaysGone(t *testing.T) {
	client := setupNATS(t)
	r, held := holdingRegistry(t, client)
	for _, sessionID := range []string{"ses_gone", "ses_kept"} {
		if err := r.Put(sessionID, SessionEntry{Port: 1, MachineID: "m1"}); err != nil {
			t.Fatalf("Put %s: %v", sessionID, err)
		}
	}
	var swept []string
	completion := held.holdCompletion(t, func() { swept = sweptList(r) })
	if err := r.Rewatch(context.Background(), client.Conn); err != nil {
		t.Fatalf("Rewatch: %v", err)
	}
	waitClosed(t, completion.reached, "the read to deliver every record")
	if err := r.Delete(context.Background(), "ses_gone"); err != nil {
		t.Fatalf("Delete during the read: %v", err)
	}
	completion.open()
	waitClosed(t, completion.done, "the read's sweep")
	if want := []string{"ses_kept"}; !slices.Equal(swept, want) {
		t.Fatalf("listed right after the sweep = %v, want %v", swept, want)
	}
	waitFollowing(t, r)
	if _, err := r.Refresh(context.Background(), "ses_gone"); !errors.Is(err, natsgo.ErrKeyNotFound) {
		t.Fatalf("Refresh(ses_gone) after the read = %v, want not found", err)
	}
	if got, want := listed(t, r), []string{"ses_kept"}; !slices.Equal(got, want) {
		t.Fatalf("listed after the read = %v, want %v", got, want)
	}
}

// delayingKV is a session bucket handle whose next Put or Get of a key a test delays goes to the
// bucket at once and answers only once the test opens its gate, so the answer arrives after
// whatever the test does in between.
type delayingKV struct {
	natsgo.KeyValue
	mu         sync.Mutex
	puts, gets map[string]*gate
}

func (kv *delayingKV) delay(t *testing.T, calls *map[string]*gate, key string) *gate {
	g := newGate(t)
	kv.mu.Lock()
	defer kv.mu.Unlock()
	if *calls == nil {
		*calls = map[string]*gate{}
	}
	(*calls)[key] = g
	return g
}

func (kv *delayingKV) take(calls map[string]*gate, key string) *gate {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	g := calls[key]
	delete(calls, key)
	return g
}

func (kv *delayingKV) Put(key string, value []byte) (uint64, error) {
	revision, err := kv.KeyValue.Put(key, value)
	if g := kv.take(kv.puts, key); g != nil {
		g.pass(func() {})
	}
	return revision, err
}

func (kv *delayingKV) Get(key string) (natsgo.KeyValueEntry, error) {
	entry, err := kv.KeyValue.Get(key)
	if g := kv.take(kv.gets, key); g != nil {
		g.pass(func() {})
	}
	return entry, err
}

// A heartbeat or a bucket read that a recreate of the session bucket overtakes is the old
// bucket's: its result reaches the cache after the new bucket's read has completed, and is
// dropped. Applied, its revision would fence out the new bucket's revisions, which number from 1
// again, and the copy would keep the old value past every later write to the new bucket.
func TestAResultFromABucketSinceRecreatedIsDropped(t *testing.T) {
	for _, test := range []struct {
		name  string
		delay func(t *testing.T, kv *delayingKV) *gate
		call  func(r *SessionRegistry) error
	}{
		{
			name:  "a heartbeat",
			delay: func(t *testing.T, kv *delayingKV) *gate { return kv.delay(t, &kv.puts, "ses_a") },
			call: func(r *SessionRegistry) error {
				return r.Put("ses_a", SessionEntry{Port: 1, MachineID: "m1", Title: "old"})
			},
		},
		{
			name:  "a bucket read",
			delay: func(t *testing.T, kv *delayingKV) *gate { return kv.delay(t, &kv.gets, "ses_a") },
			call: func(r *SessionRegistry) error {
				_, err := r.Refresh(context.Background(), "ses_a")
				return err
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := setupNATS(t)
			// No TTL on the registry or on either bucket, and a clock held from the start: nothing
			// the row reads expires or leaves the live listing, however long the machine takes.
			clock := &heldClock{now: time.Now()}
			r, err := OpenSessionRegistry(t.Context(), client.Conn, WithSessionReplicas(1), WithSessionTTL(0), WithSessionClock(clock.Now))
			if err != nil {
				t.Fatalf("open session registry: %v", err)
			}
			t.Cleanup(r.StopWatch)
			delayed := &delayingKV{KeyValue: r.watcher.KV().Legacy()}
			useKV(t, r, delayed)
			// The old bucket's revisions run well past the new bucket's.
			for range 20 {
				putRaw(t, delayed.KeyValue, "ses_filler", SessionEntry{Port: 9, MachineID: "m1"})
			}
			if err := r.Put("ses_a", SessionEntry{Port: 1, MachineID: "m1", Title: "old"}); err != nil {
				t.Fatalf("Put: %v", err)
			}

			held := test.delay(t, delayed)
			done := make(chan error, 1)
			go func() { done <- test.call(r) }()
			waitClosed(t, held.reached, "the old bucket to answer the delayed call")

			js, err := client.Conn.JetStream()
			if err != nil {
				t.Fatalf("jetstream: %v", err)
			}
			if err := js.DeleteKeyValue(bus.SessionBucket); err != nil {
				t.Fatalf("delete the session bucket: %v", err)
			}
			recreated := testnats.RecreateKeyValue(t, js, &natsgo.KeyValueConfig{Bucket: bus.SessionBucket})
			putRaw(t, recreated, "ses_a", SessionEntry{Port: 1, MachineID: "m1", Title: "new", UpdatedAt: time.Now().UnixMilli()})
			putRaw(t, recreated, "ses_b", SessionEntry{Port: 2, MachineID: "m1", UpdatedAt: time.Now().UnixMilli()})
			if err := r.Rewatch(context.Background(), client.Conn); err != nil {
				t.Fatalf("Rewatch: %v", err)
			}
			waitFor(t, "the watcher to follow the recreated bucket", func() bool { return r.watcher.Epoch() == 1 && r.watcher.Following() })

			held.open()
			if err := testwait.Receive(t, "the delayed call to answer once its gate opened", done); err != nil {
				t.Fatalf("the delayed call: %v", err)
			}
			if got := titleOf(t, r, "ses_a"); got != "new" {
				t.Fatalf("ses_a = %q after the old bucket's answer, want the new bucket's", got)
			}
			if got, want := listed(t, r), []string{"ses_a", "ses_b"}; !slices.Equal(got, want) {
				t.Fatalf("listed = %v, want the new bucket's sessions %v", got, want)
			}
			putRaw(t, recreated, "ses_a", SessionEntry{Port: 1, MachineID: "m1", Title: "newer", UpdatedAt: time.Now().UnixMilli()})
			waitFor(t, "the watcher to apply ses_a's newer value", func() bool {
				entry, err := r.Get("ses_a")
				return err == nil && entry.Title == "newer"
			})
		})
	}
}

// refreshed is what one Refresh answered.
type refreshed struct {
	entry SessionEntry
	err   error
}

// A Refresh whose bucket read a watch overtakes -- the epoch moved to a recreated bucket while the
// read was in flight -- is not applied, and answers the live entry the bucket returned, not the
// copy, which the new bucket's read has not yet given that session. Every Refresh caller is about
// to take something away from a session that looks gone.
func TestARefreshOvertakenByAWatchAnswersTheBucketsEntry(t *testing.T) {
	client := setupNATS(t)
	// No TTL on the registry or on either bucket: the session the refresh reads is live however
	// long the machine takes between its write and the answer.
	r, err := OpenSessionRegistry(t.Context(), client.Conn, WithSessionReplicas(1), WithSessionTTL(0))
	if err != nil {
		t.Fatalf("open session registry: %v", err)
	}
	waitFollowing(t, r)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r.watcher.End(ctx)
	base := r.watcher.KV()
	delayed := &delayingKV{KeyValue: base.Legacy()}
	held := &holdingCache{Cache: sessionCache{r}}
	// No probe: the Rewatch below is the only thing that moves the epoch, so the new bucket's one
	// read is the one whose apply of ses_a the test holds.
	r.watcher = kvwatch.New("session registry", bus.NewKeyValue(delayed, base.WatchHandle(), base.Streams()), held,
		kvwatch.WithStreamProbeInterval(0))
	t.Cleanup(r.StopWatch)
	r.watcher.Start()
	waitFollowing(t, r)
	// With no probe, the watcher still finds the recreate below by itself once a record of the new
	// bucket reaches it: nats.go moves the old ordered consumer onto the new stream, at the last
	// sequence it delivered + 1, 5 to 20 s after the delete (it looks for activity every 10 s and
	// resets at a look that finds none since the one before), and a record delivered then is checked
	// against the bucket's stream, which starts a read of the new bucket (kvwatch confirmStream). The
	// old consumer has delivered more records than the new bucket will hold, so it has nothing of the
	// new bucket to deliver, and the Rewatch stays the only thing that moves the epoch however long
	// the machine takes before it.
	var last uint64
	for range 20 {
		last = putRaw(t, base.Legacy(), "ses_filler", SessionEntry{Port: 9, MachineID: "m1"})
	}
	waitFor(t, "the watcher to deliver every filler record", func() bool {
		r.mu.RLock()
		defer r.mu.RUnlock()
		return r.cache.FenceFor(r.watcher.Epoch(), "ses_filler") >= last
	})

	js, err := client.Conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	if err := js.DeleteKeyValue(bus.SessionBucket); err != nil {
		t.Fatalf("delete the session bucket: %v", err)
	}
	recreated := testnats.RecreateKeyValue(t, js, &natsgo.KeyValueConfig{Bucket: bus.SessionBucket})
	putRaw(t, recreated, "ses_a", SessionEntry{Port: 1, MachineID: "m1", Title: "live", UpdatedAt: time.Now().UnixMilli()})

	get := delayed.delay(t, &delayed.gets, "ses_a")
	done := make(chan refreshed, 1)
	go func() {
		entry, err := r.Refresh(context.Background(), "ses_a")
		done <- refreshed{entry: entry, err: err}
	}()
	waitClosed(t, get.reached, "the bucket to answer the refresh's read")
	apply := held.holdApply(t, "ses_a")
	if err := r.Rewatch(context.Background(), client.Conn); err != nil {
		t.Fatalf("Rewatch: %v", err)
	}
	waitClosed(t, apply.reached, "the new bucket's read to deliver ses_a")
	if _, err := r.Get("ses_a"); !errors.Is(err, natsgo.ErrKeyNotFound) || r.watcher.Epoch() != 1 {
		t.Fatalf("before the refresh answers: Get(ses_a) = %v in epoch %d, want not found yet in epoch 1", err, r.watcher.Epoch())
	}

	get.open()
	got := testwait.Receive(t, "the overtaken Refresh to answer once its gate opened", done)
	if got.err != nil || got.entry.Title != "live" {
		t.Fatalf("Refresh(ses_a) overtaken by the watch = %+v, %v; want the live entry the bucket returned", got.entry, got.err)
	}
	apply.open()
	waitClosed(t, apply.done, "the new bucket's read to apply ses_a")
	if got := titleOf(t, r, "ses_a"); got != "live" {
		t.Fatalf("ses_a = %q once the new bucket's read applied it, want its value", got)
	}
}

// A Refresh whose read the revision fence drops answers the entry the bucket returned. The fence
// holds a revision of the bucket incarnation the copy still follows -- here a delete there -- and a
// bucket recreated under the watcher, which has not yet found it, numbers its revisions from 1
// again, so the new bucket's live session reads below that fence. Answered from the copy, the
// session would be gone. A read the fence drops still answers as Get would for its row: once the
// row is past its TTL, it is not found.
//
// The recreated bucket has no maximum age of its own, so the server keeps the new session's record
// however long the machine takes, and the registry reads a clock the test holds: the record is
// within its TTL until the test moves that clock one TTL past the Created the server stamped on it.
func TestARefreshTheFenceDropsAnswersTheBucketsEntry(t *testing.T) {
	client := setupNATS(t)
	clock := &heldClock{now: time.Now()}
	r, err := OpenSessionRegistry(t.Context(), client.Conn, WithSessionReplicas(1), WithSessionTTL(time.Minute), WithSessionClock(clock.Now))
	if err != nil {
		t.Fatalf("open session registry: %v", err)
	}
	waitFollowing(t, r)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r.watcher.End(ctx)
	// No probe, so nothing finds the recreate below while the test runs.
	r.watcher = kvwatch.New("session registry", r.watcher.KV(), sessionCache{r}, kvwatch.WithStreamProbeInterval(0))
	t.Cleanup(r.StopWatch)
	r.watcher.Start()
	waitFollowing(t, r)

	for range 20 {
		putRaw(t, r.watcher.KV().Legacy(), "ses_filler", SessionEntry{Port: 9, MachineID: "m1"})
	}
	if err := r.Put("ses_a", SessionEntry{Port: 1, MachineID: "m1", Title: "old"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := r.Delete(context.Background(), "ses_a"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	js, err := client.Conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	if err := js.DeleteKeyValue(bus.SessionBucket); err != nil {
		t.Fatalf("delete the session bucket: %v", err)
	}
	recreated := testnats.RecreateKeyValue(t, js, &natsgo.KeyValueConfig{Bucket: bus.SessionBucket})
	putRaw(t, recreated, "ses_a", SessionEntry{Port: 1, MachineID: "m1", Title: "new", UpdatedAt: time.Now().UnixMilli()})
	if r.watcher.Epoch() != 0 {
		t.Fatalf("epoch %d, want the watcher still on the first bucket's epoch 0", r.watcher.Epoch())
	}

	entry, err := r.Refresh(context.Background(), "ses_a")
	if err != nil || entry.Title != "new" {
		t.Fatalf("Refresh(ses_a) below the fence = %+v, %v; want the live entry the bucket returned", entry, err)
	}
	clock.moveTo(recordCreated(t, recreated, "ses_a").Add(2 * time.Minute))
	if _, err := r.Refresh(context.Background(), "ses_a"); !errors.Is(err, natsgo.ErrKeyNotFound) {
		t.Fatalf("Refresh(ses_a) below the fence past its TTL = %v, want not found", err)
	}
}

// clock is the registry's injected clock: the real time moved on by what the test advanced it, so
// the creation times the server stamps on records and the registry's own time agree until the test
// moves past a session's TTL without waiting it out.
type clock struct {
	mu     sync.Mutex
	offset time.Duration
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().Add(c.offset)
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.offset += d
}

// A row's state is its own registration's age, while a read of the bucket is under way and after
// it: the copy cannot see the heartbeats that renewed a session while it reads, and a session
// that heartbeated nowhere since is unresponsive either way — left out of the default listing,
// listed with its state when the unresponsive rows are asked for, and absent to the one-session
// read that judges a row by its deadline. The sweep that completes the read keeps the row, since
// the bucket still holds its key, and leaves every one of those answers as it found them: only
// the row's own deadline, the bucket's backstop, takes the row away (expireDue).
//
// The bucket has no maximum age of its own, so the server keeps ses_old for the read however long
// the machine takes, and the registry reads a clock the test holds, which the test moves past the
// live window and the row's deadline once the read has delivered every record, to a moment
// computed from the record's own Created.
func TestARowPastItsLiveWindowReadsUnresponsiveThroughAReadAndItsSweep(t *testing.T) {
	client := setupNATS(t)
	clock := &heldClock{now: time.Now()}
	r, held := holdingRegistry(t, client, WithSessionTTL(time.Minute), WithSessionClock(clock.Now), WithSessionLiveWindow(time.Minute))
	if err := r.Put("ses_old", SessionEntry{Port: 1, MachineID: "m1"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	heartbeat, err := r.Get("ses_old")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	completion := held.holdCompletion(t, nil)
	if err := r.Rewatch(context.Background(), client.Conn); err != nil {
		t.Fatalf("Rewatch: %v", err)
	}
	waitClosed(t, completion.reached, "the read to deliver every record")
	// The expiry timer waits in real time for the wait it was armed with at the Put, one TTL, and
	// moving the clock below does not re-arm it: once it fired while the copy follows the bucket,
	// it would remove the row past its deadline, and Lookup would answer gone after a real minute
	// of a loaded machine rather than because of anything the row does. So the test stops it
	// here, under the lock its callback takes. Stop answers false only when the timer has fired
	// and its callback has not yet re-armed it; that callback runs once the lock is free, finds
	// nothing due on the clock the test has not moved yet, and re-arms, so the next Stop finds it
	// armed and stops it with no callback left to run. Nothing else re-arms it in this row: every
	// deadline the read schedules is the one already armed or a later one (armExpiryLocked).
	//
	// With the timer stopped, the row runs the timer's callback itself once the copy follows its
	// bucket again (expireDue): only then does the row leave, on its deadline, and its last
	// heartbeat is still answered (LastSeen). What the row no longer exercises is the timer firing
	// on its own and running that callback; TestGet_ExpiredEntryReadsAbsentAndLeavesAtItsDeadline
	// and expiry_test.go's rows show the timer doing that, on a registry with no bucket.
	waitFor(t, "the expiry timer to stop with no callback pending", func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.expiryTimer.Stop()
	})
	clock.moveTo(recordCreated(t, r.watcher.KV().Legacy(), "ses_old").Add(2 * time.Minute))
	unresponsive := func(when string) {
		t.Helper()
		if got := listed(t, r); len(got) != 0 {
			t.Fatalf("listed %s = %v, want no live session", when, got)
		}
		withUnresponsive, err := r.List(IncludeUnresponsive())
		if err != nil {
			t.Fatalf("List with the unresponsive rows %s: %v", when, err)
		}
		if len(withUnresponsive) != 1 || withUnresponsive[0].SessionID != "ses_old" || withUnresponsive[0].State != Unresponsive {
			t.Fatalf("listed with the unresponsive rows %s = %+v, want ses_old unresponsive", when, withUnresponsive)
		}
		if entry, state := r.Lookup("ses_old"); state != Unresponsive || entry.UpdatedAt != heartbeat.UpdatedAt {
			t.Fatalf("Lookup(ses_old) %s = %+v, %s; want its record and %s", when, entry, state, Unresponsive)
		}
		if _, err := r.Get("ses_old"); !errors.Is(err, natsgo.ErrKeyNotFound) {
			t.Fatalf("Get(ses_old) %s = %v, want not found", when, err)
		}
		if got := r.LastSeen("ses_old"); got != heartbeat.UpdatedAt {
			t.Fatalf("LastSeen(ses_old) %s = %d, want its last heartbeat %d", when, got, heartbeat.UpdatedAt)
		}
	}
	unresponsive("during the read")

	completion.open()
	waitClosed(t, completion.done, "the read's sweep")
	waitFollowing(t, r)
	unresponsive("after the read")

	r.expireDue()
	if size := r.CacheSize(); size != 0 {
		t.Fatalf("cache size after the expiry = %d, want ses_old gone at its deadline", size)
	}
	if _, state := r.Lookup("ses_old"); state != Gone {
		t.Fatalf("Lookup(ses_old) after the expiry = %s, want %s", state, Gone)
	}
	if got := r.LastSeen("ses_old"); got != heartbeat.UpdatedAt {
		t.Fatalf("LastSeen(ses_old) after the expiry = %d, want its last heartbeat %d", got, heartbeat.UpdatedAt)
	}
}

// A session bucket recreated with another TTL expires its rows by its own: the read of the new
// bucket adopts its MaxAge.
//
// The recreated bucket's MaxAge outlasts the run: what is left of the test binary's deadline, or a
// day when the binary has none (-timeout 0). So the server keeps ses_long for as long as the test
// can run and the read of the new bucket finds it however long the machine takes; the registry
// reads a clock the test holds, and the expiry is checked at moments computed from the record's
// own Created. What the row proves about MaxAge: the registry adopts the age the recreated bucket
// carries as its TTL, and measures the row's deadline as that age past the record's Created, so
// the row is live a millisecond before that deadline, far past the first bucket's minute, and gone
// a millisecond after it. That the server itself drops the record at that age is the server's own
// behaviour, which this row never measured.
func TestABucketRecreatedWithAnotherTTLMovesTheExpiry(t *testing.T) {
	const firstTTL = time.Minute
	maxAge := 24 * time.Hour
	if deadline, ok := t.Deadline(); ok {
		// Rounded up to whole seconds, so the age is no shorter than what is left.
		maxAge = time.Until(deadline).Truncate(time.Second) + time.Second
	}
	if maxAge <= firstTTL {
		t.Fatalf("the test binary's deadline leaves %s, no more than the first bucket's %s, so a maximum age taken from it cannot move the expiry past the first bucket's", maxAge, firstTTL)
	}
	client := setupNATS(t)
	clock := &heldClock{now: time.Now()}
	r, err := OpenSessionRegistry(t.Context(), client.Conn, WithSessionReplicas(1), WithSessionTTL(firstTTL), WithSessionClock(clock.Now))
	if err != nil {
		t.Fatalf("open session registry: %v", err)
	}
	t.Cleanup(r.StopWatch)
	waitFollowing(t, r)

	js, err := client.Conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	if err := js.DeleteKeyValue(bus.SessionBucket); err != nil {
		t.Fatalf("delete the session bucket: %v", err)
	}
	recreated := testnats.RecreateKeyValue(t, js, &natsgo.KeyValueConfig{Bucket: bus.SessionBucket, TTL: maxAge})
	putRaw(t, recreated, "ses_long", SessionEntry{Port: 1, MachineID: "m1", UpdatedAt: time.Now().UnixMilli()})
	if err := r.Rewatch(context.Background(), client.Conn); err != nil {
		t.Fatalf("Rewatch: %v", err)
	}
	waitFor(t, "the watcher to follow the recreated bucket", func() bool { return r.watcher.Epoch() == 1 && r.watcher.Following() })
	if got := r.TTL(); got != maxAge {
		t.Fatalf("TTL after the recreate = %s, want the new bucket's %s", got, maxAge)
	}

	expires := recordCreated(t, recreated, "ses_long").Add(maxAge)
	clock.moveTo(expires.Add(-time.Millisecond))
	if _, err := r.Get("ses_long"); err != nil {
		t.Fatalf("Get(ses_long) a millisecond before the new bucket's %s from its write = %v, want it live", maxAge, err)
	}
	clock.moveTo(expires.Add(time.Millisecond))
	if _, err := r.Get("ses_long"); !errors.Is(err, natsgo.ErrKeyNotFound) {
		t.Fatalf("Get(ses_long) a millisecond past the new bucket's %s from its write = %v, want it expired", maxAge, err)
	}
}

// The leader-answered refresh is Refresh with the leader of the bucket's stream answering the
// read, and answers what Refresh answers: the record for a session whose registration stands,
// not found for one the bucket does not hold, and not found for a record past its deadline,
// which the cache measures by the bucket's maximum age. The row runs the two reads side by side
// over the same three rows, since the one thing that may differ between them is who answers the
// read, and a single in-process server, which holds one copy of the bucket and cannot be made to
// lag, answers both the same way.
//
// The bucket has no maximum age of its own, so the server keeps ses_live for every read however
// long the machine takes, and its deadline is the registry's TTL past the record's Created,
// measured on a clock the test holds: the record is past it only once the test moves that clock.
func TestTheLeaderRefreshAnswersAsRefreshDoes(t *testing.T) {
	client := setupNATS(t)
	agelessSessionBucket(t, client)
	clock := &heldClock{now: time.Now()}
	r, err := OpenSessionRegistry(t.Context(), client.Conn, WithSessionReplicas(1), WithSessionTTL(time.Minute), WithSessionClock(clock.Now))
	if err != nil {
		t.Fatalf("open session registry: %v", err)
	}
	t.Cleanup(r.StopWatch)
	waitFollowing(t, r)
	if err := r.Put("ses_live", SessionEntry{Port: 1, MachineID: "m1", Title: "live"}); err != nil {
		t.Fatalf("register ses_live: %v", err)
	}
	ctx := t.Context()

	leader, leaderErr := r.RefreshFromLeader(ctx, "ses_live")
	direct, directErr := r.Refresh(ctx, "ses_live")
	if leaderErr != nil || leader.Title != "live" {
		t.Fatalf("RefreshFromLeader(ses_live) = %+v, %v; want the record the bucket holds", leader, leaderErr)
	}
	if directErr != nil || !reflect.DeepEqual(leader, direct) {
		t.Fatalf("RefreshFromLeader(ses_live) = %+v, Refresh(ses_live) = %+v, %v; want the same record", leader, direct, directErr)
	}

	if _, err := r.RefreshFromLeader(ctx, "ses_missing"); !errors.Is(err, natsgo.ErrKeyNotFound) {
		t.Errorf("RefreshFromLeader(ses_missing) = %v, want %v", err, natsgo.ErrKeyNotFound)
	}
	if _, err := r.Refresh(ctx, "ses_missing"); !errors.Is(err, natsgo.ErrKeyNotFound) {
		t.Errorf("Refresh(ses_missing) = %v, want %v: the two answer a session the bucket does not hold alike", err, natsgo.ErrKeyNotFound)
	}

	clock.moveTo(recordCreated(t, r.watcher.KV().Legacy(), "ses_live").Add(2 * time.Minute))
	if _, err := r.RefreshFromLeader(ctx, "ses_live"); !errors.Is(err, natsgo.ErrKeyNotFound) {
		t.Errorf("RefreshFromLeader(ses_live) past its deadline = %v, want %v", err, natsgo.ErrKeyNotFound)
	}
	if _, err := r.Refresh(ctx, "ses_live"); !errors.Is(err, natsgo.ErrKeyNotFound) {
		t.Errorf("Refresh(ses_live) past its deadline = %v, want %v: the two judge a record past its deadline alike", err, natsgo.ErrKeyNotFound)
	}
}

package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/clock"
	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/internal/testwait"
)

// newTestRegistry is a registry with the given TTL built from a struct literal, as the cache's unit
// tests build one: no NATS, no watcher, and no expiry heap or timer until its first write. With no
// watcher the copy counts as following its bucket (SessionRegistry.following), so rows expire by
// their deadlines alone. Its clock is a fake that stands where the wall clock is until the test
// moves it, so a deadline passes when the test says and the expiry timer fires then (newTestRegistryOn).
func newTestRegistry(ttl time.Duration) *SessionRegistry {
	return newTestRegistryOn(ttl, clocktest.New(time.Now()))
}

// newTestRegistryOn is newTestRegistry on c, for a test that moves the clock its rows expire on:
// an Advance past a row's deadline fires the registry's expiry timer and returns once the
// callback it runs has finished, so what that callback did is done when Advance returns.
func newTestRegistryOn(ttl time.Duration, c clock.Clock) *SessionRegistry {
	r := &SessionRegistry{
		clock:    c,
		ttl:      ttl,
		lastSeen: map[string]lastSeenSession{},
	}
	r.cache = newSessionCopy(r)
	return r
}

// applyWatched applies one record as the session bucket's watcher delivers it
// (sessionCache.Apply), which is how the cache's own tests write to the copy.
func (r *SessionRegistry) applyWatched(entry natsgo.KeyValueEntry) {
	sessionCache{r}.Apply(kvwatch.LegacyEntry(entry), kvwatch.Read{})
}

// sweep runs a read of the bucket that delivers nothing, so the copy removes every row and fence
// entry it found no evidence of (kvwatch.Copy.CompleteRead): the one place the merged design
// drops a fence, in place of the lastSeen deadline that dropped it before.
func (r *SessionRegistry) sweep() {
	sessionCache{r}.BeginRead(kvwatch.Read{})
	sessionCache{r}.CompleteRead(kvwatch.Read{})
}

// watchedEntry is a nats.KeyValueEntry the session bucket's watcher could have delivered.
type watchedEntry struct {
	key      string
	value    []byte
	revision uint64
	created  time.Time
	op       natsgo.KeyValueOp
}

func (e watchedEntry) Bucket() string               { return "envoy_sessions" }
func (e watchedEntry) Key() string                  { return e.key }
func (e watchedEntry) Value() []byte                { return e.value }
func (e watchedEntry) Revision() uint64             { return e.revision }
func (e watchedEntry) Created() time.Time           { return e.created }
func (e watchedEntry) Delta() uint64                { return 0 }
func (e watchedEntry) Operation() natsgo.KeyValueOp { return e.op }

// watchedPut is the watcher's delivery of a put of item under key at revision, written at created.
func watchedPut(t testing.TB, key string, revision uint64, created time.Time, item SessionEntry) watchedEntry {
	t.Helper()
	value, err := json.Marshal(item)
	if err != nil {
		t.Fatalf("marshal %s: %v", key, err)
	}
	return watchedEntry{key: key, value: value, revision: revision, created: created, op: natsgo.KeyValuePut}
}

// watchedDelete is the watcher's delivery of a delete marker for key at revision.
func watchedDelete(key string, revision uint64) watchedEntry {
	return watchedEntry{key: key, revision: revision, created: time.Now(), op: natsgo.KeyValueDelete}
}

// waitForCacheSize waits, reading only CacheSize, until the cache holds want entries.
func waitForCacheSize(t *testing.T, r *SessionRegistry, want int) {
	t.Helper()
	testwait.Eventually(t, "the registry to settle", func() bool { return r.CacheSize() == want })
}

// lastSeenHeld reports whether the registry still holds a lastSeen record for id, expired or not.
func lastSeenHeld(r *SessionRegistry, id string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.lastSeen[id]
	return ok
}

// fence is the revision the copy's fence holds for id, and whether it holds one. A registry built
// from a struct literal has no watcher, so its read epoch is the zero one (kvwatch.Copy.FenceFor).
func fence(r *SessionRegistry, id string) (uint64, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	revision := r.cache.FenceFor(0, id)
	return revision, revision != 0
}

// TestRegistryReadsRaceWatchedWritesAndExpiry runs Get, List, LastSeen and LiveCount from several
// goroutines while one applies watcher puts and deletes and moves the registry's clock past the
// TTL, which keeps the expiry callback removing entries beside them, for the race detector. Every
// read answers an entry that was written, and once the writes stop every entry, lastSeen record
// and fence leaves at its deadline. The writer counts the work the race runs for, so the test
// does the same amount of it on any machine.
func TestRegistryReadsRaceWatchedWritesAndExpiry(t *testing.T) {
	const (
		ttl    = 20 * time.Millisecond
		writes = 1000
	)
	now := clocktest.New(time.Now())
	r := newTestRegistryOn(ttl, now)
	ids := make([]string, 40)
	puts := make([]watchedEntry, len(ids))
	for i := range ids {
		ids[i] = fmt.Sprintf("ses_race_%02d", i)
		puts[i] = watchedPut(t, ids[i], 0, time.Time{}, SessionEntry{Port: i + 1, UpdatedAt: now.Now().UnixMilli()})
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		defer close(stop)
		revision := uint64(0)
		for i := range writes {
			revision++
			j := i % len(ids)
			if i%7 == 0 {
				r.applyWatched(watchedDelete(ids[j], revision))
				continue
			}
			put := puts[j]
			put.revision, put.created = revision, now.Now()
			r.applyWatched(put)
			if i%11 == 0 {
				// Past the deadlines of the rows written a TTL ago: the expiry callback runs
				// here, on the clock's own goroutine, while the readers below are reading.
				now.Advance(ttl)
			}
		}
	})
	for range 3 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				for i, id := range ids {
					if entry, err := r.Get(id); err == nil && entry.Port != i+1 {
						t.Errorf("Get(%s).Port = %d, want %d", id, entry.Port, i+1)
						return
					}
					_ = r.LastSeen(id)
				}
				entries, err := r.List()
				if err != nil || len(entries) > len(ids) {
					t.Errorf("List = %d entries, %v; want at most %d", len(entries), err, len(ids))
					return
				}
				if live := r.LiveCount(); live > len(ids) {
					t.Errorf("LiveCount = %d, want at most %d", live, len(ids))
					return
				}
			}
		})
	}
	wg.Wait()

	// A row leaves at its own deadline and leaves a last-seen record whose deadline is a
	// retention later, so each move of the clock can schedule the next deadline: the wait moves
	// it a TTL at a time until nothing is left, which is the registry's own progress and not the
	// machine's.
	testwait.Eventually(t, "every row and last-seen record to leave at its deadline", func() bool {
		now.Advance(ttl)
		r.mu.RLock()
		defer r.mu.RUnlock()
		return r.cache.Len() == 0 && len(r.lastSeen) == 0
	})
	// The fences outlive the rows: a completed read of the bucket is the one thing that drops
	// them (kvwatch.Copy.CompleteRead), and a read that delivers nothing drops them all.
	r.sweep()
	if revision, held := fence(r, ids[0]); held {
		t.Fatalf("a completed read left fence %d for %s", revision, ids[0])
	}
	if live := r.LiveCount(); live != 0 {
		t.Fatalf("LiveCount after every deadline = %d, want 0", live)
	}
}

// TestExpiryKeepsAnEntryRewrittenBetweenItsDeadlineAndItsCallback holds the registry's write lock
// while the entry's deadline passes, so the expiry callback waits on that lock, and writes the
// session's next heartbeat as the watcher would. When the callback runs it must find the newer
// entry and keep it.
func TestExpiryKeepsAnEntryRewrittenBetweenItsDeadlineAndItsCallback(t *testing.T) {
	const ttl = time.Minute
	clk := clocktest.New(time.Now())
	r := newTestRegistryOn(ttl, clk)
	const id = "ses_rewritten"
	created := clk.Now()
	r.applyWatched(watchedPut(t, id, 1, created, SessionEntry{Port: 1, UpdatedAt: created.UnixMilli()}))

	r.mu.Lock()
	// The deadline passes while this test holds the write lock. The fake runs the timer's
	// callback on a goroutine of its own and waits for it, so the Advance goes on a goroutine of
	// its own too: the callback is queued on the lock this one holds.
	passed := make(chan struct{})
	go func() {
		defer close(passed)
		clk.Advance(ttl + time.Second)
	}()
	// The heartbeat that lands between the old deadline and its callback: it was written past
	// that deadline, so its own is a TTL further on and the callback must keep it.
	rewritten := created.Add(ttl + time.Second)
	fresh := SessionEntry{Port: 2, UpdatedAt: rewritten.UnixMilli()}
	// What applyWatched does with the watcher's next put; it takes the lock this test holds.
	value, err := json.Marshal(fresh)
	if err != nil {
		t.Fatalf("marshal %s: %v", id, err)
	}
	if err := r.cache.Delivered(kvwatch.Entry{Key: id, Value: value, Revision: 2, Created: rewritten}); err != nil {
		t.Fatalf("deliver %s: %v", id, err)
	}
	r.mu.Unlock()
	// The callback runs once the lock is free, and the move ends when it returns.
	testwait.Receive(t, "the old deadline's expiry callback to run", passed)

	got, err := r.Get(id)
	if err != nil {
		t.Fatalf("Get(%s) after the old deadline's callback: %v", id, err)
	}
	if got.Port != 2 {
		t.Fatalf("Get(%s).Port = %d, want the rewritten entry's 2", id, got.Port)
	}
	if size := r.CacheSize(); size != 1 {
		t.Fatalf("cache size = %d, want 1", size)
	}
	if seen := r.LastSeen(id); seen != 0 || lastSeenHeld(r, id) {
		t.Fatalf("LastSeen(%s) = %d (record held %v), want no record for a live session", id, seen, lastSeenHeld(r, id))
	}
}

// TestRevisionFenceLeavesWithAReadAndAStaleReplayStaysAbsent removes a session, waits for its
// lastSeen record to leave one TTL later, and then replays the put the fence used to hold off, as
// a late watcher delivery would. The put is more than one TTL old, so the session must stay
// absent, with no LastSeen answer; the fence leaves at the next completed read of the bucket, not
// at the record's deadline, and a replay after that stays absent on its age alone (decodeLocked).
func TestRevisionFenceLeavesWithAReadAndAStaleReplayStaysAbsent(t *testing.T) {
	const ttl = 100 * time.Millisecond
	clk := clocktest.New(time.Now())
	r := newTestRegistryOn(ttl, clk)
	// The live window is this registry's TTL, as production's is the bucket's maximum age today,
	// so a record past its deadline is past its live window too and reads live nowhere.
	r.liveWindow = ttl
	const id = "ses_fenced"
	created := clk.Now()
	stale := watchedPut(t, id, 5, created, SessionEntry{Port: 1, UpdatedAt: created.UnixMilli()})
	r.applyWatched(stale)
	r.applyWatched(watchedDelete(id, 6))
	if revision, ok := fence(r, id); !ok || revision != 6 {
		t.Fatalf("fence after the delete = %d (held %v), want 6", revision, ok)
	}
	r.applyWatched(stale)
	if _, err := r.Get(id); !errors.Is(err, natsgo.ErrKeyNotFound) {
		t.Fatalf("Get(%s) after a replay the fence holds off = %v, want ErrKeyNotFound", id, err)
	}

	// One retention after the delete took the row, the record it left leaves with it.
	clk.Advance(ttl + time.Millisecond)
	testwait.Eventually(t, "the registry to settle", func() bool { return !lastSeenHeld(r, id) })
	if r.LastSeen(id) != 0 {
		t.Fatalf("LastSeen %d after the record left; want 0", r.LastSeen(id))
	}
	// The fence leaves only with a completed read that found the bucket holds no such key.
	if revision, held := fence(r, id); !held || revision != 6 {
		t.Fatalf("fence after the record left = %d (held %v), want the delete's 6", revision, held)
	}
	r.sweep()

	// With the fence swept, the replay is just a record of the bucket again: it is past its
	// deadline, so it never reads as live, the expiry removes it at once, and the record its
	// removal writes leaves at its own deadline, a retention past the row's.
	r.applyWatched(stale)
	if _, err := r.Get(id); !errors.Is(err, natsgo.ErrKeyNotFound) {
		t.Fatalf("Get(%s) after a stale replay = %v, want ErrKeyNotFound", id, err)
	}
	if live := r.LiveCount(); live != 0 {
		t.Fatalf("LiveCount %d after a stale replay; want 0", live)
	}
	waitForCacheSize(t, r, 0)
	clk.Advance(ttl + time.Millisecond)
	testwait.Eventually(t, "the registry to settle", func() bool { return r.LastSeen(id) == 0 })
}

// TestRevisionFencesStayBoundedAsSessionsComeAndGo registers waves of sessions, deletes half of
// each wave and lets the other half expire, and then waits: one TTL after the last of them left,
// the registry holds no entry, lastSeen record, fence or deadline for any of them (R3).
func TestRevisionFencesStayBoundedAsSessionsComeAndGo(t *testing.T) {
	const ttl = 50 * time.Millisecond
	clk := clocktest.New(time.Now())
	r := newTestRegistryOn(ttl, clk)
	revision := uint64(0)
	for wave := range 3 {
		for i := range 100 {
			id := fmt.Sprintf("ses_wave%d_%03d", wave, i)
			revision++
			now := clk.Now()
			r.applyWatched(watchedPut(t, id, revision, now, SessionEntry{Port: i + 1, UpdatedAt: now.UnixMilli()}))
			if i%2 == 0 {
				revision++
				r.applyWatched(watchedDelete(id, revision))
			}
		}
	}
	// Past every row's deadline, and then past the retention of the record each row left as it
	// went: one TTL, since this registry sets no retention of its own.
	clk.Advance(ttl + time.Millisecond)
	clk.Advance(ttl + time.Millisecond)
	testwait.Eventually(t, "the registry to settle", func() bool {
		r.mu.RLock()
		defer r.mu.RUnlock()
		return r.cache.Len() == 0 && len(r.lastSeen) == 0 &&
			len(r.expiries) == 0 && len(r.expiryIndex) == 0
	})

	// Each wave's fences leave with the next completed read of the bucket, so nothing it holds
	// outlives the bucket's own keys (kvwatch.Copy.CompleteRead).
	r.sweep()
	r.mu.RLock()
	defer r.mu.RUnlock()
	if got := r.cache.FenceFor(0, "ses_wave0_000"); got != 0 {
		t.Fatalf("a completed read left fence %d", got)
	}
}

// TestNonPositiveTTLNeverRemovesOrSchedules writes, ages and deletes sessions in registries whose
// TTL is zero or negative: nothing expires, nothing is scheduled, and a delete keeps its fence.
func TestNonPositiveTTLNeverRemovesOrSchedules(t *testing.T) {
	for _, ttl := range []time.Duration{0, -time.Second} {
		t.Run(ttl.String(), func(t *testing.T) {
			clk := clocktest.New(time.Now())
			r := newTestRegistryOn(ttl, clk)
			old := clk.Now().Add(-time.Hour)
			r.applyWatched(watchedPut(t, "ses_kept", 1, old, SessionEntry{Port: 1, UpdatedAt: old.UnixMilli()}))
			r.applyWatched(watchedPut(t, "ses_deleted", 2, old, SessionEntry{Port: 2, UpdatedAt: old.UnixMilli()}))
			r.applyWatched(watchedDelete("ses_deleted", 3))
			// A day on the registry's clock: with no TTL there is no deadline for it to pass, and
			// no timer of the registry's it could fire.
			clk.Advance(24 * time.Hour)

			if _, err := r.Get("ses_kept"); err != nil {
				t.Fatalf("Get(ses_kept): %v", err)
			}
			// The row has no deadline to pass, so the cache keeps it; its registration is an hour
			// old, so nothing counts it live. A state is the record's age, never its membership of
			// the cache.
			if live, size := r.LiveCount(), r.CacheSize(); live != 0 || size != 1 {
				t.Fatalf("LiveCount %d, CacheSize %d; want 0 and 1", live, size)
			}
			if revision, ok := fence(r, "ses_deleted"); !ok || revision != 3 {
				t.Fatalf("fence for ses_deleted = %d (held %v), want 3", revision, ok)
			}
			if seen := r.LastSeen("ses_deleted"); seen != 0 {
				t.Fatalf("LastSeen(ses_deleted) = %d, want 0 with no TTL", seen)
			}
			r.mu.RLock()
			scheduled, timer := len(r.expiries), r.expiryTimer
			r.mu.RUnlock()
			if scheduled != 0 || timer != nil {
				t.Fatalf("scheduled %d deadlines (timer %v), want none", scheduled, timer)
			}
			if armed := clk.Armed(); len(armed) != 0 {
				t.Fatalf("the registry armed %v on its clock, want nothing with no TTL", armed)
			}
		})
	}
}

// TestLastSeenRetentionKeepsTheRecordAndFenceThatLong lets a session expire and removes another in
// a registry whose retention is longer than its TTL: LastSeen answers each one's final heartbeat,
// and the fence holds off a stale replay, until the retention has passed since the session left
// the cache, well past one TTL, and both leave together after it.
func TestLastSeenRetentionKeepsTheRecordAndFenceThatLong(t *testing.T) {
	const (
		ttl       = 50 * time.Millisecond
		retention = time.Second
	)
	clk := clocktest.New(time.Now())
	r := newTestRegistryOn(ttl, clk)
	r.lastSeenRetention = retention
	created := clk.Now()
	heartbeat := created.UnixMilli()
	stale := watchedPut(t, "ses_deleted", 1, created, SessionEntry{Port: 1, UpdatedAt: heartbeat})
	r.applyWatched(watchedPut(t, "ses_expired", 2, created, SessionEntry{Port: 2, UpdatedAt: heartbeat}))
	r.applyWatched(stale)
	r.applyWatched(watchedDelete("ses_deleted", 3))
	// Past the TTL, which takes the row the delete left and the one that expires.
	clk.Advance(ttl + time.Millisecond)
	waitForCacheSize(t, r, 0)
	left := clk.Now()

	// Several TTLs after both left, well inside the retention.
	clk.Advance(4 * ttl)
	for _, id := range []string{"ses_expired", "ses_deleted"} {
		if got := r.LastSeen(id); got != heartbeat {
			t.Fatalf("LastSeen(%s) %s after it left = %d, want its heartbeat %d", id, clk.Now().Sub(created), got, heartbeat)
		}
	}
	if revision, ok := fence(r, "ses_deleted"); !ok || revision != 3 {
		t.Fatalf("fence for ses_deleted = %d (held %v), want 3 within the retention", revision, ok)
	}
	r.applyWatched(stale)
	if _, err := r.Get("ses_deleted"); !errors.Is(err, natsgo.ErrKeyNotFound) {
		t.Fatalf("Get(ses_deleted) after a replay the fence holds off = %v, want ErrKeyNotFound", err)
	}
	if got := r.LastSeen("ses_deleted"); got != heartbeat {
		t.Fatalf("LastSeen(ses_deleted) after a replay the fence holds off = %d, want %d", got, heartbeat)
	}

	// Each record is kept the whole retention from the moment its row left: the delete's row left
	// at once and the expired one at its deadline, so a tick short of the retention after the
	// delete both are still held, and a tick past the retention after the later of the two
	// departures both are gone.
	clk.Set(created.Add(retention - time.Millisecond))
	for _, id := range []string{"ses_expired", "ses_deleted"} {
		if got := r.LastSeen(id); got != heartbeat {
			t.Fatalf("LastSeen(%s) a tick short of the retention = %d, want its heartbeat %d", id, got, heartbeat)
		}
	}
	clk.Set(left.Add(retention + time.Millisecond))
	testwait.Eventually(t, "every last-seen record to leave at its retention", func() bool {
		r.mu.RLock()
		defer r.mu.RUnlock()
		return len(r.lastSeen) == 0 && len(r.expiries) == 0
	})

	for _, id := range []string{"ses_expired", "ses_deleted"} {
		if got := r.LastSeen(id); got != 0 {
			t.Fatalf("LastSeen(%s) after the retention = %d, want 0", id, got)
		}
	}
}

// TestLastSeenRetentionShorterThanTheTTLKeepsOneTTL asks for a retention shorter than the TTL: the
// record is still kept one TTL, and the fence stays until a completed read of the bucket drops it.
func TestLastSeenRetentionShorterThanTheTTLKeepsOneTTL(t *testing.T) {
	const ttl = 500 * time.Millisecond
	clk := clocktest.New(time.Now())
	r := newTestRegistryOn(ttl, clk)
	r.lastSeenRetention = time.Millisecond
	now := clk.Now()
	r.applyWatched(watchedPut(t, "ses_deleted", 1, now, SessionEntry{Port: 1, UpdatedAt: now.UnixMilli()}))
	r.applyWatched(watchedDelete("ses_deleted", 2))

	// Many retentions later, well inside one TTL.
	clk.Advance(50 * time.Millisecond)
	if got := r.LastSeen("ses_deleted"); got != now.UnixMilli() {
		t.Fatalf("LastSeen(ses_deleted) %s after it left = %d, want its heartbeat %d", clk.Now().Sub(now), got, now.UnixMilli())
	}
	if revision, ok := fence(r, "ses_deleted"); !ok || revision != 2 {
		t.Fatalf("fence for ses_deleted = %d (held %v), want 2 within one TTL", revision, ok)
	}
	// A tick short of one TTL after the delete the record is still held, and a tick past it the
	// record leaves: the retention it was given is shorter than the TTL, and the TTL wins.
	clk.Set(now.Add(ttl - time.Millisecond))
	if !lastSeenHeld(r, "ses_deleted") {
		t.Fatalf("the record left a tick short of one TTL (%s) after the delete", ttl)
	}
	clk.Advance(2 * time.Millisecond)
	testwait.Eventually(t, "the registry to settle", func() bool { return !lastSeenHeld(r, "ses_deleted") })
	r.sweep()
	if revision, ok := fence(r, "ses_deleted"); ok {
		t.Fatalf("a completed read left fence %d", revision)
	}
}

// rowDeadline is the deadline the cache holds for id, and whether it holds a row for it at all.
func rowDeadline(r *SessionRegistry, id string) (time.Time, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	cs, ok := r.cache.Get(id)
	if !ok {
		return time.Time{}, false
	}
	return cs.expiresAt, true
}

// A pass of the listener's reaper that reads another maximum age from the bucket moves every
// cached row's deadline with it, so the cache expires rows by the age the bucket itself keeps
// them for: a raise pushes each deadline out by the difference, leaving it one new TTL from the
// moment the record was written, and a cut that puts a row's deadline in the past takes the row
// at once through the expiry timer, which keeps its last heartbeat as any other expiry does.
func TestSetTTLMovesEveryCachedRowsDeadline(t *testing.T) {
	const id = "ses_maxage"
	clk := clocktest.New(time.Now())
	r := newTestRegistryOn(time.Hour, clk)
	r.lastSeenRetention = time.Minute
	created := clk.Now()
	r.applyWatched(watchedPut(t, id, 1, created, SessionEntry{Port: 1, UpdatedAt: created.UnixMilli()}))
	if got, ok := rowDeadline(r, id); !ok || !got.Equal(created.Add(time.Hour)) {
		t.Fatalf("the row's deadline = %v (held %v), want one TTL past its record %v", got, ok, created.Add(time.Hour))
	}

	r.SetTTL(2 * time.Hour)
	if got := r.TTL(); got != 2*time.Hour {
		t.Fatalf("TTL after the raise = %s, want 2h", got)
	}
	if got, ok := rowDeadline(r, id); !ok || !got.Equal(created.Add(2*time.Hour)) {
		t.Fatalf("the row's deadline after the raise = %v (held %v), want %v", got, ok, created.Add(2*time.Hour))
	}

	// A maximum age shorter than the row's age: a second on the clock, well inside the minute
	// this registry keeps a last-seen record for, leaves the row a second old, so the cut puts
	// its new deadline in the past and the timer takes it at once.
	clk.Advance(time.Second)
	r.SetTTL(time.Millisecond)
	waitForCacheSize(t, r, 0)
	if got := r.LastSeen(id); got != created.UnixMilli() {
		t.Fatalf("LastSeen after the cut took the row = %d, want its last heartbeat %d", got, created.UnixMilli())
	}
}

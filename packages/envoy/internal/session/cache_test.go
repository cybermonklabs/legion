package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/internal/testwait"
)

// --- Unit tests (no NATS — operate on a pre-populated cache) ---

func TestExpiryFor_PrefersCreated(t *testing.T) {
	r := &SessionRegistry{ttl: 5 * time.Minute}
	created := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	got := r.expiryFor(created, time.Now().UnixMilli())
	want := created.Add(5 * time.Minute)
	if !got.Equal(want) {
		t.Fatalf("expected expiry from Created() %v, got %v", want, got)
	}
}

func TestExpiryFor_FallsBackToUpdatedAt(t *testing.T) {
	r := &SessionRegistry{ttl: 5 * time.Minute}
	updated := time.Date(2021, 6, 1, 12, 0, 0, 0, time.UTC)
	got := r.expiryFor(time.Time{}, updated.UnixMilli())
	want := updated.Add(5 * time.Minute)
	if !got.Equal(want) {
		t.Fatalf("expected fallback expiry from UpdatedAt %v, got %v", want, got)
	}
}

func TestExpiryFor_ZeroTTLNeverExpires(t *testing.T) {
	r := &SessionRegistry{ttl: 0}
	got := r.expiryFor(time.Now(), time.Now().UnixMilli())
	if !got.IsZero() {
		t.Fatalf("expected zero (never-expires) for ttl<=0, got %v", got)
	}
}

// followingRegistry opens a registry on the test's own session bucket and waits for its first read
// to complete, so the copy follows the bucket and its rows expire at their deadlines (expireDue).
// The bucket is empty, so a test puts rows in the cache itself. options are added to the
// registry's own (a test's clock, WithSessionClock).
func followingRegistry(t *testing.T, options ...SessionRegistryOption) *SessionRegistry {
	t.Helper()
	client := setupNATS(t)
	r, err := OpenSessionRegistry(t.Context(), client.Conn, append([]SessionRegistryOption{WithSessionReplicas(1), WithSessionTTL(5 * time.Minute)}, options...)...)
	if err != nil {
		t.Fatalf("open session registry: %v", err)
	}
	t.Cleanup(r.StopWatch)
	waitFollowing(t, r)
	return r
}

// waitFollowing waits until r's copy follows its bucket (kvwatch.Watcher.Following), a read of it
// having completed. Its one bound is the test binary's deadline (testwait.Until): a loaded machine
// that is slow to finish the read is not a failure.
func waitFollowing(t *testing.T, r *SessionRegistry) {
	t.Helper()
	testwait.Until(t, "the session cache's read to complete", func() (bool, bool) {
		following := r.watcher.Following()
		return following, following
	})
}

// cacheReady waits for r's first read to end (WaitForCacheReady) and fails the test when that wait
// returns an error. Its one bound is the test binary's deadline (testwait.Receive).
func cacheReady(t *testing.T, r *SessionRegistry) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan error, 1)
	go func() { ready <- r.WaitForCacheReady(ctx) }()
	if err := testwait.Receive(t, "the session cache's first read to end", ready); err != nil {
		t.Fatalf("WaitForCacheReady: %v", err)
	}
}

// hold puts rows in r's cache as a read delivers them (sessionCache.Apply), each at revision 1 and
// created one ttl before its expiresAt, so the cache computes that expiry for it (expiryFor).
func hold(t *testing.T, r *SessionRegistry, rows map[string]cachedSession) {
	t.Helper()
	r.mu.RLock()
	ttl := r.ttl
	r.mu.RUnlock()
	for sessionID, row := range rows {
		value, err := json.Marshal(row.entry)
		if err != nil {
			t.Fatalf("encode %s: %v", sessionID, err)
		}
		sessionCache{r}.Apply(kvwatch.Entry{Key: sessionID, Value: value, Revision: 1, Created: row.expiresAt.Add(-ttl)}, kvwatch.Read{})
	}
}

func TestList_ExpiredEntryReadsAbsentAndLeavesAtItsDeadline(t *testing.T) {
	now := time.Now()
	updated := now.Add(-6 * time.Minute).UnixMilli()
	r := newTestRegistry(5 * time.Minute)
	hold(t, r, map[string]cachedSession{
		"ses_live":    {entry: SessionEntry{Port: 1, UpdatedAt: now.UnixMilli()}, expiresAt: now.Add(time.Minute)},
		"ses_expired": {entry: SessionEntry{Port: 2, UpdatedAt: updated}, expiresAt: now.Add(-time.Minute)},
	})
	entries, err := r.List()
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(entries) != 1 || entries[0].SessionID != "ses_live" {
		t.Fatalf("expected only ses_live, got %v", entries)
	}
	before := r.LastSeen(context.Background(), "ses_expired")
	if before != updated {
		t.Fatalf("LastSeen before removal = %d, want the expired entry's UpdatedAt %d", before, updated)
	}

	// The copy schedules each row's deadline as it stores it (storedLocked), so the expiry timer
	// removes the expired one with no read of any kind.
	waitForCacheSize(t, r, 1)
	if after := r.LastSeen(context.Background(), "ses_expired"); after != before {
		t.Fatalf("LastSeen after removal = %d, want %d as before it", after, before)
	}
	if _, err := r.Get("ses_live"); err != nil {
		t.Fatalf("Get(ses_live) after the expiry: %v", err)
	}
}

// The registry reads a clock the test holds (heldClock), so the row it writes stays before its
// deadline for as long as the test holds the clock, however long the machine takes between two
// lines. The expiry timer measures its wait on that clock too (armExpiryLocked), so the row leaves
// once the test moves the clock past its deadline.
func TestGet_ExpiredEntryReadsAbsentAndLeavesAtItsDeadline(t *testing.T) {
	const ttl = 300 * time.Millisecond
	clock := &heldClock{now: time.Now()}
	r := newTestRegistry(ttl)
	r.now = clock.Now
	hold(t, r, map[string]cachedSession{
		"ses_expired": {entry: SessionEntry{Port: 2}, expiresAt: clock.Now().Add(-time.Second)},
	})
	if _, err := r.Get("ses_expired"); !errors.Is(err, natsgo.ErrKeyNotFound) {
		t.Fatalf("expected ErrKeyNotFound for expired entry, got %v", err)
	}
	// The copy schedules each row's deadline as it stores it (storedLocked), so the expired one
	// leaves with no read of any kind.
	waitForCacheSize(t, r, 0)

	// An entry the watcher delivers leaves at its deadline too, one TTL after it was written.
	created := clock.Now()
	r.applyWatched(watchedPut(t, "ses_short", 1, created, SessionEntry{Port: 3, UpdatedAt: created.UnixMilli()}))
	if _, err := r.Get("ses_short"); err != nil {
		t.Fatalf("Get(ses_short) before its deadline: %v", err)
	}
	// Past the row's deadline, and short of its last-seen record's, one TTL after that.
	clock.advance(ttl + ttl/2)
	waitForCacheSize(t, r, 0)
	if _, err := r.Get("ses_short"); !errors.Is(err, natsgo.ErrKeyNotFound) {
		t.Fatalf("Get(ses_short) after its deadline = %v, want ErrKeyNotFound", err)
	}
	if got := r.LastSeen(context.Background(), "ses_short"); got != created.UnixMilli() {
		t.Fatalf("LastSeen(ses_short) after removal = %d, want %d", got, created.UnixMilli())
	}
}

// The registry reads a clock the test holds (heldClock), so the row the test puts in its cache is
// before its deadline however long the machine takes to open the registry and complete its first
// read. On the machine's clock a row due a minute after the test began was past due by the Get
// when those took longer than a minute: "Get failed: nats: key not found" after 65.48 s, in a
// -race run at a load of 1,100 to 2,100.
func TestGet_ValidReturnsEntry(t *testing.T) {
	clock := &heldClock{now: time.Now()}
	r := followingRegistry(t, WithSessionClock(clock.Now))
	hold(t, r, map[string]cachedSession{
		"ses_ok": {entry: SessionEntry{Port: 99, MachineID: "m1"}, expiresAt: clock.Now().Add(time.Minute)},
	})
	got, err := r.Get("ses_ok")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if got.Port != 99 || got.MachineID != "m1" {
		t.Fatalf("unexpected entry: %+v", got)
	}
}

// The registry reads a clock the test holds (heldClock), so the record a session leaves is within
// its retention until the test moves the clock past it, however long the machine takes between
// two lines.
func TestExpiredLastSeenReadsZeroAndLeavesAtItsDeadline(t *testing.T) {
	const ttl = 200 * time.Millisecond
	clock := &heldClock{now: time.Now()}
	r := newTestRegistry(ttl)
	r.now = clock.Now
	r.seenLocal["ses_lapsed"] = clock.Now().Add(-2 * time.Minute).UnixMilli()
	if got := r.LastSeen(context.Background(), "ses_lapsed"); got != 0 {
		t.Fatalf("LastSeen(ses_lapsed) = %d, want 0 for an expired record", got)
	}
	if !lastSeenHeld(r, "ses_lapsed") {
		t.Fatal("LastSeen removed a record: a read must change nothing")
	}

	// A record the registry keeps when a session leaves leaves seenLocal at its own deadline, one
	// retention after the registration's expiry, with no read and no Ping.
	created := clock.Now()
	r.applyWatched(watchedPut(t, "ses_gone", 1, created, SessionEntry{Port: 1, UpdatedAt: created.UnixMilli()}))
	r.applyWatched(watchedDelete("ses_gone", 2))
	if got := r.LastSeen(context.Background(), "ses_gone"); got != created.UnixMilli() {
		t.Fatalf("LastSeen(ses_gone) = %d, want %d", got, created.UnixMilli())
	}
	clock.advance(2 * ttl)
	waitUntil(t, func() bool { return !lastSeenHeld(r, "ses_gone") })
	if got := r.LastSeen(context.Background(), "ses_gone"); got != 0 {
		t.Fatalf("LastSeen(ses_gone) after its deadline = %d, want 0", got)
	}
}

// Without the listener state bucket a lapsed session's last-seen time is the registry's own
// (seenLocal), reported for one retention after the expiry of the registration it came from and
// then dropped at that deadline.
func TestPerProcessLastSeenEndsOneTTLAfterTheRegistrationsExpiry(t *testing.T) {
	now := time.Now()
	r := newTestRegistry(time.Minute)
	r.seenLocal["ses_recent"] = now.Add(-90 * time.Second).UnixMilli()
	r.seenLocal["ses_old"] = now.Add(-2*time.Minute - time.Second).UnixMilli()
	if got, want := r.LastSeen(context.Background(), "ses_recent"), now.Add(-90*time.Second).UnixMilli(); got != want {
		t.Fatalf("last seen of a session that lapsed 30 s ago = %d, want %d", got, want)
	}
	if got := r.LastSeen(context.Background(), "ses_old"); got != 0 {
		t.Fatalf("last seen of a session whose registration expired over a TTL ago = %d, want none", got)
	}
	// The records went in directly, so nothing scheduled their deadlines; schedule them as a write
	// does. The expiry timer then drops the one past its retention and keeps the other.
	r.mu.Lock()
	for id, at := range r.seenLocal {
		r.scheduleLocked(id, expireLastSeen, seenUntil(at, r.ttl, r.retention()))
	}
	r.mu.Unlock()
	waitUntil(t, func() bool { return !lastSeenHeld(r, "ses_old") })
	if !lastSeenHeld(r, "ses_recent") {
		t.Fatal("the registry dropped a last-seen time within its retention")
	}
}

// A lapsed session's per-process last-seen time does not make it live, and once the session
// registers again its last-seen time is the live registration's.
func TestLastSeenRetentionDoesNotRestoreLiveSession(t *testing.T) {
	now := time.Now()
	r := followingRegistry(t)
	r.mu.Lock()
	r.seenLocal["ses_lapsed"] = now.Add(-6 * time.Minute).UnixMilli()
	r.mu.Unlock()

	if _, err := r.Get("ses_lapsed"); !errors.Is(err, natsgo.ErrKeyNotFound) {
		t.Fatalf("Get(lapsed) error = %v, want key not found", err)
	}
	entries, err := r.List()
	if err != nil {
		t.Fatalf("List sessions: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("live sessions = %+v, want none", entries)
	}
	if got := r.LastSeen(context.Background(), "ses_lapsed"); got == 0 {
		t.Fatal("last seen timestamp was lost before its retention TTL")
	}

	hold(t, r, map[string]cachedSession{
		"ses_lapsed": {entry: SessionEntry{Port: 1, UpdatedAt: now.UnixMilli()}, expiresAt: now.Add(time.Minute)},
	})
	if _, err := r.Get("ses_lapsed"); err != nil {
		t.Fatalf("Get(reregistered): %v", err)
	}
	if got := r.LastSeen(context.Background(), "ses_lapsed"); got != now.UnixMilli() {
		t.Fatalf("last seen = %d after re-registration, want the live registration's %d", got, now.UnixMilli())
	}
}

// --- Integration tests (a real NATS server, shared via setupNATS) ---

// pollSessionList asks List until it answers want entries, and returns that answer. Its one bound
// is the test binary's deadline (testwait.Until), and its failure prints the last answer.
func pollSessionList(t *testing.T, r *SessionRegistry, want int) []ListEntry {
	t.Helper()
	return testwait.Until(t, fmt.Sprintf("List to answer %d entries", want), func() ([]ListEntry, bool) {
		got, err := r.List()
		if err != nil {
			t.Fatalf("List failed: %v", err)
		}
		return got, len(got) == want
	})
}

func TestSessionWatch_BootstrapPopulatesExistingKeys(t *testing.T) {
	client := setupNATS(t)
	// Pre-populate via a first registry, then open a second to simulate a
	// listener restart against existing KV state.
	reg1, err := OpenSessionRegistry(t.Context(), client.Conn, WithSessionReplicas(1), WithSessionTTL(time.Minute))
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	if err := reg1.Put("ses_boot", SessionEntry{Port: 7, MachineID: "m1"}); err != nil {
		t.Fatalf("put: %v", err)
	}

	reg2, err := OpenSessionRegistry(t.Context(), client.Conn, WithSessionReplicas(1), WithSessionTTL(time.Minute))
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	cacheReady(t, reg2)
	// Immediately after readiness, the pre-populated entry must be visible.
	entries, err := reg2.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 || entries[0].SessionID != "ses_boot" {
		t.Fatalf("expected ses_boot after bootstrap, got %v", entries)
	}
}

func TestSessionWatch_PropagatesExternalPut(t *testing.T) {
	client := setupNATS(t)
	reg, err := OpenSessionRegistry(t.Context(), client.Conn, WithSessionReplicas(1), WithSessionTTL(time.Minute))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// Write directly via KV (not reg.Put), so only the watcher can surface it.
	buf, err := json.Marshal(SessionEntry{Port: 5, MachineID: "m1", UpdatedAt: time.Now().UnixMilli()})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := reg.watcher.KV().Put("ses_ext", buf); err != nil {
		t.Fatalf("kv put: %v", err)
	}
	entries := pollSessionList(t, reg, 1)
	if entries[0].SessionID != "ses_ext" || entries[0].Port != 5 {
		t.Fatalf("unexpected entry: %+v", entries[0])
	}
}

func TestSessionWatch_PropagatesDelete(t *testing.T) {
	client := setupNATS(t)
	reg, err := OpenSessionRegistry(t.Context(), client.Conn, WithSessionReplicas(1), WithSessionTTL(time.Minute))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := reg.Put("ses_d", SessionEntry{Port: 1, MachineID: "m1"}); err != nil {
		t.Fatalf("put: %v", err)
	}
	pollSessionList(t, reg, 1)

	// Delete directly via KV (bypassing write-through) so we exercise the
	// watcher's delete handling specifically.
	if err := reg.watcher.KV().Delete("ses_d"); err != nil {
		t.Fatalf("kv delete: %v", err)
	}
	pollSessionList(t, reg, 0)
}

func TestSessionWatch_PropagatesPurge(t *testing.T) {
	client := setupNATS(t)
	reg, err := OpenSessionRegistry(t.Context(), client.Conn, WithSessionReplicas(1), WithSessionTTL(time.Minute))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := reg.Put("ses_p", SessionEntry{Port: 1, MachineID: "m1"}); err != nil {
		t.Fatalf("put: %v", err)
	}
	pollSessionList(t, reg, 1)

	if err := reg.watcher.KV().Purge("ses_p"); err != nil {
		t.Fatalf("kv purge: %v", err)
	}
	pollSessionList(t, reg, 0)
}

func TestSessionPut_WriteThroughVisibleImmediately(t *testing.T) {
	client := setupNATS(t)
	reg, err := OpenSessionRegistry(t.Context(), client.Conn, WithSessionReplicas(1), WithSessionTTL(time.Minute))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := reg.Put("ses_wt", SessionEntry{Port: 8, MachineID: "m1"}); err != nil {
		t.Fatalf("put: %v", err)
	}
	// No sleep, no poll — write-through must make it visible synchronously,
	// even before the watcher delivers the event.
	got, err := reg.Get("ses_wt")
	if err != nil {
		t.Fatalf("Get immediately after Put: %v", err)
	}
	if got.Port != 8 {
		t.Fatalf("expected port 8, got %d", got.Port)
	}
	entries, err := reg.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry from write-through, got %d", len(entries))
	}
}

func TestSessionDelete_WriteThroughRemovesImmediately(t *testing.T) {
	client := setupNATS(t)
	reg, err := OpenSessionRegistry(t.Context(), client.Conn, WithSessionReplicas(1), WithSessionTTL(time.Minute))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := reg.Put("ses_wd", SessionEntry{Port: 8, MachineID: "m1"}); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := reg.Delete(context.Background(), "ses_wd"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	// Write-through delete must remove from cache synchronously.
	if _, err := reg.Get("ses_wd"); !errors.Is(err, natsgo.ErrKeyNotFound) {
		t.Fatalf("expected ErrKeyNotFound after delete, got %v", err)
	}
}

func TestSessionList_CacheOnlyAfterNATSShutdown(t *testing.T) {
	// After WaitForCacheReady, List must serve from cache even when NATS is gone
	// — no per-key Get() round-trips that would hang on a dead/slow link. This
	// is the core regression: the old List() did Keys()+per-key Get() and timed
	// out on a remote KV leader. The session the Put wrote through is in the cache
	// whether or not the watcher has delivered its record by then: the first read
	// keeps it through its sweep (kvwatch.Copy.BeginRead), so the connection may
	// close before that record arrives.
	client := setupNATS(t)
	reg, err := OpenSessionRegistry(t.Context(), client.Conn, WithSessionReplicas(1), WithSessionTTL(time.Minute))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := reg.Put("ses_cache", SessionEntry{Port: 3, MachineID: "m1"}); err != nil {
		t.Fatalf("put: %v", err)
	}
	cacheReady(t, reg)

	client.Close()

	// A List that hung would fail the row by name at the test binary's deadline.
	done := make(chan struct{})
	go func() {
		entries, err := reg.List()
		if err != nil {
			t.Errorf("List after shutdown: %v", err)
		}
		if len(entries) != 1 || entries[0].SessionID != "ses_cache" {
			t.Errorf("expected ses_cache from cache after shutdown, got %v", entries)
		}
		close(done)
	}()
	testwait.Receive(t, "List to answer from its cache after NATS shutdown", done)
}

func TestOpenSessionRegistry_PrunesUsingBucketTTL(t *testing.T) {
	client := setupNATS(t)
	// Create the bucket with a 2-minute TTL — deliberately different from the
	// BucketMaxAge default in OpenSessionRegistry.
	if _, err := OpenSessionRegistry(t.Context(), client.Conn, WithSessionReplicas(1), WithSessionTTL(2*time.Minute)); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	// Open a second registry WITHOUT a TTL option, so opts.ttl is the BucketMaxAge
	// default. It must adopt the bucket's real 2-minute TTL via Status(), not
	// the default — otherwise pruning is computed against the wrong window.
	reg, err := OpenSessionRegistry(t.Context(), client.Conn, WithSessionReplicas(1))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if ttl := reg.TTL(); ttl != 2*time.Minute {
		t.Fatalf("expected registry to adopt bucket TTL 2m, got %v", ttl)
	}
	// Rows are pruned for their age only once the copy follows the bucket (pruneLocked).
	waitFollowing(t, reg)
	// An entry created 3 minutes ago is past the real 2-minute TTL but still
	// within the BucketMaxAge default. Pruning against the bucket TTL drops it;
	// pruning against the default would wrongly keep it live.
	created := time.Now().Add(-3 * time.Minute)
	reg.mu.RLock()
	expiresAt := reg.expiryFor(created, 0)
	reg.mu.RUnlock()
	hold(t, reg, map[string]cachedSession{
		"ses_old": {entry: SessionEntry{Port: 1}, expiresAt: expiresAt},
	})
	entries, err := reg.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected entry older than real TTL pruned, got %v", entries)
	}
}

func TestSessionWatch_DeadWatcherFailsPing(t *testing.T) {
	client := setupNATS(t)
	reg, err := OpenSessionRegistry(t.Context(), client.Conn, WithSessionReplicas(1), WithSessionTTL(time.Minute))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	cacheReady(t, reg)
	// Healthy watcher: no error yet.
	if err := reg.WatchErr(); err != nil {
		t.Fatalf("expected no watch error while healthy, got %v", err)
	}

	// Closing the connection closes the KV watcher's Updates() channel. Because
	// this happens AFTER the initial scan, the watcher must record a watch error so
	// the frozen cache is no longer reported as healthy.
	client.Close()

	testwait.Until(t, "the watcher to record its end after Updates() closed post-scan", func() (error, bool) {
		err := reg.WatchErr()
		return err, err != nil
	})
	if err := reg.Ping(); err == nil {
		t.Fatal("expected Ping to fail once the watcher is dead")
	}
}

package dedupe

import (
	"sync"
	"testing"
	"time"
)

// entryCount is how many pairs c holds, read under its lock, as its writes and its sweep change
// them.
func entryCount(c *Cache) int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.entries)
}

// mustBeStored fails t unless c answers the pair as recorded with the copy at sequence.
func mustBeStored(t *testing.T, c *Cache, dedupeKey, sessionID string, sequence uint64) {
	t.Helper()
	if got, ok := c.Stored(dedupeKey, sessionID); !ok || got != sequence {
		t.Fatalf("Stored(%q, %q) = %d, %t; want %d, true", dedupeKey, sessionID, got, ok, sequence)
	}
}

// A pair nothing recorded is not seen.
func TestCache_FirstDeliveryNotSeen(t *testing.T) {
	clock := newTestClock()
	c := newCache(10*time.Minute, clock.Now)
	defer c.Stop()

	if c.Seen("key-1", "ses-A") {
		t.Fatal("first check should return false (not yet delivered)")
	}
}

// A recorded pair is seen, and Stored answers it with the sequence it was recorded with.
func TestCache_AfterRecordIsSeen(t *testing.T) {
	clock := newTestClock()
	c := newCache(10*time.Minute, clock.Now)
	defer c.Stop()

	c.RecordStored("key-1", "ses-A", 5)
	if !c.Seen("key-1", "ses-A") {
		t.Fatal("should be seen after RecordStored")
	}
	mustBeStored(t, c, "key-1", "ses-A", 5)
}

// Clear drops a recorded pair, which is then not seen.
func TestCache_ClearRemovesRecordedDelivery(t *testing.T) {
	clock := newTestClock()
	c := newCache(10*time.Minute, clock.Now)
	defer c.Stop()

	c.RecordStored("key-1", "ses-A", 5)
	mustBeStored(t, c, "key-1", "ses-A", 5)
	c.Clear("key-1", "ses-A")

	if c.Seen("key-1", "ses-A") {
		t.Fatal("cleared delivery should not be seen")
	}
}

// A pair recorded for one session is not seen for another session of the same key.
func TestCache_DifferentSessionsIndependent(t *testing.T) {
	clock := newTestClock()
	c := newCache(10*time.Minute, clock.Now)
	defer c.Stop()

	c.RecordStored("key-1", "ses-A", 5)
	if c.Seen("key-1", "ses-B") {
		t.Fatal("different session should not be seen")
	}
	mustBeStored(t, c, "key-1", "ses-A", 5)
}

// A pair recorded for one dedupe key is not seen for another key to the same session.
func TestCache_DifferentKeysIndependent(t *testing.T) {
	clock := newTestClock()
	c := newCache(10*time.Minute, clock.Now)
	defer c.Stop()

	c.RecordStored("key-1", "ses-A", 5)
	if c.Seen("key-2", "ses-A") {
		t.Fatal("different dedupe key should not be seen")
	}
	mustBeStored(t, c, "key-1", "ses-A", 5)
}

// A recorded pair is not seen once the TTL has passed.
func TestCache_ExpiryAfterTTL(t *testing.T) {
	clock := newTestClock()
	c := newCache(10*time.Minute, clock.Now)
	defer c.Stop()

	c.RecordStored("key-1", "ses-A", 5)
	mustBeStored(t, c, "key-1", "ses-A", 5)
	clock.Advance(11 * time.Minute) // past 10min TTL

	if c.Seen("key-1", "ses-A") {
		t.Fatal("entry should have expired after TTL")
	}
}

// Forward names the newest stored copy recorded for a request, whichever session it went to: not a
// copy recorded for another request, not a delivery recorded with RecordStored alone, and not
// after the TTL.
func TestCache_ForwardNamesTheNewestStoredCopyOfARequest(t *testing.T) {
	clock := newTestClock()
	c := newCache(10*time.Minute, clock.Now)
	defer c.Stop()

	c.RecordStored("key-1", "ses-A", 5)
	if session, _, ok := c.Forward("key-1"); ok {
		t.Fatalf("a delivery recorded by RecordStored alone is a request's forward to %q", session)
	}
	c.RecordForward("alice role-a key-1", "ses-B", 7)
	c.RecordForward("alice role-b key-1", "ses-C", 8)
	clock.Advance(time.Minute)
	if session, seq, ok := c.Forward("alice role-a key-1"); !ok || session != "ses-B" || seq != 7 {
		t.Fatalf("Forward(role a) = %q, %d, %t; want ses-B at 7, not role b's forward", session, seq, ok)
	}
	c.RecordForward("alice role-a key-1", "ses-D", 9)
	if session, seq, ok := c.Forward("alice role-a key-1"); !ok || session != "ses-D" || seq != 9 {
		t.Fatalf("Forward(role a) after a newer forward = %q, %d, %t; want ses-D at 9", session, seq, ok)
	}
	clock.Advance(11 * time.Minute)
	if session, _, ok := c.Forward("alice role-b key-1"); ok {
		t.Fatalf("Forward names %q past the TTL", session)
	}
}

// The TTL bound is inclusive: a pair exactly one TTL old is still seen, and one a nanosecond older
// is not.
func TestCache_NotSeenAtExactTTLBoundary(t *testing.T) {
	clock := newTestClock()
	ttl := 10 * time.Minute
	c := newCache(ttl, clock.Now)
	defer c.Stop()

	c.RecordStored("key-1", "ses-A", 5)

	// At exactly TTL: Sub(t) == ttl, which satisfies <= ttl
	clock.Advance(ttl)
	if !c.Seen("key-1", "ses-A") {
		t.Fatal("at exact TTL boundary, entry should still be seen (<=)")
	}
	mustBeStored(t, c, "key-1", "ses-A", 5)

	// One nanosecond past: now expired
	clock.Advance(1)
	if c.Seen("key-1", "ses-A") {
		t.Fatal("past TTL boundary, entry should not be seen")
	}
}

// A read records nothing: only RecordStored writes a pair, once the delivery is confirmed.
func TestCache_RecordOnlyAfterSuccess(t *testing.T) {
	clock := newTestClock()
	c := newCache(10*time.Minute, clock.Now)
	defer c.Stop()

	// Multiple reads without RecordStored
	c.Seen("key-1", "ses-A")
	c.Seen("key-1", "ses-A")
	c.Stored("key-1", "ses-A")

	if n := entryCount(c); n != 0 {
		t.Fatalf("Seen and Stored should not record entries, got %d", n)
	}
}

// Writes and reads of one pair from many goroutines at once race on nothing (the race detector
// reads it).
func TestCache_ConcurrentAccess(t *testing.T) {
	c := New(10 * time.Minute)
	defer c.Stop()

	var wg sync.WaitGroup
	for i := range 100 {
		wg.Add(3)
		key := "key"
		session := "ses"
		go func() {
			defer wg.Done()
			c.RecordStored(key, session, uint64(i))
		}()
		go func() {
			defer wg.Done()
			c.Seen(key, session)
		}()
		go func() {
			defer wg.Done()
			entryCount(c)
		}()
	}
	wg.Wait()
}

// The background sweep drops a pair past its TTL.
func TestCache_BackgroundCleanup(t *testing.T) {
	clock := newTestClock()
	// Short TTL so the real ticker fires quickly.
	c := newCache(50*time.Millisecond, clock.Now)
	defer c.Stop()

	c.RecordStored("key-1", "ses-A", 5)
	mustBeStored(t, c, "key-1", "ses-A", 5)
	if n := entryCount(c); n != 1 {
		t.Fatalf("expected 1 entry, got %d", n)
	}

	// Advance fake clock past TTL
	clock.Advance(100 * time.Millisecond)

	// The cleanup goroutine runs on a real ticker, and a loaded machine may not schedule it within
	// any fixed number of periods: wait for the pass that removes the expired entry. The deadline
	// bounds a failure; it is not how long cleanup takes.
	deadline := time.Now().Add(10 * time.Second)
	for entryCount(c) != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("expected 0 entries after background cleanup, got %d", entryCount(c))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Stop ends the sweep and nothing else: a pair recorded after it is still seen and stored. The
// clock is the test's, so the 10ms TTL does not lapse between the write and the reads.
func TestCache_StopHaltsCleanup(t *testing.T) {
	c := newCache(10*time.Millisecond, newTestClock().Now)
	c.Stop()

	// Should not panic on stop — goroutine exits cleanly.
	// RecordStored after stop should still work (no cleanup, but functionally correct).
	c.RecordStored("key-1", "ses-A", 5)
	if !c.Seen("key-1", "ses-A") {
		t.Fatal("RecordStored/Seen should still work after Stop")
	}
	mustBeStored(t, c, "key-1", "ses-A", 5)
}

package docs

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"

	"github.com/sjawhar/envoy/internal/testwait"
)

// Review 10's finding 4. A socket's admission takes one whole slot of the pool before it reads the
// stored document of a room no server holds here (ServeHTTP's readSlot), and the read it makes
// inside that grant (admitDocument, loadTree, heldTree) met a failed room's recovery
// (awaitRoomRecovery). The recovery is the failed room's eviction, whose closing compaction waits
// for its own bytes of the pool (servicePersistenceAdapter.Compact, compactRoom, foldRead). With
// the pool at one slot - the calibration's - the admission held every byte the compaction waited
// for and waited for the compaction, until the browser gave up after 8 s.
//
// A read whose grant holds bytes waits for no recovery: the admission is answered 503 at once and
// gives its slot back, the compaction takes its grant, the room recovers long before the browser
// would give up, and the editor's retry is admitted.
func TestRV10PoolAnAdmissionHoldingAWholeSlotWaitsOnAnEvictionThatWaitsForThatSlot(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	foldPoint := writeRoomRows(t, service, artifactID)
	ageUpdatesThrough(t, service.store, artifactID, foldPoint, time.Hour)
	if err := loadRoomOf(service, artifactID); err != nil {
		t.Fatalf("load the room: %v", err)
	}
	if service.srv.GetDoc(artifactID) == nil {
		t.Fatal("the room is not resident before the test")
	}
	// A pool of one slot, watched from before any taker can reach it: full closes once a holder
	// holds the whole pool, which is the admission's slot, and compactionTook records a take made
	// once the closing compaction has gone on past its hook, which is the compaction's own.
	pool := newLoadSlots(1)
	var waited, compactionGoes, compactionTook atomic.Bool
	full := make(chan struct{})
	var fullOnce sync.Once
	pool.waiting = func(int64) { waited.Store(true) }
	pool.taken = func(held int64) {
		if compactionGoes.Load() {
			compactionTook.Store(true)
		}
		if held == pool.poolBytes() {
			fullOnce.Do(func() { close(full) })
		}
	}
	useLoadSlots(t, pool)

	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(httpServer.Close)

	// The room fails after the admission's own wait for recovery (ServeHTTP's first
	// awaitRoomRecovery) and before it looks the room up; the hook returns once ygo has taken the
	// failed room out of its map, so the admission finds no room and takes its whole slot.
	gone := make(chan struct{})
	var recovered <-chan struct{}
	service.afterSocketIssueRead = onFirst(artifactID, func() {
		service.failRoom(artifactID, errors.New("rv10: a store failure fails the room"))
		state := service.lockExistingState(artifactID)
		recovered = state.failedDone
		service.unlockState(artifactID, state)
		testwait.Holds(10*time.Second, 5*time.Millisecond, func() bool { return service.srv.GetDoc(artifactID) == nil })
		close(gone)
	})
	// The eviction's closing compaction goes on only once the admission holds its slot, so it
	// asks the pool for its grant while the admission's read runs under that slot.
	compacting := make(chan struct{})
	service.beforeClosingCompaction = onFirst(artifactID, func() {
		close(compacting)
		select {
		case <-full:
		case <-time.After(10 * time.Second):
		}
		compactionGoes.Store(true)
	})

	const browserPatience = 8 * time.Second
	url := socketURL(httpServer.URL, artifactID)
	type dialed struct {
		err     error
		status  int
		elapsed time.Duration
	}
	dial := func(patience time.Duration) dialed {
		ctx, cancel := context.WithTimeout(context.Background(), patience)
		defer cancel()
		began := time.Now()
		connection, response, err := gws.DefaultDialer.DialContext(ctx, url, pageHeader("alice"))
		got := dialed{err: err, elapsed: time.Since(began)}
		if response != nil {
			got.status = response.StatusCode
		}
		if connection != nil {
			_ = connection.Close()
		}
		return got
	}
	result := make(chan dialed, 1)
	started := time.Now()
	go func() { result <- dial(browserPatience) }()

	select {
	case <-gone:
	case <-time.After(15 * time.Second):
		t.Fatal("the admission never reached the hook that fails the room")
	}
	select {
	case <-compacting:
	case <-time.After(15 * time.Second):
		t.Fatal("the failed room's eviction never began its closing compaction")
	}

	got := <-result
	t.Logf("the socket's dial ended after %s: status %d, %v", got.elapsed, got.status, got.err)
	if got.status != http.StatusServiceUnavailable || got.elapsed >= browserPatience/2 {
		t.Errorf("the socket's admission ended after %s with status %d (%v); want 503 at once, which the editor's backoff retries, not a wait until the browser gives up after %s",
			got.elapsed, got.status, got.err, browserPatience)
	}
	select {
	case <-recovered:
	case <-time.After(20 * time.Second):
		t.Fatal("the failed room never recovered")
	}
	took := time.Since(started)
	t.Logf("the failed room recovered %s after the dial began; the compaction waited for the pool: %t, took its grant: %t",
		took, waited.Load(), compactionTook.Load())
	if took >= browserPatience {
		t.Errorf("the failed room recovered %s after the dial began, only once the browser's %s patience was over: the admission held the pool through the recovery",
			took, browserPatience)
	}
	if !compactionTook.Load() {
		t.Error("the recovery's closing compaction took no grant of the pool")
	}

	// The editor's retry meets the recovered room and is admitted.
	retry := dial(15 * time.Second)
	if retry.err != nil {
		t.Fatalf("the editor's retry after the recovery was refused after %s: status %d, %v", retry.elapsed, retry.status, retry.err)
	}
	t.Logf("the editor's retry was admitted after %s", retry.elapsed)
}

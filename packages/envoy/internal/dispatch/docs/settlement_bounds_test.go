package docs

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/texttrees"
)

// A settlement holding one of the settlementWorkers slots takes its transaction's connection from
// the settlements pool, which has one connection for each slot, so no settlement holding a slot waits
// for a connection behind another settlement.
func TestSettlementSlotsMatchTheSettlementsPool(t *testing.T) {
	database := storetest.Open(t)
	settlements, err := database.Pool.Settlements()
	if err != nil {
		t.Fatalf("open the settlements pool: %v", err)
	}
	if size := settlements.Stat().MaxConns(); int(size) != settlementWorkers {
		t.Fatalf("the settlements pool holds %d connections, want one per settlement slot (%d)", size, settlementWorkers)
	}
}

// Twelve documents owing a settlement, all armed at once by the resumption, never run more than
// settlementWorkers settlements at a time; the rest wait for a slot, counted as waiting, and every
// one of them settles.
func TestOwedDocumentsNeverRunMoreSettlementsAtOnceThanThereAreSlots(t *testing.T) {
	service, first := newTestService(t)
	service.settle = time.Hour
	const documents = 12
	rooms := []string{first}
	for number := 2; len(rooms) < documents; number++ {
		rooms = append(rooms, createIssueDocument(t, service.store, number, "# Document"))
	}
	for index, room := range rooms {
		seedServiceText(t, service, room, fmt.Sprintf("Owed %d.\n", index))
	}
	if _, err := service.store.Pool.Exec(context.Background(), `
		insert into doc_settlements_pending (artifact_id) select unnest($1::uuid[]) on conflict (artifact_id) do nothing
	`, rooms); err != nil {
		t.Fatalf("mark every document owed: %v", err)
	}

	var running, highest, waited, entered atomic.Int64
	service.afterSettleWarm = func(string) {
		now := running.Add(1)
		for seen := highest.Load(); now > seen && !highest.CompareAndSwap(seen, now); seen = highest.Load() {
		}
		if load := service.SettlementLoad(); load.Waiting > waited.Load() {
			waited.Store(load.Waiting)
		}
		entered.Add(1)
		// Long enough that every slot is taken while the first settlements hold theirs.
		time.Sleep(200 * time.Millisecond)
		running.Add(-1)
	}
	service.settle = 10 * time.Millisecond
	if err := service.resumeOwedSettlements(context.Background(), 0); err != nil {
		t.Fatalf("resume the owed settlements: %v", err)
	}
	waitFor(t, 60*time.Second, "every owed document to settle", func() bool {
		return pendingSettlements(t, service) == 0
	})
	waitFor(t, 10*time.Second, "every settlement to give its slot back", func() bool {
		return service.SettlementLoad() == SettlementLoad{}
	})
	service.afterSettleWarm = nil

	if got := highest.Load(); got != settlementWorkers {
		t.Fatalf("at most %d settlements ran at once, want exactly %d: never more than the slots, and every slot used", got, settlementWorkers)
	}
	if entered.Load() < documents {
		t.Fatalf("%d settlements took a slot, want one for each of the %d documents", entered.Load(), documents)
	}
	if waited.Load() == 0 {
		t.Fatal("no settlement was counted waiting for a slot while every slot was held")
	}
}

// A settlement that cannot finish by its deadline - here another transaction holds the document's
// owner row past it - rolls back: the document stays owed, its room stays resident, and the
// deadline counts as no settlement failure however often it recurs, so overload fails no room. The
// resumption then settles the document.
func TestASettlementPastItsDeadlineRollsBackStaysOwedAndCountsNoFailure(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "before")
	settleCurrentGeneration(t, service, artifactID)
	editLiveTree(t, service, artifactID, replaceRun("before", "after"))
	versions := latestVersionNumber(t, service, artifactID)

	ctx := context.Background()
	hold, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the owner row's holder: %v", err)
	}
	defer hold.Rollback(ctx)
	if _, err := hold.Exec(ctx, `select key from issues where key = 'DOC-1' for no key update`); err != nil {
		t.Fatalf("hold the document's owner row: %v", err)
	}
	const deadline = 300 * time.Millisecond
	service.settleTimeout = deadline
	for attempt := range maxSettleFailures + 1 {
		started := time.Now()
		settleCurrentGeneration(t, service, artifactID)
		if took := time.Since(started); took > deadline+5*time.Second {
			t.Fatalf("attempt %d: the settlement returned after %v, want soon after its %v deadline", attempt, took, deadline)
		}
		owes, err := settlementPending(ctx, service.store.Pool, artifactID)
		if err != nil {
			t.Fatalf("read the document's pending settlement: %v", err)
		}
		if !owes {
			t.Fatalf("attempt %d: the document's pending-settlement row is gone, want it kept for the resumption", attempt)
		}
		if service.srv.GetDoc(artifactID) == nil {
			t.Fatalf("attempt %d: the document's room is gone, want it resident", attempt)
		}
		state := service.room(artifactID)
		state.mu.Lock()
		failures, failed := state.settleFailures, state.failed
		state.mu.Unlock()
		if failures != 0 || failed != nil {
			t.Fatalf("attempt %d: settlement failures = %d, room failure = %v; want none for a deadline", attempt, failures, failed)
		}
	}
	if latest := latestVersionNumber(t, service, artifactID); latest != versions {
		t.Fatalf("latest version = %d, want %d: no settlement past its deadline commits", latest, versions)
	}

	if err := hold.Rollback(ctx); err != nil {
		t.Fatalf("release the owner row: %v", err)
	}
	service.settleTimeout = settlementTimeout
	service.settle = 10 * time.Millisecond
	if err := service.resumeOwedSettlements(ctx, 0); err != nil {
		t.Fatalf("resume the owed settlement: %v", err)
	}
	waitForDocumentVersion(t, service.store, artifactID, versions+1)
	requireLatestVersionMarkdown(t, service, artifactID, "after\n")
}

// The settlement a room's last browser leaving runs in its timer's place (settleLastPeer) is held
// to the deadline a timer's is (settleRoomWithin, settleTimeout), besides carrying the mark a room
// its warm-up loads is known by (withSettlementLoad). Here another transaction holds the document's
// owner row past that deadline: the settlement rolls back at it, leaving the document owed and
// counting no failure, and the departing browser's socket, which ends only once that settlement
// has returned, ends then rather than once the owner row is let go.
func TestTheSettlementALastBrowserLeavingRunsIsHeldToItsDeadline(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "before")
	settleCurrentGeneration(t, service, artifactID)
	// The deadline the browser's leaving's settlement runs to, set before the server's goroutines
	// start: the settlement reads it on the socket's goroutine. No settlement runs before the
	// browser leaves: the edit's own is armed an hour away.
	const deadline = 300 * time.Millisecond
	service.settleTimeout = deadline
	ended := make(chan struct{}, 1)
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		service.ServeHTTP(w, r)
		if socketRoom(r) == artifactID {
			ended <- struct{}{}
		}
	}))
	t.Cleanup(httpServer.Close)
	browser := connectPeer(t, httpServer.URL, artifactID, "alice")
	if err := browser.WaitForText(peerMarkdown, "before\n", roomWait(t)); err != nil {
		t.Fatal(err)
	}
	// The edit, once stored, arms the room's settlement, which the settle delay of an hour keeps
	// armed until the browser's leaving takes it (takeSettleTimer).
	editLiveTree(t, service, artifactID, replaceRun("before", "after"))
	awaitRoomAppends(t, service, artifactID, "become durable", service.waitForDurableAppends)
	waitFor(t, roomWait(t), "the stored edit to arm the room's settlement", func() bool {
		return settlementUnderway(service, artifactID)
	})
	versions := latestVersionNumber(t, service, artifactID)

	ctx := context.Background()
	hold, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the owner row's holder: %v", err)
	}
	defer hold.Rollback(ctx)
	if _, err := hold.Exec(ctx, `select key from issues where key = 'DOC-1' for no key update`); err != nil {
		t.Fatalf("hold the document's owner row: %v", err)
	}
	closed := time.Now()
	browser.Close()
	// The socket ends within the bound a timer's settlement past its deadline returns within
	// (TestASettlementPastItsDeadlineRollsBackStaysOwedAndCountsNoFailure), far inside the idle
	// bound that ends the holder's transaction (store.RequestIdleInTransactionTimeout), after which
	// a settlement held to no deadline takes the owner row and commits.
	select {
	case <-ended:
	case <-time.After(deadline + 5*time.Second):
		t.Fatalf("the departing browser's socket had not ended %v after it left, with the document's owner row held: the settlement its leaving ran is held to no deadline", time.Since(closed).Round(time.Millisecond))
	}
	if took := time.Since(closed); took < deadline {
		t.Fatalf("the departing browser's socket ended %v after it left, before the settlement's %v deadline: that settlement never waited on the held owner row, so nothing here is tested", took, deadline)
	}
	if settlementUnderway(service, artifactID) {
		t.Fatal("a settlement of the document is armed or running once the departing browser's socket has ended, want none: the browser's leaving took the armed one and ran it to its deadline")
	}
	owes, err := settlementPending(ctx, service.store.Pool, artifactID)
	if err != nil {
		t.Fatalf("read the document's pending settlement: %v", err)
	}
	if !owes {
		t.Fatal("the document's pending-settlement row is gone, want it kept for the resumption")
	}
	state := service.room(artifactID)
	state.mu.Lock()
	failures, failed := state.settleFailures, state.failed
	state.mu.Unlock()
	if failures != 0 || failed != nil {
		t.Fatalf("settlement failures = %d, room failure = %v; want none for a deadline", failures, failed)
	}
	if latest := latestVersionNumber(t, service, artifactID); latest != versions {
		t.Fatalf("latest version = %d, want %d: no settlement past its deadline commits", latest, versions)
	}
}

// A document whose settlement is still owed can lose its room to ygo's idle sweep, which reads
// nothing of the service's settlements: it evicts a room with no peer, no caller inside it and the
// idle stamp it found (sjawhar/ygo v1.55.1-sami.2, provider/websocket/idle_sweep.go:159-197), and an
// armed settlement is a timer, which holds no Apply in the room. The settlement then loads the room
// again itself and settles it. Its warm-up in settleRoomWithin is a Server.Apply made under the
// deadline every settlement runs to (settleRoomWithin, settleTimeout), which the room's load is
// handed; the load reads the store on the pool that owns loads (store.Pool.Rooms: onLoadDocument,
// and servicePersistenceAdapter.LoadDoc, whose read names no pool of its caller's), and the
// settlement's own passes then read and write on the settlements pool; neither takes a request-pool
// connection. Here the room idles out a second after the edit while the settle delay is an hour, so
// the sweep comes first, and nothing else closes a room (the default budget holds thousands of
// these); the settlement is then run as its timer runs it (SettleNow).
func TestAnOwedSettlementLoadsTheRoomTheIdleSweepUnloadedAndSettles(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "# First")
	deps := testServiceDeps(database)
	deps.Settle = time.Hour
	// ygo sweeps every second at this timeout; no room is loaded yet.
	deps.RoomIdleTimeout = time.Second
	service := New(deps)
	t.Cleanup(func() {
		if err := service.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})
	settlements, err := service.store.Pool.Settlements()
	if err != nil {
		t.Fatalf("open the settlements pool: %v", err)
	}
	unloads := countUnloads(service)

	// What the owed settlement does, read on its own goroutine once measuring is set: the loads of
	// the room, the deadline the first ran under, and the connections each pool handed out from the
	// first settlement's start to the end of its warm-up. A retry after it, were the sweep to take
	// the room again mid-settlement, changes none of the first's figures.
	// The connection counts are the pools' own, process-wide, so the test reads nothing of the
	// request pool while they run: it waits for the warm-up (warmed) before it polls the version.
	var measuring atomic.Bool
	warmed := make(chan struct{})
	var seen struct {
		sync.Mutex
		loads                      int
		deadline, started          time.Time
		bounded, warmed, resident  bool
		roomsAt, requestsAt        uint64
		roomsTook, requestsTook    uint64
		settlementsAt, settlements int64
	}
	load := service.srv.OnLoadDocument
	service.srv.OnLoadDocument = func(ctx context.Context, room string, doc *crdt.Doc) error {
		if measuring.Load() && room == artifactID {
			seen.Lock()
			if seen.loads++; seen.loads == 1 {
				seen.deadline, seen.bounded = ctx.Deadline()
			}
			seen.Unlock()
		}
		return load(ctx, room, doc)
	}
	service.beforeSettleStart = func(room string) {
		if measuring.Load() && room == artifactID {
			seen.Lock()
			if seen.started.IsZero() {
				seen.started = time.Now()
				seen.roomsAt = poolAcquires(service, store.RoomsPool)
				seen.requestsAt = poolAcquires(service, store.RequestsPool)
			}
			seen.Unlock()
		}
	}
	service.afterSettleWarm = func(room string) {
		if measuring.Load() && room == artifactID {
			seen.Lock()
			if !seen.warmed {
				seen.warmed = true
				seen.resident = service.srv.GetDoc(room) != nil
				seen.roomsTook = poolAcquires(service, store.RoomsPool) - seen.roomsAt
				seen.requestsTook = poolAcquires(service, store.RequestsPool) - seen.requestsAt
				seen.settlementsAt = settlements.Stat().AcquireCount()
				close(warmed)
			}
			seen.Unlock()
		}
	}

	seedServiceText(t, service, artifactID, "before")
	settleCurrentGeneration(t, service, artifactID)
	// The edit, once stored, arms the document's settlement, which the settle delay of an hour
	// keeps armed while the room idles out.
	editLiveTree(t, service, artifactID, replaceRun("before", "after"))
	awaitRoomAppends(t, service, artifactID, "become durable", service.waitForDurableAppends)
	waitFor(t, roomWait(t), "the stored edit to arm the document's settlement", func() bool {
		return settlementUnderway(service, artifactID)
	})
	versions := latestVersionNumber(t, service, artifactID)
	before := unloads(artifactID)
	waitFor(t, roomWait(t), "ygo's idle sweep to unload the room of the document whose settlement is owed", func() bool {
		return unloads(artifactID) > before && !slices.Contains(service.srv.Rooms(), artifactID)
	})
	if !settlementUnderway(service, artifactID) {
		t.Fatal("the document's settlement is no longer armed once ygo's idle sweep unloaded its room, want it still owed: nothing has run it")
	}
	owes, err := settlementPending(context.Background(), service.store.Pool, artifactID)
	if err != nil {
		t.Fatalf("read the document's pending settlement: %v", err)
	}
	if !owes {
		t.Fatal("the document's pending-settlement row is gone once its room was unloaded, want it kept: the settlement is owed")
	}

	measuring.Store(true)
	service.SettleNow(artifactID)
	select {
	case <-warmed:
	case <-time.After(roomWait(t)):
		t.Fatalf("the owed settlement did not warm the room within %v", roomWait(t))
	}
	waitForDocumentVersion(t, service.store, artifactID, versions+1)
	requireLatestVersionMarkdown(t, service, artifactID, "after\n")

	seen.Lock()
	defer seen.Unlock()
	seen.settlements = settlements.Stat().AcquireCount() - seen.settlementsAt
	t.Logf("the settlement's start to its warm-up took %d rooms-pool and %d request-pool connections; its passes took %d settlements-pool connections", seen.roomsTook, seen.requestsTook, seen.settlements)
	if !seen.warmed || !seen.resident {
		t.Fatalf("the settlement warmed the room = %v, with the room resident after its warm-up = %v; want the settlement to have loaded the room itself", seen.warmed, seen.resident)
	}
	if seen.loads == 0 {
		t.Fatal("the room was not loaded for the settlement, want its warm-up to have loaded it")
	}
	if seen.roomsTook == 0 {
		t.Error("the settlement's warm-up took no rooms-pool connection, want the room's load to read the store on the pool that owns loads")
	}
	if seen.requestsTook != 0 {
		t.Errorf("the settlement's start and warm-up took %d request-pool connection(s), want none", seen.requestsTook)
	}
	if seen.settlements == 0 {
		t.Error("the settlement took no settlements-pool connection once it had warmed the room, want its passes to read and write on that pool")
	}
	// The settlement's deadline is set as settleRoomWithin starts, just before its beforeSettleStart
	// hook read the clock.
	if latest := seen.started.Add(service.settleTimeout); !seen.bounded || seen.deadline.After(latest) || seen.deadline.Before(latest.Add(-time.Second)) {
		t.Errorf("the room's load for the settlement was handed deadline %v (bounded %v), want the settlement's own, within a second before %v (its start plus settleTimeout, %v)", seen.deadline, seen.bounded, latest, service.settleTimeout)
	}
}

// An owed settlement whose room is not resident warms it first (settleRoomWithin), and the load
// that warm-up runs reads the rooms pool under its reads' own bound, a cold read's
// (coldReadTimeout, onLoadDocument), not under the settlement's deadline. A load that runs that
// bound out before the database answers (errLoadReadsBound) has met a bound of this server's own,
// which says nothing of the document, so the settlement stops short as one the database did not
// serve does (settleOverloaded): it wrote nothing into the room, starts no retry and counts no
// failure, and the document stays owed, unsettled, for the resumption, which settles it once the
// rooms pool answers again. Counted as a failure, three such stops in a row would fail the room
// (maxSettleFailures). Here the room is closed with the settlement owed and every rooms-pool
// connection is taken as the warm-up's load decodes the history, with the pool's acquire bound a
// minute, so the reads' bound, shortened to a second, is what ends the load's read
// (TestASocketWhoseRoomLoadRunsPastItsReadsBoundIsRefused503). The settlement runs as its timer
// runs it (SettleNow), in place of the edit's own arming an hour away, and the resumption runs as
// the server runs it (RunSettlementResumption), every second here: a pass arms a document's
// settlement once its pending-settlement row is a minute old (settlementResumeAge), which this
// row's marked_at is moved back to rather than waited out.
func TestAnOwedSettlementWhoseWarmUpLoadRunsOutItsReadsBoundStaysOwedForTheResumption(t *testing.T) {
	logs := captureWarnings(t)
	database := storetest.Open(t)
	database.Pool.SetAcquireBoundForTest(store.RoomsPool, time.Minute)
	previous := coldReadTimeout
	coldReadTimeout = time.Second
	t.Cleanup(func() { coldReadTimeout = previous })
	artifactID := createDocument(t, database, "# First")
	deps := testServiceDeps(database)
	deps.Settle = time.Hour
	deps.SettlementResume = time.Second
	service := New(deps)
	t.Cleanup(func() {
		if err := service.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})
	leases := excerptLeases(t, database)
	resumption, stop := context.WithCancel(context.Background())
	resumed := make(chan struct{})
	go func() {
		defer close(resumed)
		service.RunSettlementResumption(resumption, leases)
	}()
	t.Cleanup(func() {
		stop()
		<-resumed
	})
	seedServiceText(t, service, artifactID, "before")
	settleCurrentGeneration(t, service, artifactID)
	// The edit, once stored, owes the document a settlement, which the settle delay of an hour
	// leaves armed and not run.
	editLiveTree(t, service, artifactID, replaceRun("before", "after"))
	awaitRoomAppends(t, service, artifactID, "become durable", service.waitForDurableAppends)
	versions := latestVersionNumber(t, service, artifactID)
	if err := service.srv.CloseRoom(artifactID, true); err != nil {
		t.Fatalf("close the document's room: %v", err)
	}
	waitForNoLiveDocument(t, service, artifactID)
	ctx := context.Background()
	owes, err := settlementPending(ctx, service.store.Pool, artifactID)
	if err != nil {
		t.Fatalf("read the document's pending settlement: %v", err)
	}
	if !owes {
		t.Fatal("the document's pending-settlement row is gone once its room closed, want it kept: nothing has settled the edit")
	}

	holds := make(chan heldRoomsPool, 1)
	t.Cleanup(func() { releaseHeldRoomsPool(holds) })
	var decodes atomic.Int32
	service.bootstrapping = func(room string, _ []byte) {
		if room == artifactID && decodes.Add(1) == 1 {
			holds <- takeTheRoomsPool(t, service)
		}
	}
	// The record the settlement and its room's load run on, taken while the edit's arming holds
	// it: once no settlement is armed or running, the service may let it go.
	state := service.room(artifactID)
	service.SettleNow(artifactID)
	waitFor(t, roomWait(t), "the settlement whose warm-up load is held to log how it stopped short", func() bool {
		return logs.Lines("was not served by the database") != "" || logs.Lines("dispatch: settle document") != ""
	})
	if decodes.Load() == 0 {
		t.Fatal("the settlement's warm-up never decoded the room, so its load read nothing under its reads' bound")
	}

	// (1) The settlement stopped short on its load's reads' bound, as one the database did not serve.
	var loadErr error
	if failure := state.latestLoadFailure(); failure != nil {
		loadErr = failure.err
	}
	if !errors.Is(loadErr, errLoadReadsBound) {
		t.Fatalf("the warm-up's load ended with %v, want its reads' bound (errLoadReadsBound): something else ended the read the held rooms pool kept waiting", loadErr)
	}
	line := logs.Lines("dispatch: document settlement was not served by the database; the resumption settles it")
	for _, want := range []string{"room=" + artifactID, "warm document for settlement", errLoadReadsBound.Error()} {
		if !strings.Contains(line, want) {
			t.Errorf("the settlement whose warm-up load ran out its reads' bound logged %q, which lacks %q: want the line of a settlement the database did not serve, left to the resumption; the log was:\n%s", line, want, logs.String())
			break
		}
	}
	// (2) It counted no failure, failed no room and armed no retry, and the document is still
	// owed and unsettled.
	state.mu.Lock()
	failures, failed := state.settleFailures, state.failed
	state.mu.Unlock()
	if failures != 0 || failed != nil {
		t.Errorf("settlement failures = %d, room failure = %v; want none for a bound of this server's own", failures, failed)
	}
	if t.Failed() {
		t.FailNow()
	}
	waitFor(t, roomWait(t), "no settlement of the document to be armed or running once it stopped short, where a retry would be", func() bool {
		return !settlementUnderway(service, artifactID)
	})
	owes, err = settlementPending(ctx, service.store.Pool, artifactID)
	if err != nil {
		t.Fatalf("read the document's pending settlement: %v", err)
	}
	if !owes {
		t.Fatal("the document's pending-settlement row is gone once its settlement stopped short, want it kept for the resumption: nothing settled the edit")
	}
	if latest := latestVersionNumber(t, service, artifactID); latest != versions {
		t.Fatalf("latest version = %d, want %d: no settlement whose warm-up load failed commits", latest, versions)
	}
	t.Logf("the settlement stopped short: %s", strings.TrimSpace(line))

	// (3) With the rooms pool given back and the row a minute old, the resumption's next pass arms
	// the settlement, whose warm-up loads the room, and it settles the document.
	held := <-holds
	held.release()
	service.settle = 10 * time.Millisecond
	if _, err := service.store.Pool.Exec(ctx, `
		update doc_settlements_pending set marked_at = now() - interval '2 minutes' where artifact_id = $1
	`, artifactID); err != nil {
		t.Fatalf("make the document's pending-settlement row a minute old: %v", err)
	}
	waitFor(t, roomWait(t), "the resumption to settle the document", func() bool {
		return latestVersionNumber(t, service, artifactID) > versions
	})
	requireLatestVersionMarkdown(t, service, artifactID, "after\n")
	if loads := decodes.Load(); loads < 2 {
		t.Fatalf("the room was decoded %d time(s), want the resumed settlement's warm-up to have loaded it again", loads)
	}
	owes, err = settlementPending(ctx, service.store.Pool, artifactID)
	if err != nil {
		t.Fatalf("read the document's pending settlement: %v", err)
	}
	if owes {
		t.Fatal("the document's pending-settlement row stands once the resumption versioned the document, want it gone")
	}
}

// A cold room's load reads the room's stored history (servicePersistenceAdapter.LoadDoc) with no
// context of ygo's to bound it, and so under a bound of its own, the one its hook's reads run under
// (coldReadTimeout, errLoadReadsBound). Here a table lock the read waits behind - what a
// migration's DDL takes - is held past that bound, shortened to a second: the load ends at the
// bound with the lock still held, and the settlement whose warm-up it was stops short as one the
// database did not serve (settleOverloaded), the document still owed. Unbounded, the load and the
// settlement waiting on it waited for as long as the lock was held, past every bound the
// settlement's lease on its row adds up (settlementLease).
func TestARoomLoadWhoseHistoryReadWaitsPastItsBoundEndsAtIt(t *testing.T) {
	logs := captureWarnings(t)
	previous := coldReadTimeout
	coldReadTimeout = time.Second
	t.Cleanup(func() { coldReadTimeout = previous })
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "before")
	settleCurrentGeneration(t, service, artifactID)
	// The edit, once stored, owes the document a settlement, which the settle delay of an hour
	// leaves armed and not run, holding the room's record (unusedLocked).
	editLiveTree(t, service, artifactID, replaceRun("before", "after"))
	awaitRoomAppends(t, service, artifactID, "become durable", service.waitForDurableAppends)
	versions := latestVersionNumber(t, service, artifactID)
	if err := service.srv.CloseRoom(artifactID, true); err != nil {
		t.Fatalf("close the document's room: %v", err)
	}
	waitForNoLiveDocument(t, service, artifactID)
	state := service.room(artifactID)

	ctx := context.Background()
	hold, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the table lock's holder: %v", err)
	}
	defer hold.Rollback(ctx)
	// The load's first read is the history's head, which reads doc_checkpoints (headVersion).
	if _, err := hold.Exec(ctx, `lock table doc_checkpoints in access exclusive mode`); err != nil {
		t.Fatalf("lock the table the room's load reads first: %v", err)
	}
	started := time.Now()
	service.SettleNow(artifactID)
	const within = 15 * time.Second
	waitFor(t, within, "the settlement whose warm-up load waits behind the table lock to stop short", func() bool {
		return logs.Lines("was not served by the database") != ""
	})
	took := time.Since(started)
	var loadErr error
	if failure := state.latestLoadFailure(); failure != nil {
		loadErr = failure.err
	}
	if !errors.Is(loadErr, errLoadReadsBound) {
		t.Fatalf("the warm-up's load ended with %v, want its reads' bound (errLoadReadsBound)", loadErr)
	}
	line := logs.Lines("dispatch: document settlement was not served by the database; the resumption settles it")
	for _, want := range []string{"room=" + artifactID, "warm document for settlement", errLoadReadsBound.Error()} {
		if !strings.Contains(line, want) {
			t.Fatalf("the settlement whose warm-up load ran out its reads' bound logged %q, which lacks %q", line, want)
		}
	}
	if err := hold.Rollback(ctx); err != nil {
		t.Fatalf("release the table lock: %v", err)
	}
	owes, err := settlementPending(ctx, service.store.Pool, artifactID)
	if err != nil {
		t.Fatalf("read the document's pending settlement: %v", err)
	}
	if !owes {
		t.Fatal("the document's pending-settlement row is gone, want it kept: nothing settled the edit")
	}
	if latest := latestVersionNumber(t, service, artifactID); latest != versions {
		t.Fatalf("latest version = %d, want %d: no settlement whose warm-up load failed commits", latest, versions)
	}
	t.Logf("the load ended %v after the settlement was armed, with the table lock still held: %s", took.Round(time.Millisecond), strings.TrimSpace(line))
}

// The settlement an issue's close runs before it opens its transaction (SettleIssueDocuments,
// settleOwedNow) runs under the deadline every settlement runs under (settleRoomWithin,
// settleTimeout), not under its caller's context alone, which an unkeyed close's request leaves
// with no deadline at all (api's outlivingTheClient). Here another transaction holds the document's
// owner row past that deadline: each attempt rolls back at it, and SettleIssueDocuments returns
// with the document still owed while the row is still held. Unbounded, it waited for as long as
// the row was held, and the close with it.
func TestTheSettlementAnIssuesCloseRunsFirstEndsAtItsDeadline(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "before")
	settleCurrentGeneration(t, service, artifactID)
	editLiveTree(t, service, artifactID, replaceRun("before", "after"))
	awaitRoomAppends(t, service, artifactID, "become durable", service.waitForDurableAppends)
	versions := latestVersionNumber(t, service, artifactID)

	ctx := context.Background()
	hold, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the owner row's holder: %v", err)
	}
	defer hold.Rollback(ctx)
	if _, err := hold.Exec(ctx, `select key from issues where key = 'DOC-1' for no key update`); err != nil {
		t.Fatalf("hold the document's owner row: %v", err)
	}
	const deadline = 300 * time.Millisecond
	service.settleTimeout = deadline
	t.Cleanup(func() { service.settleTimeout = settlementTimeout })
	returned := make(chan error, 1)
	started := time.Now()
	go func() { returned <- service.SettleIssueDocuments(ctx, "DOC-1") }()
	// issueSettleAttempts attempts, each ending at the deadline, inside issueSettleBudget.
	const within = issueSettleBudget + 10*time.Second
	select {
	case err := <-returned:
		if err == nil {
			t.Fatal("SettleIssueDocuments returned no error, want the settlement that ran past its deadline reported")
		}
		t.Logf("SettleIssueDocuments returned after %v with the owner row held: %v", time.Since(started).Round(time.Millisecond), err)
	case <-time.After(within):
		_ = hold.Rollback(ctx)
		<-returned
		t.Fatalf("SettleIssueDocuments was still settling %v on with the owner row held, want each attempt ended at its %v deadline", within, deadline)
	}
	owes, err := settlementPending(ctx, service.store.Pool, artifactID)
	if err != nil {
		t.Fatalf("read the document's pending settlement: %v", err)
	}
	if !owes {
		t.Fatal("the document's pending-settlement row is gone, want it kept: no settlement committed")
	}
	if latest := latestVersionNumber(t, service, artifactID); latest != versions {
		t.Fatalf("latest version = %d, want %d: no settlement past its deadline commits", latest, versions)
	}
}

// A settlement that finds every connection of the settlements pool taken for the pool's whole
// acquire bound - for its lease on the document's pending-settlement row, the first statement it
// runs there (leaseSettlementPending) - is refused it (store.ErrPoolBusy) and stops short: the
// document stays owed, its room stays resident, and the refusal counts as no settlement failure
// however often it recurs, so overload fails no room. It took no lease, so once a connection is
// free the resumption settles it.
func TestASettlementRefusedAConnectionStaysOwedAndCountsNoFailure(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	service.store.Pool.SetAcquireBoundForTest(store.SettlementsPool, 200*time.Millisecond)
	seedServiceText(t, service, artifactID, "before")
	settleCurrentGeneration(t, service, artifactID)
	editLiveTree(t, service, artifactID, replaceRun("before", "after"))
	versions := latestVersionNumber(t, service, artifactID)

	settlements, err := service.store.Pool.Settlements()
	if err != nil {
		t.Fatalf("open the settlements pool: %v", err)
	}
	ctx := context.Background()
	var held []*pgxpool.Conn
	release := func() {
		for _, conn := range held {
			conn.Release()
		}
		held = nil
	}
	defer release()
	for range settlements.Stat().MaxConns() {
		conn, err := settlements.Acquire(ctx)
		if err != nil {
			t.Fatalf("hold a settlements connection: %v", err)
		}
		held = append(held, conn)
	}
	for attempt := range maxSettleFailures + 1 {
		settleCurrentGeneration(t, service, artifactID)
		owes, err := settlementPending(ctx, service.store.Pool, artifactID)
		if err != nil {
			t.Fatalf("read the document's pending settlement: %v", err)
		}
		if !owes {
			t.Fatalf("attempt %d: the document's pending-settlement row is gone, want it kept for the resumption", attempt)
		}
		if service.srv.GetDoc(artifactID) == nil {
			t.Fatalf("attempt %d: the document's room is gone, want it resident", attempt)
		}
		state := service.room(artifactID)
		state.mu.Lock()
		failures, failed := state.settleFailures, state.failed
		state.mu.Unlock()
		if failures != 0 || failed != nil {
			t.Fatalf("attempt %d: settlement failures = %d, room failure = %v; want none for a busy pool", attempt, failures, failed)
		}
	}
	if timeouts := settlementTimeouts(service); timeouts < maxSettleFailures+1 {
		t.Fatalf("the settlements pool refused %d acquires, want one for each of the %d settlements", timeouts, maxSettleFailures+1)
	}
	if latest := latestVersionNumber(t, service, artifactID); latest != versions {
		t.Fatalf("latest version = %d, want %d: no settlement refused a connection commits", latest, versions)
	}

	release()
	service.settle = 10 * time.Millisecond
	if err := service.resumeOwedSettlements(ctx, 0); err != nil {
		t.Fatalf("resume the owed settlement: %v", err)
	}
	waitForDocumentVersion(t, service.store, artifactID, versions+1)
	requireLatestVersionMarkdown(t, service, artifactID, "after\n")
}

// A settlement whose statement reached the database and was never answered is the same overload
// to this service as a pool with no connection free: the document stays owed, its room stays
// resident, and it counts no settlement failure however often it recurs. The statement ran
// outside nothing a retry cannot redo - a pass reads the document again and writes what it
// reads - so `store.Unserved` is what this branches on, not the narrower `IsPoolBusy`: under a
// stall of the whole pod every room whose settlement met one would otherwise count a failure,
// and three stalls would fail the room, which is the collapse these bounds exist to prevent
// (INFRA-592). The statement that stalls is the settlement's first on that pool, its lease on the
// document's pending-settlement row (leaseSettlementPending): the database may have run it, and
// the settlement, which never read the lease's end, cannot give back a lease it cannot name, so
// the lease runs out as a dead process's does; the test runs it out before the resumption's pass.
func TestASettlementWhoseStatementStallsStaysOwedAndCountsNoFailure(t *testing.T) {
	socket := storetest.NewStalledSocket(store.SettlementsPool)
	database := storetest.Reopened(t, storetest.Open(t), func(config *pgxpool.Config) {
		config.ConnConfig.DialFunc = socket.Dial
	})
	service, artifactID := newTestServiceWithDocument(t, database)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "before")
	settleCurrentGeneration(t, service, artifactID)
	editLiveTree(t, service, artifactID, replaceRun("before", "after"))
	versions := latestVersionNumber(t, service, artifactID)

	socket.StallAfter(artifactID)
	settleCurrentGeneration(t, service, artifactID)

	if !socket.Fired() {
		t.Fatal("the settlements socket never timed out, so the settlement ran as any other")
	}
	ctx := context.Background()
	owes, err := settlementPending(ctx, service.store.Pool, artifactID)
	if err != nil {
		t.Fatalf("read the document's pending settlement: %v", err)
	}
	if !owes {
		t.Fatal("the document's pending-settlement row is gone, want it kept for the resumption")
	}
	if service.srv.GetDoc(artifactID) == nil {
		t.Fatal("the document's room is gone, want it resident")
	}
	state := service.room(artifactID)
	state.mu.Lock()
	failures, failed := state.settleFailures, state.failed
	state.mu.Unlock()
	if failures != 0 || failed != nil {
		t.Fatalf("settlement failures = %d, room failure = %v; want none for a database that stopped answering", failures, failed)
	}
	if latest := latestVersionNumber(t, service, artifactID); latest != versions {
		t.Fatalf("latest version = %d, want %d: no settlement the database left unanswered commits", latest, versions)
	}

	expireSettlementLeases(t, service.store, artifactID)
	service.settle = 10 * time.Millisecond
	if err := service.resumeOwedSettlements(ctx, 0); err != nil {
		t.Fatalf("resume the owed settlement: %v", err)
	}
	waitForDocumentVersion(t, service.store, artifactID, versions+1)
	requireLatestVersionMarkdown(t, service, artifactID, "after\n")
}

// settlementTimeouts is how many acquires the settlements pool has refused at its bound.
func settlementTimeouts(service *Service) uint64 {
	for _, pool := range service.store.Pool.Stats() {
		if pool.Name == store.SettlementsPool {
			return pool.Acquire.Timeouts
		}
	}
	return 0
}

// A settlement that cannot open the settlements pool stops short before it warms its cold room.
// Every read a settlement makes goes through that pool - one through the request pool would take a
// connection admission counts for requests - so a settlement that has no settlements pool warms
// nothing, takes no request connection, and reports the pool's error with the document left owed.
func TestASettlementWithoutItsPoolWarmsNoColdRoomThroughTheRequestPool(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	ctx := context.Background()
	seedServiceText(t, service, artifactID, "Cold.\n")
	settleCurrentGeneration(t, service, artifactID)
	editLiveTree(t, service, artifactID, replaceRun("Cold.", "Cold again."))
	settleCurrentGeneration(t, service, artifactID)
	// The document's room, left cold with a settlement owed: the settlement below has to warm it
	// before it can read anything.
	if err := service.srv.CloseRoom(artifactID, true); err != nil {
		t.Fatalf("close the document's room: %v", err)
	}
	waitForNoLiveDocument(t, service, artifactID)
	if _, err := service.store.Pool.Exec(ctx, `
		insert into doc_settlements_pending (artifact_id) values ($1) on conflict (artifact_id) do nothing
	`, artifactID); err != nil {
		t.Fatalf("mark the cold document owed: %v", err)
	}
	state := service.room(artifactID)
	state.mu.Lock()
	generation := state.roomGeneration
	state.mu.Unlock()
	refused := errors.New("test: the settlements pool cannot open")
	service.store.Pool.SetOpenFailureForTest(store.SettlementsPool, refused)

	// What the request pool has handed out, before and after: the settlement must take none of it,
	// neither for the room's load nor for the issue read its warm-up's injection makes.
	before := service.store.Pool.Stat().AcquireCount()
	outcome := service.settleRoomWithin(ctx, artifactID, generation, false)

	if outcome.next != settleRetry || !errors.Is(outcome.err, refused) {
		t.Fatalf("the settlement ended as %+v, want a stop short carrying the settlements pool's own error", outcome)
	}
	if took := service.store.Pool.Stat().AcquireCount() - before; took != 0 {
		t.Fatalf("the settlement took %d request-pool connection(s), want none: settlement work reads through the settlements pool", took)
	}
	if service.srv.GetDoc(artifactID) != nil {
		t.Fatal("the settlement warmed the cold room, want it left cold: it has no pool to warm it through")
	}
	owes, err := settlementPending(ctx, service.store.Pool, artifactID)
	if err != nil {
		t.Fatalf("read the document's pending settlement: %v", err)
	}
	if !owes {
		t.Fatal("the document's pending-settlement row is gone, want it kept for the resumption")
	}
}

// A settlement that writes a version stores that version's display tree on the settlements pool
// its transaction ran on (Ledger.treePool), and takes no request-pool connection at all: the
// store runs once the commit has given the transaction's connection back (commitSettlement), so
// it takes that pool's connection again and none admission counts for requests. The tree is
// stored all the same, so the next reader of the version copies it.
func TestAVersioningSettlementTakesNoRequestPoolConnection(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "before")
	settleCurrentGeneration(t, service, artifactID)
	editLiveTree(t, service, artifactID, replaceRun("before", "after"))
	versions := latestVersionNumber(t, service, artifactID)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// The edit is durable before anything is counted, so the room's own persistence takes no
	// connection while the settlement runs.
	if err := service.waitForPendingUpdates(ctx, artifactID); err != nil {
		t.Fatalf("wait for the edit to reach persistence: %v", err)
	}
	if err := service.waitForDurableAppends(ctx, artifactID); err != nil {
		t.Fatalf("wait for the edit to become durable: %v", err)
	}
	settlements, err := service.store.Pool.Settlements()
	if err != nil {
		t.Fatalf("open the settlements pool: %v", err)
	}
	generation := roomGeneration(t, service, artifactID)

	before := service.store.Pool.Stat().AcquireCount()
	outcome := service.settleRoomWithin(ctx, artifactID, generation, false)
	took := service.store.Pool.Stat().AcquireCount() - before

	if outcome.next != settleEnded || outcome.err != nil {
		t.Fatalf("the settlement ended as %+v, want it ended having versioned the edit", outcome)
	}
	if latest := latestVersionNumber(t, service, artifactID); latest != versions+1 {
		t.Fatalf("latest version = %d, want %d: the settlement versions the edit", latest, versions+1)
	}
	if took != 0 {
		t.Fatalf("the settlement took %d request-pool connection(s), want none: a settlement stores its version's display tree on the settlements pool", took)
	}
	const markdown = "after\n"
	requireLatestVersionMarkdown(t, service, artifactID, markdown)
	stored, err := texttrees.New(settlements).Read(ctx, pmdoc.DisplayLatest, [][32]byte{pmdoc.TextSum(markdown)})
	if err != nil {
		t.Fatalf("read the version's stored tree: %v", err)
	}
	if _, held := stored[pmdoc.TextSum(markdown)]; !held {
		t.Fatalf("the settlement stored no display tree of its version %q", markdown)
	}
}

// A settlement whose deadline passes once it has written into its room finishes what it started.
// The room and its browsers hold the repair, so failing the room there would close every editor's
// socket and reload the document over work the settlement had all but committed. The room stays
// resident with its editor connected and counts no failure, the document stays owed rather than
// failed, and the settlement after it writes the version.
func TestASettlementPastItsDeadlineAfterWritingIntoTheRoomKeepsTheRoomResident(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, ":::ask{#ask-block urgency=\"med\" multiple=\"false\" state=\"open\"}\nShip it?\n:::\n\nContext before.\n")
	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(httpServer.Close)
	editor := connectPeer(t, httpServer.URL, artifactID, "alice")
	settleCurrentGeneration(t, service, artifactID)
	answer := answerBlockAsk(t, service, artifactID)
	editAsPeer(t, service, artifactID, replaceRun("Context before.", "Context after."))
	versions := latestVersionNumber(t, service, artifactID)
	room := service.srv.GetDoc(artifactID)
	if room == nil {
		t.Fatal("the document's room is not resident")
	}
	// The first settlement stamped every block id, so the one write the held settlement makes into
	// the room is the repair of the ask block answered above.
	tree, err := service.liveTree(artifactID, room)
	if err != nil {
		t.Fatalf("read the room's tree: %v", err)
	}
	if unstamped := pmdoc.BlockIDRepairCount(tree); unstamped != 0 {
		t.Fatalf("%d block(s) still want an id, so the held settlement would stamp before it repairs", unstamped)
	}

	const deadline = 2 * time.Second
	service.settleTimeout = deadline
	// When the settlement's deadline has certainly passed: its context is made just before the
	// settlement starts, so a hold until here outlives it.
	var passed atomic.Int64
	service.beforeSettleStart = func(room string) {
		if room == artifactID {
			passed.Store(time.Now().Add(deadline + 100*time.Millisecond).UnixNano())
		}
	}
	var held atomic.Bool
	service.afterSettleRepairAppend = func(room string) {
		if room != artifactID || !held.CompareAndSwap(false, true) {
			return
		}
		// The repair is in the room and its browsers, and appended; hold the pass here until the
		// settlement's deadline has passed, so what it has left to do meets a deadline gone.
		for until := time.Unix(0, passed.Load()); time.Now().Before(until); {
			time.Sleep(5 * time.Millisecond)
		}
	}
	settleCurrentGeneration(t, service, artifactID)
	service.beforeSettleStart, service.afterSettleRepairAppend = nil, nil
	service.settleTimeout = settlementTimeout

	if !held.Load() {
		t.Fatal("the settlement wrote no repair into the room, so its deadline never passed after a write")
	}
	state := service.room(artifactID)
	state.mu.Lock()
	failed, failures := state.failed, state.settleFailures
	state.mu.Unlock()
	if failed != nil || failures != 0 {
		t.Fatalf("the room failed with %v and counted %d settlement failure(s), want neither: the deadline passed after the settlement had written into the room", failed, failures)
	}
	if resident := service.srv.GetDoc(artifactID); resident != room {
		t.Fatalf("the room instance is %p, want the resident %p: the settlement evicted the room it wrote into", resident, room)
	}
	select {
	case <-editor.Ended:
		t.Fatal("the editor's socket was closed, want it open: the room it edits never failed")
	default:
	}
	owes, err := settlementPending(context.Background(), service.store.Pool, artifactID)
	if err != nil {
		t.Fatalf("read the document's pending settlement: %v", err)
	}
	if !owes {
		t.Fatal("the document's pending-settlement row is gone, want it kept: the settlement ended at its deadline before its version")
	}

	// The repair the held settlement committed is in the store, and the settlement after it writes
	// the version that holds it.
	settleCurrentGeneration(t, service, artifactID)
	want := ":::ask{#ask-block urgency=\"med\" multiple=\"false\" state=\"answered\" answered_by=\"alice\" answered_at=\"" +
		answer.At.Format(time.RFC3339Nano) + "\" selected=\"[]\"}\nShip it?\n:::\n\nContext after.\n"
	requireLatestVersionMarkdown(t, service, artifactID, want)
	if latest := latestVersionNumber(t, service, artifactID); latest != versions+1 {
		t.Fatalf("latest version = %d, want %d", latest, versions+1)
	}
	requireNoSuppressedSlots(t, service, artifactID, "after the settlement held past its deadline")
}

// A settlement renders its document before it locks the document's owner row: while its render is
// held for a second, another connection takes that row at once.
func TestASettlementRendersWithoutHoldingItsDocumentsOwnerRow(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "before")
	settleCurrentGeneration(t, service, artifactID)
	editLiveTree(t, service, artifactID, replaceRun("before", "after"))
	versions := latestVersionNumber(t, service, artifactID)

	rendering := make(chan struct{})
	var once sync.Once
	service.beforeSettleRender = func(room string) {
		if room == artifactID {
			once.Do(func() {
				close(rendering)
				time.Sleep(time.Second)
			})
		}
	}
	generation := roomGeneration(t, service, artifactID)
	settled := make(chan struct{})
	go func() {
		defer close(settled)
		service.settleRoom(context.Background(), artifactID, generation)
	}()
	select {
	case <-rendering:
	case <-time.After(10 * time.Second):
		t.Fatal("the settlement never reached its render")
	}
	ctx := context.Background()
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	var key string
	err = tx.QueryRow(ctx, `select key from issues where key = 'DOC-1' for no key update nowait`).Scan(&key)
	_ = tx.Rollback(ctx)
	if err != nil {
		t.Fatalf("lock the owner row while the settlement renders: %v, want it free", err)
	}
	select {
	case <-settled:
	case <-time.After(30 * time.Second):
		t.Fatal("the settlement never returned")
	}
	service.beforeSettleRender = nil
	if latest := latestVersionNumber(t, service, artifactID); latest != versions+1 {
		t.Fatalf("latest version = %d, want %d", latest, versions+1)
	}
	requireLatestVersionMarkdown(t, service, artifactID, "after\n")
}

// A settlement that stamps block ids and repairs an ask block renders nothing under its document's
// owner row either: while each of its renders - the live tree each pass read, the planned repairs -
// is held for a moment, another connection takes that row at once. The document still gets its
// stamp, its repair and its version.
func TestARepairingSettlementRendersNothingUnderItsDocumentsOwnerRow(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, ":::ask{#ask-block urgency=\"med\" multiple=\"false\" state=\"open\"}\nShip it?\n:::\n\nContext before.\n")
	service.settleRoom(context.Background(), artifactID, 0)
	answer := answerBlockAsk(t, service, artifactID)
	editLiveTree(t, service, artifactID, appendUnidentifiedBlocks(t, "Added."))
	versions := latestVersionNumber(t, service, artifactID)

	var renders atomic.Int64
	service.beforeSettleRender = func(room string) {
		if room != artifactID {
			return
		}
		renders.Add(1)
		ctx := context.Background()
		tx, err := service.store.Pool.Begin(ctx)
		if err != nil {
			t.Errorf("begin: %v", err)
			return
		}
		defer tx.Rollback(ctx)
		var key string
		if err := tx.QueryRow(ctx, `select key from issues where key = 'DOC-1' for no key update nowait`).Scan(&key); err != nil {
			t.Errorf("render %d: lock the owner row while the settlement renders: %v, want it free", renders.Load(), err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	settleCurrentGeneration(t, service, artifactID)
	service.beforeSettleRender = nil
	// The live tree, the stamped tree, the planned repairs, the tree they are committed from and
	// the repaired tree versioned. The stamp itself renders nothing: the room measures the class
	// its update is stored with (repairClass, storeRepair).
	if got := renders.Load(); got < 5 {
		t.Fatalf("the settlement rendered %d times, want a render for each of its stamp, plan, repairs and version", got)
	}
	want := ":::ask{#ask-block urgency=\"med\" multiple=\"false\" state=\"answered\" answered_by=\"alice\" answered_at=\"" +
		answer.At.Format(time.RFC3339Nano) + "\" selected=\"[]\"}\nShip it?\n:::\n\nContext before.\n\nAdded.\n"
	requireLatestVersionMarkdown(t, service, artifactID, want)
	if latest := latestVersionNumber(t, service, artifactID); latest != versions+1 {
		t.Fatalf("latest version = %d, want %d", latest, versions+1)
	}
	requireNoSuppressedSlots(t, service, artifactID, "after the settlement")
}

// An update that becomes durable while a settlement renders is missing from the tree it renders.
// The settlement writes no version of that tree, whose cursor would claim the update: under its
// locks it finds the room past where it stood when it copied the tree, rolls back, and versions the
// document read again, the update included.
func TestAnUpdateLandingDuringTheRenderProducesNoVersionOfTheStaleTree(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "First.\n\nSecond.\n")
	settleCurrentGeneration(t, service, artifactID)
	editAsPeer(t, service, artifactID, replaceRun("First.", "First, edited."))
	before := latestVersionNumber(t, service, artifactID)

	var landed atomic.Bool
	service.beforeSettleRender = func(room string) {
		if room != artifactID || !landed.CompareAndSwap(false, true) {
			return
		}
		editAsPeer(t, service, artifactID, replaceRun("Second.", "Second, edited."))
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := service.waitForPendingUpdates(ctx, artifactID); err != nil {
			t.Errorf("wait for the update to reach persistence: %v", err)
		}
		if err := service.waitForDurableAppends(ctx, artifactID); err != nil {
			t.Errorf("wait for the update to become durable: %v", err)
		}
	}
	settleCurrentGeneration(t, service, artifactID)
	service.beforeSettleRender = nil
	if !landed.Load() {
		t.Fatal("the settlement never reached its render")
	}
	var stale int
	if err := service.store.Pool.QueryRow(context.Background(), `
		select count(*) from artifact_versions where artifact_id = $1 and number > $2 and markdown = $3
	`, artifactID, before, "First, edited.\n\nSecond.\n").Scan(&stale); err != nil {
		t.Fatalf("count versions of the stale tree: %v", err)
	}
	if stale != 0 {
		t.Fatalf("%d version(s) of the tree rendered without the update that landed during its render", stale)
	}
	requireLatestVersionMarkdown(t, service, artifactID, "First, edited.\n\nSecond, edited.\n")
	if latest := latestVersionNumber(t, service, artifactID); latest != before+1 {
		t.Fatalf("latest version = %d, want %d", latest, before+1)
	}
	if markdown, cursor := latestVersionOf(t, service.store, artifactID); cursor != storedHead(t, service.store, artifactID) {
		t.Fatalf("version %q records update cursor %d, want the head %d", markdown, cursor, storedHead(t, service.store, artifactID))
	}
}

// A document whose settlement repairs it - an ask block whose row was answered - gets the repair in
// one commit, with no version, and its version in the next: when the last pass renders the document,
// the repair is stored and the latest version is still the one before it. The version holds the
// repair and the edit that owed it, records the repair's update in its cursor, and the repair's
// event names that version.
func TestADocumentNeedingRepairsGetsThemInOneCommitAndItsVersionInTheNext(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, ":::ask{#ask-block urgency=\"med\" multiple=\"false\" state=\"open\"}\nShip it?\n:::\n\nContext before.\n")
	service.settleRoom(context.Background(), artifactID, 0)
	answer := answerBlockAsk(t, service, artifactID)
	editAsPeer(t, service, artifactID, replaceRun("Context before.", "Context after."))
	generation := roomGeneration(t, service, artifactID)
	before := latestVersionNumber(t, service, artifactID)
	headBefore := storedHead(t, service.store, artifactID)

	// What the database holds as each render starts; the last is the versioning pass's.
	var renders, headAtLast, versionAtLast atomic.Int64
	service.beforeSettleRender = func(room string) {
		if room != artifactID {
			return
		}
		renders.Add(1)
		headAtLast.Store(storedHead(t, service.store, artifactID))
		versionAtLast.Store(int64(latestVersionNumber(t, service, artifactID)))
	}
	service.settleRoom(context.Background(), artifactID, generation)
	service.beforeSettleRender = nil
	if renders.Load() < 3 {
		t.Fatalf("the settlement rendered the document %d time(s), want its repairs planned and committed and its version written by passes of their own", renders.Load())
	}
	if headAtLast.Load() <= headBefore {
		t.Fatalf("when the version's pass rendered, the head stood at %d, want the repair stored past %d", headAtLast.Load(), headBefore)
	}
	if versionAtLast.Load() != int64(before) {
		t.Fatalf("when the version's pass rendered, the latest version was %d, want still %d: the repair commits no version", versionAtLast.Load(), before)
	}
	want := ":::ask{#ask-block urgency=\"med\" multiple=\"false\" state=\"answered\" answered_by=\"alice\" answered_at=\"" +
		answer.At.Format(time.RFC3339Nano) + "\" selected=\"[]\"}\nShip it?\n:::\n\nContext after.\n"
	requireLatestVersionMarkdown(t, service, artifactID, want)
	if latest := latestVersionNumber(t, service, artifactID); latest != before+1 {
		t.Fatalf("latest version = %d, want %d", latest, before+1)
	}
	if _, cursor := latestVersionOf(t, service.store, artifactID); cursor != headAtLast.Load() {
		t.Fatalf("the version records update cursor %d, want the repair's %d", cursor, headAtLast.Load())
	}
	var named string
	if err := service.store.Pool.QueryRow(context.Background(), `
		select payload->>'version' from events
		where type = 'block.repaired' and payload->>'block_id' = 'ask-block' order by id desc limit 1
	`).Scan(&named); err != nil {
		t.Fatalf("read the repair's event: %v", err)
	}
	if named != strconv.Itoa(before+1) {
		t.Fatalf("the repair's event names version %s, want %d, the version that holds it", named, before+1)
	}
}

package docs

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/persistence"

	"github.com/sjawhar/envoy/internal/clock"
	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// A deploy usually finds someone with a document open, its tab connected to the room with what it
// last typed owed a settlement. Shutdown settles the room while it is still loaded and only then
// closes the editor's connection: ygo's CloseRoom evicts the room as it closes it, and a settlement
// does not load a room during shutdown, so a room closed first would leave the edit unversioned
// until someone next opened the document.
func TestShutdownSettlesARoomWithAnEditorConnectedBeforeClosingIt(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before")
	service := newShutdownTestService(t, database, nil)
	seedServiceText(t, service, artifactID, "before")
	settleCurrentGeneration(t, service, artifactID)
	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(httpServer.Close)
	editor := connectPeer(t, httpServer.URL, artifactID, "alice")
	if err := editor.WaitForText(t, peerMarkdown, "before\n"); err != nil {
		t.Fatal(err)
	}
	typeOver(t, editor, "before", "after")
	waitForSettlementOwed(t, service, artifactID)

	if err := service.Shutdown(testwait.Context(t)); err != nil {
		t.Fatalf("shutdown with an editor connected: %v", err)
	}
	testwait.Receive(t, "the editor's connection to end once Shutdown returned", editor.Ended)
	if number, markdown, authors := latestDocumentVersion(t, database, artifactID); number != 2 || markdown != "after\n" ||
		len(authors) != 1 || authors[0] != (model.Actor{Kind: "user", ID: "alice", Proven: true}) {
		t.Fatalf("the document's latest version is %d holding %q by %v, want version 2 holding the editor's edit, %q, credited to alice",
			number, markdown, authors, "after\n")
	}
	if owed, err := settlementPending(context.Background(), database.Pool, artifactID); err != nil || owed {
		t.Fatalf("the document still owes its settlement (%v) after the shutdown that ran it", err)
	}
}

// An agent's write to a document no room holds loads no room (audit finding 11); it arms the
// document's settlement, whose timer loads the room when it fires. A deploy that stops the process
// inside that delay stops the timer, and Shutdown's own settlement of the document loads the room
// in its place, so the write is versioned before the process exits rather than left to resume.
func TestShutdownSettlesTheDocumentAColdWriteLeftOwing(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before")
	service := newShutdownTestService(t, database, nil)
	seedServiceText(t, service, artifactID, "before")
	settleCurrentGeneration(t, service, artifactID)
	closeDocumentRoom(t, service, artifactID)
	alice := model.Actor{Kind: "user", ID: "alice"}
	if _, err := joinedApplyOps(service, artifactID, []model.EditOp{{Op: "replace", Find: "before", With: "after"}}, alice, nil); err != nil {
		t.Fatalf("apply the cold write: %v", err)
	}
	if service.roomHeld(artifactID) {
		t.Fatal("the cold write loaded a room of the document")
	}
	waitForSettlementOwed(t, service, artifactID)

	if err := service.Shutdown(testwait.Context(t)); err != nil {
		t.Fatalf("shutdown with a cold write's settlement owed: %v", err)
	}
	if number, markdown, authors := latestDocumentVersion(t, database, artifactID); number != 2 || markdown != "after\n" ||
		len(authors) != 1 || authors[0] != alice {
		t.Fatalf("the document's latest version is %d holding %q by %v, want version 2 holding the cold write, %q, credited to alice",
			number, markdown, authors, "after\n")
	}
	if owed, err := settlementPending(context.Background(), database.Pool, artifactID); err != nil || owed {
		t.Fatalf("the document still owes its settlement (%v) after the shutdown that ran it", err)
	}
}

// A browser leaving a room settles what it was owed at once (settleLastPeer), on ygo's disconnect
// goroutine, in place of the timer it stops. Shutdown joins that settlement as it joins a timer's:
// it reports which documents settled, and stops ygo, only once the settlement has returned, and the
// process does not close the store under it. The departing browser's socket ends only once that
// settlement has returned, so the drain's forced close of the room, which waits for each peer's
// read loop, orders Shutdown after it too; TestShutdownJoinsATimersSettlementUnderWay is the row
// where the join alone does.
func TestShutdownJoinsTheSettlementItsLastBrowserLeavingStarted(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before")
	drainClock := clocktest.New(time.Now())
	service := newShutdownTestServiceOn(t, database, nil, drainClock)
	pause := pauseRepairCommits(service)
	t.Cleanup(pause.let)
	seedServiceText(t, service, artifactID, "before")
	settleCurrentGeneration(t, service, artifactID)
	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(httpServer.Close)
	browser := connectPeer(t, httpServer.URL, artifactID, "alice")
	if err := browser.WaitForText(t, peerMarkdown, "before\n"); err != nil {
		t.Fatal(err)
	}
	// A block with no id arms the room's settlement, which has to stamp one.
	editLiveTree(t, service, artifactID, appendUnidentifiedBlocks(t, "added"))
	ctx := testwait.Context(t)
	if err := service.waitForPendingUpdates(ctx, artifactID); err != nil {
		t.Fatalf("wait for the edit to reach persistence: %v", err)
	}
	if err := service.waitForDurableAppends(ctx, artifactID); err != nil {
		t.Fatalf("wait for the edit to be stored: %v", err)
	}

	pause.armed.Store(true)
	browser.Close()
	testwait.Receive(t, "the settlement the browser's leaving owed to stamp the room", pause.held)
	// Shutdown's own settlement of the room waits on the owner row the held one holds, and its
	// drain budget - which runs on the service's clock, so the test ends it - frees neither that
	// wait nor the held settlement itself. The goroutine records whether the held settlement had
	// been let go when Shutdown returned, which is the order this row is about: a Shutdown that
	// returned while the stamp was still committing records false.
	var released atomic.Bool
	shutdown := make(chan error, 1)
	var returnedAfterRelease bool
	go func() {
		err := service.Shutdown(testwait.Context(t))
		returnedAfterRelease = released.Load()
		shutdown <- err
	}()
	budget := drainClock.Now().Add(ShutdownDrainBudget)
	waitFor(t, "Shutdown to arm its drain budget on the service's clock", func() bool {
		return slices.Contains(drainClock.Armed(), budget)
	})
	drainClock.Advance(ShutdownDrainBudget)
	released.Store(true)
	pause.let()
	if err := testwait.Receive(t, "Shutdown to return once the settlement the browser's leaving started could go on", shutdown); err != nil {
		t.Fatalf("shutdown under the browser's settlement: %v", err)
	}
	if !returnedAfterRelease {
		t.Error("Shutdown returned while the settlement the browser's leaving started was still committing its stamp")
	}
	// The stamp went with the shutdown's gate, so the document is left to resume, its edit stored.
	if owed, err := settlementPending(context.Background(), database.Pool, artifactID); err != nil || !owed {
		t.Fatalf("the document owes no settlement (%v) after a shutdown that settled nothing", err)
	}
	waitForPersistedProofText(t, database, artifactID, "before\n\nadded\n")
}

// A settlement its timer started runs on the timer's goroutine under the timer's settleWG count.
// Held once its transaction has ended, it holds no lock and no peer's read loop, and the document
// it settled owes nothing, so Shutdown's drain neither settles the room nor waits on it, and a room
// with no editor connected is not closed: Shutdown's join of the settlements under way
// (waitSettles) is the one thing that keeps it from returning - stopping ygo, and leaving the store
// to its caller to close - while that settlement still runs.
func TestShutdownJoinsATimersSettlementUnderWay(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before")
	drainClock := clocktest.New(time.Now())
	service := newShutdownTestServiceOn(t, database, nil, drainClock)
	hold := holdSettlementsPastTheirTransaction(service)
	t.Cleanup(hold.let)
	seedServiceText(t, service, artifactID, "before")
	settleCurrentGeneration(t, service, artifactID)
	editLiveTree(t, service, artifactID, replaceRun("before", "after"))
	waitForSettlementOwed(t, service, artifactID)
	if err := service.waitForDurableAppends(testwait.Context(t), artifactID); err != nil {
		t.Fatalf("wait for the edit to be stored: %v", err)
	}

	// The edit armed the room's settlement on its hour-long timer; arming it again with no delay
	// fires it now, so the settlement runs as the timer's own.
	hold.armed.Store(true)
	state := service.lockState(artifactID)
	service.scheduleSettleAfterLocked(artifactID, state, 0)
	service.unlockState(artifactID, state)
	testwait.Receive(t, "the timer's settlement to end its transaction", hold.held)
	var released atomic.Bool
	shutdown := make(chan error, 1)
	var returnedAfterRelease bool
	go func() {
		err := service.Shutdown(testwait.Context(t))
		returnedAfterRelease = released.Load()
		shutdown <- err
	}()
	// The drain budget runs on the service's clock, so the test ends it: that frees the join's
	// first, budgeted wait, and leaves the wait after the gate closes.
	budget := drainClock.Now().Add(ShutdownDrainBudget)
	waitFor(t, "Shutdown to arm its drain budget on the service's clock", func() bool {
		return slices.Contains(drainClock.Armed(), budget)
	})
	drainClock.Advance(ShutdownDrainBudget)
	testwait.Quiet(t, "Shutdown returning while the timer's settlement is held", shutdown, 2*time.Second,
		"no state shows Shutdown waiting in its join; a Shutdown that skips it has only its evictions, "+
			"one database read and ygo's stop left once the budget ends, which take milliseconds")
	released.Store(true)
	hold.let()
	if err := testwait.Receive(t, "Shutdown to return once the timer's settlement has returned", shutdown); err != nil {
		t.Fatalf("shutdown under the timer's settlement: %v", err)
	}
	if !returnedAfterRelease {
		t.Error("Shutdown returned while the timer's settlement was still under way")
	}
	// The settlement committed before Shutdown began, so the document owes nothing.
	if owed, err := settlementPending(context.Background(), database.Pool, artifactID); err != nil || owed {
		t.Fatalf("the document still owes its settlement (%v) after the timer's settlement committed", err)
	}
	if _, markdown, _ := latestDocumentVersion(t, database, artifactID); markdown != "after\n" {
		t.Fatalf("the document's latest version holds %q, want the timer's settlement's %q", markdown, "after\n")
	}
}

// settlementHold holds a room's next settlement once armed, after its transaction has ended and
// before it returns (Service.afterSettleTransaction): the settlement has committed, holds no lock,
// and still counts as under way.
type settlementHold struct {
	armed   atomic.Bool
	held    chan struct{}
	proceed chan struct{}
	release sync.Once
}

// holdSettlementsPastTheirTransaction installs a settlementHold on service, whose settlements it
// sees from now on.
func holdSettlementsPastTheirTransaction(service *Service) *settlementHold {
	hold := &settlementHold{held: make(chan struct{}), proceed: make(chan struct{})}
	service.afterSettleTransaction = func(string) {
		if hold.armed.CompareAndSwap(true, false) {
			close(hold.held)
			<-hold.proceed
		}
	}
	return hold
}

func (hold *settlementHold) let() {
	hold.release.Do(func() { close(hold.proceed) })
}

// An append a room queued before the signal can be slow to store - waiting on its document's lock
// or a slow write - and only that room's settlement can wait on it. Every other room drains, reads
// what it owes and settles on its own, so the quiet document's edit is versioned while the held
// room's append is still outstanding, and the held room alone is left to resume.
func TestShutdownSettlesEveryOtherRoomWhileOneRoomsQueuedAppendIsHeld(t *testing.T) {
	database := storetest.Open(t)
	quiet := createIssueDocument(t, database, 1, "quiet")
	held := createIssueDocument(t, database, 2, "held")
	appends := &heldStore{VersionedStore: NewPgVersioned(database), room: held, entered: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(appends.let)
	drainClock := clocktest.New(time.Now())
	service := newShutdownTestServiceOn(t, database, appends, drainClock)
	seedServiceText(t, service, quiet, "quiet")
	settleCurrentGeneration(t, service, quiet)
	seedServiceText(t, service, held, "held")
	settleCurrentGeneration(t, service, held)
	editLiveTree(t, service, quiet, replaceRun("quiet", "quiet, edited"))
	waitForSettlementOwed(t, service, quiet)
	ctx := testwait.Context(t)
	if err := service.waitForDurableAppends(ctx, quiet); err != nil {
		t.Fatalf("wait for the quiet document's edit to be stored: %v", err)
	}
	appends.armed.Store(true)
	editLiveTree(t, service, held, replaceRun("held", "held, edited"))
	testwait.Receive(t, "the held room's append to reach the store", appends.entered)

	shutdown := make(chan error, 1)
	go func() { shutdown <- service.Shutdown(testwait.Context(t)) }()
	// The drain budget runs on the service's clock: the test ends it once the quiet room has
	// settled inside it, while the held room's append is still outstanding.
	budget := drainClock.Now().Add(ShutdownDrainBudget)
	waitFor(t, "Shutdown to arm its drain budget on the service's clock", func() bool {
		return slices.Contains(drainClock.Armed(), budget)
	})
	waitFor(t, "the quiet document's settlement to commit inside the drain budget", func() bool {
		owed, err := settlementPending(context.Background(), database.Pool, quiet)
		return err == nil && !owed
	})
	drainClock.Advance(ShutdownDrainBudget)
	shutdownErr := testwait.Receive(t, "Shutdown to return once its drain budget ended", shutdown)
	appends.let()
	if number, markdown, _ := latestDocumentVersion(t, database, quiet); number != 2 || markdown != "quiet, edited\n" {
		t.Errorf("the quiet document's latest version is %d holding %q, want version 2 holding %q: another room's held append spent its settlement's budget",
			number, markdown, "quiet, edited\n")
	}
	if owed, err := settlementPending(context.Background(), database.Pool, quiet); err != nil || owed {
		t.Errorf("the quiet document still owes its settlement (%v) after the shutdown that ran it", err)
	}
	// The held append lands once let go, and the pending-settlement row it writes resumes the
	// held document's settlement in the next process.
	waitForSettlementOwed(t, service, held)
	if number, _, _ := latestDocumentVersion(t, database, held); number != 1 {
		t.Errorf("the held document's latest version is %d, want 1 with its settlement left to the next process", number)
	}
	if !errors.Is(shutdownErr, context.DeadlineExceeded) {
		t.Errorf("Shutdown returned %v, want the held room's append the drain budget did not see through", shutdownErr)
	}
}

// A room that keeps taking writes while Shutdown drains it - not only the one already queued when
// Shutdown scanned the room, but a second recorded only after that scan started - still settles
// within the per-room budget rather than running it out. The live durableAppends counter Shutdown
// waits on has to see every append through, whenever it was recorded. The second write comes from
// a connected editor's own keystroke, not a live-tree injection: an injected write is refused once
// the room is in Shutdown's shutdownRooms set (allowInject, websocket.go), so an editor already
// connected before the scan is the only writer that can land a genuinely new append afterward.
// The barrier on service.shuttingDown makes that ordering deterministic rather than a race the
// goroutine scheduler happens to win (LEGION-501).
func TestShutdownSettlesABurstOfAppendsIncludingOneRecordedAfterItsScan(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before")
	first := &burstGate{entered: make(chan struct{}), release: make(chan struct{})}
	second := &burstGate{entered: make(chan struct{}), release: make(chan struct{})}
	appends := &burstStore{VersionedStore: NewPgVersioned(database), room: artifactID, gates: []*burstGate{first, second}}
	t.Cleanup(func() {
		first.let()
		second.let()
	})
	drainClock := clocktest.New(time.Now())
	service := newShutdownTestServiceOn(t, database, appends, drainClock)
	seedServiceText(t, service, artifactID, "before")
	settleCurrentGeneration(t, service, artifactID)
	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(httpServer.Close)
	editor := connectPeer(t, httpServer.URL, artifactID, "alice")
	if err := editor.WaitForText(t, peerMarkdown, "before\n"); err != nil {
		t.Fatal(err)
	}

	appends.armed.Store(true)
	editLiveTree(t, service, artifactID, replaceRun("before", "first"))
	testwait.Receive(t, "the first append to reach the store", first.entered)
	if err := editor.WaitForText(t, peerMarkdown, "first\n"); err != nil {
		t.Fatal(err)
	}

	shutdown := make(chan error, 1)
	go func() { shutdown <- service.Shutdown(testwait.Context(t)) }()
	waitFor(t, "Shutdown to have scanned the room", func() bool {
		return service.shuttingDown(artifactID)
	})

	// Only now, deterministically after the scan, does a second write land - the already-connected
	// editor's own keystroke, which Shutdown's inject refusal never reaches.
	typeOver(t, editor, "first", "second")
	waitFor(t, "both appends to be recorded as outstanding", func() bool {
		return service.room(artifactID).durableAppends.Load() >= 2
	})
	// Both appends stay queued past the pre-fix 5 s drain budget, and land with time to spare
	// inside the 9 s per-room budget this round widened it to: the budget runs on the service's
	// clock, which the test moves seven seconds before it lets the appends go.
	budget := drainClock.Now().Add(ShutdownDrainBudget)
	waitFor(t, "Shutdown to arm its drain budget on the service's clock", func() bool {
		return slices.Contains(drainClock.Armed(), budget)
	})
	drainClock.Advance(7 * time.Second)
	first.let()
	testwait.Receive(t, "the second append to reach the store once the first let go", second.entered)
	second.let()

	shutdownErr := testwait.Receive(t, "Shutdown to return once both appends landed", shutdown)
	if shutdownErr != nil {
		t.Errorf("Shutdown returned %v, want nil: the burst should settle within the per-room drain budget", shutdownErr)
	}
	if number, markdown, _ := latestDocumentVersion(t, database, artifactID); number != 2 || markdown != "second\n" {
		t.Errorf("the document's latest version is %d holding %q, want version 2 holding %q", number, markdown, "second\n")
	}
}

// A room whose settlement is still rendering when Shutdown's own drain budget (drainCtx) ends
// does not make Shutdown return the settlements-unconfirmed error, as long as the per-room worker
// returns within the document service's whole deadline: documentShutdownTimeout's 5 s reserve past
// ShutdownDrainBudget comfortably outlasts the 3.4 s overrun service_test.go's own measurement
// found (a 1 MiB document settling in 4.5 s against a 5 s budget). Shutdown's wait for its
// per-room workers (waitGroup(ctx, &workers)) runs under the whole outer deadline, not the drain
// budget alone, because a render or commit already under way when the budget ends does not stop
// for it; drainCtx having already expired by the time this one tries to commit still leaves it to
// resume (a documented, non-error outcome), but the worker returns in time for Shutdown itself to
// read that back and finish cleanly rather than time out (LEGION-501).
func TestShutdownReturnsNilForARenderStillRunningPastItsDrainBudget(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before")
	drainClock := clocktest.New(time.Now())
	service := newShutdownTestServiceOn(t, database, nil, drainClock)
	seedServiceText(t, service, artifactID, "before")
	settleCurrentGeneration(t, service, artifactID)
	editLiveTree(t, service, artifactID, replaceRun("before", "after"))
	waitForSettlementOwed(t, service, artifactID)
	appendCtx := testwait.Context(t)
	if err := service.waitForDurableAppends(appendCtx, artifactID); err != nil {
		t.Fatalf("wait for the edit to be stored: %v", err)
	}

	entered, overrun := make(chan struct{}), make(chan struct{})
	var once sync.Once
	service.afterSettleReconcile = func(room string) {
		if room != artifactID {
			return
		}
		once.Do(func() { close(entered) })
		// The render goes on until the test has moved the service's clock 3.4 s past the per-room
		// drain budget - the measured overrun - with 1.6 s of the reserve still to spare. Not a
		// context wait: a render or commit already running does not stop the instant drainCtx's
		// deadline passes either.
		<-overrun
	}
	t.Cleanup(func() {
		select {
		case <-overrun:
		default:
			close(overrun)
		}
	})

	// The document service's own production deadline: ShutdownDrainBudget plus the 5 s reserve
	// cmd/dispatch/shutdown.go's documentShutdownTimeout now adds past it, on the same clock.
	shutdownCtx, cancelShutdown := drainClock.WithTimeout(context.Background(), ShutdownDrainBudget+5*time.Second)
	defer cancelShutdown()
	shutdown := make(chan error, 1)
	go func() { shutdown <- service.Shutdown(shutdownCtx) }()
	testwait.Receive(t, "the settlement to reach its reconcile hook", entered)
	drainClock.Advance(ShutdownDrainBudget + 3400*time.Millisecond)
	close(overrun)
	shutdownErr := testwait.Receive(t, "Shutdown to return once the render finished", shutdown)
	if shutdownErr != nil {
		t.Errorf("Shutdown returned %v, want nil: a render already running when the budget ends should still let its worker return in time", shutdownErr)
	}
}

// A settlement that has to write into its room - stamping a block id here - is refused that write
// once Shutdown begins closing the room. Shutdown leaves the document to resume from its
// pending-settlement row, says so at WARN, and counts no settlement failure, which three of would
// fail the room.
func TestShutdownLeavesASettlementThatMustWriteIntoItsRoomToResume(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before")
	service := newShutdownTestService(t, database, nil)
	seedServiceText(t, service, artifactID, "before")
	settleCurrentGeneration(t, service, artifactID)
	// A block with no id arms the room's settlement, which has to stamp one.
	editLiveTree(t, service, artifactID, appendUnidentifiedBlocks(t, "added"))
	waitForSettlementOwed(t, service, artifactID)
	ctx := testwait.Context(t)
	if err := service.waitForPendingUpdates(ctx, artifactID); err != nil {
		t.Fatalf("wait for the edit to reach persistence: %v", err)
	}
	if err := service.waitForDurableAppends(ctx, artifactID); err != nil {
		t.Fatalf("wait for the edit to be stored: %v", err)
	}
	logs := &storetest.LockedLog{}
	logtest.SetDefault(t, slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelWarn}))

	if err := service.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown with a settlement that must stamp its room: %v", err)
	}
	state := service.room(artifactID)
	state.mu.Lock()
	failures := state.settleFailures
	state.mu.Unlock()
	if failures != 0 {
		t.Errorf("the refused write counted %d settlement failures, want none", failures)
	}
	output := logs.String()
	if !strings.Contains(output, `msg="dispatch: skip shutdown document settlement that has to write into its room" room=`+artifactID) {
		t.Errorf("Shutdown did not log the settlement it left because its room refused the write:\n%s", output)
	}
	if strings.Contains(output, `msg="dispatch: settle document"`) {
		t.Errorf("Shutdown logged the refused write as a settlement failure:\n%s", output)
	}
	if owed, err := settlementPending(context.Background(), database.Pool, artifactID); err != nil || !owed {
		t.Errorf("the document owes no settlement (%v), want it left to resume", err)
	}
}

// newShutdownTestService is a document service whose settlements wait an hour, so a test's
// Shutdown is the one that runs them, and whose browsers name their person in X-Dispatch-User.
// appends, when set, stands in for the store's own document persistence.
func newShutdownTestService(t *testing.T, database *store.Store, appends VersionedStore) *Service {
	t.Helper()
	return newShutdownTestServiceOn(t, database, appends, nil)
}

// newShutdownTestServiceOn is newShutdownTestService on serviceClock (Deps.Clock), which
// Shutdown's drain budget runs on; the real clock when nil.
func newShutdownTestServiceOn(t *testing.T, database *store.Store, appends VersionedStore, serviceClock clock.Clock) *Service {
	t.Helper()
	service := New(Deps{
		Store: database, Persistence: appends, Events: events.NewBroker(),
		Identity: identity.HeaderIdentity{Header: "X-Dispatch-User", People: store.NewPgPeopleStore(database.Pool, testSigningKey, nil)},
		Settle:   time.Hour,
		Clock:    serviceClock,
	})
	// A test that already called Shutdown runs this one a second time; it logs its own
	// "left to resume" line but does no second settlement attempt (settleRoomWithin's
	// s.stopping guard returns it almost instantly), so it is a harmless no-op, not a race.
	t.Cleanup(func() { _ = service.Shutdown(testwait.Context(t)) })
	return service
}

// peerMarkdown renders the peer's copy of the document, read under its lock while the room's
// updates apply, or "" while the copy holds no document it can render.
func peerMarkdown(peer *docstest.Peer) string {
	fragment := peer.Doc.GetXmlFragment(fragmentName)
	var rendered string
	peer.Doc.Transact(func(txn *crdt.Transaction) {
		tree, err := pmdoc.ReadInTransaction(txn, fragment)
		if err != nil {
			return
		}
		rendered, _ = pmdoc.Render(tree)
	}, nil)
	return rendered
}

// typeOver types replacement over old, the first paragraph's whole text, in one keystroke's
// transaction, and sends the room the update it made.
func typeOver(t *testing.T, peer *docstest.Peer, old, replacement string) {
	t.Helper()
	fragment := peer.Doc.GetXmlFragment(fragmentName)
	var changeErr error
	if _, err := peer.Send(func(txn *crdt.Transaction) {
		tree, err := pmdoc.ReadInTransaction(txn, fragment)
		if err != nil {
			changeErr = err
			return
		}
		if len(tree.Children) == 0 || len(tree.Children[0].Children) != 1 || tree.Children[0].Children[0].Text != old {
			changeErr = errors.New("the first paragraph does not hold only " + old)
			return
		}
		tree.Children[0].Children[0].Text = replacement
		changeErr = pmdoc.Update(txn, fragment, tree)
	}); err != nil || changeErr != nil {
		t.Fatalf("type over %q: %v %v", old, err, changeErr)
	}
}

// waitForSettlementOwed waits for the document's pending-settlement row.
func waitForSettlementOwed(t *testing.T, service *Service, artifactID string) {
	t.Helper()
	waitFor(t, "the document to owe a settlement", func() bool {
		owed, err := settlementPending(context.Background(), service.store.Pool, artifactID)
		return err == nil && owed
	})
}

// latestDocumentVersion is the number, markdown and authors of the document's latest version.
func latestDocumentVersion(t *testing.T, database *store.Store, artifactID string) (int, string, []model.Actor) {
	t.Helper()
	var number int
	var markdown string
	var authors []byte
	if err := database.Pool.QueryRow(context.Background(), `
		select number, coalesce(markdown, ''), authors
		from artifact_versions where artifact_id = $1 order by number desc limit 1
	`, artifactID).Scan(&number, &markdown, &authors); err != nil {
		t.Fatalf("read the document's latest version: %v", err)
	}
	var actors []model.Actor
	if err := json.Unmarshal(authors, &actors); err != nil {
		t.Fatalf("decode the latest version's authors: %v", err)
	}
	return number, markdown, actors
}

// heldStore stores a document's updates as the store does, but holds the held room's next append
// transaction - a batch of its updates, or one alone - once armed, until the test lets it go.
type heldStore struct {
	VersionedStore
	room    string
	armed   atomic.Bool
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *heldStore) let() { s.once.Do(func() { close(s.release) }) }

// hold holds the held room's next append, once armed, until the test lets it go. Both of the
// store's appends of a room's own update go through it: the flush its persistence worker runs
// for a classified update, and the bare append of one no class was recorded for.
func (s *heldStore) hold(room string) {
	if room == s.room && s.armed.CompareAndSwap(true, false) {
		close(s.entered)
		<-s.release
	}
}

func (s *heldStore) AppendRoomFlush(ctx context.Context, room string, flush *RoomFlush, through uint64, credit *UpdateCredit) (persistence.Version, error) {
	s.hold(room)
	return s.VersionedStore.AppendRoomFlush(ctx, room, flush, through, credit)
}

func (s *heldStore) AppendUpdatesWithCredit(ctx context.Context, room string, updates []BrowserUpdate) (persistence.Version, error) {
	s.hold(room)
	return s.VersionedStore.AppendUpdatesWithCredit(ctx, room, updates)
}

// burstGate pairs the channel a burstStore closes once an append reaches it with the channel a
// test closes, at most once, to let that append complete.
type burstGate struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (g *burstGate) let() { g.once.Do(func() { close(g.release) }) }

// burstStore holds a room's durable append transactions one at a time, in the order they reach the
// store, against the gates a test supplies - standing in for a burst of writes that keeps landing
// while Shutdown drains the room, including ones recorded only after its room scan. Once armed,
// each call consumes the next gate in order; a call past the gates a test supplied is not held.
// This duplicates heldStore's single-gate shape rather than nesting two heldStores
// (outer(inner(...))): heldStore's append calls through to its wrapped store inside the same call
// its gate guards, so an outer heldStore's gate would fire a second time on the first append once
// the inner one releases it and the call reaches the outer wrapper on its way through - a spurious
// second hold on the first append rather than a hold on the second.
type burstStore struct {
	VersionedStore
	room  string
	armed atomic.Bool
	mu    sync.Mutex
	gates []*burstGate
	next  int
}

// hold takes the next gate for a room append, as heldStore.hold does for its one gate.
func (s *burstStore) hold(room string) {
	if room != s.room || !s.armed.Load() {
		return
	}
	s.mu.Lock()
	var gate *burstGate
	if s.next < len(s.gates) {
		gate = s.gates[s.next]
		s.next++
	}
	s.mu.Unlock()
	if gate != nil {
		close(gate.entered)
		<-gate.release
	}
}

func (s *burstStore) AppendRoomFlush(ctx context.Context, room string, flush *RoomFlush, through uint64, credit *UpdateCredit) (persistence.Version, error) {
	s.hold(room)
	return s.VersionedStore.AppendRoomFlush(ctx, room, flush, through, credit)
}

func (s *burstStore) AppendUpdatesWithCredit(ctx context.Context, room string, updates []BrowserUpdate) (persistence.Version, error) {
	s.hold(room)
	return s.VersionedStore.AppendUpdatesWithCredit(ctx, room, updates)
}

package docs

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/persistence"

	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/testcleanup"
	"github.com/sjawhar/envoy/internal/testwait"
)

// A deploy usually finds someone with a document open, its tab connected to the room with what it
// last typed owed a settlement. Shutdown settles the room while it is still loaded and only then
// closes the editor's connection: ygo's CloseRoom evicts the room as it closes it, and a settlement
// does not load a room during shutdown, so a room closed first would leave the edit unversioned
// until someone next opened the document.
func TestShutdownSettlesARoomWithAnEditorConnectedBeforeClosingIt(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before")
	service := newShutdownTestService(t, database, nil)
	seedServiceText(t, service, artifactID, "before")
	settleCurrentGeneration(t, service, artifactID)
	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	testcleanup.Add(t, httpServer.Close)
	editor := connectPeer(t, httpServer.URL, artifactID, "alice")
	if err := editor.WaitForText(peerMarkdown, "before\n", 10*time.Second); err != nil {
		t.Fatal(err)
	}
	typeOver(t, editor, "before", "after")
	waitForSettlementOwed(t, service, artifactID)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := service.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown with an editor connected: %v", err)
	}
	select {
	case <-editor.Ended:
	case <-time.After(5 * time.Second):
		t.Fatal("the editor's connection was still open 5s after Shutdown returned")
	}
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
	t.Parallel()
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

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := service.Shutdown(ctx); err != nil {
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
// process does not close the store under it.
func TestShutdownJoinsTheSettlementItsLastBrowserLeavingStarted(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before")
	service := newShutdownTestService(t, database, nil)
	pause := pauseRepairCommits(service)
	testcleanup.Add(t, pause.let)
	seedServiceText(t, service, artifactID, "before")
	settleCurrentGeneration(t, service, artifactID)
	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	testcleanup.Add(t, httpServer.Close)
	browser := connectPeer(t, httpServer.URL, artifactID, "alice")
	if err := browser.WaitForText(peerMarkdown, "before\n", 10*time.Second); err != nil {
		t.Fatal(err)
	}
	// A block with no id arms the room's settlement, which has to stamp one.
	editLiveTree(t, service, artifactID, appendUnidentifiedBlocks(t, "added"))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := service.waitForPendingUpdates(ctx, artifactID); err != nil {
		t.Fatalf("wait for the edit to reach persistence: %v", err)
	}
	if err := service.waitForDurableAppends(ctx, artifactID); err != nil {
		t.Fatalf("wait for the edit to be stored: %v", err)
	}

	pause.armed.Store(true)
	browser.Close()
	select {
	case <-pause.held:
	case <-time.After(10 * time.Second):
		t.Fatal("the settlement the browser's leaving owed never stamped the room")
	}
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancelShutdown()
	shutdown := make(chan error, 1)
	go func() { shutdown <- service.Shutdown(shutdownCtx) }()
	// Shutdown's own settlement of the room waits on the owner row the held one holds until the
	// drain budget ends; a Shutdown still running a little after that is waiting on the held
	// settlement.
	select {
	case err := <-shutdown:
		t.Errorf("Shutdown returned (%v) while the settlement the browser's leaving started was still committing its stamp", err)
		shutdown <- err
	case <-time.After(ShutdownDrainBudget + 2*time.Second):
	}
	pause.let()
	select {
	case err := <-shutdown:
		if err != nil {
			t.Fatalf("shutdown under the browser's settlement: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Shutdown never returned once the settlement the browser's leaving started could go on")
	}
	// The stamp went with the shutdown's gate, so the document is left to resume, its edit stored.
	if owed, err := settlementPending(context.Background(), database.Pool, artifactID); err != nil || !owed {
		t.Fatalf("the document owes no settlement (%v) after a shutdown that settled nothing", err)
	}
	waitForPersistedProofText(t, database, artifactID, "before\n\nadded\n")
}

// An append a room queued before the signal can be slow to store - waiting on its document's lock
// or a slow write - and only that room's settlement can wait on it. Every other room drains, reads
// what it owes and settles on its own, so the quiet document's edit is versioned while the held
// room's append is still outstanding, and the held room alone is left to resume.
func TestShutdownSettlesEveryOtherRoomWhileOneRoomsQueuedAppendIsHeld(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	quiet := createIssueDocument(t, database, 1, "quiet")
	held := createIssueDocument(t, database, 2, "held")
	appends := &heldStore{VersionedStore: NewPgVersioned(database), room: held, entered: make(chan struct{}), release: make(chan struct{})}
	testcleanup.Add(t, appends.let)
	service := newShutdownTestService(t, database, appends)
	seedServiceText(t, service, quiet, "quiet")
	settleCurrentGeneration(t, service, quiet)
	seedServiceText(t, service, held, "held")
	settleCurrentGeneration(t, service, held)
	editLiveTree(t, service, quiet, replaceRun("quiet", "quiet, edited"))
	waitForSettlementOwed(t, service, quiet)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := service.waitForDurableAppends(ctx, quiet); err != nil {
		t.Fatalf("wait for the quiet document's edit to be stored: %v", err)
	}
	appends.armed.Store(true)
	editLiveTree(t, service, held, replaceRun("held", "held, edited"))
	select {
	case <-appends.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the held room's append never reached the store")
	}

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelShutdown()
	shutdown := make(chan error, 1)
	go func() { shutdown <- service.Shutdown(shutdownCtx) }()
	var shutdownErr error
	select {
	case shutdownErr = <-shutdown:
	case <-time.After(ShutdownDrainBudget + 5*time.Second):
		t.Fatal("Shutdown did not return once its drain budget ended")
	}
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
//
// The final version cannot prove this order: settlement can also persist unstored room updates
// (Ledger.flushThrough). Compare the append's commit with settlement's start instead.
// Do not spend the drain budget on a fixed hold: both appends and settlement need that budget.
func TestShutdownSettlesABurstOfAppendsIncludingOneRecordedAfterItsScan(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before")
	first := &burstGate{entered: make(chan struct{}), release: make(chan struct{})}
	second := &burstGate{entered: make(chan struct{}), release: make(chan struct{})}
	// afterAppendCommit runs in each append's call, between its commit and its cursor's advance,
	// and the second gate's entry tells the second append's commit from the first's: ygo's
	// persistence worker hands a room's updates to the store one call at a time, so the first has
	// committed before the second reaches its gate.
	var secondCommitted, settleStarted, settleStartedEarly atomic.Bool
	persist := NewPgVersioned(database)
	persist.afterAppendCommit = func(room string, _ persistence.Version) {
		if room != artifactID {
			return
		}
		select {
		case <-second.entered:
			secondCommitted.Store(true)
		default:
		}
	}
	appends := &burstStore{VersionedStore: persist, room: artifactID, gates: []*burstGate{first, second}}
	testcleanup.Add(t, func() {
		first.let()
		second.let()
	})
	service := newShutdownTestService(t, database, appends)
	// Each start of a settlement of the room once Shutdown has scanned it notes whether the second
	// append had committed by then; the room's settlement before the burst starts before the scan.
	service.beforeSettleStart = func(room string) {
		if room != artifactID || !service.shuttingDown(room) {
			return
		}
		settleStarted.Store(true)
		if !secondCommitted.Load() {
			settleStartedEarly.Store(true)
		}
	}
	seedServiceText(t, service, artifactID, "before")
	settleCurrentGeneration(t, service, artifactID)
	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	testcleanup.Add(t, httpServer.Close)
	editor := connectPeer(t, httpServer.URL, artifactID, "alice")
	if err := editor.WaitForText(peerMarkdown, "before\n", 10*time.Second); err != nil {
		t.Fatal(err)
	}

	appends.armed.Store(true)
	editLiveTree(t, service, artifactID, replaceRun("before", "first"))
	select {
	case <-first.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the first append never reached the store")
	}
	if err := editor.WaitForText(peerMarkdown, "first\n", 10*time.Second); err != nil {
		t.Fatal(err)
	}

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancelShutdown()
	shutdown := make(chan error, 1)
	go func() { shutdown <- service.Shutdown(shutdownCtx) }()
	waitFor(t, 10*time.Second, "Shutdown to have scanned the room", func() bool {
		return service.shuttingDown(artifactID)
	})

	// Only now, deterministically after the scan, does a second write land - the already-connected
	// editor's own keystroke, which Shutdown's inject refusal never reaches.
	typeOver(t, editor, "first", "second")
	waitFor(t, 10*time.Second, "both appends to be recorded as outstanding", func() bool {
		return service.room(artifactID).durableAppends.Load() >= 2
	})
	first.let()
	select {
	case <-second.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the second append never reached the store once the first let go")
	}
	second.let()

	var shutdownErr error
	select {
	case shutdownErr = <-shutdown:
	case <-time.After(10 * time.Second):
		t.Fatal("Shutdown did not return once both appends landed")
	}
	if shutdownErr != nil {
		t.Errorf("Shutdown returned %v, want nil: the burst should settle within the per-room drain budget", shutdownErr)
	}
	switch {
	case !settleStarted.Load():
		t.Errorf("Shutdown started no settlement of the room")
	case settleStartedEarly.Load():
		t.Errorf("Shutdown's settlement of the room started before the append recorded after its scan committed: its drain did not wait for that append")
	}
	if number, markdown, _ := latestDocumentVersion(t, database, artifactID); number != 2 || markdown != "second\n" {
		t.Errorf("the document's latest version is %d holding %q, want version 2 holding %q", number, markdown, "second\n")
	}
}

// A room whose settlement is still rendering when Shutdown's own drain budget (drainCtx) ends
// does not make Shutdown return the settlements-unconfirmed error: Shutdown's wait for its
// per-room workers (waitGroup(ctx, &workers)) runs under the whole outer deadline, not the drain
// budget alone, because a render or commit already under way when the budget ends does not stop
// for it. A commit the expired budget then refuses leaves the document to resume from its
// pending-settlement row, which is a documented, non-error outcome (LEGION-501).
//
// What the row needs is that order of events, not a duration. It once wrote the order as a
// render sleeping ShutdownDrainBudget + 3.4 s inside a Shutdown deadline of
// ShutdownDrainBudget + 5 s: 1.6 s of wall clock for the service's start, the settlement's work
// and its commit, which a loaded machine spends, and the row then failed on main for the
// machine rather than for the product (dispatch://INFRA-3904). Now the render is held until this
// test lets it go, Shutdown runs under the test binary's own deadline (testwait.Context), and
// each wait ends on its fact.
//
// The hold is ShutdownDrainBudget measured from the moment the render entered, which is after
// Shutdown made drainCtx, so the budget has certainly ended when the render is let go, whatever
// the machine's speed; a slower machine only holds it longer. The row proves that rather than
// trusting it: a settlement whose commit the budget refused leaves the document owing its
// settlement, so the check below fails by name if the render did not outlast the budget.
//
// The held render sits inside the settlement's own transaction, on a pooled connection, and the
// database's cleanup waits for every connection to come back (Pool.Close). So the hold is let go
// on every path, a failure included: its release is registered as a cleanup the moment it is
// made, which runs before the database's own. And whether Shutdown answered while the render was
// still held is read in Shutdown's goroutine at the moment it answers, not sampled from the test's
// side some time later.
func TestShutdownReturnsNilForARenderStillRunningPastItsDrainBudget(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before")
	service := newShutdownTestService(t, database, nil)
	seedServiceText(t, service, artifactID, "before")
	settleCurrentGeneration(t, service, artifactID)
	editLiveTree(t, service, artifactID, replaceRun("before", "after"))
	waitForSettlementOwed(t, service, artifactID)
	waitForLandedAppends(t, service, artifactID)

	entered := make(chan struct{})
	release := make(chan struct{})
	var enteredOnce, releaseOnce sync.Once
	letGo := func() { releaseOnce.Do(func() { close(release) }) }
	testcleanup.Add(t, letGo)
	service.afterSettleReconcile = func(room string) {
		if room != artifactID {
			return
		}
		enteredOnce.Do(func() { close(entered) })
		<-release
	}

	type answer struct {
		err error
		// held is whether the render was still held when Shutdown answered.
		held bool
	}
	shutdown := make(chan answer, 1)
	shutdownCtx := testwait.Context(t)
	go func() {
		err := service.Shutdown(shutdownCtx)
		select {
		case <-release:
			shutdown <- answer{err: err}
		default:
			shutdown <- answer{err: err, held: true}
		}
	}()
	testwait.Receive(t, "the settlement to reach its reconcile hook", entered)
	// Real time the product itself spends: the render has to outlast the budget, and nothing but
	// the budget says when that is. It is a floor, not a bound on the machine.
	time.Sleep(ShutdownDrainBudget)
	letGo()

	got := testwait.Receive(t, "Shutdown to answer once the render finished", shutdown)
	if got.held {
		t.Fatalf("Shutdown answered %v while the render was still held: its wait for the per-room workers ran under the drain budget, not its whole deadline", got.err)
	}
	if got.err != nil {
		t.Errorf("Shutdown returned %v, want nil: a render already running when the budget ends should still let its worker return in time", got.err)
	}
	// The control on the hold: a settlement the expired budget refused at its commit leaves the
	// document owing one. A render let go before the budget ended would have committed, and this
	// row would then prove nothing about a budget that ends under a render.
	owed, err := settlementPending(context.Background(), database.Pool, artifactID)
	if err != nil {
		t.Fatalf("read whether the document still owes a settlement: %v", err)
	}
	if !owed {
		t.Fatal("the document owes no settlement after the shutdown: its render did not outlast the drain budget, so this row did not hold one past it")
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
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
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
	service := New(Deps{
		Store: database, Persistence: appends, Events: events.NewBroker(),
		Identity: identity.HeaderIdentity{Header: "X-Dispatch-User", People: store.NewPgPeopleStore(database.Pool, testSigningKey, nil)},
		Settle:   time.Hour,
	})
	// A test that already called Shutdown runs this one a second time; it logs its own
	// "left to resume" line but does no second settlement attempt (settleRoomWithin's
	// s.stopping guard returns it almost instantly), so it is a harmless no-op, not a race.
	testcleanup.Add(t, func() { _ = service.Shutdown(context.Background()) })
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
	waitFor(t, 10*time.Second, "the document to owe a settlement", func() bool {
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

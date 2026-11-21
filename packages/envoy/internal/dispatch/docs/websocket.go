package docs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync/atomic"
	"time"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/persistence"
	"github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// connectionState is one document socket as its admission (ServeHTTP) found it and ygo's
// authorization of the same request (authorize) registered it: the caller, resolved once
// (requestActor), and the admission's one read of the document's issue, which authorize takes
// rather than resolving or reading either again; then the room, the connection id the service gave
// the browser, and whether authorize registered it, which tells the socket's end whether its place
// under the connection cap is the connection's count to forget (removeConnection) or a reservation
// to give back (releaseConnection). What else a socket's admission reads off its request goes here
// too.
type connectionState struct {
	room  string
	id    uint64
	actor model.Actor
	issue issueRead
	added bool
	// admission is this socket's schema admission: ServeHTTP makes it with the document's
	// documentSince and the cancel of the socket's own request context, authorize decides and
	// records it, the answer to the socket's token reads it (authorizeSchemaVersion), a re-check
	// revokes it (RunSchemaAdmission), and ServeHTTP forgets it once the socket has ended.
	admission *schemaAdmission
	// clients is the client ids this socket may write and announce under, which its admission
	// decided (admitClients, client_ids_admission.go) and which the peer update hook and the
	// awareness filter judge the socket's frames by.
	clients *socketClients
}

type connectionContextKey struct{}

type ownerVerifiedContextKey struct{}

// ownerVerifiedToken is installed only on the server's own calls, by a caller that already
// knows the document's owner state or has no use for it: a settlement, which holds the owner's
// lock in its transaction, and its warm-up load, which applies nothing; the publish of a
// committed live write, whose transaction held that lock until it committed; the block-id
// backfill, which is a command run against a database nobody is serving from; or one of a
// comment's anchor operations under WithCommentMarks, whose caller holds the owner's lock and
// decided under it that the comment is written. Their injections skip the issue read in
// allowInject - for the settlement that read would be a second pooled connection taken while
// its transaction is open (store.ErrNestedAcquire), for its warm-up a refusal its closed issue
// does not make (a closed issue's document settles as an open one's does), for the publish a
// read other writers' connections can starve (see publishLiveUpdate), for the backfill a
// question already answered, and for a comment's anchor a refusal its closed issue does not make.
type ownerVerifiedToken struct{ _ byte }

func withOwnerVerified(ctx context.Context) context.Context {
	return context.WithValue(ctx, ownerVerifiedContextKey{}, &ownerVerifiedToken{})
}

func isOwnerVerified(ctx context.Context) bool {
	_, ok := ctx.Value(ownerVerifiedContextKey{}).(*ownerVerifiedToken)
	return ok
}

type commentMarksContextKey struct{}

// WithCommentMarks marks ctx as a comment's writes to the document its anchor is in, made by a
// caller holding the document owner's lock in its joined transaction that decided there
// the comment is written. A closed issue still takes a comment, so its document still takes
// that comment's anchor: the comment mark MarkQuote places and VerifyMark reads, the version
// SnapshotVersion pins the anchor to, the margin record ProjectMark writes, and the
// SuggestionKind read of the thread root a reply projects. Each of those, and only those, runs
// owner-verified under this context (commentMarkOperation). A suggestion's mark, a content edit,
// a named version or a rebuild made under the same context is still refused on a closed issue.
func WithCommentMarks(ctx context.Context) context.Context {
	return context.WithValue(ctx, commentMarksContextKey{}, true)
}

// commentMarkOperation is ctx for one of the comment anchor operations WithCommentMarks names:
// owner-verified when the caller marked ctx, unchanged otherwise.
func commentMarkOperation(ctx context.Context) context.Context {
	if commentMarks(ctx) {
		return withOwnerVerified(ctx)
	}
	return ctx
}

// commentMarks is whether the caller marked ctx as a comment's writes to a document
// (WithCommentMarks). Only commentMarkOperation reads it: what every other caller asks is
// commentMarkAnchor, which is this inside the operation that marked itself owner-verified.
func commentMarks(ctx context.Context) bool {
	marked, _ := ctx.Value(commentMarksContextKey{}).(bool)
	return marked
}

// commentMarkAnchor is whether ctx is one of the comment anchor operations WithCommentMarks
// names, inside the operation that made it owner-verified (commentMarkOperation): the reads and
// writes a closed issue's document takes for a comment, and no other caller's. It is the one
// test of that, which the three decisions a closed issue turns share - the fork a comment's mark
// is written on (closedIssueCommentMark), the tree its mark_id is verified against and the
// version its anchor is pinned to (closedIssueCommentAnchor).
func commentMarkAnchor(ctx context.Context) bool {
	return commentMarks(ctx) && isOwnerVerified(ctx)
}

// closedIssueCommentAnchor is whether ctx is a comment's anchor operation (commentMarkAnchor) on
// a document whose issue is closed, read from q - the caller's transaction where it is inside
// one (queryFrom). It answers false for every other caller without reading anything, so no
// version and no mark read outside a comment's anchor pays for it.
//
// A closed issue's room holds updates the store will never take: on a process that has not heard
// the close its browsers are still writable, and the gate refuses a browser update the room took
// more than closeMargin after the close (gateAppend, appendBrowser). So the comment's anchor
// reads and writes the stored document alone - the fork its mark is written on (readFork), the
// tree its mark_id is verified against (VerifyMark) and the version that anchor is pinned to
// (captureLiveTextAndAuthors) - and all three see the same document. A mark only the room holds
// is then no anchor: VerifyMark answers ErrAnchorMissing, the API answers 409 ANCHOR_MISSING,
// and the browser's margin writes the mark again and sends the comment anew (margin-context.tsx,
// reanchor), by quote once the refused append has closed the room for its issue and its browsers
// have reconnected read-only (ProofDocument.tsx, the quote-comment action).
func (s *Service) closedIssueCommentAnchor(ctx context.Context, q Queryer, artifactID string) (bool, error) {
	if !commentMarkAnchor(ctx) {
		return false, nil
	}
	open, err := s.issueOpen(ctx, q, artifactID)
	if err != nil {
		return false, err
	}
	return !open, nil
}

type shutdownLoadContextKey struct{}

// withShutdownLoad marks the one injection allowInject lets through into a room Shutdown has
// begun closing: the empty warm-up with which Shutdown's own settlement loads a document whose
// settlement this process had armed with no room resident (a cold write's, publishLiveWrite), as
// the armed timer would have loaded it had it fired. It loads the room and writes nothing; a write
// that settlement then has to make into the room carries no mark and is refused like any other.
func withShutdownLoad(ctx context.Context) context.Context {
	return context.WithValue(ctx, shutdownLoadContextKey{}, true)
}

func isShutdownLoad(ctx context.Context) bool {
	load, _ := ctx.Value(shutdownLoadContextKey{}).(bool)
	return load
}

type settlementLoadContextKey struct{}

// withSettlementLoad marks the settlement a browser's departure runs (settleLastPeer) and the
// Apply that retires the room its warm-up loaded (retireSettlementRoom). A departure's
// settlement loads a room only when the document's room has left ygo's map before it: a close
// took the room out from under the departing browser, and the load would put back a room no one
// asked for. A timer's settlement carries no mark, and the room it loads idles out as any room
// does. ygo runs a room's load hook only on the goroutine whose call created the room, and hands
// it that call's context (sjawhar/ygo v1.55.1-sami.2, provider/websocket/server.go:1755-1769,
// 1903-1921): a load whose context carries the mark was made by such a settlement's warm-up or
// retire, not by a browser or a writer, and the room it publishes is the settlement's
// (roomState.warmed, recorded by onLoadDocument). A load the injection only waited for, made by
// another caller, never sees the mark.
func withSettlementLoad(ctx context.Context) context.Context {
	return context.WithValue(ctx, settlementLoadContextKey{}, true)
}

func isSettlementLoad(ctx context.Context) bool {
	load, _ := ctx.Value(settlementLoadContextKey{}).(bool)
	return load
}

// servicePersistenceAdapter observes ygo's otherwise asynchronous persistence
// callbacks. A failed update evicts its room so the next access reloads durable
// state instead of retaining unsaved live state.
type servicePersistenceAdapter struct {
	store   VersionedStore
	service *Service
}

// LoadDoc reads the durable state a room loads and hands ygo none of it: the state goes on the
// load's record (roomLoad.read), and the room's own load hook decodes it (bootstrapLoadedState,
// whose comment states how), because ygo's own apply runs the format cleanup a remote apply runs
// and the room's stored history already holds every cleanup the room made. A preload
// (takePreload), the state a socket's admission check read and the room's load takes in place of
// reading the store again, goes on the record as a read would. The record also takes that state's
// one decode of the load's own (loadedState, decodeStoredCopy), the preload's state and the read's
// alike: its scans are the room's scan total at its load (roomScans), and its copy the room's
// replica (keepReplica). So the stored state is decoded twice on a room's load, that copy and the
// load hook's own decode of the same bytes; those decodes, and a fold before them, run under the
// load's one grant of the pod's load concurrency (loadedState.slot).
//
// The record also carries the history's size and what a socket's admission made of that history
// when the load takes that socket's preload: the tree it read from it and the rendering it made of
// that tree (roomLoad.read, takeLoaded, admissionRendering).
//
// It records the load before it reads, and what it read after: the room's load (onLoadDocument)
// takes the record next on the same goroutine (ygo's loadRoom) to start the instance's roomHold
// and its cursor, and a version this process appends between the two reaches that instance through
// the record (roomLoad) rather than no instance at all. The read itself takes no advisory lock - a
// durable writer holds that one across its whole transaction, and a load that waited for it would
// wedge a write that forks the room it loads - so the record, not the lock, is what keeps a room
// from being born behind the head.
//
// Nothing of ygo's own runs between this read and the hook it calls next: there is no decode of
// what this returns (sjawhar/ygo v1.55.1-sami.7, provider/websocket/server.go:2019-2024), and the
// only way a load fails with nothing of this service still to run on its goroutine is a panic out
// of this read. Every way the read ends short is therefore failed here, on the load's own
// goroutine and before ygo closes the room's barrier: the store's refusal, a stored history whose
// header declares more items than one update may hold (loadDecoded, loadableHistory), and a panic
// out of any of it, recovered here as the hook recovers its own (bootstrapLoadedState,
// foldDocument). A history ygo refuses the bytes of is not one of them: the room's own decode
// fails the room for it (onLoadDocument). ygo
// recovers such a panic too and hands every waiter the load's error (server.go:2010-2029), but it
// does so with the record this load began still standing under the room's name and the room
// healthy, which left the failure to whichever caller's Apply met it - a caller that cannot tell
// the load it waited on from the load that began after it, since ygo removes the room before any
// waiter wakes (server.go:2031-2040). Recovered here, a load's failure is the load's own
// (sjawhar/legion#2519).
func (a *servicePersistenceAdapter) LoadDoc(room string) (_ []byte, err error) {
	load := a.service.beginRoomLoad(room)
	slot := newLoadSlot()
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("panic: %v", recovered)
		}
		if err != nil {
			// A failed load leaves the slot to no one: the record takes the state, the copy and
			// the slot together, and only where this returns no error (read), so what this load
			// waited for is given back here.
			slot.release()
			a.service.failRoomLoad(room, load, err)
		}
	}()
	if loaded, rendered, ok := a.service.takePreload(room); ok {
		// The admission that read this state decoded it for its own schema check and kept no
		// copy (loadTree), so the room's copy is decoded here, under the grant this load holds
		// for it, weighed on the state the admission read: its bytes and the structs its headers
		// declare (storedCopyWeight), read before this decode builds any of them. A state ygo
		// refuses the bytes of never reaches a preload - the admission's own decode refused it
		// first - and is left to the room's decode either way, as the read's own path leaves it
		// (loadDecoded).
		_ = slot.take(context.Background(), storedCopyWeight(loaded.Update))
		decoded, _ := decodeStoredCopy(loaded.Update, a.service.maxPendingItems)
		load.read(loaded.Version, loaded.Update, decoded, slot, rendered)
		return nil, nil
	}
	result, decoded, err := a.loadDecoded(room, slot)
	if err != nil {
		return nil, err
	}
	load.read(result.Version, result.Update, decoded, slot, admissionRendering{})
	return nil, nil
}

func (a *servicePersistenceAdapter) StoreUpdate(room string, update []byte) error {
	return a.StoreUpdateContext(context.Background(), room, update)
}

// StoreUpdateContext is StoreUpdateStored for a caller that reads only its error.
func (a *servicePersistenceAdapter) StoreUpdateContext(ctx context.Context, room string, update []byte) error {
	_, err := a.StoreUpdateStored(ctx, room, update)
	return err
}

// StoreUpdateStored stores a room's update, which ygo's persistence worker hands on one call per
// update, in order, and says whether the update is stored once it returns
// (websocket.PersistenceAdapterStored): the room answers the browser whose frame made the commit
// only then, applied where it is stored and not applied where it is not (Server.AnswerOnceStored,
// New). It appends, in one transaction under the document's advisory lock, every update of the
// same room instance its update observer recorded before this one and no writer has stored yet,
// this one, and the run of updates the room recorded after it that its batch takes
// (roomBatchThrough), in the room's commit order, each with the in-flight credit and the class its
// observer recorded (AppendRoomFlush, flushRun). So the order ygo's persistence hands updates on
// in never decides the order they are stored in, and an update another writer stored first is
// skipped. It returns nil only once the update is durable, by its own transaction or by one that
// stored it first, or once it is dropped, so ygo's flush still means every update it held is
// stored or dropped.
//
// The call goes at once. While it is in flight the room's later updates queue behind it, and the
// call ygo makes next takes them all (group commit, room_appends.go).
//
// Each append carries what the observer recorded with its updates: the room instance they were
// made in (documentUpdateClass.cursor) and when the room took each (documentUpdateClass.observed),
// which the store passes to the gate as a browser's append (BrowserUpdate, appendBrowser). So the
// gate admits them from a room behind the head with its history intact - they land at the head,
// and the instance's cursor folds them in once catch-up fills the gap - and refuses them only when
// the room's stored history was replaced under it, or when the document's issue is closed and the
// room took the update later than closeMargin after the close. A history a compaction folded past
// the room's cursor is no replacement for the run when it still holds every item the run builds
// on (storedHistoryHolds): it is stored, and the room, which cannot catch up to that history, is
// closed and reloaded at the head once it is (closeRoomOnReplacedHistory): this append asks for
// that itself, so it needs no room-sync lane and waits on no later notification. An update the
// room took before the close and whose append reaches the gate after it - queued behind the
// room's earlier appends, waiting on the document's lock behind a settlement or a compaction, or
// handed here by the close of the room itself (setRoomIssueOpen) - is stored, a compaction's fold
// of the history meanwhile included: a room closing for its issue takes in no catch-up while its
// close waits for this append (holdOpen), and that close is the reload a folded history needs.
// The document's next settlement saves it as it saves any stored update, whether or not the issue
// is closed (settleRoomWithin).
//
// A replaced history fails the room, as any failed append does: the room reloads from the store,
// which is the close-and-reload a replaced history needs (plan D3.3), and the browser, answered
// not applied, resends the update to the reloaded room. A refused update on a closed issue is
// dropped and closes the room for its issue (refuseClosedIssueAppend), so its browsers reconnect
// read-only. Either is decided under the document's advisory lock, so it is never this process's
// own write in flight, and needs no room-sync lane to run.
//
// An update that never reaches the store - its room failed, or a live write, a settlement or a
// catch-up already made it durable - lands its credit here instead: the room drops it with the
// update, which the browser resends. Whichever way the append ends, the update leaves its
// instance's unstored updates once it has. One a writer made durable is stored; a dropped one is
// not.
//
// It runs on the room's persistence worker, the one reader of the room's queue of updates to
// store, on which a heavy apply holding its grant of the pod's load concurrency through its commit
// waits while the queue is full (weighPeerUpdate). So nothing it decodes waits for that pool: ctx
// is marked once, here, to take only bytes free at once (withFreeLoadOnly), and the decodes its
// gate makes of a history replaced under the room take their grants under it
// (storedHistoryHolds); its decodes of the update itself take only free bytes as well
// (holdFreeLoad).
func (a *servicePersistenceAdapter) StoreUpdateStored(ctx context.Context, room string, update []byte) (stored bool, err error) {
	ctx = withFreeLoadOnly(ctx)
	if a.service.roomFailed(room) {
		// The failed room's update is dropped and its browser resends it to the room that
		// replaces this one, but its class is taken all the same, so the work the record counts
		// for it ends here (dropUpdate).
		a.dropUpdate(room, update)
		return false, nil
	}
	class, counted, found := a.service.consumeUpdateClass(room, update)
	if found {
		defer a.service.finishUpdateClass(room, counted, class)
	}
	defer class.instance.unlistUnstored(class)
	defer class.credit.landed()
	// The update's suppression slot is consumed whatever its class says, since slots are taken in
	// order. The class decides when the observer recorded one: a published write's and a
	// repair's are queued stored, and a browser update whose bytes match a repair's - a deletion
	// leaves an update that carries no struct, only its own deletes, so a browser's deletion of
	// the items a repair deleted leaves the repair's bytes - takes that repair's slot while its own
	// class is not stored, and is stored all the same; the repair's own update then finds its
	// class stored.
	suppressed := a.service.consumeSuppressedPersistence(room, update)
	if writer := class.stored.Load() || (suppressed && !found); writer || a.service.roomFailed(room) {
		if writer && found {
			a.service.resyncLanded(room, class.instance, class.seq)
		}
		return writer, nil
	}
	a.service.hooks.awaitPersistenceHold(ctx, room)
	if fail := a.service.beforeRoomAppend; fail != nil {
		if err := fail(room); err != nil {
			a.service.failRoom(room, err)
			return false, err
		}
	}
	if !found {
		// An update no class was recorded for is stored on its own: the room's queue holds
		// nothing to flush it with, and the gate reads the cursor marked above.
		updates := []BrowserUpdate{{
			Update:         update,
			ContentChanged: class.contentChanged,
			Credit:         class.credit,
			Observed:       class.observed,
		}}
		if _, err := a.store.AppendUpdatesWithCredit(withRoomCursor(ctx, class.cursor), room, updates); err != nil {
			if errors.Is(err, ErrIssueClosed) {
				a.service.refuseClosedIssueAppend(room, class.instance)
				return false, nil
			}
			a.service.failRoom(room, err)
			return false, err
		}
		return true, nil
	}
	// The call's own update must read back into a fresh copy before its batch is stored, as an
	// append of updates the store is handed checks (AppendUpdatesWithCredit): one the room took
	// but a copy cannot decode fails the room, which reloads the document from the store. Only
	// this update is read back here. The batch's other updates are stored as the room applied
	// them, decoded by the room when it took them and not again, as a joined transaction's flush
	// of the room stores them (AppendRoomUpdatesTx); a later call of an update the batch stored
	// finds its class stored and decodes nothing.
	//
	// The read-back is a whole decode of the update, as heavy as the apply that made it, and that
	// apply's grant was given back when the hook returned (weighPeerUpdate). So it takes what its
	// copy costs where the pool has that much free at once, and runs beside the pool's holders
	// where it has not (holdFreeLoad): it may not wait, since this worker is the one reader of the
	// room's queue of updates to store, and a heavy apply holding its grant through its commit
	// waits on that queue to hand the commit over while the queue is full.
	release := holdFreeLoad(storedCopyWeight(update))
	err = crdt.ApplyUpdateV1(newDocumentCopy(), update, nil)
	release()
	if err != nil {
		a.service.failRoom(room, err)
		return false, err
	}
	flush := &RoomFlush{service: a.service, room: room, cursor: class.cursor}
	through := a.service.roomBatchThrough(room, class)
	if _, err := a.store.AppendRoomFlush(ctx, room, flush, through, class.credit); err != nil {
		if errors.Is(err, ErrIssueClosed) {
			a.service.refuseClosedIssueAppend(room, class.instance)
			return false, nil
		}
		a.service.failRoom(room, err)
		return false, err
	}
	// The room's commits are stored through the batch's position, so a browser whose parked frames
	// a commit up to there landed is told now (resyncLanded).
	a.service.resyncLanded(room, class.instance, through)
	if flush.storedContentChange() {
		a.service.scheduleSettleAfterAppend(room)
	}
	if class.cursor.takeReplaced() {
		a.service.closeRoomOnReplacedHistory(room, class.instance)
	}
	return true, nil
}

// Compact is ygo's room-close compaction: its persistence worker calls it as it exits, the server's
// shutdown included, and it runs the tail rule (compactRoom) inline on that worker. roomServer
// keeps that exit from racing a repair, but intentionally does not gate a published live write: it
// is already durable, and its room can still retire under publishLiveUpdate's Apply. Compaction is
// housekeeping, so the worker leaves a room whose lock another holder has to a later compaction
// rather than waiting behind that holder. A failed room's eviction is the exception: it waits for
// the lock and compacts before recovery, and with nothing to fold it still waits for the lock
// (passRoomLock), so a repair written into the failed room commits before the room reloads.
//
// It runs on the room's persistence worker after the worker has retired from the room's queue of
// updates to store, and a commit made once the worker has retired waits for the worker to exit,
// this compaction included: ygo's persistence observer hands it to persistStranded, which waits
// for the worker's persistDone. At ygo's shutdown a browser's socket can still commit while its
// room's worker retires, and a heavy apply holds its grant of the pod's load concurrency through
// its commit (weighPeerUpdate), so a wait for that pool here would hold that apply and this
// worker against each other. So ctx is marked to take only bytes free at once
// (withFreeLoadOnly): the compaction's fold and read-back (foldRead, fold) and a parked fold's
// check of the stored versions (disagreeingVersion) take what is free at once and otherwise
// decode beside the pool's holders.
func (a *servicePersistenceAdapter) Compact(ctx context.Context, room string) error {
	ctx = withFreeLoadOnly(ctx)
	// A failed room's eviction waits for the document's lock, so a repair written into it commits
	// before the room reloads; the hook counts those waiting compactions.
	failed := a.service.roomFailed(room)
	if failed {
		a.service.hooks.countLockWaitingCompaction(room)
	}
	_, err := a.store.compactRoom(ctx, room, tailRule, failed)
	return err
}

// failRoomLoad is one room load's failure: the room fails, so the next access to it reloads the
// document rather than meeting the half-loaded one (failRoom), and the record the load began goes
// with it. No instance will take that record (keepInstance), and nothing else takes it out: it is
// the document the load never made that releases it (roomLoads).
//
// The record goes first, and what it carries goes with it: the stored state the load read and
// the copy it decoded, which no document of this process will ever hold, and the grant of the
// copy-heap bound that covered them, given back here as a replaced record's is (loadedState,
// beginRoomLoad) - the room's load, which refuses the load after it has taken all three, leaves
// the record empty and gives the grant back itself (takeLoadedState, onLoadDocument) - and the
// versions this process appended while the load ran, which the room that loads next reads from
// the store instead (took). One other holder reads that record while a room loads: a cold
// write's shadow names the load when its commit found the room still loading, and measures the
// instance that load leaves behind (installShadowLocked, roomShadow.cursorLocked). Dropping the
// record before the room fails is what decides that case - a write committing in between
// installs no shadow at all rather than one naming a load that will leave no instance, and a
// shadow installed before that is freed by the failure on the next line, since a failed room
// measures no class of its own again (failRoomLocked, handOverFailedRoomLocked). So no shadow is
// left waiting on an instance this load will never leave (sjawhar/legion#2519).
func (s *Service) failRoomLoad(room string, load *roomLoad, cause error) {
	if s.roomLoads.CompareAndDelete(room, load) {
		load.take().slot.release()
	}
	s.failRoom(room, cause)
}

// validateUpdate decodes an update about to replace a stored history, as the copies of a room's
// stored state decode it: into a document with maxPending parked items, the caller's own pending
// queue (PgVersioned.maxPendingItems for the store's check of a rebuilt seed; see maxUpdateItems
// for the rule it follows), which is the cap those copies decode under (decodeStoredCopy), so a
// seed a room would serve is never judged one that cannot load. The room's own document parks no
// more than maxParkedStructs (bootstrapLoadedState), and a rebuild's seed is the state of a
// document it built from its text (rebuiltSeed), which parks nothing. A rebuild runs it on its
// seed alone (RebuildTx); the history it replaces is judged by the decode the rebuild's source
// already made of it (rebuildSource), and a room's load by its own.
//
// It takes nothing of the pod's load concurrency itself: each caller covers the decode, the
// rebuild on the grant it took for its seed (RebuildTx) and an append of the updates it is handed
// on one that never waits (AppendUpdatesWithCredit, holdFreeLoad).
func validateUpdate(update []byte, maxPending int) error {
	if len(update) == 0 {
		return nil
	}
	return decodeStoredState(newCappedDocumentCopy(maxPending), update)
}

// decodeStoredState applies state, a stored history, one of its rows or the state a room or cold
// read loads, to doc, and is the one way the service decodes stored state, so every refusal of it
// reads the same. ygo's apply names a failure as ErrInvalidUpdate followed by the decoder's reason
// ("crdt: invalid update: encoding: unexpected end of input").
func decodeStoredState(doc *crdt.Doc, state []byte) error {
	return crdt.ApplyUpdateV1(doc, state, nil)
}

// bootstrapLoadedState decodes state, what room's load read (takeLoadedState), into doc, the
// room's document. It is the decode the room's load makes of its own: ygo is handed no bytes
// (servicePersistenceAdapter.LoadDoc), so it decodes under the pending caps ygo set on doc before
// its load hook ran, the server's MaxPendingItems and MaxPendingDeleteRanges, which New sets to
// maxParkedStructs and maxParkedDeleteRanges, and through decodeStoredState, so a refusal reads
// as every other decode of stored state does: a state that would park past either fails the
// room's load, and every read of the document answers it as one that cannot load, judged on its
// copy against the same two caps (parkedPastRoomCaps). The load decoded the same bytes once more,
// into the copy the room's replica takes (decodeStoredCopy), under the service's own cap
// (Service.maxPendingItems; see maxUpdateItems), which is no lower, and with the same cleanup
// off, so a room whose decode stands holds item for item what that copy holds. An empty state -
// an empty history - decodes nothing.
//
// It decodes with ygo's format cleanup off (crdt.Doc.SetFormatCleanup), which would follow an
// apply that touches formatted text with a commit of the room's own: that commit reaches no
// persistence (ygo registers it after the load), and the stored history already holds every
// cleanup the room made (newCappedDocumentCopy), so the reloaded room would hold a state the room
// before it never held. The cleanup is set back on once the apply has returned, before any peer or
// write reaches the room, so the room cleans up after a browser's update as Yjs does; the setter
// does not cancel a cleanup an earlier commit armed, so it is off before the apply starts, and it
// is never called inside a transaction, which holds the document's lock. The apply runs inside
// ygo's OnLoadDocument hook, which logs a panic and goes on as if the hook succeeded, so a panic
// is returned as the decode's error, which fails the room as a refusal does.
func (s *Service) bootstrapLoadedState(room string, doc *crdt.Doc, state []byte) (err error) {
	if len(state) == 0 {
		return nil
	}
	if s.bootstrapping != nil {
		s.bootstrapping(room, state)
	}
	doc.SetFormatCleanup(false)
	defer doc.SetFormatCleanup(true)
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("decode room %q: panic: %v", room, recovered)
		}
	}()
	return decodeStoredState(doc, state)
}

// loadableHistory is the refusal of a stored history whose header declares more items than one
// document update may hold (itemCapRefusal), which ygo refuses before it integrates any of it, so
// the failure names the room before the load decodes anything (loadDecoded). It decodes nothing:
// the decode that follows it is the copy the room's replica takes (decodeStoredCopy), and the
// room's own load decodes the same bytes into the room's document, in the hook ygo calls as soon
// as this read returns (onLoadDocument, bootstrapLoadedState), so a history that does not decode
// fails the room at one of those rather than at a decode made to judge it. Decoding them here as
// well cost a third decode of every room load - on a history one client wrote, which is every
// document the server seeded, 65 ms and 36 MB of each cold settlement of an 8,000-item document
// (sjawhar/legion#2519), and on a history of many writers, the costliest decode there is, all of
// it again. A history of several clients declares no single count (updateItems), so those decodes
// alone judge it.
func loadableHistory(update []byte) error {
	if items, named := updateItems(update); named && items > maxUpdateItems {
		return itemCapRefusal(items, maxUpdateItems)
	}
	return nil
}

// parkedPastCapLoadLine is the line a room's load logs when the room's own decode refuses its
// stored history for parking past one of the room's caps (onLoadDocument), and
// parkedPastCapReadLine the line a read logs when it answers such a history as one that cannot
// load (foldDocument). Each names the document, the cap, its figure and what the history parks
// (parkedPastCap.attrs), and each message is fixed, so an operator finds every such document by
// it.
const (
	parkedPastCapLoadLine = "dispatch: a document's room did not load: its stored history parks past the room's cap"
	parkedPastCapReadLine = "dispatch: a read answered a document unloadable: its stored history parks past a room's cap"
)

// The kinds of what a history parks that a parkedPastCap names (parkedPastCap.parked), each held
// to a cap of its own.
const (
	parkedKindStructs      = "structs"
	parkedKindDeleteRanges = "delete ranges"
)

// parkedPastCap is a stored history that parks more than a room's document may: parks of what
// parked names, structs or delete ranges (parkedKindStructs, parkedKindDeleteRanges), past most,
// the room's cap for them (Server.MaxPendingItems, maxParkedStructs;
// Server.MaxPendingDeleteRanges, maxParkedDeleteRanges). The room's own decode of such a history
// is refused with crdt.ErrPendingCap (bootstrapLoadedState), and so is this.
type parkedPastCap struct {
	parked string
	most   int
	parks  int
}

func (p *parkedPastCap) Error() string {
	return fmt.Sprintf("the stored history parks %d %s, past the %d a room's document parks", p.parks, p.parked, p.most)
}

// Unwrap makes it crdt.ErrPendingCap, the room's own refusal of the same history.
func (p *parkedPastCap) Unwrap() error { return crdt.ErrPendingCap }

// attrs are the log line's fields for room's history: the document, the cap and its figure, and
// what the history parks of it.
func (p *parkedPastCap) attrs(room string) []any {
	return []any{"room", room, "cap", "parked " + p.parked, "most", p.most, "parks", p.parks}
}

// parkedPastRoomCaps is the one judgement of whether a room's load takes a stored history its
// decode would park in: doc is that history decoded under the service's own cap
// (Service.maxPendingItems, PgVersioned.maxPendingItems), which parks what the history parks, and
// what it parks is held against the caps ygo sets on each room's document before the room's own
// decode (Server.MaxPendingItems and Server.MaxPendingDeleteRanges, which New sets to
// maxParkedStructs and maxParkedDeleteRanges). It answers the first cap the history parks past,
// the structs' before the delete ranges', as ygo's check of an update weighs its structs before
// its delete set, and nil for a history the room's load takes, a doc of nil included. It reads
// the copy's pending queue (crdt.Doc.PendingStats) and decodes nothing.
//
// A history whose decode parks past a room's cap is one only an earlier build can have stored:
// a room stores nothing it parks (leaveParkedOut). Every read that judges a history loadable
// takes this judgement after its decode - a socket's admission (loadTree), a cold read
// (foldDocument), a cold write's fork (coldFork) and a rebuild's source (rebuildSource) - so
// each answers such a history as it answers one that does not decode, ErrDocumentUnloadable,
// and the rebuild takes it, from its own text. Read under the service's cap alone, the history
// read as the document while its room failed every load: each socket that opened it was answered
// ygo's 500 with nothing logged, and the rebuild was refused ErrDocumentLoads
// (dispatch://INFRA-179, round 10).
func (s *Service) parkedPastRoomCaps(doc *crdt.Doc) *parkedPastCap {
	if doc == nil {
		return nil
	}
	stats := doc.PendingStats()
	if most := s.srv.MaxPendingItems; stats.Items > most {
		return &parkedPastCap{parked: parkedKindStructs, most: most, parks: stats.Items}
	}
	if most := s.srv.MaxPendingDeleteRanges; stats.DeleteRanges > most {
		return &parkedPastCap{parked: parkedKindDeleteRanges, most: most, parks: stats.DeleteRanges}
	}
	return nil
}

// refuseIfRebuilding refuses a room's load or injection while a rebuild's transaction holds it.
func (s *Service) refuseIfRebuilding(room string) error {
	if _, rebuilding := s.rebuilding.Load(room); rebuilding {
		return fmt.Errorf("%w: the document is being rebuilt; retry", ErrServiceUnavailable)
	}
	return nil
}

// allowInject decides whether ygo may apply an injection to a room. Its issue read goes
// through the shared pool for a caller that need hold no connection of its own (the unmark of
// a settled room's expired marks, unmarkExpired), and that is outside the pool's deadlock
// cycle only because ygo runs OnInject before getOrCreateRoom (sjawhar/ygo v1.55.1-sami.7,
// provider/websocket/inject.go:319-328): an injection refused here has published no room
// placeholder for a connection-holder to park on, so nothing holding a connection is waiting
// on this read. A vendored reordering of those two calls puts it back in the cycle.
func (s *Service) allowInject(ctx context.Context, info websocket.InjectInfo) error {
	if err := s.refuseIfRebuilding(info.Room); err != nil {
		return err
	}
	if s.shuttingDown(info.Room) && !isShutdownLoad(ctx) {
		return ErrServiceUnavailable
	}
	if err := s.awaitRoomRecovery(ctx, info.Room); err != nil {
		return err
	}
	if isOwnerVerified(ctx) {
		return nil
	}
	if s.roomClosed(info.Room) {
		return ErrIssueClosed
	}
	open, err := s.issueOpen(ctx, s.queryFrom(ctx), info.Room)
	if err != nil {
		return err
	}
	if !open {
		return ErrIssueClosed
	}
	return nil
}

// onLoadDocument is the room's load hook, which ygo runs once it has made room's document and set
// its pending cap, and before it hands the document to anyone. It decodes the state the room's
// load read (servicePersistenceAdapter.LoadDoc) into doc (bootstrapLoadedState), and a state that
// does not decode fails the room and drops the load's record (failRoomLoad) with the decode's
// error, which it returns to ygo, so the room is not published and its next access loads it
// afresh. A state the decode refuses for parking past one of the room's caps is logged as well,
// naming the cap and what the state parks, read off the copy the load decoded under the
// service's own cap (parkedPastCapLoadLine, parkedPastRoomCaps). Then it records what the room
// starts from: its issue's state, the settlement it owes, its ask blocks, its replica and its
// instance, and the update observer that classifies and credits every update the room commits.
func (s *Service) onLoadDocument(ctx context.Context, room string, doc *crdt.Doc) (err error) {
	// A refusal of this hook is, but for one, this service's own answer to the load - the
	// document is being rebuilt, the room has already failed, the store did not answer - and not
	// one to fail the room for a second time. The load it refuses is failed here, where that
	// refusal is made, by dropping the record the load began: no instance will take it, and ygo
	// removes the room and answers every caller with this error, so the room's name is left
	// holding nothing of this load. The one that is not ours is the stored history this load
	// read, which is decoded here (bootstrapLoadedState): a history the room's document cannot take is
	// the load's own failure, and it fails the room so the next access reloads rather than meeting
	// a half-loaded document. ygo loads one room at a time under a name - the goroutine that
	// created the placeholder owns the load and every other caller for that name waits on its
	// ready barrier (sjawhar/ygo v1.55.1-sami.7, provider/websocket/server.go:1750-1770) - and
	// this runs on that load's own goroutine, so the record read here is this load's own
	// (sjawhar/legion#2519).
	//
	// What LoadDoc recorded is taken first, so a load refused below leaves neither the state nor
	// its copy on the record it drops (loadedState). The two copies the load made are the room's
	// from here: the budget charges them below, at the bytes of the state they were built from
	// (chargeLoadedRoom).
	taken, record := s.takeLoadedState(room)
	defer func() {
		if err != nil && record != nil {
			s.roomLoads.CompareAndDelete(room, record)
		}
	}()
	loaded := taken.decoded
	// A load never waits for its own room's recovery. That recovery's eviction waits in ygo's
	// CloseRoom for the ready barrier this load holds (sjawhar/ygo v1.55.1-sami.7,
	// provider/websocket/inject.go:587-599) and closes the channel awaitRoomRecovery waits on
	// only once CloseRoom has returned (failRoomLocked), so a wait here is a cycle: the load
	// holds the eviction, and the eviction holds the load. No deadline breaks it either -
	// both loaders that reach this one carry context.Background(), the settlement warm-up
	// (settleRoom) and a committed write's publish (publishLiveUpdate) - so the room would stay
	// failed until the process restarted (LEGION-282). A failed room refuses the load
	// instead: ygo fails the load, closes the barrier with this error and removes the room,
	// which lets the eviction finish, and the next access loads the replacement.
	if err := s.refuseIfRebuilding(room); err != nil {
		taken.slot.release()
		return err
	}
	// The hold is read here, after the load read the store, so a rebuild can have committed and
	// lifted it in between: such a load read the history the rebuild replaced, and is refused as
	// the hold would have refused it (markRoomLoadRebuilt). Its next access reads the rebuilt one.
	if record != nil && record.rebuilt.Load() {
		taken.slot.release()
		return fmt.Errorf("%w: the document was rebuilt while its room loaded; retry", ErrServiceUnavailable)
	}
	if err := s.roomFailure(room); err != nil {
		taken.slot.release()
		return err
	}
	// The room counts among the resident rooms, and holds its place under the room cap
	// (reserveRoom), from here until ygo's OnUnloadDocument (releaseUnloadedRoom), unless this
	// load fails: ygo then drops the room with no OnUnloadDocument, so the load takes its own count
	// back. The token the count answers with is this load's own: the bytes counted against it are
	// taken back when an unload of the name ends it, or by this load's own failure (residentLoad).
	counted := s.roomLoaded(room)
	resident := false
	defer func() {
		if !resident {
			s.roomUnloaded(room, counted)
		}
	}()
	// The state is decoded into the room's own document here rather than through ygo (LoadDoc),
	// before anything reads or observes the document (bootstrapLoadedState). It is the last thing
	// the load's grant covers, so it is given back as soon as it returns: the reads below wait
	// on the rooms pool, and must hold nothing of it while they do (podLoadSlots).
	decodeErr := s.bootstrapLoadedState(room, doc, taken.state)
	taken.slot.release()
	if decodeErr != nil {
		// The load's own failure, not this service's refusal of it: these are the bytes the
		// store held for this document, and no room can serve them. The room fails, so the
		// reader who comes next reloads it (failRoomLoad, which the defer above then finds
		// nothing left to drop). A history that parks past the room's cap is one a person
		// loses the document to until they rebuild it, so its line says which cap and by how
		// much; every read answers it as one that cannot load, and the rebuild takes it.
		if errors.Is(decodeErr, crdt.ErrPendingCap) {
			if parked := s.parkedPastRoomCaps(loaded.doc); parked != nil {
				slog.Error(parkedPastCapLoadLine, parked.attrs(room)...)
			}
		}
		if record != nil {
			s.failRoomLoad(room, record, decodeErr)
		} else {
			s.failRoom(room, decodeErr)
		}
		return decodeErr
	}
	// Everything this load needs comes from the pool that owns loads. A writer holding a
	// pooled connection and the issue's owner lock waits for this load, so a read of the shared
	// pool here waits for a connection only that writer can release: the wedge, arrived at
	// without a transaction of its own and so invisible to the pool's guard.
	rooms, err := s.store.Pool.Rooms()
	if err != nil {
		return err
	}
	// The room's browser updates are stored on the room-appends pool, which opens a connection
	// beside this load when it holds none, so the room's first batch dials none (warmRoomAppends).
	s.warmRoomAppends()
	// The issue is read once per room load. A cold socket's admission, which holds the room's state
	// while its load runs, has read it and recorded it there, as has a close or reopen of the issue
	// since (admittedIssueRead); a load no admission holds - a settlement's warm-up, a committed
	// write's publish - reads it here.
	issue, admitted := s.admittedIssueRead(room)
	if !admitted {
		if issue, err = s.readIssue(ctx, rooms, room); err != nil {
			return err
		}
	}
	// The load takes no advisory lock, which a durable writer can hold for as long as its
	// transaction runs: the row it reads is written whole by each writer's transaction, so a
	// plain read sees one writer's commit or the one before it. What the record has recorded by
	// then is taken first, so an edit source recorded after the read is told from one before it.
	mark := s.markEditSource(room)
	owed, lastActor, err := readOwedSettlement(ctx, rooms, room)
	if err != nil {
		return err
	}
	// The room is still loading: ygo hands its document to no peer or caller until this hook
	// returns (sjawhar/ygo v1.55.1-sami.7, provider/websocket/server.go:1909-2000: loadRoom closes
	// the room's ready barrier after it), so nothing writes the tree while this walks it. Its
	// rendering, with the layout recorded with it, is the room's replica's own (keepReplica,
	// below): the replica is the copy this load decoded, which stands at the state rendered here,
	// since nothing from here to OpenUpdateLog hands the document to anything, and a replica the
	// load hands no copy takes this rendering for the base its first run starts from
	// (renderedReplica.start). It is also the room's first measurement, which the peer update
	// hook weighs the room's first update against and the room budget charges the load by. A load
	// that took a cold socket's preload takes the tree and the rendering that socket's admission
	// made of the same state, found inside the schema (roomLoad.takeLoaded), so the document is
	// rendered once.
	var size int
	var rendered admissionRendering
	if value, ok := s.roomLoads.Load(room); ok {
		size, rendered = value.(*roomLoad).takeLoaded()
	}
	var renderErr error
	if rendered.tree == nil || rendered.rendering == nil {
		rendered.tree, renderErr = treeOf(doc)
		if renderErr == nil {
			if s.admissionRendered != nil {
				s.admissionRendered(room)
			}
			rendered.rendering, renderErr = replicaRenderingOf(rendered.tree)
		}
	}
	var askBlocks map[string]struct{}
	switch {
	case errors.Is(renderErr, ErrDocOutsideSchema):
		slog.Warn("dispatch: loaded document outside Proof schema; a replacement from markdown repairs it", "room", room, "error", renderErr)
	case errors.Is(renderErr, ErrDocNewerContent):
		// The room loads, so a write meets the document's own refusal rather than a failed room.
		slog.Warn("dispatch: loaded document holding an element this build cannot read; another build may serve it", "room", room, "error", renderErr)
	case renderErr != nil:
		return renderErr
	default:
		askBlocks = askBlockIDs(rendered.tree)
	}
	// The history this load decoded counts as resident against this load's own token, so the
	// unload that ends the token takes those bytes back wherever the room's record has gone by then
	// (countResidentBytes).
	s.countResidentBytes(counted, int64(size))
	// The room this load publishes holds the state it takes from here until ygo retires it
	// (releaseIfUnusedLocked). The issue's state is the newest the record holds, this load's read
	// or a close recorded after it began.
	state := s.lockState(room)
	state.recordIssueLocked(issue)
	open := !state.closed
	// A room a late departure's settlement loaded is that settlement's until a browser or a writer
	// takes it up (withSettlementLoad, retireSettlementRoom). It is known by this load's token as
	// well as its document, so an earlier room's unload, which may run before ygo hands this room
	// to anyone, does not read as this room's end (warmedRoom).
	if isSettlementLoad(ctx) {
		state.warmed = &warmedRoom{doc: doc, load: counted}
	}
	// The room has counted itself loaded here since roomLoaded, above, and stays counted until its
	// unload (roomLoadedHere), so a socket's admission offers it no preload from then on
	// (offerPreload). An offer that checked before that count and has stored its preload is under
	// the room's name now, and it is not this load's - LoadDoc's takePreload ran before any of this
	// - so it is one no load of this room will ever use, and it is taken out here, under the room's
	// record that the offer stored it under, rather than left for the socket that offered it to
	// withdraw hours later.
	s.preloads.Delete(room)
	// The ask blocks the room loaded with are the baseline its update observer tells new ones by.
	// They were not introduced by any update the observer sees, so none gains an author here: a
	// settlement finds the author of one no ask indexes yet where its writer recorded it, in
	// doc_ask_block_authors (askBlockSources.author).
	if askBlocks == nil {
		state.askBlocks = nil
	} else {
		state.observeAskBlocks(askBlocks, nil)
	}
	// The document owes a settlement no settlement committed: one a shutdown's budget cut short,
	// one a room failure dropped (failRoomLocked), or one its issue's close stopped
	// (closeRecordLocked). Its timer lived in the process or the room that is gone, so the room
	// settles once, whether or not its issue is closed, rather than waiting for an edit to arm
	// one - unless the load is a settlement's own warm-up, which settles it next. Its authors are
	// in the document's pending authors, which that settlement reads; the room restores only the
	// latest edit source the settlement names. A room that failed again while this load ran
	// leaves the row for its own replacement.
	//
	// The row is that source as the store has it, so it replaces whatever the record held for a
	// room that has gone (a forced close keeps the record, evictRoom): a row naming none - an edit
	// any of several connected browsers could have made - leaves the settlement its own actor
	// rather than the last source an earlier room saw, and no row leaves no source, as on a fresh
	// record. The record keeps its own when the row may be older than it (rowIsCurrent). A closed
	// issue's record takes no source from the row either: the close moved the record's source to
	// the row and left the record none, which the reopen restores from the row (restoreLastActor),
	// and a settlement takes its actor from the row itself (settlementAuthors).
	switch {
	case !open || !mark.rowIsCurrent(state):
	case owed && lastActor != nil:
		state.lastActor = lastActor
		state.unsettled = true
	default:
		state.lastActor = nil
		state.unsettled = false
	}
	if state.failed == nil && owed && !state.settleWarming {
		s.scheduleSettleLocked(room, state)
	}
	s.unlockState(room, state)
	// The room takes peer updates only once this hook returns, and every one of them under the
	// room's scan budget, which keepReplica sets below (newRoomScans): the state the load decoded
	// is the server's own and went in unbounded. ygo set the room's pending cap, maxParkedStructs,
	// before it ran this hook (sjawhar/ygo v1.55.1-sami.8, loadRoom,
	// provider/websocket/server.go:1921-1924), and this hook's own decode of the state ran under
	// it (bootstrapLoadedState), as every peer's apply does
	// (TestAPeerApplyIsHeldToTheRoomsPendingCapAfterTheLoad).
	//
	// The room's scan total starts at what the decode of the state the load read scanned, and the
	// room's replica is the copy that decode made: the decode the load made of its own
	// (LoadDoc, decodeStoredCopy), which this hook's own decode of the same bytes into this
	// document matches item for item - both run with the format cleanup off and under the same
	// pending cap - scanning the same items, and reporting none, since that decode ran before
	// newRoomScans listed the document. Nothing else holds that copy - the preload and the load's
	// record gave it up as it was taken (takePreload, takeLoadedState) - so the replica owns it,
	// and the room's first transaction decodes nothing more (renderedReplica.observe). A room ygo
	// loaded without reading the store through this service's persistence is decoded again here,
	// from its own document's state, under a grant of its own weighed on that state and on the
	// structs the room itself holds, which that state's copy will hold too (holdServerLoad,
	// copyWeight, heldStructs). The structs are read as an apply reports them, parked structs
	// included: the encode carries what the room has parked, so the copy this decode makes holds
	// them as well, and a reading that left them out would under-charge a room whose pending
	// queue is heavy by everything in it (the class of review 5856254176's second High, found
	// again here by its own search).
	stateBytes := int64(len(taken.state))
	if record == nil {
		encoded := crdt.EncodeStateAsUpdateV1(doc, nil)
		release := holdServerLoad(ctx, copyWeight(int64(len(encoded)), int64(heldStructs(doc))))
		loaded, _ = decodeStoredCopy(encoded, s.maxPendingItems)
		release()
		stateBytes = int64(len(encoded))
	}
	// The log is opened before the observer is registered and before ygo hands the room to anyone
	// (above), so it holds every transaction the room commits from here on, in commit order, and
	// the copy is the room as the log's first transaction finds it, which the rendering above is
	// of. The replica's verdict starts as this load's rendering said, and the observer's renders
	// move it on from the first update (renderedReplica.verdict).
	replica := s.keepReplica(doc, doc.OpenUpdateLog(), rendered.rendering, loaded.doc)
	replica.recordVerdict(renderErr)
	replica.scans.rescanned(loaded.scans)
	// The room's charge in the room budget is the bytes of the stored state this load decoded,
	// at both copies (roomCopies, roomWeight): the room's own document and the replica's copy
	// were both built from them. From here each commit's addition to the encoded state is added
	// to the room's tracked state - a remote apply's by its report, any other by the drain that
	// hands it over (roomApplies.noteApply, renderedReplica.countCommit) - each drain weighs the
	// room again from it (renderedReplica.chargeAdded), and a compaction that folds the room's
	// whole stored history re-bases it (rebaseRoomState).
	s.chargeLoadedRoom(room, replica, stateBytes)
	instance := s.keepInstance(room, doc, counted)
	replica.classify = func(logged crdt.LoggedUpdate, seq uint64, contentChanged bool, asks func() map[string]struct{}) (bool, bool, bool) {
		return s.classifyDrained(room, instance, logged, seq, contentChanged, asks)
	}
	doc.OnUpdate(func(update []byte, origin any) {
		// A published write's update is already durable, and so is a catch-up's batch of another
		// process's rows (remoteUpdateOrigin). Its suppression slot is finished here, before ygo's
		// persistence observer, which the room registers after OnLoadDocument, hands the update to
		// the room's worker, which waits on that slot (publishLiveUpdate, catchUpRoom). Both are
		// then classified like any other update, and neither is credited to a local peer
		// (creditContentChange) nor names one as the author of the ask blocks it introduces
		// (observeAskBlocksForUpdate, registerAskAuthors), unless its drain landed a frame a local
		// peer parked, which is credited, and its blocks attributed, as a browser's commit is
		// (classifyDrained).
		switch durable := origin.(type) {
		case *liveWriteOrigin:
			s.finishSuppressedPersistence(durable.slot, update)
		case *remoteUpdateOrigin:
			s.finishSuppressedPersistence(durable.slot, update)
		}
		if repair, identityRepair := origin.(*identityClosureOrigin); identityRepair {
			s.recordSuppressedCommit(repair.slot, update)
			// A repair's update is classified like any other, and its class names the room
			// instance it was made in: one that reaches the room's persistence outside its
			// suppression slot, which another writer's update can take first
			// (consumeSuppressedPersistence), is stored as any other update, and the room it was
			// made in has to take that version in or stand behind the head for good.
		} else if s.beforeObserveUpdate != nil {
			s.beforeObserveUpdate(room)
		}
		// Every observer drains the room's update log, a repair's too, and each committed update
		// is charged and classified once, by whichever drainer reaches it first - an observer, or
		// an exact read (atPoint) - against the room as the commit before it left it
		// (renderedReplica). This observer's own update is in the log - ygo appended it under the
		// document's lock at its commit - so its class is recorded, in the drain and in commit
		// order, before ygo's persistence observer, which runs after this one on this goroutine,
		// hands it on.
		s.observeRoomUpdates(room, replica)
	})
	// A commit that changed nothing the room holds - one whose whole update waits for an edit the
	// room has not received, so it only parked - has no update observer (ygo calls OnUpdate only
	// for a transaction that changes the document, reearth/ygo#350), but ygo logs it. ygo calls
	// OnAfterTransaction for every committed transaction, outside the document's lock and before
	// OnUpdate, so the replica is drained here for a transaction that changed no shared type while
	// the log holds something: the replica takes the commit as one more position, and the claim
	// rule's set of the ids the stored history names takes the ids of what it parked
	// (classifyDrained, storedClients.took), and nothing else, whichever path the update reached
	// the room by - a peer's connection, or a published write whose update waits on an item a cold
	// fork read from the store. A transaction that changed a shared type is drained by the update
	// observer above, after the steps it takes first.
	doc.OnAfterTransaction(func(txn *crdt.Transaction) {
		if replica.log.Pending() > 0 && len(txn.ChangedTypes()) == 0 {
			s.observeRoomUpdates(room, replica)
		}
	})
	resident = true
	return nil
}

// leaveParkedOut makes what doc parks no part of it until it lands. doc is a room's document, set
// as the room's load lists it (keepReplica), or a document of its replica. Its encodes - the whole
// state a read or a rebuild takes, the answer to
// a browser's first sync, the base its update log records - carry what it integrated and nothing
// of its pending queue (crdt.Doc.SetParkedOutOfState), and a drain of that queue taken back
// discards everything the update that filled the gap released rather than parking it again,
// naming the applies that parked it (crdt.Doc.SetDiscardRefusedDrain, closeDiscardedSenders).
// With the server's half (New: RelayIntegratedOnly, AnswerParkedNotApplied) a parked frame is
// neither relayed, nor answered applied, nor stored, nor shown to a reader, so it is kept only by
// the editor that sent it, which sends it again, and by the queue until the gap fills. The queue
// itself stays bounded by the room's pending caps, Server.MaxPendingItems for its structs
// (maxParkedStructs) and Server.MaxPendingDeleteRanges for its delete ranges
// (maxParkedDeleteRanges), which park only under clients their sender's socket holds (parkJudge);
// its structs are charged against the decode cap's headroom (ceilingWithQueue), and its delete
// ranges to the room budget (bytesOfHeapAParkedRange).
//
// The replica's documents take it as the room's does, so the two agree on what a drain landed:
// the only parked content a replica can hold is what a load decoded into both from rows an
// earlier build stored of what parked, and a drain of it that the room discarded the replica's
// own check refuses (holdParked) and discards alike.
func leaveParkedOut(doc *crdt.Doc) {
	doc.SetParkedOutOfState(true)
	doc.SetDiscardRefusedDrain(true)
}

// weighStateAnswer is ygo's Server.OnStateAnswer: the room runs it immediately before it answers
// a browser's SyncStep1 with the room's state, and runs the release it returns once that answer
// is in that browser's write queue, by every way out including the one that sends nothing
// (theorem-labs/ygo v1.55.1-theorem.2, provider/websocket/state_answer.go). It holds a grant of
// the pod's copy heap for the answer, weighed by the path ygo's answer takes for that room
// (answerWeight) and by the browser's own SyncStep1 (stepOneWeight), and refuses the answer with
// the room's busy close where the pool has no room for it inside the server's wait bound.
//
// A join of a room already loaded was free here before, and it is not free. The answer does not
// send the room's state as one frame: it cuts what that browser is missing into frames each
// small enough for a browser to apply in one task and holds every one of them until they are in
// the browser's write queue (ygo's sync.EncodeSyncStep2Split), all of it built on the peer's read
// loop, so a room holds its state once more for each browser it answers. Every room's answer is
// cut from its own structs (crdt.SplitStateAsUpdateV1), which builds the frames and the cut's own
// record of each client and decodes nothing, and carries nothing the room parks, which is no part
// of its state (leaveParkedOut). Measured on a run of a million keystrokes, 1,000,010 structs, an
// answer to a browser holding nothing held 24,071,008 to 25,428,848 bytes above the document,
// having allocated about 25.7 MB in under 900 objects
// (TestAStateAnswerHoldsNoMoreHeapThanItsGrantParkedOrNot). Before the fork's v1.55.1-sami.14,
// which the pinned theorem-labs/ygo v1.55.1-theorem.13 carries, a room that parked anything was
// cut from its encoded state instead, decoding the whole document twice - 371,149,976 to
// 429,891,496 bytes held and about 840 MB
// allocated for the same room - and is no longer. Nothing in ygo bounds how many answers run
// together, so before this hook a reconnect storm on one big room built one per browser side by
// side, off the pod's load concurrency entirely, while every other whole-document copy in the
// process queued for it.
//
// The grant is read off the room without encoding anything: its tracked state bytes, which the
// service holds (roomWeighing.roomStateBytes), and the structs the answer may have to encode
// (websocket.StateAnswerInfo.Structs). Both are read again once the grant is in hand and the
// grant topped up where the room grew while this answer waited for it, which costs nothing where
// it did not (StateAnswerInfo.StructsNow, and loadSlot.take, which takes the difference alone):
// the answer is built after this returns, so a grant sized before a wait is otherwise a grant
// sized on a room that has since changed. Beside the room, the grant weighs the browser's own
// SyncStep1, whose state vector the answer decodes into a map it holds while it cuts
// (stepOneWeight): a browser names the clients it holds there, as many as its message's bytes
// carry, whatever the room holds.
//
// A wait is bounded by WriteWaitBound, the server's own bound on what a caller spends at a
// memory bound, and the browser a refusal closes is closed DOC_BUSY, the transient close the
// editor comes back from and the one the room budget closes a browser it has no room for with.
// Refusing beats waiting on past that: a browser whose answer waits has sent its SyncStep1 and
// is shown nothing at all until it comes, where a browser closed DOC_BUSY is told why and comes
// back to a pod that has room.
func (s *Service) weighStateAnswer(info websocket.StateAnswerInfo) (func(), error) {
	slot := newLoadSlot()
	ctx, cancel := context.WithTimeout(context.Background(), WriteWaitBound)
	defer cancel()
	asked := stepOneWeight(info.Step1)
	// The grant is at most one slot, as every taker's is (copyWeight), so the heaviest answer
	// waits for the pool rather than being refused by it.
	grant := func(structs int) (state, weight int64) {
		state = s.roomWeighingOf(info.Doc).roomStateBytes()
		return state, min(answerWeight(info.Doc, state, int64(structs))+asked, loadSlotBytes)
	}
	state, weight := grant(info.Structs)
	if err := slot.take(ctx, weight); err != nil {
		return nil, s.refusedStateAnswer(info, state, weight, err)
	}
	state, weight = grant(info.StructsNow())
	if err := slot.take(ctx, weight); err != nil {
		slot.release()
		return nil, s.refusedStateAnswer(info, state, weight, err)
	}
	return slot.release, nil
}

// answerWeight is what the cut of one answer to a browser's SyncStep1 holds of the pod's copy
// heap, for the room live tracked at stateBytes of state and holding structs structs, as ygo's
// answer is built for that room as it stands (crdt.SplitStateAsUpdateV1, weighStateAnswer): the
// cut of the room's own structs, which is the whole answer, since nothing the room parks is read
// back into it (leaveParkedOut). It is the larger of the room's tracked state at
// answerHeapAStateByte, and its clients at answerHeapAClient with the update the cut is packing
// beside them (answerFrameWeight). The state term prices the updates the cut has written, which
// it holds until the answer is sent; the other prices the record the cut builds of each struct of
// the update it is still packing, and the queue it keeps for each client. The grant is the larger
// of the two and not their sum because the larger covered every room measured
// (answerHeapAStruct), and a sum would charge the frame again to every room whose state term
// already covers it: a room of a mebibyte of prose is granted 24,281,920 bytes on its state, as
// it was before the frame was priced, where the update at its fullest asks 1,620,096 beside the
// clients. What the browser's own step 1 holds, and the one slot the grant is held to, are
// weighStateAnswer's.
//
// The room's clients are what this answer's heap follows, and the library counts them in constant
// time under the document's read lock (crdt.Doc.ClientCount, theorem.3). A room of many structs
// and few clients is therefore priced for the clients it has: a run of a million keystrokes is
// granted 63,869,984 bytes where it measures 25,428,848 at most, against the 340,003,400 the
// room's struct count would have charged it.
//
// It is a price of what the cut holds at once, as every taker of the pool is priced, and not of
// what it allocates, which can be several times more: what the cut allocates past what it holds
// is scratch it drops update by update - each update it packs begins a delete set and a state
// vector of its own, grows an encoder to the update's size and copies the update out of it - and
// garbage is the Go memory limit's to bound, which the collector steers the whole heap to, not
// the pool's. Measured 2026-11-16 on theorem-labs/ygo v1.55.1-theorem.6 with a struct priced at
// 88 bytes (answerHeapAStruct, raised since for arm64), the fewest bytes of five answers against
// the most live heap a collection marked (liveHeapPeak): the grant covered what
// each answer held - 1.06 to 4.21 times over in rooms of 1,000 structs and more, and in smaller
// ones by its fixed 4,096 bytes, which cover the 3,504 at most such a room held past the terms
// that grow with it (TestAnAnswerFromARoomOfUnderAThousandStructsHoldsNoMoreThanItsGrant) -
// while it fell short of what the
// answer allocated in every room of small or many-client structs: a run of GC structs of one
// client by 0.18 to 0.69 of its allocation, from 256 structs to a million (82,698,000 bytes
// allocated against 16,002,248 granted, 12,046,320 held); rooms of one GC struct a client, and
// 135 clients of 127, by 0.50 to 0.73, up to a million clients (583,662,504 against 341,412,436,
// 249,566,496 held); a split-dense run by 0.73 to 0.80, from 102,400 characters to 1,024,000
// (11,575,360 against 8,405,672, 2,411,016 held); and runs of 1,000 and 4,000 keystrokes by
// 0.68 and 0.72. Prose, a long paragraph and keystroke runs of 40,000 and more are granted past
// what they allocate (TestAnAnswerAllocatingPastItsGrantHoldsNoMoreLiveHeapThanIt).
func answerWeight(live *crdt.Doc, stateBytes, structs int64) int64 {
	return max(stateBytes*answerHeapAStateByte, int64(live.ClientCount())*answerHeapAClient+answerFrameWeight(structs))
}

// stepOneWeight is what the answer to step1, a browser's SyncStep1 message, holds of it while
// the answer is cut: the state vector step1 names, which ygo decodes into a map before the cut
// reads it (sync.EncodeSyncStep2Split, crdt.DecodeStateVectorV1), at answerHeapAStepOneEntry an
// entry. The entries are the count the vector's first number names, which the decoder makes its
// map for before reading one, and none for a message that names none or one the decoder refuses
// before it makes the map: a count past half the vector's bytes or past maxUpdateItems
// (crdt/update.go:395-418 in theorem-labs/ygo v1.55.1-theorem.9). Read off the message's first
// numbers, it costs nothing to weigh.
func stepOneWeight(step1 []byte) int64 {
	kind, rest, ok := readUvarint(step1)
	if !ok || kind != 0 {
		return 0
	}
	length, vector, ok := readUvarint(rest)
	if !ok || length > uint64(len(vector)) {
		return 0
	}
	entries, _, ok := readUvarint(vector[:length])
	if !ok || entries > length/2 || entries > maxUpdateItems {
		return 0
	}
	return int64(entries) * answerHeapAStepOneEntry
}

// answerFrameWeight is what the update an answer's cut is packing holds, for a room of structs
// structs, with what any answer holds whatever its room: answerHeapAnAnswer, and
// answerHeapAStruct for each of the room's structs up to answerFrameStructs, the most structs one
// update can hold. The structs are the ones ygo counts for the answer and hands weighStateAnswer
// (websocket.StateAnswerInfo.Structs), so the term reads nothing of the document.
func answerFrameWeight(structs int64) int64 {
	return answerHeapAnAnswer + min(structs, answerFrameStructs)*answerHeapAStruct
}

// answerHeapAStateByte is the heap an answer cut from a room's own structs holds for each byte of
// the room's state: its frames, held twice over while each message is wrapped in its frame, and
// what the cut builds besides them, the room's whole delete set among it
// (crdt.SplitStateAsUpdateV1). Measured 2026-11-13 and 14 on theorem-labs/ygo v1.55.1-theorem.2
// at copyMeasurementGCPercent, in two to three runs of three answers a shape to a browser holding
// nothing, on a two-processor machine other measurements were running on
// (TestAStateAnswerHoldsNoMoreHeapThanItsGrantParkedOrNot, run with
// `DISPATCH_MEASURE_COPY_HEAP=1 DISPATCH_TEST_DATABASE_URL="$LEGION_TEST_PG_DSN" go test
// ./internal/dispatch/docs/ -count=1 -timeout 60m -run
// TestAStateAnswerHoldsNoMoreHeapThanItsGrantParkedOrNot -v`), over the shapes of few clients:
// the most 7.83 bytes a state byte (a run of 1,024,000 characters with every other one deleted,
// 83,953,128 bytes above a 10,727,526-byte state, where the same answer's other five held
// 41,912,680 to 71,945,360), then 4.96 (one paragraph of a mebibyte typed in one transaction),
// 3.19 (a run of a million keystrokes) and the least 3.06 (a mebibyte of prose). It is set past
// the most, as the room budget's prices are (bytesOfHeapAStateByte).
const answerHeapAStateByte = 8

// answerHeapAClient is the heap an answer cut from a room's own structs holds for each client the
// room holds: the cut keeps a queue, a place in its walk and an entry of each map it keys by
// client for every one of them, whatever the client wrote. Measured as answerHeapAStateByte is,
// on a room of a million clients that wrote one struct each, which any browser can make, since
// the library takes a client id from whoever sends it: 296,397,272 to 331,374,744 bytes above
// the document, the most 331.37 bytes a client, which is 22.09 bytes a byte of its state. It is
// set past the most, as bytesOfHeapAStruct is, and is charged on the clients the room holds
// structs of, as the library counts them (crdt.Doc.ClientCount, answerWeight).
const answerHeapAClient = 340

// answerHeapAStruct is the heap an answer cut from a room's own structs holds for each struct of
// the update it is packing: a 24-byte record of where the struct's bytes lie in the update, a
// 16-byte range of the update's delete set for a struct that is deleted, and the struct's own
// bytes, each kept in an array that is held twice over while it grows (crdt.SplitStateAsUpdateV1:
// the splitter's spans, pieceDS and arena, the first and last of which it keeps from one update
// to the next). The state term prices a struct by its bytes, and the smallest struct is two
// bytes, a GC struct any browser can send: a run of 11,947 of them held 914,840 bytes where the
// state term granted 193,224, and the keystroke and split-dense runs an editor makes fell short
// of it too.
//
// Measured 2026-11-15 on theorem-labs/ygo v1.55.1-theorem.6, on a copy of the fork that collects
// at every growth of those arrays, of each encoder and of each update's copy and reads the live
// heap above the document there, counting the array a growth copies from: 680 readings of rooms
// of 4 to 137,127 structs - runs of GC structs of one client, rooms of 1, 64, 127, 128 and 200 GC
// structs a client, keystroke runs, split-dense runs, prose, a thousand clients beside a GC run
// and the rooms the tests answer - of which 585 held more than the state and client terms alone
// granted. answerHeapAStruct and answerHeapAnAnswer are the pair that covers all of them: with
// 4,096 set aside, the most a room needed was 85.95 bytes a struct (a run of 2,048 characters
// with every other one deleted, 181,656 bytes beside its two clients' 680), then 83.90 (a run of
// 257 GC structs, 27,176) and 79.54 (542 structs of 200 GC structs a client, 48,568). It was set
// past the most, at 88, as bytesOfHeapAStruct is.
//
// It is raised to 101 for arm64, which holds more of the same cut. CI's envoy-go-docs-finalizers
// job 134902505095 (run 44333064387, ubuntu-24.04-arm, 2026-11-18) read the run of 256 GC structs
// of one client - 266 structs of 2 clients - holding 30,608 bytes at the fewest of five readings
// (TestAnAnswerFromARoomOfUnderAThousandStructsHoldsNoMoreThanItsGrant), where this repository's
// amd64 pod read 27,448 at two processors in seven of eight runs that day (25,400 in the other),
// against the 28,184 that 88 granted: 1.027 times its reading. The same margin over the arm64
// reading is 30,608 x 28,184 / 27,448 = 31,429, and beside the two clients' 680 and the fixed
// 4,096 the room's 266 structs take (31,429 - 4,776) / 266 = 100.2 bytes each, so 101, which
// grants that room 31,642: 1.034 times the arm64 reading and 1.153 times the amd64 one. The run
// of 512 GC structs, which read 51,408 there and 47,928 here, is granted 57,498. On amd64 the
// raise charges 13 bytes more for each struct of the update being packed, up to
// answerFrameStructs: at most 208,000 bytes more an answer, and nothing more where the state term
// sets the grant.
const answerHeapAStruct = 101

// answerHeapAnAnswer is the heap an answer cut from a room's own structs holds whatever the room:
// the splitter, its maps, the encoders and each update's three copies at their smallest. The ten
// structs of the seeded paragraph held 2,800 to 3,296 bytes in all, 1,946 beyond their 1,010 at
// 101 a struct and their client's 340, and the document a test room is seeded with 2,112 to 2,480.
// It is set at 4,096, past those, which is what leaves answerHeapAStruct at the 100.2 the
// small-struct rooms needed, 101, rather than the 107.9 a 2,048 would.
const answerHeapAnAnswer = 4_096

// answerFrameStructs is the most structs one update of an answer can hold, past which the update
// the cut is packing grows no further. An update is at most stateAnswerMessageBytes, and the
// smallest a struct is in one is four bytes: a GC struct at a clock under 128 and its range of
// the delete set, two bytes each, with each client's group header shared by the 128 such structs
// a client can hold. Measured as answerHeapAStruct is, the most one update held was 15,791 in
// rooms of 127 and of 128 GC structs a client whose client ids take four bytes, and 15,962 where
// the ids count from 1, so that the first 127 take one. It is set past the most.
const answerFrameStructs = 16_000

// answerHeapAStepOneEntry is the heap the map ygo decodes a browser's SyncStep1 state vector into
// holds for each entry the vector names (crdt.DecodeStateVectorV1, which makes the map at that
// count before it reads one). Measured 2026-11-19 on theorem-labs/ygo v1.55.1-theorem.9 with
// go1.27.3, the live heap two collections left above the decode with the vector's own bytes kept
// alive, at counts from 7 to 1,048,576 (TestAStepOnesStateVectorHoldsNoMoreThanItsPrice): the
// most 37.83 bytes an entry at 500,000 entries, then 36.99 at 1,000 and 36.08 at 1,048,576, and
// the least 23.65, from 50,000 to 200,000, as the map's tables round up to the next size. It is
// set past the most. A browser's step 1 names what it holds, as many entries as its message
// carries: one frame can name 2^20, the most the decoder takes, which is 41,943,040 bytes here
// and nothing the room's own terms see.
const answerHeapAStepOneEntry = 40

// refusedStateAnswer is the close a browser is given whose first sync the pod's copy heap had no
// room to answer inside the server's wait bound, and the one line that says so: the room, what
// the answer would have carried, and the grant it waited for and the bound it waited to
// (weighStateAnswer). ygo logs a
// refused state answer at Debug alone (provider/websocket/state_answer.go, "state answer
// refused"), which the pod's level drops, so without this line an operator sees a browser's
// socket close before its first sync and nothing says why.
func (s *Service) refusedStateAnswer(info websocket.StateAnswerInfo, state, weight int64, err error) error {
	used, limit := s.budget.figures()
	slog.Warn("dispatch: refused to answer a browser's first sync; the pod's copy heap had no room for the answer",
		"room", info.Room, "structs", info.Structs, "state_bytes", state, "grant_bytes", weight,
		"wait_bound", WriteWaitBound, "charged", used, "limit", limit, "error", err.Error())
	return documentBusy()
}

// observeRoomUpdates drains replica's log and arms the settlement the drained updates owe. It runs
// from the room's update observer for every commit that changed the document, and from its
// after-transaction drain for one that changed nothing (onLoadDocument). The caller holds no
// replica lock.
func (s *Service) observeRoomUpdates(room string, replica *renderedReplica) {
	settles, changed, local := replica.observe(room)
	if changed && s.afterCreditUpdate != nil {
		s.afterCreditUpdate(room)
	}
	s.armDrainedSettle(room, settles, changed, local)
}

// observeRoomDoc drains live's update log for a caller that is not one of its observers, so every
// update committed to the room by now is classified (classifyDrained), and arms the settlement
// what it classified asks for, as the observer it stood in for would have. A document this
// process lists no replica for - one no room of its loaded, or whose room's document has been
// collected - has nothing to drain. It runs no room observer's test hook: it is no observer.
func (s *Service) observeRoomDoc(room string, live *crdt.Doc) {
	replica := s.replicaOf(live)
	if replica == nil {
		return
	}
	settles, changed, local := replica.observe(room)
	s.armDrainedSettle(room, settles, changed, local)
}

// armDrainedSettle arms room's settlement for what one drain of its update log classified
// (renderedReplica.observe).
func (s *Service) armDrainedSettle(room string, settles, changed, local bool) {
	switch {
	case local:
		s.scheduleSettle(room)
	case settles && changed:
		// A content change another process made, which a catch-up applied, only re-arms a
		// settlement already armed here and arms none where none is (rearmSettle): the process
		// whose peer made it armed its own when that peer's update reached its room, so editors
		// on two processes typing in turn make one burst, which settles once, a settle delay
		// after the last keystroke on either.
		s.rearmSettle(room)
	}
}

// classifyDrained records the class of logged, the update room's log handed over at commit
// position seq in instance, given whether it changed the room's rendered markdown and the ask
// blocks it left when it did (renderedReplica). It is the room's drainClassifier, which runs
// holding the replica's lock. It reports whether a settlement follows the update, which is every
// update but a repair's, whether it changed the content, and whether this process made it - a
// peer's update, a published live write's, or a catch-up whose drain landed a frame one of this
// room's browsers parked, rather than another process's append a catch-up applied.
//
// A commit can park bytes - structs or deletions that wait for an edit the room has not received -
// and ygo logs them in Parked, beside an Update that carries only what the commit integrated, the
// bytes OnUpdate hands the room's persistence. The parked bytes are no part of the document until
// the commit that lands them, whose own update carries them (leaveParkedOut), so nothing here
// takes them but the claim rule's set of the ids the stored history names, which takes the ids
// they are under (storedClients.took): a catch-up's row that parks here is stored all the same.
// A commit that only parked is logged with an empty update (emptyUpdateV1), and ygo calls no
// OnUpdate for it, so no persistence call will ever take a class for it: none is queued.
//
// The cursor is the instance's, taken here rather than looked up when the update reaches the
// store: a room evicted and reloaded in between is a different instance, which did not take this
// update and must not be recorded as holding the version it is stored at. The instance's resident
// token is taken with it, so the update's bytes count against the load that committed it
// (queueUpdateClass). An update only the room holds is listed on the instance as unstored
// (roomInstance.unstored) as it is classified, so a room query of another process can answer with
// it (unstoredShare).
func (s *Service) classifyDrained(room string, instance *roomInstance, logged crdt.LoggedUpdate, seq uint64, contentChanged bool, asks func() map[string]struct{}) (settles, changed, local bool) {
	origin, published := logged.Origin.(*liveWriteOrigin)
	catchUp, remote := logged.Origin.(*remoteUpdateOrigin)
	// The claim rule's set of the ids the stored history names takes every commit the instance
	// makes as it is classified here, the ids of what it parked included.
	instance.storedClients.took(logged.Update, logged.Parked)
	if len(logged.Parked) > 0 && bytes.Equal(logged.Update, emptyUpdateV1) {
		return false, false, false
	}
	class := documentUpdateClass{
		seq:            seq,
		contentChanged: contentChanged,
		cursor:         &instance.cursor,
		resident:       instance.resident,
		observed:       time.Now(),
	}
	if _, identityRepair := logged.Origin.(*identityClosureOrigin); identityRepair {
		// A repair's writer appends its update in its own transaction, under the document's
		// advisory lock it holds while the room takes the update, so the class is queued stored:
		// no durable writer of the room stores it again, and it is no work the room's state
		// waits on. It is queued all the same, so the room's persistence, which ygo hands it as
		// any other update, finds it stored when it falls outside its suppression slot
		// (consumeSuppressedPersistence). What it added is counted into the room's tracked state
		// by the drain that handed it over, as every commit no report covers is, at what the
		// replica's apply of it reported (renderedReplica.countCommit).
		s.queueUpdateClass(room, logged.Update, class, true)
		return false, false, false
	}
	class.durable = true
	// A published write's update was stored by the transaction that wrote it, before the room
	// took it, and a catch-up's is the stored row itself, so neither is stored again - unless the
	// commit carries more than its writer stored: the drain of the room's pending queue the apply
	// ended with landed content a browser's frame had parked behind the clocks the write supplied,
	// and that content is in no row (writtenApply.landedBeyond). Such a commit is the room's to
	// store, as a browser's is, and the browser whose frame it landed is resynced only once it is
	// stored (resyncLanded), so its applied answer follows the store. The write's own part is
	// stored a second time beside it, which a load integrates as the nothing it is. Every other
	// update only the room holds, so it names its instance and is listed there until its append
	// has returned.
	landed := published && origin.landedBeyond(logged.Update) || remote && catchUp.landedBeyond(logged.Update)
	// The ask blocks this render first showed are attributed by the rule the credit below follows:
	// a catch-up's to none of this room's peers, since another process stored the rows that
	// introduced them, unless its drain landed a frame one of them parked, whose blocks are that
	// browser's (observeAskBlocksForUpdate).
	var attributed map[string]model.Actor
	if asks != nil {
		attributed = s.observeAskBlocksForUpdate(room, asks(), remote && !landed)
	}
	// A commit is credited as its origin says (originCreditsNoOne), and a server write's whose
	// drain landed a browser's parked frame as a browser's commit is: what it carries beyond its
	// writer's rows is that browser's edit, which no writer credited, so it is credited to the peers
	// connected as the room took it, as the browser update whose drain landed it would have been,
	// and a catch-up that landed it is this process's to settle.
	if landed || !s.originCreditsNoOne(logged.Origin) {
		if contentChanged {
			// The authors of the ask blocks this render first showed go with the credit to the
			// update's append, which records them durably (recordAskAuthors): a tree is rendered
			// only for a content change, so a browser's update that names an author always
			// carries a credit. A published live write's own carries none; its commit recorded
			// its blocks' authors (Ledger.recordSettlementCredit).
			class.credit = s.creditContentChange(room, &instance.hold, attributed)
		} else {
			class.roomCredit = s.unchangedCredit(room, &instance.hold)
		}
	}
	stored := (published || remote) && !landed
	if !stored {
		class.instance = instance
	}
	queued := s.queueUpdateClass(room, logged.Update, class, stored)
	if class.instance != nil {
		instance.listUnstored(queued)
	}
	// A published write's last update closes the window of the shadow its commit left
	// (reachShadow), before any update after it is queued.
	if published && origin.shadow != nil {
		s.reachShadow(room, origin.shadow, &instance.cursor, seq)
	}
	return true, contentChanged, !remote || landed
}

// writtenApply is what the origin of a server write's apply to a room - a published live write's
// (liveWriteOrigin) or a catch-up's (remoteUpdateOrigin) - keeps for the room's update observer to
// hold the apply's commit against (classifyDrained): written, the update the writer stored, and
// checks, how many of the room's apply check's runs on the apply passed (renderCheck.check),
// counted under the document's lock before the commit is logged.
type writtenApply struct {
	written []byte
	checks  atomic.Int32
}

// applyServerWrite applies update, a server write's, to live, a room's document, under origin,
// the write's own (writtenApplyOf), and then has live forget origin as the sender of anything the
// apply parked (crdt.Doc.ForgetParkedOrigin). A room's document discards a refused drain, and
// keeps the origin of each apply that parked something to name the senders the discard closes
// (leaveParkedOut, closeDiscardedSenders); only a browser is ever closed, and a server write's
// origin holds the update it applied, so kept there it would hold those bytes for as long as what
// the write parked stayed parked. What it parked stays parked. The forget takes live's lock,
// which the apply has released by the time it returns.
func applyServerWrite(live *crdt.Doc, update []byte, origin any) error {
	err := crdt.ApplyUpdateV1(live, update, origin)
	live.ForgetParkedOrigin(origin)
	return err
}

// landedBeyond reports whether update, the room's commit of the apply, carries content the writer
// did not store, which the room then stores itself (classifyDrained). Only the drain of the room's
// pending queue the apply ends with lands such content, and ygo runs the room's apply check on a
// remote apply once for what its update changed, where it changed anything, and once more where
// that drain landed anything (crdt.WithApplyCheck); a drain the check refuses is taken back, lands
// nothing, and its run is not counted. An update that changed the room leaves a clock of written
// in the commit, a struct it integrated or a deletion it made; so a commit that shares a clock
// with written, of an apply one run of the check passed, is one whose drain landed nothing. It
// carries written and only what written's own changes reach in the room: the deletion of what the
// room held under what written deletes or replaces - the text another editor typed, after the
// write's fork read, into the paragraph the write removes - and of written's structs that land
// under what the room had deleted. No row of written holds those deleted clocks, and a load makes
// the same deletions wherever the rows holding the clocks are stored, so that commit is stored by
// its writer's rows. Taken as carrying more, it was stored again, classed as the change the write
// had made, at a place in the history where it changes nothing
// (TestStoredOrderEditAfterTheForkReadIntoADeletedParagraph). Every other commit is held against
// written clock by clock (heldAgainst): one two runs of the check passed, whose drain landed
// something; one sharing no clock with written, whose update changed nothing, so that the one run
// was the drain's; and one the check never ran on, which only a room without the check makes.
func (w *writtenApply) landedBeyond(update []byte) bool {
	beyond, shares := heldAgainst(update, w.written)
	return beyond && (w.checks.Load() != 1 || !shares)
}

// writtenApplyOf is what origin, a server write's apply origin, keeps of its apply, and nil for
// any other origin.
func writtenApplyOf(origin any) *writtenApply {
	switch write := origin.(type) {
	case *liveWriteOrigin:
		return &write.writtenApply
	case *remoteUpdateOrigin:
		return &write.writtenApply
	}
	return nil
}

// heldAgainst reads update, the update a server write's commit made in a room, against written,
// the update the writer stored: beyond, whether update holds a struct's clocks or a deleted clock
// that written does not, and shares, whether it holds one written does. Each clock of update's
// structs is held against the clocks of written's, and each deleted clock against written's
// deleted clocks, clock by clock, so a struct the drain landed inside a gap of written's own
// clocks for a client - where written holds a Skip - is beyond it, as one past their end is. What
// the drain of the room's pending queue the commit ended with landed, a browser's frame parked
// behind the clocks the write supplied, is beyond written, and no row holds it (classifyDrained).
// So is a deletion written's own changes reach in the room, which is no content of the commit's
// own, so a commit whose drain landed nothing is not judged by beyond alone
// (writtenApply.landedBeyond). The two updates are read from their structs and delete sets, one
// pass over each, with no content decoded (eachV1Range), which costs a server write's commit the
// two passes over its bytes: a published write's, and a catch-up's of the rows another process
// stored, its browsers' keystrokes among them. A keystroke this process's room applies itself
// never reaches it. An update either of them cannot read is beyond and shares nothing, which has
// the room store the commit itself: a row too many, and nothing lost.
func heldAgainst(update, written []byte) (beyond, shares bool) {
	held, deleted := crdt.NewIDSet(), crdt.NewIDSet()
	if !eachV1Range(written, held.Add, deleted.Add) {
		return true, false
	}
	against := func(ranges map[crdt.ClientID][]crdt.IDRange) func(crdt.ClientID, uint64, uint64) {
		return func(client crdt.ClientID, clock, length uint64) {
			if beyond && shares {
				return
			}
			covered, met := rangesHold(ranges[client], clock, length)
			beyond, shares = beyond || !covered, shares || met
		}
	}
	if !eachV1Range(update, against(rangesByClient(held)), against(rangesByClient(deleted))) {
		return true, false
	}
	return beyond, shares
}

// rangesByClient is set's ranges by client, sorted and merged as the set keeps them.
func rangesByClient(set *crdt.IDSet) map[crdt.ClientID][]crdt.IDRange {
	clients := set.Clients()
	ranges := make(map[crdt.ClientID][]crdt.IDRange, len(clients))
	for _, client := range clients {
		ranges[client] = set.Ranges(client)
	}
	return ranges
}

// rangesHold reports whether ranges, sorted and merged as an IDSet keeps them, hold every clock
// from clock to clock+length (covered), and whether they hold any of them (met).
func rangesHold(ranges []crdt.IDRange, clock, length uint64) (covered, met bool) {
	if length == 0 {
		return true, false
	}
	at, _ := slices.BinarySearchFunc(ranges, clock, func(r crdt.IDRange, clock uint64) int {
		switch {
		case r.Clock+r.Len <= clock:
			return -1
		case r.Clock > clock:
			return 1
		}
		return 0
	})
	if at == len(ranges) || ranges[at].Clock >= clock+length {
		return false, false
	}
	return ranges[at].Clock <= clock && clock+length <= ranges[at].Clock+ranges[at].Len, true
}

// soleConnectedActor is the one actor connected to a room, and whether more than one distinct
// actor is: an update made while exactly one of them is connected can be pinned on that actor; one
// made while several are cannot be pinned on any single one of them.
func soleConnectedActor(connected map[uint64]model.Actor) (sole *model.Actor, ambiguous bool) {
	for _, actor := range connected {
		if sole == nil {
			sole = new(actor)
		} else if actorKey(actor) != actorKey(*sole) {
			ambiguous = true
		}
	}
	if ambiguous {
		sole = nil
	}
	return sole, ambiguous
}

// observeAskBlocksForUpdate records askBlocks, the ask blocks an update left in room's document,
// and attributes each one not already recorded (observeAskBlocks), in the same critical section
// the update observer classifies the update in (renderedReplica.observe): the observer classifies
// each update in the order the replica took them, so the room's record of its ask blocks moves
// forward only. The walk that lists those ask blocks is the asks function the replica's measure
// of the update answers with (changedBy), which takeLocked hands the replica's drainClassifier,
// classifyDrained, the caller here: that measure answers one only for an update it found to be a
// real change, so the no-op path, the common one, pays for neither. A nil askBlocks is that
// walk's answer when it could not read the document, never a document holding no ask block
// (askBlockIDs): the room's record is left as it is. It returns the authors it named for the
// blocks no write registered, which the update's append records.
//
// elsewhere says the blocks were introduced on another process: a catch-up's commit of the rows
// that process stored, whose drain landed nothing a browser of this room parked. None of this
// room's connected peers wrote them, so they are recorded with no author here, as the blocks a
// room loads with are, and a settlement finds the author that process recorded
// (askBlockSources.author). A catch-up whose drain landed a frame one of this room's browsers
// parked is not elsewhere's: what it carries beyond the rows is that browser's edit, so its blocks
// go to the browsers connected as the room took it, as the browser update whose drain landed the
// frame would have named them, the rule its credit follows (classifyDrained).
func (s *Service) observeAskBlocksForUpdate(room string, askBlocks map[string]struct{}, elsewhere bool) map[string]model.Actor {
	if askBlocks == nil {
		// The walk could not read the document it had to walk, and logged that
		// (renderedReplica.spliceTouched): the room keeps the ask blocks and the authors it
		// recorded, as it does for an update whose own render failed, which is handed no asks
		// at all.
		return nil
	}
	state := s.lockState(room)
	defer s.unlockState(room, state)
	var author *model.Actor
	if elsewhere {
		author = nil
	} else if sole, ambiguous := soleConnectedActor(state.connected); ambiguous {
		author = new(SettlementActor)
	} else {
		author = sole
	}
	return state.observeAskBlocks(askBlocks, author)
}

// originCreditsNoOne is whether a commit's origin names nobody to credit for the update, the one
// rule classifyDrained weighs a commit by before either credit path (creditContentChange,
// unchangedCredit). Four origins credit no one. A service repair's origin, the one
// serviceTransact registered: the service made that commit, and a browser that was only connected
// while it happened is not credited for it. A published live write's *liveWriteOrigin:
// Ledger.Commit credited that write when its transaction committed, and it is not credited again.
// A catch-up's *remoteUpdateOrigin: another process stored that update, and its append recorded
// the authors that process credited it to, so none of this room's peers may be named for it on
// either path. And no origin at all: every commit a loaded room makes names an origin - a
// browser's update names the peer ygo applied it for (its *peer, ygo's provider/websocket
// peer_update.go), a service mutation names the sentinel ygo's Server.Apply makes for the call
// (applyOriginSentinel), which serviceTransact registers as a service origin, a repair names its
// own *identityClosureOrigin (applySuppressed), a published live write its *liveWriteOrigin and
// a catch-up its *remoteUpdateOrigin. The room's own load
// decodes the stored history with no origin (bootstrapLoadedState), earlier in the load hook
// that goes on to open the update log and register this observer, so no observer ever sees it. A
// nil origin here can therefore only be ygo's own cleanup transaction, which a document commits
// for itself after a remote delete leaves a text's formatting markers covering no text
// (reearth/ygo #284): nobody typed it, so neither credit path may name anyone for it. On the
// replica's recover path, where every update it could not place counts as a content change,
// crediting it would hand every connected browser an edit none of them made (INFRA-644); on the
// shadow path, a cleanup the room measured as no change is stored as a change wherever an open
// write's shadow measures one (storedClasses), which would store those browsers as its authors.
// A server write's commit whose drain landed a frame one of this room's browsers parked is
// credited as a browser's commit all the same (classifyDrained): what it carries beyond its
// writer's rows is no writer's.
func (s *Service) originCreditsNoOne(origin any) bool {
	if origin == nil {
		return true
	}
	switch origin.(type) {
	case *liveWriteOrigin, *remoteUpdateOrigin:
		return true
	}
	_, service := s.serviceOrigins.Load(origin)
	return service
}

// creditContentChange credits an observed content change to the peers connected to room: a
// browser edit by one of them, which ygo applies while that peer's connection is registered, or
// the commit of a server write whose drain landed a frame one of them parked (classifyDrained),
// which weighs whether a commit is credited at all first (originCreditsNoOne). ygo does not say
// which connection sent it, so every connected peer is credited with it: when exactly one is
// connected it is the latest edit source and replaces `lastActor`, and otherwise the edit cannot
// be pinned on a single peer and no older actor may stand in for it.
//
// The credit goes to F, the room's in-flight credits, until the edit's append lands it
// (AppendUpdatesWithCredit): the returned UpdateCredit is how that append reaches it, and how
// askAuthors, the authors of the ask blocks the edit's render first showed (observeAskBlocks),
// reach the durable record in the same append. This takes no advisory lock and touches no
// database: nothing reads F without holding state.mu.
func (s *Service) creditContentChange(room string, hold *roomHold, askAuthors map[string]model.Actor) *UpdateCredit {
	state := s.lockState(room)
	defer s.unlockState(room, state)
	record := connectedCreditLocked(state)
	record.seq = state.creditSeq.Add(1)
	state.inflight[record.seq] = record
	state.lastActor = record.lastActor
	state.unsettled = true
	return &UpdateCredit{service: s, room: room, state: state, record: record, hold: hold, askAuthors: askAuthors}
}

// unchangedCredit is the credit a browser update the room's update observer measured as no change
// carries for the room's shadow (documentUpdateClass.roomCredit): the peers connected as the room
// took it, the authors creditContentChange would credit the same commit to, recorded only while a
// live write is open, whose shadow may measure the update a change once the write is stored before
// it. It is not in F and takes no credit sequence: no version lists an update recorded as no
// change, and its append writes these authors only when it stores the update as a change. The
// observer weighs the origin first by the rule that path follows (originCreditsNoOne), so a
// service repair, a published live write Ledger.Commit credited already and a commit with no
// origin at all - ygo's own cleanup, which nobody typed - carry no authors for a shadow to store
// them with either. Nil too while no write is open.
func (s *Service) unchangedCredit(room string, hold *roomHold) *UpdateCredit {
	state := s.lockState(room)
	defer s.unlockState(room, state)
	if state.liveWriter == nil {
		return nil
	}
	return &UpdateCredit{service: s, room: room, state: state, record: connectedCreditLocked(state), hold: hold}
}

// connectedCreditLocked is a credit record naming the peers connected to state, and the one of
// them it pins the edit on, if one. The caller holds state.mu.
func connectedCreditLocked(state *roomState) *inflightCredit {
	record := &inflightCredit{authors: make(map[string]model.Actor, len(state.connected))}
	for _, actor := range state.connected {
		mergeAuthor(record.authors, actor)
	}
	record.lastActor, _ = soleConnectedActor(state.connected)
	return record
}

// registerConnectionLocked registers a browser connected to the room whose state is state, which
// the caller holds locked. It is credited only with browser edits observed while it is connected
// (creditContentChange), never for connecting or for an agent's edit. It counts nothing: the
// browser's count (connections) is the place under the connection cap reserveConnection took for
// it before, which removeConnection gives back as it forgets the browser. authorize registers each
// document socket's browser through it.
func (s *Service) registerConnectionLocked(state *roomState, id uint64, actor model.Actor) {
	state.connected[id] = actor
	forgetMarkAuthorDeparture(state, actor)
}

// removeConnection forgets a browser that left room, and with the last one, once ygo has retired
// the room, the room's state (releaseIfUnusedLocked). A document without state has none to forget.
// The service's count of connections drops only for an id the record held: a failed room's
// browsers are found on the record that replaced it (handOverFailedRoomLocked), so each leaves
// the count once. The departure of the last browser of an actor is when the unrecorded marks that
// actor wrote start to age (sweepUnrecordedMarks).
func (s *Service) removeConnection(room string, id uint64) {
	state := s.lockExistingState(room)
	if state == nil {
		return
	}
	actor, registered := state.connected[id]
	delete(state.departedPeers, id)
	if registered {
		delete(state.connected, id)
		s.connections.Add(-1)
		recordMarkAuthorDeparture(state, actor, s.now())
	}
	s.unlockState(room, state)
}

// settleLastPeer runs the settlement a room's last browser leaving owes now rather than when its
// timer fires. It takes over the timer's settleWG count, so Shutdown joins it as it would the
// timer's settlement: Shutdown reports which documents settled, and stops ygo, only once it has
// returned. It counts itself as running (settling) from the moment it takes the timer, as the timer
// it replaces counted as armed: a write that opens before the settlement reaches its own count
// then takes the settlement over (openLiveWrite) rather than ending it with nobody to arm it again.
// First it marks every browser the room's state names as connected departed: ygo has none left in
// the room, and their handlers forget them only once this returns (departedPeers).
// ygo can run this after a close of the room has returned (sjawhar/ygo v1.55.1-sami.2,
// provider/websocket/inject.go:483-485), so its settlement may load the document into a room of
// its own, which its mark records as the settlement's (withSettlementLoad). The departing browser
// still holds the room's record here, registered and admitted, and lets go of it only as its
// socket ends, which is where that room is retired unless someone took it up meanwhile
// (ServeHTTP, retireSettlementRoom).
func (s *Service) settleLastPeer(_ context.Context, room string) {
	state := s.lockExistingState(room)
	if state == nil {
		return
	}
	for id := range state.connected {
		if state.departedPeers == nil {
			state.departedPeers = make(map[uint64]struct{})
		}
		state.departedPeers[id] = struct{}{}
	}
	if !s.takeSettleTimer(state.settle) {
		s.unlockState(room, state)
		return
	}
	generation := state.roomGeneration
	state.settling++
	s.unlockState(room, state)
	defer s.settleWG.Done()
	defer func() {
		state.mu.Lock()
		state.settling--
		s.unlockState(room, state)
	}()
	s.followUpSettlement(room, s.settleRoomWithin(withSettlementLoad(context.Background()), room, generation, false))
}

// loadDecoded is LoadDoc's own read of room's stored state and its decode
// (loadDecodedHistory), both under slot, the load's (loadedState.slot): the read's fold of a
// history of several updates takes it where it reads the rows (foldRead), and the decode runs
// inside it, as the room's load hook's decode into the room's own document does
// (bootstrapLoadedState). ygo hands LoadDoc no context, so the wait has no deadline and holds
// no slot meanwhile (loadSlot.take), and the slot is the caller's to give back rather than the
// helper's.
//
// A history too large for one update is refused from its header, before the slot is taken and
// before anything is decoded (loadableHistory): no room could take it, and the refusal names the
// item count the header declares rather than whatever a decode of those bytes would say. A
// history ygo refuses the bytes of is not refused here: the state goes on the record with no copy
// decoded from it, and the room's own decode of the same bytes fails the room (onLoadDocument,
// bootstrapLoadedState), so a history that does not decode fails where the room decodes it and
// not one step earlier. The replica of such a room is never made, so its missing copy costs
// nothing.
func (a *servicePersistenceAdapter) loadDecoded(room string, slot *loadSlot) (persistence.LoadResult, decodedState, error) {
	history, err := loadDecodedHistory(slot.carriedBy(context.Background()), a.store, room, a.service.maxPendingItems, loadableHistory)
	if err != nil {
		return persistence.LoadResult{}, decodedState{}, err
	}
	return history.loaded, history.decoded, nil
}

package docs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// settleStep is what a settlement's stage leaves for the stage after it (settleRoomWithin).
type settleStep int

const (
	// settleDone ends the settlement, with the outcome the stage reports beside it: the stage
	// committed the document's version, stopped short, or left the document to whatever owns its
	// settlement now.
	settleDone settleStep = iota
	// settleOn is no ending: the stage left the settlement going on, and the outcome beside it is
	// nothing. Only settleRoomWithin reads it; a settlement never ends on it.
	settleOn
	// settleStampOnly is the one stage ending that names the stage after it: the document moved
	// under the settlement's read, so the block-id stamp applyStamp wrote into the room is all
	// this settlement commits, and settleRoomWithin goes on to commitStamp and to no stage after
	// it. Only applyStamp answers it, and the outcome beside it is nothing.
	settleStampOnly
)

// settlement is one document's settlement, as its stages run it (settleRoomWithin): the room it
// settles, that room's state and the live instance it holds for the whole settlement, the room
// generation it stands at, the transaction and ledger its stages write through, what they read of
// the document and whom its version credits, what they wrote into the room, and how the settlement
// reports a stop short. The stages hand each other everything here; what one stage alone uses
// stays a local of that stage.
type settlement struct {
	service    *Service
	room       string
	state      *roomState
	doc        *crdt.Doc
	generation uint64
	// stoppedShort is the outcome of a settlement that stopped short with err having written
	// nothing into the room (settleRoomWithin).
	stoppedShort func(error) settleOutcome
	// cursor is the cursor of the instance this settlement holds (doc), which gates the room
	// updates it stores and its repairs, and takes them in once they are stored
	// (commitSettlement).
	cursor *roomCursor
	ledger *Ledger
	tx     pgx.Tx
	owner  artifactOwner
	latest struct {
		model.Version
		markdown         string
		docUpdateVersion int64
	}
	// issueClosed is the document's issue row as this settlement's transaction read it under the
	// issue's owner lock (lockArtifactOwner): true for a closed issue, false for an open one and
	// for a document of no issue, so its zero value is open. A closed issue's document settles as
	// an open one's does, and every rule of the settlement that differs on a closed issue goes by
	// this row, never by the room's record of the issue (roomState.closed): a close this process
	// has not heard of yet leaves that record open.
	issueClosed bool
	// first is the settlement's read of the room before its repairs, read at an exact commit
	// position; read is that read's tree, and markdown, offsets and renderErr are what it renders
	// as, which the version reuses when it is rendered from this same tree unchanged
	// (commitVersion).
	first     settlementRead
	read      *pmdoc.Node
	markdown  string
	offsets   []pmdoc.BlockOffset
	renderErr error
	credit    settlementVersionCredit
	// slots holds a suppression slot for each update this settlement writes into the room, in the
	// order the room's persistence worker is handed them, and updates holds those updates. Every
	// path after a write finishes or discards all of them. stamp is the block-id stamp this
	// settlement wrote into the room, nil for none; it is stored with the settlement's other
	// appends, after the reconciliation (storeRepair).
	slots   []*suppressSlot
	updates [][]byte
	stamp   []byte
	// renamed are the ask blocks the stamp renamed, and carried is whom each of them carried as
	// the room's state held it before the stamp (recordStampedAskBlocks). Both are recorded
	// durably in the transaction that stores the stamp, whichever way it commits
	// (recordStampedAskAuthors).
	renamed []stampedAsk
	carried map[string]model.Actor
	// reconciled is the tree the ask blocks are reconciled on, the stamp's when it stamped ids;
	// versioned is the one the version is rendered from, read again after the repairs when the
	// settlement wrote any.
	reconciled     *pmdoc.Node
	versioned      *pmdoc.Node
	reconciliation settlementReconciliation
	// readHead is the head of the document's stored history this settlement read under the
	// document's lock, before it stored anything (readLocked). snapshotCursor is the head the
	// version records (doc_update_version), through which every one of the room's updates it holds
	// is stored; it equals readHead when the settlement stored no update (savedNothing).
	readHead       int64
	snapshotCursor int64
	// published are the events this settlement appended, published once its transaction commits.
	published []model.Event
}

// savedNothing reports whether this settlement wrote nothing that a later move of an approval
// request reads, given whether it writes a version (versioning): it writes no version, stores no
// update in the document's history - none of the room's updates, no block-id stamp and no ask- or
// check-block repair, so the head it leaves is the head it read (readHead) - and lists no author,
// neither a pending author nor an in-flight credit. A reopen's move reads the latest version and
// the editor kept for it (FollowClosedIssueVersions); what else such a settlement writes - block
// events in SettlementActor's name, the settled version's number in their payloads, the recorded
// author of an unindexed ask block - changes neither. A settlement armed while another one commits,
// as the resumption's pick of the row that one is about to delete arms it, finds nothing owed and
// is such a settlement.
func (st *settlement) savedNothing(versioning bool) bool {
	return !versioning && st.snapshotCursor == st.readHead && len(st.credit.authors) == 0
}

// skipUnreadableSettlement reports whether err stops a settlement at a document whose tree it cannot
// read, and logs it: a tree outside the Proof schema, which a replacement from markdown repairs, or
// an element this build does not know (ErrDocNewerContent), which a newer build wrote while two
// builds serve one database - in the tree, or in the version a human approved, without which the
// version write cannot tell whether his approval carries (carryApproval). The settlement writes no
// version and records no failure, and the document stays unsettled for a later settlement - a
// repair's, or one on a build that reads the element, which the log names.
func skipUnreadableSettlement(room string, err error) bool {
	switch {
	case errors.Is(err, ErrDocNewerContent):
		slog.Warn("dispatch: settle document holding an element this build cannot read; no version written; a build that reads the element writes it", "room", room, "error", err)
	case errors.Is(err, ErrDocSchema):
		slog.Error("dispatch: settle document outside Proof schema", "room", room, "error", err)
	default:
		return false
	}
	return true
}

// settleRoomWithin settles room under parent and reports how the settlement ended (settleOutcome),
// leaving what follows to its caller. A timer's settlement runs under no deadline; the one
// Shutdown runs gets its drain budget. When parent ends first the settlement's database work is
// cancelled and its transaction rolls back, and nothing retries it or counts it as a failure: the
// document's pending-settlement row leaves it to resume (onLoadDocument, RunSettlementResumption).
// So does a settlement that has to write into a room Shutdown has begun closing, which refuses the
// write (allowInject).
//
// Once Shutdown has begun closing room, a settlement loads no room of it - a room Shutdown closed
// stays closed, its settlement left to resume - unless mayLoad says this is Shutdown's own
// settlement of a document whose settlement this process had armed: the timer Shutdown stopped
// would have loaded the room, and a cold write (publishLiveWrite) arms one with no room resident.
//
// The settlement itself does only what no caller can do after it: a settlement that wrote into
// the room and cannot finish fails the room, whose reload arms the settlement again, and one whose
// room holds a replaced stored history closes the room for a reload (reloadReplacedRoom). The
// retries, the re-arming and the shutdown's log lines are its caller's: the timer's settlement and
// Shutdown's do them (followUpSettlement), and a caller that runs it synchronously and answers for
// its outcome itself reports the reason instead (settleOwedNow).
//
// A closed issue's document settles as an open one's does. Nothing that arms a settlement, or
// decides whether one goes on, reads whether the issue is closed. The settlement reads the issue's
// state only to decide what it writes, and only from the issue row its transaction locks
// (issueClosed). The issue's close ends a settlement it finds armed or running only as every move
// of the room's generation does (closeRecordLocked), and the document's pending-settlement row
// leaves that settlement to resume. The one settlement a closed issue ends besides is one whose
// room took an update the append gate refused because the issue closed more than closeMargin
// before the room took it: that room holds text the store never will, so the settlement saves
// nothing of it, fails no room and counts no failure, and the room's close and the resumption
// after it settle the document from the store (endOnRefusedUpdate).
//
// A removed document owes no settlement: its removal (api.removeArtifact) deletes its versions,
// its stored updates and its pending-settlement row with its artifacts row, and closes its room on
// every process once it has committed. A settlement already past its generation check when that
// commit lands - one that queued behind the removal for the document's owner lock, or that started
// before this process's eviction or another process's announcement reached its room - stops short
// on the row that has gone: the latest version, the owner row, or the room's load reads nothing.
// It ends there, saying nothing and retrying nothing, so no retry warms a room over the row
// (documentRemoved).
//
// The settlement runs in stages, each of which answers settleOn where the settlement goes on and
// the step and outcome it ends on otherwise: it takes the room's live instance (take), reads the
// document under the document's locks (readLocked), stamps the block ids the document lacks
// (applyStamp) and commits that stamp alone when the document moved under its read (commitStamp),
// reconciles the ask blocks (planRepairs), writes the repairs into the room and stores what it
// wrote (commitRepairs), and commits, with the document's version when the document reads
// differently from the latest one (commitVersion).
func (s *Service) settleRoomWithin(parent context.Context, room string, generation uint64, mayLoad bool) settleOutcome {
	settle := &settlement{service: s, room: room, generation: generation}
	// stoppedShort is the outcome of a settlement that stopped short with err having written
	// nothing into the room: a retry, unless parent has ended, the document has been removed or
	// Shutdown refused the write. The generation it counts that retry's failure against is the one
	// the settlement stands at now, which its take reads again once the room is warm. An err that
	// is the append gate's refusal of an update the room took after its issue closed is no fault
	// to retry: it ends the settlement (endOnRefusedUpdate).
	settle.stoppedShort = func(err error) settleOutcome {
		if errors.Is(err, ErrIssueClosed) {
			return settle.endOnRefusedUpdate()
		}
		next := settleRetry
		switch {
		case parent.Err() != nil:
			next = settleOutOfBudget
		case settle.documentRemoved(parent):
			return settleOutcome{}
		case s.shuttingDown(room) && errors.Is(err, ErrServiceUnavailable):
			next = settleRefusedInShutdown
		}
		return settleOutcome{next: next, err: err, generation: settle.generation}
	}
	if s.beforeSettleStart != nil {
		s.beforeSettleStart(room)
	}
	state := s.lockState(room)
	// A retirement that has reserved the record (roomState.retiring) is closing a room no one
	// holds: the settlement takes the room once that close has returned - the same room where ygo
	// refused the close, a fresh load of it where it closed.
	for retiring := state.retiring; retiring != nil; retiring = state.retiring {
		s.unlockState(room, state)
		if err := s.awaitRetirement(parent, room, retiring); err != nil {
			return settle.stoppedShort(err)
		}
		state = s.lockState(room)
	}
	if s.stopping.Load() || state.failed != nil || state.roomGeneration != generation {
		s.unlockState(room, state)
		return settleOutcome{}
	}
	state.settling++
	s.unlockState(room, state)
	defer func() {
		state.mu.Lock()
		state.settling--
		s.unlockState(room, state)
	}()
	settle.state = state
	if step, outcome := settle.take(parent, mayLoad); step != settleOn {
		return outcome
	}
	ledger := &Ledger{service: s, ownTx: true}
	settle.cursor = s.cursorOf(settle.doc)
	ctx := withRoomCursor(store.WithTransactionTracking(withLedger(parent, ledger)), settle.cursor)
	tx, err := s.store.Pool.Begin(ctx)
	if err != nil {
		return settle.stoppedShort(fmt.Errorf("begin document transaction: %w", err))
	}
	defer tx.Rollback(ctx)
	// Settlement decides everything before it writes anything: every write of the stages below
	// goes through write and queues on this ledger, and the queue runs back to back right before
	// the commit, so the transaction holds a transaction id only for those statements, not while
	// it renders the document, walks its anchors or carries an approval.
	ledger.writer = tx
	settle.ledger, settle.tx = ledger, tx
	owner, open, err := lockArtifactOwner(ctx, tx, room)
	if err != nil {
		return settle.stoppedShort(err)
	}
	// The issue's state is read under the owner's lock this transaction now holds, in the
	// statement after it (lockArtifactOwner), not the room's record of it: a close takes that lock
	// too, so a close this process has not heard of yet is read here, and no close commits until
	// this settlement's transaction ends.
	settle.owner, settle.issueClosed = owner, !open
	if err := lockSettlementFence(ctx, tx, room); err != nil {
		return settle.stoppedShort(err)
	}
	// The owner is read under its lock in this transaction, so the repairs settlement injects
	// below do not read it again: that read would be a second pooled connection taken while
	// this transaction holds one, which is what deadlocks the pool.
	ctx = withOwnerVerified(ctx)
	if step, outcome := settle.readLocked(ctx, parent); step != settleOn {
		return outcome
	}
	switch step, outcome := settle.applyStamp(ctx); step {
	case settleStampOnly:
		// The document moved under the settlement's read, so the stamp in the room is all it
		// commits. Committing it here rather than below is what keeps the copy lock
		// reconcileAskBlocks takes out of the transaction that holds the stamp's pending row.
		_, stamped := settle.commitStamp(ctx)
		return stamped
	case settleDone:
		return outcome
	}
	if step, outcome := settle.planRepairs(ctx); step != settleOn {
		return outcome
	}
	if step, outcome := settle.commitRepairs(ctx); step != settleOn {
		return outcome
	}
	_, outcome := settle.commitVersion(ctx)
	return outcome
}

// lockSettlementFence queues the take of the document's document_block_versions row `for no key
// update`, the revision fence's row (R23 of this change), as the settlement's first write: right
// after its owner lock, and ahead of every statement the settlement queues, so the write phase
// takes the row before anything else it writes. Whether a settlement writes a version is decided
// only in commitVersion, after reconcileAskBlocks has opened the asks of new ask blocks and
// followed each one for a session author, which writes that session's node (asks.Follow); the
// version's own fence - the version row's trigger (document_block_versions_owe, migration 0122)
// and refs.BlockIndexing's own - comes after that node. The lock order written on
// api/inbox_read.go's markInboxReadThreads has a writer under the fence take its row before any
// node, tuple or projection row it writes, so every settlement takes it here, since any of them
// may write a version. Those later takes of the same row in this transaction wait on nothing.
// Every document holds the row from its first version on; a document with none has no row, and
// the insert of its first version makes it. The take is queued rather than run, as every write of
// the settlement is: a row lock assigns the transaction its id, which the read phase holds none
// of.
func lockSettlementFence(ctx context.Context, tx pgx.Tx, artifactID string) error {
	return writeExec(ctx, tx, fmt.Sprintf("take the revision fence of document %s", artifactID), `
		select 1 from document_block_versions where artifact_id = $1 for no key update
	`, artifactID)
}

// take is what the settlement takes of its room before it opens a transaction: the room warmed
// where no browser holds it, the live instance it settles, and the generation that instance stands
// at. mayLoad is its caller's answer for a document Shutdown has begun closing: a room of one
// stays closed unless this is Shutdown's own settlement of a document whose settlement this
// process had armed (settleRoomWithin).
func (st *settlement) take(parent context.Context, mayLoad bool) (settleStep, settleOutcome) {
	s, room, state := st.service, st.room, st.state
	shuttingDown := s.shuttingDown(room)
	if shuttingDown && !mayLoad && s.srv.GetDoc(room) == nil {
		return settleDone, settleOutcome{}
	}
	if s.srv.GetDoc(room) == nil {
		state.mu.Lock()
		state.settleWarming = true
		s.unlockState(room, state)
		// The warm-up carries the service's own mark (withOwnerVerified), so allowInject does not
		// read the issue for it: it applies nothing, and a closed issue's document settles as an
		// open one's does, so the refusal a closed issue makes of a caller's injection is not
		// this load's to meet. A late departure's settlement hands it the settlement's mark too,
		// through parent (settleLastPeer, withSettlementLoad).
		warmCtx := withOwnerVerified(parent)
		if shuttingDown {
			warmCtx = withShutdownLoad(warmCtx)
		}
		err := s.srv.Apply(warmCtx, room, func(_ *crdt.Doc, _ func(func(*crdt.Transaction))) {})
		state.mu.Lock()
		state.settleWarming = false
		s.unlockState(room, state)
		if err != nil && !errors.Is(err, websocket.ErrNoChanges) {
			return settleDone, st.stoppedShort(fmt.Errorf("warm document for settlement: %w", err))
		}
	}
	// Hold the live document for the whole settlement: reading the room again later could return
	// nil, and every later stage would dereference it. A nil here is a close that retired the room
	// since the generation check in settleRoomWithin or the warm-up above, and the retry this
	// settlement stops short into decides by the room's generation (retrySettleLocked). A close
	// that moved it - an eviction, a room's failure, a close of a room whose stored history was
	// replaced, an issue's first close of the record - ended this arming: the retry counts nothing
	// and arms nothing, and the document is left to what that close arranged, a reload, the
	// document's next load or the resumption. A close that did not - ygo's idle sweep, the close
	// of the room at its issue's reopen, a repeated close of a record that already reads closed -
	// is followed by no load that arms the settlement, and this settlement took the arming it ran
	// on, so the retry arms it again, and that settlement warms the room.
	if s.afterSettleWarm != nil {
		s.afterSettleWarm(room)
	}
	doc := s.srv.GetDoc(room)
	if doc == nil {
		return settleDone, st.stoppedShort(fmt.Errorf("hold the room to settle: %w", errRoomReplaced))
	}
	st.doc = doc
	// The settlement waits for none of the room's updates to reach its persistence: it stores
	// every update of the room through the point it reads the room at itself, under the document's
	// advisory lock (readSettlementTree), and an update the room commits after that point is no
	// part of its version. A wait for the room's worker could only wait on that same lock.
	state.mu.Lock()
	st.generation = state.roomGeneration
	s.unlockState(room, state)
	return settleOn, settleOutcome{}
}

// keep records a suppression slot and the update it suppresses, in the order the room's
// persistence worker is handed them.
func (st *settlement) keep(slot *suppressSlot, update []byte) {
	if slot != nil {
		st.slots, st.updates = append(st.slots, slot), append(st.updates, update)
	}
}

// finishSlots hands the room's persistence every update this settlement wrote into the room, now
// that its transaction holds them.
func (st *settlement) finishSlots() {
	for index, slot := range st.slots {
		st.service.finishSuppressedPersistence(slot, st.updates[index])
	}
}

// abandon ends a settlement that cannot finish. What it wrote is in the room and its browsers
// and will never reach the store through this settlement, so its slots are discarded and the
// room fails, which reloads the document from the store and leaves its settlement to that load.
// A settlement that wrote nothing stops short as one that met any other fault does. Its
// transaction rolls back, so the authors it read stay where they were, it marked no in-flight
// credit, and the room updates it stored are left to the room's persistence.
//
// A store write the append gate refused because the document's issue closed (ErrIssueClosed) is
// the first order of a refused update, and fails no room whether or not the settlement wrote
// into it: the room closes for its issue instead (endOnRefusedUpdate).
func (st *settlement) abandon(err error) settleOutcome {
	if errors.Is(err, ErrIssueClosed) {
		return st.endOnRefusedUpdate()
	}
	if len(st.slots) > 0 {
		st.service.discardSuppressedPersistence(st.room, st.slots...)
		st.service.failRoom(st.room, err)
		return settleOutcome{next: settleRoomFailed, err: err}
	}
	return st.stoppedShort(err)
}

// endOnRefusedUpdate ends a settlement whose room took an update the append gate refused because
// the document's issue closed more than closeMargin before the room took it (ErrIssueClosed,
// observedBeforeClose). That update is in the room's tree and never in the store, so a version of
// that tree would save text the closed issue refused. The settlement therefore saves nothing of
// this room: it discards the updates it wrote into the room, fails no room, starts no retry and
// counts no failure, and its transaction rolls back, which keeps the document's
// pending-settlement row. The room closes for its issue (refuseClosedIssueAppend), so its
// browsers reconnect read-only to a room loaded from the store, and the resumption settles the
// document from there (resumeOwedSettlements).
//
// The refusal reaches a settlement in one of two orders. In the first, the settlement's own store
// write meets it: its flush of the room's updates (Ledger.flushThrough, storeRepair) answers
// ErrIssueClosed, and abandon or stoppedShort ends here. In the second, the room's persistence
// worker met it first, took the room instance's mark (roomInstance.issueCloseStarted) and dropped
// the update from the room's queue, so no flush of this settlement stores or refuses it, and the
// settlement ends here on the mark (roomRefusedUpdate). Whichever meets the refusal first takes
// that mark and starts the room's close; the call here starts nothing when the mark is taken.
func (st *settlement) endOnRefusedUpdate() settleOutcome {
	s := st.service
	s.discardSuppressedPersistence(st.room, st.slots...)
	s.refuseClosedIssueAppend(st.room, s.instanceOf(st.doc))
	slog.Info("dispatch: document settlement ended on a room update its closed issue refused; the room closes and the document settles from the store",
		"room", st.room)
	return settleOutcome{}
}

// roomRefusedUpdate is whether the room instance this settlement holds took an update the append
// gate refused because the document's issue closed (roomInstance.issueCloseStarted): the second
// order of a refused update (endOnRefusedUpdate), in which the room's persistence worker met the
// refusal first.
//
// The settlement reads it twice: once it holds the document's advisory lock (readLocked, after
// lockSettlementCursor), and after its last store write, before its commit (commitVersion,
// commitStamp). The worker's append takes that same lock, and the settlement holds it from the
// first read to its commit, so no append of the room is refused between the two reads. A refusal
// the worker made before the settlement took the lock is the one the first read can miss: the
// worker takes the mark after its refused transaction has released the lock. But it takes the
// mark before it drops the update from the room's queue (StoreUpdateContext, finishUpdateClass).
// So when the settlement stores the room's updates, either that update is still queued, and the
// gate refuses the settlement's own write (the first order), or it has gone, and its mark was
// taken before, which the second read finds.
func (st *settlement) roomRefusedUpdate() bool {
	instance := st.service.instanceOf(st.doc)
	return instance != nil && instance.issueCloseStarted.Load()
}

// commitFailed ends a settlement whose commit returned an error. Its outcome is unknown, and the
// room's cursor took in the versions it appended (commitSettlement), so a room it stored any
// update of fails too, and reloads the durable document whichever way the commit went.
func (st *settlement) commitFailed(advanced bool, err error) settleOutcome {
	if advanced && len(st.slots) == 0 {
		st.service.failRoom(st.room, err)
		return settleOutcome{next: settleRoomFailed, err: err}
	}
	return st.abandon(err)
}

// applyStamp stamps the block ids the settlement's read lacks into the room; a settlement whose
// read lacks none writes nothing here. It is also where the settlement meets a room gone or a
// document moved under its read, under the one hold of the room's state that records the ask
// blocks the stamp renamed. A settlement that holds a stamp in the room and meets a document
// that moved under its read ends here with settleStampOnly, which goes on to commitStamp and to
// nothing after it; any other goes on holding the ask blocks' sources read under that state.
func (st *settlement) applyStamp(ctx context.Context) (settleStep, settleOutcome) {
	s, room, state := st.service, st.room, st.state
	st.reconciled = st.read
	if pmdoc.BlockIDRepairCount(st.read) > 0 {
		slot, update, err := s.applySuppressed(ctx, room, st.doc, func(doc *crdt.Doc, origin any) error {
			var stampErr error
			st.reconciled, _, st.renamed, stampErr = stampBlockIDs(doc, origin)
			return stampErr
		})
		st.keep(slot, update)
		if err != nil {
			if len(st.slots) == 0 && skipUnreadableSettlement(room, err) {
				return settleDone, settleOutcome{}
			}
			return settleDone, st.abandon(err)
		}
		if slot != nil {
			st.stamp = update
		}
	}

	state.mu.Lock()
	// The authors the stamp carried are recorded durably in this transaction, the one that stores
	// the stamp, whichever way it commits below (recordStampedAskAuthors).
	st.carried = state.recordStampedAskBlocks(st.renamed)
	if s.stopping.Load() || state.failed != nil {
		s.unlockState(room, state)
		s.discardSuppressedPersistence(room, st.slots...)
		return settleDone, settleOutcome{}
	}
	// An open live write is not in the room yet, and since this settlement holds the owner's lock,
	// the write's transaction has already committed: the cursor this settlement versions against
	// includes its row. Write no version; finishLiveWrite arms a settlement once it is published.
	superseded := state.roomGeneration != st.generation
	if state.liveWriter != nil {
		state.settleDeferred = true
		superseded = true
	}
	if superseded {
		s.unlockState(room, state)
		if len(st.slots) == 0 {
			return settleDone, settleOutcome{}
		}
		return settleStampOnly, settleOutcome{}
	}
	// The ask blocks' sources are read now, after the tree they are reconciled on: an update in that
	// tree whose observer has already run has recorded who introduced its blocks, and one whose
	// observer has not left its blocks unseen, for the settlement that observer arms (askBlockSources).
	st.credit.askSources = state.askSources()
	state.mu.Unlock()
	// Queued before reconcileAskBlocks reads the durable authors, so a block the stamp renamed
	// is found under its minted id there too: the write phase has not run, so that read takes the
	// rows this queued from the ledger (transactionRecordedAskAuthors).
	if err := recordStampedAskAuthors(ctx, st.tx, room, st.renamed, st.carried); err != nil {
		return settleDone, st.abandon(err)
	}
	return settleOn, settleOutcome{}
}

// commitStamp commits the block-id stamp of a settlement the document moved under
// (settleStampOnly): that stamp is in the room and its browsers, so it is stored and broadcast and
// its transaction committed with no version, and the settlement ends with it.
func (st *settlement) commitStamp(ctx context.Context) (settleStep, settleOutcome) {
	s, room, tx := st.service, st.room, st.tx
	if err := st.ledger.storeRepair(ctx, tx, room, st.cursor, st.stamp); err != nil {
		return settleDone, st.abandon(fmt.Errorf("store superseded document identity update: %w", err))
	}
	// The second read of a refused update's second order (roomRefusedUpdate), after the last
	// store write of this settlement and before its commit: the stamp was made on a tree that
	// holds an update the room's closed issue refused, so none of it is committed or broadcast.
	if st.roomRefusedUpdate() {
		return settleDone, st.endOnRefusedUpdate()
	}
	identityUpdate, err := mergeUpdates(st.updates)
	if err != nil {
		return settleDone, st.abandon(err)
	}
	if err := s.srv.BroadcastUpdate(ctx, room, identityUpdate); err != nil {
		return settleDone, st.abandon(fmt.Errorf("broadcast superseded document identity update: %w", err))
	}
	if err := recordStampedAskAuthors(ctx, tx, room, st.renamed, st.carried); err != nil {
		return settleDone, st.abandon(err)
	}
	// The write phase: every statement this settlement decided on, in the order it made them.
	if err := st.ledger.runQueue(ctx, nil); err != nil {
		return settleDone, st.abandon(err)
	}
	if advanced, err := s.commitSettlement(ctx, tx, st.ledger, room, authorCapture{}, nil); err != nil {
		return settleDone, st.commitFailed(advanced, fmt.Errorf("commit superseded document identity update: %w", err))
	}
	st.finishSlots()
	return settleDone, settleOutcome{}
}

// planRepairs reconciles the document's ask blocks on the tree the settlement read, which says
// what each block's row and the document owe each other, holds its check blocks to the four
// rules (reconcileCICheckBlocks) and its decisions' server-owned attributes to what the store
// holds (reconcileDecisionBlocks): the repairs to write into the room, the events to append, and
// the copies to arm. On a closed issue the reconciliation changes no ask (reconcileAskBlocks), and
// each event it appends - a block.repaired or a block.invalid, of an ask block or a check block -
// is the settlement's own (SettlementActor), not the person whose edit the version saves: a
// system actor's event notifies nobody (events.Broker.Notify), and a closed issue's settlement
// wakes nobody. The person is still named in the event's payload (DisturbedBy) and in the
// version's authors.
func (st *settlement) planRepairs(ctx context.Context) (settleStep, settleOutcome) {
	s := st.service
	reconciliation, err := s.reconcileAskBlocks(ctx, st.tx, st.room, st.owner, st.reconciled, st.credit, st.issueClosed)
	if err != nil {
		return settleDone, st.abandon(err)
	}
	// What settlement writes of the answers it keeps in step while another build serves is weighed
	// against the document as settlement read it, before any other repair is planned on it.
	reconciliation.withholdAnswersWithoutRoom(st.room, st.reconciled, st.markdown)
	if err := reconciliation.reconcileCICheckBlocks(ctx, st.tx, st.room, st.owner, st.reconciled, st.credit.actor); err != nil {
		return settleDone, st.abandon(err)
	}
	if err := reconciliation.reconcileDecisionBlocks(ctx, st.tx, st.room, st.reconciled); err != nil {
		return settleDone, st.abandon(err)
	}
	if st.issueClosed {
		for index := range reconciliation.events {
			reconciliation.events[index].Actor = SettlementActor
		}
	}
	st.reconciliation = reconciliation
	if s.afterSettleReconcile != nil {
		s.afterSettleReconcile(st.room)
	}
	return settleOn, settleOutcome{}
}

// commitRepairs writes the reconciled repairs into the room and stores what the settlement holds,
// in the room's commit order, and reads the document again when it wrote any: that read is what
// its version is rendered from, and whom that version credits is taken over it.
func (st *settlement) commitRepairs(ctx context.Context) (settleStep, settleOutcome) {
	s, room, tx, ledger := st.service, st.room, st.tx, st.ledger
	// What the settlement stores, it stores from here on, in the room's commit order: the room's
	// updates through its read, then each repair it wrote into the room after the room's updates
	// before it (storeRepair), then, for a settlement that wrote any, the room's updates through the
	// read its version is rendered from.
	if err := ledger.flushThrough(ctx, tx, room, st.first.point); err != nil {
		return settleDone, st.abandon(fmt.Errorf("store the room's updates through the settlement's read: %w", err))
	}
	if st.stamp != nil {
		if err := ledger.storeRepair(ctx, tx, room, st.cursor, st.stamp); err != nil {
			return settleDone, st.abandon(fmt.Errorf("store document identity update: %w", err))
		}
	}
	if len(st.reconciliation.repairs) > 0 {
		// The repairs are made on the document as it stands (writeLive): the tree they were
		// reconciled on was read before settlement's database work.
		slot, update, err := s.applySuppressed(ctx, room, st.doc, st.reconciliation.writeLive)
		st.keep(slot, update)
		if err != nil {
			return settleDone, st.abandon(fmt.Errorf("write reconciled typed blocks: %w", err))
		}
		if slot != nil {
			if err := ledger.storeRepair(ctx, tx, room, st.cursor, update); err != nil {
				return settleDone, st.abandon(fmt.Errorf("store reconciled typed blocks: %w", err))
			}
		}
	}

	st.versioned = st.reconciled
	versionRead := st.first
	if len(st.slots) > 0 {
		update, err := mergeUpdates(st.updates)
		if err != nil {
			return settleDone, st.abandon(err)
		}
		if err := s.srv.BroadcastUpdate(ctx, room, update); err != nil {
			return settleDone, st.abandon(fmt.Errorf("broadcast document closure update: %w", err))
		}
		// The version is the document as it stands after the repairs, read at an exact commit
		// position with every update through it stored first, not the tree settlement reconciled
		// before them: a peer's edit made since is in the room, its browsers and, stored here, its
		// stored updates, and a version rendered from the earlier tree would leave it out. An edit
		// made after this read is in neither the version nor its authors.
		if versionRead, err = s.readSettlementTree(st.state, st.doc, room, s.afterSettleAuthorsTake, true); err != nil {
			return settleDone, st.abandon(err)
		}
		if err := ledger.flushThrough(ctx, tx, room, versionRead.point); err != nil {
			return settleDone, st.abandon(fmt.Errorf("store the room's updates through the settlement's version read: %w", err))
		}
		st.versioned = versionRead.tree
	}
	// The version's authors, taken once the room's updates it holds are stored, and the head it
	// records (doc_update_version), through which every one of them is stored. The head counts
	// what this transaction has queued and not yet written (transactionHead): the settlement's
	// appends are among them.
	credit, err := settlementCredit(ctx, tx, st.state, room, versionRead)
	if err != nil {
		return settleDone, st.abandon(err)
	}
	st.credit = credit
	snapshotCursor, err := transactionHead(ctx, tx, room)
	if err != nil {
		return settleDone, st.abandon(err)
	}
	st.snapshotCursor = snapshotCursor
	if len(st.slots) > 0 && s.afterSettleVersionRead != nil {
		s.afterSettleVersionRead(room)
	}
	return settleOn, settleOutcome{}
}

// appendEvents queues events in this settlement's transaction, as one step of its write phase.
// Each event that carries a reconciled source's text stamps that source's new mention edges with
// the event that introduced them: the version event for the document itself, and each ask's own
// opened/edited event. writeVersionTx reconciled the edges earlier in this transaction, so
// stamping runs after the append, naming the event's id as the id sequence left it
// (refs.QueueStamp), and never touches sequence allocation. What it appends joins the ledger's
// events, after those the version write appended, which the settlement publishes once it commits.
// An ask event that names thread_logins has its insert read them (settlementEventLogins), after
// the ask writes queued before it, with the logins whose rows the writes it records keep once
// they are recorded after it.
//
// caused holds, by an event's place in events, what the write phase queues with that event
// (eventWrites): the writes of the node and tuple store that event is the cause of
// (graphstore.Uncaused), in the order they are stated - the version's block index, or the node of
// an ask the settlement opened and its author's follow, what an ask's index stated - and then the
// steps that follow them under the same event: the record of a write the event records in an
// ask's thread, and the move of an ask it records restored open. All of them are queued right
// after the event's append and its stamp, naming the event's id as the id sequence left it
// (graphstore.LatestEvent), so nothing between the two appends another event. A held write is
// read when the step runs, in the write phase, so one an earlier step fills then (createAskBlock)
// is read as that step left it.
func (st *settlement) appendEvents(ctx context.Context, events []model.Event, caused map[int]eventWrites) error {
	if len(events) == 0 {
		return nil
	}
	sink := eventSink(ctx)
	return write(ctx, st.tx, func(b *store.WriteBatch) error {
		for index, planned := range events {
			writes := caused[index]
			if err := st.service.events.QueueAppendWithLogins(b, planned, settlementEventLogins(planned, writes.kept), sink); err != nil {
				return err
			}
			if sourceKind, sourceID, ok := referenceSource(planned, st.room); ok {
				refs.QueueStamp(b, sourceKind, sourceID)
			}
			for _, stated := range writes.stated {
				graphstore.QueueCaused(b, *stated, graphstore.LatestEvent)
			}
			for _, step := range writes.after {
				if err := step(b); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// stateCopiesAndReadability plans the copied_from of the nodes of the document's ask blocks
// (claimCopies) and the readability of the asks whose blocks this settlement read
// (stateInvalidReasons), both in the name the version carries (versionActor) at at, and queues
// their statements under cause as one step (write): the version's event, which the step follows
// right after its append (graphstore.LatestEvent), or graphstore.NoEvent for a settlement that
// writes no version. It runs once the version's blocks are planned, so the copies read the nodes
// the block index leaves (heldBlockIndex).
func (st *settlement) stateCopiesAndReadability(ctx context.Context, at time.Time, cause int64) error {
	who := st.credit.versionActor()
	copies, err := st.reconciliation.claimCopies(ctx, st.tx, st.room, who, at)
	if err != nil {
		return err
	}
	readability, err := st.reconciliation.stateInvalidReasons(ctx, st.tx, who, at)
	if err != nil {
		return err
	}
	if copies == nil && readability == nil {
		return nil
	}
	return write(ctx, st.tx, func(b *store.WriteBatch) error {
		if copies != nil {
			if err := copies(b, cause); err != nil {
				return err
			}
		}
		if readability != nil {
			return readability(b, cause)
		}
		return nil
	})
}

// commitVersion renders the document the settlement settled, writes its version when the document
// now reads differently from the latest one, and commits the transaction all of that and the
// settlement's stored updates are in.
//
// A settlement of a closed issue's document wakes nobody: the text it saves was in the live
// document already, often for long, so an edit notice would report nothing new. Its version opens
// no edit notice and deletes the one the document holds (document_edit_notices), moves no
// approval request and carries no approval (versionWrite.closedIssue), and the settlement sweeps
// no marks after it commits. The version still lists the people whose edits it saves, and its
// artifact.version event names the version's actor (versionActor): the actor its credit names, or
// SettlementActor where that credit is only the kept editor (below); an unnamed version is quiet
// whoever its actor is (events.Quiet). The credited actor outlives the pending row the commit
// deletes, for the settlement after the reopen to name, which wakes whom the edit's own settlement
// would have woken on the open issue (clearSettlementPending).
//
// The editor so kept is credited with the text that editor wrote and with no version that adds
// none of it. A version whose credit is only that editor (settlementVersionCredit.kept) - one of
// the reopen's ask-state write, an ask repair's sweep, a rebuild or a copy marked owed, each
// naming no editor of its own - names SettlementActor on its artifact.version event and as its
// write's actor (versionActor). On an open issue its derived events, and the move of an approval
// request that names a version from before the closed issue's versions, still name that editor
// (versionWrite.keptEditor); on a closed issue every derived event names SettlementActor
// (planRepairs).
func (st *settlement) commitVersion(ctx context.Context) (settleStep, settleOutcome) {
	s, room, state, tx, ledger := st.service, st.room, st.state, st.tx, st.ledger
	// read is the settlement's private copy of the document (readSettlementTree), and nothing but
	// planRepairs changes it in place, which records each change it makes (changedTree).
	// versioned is another tree once a stamp replaced the read or a write read the document again.
	// So the version takes the rendering of the read when it is that tree, unchanged.
	markdown, offsets, err := st.markdown, st.offsets, st.renderErr
	reused := st.versioned == st.read && !st.reconciliation.changedTree()
	if s.beforeSettleVersionRender != nil {
		s.beforeSettleVersionRender(room, st.read, reused)
	}
	if !reused {
		markdown, offsets, err = renderTreeWithOffsets(st.versioned)
	}
	if err != nil {
		if len(st.slots) == 0 && skipUnreadableSettlement(room, err) {
			return settleDone, settleOutcome{}
		}
		return settleDone, st.abandon(err)
	}

	// The second read of a refused update's second order (roomRefusedUpdate), beside the reads of
	// a stopping service and a failed room, after the last store write of this settlement was gated
	// (flushThrough and storeRepair decide in the read phase what the write phase stores) and
	// before its commit: what it would commit is rendered from a tree that holds an update the
	// room's closed issue refused.
	if st.roomRefusedUpdate() {
		return settleDone, st.endOnRefusedUpdate()
	}
	state.mu.Lock()
	// A settlement that wrote into the room commits what it wrote even when the document has moved
	// since its read: the room and its peers hold those updates, and dropped here they would never
	// reach the store. Its version is the document as it stood at its last read, after the repairs,
	// every update of which this transaction stores; a move made after that read is in a later
	// version.
	if s.stopping.Load() || state.failed != nil || (state.roomGeneration != st.generation && len(st.slots) == 0) {
		s.unlockState(room, state)
		s.discardSuppressedPersistence(room, st.slots...)
		return settleDone, settleOutcome{}
	}
	s.unlockState(room, state)

	// Settlement writes a version only when the document now reads differently from the latest
	// one. Indexing an ask block writes an `asks` row and an `ask.opened` event over words the
	// edit that wrote the block already versioned, so a version for that event alone would be
	// byte-identical and credited to nobody (LEGION-273, where it staled an approval pinned to what
	// an agent had just written; carryApproval now carries that approval to such a version
	// instead, an event nothing in the log would explain). Its tree writes - stamping block ids,
	// restoring an ask block's server-owned attributes - move the stored Proof state and often
	// render the same markdown,
	// and a version for one of those would repeat the version before it too (LEGION-229
	// requirement 2).
	// contentChanged stays the first half of the test: a version an older renderer wrote is not
	// this settlement's to canonicalise when nothing has touched the document since.
	contentChanged, err := contentChangedSinceVersion(ctx, tx, room, st.latest.docUpdateVersion)
	if err != nil {
		return settleDone, st.abandon(err)
	}
	versioning := contentChanged && markdown != st.latest.markdown
	// A retired document (RetireDocument) takes no new version: no list holds it and no reader
	// looks for text in it. Whether it is retired is the read beside the owner's lock this
	// settlement took (lockArtifactOwner, artifactOwner.Retired), the lock the retirement takes
	// too, so a retirement either committed before that read or commits after this settlement.
	// Typing a room took before the retirement closed it, or took in a room the retirement's close
	// missed (ServeHTTP), is still stored - this settlement commits the updates it read, as every
	// settlement does - and logged, so it is kept rather than dropped, but no version of the
	// retired document holds it.
	if versioning && st.owner.Retired {
		slog.Warn("dispatch: settlement stores the updates of a retired document and writes no version of it",
			"room", room, "latest_version", st.latest.Number)
		versioning = false
	}
	settledVersion := st.latest.Number
	if versioning {
		settledVersion++
	}
	// Every update this settlement read is settled once it commits, so the row that left the
	// settlement to a later load goes with that commit. It goes before the first event this
	// settlement appends. A transaction marking this document owed as a copy (owedCopiesOf) holds
	// its row until it commits, and this settlement then waits for it holding its owner lock and
	// room lock, which that transaction never takes; nor does it take what this settlement goes on
	// to lock, its events locking only this document's owner and owner row. The pending authors
	// stay unless this settlement's version lists them (deleted below): a settlement that writes no
	// version lists no one.
	//
	// The row's latest edit source goes with it, and the settlement after a reopen still names it:
	// before a closed issue's document settled, the row stood through the closed period, and that
	// settlement named its editor on its events and as the mover of an approval request. So on a
	// closed issue the editor this settlement credits is kept past the row (clearSettlementPending,
	// doc_closed_edit_sources), for the reopen's mark to write back into the row it inserts
	// (settlementPendingStatement) and the reopen's move to name (FollowClosedIssueVersions). A
	// settlement on an open issue has named its editor itself, and one on a closed issue that
	// credits SettlementActor for the edits it saves names no editor, as the settlement after the
	// reopen, crediting the same edits, named none then: each forgets the editor an earlier
	// settlement kept. One on a closed issue whose credit is only the kept editor
	// (settlementVersionCredit.kept), its version adding none of their text, keeps that same
	// editor, so the reopen still finds the editor of the text the closed issue's versions hold.
	// So does one on a closed issue that saved nothing (savedNothing): it credits SettlementActor
	// only because it found nothing to credit, and its commit deletes the pending row alone,
	// whatever arming ran it after the settlement that kept the editor. The delete and the keep
	// are queued, as every write of this settlement is, for its write phase.
	var kept *model.Actor
	if st.issueClosed && !st.credit.actor.SameAs(SettlementActor) {
		kept = &st.credit.actor
	}
	if st.issueClosed && kept == nil && st.savedNothing(versioning) {
		if err := clearSettlementPendingRow(ctx, tx, room); err != nil {
			return settleDone, st.abandon(err)
		}
	} else if err := clearSettlementPending(ctx, tx, room, kept); err != nil {
		return settleDone, st.abandon(err)
	}
	if err := st.reconciliation.nameVersion(ctx, tx, room, st.owner, settledVersion); err != nil {
		return settleDone, st.abandon(err)
	}
	// The copies among the document's ask blocks and the readability of the asks whose blocks
	// this settlement read are stated in the name the version itself carries (versionActor), at
	// one time (stateCopiesAndReadability).
	at := time.Now().UTC()
	if versioning {
		var keptEditor *model.Actor
		if st.credit.kept {
			keptEditor = &st.credit.actor
		}
		// A person's edit in the room that took an answered ask's block out of the document is
		// that person's removal, which the version's index pass states on the ask's node, as the
		// same edit over HTTP records it (holdRoomRemovals). Only an answered ask the
		// reconciled tree held no block of can be one, or any where the version is a later read.
		if st.reconciliation.answeredGone || st.versioned != st.reconciled {
			if err := holdRoomRemovals(ctx, tx, room, st.versioned, st.credit.authors); err != nil {
				return settleDone, st.abandon(err)
			}
		}
		result, writeErr := s.writeVersionTx(ctx, tx, room, renderedTree{tree: st.versioned, markdown: markdown, offsets: offsets}, st.credit.versionActor(), &versionWrite{
			authors:          st.credit.authors,
			capture:          &st.credit.capture,
			docUpdateVersion: &st.snapshotCursor,
			settled:          &st.reconciliation.sources,
			closedIssue:      st.issueClosed,
			settlement:       true,
			keptEditor:       keptEditor,
		})
		if writeErr != nil {
			// The version a human approved holds an element this build does not know: the version
			// is left, unwritten, to a build that reads it, as a tree holding one is.
			if len(st.slots) == 0 && errors.Is(writeErr, ErrDocNewerContent) && skipUnreadableSettlement(room, writeErr) {
				return settleDone, settleOutcome{}
			}
			return settleDone, st.abandon(writeErr)
		}
		// The version lists the pending authors this settlement read, so its commit deletes them.
		if err := deleteCapturedPendingAuthors(ctx, tx, room, st.credit.capture); err != nil {
			return settleDone, st.abandon(err)
		}
		if s.afterSettleVersionWrite != nil {
			if err := s.afterSettleVersionWrite(room); err != nil {
				return settleDone, st.abandon(err)
			}
		}
		// A person's version opens or moves on the document's edit notice, and a closed issue's
		// deletes it (settledEditNotice).
		if err := s.settledEditNotice(ctx, tx, room, st.issueClosed, result.version.Number, st.credit.authors, st.credit.capture.latest); err != nil {
			return settleDone, st.abandon(err)
		}
		// A document body cites nodes whose rows carry a backlink count, so the version event
		// names what this settle moved exactly as a message or comment write does.
		versionEvent := model.Event{
			IssueKey: st.owner.IssueKey,
			Type:     "artifact.version",
			Actor:    st.credit.versionActor(),
			Payload:  ArtifactVersionEventPayload(room, st.owner.Name, result.version, nil, result.changes),
		}
		if st.owner.IssueKey == nil {
			versionEvent.ArtifactID = &room
		}
		// What this settlement's block index stated waits for that event: its id is the cause
		// every node and tuple of the pass carries, and it exists only once the event's insert has
		// run, so it is stated right after it (appendEvents).
		if err := st.appendEvents(ctx, []model.Event{versionEvent}, map[int]eventWrites{0: {stated: []*graphstore.Uncaused{&result.blocks}}}); err != nil {
			return settleDone, st.abandon(err)
		}
		// The copies and the readability, once the version's blocks are planned, under that same
		// event, right after it.
		if err := st.stateCopiesAndReadability(ctx, at, graphstore.LatestEvent); err != nil {
			return settleDone, st.abandon(err)
		}
	}
	// An ask this settlement opened has its row and node only in this write phase, so a version
	// written before them stated none of its block's shaped ties; a settlement that writes a
	// version states them in that version's index, and one that writes none writes them here,
	// after the rows, before the ask.opened that names the targets they moved is appended.
	if !versioning {
		if err := st.reconciliation.writeOpenedAskTies(ctx, tx, room, settledVersion, st.versioned, offsets); err != nil {
			return settleDone, st.abandon(err)
		}
	}
	// The nodes and the follows of the asks this settlement opened, what the index of each ask it
	// opened, restored or reworded stated, their blocks' ties, and what each answer state it wrote
	// states on its ask's node wait for the events that record them, each stated right after its
	// own; and after those, under the same event, the writes it made in its asks' threads - each
	// ask it opened, each answer it restored - are recorded by the one writer of those threads, and
	// each ask it restored open reaches the person it waits on now (causedByEvent).
	caused, err := st.reconciliation.causedByEvent(st.reconciliation.events)
	if err != nil {
		return settleDone, st.abandon(err)
	}
	if err := st.appendEvents(ctx, st.reconciliation.events, caused); err != nil {
		return settleDone, st.abandon(err)
	}
	// A settlement that writes no version states the copies and the readability under no event.
	if !versioning {
		if err := st.stateCopiesAndReadability(ctx, at, graphstore.NoEvent); err != nil {
			return settleDone, st.abandon(err)
		}
	}
	// The write phase: every statement this settlement decided on, in the order it made them.
	if err := ledger.runQueue(ctx, nil); err != nil {
		return settleDone, st.abandon(err)
	}
	st.published = ledger.events
	// The commit marks the in-flight credits the version listed consumed before it releases the
	// room's state, and the room updates the settlement stored marked stored (commitSettlement); a
	// settlement that writes no version takes out none. The room forgets the author of each ask
	// block the commit indexed under the same hold, so no reader that finds the block's ask row
	// committed then finds the room still owing its author.
	committed := authorCapture{state: state}
	if versioning {
		committed = st.credit.capture
	}
	if advanced, err := s.commitSettlement(ctx, tx, ledger, room, committed, func(err error) {
		// This runs before the publish below, the order Ledger.Commit keeps for every other
		// version write, so a subscriber acting on this version's artifact.version event acts
		// after it.
		if err != nil {
			return
		}
		st.credit.consumeAskAuthorsLocked(st.reconciliation.indexedAskBlocks)
		if state.roomGeneration == st.generation {
			state.settleFailures = 0
			state.unsettled = len(state.inflight) > 0
			if !state.unsettled {
				state.lastActor = nil
			}
		}
	}); err != nil {
		return settleDone, st.commitFailed(advanced, fmt.Errorf("commit document settlement: %w", err))
	}
	st.finishSlots()
	for _, event := range st.published {
		s.events.Publish(event)
	}
	st.reconciliation.logCopiedBlocks(room)
	// A copy of an ask this settlement retracted opens its own ask at its next settlement, which
	// its pending row (reconcileAskBlocks, owedCopiesOf) also leaves to the resumption should this
	// process stop first.
	for _, copy := range st.reconciliation.owedCopies {
		s.scheduleSettle(copy)
	}
	// A closed issue's settlement sweeps no marks: the unmark is an injection without the
	// owner-verified mark, which allowInject refuses on a closed issue. The document's next
	// settlement on the open issue sweeps them: at the reopen, one runs only for a document the
	// reopen writes or marks owed.
	if !st.issueClosed {
		s.sweepUnrecordedMarks(room, st.versioned)
	}
	return settleDone, settleOutcome{}
}

// settledEditNotice is what a settlement's version does to the document's edit notice, under the
// owner lock the settlement took first: the sweep takes the two in that order too
// (sendEditNotice). A person's version on an open issue opens or moves on the notice
// (recordEditNotice). A closed issue's version opens none, and deletes in that same order the
// notice a person's version opened before the close, due or held (holdEditNotice): neither a
// reopen inside its quiet time nor the first notice after the reopen, which would start from that
// row's from_version (openEditNotice), carries the edits this version saves. The version a joined
// transaction writes as the settlement would (SettleVersion) does the same, and a snapshot's,
// whose closedIssue is false.
func (s *Service) settledEditNotice(ctx context.Context, tx pgx.Tx, room string, closedIssue bool, version int, authors []model.Actor, latest *model.Actor) error {
	if closedIssue {
		return writeExec(ctx, tx, "delete the closed issue's document's edit notice", dropEditNotice, room)
	}
	return s.recordEditNotice(ctx, tx, room, version, authors, latest)
}

package docs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
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

// settleStep is how a settlement pass ended (settlePass), or that one of its steps did not end it
// (settleOn).
type settleStep int

const (
	// settleDone ends the settlement, with the outcome the pass reports: the pass committed the
	// document's version, stopped short, or left the document to whatever owns its settlement now.
	settleDone settleStep = iota
	// settleAgain is a pass that did its part - committed a stamp or repairs, or planned the
	// repairs the next pass commits (repairPlan) - and leaves the rest to the next pass.
	settleAgain
	// settleMoved is a pass that found the document moved under what it read, and rolled back.
	settleMoved
	// settleOn is no ending: the step a pass took (take, readLocked, applyStamp) left it going on,
	// and the outcome beside it is nothing. Only settlePass reads it; a pass never ends on it.
	settleOn
)

// String names the step a log line reports (settleRoomWithin).
func (s settleStep) String() string {
	switch s {
	case settleDone:
		return "done"
	case settleAgain:
		return "again"
	case settleMoved:
		return "moved"
	default:
		return "on"
	}
}

// maxSettleRereads is how many times one settlement reads its document again after a pass found it
// moved under its read (settleMoved). A document a browser keeps moving faster than it renders is
// left owed after that: each of its updates arms a settlement of its own, and the resumption arms
// one for a row left standing.
const maxSettleRereads = 3

// maxSettlePasses bounds the passes of one settlement: one each for the stamp, the plan of the
// repairs, the repairs and the version, and one more for each read again (maxSettleRereads).
const maxSettlePasses = 4 + maxSettleRereads

// repairPlan is the ask repairs a pass found its document needs, worked out where no lock reaches:
// the tree that pass read, reconciled, with the answers its document has room for decided
// (weighAnswers), and that tree's rendering - for the read that stood at heldThrough and rendered
// as markdown. A later pass that reads the same document and reconciles it to the same tree commits
// the repairs with this rendering, so the pass that holds the document's locks renders nothing: the
// rendering says which version the events committed with the repairs name.
type repairPlan struct {
	heldThrough int64
	markdown    string
	answers     answerDecision
	tree        *pmdoc.Node
	repaired    string
	repairedErr error
}

// settlementTake is what a settlement pass takes of its room before it copies the tree it renders:
// the durable version the room's instance held every update through (heldThrough, its roomCursor),
// which the pass reads again once it holds the document's locks, the exact commit position the
// copy is the room at (point), every update of the instance through which the pass stores before
// it versions that copy (Ledger.flushThrough), and the in-flight credits its version counts -
// those observed so far (creditThrough). An update observer credits a change only once the room
// holds it, so every in-flight credit the take observed is for a change the copy holds; one
// observed after it stays pending for the next version. The latest edit source is not taken here:
// a version names the one the document's pending-settlement row holds, which the pass reads under
// the document's advisory lock (takeCredit), so two processes settle the same credit. The take
// holds state.mu for itself alone: held across the copy and its render it would block the update
// observer that credits and broadcasts every peer's keystroke.
type settlementTake struct {
	heldThrough   int64
	point         roomPoint
	creditThrough uint64
}

// copyTree is a copy of tree whose nodes and attributes its caller may change without changing
// tree's.
func copyTree(tree *pmdoc.Node) *pmdoc.Node {
	if tree == nil {
		return nil
	}
	copied := &pmdoc.Node{Type: tree.Type, Text: tree.Text, Attrs: maps.Clone(tree.Attrs), Marks: slices.Clone(tree.Marks)}
	if tree.Children != nil {
		copied.Children = make([]*pmdoc.Node, len(tree.Children))
		for index, child := range tree.Children {
			copied.Children[index] = copyTree(child)
		}
	}
	return copied
}

// settlement is one document's settlement, as its passes run it (settleRoomWithin): the room it
// settles, that room's state and the live instance it holds for the whole settlement, the room
// generation its passes stand at, which moves as a pass reads the room again, the repairs a pass
// planned for the next one to commit, and how the settlement reports a stop short.
type settlement struct {
	service     *Service
	room        string
	state       *roomState
	doc         *crdt.Doc
	settlements *store.Pool
	generation  uint64
	// caller is the context the settlement's caller handed it (settleRoomWithin), before the
	// settlement's own deadline (settleTimeout): what still ends the write phase of a pass that
	// has not written into its room, which runs apart from that deadline (writePhase).
	caller context.Context
	// lease is the end of the lease the settlement holds on the document's pending-settlement row
	// (leaseSettlementPending), nil before it takes one, where the document owed none, and once
	// its commit has deleted the row and the lease with it (commitVersion). settleRoomWithin gives
	// back a lease still held here as the settlement ends (releaseSettlementLease).
	lease *time.Time
	// reads counts the reads of the room its passes have made. The first is quiet: the updates it
	// classifies are in the tree this settlement reconciles and versions, and arming a settlement
	// for them would end this one as superseded. A later pass's read arms one, since the
	// reconciliation before it did not see them (treePoint).
	reads int
	plan  *repairPlan
	// stored says a pass of this settlement committed an update into the document's stored history
	// - a stamp (commitStamp) or repairs (commitRepairs) - before the pass that versions the
	// document, which reads the head after it and so cannot tell from its own reads
	// (settlementPass.savedNothing).
	stored bool
	// stopShort is the outcome of the settlement stopping short with err having written nothing
	// into the room (settleRoomWithin), removed being its read of whether the document was removed
	// since it started (stoppedShort: removedThroughPool outside a pass, removedThroughTransaction
	// inside one).
	stopShort func(removed func(context.Context) bool, err error) settleOutcome
}

// settlementPass is one pass of a settlement, and what that pass holds of the document: what it
// took of the room and rendered before it opened a transaction, what it read under the document's
// locks, and what it has written into the room and appended since.
type settlementPass struct {
	*settlement
	// taken is what the pass took of the room before it copied the tree it renders; cursor is the
	// cursor of the instance it settles, which gates the room updates it stores and the repairs it
	// appends, and takes them in once they are stored (commitSettlement).
	taken  settlementTake
	cursor *roomCursor
	// read is the tree the pass copied, with markdown and offsets its rendering and renderErr why
	// it has none; stamping says that tree lacks block ids, and stampSuperseded that a settlement
	// whose stamp commits alone ends with it (applyStamp).
	read            *pmdoc.Node
	markdown        string
	offsets         []pmdoc.BlockOffset
	renderErr       error
	stamping        bool
	stampSuperseded bool
	// The pass's transaction and what it read under the document's locks: the ledger its version
	// records its events on, the document's owner row, the latest version, the update cursor a
	// version of this pass would record, and what that version credits.
	ledger *Ledger
	tx     pgx.Tx
	owner  artifactOwner
	latest latestVersionRow
	// readHead is the head of the document's stored history this pass read under the document's
	// lock, before it stored anything (readLocked). snapshotCursor is the head the version records
	// (doc_update_version), through which every one of the room's updates it holds is stored; it
	// equals readHead when the pass stored no update (savedNothing).
	readHead       int64
	snapshotCursor int64
	credit         settlementVersionCredit
	// issueClosed is the document's issue row as this pass's transaction read it under the
	// issue's owner lock (lockArtifactOwner): true for a closed issue, false for an open one and
	// for a document of no issue, so its zero value is open. A closed issue's document settles as
	// an open one's does, and every rule of the settlement that differs on a closed issue goes by
	// this row, never by the room's record of the issue (roomState.closed): a close this process
	// has not heard of yet leaves that record open.
	issueClosed bool
	// slots holds a suppression slot for each update this pass writes into the room, in the order
	// the room's persistence worker is handed them, and updates holds those updates. stamp is the
	// block-id stamp applyStamp wrote into the room, nil for none, stored as the room committed it
	// (storeRepair). Every path after a write finishes or discards all of them.
	slots   []*suppressSlot
	updates [][]byte
	stamp   []byte
	// renamed are the ask blocks the stamp gave an id of its own, and carried the authors the room
	// held for the ids they replaced (recordStampedAskBlocks): the pass records them durably in
	// the transaction that stores the stamp (recordStampedAskAuthors), so another process's
	// settlement finds a stamped block's author too.
	renamed []stampedAsk
	carried map[string]model.Actor
	// reconciliation is what the pass reconciled the document's ask blocks to, planned the repairs
	// of (planRepairs), and published the events it has appended, published once it commits.
	reconciliation settlementReconciliation
	planned        *repairPlan
	published      []model.Event
}

// savedNothing reports whether this settlement wrote nothing that a later move of an approval
// request reads, given whether this pass writes a version (versioning): it writes no version,
// stores no update in the document's history - none of the room's updates, no block-id stamp and
// no ask- or check-block repair, neither in this pass, so the head it leaves is the head it read
// (readHead), nor in a pass of this settlement before it (settlement.stored) - and lists no
// author, neither a pending author nor an in-flight credit. A reopen's move reads the latest
// version and the editor kept for it (FollowClosedIssueVersions); what else such a settlement
// writes - block events in SettlementActor's name, the settled version's number in their
// payloads, the recorded author of an unindexed ask block - changes neither. A settlement armed
// while another one commits, as the resumption's pick of the row that one is about to delete arms
// it, finds nothing owed and is such a settlement.
func (p *settlementPass) savedNothing(versioning bool) bool {
	return !versioning && !p.stored && p.snapshotCursor == p.readHead && len(p.credit.authors) == 0
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
// leaving what follows to its caller: a timer's settlement, the one Shutdown runs under its drain
// budget, one settleOwedNow runs under its caller's context. Every one of them also runs under a
// deadline of its own, settleTimeout from here, so whichever way a settlement started it ends at
// that deadline or at its caller's, whichever comes first, but for the three parts no deadline
// stops (settlementTimeout): a pass's write phase among them, which runs under a bound that grows
// with the document (writePhase) and, in a pass that has written nothing into its room, still
// ends at its caller's end.
// Before anything else - before it loads a cold room - it waits for one of the settlementWorkers
// slots, holding no connection, so every way a settlement starts is bounded and the excess waits
// there. When parent or the deadline ends first, in that wait or after it, the settlement's
// database work is cancelled and its transaction rolls back, and nothing retries it or counts it
// as a failure: the document's pending-settlement row leaves it to resume (onLoadDocument,
// RunSettlementResumption). That is how a shutdown that cannot settle every room within its budget
// leaves the rest to the next process. A settlement the database did not serve (store.Unserved:
// the settlements pool's or the rooms pool's acquire bound ran out, or the database stopped
// answering inside it), or whose warm-up's load ran out the bound on its reads before the database
// answered them (errLoadReadsBound), is left to resume the same way, so overload fails
// no room (maxSettleFailures). It is one outcome for all three shapes that bound names, the
// statement the database may still have run included: a settlement pass reads the document
// again and writes what it reads, so running it again costs nothing whatever the statement did,
// where counting each stall a failure would fail every room a pod-wide stall touched three
// times over.
// So is a settlement that has to write into a room Shutdown has begun
// closing, which refuses the write (allowInject). A room Shutdown has begun closing is not loaded
// for a settlement unless mayLoad says this is the settlement the shutdown drain owes it, whose
// warm-up loads through the shutdown (withShutdownLoad).
//
// Once it holds its slot it takes a lease on the document's pending-settlement row
// (leaseSettlementPending) before it does anything to the document, so no resumption pass, in any
// process, arms the document again while it runs: that arming would move the room's generation
// under it and end it writing nothing. A commit deletes the row and the lease with it; a
// settlement that ends any other way gives the lease back as it returns (releaseSettlementLease),
// so the resumption may arm the document at once. Only a settlement that dies with its process, or
// whose give-back fails, leaves the lease to run out (settlementLease) before the resumption
// settles the document.
//
// The settlement itself does only what no caller can do after it: a settlement that wrote into
// the room and cannot finish fails the room, whose reload arms the settlement again, and one whose
// room holds a replaced stored history closes the room for a reload (reloadReplacedRoom). The
// retries, the re-arming and the log lines of a settlement cut short are its caller's: the timer's
// settlement and Shutdown's do them (followUpSettlement), and a caller that runs it synchronously
// and answers for its outcome itself reports the reason instead (settleOwedNow).
//
// A closed issue's document settles as an open one's does. Nothing that arms a settlement, or
// decides whether one goes on, reads whether the issue is closed. The settlement reads the issue's
// state only to decide what it writes, and only from the issue row each pass's transaction reads
// under the issue's owner lock (settlementPass.issueClosed). The issue's close ends a settlement
// it finds armed or running only as every move of the room's generation does
// (closeRecordLocked), and the document's pending-settlement row leaves that settlement to
// resume. The one settlement a closed issue ends besides is one whose room took an update the
// append gate refused because the issue closed more than closeMargin before the room took it:
// that room holds text the store never will, so the settlement saves nothing of it, fails no room
// and counts no failure, and the room's close and the resumption after it settle the document
// from the store (endOnRefusedUpdate).
//
// A removed document owes no settlement: its removal (api.removeArtifact) deletes its versions,
// its stored updates and its pending-settlement row with its artifacts row, and closes its room on
// every process once it has committed. A settlement already past its generation check when that
// commit lands - one that queued behind the removal for the document's owner lock, or that started
// before this process's eviction or another process's announcement reached its room - stops short
// on the row that has gone: the owner row its pass reads under the owner lock (settlePass) or the
// room's load reads nothing. It ends there, saying nothing and retrying nothing, so no retry warms
// a room over the row (documentRemoved).
//
// The settlement runs in passes (settlePass), none of which renders the document while it holds the
// document's owner lock. A pass that commits a part of the settlement - a stamp, repairs - or plans
// the repairs, or finds the document moved under its read, is followed by another, at most
// maxSettlePasses in all and maxSettleRereads reads again.
func (s *Service) settleRoomWithin(parent context.Context, room string, generation uint64, mayLoad bool) settleOutcome {
	settle := &settlement{service: s, room: room, generation: generation, caller: parent}
	parent, cancel := context.WithTimeoutCause(parent, s.settleTimeout, errSettlementTimeout)
	defer cancel()
	// stopShort is the outcome of a settlement that stopped short with err having written
	// nothing into the room: a retry, unless its deadline, its write phase's bound (writePhase)
	// or parent has ended, the pools refused it a connection or its warm-up's load ran out its
	// reads' bound (settleOverloaded), the document has been removed or Shutdown refused the
	// write. A write phase cut at its bound is a settlement past its deadline (settlePastDeadline).
	// The generation it counts that retry's failure against is the one the settlement stands at
	// now, which its passes move as they read the room again. An err that is the append gate's
	// refusal of an update the room took after its issue closed is no fault to retry: it ends the
	// settlement (endOnRefusedUpdate).
	settle.stopShort = func(removed func(context.Context) bool, err error) settleOutcome {
		if errors.Is(err, ErrIssueClosed) {
			return settle.endOnRefusedUpdate()
		}
		next := settleRetry
		switch {
		case errors.Is(context.Cause(parent), errSettlementTimeout), errors.Is(err, errSettlementWriteTimeout):
			next = settlePastDeadline
		case parent.Err() != nil:
			next = settleOutOfBudget
		case loadEndedByOwnBound(parent, err):
			next = settleOverloaded
		case removed(parent):
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
	if s.roomGone(state) || state.roomGeneration != generation {
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
	shuttingDown := s.shuttingDown(room)
	if shuttingDown && !mayLoad && s.srv.GetDoc(room) == nil {
		return settleOutcome{}
	}
	release, err := s.settlements.acquire(parent)
	if err != nil {
		return settle.stoppedShort(fmt.Errorf("wait for a settlement slot: %w", err))
	}
	defer release()
	// The settlement counted itself running (settling) while it waited, so a write that opened
	// meanwhile took it over (openLiveWrite). Whatever moved the generation while it waited owns
	// the settlement now, as it would had it moved before the settlement started.
	state.mu.Lock()
	moved := s.roomGone(state) || state.roomGeneration != generation
	s.unlockState(room, state)
	if moved {
		return settleOutcome{}
	}
	// The pool every pass of this settlement reads and writes through (settlementPass), opened once
	// here, before the warm-up, so no settlement takes a connection admission counts for requests.
	// The warm-up's load of the room is not one of those reads: ygo loads a room through its
	// persistence adapter with no caller's context (servicePersistenceAdapter.LoadDoc), so the load
	// reads the pool that owns loads (onLoadDocument, loadTxSource's fallback), as every room load
	// does, under its own bound (coldReadTimeout). A settlement that cannot open this pool writes
	// nothing and stops short, rather than reading on through the request pool.
	settlements, err := s.store.Pool.Settlements()
	if err != nil {
		return settle.stoppedShort(fmt.Errorf("open the settlements pool: %w", err))
	}
	settle.settlements = settlements
	// The settlement's work on the document begins here, under its lease on the document's
	// pending-settlement row, which keeps the resumption from arming it again while it runs. A
	// settlement that ends without its commit having deleted the row gives the lease back as it
	// returns, while it still holds its slot and with it a connection of the pool.
	lease, err := leaseSettlementPending(parent, settlements, room, s.settleLease)
	if err != nil {
		return settle.stoppedShort(err)
	}
	settle.lease = lease
	// The give-back runs once everything below has returned, when nothing this settlement started
	// can still commit: its warm-up's Apply returns only once the load it started has (ygo runs a
	// room's load on the goroutine whose Apply created the room, getOrCreateRoom), and a load it
	// only waited for is another caller's, which commits nothing of this settlement; each pass
	// returns only once its transaction has committed or rolled back (settlePass); and a commit
	// whose outcome is unknown, which may still land, drops the lease from settle.lease
	// (commitFailed), so it is left to run out.
	defer func() {
		if settle.lease != nil {
			releaseSettlementLease(parent, settlements, room, *settle.lease)
		}
	}()
	if s.srv.GetDoc(room) == nil {
		state.mu.Lock()
		state.settleWarming = true
		s.unlockState(room, state)
		// The warm-up carries the service's own mark (withOwnerVerified), so allowInject does not
		// read the issue for it: it applies nothing, and a closed issue's document settles as an
		// open one's does, so the refusal a closed issue makes of a caller's injection is not
		// this load's to meet. A late departure's settlement hands it the settlement's mark too,
		// through parent (settleLastPeer, withSettlementLoad).
		warmCtx := withOwnerVerified(withQueryPool(parent, settlements))
		if shuttingDown {
			warmCtx = withShutdownLoad(warmCtx)
		}
		err := s.srv.Apply(warmCtx, room, func(_ *crdt.Doc, _ func(func(*crdt.Transaction))) {})
		state.mu.Lock()
		state.settleWarming = false
		s.unlockState(room, state)
		if err != nil && !errors.Is(err, websocket.ErrNoChanges) {
			return settle.stoppedShort(fmt.Errorf("warm document for settlement: %w", err))
		}
	}
	// Hold the live document for the whole settlement: reading the room again later could return
	// nil, and every later pass would dereference it. A nil here is a close that retired the room
	// since the generation check above or the warm-up, and the retry this settlement stops short
	// into decides by the room's generation (retrySettleLocked). A close that moved it - an
	// eviction, a room's failure, a close of a room whose stored history was replaced, an issue's
	// first close of the record - ended this arming: the retry counts nothing and arms nothing,
	// and the document is left to what that close arranged, a reload, the document's next load or
	// the resumption. A close that did not - ygo's idle sweep, the close of the room at its issue's
	// reopen, a repeated close of a record that already reads closed - is followed by no load that
	// arms the settlement, and this settlement took the arming it ran on, so the retry arms it
	// again, and that settlement warms the room.
	if s.afterSettleWarm != nil {
		s.afterSettleWarm(room)
	}
	doc := s.srv.GetDoc(room)
	if doc == nil {
		return settle.stoppedShort(fmt.Errorf("hold the room to settle: %w", errRoomReplaced))
	}
	settle.doc = doc
	moves := 0
	for passes := 1; ; passes++ {
		step, outcome := settle.settlePass(parent)
		switch step {
		case settleDone:
			return outcome
		case settleMoved:
			moves++
		}
		if moves > maxSettleRereads || passes == maxSettlePasses {
			// The step the last pass ended on and how many passes read the document again say
			// which of the two bounds was reached, and where: a run of passes that each found
			// the document moved reads differently from one that kept committing a part.
			slog.Info("dispatch: document settlement left owed after reading a moving document again; its next update or the resumption settles it",
				"room", room, "passes", passes, "rereads", moves, "last", step)
			return settleOutcome{}
		}
	}
}

// stoppedShort is the settlement stopping short with err outside a pass (stopShort), reading
// whether the document was removed through the settlements pool (removedThroughPool).
func (s *settlement) stoppedShort(err error) settleOutcome {
	return s.stopShort(s.removedThroughPool, err)
}

// removedThroughPool is the read outside a pass of whether the document was removed
// (documentRemoved): through the settlements pool, and none before that pool is open, since a
// settlement never reads through the request pool; the document then stands.
func (s *settlement) removedThroughPool(ctx context.Context) bool {
	return s.settlements != nil && s.documentRemoved(ctx, s.settlements)
}

// stoppedShort is the settlement stopping short with err in this pass (stopShort): once the pass
// holds its transaction, the read of whether the document was removed goes through it
// (removedThroughTransaction), since a pooled read beside it would be a second connection of the
// pool that transaction holds one of, and through the settlements pool before
// (settlement.stoppedShort).
func (p *settlementPass) stoppedShort(err error) settleOutcome {
	if p.tx == nil {
		return p.settlement.stoppedShort(err)
	}
	return p.stopShort(p.removedThroughTransaction, err)
}

// removedThroughTransaction is the read of whether the document was removed (documentRemoved) in
// a pass that holds its transaction: through that transaction.
func (p *settlementPass) removedThroughTransaction(ctx context.Context) bool {
	return p.documentRemoved(ctx, p.tx)
}

// settlePass is one pass of the settlement over doc, the room instance it holds, and the outcome
// the settlement reports when the pass ends it (settleDone). The pass takes its transaction from
// the pool the settlement opened for every read and write of its own (settleRoomWithin).
//
// Nothing renders the document under its owner's lock, which serialises every writer of the
// issue. Before it opens its transaction the pass takes what its version credits
// (settlementTake), copies the live tree and renders it, both the markdown and the block offsets
// its version stores; when the tree lacks block ids it also renders a stamped copy, which says
// whether the stamp changes the markdown (take). Under the locks it checks that the room's
// instance still holds the durable version it held at the take: a durable update that landed since
// may be missing from the copy, and a version of it would record that update's cursor over text
// without it, so the pass rolls back and reads the document again instead (readLocked). Then:
//
//   - A tree that lacks block ids is stamped in the room, and the stamp committed alone
//     (commitStamp).
//   - A tree whose ask blocks need repairs, or whose check blocks break one of the four rules a
//     check proved by CI keeps (reconcileCICheckBlocks), and no plan for them is reconciled,
//     rolled back, and its repairs planned - their answers weighed and the repaired tree
//     rendered - outside the lock (planRepairs, repairPlan).
//   - A tree whose repairs match the plan has them written into the room, appended and committed
//     with the rows and events their reconciliation wrote, and no version (commitRepairs).
//   - Otherwise the pass writes the version from the pre-rendered output (commitVersion): a tree
//     that needs no repair, and one whose repairs the plan's decision all dropped, which leaves
//     every block as this pass found it (applyAnswers).
//
// Each pass but the last leaves the rest to the next, which reads the document again.
func (s *settlement) settlePass(parent context.Context) (settleStep, settleOutcome) {
	pass := &settlementPass{settlement: s}
	if step, outcome := pass.take(); step != settleOn {
		return step, outcome
	}
	if err := pass.leaseThroughWrite(parent); err != nil {
		return settleDone, pass.stoppedShort(err)
	}
	ctx, err := pass.begin(parent)
	if err != nil {
		return settleDone, pass.stoppedShort(err)
	}
	tx := pass.tx
	// Rolled back apart from ctx: one the settlement's deadline ended still rolls back.
	defer tx.Rollback(context.WithoutCancel(ctx))
	owner, open, err := lockArtifactOwner(ctx, tx, s.room)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// The document has gone: a removal deleted every row of it under the owner lock this
		// pass took or waited for (api.removeArtifact). Nothing is left to version and nothing
		// is owed - the pending-settlement row and the pending authors went with the artifact
		// row - so the pass ends here rather than reading a version that cannot exist. The room
		// is closed on every process by the removal's own announcement
		// (Service.documentRemoved), and the settlement it had armed goes with the room's state.
		return settleDone, settleOutcome{}
	case err != nil:
		return settleDone, pass.stoppedShort(err)
	}
	// The issue's state is read under the owner's lock this transaction now holds, in the
	// statement after it (lockArtifactOwner), not the room's record of it: a close takes that lock
	// too, so a close this process has not heard of yet is read here, and no close commits until
	// this pass's transaction ends.
	pass.owner, pass.issueClosed = owner, !open
	if err := lockSettlementFence(ctx, tx, s.room); err != nil {
		return settleDone, pass.stoppedShort(err)
	}
	// The owner is read under its lock in this transaction, so the repairs settlement injects
	// below do not read it again: that read would be a second pooled connection taken while
	// this transaction holds one, which is what deadlocks the pool.
	ctx = withOwnerVerified(ctx)
	if step, outcome := pass.readLocked(ctx, parent); step != settleOn {
		return step, outcome
	}
	if step, outcome := pass.applyStamp(ctx); step != settleOn {
		return step, outcome
	}
	if len(pass.slots) > 0 {
		// The stamp is in the room: it commits alone, and the pass ends with it. This return is
		// what keeps the copy lock reconcileAskBlocks takes below out of the transaction that
		// holds the stamp's pending row.
		return pass.commitStamp(ctx)
	}
	// Queued before reconcileAskBlocks reads the durable authors, so a block the stamp renamed is
	// found under its minted id there too: the write phase has not run, so that read takes the rows
	// this queued from the ledger (transactionRecordedAskAuthors).
	if err := recordStampedAskAuthors(ctx, tx, s.room, pass.renamed, pass.carried); err != nil {
		return settleDone, pass.abandon(err)
	}
	pass.reconciliation, err = s.service.reconcileAskBlocks(ctx, tx, s.room, owner, pass.read, pass.credit, pass.issueClosed)
	if err != nil {
		return settleDone, pass.abandon(err)
	}
	// The check blocks are held to the four rules in the same transaction, on the same tree: a
	// ci_check that breaks one is a repair of its own, planned and written with the ask repairs.
	if err := pass.reconciliation.reconcileCICheckBlocks(ctx, tx, s.room, owner, pass.read, pass.credit.actor); err != nil {
		return settleDone, pass.abandon(err)
	}
	// On a closed issue the reconciliation changes no ask (reconcileAskBlocks), and each event it
	// appends - a block.repaired or a block.invalid, of an ask block or a check block - is the
	// settlement's own (SettlementActor), not the person whose edit the version saves: a system
	// actor's event notifies nobody (events.Broker.Notify), and a closed issue's settlement wakes
	// nobody. The person is still named in the event's payload (DisturbedBy) and in the version's
	// authors. The events are named here, before the pass commits the repairs or the version
	// with them (commitRepairs, commitVersion), and applyAnswers only drops some of them.
	if pass.issueClosed {
		for index := range pass.reconciliation.events {
			pass.reconciliation.events[index].Actor = SettlementActor
		}
	}
	if s.service.afterSettleReconcile != nil {
		s.service.afterSettleReconcile(s.room)
	}
	// A reconciliation with repairs ends on planRepairs, which plans them, takes the plan a pass
	// before it made, and decides between the repairs and the version: it alone drops the repairs
	// its plan's decision withheld (applyAnswers), so it alone knows whether any are left to
	// write.
	if len(pass.reconciliation.repairs) > 0 {
		return pass.planRepairs(ctx)
	}
	return pass.commitVersion(ctx)
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

// begin opens the pass's transaction, on the settlements pool the settlement opened for every
// read and write of its own (settleRoomWithin), with the pass's ledger and the context the stages
// after take read and write through, which it answers. The version's display tree is stored on
// the settlements pool too (treePool), once the commit has given the transaction's connection
// back, so the store takes no connection admission counts for requests (commitSettlement).
//
// The transaction runs under the idle-in-transaction bound every settlements-pool connection
// carries (store.RequestIdleInTransactionTimeout, the request pool's). It takes the document
// owner's lock, which every request writing the document waits on until the owner lock's wait runs
// out, and the document's advisory lock, which such a request waits on until the write lock bound
// runs out, so one orphaned with its connection would hold them, refusing those requests, until
// Postgres noticed. No wait inside it outlasts that bound by design: what it waits for before it
// writes into its room is bounded by the settlement's deadline (settlementTimeout), what it has
// left after by settlementFinishBound, and its write phase by a bound of its own (writePhase).
func (p *settlementPass) begin(parent context.Context) (context.Context, error) {
	ledger := &Ledger{service: p.service, ownTx: true, treePool: p.settlements}
	ctx := withRoomCursor(store.WithTransactionTracking(withLedger(parent, ledger)), p.cursor)
	tx, err := p.settlements.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin document transaction: %w", err)
	}
	// Settlement decides everything before it writes anything: every write of the stages after
	// take goes through write and queues on this ledger, and the queue runs back to back right
	// before the commit, so the transaction holds a transaction id only for those statements, not
	// while it renders the document, walks its anchors or carries an approval.
	ledger.writer = tx
	p.ledger, p.tx = ledger, tx
	return ctx, nil
}

// leaseThroughWrite extends the settlement's lease on its document's pending-settlement row
// (leaseSettlementPending) over this pass's write phase, before the pass opens its transaction:
// the lease counts a write phase at its floor (settlementLease), and the bound of one grows with
// the blocks of the tree the pass read (settlementWriteBound). The pass ends by ctx's deadline,
// the settlement's, then the finish of a pass that has written into its room
// (settlementFinishBound) and then its write phase (writePhase), so a lease that ends sooner is
// extended to there (extendSettlementLease): one that ran out under the write phase would let a
// pass arm the document again, as one that runs out under any part of a settlement does. Only a
// document of many blocks needs it - some 130,000 or more where the room was warm, since the lease
// still holds the two cold reads such a settlement never made - and any other's pass writes
// nothing here. The extension is a statement of its own on the settlements pool,
// outside any transaction, as the lease is: made inside the pass's transaction, no pass of the
// resumption would read it before that transaction ended.
func (p *settlementPass) leaseThroughWrite(ctx context.Context) error {
	deadline, ok := ctx.Deadline()
	if p.lease == nil || !ok {
		return nil
	}
	hold := time.Until(deadline) + settlementFinishBound + settlementWriteBound(len(p.offsets))
	if !time.Now().Add(hold).After(*p.lease) {
		return nil
	}
	until, err := extendSettlementLease(ctx, p.settlements, p.room, *p.lease, hold)
	if err != nil {
		return err
	}
	if until != nil {
		p.lease = until
	}
	return nil
}

// take is what the pass takes of its room before it opens a transaction, under no lock of the
// document's: what its version credits (settlementTake), the room copied at an exact commit
// position, and that copy rendered. It answers settleOn where the pass goes on, holding what it
// took, and the step and outcome the pass ends on otherwise.
func (p *settlementPass) take() (settleStep, settleOutcome) {
	s, room, state := p.service, p.room, p.state
	if s.beforeSettleTake != nil {
		s.beforeSettleTake(room)
	}
	// A room retired or replaced since the settlement began holds the instance no longer, and the
	// retry this pass stops short into decides by the room's generation, as when the settlement
	// first holds the room (settleRoomWithin): after a close that moved it, the document is left
	// to what that close arranged; after one that did not, the retry arms the settlement again.
	if s.srv.GetDoc(room) != p.doc {
		return settleDone, p.stoppedShort(fmt.Errorf("take the room to settle: %w", errRoomReplaced))
	}
	state.mu.Lock()
	if s.roomGone(state) {
		s.unlockState(room, state)
		return settleDone, settleOutcome{}
	}
	// An open live write is not in the room yet, and finishLiveWrite arms the settlement once it
	// is published; the pass goes on all the same, because the room it holds may be behind the
	// stored head, and only a pass holding the document's lock catches it up (readLocked). The
	// write is what stops it writing a version: applyStamp defers the settlement to it there.
	p.generation = state.roomGeneration
	p.taken = settlementTake{creditThrough: state.creditSeq.Load()}
	s.unlockState(room, state)
	// The cursor of the instance this settlement holds (doc), which gates the room updates it
	// stores and the repairs it appends, and takes them in once they are stored (commitSettlement).
	p.cursor = s.cursorOf(p.doc)
	if p.cursor != nil {
		p.taken.heldThrough = p.cursor.heldThrough()
	}
	if s.afterSettleAuthorsTake != nil {
		s.afterSettleAuthorsTake(room)
	}
	// The tree is the room at an exact commit position (treePoint): the pass stores every update
	// of the instance through it before it versions the copy (Ledger.flushThrough), and nothing
	// the room commits after it is in that version. The tree is the one the room's replica holds,
	// read under the replica's lock and owned by this read alone. The room's peers and the service
	// can write the live tree while a walk of it, which takes no lock, reads it, and a torn read
	// would be versioned as the document, so the tree is taken at that one position.
	//
	// A pass that found updates but could not read the tree arms their settlement all the same,
	// so the drain's classification is acted on wherever the tree read ends.
	read, point, settles, err := s.treePoint(room, p.doc)
	if settles && p.reads > 0 {
		s.scheduleSettle(room)
	}
	p.reads++
	if err != nil {
		if skipUnreadableSettlement(room, err) {
			return settleDone, settleOutcome{}
		}
		return settleDone, p.stoppedShort(err)
	}
	p.taken.point = point
	p.read = read
	if s.afterSettleTreeRead != nil {
		s.afterSettleTreeRead(room, read)
	}
	if s.beforeSettleRender != nil {
		s.beforeSettleRender(room)
	}
	// What the document renders, with each identified block's range in it: the version a pass
	// that repairs nothing writes.
	p.markdown, p.offsets, p.renderErr = renderTreeWithOffsets(read)
	// A plan made for another read of the document is no plan for this one.
	if p.plan != nil && (p.plan.heldThrough != p.taken.heldThrough || p.plan.markdown != p.markdown) {
		p.plan = nil
	}
	p.stamping = pmdoc.BlockIDRepairCount(read) > 0
	return settleOn, settleOutcome{}
}

// keep records a suppression slot and the update it suppresses, in the order the room's
// persistence worker is handed them.
func (p *settlementPass) keep(slot *suppressSlot, update []byte) {
	if slot != nil {
		p.slots, p.updates = append(p.slots, slot), append(p.updates, update)
	}
}

// finishSlots hands the room's persistence every update this pass wrote into the room, now that
// its transaction holds them.
func (p *settlementPass) finishSlots() {
	for index, slot := range p.slots {
		p.service.finishSuppressedPersistence(slot, p.updates[index])
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
// the first order of a refused update, and fails no room whether or not the pass wrote into it:
// the room closes for its issue instead (endOnRefusedUpdate).
func (p *settlementPass) abandon(err error) settleOutcome {
	if errors.Is(err, ErrIssueClosed) {
		return p.endOnRefusedUpdate(p.slots...)
	}
	if len(p.slots) > 0 {
		p.service.discardSuppressedPersistence(p.room, p.slots...)
		p.service.failRoom(p.room, err)
		return settleOutcome{next: settleRoomFailed, err: err}
	}
	return p.stoppedShort(err)
}

// endOnRefusedUpdate ends a settlement whose room took an update the append gate refused because
// the document's issue closed more than closeMargin before the room took it (ErrIssueClosed,
// observedBeforeClose). That update is in the room's tree and never in the store, so a version of
// that tree would save text the closed issue refused. The settlement therefore saves nothing of
// this room: it discards slots, the updates its pass wrote into the room, fails no room, starts
// no retry and counts no failure, and its pass's transaction rolls back, which keeps the
// document's pending-settlement row. The room closes for its issue (refuseClosedIssueAppend), so
// its browsers reconnect read-only to a room loaded from the store, and the resumption settles the
// document from there (resumeOwedSettlements).
//
// The refusal reaches a settlement in one of two orders. In the first, the pass's own store write
// meets it: its flush of the room's updates (Ledger.flushThrough, storeRepair) answers
// ErrIssueClosed, and abandon or stoppedShort ends here. In the second, the room's persistence
// worker met it first, took the room instance's mark (roomInstance.issueCloseStarted) and dropped
// the update from the room's queue, so no flush of this settlement stores or refuses it, and the
// pass ends here on the mark (roomRefusedUpdate). Whichever meets the refusal first takes that
// mark and starts the room's close; the call here starts nothing when the mark is taken.
func (s *settlement) endOnRefusedUpdate(slots ...*suppressSlot) settleOutcome {
	service := s.service
	service.discardSuppressedPersistence(s.room, slots...)
	service.refuseClosedIssueAppend(s.room, service.instanceOf(s.doc))
	slog.Info("dispatch: document settlement ended on a room update its closed issue refused; the room closes and the document settles from the store",
		"room", s.room)
	return settleOutcome{}
}

// roomRefusedUpdate is whether the room instance this settlement holds took an update the append
// gate refused because the document's issue closed (roomInstance.issueCloseStarted): the second
// order of a refused update (endOnRefusedUpdate), in which the room's persistence worker met the
// refusal first.
//
// Each pass reads it twice: once it holds the document's advisory lock (readLocked, after
// lockSettlementCursor), and after its last store write, before its commit (commitStamp,
// commitRepairs, commitVersion). The worker's append takes that same lock, and the pass holds it
// from the first read to its commit, so no append of the room is refused between the two reads.
// A refusal the worker made before the pass took the lock is the one the first read can miss:
// the worker takes the mark after its refused transaction has released the lock. But it takes the
// mark before it drops the update from the room's queue (StoreUpdateContext, finishUpdateClass).
// So when the pass stores the room's updates, either that update is still queued, and the gate
// refuses the pass's own write (the first order), or it has gone, and its mark was taken before,
// which the second read finds.
func (s *settlement) roomRefusedUpdate() bool {
	instance := s.service.instanceOf(s.doc)
	return instance != nil && instance.issueCloseStarted.Load()
}

// commitFailed ends a pass whose commit returned an error. Its outcome is unknown, and the room's
// cursor took in the versions the pass appended (commitSettlement), so a room it stored any update
// of fails too, and reloads the durable document whichever way the commit went. The commit may
// still land, so the settlement gives back no lease (releaseSettlementLease): a give-back while its
// commit can still land is a lease cleared under a settlement that is not over, and the lease runs
// out instead (settlementLease).
func (p *settlementPass) commitFailed(advanced bool, err error) settleOutcome {
	p.lease = nil
	if advanced && len(p.slots) == 0 {
		p.service.failRoom(p.room, err)
		return settleOutcome{next: settleRoomFailed, err: err}
	}
	return p.abandon(err)
}

// appendEvents queues events in this pass's transaction, as one step of its write phase. Each
// event that carries a reconciled source's text stamps that source's new mention edges with the
// event that introduced them: the version event for the document itself, and each ask's own
// opened/edited event. writeVersionTx reconciled the edges earlier in this transaction, so
// stamping runs after the append, naming the event's id as the id sequence left it
// (refs.QueueStamp), and never touches sequence allocation. What it appends joins the ledger's
// events, after those the version write appended, which the pass publishes once it commits
// (committed). An ask event that names thread_logins has its insert read them
// (settlementEventLogins), after the ask writes queued before it.
//
// caused holds, by an event's place in events, the writes of the node and tuple store that event
// is the cause of (graphstore.Uncaused), in the order they are stated: the version's block index,
// or the node of an ask the pass opened and its author's follow. Their appends are queued right
// after the event's and its stamp, naming the event's id as the id sequence left it
// (graphstore.LatestEvent), so nothing between the two appends another event. A held write is
// read when the step runs, in the write phase, so one an earlier step fills then (createAskBlock)
// is read as that step left it.
func (p *settlementPass) appendEvents(ctx context.Context, events []model.Event, caused map[int][]*graphstore.Uncaused) error {
	if len(events) == 0 {
		return nil
	}
	sink := eventSink(ctx)
	return write(ctx, p.tx, func(b *store.WriteBatch) error {
		for index, planned := range events {
			if err := p.service.events.QueueAppendWithLogins(b, planned, settlementEventLogins(planned), sink); err != nil {
				return err
			}
			if sourceKind, sourceID, ok := referenceSource(planned, p.room); ok {
				refs.QueueStamp(b, sourceKind, sourceID)
			}
			for _, stated := range caused[index] {
				graphstore.QueueCaused(b, *stated, graphstore.LatestEvent)
			}
		}
		return nil
	})
}

// appendReconciliationEvents appends the events this pass's reconciliation decided on
// (appendEvents), each with what it is the cause of: the node of an ask the pass opened and its
// author's follow wait for that ask's ask.opened event and are stated right after it
// (followsByEvent).
func (p *settlementPass) appendReconciliationEvents(ctx context.Context) error {
	follows, err := p.reconciliation.followsByEvent(p.reconciliation.events)
	if err != nil {
		return err
	}
	return p.appendEvents(ctx, p.reconciliation.events, follows)
}

// committed ends a pass whose transaction committed: its writes reach the room's persistence, and
// its events are published. The ask blocks it indexed forgot their recorded authors under the
// commit's own hold of the room's state (consumeAskAuthorsLocked).
func (p *settlementPass) committed() {
	p.finishSlots()
	for _, event := range p.published {
		p.service.events.Publish(event)
	}
	p.reconciliation.logCopiedBlocks(p.room)
	// A copy of an ask this settlement retracted opens its own ask at its next settlement,
	// which its pending row (reconcileAskBlocks, owedCopiesOf) also leaves to the resumption
	// should this process stop first.
	for _, copy := range p.reconciliation.owedCopies {
		p.service.scheduleSettle(copy)
	}
}

// applyStamp stamps the block ids the pass's tree lacks into the room; a pass whose tree lacks none
// writes nothing here. It is also where the pass meets a room gone or a settlement superseded, under
// the one hold of the room's state that records the blocks the stamp renamed. It answers settleOn
// where the pass goes on, and the step and outcome the pass ends on otherwise. A pass whose stamp is
// in the room (p.slots) goes on only to commitStamp; any other that goes on holds the ask blocks'
// sources read under that state.
func (p *settlementPass) applyStamp(ctx context.Context) (settleStep, settleOutcome) {
	s, room, state := p.service, p.room, p.state
	// The stamp is written into the room as it stands, read inside the stamp's own transaction.
	// What says the stamp wrote is the ids it minted, not the update the room took for them: a
	// mutation that changed the tree it found and left the document as it was commits no ygo
	// update, and so returns no slot (applySuppressed, writeLive).
	stamped := false
	if p.stamping {
		slot, update, err := s.applySuppressed(ctx, room, p.doc, func(doc *crdt.Doc, origin any) error {
			var minted int
			var stampErr error
			_, minted, p.renamed, stampErr = stampBlockIDs(doc, origin)
			stamped = minted > 0
			return stampErr
		})
		p.keep(slot, update)
		p.stamp = update
		if err != nil {
			if len(p.slots) == 0 && skipUnreadableSettlement(room, err) {
				return settleDone, settleOutcome{}
			}
			return settleDone, p.abandon(err)
		}
	}

	state.mu.Lock()
	// The authors the stamp carried are recorded durably in the transaction that stores the stamp,
	// whichever way this pass commits it (recordStampedAskAuthors).
	p.carried = state.recordStampedAskBlocks(p.renamed)
	if s.roomGone(state) {
		s.unlockState(room, state)
		s.discardSuppressedPersistence(room, p.slots...)
		return settleDone, settleOutcome{}
	}
	// An open live write is not in the room yet, and since this settlement holds the owner's lock,
	// the write's transaction has already committed: the cursor this settlement versions against
	// includes its row. Write no version; finishLiveWrite arms a settlement once it is published.
	superseded := state.roomGeneration != p.generation
	if state.liveWriter != nil {
		state.settleDeferred = true
		superseded = true
	}
	if len(p.slots) > 0 {
		// The stamp commits alone, superseded or not: the room and its peers hold it, and dropped
		// here it would never reach the store (commitStamp). A superseded settlement ends with it;
		// any other reconciles and versions the stamped document in its next pass.
		p.stampSuperseded = superseded
		s.unlockState(room, state)
		return settleOn, settleOutcome{}
	}
	if superseded {
		s.unlockState(room, state)
		return settleDone, settleOutcome{}
	}
	// A stamp that minted no id found another writer had stamped them first, and the next pass
	// reads the document as that writer left it. One that minted ids the room took no update for
	// left the document as it was, so there is nothing to store or broadcast for it, and the pass
	// goes on and reconciles the tree it read.
	if p.stamping && !stamped {
		s.unlockState(room, state)
		return settleMoved, settleOutcome{}
	}
	// The ask blocks' sources are read now, after the tree they are reconciled on: an update in that
	// tree whose observer has already run has recorded who introduced its blocks, and one whose
	// observer has not left its blocks unseen, for the settlement that observer arms (askBlockSources).
	p.credit.askSources = state.askSources()
	state.mu.Unlock()
	return settleOn, settleOutcome{}
}

// commitStamp stores the stamp applyStamp put into the room and commits it alone, which is all the
// pass does: a superseded settlement ends with it, any other reconciles and versions the stamped
// document in its next pass.
func (p *settlementPass) commitStamp(ctx context.Context) (settleStep, settleOutcome) {
	s, room := p.service, p.room
	// The room holds the stamp, so what is left to store it runs apart from the settlement's
	// deadline, under a bound of its own (finishInRoom): a deadline falling between the write
	// and the commit would fail the room over an update the room already holds.
	ctx, cancel := finishInRoom(ctx)
	defer cancel()
	identityUpdate, err := mergeUpdates(p.updates)
	if err != nil {
		return settleDone, p.abandon(err)
	}
	if err := s.srv.BroadcastUpdate(ctx, room, identityUpdate); err != nil {
		return settleDone, p.abandon(fmt.Errorf("broadcast document identity update: %w", err))
	}
	// The stamp is stored at its place in the room's commit order: every update the room committed
	// before it that no writer has stored first, then the stamp, with the class the room measured
	// for it (storeRepair). The room already holds it (applySuppressed wrote it there), so the
	// instance takes the version it is stored at just before the commit, under the advisory lock
	// this transaction holds until then (commitSettlement).
	if err := p.ledger.storeRepair(ctx, p.tx, room, p.cursor, p.stamp); err != nil {
		return settleDone, p.abandon(fmt.Errorf("store document identity update: %w", err))
	}
	if s.afterSettleRepairAppend != nil {
		s.afterSettleRepairAppend(room)
	}
	// The second read of a refused update's second order (roomRefusedUpdate), after the last
	// store write of this pass and before its commit: the stamp was made on a tree that holds an
	// update the room's closed issue refused, so none of it is committed, and the room closes for
	// its issue, whose browsers reload what the store holds.
	if p.roomRefusedUpdate() {
		return settleDone, p.endOnRefusedUpdate(p.slots...)
	}
	// The authors the stamp carried, recorded in the transaction that stores the stamp: whichever
	// process settles the stamped document next finds a renamed block's author under its minted id.
	if err := recordStampedAskAuthors(ctx, p.tx, room, p.renamed, p.carried); err != nil {
		return settleDone, p.abandon(err)
	}
	// The write phase: every statement this pass decided on, in the order it made them, under the
	// bound of its own a write phase runs to (writePhase).
	write, endWrite := writePhase(ctx, nil, len(p.offsets))
	defer endWrite()
	if err := p.ledger.runQueue(write, nil); err != nil {
		return settleDone, p.abandon(writeFailed(write, err))
	}
	if advanced, err := s.commitSettlement(write, p.tx, p.ledger, room, authorCapture{}, nil); err != nil {
		return settleDone, p.commitFailed(advanced, fmt.Errorf("commit document identity update: %w", err))
	}
	p.finishSlots()
	p.stored = true
	if p.stampSuperseded {
		return settleDone, settleOutcome{}
	}
	return settleAgain, settleOutcome{}
}

// planRepairs answers for the repairs this pass's reconciliation found, and ends the pass on them:
// it plans the ones no pass has planned - rolling this pass's transaction back, since the plan
// weighs their answers and renders the repaired tree, which no pass does under the document's
// locks - and takes the plan a pass before it made when this reconciliation is that plan's. Once
// that plan holds, the repairs it leaves are written into the room (commitRepairs), and a plan
// that leaves none versions the document instead (commitVersion).
func (p *settlementPass) planRepairs(ctx context.Context) (settleStep, settleOutcome) {
	s, room := p.service, p.room
	if p.plan == nil {
		// Repairs no pass has planned: this pass's transaction goes, and the repairs are
		// planned without its locks, where their answers are weighed and the repaired tree
		// rendered. The next pass commits them if it reconciles the document to the same tree.
		_ = p.tx.Rollback(context.WithoutCancel(ctx))
		if s.beforeSettleRender != nil {
			s.beforeSettleRender(room)
		}
		answers, repaired, ok := p.reconciliation.weighAnswers(p.read, p.markdown)
		var repairedErr error
		if !ok {
			repaired, repairedErr = renderTree(p.read)
		}
		p.plan = &repairPlan{
			heldThrough: p.taken.heldThrough, markdown: p.markdown, answers: answers,
			tree: p.read, repaired: repaired, repairedErr: repairedErr,
		}
		return settleAgain, settleOutcome{}
	}
	p.planned, p.plan = p.plan, nil
	// Block ids are left out of the comparison: a decided ask's wording written back into its
	// block (writeDecidedWording) mints ids for the blocks it rewrites, other ones each time,
	// and no rendering carries a paragraph's id.
	if !p.reconciliation.applyAnswers(room, p.planned.answers) || !p.read.Equal(p.planned.tree) {
		// The ask rows moved since the plan was made, so its repairs are not this
		// reconciliation's: the next pass plans them again.
		return settleMoved, settleOutcome{}
	}
	// The repairs as the plan's decision leaves them: applyAnswers above drops every repair a
	// withheld answer makes nothing of, since withholding an answer the document has no room for
	// leaves the block exactly as this pass found it - the shape an earlier pass's own repair
	// wrote into the room. A reconciliation left with no repair has nothing to write, so the
	// document is versioned here. Written into the room instead, those repairs would change
	// nothing, which commitRepairs reads as the document having moved under them, and the
	// settlement would plan the same withheld answers again until its pass bound, leaving the
	// document owed for good. This is the one place the count is read after the prune, so no
	// caller can read it before.
	if len(p.reconciliation.repairs) > 0 {
		return p.commitRepairs(ctx)
	}
	return p.commitVersion(ctx)
}

// commitRepairs writes the planned repairs into the room and commits them, with the ask rows and
// events their reconciliation wrote, and no version: the document they leave is versioned by the
// next pass, which renders it outside the lock. The pending-settlement row and the pending authors
// stay for that version. What the repaired document renders is the plan's.
func (p *settlementPass) commitRepairs(ctx context.Context) (settleStep, settleOutcome) {
	s, room, state := p.service, p.room, p.state
	// The repairs are made on the document as it stands (writeLive): the tree they were
	// reconciled on was read before settlement's database work.
	repaired := false
	slot, update, err := s.applySuppressed(ctx, room, p.doc, func(doc *crdt.Doc, origin any) error {
		var writeErr error
		repaired, writeErr = p.reconciliation.writeLive(doc, origin)
		return writeErr
	})
	p.keep(slot, update)
	if err != nil {
		return settleDone, p.abandon(fmt.Errorf("write reconciled typed blocks: %w", err))
	}
	// Repairs that found nothing to change in the tree they were written into found the document
	// moved under them: a browser removed or changed the block a repair was for. The rollback
	// leaves the document as the browser left it, and the next pass reads it again. A pass whose
	// repairs were all dropped writes no repairs at all and never reaches here (planRepairs):
	// nothing moved under it. What says they wrote is their own answer, not the update the room
	// took for them: repairs that changed the tree and left the document as it was commit no ygo
	// update (writeLive), and the rows and events they reconciled are committed here all the same.
	if !repaired {
		return settleMoved, settleOutcome{}
	}
	// The room holds whatever the repairs left in it, so what is left to store them runs apart
	// from the settlement's deadline, under a bound of its own (finishInRoom): a deadline falling
	// between the write and the commit would fail the room over updates the room already holds.
	ctx, cancel := finishInRoom(ctx)
	defer cancel()
	if slot != nil {
		merged, err := mergeUpdates(p.updates)
		if err != nil {
			return settleDone, p.abandon(err)
		}
		if err := s.srv.BroadcastUpdate(ctx, room, merged); err != nil {
			return settleDone, p.abandon(fmt.Errorf("broadcast document closure update: %w", err))
		}
	}
	// What this pass stores, it stores from here on, after the reconciliation: the room's updates
	// through the point it read the room at, in the room's commit order (Ledger.flushThrough),
	// then the repair at its own place among them, with the class the room measured for it
	// (storeRepair). The instance takes the versions they are stored at just before the commit,
	// under the advisory lock this transaction holds until then (commitSettlement). Repairs the
	// room took no update for have nothing to broadcast and nothing to store.
	if err := p.ledger.flushThrough(ctx, p.tx, room, p.taken.point); err != nil {
		return settleDone, p.abandon(fmt.Errorf("store the room's updates through the settlement's read: %w", err))
	}
	if slot != nil {
		if err := p.ledger.storeRepair(ctx, p.tx, room, p.cursor, update); err != nil {
			return settleDone, p.abandon(fmt.Errorf("store reconciled typed blocks: %w", err))
		}
		if s.afterSettleRepairAppend != nil {
			s.afterSettleRepairAppend(room)
		}
	}
	contentChanged, err := contentChangedSinceVersion(ctx, p.tx, room, p.latest.docUpdateVersion)
	if err != nil {
		return settleDone, p.abandon(err)
	}
	// The events name the version the next pass leaves the document at: the one after the
	// latest when that pass versions the repaired document, the latest otherwise. That pass
	// versions it by commitVersion's test (latestVersionRow.holds).
	settledVersion := p.latest.Number
	if contentChanged && p.planned.repairedErr == nil && !p.latest.holds(p.planned.repaired) {
		settledVersion++
	}
	// The second read of a refused update's second order (roomRefusedUpdate), beside the read of
	// a room gone, after the last store write of this pass and before its commit: the repairs were
	// written into a tree that holds an update the room's closed issue refused.
	if p.roomRefusedUpdate() {
		return settleDone, p.endOnRefusedUpdate(p.slots...)
	}
	state.mu.Lock()
	if s.roomGone(state) {
		s.unlockState(room, state)
		s.discardSuppressedPersistence(room, p.slots...)
		return settleDone, settleOutcome{}
	}
	s.unlockState(room, state)
	if err := p.reconciliation.nameVersion(ctx, p.tx, room, p.owner, settledVersion); err != nil {
		return settleDone, p.abandon(err)
	}
	// The reconciliation's events, the follows of the asks this pass opened each stated right
	// after its own ask.opened (appendReconciliationEvents).
	if err := p.appendReconciliationEvents(ctx); err != nil {
		return settleDone, p.abandon(err)
	}
	// The write phase: every statement this pass decided on, in the order it made them, under the
	// bound of its own a write phase runs to (writePhase).
	write, endWrite := writePhase(ctx, nil, len(p.offsets))
	defer endWrite()
	if err := p.ledger.runQueue(write, nil); err != nil {
		return settleDone, p.abandon(writeFailed(write, err))
	}
	p.published = p.ledger.events
	if advanced, err := s.commitSettlement(write, p.tx, p.ledger, room, authorCapture{state: state}, func(err error) {
		// The room forgets the author of each ask block the commit indexed under the commit's own
		// hold, so no reader that finds the block's ask row committed then finds the room still
		// owing its author.
		if err != nil {
			return
		}
		p.credit.consumeAskAuthorsLocked(p.reconciliation.indexedAskBlocks)
		if state.roomGeneration == p.generation {
			state.settleFailures = 0
		}
	}); err != nil {
		return settleDone, p.commitFailed(advanced, fmt.Errorf("commit document settlement repairs: %w", err))
	}
	if s.afterSettleRepairCommit != nil {
		s.afterSettleRepairCommit(room)
	}
	p.committed()
	p.stored = true
	return settleAgain, settleOutcome{}
}

// commitVersion ends a settlement whose document needs no stamp and no repairs: it writes the
// document's version, when the document now reads differently from the latest one, with the rows
// and events of its reconciliation, and clears the pending-settlement row the settlement was owed
// on.
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
// (settlePass).
func (p *settlementPass) commitVersion(ctx context.Context) (settleStep, settleOutcome) {
	s, room, state := p.service, p.room, p.state
	if p.renderErr != nil {
		if skipUnreadableSettlement(room, p.renderErr) {
			return settleDone, settleOutcome{}
		}
		return settleDone, p.stoppedShort(p.renderErr)
	}
	state.mu.Lock()
	if s.roomGone(state) {
		s.unlockState(room, state)
		return settleDone, settleOutcome{}
	}
	// A generation that moved while the pass reconciled moved for a change this pass's read may
	// lack - a browser's edit arming its settlement, a write opening - so the pass writes no
	// version, and the next reads the document again (or, for a write still open, leaves it to
	// the write).
	if state.roomGeneration != p.generation {
		s.unlockState(room, state)
		return settleMoved, settleOutcome{}
	}
	s.unlockState(room, state)

	// What this pass stores, it stores from here on, after the reconciliation: every update of
	// the room's instance through the point it read the room at that no writer has stored yet, in
	// the room's commit order (Ledger.flushThrough). The head it leaves is the one the version
	// records as its doc_update_version, so the stored updates through it fold to the text the
	// version holds; an update the room committed after that read is stored by neither, and is
	// the next version's.
	if err := p.ledger.flushThrough(ctx, p.tx, room, p.taken.point); err != nil {
		return settleDone, p.abandon(fmt.Errorf("store the room's updates through the settlement's read: %w", err))
	}
	// The second read of a refused update's second order (roomRefusedUpdate), after the last
	// store write of this pass was gated (flushThrough and storeRepair decide in the read phase
	// what the write phase stores) and before its commit: what it would commit is rendered from a
	// tree that holds an update the room's closed issue refused.
	if p.roomRefusedUpdate() {
		return settleDone, p.endOnRefusedUpdate(p.slots...)
	}
	// The head the version records (doc_update_version) counts what this transaction has queued
	// and not yet written (transactionHead): the pass's appends are among them.
	snapshotCursor, err := transactionHead(ctx, p.tx, room)
	if err != nil {
		return settleDone, p.abandon(err)
	}
	p.snapshotCursor = snapshotCursor
	// The version's authors, taken once those appends are queued: the pending authors then hold
	// the authors they write (transactionPendingAuthors), so a credit the take did not bound whose
	// update this pass stores is listed too.
	if err := p.takeCredit(ctx); err != nil {
		return settleDone, p.abandon(err)
	}
	// The version takes the rendering this pass made of read before its transaction. Only a
	// recorded repair changes read in place (settlementReconciliation.repair, repairAsk); a pass
	// that holds one has ended above, and one whose repairs the plan's decision dropped put every
	// block it changed back as it found it (applyAnswers), so read is the tree that rendering was
	// made of.
	if s.beforeSettleVersion != nil {
		s.beforeSettleVersion(room, p.read)
	}

	// Settlement writes a version only when the document now reads differently from the latest
	// one. Indexing an ask block writes an `asks` row and an `ask.opened` event over words the
	// edit that wrote the block already versioned, so a version for that event alone would be
	// byte-identical and credited to nobody (LEGION-273, where it staled an approval pinned to what
	// an agent had just written; carryApproval now carries that approval to such a version
	// instead, an event nothing in the log would explain). Its tree writes - stamping block ids,
	// restoring an ask block's server-owned attributes - move the stored Proof state and often
	// render the same markdown, and a version for one of those would repeat the version before it
	// too (LEGION-229 requirement 2).
	// contentChanged stays the first half of the test: a version an older renderer wrote is not
	// this settlement's to canonicalise when nothing has touched the document since. The second
	// half is whether the latest version holds this rendering (latestVersionRow.holds): byte for
	// byte, or, for a version another renderer version wrote or none stamped, as markdown that
	// reads as it. So a renderer change leaves no version behind it either where an edit was made
	// and undone since, which changed the content and changed it back: the version holds the
	// same tree in the spelling its renderer wrote.
	contentChanged, err := contentChangedSinceVersion(ctx, p.tx, room, p.latest.docUpdateVersion)
	if err != nil {
		return settleDone, p.abandon(err)
	}
	versioning := contentChanged && !p.latest.holds(p.markdown)
	settledVersion := p.latest.Number
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
	if p.issueClosed && !p.credit.actor.SameAs(SettlementActor) {
		kept = &p.credit.actor
	}
	if p.issueClosed && kept == nil && p.savedNothing(versioning) {
		if err := clearSettlementPendingRow(ctx, p.tx, room); err != nil {
			return settleDone, p.abandon(err)
		}
	} else if err := clearSettlementPending(ctx, p.tx, room, kept); err != nil {
		return settleDone, p.abandon(err)
	}
	if err := p.reconciliation.nameVersion(ctx, p.tx, room, p.owner, settledVersion); err != nil {
		return settleDone, p.abandon(err)
	}
	if versioning {
		var keptEditor *model.Actor
		if p.credit.kept {
			keptEditor = &p.credit.actor
		}
		result, writeErr := s.writeVersionTx(ctx, p.tx, room, renderedTree{tree: p.read, markdown: p.markdown, offsets: p.offsets}, p.credit.versionActor(), &versionWrite{
			authors:          p.credit.authors,
			capture:          &p.credit.capture,
			docUpdateVersion: &p.snapshotCursor,
			closedIssue:      p.issueClosed,
			settlement:       true,
			keptEditor:       keptEditor,
		})
		if writeErr != nil {
			// The version a human approved holds an element this build does not know: the version
			// is left, unwritten, to a build that reads it, as a tree holding one is.
			if len(p.slots) == 0 && errors.Is(writeErr, ErrDocNewerContent) && skipUnreadableSettlement(room, writeErr) {
				return settleDone, settleOutcome{}
			}
			return settleDone, p.abandon(writeErr)
		}
		// The version lists the pending authors this settlement read, so its commit deletes them.
		if err := deleteCapturedPendingAuthors(ctx, p.tx, room, p.credit.capture); err != nil {
			return settleDone, p.abandon(err)
		}
		if s.afterSettleVersionWrite != nil {
			if err := s.afterSettleVersionWrite(room); err != nil {
				return settleDone, p.abandon(err)
			}
		}
		// A person's version opens or moves on the document's edit notice, and a closed issue's
		// deletes it (settledEditNotice).
		if err := s.settledEditNotice(ctx, p.tx, room, p.issueClosed, result.version.Number, p.credit.authors, p.credit.capture.latest); err != nil {
			return settleDone, p.abandon(err)
		}
		// A document body cites nodes whose rows carry a backlink count, so the version event
		// names what this settle moved exactly as a message or comment write does.
		versionEvent := model.Event{
			IssueKey: p.owner.IssueKey,
			Type:     "artifact.version",
			Actor:    p.credit.versionActor(),
			Payload:  ArtifactVersionEventPayload(room, p.owner.Name, result.version, nil, result.changes),
		}
		if p.owner.IssueKey == nil {
			versionEvent.ArtifactID = &room
		}
		// What this settlement's block index stated waits for that event: its id is the cause
		// every node and tuple of the pass carries, and it exists only once the event's insert has
		// run, so it is stated right after it (appendEvents).
		if err := p.appendEvents(ctx, []model.Event{versionEvent}, map[int][]*graphstore.Uncaused{0: {&result.blocks}}); err != nil {
			return settleDone, p.abandon(err)
		}
	}
	// The reconciliation's events, the nodes and the follows of the asks this pass opened each
	// stated right after its own ask.opened (appendReconciliationEvents).
	if err := p.appendReconciliationEvents(ctx); err != nil {
		return settleDone, p.abandon(err)
	}
	// The write phase: every statement this pass decided on, in the order it made them. It runs
	// apart from the settlement's deadline, under a bound of its own that grows with the
	// document's blocks, its version's block index among those statements (writePhase): ended by
	// the deadline, a version of many blocks would never be written.
	write, endWrite := writePhase(ctx, p.caller, len(p.offsets))
	defer endWrite()
	if err := p.ledger.runQueue(write, nil); err != nil {
		return settleDone, p.abandon(writeFailed(write, err))
	}
	p.published = p.ledger.events
	// The commit marks the in-flight credits the version listed consumed before it releases the
	// room's state, and the room updates this pass stored marked stored (commitSettlement); a
	// settlement that writes no version takes out none. The room forgets the author of each ask
	// block the commit indexed under the same hold, so no reader that finds the block's ask row
	// committed then finds the room still owing its author.
	consumed := authorCapture{state: state}
	if versioning {
		consumed = p.credit.capture
	}
	if advanced, err := s.commitSettlement(write, p.tx, p.ledger, room, consumed, func(err error) {
		// This runs before the publish below, the order Ledger.Commit keeps for every other
		// version write, so a subscriber acting on this version's artifact.version event acts
		// after it.
		if err != nil {
			return
		}
		p.credit.consumeAskAuthorsLocked(p.reconciliation.indexedAskBlocks)
		if state.roomGeneration == p.generation {
			state.settleFailures = 0
			state.unsettled = len(state.inflight) > 0
			if !state.unsettled {
				state.lastActor = nil
			}
		}
	}); err != nil {
		return settleDone, p.commitFailed(advanced, fmt.Errorf("commit document settlement: %w", err))
	}
	// The commit deleted the row, and the lease with it (clearSettlementPending, or
	// clearSettlementPendingRow for a closed issue's settlement that saved nothing).
	p.lease = nil
	p.committed()
	// A closed issue's settlement sweeps no marks: the unmark is an injection without the
	// owner-verified mark, which allowInject refuses on a closed issue. The document's next
	// settlement on the open issue sweeps them: at the reopen, one runs only for a document the
	// reopen writes or marks owed.
	if !p.issueClosed {
		s.sweepUnrecordedMarks(room, p.read)
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

package docs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/persistence"

	"github.com/sjawhar/envoy/internal/dispatch/asks"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/texttrees"
)

// Ledger is what the document operations joined to one database transaction produce until it
// ends: the events they generate, their writes to live documents (see liveWrite) and the versions
// they write. Join, and joinAtOnce for the decisions step's restore of one issue, are the only ways
// to join a transaction, so an operation that has one always has its ledger. Commit ends the
// transaction the way every caller must: it runs the transaction's write queue, records the
// writes' authors and takes out the authors its versions listed in the transaction itself, runs
// the steps registered to follow those writes (AfterWrites), commits, marks the in-flight credits
// its versions listed consumed, records the writes' latest edit source in their rooms, and
// publishes the writes to their rooms, in that order. Discard, which callers defer right after
// Join, drops whatever a transaction that did not commit left behind.
//
// A joined transaction runs in two phases. Its read phase takes the locks it needs - advisory
// locks, none of which gives it a transaction id - reads, and does every operation's document
// work; each operation queues its writes (write) instead of running them, and the caller queues
// its own beside them (Queue). The write phase is Commit: every queued write puts its statements
// on one batch, in the order the writes were made, so the events they append keep their order,
// and the batch goes to the server pipelined, in one round trip (store.WriteBatch), with the
// statements recording the transaction's settlement credit after them; the steps a caller
// registered to follow the batch run next (AfterWrites), and the commit follows. The transaction
// holds a transaction id from the batch's first write to its commit, and no document work runs in
// between; nor does a wait for this process to read a result, but for the round trip of each step
// after the batch, which needs a value the batch's results give in Go and none other can. What a
// queued write leaves that a later read of the same transaction would have seen - a document's
// head, a version's number and time, an anchor's quote, an approval request's state, a source's
// references - the ledger holds in memory (pendingRoom, NextVersion, RegisterPendingAnchor,
// recordPendingApproval, referenced), and the read takes it from there.
//
// A writer that runs its operations in a transaction of its own, which no caller joined, carries
// no `tx` and marks its ledger `ownTx` instead: a settlement (settleRoomWithin) and the block-id
// backfill (backfillBlockIDs) are the two. A settlement's ledger queues its writes all the same
// (writer), and the settlement runs the queue before it commits; the backfill's sets no writer,
// so its writes run at once (write, store.RunWrite). The decisions step's restore of one issue
// (DecisionStep.restoreIssue) is the third writer of a transaction of its own: it joins it with
// joinAtOnce, whose ledger carries that `tx`, so Commit commits it, and sets no writer, so its
// writes run at once, in the order it makes them, as the backfill's do. Those are the ledgers
// document operations run under: every operation that writes a document either joins its caller's
// transaction or runs in the transaction of one of those three writers. A background pass with no
// document operation queues its writes on a bare ledger (queueWrites).
type Ledger struct {
	service *Service
	tx      pgx.Tx
	// writer is the transaction whose writes this ledger queues: the joined transaction, a
	// writer's own transaction, or a background pass's (queueFor). It is nil on a ledger whose
	// writes run at once: the block-id backfill's and the decisions step's restore's (joinAtOnce).
	writer pgx.Tx
	// ownTx marks a ledger whose operations run inside a transaction of the writer's own, which
	// no caller joined: a settlement's (settleRoomWithin) and the block-id backfill's
	// (backfillBlockIDs).
	ownTx  bool
	events []model.Event
	// live holds, per document, the writes this transaction made to it; order is the order it
	// first wrote them in.
	live     map[string]*liveWrite
	order    []string
	versions []ledgerVersion
	// versionTexts is the markdown of every version this transaction wrote, with the document it
	// was written to. Its commit stores each one's display tree (storeVersionTrees), the markdown
	// being a stored text like a comment's body. It is a list of its own rather than a field of
	// ledgerVersion because a version a writer records nothing else about - one written with no
	// author capture - still stores its tree.
	versionTexts []ledgerVersionText
	// seeds holds, per document this transaction seeded (SeedText), the actor that wrote its
	// first text and the tree it wrote, which credit records and registers as that actor's ask
	// blocks once the transaction has committed.
	seeds map[string]ledgerSeed
	// rebuilds are the documents this transaction rebuilds (RebuildDocument), whose rooms refuse
	// loads until it ends, committed or not.
	rebuilds []string
	// readBacks holds, per document this transaction wrote whole (SeedText, ReplaceText), the
	// read-back its parse already made of the markdown it stores (pmdoc.ParseWritten), so a step
	// of the same request that reads that markdown again shares the parse (ReadBackOf). Discard
	// drops them when the request ends: each is a whole parsed tree of its document.
	readBacks map[string]pmdoc.WrittenReadBack
	// copies are the documents showing a copy of an ask this transaction answered or resolved,
	// whose settlement it marked owed (SettleCopiesOf); Commit arms each once it has committed.
	copies []string
	// announced is, for each document this transaction appended to, the last version it appended,
	// in the order it first appended to each: what its commit announces to every process, once
	// per document (announceAppends), rather than once per append (appendUpdateTxClass).
	announced []ledgerAnnouncement
	// flushes are the room updates this transaction stored ahead of its own appends, one flush
	// per document (flushThrough), which its commit marks stored.
	flushes []*RoomFlush
	// issueRooms holds, per document this transaction resolved into an issue room it is not
	// (Service.targetOf), that room: the transaction's write to the document is the room's
	// (liveWriteFor).
	issueRooms map[string]string
	// removals holds, per document, each answered ask a person's own write of this transaction
	// took the block of out of that document, by ask id (answerDroppedAnsweredAsks), which the
	// version the write leaves states on the ask's node (removalsOf).
	removals map[string]map[string]heldRemoval
	// queue is the transaction's write phase (WriteStep): every write its operations and its
	// caller made, in the order they made them, which Commit sends as one batch before it commits.
	// closing is the steps queued for the end of that batch (writeLast), after every step of queue
	// and the version_renders rows the batch writes, in the order they were queued. sealed is set
	// once the write phase has begun or the ledger was discarded, after which nothing more is
	// queued (Queue, AfterWrites). batch is the write phase's batch while runQueue builds it, on
	// which a write made meanwhile in the ledger's transaction puts its statements (write).
	// afterWrites are the steps the caller registered to run once that batch has run and before
	// the commit (AfterWrites), in the order it registered them.
	queue       []WriteStep
	closing     []WriteStep
	sealed      bool
	batch       *store.WriteBatch
	afterWrites []WriteStep
	// artifacts is the address of each artifacts row this transaction creates and has not
	// written yet, by artifact id (RegisterPendingArtifact), and issues the key of each issues
	// row it creates and has not written yet (RegisterPendingIssue): the two rows a write
	// indexing its own new issue's spec cites before either exists (refs.Pending). queries holds,
	// by the id of each of those artifacts rows whose node this transaction writes as a query node
	// (graphstore.QueryNode), that node's query, which the nodes of its versions read
	// (ReadVersionOwner) and which names the record that covers its versions (readCovered).
	artifacts map[string]refs.DocumentAddress
	queries   map[string]string
	issues    map[string]struct{}
	// rooms holds, per document whose stored history this transaction has queued a write to, the
	// state its queued writes leave it in (pendingRoom).
	rooms map[string]*pendingRoom
	// numbers is, per document this transaction has numbered a version of, the number it gave the
	// last one (NextVersion), and latest that version as it will stand once written, which
	// latestVersion answers for it; now is the transaction's start, read once (Now). latest is read
	// only while the ledger is unsealed (PendingVersionMarkdown, PendingVersionNumber,
	// PendingMarkdown, queueFor), and Discard drops it: each entry holds its version's whole
	// markdown.
	numbers map[string]int
	latest  map[string]pendingVersion
	now     *time.Time
	// anchors holds, per document, the anchored ask and comment rows this transaction writes, as
	// they will stand (RegisterPendingAnchor), in the order they were first registered.
	anchors map[string][]*PendingAnchor
	// approvals holds, per document, its approval request as this transaction's queued writes
	// leave it (recordPendingApproval): the row its version write moved to that version
	// (MoveApprovalAsk), edited_at included, and the answered row where its carry answered the
	// request (answerApprovalAsk). It is what the carry that reads it next moves nothing of
	// (openApprovalAsk answers none for an answered row), which the asks table does not answer
	// for before the write phase has run. Once the write phase has run it is the request as the
	// transaction wrote it, a dropped carry's answer undone (restoredCarry.drop), which the write's
	// own report of the request it moved reads (WrittenApproval, api.movedApprovalRequestAdvice).
	approvals map[string]*model.Ask
	// reviews holds, per document, the newest review this transaction has queued, which the
	// table answers only once the write phase has run (PendingReview), and once that phase has run
	// the review it wrote, a dropped carry's review taken off again (WrittenReview). The carry's
	// own review is the only one recorded here (writeCarriedReview, through recordPendingReview),
	// and refuseSecondCarry reads an entry's presence as proof that this transaction has carried an
	// approval of that document already: a write path that starts recording a review here would
	// refuse the carry of any later version of the same document in the same transaction.
	reviews map[string]model.ArtifactReview
	// referenced is, per source, the targets its references cite once this transaction's queued
	// reconciliation of them is written, and blockIndexes, per document, what its block index holds
	// once its queued indexing is written: what a later reconciliation of the same source in this
	// transaction plans against (planReplaceCounted, planBlockIndex). storedBlocks is, by node
	// address, each block a move of the node route places in this transaction
	// (HoldStoredBlock), which every block index this transaction plans reads in place of what the
	// store holds of the block (heldBlockIndex).
	referenced   map[refs.Target][]refs.Target
	blockIndexes map[string]refs.BlockIndex
	storedBlocks map[string]refs.StoredBlock
	// askParts holds, per ask this transaction has planned the parts of, what its part index
	// holds once that plan is written: what a later plan of the same ask's parts in this
	// transaction runs against (planAskPartIndex).
	askParts map[string]refs.ItemIndex
	// askBlocks holds, per document, the block asks whose rows this transaction has queued writes
	// of, by block id, as they will stand (recordAskBlock, loadAskBlocks).
	askBlocks map[string]map[string]model.Ask
	// owedAuthors holds, per document, the pending authors this transaction has queued writes of
	// (queuePendingAuthors, which every writer of doc_pending_authors' insert queues through),
	// by actorKey: the appends it makes for a room credit the browser edits they store, and
	// doc_pending_authors holds none of those rows until the write phase has run, so the
	// versions this transaction writes read them from here as well as from the table
	// (transactionPendingAuthors).
	owedAuthors map[string]map[string]model.Actor
	// askAuthors holds, per document, the durable ask block authors this transaction has queued
	// writes of (queueAskAuthors, which every writer of doc_ask_block_authors' insert queues
	// through), by block id: doc_ask_block_authors holds none of those rows until the write
	// phase has run, so what this transaction reads of them it reads from here as well as from
	// the table (transactionRecordedAskAuthors).
	askAuthors map[string]map[string]model.Actor
	// restored holds the ids of the asks this transaction writes back open from the withdrawal of
	// their issue's close (WriteRestoredAsks), each of which its caller opens again with an
	// ask.opened queued after that write (api.recordRestoredAsksTx). The asks table reads them
	// withdrawn, and the events table holds no such ask.opened, until the write phase has run, so
	// the reopen's reads that must see them restored take them from here (restoredAsksOf): its
	// mark (MarkReopenOwedAskDocuments), and its follow's request (openApprovalAsk).
	restored []string
	// settledAsks holds, per ask, the last change of its answer state this transaction has planned
	// and queued (PlanSettle): the asks row and the ask's node hold none of it until the write
	// phase has run, so a later change of the same ask in this transaction is planned from it
	// (asks.PlanSettleAfter) rather than from the tables.
	settledAsks map[string]asks.Settled
	// stated holds each version a pass of this transaction stated (recordStatedVersions), whose
	// version_renders row the write phase writes where it renders from the tree (readCovered).
	// covered is, for each stated version that renders from the tree, the issue and the query
	// whose record covers it - a query version this build's version write made - or the issue
	// alone, with no query, for a version of a document no query record covers (an upload, a
	// project's document, the issue room's own fragment); read once the queue is sealed and before
	// the write phase's first batch is sent (readStatedDocuments).
	stated  []statedVersion
	covered map[statedVersion]queryOf
	// fills is each issue whose query versions this transaction's write phase left with their
	// record's digest owed (queueStatedRenders), in the order the write phase met them, whose fill
	// runs once the transaction has committed and its rooms' writes are published, and before its
	// events are published (fillOwedDigests).
	fills []string
	// deferred is each version this transaction queued that renders from the tree and that no pass
	// of it stated - one whose write left its blocks to the block-index reconciliation, or left an
	// issue room owed - whose version_renders row the write phase inserts with no time, no seq and
	// no digest, for the first pass that states it to fill (readStatedDocuments,
	// queueStatedRenders; R15).
	deferred []statedVersion
	// mismatched is each version a reconciliation's pass of this transaction stated from a tree
	// that does not render as the version's stored markdown, byte for byte (recordTreeMismatch),
	// taken out of stated: its row takes no time, no seq and no digest, and is marked
	// tree_mismatch instead (writeStatedRenders, queueTreeMismatch; R15). firstStated is each
	// version whose version_renders row took its time from this transaction's write phase
	// (firstStatedOn), in the order the rows were written, which the reconciliation's pass carries
	// an approval to once the rows stand (carryFirstStated; R14).
	mismatched  []statedVersion
	firstStated []statedVersion
	// restores holds each carry to a version that restores the approved text and takes its
	// version_renders row in this transaction (carryRestored): its writes run in their place in
	// the write phase, before the version's artifact.version, behind a savepoint the write phase
	// takes before that version's insert, and the fill holds each one's pinned digest to the
	// version's record (writeRestored). restoring is the one whose carry runs now, which routes the
	// carry's writes into the queue as steps the write phase can drop (write) and records how to
	// undo what the carry leaves on the ledger (noteUndo).
	restores  []*restoredCarry
	restoring *restoredCarry
	// answering is, while a carry answers an approval request that names several documents
	// (answerOn), the restore carries whose queued reviews the carry read to decide that every
	// document stands approved: its answer's writes are steps the write phase drops once one of
	// them is dropped (write), and what the answer leaves on the ledger is undone with it
	// (noteUndo).
	answering []*restoredCarry
	// written is set once the write phase has run whole (commit): what the ledger then holds of a
	// carry - its review, the request its answer left - is what the transaction wrote, a dropped
	// carry's undone, which WrittenReview and WrittenApproval answer.
	written bool
}

// WriteStep is one step of a transaction's write phase: it puts the statements of one write on the
// phase's batch, with the callbacks that read their results (store.WriteBatch), and runs nothing.
// Everything the write decides on - its locks, its reads, the document work - runs before it is
// queued, in the transaction's read phase, so a step renders nothing, walks no tree and encodes no
// document, and it reads no result: the batch is sent once every step has queued its statements.
// A value a later statement needs from an earlier one is derived in the later statement's SQL (an
// event's sequence from its owner row, its id as store.LatestEventID); a statement that needs one
// it cannot derive there goes on a step registered to follow the batch instead (AfterWrites). A
// value the caller needs once Commit has returned - an appended event's id, a row's generated
// timestamp - is written by the statement's callback through a variable the step captured, and
// only that caller reads it, or a step it registered to follow the batch. A step's own error - an
// argument it cannot encode - ends the write phase as a failed statement would, after the
// statements queued before it.
type WriteStep func(b *store.WriteBatch) error

// ErrQueueSealed refuses a write queued (Queue) or a step registered to follow the write phase
// (AfterWrites) once the ledger's write phase has begun (Commit) or the ledger was discarded: a
// queued write would run after statements it was meant to precede, or in no transaction at all,
// and a step registered once the write phase has begun would never run. It is a programming error
// in the caller, which Queue and AfterWrites report rather than panic on, so a handler answers it
// as any other failure of its transaction.
var ErrQueueSealed = errors.New("docs: the transaction's write queue is sealed: Commit has begun or the ledger was discarded")

// Queue appends step to the transaction's write phase, after every write queued so far: a route
// joined to the ledger queues each of its own writes here at the point it makes it, so the batch
// carries the transaction's statements, the document operations' and the route's, in the order
// they were made. It refuses a ledger no caller joined and a sealed one (ErrQueueSealed).
func (l *Ledger) Queue(step WriteStep) error {
	if l.tx == nil {
		return errUnjoined
	}
	if l.sealed {
		return ErrQueueSealed
	}
	l.queue = append(l.queue, step)
	return nil
}

// QueueFirst puts step at the head of the transaction's write phase, before every write queued so
// far: for rows a route writes after its read phase has queued document work that reads them -
// an issue's room, which the spec's queued version, fence and index resolve the spec's room
// through - so the rows join the write phase's batch, ahead of that work, rather than running
// inline inside the transaction's id window. Every place in the queue the ledger has recorded - a
// queued version's insert, the insert a restore carry's savepoint goes before - moves one along
// with the steps. It refuses what Queue refuses.
func (l *Ledger) QueueFirst(step WriteStep) error {
	if l.tx == nil {
		return errUnjoined
	}
	if l.sealed {
		return ErrQueueSealed
	}
	l.queue = append([]WriteStep{step}, l.queue...)
	for artifactID, pending := range l.latest {
		pending.queued++
		l.latest[artifactID] = pending
	}
	for _, restore := range l.restores {
		restore.from++
	}
	return nil
}

// AfterWrites registers step to run in Commit right after the write phase's batch has run, in the
// same transaction and before it commits, after every step registered before it: a write whose
// statement needs a value only the write phase's results give in Go - a callback's value, an
// appended event's sequence - which it cannot derive in its own SQL as a later statement of the
// batch derives what an earlier one wrote. The step puts its statements on a batch of its own
// (store.RunWrite), built once every callback of the write phase has read its result, so it reads
// what those callbacks captured. Each such step costs the transaction one round trip of its own,
// between its batch and its commit, so a write that can be queued on the batch (Queue) is. A step
// that fails ends the transaction as a failed statement of the write phase does: it rolls back,
// what the ledger holds is dropped (Discard), and Commit returns the failure as the step's batch
// returned it (store.WriteBatch.Send), a callback's error unchanged, so a caller still tells a
// typed refusal apart with errors.Is. It refuses a ledger no caller joined and a sealed one
// (ErrQueueSealed), as Queue does.
func (l *Ledger) AfterWrites(step WriteStep) error {
	if l.tx == nil {
		return errUnjoined
	}
	if l.sealed {
		return ErrQueueSealed
	}
	l.afterWrites = append(l.afterWrites, step)
	return nil
}

// queueFor is the ledger whose write queue a write made in tx joins: ctx's, when it queues tx's
// writes (writer) and its write phase has not begun. Nil means the write runs at once: in a
// transaction no ledger queues (a durable writer's own, a test's), or while the write phase's
// batch is being built (batchFor).
func queueFor(ctx context.Context, tx pgx.Tx) *Ledger {
	ledger := ledgerFrom(ctx)
	if ledger == nil || ledger.writer == nil || ledger.sealed || ledger.writer != tx {
		return nil
	}
	return ledger
}

// batchFor is the write phase's batch of the ledger that queues tx's writes while runQueue builds
// it, nil at any other time.
func batchFor(ctx context.Context, tx pgx.Tx) *store.WriteBatch {
	ledger := ledgerFrom(ctx)
	if ledger == nil || ledger.writer != tx {
		return nil
	}
	return ledger.batch
}

// write runs step in tx: queued on ctx's ledger (queueFor), so its statements join the write
// phase's batch in the order the transaction made its writes - in its place but as a step the
// write phase can drop while a restore carry runs (carryRestored, restoredCarry.step), and as a
// step the write phase drops with the restore carries an approval request's answer read
// (answerOn, answeringStep); on that batch at once while the write phase builds it (batchFor); or
// else at once, as a batch of its own (store.RunWrite), where nothing queues it. Every write a
// document operation makes goes through it.
func write(ctx context.Context, tx pgx.Tx, step WriteStep) error {
	if ledger := queueFor(ctx, tx); ledger != nil {
		ledger.queue = append(ledger.queue, ledger.droppable(step))
		return nil
	}
	if batch := batchFor(ctx, tx); batch != nil {
		return step(batch)
	}
	return store.RunWrite(ctx, tx, step)
}

// writeLast is write for a step that goes at the end of the write phase's batch: after every
// step queued on ctx's ledger, whenever it was queued, and after the version_renders rows the
// batch writes where it writes them (queueStatedRenders), as a step the write phase can drop as
// write's are (runQueue, writeRestored). A carry's review goes there, after its version's
// artifact.version (writeCarriedReview). Where no ledger queues tx's writes it runs as write runs
// it, at once.
func writeLast(ctx context.Context, tx pgx.Tx, step WriteStep) error {
	if ledger := queueFor(ctx, tx); ledger != nil {
		ledger.closing = append(ledger.closing, ledger.droppable(step))
		return nil
	}
	return write(ctx, tx, step)
}

// droppable is step as the ledger queues it now: a step the write phase drops with the restore
// carries the answer running now was decided on (answeringStep), and with the restore carry
// running now (restoredCarry.step).
func (l *Ledger) droppable(step WriteStep) WriteStep {
	step = l.answeringStep(step)
	if l.restoring != nil {
		step = l.restoring.step(step)
	}
	return step
}

// execStep is a write step of one statement whose result nothing reads, its failure wrapped as
// what failed.
func execStep(what, sql string, args ...any) WriteStep {
	return func(b *store.WriteBatch) error {
		b.Queue(what, sql, args...)
		return nil
	}
}

// writeExec writes one statement whose result nothing reads (write, execStep).
func writeExec(ctx context.Context, tx pgx.Tx, what, sql string, args ...any) error {
	return write(ctx, tx, execStep(what, sql, args...))
}

// queueWrites marks ctx so the writes document code makes in tx queue (write) on a ledger of their
// own, for a background pass that decides everything before it writes anything: it does its reads
// and its work under the returned context, then runs the ledger's queue (runQueue) and commits.
// The ledger joins nothing and runs no document operation.
func queueWrites(ctx context.Context, tx pgx.Tx) (context.Context, *Ledger) {
	ledger := &Ledger{writer: tx}
	return withLedger(ctx, ledger), ledger
}

// runQueue runs the write phase. Each queued step puts its statements on one batch, in order, and
// then, when the caller passes it, after runs with a context under which the writes it makes in
// the ledger's transaction join the same batch (write, batchFor): Commit's settlement credit. The
// batch then goes to the server in one round trip (store.WriteBatch.Send), so the statements run
// back to back, and its results are read in order. The first statement or callback that fails -
// or a step that fails before it has queued its statements, once those queued before it have run
// - stops it, and runQueue returns that error as it came, so a caller still tells a typed error -
// a unique violation, ErrDocumentTooLarge, a guarded update's refusal - apart with errors.Is and
// errors.As. It seals the queue first, and then reads which versions a pass of the transaction
// stated render from the tree, and which query's record covers each (readStatedDocuments,
// readCovered). It reads before the first batch is sent, so that read holds no transaction id the
// batch takes.
//
// A transaction that commits through Commit or a settlement's commit keeps main's window - its
// reads, then one batch and its COMMIT (R15) - whatever its passes stated: the batch's last steps
// insert the version_renders row of each version that renders from the tree, with the cut it
// takes in SQL and its record's digest owed where a query's record covers it
// (queueStatedRenders), and the fill of that digest runs once the transaction has committed
// (fillOwedDigests). A ledger whose writes run at once (joinAtOnce) sends those rows as a batch of
// their own once its empty queue has run. A background pass's ledger (queueWrites), whose pass
// states a version in its own transaction, writes each row with its record's digest once the
// batch has run (writeStatedRenders), reading the record from the store as the batch left it.
//
// A transaction that holds a restore carry (carryRestored) splits its batch at the insert of the
// first version such a carry pins: the steps before it go as one batch, the rest behind a
// savepoint, with the version_renders rows, as writeRestored decides them, and after's statements
// then go as a batch of their own. That is the restore's window the owner approved (R14).
//
// The steps queued for the end of the batch (closing, writeLast) go last among the steps, after
// the version_renders rows the batch writes: in a restore's transaction at the end of the part
// behind the savepoint, which runs again with it.
func (l *Ledger) runQueue(ctx context.Context, after func(context.Context) error) error {
	l.sealed = true
	queue, closing := l.queue, l.closing
	l.queue, l.closing = nil, nil
	if err := l.readStatedDocuments(ctx); err != nil {
		return err
	}
	restored := len(l.restores) > 0
	restoredFrom := len(queue)
	for _, restore := range l.restores {
		restoredFrom = min(restoredFrom, restore.from)
	}
	owes := l.owesDigests()
	// The batch is built for ctx, so the write phase is made for the run of the command bar ctx
	// carries (store.WithRun), and the store's writers that queue on it name what it changed.
	batch := store.NewWriteBatch(ctx)
	for _, step := range queue[:restoredFrom] {
		if err := step(&batch); err != nil {
			return batch.SendFailed(ctx, l.writer, err)
		}
	}
	if restored {
		if err := batch.Send(ctx, l.writer); err != nil {
			return err
		}
		if err := l.writeRestored(ctx, slices.Concat(queue[restoredFrom:], closing)); err != nil {
			return err
		}
	} else {
		if owes && l.writer != nil {
			if err := l.queueStatedRenders(&batch); err != nil {
				return batch.SendFailed(ctx, l.writer, err)
			}
		}
		for _, step := range closing {
			if err := step(&batch); err != nil {
				return batch.SendFailed(ctx, l.writer, err)
			}
		}
	}
	if after != nil {
		l.batch = &batch
		err := after(withLedger(ctx, l))
		l.batch = nil
		if err != nil {
			return batch.SendFailed(ctx, l.writer, err)
		}
	}
	if err := batch.Send(ctx, l.writer); err != nil {
		return err
	}
	switch {
	case restored:
		return nil
	case !owes:
		_, err := l.writeStatedRenders(ctx)
		return err
	case l.writer == nil:
		return store.RunWrite(ctx, l.tx, l.queueStatedRenders)
	}
	return nil
}

// owesDigests reports whether the ledger's transaction leaves the record digests of the versions
// its passes stated owed until a fill after its COMMIT (queueStatedRenders, fillOwedDigests): a
// transaction a caller joined (Join, joinAtOnce), which commits through Commit, and a settlement's
// own (ownTx), whose commit runs the fill too. A background pass's ledger (queueWrites) records
// each digest in its own transaction.
func (l *Ledger) owesDigests() bool {
	return l.tx != nil || l.ownTx
}

// restoredCarry is a carry to a version that restores the approved text and takes its
// version_renders row in this transaction (carryRestored): the version, the digest the approval it
// carries pinned, which the version's row holds from before the carry's review is inserted, so the
// review's trigger pins it too, and the place in the queue of the version's insert, before which
// the write phase takes its savepoint (runQueue, writeRestored). dropped says the fill found the
// version's record another digest, so the carry's steps queue nothing from then on (step), nor do
// those of an approval request's answer another carry decided on the carry's review (answerOn);
// undo puts back, newest first, what the carry and such an answer left on the ledger in the read
// phase (noteUndo, drop).
type restoredCarry struct {
	version statedVersion
	pin     string
	from    int
	dropped bool
	undo    []func()
}

// step is step as the carry queues it: it puts its statements on the batch until the carry is
// dropped, and nothing from then on.
func (restore *restoredCarry) step(step WriteStep) WriteStep {
	return func(b *store.WriteBatch) error {
		if restore.dropped {
			return nil
		}
		return step(b)
	}
}

// drop drops the carry: none of its steps queues a statement again, nor does any step of an
// answer decided on its review, and each value the carry and such an answer left on the ledger in
// the read phase - the pending review, the request as an answer left it, the answer's plan and
// parts - stands again as it stood before, newest first.
func (restore *restoredCarry) drop() {
	restore.dropped = true
	for index := len(restore.undo) - 1; index >= 0; index-- {
		restore.undo[index]()
	}
	restore.undo = nil
}

// noteUndo records undo, which puts back a value of the ledger that what runs now is about to
// change, for the drop of the restore carry running now and of each restore carry the answer
// running now was decided on (answerOn); it records nothing while neither runs. Each writer of a
// value a carry leaves on the ledger notes it before it changes it.
func (l *Ledger) noteUndo(undo func()) {
	if l == nil {
		return
	}
	if l.restoring != nil {
		l.restoring.undo = append(l.restoring.undo, undo)
	}
	for _, restore := range l.answering {
		restore.undo = append(restore.undo, undo)
	}
}

// restoresRead is each restore carry of this transaction (carryRestored), none dropped yet, that
// carries a document documents names other than artifactID: the review a carry of artifactID read
// for that document in place of its stored newest one, to decide whether an approval request
// naming documents closes (namedDocumentsApproved), is that carry's, the one review of the
// document this transaction queues (refuseSecondCarry).
func (l *Ledger) restoresRead(documents []model.AskApprovalDocument, artifactID string) []*restoredCarry {
	if l == nil {
		return nil
	}
	var read []*restoredCarry
	for _, restore := range l.restores {
		if restore.dropped || restore.version.artifactID == artifactID {
			continue
		}
		if slices.ContainsFunc(documents, func(document model.AskApprovalDocument) bool {
			return document.ArtifactID == restore.version.artifactID
		}) {
			read = append(read, restore)
		}
	}
	return read
}

// answerOn runs answer, a carry's answer of an approval request that names several documents,
// which closes because each document stands approved at the version the request names: read
// among them are the queued reviews of the restore carries read (restoresRead). Each such review
// approves a version this transaction writes, which no stored review approves, so a request whose
// answer read the review of a carry the write phase drops does not close once that carry is gone.
// The answer's writes are therefore steps the write phase drops with any carry of read
// (answeringStep), and what it leaves on the ledger is undone with that carry (noteUndo): the
// request stays open, moved to the versions this transaction wrote, and the answering carry's own
// review still names it. With read empty, as for every request that names one document, answer
// runs as it is.
func (l *Ledger) answerOn(read []*restoredCarry, answer func() error) error {
	if l == nil || len(read) == 0 {
		return answer()
	}
	previous := l.answering
	l.answering = read
	defer func() { l.answering = previous }()
	return answer()
}

// answeringStep is step as it is queued while an answer runs (answerOn): it puts its statements on
// the batch until a restore carry the answer was decided on is dropped, and nothing from then on.
// It is step itself while no answer runs.
func (l *Ledger) answeringStep(step WriteStep) WriteStep {
	if len(l.answering) == 0 {
		return step
	}
	read := l.answering
	return func(b *store.WriteBatch) error {
		for _, restore := range read {
			if restore.dropped {
				return nil
			}
		}
		return step(b)
	}
}

// writeRestored runs steps, the write phase from the insert of the first version a restore carry
// pins on, behind a savepoint (a nested transaction of the writer's, pgx.Tx.Begin), and then the
// fill (writeStatedRenders), which reads each stated version's record at one cut by its own walk and
// holds each restore carry's pin to its version's digest. Where every pin is its version's digest,
// the fill writes the rows and the savepoint is released: each carry's review and artifact.approved
// stand before its version's artifact.version, as the queue made them. Where a pin is not, the
// version's record is not the one the person approved, whatever its text: the transaction rolls
// back to the savepoint, drops that carry (restoredCarry.drop) and runs steps and the fill again
// behind a new savepoint, so the version, what it states and its row are written with no carry -
// no review, no artifact.approved. Each round drops one carry at least, so it ends.
//
// What the rolled-back statements left in memory goes with them. The events their callbacks handed
// the ledger (eventSink) are cut off where the savepoint was taken, so none of them is published;
// the steps that run again append theirs again, with the ids and sequences the statements run again
// took, and every other callback of a step that runs again writes again what it read. A dropped
// carry's own read-phase values are undone by its drop. The stated versions stay recorded, so the
// fill of the next round states the same rows. The ids the rolled-back inserts drew from their
// sequences are not given back, which every reader of the event log already allows (the horizon's
// gaps); an event's seq is taken from its owner row's last_seq, which the rollback restores, so the
// seqs stay without gaps. A lock a rolled-back statement took first is let go and taken again by
// the statement that runs again, in the same order.
func (l *Ledger) writeRestored(ctx context.Context, steps []WriteStep) error {
	appended := len(l.events)
	for {
		savepoint, err := l.writer.Begin(ctx)
		if err != nil {
			return fmt.Errorf("take the savepoint of the restored versions' carries: %w", err)
		}
		batch := store.NewWriteBatch(ctx)
		for _, step := range steps {
			if err := step(&batch); err != nil {
				return batch.SendFailed(ctx, savepoint, err)
			}
		}
		if err := batch.Send(ctx, savepoint); err != nil {
			return err
		}
		missed, err := l.writeStatedRenders(ctx)
		if err != nil {
			return err
		}
		if len(missed) == 0 {
			if err := savepoint.Commit(ctx); err != nil {
				return fmt.Errorf("release the savepoint of the restored versions' carries: %w", err)
			}
			return nil
		}
		if err := savepoint.Rollback(ctx); err != nil {
			return fmt.Errorf("roll back to the savepoint of the restored versions' carries: %w", err)
		}
		l.events = l.events[:appended]
		for _, restore := range missed {
			restore.drop()
		}
	}
}

// eventSink is where an event a document operation appends goes once appended: ctx's ledger, which
// publishes it after its commit (Ledger.Commit) and whose settlement reads it back, in the order
// the appends ran. It is taken when the operation runs, so a step appending later reaches the same
// ledger whatever context the write phase runs under.
func eventSink(ctx context.Context) func(model.Event) {
	ledger := ledgerFrom(ctx)
	return func(event model.Event) {
		if ledger != nil {
			ledger.events = append(ledger.events, event)
		}
	}
}

// completeEvent gives the event the ledger holds under event's id what event now carries: a
// carried review's artifact.approved, which the ledger took in its place when it was appended
// (eventSink) and whose payload the end of the batch completed (writeCarriedReview), so the
// events stay in the order their appends ran. A ledger that holds no such event changes nothing.
func (l *Ledger) completeEvent(event model.Event) {
	if l == nil {
		return
	}
	for index := len(l.events) - 1; index >= 0; index-- {
		if l.events[index].ID == event.ID {
			l.events[index] = event
			return
		}
	}
}

// Now is the transaction's now(): the time it started, which Postgres holds fixed for the whole
// transaction, read once. A row whose column defaults to now() carries that same value whether a
// queued insert writes it explicitly or leaves it to the column's default, as the carried
// approval's review row does (writeCarriedReview, approval_carry.go), so a reader in the read
// phase knows the value the row will carry either way.
func (l *Ledger) Now(ctx context.Context) (time.Time, error) {
	if l.writer == nil {
		return time.Time{}, errUnjoined
	}
	if l.now == nil {
		now, err := readNow(ctx, l.writer)
		if err != nil {
			return time.Time{}, err
		}
		l.now = &now
	}
	return *l.now, nil
}

// transactionNow is tx's now(): through the ledger that queues tx's writes (Ledger.Now), which
// reads it once, or read at once.
func transactionNow(ctx context.Context, tx pgx.Tx) (time.Time, error) {
	if ledger := queueFor(ctx, tx); ledger != nil {
		return ledger.Now(ctx)
	}
	return readNow(ctx, tx)
}

// readNow reads the transaction's now() through q.
func readNow(ctx context.Context, q Queryer) (time.Time, error) {
	var now time.Time
	if err := q.QueryRow(ctx, `select now()`).Scan(&now); err != nil {
		return time.Time{}, fmt.Errorf("read the transaction's time: %w", err)
	}
	return now, nil
}

// NextVersion numbers the next version of artifactID this transaction writes and dates it: one
// past the document's latest version, counting the versions this transaction has numbered and not
// yet written, at the transaction's now() (Now). The caller holds the document owner's lock,
// which every writer of a version takes first, so no other transaction numbers one meanwhile. A
// route that queues a version insert of its own numbers it here, so the document service's
// versions in the same transaction follow it.
func (l *Ledger) NextVersion(ctx context.Context, artifactID string) (int, time.Time, error) {
	if l.writer == nil {
		return 0, time.Time{}, errUnjoined
	}
	number, numbered := l.numbers[artifactID]
	if !numbered {
		latest, now, err := latestStoredVersion(ctx, l.writer, artifactID)
		if err != nil {
			return 0, time.Time{}, err
		}
		number = latest
		if l.now == nil {
			l.now = &now
		}
	}
	number++
	if l.numbers == nil {
		l.numbers = make(map[string]int)
	}
	l.numbers[artifactID] = number
	return number, *l.now, nil
}

// pendingVersion is a version this transaction has queued and not yet written, as latestVersion
// reads a stored one, with queued, the place in the write queue of its insert, before which the
// write phase takes the savepoint of a restore carry to it (carryRestored).
type pendingVersion struct {
	version          model.Version
	markdown         string
	docUpdateVersion int64
	queued           int
}

// PendingVersionMarkdown is the markdown of version number of artifactID when it is the version
// this transaction last numbered and queued (writeVersionTx) and has not written yet: a route that
// reads the version it just made - the diff a named version's event carries - takes it from here,
// since the table holds the row only once Commit has run the queue. False when this transaction
// queued no such version.
func (l *Ledger) PendingVersionMarkdown(artifactID string, number int) (string, bool) {
	if l.sealed {
		return "", false
	}
	pending, queued := l.latest[artifactID]
	if !queued || pending.version.Number != number {
		return "", false
	}
	return pending.markdown, true
}

// PendingVersionNumber is the number of the version of artifactID this transaction last numbered
// and queued and has not written yet: the document's newest, since the table holds the row only
// once Commit has run the queue, so a reader before Commit that answers for the document as this
// write leaves it - the standing review a question's advice names - takes it from here. The write
// phase writes every version it queues, whatever carry it drops, so the number stands. False when
// this transaction queued no version of the document, and once its write phase has begun, when
// the table holds it.
func (l *Ledger) PendingVersionNumber(artifactID string) (int, bool) {
	if l == nil || l.sealed {
		return 0, false
	}
	pending, queued := l.latest[artifactID]
	return pending.version.Number, queued
}

// queuedVersion records pending as the latest version of artifactID, which this transaction has
// queued and not yet written (latestVersion, PendingVersionMarkdown, PendingVersionNumber).
func (l *Ledger) queuedVersion(artifactID string, pending pendingVersion) {
	if l.latest == nil {
		l.latest = make(map[string]pendingVersion)
	}
	l.latest[artifactID] = pending
}

// queuedAuthors records authors as pending authors of artifactID this transaction has queued the
// write of, each under its own document where it names one (pendingAuthorsIn), as the row is
// written: until its write phase has run, doc_pending_authors holds no row of theirs, and the
// versions this transaction writes read them from here instead (transactionPendingAuthors,
// transactionRoomPendingAuthors). Its one caller is queuePendingAuthors, which queues that write,
// so every queue of it is here. A queued delete of them (deleteCapturedPendingAuthors,
// deleteRoomPendingAuthors) is queued after the insert, so it takes the rows back out in the write
// phase; nothing of this transaction reads R between the two. Each author merges every row queued
// of them (mergeAuthor), as readPendingAuthors merges the rows the write phase leaves and the
// upsert merges a row written again: Proven only when every queued row is.
func (l *Ledger) queuedAuthors(artifactID string, authors []pendingAuthor) {
	if l.owedAuthors == nil {
		l.owedAuthors = make(map[string]map[string]model.Actor, 1)
	}
	for _, author := range authors {
		document := artifactID
		if author.document != "" {
			document = author.document
		}
		owed := l.owedAuthors[document]
		if owed == nil {
			owed = make(map[string]model.Actor, len(authors))
			l.owedAuthors[document] = owed
		}
		mergeAuthor(owed, author.actor)
	}
}

// queuedAskAuthors records authors, by block id, as durable ask block authors of artifactID this
// transaction has queued the write of: until its write phase has run, doc_ask_block_authors
// holds no row of theirs, and the reads this transaction makes of them take them from here as
// well as from the table (transactionRecordedAskAuthors). Its one caller is queueAskAuthors,
// which queues that write, so every queue of it is here. The insert keeps the author a row
// already recorded has (askAuthorsStatement), so a row the table holds wins over one recorded
// here, and so does the first entry recorded for a block. A settlement's queued delete of them
// (deleteSettledAskAuthors) is queued after the insert, so it takes the rows back out in the
// write phase; nothing of this transaction reads them between the two.
func (l *Ledger) queuedAskAuthors(artifactID string, authors map[string]model.Actor) {
	if l.askAuthors == nil {
		l.askAuthors = make(map[string]map[string]model.Actor, 1)
	}
	queued := l.askAuthors[artifactID]
	if queued == nil {
		queued = make(map[string]model.Actor, len(authors))
		l.askAuthors[artifactID] = queued
	}
	for block, actor := range authors {
		if _, recorded := queued[block]; !recorded {
			queued[block] = actor
		}
	}
}

// QueueVersion queues the insert of a document version the caller writes itself, outside the
// document service - an upload's, which lists its uploader alone, and a new issue's first version
// of its seeded spec - and returns the row as the insert will leave it. It is named when it has a
// summary. It is the version write every other version goes through (queueVersionInsert), run on
// this ledger whatever context the caller holds: the route that queues a version of its own passes
// the request's, not the one Join returned. It refuses a ledger no caller joined, and a sealed one
// (ErrQueueSealed) as Queue does: its insert would run after the statements it was meant to
// precede, or in no transaction at all.
//
// The row records what its markdown was rendered from (RenderStampOf), the tree this
// transaction's own write of the document left where that tree is what markdown came from, so a
// text read of a document still standing there serves the stored markdown rather than rendering
// the document again.
func (l *Ledger) QueueVersion(ctx context.Context, artifactID, markdown string, authors []model.Actor, summary *string) (model.Version, error) {
	if l.writer == nil {
		return model.Version{}, errUnjoined
	}
	if l.sealed {
		return model.Version{}, ErrQueueSealed
	}
	return queueVersionInsert(withLedger(ctx, l), l.writer, artifactID, markdown, authors, summary != nil, summary, nil, l.RenderStampOf(artifactID, markdown))
}

// pendingRoom is a document's stored history (doc_updates) as the writes this transaction has
// queued to it leave it. A transaction that queues a write to it holds its document lock from
// before it read the history to the end, so nothing else changes it meanwhile, and these are
// exact: head is the version the history stands at; recovered, the ceiling of an interrupted prune
// whose recovery the transaction queued, above which the table's rows are deleted first; rebuilt
// says a queued rebuild deleted every row the table holds; appended counts the rows queued since,
// appends and a rebuild's seed, and changed says one of them changed the document's content.
type pendingRoom struct {
	head      int64
	recovered *int64
	rebuilt   bool
	appended  int64
	changed   bool
}

// pendingRoomOf is room's pendingRoom in the ledger ctx queues tx's writes on, nil when there is
// no such ledger or it has queued no write to room's history.
func pendingRoomOf(ctx context.Context, tx pgx.Tx, room string) *pendingRoom {
	if ledger := queueFor(ctx, tx); ledger != nil {
		return ledger.rooms[room]
	}
	return nil
}

// PendingAnchor is an anchored ask or comment row the transaction writes in its queue - a row it
// creates, or one whose anchor or open state it changes - as the row will stand once written. The
// table holds none of that until Commit, so the transaction's anchor refreshes, which read the
// open anchors of a document (openAnchoredMarks), take it from here instead
// (RegisterPendingAnchor).
type PendingAnchor struct {
	// ID is the row's id, and Kind the mark it anchors: MarkAsk for an ask, MarkComment or
	// MarkSuggestion for a comment.
	ID   string
	Kind MarkKind
	// Anchor is the row's anchor; its ArtifactID names the document it anchors.
	Anchor model.Anchor
	// Open says whether a refresh refreshes the row: an open ask, an unresolved comment.
	Open bool
	// Event prepares the row's anchor-refresh event (ask.anchor_refreshed,
	// comment.anchor_refreshed). A refresh that moves the row's anchor calls it in the read phase,
	// in tx, to read what the event needs, and calls the builder it returns while the write phase
	// builds its batch, with the anchor refreshed to. That is before any queued statement has run,
	// so the builder uses only what the read phase read and does only the light work of a step.
	// Nil reads the row from the table in the read phase, as the refresh reads a stored row, which
	// only a row the table already holds can be.
	Event func(ctx context.Context, tx pgx.Tx) (func(refreshed model.Anchor) (model.Event, error), error)
}

// RegisterPendingAnchor records row, an anchored ask or comment row the caller has queued a write
// of, so every anchor refresh this transaction runs on row.Anchor.ArtifactID reads it as it will
// stand: a new open row is refreshed after the stored ones, a stored row the transaction changes is
// read as changed, and one it closes (Open false) is no longer refreshed. A later registration of
// the same row replaces the earlier one in its place.
func (l *Ledger) RegisterPendingAnchor(row PendingAnchor) {
	artifactID := row.Anchor.ArtifactID
	if l.anchors == nil {
		l.anchors = make(map[string][]*PendingAnchor)
	}
	for index, registered := range l.anchors[artifactID] {
		if registered.ID == row.ID {
			l.anchors[artifactID][index] = &row
			return
		}
	}
	l.anchors[artifactID] = append(l.anchors[artifactID], &row)
}

// pendingAnchorsOf is the anchored rows registered on ctx's ledger for artifactID
// (RegisterPendingAnchor), none once its write phase has begun, when the table holds them.
func pendingAnchorsOf(ctx context.Context, artifactID string) []*PendingAnchor {
	ledger := ledgerFrom(ctx)
	if ledger == nil || ledger.sealed {
		return nil
	}
	return ledger.anchors[artifactID]
}

// RegisterPendingArtifact records address as the address of artifactID, an artifacts row this
// transaction creates and has not written yet: its insert is queued with the transaction's other
// writes, or runs later in its write phase, while the document's own work names the artifact's id
// from the start, so a read of the document's address here takes it from the ledger rather than
// from the row (addressOfDocument). The caller records the address the queued insert writes
// (refs.DocumentAddress), and query, the query of the node it writes of the document where that
// node is a query node (graphstore.QueryNode), "" where it is none: a query makes each version of
// the document a query version (ReadVersionOwner), whose record that query's is (readCovered).
func (l *Ledger) RegisterPendingArtifact(artifactID string, address refs.DocumentAddress, query string) {
	if l.artifacts == nil {
		l.artifacts = make(map[string]refs.DocumentAddress)
	}
	l.artifacts[artifactID] = address
	if query != "" {
		if l.queries == nil {
			l.queries = make(map[string]string, 1)
		}
		l.queries[artifactID] = query
	}
}

// RegisterPendingIssue records key as an issues row this transaction creates and has not written
// yet: its insert is queued with the transaction's other writes, while the text the same
// transaction indexes - the spec it seeds for that issue - can name the issue being created, and
// no read of this transaction answers for the row (PendingIssue).
func (l *Ledger) RegisterPendingIssue(key string) {
	if l.issues == nil {
		l.issues = make(map[string]struct{}, 1)
	}
	l.issues[key] = struct{}{}
}

// The ledger is the refs.Pending of the transaction whose writes it queues: every context
// carrying it carries that too (withLedger), and the reads of a write run under the context
// that write queues on (ReadContext), so a reference read made anywhere in the transaction
// answers for the rows it has queued as it does for the stored ones. Each answer below is the
// overlay the write that queued the row recorded it in, so a row is registered once and read
// here.

// PendingIssue reports whether this transaction writes the issues row of key
// (RegisterPendingIssue).
func (l *Ledger) PendingIssue(key string) bool {
	if l == nil {
		return false
	}
	_, pending := l.issues[key]
	return pending
}

// PendingArtifact is the uuid and the dispatch:// address of the artifacts row of reference key
// refKey this transaction writes (RegisterPendingArtifact, refs.DocumentAddress.Ref).
func (l *Ledger) PendingArtifact(refKey string) (string, string, bool) {
	if l == nil {
		return "", "", false
	}
	for id, address := range l.artifacts {
		if address.RefKey == refKey {
			return id, address.Ref, true
		}
	}
	return "", "", false
}

// PendingDocument is the address registered for the artifacts row of uuid id this transaction
// writes (RegisterPendingArtifact).
func (l *Ledger) PendingDocument(id string) (refs.DocumentAddress, bool) {
	if l == nil {
		return refs.DocumentAddress{}, false
	}
	address, pending := l.artifacts[id]
	return address, pending
}

// pendingQuery is the query of the query node this transaction writes of the artifacts row of
// uuid id it writes (RegisterPendingArtifact), "" where the node it writes is no query node.
func (l *Ledger) pendingQuery(id string) string {
	if l == nil {
		return ""
	}
	return l.queries[id]
}

// PendingRoom is the issue room the document of ref_key refKey is edited in, where this
// transaction creates the document (RegisterPendingArtifact) and joins it to the room
// (RegisterPendingRoom): an issue born converted's spec, whose tree the transaction creates too,
// and an upload's Verification document or plan, which joins the stored tree of a converted issue.
func (l *Ledger) PendingRoom(refKey string) (string, bool) {
	if l == nil {
		return "", false
	}
	for id, address := range l.artifacts {
		if address.RefKey == refKey {
			return l.roomOf(id)
		}
	}
	return "", false
}

// PendingMarkdown is the markdown of the version of artifactID this transaction has queued and
// not written yet (queuedVersion), the latest version of that document as the transaction
// leaves it, which latestVersion answers with too.
func (l *Ledger) PendingMarkdown(artifactID string) (string, bool) {
	if l == nil || l.sealed {
		return "", false
	}
	pending, queued := l.latest[artifactID]
	return pending.markdown, queued
}

// PendingAuthors is the authors of version number of artifactID where that is the version this
// transaction has queued and not written yet (queuedVersion), as its row will store them.
func (l *Ledger) PendingAuthors(artifactID string, number int) ([]model.Actor, bool) {
	if l == nil || l.sealed {
		return nil, false
	}
	pending, queued := l.latest[artifactID]
	if !queued || pending.version.Number != number {
		return nil, false
	}
	return pending.version.Authors, true
}

// PendingBlockAsks is every question block's ask of artifactID this transaction has queued a
// write of, as the row will stand (recordAskBlock, which loadAskBlocks reads too), in the shape
// its readers read them by (refs.PendingBlockAsk).
func (l *Ledger) PendingBlockAsks(artifactID string) []refs.PendingBlockAsk {
	if l == nil {
		return nil
	}
	asks := l.askBlocks[artifactID]
	if len(asks) == 0 {
		return nil
	}
	pending := make([]refs.PendingBlockAsk, 0, len(asks))
	for blockID, ask := range asks {
		pending = append(pending, pendingBlockAsk(blockID, ask))
	}
	return pending
}

// PendingAsk is the ask of id among the same entries, which a reference to one of this
// transaction's new question blocks names and no read of the asks table answers for
// (refs.resolveAskTargets). Settlement opens the asks of one document's new blocks, so the
// scan is over that document's few entries.
func (l *Ledger) PendingAsk(id string) (refs.PendingBlockAsk, bool) {
	if l == nil {
		return refs.PendingBlockAsk{}, false
	}
	for _, asks := range l.askBlocks {
		for blockID, ask := range asks {
			if ask.ID == id {
				return pendingBlockAsk(blockID, ask), true
			}
		}
	}
	return refs.PendingBlockAsk{}, false
}

// pendingBlockAsk is one askBlocks entry in the shape its readers read it by: the owner columns
// the row carries, and the block it is the ask of.
func pendingBlockAsk(blockID string, ask model.Ask) refs.PendingBlockAsk {
	return refs.PendingBlockAsk{BlockID: blockID, ID: ask.ID, IssueKey: ask.IssueKey, ArtifactID: ask.ArtifactID}
}

// QueueAskParts queues the index of an ask's parts for a route that writes the ask itself,
// outside the document service: it plans the parts the ask holds at `at`, against what this
// transaction's own queued writes of the same ask leave (planAskPartsCounted), queues the
// statements that write them, and returns the targets they moved merged into beside, what the
// same write moved elsewhere, so the event the route appends names them all. The route queues the
// ask's own row write first: every part table references the ask's row, so the index's statements
// have to follow that write in the batch. The plan and the reads it makes run on this ledger
// whatever context the caller holds (ReadContext), as QueueVersion's write does, and it refuses
// a ledger no caller joined and a sealed one (ErrQueueSealed).
//
// document is the one the parts' `#id`s resolve against for an ask whose insert the route has
// queued, which no read of that row can answer yet (refs.PendingAskDocument); nil for an ask
// already stored, whose row the reader reads for itself (refs.ScopeOf). It is read only if a
// part holds a `#` candidate, and a route that reconciles the ask's question too hands the same
// one to both readers, so the two of them read it once.
func (l *Ledger) QueueAskParts(
	ctx context.Context,
	ask model.Ask,
	serverURL string,
	document *refs.PendingDocument,
	at time.Time,
	beside model.ReferenceChanges,
) (model.ReferenceChanges, error) {
	if l.writer == nil {
		return model.ReferenceChanges{}, errUnjoined
	}
	if l.sealed {
		return model.ReferenceChanges{}, ErrQueueSealed
	}
	// Every read below runs under this ledger's context, so a `#id` naming a question block
	// this transaction has only queued resolves to the ask it will name (refs.Pending); the
	// route's own context carries no ledger where the route passed the request's, and a caller
	// that derived its own from this ledger already hands one that does (ReadContext).
	ctx = l.ReadContext(ctx)
	// The parts are the only text of the ask this call holds, so the reader is its own: a route
	// that reconciles the question too builds one and reconciles through it
	// (refs.NewReaderInScope).
	reader := refs.NewReaderInScope(ctx, l.writer, ask.Author, "ask", ask.ID, serverURL, document)
	changes, parts, err := planAskPartsCounted(ctx, l.writer, reader, ask, at, beside)
	if err != nil {
		return model.ReferenceChanges{}, err
	}
	if err := l.Queue(parts); err != nil {
		return model.ReferenceChanges{}, err
	}
	return changes, nil
}

// pendingArtifact is the address registered for artifactID on ctx's ledger
// (RegisterPendingArtifact), false when none is and the artifacts row is the only place to read
// it.
func pendingArtifact(ctx context.Context, artifactID string) (refs.DocumentAddress, bool) {
	return ledgerFrom(ctx).PendingDocument(artifactID)
}

// recordPendingApproval records ask as artifactID's approval request as this transaction's queued
// writes leave it, on the ledger ctx queues tx's writes on: the open row a version write's move
// leaves (MoveApprovalAsk), or the answered row the same write's carry leaves
// (answerApprovalAsk). The caller has queued the write that leaves it so, and a read of the
// request before the write phase takes it from here (openApprovalAsk). The row is recorded under
// every document the request names, beside artifactID, since it is one row whichever of them a
// later read finds it by: the reopen's follow moves and carries each document of the issue in one
// transaction (FollowClosedIssueVersions), so the move of a request's second document goes on from
// the row its first document's move or carry left. A restore carry's drop puts back the rows its
// answer replaced (noteEntryUndo).
func recordPendingApproval(ctx context.Context, tx pgx.Tx, artifactID string, ask *model.Ask) {
	ledger := queueFor(ctx, tx)
	if ledger == nil {
		return
	}
	if ledger.approvals == nil {
		ledger.approvals = make(map[string]*model.Ask)
	}
	noteEntryUndo(ledger, &ledger.approvals, artifactID)
	ledger.approvals[artifactID] = ask
	if ask.Approval != nil {
		for _, document := range ask.Approval.Documents {
			noteEntryUndo(ledger, &ledger.approvals, document.ArtifactID)
			ledger.approvals[document.ArtifactID] = ask
		}
	}
}

// noteEntryUndo notes, for the drop of the restore carries noteUndo notes for, that
// (*values)[key] stands again as it stands now: the same value, or no entry at all.
func noteEntryUndo[K comparable, V any](l *Ledger, values *map[K]V, key K) {
	if l == nil || l.restoring == nil && len(l.answering) == 0 {
		return
	}
	previous, had := (*values)[key]
	l.noteUndo(func() {
		if had {
			(*values)[key] = previous
		} else {
			delete(*values, key)
		}
	})
}

// recordPendingReview records review as the newest review of artifactID on the ledger ctx queues
// tx's writes on, dated with the transaction's now() (Ledger.Now), the time its row will carry:
// writeCarriedReview, its one caller, has queued the insert that writes it (QueueReview), and a
// read of artifact_reviews before the write phase still answers the review before this one. What
// a later carry makes of the entry's presence is on the field (reviews).
func recordPendingReview(ctx context.Context, tx pgx.Tx, artifactID string, review model.ArtifactReview) error {
	ledger := queueFor(ctx, tx)
	if ledger == nil {
		return nil
	}
	createdAt, err := ledger.Now(ctx)
	if err != nil {
		return err
	}
	review.CreatedAt = createdAt
	if ledger.reviews == nil {
		ledger.reviews = make(map[string]model.ArtifactReview)
	}
	noteEntryUndo(ledger, &ledger.reviews, artifactID)
	ledger.reviews[artifactID] = review
	return nil
}

// PlanSettle plans change on the ask askID for this transaction's write phase, as the one writer
// of an ask's answer state plans it (asks.PlanSettle): from the last change of the same ask this
// transaction has planned, where it has planned one (asks.PlanSettleAfter), and from the stored row
// and node otherwise; and records the plan as the ask's latest, so a change after it is planned
// from it in turn. The caller queues the plan where it makes the write (Queue, write,
// asks.Settled.Queue). Every settle of a transaction that queues its writes is planned here - the
// reopen restores an ask and then its follow answers it, a settlement retracts an ask whose block
// then returns - so no change of an ask is planned from a row an earlier queued change has already
// moved. It refuses a ledger no caller joined and a sealed one (ErrQueueSealed).
func (l *Ledger) PlanSettle(ctx context.Context, askID string, change asks.Change) (asks.Settled, error) {
	if l.writer == nil {
		return asks.Settled{}, errUnjoined
	}
	if l.sealed {
		return asks.Settled{}, ErrQueueSealed
	}
	var settled asks.Settled
	var err error
	if earlier, planned := l.settledAsks[askID]; planned {
		settled, err = asks.PlanSettleAfter(earlier, change)
	} else {
		settled, err = asks.PlanSettle(l.ReadContext(ctx), l.writer, askID, change)
	}
	if err != nil {
		return asks.Settled{}, err
	}
	if l.settledAsks == nil {
		l.settledAsks = make(map[string]asks.Settled, 1)
	}
	noteEntryUndo(l, &l.settledAsks, askID)
	l.settledAsks[askID] = settled
	return settled, nil
}

// planSettle plans change on the ask askID in tx: through the ledger that queues tx's writes
// (Ledger.PlanSettle), which plans it after a change of the same ask that ledger already holds,
// and else from the stored row and node, for a write that runs at once (asks.PlanSettle).
func planSettle(ctx context.Context, tx pgx.Tx, askID string, change asks.Change) (asks.Settled, error) {
	if ledger := queueFor(ctx, tx); ledger != nil {
		return ledger.PlanSettle(ctx, askID, change)
	}
	return asks.PlanSettle(ctx, tx, askID, change)
}

// PendingReview is the review of artifactID this transaction has queued and not yet written
// (recordPendingReview): a review it queued is newer than every stored one, so a read of the
// document's newest review before Commit takes it from here. Its readers are the carry's own
// (refuseSecondCarry, namedDocumentsApproved), which the write phase drops with the carry; a
// route's report of the review a write leaves takes the review the write phase kept
// (WrittenReview). False when this transaction queued none, and once its write phase has begun,
// when the table holds it.
func (l *Ledger) PendingReview(artifactID string) (model.ArtifactReview, bool) {
	if l == nil || l.sealed {
		return model.ArtifactReview{}, false
	}
	review, queued := l.reviews[artifactID]
	return review, queued
}

// WrittenReview is the review of artifactID this transaction wrote, once its write phase has run
// whole: the review its carry queued (recordPendingReview), unless the write phase dropped that
// carry because its version's record was not the approved one (writeRestored), whose drop took the
// review off the ledger again (restoredCarry.drop). It runs no statement. A route's report of the
// review its write leaves reads the stored newest review in its read phase, where the table holds
// none of the transaction's writes, and takes this one in its place once Commit has returned. False
// before the write phase has run whole, and where the transaction wrote no review of the document.
func (l *Ledger) WrittenReview(artifactID string) (model.ArtifactReview, bool) {
	if l == nil || !l.written {
		return model.ArtifactReview{}, false
	}
	review, wrote := l.reviews[artifactID]
	return review, wrote
}

// WrittenApproval is artifactID's approval request as this transaction wrote it, once its write
// phase has run whole: the row its version's move wrote (MoveApprovalAsk), and the answer a carry
// queued (answerApprovalAsk), unless the write phase dropped that carry, or a restore carry whose
// review the answer was decided on (answerOn), whose drop put back the row the move left
// (restoredCarry.drop). It runs no statement. A route's report of the request its write moved reads
// the stored open request in its read phase, where the table holds none of the transaction's
// writes, and takes this one in its place once Commit has returned. False before the write phase
// has run whole, and where the transaction wrote no request naming the document.
func (l *Ledger) WrittenApproval(artifactID string) (model.Ask, bool) {
	if l == nil || !l.written {
		return model.Ask{}, false
	}
	ask := l.approvals[artifactID]
	if ask == nil {
		return model.Ask{}, false
	}
	return *ask, true
}

// ledgerAnnouncement is one document a transaction appended to and the last version it appended.
type ledgerAnnouncement struct {
	artifactID string
	head       persistence.Version
}

// deferAnnouncement records that the transaction appended version to artifactID, for its commit
// to announce (announceAppends). A later append of the same document replaces the version.
func (l *Ledger) deferAnnouncement(artifactID string, version persistence.Version) {
	for index := range l.announced {
		if l.announced[index].artifactID == artifactID {
			l.announced[index].head = version
			return
		}
	}
	l.announced = append(l.announced, ledgerAnnouncement{artifactID: artifactID, head: version})
}

// announceAppends announces, in the transaction about to commit, each document it appended to
// with the last version it appended (announceDocumentHead): one statement for all of them, under
// the advisory locks the appends took and the transaction still holds, so a listener that hears
// it reads a head at or past that version, and hears nothing of a transaction that rolls back.
// It is queued on the write phase's batch (commitWrites), the last statement after every append
// it announces, so the transaction spends no round trip of its own on it; a transaction whose
// ledger queues nothing (joinAtOnce) runs it at once, after those appends.
func (l *Ledger) announceAppends(ctx context.Context) error {
	if len(l.announced) == 0 {
		return nil
	}
	rooms := make([]string, 0, len(l.announced))
	heads := make([]int64, 0, len(l.announced))
	for _, announced := range l.announced {
		rooms = append(rooms, announced.artifactID)
		heads = append(heads, int64(announced.head))
	}
	l.announced = nil
	return writeExec(ctx, l.tx, "announce the documents the transaction appended to", `
		select `+announceDocumentHead("announced.room", "announced.head", "$3::text")+`
		from unnest($1::text[], $2::bigint[]) with ordinality as announced(room, head, position)
		order by announced.position
	`, rooms, heads, l.service.processID)
}

type ledgerVersion struct {
	artifactID string
	version    model.Version
	// capture is whom the version listed (authorCapture): the pending authors its commit deletes
	// and the in-flight credits it marks consumed.
	capture authorCapture
}

// ledgerVersionText is the markdown one version write stored, and the document it wrote it to,
// which a failed store is logged under.
type ledgerVersionText struct {
	artifactID string
	markdown   string
}

// ledgerSeed is a document SeedText wrote in this transaction: the actor who wrote its first
// text, the room it wrote it into - the document itself, or the issue room whose fragment of it
// the seed wrote - and the tree it wrote with that tree's rendering and its blocks' byte ranges.
// Credit registers the actor against every ask block the tree introduced (registerAskAuthors), in
// the room, once the transaction has committed, and the seed's version cuts its mentions' excerpts
// from the rendering.
type ledgerSeed struct {
	actor    model.Actor
	room     string
	rendered renderedTree
}

type ledgerContextKey struct{}

// errUnjoined refuses a transactional operation called outside Join: its writes would reach no
// ledger, so their authors would never be credited or released.
var errUnjoined = errors.New("docs: the operation needs a transaction joined with Service.Join")

// Join joins the document operations run with the returned context to tx, recording what they
// produce in the returned ledger and queueing their writes for its Commit.
func (s *Service) Join(ctx context.Context, tx pgx.Tx) (context.Context, *Ledger) {
	ledger := &Ledger{service: s, tx: tx, writer: tx}
	return withLedger(ctx, ledger), ledger
}

// joinAtOnce joins the document operations run with the returned context to tx, as Join does,
// and queues none of their writes: the ledger knows tx, so its operations run as inside a
// transaction (inTransaction) and Commit commits it, and it sets no writer, so each write runs at
// once, in the order the operations make it (write, store.RunWrite), and Commit's settlement
// credit runs at once too. It is for the decisions step's restore (DecisionStep.restoreIssue),
// whose transaction is one issue's restore and no more: a background pass under the step's own
// advisory lock (takeRestoreLock) whose reads must see its own writes - an ask row it moved, a
// version it wrote before the event that names it, a node it stated - and which no request waits
// on, as the block-id backfill is (backfillBlockIDs). Every other caller joins with Join.
//
// kept for one release: decisions-step
func (s *Service) joinAtOnce(ctx context.Context, tx pgx.Tx) (context.Context, *Ledger) {
	ledger := &Ledger{service: s, tx: tx}
	return withLedger(ctx, ledger), ledger
}

// inTransaction reports whether the operations recording into l run inside an open database
// transaction that this ledger knows about: one a caller joined, or a writer's own, a
// settlement's or the block-id backfill's. Such an operation fails on a failed room rather than
// waiting for its recovery (awaitRoomRecovery). An operation that runs inside a transaction
// without joining it is invisible here and still waits, which is the hang this rule exists to
// stop, so a handler joins (Docs.Join) and passes the context Join returned.
func (l *Ledger) inTransaction() bool {
	return l != nil && (l.tx != nil || l.ownTx)
}

// withLedger marks ctx with the ledger the writes made under it queue on, and with the rows that
// ledger has queued (refs.Pending), so every reference read made in the transaction resolves a
// row it creates as it will stand once the write phase has run.
//
// The two marks are one mark, and that is the contract: the context a write queues on
// (write, queueFor) is the context the reads of that write run under, so a read sees exactly
// the rows its own transaction has queued. Document code is held to it by construction - a
// write it makes under a context carrying no ledger does not queue at all - and the callers
// that hand a ledger over as an argument instead, this package's Ledger methods and api's
// write-queue helpers, are held to it by taking their reading context from that ledger
// (ReadContext) rather than from whatever the caller passed.
func withLedger(ctx context.Context, ledger *Ledger) context.Context {
	return refs.WithPending(context.WithValue(ctx, ledgerContextKey{}, ledger), ledger)
}

// ReadContext is ctx with this ledger on it: the context the read phase of a write that queues
// on this ledger reads under, whatever context its caller holds. A route passes the request's
// context to the helpers that index what it writes, not the one Join returned, so each of them
// derives its own from the ledger it was handed and no reference read of the transaction can
// miss the rows that transaction has queued (refs.Pending).
//
// A context already carrying this ledger - the one Join returned, or one a caller above derived
// from the same ledger - is returned as it is, so the context a helper hands on to another is
// wrapped once however many of them derive their own (api.queueNewAskReferences, QueueAskParts).
func (l *Ledger) ReadContext(ctx context.Context) context.Context {
	if ledgerFrom(ctx) == l {
		return ctx
	}
	return withLedger(ctx, l)
}

func ledgerFrom(ctx context.Context) *Ledger {
	ledger, _ := ctx.Value(ledgerContextKey{}).(*Ledger)
	return ledger
}

func txFromContext(ctx context.Context) (pgx.Tx, bool) {
	ledger := ledgerFrom(ctx)
	if ledger == nil || ledger.tx == nil {
		return nil, false
	}
	return ledger.tx, true
}

// collectVersionText records the markdown a version write of this transaction stored, whose
// display tree the commit stores once that version is durable (storeVersionTrees). It is
// collected here, where the version is written, rather than at the routes that write one: the
// markdown is the service's - a route hands it a tree and is given a version number back - and
// four routes, a settlement and the block-id backfill all write versions through writeVersionTx.
func collectVersionText(ctx context.Context, artifactID, markdown string) {
	if ledger := ledgerFrom(ctx); ledger != nil {
		ledger.versionTexts = append(ledger.versionTexts, ledgerVersionText{artifactID: artifactID, markdown: markdown})
	}
}

// Commit runs the transaction's write phase (its queue, one batch with the settlement credit's
// statements after it), then the steps registered to follow that batch (AfterWrites), and commits
// it; then credits, releases and publishes what its document operations recorded (see Ledger),
// which frees each room's writer slot; then fills the record digests its write phase left owed
// (fillOwed), each issue's in a transaction of its own; then closes this process's instance of
// each issue room a document joined in it (closeJoinedRooms), publishes the events its document
// operations appended and then those of the fills, so a caller's own events, published after
// Commit returns, follow them, and stores the display trees of the versions' markdown
// (storeVersionTrees). The fill follows the publish because it takes the issue's owner lock again,
// which the commit freed: another writer of the issue may hold that lock by then and wait for
// this write's slot (joinLiveWrite), a wait Postgres cannot see, so a fill that held the slot
// would wait on that writer for good. A statement of the write phase that fails, a callback
// that refuses its result, or a step after the batch that fails, stops it: the transaction rolls
// back, nothing reaches a room, and Commit returns the first such error unchanged. When the commit
// itself returns an error its outcome is unknown, so the rooms the transaction wrote are failed
// and reload the durable document.
func (l *Ledger) Commit(ctx context.Context) error {
	if err := l.commit(ctx); err != nil {
		return err
	}
	l.publish()
	l.events = append(l.events, l.service.fillOwed(ctx, l.takeFills())...)
	l.closeJoinedRooms(ctx)
	l.publishEvents()
	l.storeVersionTrees(ctx)
	for _, copy := range l.copies {
		l.service.scheduleSettle(copy)
	}
	return nil
}

// takeFills is each issue whose record digests the ledger's committed write phase left owed
// (fills), once: the caller that runs their fill takes them.
func (l *Ledger) takeFills() []string {
	fills := l.fills
	l.fills = nil
	return fills
}

// closeJoinedRooms closes this process's instance of each issue room the committed transaction
// seeded a new document's fragment into (SeedText, which resolved the document into the room its
// document_rooms row names): the document joined the room in this transaction, so an instance
// loaded before the commit holds a layout without it (layoutOf). It is closed before the writer
// answers, as every other process closes its own on the join's notice (documentRoomsMoved), so the
// writer's next write loads the room over the new layout. Left open, the instance's own
// settlement finds its layout stale and closes it (errRoomLayoutMoved, reloadReplacedRoom), and
// every write that meets that close is refused (errRoomReloading). A settlement that read the
// room's documents once this commit released the issue's row may have reserved that close before
// this one ran, so the reservation is waited out too (awaitReservedClose).
//
// A load of the room under way at the commit may have read the layout before it, and ygo reports
// a room still loading as absent (Server.GetDoc), so the look below would close nothing and the
// load would then publish an instance without the document. So the look waits first until that
// load has published its room or ended without one (awaitRoomLoad, which says how it tells that
// load from one that begins after the commit and reads the new layout). The wait is bounded by
// ctx, as the wait for a reserved close is, and by catchUpLoadingTimeout, the time a catch-up
// gives ygo to publish a room still loading.
//
// A load that begins after that look reads the layout after the commit, unless it takes the
// layout a socket's admission read before the commit (admittedIssueRead). So before the look the
// join is recorded (noteLayoutMoved), and no load that asks for an admission's read from then on
// takes one that began before it: a load that asks after the look asks after the record.
func (l *Ledger) closeJoinedRooms(ctx context.Context) {
	for document, seed := range l.seeds {
		if seed.room == document {
			continue
		}
		l.service.noteLayoutMoved()
		l.service.awaitRoomLoad(ctx, seed.room)
		if live := l.service.srv.GetDoc(seed.room); live != nil && !slices.Contains(l.service.layoutOf(live), document) {
			l.service.closeMovedRoom(context.WithoutCancel(ctx), seed.room)
		}
		l.service.awaitReservedClose(ctx, seed.room)
	}
}

// publishEvents publishes the events this ledger's document operations appended, in the order
// they appended them.
func (l *Ledger) publishEvents() {
	for _, event := range l.events {
		l.service.events.Publish(event)
	}
}

// storeVersionTrees stores the display tree of the markdown every version this transaction wrote,
// now that those versions are durable: a version's markdown is a stored text, and a read of it
// answers that tree beside it (api's storedText). It stands here, behind the commit, rather than
// in the routes that write a version, because every one of those writes passes through this
// ledger while the markdown itself never reaches the route, and because a tree that cannot be
// stored must not fail a write that has committed: a read of a version whose tree is missing
// reads the markdown, exactly as it did before there was a table.
//
// A failure is logged with the documents it was for and changes no answer. The store itself is
// texttrees.StoreFor, the one policy every path that writes a stored text keeps, which is also
// why a caller still holding a pooled connection stores nothing; the texts are gathered only
// where there is something to store, so that caller builds no slice either.
func (l *Ledger) storeVersionTrees(ctx context.Context) {
	if len(l.versionTexts) == 0 || store.HoldsPooledConnection(ctx) {
		return
	}
	written := l.versionTexts
	l.versionTexts = nil
	texts := make([]string, 0, len(written))
	for _, one := range written {
		texts = append(texts, one.markdown)
	}
	if err := texttrees.StoreFor(ctx, l.service.store, pmdoc.DisplayLatest, texts...); err != nil {
		// The documents are listed for this line alone, so they are gathered where it is written
		// rather than on every committed write.
		documents := make([]string, 0, len(written))
		for _, one := range written {
			documents = append(documents, one.artifactID)
		}
		slog.Warn("dispatch: store document version display trees failed", "documents", documents, "error", err)
	}
}

// commit is Commit up to the publish. Another transaction can run between the two, and tests
// call them apart to hold that window open.
func (l *Ledger) commit(ctx context.Context) error {
	err := l.runQueue(ctx, l.commitWrites)
	if err == nil {
		err = l.runAfterWrites(ctx)
	}
	if err != nil {
		// The transaction rolls back, so its writes reached neither the store nor a room: what it
		// leaves behind is what a transaction that never committed leaves (Discard).
		_ = l.tx.Rollback(context.Background())
		l.Discard()
		return err
	}
	l.written = true
	captures := make([]roomCapture, 0, len(l.versions)+len(l.flushes)+len(l.order))
	for _, written := range l.versions {
		// The capture holds the record of the room the version was read from, the document's
		// issue room for a converted document (targetOf), which the commit locks, sorts and lets
		// go under that room's id: let go under the document's own id, which names no room this
		// server holds, the record would be forgotten while its room still holds it.
		room := written.artifactID
		if moved, held := l.roomOf(room); held {
			room = moved
		}
		captures = append(captures, roomCapture{room: room, capture: written.capture})
	}
	for _, flush := range l.flushes {
		captures = append(captures, flushCapture(flush))
	}
	// Each write leaves its room a shadow (roomShadow), installed by the commit under its state's
	// lock, after the flushes' classes are marked stored. The shadow's document is the write's
	// fork, so here is the last point the fork is the write's own, where its rendering is checked
	// (checkRendering): once the commit drops the document's advisory lock, the room's worker
	// stores the browser updates that lock held back and measures each on the shadow
	// (roomShadow.measure), before the publish or after it.
	for _, artifactID := range l.order {
		write := l.live[artifactID]
		if l.service.checkRendering != nil {
			l.service.checkRendering("commit", write)
		}
		if shadow := write.newShadow(); shadow != nil {
			captures = append(captures, roomCapture{room: artifactID, capture: authorCapture{state: write.state}, shadow: shadow})
		}
	}
	// The cursors before the commit, not after it: this transaction holds the document's advisory
	// lock until the commit drops it, and the writers queued on that lock take it the instant it
	// goes. Recorded after the commit, a derived write - a settlement - would read the head this
	// commit wrote against a cursor still standing one version behind it and be refused
	// (ErrRoomBehindHead), and a browser's append would land past a gap this process's own write
	// left in its room's cursor. Nothing can read a cursor standing at a version this commit has
	// not yet made durable and write durable state from it: every durable writer of the document
	// takes that lock first, and catch-up, which reads the cursor without it, only skips the rows
	// such a cursor already names (catchUpRoom). A commit that fails fails these rooms (fail), so
	// the instance holding such a cursor is dropped with them.
	l.takeRoomCursors()
	err = l.service.commitConsuming(ctx, l.tx, captures, func(error) {
		// The transaction has ended whichever way the commit went, so a rebuild's room reads the
		// history the commit left from here.
		l.endRebuilds()
	})
	if err == nil && l.service.afterLiveCommit != nil {
		for _, artifactID := range l.order {
			l.service.afterLiveCommit(artifactID)
		}
	}
	l.versions = nil
	if err != nil {
		l.fail(err)
		l.flushes = nil
		return err
	}
	for _, flush := range l.flushes {
		if flush.storedContentChange() {
			l.service.scheduleSettleAfterAppend(flush.room)
		}
	}
	l.noteAppended()
	l.flushes = nil
	l.creditRooms()
	return nil
}

// runAfterWrites runs, in the ledger's transaction, each step registered to follow the write
// phase's batch (AfterWrites), each as a batch of its own (store.RunWrite), in the order they were
// registered, and returns the first failure as that batch returned it. runQueue has sealed the
// ledger, so no step is registered meanwhile.
func (l *Ledger) runAfterWrites(ctx context.Context) error {
	steps := l.afterWrites
	l.afterWrites = nil
	for _, step := range steps {
		if err := store.RunWrite(ctx, l.writer, step); err != nil {
			return err
		}
	}
	return nil
}

// noteAppended notes to the compaction queue (noteStoredUpdates) the last version each append of
// the committed transaction made durable: each live write's own appends, and the room updates it
// flushed ahead of them. A document the transaction both wrote and flushed is noted once for
// each, which asks the queue for the one check a single note would (compactionQueue.note). A
// seed's row is its document's first, which leaves nothing to fold.
func (l *Ledger) noteAppended() {
	for _, artifactID := range l.order {
		if versions := l.live[artifactID].versions; len(versions) > 0 {
			l.service.noteStoredUpdates(artifactID, int64(versions[len(versions)-1]))
		}
	}
	for _, flush := range l.flushes {
		if appended := flush.appended(nil); len(appended) > 0 {
			l.service.noteStoredUpdates(flush.room, int64(appended[len(appended)-1]))
		}
	}
}

// takeRoomCursors records on each room instance a write was made on, and on the room instance its
// publish will reach, the versions this commit is about to make durable, before the commit that
// does (commit) and the publish that applies them to the room (publish). The room does not hold
// them yet, and nothing in that window renders from it: the write holds its writer slot until the
// publish has returned, which defers its room's settlement (settleRoomWithin's supersession
// check) and holds off every other transaction's write and version (awaitLiveWriter). What does
// run in that window is a browser's append queued on the document's advisory lock, which takes it
// the instant this commit drops it and lands past the commit's versions, and a catch-up of the
// room, which skips the rows these versions name, since the publish applies them
// (TestSettlementBetweenCommitAndPublishWaitsForTheWrite,
// TestABrowserAppendQueuedBehindThisProcessesWriteIsNotRefused). A catch-up that reads in the
// moment between this record and the commit sees a head below the cursor, a replaced history, and
// confirms that verdict under the document's lock before it closes anything
// (historyReplacedUnderLock), by which time this commit has landed.
//
// The two instances are one and the same for a warm write whose room is still resident. They are
// not for a cold write, which has no cursor of its own at all (coldFork): the room it publishes
// into is one that loaded, or is loading, while the write ran, and advancing that room here is
// the only thing that keeps it at the head across this window - a room still loading has no
// document for ygo to hand out, so the version goes to the record its load leaves the instance
// (tookResidentVersion, roomLoad). They are not one for a write whose room was replaced since its
// fork either, where the fork's cursor is the gone instance's. A room that loaded at or past
// these versions takes nothing in - advanceRoomCursor only advances by the version that follows
// the one it stands at - and the publish records them again on the room it reaches
// (publishLiveUpdate).
//
// The room updates the transaction flushed (flushThrough) are already in the instance they were
// made in, and only there: they advance that instance's cursor and reach no other, as a repair's
// append does (advanceRoomCursor). A flush and a write of one instance interleave their versions
// (an operation's flush, its append, the next operation's flush), so the instance takes one run
// of both, in version order (RoomFlush.appended): a cursor advances only by the version after
// the one it stands at. A write whose room is not the flushed instance - a cold write, or one
// whose room was replaced since its fork - takes in its own versions alone. Which of the two a
// write is is decided in one place, the loop over the writes below: a flush of that write's
// document made on the instance it wrote takes the write's versions in with its own, and a
// flush no write of this transaction shares an instance with takes in its own alone.
func (l *Ledger) takeRoomCursors() {
	taken := make(map[*RoomFlush]bool, len(l.flushes))
	for _, artifactID := range l.order {
		write := l.live[artifactID]
		if flush := l.flushFor(artifactID); flush != nil && flush.cursor == write.point.cursor {
			taken[flush] = true
			advanceRoomCursorThrough(flush.cursor, flush.appended(write.versions))
		} else {
			advanceRoomCursorThrough(write.point.cursor, write.versions)
		}
		for _, version := range write.versions {
			l.service.tookResidentVersion(artifactID, version)
		}
	}
	for _, flush := range l.flushes {
		if !taken[flush] {
			advanceRoomCursorThrough(flush.cursor, flush.appended(nil))
		}
	}
}

// roomCapture is a version's capture with the room it versions its document in, the room updates
// a transaction flushed (flush), or the shadow a live write leaves its room (shadow); the last two
// hold only the state, which the commit locks.
type roomCapture struct {
	room    string
	capture authorCapture
	flush   *RoomFlush
	shadow  *roomShadow
}

// flushCapture is the roomCapture of flush, which locks the state flush took its updates from.
func flushCapture(flush *RoomFlush) roomCapture {
	return roomCapture{room: flush.room, capture: authorCapture{state: flush.state}, flush: flush}
}

// commitConsuming commits tx, which wrote the versions captures lists and the room updates their
// flushes stored, and once it has committed marks the in-flight credits each version listed
// consumed and each flushed update stored, its credit held and landed (RoomFlush.markStored), frees
// a shadow left with nothing to measure (dropDrainedShadowLocked), and installs each live write's
// shadow (installShadowLocked), in captures' order. Each capture's state is locked (state.mu) from
// before the commit until then: an append queued behind the document's advisory lock, which tx
// holds until it commits, takes that lock the instant the commit drops it, and must find its
// credit consumed when it reads it (UpdateCredit.take), or it writes an author a version listed
// to the pending authors; and a durable writer that takes it must find the updates this commit
// stored marked, or it stores them again, and the write's shadow installed, or it stores a class
// measured without the write (flushThrough). The states are locked in their rooms' sorted order,
// so two transactions versioning the same two documents cannot deadlock. ended runs once
// the commit returns, with its error and the states still locked; a capture with no state locks
// nothing. A commit that fails marks nothing: the room's persistence stores the flushed updates
// later.
func (s *Service) commitConsuming(ctx context.Context, tx pgx.Tx, captures []roomCapture, ended func(error)) error {
	sorted := slices.Clone(captures)
	slices.SortStableFunc(sorted, func(a, b roomCapture) int { return strings.Compare(a.room, b.room) })
	locked := make([]roomCapture, 0, len(sorted))
	holds := func(state *roomState) bool {
		return slices.ContainsFunc(locked, func(held roomCapture) bool { return held.capture.state == state })
	}
	for _, written := range sorted {
		if state := written.capture.state; state != nil && !holds(state) {
			state.mu.Lock()
			locked = append(locked, written)
		}
	}
	err := tx.Commit(ctx)
	var unlanded []*UpdateCredit
	var dropped []*roomShadow
	if err == nil {
		for _, written := range captures {
			written.capture.consumeLocked()
			if written.flush != nil {
				unlanded = append(unlanded, written.flush.markStored(holds)...)
				if state := written.capture.state; state != nil {
					dropped = append(dropped, state.dropDrainedShadowLocked())
				}
			}
			if written.shadow != nil {
				s.installShadowLocked(written.room, written.capture.state, written.shadow)
			}
		}
	}
	if ended != nil {
		ended(err)
	}
	for _, held := range locked {
		s.unlockState(held.room, held.capture.state)
	}
	// Each shadow the commit dropped lets go of its document here, holding no state lock
	// (roomShadow.release).
	for _, shadow := range dropped {
		shadow.release()
	}
	for _, credit := range unlanded {
		credit.landed()
	}
	return err
}

// Discard drops what a transaction that did not commit left behind: its queued writes and the
// steps registered to follow them (AfterWrites), which never ran, its restore carries, its live
// writes, which no room ever saw, and the rooms its rebuilds held. A version it wrote marked
// nothing it read, so its authors stay where they were. After Commit only the read-backs and the
// queued versions (latest) are left to drop. Every caller defers it, so it is where the request's
// use of the ledger ends.
func (l *Ledger) Discard() {
	l.sealed = true
	l.queue, l.closing = nil, nil
	l.restores, l.restoring = nil, nil
	l.afterWrites = nil
	l.fills = nil
	for _, artifactID := range l.order {
		l.service.finishLiveWrite(l.live[artifactID])
	}
	l.versions = nil
	l.versionTexts = nil
	l.flushes = nil
	// The read-backs serve the request that made them and nothing after it (ReadBackOf), while
	// the ledger outlives the request by a long way: a pooled pgx connection keeps the context of
	// the queries it last served (liveWrite.releaseFork says why that matters), and this ledger
	// is a value of that context. Kept, a whole parsed tree of every document the transaction
	// wrote whole stayed resident beside the document's room
	// (TestAnUploadsReadBackTreeGoesWhenItsRequestEnds).
	l.readBacks = nil
	// The versions this transaction queued (queuedVersion) are read only until its write phase
	// seals the ledger, when the table holds them, and each holds its version's whole markdown,
	// which the same pooled context would keep.
	l.latest = nil
	l.endRebuilds()
}

func (l *Ledger) holdRebuild(artifactID string) {
	l.rebuilds = append(l.rebuilds, artifactID)
}

func (l *Ledger) endRebuilds() {
	for _, artifactID := range l.rebuilds {
		l.service.rebuilding.Delete(artifactID)
	}
	l.rebuilds = nil
}

// recordVersion records a version this transaction wrote, with whom it listed (capture), which
// its commit takes out of the pending authors (recordSettlementCredit, commitConsuming). The
// version holds every change the transaction's write to the document has made so far, so the
// authors it lists leave the write's credits: a change the write makes after it credits its author
// again (creditLiveWrite), and that author is pending once the transaction commits.
func (l *Ledger) recordVersion(artifactID string, version model.Version, capture authorCapture) {
	l.versions = append(l.versions, ledgerVersion{artifactID: artifactID, version: version, capture: capture})
	if write := l.liveWriteFor(artifactID); write != nil {
		for _, author := range version.Authors {
			delete(write.credits, actorKey(author))
			delete(write.creditedBy, actorKey(author))
		}
	}
}

// WroteVersion records version, of artifactID, holding markdown, which the caller wrote itself in
// this transaction, outside the document service, over the transaction's own write to the document
// - an upload, whose version lists its uploader alone. Once this transaction commits, it has
// cleared every author whose change the write's last read of the room held, whether its
// replacement removed that edit or kept it (uploadCapture): each pending author row whose writing
// update that room held, and each in-flight credit observed by then. The write held the document's
// advisory lock from before that read (ReplaceText), so nothing landed between them. A row another
// process's room stored, and a credit observed after the read, stay pending for the next version.
// An upload that changed nothing has no write to hold and nothing to clear.
//
// The markdown is recorded whether or not there is a write to clear (WroteVersionText), since the
// version holds it either way.
func (l *Ledger) WroteVersion(artifactID string, version model.Version, markdown string) {
	l.WroteVersionText(artifactID, markdown)
	write := l.liveWriteFor(artifactID)
	if write == nil || len(write.updates) == 0 {
		return
	}
	l.recordVersion(artifactID, version, uploadCapture(write))
}

// WroteVersionText records the markdown of a version the caller wrote itself in this transaction,
// outside the document service, whose display tree this transaction's commit stores
// (storeVersionTrees). It is what a caller with no authors to clear records: a new issue's spec,
// whose version is the document's first and lists its writer alone (api's createIssue). A version
// written through the service records its own (collectVersionText).
func (l *Ledger) WroteVersionText(artifactID, markdown string) {
	if l == nil {
		return
	}
	l.versionTexts = append(l.versionTexts, ledgerVersionText{artifactID: artifactID, markdown: markdown})
}

// liveWriteFor is the transaction's write to artifactID's room: artifactID names the room itself,
// or a document the transaction resolved into it (Service.targetOf, noteRoom).
func (l *Ledger) liveWriteFor(artifactID string) *liveWrite {
	if l == nil {
		return nil
	}
	if room, moved := l.issueRooms[artifactID]; moved {
		artifactID = room
	}
	return l.live[artifactID]
}

// roomOf is the issue room the transaction resolved document into (Service.targetOf), and whether
// it did.
func (l *Ledger) roomOf(document string) (string, bool) {
	if l == nil {
		return "", false
	}
	room, held := l.issueRooms[document]
	return room, held
}

// noteRoom records that document is edited in room (Service.targetOf), where room is another
// room than the document itself: a document that is its own room is resolved again by each
// operation, since a transaction may yet write its document_rooms row.
func (l *Ledger) noteRoom(document, room string) {
	if l == nil || document == room {
		return
	}
	if l.issueRooms == nil {
		l.issueRooms = make(map[string]string)
	}
	l.issueRooms[document] = room
}

// RegisterPendingRoom records that document is edited in room for a document_rooms row this
// transaction writes in its write phase, after the artifacts row its foreign key names: no read of
// the transaction can see that row before then, so the operations that resolve the document's
// room through the ledger (Service.targetOf, queueVersionInsert) take it from here, as noteRoom
// records a room the transaction resolved. A read that resolves a room in its own SQL
// (refs.DocumentRoomSQL) does not see it.
func (l *Ledger) RegisterPendingRoom(document, room string) {
	l.noteRoom(document, room)
}

// pendingJoins is every document this transaction creates (RegisterPendingArtifact) whose
// document_rooms row naming room it writes in its write phase (RegisterPendingRoom), in id order,
// beside the band each one's row will rank in (refs.DocumentAddress.Band): what document_rooms
// does not hold of room yet for a read of its layout made before that row is written
// (readJoinedRoomLayout).
func (l *Ledger) pendingJoins(room string) (documents, bands []string) {
	if l == nil {
		return nil, nil
	}
	for document, joined := range l.issueRooms {
		if _, created := l.artifacts[document]; created && joined == room {
			documents = append(documents, document)
		}
	}
	slices.Sort(documents)
	bands = make([]string, len(documents))
	for index, document := range documents {
		bands[index] = string(l.artifacts[document].Band)
	}
	return documents, bands
}

// LostOps reports which operations of this transaction's write to artifactID the live document
// no longer held once the write was published, which the caller reads after Commit. The second
// return is false when no verdict was reached - the transaction wrote nothing to that document,
// its publish failed and the room is reloading, or the room holds a tree past the schema's depth
// bound, which no read can serve - which the caller reports as undetermined rather than as
// survival (LEGION-269).
func (l *Ledger) LostOps(artifactID string) ([]int, bool) {
	write := l.liveWriteFor(artifactID)
	if write == nil || !write.lostVerdict {
		return nil, false
	}
	return write.lost, true
}

func (l *Ledger) addLiveWrite(write *liveWrite) {
	if l.live == nil {
		l.live = make(map[string]*liveWrite)
	}
	l.live[write.artifactID] = write
	l.order = append(l.order, write.artifactID)
	write.flushed = l.flushFor(write.artifactID)
}

// commitWrites is the last part of the write phase, queued on its batch after every write the
// transaction's operations and its caller made (runQueue): the settlement credit's statements,
// then the one announcement of the documents this transaction appended to, which has to follow
// each of their appends (announceAppends).
func (l *Ledger) commitWrites(ctx context.Context) error {
	if err := l.recordSettlementCredit(ctx); err != nil {
		return err
	}
	return l.announceAppends(ctx)
}

// recordSettlementCredit records, in the transaction that wrote the documents, who owes what once
// it commits. It first deletes the pending authors each version this transaction wrote takes out
// (deleteCapturedPendingAuthors), then records, in one statement per write, each write's authors no
// version of the transaction lists (recordVersion), each at the append that recorded that author's
// latest change (liveWrite.creditedBy), and each write's and seed's latest edit source on the
// document's pending-settlement row. Deleting first means the write's own authors are recorded
// after the delete that would otherwise take them out. It records the author of every ask block
// each seed and write introduced, too (recordAskAuthors), which creditRooms registers in the room
// once the transaction has committed, so a settlement on any process finds it. All of it commits
// or rolls back with the content, before the request context can be canceled after commit. A
// transaction whose every write was a server lifecycle write (withLifecycleWrite) credited no
// one, so it records nothing here. Commit runs it as its write phase's last part (commitWrites,
// runQueue), so its statements join that batch after every queued write: everything they write
// is known in memory by then.
func (l *Ledger) recordSettlementCredit(ctx context.Context) error {
	for _, written := range l.versions {
		if err := deleteCapturedPendingAuthors(ctx, l.tx, written.artifactID, written.capture); err != nil {
			return err
		}
	}
	released := l.releasedAuthors()
	for artifactID, seed := range l.seeds {
		if err := recordAskAuthors(ctx, l.tx, seed.room, seededAskAuthors(seed)); err != nil {
			return err
		}
		if _, listed := released[artifactID][actorKey(seed.actor)]; listed {
			continue
		}
		if err := recordLatestEditSource(ctx, l.tx, seed.room, &seed.actor); err != nil {
			return err
		}
	}
	for _, artifactID := range l.order {
		write := l.live[artifactID]
		if write.actor == nil {
			continue
		}
		pending := writtenPendingAuthors(write)
		if err := upsertPendingAuthors(ctx, l.tx, artifactID, pending); err != nil {
			return err
		}
		if err := recordLatestEditSource(ctx, l.tx, artifactID, write.actor); err != nil {
			return err
		}
		if err := recordAskAuthors(ctx, l.tx, artifactID, writtenAskAuthors(write)); err != nil {
			return err
		}
	}
	return nil
}

// writtenPendingAuthors is the pending rows write's authors leave once its transaction commits:
// each author no version of the transaction listed, at the append that recorded its latest change
// (liveWrite.creditedBy), under each document whose fragment the write's content changes wrote
// (creditedDocuments), as an issue room's browser update records its authors (pendingAuthorsIn):
// the room's own rows for a room that is its own document, or whose own fragment alone was written.
func writtenPendingAuthors(write *liveWrite) []pendingAuthor {
	pending := pendingAuthorsOf(write.credits, func(key string) persistence.Version { return write.creditedBy[key] })
	if len(write.creditedDocuments) == 0 || slices.Equal(write.creditedDocuments, []string{write.artifactID}) {
		return pending
	}
	documents := make([]pendingAuthor, 0, len(pending)*len(write.creditedDocuments))
	for _, document := range write.creditedDocuments {
		if document == write.artifactID {
			document = ""
		}
		for _, author := range pending {
			author.document = document
			documents = append(documents, author)
		}
	}
	return documents
}

// seededAskAuthors is the author of every ask block seed introduced: its actor, as creditRooms
// registers it.
func seededAskAuthors(seed ledgerSeed) map[string]model.Actor {
	ids := askBlockIDs(seed.rendered.tree)
	authors := make(map[string]model.Actor, len(ids))
	for id := range ids {
		authors[id] = seed.actor
	}
	return authors
}

// writtenAskAuthors is the author of every ask block write introduced or renamed, as creditRooms
// registers them: the carried-forward author of a renamed id over the write's actor, which gets
// every id its own before/after diff added.
func writtenAskAuthors(write *liveWrite) map[string]model.Actor {
	authors := make(map[string]model.Actor, len(write.addedAskBlockIDs)+len(write.carriedAskAuthors))
	for id := range write.addedAskBlockIDs {
		authors[id] = *write.actor
	}
	maps.Copy(authors, write.carriedAskAuthors)
	return authors
}

// releasedAuthors is, per artifact, the keys of the authors this transaction's own versions
// listed, which a seed of the same transaction does not record as the latest edit source again.
func (l *Ledger) releasedAuthors() map[string]map[string]struct{} {
	released := make(map[string]map[string]struct{}, len(l.versions))
	for _, written := range l.versions {
		keys := released[written.artifactID]
		if keys == nil {
			keys = make(map[string]struct{}, len(written.version.Authors))
			released[written.artifactID] = keys
		}
		for _, actor := range written.version.Authors {
			keys[actorKey(actor)] = struct{}{}
		}
	}
	return released
}

// creditRooms records, once the transaction has committed and before its live writes publish,
// each seed's and write's latest edit source in its room, and registers the ask blocks each
// introduced to its author. The authors themselves, the latest edit sources and the ask blocks'
// authors are already recorded in the database (recordSettlementCredit), which is where a
// settlement on any process reads the edit source its events name (settlementAuthors) and, for a
// block its room did not register, the ask block's author (askBlockSources.author).
func (l *Ledger) creditRooms() {
	for _, seed := range l.seeds {
		state := l.service.lockState(seed.room)
		state.creditSeq.Add(1)
		state.lastActor = new(seed.actor)
		state.unsettled = true
		state.registerAskAuthors(askBlockIDs(seed.rendered.tree), seed.actor)
		l.service.unlockState(seed.room, state)
	}
	for _, artifactID := range l.order {
		write := l.live[artifactID]
		if write.actor == nil {
			continue
		}
		state := l.service.lockState(artifactID)
		state.creditSeq.Add(1)
		state.lastActor = write.actor
		state.unsettled = true
		if write.actor != nil {
			// The carried-forward author of a renamed id is registered before the ids this
			// write's own before/after diff adds, so a rename's more specific registration is
			// never overwritten by the less specific one applyLive's diff would otherwise give
			// it (registerAskAuthors, registerCarriedAskAuthors).
			state.registerCarriedAskAuthors(write.carriedAskAuthors)
			state.registerAskAuthors(write.addedAskBlockIDs, *write.actor)
		}
		l.service.unlockState(artifactID, state)
	}
}

// seeded records that this transaction seeded artifactID's first text, in room, as actor
// (SeedText), with the rendering of the tree it wrote, whose ask blocks credit registers to actor
// once the transaction commits.
func (l *Ledger) seeded(artifactID, room string, rendered renderedTree, actor model.Actor) {
	if l.seeds == nil {
		l.seeds = make(map[string]ledgerSeed)
	}
	l.seeds[artifactID] = ledgerSeed{actor: actor, room: room, rendered: rendered}
}

// writtenRendering is the rendering of the tree this transaction last wrote to artifactID: the one
// its live write's latest operation recorded of that document (recordLiveOperation, for applyLive
// and applyMargin alike), else the tree it seeded (SeedText); none when it holds neither.
func (l *Ledger) writtenRendering(artifactID string) renderedTree {
	if l == nil {
		return renderedTree{}
	}
	if write := l.liveWriteFor(artifactID); write != nil && write.tree != nil && write.treeDocument == artifactID {
		return renderedTree{tree: write.tree, markdown: write.markdown, offsets: write.offsets}
	}
	return l.seeds[artifactID].rendered
}

// wroteReadBack records the read-back a write of artifactID's whole text already made of the
// markdown it stores (pmdoc.ParseWritten): the parse ReadBackOf hands to the rest of the request.
func (l *Ledger) wroteReadBack(artifactID string, read pmdoc.WrittenReadBack) {
	if l == nil || read.Tree == nil {
		return
	}
	if l.readBacks == nil {
		l.readBacks = make(map[string]pmdoc.WrittenReadBack)
	}
	l.readBacks[artifactID] = read
}

// ReadBackOf is markdown's tree, read back by this transaction's own write of artifactID while it
// parsed what it stored (pmdoc.ParseWritten), or nil when this transaction made no such read-back
// or made one of another rendering: a caller holds the markdown it stored and shares that parse
// only where the two are the same text, as IndexDocumentReferences shares the write's rendering
// only where it is the version's markdown. A caller that reads nil parses the markdown itself.
func (l *Ledger) ReadBackOf(artifactID, markdown string) *pmdoc.Node {
	if l == nil {
		return nil
	}
	read, held := l.readBacks[artifactID]
	if !held || read.Markdown != markdown {
		return nil
	}
	return read.Tree
}

// publish applies the committed transaction's live writes to their rooms and broadcasts them.
// Each update is already durable, so the room's own persistence of it is suppressed. A room that
// cannot take its update is failed, and reloads the durable document on its next access.
func (l *Ledger) publish() {
	for _, artifactID := range l.order {
		if write := l.live[artifactID]; !write.finished {
			l.service.publishLiveWrite(write)
		}
	}
}

// fail fails the rooms a transaction wrote when its commit returned an error. The commit may
// have gone through, so the rooms cannot tell whether they should hold the writes, or the room
// updates the transaction flushed, whose versions their cursors already took in (takeRoomCursors);
// failed, they reload the durable document, whichever way the commit went.
func (l *Ledger) fail(cause error) {
	for _, flush := range l.flushes {
		if write := l.live[flush.room]; len(flush.versions) > 0 && (write == nil || len(write.updates) == 0) {
			l.service.failRoom(flush.room, fmt.Errorf("commit flushed document updates: %w", cause))
		}
	}
	for _, artifactID := range l.order {
		write := l.live[artifactID]
		if write.finished {
			continue
		}
		if len(write.updates) > 0 {
			l.service.failRoom(artifactID, fmt.Errorf("commit live document write: %w", cause))
		}
		l.service.finishLiveWrite(write)
	}
}

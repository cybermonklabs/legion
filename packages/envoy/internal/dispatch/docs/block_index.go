package docs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// blockIndexDocumentBatch is how many owed documents one block-index tick reads,
// blockIndexItemBatch how many items owed an index with the block grammar and, separately, how
// many comments, messages and asks owed a block index of their own blocks;
// blockIndexWindowBatch how many owners with blocks and no embedding window it gives windows
// to and blockIndexRetireBatch the most window owners one kind's pass of the whole-text
// retirement reads, and the most rows the retirement deletes in one tick across the kinds;
// blockIndexStep is the wait before the next tick while any batch was full,
// BlockIndexReconcileInterval the wait
// once none is. The owed reads go through partial indexes (document_block_versions_owed,
// document_block_versions_store_owed, block_grammar_owed_enqueued, comment_block_state_owed,
// message_block_state_owed, ask_block_state_owed), and the window arm's reads through the
// markers' own (document_block_versions_windows_owed, comment_block_state_windows_owed,
// message_block_state_windows_owed, ask_block_state_windows_owed), so a tick that finds
// nothing owed reads nothing else.
const (
	blockIndexDocumentBatch     = 20
	blockIndexItemBatch         = 200
	blockIndexWindowBatch       = 200
	blockIndexRetireBatch       = 500
	blockIndexStep              = 2 * time.Second
	BlockIndexReconcileInterval = 30 * time.Second
)

// BlockIndexLease names the lease one block-index tick runs under. The index it writes is the
// same whichever process writes it, so two processes indexing the same batch is work done twice
// for one result. It is exported for the memory harness, which takes this lease itself for the
// length of each measured window (cmd/dispatch's memoryServer.peakAboveIdle): a pass that ran
// inside such a window would read whole documents of its own, and the peak measured there would
// be that pass's memory and not the request's.
const BlockIndexLease = "block-index"

// BlockIndexCursors is where a tick's keyset reads start, as the tick before it left them
// (BlockIndexTick.Cursor): the owed documents, and, for each owner kind the window arm walks
// ("document", "comment", "message", "ask"), the owner its batch of that kind starts after. A
// zero value starts every read from the beginning.
//
// Retire is the same for the retirement arm, keyed by the whole-text kind the arm retires
// ("document", "comment", "message", "ask") and holding the owner key of a window - the id up
// to the first #, so an item's carries its "<kind>/" prefix - that kind's next pass starts
// after.
type BlockIndexCursors struct {
	Documents string
	Windows   map[string]string
	Retire    map[string]string
}

// BlockIndexTick is what one tick of the block-index reconciliation did (ReconcileBlockIndex).
type BlockIndexTick struct {
	// Documents is how many owed documents it indexed, Items how many owed comments, asks and
	// issue messages it indexed with the block grammar, and ItemBlocks how many owed comments and
	// messages (direct messages included) it indexed the blocks of.
	Documents  int
	Items      int
	ItemBlocks int
	// Texts is how many rows of the re-index queue (refs_grammar_owed) it drained: a comment, an
	// ask or an issue message re-indexed with this build's whole grammar, a document owed that
	// index for the documents batch to write, or one document's whole scope re-indexed, which is
	// the one row a `scope` row is counted as.
	Texts int
	// Windows is how many owners with blocks and no embedding window it gave windows to, and
	// Retired how many whole-text embeddings rows it deleted behind a current window.
	Windows int
	Retired int
	// LiveAhead is how many owed documents it skipped because their live text is ahead of their
	// latest version and differs from it, whatever their issue's status: owed a settlement that
	// has not run. An owed issue room counts once, where any of its fragments is.
	LiveAhead int
	// Unreadable is how many owed issue rooms it left owed because a fragment of the room did not
	// read back (indexOwedRoom): nothing of such a room is indexed until a write moves it.
	Unreadable int
	// Events is how many events of the log after this build's cursor the tick read, which every
	// tick reads, Marked how many documents an artifact.version, artifact.created or issue.created
	// event among them named that it marked owed a block index, Comments how many comments a
	// comment.created or comment.answered event among them named whose rows it gave their nodes,
	// and AnotherBuild whether a live server process of another build served, which decides
	// nothing of which events the tick reads and makes it owe the object pass
	// (readEventsAfterCursor).
	Events       int
	Marked       int
	Comments     int
	AnotherBuild bool
	// ObjectNodes is how many issues, documents, asks and messages whose row named no node the
	// tick wrote the node of, OrphansGone how many document nodes whose row another build removed
	// it stated gone, and StaleLensesGone how many nodes of stale lenses - each lens whose recorded
	// row no longer stands at its document's address, and everything under it - it stated gone,
	// while this build owed the object pass (writeObjectNodesAnotherBuildWrote).
	ObjectNodes     int
	OrphansGone     int
	StaleLensesGone int
	// HeadsFilled is how many issues no head mark named the tick filled the head rows of and
	// marked (graphcensus.FillIssueHeads): an issue a write of a build that keeps no head rows
	// unmarked, and one a store of the release before holds.
	HeadsFilled int
	// Queries is how many issues' marks of queries owed a version (issue_query_marks) the tick
	// drained: each a mark a request's write left and its own drain did not clear
	// (drainOwedQueryMarks).
	Queries int
	// OwedDocuments, OwedTexts, OwedItems and OwedItemBlocks are what is still owed after the tick.
	OwedDocuments  int
	OwedTexts      int
	OwedItems      int
	OwedItemBlocks int
	// OldestOwedDocumentAt dates the oldest of the documents still owed a block index: the
	// created_at of the latest markdown version it holds, which is the write that owed it one
	// (migration 0122's trigger) and the version this tick would have indexed, or its own
	// artifact's created_at where it holds no markdown version at all. Zero where none is owed.
	//
	// Nothing dates the debt itself. document_block_versions is (artifact_id, version) and no
	// more (migration 0121), and neither the trigger nor rebuild-refs writes a time when it
	// sets the version back to null, so the age is measured from the write that owed the index
	// rather than from the mark. The two differ only where a rebuild marks a document long
	// after its last version, and then the debt reads older than it is, which is the safe
	// direction for a backlog nobody is watching. The smallest change that would date the mark
	// itself: an `owed_at timestamptz not null default now()` column on
	// document_block_versions, written wherever the version goes back to null, with a partial
	// index on it where version is null, which would also make the oldest debt one index
	// lookup instead of a probe for each owed row. That needs a migration and its census file
	// (store/README.md), which this change does not.
	//
	// A document that holds no markdown version is owed an index only until a tick reaches it:
	// indexOwedDocument's owner-locked re-read finds no markdown version and one statement clears
	// its marks and states it at version 0, so its creation dates its debt only in between.
	OldestOwedDocumentAt time.Time
	// Cursor is where the next tick's batches start: the last owner each batch listed while that
	// batch was full, and the start again once a batch lists fewer.
	Cursor BlockIndexCursors
	// Full reports a batch of documents, of texts, of items, of item blocks, of owners with no
	// window or of query marks that was full, a page of events after this build's cursor that was
	// full, or a retirement pass that deleted a row, so more may be waiting. The retirement term counts
	// rows deleted rather than a full batch because that
	// arm reads window owners and retires only the few that still hold a whole-text row: a
	// pass that read its whole slice and deleted twenty rows is not full by any count of its
	// own, and the walk would then advance one slice every BlockIndexReconcileInterval instead
	// of every blockIndexStep.
	Full bool
}

// OldestOwedDocumentAge is how long the oldest document owed a block index has been owed one,
// at now, measured from the write that owed it (OldestOwedDocumentAt). Zero where none is
// owed. A loop that drains faster than work arrives holds this at about one tick; one that
// falls behind grows it, and that growth is the backlog an operator reads.
func (tick BlockIndexTick) OldestOwedDocumentAge(now time.Time) time.Duration {
	if tick.OldestOwedDocumentAt.IsZero() || !now.After(tick.OldestOwedDocumentAt) {
		return 0
	}
	return now.Sub(tick.OldestOwedDocumentAt)
}

// RunBlockIndexReconciliation indexes what no block-aware version write indexed: every document
// owed a block index (document_block_versions.version null) - each one at migration 0121, each
// version any build writes (migration 0122's trigger) that its own writer did not index, and each
// rebuild-refs marks - every document holding this build's own mark (store_owed_at, migration
// 20261116221539), which the backfill and every debt this build owes beside the null version set,
// whose blocks' nodes, part_of tuples and parent_block_id are this release's to state and which
// the release before this one never clears - every comment, ask and issue message whose text
// holds `#b-`, owed an
// index with the block grammar (block_grammar_owed, migrations 0123-0126), which a build older
// than the block grammar indexes without, every source owed an index with this build's whole
// grammar (refs_grammar_owed, migrations 20261103205037-20261103205042), which a build older than
// the bare issue keys, `#id`s and short ids indexes without, and every comment, message and ask
// owed a block index
// of its own blocks (comment_block_state, message_block_state and ask_block_state owed_at, set by
// migrations 0143-0144's and 20261101143212's triggers on every write and by rebuild-refs), which
// a build older than item blocks leaves owed.
// One tick per lease, at start, then every blockIndexStep while a batch is full and every
// BlockIndexReconcileInterval once none is, until ctx ends.
//
// The ticks are duty.RunLeasedAfterEachPass's, each one the wait the pass before it asked for after
// that pass returned, and so is the period each runs under: that wait, blockIndexStep while a batch
// is full and the full interval once none is, so a tick inside it is one some process already
// made. A tick whose lease another process holds, or whose period that process has already served,
// waits the full interval: that process is doing this work. Each of after runs once each tick of
// this process has returned, on the tick's own context: R2's daemon-thread step
// (DaemonThreadStep.Tick) re-places on each tick the nodes a release before left under a message of
// a thread the Legion daemon's session began.
func (s *Service) RunBlockIndexReconciliation(ctx context.Context, leases duty.Leases, after ...func(context.Context)) {
	ctx = store.WithTransactionTracking(ctx)
	var cursors BlockIndexCursors
	// logged is the last tick this loop logged: a tick that indexed nothing and leaves the same
	// counts (a document whose live text stays ahead, skipped every tick) logs nothing.
	var logged BlockIndexTick
	liveAhead := liveAheadDocuments{}
	duty.RunLeasedAfterEachPass(ctx, leases, BlockIndexLease, BlockIndexReconcileInterval, func(ctx context.Context, _ time.Duration) time.Duration {
		var wait time.Duration
		wait, cursors = s.blockIndexTick(ctx, cursors, liveAhead, &logged)
		for _, call := range after {
			call(ctx)
		}
		return wait
	})
}

// blockIndexTick runs one tick from cursors, logs what it did unless it indexed nothing and left
// the counts *logged holds, and returns how long to wait before the next tick and the cursors it
// starts from.
func (s *Service) blockIndexTick(ctx context.Context, cursors BlockIndexCursors, liveAhead liveAheadDocuments, logged *BlockIndexTick) (time.Duration, BlockIndexCursors) {
	tick, err := s.ReconcileBlockIndex(ctx, cursors, liveAhead)
	if ctx.Err() != nil {
		return BlockIndexReconcileInterval, cursors
	}
	if err != nil {
		slog.Error("dispatch: block index reconciliation", "error", err)
	}
	if tick.Documents > 0 || tick.Items > 0 || tick.ItemBlocks > 0 || tick.Texts > 0 ||
		tick.Windows > 0 || tick.Retired > 0 || tick.Events > 0 || tick.Marked > 0 || tick.Comments > 0 ||
		tick.ObjectNodes > 0 || tick.OrphansGone > 0 || tick.StaleLensesGone > 0 || tick.HeadsFilled > 0 || tick.Queries > 0 ||
		tick.AnotherBuild != logged.AnotherBuild || tick.LiveAhead != logged.LiveAhead || tick.Unreadable != logged.Unreadable ||
		tick.OwedDocuments != logged.OwedDocuments || tick.OwedItems != logged.OwedItems ||
		tick.OwedItemBlocks != logged.OwedItemBlocks || tick.OwedTexts != logged.OwedTexts ||
		!tick.OldestOwedDocumentAt.Equal(logged.OldestOwedDocumentAt) {
		// The backlog goes in the line that says what the tick did, so an operator reading the
		// log sees a growing one: the count, the oldest debt's own timestamp and its age now.
		// The age moves every tick while a document stays owed, and logging on that alone would
		// write a line every interval for ever, so the line is written when the oldest debt
		// changes - the drain reaching it, or a new backlog starting - and not as it ages.
		// events= is how many events the tick read after its build's event cursor, marked= and
		// comments= the documents it marked owed and the comments it gave their nodes from those
		// events, object_nodes=, orphans_gone= and stale_lenses_gone= the rows the object pass gave
		// their nodes, the orphaned document nodes it stated gone and the nodes of stale lenses it
		// stated gone, queries= the issues whose marks of queries owed a version it drained
		// (drainOwedQueryMarks), and another_build= whether a live server process
		// of another build served, which decides nothing of which events the tick reads and makes
		// it owe the object pass (readEventsAfterCursor).
		slog.Info("dispatch: block index", "documents", tick.Documents, "items", tick.Items,
			"item_blocks", tick.ItemBlocks, "texts", tick.Texts, "windows", tick.Windows,
			"retired", tick.Retired, "events", tick.Events, "marked", tick.Marked,
			"comments", tick.Comments, "object_nodes", tick.ObjectNodes, "orphans_gone", tick.OrphansGone,
			"stale_lenses_gone", tick.StaleLensesGone, "heads_filled", tick.HeadsFilled, "queries", tick.Queries,
			"another_build", tick.AnotherBuild, "owed_documents", tick.OwedDocuments,
			"owed_items", tick.OwedItems, "owed_item_blocks", tick.OwedItemBlocks,
			"owed_texts", tick.OwedTexts, "live_ahead", tick.LiveAhead, "unreadable_rooms", tick.Unreadable,
			"oldest_owed_at", refs.OwedTimeText(tick.OldestOwedDocumentAt),
			"oldest_owed_age_seconds", tick.OldestOwedDocumentAge(time.Now()).Seconds())
		*logged = tick
	}
	if tick.Full {
		return blockIndexStep, tick.Cursor
	}
	return BlockIndexReconcileInterval, tick.Cursor
}

// owedDocument is a document owed a block index as it was listed: its latest version and the
// marked_at of its pending settlement, zero when it owes none. A document of an issue room - one
// whose document_rooms row names the room, or the room's tree artifact itself - is listed with
// room, the tree artifact, and with the room's: version is then the sum of the latest version
// numbers of every artifact of the room, which any version written of any of them moves, and
// markedAt the room's pending settlement's (keyed by the tree artifact), which every append to the
// room moves. unreadable is set on a room liveAhead remembers because a fragment of it did not
// read back (indexOwedRoom). decisions is true for an issue's decisions query node
// (refs.DecisionsQuerySQL), which holds no fragment of a room and no block (R15), so
// indexOwedDocument reads no room and indexes it handed no text.
type owedDocument struct {
	id         string
	version    int
	markedAt   time.Time
	room       string
	unreadable bool
	decisions  bool
}

// resident is the room a server holds document in: its issue room, or the document itself.
func (document owedDocument) resident() string {
	if document.room != "" {
		return document.room
	}
	return document.id
}

// owedOutcome is what one owed document's index did (indexOwed): indexed, left owed with its
// live text ahead of its stored versions, left owed because a fragment of its issue room did not
// read back, or none of those for the reason err gives or for no reason of its own (another
// writer indexed it, or it holds no version).
type owedOutcome struct {
	indexed, ahead, unreadable bool
	err                        error
}

// liveAheadDocuments is, by id, each owed document a tick found live ahead (indexOwedDocument), as
// it was listed then. Every update appended to a document's store moves its pending settlement's
// marked_at (markSettlementPending), and every version write moves its version, so a document
// listed again with both unchanged still holds the stored live text a tick found ahead: it is
// counted live ahead without being read or locked. Its issue's status takes no part: a closed
// issue's document live ahead is left owed for the settlement that writes its next version, as
// an open one's is, so a close or a reopen that moves neither changes nothing remembered here.
// Only the store's text is remembered: a room this server holds can be ahead of the store (an
// edit not yet appended) or behind it (another process's append it has not yet been evicted
// for), so a document is neither remembered nor taken from here while one is resident. An
// artifact of an issue room is remembered by the room's version and settlement mark, and so is
// one whose room a fragment of did not read back (owedDocument.unreadable), whatever its
// settlement mark: it is read again once a write moves either.
type liveAheadDocuments map[string]owedDocument

// ReconcileBlockIndex runs one tick: first the repair of the versions whose version_renders rows a
// pass marked tree_mismatch before the tick began (repairTreeMismatches, R15), then the read of the
// events after its build's cursor, which
// marks owed every document they name that this build's store has not stated at its version and
// gives every comment they name whose row names no node its node (readEventsAfterCursor), then,
// while this build owes it, the object pass: the nodes of the issues, documents, asks and messages
// whose rows another build wrote with none, and the gone statement of each document node another
// build left behind no row (writeObjectNodesAnotherBuildWrote), which reads those four tables whole
// where every other read of the tick reads only what is owed; then the drain of up to a batch of
// the marks of queries owed a version that a request left (drainOwedQueryMarks); then one batch of
// the documents owed a block index past
// cursors.Documents (a document id; empty is the start), one batch of the items owed an index
// with the block grammar, oldest first (refs.ReindexOwed), one batch of the comments, messages
// and asks owed a block index of their own blocks, oldest owed_at first
// (refs.IndexOwedItemBlocks), one batch of the sources owed an index with the whole grammar,
// oldest first (reindexOwedText), each in its own transaction and each under the item's owner
// lock, taken before it reads the item, so it waits for a write of that item rather than
// writing over the one that write planned - a direct message has no owner and needs none - and
// then one batch of the owners whose embedding windows do not stand for their blocks
// (windowDrift) and one slice of the window owners whose whole-text embeddings rows the windows
// have replaced (retireWholeText). The item blocks run after the block grammar's items, so an
// item that batch indexed, blocks included, is no longer owed, and before the windows, so an
// item either batch indexed - which leaves its windows to this arm (refs.ItemIndexing.Queue) -
// is windowed by this tick rather than the next; the whole grammar's sources run after them, so
// an item it would index the blocks of again is indexed once by each queue that holds it rather
// than twice by one. A document that batch owes an index is indexed by the documents batch,
// which owns that index, in this tick when it was already owed and in the next otherwise. The
// documents are listed by keyset, so a document it skips never holds back the ones past it: the
// tick's Cursor moves past every document its batch lists, indexed or not, and goes back to the
// start once a batch lists fewer than a full one, and the window and retirement arms walk their
// owners the same way. A document, item or source it cannot index for a reason of its own is
// logged and left owed, so one never holds back the rest of its batch. liveAhead is the
// documents the calling loop's earlier ticks found live ahead, which this tick counts again
// without reading and keeps up to date.
func (s *Service) ReconcileBlockIndex(ctx context.Context, cursors BlockIndexCursors, liveAhead liveAheadDocuments) (BlockIndexTick, error) {
	// A caller outside this package can only pass nil, since liveAheadDocuments is unexported:
	// the api package's tests and cmd/dispatch's memory harness, which
	// drains what is owed before it measures a server, both do. The map is a cache the calling
	// loop keeps across its own ticks, so a throwaway one costs no more than reading again the
	// documents this one tick finds live ahead.
	if liveAhead == nil {
		liveAhead = liveAheadDocuments{}
	}
	after := cursors.Documents
	tick := BlockIndexTick{Cursor: BlockIndexCursors{Documents: after}}
	// The repair runs first, over the rows marked when the tick begins, so a mark this tick's own
	// passes set is repaired at the next tick at the earliest, once its room has had a tick to show
	// an update in flight. A repair that fails is logged and holds back nothing else of the tick:
	// every mark it did not repair stays for the next.
	if err := s.repairTreeMismatches(ctx); err != nil {
		if ctx.Err() != nil {
			return tick, ctx.Err()
		}
		slog.Error("dispatch: block index could not repair the versions marked tree_mismatch", "error", err)
	}
	// The read of the events runs next, so a document it marks is listed by this tick's batch
	// below and drained by it when the batch has room. A read that fails is logged and holds back
	// nothing else of the tick: the cursor stays where it was, and the next tick reads the same
	// events again. The object pass runs after it, so the node of a document the read marks is
	// there before the documents batch states its blocks; one that fails, or stops with work left,
	// is logged and leaves this build's debt, so the next tick runs it again.
	eventsFull, debt, err := s.readEventsAfterCursor(ctx, &tick)
	switch {
	case ctx.Err() != nil:
		return tick, ctx.Err()
	case err != nil:
		slog.Error("dispatch: block index could not read the events after its build's cursor", "error", err)
	}
	objectsLeft, err := s.writeObjectNodesAnotherBuildWrote(ctx, &tick, debt)
	switch {
	case ctx.Err() != nil:
		return tick, ctx.Err()
	case err != nil:
		slog.Error("dispatch: block index could not write the nodes of the rows another build wrote", "error", err)
	}
	eventsFull = eventsFull || objectsLeft
	// The marks of queries owed a version that a request's write left and its own drain did not
	// clear run next, each in a transaction of its own under its issue's owner lock
	// (drainOwedQueryMarks), so a version one writes whose block index it could not stand is
	// listed by this tick's documents batch below. A pass that fails is logged and holds back
	// nothing else of the tick: every mark stays for the next tick.
	queriesFull, err := s.drainOwedQueryMarks(ctx, &tick)
	switch {
	case ctx.Err() != nil:
		return tick, ctx.Err()
	case err != nil:
		slog.Error("dispatch: block index could not drain the queries the issues owe", "error", err)
	}
	eventsFull = eventsFull || queriesFull
	owed, err := s.owedDocuments(ctx, after)
	if err != nil {
		return tick, err
	}
	if s.afterOwedDocumentsListed != nil {
		if err := s.afterOwedDocumentsListed(owed); err != nil {
			return tick, err
		}
	}
	documentsFull := len(owed) == blockIndexDocumentBatch
	liveAhead.forgetUnlisted(after, owed, documentsFull)
	// An issue room is reconciled whole, once a tick, whichever of its artifacts the batch lists
	// (indexOwedRoom): rooms holds how each room this tick read fared, and a room is counted once.
	rooms := map[string]owedOutcome{}
	counted := map[string]struct{}{}
	counts := func(document owedDocument) bool {
		if document.room == "" {
			return true
		}
		_, done := counted[document.room]
		counted[document.room] = struct{}{}
		return !done
	}
	for _, document := range owed {
		tick.Cursor.Documents = document.id
		if remembered, still := s.stillLiveAhead(liveAhead, document); still {
			switch {
			case !counts(document):
			case remembered.unreadable:
				tick.Unreadable++
			default:
				tick.LiveAhead++
			}
			continue
		}
		delete(liveAhead, document.id)
		cold := s.srv.GetDoc(document.resident()) == nil
		outcome, read := rooms[document.room]
		if !read || document.room == "" {
			outcome = s.indexOwed(ctx, document)
			if document.room != "" {
				rooms[document.room] = outcome
			}
		}
		switch err := outcome.err; {
		case ctx.Err() != nil:
			return tick, ctx.Err()
		case errors.Is(err, ErrDocNewerContent):
			// Another build wrote an element this build does not know, during a release; a build
			// that reads it indexes the document, which stays owed until then.
			slog.Warn("dispatch: block index skipped a document holding an element this build cannot read",
				"artifact", document.id, "error", err)
		case err != nil:
			slog.Error("dispatch: block index skipped a document it could not index",
				"artifact", document.id, "error", err)
		case outcome.ahead:
			if counts(document) {
				tick.LiveAhead++
			}
			if cold && !document.markedAt.IsZero() && s.srv.GetDoc(document.resident()) == nil {
				liveAhead[document.id] = document
			}
		case outcome.unreadable:
			if counts(document) {
				tick.Unreadable++
			}
			if cold && s.srv.GetDoc(document.resident()) == nil {
				document.unreadable = true
				liveAhead[document.id] = document
			}
		case outcome.indexed:
			if counts(document) {
				tick.Documents++
			}
		}
	}
	if !documentsFull {
		tick.Cursor.Documents = ""
	}

	items, err := refs.ListOwed(ctx, s.store.Pool, refs.BlockGrammarOwed, blockIndexItemBatch)
	if err != nil {
		return tick, err
	}
	for _, item := range items {
		indexed, err := refs.ReindexOwed(ctx, s.store.Pool, item, s.serverURL)
		if err != nil {
			if ctx.Err() != nil {
				return tick, ctx.Err()
			}
			slog.Error("dispatch: block index skipped an item it could not index",
				"kind", item.Kind, "id", item.ID, "error", err)
			continue
		}
		if indexed {
			tick.Items++
		}
	}

	itemBlocks, err := refs.OwedItemBlocks(ctx, s.store.Pool, blockIndexItemBatch)
	if err != nil {
		return tick, err
	}
	for _, item := range itemBlocks {
		indexed, err := refs.IndexOwedItemBlocks(ctx, s.store.Pool, item, s.serverURL)
		if err != nil {
			if ctx.Err() != nil {
				return tick, ctx.Err()
			}
			slog.Error("dispatch: block index skipped the blocks of an item it could not index",
				"kind", item.Kind, "id", item.ID, "error", err)
			continue
		}
		if indexed {
			tick.ItemBlocks++
		}
	}

	texts, err := refs.ListOwed(ctx, s.store.Pool, refs.RefsGrammarOwed, blockIndexItemBatch)
	if err != nil {
		return tick, err
	}
	for _, source := range texts {
		indexed, err := s.reindexOwedText(ctx, source)
		if err != nil {
			if ctx.Err() != nil {
				return tick, ctx.Err()
			}
			slog.Error("dispatch: block index skipped text it could not index with the whole grammar",
				"kind", source.Kind, "id", source.ID, "error", err)
			continue
		}
		if indexed {
			tick.Texts++
		}
	}

	windowsFull, err := s.windowDrift(ctx, cursors.Windows, &tick)
	if err != nil {
		return tick, err
	}
	if err := s.retireWholeText(ctx, cursors.Retire, &tick); err != nil {
		return tick, err
	}
	// The head projection: up to a batch of issues no mark names, each filled from the store and
	// marked in a transaction of its own under its owner lock and its head fill lock.
	filled, headsFull, err := graphcensus.FillIssueHeads(ctx, s.store, graphcensus.HeadFillBatch)
	tick.HeadsFilled = filled
	if err != nil {
		return tick, err
	}
	tick.Full = documentsFull || len(items) == blockIndexItemBatch ||
		len(itemBlocks) == blockIndexItemBatch || len(texts) == blockIndexItemBatch ||
		windowsFull || tick.Retired > 0 || eventsFull || headsFull

	// What is owed after the tick, and how old the oldest document's debt is, in the one
	// statement: a tick pays one round trip for the whole backlog. The owed documents come
	// through the two partial indexes owedDocuments lists them by, each row counted once as the
	// listing lists it, and each costs one probe into its own versions by
	// (artifact_id, number), the same read the owed listing makes for the version it indexes.
	// Both count only what this build's own marking rule lists (refs.OwedMarkListed), so a mark of
	// a higher rule that this build leaves to that rule's build, as a build of rule 1 does after a
	// rollback from rule 2, is no backlog of this build's that only grows.
	// `envoy-dispatch census --store --sizing` reads the same two figures from the database
	// alone (graphcensus's blockIndexBacklogQuery), so an operator who cannot read the server's
	// log can still read the backlog; both derive the debts from one text
	// (refs.OwedBlockIndexDebts).
	var oldestOwed *time.Time
	if err := s.store.Pool.QueryRow(ctx, `
		select (select count(*) from document_block_versions where version is null) +
		       (select count(*) from document_block_versions b
		        where b.version is not null and `+refs.OwedMarkListed("b", "$1::integer")+`),
		       (select min(owed.owed_at) from (`+refs.OwedBlockIndexDebts+`
		       ) owed),
		       (select count(*) from block_grammar_owed),
		       (select count(*) from refs_grammar_owed),
		       (select count(*) from comment_block_state where owed_at is not null) +
		       (select count(*) from message_block_state where owed_at is not null) +
		       (select count(*) from ask_block_state where owed_at is not null)
	`, s.markingRule).Scan(&tick.OwedDocuments, &oldestOwed, &tick.OwedItems, &tick.OwedTexts, &tick.OwedItemBlocks); err != nil {
		return tick, fmt.Errorf("count what is owed a block index: %w", err)
	}
	if oldestOwed != nil {
		tick.OldestOwedDocumentAt = *oldestOwed
	}
	return tick, nil
}

// windowOwner is one kind of owner the window arm walks: name keys its cursor and names it in a
// log line, batch is the most of one tick's shared batch its kind may take, list lists at most
// limit owners of the kind whose windows do not stand for their blocks, in id order past after,
// and window writes one owner's windows and reports whether it wrote them. One loop serves them
// all, so the reset, the fullness rule and the cursor are written once and every key is this
// name.
type windowOwner struct {
	name   string
	batch  int
	list   func(ctx context.Context, after string, limit int) ([]string, error)
	window func(ctx context.Context, id string) (bool, error)
}

// windowItemKinds is the item kinds whose blocks carry embedding windows, in the order the
// window arm serves them after the documents.
var windowItemKinds = []refs.ItemKind{refs.ItemComment, refs.ItemMessage, refs.ItemAsk}

// windowOwners is the owners the window arm serves, in order: the documents, then the comments,
// the messages and the asks. The documents are held to blockIndexDocumentBatch of the shared
// batch, the bound the owed-document arm reads under, because a document's windows cost the
// same read of the document that arm makes; an item's windows are one transaction of its own,
// so its kind may take the whole batch.
func (s *Service) windowOwners() []windowOwner {
	owners := []windowOwner{{
		name:  "document",
		batch: blockIndexDocumentBatch,
		list: func(ctx context.Context, after string, limit int) ([]string, error) {
			return refs.DocumentsOwedWindows(ctx, s.store.Pool, after, limit)
		},
		window: s.windowDocument,
	}}
	for _, kind := range windowItemKinds {
		owners = append(owners, windowOwner{
			name:  string(kind),
			batch: blockIndexWindowBatch,
			list: func(ctx context.Context, after string, limit int) ([]string, error) {
				return refs.ItemsOwedWindows(ctx, s.store.Pool, kind, after, limit)
			},
			window: func(ctx context.Context, id string) (bool, error) {
				return s.windowItem(ctx, kind, id)
			},
		})
	}
	return owners
}

// windowDrift gives windows to at most blockIndexWindowBatch owners whose windows do not stand
// for their blocks. For a comment, a message and an ask that is every one whose text was
// written since this arm last windowed it: an item's write records its blocks and leaves their
// windows here (refs.queueRecordIndexed, INFRA-988), so this arm writes every window an item
// holds, a tick behind the write. A document is the same since its version write stopped
// windowing its own blocks (refs.BlockIndexing.Queue): every document whose version write
// moved its blocks is listed here, beside one written before meaning search moved to windows,
// one whose write wrote its blocks and failed before its windows, and one a build without this
// change re-indexed while the deploy rolled, which left the windows of its old text behind
// (refs.DocumentsOwedWindows, refs.ItemsOwedWindows). It counts them in tick.Windows, leaves
// each kind's cursor in tick.Cursor.Windows and reports whether a batch was full.
//
// Each owner is windowed from the text its blocks were split from, and nothing else is
// written: a document is read as the server serves it and windowed under its owner's advisory
// lock (windowDocument, lockArtifactOwner), an item is read as the blocks routes read it and
// windowed under its own owner's (windowItem, store.LockOwner). Either way the write stamps
// the state the owner's blocks are in and clears the marker this arm reads in the same
// statement - an item's in comment_block_state, message_block_state or ask_block_state, a
// document's in document_block_versions (refs.WriteItemWindows, refs.WriteDocumentWindows) -
// so an owner this arm has served is no longer listed.
//
// An owner the index holds at no block is served here too, and its windows are none: a
// document whose markdown is empty, as the spec.md of an issue created with no spec is, a
// comment carrying only a suggestion, and an owner whose last block a write removed, which
// still holds the windows of the text it had. The write below is what deletes those
// (refs.writeWindows' `dropped`), and it clears the marker in the same statement, so such an
// owner is served once rather than left marked for ever
// (refs.DocumentsOwedWindows, refs.ItemsOwedWindows, INFRA-988).
func (s *Service) windowDrift(ctx context.Context, cursors map[string]string, tick *BlockIndexTick) (bool, error) {
	owners := s.windowOwners()
	next := make(map[string]string, len(owners))
	tick.Cursor.Windows = next
	budget, full := blockIndexWindowBatch, false
	for _, owner := range owners {
		if budget <= 0 {
			break
		}
		asked := min(budget, owner.batch)
		ids, err := owner.list(ctx, cursors[owner.name], asked)
		if err != nil {
			return false, err
		}
		for _, id := range ids {
			windowed, err := owner.window(ctx, id)
			switch {
			case ctx.Err() != nil:
				return false, ctx.Err()
			case err != nil:
				slog.Error("dispatch: block index skipped an owner it could not window",
					"owner", owner.name, "id", id, "error", err)
			case windowed:
				tick.Windows++
			}
			next[owner.name] = id
		}
		budget -= len(ids)
		full = full || len(ids) == asked
		if len(ids) < asked {
			next[owner.name] = ""
		}
	}
	return full, nil
}

// windowItem writes the windows of one comment, message or ask whose windows do not stand for
// its blocks, which is every item written since this arm last windowed it, in a transaction of
// its own: it takes the item's owner lock, the advisory lock every writer of the item takes,
// reads the text its blocks were split from under it and writes the windows of those blocks
// (refs.WriteItemWindows, store.LockOwner). It writes no block row and no reference: the index
// of this item's blocks stands, and only its windows are missing. An item whose stored text no
// longer splits into the blocks the index holds is marked owed a block index instead, which
// the transaction commits for the owed arm to act on.
func (s *Service) windowItem(ctx context.Context, kind refs.ItemKind, id string) (bool, error) {
	tx, err := s.store.Pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	windowed, err := refs.WriteItemWindows(ctx, tx, kind, id)
	if err != nil {
		return false, err
	}
	return windowed, tx.Commit(ctx)
}

// windowDocument writes the windows of one document whose windows do not stand for its blocks,
// in a transaction of its own: it reads the document the server serves (TextWithBlocks) with no
// transaction held, then, under the document owner's advisory lock, which every version write
// takes first, writes the windows of the stored latest version's blocks (lockArtifactOwner,
// refs.WriteDocumentWindows). It writes no block row and no reference: the index of this
// document's blocks stands, and only its windows are missing. Since a version write records its
// blocks and leaves their windows here (refs.BlockIndexing.Queue, INFRA-988), this is where
// every window a document holds is written, a tick behind the write.
//
// What is windowed is the text the version write indexed: the stored markdown of the latest
// version, whatever the document's settlement state and whatever its issue's status. Where the
// live text is that markdown byte for byte, the live render's blocks are it. Where it is not - an
// edit landed that no settlement has written a version for, on an open issue or a closed one -
// the version's own rendering is read back and its blocks take the ids the index holds for them,
// pass by pass (heldVersionBlocks): a window of a text no version holds would cover blocks the
// index does not hold, and a document under continuous editing would otherwise keep no window
// at all until its settlement wrote the next version. This arm is the one reader of that
// rendering: the owed-document arm leaves such a document owed, and its settlement indexes it at
// the version it writes (indexOwedDocument). A version this build cannot read back is windowed
// as whole text under no block, and a document the server cannot read at all is windowed the
// same way, as indexOwedDocument indexes one: its blocks came from its version's own markdown,
// which is what is embedded.
func (s *Service) windowDocument(ctx context.Context, artifactID string) (bool, error) {
	markdown, blocks, err := s.TextWithBlocks(ctx, artifactID)
	unreadable := false
	switch {
	case ctx.Err() != nil:
		return false, ctx.Err()
	case errors.Is(err, ErrDocumentUnloadable), errors.Is(err, ErrDocOutsideSchema):
		unreadable = true
	case err != nil:
		return false, err
	}
	tx, err := s.store.Pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	switch _, _, err := lockArtifactOwner(ctx, tx, artifactID); {
	case errors.Is(err, pgx.ErrNoRows):
		return false, nil
	case err != nil:
		return false, err
	}
	var stored string
	err = tx.QueryRow(ctx, `
		select v.markdown from artifact_versions v
		where v.artifact_id = $1 and v.markdown is not null
		order by v.number desc limit 1
	`, artifactID).Scan(&stored)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("re-read the latest version of %s for its windows: %w", artifactID, err)
	}
	switch {
	case unreadable || markdown == "" && len(blocks) == 0:
		markdown, blocks = stored, nil
	case markdown != stored:
		markdown, blocks, err = heldVersionBlocks(ctx, tx, artifactID, stored, markdown, blocks,
			"dispatch: block index cannot read back a document version; its windows cover no block")
		if err != nil {
			return false, err
		}
	}
	if err := refs.WriteDocumentWindows(ctx, tx, artifactID, markdown, blocks); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

// wholeTextOwner is one kind of whole-text embeddings row the windows replace, with the kind of
// window that stands for it and the prefix every owner key of that window kind carries before
// the owner's own id: a document's windows are under its artifact id, so its prefix is empty,
// and an item's are under "<kind>/<uuid>" (refs.ItemKind.WindowOwnerKey), so its prefix is
// "comment/", "message/" or "ask/".
type wholeTextOwner struct{ kind, windowKind, ownerPrefix string }

// wholeTextOwners is every kind of row the release before this one's meaning search reads,
// which this one replaces with windows. The issue rows are not among them: an issue's whole text
// is its title and the head of its spec, which no block indexer writes, and its trigger stands.
var wholeTextOwners = func() []wholeTextOwner {
	owners := []wholeTextOwner{{kind: "document", windowKind: refs.DocumentWindowKind}}
	for _, kind := range windowItemKinds {
		owners = append(owners, wholeTextOwner{
			kind:        string(kind),
			windowKind:  kind.WindowKind(),
			ownerPrefix: kind.WindowOwnerKey(""),
		})
	}
	return owners
}()

// owners lists, in the order embeddings_window_owner holds them, at most $2 of the owner keys
// of this kind's windows past the key $1 (empty: from the start), current or not, since retire
// tests currency. A pass walks the window owners rather than the whole-text rows behind them,
// and its limit bounds it, the cursor it leaves naming where the next pass starts; no choice
// of the planner's does.
//
// The window kind is written as a literal, as refs.windowKinds explains for every read and
// write of a window (migration 20261102233630): embeddings_window_owner is partial
// over the four window kinds, and the planner can prove a literal kind satisfies that
// predicate where it can prove nothing of a parameter, so only a literal reaches the owners
// through this index.
//
// It tests neither the window's vector nor its hashes. Those columns are not in
// embeddings_window_owner, so testing them here costs a heap read for every index entry, and
// the planner answered that cost by reading all 400,000 windows in a parallel sequential scan
// and sorting them: 195 ms a pass, and growing with the windows. The currency test sits in
// retire instead, where it is one indexed probe for each owner this read. Against 400,000
// current comment_block windows this reads 500 owners in 0.885 ms, through an index scan with
// no sort and no sequential scan.
func (owner wholeTextOwner) owners() string {
	return fmt.Sprintf(`
		select distinct split_part(w.id, '#', 1) from embeddings w
		where w.kind = '%[1]s' and split_part(w.id, '#', 1) > $1
		order by 1
		limit $2`, owner.windowKind)
}

// retire deletes the whole-text rows of this kind whose id $1 holds and whose owner already
// holds a window of the matching kind carrying a vector of its current text. The release before
// this one's meaning search reads exactly these rows, so an item leaves that search only once
// this one's windows can find it, and the two builds overlap for one deploy. The vector is read
// as well as the hash: the queue settles a window whose text has nothing to embed with no
// vector and its embedded_hash set (embedqueue's settledNothingToEmbed), and such a window
// stands for nothing in a meaning search, so a row behind it would be deleted with no vector
// left for the item at all.
//
// It has no limit of its own: one pass passes the ids of one slice of owners, so that slice
// bounds it. Every read is through an index - the rows by embeddings_pkey under
// id = any($1), each owner's windows by embeddings_window_owner under the owner key the prefix
// and the row's id glue back together, with the kinds as literals for the reason owners gives.
// Against 400,000 current comment_block windows and 2,000 whole-text comment rows none of
// which can retire, 500 ids cost 2.435 ms.
//
// Both ways of writing this as one statement are unbounded. Driving the delete from the window
// rows reads every current window of the kind: 999 ms a pass on that shape, with two external
// merge sorts holding 35 MB of temp files, and production always keeps a residue of whole-text
// rows that cannot retire - one whose window settled with nothing to embed, one whose window is
// pending or stale, one of an item refs.ItemsOwedWindows never lists - so that pass pays the
// cost again on every tick for ever. Driving it from the whole-text rows reads every row of the
// kind: one pass took 25 minutes against the 482,073 comments the load-test deployment then had
// unwindowed (INFRA-988). The walk over the owners is what bounds it.
func (owner wholeTextOwner) retire() string {
	return fmt.Sprintf(`
		with picked as (
			select e.kind, e.id from embeddings e
			where e.kind = '%[1]s' and e.id = any($1::text[])
			  and exists (
				select 1 from embeddings w
				where w.kind = '%[2]s' and split_part(w.id, '#', 1) = '%[3]s' || e.id
				  and w.embedding is not null
				  and w.embedded_hash is not null and w.embedded_hash = w.content_hash
			  )
			for update skip locked
		)
		delete from embeddings using picked
		where embeddings.kind = picked.kind and embeddings.id = picked.id`,
		owner.kind, owner.windowKind, owner.ownerPrefix)
}

// pass retires one slice of this kind's whole-text rows: it reads at most limit owner keys of
// the kind's windows past after (owners), turns each key into the id of the whole-text
// row behind it, deletes the rows of those ids whose window is current (retire), and reports
// how many it deleted and the key the next pass starts after.
//
// next is the last key read while the slice came back full, and the empty string otherwise, so
// a slice shorter than asked sends the walk back to the start of the kind's owners. That is
// windowDrift's rule, held to here for the same reason.
//
// It takes a pool rather than hanging off Service so a test can run the pass itself against a
// pool of its own.
func (owner wholeTextOwner) pass(ctx context.Context, pool *store.Pool, after string, limit int) (retired int, next string, err error) {
	rows, err := pool.Query(ctx, owner.owners(), after, limit)
	if err != nil {
		return 0, "", fmt.Errorf("list the owners of the %s windows: %w", owner.windowKind, err)
	}
	keys, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, "", fmt.Errorf("list the owners of the %s windows: %w", owner.windowKind, err)
	}
	if len(keys) == limit {
		next = keys[len(keys)-1]
	}
	ids := make([]string, 0, len(keys))
	for _, key := range keys {
		// A key of another shape belongs to no row of this kind. The delete's exists says the
		// same, since it compares a window's owner key against this prefix glued onto the row's
		// id, and it is the one that holds whatever this hands it, for any id and any key; this
		// line only keeps such a key out of the array.
		if id, ok := strings.CutPrefix(key, owner.ownerPrefix); ok {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return 0, next, nil
	}
	tag, err := pool.Exec(ctx, owner.retire(), ids)
	if err != nil {
		return 0, next, fmt.Errorf("retire the whole-text embeddings of the %ss behind their windows: %w", owner.kind, err)
	}
	return int(tag.RowsAffected()), next, nil
}

// retireWholeText walks one slice of each kind's window owners, the kinds in order, and deletes
// the whole-text rows the windows have replaced. The kinds share blockIndexRetireBatch as a
// budget of rows deleted, not of keys read as the window arm's batch is: each kind's pass reads
// at most what the kinds before it left of the budget, so one tick deletes at most
// blockIndexRetireBatch rows and reads at most len(wholeTextOwners) times as many owner keys.
// It adds what it deleted to tick.Retired and leaves each kind's cursor in tick.Cursor.Retire.
// Every kind's cursor goes there before the first pass, so a kind the budget leaves nothing for,
// and a kind after one whose pass returned an error, keep the cursor they had. That matters
// because this walk reads every window owner of a kind, not only the owners that owe a delete:
// a cursor dropped back to the empty string costs the kind a whole lap over its owners, one
// slice every BlockIndexReconcileInterval for as long as the slices retire nothing.
func (s *Service) retireWholeText(ctx context.Context, cursors map[string]string, tick *BlockIndexTick) error {
	next := make(map[string]string, len(wholeTextOwners))
	for _, owner := range wholeTextOwners {
		next[owner.kind] = cursors[owner.kind]
	}
	tick.Cursor.Retire = next
	budget := blockIndexRetireBatch
	for _, owner := range wholeTextOwners {
		if budget <= 0 {
			break
		}
		retired, after, err := owner.pass(ctx, s.store.Pool, cursors[owner.kind], budget)
		if err != nil {
			return err
		}
		tick.Retired += retired
		budget -= retired
		next[owner.kind] = after
	}
	return nil
}

// stillLiveAhead answers what liveAhead holds of document, and reports whether, as listed now, it
// holds document at the same version, room and settlement mark, with no room of this server
// resident for it. Whatever its issue's status, such a document still holds the stored live text
// a tick found ahead of its version, which leaves it owed (indexOwedDocument), and such an artifact
// of an issue room still holds the room a tick found ahead or a fragment of which did not read
// back (indexOwedRoom).
func (s *Service) stillLiveAhead(liveAhead liveAheadDocuments, document owedDocument) (owedDocument, bool) {
	found, ok := liveAhead[document.id]
	return found, ok && found.version == document.version && found.room == document.room &&
		found.markedAt.Equal(document.markedAt) && s.srv.GetDoc(document.resident()) == nil
}

// forgetUnlisted drops each document past after, up to the last of owed while the batch is full,
// that owed does not list: it is owed no block index any more.
func (liveAhead liveAheadDocuments) forgetUnlisted(after string, owed []owedDocument, full bool) {
	listed := make(map[string]struct{}, len(owed))
	for _, document := range owed {
		listed[document.id] = struct{}{}
	}
	for id := range liveAhead {
		_, found := listed[id]
		if !found && id > after && (!full || id <= owed[len(owed)-1].id) {
			delete(liveAhead, id)
		}
	}
}

// owedDocuments lists, in id order, at most blockIndexDocumentBatch documents owed a block index
// past after, each with its latest markdown version and its pending settlement's marked_at. It
// reads no issue: whether a document is indexed or left owed does not depend on its issue's
// status (indexOwedDocument). A document with no markdown version has nothing to index and is
// listed with version 0, which indexOwedDocument passes over. An artifact of an issue room - a
// document whose document_rooms row names the room, or the room's tree artifact, which its
// issue_trees row names - is listed with its room, the room's revision (roomRevisionSQL) for its
// version and the room's pending settlement's marked_at, since its mark reconciles the room whole
// (indexOwedRoom).
//
// A document is owed in either of two ways, and each is read through its own partial index in key
// order before the two are merged: its version is null (document_block_versions_owed), which every
// release reads and sets; or it holds this build's own mark, store_owed_at
// (document_block_versions_store_owed, migration 20261116221539), which the backfill sets and every
// debt this build owes beside the null version (oweDocumentIndex, refs.RebuildAll), and which each
// tick sets on a document an event after its build's cursor names whose blocks this build's store
// has not stated at its version (readEventsAfterCursor).
// The second is the one the release before this one cannot clear: it neither reads nor writes that
// column, so a document it indexes after either was set stays listed here until a pass of this
// release states its blocks and clears the mark (refs.IndexBlocks). A document holding both is
// listed by the first alone.
//
// The second arm lists a mark by the marking rule that set it (store_owed_rule,
// refs.OwedMarkListed): a mark of this build's rule or a lower one as owed, and a mark of a higher
// rule only where the store last stated the document below its version, which is work of this
// build's own there. A document a higher rule marked whose stand is at its version is that rule's
// to drain: this build's pass would lose the fence at that version (refs.BlockIndexing.queueStand)
// and list it again every tick. Where this build's pass indexes a document under a higher rule's
// mark, its stand states the version and leaves the mark, so the document is listed here no more
// and the higher rule's build still lists it.
func (s *Service) owedDocuments(ctx context.Context, after string) ([]owedDocument, error) {
	var cursor any
	if after != "" {
		cursor = after
	}
	rows, err := s.store.Pool.Query(ctx, `
		select b.artifact_id::text, coalesce(room.id::text, ''),
		       case when room.id is null
		            then coalesce((select max(v.number) from artifact_versions v
		                           where v.artifact_id = b.artifact_id and v.markdown is not null), 0)
		            else `+roomRevisionSQL("room.id")+` end,
		       (select p.marked_at from doc_settlements_pending p where p.artifact_id = coalesce(room.id, b.artifact_id)),
		       room.id is null and exists (select 1 from artifacts a where a.id = b.artifact_id and `+refs.DecisionsQuerySQL("a")+`)
		from (
			(select artifact_id from document_block_versions
			 where version is null and ($1::uuid is null or artifact_id > $1::uuid)
			 order by artifact_id limit $2)
			union all
			(select o.artifact_id from document_block_versions o
			 where o.version is not null and `+refs.OwedMarkListed("o", "$3::integer")+`
			   and ($1::uuid is null or o.artifact_id > $1::uuid)
			 order by o.artifact_id limit $2)
		) b
		cross join lateral (
			select coalesce(
				(select moved.room from document_rooms moved where moved.artifact_id = b.artifact_id),
				(select tree.artifact_id from issue_trees tree where tree.artifact_id = b.artifact_id)) as id
		) room
		order by b.artifact_id
		limit $2
	`, cursor, blockIndexDocumentBatch, s.markingRule)
	if err != nil {
		return nil, fmt.Errorf("list the documents owed a block index: %w", err)
	}
	defer rows.Close()
	owed := []owedDocument{}
	for rows.Next() {
		var document owedDocument
		var markedAt *time.Time
		if err := rows.Scan(&document.id, &document.room, &document.version, &markedAt, &document.decisions); err != nil {
			return nil, fmt.Errorf("scan a document owed a block index: %w", err)
		}
		if markedAt != nil {
			document.markedAt = *markedAt
		}
		owed = append(owed, document)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list the documents owed a block index: %w", err)
	}
	return owed, nil
}

// roomRevisionSQL is, as SQL over the uuid expression room, an issue room's tree artifact, the
// sum of the latest markdown version numbers of every artifact of the room: the tree and each
// document its document_rooms rows hold. A version written of any of them moves it, so the
// reconciliation tells a room it found live ahead or unreadable from one written since
// (liveAheadDocuments), and its pass from one another writer's version overtook (indexOwedRoom).
func roomRevisionSQL(room string) string {
	return `(select coalesce(sum(latest.number), 0) from (
			select (select max(v.number) from artifact_versions v
			        where v.artifact_id = held.id and v.markdown is not null) as number
			from (select ` + room + ` as id
			      union all select joined.artifact_id from document_rooms joined where joined.room = ` + room + `) held
		) latest)`
}

// reindexOwedText drains one row of the re-index queue (refs.RefsGrammarOwed), the text an older
// build indexed without the whole grammar, and reports whether it did: a comment, an ask or an
// issue message through the queue's own re-index (refs.ReindexOwed), a document by owing it a
// block index (drainOwedDocument), which is the whole of its re-index, and a `scope` row, which
// names a document whose question blocks moved, through a re-index of every source whose text
// belongs to that document (reindexScope). The last two name no text of their own, which is why
// refs.ReindexOwed answers an error for either rather than settling such a row. An `artifact` or
// `scope` row naming an issue's decisions query node (isDecisionsQuery) is settled here without
// owing a block index or walking a scope: the node holds no fragment of a room and no block (R15),
// no version of it is indexed over its stored text, and no source's text belongs to it, so a row
// already in the database carries no real debt. Each row is settled only while it still holds the
// enqueued_at it was listed with, so a write that lands meanwhile leaves it owed for the next tick,
// which reads its newer text.
func (s *Service) reindexOwedText(ctx context.Context, source refs.Owed) (bool, error) {
	switch source.Kind {
	case "comment", "ask", "message":
		return refs.ReindexOwed(ctx, s.store.Pool, source, s.serverURL)
	case "artifact", "scope":
		decisions, err := isDecisionsQuery(ctx, s.store.Pool, source.ID)
		if err != nil {
			return false, err
		}
		if decisions {
			return refs.SettleOwed(ctx, s.store.Pool, source)
		}
		if source.Kind == "artifact" {
			return s.drainOwedDocument(ctx, source)
		}
		if err := s.reindexScope(ctx, source.ID); err != nil {
			return false, err
		}
		return refs.SettleOwed(ctx, s.store.Pool, source)
	}
	return false, fmt.Errorf("re-index %s %s: not a kind the queue holds", source.Kind, source.ID)
}

// isDecisionsQuery reports whether artifactID names an issue's decisions query node
// (refs.DecisionsQuerySQL), false for any other artifact or one that is gone.
func isDecisionsQuery(ctx context.Context, q Queryer, artifactID string) (bool, error) {
	var decisions bool
	if err := q.QueryRow(ctx, `select exists (select 1 from artifacts a where a.id = $1::uuid and `+refs.DecisionsQuerySQL("a")+`)`,
		artifactID).Scan(&decisions); err != nil {
		return false, fmt.Errorf("read whether %s is a decisions query: %w", artifactID, err)
	}
	return decisions, nil
}

// oweDocumentIndex owes a document a block index, which is what a document's re-index with the
// whole grammar is: the batch of owed documents indexes its latest version whole
// (indexOwedDocument, indexRenderedVersion), so its own mentions, the mentions its blocks hold and
// the excerpt each shows are all read with this build's grammar. The drain owes the work there
// rather than indexing the document itself because that batch is the one writer of a document's
// index and holds the rules a document needs - its keyset cursor, the owner row it indexes under,
// and the live text it must not date to the version that text is ahead of - and because a
// document an older build's version write left owed is indexed by that batch in this same tick.
// A version write that leaves its blocks to that batch owes it the same way (indexDocument).
//
// It sets both marks a debt of this build carries: document_block_versions.version null, which
// every release reads and drains, and store_owed_at, this build's own (migration 20261116221539),
// which the release before this one neither reads nor clears. That release may take the
// block-index lease first and index the document, writing every row it adds with no node and
// stating nothing in the store, and only the second mark brings this build's pass to the document
// after it, to name those rows' nodes and state what they project (refs.PlanBlocks). A mark the
// document already holds keeps its time, so the debt stays dated from the write that first owed
// it. The mark is of rule, the marking rule of the build owing the debt (Service.markingRule),
// and a mark of a higher rule the document already holds keeps its rule
// (oweDocumentIndexStatement).
//
// A document that is gone owes no index: the row is written from the artifacts table, so nothing
// is inserted for one. exec is the pool for a caller that owes the index on its own
// (reindexScope, and drainFormsDocument, which indexes the document itself in the same pass), and
// the transaction that settles a queue row for the drain (drainOwedDocument), which must owe the
// index with that write or do neither. A version write queues the same statement in its write
// phase (queueDocumentIndexOwed).
func oweDocumentIndex(ctx context.Context, exec refs.Execer, artifactID string, rule int) error {
	if _, err := exec.Exec(ctx, oweDocumentIndexStatement, artifactID, rule); err != nil {
		return fmt.Errorf("owe document %s a block index: %w", artifactID, err)
	}
	return nil
}

// queueDocumentIndexOwed is oweDocumentIndex for a version write, which owes the index with its
// own writes (indexDocument): queued in its write phase (write), after the version's insert,
// whose artifacts row the statement reads where the transaction creates the document.
func queueDocumentIndexOwed(ctx context.Context, tx pgx.Tx, artifactID string, rule int) error {
	return writeExec(ctx, tx, fmt.Sprintf("owe document %s a block index", artifactID), oweDocumentIndexStatement, artifactID, rule)
}

// oweDocumentIndexStatement sets both marks of a debt (oweDocumentIndex) on document $1, at the
// marking rule $2: a null version, and this build's own mark, store_owed_at, with its rule in
// store_owed_rule. A mark the document already holds keeps its time and keeps the higher of its
// rule and $2, so a mark of a higher rule stays that rule's to drain; where no mark stands the row
// takes $2, whatever rule a mark cleared before left on it, since the rule is read only where a
// mark stands (refs.OwedRuleKept).
var oweDocumentIndexStatement = `
	insert into document_block_versions (artifact_id, store_owed_at, store_owed_rule)
	select id, now(), $2::integer from artifacts where id = $1::uuid
	on conflict (artifact_id) do update set version = null,
		store_owed_at = coalesce(document_block_versions.store_owed_at, excluded.store_owed_at),
		store_owed_rule = ` + refs.OwedRuleKept("document_block_versions", "excluded.store_owed_rule") + `
`

// drainOwedDocument drains one `artifact` row of the re-index queue by owing its document a block
// index (oweDocumentIndex), which is the whole of that document's re-index, and reports whether it
// drained the row. The row is settled first and only while it holds the enqueued_at it was listed
// with, in the transaction that owes the index, so a write that lands meanwhile leaves the row
// owed and owes nothing. A document that is gone has its row settled all the same.
func (s *Service) drainOwedDocument(ctx context.Context, source refs.Owed) (bool, error) {
	tx, err := s.store.Pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	settled, err := refs.SettleOwed(ctx, tx, source)
	if err != nil || !settled {
		return false, err
	}
	if err := oweDocumentIndex(ctx, tx, source.ID, s.markingRule); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

// reindexScope re-indexes every source whose text belongs to document doc (refs.ScopeSources) and
// writes a `#id`: what a question block's settlement, or a version that moved the document's
// question blocks, changed for text written before it, since a `#id` written before its question
// settles resolves to nothing at its own write and to that ask once the ask exists. A source whose
// text holds no such candidate is read and left alone, which is most of a document's scope, and a
// source that fails leaves the row owed, so the next tick re-reads the whole scope; every write it
// makes is the same write a second pass would make.
func (s *Service) reindexScope(ctx context.Context, doc string) error {
	sources, err := refs.ScopeSources(ctx, s.store.Pool, doc)
	if err != nil {
		return err
	}
	for _, source := range sources {
		if source.Kind != "artifact" {
			if _, err := refs.ReindexScopeSource(ctx, s.store.Pool, source.Kind, source.ID, s.serverURL); err != nil {
				return err
			}
			continue
		}
		// The document's own text, whose `#id`s name its own question blocks: owing it an index
		// costs a re-index of every block of its latest version, so its text is read first and
		// the document is left alone unless it writes a `#id` at all. A version write owes the
		// document's own row beside this scope's (migration 20261103205041), so what this covers
		// is a question that settles with no version written after it.
		body, _, found, err := refs.SourceText(ctx, s.store.Pool, source.Kind, source.ID)
		if err != nil {
			return err
		}
		if !found || !refs.HoldsQuestionCandidate(body, s.serverURL) {
			continue
		}
		if err := oweDocumentIndex(ctx, s.store.Pool, source.ID, s.markingRule); err != nil {
			return err
		}
	}
	return nil
}

// indexOwedDocument indexes one owed document at its latest version with the full version index
// (indexRenderedVersion): references, excerpts, blocks, block texts and the indexed version. It
// reads the document the server serves (TextWithBlocks) with no transaction held, then, under the
// document owner's lock, which every version write holds first, re-checks that the latest version
// is still the one listed and the document still owed, by either mark owedDocuments lists it by
// and with the same rule for a mark of a higher marking rule (refs.OwedMarkListed); otherwise
// another writer has indexed it or will, and it does nothing.
//
// The live text is indexed only where it is the latest version's text: when the document owes no
// settlement, or owes one but its live markdown equals the version's stored markdown byte for
// byte. A document whose live text is ahead of its latest version, and differs from it, is
// skipped and reported liveAhead, and stays owed, whatever its issue's status: indexing it at
// that version would date a later edit's blocks to it. Its settlement indexes it when it writes
// the next version, and a closed issue's document settles as an open one's does: the ask states
// the issue's close, its final sweep and the repair write into the live document, and an edit
// the close could not settle, are saved by that settlement (RunSettlementResumption takes such a
// document when no room does), so a block is first indexed at the first version that holds it.
//
// The live read can hold an update of the room this process holds that the store has not taken
// yet - one whose durable append is in flight, whose pending row it writes only with that append,
// or one the room committed and no drainer has classified - so after that read, and with no
// transaction held, the pass drains the room's update log and reads whether this process counts
// such an update or such an append (roomHoldsUnstoredUpdate); where it does, the document is live
// ahead as where its pending row stands, and a later tick reads it again. So a document whose
// live text differs from its latest version, with no pending row and no update in flight, holds
// a version whose writer wrote a tree that does not render as the markdown it stored: the pass
// states that tree at the version, as it always has, and marks the version's row tree_mismatch in
// place of its time, its seq and its digest (recordTreeMismatch, R15), which a later tick repairs
// (repairTreeMismatches).
//
// A pass that gives the version's row its time first - a deferred version's first pass - runs the
// carry of R14 to it once the row stands, in its own transaction (commitPass, carryFirstStated).
//
// A document the server cannot read (ErrDocumentUnloadable, ErrDocOutsideSchema) or that has no
// stored state has no block to show, so its latest version's markdown is indexed with no blocks.
//
// A pair the index inserts takes the version's created_at, and the source_seq of the event that
// recorded the version (stampVersionEvent), queued in the step that appends the pass's facts.
//
// The pass is the reconciliation's (indexRenderedVersion's tick): it runs only on a document owed
// by either mark, and wherever it wins it also writes the node of each version of the document
// that holds none, with no version of its own left out, since this tick writes none. So a version
// the release before wrote during a roll stands at this tick rather than at the next run of the
// backfill, whichever mark it left.
//
// The transaction reads, extracts and plans the whole index before its first write (queueWrites),
// so it holds a transaction id only for the index's statements and the commit. Its first
// statement has Postgres plan each of them at the tables' sizes as it runs
// (graphstore.PlanAtCurrentSizes). Its re-read of the owed row takes no row lock. Every writer of
// a revision of document_block_versions - a version's own index, the trigger a version insert
// fires, the backfill - holds the document owner's lock, taken above, so no pass states a version
// between this read and the fence. Four writers of the
// row take no owner lock, and none of them writes a revision: oweDocumentIndex, the re-index of a
// scope that calls it (reindexScope) and refs.RebuildAll set version to null beside this build's
// mark, store_owed_at with its rule in store_owed_rule; and the tick's marking
// (markVersionedDocuments) sets that mark alone. That is why the fence stays safe beside them: a
// null version admits every pass, and a mark bears only on a pass at the version the row already
// stands at, as this one may be, which the fence's own statement decides against the row as it
// stands when it runs (refs' queueStand). A pass the row then refuses fails there and commits
// nothing, and a mark of a higher rule written in between stays.
//
// A document that holds no markdown version (listed at version 0: an image, a file, an issue's
// tree before its first version) has nothing to read or index, and its mark would otherwise stand
// for ever, since the backfill marks every row no pass of this build's rule has stated. So once the
// re-read under the document owner's lock answers the document's row with no markdown version - a
// document that is gone answers no row - one statement clears its marks and states it: version at
// coalesce(version, 0), the version it leaves, as indexOwedRoom's clear sets it, store_owed_at
// cleared where its mark is of the Service's marking rule or a lower one, and stated_version at
// that version by that rule, so neither the backfill nor a tick marks it again. The statement tests
// again that no markdown version stands. A first markdown version that commits between the listing
// and the lock - the release before's, which states nothing in the store and never sets the mark
// again - is found by the re-read, and the document keeps its mark for the next tick
// (TestAVersionWrittenAfterTheTickListedItsDocumentStaysOwed). It indexes nothing and answers so.
//
// A pass that leaves the document owing nothing - its index stated, or, for a document that holds
// no markdown version, its marks cleared - appends one artifact.indexed on the document's issue
// naming it (queueIndexedEvent), as the last write of its transaction, and publishes it once the
// transaction commits. The re-read decides it: the pass clears the null version and a mark of the
// Service's marking rule or a lower one, and a mark of a higher rule stays for that rule's pass
// (refs' queueStand), so a document that keeps one owes its index still and appends nothing. A
// project document has no issue, and its pass appends nothing either.
//
// settle is the forms queue's row the caller drains with this index (drainFormsDocument), nil for
// the block index's own batch. It is read under the document owner's lock, after every check
// above, and the index runs for it only while it holds the enqueued_at it was listed with
// (refs.StillOwed): a row this index did not run for - a document another process indexed first,
// one whose live text is ahead, one whose latest version moved - is left owed, and so is one a
// write moved meanwhile, with nothing written for it. The index then settles every queue the
// document is in, after its own deletes from block_refs and from refs (refs.BlockIndexing.Queue,
// then the reconciliation of its references, which settles the forms queue:
// refs.Replacement.SettlingForms), each of which owes the forms queue the document it rewrites
// (migrations 20261117025906 and 20261117025907). It settles it only where its block pass won the
// revision fence (indexRenderedVersion): a pass that lost writes no block, and leaves the row owed
// for the next pass, as it leaves the block index's own mark. So a document rebuild-refs left
// owed is settled by this build's block index of it, which writes its blocks, and not before.
func (s *Service) indexOwedDocument(ctx context.Context, document owedDocument, settle *refs.Owed) (indexed, liveAhead bool, err error) {
	var markdown string
	var blocks []model.ArtifactBlock
	unreadable := false
	if document.version != 0 && !document.decisions {
		markdown, blocks, err = s.TextWithBlocks(ctx, document.id)
		switch {
		case ctx.Err() != nil:
			return false, false, ctx.Err()
		case errors.Is(err, ErrDocumentUnloadable), errors.Is(err, ErrDocOutsideSchema):
			slog.Warn("dispatch: block index cannot read the document; its latest version is indexed with no blocks",
				"artifact", document.id, "error", err)
			unreadable = true
		case err != nil:
			return false, false, err
		}
	}
	if !document.decisions && s.roomHoldsUnstoredUpdate(document.id) {
		return false, true, nil
	}

	tx, err := s.store.Pool.Begin(ctx)
	if err != nil {
		return false, false, err
	}
	defer tx.Rollback(ctx)
	if err := graphstore.PlanAtCurrentSizes(ctx, tx); err != nil {
		return false, false, err
	}
	queued, ledger := queueWrites(ctx, tx)
	owner, _, err := lockArtifactOwner(queued, tx, document.id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	// The document's row, with its newest markdown version, null where it holds none, whether it
	// owes its index, and whether the pass leaves it owing nothing (cleared): a document that is
	// gone, or that has no block-index row, answers no row at all.
	var newest *int
	var newestMarkdown *string
	var newestAt *time.Time
	var owed, cleared, decisions bool
	var markedAt *time.Time
	err = tx.QueryRow(queued, `
		select v.number, v.markdown, v.created_at,
		       b.version is null or `+refs.OwedMarkListed("b", "$2::integer")+`,
		       b.store_owed_at is null or b.store_owed_rule <= $2::integer,
		       (select p.marked_at from doc_settlements_pending p where p.artifact_id = b.artifact_id),
		       exists (select 1 from artifacts a where a.id = b.artifact_id and `+refs.DecisionsQuerySQL("a")+`)
		from document_block_versions b
		left join lateral (
			select number, markdown, created_at from artifact_versions
			where artifact_id = b.artifact_id and markdown is not null
			order by number desc limit 1
		) v on true
		where b.artifact_id = $1
	`, document.id, s.markingRule).Scan(&newest, &newestMarkdown, &newestAt, &owed, &cleared, &markedAt, &decisions)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, fmt.Errorf("re-read the block index owed by %s: %w", document.id, err)
	}
	if newest == nil {
		if document.version != 0 || !owed {
			return false, false, nil
		}
		if s.beforeNoVersionClear != nil {
			if err := s.beforeNoVersionClear(document.id); err != nil {
				return false, false, err
			}
		}
		if err := writeExec(queued, tx, fmt.Sprintf("clear the marks of %s, which holds no markdown version", document.id), `
			update document_block_versions b set version = coalesce(b.version, 0),
				store_owed_at = case when b.store_owed_rule <= $2 then null else b.store_owed_at end,
				stated_version = coalesce(b.version, 0), stated_rule = $2
			where b.artifact_id = $1::uuid and (b.version is null or b.store_owed_at is not null)
			  and not exists (select 1 from artifact_versions v where v.artifact_id = $1::uuid and v.markdown is not null)
		`, document.id, s.markingRule); err != nil {
			return false, false, err
		}
		if cleared {
			if err := s.queueIndexedEvent(queued, tx, owner.IssueKey, []string{document.id}); err != nil {
				return false, false, err
			}
		}
		if err := ledger.runQueue(ctx, nil); err != nil {
			return false, false, err
		}
		if err := tx.Commit(ctx); err != nil {
			return false, false, err
		}
		for _, event := range ledger.events {
			s.events.Publish(event)
		}
		return false, false, nil
	}
	version, stored, versionAt := *newest, *newestMarkdown, *newestAt
	if version != document.version || !owed {
		return false, false, nil
	}
	mismatched := false
	var tree *pmdoc.Node
	switch {
	case decisions:
		// An issue's decisions query node holds no fragment of a room and no block (R15): stand
		// its block index with no blocks and no text, as version 1's index does, rather than
		// indexing its stored rendering.
		markdown, blocks, tree = "", nil, &pmdoc.Node{Type: "doc"}
	case unreadable || markdown == "" && len(blocks) == 0:
		markdown, blocks = stored, nil
	case markdown != stored && markedAt != nil:
		return false, true, nil
	case markdown != stored:
		mismatched = true
	}
	if settle != nil {
		owes, err := refs.StillOwed(queued, tx, *settle)
		if err != nil || !owes {
			return false, false, err
		}
	}
	// The event that recorded this version is read before the index pass: it is the cause every
	// node and tuple of the pass carries, and on this path it already exists, while on the
	// request path it does not and the pass's facts travel to the caller instead
	// (graphstore.Uncaused). A version no event recorded leaves the sentinel, and the rows the
	// pass writes keep no source_seq either.
	eventID, err := versionEventID(queued, tx, owner, document.id, version)
	if err != nil {
		return false, false, err
	}
	_, stated, err := s.indexRenderedVersion(queued, tx, document.id, refs.OneVersion(document.id, version, versionAt), nil, true, markdown, blocks, tree, nil)
	if err != nil {
		return false, false, err
	}
	// The stamp goes in the step that appends the pass's facts under the same event: a block_refs
	// row's source_seq is the legacy_source_seq of the tuple it projects (graphstore.UncausedTuple's
	// LegacyFromCause), so the row learns it beside the store write that states it.
	if err := write(queued, tx, func(b *store.WriteBatch) error {
		graphstore.QueueCaused(b, stated, eventID)
		graphstore.QueueHeads(b, graphstore.HeldHeads(stated))
		stampVersionEvent(b, document.id, document.id, version, versionAt, eventID)
		return nil
	}); err != nil {
		return false, false, err
	}
	if mismatched {
		slog.Warn("dispatch: block index stated a document whose live tree does not render as its latest version's stored markdown; the version's row is marked tree_mismatch and keeps its stored bytes until a repair",
			"artifact", document.id, "version", version)
		recordTreeMismatch(queued, tx, refs.OneVersion(document.id, version, versionAt))
	}
	if cleared {
		if err := s.queueIndexedEvent(queued, tx, owner.IssueKey, []string{document.id}); err != nil {
			return false, false, err
		}
	}
	if err := s.commitPass(ctx, tx, ledger, func() error {
		if s.afterBlockIndexWrite != nil {
			return s.afterBlockIndexWrite(document.id)
		}
		return nil
	}); err != nil {
		return false, false, err
	}
	return true, false, nil
}

// restateDocumentVersion runs, in tx, the reconciliation's pass over one document that is its own
// room at version, a version marked tree_mismatch the repair restates (repairDocumentMismatch),
// from markdown, the version's stored markdown, its blocks and tree, the tree they were rendered
// from (indexRenderedVersion's tick), as indexOwedDocument runs it over a document's newest version:
// the event that recorded the version read before the pass, the pass's facts caused by it
// (causeRoomFacts, over the document as its one fragment) and the stamp of its links with it
// (stampVersionEvent).
func (s *Service) restateDocumentVersion(ctx context.Context, tx pgx.Tx, owner artifactOwner, artifactID string, version int, versionAt time.Time, markdown string, blocks []model.ArtifactBlock, tree *pmdoc.Node) error {
	eventID, err := versionEventID(ctx, tx, owner, artifactID, version)
	if err != nil {
		return err
	}
	_, stated, err := s.indexRenderedVersion(ctx, tx, artifactID, refs.OneVersion(artifactID, version, versionAt), nil, true, markdown, blocks, tree, nil)
	if err != nil {
		return err
	}
	if err := causeRoomFacts(ctx, tx, artifactID, map[string]graphstore.Uncaused{artifactID: stated}, map[string]int64{artifactID: eventID}, []string{artifactID}); err != nil {
		return err
	}
	return write(ctx, tx, func(b *store.WriteBatch) error {
		stampVersionEvent(b, artifactID, artifactID, version, versionAt, eventID)
		return nil
	})
}

// roomHoldsUnstoredUpdate reports whether room, as this process holds it, holds an update the
// store has not taken yet, which a reconciliation's pass reads after its live read and with no
// transaction held, before it reads the room's pending row under the owner lock (R15). It drains
// the room's update log first (observeRoomDoc), as a read of a room behind the stored head drains
// it (unstoredSnapshot): ygo appends each commit to that log under the document's lock, before any
// observer of the commit runs, so the drain classifies every update the live read can hold, though
// the room's own observer may still wait for the replica's lock. Then it reads whether this
// process's record of the room counts an update its persistence has not taken or a durable append
// that has not landed (hasPendingUpdates, hasDurableAppend): a classified update stays counted
// until its append lands or the room drops it, and the append commits the room's pending row in
// its own transaction (docs/append_writes.go), so a pass that reads the counts and then the row
// sees the one or the other. A room this process does not hold has nothing to drain and counts
// nothing: another process takes a room's updates in only from the stored head (roomCatchUps).
func (s *Service) roomHoldsUnstoredUpdate(room string) bool {
	if live := s.srv.GetDoc(room); live != nil {
		s.observeRoomDoc(room, live)
	}
	return s.hasPendingUpdates(room) || s.hasDurableAppend(room)
}

// versionEventID is the id of the event that recorded version: its artifact.version, or for
// version 1 the document's artifact.created, or the issue.created of the issue whose spec it
// seeded. graphstore.NoEvent, the sentinel, where no event recorded it - a version a build
// without the block index wrote and nothing else named.
//
// It is read before the index pass rather than after it, because what that pass states in the
// node and tuple store carries this event as its cause and no tuple may be updated once it is
// appended. The stamp it feeds still runs after the pass: it fills the source_seq of the rows
// the pass writes.
func versionEventID(ctx context.Context, tx pgx.Tx, owner artifactOwner, artifactID string, version int) (int64, error) {
	var eventID int64
	err := tx.QueryRow(ctx, `
		select id from events
		where (issue_key = $1 or artifact_id = $2::uuid)
		  and (type = 'artifact.version' and payload->>'artifact_id' = $3 and payload->'version'->>'number' = $4
		       or $5 and type = 'artifact.created' and payload->'artifact'->>'id' = $3
		       or $5 and type = 'issue.created' and issue_key = $1
		          and exists (select 1 from artifacts a where a.id = $2::uuid and a.is_primary))
		order by id desc
		limit 1
	`, owner.IssueKey, artifactID, artifactID, strconv.Itoa(version), version == 1).Scan(&eventID)
	if errors.Is(err, pgx.ErrNoRows) {
		return graphstore.NoEvent, nil
	}
	if err != nil {
		return graphstore.NoEvent, fmt.Errorf("read the event that recorded version %d of %s: %w", version, artifactID, err)
	}
	return eventID, nil
}

// stampVersionEvent queues on b the stamp that gives the edges a re-index of version of document
// inserted the source_seq of eventID, the event that recorded that version (versionEventID). A
// version write stamps its own edges once it appends its event (refs.QueueStamp); this is that
// stamp for a version a build without the block index wrote, and for each version an issue room's
// pass covers. It stamps only what the re-index inserted - the block pairs it created at this
// version, of the blocks of document (refs.BlockDocumentSQL: an issue room's rows are keyed by its
// tree artifact, artifactID, and carry each document's own version numbers), and the references of
// artifactID it dated versionAt - so an edge rebuild-refs inserted keeps no source_seq. With no
// such event the edges keep none and nothing is queued. A document that is its own room is its
// own artifactID. The event is read where it is called, or it is graphstore.LatestEvent, the event
// the write phase appends right before these stamps (store.LatestEventID): an issue room's
// settlement queues each document's stamp right after that document's version event
// (commitRoomVersions). b is the step that appends the pass's facts under the event
// (indexOwedDocument), or a write step of the caller's beside them (indexOwedRoom,
// commitRoomVersions).
func stampVersionEvent(b *store.WriteBatch, artifactID, document string, version int, versionAt time.Time, eventID int64) {
	if eventID == graphstore.NoEvent {
		return
	}
	source := strconv.FormatInt(eventID, 10)
	if eventID == graphstore.LatestEvent {
		source = store.LatestEventID
	}
	b.Queue(fmt.Sprintf("stamp the block mentions of version %d of %s", version, document), `
		update block_refs r set source_seq = `+source+`
		where r.artifact_id = $1 and r.created_version = $2 and r.source_seq is null
		  and ($1 = $3 or `+refs.BlockDocumentSQL("r")+` = $3::uuid)
	`, artifactID, version, document)
	b.Queue(fmt.Sprintf("stamp the references of version %d of %s", version, document), `
		update refs set source_seq = `+source+`
		where from_kind = 'artifact' and from_id = $1 and source_seq is null and created_at = $2
	`, artifactID, versionAt)
}

// BlockIndexActor is the writer the block-index reconciliation names on the artifact.indexed it
// appends (queueIndexedEvent): a system actor of its own, as QueryDrainActor is a drain's.
var BlockIndexActor = model.Actor{Kind: "system", ID: "block-index"}

// queueIndexedEvent queues in tx, a reconciliation's pass over documents of the issue issueKey
// (indexOwedDocument, indexOwedRoom), the append of one artifact.indexed on that issue naming
// documents, each document whose owed block index the pass clears, on the pass's own ledger, whose
// events the pass publishes once tx has committed (commitPass). It is the pass's last write, so no
// statement of the pass after it names the event its write appended last (store.LatestEventID) and
// finds this one; the carry commitPass runs after the write phase appends its own event before any
// statement of it names one. Every page holding a read of the issue that an owed index holds back
// - the coverage, the tree's reads, a named document's blocks - reads it again on this event
// (web's event-refresh), which reaches it through the event log and the event stream relay once
// tx commits, as every committed event does, from whichever process wrote it; a transaction that
// rolls back appends nothing. It is quiet (events.Quiet): it wakes nobody. A project document has
// no issue, and a pass that clears nothing names no document: neither queues anything.
func (s *Service) queueIndexedEvent(ctx context.Context, tx pgx.Tx, issueKey *string, documents []string) error {
	if issueKey == nil || len(documents) == 0 {
		return nil
	}
	sink := eventSink(ctx)
	event := model.Event{
		IssueKey: issueKey, Type: "artifact.indexed", Actor: BlockIndexActor,
		Payload: model.ArtifactIndexedEventPayload{ArtifactIDs: documents},
	}
	return write(ctx, tx, func(b *store.WriteBatch) error {
		return s.events.QueueAppend(b, event, sink)
	})
}

// indexOwed indexes one owed document (indexOwedDocument), or the issue room it is an artifact
// of, whole (indexOwedRoom).
func (s *Service) indexOwed(ctx context.Context, document owedDocument) owedOutcome {
	if document.room != "" {
		indexed, ahead, unreadable, err := s.indexOwedRoom(ctx, document)
		return owedOutcome{indexed: indexed, ahead: ahead, unreadable: unreadable, err: err}
	}
	indexed, ahead, err := s.indexOwedDocument(ctx, document, nil)
	return owedOutcome{indexed: indexed, ahead: ahead, err: err}
}

// indexOwedRoom reconciles the issue room document.room whole, on an owed mark of any of its
// artifacts - a document its document_rooms rows hold, or its tree artifact (T12a of
// the issue-tree plan): never one artifact alone, so no pass writes a document_blocks row
// keyed by a converted document, and the pass that decides which blocks are gone reads every
// fragment. It reads the room the server serves over every fragment of its layout (roomTree), with
// no transaction held, then, under the tree artifact's owner lock, which every version write of
// the room's documents holds first, re-checks that the room still holds the documents it read,
// at the revision listed (roomRevisionSQL), with an artifact still owed; otherwise another writer
// has indexed it or will, and it does nothing.
//
// Each fragment is read as today's path reads one document: its live text, here the room's tree
// parted into its fragments (roomLayout.parts), against the newest version stored of that
// fragment's document, each document keeping its own history, and a document holding no version
// yet - the tree artifact before its first, say - read as an empty fragment. The rule that keeps a
// later edit from being dated to an older version reads the room's pending settlement, keyed by
// the tree artifact: where the room owes one and any fragment's text differs from its document's
// version, the room is live ahead and stays owed, for the settlement that writes those versions.
// Where a fragment does not read back (ErrDocumentUnloadable, ErrDocOutsideSchema), the room is
// unreadable: it stays owed and nothing of it is indexed, rather than a fragment being indexed as
// holding no blocks, so no pass removes a block it could not read.
//
// After its live read, and with no transaction held, it drains the room's update log and reads
// whether this process counts an update of the room the store has not taken yet
// (roomHoldsUnstoredUpdate), as indexOwedDocument does: where it does, the room is live ahead and
// stays owed, since that update's pending row is written only with its append. So a fragment that
// reads otherwise than its document's newest version, with no pending row and no update in
// flight, is a version whose writer wrote a tree that does not render as the markdown it stored:
// the pass states the room as it reads it, and marks that version's row tree_mismatch in place of
// its time, its seq and its digest (recordTreeMismatch, R15), which a later tick repairs
// (repairTreeMismatches). A version whose row the pass gives its time first takes the carry of
// R14 in the pass's own transaction (commitPass).
//
// A room neither live ahead nor unreadable takes, in the transaction, the one pass under the tree
// artifact a settlement of the room runs (stateRoom: indexRoom, over the fragments' own renderings
// laid end to end in the room's order, fragmentsText), at the newest version of each fragment's
// document. Each document's facts are caused by the event that recorded its version
// (causeRoomFacts), each document's links are stamped with that event, and the owed mark of every
// artifact of the room, the tree's own included, is cleared: the pass's fence (refs.PlanBlocks)
// clears the marks of the documents it indexes, and a document holding no version is cleared there.
//
// The transaction reads, extracts and plans the whole pass before its first write (queueWrites),
// as indexOwedDocument's does, so it holds a transaction id only for the pass's statements and the
// commit. Its first statement has Postgres plan each of them at the tables' sizes as it runs
// (graphstore.PlanAtCurrentSizes). Its re-read of the owed rows takes no row lock: every writer of
// document_block_versions holds the owner's lock of the room's issue, taken above.
//
// The pass is the reconciliation's, as for one document: wherever it wins it also writes the node
// of each version of every document of the room it covers that holds none (indexRenderedVersion's
// tick, with no version of its own left out).
//
// A room no document has joined yet is the tree artifact alone, one fragment, a room that is its
// own document: its tree is indexed as one, through indexOwedDocument, at its newest version,
// which the room's revision then is (roomRevisionSQL).
//
// A pass appends one artifact.indexed on the room's issue naming each artifact of the room whose
// owed index it clears (queueIndexedEvent), as the last write of its transaction, and publishes it
// once the transaction commits. The re-read decides which: an artifact whose row is marked owes
// nothing once the pass has run unless it holds a mark of a higher rule than the Service's, which
// the pass leaves for that rule's pass (stateRoom), and a pass that clears no artifact's mark
// appends nothing.
func (s *Service) indexOwedRoom(ctx context.Context, document owedDocument) (indexed, liveAhead, unreadable bool, err error) {
	room := document.room
	read, err := readDocumentRoom(ctx, s.store.Pool, room)
	if err != nil {
		return false, false, false, err
	}
	if len(read.layout) == 0 {
		indexed, liveAhead, err := s.indexOwedDocument(ctx, owedDocument{id: room, version: document.version, markedAt: document.markedAt}, nil)
		return indexed, liveAhead, false, err
	}
	documents := read.layout.documents(room)
	tree, counts, err := s.roomTree(ctx, room, read.layout)
	switch {
	case ctx.Err() != nil:
		return false, false, false, ctx.Err()
	case errors.Is(err, ErrDocumentUnloadable), errors.Is(err, ErrDocOutsideSchema):
		slog.Warn("dispatch: block index cannot read a fragment of an issue room; the room stays owed and nothing of it is indexed",
			"room", room, "artifact", document.id, "error", err)
		return false, false, true, nil
	case err != nil:
		return false, false, false, err
	}
	parts := read.layout.parts(tree, counts)
	if parts == nil {
		return false, false, false, fmt.Errorf("the tree of room %s does not part into its %d fragments", room, len(documents))
	}
	if s.roomHoldsUnstoredUpdate(room) {
		return false, true, false, nil
	}

	tx, err := s.store.Pool.Begin(ctx)
	if err != nil {
		return false, false, false, err
	}
	defer tx.Rollback(ctx)
	if err := graphstore.PlanAtCurrentSizes(ctx, tx); err != nil {
		return false, false, false, err
	}
	queued, ledger := queueWrites(ctx, tx)
	owner, _, err := lockArtifactOwner(queued, tx, room)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false, false, nil
	}
	if err != nil {
		return false, false, false, err
	}
	var revision int
	var held, cleared []string
	var owed bool
	var markedAt *time.Time
	// The documents of the room are those its layout holds (roomLayoutSQL): a retired document's
	// fragment is none of them. Its mark counts beside theirs, and goes with theirs below, since
	// no pass indexes it again: the room's passes keep its blocks as its retirement left them
	// (refs.PlanBlocks). cleared is each marked artifact of the room the pass leaves owing nothing:
	// every one but one that holds a mark of a higher rule than $2, the Service's marking rule.
	if err := tx.QueryRow(queued, `
		with marked as (
			select b.artifact_id, b.version is null or b.store_owed_at is not null as owed,
			       b.store_owed_at is null or b.store_owed_rule <= $2::integer as cleared
			from document_block_versions b
			where b.artifact_id = $1::uuid
			   or b.artifact_id in (select d.artifact_id from document_rooms d where d.room = $1::uuid)
		)
		select `+roomRevisionSQL("$1::uuid")+`,
		       array(select d.artifact_id::text from document_rooms d join artifacts a on a.id = d.artifact_id
		             where d.room = $1::uuid and a.retired_at is null order by 1),
		       coalesce((select bool_or(marked.owed) from marked), false),
		       array(select marked.artifact_id::text from marked where marked.owed and marked.cleared order by 1),
		       (select p.marked_at from doc_settlements_pending p where p.artifact_id = $1::uuid)
	`, room, s.markingRule).Scan(&revision, &held, &owed, &cleared, &markedAt); err != nil {
		return false, false, false, fmt.Errorf("re-read the block index owed by room %s: %w", room, err)
	}
	if revision != document.version || !owed || !slices.Equal(held, slices.Sorted(slices.Values(documents[1:]))) {
		return false, false, false, nil
	}
	latest, err := readRoomVersions(queued, tx, documents)
	if err != nil {
		return false, false, false, err
	}
	versions := refs.Versions{}
	// mismatched is each fragment's version the pass states from a fragment that reads otherwise
	// than that version's stored markdown, with no pending row and no update in flight.
	mismatched := refs.Versions{}
	// markdowns is each fragment's own rendering, with its blocks' ranges in it (offsets): the text
	// the pass indexes, laid end to end (fragmentsText), in which one that renders as no text holds
	// no block.
	markdowns := make([]string, len(documents))
	offsets := make([][]pmdoc.BlockOffset, len(documents))
	for index, fragment := range documents {
		if latest[index].number == 0 {
			parts[index] = &pmdoc.Node{Type: parts[index].Type, Attrs: parts[index].Attrs}
			continue
		}
		versions[fragment] = refs.Version{Number: latest[index].number, At: latest[index].at}
		markdown, held, err := renderFragment(parts[index])
		if err != nil {
			slog.Warn("dispatch: block index cannot render a fragment of an issue room; the room stays owed and nothing of it is indexed",
				"room", room, "artifact", fragment, "error", err)
			return false, false, true, nil
		}
		if markdown != latest[index].markdown {
			if markedAt != nil {
				return false, true, false, nil
			}
			mismatched[fragment] = versions[fragment]
		}
		markdowns[index], offsets[index] = markdown, held
	}
	if err := s.stateRoom(queued, tx, owner, room, read.layout, documents, versions, parts, markdowns, offsets); err != nil {
		return false, false, false, err
	}
	if len(mismatched) > 0 {
		slog.Warn("dispatch: block index stated an issue room a fragment of which does not render as its document's latest version's stored markdown; that version's row is marked tree_mismatch and keeps its stored bytes until a repair",
			"room", room, "documents", slices.Sorted(maps.Keys(mismatched)))
		recordTreeMismatch(queued, tx, mismatched)
	}
	if err := s.queueIndexedEvent(queued, tx, owner.IssueKey, cleared); err != nil {
		return false, false, false, err
	}
	if err := s.commitPass(ctx, tx, ledger, func() error {
		if s.afterBlockIndexWrite != nil {
			return s.afterBlockIndexWrite(document.id)
		}
		return nil
	}); err != nil {
		return false, false, false, err
	}
	return true, false, false, nil
}

// stateRoom runs, in tx, the one pass under the issue room room's tree artifact a settlement of
// the room runs (indexRoom), the reconciliation's (indexOwedRoom) or a repair's restate of it
// (repairRoomMismatch): over parts, each fragment's part of the room in the order of
// layout.documents (documents), with each fragment's own rendering and its blocks' ranges in it
// (markdowns, offsets) laid end to end in the room's order (fragmentsText), at versions, the
// version each fragment's document is stated at, none for a fragment whose document holds none.
// Each document's facts are caused by the event that recorded its version (causeRoomFacts), and
// each document's links stamped with that event. It then clears the owed mark of every artifact of
// the room, the tree's own included: the pass's fence (refs.PlanBlocks) clears the marks of the
// documents it indexes, and a document holding no version is cleared here.
func (s *Service) stateRoom(ctx context.Context, tx pgx.Tx, owner artifactOwner, room string, layout roomLayout, documents []string, versions refs.Versions, parts []*pmdoc.Node, markdowns []string, offsets [][]pmdoc.BlockOffset) error {
	if len(versions) > 0 {
		joined, joinedCounts := joinFragments(parts)
		markdown, laid := fragmentsText(markdowns, offsets)
		blocks := offsetBlocks(laid, layout.blockDocuments(room, joined, joinedCounts))
		// The events that recorded each version are read before the pass, as for one document:
		// each document's is the cause of the facts the pass states of its blocks.
		events := make(map[string]int64, len(versions))
		for indexed, version := range versions {
			var err error
			if events[indexed], err = versionEventID(ctx, tx, owner, indexed, version.Number); err != nil {
				return err
			}
		}
		index, err := s.indexRoom(ctx, tx, room, versions, nil, true, markdown, blocks, joined, nil)
		if err != nil {
			return err
		}
		// Each document's part under its own stored event, in id order, the room's references with
		// the part of the document whose event the stamps below give their rows.
		facts := roomFactsByDocument(index, roomReferencesDocument(room, versions, events, slices.Sorted(maps.Keys(versions))))
		if err := causeRoomFacts(ctx, tx, room, facts, events, slices.Sorted(maps.Keys(facts))); err != nil {
			return err
		}
		if err := write(ctx, tx, func(b *store.WriteBatch) error {
			for _, indexed := range slices.Sorted(maps.Keys(versions)) {
				stampVersionEvent(b, room, indexed, versions[indexed].Number, versions[indexed].At, events[indexed])
			}
			return nil
		}); err != nil {
			return err
		}
	}
	// A document of the room holding no version is no part of the pass's fence, which cleared the
	// marks of every document it indexed and stated each, and neither is a retired document of the
	// room: each one's own mark goes here, so no mark of the room is left, and the same statement
	// states it at the version it leaves by the Service's marking rule (refs.BlockIndexMarkingRule),
	// so neither the backfill nor a tick marks it again; a mark of a higher rule stays for that rule's
	// pass, as the fence leaves it (refs' queueStand).
	return writeExec(ctx, tx, fmt.Sprintf("clear the marks of room %s", room), `
		update document_block_versions set version = coalesce(version, 0),
			store_owed_at = case when store_owed_rule <= $3 then null else store_owed_at end,
			stated_version = coalesce(version, 0), stated_rule = $3
		where (artifact_id = any($1::uuid[]) or artifact_id in (select d.artifact_id from document_rooms d where d.room = $2::uuid))
		  and (version is null or store_owed_at is not null)
	`, documents, room, s.markingRule)
}

// roomTree is the tree of the issue room room over every fragment of layout, joined as the room
// renders them, with how many top-level blocks each fragment holds (treeOfFragments): the tree
// this server holds (heldTree), else the stored room decoded (storedWithUpdates), an empty
// document where it stores none. A history that does not decode is ErrDocumentUnloadable and a
// fragment outside the schema ErrDocOutsideSchema, neither failing the room: the block-index
// reconciliation reads a room through here, and leaves such a room owed (indexOwedRoom).
func (s *Service) roomTree(ctx context.Context, room string, layout roomLayout) (*pmdoc.Node, []int, error) {
	fragments := layout.fragments()
	tree, counts, held, err := s.heldTree(ctx, room, fragments)
	if err != nil || held {
		return tree, counts, err
	}
	return s.storedWithUpdates(ctx, room, nil, fragments)
}

// indexRoom is indexRenderedVersion over an issue room, under its tree artifact room, at the
// version of each fragment's document (versions): the room's own pass, which a settlement of the
// room and its reconciliation run alike. written is the versions of those this transaction wrote,
// whose nodes its caller writes after the pass, and tick is set by the reconciliation alone
// (indexRenderedVersion). It answers what the pass stated, parted by the document each fact is of
// (roomFacts), and the reference changes the pass moved.
func (s *Service) indexRoom(ctx context.Context, tx pgx.Tx, room string, versions, written refs.Versions, tick bool, markdown string, blocks []model.ArtifactBlock, tree *pmdoc.Node, settled *askBlockSources) (roomIndex, error) {
	var refKey string
	var heldBlocks, heldDocuments []string
	if address, created := pendingArtifact(ctx, room); created {
		// A tree artifact this transaction creates (an issue born converted's) has no row yet,
		// and so no issue_room_blocks row names it: its key is its registered address's.
		refKey = address.RefKey
	} else if err := tx.QueryRow(ctx, `
		select a.ref_key,
		       array(select r.block_id from issue_room_blocks r where r.tree_artifact_id = a.id order by r.block_id),
		       array(select r.document_id::text from issue_room_blocks r where r.tree_artifact_id = a.id order by r.block_id)
		from artifacts a where a.id = $1`, room).Scan(&refKey, &heldBlocks, &heldDocuments); err != nil {
		return roomIndex{}, fmt.Errorf("read the documents of the blocks of room %s: %w", room, err)
	}
	documents := make(map[string]string, len(heldBlocks)+len(blocks))
	for index, block := range heldBlocks {
		documents[block] = heldDocuments[index]
	}
	for _, block := range blocks {
		documents[block.ID] = room
		if block.Document != "" {
			documents[block.ID] = block.Document
		}
	}
	changes, write, err := s.indexRenderedVersion(ctx, tx, room, versions, written, tick, markdown, blocks, tree, settled)
	if err != nil {
		return roomIndex{}, err
	}
	facts, references := roomFacts(write, room, refKey, documents)
	return roomIndex{changes: changes, facts: facts, references: references}, nil
}

// roomIndex is what one pass over an issue room did (indexRoom): the reference changes it moved,
// what it stated, by the document each fact is of, and apart from those the facts of the edges
// the room's own references moved (Replacement.ArtifactMentions), which no one document's version
// is the cause of (causeRoomFacts).
type roomIndex struct {
	changes    model.ReferenceChanges
	facts      map[string]graphstore.Uncaused
	references graphstore.Uncaused
}

// roomReferencesPart is the key roomFacts parts the facts of the room's own references under: no
// document id is empty.
const roomReferencesPart = ""

// roomFacts parts write, what one pass over the issue room room stated, by the document each fact
// is of (graphstore.Parted, which keeps each part's node write for its cause), so each is caused
// by its own document's version event (causeRoomFacts): a node of a block of the room (refKey's),
// and a tuple out of one, is of the document documents names for that block - the one whose
// fragment holds it, or, for a block the pass stated gone, the one its row held - and every other,
// an author's node, say, is of the tree artifact, room, whose own fragment belongs to no other
// document. The tree artifact's own node and a tuple out of it are the room's references, written
// under the tree artifact, and are answered apart.
func roomFacts(write graphstore.Uncaused, room, refKey string, documents map[string]string) (map[string]graphstore.Uncaused, graphstore.Uncaused) {
	parts := graphstore.Parted(write, func(kind, id string) string {
		switch {
		case kind == graphstore.KindArtifact && id == refKey:
			return roomReferencesPart
		case kind == refs.BlockNodeKind:
			if block, ok := strings.CutPrefix(id, refKey+"#"); ok {
				if document, held := documents[block]; held {
					return document
				}
			}
		}
		return room
	})
	references := parts[roomReferencesPart]
	delete(parts, roomReferencesPart)
	return parts, references
}

// roomFactsByDocument is what causeRoomFacts causes of each document of an issue room's pass:
// index.facts, with the facts of the room's own references joined into the part of the document
// whose event stamps their rows (reference, roomReferencesDocument), so each assertion takes its
// row's source_seq.
func roomFactsByDocument(index roomIndex, reference string) map[string]graphstore.Uncaused {
	facts := maps.Clone(index.facts)
	if facts == nil {
		facts = map[string]graphstore.Uncaused{}
	}
	if len(index.references.Nodes) > 0 || len(index.references.Tuples) > 0 {
		facts[reference] = graphstore.Joined(facts[reference], index.references)
	}
	return facts
}

// causeRoomFacts queues, as one step through write, what an issue room's pass stated of each of
// documents, in that order (facts, roomFactsByDocument), each document's part under that
// document's event (events, by document id: an event the store already holds, or
// graphstore.LatestEvent for one the write phase appends right before the step,
// commitRoomVersions); a part of a document no event is given for is appended at the sentinel, as
// a version no event recorded is. indexOwedRoom causes every document's part at once, in id order;
// an issue room's settlement causes each written document's right after its own version event.
func causeRoomFacts(ctx context.Context, tx pgx.Tx, room string, facts map[string]graphstore.Uncaused, events map[string]int64, documents []string) error {
	return write(ctx, tx, func(b *store.WriteBatch) error {
		for _, document := range documents {
			event, held := events[document]
			if !held {
				event = graphstore.NoEvent
			}
			graphstore.QueueCaused(b, facts[document], event)
		}
		for _, document := range documents {
			graphstore.QueueHeads(b, graphstore.HeldHeads(facts[document]))
		}
		return nil
	})
}

// roomReferencesDocument is the document whose event stampVersionEvent stamps the references a
// pass over the issue room room wrote with: the room's references are dated at the time of the
// tree artifact's own version, or of the newest version the pass covers where the tree holds none
// (indexRenderedVersion, ownVersion), and the stamps, queued in order, give them the event of the
// first document of order written at that time that an event recorded. A room with no such
// document is room itself, whose part no event is given for where it holds no version, as its
// references' rows keep no source_seq.
func roomReferencesDocument(room string, versions refs.Versions, events map[string]int64, order []string) string {
	own := ownVersion(room, versions)
	for _, document := range order {
		if version, covered := versions[document]; covered && version.At.Equal(own.At) && events[document] != graphstore.NoEvent {
			return document
		}
	}
	return room
}

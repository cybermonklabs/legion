package api

import (
	"context"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// A route that writes may run its transaction in two phases (INFRA-636): a read phase that takes
// its locks, reads and does the document work holding no transaction id, and a write phase,
// Ledger.Commit, that sends every write as one pipelined batch and commits. A route that takes
// those two phases joins a document ledger (Docs.Join), whatever document work it does: the
// document operations queue their writes on the ledger, and the route queues its own beside them
// with these helpers, at the point it makes each write (Ledger.Queue), so the batch carries the
// route's statements and the document operations' in the order the route made them, and the
// events they append keep that order. No statement's result is read in Go before a later
// statement is sent: a value a later statement needs - an appended event's id for its stamp - is
// named in that statement's SQL (store.LatestEventID), and a value the route needs after Commit -
// the appended event - is written through a pointer the statement's callback fills in.
//
// A failing statement or a callback's error stops the write phase and Ledger.Commit returns it
// unchanged, so the route answers it as it would a statement run at once. A write a transaction
// no ledger joined makes - one that writes outside a route's two phases, as the claim cycle's
// does - runs at once instead, through store.RunWrite.
//
// A read of this transaction must see the rows it has queued, which only a context carrying the
// ledger shows it (refs.Pending). Routes pass these helpers whatever context they hold - the
// request's, or the one Docs.Join returned - so each helper that reads derives its own from the
// ledger it is handed (docs.Ledger.ReadContext) instead of trusting the one it was passed: the
// contract is one thing, and no route can get it wrong by passing the context it has.

// queueExec queues one statement whose result nothing reads. Its failure comes back unwrapped, as
// tx.Exec's would.
func queueExec(ledger *docs.Ledger, sql string, args ...any) error {
	return ledger.Queue(func(b *store.WriteBatch) error {
		b.Queue("", sql, args...)
		return nil
	})
}

// queueEvent queues event's append (events.Broker.QueueAppend). The returned event is the appended
// one, with its id, sequence and time, once Commit has run the write phase: the route publishes it
// after Commit (appendedEvents).
func (s *server) queueEvent(ledger *docs.Ledger, event model.Event) (*model.Event, error) {
	return s.queueStampedEvent(ledger, event, "", "")
}

// queueStampedEvent is queueEvent followed, in the same step, by the stamp of fromKind/fromID's new
// reference edges with the event's id (refs.QueueStamp), which the stamp names as the id sequence
// left it: nothing comes between the event's insert and the stamp. An empty fromKind stamps
// nothing.
func (s *server) queueStampedEvent(ledger *docs.Ledger, event model.Event, fromKind, fromID string) (*model.Event, error) {
	return s.queueLoggedEvent(ledger, event, nil, fromKind, fromID)
}

// queueLoggedEvent is queueStampedEvent for an event whose payload names thread_logins, which the
// event's insert reads (events.Broker.QueueAppendWithLogins): the logins the route's earlier
// queued writes, and the user_threads triggers they fire, leave, as a read after those writes
// would see them. A nil logins names none.
func (s *server) queueLoggedEvent(ledger *docs.Ledger, event model.Event, logins *events.Logins, fromKind, fromID string) (*model.Event, error) {
	appended := new(model.Event)
	return appended, ledger.Queue(func(b *store.WriteBatch) error {
		if err := s.deps.Events.QueueAppendWithLogins(b, event, logins, func(written model.Event) { *appended = written }); err != nil {
			return err
		}
		if fromKind != "" {
			refs.QueueStamp(b, fromKind, fromID)
		}
		return nil
	})
}

// queueNewAskReferences reconciles, in the route's two phases, everything the ask whose insert
// this transaction has just queued writes: its question's references and its parts - the
// question itself, the options, and the answer it carries - each planned here and queued on the
// ledger, with the targets both moved counted together, so the ask.opened the route appends
// names them all. Every route that opens an ask this way - an issue's or a document's ask, an
// approval request - goes through this one call.
//
// The question and the parts are read through readers of their own, since the parts are planned
// by the ledger (queueAskParts), and both are given the same document, the one the queued row
// will belong to (refs.PendingAskDocument): no read of that row can answer it yet, and the two
// readers between them read it once, for the first of the two texts that names a question block
// and not at all for an ask whose texts name none.
func (s *server) queueNewAskReferences(ctx context.Context, tx pgx.Tx, ledger *docs.Ledger, ask model.Ask) (model.ReferenceChanges, error) {
	ctx = ledger.ReadContext(ctx)
	document := refs.PendingAskDocument(ask)
	located := refs.NewReaderInScope(ctx, tx, "ask", ask.ID, s.deps.ServerURL, document).Mentions(ask.Question)
	replacement, changes, err := planFoundReferences(ctx, tx, "ask", ask.ID, text.Refs(located), nil)
	if err != nil {
		return model.ReferenceChanges{}, err
	}
	if err := queueReplacement(ledger, replacement); err != nil {
		return model.ReferenceChanges{}, err
	}
	return s.queueAskParts(ctx, ledger, ask, document, ask.CreatedAt, changes)
}

// queueEditedReferences reconciles fromKind/fromID's references in the route's two phases for an
// edit of an ask's question or a comment's body from previous to body: the counted targets also
// name every target the source still cites when the edit moves the head of its text, which each
// of its edges shows (refs.EditedTargets). The source's row is stored - an edit changes a row an
// earlier transaction wrote - so the document its `#id`s resolve against is the one that row
// answers (refs.ScopeOf), read only if the body names a question block.
func (s *server) queueEditedReferences(ctx context.Context, tx pgx.Tx, ledger *docs.Ledger, fromKind, fromID, previous, body string) (model.ReferenceChanges, error) {
	ctx = ledger.ReadContext(ctx)
	found := text.Refs(refs.NewReader(ctx, tx, fromKind, fromID, s.deps.ServerURL).Mentions(body))
	replacement, changes, err := planFoundReferences(ctx, tx, fromKind, fromID, found, refs.EditedTargets(previous, body, found))
	if err != nil {
		return model.ReferenceChanges{}, err
	}
	return changes, queueReplacement(ledger, replacement)
}

// pendingItem is what the route writing a comment or an issue message hands over for an item
// whose insert this transaction has only queued: the two things a read of that row would
// answer and cannot yet. windowContext is what the windows of the item's blocks embed them
// under, read from the issue the item is written on (refs.IssueWindowContext); document is the
// one the item's `#id`s resolve against, read from the fields the queued row will hold and only
// for a body that names a question block (refs.PendingDocument).
type pendingItem struct {
	windowContext string
	document      *refs.PendingDocument
}

// queueItemReferences is refs.ReplaceCountedItem split in two, for a comment or an issue message
// this transaction writes at `at`: it plans the index of the item's blocks and the mentions each
// holds (refs.PlanItemIndex) and the reconciliation of the item's own references, resolves the
// counted targets both move, and queues the index's statements and the reconciliation's, in that
// order, in tx's write phase. A direct message (cites false) writes no mention, so its blocks
// hold none either: its blocks alone are indexed, and it moves nothing. The transaction has only
// queued the item's insert, so what no read of that row can answer - the context its blocks'
// windows embed them under and the document its `#id`s resolve against - is read from the fields
// source says the row will hold and handed over with it (pendingItem).
func (s *server) queueItemReferences(ctx context.Context, tx pgx.Tx, ledger *docs.Ledger, kind refs.ItemKind, itemID, body string, source refs.PendingSource, at time.Time, cites bool) (model.ReferenceChanges, error) {
	pending, err := s.pendingItemOf(ctx, tx, string(kind), source)
	if err != nil {
		return model.ReferenceChanges{}, err
	}
	changes, write, err := s.planItemWrite(ctx, tx, ledger, kind, itemID, body, pending, at, cites, nil, nil)
	if err != nil {
		return model.ReferenceChanges{}, err
	}
	return changes, ledger.Queue(write)
}

// pendingItemOf is what a route hands over for an item of kind kind whose insert it has queued,
// over the fields source says that row will hold. The window context is read here, since every
// item's blocks are indexed under it; the document is not read at all unless the item's body
// names a question block, so it is handed over unread (refs.PendingDocument).
func (s *server) pendingItemOf(ctx context.Context, tx pgx.Tx, kind string, source refs.PendingSource) (*pendingItem, error) {
	windowContext, err := refs.IssueWindowContext(ctx, tx, source.IssueKey)
	if err != nil {
		return nil, err
	}
	return &pendingItem{windowContext: windowContext, document: refs.NewPendingDocument(kind, source)}, nil
}

// queueEditedItemReferences is queueItemReferences for an edit of a comment from previous to
// body, written at `at`, with the block ids the editor carried (carried; nil: the ids follow the
// text, refs.IndexItem). The counted targets also name every target the comment still cites when
// the edit moves the head of its text, which each of its edges shows (refs.EditedTargets). A
// carried id the index refuses returns its *itemblocks.CarriedError.
func (s *server) queueEditedItemReferences(ctx context.Context, tx pgx.Tx, ledger *docs.Ledger, kind refs.ItemKind, itemID, previous, body string, at time.Time, carried *refs.CarriedEdit) (model.ReferenceChanges, error) {
	changes, write, err := s.planItemWrite(ctx, tx, ledger, kind, itemID, body, nil, at, true, carried, func(found []text.Ref) []refs.Target {
		return refs.EditedTargets(previous, body, found)
	})
	if err != nil {
		return model.ReferenceChanges{}, err
	}
	return changes, ledger.Queue(write)
}

// planItemWrite plans the item's index and, when the item cites its mentions, the reconciliation
// of its own references, counts what they and edited (the targets an edit moves although its
// edges stay; nil for a first write) move, and returns those counts with the step that writes
// both, for the route to queue where it makes the write: on its ledger (queueItemReferences), or
// on the batch of a transaction that runs its write phase itself (createStoredMessage).
//
// Every read it makes runs under the ledger the write queues on (docs.Ledger.ReadContext), so a
// mention of a row this transaction has queued resolves to that row (refs.Pending); the ledger
// is nil for a write that queues on no ledger and has queued no row (createStoredMessage).
//
// pending is what the route hands over for an item whose insert this transaction has queued, so
// that no read of the item's own row can answer it: the context its blocks' windows embed them
// under (refs.ItemIndex.WithWindowContext) and the document its `#id`s resolve against
// (refs.NewReaderInScope). It is nil for a write of an item already stored, which the index's
// read and ScopeOf answer both of for themselves.
func (s *server) planItemWrite(
	ctx context.Context,
	tx pgx.Tx,
	ledger *docs.Ledger,
	kind refs.ItemKind,
	itemID, body string,
	pending *pendingItem,
	at time.Time,
	cites bool,
	carried *refs.CarriedEdit,
	edited func(found []text.Ref) []refs.Target,
) (model.ReferenceChanges, docs.WriteStep, error) {
	ctx = ledger.ReadContext(ctx)
	index, err := refs.ReadItemIndex(ctx, tx, kind, itemID)
	if err != nil {
		return model.ReferenceChanges{}, nil, err
	}
	var document *refs.PendingDocument
	if pending != nil {
		index = index.WithWindowContext(pending.windowContext)
		document = pending.document
	}
	if edited != nil {
		// The update that sets edited_at is queued, so the read above does not show it.
		index = index.Edited()
	}
	var located []text.Located
	if cites {
		located = refs.NewReaderInScope(ctx, tx, string(kind), itemID, s.deps.ServerURL, document).Mentions(body)
	}
	indexing, err := refs.PlanItemIndex(index, kind, itemID, body, at, located, carried)
	if err != nil {
		return model.ReferenceChanges{}, nil, err
	}
	var replacement refs.Replacement
	var changes model.ReferenceChanges
	if cites {
		found := text.Refs(located)
		moved := indexing.Moved()
		if edited != nil {
			moved = slices.Concat(moved, edited(found))
		}
		if replacement, changes, err = planFoundReferences(ctx, tx, string(kind), itemID, found, moved); err != nil {
			return model.ReferenceChanges{}, nil, err
		}
	}
	return changes, func(b *store.WriteBatch) error {
		indexing.Queue(b)
		if cites {
			replacement.Queue(b)
		}
		return nil
	}, nil
}

// queueReplacement queues replacement's delete and insert in the transaction's write phase.
func queueReplacement(ledger *docs.Ledger, replacement refs.Replacement) error {
	return ledger.Queue(func(b *store.WriteBatch) error {
		replacement.Queue(b)
		return nil
	})
}

// planFoundReferences plans the reconciliation of fromKind/fromID's references with found and
// resolves the counted targets it and moved move. The resolution reads issues, asks and
// artifacts, which the reconciliation does not write, so it reads what it read after the write.
// No document operation reconciles a comment's, an ask's or a message's references, so the
// source's held edges are the stored ones.
func planFoundReferences(ctx context.Context, tx pgx.Tx, fromKind, fromID string, found []text.Ref, moved []refs.Target) (refs.Replacement, model.ReferenceChanges, error) {
	replacement, err := refs.PlanReplace(ctx, tx, fromKind, fromID, found, nil)
	if err != nil {
		return refs.Replacement{}, model.ReferenceChanges{}, err
	}
	changes, err := refs.CountedChanges(ctx, tx, slices.Concat(replacement.Changed(), moved))
	if err != nil {
		return refs.Replacement{}, model.ReferenceChanges{}, err
	}
	return replacement, changes, nil
}

// appendedEvents is the events queueEvent appended, read once Commit has run the write phase, in
// the order given.
func appendedEvents(events ...*model.Event) []model.Event {
	appended := make([]model.Event, len(events))
	for index, event := range events {
		appended[index] = *event
	}
	return appended
}

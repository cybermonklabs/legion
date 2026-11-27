package api

import (
	"context"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
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
		return s.queueLoggedEventOn(b, event, logins, fromKind, fromID, func(written model.Event) { *appended = written })
	})
}

// queueLoggedEventOn is queueLoggedEvent's step on b itself, for a route that runs its write
// phase as a batch of its own (store.RunWrite: createStoredMessage, editMessage, deleteMessage):
// the event's insert with the thread_logins it reads, then the stamp of fromKind/fromID's new
// reference edges with the event's id, none for an empty fromKind. appended receives the event
// once the batch has run it, for the route to publish after its commit.
func (s *server) queueLoggedEventOn(
	b *store.WriteBatch, event model.Event, logins *events.Logins, fromKind, fromID string, appended func(model.Event),
) error {
	if err := s.deps.Events.QueueAppendWithLogins(b, event, logins, appended); err != nil {
		return err
	}
	if fromKind != "" {
		refs.QueueStamp(b, fromKind, fromID)
	}
	return nil
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
// and not at all for an ask whose texts name none. Both resolve the short ids for the ask's
// author (refs.NewReader).
//
// The ask's node is the one the route queued before the row (node): its question's citations and
// its parts' block nodes and links are stated out of it (refs.WrittenItem), so neither reads the
// node back. What they state is answered for the route to queue under the ask.opened it appends
// (graphstore.QueueCaused).
func (s *server) queueNewAskReferences(ctx context.Context, tx pgx.Tx, ledger *docs.Ledger, ask model.Ask, node graphstore.Node) (model.ReferenceChanges, graphstore.Uncaused, error) {
	ctx = ledger.ReadContext(ctx)
	document := refs.PendingAskDocument(ask)
	scope := refs.WrittenItem(node)
	located := refs.NewReaderInScope(ctx, tx, ask.Author, "ask", ask.ID, s.deps.ServerURL, document).Mentions(ask.Question)
	replacement, changes, err := planFoundReferences(ctx, tx, "ask", ask.ID, text.Refs(located), nil, &scope, ask.CreatedAt)
	if err != nil {
		return model.ReferenceChanges{}, graphstore.Uncaused{}, err
	}
	if err := queueReplacement(ledger, replacement); err != nil {
		return model.ReferenceChanges{}, graphstore.Uncaused{}, err
	}
	// The question's reconciliation is queued before the parts, so the parts' statements settle
	// the ask's row of the forms queue (questionReconciled).
	changes, stated, err := s.queueAskParts(ctx, ledger, ask, document, &scope, ask.CreatedAt, true, changes)
	if err != nil {
		return model.ReferenceChanges{}, graphstore.Uncaused{}, err
	}
	stated.Tuples = append(stated.Tuples, replacement.Cited().Tuples...)
	return changes, stated, nil
}

// queueEditedReferences reconciles fromKind/fromID's references in the route's two phases for an
// edit of an ask's question or a comment's body from previous to body, made at `at`, whose short
// ids resolve for writer, the author the source's row records (refs.NewReader): the counted
// targets also name every target the source still cites when the edit moves the head of its
// text, which each of its edges shows, cut against this server's deployment (refs.EditedTargets).
// The source's row is stored - an edit changes a row an earlier transaction wrote - so the
// document its `#id`s resolve against is the one that row answers (refs.ScopeOf), read only if the
// body names a question block, and an item's node is the one the store holds
// (planFoundReferences). It answers the citations the reconciliation states, for the route to
// queue under the event it appends.
func (s *server) queueEditedReferences(ctx context.Context, tx pgx.Tx, ledger *docs.Ledger, writer model.Actor, fromKind, fromID, previous, body string, at time.Time) (model.ReferenceChanges, graphstore.Uncaused, error) {
	ctx = ledger.ReadContext(ctx)
	found := text.Refs(refs.NewReader(ctx, tx, writer, fromKind, fromID, s.deps.ServerURL).Mentions(body))
	replacement, changes, err := planFoundReferences(ctx, tx, fromKind, fromID, found, refs.EditedTargets(previous, body, s.deps.ServerURL, found), nil, at)
	if err != nil {
		return model.ReferenceChanges{}, graphstore.Uncaused{}, err
	}
	return changes, replacement.Cited(), queueReplacement(ledger, replacement)
}

// queueItemReferences plans the index of the blocks of a comment or an issue message this
// transaction writes at `at`, as writer, and the mentions each holds, with the reconciliation of
// the item's own references (planPendingItem), and queues the index's statements and the
// reconciliation's, in that order, in tx's write phase. A direct message (cites false) writes no
// mention, so its blocks hold none either: its blocks alone are indexed, and it moves nothing.
// Every read below runs under the ledger the write queues on, whatever context the route passed
// (docs.Ledger.ReadContext). scope is the item's node as the route queued it (refs.WrittenItem),
// and what the index and the citations state is answered for the route to queue under its event.
func (s *server) queueItemReferences(ctx context.Context, tx pgx.Tx, ledger *docs.Ledger, writer model.Actor, kind refs.ItemKind, itemID, body string, source refs.PendingSource, scope refs.ItemScope, at time.Time, cites bool) (model.ReferenceChanges, graphstore.Uncaused, error) {
	ctx = ledger.ReadContext(ctx)
	changes, write, stated, err := s.planPendingItem(ctx, tx, writer, kind, itemID, body, source, scope, at, cites)
	if err != nil {
		return model.ReferenceChanges{}, graphstore.Uncaused{}, err
	}
	return changes, stated, ledger.Queue(write)
}

// planPendingItem is planItemWrite for an item of kind kind whose row this transaction has not
// written: it plans the item's index and its references over the fields source says the row
// will hold. The document the item's `#id`s resolve against is handed over unread, read from
// those fields and only for a body that names a question block (refs.NewPendingDocument,
// refs.PendingDocument), since no read of that row can answer it yet.
//
// Every route that creates a comment or a message plans through this call, in one of three
// shapes: the one whose insert is queued on a ledger and whose index is queued beside it
// (queueItemReferences), the one whose insert is queued on a batch it runs itself
// (createStoredMessage, and each copy of a broadcast, planBroadcastCopy), and the two delivery
// replies, which have not made the insert at all when they plan and run the planned step where
// they make their write (planCommentReply, planMessageReply).
func (s *server) planPendingItem(
	ctx context.Context, tx pgx.Tx, writer model.Actor, kind refs.ItemKind, itemID, body string,
	source refs.PendingSource, scope refs.ItemScope, at time.Time, cites bool,
) (model.ReferenceChanges, docs.WriteStep, graphstore.Uncaused, error) {
	return s.planItemWrite(ctx, tx, writer, kind, itemID, body, refs.NewPendingDocument(string(kind), source), &scope, at, cites, nil, nil)
}

// queueEditedItemReferences is queueItemReferences for an edit of a comment from previous to
// body, written at `at`, with the block ids the editor carried (carried; nil: the ids follow the
// text); writer is the author the comment's row records. It reads planEditedItem under the
// ledger's read context, queues its write there, and returns the stated tuples for the route to
// queue under its event. A carried id the index refuses returns its *itemblocks.CarriedError.
func (s *server) queueEditedItemReferences(ctx context.Context, tx pgx.Tx, ledger *docs.Ledger, writer model.Actor, kind refs.ItemKind, itemID, previous, body string, at time.Time, carried *refs.CarriedEdit) (model.ReferenceChanges, graphstore.Uncaused, error) {
	changes, write, stated, err := s.planEditedItem(ledger.ReadContext(ctx), tx, writer, kind, itemID, previous, body, at, carried)
	if err != nil {
		return model.ReferenceChanges{}, graphstore.Uncaused{}, err
	}
	return changes, stated, ledger.Queue(write)
}

// planEditedItem is planItemWrite for a rewrite of a stored comment's or issue message's body
// from previous to body, at `at`, with the block ids the editor carried (carried; nil: the ids
// follow the text): an edit of either kind, and a delete's clear, whose body is empty
// (clearDeletedPostBody). writer is the author the item's row records, whom its short ids
// resolve for (refs.NewReader). Both kinds it rewrites cite what they mention, and the counted
// targets also name every target the item still cites when the rewrite moves the head of its
// text, which each of its edges shows (refs.EditedTargets, which reads both texts with the shared
// grammar against this server's deployment). It is the one plan of every such rewrite, which its
// caller queues where it makes the write: on a comment's ledger (queueEditedItemReferences, and
// deleteComment's through clearDeletedPostBody) or on the batch a message's route runs itself
// (editMessage, and deleteMessage's through clearDeletedPostBody). The item's node is the one the
// store holds, and what the index and the citations state is answered for the caller to queue
// under the event it appends (graphstore.QueueCaused).
func (s *server) planEditedItem(
	ctx context.Context, tx pgx.Tx, writer model.Actor, kind refs.ItemKind, itemID, previous, body string, at time.Time, carried *refs.CarriedEdit,
) (model.ReferenceChanges, docs.WriteStep, graphstore.Uncaused, error) {
	return s.planItemWrite(ctx, tx, writer, kind, itemID, body, nil, nil, at, true, carried, func(found []text.Ref) []refs.Target {
		return refs.EditedTargets(previous, body, s.deps.ServerURL, found)
	})
}

// planItemWrite plans the item's index and, when the item cites its mentions, the reconciliation
// of its own references, counts what they and edited (the targets an edit moves although its
// edges stay; nil for a first write) move, and returns those counts with the step that writes
// both, for the route to run where it makes the write: queued on its ledger
// (queueItemReferences), on the batch of a transaction that runs its write phase itself
// (createStoredMessage, writeBroadcast's copies), in the batch that inserts the row of a route
// that writes as it goes (store.RunWrite, a session's reply to a message, insertSessionReply), or
// as a batch of its own beside such a route's statements (the comment reply, replyComment). The
// reconciliation is queued after the index, so it settles the item's row of the forms queue after
// both deletes (refs.Replacement.SettlingForms), and the excerpt of each block is cut in the plan,
// against this server's deployment (refs.PlanItemIndex). It answers what the index and the
// citations state in the node and tuple store - the block nodes, their part_of and links, and the
// item's mentions - which the route queues under the event it appends (graphstore.QueueCaused).
//
// Every read it makes runs under the context it is given, which its caller derives from the
// ledger the write queues on (queueItemReferences, docs.Ledger.ReadContext), so a mention of a
// row this transaction has queued resolves to that row (refs.Pending). A transaction that runs
// its own write phase has queued no row and reads under the context it holds
// (createStoredMessage, planBroadcastCopy, planCommentReply, planMessageReply).
//
// document is what the route hands over for an item whose row this transaction has not written,
// so that no read of it can answer: the document its `#id`s resolve against
// (refs.NewReaderInScope, planPendingItem). It is nil for a write of an item already stored,
// which ScopeOf answers for itself. scope is likewise the item's node as the route queued it
// (refs.WrittenItem), and nil for an item whose node the store holds, which the plan reads.
// writer is the author the item's row records, or will record for a row only queued, whom its
// short ids resolve for (refs.NewReader).
func (s *server) planItemWrite(
	ctx context.Context,
	tx pgx.Tx,
	writer model.Actor,
	kind refs.ItemKind,
	itemID, body string,
	document *refs.PendingDocument,
	scope *refs.ItemScope,
	at time.Time,
	cites bool,
	carried *refs.CarriedEdit,
	edited func(found []text.Ref) []refs.Target,
) (model.ReferenceChanges, docs.WriteStep, graphstore.Uncaused, error) {
	index, err := refs.ReadItemIndex(ctx, tx, kind, itemID)
	if err != nil {
		return model.ReferenceChanges{}, nil, graphstore.Uncaused{}, err
	}
	if scope != nil {
		index = index.Scoped(*scope)
	}
	if edited != nil {
		// The update that sets edited_at is queued, so the read above does not show it.
		index = index.Edited()
	}
	var located []text.Located
	if cites {
		located = refs.NewReaderInScope(ctx, tx, writer, string(kind), itemID, s.deps.ServerURL, document).Mentions(body)
	}
	indexing, err := refs.PlanItemIndex(index, kind, itemID, body, s.deps.ServerURL, at, located, carried)
	if err != nil {
		return model.ReferenceChanges{}, nil, graphstore.Uncaused{}, err
	}
	stated := indexing.Stated()
	var replacement refs.Replacement
	var changes model.ReferenceChanges
	if cites {
		found := text.Refs(located)
		moved := indexing.Moved()
		if edited != nil {
			moved = slices.Concat(moved, edited(found))
		}
		if replacement, changes, err = planFoundReferences(ctx, tx, string(kind), itemID, found, moved, scope, at); err != nil {
			return model.ReferenceChanges{}, nil, graphstore.Uncaused{}, err
		}
		replacement = replacement.SettlingForms()
		stated.Tuples = append(stated.Tuples, replacement.Cited().Tuples...)
	}
	return changes, func(b *store.WriteBatch) error {
		if err := indexing.Queue(b); err != nil {
			return err
		}
		if cites {
			replacement.Queue(b)
		}
		return nil
	}, stated, nil
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
//
// A comment's, a message's or an ask's reconciliation is planned with the item's citations
// (refs.PlanItemReplace), which every writer of an item's refs rows states, so no row is written
// holding tuples_stored without the tuple it says the store holds: under scope, the item's node
// as the calling write queued it (refs.WrittenItem), or, where scope is nil, the node the store
// holds, read here (refs.ReadItemScope); its new rows and their citations are dated `at`, the
// moment of the write. Any other source's is planned as refs.PlanReplace plans it.
func planFoundReferences(ctx context.Context, tx pgx.Tx, fromKind, fromID string, found []text.Ref, moved []refs.Target, scope *refs.ItemScope, at time.Time) (refs.Replacement, model.ReferenceChanges, error) {
	var replacement refs.Replacement
	var err error
	switch kind := refs.ItemKind(fromKind); kind {
	case refs.ItemComment, refs.ItemMessage, refs.ItemAsk:
		var itemScope refs.ItemScope
		if scope != nil {
			itemScope = *scope
		} else if itemScope, err = refs.ReadItemScope(ctx, tx, kind, fromID); err != nil {
			return refs.Replacement{}, model.ReferenceChanges{}, err
		}
		replacement, err = refs.PlanItemReplace(ctx, tx, itemScope, kind, fromID, found, at)
	default:
		replacement, err = refs.PlanReplace(ctx, tx, fromKind, fromID, found, nil)
	}
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

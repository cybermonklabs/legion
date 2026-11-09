package docs

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/persistence"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// appendUpdateTxClass appends one update inside tx under the gate: a run of one
// (appendUpdatesTx), with no credit and no flush gating it. The version returned is the one it
// stored; a run of one is stored whole or refused.
func (p *PgVersioned) appendUpdateTxClass(ctx context.Context, tx pgx.Tx, room string, update []byte, contentChanged bool, kind appendKind) (persistence.Version, error) {
	version, _, err := p.appendUpdatesTx(ctx, tx, room, []BrowserUpdate{{Update: update, ContentChanged: contentChanged}}, kind, nil, false)
	return version, err
}

// AppendRoomUpdatesTx appends inside tx the run of updates a room of this process has already
// applied, in the room's commit order, with the class and the in-flight credit its update observer
// recorded for each, as one append of flush, the run the appending transaction stores of that room
// instance (flushThrough). The room decoded each when it applied it, so none is decoded again.
// flush is what gates the append: the instance's cursor with the versions this flush has appended
// for it folded in, since a transaction that flushes a room it holds no write of (a settlement, a
// version's capture, the block-id backfill) gates its flush appends by that flush
// (appendingCursor). The room's own persistence worker reaches the gate another way, in its
// batch's first pipeline (queueAppendGate, PgVersioned.AppendRoomFlush).
//
// It takes no lock of its own: every flush runs under the room's advisory lock already, the
// session-level one on the room's own persistence worker's connection and the transaction-level
// one on every other writer's, taken before the head the flush is gated on was read
// (Service.flushThrough states the rule and names the callers). A second acquisition would cost a
// round trip and grant what the caller holds.
//
// It answers the first version the run stored and how many of the run it stored, which the caller
// judges (Service.flushThrough): the gate admits a prefix of a browser's run and no more, cutting
// it at the first update its document's closed issue no longer admits (gateAppend), and a flush
// left short of its run fails with ErrIssueClosed, so the transaction that joined it rolls back
// and stores nothing of the run. The prefix rule is the room's own batch path's (appendRoomTx),
// which stores what the room took in time and leaves the refused rest queued, unstored and
// unmarked, for its own call to meet the gate again, which refuses it and closes the room for its
// issue.
func (p *PgVersioned) AppendRoomUpdatesTx(ctx context.Context, tx pgx.Tx, room string, flush *RoomFlush, updates []BrowserUpdate) (persistence.Version, int, error) {
	return p.appendUpdatesTx(ctx, tx, room, updates, appendBrowser, flush, true)
}

// appendUpdatesTx appends updates inside tx under the gate, at consecutive versions from the head
// it reads: the document's advisory lock where the caller holds none (roomLocked), the gate's read
// (readAppendGate), a crashed prune's recovery where one stands, the gate's verdict (gateAppend)
// and the append's writes (appendWrites), put on the transaction's write phase as one step
// (write). It answers the first version the run stored and how many of the run it stored, which
// the gate's prefix rule can leave short of the run it was handed.
func (p *PgVersioned) appendUpdatesTx(ctx context.Context, tx pgx.Tx, room string, updates []BrowserUpdate, kind appendKind, flush *RoomFlush, roomLocked bool) (persistence.Version, int, error) {
	if err := ctx.Err(); err != nil {
		return 0, 0, err
	}
	// A transaction that has already queued a write of this document's history holds the
	// document's lock from that write's own read and knows the head its queue leaves
	// (pendingRoom): the table holds none of those rows yet, so the head and the intact flag come
	// from the queue (appendGateRead.after) and the rest of the verdict from this read, and the
	// prune recovery is the first write's alone. Every other append takes the lock here, once:
	// before the cursor the gate reads and before the gate's own read, and not again, since a
	// second acquisition in the transaction that holds it grants what the transaction has and
	// costs the round trip every other writer of the document waits out.
	pending := pendingRoomOf(ctx, tx, room)
	if !roomLocked && pending == nil {
		if err := lockDocumentRoom(ctx, tx, room); err != nil {
			return 0, 0, err
		}
	}
	// One read answers the gate's three questions and carries the prune checkpoint with it: a
	// crashed prune's recovery (recoverPruneTx) runs only where a checkpoint stands, and the gate
	// is read again under the ceiling that recovery leaves, since its deletes of the rows above
	// it are queued with the transaction's other writes.
	read, err := readAppendGate(ctx, tx, room, nil)
	if err != nil {
		return 0, 0, err
	}
	if pending == nil {
		var recovered *int64
		if read.pruneCeiling != nil {
			ceiling, recovering, err := p.recoverPruneTx(ctx, tx, room)
			if err != nil {
				return 0, 0, err
			}
			recovered = ceilingOf(ceiling, recovering)
			// The recovery's deletes go on the transaction's write phase, so the gate is read
			// again under the ceiling they leave rather than over rows they have not taken out
			// yet.
			if read, err = readAppendGate(ctx, tx, room, recovered); err != nil {
				return 0, 0, err
			}
		}
		pending = recordPendingRoom(ctx, tx, room, read.latest, recovered)
	}
	read = read.after(pending)
	through, gated := appendingThrough(ctx, room, flush)
	head, admitted := read.latest, len(updates)
	if gated {
		var replaced bool
		head, admitted, replaced, err = gateAppend(ctx, tx, room, through, kind, updates, read, p.maxPendingItems)
		if err != nil {
			return 0, 0, err
		}
		if replaced {
			p.markRunReplaced(room, roomCursorFrom(ctx), flush)
		}
	}
	// A closed issue admits only the updates the room took in time, a prefix of the run
	// (gateAppend): the rest are not stored here and are left queued, as the room's own batch
	// path leaves them (appendRoomTx). A flush left short this way is failed by its caller, which
	// rolls this prefix back with it (Service.flushThrough).
	updates = updates[:admitted]
	plan, err := p.appendWrites(ctx, tx, room, head, updates, true)
	if err != nil {
		return 0, 0, err
	}
	if err := plan.send(ctx, tx); err != nil {
		return 0, 0, err
	}
	// A queued statement writes nothing until the write phase runs, so the history its queue
	// leaves is recorded for the reads of this transaction that follow it (pendingRoom): a later
	// append of the same document in the same transaction goes after this one, and a read of the
	// head takes it from there. What the pending-author and ask-block-author statements will
	// leave is recorded by the helpers that queued them (queuePendingAuthors, queueAskAuthors).
	if pending != nil {
		pending.head = int64(plan.last)
		pending.appended += int64(len(updates))
		for _, appended := range updates {
			pending.changed = pending.changed || appended.ContentChanged
		}
	}
	return persistence.Version(head + 1), len(updates), nil
}

// markRunReplaced marks the room instance an admitted run was made in as standing on a replaced
// history, once for the run: its caller closes and reloads the room after the append
// (roomCursor.takeReplaced, Service.closeRoomOnReplacedHistory). The instance is the one the
// context carries, or the flush's when the run is a flush's.
func (p *PgVersioned) markRunReplaced(room string, cursor *roomCursor, flush *RoomFlush) {
	if cursor == nil && flush != nil {
		cursor = flush.cursor
	}
	if cursor != nil {
		cursor.markReplaced()
	}
	if p.storedHistoryHeld != nil {
		p.storedHistoryHeld(room)
	}
}

// appendUpdatesStatement stores every update of one append transaction and announces the change,
// in one statement: the insert of one doc_updates row per update, at consecutive versions from the
// head the gate read, each with its own content class, is a data-modifying CTE, and so is the
// document's name in doc_compaction_owed, the documents the compaction sweep reads
// (compactionCandidates); the select they feed is announceDocumentHead, which sends both
// notifications, documentHeadChannel's {r,v,p} naming the run's last version and
// documentChangedChannel's bare id. One update or a room's whole batch, the transaction spends one
// round trip under the document's advisory lock on its rows and its announcement, and sends each
// notification once. Why the announcement belongs inside this transaction is
// announceDocumentChange's doc comment, which carries a rebuild's, a prune's and a deletion's. The
// room is passed twice because its two parameters are different types: pg_notify takes text ($1),
// and doc_updates.artifact_id is a uuid ($2).
var appendUpdatesStatement = `
	with appended as (
		insert into doc_updates (artifact_id, version, update, content_changed)
		select $2::uuid, version, update, content_changed
		from unnest($4::bigint[], $5::bytea[], $6::boolean[]) as appended(version, update, content_changed)
	), owed as (
		insert into doc_compaction_owed (artifact_id) select $2::uuid where $7 on conflict do nothing
	)
	select ` + announceDocumentHead("$1::text", "$3::bigint", "$8::text")

// appendUpdatesJoinedStatement is appendUpdatesStatement inside a transaction a caller joined
// (Service.Join), which announces once at its commit with the last version it appended
// (Ledger.announceAppends) rather than once per append. It takes the document's id once, as the
// uuid its rows carry: with no announcement it sends no notification, so it needs neither the
// text spelling of the id nor the version and the process the notification names, and a
// parameter it did not read would be one Postgres could find no type for.
const appendUpdatesJoinedStatement = `
	with appended as (
		insert into doc_updates (artifact_id, version, update, content_changed)
		select $1::uuid, version, update, content_changed
		from unnest($2::bigint[], $3::bytea[], $4::boolean[]) as appended(version, update, content_changed)
	)
	insert into doc_compaction_owed (artifact_id) select $1::uuid where $5 on conflict do nothing`

// appendPlan is what one write owes: statements, in the order the transaction runs them, and
// last, the version the last of its updates takes (0 for a write that appends none). A caller
// puts them on its transaction's write phase (send), or queues them into a batch it commits
// itself (appendRoomTx, writeStatement.queue).
//
// A writer with one statement to make gathers it here too, so that statement is queued by the
// same helper the append queues it through and cannot be queued without what the helper records
// beside it (queueAskAuthors, queuePendingAuthors): it sends a plan of one.
type appendPlan struct {
	statements []writeStatement
	last       persistence.Version
}

// queue adds one statement to the plan, with what to say when it fails.
func (p *appendPlan) queue(what, sql string, args ...any) {
	p.add(writeStatement{sql: sql, args: args, what: what})
}

// add adds a statement a builder made (pendingAuthorsStatement, updateOwedStatement).
func (p *appendPlan) add(statement writeStatement) {
	p.statements = append(p.statements, statement)
}

// send puts every statement of the plan on the transaction's write phase, in the order it queued
// them, so a failure is reported as its own statement's: where the transaction queues its writes
// they join its one pipelined batch, and where it does not they are a batch of their own, one
// round trip (write, store.RunWrite). A plan with nothing in it writes nothing: a step of no
// statements would still cost the round trip a write of its own takes.
func (p *appendPlan) send(ctx context.Context, tx pgx.Tx) error {
	if len(p.statements) == 0 {
		return nil
	}
	return write(ctx, tx, func(b *store.WriteBatch) error {
		for _, statement := range p.statements {
			statement.queueWrite(b)
		}
		return nil
	})
}

// appendWrites is every write of an append transaction whose gate passed at head, in the order the
// transaction runs them, and the last version they store: the insert with its announcement and the
// document's compaction entry (appendUpdatesStatement), the owed settlement written once
// (updateOwedStatement), one doc_pending_authors upsert of every credited update's authors at that
// update's own version (pendingAuthorsStatement), left out when no update credits anyone, and the
// authors of the ask blocks the run's renders first showed (askAuthorsStatement), recorded with
// the updates so a settlement on any process finds them, consumed credit or not: a version lists
// authors, not ask blocks. The rows are exactly those the same updates leave appended one at a
// time. A caller puts the statements on its transaction's write phase (appendPlan.send, which
// queues each with writeStatement.queueWrite) or queues them itself into the batch its own
// connection commits with (writeStatement.queue, appendRoomTx in room_appends.go).
//
// owes is whether the document owes a settlement from these updates. It is false for a room's
// parked bytes alone (AppendParkedUpdate), which change nothing a settlement reads and credit
// nobody: they write no owed row and no pending authors, and the commit that later integrates
// them marks the document owed itself.
//
// Each update's credit is taken here (UpdateCredit.take), so the caller holds the document's
// advisory lock from before the gate until the transaction ends. The owed row is taken with the
// write, so a route that goes on to append an event (an upload, issue creation) holds it before
// its event, as an answer marking this document a copy owed does (owedCopiesOf);
// Ledger.Commit's later recordLatestEditSource then waits on nothing.
func (p *PgVersioned) appendWrites(ctx context.Context, tx pgx.Tx, room string, head int64, updates []BrowserUpdate, owes bool) (appendPlan, error) {
	if len(updates) == 0 {
		return appendPlan{}, errors.New("append document updates: no update to append")
	}
	versions := make([]int64, len(updates))
	contents := make([][]byte, len(updates))
	classes := make([]bool, len(updates))
	var owed owedUpdates
	var authors []pendingAuthor
	var askAuthors map[string]model.Actor
	for index, appended := range updates {
		version := head + int64(index) + 1
		versions[index], contents[index], classes[index] = version, appended.Update, appended.ContentChanged
		updateAuthors, updateActor, include := appended.Credit.take()
		// An edit credited to authors names its latest edit source, none included when no one
		// peer made it, and does to the latest person sender what latestPersonOf says; an update
		// no one is credited with keeps the row's.
		if include && (len(updateAuthors) > 0 || updateActor != nil) {
			owed.credit(updateActor, anyPerson(updateAuthors))
		}
		if include {
			authors = append(authors, pendingAuthorsAt(updateAuthors, persistence.Version(version))...)
		}
		// The ask block authors of the whole run go in one statement, earliest update first:
		// the statement writes each block once (on conflict do nothing), so the earliest update
		// that showed a block names its author, as it would have appending alone.
		for block, actor := range appended.Credit.takeAskAuthors() {
			if askAuthors == nil {
				askAuthors = make(map[string]model.Actor)
			}
			if _, named := askAuthors[block]; !named {
				askAuthors[block] = actor
			}
		}
	}
	last := versions[len(versions)-1]
	// The document is named in doc_compaction_owed once a row the run appends is not the
	// document's first: a first row has nothing to fold into, and a document already named stays
	// as it is.
	folds := head > 0 || len(updates) > 1
	insert := writeStatement{
		sql:  appendUpdatesStatement,
		args: []any{room, room, last, versions, contents, classes, folds, p.processID},
		what: "append document update",
	}
	// Inside a transaction a caller joined (Service.Join) the insert carries no announcement: the
	// ledger announces once at its commit, with the last version the transaction appended. Its
	// statement reads the id, the rows and that one flag, and nothing else.
	if ledger := ledgerFrom(ctx); ledger != nil && ledger.tx != nil {
		insert.sql = appendUpdatesJoinedStatement
		insert.args = []any{room, versions, contents, classes, folds}
		ledger.deferAnnouncement(room, persistence.Version(last))
	}
	plan := appendPlan{statements: []writeStatement{insert}, last: persistence.Version(last)}
	if !owes {
		return plan, nil
	}
	owedStatement, err := updateOwedStatement(room, owed)
	if err != nil {
		return appendPlan{}, err
	}
	plan.add(owedStatement)
	// The pending authors and the authors of the ask blocks this run's renders first showed go
	// through the one helper per table that queues the statement and records what it will leave
	// on the ledger (queuePendingAuthors, queueAskAuthors), so no writer can do one without the
	// other. The ask blocks are recorded with the updates whether or not their credit was
	// consumed: a version lists authors, not ask blocks.
	if err := queuePendingAuthors(ctx, tx, &plan, room, authors); err != nil {
		return appendPlan{}, err
	}
	if err := queueAskAuthors(ctx, tx, &plan, room, askAuthors); err != nil {
		return appendPlan{}, err
	}
	return plan, nil
}

// writeStatement is one statement of a durable write, with the words its error is wrapped in. A
// caller runs it in its own transaction (exec), puts it on that transaction's write phase (write,
// queueWrite), or queues it into a batch it sends itself (queue), so one SQL serves a write made
// any of those ways.
type writeStatement struct {
	sql  string
	args []any
	what string
}

// exec runs the statement in tx.
func (s writeStatement) exec(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, s.sql, s.args...); err != nil {
		return fmt.Errorf("%s: %w", s.what, err)
	}
	return nil
}

// write puts the statement on tx's write phase, so a transaction whose writes are queued runs it
// in its own batch (writeExec), and one whose writes are not runs it at once.
func (s writeStatement) write(ctx context.Context, tx pgx.Tx) error {
	return writeExec(ctx, tx, s.what, s.sql, s.args...)
}

// queueWrite queues the statement on a write phase's batch, for a caller already building one.
func (s writeStatement) queueWrite(b *store.WriteBatch) {
	b.Queue(s.what, s.sql, s.args...)
}

// queue queues the statement into b (store.QueueExec); its error, wrapped as exec wraps it, is
// what b's Close returns when the statement is the first of b's to fail.
func (s writeStatement) queue(b *pgx.Batch) {
	store.QueueExec(b, s.what, s.sql, s.args...)
}

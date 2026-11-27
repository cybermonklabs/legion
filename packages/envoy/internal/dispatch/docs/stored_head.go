package docs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/crdt"
	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// The stored head - the greatest version a document's stored history holds - is what this
// process's view of a document is judged against while another envoy-dispatch process writes the
// same document: what a read compares a resident room's cursor against (readStoredHead,
// roomHoldsHead, docTree), what a write's fork is read against under the document's lock, which
// its transaction takes with that head and holds to its end (readFork, roomBehindHead), what a
// committed write's lost operations are judged on (lostAtStoredHead), what a published write's
// lost operations are judged on (recordPublishedLoss), and what the other processes are asked to
// store before any of that is read (flushOtherRooms).
// A room of this process is the head only while it holds every stored row (roomCursor,
// roomCatchUps); behind it, a read takes the stored document merged with the updates only that
// room holds instead (storedWithUpdates), and waits for no catch-up.

// flushOtherRooms has every other envoy-dispatch process that holds a room of artifactID store the
// updates that room has applied (a flush query, room_query.go), and waits for their answers, for
// at most roomQueryWait. A write forked after it - from the store, or from this process's room once
// it is at the head (forkLive) - then holds every update any process applied before it asked: an
// edit whose target a person on another process has just deleted fails as it would on one process
// (ErrQuoteNotFound), rather than writing into a paragraph that is gone.
//
// What the wait holds, and why it cannot deadlock: the calling transaction's own connection
// (tx, through which it reads the members, so it takes no second connection of the pool), and no
// lock of this document - not its owner row, not its writer slot, not its advisory lock. The query
// goes through the process's NOTIFY sender, its own connection, and is delivered at once, not at
// tx's commit. What a member's flush waits on is its room's appends, which take this document's
// advisory lock and a key-share lock on its artifact row, neither of which the caller holds. A
// caller holding the advisory lock is a mistake in the lock order the flush depends on; the query
// reads that from the database and skips the flush rather than wait out every member
// (roomQueries.ask).
func (s *Service) flushOtherRooms(ctx context.Context, tx Queryer, artifactID string) error {
	_, _, err := s.roomQueries.ask(ctx, tx, artifactID, roomQueryFlush)
	return err
}

// readStoredHead is the stored head a read of artifactID compares a resident room's cursor
// against (roomHoldsHead): the greatest stored version, as the gate and a version capture read it
// (currentUpdateCursor), read for who (storedHeadSQL): a document who may not read is
// pgx.ErrNoRows, as a document no row holds is, so the read holds the reader's rule in the
// statement that reads the document's history and not only in the route's earlier read of its
// row. It reads where writerStoredHead reads (headReader), takes no lock and waits for nothing. It
// is the one place a read learns the head.
func (s *Service) readStoredHead(ctx context.Context, who model.Actor, artifactID string) (int64, error) {
	q, err := s.headReader(ctx)
	if err != nil {
		return 0, err
	}
	var head int64
	if err := q.QueryRow(ctx, storedHeadSQL, artifactID, refs.VisibilityArg(who)).Scan(&head); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, pgx.ErrNoRows
		}
		return 0, headError(ctx, err)
	}
	return head, nil
}

// storedHeadSQL is currentUpdateCursor's head of the document $1 names, for the reader whose viewer
// $2 binds (refs.VisibilityArg): read only where that reader may read the document
// (access.DocumentClause over its own row), so a document it may not read is a row the statement
// does not find.
var storedHeadSQL = `select coalesce((select max(u.version) from doc_updates u where u.artifact_id = a.id), 0)
		from artifacts a where a.id = $1 and ` + access.DocumentClause("a", "$2")

// writerStoredHead is the stored head a write's read of artifactID (docTree) compares a resident
// room's cursor against, as readStoredHead does for a read: currentUpdateCursor with no reader,
// since every caller is a write that has judged its writer's sight of the document itself,
// under the document owner's lock where it holds a transaction.
func (s *Service) writerStoredHead(ctx context.Context, artifactID string) (int64, error) {
	q, err := s.headReader(ctx)
	if err != nil {
		return 0, err
	}
	head, err := currentUpdateCursor(ctx, q, artifactID)
	if err != nil {
		return 0, headError(ctx, err)
	}
	return head, nil
}

// headReader is where a read of the stored head reads: through the caller's transaction when there
// is one, since a second connection of the shared pool taken under it is what deadlocks the pool
// (store.ErrNestedAcquire), and otherwise on the pool that owns loads, as a cold read reads the
// store's stamp (PgVersioned.DocumentStamp), so a caller holding a pooled connection it did not
// join, settlement's transaction, takes no second one either.
func (s *Service) headReader(ctx context.Context) (Queryer, error) {
	if tx, joined := txFromContext(ctx); joined {
		return tx, nil
	}
	rooms, err := s.store.Pool.Rooms()
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrServiceUnavailable, err)
	}
	return rooms, nil
}

// headError is a failed head read as its caller answers it: the request's own end, else
// ErrServiceUnavailable.
func headError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return fmt.Errorf("%w: %w", ErrServiceUnavailable, err)
}

// roomHoldsHead reports whether live, a resident room, holds every stored update through head:
// its cursor's contiguous run (roomCursor.heldThrough) stands at or past it. Each cursor advance
// follows the update into the document, so a room it reports holds that much, and a room only
// gains updates, so it holds them for every read after this. A room behind head is missing an
// update another process stored that its catch-up has not applied yet (roomCatchUps), and a read
// takes the stored document merged with the updates only that room holds (storedWithUpdates,
// unstoredShare) rather than wait for that catch-up: what it reads is then that room's own text
// and what another process stored on top of it, so the read is older than neither. A browser's own
// append is enough to leave a room judged behind - its commit raises the head and its cursor
// takes that version a moment later (appendUpdate) - and the merge is why such a read answers
// with the keystroke that append has not stored yet rather than taking it back. A room that
// keeps no cursor is not judged behind: nothing says it is (roomBehindHead).
func (s *Service) roomHoldsHead(live *crdt.Doc, head int64) bool {
	cursor := s.cursorOf(live)
	return cursor == nil || cursor.heldThrough() >= head
}

// roomHeld reports whether this process holds a room of room, resident or still loading.
func (s *Service) roomHeld(room string) bool {
	return s.srv.GetDoc(room) != nil || slices.Contains(s.srv.Rooms(), room)
}

// publishedLossTimeout bounds what recordPublishedLoss reads: the other processes' flush, a
// catch-up of this process's room, or a load of the stored document. The write's writer slot is
// held until it returns.
const publishedLossTimeout = 10 * time.Second

// recordPublishedLoss judges, once the write is committed and published, which of the write's
// operations the document at its stored head no longer holds, and records it: the lost-edit
// check's second window, a concurrent change that landed between the version render and this
// publish. Nothing can be rolled back here - the update is durable and the version written - so
// the verdict rides the caller's response instead (Ledger.LostOps).
//
// A write that inserted no text reaches the verdict too, an empty one: a delete, a replace that
// only shortens, a retype, an operation that changed nothing - none of them wrote anything a
// concurrent change could remove, so "nothing was lost" is a statement this can make without
// reading the document, and it is the one those edits deserve. Only a check that could not read
// the document, or a document holding a tree too deep to read, leaves no verdict, which the caller
// reports as undetermined.
//
// A person on another process can have deleted what the write inserted, in a room this process
// never holds. So first every other process holding a room of the document makes its updates
// durable (a second flush, flushOtherRooms's query, asked now that the write holds no lock), and
// the verdict is judged on the document at the stored head: this process's room, caught up to the
// head first when another process is a member (catchUpRoom), or, with no room here or one that
// could not be caught up, the stored document itself, read without loading a room (on the pool
// document loads use: other writers waiting for this write's slot hold the shared pool's
// connections).
//
// The read is a point-in-time statement, as the spec says it must be: a deletion landing after it
// is not reported, and a deletion landing before it is. A room is read as of one moment under its
// document lock (readLive), since the room's peers write it while the check walks it, and the walk
// (pmdoc.AuthoredTextRuns) takes no lock. The room's update observer has already brought its
// replica up to date with this write, so the check reads the write's own blocks rather than a copy
// of the whole document. It takes no Yjs transaction of its own, which would hold the room's
// document lock, and so every browser's update to the room, for the whole walk.
func (s *Service) recordPublishedLoss(write *liveWrite) {
	if write.loss == nil {
		write.lost, write.lostVerdict = nil, true
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), publishedLossTimeout)
	defer cancel()
	rooms, err := s.store.Pool.Rooms()
	if err != nil {
		slog.Warn("dispatch: reach the document store to judge a published write's text", "room", write.artifactID, "error", err)
		return
	}
	_, others, err := s.roomQueries.ask(ctx, rooms, write.artifactID, roomQueryFlush)
	if err != nil {
		// Whether another process holds the document is unknown, so the verdict reads the head.
		slog.Warn("dispatch: ask the other processes to store their rooms' updates before judging a published write's text",
			"room", write.artifactID, "error", err)
		others = true
	}
	lost, err := s.lostAtStoredHead(ctx, rooms, write, others)
	if err != nil {
		slog.Warn("dispatch: read a published write's text back from the document", "room", write.artifactID, "error", err)
		return
	}
	write.lost, write.lostVerdict = lost, true
}

// lostAtStoredHead is write's lost operations on the document at its stored head: this process's
// room - caught up through rooms first when others says another process may have stored what it
// lacks - or the stored document when no room is resident here or its catch-up did not bring it
// to the head.
func (s *Service) lostAtStoredHead(ctx context.Context, rooms Queryer, write *liveWrite, others bool) ([]int, error) {
	room := write.artifactID
	if live := s.srv.GetDoc(room); live != nil {
		current := true
		if others {
			outcome, err := s.catchUpRoom(ctx, rooms, room, live)
			current = err == nil && (outcome == catchUpCurrent || outcome == catchUpApplied)
		}
		if current {
			var lost []int
			var lossErr error
			if err := s.readLive(room, live, func(doc *crdt.Doc) {
				lost, lossErr = write.loss.lost(doc)
			}); err != nil {
				return nil, fmt.Errorf("copy the room: %w", err)
			}
			return lost, lossErr
		}
	}
	loaded, err := s.persistence.Load(ctx, room)
	if err != nil {
		return nil, fmt.Errorf("load the stored document: %w", err)
	}
	doc := newDocumentCopy()
	if len(loaded.Update) > 0 {
		if err := crdt.ApplyUpdateV1(doc, loaded.Update, nil); err != nil {
			return nil, fmt.Errorf("decode the stored document: %w", err)
		}
	}
	return write.loss.lost(doc)
}

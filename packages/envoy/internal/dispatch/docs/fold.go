package docs

// The fold and decode of a room's stored history: the read of its rows a load and a compaction
// share (foldRead), which folds rows totalling at most foldBufferBytes from memory once its
// transaction has ended and streams larger logs inside it; the state the rows make (stateThrough);
// the document that state decodes into (documentThrough, LoadDocument); the fold itself (fold,
// readsBackOtherwise); the rows applied in the order they were stored where the fold does not read
// back (runsThrough, runsState, runsDocument); and whether a document holds anything it cannot
// place (parks).

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"

	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/persistence"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// parks reports whether doc holds anything it cannot place: an item whose origin or predecessor
// has not arrived, or a delete of an item it does not hold. A document that parks nothing holds
// whole every update applied to it.
func parks(doc *crdt.Doc) bool {
	parked := doc.PendingStats()
	return parked.Items > 0 || parked.DeleteRanges > 0
}

// stateThrough returns the state the room's stored updates through version, which updates replays,
// make, as one V1 update. It applies the updates one at a time, oldest first, to one document that
// collects garbage, and encodes that document, so a deleted item keeps its id and length but none
// of its content, and what it holds at once is that document and the one update being applied,
// however much the stored updates inserted and later deleted. Nothing reads deleted content back
// from the store: every document built from it - a room ygo's server loads, a read, a fork, a
// validation - is a crdt.New, which collects an item's content in the transaction that deletes it,
// the transaction that applies a load included.
//
// A document parks an update whose dependencies have not arrived, and a delete of an item it does
// not hold. ygo's encoding of the document carries both (upstream since v1.51.0), and when the
// document parked anything stateThrough also merges in each stored update's structs past the
// document's state vector and every stored update's deletes, as it did while ygo's encoding
// carried neither; a room that loads the state parks them again.
//
// The state is kept only when it reads back as the document that made it: decoded into a fresh
// document, it must make the same state vector. That catches a re-encoding that renumbers or drops
// a client's clocks, as a decoder that integrated the items after a skipped clock range would, at
// lower clocks; ygo parks them (upstream since v1.51.0, reearth/ygo#257). It does not catch a
// re-encoding that keeps every clock and moves text, such as a lost right origin:
// TestACompactedDocumentKeepsItsOrderThroughLaterUpdates guards that on each ygo bump. A state that
// reads back otherwise, or a log the fold's document cannot apply (one parking more than its
// pending queue, maxPending, holds), is not used, and a compaction, which calls fold itself, leaves
// the updates as stored. stateThrough returns them applied in the order they were stored, never
// merged across that order (runsThrough, runsState): rows that form one run are returned as that
// run, their merge, which holds what applying them one after another holds; rows of several runs
// are applied run by run to one document of maxPending pending items (runsDocument), and that
// document's encoding is returned. Whether a struct parks is decided by the document that receives
// it, which no merge of updates has, so a merge of two runs can keep a struct that a document
// applying them parks and drop the rows it covers. A document that parks a struct over clocks it
// holds encodes as ygo's merge of its parked structs leaves it (crdt.EncodeStateAsUpdateV1), so
// such a state is as whole as that merge, where documentThrough's document, which nothing encodes,
// holds what the runs applied. A log whose runs do not apply in order either, parking past the
// pending queue, makes no state: stateThrough answers it as an unappliedHistory, which is
// ErrDocumentUnloadable, carrying what the rebuild that error offers reads. A merge of such runs
// is no stand-in for them. Twenty keystrokes with the parked row stored after the first three,
// then another client's rows that park past the queue until a later row of that client's arrives,
// merged into bytes a room's load decoded holding three of the twenty keystrokes, under ygo
// v1.55.1-theorem.3 and with the fix of its parked merge ported onto it alike; with the parked row
// stored after all twenty, three under v1.55.1-theorem.3 and all twenty with the fix
// (TestAHistoryWhoseRunsDoNotApplyInOrderIsUnloadableAndItsRebuildRetiresItsClients).
//
// A log of one update is returned as stored: a compaction leaves the rows it folds as one update
// holding their state, and a document's first update deletes nothing an earlier one inserted.
func stateThrough(ctx context.Context, room string, version persistence.Version, updates updateSource, maxPending int) ([]byte, error) {
	state, _, _, misread, err := fold(ctx, updates, maxPending)
	if err != nil {
		return nil, err
	}
	if misread == nil {
		return state, nil
	}
	slog.Warn(misreadWarning+"; serving them applied in order",
		"room", room, "version", int64(version), "error", misread)
	runs, err := runsThrough(updates)
	if err != nil {
		return nil, err
	}
	return runsState(ctx, version, runs, maxPending)
}

// runsState is the state runs, a log's stored updates through version as runsThrough cuts them,
// make applied in order, as one V1 update: a single run as it is, and several applied to one
// document (runsDocument) and encoded, under the load grant ctx carries. Several runs that do not
// apply (ErrDocumentUnloadable) make no state, and are answered as an unappliedHistory.
func runsState(ctx context.Context, version persistence.Version, runs [][]byte, maxPending int) ([]byte, error) {
	if len(runs) == 1 {
		return runs[0], nil
	}
	doc, err := runsDocument(ctx, runs, maxPending)
	switch {
	case errors.Is(err, ErrDocumentUnloadable):
		return nil, newUnappliedHistory(version, runs, err)
	case err != nil:
		return nil, err
	}
	return crdt.EncodeStateAsUpdateV1(doc, nil), nil
}

// unappliedHistory is a stored history whose runs do not apply in order to one document of the
// store's pending queue (runsDocument, refused): it makes no state, so Load and MaterializeAt
// answer it as this error, which is ErrDocumentUnloadable, and no bytes. A socket's admission
// (loadTree) and a cold write's fork (coldFork) answer it as the document's own failure, as they
// answer a history ygo refuses the bytes of, and a room's load fails on it (LoadDoc). What the
// rebuild ErrDocumentUnloadable offers reads of a history (rebuildSource) it carries itself: the
// version the history stands at, and every client its runs name at the clock each reached, which
// for a history ygo refuses the bytes of is read from those bytes (undecodableHistoryClients).
type unappliedHistory struct {
	version persistence.Version
	clients map[crdt.ClientID]uint64
	refused error
}

// newUnappliedHistory is the unappliedHistory through version whose runs runsDocument refused.
func newUnappliedHistory(version persistence.Version, runs [][]byte, refused error) error {
	clients := make(map[crdt.ClientID]uint64)
	for _, run := range runs {
		_, to, err := crdt.ParseUpdateMeta(run)
		if err != nil {
			return fmt.Errorf("read the clocks of a stored update: %w", err)
		}
		for client, clock := range to {
			clients[client] = max(clients[client], clock)
		}
	}
	return &unappliedHistory{version: version, clients: clients, refused: refused}
}

func (h *unappliedHistory) Error() string { return h.refused.Error() }

func (h *unappliedHistory) Unwrap() error { return h.refused }

// documentThrough is the document the room's stored updates through version make, as stateThrough
// reads them, decoded once: the document the fold's read-back check decoded when the state reads
// back; a log of one update, which the fold returns as stored, decoded into a document with the
// store's pending queue, maxPending, as a room decodes it; and the runs of a log that does not fold
// (runsThrough) applied in order to such a document (runsDocument) - the document whose encoding
// stateThrough returns for a log of several runs, which holds what the runs applied even where
// that encoding keeps a parked struct over them. Each decode runs under the load grant ctx carries
// or one of its own (runsDocument). A history that does not decode is ErrDocumentUnloadable; a log
// holding no update is no document.
func documentThrough(ctx context.Context, room string, version persistence.Version, updates updateSource, maxPending int) (*crdt.Doc, error) {
	state, readBack, _, misread, err := fold(ctx, updates, maxPending)
	switch {
	case err != nil:
		return nil, err
	case readBack != nil:
		return readBack, nil
	case misread == nil && len(state) == 0:
		return nil, nil
	case misread == nil:
		return runsDocument(ctx, [][]byte{state}, maxPending)
	}
	slog.Warn(misreadWarning+"; serving them applied in order",
		"room", room, "version", int64(version), "error", misread)
	runs, err := runsThrough(updates)
	if err != nil {
		return nil, err
	}
	return runsDocument(ctx, runs, maxPending)
}

// runsThrough is the stored updates updates replays, in the order they were stored, as the runs
// a document applies them in (mergedRuns, which a catch-up applies its rows by): a run ends before
// a row carrying some client's clocks below the end of an earlier row of the run, and each run is
// its rows merged, which within a run keeps every struct whole. Rows typed one after another are
// one run.
func runsThrough(updates updateSource) ([][]byte, error) {
	var stored [][]byte
	if err := updates(func(update []byte) error {
		stored = append(stored, update)
		return nil
	}); err != nil {
		return nil, err
	}
	runs, err := mergedRuns(stored)
	if err != nil {
		return nil, fmt.Errorf("merge document updates into runs: %w", err)
	}
	return runs, nil
}

// runsDocument is runs applied in order to one document of maxPending pending items, as a room
// decodes its load (decodeStoredState), under the load grant ctx carries or one of its own weighed
// on the runs' bytes (holdLoad, copyWeight): the structs they hold are not known until this decode
// counts them, so the state term alone weighs the grant. A run that does not decode is
// ErrDocumentUnloadable.
func runsDocument(ctx context.Context, runs [][]byte, maxPending int) (*crdt.Doc, error) {
	var bytes int64
	for _, run := range runs {
		bytes += int64(len(run))
	}
	release, err := holdLoad(ctx, copyWeight(bytes, 0))
	if err != nil {
		return nil, err
	}
	defer release()
	doc := newCappedDocumentCopy(maxPending)
	for _, run := range runs {
		if err := decodeStoredState(doc, run); err != nil {
			return nil, fmt.Errorf("%w: decode live document: %w", ErrDocumentUnloadable, err)
		}
	}
	return doc, nil
}

// foldBufferBytes is the most stored update bytes a fold reads into memory (foldRead): a load or a
// compaction whose rows total at most this reads them, ends its read transaction and folds with no
// connection held, and one past it folds while it streams them inside that transaction, one row at
// a time. A fold holds its document beside the rows either way, so this bounds what reading the
// rows first adds to it.
const foldBufferBytes = 32 << 20

// bufferedFolds is how many folds of rows read into memory (foldRead) run at once in a process,
// across its room loads, cold reads and compactions (PgVersioned.foldSlots): each holds up to
// foldBufferBytes of rows beside its fold's document and the read-back's, so a process holds at most
// bufferedFolds × foldBufferBytes of buffered rows. It is the rooms pool's size
// (store.RoomsPoolSize): a load used to fold inside its transaction on that pool, whose connections
// bounded how many folds ran at once, and a buffered fold that has ended its transaction holds no
// connection to bound it.
const bufferedFolds = store.RoomsPoolSize

// foldRead hands use the room's stored updates through version, the rows of them tx reads, which
// number rows and total bytes, and ends tx, committing it as what names. Rows within foldBuffer
// are read into memory and tx commits before use runs, so a fold of them holds no connection while
// it works; past it use streams them inside tx (storedUpdates), which commits once use returns, so
// a fold of a log however large holds one row at once beside its document. Either source replays
// the rows as often as use asks (updateSource), from one snapshot.
//
// A buffered fold first takes one of the process's foldSlots, waiting within ctx, and gives it back
// once use returns, so at most bufferedFolds hold their rows at once. The wait cannot close a
// cycle: a waiter holds tx's connection, of the rooms pool or the compaction pool, but a holder of
// a slot waits for nothing that waits for a slot - it holds only its own transaction's connection
// while it reads the rows and no connection at all while it folds them, and the one thing it does
// wait for, the load slot below, has no holder that takes a fold slot - so every holder gives
// its fold slot back.
//
// A log of more than one update is read and folded under one grant of the pod's load concurrency
// (loadSlots), the grant ctx carries - a room load's, whose fold is its first decode - or one of
// this read's own, taken on the rows' own bytes, which is what the state they fold into is built
// from, and given back as it returns; use takes that ctx, so the fold's own decodes run inside
// the same grant (fold, documentThrough). A log of one update takes nothing of its own: it is
// returned as stored, and whoever decodes it takes the grant (documentThrough). The buffered
// fold's slot is taken before the load grant, never the other way about, which is what keeps
// that one wait out of a cycle (podLoadSlots).
func (p *PgVersioned) foldRead(ctx context.Context, tx pgx.Tx, room string, version persistence.Version, rows int, bytes int64, what string, use func(context.Context, updateSource) error) error {
	ctx, slot, release := withLoadSlot(ctx)
	defer release()
	commit := func() error {
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit %s: %w", what, err)
		}
		return nil
	}
	cover := func() error {
		if rows <= 1 {
			return nil
		}
		// The rows are bytes of a stored history nothing has decoded, so no struct count can be
		// read of them and the state term alone weighs the fold (copyWeight).
		return slot.take(ctx, copyWeight(bytes, 0))
	}
	if bytes > p.foldBuffer() {
		if err := cover(); err != nil {
			return err
		}
		if p.beforeFold != nil {
			p.beforeFold(room)
		}
		if err := use(ctx, storedUpdates(ctx, tx, room, version)); err != nil {
			return err
		}
		return commit()
	}
	select {
	case p.foldSlots <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-p.foldSlots }()
	if err := cover(); err != nil {
		return err
	}
	updates := make([][]byte, 0, rows)
	if err := eachUpdateThrough(ctx, tx, room, version, func(update []byte) error {
		updates = append(updates, update)
		return nil
	}); err != nil {
		return err
	}
	if err := commit(); err != nil {
		return err
	}
	if p.beforeFold != nil {
		p.beforeFold(room)
	}
	return use(ctx, memoryUpdates(ctx, updates))
}

// foldBuffer is the most stored update bytes foldRead reads into memory: foldBufferBytes, or the
// lower bound a test set (foldBufferLimit).
func (p *PgVersioned) foldBuffer() int64 {
	if p.foldBufferLimit != 0 {
		return p.foldBufferLimit
	}
	return foldBufferBytes
}

// storedLog is what a load reads of a room's stored updates before it reads the updates
// themselves, in one statement of its transaction: the stamp of the state at their head
// (readDocumentStamp), the head included, and how many rows stand at or below the head and their
// total bytes, which decide where the fold reads them from (foldRead).
type storedLog struct {
	stamp DocumentStamp
	rows  int
	bytes int64
}

// readStoredLog is the room's storedLog as tx reads it.
func readStoredLog(ctx context.Context, tx pgx.Tx, room string) (storedLog, error) {
	var stored storedLog
	if err := tx.QueryRow(ctx, `
		select s.version, s.row_xmin, r.updates, r.bytes
		from (`+documentStampSQL+`
		) s
		cross join lateral (
			select count(*) as updates, coalesce(sum(octet_length(d.update)), 0)::bigint as bytes
			from doc_updates d where d.artifact_id = $1 and d.version <= s.version
		) r
	`, room).Scan(&stored.stamp.Version, &stored.stamp.Row, &stored.rows, &stored.bytes); err != nil {
		return storedLog{}, fmt.Errorf("read the document's stored updates: %w", err)
	}
	return stored, nil
}

// errUnfoldable marks a stored update the fold's document could not apply.
var errUnfoldable = errors.New("apply a stored document update")

// updateSource hands use each update of a stored log, oldest first, every time it is called: the
// fold reads the log a second time when its document parked anything. It replays either the rows
// a transaction reads (storedUpdates) or rows already read into memory (memoryUpdates), so a load
// and a compaction fold one way whichever holds the rows.
type updateSource func(use func([]byte) error) error

// storedUpdates replays the room's stored updates through version, reading them one row at a time
// through q, the transaction or connection the caller is inside (eachUpdateThrough).
func storedUpdates(ctx context.Context, q Queryer, room string, version persistence.Version) updateSource {
	return func(use func([]byte) error) error {
		return eachUpdateThrough(ctx, q, room, version, use)
	}
}

// memoryUpdates replays updates, a log's rows already read, holding no connection. It stops with
// ctx's error before any update once ctx has ended, so a cold read's bound (coldReadTimeout) and
// Shutdown's deadline end a fold of rows in memory as they end one that streams them.
func memoryUpdates(ctx context.Context, updates [][]byte) updateSource {
	return func(use func([]byte) error) error {
		for _, update := range updates {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := use(update); err != nil {
				return err
			}
		}
		return nil
	}
}

// fold is the state the updates make, the document its read-back check decoded that state into,
// readBack, nil unless the state reads back, whether the fold's document parked anything once it
// had applied every update (parks), or misread naming why the updates do not fold into a state
// that reads back as them - an update the fold's document could not apply within its pending queue
// of maxPending items, or a state that reads back otherwise. A log of one update is returned as
// stored, with no document. The fold holds one document at once: its own, until it has encoded
// the state, then the read-back's.
//
// parked is the one case in which ygo's encoding of the fold's document merges anything
// (crdt.EncodeStateAsUpdateV1's parked structs and deletions) and the fold merges the stored
// updates back in, so it is the one fold whose state a change to how ygo merges parked structs can
// change: a compaction checks the stored versions inside such a fold before it writes it
// (disagreeingVersion).
// What the fold merges back of each stored update lies past the state vector of its document,
// which applied every update in order (crdt.DiffUpdateV1), with the update's deletes, so that
// merge decides nothing the document placed: only the structs it parks, which a document's own
// pending queue merges as they arrive.
//
// Whoever read the updates took the load grant ctx carries (foldRead), and the read-back's decode
// runs inside it. A fold under a context carrying no grant takes one of its own, weighed on the
// state it is about to read back, and given back as it returns (holdLoad).
func fold(ctx context.Context, updates updateSource, maxPending int) (state []byte, readBack *crdt.Doc, parked bool, misread error, err error) {
	var doc *crdt.Doc
	apply := func(update []byte) error {
		if doc == nil {
			doc = newCappedDocumentCopy(maxPending)
		}
		if err := decodeStoredState(doc, update); err != nil {
			return fmt.Errorf("%w: %w", errUnfoldable, err)
		}
		return nil
	}
	var first []byte
	seen := 0
	err = updates(func(update []byte) error {
		seen++
		switch seen {
		case 1:
			first = update
			return nil
		case 2:
			if err := apply(first); err != nil {
				return err
			}
			first = nil
		}
		return apply(update)
	})
	switch {
	case errors.Is(err, errUnfoldable):
		return nil, nil, false, err, nil
	case err != nil:
		return nil, nil, false, nil, err
	case seen <= 1:
		return first, nil, false, nil, nil
	}
	state = crdt.EncodeStateAsUpdateV1(doc, nil)
	made := doc.StateVector()
	if parked = parks(doc); parked {
		parts := [][]byte{state}
		if err := updates(func(update []byte) error {
			rest, err := crdt.DiffUpdateV1(update, made)
			if err != nil {
				return fmt.Errorf("read parked document update: %w", err)
			}
			parts = append(parts, rest)
			return nil
		}); err != nil {
			return nil, nil, false, nil, err
		}
		if state, err = crdt.MergeUpdatesV1(parts...); err != nil {
			return nil, nil, false, nil, fmt.Errorf("keep parked document updates: %w", err)
		}
	}
	doc = nil
	// Nothing has decoded the folded state yet, so the state term alone weighs this grant
	// (copyWeight).
	release, err := holdLoad(ctx, copyWeight(int64(len(state)), 0))
	if err != nil {
		return nil, nil, false, nil, err
	}
	defer release()
	readBack, misread = readsBackOtherwise(state, made, maxPending)
	return state, readBack, parked, misread, nil
}

// readsBackOtherwise decodes state into a fresh document of maxPending pending items and names how
// that document's state vector differs from made, the state vector of the document that encoded
// it. When it does not differ it returns the document, the state decoded as a room decodes it, and
// a nil error.
func readsBackOtherwise(state []byte, made crdt.StateVector, maxPending int) (*crdt.Doc, error) {
	doc := newCappedDocumentCopy(maxPending)
	if err := decodeStoredState(doc, state); err != nil {
		return nil, fmt.Errorf("decode the folded state: %w", err)
	}
	read := doc.StateVector()
	if maps.Equal(made, read) {
		return doc, nil
	}
	for client, clock := range made {
		if read[client] != clock {
			return nil, fmt.Errorf("client %d reads back at clock %d, the stored updates make %d", client, read[client], clock)
		}
	}
	for client, clock := range read {
		if made[client] != clock {
			return nil, fmt.Errorf("client %d reads back at clock %d, the stored updates make %d", client, clock, made[client])
		}
	}
	return doc, nil
}

// eachUpdateThrough calls use with each of the room's stored updates through version, oldest first,
// reading one row at a time through q.
func eachUpdateThrough(ctx context.Context, q Queryer, room string, version persistence.Version, use func([]byte) error) error {
	rows, err := q.Query(ctx, `
		select update from doc_updates
		where artifact_id = $1 and version <= $2
		order by version asc
	`, room, int64(version))
	if err != nil {
		return fmt.Errorf("read document updates: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var update []byte
		if err := rows.Scan(&update); err != nil {
			return fmt.Errorf("scan document update: %w", err)
		}
		if err := use(update); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read document updates: %w", err)
	}
	return nil
}

// LoadedDocument is a room's document as of one stored state: Doc decoded from it, nil for a
// document with no stored update, and Stamp naming it.
type LoadedDocument struct {
	Doc   *crdt.Doc
	Stamp DocumentStamp
}

// LoadDocument is the room's document at its head, decoded once, and the stamp of the state it was
// decoded from, read as Load reads (loadStored): in one repeatable-read transaction on the rooms
// pool, folded once that transaction has ended when the rows at or below the head total at most
// foldBufferBytes and while it streams them inside it otherwise (foldRead), and noted once it has
// read more than one row. Its document is the one Load's state decodes into (documentThrough), so
// a read of it shows what a room load would, save for a log that does not fold whose runs leave a
// struct parked over clocks the document holds: its document holds the rows the runs applied,
// whatever Load's encoding of it keeps (stateThrough). A history that does not decode is
// ErrDocumentUnloadable. Its fold and decode share one grant of the pod's load concurrency,
// weighed on the rows they fold, given back as it returns the document to its caller.
func (p *PgVersioned) LoadDocument(ctx context.Context, room string) (LoadedDocument, error) {
	ctx, _, done := withLoadSlot(ctx)
	defer done()
	var doc *crdt.Doc
	stamp, err := p.loadStored(ctx, room, func(ctx context.Context, head persistence.Version, updates updateSource) (err error) {
		doc, err = documentThrough(ctx, room, head, updates, p.maxPendingItems)
		return err
	})
	if err != nil {
		return LoadedDocument{}, err
	}
	return LoadedDocument{Doc: doc, Stamp: stamp}, nil
}

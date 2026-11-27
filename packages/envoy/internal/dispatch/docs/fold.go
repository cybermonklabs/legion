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
	"sync/atomic"

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
// is no stand-in for them. A log ygo will not even merge into runs is that same state, answered
// the same way (runsThrough). Twenty keystrokes with the parked row stored after the first three,
// then another client's rows that park past the queue until a later row of that client's arrives,
// merged into bytes a room's load decoded holding three of the twenty keystrokes, under
// cybermonklabs/ygo at 9c4cced4 and with the fix of its parked merge ported onto it alike; with
// the parked row stored after all twenty, three under 9c4cced4 and all twenty with the fix
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
	runs, err := runsThrough(version, updates)
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
// store's pending queue (runsDocument, refused), or whose rows ygo will not merge into runs at all
// (runsThrough): it makes no state, so Load and MaterializeAt answer it as this error, which is
// ErrDocumentUnloadable, and no bytes. A socket's admission (loadTree) and a cold write's fork
// (coldFork) answer it as the document's own failure, as they answer a history ygo refuses the
// bytes of, and a room's load fails on it (LoadDocContext). What the rebuild ErrDocumentUnloadable offers
// reads of a history (rebuildSource) it carries itself: the version the history stands at, and
// every client its runs name at the clock each reached, which for a history ygo refuses the bytes
// of is read from those bytes (undecodableHistoryClients).
type unappliedHistory struct {
	version persistence.Version
	clients map[crdt.ClientID]uint64
	refused error
}

// newUnappliedHistory is the unappliedHistory through version whose updates - the runs
// runsDocument refused, or the rows runsThrough could not merge into runs - ygo refused. An update
// whose header does not read retires no client, as undecodableHistoryClients reads one: no clock
// of it can be trusted, and the rebuild replaces the history either way.
func newUnappliedHistory(version persistence.Version, updates [][]byte, refused error) error {
	clients := make(map[crdt.ClientID]uint64)
	for _, update := range updates {
		for client, clock := range undecodableHistoryClients(update) {
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
// that encoding keeps a parked struct over them. Each decode runs under the load grant ctx carries,
// which each caller takes on the rows before the fold decodes any of them (foldRead,
// storedHistoryHolds), and which the read-back or the runs' decode takes more of where it weighs
// more, giving the grant back and taking its own weight whole where the pool does not give the
// difference at once (fold, runsDocument, holdUnbuiltLoad); under a context carrying none, the
// read-back and the runs' decode take grants of their own and the fold's own document is counted
// by none. A history that does not decode is ErrDocumentUnloadable; a log holding no update is no
// document.
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
	runs, err := runsThrough(version, updates)
	if err != nil {
		return nil, err
	}
	return runsDocument(ctx, runs, maxPending)
}

// runsThrough is the stored updates through version updates replays, in the order they were
// stored, as the runs a document applies them in (mergedRuns, which a catch-up applies its rows
// by): a run ends before a row carrying some client's clocks below the end of an earlier row of
// the run, and each run is its rows merged, which within a run keeps every struct whole. Rows
// typed one after another are one run.
//
// A history ygo will not merge does not decode either: it refuses a clock or a client its state
// encodings cannot carry wherever it reads one, so every read of this document meets the refusal.
// That is a history that does not load, the state a rebuild repairs, not a fault of the server
// this read can retry past, so it is an unappliedHistory, carrying the version and the clients
// that rebuild reads.
func runsThrough(version persistence.Version, updates updateSource) ([][]byte, error) {
	var stored [][]byte
	if err := updates(func(update []byte) error {
		stored = append(stored, update)
		return nil
	}); err != nil {
		return nil, err
	}
	runs, err := mergedRuns(stored)
	if err != nil {
		return nil, newUnappliedHistory(version, stored, fmt.Errorf("%w: merge document updates into runs: %w", ErrDocumentUnloadable, err))
	}
	return runs, nil
}

// runsDocument is runs applied in order to one document of maxPending pending items, as a room
// decodes its load (decodeStoredState), under the load grant ctx carries or one of its own weighed
// on the runs' bytes and the structs their headers declare (storedCopyWeight), read before this
// decode builds any of them. It runs where the fold before it built nothing it still holds, so on
// a grant its read holds alone it gives the grant back and takes that weight whole where the pool
// does not give the difference at once (holdUnbuiltLoad). A run that does not decode is
// ErrDocumentUnloadable.
func runsDocument(ctx context.Context, runs [][]byte, maxPending int) (*crdt.Doc, error) {
	release, err := holdUnbuiltLoad(ctx, storedCopyWeight(runs...))
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
// connection to bound it. A document load takes its slot before it knows whether its rows stream,
// and gives it back as soon as it knows they do: before it waits for the whole slot a streamed
// fold takes, and before a fold already on a whole slot streams them (loadStoredOnce).
const bufferedFolds = store.RoomsPoolSize

// foldRead hands use the room's stored updates through version, the rows of them tx reads, which
// number rows and total bytes, and ends tx, committing it as what names. A buffered read, which
// its caller makes of rows within foldBuffer while it holds one of the process's foldSlots
// (takeFoldSlot), reads them into memory and commits tx before use runs, so a fold of them holds
// no connection while it works; any other read has use stream them inside tx (storedUpdates),
// which commits once use returns, so a fold of a log however large holds one row at once beside
// its document. Either source replays the rows as often as use asks (updateSource), from one
// snapshot.
//
// The fold slot is the caller's, taken before tx began and given back once use has returned: a
// document load takes it before its rooms connection (loadStored), a compaction's read holding
// its compaction connection (readCompaction). So at most bufferedFolds hold their rows at once, and
// a holder of a slot holds only its own transaction's connection while it reads the rows and no
// connection at all while it waits for its load grant and folds them. Every holder of a load grant
// gives it back, so a holder of a slot gives it back unless its grant's wait is on a holder of the
// pool that waits for a slot, and none does (takeFoldSlot).
//
// A log of more than one update is folded under one grant of the pod's load concurrency
// (loadSlots), the grant ctx carries - a room load's, whose fold is its first decode, or a
// transaction's (withLoadSlot) - or one of this read's own, given back as it returns; use takes
// that ctx, so the fold's own decodes run inside the same grant (fold, documentThrough). Rows read
// into memory are weighed once they are in hand, before the fold decodes any of them: their bytes,
// which is what the state they fold into is built from, and the structs their headers declare
// (declaredStructs), which the fold's document is built of. What they hold is bounded by foldSlots
// outside the pool. Rows streamed are not in hand at once, so the grant is taken before they are
// read, on their bytes alone, which past foldBufferBytes already weigh within 5% of a whole slot,
// past which copyWeight weighs nothing more. A document load streams only on a grant holding one
// whole slot, taken before it holds its rooms connection at all, for every take the fold makes
// (loadStored), so the take here takes nothing more and no fold of a document load takes a grant
// holding a rooms connection (loadSlots); a compaction's fold takes it here, holding its
// compaction connection. A log of one update takes nothing of its own: it is returned as stored,
// and whoever decodes it takes the grant (documentThrough).
//
// A grant that held nothing as the read began is the read's alone, and ctx marks it so for use
// (ownedBy): the fold's read-back and the decode of its runs, which hold nothing they built as
// they take more of it, give it back and take their own weight whole where the pool does not give
// the difference at once (holdUnbuiltLoad). A grant held as the read begins - a transaction's,
// which its fork holds - takes more only where the pool gives it at once, and is refused busy
// otherwise (loadSlot.take, errGrantShort).
func (p *PgVersioned) foldRead(ctx context.Context, tx pgx.Tx, room string, version persistence.Version, rows int, bytes int64, what string, buffered bool, use func(context.Context, updateSource) error) error {
	ctx, slot, release := withLoadSlot(ctx)
	defer release()
	ctx = slot.ownedBy(ctx)
	commit := func() error {
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit %s: %w", what, err)
		}
		return nil
	}
	cover := func(updates [][]byte) error {
		if rows <= 1 {
			return nil
		}
		var structs int64
		for _, update := range updates {
			structs += int64(declaredStructs(update))
		}
		return slot.take(ctx, copyWeight(bytes, structs))
	}
	if !buffered {
		if err := cover(nil); err != nil {
			return err
		}
		noteRowsRead(ctx)
		if p.beforeFold != nil {
			p.beforeFold(room)
		}
		if err := use(ctx, storedUpdates(ctx, tx, room, version)); err != nil {
			return err
		}
		return commit()
	}
	noteRowsRead(ctx)
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
	if err := cover(updates); err != nil {
		return err
	}
	if p.beforeFold != nil {
		p.beforeFold(room)
	}
	return use(ctx, memoryUpdates(ctx, updates))
}

// rowsReadKey marks a context whose read notes the moment it begins to read a log's rows
// (watchRowsRead).
type rowsReadKey struct{}

// watchRowsRead is ctx marked so that the fold of a read under it notes the moment it begins to
// read the stored rows (noteRowsRead), and the flag that moment sets: a cold read's fold, which
// fails the room only where its fold had begun reading them (foldDocument). Every wait a read makes
// for what it takes - a slot of the buffered folds, its grant, its rooms connection - comes before
// that moment, but for the grant a fold of rows in memory takes once it holds them (foldRead) and
// the wait of a decode that gives its grant back and takes its weight whole (holdUnbuiltLoad),
// each of which holds nothing of the pool while it waits.
func watchRowsRead(ctx context.Context) (context.Context, *atomic.Bool) {
	read := new(atomic.Bool)
	return context.WithValue(ctx, rowsReadKey{}, read), read
}

// noteRowsRead sets the flag watchRowsRead put in ctx, if any: the read under ctx begins to read
// the stored rows, having waited for everything it takes before them.
func noteRowsRead(ctx context.Context) {
	if read, _ := ctx.Value(rowsReadKey{}).(*atomic.Bool); read != nil {
		read.Store(true)
	}
}

// takeFoldSlot takes one of the process's foldSlots for a read about to fold rows in memory
// (foldRead), before that read takes its connection, and returns what gives it back. A read whose
// grant of the pod's load concurrency holds bytes - a transaction's, which its fork holds, or a
// read's later pass under the total its first pass was refused (redoWhole) - takes one only where
// one is free at once, and is refused busy otherwise (heldGrant, errFoldSlotsBusy); a document
// load so refused streams its rows on one whole slot instead, where the pool gives that slot at
// once (loadStoredOnce). Only a read whose grant holds nothing waits for one, within ctx. A holder
// of a slot can wait for the pool - a buffered fold takes its grant once it holds its rows
// (foldRead) - so a holder of the pool that waited for a slot could wait on a holder that waits
// for the bytes it holds, until its deadline (loadSlots).
func (p *PgVersioned) takeFoldSlot(ctx context.Context) (func(), error) {
	if heldGrant(ctx) != nil {
		if giveBack := p.tryFoldSlot(); giveBack != nil {
			return giveBack, nil
		}
		return nil, errFoldSlotsBusy
	}
	select {
	case p.foldSlots <- struct{}{}:
		return func() { <-p.foldSlots }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// tryFoldSlot takes one of the process's foldSlots where one is free at once, and returns what
// gives it back, or nil where none is: what a read whose grant holds bytes takes (takeFoldSlot).
func (p *PgVersioned) tryFoldSlot() func() {
	select {
	case p.foldSlots <- struct{}{}:
		return func() { <-p.foldSlots }
	default:
		return nil
	}
}

// errFoldSlotsBusy refuses a read whose grant of the pod's load concurrency holds bytes where every
// slot of the buffered folds is taken (takeFoldSlot), and, for a document load, the pool does not
// give it one whole slot at once to stream its rows on instead (loadStoredOnce): such a read waits
// for no slot, since a holder of one can be waiting for the bytes it holds (loadSlots). It is
// ErrServiceUnavailable, the API's 503 DOC_SERVICE_UNAVAILABLE, which its caller retries.
var errFoldSlotsBusy = fmt.Errorf("%w: every slot of the buffered folds is taken, and a holder of the pod's load concurrency waits for none", ErrServiceUnavailable)

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
// Whoever read the updates took the load grant ctx carries (foldRead, storedHistoryHolds), and the
// read-back's decode runs inside it, taking more of it where the folded state weighs more than the
// rows did (storedCopyWeight): a delete inside a run splits it into structs no row declared. The
// fold's own document is dropped before that take, so on a grant its read holds alone a
// difference the pool does not give at once gives the grant back and takes the state's weight
// whole, waiting holding nothing of the pool, rather than queueing for more while it holds
// (holdUnbuiltLoad, loadSlots). A fold under a context carrying no grant takes one of its own for
// the read-back alone, weighed on the state it is about to read back, and given back as it
// returns.
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
	// The folded state is weighed by its bytes and the structs its headers declare
	// (storedCopyWeight), read before the read-back decodes any of them.
	release, err := holdUnbuiltLoad(ctx, storedCopyWeight(state))
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
// weighed on the rows they fold: the grant ctx carries, which its owner gives back, so a caller
// that carries one holds the document under it (storedWithUpdates, foldDocument), or one of its
// own, given back as it returns the document to its caller.
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

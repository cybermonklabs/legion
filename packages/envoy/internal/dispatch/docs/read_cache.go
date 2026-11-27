package docs

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strconv"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sync/singleflight"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// DocumentStamp names one stored state of a document: Version is the head Load folds the stored
// updates through (PgVersioned.head: a pending prune's ceiling, else the newest update's version,
// else 0, a document with no stored update), and Row is the id of the transaction that wrote the
// update row at that version, Postgres's xmin ("" where no row stands there).
//
// The version alone does not name a state: a prune deletes the updates past its target, and the
// next append takes the number the first of them had (appendUpdateTxClass), with other content.
// The row's transaction does: every writer of doc_updates either adds a row above the head (an
// append, a rebuild's seed), deletes every row (a rebuild, Delete), deletes rows above a target and
// leaves the target's row as it was (a prune), or rewrites one row with the state the rows it
// deletes below it made, which gives it a new transaction id over the same state (a compaction,
// compactRoom). The row a compaction rewrites is its fold point, which is the head row only when
// every row at or below the head is older than compactTail; a young head row keeps its xmin, and
// the stamp does not move. So two reads of one stamp read one state, on any task sharing the
// database. xmin is 32 bits and wraps: a false match needs a row re-appended at the same version
// exactly 2^32 transactions after the one it replaced, while a reading of the old row is still
// held.
type DocumentStamp struct {
	Version int64
	Row     string
}

// documentStampSQL selects the room $1's DocumentStamp, its version and then its row, in one
// statement, so the version and the xmin of the row standing there are of one snapshot: the head as
// head reads it (headVersion), then the row at it by its primary key. A load reads it as a subquery
// of its own statement (readStoredLog), so its stamp is this one.
const documentStampSQL = `
		select h.version, coalesce(u.xmin::text, '') as row_xmin
		from (select ` + headVersion + ` as version) h
		left join doc_updates u on u.artifact_id = $1 and u.version = h.version`

// readDocumentStamp is the room's DocumentStamp, read through q (documentStampSQL).
func readDocumentStamp(ctx context.Context, q Queryer, room string) (DocumentStamp, error) {
	var stamp DocumentStamp
	if err := q.QueryRow(ctx, documentStampSQL, room).Scan(&stamp.Version, &stamp.Row); err != nil {
		return DocumentStamp{}, fmt.Errorf("read document stamp: %w", err)
	}
	return stamp, nil
}

// documentReadBudget is the most weight (documentRead.weight) the cold reads' cache holds: 256 MiB
// in every process, whatever its Go memory limit (MemoryBounds.DocumentReadCache, which does not
// scale), and what boot logs as read_budget_bytes. It is also the figure this package's memory
// arithmetic is calibrated at: the task those figures were measured on has 4,096 MiB, and one
// request may raise the cache by at most 256 MiB (cmd/dispatch/memory_test.go). An entry heavier
// than an eighth of the budget is never stored.
const documentReadBudget = 256 << 20

// coldReadTimeout bounds a cold read's fold of the stored history (LoadDocument), which belongs to
// the document rather than to the request that started it, so no request's end cancels it.
var coldReadTimeout = 60 * time.Second

// documentRead is everything the four reads of a document no room holds answer (Text,
// TextWithToken, TextWithBlocks and BlockPath), rendered from one tree of one stored state, the
// state stamp names. Nothing writes it once it is built: a read hands out its strings, a copy of
// its blocks (a caller writes References into its slice) and a copy of a path's entries. A table
// block's DescendantIDs and Table, and a path's table position (its Row, Column, Header and Cells),
// are handed out as they are, shared with every later read of the same stored head, so no caller
// may write through them; none does, and copying them would cost every hit an allocation for a
// table's size.
type documentRead struct {
	artifactID string
	stamp      DocumentStamp
	markdown   string
	token      string
	blocks     []model.ArtifactBlock
	paths      map[string]pmdoc.BlockPath
	// nearDecodeCap is why the stored state stands near the decode cap, as the rebuild route's
	// own test reads it (nearTheDecodeCap), or "": answered from the document the fold decoded,
	// which no read has once the fold is done (NearDecodeCap).
	nearDecodeCap string
	weight        int64
}

// documentReads holds a documentRead for each document a cold read rendered, the most recently
// read first, while their weights together stay within budget: one entry for a document, a
// rendering of a newer stamp replacing the older. A read uses an entry only while the store's stamp
// is still its stamp, so an entry another task's write has moved past is never served.
type documentReads struct {
	mu      sync.Mutex
	entries map[string]*list.Element
	order   *list.List
	bytes   int64
	budget  int64
	// flights folds each (document, stamp) once however many reads miss it at once.
	flights singleflight.Group
}

func newDocumentReads(budget int64) *documentReads {
	return &documentReads{entries: make(map[string]*list.Element), order: list.New(), budget: budget}
}

// figures is the cache's weight, its entry count and its budget, for the room figures.
func (r *documentReads) figures() ReadCacheFigures {
	r.mu.Lock()
	defer r.mu.Unlock()
	return ReadCacheFigures{Bytes: r.bytes, Entries: len(r.entries), Budget: r.budget}
}

// get is artifactID's rendering when it was rendered from the state stamp names, which it marks the
// most recently read; nil otherwise.
func (r *documentReads) get(artifactID string, stamp DocumentStamp) *documentRead {
	r.mu.Lock()
	defer r.mu.Unlock()
	element, ok := r.entries[artifactID]
	if !ok {
		return nil
	}
	read := element.Value.(*documentRead)
	if read.stamp != stamp {
		return nil
	}
	r.order.MoveToFront(element)
	return read
}

// put stores read as its document's rendering in place of any it held, then drops the least
// recently read renderings until the cache's weight is within its budget. A rendering heavier than
// an eighth of the budget is not stored, and the one it replaces goes. put does not compare stamps:
// a slow fold of an older stamp can replace a newer rendering, which costs the next read a refold,
// never a stale read, since get serves an entry only at the stamp its caller just read.
func (r *documentReads) put(read *documentRead) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if element, ok := r.entries[read.artifactID]; ok {
		r.removeLocked(element)
	}
	if read.weight > r.budget/8 {
		return
	}
	r.entries[read.artifactID] = r.order.PushFront(read)
	r.bytes += read.weight
	for r.bytes > r.budget {
		r.removeLocked(r.order.Back())
	}
}

func (r *documentReads) removeLocked(element *list.Element) {
	read := r.order.Remove(element).(*documentRead)
	delete(r.entries, read.artifactID)
	r.bytes -= read.weight
}

// Overheads documentRead.weight counts beside the bytes its strings and slices hold: the entry's
// own struct with its list element and its slot in the cache's map; and a path's slot in the paths
// map, its key and value and the slot's control byte, at the load a map's table holds just after it
// grows (7/16 of its slots), so no map is counted as holding fewer slots than it allocated.
// TestADocumentReadWeighsAboutTheHeapItHolds (read_cache_weight_test.go) holds the weight they make
// within 0.97-1.25x of the heap a read holds. The LEGION-530 replay measured the cache's heap at
// 1.04-1.14x its weight under production's documents, at d91051ba, before a table's headers, its
// ordinals and a block's DescendantIDs were counted
// (dispatch://LEGION-530/artifact/replay-before-after-md).
const (
	readEntryOverhead = int64(unsafe.Sizeof(documentRead{})) + 128
	readPathOverhead  = (int64(unsafe.Sizeof("")+unsafe.Sizeof(pmdoc.BlockPath{})) + 1) * 16 / 7
)

// measure is the bytes the rendering's strings and slices hold, and its fixed overheads. A block id
// is counted where each part of the rendering holds it - a block, a path's key, a table's list of
// the blocks it holds - but not again on the path entries that name it. Storage that paths share
// is counted once however many share it (tablePositionsWeight), and so is a table block's rows
// (blockTableWeight).
func (read *documentRead) measure() int64 {
	weight := readEntryOverhead + int64(len(read.artifactID)+len(read.stamp.Row)+len(read.markdown)+len(read.token)+len(read.nearDecodeCap))
	weight += int64(cap(read.blocks)) * int64(unsafe.Sizeof(model.ArtifactBlock{}))
	tables := make(map[*model.BlockTable]struct{})
	for _, block := range read.blocks {
		weight += int64(len(block.ID)+len(block.Type)+len(block.Token)) + int64(cap(block.DescendantIDs))*int64(unsafe.Sizeof(""))
		for _, id := range block.DescendantIDs {
			weight += int64(len(id))
		}
		if block.Table != nil {
			if _, seen := tables[block.Table]; !seen {
				tables[block.Table] = struct{}{}
				weight += blockTableWeight(block.Table)
			}
		}
	}
	positions := make(map[*pmdoc.TablePosition]struct{})
	for id, path := range read.paths {
		weight += readPathOverhead + int64(len(id)+len(path.Type)) + int64(cap(path.Path))*int64(unsafe.Sizeof(pmdoc.BlockPathEntry{}))
		if path.Table != nil {
			positions[path.Table] = struct{}{}
		}
	}
	return weight + tablePositionsWeight(positions)
}

// blockTableWeight is the bytes a table block's rows hold beside the strings they name: a cell's
// id and opening words, and its header's (the header row's), are the strings the table's paths
// hold (pmdoc.BlockPathsAndTableRows lists them from the same layout), which a path's key and its
// table position count. A stored tree carries a block id on every row and cell, since settlement
// mints them before it persists.
func blockTableWeight(table *model.BlockTable) int64 {
	weight := int64(unsafe.Sizeof(model.BlockTable{})) + int64(cap(table.Rows))*int64(unsafe.Sizeof(model.BlockTableRow{}))
	for _, row := range table.Rows {
		weight += int64(cap(row.Cells)) * int64(unsafe.Sizeof(model.BlockTableCell{}))
	}
	return weight
}

// tablePositionsWeight is the bytes the distinct table positions hold, each once however many paths
// share it (every block in a cell shares its cell's), and the storage they point into, each once
// however many positions point at it (pmdoc.BlockPathsAndTableRows shares it within a table): a row's cells'
// opening words, a header's text and its slot in the table's array of headers, and the slots of the
// table's array of ordinals that Row and Column point at.
func tablePositionsWeight(positions map[*pmdoc.TablePosition]struct{}) int64 {
	weight := int64(len(positions)) * int64(unsafe.Sizeof(pmdoc.TablePosition{}))
	cells := make(map[*string]struct{})
	headers := make(map[*string]struct{})
	ordinals := make(map[*int]struct{})
	for position := range positions {
		if len(position.Cells) > 0 {
			if _, seen := cells[&position.Cells[0]]; !seen {
				cells[&position.Cells[0]] = struct{}{}
				weight += int64(cap(position.Cells)) * int64(unsafe.Sizeof(""))
				for _, cell := range position.Cells {
					weight += int64(len(cell))
				}
			}
		}
		if header := position.Header; header != nil {
			if _, seen := headers[header]; !seen {
				headers[header] = struct{}{}
				weight += int64(unsafe.Sizeof("")) + int64(len(*header))
			}
		}
		for _, ordinal := range [...]*int{position.Row, position.Column} {
			if ordinal == nil {
				continue
			}
			if _, seen := ordinals[ordinal]; !seen {
				ordinals[ordinal] = struct{}{}
				weight += int64(unsafe.Sizeof(0))
			}
		}
	}
	return weight
}

// buildDocumentRead renders tree, the document as of stamp, into everything the four reads answer,
// with one render, one placement walk, which lays each table out once for its paths and its rows,
// and one token encoding for the document and its blocks, none of which validates the tree again:
// its caller validated it once (pmdoc.ValidDocument). Its error is the one any of them gives when
// it refuses the tree, a document outside the schema, as the read computing it from the tree
// would: the placement walk's as it is, the descendants', a block token's or the rendering's
// through documentSchemaError (renderedBlocks), and the document token's as nodeToken returns it,
// after the rendering's.
func buildDocumentRead(artifactID string, stamp DocumentStamp, tree pmdoc.ValidDocument) (*documentRead, error) {
	paths, rows, err := tree.BlockPathsAndTableRows()
	if err != nil {
		return nil, err
	}
	rendered, err := renderedBlocks(tree, rows)
	if err != nil {
		return nil, err
	}
	if rendered.tokenErr != nil {
		return nil, rendered.tokenErr
	}
	read := &documentRead{artifactID: artifactID, stamp: stamp, markdown: rendered.markdown, token: rendered.token, blocks: rendered.blocks, paths: paths}
	read.weight = read.measure()
	return read, nil
}

// coldFold is a fold of a document's stored history: read, its rendering, which it stored; tree,
// the tree it was rendered from, which a read computes its answer from when read is nil because
// the tree is outside what the rendering takes; treeErr, why the stored document has no tree. All
// three are zero for a document with no stored state.
type coldFold struct {
	read    *documentRead
	tree    *pmdoc.Node
	treeErr error
}

// coldRead is the read of artifactID when neither a transaction's fork nor a resident room holds
// it: the rendering of the stored state the store's stamp names now, from the cache, or else from
// one fold of the history (foldDocument) that every read missing that state at once shares. It
// returns the rendering, or a nil rendering and the tree when the tree is outside what the
// rendering takes, or neither for a document with no stored state.
//
// The fold is the document's: it runs on a context no request's end cancels, bounded by the
// cache's timeout, so a read whose request ends gets its own context's error at once and leaves
// the fold to the others. Only the read's own context decides that it ended. The fold holds one
// grant of the pod's load concurrency from its load until its rendering is done (foldDocument). A
// fold that fails fails the room, as a failed room load does, and reaches each read as
// ErrServiceUnavailable, unless the history does not decode or parks past a room's cap
// (parkedPastRoomCaps), which is ErrDocumentUnloadable, the state a rebuild repairs.
//
// A read whose context carries a grant its caller holds - a transaction's, which the context Join
// returned carries, or a write's - folds on its own goroutine under that grant instead, sharing
// no fold (carriesLoadGrant): a grant serves its holder's goroutine alone, and a fold shared with
// other reads would take it on another goroutine, and, while the transaction holds a fork, for
// those reads too. It waits for its fold whatever its context does, within the cache's timeout.
// A fold refused busy fails no room, since the document is sound: one the pool refused - a
// transaction holding a fork takes more of its grant only where the pool gives it at once
// (loadSlots) - and one the cache's timeout ended before its fold had begun reading the rows, or
// while it waited for the pool holding no grant, having waited that long for the pool, a fold slot
// or a rooms connection (foldDocument). Each is ErrServiceUnavailable, the 503 its caller retries,
// and the room keeps its settlement.
func (s *Service) coldRead(ctx context.Context, artifactID string) (*documentRead, *pmdoc.Node, error) {
	stamp, err := s.persistence.DocumentStamp(ctx, artifactID)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, nil, ctxErr
		}
		s.failRoom(artifactID, err)
		return nil, nil, fmt.Errorf("%w: %w", ErrServiceUnavailable, err)
	}
	if stamp.Version == 0 {
		return nil, nil, nil
	}
	if read := s.reads.get(artifactID, stamp); read != nil {
		return read, nil, nil
	}
	fold := func() (any, error) {
		cold, err := s.foldDocument(ctx, artifactID)
		if err != nil && !RefusedBusy(err) {
			s.failRoom(artifactID, err)
		}
		return cold, err
	}
	var result singleflight.Result
	if carriesLoadGrant(ctx) {
		result.Val, result.Err = fold()
	} else {
		key := artifactID + "@" + strconv.FormatInt(stamp.Version, 10) + "/" + stamp.Row
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case result = <-s.reads.flights.DoChan(key, fold):
		}
	}
	if result.Err != nil {
		if errors.Is(result.Err, ErrDocumentUnloadable) {
			return nil, nil, result.Err
		}
		return nil, nil, fmt.Errorf("%w: %w", ErrServiceUnavailable, result.Err)
	}
	folded := result.Val.(coldFold)
	return folded.read, folded.tree, folded.treeErr
}

// foldDocument loads artifactID's stored history decoded once (LoadDocument), reads its tree and
// renders it (foldLoaded), storing the rendering under the stamp the load read in its own
// transaction, never the one read before the miss, so a write between the two cannot store old
// content under a new stamp. A history whose decode parks past a room's cap is no document a room
// can load, so it is ErrDocumentUnloadable here as a history ygo refuses the bytes of is, and
// logged (parkedPastCapReadLine): read under the service's cap alone, every read served it while
// every socket that opened it met its room's failed load.
// The load, the read of its tree, the rendering and the walk of it share one grant of the pod's
// load concurrency (withLoadSlot), held until the fold returns: the load takes it on the rows it
// folds and the state it decodes and leaves it held (LoadDocument), so the document it hands back
// is counted while foldLoaded reads, renders and walks it, and cold reads of different documents
// fold side by side only as far as the pool holds their documents. The load's waits - for a slot
// of the buffered folds, for its grant, for its rooms connection - are bounded by coldReadTimeout;
// what runs after the load holds the grant waiting for nothing, takes no connection and nothing
// more of the pool, and takes the cache's lock only to store the rendering. A load the timeout
// ends before its fold has begun reading the rows, or while its grant holds nothing, was waiting
// for what it takes - the pool, a slot of the buffered folds, a rooms connection - which a burst of
// cold reads holds, no fault of the document: it is refused busy, errGrantShort, which fails no
// room (coldRead, RefusedBusy). That holds for a load that already holds its grant too: a log
// past the fold buffer streams on one whole slot taken before its rooms connection (loadStored),
// and a read under a transaction's grant holds bytes from the start (carriesLoadGrant), and each
// can wait for one of the rooms pool's connections, every one of them held by another streamed
// load. Which side of that moment the timeout met is read off a flag the fold sets as it begins
// to read the rows (watchRowsRead, noteRowsRead); a timeout past it, while the fold reads and
// decodes holding its grant, is the fold's own, and fails the room as any error of the fold does.
// Its error is the load's, or a panic recovered from any of it: singleflight re-panics an
// unrecovered panic of a call it has waiters for in a goroutine nothing can recover, where a read
// panicking on the request's own goroutine would fail that one request.
func (s *Service) foldDocument(ctx context.Context, artifactID string) (fold coldFold, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("cold document read panicked: %v\n%s", recovered, debug.Stack())
			slog.Error("dispatch: cold document read panicked", "room", artifactID, "error", err)
		}
	}()
	foldCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), coldReadTimeout)
	defer cancel()
	foldCtx, rowsRead := watchRowsRead(foldCtx)
	foldCtx, slot, done := withLoadSlot(foldCtx)
	defer done()
	loaded, err := s.persistence.LoadDocument(foldCtx, artifactID)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) && errors.Is(foldCtx.Err(), context.DeadlineExceeded) {
			if !slot.held || !rowsRead.Load() {
				return coldFold{}, fmt.Errorf("%w: a cold document read waited %s for its turn to load the document", errGrantShort, coldReadTimeout)
			}
			return coldFold{}, fmt.Errorf("cold document read timed out after %s", coldReadTimeout)
		}
		return coldFold{}, err
	}
	if parked := s.parkedPastRoomCaps(loaded.Doc); parked != nil {
		return coldFold{}, refuseParkedRead(artifactID, parked)
	}
	// A rendering refused leaves the fold its tree, from which each read computes its own answer
	// and refusal, as it would without the cache.
	fold, _ = foldLoaded(artifactID, loaded)
	if fold.read != nil {
		s.reads.put(fold.read)
	}
	return fold, nil
}

// foldLoaded is the fold of loaded, a document its caller has just decoded and holds alone, still
// under the grant its load took (foldDocument): its tree read once and validated once
// (pmdoc.ReadUnshared) and rendered without validating it again (buildDocumentRead). A tree the
// read refuses is the fold's treeErr, the read's error through documentSchemaError as treeOf gives
// it; a tree the rendering refuses is the fold's tree without a read, and renderErr is the
// rendering's refusal. A loaded state without a document is the empty fold. A rendered fold also
// answers whether the state stands near the decode cap (documentRead.nearDecodeCap), from the
// decoded document's struct total and a floor under its whole-text count, a walk of the tree it
// rendered (wholeTextWriteFloor), which only the fold holds; it writes nothing and encodes nothing
// (NearDecodeCap).
func foldLoaded(artifactID string, loaded LoadedDocument) (fold coldFold, renderErr error) {
	if loaded.Doc == nil {
		return coldFold{}, nil
	}
	valid, err := pmdoc.ReadUnshared(loaded.Doc.GetXmlFragment(fragmentName))
	if err != nil {
		return coldFold{treeErr: documentSchemaError(err)}, nil
	}
	read, err := buildDocumentRead(artifactID, loaded.Stamp, valid)
	if err != nil {
		return coldFold{tree: valid.Node()}, err
	}
	read.nearDecodeCap, _ = nearTheDecodeCap(structCeiling(loaded.Doc), func() (uint64, bool) { return wholeTextWriteFloor(valid.Node()), true })
	read.weight += int64(len(read.nearDecodeCap))
	return coldFold{read: read, tree: valid.Node()}, nil
}

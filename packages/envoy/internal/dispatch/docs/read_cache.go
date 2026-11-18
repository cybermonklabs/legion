package docs

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
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

// documentRoomStampSQL selects, for the document $1, the room its document_rooms row names, null for
// a document that is its own room (documentRoomOfStatement reads the same row), beside the
// DocumentStamp of the stored history named $1 (documentStampSQL), in one statement, so a cold read
// of a document that is its own room learns both in one round trip (documentReadRoom).
const documentRoomStampSQL = `
		select (select moved.room::text from document_rooms moved where moved.artifact_id = $1),
			s.version, s.row_xmin
		from (` + documentStampSQL + `
		) s`

// readDocumentStamp is, read through q (documentRoomStampSQL), the room artifactID's document is
// edited in - its document_rooms row's room, else artifactID itself, spelled as the caller spelled
// it, as roomOf answers it - and the DocumentStamp of the stored history named artifactID, which is
// that room's stamp where the document is its own room.
func readDocumentStamp(ctx context.Context, q Queryer, artifactID string) (string, DocumentStamp, error) {
	var moved *string
	var stamp DocumentStamp
	if err := q.QueryRow(ctx, documentRoomStampSQL, artifactID).Scan(&moved, &stamp.Version, &stamp.Row); err != nil {
		return "", DocumentStamp{}, fmt.Errorf("read document stamp: %w", err)
	}
	if moved != nil {
		return *moved, stamp, nil
	}
	return artifactID, stamp, nil
}

// documentReadBudget is the most weight (documentRead.weight) the cold reads' cache holds in a
// process that runs with no Go memory limit, where nothing says how much memory it has: 256 MiB.
// An entry heavier than an eighth of the budget is never stored.
const documentReadBudget = 256 << 20

// readBudgetFor is the cold reads' cache budget for a process whose Go memory limit is memoryLimit
// (debug.SetMemoryLimit(-1), math.MaxInt64 when none is set): a sixteenth of the limit, so the
// cache follows the memory the process is given, at least 64 MiB, so the cache still stores a
// document of 8 MiB, and at most 1 GiB; documentReadBudget with no limit.
func readBudgetFor(memoryLimit int64) int64 {
	if memoryLimit == math.MaxInt64 {
		return documentReadBudget
	}
	return min(max(memoryLimit/16, 64<<20), 1<<30)
}

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
//
// tree is the tree it was rendered from, kept only for a document holding an ask block: the text
// an agent reads renders each ask's answer on it from the store (TextWithAnswers), which a read
// taking the cached rendering would otherwise have to decode the stored history again for. It is
// shared with every read of the stored head as the rest is, so no reader writes into it; the
// answers are rendered on a copy of the path to each block they change (withAskBlocks).
type documentRead struct {
	artifactID string
	stamp      DocumentStamp
	markdown   string
	token      string
	blocks     []model.ArtifactBlock
	paths      map[string]pmdoc.BlockPath
	tree       *pmdoc.Node
	weight     int64
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
// (blockTableWeight). A kept tree is counted whole (treeWeight).
func (read *documentRead) measure() int64 {
	weight := readEntryOverhead + int64(len(read.artifactID)+len(read.stamp.Row)+len(read.markdown)+len(read.token))
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
	return weight + tablePositionsWeight(positions) + treeWeight(read.tree)
}

// Overheads treeWeight counts: a node's struct; a mark's slot; a child's slot; an attribute map's
// header, and each group of eight slots its table holds, each slot a string key and an interface
// value, beside the group's control word; and the header of a string or a list an interface value
// boxes.
const (
	treeNodeOverhead  = int64(unsafe.Sizeof(pmdoc.Node{}))
	treeMarkOverhead  = int64(unsafe.Sizeof(pmdoc.Mark{}))
	treeChildOverhead = int64(unsafe.Sizeof((*pmdoc.Node)(nil)))
	treeMapOverhead   = 48
	treeGroupOverhead = 8 + 8*int64(unsafe.Sizeof("")+unsafe.Sizeof(any(nil)))
	treeStringBoxed   = int64(unsafe.Sizeof(""))
	treeListBoxed     = int64(unsafe.Sizeof([]any(nil)))
)

// treeWeight is the heap a kept tree holds (documentRead.tree): each node's struct, its type and
// its text, its marks and its children's slots, and its attributes and its marks' attributes
// (attrsWeight). A nil tree holds none.
func treeWeight(node *pmdoc.Node) int64 {
	if node == nil {
		return 0
	}
	weight := treeNodeOverhead + int64(len(node.Type)+len(node.Text)) + attrsWeight(node.Attrs)
	weight += int64(cap(node.Marks)) * treeMarkOverhead
	for _, mark := range node.Marks {
		weight += int64(len(mark.Type)) + attrsWeight(mark.Attrs)
	}
	weight += int64(cap(node.Children)) * treeChildOverhead
	for _, child := range node.Children {
		weight += treeWeight(child)
	}
	return weight
}

// attrsWeight is the heap an attribute map holds: its header and groups - one for up to eight
// attributes, and past that a table of a power of two slots at most 7/8 full - each key's bytes,
// and what each value boxes (boxedWeight).
func attrsWeight(attrs pmdoc.Attrs) int64 {
	if attrs == nil {
		return 0
	}
	slots := int64(8)
	for slots*7 < int64(len(attrs))*8 && len(attrs) > 8 {
		slots *= 2
	}
	weight := treeMapOverhead + slots/8*treeGroupOverhead
	for name, value := range attrs {
		weight += int64(len(name)) + boxedWeight(value)
	}
	return weight
}

// boxedWeight is what value, an attribute's, boxes beside its interface slot: a string's header
// and bytes, and a list's header, its slots and what each holds. A bool, a number or a nil boxes
// nothing Go allocates for a value that small.
func boxedWeight(value any) int64 {
	switch value := value.(type) {
	case string:
		return treeStringBoxed + int64(len(value))
	case []string:
		weight := treeListBoxed + int64(cap(value))*treeStringBoxed
		for _, item := range value {
			weight += int64(len(item))
		}
		return weight
	case []any:
		weight := treeListBoxed + int64(cap(value))*int64(unsafe.Sizeof(any(nil)))
		for _, item := range value {
			weight += boxedWeight(item)
		}
		return weight
	}
	return 0
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
	if pmdoc.HoldsAskBlock(tree.Node()) {
		read.tree = tree.Node()
	}
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
// rendering takes, or neither for a document with no stored state. room is the room artifactID's
// document is edited in (roomOf), whose stored history is folded, and fragment the fragment of it
// the document is held in (fragmentOf): the entry is the document's, at its room's stamp.
//
// The fold is the document's: it runs on a context no request's end cancels, bounded by the
// cache's timeout, so a read whose request ends gets its own context's error at once and leaves
// the fold to the others. Only the read's own context decides that it ended. A fold that fails
// fails the room, as a failed room load does, and reaches each read as ErrServiceUnavailable,
// unless the history does not decode, which is ErrDocumentUnloadable, the state a rebuild repairs.
func (s *Service) coldRead(ctx context.Context, artifactID, room, fragment string) (*documentRead, *pmdoc.Node, error) {
	_, stamp, err := s.persistence.DocumentStamp(ctx, room)
	if err != nil {
		return nil, nil, s.stampUnread(ctx, room, err)
	}
	return s.coldReadAt(ctx, artifactID, room, fragment, stamp)
}

// stampUnread is what a read answers for a stamp of room the store did not give it: the read's own
// context's error where that context ended, else the store's as ErrServiceUnavailable, after
// failing room, as a failed load does.
func (s *Service) stampUnread(ctx context.Context, room string, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	s.failRoom(room, err)
	return fmt.Errorf("%w: %w", ErrServiceUnavailable, err)
}

// coldReadAt is coldRead at stamp, the stamp of room its caller has just read, after finding that
// no room of this process holds the document (heldTree, documentReadRoom).
func (s *Service) coldReadAt(ctx context.Context, artifactID, room, fragment string, stamp DocumentStamp) (*documentRead, *pmdoc.Node, error) {
	if stamp.Version == 0 {
		return nil, nil, nil
	}
	if read := s.reads.get(artifactID, stamp); read != nil {
		return read, nil, nil
	}
	key := artifactID + "@" + strconv.FormatInt(stamp.Version, 10) + "/" + stamp.Row
	flight := s.reads.flights.DoChan(key, func() (any, error) {
		fold, err := s.foldDocument(ctx, artifactID, room, fragment)
		if err != nil {
			s.failRoom(room, err)
		}
		return fold, err
	})
	select {
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	case result := <-flight:
		if result.Err != nil {
			if errors.Is(result.Err, ErrDocumentUnloadable) {
				return nil, nil, result.Err
			}
			return nil, nil, fmt.Errorf("%w: %w", ErrServiceUnavailable, result.Err)
		}
		fold := result.Val.(coldFold)
		return fold.read, fold.tree, fold.treeErr
	}
}

// foldDocument loads room's stored history decoded once (LoadDocument), reads the tree of
// fragment, artifactID's fragment of it, and renders it (foldLoaded), storing the rendering under
// the stamp the load read in its own transaction, never the one read before the miss, so a write
// between the two cannot store old content under a new stamp.
// Its error is the load's, or a panic recovered from any of it: singleflight re-panics an
// unrecovered panic of a call it has waiters for in a goroutine nothing can recover, where a read
// panicking on the request's own goroutine would fail that one request.
func (s *Service) foldDocument(ctx context.Context, artifactID, room, fragment string) (fold coldFold, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("cold document read panicked: %v\n%s", recovered, debug.Stack())
			slog.Error("dispatch: cold document read panicked", "room", room, "error", err)
		}
	}()
	foldCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), coldReadTimeout)
	defer cancel()
	loaded, err := s.persistence.LoadDocument(foldCtx, room)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) && errors.Is(foldCtx.Err(), context.DeadlineExceeded) {
			return coldFold{}, fmt.Errorf("cold document read timed out after %s", coldReadTimeout)
		}
		return coldFold{}, err
	}
	// A rendering refused leaves the fold its tree, from which each read computes its own answer
	// and refusal, as it would without the cache.
	fold, _ = foldLoaded(artifactID, fragment, loaded)
	if fold.read != nil {
		s.reads.put(fold.read)
	}
	return fold, nil
}

// foldLoaded is the fold of fragment of loaded, a document its caller has just decoded and holds
// alone: its tree read once and validated once (pmdoc.ReadUnshared) and rendered without
// validating it again (buildDocumentRead), as artifactID's. A tree the read refuses is the fold's
// treeErr, the read's error through documentSchemaError as treeOf gives it; a tree the rendering
// refuses is the fold's tree without a read, and renderErr is the rendering's refusal. A loaded
// state without a document is the empty fold.
func foldLoaded(artifactID, fragment string, loaded LoadedDocument) (fold coldFold, renderErr error) {
	if loaded.Doc == nil {
		return coldFold{}, nil
	}
	held := loaded.Doc.GetXmlFragment(fragment)
	if fragment != fragmentName && held.Len() == 0 {
		// A converted document whose fragment of its issue room holds nothing has no tree.
		return coldFold{}, nil
	}
	valid, err := pmdoc.ReadUnshared(held)
	if err != nil {
		return coldFold{treeErr: documentSchemaError(err)}, nil
	}
	read, err := buildDocumentRead(artifactID, loaded.Stamp, valid)
	if err != nil {
		return coldFold{tree: valid.Node()}, err
	}
	return coldFold{read: read, tree: valid.Node()}, nil
}

package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/texttrees"
)

// storeDisplayTrees stores the display tree of every text a committed write stored, so a read of
// that text copies the stored bytes instead of reading the markdown again (texttrees). Every call
// stands right after the write's own commit, in the request that made it and before its response:
// never inside the transaction, because a tree that cannot be stored must never fail the write,
// and the read makes it then exactly as it does for a text whose row is not written yet.
//
// A failure is logged under subject - the issue, or the id of the row the texts were written for -
// and changes no answer. Nothing else is reported: the answer a reader is given is the same either
// way.
//
// Every write stores the latest mode, which is what a reader of a comment, a message, an ask or a
// document's head is shown. A past version is read in the version mode, which no write produces,
// so its tree is stored by the first read of it (getArtifactVersion) and by nothing else.
//
// The store itself is texttrees.StoreFor, the one policy every path that writes a stored text
// keeps, which is also why a caller still holding a pooled connection stores nothing.
func (s *server) storeDisplayTrees(ctx context.Context, subject string, texts ...string) {
	if err := texttrees.StoreFor(ctx, s.deps.Store, pmdoc.DisplayLatest, texts...); err != nil {
		slog.Warn("dispatch: store display trees failed", "subject", subject, "error", err)
	}
}

// displayTreeReader is one request's reading of the display trees its answer carries: every
// route that shows a stored text's tree has one, and no route reads a text for display on its
// own account.
//
// A request's shaping registers each tree field it is about to fill beside the text that fills
// it (text, textOf, textInto), hashing that text once; resolve then runs one query for the sums
// the process does not already hold, fills every registered field - from the stored bytes where
// the table had them and from a reading of the markdown where it did not - and answers the store
// of what was read, which the handler defers:
//
//	reader := s.readDisplayTrees(r.Context(), trees, issueKey)
//	rows := commentReads(comments, reader)
//	defer reader.resolve()()
//	WriteJSON(w, http.StatusOK, rows)
//
// What the handler defers is the hand-off and not the store: the store itself runs on a
// goroutine of the server's background work, once the request's own goroutine has let go of it,
// so no reader ever waits for an INSERT of a tree that is the next reader's saving and not its
// own (storeOf). A store that fails has changed no answer.
//
// A caller that asked for no tree has no reader at all: every method below answers at once on a
// nil receiver, so a route shapes its rows the one way whether or not the header was sent, and
// neither reads the table nor writes to it when it was not.
//
// Only a stored text is registered. A cut - a snippet, a first line, a thread subject - is one
// line of a longer stored text, which nothing stores a row for: it is read where it stands
// (displayTree), since registering it would ask the table for a text no writer stores and store
// a row no reader takes.
type displayTreeReader struct {
	// server is the server whose table the trees are read from and stored to, and nil for a
	// reader that reads every text for display itself: a write's own response, which answers the
	// text it has just stored the tree of, and a live frame, which has no request behind it.
	server  *server
	ctx     context.Context
	mode    pmdoc.DisplayMode
	subject string
	// mu guards the registrations alone. A list is shaped where it stands, so the lock is
	// uncontended on every route today; it is held because a shaper that fans out stays safe.
	mu    sync.Mutex
	texts []readDisplayText
	at    map[[32]byte]int
}

// readDisplayText is one distinct text of a request, the fields its tree fills, and that tree
// once resolve has it. A text registered twice - one body quoted in two rows - is one of these,
// so it is hashed once, asked for once and read once.
type readDisplayText struct {
	text   string
	sum    [32]byte
	tree   json.RawMessage
	fields []*json.RawMessage
	into   []func(json.RawMessage)
}

// readDisplayTrees is the reader of a request that answers stored texts in the latest mode,
// which is everything a write stores: a comment, a message, an ask, a broadcast, a document's
// head. subject is what a failed store is logged under - the issue, or the id of the row the
// texts belong to - and trees is what the caller asked for (wantsDisplayTrees), a false answering
// no reader at all.
func (s *server) readDisplayTrees(ctx context.Context, trees bool, subject string) *displayTreeReader {
	return s.readDisplayTreesIn(ctx, pmdoc.DisplayLatest, trees, subject)
}

// readVersionDisplayTrees is readDisplayTrees for a past version's text, which is read in the
// version mode because the attributes the server wrote into that text are part of what the
// version said. No write stores that mode, so the first read of a version stores its tree and
// every later read of it copies those bytes - the one place a read, rather than a write, is
// where a tree first reaches the table.
func (s *server) readVersionDisplayTrees(ctx context.Context, trees bool, subject string) *displayTreeReader {
	return s.readDisplayTreesIn(ctx, pmdoc.DisplayVersion, trees, subject)
}

func (s *server) readDisplayTreesIn(ctx context.Context, mode pmdoc.DisplayMode, trees bool, subject string) *displayTreeReader {
	if !trees {
		return nil
	}
	return &displayTreeReader{server: s, ctx: ctx, mode: mode, subject: subject}
}

// builtDisplayTrees is the reader of an answer whose texts are read where they stand: a write's
// own response, which carries the text the request has just written and stored the tree of, and
// a live frame, which is no request. It reads no row and stores none - the write beside it does
// both - and is the same reader otherwise, so one shaping serves a read and a write alike.
func builtDisplayTrees(trees bool) *displayTreeReader {
	if !trees {
		return nil
	}
	return &displayTreeReader{mode: pmdoc.DisplayLatest}
}

// text registers field as the tree of markdown, to be filled by resolve. An empty text has no
// tree and no row (pmdoc.DisplayJSON), so it is neither asked for nor read: its field stays nil,
// which is the field left out of the answer.
//
// field must stand where it will still stand when resolve runs - a row of a slice the shaping no
// longer appends to, or a value on the heap - because what is filled is the field this points
// at and not a copy of it.
func (r *displayTreeReader) text(field *json.RawMessage, markdown string) {
	if r == nil || markdown == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	slot := r.slotOf(markdown)
	slot.fields = append(slot.fields, field)
}

// textOf is text for a text a row may not hold at all.
func (r *displayTreeReader) textOf(field *json.RawMessage, markdown *string) {
	if markdown == nil {
		return
	}
	r.text(field, *markdown)
}

// textInto registers assign to be given the tree of markdown, for a tree that stands somewhere
// no field can be pointed at: an event's trees are a map keyed by the payload path, whose values
// have no address. assign is called once, by resolve, and never for a text with no tree.
func (r *displayTreeReader) textInto(assign func(json.RawMessage), markdown string) {
	if r == nil || markdown == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	slot := r.slotOf(markdown)
	slot.into = append(slot.into, assign)
}

// slotOf is markdown's own slot, made on its first registration. The caller holds the lock, and
// the pointer is used before the next registration can move the slice.
func (r *displayTreeReader) slotOf(markdown string) *readDisplayText {
	sum := pmdoc.TextSum(markdown)
	if index, held := r.at[sum]; held {
		return &r.texts[index]
	}
	if r.at == nil {
		r.at = make(map[[32]byte]int, 8)
	}
	r.at[sum] = len(r.texts)
	r.texts = append(r.texts, readDisplayText{text: markdown, sum: sum})
	return &r.texts[len(r.texts)-1]
}

// resolve fills every registered field and answers the hand-off of the store of the texts the
// table held no tree for, which the handler defers and which runs that store on a goroutine of
// its own, after the answer (storeOf).
//
// It is three steps over the texts registered, each of them once over the whole request:
//
//   - The trees this process already holds are taken from its display cache, by the sum the text
//     was registered under (pmdoc.HeldDisplayTree). One new comment moves both lists' validators,
//     so the read after a write is this case and not the restart: 199 of the 200 bodies of a feed
//     are where the comment list left them. Such a text is neither asked for nor stored, which
//     costs this read learning whether the table holds its row - a tree this process built and
//     failed to store waits for the fill pass (texttreefill) or a restart - and saves it the only
//     query it would otherwise make.
//   - The rest are one query, however many texts the answer carries: a comment list of two
//     hundred bodies after a restart is one round trip, not one per body and not two hundred
//     markdown parses. What it answers is put in the display cache as well as into the fields, so
//     the next request for the same text asks nothing.
//   - What the table did not hold is read for display, on as many processors as the process may
//     use, exactly as every read did before there was a table, and stored afterwards on a
//     connection of its own and outside any transaction of the caller's.
//
// A caller still holding a pooled connection reads nothing from the table and stores nothing: a
// document load and a compaction shape their answers inside read-only transactions
// (docs/persistence.go, docs/compaction.go), and a second connection taken while one is held is
// the deadlock store.ErrNestedAcquire refuses. The reader then builds every tree, which is the
// one thing it is always allowed to fall back to. A failed query costs the same and no answer.
func (r *displayTreeReader) resolve() func() {
	if r == nil || len(r.texts) == 0 {
		return func() {}
	}
	ask := make([][32]byte, 0, len(r.texts))
	for index := range r.texts {
		if tree, held := pmdoc.HeldDisplayTree(r.texts[index].sum, r.mode); held {
			r.texts[index].tree = tree
			continue
		}
		ask = append(ask, r.texts[index].sum)
	}
	stored := r.storedTrees(ask)
	missing := make([]int, 0, len(ask))
	for index := range r.texts {
		if r.texts[index].tree != nil {
			continue
		}
		if tree, held := stored[r.texts[index].sum]; held {
			r.texts[index].tree = tree
			pmdoc.PutStoredDisplayTree(r.texts[index].sum, r.mode, tree)
			continue
		}
		missing = append(missing, index)
	}
	buildDisplayTrees(len(missing), func(at int) {
		slot := &r.texts[missing[at]]
		slot.tree = builtDisplayTree(slot.sum, slot.text, r.mode)
	})
	for index := range r.texts {
		tree := r.texts[index].tree
		if tree == nil {
			continue
		}
		for _, field := range r.texts[index].fields {
			*field = tree
		}
		for _, assign := range r.texts[index].into {
			assign(tree)
		}
	}
	return r.storeOf(missing)
}

// storedTrees is the table's tree of each of sums, and nothing at all where there is no table to
// ask, where a connection of the caller's is already held, or where the query failed - each of
// which leaves every text to be read for display, as it was before there was a table.
func (r *displayTreeReader) storedTrees(sums [][32]byte) map[[32]byte]json.RawMessage {
	if r.server == nil || len(sums) == 0 || store.HoldsPooledConnection(r.ctx) {
		return nil
	}
	stored, err := texttrees.For(r.server.deps.Store).Read(r.ctx, r.mode, sums)
	if err != nil {
		slog.Warn("dispatch: read display trees failed", "texts", len(sums), "error", err)
		return nil
	}
	return stored
}

// storeOf is the store of the trees the request read for itself, and nothing at all for a reader
// that reads no table, that missed on nothing, or that holds a pooled connection of its own. The
// trees are stored as they were read: the texts are neither hashed nor read a second time.
//
// What the handler's defer runs is the hand-off alone. The store is a goroutine of the server's
// background work (backgroundWork.goDetached), which shutdown waits for as it waits for a
// broadcast's delivery, because the handler's defer stands before compressAnswers calls finish()
// and before net/http writes the buffered tail and the chunked terminator: a store run there is
// an INSERT a cold reader with misses waits out, after its answer is built and before it has
// every byte of it.
//
// It runs on the request's context detached from its cancellation (context.WithoutCancel), since
// a reader who goes away must not leave the saving to the next one, and bounded by
// storeTreesTimeout, which is what bounds it once cancellation no longer does. The hold mark is
// the goroutine's own (store.ForConcurrentWorker), as the fill's connections are marked and as
// every other detached run of this server is: the request's mark is still in use when the
// handler returns - keyedWrites settles the Idempotency-Key on it - and the pool refuses a
// second connection under a mark that already holds one (store.ErrNestedAcquire).
//
// A caller still holding a pooled connection stores nothing, which is decided here, while that
// connection is still held: a document load and a compaction shape their answers inside
// read-only transactions (docs/persistence.go, docs/compaction.go), and the fresh mark the
// goroutine takes cannot see the connection those hold.
func (r *displayTreeReader) storeOf(missing []int) func() {
	if r.server == nil || len(missing) == 0 || store.HoldsPooledConnection(r.ctx) {
		return func() {}
	}
	built := make([]texttrees.Tree, 0, len(missing))
	for _, index := range missing {
		if r.texts[index].tree == nil {
			continue
		}
		built = append(built, texttrees.Tree{Sum: r.texts[index].sum, Tree: r.texts[index].tree})
	}
	if len(built) == 0 {
		return func() {}
	}
	storing := store.ForConcurrentWorker(context.WithoutCancel(r.ctx))
	return func() {
		r.server.detached.goDetached(func() {
			ctx, cancel := context.WithTimeout(storing, storeTreesTimeout)
			defer cancel()
			if err := texttrees.StoreTreesFor(ctx, r.server.deps.Store, r.mode, built); err != nil {
				slog.Warn("dispatch: store display trees failed",
					"subject", r.subject, "mode", int(r.mode), "error", err)
			}
		})
	}
}

// storeTreesTimeout bounds the store of a read's own trees, which runs detached from the request
// that read them and so from every cancellation that would otherwise end it. It is generous
// because the work is one INSERT of a page's trees and the only cost of losing it is the next
// reader's reading, and it is bounded at all because shutdown waits for this goroutine.
const storeTreesTimeout = 30 * time.Second

// storeEventDisplayTrees stores the display trees of every stored text the published events carry
// in their payloads, keyed as every other tree is by the text alone. The event log and a live
// frame answer those trees by path (eventDisplayTrees), and an event carries texts no write of a
// stored text would ever store: an ask edit's payload holds the question and option labels it
// replaced, which no row holds any more, and those are read on every reading of the ask's history.
//
// The texts are found by the reader the event lists use (eventPayloadTexts), so what is stored is
// exactly what a read of those events answers, field for field.
//
// It runs after the broker has the events: a live socket's frame is a reader waiting, and it is
// not held for a write whose only effect is to save that reader's next read a markdown parse.
func (s *server) storeEventDisplayTrees(ctx context.Context, events []model.Event) {
	var texts []string
	var subject string
	for _, event := range events {
		texts = eventDisplayTexts(texts, event)
		if subject == "" && len(texts) > 0 {
			subject = event.Type + " " + strconv.FormatInt(event.ID, 10)
		}
	}
	s.storeDisplayTrees(ctx, subject, texts...)
}

// eventDisplayTexts appends every stored text the events carry a tree for, found by the reader
// the shaping uses (eventPayloadTexts), so what the publish path stores is exactly what a read
// of those events answers.
func eventDisplayTexts(texts []string, events ...model.Event) []string {
	for _, event := range events {
		eventPayloadTexts(event, func(_, text string) { texts = append(texts, text) })
	}
	return texts
}

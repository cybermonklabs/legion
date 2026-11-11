// The first screen: the opening blocks of the document a browser address names, drawn into the
// page the server sends so a reader meets their document in the page's own bytes rather than
// after the editor has been downloaded, built and connected (dispatch://INFRA-341 R11). The
// draw itself is `pmdoc.FirstScreenHTML`, through `api.Background`'s FirstScreen; what is here
// is the page handler around it - which addresses carry a document, what the page may spend on
// one, and where the blocks go in the page. Its rows are `routes/first_screen_test.go`.
package routes

import (
	"bytes"
	"context"
	"html"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/api"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// firstScreenMarker is where index.html keeps the server's place for a document's opening
// blocks: inside the page's own `#first-screen` element, which sits after `#root` because
// `createRoot` clears everything inside the element it mounts into. A page that carries no
// document leaves the marker where it is, so a reader with no session, or at any other address,
// gets the build's own bytes.
const firstScreenMarker = "<!--first-screen-->"

// firstScreenElement opens the element the blocks are drawn in: the document, named for what it
// is, exactly as the page's own read-only preview names it
// (`web/src/features/doc/DocumentPreview.tsx`), so what reads either surface reads both and the
// page's preview can take these blocks as its own. It carries the address it was drawn for, so a
// page the reader has already navigated away from never hands its blocks to another document
// (`web/src/features/doc/first-screen.ts`).
func firstScreenElement(address string) string {
	return `<div role="document" aria-label="Document text" data-first-screen-address="` +
		html.EscapeString(address) + `">`
}

// defaultFirstScreenBudget is the longest the page may wait for a document before it sends the
// shell. The page is what the reader is waiting for; a document the server cannot draw in this
// long is one the page's own read will draw for them a moment later, and holding the page for it
// would cost every reader of that document the wait.
//
// A test names its own in place of it (`AppContextOptions.FirstScreenBudget`) rather than this
// being a variable a test reassigns: a row about what the page CARRIES asked for production's
// 300 ms, and on a machine busy with something else - four Go packages at once on two
// processors - the read lost that race and the server did the right thing, which the row then
// read as a failure. A verdict that moves with the machine's load is no verdict
// (dispatch://INFRA-341).
const defaultFirstScreenBudget = 300 * time.Millisecond

// documentPageCacheControl is what a page carrying a document may be kept as: nothing, by
// anything. The bytes are one reader's document, so no shared cache may hold them, and a browser
// that kept them would show one person's document to the next person to sign in on that machine.
const documentPageCacheControl = "private, no-store"

// servePageForDocument serves the dashboard page, with the opening blocks of the document the
// address names in it where there is one to draw inside the budget (dispatch://INFRA-341 R11). A
// page carrying a document is nobody's to keep and carries no validator at all: no ETag, and so
// no revalidation for an If-None-Match to answer, since a 304 would leave a browser showing the
// document it held before - another reader's, or one this reader may no longer read.
//
// The budget bounds the whole draw, and it does it from outside the draw rather than through the
// draw's own context: the caller check waits on the sign-in issuer and the fold of a cold
// document takes no context at all, so a deadline the steps between them watch for bounded
// neither, and a page could be held for either and then ship the document after its budget was
// spent. The draw runs in a goroutine beside the page's own read, under a budget counted from
// the moment it starts; the budget wins the select, and what the reader gets then is the static
// shell, which is what a refused read gets.
//
// What the draw is given is a detached context (context.WithoutCancel) with a deadline of its
// own over it (firstScreenDrawCap): a context cancelled under a running query makes pgx close
// the connection it was on (outbox/lease.go), so a slow database would cost a pooled connection
// for every page view that gave up on it, but a draw nothing at all can end is worse - it holds
// a rooms-pool connection and a goroutine for the life of the process, and these goroutines are
// registered with nothing that waits for them (api/server.go's Background.Wait). Nothing of a
// draw that arrives after the budget reaches the reader, and a draw that outlives its handler
// reads nothing of the request but the headers its caller check reads the session cookie from,
// which no answer takes away.
func (r *router) servePageForDocument(w http.ResponseWriter, req *http.Request, path, normalized string) {
	address, ok := documentAddress(req, normalized)
	if !ok || r.firstScreen == nil {
		servePage(w, req, path)
		return
	}
	budget := r.ctx.firstScreenBudget
	if budget <= 0 {
		budget = defaultFirstScreenBudget
	}
	drawn, started := r.startDraw(req, address)
	if !started {
		servePage(w, req, path)
		return
	}
	spent := time.NewTimer(budget)
	defer spent.Stop()
	page, ok := readPage(w, path)
	if !ok {
		return
	}
	var screen api.FirstScreen
	select {
	case screen = <-drawn:
		// A select picks between two ready cases at random, so a draw that landed in the same
		// moment the budget ran out is held to the budget here rather than to that choice: past
		// it, the reader gets the shell whichever case the runtime took.
		select {
		case <-spent.C:
			screen = api.FirstScreen{}
		default:
		}
	case <-spent.C:
	}
	if !screen.Drawn {
		sendPage(w, req, path, page)
		return
	}
	marker := bytes.Index(page, []byte(firstScreenMarker))
	if marker < 0 {
		slog.Warn("dispatch: the dashboard page holds no first-screen marker", "path", path)
		sendPage(w, req, path, page)
		return
	}
	w.Header().Set("Content-Type", contentType(path))
	w.Header().Set("Cache-Control", documentPageCacheControl)
	// The drawn page writes its own bytes rather than going through http.ServeContent, which is
	// what sets this for the shell (sendPage), so the length is counted here: the page with the
	// marker taken out of it, and the element, the blocks and its closing tag put in. Without it
	// a HEAD of a drawn page answers no length at all, and the body is written before any
	// chunked answer could carry one.
	w.Header().Set("Content-Length", strconv.Itoa(
		len(page)-len(firstScreenMarker)+len(firstScreenElement(normalized))+len(screen.HTML)+len("</div>")))
	w.WriteHeader(http.StatusOK)
	if req.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(page[:marker])
	_, _ = io.WriteString(w, firstScreenElement(normalized))
	_, _ = io.WriteString(w, screen.HTML)
	_, _ = io.WriteString(w, "</div>")
	_, _ = w.Write(page[marker+len(firstScreenMarker):])
}

// firstScreenDrawCap is the longest a draw may run after the page has stopped waiting for it.
// The draw is detached from the request on purpose (servePageForDocument), so this deadline is
// the only thing that ends one: a document whose room has to be loaded queues on the rooms pool
// (store.RoomsPoolSize) and then folds its history, and a draw with nothing to end it waits
// there for ever on the connections the reader's own editor needs.
//
// Five seconds, which is both an order of magnitude above the 300 ms the page waits (so a draw
// that is merely slow still finishes, costs the pool no connection, and warms the room for the
// reader's own read a moment later) and far below the waits a draw can otherwise park in - the
// 15 s of identity.Membership's refresh, a failed room's recovery, an unbounded pool acquire.
const firstScreenDrawCap = 5 * time.Second

// firstScreenDrawSlots bounds the draws in flight across the whole process. A draw is work
// nobody is waiting for past the budget, and its heaviest step is a document load, which takes
// one of the RoomsPoolSize connections document loads share: without a cap, every page view of a
// slow document adds a goroutine queued on those four connections, and the queue outlives the
// readers it was for.
//
// Twice RoomsPoolSize, so each of those connections can have one draw using it and one waiting
// to - enough that the pool is never idle while draws are owed - and no deeper, because a draw
// that would be third in that queue is one the budget will beat anyway. A page view that finds
// no slot free sends the shell at once, which is what a refused draw sends and what the budget
// sends.
var firstScreenDrawSlots = make(chan struct{}, 2*store.RoomsPoolSize)

// startDraw starts the draw of address in a goroutine of its own and answers the channel its
// answer arrives on, or started=false where no slot is free and the page is to send the shell.
//
// The channel is buffered, so the draw that loses the race to the budget hands its answer over
// and ends rather than blocking on a reader nobody is left to send it to.
func (r *router) startDraw(req *http.Request, address api.FirstScreenAddress) (drawn chan api.FirstScreen, started bool) {
	select {
	case firstScreenDrawSlots <- struct{}{}:
	default:
		return nil, false
	}
	// Marked WithoutRecoveryWait, because a failed room's recovery is a wait the page has no use
	// for: the reader is sent the shell and their own editor meets the room's refusal with its
	// reason. Its deadline is the draw's own (firstScreenDrawCap), counted from here.
	drawing, endDraw := context.WithTimeout(context.WithoutCancel(req.Context()), firstScreenDrawCap)
	drawn = make(chan api.FirstScreen, 1)
	go func() {
		defer endDraw()
		defer func() { <-firstScreenDrawSlots }()
		drawn <- r.firstScreen(docs.WithoutRecoveryWait(drawing), req, address)
	}()
	return drawn, true
}

// documentAddress is the document a browser address opens, for the page handler. Four addresses
// open one: the issue's own spec at `/issues/{key}` and at `/issues/{key}/spec`, which a link to
// a block of the spec names, and the issue document at `/issues/{key}/artifacts/{slug}` and at
// `/issues/{key}/artifact/{slug}` - both spellings are written, and both are the same document.
// Nothing else carries a document into its page: a pinned version (`?v=`) is a different
// document than the one this draws, an agent's conversation document is read by another route,
// and every other address opens no document at all.
func documentAddress(req *http.Request, normalized string) (api.FirstScreenAddress, bool) {
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		return api.FirstScreenAddress{}, false
	}
	if req.URL.Query().Has("v") {
		return api.FirstScreenAddress{}, false
	}
	rest, found := strings.CutPrefix(normalized, "/issues/")
	if !found {
		return api.FirstScreenAddress{}, false
	}
	key, tail, hasTail := strings.Cut(rest, "/")
	if !text.IsIssueKey(key) {
		return api.FirstScreenAddress{}, false
	}
	if !hasTail || tail == "spec" {
		return api.FirstScreenAddress{IssueKey: key}, true
	}
	slug, found := strings.CutPrefix(tail, "artifacts/")
	if !found {
		slug, found = strings.CutPrefix(tail, "artifact/")
	}
	if !found || slug == "" || strings.Contains(slug, "/") {
		return api.FirstScreenAddress{}, false
	}
	return api.FirstScreenAddress{IssueKey: key, Slug: slug}, true
}

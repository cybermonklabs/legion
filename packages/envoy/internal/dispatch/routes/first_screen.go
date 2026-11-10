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
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/api"
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
// spent. The draw runs in a goroutine beside the page's own read; the budget wins the select, and
// what the reader gets then is the static shell, which is what a refused read gets.
//
// What the draw is given is a detached context (context.WithoutCancel): it is bounded by its own
// work and ends by itself, and a context cancelled under a running query makes pgx close the
// connection it was on (outbox/lease.go), so a slow database would cost a pooled connection for
// every page view that gave up on it. Nothing of a draw that arrives after the budget reaches
// the reader.
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
	drawing := context.WithoutCancel(req.Context())
	// Buffered, so the draw that loses the race to the budget hands its answer over and ends
	// rather than blocking on a reader nobody is left to send it to.
	drawn := make(chan api.FirstScreen, 1)
	go func() { drawn <- r.firstScreen(drawing, req, address) }()
	page, ok := readPage(w, path)
	if !ok {
		return
	}
	spent := time.NewTimer(budget)
	defer spent.Stop()
	var screen api.FirstScreen
	select {
	case screen = <-drawn:
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

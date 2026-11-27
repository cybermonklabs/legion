package api

import (
	"fmt"
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/reference-targets",
	Auth:        authAny,
	Description: fmt.Sprintf("What many dispatch:// references point at, in one read: ?ref=<dispatch ref> once per reference, at most %d of them (past that 400 CAP_EXCEEDED naming the cap). {targets}, one member per ?ref= in the request's order (a reference named twice answered twice), each {ref, kind, name, gone}: ref as the request wrote it, kind the kind of node it names, name the words shown for the target in place of its text (an issue's title, a document's name, an ask's question, a comment's or a message's body, a component's title, a block's text), and gone whether nothing answers to that reference now, once an id it writes short is resolved, which carries no name. A member naming a comment or a message, or a block of one, also carries edited_at where the post's author edited it, and deleted_at and deleted_by where its author deleted it: a deleted post's member is not gone and its name is \"\", and a member naming a block of it is gone, with no name and no text_tree. A caller that sent Dispatch-Trees: 1 is also given text_tree on each member whose kind is block: the block's whole text as the tree a reader is shown, links and references kept and nothing cut, a typed block's being of what stands between its opening line and its closing one. The lines between a typed block's fences are read by the block's kind: a document's block has the closing fence's indentation taken out of each line, which a block in a list item or a footnote definition carries, and a block of a comment, a message or an ask, stored from the start of its line, has its opening line's indentation, up to three spaces, taken off before the directive is read and up to as many columns taken off each later line, as its own text's parse reads them, the indentation left on a line, and the spaces or tabs after a list marker or a quote marker, written as spaces with each tab counted to the next multiple of four from its own column. With its opening line at column 0, such a block keeps every line as written; an opening line four spaces or a tab in opens no typed block, so that text is given the tree of its whole stored text. A typed block in a quote is given the tree of its whole stored text, its opening line included, and so is a block of a comment, a message or an ask whose closing fence stands four spaces or a tab in, since that fence closes nothing in its text. A reference the grammar cannot read is 400 INVALID_REFERENCE naming it, and so is a reference to a party (a person, an agent's session or a role), which the grammar reads but the reference graph holds no node for; any other reference that reads but names nothing is answered gone, never refused. An item id or a block id written short (8 characters or more, never a whole uuid) is resolved in the scope the reference names, as GET /api/v1/references resolves its ?to=, and ref still echoes it as written: a prefix that nothing carries is answered gone, and a prefix that two or more items carry answers that member gone with ambiguous: true, which no other member carries, and every other member is answered; GET /api/v1/references refuses such a prefix with 409 ID_AMBIGUOUS, and this read refuses none. Under an ETag over the whole answer, each tree and each ambiguous included (If-None-Match answers 304). This answers no edges: GET /api/v1/references reads one node's edges.", referenceTargetsMax),
	Handler:     (*server).getReferenceTargets,
	Successor:   successorIs(http.MethodGet, "/reference-targets"),
	Read:        readEntry{Writes: readWritesNothing},
	Shape: routeShape{
		Status: http.StatusOK, Response: model.ReferenceTargets{}, Params: referenceTargetsQuery{},
		// An If-None-Match that matches the answer's ETag is answered 304 and no body.
		Also: []routeAnswer{{Status: http.StatusNotModified}},
	},
})

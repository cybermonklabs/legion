package api

import (
	"net/http"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// storedMarkdown is text as the store would hold it: the parse and the render every write of a
// whole document runs over it (docs' SeedText and ReplaceText, pmdoc.ParseWritten over no live
// document, taken as an upload is taken, then pmdoc.Render), without writing anything. Nothing
// else of those writes is run here, since nothing else of them changes the markdown.
//
// Two callers read it. The near-copy gate compares a new spec with document_search.search, the
// vector of the markdown the seed stored, and a rendering differs from the text it was parsed
// from wherever the renderer normalises one: an ordered list is renumbered, and Postgres keeps a
// list's numbers as lexemes, so `1.` `1.` is stored as `1.` `2.` and an exact copy of that spec
// would hold different lexemes from the row it copies. POST /api/v1/markdown/round-trip answers
// it, so `dispatch artifact` refuses inline text the store would change and names `--path`.
//
// An empty answer is what a create seeds for a spec of whitespace alone. Markdown the parse or
// the render refuses is handed back as it came. The write that follows refuses it with the error
// of its own seed, which says what is wrong with it; both readers answer on the text they were
// given rather than failing a write the seed would have let through.
func storedMarkdown(text string) string {
	if strings.TrimSpace(text) == "" {
		return ""
	}
	tree, _, err := pmdoc.ParseWritten(text, nil)
	if err != nil {
		return text
	}
	tree = pmdoc.StripAnchorMarks(tree)
	pmdoc.StripServerOwnedAttrs(tree)
	stored, err := pmdoc.Render(tree)
	if err != nil {
		return text
	}
	return stored
}

// markdownRoundTripLimit is the most this route's request may hold, derived from the largest
// text it is ever asked about. That text is the one an inline upload is about to store, which
// is a markdown document of at most maxDocumentMarkdownBytes - the upload refuses a byte more
// (artifacts.go) - and it travels here as a JSON string, whose escaping can make each byte of
// it up to six on the wire (a client that writes `<` as \u003c, or a byte of a two-byte
// character as \u00e9), which is how the handoff write's own limit counts the same escaping
// (handoffRequestLimit). The kilobyte beside it carries the object around that string.
//
// A megabyte would refuse text the upload itself takes, since JSON adds a byte for every
// newline, quote, backslash and tab of a document already at the cap. `dispatch artifact`
// refuses the upload on every failure of this route but its absence, so such a writer would
// be told to send an upload again that no repeat could ever get through.
const markdownRoundTripLimit = 6*maxDocumentMarkdownBytes + 1<<10

// errMarkdownRoundTripTooLarge is this route's answer to a body past its limit, which the
// drain, the keyed layer and the handler all give alike (bodyTooLarge). It names the body's own
// bound and nothing else: the text a caller asks about is bounded by the write that would store
// it, which refuses its own, and a refusal here that named the text's bound would send a writer
// back to a call no repeat gets through.
var errMarkdownRoundTripTooLarge = errorf(http.StatusRequestEntityTooLarge, "CAP_EXCEEDED",
	"request body exceeds %d bytes", markdownRoundTripLimit)

// markdownRoundTripInput is the body of POST /api/v1/markdown/round-trip: the text a caller is
// about to store as a Markdown document.
type markdownRoundTripInput struct {
	Markdown string `json:"markdown" required:"true"`
}

// markdownRoundTripResponse is the markdown the store would hold for that text.
type markdownRoundTripResponse struct {
	Markdown string `json:"markdown"`
}

// postMarkdownRoundTrip answers POST /api/v1/markdown/round-trip. It writes nothing and reads no
// row: it parses the text and renders it again, as a document write does, and answers what that
// write would store. A caller compares the two and refuses its own call where they differ.
//
// The body is read within this route's own limit, not a JSON body's megabyte, so no text an
// inline upload would store is refused here (markdownRoundTripLimit).
//
// Text longer than a document may hold comes back unchanged, as storedMarkdown hands back text
// the parse or the render refuses: no write would store that text at all, so this route says
// nothing about its spelling and leaves the refusal to the write, which names the bound a
// writer can act on (artifacts.go). A caller comparing the two then finds them equal and makes
// its own call, rather than reading a refusal of ours that no repeat could get through.
func (s *server) postMarkdownRoundTrip(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	var input markdownRoundTripInput
	if err := decodeJSONWithin(r, &input, markdownRoundTripLimit, errMarkdownRoundTripTooLarge); err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	stored := input.Markdown
	if len(stored) <= maxDocumentMarkdownBytes {
		stored = storedMarkdown(stored)
	}
	WriteJSON(w, http.StatusOK, markdownRoundTripResponse{Markdown: stored})
}

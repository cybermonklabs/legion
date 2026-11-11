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
func (s *server) postMarkdownRoundTrip(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	var input markdownRoundTripInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, markdownRoundTripResponse{Markdown: storedMarkdown(input.Markdown)})
}

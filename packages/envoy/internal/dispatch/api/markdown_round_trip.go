package api

import (
	"net/http"
	"strings"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
)

// markdownRoundTrip is the route's answer to content, the text an upload would store as a new
// markdown document - inline, as a JSON upload's `content`, or with file, a file's bytes, as a
// multipart upload's file part: the refusal that upload makes of it for the text alone, with the
// scope that refusal holds for (contracts.TextRefusalHeader's value), or else the markdown it
// stores. The refusals are the upload's own, in its order, served as the upload serves them:
//
//   - documentTextRefusal's, which every upload of a markdown document makes before its write -
//     the text's encoding and its bytes - scoped contracts.TextRefusalUpload;
//   - then the seed's, docs.SeedRendering, the steps of SeedText that read the text alone: its
//     parse, which counts the elements the text makes and refuses markdown the schema does not
//     take, its asks' content rule, its render, and the weigh of that rendering against the bytes
//     and the elements one document holds. Only a new document is seeded: a replacement is held
//     to looser rules, judged against the document it replaces (docs' ReplaceText), so these are
//     scoped contracts.TextRefusalNewDocument.
//
// A text neither refuses is answered with the rendering the seed stores.
//
// Text `strings.TrimSpace` empties is answered "", as the near-copy gate's storedMarkdown answers
// it, once neither has refused it. That answer is not always what the upload stores: the seed
// stores spaces, tabs, carriage returns and line feeds alone as an empty document, but keeps every
// other character strings.TrimSpace empties, so an upload of "\u00a0\n" stores "\u00a0\n"
// (measured through the upload route on 2026-11-21). The route answers "" so that its answer to
// every text within the limits is the one it gave before it read the seed, and `dispatch artifact`
// takes only a refusal from it for such inline text (goBlankPattern) and for a file.
func markdownRoundTrip[T string | []byte](content T, file bool) (stored, scope string, err error) {
	if refusal := documentTextRefusal(content, file); refusal != nil {
		return "", contracts.TextRefusalUpload, refusal
	}
	text := string(content)
	seeded, err := docs.SeedRendering(text)
	if err != nil {
		return "", contracts.TextRefusalNewDocument, err
	}
	if strings.TrimSpace(text) == "" {
		return "", "", nil
	}
	return seeded, "", nil
}

// markdownRoundTripPattern is the route's pattern, and markdownRoundTripRoute that route: with
// the handoff write, one of the two JSON routes whose body limit is its own rather than
// maxJSONRequestBytes (routeBody).
const (
	markdownRoundTripPattern = "/api/v1/markdown/round-trip"
	markdownRoundTripRoute   = http.MethodPost + " " + markdownRoundTripPattern
)

// markdownRoundTripLimit is the most this route's request may hold, derived from the largest
// text it is ever asked about. That text is the one an upload is about to store as a markdown
// document, which is at most maxDocumentMarkdownBytes - the upload refuses a byte more, and so
// does this route (documentTextRefusal). Inline, it travels here as a JSON string, whose escaping
// can make each byte of it up to six on the wire (a client that writes `<` as \u003c, or a byte
// of a two-byte character as \u00e9), which is how the handoff write's own limit counts the same
// escaping (handoffRequestLimit); a file's bytes travel as base64, four bytes for every three, well
// within that. The kilobyte beside it carries the object around that string.
//
// A megabyte would refuse text the upload itself takes, since JSON adds a byte for every
// newline, quote, backslash and tab of a document already at the cap. This route's refusal of a
// body past its limit carries no contracts.TextRefusalHeader, so `dispatch artifact` reads it as a
// round trip that did not answer: the call refuses an upload of inline text that is not blank
// until the round trip answers, and its dry run passes with a warning, so such a writer would be
// refused an inline text the store takes.
const markdownRoundTripLimit = 6*maxDocumentMarkdownBytes + 1<<10

// errMarkdownRoundTripTooLarge is this route's answer to a body past its limit, which the
// drain, the keyed layer and the handler all give alike (routeBody). It names the body's own
// bound, which is applied before any text in the body is read. `dispatch artifact` sends no text
// past the bytes one document holds, which it refuses itself, so no body it sends is past it.
var errMarkdownRoundTripTooLarge = errorf(http.StatusRequestEntityTooLarge, "CAP_EXCEEDED",
	"request body exceeds %d bytes", markdownRoundTripLimit)

// errMarkdownRoundTripInput refuses a body that gives both the text and the file, or neither.
var errMarkdownRoundTripInput = errorf(http.StatusBadRequest, "ROUND_TRIP_INPUT",
	"give exactly one of markdown, the text an inline upload sends, and file, the bytes of a file an upload sends, as base64")

// markdownRoundTripInput is the body of POST /api/v1/markdown/round-trip: what a caller is about
// to store as a Markdown document, exactly one of two. Markdown is text, as an inline upload sends
// it as `content`. File is a file's bytes, base64 in the JSON, as a multipart upload sends them as
// its file part: they are checked as bytes, so a file holding a byte that is not UTF-8 is refused
// as its upload is, which no JSON string could carry.
type markdownRoundTripInput struct {
	Markdown *string `json:"markdown,omitempty"`
	File     []byte  `json:"file,omitempty"`
}

// markdownRoundTripResponse is the markdown the store would hold for that text.
type markdownRoundTripResponse struct {
	Markdown string `json:"markdown"`
}

// postMarkdownRoundTrip answers POST /api/v1/markdown/round-trip. It writes nothing and reads no
// row: it checks the text as an upload of a new markdown document checks it, parses it and
// renders it, as that upload's seed does, and answers what that seed would store
// (markdownRoundTrip). A caller compares the two and refuses its own call where they differ.
//
// Text an upload of a new markdown document refuses for the text alone is refused here with that
// upload's own status, code and words: text holding U+0000, or a file holding a byte that is not
// UTF-8; text past maxDocumentMarkdownBytes; markdown the parse refuses, a rendering no reader
// reads back among it; an ask that breaks its content rule; text, or its rendering, past the bytes
// or the elements one document holds; and any failure of the seed on the text, at whatever status
// the upload serves it. Each such refusal, whatever its status, carries
// contracts.TextRefusalHeader, valued with its scope (markdownRoundTrip): TextRefusalUpload for a
// refusal every upload of a markdown document makes, TextRefusalNewDocument for one only a new
// document's seed makes. No other answer of the route carries it. `dispatch artifact` and its dry
// run take an answer carrying it as the call's refusal where its scope holds for the call, so they
// refuse such text as the upload would, inline or from a markdown file, and a check the upload
// makes of the text alone before its write (documentTextRefusal) or in the steps of its seed that
// read the text alone (docs.SeedRendering) reaches them with no change to the client.
//
// The body is decoded without the JSON routes' own refusal of U+0000 (decodeJSONBody): the text
// it carries is checked as the upload checks its own, under the upload's name for it. It is read
// within this route's own limit, not a JSON body's megabyte, so no text an upload would store is
// refused here for its body (markdownRoundTripLimit).
func (s *server) postMarkdownRoundTrip(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	var input markdownRoundTripInput
	limit, tooLarge := routeBody(http.MethodPost, markdownRoundTripPattern)
	body, err := decodeJSONBody(r, &input, limit, tooLarge)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	logUndeclaredMembers(r, &input, body)
	var stored, scope string
	switch {
	case (input.Markdown == nil) == (input.File == nil):
		s.writeHandlerError(w, errMarkdownRoundTripInput)
		return
	case input.File != nil:
		stored, scope, err = markdownRoundTrip(input.File, true)
	default:
		stored, scope, err = markdownRoundTrip(*input.Markdown, false)
	}
	if err != nil {
		// Every error markdownRoundTrip answers is an upload's refusal of the text alone, and
		// carries the scope it holds for: its checks read the text and nothing else.
		w.Header().Set(contracts.TextRefusalHeader, scope)
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, markdownRoundTripResponse{Markdown: stored})
}

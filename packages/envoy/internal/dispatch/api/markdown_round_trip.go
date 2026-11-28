package api

import (
	"net/http"
	"strings"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
)

// markdownRoundTrip is the round trip's answer to content, the text an upload would store as a
// markdown document - inline, as a JSON upload's `content`, or with file, a file's bytes, as a
// multipart upload's file part: the refusal that upload makes of it for the text alone, or else
// the markdown it stores. The refusals are the upload's own, in its order, served as the upload
// serves them: first documentTextRefusal's, which every upload of a markdown document makes before
// its write - the text's bytes, and a file's encoding - and then its write's.
//
// With newDocument the write is a new document's seed (docs.SeedRendering), the steps of SeedText
// that read the text alone: the answer is its rendering, or its refusal. Without it, the write is
// the most permissive replacement (docs.ReplacementRendering), one over a document that already
// holds what the text holds: the answer is the rendering that replacement stores, or the refusal
// every replacement makes of the text - the parse's own, the ask refusal the parse's read-back
// names instead (askRefusalInstead), and a render that fails. So a client that does not ask for a
// new document's refusals, and refuses its upload on any failure of the round trip but the route's
// absence, is refused no replacement the store takes, and compares the spelling that replacement
// stores. For a text main's parse takes, that rendering is main's answer, storedMarkdown's; for one
// it refused, a block id named twice or an element this build cannot create yet, main answered
// the text itself, and this answers the rendering. A JSON upload of blank content over a held
// document is refused as blank before its text is parsed (blankWriteToADocumentThatExists), which
// reads more than the text, so blank text is answered as that replacement would parse it.
//
// Text `strings.TrimSpace` empties is answered "", as the near-copy gate's storedMarkdown answers
// it, once nothing has refused it. That answer is not always what the upload stores: the seed
// stores spaces, tabs, carriage returns and line feeds alone as an empty document, but keeps every
// other character strings.TrimSpace empties, so an upload of "\u00a0\n" stores "\u00a0\n"
// (measured through the upload route on 2026-11-21). The route answers "" so that its answer to
// every text within the limits is the one it gave before it read the seed, and `dispatch artifact`
// takes only a refusal from it for such inline text (goBlankPattern) and for a file.
func markdownRoundTrip[T string | []byte](content T, file, newDocument bool) (string, error) {
	if refusal := documentTextRefusal(content, file); refusal != nil {
		return "", refusal
	}
	text := string(content)
	rendering := docs.ReplacementRendering
	if newDocument {
		rendering = docs.SeedRendering
	}
	stored, err := rendering(text)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(text) == "" {
		return "", nil
	}
	return stored, nil
}

// markdownRoundTripPattern is the inline round trip's pattern and markdownRoundTripRoute that
// route, and fileRoundTripPattern and fileRoundTripRoute the file round trip's: with the handoff
// write, the JSON routes whose body limit is their own rather than maxJSONRequestBytes
// (routeBody). The file's check is a route of its own rather than a member of the inline one,
// since a Dispatch that does not know such a member ignores it (decodeJSON) and would answer a
// file's check as the check of no text at all; a Dispatch that does not know the route answers
// 404 or 405, which `dispatch artifact` reads as a Dispatch that cannot check the file.
const (
	markdownRoundTripPattern = "/api/v1/markdown/round-trip"
	markdownRoundTripRoute   = http.MethodPost + " " + markdownRoundTripPattern
	fileRoundTripPattern     = "/api/v1/markdown/file-round-trip"
	fileRoundTripRoute       = http.MethodPost + " " + fileRoundTripPattern
)

// markdownRoundTripLimit is the most the inline round trip's request may hold, derived from the
// largest text it is ever asked about. That text is the one an inline upload is about to store as
// a markdown document, which is at most maxDocumentMarkdownBytes - the upload refuses a byte more,
// and so does this route (documentTextRefusal). It travels here as a JSON string, whose escaping
// can make each byte of it up to six on the wire (a client that writes `<` as \u003c, or a byte of
// a two-byte character as \u00e9), which is how the handoff write's own limit counts the same
// escaping (handoffRequestLimit). The kilobyte beside it carries the object around that string.
//
// A megabyte would refuse text the upload itself takes, since JSON adds a byte for every
// newline, quote, backslash and tab of a document already at the cap. This route's refusal of a
// body past its limit carries no contracts.TextRefusalHeader, so `dispatch artifact` reads it as a
// round trip that did not answer: the call refuses an upload of inline text that is not blank
// until the round trip answers, and its dry run passes with a warning, so such a writer would be
// refused an inline text the store takes.
const markdownRoundTripLimit = 6*maxDocumentMarkdownBytes + 1<<10

// fileRoundTripLimit is the most the file round trip's request may hold: the bytes of the largest
// file an upload stores as a markdown document, maxDocumentMarkdownBytes, as base64, four bytes for
// every three, and a kilobyte for the object around them. A file a little past that size is
// refused as the upload refuses it (documentTextRefusal); one far past it is refused for its body,
// with no contracts.TextRefusalHeader, and `dispatch artifact` asks about no such file, since it
// refuses a file past the bytes one document holds before it asks.
const fileRoundTripLimit = 4*((maxDocumentMarkdownBytes+2)/3) + 1<<10

// errMarkdownRoundTripTooLarge and errFileRoundTripTooLarge are the two round trips' answers to a
// body past its limit, which the drain, the keyed layer and the handler all give alike
// (routeBody). Each names its body's own bound, which is applied before any text in the body is
// read. `dispatch artifact` sends no text past the bytes one document holds, which it refuses
// itself, so no body it sends is past either.
var (
	errMarkdownRoundTripTooLarge = errorf(apicode.CapExceededTooLarge,
		"request body exceeds %d bytes", markdownRoundTripLimit)
	errFileRoundTripTooLarge = errorf(apicode.CapExceededTooLarge,
		"request body exceeds %d bytes", fileRoundTripLimit)
)

// markdownRoundTripInput is the body of POST /api/v1/markdown/round-trip and of POST
// /api/v2/markdown-round-trips (postMarkdownRoundTripV2): Markdown, the text a
// caller is about to store as a Markdown document, as an inline upload sends it as `content`, and
// NewDocument, which asks for the refusals only a new document's seed makes of it
// (markdownRoundTrip). A body without NewDocument is answered as the most permissive replacement
// of a document takes the text; it is the body this route took before it made any refusal. A body
// naming no markdown asks about the empty text.
type markdownRoundTripInput struct {
	Markdown    string `json:"markdown" required:"true"`
	NewDocument bool   `json:"new_document,omitempty"`
}

// fileRoundTripInput is the body of POST /api/v1/markdown/file-round-trip and of POST
// /api/v2/markdown-file-round-trips (postFileRoundTripV2): File, the bytes of a
// file a caller is about to upload as a Markdown document, base64 in the JSON, as a multipart
// upload sends them as its file part, and NewDocument, as markdownRoundTripInput takes it. The
// bytes are checked as bytes, so a file holding a byte that is not UTF-8 is refused as its upload
// is, which no JSON string could carry. A body naming no file asks about an empty file.
type fileRoundTripInput struct {
	File        []byte `json:"file" required:"true"`
	NewDocument bool   `json:"new_document,omitempty"`
}

// markdownRoundTripResponse is the markdown the store would hold for that text.
type markdownRoundTripResponse struct {
	Markdown string `json:"markdown"`
}

// postMarkdownRoundTrip answers POST /api/v1/markdown/round-trip, and postFileRoundTrip POST
// /api/v1/markdown/file-round-trip. Neither writes anything or reads a row: each checks the text
// as an upload of a markdown document checks it, parses it and renders it, as that upload's write
// does, and answers what that write would store (markdownRoundTrip): a new document's seed for a
// body with new_document true, and otherwise the most permissive replacement. A caller compares
// the two and refuses its own call where they differ.
//
// Text every upload of a markdown document refuses for the text alone (a JSON upload of blank
// content over a held document is refused as blank before its text is parsed) is refused here
// with that upload's own status, code and words: inline text holding U+0000, a file holding
// U+0000 or a byte that is not UTF-8, text past maxDocumentMarkdownBytes, markdown the parse
// refuses (its element budget, its schema, a rendering no reader reads back, and an ask that
// breaks its content rule in a text whose rendering no reader reads back, which that refusal
// names instead) and a render that fails. For a body with new_document true, so is text only an
// upload of a new markdown document refuses for the text alone: a block id named on two blocks,
// an element this build cannot create yet, an ask that breaks its content rule, a rendering past
// the bytes or the elements one document holds, and a check CI proves that breaks one of the
// rules its attributes keep. Both are made in the seed's order, so such a body is refused a text
// that breaks two rules with the first.
// A body without it is answered with the rendering the most permissive replacement stores, or
// with the first refusal every upload makes of the text. Each refusal, whatever its status,
// carries contracts.TextRefusalHeader, valued textRefusalMark. No other answer of either route
// carries it. `dispatch artifact` and its dry run ask for a new document's refusals where the call
// makes a new document, without --force, and take an answer carrying the header as the call's
// refusal, so they refuse such text as the upload would, inline or from a markdown file, and a
// check the upload makes of the text alone before its write (documentTextRefusal) or in the steps
// of its seed or its replacement that read the text alone (docs.SeedRendering,
// docs.ReplacementRendering) reaches them with no change to the client.
//
// The inline body is decoded without the JSON routes' own refusal of U+0000
// (readJSONBodyAllowingNUL): the text it carries is checked as the upload checks its own, under the
// upload's name for it, `content`. Each body is read within its route's own limit, not a JSON body's
// megabyte, so no text an upload would store is refused here for its body (markdownRoundTripLimit,
// fileRoundTripLimit).
func (s *server) postMarkdownRoundTrip(w http.ResponseWriter, r *http.Request) {
	var input markdownRoundTripInput
	if !s.decodeRoundTrip(w, r, markdownRoundTripPattern, &input) {
		return
	}
	s.answerMarkdownRoundTrip(w, r, input)
}

// postFileRoundTrip answers POST /api/v1/markdown/file-round-trip (postMarkdownRoundTrip).
func (s *server) postFileRoundTrip(w http.ResponseWriter, r *http.Request) {
	var input fileRoundTripInput
	if !s.decodeRoundTrip(w, r, fileRoundTripPattern, &input) {
		return
	}
	s.answerFileRoundTrip(w, r, input)
}

// answerMarkdownRoundTrip answers the inline round trip of both versions for input, its body as
// decoded (postMarkdownRoundTrip, postMarkdownRoundTripV2). The JSON upload's decode refuses
// U+0000 in its `content` before anything else of the text (unstorableJSON), so this makes that
// refusal first, in those words; a JSON string holds no byte that is not UTF-8, since decoding
// writes each as U+FFFD.
func (s *server) answerMarkdownRoundTrip(w http.ResponseWriter, r *http.Request, input markdownRoundTripInput) {
	if refusal := unstorableText("content", input.Markdown); refusal != nil {
		s.writeTextRefusal(w, r, refusal)
		return
	}
	stored, err := markdownRoundTrip(input.Markdown, false, input.NewDocument)
	s.writeRoundTrip(w, r, stored, err)
}

// answerFileRoundTrip answers the file round trip of both versions for input, its body as decoded
// (postFileRoundTrip, postFileRoundTripV2).
func (s *server) answerFileRoundTrip(w http.ResponseWriter, r *http.Request, input fileRoundTripInput) {
	stored, err := markdownRoundTrip(input.File, true, input.NewDocument)
	s.writeRoundTrip(w, r, stored, err)
}

// decodeRoundTrip decodes the body of the round trip at pattern into input, within that route's
// own limit (routeBody), and reports whether it did; a refusal is written here.
func (s *server) decodeRoundTrip(w http.ResponseWriter, r *http.Request, pattern string, input any) bool {
	if !s.requireAuthenticated(w, r) {
		return false
	}
	limit, tooLarge := routeBody(http.MethodPost, pattern)
	body, err := readJSONBodyAllowingNUL(r, input, limit, tooLarge)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return false
	}
	logUndeclaredMembers(r, input, body)
	return true
}

// writeRoundTrip writes a round trip's answer: err, the refusal of the text, or the markdown
// stored.
func (s *server) writeRoundTrip(w http.ResponseWriter, r *http.Request, stored string, err error) {
	if err != nil {
		s.writeTextRefusal(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, markdownRoundTripResponse{Markdown: stored})
}

// textRefusalMark is the one value of contracts.TextRefusalHeader: the header marks a round trip's
// answer as the refusal of the text, and a client reads only that the header is there.
const textRefusalMark = "text"

// writeTextRefusal writes refusal, an upload's refusal of a text for the text alone, marked as
// such (contracts.TextRefusalHeader). Every error a round trip answers after its body is read is
// one: its checks read the text and nothing else.
func (s *server) writeTextRefusal(w http.ResponseWriter, r *http.Request, refusal error) {
	w.Header().Set(contracts.TextRefusalHeader, textRefusalMark)
	s.writeHandlerError(w, r, refusal)
}

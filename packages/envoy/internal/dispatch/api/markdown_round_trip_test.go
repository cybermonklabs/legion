package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// markdownCorpus is every markdown document of the parser's corpus, which holds what the two
// renderings have to agree on: tables, footnotes, typed blocks, lazy lines, escapes and the
// generated cases a fuzzer found.
func markdownCorpus(t *testing.T) []string {
	t.Helper()
	paths, err := filepath.Glob("../pmdoc/testdata/corpus/*.md")
	if err != nil {
		t.Fatalf("read the markdown corpus: %v", err)
	}
	if len(paths) < 100 {
		t.Fatalf("the markdown corpus holds %d documents, too few to compare two renderings over", len(paths))
	}
	return paths
}

// TestMarkdownRoundTripAnswersWhatTheStoreWouldHold is the route's one claim: for every document
// of the parser's corpus, POST /api/v1/markdown/round-trip answers exactly what a document write
// of that text stores (docs' ReplaceText). A round trip that is not the store's own would refuse
// good Markdown wherever the two renderers differ by a byte, which is what `dispatch artifact`
// reads the route for, so a second rendering written beside the store's is what this test fails.
//
// Each corpus document is written into a document of its own, seeded with a text holding no
// typed block, so no block id of the seed is carried into the rendering. The ids a write mints
// are random, so both sides mint from one counter that is reset before each of them: the parse
// is the same parse, so it mints the same ids in the same order, and the two renderings are
// comparable byte for byte.
func TestMarkdownRoundTripAnswersWhatTheStoreWouldHold(t *testing.T) {
	var documentService *docs.Service
	handler, database := newInteractionHandler(t, func(database *store.Store) docs.API {
		documentService = docs.New(docs.Deps{Store: database, Settle: time.Hour})
		t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
		return documentService
	})
	issue := createInteractionIssue(t, handler, "ROUND", "Markdown round trip", "The seed.\n")
	alice := model.Actor{Kind: "user", ID: "alice"}

	var prefix atomic.Value
	var next atomic.Int64
	prefix.Store("seed")
	pmdoc.SetBlockIDGenerator(func() string { return fmt.Sprintf("%s-%d", prefix.Load(), next.Add(1)) })
	t.Cleanup(func() { pmdoc.SetBlockIDGenerator(nil) })
	// mint restarts the ids under their own prefix, so the seed's ids are no write's and the
	// write and the route mint the same ones.
	mint := func(under string) {
		prefix.Store(under)
		next.Store(0)
	}

	refused, compared := 0, 0
	for index, path := range markdownCorpus(t) {
		name := filepath.Base(path)
		source, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		mint("seed")
		created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]any{
			"name": fmt.Sprintf("corpus-%d.md", index), "content": "The seed.\n", "actor": sessionActor(),
		}, "alice")
		if created.Code != http.StatusCreated {
			t.Fatalf("%s: seed its document: status=%d body=%.300s", name, created.Code, created.Body.String())
		}
		artifactID := decodeBody[struct {
			Artifact struct {
				ID string `json:"id"`
			} `json:"artifact"`
		}](t, created).Artifact.ID

		mint("write")
		stored, storeErr := replaceDocumentText(database, documentService, artifactID, string(source), alice)
		mint("write")
		answered := roundTripOf(t, handler, "inline", string(source), true)
		if answered.Code != http.StatusOK {
			// A text the round trip refuses, asked as `dispatch artifact` asks it, for a new
			// document's refusals, as an upload of it as a new document does. The upload must refuse
			// it too, else the command would refuse a text the upload would store.
			upload := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]any{
				"name": fmt.Sprintf("refused-%d.md", index), "content": string(source), "actor": sessionActor(),
			}, "alice")
			if upload.Code < http.StatusBadRequest {
				t.Fatalf("%s: round trip: status=%d body=%.300s, and the upload of the same text answers %d", name, answered.Code, answered.Body.String(), upload.Code)
			}
			refused++
			t.Logf("%s: both the round trip and the upload refuse this text: %s", name, answered.Body.String())
			continue
		}
		roundTripped := decodeBody[markdownRoundTripResponse](t, answered).Markdown
		if storeErr != nil {
			// The store refuses this text as a replacement, for a rule of its own that a new
			// document's seed, which the round trip runs, does not hold it to. There is no stored
			// text to compare the route's answer with, so this document proves nothing either way.
			refused++
			t.Logf("%s: the store refuses this text: %v", name, storeErr)
			continue
		}
		compared++
		if roundTripped != stored {
			t.Fatalf("%s: the round trip answers %q, and the store holds %q", name, roundTripped, stored)
		}
	}
	// A test that compared nothing would pass whatever the route answered.
	if compared < len(markdownCorpus(t))/2 {
		t.Fatalf("the store refused %d of %d corpus documents, so too few proved the route", refused, compared+refused)
	}
	t.Logf("compared %d corpus documents, %d refused by the store", compared, refused)
}

// TestMarkdownRoundTripNamesWhatTheStoreChanges is the route's answer for the texts the command
// refuses on: text the store would change comes back changed, and text it would hold as it
// stands comes back unchanged.
func TestMarkdownRoundTripNamesWhatTheStoreChanges(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	for _, test := range []struct {
		name     string
		markdown string
		want     string
	}{
		{"a rendered document is unchanged", "# Design\n\nThe first paragraph.\n", "# Design\n\nThe first paragraph.\n"},
		{"a star list is stored with dashes", "* one\n* two\n", "- one\n- two\n"},
		{"an ordered list is renumbered", "1. one\n1. two\n", "1. one\n2. two\n"},
		{"a carriage return joins the lines it ends", "one\r\ntwo\n", "one two\n"},
		{"a last line with no line feed takes one", "one", "one\n"},
		{"text of whitespace alone is stored empty", "   \n", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/markdown/round-trip", map[string]any{
				"markdown": test.markdown,
			}, "alice")
			if response.Code != http.StatusOK {
				t.Fatalf("status=%d body=%.300s", response.Code, response.Body.String())
			}
			if answered := decodeBody[markdownRoundTripResponse](t, response).Markdown; answered != test.want {
				t.Fatalf("the round trip of %q answers %q, want %q", test.markdown, answered, test.want)
			}
		})
	}
}

// The route's body limit, which is the largest text an inline upload stores and not a JSON
// body's megabyte (markdownRoundTripLimit). The text this route is asked about is the text an
// upload is about to send, and `dispatch artifact` refuses that upload when this route refuses
// it, so a 413 here for a document the store itself would take would refuse an upload the store
// takes.
//
// A markdown document of maxDocumentMarkdownBytes is that largest text: the upload refuses one
// byte more (artifacts.go). Its body is longer than the text, by the object around the string
// and by a byte for every character JSON escapes, so a megabyte of body refuses it
// (dispatch://INFRA-1522).
func TestMarkdownRoundTripTakesTheLargestTextAnInlineUploadStores(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	// One paragraph line of words is its own canonical rendering, so the answer is the text
	// itself and the size is the whole of what this test is about.
	words := strings.Repeat("word ", maxDocumentMarkdownBytes/len("word "))
	largest := words[:len(words)-1] + "s\n"
	if len(largest) != maxDocumentMarkdownBytes {
		t.Fatalf("the text is %d bytes, want a document at the cap of %d", len(largest), maxDocumentMarkdownBytes)
	}

	answered := dispatchRequest(t, handler, http.MethodPost, "/api/v1/markdown/round-trip", map[string]any{
		"markdown": largest,
	}, "alice")
	if answered.Code != http.StatusOK {
		t.Fatalf("the round trip of the largest text an inline upload stores: status=%d body=%.300s",
			answered.Code, answered.Body.String())
	}
	if stored := decodeBody[markdownRoundTripResponse](t, answered).Markdown; stored != largest {
		t.Fatalf("the round trip answers %d bytes, want the %d it was given unchanged", len(stored), len(largest))
	}

	// The upload that text is checked for takes it, which is what the limit is measured
	// against: this route refuses no text the store would hold.
	issue := createArtifactIssue(t, handler)
	uploaded := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{
		"name": "largest.md", "content": largest,
	}, "alice")
	if uploaded.Code != http.StatusCreated {
		t.Fatalf("the inline upload of that text: status=%d body=%.300s", uploaded.Code, uploaded.Body.String())
	}

	// Text no document may hold is refused as the upload refuses it, in the upload's own words,
	// rather than answered with a spelling: the text here is a `*` bullet the renderer would write
	// as `-`, and its upload is refused all the same.
	overDocument := "* " + strings.Repeat("one ", maxDocumentMarkdownBytes/len("one ")) + "two\n"
	if len(overDocument) <= maxDocumentMarkdownBytes {
		t.Fatalf("the text is %d bytes, want more than a document's %d", len(overDocument), maxDocumentMarkdownBytes)
	}
	over := dispatchRequest(t, handler, http.MethodPost, "/api/v1/markdown/round-trip", map[string]any{
		"markdown": overDocument,
	}, "alice")
	if code, message := refusalOf(t, over); over.Code != http.StatusRequestEntityTooLarge || code != "CAP_EXCEEDED" ||
		message != contracts.MarkdownDocumentTooLarge {
		t.Fatalf("the round trip of text past a document's bound: status=%d body=%.300s, want 413 CAP_EXCEEDED %q",
			over.Code, over.Body.String(), contracts.MarkdownDocumentTooLarge)
	}

	// The limit is still a limit: a body past it is this route's own refusal, the text is never
	// parsed, and the refusal states the number the handler applies and no other.
	past := dispatchRequest(t, handler, http.MethodPost, "/api/v1/markdown/round-trip", map[string]any{
		"markdown": strings.Repeat("a", markdownRoundTripLimit),
	}, "alice")
	code, message := refusalOf(t, past)
	if past.Code != http.StatusRequestEntityTooLarge || code != "CAP_EXCEEDED" {
		t.Fatalf("a body past the route's own limit: status=%d body=%.300s, want 413 CAP_EXCEEDED",
			past.Code, past.Body.String())
	}
	if want := fmt.Sprintf("request body exceeds %d bytes", markdownRoundTripLimit); message != want {
		t.Fatalf("the refusal reads %q, want %q: a refusal states the bound its own code applies", message, want)
	}
}

// askAfterOptions is an ask block holding a paragraph after its options, which breaks its content
// rule, paragraph+ bullet_list?, so markdown written whole that holds it is refused.
const askAfterOptions = ":::ask{#a1 urgency=\"med\" multiple=\"false\" state=\"open\"}\nWhich database?\n\n" +
	"- Postgres\n- SQLite\n\nPick one by Friday.\n:::\n"

// misreadRendering is markdown whose rendering no reader reads back, which every upload refuses,
// a replacement included (pmdoc.ErrRenderingRefused).
const misreadRendering = "2. [^N]:\n       12. *\n       --\n"

// repeatedCheckID is two check blocks naming one block id, c1, written as the store renders them
// (renderedCheck), which only a new document's seed refuses: a replacement over a document that
// names c1 on two blocks already takes it, keeping c1 for the first block and minting the second
// another id (pmdoc.EnsureBlockIDs), and mintedCheckID is the rendering it stores, that minted id
// written "minted" (mintedBlockIDs). The id is all the store changes.
var (
	repeatedCheckID = ciCheckBlock("c1", renderedCheck) + "\n" + ciCheckBlock("c1", renderedCheck)
	mintedCheckID   = ciCheckBlock("c1", renderedCheck) + "\n" + ciCheckBlock("minted", renderedCheck)
)

// renderedCheck is ciCheckOrdinary as the store renders a check block's attributes, its `when`
// written out.
const renderedCheck = `when="before_merge" ` + ciCheckOrdinary

// mintedBlockIDs matches a block id the parse mints (pmdoc's newBlockID, a version 4 UUID), which
// is random, so a rendering holding one is compared with each written "minted".
var mintedBlockIDs = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}`)

// roundTripRefusal is a text an upload refuses for the text alone, how the round trip is sent it
// (sent: "inline", in the markdown round trip's markdown, and "file", as base64 in the file round
// trip's file), the status, code and words of that refusal, its scope (textRefusalsFile), and
// replaced, the answer to a body without new_document where it is not that refusal.
type roundTripRefusal struct {
	name, text         string
	sent               []string
	status             int
	code, words, scope string
	replaced           roundTripReplaced
}

// uploadScope and newDocumentScope are the two scopes a row of textRefusalsFile names.
const (
	uploadScope      = "upload"
	newDocumentScope = "new-document"
)

// roundTripReplaced is the answer to a body without new_document for a text whose scope is
// new-document, the most permissive replacement's (markdownRoundTrip), where it is not
// storedMarkdown's, main's answer: a refusal that replacement makes of the text (status, code and
// words), or, for a text main's parse refused, the rendering that replacement stores, each id its
// parse minted written "minted" (mintedBlockIDs). The zero value is storedMarkdown's answer, the
// rendering of a text main's parse takes.
type roundTripReplaced struct {
	status      int
	code, words string
	rendering   string
}

// textRefusalsFile is the list of the texts an upload refuses for the text alone, one JSON object
// a row in a JSON array. roundTripRefusals reads it for the route tests here, and the end-to-end
// spec packages/dispatch/e2e/artifact-dry-run.e2e.ts reads it by a relative path against a
// running Dispatch, so both take their rows from one list. JSON takes no comments, so this is its
// description. A row is a roundTripRefusal, its members named as that type's fields: name; text, a
// list of parts joined in order; sent, ["inline", "file"] or ["file"]; status; code; words, ""
// where the row names none; scope; and replaced, absent where it is the zero value, with
// {status, code, words} or {rendering}, each id the parse mints written "minted". Every member but
// replaced and command is written on every row. command, absent where it is empty, names the
// inline ways the dispatch command takes a text in that the route tests cannot send inline, and
// refuses itself, before it asks Dispatch: ["inline"] for a text it refuses given inline, from a
// --content-file or on stdin, in Dispatch's own code and in words holding the row's words - bytes
// that are not UTF-8, which no JSON string carries, and which the command reads as bytes, refused
// as a Markdown --path file of the same bytes is, the size first, then the first U+0000 or byte
// that is not UTF-8. The command refuses itself, in the same code and words, any other text from a
// --content-file or on stdin past the bytes or holding U+0000, which the route tests send inline,
// so command leaves those rows out. The end-to-end spec runs those ways too; the route tests read
// command for nothing but its check here.
//
// scope is a fact of the tests, which they sort the rows by; Dispatch's answers carry no scope.
// It is "upload" where every upload of the text refuses it with the row's status, code and words,
// a replacement over a held document included, which the round trip without new_document then
// answers alike (requireReplacementAnswer) and the replacement upload is held to
// (TestARoundTripWithoutNewDocumentRefusesInTheReplacementUploadsOwnWords). It is "new-document"
// where a replacement need not refuse the text alike, and replaced says what the round trip
// without new_document answers for it. A part of a row's text is one of these, so the large texts
// are generated rather than stored:
//
//   - {"text": s}: s as written;
//   - {"repeat": s, "times": n}: s n times;
//   - {"repeat": s, "bytes": n}: s repeated and cut to its first n bytes;
//   - {"base64": s}: the bytes s decodes to, for bytes no JSON string carries, such as 0xFF.
const textRefusalsFile = "testdata/text-refusals.json"

// textRefusalRow is a row of textRefusalsFile as JSON writes it, and textRefusalPart a part of its
// text.
type textRefusalRow struct {
	Name     string            `json:"name"`
	Text     []textRefusalPart `json:"text"`
	Sent     []string          `json:"sent"`
	Command  []string          `json:"command"`
	Status   int               `json:"status"`
	Code     string            `json:"code"`
	Words    string            `json:"words"`
	Scope    string            `json:"scope"`
	Replaced struct {
		Status    int    `json:"status"`
		Code      string `json:"code"`
		Words     string `json:"words"`
		Rendering string `json:"rendering"`
	} `json:"replaced"`
}

type textRefusalPart struct {
	Text   *string `json:"text"`
	Base64 *string `json:"base64"`
	Repeat string  `json:"repeat"`
	Times  int     `json:"times"`
	Bytes  int     `json:"bytes"`
}

// textRefusalText is the text parts stand for (textRefusalsFile), or an error naming the first
// part that is none of the four.
func textRefusalText(parts []textRefusalPart) (string, error) {
	var text strings.Builder
	for index, part := range parts {
		literal, encoded, repeated := part.Text != nil, part.Base64 != nil, part.Repeat != ""
		switch {
		case literal && !encoded && !repeated && part.Times == 0 && part.Bytes == 0:
			text.WriteString(*part.Text)
		case encoded && !literal && !repeated && part.Times == 0 && part.Bytes == 0:
			decoded, err := base64.StdEncoding.DecodeString(*part.Base64)
			if err != nil {
				return "", fmt.Errorf("part %d: %w", index, err)
			}
			text.Write(decoded)
		case repeated && !literal && !encoded && part.Times > 0 && part.Bytes == 0:
			text.WriteString(strings.Repeat(part.Repeat, part.Times))
		case repeated && !literal && !encoded && part.Bytes > 0 && part.Times == 0:
			text.WriteString(strings.Repeat(part.Repeat, part.Bytes/len(part.Repeat)+1)[:part.Bytes])
		default:
			return "", fmt.Errorf(`part %d is none of {"text": s}, {"base64": s}, {"repeat": s, "times": n} and {"repeat": s, "bytes": n}, s not empty and n above 0`, index)
		}
	}
	return text.String(), nil
}

// roundTripRefusals is every row of textRefusalsFile, the texts of
// TestMarkdownRoundTripRefusesWhatAnUploadRefusesForItsText, each with its scope.
//
// The texts are built to the sizes checked here, a row's words are the constant's where the Go
// code names one, the CI checks' as ciCheckRefusal writes them, and a row whose text or rendering
// is a value the other tests here use (askAfterOptions, misreadRendering, repeatedCheckID,
// mintedCheckID and each ciCheckBreaks rule's block) is that value, so the list and those tests
// cannot drift apart. A row's sent names "inline" and "file" alone, its command "inline" alone,
// and its scope is uploadScope or newDocumentScope.
func roundTripRefusals(t *testing.T) []roundTripRefusal {
	t.Helper()
	raw, err := os.ReadFile(textRefusalsFile)
	if err != nil {
		t.Fatalf("read the text refusals: %v", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var rows []textRefusalRow
	if err := decoder.Decode(&rows); err != nil {
		t.Fatalf("decode %s: %v", textRefusalsFile, err)
	}
	refusals := make([]roundTripRefusal, 0, len(rows))
	named := make(map[string]roundTripRefusal, len(rows))
	for _, row := range rows {
		text, err := textRefusalText(row.Text)
		if err != nil {
			t.Fatalf("%s: the text of %q: %v", textRefusalsFile, row.Name, err)
		}
		if _, ok := named[row.Name]; ok {
			t.Fatalf("%s names two rows %q", textRefusalsFile, row.Name)
		}
		for _, way := range row.Sent {
			if way != "inline" && way != "file" {
				t.Fatalf(`%s gives %q the way %q in sent, want "inline" or "file"`, textRefusalsFile, row.Name, way)
			}
		}
		for _, way := range row.Command {
			if way != "inline" {
				t.Fatalf(`%s gives %q the way %q in command, want "inline"`, textRefusalsFile, row.Name, way)
			}
		}
		if row.Scope != uploadScope && row.Scope != newDocumentScope {
			t.Fatalf(`%s gives %q the scope %q, want %q or %q`, textRefusalsFile, row.Name, row.Scope, uploadScope, newDocumentScope)
		}
		refusal := roundTripRefusal{
			name: row.Name, text: text, sent: row.Sent, status: row.Status, code: row.Code, words: row.Words, scope: row.Scope,
			replaced: roundTripReplaced{
				status: row.Replaced.Status, code: row.Replaced.Code, words: row.Replaced.Words, rendering: row.Replaced.Rendering,
			},
		}
		refusals = append(refusals, refusal)
		named[row.Name] = refusal
	}
	rowNamed := func(name string) roundTripRefusal {
		t.Helper()
		refusal, ok := named[name]
		if !ok {
			t.Fatalf("%s holds no row named %q", textRefusalsFile, name)
		}
		return refusal
	}

	pastBytesRows := []string{"text past the bytes one document holds", "a file past the bytes one document holds holding a byte that is not UTF-8"}
	for _, name := range pastBytesRows {
		if pastBytes := rowNamed(name).text; len(pastBytes) <= maxDocumentMarkdownBytes {
			t.Fatalf("%q is %d bytes, want more than a document's %d", name, len(pastBytes), maxDocumentMarkdownBytes)
		}
	}
	// A paragraph line at the bytes one document holds, with no final line feed: the rendering ends
	// it with one, a byte past the bytes.
	unended := rowNamed("text at the bytes whose rendering adds a final line feed").text
	if len(unended) != maxDocumentMarkdownBytes || strings.HasSuffix(unended, "\n") {
		t.Fatalf("the text is %d bytes, want a document's %d with no final line feed", len(unended), maxDocumentMarkdownBytes)
	}
	// 1,100 paragraphs of one backtick each, which pairs with no other, so the parse reads it as
	// text and the rendering writes it escaped, a byte more each, after a paragraph that leaves the
	// text a thousand bytes short of the cap.
	if escaped := rowNamed("text whose rendering escapes 1,100 backticks past the bytes").text; len(escaped) != maxDocumentMarkdownBytes-1_000 {
		t.Fatalf("the text is %d bytes, want a thousand short of a document's %d", len(escaped), maxDocumentMarkdownBytes)
	}
	if blank := rowNamed("text strings.TrimSpace empties, past the elements one write may make").text; strings.TrimSpace(blank) != "" {
		t.Fatal("the no-break space paragraphs are not blank to strings.TrimSpace")
	}

	constants := map[string]string{}
	for _, name := range pastBytesRows {
		constants[name] = contracts.MarkdownDocumentTooLarge
	}
	for _, rule := range ciCheckBreaks {
		constants["a check proved by CI with "+rule.name] = ciCheckRefusal("c2", rule.rule)
	}
	for name, words := range constants {
		if got := rowNamed(name).words; got != words {
			t.Fatalf("%s gives %q the words %q, want %q", textRefusalsFile, name, got, words)
		}
	}
	texts := map[string]string{
		"an ask block that breaks its content rule":                                            askAfterOptions,
		"markdown whose rendering no reader reads back":                                        misreadRendering,
		"an ask that breaks its content rule in markdown whose rendering no reader reads back": askAfterOptions + "\n" + misreadRendering,
		"a block id named on two blocks":                                                       repeatedCheckID,
		"a block id named on two blocks beside a list the store respells":                      "* a\n\n" + repeatedCheckID,
		"a block id named on two blocks in markdown whose rendering no reader reads back":      repeatedCheckID + "\n" + misreadRendering,
	}
	for _, rule := range ciCheckBreaks {
		texts["a check proved by CI with "+rule.name] = "Intro.\n\n" + ciCheckBlock("c2", rule.markdown)
	}
	for name, text := range texts {
		if got := rowNamed(name).text; got != text {
			t.Fatalf("%s gives %q the text %q, want %q", textRefusalsFile, name, got, text)
		}
	}
	renderings := map[string]string{
		"a block id named on two blocks":                                  mintedCheckID,
		"a block id named on two blocks beside a list the store respells": "- a\n\n" + mintedCheckID,
	}
	for name, rendering := range renderings {
		if got := rowNamed(name).replaced.rendering; got != rendering {
			t.Fatalf("%s gives %q the replaced rendering %q, want %q", textRefusalsFile, name, got, rendering)
		}
	}
	return refusals
}

// roundTripDeclaredRefusal matches a refusal a route's description names: a three-digit status
// and then an upper-case code of letters, digits and underscores, as in "400 INVALID_UTF8".
var roundTripDeclaredRefusal = regexp.MustCompile(`\b[0-9]{3} [A-Z][A-Z0-9_]*\b`)

// TestEveryRefusalTheRoundTripsDeclareHasATextRefusalRow binds textRefusalsFile to the round
// trips' contract: each status and code pair the descriptions of POST /api/v1/markdown/round-trip
// and POST /api/v1/markdown/file-round-trip name, which the OpenAPI document publishes, is the
// status and code of at least one row. The pairs are read from the route table itself, not from a
// copy of it. A pair is all it binds, so a declared refusal that shares its pair with another is
// bound through that pair alone, and three declared refusals are not bound by this test by
// themselves: 400 INVALID_MARKDOWN for an element this build cannot create yet and 413
// CAP_EXCEEDED for text whose rendering is past the elements one document holds, which no row
// holds and whose pairs other rows hold, and a render that fails, which the descriptions give no
// pair of its own.
//
// The inline round trip's description also names the route's own refusal of a body past its
// bound, 413 CAP_EXCEEDED, which is no refusal of a text. It needs no exclusion: it is the pair of
// the refusal of text past the bytes one document holds, which the description names too and rows
// prove.
func TestEveryRefusalTheRoundTripsDeclareHasATextRefusalRow(t *testing.T) {
	held := map[string]bool{}
	for _, refusal := range roundTripRefusals(t) {
		held[fmt.Sprintf("%d %s", refusal.status, refusal.code)] = true
	}
	declared, described := map[string]bool{}, 0
	for _, route := range (&server{}).routes() {
		if name := route.Method + " " + route.Pattern; name != markdownRoundTripRoute && name != fileRoundTripRoute {
			continue
		}
		described++
		for _, pair := range roundTripDeclaredRefusal.FindAllString(route.Description, -1) {
			declared[pair] = true
		}
	}
	if described != 2 || len(declared) == 0 {
		t.Fatalf("the route table holds %d of the two round trips, which name %d refusals; want both, naming at least one", described, len(declared))
	}
	var missing []string
	for pair := range declared {
		if !held[pair] {
			missing = append(missing, pair)
		}
	}
	slices.Sort(missing)
	if len(missing) > 0 {
		t.Fatalf("the round trips declare %s, and no row of %s is refused with it", strings.Join(missing, ", "), textRefusalsFile)
	}
}

// roundTripOf is the round trip of text sent as kind - "inline" as the markdown round trip's
// markdown, "file" as base64 in the file round trip's file - with new_document true where
// newDocument.
func roundTripOf(t *testing.T, handler http.Handler, kind, text string, newDocument bool) *httptest.ResponseRecorder {
	t.Helper()
	pattern, body := markdownRoundTripPattern, map[string]any{"markdown": text}
	if kind == "file" {
		pattern, body = fileRoundTripPattern, map[string]any{"file": []byte(text)}
	}
	if newDocument {
		body["new_document"] = true
	}
	return dispatchRequest(t, handler, http.MethodPost, pattern, body, "alice")
}

// TestMarkdownRoundTripRefusesWhatAnUploadRefusesForItsText is the round trip's answer to text an
// upload of a new markdown document refuses for the text alone (INFRA-4224, R3), asked as
// `dispatch artifact` asks it for a new document, for a new document's refusals (new_document):
// each text of roundTripRefusals is refused with the status, code and words the upload of a new
// document refuses it with, inline (the markdown round trip against a JSON upload's content) and
// as a file (the file round trip against a multipart upload's file part), and carries the mark of
// a refusal of the text (contracts.TextRefusalHeader, valued textRefusalMark). `dispatch artifact`
// and its dry run take a marked answer as the call's refusal, so they refuse it as the upload does.
//
// Each text is asked without new_document too, as a client asks that does not ask for a new
// document's refusals: `dispatch artifact --force`, and a client that does not know the member.
// That body is answered as the most permissive replacement takes the text (markdownRoundTrip),
// since a replacement, judged against the document it replaces, need not make a new document's
// refusals (requireReplacementAnswer). A client that refuses its upload on any failure of the
// round trip but the route's absence is then refused no replacement the store takes.
//
// A body naming no text asks about the empty text, on both routes, and is answered "" with no
// mark. A text within the limits - 9,000 list items, close below the element limit, in a spelling
// the store changes - is answered with the markdown the store holds for it, with new_document or
// without it, which the upload then stores, and text of whitespace alone that the seed takes is
// answered "" either way.
func TestMarkdownRoundTripRefusesWhatAnUploadRefusesForItsText(t *testing.T) {
	handler := newTestHandler(t)
	issue := createArtifactIssue(t, handler)
	uploads := "/api/v1/issues/" + issue.Key + "/artifacts"
	for index, test := range roundTripRefusals(t) {
		t.Run(test.name, func(t *testing.T) {
			for _, kind := range test.sent {
				roundTrip := roundTripOf(t, handler, kind, test.text, true)
				var uploaded *httptest.ResponseRecorder
				if kind == "file" {
					uploaded = multipartRequest(t, handler, uploads, map[string]string{"name": fmt.Sprintf("file-%d.md", index)},
						"file.md", "text/markdown", []byte(test.text), "alice")
				} else {
					uploaded = dispatchRequest(t, handler, http.MethodPost, uploads, map[string]string{
						"name": fmt.Sprintf("inline-%d.md", index), "content": test.text,
					}, "alice")
				}
				code, message := refusalOf(t, roundTrip)
				if roundTrip.Code != test.status || code != test.code || !strings.Contains(message, test.words) {
					t.Fatalf("the round trip of the %s: status=%d body=%.300s, want %d %s naming %q",
						kind, roundTrip.Code, roundTrip.Body.String(), test.status, test.code, test.words)
				}
				if mark := roundTrip.Header().Get(contracts.TextRefusalHeader); mark != textRefusalMark {
					t.Fatalf("the round trip's refusal of the %s carries %s %q, want %q", kind, contracts.TextRefusalHeader, mark, textRefusalMark)
				}
				uploadCode, uploadMessage := refusalOf(t, uploaded)
				if uploaded.Code != roundTrip.Code || uploadCode != code || uploadMessage != message {
					t.Fatalf("the %s upload answers %d %s %.200q, want the round trip's %d %s %.200q",
						kind, uploaded.Code, uploadCode, uploadMessage, roundTrip.Code, code, message)
				}
				requireReplacementAnswer(t, kind, test, roundTrip, roundTripOf(t, handler, kind, test.text, false))
			}
		})
	}

	// A body naming no text asks about the empty text, on either route, whether or not it asks for
	// a new document's refusals, and is answered "" with no mark, as the markdown round trip
	// answered such a body before it made any refusal.
	for _, pattern := range []string{markdownRoundTripPattern, fileRoundTripPattern} {
		for _, body := range []map[string]any{{}, {"new_document": true}} {
			empty := dispatchRequest(t, handler, http.MethodPost, pattern, body, "alice")
			if stored := decodeBody[markdownRoundTripResponse](t, empty).Markdown; empty.Code != http.StatusOK || stored != "" ||
				empty.Header().Get(contracts.TextRefusalHeader) != "" {
				t.Fatalf("%s with %v: status=%d headers=%v body=%.300s, want 200, the empty markdown and no %s",
					pattern, body, empty.Code, empty.Header(), empty.Body.String(), contracts.TextRefusalHeader)
			}
		}
	}

	// Text of whitespace alone that the seed takes is answered "", whether the seed stores it empty
	// or keeps it (markdownRoundTrip).
	for _, space := range []string{"   \n", "\u00a0\n"} {
		for _, newDocument := range []bool{false, true} {
			answered := roundTripOf(t, handler, "inline", space, newDocument)
			if stored := decodeBody[markdownRoundTripResponse](t, answered).Markdown; answered.Code != http.StatusOK || stored != "" {
				t.Fatalf("the round trip of %q, new_document %v: status=%d body=%.300s, want 200 and the empty markdown",
					space, newDocument, answered.Code, answered.Body.String())
			}
		}
	}

	within := strings.Repeat("* a\n", 9_000)
	want := strings.Repeat("- a\n", 9_000)
	for _, newDocument := range []bool{false, true} {
		answered := roundTripOf(t, handler, "inline", within, newDocument)
		if answered.Code != http.StatusOK {
			t.Fatalf("the round trip of text within both limits, new_document %v: status=%d body=%.300s",
				newDocument, answered.Code, answered.Body.String())
		}
		if stored := decodeBody[markdownRoundTripResponse](t, answered).Markdown; stored != want {
			t.Fatalf("the round trip, new_document %v, answers %.40q, want the store's spelling %.40q", newDocument, stored, want)
		}
	}
	uploaded := dispatchRequest(t, handler, http.MethodPost, uploads, map[string]string{
		"name": "within.md", "content": within,
	}, "alice")
	if uploaded.Code != http.StatusCreated {
		t.Fatalf("the upload of text within both limits: status=%d body=%.300s", uploaded.Code, uploaded.Body.String())
	}
	artifact := decodeBody[artifactUploadResponse](t, uploaded).Artifact
	if markdown := documentMarkdown(t, handler, artifact.ID); markdown != want {
		t.Fatalf("the upload stored %.40q, want the round trip's answer %.40q", markdown, want)
	}
}

// requireReplacementAnswer fails t unless unasked, the round trip of test's text sent as kind
// without new_document, is the most permissive replacement's answer (markdownRoundTrip), where
// asked is the round trip with new_document. A text whose scope is upload, which every upload
// refuses alike, a replacement included - its encoding, its bytes, the parse's refusals of a text
// that is not blank - is refused alike with the member and without it, with the same status,
// code, words and mark. A text whose scope is new-document is answered as test.replaced says:
// refused, marked, for a refusal that replacement makes (the new document's own, or one it came
// before); or else answered 200, with no mark, with the rendering that replacement stores - for a
// text main's parse takes, storedMarkdown's answer, main's, and for one it refused, a block id
// named twice, the rendering with the second block's id minted, where main answered the text
// itself.
func requireReplacementAnswer(t *testing.T, kind string, test roundTripRefusal, asked, unasked *httptest.ResponseRecorder) {
	t.Helper()
	mark := unasked.Header().Get(contracts.TextRefusalHeader)
	code, message := refusalOf(t, unasked)
	switch {
	case test.scope == uploadScope:
		askedCode, askedMessage := refusalOf(t, asked)
		if unasked.Code != asked.Code || code != askedCode || message != askedMessage || mark != textRefusalMark {
			t.Fatalf("the round trip of the %s without new_document: status=%d %s=%q body=%.300s, want the refusal with it, %d %s %.200q, marked %q",
				kind, unasked.Code, contracts.TextRefusalHeader, mark, unasked.Body.String(), asked.Code, askedCode, askedMessage, textRefusalMark)
		}
	case test.replaced.status != 0:
		if unasked.Code != test.replaced.status || code != test.replaced.code || !strings.Contains(message, test.replaced.words) ||
			mark != textRefusalMark {
			t.Fatalf("the round trip of the %s without new_document: status=%d %s=%q body=%.300s, want %d %s naming %q, marked %q",
				kind, unasked.Code, contracts.TextRefusalHeader, mark, unasked.Body.String(),
				test.replaced.status, test.replaced.code, test.replaced.words, textRefusalMark)
		}
	default:
		if unasked.Code != http.StatusOK || mark != "" {
			t.Fatalf("the round trip of the %s without new_document: status=%d %s=%q body=%.300s, want 200 and no mark",
				kind, unasked.Code, contracts.TextRefusalHeader, mark, unasked.Body.String())
		}
		answered, want := decodeBody[markdownRoundTripResponse](t, unasked).Markdown, storedMarkdown(test.text)
		if test.replaced.rendering != "" {
			answered, want = mintedBlockIDs.ReplaceAllString(answered, "minted"), test.replaced.rendering
		}
		if answered != want {
			t.Fatalf("the round trip of the %s without new_document answers %d bytes %.200q, want %d bytes %.200q",
				kind, len(answered), answered, len(want), want)
		}
	}
}

// TestMarkdownRoundTripWithoutNewDocumentAnswersAReplacementTheUploadStores is the replacement a
// client that does not ask for a new document's refusals uploads with --force: one that makes the
// round trip before every inline upload that is not blank, and refuses the upload on any failure
// of it but the route's absence. The document holds an ask with a paragraph after its options,
// which breaks the ask's content rule, written as a browser editor writes it: a Yjs update through
// the document's socket (writeBrowserDocument), which is not held to the checks the server's own
// writes run. The replacement changes the paragraph beside the ask and leaves the ask as the
// document holds it. A new document's seed refuses that text - the round trip asked with
// new_document refuses it, marked, and an upload under a new name refuses it alike -
// and a replacement takes it, since a write is held to an ask's content rule only where it
// changes the ask. Without new_document the round trip answers the rendering that replacement
// stores, which is the text itself here, the document's own rendering with one paragraph changed:
// that client compares it with what it holds and sends it, and the upload naming the document's
// token stores it.
func TestMarkdownRoundTripWithoutNewDocumentAnswersAReplacementTheUploadStores(t *testing.T) {
	documentService, handler, _ := browserDocumentService(t)
	issue := createInteractionIssue(t, handler, "TEST", "A replacement over a browser's ask", "The spec.\n")
	artifactID := seedUploadedDocument(t, handler, issue.Key, "notes.md", "Intro.\n")
	writeBrowserDocument(t, documentService, artifactID, "Intro.\n\n"+askAfterOptions)
	held := documentMarkdown(t, handler, artifactID)
	if !strings.Contains(held, "- SQLite\n\nPick one by Friday.\n") {
		t.Fatalf("the document holds %q, want the browser's ask with a paragraph after its options", held)
	}
	replacement := strings.Replace(held, "Intro.", "Intro, revised.", 1)

	asked := roundTripOf(t, handler, "inline", replacement, true)
	code, message := refusalOf(t, asked)
	if asked.Code != http.StatusBadRequest || code != "INVALID_ASK_BLOCK" || !strings.Contains(message, "holds a paragraph after its options") ||
		asked.Header().Get(contracts.TextRefusalHeader) != textRefusalMark {
		t.Fatalf("the round trip of the replacement with new_document: status=%d %s=%q body=%.300s, want 400 INVALID_ASK_BLOCK marked %q",
			asked.Code, contracts.TextRefusalHeader, asked.Header().Get(contracts.TextRefusalHeader), asked.Body.String(), textRefusalMark)
	}
	asNew := postUpload(t, handler, issue.Key, map[string]any{"name": "other.md", "content": replacement})
	if newCode, newMessage := refusalOf(t, asNew); asNew.Code != asked.Code || newCode != code || newMessage != message {
		t.Fatalf("the upload of the replacement as a new document answers %d %s %q, want the round trip's %d %s %q",
			asNew.Code, newCode, newMessage, asked.Code, code, message)
	}

	unasked := roundTripOf(t, handler, "inline", replacement, false)
	if unasked.Code != http.StatusOK || unasked.Header().Get(contracts.TextRefusalHeader) != "" {
		t.Fatalf("the round trip of the replacement without new_document: status=%d headers=%v body=%.300s, want 200 and no mark",
			unasked.Code, unasked.Header(), unasked.Body.String())
	}
	if answered := decodeBody[markdownRoundTripResponse](t, unasked).Markdown; answered != replacement {
		t.Fatalf("the round trip without new_document answers %q, want its rendering, the text it was given, %q", answered, replacement)
	}

	uploaded := postUpload(t, handler, issue.Key, map[string]any{
		"name": "notes.md", "content": replacement, "precondition": uploadBase(t, handler, artifactID),
	})
	if uploaded.Code != http.StatusCreated {
		t.Fatalf("the replacement naming the document's token: status=%d body=%s", uploaded.Code, uploaded.Body.String())
	}
	if stored := documentMarkdown(t, handler, artifactID); stored != replacement {
		t.Fatalf("the store holds %q after the replacement, want %q", stored, replacement)
	}
}

// TestMarkdownRoundTripWithoutNewDocumentAnswersAReplacementOverARepeatedBlockID is the replacement
// `dispatch artifact --force` uploads over a document that names one block id, c1, on two check
// blocks, as a browser editor can leave it until settlement repairs it. A replacement naming c1 on
// two blocks too is not refused the repeat, which the document holds already, and the round trip
// without new_document answers as that replacement does. Markdown whose rendering no reader reads
// back beside the repeat is refused that read-back by both, in the same status and code, the
// replacement in the round trip's own words, where the round trip with new_document
// refuses the repeat (roundTripRefusals). The repeat alone is stored, the second block under an
// id the store mints (pmdoc.EnsureBlockIDs), which is the rendering the round trip answers: the
// store keeps no text naming one id on two blocks as it is written, so a client that compares the
// round trip's answer with its text refuses every such replacement for its spelling, where main
// answered the text itself and the store respelled it.
func TestMarkdownRoundTripWithoutNewDocumentAnswersAReplacementOverARepeatedBlockID(t *testing.T) {
	documentService, handler, _ := browserDocumentService(t)
	issue := createInteractionIssue(t, handler, "TEST", "A replacement over a repeated block id", "The spec.\n")
	artifactID := seedUploadedDocument(t, handler, issue.Key, "notes.md", "Intro.\n")
	repeated, err := pmdoc.Parse(ciCheckBlock("c1", renderedCheck) + "\n" + ciCheckBlock("c2", renderedCheck))
	if err != nil {
		t.Fatalf("parse the browser's document: %v", err)
	}
	repeated.Children[1].Attrs[pmdoc.BlockIDAttr] = "c1"
	peer := connectBrowserPeer(t, documentService, artifactID)
	t.Cleanup(peer.close)
	peer.edit(t, func(tree *pmdoc.Node) error {
		tree.Children = repeated.Children
		return nil
	})
	waitForLiveText(t, documentService, artifactID, repeatedCheckID)
	replace := func(text string) *httptest.ResponseRecorder {
		return postUpload(t, handler, issue.Key, map[string]any{
			"name": "notes.md", "content": text, "precondition": uploadBase(t, handler, artifactID),
		})
	}

	misread := repeatedCheckID + "\n" + misreadRendering
	unasked := roundTripOf(t, handler, "inline", misread, false)
	code, message := refusalOf(t, unasked)
	if unasked.Code != http.StatusBadRequest || code != "INVALID_MARKDOWN" || !strings.Contains(message, "cannot be read back") ||
		unasked.Header().Get(contracts.TextRefusalHeader) != textRefusalMark {
		t.Fatalf("the round trip of the repeat beside a rendering no reader reads back, without new_document: status=%d %s=%q body=%.300s, want 400 INVALID_MARKDOWN naming the read-back, marked %q",
			unasked.Code, contracts.TextRefusalHeader, unasked.Header().Get(contracts.TextRefusalHeader), unasked.Body.String(), textRefusalMark)
	}
	// A replacement's refusal of its parse is the round trip's, in the round trip's own words
	// (ReplaceText).
	refused := replace(misread)
	if replacedCode, replacedMessage := refusalOf(t, refused); refused.Code != unasked.Code || replacedCode != code ||
		replacedMessage != message {
		t.Fatalf("the replacement over the repeat answers %d %s %q, want the round trip's %d %s %q",
			refused.Code, replacedCode, replacedMessage, unasked.Code, code, message)
	}

	unasked = roundTripOf(t, handler, "inline", repeatedCheckID, false)
	if unasked.Code != http.StatusOK || unasked.Header().Get(contracts.TextRefusalHeader) != "" {
		t.Fatalf("the round trip of the repeat without new_document: status=%d headers=%v body=%.300s, want 200 and no mark",
			unasked.Code, unasked.Header(), unasked.Body.String())
	}
	answered := decodeBody[markdownRoundTripResponse](t, unasked).Markdown
	if answered == repeatedCheckID || mintedBlockIDs.ReplaceAllString(answered, "minted") != mintedCheckID {
		t.Fatalf("the round trip of the repeat without new_document answers %q, want its rendering, the second block's id minted: %q", answered, mintedCheckID)
	}
	if stored := replace(repeatedCheckID); stored.Code != http.StatusCreated {
		t.Fatalf("the replacement over the repeat: status=%d body=%s", stored.Code, stored.Body.String())
	}
	if held := documentMarkdown(t, handler, artifactID); mintedBlockIDs.ReplaceAllString(held, "minted") != mintedCheckID {
		t.Fatalf("the store holds %q after the replacement, want the round trip's rendering, the second block's id minted: %q", held, mintedCheckID)
	}
}

// TestMarkdownRoundTripWithoutNewDocumentAnswersTheRenderingOfAnElementThisBuildCannotCreate is the
// round trip of a text holding an element this build reads and cannot create yet: a typed block a
// newer build writes (pmdoc.UseStagedBlockForTest). Only a new document's seed refuses it; a
// replacement over a document holding the element already keeps it, and a document comes to hold
// it through a rebuild from a version that stored it (TestARebuildFromTheLatestVersionAnswersForWhatThatVersionHolds).
// With new_document the round trip refuses the text, marked, as the upload of a new
// document does. Without it, it answers the rendering that replacement stores, where main answered
// the text itself: a text the store keeps as written is answered with itself, which a client
// comparing the two passes, and the replacement stores it as written; a text the store respells
// is answered respelled, which such a client refuses, and the replacement stores that spelling.
func TestMarkdownRoundTripWithoutNewDocumentAnswersTheRenderingOfAnElementThisBuildCannotCreate(t *testing.T) {
	restore, err := pmdoc.UseStagedBlockForTest("staged", false)
	if err != nil {
		t.Fatalf("stage a typed block this build reads and may not create: %v", err)
	}
	t.Cleanup(restore)
	handler, database, broken := newBrokenTestServer(t)
	issue := createArtifactIssue(t, handler)
	const kept = "before\n\n:::staged{#staged-1}\nBody.\n:::\n"
	artifactID := seedUploadedDocument(t, handler, issue.Key, "staged.md", "before\n")
	if _, err := database.Pool.Exec(context.Background(), `update artifact_versions set markdown = $2 where artifact_id = $1`, artifactID, kept); err != nil {
		t.Fatalf("store the latest version: %v", err)
	}
	broken.failNextLoad(artifactID)
	if rebuilt := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+artifactID+"/rebuild", map[string]any{}, "alice"); rebuilt.Code != http.StatusOK {
		t.Fatalf("rebuild from the version holding the staged block: status=%d body=%s", rebuilt.Code, rebuilt.Body.String())
	}
	if held := documentMarkdown(t, handler, artifactID); held != kept {
		t.Fatalf("the rebuilt document holds %q, want %q", held, kept)
	}

	for index, test := range []struct{ name, text, rendering string }{
		{"a text the store keeps as written", kept, kept},
		{"a text the store respells", strings.Replace(kept, "Body.", "* a", 1), strings.Replace(kept, "Body.", "- a", 1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			asked := roundTripOf(t, handler, "inline", test.text, true)
			code, message := refusalOf(t, asked)
			if asked.Code != http.StatusBadRequest || code != "INVALID_MARKDOWN" || !strings.Contains(message, "cannot write it yet") ||
				asked.Header().Get(contracts.TextRefusalHeader) != textRefusalMark {
				t.Fatalf("the round trip with new_document: status=%d %s=%q body=%.300s, want 400 INVALID_MARKDOWN naming the staged block, marked %q",
					asked.Code, contracts.TextRefusalHeader, asked.Header().Get(contracts.TextRefusalHeader), asked.Body.String(), textRefusalMark)
			}
			asNew := postUpload(t, handler, issue.Key, map[string]any{"name": fmt.Sprintf("new-%d.md", index), "content": test.text})
			if newCode, newMessage := refusalOf(t, asNew); asNew.Code != asked.Code || newCode != code || newMessage != message {
				t.Fatalf("the upload as a new document answers %d %s %q, want the round trip's %d %s %q",
					asNew.Code, newCode, newMessage, asked.Code, code, message)
			}

			unasked := roundTripOf(t, handler, "inline", test.text, false)
			if unasked.Code != http.StatusOK || unasked.Header().Get(contracts.TextRefusalHeader) != "" {
				t.Fatalf("the round trip without new_document: status=%d headers=%v body=%.300s, want 200 and no mark",
					unasked.Code, unasked.Header(), unasked.Body.String())
			}
			if answered := decodeBody[markdownRoundTripResponse](t, unasked).Markdown; answered != test.rendering {
				t.Fatalf("the round trip without new_document answers %q, want its rendering %q", answered, test.rendering)
			}
			replaced := postUpload(t, handler, issue.Key, map[string]any{
				"name": "staged.md", "content": test.text, "precondition": uploadBase(t, handler, artifactID),
			})
			if replaced.Code != http.StatusCreated {
				t.Fatalf("the replacement over the staged block: status=%d body=%s", replaced.Code, replaced.Body.String())
			}
			if held := documentMarkdown(t, handler, artifactID); held != test.rendering {
				t.Fatalf("the store holds %q after the replacement, want the round trip's answer %q", held, test.rendering)
			}
		})
	}
}

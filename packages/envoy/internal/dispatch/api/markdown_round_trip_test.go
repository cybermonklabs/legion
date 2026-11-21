package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
		answered := dispatchRequest(t, handler, http.MethodPost, "/api/v1/markdown/round-trip", map[string]any{
			"markdown": string(source),
		}, "alice")
		if answered.Code != http.StatusOK {
			// A text the round trip refuses, as an upload of it as a new document does. The upload
			// must refuse it too, else the command would refuse a text the upload would store.
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

// TestMarkdownRoundTripRefusesWhatAnUploadRefusesForItsText is the round trip's answer to text an
// upload of a new markdown document refuses for the text alone (INFRA-4224, R3): text past the
// bytes one document holds; 12,000 list items, past the elements one write may make; text within
// both whose rendering, which the seed weighs, passes the bytes; text Go's strings.TrimSpace
// empties that makes more elements than one write may; text holding U+0000; a file holding a byte
// that is not UTF-8, which no JSON string carries; an ask block that breaks its content rule;
// markdown the parser refuses; and markdown whose rendering no reader reads back. Each is refused
// with the status, code and words the upload refuses it with, inline (the body's markdown against
// a JSON upload's content) and as a file (the body's file against a multipart upload's file
// part), and carries the mark of a refusal of the text (contracts.TextRefusalHeader) with its
// scope: upload for the encoding and the bytes, which every upload checks before its write, and
// new-document for the seed's refusals. `dispatch artifact` and its dry run take a marked answer
// as the call's refusal where its scope holds, so they refuse it as the upload does; the route's
// refusal of a body naming no text carries no mark. A text within the
// limits - 9,000 list items, close below the element limit, in a spelling the store changes - is
// answered with the markdown the store holds for it, which the upload then stores, and text of
// whitespace alone that the seed takes is answered "".
func TestMarkdownRoundTripRefusesWhatAnUploadRefusesForItsText(t *testing.T) {
	handler := newTestHandler(t)
	issue := createArtifactIssue(t, handler)
	uploads := "/api/v1/issues/" + issue.Key + "/artifacts"
	words := strings.Repeat("word ", maxDocumentMarkdownBytes/len("word "))
	pastBytes := words + "past\n"
	if len(pastBytes) <= maxDocumentMarkdownBytes {
		t.Fatalf("the text is %d bytes, want more than a document's %d", len(pastBytes), maxDocumentMarkdownBytes)
	}
	// A paragraph line at the bytes one document holds, with no final line feed: the rendering ends
	// it with one, a byte past the bytes.
	unended := words[:len(words)-1] + "ss"
	if len(unended) != maxDocumentMarkdownBytes || strings.HasSuffix(unended, "\n") {
		t.Fatalf("the text is %d bytes, want a document's %d with no final line feed", len(unended), maxDocumentMarkdownBytes)
	}
	// 1,100 paragraphs of one backtick each, which pairs with no other, so the parse reads it as
	// text and the rendering writes it escaped, a byte more each, after a paragraph that leaves the
	// text a thousand bytes short of the cap.
	ticks := strings.Repeat("a`\n\n", 1_100)
	escaped := words[:maxDocumentMarkdownBytes-1_000-len(ticks)-2] + "\n\n" + ticks
	if len(escaped) != maxDocumentMarkdownBytes-1_000 {
		t.Fatalf("the text is %d bytes, want a thousand short of a document's %d", len(escaped), maxDocumentMarkdownBytes)
	}
	blank := strings.Repeat("\u00a0\n\n", 20_000)
	if strings.TrimSpace(blank) != "" {
		t.Fatal("the no-break space paragraphs are not blank to strings.TrimSpace")
	}
	askAfterOptions := ":::ask{#a1 urgency=\"med\" multiple=\"false\" state=\"open\"}\nWhich database?\n\n" +
		"- Postgres\n- SQLite\n\nPick one by Friday.\n:::\n"
	both, fileOnly := []string{"inline", "file"}, []string{"file"}
	upload, newDocument := contracts.TextRefusalUpload, contracts.TextRefusalNewDocument
	for index, test := range []struct {
		name, text         string
		sent               []string
		status             int
		code, words, scope string
	}{
		{"text past the bytes one document holds", pastBytes, both,
			http.StatusRequestEntityTooLarge, "CAP_EXCEEDED", contracts.MarkdownDocumentTooLarge, upload},
		{"12,000 list items, past the elements one write may make", strings.Repeat("- a\n", 12_000), both,
			http.StatusRequestEntityTooLarge, "CAP_EXCEEDED", "more than 65536 elements", newDocument},
		{"text at the bytes whose rendering adds a final line feed", unended, both,
			http.StatusRequestEntityTooLarge, "CAP_EXCEEDED", "this change would make the document's markdown", newDocument},
		{"text whose rendering escapes 1,100 backticks past the bytes", escaped, both,
			http.StatusRequestEntityTooLarge, "CAP_EXCEEDED", "this change would make the document's markdown", newDocument},
		{"text strings.TrimSpace empties, past the elements one write may make", blank, both,
			http.StatusRequestEntityTooLarge, "CAP_EXCEEDED", "more than 65536 elements", newDocument},
		{"text holding U+0000", "# Notes\n\nA\x00B\n", both,
			http.StatusBadRequest, "NUL_CHARACTER", "holds a NUL character (U+0000) at character 11", upload},
		{"a file holding a byte that is not UTF-8", "# Notes\n\nA\xffB\n", fileOnly,
			http.StatusBadRequest, "INVALID_UTF8", "file holds a byte that is not UTF-8 (0xFF) at character 11", upload},
		{"an ask block that breaks its content rule", askAfterOptions, both,
			http.StatusBadRequest, "INVALID_ASK_BLOCK", "holds a paragraph after its options", newDocument},
		{"markdown the parser refuses", "<details>x</details>", both,
			http.StatusBadRequest, "INVALID_MARKDOWN", "", newDocument},
		{"markdown whose rendering no reader reads back", "2. [^N]:\n       12. *\n       --\n", both,
			http.StatusBadRequest, "INVALID_MARKDOWN", "cannot be read back", newDocument},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, kind := range test.sent {
				var roundTrip, uploaded *httptest.ResponseRecorder
				if kind == "file" {
					roundTrip = dispatchRequest(t, handler, http.MethodPost, markdownRoundTripPattern, map[string]any{
						"file": []byte(test.text),
					}, "alice")
					uploaded = multipartRequest(t, handler, uploads, map[string]string{"name": fmt.Sprintf("file-%d.md", index)},
						"file.md", "text/markdown", []byte(test.text), "alice")
				} else {
					roundTrip = dispatchRequest(t, handler, http.MethodPost, markdownRoundTripPattern, map[string]any{
						"markdown": test.text,
					}, "alice")
					uploaded = dispatchRequest(t, handler, http.MethodPost, uploads, map[string]string{
						"name": fmt.Sprintf("inline-%d.md", index), "content": test.text,
					}, "alice")
				}
				code, message := refusalOf(t, roundTrip)
				if roundTrip.Code != test.status || code != test.code || !strings.Contains(message, test.words) {
					t.Fatalf("the round trip of the %s: status=%d body=%.300s, want %d %s naming %q",
						kind, roundTrip.Code, roundTrip.Body.String(), test.status, test.code, test.words)
				}
				if scope := roundTrip.Header().Get(contracts.TextRefusalHeader); scope != test.scope {
					t.Fatalf("the round trip's refusal of the %s carries %s %q, want %q", kind, contracts.TextRefusalHeader, scope, test.scope)
				}
				uploadCode, uploadMessage := refusalOf(t, uploaded)
				if uploaded.Code != roundTrip.Code || uploadCode != code || uploadMessage != message {
					t.Fatalf("the %s upload answers %d %s %.200q, want the round trip's %d %s %.200q",
						kind, uploaded.Code, uploadCode, uploadMessage, roundTrip.Code, code, message)
				}
			}
		})
	}

	// A refusal of the body rather than of the text carries no mark.
	neither := dispatchRequest(t, handler, http.MethodPost, markdownRoundTripPattern, map[string]any{}, "alice")
	if code, _ := refusalOf(t, neither); neither.Code != http.StatusBadRequest || code != "ROUND_TRIP_INPUT" ||
		neither.Header().Get(contracts.TextRefusalHeader) != "" {
		t.Fatalf("a body naming no text: status=%d headers=%v body=%.300s, want 400 ROUND_TRIP_INPUT and no %s",
			neither.Code, neither.Header(), neither.Body.String(), contracts.TextRefusalHeader)
	}

	// Text of whitespace alone that the seed takes is answered "", whether the seed stores it empty
	// or keeps it (markdownRoundTrip).
	for _, space := range []string{"   \n", "\u00a0\n"} {
		answered := dispatchRequest(t, handler, http.MethodPost, markdownRoundTripPattern, map[string]any{
			"markdown": space,
		}, "alice")
		if stored := decodeBody[markdownRoundTripResponse](t, answered).Markdown; answered.Code != http.StatusOK || stored != "" {
			t.Fatalf("the round trip of %q: status=%d body=%.300s, want 200 and the empty markdown", space, answered.Code, answered.Body.String())
		}
	}

	within := strings.Repeat("* a\n", 9_000)
	answered := dispatchRequest(t, handler, http.MethodPost, markdownRoundTripPattern, map[string]any{
		"markdown": within,
	}, "alice")
	if answered.Code != http.StatusOK {
		t.Fatalf("the round trip of text within both limits: status=%d body=%.300s", answered.Code, answered.Body.String())
	}
	stored := decodeBody[markdownRoundTripResponse](t, answered).Markdown
	if want := strings.Repeat("- a\n", 9_000); stored != want {
		t.Fatalf("the round trip answers %.40q, want the store's spelling %.40q", stored, want)
	}
	uploaded := dispatchRequest(t, handler, http.MethodPost, uploads, map[string]string{
		"name": "within.md", "content": within,
	}, "alice")
	if uploaded.Code != http.StatusCreated {
		t.Fatalf("the upload of text within both limits: status=%d body=%.300s", uploaded.Code, uploaded.Body.String())
	}
	artifact := decodeBody[artifactUploadResponse](t, uploaded).Artifact
	if markdown := documentMarkdown(t, handler, artifact.ID); markdown != stored {
		t.Fatalf("the upload stored %.40q, want the round trip's answer %.40q", markdown, stored)
	}
}

package api

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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
			// A text no route of this API takes at all (a NUL character in the body). The upload
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
			// The store refuses this text for a rule of its own (an ask block's content rule,
			// a document too heavy), which the upload that follows the round trip refuses too.
			// The route still answers a rendering, and there is no stored text to compare it
			// with, so this document proves nothing either way.
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
// upload is about to send, and `dispatch artifact` refuses that upload on every failure of
// this route but its absence, so a 413 here tells a writer to send again an upload no repeat
// could ever get through — for a document the store itself would have taken.
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

	// Text no document may hold comes back unchanged, so a caller comparing the two finds them
	// equal and the write makes its own refusal, which names a bound the writer can act on. The
	// text here is a `*` bullet the renderer would write as `-`, so an answer of the store's own
	// spelling would differ and this row would see it.
	overDocument := "* " + strings.Repeat("one ", maxDocumentMarkdownBytes/len("one ")) + "two\n"
	if len(overDocument) <= maxDocumentMarkdownBytes {
		t.Fatalf("the text is %d bytes, want more than a document's %d", len(overDocument), maxDocumentMarkdownBytes)
	}
	over := dispatchRequest(t, handler, http.MethodPost, "/api/v1/markdown/round-trip", map[string]any{
		"markdown": overDocument,
	}, "alice")
	if over.Code != http.StatusOK {
		t.Fatalf("the round trip of text past a document's bound: status=%d body=%.300s", over.Code, over.Body.String())
	}
	if stored := decodeBody[markdownRoundTripResponse](t, over).Markdown; stored != overDocument {
		t.Fatalf("the round trip of text past a document's bound answers %.40q, want the text it was given unchanged",
			stored)
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

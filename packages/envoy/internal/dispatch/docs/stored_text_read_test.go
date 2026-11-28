package docs

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// storedTextReadRenders is how often a text read of a document a room holds renders it: not at
// all. The version written from that very state stored its markdown and its document token beside
// what they were rendered from, and the read answers them without reading the document
// (TextWithToken, storedText). The base this measurement records was one: the read took the
// room's tree (heldTree) and rendered it, though the version held that rendering already.
const storedTextReadRenders = 0

// storedTextReadRendersUnserved is how often a read renders the document where the stored
// rendering is not the document's own: once, as it always did.
const storedTextReadRendersUnserved = 1

// seedSettledDocument seeds markdown as the document's text and settles it, so the document's
// latest version was written from the state its room still holds, and the room holds it.
func seedSettledDocument(t *testing.T, service *Service, artifactID, markdown string) {
	t.Helper()
	seedServiceText(t, service, artifactID, markdown)
	// The settlement writes the version from the room's document, and nothing edits the room
	// afterwards, so the room holds the state the version was written from.
	settleCurrentGeneration(t, service, artifactID)
	requireLatestVersionMarkdown(t, service, artifactID, markdown)
	if tree := liveTree(t, service, artifactID); tree == nil {
		t.Fatal("the room holds no document to read")
	}
	if service.srv.GetDoc(artifactID) == nil {
		t.Fatal("no room holds the document, so the read would be a cold one")
	}
}

// A text read of a document whose room still holds the state its latest version was written from
// renders the document storedTextReadRenders times, and answers the version's stored markdown.
func TestStoredTextReadCountsTheRendersOfAReadOfAVersionedDocument(t *testing.T) {
	// Not parallel: it records every document render the process makes in the package variable
	// documentRenders (countRenders), which refuses a second recorder and would count the renders of
	// the tests beside it.
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	const written = "The quick brown fox.\n"
	seedSettledDocument(t, service, artifactID, written)
	stored := latestVersionMarkdown(t, service, artifactID)

	renders := countRenders(t)
	markdown, token, err := service.TextWithToken(context.Background(), artifactID)
	counted := len(renders())
	if err != nil {
		t.Fatalf("read the document's text: %v", err)
	}
	t.Logf("a text read of a document its room holds costs %d renders of it", counted)
	if markdown != written {
		t.Fatalf("the read answered %q, want the version's markdown %q", markdown, written)
	}
	// The read answers the very bytes the version stored, not a rendering that agrees with them.
	if markdown != stored {
		t.Fatalf("the read answered %q, want the version's stored markdown %q, byte for byte", markdown, stored)
	}
	// The token is the live document's own, so a guarded edit holding it means what it meant
	// when the read rendered the document itself.
	live, err := nodeToken(liveTree(t, service, artifactID))
	if err != nil {
		t.Fatalf("take the live document's token: %v", err)
	}
	if token == "" || token != live {
		t.Fatalf("the read answered token %q, want the live document's %q", token, live)
	}
	if counted != storedTextReadRenders {
		t.Fatalf("the read rendered the document %d times, want %d", counted, storedTextReadRenders)
	}
}

// The text routes serve a document whose room holds the state its latest version was written from
// as that version stored it, rendering it storedTextReadRenders times, where the version's text
// holds no ask block's opener (holdsAskOpener): a plan document, whose `:::plan_task{` opener
// holds `ask{` and which holds no ask block, is served so. A document that holds an ask block is
// read through the room and rendered once, so its asks are read off its tree (textAsks). Each
// answers the version's markdown and the live document's token, in both forms the text routes
// serve.
func TestStoredTextReadServesAPlanDocumentToTheTextRoutesWithNoRender(t *testing.T) {
	// Not parallel: it records every document render the process makes in the package variable
	// documentRenders (countRenders), which refuses a second recorder and would count the renders of
	// the tests beside it.
	for _, document := range []struct {
		name, markdown string
		renders        int
		asks           []string
	}{
		{name: "a plan document", markdown: ":::plan_task{#t1}\nRead the version.\n:::\n", renders: storedTextReadRenders},
		{
			name: "a document holding an ask block", markdown: ":::ask{#a1 urgency=\"med\" multiple=\"false\"}\nShip it?\n:::\n",
			renders: storedTextReadRendersUnserved, asks: []string{"a1"},
		},
	} {
		t.Run(document.name, func(t *testing.T) {
			service, artifactID := newTestService(t)
			service.settle = time.Hour
			seedSettledDocument(t, service, artifactID, document.markdown)
			live, err := nodeToken(liveTree(t, service, artifactID))
			if err != nil {
				t.Fatalf("take the live document's token: %v", err)
			}
			renders := countRenders(t)
			for _, form := range []struct {
				name string
				read func(context.Context, string, bool) (AskedText, error)
			}{
				{"the text a reader of asks reads", service.TextWithAsks},
				{"the kept form", service.KeptText},
			} {
				before := len(renders())
				text, err := form.read(context.Background(), artifactID, false)
				counted := len(renders()) - before
				if err != nil {
					t.Fatalf("%s: read the document's text: %v", form.name, err)
				}
				if text.Markdown != document.markdown || text.Token != live {
					t.Errorf("%s: the read answered %q with token %q, want the version's markdown %q and the live document's token %q",
						form.name, text.Markdown, text.Token, document.markdown, live)
				}
				var asks []string
				for _, entry := range text.Asks {
					asks = append(asks, entry.BlockID)
				}
				if !slices.Equal(asks, document.asks) {
					t.Errorf("%s: the read answered asks for the blocks %q, want %q", form.name, asks, document.asks)
				}
				if counted != document.renders {
					t.Errorf("%s: the read rendered the document %d times, want %d", form.name, counted, document.renders)
				}
			}
		})
	}
}

// A room that has applied a browser's update and not yet stored it holds text no version does, so
// the read renders the room rather than serving the version's markdown.
func TestStoredTextReadRendersARoomHoldingAnUnstoredUpdate(t *testing.T) {
	// Not parallel: it records every document render the process makes in the package variable
	// documentRenders (countRenders), which refuses a second recorder and would count the renders of
	// the tests beside it.
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedSettledDocument(t, service, artifactID, "The quick brown fox.\n")
	// The peer's update is applied to the room and counted among the updates it has not stored
	// (queueUpdateClass), and its observer is held before persistence is handed it.
	release := holdPeerEdit(t, service, artifactID, &service.afterCreditUpdate, replaceRun("fox.", "dog."))
	defer release()

	renders := countRenders(t)
	markdown, token, err := service.TextWithToken(context.Background(), artifactID)
	counted := len(renders())
	if err != nil {
		t.Fatalf("read the document's text: %v", err)
	}
	if markdown != "The quick brown dog.\n" {
		t.Fatalf("the read answered %q, want the room's unstored text", markdown)
	}
	if token == "" {
		t.Fatal("the read answered no document token")
	}
	if counted != storedTextReadRendersUnserved {
		t.Fatalf("the read rendered the document %d times, want %d", counted, storedTextReadRendersUnserved)
	}
}

// An anchor write appends an update and writes no version, so the document's stamp moves past the
// one its latest version holds and the read renders the document.
func TestStoredTextReadRendersAfterAnAnchorWriteMovedTheStamp(t *testing.T) {
	// Not parallel: it records every document render the process makes in the package variable
	// documentRenders (countRenders), which refuses a second recorder and would count the renders of
	// the tests beside it.
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	const written = "The quick brown fox.\n"
	seedSettledDocument(t, service, artifactID, written)
	spec := MarkSpec{Kind: MarkComment, ID: "comment-1", By: model.Actor{Kind: "session", ID: "session-0123456789abcdef"}}
	if _, err := joinedMarkQuote(service, artifactID, spec, "brown", nil); err != nil {
		t.Fatalf("anchor a comment: %v", err)
	}

	renders := countRenders(t)
	markdown, token, err := service.TextWithToken(context.Background(), artifactID)
	counted := len(renders())
	if err != nil {
		t.Fatalf("read the document's text: %v", err)
	}
	// A mark renders nothing, so the text is the version's all the same; the stamp is not.
	if markdown != written {
		t.Fatalf("the read answered %q, want %q", markdown, written)
	}
	if token == "" {
		t.Fatal("the read answered no document token")
	}
	if counted != storedTextReadRendersUnserved {
		t.Fatalf("the read rendered the document %d times, want %d", counted, storedTextReadRendersUnserved)
	}
}

// A version another renderer wrote is not this build's to serve: its markdown and its token are
// what that renderer made of the tree, so the read renders the document itself.
func TestStoredTextReadRendersAVersionOfAnotherRendererVersion(t *testing.T) {
	// Not parallel: it records every document render the process makes in the package variable
	// documentRenders (countRenders), which refuses a second recorder and would count the renders of
	// the tests beside it.
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	const written = "The quick brown fox.\n"
	seedSettledDocument(t, service, artifactID, written)
	if _, err := service.store.Pool.Exec(context.Background(), `
		update artifact_versions set renderer_version = $2 where artifact_id = $1
	`, artifactID, pmdoc.RendererVersion+1); err != nil {
		t.Fatalf("stamp the version with another renderer version: %v", err)
	}

	renders := countRenders(t)
	markdown, token, err := service.TextWithToken(context.Background(), artifactID)
	counted := len(renders())
	if err != nil {
		t.Fatalf("read the document's text: %v", err)
	}
	if markdown != written {
		t.Fatalf("the read answered %q, want %q", markdown, written)
	}
	if token == "" {
		t.Fatal("the read answered no document token")
	}
	if counted != storedTextReadRendersUnserved {
		t.Fatalf("the read rendered the document %d times, want %d", counted, storedTextReadRendersUnserved)
	}
}

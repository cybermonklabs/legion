package docs

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/logtest"
)

// readBackSpec is a document holding a callout inside a list item, the tree a browser edit turns
// into one the renderer may render as markdown the parser refuses (R25 of this change).
const readBackSpec = "# Spec\n\n- Lead.\n\n  :::callout{#c1 kind=\"note\" title=\"T\"}\n  Gone.\n  :::\n"

// readBackCallout is the first callout of tree in document order, nil where it holds none.
func readBackCallout(tree *pmdoc.Node) *pmdoc.Node {
	var callout *pmdoc.Node
	pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
		if node.Type == "callout" {
			callout = node
			return false
		}
		return true
	})
	return callout
}

// readBackText is every text run under node, in document order.
func readBackText(node *pmdoc.Node) string {
	var text strings.Builder
	pmdoc.Walk(node, func(held *pmdoc.Node) bool {
		text.WriteString(held.Text)
		return true
	})
	return text.String()
}

// readBackFootnote is the text of the footnote definition callout holds as its one child, and
// whether it holds one: the tree the browser's first edit leaves.
func readBackFootnote(callout *pmdoc.Node) (string, bool) {
	if callout == nil || len(callout.Children) != 1 || callout.Children[0].Type != "footnote_definition" {
		return "", false
	}
	return readBackText(callout.Children[0]), true
}

// R25 of this change, on a document that is its own room (the api test
// TestABrowserEditThatDoesNotReadBackStoresNoVersion drives an issue room): no version the
// settlement writes is one the parser refuses. A browser moves the text of a callout inside a list
// item into a footnote definition inside the callout, and the room's settlement runs. Unless the
// renderer renders that tree as markdown that reads back (pmdoc.ParseRendering), the document
// holds no new version and its newest version still reads back, the log holds the settlement's
// line naming the document and the parser's error, and the stored document keeps the edit; the
// browser then moves the text back out and changes a word of it, and the next settlement writes
// the version, which reads back holding that word. Where the renderer renders the edited tree as
// markdown that reads back, the settlement writes that version instead. Serial: it swaps the
// default logger.
func TestASettlementWritesNoVersionThatTheParserRefusesAndTheNextOneThatReadsBack(t *testing.T) {
	logs := &storetest.LockedLog{}
	logtest.SetDefault(t, slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, readBackSpec)
	settleCurrentGeneration(t, service, artifactID)
	before := latestVersionNumber(t, service, artifactID)
	alice := model.Actor{Kind: "user", ID: "alice"}
	// browserEdit changes the live tree as a browser connected as alice does, whom the room
	// credits with it (creditContentChange).
	browserEdit := func(edit func(tree *pmdoc.Node)) {
		t.Helper()
		connection := service.nextConnection.Add(1)
		service.addConnection(artifactID, connection, alice)
		defer service.removeConnection(artifactID, connection)
		editLiveTree(t, service, artifactID, func(tree *pmdoc.Node) *pmdoc.Node {
			edit(tree)
			return tree
		})
	}

	browserEdit(func(tree *pmdoc.Node) {
		callout := readBackCallout(tree)
		if callout == nil {
			t.Fatal("the document holds no callout to move the text of")
		}
		callout.Children = []*pmdoc.Node{{
			Type: "footnote_definition", Attrs: pmdoc.Attrs{pmdoc.BlockIDAttr: "moved", "label": "n"}, Children: callout.Children,
		}}
	})
	rendering, err := renderTree(liveTree(t, service, artifactID))
	if err != nil {
		t.Fatalf("render the edited document: %v", err)
	}
	_, parsed := pmdoc.ParseRendering(rendering)
	settleCurrentGeneration(t, service, artifactID)
	latest, markdown := latestVersionNumber(t, service, artifactID), latestVersionMarkdown(t, service, artifactID)
	stored, storedErr := pmdoc.ParseRendering(markdown)

	if parsed == nil {
		if latest != before+1 {
			t.Fatalf("the renderer renders the edited tree as markdown that reads back, and the document's newest version is %d, want %d", latest, before+1)
		}
		if storedErr != nil {
			t.Fatalf("version %d does not read back: %v\n%s", latest, storedErr, markdown)
		}
		if text, held := readBackFootnote(readBackCallout(stored)); !held || text != "Gone." {
			t.Fatalf("version %d reads back with the footnote definition held %t holding %q, want the callout's text in it:\n%s", latest, held, text, markdown)
		}
		return
	}

	if latest != before {
		t.Fatalf("the settlement wrote version %d, though the rendering of the edited tree does not read back (%v); want the document left at version %d:\n%s",
			latest, parsed, before, markdown)
	}
	if storedErr != nil {
		t.Fatalf("the document's newest version, %d, does not read back: %v\n%s", latest, storedErr, markdown)
	}
	message := parsed.Error()
	quoted := strconv.Quote(message)
	if lines := logs.Lines(artifactID); !strings.Contains(lines, message) && !strings.Contains(lines, quoted[1:len(quoted)-1]) {
		t.Fatalf("the log holds no settlement line naming %s and the parser's error %q; its lines naming the document are:\n%s", artifactID, message, lines)
	}
	kept, err := service.storedTree(context.Background(), roomTarget(artifactID))
	if err != nil {
		t.Fatalf("read the stored document: %v", err)
	}
	if _, held := readBackFootnote(readBackCallout(kept)); !held {
		t.Fatal("the stored document lost the browser's edit: its callout holds no footnote definition")
	}

	// The browser moves the text back out and changes a word of it, so the markdown differs from
	// the version the document holds.
	browserEdit(func(tree *pmdoc.Node) {
		callout := readBackCallout(tree)
		if _, held := readBackFootnote(callout); !held {
			t.Fatal("the room lost the browser's first edit: its callout holds no footnote definition")
		}
		callout.Children = callout.Children[0].Children
		reworded := false
		pmdoc.Walk(callout, func(node *pmdoc.Node) bool {
			if node.Text == "Gone." {
				node.Text, reworded = "Kept.", true
				return false
			}
			return true
		})
		if !reworded {
			t.Fatalf("the callout's text holds no run %q to change a word of", "Gone.")
		}
	})
	settleCurrentGeneration(t, service, artifactID)
	latest, markdown = latestVersionNumber(t, service, artifactID), latestVersionMarkdown(t, service, artifactID)
	if latest != before+1 {
		t.Fatalf("after the browser moved the text back out and changed a word of it, the document's newest version is %d, want %d", latest, before+1)
	}
	written, err := pmdoc.ParseRendering(markdown)
	if err != nil {
		t.Fatalf("version %d does not read back: %v\n%s", latest, err, markdown)
	}
	if text := readBackText(readBackCallout(written)); text != "Kept." {
		t.Fatalf("version %d reads back with the callout's text %q, want the changed word, %q:\n%s", latest, text, "Kept.", markdown)
	}
}

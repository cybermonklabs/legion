package api

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// settlementReadsBackSpec is a spec holding a callout inside a list item, the tree a browser edit
// turns into one the renderer may render in a form the parser refuses (this issue's spec R25).
const settlementReadsBackSpec = "# Spec\n\nDispatch shows each record.\n\n- Lead.\n\n  :::callout{#c1 kind=\"note\" title=\"T\"}\n  Gone.\n  :::\n"

// settlementReadsBackNewest is the number and the markdown of the newest stored version of the
// document artifactID.
func settlementReadsBackNewest(t *testing.T, database *store.Store, artifactID string) (int, string) {
	t.Helper()
	var number int
	var markdown string
	if err := database.Pool.QueryRow(context.Background(),
		`select number, markdown from artifact_versions where artifact_id = $1 order by number desc limit 1`,
		artifactID).Scan(&number, &markdown); err != nil {
		t.Fatalf("read the newest version of %s: %v", artifactID, err)
	}
	return number, markdown
}

// settlementReadsBackCallout is the first callout of tree in document order, nil where it holds
// none.
func settlementReadsBackCallout(tree *pmdoc.Node) *pmdoc.Node {
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

// settlementReadsBackText is every text run under node, in document order.
func settlementReadsBackText(node *pmdoc.Node) string {
	var text strings.Builder
	pmdoc.Walk(node, func(held *pmdoc.Node) bool {
		text.WriteString(held.Text)
		return true
	})
	return text.String()
}

// settlementReadsBackFootnote is the text of the footnote definition callout holds as its one
// child, and whether it holds one: the tree the browser's first edit leaves.
func settlementReadsBackFootnote(callout *pmdoc.Node) (string, bool) {
	if callout == nil || len(callout.Children) != 1 || callout.Children[0].Type != "footnote_definition" {
		return "", false
	}
	return settlementReadsBackText(callout.Children[0]), true
}

// settlementReadsBackLogLine is the first line of logged that names room and holds the parser's
// error parsed, written as it is or as a text handler quotes it; "" where no line does.
func settlementReadsBackLogLine(logged, room string, parsed error) string {
	message := parsed.Error()
	quoted := strconv.Quote(message)
	escaped := quoted[1 : len(quoted)-1]
	for _, line := range strings.Split(logged, "\n") {
		if strings.Contains(line, room) && (strings.Contains(line, message) || strings.Contains(line, escaped)) {
			return line
		}
	}
	return ""
}

// c-settlement-reads-back (this issue's spec R25): no version Dispatch stores is one its own parser
// refuses. A browser peer connected to the document socket of a spec holding a callout inside a
// list item moves the callout's text into a footnote definition inside the callout and closes its
// connection, so the last peer's settlement runs. Unless the renderer renders that tree in a form
// that reads back (pmdoc.ParseRendering) as the same tree, the spec then holds no version 2, the
// log holds the settlement's line naming the room and the parser's error, and the newest version
// reads back; the peer then moves the text back out of the footnote definition, changes one word
// of it, and closes again, and the spec holds a version 2 that reads back and holds that word.
// Where the renderer renders that tree readably, version 2 is stored and reads back as the edited
// tree instead.
func TestABrowserEditThatDoesNotReadBackStoresNoVersion(t *testing.T) {
	logged := captureLog(t)
	documentService, handler, database := browserDocumentService(t)
	issue := createInteractionIssue(t, handler, "TEST", "A spec that a browser edits", settlementReadsBackSpec)
	spec := issue.PrimaryArtifactID
	room := artifactRoom(t, handler, spec)
	if newest, _ := settlementReadsBackNewest(t, database, spec); newest != 1 {
		t.Fatalf("the spec holds version %d before the browser edits it, want 1", newest)
	}

	peer := connectBrowserPeer(t, handler, documentService, spec)
	peer.edit(t, func(tree *pmdoc.Node) error {
		callout := settlementReadsBackCallout(tree)
		if callout == nil {
			return errors.New("the spec holds no callout to move the text of")
		}
		callout.Children = []*pmdoc.Node{{
			Type: "footnote_definition", Attrs: pmdoc.Attrs{pmdoc.BlockIDAttr: "moved", "label": "n"}, Children: callout.Children,
		}}
		return nil
	})
	peer.barrier(t)
	rendering := renderFragment(t, peer.doc, peer.fragment)
	readBack, parsed := pmdoc.ParseRendering(rendering)
	// The last peer's settlement is done when the server lets its connection go.
	peer.closeAndWait(t)
	newest, markdown := settlementReadsBackNewest(t, database, spec)

	if parsed == nil {
		rerendered, err := pmdoc.Render(readBack)
		if text, held := settlementReadsBackFootnote(settlementReadsBackCallout(readBack)); err != nil || rerendered != rendering || !held || text != "Gone." {
			t.Fatalf("the rendering of the edited tree reads back as another tree (render error %v, footnote held %t with %q):\n%s\nreads back as\n%s",
				err, held, text, rendering, rerendered)
		}
		if newest != 2 {
			t.Fatalf("the renderer renders the edited tree in a form that reads back, and the spec's newest version is %d, want 2", newest)
		}
		stored, err := pmdoc.ParseRendering(markdown)
		if err != nil {
			t.Fatalf("version 2 of the spec does not read back: %v\n%s", err, markdown)
		}
		if text, held := settlementReadsBackFootnote(settlementReadsBackCallout(stored)); !held || text != "Gone." {
			t.Fatalf("version 2 of the spec reads back with the footnote definition held %t holding %q, want the edited tree, the callout's text in its footnote definition:\n%s",
				held, text, markdown)
		}
		return
	}

	if newest != 1 {
		if _, err := pmdoc.ParseRendering(markdown); err != nil {
			t.Fatalf("the browser edit stored version %d, which the parser refuses: %v\n%s", newest, err, markdown)
		}
		t.Fatalf("the browser edit stored version %d, though the rendering of its tree does not read back (%v); want no version past 1", newest, parsed)
	}
	if _, err := pmdoc.ParseRendering(markdown); err != nil {
		t.Fatalf("the spec's newest version, 1, does not read back: %v\n%s", err, markdown)
	}
	if line := settlementReadsBackLogLine(logged.String(), room, parsed); line == "" {
		t.Fatalf("the log holds no settlement line naming the room %s and the parser's error %q; it holds:\n%s", room, parsed, logged.String())
	}

	// The room keeps the edit: the next peer reads the callout's text in its footnote definition,
	// moves it back out, and changes a word of it, so the markdown differs from version 1's.
	again := connectBrowserPeer(t, handler, documentService, spec)
	again.edit(t, func(tree *pmdoc.Node) error {
		callout := settlementReadsBackCallout(tree)
		if _, held := settlementReadsBackFootnote(callout); !held {
			return errors.New("the room lost the browser's first edit: its callout holds no footnote definition")
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
			return errors.New("the callout's text holds no run \"Gone.\" to change a word of")
		}
		return nil
	})
	again.barrier(t)
	again.closeAndWait(t)
	newest, markdown = settlementReadsBackNewest(t, database, spec)
	if newest != 2 {
		t.Fatalf("after the browser moved the text back out and changed a word of it, the spec's newest version is %d, want 2", newest)
	}
	stored, err := pmdoc.ParseRendering(markdown)
	if err != nil {
		t.Fatalf("version 2 of the spec does not read back: %v\n%s", err, markdown)
	}
	if text := settlementReadsBackText(settlementReadsBackCallout(stored)); text != "Kept." {
		t.Fatalf("version 2 of the spec reads back with the callout's text %q, want the changed word, %q:\n%s", text, "Kept.", markdown)
	}
}

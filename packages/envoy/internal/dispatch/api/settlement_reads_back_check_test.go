package api

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// settlementReadsBackSpec is a spec holding a callout inside a list item, the tree a browser edit
// turns into one the renderer may render in a form the parser refuses (this issue's spec R25).
const settlementReadsBackSpec = "# Spec\n\nDispatch shows each record.\n\n- Lead.\n\n  :::callout{#c1 kind=\"note\" title=\"T\"}\n  Gone.\n  :::\n"

// settlementReadsBackRefused is a document the parser refuses, which pmdoc's
// TestCorpusErrorsOfItemSpreadReadOtherwiseAreRefused pins: a footnote definition inside a callout
// inside a list item, in the spacing the browser editor reads otherwise. An earlier release stored
// such a version where a browser's edit left that tree.
const settlementReadsBackRefused = "- x\n  :::callout{}\n\n  [^g]: d\n"

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

// settlementReadsBackLogLine is the first line of logged that holds each of named and the parser's
// error parsed, written as it is or as a text handler quotes it; "" where no line does.
func settlementReadsBackLogLine(logged string, parsed error, named ...string) string {
	message := parsed.Error()
	quoted := strconv.Quote(message)
	escaped := quoted[1 : len(quoted)-1]
	for _, line := range strings.Split(logged, "\n") {
		lacks := func(text string) bool { return !strings.Contains(line, text) }
		if (lacks(message) && lacks(escaped)) || slices.ContainsFunc(named, lacks) {
			continue
		}
		return line
	}
	return ""
}

func settlementReadsBackPending(t *testing.T, database *store.Store, room string) bool {
	t.Helper()
	var pending bool
	if err := database.Pool.QueryRow(t.Context(),
		`select exists (select 1 from doc_settlements_pending where artifact_id = $1::uuid)`, room).Scan(&pending); err != nil {
		t.Fatalf("read the pending settlement of room %s: %v", room, err)
	}
	return pending
}

// c-settlement-reads-back (this issue's spec R25): no version Dispatch stores is one its own parser
// refuses, and the conversion passes over a stored version that does not read back, written
// before this work, with the parser's error. The first half is the settlement's
// (settlementReadsBackBrowserEdit), the second the conversion's (settlementReadsBackConversion).
func TestABrowserEditThatDoesNotReadBackStoresNoVersion(t *testing.T) {
	logged := captureLog(t)
	documentService, handler, database := browserDocumentService(t)
	release := holdUnconverted(t, database)
	issue := createInteractionIssue(t, handler, "TEST", "A spec that a browser edits", settlementReadsBackSpec)
	release()
	settlementReadsBackBrowserEdit(t, logged, documentService, handler, database, issue.Key, issue.PrimaryArtifactID, false, 1)
	settlementReadsBackConversion(t, logged, documentService, handler, database, issue.Key)
}

// A refusal in a shared room must not stop its other documents from taking versions.
func TestAnIssueRoomVersionsOnlyFragmentsThatReadBack(t *testing.T) {
	t.Run("born converted", func(t *testing.T) {
		logged := captureLog(t)
		service, handler, database := browserDocumentService(t)
		issue := createInteractionIssue(t, handler, "TEST", "A room with two edited fragments", settlementReadsBackSpec)
		settlementReadsBackBrowserEdit(t, logged, service, handler, database, issue.Key, issue.PrimaryArtifactID, true, 1)
	})
	t.Run("converted from a room with older updates", func(t *testing.T) {
		logged := captureLog(t)
		service, handler, database := browserDocumentService(t)
		ctx := t.Context()
		release := holdUnconverted(t, database)
		issue := createInteractionIssue(t, handler, "TEST", "A room with old history", settlementReadsBackSpec)
		release()
		spec := issue.PrimaryArtifactID
		if artifactRoom(t, handler, spec) != spec {
			t.Fatal("the issue must start with a separate spec room")
		}
		if err := service.SettleIssueDocuments(ctx, issue.Key); err != nil {
			t.Fatal(err)
		}
		persistence := docs.NewPgVersioned(database)
		tx, err := database.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		empty := crdt.EncodeStateAsUpdateV1(crdt.New(), nil)
		for range 39 {
			if _, err := persistence.AppendUpdateTx(ctx, tx, spec, empty, false); err != nil {
				t.Fatal(err)
			}
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if head, err := persistence.Head(ctx, spec); err != nil || head < 40 {
			t.Fatalf("the old room's head is %d, want at least 40: %v", head, err)
		}
		if result, err := service.ConvertIssue(ctx, issue.Key, docs.IssueRoomGate, false); err != nil || result.Outcome != docs.IssueConverted {
			t.Fatalf("convert the issue with the older room: %+v, %v", result, err)
		}
		settlementReadsBackBrowserEdit(t, logged, service, handler, database, issue.Key, spec, true, 2)
	})
}

// The first browser edit moves the callout's text into a footnote definition in the callout.
// The second restores the callout's structure and changes a word to force a new version.
func settlementReadsBackBrowserEdit(t *testing.T, logged *logBuffer, documentService *docs.Service, handler http.Handler, database *store.Store, issue, spec string, shared bool, beforeVersion int) {
	t.Helper()
	room := artifactRoom(t, handler, spec)
	if (room != spec) != shared {
		t.Fatalf("spec %s has room %s; want a shared room: %t", spec, room, shared)
	}
	if newest, _ := settlementReadsBackNewest(t, database, spec); newest != beforeVersion {
		t.Fatalf("the spec holds version %d before the browser edits it, want %d", newest, beforeVersion)
	}

	peer := connectBrowserPeer(t, handler, documentService, spec)
	own := peer.doc.GetXmlFragment(model.ArtifactFragmentOwn)
	var readable *pmdoc.Node
	const sibling = "A readable sibling edit."
	if shared {
		var err error
		readable, err = pmdoc.ParseRendering(sibling + "\n")
		if err != nil {
			t.Fatal(err)
		}
	}
	peer.transact(t, func(tx *crdt.Transaction, fragment *crdt.YXmlFragment) error {
		tree, err := pmdoc.ReadInTransaction(tx, fragment)
		if err != nil {
			return err
		}
		callout := settlementReadsBackCallout(tree)
		if callout == nil {
			return errors.New("the spec holds no callout to move the text of")
		}
		callout.Children = []*pmdoc.Node{{
			Type: "footnote_definition", Attrs: pmdoc.Attrs{pmdoc.BlockIDAttr: "moved", "label": "n"}, Children: callout.Children,
		}}
		if err := pmdoc.Update(tx, fragment, tree); err != nil {
			return err
		}
		if shared {
			return pmdoc.Update(tx, own, readable)
		}
		return nil
	})
	peer.barrier(t)
	rendering := renderFragment(t, peer.doc, peer.fragment)
	readBack, parsed := pmdoc.ParseRendering(rendering)
	// The last peer's settlement is done when the server lets its connection go.
	peer.closeAndWait(t)
	newest, markdown := settlementReadsBackNewest(t, database, spec)
	if shared {
		number, text := settlementReadsBackNewest(t, database, room)
		if number != 2 || strings.TrimSpace(text) != sibling {
			t.Fatalf("the readable sibling stored version %d with %q, want version 2 with %q", number, text, sibling)
		}
	}

	if parsed == nil {
		rerendered, err := pmdoc.Render(readBack)
		if text, held := settlementReadsBackFootnote(settlementReadsBackCallout(readBack)); err != nil || rerendered != rendering || !held || text != "Gone." {
			t.Fatalf("the rendering of the edited tree reads back as another tree (render error %v, footnote held %t with %q):\n%s\nreads back as\n%s",
				err, held, text, rendering, rerendered)
		}
		if newest != beforeVersion+1 {
			t.Fatalf("the renderer renders the edited tree in a form that reads back, and the spec's newest version is %d, want %d", newest, beforeVersion+1)
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

	if newest != beforeVersion {
		if _, err := pmdoc.ParseRendering(markdown); err != nil {
			t.Fatalf("the browser edit stored version %d, which the parser refuses: %v\n%s", newest, err, markdown)
		}
		t.Fatalf("the browser edit stored version %d even though it does not read back (%v); want no version past %d", newest, parsed, beforeVersion)
	}
	if _, err := pmdoc.ParseRendering(markdown); err != nil {
		t.Fatalf("the spec's newest version, 1, does not read back: %v\n%s", err, markdown)
	}
	if line := settlementReadsBackLogLine(logged.String(), parsed, "settlement writes no version", room, spec); line == "" {
		t.Fatalf("the log holds no settlement line naming the room %s and the parser's error %q; it holds:\n%s", room, parsed, logged.String())
	}
	if !settlementReadsBackPending(t, database, room) {
		t.Fatalf("the refusal cleared the pending-settlement row of room %s", room)
	}
	if shared {
		var dated bool
		if err := database.Pool.QueryRow(t.Context(), `
			select exists (select 1 from version_renders where artifact_id = $1 and number = 2 and at is not null)
		`, room).Scan(&dated); err != nil {
			t.Fatal(err)
		}
		if dated {
			t.Fatalf("room %s indexed the readable sibling before all fragments read back", room)
		}
		// The readable sibling advanced the tree's cursor. A retry without a new edit must still
		// see the spec's outstanding edit, not clear the pending row as if it had saved it.
		err := documentService.SettleIssueDocuments(t.Context(), issue)
		if err == nil || !settlementReadsBackPending(t, database, room) {
			t.Fatalf("retry of room %s no longer owes a settlement while its spec still does not read back: %v", room, err)
		}
		if newest, _ := settlementReadsBackNewest(t, database, spec); newest != beforeVersion {
			t.Fatalf("retry stored version %d of the still unreadable fragment, want %d", newest, beforeVersion)
		}
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
	if newest != beforeVersion+1 {
		t.Fatalf("after the browser restored the callout and changed a word, the spec's newest version is %d, want %d", newest, beforeVersion+1)
	}
	stored, err := pmdoc.ParseRendering(markdown)
	if err != nil {
		t.Fatalf("version 2 of the spec does not read back: %v\n%s", err, markdown)
	}
	if text := settlementReadsBackText(settlementReadsBackCallout(stored)); text != "Kept." {
		t.Fatalf("version 2 of the spec reads back with the callout's text %q, want the changed word, %q:\n%s", text, "Kept.", markdown)
	}
	if settlementReadsBackPending(t, database, room) {
		t.Fatalf("room %s remained pending after the spec read back", room)
	}
	if shared {
		if number, _ := settlementReadsBackNewest(t, database, room); number != 2 {
			t.Fatalf("the retry or repair wrote version %d of the unchanged sibling; want version 2", number)
		}
	}
}

// Insert a legacy version directly in the store. The conversion must pass over that issue,
// with the parser error in its log, and convert the issue whose browser edit was repaired.
func settlementReadsBackConversion(t *testing.T, logged *logBuffer, documentService *docs.Service, handler http.Handler, database *store.Store, readableIssue string) {
	t.Helper()
	ctx := t.Context()
	_, refusal := pmdoc.ParseRendering(settlementReadsBackRefused)
	if refusal == nil {
		t.Fatalf("the parser reads %q back, want it refused: the conversion's half proves nothing", settlementReadsBackRefused)
	}
	release := holdUnconverted(t, database)
	passed, passedTree := createRoomIssue(t, handler, database, "TEST", "A spec an earlier release stored unreadable", settlementReadsBackSpec, "")
	release()
	if passedTree != "" {
		t.Fatalf("issue %s was already converted: tree %s", passed.Key, passedTree)
	}
	// Clear any seed settlement debt before the direct insert. Otherwise the block-index tick
	// treats the live/stored difference as an unversioned browser edit and leaves it owed.
	if err := documentService.SettleIssueDocuments(ctx, passed.Key); err != nil {
		t.Fatalf("settle the newly created spec of %s: %v", passed.Key, err)
	}
	newest, _ := settlementReadsBackNewest(t, database, passed.PrimaryArtifactID)
	unreadable := newest + 1
	if _, err := database.Pool.Exec(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, authors)
		values ($1, $2, $3, '[{"kind":"user","id":"alice"}]')
	`, passed.PrimaryArtifactID, unreadable, settlementReadsBackRefused); err != nil {
		t.Fatalf("write version %d of the spec of %s as an earlier release stored it: %v", unreadable, passed.Key, err)
	}
	if _, err := documentService.ReconcileBlockIndex(ctx, docs.BlockIndexCursors{}, nil); err != nil {
		t.Fatalf("run the block-index tick over what the version's write left owed: %v", err)
	}
	if err := docs.NewIssueRoomStep(documentService, database, nil).Tick(ctx); err != nil {
		t.Fatalf("run the issue room step: %v", err)
	}

	if tree, err := issueTreeOf(ctx, database.Pool, passed.Key); err != nil || tree != "" {
		t.Fatalf("the conversion converted %s, whose newest spec version, %d, does not read back (%v): it holds the tree %q (%v)", passed.Key, unreadable, refusal, tree, err)
	}
	if line := settlementReadsBackLogLine(logged.String(), refusal, "passed over", passed.Key); line == "" {
		t.Fatalf("the log holds no line of the conversion passing over %s with the parser's error %q; it holds:\n%s", passed.Key, refusal, logged.String())
	}
	if tree, err := issueTreeOf(ctx, database.Pool, readableIssue); err != nil || tree == "" {
		t.Fatalf("the conversion did not convert %s, whose spec reads back: tree %q, error %v", readableIssue, tree, err)
	}
}

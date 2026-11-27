package docs

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// resetConnectionCloseCode is the code ygo closes a socket with whose update Server.OnPeerUpdate
// refused (sjawhar/ygo provider/websocket, wsCodeResetConnection), as @hocuspocus/server closes on
// a refused message.
const resetConnectionCloseCode = 4205

// A browser that held a document's own room when its issue converted keeps that room's Yjs
// document, and its provider reconnects with it (R6 of the issue-tree spec, c-room-cutover): the
// socket is admitted to the issue's room under the document's name, and the provider answers the
// room's sync step 1 with the whole of the document it kept, whose blocks stand under its room's
// own fragment. The issue's room refuses that update (oldRoomDocument) and closes the socket:
// the tree's own fragment reads as the conversion left it, the spec's fragment holds the spec
// once, and the room's stored history holds no update more. A write the same browser's next
// socket makes into the spec's fragment, on a document read from the room, is taken.
func TestAReconnectionThatKeptADocumentsOwnRoomWritesNothingIntoItsIssuesRoom(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	service, server := newTestServiceInstance(t, database)
	issue := createUnconvertedIssue(t, database, service, 1, "",
		"The spec's opening paragraph.\n\n:::requirement{#r1}\nThe requirement.\n:::\n",
		"The verification's paragraph.\n", "The plan's paragraph.\n")
	spec := issue.markdown[issue.spec]
	held := connectPeer(t, server.URL, issue.spec, "alice")
	if err := held.WaitForText(peerMarkdown, spec, roomWait(t)); err != nil {
		t.Fatal(err)
	}
	if err := service.waitForDurableAppends(ctx, issue.spec); err != nil {
		t.Fatalf("wait for the appends of the spec's own room: %v", err)
	}

	result, err := service.ConvertIssue(ctx, issue.key, IssueRoomGate, false)
	if err != nil || result.Outcome != IssueConverted {
		t.Fatalf("the conversion of %s answered %+v, %v, want it converted", issue.key, result, err)
	}
	tree, _ := treeOfIssue(t, database, issue.key)
	// The conversion's notice closes the old room on each process that hears it
	// (documentRoomsMoved); this process closes its own as that handler does.
	service.closeMovedRoom(ctx, issue.spec)
	select {
	case <-held.Ended:
	case <-time.After(roomWait(t)):
		t.Fatal("the socket held on the spec's own room was still open after the spec moved into its issue's room")
	}
	if kept := peerMarkdown(held); kept != spec {
		t.Fatalf("the held browser's document reads %q under its room's own fragment, want the spec it held, %q", kept, spec)
	}

	before := connectPeer(t, server.URL, tree, "alice")
	if err := before.WaitForText(fragmentMarkdown(issue.spec), spec, roomWait(t)); err != nil {
		t.Fatal(err)
	}
	own := fragmentMarkdown(fragmentName)(before)
	if strings.Contains(own, "The requirement.") {
		t.Fatalf("the tree's own fragment reads %q before the reconnection, want none of the spec's words: the conversion writes the spec into the spec's fragment", own)
	}
	stored := storedUpdateCount(t, database, tree)

	again := docstest.Dial(t, socketURL(server.URL, issue.spec), http.Header{"X-Dispatch-User": []string{"alice"}}, issue.spec, held.Doc)
	select {
	case <-again.Ended:
	case <-time.After(roomWait(t)):
		t.Fatalf("the room left open the socket that sent it the spec as the spec's own room held it; the tree's own fragment reads %q", fragmentMarkdown(fragmentName)(before))
	}
	if code := again.CloseFrame(); code != resetConnectionCloseCode {
		t.Errorf("the room closed the reconnection with the close frame %d, want %d, a refused update's", code, resetConnectionCloseCode)
	}

	after := connectPeer(t, server.URL, tree, "alice")
	if err := after.WaitForText(fragmentMarkdown(issue.spec), spec, roomWait(t)); err != nil {
		t.Fatal(err)
	}
	if landed := fragmentMarkdown(fragmentName)(after); landed != own {
		t.Errorf("the tree's own fragment reads %q after the reconnection, want %q, as the conversion left it: the old room's document landed in it", landed, own)
	}
	if now := storedUpdateCount(t, database, tree); now != stored {
		t.Errorf("the issue's room holds %d stored updates after the reconnection, want the %d it held before it", now, stored)
	}

	next := connectPeer(t, server.URL, issue.spec, "alice")
	if err := next.WaitForText(fragmentMarkdown(issue.spec), spec, roomWait(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := next.Send(writeParagraph(next.Doc, issue.spec, "Written after the move.")); err != nil {
		t.Fatalf("type into the spec's fragment on the browser's next socket: %v", err)
	}
	waitForStoredUpdates(t, database, tree, stored)
	if text, err := service.Text(ctx, issue.spec); err != nil || !strings.HasPrefix(text, spec) || !strings.Contains(text, "Written after the move.") {
		t.Errorf("the text of the spec reads %q (%v) after the browser's next socket wrote into its fragment, want the spec, %q, and then the paragraph it wrote", text, err, spec)
	}
}

// The refusal reads what the update holds, not where it lands: the first block typed into the own
// fragment of an issue's room that holds none is an update that, applied alone, leaves a block
// under that fragment too, and it is taken, since its id is no block's of a document of the room.
func TestAFirstBlockTypedIntoAnIssueRoomsOwnFragmentIsTaken(t *testing.T) {
	database := storetest.Open(t)
	_, server := newTestServiceInstance(t, database)
	room := createIssueRoom(t, database, 1)
	peer := connectPeer(t, server.URL, room.tree, "alice")
	if err := peer.WaitForText(fragmentMarkdown(room.plan), planSeed, roomWait(t)); err != nil {
		t.Fatal(err)
	}
	if own := peer.Doc.GetXmlFragment(fragmentName).Len(); own != 0 {
		t.Fatalf("the tree's own fragment holds %d blocks before the first is typed, want none", own)
	}
	stored := storedUpdateCount(t, database, room.tree)
	own := peer.Doc.GetXmlFragment(fragmentName)
	if _, err := peer.Send(func(txn *crdt.Transaction) {
		paragraph := crdt.NewYXmlElement("paragraph")
		own.InsertElement(txn, 0, paragraph)
		paragraph.SetAttribute(txn, pmdoc.BlockIDAttr, "own-first")
		run := crdt.NewYXmlText()
		paragraph.InsertText(txn, 0, run)
		run.Insert(txn, 0, "The tree's first block.", nil)
	}); err != nil {
		t.Fatalf("type the first block of the tree's own fragment: %v", err)
	}
	waitForStoredUpdates(t, database, room.tree, stored)
	reader := connectPeer(t, server.URL, room.tree, "alice")
	if err := reader.WaitForText(fragmentMarkdown(fragmentName), "The tree's first block.\n", roomWait(t)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-peer.Ended:
		t.Fatalf("the room closed the socket that typed the first block of its own fragment (close frame %d)", peer.CloseFrame())
	default:
	}
}

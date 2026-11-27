package docs

import (
	"bytes"
	"errors"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// errOldRoomDocument refuses a socket's update that holds a document of an issue's room as that
// document's own room held it before the document moved into the issue's room (oldRoomDocument).
// ygo closes the socket whose update the hook refuses (Server.OnPeerUpdate), as it closes any
// refused update's: the client's later edits build on the one refused.
var errOldRoomDocument = errors.New("the update holds a document as its own room held it before the document moved into its issue's room")

// oldRoomDocument answers, for a peer's update to an issue's room, the document of that room the
// update holds as the document's own room held it, with one block of it, and "" for every other
// update.
//
// A browser that held a document's own room when the document moved into its issue's room (R6 of
// the issue-tree spec) keeps that room's Yjs document, and its provider reconnects with it: the
// socket is admitted to the issue's room under the document's name (ServeHTTP, roomAliasWriter),
// and the provider answers the room's sync step 1 with everything the room lacks of its copy,
// which is the whole of the old room's document, since the issue room's history is the
// conversion's own seed. That document's blocks stand under its room's own fragment
// (fragmentName), and the issue room's fragment of that name is the tree artifact's own: applied,
// the update would write the moved document a second time, into the tree's own fragment, beside
// the copy the conversion wrote into the document's fragment. The append gate refuses a server's
// own write built on such a room (ErrRoomMoved, appendUpdateTxClass); this refuses a socket's.
//
// Such an update is told by what it holds alone: applied to an empty document it leaves blocks
// under the own fragment - every other update a socket of the room sends names items the room
// already holds as their neighbours or their parents, and leaves nothing there - and one of those
// blocks carries an id a document's fragment of the room holds, which the conversion kept
// (issueConversion.remint gives a fresh id only to a block whose id a fragment before its own
// holds). A first block typed into an own fragment that holds none is no such update: its id is
// its own. A room that is its own document holds no document's fragment, and no update to it is
// read here; nor is one whose bytes do not name the own fragment, which an item written into a
// fragment that already holds one never does, so a keystroke costs one scan of its own bytes.
func (s *Service) oldRoomDocument(info websocket.PeerUpdateInfo) (document, block string) {
	layout := s.layoutOf(info.Doc)
	if len(layout) == 0 || !bytes.Contains(info.Update, []byte(fragmentName)) {
		return "", ""
	}
	alone := newDocumentCopy()
	if err := crdt.ApplyUpdateV1(alone, info.Update, nil); err != nil {
		// The room's own apply refuses it, and answers the peer as it answers any such update.
		return "", ""
	}
	ids := blockIDsUnder(alone.GetXmlFragment(fragmentName), nil)
	if len(ids) == 0 {
		return "", ""
	}
	tree, counts, err := s.liveFragmentsTree(info.Room, info.Doc, layout.fragments())
	if err != nil {
		return "", ""
	}
	documents := layout.blockDocuments(info.Room, tree, counts)
	for _, id := range ids {
		if held := documents[id]; held != "" {
			return held, id
		}
	}
	return "", ""
}

// blockIDsUnder appends to ids the block id of every element under fragment, in document order.
// fragment is of a document only its caller reads.
func blockIDsUnder(fragment *crdt.YXmlFragment, ids []string) []string {
	for _, child := range fragment.Children() {
		element, ok := child.(*crdt.YXmlElement)
		if !ok {
			continue
		}
		if id, held := element.GetAttribute(pmdoc.BlockIDAttr); held && id != "" {
			ids = append(ids, id)
		}
		ids = blockIDsUnder(&element.YXmlFragment, ids)
	}
	return ids
}

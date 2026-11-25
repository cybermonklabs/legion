package docs

import (
	"log/slog"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// readsBack reports whether markdown, the rendering this settlement would store as a version of
// document, reads back through the parser every read of a stored version goes through
// (pmdoc.ParseRendering), as a write of a whole document reads its own rendering back
// (pmdoc.ParseWritten): no version Dispatch stores is one its own parser refuses (R25 of this
// change). A browser edit can leave a tree that renders as markdown the parser refuses - a footnote
// definition inside a callout inside a list item - and no route refused it, so the settlement does
// not refuse it either: where the parser refuses the rendering it writes no version and logs one
// line naming the room, the document and the parser's error. It commits all the same what it read
// and stored - the room's updates, its stamp and its repairs - so the room keeps the edit, the
// document's newest version stays the last one that read back, and the document's pending
// authors stay for the next version. Content has changed since that version
// (contentChangedSinceVersion), so the next settlement whose tree reads back writes it. Every error
// the parser answers is such a refusal, a fault of its own (pmdoc.ErrPanic) among them: a version
// of that markdown would read back no better for it.
func (st *settlement) readsBack(document, markdown string) bool {
	if _, err := pmdoc.ParseRendering(markdown); err != nil {
		slog.Warn("dispatch: settlement writes no version of a document whose rendering does not read back; the room keeps the edit for the next settlement whose tree reads back",
			"room", st.room, "artifact", document, "error", err)
		return false
	}
	return true
}

// holdUnreadableRoom marks none of fragments changed where the rendering of a fragment an issue
// room's settlement would version does not read back (readsBack), read in the room's order up to
// the first that does not: such a settlement writes no version of any document of the room. The
// room's one pass indexes every fragment's rendering at the version of that fragment's document
// (commitRoomVersions, indexRoom), so a fragment left at its latest version while its text moved
// on would have that text indexed, and stated, as that version's, which no version holds: the
// dating indexOwedRoom refuses for a room whose text is ahead of its versions. So the whole room
// waits for the next settlement whose tree reads back.
func (st *settlement) holdUnreadableRoom(fragments []roomFragment) {
	for _, fragment := range fragments {
		if fragment.changed && !st.readsBack(fragment.document, fragment.markdown) {
			for index := range fragments {
				fragments[index].changed = false
			}
			return
		}
	}
}

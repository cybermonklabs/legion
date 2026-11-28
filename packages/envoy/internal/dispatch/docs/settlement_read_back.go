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
// not refuse it either: where the parser refuses the rendering it writes no version of document
// and logs one line naming the room, the document and the parser's error. It records no failure
// and carries no approval, and commits all the same what it read and stored - the room's updates,
// its stamp and its repairs - so the room keeps the edit, the document's newest version stays the
// last one that read back, and the document's pending authors stay for the next version. It leaves
// the room's pending-settlement row standing (commitVersion skips clearSettlementPending), so the
// document stays owed and the resumption arms its settlement again (RunSettlementResumption).
// Content has changed since that version (contentChangedSinceVersion), so the next settlement
// whose tree reads back writes it. Every error the parser answers is such a refusal, a fault of
// its own (pmdoc.ErrPanic) among them: a version of that markdown would read back no better for
// it.
func (st *settlement) readsBack(document, markdown string) bool {
	if _, err := pmdoc.ParseRendering(markdown); err != nil {
		slog.Warn("dispatch: settlement writes no version of a document whose rendering does not read back; the room keeps the edit for the next settlement whose tree reads back",
			"room", st.room, "artifact", document, "error", err)
		return false
	}
	return true
}

// holdUnreadableFragments checks each rendering the settlement would store. It suppresses only
// the versions the parser refuses and reports whether it suppressed any, so the settlement
// can leave the room's pending row and block index owed.
// It also rechecks divergent fragments behind the tree's version: one its latest version does not
// hold (roomVersion.holds). Compaction can fold the content-change flag away once the readable
// tree fragment advances that version's cursor.
func (st *settlement) holdUnreadableFragments(fragments []roomFragment) bool {
	held := false
	for index := range fragments {
		fragment := &fragments[index]
		ahead := fragment.latest.cursor < st.latest.docUpdateVersion && !fragment.latest.holds(fragment.markdown)
		if (fragment.changed || ahead) && !st.readsBack(fragment.document, fragment.markdown) {
			fragment.changed = false
			held = true
		}
	}
	return held
}

// readsBackAsVersion reports whether markdown, the rendering a version write of a request would
// store as a version of document - writer names it in the log: a snapshot (SnapshotVersion,
// SettleVersion, the pin of an anchored comment or ask) or a named version (NamedVersion, the
// accept of a suggestion) - reads back through the parser every read of a stored version goes
// through (pmdoc.ParseRendering), as the settlement's does (readsBack): no version Dispatch stores
// is one its own parser refuses (R25). Where the parser refuses it, the write stores no version,
// answers the document's newest version, which is the last one that read back, and logs one line
// naming the writer, the document and the parser's error; the room keeps the edit, so the next
// version whose tree reads back stores it. An edit that writes the document whole reads its own
// rendering back already (pmdoc.ParseWritten).
func readsBackAsVersion(writer, document, markdown string) bool {
	if _, err := pmdoc.ParseRendering(markdown); err != nil {
		slog.Warn("dispatch: a version write stores no version of a document whose rendering does not read back; the room keeps the edit for the next version whose tree reads back",
			"writer", writer, "artifact", document, "error", err)
		return false
	}
	return true
}

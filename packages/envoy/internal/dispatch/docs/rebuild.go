package docs

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/persistence"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// RebuildReport describes the durable history a rebuild replaced. SourceVersion is the artifact
// version the fresh document state was built from - the latest saved one, or the one a rebuild
// from supplied markdown that changed the document wrote; a rebuild of a history that loads,
// which is built from that history's own text, can hold edits newer than it that settlement has
// not yet written, and a closed issue's rebuild holds the close's ask states (RebuildDocument).
// Head is the new monotonic update version. ValidationError says why the history was rebuilt:
// ygo's refusal of it, or how near a cap it stood.
type RebuildReport struct {
	Head               int64  `json:"head"`
	RemovedCheckpoints int64  `json:"removed_checkpoints"`
	RemovedSnapshots   int64  `json:"removed_snapshots"`
	RemovedUpdates     int64  `json:"removed_updates"`
	SourceVersion      int    `json:"source_version"`
	ValidationError    string `json:"validation_error"`
}

// smallestReplacementAdds is what the smallest replacement a write can make adds to a document's
// struct total, as refuseUndecodable bounds a write's updates (updatesAdd): one character
// replaced by another, the new item and the two items the delete and the insert can split off
// between them.
const smallestReplacementAdds = 3

// nearTheStateCap is how much state headroom a document may have and still be offered the
// rebuild: the bytes a browser's own update may add past the document bound (maxPeerByteCreep,
// 64 KiB).
//
// It was 64 bytes, a write's smallest replacement - one character inserted and the headers of the
// structs it splits - which asked whether a one-character edit would pass the cap. That is not what the room refuses. The hook refuses a browser's update whose content
// passes what is left of the cap (ruleStateCap, DOC_NEEDS_REBUILD), so a room with 10 KiB of
// headroom drops a person's 20 KiB paste and the editor offers the rebuild - and the route then
// answered 409 DOCUMENT_LIVE, because 10 KiB is far more than 64 bytes. The person's edit was
// dropped and the remedy offered failed (dispatch://INFRA-179, review 6's finding 4, reproduced
// by TestR6Finding4StateCapRefusalYetNotNearACap).
//
// The figure is the room's own: 64 KiB is what a browser may add past the document bound, so a
// room with less than that above its state is one whose person meets the refusal for an ordinary
// edit, and a room with more than that is one whose refusals are of pastes larger than any
// allowance a full document has. Those are refused for the edit's own size, which a smaller edit
// gets past, so the hook closes them DOC_TOO_LARGE and offers no rebuild (nearACapUnencoded): a
// paste of 200 KiB into a room with 100 KiB of headroom was closed DOC_NEEDS_REBUILD, and the
// rebuild it offered was answered 409 DOCUMENT_LIVE
// (TestR7AStateCapRefusalOfAPastePastTheMarginIsOfferedARebuildTheRouteRefuses, review 7).
const nearTheStateCap = maxPeerByteCreep

// wholeTextWriteCeiling is more structs than the near-cap measure (wholeTextWriteAdds) counts for
// any document a whole-text upload can write, so a document holding maxUpdateItems less this many
// structs or fewer is far from the decode cap whatever its text, and nearTheDecodeCap reads no
// tree for it. An upload is held to pmdoc.MaxDocumentElements, and of the nine documents measured
// at that bound the heaviest, one paragraph of bold italic runs each carrying an anchor mark,
// counts 360,424 structs, 16,384 headings 163,840 and 16,384 paragraphs 131,072
// (TestNoDocumentAnUploadCanWriteCountsPastTheWholeTextWriteCeiling, which holds each under this).
// A document browsers grew past the element bound takes no whole-text upload at all, so the
// decode cap is not what refuses one.
const wholeTextWriteCeiling = maxUpdateItems / 2

// wholeTextWriteAdds is how many structs a write replacing tree, a document's whole current text,
// with text of its own shape adds to the document's struct total at most, as refuseUndecodable
// counts a write (updatesAdd): the structs a fresh encode of tree holds, which is what the write
// inserts where it replaces a block (pmdoc.Update writes a new block as encodeDocumentTree
// writes the whole document), and two for each text of tree, for each of its runs and for each
// mark a run carries, the structs the write splits off the ones it deletes or formats. A text it
// keeps is written as one delete and one insert between the text's common prefix and suffix
// (simpleDiff), a range that splits only the two items its ends fall inside; then each run
// whose marks changed is formatted, which splits the two items at the run's ends, and each mark a
// run no longer carries is cleared, a format at each end of it. What the write deletes whole adds
// nothing: ygo marks an item deleted where it stands.
//
// Measured on the document the decode cap refused (dispatch://INFRA-179): 3,000 paragraphs, whose
// fresh encode holds 12,000 structs, and 2 for each of 3,000 texts and 3,000 runs, 24,000 in all,
// where each of the agent's uploads added 6,000 (12,000 the first time it rewrote a paragraph,
// splitting the paragraph's text) (TestADocumentWholeUploadsCarriedPastTheCapIsNearAndItsRebuildTakesTheUpload).
func wholeTextWriteAdds(tree *pmdoc.Node) (uint64, error) {
	fresh := crdt.New()
	fragment := fresh.GetXmlFragment(fragmentName)
	if err := fresh.TransactE(func(txn *crdt.Transaction) error { return pmdoc.Update(txn, fragment, tree) }); err != nil {
		return 0, err
	}
	adds := uint64(fresh.StructCount())
	pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
		if node.Type == "text" {
			adds += 2 + 2*uint64(len(node.Marks))
			return true
		}
		// Consecutive text runs are one text (pmdoc's normalizePNodeContent).
		inText := false
		for _, child := range node.Children {
			if child.Type == "text" && !inText {
				adds += 2
			}
			inText = child.Type == "text"
		}
		return true
	})
	return adds, nil
}

// wholeTextWriteFloor is a floor under wholeTextWriteAdds(tree) that writes nothing: a walk of
// tree counting the one item a fresh encode holds for each element and each text (pmdoc.Update
// inserts one ygo type for each), and the splits wholeTextWriteAdds counts for each text, run and
// mark, which it counts the same way. What the floor leaves out - the attributes, the strings,
// the formats - a fresh encode holds as well, so the floor never passes the measure.
func wholeTextWriteFloor(tree *pmdoc.Node) uint64 {
	var floor uint64
	pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
		if node.Type == "text" {
			floor += 2 + 2*uint64(len(node.Marks))
			return true
		}
		if node.Type != "doc" {
			floor++
		}
		inText := false
		for _, child := range node.Children {
			if child.Type == "text" && !inText {
				floor += 1 + 2
			}
			inText = child.Type == "text"
		}
		return true
	})
	return floor
}

// measuredWholeText is the whole-text count nearTheDecodeCap reads for the rebuild route and the
// decode cap's refusal: tree read, and wholeTextWriteAdds of it, a fresh write of the whole text.
// A tree that does not read or does not write is no whole-text write to count.
func measuredWholeText(tree func() (*pmdoc.Node, error)) func() (uint64, bool) {
	return func() (uint64, bool) {
		current, err := tree()
		if err != nil {
			return 0, false
		}
		adds, err := wholeTextWriteAdds(current)
		return adds, err == nil
	}
}

// nearTheDecodeCap reports whether a document holding structs structs as a load decodes them
// (structCeiling) stands so near the decode cap that a write it must still take is refused, and
// why: the smallest replacement any write makes (smallestReplacementAdds), or a write of its whole
// text in its own shape (wholeTextWriteAdds), would carry it past maxUpdateItems, which
// refuseUndecodable refuses. wholeText counts what that write adds - the measure itself
// (measuredWholeText), or for a document's read a floor under it that the read already holds
// (NearDecodeCap) - and is called only where the answer turns on it: a document further from
// the cap than wholeTextWriteCeiling is far without it. A whole text it cannot count leaves the
// smallest replacement alone to decide.
//
// It is the rebuild route's own test of the decode cap (nearACap), the one the decode cap's
// refusal of a write names the rebuild by (refuseUndecodable), and the one a document's read
// reports to the page with its floor (NearDecodeCap), which can only answer near where the route
// does.
//
// It was the smallest replacement alone, which a document of 3,000 paragraphs an agent rewrote
// whole until the cap refused its upload, at 1,047,000 structs, never met: a small write still
// landed, the agent's every upload was refused, and the route answered that the document loads
// and there was nothing to rebuild (dispatch://INFRA-179).
func nearTheDecodeCap(structs uint64, wholeText func() (adds uint64, counted bool)) (why string, near bool) {
	if structs+smallestReplacementAdds > maxUpdateItems {
		return fmt.Sprintf("the document's live state holds %d structs, and a write's smallest replacement, %d more, would carry it past the %d ygo decodes in one update",
			structs, smallestReplacementAdds, maxUpdateItems), true
	}
	if structs+wholeTextWriteCeiling <= maxUpdateItems {
		return "", false
	}
	adds, counted := wholeText()
	if !counted || structs+adds <= maxUpdateItems {
		return "", false
	}
	return fmt.Sprintf("the document's live state holds %d structs, and a write of its whole text counts %d more, which would carry it past the %d ygo decodes in one update",
		structs, adds, maxUpdateItems), true
}

// NearDecodeCap reports whether the rebuild route takes artifactID because it stands near the
// decode cap, and why, so the page can offer the rebuild to a person who has met no refusal of
// their own: an agent's uploads are what a document past the cap by its history refuses, while
// that person's typing still lands. It is nearTheDecodeCap, the route's own test, read from what
// the service holds of the document without encoding or writing anything, and with a floor under
// the whole-text count in place of the fresh write the route makes (wholeTextWriteAdds), so it
// answers near only where the route does, and may answer far where the route would take the
// document:
//
//   - a room this server holds: its struct total (structCeiling, a walk of its client slices, as
//     the route counts that room), and one struct for each of its top-level blocks, which its
//     replica's latest rendering records (pmdoc.Layout.Blocks) - a block is at least the one
//     element a fresh encode holds for it. A room whose replica holds no rendering, a document
//     outside the schema, counts no blocks.
//   - otherwise its stored state, which the cold read decoded and judged once a stamp with the
//     tree it rendered (foldLoaded, wholeTextWriteFloor), and keeps beside that rendering
//     (documentRead.nearDecodeCap), as the route decodes the stored history (rebuildSource). A
//     document with no stored state, or one whose tree the rendering refuses, is answered far;
//     one whose history does not load is the read's error, ErrDocumentUnloadable, which the
//     rebuild takes on its own.
//
// It answers the decode cap alone. The route also takes a document near the state cap or the scan
// total, whose refusals the browser meets on its own edits (DOC_NEEDS_REBUILD), and the route's
// scan test encodes the whole state and decodes it again, which no read pays for.
func (s *Service) NearDecodeCap(ctx context.Context, artifactID string) (why string, near bool, err error) {
	if live := s.srv.GetDoc(artifactID); live != nil {
		why, near = nearTheDecodeCap(structCeiling(live), func() (uint64, bool) { return s.roomBlocks(live) })
		return why, near, nil
	}
	read, _, err := s.coldRead(ctx, artifactID)
	if err != nil || read == nil {
		return "", false, err
	}
	return read.nearDecodeCap, read.nearDecodeCap != "", nil
}

// roomBlocks is how many top-level blocks live's replica last rendered (pmdoc.Layout.Blocks), and
// whether it holds a rendering to count: read under the replica's lock, which its update observer
// holds across one update's render, without reading or rendering the document.
func (s *Service) roomBlocks(live *crdt.Doc) (uint64, bool) {
	replica := s.replicaOf(live)
	if replica == nil {
		return 0, false
	}
	replica.mu.Lock()
	rendering := replica.rendering
	replica.mu.Unlock()
	if rendering == nil || rendering.layout == nil {
		return 0, false
	}
	return uint64(rendering.layout.Blocks()), true
}

// nearACap reports whether doc, a document's decoded history or its live room, stands so near a
// cap that it must be rebuilt, and why. Near the decode cap is nearTheDecodeCap of doc's struct
// total as the decode cap weighs it (structCeiling) and its tree: a write it must still take, a
// write of its whole text or a write's smallest replacement, would carry it past the cap, which
// refuseUndecodable refuses, as it refuses the write of a document no room holds by the count of
// its fork, a decode of the same history (leavesRoomPastDecodeCap). Near the scan total means a
// re-scan of its state (rescan) scans at least roomScanTotal less honestHandoffScans items, a
// state the decode refuses counting as no answer.
//
// The struct total costs neither an encode nor a decode (crdt.Doc.StructCount), and the tree is
// read and written afresh only for a document within wholeTextWriteCeiling of the cap. The scan
// test encodes doc's state and decodes it afresh, a whole-document decode like any other, so the
// check holds bytes of the pod's load concurrency (holdLoad) from before that encode until it
// answers: its own where ctx carries none, as the check of a live room makes it
// (RebuildDocument, which weighs it at a whole copy of that room, copyOfRoom), and the caller's
// where ctx carries one, which covers it already (rebuildSource). A wait ctx ends is its error,
// and no answer.
func nearACap(ctx context.Context, doc *crdt.Doc, bytes int64, tracked int64) (why string, near bool, err error) {
	release, err := holdLoad(ctx, bytes)
	if err != nil {
		return "", false, err
	}
	defer release()
	if why, near := nearTheDecodeCap(structCeiling(doc), measuredWholeText(func() (*pmdoc.Node, error) { return treeOf(doc) })); near {
		return why, true, nil
	}
	state := crdt.EncodeStateAsUpdateV1(doc, nil)
	// The state cap, which is what the room's hook refuses a browser's update by (ruleStateCap,
	// DOC_NEEDS_REBUILD) and so what the dashboard's rebuild offer answers: a document at it took
	// that offer and was told 409 DOCUMENT_LIVE, because this check read the struct count and the
	// scan total alone - a room may reach the cap with few structs, by map keys or by long
	// content (dispatch://INFRA-179, review 5820480859's finding 6). The larger of the two
	// figures decides: the state this encode holds, and the room's own tracked state where the
	// caller knows it, which runs at or above the encode between re-bases and is what the hook
	// compares (roomWeight's note on tracked state).
	held := int64(len(state))
	if tracked > held {
		held = tracked
	}
	if held+nearTheStateCap > maxRoomStateBytes {
		return fmt.Sprintf("the document's live state holds %d bytes, within %d of the %d one room may hold, so the room refuses a browser's ordinary edit",
			held, nearTheStateCap, maxRoomStateBytes), true, nil
	}
	if scans, err := rescan(state); err == nil && scans+honestHandoffScans >= roomScanTotal {
		return fmt.Sprintf("a re-scan of the document's live state scans %d items, within %d of the %d a room's history may scan",
			scans, honestHandoffScans, roomScanTotal), true, nil
	}
	return "", false, nil
}

// nearACapUnencoded reports whether doc, a room resident in this server, stands near a cap by the
// two of nearACap's tests that read neither its state nor its tree: its struct total as the
// decode cap weighs it (structCeiling) against the decode cap with a write's smallest
// replacement, its whole text counted as nothing (nearTheDecodeCap), and tracked, the bytes of
// state the room is
// tracked at (roomApplies.stateBytes), against the state cap's margin (nearTheStateCap). nearACap
// makes both tests of the same room on the same figures, and each of its own readings can only
// come out nearer - its encode is read beside tracked and the larger decides, and its whole-text
// count adds to the smallest replacement - so where this answers near, the route takes the room
// (RebuildDocument).
//
// It is the peer update hook's reading, which may encode nothing, decode nothing, read no tree
// and make the replica's weak pointer strong for none of it (roomWeighing): the struct total is
// a walk of the store's client slices and a read of its pending queue, and tracked is a field.
// A refusal of a browser's update offers the rebuild only where this answers near
// (peerRule.refusal), so no close offers a rebuild the route answers 409 DOCUMENT_LIVE: a
// state-cap refusal of a paste past the margin and a decode-cap refusal of a frame past the
// structs' share both closed DOC_NEEDS_REBUILD in rooms this route refused (the
// TestR7A...RefusalIsOfferedARebuildTheRouteRefuses tests, dispatch://INFRA-179, review 7).
//
// It answers far where the route may still take the room by its whole text or its scan total. A
// refusal there is of the edit's own size, and its remedy is a smaller edit, which a room this
// answers far still takes: at least smallestReplacementAdds items of decode headroom, of which
// the structs' share admits a one-struct keystroke and the splits' share the item it cuts
// (capShares), and nearTheStateCap bytes of state. The page offers such a room the rebuild on its
// own reading of the decode cap (NearDecodeCap), which counts the room's blocks as well.
func nearACapUnencoded(doc *crdt.Doc, tracked int64) bool {
	if tracked+nearTheStateCap > maxRoomStateBytes {
		return true
	}
	_, near := nearTheDecodeCap(structCeiling(doc), func() (uint64, bool) { return 0, false })
	return near
}

// RebuildDocument replaces a history no write can change with one fresh update, inside the
// transaction the context's ledger joined: a history ygo cannot decode, built afresh from its
// latest saved markdown or supplied markdown; a history that decodes but parks more than a room's
// document may (parkedPastRoomCaps), which no room loads, built from its own text or supplied
// markdown, the fresh seed holding nothing it parked; and a history that loads but stands so near
// the decode cap or the scan total that a write it must still take is refused (nearACap), built
// from its own text or supplied markdown. It is deliberately never a load fallback: a history
// that loads can contain unsettled edits newer than its latest version, so one further from both
// caps than that is refused unchanged, and one that is rebuilt keeps those edits, since what it is
// built from is its own tree. Every path keeps the block ids, the open anchors and the margin
// records of what it is built from, as far as that names them (rebuild_keeps.go).
//
// A document near a cap cannot take the writes its writers make: near the decode cap, a write of
// its whole text is refused while a small one may still land, and at the cap every write that adds
// a struct is; a browser's edit is refused by the room's totals; and only a rebuild leaves it
// small. A write that only deletes can land, since a delete range splits an item only where one of
// its ends falls inside that item, so a range covering whole items splits none of them and adds no
// struct (sjawhar/ygo v1.55.1-sami.7, crdt/yarray.go:829-846), and it leaves the state as large as
// it was: the document's own text is what a trim shortens, not its history. Supplied markdown does
// not widen this: a history further from both caps is refused with or without it, and is trimmed
// by a write.
//
// Every refusal comes before the first write: a room resident in this server whose document is far
// from both caps, a room resident in another envoy-dispatch process (a holds query,
// roomQueries.ask), a history a room loads that is not near a cap, a history another rebuild or an
// append moved past the one this rebuild loaded (RebuildTx), a closed issue (for supplied
// markdown, which changes the document), a tree it would be built from - the history's own or the
// latest version's - holding an element this build does not know (ErrDocNewerContent, as every
// read of it answers), and markdown that is not a Proof document, drops an open ask block,
// introduces a block pointer at a block the attribute may not name (refuseBlockPointers), or
// introduces a check block that breaks a rule of a check CI proves (refuseCICheckBlocks), both
// against the latest version. Supplied markdown is caller text: it is parsed as an upload is, and
// its rendering is weighed as a new document's is (SeedText). The latest version is the server's
// own rendering of the document, read back as every stored rendering is (pmdoc.ParseRendering), so
// a document whose version is past what one write may make - stored before that bound, or grown
// past it by browser edits - still rebuilds, and so does one holding an element this build reads
// but may not create yet, which the seed puts back rather than creates (rebuiltSeed,
// pmdoc.UpdateHolding), as it puts back such an element a history's own tree holds. Supplied
// markdown that differs from the latest version is a document change, so the same transaction
// writes its version, which moves the open approval request to it (writeVersionTx,
// MoveApprovalAsk); the VersionResult is what the caller's artifact.version event names. A
// failure anywhere rolls the whole rebuild back with the transaction. The room refuses loads
// until that transaction ends (Ledger.holdRebuild): a load before the commit would read the old
// history, and a second rebuild would preflight against it.
//
// A document near a cap is one people are typing into when they are refused, so its room is
// usually resident, and a refusal of every resident room would leave no way to rebuild it. Its
// room is closed instead, through evictBeforeRebuild, which flushes what the room holds into the
// stored history before it closes; the history this rebuild then reads holds every edit the room
// took. A room a Server.Apply is inside its callback on is left open and the rebuild refused, so
// no commit of the server's own lands beside the history this replaces.
// The browsers connected to it reconnect, and once the rebuild commits they load the rebuilt
// document and are closed as holders of the replaced history (below), dropping their unsent
// edits. A resident room of a document far from both caps is refused with ErrDocumentLive, as
// before: its document can be trimmed by a write.
//
// The fresh history lists the clients of the history it replaces, and the clock each had reached,
// under retiredMapName (rebuiltSeed), and the room refuses an update carrying any of their structs
// below that clock (weighPeerUpdate). A browser that held the document before the rebuild still
// holds the old history, and when it reconnects, the room holding none of it, its whole-state
// message hands all of it back: applied, the old document would sit beside the rebuilt one, every
// block twice, and the old history's structs would carry the room back toward the cap. Refused
// with DocumentTooLargeCloseCode and DocumentRebuiltCloseReason, the editor drops what it
// sent and builds its document afresh from the rebuilt room, and drops its saved rows typed
// against the old history as rows of a rebuilt document (pending-sync.ts, parkedClients). Before
// this list a rebuild of a history that cannot load had no such guard: the hook took the old
// history back unless the struct total refused it, which a browser's own copy, whose deleted runs
// Yjs merges, need not pass.
//
// A rebuild of a closed issue's document writes the close's ask states into the tree it seeds, as
// the close's final sweep writes them into the issue's other documents (closedIssueAskState), and
// marks the document owing the settlement those writes leave owed, so no ask block the close
// withdrew reads open again once the rebuild commits. That is also why the close's states go into
// the seed rather than through a live write after it (WriteIssueAskStates): such a write's fork is
// refused while the rebuild holds the room, and would read the old history through the pool that
// owns loads.
func (s *Service) RebuildDocument(ctx context.Context, artifactID string, markdown *string, actor model.Actor) (RebuildReport, VersionResult, error) {
	tx, joined := txFromContext(ctx)
	if !joined {
		return RebuildReport{}, VersionResult{}, errUnjoined
	}
	if _, loaded := s.rebuilding.LoadOrStore(artifactID, struct{}{}); loaded {
		return RebuildReport{}, VersionResult{}, fmt.Errorf("%w: document %s is already being rebuilt", ErrDocumentLive, artifactID)
	}
	ledgerFrom(ctx).holdRebuild(artifactID)
	// A room still loading is not resident yet: ygo reports it absent until its load hook has
	// returned and it is published (Server.GetDoc), so the check below does not see it, and it
	// reads the history this transaction is about to replace. Each load whose hook runs from here
	// on is refused there: by the hold while it stands, and once the commit lifts it, by the mark
	// the hold leaves on the load under way as it goes (Ledger.endRebuilds, markRoomLoadRebuilt).
	// Without the mark a load that read before the commit and reached its hook after it published
	// a room of the replaced history, with no list of the clients this rebuild retires, which a
	// browser joining it took and handed back to the rebuilt room on its reconnect.
	//
	// That leaves a load whose hook had passed the hold's check before the hold was taken. Unseen
	// by the check below, it published a room of the replaced history while this transaction ran,
	// and that room served every browser that opened the document until the catch-up closed it
	// after the commit: the person who asked for this rebuild, opening it again, was served the
	// replaced history, what they typed there was refused at the store, which no longer held the
	// items it was typed against, and their reconnect was refused as a holder of that history, so
	// the page dropped what they had typed and told them someone else had rebuilt the document. So
	// the rebuild waits out any load of the room first (awaitLoaded): a room that load published is
	// resident when the check below looks, and is judged and closed as any resident room is - what
	// was typed in it flushed into the history this rebuild reads - or the rebuild refused for its
	// caller to retry, as for any resident room a caller is still inside. Every load that can still
	// publish is one this wait sees: the hold, taken above, is checked by every hook that runs
	// after it. A load that starts once the rebuild commits reads the rebuilt head: versions only
	// increase, so its room holds the rebuilt update and none it would mistake for it. A room stays
	// resident for roomIdleTimeout after its last editor leaves, and ygo's idle sweep that then
	// evicts it runs every 30 seconds, so the refusal names two minutes.
	//
	// The other envoy-dispatch processes' rooms are asked the same through a holds query below: a
	// member that answers it holds a room of the document refuses the rebuild as this server's
	// room does. A member that does not answer within the query's wait, or a room that loads
	// there after it answered, is covered by room sync instead. RebuildTx replaces the whole
	// history with one update at head+1 and announces it, so an instance over there finds its
	// stored history replaced (historyReplaced): its catch-up closes and reloads the room
	// (roomCatchUps.closeReplaced), and until then its settlement and its browser's append are
	// refused (ErrRoomBehindHead) and lead to the same close, and the reload reads the rebuilt
	// document.
	// A room this server holds that stands near a cap decides the rebuild: the history the rebuild
	// reads once the room is closed is judged again only where no room decided (rebuildSource). A
	// decode of that history can read otherwise - it merges the runs of typing the room holds a
	// struct a keystroke, and the room's tracked state runs at or above its encode between
	// re-bases - so a room judged near was once closed and then answered DOCUMENT_LOADS, which
	// offered the rebuild and refused it on the same click.
	if err := s.srv.awaitLoaded(ctx, artifactID); err != nil {
		return RebuildReport{}, VersionResult{}, fmt.Errorf("%w: wait for document %s's room to load before its rebuild: %v", ErrServiceUnavailable, artifactID, err)
	}
	var decided string
	if live := s.srv.GetDoc(artifactID); live != nil {
		// The check decodes the room's whole state, so it waits for its grant of the copy-heap
		// bound within this request (nearACap): a wait this request ends leaves the rebuild to
		// the caller's retry, as a close the server could not make does below.
		why, near, err := nearACap(ctx, live, s.copyOfRoom(live), int64(s.roomStateBytes(live)))
		if err != nil {
			return RebuildReport{}, VersionResult{}, fmt.Errorf("%w: weigh document %s before its rebuild: %v", ErrServiceUnavailable, artifactID, err)
		}
		if !near {
			return RebuildReport{}, VersionResult{}, liveRebuildRefusal(ctx, tx, artifactID, roomOpenHere, askAfterTwoMinutes)
		}
		decided = why
		// The loads its browsers make as they reconnect are refused until this transaction
		// ends (refuseIfRebuilding), so the room stays closed while the history is replaced. A
		// Server.Apply already inside its callback on the room is the one thing that close gives
		// way to, since its commit would reach the store beside the history this rebuild
		// replaces (evictBeforeRebuild); the rebuild is refused, and the caller retries.
		closed, err := s.evictBeforeRebuild(artifactID)
		if err != nil {
			return RebuildReport{}, VersionResult{}, fmt.Errorf("%w: close document %s before its rebuild: %v", ErrServiceUnavailable, artifactID, err)
		}
		if !closed {
			return RebuildReport{}, VersionResult{}, fmt.Errorf("%w: document %s is being written in this server; retry", ErrServiceUnavailable, artifactID)
		}
	}
	answers, _, err := s.roomQueries.ask(ctx, tx, artifactID, roomQueryHolds)
	if err != nil {
		return RebuildReport{}, VersionResult{}, fmt.Errorf("%w: ask the other processes whether they hold the document: %v", ErrServiceUnavailable, err)
	}
	for _, answer := range answers {
		if answer.holds {
			return RebuildReport{}, VersionResult{}, liveRebuildRefusal(ctx, tx, artifactID, roomOpenElsewhere, askAfterTwoMinutes)
		}
	}
	history, loaded, why, retired, err := s.rebuildSource(ctx, artifactID, decided)
	if errors.Is(err, ErrDocumentLoads) {
		latest, err := savedVersionNumber(ctx, tx, artifactID)
		if err != nil {
			return RebuildReport{}, VersionResult{}, err
		}
		return RebuildReport{}, VersionResult{}, fmt.Errorf("%w (document %s, latest saved version %d)", ErrDocumentLoads, artifactID, latest)
	}
	if err != nil {
		return RebuildReport{}, VersionResult{}, err
	}
	// The owner's lock before the room lock the write below takes, as every writer that takes both
	// does (lockDocumentRoom). Held, it also keeps the open asks the replacement must keep, and
	// the approval request the version moves, from changing under this transaction.
	owner, open, err := lockArtifactOwner(ctx, tx, artifactID)
	if err != nil {
		return RebuildReport{}, VersionResult{}, err
	}
	latest, err := latestVersion(ctx, tx, artifactID)
	if err != nil {
		return RebuildReport{}, VersionResult{}, err
	}
	if markdown != nil && !open {
		return RebuildReport{}, VersionResult{}, ErrIssueClosed
	}
	// What the rebuild keeps beside the text (rebuild_keeps.go): every path keeps the document's
	// block ids, its open anchors and its margin records as far as what it is built from names
	// them. A history that decodes holds all three, whether a room loads it or it parks past a
	// room's cap (parkedPastRoomCaps): its own tree is the document, with every edit
	// settlement has not yet written into a version, its anchor marks and its typed blocks' state,
	// and its marks map holds the margin; supplied markdown over it is taken as an upload over it
	// is, its unchanged blocks keeping their ids and its anchors marked again (keptReplacement). A
	// tree outside the schema has no text to carry anything from, and the markdown repairs it, as
	// an upload repairs one; a tree holding an element this build does not know is refused, with
	// or without markdown, as every read and write of the document refuses it, since a rebuild
	// from here would drop that element. A history that does not decode holds nothing that
	// reads, so the text it is rebuilt from takes back what the rows record of it
	// (restoredFromRows).
	//
	// stored is the tree the document already holds where the rebuild is built from it - the
	// history's own tree, or the latest version's as the version holds it - which the seed puts
	// back rather than creates, so an element this build reads and may not create yet is held
	// already, not created (rebuiltSeed, pmdoc.UpdateHolding). Supplied markdown over a history
	// that loads is taken as an upload over it is, and an upload holds what the document holds;
	// supplied markdown with no tree to take it over creates every element it holds, so stored is
	// nil for it.
	var tree, stored *pmdoc.Node
	var margin map[string]any
	if history != nil {
		margin = marginRecords(history)
		current, treeErr := treeOf(history)
		switch {
		case errors.Is(treeErr, ErrDocNewerContent):
			err = treeErr
		case markdown == nil:
			tree, err = current, treeErr
			stored = current
		case treeErr != nil:
			tree, err = parseInput(*markdown)
		default:
			tree, err = s.keptReplacement(ctx, tx, artifactID, current, *markdown)
			stored = current
		}
	} else {
		if markdown != nil {
			tree, err = parseInput(*markdown)
		} else {
			// The version's markdown is the server's own, not the caller's, so an element this
			// build does not know in it is newer content, as every read of the document answers
			// it, not invalid markdown.
			parsed, parseErr := pmdoc.ParseRendering(latest.markdown)
			if newer := documentSchemaError(parseErr); errors.Is(newer, ErrDocNewerContent) {
				return RebuildReport{}, VersionResult{}, newer
			}
			// uploadedInput copies parsed, so the rows' marks and the close's states written into
			// tree below leave stored as the version holds it.
			tree, err = uploadedInput(parsed, parseErr)
			stored = parsed
		}
		if err == nil {
			tree, margin, err = s.restoredFromRows(ctx, tx, artifactID, tree)
		}
	}
	history = nil
	if err != nil {
		return RebuildReport{}, VersionResult{}, err
	}
	// A caller's markdown is markdown written in whole, so an ask written with its options and no
	// question is refused (validateWrittenAskBlocks), as the markdown of such an ask was before
	// both parsers read the empty paragraph the schema fills in ahead of its options. The latest
	// version's own markdown is held to the tree rule instead (validateStoredAskBlocks): a
	// person's edit can empty an ask's question in the live document, which this package keeps
	// and repairs through that question (refuseEmptyQuestionEdit), and a document the product
	// stores must be one a rebuild can restore - refusing it here would leave it with no way
	// back. Both doors hold every ask to the wording bound it shares.
	content := validateWrittenAskBlocks
	if markdown == nil {
		content = validateStoredAskBlocks
	}
	if err := content(tree); err != nil {
		return RebuildReport{}, VersionResult{}, &ErrInvalidAskBlock{Reason: err}
	}
	if err := s.refuseDroppedAskBlocks(ctx, artifactID, tree); err != nil {
		return RebuildReport{}, VersionResult{}, &ErrInvalidAskBlock{Reason: err}
	}
	// A closed issue's document shows its asks as the close left them, and the tree this rebuild
	// was built from - the history's own, or the latest version's - need not: no settlement
	// versions a document whose history does not load, the history a rebuild builds from the
	// latest version, so that version can predate the close, and the close's live writes to the
	// document either never reached it, its history not loading or standing at a cap then, or are
	// in the history this replaces. So the close's final sweep is written into the tree here
	// (closedIssueAskState): every resolved ask's block and copy, and every answered ask's copy,
	// from the rows as they stand under the issue's owner lock this transaction holds, and every
	// readable ask block with no ask row marked resolved. The ask rows are taken before RebuildTx
	// takes the document's advisory lock, the order the close's own write takes them in
	// (WriteIssueAskStates). Once this transaction commits, the rebuilt document reads as the
	// issue's other documents do.
	var closedStates *issueAskStateWrites
	if !open {
		closedStates, err = readIssueAskStateWrites(ctx, tx, artifactID, owner, tree, closedIssueAskState, true)
		if err != nil {
			return RebuildReport{}, VersionResult{}, err
		}
		if !closedStates.apply(tree) {
			closedStates = nil
		}
	}
	canonical, offsets, err := renderTreeWithOffsets(tree)
	if err != nil {
		return RebuildReport{}, VersionResult{}, err
	}
	if markdown != nil {
		if err := weighRendering("", canonical); err != nil {
			return RebuildReport{}, VersionResult{}, err
		}
		// Supplied markdown is a caller's write, so a block pointer or a check block it introduces
		// over the latest version is checked. That version is read back as the tree before the
		// write; one that does not read back holds no pointer and no check block.
		var prior *pmdoc.Node
		if latestTree, err := pmdoc.ParseRendering(latest.markdown); err == nil {
			prior = latestTree
		}
		if err := refuseBlockPointers(ctx, tx, artifactID, tree, prior, offsets); err != nil {
			return RebuildReport{}, VersionResult{}, err
		}
		if err := refuseCICheckBlocks(ctx, tx, artifactID, tree, prior); err != nil {
			return RebuildReport{}, VersionResult{}, err
		}
	}
	seed, err := rebuiltSeed(tree, stored, retired, margin)
	if err != nil {
		return RebuildReport{}, VersionResult{}, fmt.Errorf("seed rebuilt document tree: %w", err)
	}

	// RebuildTx replaces the history only if its head is still the one loaded above, which is the
	// history validated as one that cannot load: a second rebuild of the document on another
	// process that loaded the same history waits on the owner lock above until this one commits,
	// then finds the head moved and is refused.
	report, err := s.persistence.RebuildTx(ctx, tx, artifactID, loaded, seed)
	if err != nil {
		return RebuildReport{}, VersionResult{}, err
	}
	report.SourceVersion = latest.Number
	report.ValidationError = why
	if closedStates != nil {
		// markdown is nil here: a closed issue refuses supplied markdown above. The seed differs
		// from the tree it was built from by the close's states, as each of the issue's documents
		// the close wrote differs from its own, so it owes the settlement those writes leave owed
		// (updateOwedStatement). That settlement versions the document whether or not the issue
		// is closed. While the issue is closed it keeps each block marked resolved here with no
		// ask row resolved and opens no ask for it (reconcileAskBlocks), and a settlement after
		// the reopen indexes it as a new open ask. RebuildTx holds the document's advisory lock,
		// which the mark is ordered by. The writes credit no one, as the close's do (the zero
		// owedUpdates): no pending author is added, and the latest edit source the row names is
		// kept.
		owed, err := updateOwedStatement(artifactID, owedUpdates{})
		if err != nil {
			return RebuildReport{}, VersionResult{}, err
		}
		if err := owed.exec(ctx, tx); err != nil {
			return RebuildReport{}, VersionResult{}, err
		}
		logUnindexedAskBlocks(*owner.IssueKey, closedStates.marked)
	}
	if markdown == nil || canonical == latest.markdown {
		return report, VersionResult{Version: latest.Version}, nil
	}
	// RebuildTx holds the document's advisory lock until this transaction ends, so the pending
	// authors read here are the ones the version's commit deletes.
	capture, err := s.capturePendingAuthors(ctx, tx, artifactID, nil, &actor, everyCredit)
	if err != nil {
		return RebuildReport{}, VersionResult{}, err
	}
	written, err := s.writeVersionTx(ctx, tx, artifactID, renderedTree{tree: tree, markdown: canonical, offsets: offsets}, actor, &versionWrite{
		authors: actorSlice(capture.authors),
		capture: &capture,
	})
	if err != nil {
		return RebuildReport{}, VersionResult{}, err
	}
	// A rebuild replaces the document's content with markdown its own actor wrote, so that actor
	// made the document's latest change, whatever edit the broken history still had pending. It is
	// recorded as the latest edit source for the versions that follow, and names this version's
	// edit notice (recordEditNotice), which goes when that actor is a person.
	if err := recordLatestEditSource(ctx, tx, artifactID, &actor); err != nil {
		return RebuildReport{}, VersionResult{}, err
	}
	if err := s.recordEditNotice(ctx, tx, artifactID, written.version.Number, actorSlice(capture.authors), &actor); err != nil {
		return RebuildReport{}, VersionResult{}, err
	}
	report.SourceVersion = written.version.Number
	return report, VersionResult{Version: written.version, Wrote: true, Changes: written.changes}, nil
}

// rebuildSource is what a rebuild of artifactID starts from: the stored history decoded (nil for
// one that does not decode), the version that history stood at, which RebuildTx replaces only
// while the head is still it, why it must be rebuilt - how near a cap it stands (nearACap), the
// decode's refusal, or the room's cap the history parks past (parkedPastRoomCaps) - and the
// clients it retires. A history whose runs do not apply in order has no bytes to decode, and
// carries its version and clients itself (unappliedHistory). decided is why the room this server
// held stood near a cap, which RebuildDocument judged and closed, or "" where no room decided: a
// history that loads is judged here only then, and one that stands near no cap is
// ErrDocumentLoads, which RebuildDocument answers with the latest saved version named.
//
// A history that parks past a room's cap is one no room loads and every read answers as one that
// cannot load, so it is rebuilt, with that cap and what the history parks as its reason. It
// decodes under the service's own cap all the same, and is the source, as a history near a cap
// is: the rebuild keeps its text, unsettled edits included, and its block ids, anchors and margin
// records, and the fresh seed built from its tree holds nothing it parked. Before it was judged
// against the room's caps, this read took such a history as one that loads, and the rebuild was
// refused ErrDocumentLoads while its room failed every load (dispatch://INFRA-179, round 10).
//
// The read's fold, its decode (loadDecodedHistory) and the check's re-scan of it run under one
// slot of the pod's load concurrency, whose wait ends with ctx, held here rather than by the read,
// since the check runs inside it, and given back once the history is the rebuild's.
func (s *Service) rebuildSource(ctx context.Context, artifactID, decided string) (*crdt.Doc, persistence.Version, string, map[crdt.ClientID]uint64, error) {
	ctx, _, done := withLoadSlot(ctx)
	defer done()
	read, err := loadDecodedHistory(ctx, s.persistence, artifactID, s.maxPendingItems, nil)
	var unapplied *unappliedHistory
	if errors.As(err, &unapplied) {
		return nil, unapplied.version, unapplied.Error(), unapplied.clients, nil
	}
	if err != nil {
		return nil, 0, "", nil, fmt.Errorf("%w: load document before rebuild: %v", ErrServiceUnavailable, err)
	}
	if read.refused != nil {
		return nil, read.loaded.Version, read.refused.Error(), undecodableHistoryClients(read.loaded.Update), nil
	}
	// An empty history decodes into no document at all (decodeStoredCopy); what a rebuild of it
	// starts from is the empty document a room's load of it would hold.
	history := read.decoded.doc
	if history == nil {
		history = newCappedDocumentCopy(s.maxPendingItems)
	}
	if decided != "" {
		return history, read.loaded.Version, decided, historyClients(history), nil
	}
	if parked := s.parkedPastRoomCaps(history); parked != nil {
		return history, read.loaded.Version, parked.Error(), historyClients(history), nil
	}
	// The stored history has not been decoded here, so no struct count can be read of it and
	// the state term alone weighs the grant nearACap takes (copyWeight).
	why, near, err := nearACap(ctx, history, copyWeight(int64(len(read.loaded.Update)), 0), int64(len(read.loaded.Update)))
	if err != nil {
		return nil, 0, "", nil, fmt.Errorf("%w: weigh document %s before its rebuild: %v", ErrServiceUnavailable, artifactID, err)
	}
	if !near {
		return nil, 0, "", nil, ErrDocumentLoads
	}
	return history, read.loaded.Version, why, historyClients(history), nil
}

// rebuiltSeed writes tree into a fresh document, as encodeDocumentTreeHolding does with held, the
// tree the document already holds that the rebuild is built from (nil for none): an element this
// build reads and may not create yet is put back where held holds it, not created
// (pmdoc.UpdateHolding). Beside it go the retired clients, listed under retiredMapName, each its
// client id in decimal against the clock it had reached, and margin, the margin records the
// rebuild keeps, in the marks map under their keys. It returns the document's first durable
// update.
func rebuiltSeed(tree, held *pmdoc.Node, retired map[crdt.ClientID]uint64, margin map[string]any) ([]byte, error) {
	doc := crdt.New()
	fragment := doc.GetXmlFragment(fragmentName)
	marks := doc.GetMap(marksMapName)
	list := doc.GetMap(retiredMapName)
	if err := doc.TransactE(func(transaction *crdt.Transaction) error {
		for client, clock := range retired {
			list.Set(transaction, strconv.FormatUint(uint64(client), 10), int64(clock))
		}
		for _, key := range sortedKeys(margin) {
			marks.Set(transaction, key, margin[key])
		}
		return pmdoc.UpdateHolding(transaction, fragment, tree, held)
	}); err != nil {
		return nil, err
	}
	return crdt.EncodeStateAsUpdateV1(doc, nil), nil
}

// historyClients is what a rebuild of history, a history that decodes, retires: every client its
// state vector names, at the clock it reached, and every client the history itself listed as
// retired by an earlier rebuild, so a browser holding any history the document has had is refused.
func historyClients(history *crdt.Doc) map[crdt.ClientID]uint64 {
	retired := retiredClientsOf(history)
	if retired == nil {
		retired = make(map[crdt.ClientID]uint64)
	}
	for client, clock := range history.StateVector() {
		retired[client] = max(retired[client], clock)
	}
	return retired
}

// undecodableHistoryClients is what a rebuild of a history ygo cannot load retires: the clients
// the header of its update names, each at the clock past its last struct, read without decoding
// any content (crdt.ParseUpdateMeta). A browser holding such a history holds those clients, so
// one retired client is enough to refuse it. A header that does not read at all retires none:
// no clock of it can be trusted, and the rebuild replaces the history either way.
//
// Where the walk this replaced left a client's withheld clocks out, the header counts them: a
// Skip is read as the client's own structs, so a client is retired at the end of its last
// struct whether or not the history held every clock before it. That retires a browser at a
// clock it may not have reached, which refuses it exactly as a clock it did reach would - the
// rebuild replaced its history either way.
func undecodableHistoryClients(update []byte) map[crdt.ClientID]uint64 {
	_, to, err := crdt.ParseUpdateMeta(update)
	if err != nil {
		return nil
	}
	return to
}

// The causes and next steps of the rebuild refusals liveRebuildRefusal builds.
const (
	roomOpenHere       = "is open in this Dispatch process, or its last editor left it less than two minutes ago"
	roomOpenElsewhere  = "is open in another Dispatch process, or its last editor left it less than two minutes ago"
	askAfterTwoMinutes = "Stop. Ask a person whether to rebuild the document after two minutes"
	readAgainFirst     = "Another rebuild may already have repaired the document: read it again, and ask a person whether to rebuild it only if it still does not load"
)

// liveRebuildRefusal is every ErrDocumentLive refusal of a rebuild that names what a rebuild would
// discard: why Dispatch refused (cause), the latest saved version a rebuild from it would go back
// to, read on tx, and what the caller does next (next). One builder, so no refusal keeps advice
// another has dropped. The one ErrDocumentLive refusal it does not build is "already being
// rebuilt" (RebuildDocument), which says only that a rebuild is running and offers no repair.
func liveRebuildRefusal(ctx context.Context, tx pgx.Tx, artifactID, cause, next string) error {
	latest, err := savedVersionNumber(ctx, tx, artifactID)
	if err != nil {
		return err
	}
	return fmt.Errorf("%w: document %s %s, so Dispatch refused the rebuild. A rebuild from the latest saved version, %d, discards every edit made after it. %s",
		ErrDocumentLive, artifactID, cause, latest, next)
}

// savedVersionNumber is the number of artifactID's latest saved version, which a refused rebuild
// names: a rebuild discards every edit made after it.
func savedVersionNumber(ctx context.Context, tx pgx.Tx, artifactID string) (int, error) {
	var number int
	if err := tx.QueryRow(ctx, `
		select coalesce(max(number), 0) from artifact_versions where artifact_id = $1
	`, artifactID).Scan(&number); err != nil {
		return 0, fmt.Errorf("read latest document version number: %w", err)
	}
	return number, nil
}

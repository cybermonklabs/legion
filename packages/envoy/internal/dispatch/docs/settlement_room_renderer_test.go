package docs

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A renderer change leaves a document of an issue room as it leaves a document that is its own
// room (TestSettlementWritesNoVersionForAnotherRendererVersionsSpellingOfTheSameTree): the spec's
// fragment holds a loose list, and the spec's latest version holds the same tree in another
// renderer version's spelling. An edit typed into the spec's fragment and undone changes the
// room's content and changes it back, and the room's settlement renders the fragment in this
// build's spelling, which that version holds (roomVersion.holds): it writes no version of the
// spec, so an approval of that version still reads approved. An edit that stays is still
// versioned.
func TestAnIssueRoomsSettlementWritesNoVersionForAnotherRendererVersionsSpellingOfAFragment(t *testing.T) {
	database := storetest.Open(t)
	room := createIssueRoom(t, database, 1)
	service := newTestServiceOn(t, database)
	service.settle = time.Hour
	written, err := pmdoc.Parse(looseList)
	if err != nil {
		t.Fatalf("parse the loose list: %v", err)
	}
	editRoomFragment(t, service, room, room.spec, func(*pmdoc.Node) *pmdoc.Node { return written })
	settleCurrentGeneration(t, service, room.tree)
	if stored := latestVersionMarkdown(t, service, room.spec); stored != looseList {
		t.Fatalf("the spec's latest version holds %q, want %q, its fragment as this build renders it", stored, looseList)
	}
	approved := respellLatestVersion(t, database, room.spec, respelledLooseList)
	approveStoredVersion(t, database, room.spec, approved, "alice")

	editRoomFragment(t, service, room, room.spec, replaceRun("first", "first typed"))
	editRoomFragment(t, service, room, room.spec, replaceRun("first typed", "first"))
	awaitRoomAppends(t, service, room.tree, "reach persistence", service.waitForPendingUpdates)
	awaitRoomAppends(t, service, room.tree, "become durable", service.waitForDurableAppends)
	if rows := contentRowsPastFragmentVersion(t, database, room, room.spec); rows == 0 {
		t.Fatal("the typed and undone edit left no content-class row of the room past the cursor of the spec's latest version")
	}

	settleCurrentGeneration(t, service, room.tree)

	if latest := latestVersionNumber(t, service, room.spec); latest != approved {
		t.Fatalf("settling an edit of the spec's fragment typed and undone wrote version %d of the spec, want none past version %d, which holds the same tree in another renderer version's spelling", latest, approved)
	}
	if stored := latestVersionMarkdown(t, service, room.spec); stored != respelledLooseList {
		t.Fatalf("the spec's latest version holds %q, want %q as it was stored", stored, respelledLooseList)
	}
	requireApprovedAt(t, service, room.spec, approved)

	editRoomFragment(t, service, room, room.spec, replaceRun("third", "third, edited"))
	settleCurrentGeneration(t, service, room.tree)
	if latest := latestVersionNumber(t, service, room.spec); latest != approved+1 {
		t.Fatalf("settling an edit of the spec's fragment that stays left the spec at version %d, want %d", latest, approved+1)
	}
	if stored := latestVersionMarkdown(t, service, room.spec); !strings.Contains(stored, "third, edited") {
		t.Fatalf("the spec's version %d holds %q, want the edit that stays", approved+1, stored)
	}
}

// editRoomFragment writes a browser-style tree change of the fragment named fragment of room's
// issue room through the live Yjs room, as editLiveTree writes one of a document that is its own
// room. A fragment that holds no block yet - a document of the room nobody has written into, which
// reads as no document - is handed to edit as nil, as a first write into it is written without a
// read (TestARoomSettlementNamesABlockEventWithItsOwnDocumentsVersion).
func editRoomFragment(t *testing.T, service *Service, room issueRoom, fragment string, edit func(*pmdoc.Node) *pmdoc.Node) {
	t.Helper()
	err := service.srv.Apply(context.Background(), room.tree, func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
		held := doc.GetXmlFragment(fragment)
		var tree *pmdoc.Node
		if held.Len() > 0 {
			var err error
			if tree, err = treeOfFragment(doc, fragment); err != nil {
				t.Errorf("read the fragment %s of room %s: %v", fragment, room.tree, err)
				return
			}
		}
		want := edit(tree)
		transact(func(txn *crdt.Transaction) {
			if err := pmdoc.Update(txn, held, want); err != nil {
				t.Errorf("update the fragment %s of room %s: %v", fragment, room.tree, err)
			}
		})
	})
	if err != nil {
		t.Fatalf("apply the edit of the fragment %s of room %s: %v", fragment, room.tree, err)
	}
	if t.Failed() {
		t.FailNow()
	}
}

// contentRowsPastFragmentVersion is how many content-class updates of room's issue room lie past
// the cursor of document's latest version, as contentRowsPastLatestVersion counts them for a
// document that is its own room: the room's updates are stored under its tree artifact, and each
// document's version records the room's cursor it covers.
func contentRowsPastFragmentVersion(t *testing.T, database *store.Store, room issueRoom, document string) int {
	t.Helper()
	var rows int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from doc_updates
		where artifact_id = $1 and content_changed and version > (
			select doc_update_version from artifact_versions
			where artifact_id = $2 order by number desc limit 1
		)
	`, room.tree, document).Scan(&rows); err != nil {
		t.Fatalf("count the content updates of room %s past the latest version of %s: %v", room.tree, document, err)
	}
	return rows
}

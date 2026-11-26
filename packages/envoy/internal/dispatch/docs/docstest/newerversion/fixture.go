package newerversion

import (
	"testing"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// StoreNewerVersion stages a later build's version and live tree without using this build's
// document write routes, and answers the version's number. The tree is markdown as that build
// renders it, written into the document's own fragment of the room it is edited in, which is its
// issue's room once the issue has converted (docs.AppendStoredChangeForTest): the stored history
// every room of the document loads, and whose append announces the new head to a room this
// process holds open, as another process's write does. The version is the document's next one,
// standing at that room's head.
func StoreNewerVersion(t *testing.T, database *store.Store, id, markdown string) int {
	t.Helper()
	ctx := t.Context()
	tree, err := pmdoc.ParseRendering(markdown)
	if err != nil {
		t.Fatalf("parse the later build's rendering: %v", err)
	}
	markdown, err = pmdoc.Render(tree)
	if err != nil {
		t.Fatalf("render the later build's version: %v", err)
	}
	if err := docs.AppendStoredChangeForTest(ctx, database, id, func(txn *crdt.Transaction, fragment *crdt.YXmlFragment) error {
		return pmdoc.UpdateHolding(txn, fragment, tree, tree)
	}); err != nil {
		t.Fatalf("store the later build's live tree: %v", err)
	}
	var number int
	if err := database.Pool.QueryRow(ctx, `
		with held as (`+docs.DocumentRoomOfStatement+`)
		insert into artifact_versions (artifact_id, number, markdown, authors, doc_update_version)
		select $2::uuid, (select coalesce(max(number), 0) + 1 from artifact_versions where artifact_id = $2::uuid), $3, '[]',
			(select coalesce(max(version), 0) from doc_updates where artifact_id = held.room::uuid)
		from held
		returning number
	`, id, id, markdown).Scan(&number); err != nil {
		t.Fatalf("store the later build's version: %v", err)
	}
	return number
}

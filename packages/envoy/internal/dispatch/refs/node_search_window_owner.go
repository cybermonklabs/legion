package refs

import (
	"context"
	"fmt"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
)

// The owner key of a document's embedding windows (R17 of the issue-tree spec). A
// window's id is `<owner key>#<first block>`, and each writer of an owner's windows deletes the
// owner's windows its new set does not name by split_part(id, '#', 1), which
// embeddings_window_owner serves (writeWindows). A document's owner key is the room it is edited
// in (DocumentRoomSQL): the issue's tree artifact for a document converted into its issue's room,
// whose blocks the room keys under the tree, and the document itself for one that is its own room.
// So the windows of a block that moves out of a document into its issue's room are written under
// the tree, and the document's own windows from before the move stay where they are: a rollback to
// the release before searches by meaning over the windows keyed by the document's artifact id, and
// the root's own pull request deletes them. This build's search reads only the windows whose owner
// is current (WindowDocumentSQL), and the store census counts the others once
// (graphcensus' movedOwnerWindows).

// DocumentWindowOwner is the owner key the windows of document artifactID are written and read
// under, read through q: the room it is edited in, its issue's tree artifact once it is converted
// into its issue's room, and itself where it is its own room (DocumentRoomSQL).
func DocumentWindowOwner(ctx context.Context, q Queryer, artifactID string) (string, error) {
	var owner string
	if err := q.QueryRow(ctx, `select (`+DocumentRoomSQL("$1::uuid")+`)::text`, artifactID).Scan(&owner); err != nil {
		return "", fmt.Errorf("read the owner of the windows of %s: %w", artifactID, err)
	}
	return owner, nil
}

// documentWindowsDropped is the condition, over the embeddings row the write of a document's
// windows deletes, that a window of the owner its new set does not name meets to be deleted with
// that write (blockState.dropped): its first block is the document's own, or is in no fragment of
// the room at all. An issue room keys the windows of all its documents under its tree artifact, so
// the write of one document's windows leaves every other document's standing, and a window whose
// first block the room no longer holds is no document's and goes with the next write of any. Over
// a document that is its own room no issue_room_blocks row is keyed by it, so every window of the
// owner is the document's, as it was before the room. The statement binds the owner key as $2
// (windowsStatement) and the document as $5, the first argument of the document's state
// (WriteDocumentWindows).
var documentWindowsDropped = `
			  and coalesce(` + graphstore.RoomBlockRowSQL("$2::uuid", "covers[1]") + `, $5::uuid) = $5::uuid`

// WindowDocumentSQL is the document whose blocks a document window covers, as SQL text answering
// a uuid, and null for a window this build's search reads none of: owner is the window's owner key
// as a uuid SQL expression and block the first block the window covers. A window under an issue's
// tree artifact is of the document whose fragment of the room holds that block
// (graphstore.RoomBlockRowSQL), the tree artifact itself for a block of the room's own fragment; a
// window under a document that is its own room is that document's; and a window under a document
// converted into its issue's room (document_rooms) is one its old owner key still holds, which a
// rollback's search by meaning reads and this build's does not.
func WindowDocumentSQL(owner, block string) string {
	return `(select coalesce(` + graphstore.RoomBlockRowSQL("window_owner.id", block) + `, window_owner.id)
		from artifacts window_owner
		where window_owner.id = ` + owner + `
		  and not exists (select 1 from document_rooms moved where moved.artifact_id = window_owner.id))`
}

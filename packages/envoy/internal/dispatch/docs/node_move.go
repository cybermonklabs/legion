package docs

import (
	"context"
	"errors"
	"fmt"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// MoveBlockText writes the text of a move of the node route (R7 of dispatch://INFRA-1253: api's
// PATCH /api/v1/issues/{key}/nodes/{id}) into the tree of artifactID, the document that holds the
// moved block, in the transaction ctx joined, which holds the owner locks of the move: the
// document's own room or its issue's room is joined for the write (joinLiveWrite, whose owner lock
// the transaction holds already), and the room's lock is taken (lockDocumentRoom), after every
// owner lock, as an edit with a precondition takes it. Under them the document's tree is read
// (applyLive), move is written into it whole (pmdoc.ValidDocument.MoveNodeText), the block ids a
// new list lacks are minted (pmdoc.EnsureBlockIDs), and the tree is validated once and written as
// the fragment's new state (pmdoc.UpdateDocument). The edit route's operations are no part of it:
// pmdoc.MoveBlock nests no list item and validates the document after each move.
//
// It answers whether the write changed the document, which a version the caller writes then
// records (SnapshotVersion). A move that would leave a callout or a list item with no block of its
// own is pmdoc.ContainerEmptiedError, one whose text the schema cannot hold where the move puts it
// pmdoc.TextPlaceError, and nothing is written for either.
func (s *Service) MoveBlockText(ctx context.Context, artifactID string, move pmdoc.NodeTextMove, actor model.Actor) (bool, error) {
	tx, joined := txFromContext(ctx)
	if !joined {
		return false, errUnjoined
	}
	target, err := s.targetOf(ctx, artifactID)
	if err != nil {
		return false, err
	}
	if _, err := s.joinLiveWrite(ctx, ledgerFrom(ctx), target); err != nil {
		return false, err
	}
	if err := lockDocumentRoom(ctx, tx, target.room); err != nil {
		return false, err
	}
	changed := false
	err = s.applyLive(ctx, artifactID, actor, func(doc liveDoc, current pmdoc.ValidDocument, transact func(func(*crdt.Transaction))) error {
		start, err := startTree(current, doc)
		if err != nil {
			return err
		}
		next, err := start.MoveNodeText(move)
		if err != nil || next == nil {
			return err
		}
		pmdoc.EnsureBlockIDs(next)
		written, err := pmdoc.ValidateDocument(next, "MoveBlockText")
		if err != nil {
			return err
		}
		// The fragment is taken before the transaction: Transact holds the document's lock, which
		// GetXmlFragment takes too.
		fragment := doc.documentFragment()
		var updateErr error
		transact(func(transaction *crdt.Transaction) {
			updateErr = pmdoc.UpdateDocument(transaction, fragment, written)
		})
		changed = updateErr == nil
		return updateErr
	})
	if errors.Is(err, websocket.ErrNoChanges) {
		return false, nil
	}
	if err != nil {
		var emptied *pmdoc.ContainerEmptiedError
		var placed *pmdoc.TextPlaceError
		if errors.As(err, &emptied) || errors.As(err, &placed) {
			return false, err
		}
		return false, fmt.Errorf("write the move of block %s into %s: %w", move.Block, artifactID, err)
	}
	return changed, nil
}

// HoldStoredBlock records, in the ledger of the transaction ctx joined, that a move of the node
// route places the block whose node address is address as block says (refs.StoredBlock): every
// block index this transaction plans after it reads the block as placed there, at the placement
// refs.PlacementStored (heldBlockIndex), since the move's own part_of, rank and placement are
// queued in the write phase and no read of the transaction sees them. The caller records it before
// the version its move writes, whose index pass would otherwise place the block by its text.
func (s *Service) HoldStoredBlock(ctx context.Context, address string, block refs.StoredBlock) error {
	ledger := ledgerFrom(ctx)
	if ledger == nil || ledger.tx == nil {
		return errUnjoined
	}
	if ledger.storedBlocks == nil {
		ledger.storedBlocks = map[string]refs.StoredBlock{}
	}
	ledger.storedBlocks[address] = block
	return nil
}

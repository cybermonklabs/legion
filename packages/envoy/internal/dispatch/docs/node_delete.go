package docs

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// DeleteBlocksText writes the text of a delete of the node route (R7 of the issue-tree spec:
// api's DELETE /api/v1/issues/{key}/nodes/{id}) into the tree of each document of taken, keyed by
// the document's id, each holding the ids of the blocks of that document's text the delete takes,
// in the transaction ctx joined, which holds the owner locks of the delete: of the issue it deletes
// under and of every issue a node it takes stands under, a block of another issue's room among them.
//
// It takes the rooms in the order of their ids, after every owner lock, as the edit route takes
// one: for each room, its write is joined (joinLiveWrite, whose owner lock the transaction holds
// already) and its lock taken (lockDocumentRoom), once for a room several of the documents share.
// Under them each document's tree is read, its taken blocks' text is taken out of it with what each
// holds that the delete does not take lifted out to where that text stood
// (pmdoc.ValidDocument.DeleteNodeText, which validates the tree at the end), and the tree is
// validated again and written as the fragment's new state (writeNodeText), never through the edit
// route's ops: pmdoc.MoveBlock validates the document after each move, and the block+ content of a
// callout or a decision refuses a container a lift-out leaves empty for a moment, though the same
// write then takes it out.
//
// It answers the documents whose text the write changed, in the order it wrote them, each of which
// the caller versions (SnapshotVersion). A document the write would leave holding a block with no
// block of its own is pmdoc.ContainerEmptiedError, naming that block, and nothing is written.
func (s *Service) DeleteBlocksText(ctx context.Context, taken map[string][]string, actor model.Actor) ([]string, error) {
	tx, joined := txFromContext(ctx)
	if !joined {
		return nil, errUnjoined
	}
	targets := make([]liveTarget, 0, len(taken))
	for document := range taken {
		target, err := s.targetOf(ctx, document)
		if err != nil {
			return nil, err
		}
		targets = append(targets, target)
	}
	sortDocumentTargets(targets)
	ledger := ledgerFrom(ctx)
	for index, target := range targets {
		if _, err := s.joinLiveWrite(ctx, ledger, target); err != nil {
			return nil, err
		}
		if index > 0 && targets[index-1].room == target.room {
			continue
		}
		if err := lockDocumentRoom(ctx, tx, target.room); err != nil {
			return nil, err
		}
	}
	var changed []string
	for _, target := range targets {
		blocks := takenBlocks(taken[target.document])
		wrote, err := s.writeNodeText(ctx, target.document, actor, "DeleteBlocksText", func(start pmdoc.ValidDocument) (*pmdoc.Node, error) {
			return start.DeleteNodeText(blocks)
		})
		if err != nil {
			var emptied *pmdoc.ContainerEmptiedError
			if errors.As(err, &emptied) {
				emptied.Document = target.document
				return nil, err
			}
			return nil, fmt.Errorf("write the delete of %d blocks into %s: %w", len(blocks), target.document, err)
		}
		if wrote {
			changed = append(changed, target.document)
		}
	}
	return changed, nil
}

// sortDocumentTargets puts targets in the order a delete writes them: by room, then by document.
func sortDocumentTargets(targets []liveTarget) {
	slices.SortFunc(targets, func(left, right liveTarget) int {
		return cmp.Or(cmp.Compare(left.room, right.room), cmp.Compare(left.document, right.document))
	})
}

// takenBlocks is the set of ids, the blocks of one document a delete takes.
func takenBlocks(ids []string) map[string]bool {
	blocks := make(map[string]bool, len(ids))
	for _, id := range ids {
		blocks[id] = true
	}
	return blocks
}

// PlanDeleteBlocksText is the dry run of DeleteBlocksText over taken: it reads each document's tree
// as a read of its text reads it (documentTree), with no lock taken and nothing written, in the order
// the delete writes them, and takes the taken blocks out of a copy of it
// (pmdoc.ValidDocument.DeleteNodeText). It answers the pmdoc.ContainerEmptiedError the delete's
// write would answer at the first document it would leave holding a block with no block of its own,
// and nil where the write would leave none, so the dry run refuses what the delete refuses.
func (s *Service) PlanDeleteBlocksText(ctx context.Context, taken map[string][]string) error {
	targets := make([]liveTarget, 0, len(taken))
	for document := range taken {
		room, err := roomOf(ctx, s.queryFrom(ctx), document)
		if err != nil {
			return err
		}
		targets = append(targets, liveTarget{document: document, room: room})
	}
	sortDocumentTargets(targets)
	for _, target := range targets {
		tree, err := s.documentTree(ctx, target.document)
		if err != nil {
			return err
		}
		current, err := pmdoc.ValidateDocument(tree, "PlanDeleteBlocksText")
		if err != nil {
			return fmt.Errorf("read the tree of %s for the delete's dry run: %w", target.document, err)
		}
		blocks := takenBlocks(taken[target.document])
		if _, err := current.DeleteNodeText(blocks); err != nil {
			var emptied *pmdoc.ContainerEmptiedError
			if errors.As(err, &emptied) {
				emptied.Document = target.document
				return err
			}
			return fmt.Errorf("take %d blocks out of a copy of %s: %w", len(blocks), target.document, err)
		}
	}
	return nil
}

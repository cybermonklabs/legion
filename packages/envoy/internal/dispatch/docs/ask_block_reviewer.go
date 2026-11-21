package docs

import (
	"context"
	"sync/atomic"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// AskBlockReviewer is told, in a settlement's transaction, of each decision block the settlement
// opens and of each open one whose wording it moves: the server's reviewer of INFRA-3231's R5
// (api/decision_blocks.go), which may hold a new block from people's Inboxes there, in the same
// transaction that opens it, so no read of an Inbox or a count ever shows it before its hold. What
// each call returns runs once that transaction has committed (the review it starts), and not at
// all where the settlement does not commit. The settlement runs in two phases (Ledger): a call
// reads in its read phase, where the new block's ask row is queued and not yet written, and
// writes through Write, so its writes join the settlement's write phase after the ask's own.
type AskBlockReviewer interface {
	// AskBlockOpened is the decision block ask, which author's write opened; personTyped is
	// whether a person typed that write in a session (doc_ask_block_authors.person_typed).
	AskBlockOpened(ctx context.Context, tx pgx.Tx, ask model.Ask, author model.Actor, personTyped bool) (func(), error)
	// AskBlockReworded is the open decision block ask, whose wording editor's write moved from
	// previous to the ask's own; personTyped is whether a person typed that write in a session.
	AskBlockReworded(ctx context.Context, tx pgx.Tx, ask model.Ask, previous model.AskEditPrevious, editor model.Actor, personTyped bool) (func(), error)
}

// askBlockReviewerSlot holds the Service's AskBlockReviewer, which the server sets once it is
// built (the server is built after the Service it is handed).
type askBlockReviewerSlot struct {
	reviewer atomic.Pointer[AskBlockReviewer]
}

// SetAskBlockReviewer has every settlement from now on tell reviewer of the decision blocks it
// opens and rewords.
func (s *Service) SetAskBlockReviewer(reviewer AskBlockReviewer) {
	s.askBlocks.reviewer.Store(&reviewer)
}

// askBlockReviewer is the Service's AskBlockReviewer, nil for none.
func (s *Service) askBlockReviewer() AskBlockReviewer {
	if held := s.askBlocks.reviewer.Load(); held != nil {
		return *held
	}
	return nil
}

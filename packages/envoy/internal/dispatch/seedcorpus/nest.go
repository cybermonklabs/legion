package seedcorpus

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// nest is one reply the loader moves under another comment of its thread: comment's reply_to
// becomes parent.
type nest struct {
	comment, parent string
}

// nestReplies points each comment's comments.reply_to at its parent, in one transaction that
// holds the issue's owner lock first, as every writer of a comments row does (store.LockOwner).
// It is one of the loader's two statements of its own (the other is writeNobodysRun's row),
// decision 5 of dispatch://INFRA-1098's plan: the comment route stores every reply against its
// thread's head (api's threadHeadOf) and keeps the comment it answered in comments.answers, so a
// reply nested under a reply, which the walks over comments.reply_to read (outbox's
// loadRootCommentAuthors and commentRecipientsQuery), can be written by no route, and neither can
// a cycle of them. Each comment was written through the route first, so its node, its blocks and
// its event are the route's; this moves the one column the walks read and nothing else, and no
// tuple projects that column at this head. The head row of a comment holds that column
// (item_reply_to), so each statement claims the head rows kept (graphstore.KeptHeadRows) and the
// transaction writes the head rows of the comments it moved after them (graphstore.WriteHeads).
func (s *seeder) nestReplies(ctx context.Context, key string, nests []nest) error {
	return pgx.BeginFunc(ctx, s.database.Pool, func(tx pgx.Tx) error {
		if err := store.LockOwner(ctx, tx, store.IssueOwner(key)); err != nil {
			return fmt.Errorf("take the owner lock of %s: %w", key, err)
		}
		moved := make([]graphstore.NodeKey, 0, len(nests))
		for _, nested := range nests {
			tag, err := tx.Exec(ctx, `update comments set reply_to = $2::uuid where id = $1::uuid and issue_key = $3 and `+graphstore.KeptHeadRows,
				nested.comment, nested.parent, key)
			if err != nil {
				return fmt.Errorf("nest comment %s under %s: %w", nested.comment, nested.parent, err)
			}
			if tag.RowsAffected() != 1 {
				return fmt.Errorf("nest comment %s under %s moved %d rows, want one", nested.comment, nested.parent, tag.RowsAffected())
			}
			moved = append(moved, graphstore.NodeKey{Kind: graphstore.KindComment, ID: nested.comment})
		}
		return graphstore.WriteHeads(ctx, tx, moved)
	})
}

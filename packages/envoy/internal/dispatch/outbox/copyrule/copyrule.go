// Package copyrule reads the order rule of a comment's mention copies: the earlier event of the
// comment's owner that a copy waits behind before it reaches its session. It is the order the
// outbox keeps for its own sends (planChains in internal/dispatch/outbox), asked by the two roads
// a copy takes outside the outbox's chains - the API's own send of the mention
// (internal/dispatch/api) and the outbox's fallback pass (decideFallbackCopy) - so both call
// Holder, and nothing else reads the rule (TestOnlyHolderReadsTheCopyRule).
package copyrule

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// Querier is what Holder needs of a pool: one statement.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Holder is the earlier event that a copy of the comment whose comment.created event is eventID,
// bound for the session key key, waits behind, and nil while it waits for none. It reads the rule
// in one statement, so in one snapshot (model.MentionCopyRuleSQL): an earlier event that still
// owes a send that session may take - one to the owner's topic family, to key itself, or, its
// recipient read having failed, to any session - or whose record is older than the owner's newest
// event; and, only when there is none, the role routes the earlier events still owe. Each of those
// is asked of holds, the caller's read of who holds a role now: the route holds the copy when
// holds answers true for its role - the session holds it, or may - and the earliest event that
// owes the first such role, in role topic order, is the one answered. A key that names no session
// - a role topic, for a copy kept for the role's next holder - passes a nil holds, and no role
// route holds it. holds is asked once the statement's rows are read and its connection given
// back, so a slow read of a role's holder holds no connection.
func Holder(
	ctx context.Context, db Querier, eventID int64, key string, holds func(ctx context.Context, role string) bool,
) (*model.MentionCopyHolder, error) {
	rows, err := db.Query(ctx, model.MentionCopyRuleSQL, eventID, key)
	if err != nil {
		return nil, fmt.Errorf("read the earlier event that holds event %d's copy for %s: %w", eventID, key, err)
	}
	read, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (owed, error) {
		var o owed
		err := row.Scan(&o.roleTopic, &o.by.EventID, &o.by.PublishError)
		return o, err
	})
	if err != nil {
		return nil, fmt.Errorf("read the earlier event that holds event %d's copy for %s: %w", eventID, key, err)
	}
	for _, o := range read {
		if o.roleTopic == "" || (holds != nil && holds(ctx, strings.TrimPrefix(o.roleTopic, contracts.RoleTopicPrefix))) {
			return &o.by, nil
		}
	}
	return nil, nil
}

// owed is one row of model.MentionCopyRuleSQL: the earlier event by, and the role topic it owes,
// "" for the row of an event that holds the copy whoever holds which role.
type owed struct {
	roleTopic string
	by        model.MentionCopyHolder
}

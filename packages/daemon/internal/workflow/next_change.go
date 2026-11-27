package workflow

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// ChangePlace is where a next change of an issue stands when it starts (StartNextChange): the
// tree it runs in, the parent it hangs under there, and since when its close waits. A reason
// found after a merge starts the change where the issue already stands (ChangeInPlace); a
// request for new work on a closed issue names the placement its own door decided (D17).
type ChangePlace struct {
	// Tree is the tree the change runs in and Parent the issue's parent in it, nil for none.
	Tree   string
	Parent *string
	// WaitsSince is when the issue's close waits from (Issue.CloseWaitSince): nil carries the
	// moment the merge set, which every reason after a merge does, and a time is the moment a
	// request applied (D1).
	WaitsSince *time.Time
}

// ChangeInPlace is the placement of a change that starts where the issue already stands: its own
// tree and parent, carrying the wait its merge began.
func ChangeInPlace(issue record.Issue) ChangePlace {
	return ChangePlace{Tree: issue.Tree, Parent: issue.Parent}
}

// StartNextChange is the one transition of new work on an issue whose work merged (D7): the next
// generation of that issue, at admitted with its gate kept, carrying reasons. Its callers are the
// judge of the close, for a reason found after the merge, and, from Task 13, each entry door of a
// request for new work.
func (e *Engine) StartNextChange(ctx context.Context, tx pgx.Tx, issue record.Issue, reasons []record.Defect, place ChangePlace) error {
	return nil
}

// endChange is the one end of a next change: at its merge, and at its end with no pull request
// once each reason it carries is resolved (D7).
func (e *Engine) endChange(ctx context.Context, tx pgx.Tx, issue record.Issue, pr *record.PullRequest) error {
	return nil
}

package workflow

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// completingRole is the role whose completion a move of the table records, by its trigger: the
// phases row the daemon hands the move (handoff, settleRound) is that role's, so the task of the
// role started next names that role's handoff. A move no completion makes — a conflicting head's
// or red checks' move back — hands the move no row, as the daemon's mergeability does.
var completingRole = map[TriggerKind]claim.Role{
	TriggerPlannerCompleted:    claim.RolePlanner,
	TriggerImplementationReady: claim.RoleImplementer,
	TriggerTesterPassed:        claim.RoleTester,
	TriggerTesterFailed:        claim.RoleTester,
	TriggerReviewApproved:      claim.RoleReviewer,
	TriggerReviewRejected:      claim.RoleReviewer,
	TriggerRetroCompleted:      claim.RoleImplementer,
}

// ReplayTransition makes, in tx, one move of a recorded window that a replay of the window
// (internal/replay) enters at the end of its work segment: key's issue, at generation, moved from
// from to to by trigger, through the row of the table of the issue's line under the engine's slot
// rule (Walk.table) and the transition the daemon runs for that trigger, so every effect of the
// move is the daemon's own — the status it writes, the start of the next phase's worker with its
// slot mark and its entry, the notice, the merge forward on entry, the leave of a move to done.
// The checks a live completion passes before it reaches the transition (its claim's run, its
// handoff record, a review's head) are not made: the record shows the completion passed them.
//
// It answers why it refused the move, and "" for a move made. It refuses, writing nothing, unless
// the issue is recorded, stands in from at generation, a row of the table serves the move with the
// issue's pull request as the record holds it (the guard of implementation_ready reads it), and
// the issue's tree does not linger; and once the transition ran it reads the issue back, and
// answers a move that left the issue anywhere but to as refused too, though what the transition
// wrote stands. A replay counts every refusal with its reason, and opens no segment after one.
func (e *Engine) ReplayTransition(ctx context.Context, tx pgx.Tx, key string, generation uint64, from phase.Phase, trigger TriggerKind,
	to phase.Phase,
) (string, error) {
	issue, err := e.store.Issue(ctx, tx, key)
	if err != nil {
		return "", err
	}
	switch {
	case issue == nil:
		return fmt.Sprintf("%s is not recorded", key), nil
	case issue.Generation != generation:
		return fmt.Sprintf("%s stands at generation %d, and the recorded move of %s by %s is generation %d's", key, issue.Generation, from,
			trigger, generation), nil
	case issue.Phase != from:
		return fmt.Sprintf("%s stands in %s, and the recorded move by %s leaves %s", key, issue.Phase, trigger, from), nil
	}
	pr, err := e.store.PullRequest(ctx, tx, key)
	if err != nil {
		return "", err
	}
	w, err := e.walk(ctx, tx, *issue)
	if err != nil {
		return "", err
	}
	if _, served := w.row(from, trigger, to, Snapshot{Phase: from, HasOpenOrMergedPR: record.LetsImplementationMove(pr)}); !served {
		return fmt.Sprintf("no row of the table moves %s from %s to %s by %s with its pull request as recorded", key, from, to, trigger), nil
	}
	lingers, err := record.TreeLingers(ctx, e.store, tx, issue.Tree)
	if err != nil {
		return "", err
	}
	if lingers {
		return fmt.Sprintf("the tree %s of %s lingers, which holds its members where they stand", issue.Tree, key), nil
	}
	handoff := record.PhaseRow{}
	if role, completes := completingRole[trigger]; completes {
		if handoff, err = e.phaseRow(ctx, tx, key, role); err != nil {
			return "", err
		}
		// The record shows the completion accepted at this moment, as a live one stamps it
		// (handoff's CompletedAt).
		handoff.CompletedAt = e.now()
	}
	if err := e.transition(ctx, tx, *issue, trigger, to, handoff, pr, ""); err != nil {
		return "", err
	}
	moved, err := e.store.Issue(ctx, tx, key)
	if err != nil {
		return "", err
	}
	if moved == nil || moved.Phase != to {
		at := "no record"
		if moved != nil {
			at = string(moved.Phase)
		}
		return fmt.Sprintf("the move of %s by %s left it in %s, not %s", key, trigger, at, to), nil
	}
	return "", nil
}

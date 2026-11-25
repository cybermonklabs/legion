package daemon

import (
	"context"
	"fmt"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/wait"
)

// suspendToLeave makes machine's claim run nothing where work, the start's, leaves the session the
// claim records (supervise.Machine.Leaves, the one test of that) and the claim still runs a
// process: that process runs the session the start's stored step names to leave
// (record.PlanStep.LeaveSession, which no writer sets now), which the agent must not resume. It
// suspends the claim and waits until it runs nothing, as prepareClaimForStep does for a re-role
// (suspendUntilIdle). The start then brings the claim back, and the decision that does leaves the
// session (supervise's revive). A start that leaves no session, or one the claim no longer
// records, and a claim that already runs nothing, ask nothing.
//
// ctx carries the standing of the step the start is for where it has one (stepStanding): a step
// the issue left before the machine's lock is supervise.ErrStepLeft, with nothing of the claim
// changed.
func (r *outbox) suspendToLeave(ctx context.Context, machine *supervise.Machine, token claim.Token, row record.OutboxRow,
	payload record.SuperviseRequest, work supervise.StartWork) error {
	if !machine.Leaves(work) || machine.Claim().RunsNothing() {
		return nil
	}
	reason := fmt.Sprintf("its agent's step on %s starts in a session of its own, which leaves session %s", row.Issue, work.LeaveSession)
	return suspendUntilIdle(ctx, machine, token, reason, func(supervise.Claim) error {
		return wait.Errorf("the %s step of %s waits for its claim %s to end its turn in session %s, which it leaves", payload.Role, row.Issue, token, work.LeaveSession)
	})
}

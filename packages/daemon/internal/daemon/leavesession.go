package daemon

import (
	"context"
	"fmt"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/wait"
)

// suspendToLeave makes machine's claim run nothing where work, the start's, leaves the session the
// claim records (supervise.Machine.Leaves, the one test of that) and the claim still runs a
// process: that process runs the session of the key whose name the start's agent took, which the
// agent must not resume (workflow's leaveKeySessions). It suspends the claim and waits until it
// runs nothing, as reroleForStep does (suspendUntilIdle). The start then brings the claim back,
// and the decision that does leaves the session (supervise's revive). A start that leaves no
// session, or one the claim no longer records, and a claim that already runs nothing, ask nothing.
//
// A start of a step's entry asks the suspension with the step's standing, as reroleForStep does
// (supervise.Standing, stepStands): a step the issue left before the machine's lock is
// supervise.ErrStepLeft, with nothing of the claim changed.
func suspendToLeave(ctx context.Context, machine *supervise.Machine, token claim.Token, issue string,
	role claim.Role, work supervise.StartWork) error {
	if !machine.Leaves(work) || machine.Claim().RunsNothing() {
		return nil
	}
	reason := fmt.Sprintf("its agent's step on %s starts in a session of its own, which leaves session %s", issue, work.LeaveSession)
	return suspendUntilIdle(ctx, machine, token, reason, func(supervise.Claim) error {
		return wait.Errorf("the %s step of %s waits for its claim %s to end its turn in session %s, which it leaves", role, issue, token, work.LeaveSession)
	})
}

package daemon

import (
	"context"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// taskConfirmations is a workflow handler that counts the confirmation facts applied through it.
// A fact the intake finds already processed reaches no handler, so the count is how many
// confirmations the workflow was told.
type taskConfirmations struct {
	mu   sync.Mutex
	seen []intake.TaskConfirmed
}

func (h *taskConfirmations) Apply(_ context.Context, _ pgx.Tx, fact intake.Fact) (intake.Result, error) {
	if confirmed, ok := fact.(intake.TaskConfirmed); ok {
		h.mu.Lock()
		h.seen = append(h.seen, confirmed)
		h.mu.Unlock()
	}
	return intake.Result{}, nil
}

func (h *taskConfirmations) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.seen)
}

// The confirmation of an accepted take reaches the workflow once per task, and a task numbered 1
// again after the store migration's way back and roll-forward is a task of its own
// (dispatch://INFRA-2811). processed_events, which dedupes the fact, is never pruned, and the
// second fill numbers every claim from task 1 again, so the confirmation's event id names the
// task's delivery id beside its number (confirmationEventID). The claim's first lifetime takes
// task 1 of delivery outbox:71 and the workflow is told; the same task taken back and taken
// again by a relaunched process is the same event and comes back a duplicate; after the rollback
// and the second fill, the claim takes task 1 of delivery outbox:90, an older daemon's task, and
// the workflow is told again.
//
// The negative control is the event id of the number alone (`task-confirmed:<token>:task-<n>`):
// the second lifetime's confirmation then comes back a duplicate, and the workflow is told once.
func TestTaskConfirmationAfterTheRollbackAndASecondFillReachesTheWorkflow(t *testing.T) {
	pool := isolatedOutboxPool(t)
	ctx := context.Background()
	facts := &taskConfirmations{}
	w := &workflowRuntime{pool: pool, handlers: []intake.Handler{facts}, log: quietLogger()}

	taken := supervise.Claim{Token: mustClaimToken(t, "LEGION-208", claim.RoleImplementer), Project: "legion",
		Tree: "LEGION-208", Issue: "LEGION-208", Role: claim.RoleImplementer, Generation: 1,
		Pending: &supervise.Delivery{ID: "outbox:71", Task: "Implement the change.", Number: 1}}
	w.taskConfirmed(taken)
	testwait.Eventually(t, "the workflow told of the first lifetime's task 1", func() bool { return facts.count() == 1 })

	retaken := taken
	retaken.Generation = 2
	again, err := intake.ApplyFact(ctx, pool, "supervise", confirmationEventID(retaken),
		intake.TaskConfirmed{Issue: retaken.Issue, Role: retaken.Role, Generation: retaken.Generation}, facts)
	if err != nil {
		t.Fatalf("apply the confirmation of the task taken again: %v", err)
	}
	if !again.Duplicate || facts.count() != 1 {
		t.Fatalf("the confirmation of task 1 of outbox:71 taken again by a relaunched process = %+v with %d told, want a duplicate and 1 told: a take-back keeps the task",
			again, facts.count())
	}

	renumbered := taken
	renumbered.Generation = 3
	renumbered.Pending = &supervise.Delivery{ID: "outbox:90", Task: "Review the change.", Number: 1}
	w.taskConfirmed(renumbered)
	testwait.Eventually(t, "the workflow told of task 1 of the roll-forward's delivery outbox:90, which the first lifetime's task 1 is not", func() bool {
		return facts.count() == 2
	})
}

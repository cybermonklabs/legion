package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// A claim held failed while it holds only a task of news of the approved design alone
// (supervise.Delivery.NewsOnly) holds no task of its phase: the news task names the phase and the
// run, so that it is dropped once the issue leaves the phase, and asks the worker only to read
// what changed. So the architect's retry of the held phase relaunches the claim and still hands it
// the retry's own task, as it does to a claim that kept a task of another phase or run. The retry
// here carries no entry (retryHeldPhase), so it carries no news either, and its task waits behind
// the news task (the claim refuses a second pending one), and the row is retried until it goes; a
// start that carries an entry takes the news task's place instead, and carries its news
// (supervise's TestAnEntrysTaskTakesTheUntakenNewsTasksPlaceForEveryRole). Read as the phase's
// task, the news task would have the retry deliver nothing, and the relaunched worker would get the
// news and no work.
func TestARetryOfAFailedClaimHoldingOnlyANewsTaskStillDeliversItsPhaseTask(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	issue := record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION",
		Tree: "LEGION-208", Title: "Workflow", Phase: phase.Implementing, Generation: 2, Status: "in_progress"}
	putOutboxIssue(t, pool, records, issue)
	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	kept := supervise.Delivery{ID: "outbox:41", Task: "The approved design of LEGION-208 changed.", Phase: phase.Implementing,
		Generation: issue.Generation, QueuedAt: time.Now(), News: []byte(`{"task":""}`), NewsOnly: true}
	machine := heldImplementer(t, sup, issue, kept)
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, supervisor: sup, tokens: outboxTokens{},
		project: "legion", stateDir: t.TempDir()}

	err := runner.execute(context.Background(), retryHeldPhase(t, issue))

	if !errors.Is(err, supervise.ErrDeliveryPending) {
		t.Fatalf("start the held claim = %v, want its own task waiting behind the news task (%v)", err, supervise.ErrDeliveryPending)
	}
	if got := machine.Claim(); got.State != supervise.StateLaunching || len(rt.CallsOf("Resume")) != 1 || got.Pending == nil || got.Pending.ID != kept.ID {
		t.Fatalf("retried claim = %s holding %+v after %d resumes, want it relaunched once, holding the news task its own task waits behind",
			got.State, got.Pending, len(rt.CallsOf("Resume")))
	}
}

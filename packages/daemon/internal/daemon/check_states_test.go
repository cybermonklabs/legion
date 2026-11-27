package daemon

import (
	"context"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/checkstatetest"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// phaseVerdictGoneVersion is the migration that takes the tester's verdict out of the record
// (internal/store/migrations/20261126110301_phase_verdict_gone.up.sql): a rollback past it runs its
// way back before any older one, the newest first (docs/kubernetes.md).
const phaseVerdictGoneVersion = 20261126110301

// entryRigHead is the head of the pull request outbox_entry_test.go's rig records: a full commit id,
// since a check.state_changed event names its head as one.
const entryRigHead = "0000000000000000000000000000000000c0ffee"

// testedFact is the check.state_changed event that decides a tester's completion of issue's
// testing, decoded, and the event id the live consumer would apply it under
// (checkstatetest.Tested): k1, the head's one check, yes from the tester where passes, and no for a
// fail where not, at head, the commit the completion names, on acme/widgets where issue has no
// pull request.
func testedFact(t *testing.T, pool store.TxPool, issue, head string, passes bool) (intake.DispatchCheckState, string) {
	t.Helper()
	return checkstatetest.Tested(t, pool, issue, "acme/widgets", head, passes)
}

// testedAs applies testedFact's event through intake.ApplyFact and handlers, as the live consumer
// applies it, ahead of the tester's completion it decides. With no handlers it applies the event
// through a workflow of its own over pool, which holds the state as the daemon's own workflow
// does: a live proof's daemon runs no consumer of Dispatch's events, and the completion that
// follows is the daemon's own to decide on what the store then holds.
func testedAs(t *testing.T, pool store.TxPool, issue, head string, passes bool, handlers ...intake.Handler) {
	t.Helper()
	if len(handlers) == 0 {
		handlers = []intake.Handler{workflow.New(record.NewStore(), workflow.Config{Project: "LEGION"}, quietLogger())}
	}
	fact, id := testedFact(t, pool, issue, head, passes)
	if result, err := intake.ApplyFact(context.Background(), pool, "dispatch", id, fact, handlers...); err != nil || result.Refusal != nil {
		t.Fatalf("apply the state event %s: %+v, %v", id, result.Refusal, err)
	}
}

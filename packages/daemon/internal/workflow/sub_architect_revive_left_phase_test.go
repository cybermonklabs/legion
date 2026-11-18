package workflow

import (
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// A start already queued for a failed child architect stands in for the revive only when the
// outbox will act on it. A tree's re-admission brings back each role of a mid-phase child that has
// a claim, a failed architect among them, with a start that names the phase the child stood in
// (admit's startMidPhaseMembers), and the outbox finishes, without acting, a start that serves a
// phase the issue has left. So when the child's next phase start comes while that start still
// waits (behind its branch row, say), the start it would count is one the outbox will drop, and
// the child's next phase start must queue the plain start of its failed architect all the same:
// otherwise the architect stays failed through the whole phase, against R2's "the daemon starts
// it again at the child's next phase start".
func TestAChildsNextPhaseStartRevivesItsFailedArchitectPastAStartOfAPhaseItLeft(t *testing.T) {
	pool := migratedPool(t)
	seedReviveTree(t, pool, phase.Implementing)
	seedIssue(t, pool, reviveIssue(reviveChild, phase.Planning, PhaseStatus(phase.Planning)))
	plannerCompletes.seed(t, pool, reviveChild)
	seedIssuesArchitect(t, pool, reviveChild, supervise.StateFailed, "")
	// The start re-admission queued while the child stood in planning, still waiting.
	left := record.SuperviseRequest{Op: "start", Tree: reviveRoot, Role: claim.RoleArchitect, Generation: 1, Phase: phase.Planning}
	row, err := record.NewOutboxRow(reviveChild, left, time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("build the waiting architect start of %s: %v", reviveChild, err)
	}
	seedRecord(t, pool, func(tx pgx.Tx) error { return record.NewStore().Enqueue(t.Context(), tx, row) })

	plannerCompletes.moved(t, pool, testEngine(config.DesignGateRootIssues, nil), reviveChild)
	got := architectStartsOf(t, pool, reviveChild)
	if !slices.Contains(got, plainArchitectStart(1)) {
		t.Fatalf("the child's architect starts after the planner's completion = %#v; want the plain start %#v beside the waiting start of planning, which the outbox finishes without acting once the child stands in implementing",
			got, plainArchitectStart(1))
	}
}

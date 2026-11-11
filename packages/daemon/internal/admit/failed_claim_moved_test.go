package admit

import (
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// A claim that fails takes its issue into held, which is a phase change and so a movement the
// record keeps (dispatch://INFRA-2560, R3, and Engine.claimFailed). The measure reads it: a
// tree stops at the moment its worker dies, so its quiet time runs from that moment and not
// from whatever last touched the issue before it. Without the mark a tree whose worker died
// hours into a silent phase reads as quiet for longer than it has been, and the controller is
// told it stopped at the wrong moment.
//
// It is a site of its own because it is reached by a fact of its own: the supervisor's report
// that a claim is dead, which no other movement of R3's list carries. TestTheRecordKeepsWhen-
// AnIssueLastMoved drives the six movements the requirement names and the four writes of the
// admission pass, and none of them reaches this one; with the mark taken off this line, every
// check of this issue stays green, which is why the lock is here.
//
// The second report is the other half: an issue already held writes nothing, so a supervisor
// that reports the same dead claim again moves the tree nowhere.
func TestAFailedClaimsHoldIsAMovementOfItsIssue(t *testing.T) {
	pool := migratedPool(t)
	const issueKey = "LEGION-780"
	now := fixedNow
	engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Repositories: testRepositories,
		Clock: func() time.Time { return now }}, nil)
	admission := New(record.NewStore(), engine, 1, testProject, nil)
	admission.now = func() time.Time { return fixedNow }

	seedSlotted(t, pool, issueKey, "U")
	stands := issue(t, pool, issueKey)
	stands.Phase, stands.Status, stands.DispatchStatus, stands.LastDispatchSeq = phase.Implementing, "in_progress", "in_progress", 1
	putIssue(t, pool, stands)
	before := movedAt(t, pool, issueKey)
	if before.IsZero() {
		t.Fatalf("the record of %s carries no movement before the failure: every row is stamped when it is written", issueKey)
	}

	// The clock stands past the row's own stamp, because a movement is only ever written
	// forwards: a clock behind it would prove nothing.
	died := before.Add(time.Minute)
	now = died
	applyMovement(t, pool, admission, engine, "api", "implementer-failed",
		intake.ClaimFailed{Issue: issueKey, Role: claim.RoleImplementer})

	if got := issue(t, pool, issueKey).Phase; got != phase.Held {
		t.Fatalf("%s stands in %s after its implementer's claim failed, want held: the fact must be the phase change this test reads", issueKey, got)
	}
	if got := movedAt(t, pool, issueKey); !got.Equal(died) {
		t.Fatalf("after its worker's claim failed the record says %s last moved at %s, want %s, the moment of that fact: a tree stops when its worker dies",
			issueKey, got.Format(time.RFC3339Nano), died.Format(time.RFC3339Nano))
	}

	// The same dead claim reported again: the issue is held already, so nothing is written and
	// the tree moves nowhere.
	now = died.Add(time.Hour)
	applyMovement(t, pool, admission, engine, "api", "implementer-failed-again",
		intake.ClaimFailed{Issue: issueKey, Role: claim.RoleImplementer})

	if got := movedAt(t, pool, issueKey); !got.Equal(died) {
		t.Fatalf("after a second report of the same dead claim the record says %s last moved at %s, want %s still: a repeat of a fact the record already took moves no tree",
			issueKey, got.Format(time.RFC3339Nano), died.Format(time.RFC3339Nano))
	}
}

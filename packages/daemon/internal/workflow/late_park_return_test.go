package workflow

import (
	"fmt"
	"slices"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
)

// A person parks a root, approves its spec at sequence 9, moves it to todo at 10 and parks it
// again at 11, in the status it had. Events can apply out of their order: the approval's
// preparation and the todo event's both read Dispatch, and a read that fails sends each back for
// a later delivery, while the park at 11 needs no read and applies first. The copy of Dispatch's
// issues then holds the park at 11 as no change of status, and takes nothing of the todo at 10
// when it comes, being older than what it holds; the approval at 9, applied last, must still
// queue nothing, since the person parked the root after approving it.
func TestALateApprovalQueuesNoRootParkedAgainInTheSameStatusAfterIt(t *testing.T) {
	event := func(seq int64, status string) intake.DispatchIssue {
		return intake.DispatchIssue{Key: "LEGION-208", ResolvedRepo: "sjawhar/legion", Seq: seq, Type: "issue.updated", Status: status,
			Title: "root", Rank: "U", HandedOver: true}
	}
	pool := migratedPool(t)
	engine := testEngine(config.DesignGateRootIssues, nil)
	// Parked before the approval, parked again after it; the move to todo between them arrives late.
	for _, fact := range []intake.DispatchIssue{event(5, "icebox"), event(11, "icebox"), event(10, "todo")} {
		applyMirrored(t, pool, engine, fmt.Sprintf("event-%d", fact.Seq), fact)
	}

	applyMirrored(t, pool, engine, "late-approval", approvalOf(askPerson, parkedRootView("icebox")))

	if writes := statusWrites(t, pool, "LEGION-208"); slices.Contains(writes, personHandoverWrite("icebox")) {
		t.Fatalf("the approval at sequence 9 queued the person's handover of a root they moved to todo at 10 and parked again at 11; status writes %#v", writes)
	}
}

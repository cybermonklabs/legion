package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// Main's parked-decision notice (record/parked_decision.go:RelayParkedDecision at d46ec4e0) named
// the decision, the root, where in Dispatch, who and a spec decision's version, and no time:
// ControllerNotice{Kind: ParkedDecisionNotice, Decision: decision.Decision, Root: root.Key, Ref:
// decision.Ref, Actor: decision.Actor, Version: version, Reason: reason}. The branch adds `at`, and
// roles/controller-root.md says the notice names "who made it and when (`actor` and `at`)".
// INFRA-3686's R2: "The fields are those of the kind, the issue, the act, the actor, the time …";
// the change's own wrong fact is its defect. A person's approval reaches the outbox as a
// design-approved notice that carries when the person approved (workflow.DesignApprovedNotice,
// `at`, from the approval's Dispatch time); when nobody in the tree hears it, parkDecision keeps
// the decision at the runner's clock (`At: now`) and the parked-decision notice names that time,
// not the approval's.
func TestAParkedSpecApprovalNamesWhenThePersonApproved(t *testing.T) {
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	noticeTree(t, pool, records, true)
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	architectClaim(t, sup, "LEGION-1", supervise.StateSuspended)
	approvedAt := time.Now().Add(-6 * time.Hour).UTC().Truncate(time.Second)
	approved, err := record.NoticeOf(workflow.DesignApprovedNotice("LEGION-1", record.DesignStepSpec, 3, "user:alice", approvedAt))
	if err != nil {
		t.Fatal(err)
	}
	if !approved.At.Equal(approvedAt) {
		t.Fatalf("the design-approved notice names %s, want the approval's time %s", approved.At, approvedAt)
	}
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, notices: &holderPublisher{},
		supervisor: sup, project: "legion", now: time.Now}
	row := leasedOutboxRow(t, pool, records, mustOutboxRow(t, "LEGION-1", approved, time.Now()))
	if err := runner.notice(ctx, row, approved); err != nil {
		t.Fatal(err)
	}
	parked := parkedDecisions(t, pool)
	if len(parked) != 1 || parked[0].Decision != record.DecisionDesignApproved || parked[0].Actor != "user:alice" {
		t.Fatalf("parked decisions %+v, want one design-approved decision by user:alice", parked)
	}
	body, err := os.ReadFile(filepath.Join("..", "prompts", "roles", "controller-root.md"))
	if err != nil {
		t.Fatal(err)
	}
	var passage string
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, "A `parked-decision` notice says") && strings.Contains(line, "who made it and when (`actor` and `at`)") {
			passage = "who made it and when (`actor` and `at`)"
		}
	}
	if passage != "" && !parked[0].At.Equal(approvedAt) {
		t.Errorf("roles/controller-root.md says the parked-decision notice names %q, but the notice of user:alice's approval at %s names at: %s, "+
			"the time the daemon parked it", passage, approvedAt.Format(time.RFC3339), parked[0].At.Format(time.RFC3339))
	}
}

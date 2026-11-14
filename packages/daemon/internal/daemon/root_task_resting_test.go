package daemon

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// A root architect's start task names the roots that rest on the root's approval apart from the
// decisions it names (workflow.ArchitectStartTask, dispatch://INFRA-3320 R8), so a held task the
// send narrows of its decisions (the daemon's rootTask) still names those roots, as the record
// shows them then: a task left naming no decision is sent naming them alone, beside the approval
// it kept where it kept one, and is retired only when no such root is left in the workflow either.
func TestARootTaskNarrowedOfEveryDecisionStillNamesTheRootsRestingOnIt(t *testing.T) {
	const resting = "LEGION-320"
	restingTask := "These roots rest on the approval of LEGION-310 and are still in the workflow: LEGION-320 (its spec step, in implementing). " +
		"The daemon refuses the sign-off and the close of LEGION-310 while any of them is in the workflow (SIGNOFF_RESTING_ROOT_IN_WORKFLOW). " +
		"Each root's own architect, or a person, moves it."
	approval := record.KeptApproval{Step: record.DesignStepSpec, Approved: 2, Latest: 4}
	approvalTask := "A person approved version 2 of the primary document, and the primary document is now at version 4. " +
		"Request approval of version 4, as skill://legion-architect/references/design-gate.md (\"The armed gate\") says. " +
		"The other way is to write LEGION-310 as a view on an approved ancestor and register the step on that ancestor's approval, as the same page says."
	for _, tc := range []struct {
		name     string
		at       phase.Phase
		status   string
		approval record.KeptApproval
		want     string
		kept     bool
	}{
		{name: "a resting root in the workflow", at: phase.Implementing, status: "in_progress", want: restingTask},
		{name: "a resting root in the workflow beside the approval kept", at: phase.Implementing, status: "in_progress", approval: approval,
			want: approvalTask + " " + restingTask, kept: true},
		{name: "a resting root that is done", at: phase.Done, status: "done", want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			putOutboxIssue(t, pool, records, record.Issue{Key: heldTaskRoot, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
				Project: heldTaskProject, Tree: heldTaskRoot, Title: "Root", Phase: phase.Admitted, Generation: 1, Status: "in_progress", Rank: "A"})
			putOutboxIssue(t, pool, records, record.Issue{Key: resting, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
				Project: heldTaskProject, Tree: resting, Title: "Resting", Phase: tc.at, Generation: 1, Status: tc.status, Rank: "B"})
			at := time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
			if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
				return records.PutGate(ctx, tx, record.OpenOnBasis(record.DesignGate{Issue: resting, ArtifactID: "artifact-320", LatestVersion: 2},
					record.GateBasis{Issue: heldTaskRoot, ArtifactID: "artifact-310", Version: 3, Approval: record.EventMark{Seq: 40, At: at},
						Approver: "user:alice", RegisteredAt: at}))
			}); err != nil {
				t.Fatalf("put the resting root's gate: %v", err)
			}
			// The task named one decision this delivery no longer holds: something else told it.
			named, err := json.Marshal(record.KeptDecisions{Decisions: []record.DecisionMark{{Ref: "dispatch://LEGION-310/ask/ask-1", At: at,
				Issue: heldTaskRoot, Decision: record.DecisionAskAnswered, Actor: "user:alice"}}, KeptApproval: tc.approval})
			if err != nil {
				t.Fatalf("encode what the task names: %v", err)
			}
			runtime := &workflowRuntime{pool: pool, records: records}
			task, kept, err := runtime.rootTask(ctx, supervise.Claim{Token: mustClaimToken(t, heldTaskRoot, claim.RoleArchitect), Tree: heldTaskRoot},
				supervise.Delivery{ID: "outbox-71", Task: "a person decided on the tree", Kept: named})
			if err != nil {
				t.Fatalf("narrow the held task: %v", err)
			}
			if task != tc.want || (kept != nil) != tc.kept {
				t.Fatalf("the narrowed task = %q naming %s, want %q naming something = %t", task, kept, tc.want, tc.kept)
			}
		})
	}
}

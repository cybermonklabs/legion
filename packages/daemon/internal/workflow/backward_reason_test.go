package workflow

import (
	"context"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/dispatchbody"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A backward move between two phases that show the same Dispatch status writes no status row, so
// the reason goes as a message row of its own: carried only on a status row, it would reach no
// issue, and the issue would show nothing of the move. In the default steps the three such pairs
// are the implementer asking for a new plan (in_progress), the architect sending accepting back
// to reviewing (needs_review) and the merger sending merging back to retro (retro); a task whose
// planner stands after its implementer, or whose reviewer stands after its acceptance, moves back
// between the same two statuses the other way, planning to implementing or reviewing to
// accepting.
func TestABackwardMoveThatKeepsItsStatusPostsItsReasonAsAMessage(t *testing.T) {
	for _, tc := range []struct {
		from, to phase.Phase
		want     string
	}{
		{phase.Implementing, phase.Planning,
			"Legion moved this issue from implementing back to planning at the implementer's request: the plan names the wrong table."},
		{phase.Accepting, phase.Reviewing,
			"Legion moved this issue from accepting back to reviewing at the architect's request: the plan names the wrong table."},
		{phase.Merging, phase.Retro,
			"Legion moved this issue from merging back to retro at the merger's request: the plan names the wrong table."},
	} {
		t.Run(string(tc.from)+"-to-"+string(tc.to), func(t *testing.T) {
			if PhaseStatus(tc.from) != PhaseStatus(tc.to) {
				t.Fatalf("%s shows %q and %s %q: this case is for two phases of one status", tc.from, PhaseStatus(tc.from), tc.to, PhaseStatus(tc.to))
			}
			pool := migratedPool(t)
			role := phase.RoleFor(tc.from)
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "backward", Phase: tc.from, Generation: 1, Status: PhaseStatus(tc.from), Rank: "U"})
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: role, Claim: "claim"})
			engine := testEngine(config.DesignGateRootIssues, nil)
			move := intake.BackwardMove{Issue: "LEGION-208", Requester: role, To: tc.to, Reason: "the plan names the wrong table"}
			if role == claim.RoleArchitect {
				// An architect's move names its owner and the run it read (api's phaseBackward).
				move = rootsMoveBack(t, pool, engine, "LEGION-208", tc.to, "the plan names the wrong table")
			}
			applyRefusingNothing(t, pool, engine, move)
			assertPhase(t, pool, tc.to)
			var statuses int
			if err := pool.QueryRow(context.Background(), "select count(*) from outbox where kind = $1", string(record.OutboxKindDispatchStatus)).Scan(&statuses); err != nil || statuses != 0 {
				t.Fatalf("status rows = %d, %v; want none, since the status does not change", statuses, err)
			}
			rows, err := pool.Query(context.Background(), "select payload->>'body' from outbox where kind = $1 and issue = 'LEGION-208' order by id", string(record.OutboxKindDispatchMessage))
			if err != nil {
				t.Fatalf("read the message rows: %v", err)
			}
			var bodies []string
			for rows.Next() {
				var body string
				if err := rows.Scan(&body); err != nil {
					t.Fatalf("scan a message row: %v", err)
				}
				bodies = append(bodies, body)
			}
			if err := rows.Err(); err != nil {
				t.Fatalf("read the message rows: %v", err)
			}
			if len(bodies) != 1 || bodies[0] != tc.want {
				t.Fatalf("message rows = %q, want the one reason %q", bodies, tc.want)
			}
		})
	}
}

// A backward move whose post Dispatch would refuse is refused before anything moves, as an
// architect's close is (api's closeRoot): refused at the post, the reason would reach no issue and
// the worker would never learn why. The bound is the post's, the move's sentence under the
// outbox's marker, so a reason of as many blocks as Dispatch takes on its own is refused for the
// block the marker adds; an ask block is refused however short the reason is, under a code of
// its own. Each refusal moves, counts and queues nothing.
func TestABackwardMoveWhosePostDispatchWouldRefuseIsRefused(t *testing.T) {
	const decision = ":::ask{urgency=\"med\"}\nWhich table should the plan name?\n\n- users: the old one\n- accounts: the new one\n:::"
	for _, tc := range []struct {
		name, reason, code string
		want               []string
	}{
		{"a reason of as many blocks as Dispatch holds a body to",
			strings.TrimSuffix(strings.Repeat("y\n\n", dispatchbody.ItemBlocks), "\n\n"), "BACKWARD_REASON_TOO_LONG",
			[]string{"the reason makes a message Dispatch refuses", "the move was not applied", "shorter reason"}},
		{"a reason holding an ask block",
			"The plan is wrong.\n\n" + decision, "BACKWARD_REASON_DECISION_BLOCK",
			[]string{"the reason holds an ask block", "the move was not applied"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "backward", Phase: phase.Testing, Generation: 1, Status: PhaseStatus(phase.Testing), Rank: "U"})
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleTester, Claim: "test-claim"})
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim"})
			result, err := intake.ApplyFact(context.Background(), pool, "api", "backward", intake.BackwardMove{Issue: "LEGION-208", Requester: claim.RoleTester, To: phase.Implementing, Reason: tc.reason}, testEngine(config.DesignGateRootIssues, nil), admissionStub{})
			if err != nil || refusalCode(result) != tc.code {
				t.Fatalf("the move = %+v, %v; want %s", result.Refusal, err, tc.code)
			}
			for _, want := range tc.want {
				if !strings.Contains(result.Refusal.Message, want) {
					t.Fatalf("refusal %q does not say %q", result.Refusal.Message, want)
				}
			}
			if got, pass := issuePhase(t, pool), implementerPass(t, pool); got != phase.Testing || pass != 0 {
				t.Fatalf("phase = %q, pass %d; want testing and no pass", got, pass)
			}
			assertOutboxKinds(t, pool, []string{})
		})
	}
}

// The reason Dispatch takes on its own may still make a post it refuses, since the move's sentence
// and the outbox's marker come with it, so the check reads the post and not the reason; a reason
// one block shorter is taken.
func TestABackwardReasonIsMeasuredAsItsPost(t *testing.T) {
	reason := strings.TrimSuffix(strings.Repeat("y\n\n", dispatchbody.ItemBlocks), "\n\n")
	if err := dispatchbody.Check(reason); err != nil {
		t.Fatalf("the reason alone = %v, want Dispatch taking it", err)
	}
	if code, _ := backwardReasonRefusal(phase.Testing, phase.Implementing, reason); code != "BACKWARD_REASON_TOO_LONG" {
		t.Fatalf("the reason's post refuses as %q, want BACKWARD_REASON_TOO_LONG", code)
	}
	shorter := strings.TrimSuffix(strings.Repeat("y\n\n", dispatchbody.ItemBlocks-1), "\n\n")
	if code, why := backwardReasonRefusal(phase.Testing, phase.Implementing, shorter); code != "" {
		t.Fatalf("a reason one block shorter refuses as %q (%s), want it taken", code, why)
	}
}

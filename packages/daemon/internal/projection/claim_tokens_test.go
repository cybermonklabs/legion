package projection

import (
	"context"
	"slices"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// Two live claims hold the tester's role of an issue at testing: the agent of a key, whose claim
// the tester's phases row names while it runs the plan's step, and the tester's own claim of an
// earlier version of the plan. The state names every claim of the issue by its token: the step's
// worker under `workers.tester`, with its token, and the other claim of the role under
// `otherClaims`, with its role and its token, so a reader of the state can reach either.
func TestTheStateNamesEveryLiveClaimOfAnIssueByItsToken(t *testing.T) {
	const project = "LEGION"
	keyed, err := claim.AgentToken("legion", "LEGION-20", "implementer")
	if err != nil {
		t.Fatal(err)
	}
	own := realToken("LEGION-20", claim.RoleTester)
	store := projectionStore{
		issues: []record.Issue{{Key: "LEGION-20", Project: project, Tree: "LEGION-20", Phase: phase.Testing, Generation: 1,
			Status: "testing", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Rank: "a"}},
		phases: map[string][]record.PhaseRow{
			"LEGION-20": {{Issue: "LEGION-20", Role: claim.RoleTester, Claim: keyed, Entry: 2, StepTask: "task-2"}},
		},
	}
	claims := []supervise.Claim{
		{Token: own, Issue: "LEGION-20", Tree: "LEGION-20", Role: claim.RoleTester, State: supervise.StateIdle, Session: "ses_own"},
		{Token: keyed, Issue: "LEGION-20", Tree: "LEGION-20", Role: claim.RoleTester, State: supervise.StateWorking, Session: "ses_keyed"},
	}
	got, err := Project(context.Background(), Inputs{Store: store, Project: project, Claims: claims, Now: projectedAt})
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	view := got.Issues["LEGION-20"]
	if worker := view.Workers[claim.RoleTester].Claim; worker.Token != keyed || worker.Session != "ses_keyed" {
		t.Fatalf("workers.tester = token %q session %q, want the step's worker %s", worker.Token, worker.Session, keyed)
	}
	if len(view.OtherClaims) != 1 || view.OtherClaims[0].Role != claim.RoleTester || view.OtherClaims[0].Claim.Token != own ||
		view.OtherClaims[0].Claim.Session != "ses_own" {
		t.Fatalf("otherClaims = %+v, want the tester's own claim %s, which works no step", view.OtherClaims, own)
	}
}

// The claim the state shows for a role under `workers` is the claim the issue and the role reach:
// the claim record.WorkerClaim gives over the issue's phases rows, which fresh_session and compact
// act on when a call names the issue and the role and no claim (api's workerToken). Every other
// claim of the issue is under `otherClaims`, which a reader reaches by its token. The role's row
// names another claim than that one in two windows, and in each the row's claim is not shown as
// the role's:
//   - A new generation of the issue keeps each row's claim and clears its step
//     (record.Store.ClearGeneration): here a root re-admitted past its implementer steps, whose
//     plan was `implementer reviewer implementer`, so its implementer's row keeps
//     `…-implementer-2`, the agent of the role's last step, and the issue and the role reach the
//     role's own claim.
//   - A step's entry writes its agent's claim on the row before the start makes that claim: the
//     issue and the role reach `…-implementer-2`, which the state does not hold yet, so no claim of
//     the role is shown under `workers`, and the role's own claim is another claim of the role.
func TestTheStateShowsForARoleTheClaimItsIssueAndRoleReach(t *testing.T) {
	const project, key = "LEGION", "LEGION-20"
	own := realToken(key, claim.RoleImplementer)
	second, err := claim.AgentToken("legion", key, "implementer-2")
	if err != nil {
		t.Fatal(err)
	}
	ownClaim := supervise.Claim{Token: own, Issue: key, Tree: key, Role: claim.RoleImplementer, State: supervise.StateIdle, Session: "ses_own"}
	secondClaim := supervise.Claim{Token: second, Issue: key, Tree: key, Role: claim.RoleImplementer, State: supervise.StateIdle, Session: "ses_second"}
	// Each case's reached is the claim the issue and the role reach (record.WorkerClaim), shown the
	// claim `workers.implementer` shows, "" for none, and others the claims under `otherClaims`.
	for _, tc := range []struct {
		name           string
		row            record.PhaseRow
		claims         []supervise.Claim
		reached, shown claim.Token
		others         []claim.Token
	}{
		{
			name:    "a new generation cleared the step of the row a second agent worked last",
			row:     record.PhaseRow{Issue: key, Role: claim.RoleImplementer, Claim: second, Entry: 3},
			claims:  []supervise.Claim{ownClaim, secondClaim},
			reached: own, shown: own, others: []claim.Token{second},
		},
		{
			name:    "a step's entry names a second agent the start has not made yet",
			row:     record.PhaseRow{Issue: key, Role: claim.RoleImplementer, Claim: second, Entry: 3, StepTask: "task-1", StepPlace: 3},
			claims:  []supervise.Claim{ownClaim},
			reached: second, shown: "", others: []claim.Token{own},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows := []record.PhaseRow{tc.row}
			// The window is the one its name says: the issue and the role reach the claim it names.
			if reached, err := record.WorkerClaim("legion", key, claim.RoleImplementer, rows); err != nil || reached != tc.reached {
				t.Fatalf("record.WorkerClaim = %s, %v; want %s: the fixture is not the window it names", reached, err, tc.reached)
			}
			store := projectionStore{
				issues: []record.Issue{{Key: key, Project: project, Tree: key, Phase: phase.Reviewing, Generation: 2,
					Status: "needs_review", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Rank: "a"}},
				phases: map[string][]record.PhaseRow{key: rows},
			}
			got, err := Project(context.Background(), Inputs{Store: store, Project: project, Claims: tc.claims, Now: projectedAt})
			if err != nil {
				t.Fatalf("Project: %v", err)
			}
			view := got.Issues[key]
			if worker, shown := view.Workers[claim.RoleImplementer]; worker.Claim.Token != tc.shown || shown != (tc.shown != "") {
				t.Errorf("workers.implementer = %q (shown %t), want %q: issue and role reach %s", worker.Claim.Token, shown, tc.shown, tc.reached)
			}
			var others []claim.Token
			for _, other := range view.OtherClaims {
				if other.Role != claim.RoleImplementer {
					t.Errorf("otherClaims holds %s of the role %s, want only implementer claims", other.Claim.Token, other.Role)
				}
				others = append(others, other.Claim.Token)
			}
			if !slices.Equal(others, tc.others) {
				t.Errorf("otherClaims = %v, want %v: each claim the issue and the role do not reach", others, tc.others)
			}
		})
	}
}

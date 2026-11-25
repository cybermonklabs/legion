package workflow

import (
	"context"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// childStandsAt is LEGION-209, a child of the tree root LEGION-208, standing in at at step place
// of the plan whose steps are words, with the type of each role in types stored for it
// (seedStepsAt), with each step's phases row naming the claim rows gives its place (rows[0] for
// step 1, and so on), and its pull request #43 open at head-43, with no merge forward asked yet.
func childStandsAt(t *testing.T, at phase.Phase, place int, types map[string]string, words []string, rows []claim.Token) *pgxpool.Pool {
	t.Helper()
	pool := migratedPool(t)
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Implementing, Generation: 4, Status: "in_progress", Rank: "U",
		LastDispatchSeq: 1})
	seedSlot(t, pool, record.Slot{Issue: "LEGION-208", Index: 0})
	seedIssue(t, pool, record.Issue{Key: "LEGION-209", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: "LEGION-208", Project: "LEGION", Title: "LEGION-209", Parent: new("LEGION-208"), Phase: at, Generation: 4,
		Status: "in_progress", Rank: "V", LastDispatchSeq: 1})
	seedStepsAt(t, pool, "LEGION-209", 4, place, types, words...)
	for i, role := range standingSteps(words) {
		seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-209", Role: role, Claim: rows[i], StepTask: "a", StepPlace: i + 1, Entry: 1})
	}
	seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-209", Repo: "sjawhar/legion", Number: 43,
		Branch: "legion/LEGION-209", Base: "main", HeadSHA: "head-43", Failing: []string{}})
	return pool
}

// asksThenLands asks LEGION-209's merge forward (mergeForward) and then lands it, with a
// synchronize that moves its pull request off head-43.
func asksThenLands(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	engine := returnEngine()
	onStored(t, pool, engine, "LEGION-209", func(ctx context.Context, tx pgx.Tx, issue record.Issue) error {
		stored, err := engine.store.PullRequest(ctx, tx, issue.Key)
		if err != nil {
			return err
		}
		w, err := engine.walk(ctx, tx, issue)
		if err != nil {
			return err
		}
		return engine.mergeForward(ctx, tx, issue, w, stored, record.ConflictCheckNotGreen)
	})
	applyRefusingNothing(t, pool, engine, intake.PullRequestSynchronized{Repo: "sjawhar/legion", Number: 43, Branch: "legion/LEGION-209",
		Base: "main", HeadSHA: "head-forward", Before: "head-43", Sender: "legion-implementer[bot]"})
}

// The owner does a step of its own plan whatever the step's phases row names: its owner-step and
// its report are the owner's (record.OwnerClaim). The row can name the architect above, which the
// start writes there while the issue's own architect holds no role (ownerAgent). A message to the
// worker of that step reaches the owner too. LEGION-209, a child at the reviewer's step of
// `implementer reviewer:owner`, whose reviewer row names LEGION-208's architect: the merge forward
// holds LEGION-209's own architect, and the landing tells it.
//
// The control: the same child whose reviewer row names its own architect, as a start writes it
// while that architect holds its role.
func TestAMessageToTheOwnersStepReachesTheOwnerWhateverItsRowNames(t *testing.T) {
	childOwner := claim.Token(treeRoleToken(t, "LEGION-209", claim.RoleArchitect))
	above := claim.Token(treeRoleToken(t, "LEGION-208", claim.RoleArchitect))
	implementer := claim.Token(treeRoleToken(t, "LEGION-209", claim.RoleImplementer))
	for _, tc := range []struct {
		name string
		row  claim.Token
	}{
		{name: "the row names the architect above", row: above},
		{name: "control: the row names the owner", row: childOwner},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := childStandsAt(t, phase.Reviewing, 2, nil, []string{"implementer", "reviewer:owner"}, []claim.Token{implementer, tc.row})
			asksThenLands(t, pool)
			var held, landed []claim.Token
			for _, message := range mergeForwardMessages(t, pool) {
				switch message.Step {
				case record.MergeForwardHold:
					held = append(held, message.Claim)
				case record.MergeForwardLanded:
					landed = append(landed, message.Claim)
				}
			}
			if len(held) != 1 || held[0] != childOwner || len(landed) != 1 || landed[0] != childOwner {
				t.Fatalf("the hold reached %v and the landed %v, want each to reach the owner %s alone", held, landed, childOwner)
			}
		})
	}
}

// The review of a merge that lands past review goes to the agent of the task's reviewer step
// (workAgent), the owner where that step's key is owner, whatever the reviewer's phases row names:
// the row can name the architect above (ownerAgent), which does not do the step. LEGION-209, a
// child at the owner's acceptance of `implementer reviewer:owner acceptance:owner`, whose reviewer
// and acceptance rows name LEGION-208's architect: the landing sends one review, to LEGION-209's own
// architect, which covers the landed, since that architect is also the worker of the acceptance
// step (record.Issue.StepWorker). No landed goes, and the record keeps that the review was asked
// (record.MergeForward.ReviewAsked), which the merger waits on.
//
// The control: the same child whose rows name its own architect, which gets the same one review.
// It shows the review reaches the owner where the reviewer's row names it, so the case above fails
// only where the review follows the row to the architect above.
func TestTheReviewOfAMergeThatLandsAtTheOwnersAcceptanceReachesTheOwnerWhateverItsRowNames(t *testing.T) {
	childOwner := claim.Token(treeRoleToken(t, "LEGION-209", claim.RoleArchitect))
	above := claim.Token(treeRoleToken(t, "LEGION-208", claim.RoleArchitect))
	implementer := claim.Token(treeRoleToken(t, "LEGION-209", claim.RoleImplementer))
	for _, tc := range []struct {
		name string
		row  claim.Token
	}{
		{name: "the rows name the architect above", row: above},
		{name: "control: the rows name the owner", row: childOwner},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := childStandsAt(t, phase.Accepting, 3, nil, []string{"implementer", "reviewer:owner", "acceptance:owner"},
				[]claim.Token{implementer, tc.row, tc.row})
			asksThenLands(t, pool)
			var landing []sent
			for _, message := range sentOf(mergeForwardMessages(t, pool)) {
				if message.step == record.MergeForwardReview || message.step == record.MergeForwardLanded {
					landing = append(landing, message)
				}
			}
			want := []sent{{step: record.MergeForwardReview, role: claim.RoleReviewer, claim: childOwner, covers: record.MergeForwardLanded}}
			if !reflect.DeepEqual(landing, want) {
				t.Fatalf("the landing sent %+v, want %+v", landing, want)
			}
			if landed := recordedPR(t, pool, "LEGION-209").MergeForward; landed == nil || landed.Landed != "head-forward" || !landed.ReviewAsked {
				t.Fatalf("the record holds the merge forward %+v, want it landed at head-forward with the review asked", landed)
			}
		})
	}
}

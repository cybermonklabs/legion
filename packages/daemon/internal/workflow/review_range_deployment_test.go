package workflow

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/reviewround"
)

const (
	// stepCommitW is the commit a round of the generation before a reset read.
	stepCommitW = "1111111111111111111111111111111111111111"
	// stepCommitX is the commit the critic's first round reads, and the pull request's head.
	stepCommitX = "2222222222222222222222222222222222222222"
	// stepCommitY is the commit the implementer pushes after the critic's fail.
	stepCommitY = "3333333333333333333333333333333333333333"
	// resetEntry is the entry the critic's row stands on before the reset.
	resetEntry int64 = 4
)

// reportsAtEntry applies role's completion of key at generation with verdict and commit, carrying
// entry as the route carries the entry the claim serves, under id, and fails the test on a
// refusal. An implementer's completion names the handoff record id.
func reportsAtEntry(t *testing.T, pool *pgxpool.Pool, engine *Engine, id, key string, generation uint64, role claim.Role, verdict, commit string, entry int64) {
	t.Helper()
	fact := intake.HandoffComplete{Generation: generation, Issue: key, Role: role, Claim: workerOr(t, pool, key, role, claim.Token(string(role)+"-claim")), Summary: "reported",
		Commit: commit, Verdict: verdict, Entry: entry}
	if role == claim.RoleImplementer {
		fact.HandoffRecord, fact.HandoffVersion = id, 1
	}
	if result := applyFacts(t, pool, engine)(id, fact); result.Refusal != nil {
		t.Fatalf("the %s's completion %s of %s was refused: %+v", role, id, key, result.Refusal)
	}
}

// stepReviewedHead is the commit the newest run of role's review step of key read, as role's
// phases row holds it (phases.reviewed_head, which recordStepReview writes): a role runs at most one
// step of an issue.
func stepReviewedHead(t *testing.T, pool *pgxpool.Pool, key string, role claim.Role) string {
	t.Helper()
	var head string
	if err := pool.QueryRow(t.Context(), `select reviewed_head from phases where issue = $1 and role = $2`, key, string(role)).Scan(&head); err != nil {
		t.Fatalf("read the reviewed commit of the %s's step of %s: %v", role, key, err)
	}
	return head
}

// A step of the type review whose role the deployment defines gets the commit its run before read,
// as the reviewer does (v-review-range for this role): the critic fails at X, its completion
// carrying the entry its row stands on, the implementer pushes Y, and the critic's second task
// names X; its first names none. The checker, of the type testing, in the same plan form, is told
// no commit. The control, which stays in the suite: with the commit the critic's phases row holds
// cleared between the rounds, the critic's second task names none, so the commit comes from that
// row alone.
func TestAReviewStepOfADeploymentRoleGetsTheCommitItsRunBeforeRead(t *testing.T) {
	for _, tc := range []struct {
		name    string
		role    claim.Role
		typ     string
		cleared bool
		names   bool
	}{
		{"the critic, of the type review", "critic", "review", false, true},
		{"the checker, of the type testing", "checker", "testing", false, false},
		{"the control: the critic with its step's commit cleared between the rounds", "critic", "review", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			engine := deploymentEngine()
			root := stepRoot(t, pool, phase.Admitted)
			seedStepsPlan(t, pool, root.Key, root.Generation, "implementer", string(tc.role))
			if _, err := pool.Exec(t.Context(), "update plan_steps set type = $2 where issue = $1 and task = 'a' and position = 2", root.Key, tc.typ); err != nil {
				t.Fatalf("store the type of %s: %v", tc.role, err)
			}
			move(t, pool, engine, root.Key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
			reportsAtEntry(t, pool, engine, "impl-1", root.Key, root.Generation, claim.RoleImplementer, "", stepCommitW, 0)
			first := roleRowOf(t, pool, engine, root.Key, tc.role)
			reportsAtEntry(t, pool, engine, "fail-1", root.Key, root.Generation, tc.role, "fail", stepCommitX, first.Entry)
			if tc.cleared {
				if _, err := pool.Exec(t.Context(), "update phases set reviewed_head = '' where issue = $1 and role = $2", root.Key, string(tc.role)); err != nil {
					t.Fatalf("clear the step's commit: %v", err)
				}
			}
			reportsAtEntry(t, pool, engine, "impl-2", root.Key, root.Generation, claim.RoleImplementer, "", stepCommitY, 0)

			// Each task is the one the role's row holds for its entry (EntryTask), which each start
			// writes beside the entry it raises.
			second := roleRowOf(t, pool, engine, root.Key, tc.role)
			if second.Entry <= first.Entry {
				t.Fatalf("the %s stands on entry %d after the implementer's second completion, want one above %d", tc.role, second.Entry, first.Entry)
			}
			if strings.Contains(first.EntryTask, reviewround.CommitLabel) {
				t.Fatalf("the %s's first task = %q, want no %s", tc.role, first.EntryTask, reviewround.CommitLabel)
			}
			says := reviewedPartOf(stepCommitX)
			if got := strings.Contains(second.EntryTask, says); got != tc.names {
				t.Fatalf("the %s's second task = %q; saying %q is %t, want %t", tc.role, second.EntryTask, says, got, tc.names)
			}
			if !tc.names && strings.Contains(second.EntryTask, reviewround.CommitLabel) {
				t.Fatalf("the %s's second task = %q, want no %s", tc.role, second.EntryTask, reviewround.CommitLabel)
			}
		})
	}
}

// resetCritic seeds the root LEGION-207, whose tree runs, and its child LEGION-208 at step 2 of
// `implementer critic`, the critic (review) running on entry resetEntry with its task and the
// reviewed commit W on its phases row (a round of the generation before read it), and the pull
// request at head X; then clears the root's tree's generation (record.Store.ClearTreeGeneration),
// and fails unless the critic's row holds no commit right after the clear.
func resetCritic(t *testing.T) (*pgxpool.Pool, *Engine, record.Issue) {
	t.Helper()
	pool := migratedPool(t)
	liveRootOf(t, pool, "LEGION-207", 0)
	parent := "LEGION-207"
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: parent,
		Parent: &parent, Project: "LEGION", Title: "child", Phase: phase.Step, Generation: 1, Status: "in_progress", Rank: "U", LastDispatchSeq: 1})
	seedStepsAt(t, pool, "LEGION-208", 1, 2, map[string]string{"critic": "review"}, "implementer", "critic")
	seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion", Number: 43,
		Branch: "legion/LEGION-208", HeadSHA: stepCommitX})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: "critic", Claim: "critic-claim", Entry: resetEntry,
		EntryTask: "Continue child. Issue: LEGION-208. Phase: step.", ReviewedHead: stepCommitW})
	seedRecord(t, pool, func(tx pgx.Tx) error {
		return record.NewStore().ClearTreeGeneration(t.Context(), tx, "LEGION-207", time.Now().UTC())
	})
	if head := stepReviewedHead(t, pool, "LEGION-208", "critic"); head != "" {
		t.Fatalf("the critic's row holds the commit %q right after the clear of its tree's generation, want none", head)
	}
	return pool, deploymentEngine(), *issueRecord(t, pool, "LEGION-208")
}

// Only a completion of the entry that stands, on a row that holds that entry's task, moves a review
// step's commit: a completion the route read before the reset of the tree's generation and
// the engine applies after it writes no commit of the generation before into the new one. With the
// promotion (StartPromotedWorker raises the critic's row to the next entry, whose task names no
// commit), a completion of the entry before the reset is refused HANDOFF_STALE_ENTRY under the
// fact's lock, since step ends with no handoff record and so nothing but its entry says which
// entry's work it is (intake.BehindEntryClause): it moves nothing, and the step's commit stays empty. A
// completion of no entry is judged by nothing of an entry: it moves the step and leaves its commit
// empty, and the critic's next task names none; the control, a completion of the promoted entry,
// writes X, which that task names. Without the promotion (the root waits in the line), the row
// stands on the entry before the reset with no task, and a completion of that entry writes none.
func TestACompletionOfTheEntryBeforeAResetMovesNoStepCommit(t *testing.T) {
	for _, tc := range []struct {
		name     string
		promoted bool
		entry    int64
		refused  bool
		want     string
	}{
		{"the entry before the reset, after the promotion", true, resetEntry, true, ""},
		{"no entry, after the promotion", true, 0, false, ""},
		{"the control: the promoted entry", true, resetEntry + 1, false, stepCommitX},
		{"the entry before the reset, with no promotion", false, resetEntry, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, engine, child := resetCritic(t)
			if tc.promoted {
				applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
					s, err := firstScope(ctx, tx, engine, child)
					if err != nil {
						return err
					}
					return engine.StartPromotedWorker(ctx, tx, s, "critic")
				})
				row := roleRowOf(t, pool, engine, child.Key, "critic")
				if row.Entry != resetEntry+1 || strings.Contains(row.EntryTask, reviewround.CommitLabel) {
					t.Fatalf("the promoted critic is on entry %d with the task %q, want entry %d naming no %s", row.Entry, row.EntryTask, resetEntry+1, reviewround.CommitLabel)
				}
			}

			if tc.refused {
				result := applyFacts(t, pool, engine)("critic-fail", intake.HandoffComplete{Generation: child.Generation, Issue: child.Key, Role: "critic",
					Claim: workerOr(t, pool, child.Key, "critic", "critic-claim"), Summary: "reported", Commit: stepCommitX, Verdict: "fail", Entry: tc.entry})
				got := issueOf(t, pool, engine, child.Key)
				if refusalCode(result) != "HANDOFF_STALE_ENTRY" || got.Phase != phase.Step || stepRoleOf(t, pool, engine, child.Key) != "critic" {
					t.Fatalf("the critic's fail of entry %d with entry %d standing: refusal %+v, %s stands in %s at the %q step; want HANDOFF_STALE_ENTRY and the critic's step",
						tc.entry, resetEntry+1, result.Refusal, child.Key, got.Phase, stepRoleOf(t, pool, engine, child.Key))
				}
				if head := stepReviewedHead(t, pool, child.Key, "critic"); head != "" {
					t.Fatalf("the critic's step holds the commit %q after the refused completion of entry %d, want none", head, tc.entry)
				}
				return
			}
			reportsAtEntry(t, pool, engine, "critic-fail", child.Key, child.Generation, "critic", "fail", stepCommitX, tc.entry)

			if head := stepReviewedHead(t, pool, child.Key, "critic"); head != tc.want {
				t.Fatalf("the critic's step holds the commit %q after its completion of entry %d, want %q", head, tc.entry, tc.want)
			}
			reportsAtEntry(t, pool, engine, "impl-after", child.Key, child.Generation, claim.RoleImplementer, "", stepCommitY, 0)
			next := roleRowOf(t, pool, engine, child.Key, "critic").EntryTask
			if named := strings.Contains(next, reviewround.CommitLabel); named != (tc.want != "") {
				t.Fatalf("the critic's next task = %q; naming a %s is %t, want %t", next, reviewround.CommitLabel, named, tc.want != "")
			}
		})
	}
}

// A review step of a deployment role that is resumed, not started by a move, is told the commit its
// step read too, as a resumed reviewer is (namingTheReviewRound reads the role's phases row): the
// ready of the critic's claim at step 2 of `implementer critic`, whose row holds X, sends a resume
// task naming X; the same ready with the row holding none names no commit.
func TestAResumedReviewStepIsToldTheCommitItsStepRead(t *testing.T) {
	for _, tc := range []struct {
		name string
		head string
	}{{"the row holds X", stepCommitX}, {"the row holds none", ""}} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			const key = "LEGION-209"
			seedIssue(t, pool, record.Issue{Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: key,
				Project: "LEGION", Title: "a root", Phase: phase.Step, Generation: 1, Status: "in_progress", Rank: "U", HandedOver: true})
			seedStepsAt(t, pool, key, 1, 2, map[string]string{"critic": "review"}, "implementer", "critic")
			seedPhase(t, pool, record.PhaseRow{Issue: key, Role: "critic", ReviewedHead: tc.head})
			seedSlot(t, pool, record.Slot{Issue: key, Index: 0})
			seedPhaseWorker(t, pool, key, "critic", "")

			if _, err := intake.ApplyFact(t.Context(), pool, "supervise", "supervise:ready", intake.ClaimReady{Issue: key, Role: "critic", Launch: 2},
				withDeployment(testEngine(config.DesignGateRootIssues, nil)), admissionStub{}); err != nil {
				t.Fatalf("apply the ready: %v", err)
			}
			starts := superviseRowsOf(t, pool, key, "start")
			if len(starts) != 1 {
				t.Fatalf("the ready queued starts %+v, want the critic's one", starts)
			}
			says := reviewedPartOf(stepCommitX)
			if named := strings.Contains(starts[0].Task, says); named != (tc.head != "") {
				t.Fatalf("the critic's resume task = %q; saying %q is %t, want %t", starts[0].Task, says, named, tc.head != "")
			}
		})
	}
}

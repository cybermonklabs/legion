package workflow

import (
	"bytes"
	"context"
	"log/slog"
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
// phases row holds it (phases.reviewed_head, which recordStepReview writes): role stands at one
// step of each plan here, so it has one row.
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
// no commit. A completion whose commit is not spelled as a head is (reviewround.IsHead), a short
// SHA here, writes no commit, as a review footer's head of that spelling names none
// (intake.PullRequestReview.LegionHead): the critic's second task names none, and the daemon logs
// one line that names the issue, the role, the step and the commit it refused, so an operator sees
// why the next run reads the whole change; no other row logs one. The control, which stays in the
// suite: with the commit the critic's phases row holds cleared between the rounds, the critic's
// second task names none, so the commit comes from that row alone.
func TestAReviewStepOfADeploymentRoleGetsTheCommitItsRunBeforeRead(t *testing.T) {
	for _, tc := range []struct {
		name    string
		role    claim.Role
		typ     string
		read    string
		cleared bool
		names   bool
		refused bool
	}{
		{name: "the critic, of the type review", role: "critic", typ: "review", read: stepCommitX, names: true},
		{name: "the checker, of the type testing", role: "checker", typ: "testing", read: stepCommitX},
		{name: "the critic, whose completion names a short SHA", role: "critic", typ: "review", read: stepCommitX[:7], refused: true},
		{name: "the control: the critic with its step's commit cleared between the rounds", role: "critic", typ: "review", read: stepCommitX, cleared: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			engine := deploymentEngine()
			var logged bytes.Buffer
			engine.log = slog.New(slog.NewJSONHandler(&logged, nil))
			root := stepRoot(t, pool, phase.Admitted)
			seedStepsPlan(t, pool, root.Key, root.Generation, "implementer", string(tc.role))
			if _, err := pool.Exec(t.Context(), "update plan_steps set type = $2 where issue = $1 and task = 'a' and position = 2", root.Key, tc.typ); err != nil {
				t.Fatalf("store the type of %s: %v", tc.role, err)
			}
			move(t, pool, engine, root.Key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
			reportsAtEntry(t, pool, engine, "impl-1", root.Key, root.Generation, claim.RoleImplementer, "", stepCommitW, 0)
			first := roleRowOf(t, pool, engine, root.Key, tc.role)
			reportsAtEntry(t, pool, engine, "fail-1", root.Key, root.Generation, tc.role, "fail", tc.read, first.Entry)
			refusals := jsonLogLines(&logged, func(entry map[string]any) bool {
				msg, _ := entry["msg"].(string)
				return strings.HasPrefix(msg, stepCommitStaysLog)
			})
			if !tc.refused && len(refusals) != 0 {
				t.Fatalf("the engine logged %v, want no line that the %s's completion moves no reviewed commit", refusals, tc.role)
			}
			if tc.refused && (len(refusals) != 1 || refusals[0]["issue"] != root.Key || refusals[0]["role"] != string(tc.role) ||
				refusals[0]["step"] != float64(2) || refusals[0]["commit"] != tc.read) {
				t.Fatalf("the engine logged %v, want one line that the completion moves no reviewed commit, naming %s, the %s, step 2 and %q",
					refusals, root.Key, tc.role, tc.read)
			}
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
					return engine.StartPromotedWorker(ctx, tx, child, "critic")
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
				if refusalCode(result) != "HANDOFF_STALE_ENTRY" || got.Phase != phase.Step || got.StepRole != "critic" {
					t.Fatalf("the critic's fail of entry %d with entry %d standing: refusal %+v, %s stands in %s at the %q step; want HANDOFF_STALE_ENTRY and the critic's step",
						tc.entry, resetEntry+1, result.Refusal, child.Key, got.Phase, got.StepRole)
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

// critics writes LEGION-208's plan of words under engine, each step of the role critic of the type
// review, and walks the plan into its first step, whose implementer then reports at stepCommitW.
func critics(t *testing.T, pool *pgxpool.Pool, engine *Engine, words ...string) {
	t.Helper()
	root := stepRoot(t, pool, phase.Admitted)
	seedStepsPlan(t, pool, root.Key, root.Generation, words...)
	if _, err := pool.Exec(t.Context(), `update plan_steps set type = case role when 'critic' then 'review' else 'testing' end
		where issue = $1 and task = 'a' and role in ('critic', 'checker')`, root.Key); err != nil {
		t.Fatalf("store the types of the critic and the checker: %v", err)
	}
	move(t, pool, engine, root.Key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
	reportsAtEntry(t, pool, engine, "impl-1", root.Key, root.Generation, claim.RoleImplementer, "", stepCommitW, 0)
}

// criticReportsAt is the critic's report of the step it runs on LEGION-208 now, under id with
// verdict, at commit, of the entry its row of that step stands on, place: a report of the entry that
// stands, which records commit as the one its review read (recordStepReview).
func criticReportsAt(t *testing.T, pool *pgxpool.Pool, engine *Engine, id string, place int, verdict, commit string) {
	t.Helper()
	row := rolesRows(t, pool, engine, "LEGION-208", "critic")[place]
	reportsAtEntry(t, pool, engine, id, "LEGION-208", 1, "critic", verdict, commit, row.Entry)
}

// A review step of a deployment role is named the commits a reviewer step is, by the same lookup
// of the step's agent: in `implementer:a critic critic:a`, step 3 is agent a's first critic step
// after the critic's own agent reviewed at step 2, so its first task names no commit of its own and
// names step 2's commit as another agent's (reviewround.AnotherAgentsSentence); in `implementer:a
// critic:a critic critic:a`, step 4 is agent a's second critic step, past the critic's own agent's
// step 3, and its first task names agent a's own commit of step 2. Before, step 3 named no commit at
// all, and step 4's first run cleared the commit it took from step 3's row as another agent's.
func TestAReviewStepOfADeploymentRoleIsNamedItsAgentsCommitOrAnotherAgentsBesideNone(t *testing.T) {
	for _, tc := range []struct {
		name  string
		words []string
		// at is the step whose first task is read; own is whether it names stepCommitX as its own
		// commit, and otherwise it names stepCommitX as another agent's.
		at  int
		own bool
	}{
		{"another agent's first critic step", []string{"implementer:a", "critic", "critic:a"}, 3, false},
		{"an agent's later critic step past another agent's", []string{"implementer:a", "critic:a", "critic", "critic:a"}, 4, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			engine := deploymentEngine()
			critics(t, pool, engine, tc.words...)
			criticReportsAt(t, pool, engine, "pass-2", 2, "pass", stepCommitX)
			if tc.at == 4 {
				criticReportsAt(t, pool, engine, "pass-3", 3, "pass", stepCommitY)
			}
			assertWalksAt(t, pool, engine, "LEGION-208", phase.Step, stepPlace(tc.at))
			row := rolesRows(t, pool, engine, "LEGION-208", "critic")[tc.at]
			own, other := reviewedPartOf(stepCommitX), reviewround.AnotherAgentsSentence(stepCommitX)
			switch {
			case tc.own && (row.ReviewedHead != stepCommitX || !strings.HasSuffix(row.EntryTask, own) || strings.Contains(row.EntryTask, reviewround.AnotherAgentLabel)):
				t.Fatalf("step %d's first task = %q with the commit %q, want it to end %q, its agent's own, and name no other agent's", tc.at, row.EntryTask, row.ReviewedHead, own)
			case !tc.own && (row.ReviewedHead != "" || !strings.HasSuffix(row.EntryTask, other) || strings.Contains(row.EntryTask, reviewround.CommitLabel)):
				t.Fatalf("step %d's first task = %q with the commit %q, want no commit of its own and a task ending %q", tc.at, row.EntryTask, row.ReviewedHead, other)
			}
		})
	}
}

// A deployment role's review step is named its agent's newest commit on the task, at whichever of
// its steps it was read, as a reviewer step is: in `implementer:a critic:c checker critic:c`, key
// c's agent passes step 2 at X and fails step 4 at Y, the task returns to step 1, and step 2's next
// task names Y, each row of key c's agent holding it. Before, step 2's row kept X, its own run's.
func TestADeploymentReviewStepIsNamedItsAgentsNewestCommitOfAnotherStep(t *testing.T) {
	pool := migratedPool(t)
	engine := deploymentEngine()
	critics(t, pool, engine, "implementer:a", "critic:c", "checker", "critic:c")
	criticReportsAt(t, pool, engine, "pass-2", 2, "pass", stepCommitX)
	reportsAtEntry(t, pool, engine, "check-3", "LEGION-208", 1, "checker", "pass", stepCommitX, 0)
	assertWalksAt(t, pool, engine, "LEGION-208", phase.Step, stepPlace(4))
	criticReportsAt(t, pool, engine, "fail-4", 4, "fail", stepCommitY)
	assertWalksAt(t, pool, engine, "LEGION-208", phase.Implementing, stepPlace(1))
	agentHolds(t, pool, engine, "critic", agentToken(t, "critic"), stepCommitY, "after step 4's fail")
	reportsAtEntry(t, pool, engine, "impl-2", "LEGION-208", 1, claim.RoleImplementer, "", stepCommitY, 0)
	assertWalksAt(t, pool, engine, "LEGION-208", phase.Step, stepPlace(2))
	two := rolesRows(t, pool, engine, "LEGION-208", "critic")[2]
	if says := reviewedPartOf(stepCommitY); !strings.HasSuffix(two.EntryTask, says) {
		t.Fatalf("step 2's next task = %q, want it to end %q, key c's agent's newest commit, step 4's", two.EntryTask, says)
	}
}

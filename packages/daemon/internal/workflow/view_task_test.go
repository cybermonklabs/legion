package workflow

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// viewTaskSentence is the one sentence every task of a phase worker of LEGION-208 carries while its
// spec is a view on LEGION-100's requirements (dispatch://INFRA-3320, R4).
const viewTaskSentence = "LEGION-208 is a view on LEGION-100: the requirements of LEGION-208 are the parts of the requirements of LEGION-100 " +
	"that the Delivers: line in the spec of LEGION-208 names."

// assertViewSentence fails the test unless task carries viewTaskSentence exactly once where view
// says the issue is a view, and not at all where it is not.
func assertViewSentence(t *testing.T, what, task string, view bool) {
	t.Helper()
	want := 0
	if view {
		want = 1
	}
	if got := strings.Count(task, viewTaskSentence); got != want {
		t.Fatalf("%s = %q carries the view sentence %d times, want %d", what, task, got, want)
	}
}

// phaseStartTask is the task of the one start of role in at the outbox holds for LEGION-208,
// which the role's phases row stores whole as its entry's work (enter), so a resent entry carries
// it too.
func phaseStartTask(t *testing.T, pool *pgxpool.Pool, role claim.Role, at phase.Phase) string {
	t.Helper()
	var tasks []string
	for _, start := range superviseRowsOf(t, pool, gatedRoot, "start") {
		if start.Role == role && start.Phase == at {
			tasks = append(tasks, start.Task)
		}
	}
	if len(tasks) != 1 {
		t.Fatalf("the starts of the %s in %s carry %q, want one", role, at, tasks)
	}
	if row := entryRow(t, pool, role); row.EntryTask != tasks[0] {
		t.Fatalf("the %s's phases row stores %q as its entry's work, want the start's task %q", role, row.EntryTask, tasks[0])
	}
	return tasks[0]
}

// The task the daemon gives each phase worker of a view says in one sentence that the issue's
// requirements are the parts of the ancestor's requirements its Delivers: line names, whether an
// ancestor's approval or a person's approval of the view's own spec opened its step: the
// planner's, the implementer's, the tester's and the reviewer's, each as its phase starts, and the
// resume of a phase under way. The task is built from the record's view fact alone, so an issue
// whose registration recorded none carries no such sentence.
func TestEveryPhaseTaskOfAViewNamesTheAncestorsRequirements(t *testing.T) {
	alice := dispatch.Actor{Kind: "user", ID: "alice"}
	for _, tc := range []struct {
		name     string
		register intake.GateRegistered
		view     bool
	}{
		{name: "a view opened on the ancestor's approval", register: onBasis(approvedBasis(), 6), view: true},
		{name: "a view opened on a person's approval of its own spec", view: true,
			register: intake.GateRegistered{Issue: gatedRoot, ArtifactID: gateDoc, Version: 4, ApprovedVersion: 4, ReviewedBy: alice, ViewOf: basisAncestor}},
		{name: "no view", register: intake.GateRegistered{Issue: gatedRoot, ArtifactID: gateDoc, Version: 4, ApprovedVersion: 4, ReviewedBy: alice}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			engine := testEngine(config.DesignGateRootIssues, nil)
			apply := applyFacts(t, pool, engine)
			seedGatedTree(t, pool)
			seedLiveAncestor(t, pool)
			complete := func(id string, fact intake.HandoffComplete) {
				t.Helper()
				if result := apply(id, fact); result.Refusal != nil {
					t.Fatalf("the %s's completion was refused: %+v", fact.Role, *result.Refusal)
				}
			}

			apply("register", tc.register)
			admissionPass(t, pool, engine)
			if got := phaseOf(t, pool, gatedRoot); got != phase.Planning {
				t.Fatalf("the root = %s after admission's pass, want planning", got)
			}
			planner := phaseStartTask(t, pool, claim.RolePlanner, phase.Planning)
			want := "Continue root. Issue: LEGION-208. Phase: planning."
			if tc.view {
				want += " " + viewTaskSentence
			}
			if planner != want {
				t.Fatalf("the planner's task = %q, want %q", planner, want)
			}
			if got := ResumePhaseTask(recordedRoot(t, pool)); !strings.HasPrefix(got, want+" ") {
				t.Fatalf("the resume task of the planning phase = %q, want it to open with %q", got, want)
			}

			complete("plan", intake.HandoffComplete{Generation: 1, Issue: gatedRoot, Role: claim.RolePlanner, Claim: "plan-claim",
				HandoffRecord: "plan-1", HandoffVersion: 1})
			implementer := phaseStartTask(t, pool, claim.RoleImplementer, phase.Implementing)
			assertViewSentence(t, "the implementer's task", implementer, tc.view)

			seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: gatedRoot, Repo: "sjawhar/legion", Number: 42,
				Branch: "legion/" + gatedRoot, HeadSHA: "head", CheckedHead: "head", Failing: []string{}})
			complete("implement", intake.HandoffComplete{Generation: 1, Issue: gatedRoot, Role: claim.RoleImplementer, Claim: "implement-claim",
				Commit: "fix-1", HandoffRecord: "implement-1", HandoffVersion: 1})
			tester := phaseStartTask(t, pool, claim.RoleTester, phase.Testing)
			assertViewSentence(t, "the tester's task", tester, tc.view)

			complete("test", intake.HandoffComplete{Generation: 1, Issue: gatedRoot, Role: claim.RoleTester, Claim: "test-claim", Verdict: "pass",
				HandoffRecord: "test-1", HandoffVersion: 1})
			reviewer := phaseStartTask(t, pool, claim.RoleReviewer, phase.Reviewing)
			assertViewSentence(t, "the reviewer's task", reviewer, tc.view)
			if !strings.Contains(reviewer, "Pull request: #42.") {
				t.Fatalf("the reviewer's task = %q, want it to name the pull request as well", reviewer)
			}
		})
	}
}

// The production check's task names a view's ancestor's requirements as every phase task does, in
// the line after its first, and the check of an issue that is no view names none.
func TestTheProductionCheckTaskOfAViewNamesTheAncestorsRequirements(t *testing.T) {
	for _, tc := range []struct {
		name   string
		viewOf string
	}{
		{name: "a view", viewOf: basisAncestor},
		{name: "no view"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newCheckRig(t)
			seedIssue(t, rig.pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
				Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Merging, Generation: 7, Status: "retro", Rank: "U",
				LastDispatchSeq: 1, ViewOf: tc.viewOf})
			parent := "LEGION-208"
			seedIssue(t, rig.pool, record.Issue{Key: "LEGION-209", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
				Tree: "LEGION-208", Project: "LEGION", Title: "child", Parent: &parent, Phase: phase.Implementing, Generation: 3,
				Status: "in_progress", Rank: "V", LastDispatchSeq: 1})
			seedPR(t, rig.pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion", Number: 42,
				Branch: "legion/LEGION-208", HeadSHA: "head", Failing: []string{}})
			rig.apply("merged", intake.PullRequestMerged{Repo: "sjawhar/legion", Number: 42, MergeSHA: "merge"})
			rig.apply("root-closed", intake.DispatchIssue{Key: "LEGION-208", Seq: 2, Type: "issue.closed", Status: "done", Title: "root", Rank: "U"})

			var looks []string
			for _, start := range superviseRowsOf(t, rig.pool, "LEGION-208", "start") {
				if start.CheckLook != 0 {
					looks = append(looks, start.Task)
				}
			}
			if len(looks) != 1 {
				t.Fatalf("the check's look starts carry %q, want one", looks)
			}
			lines := strings.Split(looks[0], "\n")
			if lines[0] != checkTaskLead+"LEGION-208: root." {
				t.Fatalf("the check task opens with %q, want its lead line", lines[0])
			}
			assertViewSentence(t, "the production check's task", looks[0], tc.viewOf != "")
			if tc.viewOf != "" && lines[1] != viewTaskSentence {
				t.Fatalf("the check task's second line = %q, want the view sentence", lines[1])
			}
		})
	}
}

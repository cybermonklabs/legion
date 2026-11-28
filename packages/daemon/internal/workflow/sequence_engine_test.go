package workflow

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// sequenceEngine is testEngine with the project's default workflow and the workflows it offers
// (Config.Workflow, Config.Workflows).
func sequenceEngine(projectDefault string, offered ...string) *Engine {
	return New(record.NewStore(), Config{
		Project: "LEGION", Steps: config.DesignGateRootIssues.Steps(), ReviewRoundCap: 3, Linger: time.Hour, ProductionCheckRole: claim.RoleTester,
		ReviewAppLogin: testReviewApp, Repositories: testRepositories,
		Clock:    func() time.Time { return time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC) },
		Workflow: projectDefault, Workflows: offered,
	}, nil)
}

// sequenceIssue is LEGION-208, a root standing at at on workflow ("" for a record that names
// none).
func sequenceIssue(at phase.Phase, workflow string) record.Issue {
	return record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208",
		Project: "LEGION", Title: "root", Phase: at, Generation: 1, Status: PhaseStatus(at), Rank: "U", Workflow: workflow}
}

// startTasks is every task a start the outbox holds for issue's role carries.
func startTasks(t *testing.T, pool *pgxpool.Pool, issue string, role claim.Role) []string {
	t.Helper()
	rows, err := pool.Query(t.Context(), `select coalesce(payload->>'task', '') from outbox
		where kind = 'supervise' and issue = $1 and payload->>'op' = 'start' and payload->>'role' = $2 order by id`, issue, string(role))
	if err != nil {
		t.Fatalf("read the starts of %s's %s: %v", issue, role, err)
	}
	defer rows.Close()
	var tasks []string
	for rows.Next() {
		var task string
		if err := rows.Scan(&task); err != nil {
			t.Fatalf("scan a start task: %v", err)
		}
		tasks = append(tasks, task)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the starts of %s's %s: %v", issue, role, err)
	}
	return tasks
}

// A light issue's implementer hands its pull request straight to the reviewer: its completion in
// implementing, with the pull request recorded open, moves the issue to reviewing and starts the
// reviewer, and no tester is started. The same completion on the default workflow is the control:
// it moves the issue to testing and starts the tester, as every issue did before
// dispatch://INFRA-3570. The light reviewer's task names its issue's workflow, and the default
// tester's names none, as no task did before the library existed.
func TestALightIssuesImplementerHandsItsPullRequestToTheReviewer(t *testing.T) {
	for _, tc := range []struct {
		workflow     string
		want         phase.Phase
		started      claim.Role
		notStarted   claim.Role
		workflowLine string
	}{
		{workflow: "light", want: phase.Reviewing, started: claim.RoleReviewer, notStarted: claim.RoleTester,
			workflowLine: "Workflow: light, in order: implementing (implementer), reviewing (reviewer), merging (merger)."},
		{workflow: phase.DefaultSequenceName, want: phase.Testing, started: claim.RoleTester, notStarted: claim.RoleReviewer},
	} {
		t.Run(tc.workflow, func(t *testing.T) {
			pool := migratedPool(t)
			seedIssue(t, pool, sequenceIssue(phase.Implementing, tc.workflow))
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim"})
			seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion", Number: 42,
				Branch: "legion/LEGION-208", HeadSHA: headOne, Failing: []string{}})
			applyRefusingNothing(t, pool, sequenceEngine(""), intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleImplementer,
				Claim: "implement-claim", Commit: headOne, HandoffRecord: "implement-1", HandoffVersion: 1})

			assertPhase(t, pool, tc.want)
			started := superviseRequests(t, pool, "start")
			if started["LEGION-208/"+string(tc.started)] != 1 || started["LEGION-208/"+string(tc.notStarted)] != 0 {
				t.Fatalf("starts = %v, want the %s started once and no %s", started, tc.started, tc.notStarted)
			}
			tasks := startTasks(t, pool, "LEGION-208", tc.started)
			if len(tasks) != 1 {
				t.Fatalf("the %s's start tasks = %q, want one", tc.started, tasks)
			}
			if tc.workflowLine != "" && !strings.Contains(tasks[0], tc.workflowLine) {
				t.Errorf("the %s's task = %q, want it to name the issue's workflow %q", tc.started, tasks[0], tc.workflowLine)
			}
			if tc.workflowLine == "" && strings.Contains(tasks[0], "Workflow:") {
				t.Errorf("the %s's task = %q, want no workflow line on the default", tc.started, tasks[0])
			}
		})
	}
}

// A light issue's approved review hands it straight to the merger: the approval moves the issue
// from reviewing to merging and starts the merger, with no acceptance and no retro between, so the
// architect signs nothing off and no implementer runs a retro. The same approval on the default
// workflow is the control: it moves the issue to accepting, which starts nobody, the tree's
// architect being already live.
func TestALightIssuesApprovalStartsTheMerger(t *testing.T) {
	for _, tc := range []struct {
		workflow string
		want     phase.Phase
		starts   map[string]int
	}{
		{workflow: "light", want: phase.Merging, starts: map[string]int{"LEGION-208/merger": 1}},
		{workflow: phase.DefaultSequenceName, want: phase.Accepting, starts: map[string]int{}},
	} {
		t.Run(tc.workflow, func(t *testing.T) {
			pool := migratedPool(t)
			seedIssue(t, pool, sequenceIssue(phase.Reviewing, tc.workflow))
			// The reviewer has completed its round, so GitHub's approval on a green head is the
			// review's second half and ends it.
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: "review-claim", HandoffRecord: "review-1", HandoffVersion: 1})
			seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion", Number: 42,
				Branch: "legion/LEGION-208", HeadSHA: headOne, CheckedHead: headOne, Failing: []string{}})
			applyRefusingNothing(t, pool, sequenceEngine(""), intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, State: "approved",
				CommitID: headOne, HeadSHA: headOne, Author: testReviewApp, Body: reviewBody("ship it", headOne)})

			assertPhase(t, pool, tc.want)
			if started := superviseRequests(t, pool, "start"); !sameCounts(started, tc.starts) {
				t.Fatalf("starts = %v, want %v", started, tc.starts)
			}
			for _, absent := range []string{string(phase.Accepting), string(phase.Retro)} {
				if tc.want == phase.Merging && strings.Contains(strings.Join(startTasks(t, pool, "LEGION-208", claim.RoleMerger), " "), "Phase: "+absent) {
					t.Errorf("the merger's task names phase %s, which light does not walk", absent)
				}
			}
		})
	}
}

// The engine reads an issue's own workflow first, then its project's default, then the compiled
// default, and the first move writes the one it read onto the record, where every later move of
// the issue reads it. Each case starts a child's first phase from admitted as the waiting line
// does: the child's own light under a project whose default is no-planner starts implementing on
// light; a child naming none under that project starts implementing on no-planner; and a child
// naming none under a project that names no default starts planning on the compiled default.
func TestTheEngineReadsTheIssuesWorkflowThenTheProjectsThenTheDefault(t *testing.T) {
	for _, tc := range []struct {
		name, own, projectDefault, want string
		first                           phase.Phase
	}{
		{name: "the issue's own", own: "light", projectDefault: "no-planner", want: "light", first: phase.Implementing},
		{name: "the project's default", projectDefault: "no-planner", want: "no-planner", first: phase.Implementing},
		{name: "the compiled default", want: phase.DefaultSequenceName, first: phase.Planning},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedHeldTree(t, pool, record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 1, ApprovedVersion: new(1)})
			child := record.Issue{Key: "LEGION-209", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
				Tree: "LEGION-208", Project: "LEGION", Title: "LEGION-209", Parent: new("LEGION-208"), Phase: phase.Admitted,
				Generation: 4, Status: "todo", Rank: "V", LastDispatchSeq: 1, Workflow: tc.own}
			seedIssue(t, pool, child)
			startUnblockedChildren(t, pool, sequenceEngine(tc.projectDefault))

			got := storedIssue(t, pool, "LEGION-209")
			if got.Phase != tc.first || got.Workflow != tc.want {
				t.Fatalf("LEGION-209 = phase %s on workflow %q, want %s on %q", got.Phase, got.Workflow, tc.first, tc.want)
			}
			if starts := superviseRequests(t, pool, "start"); starts["LEGION-209/"+string(phase.RoleFor(tc.first))] != 1 {
				t.Fatalf("starts = %v, want LEGION-209's %s started once", starts, phase.RoleFor(tc.first))
			}
			want := "LEGION-209 starts " + string(tc.first) + " under LEGION-208: it reached the front of the waiting line and took an admission slot"
			if got := childStatusNotices(t, pool); len(got) != 1 || got[0] != want {
				t.Fatalf("child-status notices = %q, want one reading %q", got, want)
			}
		})
	}
}

// An issue whose record names a workflow keeps it whatever its project's default becomes: a light
// issue standing in implementing under a project whose default is no-planner still hands its
// pull request to the reviewer, and an issue that started before the record named a workflow,
// which ran the compiled default, still goes to testing under that project, never onto a line its
// project chose after it started.
func TestARunningIssueKeepsItsWorkflowWhateverItsProjectsDefaultBecomes(t *testing.T) {
	for _, tc := range []struct {
		name, own string
		want      phase.Phase
	}{
		{name: "a light issue", own: "light", want: phase.Reviewing},
		{name: "an issue recorded before the record named a workflow", want: phase.Testing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedIssue(t, pool, sequenceIssue(phase.Implementing, tc.own))
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim"})
			seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion", Number: 42,
				Branch: "legion/LEGION-208", HeadSHA: headOne, Failing: []string{}})
			applyRefusingNothing(t, pool, sequenceEngine("no-planner"), intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleImplementer,
				Claim: "implement-claim", Commit: headOne, HandoffRecord: "implement-1", HandoffVersion: 1})
			assertPhase(t, pool, tc.want)
		})
	}
}

// The implementer works implementing and retro on the default workflow and implementing alone on
// light, so a completion it reports after its issue moved on is of the phase it left on the
// issue's own workflow (phase.Of). Standing in merging, a light issue's implementer left
// implementing, whose completion names a handoff record: one naming none is refused
// HANDOFF_RECORD_REQUIRED for implementing, and one naming its record is recorded. The same
// completion naming none on the default workflow is the control: there the implementer left
// retro, which writes no handoff record, and the completion is recorded.
func TestALateImplementerCompletionIsOfThePhaseItsWorkflowLeft(t *testing.T) {
	for _, tc := range []struct {
		workflow string
		record   string
		version  int
		refusal  string
		says     string
	}{
		{workflow: "light", refusal: "HANDOFF_RECORD_REQUIRED", says: "completion of phase implementing"},
		{workflow: "light", record: "implement-2", version: 2},
		{workflow: phase.DefaultSequenceName},
	} {
		t.Run(tc.workflow+"/"+tc.record, func(t *testing.T) {
			pool := migratedPool(t)
			seedIssue(t, pool, sequenceIssue(phase.Merging, tc.workflow))
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim", LastHandoff: "implement-1"})
			// The daemon started the implementer on the issue, which is what lets a completion of a
			// phase it left be recorded rather than refused (leftItsPhase).
			if _, err := pool.Exec(context.Background(), `insert into claims (token, project, tree, issue, role, generation, session, session_file,
				state, launch_failures, prompt_failures, prompt_retires, uncertain_streak)
				values ('implement-claim', 'LEGION', 'LEGION-208', 'LEGION-208', 'implementer', 1, 'ses-implementer', '', 'working', 0, 0, 0, 0)`); err != nil {
				t.Fatalf("seed the implementer's claim: %v", err)
			}
			result, err := intake.ApplyFact(t.Context(), pool, "api", "late", intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleImplementer,
				Claim: "implement-claim", Commit: "late", HandoffRecord: tc.record, HandoffVersion: tc.version}, sequenceEngine(""), admissionStub{})
			if err != nil {
				t.Fatalf("ApplyFact: %v", err)
			}
			if tc.refusal != "" {
				if result.Refusal == nil || result.Refusal.Code != tc.refusal || !strings.Contains(result.Refusal.Message, tc.says) {
					t.Fatalf("refusal = %+v, want %s naming %q", result.Refusal, tc.refusal, tc.says)
				}
				if row := phaseRowOf(t, pool, claim.RoleImplementer); row.HandoffCommit != "" {
					t.Fatalf("the implementer's row = %+v, want nothing recorded", row)
				}
				return
			}
			if result.Refusal != nil {
				t.Fatalf("refusal = %+v, want the completion recorded", result.Refusal)
			}
			if row := phaseRowOf(t, pool, claim.RoleImplementer); row.HandoffCommit != "late" || row.HandoffRecord != tc.record {
				t.Fatalf("the implementer's row = %+v, want the late completion recorded with record %q", row, tc.record)
			}
			assertPhase(t, pool, phase.Merging)
		})
	}
}

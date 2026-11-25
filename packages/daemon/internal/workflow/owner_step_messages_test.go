package workflow

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The owner at its own tester or reviewer step is that step's worker, so a merge forward asked
// while it works there holds it and tells it of the landing, addressed by the step's role, as a
// fresh worker of that step is told: the role whose phases row names the owner's claim, and not
// the architect, the role of the place the owner works. On `implementer tester:owner` in testing
// and `implementer reviewer:owner` in reviewing, the merge forward asks the fresh implementer and
// holds the owner; the landing tells the owner, by the step's role, that the merge landed.
//
// The control: on `implementer reviewer` the fresh reviewer is held and told, under its own claim.
func TestTheOwnerAtItsOwnStepIsHeldAndToldTheMergeLanded(t *testing.T) {
	for _, tc := range []struct {
		name   string
		steps  []string
		at     phase.Phase
		worker claim.Role
		owner  bool
	}{
		{name: "tester:owner", steps: []string{"implementer", "tester:owner"}, at: phase.Testing, worker: claim.RoleTester, owner: true},
		{name: "reviewer:owner", steps: []string{"implementer", "reviewer:owner"}, at: phase.Reviewing, worker: claim.RoleReviewer, owner: true},
		{name: "control: a fresh reviewer", steps: []string{"implementer", "reviewer"}, at: phase.Reviewing, worker: claim.RoleReviewer},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			engine := returnEngine()
			root := stepRoot(t, pool, phase.Admitted)
			pr := recordedPR(t, pool, root.Key)
			pr.Base = "main"
			seedPR(t, pool, pr)
			seedStepsPlan(t, pool, root.Key, root.Generation, tc.steps...)
			move(t, pool, engine, root.Key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
			move(t, pool, engine, root.Key, TriggerImplementationReady, record.TaskPlace{}, record.PhaseRow{})
			assertWalksAt(t, pool, engine, root.Key, tc.at, stepPlace(2))
			owner := claim.Token(treeRoleToken(t, root.Key, claim.RoleArchitect))
			// worker is the claim the case's worker works under: the owner's, or the fresh reviewer's own.
			worker := claim.Token(treeRoleToken(t, root.Key, tc.worker))
			if tc.owner {
				worker = owner
			}

			onStored(t, pool, engine, root.Key, func(ctx context.Context, tx pgx.Tx, issue record.Issue) error {
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
			asked := mergeForwardMessages(t, pool)
			if got, want := sentOf(asked), []sent{{step: record.MergeForwardAsk, role: claim.RoleImplementer, claim: claim.Token(treeRoleToken(t, root.Key, claim.RoleImplementer))},
				{step: record.MergeForwardHold, role: tc.worker, claim: worker}}; !reflect.DeepEqual(got, want) {
				t.Fatalf("the merge forward queued %+v, want %+v, the hold under the owner's claim: %t", got, want, tc.owner)
			}

			applyRefusingNothing(t, pool, engine, intake.PullRequestSynchronized{Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", Base: "main",
				HeadSHA: "head-forward", Before: pr.HeadSHA, Sender: "legion-implementer[bot]"})
			landed := mergeForwardMessages(t, pool)[len(asked):]
			if got, want := sentOf(landed), []sent{{step: record.MergeForwardLanded, role: tc.worker, claim: worker}}; !reflect.DeepEqual(got, want) {
				t.Fatalf("the landing queued %+v, want %+v, under the owner's claim: %t", got, want, tc.owner)
			}
		})
	}
}

// The owner at a step of its own plan builds to the tree's design as a fresh worker of that step
// does, so a move of the root's design gate tells it, by the step's role. LEGION-209, a child at
// the reviewer's step of `implementer reviewer:owner`, which its own architect does, is told when a
// new version of the root's approved spec closes the gate.
//
// The control: the same child at the step of a fresh reviewer tells that reviewer's own claim.
func TestTheOwnerAtItsOwnStepIsToldTheDesignGateMoved(t *testing.T) {
	for _, tc := range []struct {
		name  string
		steps []string
		owner bool
	}{
		{name: "reviewer:owner", steps: []string{"implementer", "reviewer:owner"}, owner: true},
		{name: "control: a fresh reviewer", steps: []string{"implementer", "reviewer"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
				Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Planning, Generation: 4, Status: "in_progress", Rank: "U"})
			seedIssue(t, pool, record.Issue{Key: "LEGION-209", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
				Tree: "LEGION-208", Project: "LEGION", Title: "a child at its reviewer's step", Parent: new("LEGION-208"), Phase: phase.Reviewing,
				Generation: 4, Status: "in_progress", Rank: "V"})
			seedStepsAt(t, pool, "LEGION-209", 4, 2, nil, tc.steps...)
			worker := claim.Token(treeRoleToken(t, "LEGION-209", claim.RoleReviewer))
			if tc.owner {
				worker = claim.Token(treeRoleToken(t, "LEGION-209", claim.RoleArchitect))
			}
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-209", Role: claim.RoleReviewer, Claim: worker, StepTask: "a", StepPlace: 2, Entry: 1})
			seedGate(t, pool, record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 3, ApprovedVersion: new(3)})
			seedSlot(t, pool, record.Slot{Issue: "LEGION-208", Index: 0, AdmittedAt: time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)})

			applyMirrored(t, pool, testEngine(config.DesignGateRootIssues, nil), "version-4", intake.DispatchArtifact{
				Key: "LEGION-208", ArtifactID: "artifact-208", Kind: intake.DispatchArtifactVersion, Version: 4})

			// Who is told is the claim each role message is published to (record.RoleMessage.Role),
			// and the rows of LEGION-209's outbox are the messages to its workers.
			var told []string
			for _, row := range queuedRows(t, pool) {
				if message, ok := row.payload.(record.RoleMessage); ok && row.issue == "LEGION-209" {
					told = append(told, message.Role)
				}
			}
			if len(told) != 1 || told[0] != string(worker) {
				t.Fatalf("the gate's move told %v on LEGION-209, want the worker of its step, %s, alone", told, worker)
			}
		})
	}
}

// askedOfChild is LEGION-209, a child of the live tree LEGION-208, at the reviewer's step of
// `implementer:owner reviewer`, with the merge forward of its pull request #43 asked of agent (the
// claim the ask reached: record.MergeForward.Agent, and the implementer's phases row that ran
// step 1) in reviewing, not landed.
func askedOfChild(t *testing.T, agent claim.Token) *pgxpool.Pool {
	t.Helper()
	pool := migratedPool(t)
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Implementing, Generation: 4, Status: "in_progress", Rank: "U",
		LastDispatchSeq: 1})
	seedSlot(t, pool, record.Slot{Issue: "LEGION-208", Index: 0})
	seedIssue(t, pool, record.Issue{Key: "LEGION-209", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: "LEGION-208", Project: "LEGION", Title: "LEGION-209", Parent: new("LEGION-208"), Phase: phase.Reviewing, Generation: 4,
		Status: "in_progress", Rank: "V", LastDispatchSeq: 1})
	seedStepsAt(t, pool, "LEGION-209", 4, 2, nil, "implementer:owner", "reviewer")
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-209", Role: claim.RoleImplementer, Claim: agent, StepTask: "a", StepPlace: 1, Entry: 1})
	seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-209", Repo: "sjawhar/legion", Number: 43,
		Branch: "legion/LEGION-209", Base: "main", HeadSHA: "head-43", Failing: []string{},
		MergeForward: &record.MergeForward{Head: "head-43", Base: "main", Phase: phase.Reviewing, Agent: agent, Why: record.ConflictCheckNotGreen, AskedAt: stepClock}})
	return pool
}

// A child set back to todo while it runs is re-entered at its next generation (ReenterChild),
// which clears the merge forward of its pull request with the run. That ends a merge forward asked
// of the owner while the child's architect still runs, so it writes the owner's slot back to its
// own App: one supervise slot_credential of the child's own architect.
//
// The control: the ask of a fresh implementer leaves the owner's slot alone.
func TestAChildSetBackToTodoGivesItsOwnersSlotItsOwnAppBack(t *testing.T) {
	owner := claim.Token(treeRoleToken(t, "LEGION-209", claim.RoleArchitect))
	for _, tc := range []struct {
		name   string
		agent  claim.Token
		queued int
	}{
		{name: "an ask of the owner", agent: owner, queued: 1},
		{name: "control: an ask of a fresh implementer", agent: claim.Token(treeRoleToken(t, "LEGION-209", claim.RoleImplementer))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := askedOfChild(t, tc.agent)
			engine := returnEngine()
			personsTodoOnChild(t, pool, engine, "LEGION-209", "LEGION-208")
			if pr := recordedPR(t, pool, "LEGION-209"); pr.MergeForward != nil {
				t.Fatalf("the re-entry left the merge forward %+v, want it cleared with the run: this case proves nothing", pr.MergeForward)
			}
			rows := superviseRowsOf(t, pool, "LEGION-209", "slot_credential")
			if len(rows) != tc.queued {
				t.Fatalf("the re-entry queued the slot_credential rows %+v, want %d", rows, tc.queued)
			}
			for _, row := range rows {
				if row.Claim != owner || row.Role != claim.RoleArchitect || row.Tree != "LEGION-208" {
					t.Fatalf("the slot_credential row %+v, want one of the owner %s, the architect, in the tree LEGION-208", row, owner)
				}
			}
		})
	}
}

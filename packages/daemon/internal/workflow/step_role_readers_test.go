package workflow

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// deploymentChecker is a role the deployment defines: no phase of Legion's is its, so a step of
// it runs as the phase step, and the plan rows name it as the role of the issue's running step
// (StepRole), the role that works it where the step's key is not owner.
const deploymentChecker claim.Role = "checker"

// standAtStep writes issue standing at the phase step, or held from it (a failed claim's hold,
// which keeps the phase it left), with the plan `implementer checker` of task a running at step
// 2, so the running role read with the issue is the checker. It answers the issue as the store
// reads it.
func standAtStep(t *testing.T, pool *pgxpool.Pool, issue record.Issue, held bool) record.Issue {
	t.Helper()
	issue.Phase, issue.Hold = phase.Step, nil
	if held {
		issue.Phase, issue.Hold = phase.Held, &record.Hold{From: phase.Step}
	}
	seedIssue(t, pool, issue)
	seedStepsPlan(t, pool, issue.Key, issue.Generation, "implementer", string(deploymentChecker))
	seedRecord(t, pool, func(tx pgx.Tx) error {
		return record.NewStore().MoveTask(t.Context(), tx, issue.Key, issue.Generation, "a",
			record.TaskPlace{Kind: record.PlaceStep, Position: 2}, record.TaskRunning)
	})
	issue.StepRole = deploymentChecker
	return issue
}

// An issue whose running step is of a role the deployment defines, done by that role's own agent
// (here the plan `implementer checker`), stands at step, which no role of Legion's works, so each
// path that brings a worker back to its work reads the role with the
// issue (OpenRoleOf, Issue.RoleAt): the ready of a claim (readyWorkerTask), the boot's resume by
// rule (booted, through suspendedPhaseOwners), the silence repair (workerSilent, through
// phaseWorkerRecord and unpromptedPhaseWorker) and the CI settlement's wake of a released pod
// (wakeOnSettlement) each give the checker its work. Held from step, the issue stands in held,
// which no role works, so none of them gives the checker anything. Stranded reads the phase the
// hold took the issue from, so it names a failed checker on a held issue as on one at step: only
// its architect's retry starts it again.
func TestTheResumePathsGiveTheRunningDeploymentRoleItsWork(t *testing.T) {
	for _, held := range []bool{false, true} {
		name := "at step"
		if held {
			name = "held from step"
		}
		t.Run(name, func(t *testing.T) {
			t.Run("the ready of a claim", func(t *testing.T) {
				pool := migratedPool(t)
				const key = "LEGION-209"
				standAtStep(t, pool, record.Issue{Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
					Tree: key, Project: "LEGION", Title: "a root", Generation: 1, Status: "in_progress", Rank: "U", HandedOver: true}, held)
				seedSlot(t, pool, record.Slot{Issue: key, Index: 0})
				seedPhaseWorker(t, pool, key, deploymentChecker, "")

				if _, err := intake.ApplyFact(t.Context(), pool, "supervise", "supervise:ready", intake.ClaimReady{Issue: key, Role: deploymentChecker, Launch: 2},
					testEngine(config.DesignGateRootIssues, nil), admissionStub{}); err != nil {
					t.Fatalf("apply the ready: %v", err)
				}
				want := map[string]int{key + "/" + string(deploymentChecker): 1}
				if held {
					want = map[string]int{}
				}
				if got := superviseRequests(t, pool, "start"); !sameCounts(got, want) || !sameCounts(want, got) {
					t.Fatalf("the ready of the checker queued starts %v, want %v", got, want)
				}
			})

			t.Run("the boot's resume by rule", func(t *testing.T) {
				pool := migratedPool(t)
				project, err := claim.ProjectToken("LEGION")
				if err != nil {
					t.Fatal(err)
				}
				const key = "LEGION-212"
				standAtStep(t, pool, record.Issue{Key: key, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
					Tree: key, Project: "LEGION", Title: "a root", Generation: 1, Status: "in_progress", Rank: "U"}, held)
				seedTreeLifecycle(t, pool, key)
				seedRuleSuspendedOwner(t, pool, project, key, deploymentChecker)
				engine := withDeployment(testEngine(config.DesignGateRootIssues, nil))

				if _, err := intake.ApplyFact(t.Context(), pool, "daemon", "boot:LEGION:1", intake.DaemonBooted{Boot: 1}, engine, admissionStub{}); err != nil {
					t.Fatalf("apply the boot: %v", err)
				}
				rows := oneStartRowsOf(t, pool, key, engine.now())
				if held && len(rows) != 0 {
					t.Fatalf("the start rows of %s after the boot = %+v, want none", key, rows)
				}
				if !held && (len(rows) != 1 || rows[0].role != deploymentChecker || rows[0].task == "") {
					t.Fatalf("the start rows of %s after the boot = %+v, want the one resume of its checker carrying its task", key, rows)
				}
			})

			t.Run("the silence repair", func(t *testing.T) {
				pool := migratedPool(t)
				issue := standAtStep(t, pool, record.Issue{Key: silenceIssue, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
					Tree: silenceIssue, Project: "LEGION", Title: "a worker that stopped taking turns", Generation: 1, Status: "in_progress", Rank: "U"}, held)
				seedSilentWorker(t, pool, issue, deploymentChecker)

				if err := applySilence(t, pool, testEngine(config.DesignGateRootIssues, nil), aSilenceOf(deploymentChecker, 1)); err != nil {
					t.Fatalf("apply the silence: %v", err)
				}
				rows, notices := silenceSuperviseRows(t, pool), noticesOfKind(t, pool, silenceIssue, workerSilentNotice)
				if held && (len(rows) != 0 || len(notices) != 0) {
					t.Fatalf("after the silence of its checker, %s has supervise rows %+v and %d %s notices, want none", silenceIssue, rows, len(notices), workerSilentNotice)
				}
				if !held && (len(rows) != 1 || rows[0].op != "start" || rows[0].role != deploymentChecker || rows[0].task == "" || len(notices) != 1) {
					t.Fatalf("after the silence of its checker, %s has supervise rows %+v and %d %s notices, want its one start carrying its task and the one notice",
						silenceIssue, rows, len(notices), workerSilentNotice)
				}
			})

			t.Run("the CI settlement's wake of a released pod", func(t *testing.T) {
				rig := newWakeRig(t)
				standAtStep(t, rig.pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
					Tree: "LEGION-208", Project: "LEGION", Title: "root", Generation: wakeIssueGeneration, Status: "in_progress", Rank: "U", LastDispatchSeq: 1}, held)
				seedReleasedClaim(t, rig.pool, deploymentChecker, "session-checker")

				rig.apply("settled", settlement("head1", "build"))
				if !held {
					oneWake(t, rig.pool, deploymentChecker, pods.WakeChecks, "head1", "build")
					return
				}
				if starts, resumes := wakeStarts(t, rig.pool), wakeResumes(t, rig.pool); len(starts) != 0 || len(resumes) != 0 {
					t.Fatalf("the settlement wrote starts %+v and resumes %v, want no wake", starts, resumes)
				}
			})
		})
	}

	t.Run("Stranded", func(t *testing.T) {
		atStep := strandedIssue("LEGION-30", "LEGION-30", phase.Step)
		atStep.StepRole = deploymentChecker
		heldFromStep := strandedIssue("LEGION-31", "LEGION-31", phase.Held)
		heldFromStep.Hold, heldFromStep.StepRole = &record.Hold{From: phase.Step}, deploymentChecker
		failedAtStep := strandedClaim(t, "LEGION-30", "LEGION-30", deploymentChecker, supervise.StateFailed)
		failedHeld := strandedClaim(t, "LEGION-31", "LEGION-31", deploymentChecker, supervise.StateFailed)
		claims := []supervise.Claim{failedAtStep, failedHeld}
		stored := map[claim.Token]supervise.Claim{}
		for _, c := range claims {
			stored[c.Token] = c
		}
		s := &strandedStore{issues: []record.Issue{atStep, heldFromStep}, slots: slotsOf("LEGION-30", "LEGION-31"), claims: stored}

		got, err := Stranded(t.Context(), nil, s, "LEGION", nil, claims, nil, strandedNow)
		if err != nil {
			t.Fatalf("Stranded: %v", err)
		}
		if want := claims; !reflect.DeepEqual(got, want) {
			t.Fatalf("stranded = %v, want the failed checker of the issue at step and of the issue held from it, %v", names(got), names(want))
		}
	})
}

// Each other reader of the workflow that names the worker of an issue's phase reads the role
// with the issue (Issue.RoleAt, OpenRoleOf), so at step it names the role of the running step
// where that step's own agent does it, as here (at a step the owner does, the role there is the
// architect): the silence rule's unprompted phase worker (unpromptedPhaseWorker), the members a
// design gate's move tells (tellGateMoved), the roles a CI settlement tells beside the architect
// and the implementer (tellChecks), and the closing sentence of the architect's task for a root
// carried at step, whose worker the daemon starts itself (writeCarriedPhase).
func TestTheWorkflowsOtherReadersNameTheRunningDeploymentRole(t *testing.T) {
	t.Run("the unprompted phase worker", func(t *testing.T) {
		s := aSilentImplementer()
		s.issue.Phase, s.issue.StepRole, s.role = phase.Step, deploymentChecker, deploymentChecker
		if !unpromptedPhaseWorker(s.issue, s.root, s.gate, s.role, s.run) || !s.answer() {
			t.Fatalf("the checker of an issue at step, up between turns with no task and silent past its bound, is not named its unprompted phase worker")
		}
	})

	t.Run("a design gate's move", func(t *testing.T) {
		pool := migratedPool(t)
		seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
			Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Planning, Generation: 4, Status: "in_progress", Rank: "U"})
		standAtStep(t, pool, record.Issue{Key: "LEGION-209", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
			Tree: "LEGION-208", Project: "LEGION", Title: "a child at its checker's step", Parent: new("LEGION-208"),
			Generation: 4, Status: "in_progress", Rank: "V"}, false)
		seedGate(t, pool, record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 3, ApprovedVersion: new(3)})
		seedSlot(t, pool, record.Slot{Issue: "LEGION-208", Index: 0, AdmittedAt: time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)})

		applyMirrored(t, pool, testEngine(config.DesignGateRootIssues, nil), "version-4", intake.DispatchArtifact{
			Key: "LEGION-208", ArtifactID: "artifact-208", Kind: intake.DispatchArtifactVersion, Version: 4})

		closed := gateMovedBody(record.DesignStepSpec, "artifact-208", 4, false)
		assertRoleMessages(t, pool, "LEGION-208", []record.RoleMessage{
			{Role: treeRoleToken(t, "LEGION-208", claim.RolePlanner), Body: closed},
			{Role: treeRoleToken(t, "LEGION-208", claim.RoleArchitect), Body: closed},
		})
		assertRoleMessages(t, pool, "LEGION-209", []record.RoleMessage{
			{Role: treeRoleToken(t, "LEGION-209", deploymentChecker), Body: closed},
		})
	})

	t.Run("a CI settlement", func(t *testing.T) {
		pool := migratedPool(t)
		standAtStep(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208",
			Project: "LEGION", Title: "root", Generation: 1, Status: "in_progress", Rank: "U"}, false)
		seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion",
			Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head"})
		seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim"})
		seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: deploymentChecker, Claim: "checker-claim"})

		applyRefusingNothing(t, pool, mergingEngine(), settledAt("head", 1, "unit"))

		body := toldBody("CI settled at head: \"build\" succeeded, \"unit\" failed")
		assertRoleMessages(t, pool, "LEGION-208", []record.RoleMessage{
			{Role: roleToken(t, claim.RoleArchitect), Body: body},
			{Role: roleToken(t, deploymentChecker), Body: body},
			{Role: roleToken(t, claim.RoleImplementer), Body: body},
		})
	})

	t.Run("the architect's start of a root carried at step", func(t *testing.T) {
		ctx := t.Context()
		pool := migratedPool(t)
		root := architectEntryRoot()
		root.Generation = 2
		root = standAtStep(t, pool, root, false)
		seedRecord(t, pool, func(tx pgx.Tx) error {
			return record.NewStore().PutHandedOverRoot(ctx, tx, record.HandedOverRoot{Issue: root.Key, Generation: root.Generation, GateCarried: true})
		})
		var start ArchitectStart
		if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			var err error
			start, err = ArchitectStartOf(ctx, tx, record.NewStore(), specGated, root)
			return err
		}); err != nil {
			t.Fatalf("read what the architect's first task is derived from: %v", err)
		}
		const says = " The daemon starts that phase's own worker itself."
		if task := ArchitectStartTask(start); !start.Carried || !strings.Contains(task, says) {
			t.Fatalf("the architect's start task of a root carried at its checker's step (carried %t) = %q, want it to say %q", start.Carried, task, says)
		}
	})
}

// The architect's catch-up names the role that runs a member at step, beside its phase: step alone
// does not say which role of the deployment works it. A member at a phase of Legion's names none.
func TestTheCatchUpNamesTheRunningDeploymentRole(t *testing.T) {
	pool := migratedPool(t)
	root := seedAdmittedTree(t, pool, 1)
	root.Phase = phase.Implementing
	seedIssue(t, pool, root)
	parent := root.Key
	standAtStep(t, pool, record.Issue{Key: "LEGION-2", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root.Key,
		Project: "LEGION", Title: "Child", Parent: &parent, Generation: 1, Status: "in_progress", Rank: "V"}, false)

	claimReady(t, pool, testEngine(config.DesignGateRootIssues, nil), root.Key, claim.RoleArchitect, 1)

	var listed string
	if err := pool.QueryRow(t.Context(), `select (payload->'catch_up'->'issues')::text from outbox
		where kind = 'notice' and payload->>'kind' = 'catch-up' order by id limit 1`).Scan(&listed); err != nil {
		t.Fatalf("read the catch-up's issues: %v", err)
	}
	var issues []map[string]any
	if err := json.Unmarshal([]byte(listed), &issues); err != nil {
		t.Fatalf("decode the catch-up's issues %s: %v", listed, err)
	}
	roles := map[string]any{}
	for _, issue := range issues {
		roles[issue["key"].(string)] = issue["stepRole"]
	}
	if want := map[string]any{"LEGION-1": nil, "LEGION-2": "checker"}; !reflect.DeepEqual(roles, want) {
		t.Fatalf("the catch-up's running roles = %v (issues %s), want %v", roles, listed, want)
	}
}

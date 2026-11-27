package workflow

import (
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

// seedReleaseStopped seeds the claim of role on issue, a member of LEGION-208's tree, as the
// project release leaves it: suspended by `project_release`, its session kept, given entry given.
// The controller's claim is on no issue and no tree.
func seedReleaseStopped(t *testing.T, pool *pgxpool.Pool, issue string, role claim.Role, given int64) claim.Token {
	t.Helper()
	project, err := claim.ProjectToken("LEGION")
	if err != nil {
		t.Fatal(err)
	}
	token, tree := claim.ControllerToken(project), ""
	if role != claim.RoleController {
		if token, err = claim.NewToken(project, issue, role); err != nil {
			t.Fatal(err)
		}
		tree = "LEGION-208"
	}
	if _, err := pool.Exec(t.Context(), `insert into claims (token, project, tree, issue, role, generation, session, session_file,
		state, launch_failures, prompt_failures, prompt_retires, uncertain_streak, suspended_by, given_entry)
		values ($1, $2, $3, $4, $5, 1, $6, '/sessions/ses.jsonl', 'suspended', 0, 0, 0, 0, 'project_release', $7)`,
		string(token), project, tree, issue, string(role), "ses-"+string(token), given); err != nil {
		t.Fatalf("seed the %s %s the project release stopped: %v", issue, role, err)
	}
	return token
}

// The daemon's next boot starts again each claim the project release stopped, whatever its role
// (dispatch://INFRA-3583/spec R8), one start each, at its wake pace, on the session the claim
// kept; a claim one of the boot's other steps already started gets none, and the controller's
// claim is left to its keeper. Each case is one role kind of the tree, read off one boot:
//
//   - the tree's architect, which the resume by rule never starts: a start with no task and no
//     phase, so no phase fence ends it while it waits its turn;
//   - a phase owner level with the entry its phases row names, and one whose turn the release
//     cut, holding that turn's task: a start of its phase carrying the phase's resume task, so its
//     ready writes no second start (readyWorkerTask); the outbox sends a claim that still holds
//     its cut turn's task that task and not this one, and the release's ask is no task;
//   - a phase owner behind its entry: that entry's work at that entry, as admission's pass sends
//     it;
//   - a role of an earlier phase: a start with no task and no phase;
//   - the role of an open look of a closed issue's production check: that look's start again,
//     with no task and no second look opened;
//   - a claim admission's first pass already started, behind its entry: that start and no other;
//   - the controller: nothing.
//
// Every start this step writes carries the ruleResume mark and waits for no slot.
func TestTheBootStartsEachClaimTheProjectReleaseStoppedOnce(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	ctx := t.Context()
	root := "LEGION-208"
	seedIssue(t, pool, record.Issue{Key: root, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root,
		Project: "LEGION", Title: "root", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "U", HandedOver: true})
	seedSlot(t, pool, record.Slot{Issue: root, Index: 0})
	child := func(key string, p phase.Phase, status string) {
		seedIssue(t, pool, record.Issue{Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root,
			Parent: &root, Project: "LEGION", Title: key, Phase: p, Generation: 1, Status: status, Rank: "V"})
	}
	child("LEGION-209", phase.Testing, "in_progress")
	child("LEGION-210", phase.Implementing, "in_progress")
	child("LEGION-211", phase.Implementing, "in_progress")
	child("LEGION-212", phase.Done, "done")

	seedReleaseStopped(t, pool, root, claim.RoleArchitect, 0)
	levelOwner := seedReleaseStopped(t, pool, root, claim.RoleImplementer, seededEntry)
	seedPhase(t, pool, record.PhaseRow{Issue: root, Role: claim.RoleImplementer, Claim: levelOwner, Entry: seededEntry,
		EntryTask: "Implement the root.", EntryRaisedAt: seededRaisedAt})
	seedReleaseStopped(t, pool, root, claim.RolePlanner, 0)

	behindOwner := seedReleaseStopped(t, pool, "LEGION-209", claim.RoleTester, seededEntry-1)
	behindTask := "Test LEGION-209 again: the implementer pushed a fix."
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-209", Role: claim.RoleTester, Claim: behindOwner, Entry: seededEntry,
		EntryTask: behindTask, EntryRaisedAt: seededRaisedAt})

	cutTurn := seedReleaseStopped(t, pool, "LEGION-210", claim.RoleImplementer, 0)
	if _, err := pool.Exec(ctx, `insert into pending_task_deliveries (claim_token, delivery_id, task, phase, generation, queued_at)
		values ($1, 'outbox:7', 'Implement LEGION-210.', 'implementing', 1, now())`, string(cutTurn)); err != nil {
		t.Fatalf("seed the task of the turn the release cut: %v", err)
	}

	started := seedReleaseStopped(t, pool, "LEGION-211", claim.RoleImplementer, seededEntry-1)
	startedTask := "Implement LEGION-211 on entry 4."
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-211", Role: claim.RoleImplementer, Claim: started, Entry: seededEntry,
		EntryTask: startedTask, EntryRaisedAt: seededRaisedAt})
	seedRecord(t, pool, func(tx pgx.Tx) error {
		row, err := record.NewOutboxRow("LEGION-211", record.SuperviseRequest{Op: "start", Tree: root, Role: claim.RoleImplementer,
			Task: startedTask, Generation: 1, Phase: phase.Implementing, Entry: seededEntry}, time.Now())
		if err != nil {
			return err
		}
		return record.NewStore().Enqueue(ctx, tx, row)
	})

	lookOpened := time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC)
	seedReleaseStopped(t, pool, "LEGION-212", claim.RoleTester, 0)
	seedRecord(t, pool, func(tx pgx.Tx) error {
		return record.NewStore().PutProductionCheck(ctx, tx, record.ProductionCheck{
			Issue: "LEGION-212", Project: "LEGION", Tree: root, Repo: "acme/widgets", Number: 12, MergeSHA: "merge-12",
			OpenedAt: lookOpened, BoundAt: lookOpened.Add(24 * time.Hour), Role: claim.RoleTester, Looks: 2, LookOpen: true,
			LookWakeAt: &lookOpened, LastWakeKind: wakeMerge, LastWakeDetail: "merge-12", LastWakeAt: &lookOpened,
		})
	})

	seedReleaseStopped(t, pool, "", claim.RoleController, 0)

	if _, err := intake.ApplyFact(ctx, pool, "daemon", "boot:LEGION:1", intake.DaemonBooted{Boot: 1},
		testEngine(config.DesignGateRootIssues, nil), admissionStub{}); err != nil {
		t.Fatalf("apply the boot: %v", err)
	}

	// one is the one start the case reads, after asserting there is exactly one.
	one := func(t *testing.T, issue string, role claim.Role) record.SuperviseRequest {
		t.Helper()
		starts := roleStartsOf(t, pool, issue, role)
		if len(starts) != 1 {
			t.Fatalf("the boot left %s's %s the starts %+v, want exactly one", issue, role, starts)
		}
		return starts[0]
	}
	// taskless is a start that hands the role no work and resumes it by rule.
	taskless := func(t *testing.T, start record.SuperviseRequest, wantPhase phase.Phase) {
		t.Helper()
		if start.Task != "" || start.Entry != 0 || start.Phase != wantPhase || !start.RuleResume || start.NeedsSlot || start.CheckLook != 0 {
			t.Fatalf("the boot's start = %+v, want one with no task, no entry, phase %q, the ruleResume mark and no slot", start, wantPhase)
		}
	}
	// resuming is a level phase owner's start: its phase, the phase's resume task marked as one,
	// and the ruleResume mark, with no entry and no slot.
	resuming := func(t *testing.T, start record.SuperviseRequest, issue record.Issue) {
		t.Helper()
		if want := phaseResumeTask(t, pool, testEngine(config.DesignGateRootIssues, nil), issue.Key); start.Task != want || !start.ResumeTask || start.Entry != 0 || start.Phase != issue.Phase ||
			!start.RuleResume || start.NeedsSlot || start.CheckLook != 0 {
			t.Fatalf("the boot's start = %+v, want one of phase %q carrying the resume task %q, marked ResumeTask and ruleResume, with no entry and no slot",
				start, issue.Phase, want)
		}
	}

	t.Run("the tree's architect", func(t *testing.T) {
		taskless(t, one(t, root, claim.RoleArchitect), "")
	})
	t.Run("a phase owner level with its entry", func(t *testing.T) {
		resuming(t, one(t, root, claim.RoleImplementer), record.Issue{Key: root, Title: "root", Phase: phase.Implementing})
	})
	t.Run("a phase owner behind its entry", func(t *testing.T) {
		start := one(t, "LEGION-209", claim.RoleTester)
		if start.Task != behindTask || start.Entry != seededEntry || start.Phase != phase.Testing || !start.RuleResume {
			t.Fatalf("the boot's start of the tester behind its entry = %+v, want entry %d's work %q at that entry, in testing, marked ruleResume",
				start, seededEntry, behindTask)
		}
	})
	t.Run("a role of an earlier phase", func(t *testing.T) {
		taskless(t, one(t, root, claim.RolePlanner), "")
	})
	t.Run("a cut turn holding its task", func(t *testing.T) {
		resuming(t, one(t, "LEGION-210", claim.RoleImplementer), record.Issue{Key: "LEGION-210", Title: "LEGION-210", Phase: phase.Implementing})
	})
	t.Run("the role of an open production check look", func(t *testing.T) {
		start := one(t, "LEGION-212", claim.RoleTester)
		if start.CheckLook != 2 || !start.CheckOpenedAt.Equal(lookOpened) || start.Task != "" || start.Phase != "" || !start.RuleResume {
			t.Fatalf("the boot's start of the check's tester = %+v, want look 2 of the check opened %s again, with no task and no phase, marked ruleResume",
				start, lookOpened)
		}
		if check := productionCheck(t, pool, "LEGION-212"); check.Looks != 2 || !check.LookOpen {
			t.Fatalf("the check after the boot = %+v, want it still at look 2, open: the boot opens no next look", check)
		}
	})
	t.Run("a claim admission's pass already started", func(t *testing.T) {
		start := one(t, "LEGION-211", claim.RoleImplementer)
		if start.Task != startedTask || start.RuleResume {
			t.Fatalf("the start of the implementer admission started = %+v, want admission's own and no second", start)
		}
	})
	t.Run("the controller", func(t *testing.T) {
		if got := superviseRequests(t, pool, "start"); got["/"+string(claim.RoleController)] != 0 {
			t.Fatalf("the boot queued starts %v, want none of the controller, which its keeper resumes", got)
		}
	})
	if got, want := superviseRequests(t, pool, "start"), map[string]int{
		root + "/architect": 1, root + "/implementer": 1, root + "/planner": 1, "LEGION-209/tester": 1,
		"LEGION-210/implementer": 1, "LEGION-211/implementer": 1, "LEGION-212/tester": 1,
	}; !sameCounts(got, want) {
		t.Fatalf("the boot queued starts %v, want %v", got, want)
	}
}

// A root's architect the project release stopped at admitted comes back as it does in every other
// phase: one start with no task, no entry and no phase, marked ruleResume, though phase.RoleFor
// answers the architect for admitted (dispatch://INFRA-2614) and its phases row names an entry
// above the one its claim was given. startStoppedClaim reads no architect as a phase owner, so the
// boot sends it no entry's work and names no phase the outbox's phase fence could end the start
// on; the work of its entry is admission's sweep's to send, as for an architect the resume by rule
// leaves alone (TestTheBootsResumeReadsRoleForAndStartsNoArchitectAtAdmittedOrAccepting).
func TestTheBootStartsAnArchitectTheProjectReleaseStoppedAtAdmittedWithNoTask(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	root := "LEGION-208"
	seedIssue(t, pool, record.Issue{Key: root, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root,
		Project: "LEGION", Title: "root", Phase: phase.Admitted, Generation: 1, Status: "in_progress", Rank: "U"})
	seedTreeLifecycle(t, pool, root)
	architect := seedReleaseStopped(t, pool, root, claim.RoleArchitect, seededEntry-1)
	seedPhase(t, pool, record.PhaseRow{Issue: root, Role: claim.RoleArchitect, Claim: architect, Entry: seededEntry,
		EntryTask: "Write the root's spec.", EntryRaisedAt: seededRaisedAt})

	if _, err := intake.ApplyFact(t.Context(), pool, "daemon", "boot:LEGION:1", intake.DaemonBooted{Boot: 1},
		testEngine(config.DesignGateRootIssues, nil), admissionStub{}); err != nil {
		t.Fatalf("apply the boot: %v", err)
	}

	starts := roleStartsOf(t, pool, root, claim.RoleArchitect)
	if len(starts) != 1 {
		t.Fatalf("the boot left the architect stopped at admitted the starts %+v, want exactly one", starts)
	}
	if start := starts[0]; start.Task != "" || start.Entry != 0 || start.Phase != "" || !start.RuleResume || start.NeedsSlot {
		t.Fatalf("the boot's start of the architect stopped at admitted = %+v, want one with no task, no entry and no phase, marked ruleResume and waiting for no slot",
			start)
	}
}

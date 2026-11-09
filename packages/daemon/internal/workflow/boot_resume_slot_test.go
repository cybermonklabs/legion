package workflow

import (
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// seedRuleSuspendedOwner records the phase owner of issue as a claim the daemon's own rule
// suspended, the one state the boot's resume acts on (booted).
func seedRuleSuspendedOwner(t *testing.T, pool *pgxpool.Pool, project string, issue string, role claim.Role) {
	t.Helper()
	token, err := claim.NewToken(project, issue, role)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `insert into claims (token, project, tree, issue, role, generation, session, session_file,
		state, launch_failures, prompt_failures, prompt_retires, uncertain_streak, suspended_by)
		values ($1, $2, $3, $3, $4, 1, 'ses', '/sessions/ses.jsonl', 'suspended', 0, 0, 0, 0, 'daemon')`,
		string(token), project, issue, string(role)); err != nil {
		t.Fatalf("seed the suspended %s of %s: %v", role, issue, err)
	}
}

// The boot's resume by rule holds its one row for the issue's admission slot only where the
// issue is at work, and a row of an issue that is not is runnable the moment it is written.
//
// A start is one row since INFRA-1573, and that row carries the worker's task, so the mark that
// holds it for a slot now decides when the worker comes back. The mark is for the start that
// puts a worker to work, and a merger standing in merging while a closed design gate holds the
// READY it already built is the one phase owner that puts none to work: it comes back to wait
// on a person. Marked for a slot it would stand unclaimable until an admission pass gave its
// issue a row, for work it does not have, and the session a person may still want to talk to
// would stay down meanwhile; marked for none it comes back at once, which is what the boot
// before INFRA-1573 did with its task-less relaunch row. Admission's own repair reads the same
// holder the same way (admit's resumeSlottedWorkers, record.AtWork).
//
// The two issues differ in nothing but their phase and that held READY: each is a root of its
// own running tree whose phase owner the daemon's rule suspended, and neither holds a slot.
func TestTheBootsResumeWaitsForASlotOnlyWhereTheIssueIsAtWork(t *testing.T) {
	pool := migratedPool(t)
	project, err := claim.ProjectToken("LEGION")
	if err != nil {
		t.Fatal(err)
	}
	const working, heldReady = "LEGION-208", "LEGION-209"
	for _, issue := range []record.Issue{
		{Key: working, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: working, Project: "LEGION",
			Title: "at work", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "U"},
		{Key: heldReady, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: heldReady, Project: "LEGION",
			Title: "a READY the gate holds", Phase: phase.Merging, Generation: 1, Status: "retro", Rank: "V",
			ReadyPending: &record.ReadyPending{Step: record.DesignStepSpec, Version: 2}},
	} {
		seedIssue(t, pool, issue)
		seedTreeLifecycle(t, pool, issue.Key)
	}
	seedRuleSuspendedOwner(t, pool, project, working, claim.RoleImplementer)
	seedRuleSuspendedOwner(t, pool, project, heldReady, claim.RoleMerger)
	engine := testEngine(config.DesignGateRootIssues, nil)
	now := engine.now()

	if _, err := intake.ApplyFact(t.Context(), pool, "daemon", "boot:LEGION:1", intake.DaemonBooted{Boot: 1}, engine, admissionStub{}); err != nil {
		t.Fatalf("apply the boot: %v", err)
	}

	atWork := oneStartRowsOf(t, pool, working, now)
	if len(atWork) != 1 || atWork[0].role != claim.RoleImplementer || atWork[0].task == "" {
		t.Fatalf("the start rows of %s = %+v, want one row for its implementer carrying its resume task", working, atWork)
	}
	if !atWork[0].needsSlot || atWork[0].claimable {
		t.Fatalf("the boot's resume of %s = %+v, want it held for the issue's admission slot: the issue is at work, and this row puts its worker to work",
			working, atWork[0])
	}

	waiting := oneStartRowsOf(t, pool, heldReady, now)
	if len(waiting) != 1 || waiting[0].role != claim.RoleMerger || waiting[0].task == "" {
		t.Fatalf("the start rows of %s = %+v, want one row for its merger carrying its resume task", heldReady, waiting)
	}
	if waiting[0].needsSlot || !waiting[0].claimable {
		t.Fatalf("the boot's resume of %s = %+v, want it runnable at once and marked for no slot: a merger whose READY a closed design gate holds has no work where it stands, so this row brings its session back rather than putting a worker to work",
			heldReady, waiting[0])
	}
}

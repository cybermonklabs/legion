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

// The boot's resume by rule marks its one row for the issue's admission slot where the phase it
// resumes holds one, and writes it unmarked where that phase holds none (StartOrOwe, over
// record.PhaseKeepsSlot).
//
// A start is one row since INFRA-1573, and that row carries the worker's task, so the mark now
// decides when the worker comes back. An issue holds a slot from the moment the daemon starts
// it until its work is past its review (record.KeepsSlot), so the resume of a worker past that
// review waits for nothing: the row is runnable the moment the boot writes it, and the merger
// is reached. What the mark buys is the other issue: one at work whose tree runs but which
// holds no slot — a member of a re-admitted tree still waiting in the line while the cap is
// full — whose row waits for the slot instead of putting a worker to work on an issue admission
// has not admitted, and runs when the slot comes.
//
// The worker past its review read here is the merger of an issue standing in merging: its own
// merge is what ends the phase, and it works it on no row of the cap's, since merging stands
// past the issue's review.
//
// The two issues differ in their phase and in the mark their resume carries: each is a root of
// its own running tree whose phase owner the daemon's rule suspended.
func TestTheBootsResumeRunsPastAReviewAndWaitsWhereTheIssueHoldsNoRow(t *testing.T) {
	pool := migratedPool(t)
	project, err := claim.ProjectToken("LEGION")
	if err != nil {
		t.Fatal(err)
	}
	const unslotted, merging = "LEGION-208", "LEGION-209"
	seedIssue(t, pool, record.Issue{Key: unslotted, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: unslotted, Project: "LEGION", Title: "waiting for a slot", Phase: phase.Implementing, Generation: 1,
		Status: "in_progress", Rank: "U"})
	seedTreeLifecycle(t, pool, unslotted)
	seedIssue(t, pool, record.Issue{Key: merging, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: merging, Project: "LEGION", Title: "merging its reviewed head", Phase: phase.Merging, Generation: 1,
		Status: "retro", Rank: "V"})
	seedRuleSuspendedOwner(t, pool, project, unslotted, claim.RoleImplementer)
	seedRuleSuspendedOwner(t, pool, project, merging, claim.RoleMerger)
	engine := testEngine(config.DesignGateRootIssues, nil)
	now := engine.now()
	// The issue in merging holds no row: its work stands past its review (record.KeepsSlot), so
	// the lifecycle of its tree is opened on its own rather than by a slot of the cap's.
	seedTreeLifecycle(t, pool, merging)

	if _, err := intake.ApplyFact(t.Context(), pool, "daemon", "boot:LEGION:1", intake.DaemonBooted{Boot: 1}, engine, admissionStub{}); err != nil {
		t.Fatalf("apply the boot: %v", err)
	}

	slotted := oneStartRowsOf(t, pool, merging, now)
	if len(slotted) != 1 || slotted[0].role != claim.RoleMerger || slotted[0].task == "" {
		t.Fatalf("the start rows of %s = %+v, want one row for its merger carrying its resume task", merging, slotted)
	}
	if slotted[0].needsSlot || !slotted[0].claimable {
		t.Fatalf("the boot's resume of %s = %+v, want it marked for no admission slot and runnable at once: the issue stands past its review, where it holds no row a marked start could wait on",
			merging, slotted[0])
	}

	waiting := oneStartRowsOf(t, pool, unslotted, now)
	if len(waiting) != 1 || waiting[0].role != claim.RoleImplementer || waiting[0].task == "" {
		t.Fatalf("the start rows of %s = %+v, want one row for its implementer carrying its resume task", unslotted, waiting)
	}
	if !waiting[0].needsSlot || waiting[0].claimable {
		t.Fatalf("the boot's resume of %s = %+v, want it held for the issue's admission slot: the issue holds none, and a row runnable now would put its worker to work on an issue the cap has not admitted",
			unslotted, waiting[0])
	}

	seedSlot(t, pool, record.Slot{Issue: unslotted, Index: 1, AdmittedAt: now})

	after := oneStartRowsOf(t, pool, unslotted, now)
	if len(after) != 1 || after[0].id != waiting[0].id || !after[0].claimable {
		t.Fatalf("the start rows of %s once a slot names it = %+v, want the one row %d, now runnable: the row waits for the slot rather than being dropped",
			unslotted, after, waiting[0].id)
	}
	if after[0].task != waiting[0].task {
		t.Fatalf("the runnable row of %s carries task %q, want the %q the boot wrote it with", unslotted, after[0].task, waiting[0].task)
	}
}

// The boot's resume by rule leaves a root's architect alone, at admitted as in accepting, though
// phase.RoleFor answers the architect for both (dispatch://INFRA-2614, R2). The architect's claim
// is started with its tree by admission and sent the work of its entry by admission's sweep, so
// the phase owners the boot reads (suspendedPhaseOwners) name no architect, and the boot writes
// no start for one whatever source suspended its claim. The control is a phase worker of a
// running tree, which the same boot resumes.
func TestTheBootsResumeReadsRoleForAndStartsNoArchitectAtAdmittedOrAccepting(t *testing.T) {
	pool := migratedPool(t)
	project, err := claim.ProjectToken("LEGION")
	if err != nil {
		t.Fatal(err)
	}
	const admitted, accepting, working = "LEGION-210", "LEGION-211", "LEGION-212"
	for _, seeded := range []struct {
		key   string
		stand phase.Phase
		role  claim.Role
	}{
		{admitted, phase.Admitted, claim.RoleArchitect},
		{accepting, phase.Accepting, claim.RoleArchitect},
		{working, phase.Implementing, claim.RoleImplementer},
	} {
		seedIssue(t, pool, record.Issue{Key: seeded.key, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
			Tree: seeded.key, Project: "LEGION", Title: "a root in " + string(seeded.stand), Phase: seeded.stand, Generation: 1,
			Status: "in_progress", Rank: seeded.key})
		seedTreeLifecycle(t, pool, seeded.key)
		seedRuleSuspendedOwner(t, pool, project, seeded.key, seeded.role)
	}
	engine := testEngine(config.DesignGateRootIssues, nil)
	now := engine.now()

	if _, err := intake.ApplyFact(t.Context(), pool, "daemon", "boot:LEGION:1", intake.DaemonBooted{Boot: 1}, engine, admissionStub{}); err != nil {
		t.Fatalf("apply the boot: %v", err)
	}

	for _, root := range []string{admitted, accepting} {
		if rows := oneStartRowsOf(t, pool, root, now); len(rows) != 0 {
			t.Fatalf("the start rows of %s after the boot = %+v, want none: the boot's resume reads no architect as a phase owner", root, rows)
		}
	}
	if rows := oneStartRowsOf(t, pool, working, now); len(rows) != 1 || rows[0].role != claim.RoleImplementer {
		t.Fatalf("the start rows of %s after the boot = %+v, want the one resume of its implementer: the control that the boot ran", working, rows)
	}
}

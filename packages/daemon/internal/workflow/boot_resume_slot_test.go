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

// The boot's resume by rule holds its one row for the issue's admission slot, like every other
// start of a phase worker (StartOrOwe), and that mark is what the slot the issue already holds
// answers at once.
//
// A start is one row since INFRA-1573, and that row carries the worker's task, so the mark now
// decides when the worker comes back. An issue holds its slot from the moment the daemon starts
// it until its work ends (record.KeepsSlot), merging included, so the mark costs the resume of
// a slot holder nothing: the row is runnable the moment the boot writes it, and the merger is
// reached. What the mark buys is the other issue: one whose tree runs but which holds no slot —
// a member of a re-admitted tree still waiting in the line while the cap is full — whose row
// waits for the slot instead of putting a worker to work on an issue admission has not admitted,
// and runs when the slot comes.
//
// The merger of an issue standing in merging while a closed design gate holds the READY it
// already built is the holder that used to be marked for no slot, on the ground that it comes
// back to wait on a person rather than to work. It is the case read here because it is the one
// the conditional was for: it holds its slot like any other issue in merging, so the row it is
// given is runnable at once all the same.
//
// The two issues differ in their phase, that held READY, and the slot: each is a root of its own
// running tree whose phase owner the daemon's rule suspended.
func TestTheBootsResumeReachesASlotHolderAndWaitsWhereTheIssueHoldsNone(t *testing.T) {
	pool := migratedPool(t)
	project, err := claim.ProjectToken("LEGION")
	if err != nil {
		t.Fatal(err)
	}
	const unslotted, heldReady = "LEGION-208", "LEGION-209"
	seedIssue(t, pool, record.Issue{Key: unslotted, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: unslotted, Project: "LEGION", Title: "waiting for a slot", Phase: phase.Implementing, Generation: 1,
		Status: "in_progress", Rank: "U"})
	seedTreeLifecycle(t, pool, unslotted)
	seedIssue(t, pool, record.Issue{Key: heldReady, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: heldReady, Project: "LEGION", Title: "a READY the gate holds", Phase: phase.Merging, Generation: 1,
		Status: "retro", Rank: "V", ReadyPending: &record.ReadyPending{Step: record.DesignStepSpec, Version: 2}})
	seedRuleSuspendedOwner(t, pool, project, unslotted, claim.RoleImplementer)
	seedRuleSuspendedOwner(t, pool, project, heldReady, claim.RoleMerger)
	engine := testEngine(config.DesignGateRootIssues, nil)
	now := engine.now()
	// The slot the issue holds where it stands: an issue in merging keeps the row the daemon
	// gave it when it started the issue (record.KeepsSlot), which is what makes the mark free
	// for this resume. seedSlot opens the tree's lifecycle with it.
	seedSlot(t, pool, record.Slot{Issue: heldReady, Index: 0, AdmittedAt: now})

	if _, err := intake.ApplyFact(t.Context(), pool, "daemon", "boot:LEGION:1", intake.DaemonBooted{Boot: 1}, engine, admissionStub{}); err != nil {
		t.Fatalf("apply the boot: %v", err)
	}

	slotted := oneStartRowsOf(t, pool, heldReady, now)
	if len(slotted) != 1 || slotted[0].role != claim.RoleMerger || slotted[0].task == "" {
		t.Fatalf("the start rows of %s = %+v, want one row for its merger carrying its resume task", heldReady, slotted)
	}
	if !slotted[0].needsSlot || !slotted[0].claimable {
		t.Fatalf("the boot's resume of %s = %+v, want it marked for the issue's admission slot and runnable at once: the issue holds its slot in merging, so the mark it carries is already answered and the row reaches the merger",
			heldReady, slotted[0])
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

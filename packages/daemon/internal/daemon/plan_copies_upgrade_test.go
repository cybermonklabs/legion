package daemon

import (
	"context"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/store/migrations"
	"github.com/sjawhar/legion/daemon/internal/testpg/testdb"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// taskCopiesMigration ends the name of the migration that gives each task its branch, the first one
// of the release of each task's working copy. It is found by its name rather than its number, so
// the store of the release before is the same whatever number a later merge gives it.
const taskCopiesMigration = "_task_copies.up.sql"

// An issue that runs at the release goes on in the working copy it has. The store is the one the
// release before task copies leaves: every embedded migration below taskCopiesMigration, applied by
// hand, with one issue of one task, a, at its step 2, testing, its tester's claim a launch that
// release began, written in that schema's columns (seedEarlierReleaseIssue). The release's migrations
// then run on it, and the whole daemon boots on it over the real Dispatch of this package's tests.
// After the tester's pass the task's row still holds no branch, so it reads the issue's own; the
// reviewer's task names the issue's own branch and directory, as every task of the issue did; no
// start's delivery and no adoption names a branch of its own; and nothing gave the task a branch:
// no name is given in plan_branches and no task_branch row is queued.
//
// The fault: an issue in flight moved to a new working copy at the release would leave its agent's
// edits behind in the one it worked (R20). The control is a mutation of the release's code, run by
// hand: record.PlanTask.BranchOf answering legion/<KEY>.default for a row that holds none; the
// reviewer's task then names that branch, and the check of its working copy line fails.
func TestAnIssueThatRunsAtTheReleaseKeepsItsWorkingCopy(t *testing.T) {
	t.Parallel()
	r := newCopiesUpgradeRig(t)
	ctx := context.Background()
	r.work(claim.RoleTester, "test", testCompletion, phase.Reviewing)
	text := r.worker(claim.RoleReviewer).takeTask()
	line := " Working copy: branch " + claim.IssueBranch(r.key) + ", directory "
	if !strings.Contains(text, line) {
		t.Errorf("the reviewer's task = %q, want it to name the issue's own working copy, %q", text, line)
	}
	if strings.Contains(text, claim.IssueBranch(r.key)+".") {
		t.Errorf("the reviewer's task = %q, want it to name no branch of a task of its own", text)
	}
	var branch string
	var given, queued, delivered int
	if err := r.st.Pool().QueryRow(ctx, `select (select branch from plan_tasks where issue = $1 and generation = 1 and task = 'a'),
		(select count(*) from plan_branches where issue = $1),
		(select count(*) from outbox where issue = $1 and kind = 'task_branch'),
		(select count(*) from pending_task_deliveries d join claims c on c.token = d.claim_token where c.issue = $1 and d.branch <> '')`,
		r.key).Scan(&branch, &given, &queued, &delivered); err != nil {
		t.Fatalf("read what the release gave task a of %s: %v", r.key, err)
	}
	if branch != "" || given != 0 || queued != 0 || delivered != 0 {
		t.Errorf("task a of %s holds the branch %q, %d names given, %d task_branch rows queued and %d deliveries naming a branch; want '' and none",
			r.key, branch, given, queued, delivered)
	}
	adopted := 0
	for _, call := range r.rt.CallsOf("AdoptWorkingCopy") {
		if strings.Contains(string(call.Locator.Claim), strings.ToLower(r.key)) {
			adopted++
			if call.Branch != "" {
				t.Errorf("the adoption of %s names the branch %q, want none: the issue's own working copy", call.Locator.Claim, call.Branch)
			}
		}
	}
	// The tester's and the reviewer's tasks each adopt the working copy: a check that read no
	// adoption would hold nothing of what the adoption names.
	if adopted == 0 {
		t.Errorf("no adoption of %s's claims was made, so nothing showed which working copy the steps adopted", r.key)
	}
}

// newCopiesUpgradeRig is the store the release before task copies leaves (storeBeforeTaskCopies),
// seeded with one issue of one task at testing (seedEarlierReleaseIssue), migrated to this release's
// schema, with the daemon booted on it at the real Dispatch of this package's tests and the issue's
// architect and tester resumed in the sessions the earlier daemon recorded.
func newCopiesUpgradeRig(t *testing.T) *earlierReleaseRig {
	t.Helper()
	proof := newLiveProofOn(t, workflowNATS(t))
	st, dsn, version := storeBeforeTaskCopies(t)
	proof.st, proof.cfg.PostgresDSN = st, dsn
	proof.cfg.Project = "TC" + randomSuffix(t)[:6]
	proof.cfg.Projects = map[string]config.Project{proof.cfg.Project: {}}
	proof.cfg.AdmissionCap = 4
	var err error
	if proof.project, err = claim.ProjectToken(proof.cfg.Project); err != nil {
		t.Fatal(err)
	}
	number, err := strconv.ParseInt(randomSuffix(t)[:6], 16, 64)
	if err != nil {
		t.Fatal(err)
	}
	r := &earlierReleaseRig{stepRecordsRig: &stepRecordsRig{liveProof: proof, dispatch: startScratchDispatchOn(t, scratchDispatchOptions{}),
		github: newStepRecordsGitHub(t), claims: map[claim.Token]*stepClaimant{}, waits: make(chan claim.Token, 16)}, migrationVersion: version}
	if err := os.WriteFile(r.cfg.DispatchTokenFile, []byte(r.dispatch.token+"\n"), 0o600); err != nil {
		t.Fatalf("write the Dispatch token file: %v", err)
	}
	r.cfg.DispatchURL = r.dispatch.base
	r.person(http.MethodPost, "/api/v1/projects", map[string]string{"key": r.cfg.Project, "name": "Task copies"}, http.StatusCreated, nil)
	r.person(http.MethodPut, "/api/v1/projects/"+r.cfg.Project+"/default-repository", map[string]any{"repo": liveRepo}, http.StatusOK, nil)
	const title = "Task copies issue continues"
	var created struct {
		Key     string `json:"key"`
		Rank    string `json:"rank"`
		LastSeq int64  `json:"last_seq"`
	}
	status := workflow.PhaseStatus(phase.Testing)
	r.person(http.MethodPost, "/api/v1/issues", map[string]string{"project": r.cfg.Project, "title": title}, http.StatusCreated, &created)
	r.person(http.MethodPatch, "/api/v1/issues/"+created.Key, map[string]any{"status": status, "labels": []string{"legion"}}, http.StatusOK, &created)
	r.issues = append(r.issues, walkIssue{key: created.Key, number: int(number)})
	r.on(created.Key)
	r.github.open("legion/"+r.key, r.number)
	sessions := r.seedEarlierReleaseIssue(title, status, created.Rank, created.LastSeq)
	if _, err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate the store the release before task copies left to this release's schema: %v", err)
	}
	r.boot()
	for _, role := range []claim.Role{claim.RoleArchitect, claim.RoleTester} {
		r.resume(role, sessions[role])
	}
	return r
}

// storeBeforeTaskCopies is a database of this test's own carrying every embedded migration whose
// version is below taskCopiesMigration's, applied by hand in version order (applyMigrationsByHand):
// the schema the release before task copies leaves. It answers the store, its URL and that
// migration's version.
func storeBeforeTaskCopies(t *testing.T) (*store.Store, string, int) {
	t.Helper()
	dsn := testdb.Database(t, "legion_task_copies")
	st, err := store.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open the store the release before task copies leaves: %v", err)
	}
	t.Cleanup(st.Close)
	all, err := migrations.All()
	if err != nil {
		t.Fatalf("read the embedded migrations: %v", err)
	}
	first := 0
	for _, migration := range all {
		if strings.HasSuffix(migration.Name, taskCopiesMigration) {
			first = migration.Version
		}
	}
	if first == 0 {
		t.Fatalf("no embedded migration ends %s, the first one of the release of each task's working copy", taskCopiesMigration)
	}
	var before []migrations.Migration
	for _, migration := range all {
		if migration.Version < first {
			before = append(before, migration)
		}
	}
	applyMigrationsByHand(t, st, before)
	return st, dsn, first
}

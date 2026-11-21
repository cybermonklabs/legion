package daemon

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/prompts"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// rerunChildKey is the child of the correction rig's live root that the test here parks and runs
// again, and rerunChildTitle and rerunChildRank are what Dispatch shows of it.
const (
	rerunChildKey   = "LEGION-401"
	rerunChildTitle = "the child"
	rerunChildRank  = "B"
)

// childStatusWrite applies the daemon's own write of the child's status, at seq and with the
// child resolved to resolved, the way api's recordOwnStatusWrite applies the issue Dispatch
// answered park_child's and rerun_child's write with: Dispatch's view of the issue
// (intake.ObservedIssue) carrying the event that write makes (intake.OwnStatusWrite) as Delivered,
// which ApplyFact applies in the view's place for an issue the workflow records, through the
// workflow then admission. The route prepares the fact first (its PrepareRepository), which for a
// recorded issue moving to another repository reads GitHub for it (prepareRecorded); the rig's
// reads stand in for that preparation, and ApplyFact carries the view's read onto the event it
// applies in the view's place (intake's settleObserved).
func (r *correctionRig) childStatusWrite(seq int64, status, resolved string) {
	r.t.Helper()
	parent := correctionKey
	written := dispatch.Issue{Key: rerunChildKey, Project: correctionProject, Title: rerunChildTitle, Status: status, Parent: &parent,
		Rank: rerunChildRank, LastSeq: seq, ResolvedRepo: resolved}
	event := intake.OwnStatusWrite(written)
	fact := r.prepared(intake.ObservedIssue(written))
	fact.Delivered = &event
	applyFact(r.t, r.pool, "api", fmt.Sprintf("status/own-write-%d", seq), fact, r.engine, r.admission)
}

// TestRepositoryCorrectionFollowsRerunChildIntoTheFirstLaunch is the launch half of api's
// TestRepositoryCorrectionFollowsRerunChild, which proves the record half: a child parked on
// acme/widgets that Dispatch moves to acme/tools while it is parked, then run again by
// rerun_child, starts its next run (generation 2) on acme/tools, with acme/widgets kept as the
// repository it left. This proves that the first worker launch of that run, the planner's, is
// addressed so: its task names acme/tools at its default branch as the issue's repository, says
// the daemon changed the repository from acme/widgets and when, and says the work is on
// acme/tools now; and the launch itself works acme/tools at release/v7.1.
//
// The park and the rerun are each the route's own status write applied as the route applies it
// (childStatusWrite: intake.ObservedIssue of the issue Dispatch answered, with
// intake.OwnStatusWrite of it as Delivered), through the rig's engine then admission, the
// handler pair the api applies a fact through. The launch is built by the daemon's own specs over
// the workflow's own launch read (workflowRuntime.issueLaunchRepository) on the same database, as
// newLeftPullRequestRig builds it, so nothing between the record and the task is faked.
func TestRepositoryCorrectionFollowsRerunChildIntoTheFirstLaunch(t *testing.T) {
	ctx := context.Background()
	r := newCorrectionRig(t)
	// The api harness's tree: the project admits two issues, the root holds one slot and its spec
	// gate is open, so the child's next run is offered the other slot and the promotion in the
	// rerun's own fact starts its planner, as the api test reads the rerun child planning.
	r.admission = admit.New(r.records, r.engine, 2, correctionProject, quietLogger())
	root := correctionKey
	if err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		if err := r.records.PutGate(ctx, tx, openSpecGate(correctionKey, 1)); err != nil {
			return err
		}
		return r.records.PutIssue(ctx, tx, record.Issue{Key: rerunChildKey, Tree: correctionKey, Parent: &root, Project: correctionProject,
			Title: rerunChildTitle, Phase: phase.Planning, Generation: 1, Status: "in_progress", DispatchStatus: "in_progress", Rank: rerunChildRank,
			LastDispatchSeq: 2, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets"})
	}); err != nil {
		t.Fatalf("record the child of %s: %v", correctionKey, err)
	}

	// park_child writes backlog while Dispatch still resolves the child to acme/widgets.
	r.childStatusWrite(3, "backlog", "acme/widgets")
	parked := recordOf(t, r.pool, r.records, rerunChildKey)
	if parked.Phase != phase.Done || parked.Status != "backlog" || parked.Generation != 1 || parked.Repo != "acme/widgets" || parked.RepoCorrection != nil {
		t.Fatalf("the parked child = %#v, want it out of the workflow at generation 1, backlog, on acme/widgets and corrected in nothing", parked)
	}

	// Dispatch moves the child to acme/tools while it is parked, so the issue Dispatch answers
	// rerun_child's todo with resolves to acme/tools.
	r.childStatusWrite(4, "todo", "acme/tools")
	rerun := recordOf(t, r.pool, r.records, rerunChildKey)
	r.correctedOn(rerun, "acme/tools", "release/v7.1", "acme/tools", "acme/widgets")
	if rerun.Generation != 2 || rerun.Phase != phase.Planning {
		t.Fatalf("the rerun child is at generation %d in %s, want its next run (generation 2) planning on the corrected repository",
			rerun.Generation, rerun.Phase)
	}
	planners := 0
	for _, start := range sweptStarts(t, r.pool, rerunChildKey) {
		if start.role == claim.RolePlanner && start.task != "" {
			planners++
		}
	}
	if planners != 1 {
		t.Fatalf("the rerun child holds %d planner starts carrying a task, want the one its promotion queued: the planner is its first worker", planners)
	}

	// The planner's launch, built as the daemon builds it from the record as it stands now.
	composer, err := prompts.New(t.TempDir())
	if err != nil {
		t.Fatalf("compose the shipped prompts: %v", err)
	}
	const project = "legion"
	w := launchReadRuntime(r.pool, r.records, quietLogger(), "acme/widgets", "acme/tools")
	launches := specs{
		stateDir: t.TempDir(), project: project, prompts: composer,
		repository: w.issueLaunchRepository,
		appOwners:  func(claim.Role) []string { return []string{"acme"} },
	}
	token, err := claim.NewToken(project, rerunChildKey, claim.RolePlanner)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := launches.SpawnSpec(ctx, supervise.Claim{Token: token, Issue: rerunChildKey, Tree: correctionKey, Role: claim.RolePlanner})
	if err != nil {
		t.Fatalf("the launch of %s: %v", token, err)
	}
	addressing := spec.Prompt.Addressing
	for _, want := range []string{
		"Your issue's repository is acme/tools (default branch release/v7.1);",
		"The daemon changed your issue's repository from acme/widgets at 2026-11-12T09:30:00Z.",
		"Your issue's branch and your issue's pull request are on acme/widgets.",
		"Your work on this issue is on acme/tools now.",
	} {
		if !strings.Contains(addressing, want) {
			t.Errorf("the first launch of the rerun child is told %q, want it to say %q", addressing, want)
		}
	}
	if spec.Repository.String() != "acme/tools" || spec.DefaultBranch != "release/v7.1" {
		t.Errorf("the first launch of the rerun child works %s at %s, want acme/tools at release/v7.1", spec.Repository, spec.DefaultBranch)
	}
}

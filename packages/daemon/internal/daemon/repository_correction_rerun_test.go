package daemon

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/prompts"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// rerunChildKey is the child of the correction rig's live root that the test here takes out of the
// workflow and runs again, and rerunChildTitle and rerunChildRank are what Dispatch shows of it.
const (
	rerunChildKey   = "LEGION-401"
	rerunChildTitle = "the child"
	rerunChildRank  = "B"
)

// childEcho applies Dispatch's event of the daemon's own write of the child's status, at seq and
// with the child resolved to resolved: the echo of the todo rerun_child queued as a status row
// (the workflow's statusCall), which the outbox writes and Dispatch answers with an event of its
// own, carrying the issue as Dispatch holds it, resolved repository and all. It is prepared as the
// Dispatch consumer prepares a recorded issue's event (prepareRecorded): the rig's reads stand in
// for GitHub's answer for the repository it moves onto. It goes through the workflow then
// admission, in one fact.
func (r *correctionRig) childEcho(seq int64, status, resolved string) {
	r.t.Helper()
	applyFact(r.t, r.pool, "dispatch", fmt.Sprintf("child-echo-%d", seq), r.prepared(intake.DispatchIssue{Key: rerunChildKey, Seq: seq,
		Type: "issue.updated", Status: status, Title: rerunChildTitle, Parent: correctionKey, Rank: rerunChildRank, ResolvedRepo: resolved,
		DaemonActor: true, ClaimKnown: true}), r.engine, r.admission)
}

// TestRepositoryCorrectionFollowsRerunChildIntoTheFirstLaunch is the launch half of api's
// TestRepositoryCorrectionFollowsRerunChild, which proves the record half: a child out of the
// workflow on acme/widgets that Dispatch moves to acme/tools meanwhile, then run again by
// rerun_child. rerun_child queues the child's todo and opens its next run (generation 2) on its
// record from the record alone (decision 5 of dispatch://INFRA-4730, the ruling
// dispatch://INFRA-4730/comment/66d1dd6a-b428-4876-8e24-e3d3cd30bc2d), so until Dispatch echoes the
// write that run stands on acme/widgets with no correction. The echo carries Dispatch's
// resolution, and the record follows it onto acme/tools (workflow.Engine.ApplyRepositoryChange),
// keeping acme/widgets as the repository it left. A pod built for that run before the echo is on
// the repository the record has left, so the update round replaces it (repositoryDrift,
// repositoryReason). And the first worker launch after the echo, the planner's, is addressed with
// both repositories: its task names acme/tools at its default branch as the issue's repository,
// says the daemon changed the repository from acme/widgets and when, says the branch and the pull
// request are on acme/widgets and that the work is on acme/tools now; and the launch itself works
// acme/tools at release/v7.1.
//
// The child leaves the workflow by a backlog written in Dispatch, delivered as the event consumer
// delivers a move with no session (intake.DispatchIssue): the owning architect's own backlog is
// taken as that move is (workflow's owningArchitectsMove, dispatch://INFRA-3178). The rerun is the
// route's own status call (intake.StatusCall, intake.RerunChildCall) from the root's architect,
// whose issue stands above the child on the Dispatch chain (record.ArchitectAbove), and the echo is
// childEcho. Each goes through the rig's engine then admission, the handler pair the api applies a
// fact through. The copy of Dispatch's issues holds the child under the root at each status
// Dispatch shows, which the call reads its chain and its status from. The launch is built by the
// daemon's own specs over the workflow's own launch read (workflowRuntime.issueLaunchRepository)
// on the same database, as newLeftPullRequestRig builds it, so nothing between the record and the
// task is faked.
func TestRepositoryCorrectionFollowsRerunChildIntoTheFirstLaunch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newCorrectionRig(t)
	// The api harness's tree: the project admits two issues, the root holds one slot and its spec
	// gate is open, so the child's next run is offered the other slot and the promotion in the
	// rerun's own fact starts its planner, as the api test reads the rerun child planning.
	r.admission = admit.New(r.records, r.engine, 2, correctionProject, quietLogger())
	root := correctionKey
	if err := store.Transact(ctx, r.pool, func(ctx context.Context, tx pgx.Tx) error {
		if err := r.records.PutGate(ctx, tx, openSpecGate(correctionKey, 1)); err != nil {
			return err
		}
		if err := r.records.PutIssue(ctx, tx, record.Issue{Key: rerunChildKey, Tree: correctionKey, Parent: &root, Project: correctionProject,
			Title: rerunChildTitle, Phase: phase.Planning, Generation: 1, Status: "in_progress", DispatchStatus: "in_progress", Rank: rerunChildRank,
			LastDispatchSeq: 2, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets"}); err != nil {
			return err
		}
		seedCopyRow(t, tx, rerunChildKey, correctionKey, "in_progress", 2)
		return nil
	}); err != nil {
		t.Fatalf("record the child of %s: %v", correctionKey, err)
	}

	// The child leaves the workflow by a backlog in Dispatch while Dispatch still resolves it to
	// acme/widgets.
	applyFact(t, r.pool, "dispatch", "child-backlog-3", r.prepared(intake.DispatchIssue{Key: rerunChildKey, Seq: 3, Type: "issue.updated",
		Status: "backlog", Title: rerunChildTitle, Parent: correctionKey, Rank: rerunChildRank, ResolvedRepo: "acme/widgets"}), r.engine, r.admission)
	putCopyRow(t, r.pool, rerunChildKey, correctionKey, "backlog")
	out := recordOf(t, r.pool, r.records, rerunChildKey)
	if record.InLegionsWork(out) || out.Status != "backlog" || out.Generation != 1 || out.Repo != "acme/widgets" || out.RepoCorrection != nil {
		t.Fatalf("the child out of the workflow = %#v, want it at generation 1, backlog, out of Legion's work, on acme/widgets and corrected in nothing", out)
	}

	// rerun_child queues the child's todo and opens its next run from the record alone: before
	// Dispatch echoes the write, that run stands on acme/widgets, the repository it left nothing
	// of yet.
	const project = "legion"
	architect, err := claim.NewToken(project, correctionKey, claim.RoleArchitect)
	if err != nil {
		t.Fatal(err)
	}
	applyFact(t, r.pool, "api", "rerun-child-4", intake.StatusCall{Op: intake.RerunChildCall, Architect: architect,
		Issues: []intake.StatusCallIssue{{Key: rerunChildKey, Status: "todo"}}}, r.engine, r.admission)
	opened := recordOf(t, r.pool, r.records, rerunChildKey)
	if opened.Generation != 2 || opened.Repo != "acme/widgets" || opened.GitHubRepo != "acme/widgets" || opened.RepoCorrection != nil || opened.RepoChange != nil {
		t.Fatalf("the child's next run before the todo's echo = %#v, want generation 2 on acme/widgets, corrected in nothing and with no change waiting", opened)
	}
	w := launchReadRuntime(r.pool, r.records, quietLogger(), "acme/widgets", "acme/tools")
	rounds := &supervision{log: quietLogger(), plan: plan{recordedRepository: recordedRepositoryOf(w.recordedIssue)}}
	if _, reason := rounds.repositoryDrift(ctx, rerunChildKey, "acme/widgets"); reason != "" {
		t.Fatalf("a pod built from acme/widgets before the echo drifts by %q, want none: the record has moved nowhere yet", reason)
	}

	// The outbox writes the queued todo, and Dispatch's event of that write carries the child
	// resolved to acme/tools, where Dispatch moved it while it was out of the workflow.
	r.childEcho(4, "todo", "acme/tools")
	rerun := recordOf(t, r.pool, r.records, rerunChildKey)
	r.correctedOn(rerun, "acme/tools", "release/v7.1", "acme/tools", "acme/widgets")
	if rerun.Generation != 2 || rerun.Phase != phase.Planning {
		t.Fatalf("the rerun child is at generation %d in %s, want the run the rerun opened (generation 2) planning on the corrected repository",
			rerun.Generation, rerun.Phase)
	}
	// The pod of that run, built from acme/widgets, is on the repository the record has left now,
	// which is the drift the update round replaces a pod over.
	if _, reason := rounds.repositoryDrift(ctx, rerunChildKey, "acme/widgets"); reason == "" {
		t.Fatal("a pod built from acme/widgets after the echo drifts by nothing, want the repository drift that replaces it")
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

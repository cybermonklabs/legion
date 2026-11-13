package daemon

// A corrected repository and the pod the issue runs in: Dispatch moved the issue to another
// repository, the daemon's record followed it (record.Store.CorrectIssueRepository), and the pod
// still holds the clone, the branch and the gh owner directories of the repository the issue has
// left. These tests drive the same supervision the rest of the update-round tests drive
// (podupdate_test.go's rig), with the runtime answering what the pod was built from
// (runtime.Runtime.PodRepository), and read what the sweep then did: the round it opened, the
// replacement it made, the launches that followed it, and what the issue was told.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

const (
	// podUpdateCorrectedRepo is the repository Dispatch moved the issue to, and
	// podUpdateCorrectedBranch that repository's default branch, which the next launch provisions
	// the issue's workspace from.
	podUpdateCorrectedRepo   = "zeta/tools"
	podUpdateCorrectedBranch = "release/v9"
	// podUpdateRepositoryReason is the one reason a corrected repository adds to a round: the
	// repository the pod was built from, then the repository the record holds now.
	podUpdateRepositoryReason = "repository " + podUpdateRepo + ", now " + podUpdateCorrectedRepo
)

// recordSpecs is the rig's launch spec: the repository the issue's record names now and that
// repository's default branch, which is what the daemon's own specs carries into every launch
// (specs.workerSpawnSpec, over workflowRuntime.issueRepository) and what a pod is built from —
// the clone, the branch it checks out and the gh owner directories of that repository's owner.
// Everything else a launch carries is another test's business, so nothing else is built here.
type recordSpecs struct {
	read func(ctx context.Context, key string) (*record.Issue, error)
}

func (s recordSpecs) SpawnSpec(ctx context.Context, c supervise.Claim) (runtime.SpawnSpec, error) {
	recorded, err := s.read(ctx, c.Issue)
	if err != nil {
		return runtime.SpawnSpec{}, fmt.Errorf("the record of %s: %w", c.Issue, err)
	}
	if recorded == nil {
		return runtime.SpawnSpec{}, nil
	}
	repo, err := recorded.Repository()
	if err != nil {
		return runtime.SpawnSpec{}, err
	}
	return runtime.SpawnSpec{Repository: repo, DefaultBranch: recorded.DefaultBranch}, nil
}

// corrects moves the issue's record onto repo at branch, as the workflow's own pass does when
// Dispatch names another repository for the issue (workflow.(*Engine).ApplyRepositoryChange).
func (r *podUpdateRig) corrects(repo, branch string) {
	r.t.Helper()
	correctRecord(r.t, r.pool, r.records, podUpdateIssue, repo, branch, r.clock.Now())
}

// podRepository scripts what every claim's pod was built from, which is one repository for the
// whole group: every role of an issue runs in that issue's one pod.
func (r *podUpdateRig) podRepository(repo string) {
	for _, token := range r.tokens {
		r.rt.ScriptPodRepository(token, repo)
	}
}

// The round a correction opens: the pod was built from the repository the issue has left, the
// record holds the one Dispatch moved it to, and the round names both — the pod's first, because
// that is what the workers in it are still working, and the record's after it, because that is
// what the pod replacing it will be built from. Nothing else of the pod has drifted, so the
// reason the correction adds is the whole of the round's reasons.
func TestRepositoryCorrectionOpensAnUpdateRoundNamingBothRepositories(t *testing.T) {
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester))
	rig.corrects(podUpdateCorrectedRepo, podUpdateCorrectedBranch)

	rig.tick()

	for _, role := range []claim.Role{claim.RoleImplementer, claim.RoleTester} {
		c := rig.awaitRound(role, podUpdateRound)
		if c.UpdateReasons != podUpdateRepositoryReason {
			t.Errorf("%s UpdateReasons = %q, want %q", role, c.UpdateReasons, podUpdateRepositoryReason)
		}
		if c.Pending == nil || !strings.HasPrefix(c.Pending.ID, "update:"+podUpdateRound+":") {
			t.Errorf("%s pending = %+v, want the round's task", role, c.Pending)
		}
	}
}

// The whole of the slice, end to end: a corrected record opens one round, every role answers it,
// the pod is replaced, and each role comes back in the pod that replaced it — on its own session,
// in a launch built from the repository the record holds now. The launch is what carries the
// correction into the new pod: the clone it provisions, the branch it checks out, and the owner
// whose gh directories and token the pod holds all come from that one repository.
func TestRepositoryCorrectionReplacesThePodAndRelaunchesEveryRoleOnItsSession(t *testing.T) {
	roles := []claim.Role{claim.RoleImplementer, claim.RoleTester}
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester))
	sessions := map[claim.Role]string{}
	for _, role := range roles {
		sessions[role] = rig.claimOf(role).Session
	}
	rig.corrects(podUpdateCorrectedRepo, podUpdateCorrectedBranch)

	rig.tick()
	for _, role := range roles {
		rig.awaitRound(role, podUpdateRound)
		rig.signal(role, "")
	}
	calls := rig.awaitReplacement(1, "once every role answered the correction's round")
	if calls[0].Pod.Locator == nil || calls[0].Pod.Locator.Sandbox.PodUID != podUpdatePod {
		t.Fatalf("ReplacePod called with %+v, want a process of pod %s", calls[0].Pod, podUpdatePod)
	}

	// The pod the replacement leaves in its stead is built from the record, which is what the
	// runtime answers for it from here on.
	rig.podRepository(podUpdateCorrectedRepo)
	for _, role := range roles {
		rig.displacedInto(role, podUpdateReplacementPod)
	}
	for _, role := range roles {
		c := rig.claimOf(role)
		if c.Session != sessions[role] {
			t.Errorf("the %s came back on session %q, want its own %q: a replacement keeps every role's session", role, c.Session, sessions[role])
		}
	}
	for _, call := range rig.rt.CallsOf("Resume") {
		if call.Spec.Repository.String() != podUpdateCorrectedRepo || call.Spec.DefaultBranch != podUpdateCorrectedBranch {
			t.Errorf("the relaunch of %s works %s at %s, want %s at %s: the launch after the replacement is built from the record",
				call.Spec.Claim, call.Spec.Repository, call.Spec.DefaultBranch, podUpdateCorrectedRepo, podUpdateCorrectedBranch)
		}
		if call.Spec.ResumeSessionFile == "" || call.Previous == nil {
			t.Errorf("the %s was started afresh (session file %q, previous %+v), want it resumed on its recorded session",
				call.Spec.Claim, call.Spec.ResumeSessionFile, call.Previous)
		}
	}
	if len(rig.rt.CallsOf("Spawn")) != 0 {
		t.Errorf("the replacement started %d fresh agents, want none: every role keeps its session", len(rig.rt.CallsOf("Spawn")))
	}

	// The pod and the record agree again, so the sweep after the replacement opens nothing.
	rig.tick()
	for _, role := range roles {
		if c := rig.claimOf(role); c.UpdateRound != "" {
			t.Errorf("the %s is in round %q after its pod was replaced, want none", role, c.UpdateRound)
		}
	}
	rig.awaitReplacement(1, "after the pod and the record agree again")
}

// One correction is one pod's, not one claim's: the repository is read once for the group,
// through the first claim that records a process, and the round it opens is the one round over
// that pod. Read per claim, a group of three would ask the runtime and the record three times for
// one answer.
func TestRepositoryCorrectionOpensOneRoundForTheWholeGroup(t *testing.T) {
	roles := []claim.Role{claim.RoleImplementer, claim.RoleTester, claim.RoleReviewer}
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester), readyWorker(claim.RoleReviewer))
	rig.corrects(podUpdateCorrectedRepo, podUpdateCorrectedBranch)

	rig.tick()

	for _, role := range roles {
		c := rig.awaitRound(role, podUpdateRound)
		if c.UpdateReasons != podUpdateRepositoryReason {
			t.Errorf("%s UpdateReasons = %q, want the one reason %q", role, c.UpdateReasons, podUpdateRepositoryReason)
		}
	}
	if asked := rig.rt.CallsOf("PodRepository"); len(asked) != 1 {
		t.Errorf("the sweep asked what the pod was built from %d times, want once for the group: every role runs in the one pod", len(asked))
	}
	if asked := rig.rt.CallsOf("PodDrift"); len(asked) != len(roles) {
		t.Errorf("the sweep asked PodDrift %d times, want %d: that reason is read per claim", len(asked), len(roles))
	}
}

// Nothing but two repository names that are both there and differ is a reason. The record and the
// pod naming one repository — the same name, the same name in GitHub's capitals, or the two names
// of a configured rename — is no correction at all, and an unknown is never a statement that
// something drifted: a pod that answers no repository and a runtime that cannot be asked each add
// nothing, and no round opens over either.
func TestRepositoryCorrectionAddsNoReasonWhereNothingDisagrees(t *testing.T) {
	for name, tc := range map[string]struct {
		pod      string
		recorded string
		renames  ghrepo.Renames
		unread   bool
	}{
		"the record and the pod on one name": {
			pod: podUpdateRepo,
		},
		"the pod naming it in GitHub's capitals": {
			pod: "Acme/Widgets",
		},
		"the record on the other name of a configured rename": {
			pod:      podUpdateRepo,
			recorded: "zeta/widgets",
			renames:  ghrepo.MustRenames(ghrepo.MustRename(podUpdateRepo, "zeta/widgets")),
		},
		"a pod that was built from no repository": {
			pod:      "",
			recorded: podUpdateCorrectedRepo,
		},
		"a runtime that cannot be asked what the pod holds": {
			pod:      podUpdateRepo,
			recorded: podUpdateCorrectedRepo,
			unread:   true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester))
			if !tc.renames.IsZero() {
				rig.s.plan.repositories = newRepositories("LEGION", tc.renames)
			}
			if tc.recorded != "" {
				rig.corrects(tc.recorded, podUpdateCorrectedBranch)
			}
			rig.podRepository(tc.pod)
			if tc.unread {
				rig.rt.FailPodRepository(errors.New("the cluster did not answer for this pod"))
			}

			rig.tick()

			for _, role := range []claim.Role{claim.RoleImplementer, claim.RoleTester} {
				if c := rig.claimOf(role); c.UpdateRound != "" {
					t.Errorf("the %s is in round %q for reasons %q, want no round at all", role, c.UpdateRound, c.UpdateReasons)
				}
			}
			if calls := rig.replacements(); len(calls) != 0 {
				t.Fatalf("the pod was replaced %d times, want never", len(calls))
			}
		})
	}
}

// The rescue branches are in the repository the pod pushed to, which after a correction is the
// repository the issue has left: the push check runs inside the pod, against the remote the pod's
// own clone has. The message names that repository, so the agent that reads it does not look for
// the branches in the repository the issue is recorded on now, which holds none of them.
func TestRepositoryCorrectionNamesTheRepositoryTheRescueBranchesAreIn(t *testing.T) {
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer))
	branch := rescueBranch(podUpdateIssue, podUpdatePod) + "-1"
	rig.rt.RescueWith(func(context.Context, runtime.Known, runtime.RescueRequest) ([]runtime.WorkspaceRescue, error) {
		return []runtime.WorkspaceRescue{{
			Dir: "/workspaces/" + podUpdateIssue, Status: runtime.RescueRescued,
			Branches: []runtime.RescuedBranch{{Name: branch, Commit: "a1b2c3d"}},
		}}, nil
	})
	rig.corrects(podUpdateCorrectedRepo, podUpdateCorrectedBranch)

	rig.tick()
	rig.awaitRound(claim.RoleImplementer, podUpdateRound)
	rig.signal(claim.RoleImplementer, "")
	rig.awaitReplacement(1, "once the one worker answered the correction's round")

	messages := rig.messages()
	if len(messages) != 1 {
		t.Fatalf("the issue was told %q, want one message naming the rescue branch", messages)
	}
	where := "The branches are in the repository `" + podUpdateRepo + "`."
	if !strings.Contains(messages[0], where) {
		t.Errorf("the issue was told %q, want it to say %q", messages[0], where)
	}
	if !strings.Contains(messages[0], "`"+branch+"` (commit a1b2c3d)") {
		t.Errorf("the issue was told %q, want it to name the branch %s at a1b2c3d", messages[0], branch)
	}
	if strings.Contains(messages[0], podUpdateCorrectedRepo) {
		t.Errorf("the issue was told %q, which names %s: the branches are not in the repository the issue moved to", messages[0], podUpdateCorrectedRepo)
	}
}

// The pod of an issue is asked its repository through one claim, the one that records a process
// and has the lowest token (podRepository), whatever order the claims come in. The idle rule
// reads them in the order the daemon supervises them (issuePodRepository) and the update-round
// sweep in the order the store answered them (supervision.groupDrift), and the two must ask the
// same claim: a release that held a pod read through one claim, and a sweep that replaced it
// read through another, would disagree whenever the two answers differ. The two claims here
// answer two repositories so that a read through the wrong one shows.
func TestThePodsRepositoryIsReadThroughTheSameClaimInAnyOrder(t *testing.T) {
	rt := fake.NewRuntime()
	architect := claim.Token(podUpdateIssue + "-architect")
	implementer := claim.Token(podUpdateIssue + "-implementer")
	reviewer := claim.Token(podUpdateIssue + "-reviewer")
	rt.ScriptPodRepository(implementer, podUpdateRepo)
	rt.ScriptPodRepository(reviewer, podUpdateCorrectedRepo)
	claims := []supervise.Claim{
		{Token: reviewer, Issue: podUpdateIssue, Locator: podUpdateLocator(reviewer, podUpdateIssue, "pod-1")},
		// The lowest token of the three, and no process: a suspended claim answers for no pod.
		{Token: architect, Issue: podUpdateIssue},
		{Token: implementer, Issue: podUpdateIssue, Locator: podUpdateLocator(implementer, podUpdateIssue, "pod-1")},
	}
	reversed := slices.Clone(claims)
	slices.Reverse(reversed)
	for _, order := range [][]supervise.Claim{claims, reversed} {
		got, err := podRepository(context.Background(), rt, slices.Values(order))
		if err != nil || got != podUpdateRepo {
			t.Fatalf("the pod read through claims in the order %v answered %q (%v), want %s through %s, the lowest token that records a process",
				tokensOf(order), got, err, podUpdateRepo, implementer)
		}
	}
}

// tokensOf is each claim's token, in the claims' order.
func tokensOf(claims []supervise.Claim) []claim.Token {
	tokens := make([]claim.Token, 0, len(claims))
	for _, c := range claims {
		tokens = append(tokens, c.Token)
	}
	return tokens
}

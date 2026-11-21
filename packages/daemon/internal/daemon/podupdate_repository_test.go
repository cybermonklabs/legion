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

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghbranch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
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
	}
	// The round asks each role over Envoy, at the sweep after its post lands on the claims
	// (unaskedClaims), and queues no task: the ask is what tells a role the round's reasons, so it
	// is what has to name both repositories.
	for _, role := range []claim.Role{claim.RoleImplementer, claim.RoleTester} {
		rig.awaitAsked(role, podUpdateRound)
	}
	text := supervise.UpdateTask(podUpdateRound, podUpdateRepositoryReason, podUpdateStart.Add(podUpdateGrace))
	asked := map[claim.Role]int{}
	for _, ask := range rig.asks() {
		asked[ask.Role]++
		if ask.Round != podUpdateRound || ask.Text != text {
			t.Errorf("the ask to the %s = %+v, want round %s with the text:\n%s", ask.Role, ask, podUpdateRound, text)
		}
		if !strings.Contains(ask.Text, podUpdateRepo) || !strings.Contains(ask.Text, podUpdateCorrectedRepo) {
			t.Errorf("the ask to the %s says %q, want it to name both %s and %s", ask.Role, ask.Text, podUpdateRepo, podUpdateCorrectedRepo)
		}
	}
	for _, role := range []claim.Role{claim.RoleImplementer, claim.RoleTester} {
		if asked[role] != 1 {
			t.Errorf("the %s was asked %d times, want once", role, asked[role])
		}
		if p := rig.claimOf(role).Pending; p != nil {
			t.Errorf("the %s holds %+v, want no task queued for the round", role, p)
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

// The rename pass and the correction are two writers of a record's repository that cannot undo
// each other, and neither the pass nor the event after it replaces a pod. The pass writes a
// configured pair's `to` over the `from` the record held (record.RenameIssueRepository); Dispatch
// still resolves the issue to the `from` until its own repository moves (dispatch://INFRA-4412),
// so the issue's next event names the `from`, whose preparation read GitHub for it. The event
// reads the pair as one repository (the engine's sameRepository) and corrects nothing, and the pod
// built from the `from` reads as on the record's repository (repositories.left), so no round
// opens and no pod is replaced.
func TestTheRenamePassAndTheEventAfterItNamingTheOldNameCorrectNothingAndReplaceNoPod(t *testing.T) {
	const renamedTo = "zeta/widgets"
	renames := ghrepo.MustRenames(ghrepo.MustRename(podUpdateRepo, renamedTo))
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester))
	rig.s.plan.repositories = newRepositories("LEGION", renames)
	ctx := context.Background()
	if err := pgx.BeginFunc(ctx, rig.pool, func(tx pgx.Tx) error {
		_, err := record.RenameIssueRepository(ctx, tx, podUpdateIssue, podUpdateRepo, renamedTo)
		return err
	}); err != nil {
		t.Fatalf("the rename pass's write of %s: %v", podUpdateIssue, err)
	}
	rig.podRepository(podUpdateRepo)

	engine := workflow.New(rig.records, workflow.Config{Project: "LEGION", Repositories: testRepositories, RepositoryRenames: renames,
		Steps: config.DesignGateOff.Steps(), Clock: rig.clock.Now}, quietLogger())
	stillOld := intake.DispatchIssue{Key: podUpdateIssue, ResolvedRepo: podUpdateRepo,
		RepositoryRead: &ghbranch.Repository{Name: ghrepo.MustParse(podUpdateRepo), DefaultBranch: "main"}}
	if err := pgx.BeginFunc(ctx, rig.pool, func(tx pgx.Tx) error {
		stored, err := rig.records.Issue(ctx, tx, podUpdateIssue)
		if err != nil {
			return err
		}
		return engine.ApplyRepositoryChange(ctx, tx, *stored, stillOld)
	}); err != nil {
		t.Fatalf("the event naming %s after the rename pass: %v", podUpdateRepo, err)
	}
	got := recordOf(t, rig.pool, rig.records, podUpdateIssue)
	if got.Repo != renamedTo || got.RepoCorrection != nil || got.RepoChange != nil {
		t.Fatalf("%s = %+v, want it on %s with no correction and nothing waiting: the pair is one repository", podUpdateIssue, got, renamedTo)
	}

	rig.tick()
	for _, role := range []claim.Role{claim.RoleImplementer, claim.RoleTester} {
		if c := rig.claimOf(role); c.UpdateRound != "" {
			t.Errorf("the %s is in round %q for reasons %q, want no round: the pod is on the record's repository under its old name",
				role, c.UpdateRound, c.UpdateReasons)
		}
	}
	if calls := rig.replacements(); len(calls) != 0 {
		t.Fatalf("the pod was replaced %d times, want never", len(calls))
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

// A gone pod's message names the old repository's workspace only on a round whose reasons end in
// the reason repositoryDrift writes (podWentMessage). Every other text reads as no repository
// round: no reason at all, the runtime's reasons alone, the reason's words inside another reason
// or before one, and a reason whose names are not two repositories.
func TestTheRepositoryReasonReadsBackOnlyAsItIsWritten(t *testing.T) {
	written := repositoryReason(podUpdateRepo, podUpdateCorrectedRepo)
	for _, tc := range []struct {
		reasons string
		want    bool
	}{
		{written, true},
		{podUpdateDrift + ", " + written, true},
		{"", false},
		{podUpdateDrift, false},
		{podUpdateDrift + " " + written, false},
		{written + ", " + podUpdateDrift, false},
		{"repository " + podUpdateRepo, false},
		{"repository acme widgets, now " + podUpdateCorrectedRepo, false},
	} {
		pod, recorded, ok := roundRepositories(tc.reasons)
		if ok != tc.want {
			t.Errorf("roundRepositories(%q) reads a repository round: %t, want %t",
				tc.reasons, ok, tc.want)
			continue
		}
		if ok && (pod.String() != podUpdateRepo || recorded.String() != podUpdateCorrectedRepo) {
			t.Errorf("roundRepositories(%q) = %s, %s, want %s, %s",
				tc.reasons, pod, recorded, podUpdateRepo, podUpdateCorrectedRepo)
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

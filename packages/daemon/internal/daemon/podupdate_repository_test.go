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
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghbranch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
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
	// repository the pod was built from, then the repository the record holds now, behind the mark
	// of a drift that breaks the pod (repositoryReason, runtime.BreakingDrift).
	podUpdateRepositoryReason = runtime.BreakingDrift + "repository " + podUpdateRepo + ", now " + podUpdateCorrectedRepo
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
	t.Parallel()
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
	asked := map[claim.Role]int{}
	for _, ask := range rig.asks() {
		asked[ask.Role]++
		text := supervise.UpdateTask(ask.Role, podUpdateRound, podUpdateRepositoryReason, podUpdateStart.Add(podUpdateGrace))
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	const renamedTo = "zeta/widgets"
	renames := ghrepo.MustRenames(ghrepo.MustRename(podUpdateRepo, renamedTo))
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester))
	rig.s.plan.repositories = newRepositories("LEGION", renames)
	ctx := context.Background()
	if err := store.Transact(ctx, rig.pool, func(ctx context.Context, tx pgx.Tx) error {
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
	if err := store.Transact(ctx, rig.pool, func(ctx context.Context, tx pgx.Tx) error {
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
	t.Parallel()
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

// A gone pod's message reads the repository the round's pod was built from off the round itself
// (supervise.Claim.UpdateRepository), never out of the round's reasons, which are text for a
// reader. The round here was opened over a pod of podUpdateRepo, and its reasons name only the
// image that the pod drifted on as well, while the record has moved to podUpdateCorrectedRepo. The
// message still names the old repository's workspace and its issue branch, beside the repository
// the record holds.
func TestAPodGoneOnARepositoryRoundReadsThePodsRepositoryOffTheRound(t *testing.T) {
	t.Parallel()
	roles := []podUpdateRole{readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester)}
	for i := range roles {
		roles[i].round, roles[i].reasons, roles[i].repository = podUpdateRound, podUpdateDrift, podUpdateRepo
	}
	roles[1].signalled = true
	rig := newPodUpdateRig(t, roles...)
	rig.corrects(podUpdateCorrectedRepo, podUpdateCorrectedBranch)
	rig.rt.ScriptKeptVolume(podUpdateIssue, podUpdateStart.Add(-time.Hour))

	rig.tickBackIn(podUpdateReplacementPod)

	messages := rig.messages()
	if len(messages) != 1 {
		t.Fatalf("the issue was written on %d times, want once for the pod that went", len(messages))
	}
	want := implementerLostWith(podUpdateDrift,
		keptVolumeMovedTo(podUpdateCorrectedRepo), pushedMovedTo(podUpdateCorrectedRepo))
	if messages[0] != want {
		t.Fatalf("the message on the issue =\n%s\nwant\n%s", messages[0], want)
	}
}

// A gone pod's message names the repository the round's pod was built from and the repository
// the issue's record holds when the message is written, never the one the record held at the
// round's open. Here Dispatch moves the issue twice: the first correction opens the round over the
// pod of podUpdateRepo, and the second moves the record on while the round is open. The round's
// reasons keep the record as it stood at the open, since a round's terms are fixed then, but the
// next pod is built from the record as it stands now. So after a second move onto a third
// repository the message names the old workspace and issue branch of podUpdateRepo, the pod's
// own, and the third repository as the new pod's origin, and names podUpdateCorrectedRepo only in
// the reasons. A second move back onto podUpdateRepo is no move at all: the next pod is built from
// the repository the gone pod was, so the message is the one of an issue that never moved, but
// for its reasons.
func TestAPodGoneAfterASecondCorrectionNamesThePodsRepositoryAndTheRecordsNow(t *testing.T) {
	t.Parallel()
	const third = "zeta/gadgets"
	for _, tc := range []struct {
		name string
		// again and branch are the repository the second correction moves the record onto and
		// that repository's default branch.
		again, branch string
		disk, pushed  string
	}{
		{"a second move onto a third repository", third, "release/v10", keptVolumeMovedTo(third), pushedMovedTo(third)},
		{"a second move back onto the pod's repository", podUpdateRepo, "main", podUpdateKeptVolumeStays, podUpdateAtOrigin},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester))
			rig.corrects(podUpdateCorrectedRepo, podUpdateCorrectedBranch)
			rig.rt.ScriptKeptVolume(podUpdateIssue, podUpdateStart.Add(-time.Hour))

			rig.tick()
			rig.awaitRound(claim.RoleImplementer, podUpdateRound)
			rig.awaitRound(claim.RoleTester, podUpdateRound)
			rig.corrects(tc.again, tc.branch)
			rig.signal(claim.RoleTester, "a1b2c3d")
			rig.tickBackIn(podUpdateReplacementPod)

			messages := rig.messages()
			if len(messages) != 1 {
				t.Fatalf("the issue was written on %d times, want once for the pod that went", len(messages))
			}
			if want := implementerLostWith(podUpdateRepositoryReason, tc.disk, tc.pushed); messages[0] != want {
				t.Fatalf("the message on the issue =\n%s\nwant\n%s", messages[0], want)
			}
		})
	}
}

// A correction can land while a round that another drift opened runs. The round here opens for
// the image alone, over a pod built from podUpdateRepo while the record still holds that
// repository, and Dispatch moves the issue to podUpdateCorrectedRepo before the pod goes. The
// round records the repository its pod was built from at its open, whatever drift opened it
// (supervise.Claim.UpdateRepository), so the message on the gone pod still names the issue branch
// of podUpdateRepo, and on a kept volume its workspace, and names podUpdateCorrectedRepo as the
// new pod's origin. Its reasons name the image alone: a round's terms are fixed at its open.
func TestACorrectionWhileAnImageRoundRunsNamesThePodsRepositoryWhenThePodGoes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// created is when the issue's kept Sandbox was made, zero for an ephemeral volume.
		created time.Time
		disk    string
	}{
		{"a kept volume", podUpdateStart.Add(-time.Hour), keptVolumeMovedTo(podUpdateCorrectedRepo)},
		{"an ephemeral volume", time.Time{}, podUpdateDiskLost},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester))
			rig.drifts(podUpdateDrift)
			if !tc.created.IsZero() {
				rig.rt.ScriptKeptVolume(podUpdateIssue, tc.created)
			}

			rig.tick()
			for _, role := range []claim.Role{claim.RoleImplementer, claim.RoleTester} {
				c := rig.awaitRound(role, podUpdateRound)
				if c.UpdateReasons != podUpdateDrift || c.UpdateRepository != ghrepo.MustParse(podUpdateRepo) {
					t.Errorf("the %s's round records the reasons %q and the repository %q, want %q and %s, its pod's",
						role, c.UpdateReasons, c.UpdateRepository, podUpdateDrift, podUpdateRepo)
				}
			}
			rig.corrects(podUpdateCorrectedRepo, podUpdateCorrectedBranch)
			rig.signal(claim.RoleTester, "a1b2c3d")
			rig.tickBackIn(podUpdateReplacementPod)

			messages := rig.messages()
			if len(messages) != 1 {
				t.Fatalf("the issue was written on %d times, want once for the pod that went", len(messages))
			}
			if want := implementerLostWith(podUpdateDrift, tc.disk, pushedMovedTo(podUpdateCorrectedRepo)); messages[0] != want {
				t.Fatalf("the message on the issue =\n%s\nwant\n%s", messages[0], want)
			}
		})
	}
}

// A tick whose read of the pod's repository fails has read the pod no better than one whose drift
// read fails, and opens no round however the image drifted (groupDrift, podUnread). A round opened
// on it would record no repository (supervise.Claim.UpdateRepository) for its whole life, since
// its terms are fixed at its open, and a correction landing while it ran would send each role to
// its issue branch at the new pod's origin, which is the other repository. The next tick reads the
// repository and opens the round with it recorded.
func TestARoundOpensOnlyOnATickThatReadThePodsRepository(t *testing.T) {
	t.Parallel()
	roles := []claim.Role{claim.RoleImplementer, claim.RoleTester}
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester))
	rig.drifts(podUpdateDrift)
	rig.rt.FailPodRepository(errors.New("the cluster did not answer for this pod"))

	rig.tick()
	for _, role := range roles {
		rig.awaitPosted(role)
		if c := rig.claimOf(role); c.UpdateRound != "" {
			t.Errorf("after the tick whose repository read failed, the %s is in round %q for reasons %q with the repository %q, want no round",
				role, c.UpdateRound, c.UpdateReasons, c.UpdateRepository)
		}
	}

	rig.rt.FailPodRepository(nil)
	rig.tick()
	for _, role := range roles {
		c := rig.awaitRound(role, podUpdateRound)
		if c.UpdateReasons != podUpdateDrift || c.UpdateRepository != ghrepo.MustParse(podUpdateRepo) {
			t.Errorf("the %s's round records the reasons %q and the repository %q, want %q and %s, its pod's",
				role, c.UpdateReasons, c.UpdateRepository, podUpdateDrift, podUpdateRepo)
		}
	}
}

// A replacement that took work its read found tells the issue's architect what it took
// (workspaceTakenMessage), and that notice says where the work is as the message on a gone pod
// does. The round here opens over a drift that breaks the pod (roundBreakingDrift), which is
// replaced whatever its read finds (roundKeeps, R7 of #5285), over a pod of podUpdateRepo; the
// daemon suspended the planner, and the read through the implementer finds one commit that no
// branch holds, or fails. Where Dispatch moves the issue to podUpdateCorrectedRepo while the round
// runs, the notice names the work by podUpdateRepo and not by origin, in what it says the read
// found or could not know as in what it says of the disk: the read ran in the round's pod, whose
// origin was podUpdateRepo. It sends each role to the issue branch of podUpdateRepo, names
// podUpdateCorrectedRepo as the new pod's origin and, on a kept volume, names the workspace of
// podUpdateRepo: the repository the replacement's record holds and the record as it stands when
// the notice is queued (roundMove). The control is the same round of an issue that never moved,
// whose notice is the text of every other such replacement, word for word. The issue is written
// nothing: the architect is the one told what a replacement took (wantToldTaken).
func TestAReplacementOverAPassedOverRoleNamesThePodsRepositoryWhereTheIssueMoved(t *testing.T) {
	t.Parallel()
	madeBefore := podUpdateStart.Add(-time.Hour)
	const (
		// atOrigin and ofThePods are the words the notice names the work that no branch holds
		// with, on an issue that never moved and on one that moved off the pod's repository, and
		// heldAtOrigin and heldOfThePods the words it names the work the read was for with.
		atOrigin      = "Work that is not on a branch at origin"
		ofThePods     = "Work that is not on a branch of the repository `" + podUpdateRepo + "`"
		heldAtOrigin  = "no branch or tag at origin holds"
		heldOfThePods = "no branch or tag of the repository `" + podUpdateRepo + "` holds"
		// lost and kept are what the notice says of the pod and its disk.
		lost = "The pod and its disk are gone."
		kept = "The pod is gone. Its disk is a kept volume, and the volume is still there."
	)
	for _, tc := range []struct {
		name string
		// keptVolumeAt is when the issue's Sandbox made the kept volume that is the pod's disk,
		// nil for an ephemeral disk, corrected is whether Dispatch moves the issue to
		// podUpdateCorrectedRepo while the round runs, and fails is whether the read fails with
		// errPassedOverRead rather than find the one commit.
		keptVolumeAt     *time.Time
		corrected, fails bool
		// gone, held, work and pushed are what the notice says of the pod and its disk, the
		// words it names the work the read was for with, what it says of the work that no
		// branch holds, and of the work each role pushed.
		gone, held, work, pushed string
	}{
		{"an ephemeral disk of an issue that never moved", nil, false, false, lost, heldAtOrigin,
			atOrigin + " is lost.", podUpdateAtOrigin},
		{"a kept volume of an issue that never moved", &madeBefore, false, false, kept, heldAtOrigin,
			atOrigin + " is on that volume. The next pod of this issue mounts the volume.", podUpdateAtOrigin},
		{"a read that fails on an issue that never moved", nil, false, true, lost, heldAtOrigin,
			atOrigin + " is lost.", podUpdateAtOrigin},
		{"an ephemeral disk of an issue that moved", nil, true, false, lost, heldOfThePods,
			ofThePods + " is lost.", pushedMovedTo(podUpdateCorrectedRepo)},
		{"a kept volume of an issue that moved", &madeBefore, true, false, kept, heldOfThePods,
			keptWorkMovedTo(ofThePods, podUpdateCorrectedRepo), pushedMovedTo(podUpdateCorrectedRepo)},
		{"a read that fails on an issue that moved", nil, true, true, lost, heldOfThePods,
			ofThePods + " is lost.", pushedMovedTo(podUpdateCorrectedRepo)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), suspendedRole(claim.RolePlanner, supervise.SuspendedByDaemon))
			read := "Before the replacement, the daemon read the issue workspace. " +
				"The read found work that " + tc.held + ". " +
				"The workspace held one commit that " + tc.held + ": " + passedOverCommit + "."
			if tc.fails {
				rig.rt.FailWorkspaceWork(podUpdateIssue, errPassedOverRead)
				read = "No read of the workspace could be made before the replacement. " +
					"The error of the read, in quotes, is: " + readError(errPassedOverRead) + ". " +
					"The daemon does not know if the workspace held work that " + tc.held + "."
			} else {
				rig.rt.ScriptWorkspaceWork(podUpdateIssue, runtime.WorkspaceWork{Head: passedOverHead, Commits: []string{passedOverCommit}, Count: 1})
			}
			if tc.keptVolumeAt != nil {
				rig.rt.ScriptKeptVolume(podUpdateIssue, *tc.keptVolumeAt)
			}
			rig.drifts(roundBreakingDrift)

			rig.tick()
			rig.awaitAsked(claim.RoleImplementer, podUpdateRound)
			rig.awaitRound(claim.RolePlanner, podUpdateRound)
			if tc.corrected {
				rig.corrects(podUpdateCorrectedRepo, podUpdateCorrectedBranch)
			}
			rig.signal(claim.RoleImplementer, "a1b2c3d")
			rig.settleAll()
			rig.awaitReplacement(1, "once the implementer answered")

			notice := wantToldTaken(t, rig, "planner (suspended)")
			want := "The daemon replaced the pod of " + podUpdateIssue + " (pod " + podUpdatePod + ") for the update round " + podUpdateRound + ". " +
				"The reasons were: " + roundBreakingDrift + ". " +
				"The round was over a drift that breaks the pod, so no role could keep working in the pod. " + tc.gone + " " +
				toldTakenRoles + "planner (suspended). " +
				read + " " + tc.work + " " + tc.pushed + " You are told this once for this replacement."
			if notice != want {
				t.Fatalf("the architect's notice =\n%s\nwant\n%s", notice, want)
			}
		})
	}
}

// podUpdatePushedUnread is the last sentence of the message on a gone pod whose read of the
// issue's record failed, and podUpdateKeptVolumeUnread what that message says of a kept volume that
// is still there: each names the repository the round's pod was built from, podUpdateRepo, and
// never origin.
const (
	podUpdatePushedUnread = "Work that each role pushed before the pod went " +
		"is on the issue branch of the repository `" + podUpdateRepo + "`. " +
		"The daemon could not read the repository that this issue's record holds now. " +
		"It does not know which repository is origin in the new pod. " +
		"To see that work, each role must read the issue branch of the repository `" + podUpdateRepo + "`."
	podUpdateKeptVolumeUnread = "The pod's disk is a kept volume, and the volume is still there. " +
		"Work that these roles did not push is on that volume, " +
		"in the workspace of `" + podUpdateRepo + "` at `" + podUpdateOldWorkspace + "`. " +
		"The next pod of this issue mounts the volume. " +
		"If that pod makes a workspace of a different repository, " +
		"read that work at `" + podUpdateOldWorkspace + "` in that pod. " +
		"Put that work on the issue branch in the new workspace. Then push the issue branch."
)

// A gone pod's message whose read of the issue's record fails names the repository the round's
// pod was built from (supervise.Claim.UpdateRepository), never origin: a correction can have moved
// the record, and origin in the next pod is then the repository the record holds now, which holds
// none of the work the roles pushed. The round here was opened over a pod of podUpdateRepo, and
// the record cannot be read when the pod goes. The message sends each role to the issue branch of
// podUpdateRepo, names the workspace of podUpdateRepo on a kept volume, and says that the daemon
// could not read the record. Control: with the failed read taken as no move, the message sends
// each role to its issue branch at origin.
func TestAPodGoneWhileTheRecordCannotBeReadNamesThePodsRepository(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// created is when the issue's kept Sandbox was made, zero for an ephemeral volume.
		created time.Time
		disk    string
	}{
		{"a kept volume", podUpdateStart.Add(-time.Hour), podUpdateKeptVolumeUnread},
		{"an ephemeral volume", time.Time{}, podUpdateDiskLost},
	} {
		t.Run(tc.name, func(t *testing.T) {
			roles := []podUpdateRole{readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester)}
			for i := range roles {
				roles[i].round, roles[i].reasons, roles[i].repository = podUpdateRound, podUpdateDrift, podUpdateRepo
			}
			roles[1].signalled = true
			rig := newPodUpdateRig(t, roles...)
			if !tc.created.IsZero() {
				rig.rt.ScriptKeptVolume(podUpdateIssue, tc.created)
			}
			rig.s.plan.recordedRepository = func(context.Context, string) (string, error) {
				return "", errors.New("the store did not answer")
			}

			rig.tickBackIn(podUpdateReplacementPod)

			messages := rig.messages()
			if len(messages) != 1 {
				t.Fatalf("the issue was written on %d times, want once for the pod that went", len(messages))
			}
			if want := implementerLostWith(podUpdateDrift, tc.disk, podUpdatePushedUnread); messages[0] != want {
				t.Fatalf("the message on the issue =\n%s\nwant\n%s", messages[0], want)
			}
		})
	}
}

// The notice of a replacement that took work reads the record as the message on a gone pod does
// (roundMove): where the record cannot be read when the notice is queued, it names the work that
// no branch holds by the repository of the round's pod, podUpdateRepo, and not by origin, in what
// it says the read found and in what it says of the disk, and its sentence of the pushed work is
// the one of a gone pod whose record could not be read. The round is over a drift that breaks the
// pod (roundBreakingDrift), so it replaces the pod over the work its read found (roundKeeps).
// Control: with the failed read taken as no move, the notice names that work at origin.
func TestAReplacementWhileTheRecordCannotBeReadNamesThePodsRepository(t *testing.T) {
	t.Parallel()
	const held = "no branch or tag of the repository `" + podUpdateRepo + "` holds"
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), suspendedRole(claim.RolePlanner, supervise.SuspendedByDaemon))
	rig.rt.ScriptWorkspaceWork(podUpdateIssue, runtime.WorkspaceWork{Head: passedOverHead, Commits: []string{passedOverCommit}, Count: 1})
	rig.drifts(roundBreakingDrift)

	rig.tick()
	rig.awaitAsked(claim.RoleImplementer, podUpdateRound)
	rig.awaitRound(claim.RolePlanner, podUpdateRound)
	rig.s.plan.recordedRepository = func(context.Context, string) (string, error) {
		return "", errors.New("the store did not answer")
	}
	rig.signal(claim.RoleImplementer, "a1b2c3d")
	rig.settleAll()
	rig.awaitReplacement(1, "once the implementer answered")

	notice := wantToldTaken(t, rig, "planner (suspended)")
	want := "The daemon replaced the pod of " + podUpdateIssue + " (pod " + podUpdatePod + ") for the update round " + podUpdateRound + ". " +
		"The reasons were: " + roundBreakingDrift + ". " +
		"The round was over a drift that breaks the pod, so no role could keep working in the pod. The pod and its disk are gone. " +
		toldTakenRoles + "planner (suspended). " +
		"Before the replacement, the daemon read the issue workspace. " +
		"The read found work that " + held + ". " +
		"The workspace held one commit that " + held + ": " + passedOverCommit + ". " +
		"Work that is not on a branch of the repository `" + podUpdateRepo + "` is lost. " + podUpdatePushedUnread +
		" You are told this once for this replacement."
	if notice != want {
		t.Fatalf("the architect's notice =\n%s\nwant\n%s", notice, want)
	}
}

// R3 of #3712 (spec v8) under R7 of #5285: a corrected repository breaks the pod
// (repositoryReason opens on runtime.BreakingDrift), since no role can move the work onto the
// corrected repository in a pod that clones the repository the issue has left. So the round a
// correction opens replaces the pod whatever the read of its workspace finds (roundKeeps) and never
// keeps it. Over a read that finds a commit no branch holds, the issue's architect is told once
// what the replacement took, naming that work by podUpdateRepo, the pod's repository, and sending
// each role to that repository's issue branch, with podUpdateCorrectedRepo as the new pod's
// origin. Over a clean read nobody is told. The control is the same read that finds work under a
// round over a drift the roles can keep working under, podUpdateDrift alone: that round keeps the
// pod and replaces nothing.
func TestARepositoryCorrectionReplacesThePodWhateverItsReadFinds(t *testing.T) {
	t.Parallel()
	roles := []claim.Role{claim.RoleImplementer, claim.RoleTester}
	const held = "no branch or tag of the repository `" + podUpdateRepo + "` holds"
	for _, tc := range []struct {
		name string
		// corrected is whether Dispatch moved the issue to podUpdateCorrectedRepo, which opens the
		// round; otherwise the round is over podUpdateDrift alone. work is what the read finds.
		corrected bool
		work      runtime.WorkspaceWork
		// replaced and told are whether the pod is replaced and whether the architect is told.
		replaced, told bool
	}{
		{"a correction over a read that finds work", true, roundUnpushed, true, true},
		{"a correction over a clean read", true, roundClean, true, false},
		{"control: an image drift over a read that finds work", false, roundUnpushed, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester))
			rig.rt.ScriptWorkspaceWork(podUpdateIssue, tc.work)
			if tc.corrected {
				rig.corrects(podUpdateCorrectedRepo, podUpdateCorrectedBranch)
			} else {
				rig.drifts(podUpdateDrift)
			}
			rig.tick()
			for _, role := range roles {
				if c := rig.awaitAsked(role, podUpdateRound); runtime.Breaks(c.UpdateReasons) != tc.corrected {
					t.Fatalf("the %s's round records the reasons %q, want them to break the pod only for a correction", role, c.UpdateReasons)
				}
			}
			for _, role := range roles {
				rig.signal(role, "a1b2c3d")
			}
			rig.settleAll()
			if !tc.replaced {
				for _, role := range roles {
					rig.awaitSwept(role, "its round to end with the pod kept", func(c supervise.Claim) bool { return c.UpdateRound == "" })
				}
				if replaced := rig.replacements(); len(replaced) != 0 {
					t.Fatalf("the pod was replaced %d times, want none: the round over a drift the roles keep working under kept it", len(replaced))
				}
				if reason, _ := rig.keptReason(); reason == "" {
					t.Fatalf("the issue holds no decline row, want the pod kept for the work its read found")
				}
				return
			}
			rig.awaitReplacement(1, "of the pod of the correction's round")
			if reason, _ := rig.keptReason(); reason != "" {
				t.Fatalf("the issue's decline row reads %q, want none: a correction's round never keeps its pod", reason)
			}
			if !tc.told {
				if told := rig.architectNotices(record.WorkspaceTakenNotice); len(told) != 0 {
					t.Fatalf("the architect was told %+v, want no workspace-taken notice", told)
				}
				return
			}
			notice := wantToldTaken(t, rig)
			for _, want := range []string{
				"The workspace held one commit that " + held + ": " + passedOverCommit + ".",
				"Work that is not on a branch of the repository `" + podUpdateRepo + "` is lost.",
				pushedMovedTo(podUpdateCorrectedRepo) + " You are told this once for this replacement.",
			} {
				if !strings.Contains(notice, want) {
					t.Errorf("the notice does not say %q:\n%s", want, notice)
				}
			}
			if strings.Contains(notice, "at origin") {
				t.Errorf("the notice names work at origin, which in the new pod is %s and holds none of it:\n%s", podUpdateCorrectedRepo, notice)
			}
		})
	}
}

// The record of a correction round's replacement holds the repository the round's pod was built
// from (replacementRecord.Repository), and its finish reads the issue's record again when it
// queues the architect's notice (roundMove). Here the call's suspension lands and the call fails,
// so the record stands; Dispatch moves the issue on again, to a third repository; and a restarted
// daemon, whose runtime answers no repository for the pod, finishes the replacement from the
// record (finishReplacements). Its notice names the work by podUpdateRepo, the repository the
// record holds, and names the third repository as the new pod's origin, as the record stands at
// the finish.
func TestARepositoryCorrectionsReplacementFinishedAfterARestartNamesTheRecordedRepository(t *testing.T) {
	t.Parallel()
	const third, thirdBranch = "zeta/gadgets", "release/v10"
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester))
	rig.rt.ScriptWorkspaceWork(podUpdateIssue, roundUnpushed)
	rig.rt.FailReplacePodAfterItsSuspension(errWaitedOut)
	rig.corrects(podUpdateCorrectedRepo, podUpdateCorrectedBranch)
	rig.tick()
	for _, role := range landedRoles {
		rig.awaitAsked(role, podUpdateRound)
	}
	for _, role := range landedRoles {
		rig.signal(role, "a1b2c3d")
	}
	rig.settleAll()
	rig.awaitReplacement(1, "for the call whose wait ran out once its suspension landed")

	var held string
	if err := rig.pool.QueryRow(rig.ctx, `select coalesce(record->>'repository', '') from pod_replacements where issue = $1`, podUpdateIssue).Scan(&held); err != nil {
		t.Fatalf("read the record of the replacement: %v", err)
	}
	if held != podUpdateRepo {
		t.Fatalf("the record of the replacement holds the repository %q, want %s, the one the round's pod was built from", held, podUpdateRepo)
	}
	if told := rig.architectNotices(record.WorkspaceTakenNotice); len(told) != 0 {
		t.Fatalf("the architect was told %+v before the record was finished, want nothing yet", told)
	}

	rig.corrects(third, thirdBranch)
	next := restartedOverIt(rig)
	for _, role := range landedRoles {
		next.awaitSwept(role, "its round to end as the replacement it was", func(c supervise.Claim) bool { return c.UpdateRound == "" })
	}
	told := next.architectNotices(record.WorkspaceTakenNotice)
	if len(told) != 1 {
		t.Fatalf("the architect was told %+v, want one workspace-taken notice", told)
	}
	for _, want := range []string{
		"The workspace held one commit that no branch or tag of the repository `" + podUpdateRepo + "` holds: " + passedOverCommit + ".",
		pushedMovedTo(third) + " You are told this once for this replacement.",
	} {
		if !strings.Contains(told[0].Reason, want) {
			t.Errorf("the notice does not say %q:\n%s", want, told[0].Reason)
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

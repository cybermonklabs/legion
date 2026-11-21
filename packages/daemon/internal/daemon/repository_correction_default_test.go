package daemon

// The project's default repository: an issue that names no repository of its own,
// and has no ancestor that does, resolves to its project's default, so a change of that default
// in Dispatch moves every recorded issue that inherits it at once.
//
// Since dispatch://INFRA-4401 the daemon hears no settings.repo_project.updated and reads no
// repository mapping, and Dispatch sends no event of an issue whose resolution a default change
// moved, so such an issue's record follows at the issue's own next event or at the next boot,
// whose listing leaves each recorded issue's change to the follow after it (followListing): one
// single-issue correction per issue (workflow.(*Engine).ApplyRepositoryChange), from the read its
// preparation made of GitHub, in one transaction. These tests read what that pass writes for
// several issues at once: each is corrected on its own, with its own moment, and the pods are
// replaced by the ordinary per-issue update round rather than by a batch with a path of its own.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/ghbranch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// defaultChangeKeys are the two recorded roots that inherit the project's default repository:
// neither names a repository of its own, so Dispatch resolves both to the default, and a change
// of it moves both.
var defaultChangeKeys = []string{"LEGION-610", "LEGION-611"}

// defaultChangeStart is where the engine's clock stands when a test begins, and defaultChangeStep
// how far one read of it moves: every call answers a moment of its own, so a record stamped with
// one correction's moment cannot be holding another's.
var (
	defaultChangeStart = time.Date(2026, 11, 12, 11, 0, 0, 0, time.UTC)
	defaultChangeStep  = time.Minute
)

// defaultChangeRig is one test's daemon: the records and the engine whose single-issue
// correction the follow's pass applies over them, with a clock that answers a new moment at every
// read.
type defaultChangeRig struct {
	t       *testing.T
	pool    *store.Pool
	records record.Store
	engine  *workflow.Engine
	// read is every moment the engine's clock has answered, in order, which is what says a
	// correction's moment is its own read rather than a moment shared by the pass.
	read []time.Time
}

func newDefaultChangeRig(t *testing.T) *defaultChangeRig {
	t.Helper()
	r := &defaultChangeRig{t: t, pool: isolatedOutboxPool(t), records: record.NewStore()}
	at := defaultChangeStart
	clock := func() time.Time {
		at = at.Add(defaultChangeStep)
		r.read = append(r.read, at)
		return at
	}
	r.engine = workflow.New(r.records, workflow.Config{Project: correctionProject, Repositories: testRepositories,
		Steps: config.DesignGateOff.Steps(), Clock: clock}, quietLogger())
	for index, key := range defaultChangeKeys {
		r.recordRoot(key, index)
	}
	return r
}

// recordRoot records one running root on the project's default repository as it stands, in the
// slot index, with its tree live: the shape a correction may not move.
func (r *defaultChangeRig) recordRoot(key string, index int) {
	r.t.Helper()
	ctx := context.Background()
	root := record.Issue{Key: key, Tree: key, Project: correctionProject, Title: "a root on the project's default",
		Phase: phase.Implementing, Generation: correctionGeneration, Status: "in_progress", DispatchStatus: "in_progress",
		Rank: "A", HandedOver: true, LastDispatchSeq: 1, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets"}
	if err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		if err := r.records.PutIssue(ctx, tx, root); err != nil {
			return err
		}
		if err := r.records.PutSlot(ctx, tx, record.Slot{Issue: key, Index: index, AdmittedAt: defaultChangeStart}); err != nil {
			return err
		}
		return openTreeTx(ctx, tx, correctionProject, key)
	}); err != nil {
		r.t.Fatalf("record %s: %v", key, err)
	}
}

// followed applies what the follow after a boot writes over a Dispatch listing that shows both
// issues resolved to resolved, at the sequence their records already hold (followListing): each
// issue's single-issue correction, in one transaction under the fact lock, from the read its
// preparation made of GitHub, which names resolved at branch. Nothing of either issue moved in
// Dispatch but the repository it resolves to, which is what a change of the project's default
// leaves behind.
func (r *defaultChangeRig) followed(resolved, branch string) {
	r.t.Helper()
	read := &ghbranch.Repository{Name: ghrepo.MustParse(resolved), DefaultBranch: branch}
	scoped, committed := intake.WithCommitHooks(context.Background())
	if err := pgx.BeginFunc(scoped, r.pool, func(tx pgx.Tx) error {
		if err := intake.SerializeFacts(scoped, tx); err != nil {
			return err
		}
		for _, key := range defaultChangeKeys {
			stored, err := r.records.Issue(scoped, tx, key)
			if err != nil {
				return err
			}
			fact := intake.ListedIssue(dispatch.IssueSummary{Key: key, Title: "a root on the project's default", Status: "in_progress",
				Rank: "A", HandedOver: true, LastSeq: 1, ResolvedRepo: resolved})
			fact.RepositoryRead = read
			if err := r.engine.ApplyRepositoryChange(scoped, tx, *stored, fact); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		r.t.Fatalf("the follow onto %s: %v", resolved, err)
	}
	committed()
}

// issue is key's record as it stands.
func (r *defaultChangeRig) issue(key string) record.Issue {
	r.t.Helper()
	ctx := context.Background()
	var issue *record.Issue
	if err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		var err error
		issue, err = r.records.Issue(ctx, tx, key)
		return err
	}); err != nil {
		r.t.Fatal(err)
	}
	if issue == nil {
		r.t.Fatalf("%s is not recorded", key)
	}
	return *issue
}

// A change of the project's default repository moves every recorded issue that inherits it, and
// the pass that reads it records each move on its own: one correction per issue, each naming the
// repository that issue left and the moment of its own correction. The moments differ because
// each is the engine's own read at that issue's correction, which is what a single-issue
// correction does; nothing here is a batch with one moment for the group.
func TestRepositoryCorrectionFollowsAChangeOfTheProjectsDefaultRepositoryForEveryIssueThatInheritsIt(t *testing.T) {
	r := newDefaultChangeRig(t)

	// Dispatch's default for the project is acme/tools now, so every issue that names no
	// repository of its own resolves to it.
	r.followed("acme/tools", "release/v7.1")

	moments := map[string]time.Time{}
	for _, key := range defaultChangeKeys {
		issue := r.issue(key)
		if issue.Repo != "acme/tools" || issue.DefaultBranch != "release/v7.1" || issue.GitHubRepo != "acme/tools" {
			t.Fatalf("%s is on %s at %s, spelled %s; want acme/tools at release/v7.1", key, issue.Repo, issue.DefaultBranch, issue.GitHubRepo)
		}
		if issue.RepoCorrection == nil || issue.RepoCorrection.Before != "acme/widgets" {
			t.Fatalf("the correction of %s = %+v, want acme/widgets left", key, issue.RepoCorrection)
		}
		if issue.Generation != correctionGeneration || issue.Phase != phase.Implementing {
			t.Fatalf("%s is at generation %d in %s, want generation %d in %s: a correction takes no new generation and moves no phase",
				key, issue.Generation, issue.Phase, correctionGeneration, phase.Implementing)
		}
		moments[key] = issue.RepoCorrection.At
	}
	first, second := moments[defaultChangeKeys[0]], moments[defaultChangeKeys[1]]
	if first.Equal(second) {
		t.Fatalf("both issues were corrected at %v, want a moment each: the pass reads the clock at each correction", first)
	}
	for key, at := range moments {
		found := false
		for _, read := range r.read {
			found = found || read.Equal(at)
		}
		if !found {
			t.Fatalf("%s was corrected at %v, which the engine's clock never answered (%v)", key, at, r.read)
		}
	}

	// The pass is idempotent over the same listing: the default is still acme/tools, so the
	// second reading corrects nothing and leaves each moment where it was.
	r.followed("acme/tools", "release/v7.1")
	for _, key := range defaultChangeKeys {
		issue := r.issue(key)
		if c := issue.RepoCorrection; c == nil || !c.At.Equal(moments[key]) || c.Before != "acme/widgets" {
			t.Fatalf("the correction of %s = %+v after a second reading of the same default, want acme/widgets left at its first moment %v",
				key, c, moments[key])
		}
	}
}

// defaultChangeSecondIssue is the second issue of the update-round rig: a running issue of the
// same project, in a pod of its own, whose record a default change moves with the rig's own.
const (
	defaultChangeSecondIssue = "LEGION-485"
	defaultChangeSecondPod   = "b86d2f41-79c3-4e05-8a17-2f6b9d3c0e74"
	defaultChangeSecondRound = "pod:" + defaultChangeSecondPod
)

// The pod replacements a default change asks for are the ordinary per-issue ones: the correction
// writes records and nothing else, and the update-round sweep then reads each issue on its own —
// its own pod, its own round, its own two repositories in the reasons. No batch path opens a
// round over two issues, and no issue's round names another issue's pod.
func TestRepositoryCorrectionOpensAnOrdinaryRoundForEachIssueADefaultChangeMoved(t *testing.T) {
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer))
	second := rig.secondIssue(defaultChangeSecondIssue, defaultChangeSecondPod)

	// The default moved from acme/widgets to zeta/tools, and the record of each issue that
	// inherits it followed, each correction its own statement.
	rig.corrects(podUpdateCorrectedRepo, podUpdateCorrectedBranch)
	rig.correctsIssue(defaultChangeSecondIssue, podUpdateCorrectedRepo, podUpdateCorrectedBranch)

	rig.tick()

	first := rig.awaitAsked(claim.RoleImplementer, podUpdateRound)
	if first.UpdateReasons != podUpdateRepositoryReason {
		t.Errorf("%s's round reasons = %q, want %q", podUpdateIssue, first.UpdateReasons, podUpdateRepositoryReason)
	}
	other := rig.awaitAskedOf(second, defaultChangeSecondRound)
	if other.UpdateReasons != podUpdateRepositoryReason {
		t.Errorf("%s's round reasons = %q, want %q", defaultChangeSecondIssue, other.UpdateReasons, podUpdateRepositoryReason)
	}
	if first.UpdateRound == other.UpdateRound {
		t.Fatalf("both issues are in the round %q, want a round of each issue's own pod", first.UpdateRound)
	}
	// Each round asks its own issue's role over Envoy, naming its own round and pod and both
	// repositories, and no ask of one issue names the other's pod. The lane has run and deleted
	// each row by now (awaitAsked), so they are read from the rig's record of every row it saw.
	rounds := map[string]string{podUpdateIssue: podUpdateRound, defaultChangeSecondIssue: defaultChangeSecondRound}
	pods := map[string]string{podUpdateIssue: podUpdatePod, defaultChangeSecondIssue: defaultChangeSecondPod}
	for issue := range rounds {
		asks := rig.asksOn(issue)
		if len(asks) != 1 {
			t.Errorf("%s was asked %d times, want once, by its own round", issue, len(asks))
		}
		for _, ask := range asks {
			if ask.Round != rounds[issue] || ask.Pod != pods[issue] {
				t.Errorf("the ask on %s = %+v, want round %s of pod %s", issue, ask, rounds[issue], pods[issue])
			}
			if !strings.Contains(ask.Text, podUpdateRepo) || !strings.Contains(ask.Text, podUpdateCorrectedRepo) {
				t.Errorf("the ask on %s says %q, want it to name both %s and %s", issue, ask.Text, podUpdateRepo, podUpdateCorrectedRepo)
			}
		}
	}
}

// secondIssue seeds another running issue of the project in a pod of its own, restores its one
// claim as the boot does, and scripts the repository its pod was built from: what the rig seeds
// for its own issue, for a test that reads the sweep over two issues at once. It answers that
// claim's token.
func (r *podUpdateRig) secondIssue(issue, pod string) claim.Token {
	r.t.Helper()
	r.second[issue] = true
	token := mustClaimToken(r.t, issue, claim.RoleImplementer)
	c := supervise.Claim{
		Token: token, Project: podUpdateProject, Tree: issue, Issue: issue, Role: claim.RoleImplementer,
		Generation: 1, State: supervise.StateReady, PluginContract: api.DaemonAPIVersion,
		Session: "ses_" + issue, SessionFile: "/sessions/" + issue + ".jsonl",
		Locator: podUpdateLocator(token, issue, pod),
	}
	seedWorkflowTree(r.t, r.pool.Config().ConnString(), podUpdateProject, record.Issue{
		Key: issue, Repo: podUpdateRepo, DefaultBranch: "main", GitHubRepo: podUpdateRepo,
		Project: "LEGION", Tree: issue, Title: "another issue on the project's default", Phase: phase.Implementing,
		Generation: 1, Status: "in_progress", HandedOver: true, Rank: "U",
	}, c)
	stored, err := r.sup.Claims(r.ctx)
	if err != nil {
		r.t.Fatalf("read the seeded claims: %v", err)
	}
	fresh := make([]supervise.Claim, 0, 1)
	for _, seeded := range stored {
		if seeded.Issue == issue {
			fresh = append(fresh, seeded)
		}
	}
	if _, err := r.sup.restore(r.ctx, fresh); err != nil {
		r.t.Fatalf("restore the claims of %s: %v", issue, err)
	}
	r.rt.ScriptPodRepository(token, podUpdateRepo)
	return token
}

// correctsIssue moves another issue's record onto repo at branch, as corrects moves the rig's own.
func (r *podUpdateRig) correctsIssue(issue, repo, branch string) {
	r.t.Helper()
	correctRecord(r.t, r.pool, r.records, issue, repo, branch, r.clock.Now())
}

// awaitAskedOf sweeps until one claim named by its token records round and the sweep's publish
// mark of its ask (supervise.Claim.UpdateTaskedAt), which is how a test reads an issue the rig's
// role map does not name (awaitAsked).
func (r *podUpdateRig) awaitAskedOf(token claim.Token, round string) supervise.Claim {
	r.t.Helper()
	claimOf := func() supervise.Claim {
		m, ok := r.sup.Machine(token)
		if !ok {
			r.t.Fatalf("%s is not supervised", token)
		}
		return m.Claim()
	}
	asked := func() bool {
		c := claimOf()
		return c.UpdateRound == round && c.UpdateTaskedAt != nil
	}
	landing := true
	testwait.Eventually(r.t, string(token)+": the ask of the update round "+round, func() bool {
		if asked() {
			return true
		}
		if landing {
			landing = false
			return false
		}
		r.tick()
		return asked()
	})
	return claimOf()
}

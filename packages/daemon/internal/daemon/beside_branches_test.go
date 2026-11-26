package daemon

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/checkstatetest"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// The heads of the task's pull request in TestNoBesideBranchIsLeftAfterTheTaskEnds: X, where the
// check k-ci fails and the tester tests, and Y, where fix 1 ends with commits of its own; and the
// heads the agents' pushes leave the tester's branch and the fix's own branch at.
const (
	taskEndX      = "e0d0000000000000000000000000000000000a01"
	taskEndY      = "e0d0000000000000000000000000000000000b02"
	taskEndTested = "be51de00000000000000000000000000000000c1"
	taskEndFixed  = "be51de00000000000000000000000000000000f1"
)

// taskEndChecks is the set of checks Dispatch publishes at every head of the walk: k1, which the
// tester proves, and k-ci, which a CI job proves.
var taskEndChecks = []string{"k1", "k-ci"}

// taskEndWalk is one whole daemon (the CI news tests' ciDaemon) walking one task to its end: the
// issue on the plan `implementer tester reviewer`, its one task a taking a fix, standing at its
// testing step with the tester's run open at X, its pull request open at X. GitHub is
// githubtest.Refs beside the reads every boot makes (ciGitHub). Dispatch is the board of
// newPauseDaemonRig, with the read of a fix's failed CI job answered as Dispatch answers it for a
// check it lists no job of, so the read starts the fix at once. Each fact is applied with the
// daemon's own handlers, as its consumers and its API apply one.
type taskEndWalk struct {
	*ciDaemon
	refs *githubtest.Refs
	// serial is the serial of the last check state the walk applied.
	serial int64
	// testerBranch is the tester's branch beside fix 1 (B), fixBranch the fix's own branch (F),
	// besidePrefix the start of every branch beside a fix of the issue, and issueBranch the
	// issue's own branch, which no deletion of a branch beside a fix may take.
	testerBranch, fixBranch, besidePrefix, issueBranch string
}

// newTaskEndWalk seeds the walk's issue and boots its daemon, with engine set on the daemon's
// engine (nil for no seam).
func newTaskEndWalk(t *testing.T, engine func(*workflow.Engine)) *taskEndWalk {
	t.Helper()
	listener := newCIListener(t)
	var board atomic.Pointer[liveDispatch]
	dispatch := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		key, results := strings.CutSuffix(strings.TrimPrefix(req.URL.Path, "/api/v1/issues/"), "/results")
		if req.Method == http.MethodGet && results && strings.HasPrefix(req.URL.Path, "/api/v1/issues/") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"issue":%q,"verification_artifact_id":"a-verification","checks":[]}`, key)
			return
		}
		board.Load().ServeHTTP(w, req)
	}))
	t.Cleanup(dispatch.Close)
	r := newPauseDaemonRig(t, func(cfg *config.Config) { cfg.EnvoyURL, cfg.DispatchURL = listener.url, dispatch.URL })
	board.Store(r.board)
	d := &ciDaemon{pauseDaemonRig: r, listener: listener, key: r.issue(1)}
	r.o.outbox = func(o *outbox) {
		d.runnerMu.Lock()
		defer d.runnerMu.Unlock()
		d.runner = o
	}
	r.o.engine = engine
	refs := githubtest.NewRefs()
	r.o.githubAPI = ciGitHub(t, refs)
	w := &taskEndWalk{ciDaemon: d, refs: refs, besidePrefix: "legion-beside/" + d.key + "/", issueBranch: claim.IssueBranch(d.key)}
	for role, branch := range map[claim.Role]*string{claim.RoleTester: &w.testerBranch, claim.RoleImplementer: &w.fixBranch} {
		name, err := claim.BesideBranch(d.key, 1, "a", 1, role)
		if err != nil || !strings.HasPrefix(name, w.besidePrefix) {
			t.Fatalf("the %s's branch beside fix 1 of task a = %q, %v; want one under %s", role, name, err, w.besidePrefix)
		}
		*branch = name
	}
	refs.Set(liveRepo, w.issueBranch, taskEndX)
	w.seedTesting()
	d.boot()
	return w
}

// seedTesting writes the walk's issue standing in testing: the tree, its pull request open at X,
// the plan, the tester's phases row at its entry, and the tester's open start at X, as the
// implementer's completion at X left them.
func (w *taskEndWalk) seedTesting() {
	w.t.Helper()
	w.seed(w.root(w.key, phase.Testing))
	w.pullRequest(w.key, ciPullRequest, taskEndX)
	given := time.Now().UTC().Add(-time.Hour)
	w.write("the plan of task a", func(ctx context.Context, tx pgx.Tx) error {
		v := record.PlanVersion{Issue: w.key, Generation: 1, Version: 1, ArtifactID: "plan-" + w.key,
			Source: record.PlanSourceOwner, GivenBy: "owner", GivenAt: given}
		tasks := []record.PlanTask{{Issue: w.key, Generation: 1, Task: "a", PlanVersion: 1, BlockText: "Task a.",
			State: record.TaskWaiting, FixesCI: true}}
		steps := []record.PlanStep{
			{Issue: w.key, Generation: 1, Task: "a", Position: 1, Role: "implementer", Type: "implementation"},
			{Issue: w.key, Generation: 1, Task: "a", Position: 2, Role: "tester", Type: "testing"},
			{Issue: w.key, Generation: 1, Task: "a", Position: 3, Role: "reviewer", Type: "review"},
		}
		if err := w.records.PutPlan(ctx, tx, v, tasks, steps); err != nil {
			return err
		}
		return w.records.MoveTask(ctx, tx, w.key, 1, "a", record.TaskPlace{Kind: record.PlaceStep, Position: 2}, record.TaskRunning)
	})
	w.entry(claim.RoleTester, 1)
	at := record.StepPlace{Task: "a", Kind: record.PlaceStep, Place: 2}
	started, head := given.Add(30*time.Minute), taskEndX
	w.write("the tester's open start", func(ctx context.Context, tx pgx.Tx) error {
		_, err := w.records.RaiseStepStarts(ctx, tx, w.key, at, func(count int64) record.StepRecord {
			return record.StepRecord{ID: workflow.StepID(w.key, 1, at, count), Task: "a", Step: 2, Kind: record.PlaceStep, Role: "tester",
				Type: "testing", Agent: string(w.token(w.key, claim.RoleTester)), Generation: 1, StartedAt: &started, StartCommit: &head}
		})
		return err
	})
}

// write runs write in one transaction over the daemon's database, failing the test on its error.
func (w *taskEndWalk) write(what string, write func(ctx context.Context, tx pgx.Tx) error) {
	w.t.Helper()
	if err := store.Transact(w.ctx, w.pool, write); err != nil {
		w.t.Fatalf("%s: %v", what, err)
	}
}

// stands is the walk's issue as the store holds it now.
func (w *taskEndWalk) stands() record.Issue {
	w.t.Helper()
	var issue *record.Issue
	w.write("read the issue", func(ctx context.Context, tx pgx.Tx) error {
		var err error
		issue, err = w.records.Issue(ctx, tx, w.key)
		return err
	})
	if issue == nil {
		w.t.Fatalf("the store holds no issue %s", w.key)
	}
	return *issue
}

// fixOne is the latest fix of task a as the store holds it now, the zero fix where it holds none.
func (w *taskEndWalk) fixOne() record.TaskFix {
	w.t.Helper()
	var fix *record.TaskFix
	w.write("read the latest fix", func(ctx context.Context, tx pgx.Tx) error {
		var err error
		fix, err = w.records.LatestFix(ctx, tx, w.key, 1, "a")
		return err
	})
	if fix == nil {
		return record.TaskFix{}
	}
	return *fix
}

// testerStart is the latest start of the tester's step, place 2 of task a.
func (w *taskEndWalk) testerStart() record.StepStart {
	w.t.Helper()
	var start record.StepStart
	w.write("read the tester's start", func(ctx context.Context, tx pgx.Tx) error {
		var err error
		start, err = w.records.StepStarts(ctx, tx, w.key, record.StepPlace{Task: "a", Kind: record.PlaceStep, Place: 2})
		return err
	})
	return start
}

// checkAt applies Dispatch's check.state_changed of state at head, carrying taskEndChecks as the
// head's set, at the walk's next serial, as the live consumer applies it, and answers its serial.
func (w *taskEndWalk) checkAt(head string, state intake.DispatchCheckState) int64 {
	w.t.Helper()
	w.serial++
	state.Checks, state.Serial = slices.Clone(taskEndChecks), w.serial
	fact, id := checkstatetest.Decoded(w.t, w.key, liveRepo, head, state)
	w.applyFact("dispatch", id, fact)
	return w.serial
}

// completes applies role's completion at commit, naming the handoff record handoff, the states
// serial of a tester's test record and the fix whose task it ends (0 for none), as the handoff
// route applies it.
func (w *taskEndWalk) completes(role claim.Role, commit, handoff string, serial int64, fix int) {
	w.t.Helper()
	w.applyFact("api", "complete-"+handoff, intake.HandoffComplete{Generation: 1, Issue: w.key, Role: role, Claim: w.token(w.key, role),
		Summary: fmt.Sprintf("%s done at %s", role, commit), Commit: commit, HandoffRecord: handoff, HandoffVersion: 1,
		StatesSerial: serial, Fix: fix})
}

// pushesBeside is the agents' pushes beside running fix 1: the tester's to its branch B, and the
// fix's to its own branch F. It reports UNKNOWN and fails where the planting leaves no branch
// under the issue's prefix, since every later read of no branch would then pass.
func (w *taskEndWalk) pushesBeside() {
	w.t.Helper()
	w.refs.Set(liveRepo, w.testerBranch, taskEndTested)
	w.refs.Set(liveRepo, w.fixBranch, taskEndFixed)
	held := w.refs.Branches(liveRepo, w.besidePrefix)
	if len(held) == 0 {
		w.t.Fatalf("UNKNOWN: GitHub holds no branch under %s before the task's end, so no read after it can tell a deletion from nothing to delete", w.besidePrefix)
	}
	if want := slices.Sorted(slices.Values([]string{w.testerBranch, w.fixBranch})); !slices.Equal(held, want) {
		w.t.Fatalf("GitHub holds %v under %s before the task's end, want %v", held, w.besidePrefix, want)
	}
}

// bothStand fails unless B and F both stand and no deletion of the branches beside a fix is
// queued for the issue, when names the moment.
func (w *taskEndWalk) bothStand(when string) {
	w.t.Helper()
	if held := standing(w.refs, w.testerBranch, w.fixBranch); len(held) != 2 {
		w.t.Fatalf("%s %v of %s and %s stand, want both", when, held, w.testerBranch, w.fixBranch)
	}
	if left := w.left(record.OutboxKindBesideBranchesDelete); left != 0 {
		w.t.Fatalf("%s %d deletions of the branches beside a fix are queued, want none", when, left)
	}
}

// walkTaskToItsEnd walks the task from its testing step to its retro, with engine set on the
// daemon's engine (nil for no seam), through each barrier the test names, and answers once the
// issue stands in its retro and no deletion of the branches beside a fix is left for it.
func walkTaskToItsEnd(t *testing.T, engine func(*workflow.Engine)) *taskEndWalk {
	t.Helper()
	w := newTaskEndWalk(t, engine)

	// k-ci fails at X while the tester runs: fix 1 opens beside it, reads its job and runs, and the
	// tester's open start is bound to its own branch B.
	w.checkAt(taskEndX, intake.DispatchCheckState{Check: "k-ci", State: "no", Reason: "fail", CICase: "failure", Roles: []string{"ci"}})
	w.until("", "fix 1 to run beside the tester", func() bool { return w.fixOne().State == record.FixRunning })
	if start := w.testerStart(); start.Run != record.RunOpen || start.PushBranch != w.testerBranch {
		t.Fatalf("the tester's start beside running fix 1 = %+v, want its run open and bound to %s", start, w.testerBranch)
	}
	w.pushesBeside()

	// Fix 1 ends with commits of its own at Y while the tester runs: its way waits for the tester.
	w.pullRequest(w.key, ciPullRequest, taskEndY)
	w.completes(claim.RoleImplementer, taskEndY, "fix-1", 0, 1)
	if fix := w.fixOne(); fix.State != record.FixEnded || fix.EndHead != taskEndY || fix.Way != 0 {
		t.Fatalf("fix 1 after its end at %s beside the running tester = %+v, want ended there with its way pending", taskEndY, fix)
	}

	// The tester fails at X, so way 1 returns the task to the implementer and deletes nothing.
	failed := w.checkAt(taskEndX, intake.DispatchCheckState{Check: "k1", State: "no", Reason: "fail"})
	w.completes(claim.RoleTester, taskEndX, "test-1", failed, 0)
	if fix, issue := w.fixOne(), w.stands(); fix.Way != 1 || issue.Phase != phase.Implementing {
		t.Fatalf("after the tester's fail fix 1 took way %d and the issue stands in %s, want way 1 and implementing", fix.Way, issue.Phase)
	}
	w.bothStand("after way 1")

	// The implementer completes at Y, and testing passes there while k-ci runs.
	w.completes(claim.RoleImplementer, taskEndY, "implement-2", 0, 0)
	if issue := w.stands(); issue.Phase != phase.Testing {
		t.Fatalf("after the implementer's completion at %s the issue stands in %s, want testing", taskEndY, issue.Phase)
	}
	w.checkAt(taskEndY, intake.DispatchCheckState{Check: "k-ci", State: "no", Reason: "no_result", CICase: record.CINotCompleted, Roles: []string{"ci"}})
	passed := w.checkAt(taskEndY, intake.DispatchCheckState{Check: "k1", State: "yes", Roles: []string{"tester"}})
	w.completes(claim.RoleTester, taskEndY, "test-2", passed, 0)
	if issue := w.stands(); issue.Phase != phase.Reviewing {
		t.Fatalf("after the tester's pass at %s the issue stands in %s, want reviewing", taskEndY, issue.Phase)
	}

	// The review passes at Y: GitHub reads Y clean, and an account with write access approves it.
	// The task's end is held for k-ci, which still runs at Y.
	now := time.Now().UTC()
	w.applyFact("github", "mergeability-y", intake.PullRequestMergeability{Repo: liveRepo, Number: ciPullRequest, Base: "main", HeadSHA: taskEndY,
		Mergeable: record.MergeabilityMergeable, MergeState: record.MergeStateClean, ReadAt: now})
	w.applyFact("github", "review-y", intake.PullRequestReview{Repo: liveRepo, Number: ciPullRequest, ID: 7001, SubmittedAt: now,
		State: "APPROVED", CommitID: taskEndY, HeadSHA: taskEndY, Author: "a-maintainer", AuthorCanWrite: true, Body: "Approved."})
	if issue := w.stands(); issue.Phase != phase.Held || issue.Hold == nil || issue.Hold.From != phase.Retro || issue.Hold.Reason != record.HoldForCI {
		t.Fatalf("after the review's approval at %s the issue stands in %s with hold %+v, want held before its retro for its CI", taskEndY, issue.Phase, issue.Hold)
	}
	w.bothStand("while the task's end is held for its CI")

	// CI passes at Y: the hold is released into the retro, and no step of the task runs again.
	w.checkAt(taskEndY, intake.DispatchCheckState{Check: "k-ci", State: "yes", CICase: "success", Roles: []string{"ci"}})
	if issue := w.stands(); issue.Phase != phase.Retro || issue.Hold != nil {
		t.Fatalf("after k-ci passed at %s the issue stands in %s with hold %+v, want its retro", taskEndY, issue.Phase, issue.Hold)
	}
	w.until("", "no deletion of the branches beside a fix to be left for the issue", func() bool {
		return w.left(record.OutboxKindBesideBranchesDelete) == 0
	})
	return w
}

// No branch beside a fix is left once the task ends. Task a takes a fix, and fix 1 runs beside the
// tester; GitHub holds the tester's branch B and the fix's own branch F, as the agents' pushes
// leave them. Fix 1 ends with commits of its own at Y while the tester runs, and the tester then
// fails, so way 1 returns the task to the implementer and deletes neither branch. The implementer
// completes at Y, testing and the review pass there, and CI passes there last, which releases the
// task's end into its retro: no step of the task can run again, so the end's deletion takes every
// branch beside the task's fixes, and no branch under legion-beside/<issue>/ is left. The issue's
// own branch stands.
//
// The control, kept as a subtest of its own daemon: with the end's deletion taken out of the
// engine (WithoutEndBesideDelete), both branches stand after the same barriers.
func TestNoBesideBranchIsLeftAfterTheTaskEnds(t *testing.T) {
	t.Parallel()
	t.Run("the end of the task deletes the tester's branch and the fix's", func(t *testing.T) {
		t.Parallel()
		w := walkTaskToItsEnd(t, nil)
		if left := w.refs.Branches(liveRepo, w.besidePrefix); len(left) != 0 {
			t.Fatalf("after the task's end %v stand under %s, want none: the end deleted no branch beside its fix", left, w.besidePrefix)
		}
		for _, branch := range []string{w.testerBranch, w.fixBranch} {
			if deleted := w.besideLines(besideDeletedLine, branch); len(deleted) != 1 {
				t.Fatalf("the daemon logged %d deletions of %s, want one: the end's deletion of the branch", len(deleted), branch)
			}
		}
		if held := standing(w.refs, w.issueBranch); len(held) != 1 {
			t.Fatalf("after the task's end the issue's branch %s stands %v, want it to stand", w.issueBranch, held)
		}
	})
	t.Run("control: WithoutEndBesideDelete leaves both branches after the same barriers", func(t *testing.T) {
		t.Parallel()
		w := walkTaskToItsEnd(t, func(e *workflow.Engine) { e.WithoutEndBesideDelete() })
		w.bothStand("with the end's deletion taken out, after the task's end")
		for _, branch := range []string{w.testerBranch, w.fixBranch} {
			if deleted := w.besideLines(besideDeletedLine, branch); len(deleted) != 0 {
				t.Fatalf("with the end's deletion taken out the daemon logged %d deletions of %s, want none", len(deleted), branch)
			}
		}
	})
}

package daemon

import (
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// fenceDropLine is the supervise fence's line for a row it finishes without acting because the row
// serves a generation other than the one the issue's record holds for it (the outbox's supervise).
const fenceDropLine = "outbox supervise row serves an earlier generation; finished without acting"

// nextChange raises the root's own generation as its next change does, and leaves its tree's run
// where it stands: PutIssue writes a record of the same tree with the tree_generation the store
// holds. The root's architect runs on that run (record.Issue.ArchitectGeneration).
func (r *mainPathRig) nextChange() record.Issue {
	r.t.Helper()
	before := r.issue()
	raised := before
	raised.Generation++
	r.put(raised)
	after := r.issue()
	if after.Generation != before.Generation+1 || after.ArchitectGeneration() != before.ArchitectGeneration() {
		r.t.Fatalf("the root after its next change stands at generation %d with its architect on run %d, want generation %d on run %d",
			after.Generation, after.ArchitectGeneration(), before.Generation+1, before.ArchitectGeneration())
	}
	return after
}

// watchFence gives the rig's outbox a log the test reads, and returns it.
func (r *mainPathRig) watchFence() *syncBuffer {
	logs, log := watchedLog()
	r.runner.log = log
	return logs
}

// newestArchitectStart is the newest start of the root's architect the outbox holds.
func (r *mainPathRig) newestArchitectStart() entryStartRow {
	r.t.Helper()
	starts := roleStartRows(r.t, r.pool, r.key, claim.RoleArchitect)
	if len(starts) == 0 {
		r.t.Fatalf("%s holds no start of its architect", r.key)
	}
	return starts[len(starts)-1]
}

// assertBackOnTheTreesRun holds the root architect's claim to what start, the newest start of it,
// was queued to do: bring the claim back from its release, holding task where task is not empty.
// The fence finishes a root architect's start of any generation but its tree's run without
// acting, and the claim then stays released.
func (r *mainPathRig) assertBackOnTheTreesRun(logs *syncBuffer, start entryStartRow, root record.Issue, what, task string) {
	r.t.Helper()
	held := r.machine().Claim()
	dropped := count(logs, fenceDropLine)
	if held.Released() || dropped != 0 || (task != "" && (held.Pending == nil || held.Pending.Task != task)) {
		r.t.Fatalf("%s serves generation %d. The root stands at generation %d, and its architect runs on the tree's run %d. "+
			"The fence finished %d starts without acting, and the architect is %s by %q holding %#v. Want it back on the tree's run",
			what, start.request.Generation, root.Generation, root.ArchitectGeneration(), dropped, held.State, held.SuspendedBy, held.Pending)
	}
}

// A root's next change raises the root's own generation and leaves its tree's run, which its
// architect runs on (record.Issue.ArchitectGeneration), and the supervise fence holds every start
// of that architect to that run. So a wake of the architect's released pod after the change queues
// its start on the tree's run (workflow's wakeRow), and the start brings the architect back with
// the wake's task. A start of the root's own generation is finished without acting, with the wake
// already taken, and the architect stays released.
func TestAWokenRootArchitectComesBackOnTheTreesRunAfterItsRootsNextChange(t *testing.T) {
	t.Parallel()
	r := newMainPathRig(t, true)
	r.put(r.waitingRoot())
	r.admitPass()
	r.runStarts()
	r.bringUp()
	r.takeTheTask()
	root := r.nextChange()
	r.release()
	r.recordClaim()

	logs := r.watchFence()
	wake := r.wakeByRoleMessage("role-message-1")
	start := r.newestArchitectStart()
	r.runStarts()
	r.assertBackOnTheTreesRun(logs, start, root, "The wake's start", wake)
}

// The resume a cancelled idle release queues for each claim it had suspended (the outbox's
// releaseResume, store.CancelIssueRelease) is a start of that role. For a root's architect after
// the root's next change, it is a start on the tree's run, so it brings the architect back. A
// resume of the root's own generation is finished without acting, the release's mark is already
// gone, and the architect stays released until a wake.
func TestTheResumeOfACancelledReleaseBringsARootArchitectBackOnTheTreesRunAfterItsRootsNextChange(t *testing.T) {
	t.Parallel()
	r := newMainPathRig(t, true)
	r.put(r.waitingRoot())
	r.admitPass()
	r.runStarts()
	r.bringUp()
	r.takeTheTask()
	root := r.nextChange()
	r.release()
	r.recordClaim()
	if _, err := r.pool.Exec(r.ctx, `insert into issue_releases
		(project, issue, generation, first_row, boundary_row, last_row, started_row, role, suspended, marked_at)
		values ('legion', $1, $2, 1, 2, 3, 0, 'architect', $3, now())`,
		r.key, int64(root.Generation), []string{string(claim.RoleArchitect)}); err != nil {
		t.Fatalf("mark the release of the pod of %s: %v", r.key, err)
	}

	r.runner.trees = r.trees
	logs := r.watchFence()
	if err := r.runner.cancelRelease(r.ctx, r.key, r.key, workflow.IdleHoldOvertaken, "the pod was given work after its release was marked"); err != nil {
		t.Fatalf("cancel the release of the pod of %s: %v", r.key, err)
	}
	start := r.newestArchitectStart()
	r.runStarts()
	r.assertBackOnTheTreesRun(logs, start, root, "The resume of the cancelled release", "")
}

// nextChange raises the root's own generation as its next change does, and leaves its tree's run,
// which its architect runs on (record.Issue.ArchitectGeneration). It returns the root as the store
// reads it back.
func (r *newsReconcileRig) nextChange(root record.Issue) record.Issue {
	r.t.Helper()
	run := root.ArchitectGeneration()
	root.Generation++
	putOutboxIssue(r.t, r.pool, r.records, root)
	var got *record.Issue
	r.inTx(func(tx pgx.Tx) error {
		var err error
		got, err = r.records.Issue(r.ctx, tx, root.Key)
		return err
	})
	if got == nil || got.Generation != root.Generation || got.ArchitectGeneration() != run {
		r.t.Fatalf("the root after its next change = %+v, want generation %d with its architect on run %d", got, root.Generation, run)
	}
	return *got
}

// rootArchitectOwedItsSummary records the root's architect as the record holds it once it took
// version 4 of the spec, which stands approved at 5: an idle claim given entry 2, the entry its
// phases row names, so a news-only start reaches it (the reconcile's recordedClaimTakesNews). The
// root's phase holder took version 5 and is owed nothing.
func (r *newsReconcileRig) rootArchitectOwedItsSummary() {
	r.t.Helper()
	r.told(reconcileRoot, record.AudienceHolder, 5)
	r.told(reconcileRoot, record.AudienceArchitect, 4)
	token := r.holder(reconcileRoot, claim.RoleArchitect, supervise.StateIdle, "", 2)
	r.inTx(func(tx pgx.Tx) error {
		return r.records.PutPhase(r.ctx, tx, record.PhaseRow{Issue: reconcileRoot, Role: claim.RoleArchitect, Claim: token, Entry: 2,
			EntryTask: "Carry the tree on.", EntryRaisedAt: time.Now().UTC()})
	})
}

// startsAt names each news-only start by who it goes to and the generation it serves.
func startsAt(starts []entryStartRow) []string {
	named := make([]string, len(starts))
	for i, start := range starts {
		named[i] = fmt.Sprintf("%s/%s at generation %d", start.row.Issue, start.request.Role, start.request.Generation)
	}
	return named
}

// The reconcile of a tree's news tells the root's architect its summary by a news-only start
// (the outbox's settleNews). After the root's next change that start serves the tree's run, which
// the architect runs on, so the fence lets it through and it gives the architect its summary. A
// start of the root's own generation is finished without acting, and the summary stays owed while
// a later reconcile queues the same start again.
func TestARootArchitectsNewsOnlyStartReachesItOnTheTreesRunAfterItsRootsNextChange(t *testing.T) {
	t.Parallel()
	r := newNewsReconcileRig(t)
	root := r.nextChange(r.member(reconcileRoot, "", phase.Implementing))
	r.rootArchitectOwedItsSummary()

	if err := r.reconcile(0); err != nil {
		t.Fatalf("reconcile the tree's news: %v", err)
	}
	starts := r.newsStarts()
	if got := startsTo(starts); len(got) != 1 || got[0] != reconcileRoot+"/architect@" {
		t.Fatalf("the news-only starts = %v, want the root architect's one", startsAt(starts))
	}
	machine := r.machine(reconcileRoot, claim.RoleArchitect, supervise.StateIdle, "", 2)
	logs, log := watchedLog()
	r.runner.log = log
	if err := r.runner.execute(r.ctx, starts[0].row); err != nil {
		t.Fatalf("run the root architect's news-only start: %v", err)
	}
	machine.Wait()
	if pending, dropped := machine.Claim().Pending, count(logs, fenceDropLine); pending == nil || dropped != 0 {
		t.Fatalf("The root architect's news-only start serves generation %d. The root stands at generation %d, and its architect runs on the tree's run %d. "+
			"The fence finished %d starts without acting, and the architect holds %#v. Want the start to give it its summary",
			starts[0].request.Generation, root.Generation, root.ArchitectGeneration(), dropped, pending)
	}
	if _, news := pendingNews(t, machine); len(news.Items) != 1 || news.Items[0].Audience != record.AudienceArchitect || news.Items[0].To != 5 {
		t.Fatalf("the root architect's task carries %+v, want its summary of version 5", news)
	}
}

// The reconcile queues a claim's news-only start only where none is queued for it already
// (newsStartQueued), and it reads that from the claim's run (readOwedHolders, record.Store.RoleRuns).
// The run of a root's architect is the tree's run, which the root's next change leaves. So a
// news-only start the architect was queued on that run, still waiting, is the architect's one start
// after the change, and the reconcile queues no second one beside it.
func TestAQueuedNewsOnlyStartOfTheTreesRunIsTheRootArchitectsOnlyOneAfterItsRootsNextChange(t *testing.T) {
	t.Parallel()
	r := newNewsReconcileRig(t)
	root := r.nextChange(r.member(reconcileRoot, "", phase.Implementing))
	r.rootArchitectOwedItsSummary()
	r.inTx(func(tx pgx.Tx) error {
		row, err := record.NewOutboxRow(reconcileRoot, record.SuperviseRequest{Op: "start", Tree: reconcileRoot, Role: claim.RoleArchitect,
			Generation: root.ArchitectGeneration(), News: true}, time.Now().UTC())
		if err != nil {
			return err
		}
		return r.records.Enqueue(r.ctx, tx, row)
	})

	if err := r.reconcile(0); err != nil {
		t.Fatalf("reconcile the tree's news: %v", err)
	}
	if starts := r.newsStarts(); len(starts) != 1 {
		t.Fatalf("the news-only starts after the reconcile = %v. Want only the one already queued on the tree's run %d. "+
			"The root stands at generation %d, and a run read at that generation misses the queued start",
			startsAt(starts), root.ArchitectGeneration(), root.Generation)
	}
}

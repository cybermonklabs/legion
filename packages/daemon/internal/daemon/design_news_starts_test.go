package daemon

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// The reconcile sends a member's news to the claim that holds its phase, by a news-only start of
// its own: a start of no entry and no task, naming the phase it tells, which reads what it carries
// when it runs. Run against a reviewer that was given its phase's entry and is between turns, it
// hands the reviewer its news as a task.
func TestANewsOnlyStartIsQueuedForALiveReviewer(t *testing.T) {
	t.Parallel()
	r := newNewsReconcileRig(t)
	r.member(reconcileRoot, "", phase.Reviewing)
	r.told(reconcileRoot, record.AudienceHolder, 4)
	r.holder(reconcileRoot, claim.RoleReviewer, supervise.StateIdle, "", 1)

	if err := r.reconcile(0); err != nil {
		t.Fatalf("reconcile the tree's news: %v", err)
	}
	starts := r.newsStarts()
	want := record.SuperviseRequest{Op: "start", Tree: reconcileRoot, Role: claim.RoleReviewer, Generation: 1, Phase: phase.Reviewing, News: true}
	if len(starts) != 1 || starts[0].row.Issue != reconcileRoot || !reflect.DeepEqual(starts[0].request, want) {
		t.Fatalf("the news-only starts = %+v, want one %+v on %s", starts, want, reconcileRoot)
	}

	machine := r.machine(reconcileRoot, claim.RoleReviewer, supervise.StateIdle, "", 1)
	if err := r.runner.execute(r.ctx, starts[0].row); err != nil {
		t.Fatalf("run the news-only start: %v", err)
	}
	task, news := pendingNews(t, machine)
	if !strings.Contains(task, "version 5, since version 4, which your phase last took") || !strings.Contains(task, `"Design" changed`) {
		t.Fatalf("the reviewer's task = %q, want the spec's pair and its change list", task)
	}
	if !machine.Claim().Pending.NewsOnly || !reflect.DeepEqual(news.Items, []record.DeliveryNewsItem{heldItem(reconcileRoot, 4)}) {
		t.Fatalf("the reviewer's task carries %+v, want the news-only item of the spec from 4 to 5", news)
	}
}

// A news-only start goes only where nothing else will carry the news, and never where the record
// says nothing may: not to a claim owed its phase start (that start carries the news), not to a
// retired or failed claim (the phase's own repair revives it with a start that carries the news),
// not to an agent a suspension that holds is over (the operator's or the controller's, which no
// start the daemon queues gets past), and not into a tree that lingers or to an issue not at work,
// which are owed nothing. The reconcile queues none for such a claim, and a
// news-only start that meets one when it runs finishes without acting. The live claim is the
// control: the same reconcile queues its start, and the same start hands it the news.
func TestNoNewsOnlyStartGoesWhereTheRecordSaysNoNewsMay(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		state supervise.ClaimState
		by    supervise.SuspendSource
		given int64
		// lingers puts the tree in its linger, and held stands the member in held.
		lingers, held bool
		queued        bool
	}{
		{name: "a live claim", state: supervise.StateIdle, given: 1, queued: true},
		{name: "a claim owed its phase start", state: supervise.StateIdle, given: 0},
		{name: "a retired claim", state: supervise.StateRetired, given: 1},
		{name: "a failed claim", state: supervise.StateFailed, given: 1},
		{name: "an operator-suspended claim", state: supervise.StateSuspended, by: supervise.SuspendedByOperator, given: 1},
		{name: "an agent the controller suspended", state: supervise.StateSuspended, by: supervise.SuspendedByController, given: 1},
		{name: "a lingering tree", state: supervise.StateIdle, given: 1, lingers: true},
		{name: "an issue not at work", state: supervise.StateIdle, given: 1, held: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newNewsReconcileRig(t)
			root := r.member(reconcileRoot, "", phase.Reviewing)
			r.told(reconcileRoot, record.AudienceHolder, 4)
			r.holder(reconcileRoot, claim.RoleReviewer, tc.state, tc.by, tc.given)
			if tc.lingers {
				lingers := time.Now().Add(time.Hour)
				root.LingerUntil = &lingers
				putOutboxIssue(t, r.pool, r.records, root)
			}
			if tc.held {
				root.Phase, root.Hold = phase.Held, &record.Hold{From: phase.Reviewing}
				putOutboxIssue(t, r.pool, r.records, root)
			}

			if err := r.reconcile(0); err != nil {
				t.Fatalf("reconcile the tree's news: %v", err)
			}
			starts := r.newsStarts()
			if queued := len(starts) == 1; queued != tc.queued || len(starts) > 1 {
				t.Fatalf("the news-only starts = %v, want one queued: %t", startsTo(starts), tc.queued)
			}
			if _, recorded := r.recorded(reconcilePair); recorded == (tc.lingers || tc.held) {
				t.Fatalf("the pair recorded = %t, want it recorded only where a member is owed it", recorded)
			}

			// The start as it would meet the claim when it runs, queued or not.
			machine := r.machine(reconcileRoot, claim.RoleReviewer, tc.state, tc.by, tc.given)
			row := mustOutboxRow(t, reconcileRoot, record.SuperviseRequest{Op: "start", Tree: reconcileRoot, Role: claim.RoleReviewer,
				Generation: 1, Phase: phase.Reviewing, News: true}, time.Now())
			if tc.held {
				row = mustOutboxRow(t, reconcileRoot, record.SuperviseRequest{Op: "start", Tree: reconcileRoot, Role: claim.RoleReviewer,
					Generation: 1, Phase: phase.Held, News: true}, time.Now())
			}
			if err := r.runner.execute(r.ctx, row); err != nil {
				t.Fatalf("run the news-only start: %v", err)
			}
			machine.Wait()
			if delivered := machine.Claim().Pending != nil; delivered != tc.queued {
				t.Fatalf("the claim holds %+v after the news-only start, want a task delivered: %t", machine.Claim().Pending, tc.queued)
			}
			if calls := r.rt.Calls(); !tc.queued && len(calls) != 0 {
				t.Fatalf("the news-only start made %d runtime calls, want none: it finishes without acting", len(calls))
			}
		})
	}
}

// A claim whose worker has not yet taken the task it holds — a delivery pending with no
// confirmation — takes no second task before it does: a news-only start queued then would only
// wait behind it, retrying on its backoff. So the reconcile queues none, and the news stays owed
// until a turn takes that task. The root architect's summary says so of the member
// (workflow.NewsWaitsForTask), never that a task tells it now nor that its next start carries it.
// A claim whose pending task a turn has taken is the control: its confirmation is recorded, the
// reconcile queues its start, and the summary says a task tells it now. So is a claim that holds
// only the daemon's own delivery of a relaunch's record, unconfirmed: that delivery is no task
// (supervise.HoldsTask), and a newer task takes its place while no prompt of it waits on a result,
// carrying its sentence in front of the news.
func TestNoNewsOnlyStartIsQueuedBehindATaskTheHolderHasNotTaken(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		relaunch  bool
		confirmed bool
		queued    bool
	}{
		{name: "a task not taken yet"},
		{name: "a task a turn took", confirmed: true, queued: true},
		{name: "the daemon's own delivery of a relaunch's record, not taken yet", relaunch: true, queued: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newNewsReconcileRig(t)
			r.member(reconcileRoot, "", phase.Reviewing)
			r.told(reconcileRoot, record.AudienceHolder, 4)
			token := r.holder(reconcileRoot, claim.RoleReviewer, supervise.StateWorking, "", 1)
			// The phase's task names its phase and run; the relaunch's delivery names neither, and
			// its text is the record's sentence alone (supervise's queueRelaunch).
			id, task, generation, worked := "outbox:7", "Review the change.", 1, string(phase.Reviewing)
			if tc.relaunch {
				id, task, generation, worked = "relaunch:QXKZ7TRWJ5", "Your previous process ended.", 0, ""
			}
			r.inTx(func(tx pgx.Tx) error {
				_, err := tx.Exec(r.ctx, `insert into pending_task_deliveries (claim_token, delivery_id, task, queued_at, generation, phase, confirmed_at)
					values ($1, $2, $3, now(), $4, $5, case when $6 then now() end)`, string(token), id, task, generation, worked, tc.confirmed)
				return err
			})

			if err := r.reconcile(0); err != nil {
				t.Fatalf("reconcile the tree's news: %v", err)
			}
			starts := r.newsStarts()
			if queued := len(starts) == 1; queued != tc.queued || len(starts) > 1 {
				t.Fatalf("the news-only starts = %v, want one queued: %t", startsTo(starts), tc.queued)
			}
			if took, _ := r.toldOf(reconcileRoot, record.AudienceHolder).Took(record.DesignStepSpec); took != 4 {
				t.Fatalf("the reviewer's told version = %d, want 4: the news stays owed until a task carrying it is taken", took)
			}
			var drafts []memberDraft
			r.inTx(func(tx pgx.Tx) error {
				root, err := r.records.Issue(r.ctx, tx, reconcileRoot)
				if err != nil {
					return err
				}
				drafts, err = readTreeMembers(r.ctx, tx, r.records, config.DesignGateRootIssues.Steps(), record.NewNewsClaims(*root, nil))
				return err
			})
			want := workflow.NewsWaitsForTask
			if tc.queued {
				want = workflow.NewsSentNow
			}
			if len(drafts) != 1 || drafts[0].state != want {
				t.Fatalf("the summary's members = %+v, want the reviewer's issue told %q", drafts, want)
			}
		})
	}
}

// A claim an idle pod release suspended still holds its role, and the news goes to it: the
// reconcile queues its news-only start, and the start wakes the claim as any start wakes one,
// resuming its kept session and handing it the news.
func TestANewsOnlyStartWakesAClaimAnIdleReleaseSuspended(t *testing.T) {
	t.Parallel()
	r := newNewsReconcileRig(t)
	r.member(reconcileRoot, "", phase.Reviewing)
	r.told(reconcileRoot, record.AudienceHolder, 4)
	r.holder(reconcileRoot, claim.RoleReviewer, supervise.StateSuspended, supervise.SuspendedByReleased, 1)

	if err := r.reconcile(0); err != nil {
		t.Fatalf("reconcile the tree's news: %v", err)
	}
	starts := r.newsStarts()
	if got := startsTo(starts); !reflect.DeepEqual(got, []string{reconcileRoot + "/reviewer@reviewing"}) {
		t.Fatalf("the news-only starts = %v, want the released reviewer's", got)
	}

	machine := r.machine(reconcileRoot, claim.RoleReviewer, supervise.StateSuspended, supervise.SuspendedByReleased, 1)
	if err := r.runner.execute(r.ctx, starts[0].row); err != nil {
		t.Fatalf("run the news-only start: %v", err)
	}
	if resumes := resumesOf(r.rt, machine.Claim().Token); len(resumes) != 1 {
		t.Fatalf("the start resumed the released reviewer's session %d times, want once", len(resumes))
	}
	if _, news := pendingNews(t, machine); !reflect.DeepEqual(news.Items, []record.DeliveryNewsItem{heldItem(reconcileRoot, 4)}) {
		t.Fatalf("the woken reviewer's task carries %+v, want the spec from 4 to 5", news)
	}
}

// The root architect is owed its own summary of the tree, and the holder form of each member in
// accepting it owns: one news-only start carries both, queued once for the tree, and a second run
// of the reconcile while that start is still to run queues no second.
func TestTheRootArchitectsStartIsQueuedOncePerTree(t *testing.T) {
	t.Parallel()
	r := newNewsReconcileRig(t)
	r.member(reconcileRoot, "", phase.Implementing)
	r.member(reconcileChild, reconcileRoot, phase.Accepting)
	r.told(reconcileRoot, record.AudienceHolder, 5)
	r.told(reconcileRoot, record.AudienceArchitect, 4)
	r.told(reconcileChild, record.AudienceHolder, 4)
	r.holder(reconcileRoot, claim.RoleArchitect, supervise.StateWorking, "", 0)

	for run := range 2 {
		if err := r.reconcile(0); err != nil {
			t.Fatalf("reconcile %d of the tree's news: %v", run+1, err)
		}
	}
	starts := r.newsStarts()
	if got := startsTo(starts); !reflect.DeepEqual(got, []string{reconcileRoot + "/architect@"}) {
		t.Fatalf("the news-only starts after two reconciles = %v, want the root architect's one", got)
	}

	machine := r.machine(reconcileRoot, claim.RoleArchitect, supervise.StateIdle, "", 0)
	if err := r.runner.execute(r.ctx, starts[0].row); err != nil {
		t.Fatalf("run the root architect's news-only start: %v", err)
	}
	task, news := pendingNews(t, machine)
	for _, want := range []string{
		"A person approved a new version of the design of LEGION-600.",
		"The approved design of LEGION-600 changed for LEGION-601, which you hold in accepting:",
		"sign LEGION-601 off with `sign_off`",
	} {
		if !strings.Contains(task, want) {
			t.Fatalf("the root architect's task = %q, want it to say %q", task, want)
		}
	}
	if len(news.Items) != 2 {
		t.Fatalf("the root architect's task carries %+v, want its own item and the accepting member's", news)
	}
}

// A member in accepting is held by the architect that owns it: a sub-architect running for its
// parent owns it, so the news goes to that sub-architect's claim, in the holder form, and the root
// architect, owed nothing of its own, is queued nothing.
func TestASubArchitectsStartIsQueuedForItsAcceptingChild(t *testing.T) {
	t.Parallel()
	r := newNewsReconcileRig(t)
	r.member(reconcileRoot, "", phase.Implementing)
	r.member(reconcileChild, reconcileRoot, phase.Implementing)
	r.member(reconcileGrandchild, reconcileChild, phase.Accepting)
	r.told(reconcileRoot, record.AudienceHolder, 5)
	r.told(reconcileChild, record.AudienceHolder, 5)
	r.told(reconcileGrandchild, record.AudienceHolder, 4)
	r.holder(reconcileRoot, claim.RoleArchitect, supervise.StateWorking, "", 0)
	r.holder(reconcileChild, claim.RoleArchitect, supervise.StateWorking, "", 0)

	if err := r.reconcile(0); err != nil {
		t.Fatalf("reconcile the tree's news: %v", err)
	}
	starts := r.newsStarts()
	if got := startsTo(starts); !reflect.DeepEqual(got, []string{reconcileChild + "/architect@"}) {
		t.Fatalf("the news-only starts = %v, want the sub-architect's of %s alone", got, reconcileChild)
	}

	machine := r.machine(reconcileChild, claim.RoleArchitect, supervise.StateIdle, "", 0)
	if err := r.runner.execute(r.ctx, starts[0].row); err != nil {
		t.Fatalf("run the sub-architect's news-only start: %v", err)
	}
	task, news := pendingNews(t, machine)
	if !strings.Contains(task, "changed for LEGION-602, which you hold in accepting:") || strings.Contains(task, "Each issue of the tree") {
		t.Fatalf("the sub-architect's task = %q, want the holder form of its accepting child and no summary of the tree", task)
	}
	if !reflect.DeepEqual(news.Items, []record.DeliveryNewsItem{heldItem(reconcileGrandchild, 4)}) {
		t.Fatalf("the sub-architect's task carries %+v, want the accepting child's item", news)
	}
}

// An issue in accepting is told an approved new version through the architect that owns it by the
// owner walk (record.OwningIssue): the walk that routed the reviewer's approval notice which moved
// it there, and the architect sign_off accepts it from. A child whose own architect holds its role
// is owned by that architect, so the child's news goes to it, in the holder form, by a news-only
// start of its own, and the root's architect is handed none of it. The root's architect still gets
// its one summary of the tree, which names the child's own architect as the one told, never the
// root's architect as the one to answer. The control is TestTheRootArchitectsStartIsQueuedOncePerTree:
// the same tree with no architect on the child, whose news the root's architect carries and
// answers itself.
func TestAnAcceptingChildsOwnArchitectIsToldItsNewsAndTheRootsSummaryNamesIt(t *testing.T) {
	t.Parallel()
	r := newNewsReconcileRig(t)
	r.member(reconcileRoot, "", phase.Implementing)
	r.member(reconcileChild, reconcileRoot, phase.Accepting)
	r.told(reconcileRoot, record.AudienceHolder, 5)
	r.told(reconcileRoot, record.AudienceArchitect, 4)
	r.told(reconcileChild, record.AudienceHolder, 4)
	r.holder(reconcileRoot, claim.RoleArchitect, supervise.StateWorking, "", 0)
	r.holder(reconcileChild, claim.RoleArchitect, supervise.StateWorking, "", 0)

	if err := r.reconcile(0); err != nil {
		t.Fatalf("reconcile the tree's news: %v", err)
	}
	starts := r.newsStarts()
	if got := startsTo(starts); !reflect.DeepEqual(got, []string{reconcileRoot + "/architect@", reconcileChild + "/architect@"}) {
		t.Fatalf("the news-only starts = %v, want the root architect's summary and the start of %s's own architect", got, reconcileChild)
	}

	// Both architects run when either start does, so each start reads the other's claim as the
	// outbox holds it. The root's goes first, while the child's architect holds no task yet.
	root := r.machine(reconcileRoot, claim.RoleArchitect, supervise.StateIdle, "", 0)
	own := r.machine(reconcileChild, claim.RoleArchitect, supervise.StateIdle, "", 0)
	for _, start := range starts {
		if err := r.runner.execute(r.ctx, start.row); err != nil {
			t.Fatalf("run the news-only start of %s's architect: %v", start.row.Issue, err)
		}
	}

	const accepting = "The approved design of LEGION-600 changed for LEGION-601, which you hold in accepting:"
	task, news := pendingNews(t, own)
	for _, want := range []string{accepting, "sign LEGION-601 off with `sign_off`"} {
		if !strings.Contains(task, want) {
			t.Fatalf("the task of %s's own architect = %q, want it to say %q", reconcileChild, task, want)
		}
	}
	if strings.Contains(task, "A person approved a new version of the design of LEGION-600.") {
		t.Fatalf("the task of %s's own architect = %q, want no summary of the tree: the summary is the root architect's", reconcileChild, task)
	}
	if !reflect.DeepEqual(news.Items, []record.DeliveryNewsItem{heldItem(reconcileChild, 4)}) {
		t.Fatalf("the task of %s's own architect carries %+v, want the accepting child's item", reconcileChild, news)
	}

	summary, carried := pendingNews(t, root)
	if !strings.Contains(summary, "A person approved a new version of the design of LEGION-600.") {
		t.Fatalf("the root architect's task = %q, want its summary of the tree", summary)
	}
	if strings.Contains(summary, accepting) {
		t.Fatalf("the root architect's task = %q, want no holder form of %s: its own architect holds it", summary, reconcileChild)
	}
	if want := []record.DeliveryNewsItem{newsItem(reconcileRoot, record.AudienceArchitect, record.DesignStepSpec, reconcileDoc, 4, 5)}; !reflect.DeepEqual(carried.Items, want) {
		t.Fatalf("the root architect's task carries %+v, want its own item alone: %+v", carried.Items, want)
	}
	const prefix = "- LEGION-601, in accepting: "
	var line string
	for _, candidate := range strings.Split(summary, "\n") {
		if strings.HasPrefix(candidate, prefix) {
			line = candidate
		}
	}
	if want := prefix + "owed version 5 of the spec document, since version 4, which its phase last took; a task tells its own architect now."; line != want {
		t.Fatalf("the summary's line for %s = %q, want %q", reconcileChild, line, want)
	}
}

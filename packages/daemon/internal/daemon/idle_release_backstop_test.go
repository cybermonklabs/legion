package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// dispatch://INFRA-2746 R1 at the release of a pod (check
// c-idle-keeps-nothing-for-a-claim-that-owes-nothing, its daemon row and its second, third,
// fourth, fifth and seventh controls), through the writer a release's last step runs
// (outbox.armBackstops) on the pause rig's store, with no pause: the released worker of
// LEGION-208's open phase gets the backstop the release arms while it owes its phase by the owed
// answer's rule (record.PhaseOwed), and while it is of one of the two kinds of claim that owe
// nothing and still get one (holdsUndeliveredEntryTask, holdsUndeliveredNewsTask), and gets none
// otherwise.
// TestIdleOfAClaimThatOwesNothingArmsNoBackstopForItsRole drives a recorded completion through
// the daemon a deployment runs; these rows are shapes that rig does not build, each read from the
// whole claim and from the record in the store (store.ClaimTx, record.FillIdleClaims).
//
// The second control is the shape the review of 0a66d46a reproduced: the implementer took entry
// 1, was handed the task of entry 2, which its row stands on, and the send of that task failed
// before any turn, so the claim holds it unconfirmed, is given entry 2 and serves entry 1. That
// delivery waits for the claim to come back, and admission's owed-entry repair passes over a
// claim that holds a task of the issue's run and phase it still works
// (supervise.Delivery.StillWorked), so the release arms its backstop. The same claim with its
// completion of that entry recorded gets none, and so does the tester of testing holding such a
// task while a merge forward holds it. A claim behind its entry that holds no task of the issue's
// run and phase gets none either: the repair sends it that entry's task, which wakes its pod.
//
// The third control is a relaunch that kept the task it held (keepsStartTask): the implementer
// took entry 1 and holds the task of entry 2 unconfirmed, while its row stands on entry 3. That
// task is of an earlier entry than the row's, and the repair passes over the claim all the same,
// so the release arms its backstop: the exception reads a task of the issue's run and phase of
// the row's entry, of an earlier one or of no entry. The same claim with its completion of the
// row's entry recorded gets none, since the exception's other terms are read at the row's entry.
//
// The fourth control is a ready worker's resume task (workflow.Engine.ResumeTask), a task of no
// entry: the implementer took entry 1, its row stands on entry 3, and it holds a task of entry 0
// of the issue's run and phase unconfirmed. The repair passes over it as well, so the release
// arms its backstop. The third and fourth controls fail if the exception reads only a task of the
// row's entry (p.Entry == f.RowEntry).
//
// The fifth control is a task of news alone (supervise.Delivery.NewsOnly), the second kind, which
// the daemon sends a claim level with its row's entry whatever that claim completed
// (newsClaimTakes): LEGION-208's reviewer in reviewing took entry 1, its row stands on entry 1
// and records its approval, as OSSV-732's reviewer's approval stood while GitHub held the merge,
// and it holds an unconfirmed task of news alone of the issue's run and phase whose send failed
// before any turn. Nothing sends that news again, so the release arms its backstop, though the
// reviewer owes its phase nothing. The same reviewer gets none holding nothing, holding a task of
// news alone that a turn confirmed, or holding an unconfirmed one of another run or of another
// phase. Those last three rows fail if the second kind reads only the task's mark (p.NewsOnly),
// and the fifth control's backstop row fails without the second kind.
//
// The sixth control, a claim whose first task of the issue's run is not yet delivered, is
// TestIdleOfAClaimThatOwesNothingStillGetsABackstopForTheUndeliveredFirstTaskOfItsRun, on this
// writer and on both of a pause's: a fresh claim, which serves that task's run and entry
// (supervise.Claim.ServingAt) and owes its phase, and a claim kept across a new run, which still
// serves the earlier run and is of the first kind. Every row here serves the issue's run.
//
// A merger in merging whose pull request is merged gets none, since the merge finished its
// merging (record.MergedInMerging). The controls, an implementer that serves its row's entry and
// a merger whose pull request is open, each owe their phase and get one.
//
// The seventh control is a merger in merging with a merge forward of its pull request's head that
// has not landed (cybermonklabs/legion#4979). The merger the daemon asked to merge forward owes
// the merge, which is its own open work, so the release arms its backstop. The merger the move
// into merging held rather than asked, since the implementer's ask made in reviewing had not
// landed (workflow's holdStartedWorker), is held by that ask (record.MergeForwardHolds) and gets none:
// the landed message wakes its pod. Each reads the ask from the pull request in the store
// (record.FillIdleClaims), so a rule that holds the merger asked, or not the merger held, fails
// one of them.
func TestIdleOfAClaimThatOwesNothingStillGetsABackstopOnItsReleaseForAnUndeliveredTaskOfItsEntry(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		// at is the phase LEGION-208 stands in, and role the released worker of it.
		at   phase.Phase
		role claim.Role
		// served is the entry the worker last served, and rowEntry and completed its phases row:
		// the entry the row stands on and whether it records the completion. task is the entry
		// of the task the worker was handed and holds unconfirmed, its send failed before any
		// turn (undeliveredTask), and 0 for none.
		served, rowEntry, task int64
		completed              bool
		// noEntryTask is whether the worker holds, unconfirmed in the same way, a task of no
		// entry (entry 0) of the issue's run and phase in place of a task of an entry: a ready
		// worker's resume task (workflow.Engine.ResumeTask), which gives the claim no entry.
		noEntryTask bool
		// news is the task of news alone (supervise.Delivery.NewsOnly, newsTask) the worker
		// holds in place of any other, nil for none.
		news *supervise.Delivery
		// pullRequest is the state of LEGION-208's pull request, "" for none, and ask the
		// daemon's ask to merge the base forward onto the head that pull request stands at, not
		// landed, nil for none (mergeForwardAskOf).
		pullRequest record.PullRequestState
		ask         *record.MergeForward
		// backstop is whether the release arms the worker's backstop, and why says why.
		backstop bool
		why      string
	}{
		{name: "the control: it serves its row's entry and owes its phase", at: phase.Implementing, role: claim.RoleImplementer,
			served: 1, rowEntry: 1, backstop: true, why: "it owes its phase"},
		{name: "the second control: it took entry 1 and holds the task of entry 2, whose send failed before any turn",
			at: phase.Implementing, role: claim.RoleImplementer, served: 1, rowEntry: 2, task: 2, backstop: true,
			why: "its delivery waits for it to come back, and the backstop is the one wake that brings its pod back"},
		{name: "it is behind its entry and holds no task of it", at: phase.Implementing, role: claim.RoleImplementer,
			served: 1, rowEntry: 2, why: "admission's owed-entry repair sends it that entry's task, which wakes its pod"},
		{name: "it holds the task of its entry and its completion of that entry is recorded", at: phase.Implementing,
			role: claim.RoleImplementer, served: 1, rowEntry: 2, task: 2, completed: true,
			why: "its completion stands, so it owes nothing a wake could bring back"},
		{name: "the third control: it took entry 1, its row stands on entry 3, and it holds the task of entry 2, which a relaunch kept",
			at: phase.Implementing, role: claim.RoleImplementer, served: 1, rowEntry: 3, task: 2, backstop: true,
			why: "admission's owed-entry repair passes over a claim that holds a task it still works, so the backstop is the one wake that brings its pod back"},
		{name: "the fourth control: it took entry 1, its row stands on entry 3, and it holds a task of no entry of the issue's run and phase, a ready worker's resume task",
			at: phase.Implementing, role: claim.RoleImplementer, served: 1, rowEntry: 3, noEntryTask: true, backstop: true,
			why: "admission's owed-entry repair passes over a claim that holds a task it still works, of whatever entry, so the backstop is the one wake that brings its pod back"},
		{name: "it holds the task of an earlier entry and its completion of its row's entry is recorded", at: phase.Implementing,
			role: claim.RoleImplementer, served: 1, rowEntry: 3, task: 2, completed: true,
			why: "its completion of its row's entry stands, so it owes nothing a wake could bring back"},
		{name: "it holds the task of its entry and a merge forward holds it", at: phase.Testing, role: claim.RoleTester,
			served: 1, rowEntry: 2, task: 2, pullRequest: record.PullRequestOpen, ask: mergeForwardAskOf(claim.RoleImplementer, phase.Testing),
			why: "the merge forward told it to wait for the landed message, which wakes its pod"},
		{name: "the fifth control: a reviewer level with its row's entry, its approval of that entry recorded, holds an unconfirmed task of news alone of the issue's run and phase, whose send failed before any turn",
			at: phase.Reviewing, role: claim.RoleReviewer, served: 1, rowEntry: 1, completed: true,
			news: newsTask(phase.Reviewing, 1, false), backstop: true,
			why: "nothing sends that news again, so the backstop is the one wake that delivers it"},
		{name: "a reviewer level with its row's entry, its approval of that entry recorded, holds nothing",
			at: phase.Reviewing, role: claim.RoleReviewer, served: 1, rowEntry: 1, completed: true,
			why: "its approval stands and it holds nothing that waits for it to come back"},
		{name: "a reviewer level with its row's entry, its approval of that entry recorded, holds a task of news alone that a turn confirmed",
			at: phase.Reviewing, role: claim.RoleReviewer, served: 1, rowEntry: 1, completed: true,
			news: newsTask(phase.Reviewing, 1, true),
			why:  "a turn took that news, so nothing waits for it to come back"},
		{name: "a reviewer level with its row's entry, its approval of that entry recorded, holds an unconfirmed task of news alone of another run",
			at: phase.Reviewing, role: claim.RoleReviewer, served: 1, rowEntry: 1, completed: true,
			news: newsTask(phase.Reviewing, 2, false),
			why:  "a task of news alone of another run gets none"},
		{name: "a reviewer level with its row's entry, its approval of that entry recorded, holds an unconfirmed task of news alone of another phase",
			at: phase.Reviewing, role: claim.RoleReviewer, served: 1, rowEntry: 1, completed: true,
			news: newsTask(phase.Testing, 1, false),
			why:  "a task of news alone of another phase gets none"},
		{name: "a merger in merging whose pull request is merged", at: phase.Merging, role: claim.RoleMerger,
			served: 1, rowEntry: 1, pullRequest: record.PullRequestMerged, why: "the merge finished its merging"},
		{name: "the control: a merger in merging whose pull request is open", at: phase.Merging, role: claim.RoleMerger,
			served: 1, rowEntry: 1, pullRequest: record.PullRequestOpen, backstop: true, why: "it owes the merge"},
		{name: "the seventh control: the merger the daemon asked to merge forward in merging, its merge not landed", at: phase.Merging,
			role: claim.RoleMerger, served: 1, rowEntry: 1, pullRequest: record.PullRequestOpen, ask: mergeForwardAskOf(claim.RoleMerger, phase.Merging),
			backstop: true, why: "the merge forward is its own open work, so it owes the merge"},
		{name: "a merger held in merging by the implementer's ask made in reviewing, which has not landed", at: phase.Merging,
			role: claim.RoleMerger, served: 1, rowEntry: 1, pullRequest: record.PullRequestOpen, ask: mergeForwardAskOf(claim.RoleImplementer, phase.Reviewing),
			why: "the ask holds it to wait for the landed message, which wakes its pod"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newPauseRig(t)
			if _, err := r.pool.Exec(r.ctx, `update issues set phase = $2 where key = $1`, pauseIssue, string(c.at)); err != nil {
				t.Fatalf("move %s into %s: %v", pauseIssue, c.at, err)
			}
			worker := r.claim(c.role, supervise.StateSuspended)
			worker.ServedEntry, worker.GivenEntry = c.served, c.served
			switch {
			case c.task != 0:
				worker.GivenEntry, worker.Pending = c.task, undeliveredTask(c.at, c.task)
			case c.noEntryTask:
				worker.Pending = undeliveredTask(c.at, 0)
			case c.news != nil:
				task := *c.news
				worker.Pending = &task
			}
			r.putClaim(worker)
			r.putPhaseRow(c.role, c.rowEntry, c.completed)
			if c.pullRequest != "" {
				r.putPullRequest(c.pullRequest, c.ask)
			}

			runner := &outbox{pool: r.pool, records: r.pauses.records, trees: r.st, project: "legion", backstop: time.Hour,
				now: time.Now, log: quietLogger()}
			if err := runner.armBackstops(r.ctx, record.Issue{Key: pauseIssue, Tree: pauseIssue, Phase: c.at, Generation: 1}); err != nil {
				t.Fatalf("arm the backstops of %s's release: %v", pauseIssue, err)
			}

			armed := r.count(`select count(*) from pod_wake_backstops where issue = $1 and role = $2 and not fired`, pauseIssue, string(c.role))
			want := 0
			if c.backstop {
				want = 1
			}
			if armed != want {
				t.Errorf("the release of %s armed %d open backstops for its %s, want %d: %s", pauseIssue, armed, c.role, want, c.why)
			}
		})
	}
}

// mergeForwardAskOf is the daemon's ask of role to merge the base forward onto head c0de, made in
// the phase in and not landed, as the record keeps it (record.MergeForward).
func mergeForwardAskOf(role claim.Role, in phase.Phase) *record.MergeForward {
	return &record.MergeForward{Head: "c0de", Base: "main", Role: role, Phase: in, Why: record.ConflictRulesUnread,
		AskedAt: time.Now().UTC()}
}

// putPullRequest records LEGION-208's pull request in state at head c0de, with ask as the merge
// forward the record holds, nil for none.
func (r *pauseRig) putPullRequest(state record.PullRequestState, ask *record.MergeForward) {
	r.t.Helper()
	pr := record.PullRequest{State: state, Issue: pauseIssue, Repo: "acme/widgets", Number: 42, Branch: "legion/" + pauseIssue,
		HeadSHA: "c0de", Failing: []string{}, MergeForward: ask}
	if err := store.Transact(r.ctx, r.pool, func(ctx context.Context, tx pgx.Tx) error {
		return r.pauses.records.PutPullRequest(r.ctx, tx, pr)
	}); err != nil {
		r.t.Fatalf("record the pull request of %s: %v", pauseIssue, err)
	}
}

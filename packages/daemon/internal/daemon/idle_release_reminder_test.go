package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// idleRigOwesNothing is the one issue of each rig below: an issue in implementing whose
// implementer has taken its start task and reported, and which a case then gives a recorded
// completion or leaves owing its phase.
const idleRigOwesNothing = "owes-nothing"

// dispatch://INFRA-2746 R1, through the daemon a deployment runs (check
// c-idle-keeps-nothing-for-a-claim-that-owes-nothing, its daemon row): the release of a pod whose
// open phase's role has its completion recorded, while the issue stays in that phase, arms no
// backstop for that role, since that role owes nothing a wake could bring back. OSSV-732's
// reviewer is the shape: its completion stood while its issue stayed in reviewing, waiting on a
// code-owner review. The control, the same release with no completion recorded, arms the open
// phase's backstop as it always has.
func TestIdleOfAClaimThatOwesNothingArmsNoBackstopForItsRole(t *testing.T) {
	for _, c := range []struct {
		name      string
		completed bool
	}{
		{name: "its role's completion is recorded", completed: true},
		{name: "the control: its role owes its phase", completed: false},
	} {
		t.Run(c.name, func(t *testing.T) {
			rig := newIdleWakeRig(t, nil, idleRigOwesNothing)
			issue := rig.issue(idleRigOwesNothing)
			rig.awaitStartTask(issue)
			rig.awaitReported(rig.token(issue), "")
			if c.completed {
				rig.recordCompletion(issue)
			}

			rig.awaitReleased(issue)
			rig.awaitReleaseStepDone(issue)

			armed := rig.openBackstops(issue)
			if c.completed && armed != 0 {
				t.Errorf("the release of %s armed %d open backstops for its %s, want none: its completion stands, so it owes nothing a wake could bring back",
					issue.key, armed, issue.role)
			}
			if !c.completed && armed != 1 {
				t.Errorf("the release of %s armed %d open backstops for its %s, want 1: it owes its phase", issue.key, armed, issue.role)
			}
			rig.kube.requireServed(t)
		})
	}
}

// dispatch://INFRA-2746 R1's last sentence, through the daemon a deployment runs: a claim that
// owes nothing holds no reminder wake, so no reminder holds its pod. The idle release reads what
// the record holds of each claim's role through the reader the daemon hands the store when it
// attaches the release (attachIdleRelease, store.ReadIdleRecordsWith, record.FillIdleClaims), and
// the reminder hold reads record.PhaseOwed from it (workflow.IdleRelease). The workflow and store
// tests each bring their own half; this case fails if the daemon stops handing the store that
// reader.
//
// The claim's last report names a reminder wake a minute past due: the report of a timer that
// never got to report that it cleared it. The wake is written in the same statement that makes
// the claim quiet (quietWithPastDueReminder), each time the case makes it quiet, so a report the
// role's process sends meanwhile, a relaunched process's included, cannot leave the pod quiet
// with no wake due. With its role's completion recorded, the pod is released once quiet past the
// idle bound and no pass holds it for the reminder. The control, the same past-due wake with no
// completion recorded, is held as reminder_due pass after pass.
//
// A fresh claim owes its phase too (spec v72): a live implementer whose claims row records it
// serving run 0, as one that has retired no run, and whose pending delivery is its first task of
// the issue's run and phase, prompted and neither delivered nor confirmed, as a send that failed
// before any turn leaves it, serves that task's run and entry by the owed answer's rule
// (supervise.Claim.ServingAt). The store's idle view fills each claim's served run and entry by
// that rule (store's readIdleView, pods.IdleClaim.ReadServing), so with its reported reminder
// wake past due the claim is held reminder_due. It reads the view through the store's own
// release transaction (store.Store.ReleaseIdleIssue), with the record's reader the daemon hands
// it (record.FillIdleClaims), and decides it as the idle tick does (workflow.IdleRelease); it
// fails if the view reads the run from the claims row (serving_generation) instead.
//
// The seventh control (cybermonklabs/legion#4979) is read the same way: LEGION-208 stands in
// merging with its pull request open at a head of which the daemon's merge forward has not
// landed, and its live merger, level with its row's entry, reported a reminder wake a minute
// past due. The merger the daemon asked to merge forward owes the merge, its own open work, so
// its pod is held reminder_due. The merger the move into merging held rather than asked, since
// the implementer's ask made in reviewing had not landed (workflow's holdStartedWorker), owes nothing
// while that ask holds it (record.MergeForwardHolds, read from the pull request in the store by
// record.FillIdleClaims), so no reminder holds its pod, and quiet past the idle bound it is
// released.
func TestIdleOfAClaimThatOwesNothingHoldsNoPodForAPastDueReminder(t *testing.T) {
	t.Run("its role's completion is recorded", func(t *testing.T) {
		rig := newIdleWakeRig(t, nil, idleRigOwesNothing)
		issue := rig.issue(idleRigOwesNothing)
		rig.awaitStartTask(issue)
		rig.awaitReported(rig.token(issue), "")
		rig.recordCompletion(issue)
		token := rig.token(issue)

		var quieted time.Time
		idleRigEventually(t, "the pod of "+issue.key+" released", func() bool {
			if time.Since(quieted) > 2*time.Second {
				quieted = time.Now()
				rig.quietWithPastDueReminder(issue)
			}
			state := rig.api.claim(token)
			return state.State == string(supervise.StateSuspended) && state.SuspendedBy == string(supervise.SuspendedByReleased) &&
				!rig.kube.pod(issue.pod) && rig.kube.sandbox(issue.pod) == nil
		})

		records := rig.awaitReleaseRecords(issue)
		idleRigReleaseOf(t, records, issue)
		for _, hold := range records.Holds {
			if hold.Issue == issue.key && hold.Reason == workflow.IdleHoldReminder.String() {
				t.Errorf("the pod of %s was held for a reminder (%+v), want no such hold: its role's completion stands, so its claim owes nothing", issue.key, hold)
			}
		}
		rig.kube.requireServed(t)
	})
	t.Run("the control: its role owes its phase", func(t *testing.T) {
		rig := newIdleWakeRig(t, nil, idleRigOwesNothing)
		issue := rig.issue(idleRigOwesNothing)
		rig.awaitStartTask(issue)
		rig.awaitReported(rig.token(issue), "")

		ctx := context.Background()
		st, err := store.Open(ctx, rig.cfg.PostgresDSN)
		if err != nil {
			t.Fatalf("open the store to read the release records: %v", err)
		}
		defer st.Close()
		var quieted time.Time
		idleRigEventually(t, "the pod of "+issue.key+" held for a due reminder over passes of the idle tick", func() bool {
			if time.Since(quieted) > 2*time.Second {
				quieted = time.Now()
				rig.quietWithPastDueReminder(issue)
			}
			read, err := st.PodReleaseRecords(ctx, rig.project, time.Time{})
			if err != nil {
				t.Fatalf("read the release records: %v", err)
			}
			for _, release := range read.Releases {
				if release.Issue == issue.key {
					t.Fatalf("a release of %s was recorded (%+v) while its claim owes its phase and its reminder is due", issue.key, release)
				}
			}
			for _, hold := range read.Holds {
				if hold.Issue == issue.key && hold.Reason == workflow.IdleHoldReminder.String() && hold.Passes >= idleRigHeldPasses {
					return true
				}
			}
			return false
		})
		if !rig.kube.pod(issue.pod) || rig.kube.sandbox(issue.pod) == nil {
			t.Fatalf("the pod of %s is gone while its claim owes its phase and its reminder is due", issue.key)
		}
		rig.kube.requireServed(t)
	})
	t.Run("a fresh claim whose first task of the run is not yet delivered owes its phase", func(t *testing.T) {
		r := newPauseRig(t)
		fresh := r.claim(claim.RoleImplementer, supervise.StateIdle)
		fresh.ServingGeneration, fresh.ServedEntry, fresh.GivenEntry = 0, 0, 1
		fresh.Pending = undeliveredTask(phase.Implementing, 1)
		r.putClaim(fresh)
		r.putPhaseRow(claim.RoleImplementer, 1, false)
		got, served := r.idleDecisionWithPastDueReminder(fresh)
		if got.Hold != workflow.IdleHoldReminder {
			t.Errorf("the pod of %s decided %+v (the implementer serves run %d, entry %d), want held %s: the fresh claim serves its first task's run and entry and owes its phase",
				pauseIssue, got, served.ServingGeneration, served.ServingEntry, workflow.IdleHoldReminder)
		}
	})
	for _, c := range []struct {
		name string
		// ask is the daemon's ask to merge the base forward onto the head the pull request
		// stands at, not landed, and held whether it holds the merger.
		ask  *record.MergeForward
		held bool
	}{
		{name: "the seventh control: the merger the daemon asked to merge forward in merging owes its phase",
			ask: mergeForwardAskOf(claim.RoleMerger, phase.Merging)},
		{name: "a merger held in merging by the implementer's ask made in reviewing, which has not landed, owes nothing",
			ask: mergeForwardAskOf(claim.RoleImplementer, phase.Reviewing), held: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newPauseRig(t)
			if _, err := r.pool.Exec(r.ctx, `update issues set phase = 'merging' where key = $1`, pauseIssue); err != nil {
				t.Fatalf("move %s into merging: %v", pauseIssue, err)
			}
			merger := r.claim(claim.RoleMerger, supervise.StateIdle)
			merger.ServedEntry, merger.GivenEntry = 1, 1
			r.putClaim(merger)
			r.putPhaseRow(claim.RoleMerger, 1, false)
			r.putPullRequest(record.PullRequestOpen, c.ask)
			got, read := r.idleDecisionWithPastDueReminder(merger)
			if read.MergeForwardHeld != c.held {
				t.Errorf("the idle view read the merger held by the merge forward = %t, want %t", read.MergeForwardHeld, c.held)
			}
			if c.held {
				if !got.Release || got.Hold != workflow.IdleHoldNone {
					t.Errorf("the pod of %s decided %+v, want it released: the ask holds the merger, so it owes nothing and its past-due reminder holds no pod",
						pauseIssue, got)
				}
				return
			}
			if got.Hold != workflow.IdleHoldReminder {
				t.Errorf("the pod of %s decided %+v, want held %s: the merger owes the merge it was asked to make",
					pauseIssue, got, workflow.IdleHoldReminder)
			}
		})
	}
}

// idleDecisionWithPastDueReminder has c, a live claim of LEGION-208 the rig wrote, report on
// the daemon's contract twenty minutes ago with a reminder wake a minute past due, its pod having
// recorded nothing for half an hour, and decides the pod as the idle tick does
// (workflow.IdleRelease, with a ten-minute idle bound) on the view the store's own release
// transaction reads (store.Store.ReleaseIdleIssue), through the record's reader the daemon hands
// the store (record.FillIdleClaims). It answers the decision and what that view read of c.
func (r *pauseRig) idleDecisionWithPastDueReminder(c supervise.Claim) (workflow.IdleDecision, pods.IdleClaim) {
	r.t.Helper()
	const contract = 47
	r.st.ReadIdleRecordsWith(func(ctx context.Context, tx pgx.Tx, issue string, at phase.Phase, claims []pods.IdleClaim) error {
		return record.FillIdleClaims(ctx, r.pauses.records, tx, issue, at, claims)
	})
	if _, err := r.pool.Exec(r.ctx, `update claims set plugin_contract = $2, activity_at = now() - interval '20 minutes',
		activity_next_reminder_at = now() - interval '1 minute', updated_at = now() - interval '30 minutes'
		where token = $1`, string(c.Token), contract); err != nil {
		r.t.Fatalf("report the %s's reminder wake past due: %v", c.Role, err)
	}
	var got workflow.IdleDecision
	var read pods.IdleClaim
	if _, err := r.st.ReleaseIdleIssue(r.ctx, "legion", pauseIssue, func(view pods.IdleView) (store.IdleReleaseMark, error) {
		got = workflow.IdleRelease(view, time.Now(), 10*time.Minute, true, false, contract)
		for _, viewed := range view.Claims {
			if viewed.Role == c.Role {
				read = viewed
			}
		}
		return store.IdleReleaseMark{}, nil
	}); err != nil {
		r.t.Fatalf("read the idle view of %s: %v", pauseIssue, err)
	}
	return got, read
}

// The other half of dispatch://INFRA-2746 R1 at the release of a pod (check
// c-idle-keeps-nothing-for-a-claim-that-owes-nothing): a released claim that owes nothing and
// holds no task that no turn has confirmed gets no backstop, and its later work still brings
// its pod back, as a daemon task once it is sent. The implementer's completion is recorded and
// its pod is released with no backstop; the pod then stays released, pass after pass, with
// nothing to wake it for. Then the issue moves back into implementing, as a review that asks for
// changes leaves the implementer's row: its completion gone and its entry raised above the one
// the claim was given, with no task of that entry queued. The admission pass's owed-entry repair
// (workflow.OwedEntry) sends that entry's task, and the task brings the pod back: the role
// registers again and is given a task.
func TestIdleOfAClaimThatOwesNothingGetsItsPodBackForItsNextWork(t *testing.T) {
	rig := newIdleWakeRig(t, nil, idleRigOwesNothing)
	issue := rig.issue(idleRigOwesNothing)
	token := rig.token(issue)
	rig.awaitStartTask(issue)
	rig.awaitReported(token, "")
	rig.recordCompletion(issue)
	registered := rig.agents.role(issue.key, string(issue.role)).Registered

	rig.awaitReleased(issue)
	rig.awaitReleaseStepDone(issue)
	if armed := rig.openBackstops(issue); armed != 0 {
		t.Fatalf("the release of %s armed %d open backstops for its %s, want none: its completion stands", issue.key, armed, issue.role)
	}
	rig.markPrompts()

	// No false wake: forty passes of the rig's idle tick, and the claim stays released.
	deadline := time.Now().Add(40 * idleRigTick)
	for time.Now().Before(deadline) {
		if state := rig.api.claim(token); state.State != string(supervise.StateSuspended) {
			t.Fatalf("%s's %s is %s (suspended by %q) while it owes nothing, want it left released", issue.key, issue.role, state.State, state.SuspendedBy)
		}
		time.Sleep(idleRigTick)
	}
	if prompts := rig.newPrompts(issue); len(prompts) != 0 {
		t.Fatalf("%s's released %s was given %d tasks while it owed nothing: %v", issue.key, issue.role, len(prompts), idleRigTexts(prompts))
	}

	rig.moveBack(issue)

	rig.awaitResumed(issue, registered+1)
	idleRigEventually(t, "a task of "+issue.key+"'s phase given to its woken "+string(issue.role), func() bool {
		return len(rig.newPrompts(issue)) > 0
	})
	rig.kube.requireServed(t)
}

// moveBack leaves the issue's role's phases row as a move back into its phase leaves it: no
// completion, and an entry one above the one its claim was given, raised now. It queues no task:
// the daemon's own repair of an owed entry is what sends it.
func (r *idleWakeRig) moveBack(issue idleRigIssue) {
	r.t.Helper()
	ctx := context.Background()
	if err := store.Transact(ctx, r.pool, func(ctx context.Context, tx pgx.Tx) error {
		var given int64
		if err := tx.QueryRow(ctx, `select given_entry from claims where token = $1`, string(r.token(issue))).Scan(&given); err != nil {
			return err
		}
		rows, err := r.records.Phases(ctx, tx, issue.key)
		if err != nil {
			return err
		}
		row := record.PhaseRowOf(rows, issue.role)
		row.Issue, row.Role, row.Claim = issue.key, issue.role, r.token(issue)
		row.HandoffRecord, row.HandoffVersion, row.HandoffCommit = "", 0, ""
		row.Entry, row.EntryRaisedAt, row.EntryMovedBack = given+1, time.Now().UTC(), true
		return r.records.PutPhase(ctx, tx, row)
	}); err != nil {
		r.t.Fatalf("move %s's %s back into its phase: %v", issue.key, issue.role, err)
	}
}

// recordCompletion writes the completion of the issue's role on its phases row, the handoff
// record a completion names, as the workflow records it, and leaves the issue in its phase.
func (r *idleWakeRig) recordCompletion(issue idleRigIssue) {
	r.t.Helper()
	ctx := context.Background()
	if err := store.Transact(ctx, r.pool, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := r.records.Phases(ctx, tx, issue.key)
		if err != nil {
			return err
		}
		row := record.PhaseRowOf(rows, issue.role)
		row.Issue, row.Role, row.Claim = issue.key, issue.role, r.token(issue)
		row.HandoffRecord, row.HandoffVersion = "tester-completion", 1
		return r.records.PutPhase(ctx, tx, row)
	}); err != nil {
		r.t.Fatalf("record the completion of %s's %s: %v", issue.key, issue.role, err)
	}
}

// quietWithPastDueReminder back-dates the issue's claim as quiet does and, in the same statement,
// sets its last report's reminder wake a minute in the past, which is what a session that armed a
// reminder and reported nothing since leaves the daemon holding. One statement, so the idle tick
// never reads the claim quiet with a report that a newer one of the role's process replaced: such
// a report makes the claim busy again until the next call writes both. A claim whose current
// launch has not reported yet is left as it is.
func (r *idleWakeRig) quietWithPastDueReminder(issue idleRigIssue) {
	r.t.Helper()
	if _, err := r.pool.Exec(context.Background(),
		`update claims set activity_next_reminder_at = now() - interval '1 minute', updated_at = now() - interval '30 minutes'
		 where token = $1 and activity_at is not null`, string(r.token(issue))); err != nil {
		r.t.Fatalf("make %s quiet with a reminder wake past due: %v", issue.key, err)
	}
}

// awaitReleaseStepDone waits until the outbox step that released the issue's pod has finished:
// it arms the open phase's backstop (armBackstops) after the pod is gone and before it forgets
// the release's mark (ClearIssueRelease), so once the mark is gone every backstop that release
// arms is in the table.
func (r *idleWakeRig) awaitReleaseStepDone(issue idleRigIssue) {
	r.t.Helper()
	idleRigEventually(r.t, "the release step of "+issue.key+" done", func() bool {
		var marks int
		if err := r.pool.QueryRow(context.Background(),
			`select count(*) from issue_releases where project = $1 and issue = $2`, r.project, issue.key).Scan(&marks); err != nil {
			r.t.Fatalf("read the release mark of %s: %v", issue.key, err)
		}
		return marks == 0
	})
}

// openBackstops is how many backstops of the issue's role stand armed and not fired.
func (r *idleWakeRig) openBackstops(issue idleRigIssue) int {
	r.t.Helper()
	var armed int
	if err := r.pool.QueryRow(context.Background(),
		`select count(*) from pod_wake_backstops where project = $1 and issue = $2 and role = $3 and not fired`,
		r.project, issue.key, string(issue.role)).Scan(&armed); err != nil {
		r.t.Fatalf("read the backstops of %s: %v", issue.key, err)
	}
	return armed
}

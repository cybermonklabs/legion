package daemon

import (
	"context"
	"fmt"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/wait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

const (
	// wakeTickInterval is how often the daemon fires the backstops that came due and warns about
	// the resumes that are stuck. Both are measured in minutes, so a minute is often enough to
	// act within a tick of a due time and rare enough to cost two small queries a minute.
	wakeTickInterval = time.Minute
	// listenerReadyWindow is how fresh the Envoy listener's own `listener_ready` report must be
	// for the daemon to release anything. The listener publishes one at its start and every five
	// minutes after it, so fifteen minutes is three reports' worth of grace and still short
	// enough that a listener that stopped reporting holds release off within one idle period.
	//
	// The gate is R7's and it is one-sided on purpose: a released pod comes back for a role
	// message or an agent message only because the listener reports them, and without a positive
	// signal "the listener sends no reports" and "the listener has nothing to report" are the
	// same silence. A pod released into that silence could sit released with a message waiting
	// for it, so the daemon releases nothing until it has heard the listener.
	listenerReadyWindow = 15 * time.Minute
	// parkedWakeBound is the longest a delivery row waits for a launch the wake pacing parked
	// before it reads the claim again (parkedWait). The turn the pacing gave that launch lives in
	// the machine's memory alone, and the park can end before it: an operator's resume, retry,
	// spawn or task drops the wake and launches at once (supervise's dropWakeFor), a daemon
	// restart loses the wake, and a suspension takes it. The row hears none of these, so a park
	// that ends early costs it up to this long before it publishes, and for as long its queued
	// row holds the pod from idle release (pods.IdleView): five minutes is half the default idle
	// period. Against that, a park that lasts the hour a wave can take costs the row about twelve
	// attempts, each logged at debug, where its backoff's 64 s would make about 57 of them.
	parkedWakeBound = 5 * time.Minute
	// A wake that has not reached the woken role's first turn within workflow.StrandedReleaseWake
	// is the same fact two passes read: this one warns about it and marks it (R5), and the
	// stranded report names its claim (workflow.Stranded), so the bound is that rule's and is
	// not restated here.
)

// clockNow is the daemon's one clock as this runtime reads it (workflowRuntime.now), and the
// wall clock in a runtime a test builds by hand with none.
func (w *workflowRuntime) clockNow() time.Time {
	if w.now == nil {
		return time.Now()
	}
	return w.now()
}

// listenerLive is whether the Envoy listener has said it is alive and reporting within
// listenerReadyWindow (store.Store.ListenerLive): the one gate the idle rule reads before it
// releases anything. A daemon that has read no report at all, and one whose newest report is
// older than the window, releases nothing and says which of the two it is.
func (w *workflowRuntime) listenerLive(ctx context.Context) bool {
	if w.idleStore == nil {
		return false
	}
	now := w.clockNow()
	live, at, found, err := w.idleStore.ListenerLive(ctx, w.projectID, now, listenerReadyWindow)
	switch {
	case err != nil:
		w.log.Warn("idle release: could not read the listener's liveness report; releasing nothing", "error", err)
	case !found:
		w.log.Info("idle release: the Envoy listener has not once reported itself ready to this daemon; releasing nothing")
	case !live:
		w.log.Warn("idle release: the Envoy listener's newest report is too old to release a pod on; a released pod would not be woken by a role or agent message",
			"reported", at.Format(time.RFC3339), "age", now.Sub(at).Truncate(time.Second).String(), "window", listenerReadyWindow.String())
	}
	return live
}

// tickWakes fires the backstops that came due and warns about the resumes that are stuck, on the
// daemon's own period. It runs only where release runs: with no idle period and no store there is
// no released pod to wake and no resume to measure.
func (w *workflowRuntime) tickWakes(ctx context.Context) {
	if w.idlePeriod <= 0 || w.idleStore == nil {
		return
	}
	interval := w.idleTick
	if interval <= 0 {
		interval = wakeTickInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := w.clockNow().UTC()
			w.fireDueBackstops(ctx, now)
			w.warnStuckResumes(ctx, now)
		}
	}
}

// fireDueBackstops applies one PodBackstopDue fact per backstop of the project due at or before
// now. The row is the durable half and the fact the acting half: a backstop that came due while
// the daemon was down is read by the first pass after it starts, and the row's conditional fire
// (store.PodRecords.FirePodBackstop) is what makes two passes that read the same row wake its
// pod once.
func (w *workflowRuntime) fireDueBackstops(ctx context.Context, now time.Time) {
	due, err := w.idleStore.DuePodBackstops(ctx, w.projectID, now)
	if err != nil {
		w.log.Warn("wake: could not read the backstops that came due", "error", err)
		return
	}
	for _, backstop := range due {
		if ctx.Err() != nil {
			return
		}
		fact := intake.PodBackstopDue{ID: backstop.ID, Issue: backstop.Issue, Role: backstop.Role}
		id := fmt.Sprintf("pod-backstop:%d", backstop.ID)
		if _, err := intake.ApplyFact(ctx, w.pool, "wake", id, fact, w.handlers...); err != nil {
			w.log.Warn("wake: could not fire a released pod's backstop", "issue", backstop.Issue, "role", backstop.Role, "error", err)
			continue
		}
		w.wakeOutbox(backstop.Issue, record.OutboxKindSupervise)
	}
}

// warnStuckResumes warns about each resume whose stuck clock ran out of
// workflow.StrandedReleaseWake with the woken role still having taken no turn, and marks it, so
// each is warned about once. The warning names the issue, the role and the step the resume is
// waiting on, which is what an operator needs to tell "the pod never came back" from "the pod
// came back and the agent never took its task".
//
// The clock is the resume's stuck clock (store.PodResume.ClockAt), which starts at the wake and
// moves to the moment the wake pacing let the launch out, or a project's resume ended the pause
// that held it; the read passes over a paused project's resumes (decision 14 of
// dispatch://INFRA-3759).
func (w *workflowRuntime) warnStuckResumes(ctx context.Context, now time.Time) {
	stuck, err := w.idleStore.StuckPodResumes(ctx, w.projectID, now.Add(-workflow.StrandedReleaseWake))
	if err != nil {
		w.log.Warn("wake: could not read the resumes with no first turn", "error", err)
		return
	}
	for _, resume := range stuck {
		if ctx.Err() != nil {
			return
		}
		w.log.Warn("wake: a released pod's resume has had no first turn since its wake",
			"issue", resume.Issue, "role", resume.Role, "kind", resume.Kind,
			"waiting", w.stuckResumeStep(ctx, resume), "wake", resume.At.UTC().Format(time.RFC3339),
			"clock", resume.ClockAt.UTC().Format(time.RFC3339), "after", now.Sub(resume.ClockAt).Truncate(time.Second).String())
		if err := w.idleStore.MarkPodResumeStuck(ctx, resume.ID); err != nil {
			w.log.Warn("wake: could not mark a resume stuck", "issue", resume.Issue, "role", resume.Role, "error", err)
		}
	}
}

// stuckResumeStep is the step a stuck resume is waiting on, which is what its warning names: the
// replacement pod, when nothing has observed it ready, or the woken role's first turn, when the
// pod is up and the agent has taken nothing. The pod-ready stamp is read here rather than
// carried on the resume, because the read that lists the stuck resumes does not carry it.
func (w *workflowRuntime) stuckResumeStep(ctx context.Context, resume store.PodResume) string {
	ready, err := w.idleStore.PodResumeReadyAt(ctx, resume.ID)
	if err != nil {
		w.log.Warn("wake: could not read a stuck resume's pod-ready stamp", "issue", resume.Issue, "error", err)
	}
	if ready.IsZero() {
		return "the replacement pod to be ready"
	}
	return "the woken role's first turn"
}

// podResume stamps what a supervisor's durable transition says about a woken role's open resume:
// a ready claim is the replacement pod up, the first observation the daemon has of it, and a
// working claim is the woken role's first turn, which ends the resume R5 measures. A wake for a
// notice or a hook prompt gives the role no delivery to confirm, so the turn itself — not a
// confirmation — is what says the pod came back and took its work.
//
// A ready claim also ends a pause's hold of its pod (decision 13 of dispatch://INFRA-3759): a role
// of the pod runs again, so the pod is in the state a released pod's wake leaves it in
// (endPodHoldAtReady).
//
// It is a supervisor terminal callback, so it runs on the machine's own goroutine and must not
// block it; a failed stamp is logged and the resume keeps its zero time.
func (w *workflowRuntime) podResume(c supervise.Claim, state supervise.ClaimState) {
	if w.idleStore == nil || c.Issue == "" || c.Role == claim.RoleController {
		return
	}
	mark, what := w.idleStore.MarkPodResumeReady, "ready"
	switch state {
	case supervise.StateReady:
	case supervise.StateWorking:
		mark, what = w.idleStore.MarkPodResumeFirstTurn, "first turn"
	default:
		return
	}
	go func() {
		// This goroutine runs beside the machine that started it and every other, so it takes a
		// hold mark of its own (store.ForConcurrentWorker).
		ctx, cancel := context.WithTimeout(store.ForConcurrentWorker(context.Background()), readyReleaseTimeout)
		defer cancel()
		w.stampPodResume(ctx, c.Issue, c.Role, mark, what)
		if state == supervise.StateReady {
			w.endPodHoldAtReady(ctx, c)
		}
	}()
}

// endPodHoldAtReady ends the pause's hold of c's pod at c's ready (store.EndLivePausePodHold,
// PausePodHoldReady): the first ready of a role of the pod ends it, and every later ready finds
// none. It is ended under the issue's lock where a role of the pod is live, so a stop transaction
// of the same pod that commits first, and opens a hold because no role runs any more, keeps its
// hold. A failed end is logged, and the next ready of a role of the pod ends it.
func (w *workflowRuntime) endPodHoldAtReady(ctx context.Context, c supervise.Claim) {
	ended, err := w.idleStore.EndLivePausePodHold(ctx, w.projectID, c.Issue, store.PausePodHoldReady, w.clockNow().UTC())
	switch {
	case err != nil:
		w.log.Warn("pause: could not end a pod's hold at a role's ready; the next ready of a role of the pod ends it",
			"claim", c.Token, "issue", c.Issue, "error", err)
	case ended:
		w.log.Info("pause: a pod's hold ended; a role of the pod is ready", "claim", c.Token, "issue", c.Issue)
	}
}

// stampPodResume writes one observation onto the newest resume of issue's role that has not
// reached its first turn, when there is one. A claim that was never released has no open resume
// and nothing is written, which is the ordinary case: every ready and every turn of every claim
// passes here.
func (w *workflowRuntime) stampPodResume(ctx context.Context, issue string, role claim.Role,
	mark func(context.Context, int64, time.Time) error, what string) {
	id, _, found, err := w.idleStore.OpenPodResume(ctx, w.projectID, issue, role)
	if err != nil {
		w.log.Warn("wake: could not read the open resume of a role", "issue", issue, "role", role, "error", err)
		return
	}
	if !found {
		return
	}
	if err := mark(ctx, id, w.clockNow().UTC()); err != nil {
		w.log.Warn("wake: could not stamp a resume", "issue", issue, "role", role, "observation", what, "error", err)
	}
}

// wakeForDelivery brings a released pod back for a delivery the daemon already has for one of its
// roles and cannot hand over any other way: a notice whose owning architect is released, a hook
// prompt whose role is, the message that tells a released merger its pull request left the merge
// queue (mergeQueueRemoval), and a message of a merge forward whose role is released
// (mergeForwardMessage). Every other delivery is an outbox `start` or a task, which the supervise
// machine already resumes a suspended claim for (deliverResuming); these four publish to a live
// session instead, so the row's own retry is what waits for the pod, and this is what makes the
// pod come back for it to find.
//
// It answers whether the row must wait for a launch of the claim, and when that launch runs where
// the wake pacing holds it. The row waits for a released claim, which it resumes here, and for a
// claim whose launch the wake pacing has parked over no process, which is what that resume leaves
// while it waits for its turn. The pacing does not exempt the resume (supervise's exemptWake lets
// only an operator's requests through), so over a wave it can wait more than an hour, the machine
// holding the claim launching with no process and its view naming the waiting wake (supervise's
// parkWake, ClaimView.WakeWaiting). The row waits through that park because a hook prompt
// published then would be kept for the listener's one hour from that publish, which can end before
// the pod starts. So a waiting wake that starts a process
// (supervise.StartsProcess) over a claim that records no process holds the row until the launch
// runs, whichever launch it is. A relaunch parked over a process the claim still records holds
// nothing: the machine keeps that claim's state, and its live session can take the publish now.
//
// For such a park it answers the wake's turn as well (supervise.WakeWaiting.At), and each caller
// returns the wait parkedWait builds from it: the row runs again at the turn, or parkedWakeBound
// on when that comes first, and reads the claim again then. The turn lives only in the machine's
// memory, and the park can end before it — an operator's request, a restart or a suspension
// (parkedWakeBound says how) — so a row that slept to the turn could publish long after the pod
// came back and would hold that pod from idle release the whole time; the bound caps what such an
// early end costs. A row that climbed its backoff instead would try again every 64 s and log a
// Warn at every attempt from the seventh (settle), about once a minute for as long as the turn is
// away; this wait is routine (routineWaiting), so its attempts stay at debug. A turn already past
// when the row settles — the row ran a moment before the machine's own timer — is the backoff's
// (retry). A released claim this call resumes answers no turn, even where the resume parks: the
// row waits on its backoff, and its next attempt reads the park.
//
// The role's one open resume is what makes the wake one wake however often the row retries, so a
// notice that backs off for ten minutes resumes the claim once. A claim of an issue that has left
// the workflow, or whose tree lingers, is not woken: that pod is a close's to stop. An outbox
// with no supervisor — a test's, which supervises nothing — wakes nothing: there is no machine to
// read a claim's state from.
//
// mark is the pause that held the row the wake is for (record.OutboxRow.HeldByPause), 0 for a row
// no pause held: the resume writes it on every row its pause holds, whatever the row's kind or due
// time. The store reads which pause holds the wake with it (store.PodRecords.PauseOfWake, decision
// 15 of dispatch://INFRA-3759). So a row the pause held, which runs only after the resume, wakes
// its role with a held wake of that pause, and so does a row written after the resume for a role
// that pause stopped, as the workflow's wake of that role is.
//
// The claim is read from the machine's View, which never waits on the decision in flight: a
// machine holds its lock through the runtime call its decision makes, so a read that waited
// would hold the notice and the hook prompt behind a Sandbox relaunch waiting on the
// cluster, which is the defect LEGION-650 fixed in the routing beside this read.
func (r *outbox) wakeForDelivery(ctx context.Context, token claim.Token, detail string, mark int64) (waits bool, turn time.Time, err error) {
	if r.supervisor == nil {
		return false, time.Time{}, nil
	}
	machine, found := r.supervisor.Machine(token)
	if !found {
		return false, time.Time{}, nil
	}
	held := machine.View()
	if !held.Released() {
		if at, parked := parkedLaunch(held); parked {
			return true, at, nil
		}
		return false, time.Time{}, nil
	}
	issue, wakeable, err := r.wakeableIssue(ctx, held.Issue)
	if err != nil {
		return false, time.Time{}, err
	}
	if !wakeable {
		return false, time.Time{}, nil
	}
	taken, err := r.trees.TakePodWake(ctx, r.project, issue.Key, token, pods.WakeDelivery, r.now().UTC(), mark)
	if err != nil {
		return false, time.Time{}, err
	}
	// A wake the role's open resume refused is a pod already on its way back — unless no resume
	// of that claim ever ran. This take commits before the resume it leads to, and that resume is
	// in process rather than a row of the take's own transaction (the workflow's wakes queue a
	// `start`, this one calls the machine), so a resume refused by the store, or a daemon that
	// restarted between the two, would leave the claim released with the role's resume open —
	// and that open resume would then refuse every later wake of the role for as long as the
	// issue lives, since only a release closes one and a pod fully released is never released
	// again. So the claim is read again here, and one that is still not running is resumed
	// whatever took the wake: resuming is idempotent — the machine serializes on its own lock and
	// a claim already running is left alone (supervise's RequestRuleResume) — so the wake that
	// meets a stuck resume heals it rather than reading it as a pod coming back.
	reason := "a wake for " + detail
	if !taken {
		if !machine.View().Released() {
			return true, time.Time{}, nil
		}
		reason = "a wake for " + detail + ", whose resume the record already held open"
		r.logRefusedByOpenResume(ctx, issue.Key, held.Role, detail)
	} else {
		r.log.Info("wake: a released pod is coming back for a delivery the daemon holds for one of its roles",
			"issue", issue.Key, "role", held.Role, "detail", detail)
	}
	if err := machine.Handle(ctx, supervise.RequestRuleResume{Claim: token, Reason: reason}); err != nil {
		return false, time.Time{}, fmt.Errorf("wake claim %s for %s: %w", token, detail, err)
	}
	return true, time.Time{}, nil
}

// wakeableIssue is the record of issue key, and whether a released claim of it can be woken: the
// issue is in the workflow and its tree does not linger (pods.InWorkflowNotLingering). A claim of
// an issue that has left the workflow, or whose tree lingers, is a close's to stop, not a wake's
// to bring back. An issue the daemon keeps no record of is an error, as outbox.issue says.
func (r *outbox) wakeableIssue(ctx context.Context, key string) (record.Issue, bool, error) {
	issue, err := r.issue(ctx, key)
	if err != nil {
		return record.Issue{}, false, err
	}
	root, err := r.root(ctx, issue)
	if err != nil {
		return record.Issue{}, false, err
	}
	return issue, root != nil && pods.InWorkflowNotLingering(issue.Standing(), root.Standing(), record.OutOfWorkflow), nil
}

// parkedLaunch is whether view's launch is parked by the wake pacing over no process, and that
// launch's turn: the claim is launching, records no process, and its view names a waiting wake
// that starts one (supervise.StartsProcess). wakeForDelivery says why a delivery row waits through
// such a park and not through a relaunch parked over a process the claim still records.
func parkedLaunch(view supervise.ClaimView) (turn time.Time, parked bool) {
	if view.State == supervise.StateLaunching && view.Locator == nil &&
		view.WakeWaiting != nil && supervise.StartsProcess(view.WakeWaiting.Kind) {
		return view.WakeWaiting.At, true
	}
	return time.Time{}, false
}

// parkedWait is the wait of a delivery row whose role's launch the wake pacing has parked over no
// process (wakeForDelivery's turn), with text as its message: the row runs again at turn, or
// parkedWakeBound from now when that comes first, and reads the claim again then. A turn already
// past is the backoff's, as every moment a wait.Until names is once it has passed (retry). The
// wait is routine (routineWaiting): the launch's turn ends it, or an earlier end of the park the
// row's next read finds, so nobody has to act on it, and a park of an hour would otherwise be
// logged as a warning at every attempt from the seventh (waitWarnAttempts).
func (r *outbox) parkedWait(turn time.Time, text string) error {
	at := r.now().UTC().Add(parkedWakeBound)
	if turn.Before(at) {
		at = turn
	}
	return routineWaiting{err: wait.Until(at, text)}
}

// logRefusedByOpenResume says why a delivery wake whose take the role's open resume refused
// resumes a claim that is still released (wakeForDelivery), by whose that open resume is
// (store.OpenPodResume). One a pause held (its pause, the held wake of decision 15 of
// dispatch://INFRA-3759) is that pause's owed launch, whose start row waits in the outbox; the
// wake an earlier delivery row of the role that the pause held took after the resume; or the
// workflow's wake of a role the pause stopped, which store.PodRecords.PauseOfWake gives the pause
// (the listener's report of a role message the pause held, or of a message another session sent
// the role, a CI settlement or a backstop). The resume wakes every lane at once
// (projectPause.Resumed), so a held notice, hook prompt, merge-queue removal or merge forward's
// message of the same role can run before that start, or after that other wake, and find the role
// released under the held wake. Nothing is wrong there, and the line says so at Info. Any other
// open resume over a released claim is one whose process never came — a resume the store refused
// after its take committed, or a daemon that stopped between the two — and the line says so at
// Warn, as it does when the read fails, with its error: the resume goes ahead either way.
func (r *outbox) logRefusedByOpenResume(ctx context.Context, issue string, role claim.Role, detail string) {
	_, pause, open, err := r.trees.OpenPodResume(ctx, r.project, issue, role)
	if err == nil && open && pause != 0 {
		r.log.Info("wake: a released role's open resume is its pause's held wake; this wake resumes the role through the wake pacing",
			"issue", issue, "role", role, "detail", detail, "pause", pause)
		return
	}
	attrs := []any{"issue", issue, "role", role, "detail", detail}
	if err != nil {
		attrs = append(attrs, "error", err)
	}
	r.log.Warn("wake: a released pod's resume stands open with no process behind it; this wake resumes it", attrs...)
}

// armBackstops arms the backstop of a release that completed: backstop_minutes from now, for the
// worker of the pod's open phase (workflow.OpenPhaseRole). An architect gets none — nothing of
// its own is due, and a sub-architect woken for nothing would hold a pod open for the backstop
// period — and neither does an issue whose phase no role works.
//
// It also cancels the backstops the release before this one armed: that pod is gone, and its
// backstop has nothing left to bring back.
func (r *outbox) armBackstops(ctx context.Context, issue record.Issue) error {
	if err := r.trees.CancelPodBackstopsOf(ctx, r.project, issue.Key, ""); err != nil {
		return err
	}
	if r.backstop <= 0 {
		return nil
	}
	role := workflow.OpenPhaseRole(issue.Phase)
	if role == "" {
		return nil
	}
	released, err := r.trees.ReleasedClaimsIn(ctx, r.project, issue.Key)
	if err != nil {
		return err
	}
	for _, c := range released {
		if c.Role != role || c.ServingGeneration != issue.Generation {
			continue
		}
		due := r.now().UTC().Add(r.backstop)
		r.log.Info("wake: the release of an issue's pod arms its open phase's backstop",
			"issue", issue.Key, "role", role, "due", due.Format(time.RFC3339))
		return r.trees.ArmPodBackstop(ctx, store.PodWakeBackstop{Project: r.project, Issue: issue.Key,
			Role: role, DueAt: due})
	}
	return nil
}

// takeOverRelease is what every path that suspends, stops or closes a claim does to a pod that
// was released: it drops the open backstops the path has taken over, so no later backstop acts on
// a claim this path owns. A `suspend` row takes one role over and drops that role's backstop alone
// (decision 17 of dispatch://INFRA-3759): another role of the pod may be waiting on a deploy with
// nothing but its own backstop to bring it back. An issue's close and a tree's close name no role
// (dropReleasedWork), and drop every backstop of the issue, since the issue's work is over. The
// claim's own suspension source is re-stamped by the machine (supervise's suspendedAgain), which
// is what takes it out of every released-claim read; this drops what is left behind in the
// record.
//
// A runner with no tree store does nothing here. Every runner the daemon builds has one
// (newOutbox), so this is the test that builds a runner by hand for a path of its own, as the
// zero clock and the zero design gate are; such a runner has released no pod, so there is
// nothing of a release to take over.
func (r *outbox) takeOverRelease(ctx context.Context, issue string, role claim.Role) {
	if r.trees == nil {
		return
	}
	if err := r.trees.CancelPodBackstopsOf(ctx, r.project, issue, role); err != nil {
		r.log.Warn("wake: could not forget the wakes of a pod another path took over", "issue", issue, "role", role, "error", err)
	}
}

// dropReleasedWork takes a released pod's issue over and deletes both saved copies of its
// workspace, the bundle its release saved and the copy taken while its pod ran, in one
// transaction (store.DeleteWorkspaceBundle): the issue is done, or its tree is closing, so
// nothing will restore either and nothing should. A linger and a park keep theirs, because their
// issue comes back. The two go together so that no failure can leave one behind, since a running
// copy of a closed issue left in the table is the copy a later generation's pod would be sent. A
// runner with no tree store does nothing, as takeOverRelease says.
func (r *outbox) dropReleasedWork(ctx context.Context, issue string) {
	r.takeOverRelease(ctx, issue, "")
	if r.trees == nil {
		return
	}
	if err := r.trees.DeleteWorkspaceBundle(ctx, issue); err != nil {
		r.log.Warn("wake: could not delete the saved workspace copies of a finished issue", "issue", issue, "error", err)
	}
}

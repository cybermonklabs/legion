package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/sandbox"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// idleTickInterval is how often the daemon decides every pod of its project. The shortest idle
// period an operator would set is minutes, and one read per issue is three small queries, so a
// minute is often enough to release a pod within a tick of its period passing and rare enough to
// cost nothing.
const idleTickInterval = time.Minute

// issuePods is what the daemon asks a runtime that runs a pod per issue about an issue's pod,
// which is everything about it the daemon's own Postgres does not hold: whether the pod runs the
// image the configuration names (sandbox.Runtime.IssuePodRunsConfiguredImage), whether the runtime
// still holds the issue's Sandbox and under what name (sandbox.Runtime.IssueSandbox), which pod
// that Sandbox holds, or that the runtime cannot say (runtime.IssuePodReader), and whether the
// runtime keeps the issue's volume for its next pod (runtime.IssueVolumeReader). It is the one
// test of whether a runtime runs a pod per issue, so every rule that asks it gives one answer: the
// idle pass and its release (attachIdleRelease, outbox.issueSandboxName), whether an issue holds
// its workspace (workspaceHeldBy) and the rename pass's live-pod rules (bootKeep, podEndKeep).
// The Kubernetes runtime implements it; a runtime that runs no pods implements none of it, and the
// daemon then releases nothing, which is also what `runtime: tmux` wants.
type issuePods interface {
	IssuePodRunsConfiguredImage(issue string) bool
	IssueSandbox(issue string) (name string, present bool)
	runtime.IssuePodReader
	runtime.IssueVolumeReader
}

// Without it, a sandbox.Runtime missing a method fails the rt.(issuePods) checks with no error.
var _ issuePods = (*sandbox.Runtime)(nil)

// tickIdle decides every pod of the project on the daemon's own period (idleTickInterval),
// under both release rules: the release of a pod no claim owns, whose every claim is failed,
// retired or suspended (releaseUnownedPods), and the idle release, which takes a pod whose live
// roles have had no turn in progress and no turn due for the idle period (releaseIdlePods). The
// pod of a child that left the workflow while its tree runs on is closed by the same two passes,
// with no live claim by the first (closeLeftPods) and with one by the second
// (workflow.LeftPodClose). It returns at once — running no ticker at all — when release is off:
// `idle_minutes: 0`, or a runtime that is not Kubernetes, which has no pod to release and no image
// to compare. Both rules are off with it, since one period and one tick carry the two.
func (w *workflowRuntime) tickIdle(ctx context.Context) {
	if w.idlePeriod <= 0 || w.idleStore == nil || w.idleRuntime == nil {
		return
	}
	interval := w.idleTick
	if interval <= 0 {
		interval = idleTickInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// The release of a pod no claim owns runs first, and outside the gate the idle
			// release reads (workflowRuntime.listenerLive). That gate exists because an idle
			// release suspends live claims, and such a claim comes back for a role or an agent
			// message only while the Envoy listener reports those messages. This release
			// suspends no claim and leaves none waiting for a message, and the two starts that
			// build its pod again — an operator's retry and a start the daemon queues — read no
			// listener, so nothing of that reasoning reaches it.
			w.releaseUnownedPods(ctx)
			w.releaseIdlePods(ctx)
		}
	}
}

// releaseIdlePods decides each pod of the project in turn. One issue's failure is logged and the
// rest are decided: a release is a saving and never a correctness step, and the one start that
// waits on it, a start whose pod was built before its role, is served by the update round too.
func (w *workflowRuntime) releaseIdlePods(ctx context.Context) {
	if !w.listenerLive(ctx) {
		return
	}
	issues, err := w.idleStore.IdlePods(ctx, w.projectID)
	if err != nil {
		w.log.Warn("idle release: could not list the project's pods", "error", err)
		return
	}
	for _, issue := range issues {
		if ctx.Err() != nil {
			return
		}
		w.releaseIdlePod(ctx, issue)
	}
}

// releaseIdlePod decides one issue's pod and, when the rule releases it, writes the release's
// rows and its mark in one transaction with the view they rest on (store.ReleaseIdleIssue). The
// pod's image is read before that transaction, from the runtime's own store of pods, because it
// is the one input the daemon's Postgres does not hold.
//
// A pod the rule kept that was quiet for the whole idle period is recorded as a hold
// (recordIdleHold): that is a pod release would have taken and something else stopped, which is
// what tells a deployment whether release is working. A pod whose last release the workspace
// check declined is such a pod (workflow.IdleHoldUnpushedWork, IdleHoldWorkspaceUnread): the
// rule reads the decline from the view, so this pass marks no release, writes no row and asks no
// shim for it until a role of the issue starts a turn or a process again. The release itself is
// recorded where it completes (outbox.releaseIssue, through recordPodRelease).
// A pod whose issue the workflow has taken out may be closed here instead (workflow.LeftPodClose,
// closedLeftPod).
func (w *workflowRuntime) releaseIdlePod(ctx context.Context, issue string) {
	now := w.clockNow().UTC()
	onConfiguredImage := w.idleRuntime.IssuePodRunsConfiguredImage(issue)
	var decision workflow.IdleDecision
	var quiet, holding bool
	podUID := w.runningPod(issue)
	// The release's line and the wake of its lane are hooks of the mark's commit, so a mark whose
	// reply is lost and that the store says committed still says so and still wakes its lane, and
	// so are a left pod's close's (closedLeftPod). The kept pod's record stays after the
	// transaction: it is a write of its own.
	var marked bool
	err := store.Transact(ctx, w.idleStore.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		marked, err = w.idleStore.ReleaseIdleIssue(ctx, w.projectID, issue, func(view pods.IdleView) (store.IdleReleaseMark, error) {
			view.PodUID = podUID
			decision = workflow.IdleRelease(view, now, w.idlePeriod, onConfiguredImage, api.DaemonAPIVersion())
			since, found := view.QuietSince()
			quiet = found && now.Sub(since) >= w.idlePeriod
			holding = view.Holding
			switch {
			case decision.Close:
				return leftPodCloseRows(view, decision, now)
			case !decision.Release:
				return store.IdleReleaseMark{}, nil
			}
			return idleReleaseRows(view, decision, now)
		})
		if err != nil || !marked {
			return err
		}
		if decision.Close {
			return w.closedLeftPod(ctx, issue, decision, holding)
		}
		roles := len(decision.Roles)
		store.OnCommit(ctx, func() {
			w.log.Info("idle release: releasing an issue's pod, which has had no turn in progress and no turn due for the idle period",
				"issue", issue, "idle", w.idlePeriod.String(), "roles", roles)
			w.wakeOutbox(issue, record.OutboxKindSupervise)
		})
		return nil
	})
	if err != nil {
		w.log.Warn("idle release: could not decide an issue's pod", "issue", issue, "error", err)
		return
	}
	if !marked {
		detail := strings.Join(decision.HoldKinds, ",")
		w.log.Debug("idle release: the pod is kept", "issue", issue, "hold", decision.Hold.String(), "detail", detail)
		w.recordKeptPod(ctx, issue, decision.Hold, detail, quiet, holding, now)
	}
}

// releaseUnownedPods decides each pod of the project that no claim of its issue can use: an
// issue whose every claim is failed, retired or suspended holds a pod no role can take a turn
// in, and the pod's reservation with it (2 CPU and 8 GiB at the defaults), and this is the pass
// that releases it. One issue's failure is logged and the rest are decided, as the idle pass has
// it: a release is a saving and never a correctness step, and the one start that waits on it, a
// start whose pod was built before its role, is served by the update round too.
//
// It writes one line saying what the whole pass did — how many issues it decided, how many it
// released and how many it held under each reason — whenever it decided at least one. That line
// is what tells a reader of the daemon's log that this rule ran on a day it released nothing,
// which a release line alone cannot say, and it is what a production check of this rule reads.
// Nothing takes a released issue back out of the candidate answer, since this release changes
// no claim: an issue whose pod it took stands there until a start makes a claim of it live or
// the issue closes, and every later pass reads it again and holds it as unowned_sandbox_gone.
// So a deployment holding released issues pays one line a minute, reading released=0 and those
// holds, and that is what is wanted: the line is the one per-pass record that this rule ran on
// a day it released nothing, which is what its production check reads. Only a pass with no
// candidate at all writes nothing.
func (w *workflowRuntime) releaseUnownedPods(ctx context.Context) {
	// The pods of children that left the workflow under a running tree are pods no claim owns
	// too, and none of them is in the candidate answer below, which holds only issues still in
	// the workflow: closeLeftPods decides them, before this rule's own.
	w.closeLeftPods(ctx)
	// The project is spelled two ways here, and the two are not the same string. The issue
	// records the candidate answer reads carry the Dispatch project key (w.dispatchProject,
	// "LEGION"), while the claims it reads beside them, the release's transaction, the hold
	// records and pod_holds are all keyed on the claim token's spelling (w.projectID,
	// "legion"), which is what IdlePods and ReleaseIdleIssue read. Store.UnownedPods takes both
	// and says in its own doc comment which is which: the two arguments are not interchangeable
	// and either one in the other's place answers no issue at all.
	issues, err := w.idleStore.UnownedPods(ctx, w.dispatchProject, w.projectID, record.OutOfWorkflowStatuses)
	if err != nil {
		w.log.Warn("unowned release: could not list the project's pods no claim owns", "error", err)
		return
	}
	decided, released := 0, 0
	held := map[string]int{}
	for _, issue := range issues {
		if ctx.Err() != nil {
			return
		}
		decided++
		hold, marked := w.releaseUnownedPod(ctx, issue)
		switch {
		case marked:
			released++
		case hold != workflow.IdleHoldNone:
			// A pod whose decision could not be read at all answers IdleHoldNone and no mark,
			// and is counted under neither: its own warning has already named it.
			held[hold.String()]++
		}
	}
	if decided == 0 {
		return
	}
	holds := make([]string, 0, len(held))
	for reason, count := range held {
		holds = append(holds, fmt.Sprintf("%s=%d", reason, count))
	}
	slices.Sort(holds)
	w.log.Info("unowned release: decided every pod of the project no claim owns",
		"decided", decided, "released", released, "held", strings.Join(holds, ","))
}

// releaseUnownedPod decides one issue's pod under the rule for a pod no claim owns
// (workflow.UnownedPodRelease) and, when the rule releases it, writes the release's one row and
// its mark in one transaction with the view they rest on (store.ReleaseIdleIssue). It answers
// the hold that kept the pod and whether a release was marked, which is what the pass counts.
//
// The issue's Sandbox is read before that transaction, from the runtime's own store of pods,
// exactly as the idle pass reads the pod's image before its own, and a Sandbox the runtime no
// longer holds ends the pass here. That order is this rule's own economy, not a nicety: this
// release changes no claim, so an issue it has released keeps every claim stopped and stands in
// the candidate answer for ever. Deciding it inside a transaction would cost one
// store.ReleaseIdleIssue a minute for ever, and that transaction opens by taking the daemon's
// global fact serializer (treelifecycle.Serialize) — the same lock the intake takes for every
// fact, which is the very cost the candidate filter exists to avoid. On the INFRA deployment at
// 2026-11-10 21:23Z, 33 of the project's issues stood in that state.
//
// Such a pass still ends an open hold streak of the pod, and reads whether there is one first
// (store.OpenPodHold): a pod with no streak is left alone, since an update that matches no row
// costs a write transaction all the same.
//
// A pod the rule kept that was quiet for the whole idle period is recorded as a hold
// (recordIdleHold), as the idle pass records its own: that is a pod release would have taken and
// something else stopped. Five of this rule's eleven holds are such a hold, and recordableHold is
// what says which. The release itself is recorded where it completes (outbox.releaseIssue).
func (w *workflowRuntime) releaseUnownedPod(ctx context.Context, issue string) (workflow.IdleHold, bool) {
	now := w.clockNow().UTC()
	if _, present := w.idleRuntime.IssueSandbox(issue); !present {
		w.log.Debug("unowned release: the pod is kept", "issue", issue,
			"hold", workflow.IdleHoldUnownedSandboxGone.String())
		holding, err := w.idleStore.OpenPodHold(ctx, w.projectID, issue)
		if err != nil {
			w.log.Warn("unowned release: could not read whether a hold streak of the pod is running",
				"issue", issue, "error", err)
		}
		// There is no view here and so no quiet period to read, and none is needed:
		// IdleHoldUnownedSandboxGone is no hold a deployment reads, so this pass ends the pod's
		// streak whatever the pod was doing.
		w.recordKeptPod(ctx, issue, workflow.IdleHoldUnownedSandboxGone, "", false, holding, now)
		return workflow.IdleHoldUnownedSandboxGone, false
	}
	var decision workflow.IdleDecision
	var quiet, holding bool
	var waiting []claim.Role
	var heldFor string
	var suspended []string
	podUID := w.runningPod(issue)
	// The mark's line and the wake of its lane are hooks of its commit, as the idle pass's are. The
	// pass counts the release from what Transact answers, never from a hook, so a hook the watcher
	// runs later changes no count of a pass.
	var marked bool
	err := store.Transact(ctx, w.idleStore.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		marked, err = w.idleStore.ReleaseIdleIssue(ctx, w.projectID, issue, func(view pods.IdleView) (store.IdleReleaseMark, error) {
			// The Sandbox is there: the read above, before the transaction, is this conjunct's
			// answer, and the rule takes it rather than asking the runtime again. So is the pod a
			// decline on record is read against.
			view.PodUID = podUID
			decision = workflow.UnownedPodRelease(view, now, w.idlePeriod, true)
			if decision.TurnsOnKeptVolume() {
				// Whether the Sandbox keeps a volume the issue's next pod mounts, which the release
				// keeps and so needs no read of and holds for no failed one
				// (pods.IdleView.KeptVolume), is the one read of the runtime under the
				// transaction's lock, and is asked only of a pod the rule would otherwise release
				// with a read or hold for a read that failed, which is rare: the runtime's answer is
				// a read of its informer's Sandbox that decodes the claim templates
				// (internal/runtime/sandbox's IssueKeepsVolume), which no pass's tick pays.
				view.KeptVolume = w.idleRuntime.IssueKeepsVolume(issue, now)
				decision = workflow.UnownedPodRelease(view, now, w.idlePeriod, true)
			}
			since, found := view.QuietSince()
			quiet = found && now.Sub(since) >= w.idlePeriod
			holding = view.Holding
			waiting, heldFor, suspended = view.StartsWaitingForPod, view.HeldFor, operatorSuspended(view)
			if !decision.Release {
				return store.IdleReleaseMark{}, nil
			}
			return unownedReleaseRows(view, decision, now)
		})
		if err != nil || !marked {
			return err
		}
		store.OnCommit(ctx, func() {
			// The line that ends this release is the teardown's, and it is the release's one Info
			// line, whoever waits on it; the mark is a Debug line here for that reason.
			w.log.Debug("unowned release: releasing an issue's pod, which no claim of it can use",
				"issue", issue, "idle", w.idlePeriod.String())
			w.wakeOutbox(issue, record.OutboxKindSupervise)
		})
		return nil
	})
	if err != nil {
		w.log.Warn("unowned release: could not decide an issue's pod", "issue", issue, "error", err)
		return workflow.IdleHoldNone, false
	}
	if !marked {
		w.log.Debug("unowned release: the pod is kept", "issue", issue, "hold", decision.Hold.String())
		// An operator's suspension keeps a pod a role's start waits for, a wait nothing ends
		// until the operator acts: the controller is told once for each streak of the hold, at
		// the pass that opens it (HeldFor names no streak of it yet), before it is recorded.
		if decision.Hold == workflow.IdleHoldUnownedSuspensionHolds && quiet && len(waiting) > 0 &&
			heldFor != workflow.IdleHoldUnownedSuspensionHolds.String() {
			notice := record.ControllerNotice{Kind: record.PodWaitHeldNotice, Role: waiting[0],
				Reason: podWaitHeldWords(issue, waiting, suspended, w.idlePeriod)}
			if _, err := w.enqueueOutbox(ctx, issue, notice); err != nil {
				w.log.Warn("unowned release: could not tell the controller of a pod an operator's suspension holds", "issue", issue, "error", err)
			}
		}
		// This rule names one reason per pass and no kinds beside it, so its holds carry no
		// detail, as the teardown's do not.
		w.recordKeptPod(ctx, issue, decision.Hold, "", quiet, holding, now)
		return decision.Hold, false
	}
	return workflow.IdleHoldNone, true
}

// operatorSuspended is each claim of view an operator suspended, as `<role> (<state>)`.
func operatorSuspended(view pods.IdleView) []string {
	var held []string
	for _, c := range view.Claims {
		if c.SuspendedBy == supervise.SuspendedByOperator {
			held = append(held, fmt.Sprintf("%s (%s)", c.Role, c.State))
		}
	}
	return held
}

// podWaitHeldWords is what the controller is told of the roles waiting, whose start waits for
// the pod of issue that an operator's suspension of the claims suspended holds, for the controller
// agent in plain words.
func podWaitHeldWords(issue string, waiting []claim.Role, suspended []string, idle time.Duration) string {
	names := make([]string, len(waiting))
	for i, role := range waiting {
		names[i] = string(role)
	}
	roles := strings.Join(names, ", ")
	return fmt.Sprintf("%s of %s cannot start. The pod of %s was made before the deployment defined %s, and no role runs in it. "+
		"The daemon removes such a pod when it has had no activity for %d minutes, but not while an operator holds a claim of it. "+
		"The operator suspended %s. Resume or stop that claim. Then the daemon replaces or removes the pod, and %s start in a new pod.",
		roles, issue, issue, roles, int(idle.Minutes()), strings.Join(suspended, ", "), roles)
}

// closeLeftPods finishes the close of each pod whose issue left the workflow while its tree runs
// on and whose claims have all stopped (store.LeftPods, workflow.LeftPodClose). Such a pod stands
// where something built it again after the issue's own close — an operator's resume, task or
// retry, or the controller's fresh session — and its roles have stopped since, or where that
// close never took it. Nothing else takes it: the release of a pod no claim owns decides only
// issues still in the workflow (store.UnownedPods), and the idle rule only pods with a live claim
// (store.IdlePods), whose left pods it closes itself (releaseIdlePod).
//
// Each candidate is read against the runtime before anything else, as releaseUnownedPod reads
// it, and one whose Sandbox the runtime does not hold is passed over with no line and no write:
// that is what the issue's close left of nearly every such issue, so it is no hold of anything,
// and the read is the informer's store rather than an API call (sandbox.Runtime.IssueSandbox).
func (w *workflowRuntime) closeLeftPods(ctx context.Context) {
	issues, err := w.idleStore.LeftPods(ctx, w.dispatchProject, w.projectID, record.OutOfWorkflowStatuses)
	if err != nil {
		w.log.Warn("left pod close: could not list the pods of issues that left the workflow", "error", err)
		return
	}
	for _, issue := range issues {
		if ctx.Err() != nil {
			return
		}
		if _, present := w.idleRuntime.IssueSandbox(issue); !present {
			continue
		}
		w.closeLeftPod(ctx, issue)
	}
}

// closeLeftPod decides one such pod (workflow.LeftPodClose) and, when the rule closes it, writes
// the close's rows in one transaction with the view they rest on (store.ReleaseIdleIssue), and
// what follows the close in that transaction too (closedLeftPod), as the release passes write
// theirs. A pod the rule kept that was quiet for the whole idle period is recorded as a hold, as
// the two release passes record theirs (recordKeptPod).
func (w *workflowRuntime) closeLeftPod(ctx context.Context, issue string) {
	now := w.clockNow().UTC()
	var decision workflow.IdleDecision
	var quiet, holding bool
	podUID := w.runningPod(issue)
	var closed bool
	err := store.Transact(ctx, w.idleStore.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		closed, err = w.idleStore.ReleaseIdleIssue(ctx, w.projectID, issue, func(view pods.IdleView) (store.IdleReleaseMark, error) {
			view.PodUID = podUID
			decision = workflow.LeftPodClose(view, now, w.idlePeriod)
			since, found := view.QuietSince()
			quiet = found && now.Sub(since) >= w.idlePeriod
			holding = view.Holding
			if !decision.Close {
				return store.IdleReleaseMark{}, nil
			}
			return leftPodCloseRows(view, decision, now)
		})
		if err != nil || !closed {
			return err
		}
		return w.closedLeftPod(ctx, issue, decision, holding)
	})
	if err != nil {
		w.log.Warn("left pod close: could not decide an issue's pod", "issue", issue, "error", err)
		return
	}
	if !closed {
		w.log.Debug("left pod close: the pod is kept", "issue", issue, "hold", decision.Hold.String(),
			"detail", strings.Join(decision.HoldKinds, ","))
		w.recordKeptPod(ctx, issue, decision.Hold, strings.Join(decision.HoldKinds, ","), quiet, holding, now)
	}
}

// closedLeftPod is what follows the close of a pod whose issue left the workflow, whichever pass
// wrote it, inside the close's own transaction (ctx carries it): the end of the pod's hold streak,
// since whatever kept the pod is over, and, as hooks of the close's commit, one line and the wake
// of the issue's lane, which runs the close's rows. So a close whose reply is lost and that the
// store says committed still says so and still wakes its lane, and a streak that cannot be ended
// fails the close, which the next pass decides again. The row the pass wrote is the workspace check
// alone (leftPodCloseRows), read through a live role where one stands and through the pod's
// launcher where none does, and the line says which: the close follows only where that read finds
// nothing that origin does not hold.
func (w *workflowRuntime) closedLeftPod(ctx context.Context, issue string, decision workflow.IdleDecision, holding bool) error {
	if holding {
		if err := w.idleStore.EndPodHoldStreaks(ctx, w.projectID, issue); err != nil {
			return err
		}
	}
	roles := len(decision.Roles)
	store.OnCommit(ctx, func() {
		if roles > 0 {
			w.log.Info("left pod close: reading the workspace of the pod of an issue that left the workflow while its tree runs on, before its close stops a live role",
				"issue", issue, "idle", w.idlePeriod.String(), "roles", roles)
		} else {
			w.log.Info("left pod close: reading the workspace of the pod of an issue that left the workflow while its tree runs on through the pod's launcher, before its close deletes the disk; no role process runs there",
				"issue", issue, "idle", w.idlePeriod.String(), "roles", roles)
		}
		w.wakeOutbox(issue, record.OutboxKindIssueSuspend)
	})
	return nil
}

// runningPod is the uid of the pod the runtime holds for issue now, read before a pass's
// transaction as the pod's image is, and "" where the runtime cannot say
// (runtime.IssuePodReader). The rules read it against a decline's own (pods.IdleView.PodUID): a
// decline taken in another pod ends, since that pod's disk went with it.
func (w *workflowRuntime) runningPod(issue string) string {
	pod, read := w.idleRuntime.IssuePod(issue)
	if !read {
		return ""
	}
	return pod
}

// recordableHold is whether a hold is one a deployment reads as "release would have taken this
// pod and something stopped it". Eleven of workflow's values are not. Five belong to the idle
// rule:
//   - IdleHoldNone is a release, not a hold;
//   - IdleHoldNotIdle is a pod that is simply not idle yet, which is every working pod on nearly
//     every tick and says nothing about release;
//   - IdleHoldNoPod is an issue the record does not hold under this project, so there is no pod
//     of the workflow's to have kept;
//   - IdleHoldNoLiveClaim is an issue whose claims have all stopped. Such a view is quiet, since
//     the quiet period is measured from the pod's own claims and those claims stopped writing
//     when they stopped, so a pass over one would record a hold every tick for as long as the
//     record stands. It is no hold of the idle rule's anyway: the pod it names is the second
//     rule's to decide, and that rule records what it finds;
//   - IdleHoldReleasing is this daemon's own release already running on the lane, so recording it
//     would count every release again, once per tick of its teardown.
//
// Six belong to the second rule, the release of a pod no claim owns: IdleHoldUnownedClaimLive,
// IdleHoldUnownedNoClaim, IdleHoldUnownedSandboxGone, IdleHoldUnownedTreeNotLive,
// IdleHoldUnownedReleasing and IdleHoldUnownedNotIdle. In five of them no pod stands idle with
// nobody in it: the first names a pod a live claim still holds, which the idle rule decides and
// records; the second and the third name an issue with no pod at all, one with no claim that
// ever ran and one whose Sandbox is already gone; and the last two name this rule's own release
// already running and a quiet period that has not passed. A row for any of those five would say
// a pod was kept where nothing was kept.
//
// IdleHoldUnownedTreeNotLive, the fourth, is the one of the six whose pod can stand with nobody
// in it: the issue or its tree root has left the workflow, or one of them lingers, and the close
// that stops that pod has not finished. The rule meets it only where its candidate left the
// workflow between the candidate answer, which holds only issues in the workflow (UnownedPods),
// and the pass's own transaction. Of the pods of issues already out of the workflow with no live
// claim, the close pass decides a child's under a running tree (closeLeftPods) and records its
// own holds, and the idle rule decides only the issues that have a live claim (IdlePods), so no
// pod this rule reads is ever reported as IdleHoldTreeNotLive. It needs no row: the pass's own
// line counts the hold under unowned_tree_not_live.
//
// The pass that reads one of them ends the pod's streaks instead of recording, which is the one
// place a streak ends outside a hold of another reason (releaseIdlePod).
//
// Every other value is a reason worth a row, the ones that are working as designed
// (background_work, in_turn, delivery_waiting, reminder_due, awaiting_person) as much as the
// ones that are not (image, plugin_contract, unreported, unpushed_work, workspace_unread,
// overtaken, unauthorized): a deployment reads release's health from which of them it sees, so
// leaving the healthy ones out would leave it unable to tell a pod kept for good reason from one
// never decided at all. IdleHoldTreeNotLive is recorded too: it holds a pod of an issue the
// workflow has taken out only while a close of that pod is still to come or still running
// (workflow.LeftPodClose) — a tree that lingers, a root out of the workflow, an open look of the
// issue's production check, a role stop or a Sandbox suspension of the issue still queued — so a
// deployment that sees it every tick past a linger's end or a look's bound is seeing a close that
// is stuck.
//
// The second rule's other three — IdleHoldUnownedClaimProcess, IdleHoldUnownedSuspensionHolds
// and IdleHoldUnownedDelivery — are recorded for the same reason, and it is the sharpest one
// here: each names a pod that stands, past the quiet period, with no live claim in it, and such
// a pod holds its reservation (2 CPU and 8 GiB at the defaults) for as long as it stands. A
// deployment that sees one of the three every tick is reading capacity held by something that is
// not work. IdleHoldUnownedUnpushedWork is recorded too: its pod stands with no live claim
// because its disk holds work origin does not hold, and the record is where an operator finds
// the pod whose roles must come back to push it.
//
// A project's pause's three holds are recorded too (IdleHoldPaused, IdleHoldPauseHeld,
// IdleHoldUnownedPauseHeld, decision 12 of dispatch://INFRA-3759): each is a pod release would
// have taken that the pause kept on purpose, and a deployment that reads them can tell the
// capacity a pause holds from capacity nothing holds.
func recordableHold(hold workflow.IdleHold) bool {
	switch hold {
	case workflow.IdleHoldNone, workflow.IdleHoldNotIdle, workflow.IdleHoldNoPod,
		workflow.IdleHoldNoLiveClaim, workflow.IdleHoldReleasing,
		workflow.IdleHoldUnownedClaimLive, workflow.IdleHoldUnownedNoClaim,
		workflow.IdleHoldUnownedSandboxGone, workflow.IdleHoldUnownedTreeNotLive,
		workflow.IdleHoldUnownedReleasing, workflow.IdleHoldUnownedNotIdle:
		return false
	}
	return true
}

// recordKeptPod is the bookkeeping of a pod a pass kept, which is one policy and three passes:
// the idle rule's (releaseIdlePod), the second rule's (releaseUnownedPod), and that rule's pass
// over an issue whose Sandbox the runtime no longer holds, which reads no view at all. A pod
// quiet for the whole period under a reason a deployment reads as a hold gets a row of its own
// (recordIdleHold); anything else — a pod that did something inside the period, or a reason no
// deployment reads — ends whatever streak the pod had, so a later pass that does hold it opens
// a new one. A pod with no streak is left alone, since nearly every pass over a working pod is
// that one and an update that matches no row costs a write transaction all the same.
//
// The caller logs its own line first and hands over what it read: the hold and the detail its
// rule names ("" where it names none), whether the pod was quiet for the whole period, and
// whether a streak of it is running.
func (w *workflowRuntime) recordKeptPod(ctx context.Context, issue string, hold workflow.IdleHold,
	detail string, quiet, holding bool, now time.Time) {
	if !quiet || !recordableHold(hold) {
		if holding {
			endIdleHolds(ctx, w.idleStore, w.projectID, issue, w.log)
		}
		return
	}
	recordIdleHold(ctx, w.idleStore, w.projectID, issue, hold, detail, now, w.log)
}

// recordIdleHold records one pass of the idle rule that kept a pod past the idle period, under
// the rule's own word for the reason and the detail that word leaves out ("" where there is
// none), and ends every streak of that pod under another reason (store.RecordPodHold). It writes
// every hold it is given: the passes read recordableHold before they call (recordKeptPod). Ending
// a streak is recordKeptPod's, which knows whether the pod has one. A record that cannot be
// written is logged and nothing else: the records are a deployment's measurement, and a release
// is a saving, so nothing waits on either.
func recordIdleHold(ctx context.Context, st *store.Store, project, issue string, hold workflow.IdleHold, detail string, at time.Time, log *slog.Logger) {
	if err := st.RecordPodHold(ctx, store.PodHold{Project: project, Issue: issue, Reason: hold.String(), Detail: detail, At: at}); err != nil {
		log.Warn("idle release: could not record a pod's hold", "issue", issue, "hold", hold.String(), "error", err)
	}
}

// endIdleHolds ends every hold streak of a pod, which is what a pass that reads no hold worth a
// row leaves behind.
func endIdleHolds(ctx context.Context, st *store.Store, project, issue string, log *slog.Logger) {
	if err := st.EndPodHoldStreaks(ctx, project, issue); err != nil {
		log.Warn("idle release: could not end a pod's hold streaks", "issue", issue, "error", err)
	}
}

// recordPodRelease writes the record of one completed release: the run it released, the roles
// whose claims it suspended as released and the moment it completed. It is called from the
// teardown (releaseIssue), the one place the fact that the release finished is known.
//
// The roles are the ones this release's own stops suspended, as its mark recorded them
// (store.ReleaseSuspendedClaim), and never every released claim of the issue: a claim an earlier
// release left `released` and no wake has brought back is still released now, and counting it
// here would charge this release with a role it never touched and the hours that role's pod was
// gone a second time.
//
// The record names the release's mark (release, issue_releases.id), so a teardown that runs again
// after its record and before the mark's clear writes no second record (store.RecordPodRelease).
// The release line follows the record's first write. A retry whose record was already written
// logs no second line.
//
// If the write fails, the teardown logs the release line beside the warning: the pod is already
// released, and nothing undoes that for want of a row.
func (r *outbox) recordPodRelease(ctx context.Context, issue string, release int64, generation uint64, suspended []claim.Role) bool {
	if r.supervisor == nil || r.supervisor.store == nil {
		return true
	}
	roles := make([]string, 0, len(suspended))
	for _, role := range suspended {
		roles = append(roles, string(role))
	}
	slices.Sort(roles)
	_, wrote, err := r.supervisor.store.RecordPodRelease(ctx, store.PodRelease{
		Project: r.project, Issue: issue, Generation: generation, Roles: roles,
		At: time.Now().UTC(), Release: release,
	})
	if err != nil {
		r.log.Warn("outbox: could not record an issue pod's release", "issue", issue, "error", err)
		return true
	}
	return wrote
}

// releaseHold is the hold a cancel the store decided is recorded under, and the sentence the
// log line carries. The cause is the store's answer and not the row's, so a pod taken up again
// is one reason in the records wherever the release read it: at a role stop, where the start or
// the delivery arrives, and at the teardown, where the same read runs again. The store names a
// cause on every cancel it decides, and those three are all it names: the pod was given work, the
// store no longer authorizes the release, or the project was paused before the release's
// workspace check ran (store.ReleaseCausePaused: #4115's project pause starts no release). A check
// that declines the release names its own hold (declineRelease).
func releaseHold(cause store.ReleaseCause) (workflow.IdleHold, string) {
	switch cause {
	case store.ReleaseCauseOvertaken:
		return workflow.IdleHoldOvertaken, "the pod was given work after its release was marked"
	case store.ReleaseCausePaused:
		return workflow.IdleHoldPaused, "the project was paused before the release checked its workspace"
	}
	return workflow.IdleHoldUnauthorized, "the store no longer authorizes it"
}

// idleReleaseRows is the release as outbox rows, in the order the issue's lifecycle lane runs
// them — three acts, each one row a reader can name:
//
//  1. the workspace check (record.IssueWorkspaceCheck): the shim of a live role reads whether the
//     workspace every role shares holds work that no branch or tag on origin holds. It runs first
//     because only a shim can read it and a shim dies with its role's process, which act 2
//     stops. It stops nothing itself, so a release it declines leaves the pod as it was;
//  2. the role stops: one `release` supervise row per live role, each stopping that role's
//     process and keeping its session. The first of them is the release's boundary — the stop of
//     the first role process of the pod — which is what R4's cancel is read against, so a
//     delivery queued while the check ran is still below it and cancels the whole release, save a
//     start that waits for its pod, which the next pod serves;
//  3. the teardown (record.IssueRelease): it deletes the pod, its disk and its Sandbox, and
//     records the release. A Sandbox that owns the issue's kept volume stays, Suspended, with
//     that volume: its pod and its Secrets go (runtime.IssueReleaser).
//
// The role the check reads through, which the mark names, is the role told when the check
// declines (declineRelease): checkRole's.
func idleReleaseRows(view pods.IdleView, decision workflow.IdleDecision, now time.Time) (store.IdleReleaseMark, error) {
	told := checkRole(view, decision.Roles)
	mark := store.IdleReleaseMark{Generation: view.Generation, Role: told}
	rows := markRows{issue: view.Issue, act: "release", now: now, rows: make([]store.IdleReleaseRow, 0, len(decision.Roles)+2)}
	if err := rows.add(record.IssueWorkspaceCheck{Tree: view.Tree, Generation: view.Generation,
		TreeGeneration: view.Root.Generation, Role: mark.Role}, false); err != nil {
		return store.IdleReleaseMark{}, err
	}
	for i, role := range decision.Roles {
		if err := rows.add(record.SuperviseRequest{Op: "release", Tree: view.Tree, Role: role,
			Generation: view.Generation, Reason: "the issue's pod has been idle for the release period"}, i == 0); err != nil {
			return store.IdleReleaseMark{}, err
		}
	}
	if err := rows.add(record.IssueRelease{Tree: view.Tree, Generation: view.Generation,
		TreeGeneration: view.Root.Generation}, false); err != nil {
		return store.IdleReleaseMark{}, err
	}
	mark.Rows = rows.rows
	return mark, nil
}

// checkRole is the role a workspace check of view's pod reads through, and the one its decline
// tells: the worker of the issue's open phase where it is live (workflow.OpenRoleAt, which for step
// is the role of the running step, view.StepRole), since that is the role whose work the phase is,
// and otherwise the first of live. Every role of the issue shares the one workspace, so any of them
// reads the same answer.
func checkRole(view pods.IdleView, live []claim.Role) claim.Role {
	if open := workflow.OpenRoleAt(view.Phase, view.StepRole); slices.Contains(live, open) {
		return open
	}
	return live[0]
}

// markRows is the outbox rows of one release's or close's mark as the store writes them
// (store.IdleReleaseMark.Rows), each encoded for issue at now; act names the act, "release" or
// "close", in an error. Every builder of a mark's rows below adds its rows through it.
type markRows struct {
	issue, act string
	now        time.Time
	rows       []store.IdleReleaseRow
}

// add encodes payload as the mark's next row. boundary marks the row the store reads as the
// release's boundary (store.IdleReleaseRow.Boundary).
func (m *markRows) add(payload record.OutboxPayload, boundary bool) error {
	row, err := record.NewOutboxRow(m.issue, payload, m.now)
	if err != nil {
		return fmt.Errorf("encode the %s of %s's %s: %w", payload.OutboxKind(), m.issue, m.act, err)
	}
	m.rows = append(m.rows, store.IdleReleaseRow{Kind: string(row.Kind), Payload: row.Payload, Boundary: boundary})
	return nil
}

// leftPodCloseRows is the close of a pod whose issue left the workflow as outbox rows
// (workflow.LeftPodClose): one row, and no other, the workspace check (record.IssueWorkspaceCheck),
// which queues the stops and the suspension only where its read finds nothing that origin does not
// hold (checkLeftPodWorkspace), since those delete the disk. A close that is Read runs no role to
// read through, so its pod's launcher reads the pod (launcherCheck, with Close: R6 of
// #5285); any other close names its live roles, and is read through the open
// phase's worker where it is live, else the first live role (checkRole, CloseRoles). The rows carry no
// release and the store writes them with no mark (store.IdleReleaseMark.Close).
func leftPodCloseRows(view pods.IdleView, decision workflow.IdleDecision, now time.Time) (store.IdleReleaseMark, error) {
	var check record.IssueWorkspaceCheck
	if decision.Read {
		check = launcherCheck(view)
		check.Close = true
	} else {
		check = record.IssueWorkspaceCheck{Tree: view.Tree, Generation: view.Generation,
			TreeGeneration: view.Root.Generation, Role: checkRole(view, decision.Roles), CloseRoles: decision.Roles}
	}
	check.TurnStart = view.NewestTurnStart()
	rows := markRows{issue: view.Issue, act: "close", now: now}
	if err := rows.add(check, false); err != nil {
		return store.IdleReleaseMark{}, err
	}
	return store.IdleReleaseMark{Generation: view.Generation, Close: true, Rows: rows.rows}, nil
}

// launcherCheck is the workspace check the pod's launcher reads where no role process runs in the
// pod (record.IssueWorkspaceCheck.Launcher, R6 of #5285): of the release of a pod no
// claim owns and, with Close, of the close of a left pod with no live claim. It names no role,
// since no role's shim reads it, and names the pod the pass read the runtime holding, which binds
// the read and the teardown or suspension a clean read lets through (PodUID).
func launcherCheck(view pods.IdleView) record.IssueWorkspaceCheck {
	return record.IssueWorkspaceCheck{Tree: view.Tree, Generation: view.Generation,
		TreeGeneration: view.Root.Generation, Launcher: true, PodUID: view.PodUID}
}

// leftPodStopsAfterCheck is the decision the close's workspace check makes once its read found
// nothing that origin does not hold (checkLeftPodWorkspace): the close's stops and suspension
// (leftPodStopRows) over a view read again in the store's transaction, where the close still
// stands there and no claim started a turn after turnStart, the newest turn start of the pass that
// queued the check (record.IssueWorkspaceCheck.TurnStart, workflow.LeftPodCloseAfterCheck), and no
// rows where it does not. Each stop and the suspension carry turnStart on, so a turn that starts
// after this decision and before the lane runs the stop holds the stop and finishes the suspension
// (record.SuperviseRequest.TurnStart, record.IssueSuspend.TurnStart).
func leftPodStopsAfterCheck(now, turnStart time.Time, pod string) func(pods.IdleView) (store.IdleReleaseMark, error) {
	return func(view pods.IdleView) (store.IdleReleaseMark, error) {
		decision := workflow.LeftPodCloseAfterCheck(view, turnStart)
		if !decision.Close {
			return store.IdleReleaseMark{}, nil
		}
		return leftPodStopRows(view, decision, now, turnStart, pod)
	}
}

// leftPodStopRows is the close itself, the rows that issue's departure wrote (the workflow's leave):
// one `suspend` supervise row per live role, at the issue's generation, which suspends the claim
// for the daemon with its session kept, and then the issue's Sandbox suspension
// (record.IssueSuspend), which deletes the Sandbox, the pod and the disk once every role has
// stopped (store.IssueSuspension). Every close reaches it only through its workspace check
// (leftPodCloseRows, checkLeftPodWorkspace), and every row of it is fenced on turnStart, the turn
// start that check read (record.SuperviseRequest.TurnStart, record.IssueSuspend.TurnStart): a
// turn after the read, of a role the close stops or of one that started since in a pod no role
// ran in, voids the read, and the suspension finishes without acting.
//
// pod, for a close whose launcher read the workspace, is the pod that read found clean
// (record.IssueWorkspaceCheck.PodUID): the suspension takes that pod or nothing
// (record.IssueSuspend.PodUID). A close of live roles binds none ("").
func leftPodStopRows(view pods.IdleView, decision workflow.IdleDecision, now, turnStart time.Time, pod string) (store.IdleReleaseMark, error) {
	rows := markRows{issue: view.Issue, act: "close", now: now}
	fence := &turnStart
	reason := fmt.Sprintf("%s left the workflow, and its pod has been idle for the release period", view.Issue)
	for _, role := range decision.Roles {
		if err := rows.add(record.SuperviseRequest{Op: "suspend", Tree: view.Tree, Role: role,
			Generation: view.Generation, Reason: reason, TurnStart: fence}, false); err != nil {
			return store.IdleReleaseMark{}, err
		}
	}
	if err := rows.add(record.IssueSuspend{Tree: view.Tree, Generation: view.Generation,
		TreeGeneration: view.Root.Generation, TurnStart: fence, PodUID: pod}, false); err != nil {
		return store.IdleReleaseMark{}, err
	}
	return store.IdleReleaseMark{Generation: view.Generation, Close: true, Rows: rows.rows}, nil
}

// unownedReleaseRows is the release of a pod no claim owns as outbox rows. Its last act is the
// teardown (record.IssueRelease), which deletes the pod, its disk and its Sandbox, or, for a
// Sandbox that owns the issue's kept volume, the pod and the Secrets alone
// (runtime.IssueReleaser). There are no role stops, because every claim of the issue is already
// failed, retired or suspended. A release that deletes the disk (decision.Read) runs the workspace
// check first (record.IssueWorkspaceCheck, Launcher): no role process runs in the pod, so its
// launcher reads the workspace, and a read that finds work that origin does not hold, or that
// cannot be made, declines the release and keeps the pod (checkIssueWorkspace, R6 of
// #5285, launcherCheck), and that decline tells the issue's architect. The
// teardown waits for the check (store.IssueRelease), and a start or a turn of a claim of the issue
// after the mark cancels it (store's releaseOvertaken), so a read a later turn could make stale
// deletes nothing. A release of a pod whose Sandbox keeps a volume the next pod mounts deletes no
// disk, and queues the teardown alone.
//
// The teardown carries the release's boundary flag. The store requires some row of a release to
// be its boundary (store.ReleaseIdleIssue), and no reader of this release reads it: the
// boundary's own reader is the cancel that a delivery below the stop of the first role process
// cancels (store.IssueReleaseStop), which belongs to a release that stops roles. The flag on
// the teardown is what satisfies the store, and a reader who goes looking for what it means
// here finds this.
//
// The mark says two things about the release beside its rows: SuspendsNoRole, which is what
// writes issue_releases.kind as `unowned` and leaves the role column empty, so the teardown and
// the cancel can read what this release did rather than guess it from a missing role; and the
// generation of the issue the view was read at, as every mark carries.
func unownedReleaseRows(view pods.IdleView, decision workflow.IdleDecision, now time.Time) (store.IdleReleaseMark, error) {
	rows := markRows{issue: view.Issue, act: "release", now: now}
	// pod is the pod the launcher's check reads, which binds the teardown (record.IssueRelease's
	// PodUID): a release that reads nothing binds none.
	pod := ""
	if decision.Read {
		check := launcherCheck(view)
		pod = check.PodUID
		if err := rows.add(check, false); err != nil {
			return store.IdleReleaseMark{}, err
		}
	}
	if err := rows.add(record.IssueRelease{Tree: view.Tree, Generation: view.Generation,
		TreeGeneration: view.Root.Generation, PodUID: pod}, true); err != nil {
		return store.IdleReleaseMark{}, err
	}
	return store.IdleReleaseMark{Generation: view.Generation, SuspendsNoRole: true, Rows: rows.rows}, nil
}

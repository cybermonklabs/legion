package daemon

import (
	"context"
	"fmt"
	"iter"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// idleTickInterval is how often the daemon decides every pod of its project. The shortest idle
// period an operator would set is minutes, and one read per issue is three small queries, so a
// minute is often enough to release a pod within a tick of its period passing and rare enough to
// cost nothing.
const idleTickInterval = time.Minute

// issuePods is what the idle pass asks the runtime about an issue's pod, which is everything
// about a release the daemon's own Postgres does not hold: whether the pod runs the image the
// configuration names (sandbox.Runtime.IssuePodRunsConfiguredImage), and whether the runtime
// still holds the issue's Sandbox and under what name (sandbox.Runtime.IssueSandbox). A runtime
// that runs no pods implements neither, and the daemon then releases nothing, which is also what
// `runtime: tmux` wants.
type issuePods interface {
	IssuePodRunsConfiguredImage(issue string) bool
	IssueSandbox(issue string) (name string, present bool)
}

// issuePodRepository is the repository the pod of issue was built from, read through the claims
// of the issue the daemon supervises (podRepository). Two readers want it: the idle rule's
// repository hold (podRepositoryDrifted) and the label of the bundle a release saves
// (outbox.bundleIssueWorkspace), and the two read the one pod the one way.
//
// It is the pod's own fact and not the record's: a pod clones one repository into the issue's
// workspace at its creation and holds the gh directories and the token of that repository's
// owner for its whole life, so this is the repository every role in it works, and the
// repository every byte of unpushed work on its disk is of, until the pod is replaced.
//
// The claims are read as their machines last published them (supervise.Machine.View), never
// waiting on a decision in flight, exactly as the capability report reads them
// (supervision.deployment).
//
// It answers "" with no error where nothing can say: a daemon with no supervisor or no runtime,
// an issue no claim of which records a process, and the tmux runtime, whose panes share no pod
// and which answers "" itself. Each of those is an unknown rather than a statement, which is
// the rule every reader of a pod's repository already follows (repositoryDrift).
func issuePodRepository(ctx context.Context, sup *supervisor, issue string) (string, error) {
	if sup == nil || sup.deps.Runtime == nil {
		return "", nil
	}
	return podRepository(ctx, sup.deps.Runtime, func(yield func(supervise.Claim) bool) {
		for _, m := range sup.supervised() {
			if c := m.View().Claim; c.Issue == issue && !yield(c) {
				return
			}
		}
	})
}

// podRepository is the repository the pod claims run in was built from, as that pod itself
// names it (runtime.Runtime.PodRepository), asked through the claim of claims that records a
// process and has the lowest token. One claim answers for the whole issue: every role of an
// issue runs in that one pod and works the one workspace its fetch cloned. The claim is chosen
// by its token and not by the order claims arrive in, so every reader asks the pod through the
// same claim whichever list it holds: the idle rule and a release's save read the claims the
// daemon supervises (issuePodRepository), and the update-round sweep reads the claims the store
// answered (supervision.groupDrift). That is what lets the release and the replacement agree on
// which pods are on a repository the issue has left (podRepositoryDrifted). It answers "" with
// no error where no claim records a process.
func podRepository(ctx context.Context, rt runtime.Runtime, claims iter.Seq[supervise.Claim]) (string, error) {
	var pod supervise.Claim
	found := false
	for c := range claims {
		if c.Locator != nil && (!found || c.Token < pod.Token) {
			pod, found = c, true
		}
	}
	if !found {
		return "", nil
	}
	repository, err := rt.PodRepository(ctx, runtime.Known{Claim: pod.Token, Locator: pod.Locator})
	if err != nil {
		return "", fmt.Errorf("ask the pod through the claim %s: %w", pod.Token, err)
	}
	return repository, nil
}

// podRepositoryDrifted is whether the issue's pod was built from a repository the issue's
// record no longer holds, which is the idle rule's repository hold (workflow.IdleHoldRepository)
// and the one input of it the daemon's Postgres does not hold on its own. It is the update-round
// sweep's reading of the same pair, through the same claim (podRepository) and the sweep's own
// comparison (repositoryDrift), so the release and the replacement never disagree about which
// pods are on a repository the issue has left: the sweep opens a round over exactly the pods
// this holds.
//
// Anything that leaves either name unread answers false — a runtime that cannot be asked, a pod
// that names no repository, a deployment with no workflow, a record that cannot be read — since
// an unknown is never a statement that something drifted, and a hold on one would keep a pod
// that nothing is going to replace.
func (s *supervision) podRepositoryDrifted(ctx context.Context, issue string) bool {
	pod, err := issuePodRepository(ctx, s.supervisor, issue)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Error("idle release: read the repository this issue's pod was built from", "issue", issue, "error", err)
		}
		return false
	}
	return s.repositoryDrift(ctx, issue, pod) != ""
}

// tickIdle decides every pod of the project on the daemon's own period (idleTickInterval),
// under both release rules: the release of a pod no claim owns, whose every claim is failed,
// retired or suspended (releaseUnownedPods), and the idle release, which takes a pod whose live
// roles have had no turn in progress and no turn due for the idle period (releaseIdlePods). It
// returns at once — running no ticker at all — when release is off: `idle_minutes: 0`, or a
// runtime that is not Kubernetes, which has no pod to release and no image to compare. Both
// rules are off with it, since one period and one tick carry the two.
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
// rest are decided: a release is a saving, never a correctness step, and nothing waits on it.
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
// pod's image and the repository it was built from are read before that transaction, from the
// runtime's own store of pods, because they are the inputs the daemon's Postgres does not hold.
// The repository, and the record it is read against, are read only for a pod on the configured
// image: the rule holds any other pod for its image, or for an earlier reason, before it reads
// the repository (workflow.IdleRelease), so a read there would decide nothing. A deployment that
// wired no repository reader — a test that builds the runtime by hand — reads no drift, as a pod
// whose repository cannot be read does.
//
// A pod the rule kept that was quiet for the whole idle period is recorded as a hold
// (recordIdleHold): that is a pod release would have taken and something else stopped, which is
// what tells a deployment whether release is working. The release itself is recorded where it
// completes, since that is where the bundle it saved is known (outbox.releaseIssue, through
// recordPodRelease).
func (w *workflowRuntime) releaseIdlePod(ctx context.Context, issue string) {
	now := time.Now().UTC()
	onConfiguredImage := w.idleRuntime.IssuePodRunsConfiguredImage(issue)
	repositoryDrifted := onConfiguredImage && w.idleRepositoryDrifted != nil && w.idleRepositoryDrifted(ctx, issue)
	var decision workflow.IdleDecision
	var quiet, holding bool
	marked, err := w.idleStore.ReleaseIdleIssue(ctx, w.projectID, issue, func(view pods.IdleView) (store.IdleReleaseMark, error) {
		decision = workflow.IdleRelease(view, now, w.idlePeriod, onConfiguredImage, repositoryDrifted, api.DaemonAPIVersion)
		since, found := view.QuietSince()
		quiet = found && now.Sub(since) >= w.idlePeriod
		holding = view.Holding
		if !decision.Release {
			return store.IdleReleaseMark{}, nil
		}
		return idleReleaseRows(view, decision, now)
	})
	if err != nil {
		w.log.Warn("idle release: could not decide an issue's pod", "issue", issue, "error", err)
		return
	}
	if !marked {
		detail := strings.Join(decision.HoldKinds, ",")
		w.log.Debug("idle release: the pod is kept", "issue", issue, "hold", decision.Hold.String(), "detail", detail)
		w.recordKeptPod(ctx, issue, decision.Hold, detail, quiet, holding, now)
		return
	}
	w.log.Info("idle release: releasing an issue's pod, which has had no turn in progress and no turn due for the idle period",
		"issue", issue, "idle", w.idlePeriod.String(), "roles", len(decision.Roles))
	w.wakeOutbox(issue, record.OutboxKindSupervise)
}

// releaseUnownedPods decides each pod of the project that no claim of its issue can use: an
// issue whose every claim is failed, retired or suspended holds a pod of 3 CPU and 19 GiB that
// no role can take a turn in, and this is the pass that releases it. One issue's failure is
// logged and the rest are decided, as the idle pass has it: a release is a saving, never a
// correctness step, and nothing waits on it.
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
// something else stopped. Three of this rule's nine holds are such a hold, and recordableHold is
// what says which. The release itself is recorded where it completes (outbox.releaseIssue).
func (w *workflowRuntime) releaseUnownedPod(ctx context.Context, issue string) (workflow.IdleHold, bool) {
	now := time.Now().UTC()
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
	marked, err := w.idleStore.ReleaseIdleIssue(ctx, w.projectID, issue, func(view pods.IdleView) (store.IdleReleaseMark, error) {
		// The Sandbox is there: the read above is this conjunct's answer, and the rule takes it
		// rather than reading the runtime again under the transaction's lock.
		decision = workflow.UnownedPodRelease(view, now, w.idlePeriod, true)
		since, found := view.QuietSince()
		quiet = found && now.Sub(since) >= w.idlePeriod
		holding = view.Holding
		if !decision.Release {
			return store.IdleReleaseMark{}, nil
		}
		return unownedReleaseRows(view, now)
	})
	if err != nil {
		w.log.Warn("unowned release: could not decide an issue's pod", "issue", issue, "error", err)
		return workflow.IdleHoldNone, false
	}
	if !marked {
		w.log.Debug("unowned release: the pod is kept", "issue", issue, "hold", decision.Hold.String())
		// This rule names one reason per pass and no kinds beside it, so its holds carry no
		// detail, as the teardown's do not.
		w.recordKeptPod(ctx, issue, decision.Hold, "", quiet, holding, now)
		return decision.Hold, false
	}
	// The line that ends this release is the teardown's, and it is the one Info line of a
	// release no role is waiting on; the mark is a Debug line here for that reason.
	w.log.Debug("unowned release: releasing an issue's pod, which no claim of it can use",
		"issue", issue, "idle", w.idlePeriod.String())
	w.wakeOutbox(issue, record.OutboxKindSupervise)
	return workflow.IdleHoldNone, true
}

// recordableHold is whether a hold is one a deployment reads as "release would have taken this
// pod and something stopped it". Eleven of workflow's values are not. Five belong to the idle
// rule:
//   - IdleHoldNone is a release, not a hold;
//   - IdleHoldNotIdle is a pod that is simply not idle yet, which is every working pod on nearly
//     every tick and says nothing about release;
//   - IdleHoldNoPod is an issue the record does not hold, so there is no pod to have kept;
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
// that stops that pod has not finished. The idle rule reports no such pod either — it decides
// the issues that have a live claim (IdlePods) and this rule the issues that have none
// (UnownedPods), so the two candidate sets are disjoint and no pod this rule reads is ever
// reported as IdleHoldTreeNotLive — and a close stuck on one leaves no hold row at all. It needs
// none: the pass's own line counts the hold under unowned_tree_not_live once a minute for as
// long as the pod stands, and `legion state` reports what the tree is doing, which is what the
// operator acts on. A row would be a third reading of one stuck close with no fact of its own.
//
// The pass that reads one of them ends the pod's streaks instead of recording, which is the one
// place a streak ends outside a hold of another reason (releaseIdlePod).
//
// Every other value is a reason worth a row, the ones that are working as designed
// (background_work, in_turn, delivery_waiting, reminder_due, awaiting_person, repository, which
// is a pod the update round is about to replace) as much as the ones that are not (image,
// plugin_contract, unreported, bundle_too_large, bundle_save_failed, overtaken, unauthorized): a
// deployment reads release's health from which of them it sees, so leaving the healthy ones out
// would leave it unable to tell a pod kept for good reason from one
// never decided at all. IdleHoldTreeNotLive is recorded too: a pod of an issue the workflow has
// taken out is one the close is stopping, and a deployment that sees it every tick is seeing a
// close that is stuck.
//
// The second rule's other three — IdleHoldUnownedClaimProcess, IdleHoldUnownedOperatorSuspended
// and IdleHoldUnownedDelivery — are recorded for the same reason, and it is the sharpest one
// here: each names a pod that stands, past the quiet period, with no live claim in it, and such
// a pod holds 3 CPU and 19 GiB for as long as it stands. A deployment that sees one of the three
// every tick is reading capacity held by something that is not work.
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
// every hold it is given: the passes read recordableHold before they call (recordKeptPod), and
// the teardown's holds come from releaseHold, which names only recordable ones. Ending a streak
// is recordKeptPod's, which knows whether the pod has one. A record that cannot be written is
// logged and nothing else: the records are a deployment's measurement, and a release is a
// saving, so nothing waits on either.
func recordIdleHold(ctx context.Context, st *store.Store, project, issue string, hold workflow.IdleHold, detail string, at time.Time, log *slog.Logger) {
	if st == nil {
		return
	}
	if err := st.RecordPodHold(ctx, store.PodHold{Project: project, Issue: issue, Reason: hold.String(), Detail: detail, At: at}); err != nil {
		log.Warn("idle release: could not record a pod's hold", "issue", issue, "hold", hold.String(), "error", err)
	}
}

// endIdleHolds ends every hold streak of a pod, which is what a pass that reads no hold worth a
// row leaves behind.
func endIdleHolds(ctx context.Context, st *store.Store, project, issue string, log *slog.Logger) {
	if st == nil {
		return
	}
	if err := st.EndPodHoldStreaks(ctx, project, issue); err != nil {
		log.Warn("idle release: could not end a pod's hold streaks", "issue", issue, "error", err)
	}
}

// recordPodRelease writes the record of one completed release: the run it released, the roles
// whose claims it suspended as released, the moment it completed, and the size of the bundle of
// unpushed work it saved — zero when the workspace held none. It is called from the teardown
// (releaseIssue), the one place both the bundle and the fact that the release finished are known.
//
// The roles are the ones this release's own stops suspended, as its mark recorded them
// (store.ReleaseSuspendedClaim), and never every released claim of the issue: a claim an earlier
// release left `released` and no wake has brought back is still released now, and counting it
// here would charge this release with a role it never touched and the hours that role's pod was
// gone a second time.
//
// A record that cannot be written is logged: the pod is already released, and nothing undoes
// that for want of a row.
func (r *outbox) recordPodRelease(ctx context.Context, issue string, generation uint64, suspended []claim.Role, bundleBytes int) {
	if r.supervisor == nil || r.supervisor.store == nil {
		return
	}
	roles := make([]string, 0, len(suspended))
	for _, role := range suspended {
		roles = append(roles, string(role))
	}
	slices.Sort(roles)
	if _, err := r.supervisor.store.RecordPodRelease(ctx, store.PodRelease{
		Project: r.project, Issue: issue, Generation: generation, Roles: roles,
		At: time.Now().UTC(), BundleBytes: bundleBytes,
	}); err != nil {
		r.log.Warn("outbox: could not record an issue pod's release", "issue", issue, "error", err)
	}
}

// releaseHold is the hold a cancel the store decided is recorded under, and the sentence the
// log line carries. The cause is the store's answer and not the row's, so a pod taken up again
// is one reason in the records wherever the release read it: at a role stop, where the start or
// the delivery arrives, and at the teardown, where the same read runs again. The store names a
// cause on every cancel it decides, and those two are all it names.
func releaseHold(cause store.ReleaseCause) (workflow.IdleHold, string) {
	if cause == store.ReleaseCauseOvertaken {
		return workflow.IdleHoldOvertaken, "the pod was given work after its release was marked"
	}
	return workflow.IdleHoldUnauthorized, "the store no longer authorizes it"
}

// recordPodHold writes the hold of a release that did not finish (cancelRelease): the pod was
// kept, and the reason the teardown read is the same vocabulary the idle tick records. The
// teardown reads a hold and not the view behind it, so its holds carry no detail.
func (r *outbox) recordPodHold(ctx context.Context, issue string, hold workflow.IdleHold) {
	if r.supervisor == nil {
		return
	}
	recordIdleHold(ctx, r.supervisor.store, r.project, issue, hold, "", time.Now().UTC(), r.log)
}

// idleReleaseRows is the release as outbox rows, in the order the issue's lifecycle lane runs
// them — three acts, each one row a reader can name:
//
//  1. the save (record.IssueBundle): the shim of the first live role is asked for the git bundle
//     of the workspace's unpushed work, which is stored in the daemon's database. It runs first
//     because only a shim can build it and a shim dies with its role's process, which act 2
//     stops. It stops nothing itself, so a release cancelled here costs only the save;
//  2. the role stops: one `release` supervise row per live role, each stopping that role's
//     process and keeping its session. The first of them is the release's boundary — the stop of
//     the first role process of the pod — which is what R4's cancel is read against, so a
//     delivery queued while the save ran is still below it and cancels the whole release;
//  3. the teardown (record.IssueRelease): it deletes the pod, its disk and its Sandbox, and
//     records the release with the size the save stored.
func idleReleaseRows(view pods.IdleView, decision workflow.IdleDecision, now time.Time) (store.IdleReleaseMark, error) {
	mark := store.IdleReleaseMark{Generation: view.Generation, Role: decision.Roles[0]}
	rows := make([]store.IdleReleaseRow, 0, len(decision.Roles)+2)
	add := func(payload record.OutboxPayload, boundary bool) error {
		row, err := record.NewOutboxRow(view.Issue, payload, now)
		if err != nil {
			return fmt.Errorf("encode the %s of %s's release: %w", payload.OutboxKind(), view.Issue, err)
		}
		rows = append(rows, store.IdleReleaseRow{Kind: string(row.Kind), Payload: row.Payload, Boundary: boundary})
		return nil
	}
	if err := add(record.IssueBundle{Tree: view.Tree, Generation: view.Generation,
		TreeGeneration: view.Root.Generation, Role: mark.Role}, false); err != nil {
		return store.IdleReleaseMark{}, err
	}
	for i, role := range decision.Roles {
		if err := add(record.SuperviseRequest{Op: "release", Tree: view.Tree, Role: role,
			Generation: view.Generation, Reason: "the issue's pod has been idle for the release period"}, i == 0); err != nil {
			return store.IdleReleaseMark{}, err
		}
	}
	if err := add(record.IssueRelease{Tree: view.Tree, Generation: view.Generation,
		TreeGeneration: view.Root.Generation}, false); err != nil {
		return store.IdleReleaseMark{}, err
	}
	mark.Rows = rows
	return mark, nil
}

// unownedReleaseRows is the release of a pod no claim owns as outbox rows: one act and one row,
// the teardown (record.IssueRelease), which deletes the pod, its disk and its Sandbox. The two
// acts the idle release runs before its own teardown have nothing to do here. There is no save,
// because no shim is left to build a bundle and no role is coming back to a workspace that
// would need one, and there are no role stops, because every claim of the issue is already
// failed, retired or suspended.
//
// That one row carries the release's boundary flag. The store requires some row of a release to
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
func unownedReleaseRows(view pods.IdleView, now time.Time) (store.IdleReleaseMark, error) {
	payload := record.IssueRelease{Tree: view.Tree, Generation: view.Generation,
		TreeGeneration: view.Root.Generation}
	row, err := record.NewOutboxRow(view.Issue, payload, now)
	if err != nil {
		return store.IdleReleaseMark{}, fmt.Errorf("encode the %s of %s's release: %w",
			payload.OutboxKind(), view.Issue, err)
	}
	return store.IdleReleaseMark{Generation: view.Generation, SuspendsNoRole: true,
		Rows: []store.IdleReleaseRow{{Kind: string(row.Kind), Payload: row.Payload, Boundary: true}}}, nil
}

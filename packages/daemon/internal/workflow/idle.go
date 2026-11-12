package workflow

import (
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// IdleHold is why a pod the idle rule read was not released, or IdleHoldNone when it was. The
// daemon records the hold of every pod it keeps past the idle period, so the value is the
// vocabulary of that record and String is how it is written down.
type IdleHold int

const (
	// IdleHoldNone: nothing held the pod and it is released.
	IdleHoldNone IdleHold = iota
	// IdleHoldTreeNotLive: the issue or its tree root has left the workflow, or the root lingers.
	// Such a pod is a close's to stop, not a release's.
	IdleHoldTreeNotLive
	// IdleHoldAwaitingPerson: the pod is a root's, and the root waits on an answer a person has
	// not given. Its children's pods are not held by it.
	IdleHoldAwaitingPerson
	// IdleHoldImage: the pod runs an image other than the one the configuration names, or its
	// image could not be read at all.
	IdleHoldImage
	// IdleHoldRepository: the pod was built from a repository the issue's record no longer
	// holds, because Dispatch moved the issue and the daemon's record followed it
	// (record.Store.CorrectIssueRepository). Such a pod is a replacement's to take and never a
	// release's: every role in it works a clone of the repository the issue has left, and the
	// bundle a release would save out of it is work no pod of the issue can ever restore
	// (store.TakeWorkspaceBundleForRestore refuses it). It is the image hold's neighbour for
	// that reason — a pod already behind is a relaunch's to replace, not a release's to save.
	IdleHoldRepository
	// IdleHoldPluginContract: a live claim of the pod speaks a plugin contract other than the
	// daemon's current one.
	IdleHoldPluginContract
	// IdleHoldUpdateRound: an update round of the pod is open on a live claim of it, so the pod
	// is already being replaced and the replacement is waiting for its workers to push.
	IdleHoldUpdateRound
	// IdleHoldInTurn: a claim of the pod has a process in a turn, starting or stopping, or one
	// whose launch is not yet known.
	IdleHoldInTurn
	// IdleHoldDelivery: a task, notice, message or start waits for delivery to a role of the pod.
	IdleHoldDelivery
	// IdleHoldBackgroundWork: a live role's session reported background work of its own running.
	IdleHoldBackgroundWork
	// IdleHoldUnreported: a live role's session has reported nothing, or last reported before its
	// own last turn, so nothing says what it is doing now.
	IdleHoldUnreported
	// IdleHoldReminder: a live phase worker whose phase is open has a phase reminder due inside
	// the idle period, which is a turn due.
	IdleHoldReminder
	// IdleHoldNotIdle: the pod did something inside the idle period, so the period has not passed.
	IdleHoldNotIdle
	// IdleHoldNoPod: the record holds no such issue, so there is no pod of the workflow's to
	// release at all. An issue the record does hold whose claims have all stopped is
	// IdleHoldNoLiveClaim instead: a pod stands there, and this rule is simply not the one that
	// decides it.
	IdleHoldNoPod
	// IdleHoldReleasing: a release of this pod is already in progress. It is the idle tick's own
	// word for "my release is already running", never a release that stopped short: a release
	// cancelled at one of its own rows records IdleHoldOvertaken or IdleHoldUnauthorized.
	IdleHoldReleasing
	// IdleHoldBundleUnrestored: a release of this pod saved a bundle of unpushed work whose
	// restore no pod has confirmed. Releasing the pod again would store a bundle of a workspace
	// that never had that work over the one that does. A bundle refused because it is of a
	// repository the issue has left is no such bundle: no pod can ever restore it, so it holds
	// nothing (store's workspaceBundleUnrestored).
	IdleHoldBundleUnrestored
	// IdleHoldBundleTooLarge: the workspace's unpushed work does not fit the bundle a release
	// carries (runtime.MaxWorkspaceBundleBytes), so the pod is kept and the work with it.
	IdleHoldBundleTooLarge
	// IdleHoldBundleSave: the bundle could not be built or could not be saved, so the pod is kept.
	IdleHoldBundleSave
	// IdleHoldOvertaken: the pod was given work after its release was marked — a claim of it was
	// started or resumed, or holds a process, at any of the release's role stops or at its
	// teardown, or, at the first of the stops, before any role process was stopped, a task,
	// notice, message or start reached a role of the pod — so that row cancelled the release
	// and the roles go on in the same pod. It is one hold wherever the release read it, because
	// it is one thing that happened (store.ReleaseCauseOvertaken).
	IdleHoldOvertaken
	// IdleHoldUnauthorized: the store no longer authorized the teardown when it asked — the run
	// the release was taken against has moved on, the issue has left the workflow, its tree
	// lingers or its cleanup is reserved — so the teardown cancelled the release instead of
	// deleting the pod (store.ReleaseCauseUnauthorized).
	IdleHoldUnauthorized
	// IdleHoldNoLiveClaim: the record holds the issue, and every claim of it has failed, been
	// retired or been suspended, so this rule finds no live role to ask for a bundle and no
	// claim to suspend. The pod stands all the same, with nobody in it: that is the state the
	// second rule acts on (UnownedPodRelease), which is why it is a hold of its own rather than
	// IdleHoldNoPod, whose name says there is no pod.
	IdleHoldNoLiveClaim

	// The nine holds below are the second rule's, the release of a pod no claim owns
	// (UnownedPodRelease). Every one of their names carries the unowned_ prefix because a hold
	// record holds the hold's name and nothing else, and a reader of one must be able to tell
	// which of the two rules kept the pod.

	// IdleHoldUnownedClaimLive: a claim of the pod is still live, so the pod is a role's to use
	// and the idle rule's to decide. This rule takes only a pod every claim of which has
	// stopped.
	IdleHoldUnownedClaimLive
	// IdleHoldUnownedNoClaim: the issue holds no claim at all, so there is no pod of its own
	// that any claim of it ever ran in.
	IdleHoldUnownedNoClaim
	// IdleHoldUnownedSandboxGone: the cluster holds no Sandbox for the issue, so the pod this
	// rule would release is already gone and nothing of it is reserved.
	IdleHoldUnownedSandboxGone
	// IdleHoldUnownedClaimProcess: a stopped claim of the pod still records a process in it
	// (pods.IdleClaim.HasProcess), so something the daemon started is still running there and a
	// deletion would take it with the pod.
	IdleHoldUnownedClaimProcess
	// IdleHoldUnownedOperatorSuspended: an operator's own suspension holds a claim of the pod
	// (pods.IdleClaim.OperatorSuspended), the one suspension no daemon rule undoes, and the
	// operator's resume brings that role back into this pod.
	IdleHoldUnownedOperatorSuspended
	// IdleHoldUnownedDelivery: a task, notice, message or start waits for delivery to a role of
	// the pod, and the delivery is what brings a role back into it.
	IdleHoldUnownedDelivery
	// IdleHoldUnownedTreeNotLive: the issue or its tree root has left the workflow, or one of
	// them lingers. Such a pod is a close's to stop, as it is for the idle rule
	// (IdleHoldTreeNotLive).
	IdleHoldUnownedTreeNotLive
	// IdleHoldUnownedReleasing: a release of this pod is already in progress, so this rule's own
	// release is already running on the lane.
	IdleHoldUnownedReleasing
	// IdleHoldUnownedNotIdle: the pod did something inside the quiet period, so the period has
	// not passed and the pod is not this rule's to take yet.
	IdleHoldUnownedNotIdle
)

var idleHoldNames = map[IdleHold]string{
	IdleHoldNone:             "none",
	IdleHoldTreeNotLive:      "tree_not_live",
	IdleHoldAwaitingPerson:   "awaiting_person",
	IdleHoldImage:            "image",
	IdleHoldRepository:       "repository",
	IdleHoldPluginContract:   "plugin_contract",
	IdleHoldUpdateRound:      "update_round",
	IdleHoldInTurn:           "in_turn",
	IdleHoldDelivery:         "delivery_waiting",
	IdleHoldBackgroundWork:   "background_work",
	IdleHoldUnreported:       "unreported",
	IdleHoldReminder:         "reminder_due",
	IdleHoldNotIdle:          "not_idle",
	IdleHoldNoPod:            "no_pod",
	IdleHoldReleasing:        "releasing",
	IdleHoldBundleUnrestored: "bundle_unrestored",
	IdleHoldBundleTooLarge:   "bundle_too_large",
	IdleHoldBundleSave:       "bundle_save_failed",
	IdleHoldOvertaken:        "overtaken",
	IdleHoldUnauthorized:     "unauthorized",
	IdleHoldNoLiveClaim:      "no_live_claim",

	IdleHoldUnownedClaimLive:         "unowned_claim_live",
	IdleHoldUnownedNoClaim:           "unowned_no_claim",
	IdleHoldUnownedSandboxGone:       "unowned_sandbox_gone",
	IdleHoldUnownedClaimProcess:      "unowned_claim_process",
	IdleHoldUnownedOperatorSuspended: "unowned_operator_suspended",
	IdleHoldUnownedDelivery:          "unowned_delivery_waiting",
	IdleHoldUnownedTreeNotLive:       "unowned_tree_not_live",
	IdleHoldUnownedReleasing:         "unowned_releasing",
	IdleHoldUnownedNotIdle:           "unowned_not_idle",
}

// String is the hold's recorded name, which a reader of the daemon's release records reads.
func (h IdleHold) String() string {
	if name, ok := idleHoldNames[h]; ok {
		return name
	}
	return "unknown"
}

// IdleDecision is what one of the two pod-release rules, IdleRelease or UnownedPodRelease,
// decided about one pod: whether it is released, the hold that kept it when it is not, and the
// roles whose live claims a release suspends, in the order the view read them. A pod no claim
// owns has no such role, so UnownedPodRelease names none.
type IdleDecision struct {
	Release bool
	Hold    IdleHold
	Roles   []claim.Role
	// HoldKinds names the hold more closely for the one hold that has more to say:
	// IdleHoldBackgroundWork carries the kinds the held session reported (`subagent`,
	// `async-job`, `claude-background-task`), which is what tells an operator reading the hold
	// whether a pod is kept by a subagent or by a background job. Every other hold leaves it nil.
	HoldKinds []string
}

// IdleRelease decides whether an issue's pod may be released now. It is the whole rule and it is
// pure: the view the store read (pods.IdleView), the clock, the idle period, whether the pod
// runs the image the configuration names, and the plugin contract this daemon serves
// (api.DaemonAPIVersion) are all it reads, so every condition below is one row of a table test.
//
// A pod is released only when all of these hold, and the first that does not is the hold the
// decision reports:
//
//   - the issue is in the workflow and its tree is live: neither the issue nor its root has left
//     the workflow, and neither lingers (pods.InWorkflowNotLingering). A pod in that state is a
//     close's to stop;
//
//   - no bundle of the issue's unpushed work is waiting to be restored. A release saves that
//     work out of the pod it deletes, and the pod that replaces it puts it back; until a
//     restore is confirmed, a second release would take a bundle of a workspace that never had
//     that work and store it over the one that does. A bundle the store refused because it is
//     of a repository the issue has left waits for nothing and holds nothing
//     (runtime.WorkspaceBundleRefusedRepository): every pod of the issue from now on is a clone
//     of another repository, so no pod can ever restore that work and the hold would hold the
//     issue's pod for ever. The work stays in the daemon's database and the daemon writes on
//     the issue where it is (store's workspaceBundleUnrestored);
//
//   - the pod is not a root's that waits on an answer a person has not given. A root whose answer
//     has come waits only for its slot and qualifies, and the children of a root that waits on a
//     person qualify whatever their root is waiting for;
//
//   - the pod runs the image the configuration names, and each live claim speaks this daemon's
//     plugin contract: a released pod comes back as a fresh pod on the configured image, so a pod
//     already behind is a relaunch's to replace, not a release's to save;
//
//   - the pod was built from the repository the issue's record holds now (repositoryDrifted,
//     which the daemon reads of the pod itself against that record). A correction of the
//     repository in Dispatch leaves the pod a clone of the repository the issue has left, and
//     the update round that replaces it opens on a later sweep: a release taken in between
//     would save that pod's unpushed work, which the store then refuses to every pod of the
//     issue, since no clone of the new repository can take commits of the old one
//     (store.TakeWorkspaceBundleForRestore). The pod is replaced rather than released, as a pod
//     on an image this daemon does not serve is;
//
//   - no update round of the pod is open on a live claim of it (sjawhar/legion#2538, read from
//     supervise.Claim.UpdateRound with supervise.IsPodRound, the predicate Claim.InPodRound()
//     is): such a pod is already being replaced, and the replacement waits until every worker in
//     it has pushed its work and answered. A release that deleted the pod under that wait would
//     leave the round to read its own pod as one that went, and to tell the issue that the
//     workers which had not answered lost what they had not pushed. A claim's own plugin round
//     is not this condition's: that claim speaks a contract other than this daemon's, which the
//     condition above already holds the pod for, and such a round keeps the pod and the disk
//     anyway;
//
//   - no claim of the pod has a process in a turn, starting or stopping, or whose launch is not
//     yet known. Failed, retired and already suspended claims stay as they are and hold nothing;
//
//   - no task, notice, message or start waits for delivery to a role of the pod;
//
//   - each live role's session reported, after its last turn, that it runs no background work. A
//     claim with no report, or one whose report predates its last turn, is never released: that
//     is what makes a host that cannot report degrade safely;
//
//   - no live phase worker whose phase is open has a phase reminder due inside the idle period. A
//     phase worker's phase is open when the issue's current phase is that worker's role and its
//     claim serves the current generation;
//
//   - the pod has done nothing the daemon recorded for the whole idle period.
//
// A pod round is a live claim's, as the claim records it, and a claim that left the round keeps
// the round it left (supervise.Claim.UpdateRound): the round is over for every claim of it only
// when the daemon clears it, so a pod is released once the round's own end has run and not
// before.
func IdleRelease(view pods.IdleView, now time.Time, idle time.Duration, onConfiguredImage, repositoryDrifted bool, contract int) IdleDecision {
	if !view.Found {
		return IdleDecision{Hold: IdleHoldNoPod}
	}
	if view.Releasing {
		return IdleDecision{Hold: IdleHoldReleasing}
	}
	if !pods.InWorkflowNotLingering(view.Standing(), view.RootStanding(), record.OutOfWorkflow) {
		return IdleDecision{Hold: IdleHoldTreeNotLive}
	}
	if view.BundleUnrestored {
		return IdleDecision{Hold: IdleHoldBundleUnrestored}
	}
	if view.IsRoot() && view.Root.AwaitingPerson && !view.Root.Answered {
		return IdleDecision{Hold: IdleHoldAwaitingPerson}
	}
	var roles []claim.Role
	for _, c := range view.Claims {
		if !c.Live() {
			continue
		}
		roles = append(roles, c.Role)
	}
	if len(roles) == 0 {
		return IdleDecision{Hold: IdleHoldNoLiveClaim}
	}
	if !onConfiguredImage {
		return IdleDecision{Hold: IdleHoldImage}
	}
	if repositoryDrifted {
		return IdleDecision{Hold: IdleHoldRepository}
	}
	for _, c := range view.Claims {
		if c.Live() && c.PluginContract != contract {
			return IdleDecision{Hold: IdleHoldPluginContract}
		}
	}
	for _, c := range view.Claims {
		if c.Live() && supervise.IsPodRound(c.UpdateRound) {
			return IdleDecision{Hold: IdleHoldUpdateRound}
		}
	}
	for _, c := range view.Claims {
		if c.Live() && !idleStateReleasable(c) {
			return IdleDecision{Hold: IdleHoldInTurn}
		}
	}
	if view.Waiting {
		return IdleDecision{Hold: IdleHoldDelivery}
	}
	for _, c := range view.Claims {
		if !c.Live() {
			continue
		}
		if !c.Reported() {
			return IdleDecision{Hold: IdleHoldUnreported}
		}
		if c.ActivityBackground {
			return IdleDecision{Hold: IdleHoldBackgroundWork, HoldKinds: c.ActivityKinds}
		}
	}
	for _, c := range view.Claims {
		if !c.Live() || !idlePhaseOpen(view, c) {
			continue
		}
		if due := c.ActivityNextReminderAt; !due.IsZero() && !due.After(now) {
			return IdleDecision{Hold: IdleHoldReminder}
		}
	}
	since, ok := view.QuietSince()
	if !ok || now.Sub(since) < idle {
		return IdleDecision{Hold: IdleHoldNotIdle}
	}
	return IdleDecision{Release: true, Roles: roles}
}

// UnownedPodRelease decides whether an issue's pod may be released because no claim of the issue
// can use it. It is the daemon's second pod-release rule, beside IdleRelease, and it exists
// because an issue every claim of which has failed, been retired or been suspended still holds
// its Kubernetes pod, 3 CPU and 19 GiB, with nobody in it: a claim in any of those three states
// runs no process (pods.IdleClaim.Live), so no role is in that pod and none is coming back to it
// on its own. Nothing else takes such a pod back, and it stands for as long as the record holds
// the issue.
//
// It is pure, as IdleRelease is, and it reads less than that rule: no image, no plugin contract,
// no turn and no reminder. Each of those is a fact about a live role — the image a released pod
// would come back on, the contract a live claim speaks, the turn a process is in, the reminder
// armed in a phase worker's session — and this rule acts only where there is no live role to
// have one. The idle rule's bundle hold is not a conjunct here either, because this release
// takes no bundle: it asks the pod for nothing and stores nothing, so an issue whose last
// release saved work no pod has restored is still this rule's to release, with that bundle left
// as it is for the pod a later start makes. Nor is the idle rule's hold for a root waiting on a
// person (IdleHoldAwaitingPerson): that hold keeps the pod of a root whose architect is live and
// idle, so the answer reaches the session it is waiting in, and a root this rule reads has no
// live architect to answer in any pod. The answer promotes the root and the daemon queues the
// architect's start, which builds the Sandbox, the pod and the workspace again.
//
// The pod is released only when all of these hold, and the first that does not is the hold the
// decision reports:
//
//   - the record holds the issue and at least one claim of it. An issue the record does not
//     hold, or one no claim of which ever ran, has no pod of this workflow's to take back
//     (IdleHoldUnownedNoClaim);
//
//   - no claim of the issue is live. A live claim is a role the pod is there for, so such a pod
//     is the idle rule's to decide and this rule leaves it alone (IdleHoldUnownedClaimLive);
//
//   - no release of this Sandbox is already under way, which is this rule's own release already
//     running on the lane (IdleHoldUnownedReleasing);
//
//   - the issue and its tree are the workflow's: neither the issue nor its root has left the
//     workflow, and neither lingers (pods.InWorkflowNotLingering). A pod in that state is a
//     close's to stop, exactly as it is for the idle rule (IdleHoldUnownedTreeNotLive);
//
//   - the runtime holds the issue's Sandbox. Where it does not, the pod this rule would release
//     is already gone and nothing of it is reserved (IdleHoldUnownedSandboxGone);
//
//   - no claim records a process (pods.IdleClaim.HasProcess). A stopped claim that still records
//     a locator is one the teardown's own fence refuses (claimMayRun in internal/store/claims.go,
//     read through issueStopFences), so a release marked for such an issue could never finish:
//     the mark would stand while the teardown waited for a process nothing is going to stop;
//
//   - no claim is an operator's suspension (pods.IdleClaim.OperatorSuspended). It is the one
//     suspension the daemon never ends by itself (supervise.Claim.RuleMayResume), and the
//     operator who suspended the role may want the workspace as its roles left it. Such a pod
//     stands for as long as the operator leaves it standing: they end it by resuming the claim,
//     which starts the role in the pod that is still there, or by stopping it, which retires the
//     claim and leaves the issue to this rule (IdleHoldUnownedOperatorSuspended);
//
//   - nothing waits for delivery to a role of the issue. A start queued for a stopped claim is
//     the one delivery such an issue carries, and it is what brings a role back into this pod; a
//     release taken under it would leave that start to find its pod gone and to provision the
//     workspace afresh (IdleHoldUnownedDelivery);
//
//   - the pod has recorded nothing for the whole idle period, read as the newest write to any
//     claim row of the issue (view.QuietSince, compared as IdleRelease compares it)
//     (IdleHoldUnownedNotIdle).
//
// The quiet conjunct is last, as the idle rule's is, so that a pass records a hold only for a pod
// that has already stood past the period: a pod inside it is simply not this rule's yet, while
// every other hold names something true of a pod that has stood past it.
//
// The decision names no roles. There is no live claim to suspend and no bundle to ask the pod
// for, so the release this answers changes no claim at all: it takes the Sandbox, the pod and
// the disk, and every path that starts a claim of the issue again builds all three afresh.
func UnownedPodRelease(view pods.IdleView, now time.Time, idle time.Duration, sandboxPresent bool) IdleDecision {
	if !view.Found || len(view.Claims) == 0 {
		return IdleDecision{Hold: IdleHoldUnownedNoClaim}
	}
	for _, c := range view.Claims {
		if c.Live() {
			return IdleDecision{Hold: IdleHoldUnownedClaimLive}
		}
	}
	if view.Releasing {
		return IdleDecision{Hold: IdleHoldUnownedReleasing}
	}
	if !pods.InWorkflowNotLingering(view.Standing(), view.RootStanding(), record.OutOfWorkflow) {
		return IdleDecision{Hold: IdleHoldUnownedTreeNotLive}
	}
	if !sandboxPresent {
		return IdleDecision{Hold: IdleHoldUnownedSandboxGone}
	}
	for _, c := range view.Claims {
		if c.HasProcess {
			return IdleDecision{Hold: IdleHoldUnownedClaimProcess}
		}
	}
	for _, c := range view.Claims {
		if c.OperatorSuspended() {
			return IdleDecision{Hold: IdleHoldUnownedOperatorSuspended}
		}
	}
	if view.Waiting {
		return IdleDecision{Hold: IdleHoldUnownedDelivery}
	}
	if since, ok := view.QuietSince(); !ok || now.Sub(since) < idle {
		return IdleDecision{Hold: IdleHoldUnownedNotIdle}
	}
	return IdleDecision{Release: true}
}

// idleStateReleasable is whether a live claim's state is one a release may suspend: an agent that
// finished booting and is waiting for work (ready), or one that finished a turn (idle). Every
// other live state is a process in a turn, one starting, or one whose launch is not yet known.
func idleStateReleasable(c pods.IdleClaim) bool {
	switch c.State {
	case supervise.StateReady, supervise.StateIdle:
		return true
	}
	return false
}

// idlePhaseOpen is whether the claim is the worker of the issue's open phase (OpenPhaseRole),
// serving the current generation: the one shape of claim a phase reminder is armed in.
func idlePhaseOpen(view pods.IdleView, c pods.IdleClaim) bool {
	return OpenPhaseRole(view.Phase) == c.Role && c.ServingGeneration == view.Generation
}

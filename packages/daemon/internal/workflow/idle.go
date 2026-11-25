package workflow

import (
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// IdleHold is why a pod the idle rule read was not released, or IdleHoldNone when it was. The
// daemon records the hold of every pod it keeps past the idle period, so the value is the
// vocabulary of that record and String is how it is written down.
type IdleHold int

const (
	// IdleHoldNone: nothing held the pod and it is released.
	IdleHoldNone IdleHold = iota
	// IdleHoldTreeNotLive: the issue or its tree root has left the workflow, or the root lingers,
	// and a close of the pod is still to come or still running (LeftPodClose). Such a pod is that
	// close's to stop, not a release's.
	IdleHoldTreeNotLive
	// IdleHoldAwaitingPerson: the pod is a root's, and the root waits on an answer a person has
	// not given. Its children's pods are not held by it.
	IdleHoldAwaitingPerson
	// IdleHoldImage: the pod runs an image other than the one the configuration names, or its
	// image could not be read at all.
	IdleHoldImage
	// IdleHoldPluginContract: a live claim of the pod speaks a plugin contract other than the
	// daemon's current one.
	IdleHoldPluginContract
	// IdleHoldUpdateRound: an update round of the pod is open on a live claim of it, so the pod
	// is already being replaced and the replacement is waiting for its workers to do the steps of
	// the update message (supervise.UpdateTask).
	IdleHoldUpdateRound
	// IdleHoldInTurn: a claim of the pod has a process in a turn, starting or stopping, or one
	// whose launch is not yet known.
	IdleHoldInTurn
	// IdleHoldDelivery: a task, notice, message or start waits for delivery to a role of the pod;
	// a start that waits for its pod, built before its role, is none (pods.StartWaitsForPodPrefix).
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
	// IdleHoldNoPod: the record holds no such issue under the daemon's own project, so there is
	// no pod of the workflow's to release at all: a record another project's daemon keeps of the
	// issue is none of this daemon's (store.readIdleView). An issue the record does hold whose
	// claims have all stopped is IdleHoldNoLiveClaim instead: a pod stands there, and this rule
	// is simply not the one that decides it.
	IdleHoldNoPod
	// IdleHoldReleasing: a release of this pod is already in progress. It is the idle tick's own
	// word for "my release is already running", never a release that stopped short: a release
	// cancelled at one of its own rows records IdleHoldOvertaken or IdleHoldUnauthorized.
	IdleHoldReleasing
	// IdleHoldUnpushedWork: the release's check read the issue's workspace and found a commit on a
	// working copy or a local bookmark that no branch or tag on origin holds, or a working-copy
	// change, so the release was declined and the pod stays with the work in it. The daemon pushes
	// nothing for a role: it tells the role once that the release waits for its push, and the pod
	// stays until a role of it starts a turn or a process again or the pod is replaced
	// (pods.IdleDecline.Stands). A read after such a turn that finds the same work tells nobody.
	IdleHoldUnpushedWork
	// IdleHoldWorkspaceUnread: the release's check could not read the issue's workspace, so it
	// could not show that the workspace holds nothing unpushed, and the release was declined. The
	// pod stays until a role of it starts a turn or a process again, the pod is replaced, or
	// pods.DeclineUnreadRetry has passed.
	IdleHoldWorkspaceUnread
	// IdleHoldOvertaken: the pod was given work after its release was marked — a claim of it was
	// started or resumed, or holds a process, at any of the release's role stops or at its
	// teardown, or, at the first of the stops, before any role process was stopped, a task,
	// notice, message or start reached a role of the pod, a start that waits for its pod excepted
	// — so that row cancelled the release
	// and the roles go on in the same pod. It is one hold wherever the release read it, because
	// it is one thing that happened (store.ReleaseCauseOvertaken).
	IdleHoldOvertaken
	// IdleHoldUnauthorized: the store no longer authorized the teardown when it asked — the run
	// the release was taken against has moved on, the issue has left the workflow, its tree
	// lingers or its cleanup is reserved — so the teardown cancelled the release instead of
	// deleting the pod (store.ReleaseCauseUnauthorized).
	IdleHoldUnauthorized
	// IdleHoldNoLiveClaim: the record holds the issue, and every claim of it has failed, been
	// retired or been suspended, so this rule finds no live role to read the workspace through and no
	// claim to suspend. The pod stands all the same, with nobody in it: that is the state the
	// second rule acts on (UnownedPodRelease), which is why it is a hold of its own rather than
	// IdleHoldNoPod, whose name says there is no pod.
	IdleHoldNoLiveClaim

	// The nine holds below are the second rule's, the release of a pod no claim owns
	// (UnownedPodRelease). Every one of their names carries the unowned_ prefix because a hold
	// record holds the hold's name and nothing else, and a reader of one must be able to tell
	// which of the two rules kept the pod. The close of a pod whose issue left the workflow
	// (LeftPodClose) answers two of them, unowned_claim_process and unowned_operator_suspended,
	// for the same two states of a stopped claim, which keep that pod for the same reasons.

	// IdleHoldUnownedClaimLive: a claim of the pod is still live, so the pod is a role's to use
	// and the idle rule's to decide. This rule takes only a pod every claim of which has
	// stopped.
	IdleHoldUnownedClaimLive
	// IdleHoldUnownedNoClaim: the issue holds no claim at all, so there is no pod of its own
	// that any claim of it ever ran in.
	IdleHoldUnownedNoClaim
	// IdleHoldUnownedSandboxGone: the cluster holds no Sandbox for the issue, so the pod this
	// rule would release is already gone and nothing of it is reserved. The hold also means a
	// Sandbox that owns the issue's kept volume, kept Suspended by an earlier release, that runs
	// no pod: its volume waits for the issue's next pod, and there is no pod to release.
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
	// the pod, and the delivery is what brings a role back into it. A start that waits for its pod
	// is none, since the next pod serves it.
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

	// The three holds below are a project's pause's (decision 12 of dispatch://INFRA-3759).
	// IdleHoldPaused is both rules' (IdleRelease, UnownedPodRelease), and the daemon records it
	// too for a release a pause cancelled at its workspace check (releaseHold in internal/daemon);
	// IdleHoldPauseHeld is the idle rule's; and IdleHoldUnownedPauseHeld is the second rule's,
	// whose name carries the unowned_ prefix as the nine above do.

	// IdleHoldPaused: the daemon's project is paused (pods.IdleView.Paused), and a pause starts
	// no release.
	IdleHoldPaused
	// IdleHoldPauseHeld: a pause holds the pod. The pause stopped its roles and kept it, and it
	// stays until a role of it is ready again or something deletes it (decision 13).
	IdleHoldPauseHeld
	// IdleHoldUnownedPauseHeld: a pause holds the pod no claim can use now. Its roles are the
	// pause's to bring back in this pod, so the second rule leaves it to them.
	IdleHoldUnownedPauseHeld

	// IdleHoldUnownedUnpushedWork: the hold of a pod with no live claim whose disk a decline of the
	// issue's idle release says holds work that no branch or tag on origin holds
	// (pods.IdleDecline.HoldsUnpushedWork): the second rule's (UnownedPodRelease), and the close's
	// of a left pod with no live claim (LeftPodClose). No role runs in the pod to read it again, so
	// the decline on record is the one word the daemon has of that disk, and the pod stays with the
	// work until a role of the issue comes back and a read finds it gone, or the pod is replaced.
	IdleHoldUnownedUnpushedWork
)

var idleHoldNames = map[IdleHold]string{
	IdleHoldNone:           "none",
	IdleHoldTreeNotLive:    "tree_not_live",
	IdleHoldAwaitingPerson: "awaiting_person",
	IdleHoldImage:          "image",
	IdleHoldPluginContract: "plugin_contract",
	IdleHoldUpdateRound:    "update_round",
	IdleHoldInTurn:         "in_turn",
	IdleHoldDelivery:       "delivery_waiting",
	IdleHoldBackgroundWork: "background_work",
	IdleHoldUnreported:     "unreported",
	IdleHoldReminder:       "reminder_due",
	IdleHoldNotIdle:        "not_idle",
	IdleHoldNoPod:          "no_pod",
	IdleHoldReleasing:      "releasing",
	// The two decline holds are named by the reasons the decline records (store's
	// idle_release_declines), so the daemon records a decline under its hold's own name.
	IdleHoldUnpushedWork:    pods.DeclineUnpushedWork,
	IdleHoldWorkspaceUnread: pods.DeclineWorkspaceUnread,
	IdleHoldOvertaken:       "overtaken",
	IdleHoldUnauthorized:    "unauthorized",
	IdleHoldNoLiveClaim:     "no_live_claim",

	IdleHoldUnownedClaimLive:         "unowned_claim_live",
	IdleHoldUnownedNoClaim:           "unowned_no_claim",
	IdleHoldUnownedSandboxGone:       "unowned_sandbox_gone",
	IdleHoldUnownedClaimProcess:      "unowned_claim_process",
	IdleHoldUnownedOperatorSuspended: "unowned_operator_suspended",
	IdleHoldUnownedDelivery:          "unowned_delivery_waiting",
	IdleHoldUnownedTreeNotLive:       "unowned_tree_not_live",
	IdleHoldUnownedReleasing:         "unowned_releasing",
	IdleHoldUnownedNotIdle:           "unowned_not_idle",

	IdleHoldPaused:           "paused",
	IdleHoldPauseHeld:        "pause_held",
	IdleHoldUnownedPauseHeld: "unowned_pause_held",

	IdleHoldUnownedUnpushedWork: "unowned_unpushed_work",
}

// String is the hold's recorded name, which a reader of the daemon's release records reads.
func (h IdleHold) String() string {
	if name, ok := idleHoldNames[h]; ok {
		return name
	}
	return "unknown"
}

// IdleDecision is what one of the pod rules, IdleRelease, UnownedPodRelease or LeftPodClose,
// decided about one pod: whether it is released, whether it is closed, the hold that kept it when
// it is neither, and the live claims a release suspends or a close stops, each by its token beside
// its stored role (store.ReleasedClaim), in the order the view read them, which is token order
// (store's readIdleView). Two claims can hold one role, so each stop names its claim. A pod no
// claim owns has no such claim, so UnownedPodRelease names none.
type IdleDecision struct {
	Release bool
	// Close finishes the close of an issue the workflow took out: each live claim in Claims is
	// suspended for the daemon, and the issue's Sandbox goes with its pod and disk
	// (record.IssueSuspend). It is never set with Release.
	Close  bool
	Hold   IdleHold
	Claims []store.ReleasedClaim
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
//     close's to stop, and LeftPodClose decides it: it is held while that close is still to come
//     or still running (IdleHoldTreeNotLive), and closed where nothing else will close it;
//
//   - the daemon's project is not paused (IdleHoldPaused), and no pause holds the pod
//     (IdleHoldPauseHeld). A pause keeps every pod of its project and starts no release, and a
//     pod it held stays after the resume until a role of it is ready again (decision 12 of
//     dispatch://INFRA-3759);
//
//   - the pod is not a root's that waits on an answer a person has not given. A root whose answer
//     has come waits only for the pass that resumes it on its slot and qualifies, and the children of a root that waits on a
//     person qualify whatever their root is waiting for;
//
//   - the pod runs the image the configuration names, and each live claim speaks this daemon's
//     plugin contract: a released pod comes back as a fresh pod on the configured image, so a pod
//     already behind is a relaunch's to replace, not a release's to save;
//
//   - no update round of the pod is open on a live claim of it (sjawhar/legion#2538, read from
//     supervise.Claim.UpdateRound with supervise.IsPodRound, the predicate Claim.InPodRound()
//     is): such a pod is already being replaced, and the replacement waits until every role in
//     it has answered the update message, no turn runs and no claim's activity report holds it
//     (internal/daemon's roundEnd.holding), or until the round's bound. A release that deleted the
//     pod under that wait would leave the round to read its own pod as one that went, and to
//     tell the issue that the workers which had not answered lost what they had not pushed. A
//     claim's own plugin round is not this condition's: that claim speaks a contract other than
//     this daemon's, which the condition above already holds the pod for, and such a round keeps
//     the pod and the disk anyway;
//
//   - no claim of the pod has a process in a turn, starting or stopping, or whose launch is not
//     yet known. Failed, retired and already suspended claims stay as they are and hold nothing;
//
//   - no task, notice, message or start waits for delivery to a role of the pod, but a start
//     queued for news alone, which wakes the claim a release suspends, and a start that waits for
//     its pod (pods.IdleView.Waiting);
//
//   - each live role's session reported, after its last turn, that it runs no background work. A
//     claim with no report, or one whose report predates its last turn, is never released: that
//     is what makes a host that cannot report degrade safely;
//
//   - no live phase worker whose phase is open has a phase reminder due inside the idle period. A
//     phase worker's phase is open when the issue's current phase is that worker's role and its
//     claim serves the current generation;
//
//   - the pod has done nothing the daemon recorded for the whole idle period;
//
//   - no decline of the issue's release stands (pods.IdleDecline.Stands). A release's check reads
//     the workspace before any role stops, and declines where it finds work that no branch or tag
//     on origin holds, or cannot read it (IdleHoldUnpushedWork, IdleHoldWorkspaceUnread). The
//     decline holds the pod, with no new mark and no new read, until a role of it starts a turn
//     or a process again or the runtime holds another pod for the issue, so a pod that keeps its
//     work is asked once and not at every pass; the read after a turn that finds the same work
//     keeps the decline and tells nobody, so the turn the decline's message starts is followed by
//     one read and no second message. It comes last, so a pod whose period has not passed reads
//     not_idle rather than the decline.
//
// A pod round is a live claim's, as the claim records it, and a claim that left the round keeps
// the round it left (supervise.Claim.UpdateRound): the round is over for every claim of it only
// when the daemon clears it, so a pod is released once the round's own end has run and not
// before.
func IdleRelease(view pods.IdleView, now time.Time, idle time.Duration, onConfiguredImage bool, contract int) IdleDecision {
	if !view.Found {
		return IdleDecision{Hold: IdleHoldNoPod}
	}
	if view.Releasing {
		return IdleDecision{Hold: IdleHoldReleasing}
	}
	if !pods.InWorkflowNotLingering(view.Standing(), view.RootStanding(), record.OutOfWorkflow) {
		return LeftPodClose(view, now, idle)
	}
	if view.Paused {
		return IdleDecision{Hold: IdleHoldPaused}
	}
	if view.PauseHeld {
		return IdleDecision{Hold: IdleHoldPauseHeld}
	}
	if view.IsRoot() && view.Root.AwaitingPerson && !view.Root.Answered {
		return IdleDecision{Hold: IdleHoldAwaitingPerson}
	}
	var claims []store.ReleasedClaim
	for _, c := range view.Claims {
		if !c.Live() {
			continue
		}
		claims = append(claims, store.ReleasedClaim{Token: c.Token, Role: c.Role})
	}
	if len(claims) == 0 {
		return IdleDecision{Hold: IdleHoldNoLiveClaim}
	}
	if !onConfiguredImage {
		return IdleDecision{Hold: IdleHoldImage}
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
		switch claim.ReadSessionWork(c.ActivityAt, c.LastTurnAt, c.ActivityBackground) {
		case claim.WorkUnreported:
			return IdleDecision{Hold: IdleHoldUnreported}
		case claim.WorkRunning:
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
	if view.Declined.Stands(view, now) {
		if view.Declined.Reason == pods.DeclineWorkspaceUnread {
			return IdleDecision{Hold: IdleHoldWorkspaceUnread}
		}
		return IdleDecision{Hold: IdleHoldUnpushedWork}
	}
	return IdleDecision{Release: true, Claims: claims}
}

// LeftPodClose decides the pod of an issue the workflow has taken out: one whose issue or tree
// root has left the workflow, or lingers (pods.InWorkflowNotLingering is false). IdleRelease hands
// it every such pod that has a live claim, and the daemon's close pass every such pod whose claims
// have all stopped (store.LeftPods). It is pure, as the two release rules are.
//
// Such a pod is a close's to stop, and in every shape but one that close is still to come or
// still running, so the rule keeps the pod for it (IdleHoldTreeNotLive). A tree that lingers is
// closed when its linger ends, by the linger's close of every member and the cleanup that takes
// their pods. A root out of the workflow that does not linger has had its tree's close already,
// or is a root the operator runs a tree under, whose claims and pod are the operator's
// (leaveUnderOperator).
//
// The one shape no close reaches again is a child that left the workflow while its tree runs on
// (leftUnderRunningTree): parked in backlog, icebox or triage, or closed as done, under a root
// still in the workflow. Its departure was its one close (leave): it suspended or closed each of
// its claims and deleted its Sandbox with the pod and the disk (record.IssueSuspend), and nothing
// of its running tree closes it again. A claim that runs there after that close runs in a pod
// built again for it, because an operator's resume, an operator's task to a suspended claim, an
// operator's retry and the controller's fresh session each start a claim without reading its
// issue's standing (the api's resumeEvent, deliverEvent, machineRetry and freshSession). Once its
// turns end nothing stops it. Held as IdleHoldTreeNotLive, that pod stood for as long as its
// issue did, and every pass recorded a close that was not coming.
//
// So that pod is decided by what still works in it, as an idle pod is, and where nothing does the
// rule finishes the close the departure made (IdleDecision.Close): each live claim is suspended
// for the daemon with its session kept, as the departure suspended it, and the issue's Sandbox
// goes with its pod and its disk. The pod is kept, and the first of these that holds is the hold
// reported, while:
//
//   - a role stop of the issue is still queued (pods.IdleView.RoleStopQueued): a close or a
//     release is stopping its roles now (IdleHoldTreeNotLive);
//   - a look of the issue's production check is open (pods.IdleView.CheckLookOpen): the look runs
//     in the checking role's session, and its end is a close of its own, which stops that role
//     and deletes the Sandbox (the workflow's endLookPending, closeCheck) (IdleHoldTreeNotLive);
//   - no claim is live and the issue's Sandbox suspension is still queued
//     (pods.IdleView.IssueSuspendQueued): the close is taking the pod now (IdleHoldTreeNotLive).
//     Beside a live claim a queued suspension is no close under way: it waits for that claim to
//     stop (store.IssueSuspension), and nothing but this rule will stop it;
//   - the project is paused, or a pause holds the pod (IdleHoldPaused, IdleHoldPauseHeld);
//   - a live claim is in a turn, starting or stopping (IdleHoldInTurn);
//   - a stopped claim still records a process, which the suspension would wait on for as long as
//     it stands (IdleHoldUnownedClaimProcess), or an operator's suspension holds a claim, which
//     keeps the pod for that operator as UnownedPodRelease keeps it
//     (IdleHoldUnownedOperatorSuspended);
//   - a task, notice, message or start waits for delivery to a role of the pod (IdleHoldDelivery);
//   - a live role's session has reported nothing since its last turn, or reported background
//     work of its own running (IdleHoldUnreported, IdleHoldBackgroundWork);
//   - the pod did something inside the idle period (IdleHoldNotIdle);
//   - a live claim stands and a decline of the issue's idle release stands (pods.IdleDecline.Stands):
//     the last read of the workspace found work that origin does not hold, or could not read it
//     (IdleHoldUnpushedWork, IdleHoldWorkspaceUnread);
//   - no claim is live and a decline says the disk of this pod holds work that origin does not hold
//     (pods.IdleDecline.HoldsUnpushedWork): a role read through after an operator's resume left
//     that work, and its claim then stopped. No role runs to read the disk again, so the pod is
//     kept as the release of a pod no claim owns keeps such a pod (IdleHoldUnownedUnpushedWork).
//
// A close with a live claim deletes a disk that a role worked on, so it reads that disk first, as
// the idle release does: the decision names the live claims, and the daemon queues the workspace
// check alone (record.IssueWorkspaceCheck, CloseRoles), which reads through one of them and queues
// the stops and the suspension only where it finds nothing that origin does not hold
// (LeftPodCloseAfterCheck). A read that finds work, or that fails, records a decline instead, and
// the pod stays with its work. While that check waits on the lane, the pod is held as this
// daemon's own act under way (IdleHoldReleasing, pods.IdleView.WorkspaceCheckQueued), which no
// record counts. A pod with no live claim runs no role to read through: it is closed where no
// decline says its disk holds work, and kept where one does.
//
// It reads nothing of the image, the plugin contract, an update round or a person's wait: each
// of those is about the pod a release brings back, and this close brings no pod back. The issue's
// next run, a todo that re-enters it, builds its pod afresh.
func LeftPodClose(view pods.IdleView, now time.Time, idle time.Duration) IdleDecision {
	if view.Found && view.WorkspaceCheckQueued {
		return IdleDecision{Hold: IdleHoldReleasing}
	}
	decision := leftPodStops(view)
	if !decision.Close {
		return decision
	}
	if since, ok := view.QuietSince(); !ok || now.Sub(since) < idle {
		return IdleDecision{Hold: IdleHoldNotIdle}
	}
	if len(decision.Claims) > 0 && view.Declined.Stands(view, now) {
		if view.Declined.Reason == pods.DeclineWorkspaceUnread {
			return IdleDecision{Hold: IdleHoldWorkspaceUnread}
		}
		return IdleDecision{Hold: IdleHoldUnpushedWork}
	}
	if len(decision.Claims) == 0 && view.Declined.HoldsUnpushedWork(view) {
		return IdleDecision{Hold: IdleHoldUnownedUnpushedWork}
	}
	return decision
}

// LeftPodCloseAfterCheck decides, once the close's workspace check has read the workspace and found
// nothing that origin does not hold, whether the close still stops the pod's roles and suspends its
// Sandbox (internal/daemon's checkLeftPodWorkspace). turnStart is the newest turn start of the
// issue's claims in the view the pass queued the check from (record.IssueWorkspaceCheck.TurnStart,
// pods.IdleView.NewestTurnStart). It reads every conjunct of LeftPodClose but three: the check is
// the one queued, the quiet period was read when the check was queued, and the read it made is
// newer than any decline on record. A claim of the issue that started a turn after turnStart may
// have made work the read did not see: a turn that starts before the read and ends while it runs
// commits after the read's snapshot, and the read's own moment does not show it. So the close waits
// for the next pass and its next read (IdleHoldNotIdle), as a release's role stop is cancelled by a
// turn after its mark. A turn that starts after this decision holds the close's stop of its role
// instead (record.SuperviseRequest.TurnStart).
func LeftPodCloseAfterCheck(view pods.IdleView, turnStart time.Time) IdleDecision {
	decision := leftPodStops(view)
	if !decision.Close {
		return decision
	}
	if view.NewestTurnStart().After(turnStart) {
		return IdleDecision{Hold: IdleHoldNotIdle}
	}
	return decision
}

// leftPodStops is LeftPodClose's conjuncts on what works in the pod, in its order, and the live
// roles of a close none of them holds.
func leftPodStops(view pods.IdleView) IdleDecision {
	if !view.Found {
		return IdleDecision{Hold: IdleHoldNoPod}
	}
	if view.Releasing {
		return IdleDecision{Hold: IdleHoldReleasing}
	}
	if !leftUnderRunningTree(view) || view.RoleStopQueued || view.CheckLookOpen {
		return IdleDecision{Hold: IdleHoldTreeNotLive}
	}
	var claims []store.ReleasedClaim
	for _, c := range view.Claims {
		if c.Live() {
			claims = append(claims, store.ReleasedClaim{Token: c.Token, Role: c.Role})
		}
	}
	if len(claims) == 0 && view.IssueSuspendQueued {
		return IdleDecision{Hold: IdleHoldTreeNotLive}
	}
	if view.Paused {
		return IdleDecision{Hold: IdleHoldPaused}
	}
	if view.PauseHeld {
		return IdleDecision{Hold: IdleHoldPauseHeld}
	}
	for _, c := range view.Claims {
		if c.Live() && !idleStateReleasable(c) {
			return IdleDecision{Hold: IdleHoldInTurn}
		}
	}
	for _, c := range view.Claims {
		if !c.Live() && c.HasProcess {
			return IdleDecision{Hold: IdleHoldUnownedClaimProcess}
		}
	}
	for _, c := range view.Claims {
		if c.OperatorSuspended() {
			return IdleDecision{Hold: IdleHoldUnownedOperatorSuspended}
		}
	}
	if view.Waiting {
		return IdleDecision{Hold: IdleHoldDelivery}
	}
	for _, c := range view.Claims {
		if !c.Live() {
			continue
		}
		switch claim.ReadSessionWork(c.ActivityAt, c.LastTurnAt, c.ActivityBackground) {
		case claim.WorkUnreported:
			return IdleDecision{Hold: IdleHoldUnreported}
		case claim.WorkRunning:
			return IdleDecision{Hold: IdleHoldBackgroundWork, HoldKinds: c.ActivityKinds}
		}
	}
	return IdleDecision{Close: true, Claims: claims}
}

// leftUnderRunningTree is whether the pod's issue is a child that left the workflow while its tree
// runs on: its phase done and its status one that takes it out of the workflow, neither it nor its
// root lingering, and its root in the workflow. It is the shape in which store.IssueSuspension
// authorizes a child's close, so the suspension LeftPodClose decides is one that acts.
func leftUnderRunningTree(view pods.IdleView) bool {
	return !view.IsRoot() && view.Phase == phase.Done && record.OutOfWorkflow(view.Status) &&
		view.LingerUntil == nil && view.Root.LingerUntil == nil &&
		view.RootStanding().InWorkflow(record.OutOfWorkflow)
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
// have one. This release makes no check of its own: the workspace read is made by a role's process
// (runtime.WorkspaceWorkReader), and a pod this rule reads runs none. It reads the idle rule's
// decline instead, where that decline says the pod's disk holds work (below). Nor is the idle
// rule's hold for a root waiting on a
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
//   - no pause holds the pod (pods.IdleView.PauseHeld). A pause stopped its roles and kept it,
//     and it stays until a role of it is ready again or something deletes it
//     (IdleHoldUnownedPauseHeld);
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
//     (IdleHoldUnownedNotIdle);
//
//   - no decline of the issue's idle release says the disk of this pod holds work that no branch
//     or tag on origin holds (pods.IdleDecline.HoldsUnpushedWork): the last read that could read
//     the workspace found such work, and the pod is the one that read was made in. A failed read
//     after it keeps that work. Where the decline and the runtime both name the pod, only another
//     pod ends it, and a start row into the same pod does not; only where either uid is unknown
//     does a start row after the decline end it. A role whose claims all stopped after such a
//     decline — relaunches that ran out, say — left that work on this disk, and the release would
//     delete it (IdleHoldUnownedUnpushedWork);
//
//   - the daemon's project is not paused (pods.IdleView.Paused). The release's own transaction
//     marks no release while the project is paused (store.Store.ReleaseIdleIssue, decision 12 of
//     dispatch://INFRA-3759), so a pod this rule would otherwise release is kept, and the hold
//     says what kept it (IdleHoldPaused).
//
// The quiet conjunct comes before the last two, where the idle rule's comes last but for its
// decline, so that a pass records a hold only for a pod that has already stood past the period: a
// pod inside it is simply not this rule's yet, while every other hold names something true of a
// pod that has stood past it. The pause's comes last, since a pause keeps only a pod this rule
// would release.
//
// The decision names no roles. There is no live claim to suspend and no workspace to read through
// one, so the release this answers changes no claim at all: it takes the Sandbox, the pod and
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
	if view.PauseHeld {
		return IdleDecision{Hold: IdleHoldUnownedPauseHeld}
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
	if view.Declined.HoldsUnpushedWork(view) {
		return IdleDecision{Hold: IdleHoldUnownedUnpushedWork}
	}
	if view.Paused {
		return IdleDecision{Hold: IdleHoldPaused}
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

// idlePhaseOpen is whether the claim is the worker of the issue's open phase (OpenRoleAt, which at
// step is the role of the running step the view reads with the issue, pods.IdleView.StepRole),
// serving the current generation: the one shape of claim a phase reminder is armed in. Two claims
// of one issue can hold that role, and only the claim the record gives the role's step works
// the phase (record.WorkerClaim over the view's phases rows, record.IdlePhaseRows): the claim the
// row names while it runs a step, and the role's own token otherwise. A reminder due on the other
// holds no pod.
func idlePhaseOpen(view pods.IdleView, c pods.IdleClaim) bool {
	role := OpenRoleOf(record.Issue{Phase: view.Phase, StepRole: view.StepRole})
	if role != c.Role || c.ServingGeneration != view.Generation {
		return false
	}
	worker, err := record.WorkerClaim(view.Project, view.Issue, role, record.IdlePhaseRows(view))
	return err == nil && worker == c.Token
}

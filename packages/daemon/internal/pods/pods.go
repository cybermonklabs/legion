// Package pods holds what the daemon's two sides of an idle issue pod both name: the view of one
// pod the idle rule decides over, which the store fills and the workflow reads, and the
// vocabulary of a wake. It is a package of its own because those two sides are the store and the
// workflow, and neither may import the other.
package pods

import (
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// Wake kinds, the vocabulary of a recorded resume's kind: the six ways a released pod comes
// back. WakeDelivery is a delivery the daemon already had for a role of the
// pod — a phase start, a task, a backward move, a notice, a phase-ended message, a merge forward's
// message, a production check's look, a slot resume; WakeChecks is a CI settlement on the head of
// the issue's pull request; WakeRoleMessage and WakeAgentMessage are what only the Envoy listener
// sees, a message published to a role topic of the pod and one sent to the agent subject of a
// released session; WakeBackstop is the stored backstop a release armed; WakePause is the launch
// a project's pause owed a role it stopped, held until the resume (store's PodRecords.TakePodWake).
//
// They are records rather than the rule's own, because the rule that names a wake (workflow) and
// the store that files it under this word are different packages, and one vocabulary is what
// keeps a wake's resume row and the task it writes saying the same thing.
const (
	WakeDelivery     = "delivery"
	WakeChecks       = "checks"
	WakeRoleMessage  = "role_message"
	WakeAgentMessage = "agent_message"
	WakeBackstop     = "backstop"
	WakePause        = "pause"
)

// IdleClaim is one claim of an issue's pod as the idle rule reads it. Every field comes from the
// claims table and none from a live machine: the activity columns are written by the session's
// activity route alone and are deliberately absent from the claim insert's conflict update, so a
// machine's in-memory claim shows their zero values until the claim is read again.
type IdleClaim struct {
	// Token names the claim, which a decision about one claim of the pod is logged under
	// (workflow.IdleDecision.OldPlugin).
	Token          claim.Token
	Role           claim.Role
	State          supervise.ClaimState
	PluginContract int
	// HasProcess is whether the claim's row still records a locator, the process its role was
	// last launched as. It is read beside the state because the two can disagree: a claim the
	// supervisor has already marked failed, retired or suspended whose locator is still there is
	// one the teardown's own fence refuses (claimMayRun in internal/store/claims.go, read through
	// issueStopFences in internal/store/issue_suspend.go, which waits rather than acts while any
	// claim of the issue may still hold a process). A rule that could not see it would mark a
	// release whose teardown waits for a process nothing is going to stop, so the pod would stay
	// and the mark would stand.
	HasProcess bool
	// SuspendedBy is the source recorded on the claim's suspension (supervise.Claim.SuspendedBy),
	// "" for a claim that is not suspended: who stopped this role — the operator, the project's
	// controller, the workflow's own close, an idle release. The rule reads it to tell the
	// suspensions no daemon rule may undo, the operator's and the controller's, from every other
	// (IdleClaim.SuspensionHolds).
	SuspendedBy supervise.SuspendSource
	// ServingGeneration is the run the claim is serving, which tells a phase worker of the
	// issue's current run from one left over by an older one.
	ServingGeneration uint64
	// LastStartRow is the newest start row the outbox ran against the claim. The release records
	// the highest of the pod's as its mark's started_row, which is what tells a start queued
	// before the boundary and already run from one of the release's own rows.
	LastStartRow int64
	// LastTurnAt is the newest moment a turn of this claim is known to have been under way: the
	// confirmation of its pending delivery, the moment the daemon recorded its task as taken, zero
	// for a claim holding no delivery and for one whose delivery is not confirmed. A session's
	// report counts only when it was read at or after it (claim.ReadSessionWork).
	LastTurnAt time.Time
	// LastTurnStart is the newest turn the claim's agent started, as the daemon recorded it when
	// the turn's start moved the claim to working (claims.last_turn_start, supervise.Claim's
	// LastTurnStart), and zero for a claim that recorded none. A turn is what can change the
	// workspace, so a turn that started after the idle release declined the pod ends that decline
	// (IdleDecline.Stands).
	LastTurnStart time.Time
	// ActivityAt, ActivityBackground, ActivityKinds and ActivityNextReminderAt are the last
	// activity report of the process the claim runs now (POST /legion/v1/claims/activity): when
	// it read what it was doing, whether it found background work of its own running, what kinds
	// it named, and when the phase reminder armed in it is due. ActivityAt is zero for a session
	// that has reported none, and for a claim whose only report an earlier process of it made:
	// the launch that replaced that process cleared the four columns (store's putClaim clears
	// them on every write that moves the claim's generation). So the release holds no pod on
	// background work a pod replacement killed, or on a reminder that died with the process
	// that armed it.
	ActivityAt             time.Time
	ActivityBackground     bool
	ActivityKinds          []string
	ActivityNextReminderAt time.Time
	// UpdateRound is the update round the claim is in, "" when none is open: the pod round every
	// claim of a drifted pod shares, or the claim's own plugin round (supervise.Claim.UpdateRound,
	// read with supervise.IsPodRound as Claim.InPodRound() reads it).
	UpdateRound string
	// UpdatedAt is when the claim's row was last written: every state the machine persisted and
	// every activity report. The newest UpdatedAt of a pod's claims is when the pod last did
	// anything the daemon recorded, which is what the idle period is measured from.
	UpdatedAt time.Time
}

// Live is whether the claim still holds its place in the pod: a failed, retired or already
// suspended claim stays as it is and holds no pod.
func (c IdleClaim) Live() bool {
	switch c.State {
	case supervise.StateFailed, supervise.StateRetired, supervise.StateSuspended:
		return false
	}
	return true
}

// SuspensionHolds is whether a suspension that holds is over this claim's agent: the operator's
// own or the project's controller's, the suspensions no daemon rule undoes, which only a resume
// ends (supervise.SuspendSource.Holds). The hold it decides keeps the name it had before the
// controller's suspension, which holds the pod for the same reason
// (workflow.IdleHoldUnownedSuspensionHolds), since release records carry that hold's key.
//
// The expression is written here rather than called on supervise.Claim because there is no such
// method on supervise.Claim to call: Claim.RuleHolds and Claim.RuleMayResume are where it lives
// today, and neither takes the shape this view needs. RuleHolds's other arm — such a suspension
// held for its agent's turn — is about a claim that is still live, and a live claim
// holds the pod through the rule's own live-claim conjunct, so repeating it here would say
// nothing the rule does not already read. The view carries the two columns and not a
// supervise.Claim (IdleClaim), so the predicate is the view's.
func (c IdleClaim) SuspensionHolds() bool {
	return c.State == supervise.StateSuspended && c.SuspendedBy.Holds()
}

// IdleRoot is the tree root of the issue the idle rule is deciding, read with it.
type IdleRoot struct {
	Phase       phase.Phase
	Status      string
	Generation  uint64
	LingerUntil *time.Time
	// AwaitingPerson is whether the root waits on a person (phase awaiting_person) and Answered
	// whether that person has answered, which leaves the root waiting only for the admission pass
	// that resumes it on the slot it holds.
	AwaitingPerson bool
	Answered       bool
}

// IdleView is one issue's pod as the idle rule reads it, in one read per issue: every claim of
// the pod, whether any queued outbox row is addressed to one of its roles, the issue's own place
// in the workflow and its tree root's. Found is false for an issue the record does not hold under
// the daemon's own project, which is no pod of the workflow's to release: a record another
// project's daemon keeps of the issue on a shared database is not this daemon's
// (store.readIdleView).
//
// It is a record type rather than a store type because the rule that reads it is the workflow's
// (workflow.IdleRelease) and the store is what fills it, so neither of the two imports the other
// for it.
type IdleView struct {
	Issue       string
	Tree        string
	Found       bool
	Phase       phase.Phase
	Status      string
	Generation  uint64
	LingerUntil *time.Time
	Root        IdleRoot
	Claims      []IdleClaim
	// Waiting is whether a task, notice, message or start of the issue waits for delivery to a
	// role of its pod. A start queued for news of the approved design alone is not counted, and
	// neither is a worker start that waits for an admission slot its issue does not hold: each
	// wakes the claim a release suspends when it runs. Nor is a hook prompt or a phase-ended
	// message to a role with no claim this pod answers: such a role comes back only by a start or
	// a resume, the operator's or the controller's, which brings a pod of its own (store's
	// idleWaitingRow). The rule reads only whether there is one, so the store reads only that.
	Waiting bool
	// RoleStopQueued is whether a supervise row that stops a role of the issue still waits in
	// the outbox: a close's suspend, issue_close or tree_close, or an idle release's role stop.
	// IssueSuspendQueued is whether the issue's issue_suspend row does, the act of a close that
	// deletes its Sandbox, its pod and its disk. CheckLookOpen is whether a look of the issue's
	// production check is open: its bound row (record.CheckLook) still waits, and the look's end
	// stops the checking role and deletes the Sandbox. Only the rule for the pod of an issue the
	// workflow has taken out reads the three (workflow.LeftPodClose): each says a close of that
	// pod is still under way or still to come, so the rule leaves the pod to it rather than
	// stopping it a second time.
	RoleStopQueued     bool
	IssueSuspendQueued bool
	CheckLookOpen      bool
	// Releasing is whether a release of this pod is already marked in progress, which no second
	// release starts behind.
	Releasing bool
	// WorkspaceCheckQueued is whether a workspace check of the issue still waits in the outbox
	// (record.IssueWorkspaceCheck): an idle release's first act, or the read the close of a pod
	// whose issue left the workflow makes before it stops any live role. The close's rule reads it
	// (workflow.LeftPodClose), so that it queues no second read while the first is on the lane.
	WorkspaceCheckQueued bool
	// Declined is the idle release the daemon last declined for this issue, and the zero
	// IdleDecline where none stands: the release's check read the workspace and found work that no
	// branch or tag on origin holds on a working copy or a local bookmark, or could not read it
	// (store's idle_release_declines). The rules keep the pod under that reason while the decline
	// stands (IdleDecline.Stands, HoldsUnpushedWork), so a pass neither marks a release nor asks
	// the pod again while nothing it reads has changed.
	Declined IdleDecline
	// PodUID is the uid of the pod the runtime holds for the issue now, and "" where the runtime
	// cannot say. The store does not hold it: the daemon's pass reads it from the runtime
	// (runtime.IssuePodReader) and sets it on the view before a rule reads the view, so that a
	// decline taken in another pod ends (IdleDecline.Stands): that pod's disk, and the work the
	// decline names, went with it.
	PodUID string
	// KeptVolume is whether the issue's Sandbox keeps a volume the issue's next pod mounts
	// (runtime.IssueVolumeReader): a release of a pod no claim owns keeps that Sandbox and its
	// volume, so it deletes no disk, reads none, and is held by no decline whose read failed
	// (workflow.UnownedPodRelease). The store does not hold it either: the daemon's pass reads it
	// from the runtime and sets it on the view, and only where the rule's decision without it turns
	// on it (workflow.IdleDecision.TurnsOnKeptVolume). The close of a left pod never sets it, since
	// a close deletes the Sandbox with any volume it holds.
	KeptVolume bool
	// Paused is whether the daemon's project is paused (a project_pauses row of that project
	// stands open), read by the project's token whatever project the issue's key names (store's
	// readOpenPauseByToken says why). A paused project's pods stay: the pause starts no release
	// (decision 12 of dispatch://INFRA-3759).
	Paused bool
	// PauseHeld is whether a pause's hold stands on the pod (project_pause_pod_holds): a pod whose
	// roles a pause stopped, which stays until a role of it is ready again or something deletes it
	// (decision 13).
	PauseHeld bool
	// Holding is whether a hold streak of this pod is running (store's pod_holds). The rule
	// never reads it: it is here because the daemon's pass needs it to know whether it has a
	// streak to end, and the view is the one read of the pod that pass already makes.
	Holding bool
}

// IsRoot is whether the issue is its tree's root, the one issue a root's own wait on a person
// holds: the children of a root that waits on a person are released like any other issue.
func (v IdleView) IsRoot() bool { return v.Issue == v.Tree }

// Standing is where one issue stands in the three plain fields the pod gate reads: its phase,
// its Dispatch status and whether it lingers after a close.
type Standing struct {
	Phase   phase.Phase
	Status  string
	Lingers bool
}

// Standing is the issue's own standing, and RootStanding its tree root's.
func (v IdleView) Standing() Standing {
	return Standing{Phase: v.Phase, Status: v.Status, Lingers: v.LingerUntil != nil}
}

func (v IdleView) RootStanding() Standing {
	return Standing{Phase: v.Root.Phase, Status: v.Root.Status, Lingers: v.Root.LingerUntil != nil}
}

// InWorkflow is whether one issue's status and phase alone are still the workflow's: it carries
// no status that takes an issue out of the workflow (outOfWorkflow, which is
// record.OutOfWorkflow — this package sits below record, so the rule is passed in rather than
// restated) and its phase is short of done. A linger is not read here: admission and
// awaiting-person add it beside this call themselves, the production check and the tree-close
// wait leave it out, and the pod gate adds it in InWorkflowNotLingering.
//
// It is the one statement of that rule in the daemon: workflow.InWorkflow is this call, which
// is what admission, the linger, the production check and awaiting-person read, and
// InWorkflowNotLingering is this call and the linger, which is what every path that acts on an
// idle or released pod reads. A new terminal phase added here moves all of them together, where
// two copies of the rule would have let the release gate and admission disagree about one issue.
func (s Standing) InWorkflow(outOfWorkflow func(status string) bool) bool {
	return !outOfWorkflow(s.Status) && s.Phase != phase.Done
}

// InWorkflowNotLingering is whether an issue and its tree root are both still the workflow's to
// run: neither is out of the workflow by status or phase (InWorkflow), and neither lingers after
// its close.
//
// It is the one gate every path that acts on an idle or released pod reads — the idle rule, a
// wake applied inside a fact, a wake the outbox takes for a delivery it holds, and the store's
// own authorization of a release's teardown — because a pod whose tree has stopped being the
// workflow's is a close's to stop, never a release's to take or a wake's to bring back. The store
// is one of the four and sits below workflow, so the rule lives here, where both reach it.
func InWorkflowNotLingering(issue, root Standing, outOfWorkflow func(status string) bool) bool {
	return issue.live(outOfWorkflow) && root.live(outOfWorkflow)
}

// live is one issue's half of InWorkflowNotLingering.
func (s Standing) live(outOfWorkflow func(status string) bool) bool {
	return s.InWorkflow(outOfWorkflow) && !s.Lingers
}

// QuietSince is when anything of the pod was last recorded — the newest claim write of any of its
// claims, state and activity report alike — and false while the pod has no claim at all.
func (v IdleView) QuietSince() (time.Time, bool) {
	var newest time.Time
	for _, c := range v.Claims {
		if c.UpdatedAt.After(newest) {
			newest = c.UpdatedAt
		}
	}
	return newest, !newest.IsZero()
}

// NewestTurnStart is the newest turn start of the pod's claims (IdleClaim.LastTurnStart), zero
// where none has had a turn. It is the one moment the idle rules weigh a later turn against: a
// release's mark records it (store's issue_releases.turn_start), and a left pod's close carries it
// in its workspace check and its stops (record.IssueWorkspaceCheck.TurnStart). A claim whose turn
// started after it is the pod given work since, which cancels the release and holds the close.
func (v IdleView) NewestTurnStart() time.Time {
	var newest time.Time
	for _, c := range v.Claims {
		if c.LastTurnStart.After(newest) {
			newest = c.LastTurnStart
		}
	}
	return newest
}

// The two reasons the idle release's check declines a release (store's idle_release_declines,
// internal/daemon's checkIssueWorkspace): the workspace holds work that no branch or tag on origin
// holds, or the check could not read the workspace and so cannot say that it holds none.
const (
	DeclineUnpushedWork    = "unpushed_work"
	DeclineWorkspaceUnread = "workspace_unread"
)

// DeclineUnreadRetry is how long a decline whose check could not read the workspace stands with
// no turn and no start of a role. A failed read can have a cause that clears by itself (a role's
// connection that was down, a git lock a later command clears), so such a pod is asked again after
// an hour rather than held until a role acts; unpushed work does not go away by itself, so a
// decline for it waits for the role.
const DeclineUnreadRetry = time.Hour

// IdleDecline is the idle release the daemon last declined for an issue, as the idle view reads it.
// At is when the check first found the work the decline names, by the daemon's clock, and the
// moment the role was told; CheckedAt is when a check last read the workspace, or tried to. Each is
// the moment the read began, so a turn that starts while the read runs is a turn after it. Reason
// is one of the two above. StartedRow is the newest start row of the issue's claims at the last
// check, and PodUID the pod the last check read, "" where the runtime could not say. Work is
// whether the last read that could read the workspace found work that origin does not hold: a read
// that failed keeps what the read before it found (store's recordDecline). The zero IdleDecline is
// no decline.
//
// A check that reads the same work again through the same role (the same working-copy commit, the
// same count of commits and the same change) keeps At and tells nobody (store's
// DeclineIssueRelease): the role was told at At, and telling it again would start another turn for
// nothing.
type IdleDecline struct {
	At         time.Time
	CheckedAt  time.Time
	Reason     string
	StartedRow int64
	PodUID     string
	Work       bool
}

// Stands is whether the decline still holds the pod at now, so that the pass marks no release,
// writes no outbox row, asks no shim and sends no message. It ends at the first of four facts the
// pass reads without asking the pod anything:
//
//   - a live claim of the issue started a turn after the last check: a role changes its
//     workspace only in a turn, so only after one can a read find anything other than what the
//     last check found. The one turn the decline's own message starts ends the decline once, and
//     the read it allows finds the same work, which keeps the decline's moment and sends no
//     second message, so no message starts the next turn;
//   - a claim's process was started again after the last check (a start row above StartedRow);
//   - the runtime holds another pod for the issue than the one the last check read (podMoved):
//     the disk the check read went with that pod, and with it the work the decline names;
//   - for a read that failed, DeclineUnreadRetry has passed since the last check.
func (d IdleDecline) Stands(v IdleView, now time.Time) bool {
	if d.At.IsZero() || d.podMoved(v) || d.RetryDue(now) {
		return false
	}
	for _, c := range v.Claims {
		// A start ends the decline whatever the claim is now; a turn ends it only where its claim
		// is live.
		if d.StartedSince(c.LastStartRow) || c.Live() && d.TurnedSince(c.LastTurnStart) {
			return false
		}
	}
	return true
}

// HoldsUnpushedWork is whether the decline says the disk of the pod that stands now holds work
// that no branch or tag on origin holds: the last read that could read the workspace found such
// work, the pod is the one that read was made in, and no claim of the issue was started or ran a
// turn since that read. The release of a pod no claim owns and the close of a left pod with no live
// claim read it (workflow.UnownedPodRelease, LeftPodClose): such a pod runs no role to read
// through, so the decline on record is the one word the daemon has of its disk. A read that failed
// says nothing of the disk, so it ends nothing.
//
// A start of a claim's process after the last read (a start row above StartedRow), or a turn of a
// claim that started after it, ends the hold, in whichever pod it ran: the process can have pushed
// the work, or made more (the Legion reviewer's point 2 on R6 of #5285). Ending it
// deletes nothing unread: both callers then have the pod's launcher read the disk before any
// delete (workflow.IdleDecision.Read), and a read that finds the same work writes the decline again
// at its own moment, so each start or turn costs one read.
func (d IdleDecline) HoldsUnpushedWork(v IdleView) bool {
	if d.At.IsZero() || !(d.Work || d.Reason == DeclineUnpushedWork) || d.podMoved(v) {
		return false
	}
	for _, c := range v.Claims {
		if d.StartedSince(c.LastStartRow) || d.TurnedSince(c.LastTurnStart) {
			return false
		}
	}
	return true
}

// StartedSince is whether a claim whose newest start row is startRow had its process started again
// after the decline's last check (a start row above StartedRow). It and TurnedSince are the two
// tests of a claim that ends a decline: the idle rules read them (Stands, HoldsUnpushedWork), and
// so does the read that may end an update round's keep of a pod (internal/daemon's
// keptPodReadDue).
func (d IdleDecline) StartedSince(startRow int64) bool { return startRow > d.StartedRow }

// TurnedSince is whether a claim whose newest turn started at turnStart started that turn after the
// decline's last check (StartedSince).
func (d IdleDecline) TurnedSince(turnStart time.Time) bool { return turnStart.After(d.checked()) }

// RetryDue is whether the decline is of a read that failed (DeclineWorkspaceUnread) and
// DeclineUnreadRetry has passed at now since its last check (Stands, and internal/daemon's
// keptPodReadDue).
func (d IdleDecline) RetryDue(now time.Time) bool {
	return d.Reason == DeclineWorkspaceUnread && !now.Before(d.checked().Add(DeclineUnreadRetry))
}

// podMoved is whether the runtime holds another pod for the issue than the one the decline's last
// check read. Where either uid is unknown it is not: holding the pod keeps the work, and a pass
// that cannot tell must not delete a disk a decline says holds work.
func (d IdleDecline) podMoved(v IdleView) bool {
	return d.PodUID != "" && v.PodUID != "" && d.PodUID != v.PodUID
}

// checked is the moment of the decline's last check, which is its first moment where no later
// check has read the same work.
func (d IdleDecline) checked() time.Time {
	if d.CheckedAt.Before(d.At) {
		return d.At
	}
	return d.CheckedAt
}

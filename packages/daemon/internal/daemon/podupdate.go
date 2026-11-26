package daemon

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/sandbox"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// reconcilePodUpdates carries every update round of this project one step, over the claims the
// orphan sweep already read this tick. A running issue pod whose configuration has drifted is
// never replaced where it is found: that ends every role process in it mid-turn and deletes the
// pod's disk with everything unpushed on it. The daemon opens a round over the pod instead — each
// role the round asks (roundAsks) is asked, on its own Envoy topic, to do the steps of the update
// message and answer (supervise.UpdateTask holds those steps; openPodUpdate, record.UpdateAsk,
// api's claims/ready-for-update) — and replaces the pod once each of them has answered
// (waitingClaims), no turn runs and no claim with a process in the pod reports background work
// that runs or has sent no report since its last turn started (roundEnd.holding), or at the
// round's bound (pod_update_grace_minutes after the round opened, roundBound) whatever the roles
// said and whatever runs (replacementDue). Before the bound the replacement also waits for a claim
// that a project's pause still keeps and that the round does not ask (unansweredClaims). A role's
// own push is what keeps its work. Before every replacement the daemon reads the pod's workspace
// through a process in the pod, or through the pod's launcher where none runs there
// (readPodWorkspace, R7 of #5285): over a drift the pod's roles can keep working under, a read
// that finds work that no branch or tag on origin holds, or that cannot be made, keeps the pod,
// and the round ends with no replacement (roundKeeps, keepPodForRound). What a role has not pushed
// when a replacement still takes the pod — a round over a drift that breaks the pod
// (runtime.Breaks) — is lost with the pod, unless the pod's disk is a kept volume that the issue's
// next pod mounts (roundKeptVolume), and the issue's architect is told what the replacement took.
// An answer from every role the round asks ends the round early once nothing else holds it
// (roundEnd.holding); no answer holds it past the bound. An operator's suspension of a claim is
// the one wait that outlasts the bound, and only over a pod that is there: a gone pod ends its
// round whatever the hold (readReplacement).
//
// The round is the issue's, not the locator's: a suspended claim's locator is nil (supervise's
// letGo), and before the bound no pod disk may be deleted before every worker the round asks has
// signalled (roundAsks, waitingClaims). A claim an operator suspended is one of them. So is a
// claim a project's pause stopped, while the pause still keeps it: the one exception
// (podHolds.owes, roundClock.passesOver). Any other claim the daemon suspended is not one of
// them: it runs no process and no turn in the pod (dispatch://INFRA-4109 R1). What such a claim
// left unpushed is on the pod's disk, which the read before the replacement reads through a role
// whose process runs in the pod, or through the pod's launcher where none does
// (readPodWorkspace), and the replacement's line names what the read found. Every role claim of one
// issue shares that issue's one Sandbox and pod, so the issue is what groups them. A group whose
// claims' locators name two different pods is left alone this tick: a replacement is already in
// flight, and the next tick reads it settled.
//
// Two kinds of claim are left out of every group, and so out of every decision below. A retired
// claim runs nothing on any pod: it holds no process, it blocks no round, and a round an earlier
// release left recorded on it names a pod that is long gone — read beside a live claim's pod it
// would say, at every tick, that this issue's pod went without its round. A claim with no issue
// (the project controller's) is in no round at all: its pod has no issue workspace and nothing to
// push, and its plugin contract is carried by the stale-plugin relaunch instead
// (supervise's refreshPlugin).
//
// The store never holds the live pod a claim's launch waits on (supervise.Claim.AwaitsPod): only
// the claim's machine holds it. Such a claim records no process and may be its issue's only
// claim, so without that pod the round would have no pod to open over (groupPod). So for each
// claim it groups that records no process, the sweep reads that pod off the claim's machine
// (supervise.Machine.AwaitsPod). That read is the machine's published view and never waits on the
// decision in flight, for the reason roundGroup gives. Nor does the store hold the set that each
// end of a round reads to name its members: the pods in which an agent of a claim registered a
// session (supervise.Claim.RegisteredPods, which workOnRoundPod reads). A claim keeps a pod in it
// after it has started in another pod, so the sweep reads the set off the machine of every claim
// it groups (supervise.Machine.RegisteredPods), from the same published view. A claim that no
// machine of this daemon holds is grouped as the store gives it, with an empty set. What is read
// goes on the group's own copy of the claim and never on claims, which the orphan sweep's other
// passes read after this one (reconcileOrphans). Whom an end names reads one fact of the store
// besides the claims: whether the round's pod mounts a kept volume that an earlier pod of the
// issue mounted (roundPodHoldsEarlierWork). The replacement through a process and the replacement
// by issue read it before they ask the runtime (ReplacePod, ReplaceIssuePod), which can refuse or
// answer that the replacement is no longer due and end nothing; the next sweep then reads it
// again. The finish of a replacement's record outside the round's own call reads it as it ends
// the round (endRecordedRound, endTakenRound).
//
// Everything a round decides comes from durable records, but the pod a claim's launch waits on
// (above): the claim columns — the round, its reasons, when it opened, when each worker was asked,
// when each answered, and when the operator's side was told — the decline row an earlier round's
// keep of the pod wrote (store's idle_release_declines, podKeptForWork), and the record of each
// replacement that may take a disk, from before the act until its notice is queued (store's
// pod_replacements, podreplace.go), whose unfinished row holds the issue's round at a sweep
// (finishReplacements). A daemon restart therefore resumes a round exactly where it stood, and the
// same records give the same decision, once each claim that waits on its pod has been refused
// again and holds that pod again. Whom an end names reads the set beside that pod too (above). A
// restart forgets it, and the claim's machine reads it again off the stored claim (supervise's
// bootRegisteredPods); what an end names of a claim whose set the restart left unknown is
// workOnRoundPod's to say. This pass keeps three things of its own in memory, and none of them
// goes in the store:
//
//   - The pace at which it opens new rounds: when it last opened one, and the issues it has
//     opened one for whose claims have not recorded it yet (podUpdateOpenings). It paces this
//     daemon's own openings and decides no round. A daemon that has just booted opens its first
//     round at its first sweep, one sweep interval after the start.
//   - The rounds it has ended whose end a member's machine has not taken yet (podUpdateEnded).
//     The end is posted to each member, and a machine in a relaunch takes the post only once the
//     relaunch is done, so its claim keeps the round until then. While it does, the sweep reads
//     that claim as the end leaves it: it decides the end once, and the round holds no
//     open-round slot (reconcileIssuePodUpdate, dispatch://INFRA-4109 R6). A daemon that has
//     just booted has lost this record and the posts not taken yet, and it makes the end once
//     more for each claim the store still holds in the round (dispatch://INFRA-3276 R5).
//   - What the launches into an issue's pod decided of the round over it
//     (podUpdateLaunchReplaced). A launch reads the round as it runs, and where the round is over
//     with no role process in its pod, the launch replaces the pod as the round's own replacement
//     (roundPodEndsAtLaunch, dispatch://INFRA-4109 R7); the sweep that then reads that pod gone,
//     or another pod in its place, ends the round as that replacement, with its lines and its
//     message (endRoundALaunchReplaced). The record also holds each launch the reading kept the
//     pod for, so a second launch at once reads the first as a process in the pod. A daemon that
//     has just booted has lost it; the record the launch wrote of the replacement in the store
//     still stands, and the sweep ends the round as that replacement from it (finishReplacements),
//     whose line names the members by the same rule (lostClaims), read over the sets the restart
//     read again (above).
//
// The sweep walks the issues in key order, decides each of them, and only then makes the pod
// replacements those decisions left it, also in key order. A replacement waits its pod out — a
// sweep interval at worst (replacePodForRound) — so one made where it is decided holds every
// issue after it in the walk for minutes, and the project's two limits (podupdate_limits.go)
// would open one round per slow sweep rather than the rounds a minute the project asks for.
//
// Those two limits are what keeps one pod template change from waking every worker of the fleet
// in one minute: `pod_update_max_open_rounds` caps how many issues may hold an open round at
// once, and `pod_update_rounds_per_minute` how many rounds a sweep may open (podUpdateSweep).
// Neither ends a round, pauses one, or holds back the ask of one: the gate stands only where
// a new round would open, so a change reaches a few pods first and the rest as those finish.
//
// A pod a project's pause holds (store's project_pause_pod_holds, decision 12 of
// dispatch://INFRA-3759) is neither replaced nor given a round: the pause kept the pod and the
// work on its disk for its stopped roles. A round over it waits as it waits for an operator's
// suspension, tells nobody, and its bound moves by the time the pod was held, counted on to the
// let-out of the last launch the pause held for a role of the pod (roundBound). A role it stopped
// is a claim the round asks once its agent is ready and waits for, although the pause suspended
// it, by the one exception (podHolds.owes, roundClock.passesOver). The sweep reads the claims from
// memory, so it reads the project's holds once, here, rather than once per issue (podPauseHolds),
// and hands each issue its holds with the sweep's now as one value (roundClock).
// A sweep that cannot read them decides nothing, since a reading nobody took may not delete a disk
// (replacementWait); the next sweep reads them again.
func (s *supervision) reconcilePodUpdates(ctx context.Context, claims []supervise.Claim) {
	now := s.plan.clock.Now()
	byIssue := map[string][]supervise.Claim{}
	var issues []string
	for _, c := range claims {
		if c.Issue == "" || c.State == supervise.StateRetired {
			continue
		}
		// c is a copy of the claim in claims, so what is set on it stays in the group. Only a
		// claim that records no process is read for the pod its launch waits on, since every
		// reader takes c.Locator first (podLocator, awaitsRoundPod). Every claim is read for the
		// pods an agent of it registered a session in, which keep a pod the claim has left since
		// (workOnRoundPod).
		if m, ok := s.supervisor.Machine(c.Token); ok {
			if c.Locator == nil {
				c.AwaitsPod = m.AwaitsPod()
			}
			c.RegisteredPods = m.RegisteredPods()
		}
		if _, grouped := byIssue[c.Issue]; !grouped {
			issues = append(issues, c.Issue)
		}
		byIssue[c.Issue] = append(byIssue[c.Issue], c)
	}
	slices.Sort(issues)
	holds, err := s.podPauseHolds(ctx, claims, now)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Error("supervise: read the pods a project's pause holds; no update round is decided this sweep",
				"error", err)
		}
		return
	}
	// The records of replacements a call or a daemon left standing are finished first, from the
	// read made before each, so the walk reads each round as that finish leaves it (podreplace.go).
	// An issue whose round's record still stands is decided nothing of at this sweep, and a store
	// that cannot be read decides nothing at all: a reading nobody took may not delete a disk.
	unfinished, err := s.finishReplacements(ctx, claims)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Error("supervise: read the records of the replacements of issue pods; no update round is decided this sweep",
				"error", err)
		}
		return
	}
	sweep := s.beginPodUpdateSweep(now, byIssue)
	s.forgetLaunchRounds(byIssue)
	var replacements []podReplacement
	for _, issue := range issues {
		if ctx.Err() != nil {
			return
		}
		if unfinished[issue] {
			s.log.Info("supervise: the record of a replacement of this issue's pod stands unfinished; no update round of the issue is decided this sweep",
				"issue", issue)
			continue
		}
		if replace := s.reconcileIssuePodUpdate(ctx, issue, byIssue[issue], &sweep, s.roundClockAt(now, holds[issue])); replace != nil {
			replacements = append(replacements, *replace)
		}
	}
	if len(sweep.waiting) > 0 {
		s.log.Info("supervise: these drifted issues wait for an update round under this project's limits",
			"waiting", sweep.held())
	}
	s.recordPodUpdateSweep(issues, byIssue, sweep)
	for _, replace := range replacements {
		if ctx.Err() != nil {
			return
		}
		if replace.by == viaIssue {
			s.replaceIssuePodForRound(ctx, replace)
		} else {
			s.replacePodForRound(ctx, replace)
		}
	}
	for _, read := range sweep.keptReads {
		if ctx.Err() != nil {
			return
		}
		s.readKeptPod(ctx, read)
	}
}

// podHolds is what a project's pause did to one issue's pod as a sweep reads it
// (store.Store.PausePodHoldsSince): each hold that stands on the pod, each that ended, or whose
// pause let out a launch of the pod, since the oldest round of the sweep opened, and each whose
// pause still owes a role of the pod a launch, with each role it owes one
// (store.PausePodHold.Owed, decision 12 of dispatch://INFRA-3759).
type podHolds []store.PausePodHold

// held is whether a hold stands on the pod now: a round over it waits, and no round opens over
// it.
func (h podHolds) held() bool {
	for _, hold := range h {
		if hold.EndedAt.IsZero() {
			return true
		}
	}
	return false
}

// since is how long the holds kept the pod from opened on, read at now: each hold's span after
// opened, up to the moment it counts to (heldUntil). A round's bound moves by it
// (roundClock.roundBound).
func (h podHolds) since(opened, now time.Time) time.Duration {
	var held time.Duration
	for _, hold := range h {
		start, end := hold.HeldAt, heldUntil(hold, now)
		if start.Before(opened) {
			start = opened
		}
		if end.After(start) {
			held += end.Sub(start)
		}
	}
	return held
}

// heldUntil is the moment hold counts as keeping its pod to, read at now, for the round's bound
// alone (roundClock.roundBound), which is the one reader of the latest let-out of all the roles.
// A hold that stands counts to now. A hold that ended at the resume or at a role's ready counts to
// the later of that end and the latest let-out of a launch its pause held for a role of the pod
// (store.PausePodHold.LetOut), which is the sweep's own now while such a launch still waits in
// the wake pacing's queue: a role whose launch waits its turn in the wake pacing can answer no
// round, and the pause, not the role, kept it from taking a turn, as the stuck clock of its
// resume counts nothing before that let-out either (decision 14 of dispatch://INFRA-3759). A hold
// a deletion ended counts to that end: the pod is gone.
func heldUntil(hold store.PausePodHold, now time.Time) time.Time {
	switch {
	case hold.EndedAt.IsZero():
		return now
	case hold.End == store.PausePodHoldDeleted || !hold.LetOut.After(hold.EndedAt):
		return hold.EndedAt
	default:
		return hold.LetOut
	}
}

// owes is whether a project's pause still keeps the pod of round for c's work. It is the one
// statement of that rule: the round's sets read it (roundClock.passesOver), and so do the claims
// that hold the replacement through a process for an answer (unansweredClaims) and what holds the
// pod of a round with no process in it (roundPodEnds), for every claim of the issue, the claim of
// a launch into the pod included (roundPodEndsAtLaunch). Three things must all be true:
//
//   - A hold that no deletion ended owes c's role a launch (owedLaunch): the store reads, role by
//     role, that the role has not run in the pod since the pause stopped it
//     (store.PausePodHold.Owed). A hold a deletion ended keeps nothing: the pod is gone.
//   - c has not answered the round (answered). A claim that answered pushed its work first, so the
//     pod keeps nothing for it.
//   - c is still the pause's now. It is suspended released, since its held start has not run
//     (supervise.Claim.Released). Or it records no process and it is not suspended, failed or
//     retired (supervise.Claim.Gone). That is the launch the pause still owes the role, in each
//     state the launch can be in. The wake pacing or the pause's gate parks it, or its launch
//     decision runs (supervise's waitOnMachine). A daemon restart restored it launch_uncertain,
//     and the release of that launch made it queued (supervisor.restore,
//     supervise.Claim.ReleasedLaunch). Or the launch forgot the role's lost session, so the claim
//     records no session until its new agent registers (supervise's startFresh).
//
// All three are read from the claim, the round and the store's list of owed roles, and nothing
// else. The decision that parks a launch writes the claim launching in that same decision
// (waitOnMachine). So no reading waits for a write that comes later, such as the let-out that the
// daemon writes on a goroutine of its own after the park (pause.go). A role that came up in the pod
// is owed nothing. A claim suspended by any other source than a release (the daemon's own rule, a
// root's exit, an unknown source or an operator) is gone and not released, and so is a failed or a
// retired claim. None of them is the pause's, whatever another role of the pod still waits for. A
// hold that stands needs no case here: a round waits over a pod a hold stands on, and no round
// opens over it (held).
func (h podHolds) owes(c supervise.Claim, round string) bool {
	return h.owedLaunch(c.Role) && !answered(c, round) && (c.Released() || c.Locator == nil && !c.Gone())
}

// owedLaunch is whether a hold of the pod that no deletion ended owes role a launch
// (store.PausePodHold.Owed). One pause can hold the pod twice: a hold ends at a role's ready while
// the pause stands, and a later stop of the same pause opens a second hold. The store reads the
// resumes of a hold by its pause and its issue, so both holds name the role, and either answers.
func (h podHolds) owedLaunch(role claim.Role) bool {
	for _, hold := range h {
		if hold.End != store.PausePodHoldDeleted && slices.Contains(hold.Owed, role) {
			return true
		}
	}
	return false
}

// podPauseHolds is the holds a project's pause has or had on the project's pods, by issue, read
// once for the sweep, and again under a pod's launch turn (clockUnderTurn): every hold that
// stands, every hold that ended, or whose pause let out a launch of its pod, since the oldest
// round any of claims records opened, and every hold whose pause still owes a role of its pod a
// launch, with each role it owes one. These are the holds a round's bound can move by
// (podHolds.since) and the holds that can still keep a pod for a role (podHolds.owes).
func (s *supervision) podPauseHolds(ctx context.Context, claims []supervise.Claim, now time.Time) (map[string]podHolds, error) {
	since := now
	for _, c := range claims {
		if c.UpdateOpenedAt != nil && c.UpdateOpenedAt.Before(since) {
			since = *c.UpdateOpenedAt
		}
	}
	read, err := s.supervisor.store.PausePodHoldsSince(ctx, s.cfg.Project, since, now)
	if err != nil {
		return nil, err
	}
	holds := map[string]podHolds{}
	for _, hold := range read {
		holds[hold.Issue] = append(holds[hold.Issue], hold)
	}
	return holds, nil
}

// reconcileIssuePodUpdate carries one issue's pod round a step. group is every claim of the issue
// that has not retired, whatever state it is in.
//
// The decisions are taken in this order, and the order is the rule. A round the sweep has already
// ended comes first, and is not decided again (below). A round open over a pod other than the one
// the claims record is over: that pod went, and the pod the claims record now is the one in its
// place. Such a round ends at that sweep, by the one rule for a round whose pod went
// (endRoundWhosePodWent), whatever the runtime says of the pod that replaced it, its drift
// included — a drifting replacement is given a round of its own at a later sweep, an
// opening under the project's limits like any other — and the end is posted to the members of
// that round alone (endPodUpdates), so a claim already in a round over the new pod stays in it.
// Then the drift, which is also what says the pod is not there: a round open over a pod the
// runtime no longer has ends at that reading, and a pod that has drifted from nothing and has no
// round open over it needs no replacement. A round open over the pod the claims still record goes
// on, drift or no drift: once opened, a round ends only by replacing its pod, or because that pod
// is gone. Only then is a complete round acted on, and only then is a round opened or kept open.
//
// The sweep decides the end of a round once (dispatch://INFRA-4109 R6). Every end it makes is
// posted to the round's members and kept as the issue's ended round (endPodUpdates,
// podUpdateEnded). The post is asynchronous: a member whose machine is in a relaunch takes it
// only once the relaunch is done, and its claim records the round until then. While a claim
// records that round, the sweep reads the claim as the end leaves it, in no round
// (withoutRound), and it decides, logs, posts and writes on the issue nothing of that round.
// Read as it stands, the round would be ended again at every sweep, with its end posted to each
// member again and its message written on the issue again: OSSV-522's round was ended at each of
// 11 sweeps while its implementer's machine relaunched. Within one daemon a post to a machine's
// queue is never lost (inbox), so the end reaches every member.
//
// Only the ended round is read as over. One group can hold the dead pod's round on one claim and
// the round over the pod in its place on another (endPodUpdates), and that other round is decided
// as usual, from the group as the end leaves it. Where no claim records another round, nothing
// else of the issue is decided at that sweep: its claims are not yet as the end leaves them, what
// they record of the pod can be the pod the ended round was opened over, and a round opened from
// that pod would be the ended round again. The issue's next round can open from the first sweep
// after the end reaches every member, and the ended round holds no slot meanwhile
// (beginPodUpdateSweep).
//
// A restart forgets the ended rounds and the posts not taken yet, and the store keeps the round on
// each claim whose machine did not take the end, for the next boot to restore. The end of a round
// over a replaced pod is read from the claims, so that boot makes the end once more. Waiting for
// the pod in its place to stop drifting would leave such a round open for as long as that pod
// drifted, and the dead round's slot would hold the issue's own next round back behind the
// project's open-round limit (dispatch://INFRA-3276, R5).
//
// A claim whose launch the runtime refused for want of its pod's update round records no process,
// and the pod its launch waits on stands in for one (supervise.Claim.AwaitsPod, podLocator): under
// dispatch_token_broker with no transition token, a launch that would keep a live pod built
// before the exchange identity is refused at every try until that pod's round replaces it, and
// such a claim may be the issue's only one. So that pod is the group's pod (groupPod), its drift
// is read through that claim (groupDrift), and the round opens over it as over a pod a process
// runs in. The round waits on no answer and no turn of such a claim, since it has no process
// there (waitingClaims). Which such claims a gone pod names as having lost work is lostClaims'
// question, and which ones a replacement's line names is unwaitedOnThePod's. Where no claim
// records a process in the pod, the pod is replaced through it (podProcess), and its next try,
// within a minute, comes up in the pod that replaced it (dispatch://INFRA-4425 R4). Which
// replacement of the pod comes first, whether it reads the workspace, and what it tells the issue
// of such a claim are written once, in the doc of lostWaitingClaims.
//
// A group whose claims record no pod at all, neither a process nor a pod a launch waits on,
// decides nothing from its claims: no locator is no evidence either way — every claim of the
// issue may be suspended (letGo clears the locator), or the one live claim may be minutes into a
// relaunch. So the runtime is asked, by the issue alone, which pod the issue's Sandbox holds now
// (podlessRound): another pod than the round's, or none, is the round's pod gone, and the round
// ends as a gone pod's round does. The round's own pod is replaced by issue where nothing holds it
// (roundPodEnds, dispatch://INFRA-4109 R7), and otherwise, or with no answer, the round waits for
// a claim to record a pod again. A launch into that pod reads the same rule as it runs
// (roundPodEndsAtLaunch), so a launch that comes before the sweep replaces the pod itself.
//
// A group whose claims record a pod the runtime no longer has ends the round open over it at
// that sweep, and decides nothing else (groupDrift, podGone, endRoundOverAGonePod). It is one of
// the readings that say a pod went, beside a different pod in its place and, where no claim
// records a pod, the runtime's word on the issue's Sandbox, and the round ends on whichever the
// sweep reaches first.
//
// A group whose claims record a pod the runtime cannot read decides nothing at that sweep: no
// round opens over it and none ends (groupDrift, podUnread), and the next sweep reads it again.
// Past the bound of a round open over it, that read holds the round, and the operator's side is
// told of it once, or of an operator's hold in its place (tellUnreadPod).
//
// A pod a launch replaced as its round's own replacement (roundPodEndsAtLaunch) is read gone by
// the same readings, and the round it was the pod of ends at whichever comes first as the
// replacement the round made (endRoundALaunchReplaced, podUpdateLaunchReplaced), with the line of
// a replacement by issue, which names the members that had not answered and whose work may be on
// the pod's disk (lostClaims), and what the pod's launcher read before it, and not as a pod that
// went without its round. A daemon that has just booted has lost that record, and ends such a
// round as that replacement from the record the launch wrote in the store before it
// (finishReplacements), whose line names the members by the same rule.
//
// Which reading that is, and when it comes, changes nothing the issue is told, because the
// report is read from the round's own record and from where each member's work may be
// (workOnRoundPod) rather than from where any claim's process is now. The round records, on each
// claim it asked, the round that claim left and whether it had answered when it left (supervise's
// leftPodRoundAt), and only the round's end clears either. So a claim relaunched into the pod that
// replaced this one is a member of the round still, with the part the dead pod left it; a role
// first started in that new pod is no member and is named nothing; and an earlier sweep's end
// landing on some claims before others takes those claims out of the round rather than into the
// report. What the pod took with it is the members who neither answered nor left having answered
// and whose work may be on that pod's disk (lostClaims), which is a different question from whom
// a replacement waits for (unansweredClaims).
//
// It reports the replacement it leaves the sweep: the replacement of a complete round's pod, or of
// the pod of a round that has no role process in it and that nothing holds (by issue), which the
// sweep makes after its whole walk over the issues, or nil where there is nothing left to do for
// the issue (reconcilePodUpdates, podReplacement). The walk leaves the sweep one more kind of step,
// the read that may end an earlier round's keep of a pod, on the sweep's own list
// (podUpdateSweep.keptReads, readKeptPod).
//
// clock is the sweep's now and what a project's pause did to the issue's pod (roundClock), built
// once for the issue: each round bound and each pause hold read below comes from it, and an
// opening is recorded at its now. No round opens over a pod a hold stands on (roundClock.held):
// the pause kept that pod for its stopped roles, and a round would ask them to push work for a
// replacement while nothing of theirs runs.
func (s *supervision) reconcileIssuePodUpdate(ctx context.Context, issue string, group []supervise.Claim, sweep *podUpdateSweep, clock roundClock) *podReplacement {
	slices.SortFunc(group, func(a, b supervise.Claim) int { return strings.Compare(string(a.Token), string(b.Token)) })
	if sweep.ended[issue] != "" {
		// A claim of the issue still records the round the sweep has ended (podUpdateSweep.ended
		// keeps no other), so the issue is decided from the group as that end leaves it.
		group = sweep.asEnded(issue, group)
		if openPodRound(group) == "" {
			return nil
		}
	}
	pod, split := groupPod(group)
	if split {
		s.log.Info("supervise: the claims of this issue run in two pods; their update round waits for the replacement in flight",
			"issue", issue)
		return nil
	}
	open := openPodRound(group)
	if pod == "" {
		if open != "" {
			return s.podlessRound(ctx, issue, group, open, clock)
		}
		// No drift can be read of a pod no claim records, so no pod round opens over one.
		return nil
	}
	round := supervise.PodRound(pod)
	if open != "" && open != round {
		// The claims record the pod that replaced the round's own, which is the evidence that the
		// round's pod went. A launch into the round's pod may have replaced it as the round's own
		// replacement (roundPodEndsAtLaunch). Otherwise the pod was replaced where it stood for a
		// failure of its own — a terminal pod, a failed init container, a superseded disk, a lost
		// launcher binding — while a worker still had work to push. Its work stays only where the
		// issue's Sandbox still holds the kept volume that it held when the round opened, and the
		// issue's next launch keeps that volume (podWentWithoutItsRound, roundKeptVolume); an
		// ephemeral disk went with the pod. The round ends by the one rule for a round whose pod
		// went (endRoundWhosePodWent), counted from the group this walk decided from. Nothing of the
		// pod in its place decides anything here: the round was opened over a pod that is not
		// there, no answer and no replacement can end it any more, and the drift of the new pod is
		// that pod's own round to open.
		s.endRoundWhosePodWent(ctx, issue, group, open, pod, podWentOverLine)
		return nil
	}
	drift := s.groupDrift(ctx, issue, group)
	if drift.reading == podGone {
		s.endRoundOverAGonePod(ctx, issue, group, open, pod)
		return nil
	}
	if drift.reading != podRead {
		// The unread pod decides nothing, and past the bound of a round open over it the
		// operator's side is told of the round once (tellUnreadPod). The snapshot says first
		// whether the bound has come and whether the round was told, so a round before its bound
		// costs no read of the machines. A sweep the daemon cancelled read nothing of the pod, and
		// tells nothing.
		if open != "" && ctx.Err() == nil && clock.pastBound(group, open) && !roundTold(group, open) {
			told := s.noticeGroup(group)
			s.tellUnreadPod(ctx, issue, told, open, pod, roundReasons(group, open), heldByOperator(told, open, clock), clock)
		}
		return nil
	}
	if len(drift.reasons) == 0 && open == "" {
		// A pod that has drifted from nothing, with no round open over it, needs no replacement:
		// returning here is what keeps the code below from opening a round over it.
		return nil
	}
	// The drift as a round would record it: each reason of a drift that breaks the pod first
	// (runtime.RoundReasons), so the record opens on the mark where one of its reasons does.
	terms := runtime.RoundReasons(drift.reasons)
	if open == "" && clock.held() {
		s.log.Info("supervise: a project's pause holds this drifted issue's pod; no update round opens over it until the hold ends",
			"issue", issue, "pod", pod, "reasons", terms)
		return nil
	}
	if open == "" {
		// An earlier round kept this pod for work on its disk that no branch or tag on origin
		// holds, or for a read that could not be made (keepPodForRound): no round opens over it
		// while that record stands, unless this drift breaks the pod, and the read that may end
		// the record is a step after the walk (readKeptPod), without a round (R7 of
		// #5285).
		if kept, due := s.podKeptForWork(ctx, issue, group, pod, terms); kept {
			if due {
				sweep.keptReads = append(sweep.keptReads, keptPodRead{issue: issue, pod: pod, reasons: terms})
			}
			return nil
		}
	}
	// The drift this tick read opens a round and decides nothing of a round already open: a
	// round's terms are fixed at its open (dispatch://INFRA-2981 R9), so what an open round's
	// notices and its replacement name is its own recorded reasons (roundReasons), and what its
	// post carries is those and its own recorded repository (roundRepository). That holds
	// where the drift has gone away too — an operator rolled the release back — and the round
	// replaces the pod all the same, and where a drift that breaks the pod came after the open:
	// the round is read by the reasons it recorded, and the next round is over that drift (spec
	// v23). The round has already asked each role it asks (roundAsks) to
	// do the steps of its update message (supervise.UpdateTask) and to start no new work, and no
	// path is left that lets a role carry on in the pod it was told would be replaced. Replacing it
	// is one restart of each role in its own session, which the alternative — a release of the
	// answered roles that the daemon alone knows about — is not.
	repository := drift.repository
	// One live read, given to both decisions below, so the claims the replacement waits on and
	// the claims the post records the round on, which the ask then reaches, are one set. Read
	// live by the replacement and from the snapshot by the post, a claim whose session registered
	// after the tick's read would block the round in the one and be no worker of it in the other:
	// the replacement would wait on that claim for an answer to a round the post never recorded
	// on it, so no ask would reach it, and the round would run to its bound waiting for a worker
	// nothing had asked anything of.
	//
	// The replacement reads that set masked (live), and the post and the ask read it as the claims
	// hold it (told): unaskedClaims says why the ask may not read it masked.
	var told []supervise.Claim
	if open != "" {
		// A round open here is over the pod the claims record, the one round this issue can be
		// in: a round over any other pod has ended above.
		var live []supervise.Claim
		told, live = s.liveGroup(group)
		terms, repository = roundReasons(group, open), roundRepository(group, open)
		if replace, decided := s.completePodUpdate(ctx, issue, group, live, open, pod, terms, clock); decided {
			return replace
		}
	}
	// A round the claims do not already record is an opening, and the project's limits may hold
	// it back (podUpdateSweep). A round they do record is posted again at every tick, which
	// records nothing new on a claim that holds it, and no limit holds that post back: a round
	// held back after it opened would leave its workers waiting in a pod the daemon has already
	// told them is going, and these limits are about how fast a change reaches the fleet, not
	// about how long a round may run.
	//
	// A round this daemon has already opened, whose post the claims have not recorded yet, is
	// posted again too, as that same opening rather than a new one, and no limit holds that post
	// back either (podUpdateSweep.reposts): it is in a round for the sweep's record, and nothing
	// more is charged for it.
	opening := open == "" && !sweep.reposts(issue, round)
	if opening && sweep.holds(issue) {
		return nil
	}
	if open == "" {
		told = s.noticeGroup(group)
	}
	if s.openPodUpdate(ctx, issue, told, pod, terms, repository, clock) && opening {
		s.recordPodUpdateOpening(sweep, issue, round, clock.now)
	}
	return nil
}

// completePodUpdate decides a round that is over. Before the round's bound that is every claim
// the round asks (roundAsks, waitingClaims) having answered, no claim that a project's pause still
// keeps and that the round does not ask (unansweredClaims, dispatch://INFRA-4109 R8), no claim of
// the group running a turn, and no claim with a process in the round's pod whose activity report
// names background work that runs or that has sent no report since its last turn started
// (backgroundClaims). Waiting for no claim to be working keeps the promise that no worker is
// stopped mid-turn before the bound — a worker answers from inside a turn, and that turn ends a
// moment later. Waiting for the reports keeps the same promise for the work a turn left running:
// a subagent or a background job is no turn, and it ends with the pod (dispatch://INFRA-4203 R1).
// A claim the round passes over (roundClock.passesOver) holds nothing here, whatever it left in
// the workspace (dispatch://INFRA-4109 R1). At the bound (roundBound) neither an answer, a turn
// nor a report holds the round any more, and the pod is replaced whatever the roles said and
// whatever runs (dispatch://INFRA-2981 R2, as dispatch://INFRA-4203 R1 changes it). It reports
// whether it decided the round, and with it the pod replacement that decision leaves the sweep to
// make.
//
// The replacement is handed back rather than made here because it waits its pod out, and the
// sweep makes every one of this tick's replacements after its whole walk over the issues
// (reconcilePodUpdates, replacePodForRound). Everything else this round is owed before the
// replacement — the log line, the notice of a round that has outlived its bound — is decided
// here, where the claims the sweep walked are. What the replacement itself is owed — the read of
// the issue's workspace where the round passed over a claim, the line that names each claim the
// round did not wait for and what the read found, and the message on the issue — is the
// replacement's, from the claims it reads again: the replacement handed back carries nothing of
// the claims (podReplacement).
//
// Every wait is read from the claims as their machines hold them now (live), never from the
// sweep's snapshot, because this is where the deletion of a disk is decided. The snapshot is the
// orphan sweep's, read once at the start of the tick, and after a fleet-wide roll it is minutes
// old by the time this issue is reached. A turn that starts inside that window is in the live
// read alone; a sweep deciding from the snapshot would end that turn before the round's bound
// and delete its disk. A claim whose machine is deciding reads as unanswered in the live read,
// which this is given masked (masked). A round the fresh read finds unanswered before its bound
// (unansweredClaims) is simply not complete: this reports that it decided nothing, and the post
// below keeps the round recorded on every claim. The snapshot is kept for one thing only:
// saying, in the log line, that the fresh read differs from it rather than that a worker never
// answered. The snapshot is read by the same rule (unansweredClaims), so the line says that only
// where the fresh read alone finds a claim that has not answered, and it says that the claim has
// not answered or its machine was deciding: the masked read cannot tell the two apart.
//
// The activity report is the one exception. The activity route writes it to the store and posts
// nothing to a machine, so the live read takes it from the group's own store rows (roundGroup),
// and those rows are the sweep's snapshot, up to one sweep old. A report that came after the
// snapshot is not lost: the replacement and its last word read the store again before they act
// (replacePodForRound, replacementDue), and a report of running work refuses the replacement
// there.
//
// Even that read is older than the act by the time the act is made, because the replacement
// runs after the walk. The replacement reads the claims once more immediately before it calls
// the runtime, and carries the daemon's own last word into the call behind that
// (replacePodForRound, replacementDue).
//
// A round with no live claim that records a process in its pod, nor one whose launch waits on
// that pod (podProcess), has nothing to replace the pod through, and ends on nothing the claims
// say: the runtime is asked about the pod itself (roundWithNoProcessInItsPod). A pod it answers
// gone ends the round. A pod it answers there is replaced by issue where nothing holds it
// (roundPodEnds, dispatch://INFRA-4109 R7), so no launch goes into a pod the round is about to
// replace. Once opened, a round ends by replacing its pod or because that pod is gone, and no
// locator is no evidence either way. The pod is asked about before an operator's suspension is
// weighed: a gone pod ends its round whatever the hold, and the hold keeps its place wherever the
// pod is there.
//
// Four readings hold a round past its bound, and each is told to the operator's side once, at
// the bound, naming the reading (tellLateRound): an operator's suspension, no process in the
// round's pod, two pods at once, and a pod the runtime cannot read. No process in the round's pod
// holds the round past its bound only where the runtime replaces no pod by issue
// (runtime.IssuePodReplacer). Where it does, the pod is replaced at the bound, and only a
// replacement by issue that the runtime refuses holds the round, which is told as a refused
// ReplacePod is (replaceIssuePodForRound). A pod the runtime cannot read is read so either before
// this is reached (reconcileIssuePodUpdate's podUnread) or when the round's own pod is asked about
// (roundWithNoProcessInItsPod). Such a read decides nothing, and an operator's suspension read in
// the same live group is told in its place (tellUnreadPod): one round is told once, and only that
// operator can end the hold. A round that waits for an answer, a turn or a report of background
// work before its bound tells nobody: the bound ends that wait by itself.
//
// A pod a project's pause holds waits as an operator's suspension does, whatever the clock says,
// and tells nobody (waitsForPause, decision 12 of dispatch://INFRA-3759): the pause is a person's
// or the controller's act, and its resume and a role's ready end the hold. The round's bound
// moves by the time the pod was held, counted on to the let-out of the last launch the pause held
// for a role of the pod (clock, roundClock.roundBound), and the replacement left to the sweep
// carries the holds the clock was built from (podReplacement).
func (s *supervision) completePodUpdate(ctx context.Context, issue string, snapshot, live []supervise.Claim, round, pod, reasons string, clock roundClock) (*podReplacement, bool) {
	reading := readReplacement(live, round, clock)
	switch reading.wait {
	case waitsForOperator:
		s.waitForOperator(ctx, issue, live, round, pod, reasons, reading.held, clock)
		// The round is not decided: the post below keeps it recorded on every claim, and asks
		// a claim it has not asked yet, as it does for a round that waits for an answer.
		return nil, false
	case waitsForPause:
		s.log.Info("supervise: a project's pause holds the pod of this update round; the replacement waits for the hold to end",
			"issue", issue, "round", round, "pod", pod)
		// Not decided either: the post below keeps the round recorded, as for an operator's hold.
		return nil, false
	case waitsForAnswer:
		if len(unansweredClaims(snapshot, round, clock)) == 0 {
			s.log.Info("supervise: every worker of this update round had answered in the sweep's snapshot, and the fresh read finds one that has not answered or whose machine was deciding something; the replacement waits for the next sweep or for the round's bound",
				"issue", issue, "round", round, "waiting", namesOf(reading.unanswered))
		}
		return nil, false
	case waitsForProcessInItsPod:
		return s.roundWithNoProcessInItsPod(ctx, issue, snapshot, live, round, pod, reasons, reading.held, clock)
	case waitsForTurn:
		s.log.Info("supervise: every worker of this update round has answered; the replacement waits for the turns still running, or for the round's bound",
			"issue", issue, "round", round, "working", reading.working)
		return nil, true
	case waitsForBackground:
		s.log.Info("supervise: every worker of this update round has answered and no turn runs; the replacement waits for the background work its roles report, or for the round's bound",
			append([]any{"issue", issue, "round", round}, reading.background.attributes()...)...)
		return nil, true
	case waitsForPodInFlight:
		s.log.Info("supervise: the claims of this issue run in two pods now; this round's replacement waits for the one in flight",
			"issue", issue, "round", round, "pod", pod)
		s.tellLateRound(ctx, issue, live, round, pod, reasons, twoPodsReason, clock)
		return nil, true
	case replacementReady:
		return &podReplacement{issue: issue, round: round, pod: pod, reasons: reasons, holds: clock.holds}, true
	default:
		s.log.Error("supervise: this update round's replacement was held back by a reading this sweep cannot name; nothing is replaced and the round stays open for the next sweep",
			"issue", issue, "round", round, "pod", pod, "reading", int(reading.wait))
		return nil, true
	}
}

// replacementWait is the one reading that holds a round's pod replacement back, and
// replacementReady where none of the seven does. The zero value is a wait rather than the
// go-ahead, because what these readings decide is the deletion of a pod's disk: a reading nobody
// took may hold a replacement for a sweep, and may not make one.
//
// The same rule is what both switches over it are written to (completePodUpdate,
// replacePodForRound): replacementReady is the one case that reaches the replacement, and every
// other value waits, the ones below named each with the line its caller logs and whatever else
// it decides, and anything else in a default that logs and replaces nothing. An eighth reading
// added here and missed at one of the two sites would otherwise delete a disk there, and in
// replacePodForRound call ReplacePod with the empty process of a reading that set none.
type replacementWait int

const (
	// waitsForAnswer is a claim that has not answered the round (unansweredClaims), read only
	// before the round's bound.
	waitsForAnswer replacementWait = iota
	// waitsForOperator is a claim the round asks that an operator's suspension holds
	// (heldByOperator), with a live claim recording a process in the round's own pod, or one
	// whose launch waits on it (podProcess), read whatever the clock says.
	waitsForOperator
	// waitsForProcessInItsPod is no live claim recording a process in the round's own pod, nor
	// one whose launch waits on it (podProcess), read whatever the clock says. It carries the
	// claims an operator's suspension holds, if any, and whether a project's pause holds the pod:
	// either hold is read only once the pod is known to be there (roundWithNoProcessInItsPod).
	// With no process to replace the pod through, the walk replaces it by issue where nothing
	// holds it (roundPodEnds), and the replacement through a process waits (replacePodForRound).
	waitsForProcessInItsPod
	// waitsForTurn is a worker of the issue running a turn (workingClaims), read only before the
	// round's bound.
	waitsForTurn
	// waitsForBackground is a claim with a process in the round's own pod whose activity report
	// names background work that runs, or that has sent no report since its last turn started
	// (backgroundClaims, dispatch://INFRA-4203 R1), read only before the round's bound. It is read
	// once no turn runs, so it is what holds a round whose roles have all answered and whose turns
	// have all ended while a subagent or a background job of a role may still run in the pod.
	waitsForBackground
	// waitsForPodInFlight is the claims recording two pods (groupPod), which is a replacement
	// already in flight, read whatever the clock says.
	waitsForPodInFlight
	// waitsForPause is a project's pause holding the round's pod (roundClock.held), with a live
	// claim recording a process in the round's own pod, read whatever the clock says (decision
	// 12 of dispatch://INFRA-3759). It is the operator's suspension's twin: the pause kept the
	// pod for its stopped roles, and its resume and a role's ready end the hold.
	waitsForPause
	// replacementReady is every reading passed: the round's pod may be replaced, through the
	// process the reading carries.
	replacementReady
)

// replacementReading is what the seven readings answered over one live group: the claim the
// round's pod is replaced through (podProcess), or the first reading that holds the replacement
// back with what its caller has to name of it — the claims that have not answered (unanswered,
// read by unansweredClaims), the claims an operator holds (for waitsForOperator, and for
// waitsForProcessInItsPod where a hold was read), the roles whose turns run, or the roles whose
// reports hold the round (backgroundHold). Nothing but the fields the wait names is set, and
// through is set for replacementReady alone.
type replacementReading struct {
	wait       replacementWait
	through    supervise.Claim
	unanswered []supervise.Claim
	held       []supervise.Claim
	working    []string
	background backgroundHold
}

// roundEnd is what a round's end reads of its claims: whether the round's bound has come, the
// claims the round asks that answered it (answeredClaims), the claims that hold the replacement
// for an answer (unanswered, unansweredClaims), the roles whose turns run, and the roles whose
// activity reports hold the round (backgroundClaims). It is what the line of a replacement the
// bound made names (replacePodForRound). lineWritten is whether that line is written already: the
// walk ends a round whose pod a launch replaced with the launch's reading at each sweep until the
// store finishes the replacement's record (endRoundALaunchReplaced), and only the first end writes
// the line (the Legion reviewer's item 1 on #5285 at 69e00593).
type roundEnd struct {
	past        bool
	answered    []supervise.Claim
	unanswered  []supervise.Claim
	working     []string
	background  backgroundHold
	lineWritten bool
}

// holding is what holds the round's pod replacement at this reading of the round's end: before
// the round's bound, the claims that have not answered it (unansweredClaims, which names a claim
// that a project's pause still keeps whether or not it has a session), the roles whose turns run,
// and the claims with a process in the round's pod whose report names background work that runs
// or that have sent no report since their last turn started; past the bound nothing. It is the one
// statement of that rule: dispatch://INFRA-2981 R2, as dispatch://INFRA-4203 R1 and
// dispatch://INFRA-4109 R8 change it. The walk's reading (readReplacement) and the last word under
// the pod's launch turn (replacementDue) read it here. The line of a replacement the bound made
// (bounded) reads the same three fields, unanswered, working and background, past the bound.
func (e roundEnd) holding() (unanswered []supervise.Claim, working []string, background backgroundHold) {
	if e.past {
		return nil, nil, backgroundHold{}
	}
	return e.unanswered, e.working, e.background
}

// bounded is whether the bound is what let the replacement through: the bound has come, and the
// same reading taken before the bound would have held the replacement (holding) for an answer
// not given, a turn that runs, or a report of background work or the lack of one.
func (e roundEnd) bounded() bool {
	return e.past && (len(e.unanswered) > 0 || len(e.working) > 0 || e.background.holds())
}

// readRoundEnd is the round's end as group reads at the clock's now, under the round's bound
// (roundClock.pastBound), with the claims that answered and the claims that hold the replacement
// for an answer read at the same clock (answeredClaims, unansweredClaims). Each of its sets is
// read whatever the clock says: holding drops them past the bound, and the line of a replacement
// the bound made names what each of them still held then.
func readRoundEnd(group []supervise.Claim, round string, clock roundClock) roundEnd {
	return roundEnd{
		past:       clock.pastBound(group, round),
		answered:   answeredClaims(group, round, clock),
		unanswered: unansweredClaims(group, round, clock),
		working:    workingClaims(group),
		background: backgroundClaims(group, round),
	}
}

// roundClock is what one issue's update round is read against at one moment: the moment itself
// (now), the project's pod_update_grace_minutes (grace), and what a project's pause did to the
// issue's pod (holds, podHolds). It answers the three questions the round's flow asks of a pause:
// whether a hold stands on the pod (held), when a round ends (roundBound, pastBound), and whether
// the round passes over a claim the pause stopped (passesOver). Each step of the flow takes this
// one value, never the moment and the holds apart.
//
// It is built once per reading of the claims: by the sweep for each issue, from the holds it read
// once and its own now (reconcilePodUpdates, podPauseHolds); by the replacement the sweep leaves
// for after its walk, from the same holds at the clock as it stands when that replacement is made
// (replacePodForRound); and by the last word, at the clock under the pod's launch turn
// (replacementDue). It holds no bound of its own: roundBound reads the round's opening from the
// group it is given, so each round it is asked of, one the sweep has just opened included
// (openPodUpdate, askRound), is bounded by that round's own opening.
type roundClock struct {
	now   time.Time
	grace time.Duration
	holds podHolds
}

// roundClockAt is the round clock of an issue at now, over the holds a project's pause has or had
// on the issue's pod, with this project's grace.
func (s *supervision) roundClockAt(now time.Time, holds podHolds) roundClock {
	return roundClock{now: now, grace: s.cfg.PodUpdateGrace, holds: holds}
}

// held is whether a project's pause holds the pod at this reading (podHolds.held): a round over
// it waits, and no round opens over it.
func (c roundClock) held() bool {
	return c.holds.held()
}

// roundBound is the moment the round ends whatever its roles say: its opening, the earliest its
// claims record (roundOpenedAt), plus pod_update_grace_minutes, plus the time a project's pause
// held the pod since the opening (podHolds.since, at the clock's now): the time so far of a hold
// that stands, and the span of each hold that ended, which for a hold that ended at the resume or
// at a role's ready runs on to the latest let-out of a launch its pause held for a role of the pod
// (heldUntil; decision 12 of dispatch://INFRA-3759). A hold that stands therefore moves the bound
// on as long as it stands, and the roles get their whole grace once it has ended and the wake
// pacing has let out every launch its pause held for them. It is nil where no claim of group
// records the opening, which no round's end is read past.
func (c roundClock) roundBound(group []supervise.Claim, round string) *time.Time {
	opened := roundOpenedAt(group, round)
	if opened == nil {
		return nil
	}
	bound := opened.Add(c.grace + c.holds.since(*opened, c.now))
	return &bound
}

// pastBound is whether the clock's now is at or past the round's bound (roundBound). No bound —
// no claim of group records when the round opened — is a bound that has not come.
func (c roundClock) pastBound(group []supervise.Claim, round string) bool {
	bound := c.roundBound(group, round)
	return bound != nil && !c.now.Before(*bound)
}

// passesOver is whether round passes over a claim, read over the clock's holds
// (dispatch://INFRA-4109 R1). It is the one statement of that rule: roundAsks leaves such a claim
// out of both of the round's sets, and notWaitedFor names it. It is every
// claim supervise's Claim.PassedOverByRound names, a suspension by the daemon's own rule, by a
// root's exit, by an idle release or by an unknown source, but the one exception: a claim that a
// project's pause still keeps (podHolds.owes, which reads the round for the claim's answer).
//
// A pause's stop suspends each role it stops as released (supervise.SuspendedByReleased) and
// keeps the pod for the work on its disk (decision 12 of dispatch://INFRA-3759). So such a claim
// is the pause's, not R1's, as the architect of dispatch://INFRA-4109 decided on 2026-11-19. The
// round asks it once its agent is ready, as it asks any claim that is not ready yet, and waits for
// its answer before the bound. The hold has already moved that bound (roundBound). A hold that
// stands holds the replacement whatever the answers (readReplacement's waitsForPause). How long
// the pause keeps the claim is the one exception's to say (podHolds.owes). A claim that the pause
// no longer keeps is R1's when the daemon suspends it, whatever another role of the pod still
// waits for. So is a claim the pause stopped that answered the round. It is in no wait of the round
// either way, because it answered (waitingClaims, unaskedClaims, notWaitedFor).
//
// The claim's machine still refuses an ask of a released claim (supervise's
// Machine.PublishUpdateAsk, UpdateAskSuspended), so nothing is published to a claim the pause
// keeps: a publish to a role with no holder raises a role wake, and the pause's resume, not the
// round, brings a stopped role back. The sweep asks such a claim only once its agent is ready
// anyway (askRound reads supervise.SaidReady), and a ready claim is no longer suspended.
func (c roundClock) passesOver(of supervise.Claim, round string) bool {
	return of.PassedOverByRound() && !c.holds.owes(of, round)
}

// readReplacement takes the seven readings that hold a round's pod replacement back over one live
// group, at the clock's now and under the round's bound (clock), and answers either the claim the
// round's pod is replaced through or the first reading that holds it. Both the walk that
// finds a round complete (completePodUpdate) and the replacement that walk leaves for afterwards
// (replacePodForRound) read it over claims each of them read live, so when a replacement waits is
// decided in one place; what either of them does with a wait — the line it logs, the notice it
// writes, whether it reports the round decided — is its own. It reads the claims and the clock —
// whether a project's pause holds the pod, from the holds the sweep read once (roundClock.held,
// podPauseHolds), and the round's bound — and nothing else, so neither reading costs a call to
// the runtime or the store.
//
// The order the readings are taken in is the rule. An operator's suspension comes first, because
// it is the one wait that is a person's and the one the bound never steps over
// (dispatch://INFRA-2981 R5): a person's hold does not yield to a clock. It holds a pod that is
// there, and only that: a hold over a round with no live claim that records a process in its
// pod, nor one whose launch waits on that pod (podProcess), is read as no process in the round's
// pod, carrying the claims the operator holds, so that its caller asks first whether the pod is
// there at all. A claim whose launch waits on the pod is a way to that pod as a process is, so a
// hold over a round that has one is the operator's wait (waitsForOperator). A round whose pod is
// gone ends whatever the operator's suspension says (endRoundOverAGonePod): there is no pod left
// to replace, and a wait for the resume of a claim on a pod that no longer exists is a wait for
// nothing. A claim that has not answered (unansweredClaims, which names a claim that a project's
// pause still keeps whether or not it has a session), a turn that runs, and a claim whose report
// names background work that runs or that has no report since its last turn hold the replacement
// only before the bound (roundEnd.holding), which is what makes the bound the end of every round
// nobody answers (R2) and of every round whose background work outlives it
// (dispatch://INFRA-4203 R1). No process in the round's pod and two pods at once are facts about
// the pod, not about any answer, so they hold this replacement whatever the clock says: there is
// nothing in such a pod to replace it through, or a replacement is already in flight. A pod with
// no process in it is replaced by issue instead where nothing holds it
// (roundWithNoProcessInItsPod, roundPodEnds).
//
// A project's pause's hold of the pod comes next, and is read the same way (decision 12 of
// dispatch://INFRA-3759): it holds a pod that is there whatever the clock says, and over a round
// no live claim records a process in the pod of it is read as no process in the round's pod, which
// its caller weighs with the hold (roundWithNoProcessInItsPod), so it asks first whether the pod
// is there at all.
//
// The reports come after the turn and before two pods at once. A turn that runs is named first,
// on the line of the turns, so the line of the reports is written only once no turn runs and
// names the background work alone.
//
// Gone comes before the turn: whether this round's pod is still there is asked before the round
// waits on a turn, on reported background work or on a replacement in flight, because each of
// those is a wait on that pod. The wait for an answer above it belongs to no pod: a round nobody
// has answered has lost nothing to tell anyone of, whether its pod is there or not. A round whose
// pod no live claim records has nothing in that pod to cut and nothing there to replace, so
// waiting for a turn — which by then is a turn in the pod that replaced this one — would hold
// every answered worker's next task, and the notice would name a pod that is gone. What is left
// to decide of such a round is read from the round's own record of its members
// (roundWithNoProcessInItsPod, lostClaims).
func readReplacement(live []supervise.Claim, round string, clock roundClock) replacementReading {
	through, reached := podProcess(live, round)
	if held := heldByOperator(live, round, clock); len(held) > 0 {
		if !reached {
			return replacementReading{wait: waitsForProcessInItsPod, held: held}
		}
		return replacementReading{wait: waitsForOperator, held: held}
	}
	if clock.held() {
		if !reached {
			return replacementReading{wait: waitsForProcessInItsPod}
		}
		return replacementReading{wait: waitsForPause}
	}
	unanswered, working, background := readRoundEnd(live, round, clock).holding()
	if len(unanswered) > 0 {
		return replacementReading{wait: waitsForAnswer, unanswered: unanswered}
	}
	if !reached {
		return replacementReading{wait: waitsForProcessInItsPod}
	}
	if len(working) > 0 {
		return replacementReading{wait: waitsForTurn, working: working}
	}
	if background.holds() {
		return replacementReading{wait: waitsForBackground, background: background}
	}
	if _, split := groupPod(live); split {
		return replacementReading{wait: waitsForPodInFlight}
	}
	return replacementReading{wait: replacementReady, through: through}
}

// roundReplacer is who makes a round's own replacement of its pod: the sweep through a process in
// the pod, or, where none runs there, through a claim whose launch waits on it (viaProcess,
// replacePodForRound, podProcess), the sweep by issue (viaIssue, replaceIssuePodForRound), or a
// launch into the pod (viaLaunch, endRoundALaunchReplaced). It is the one record of that fact. The
// sweep hands it from its walk to the step after it (podReplacement), and the read of the
// workspace carries it to the replacement's line and to the replacement's record (workspaceRead,
// roundReplaced, roundRecordOf). Who reads the workspace does not follow from it alone: the
// sweep's replacement through a claim whose launch waits on the pod has no role process to read
// through, so the pod's launcher reads, as for a replacement by issue (readPodWorkspace). The zero
// value is the replacement through a process.
type roundReplacer int

const (
	viaProcess roundReplacer = iota
	viaIssue
	viaLaunch
)

// String is the key that the replacement's line carries as true for a replacement by issue or at
// a launch (roundReplaced): "byIssue" or "atLaunch". It is "" for the sweep's replacement through
// a process or through a claim whose launch waits on the pod, whose line names no replacer.
func (r roundReplacer) String() string {
	switch r {
	case viaIssue:
		return "byIssue"
	case viaLaunch:
		return "atLaunch"
	}
	return ""
}

// podReplacement is the replacement the sweep's walk over the issues leaves for after the walk,
// beside the reads of kept pods (podUpdateSweep.keptReads): the pod of a round that is over is to
// be replaced (completePodUpdate), or the pod of a round that
// has no role process in it and that nothing holds (viaIssue: podlessRound,
// roundWithNoProcessInItsPod). It carries what the step cannot read again — the issue, the round,
// the pod the round was opened over, the reasons the round records, and which kind of replacement
// it is — and the pause's holds of the pod as the sweep read them, which the step's read before
// the pod's launch turn takes (replacePodForRound, replaceIssuePodForRound). The step's last word
// under that turn reads the holds again (replacementDue, issueReplacementDue). The step carries
// nothing of the claims or the clock, which it reads afresh at its own moments (roundClock), and
// it makes the read of the workspace before the replacement afresh too (readPodWorkspace):
// carried from the walk, either would be a second and older reading of what the step has just
// read again.
type podReplacement struct {
	issue   string
	round   string
	pod     string
	reasons string
	holds   podHolds
	// by is who makes the replacement. viaIssue is a replacement of a pod no claim records a
	// process in: the runtime is asked by the issue (replaceIssuePodForRound). viaProcess, the zero
	// value, asks it through a process (replacePodForRound). A launch makes its own replacement
	// (viaLaunch, endRoundALaunchReplaced), and the sweep hands none on.
	by roundReplacer
}

// replacePodForRound makes one replacement the sweep's walk over the issues decided on, after
// that walk (reconcilePodUpdates). It is the only thing in the sweep that waits on a pod: it
// takes that pod's launch turn and then waits the pod out, up to worker_boot_timeout_seconds,
// while every other issue's round, this tick's stale-plugin re-posts and the orphan reaping
// behind it wait. Made where it is decided, it would hold every issue after that one in the walk
// for those minutes, and the project's limit on how many rounds a minute may open would open one
// round per slow sweep rather than the rounds a minute the project asks for (podUpdateSweep).
//
// The claims are read again here, immediately before the call, because the walk's own read of
// them is by now older than it was when the round was found complete: every replacement this
// same sweep decided before this one has run since, each of them a pod boot long. The seven
// readings that hold a replacement back are taken again over that read and at the clock as it
// stands now, through a roundClock of its own over the holds the sweep read, by the one predicate
// the walk itself asked (readReplacement), and each of them gives this replacement up: an
// operator's suspension, a project's pause's hold of the pod (as the sweep read it,
// podPauseHolds), a claim that has not answered before the bound (unansweredClaims), no live claim
// recording a process in the round's own pod nor one whose launch waits on it (podProcess), a turn
// running in the issue before the bound, a claim with a process in the round's pod whose report
// names background work that runs or that has no report since its last turn before the bound, and
// claims recording two pods. This read takes each report from the store as it stands now, so a
// report that came after the walk's snapshot is weighed here (roundGroup). None of them ends
// anything: the round stays open, and the next sweep decides it afresh from claims it reads then.
// An operator's suspension and two pods at once are not stepped over by the bound, so each is told
// to the operator's side once the bound has come (tellLateRound), as the walk tells them; a
// pause's hold is told to nobody. No process in the round's pod is decided by the walk alone,
// which asks the runtime first whether the pod is there at all (roundWithNoProcessInItsPod) and
// then replaces it by issue or tells of it. That includes a round an operator's suspension or a
// pause holds with no live process in its pod: the hold is weighed only once that read finds the
// pod there, and a pod it finds gone ends the round whatever the hold.
//
// That read is no replacement for the daemon's last word inside the runtime (replacementDue),
// which it stands in front of rather than in place of: ReplacePod takes the pod's launch turn
// and waits it out, so a round complete at this read is still a round a worker can start a turn
// in, or an operator can suspend a claim of, while the call waits. The runtime asks that last
// word one statement before the write that ends every process in the pod, and a replacement no
// longer due is refused there, ends nothing and leaves the round open for the next sweep.
//
// The last word is asked only of a pod that is there. The Sandbox runtime asks it after it has
// found the pod the locator records still the Sandbox's own, and answers a pod that is gone by
// replacing nothing and returning nil: no operator's hold is read over a pod that is not there.
// Such a call took nothing, so the round ends as the act that took the pod (replaceForRound): as a
// launch's replacement of the pod as the round's own, whose record the walk finishes
// (overtakenByLaunch); as a launch's delete of the issue's Sandbox over the pod, whose record
// tells the architect (roundReplaced's yield); and, where no record of such an act stands, as a
// replacement made, from this call's record.
//
// Before that call, and outside the pod's launch turn that ReplacePod takes, the replacement reads
// the issue's workspace (readPodWorkspace, R7 of #5285) through the role of the
// process the pod is replaced through, since only a process in the pod can read the disk that the
// replacement takes from the pod. A pod replaced through a claim whose launch waits on it
// (podProcess) has no role process to read through, so the pod's launcher reads it, as for a
// replacement by issue. Every replacement reads afresh: a turn after a read voids it, so
// no read is kept for a later sweep. Before the round's bound the read is made only once every
// claim the round asks has answered and no turn runs (readReplacement); past the bound it is made
// while a turn may run, since a turn's change in the working copy is work. The read decides the
// replacement (roundKeeps): a read that finds nothing lets it go on, and a read that finds work
// that no branch or tag on origin holds, or that cannot be made, keeps the pod over a drift its
// roles can keep working under, and the round ends with no replacement (keepPodForRound), whatever
// the disk: a kept volume that the issue's next pod mounts is no exception. A round over a drift
// that breaks the pod (runtime.Breaks) is replaced whatever the read found. The read is
// bounded to one sweep interval, and a read its bound ends is a read that failed. The last word
// under the pod's launch turn refuses the replacement where a claim of the issue started a turn
// after the read began (replacementDue). What each replacement reads and names of a claim whose
// launch waits on the pod is written once, in the doc of lostWaitingClaims.
//
// ReplacePod is called once, with the process of a claim that records this round's own pod or,
// where none does, the locator of a claim whose launch waits on that pod (podProcess). A
// failed ReplacePod is logged and its record left for the next sweep, which finishes the
// replacement where the runtime shows the suspension landed, and forgets the record and decides
// the round afresh where it did not (finishReplacements): until then nothing is ended, so the round
// still holds every answered worker's next task. A ReplacePod that keeps failing holds the round
// past its bound, which only a person can end, so it is told to the operator's side once the
// bound has come (tellLateRound, failedReplacementReason) and names what holds the round. One
// round is told once.
//
// A replacement made is logged on one line that holds for a round that ended either way, by its
// answers or by its bound. It names each claim with a session that the round did not wait for,
// that had not answered and whose work may be on the pod's disk (notWaitedFor): a failed claim,
// and a claim the round passed over. Beside them, under waitingOnThePodNotWaitedFor, it names each
// claim whose launch waits on the round's own pod, that had not answered and whose work may be on
// the pod's disk (unwaitedOnThePod): the round waits on no answer of such a claim either
// (waitingClaims). Whether a claim's work may be on the pod's disk is workOnRoundPod's one
// statement. The line names too what the read of the workspace found
// (workspaceRead.attrs). One that the bound made — a claim had not answered, a turn ran, or a
// claim's report named background work or was missing, when the last word let it through — is
// logged on a second line of its own, `supervise pod update replaced at the bound`, naming the
// claims that answered and those that did not, the turns that ran, and apart from them the roles
// whose reports held the round (dispatch://INFRA-2981 R8, whose production check reads those
// words; dispatch://INFRA-4203 R4). That line is written only where the last word ran and let the
// replacement through. A runtime that found no pod to replace and returned nil without asking the
// last word replaced nothing, so the bound replaced nothing either, and such an end writes the
// first line only where it ends the round as a replacement made (above). What either line names a
// claim as having said is read from the claims as they hold it (told, and in the last word's
// ended), never masked: the mask holds a replacement back for a sweep, and is no answer a claim
// withdrew (noticeGroup).
//
// Where the read found work that no branch or tag on origin holds, or failed, and the replacement
// is made all the same, the issue's architect is told what it took (roundReplaced,
// finishReplacement), and the round ends only once that notice is queued. The notice names the
// claims notAskedToPush gives, or, where the pod was replaced through a claim whose launch waits on
// it and no role process ran in the pod, the members lostClaims gives. How the decision, the read,
// the line and the notice each read the claims is written once, in masked. A replacement the last
// word refused tells nobody, and its record goes. The read, the keep, the record, the call and
// the end after it are the replacement by issue's too, in one function (replaceForRound).
//
// The call is bounded to one sweep interval. The suspension that displaces the pod may have landed
// when the bound hits, or when the call fails otherwise. The record written before the call stands,
// and the next sweep has the runtime read the act under the pod's launch turn before it decides the
// issue (finishReplacements): a suspension that landed ends the round as this replacement, from the
// read made before it, and the pod is read no more and never kept.
func (s *supervision) replacePodForRound(ctx context.Context, replace podReplacement) {
	issue, round, pod, reasons := replace.issue, replace.round, replace.pod, replace.reasons
	// The read's moment is taken before the claims are read, so a turn that starts after the read
	// of the claims is a turn after the read too (the Legion reviewer's point 1).
	at := s.plan.clock.Now()
	claims, err := s.supervisor.Claims(ctx)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Error("supervise: read this issue's claims again before its update round's replacement; nothing was replaced and the round stays open",
				"issue", issue, "round", round, "pod", pod, "error", err)
		}
		return
	}
	told, live := s.liveGroup(claimsOf(claims, issue))
	clock := s.roundClockAt(s.plan.clock.Now(), replace.holds)
	reading := readReplacement(live, round, clock)
	switch reading.wait {
	case waitsForOperator:
		s.log.Info("supervise: an operator's suspension holds this update round now the sweep has reached its replacement; nothing was replaced and the round stays open",
			"issue", issue, "round", round, "pod", pod, "held", namesOf(reading.held))
		s.tellLateRound(ctx, issue, live, round, pod, reasons, operatorHoldReason(reading.held), clock)
		return
	case waitsForPause:
		s.log.Info("supervise: a project's pause holds this update round's pod now the sweep has reached its replacement; nothing was replaced and the round stays open",
			"issue", issue, "round", round, "pod", pod)
		return
	case waitsForAnswer:
		s.log.Info("supervise: a worker of this update round has not answered, or its machine was deciding something, now the sweep has reached its replacement, and the round's bound has not come; nothing was replaced and the round stays open",
			"issue", issue, "round", round, "pod", pod, "waiting", namesOf(reading.unanswered))
		return
	case waitsForProcessInItsPod:
		s.log.Info("supervise: no claim of this issue records a process in this update round's pod now the sweep has reached its replacement; nothing was replaced and the round stays open",
			"issue", issue, "round", round, "pod", pod, "held", namesOf(reading.held))
		// The walk asks the runtime whether this pod is there before it tells anyone of it or
		// weighs an operator's hold, and the next sweep's walk decides this round
		// (roundWithNoProcessInItsPod).
		return
	case waitsForTurn:
		s.log.Info("supervise: a turn of this issue runs now the sweep has reached this update round's replacement, and the round's bound has not come; nothing was replaced and the round stays open",
			"issue", issue, "round", round, "pod", pod, "working", reading.working)
		return
	case waitsForBackground:
		s.log.Info("supervise: a role of this issue reports background work now the sweep has reached this update round's replacement, and the round's bound has not come; nothing was replaced and the round stays open",
			append([]any{"issue", issue, "round", round, "pod", pod}, reading.background.attributes()...)...)
		return
	case waitsForPodInFlight:
		s.log.Info("supervise: the claims of this issue run in two pods now the sweep has reached this update round's replacement; nothing was replaced and the round stays open",
			"issue", issue, "round", round, "pod", pod)
		s.tellLateRound(ctx, issue, live, round, pod, reasons, twoPodsReason, clock)
		return
	case replacementReady:
		// The replacement below is this one reading's; every other one has returned by here.
	default:
		s.log.Error("supervise: this update round's replacement was held back by a reading this sweep cannot name; nothing was replaced and the round stays open",
			"issue", issue, "round", round, "pod", pod, "reading", int(reading.wait))
		return
	}
	// The decision (readReplacement, replacementDue) reads the claims masked (live), because what
	// it lets through deletes the pod's disk (masked). The keep (keepPodForRound), the line (named,
	// roundReplaced) and the notice (lost) read them as the claims hold their answers (told): the
	// read stops nothing in the pod, and the line and the notice report what each claim answered.
	// A pod replaced through a claim whose launch waits on it has no role process to read the
	// workspace through, so the pod's launcher reads it (readPodWorkspace), and the notice names
	// the members lostClaims names, as the notice of a replacement by issue does
	// (lostWaitingClaims).
	earlierWork := s.roundPodHoldsEarlierWork(ctx, issue, round)
	process, lost := podKnown(reading.through), notAskedToPush(told, round, clock, earlierWork)
	if awaitsRoundPod(reading.through, round) {
		process, lost = runtime.Known{}, lostClaims(told, round, earlierWork)
	}
	s.replaceForRound(ctx, replace, at, told, live, clock, roundCall{
		process:     process,
		name:        "replacement through a process",
		notDue:      "supervise: this update round's replacement was no longer due when its pod's launch turn came; nothing was replaced and the round stays open",
		failed:      "supervise: replace the pod of a finished update round; retrying at the next sweep, which finishes the replacement from its record where its suspension landed",
		due:         s.replacementDue,
		named:       notWaitedFor(told, round, clock, earlierWork),
		lost:        lost,
		earlierWork: earlierWork,
		call: func(ctx context.Context, due func(context.Context) error) error {
			return s.runtime.ReplacePod(ctx, podKnown(reading.through), due)
		},
	})
}

// roundCall is what one of a round's two calls of the runtime differs by, as its caller builds it
// (replaceForRound; the Legion reviewer's fix 3 on #5285 at 4b123279): through a process
// (replacePodForRound), and by issue (replaceIssuePodForRound).
type roundCall struct {
	// process reads the issue's workspace: the process of a claim that records the round's own pod,
	// or the zero runtime.Known, for the pod's launcher, by issue or through a claim whose launch
	// waits on the pod (readPodWorkspace).
	process runtime.Known
	// name is the call's name on the line that says a launch overtook it, notDue the line of a last
	// word that refused, and failed the line of a call that failed otherwise.
	name, notDue, failed string
	// due is the last word the runtime asks under the pod's launch turn (replacementDue,
	// issueReplacementDue), built over the round, the moment the read began and the end it reads.
	due func(issue, round string, readAt time.Time, ended *roundEnd) func(context.Context) error
	// named is the claims the round's lines name: the claims the round did not wait for
	// (notWaitedFor), or the members that had not answered it (lostClaims). lost is the claims the
	// architect's notice names: the claims no role was asked to push for (notAskedToPush), or the
	// members lostClaims names. earlierWork is roundPodHoldsEarlierWork's answer, which both were
	// read over and the line reads again (unwaitedOnThePod).
	named, lost []supervise.Claim
	earlierWork bool
	// call is the runtime's call (ReplacePod, or runtime.IssuePodReplacer's ReplaceIssuePod), with
	// the last word it asks.
	call func(ctx context.Context, due func(context.Context) error) error
}

// replaceForRound is the one tail of a round's two calls of the runtime, once the call has read the
// issue's claims again and found its replacement due (the Legion reviewer's item 3 on #5285 at
// 69e00593): through a process (replacePodForRound), and by issue (replaceIssuePodForRound). What
// the two differ by is one value its caller builds (roundCall): who reads the workspace, which last
// word the runtime asks under the pod's launch turn, which claims the round's lines and notice name,
// what the call's lines say, and the runtime's call it makes (the Legion reviewer's fix 3 on #5285
// at 4b123279). replace.by, who makes the replacement (roundReplacer), is carried on the read to
// the replacement's line and notice (workspaceRead). at is the moment the read began, taken before
// the claims were read again; told and live are those claims, unmasked and masked (liveGroup); and
// clock is the moment they were read at.
//
// The read decides the replacement (roundKeeps): a read that keeps the pod ends the round with no
// replacement (keepPodForRound). Otherwise the replacement's record is written before the call
// (recordRoundReplacement), so the last word is the last decision before the suspension, and a last
// word that refuses takes it away again (recordedDue), which also says whether the runtime asked
// the last word (asked). The call is bounded to one sweep interval. A last word that refuses is the
// daemon's own and is logged only, and the round stays open. A call that fails otherwise is logged
// and its record left for the next sweep (finishReplacements), and past the bound it holds the
// round, so it is told to the operator's side once (tellLateRound, failedReplacementReason).
//
// A call that answers nil made the replacement, or found the round's pod already taken and asked
// no last word. Such a call took nothing, and the round ends as the act that did take the pod: as
// a launch's replacement of the pod as the round's own, whose record the walk finishes, and this
// call's record goes (overtakenByLaunch); or as a launch's delete of the issue's Sandbox over the
// pod, whose record tells the architect, and the round's finish yields to it (roundReplaced). Where
// no record of such an act stands, a nil ends the round as the replacement made (roundReplaced).
func (s *supervision) replaceForRound(ctx context.Context, replace podReplacement, at time.Time, told, live []supervise.Claim, clock roundClock,
	rc roundCall) {
	issue, round, pod, reasons := replace.issue, replace.round, replace.pod, replace.reasons
	read := s.readPodWorkspace(ctx, issue, round, rc.process, replace.by, at)
	read.origin = roundRepository(told, round)
	if roundKeeps(reasons, read) {
		s.keepPodForRound(ctx, issue, round, pod, reasons, read, told)
		return
	}
	// The replacement's record, written before the call, so the last word the runtime asks under
	// the pod's launch turn is the last decision before the suspension (recordRoundReplacement), and
	// finished with the architect's notice once the replacement is made (roundReplaced). A last word
	// that refuses takes it away again (recordedDue), and a failed call leaves it for the next sweep
	// to finish or forget (finishReplacements).
	recorded, ok := s.recordRoundReplacement(ctx, issue, round, pod, reasons, read)
	if !ok {
		return
	}
	limited, done := context.WithTimeout(ctx, s.plan.orphanSweep)
	defer done()
	// ended is what the last word read of the round's end under the launch turn, which is the
	// read the replacement is made on. It stays the zero roundEnd, which no bound made, where the
	// runtime returns nil without asking the last word (asked false): the Sandbox runtime does that
	// for a Sandbox that is absent or suspended, or whose pod is no longer the one the call names,
	// and replaces nothing there.
	var (
		ended roundEnd
		asked bool
	)
	if err := rc.call(limited, s.recordedDue(recorded, &asked, rc.due(issue, round, read.at, &ended))); err != nil {
		if errors.Is(err, errReplacementNotDue) {
			s.log.Info(rc.notDue, "issue", issue, "round", round, "pod", pod, "reason", err)
			return
		}
		if ctx.Err() == nil {
			// The suspension may have landed before the call failed: the record written before the
			// call stands, and the next sweep finishes the replacement from it where the runtime
			// shows the pod taken, or forgets it (finishReplacements).
			s.log.Error(rc.failed, "issue", issue, "round", round, "pod", pod, "error", err)
			s.tellLateRound(ctx, issue, live, round, pod, reasons, failedReplacementReason(err), clock)
		}
		return
	}
	if s.overtakenByLaunch(ctx, recorded, round, asked, rc.name) {
		return
	}
	s.roundReplaced(ctx, issue, round, pod, reasons, ended, rc.named, rc.lost, read, told, rc.earlierWork, recorded, !asked)
}

// overtakenByLaunch is whether a round's replacement whose call answered nil took nothing because
// a launch replaced the round's pod as the round's own replacement while the call waited for the
// pod's launch turn: the call asked no last word (asked false), since the Sandbox no longer ran
// the round's pod, and this daemon remembers that launch (launchReplaced). The act is then the
// launch's, under its own record, and the walk ends the round as that replacement
// (endRoundALaunchReplaced), told from the launcher's read at the launch. So the call's record,
// recorded, goes, and the architect is told of the one act once, from that read (the Legion
// reviewer's Important 1 on #5285 at 7da847c0, trace a, for a replacement by issue, and its
// Important 2 at acda4d1e, for a replacement through a process). which names the call on the line.
func (s *supervision) overtakenByLaunch(ctx context.Context, recorded store.PodReplacement, round string, asked bool, which string) bool {
	if _, launched := s.launchReplaced(recorded.Issue, round); asked || !launched {
		return false
	}
	s.forgetReplacement(ctx, recorded)
	s.log.Info("supervise: a launch replaced the pod of this update round as the round's own replacement while the round's call waited; the call took nothing, and the round ends as the launch's",
		"issue", recorded.Issue, "round", round, "pod", recorded.PodUID, "call", which)
	return true
}

// roundReplaced is what each of a round's own replacements owes once it is made: through a process
// (replacePodForRound), by issue (replaceIssuePodForRound), at a launch (endRoundALaunchReplaced),
// or one whose call failed or whose daemon stopped once its suspension landed (endRecordedRound).
// It logs the bound's own line where the bound let the replacement through (ended, what the last
// word or the launch read of the round's end under the pod's launch turn), since the replacement
// is made by then, finishes the replacement's record, recorded, telling the issue's architect what
// the replacement took where the read that tells most of the records the finish takes found work or
// failed (finishReplacement, tellingRecord), and only then logs the replacement's line and ends the
// round on the claims of members (endPodUpdates). A store that refuses the finish
// leaves the round open with its record (the Legion reviewer's point D on fa9deb04): the notice
// names what the round's claims record — the members that had not answered, and whether the kept
// volume was still the round's — and the end clears it, so a later sweep finishes the record from
// the round as it stands, and ends it then (finishReplacements, or the walk's end of a round a
// launch replaced). It answers what the finish did, and whether this finish took the record and
// the round is over: false where the store refused the finish, and where the finish of another
// act on the pod took the record first and ended the round as that act (endTakenRound).
//
// yield is a round's call that answered nil without asking its last word (asked false): it found
// the pod taken already, and took nothing. Where the record of a launch's delete of the issue's
// Sandbox and its kept volume over that pod stands (runtime.TakenPod.WithPod), that launch took
// the pod, so the finish yields to it (store.Store.FinishPodReplacement): the round queues no
// notice and ends, and in the finish's transaction the round's read moves onto the launch's record
// where it is the read that tells of the two, by the rule of every notice (movedRead,
// tellingRecord), and the round's record goes. The launch's record alone then stands for the one
// act: its finish tells the architect from the read that tells most of the records it takes once
// that act is confirmed (the Legion reviewer's Medium 1 on #5285 at 4b123279), and where the
// runtime shows the act not made, the record is forgotten and nothing is told of a call that took
// nothing (the Legion reviewer's Medium 1 on #5285 at 18d81dad). The end is the round's end all
// the same, so within one daemon run no later walk reads the round's pod as one that went without
// its round (the Legion reviewer's Important on #5285 at 69e00593, path a). The end is posted to the
// members' machines and kept in memory (endPodUpdates, keepEndedRound), and the yield deleted the
// round's record: a daemon that stops after the yield commits and before every member's machine has
// taken the end boots with the round restored from the claims and no record holding the issue, so
// its walk can end the round again as a pod that went, and the launch's later finish tells the
// architect as well. Every round's own finish has the same window (the Legion reviewer's finding on
// podupdate.go:1416 on #5285 at aae2c628).
//
// read is what the read of the workspace before the replacement found (workspaceRead), and read.by
// is who made the replacement (roundReplacer). The line of a replacement by issue or at a launch
// carries its replacer's key as true right after atTheBound, "byIssue" or "atLaunch"; the line of
// the replacement through a process, or through a claim whose launch waits on the pod, carries
// none. named is the claims the line names: under notWaitedFor where read.by is viaProcess, with
// the claims unwaitedOnThePod gives of members beside them under waitingOnThePodNotWaitedFor, and
// under unansweredMembers otherwise. lost is the claims the architect's notice names
// (workspaceTakenMessage). earlierWork is roundPodHoldsEarlierWork's answer, which
// unwaitedOnThePod reads. Where the pod's disk is a kept volume that is still there and that the
// issue's next launch keeps (roundKeptVolume, read once the replacement is made, which may have
// marked the volume for a reset), the line carries keptVolume and the notice says the work is on
// that volume.
func (s *supervision) roundReplaced(ctx context.Context, issue, round, pod, reasons string, ended roundEnd,
	named, lost []supervise.Claim, read workspaceRead, members []supervise.Claim, earlierWork bool, recorded store.PodReplacement,
	yield bool) (store.PodReplacementFinish, bool) {
	bounded := ended.bounded()
	kept := s.roundKeptVolume(issue, members, round)
	line := []any{"issue", issue, "round", round, "pod", pod, "atTheBound", bounded}
	if read.by == viaProcess {
		// The round waited for no claim whose launch waits on its own pod either, since no
		// process of such a claim can answer (waitingClaims), and the line names the ones
		// unwaitedOnThePod gives.
		line = append(line, "notWaitedFor", namesOf(named),
			"waitingOnThePodNotWaitedFor", namesOf(unwaitedOnThePod(members, round, earlierWork)))
	} else {
		line = append(line, read.by.String(), true, "unansweredMembers", namesOf(named))
	}
	if kept {
		line = append(line, "keptVolume", true)
	}
	// The repository facts of the notice: the repository the round's pod was built from, as the
	// replacement's record holds it (workspaceRead.origin), and the repository the issue's record
	// holds now, read here rather than in the notice: the store builds the notice inside the
	// transaction that finishes the record (finishReplacement), which waits on no other read. A
	// retry of a refused finish, by a later sweep or after a restart, reads the record again.
	moved := s.roundMove(ctx, issue, read.origin)
	message := func(told recordedRead) string {
		return workspaceTakenMessage(issue, round, pod, reasons, lost, told.work, told.err, kept, moved)
	}
	// The bound's line comes before the finish: the replacement is made, and a retry of a refused
	// finish (finishReplacements) carries no reading of the bound, so this is the line's one chance.
	// The walk's retry of a launch's replacement carries the launch's reading again, which says the
	// line is written (roundEnd.lineWritten, endRoundALaunchReplaced).
	if bounded && !ended.lineWritten {
		s.log.Info("supervise pod update replaced at the bound",
			append([]any{"issue", issue, "round", round, "pod", pod,
				"answered", namesOf(ended.answered), "unanswered", namesOf(ended.unanswered), "working", ended.working},
				ended.background.attributes()...)...)
	}
	// The round is over only once its replacement's record is finished, in the transaction that
	// queues the architect's notice: a store that refuses that notice leaves the round open, and
	// each later sweep tries again, so the line that says the round is over comes once, after the
	// finish commits. A record the finish of another act on the pod took is finished already, and
	// that finish ended the round as its act (endTakenRound), so it is ended no second time here,
	// and nor is a round that is over already (roundOver).
	finish, err := s.finishReplacement(ctx, recorded.Operation, issue, pod, yield, message, "round", round)
	switch {
	case err != nil:
		s.log.Info("supervise: the pod of this update round is replaced, and the round stays open: the store refused to finish the replacement's record and to queue the architect's workspace-taken notice with it; a later sweep finishes it",
			"issue", issue, "round", round, "pod", pod)
		return finish, false
	case !finish.Found:
		s.log.Info("supervise: the record of this update round's replacement was taken by the finish of another act on its pod, which ends the round as that act",
			"issue", issue, "round", round, "pod", pod)
		return finish, false
	case finish.Yielded:
		s.log.Info("supervise: a launch's delete of the issue's Sandbox and its kept volume took the pod of this update round before the round's call; the call took nothing, and the round is over with no notice of its own, since the launch's finish tells the architect from the read of this record and its own",
			"issue", issue, "round", round, "pod", pod)
	case s.roundOver(issue, members, round):
		s.log.Info("supervise: the record of this update round's replacement is finished, and the round was over already", append(line, read.attrs()...)...)
		return finish, true
	default:
		s.log.Info("supervise: the pod of this update round is replaced and the round is over", append(line, read.attrs()...)...)
	}
	s.endPodUpdates(issue, members, round)
	return finish, true
}

// workspaceRead is what the read of an issue's workspace before one of a round's replacements
// answered (readPodWorkspace): the round it was made for, who makes the replacement (by,
// roundReplacer), the role whose process made it ("" for the pod's launcher), the moment it began
// (at), which the last word weighs a later turn against (replacementDue), and what it found or
// the error it failed with. origin is the repository the round's pod was built from, as the round
// recorded it at its open (roundRepository; the zero Repository where the pod named none): origin
// in the pod the read ran in, whose branches and tags the read weighs the work against. The caller
// sets it before the replacement's record is written, which holds it (replacementRecord), so every
// finish of the replacement, after a restart too, names the work by that repository where the
// issue's record has left it (roundMove, workspaceTakenMessage).
type workspaceRead struct {
	round  string
	by     roundReplacer
	role   claim.Role
	at     time.Time
	work   runtime.WorkspaceWork
	err    error
	origin ghrepo.Repository
}

// clean is whether the read was made and found nothing that origin does not hold: the one read a
// replacement over a drift the pod's roles can keep working under goes on over (roundKeeps).
func (r workspaceRead) clean() bool {
	return r.err == nil && !r.work.HoldsWork()
}

// The errors of a read that this runtime cannot make (readPodWorkspace). People and agents read
// them, so they follow ASD-STE100.
var (
	errNoRoleReader     = errors.New("this runtime cannot read the issue workspace through a role")
	errNoLauncherReader = errors.New("this runtime cannot have the pod's launcher read the issue workspace")
)

// readPodWorkspace is the read of the issue's workspace before one of round's replacements, with
// R5's count (R7 of #5285): through the role of process, the claim whose process
// in the round's pod the pod is replaced through (runtime.WorkspaceWorkReader), and through the
// pod's launcher where no role process runs in the pod, which process is then the zero
// runtime.Known (runtime.PodWorkspaceReader, R6): by issue, at a launch, and through a claim whose
// launch waits on the pod (podProcess). by is who makes the replacement (roundReplacer), which the
// read carries to the replacement's line and record and which chooses no reader: the sweep's
// replacement through a claim whose launch waits on the pod is viaProcess and reads through the
// launcher. It is also the read that may end an earlier round's keep of the pod (podKeptForWork),
// through a live role in the pod where one runs, else through the launcher. Its moment is at,
// which the caller takes before it reads the claims the read is decided on, so a turn that starts
// after that reading, or while the read runs, is a turn after it (the Legion reviewer's point 1).
// A runtime that cannot make the read, a claim that names no role, and a launcher's read of
// another pod than the round's are each a read that failed. The read is bounded to one sweep
// interval, and a read its bound ends is a read that failed.
func (s *supervision) readPodWorkspace(ctx context.Context, issue, round string, process runtime.Known, by roundReplacer, at time.Time) workspaceRead {
	read := workspaceRead{round: round, by: by, at: at}
	limited, done := context.WithTimeout(ctx, s.plan.orphanSweep)
	defer done()
	if process != (runtime.Known{}) {
		reader, ok := s.runtime.(runtime.WorkspaceWorkReader)
		role, named := process.Claim.Role()
		switch {
		case !ok:
			read.err = errNoRoleReader
		case !named:
			read.err = fmt.Errorf("the claim %s names no role to read the workspace through", process.Claim)
		default:
			read.role = role
			read.work, read.err = reader.WorkspaceWork(limited, issue, role)
		}
		return read
	}
	reader, ok := s.runtime.(runtime.PodWorkspaceReader)
	if !ok {
		read.err = errNoLauncherReader
		return read
	}
	read.work, read.err = reader.PodWorkspaceWork(limited, issue)
	if pod := strings.TrimPrefix(round, supervise.PodRoundPrefix); read.err == nil && read.work.PodUID != "" && read.work.PodUID != pod {
		read.err = fmt.Errorf("the pod's launcher read pod %s, not the round's pod %s", read.work.PodUID, pod)
	}
	return read
}

// attrs is the read as the replacement's line names it (replacePodForRound): who made it, the role
// or the pod's launcher, and what it found — the working-copy commit, whether it held a change,
// the first work commits and how many there are in all — or the error it failed with.
func (r workspaceRead) attrs() []any {
	by := "launcher"
	if r.role != "" {
		by = string(r.role)
	}
	if r.err != nil {
		return []any{"workspaceRead", "failed", "workspaceReadBy", by, "workspaceReadError", r.err.Error()}
	}
	found := "found no work"
	if r.work.HoldsWork() {
		found = "found work"
	}
	return []any{"workspaceRead", found, "workspaceReadBy", by, "workspaceHead", r.work.Head,
		"workspaceChanged", r.work.Changed, "workspaceCommits", r.work.Commits, "workspaceCommitCount", r.work.Count}
}

// boundPastReason opens every reason the late-round notice gives (tellLateRound,
// tellPodlessRound): the notice is told only once the round's bound has come.
const boundPastReason = "The bound of the update round is past. "

// The reasons the late-round notice gives for the three readings that hold a round past its bound
// and name no claim of their own (tellLateRound). The controller reads them, so they follow
// ASD-STE100. No process in the round's pod holds a round past its bound only where the runtime
// replaces no pod by issue (runtime.IssuePodReplacer, roundWithNoProcessInItsPod): where it does,
// the walk replaces the pod once the bound has come, and only a refusal of that replacement is
// told (replaceIssuePodForRound, failedReplacementReason). A pod the runtime cannot read decides nothing, so
// the round waits for a read of it that succeeds (tellUnreadPod).
const (
	noProcessReason = boundPastReason + "No claim of the issue records a process in the pod of the round. The daemon has no process to replace the pod through."
	twoPodsReason   = boundPastReason + "The claims of the issue record two pods. A replacement is in flight, and the round waits for it."
	unreadPodReason = boundPastReason + "The daemon cannot read the pod of the round from the runtime. The round waits until the daemon can read the pod."
)

// operatorHoldReason is the reason the late-round notice gives for a round an operator's
// suspension holds past its bound, naming each claim the suspension holds.
func operatorHoldReason(held []supervise.Claim) string {
	return fmt.Sprintf(boundPastReason+"An operator suspended %s. The daemon does not replace the pod until the operator resumes each of them.",
		roleStates(held))
}

// failedReplacementReason is the reason the late-round notice gives for a round whose pod
// replacement failed past the bound, through a process or by issue (replacePodForRound,
// replaceIssuePodForRound), naming the runtime's error. The call can fail after its suspension
// landed (the Legion reviewer's minor point 5 on fa9deb04), so the notice says the pod may be
// gone, and that a later sweep ends the round as that replacement if it is (finishReplacements).
// The notice is stored, and the controller reads its reason and publishes it, so the error goes in
// as data, as readError keeps a read's error: bounded as a far side's text and one quoted string,
// labelled as the runtime's, which no line or word of it can leave (GPT-6.1 Sol's point 5 on #5285
// at 0259f8af; the Legion reviewer's minor point 8). The caller's error line logs it whole.
func failedReplacementReason(err error) string {
	return boundPastReason + "The pod replacement failed. The runtime can fail after it suspended the pod. So the pod may be gone, " +
		"and if it is, the daemon ends the round as that replacement at a later sweep. The error of the runtime, in quotes, is: " + readError(err) + "."
}

// roundWithNoProcessInItsPod decides a round, before or past its bound, whose live claims record
// no process in the round's own pod, and no launch of which waits on that pod (podProcess).
// There is nothing to replace the pod through, and that is no statement about the pod: a
// suspension lets the worker's process go (letGo clears the locator), and the relaunch of a
// claim can run for minutes of a fleet roll. Read as a replacement made, it would end the round
// over a pod that is still there and still drifted, which is the third way out of a round that
// must not exist — the task the round was holding would go out, and the next tick would open the
// round over the same pod again.
//
// So the runtime is asked about the pod itself, through the claim the sweep's snapshot still
// records it on, and only runtime.ErrPodGone ends the round, through endRoundOverAGonePod, as
// every answer of the runtime that a round's pod is gone ends one (groupDrift's podGone,
// podlessRound). The other reading of a gone pod, claims recording the pod in its place, ends its
// round before this is reached, by the one rule for a round whose pod went
// (reconcileIssuePodUpdate, endRoundWhosePodWent). What the pod has drifted to says nothing
// here: the round is open, and its recorded reasons are what its ask and its notice state.
//
// A read that could not be made leaves the round where it stands for the next sweep to decide
// from claims it reads afresh: nothing is ended, and nothing is replaced. Past the bound such a
// read holds the round, so the operator's side is told of it once (tellUnreadPod,
// unreadPodReason). A pod that is there with no claim recording a process in it, and that nothing
// holds (roundPodEnds), is replaced by issue after the sweep's walk (replacementByIssue,
// replaceIssuePodForRound), and that replacement ends the round: a launch into the pod before then
// would have its first turn cut by it (dispatch://INFRA-4109 R7), so a launch that comes first
// reads the same rule as it runs and replaces the pod itself (roundPodEndsAtLaunch). Where
// something holds the pod (roundPodEnds), the round waits. A runtime that replaces no pod by issue
// (runtime.IssuePodReplacer) leaves the round waiting too, and such a pod holds the round past its
// bound, so the operator's side is told of it once the bound has come (tellLateRound,
// noProcessReason).
//
// held is the claims of the round an operator's suspension holds (readReplacement), and it is
// weighed here, after the pod is read, never before. A gone pod ends its round whatever the
// hold: the operator's suspension holds a pod that is there, and there is no pod left to wait
// for the resume of. A pod that is there keeps the hold in its place: the round waits for the
// operator as waitsForOperator's does (waitForOperator), the notice names the suspension rather
// than the missing process, and the round is not decided, so the post keeps it recorded and asks
// a claim it has not asked yet. A read that could not be made leaves such a round undecided too,
// and past the bound the notice names the operator's hold in place of the unread pod
// (tellUnreadPod): one round is told once, and only that operator can end the hold.
//
// clock carries what a project's pause did to the pod, which the round's bound moves by
// (roundClock.roundBound), and a hold that stands (roundClock.held) is weighed the same way,
// after the operator's: a pod that is there keeps the round waiting, undecided, and the round
// tells nobody of it (decision 12 of dispatch://INFRA-3759). A pod it could not read under such
// a hold tells nobody either.
//
// The snapshot always records a process in that pod, or a claim whose launch waits on it
// (podLocator), and that claim is the one this asks through: the sweep decides a round here only
// over the pod its claims record, and ends a round over any other pod before it gets this far
// (reconcileIssuePodUpdate).
//
// It reports the replacement by issue it leaves the sweep, if any, and whether it decided the
// round (completePodUpdate).
func (s *supervision) roundWithNoProcessInItsPod(ctx context.Context, issue string, snapshot, live []supervise.Claim, round, pod, reasons string,
	held []supervise.Claim, clock roundClock) (*podReplacement, bool) {
	recorded, _ := podProcess(snapshot, round)
	switch _, err := s.runtime.PodDrift(ctx, podKnown(recorded)); {
	case errors.Is(err, runtime.ErrPodGone):
		s.endRoundOverAGonePod(ctx, issue, snapshot, round, pod)
		return nil, true
	case err != nil:
		if ctx.Err() == nil {
			s.log.Error("supervise: read whether the pod of an update round is still there; the round stays open and the next sweep reads it again",
				"issue", issue, "round", round, "pod", pod, "error", err)
			s.tellUnreadPod(ctx, issue, live, round, pod, reasons, held, clock)
		}
	case len(held) > 0:
		s.waitForOperator(ctx, issue, live, round, pod, reasons, held, clock)
	case clock.held():
		s.log.Info("supervise: no claim of this update round records a process in its pod, and a project's pause holds the pod; the round waits for the hold to end",
			"issue", issue, "round", round, "pod", pod)
	default:
		if replace := s.replacementByIssue(issue, live, round, pod, reasons, clock); replace != nil {
			return replace, true
		}
		s.log.Info("supervise: no claim of this update round records a process in its pod now; the pod is there, and the round waits for a claim to record it again",
			"issue", issue, "round", round, "pod", pod)
		s.tellLateRound(ctx, issue, live, round, pod, reasons, noProcessReason, clock)
	}
	return nil, len(held) == 0 && !clock.held()
}

// waitForOperator is the walk's part of a round an operator's suspension holds over a pod that
// is there (completePodUpdate's waitsForOperator, and roundWithNoProcessInItsPod once it has read
// the pod there): the line, and the notice naming the suspension once the bound has come. The
// replacement waits for the operator's resume, whatever the round's bound.
func (s *supervision) waitForOperator(ctx context.Context, issue string, live []supervise.Claim, round, pod, reasons string,
	held []supervise.Claim, clock roundClock) {
	s.log.Info("supervise: an operator's suspension holds this update round; the replacement waits for the operator's resume, whatever the round's bound",
		"issue", issue, "round", round, "pod", pod, "held", namesOf(held))
	s.tellLateRound(ctx, issue, live, round, pod, reasons, operatorHoldReason(held), clock)
}

// tellUnreadPod is the walk's part of a round over a pod the runtime could not read: the drift
// read of the pod the claims record (reconcileIssuePodUpdate, groupDrift's podUnread), or the read
// of the round's own pod where no live claim records a process in it (roundWithNoProcessInItsPod).
// Such a read decides nothing: nothing ends, nothing is replaced, and the next sweep reads the pod
// again. A runtime that keeps failing that read therefore holds the round past its bound, and only
// a person can find out why, so the operator's side is told of it once the bound has come
// (tellLateRound, unreadPodReason; dispatch://INFRA-4109 R5).
//
// held is the claims of the round an operator's suspension holds, read in the same live group
// (heldByOperator), and live is that group. One round is told once (roundTold), so where an
// operator holds a claim of the round the notice names that hold instead (operatorHoldReason):
// only that operator can end it, and a notice of the unread pod would hide it for as long as the
// round stays open. A hold a project's pause has on the pod that stands is told to nobody,
// whatever the runtime reads of the pod, as it is wherever it holds a round (waitsForPause,
// decision 12 of dispatch://INFRA-3759).
func (s *supervision) tellUnreadPod(ctx context.Context, issue string, live []supervise.Claim, round, pod, reasons string,
	held []supervise.Claim, clock roundClock) {
	if len(held) > 0 {
		s.tellLateRound(ctx, issue, live, round, pod, reasons, operatorHoldReason(held), clock)
		return
	}
	if !clock.held() {
		s.tellLateRound(ctx, issue, live, round, pod, reasons, unreadPodReason, clock)
	}
}

// errReplacementNotDue is every answer replacementDue gives that is not "go on": the round the
// sweep found complete is not complete now, or the claims or the pause's holds could not be read
// to say that it is.
// The runtime hands it back from inside ReplacePod having written nothing (runtime.Runtime), and
// replacePodForRound tells it from a replacement that failed by this error alone — a failure is
// a hold the operator's side is told of, and this is the daemon's own refusal, which costs the
// round one sweep and nothing else.
var errReplacementNotDue = errors.New("the pod replacement is no longer due")

// replacementDue is the daemon's last word on a replacement, which the runtime asks under the
// pod's launch turn and one statement before the write that ends every process in that pod. It
// reads the issue's claims as they are at that instant, at the clock as it stands then, and
// refuses the replacement when one of six readings holds it: an operator's suspension of a
// claim the round asks (heldByOperator), a hold of a project's pause that stands on the pod
// (roundClock.held, read under the turn, below), and a claim of the issue that started a turn
// after the read of the workspace began (turnsSince, readAt; R7 of #5285),
// whatever the clock says; and, only before the round's bound, a claim that has not answered
// (unansweredClaims, which names a claim that a project's pause still keeps whether or not it has
// a session), a claim of the issue that runs a turn, or a claim with a process in the round's pod
// whose report names background work that runs or that has no report since its last turn
// (roundEnd.holding). It does not call readReplacement: it takes the five of that predicate's
// readings that are about the claims and the hold, through the same helpers, because the runtime
// asks it only of a pod it found there. Those readings take the claims masked (masked), since what
// they let through deletes the pod's disk. What it read of the round's end is left in ended, which
// the replacement's log lines name, and that is read as the claims hold it, unmasked: the lines
// say what each claim answered (noticeGroup). The Sandbox runtime asks this only of a pod that is
// there (replacePodForRound), so the operator's hold it reads first is a hold over a pod there is
// to replace.
//
// A turn that started after the read can have made work the read did not see, so the read is no
// word of the disk the replacement would take: the replacement is refused, and the next sweep
// reads the workspace again before it asks for the replacement again (replacePodForRound). A turn
// that started before the read and runs past the bound does not refuse it: the read was made
// beside that turn, which is what the bound steps over.
//
// It reads the project pause's holds of the pod again too (clockUnderTurn), as the launch hook
// reads them under the same turn (roundPodEndsAtLaunch). Whether a hold stands on the pod is read
// from those holds (roundClock.held). That read is the store's as it stands under the turn, so
// it is the read that decision 12 of dispatch://INFRA-3759 asks for: a hold that a pause put on
// the pod after the sweep's read refuses the replacement. The round's bound moves by the holds,
// and the holds say which claims the pause still keeps (podHolds.owes): the round does not pass
// over such a claim (roundClock.passesOver), and the replacement waits for it, with a session or
// with none (unansweredClaims). Both are read through one roundClock over the holds at the clock
// as it stands then. The holds the sweep read at its start are not enough. A wake taken under the
// pause after that read can make a role owed: the wake opens a resume of that pause for its role
// (store.PodRecords.PauseOfWake). The delivery of a row that the pause held is such a wake. Read
// from the sweep's holds, the round would pass over that role, and the replacement would delete
// the pod that the pause keeps for the role's work. A read of the holds that fails refuses the
// replacement.
//
// Which claims exist is read from the store, not from the group this sweep carries: a claim
// created after the sweep's snapshot is a claim the group never held, and the next phase's role,
// launched into the very pod this round keeps, is exactly that claim. Ending the pod under it
// before the bound would cut a turn the round never asked anything of.
//
// What each claim holds is read from its machine's published view (Machine.View), never from
// Machine.Claim: Claim waits out the decision in flight, and the decision in flight can be the
// launch or the suspension that is waiting on the very pod lock this call is made under, which
// would deadlock the daemon's sweep against its own runtime. The view is at least as fresh as the
// store for a claim a machine holds — it is what the latest decision left, or what the decision
// in flight last tried to write — and the store's row stands for a claim no machine of this
// daemon holds. The activity report alone is read from the store's row this call reads
// (roundGroup), so a report a session sent while the runtime waited for the pod's launch turn is
// weighed here.
func (s *supervision) replacementDue(issue, round string, readAt time.Time, ended *roundEnd) func(context.Context) error {
	return func(ctx context.Context) error {
		claims, err := s.supervisor.Claims(ctx)
		if err != nil {
			return fmt.Errorf("%w: the claims of %s cannot be read: %w", errReplacementNotDue, issue, err)
		}
		told, live := s.liveGroup(claimsOf(claims, issue))
		clock, err := s.clockUnderTurn(ctx, issue, live)
		if err != nil {
			return fmt.Errorf("%w: the holds of a project's pause on the pod of %s cannot be read: %w", errReplacementNotDue, issue, err)
		}
		if held := heldByOperator(live, round, clock); len(held) > 0 {
			return fmt.Errorf("%w: an operator's suspension holds %s", errReplacementNotDue, strings.Join(namesOf(held), ", "))
		}
		if clock.held() {
			return fmt.Errorf("%w: a project's pause holds the pod of %s", errReplacementNotDue, issue)
		}
		if turned := turnsSince(live, readAt); len(turned) > 0 {
			return fmt.Errorf("%w: %s started a turn after the read of the workspace", errReplacementNotDue, strings.Join(turned, ", "))
		}
		*ended = readRoundEnd(told, round, clock)
		unanswered, working, background := readRoundEnd(live, round, clock).holding()
		if len(unanswered) > 0 {
			return fmt.Errorf("%w: %s has not answered it, or its machine was deciding something", errReplacementNotDue, strings.Join(namesOf(unanswered), ", "))
		}
		if len(working) > 0 {
			return fmt.Errorf("%w: the turn of %s runs now", errReplacementNotDue, strings.Join(working, ", "))
		}
		if background.holds() {
			return fmt.Errorf("%w: %s", errReplacementNotDue, background.refusal())
		}
		return nil
	}
}

// clockUnderTurn is the round clock of issue as it stands now, over the project pause's holds of
// the issue's pod read again from the store (podPauseHolds). group is the issue's claims. Each
// reading that the runtime asks under the pod's launch turn reads it: the two last words on a
// replacement (replacementDue, issueReplacementDue) and the launch hook (roundPodEndsAtLaunch).
// replacementDue and roundPodEndsAtLaunch say why they read the holds again. It returns the error
// of the read as it is, and each caller says what a read that fails does.
func (s *supervision) clockUnderTurn(ctx context.Context, issue string, group []supervise.Claim) (roundClock, error) {
	now := s.plan.clock.Now()
	holds, err := s.podPauseHolds(ctx, group, now)
	if err != nil {
		return roundClock{}, err
	}
	return s.roundClockAt(now, holds[issue]), nil
}

// replacementByIssue is the replacement a round with no role process in its pod leaves the sweep
// (podlessRound, roundWithNoProcessInItsPod) where the runtime can replace a pod by issue
// (runtime.IssuePodReplacer) and nothing holds the pod (roundPodEnds), and nil otherwise. live is
// the group as its claims' machines hold it now, and pod is the round's own pod, which the runtime
// has just answered is there.
func (s *supervision) replacementByIssue(issue string, live []supervise.Claim, round, pod, reasons string, clock roundClock) *podReplacement {
	if _, ok := s.runtime.(runtime.IssuePodReplacer); !ok {
		return nil
	}
	if ends, _ := roundPodEnds(live, round, clock); !ends {
		return nil
	}
	return &podReplacement{issue: issue, round: round, pod: pod, reasons: reasons, holds: clock.holds, by: viaIssue}
}

// roundPodEnds is whether nothing holds the pod of a round that has no role process in it, and,
// where something does, what that is, for the line or the refusal that names it. Where nothing
// holds the pod, the round ends by the pod's replacement before any launch goes into it: a launch
// into a pod the round is about to replace has its first turn cut by that replacement, and is
// launched a second time (dispatch://INFRA-4109 R7, INFRA-4455's tester on 2026-11-19). It is the
// one statement of that rule. The walk reads it in both of its readings of such a round
// (podlessRound, roundWithNoProcessInItsPod), the replacement reads it again before and under the
// pod's launch turn (issuePodKept), and a launch into the pod reads it under that same turn, when
// the launch runs, so the bound is read then rather than when the claim was resumed
// (roundPodEndsAtLaunch).
//
// Four things hold the pod, read in this order:
//
//   - An operator's suspension of a claim the round asks (heldByOperator), whatever the round's
//     bound: it is a person's hold (dispatch://INFRA-2981 R5).
//   - A project's pause's hold that stands on the pod (roundClock.held), whatever the round's
//     bound: the pause kept the pod for the work on its disk (decision 12 of
//     dispatch://INFRA-3759).
//   - Before the round's bound, a claim of the group that a project's pause still keeps: the one
//     exception (podHolds.owes). Every claim of the group is read, and not only the claims the
//     round waits for (waitingClaims): the launch the pause owes can be a claim that the round
//     does not ask. A launch that forgot its role's lost session records no session until its new
//     agent registers (supervise's startFresh), and the round asks no claim with no session
//     (roundAsks). The pause, not the claim, keeps the claim from a turn, and the pause kept the
//     pod for the work on its disk (decision 12 of dispatch://INFRA-3759,
//     roundClock.passesOver). This holds whether or not an ask of the round reached the claim. No
//     round opens while a hold stands, so a round that opened after the hold ended never asked a
//     role the pause stopped. A launch the wake pacing parked is launching, not suspended, and its
//     machine is in no decision (supervise's waitOnMachine). Neither is the machine of a launch
//     that a daemon restart left launch_uncertain, or released to queued (supervisor.restore). So
//     only this reading keeps the pod for such a launch: no suspension of the claim does, and no
//     machine in a decision does (issuePodKept). Without this reading, the pod and that role's
//     unpushed work would go when the last process left the pod before that role ran again. A
//     launch into the pod reads its own claim here too, as a claim of the group, so this reading
//     also keeps the pod for the launch the pause still owes when that launch runs
//     (roundPodEndsAtLaunch).
//   - Before the round's bound, a claim the round waits for that holds an ask of this round that
//     reached its session (supervise.Claim.UpdateTaskedAt) and that it has not answered. Its
//     process went while it held the ask, so its unpushed work may be on the pod's disk. Any
//     other claim the ask never reached holds nothing: nothing asked it to push. A claim the
//     round passes over holds nothing either, even where the daemon suspended it after its ask
//     reached it (roundClock.passesOver, dispatch://INFRA-4109 R1).
//
// Past the bound only the first two hold the pod: the bound ends every other wait of a round. The
// bound has already moved by the time the pause held the pod (roundClock.roundBound).
func roundPodEnds(group []supervise.Claim, round string, clock roundClock) (bool, string) {
	if held := heldByOperator(group, round, clock); len(held) > 0 {
		return false, "an operator's suspension holds " + strings.Join(namesOf(held), ", ")
	}
	if clock.held() {
		return false, "a project's pause holds the pod"
	}
	if clock.pastBound(group, round) {
		return true, ""
	}
	var owed []supervise.Claim
	for _, c := range group {
		if clock.holds.owes(c, round) {
			owed = append(owed, c)
		}
	}
	if len(owed) > 0 {
		return false, "a project's pause owes the " + strings.Join(namesOf(owed), ", ") +
			" a launch, and it keeps the pod for their work until each runs again"
	}
	var asked []supervise.Claim
	for _, c := range waitingClaims(group, round, clock) {
		if holdsUnansweredAsk(c, round) {
			asked = append(asked, c)
		}
	}
	if len(asked) > 0 {
		return false, strings.Join(namesOf(asked), ", ") + " holds the ask of the round and has not answered it"
	}
	return true, ""
}

// issuePodKept is why a replacement by issue may not be made at one read of the issue's claims,
// wrapping errReplacementNotDue, or nil where it may. The replacement reads it before the pod's
// launch turn and again under it (replaceIssuePodForRound, issueReplacementDue). group is the
// issue's claims as their machines hold them, masked (masked), and clock is the moment of the
// read.
//
// A claim whose machine is deciding (Machine.Deciding) keeps the pod for that read: the decision
// can be a launch into the pod that has let the launch turn go and has not written its locator
// yet, and a replacement then would cut its first turn (dispatch://INFRA-4109 R7). A claim that
// records a process in the round's pod, or whose launch waits on that pod (podProcess), keeps it
// too: the round is then an ordinary round, replaced through that claim once it is over. Last
// comes what holds the pod of a round with no process in it (roundPodEnds).
func (s *supervision) issuePodKept(group []supervise.Claim, round string, clock roundClock) error {
	for _, c := range group {
		if m, ok := s.supervisor.Machine(c.Token); ok && m.Deciding() != nil {
			return fmt.Errorf("%w: the machine of the %s is in a decision", errReplacementNotDue, c.Role)
		}
	}
	if through, reached := podProcess(group, round); reached {
		return fmt.Errorf("%w: the claim %s records a process in the pod or its launch waits on it", errReplacementNotDue, through.Token)
	}
	if ends, held := roundPodEnds(group, round, clock); !ends {
		return fmt.Errorf("%w: %s", errReplacementNotDue, held)
	}
	return nil
}

// replaceIssuePodForRound makes one replacement by issue that the sweep's walk decided on, after
// that walk (reconcilePodUpdates): the pod of a round that has no role process in it and that
// nothing holds (roundPodEnds). It is replacePodForRound's twin for a pod no claim records a
// process in. There is no process to replace the pod through, so the runtime is asked by the
// issue, with the round's own pod (runtime.IssuePodReplacer), and it replaces nothing where the
// issue's Sandbox holds another pod or none.
//
// The claims are read again before the call, as replacePodForRound reads them, and the
// replacement is given up where that read keeps the pod (issuePodKept): a machine in a decision,
// a claim that records a process in the pod, or what holds the pod. Nothing then ends, and the
// next sweep decides the round afresh. The runtime asks the same last word again under the pod's
// launch turn (issueReplacementDue), and a refusal there is the daemon's own and is logged only. A
// replacement the runtime refused for any other reason is retried at the next sweep. Past the
// bound it holds the round, so it is told to the operator's side once, as a refused ReplacePod is
// (tellLateRound, failedReplacementReason).
//
// A replacement made ends the round as a replacement the round made (endPodUpdates). The runtime's
// nil for a pod it found no longer there took nothing, and ends the round as the act that took the
// pod, as ReplacePod's nil does: as a launch's replacement of the pod as the round's own, where this
// call's record goes (overtakenByLaunch); as a launch's delete of the issue's Sandbox over the pod,
// whose record tells the architect (roundReplaced's yield); and otherwise as a replacement made.
// Before the call, and outside the pod's launch turn, the pod's launcher reads the issue's
// workspace (readPodWorkspace, R7 of #5285), since no role process runs in the
// pod to read through. The read decides the replacement as the replacement through a process
// decides it (roundKeeps, keepPodForRound), and the last word refuses it where a claim of the
// issue started a turn after the read began (issueReplacementDue). What may go with the pod is
// named as the members lostClaims names, each one that had not answered and whose work may be on
// the pod's disk (workOnRoundPod), with a session or with none, read unmasked from the same read
// of the issue's claims that issuePodKept reads masked (masked says why), and that the keep reads
// unmasked too (roundKeeps, keepPodForRound), as a gone pod's end reads them
// (endRoundOverAGonePod). The replacement's line names them as the members that had not answered
// (unansweredMembers), says that the replacement was by issue and what the launcher's read found,
// and where that read found work or failed the issue's architect is told what the replacement
// took, naming them (roundReplaced, finishReplacement). A replacement that the bound let through
// over a claim that had not answered (unansweredClaims) is logged on the bound's own line too, as
// replacePodForRound logs it.
//
// The call is bounded to one sweep interval, as replacePodForRound's is: from the read on, the two
// are one function (replaceForRound), and what they differ by is the one value each builds
// (roundCall).
func (s *supervision) replaceIssuePodForRound(ctx context.Context, replace podReplacement) {
	issue, round, pod := replace.issue, replace.round, replace.pod
	replacer, ok := s.runtime.(runtime.IssuePodReplacer)
	if !ok {
		s.log.Error("supervise: this runtime cannot replace a pod by issue; nothing was replaced and the round stays open",
			"issue", issue, "round", round, "pod", pod)
		return
	}
	at := s.plan.clock.Now()
	claims, err := s.supervisor.Claims(ctx)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Error("supervise: read this issue's claims again before its update round's replacement by issue; nothing was replaced and the round stays open",
				"issue", issue, "round", round, "pod", pod, "error", err)
		}
		return
	}
	told, live := s.liveGroup(claimsOf(claims, issue))
	clock := s.roundClockAt(s.plan.clock.Now(), replace.holds)
	if err := s.issuePodKept(live, round, clock); err != nil {
		s.log.Info("supervise: the pod of this update round is kept now the sweep has reached its replacement by issue; nothing was replaced and the round stays open",
			"issue", issue, "round", round, "pod", pod, "reason", err)
		return
	}
	earlierWork := s.roundPodHoldsEarlierWork(ctx, issue, round)
	lost := lostClaims(told, round, earlierWork)
	s.replaceForRound(ctx, replace, at, told, live, clock, roundCall{
		name:        "replacement by issue",
		notDue:      "supervise: this update round's replacement by issue was no longer due when its pod's launch turn came; nothing was replaced and the round stays open",
		failed:      "supervise: replace by issue the pod of an update round with no process in it; retrying at the next sweep",
		due:         s.issueReplacementDue,
		named:       lost,
		lost:        lost,
		earlierWork: earlierWork,
		call: func(ctx context.Context, due func(context.Context) error) error {
			return replacer.ReplaceIssuePod(ctx, issue, pod, due)
		},
	})
}

// issueReplacementDue is the daemon's last word on a replacement by issue, which the runtime asks
// under the pod's launch turn and one statement before the write that ends the pod
// (replaceIssuePodForRound). It reads the issue's claims as replacementDue reads them — which
// claims there are from the store, and what each holds from its machine's view (Machine.View),
// never from Machine.Claim, which waits out a decision that may wait on this very launch turn —
// at the clock as it stands then, over the project pause's holds of the pod read again under that
// turn (clockUnderTurn; replacementDue says why). It refuses the replacement where that read keeps
// the pod (issuePodKept), and where a claim of the issue started a turn after the read of the
// workspace began (turnsSince, readAt; R7 of #5285). A hold of a project's pause
// that stands on the pod is one such reading (roundPodEnds). It leaves what it read of the round's
// end in ended, which the replacement's lines name, read unmasked as replacementDue leaves it.
func (s *supervision) issueReplacementDue(issue, round string, readAt time.Time, ended *roundEnd) func(context.Context) error {
	return func(ctx context.Context) error {
		claims, err := s.supervisor.Claims(ctx)
		if err != nil {
			return fmt.Errorf("%w: the claims of %s cannot be read: %w", errReplacementNotDue, issue, err)
		}
		told, live := s.liveGroup(claimsOf(claims, issue))
		clock, err := s.clockUnderTurn(ctx, issue, live)
		if err != nil {
			return fmt.Errorf("%w: the holds of a project's pause on the pod of %s cannot be read: %w", errReplacementNotDue, issue, err)
		}
		if err := s.issuePodKept(live, round, clock); err != nil {
			return err
		}
		if turned := turnsSince(live, readAt); len(turned) > 0 {
			return fmt.Errorf("%w: %s started a turn after the read of the workspace", errReplacementNotDue, strings.Join(turned, ", "))
		}
		*ended = readRoundEnd(told, round, clock)
		return nil
	}
}

// roundPodEndsAtLaunch is the daemon's reading of the update round over a pod a launch would keep,
// which the Sandbox runtime asks under that pod's launch turn (sandbox.Options.RoundPodEnds,
// relaunch): whether the round open over podUID, the live pod of token's issue, is over by the
// rule for a round with no role process in its pod (roundPodEnds, dispatch://INFRA-4109 R7). A yes
// has the launch replace the pod as the round's own replacement and start its role in a new pod,
// so the role is launched once. The round's bound is read when the launch runs, not when the claim
// was resumed: INFRA-4455's tester was resumed before its round's bound, and the wake pacing
// launched it 29 minutes later, past the bound, into the pod the round then replaced 18 s into its
// first turn.
//
// It answers no, and the launch keeps the pod, where:
//
//   - No claim of the issue records a round over podUID: there is no round to end.
//   - A claim of the issue other than token's records a process in the pod: the round is an
//     ordinary round, which is replaced through that process once it is over.
//   - A sibling's machine is in a decision, and this hook kept the pod for that sibling's launch
//     into it in this round (podUpdateLaunchReplaced): that launch has let the launch turn go and
//     has not written its locator yet, so it is a process in the pod. A sibling in any other
//     decision is no process in the pod: it waits for this launch turn and asks this hook itself
//     when it gets it, or it went into a pod that replaced this one. Counted as a process, it would
//     keep the round's pod for two launches at once, and neither would replace it. A launch the
//     wake pacing parked is in no decision, so this reading keeps nothing for it; the next one
//     keeps the pod for it by the one exception (podHolds.owes).
//   - Something holds the pod (roundPodEnds, which launchRoundPodEnds reads). The launching claim
//     is in the group, launching with no process recorded, and roundPodEnds reads every claim of
//     the group for the launch that a project's pause still owes (podHolds.owes). So the launch
//     that the pause still owes keeps the pod by the one exception, whether or not the round's
//     sets read the launching claim: a launch that forgot its role's lost session records no
//     session (supervise's startFresh). The launch of any other role keeps the pod for that owed
//     launch too. Without that, the launch would replace the pod that the pause kept for that very
//     work.
//   - Before the bound, the launching claim holds the round's ask itself and has not answered it:
//     its process went while it held the ask, and its work may be on the disk. That holds of the
//     launch of a fresh session too. Such a claim records no session until its new agent
//     registers, so the round's sets (roundAsks) do not read it as a member yet, but its ask mark
//     stays across the fresh session, since only the round's end clears it.
//
// The project pause's holds are read again from the store under the turn (clockUnderTurn), as the
// sweep reads them (podPauseHolds), so the bound is read moved by the time the pause held the
// pod, and what the pause owes each role is read from the same holds. A read of the holds that
// fails keeps the pod, since a reading nobody took may not delete a disk, and it is logged. A read
// of the claims that fails is the launch's error, which fails the launch as any of its steps does.
//
// It runs inside the launching machine's decision and under the pod's launch turn. So it reads
// which claims there are from the store and what each holds from its machine's published view and
// Deciding alone, never from Machine.Claim, which waits out a decision that may wait on this very
// launch turn (replacementDue). It reads every claim unmasked (noticeGroup, never masked),
// unlike the other readers that may delete a disk. They mask the answer of a claim whose machine
// is deciding, because that decision may start a turn in the pod that the view does not show yet.
// No sibling here can start such a turn unseen. A sibling with no process in this pod cannot
// start a turn in it: it waits for this launch turn and asks this hook itself, or it went into a
// pod that replaced this one. A sibling with a process in the pod keeps the pod already
// (inRoundPod). A sibling's launch that this hook kept the pod for, and that let the launch turn
// go, keeps the pod too (launchKept). So the mask has no job here, and it would do harm. Two
// members that had answered, which the daemon suspended and then resumed at once, would each keep
// the pod: the launch that reads the round first would read the other, which waits for the launch
// turn in its decision, as a member that holds the round's ask and has not answered it, and the
// other would then find that launch kept or its process in the pod. Both would start in the
// round's pod and be launched again once the round replaced it (row 8b of the plan's state table,
// dispatch://INFRA-4109 R7). The launching claim is read in the same group, unmasked as its
// siblings are: its decision is this launch, which starts no turn, and an answer it gave before
// its process went stands.
//
// Most launches it is asked about find no round recorded over the pod, and the store's whole
// claims table is not read for them under the launch turn. The launching claim's issue is read
// from its own machine's view, and the machines' published views are asked first whether a claim
// of that issue that has not retired records the round (viewsRecordRound): where none does, the
// answer is no, and the store is not read. A token no machine holds is no launch of this daemon's,
// and it keeps the pod. The views suffice for that no. The boot's restore builds a machine for
// every claim the store holds, and Create adds one for every claim it writes, so every claim of
// the store has a machine; and a machine's view is at least as fresh as the store's row, which is
// what the read of the group below takes for that claim in any case (roundGroup). The rounds
// TestALaunchUnderARoundWithNoProcessIsLaunchedOnce and
// TestALaunchUnderARoundWithNoProcessKeepsThePodForAMemberThatHoldsItsAsk launch under are
// recorded on restored claims and read through the views, and the second of the two launches at
// once in the first test reads no round over the pod the first launch built, and keeps that pod.
//
// Where the round is over by that rule, the pod's launcher reads the issue's workspace before the
// answer (readPodWorkspace, R7 of #5285), under the pod's launch turn, which that
// read does not take. The read decides as the replacement by issue's does (roundKeeps): a read
// that finds work that no branch or tag on origin holds, or that cannot be made, over a drift the
// pod's roles can keep working under, keeps the pod, ends the round with no replacement
// (keepPodForRound), and answers no, so the launch starts its role in the kept pod. A read that
// finds nothing, or a round over a drift that breaks the pod, answers yes, and the read is kept
// with the replacement for the sweep's end of the round (endRoundALaunchReplaced). No turn voids
// this read: the launch holds the pod's launch turn, so no role can start in the pod before the
// launch replaces it.
//
// Each answer is kept for the round (podUpdateLaunchReplaced), under a mutex that no runtime call
// is made under: a keep for token, which the next launch into the pod reads, and a replacement,
// which the sweep reads to end the round as the replacement it is (endRoundALaunchReplaced). A
// replacement keeps no reading of the members with it: whom that end names is read at the end, as
// at every end (lostClaims, workOnRoundPod).
func (s *supervision) roundPodEndsAtLaunch(ctx context.Context, token claim.Token, podUID string) (bool, error) {
	own, ok := s.supervisor.Machine(token)
	if !ok {
		return false, nil
	}
	issue := own.View().Issue
	if issue == "" {
		return false, nil
	}
	round := supervise.PodRound(podUID)
	if !s.viewsRecordRound(issue, round) {
		return false, nil
	}
	at := s.plan.clock.Now()
	claims, err := s.supervisor.Claims(ctx)
	if err != nil {
		return false, fmt.Errorf("read the claims of the update round over pod %s: %w", podUID, err)
	}
	group := s.noticeGroup(claimsOf(claims, issue))
	if !recordsRound(group, round) {
		return false, nil
	}
	var launching supervise.Claim
	for _, c := range group {
		m, held := s.supervisor.Machine(c.Token)
		switch {
		case c.Token == token:
			launching = c
		case inRoundPod(c, round):
			return s.keepAtLaunch(issue, round, token, fmt.Sprintf("the claim %s records a process in the pod", c.Token))
		case held && m.Deciding() != nil && s.launchKept(issue, round, c.Token):
			return s.keepAtLaunch(issue, round, token, fmt.Sprintf("the launch of the %s into the pod is in a decision", c.Role))
		}
	}
	clock, err := s.clockUnderTurn(ctx, issue, group)
	if err != nil {
		s.log.Error("supervise: read the pods a project's pause holds; the launch keeps the pod of the update round over it",
			"issue", issue, "round", round, "claim", token, "error", err)
		return s.keepAtLaunch(issue, round, token, "the holds of a project's pause cannot be read")
	}
	if ends, held := launchRoundPodEnds(group, launching, round, clock); !ends {
		return s.keepAtLaunch(issue, round, token, held)
	}
	reasons := roundReasons(group, round)
	read := s.readPodWorkspace(ctx, issue, round, runtime.Known{}, viaLaunch, at)
	read.origin = roundRepository(group, round)
	if roundKeeps(reasons, read) {
		s.keepPodForRound(ctx, issue, round, podUID, reasons, read, group)
		return s.keepAtLaunch(issue, round, token, "the read of the pod's workspace found work that no branch or tag on origin holds, or could not be made")
	}
	// The replacement's record, written before the answer that has the launch suspend the pod, and
	// kept for the sweep that ends the round as this replacement (endRoundALaunchReplaced, or
	// finishReplacements after a restart). A record that cannot be written keeps the pod
	// (recordRoundReplacement).
	recorded, ok := s.recordRoundReplacement(ctx, issue, round, podUID, reasons, read)
	if !ok {
		return s.keepAtLaunch(issue, round, token, "the record of the replacement cannot be written")
	}
	s.replacedAtLaunch(issue, round, replacedLaunch{ended: readRoundEnd(group, round, clock), read: read, record: recorded})
	s.log.Info("supervise: no role process runs in the pod of this update round, and nothing holds the pod; the launch replaces the pod and starts its role in a new pod",
		append([]any{"issue", issue, "round", round, "pod", podUID, "claim", token}, read.attrs()...)...)
	return true, nil
}

// launchRoundPodEnds is the reading of roundPodEndsAtLaunch that the claims and the clock alone
// answer, over group, the claims of the issue with launching, the launching claim, among them. It
// is whether a launch into the pod of round replaces that pod, and, where it does not, why. The
// launch keeps the pod where something holds it (roundPodEnds), or where, before the bound, the
// launching claim holds the round's ask itself and has not answered it (holdsUnansweredAsk).
func launchRoundPodEnds(group []supervise.Claim, launching supervise.Claim, round string, clock roundClock) (bool, string) {
	if ends, held := roundPodEnds(group, round, clock); !ends {
		return false, held
	}
	if !clock.pastBound(group, round) && holdsUnansweredAsk(launching, round) {
		return false, "the launching claim holds the ask of the round and has not answered it"
	}
	return true, ""
}

// keepAtLaunch is the answer of roundPodEndsAtLaunch that keeps round's pod for token's launch, for
// the reason why: the keep is kept for the round (podUpdateLaunchReplaced) and logged.
func (s *supervision) keepAtLaunch(issue, round string, token claim.Token, why string) (bool, error) {
	s.launchRecord(issue, round, func(at *launchRound) { at.kept[token] = true })
	s.log.Info("supervise: a launch goes into the pod of an update round that keeps its pod",
		"issue", issue, "round", round, "claim", token, "reason", why)
	return false, nil
}

// launchRound is what the launches into one issue's pod decided of the update round open over that
// pod (roundPodEndsAtLaunch), as podUpdateLaunchReplaced keeps it for the issue: the round, the
// launch's replacement of its pod, nil where no launch replaced it, and each claim whose launch into
// the pod the hook kept the pod for.
//
// It keeps no reading of whom the end names: the end reads its group at the sweep
// (endRoundALaunchReplaced), as every end of a round does (lostClaims, workOnRoundPod).
type launchRound struct {
	round    string
	replaced *replacedLaunch
	kept     map[claim.Token]bool
}

// replacedLaunch is a launch's replacement of a round's pod as the round's own
// (roundPodEndsAtLaunch): what the launch read of the round's end, what the pod's launcher read of
// the workspace before it, and the replacement's record (recordRoundReplacement).
type replacedLaunch struct {
	ended  roundEnd
	read   workspaceRead
	record store.PodReplacement
}

// launchRecord runs write on the record podUpdateLaunchReplaced keeps of round for the issue, in
// place of a record of any other round, under the record's own mutex.
func (s *supervision) launchRecord(issue, round string, write func(*launchRound)) {
	s.podUpdateLaunchMu.Lock()
	defer s.podUpdateLaunchMu.Unlock()
	if s.podUpdateLaunchReplaced == nil {
		s.podUpdateLaunchReplaced = map[string]*launchRound{}
	}
	at := s.podUpdateLaunchReplaced[issue]
	if at == nil || at.round != round {
		at = &launchRound{round: round, kept: map[claim.Token]bool{}}
		s.podUpdateLaunchReplaced[issue] = at
	}
	write(at)
}

// replacedAtLaunch keeps that a launch replaced round's pod as the round's own replacement, launch
// (podUpdateLaunchReplaced).
func (s *supervision) replacedAtLaunch(issue, round string, launch replacedLaunch) {
	s.launchRecord(issue, round, func(at *launchRound) { at.replaced = &launch })
}

// launchKept is whether roundPodEndsAtLaunch kept round's pod for token's launch into it.
func (s *supervision) launchKept(issue, round string, token claim.Token) bool {
	s.podUpdateLaunchMu.Lock()
	defer s.podUpdateLaunchMu.Unlock()
	at := s.podUpdateLaunchReplaced[issue]
	return at != nil && at.round == round && at.kept[token]
}

// launchReplaced is the launch's replacement of round's pod as the round's own replacement, and
// whether a launch made one (podUpdateLaunchReplaced).
func (s *supervision) launchReplaced(issue, round string) (replacedLaunch, bool) {
	s.podUpdateLaunchMu.Lock()
	defer s.podUpdateLaunchMu.Unlock()
	at := s.podUpdateLaunchReplaced[issue]
	if at == nil || at.round != round || at.replaced == nil {
		return replacedLaunch{}, false
	}
	return *at.replaced, true
}

// launchBoundLineWritten keeps that the bound's line of a launch's replacement of round's pod is
// written (roundEnd.lineWritten), where podUpdateLaunchReplaced still holds that replacement: the
// walk's next end of the round, after a finish the store refused, writes it no second time.
func (s *supervision) launchBoundLineWritten(issue, round string) {
	s.podUpdateLaunchMu.Lock()
	defer s.podUpdateLaunchMu.Unlock()
	if at := s.podUpdateLaunchReplaced[issue]; at != nil && at.round == round && at.replaced != nil {
		at.replaced.ended.lineWritten = true
	}
}

// forgetLaunchRound drops what podUpdateLaunchReplaced keeps of round for the issue: the sweep has
// ended that round (endPodUpdates), and no launch is kept for it any more.
func (s *supervision) forgetLaunchRound(issue, round string) {
	s.podUpdateLaunchMu.Lock()
	defer s.podUpdateLaunchMu.Unlock()
	if at := s.podUpdateLaunchReplaced[issue]; at != nil && at.round == round {
		delete(s.podUpdateLaunchReplaced, issue)
	}
}

// forgetLaunchRounds drops what podUpdateLaunchReplaced keeps for each issue that has no claim in
// this sweep: every claim of the issue has retired, so no launch into its pod is left to keep the
// pod for, and no sweep is left to end its round as a launch's replacement. Without it such an
// entry would stay until a restart, since only the end of its round drops it otherwise
// (forgetLaunchRound). byIssue is the sweep's groups, which hold no retired claim.
func (s *supervision) forgetLaunchRounds(byIssue map[string][]supervise.Claim) {
	s.podUpdateLaunchMu.Lock()
	defer s.podUpdateLaunchMu.Unlock()
	for issue := range s.podUpdateLaunchReplaced {
		if len(byIssue[issue]) == 0 {
			delete(s.podUpdateLaunchReplaced, issue)
		}
	}
}

// endRoundALaunchReplaced ends round, whose pod a launch replaced as the round's own replacement
// (roundPodEndsAtLaunch, podUpdateLaunchReplaced), at the first sweep that reads that pod gone or
// another pod in its place (endRoundWhosePodWent). The launch made the replacement the sweep makes
// by issue (replaceIssuePodForRound), by the same rule, so the end is that replacement's
// (roundReplaced): the line of a replacement the round made, here at a launch (atLaunch), which
// names the members of the round that lostClaims names (unansweredMembers, read unmasked as
// noticeGroup reads them), and what the pod's launcher read of the workspace before the
// replacement (read); the bound's own line where the bound let the replacement through (ended,
// what the launch read under the pod's launch turn), written at the first end alone, since a store
// that refuses the finish has the walk end the round again at each sweep with the same reading
// (launchBoundLineWritten); and, where the read found work or failed, the architect's notice of
// what the replacement took, naming the same members (finishReplacement). Whom it names is read of
// each member as it stands at this sweep (lostClaims, workOnRoundPod), over earlierWork, the
// store's record of the disk of the round's pod as this end read it (roundPodHoldsEarlierWork).
// It is not podWentWithoutItsRound's end, which tells the issue that its pod went before its round
// was over: this round was over, and its own replacement took the pod. The in-memory record goes
// with the end (endPodUpdates), and the end comes once the notice is queued.
//
// The read and the record are kept in memory, and the record in the store too: a daemon that
// restarts between the launch and the sweep finishes the replacement from the store's record
// (finishReplacements), as the round's own, from the read the launcher made before it, and names
// the members by the same rule, over the sets its machines read again off the stored claims
// (supervise's bootRegisteredPods).
func (s *supervision) endRoundALaunchReplaced(ctx context.Context, issue string, group []supervise.Claim, round string, launch replacedLaunch,
	earlierWork bool) {
	told := s.noticeGroup(group)
	lost := lostClaims(told, round, earlierWork)
	if _, ended := s.roundReplaced(ctx, issue, round, strings.TrimPrefix(round, supervise.PodRoundPrefix), roundReasons(told, round), launch.ended,
		lost, lost, launch.read, told, earlierWork, launch.record, false); !ended {
		s.launchBoundLineWritten(issue, round)
	}
}

// viewsRecordRound is whether a claim of the issue that has not retired records round, read from
// each machine's published claim (Machine.Published) alone: never from Machine.Claim, which waits
// out the decision in flight, and never from the store. roundPodEndsAtLaunch asks it under a pod's
// launch turn before it reads the store, and reads the store only where this answers yes.
func (s *supervision) viewsRecordRound(issue, round string) bool {
	for _, m := range s.supervisor.supervised() {
		c := m.Published()
		if c.Issue == issue && c.State != supervise.StateRetired && memberOf(c, round) {
			return true
		}
	}
	return false
}

// claimsOf is every claim of claims that is the issue's, in their order.
func claimsOf(claims []supervise.Claim, issue string) []supervise.Claim {
	var of []supervise.Claim
	for _, c := range claims {
		if c.Issue == issue {
			of = append(of, c)
		}
	}
	return of
}

// liveGroup is one read of the group as its claims' own machines hold them now, in the group's
// order, given twice: as the claims hold their answers (told, which noticeGroup gives alone), and
// masked (live, masked). A caller that decides from live and asks or reports from told reads both
// at one moment, and the mask is applied here alone, so no caller pairs a claim with another
// claim's machine.
func (s *supervision) liveGroup(group []supervise.Claim) (told, live []supervise.Claim) {
	told, deciding := s.roundGroup(group)
	return told, masked(told, deciding)
}

// noticeGroup is the group as its claims hold their answers: liveGroup's told alone, one fresh read
// of each claim's machine, unmasked. masked says which readers take this view and which take the
// masked one, and why.
func (s *supervision) noticeGroup(group []supervise.Claim) []supervise.Claim {
	read, _ := s.roundGroup(group)
	return read
}

// machinesGroup is the issue's claims as this daemon's machines publish them now, unmasked
// (noticeGroup), in the order of their tokens, as the store reads a group. It reads no store, so
// an end that a finish outside the round's own call decides (endTakenRounds) ends its round
// whatever the store answers: an end left undone for a failed read of the claims would leave the
// round open, and a later walk would read the pod as one that went before its round was over (the
// Legion reviewer's finding on podreplace.go:355 on #5285 at 18d81dad). The end goes to the
// members' machines alone (endPodUpdates), so a claim no machine of this daemon holds is one no end
// reaches either.
func (s *supervision) machinesGroup(issue string) []supervise.Claim {
	var group []supervise.Claim
	for _, m := range s.supervisor.supervised() {
		if view := m.View().Claim; view.Issue == issue {
			group = append(group, view)
		}
	}
	slices.SortFunc(group, func(a, b supervise.Claim) int { return strings.Compare(string(a.Token), string(b.Token)) })
	return s.noticeGroup(group)
}

// roundGroup is the claims of group as a round reads them now, less the ones no round weighs, and
// which of them a machine was deciding something for at that read (deciding, nil where none was).
// It is the one read the sweep's decision, its ask, the replacement's last word and the
// operator's notice are all built from, so no two of them can disagree about what a claim holds.
// The round asks each claim roundAsks keeps, and replaces the pod once each of them has answered
// (waitingClaims), no claim that a project's pause still keeps and that the round does not ask
// holds it (unansweredClaims, dispatch://INFRA-4109 R8), no turn runs and no claim with a process
// in the round's pod reports background work that runs or has sent no report since its last turn
// started (roundEnd.holding), or at the round's bound (pod_update_grace_minutes) whatever the
// roles answered and whatever runs, an operator's suspension excepted.
//
// What it reads is each machine's published view (Machine.View), never Machine.Claim: Claim
// waits out the decision in flight, and that decision can be a Sandbox relaunch that holds the
// machine for minutes. This runs for every drifted issue on every tick, so one relaunching claim
// would otherwise stall the whole sweep behind it — the orphan reaping, the stale-plugin posts
// and every later issue's round. The view is at least as fresh as the store for a claim a
// machine holds, and the store's row stands for a claim no machine of this daemon holds.
//
// The activity report is the one part of a claim read from the store's row in group and not from
// the view. The activity route writes the store and posts nothing to the machine, and a machine
// holds the report only as it read it at its own boot (store.ClaimActivity's doc), so the view's
// report is as old as that boot. The four activity fields are copied from the row onto the view
// only where the row is of the view's own launch (Generation). A view of another launch — a
// relaunch made after the row was read, or one still in flight — is a process the row's report
// does not describe: its four fields are cleared, and the background hold (backgroundClaims)
// reads it as a claim with no report. The store clears the four columns at each launch
// (putClaim) and writes no report onto another launch than the one it names (PutClaimActivity),
// so this check is for a row and a view read at two moments, which the store cannot see.
//
// A retired claim is weighed by no round: it runs nothing in any pod.
//
// Each claim is read as its machine published it, its answer included, which is a fact about the
// claim. Where its machine was deciding something at the same read (Machine.Deciding, which
// waits on nothing either), its position in read is kept in deciding, for the one kind of reader
// that weighs it: a decision to replace the pod, which reads the claims masked. liveGroup alone
// pairs the two.
func (s *supervision) roundGroup(group []supervise.Claim) (read []supervise.Claim, deciding []int) {
	read = make([]supervise.Claim, 0, len(group))
	for _, c := range group {
		viewed, busy := c, false
		if m, ok := s.supervisor.Machine(c.Token); ok {
			viewed = m.View().Claim
			if viewed.Generation == c.Generation {
				viewed.ActivityAt, viewed.ActivityBackground = c.ActivityAt, c.ActivityBackground
				viewed.ActivityKinds, viewed.ActivityNextReminderAt = c.ActivityKinds, c.ActivityNextReminderAt
			} else {
				viewed.ActivityAt, viewed.ActivityBackground = time.Time{}, false
				viewed.ActivityKinds, viewed.ActivityNextReminderAt = nil, time.Time{}
			}
			busy = m.Deciding() != nil
		}
		if viewed.State == supervise.StateRetired {
			continue
		}
		if busy {
			deciding = append(deciding, len(read))
		}
		read = append(read, viewed)
	}
	return read, deciding
}

// masked is roundGroup's read as a decision to replace the pod reads it: a claim whose machine
// was deciding something at the read (at a position deciding names) reads as not having answered.
// The view of such a claim is what the decision in flight last tried to write, and the decision
// may be a turn starting that the view does not show yet, so a reader about to delete a disk
// before the round's bound masks it: the cost is one sweep, and the cost of the other way is a
// turn cut before the bound. Those readers are the walk's reading (readReplacement) and what it
// decides of a pod with no process in it (roundWithNoProcessInItsPod, roundPodEnds), and the
// replacement's read again before the pod's launch turn and its last word under it
// (replacePodForRound, replacementDue, issuePodKept). The launch hook may delete a disk and masks
// nothing (roundPodEndsAtLaunch says why).
//
// Every other reader takes the answer as the claim holds it (noticeGroup, or liveGroup's told):
// the round's post and its ask (unaskedClaims says why the ask), the operator's notices, the
// round's keep of its pod over what the read of the workspace before a replacement found, which
// stops nothing in the pod (roundKeeps, keepPodForRound), the read that may end that keep
// (readKeptPod), and the names on the lines and messages of a round's end. The mask is a decision
// the replacement makes, not a fact about a claim: the controller's notice, for one, marks each
// claim the round asks with whether it answered (record.UpdateWaiting.Answered), and a masked
// claim would read there as one that said nothing although it answered. The launch hook reads
// noticeGroup too, though it may delete a disk, for the reason its own doc gives.
//
// Where no machine was deciding, masked is read itself, so no reader of either may write it;
// otherwise it is a copy.
func masked(read []supervise.Claim, deciding []int) []supervise.Claim {
	if deciding == nil {
		return read
	}
	live := slices.Clone(read)
	for _, i := range deciding {
		live[i].UpdateSignalledAt, live[i].UpdateHead = nil, ""
	}
	return live
}

// tellLateRound tells the operator's side of a round that has outlived its bound (roundBound)
// and still has not replaced its pod. It is the one writer of the late-round notice, and it tells
// nothing before the bound: a round waiting on its roles is no stall, because the bound ends that
// wait by itself. Its callers are the readings that hold a round past its bound — an operator's
// suspension, no process in the round's pod, two pods at once, a pod the runtime cannot read
// (tellUnreadPod) — and the act of the replacement itself that holds it, a replacement the
// runtime refused, through a process or by issue. why is the reason the controller reads, naming
// that reading. The issue is written nothing: there is no instruction for its roles here, and the
// operator reads the round through the controller's notice.
//
// The bound runs from the round's opening and from nothing else, so one round is told at one
// moment whatever its roles did before it: an answer moves the round's end earlier and never its
// notice later. The one thing that moves it later is a project's pause's hold of the pod (clock,
// roundClock.roundBound), since the pause kept the roles from answering.
//
// The notice lists every claim of the group the round is posted to that has not failed
// (noticeClaims, postedClaims): each claim with a session, and each claim whose launch waits on
// the round's own pod. Each is listed with whether it answered (record.UpdateWaiting.Answered),
// read unmasked (noticeGroup): past the bound no answer holds the round, so the list says what
// each role said rather than whom the round waits for. The list names a claim the daemon
// suspended too, whether or not the round asks it and waits for it (roundAsks,
// roundClock.passesOver), and a claim whose launch waits on the round's pod, whose answer the
// round does not wait for (awaitsAnswer): the controller reads every role of the pod, each with
// its state. It is never empty while the group holds such a claim, which the store requires of
// the notice (record.UpdateStalled). A group with no such claim is told nothing: there is no role
// to name. A round that group already records as told is told nothing again and read no further.
func (s *supervision) tellLateRound(ctx context.Context, issue string, group []supervise.Claim, round, pod, reasons, why string, clock roundClock) {
	if !clock.pastBound(group, round) || roundTold(group, round) {
		return
	}
	told := s.noticeGroup(group)
	waiting := waitingOf(round, noticeClaims(told, round))
	if len(waiting) == 0 {
		return
	}
	s.tellUpdateStalled(ctx, updateStall{
		issue: issue, round: round, pod: pod, reasons: reasons, group: told,
		waiting: waiting,
		reason:  why,
	})
}

// podlessRound decides a round open over an issue whose claims record no pod at all. No locator
// is no evidence either way — every claim of the issue may be suspended (letGo clears the
// locator), or the one live claim may be minutes into a relaunch — so nothing is decided from the
// claims, and the runtime is asked about the round's own pod by the one thing left to ask it by:
// the issue, whose Sandbox that pod was (runtime.IssuePodReader).
//
// A Sandbox that holds another pod than the round's, or no pod, or no Sandbox at all, is the
// round's pod gone, whatever replaced or removed it — the release of a pod no live claim owns, the
// issue's stop, a pod replaced for a failure of its own while every role of it was suspended — and
// the round ends there as every gone pod's round ends, naming the members that lost work
// (endRoundOverAGonePod). That end is told of the round's own pod, the uid its id carries
// (supervise.PodRound), since that is the pod that is not there; what the Sandbox holds now is
// this reading's own line. Left open, the round would hold the next task of every member that
// answered it for as long as no claim runs, and neither an answer nor a replacement could end it
// then: the pod both of them are about is gone.
//
// The round's own pod is there with no role process in it. Where nothing holds that pod
// (roundPodEnds, read over the claims as their machines hold them now), the round ends by its
// replacement, which the runtime makes by issue after the sweep's walk (runtime.IssuePodReplacer,
// replaceIssuePodForRound): the pod is replaced before any launch goes into it, once the pod's
// launcher has read its workspace and found nothing
// that origin does not hold (readPodWorkspace, R7 of #5285). The replacement is
// reported for the walk to hand on
// (reconcileIssuePodUpdate). A launch that comes before the replacement reads the same rule as it
// runs and replaces the pod itself (roundPodEndsAtLaunch); the sweep then reads the pod gone, or
// another in its place, and ends the round as that replacement (endRoundALaunchReplaced).
//
// A pod something holds, no answer — a store that could not be read, or a runtime that runs no
// pods and implements no such read — and a runtime that replaces no pod by issue leave the round
// where it stands, waiting for a claim to record a pod again, and the operator's side is told of
// it once the round's bound has come (tellPodlessRound). A pod a project's pause holds tells
// nobody (roundClock.held, decision 12 of dispatch://INFRA-3759): the pause stopped every role of
// it on purpose, and the round waits for the hold to end.
//
// A claim whose launch waits on a pod records that pod for this reading (podLocator, groupPod),
// so an issue such a claim holds never comes here: neither its round's end nor its wait is read
// from the Sandbox, and a round over the pod that claim waits on is carried as any round over a
// pod a process runs in. Read here, the round would wait for a claim to record a pod again
// wherever the runtime replaces no pod by issue, and that claim's every launch is refused until
// the round replaces the pod.
func (s *supervision) podlessRound(ctx context.Context, issue string, group []supervise.Claim, round string, clock roundClock) *podReplacement {
	if pods, ok := s.runtime.(runtime.IssuePodReader); ok {
		sandboxPod, read := pods.IssuePod(issue)
		if read && supervise.PodRound(sandboxPod) != round {
			s.log.Info("supervise: no claim of this issue records a pod, and the issue's Sandbox no longer holds the pod its update round was opened over",
				"issue", issue, "round", round, "sandboxPod", sandboxPod)
			s.endRoundOverAGonePod(ctx, issue, group, round, strings.TrimPrefix(round, supervise.PodRoundPrefix))
			return nil
		}
		if read {
			_, live := s.liveGroup(group)
			if replace := s.replacementByIssue(issue, live, round, sandboxPod, roundReasons(group, round), clock); replace != nil {
				return replace
			}
		}
	}
	if clock.held() {
		s.log.Info("supervise: no claim of this issue records a pod, and a project's pause holds it; its update round waits for the hold to end",
			"issue", issue, "round", round)
		return nil
	}
	s.log.Info("supervise: no claim of this issue records a pod; its update round waits for one to run again",
		"issue", issue, "round", round)
	s.tellPodlessRound(ctx, issue, group, round, clock)
	return nil
}

// tellPodlessRound tells the operator's side of a round whose issue records no pod at all, and
// whose own pod the runtime says is still there or cannot say of (podlessRound), once the round's
// bound has come (roundBound), the same clock as every other late-round notice (tellLateRound).
// Such a round reaches no replacement through a process: no drift can be read, and it waits for
// one of the issue's workers to run again. Past the bound it waits only where it cannot be ended:
// an operator's suspension holds a claim of it, the runtime cannot say where its pod is, or the
// runtime replaces no pod by issue (runtime.IssuePodReplacer). Where the runtime says the round's
// own pod is there and replaces pods by issue, nothing but an operator's or a pause's hold keeps
// that pod past the bound (roundPodEnds), and the replacement by issue ends the round instead.
// The notice names the reading of a claim that records no process in the round's pod. Where an
// operator's suspension holds a claim of the round (heldByOperator) — a lone worker an operator
// suspended is the common shape, since a suspension lets the process go (letGo clears the
// locator) — only that operator can end the wait, and the notice names the suspension instead.
// No other path here would ever tell them: this round reaches no replacement decision.
//
// A round with no unanswered claim (unansweredClaims) and no claim an operator holds is told
// nothing, and that is the ordinary end of a phase rather than a hold: the workers answered, the
// daemon suspended each of them, and letGo took the last locator with the last process. A claim
// the daemon suspended before it answered does not change that: the round does not wait for it
// (roundAsks, dispatch://INFRA-4109 R1). A claim that a project's pause still keeps is the one
// exception (podHolds.owes): a round past its bound with it unanswered is told, whether or not the
// claim has a session (unansweredClaims). Nobody has to do anything about the ordinary end — the
// replacement by issue ends it, or, where the runtime makes none, a role of the issue that runs in
// a pod again ends it — so telling the controller only opens a finding ask over a round that needs
// no one.
//
// The sweep's snapshot decides first whether the bound has come and whether the round was told,
// so a round before its bound costs no read of the machines. The rest is tellLateRound's, with no
// pod to name.
func (s *supervision) tellPodlessRound(ctx context.Context, issue string, group []supervise.Claim, round string, clock roundClock) {
	if !clock.pastBound(group, round) || roundTold(group, round) {
		return
	}
	told := s.noticeGroup(group)
	held := heldByOperator(told, round, clock)
	if len(held) == 0 && len(unansweredClaims(told, round, clock)) == 0 {
		return
	}
	why := boundPastReason + "No claim of this issue records a process in a pod. The round waits for a claim of the issue to run again."
	if len(held) > 0 {
		why = fmt.Sprintf(boundPastReason+"No claim of this issue records a process in a pod. An operator suspended %s. The round waits for the operator to resume a claim.",
			roleStates(held))
	}
	s.tellLateRound(ctx, issue, group, round, "", roundReasons(group, round), why, clock)
}

// podWentWithoutItsRound is the round's pod gone before the round could finish: the message the
// issue's spec requires is written on the issue, naming what was lost, and the round ends.
// Nothing here can bring the work back, so the message is the whole of it. The work stays only
// where the issue's Sandbox still holds the kept volume that it held when the round opened, and the
// issue's next launch keeps that volume (roundKeptVolume). The issue's next pod mounts that volume,
// so the message says where the work is. An ephemeral disk went with the pod, and the work on it
// with it. A reset of a kept volume or a change of the disk's shape made a new Sandbox, which holds
// a new, empty volume, so the work is lost there too. So is a Sandbox that carries the reset mark,
// or whose disk shape is not the one the configuration now renders: the next launch deletes it,
// with its volume. On another project's issue, the pod of the operator's spawn of it, no message is
// written (plan.writeOnIssue), and the daemon's log lines are the whole of it.
//
// Three readings say the pod went. The claims recording a pod other than the round's own is one
// (reconcileIssuePodUpdate), whose end counts the claims it decided from; the runtime answering
// that the pod the claims record is not there is another; and, where no claim records a pod, the
// runtime answering that the issue's Sandbox holds another pod or none is the third
// (podlessRound). The last two end through endRoundOverAGonePod, whose end counts the machines'
// own unmasked view of the group (noticeGroup). Each reading comes here through the one rule for
// a round whose pod went (endRoundWhosePodWent). What was lost is read from the group this is
// given, so that group is the one the issue is told of and the one the round ends on.
//
// What was lost is the round's own question, which is why it is lostClaims and never
// unansweredClaims: the members of the round that neither answered nor left having answered, and
// whose work may be on the round's pod's disk (workOnRoundPod). A claim this issue holds that is
// in no round of its own — one an earlier sweep's end already took out of this round, or a role
// first started in the pod that replaced this round's own — was never in the pod whose disk went,
// and naming it would tell the issue it lost work that nobody wrote. The caller reads lost, the
// members to name, once (endRoundWhosePodWent), and this names those and no other.
func (s *supervision) podWentWithoutItsRound(ctx context.Context, issue string, group []supervise.Claim, round, pod string, lost []supervise.Claim) {
	reasons := roundReasons(group, round)
	kept := s.roundKeptVolume(issue, group, round)
	if kept {
		s.log.Warn("supervise: the pod of an open update round is gone; its disk is a kept volume, so what the roles that had not answered did not push is still on it",
			"issue", issue, "round", round, "pod", pod, "lost", namesOf(lost))
	} else {
		s.log.Warn(podWentLostLine,
			"issue", issue, "round", round, "pod", pod, "lost", namesOf(lost))
	}
	moved := s.roundMove(ctx, issue, roundRepository(group, round))
	written, err := s.plan.writeOnIssue(ctx, issue, record.MessagePost{Body: podWentMessage(issue, round, reasons, lost, kept, moved)})
	switch {
	case err != nil:
		s.log.Error("supervise: write on the issue that its pod went without its update round",
			"issue", issue, "round", round, "error", err)
	case !written:
		s.log.Info("supervise: nothing is written on the issue that its pod went without its update round: its issue is another project's, whose rows this daemon writes none of",
			"issue", issue, "round", round)
	}
	s.endPodUpdates(issue, group, round)
}

// endRoundWhosePodWent is the one rule for the end of a round whose pod went, whichever reading
// said it went: the claims recording another pod in its place (reconcileIssuePodUpdate), or the
// runtime's answer that the pod is not there (endRoundOverAGonePod). The ends are tried in this
// order, and the order is the rule:
//
//   - A launch into the round's pod replaced it as the round's own replacement: the round was
//     over by the rule for a pod with no role process in it, and the launch read that rule as it
//     ran and started its role in the pod in its place (roundPodEndsAtLaunch). That is the end
//     the round itself made, and it is ended as one, with its lines and its one message
//     (endRoundALaunchReplaced), not as a pod that went without its round. That end reads group
//     itself, unmasked (noticeGroup).
//   - Otherwise the pod went without the round, and a member of the round that had not answered
//     and whose work may be on the pod's disk (lostClaims) lost what it had not pushed, or, on a
//     kept volume, left it there: the issue is told so (podWentWithoutItsRound).
//   - Otherwise nothing is left to tell the issue, and the round ends (endPodUpdates). Where every
//     member of the round pushed its work before the pod went, the line is over alone. Where a
//     member had not answered but the work of no such member may be on the pod's disk, the line
//     is podWentNoWorkerLine, which names those members under ranNoWorkerInPod, since over may
//     say that every member answered.
//
// What was lost is read once, here, and counted exactly as the message counts it (lostClaims), so
// the message is written when, and only when, there is a role to name. Counted as the claims the
// replacement waits for (unansweredClaims), it would be written for a claim of the issue that is
// in no round at all — one an earlier sweep's end has already taken out of this round, or a role
// first started in the pod that replaced this one — naming either nobody or a worker that lost
// nothing. Whether the disk of the round's pod holds work from before the pod is read once here
// too (roundPodHoldsEarlierWork), for whichever end this is.
//
// The last two ends count group as the caller read it, and end the round on it. over is the line
// of the end where every member answered, which each caller words for the reading that found the
// pod gone, with the issue, the round and the pod.
func (s *supervision) endRoundWhosePodWent(ctx context.Context, issue string, group []supervise.Claim, round, pod, over string) {
	earlierWork := s.roundPodHoldsEarlierWork(ctx, issue, round)
	if launch, replaced := s.launchReplaced(issue, round); replaced {
		s.endRoundALaunchReplaced(ctx, issue, group, round, launch, earlierWork)
		return
	}
	if lost := lostClaims(group, round, earlierWork); len(lost) > 0 {
		s.podWentWithoutItsRound(ctx, issue, group, round, pod, lost)
		return
	}
	if unanswered := unansweredMembers(group, round); len(unanswered) > 0 {
		s.log.Info(podWentNoWorkerLine, "issue", issue, "round", round, "pod", pod, "ranNoWorkerInPod", namesOf(unanswered))
	} else {
		s.log.Info(over, "issue", issue, "round", round, "pod", pod)
	}
	s.endPodUpdates(issue, group, round)
}

// repositoryMove is what a message of a round says of the repository the round's pod was built
// from (podWentMessage, workspaceTakenMessage), as roundMove reads it. The zero value is
// no move: the record was read and holds that repository, or the round recorded none, and the
// message sends each role to origin. from and to together are a move: from is the repository the
// pod was built from, and to the one the record holds now. from alone is the round's repository
// where the record could not be read (unread).
type repositoryMove struct {
	from, to ghrepo.Repository
}

// unread is whether the message names the round's own repository because the issue's record
// could not be read (roundMove). Origin in the next pod is then unknown: a correction can have
// moved the record, so origin can hold none of the work the roles pushed.
func (m repositoryMove) unread() bool {
	return !m.from.IsZero() && m.to.IsZero()
}

// roundMove is the move a message of a round names: built, the repository the round's pod was
// built from, as the round recorded it at its open (roundRepository) or as the record of its
// replacement holds it (workspaceRead.origin), and the repository the issue's record holds when
// the message is written (recordedLeft). The record is read now and not taken from the round's
// reasons, which name it only on a round a correction opened, and as it stood at the round's
// open: a correction while the round ran moved the record on, whatever drift opened the round, and
// the issue's next pod is built from the record as it stands now. It is no move for a round that
// recorded no repository, whose pod named none, and for a round whose issue's record holds the
// pod's repository. A record that cannot be read is the round's own repository alone
// (repositoryMove.unread), never no move: a message that sent each role to origin would send it,
// after a correction, to a repository that holds none of its work.
func (s *supervision) roundMove(ctx context.Context, issue string, built ghrepo.Repository) repositoryMove {
	recorded, read := s.recordedLeft(ctx, issue, built)
	if !read {
		return repositoryMove{from: built}
	}
	if recorded.IsZero() {
		return repositoryMove{}
	}
	return repositoryMove{from: built, to: recorded}
}

// The lines of the end of a round whose pod went (endRoundWhosePodWent), each one statement, which
// the tests read by name:
//
//   - podWentOverLine is the end where every member answered, as the sweep that reads the claims
//     recording another pod in its place words it (reconcileIssuePodUpdate);
//   - podNotThereOverLine is the same end as the sweep that reads the pod gone from the runtime
//     words it (endRoundOverAGonePod);
//   - podWentNoWorkerLine is the end where a member had not answered, and no such member ran a
//     worker in that pod or has work on its disk: nothing was lost, although not every member
//     answered;
//   - podWentLostLine is the end where a member that had not answered lost what it had not pushed
//     on a disk that went with the pod (podWentWithoutItsRound), beside the message on the issue.
const (
	podWentOverLine     = "supervise: the pod this issue's update round was opened over is gone; the round is over"
	podNotThereOverLine = "supervise: the pod of this issue's update round is not there, and every claim of the round had answered; nothing was lost and the round is over"
	podWentNoWorkerLine = "supervise: the pod of this issue's update round is gone, and each member of the round that had not answered ran no worker in that pod; nothing was lost and the round is over"
	podWentLostLine     = "supervise: the pod of an open update round is gone; the roles that had not answered lost what they had not pushed"
)

// roundKeptVolume is whether the pod of the round that went had a kept volume that is still there
// and that the issue's next launch keeps, as the runtime answers it (runtime.IssueVolumeReader) for
// the time the round opened: a Sandbox made after that time holds a new volume, which holds nothing
// of the round's pod, and the next launch deletes a Sandbox that carries the reset mark or has
// another disk shape. Where no claim records when the round opened, or the runtime keeps no volume,
// the answer is false.
func (s *supervision) roundKeptVolume(issue string, group []supervise.Claim, round string) bool {
	volumes, ok := s.runtime.(runtime.IssueVolumeReader)
	if !ok {
		return false
	}
	opened := roundOpenedAt(group, round)
	if opened == nil {
		return false
	}
	return volumes.IssueKeepsVolume(issue, *opened)
}

// endRoundOverAGonePod ends the round the sweep has just read the pod of as gone. The report is
// decided from the parts of the round its claims hold: a claim the daemon started again in
// another pod keeps the part the dead pod left it (Claim.LeftPodRound, the round and the answer
// it left with), so no turn or answer of the process that replaced it changes the report.
//
// What was lost is the members of the round that neither answered it nor left it having
// answered, and whose work may be on the round's pod's disk (lostClaims), so a round every member
// of which pushed its work, or has no work on the pod's disk, lost nothing and is told to
// nobody: a message naming no role would say an issue lost work it did not lose. A claim of this
// issue that is in no round at all is a member of none, and is named by nothing here.
//
// Those answers are read from the machines as they stand rather than from the tick's snapshot,
// and unmasked (noticeGroup): an answer recorded is an answer whatever its machine is deciding
// now, and what this decides is a report of what is already gone, never the deletion of a disk —
// which is what the mask exists for (masked).
//
// A pod that is not there with no round open over it decides nothing, as a group whose claims
// record no pod and hold no round does: there is no round to end, the drift of a pod that is not
// there says nothing, and the next launch into the issue builds a pod from the configuration the
// runtime has now.
//
// A pod a launch replaced as its round's own replacement (roundPodEndsAtLaunch) is read gone here
// too, where no claim records the pod in its place yet or the runtime answers the claims' pod gone,
// and that round ends as the replacement the round made (endRoundALaunchReplaced), not as a pod
// that went without it.
//
// A round open over the gone pod ends by the one rule for a round whose pod went
// (endRoundWhosePodWent), counted unmasked as read above.
func (s *supervision) endRoundOverAGonePod(ctx context.Context, issue string, group []supervise.Claim, round, pod string) {
	if round == "" {
		s.log.Info("supervise: the pod this issue's claims record is not there, and no update round is open over it; nothing is decided of this issue",
			"issue", issue, "pod", pod)
		return
	}
	s.endRoundWhosePodWent(ctx, issue, s.noticeGroup(group), round, pod, podNotThereOverLine)
}

// openPodUpdate opens the round, or keeps it open, and asks each claim the round asks (roundAsks)
// once: one ask that reaches the claim's session, since an ask that reached none is asked again
// (askRound). Every claim with a session, and every claim whose launch waits on the round's own
// pod, is posted PodUpdateOpen at every tick while the round runs (postedClaims): the event is
// idempotent, and it is what records the round on the claim (supervise's openPodUpdate), with its
// reasons, the repository its pod was built from (repository, the zero Repository where the pod
// named none) and its clock fixed at the post that opens it.
//
// The ask is the sweep's own, enqueued on the reading that finds the round recorded (askRound).
// The post is asynchronous (supervisor.post), so the tick that opens a round asks nothing, and a
// later tick asks each claim whose record shows this round, no ask that reached it and no ask
// still queued for it, once its session is ready to take the ask. A claim that left the round's
// pod, a failed claim and a claim the round passes over are asked nothing (roundAsks,
// roundClock.passesOver).
//
// The ask reads the group the tick already read, as the claims hold it (noticeGroup; unaskedClaims
// says why never masked), and takes no machine's lock: the outbox's row takes the claim's lock
// when it publishes, and waits rather than block where a decision holds it (supervise's
// Machine.PublishUpdateAsk). So whether this tick's posts land before or after the ask changes
// nothing it decides.
//
// Every claim of the group with a session is posted the round (sessionClaims): an agent has run
// for it, and may hold unpushed work. So is a claim whose launch waits on the round's own pod,
// with a session or with none (postedClaims): it may be the one claim the round can be recorded
// on, and a round posted to nobody would be opened again at every sweep and carried at none, while
// that claim is refused at every try. Any other claim with no session runs no agent now and is
// posted nothing (sessionClaims says which claims those are). The round asks the claims roundAsks
// keeps and no other, and its wait set is the same claims (waitingClaims,
// dispatch://INFRA-4109 R2) less each claim whose launch waits on the round's own pod: such a
// claim runs no process that could answer, so the round waits on no answer of it (awaitsAnswer),
// and it says it is ready only once it runs a process, so the round never asks it either
// (askRound reads supervise.SaidReady). The replacement also waits for a claim that a project's
// pause still keeps and that the round does not ask (unansweredClaims, dispatch://INFRA-4109 R8).
// An operator-suspended claim holds the round until the operator acts, whatever the round's
// bound, which is this issue's own rule (readReplacement's waitsForOperator), as long as the
// round's pod is there: a gone pod ends its round whatever the hold (roundWithNoProcessInItsPod).
// The late-round notice is what tells the operator (tellLateRound, tellPodlessRound). A failed
// claim and a claim the daemon suspended run no turn on the pod and are asked nothing, so the
// round does not wait for them (roundAsks, dispatch://INFRA-4109 R1). A claim that a project's
// pause still keeps is the one exception (podHolds.owes, roundClock.passesOver): the round waits
// for it and asks it once its agent is ready. A worker whose plugin has no ready_for_update op is
// asked like any other.
//
// It tells nobody. A round that waits on its roles is no stall: its bound ends the wait whatever
// they say, and the readings that hold a round past its bound are told where they are read.
//
// It reports whether a claim's machine records the round once this tick's post lands, which is
// what the project's limits charge an opening for (reconcileIssuePodUpdate). Those are the claims
// noticeClaims keeps rather than every claim the round is posted to: a failed claim is posted like
// any other and its machine ignores the event (supervise's table), so a round whose claims with a
// session have all failed is recorded by no claim at all. A claim the daemon suspended records
// the round although the round asks it nothing, so it counts, and so does a claim whose launch
// waits on the round's own pod, with a session or with none (noticeClaims). An issue with no claim
// that records the round holds no slot: charged an opening all the same, it would be read as an
// opening of this daemon's still in flight at every later sweep (podUpdateOpenings), hold that
// slot for ever, and leave every other drifted issue waiting until the daemon restarts.
func (s *supervision) openPodUpdate(ctx context.Context, issue string, group []supervise.Claim, pod, reasons string,
	repository ghrepo.Repository, clock roundClock) bool {
	round := supervise.PodRound(pod)
	posted := postedClaims(group, round)
	reached := len(noticeClaims(group, round)) > 0
	s.askRound(ctx, issue, group, round, pod, clock)
	for _, c := range posted {
		s.supervisor.post(c.Token, supervise.PodUpdateOpen{Claim: c.Token, Round: round, Reasons: reasons, Repository: repository})
	}
	return reached
}

// updateStall is one round that has outlived its bound, as the operator's side is told of it:
// the issue and round it is of, the pod it was opened over (none where no claim of the issue
// records one, tellPodlessRound), what the round was opened for, every claim the notice lists
// (noticeClaims) with whether it answered, the reason that names what holds the round, and the
// group whose claims mark the round told. Nothing is written on the issue: a round held past its
// bound asks nothing of its workers that they have not already been asked, and the operator
// reads it through the controller.
//
// reasons is the one string the round records and the ask states (supervise.Claim.UpdateReasons),
// never a list: each reason carries ", " inside it already, so nothing here may cut one up
// (record.UpdateStalled).
type updateStall struct {
	issue   string
	round   string
	pod     string
	reasons string
	waiting []record.UpdateWaiting
	reason  string
	group   []supervise.Claim
}

// tellUpdateStalled tells the controller that a round is held past its bound (the
// update-stalled notice, record.UpdateStalledNotice). One round is told once however many ticks
// find it open: the telling is marked on every claim of the round (UpdateToldAt), and a round
// already marked is told nothing more. The mark is written after the row is enqueued, so a row
// the store refuses is written again at the next tick rather than swallowed.
//
// A round over the pod of a claim of another project's issue, which the operator's spawn makes,
// queues no notice: its row would be keyed by that issue, which only that project's daemon runs
// (plan.enqueue). Its line is then the whole of its telling, as the stall backstop's line
// is for a claim no owner can be told of (tellStallBackstopped), and says that the controller is
// told nothing; the round is marked told, so the line is written once rather than at every tick.
func (s *supervision) tellUpdateStalled(ctx context.Context, stall updateStall) {
	if roundTold(stall.group, stall.round) {
		return
	}
	if s.plan.enqueue == nil {
		s.log.Warn("supervise: an update round is held past its bound, and this deployment has no workflow to tell the operator's side through",
			"issue", stall.issue, "round", stall.round, "reason", stall.reason)
		return
	}
	rows := []record.OutboxPayload{record.ControllerNotice{
		Kind:   record.UpdateStalledNotice,
		Reason: stall.reason,
		Update: &record.UpdateStalled{Round: stall.round, Pod: stall.pod, Reasons: stall.reasons, Waiting: stall.waiting},
	}}
	written, err := s.plan.enqueue(ctx, stall.issue, rows...)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Error("supervise: tell the operator's side that an update round is held past its bound; telling again at the next sweep",
				"issue", stall.issue, "round", stall.round, "error", err)
		}
		return
	}
	if written {
		s.log.Warn("supervise: an update round is held past its bound; the controller is told", "issue", stall.issue, "round", stall.round,
			"pod", stall.pod, "reason", stall.reason, "waiting", stall.waiting)
	} else {
		s.log.Warn("supervise: an update round is held past its bound; its issue is another project's, whose rows this daemon writes none of, so the controller is told nothing and this line is the whole of the telling",
			"issue", stall.issue, "round", stall.round, "pod", stall.pod, "reason", stall.reason, "waiting", stall.waiting)
	}
	s.markUpdateTold(ctx, stall.group, stall.round)
}

// markUpdateTold records on every claim of the round that this round was told, so no later tick
// tells it again. A claim whose machine the daemon no longer holds, or whose round has moved on,
// marks nothing (supervise's Machine.PodUpdateTold).
func (s *supervision) markUpdateTold(ctx context.Context, group []supervise.Claim, round string) {
	for _, c := range group {
		if c.UpdateRound != round {
			continue
		}
		m, ok := s.supervisor.Machine(c.Token)
		if !ok {
			continue
		}
		if _, err := m.PodUpdateTold(ctx, round); err != nil && ctx.Err() == nil {
			s.log.Error("supervise: record that this claim's update round was told", "claim", c.Token, "round", round, "error", err)
		}
	}
}

// askRound enqueues the round's ask for each claim the round asks that records this round and
// has no ask of it that reached a session (unaskedClaims): one record.UpdateAsk row per claim,
// each to the claim's own role on the issue, the architect's included, since every role whose
// process runs in the pod loses it when the round replaces the pod (dispatch://INFRA-2981 R1).
//
// The text is the round's own terms, as its claims recorded them: the reasons the round opened
// with (roundReasons), never the drift this tick read, and the moment the round ends at the
// latest, its recorded opening plus pod_update_grace_minutes (supervise.UpdateTask). So every
// role of one round reads one text, and the record an operator reads names the same reasons
// (dispatch://INFRA-2981 R9).
//
// It marks nothing. A claim is asked once an ask reached a session, and the outbox's row is what
// learns that: it publishes under the claim's own lock and marks the claim asked when the publish
// reached a session (outbox's updateAsk, supervise's Machine.PublishUpdateAsk). An ask whose role
// topic no live session held (notify.ErrNoHolder) marks nothing and is lost, not given, so the
// next sweep that finds the claim ready asks it again (dispatch://INFRA-4109 R4,
// dispatch://INFRA-4131 R4). The round waits for every claim it asks until its bound, and a claim
// marked over an ask it never read would hold the round, and every answered worker's next task,
// to that bound for an answer nobody asked it for. A mark made here, at the enqueue, could be
// neither taken back by the row it was made for nor kept from a row it was not made for: the row
// can run before the mark is written, and a second row of the round can run after it.
//
// So that one ask of a round is in flight per claim, a claim whose ask of this round still waits
// in the outbox is skipped (store's QueuedUpdateAsks): one indexed read for the issue, made only
// once some claim of the issue is to be asked. Its row either reaches the session and marks the
// claim, or ends with nothing marked, and the sweep after that asks again. The cost of a ready
// claim whose session does not hold its topic is one row and one log line a sweep at most, for
// as long as that lasts: until the round's bound, or, under an operator's suspension, which keeps
// the round open past its bound (completePodUpdate's waitsForOperator), until the suspension ends.
//
// Every row of the round goes in one enqueue, so the round's asks land together or not at all
// (workflowRuntime.enqueueOutbox). An enqueue that fails, or a read of the queued asks that
// fails, enqueues nothing, and the next tick asks again.
//
// A claim no machine of this daemon holds is asked nothing: its row would find no machine to
// publish under and end, so every tick would enqueue it again.
//
// A claim whose agent has not said it is ready (supervise.SaidReady) is asked at a later tick.
// Such a claim is queued, launching, registered but not ready, suspended by an operator, or
// stopped by a project's pause that still keeps it by the one exception (podHolds.owes; the round
// asks no other claim the daemon suspended: roundAsks, roundClock.passesOver). No live session of
// it holds the role's topic, so the publish would end at notify.ErrNoHolder at every sweep, and
// the machine refuses the ask of a claim the pause stopped before any publish (supervise's
// UpdateAskSuspended): a row and a log line a minute for a claim that may stay suspended until the
// operator resumes it or the pause's resume launches it. The first tick after the agent says it
// is ready asks it. A ready session claims its topic inside its first turn or at the first-turn
// bound (supervise.SaidReady), so an ask in that short window can still end at
// notify.ErrNoHolder, and the claim is then asked again at a later sweep, as above.
//
// The ask states the round's bound (roundClock.roundBound), read at the clock's now with the time
// a project's pause held the pod. A round no claim of group records the opening of has no bound
// to state, and its ask would state a wrong end, so the sweep logs an error and publishes
// nothing. The one write of a round on a claim records its opening in the same statement
// (supervise's openPodUpdate), so this is never expected.
//
// A deployment with no outbox (enqueue nil) publishes nothing. The round still runs: it records
// itself on the claims, and its roles can answer it all the same. Nor does a round over the pod of
// a claim of another project's issue, which the operator's spawn makes: its ask would be a row
// keyed by that issue, which only that project's daemon runs, so enqueue writes none. With no row
// queued, no Machine.PublishUpdateAsk marks the claim asked, so each tick reads the queued asks
// again, finds none, and logs at Debug that the ask is published to no role, writing nothing.
func (s *supervision) askRound(ctx context.Context, issue string, group []supervise.Claim, round, pod string, clock roundClock) {
	var asking []supervise.Claim
	for _, c := range unaskedClaims(group, round, clock) {
		if !supervise.SaidReady(c.State) {
			continue
		}
		if _, ok := s.supervisor.Machine(c.Token); !ok {
			continue
		}
		asking = append(asking, c)
	}
	if len(asking) == 0 {
		return
	}
	if s.plan.enqueue == nil {
		s.log.Debug("supervise: this deployment has no outbox, so the update round's ask is published to no role",
			"issue", issue, "round", round, "roles", namesOf(asking))
		return
	}
	bound := clock.roundBound(group, round)
	if bound == nil {
		s.log.Error("supervise: no claim of this update round records when the round opened, so its ask has no end to state; the ask is published to no role",
			"issue", issue, "round", round, "roles", namesOf(asking))
		return
	}
	queued, err := s.supervisor.store.QueuedUpdateAsks(ctx, issue, round)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Error("supervise: read the update round's asks still queued; asking again at the next sweep",
				"issue", issue, "round", round, "roles", namesOf(asking), "error", err)
		}
		return
	}
	asking = slices.DeleteFunc(asking, func(c supervise.Claim) bool { return queued[c.Role] })
	if len(asking) == 0 {
		return
	}
	reasons := roundReasons(group, round)
	rows := make([]record.OutboxPayload, len(asking))
	for i, c := range asking {
		rows[i] = record.UpdateAsk{Role: c.Role, Round: round, Pod: pod, Text: supervise.UpdateTask(c.Role, round, reasons, *bound)}
	}
	written, err := s.plan.enqueue(ctx, issue, rows...)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Error("supervise: enqueue the update round's ask to its roles; asking again at the next sweep",
				"issue", issue, "round", round, "roles", namesOf(asking), "error", err)
		}
		return
	}
	if !written {
		s.log.Debug("supervise: the update round's ask is published to no role: its issue is another project's, whose rows this daemon writes none of",
			"issue", issue, "round", round, "roles", namesOf(asking))
		return
	}
	s.log.Info("supervise: the update round's ask is enqueued for each role of the pod",
		"issue", issue, "round", round, "pod", pod, "roles", namesOf(asking))
}

// unaskedClaims is every claim the round asks at the clock's now (roundAsks) whose own record
// shows this round, no answer to it and no ask mark (supervise.Claim.UpdateTaskedAt): no ask of
// the round has reached its session yet (supervise's Machine.PublishUpdateAsk). A claim that has
// not recorded the round yet is asked at the tick after its post lands, so the ask and the round
// it names are one claim's record. A claim that left the round's pod and a claim the round passes
// over are asked nothing, because roundAsks leaves both out. A claim that a project's pause still
// keeps is in, by the one exception (podHolds.owes, roundClock.passesOver), and askRound asks it
// once its agent is ready, as it asks any claim.
//
// The answer it reads is the claim's own, never masked (masked): only the claim leaving the round
// clears its answer (supervise's clearUpdate, at the round's end, at another round's opening or at
// the claim's retirement), so a decision in flight has not withdrawn an answer the view shows. A
// claim can hold an answer and no ask mark: the deploy's reset (migration 20261113110610) clears
// the mark and keeps the answer. Read masked, such a claim would be asked again whenever its
// machine was deciding at the read.
func unaskedClaims(group []supervise.Claim, round string, clock roundClock) []supervise.Claim {
	var asking []supervise.Claim
	for _, c := range group {
		if roundAsks(c, round, clock) && c.UpdateRound == round && c.UpdateTaskedAt == nil && !answered(c, round) {
			asking = append(asking, c)
		}
	}
	return asking
}

// endPodUpdates ends round of the issue on every claim of the group that is one of its members
// (memberOf), and on no other. It is the one way the sweep ends a round: the round's own
// replacement (replacePodForRound, by issue replaceIssuePodForRound, or at a launch
// endRoundALaunchReplaced), its pod gone with another pod recorded in its place
// (reconcileIssuePodUpdate), its pod read gone (endRoundOverAGonePod) and its pod gone with a
// member's work (podWentWithoutItsRound) all end it here, and so does the round's keep of its pod
// for the work on its disk (keepPodForRound). A round ends where the pod it was opened over is
// gone — replaced by the round itself, replaced under it, or gone for a failure of its own — or
// where the round kept it, so the post carries nothing of that pod's fate
// (supervise.PodUpdateEnded). What the launches into the pod left of the round goes with it
// (forgetLaunchRound): no launch into that pod is left to keep it for, and no end is left to make.
//
// The end is kept as the issue's ended round (podUpdateEnded) in the same step that posts it. The
// post is asynchronous, and a member's machine in a relaunch takes it only once the relaunch is
// done, so the sweeps until then find the round on that member's claim. The kept end is what stops
// those sweeps from deciding the round again (reconcileIssuePodUpdate): an end made anywhere else
// would be made again at every sweep until each member took it.
//
// A claim of the group in another round is left in it. Read while a replacement settles, one
// group can hold the dead pod's round on one claim and the round over the pod that replaced it on
// another, and posted each claim's own round, the end of the one would end the other as well: a
// round whose workers have been given the steps of its update message (supervise.UpdateTask),
// over a pod that is still there.
func (s *supervision) endPodUpdates(issue string, group []supervise.Claim, round string) {
	s.forgetLaunchRound(issue, round)
	s.keepEndedRound(issue, round)
	for _, c := range group {
		if !memberOf(c, round) {
			continue
		}
		s.supervisor.post(c.Token, supervise.PodUpdateEnded{Claim: c.Token, Round: round})
	}
}

// keepEndedRound keeps round as the round of the issue the sweep has ended (podUpdateEnded), in
// place of whatever was kept for the issue before. One round is kept for each issue. A second end
// on the issue while a claim still records the first needs two rounds on one group and two
// machines in a relaunch at once, and then the first round is read again and ended once more, as
// a restart ends it.
func (s *supervision) keepEndedRound(issue, round string) {
	s.podUpdateEndedMu.Lock()
	defer s.podUpdateEndedMu.Unlock()
	if s.podUpdateEnded == nil {
		s.podUpdateEnded = map[string]string{}
	}
	s.podUpdateEnded[issue] = round
}

// endedRounds is the rounds the sweep has ended that a claim of their issue still records, by
// issue, read once before the walk (beginPodUpdateSweep). An entry goes at the first sweep where
// no claim of its issue records its round: every member's machine has taken the end, and nothing
// of the round is left to read as over. byIssue is the sweep's groups, so an issue with no live
// claim records no round. What is answered is a copy, so the walk reads it without this mutex.
func (s *supervision) endedRounds(byIssue map[string][]supervise.Claim) map[string]string {
	s.podUpdateEndedMu.Lock()
	defer s.podUpdateEndedMu.Unlock()
	for issue, round := range s.podUpdateEnded {
		if !recordsRound(byIssue[issue], round) {
			delete(s.podUpdateEnded, issue)
		}
	}
	return maps.Clone(s.podUpdateEnded)
}

// roundOver is whether round, a round of the issue, is over as group, the issue's claims, and the
// sweep's ends read it: no claim of group records it, or the sweep has ended it and a member's
// machine has not taken the end yet (keepEndedRound). A round's record of a replacement whose round
// is over holds nothing of the issue (finishReplacements), and its round is ended no second time
// (endTakenRound, roundReplaced).
func (s *supervision) roundOver(issue string, group []supervise.Claim, round string) bool {
	if !recordsRound(group, round) {
		return true
	}
	s.podUpdateEndedMu.Lock()
	defer s.podUpdateEndedMu.Unlock()
	return s.podUpdateEnded[issue] == round
}

// podReading is what one sweep could read of the pod the group's claims record, and the one
// thing that says what the reasons beside it are worth (groupDrift). The three answers are
// decisions, not degrees of one: a pod that has drifted from nothing is still running and its
// round replaces it, a pod that cannot be read decides nothing at all, and a pod that is not
// there has taken its disk with it and ends the round open over it. No two of them may be read
// from the same empty list of reasons, which is why this is an answer of its own rather than a
// flag a caller may drop.
//
// podUnread is the zero value, so a reading nobody set decides nothing: of the three, it is the
// only one that is safe to be wrong about.
type podReading int

const (
	// podUnread is the runtime refusing the question: an error is never a statement that
	// something drifted, and a round must neither open nor end on one.
	podUnread podReading = iota
	// podRead is the pod being there and read, and only under it do the reasons beside this
	// reading mean anything: no reason at all is then a pod that has drifted from nothing.
	podRead
	// podGone is the pod the claims record not being there (runtime.ErrPodGone): it was replaced
	// where it stood for a failure of its own, or it is on its way out for an act that is not
	// this round's, and whatever was unpushed on its disk went with it.
	podGone
)

// podDrift is what one sweep read of the pod the group's claims record (groupDrift): why that
// pod has drifted and the reading that says what the reasons are worth. The two are one value
// because neither decides anything alone — no reason is a pod that drifted from nothing only
// under podRead — and a caller that took them as two results could hold the reasons of one
// round beside the reading of another.
type podDrift struct {
	// reasons is why the pod has drifted, in the claims' own order and without repeats, empty
	// for a pod that has drifted from nothing.
	reasons []string
	// repository is the repository the pod was built from, as the pod names it (repositoryDrift),
	// which the round this drift opens records whatever drift opened it
	// (supervise.Claim.UpdateRepository), and the zero Repository where nothing could say.
	repository ghrepo.Repository
	// reading is what could be read of the pod at all (podReading).
	reading podReading
}

// groupDrift is why the group's pod has drifted: the union of the runtime's PodDrift over every
// claim of the group that records a process or a pod its launch waits on (podLocator), in the
// claims' own order and without repeats, and then the repository the pod was built from read
// against the repository the issue's record holds now (repositoryDrift), with the reading that
// says what the reasons are worth (podReading). A claim's awaited pod is asked about as the pod of
// a process there is: the runtime refused that claim's launch because the pod lacks the exchange
// identity a pod built now carries, which is drift PodDrift reports of the pod, so the round opens
// over it.
//
// It reads podUnread when the runtime could not be asked at all: an error is never a statement
// that something drifted, and a round must neither open nor end on one.
//
// The pod the claims record not being there is the one error of its own (runtime.ErrPodGone),
// and it reads podGone, which is a statement rather than a failure: nothing more can be read of
// that pod, and the round open over it is over with it (endRoundOverAGonePod). Read as a pod
// that has drifted from nothing, it would carry the round on over a pod that took its workers'
// unpushed work with it. Read as nothing at all, it would leave that round open over a pod that is
// not there. The caller is what logs it, because the caller is what decides it.
//
// The repository is asked once for the group, through the one helper every reader of the pod's
// repository asks it by (podRepository), because it is the pod's and not the claim's: every role
// of the issue runs in that one pod and works the one workspace its fetch cloned, so one
// correction opens one round rather than one per claim, and the idle rule's repository hold
// reads the pod through the same claim (podRepositoryDrifted). A runtime that answers nothing —
// one whose processes share no such place, a pod that clones no repository — and a group no
// claim of which records a process end the question there. A read that fails reads podUnread, as
// a failed drift read does: a round opened on it would record no repository for its whole life,
// since the open fixes its terms (supervise.Claim.UpdateRepository).
//
// The pod's repository goes with the drift whatever the record holds, so every round records the
// repository its pod was built from at its open, and not only a round a correction opened: a
// correction can land while a round that another drift opened runs, and each message of that
// round on the issue reads the record against that repository when it is written (roundMove's
// callers).
func (s *supervision) groupDrift(ctx context.Context, issue string, group []supervise.Claim) podDrift {
	var reasons []string
	for _, c := range group {
		loc := podLocator(c)
		if loc == nil {
			continue
		}
		drift, err := s.runtime.PodDrift(ctx, runtime.Known{Claim: c.Token, Locator: loc})
		if errors.Is(err, runtime.ErrPodGone) {
			return podDrift{reading: podGone}
		}
		if err != nil {
			if ctx.Err() == nil {
				s.log.Error("supervise: read whether this issue's pod has drifted", "issue", issue, "claim", c.Token, "error", err)
			}
			return podDrift{reading: podUnread}
		}
		for _, reason := range drift {
			if !slices.Contains(reasons, reason) {
				reasons = append(reasons, reason)
			}
		}
	}
	repository, err := podRepository(ctx, s.runtime, slices.Values(group))
	if err != nil {
		if ctx.Err() == nil {
			s.log.Error("supervise: read the repository this issue's pod was built from", "issue", issue, "error", err)
		}
		return podDrift{reading: podUnread}
	}
	built, reason := s.repositoryDrift(ctx, issue, repository)
	if reason != "" && !slices.Contains(reasons, reason) {
		reasons = append(reasons, reason)
	}
	return podDrift{reasons: reasons, repository: built, reading: podRead}
}

// repositoryDrift is the one reason the runtime cannot state itself: the pod was built from one
// repository, and the issue's record names another now, because Dispatch moved the issue and the
// daemon's record followed it (record.Store.CorrectIssueRepository). The pod clones its
// repository into the issue's workspace once, at its creation, and holds the gh directories and
// the token of that repository's owner for its whole life, so the issue works the repository it
// has left until the pod is replaced.
//
// The two names are read by the one rule the daemon decides whether an issue has left a
// repository by (repositories.left, ghrepo.Renames.Same): without regard to case, as GitHub
// compares names, and a configured rename's two names as one repository, so re-pointing an issue
// at the name a transfer moved its repository to replaces no pod.
//
// Nothing is a reason but two names that are both there and differ. A pod that answers no name
// and a name that is no repository add none here, and recordedLeft says where the record adds
// none: an unknown is never a statement that something drifted, which is the rule this sweep
// already reads an error by. It answers the pod's repository, parsed, beside the reason and with
// no reason too, so every round records its pod's repository as a value rather than as text
// (supervise.Claim.UpdateRepository), whatever drift opened it (groupDrift).
func (s *supervision) repositoryDrift(ctx context.Context, issue, pod string) (ghrepo.Repository, string) {
	if pod == "" {
		return ghrepo.Repository{}, ""
	}
	built, err := ghrepo.Parse("the repository this issue's pod was built from", pod)
	if err != nil {
		s.log.Error("supervise: the repository this issue's pod names is no repository; the pod is read as built from none",
			"issue", issue, "error", err)
		return ghrepo.Repository{}, ""
	}
	recorded, _ := s.recordedLeft(ctx, issue, built)
	if recorded.IsZero() {
		return built, ""
	}
	return built, repositoryReason(built, recorded)
}

// recordedLeft is the repository the issue's record holds now where the issue has left pod, the
// repository its pod was built from, read by repositories.left as repositoryDrift says, and the
// zero Repository wherever nothing says it has: no pod repository, a deployment with no workflow
// (plan.recordedRepository nil), an issue the record does not hold, a record that names the
// pod's repository, and a record that cannot be read or whose name is no repository, each of the
// last two logged. read is false for those last two alone, the record whose answer nobody has:
// the round's open adds no reason for it (repositoryDrift), as for every unknown, while a message
// of the round on the issue names the round's own repository for it rather than origin
// (roundMove), since origin in the next pod is the record's repository, which nothing read. Each
// reads it at its own moment.
func (s *supervision) recordedLeft(ctx context.Context, issue string, pod ghrepo.Repository) (left ghrepo.Repository, read bool) {
	if pod.IsZero() || s.plan.recordedRepository == nil {
		return ghrepo.Repository{}, true
	}
	recorded, err := s.plan.recordedRepository(ctx, issue)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Error("supervise: read the repository this issue's record holds", "issue", issue, "pod", pod.String(), "error", err)
		}
		return ghrepo.Repository{}, false
	}
	if !s.plan.repositories.left(pod.String(), recorded) {
		return ghrepo.Repository{}, true
	}
	left, err = ghrepo.Parse("the repository this issue's record holds", recorded)
	if err != nil {
		s.log.Error("supervise: the repository this issue's record holds is no repository; it is read as unknown",
			"issue", issue, "pod", pod.String(), "error", err)
		return ghrepo.Repository{}, false
	}
	return left, true
}

// repositoryReason is the reason repositoryDrift adds to a round, as the round's ask states it to
// each role and as an operator reads it: the repository the pod was built from, then the one the
// issue's record holds at the round's open. It opens on runtime.BreakingDrift, because the drift
// breaks the pod (R3 of #3712, spec v8): the pod clones the repository the issue has left and
// holds that repository's owner's gh directories and token for its whole life, so no role can
// move the work onto the corrected repository in it, and only a replacement does. A round over it
// replaces the pod whatever the read of the workspace finds (roundKeeps), no earlier keep holds it
// back (podKeptForWork), and the issue's architect is told what the replacement took
// (workspaceTakenMessage). Nothing reads it back: the round records the pod's repository as a
// field of its own (supervise.Claim.UpdateRepository).
func repositoryReason(pod, recorded ghrepo.Repository) string {
	return runtime.BreakingDrift + "repository " + pod.String() + ", now " + recorded.String()
}

// recordedRepositoryOf is the reader the plan takes (plan.recordedRepository), built
// on the workflow's own single-issue read: the repository the record names, and "" for an issue
// the record does not hold, which is every issue of a project the workflow has not admitted.
func recordedRepositoryOf(read func(ctx context.Context, key string) (*record.Issue, error)) func(context.Context, string) (string, error) {
	return func(ctx context.Context, issue string) (string, error) {
		recorded, err := read(ctx, issue)
		if err != nil || recorded == nil {
			return "", err
		}
		return recorded.Repo, nil
	}
}

// groupPod is the pod every claim of the group that records one runs in, or that a claim's launch
// waits on (podLocator), and whether they record two different pods, which is a replacement
// already in flight.
func groupPod(group []supervise.Claim) (string, bool) {
	pod := ""
	for _, c := range group {
		found := podOf(podLocator(c))
		if found == "" {
			continue
		}
		if pod != "" && pod != found {
			return "", true
		}
		pod = found
	}
	return pod, false
}

// podOf is the pod a locator names, and "" for a claim with no process and for a runtime whose
// processes are in no pod (a tmux pane).
func podOf(locator *runtime.Locator) string {
	if locator == nil || locator.Sandbox == nil {
		return ""
	}
	return locator.Sandbox.PodUID
}

// podLocator is where c stands in its issue's pod for a round: the process it records, or, where
// it records none, the live pod its launch waits on because that pod waits for its update round
// (supervise.Claim.AwaitsPod). Such a claim is refused at every try until the round replaces the
// pod, and it may be the issue's only claim, so a round read from the process alone would never
// open over that pod, and the claim would wait for ever.
func podLocator(c supervise.Claim) *runtime.Locator {
	if c.Locator != nil {
		return c.Locator
	}
	return c.AwaitsPod
}

// awaitsRoundPod is whether c records no process and its launch waits on round's own pod
// (podLocator). Such a claim has no process and no turn in that pod: the round is posted to it
// whether or not it has a session (postedClaims), asks it nothing, since it never said it is
// ready (askRound), and waits on no answer of it (awaitsAnswer, waitingClaims). The pod is replaced
// through it where no claim records a process there (podProcess). lostClaims says which such
// claims a gone pod, or that replacement, names on the issue as having lost work, and
// unwaitedOnThePod which ones a replacement's line names.
func awaitsRoundPod(c supervise.Claim, round string) bool {
	return c.Locator == nil && c.AwaitsPod != nil && supervise.PodRound(podOf(c.AwaitsPod)) == round
}

// inRoundPod is whether c records a process in the round's own pod: the pod podProcess ends the
// round through, and the only pod whose background work the round weighs (backgroundClaims). A
// claim the sweep relaunched while the round ran records the pod that replaced the round's own,
// which is no process in it (roundPodEndsAtLaunch).
func inRoundPod(c supervise.Claim, round string) bool {
	return c.Locator != nil && supervise.PodRound(podOf(c.Locator)) == round
}

// openPodRound is the pod round open over the group, and "" when none is. Every claim the sweep
// groups is live (reconcilePodUpdates drops the retired ones), so the round read here is one a
// claim of this issue is still in.
func openPodRound(group []supervise.Claim) string {
	for _, c := range group {
		if c.InPodRound() {
			return c.UpdateRound
		}
	}
	return ""
}

// roundReasons is what the round was opened for, as its claims recorded it: the one string the
// round's ask states (record.UpdateStalled).
func roundReasons(group []supervise.Claim, round string) string {
	for _, c := range group {
		if c.UpdateRound == round && c.UpdateReasons != "" {
			return c.UpdateReasons
		}
	}
	return ""
}

// roundRepository is the repository the round's pod was built from, as its claims recorded it at
// the round's open (supervise.Claim.UpdateRepository), and the zero Repository for a round whose
// pod named none.
func roundRepository(group []supervise.Claim, round string) ghrepo.Repository {
	for _, c := range group {
		if c.UpdateRound == round && !c.UpdateRepository.IsZero() {
			return c.UpdateRepository
		}
	}
	return ghrepo.Repository{}
}

// roundOpenedAt is when the round opened, the earliest its claims record, and nil when no claim of
// the group is in it.
func roundOpenedAt(group []supervise.Claim, round string) *time.Time {
	var opened *time.Time
	for _, c := range group {
		if c.UpdateRound != round || c.UpdateOpenedAt == nil {
			continue
		}
		if opened == nil || c.UpdateOpenedAt.Before(*opened) {
			opened = c.UpdateOpenedAt
		}
	}
	return opened
}

// answered is whether c has answered this round: the round it records is this one, and it
// signalled in that round. It is the one statement of "answered this round", read by every place
// the round's answers are weighed — the claims it still waits on before its bound (waitingClaims,
// unansweredClaims), the claims the line of a bounded replacement names as having answered
// (answeredClaims) and the mark the controller's notice carries per claim (waitingOf). An answer
// a claim gave to another round says nothing of this one: the claim's recorded answer goes with
// the round that ends (clearUpdate), and a round that replaced another promises something else
// of the pod and the disk.
func answered(c supervise.Claim, round string) bool {
	return c.UpdateRound == round && c.UpdateSignalledAt != nil
}

// answeredClaims is every claim the round asks at the clock's now (roundAsks) that has answered
// it.
func answeredClaims(group []supervise.Claim, round string, clock roundClock) []supervise.Claim {
	var of []supervise.Claim
	for _, c := range group {
		if roundAsks(c, round, clock) && answered(c, round) {
			of = append(of, c)
		}
	}
	return of
}

// heldByOperator is every claim the round asks (roundAsks) that an operator's suspension holds
// (supervise's Claim.RuleHolds): one the operator suspended, or one whose operator suspension
// waits for its agent's turn to end. It is the one wait of a round that outlasts the round's
// bound, because it is a person's (dispatch://INFRA-2981 R5): where a suspended claim is the
// pod's only role its process is gone and nothing else would hold the pod, and where another
// role still runs, nothing but this reading would hold it once the bound steps over the answers.
//
// A claim that left the round's pod (supervise's Claim.LeftPodRound) holds nothing of it: its
// process and its work are in another pod, and the round asks it nothing (roundAsks). The clock
// is the one roundAsks reads; no claim an operator holds is one the round passes over at any
// moment (roundClock.passesOver).
func heldByOperator(group []supervise.Claim, round string, clock roundClock) []supervise.Claim {
	var held []supervise.Claim
	for _, c := range group {
		if roundAsks(c, round, clock) && c.RuleHolds() {
			held = append(held, c)
		}
	}
	return held
}

// roundTold is whether this round was already told to the operator's side.
func roundTold(group []supervise.Claim, round string) bool {
	for _, c := range group {
		if c.UpdateRound == round && c.UpdateToldAt != nil {
			return true
		}
	}
	return false
}

// sessionClaims is every claim of the group with a session: an agent has run for it, and may hold
// unpushed work. The round is posted to these claims, and to each claim whose launch waits on the
// round's own pod (postedClaims). The late-round notice lists the posted claims that have not
// failed (noticeClaims), and the replacement's line names the ones of these the round does not
// ask (notWaitedFor). The round asks and waits for a part of them (roundAsks). Every claim the
// sweep groups is live already (reconcilePodUpdates drops the retired ones).
//
// A claim with no session runs no agent now. No agent ran for it yet, or it forgot its session
// and its new agent has not registered, as a launch that forgot its role's lost session does
// (supervise's startFresh). A claim of the second kind had a worker, and that worker's unpushed
// work can be on the pod's disk. So what a pod took with it is not read from this set
// (lostClaims), and a claim that a project's pause still keeps holds the replacement with a
// session or with none (unansweredClaims).
func sessionClaims(group []supervise.Claim) []supervise.Claim {
	var sessions []supervise.Claim
	for _, c := range group {
		if c.Session != "" {
			sessions = append(sessions, c)
		}
	}
	return sessions
}

// postedClaims is every claim of the group the round is posted to (openPodUpdate): each claim with
// a session (sessionClaims), and each claim whose launch waits on this round's own pod
// (awaitsRoundPod), with a session or with none. Every claim the sweep groups is live already
// (reconcilePodUpdates drops the retired ones).
//
// A claim whose launch waits on the round's pod is the round's own way to that pod where no claim
// records a process in it (podProcess), and it may be the issue's only claim. A round is open only
// while a claim records it (openPodRound), so such a claim is posted the round whether or not it
// has a session: a round posted to nobody would be opened again at every sweep and carried at
// none, and that claim would be refused at every try. It records the round, and the round waits
// on no answer of it (awaitsAnswer). A claim with no session whose launch waits on another pod is
// posted nothing: it is no way to this round's pod, and no agent runs for it now.
func postedClaims(group []supervise.Claim, round string) []supervise.Claim {
	var posted []supervise.Claim
	for _, c := range group {
		if c.Session != "" || awaitsRoundPod(c, round) {
			posted = append(posted, c)
		}
	}
	return posted
}

// waitingClaims is whom the round waits for before its bound, the round's wait set: every claim of
// the group that the round asks at the clock's now and that has not answered this round
// (awaitsAnswer), in the group's order. The replacement waits for each of them, and the line of a
// replacement the bound made names each of them as not having answered, beside the claims the
// pause still keeps that the round does not ask (unansweredClaims). Before its bound the round
// reads its workspace only once none of them is left and no turn runs (readReplacement). It is
// deliberately the conservative of the round's two questions, because what it decides before the
// bound is the deletion of a disk. A claim the round has not reached yet — one whose recorded
// round is another, or none — has not answered it either, and it holds the pod until it does or
// the bound comes: the
// round asks it at the next tick, and a replacement made before that would end a turn of a worker
// the round asked nothing of.
//
// A claim that has left *this* round waits for nothing (supervise's Claim.LeftPodRound, which is
// the one statement of that rule, and which roundAsks asks rather than restates). The pod it was
// in is gone, the round asks it nothing more, and a replacement held for its answer would be held
// for an answer no process will give.
//
// A claim that left some earlier round is not that claim. It still records the round it left,
// this round's open has not reached it yet, and it holds this round's pod exactly as any claim
// the round has not reached does, which is why the predicate is asked about the round being
// decided rather than about the claim's own: read of the claim's own round, such a claim would
// hold the pod no longer, and its disk could be deleted with work on it the round had asked
// nobody for.
//
// A claim the round passes over waits for nothing either (roundAsks, roundClock.passesOver): any
// claim the daemon suspended but the one exception, a claim that a project's pause still keeps
// (podHolds.owes). It runs no process and no turn in the pod, and the round does not ask it. A
// replacement held for its answer would be held to the bound for an answer that nobody asked for
// (dispatch://INFRA-4109 R1). A claim the pause still keeps is not passed over: the pause, not the
// claim, kept it from a turn, and the work on the pod's disk is what the pause kept the pod for.
// The round asks it and waits for it once it has a session. With no session it is in neither of
// the round's sets, and the replacement waits for it all the same (unansweredClaims).
//
// A claim whose launch waits on this round's own pod waits for nothing either (awaitsRoundPod).
// It records no process, so it has no turn in that pod and can give no answer: no process of it
// has said it is ready, so the round never asks it (askRound), and each of its launches is
// refused until the round replaces the pod. A replacement held for its answer would wait out the
// round's bound for an answer nobody can give, and where it is the issue's only claim that wait
// is all the round would do. The round asks it (roundAsks), and its wait set leaves it out
// (awaitsAnswer).
//
// The round waits on no answer of a claim whose launch waits on this round's pod, and whether
// that claim lost work is the other question (lostClaims).
func waitingClaims(group []supervise.Claim, round string, clock roundClock) []supervise.Claim {
	var waiting []supervise.Claim
	for _, c := range group {
		if awaitsAnswer(c, round, clock) {
			waiting = append(waiting, c)
		}
	}
	return waiting
}

// unansweredClaims is every claim of the group that holds the round's replacement through a
// process for an answer before the round's bound, in the group's order and each named once: each
// claim the round waits for (awaitsAnswer), and each claim that a project's pause still keeps
// (podHolds.owes), the ones the round does not ask included. It is the one statement of what the
// round's end reads as unanswered (readRoundEnd, dispatch://INFRA-4109 R8). The walk's reading and
// the last word hold the replacement for these claims before the bound (roundEnd.holding), and the
// line of a replacement the bound made names them. A claim of the pause that the round does not
// ask does not stop the round's read of its workspace: it runs no process in the pod.
//
// The round asks no claim with no session (roundAsks). But a launch that forgot its role's lost
// session records no session until its new agent registers (supervise's startFresh), and the pause
// kept the pod for that role's work (decision 12 of dispatch://INFRA-3759). So such a claim holds
// the replacement while the pause still keeps it, as it holds the pod of a round with no process
// in it (roundPodEnds). The round's two sets do not change (R2): the round neither asks the claim
// nor counts it in waitingClaims.
func unansweredClaims(group []supervise.Claim, round string, clock roundClock) []supervise.Claim {
	var of []supervise.Claim
	for _, c := range group {
		if awaitsAnswer(c, round, clock) || clock.holds.owes(c, round) {
			of = append(of, c)
		}
	}
	return of
}

// awaitsAnswer is whether the round, read at the clock's now, waits for c's answer: the round asks
// c (roundAsks), c has not answered this round (answered), and c's launch does not wait on the
// round's own pod (awaitsRoundPod), since no process of such a claim can answer. It is the one
// per-claim statement of the round's wait set (waitingClaims), and the claims that hold the
// replacement for an answer read it too (unansweredClaims).
func awaitsAnswer(c supervise.Claim, round string, clock roundClock) bool {
	return roundAsks(c, round, clock) && !answered(c, round) && !awaitsRoundPod(c, round)
}

// roundAsks is whether the round, read at the clock's now, asks c and waits for its answer. It is
// the one predicate that both sets of the round read (dispatch://INFRA-4109 R2): the claims the
// sweep asks (unaskedClaims, askRound) and the claims the round waits for (waitingClaims),
// with the operator's hold (heldByOperator) and the answers a bounded replacement names
// (answeredClaims). So no claim is in the round's wait set and not asked. One claim the round asks
// is in no wait of it: a claim whose launch waits on the round's own pod (awaitsRoundPod), which
// runs no process that could answer, and which says it is ready only once it runs one, so the
// sweep never asks it either (awaitsAnswer). The replacement also waits for a claim that a
// project's pause still keeps and that the round does not ask, which is in neither set
// (unansweredClaims, dispatch://INFRA-4109 R8). Two predicates held BET-1182's pod until its bound
// on 2026-11-16: the replacement waited for every claim with a session that had not failed, and
// the sweep asked only a claim whose agent was ready. So a claim the daemon had suspended was
// waited for and never asked. The predicate takes the round's clock because one exception to R1
// is read from what a project's pause did to the pod (roundClock.passesOver).
//
// A claim the round asks is asked once its agent says it is ready (askRound reads
// supervise.SaidReady), and, but for a claim whose launch waits on the round's own pod, it is
// waited for until then. So a queued, launching or registered claim is in, and so is a claim that
// records another round or none: the round asks it at the tick after its post lands. A claim an
// operator suspended is in, and so is a claim whose operator suspension waits for its agent's turn
// to end: the round waits for the operator, whatever its bound (heldByOperator,
// dispatch://INFRA-2981 R5). A claim that a project's pause still keeps is in, by the one
// exception (podHolds.owes, roundClock.passesOver): the pause kept the pod for the work on its
// disk, and the round waits for its answer before the bound the hold moved.
//
// Five kinds of claim are out:
//
//   - A claim with no session runs no agent that an ask can reach (sessionClaims says which
//     claims those are). Where a project's pause still keeps such a claim, it holds the
//     replacement all the same, outside both sets (unansweredClaims).
//   - A retired claim runs nothing in any pod. The sweep drops retired claims before it groups
//     them (reconcilePodUpdates, roundGroup). The predicate leaves them out too, so it holds for
//     any group it is given.
//   - A failed claim's launches or prompts ran out, so it runs no process and no turn of it runs
//     on the pod. Its machine ignores the round's post, and only a retry of it brings it back
//     (supervise's onRetry and onOperatorRetry rows), which the round does not make. Holding the
//     round for its answer held the pod, and every answered worker's next task with it, for an
//     answer it can never give: on the first contract-46 roll a planner that had failed at
//     contract 41 recorded no round at all, and held its issue's round open behind the three
//     workers that had answered. A retry starts it again, and it is asked like any other claim
//     from the next tick.
//   - A claim that left the round's pod (supervise's Claim.LeftPodRound) runs its process in
//     another pod. The ask names work that died with that pod, and no process the claim runs now
//     holds any of it.
//   - A claim the round passes over (roundClock.passesOver): one the daemon suspended, by the
//     daemon's own rule, by a root's exit, by an idle release, or by an unknown source
//     (supervise's Claim.PassedOverByRound), but not the one exception, a claim that a project's
//     pause still keeps (podHolds.owes). Such a claim runs no process and no turn in the pod
//     (dispatch://INFRA-4109 R1). An ask would reach no session. A wake or a resume would
//     override the reason the daemon stopped the claim: an idle-released pod is not woken to be
//     asked, because it holds no turn to interrupt (dispatch://INFRA-2981). The outbox's row
//     refuses the ask of such a claim too (supervise's Machine.PublishUpdateAsk), so a row queued
//     before the suspension publishes nothing. It refuses the ask of a claim the pause keeps as
//     well, since a publish to a role with no holder raises a role wake; the sweep asks that
//     claim only once its agent is ready, and a ready claim is no longer suspended.
//
// Work that a claim left out of the round left unpushed is on the pod's disk, which the read
// before every replacement reads (readPodWorkspace, R7 of #5285): over a drift
// the pod's roles can keep working under, a read that finds work that origin does not hold, or
// that cannot be made, keeps the pod (roundKeeps). The replacement's log line names each failed
// claim and each claim the round passed over that it did not wait for and that had not answered
// (notWaitedFor), and what the read found.
func roundAsks(c supervise.Claim, round string, clock roundClock) bool {
	return c.Session != "" && c.State != supervise.StateFailed && c.State != supervise.StateRetired &&
		!c.LeftPodRound(round) && !clock.passesOver(c, round)
}

// noticeClaims is every claim of the group the round is posted to (postedClaims) that has not
// failed: each claim with a session, and each claim whose launch waits on the round's own pod. It
// is wider than the claims the round asks (roundAsks), and it answers two questions that are not
// whom the round asks: the claims the late-round notice lists (tellLateRound), and whether any
// claim's machine records the round once its post lands (openPodUpdate). A claim the daemon
// suspended is in it. Its machine records the round in the suspended state (supervise's table),
// and the notice names it with its state, so the controller reads every role of the pod and the
// notice is never empty while the round has a role (record.UpdateStalled). A claim whose launch
// waits on the round's pod is in it too, with a session or with none: its machine records the
// round it is posted (postedClaims). A failed claim is out: its machine ignores the round's post,
// so it records no round.
func noticeClaims(group []supervise.Claim, round string) []supervise.Claim {
	var of []supervise.Claim
	for _, c := range postedClaims(group, round) {
		if c.State != supervise.StateFailed {
			of = append(of, c)
		}
	}
	return of
}

// notWaitedFor is every claim of the group with a session that the round does not ask (roundAsks),
// that has neither answered the round nor left it, and whose work may be on the round's pod's disk
// (workOnRoundPod). It reads the one predicate of the round's two sets (dispatch://INFRA-4109 R2),
// so each claim it names is one the round neither asks nor waits for. Over a group a replacement
// reads, which holds no retired claim (roundGroup), each is a failed claim or a claim the round
// passes over at the clock's now (roundClock.passesOver). Neither runs a process in the pod, so
// what either left unpushed is on the pod's disk, and a replacement names each of them on its line
// (roundReplaced) and, where it is made over a read that found work or failed, in the architect's
// notice of what it took (notAskedToPush). A claim that answered pushed its work first, a claim
// that left the round's pod (supervise's Claim.LeftPodRound) has no work on this pod's disk, and
// neither has a claim of which workOnRoundPod says no, so none of them is named. A claim whose
// launch waits on the round's own pod is one the round asks, so it is not named here either: the
// line names it apart (unwaitedOnThePod), and the notice names it as lostWaitingClaims says. A
// claim with no session is not read here at all, so the replacement through a process names a
// claim the round passed over with no session nowhere, as one whose session was forgotten after an
// idle release suspended it: the ends that read lostClaims name it, and this one does not.
func notWaitedFor(group []supervise.Claim, round string, clock roundClock, earlierWork bool) []supervise.Claim {
	var of []supervise.Claim
	for _, c := range sessionClaims(group) {
		if notWaitedForClaim(c, round, clock, earlierWork) {
			of = append(of, c)
		}
	}
	return of
}

// notWaitedForClaim is notWaitedFor's reading of c, a claim with a session (sessionClaims): the
// round does not ask it (roundAsks), it has neither answered the round nor left it, and its work
// may be on the round's pod's disk (workOnRoundPod).
func notWaitedForClaim(c supervise.Claim, round string, clock roundClock, earlierWork bool) bool {
	return !roundAsks(c, round, clock) && !c.LeftPodRound(round) && !answered(c, round) && workOnRoundPod(c, round, earlierWork)
}

// memberOf is whether c is one of round's claims: the round it records is this one. A claim that
// leaves a round keeps the round it records (supervise's leftPodRoundAt), so membership survives
// the leaving and this one reading is the whole of it.
//
// A claim in no round and a claim in another round are members of this one in neither sense.
// Each of the two is why the report may not be read from "has not answered": a claim an earlier
// sweep's end already took out of the round (supervise's clearUpdate) and a role first started
// in the pod that replaced the round's own have both answered nothing, and neither of them was
// ever in the pod whose disk went.
func memberOf(c supervise.Claim, round string) bool {
	return c.UpdateRound == round
}

// withoutRound is group as the end of round leaves it: each claim that records round reads as in
// no round. The sweep reads a group so while an end it posted is on its way to a member's machine
// (podUpdateEnded, podUpdateSweep.asEnded): the walk decides the issue from it
// (reconcileIssuePodUpdate), and the project's open-round slots are counted over it
// (beginPodUpdateSweep). Only the round the claim records is cleared, because every reading of a
// round here goes through it (memberOf, answered, Claim.InPodRound, Claim.LeftPodRound), so a
// claim that records none is no member of any round for each of them. A group where no claim
// records round, and every group for no round, is answered as it is; any other is a copy, so the
// sweep's own group is not written.
func withoutRound(group []supervise.Claim, round string) []supervise.Claim {
	if round == "" || !recordsRound(group, round) {
		return group
	}
	ended := slices.Clone(group)
	for i := range ended {
		if memberOf(ended[i], round) {
			ended[i].UpdateRound = ""
		}
	}
	return ended
}

// lostClaims is what the round's pod took with it: each claim of the group that lostClaim reads as
// lost, in the group's order. Every end of the round names it: what a gone pod took
// (podWentWithoutItsRound, endRoundOverAGonePod) and what the replacement of a pod no role process
// ran in took: by issue, at a launch, or through a claim whose launch waits on the pod
// (replaceIssuePodForRound, endRoundALaunchReplaced, replacePodForRound). Of a replacement through
// a process in the pod, the message names the part of it whose launch waits on the pod
// (lostWaitingClaims). That is a different question from whom the replacement waits for
// (unansweredClaims): this one is about work that is already gone, and it is read from the
// round's own record of each member and from where its work may be (workOnRoundPod), rather than
// from where any claim's process is now.
//
// A member that answered pushed its work before the pod went and lost nothing, whether it
// answered in the round's own pod or had already answered when it left: leaving records no
// answer and withdraws none. One that never answered lost what it had not pushed where that work
// may be on the pod's disk, and nothing there otherwise. No turn and no answer of the process
// that replaced it changes either reading.
func lostClaims(group []supervise.Claim, round string, earlierWork bool) []supervise.Claim {
	var lost []supervise.Claim
	for _, c := range group {
		if lostClaim(c, round, earlierWork) {
			lost = append(lost, c)
		}
	}
	return lost
}

// lostClaim is lostClaims' reading of c: a member of the round that had not answered it
// (unansweredMember), and whose work may be on the round's own pod's disk (workOnRoundPod), with a
// session now or with none. Every end reads it of each member as the member stands at that end.
// Where no member is left to name, as where the sweep replaces the pod through a waiting claim
// with no session that is the issue's only claim and never ran there, the line names nobody, and
// the architect's notice, where the read before the replacement found work or failed, names no
// role (workspaceTakenMessage), and the end of a pod that went writes nothing on the issue
// (endRoundWhosePodWent).
func lostClaim(c supervise.Claim, round string, earlierWork bool) bool {
	return unansweredMember(c, round) && workOnRoundPod(c, round, earlierWork)
}

// unansweredMember is whether c is a member of the round (memberOf) that had not answered it
// (answered). lostClaim names such a member where its work may be on the round's pod's disk.
func unansweredMember(c supervise.Claim, round string) bool {
	return memberOf(c, round) && !answered(c, round)
}

// unansweredMembers is every claim of the group that unansweredMember reads as one, in the group's
// order, whatever workOnRoundPod says of it: what the end of a round whose pod went names on its
// line, under ranNoWorkerInPod, where lostClaims names nothing, since not every member answered
// (endRoundWhosePodWent).
func unansweredMembers(group []supervise.Claim, round string) []supervise.Claim {
	var unanswered []supervise.Claim
	for _, c := range group {
		if unansweredMember(c, round) {
			unanswered = append(unanswered, c)
		}
	}
	return unanswered
}

// workOnRoundPod is whether what c left unpushed may be on the disk of round's own pod, which is
// the one fact every end of the round names a member by. It is the one statement of that rule:
// every claim an end of the round names, on its line, in the architect's notice or on the issue,
// is one it holds of (lostClaim, notWaitedForClaim, unwaitedOnThePod). The read of the workspace
// before a replacement does not read it: every replacement reads (readPodWorkspace), whomever it
// names. earlierWork is whether the disk of round's pod holds work from before the pod, as the
// store marks it and the end read it (roundPodHoldsEarlierWork). It holds in three cases:
//
//   - c's set holds round's own pod (supervise.Claim.RegisteredPods, which the sweep reads off c's
//     machine, reconcilePodUpdates; each pod compared by its round, as awaitsRoundPod compares the
//     pod a launch waits on). Each pod in which an agent of c registered a session is in the set: a
//     worker of c ran there, and what it did not push was on that pod's disk. What c records at
//     the end says nothing of it: a claim refused for the pod with no session that started in the
//     pod in its place and registered there has a session and never ran in the round's pod, and
//     one that registered in the pod and lost its session, as a fresh start after a resume that
//     found no session does, has none and ran there.
//   - the disk of round's pod holds work from before the pod (earlierWork), and c has a session or
//     a pod in its set. That disk is a kept volume that an earlier pod of the issue mounted, which
//     holds the workspace that every role of the issue shares as that earlier pod left it; the
//     store marks such a pod (store.PodHoldsEarlierWork). So the work of any role an agent of which
//     had run can be on that pod's disk, whether or not an agent of it registered there, whatever
//     suspended it since and whatever it runs now: a role the daemon or the operator took over or
//     suspended, a woken role whose launch the wake pacing parks, a role whose own launch replaced
//     the pod, and a role that was suspended already when the earlier pod went, whose work was in
//     the shared workspace all the same. The store holds the record, so a daemon restart between
//     the write of the mark and the end keeps it. An end over such a pod may name a role whose
//     work the pod does not hold, which is the cost of reading the pod rather than each role
//     (dispatch://INFRA-4425/comment/566feff4-2b27-41f2-89a9-b1a333d97ecd,
//     dispatch://INFRA-4425/comment/65c8b607-a452-4d88-9bcd-624fd7ab8b27). A claim with no session
//     and an empty set never had an agent anywhere, as one refused for the pod with no session, so
//     no work of it can be on any disk, and it is not named. A project pause's stop keeps the pod,
//     so a pause alone puts no role's work on a later pod's disk.
//   - c's set is unknown (supervise.RegisteredPods.Unknown): a machine built over a stored claim
//     that shows a session it held, now or abandoned, cannot say where each earlier agent of c
//     registered (supervise's bootRegisteredPods). An end names every member that had not answered
//     whose set is unknown, whatever it records: the rule errs towards naming a role that lost
//     work, never towards hiding a loss.
//
// A claim of which none of the three holds ran no worker in the pod and has no work from before
// the pod on its disk: one that never ran an agent and is refused for the pod with no session,
// and one whose agents registered only in another pod, of a round whose pod's disk the store does
// not mark (roundPodHoldsEarlierWork).
func workOnRoundPod(c supervise.Claim, round string, earlierWork bool) bool {
	return c.RegisteredPods.Unknown || (earlierWork && (c.Session != "" || len(c.RegisteredPods.Pods) > 0)) ||
		slices.ContainsFunc(c.RegisteredPods.Pods, func(pod string) bool { return supervise.PodRound(pod) == round })
}

// roundPodHoldsEarlierWork is whether the disk of the pod of round, a round of issue, holds work
// from before the pod, as the store marks it (store.PodHoldsEarlierWork: a later pod of the issue's
// kept volume). Each end of the round reads it as it ends the round, and hands the answer to
// every reader of workOnRoundPod that end makes (replacePodForRound, replaceIssuePodForRound,
// endRoundWhosePodWent, endRecordedRound, endTakenRound). A read that fails must not hide a loss,
// so it answers true, as if the disk held such work, and the error is logged, unless the daemon's
// own stop cut the read short, which is no fault of the store's: the end then names every member
// with a session or a pod that had not answered, as an earlier release named every member.
func (s *supervision) roundPodHoldsEarlierWork(ctx context.Context, issue, round string) bool {
	pod := strings.TrimPrefix(round, supervise.PodRoundPrefix)
	holds, err := s.supervisor.store.PodHoldsEarlierWork(ctx, issue, pod)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Error("supervise: read whether the disk of this update round's pod holds work from before the pod; the round's end names every member with a session or a pod that had not answered",
				"issue", issue, "round", round, "pod", pod, "error", err)
		}
		return true
	}
	return holds
}

// lostWaitingClaims is the part of lostClaims whose launch waits on the round's own pod
// (awaitsRoundPod): every member of the round whose launch waits on that pod, that had not
// answered it, and whose work may be on that pod's disk (lostClaim), with a session or with none.
// A worker of such a claim can have died in the pod before the runtime refused its relaunch, or
// the pod can mount the kept volume of an earlier pod (workOnRoundPod). No ask of the round
// reaches such a claim (askRound), so no role was asked to push that work. Every replacement reads
// the workspace first and the read decides it (readPodWorkspace, roundKeeps), whoever made it.
// Who reads, and whom the architect's notice names where the replacement is made over a read that
// found work or failed, turns on whether a process of another claim runs in the pod, and this is
// the one statement of that rule:
//
//   - Where no claim records a process in the pod, the sweep replaces the pod through the waiting
//     claim (podProcess), or the claim's own retry replaces it first where nothing holds the pod
//     (roundPodEndsAtLaunch, endRoundALaunchReplaced). No role process runs in the pod, so the
//     pod's launcher reads the workspace before either replacement, as before a replacement by
//     issue, and the notice names each claim lostClaims names, such a claim among them.
//   - Where a process of another claim runs in the pod, the claim's retry cannot replace the pod,
//     since a launch keeps a pod that a claim records a process in (roundPodEndsAtLaunch,
//     keepAtLaunch). The sweep replaces the pod through that process, which reads the workspace,
//     and the notice names the claim beside the claims the round did not wait for
//     (notAskedToPush).
func lostWaitingClaims(group []supervise.Claim, round string, earlierWork bool) []supervise.Claim {
	var waiting []supervise.Claim
	for _, c := range group {
		if lostWaitingClaim(c, round, earlierWork) {
			waiting = append(waiting, c)
		}
	}
	return waiting
}

// lostWaitingClaim is lostWaitingClaims' reading of c: a claim lostClaim names whose launch waits
// on the round's own pod.
func lostWaitingClaim(c supervise.Claim, round string, earlierWork bool) bool {
	return lostClaim(c, round, earlierWork) && awaitsRoundPod(c, round)
}

// notAskedToPush is what the architect's notice of a replacement through a process in the pod
// names, where the read of the workspace found work or failed (replacePodForRound,
// roundReplaced): each claim notWaitedFor names and each claim lostWaitingClaims names, once each
// and in the group's order. No role was asked to push what either left on the pod's disk: the
// round does not ask the one (roundAsks), and its ask does not reach the other (askRound). Each
// claim notWaitedFor names has a session (sessionClaims), and a claim lostWaitingClaims names may
// have none, where it lost its session since (lostClaim), so the pass reads every claim of the
// group and asks the first of a claim with a session alone. Both read whether the claim's work may
// be on the pod's disk by the one predicate (workOnRoundPod).
func notAskedToPush(group []supervise.Claim, round string, clock roundClock, earlierWork bool) []supervise.Claim {
	var of []supervise.Claim
	for _, c := range group {
		if (c.Session != "" && notWaitedForClaim(c, round, clock, earlierWork)) || lostWaitingClaim(c, round, earlierWork) {
			of = append(of, c)
		}
	}
	return of
}

// unwaitedOnThePod is what the replacement's log line names under waitingOnThePodNotWaitedFor
// (roundReplaced): every claim the round is posted to that has not failed (noticeClaims) whose
// launch waits on the round's own pod (awaitsRoundPod), that had not answered the round, and whose
// work may be on that pod's disk (workOnRoundPod), with a session or with none, whether or not it
// records the round. The round waits on no answer of such a claim (awaitsAnswer), and whatever it
// left unpushed went with the pod. A claim that waits on the pod and of which workOnRoundPod says
// no is named nothing, as lostClaims names nothing of it.
//
// Neither it nor lostClaims names all of what the other names. lostClaims names every member of
// the round (memberOf) that had not answered and whose work may be on the pod's disk, a claim that
// runs in the pod or is suspended included, which this one never names. Of the claims whose
// launch waits on the round's pod, this one names more: it also names one the round has not
// reached yet, which records another round or none, and which is no member. The architect's
// notice names those of lostClaims (lostWaitingClaims), never the ones only this one names.
func unwaitedOnThePod(group []supervise.Claim, round string, earlierWork bool) []supervise.Claim {
	var unwaited []supervise.Claim
	for _, c := range noticeClaims(group, round) {
		if !answered(c, round) && awaitsRoundPod(c, round) && workOnRoundPod(c, round, earlierWork) {
			unwaited = append(unwaited, c)
		}
	}
	return unwaited
}

// workingClaims is every claim of the group whose worker runs a turn now, by role.
func workingClaims(group []supervise.Claim) []string {
	var working []string
	for _, c := range group {
		if c.State == supervise.StateWorking {
			working = append(working, string(c.Role))
		}
	}
	return working
}

// backgroundHold is the background hold over a round's pod replacement
// (dispatch://INFRA-4203 R1) as backgroundClaims reads it: running is each role whose activity
// report names background work that runs, with the kinds that report names, and unreported is
// each role that has sent no report since its last turn started, in the group's order. It holds
// the replacement when it names a role; the zero value names none.
type backgroundHold struct {
	running    map[string][]string
	unreported []string
}

// holds is whether the hold names a role.
func (h backgroundHold) holds() bool {
	return len(h.running) > 0 || len(h.unreported) > 0
}

// attributes is the hold as a log line names it (dispatch://INFRA-4203 R4): background, an
// object from each role of running to the kinds its report names, and unreported, the roles with
// no report since their last turn. Each of the two is written only when it names a role, so no
// line that names the round carries an empty list.
func (h backgroundHold) attributes() []any {
	var attributes []any
	if len(h.running) > 0 {
		attributes = append(attributes, "background", h.running)
	}
	if len(h.unreported) > 0 {
		attributes = append(attributes, "unreported", h.unreported)
	}
	return attributes
}

// refusal is the hold as the last word's refusal names it (replacementDue): each role of running,
// in role order, with the kinds its report names, and then each role with no report since its
// last turn.
func (h backgroundHold) refusal() string {
	named := make([]string, 0, len(h.running)+len(h.unreported))
	for _, role := range slices.Sorted(maps.Keys(h.running)) {
		named = append(named, fmt.Sprintf("%s reports background work (%s)", role, strings.Join(h.running[role], ", ")))
	}
	for _, role := range h.unreported {
		named = append(named, role+" has sent no report since its last turn")
	}
	return strings.Join(named, "; ")
}

// backgroundClaims is the background hold as group reads it (dispatch://INFRA-4203 R1): every
// claim that can still run work in the round's own pod whose activity report does not say that
// nothing of its own runs. Such a claim is not gone (supervise.Claim.Gone): it is in no failed,
// retired or suspended state, none of which runs a process. Its locator records a process in the
// round's pod (inRoundPod), as podProcess reads it: a claim whose process is in another pod runs
// nothing this round's replacement ends. A claim the round does not ask is weighed too, since a
// role launched into the round's pod after the round opened, or still launching there with no
// session, runs in the pod the replacement deletes all the same.
//
// The report is read by claim.ReadSessionWork against the claim's last turn start
// (supervise.Claim.LastTurnStart), which every turn moves. A report that names running background
// work holds the round with the kinds it names. No report, or one read before that turn started,
// holds the round as unreported, because such a turn can have started work the report does not
// name. The report is the store row's, at the view's own launch (roundGroup).
func backgroundClaims(group []supervise.Claim, round string) backgroundHold {
	var hold backgroundHold
	for _, c := range group {
		if c.Gone() {
			continue
		}
		if !inRoundPod(c, round) {
			continue
		}
		switch claim.ReadSessionWork(c.ActivityAt, c.LastTurnStart, c.ActivityBackground) {
		case claim.WorkRunning:
			if hold.running == nil {
				hold.running = map[string][]string{}
			}
			hold.running[string(c.Role)] = c.ActivityKinds
		case claim.WorkUnreported:
			hold.unreported = append(hold.unreported, string(c.Role))
		case claim.WorkSettled:
			// A report read since the last turn started that names nothing running holds nothing.
		}
	}
	return hold
}

// podProcess is the claim the round's pod is ended through: the first claim of the group that
// records a process in that round's own pod. The pod is what the filter is on, not merely a
// recorded process: a claim the sweep relaunched while the round ran records the pod that
// replaced this one, and ending that pod would delete a pod no round covers, which has drifted
// from nothing and which the round's own replacement has just built.
//
// Where no claim records a process in that pod, it is the first claim whose launch waits on it
// (awaitsRoundPod), through the locator the runtime's refusal named, which is the pod's as a
// process's there is: the runtime takes it for PodDrift and ReplacePod alike (podKnown). Such a
// claim is refused at every try until the round replaces the pod, and where it is the issue's only
// claim the round has no other way to its pod. A process that runs there is still the one the pod
// is ended through, whichever claim sorts first. Which of the two the answer is says whether a
// role process ran in the pod (replacePodForRound): awaitsRoundPod holds of the answer only where
// none did.
func podProcess(group []supervise.Claim, round string) (supervise.Claim, bool) {
	for _, c := range group {
		if inRoundPod(c, round) {
			return c, true
		}
	}
	for _, c := range group {
		if awaitsRoundPod(c, round) {
			return c, true
		}
	}
	return supervise.Claim{}, false
}

// podKnown is c as the runtime is asked of its issue's pod through it (PodDrift, ReplacePod): its
// token, with the process it records, or, where it records none, the pod its launch waits on
// (podLocator).
func podKnown(c supervise.Claim) runtime.Known {
	return runtime.Known{Claim: c.Token, Locator: podLocator(c)}
}

// holdsUnansweredAsk is whether c holds an ask of round that reached its session
// (supervise.Claim.UpdateTaskedAt) and that it has not answered, and has not left the round's pod
// (supervise's Claim.LeftPodRound). Where no role process runs in the round's pod, such a claim's
// process went while it held the ask, so its unpushed work may be on the pod's disk, and before
// the round's bound it keeps the pod (roundPodEnds, roundPodEndsAtLaunch).
func holdsUnansweredAsk(c supervise.Claim, round string) bool {
	return c.UpdateRound == round && c.UpdateTaskedAt != nil && !answered(c, round) && !c.LeftPodRound(round)
}

// waitingOf is the claims as the controller's notice lists them, so it can retry one claim or ask
// the operator to act on one. Each is marked with whether it has already answered this round.
//
// Every late-round notice lists every claim noticeClaims keeps with this mark (tellLateRound,
// tellPodlessRound), a claim the daemon suspended included. So the list is never empty while the
// group holds such a claim, which the store requires of the notice (record.UpdateStalled). Past
// the bound no answer holds the round, so the mark tells the controller what each role said, not
// whom to prompt: the daemon replaces the pod at the bound whether or not a role answers, and the
// notice is told only for what holds the round past that. The mark is the round's own: an answer
// a claim gave to another round says nothing here.
func waitingOf(round string, claims []supervise.Claim) []record.UpdateWaiting {
	waiting := make([]record.UpdateWaiting, len(claims))
	for i, c := range claims {
		waiting[i] = record.UpdateWaiting{
			Claim: string(c.Token), Role: c.Role, State: string(c.State),
			Answered: answered(c, round),
		}
	}
	return waiting
}

// namesOf is the roles of claims, for a log line.
func namesOf(claims []supervise.Claim) []string {
	names := make([]string, len(claims))
	for i, c := range claims {
		names[i] = string(c.Role)
	}
	return names
}

// roleStates is each claim as a message names it: its role and the state it is in.
func roleStates(claims []supervise.Claim) string {
	named := make([]string, len(claims))
	for i, c := range claims {
		named[i] = fmt.Sprintf("%s (%s)", c.Role, c.State)
	}
	return strings.Join(named, ", ")
}

// roundOpenedFor is what a message says the round was opened for (podWentMessage,
// workspaceTakenMessage, podKeptMessage, roundKeptMessage): the reasons the round recorded, or
// that the daemon recorded none.
func roundOpenedFor(reasons string) string {
	if reasons == "" {
		return "The daemon did not record why."
	}
	return "The reasons were: " + reasons + "."
}

// podWentMessage is what the issue is told when its pod went before its round was over: the pod
// was replaced where it stood for a failure of its own. Its disk went with it, or, where kept is
// true, its disk is a kept volume that is still there and holds what the roles did not push. The
// issue's spec requires this message. Another agent reads it, so it follows ASD-STE100.
//
// Where the issue's record has left the repository the round's pod was built from, "its issue
// branch at origin" would send a role to the wrong place, whatever the disk and whatever drift
// opened the round: every push a role made before the pod went is on the issue branch of the
// repository the pod was built from, and the new pod's origin is the one the issue's record holds
// now. So where moved is a move (roundMove), the last sentence names both repositories and sends
// a role to the old one's issue branch (pushedWorkMessage), and what it says of a kept volume
// names the old repository's workspace (keptDiskMessage). Where the record could not be read
// (repositoryMove.unread), the same sentences name the pod's repository and say that the daemon
// does not know the new pod's origin.
func podWentMessage(issue, round, reasons string, lost []supervise.Claim, kept bool, moved repositoryMove) string {
	disk := "The pod's disk went with the pod. Work that these roles did not push is lost."
	if kept {
		disk = "The pod's disk is a kept volume, and the volume is still there. " +
			keptDiskMessage(issue, "Work that these roles did not push", moved)
	}
	return fmt.Sprintf(`The pod that ran this issue's roles is gone. The daemon opened an update round over that pod (%s). %s The pod went before the round was over.

These roles did not answer the update round before the pod went: %s. %s

The daemon starts each role again, in the same session, on a new pod. %s`,
		round, roundOpenedFor(reasons), roleStates(lost), disk, pushedWorkMessage(moved))
}

// pushedWorkMessage is the sentence of a message of a round (podWentMessage, workspaceTakenMessage)
// that says where each role reads the work that it pushed, which is its issue branch
// at origin. Where moved is a move (roundMove), every push a role made before the pod went is on
// the issue branch of the repository the pod was built from, and the new pod's origin is the
// repository the issue's record holds now, so the sentence names both repositories and sends a
// role to the old one's issue branch. Where the record could not be read
// (repositoryMove.unread), the new pod's origin is unknown, so the sentence sends a role to the
// issue branch of the pod's repository and says that the daemon could not read the record.
func pushedWorkMessage(moved repositoryMove) string {
	switch {
	case moved == (repositoryMove{}):
		return "Each role must read its issue branch at origin to see the work that it pushed."
	case moved.unread():
		return fmt.Sprintf("Work that each role pushed before the pod went "+
			"is on the issue branch of the repository `%[1]s`. "+
			"The daemon could not read the repository that this issue's record holds now. "+
			"It does not know which repository is origin in the new pod. "+
			"To see that work, each role must read the issue branch of the repository `%[1]s`.",
			moved.from)
	}
	return fmt.Sprintf("Work that each role pushed before the pod went "+
		"is on the issue branch of the repository `%[1]s`. "+
		"In the new pod, origin is the repository `%[2]s`. "+
		"To see that work, each role must read the issue branch of the repository `%[1]s`.",
		moved.from, moved.to)
}

// podOrigin is the words a message of a round names the repository of the round's pod with, where
// it names the work that is or is not on that repository's branches and tags
// (workspaceTakenMessage, readFound): "at origin" where moved is no move. Where moved is a move
// (roundMove), it names the repository the pod was built from: the read of the workspace ran in
// that pod, whose origin was that repository, and a role reads what it is told in the new pod,
// whose origin is the repository the issue's record holds now, which holds none of that work. It
// names that repository too where the record could not be read (repositoryMove.unread), since the
// new pod's origin is then unknown.
func podOrigin(moved repositoryMove) string {
	if moved == (repositoryMove{}) {
		return "at origin"
	}
	return fmt.Sprintf("of the repository `%s`", moved.from)
}

// keptDiskMessage is what a message of a round says of the work on a kept volume that is still
// there (podWentMessage, workspaceTakenMessage), unpushed being the words that
// message names that work with. Where moved is a move (roundMove: the repository the gone pod was
// built from and the one the issue's record holds now), "the next pod mounts the volume" alone
// would mislead: the work is in the old repository's workspace, and the next pod makes a
// workspace of the new repository beside it, where every role works, and no role works in the old
// one again. So the message names that workspace's path and how a role keeps the work. Where the
// record could not be read (repositoryMove.unread), the next pod's repository is unknown, so the
// message names the path and says how a role keeps the work if that pod makes a workspace of
// another repository; pushedWorkMessage says that the daemon could not read the record. The path
// is the one the Sandbox runtime's pod gives the workspace (sandbox.WorkspaceDir), as that
// runtime is the one that keeps a volume (runtime.IssueVolumeReader).
func keptDiskMessage(issue, unpushed string, moved repositoryMove) string {
	if moved != (repositoryMove{}) {
		if old, err := sandbox.WorkspaceDir(moved.from, issue); err == nil {
			if moved.unread() {
				return fmt.Sprintf("%[3]s is on that volume, "+
					"in the workspace of `%[1]s` at `%[2]s`. "+
					"The next pod of this issue mounts the volume. "+
					"If that pod makes a workspace of a different repository, "+
					"read that work at `%[2]s` in that pod. "+
					"Put that work on the issue branch in the new workspace. "+
					"Then push the issue branch.",
					moved.from, old, unpushed)
			}
			return fmt.Sprintf("This issue moved from the repository `%[1]s` "+
				"to the repository `%[2]s`. "+
				"%[4]s is on that volume, "+
				"in the workspace of `%[1]s` at `%[3]s`. "+
				"The next pod of this issue mounts the volume. "+
				"That pod makes a new workspace of `%[2]s`. "+
				"Every role works in that new workspace. "+
				"No role works in the workspace of `%[1]s` again. "+
				"To keep that work, read it at `%[3]s` in the next pod. "+
				"Put that work on the issue branch in the new workspace. "+
				"Then push the issue branch.",
				moved.from, moved.to, old, unpushed)
		}
	}
	return unpushed + " is on that volume. The next pod of this issue mounts the volume."
}

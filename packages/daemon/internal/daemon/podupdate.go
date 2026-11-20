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
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// reconcilePodUpdates carries every update round of this project one step, over the claims the
// orphan sweep already read this tick. A running issue pod whose configuration has drifted is
// never replaced where it is found: that ends every role process in it mid-turn and deletes the
// pod's disk with everything unpushed on it. The daemon opens a round over the pod instead — each
// role the round asks (roundAsks) is asked, on its own Envoy topic, to do the steps of the update
// message and answer (supervise.UpdateTask holds those steps; openPodUpdate, record.UpdateAsk,
// api's claims/ready-for-update) — and replaces the pod once each of them has answered, no turn
// runs and no claim with a process in the pod reports background work that runs or has sent no
// report since its last turn started (roundEnd.holding), or at the round's bound
// (pod_update_grace_minutes after the round opened, roundBound) whatever the roles said and
// whatever runs (replacementDue). A role's own push is what keeps its work: what it has not
// pushed when the pod goes is lost with the pod. An answer from every role the round asks ends
// the round early once no turn and no reported background work holds it (dispatch://INFRA-4203
// R1); no answer holds it past the bound. An operator's suspension of a claim is the one wait
// that outlasts the bound, and only over a pod that is there: a gone pod ends its round whatever
// the hold (readReplacement).
//
// The round is the issue's, not the locator's: a suspended claim's locator is nil (supervise's
// letGo), and before the bound no pod disk may be deleted before every worker the round asks has
// signalled (roundAsks). A claim an operator suspended is one of them. So is a claim a project's
// pause stopped, while the pause keeps the pod for the work on its disk (roundClock.passesOver).
// Any other claim the daemon suspended is not: it runs no process and no turn in the pod
// (dispatch://INFRA-4109 R1). What such a claim left unpushed goes with the pod, so the
// replacement first reads the issue's workspace through a role whose process runs in the pod,
// and names on its line, and to the issue where the read found work or failed, what the read
// found (replacePodForRound, readRoundWorkspace). Every role claim of one issue shares that
// issue's one Sandbox and pod, so the issue is what groups them. A group whose claims' locators
// name two different pods is left alone this tick: a replacement is already in flight, and the
// next tick reads it settled.
//
// Two kinds of claim are left out of every group, and so out of every decision below. A retired
// claim runs nothing on any pod: it holds no process, it blocks no round, and a round an earlier
// release left recorded on it names a pod that is long gone — read beside a live claim's pod it
// would say, at every tick, that this issue's pod went without its round. A claim with no issue
// (the project controller's) is in no round at all: its pod has no issue workspace and nothing to
// push, and its plugin contract is carried by the stale-plugin relaunch instead
// (supervise's refreshPlugin).
//
// Everything a round decides comes from the durable claim columns — the round, its reasons, when
// it opened, when each worker was asked, when each answered, and when the operator's side was
// told. A daemon restart therefore resumes a round exactly where it stood, and the same record
// gives the same decision. This pass keeps four things of its own in memory, and none of them
// goes in the store:
//
//   - The pace at which it opens new rounds: when it last opened one, and the issues it has
//     opened one for whose claims have not recorded it yet (podUpdateOpenings). It paces this
//     daemon's own openings and decides no round. A daemon that has just booted opens its first
//     round at its first sweep, one sweep interval after the start.
//   - What a round's one read of its issue's workspace found before the pod's replacement
//     (podUpdateWorkspace), which that replacement names. The replacement is made whatever the
//     read found. A daemon that has just booted reads the workspace again before the round's
//     replacement.
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
//     has just booted has lost it, and reads a pod a launch replaced as a pod gone, whose message
//     names the same members.
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
// let-out of the last launch the pause held for a role of the pod (roundBound). Until that
// let-out, a role the pause stopped is a claim the round asks once its agent is ready and waits
// for, although the pause suspended it (roundClock.passesOver). The sweep reads the claims from
// memory, so it reads the project's holds once, here, rather than once per issue
// (podPauseHolds), and hands each issue its holds with the sweep's now as one value (roundClock).
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
	sweep := s.beginPodUpdateSweep(now, byIssue)
	s.forgetWorkspaceReads(byIssue)
	s.forgetLaunchRounds(byIssue)
	var replacements []podReplacement
	for _, issue := range issues {
		if ctx.Err() != nil {
			return
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
			continue
		}
		s.replacePodForRound(ctx, replace)
	}
}

// podHolds is what a project's pause did to one issue's pod as a sweep reads it
// (store.Store.PausePodHoldsSince): each hold that stands on the pod, and each that ended, or whose
// pause let out a launch of the pod, since the oldest round of the sweep opened (decision 12 of
// dispatch://INFRA-3759).
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

// heldUntil is the moment hold counts as keeping its pod to, read at now. A hold that stands
// counts to now. A hold that ended at the resume or at a role's ready counts to the later of that
// end and the latest let-out of a launch its pause held for a role of the pod
// (store.PausePodHold.LetOut), which may still be ahead: a role whose launch waits its turn in the
// wake pacing can answer no round, and the pause, not the role, kept it from taking a turn, as the
// stuck clock of its resume counts nothing before that let-out either (decision 14 of
// dispatch://INFRA-3759). A hold a deletion ended counts to that end: the pod is gone.
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

// keeping is whether the holds still keep the pod for the roles their pause stopped, read at now:
// a hold stands on it, or a hold ended at the resume or at a role's ready and the moment it counts
// to (heldUntil), the let-out of a launch its pause held for a role of the pod, is still ahead of
// now. A hold a deletion ended keeps nothing: the pod is gone. While the holds keep the pod, a
// claim the pause stopped is the pause's and not R1's (roundClock.passesOver).
//
// One more ended hold keeps the pod: one whose pause held a launch of the pod (LetOut is set)
// that the wake pacing has not let out past the hold's end yet. The resume sets the stuck clock
// of each launch it held to its own moment (store.SetProjectResumeClocksTx), which is the hold's
// end, and only the launch's reservation in the wake pacing moves it on to the let-out
// (store.SetResumeClock). Between the two, heldUntil reads the hold's end, which is already
// past, while the stopped role still waits for its owed launch. A launch let out at that very
// moment leaves its claim launching, and a launching claim is no claim R1 passes over, so the
// rule costs that claim nothing. Such a hold keeps the pod at most until the round's bound, which
// ends every wait of a round but an operator's and a standing hold's.
func (h podHolds) keeping(now time.Time) bool {
	for _, hold := range h {
		switch {
		case hold.EndedAt.IsZero(), heldUntil(hold, now).After(now):
			return true
		case hold.End != store.PausePodHoldDeleted && !hold.LetOut.IsZero() && !hold.LetOut.After(hold.EndedAt):
			return true
		}
	}
	return false
}

// podPauseHolds is the holds a project's pause has or had on the project's pods, by issue, read
// once for the sweep: every hold that stands, and every hold that ended, or whose pause let out a
// launch of its pod, since the oldest round any of claims records opened, which are the holds a
// round's bound can move by.
func (s *supervision) podPauseHolds(ctx context.Context, claims []supervise.Claim, now time.Time) (map[string]podHolds, error) {
	since := now
	for _, c := range claims {
		if c.UpdateOpenedAt != nil && c.UpdateOpenedAt.Before(since) {
			since = *c.UpdateOpenedAt
		}
	}
	read, err := s.supervisor.store.PausePodHoldsSince(ctx, s.cfg.Project, since)
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
// A group whose claims record no pod at all decides nothing from its claims: no locator is no
// evidence either way — every claim of the issue may be suspended (letGo clears the locator), or
// the one live claim may be minutes into a relaunch. So the runtime is asked, by the issue alone,
// which pod the issue's Sandbox holds now (podlessRound): another pod than the round's, or none,
// is the round's pod gone, and the round ends as a gone pod's round does. The round's own pod is
// replaced by issue where nothing holds it (roundPodEnds, dispatch://INFRA-4109 R7), and otherwise,
// or with no answer, the round waits for a claim to record a pod again. A launch into that pod
// reads the same rule as it runs (roundPodEndsAtLaunch), so a launch that comes before the sweep
// replaces the pod itself.
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
// replacement the round made (endRoundALaunchReplaced, podUpdateLaunchReplaced): the issue is told
// what a replacement by issue tells it, which names the members that had not answered and says
// that no process could read the workspace, and not that its pod went without its round. A daemon
// that has just booted has lost that record, and ends such a round as a pod gone, whose message
// names the same members.
//
// Which reading that is, and when it comes, changes nothing the issue is told, because the
// report is read from the round's own record rather than from where any claim's process is now.
// The round records, on each claim it asked, the round that claim left and whether it had
// answered when it left (supervise's leftPodRoundAt), and only the round's end clears either.
// So a claim relaunched into the pod that replaced this one is a member of the round still, with
// the part the dead pod left it; a role first started in that new pod is no member and is named
// nothing; and an earlier sweep's end landing on some claims before others takes those claims
// out of the round rather than into the report. What the pod took with it is the members who
// neither answered nor left having answered (lostClaims), which is a different question from
// whom a replacement waits for (waitingClaims).
//
// It reports the one step it leaves the sweep: the replacement of a complete round's pod, or of
// the pod of a round that has no role process in it and that nothing holds (by issue), which the
// sweep makes after its whole walk over the issues, or nil where there is nothing left to do for
// the issue (reconcilePodUpdates, podReplacement).
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
		// launcher binding — and its disk went with it while a worker still had work to push. The
		// round ends by the one rule for a round whose pod went (endRoundWhosePodWent), counted
		// from the group this walk decided from. Nothing of the pod in its place decides anything
		// here: the round was opened over a pod that is not there, no answer and no replacement can
		// end it any more, and the drift of the new pod is that pod's own round to open.
		s.endRoundWhosePodWent(ctx, issue, group, open, pod, false,
			"supervise: the pod this issue's update round was opened over is gone; the round is over")
		return nil
	}
	reasons, reading := s.groupDrift(ctx, issue, group)
	if reading == podGone {
		s.endRoundOverAGonePod(ctx, issue, group, open, pod)
		return nil
	}
	if reading != podRead {
		// The unread pod decides nothing, and past the bound of a round open over it the
		// operator's side is told of the round once (tellUnreadPod). The snapshot says first
		// whether the bound has come and whether the round was told, so a round before its bound
		// costs no read of the machines. A sweep the daemon cancelled read nothing of the pod, and
		// tells nothing.
		if open != "" && ctx.Err() == nil && clock.pastBound(group, open) && !roundTold(group, open) {
			live := s.liveGroup(group)
			s.tellUnreadPod(ctx, issue, live, open, pod, roundReasons(group, open), heldByOperator(live, open, clock), clock)
		}
		return nil
	}
	if len(reasons) == 0 && open == "" {
		// A pod that has drifted from nothing, with no round open over it, needs no replacement:
		// returning here is what keeps the code below from opening a round over it.
		return nil
	}
	if open == "" && clock.held() {
		s.log.Info("supervise: a project's pause holds this drifted issue's pod; no update round opens over it until the hold ends",
			"issue", issue, "pod", pod, "reasons", strings.Join(reasons, ", "))
		return nil
	}
	// The drift this tick read opens a round and decides nothing of a round already open: a
	// round's terms are fixed at its open (dispatch://INFRA-2981 R9), so what an open round's
	// notices and its replacement name is its own recorded reasons (roundReasons). That holds
	// where the drift has gone away too — an operator rolled the release back — and the round
	// replaces the pod all the same. The round has already asked each role it asks (roundAsks) to
	// do the steps of its update message (supervise.UpdateTask) and to start no new work, and no
	// path is left that lets a role carry on in the pod it was told would be replaced. Replacing it
	// is one restart of each role in its own session, which the alternative — a release of the
	// answered roles that the daemon alone knows about — is not.
	terms := strings.Join(reasons, ", ")
	// One live read, given to both decisions below, so the claims the replacement waits on and
	// the claims the post records the round on, which the ask then reaches, are one set. Read
	// live by the replacement and from the snapshot by the post, a claim whose session registered
	// after the tick's read would block the round in the one and be no worker of it in the other:
	// the replacement would wait on that claim for an answer to a round the post never recorded
	// on it, so no ask would reach it, and the round would run to its bound waiting for a worker
	// nothing had asked anything of.
	var live []supervise.Claim
	if open != "" {
		// A round open here is over the pod the claims record, the one round this issue can be
		// in: a round over any other pod has ended above.
		live = s.liveGroup(group)
		terms = roundReasons(group, open)
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
		live = s.liveGroup(group)
	}
	if s.openPodUpdate(ctx, issue, live, pod, terms, clock) && opening {
		s.recordPodUpdateOpening(sweep, issue, round, clock.now)
	}
	return nil
}

// completePodUpdate decides a round that is over. Before the round's bound that is every claim
// the round asks (roundAsks) having answered, no claim of the group running a turn, and no claim
// with a process in the round's pod whose activity report names background work that runs or
// that has sent no report since its last turn started (backgroundClaims). Waiting for no claim to
// be working keeps the promise that no worker is stopped mid-turn before the bound — a worker
// answers from inside a turn, and that turn ends a moment later. Waiting for the reports keeps
// the same promise for the work a turn left running: a subagent or a background job is no turn,
// and it ends with the pod (dispatch://INFRA-4203 R1). A claim the round passes over
// (roundClock.passesOver) holds nothing here, whatever it left in the workspace
// (dispatch://INFRA-4109 R1). At the bound (roundBound) neither an answer, a turn nor a report
// holds the round any more, and the pod is replaced whatever the roles said and whatever runs
// (dispatch://INFRA-2981 R2, as dispatch://INFRA-4203 R1 changes it). It reports whether it
// decided the round, and with it the pod replacement that decision leaves the sweep to make.
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
// and delete its disk. A claim whose machine is deciding reads as unanswered in the live read
// (roundGroup's mask). A round the fresh read finds unanswered before its bound is simply not
// complete: this reports that it decided nothing, and the post below keeps the round recorded
// on every claim. The snapshot is kept for one thing only: saying, in the log line, that the
// fresh read differs from it rather than that a worker never answered.
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
// A round no live claim records a process in the pod of has nothing to replace the pod through,
// and ends on nothing the claims say: the runtime is asked about the pod itself
// (roundWithNoProcessInItsPod). A pod it answers gone ends the round. A pod it answers there is
// replaced by issue where nothing holds it (roundPodEnds, dispatch://INFRA-4109 R7), so no launch
// goes into a pod the round is about to replace. Once opened, a round ends by replacing its pod or
// because that pod is gone, and no locator is no evidence either way. The pod is asked about
// before an operator's suspension is weighed: a gone pod ends its round whatever the hold, and the
// hold keeps its place wherever the pod is there.
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
		if len(waitingClaims(snapshot, round, clock)) == 0 {
			s.log.Info("supervise: every worker of this update round had answered in the sweep's snapshot, and the fresh read finds one unanswered; the replacement waits for the next sweep or for the round's bound",
				"issue", issue, "round", round, "waiting", namesOf(reading.waiting))
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
	// waitsForAnswer is a claim the round asks that has not answered it (waitingClaims), read
	// only before the round's bound.
	waitsForAnswer replacementWait = iota
	// waitsForOperator is a claim the round asks that an operator's suspension holds
	// (heldByOperator), with a live claim recording a process in the round's own pod, read
	// whatever the clock says.
	waitsForOperator
	// waitsForProcessInItsPod is no live claim recording a process in the round's own pod
	// (podProcess), read whatever the clock says. It carries the claims an operator's suspension
	// holds, if any, and whether a project's pause holds the pod: either hold is read only once
	// the pod is known to be there (roundWithNoProcessInItsPod). With no process to replace the
	// pod through, the walk replaces it by issue where nothing holds it (roundPodEnds), and the
	// replacement through a process waits (replacePodForRound).
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

// replacementReading is what the seven readings answered over one live group: the process the
// round's pod is replaced through, or the first reading that holds the replacement back with
// what its caller has to name of it — the claims that have not answered, the claims an operator
// holds (for waitsForOperator, and for waitsForProcessInItsPod where a hold was read), the roles
// whose turns run, or the roles whose reports hold the round (backgroundHold). Nothing but the
// fields the wait names is set, and process is set for replacementReady alone.
type replacementReading struct {
	wait       replacementWait
	process    runtime.Known
	waiting    []supervise.Claim
	held       []supervise.Claim
	working    []string
	background backgroundHold
}

// roundEnd is what a round's end reads of its claims: whether the round's bound has come, the
// claims the round asks that answered it and those that did not, the roles whose turns run, and
// the roles whose activity reports hold the round (backgroundClaims). It is what the line of a
// replacement the bound made names (replacePodForRound), and what says whether the round reads
// its workspace before that replacement (readRoundWorkspace).
type roundEnd struct {
	past       bool
	answered   []supervise.Claim
	unanswered []supervise.Claim
	working    []string
	background backgroundHold
}

// holding is what holds the round's pod replacement at this reading of the round's end: before
// the round's bound, the claims the round asks that have not answered it, the roles whose turns
// run, and the claims with a process in the round's pod whose report names background work that
// runs or that have sent no report since their last turn started; past the bound nothing. It is
// the one statement of that rule: dispatch://INFRA-2981 R2, as dispatch://INFRA-4203 R1 changes
// it. The walk's reading (readReplacement) and the last word under the pod's launch turn
// (replacementDue) read it here. The line of a replacement the bound made (bounded) reads the
// same three fields, unanswered, working and background, past the bound.
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

// settled is whether every claim the round asks has answered it and no turn runs, whatever the
// clock says and whatever background work a role reports: the moment the round reads its
// workspace at (readRoundWorkspace, dispatch://INFRA-4109 R1). A replacement the bound let
// through over an answer not given or a turn makes no read; one it let through over a report of
// background work alone makes it, since the read stops nothing that runs in the pod and changes
// no file of the working copy. What the read writes is jj's snapshot of the working copy, as every
// jj command that reads the working copy does (the shim's readWorkspaceWork).
func (e roundEnd) settled() bool {
	return len(e.unanswered) == 0 && len(e.working) == 0
}

// readRoundEnd is the round's end as group reads at the clock's now, under the round's bound
// (roundClock.pastBound), with the claims the round asks read at the same clock (roundAsks). Each
// of its sets is read whatever the clock says: holding drops them past the bound, and the line of
// a replacement the bound made names what each of them still held then.
func readRoundEnd(group []supervise.Claim, round string, clock roundClock) roundEnd {
	return roundEnd{
		past:       clock.pastBound(group, round),
		answered:   answeredClaims(group, round, clock),
		unanswered: waitingClaims(group, round, clock),
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

// passesOver is whether the round, read at the clock's now, passes over a claim
// (dispatch://INFRA-4109 R1). It is the one statement of that rule: roundAsks leaves such a claim
// out of both of the round's sets, and notWaitedFor and passedOverClaims name it. It is every
// claim supervise's Claim.PassedOverByRound names, a suspension by the daemon's own rule, by a
// root's exit, by an idle release or by an unknown source, but one: a claim suspended released
// while a project's pause still keeps the issue's pod (podHolds.keeping).
//
// A pause's stop suspends each role it stops as released (supervise.SuspendedByReleased) and
// keeps the pod for the work on its disk (decision 12 of dispatch://INFRA-3759). So such a claim
// is the pause's, not R1's, as the architect of dispatch://INFRA-4109 decided on 2026-11-19. The
// round asks it once its agent is ready, as it asks any claim that is not ready yet, and waits for
// its answer before the bound. The hold has already moved that bound (roundBound). A hold that
// stands holds the replacement whatever the answers (readReplacement's waitsForPause). Once the
// hold has ended and the let-out of the last launch its pause held for a role of the pod has
// passed, a claim still suspended released is R1's again: the pause no longer keeps it from a
// turn.
//
// The claim's machine still refuses an ask of a released claim (supervise's
// Machine.PublishUpdateAsk, UpdateAskSuspended), so nothing is published to a claim the pause
// keeps: a publish to a role with no holder raises a role wake, and the pause's resume, not the
// round, brings a stopped role back. The sweep asks such a claim only once its agent is ready
// anyway (askRound reads supervise.SaidReady), and a ready claim is no longer suspended.
func (c roundClock) passesOver(of supervise.Claim) bool {
	return of.PassedOverByRound() && !(of.Released() && c.holds.keeping(c.now))
}

// readReplacement takes the seven readings that hold a round's pod replacement back over one live
// group, at the clock's now and under the round's bound (clock), and answers either the process
// the round's pod is replaced through or the first reading that holds it. Both the walk that
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
// there, and only that: a hold over a round no live claim records a process in the pod of is
// read as no process in the round's pod, carrying the claims the operator holds, so that its
// caller asks first whether the pod is there at all. A round whose pod is gone ends whatever
// the operator's suspension says (endRoundOverAGonePod): there is no pod left to replace, and a
// wait for the resume of a claim on a pod that no longer exists is a wait for nothing. A claim
// that has not answered, a turn that runs, and a claim whose report names background work that
// runs or that has no report since its last turn hold the replacement only before the bound
// (roundEnd.holding), which is what makes the bound the end of every round nobody answers (R2)
// and of every round whose background work outlives it (dispatch://INFRA-4203 R1). No process in
// the round's pod and two pods at once are facts about the pod, not about any answer, so they
// hold this replacement whatever the clock says: there is nothing in such a pod to replace it
// through, or a replacement is already in flight. A pod with no process in it is replaced by
// issue instead where nothing holds it (roundWithNoProcessInItsPod, roundPodEnds).
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
	process, running := podProcess(live, round)
	if held := heldByOperator(live, round, clock); len(held) > 0 {
		if !running {
			return replacementReading{wait: waitsForProcessInItsPod, held: held}
		}
		return replacementReading{wait: waitsForOperator, held: held}
	}
	if clock.held() {
		if !running {
			return replacementReading{wait: waitsForProcessInItsPod}
		}
		return replacementReading{wait: waitsForPause}
	}
	unanswered, working, background := readRoundEnd(live, round, clock).holding()
	if len(unanswered) > 0 {
		return replacementReading{wait: waitsForAnswer, waiting: unanswered}
	}
	if !running {
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
	return replacementReading{wait: replacementReady, process: process}
}

// roundReplacer is who makes a round's own replacement of its pod: the sweep through a process in
// the pod (viaProcess, replacePodForRound), the sweep by issue (viaIssue, replaceIssuePodForRound),
// or a launch into the pod (viaLaunch, endRoundALaunchReplaced). It is the one record of that
// fact. The sweep hands it from its walk to the step after it (podReplacement), and the read of
// the workspace carries it to the replacement's line, to whether the issue is told and to what
// the issue is told (workspaceRead, roundReplaced, workspaceLostMessage). The zero value is the
// replacement through a process.
type roundReplacer int

const (
	viaProcess roundReplacer = iota
	viaIssue
	viaLaunch
)

// String is the key that the replacement's line carries as true for a replacement of a pod no
// role process ran in (roundReplaced): "byIssue" or "atLaunch". It is "" for the replacement
// through a process, whose line names no replacer.
func (r roundReplacer) String() string {
	switch r {
	case viaIssue:
		return "byIssue"
	case viaLaunch:
		return "atLaunch"
	}
	return ""
}

// podReplacement is the one step the sweep's walk over the issues leaves for after the walk: the
// pod of a round that is over is to be replaced (completePodUpdate), or the pod of a round that
// has no role process in it and that nothing holds (viaIssue: podlessRound,
// roundWithNoProcessInItsPod). It carries what the step cannot read again — the issue, the round,
// the pod the round was opened over, the reasons the round records, the pause's holds of the pod
// the sweep read once, and which kind of replacement it is — and nothing of the claims or the
// clock, which the step reads afresh: it reads the holds at its own moments (roundClock,
// replacePodForRound, replaceIssuePodForRound). That includes what decides the read of the
// workspace before the replacement: whether the round passed over a claim that has not answered,
// and whether every claim it asks has answered with no turn running (readRoundWorkspace). Carried
// from the walk, either would be a second and older reading of the claims the step has just read
// again.
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
// podPauseHolds), a claim of the round that has not answered before the bound, no live claim
// recording a process in the round's own pod, a turn running in the issue before the bound, a
// claim with a process in the round's pod whose report names background work that runs or that
// has no report since its last turn before the bound, and claims recording two pods. This read
// takes each report from the store as it stands now, so a report that came after the walk's
// snapshot is weighed here (roundGroup). None of them ends anything: the round stays open, and
// the next sweep decides it afresh from claims it reads then. An operator's suspension and two
// pods at once are not stepped over by the bound, so each is told to the operator's side once the
// bound has come (tellLateRound), as the walk tells them; a pause's hold is told to nobody. No
// process in the round's pod is decided by the walk alone, which asks the runtime first whether
// the pod is there at all (roundWithNoProcessInItsPod) and then replaces it by issue or tells of
// it. That includes a round an operator's suspension or a pause holds with no live process in its
// pod: the hold is weighed only once that read finds the pod there, and a pod it finds gone ends
// the round whatever the hold.
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
// replacing nothing and returning nil, which ends the round here as a replacement made does: no
// operator's hold is read over a pod that is not there.
//
// Before that call, and outside the pod's launch turn that ReplacePod takes, a round that passed
// over a claim that has not answered (passedOverClaims: a claim the daemon suspended that the
// round does not wait for, roundClock.passesOver, and which runs no process in the pod) reads the
// issue's workspace once (readRoundWorkspace). The read goes through the role of the process the
// pod is replaced through, since only a process in the pod can read the disk the replacement
// deletes. It is made only once every claim the round asks has answered and no turn runs
// (roundEnd.settled), so a replacement the bound let through over a claim that had not answered,
// or over a turn, makes none; a replacement the bound let through over a report of background
// work alone makes it. What it found is kept for the round (podUpdateWorkspace): a replacement the
// last word refuses, or one that fails, reads nothing at the next sweep that reaches the
// replacement, which names the same read.
// The read decides nothing: the replacement is made at the same sweep whether the read found
// work, found none, or failed.
//
// ReplacePod is called once, with the process of a claim that records this round's own pod. A
// failed ReplacePod is logged and retried at the next sweep: nothing is ended, so the round
// still holds every answered worker's next task. A ReplacePod that keeps failing holds the round
// past its bound, which only a person can end, so it is told to the operator's side once the
// bound has come (tellLateRound) and names what holds the round. One round is told once.
//
// A replacement made is logged on one line that holds for a round that ended either way, by its
// answers or by its bound. It names each claim with a session that the round did not wait for
// and that had not answered (notWaitedFor): a failed claim, and a claim the round passed over.
// What each left unpushed went with the pod. The line names too what the read of the workspace
// found, or why no read was made (workspaceRead.attrs). One that the bound made — a claim had
// not answered, a turn ran, or a claim's report named background work or was missing, when the
// last word let it through — is logged on a second line of its own, `supervise pod update
// replaced at the bound`, naming the claims that answered and those that did not, the turns that
// ran, and apart from them the roles whose reports held the round (dispatch://INFRA-2981 R8,
// whose production check reads those words; dispatch://INFRA-4203 R4). That line is written only
// where the last word ran and let the replacement through. A runtime that found no pod to
// replace and returned nil without asking the last word replaced nothing, so the bound replaced
// nothing either.
//
// Where the read found work that no branch or tag on origin holds, or failed, one message on the
// issue names the claims the round did not wait for and what the read found
// (workspaceLostMessage). It is written with the replacement made and with no other: a
// replacement refused or failed ends nothing, and the round's end forgets the read
// (endPodUpdates). A read that found no work writes nothing on the issue, and a replacement that
// made no read writes nothing either.
//
// The call is bounded to one sweep interval. The bound costs the round nothing. The suspension
// that displaces the pod has already landed when it hits, so the next sweep reads the Sandbox
// suspended and naming that very pod as the one it displaced, which is this round's own
// replacement in flight: PodDrift answers for it as it does for a pod that is there and has not
// drifted, and the round is finished at that sweep, where only the pod's end is left to wait
// for. Once the pod itself is gone, that sweep reads it gone and the round ends there instead.
// The read before the call has a bound of its own, also one sweep interval, so a slow read takes
// nothing of the call's: a read its bound ends is a read that failed, and the issue is told so.
func (s *supervision) replacePodForRound(ctx context.Context, replace podReplacement) {
	issue, round, pod, reasons := replace.issue, replace.round, replace.pod, replace.reasons
	claims, err := s.supervisor.Claims(ctx)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Error("supervise: read this issue's claims again before its update round's replacement; nothing was replaced and the round stays open",
				"issue", issue, "round", round, "pod", pod, "error", err)
		}
		return
	}
	live := s.viewedGroup(claims, issue)
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
		s.log.Info("supervise: a worker of this update round has not answered now the sweep has reached its replacement, and the round's bound has not come; nothing was replaced and the round stays open",
			"issue", issue, "round", round, "pod", pod, "waiting", namesOf(reading.waiting))
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
	read := s.readRoundWorkspace(ctx, issue, round, live, reading.process, clock)
	limited, done := context.WithTimeout(ctx, s.plan.orphanSweep)
	defer done()
	// ended is what the last word read of the round's end under the launch turn, which is the
	// read the replacement is made on. It stays the zero roundEnd, which no bound made, where the
	// runtime returns nil without asking the last word: the Sandbox runtime does that for a
	// Sandbox that is absent or suspended, or whose pod is no longer the locator's, and replaces
	// nothing there.
	var ended roundEnd
	if err := s.runtime.ReplacePod(limited, reading.process, s.replacementDue(issue, round, replace.holds, &ended)); err != nil {
		if errors.Is(err, errReplacementNotDue) {
			s.log.Info("supervise: this update round's replacement was no longer due when its pod's launch turn came; nothing was replaced and the round stays open",
				"issue", issue, "round", round, "pod", pod, "reason", err)
			return
		}
		if ctx.Err() == nil {
			s.log.Error("supervise: replace the pod of a finished update round; retrying at the next sweep",
				"issue", issue, "round", round, "pod", pod, "error", err)
			s.tellLateRound(ctx, issue, live, round, pod, reasons, refusedReason(err), clock)
		}
		return
	}
	s.roundReplaced(ctx, issue, round, pod, reasons, ended, notWaitedFor(live, round, clock), read, live)
}

// roundReplaced is what each of a round's own replacements owes once it is made: through a process
// (replacePodForRound), by issue (replaceIssuePodForRound) or at a launch (endRoundALaunchReplaced).
// It logs the replacement's line, and the bound's own line where the bound let the replacement
// through (ended, what the last word or the launch read of the round's end under the pod's launch
// turn), writes the message on the issue (tellWorkspaceLost), and ends the round on the claims of
// members (endPodUpdates).
//
// read is what the read of the workspace found (workspaceRead), and read.by is who made the
// replacement (roundReplacer). The line of a replacement of a pod no role process ran in carries
// its replacer's key as true right after atTheBound, "byIssue" or "atLaunch"; the line of the
// replacement through a process carries none. named is the claims the line and the message name.
// For the replacement through a process, named is the claims the round did not wait for
// (notWaitedFor), under notWaitedFor; for one by issue or at a launch, it is the members of the
// round that had not answered it (lostClaims), under unansweredMembers.
func (s *supervision) roundReplaced(ctx context.Context, issue, round, pod, reasons string, ended roundEnd,
	named []supervise.Claim, read workspaceRead, members []supervise.Claim) {
	bounded := ended.bounded()
	line := []any{"issue", issue, "round", round, "pod", pod, "atTheBound", bounded}
	if read.by == viaProcess {
		line = append(line, "notWaitedFor", namesOf(named))
	} else {
		line = append(line, read.by.String(), true, "unansweredMembers", namesOf(named))
	}
	s.log.Info("supervise: the pod of this update round is replaced and the round is over", append(line, read.attrs()...)...)
	if bounded {
		s.log.Info("supervise pod update replaced at the bound",
			append([]any{"issue", issue, "round", round, "pod", pod,
				"answered", namesOf(ended.answered), "unanswered", namesOf(ended.unanswered), "working", ended.working},
				ended.background.attributes()...)...)
	}
	s.tellWorkspaceLost(ctx, issue, round, reasons, named, read)
	s.endPodUpdates(issue, members, round)
}

// tellWorkspaceLost writes on the issue the one message a replacement made owes it where the pod
// may have taken work with it (workspaceRead.tells): named is the claims the message names, each
// with its state, and read is what the read of the workspace found (workspaceLostMessage). A
// message that would name no claim is not written. Each of the three replacements of a round —
// through a process, by issue and at a launch — calls it through roundReplaced once the
// replacement is made, and never for a replacement refused or failed, which ends nothing
// (replacePodForRound, replaceIssuePodForRound, endRoundALaunchReplaced).
func (s *supervision) tellWorkspaceLost(ctx context.Context, issue, round, reasons string, named []supervise.Claim, read workspaceRead) {
	if !read.tells() || len(named) == 0 {
		return
	}
	s.tellIssueOfRound(ctx, issue, round, workspaceLostMessage(round, reasons, named, read),
		"supervise: write on the issue what the workspace of its replaced pod held",
		"supervise: nothing is written on the issue what the workspace of its replaced pod held: its issue is another project's, whose rows this daemon writes none of")
}

// tellIssueOfRound writes body, a message round owes the issue, on the issue, and logs what came of
// the write: the line failed at Error where the write failed, and the line foreign where nothing
// was written because the issue is another project's (writeOnIssue). Each caller names its own
// message in both lines (tellWorkspaceLost, podWentWithoutItsRound).
func (s *supervision) tellIssueOfRound(ctx context.Context, issue, round, body, failed, foreign string) {
	written, err := s.writeOnIssue(ctx, issue, record.MessagePost{Body: body})
	switch {
	case err != nil:
		s.log.Error(failed, "issue", issue, "round", round, "error", err)
	case !written:
		s.log.Info(foreign, "issue", issue, "round", round)
	}
}

// workspaceRead is what a round's one read of its issue's workspace answered before the pod's
// replacement (readRoundWorkspace): the round it was made for, who made the replacement (by,
// roundReplacer), the role whose process made it, and what it found or the error it failed with.
// notMade is why no read was made, and it is set with the round alone, or with the round and a
// replacer that no process ran under (processlessRead). Only a read that was made is kept
// (podUpdateWorkspace).
type workspaceRead struct {
	round   string
	by      roundReplacer
	role    claim.Role
	work    runtime.WorkspaceWork
	err     error
	notMade string
}

// processlessRead is the read of a replacement by issue or at a launch (by): none was made,
// because no role process ran in the pod to make it (noProcessRead). The replacement by issue and
// the replacement at a launch both answer it (replaceIssuePodForRound, endRoundALaunchReplaced).
func processlessRead(round string, by roundReplacer) workspaceRead {
	return workspaceRead{round: round, by: by, notMade: noProcessRead}
}

// noProcess is whether the read is one that no process could make, because the replacement was
// made by issue or at a launch, where no role process ran in the pod (processlessRead). Unlike
// every other read that was not made, it tells the issue (tells): the pod may have taken work that
// nobody read.
func (r workspaceRead) noProcess() bool {
	return r.by != viaProcess
}

// The reasons the replacement's line gives for a replacement that made no read of the issue's
// workspace (readRoundWorkspace, replaceIssuePodForRound). People and agents read the line, so
// they follow ASD-STE100.
const (
	nothingPassedOverRead = "The round passed over no claim that had not answered."
	boundSteppedOverRead  = "The bound let the replacement through before every claim that the round asks answered and every turn ended."
	noWorkspaceReaderRead = "This runtime cannot read the issue workspace."
	noProcessRead         = "No role process ran in the pod. Only a role process can read the issue workspace."
)

// readRoundWorkspace is the round's one read of the issue's workspace before its pod's
// replacement (replacePodForRound), or why the replacement makes none. live and clock are what the
// replacement's own reading was taken over, and process is the claim the pod is replaced
// through, whose process runs in the round's pod: the read goes through that claim's role,
// since only a process in the pod can read the disk the replacement deletes
// (runtime.WorkspaceWorkReader).
//
// A read is made only where three things hold, read in this order:
//
//   - The round passed over a claim that has not answered (passedOverClaims). What such a claim
//     left unpushed is what the read is for: no role was asked to push it. A failed claim the
//     round did not wait for is named on the line, and it alone makes no read.
//   - Every claim the round asks has answered and no turn runs (roundEnd.settled). The round
//     reads the workspace only then (dispatch://INFRA-4109 R1), so a replacement the bound let
//     through over a claim that had not answered, or over a turn, makes no read. Background work
//     a role reports does not stop the read: it holds the replacement before the bound
//     (backgroundClaims), and past the bound it holds nothing, the read included.
//   - The runtime can read a workspace at all.
//
// A read made is kept for the round (podUpdateWorkspace), its error included, and a later call
// for the same round answers it rather than read again: a replacement the last word refused, or
// one that failed, comes back here at a later sweep, and the issue is told what one read found.
// A kept read is answered before the bound is weighed again, because it was made once every
// claim the round asks had answered and no turn ran. The read is bounded to one sweep interval,
// and a read its bound ends is a read that failed.
func (s *supervision) readRoundWorkspace(ctx context.Context, issue, round string, live []supervise.Claim, process runtime.Known, clock roundClock) workspaceRead {
	if len(passedOverClaims(live, round, clock)) == 0 {
		return workspaceRead{round: round, notMade: nothingPassedOverRead}
	}
	if kept, ok := s.keptWorkspaceRead(issue, round); ok {
		return kept
	}
	if !readRoundEnd(live, round, clock).settled() {
		return workspaceRead{round: round, notMade: boundSteppedOverRead}
	}
	reader, ok := s.runtime.(runtime.WorkspaceWorkReader)
	if !ok {
		return workspaceRead{round: round, notMade: noWorkspaceReaderRead}
	}
	read := workspaceRead{round: round}
	if role, named := process.Claim.Role(); !named {
		read.err = fmt.Errorf("the claim %s names no role to read the workspace through", process.Claim)
	} else {
		limited, done := context.WithTimeout(ctx, s.plan.orphanSweep)
		read.role = role
		read.work, read.err = reader.WorkspaceWork(limited, issue, role)
		done()
	}
	s.keepWorkspaceRead(issue, read)
	return read
}

// keptWorkspaceRead is the read podUpdateWorkspace keeps for round of the issue, if it keeps one.
func (s *supervision) keptWorkspaceRead(issue, round string) (workspaceRead, bool) {
	s.podUpdateWorkspaceMu.Lock()
	defer s.podUpdateWorkspaceMu.Unlock()
	read, ok := s.podUpdateWorkspace[issue]
	return read, ok && read.round == round
}

// keepWorkspaceRead keeps read for its round of the issue (podUpdateWorkspace), in place of
// whatever was kept for the issue before.
func (s *supervision) keepWorkspaceRead(issue string, read workspaceRead) {
	s.podUpdateWorkspaceMu.Lock()
	defer s.podUpdateWorkspaceMu.Unlock()
	if s.podUpdateWorkspace == nil {
		s.podUpdateWorkspace = map[string]workspaceRead{}
	}
	s.podUpdateWorkspace[issue] = read
}

// forgetWorkspaceRead drops the read kept for round of the issue: the sweep has ended that round
// (endPodUpdates), and no replacement of it is left to name the read.
func (s *supervision) forgetWorkspaceRead(issue, round string) {
	s.podUpdateWorkspaceMu.Lock()
	defer s.podUpdateWorkspaceMu.Unlock()
	if read, ok := s.podUpdateWorkspace[issue]; ok && read.round == round {
		delete(s.podUpdateWorkspace, issue)
	}
}

// forgetWorkspaceReads drops the read kept for each issue that this sweep finds in another round
// than the read's, or in none (openPodRound): that round is over, whatever ended it, and no
// replacement of it is left to name the read. byIssue is the sweep's groups, so an issue with no
// live claim is in no round.
func (s *supervision) forgetWorkspaceReads(byIssue map[string][]supervise.Claim) {
	s.podUpdateWorkspaceMu.Lock()
	defer s.podUpdateWorkspaceMu.Unlock()
	for issue, read := range s.podUpdateWorkspace {
		if openPodRound(byIssue[issue]) != read.round {
			delete(s.podUpdateWorkspace, issue)
		}
	}
}

// attrs is the read as the replacement's line names it (replacePodForRound): what the read found
// — the working-copy commit, whether it held a change, the first work commits and how many there
// are in all — or the error it failed with, or why no read was made.
func (r workspaceRead) attrs() []any {
	switch {
	case r.notMade != "":
		return []any{"workspaceRead", "not made", "workspaceNotReadBecause", r.notMade}
	case r.err != nil:
		return []any{"workspaceRead", "failed", "workspaceReadBy", r.role, "workspaceReadError", r.err.Error()}
	}
	found := "found no work"
	if r.work.HoldsWork() {
		found = "found work"
	}
	return []any{"workspaceRead", found, "workspaceReadBy", r.role, "workspaceHead", r.work.Head,
		"workspaceChanged", r.work.Changed, "workspaceCommits", r.work.Commits, "workspaceCommitCount", r.work.Count}
}

// tells is whether the issue is told of the read (workspaceLostMessage): it found work that no
// branch or tag on origin holds, it failed, or no process could make it (noProcess), so the pod
// may have taken work with it. A read that found no work tells the issue nothing, and any other
// read that was not made tells it nothing.
func (r workspaceRead) tells() bool {
	return r.noProcess() || (r.notMade == "" && (r.err != nil || r.work.HoldsWork()))
}

// boundPastReason opens every reason the late-round notice gives (tellLateRound,
// tellPodlessRound): the notice is told only once the round's bound has come.
const boundPastReason = "The bound of the update round is past. "

// The reasons the late-round notice gives for the three readings that hold a round past its bound
// and name no claim of their own (tellLateRound). The controller reads them, so they follow
// ASD-STE100. No process in the round's pod holds a round past its bound only where the runtime
// replaces no pod by issue (runtime.IssuePodReplacer, roundWithNoProcessInItsPod): where it does,
// the walk replaces the pod once the bound has come, and only a refusal of that replacement is
// told (replaceIssuePodForRound, refusedReason). A pod the runtime cannot read decides nothing, so
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

// refusedReason is the reason the late-round notice gives for a round whose pod replacement the
// runtime refused past the bound, through a process or by issue (replacePodForRound,
// replaceIssuePodForRound), naming the runtime's error.
func refusedReason(err error) string {
	return fmt.Sprintf(boundPastReason+"The runtime refused the pod replacement: %v", err)
}

// roundWithNoProcessInItsPod decides a round, before or past its bound, whose live claims record
// no process in the round's own pod. There is nothing to replace the pod through, and that is no
// statement about the pod: a suspension lets the worker's process go (letGo clears the locator),
// and the relaunch of a claim can run for minutes of a fleet roll. Read as a replacement made, it
// would end the round over a pod that is still there and still drifted, which is the third way
// out of a round that must not exist — the task the round was holding would go out, and the next
// tick would open the round over the same pod again.
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
// reads the same rule as it runs and replaces the pod itself (roundPodEndsAtLaunch). Before the
// bound, a claim a project's pause stopped keeps the pod while the pause still keeps it, and so
// does a claim the round waits for that holds the round's ask and has not answered it; the round
// then waits. A runtime that replaces no pod by issue (runtime.IssuePodReplacer) leaves the round
// waiting too, and such a pod holds the round past its bound, so the operator's side is told of it
// once the bound has come (tellLateRound, noProcessReason).
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
// The snapshot always records a process in that pod, and that process's claim is the one this
// asks through: the sweep decides a round here only over the pod its claims record, and ends a
// round over any other pod before it gets this far (reconcileIssuePodUpdate).
//
// It reports the replacement by issue it leaves the sweep, if any, and whether it decided the
// round (completePodUpdate).
func (s *supervision) roundWithNoProcessInItsPod(ctx context.Context, issue string, snapshot, live []supervise.Claim, round, pod, reasons string,
	held []supervise.Claim, clock roundClock) (*podReplacement, bool) {
	recorded, _ := podProcess(snapshot, round)
	switch _, err := s.runtime.PodDrift(ctx, recorded); {
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
// sweep found complete is not complete now, or the claims could not be read to say that it is.
// The runtime hands it back from inside ReplacePod having written nothing (runtime.Runtime), and
// replacePodForRound tells it from a replacement that failed by this error alone — a failure is
// a hold the operator's side is told of, and this is the daemon's own refusal, which costs the
// round one sweep and nothing else.
var errReplacementNotDue = errors.New("the pod replacement is no longer due")

// replacementDue is the daemon's last word on a replacement, which the runtime asks under the
// pod's launch turn and one statement before the write that ends every process in that pod. It
// reads the issue's claims as they are at that instant, at the clock as it stands then, and
// refuses the replacement when one of five readings holds it: an operator's suspension of a
// claim the round asks (heldByOperator) and a project's pause's hold of the pod
// (store.Store.PausePodHoldOf, read from the store now, decision 12 of dispatch://INFRA-3759),
// whatever the clock says; and, only before the round's bound, a claim the round asks that has
// not answered, a claim of the issue that runs a turn, or a claim with a process in the round's
// pod whose report names background work that runs or that has no report since its last turn
// (roundEnd.holding). It does not call readReplacement: it takes the five of that predicate's
// readings that are about the claims and the hold, through the same helpers, because the runtime
// asks it only of a pod it found there. What it read of the round's end is left in ended, which
// the replacement's log lines name. The Sandbox runtime asks this only of a pod that is there
// (replacePodForRound), so the operator's hold it reads first is a hold over a pod there is to
// replace. holds is what the pause did to the pod as the sweep read it, which the round's bound
// moves by and which says whether the round passes over a claim the pause stopped
// (roundClock.passesOver): both are read through one roundClock over them at the clock as it
// stands then.
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
func (s *supervision) replacementDue(issue, round string, holds podHolds, ended *roundEnd) func(context.Context) error {
	return func(ctx context.Context) error {
		claims, err := s.supervisor.Claims(ctx)
		if err != nil {
			return fmt.Errorf("%w: the claims of %s cannot be read: %w", errReplacementNotDue, issue, err)
		}
		group := s.viewedGroup(claims, issue)
		clock := s.roundClockAt(s.plan.clock.Now(), holds)
		if held := heldByOperator(group, round, clock); len(held) > 0 {
			return fmt.Errorf("%w: an operator's suspension holds %s", errReplacementNotDue, strings.Join(namesOf(held), ", "))
		}
		if err := s.pauseHoldsPod(ctx, issue); err != nil {
			return err
		}
		*ended = readRoundEnd(group, round, clock)
		unanswered, working, background := ended.holding()
		if len(unanswered) > 0 {
			return fmt.Errorf("%w: %s has not answered it", errReplacementNotDue, strings.Join(namesOf(unanswered), ", "))
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

// pauseHoldsPod is errReplacementNotDue where a project's pause holds the issue's pod now, read
// from the store (store.Store.PausePodHoldOf, decision 12 of dispatch://INFRA-3759), or where that
// cannot be read, and nil where no pause holds the pod. Each last word on a replacement reads it
// (replacementDue, issueReplacementDue).
func (s *supervision) pauseHoldsPod(ctx context.Context, issue string) error {
	switch _, held, err := s.supervisor.store.PausePodHoldOf(ctx, s.cfg.Project, issue); {
	case err != nil:
		return fmt.Errorf("%w: whether a pause holds the pod of %s cannot be read: %w", errReplacementNotDue, issue, err)
	case held:
		return fmt.Errorf("%w: a project's pause holds the pod of %s", errReplacementNotDue, issue)
	}
	return nil
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
//   - Before the round's bound, a claim the round waits for (waitingClaims) that a project's
//     pause stopped, while the pause still keeps the pod (supervise.Claim.Released,
//     podHolds.keeping): no hold stands, but the let-out of a launch the pause held for a role of
//     the pod is still ahead. The pause, not the claim, keeps the claim from a turn, and the
//     pause kept the pod for the work on its disk (decision 12 of dispatch://INFRA-3759,
//     roundClock.passesOver). This holds whether or not an ask of the round reached the claim.
//     No round opens while a hold stands, so a round that opened after the hold ended never
//     asked a role the pause stopped. Without this reading, the pod and that role's unpushed work
//     would go when the last process left the pod before the let-out.
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
	pauseKeeps := clock.holds.keeping(clock.now)
	var stopped, asked []supervise.Claim
	for _, c := range waitingClaims(group, round, clock) {
		switch {
		case pauseKeeps && c.Released():
			stopped = append(stopped, c)
		case holdsUnansweredAsk(c, round):
			asked = append(asked, c)
		}
	}
	if len(stopped) > 0 {
		return false, "a project's pause keeps the pod for the " + strings.Join(namesOf(stopped), ", ") + " it stopped"
	}
	if len(asked) > 0 {
		return false, strings.Join(namesOf(asked), ", ") + " holds the ask of the round and has not answered it"
	}
	return true, ""
}

// issuePodKept is why a replacement by issue may not be made at one read of the issue's claims,
// wrapping errReplacementNotDue, or nil where it may. The replacement reads it before the pod's
// launch turn and again under it (replaceIssuePodForRound, issueReplacementDue). group is the
// issue's claims as their machines hold them, masked (roundGroup), and clock is the moment of the
// read.
//
// A claim whose machine is deciding (Machine.Deciding) keeps the pod for that read: the decision
// can be a launch into the pod that has let the launch turn go and has not written its locator
// yet, and a replacement then would cut its first turn (dispatch://INFRA-4109 R7). A claim that
// records a process in the round's pod (podProcess) keeps it too: the round is then an ordinary
// round, replaced through that process once it is over. Last comes what holds the pod of a round
// with no process in it (roundPodEnds).
func (s *supervision) issuePodKept(group []supervise.Claim, round string, clock roundClock) error {
	for _, c := range group {
		if m, ok := s.supervisor.Machine(c.Token); ok && m.Deciding() != nil {
			return fmt.Errorf("%w: the machine of the %s is in a decision", errReplacementNotDue, c.Role)
		}
	}
	if process, running := podProcess(group, round); running {
		return fmt.Errorf("%w: the claim %s records a process in the pod", errReplacementNotDue, process.Claim)
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
// (tellLateRound, refusedReason).
//
// A replacement made ends the round as a replacement the round made (endPodUpdates), and the
// runtime's nil for a pod it found no longer there ends it the same way, as ReplacePod's does. No
// process can read the issue's workspace before the replacement, so the round makes no read
// (processlessRead), and what may go with the pod is named instead: every member of the
// round with a session that had not answered it (lostClaims), read unmasked (noticeGroup) as a
// gone pod's end reads them (endRoundOverAGonePod). The replacement's line names them as the
// members that had not answered (unansweredMembers), says that the replacement was by issue and
// why no read was made, and one message on the issue names each of them and says that no process
// could read the workspace (roundReplaced, tellWorkspaceLost, workspaceLostMessage). A round every
// member of which had answered writes nothing on the issue, as a gone pod's round writes nothing.
// A replacement that the bound let through over a claim the round waits for is logged on the
// bound's own line too, as replacePodForRound logs it.
//
// The call is bounded to one sweep interval, as replacePodForRound's is.
func (s *supervision) replaceIssuePodForRound(ctx context.Context, replace podReplacement) {
	issue, round, pod, reasons := replace.issue, replace.round, replace.pod, replace.reasons
	replacer, ok := s.runtime.(runtime.IssuePodReplacer)
	if !ok {
		s.log.Error("supervise: this runtime cannot replace a pod by issue; nothing was replaced and the round stays open",
			"issue", issue, "round", round, "pod", pod)
		return
	}
	claims, err := s.supervisor.Claims(ctx)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Error("supervise: read this issue's claims again before its update round's replacement by issue; nothing was replaced and the round stays open",
				"issue", issue, "round", round, "pod", pod, "error", err)
		}
		return
	}
	live := s.viewedGroup(claims, issue)
	clock := s.roundClockAt(s.plan.clock.Now(), replace.holds)
	if err := s.issuePodKept(live, round, clock); err != nil {
		s.log.Info("supervise: the pod of this update round is kept now the sweep has reached its replacement by issue; nothing was replaced and the round stays open",
			"issue", issue, "round", round, "pod", pod, "reason", err)
		return
	}
	told := s.noticeGroup(claimsOf(claims, issue))
	lost := lostClaims(told, round)
	limited, done := context.WithTimeout(ctx, s.plan.orphanSweep)
	defer done()
	// ended is what the last word read of the round's end under the launch turn. It stays the
	// zero roundEnd, which no bound made, where the runtime returns nil without asking the last
	// word.
	var ended roundEnd
	if err := replacer.ReplaceIssuePod(limited, issue, pod, s.issueReplacementDue(issue, round, replace.holds, &ended)); err != nil {
		if errors.Is(err, errReplacementNotDue) {
			s.log.Info("supervise: this update round's replacement by issue was no longer due when its pod's launch turn came; nothing was replaced and the round stays open",
				"issue", issue, "round", round, "pod", pod, "reason", err)
			return
		}
		if ctx.Err() == nil {
			s.log.Error("supervise: replace by issue the pod of an update round with no process in it; retrying at the next sweep",
				"issue", issue, "round", round, "pod", pod, "error", err)
			s.tellLateRound(ctx, issue, live, round, pod, reasons, refusedReason(err), clock)
		}
		return
	}
	s.roundReplaced(ctx, issue, round, pod, reasons, ended, lost, processlessRead(round, replace.by), told)
}

// issueReplacementDue is the daemon's last word on a replacement by issue, which the runtime asks
// under the pod's launch turn and one statement before the write that ends the pod
// (replaceIssuePodForRound). It reads the issue's claims as replacementDue reads them — which
// claims there are from the store, and what each holds from its machine's view (Machine.View),
// never from Machine.Claim, which waits out a decision that may wait on this very launch turn —
// at the clock as it stands then, over the holds the sweep read. It refuses the replacement where
// that read keeps the pod (issuePodKept) or a project's pause holds the pod now (pauseHoldsPod),
// and leaves what it read of the round's end in ended, which the replacement's lines name.
func (s *supervision) issueReplacementDue(issue, round string, holds podHolds, ended *roundEnd) func(context.Context) error {
	return func(ctx context.Context) error {
		claims, err := s.supervisor.Claims(ctx)
		if err != nil {
			return fmt.Errorf("%w: the claims of %s cannot be read: %w", errReplacementNotDue, issue, err)
		}
		group := s.viewedGroup(claims, issue)
		clock := s.roundClockAt(s.plan.clock.Now(), holds)
		if err := s.issuePodKept(group, round, clock); err != nil {
			return err
		}
		if err := s.pauseHoldsPod(ctx, issue); err != nil {
			return err
		}
		*ended = readRoundEnd(group, round, clock)
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
//     wake pacing parked is in no decision and keeps nothing.
//   - Something holds the pod (roundPodEnds): an operator's suspension of a member, a project
//     pause's hold that stands, or, before the bound, a member a project's pause stopped while the
//     pause still keeps the pod, or a member the round waits for that holds the round's ask it has
//     not answered.
//   - Before the bound, the launching claim holds the round's ask itself and has not answered it:
//     its process went while it held the ask, and its work may be on the disk. That holds of the
//     launch of a fresh session too. Such a claim records no session until its new agent
//     registers, so the round's sets (roundAsks) do not read it as a member yet, but its ask mark
//     stays across the fresh session, since only the round's end clears it.
//   - Before the bound, the launch is the one a project's pause held for the launching claim on
//     this pod (pauseHeldLaunch): the pause kept the pod for that claim's work (decision 12 of
//     dispatch://INFRA-3759). At its let-out the claim is launching, not suspended, and the
//     pause's keep has ended (podHolds.keeping), so the round's sets no longer read the claim as
//     the pause's, and the round never asked it. Without this case the launch would replace the
//     pod that the pause kept for that very work.
//
// The project pause's holds are read from the store as the sweep reads them (podPauseHolds), so
// the bound is read moved by the time the pause held the pod. A read of the holds that fails keeps
// the pod, since a reading nobody took may not delete a disk, and it is logged. The launching
// claim's open resume is read from the store too (pauseHeldLaunch), and a read of it that fails
// keeps the pod and is logged in the same way. A read of the claims that fails is the launch's
// error, which fails the launch as any of its steps does.
//
// It runs inside the launching machine's decision and under the pod's launch turn. So it reads
// which claims there are from the store and what each holds from its machine's published view and
// Deciding alone, never from Machine.Claim, which waits out a decision that may wait on this very
// launch turn (replacementDue). It reads every claim unmasked (roundGroup with no mask), unlike
// the other readers that may delete a disk. They mask the answer of a claim whose machine is
// deciding, because that decision may start a turn in the pod that the view does not show yet.
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
// Each answer is kept for the round (podUpdateLaunchReplaced), under a mutex that no runtime call
// is made under: a keep for token, which the next launch into the pod reads, and a replacement,
// which the sweep reads to end the round as the replacement it is (endRoundALaunchReplaced).
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
	claims, err := s.supervisor.Claims(ctx)
	if err != nil {
		return false, fmt.Errorf("read the claims of the update round over pod %s: %w", podUID, err)
	}
	group := s.roundGroup(claimsOf(claims, issue), false)
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
	now := s.plan.clock.Now()
	holds, err := s.podPauseHolds(ctx, group, now)
	if err != nil {
		s.log.Error("supervise: read the pods a project's pause holds; the launch keeps the pod of the update round over it",
			"issue", issue, "round", round, "claim", token, "error", err)
		return s.keepAtLaunch(issue, round, token, "the holds of a project's pause cannot be read")
	}
	clock := s.roundClockAt(now, holds[issue])
	if ends, held := roundPodEnds(group, round, clock); !ends {
		return s.keepAtLaunch(issue, round, token, held)
	}
	if !clock.pastBound(group, round) {
		if holdsUnansweredAsk(launching, round) {
			return s.keepAtLaunch(issue, round, token, "the launching claim holds the ask of the round and has not answered it")
		}
		pauseHeld, err := s.pauseHeldLaunch(ctx, issue, launching.Role, holds[issue])
		if err != nil {
			s.log.Error("supervise: read the open resume of the launching claim; the launch keeps the pod of the update round over it",
				"issue", issue, "round", round, "claim", token, "error", err)
			return s.keepAtLaunch(issue, round, token, "the open resume of the launching claim cannot be read")
		}
		if pauseHeld {
			return s.keepAtLaunch(issue, round, token,
				fmt.Sprintf("the launch of the %s is the one a project's pause held, and the pause kept the pod for its work", launching.Role))
		}
	}
	s.replacedAtLaunch(issue, round, readRoundEnd(group, round, clock))
	s.log.Info("supervise: no role process runs in the pod of this update round, and nothing holds the pod; the launch replaces the pod and starts its role in a new pod",
		"issue", issue, "round", round, "pod", podUID, "claim", token)
	return true, nil
}

// pauseHeldLaunch is whether the launch of the issue's role is the one a project's pause held for
// it on the issue's pod (roundPodEndsAtLaunch). The role's open resume names the pause that held
// its wake (store.Store.OpenPodResume, pod_resumes.pause_id), and holds, what the pauses did to
// the pod as the sweep reads it (podPauseHolds), has a hold of that pause on the pod that no
// deletion ended: a pod a deletion ended the hold of is gone, and the pause keeps nothing of it.
// pod_resumes holds the project token, so the resume is read by the plan's project, not by the
// Dispatch key the holds are read by. Where holds has no hold, no pause held this pod, and the
// store is not read.
func (s *supervision) pauseHeldLaunch(ctx context.Context, issue string, role claim.Role, holds podHolds) (bool, error) {
	if len(holds) == 0 {
		return false, nil
	}
	_, pause, open, err := s.supervisor.store.OpenPodResume(ctx, s.plan.project, issue, role)
	if err != nil || !open || pause == 0 {
		return false, err
	}
	return slices.ContainsFunc(holds, func(hold store.PausePodHold) bool {
		return hold.Pause == pause && hold.End != store.PausePodHoldDeleted
	}), nil
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
// pod (roundPodEndsAtLaunch), as podUpdateLaunchReplaced keeps it for the issue: the round, whether
// a launch replaced its pod and what the launch read of the round's end when it did, and each claim
// whose launch into the pod the hook kept the pod for.
type launchRound struct {
	round    string
	replaced bool
	ended    roundEnd
	kept     map[claim.Token]bool
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

// replacedAtLaunch keeps that a launch replaced round's pod as the round's own replacement, and
// ended, what the launch read of the round's end (podUpdateLaunchReplaced).
func (s *supervision) replacedAtLaunch(issue, round string, ended roundEnd) {
	s.launchRecord(issue, round, func(at *launchRound) { at.replaced, at.ended = true, ended })
}

// launchKept is whether roundPodEndsAtLaunch kept round's pod for token's launch into it.
func (s *supervision) launchKept(issue, round string, token claim.Token) bool {
	s.podUpdateLaunchMu.Lock()
	defer s.podUpdateLaunchMu.Unlock()
	at := s.podUpdateLaunchReplaced[issue]
	return at != nil && at.round == round && at.kept[token]
}

// launchReplaced is whether a launch replaced round's pod as the round's own replacement, and what
// that launch read of the round's end (podUpdateLaunchReplaced).
func (s *supervision) launchReplaced(issue, round string) (roundEnd, bool) {
	s.podUpdateLaunchMu.Lock()
	defer s.podUpdateLaunchMu.Unlock()
	at := s.podUpdateLaunchReplaced[issue]
	if at == nil || at.round != round || !at.replaced {
		return roundEnd{}, false
	}
	return at.ended, true
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
// another pod in its place (endRoundWhosePodWent). The launch made the
// replacement the sweep makes by issue (replaceIssuePodForRound), by the same rule, so the end is
// that replacement's (roundReplaced): the line of a replacement the round made, here at a launch
// (atLaunch), which names every member of the round with a session that had not answered it
// (unansweredMembers: lostClaims, read unmasked as noticeGroup reads them) and says why no read of
// the workspace was made, since no role process ran in the pod; the bound's own line where the
// bound let the replacement through (ended, what the launch read under the pod's launch turn); and
// one message on the issue that names those members and says that no process could read the
// workspace (tellWorkspaceLost). It is not podWentWithoutItsRound's end, which tells the issue
// that its pod went before its round was over: this round was over, and its own replacement took
// the pod. The record goes with the end (endPodUpdates).
//
// The record is in memory alone. A daemon that restarts between the launch and the sweep reads the
// same end as a pod gone (podWentWithoutItsRound), whose message names the same members as having
// lost what they had not pushed.
func (s *supervision) endRoundALaunchReplaced(ctx context.Context, issue string, group []supervise.Claim, round string, ended roundEnd) {
	told := s.noticeGroup(group)
	s.roundReplaced(ctx, issue, round, strings.TrimPrefix(round, supervise.PodRoundPrefix), roundReasons(told, round), ended,
		lostClaims(told, round), processlessRead(round, viaLaunch), told)
}

// viewedGroup is every claim of the issue a round weighs, as the store names them and as each
// machine last published it: the store says which claims there are, and roundGroup says what
// each holds now, with the answer of a claim whose machine is deciding masked, as the decision
// the replacement rests on needs it.
func (s *supervision) viewedGroup(claims []supervise.Claim, issue string) []supervise.Claim {
	return s.roundGroup(claimsOf(claims, issue), true)
}

// viewsRecordRound is whether a claim of the issue that has not retired records round, read from
// each machine's published view (Machine.View) alone: never from Machine.Claim, which waits out
// the decision in flight, and never from the store. roundPodEndsAtLaunch asks it under a pod's
// launch turn before it reads the store, and reads the store only where this answers yes.
func (s *supervision) viewsRecordRound(issue, round string) bool {
	for _, m := range s.supervisor.supervised() {
		view := m.View()
		if view.Issue == issue && view.State != supervise.StateRetired && memberOf(view.Claim, round) {
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

// liveGroup is the group as its claims' own machines hold them now, in the group's order, with
// the same mask: it is what the sweep's own decision to replace the pod is read from.
func (s *supervision) liveGroup(group []supervise.Claim) []supervise.Claim {
	return s.roundGroup(group, true)
}

// noticeGroup is the group as the operator's side is told of it: the same fresh read, unmasked.
// The mask is a decision the replacement makes — a claim whose machine is deciding has not
// answered, for the purpose of deleting a disk — and it is not a fact about the claim. Told to
// the controller it would be a false one: the notice marks each claim the round asks with
// whether it answered (record.UpdateWaiting.Answered), and a masked claim would read as one that
// said nothing although it answered.
func (s *supervision) noticeGroup(group []supervise.Claim) []supervise.Claim {
	return s.roundGroup(group, false)
}

// roundGroup is the claims of group as a round reads them now, less the ones no round weighs.
// It is the one read the sweep's decision, the replacement's last word and the operator's notice
// are all built from, so no two of them can disagree about what a claim holds. The round asks
// each claim roundAsks keeps, and replaces the pod once each of them has answered, no turn runs
// and no claim with a process in the round's pod reports background work that runs or has sent
// no report since its last turn started (roundEnd.holding), or at the round's bound
// (pod_update_grace_minutes) whatever the roles answered and whatever runs, an operator's
// suspension excepted.
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
// mask is whether a claim whose machine is deciding something now (Machine.Deciding, which waits
// on nothing either) is read as not having answered. The view of such a claim is what the
// decision in flight last tried to write, and the decision may be a turn starting that the view
// does not show yet, so a reader about to delete a disk before the round's bound masks it: the
// cost is one sweep, and the cost of the other way is a turn cut before the bound. A reader that
// only reports masks nothing (noticeGroup).
func (s *supervision) roundGroup(group []supervise.Claim, mask bool) []supervise.Claim {
	read := make([]supervise.Claim, 0, len(group))
	for _, c := range group {
		viewed := c
		if m, ok := s.supervisor.Machine(c.Token); ok {
			viewed = m.View().Claim
			if viewed.Generation == c.Generation {
				viewed.ActivityAt, viewed.ActivityBackground = c.ActivityAt, c.ActivityBackground
				viewed.ActivityKinds, viewed.ActivityNextReminderAt = c.ActivityKinds, c.ActivityNextReminderAt
			} else {
				viewed.ActivityAt, viewed.ActivityBackground = time.Time{}, false
				viewed.ActivityKinds, viewed.ActivityNextReminderAt = nil, time.Time{}
			}
			if mask && m.Deciding() != nil {
				viewed.UpdateSignalledAt, viewed.UpdateHead = nil, ""
			}
		}
		if viewed.State != supervise.StateRetired {
			read = append(read, viewed)
		}
	}
	return read
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
// The notice lists every claim of the group with a session that has not failed (noticeClaims),
// each with whether it answered (record.UpdateWaiting.Answered), read unmasked (noticeGroup):
// past the bound no answer holds the round, so the list says what each role said rather than
// whom the round waits for. The list names a claim the daemon suspended too, whether or not the
// round asks it and waits for it (roundAsks, roundClock.passesOver): the controller reads every
// role of the pod, each with its state. It is never empty while the group holds such a claim,
// which the store requires of the notice (record.UpdateStalled). A group with no such claim is
// told nothing: there is no role to name. A round that group already records as told is told
// nothing again and read no further.
func (s *supervision) tellLateRound(ctx context.Context, issue string, group []supervise.Claim, round, pod, reasons, why string, clock roundClock) {
	if !clock.pastBound(group, round) || roundTold(group, round) {
		return
	}
	told := s.noticeGroup(group)
	waiting := waitingOf(round, noticeClaims(told))
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
// replaceIssuePodForRound): the pod is replaced before any launch goes into it, and the issue is
// told which members had not answered, since no process could read the workspace
// (dispatch://INFRA-4109 R7). The replacement is reported for the walk to hand on
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
			if replace := s.replacementByIssue(issue, s.liveGroup(group), round, sandboxPod, roundReasons(group, round), clock); replace != nil {
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
// A round every claim of which it waits for has answered (waitingClaims), with no claim an
// operator holds, is told nothing, and that is the ordinary end of a phase rather than a hold:
// the workers answered, the daemon suspended each of them, and letGo took the last locator with
// the last process. A claim the daemon suspended before it answered does not change that: the
// round does not wait for it (roundAsks, dispatch://INFRA-4109 R1). A claim a project's pause
// stopped is the one exception while the pause still keeps the pod (roundClock.passesOver): the
// round waits for it, and a round past its bound with it unanswered is told. Nobody has to do
// anything about the ordinary end — the replacement by issue ends it, or, where the runtime makes
// none, a role of the issue that runs in a pod again ends it — so telling the controller only
// opens a finding ask over a round that needs no one.
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
	if len(held) == 0 && len(waitingClaims(told, round, clock)) == 0 {
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
// Nothing here can bring the work back — the disk went with the pod — so the message is the whole
// of it. On another project's issue, the pod of the operator's spawn of it, no message is written
// (writeOnIssue), and the daemon's log lines are the whole of it.
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
// waitingClaims: the members of the round that neither answered nor left having answered. A
// claim this issue holds that is in no round of its own — one an earlier sweep's end already
// took out of this round, or a role first started in the pod that replaced this round's own —
// was never in the pod whose disk went, and naming it would tell the issue it lost work that
// nobody wrote.
func (s *supervision) podWentWithoutItsRound(ctx context.Context, issue string, group []supervise.Claim, round, pod string) {
	lost := lostClaims(group, round)
	reasons := roundReasons(group, round)
	s.log.Warn("supervise: the pod of an open update round is gone; the roles that had not answered lost what they had not pushed",
		"issue", issue, "round", round, "pod", pod, "lost", namesOf(lost))
	s.tellIssueOfRound(ctx, issue, round, podWentMessage(round, reasons, lost),
		"supervise: write on the issue that its pod went without its update round",
		"supervise: nothing is written on the issue that its pod went without its update round: its issue is another project's, whose rows this daemon writes none of")
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
//     lost what it had not pushed: the issue is told so (podWentWithoutItsRound).
//   - Otherwise every member of the round pushed its work before the pod went, nothing is left to
//     tell the issue, and the round ends with the line over alone (endPodUpdates).
//
// What was lost is counted exactly as the message counts it (lostClaims), so the message is
// written when, and only when, there is a role to name. Counted as the claims the replacement
// waits for (waitingClaims), it would be written for a claim of the issue that is in no round at
// all — one an earlier sweep's end has already taken out of this round, or a role first started
// in the pod that replaced this one — naming either nobody or a worker that lost nothing.
//
// unmasked is which group the last two ends count and end the round on: false is group as the
// caller read it, and true is group as its claims' machines hold it now, unmasked (noticeGroup),
// which is read only once the end is not the launch's. over is the line of the last end, which
// each caller words for the reading that found the pod gone, with the issue, the round and the
// pod.
func (s *supervision) endRoundWhosePodWent(ctx context.Context, issue string, group []supervise.Claim, round, pod string, unmasked bool, over string) {
	if ended, replaced := s.launchReplaced(issue, round); replaced {
		s.endRoundALaunchReplaced(ctx, issue, group, round, ended)
		return
	}
	if unmasked {
		group = s.noticeGroup(group)
	}
	if len(lostClaims(group, round)) > 0 {
		s.podWentWithoutItsRound(ctx, issue, group, round, pod)
		return
	}
	s.log.Info(over, "issue", issue, "round", round, "pod", pod)
	s.endPodUpdates(issue, group, round)
}

// endRoundOverAGonePod ends the round the sweep has just read the pod of as gone. The report is
// decided from the parts of the round its claims hold: a claim the daemon started again in
// another pod keeps the part the dead pod left it (Claim.LeftPodRound, the round and the answer
// it left with), so no turn or answer of the process that replaced it changes the report.
//
// What was lost is the members of the round that neither answered it nor left it having
// answered (lostClaims), so a round every member of which pushed its work lost nothing and is
// told to nobody: a message naming no role would say an issue lost work it did not lose. A
// claim of this issue that is in no round at all is a member of none, and is named by nothing
// here.
//
// Those answers are read from the machines as they stand rather than from the tick's snapshot,
// and unmasked (noticeGroup): an answer recorded is an answer whatever its machine is deciding
// now, and what this decides is a report of what is already gone, never the deletion of a disk —
// which is what the mask exists for (roundGroup).
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
	s.endRoundWhosePodWent(ctx, issue, group, round, pod, true,
		"supervise: the pod of this issue's update round is not there, and every claim of the round had answered; nothing was lost and the round is over")
}

// openPodUpdate opens the round, or keeps it open, and asks each claim the round asks (roundAsks)
// once: one ask that reaches the claim's session, since an ask that reached none is asked again
// (askRound). Every claim with a session is posted PodUpdateOpen at every tick while the round
// runs: the event is idempotent, and it is what records the round on the claim (supervise's
// openPodUpdate), with its reasons and its clock fixed at the post that opens it.
//
// The ask is the sweep's own, enqueued on the reading that finds the round recorded (askRound).
// The post is asynchronous (supervisor.post), so the tick that opens a round asks nothing, and a
// later tick asks each claim whose record shows this round, no ask that reached it and no ask
// still queued for it, once its session is ready to take the ask. A claim that left the round's
// pod, a failed claim and a claim the round passes over are asked nothing (roundAsks,
// roundClock.passesOver).
//
// The ask reads the group the tick already read and takes no machine's lock: the outbox's row
// takes the claim's lock when it publishes, and waits rather than block where a decision holds it
// (supervise's Machine.PublishUpdateAsk). So whether this tick's posts land before or after the
// ask changes nothing it decides.
//
// Every claim of the group with a session is posted the round (sessionClaims): an agent has run
// for it, and may hold unpushed work. A claim with no session has never had a worker and is
// posted nothing. The round asks and waits for the claims roundAsks keeps, and for no other. An
// operator-suspended claim holds the round until the operator acts, whatever the round's bound,
// which is this issue's own rule (readReplacement's waitsForOperator), as long as the round's pod
// is there: a gone pod ends its round whatever the hold (roundWithNoProcessInItsPod). The
// late-round notice is what tells the operator (tellLateRound, tellPodlessRound). A failed claim
// and a claim the daemon suspended run no turn on the pod and are asked nothing, so the round does
// not wait for them (roundAsks, dispatch://INFRA-4109 R1). A claim a project's pause stopped is
// the exception while the pause still keeps the pod (roundClock.passesOver): the round waits for
// it and asks it once its agent is ready. A worker whose plugin has no ready_for_update op is
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
// the round although the round asks it nothing, so it counts. An issue with no claim that records
// the round holds no slot: charged an opening all the same, it would be read as an opening of
// this daemon's still in flight at every later sweep (podUpdateOpenings), hold that slot for
// ever, and leave every other drifted issue waiting until the daemon restarts.
func (s *supervision) openPodUpdate(ctx context.Context, issue string, group []supervise.Claim, pod, reasons string, clock roundClock) bool {
	round := supervise.PodRound(pod)
	posted := sessionClaims(group)
	reached := len(noticeClaims(group)) > 0
	s.askRound(ctx, issue, group, round, pod, clock)
	for _, c := range posted {
		s.supervisor.post(c.Token, supervise.PodUpdateOpen{Claim: c.Token, Round: round, Reasons: reasons})
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
// (supervision.enqueue). Its line is then the whole of its telling, as the stall backstop's line
// is for a claim no owner can be told of (tellStallBackstopped), and says that the controller is
// told nothing; the round is marked told, so the line is written once rather than at every tick.
func (s *supervision) tellUpdateStalled(ctx context.Context, stall updateStall) {
	if roundTold(stall.group, stall.round) {
		return
	}
	if s.enqueue == nil {
		s.log.Warn("supervise: an update round is held past its bound, and this deployment has no workflow to tell the operator's side through",
			"issue", stall.issue, "round", stall.round, "reason", stall.reason)
		return
	}
	rows := []record.OutboxPayload{record.ControllerNotice{
		Kind:   record.UpdateStalledNotice,
		Reason: stall.reason,
		Update: &record.UpdateStalled{Round: stall.round, Pod: stall.pod, Reasons: stall.reasons, Waiting: stall.waiting},
	}}
	written, err := s.enqueue(ctx, stall.issue, rows...)
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
// stopped by a project's pause that still keeps the pod (the round asks no other claim the daemon
// suspended: roundAsks, roundClock.passesOver). No live session of it holds the role's topic, so
// the publish would end at notify.ErrNoHolder at every sweep, and the machine refuses the ask of
// a claim the pause stopped before any publish (supervise's UpdateAskSuspended): a row and a log
// line a minute for a claim that may stay suspended until the operator resumes it or the pause's
// resume launches it. The first tick after the agent says it is ready asks it. A ready session
// claims its topic inside its first turn or at the first-turn bound (supervise.SaidReady), so an
// ask in that short window can still end at notify.ErrNoHolder, and the claim is then asked again
// at a later sweep, as above.
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
	if s.enqueue == nil {
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
	text := supervise.UpdateTask(round, roundReasons(group, round), *bound)
	rows := make([]record.OutboxPayload, len(asking))
	for i, c := range asking {
		rows[i] = record.UpdateAsk{Role: c.Role, Round: round, Pod: pod, Text: text}
	}
	written, err := s.enqueue(ctx, issue, rows...)
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
// over are asked nothing, because roundAsks leaves both out. A claim a project's pause stopped is
// in while the pause keeps the pod (roundClock.passesOver), and askRound asks it once its agent is
// ready, as it asks any claim.
func unaskedClaims(group []supervise.Claim, round string, clock roundClock) []supervise.Claim {
	var asking []supervise.Claim
	for _, c := range group {
		if roundAsks(c, round, clock) && c.UpdateRound == round && c.UpdateTaskedAt == nil && !answered(c, round) {
			asking = append(asking, c)
		}
	}
	return asking
}

// writeOnIssue posts one message on the issue, with the document it links when it links one,
// through the outbox's Dispatch message row, and answers whether it queued it: it queues none on
// another project's issue (supervision.enqueue).
func (s *supervision) writeOnIssue(ctx context.Context, issue string, post record.MessagePost) (bool, error) {
	if s.enqueue == nil {
		return false, fmt.Errorf("write on %s: this deployment has no workflow to write through", issue)
	}
	return s.enqueue(ctx, issue, post)
}

// endPodUpdates ends round of the issue on every claim of the group that is one of its members
// (memberOf), and on no other. It is the one way the sweep ends a round: the round's own
// replacement (replacePodForRound, by issue replaceIssuePodForRound, or at a launch
// endRoundALaunchReplaced), its pod gone with another pod recorded in its place
// (reconcileIssuePodUpdate), its pod read gone (endRoundOverAGonePod) and its pod gone with a
// member's work (podWentWithoutItsRound) all end it here. A round ends only where the pod it was
// opened over is gone — replaced by the round itself, replaced under it, or gone for a failure of
// its own — so the post carries nothing of that pod's fate (supervise.PodUpdateEnded). The read of
// the issue's workspace the round kept goes with it (forgetWorkspaceRead): no replacement of the
// round is left to name the read. So does what the launches into the pod left of the round
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
	s.forgetWorkspaceRead(issue, round)
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

// groupDrift is why the group's pod has drifted: the union of the runtime's PodDrift over every
// claim of the group that records a process, in the claims' own order and without repeats, with
// the reading that says what those reasons are worth (podReading).
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
func (s *supervision) groupDrift(ctx context.Context, issue string, group []supervise.Claim) ([]string, podReading) {
	var reasons []string
	for _, c := range group {
		if c.Locator == nil {
			continue
		}
		drift, err := s.runtime.PodDrift(ctx, runtime.Known{Claim: c.Token, Locator: c.Locator})
		if errors.Is(err, runtime.ErrPodGone) {
			return nil, podGone
		}
		if err != nil {
			if ctx.Err() == nil {
				s.log.Error("supervise: read whether this issue's pod has drifted", "issue", issue, "claim", c.Token, "error", err)
			}
			return nil, podUnread
		}
		for _, reason := range drift {
			if !slices.Contains(reasons, reason) {
				reasons = append(reasons, reason)
			}
		}
	}
	return reasons, podRead
}

// groupPod is the pod every claim of the group that records one runs in, and whether they record
// two different pods, which is a replacement already in flight.
func groupPod(group []supervise.Claim) (string, bool) {
	pod := ""
	for _, c := range group {
		found := podOf(c.Locator)
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
// round's ask states, never cut up (record.UpdateStalled).
func roundReasons(group []supervise.Claim, round string) string {
	for _, c := range group {
		if c.UpdateRound == round && c.UpdateReasons != "" {
			return c.UpdateReasons
		}
	}
	return ""
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
// the round's answers are weighed — the claims it still waits on before its bound
// (waitingClaims), the claims the line of a bounded replacement names as having answered
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

// sessionClaims is every claim of the group with a session, whose agent has run and may hold
// unpushed work: the claims the round is posted to (openPodUpdate), and the members a gone pod's
// report weighs (lostClaims). The round asks and waits for a part of them (roundAsks). Every
// claim the sweep groups is live already (reconcilePodUpdates drops the retired ones), and a
// claim with no session has never had a worker.
func sessionClaims(group []supervise.Claim) []supervise.Claim {
	var sessions []supervise.Claim
	for _, c := range group {
		if c.Session != "" {
			sessions = append(sessions, c)
		}
	}
	return sessions
}

// waitingClaims is whom the replacement waits for before the round's bound, and whom the line of
// a replacement the bound made names as not having answered: every claim the round asks at the
// clock's now (roundAsks) that has not answered this round. It is deliberately the conservative of
// the round's two questions, because what it decides before the bound is the deletion of a disk. A
// claim the round has not reached yet — one whose recorded round is another, or none — has not
// answered it either, and it holds the pod until it does or the bound comes: the round asks it at
// the next tick, and a replacement made before that would end a turn of a worker the round asked
// nothing of.
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
// claim the daemon suspended except one a project's pause stopped while the pause still keeps the
// pod. It runs no process and no turn in the pod, and the round does not ask it. A replacement
// held for its answer would be held to the bound for an answer that nobody asked for
// (dispatch://INFRA-4109 R1). A claim the pause stopped is waited for while the pause keeps the
// pod: the pause, not the claim, kept it from a turn, and the work on the pod's disk is what the
// pause kept the pod for.
//
// What a claim lost is the other question (lostClaims), and the two are never the same set.
func waitingClaims(group []supervise.Claim, round string, clock roundClock) []supervise.Claim {
	var waiting []supervise.Claim
	for _, c := range group {
		if roundAsks(c, round, clock) && !answered(c, round) {
			waiting = append(waiting, c)
		}
	}
	return waiting
}

// roundAsks is whether the round, read at the clock's now, asks c and waits for its answer. It is
// the one predicate that both sets of the round read (dispatch://INFRA-4109 R2): the claims the
// sweep asks (unaskedClaims, askRound) and the claims the replacement waits for (waitingClaims),
// with the operator's hold (heldByOperator) and the answers a bounded replacement names
// (answeredClaims). So no claim is waited for and not asked. Two predicates held BET-1182's pod
// until its bound on 2026-11-16: the replacement waited for every claim with a session that had
// not failed, and the sweep asked only a claim whose agent was ready. So a claim the daemon had
// suspended was waited for and never asked. The predicate takes the round's clock because one
// exception to R1 is read from what a project's pause did to the pod (roundClock.passesOver).
//
// A claim the round asks is asked once its agent says it is ready (askRound reads
// supervise.SaidReady), and it is waited for until then. So a queued, launching or registered
// claim is in, and so is a claim that records another round or none: the round asks it at the
// tick after its post lands. A claim an operator suspended is in, and so is a claim whose
// operator suspension waits for its agent's turn to end: the round waits for the operator,
// whatever its bound (heldByOperator, dispatch://INFRA-2981 R5). A claim a project's pause stopped
// is in while the pause still keeps the pod (roundClock.passesOver): the pause kept the pod for
// the work on its disk, and the round waits for its answer before the bound the hold moved.
//
// Five kinds of claim are out:
//
//   - A claim with no session has never had a worker.
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
//     (supervise's Claim.PassedOverByRound), but not one a project's pause stopped while the
//     pause still keeps the pod. Such a claim runs no process and no turn in the pod
//     (dispatch://INFRA-4109 R1). An ask would reach no session. A wake or a resume would
//     override the reason the daemon stopped the claim: an idle-released pod is not woken to be
//     asked, because it holds no turn to interrupt (dispatch://INFRA-2981). The outbox's row
//     refuses the ask of such a claim too (supervise's Machine.PublishUpdateAsk), so a row queued
//     before the suspension publishes nothing. It refuses the ask of a claim the pause keeps as
//     well, since a publish to a role with no holder raises a role wake; the sweep asks that
//     claim only once its agent is ready, and a ready claim is no longer suspended.
//
// Work that a claim left out of the round left unpushed on the pod's disk goes with the pod. The
// replacement's log line names each failed claim and each claim the round passed over that it
// did not wait for and that had not answered (notWaitedFor). Where it passed over a claim the
// daemon suspended, the replacement first reads the issue's workspace, and the line names what
// the read found (readRoundWorkspace).
func roundAsks(c supervise.Claim, round string, clock roundClock) bool {
	return c.Session != "" && c.State != supervise.StateFailed && c.State != supervise.StateRetired &&
		!c.LeftPodRound(round) && !clock.passesOver(c)
}

// noticeClaims is every claim of the group with a session that has not failed. It is wider than
// the claims the round asks (roundAsks), and it answers two questions that are not whom the round
// asks: the claims the late-round notice lists (tellLateRound), and whether any claim's machine
// records the round once its post lands (openPodUpdate). A claim the daemon suspended is in it.
// Its machine records the round in the suspended state (supervise's table), and the notice names
// it with its state, so the controller reads every role of the pod and the notice is never empty
// while the round has a role (record.UpdateStalled). A failed claim is out: its machine ignores
// the round's post, so it records no round.
func noticeClaims(group []supervise.Claim) []supervise.Claim {
	var of []supervise.Claim
	for _, c := range sessionClaims(group) {
		if c.State != supervise.StateFailed {
			of = append(of, c)
		}
	}
	return of
}

// notWaitedFor is every claim of the group with a session that the round does not ask (roundAsks)
// and that has neither answered the round nor left it. It reads the one predicate of the round's
// two sets (dispatch://INFRA-4109 R2), so each claim it names is one the round neither asks nor
// waits for. Over a group a replacement reads, which holds no retired claim (roundGroup), each is
// a failed claim or a claim the round passes over at the clock's now (roundClock.passesOver).
// Neither runs a process in the pod, so what either left unpushed on the pod's disk goes with the
// pod, and the replacement names each of them on its line and, where the read of the workspace
// found work or failed, on the issue (replacePodForRound). A claim that answered pushed its work
// first, and a claim that left the round's pod (supervise's Claim.LeftPodRound) has no work on
// this pod's disk, so neither is named.
func notWaitedFor(group []supervise.Claim, round string, clock roundClock) []supervise.Claim {
	var of []supervise.Claim
	for _, c := range sessionClaims(group) {
		if !roundAsks(c, round, clock) && !c.LeftPodRound(round) && !answered(c, round) {
			of = append(of, c)
		}
	}
	return of
}

// passedOverClaims is the part of notWaitedFor the round passed over at the clock's now
// (roundClock.passesOver): every claim the daemon suspended and the round does not wait for that
// has a session and has neither answered the round nor left it. A round that holds one reads the
// issue's workspace before its pod's replacement (readRoundWorkspace), since no role was asked to
// push what such a claim left there.
func passedOverClaims(group []supervise.Claim, round string, clock roundClock) []supervise.Claim {
	return slices.DeleteFunc(notWaitedFor(group, round, clock), func(c supervise.Claim) bool { return !clock.passesOver(c) })
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

// lostClaims is what the round's pod took with it: every member of the round with a worker that
// neither answered it nor left it having answered. It is the gone-pod report and nothing else
// (podWentWithoutItsRound, endRoundOverAGonePod), which is a different question from whom the
// replacement waits for (waitingClaims): this one is about work that is already gone, and it is
// read from the round's own record of each member rather than from where any claim's process is
// now.
//
// A member that answered pushed its work before the pod went and lost nothing, whether it
// answered in the round's own pod or had already answered when it left: leaving records no
// answer and withdraws none. One that never answered lost whatever it had not pushed. No turn
// and no answer of the process that replaced it changes either reading.
func lostClaims(group []supervise.Claim, round string) []supervise.Claim {
	var lost []supervise.Claim
	for _, c := range sessionClaims(group) {
		if memberOf(c, round) && !answered(c, round) {
			lost = append(lost, c)
		}
	}
	return lost
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

// podProcess is the process the round's pod is ended through: the first claim of the group that
// records a process in that round's own pod. The pod is what the filter is on, not merely a
// recorded process: a claim the sweep relaunched while the round ran records the pod that
// replaced this one, and ending that pod would delete a pod no round covers, which has drifted
// from nothing and which the round's own replacement has just built.
func podProcess(group []supervise.Claim, round string) (runtime.Known, bool) {
	for _, c := range group {
		if inRoundPod(c, round) {
			return runtime.Known{Claim: c.Token, Locator: c.Locator}, true
		}
	}
	return runtime.Known{}, false
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

// roundOpenedFor is what a message on the issue says the round was opened for (podWentMessage,
// workspaceLostMessage): the reasons the round recorded, or that the daemon recorded none.
func roundOpenedFor(reasons string) string {
	if reasons == "" {
		return "The daemon did not record why."
	}
	return "The reasons were: " + reasons + "."
}

// podWentMessage is what the issue is told when its pod went before its round was over: the pod
// was replaced where it stood for a failure of its own, and the disk went with it. The issue's
// spec requires this message. Another agent reads it, so it follows ASD-STE100.
func podWentMessage(round, reasons string, lost []supervise.Claim) string {
	return fmt.Sprintf(`The pod that ran this issue's roles is gone. The daemon opened an update round over that pod (%s). %s The pod went before the round was over.

These roles did not answer the update ask before the pod went: %s. The pod's disk went with the pod. Work that these roles did not push is lost.

The daemon starts each role again, in the same session, on a new pod. Each role must read its issue branch at origin to see the work that it pushed.`,
		round, roundOpenedFor(reasons), roleStates(lost))
}

// workspaceLostMessage is what the issue is told when its round replaced the pod without waiting
// for every role, and the read of the workspace before the replacement found work that no branch
// or tag on origin holds, or failed, or could not be made because no role process ran in the pod
// (tellWorkspaceLost). It names each claim of named with its state — each claim the round did not
// wait for (notWaitedFor), or, for a pod no role process ran in, each member of the round that had
// not answered it (lostClaims) — and what the read found (workspaceRead.found). The issue's spec
// requires this message (dispatch://INFRA-4109 R1, R7). Another agent reads it, so it follows
// ASD-STE100, as podWentMessage does.
func workspaceLostMessage(round, reasons string, named []supervise.Claim, read workspaceRead) string {
	roles := "The round did not wait for these roles, because no process of these roles ran in the pod: "
	if read.noProcess() {
		roles = "These roles did not answer the update ask before the replacement. No process of these roles ran in the pod: "
	}
	return fmt.Sprintf(`The daemon replaced the pod that ran this issue's roles. The daemon opened an update round over that pod (%s). %s The pod and its disk are gone.

%s%s.

%s

Work that is not on a branch at origin is lost. Each role must read its issue branch at origin to see the work that it pushed.`,
		round, roundOpenedFor(reasons), roles, roleStates(named), read.found())
}

// found is what the issue's message says the read found (workspaceLostMessage): that no process
// could make it, the error it failed with, or the change in the working copy and the work commits,
// by id up to shimwire.MaxWorkspaceWorkCommits of them and by count all of them.
func (r workspaceRead) found() string {
	if r.noProcess() {
		return strings.TrimSuffix(noProcessRead, ".") + ", so the daemon did not read it before the replacement. The daemon does not know if the workspace held work that no branch or tag at origin holds."
	}
	if r.err != nil {
		return fmt.Sprintf("Before the replacement, the daemon tried to read the issue workspace. The read failed: %v. The daemon does not know if the workspace held work that no branch or tag at origin holds.", r.err)
	}
	found := []string{"Before the replacement, the daemon read the issue workspace. The read found work that no branch or tag at origin holds."}
	if r.work.Changed {
		found = append(found, fmt.Sprintf("The working copy held a change (commit %s).", r.work.Head))
	}
	commits := strings.Join(r.work.Commits, ", ")
	switch {
	case r.work.Count > len(r.work.Commits):
		found = append(found, fmt.Sprintf("The workspace held %d commits that no branch or tag at origin holds. The first %d of these commits are: %s.",
			r.work.Count, len(r.work.Commits), commits))
	case r.work.Count == 1:
		found = append(found, fmt.Sprintf("The workspace held one commit that no branch or tag at origin holds: %s.", commits))
	case r.work.Count > 1:
		found = append(found, fmt.Sprintf("The workspace held %d commits that no branch or tag at origin holds: %s.", r.work.Count, commits))
	}
	return strings.Join(found, " ")
}

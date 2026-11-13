package daemon

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/rescue"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// reconcilePodUpdates carries every update round of this project one step, over the claims the
// orphan sweep already read this tick. A running issue pod whose configuration has drifted is
// never replaced where it is found: that ends every role process in it mid-turn and deletes the
// pod's disk with everything unpushed on it. The daemon opens a round over the pod instead — each
// role of the issue is asked, on its own Envoy topic, to push its work and answer (openPodUpdate,
// record.UpdateAsk, api's claims/ready-for-update) — and replaces the pod once each has answered,
// or at the round's bound (pod_update_grace_minutes after the round opened, roundBound) whatever
// the roles said, and in both cases only after the daemon's own push check has carried what the
// disk still held off it (replacementDue, checkUnpushed). An answer from every role ends the
// round early; no answer holds it past the bound. An operator's suspension of a claim is the one
// wait that outlasts the bound, and only over a pod that is there: a gone pod ends its round
// whatever the hold (readReplacement).
//
// The round is the issue's, not the locator's: a suspended claim's locator is nil (supervise's
// letGo), and before the bound no pod disk may be deleted before every worker in it, suspended
// ones included, has signalled. Every role claim of one issue shares that issue's one Sandbox
// and pod, so the issue is what groups them. A group whose claims' locators name two different
// pods is left alone this tick: a replacement is already in flight, and the next tick reads it
// settled.
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
// gives the same decision. The one thing this pass keeps of its own is the pace at which it
// opens new rounds: when it last opened one, and the issues it has opened one for whose claims
// have not recorded it yet (podUpdateOpenings). Neither goes in the store, because neither
// decides a round: they pace this daemon's own openings, and a daemon that has just booted
// opens its first round at its first sweep, one sweep interval after the start.
//
// The sweep walks the issues in key order, decides each of them, and only then makes the pod
// replacements those decisions left it, also in key order. A replacement waits its pod out — a
// sweep interval and the push check's own bound at worst (replacePodForRound) — so one made
// where it is decided holds every issue after it in the walk for minutes, and the project's two
// limits (podupdate_limits.go) would open one round per slow sweep rather than the rounds a
// minute the project asks for.
//
// Those two limits are what keeps one pod template change from waking every worker of the fleet
// in one minute: `pod_update_max_open_rounds` caps how many issues may hold an open round at
// once, and `pod_update_rounds_per_minute` how many rounds a sweep may open (podUpdateSweep).
// Neither ends a round, pauses one, or holds back the ask of one: the gate stands only where
// a new round would open, so a change reaches a few pods first and the rest as those finish.
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
	sweep := s.beginPodUpdateSweep(now, byIssue)
	var replacements []podReplacement
	for _, issue := range issues {
		if ctx.Err() != nil {
			return
		}
		if replace := s.reconcileIssuePodUpdate(ctx, issue, byIssue[issue], now, &sweep); replace != nil {
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
		s.replacePodForRound(ctx, replace)
	}
}

// reconcileIssuePodUpdate carries one issue's pod round a step. group is every claim of the issue
// that has not retired, whatever state it is in.
//
// The decisions are taken in this order, and the order is the rule. A pod the group has replaced
// with another went while its round was open, whatever the runtime says of a pod that is not
// there. Then the drift, which is also what says the pod is not there: a round open over a pod
// the runtime no longer has ends at that reading, and a pod that has drifted from nothing and
// has no round open over it needs no replacement, as does one whose round's own pod is already
// gone, which ends that round over the pod that replaced it. A round open over the pod the
// claims still record goes on, drift or no drift: once opened, a round ends only by replacing
// its pod, or because that pod is gone. Only then is a complete round acted on, and only then is
// a round opened or kept open.
//
// A group whose claims record no pod at all decides nothing: not that the pod went, and not that
// it has drifted from nothing. No locator is no evidence either way — every claim of the issue
// may be suspended (letGo clears the locator), or the one live claim may be minutes into a
// relaunch — and the round waits for a claim to record a pod again. The one thing that says a
// pod went is a different pod in its place.
//
// A group whose claims record a pod the runtime no longer has ends the round open over it at
// that sweep, and decides nothing else (groupDrift, podGone, endRoundOverAGonePod). It is one of
// the two readings that say a pod went, the other being a different pod in its place, and the
// round ends on whichever the sweep reaches first.
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
// It reports the one step it leaves the sweep: the replacement of a complete round's pod, which
// the sweep makes after its whole walk over the issues, or nil where there is nothing left to do
// for the issue (reconcilePodUpdates, podReplacement).
func (s *supervision) reconcileIssuePodUpdate(ctx context.Context, issue string, group []supervise.Claim, now time.Time, sweep *podUpdateSweep) *podReplacement {
	slices.SortFunc(group, func(a, b supervise.Claim) int { return strings.Compare(string(a.Token), string(b.Token)) })
	pod, split := groupPod(group)
	if split {
		s.log.Info("supervise: the claims of this issue run in two pods; their update round waits for the replacement in flight",
			"issue", issue)
		return nil
	}
	open := openPodRound(group)
	if pod == "" {
		if open != "" {
			s.log.Info("supervise: no claim of this issue records a pod; its update round waits for one to run again",
				"issue", issue, "round", open)
			s.tellPodlessRound(ctx, issue, group, open, now)
			return nil
		}
		// Nothing can be read of a pod no claim records, so no pod round opens and none ends.
		return nil
	}
	// The pod went without the round: it was replaced where it stood for a failure of its own — a
	// terminal pod, a failed init container, a superseded disk, a lost launcher binding — and its
	// disk went with it while a worker still had work to push. The claims record the pod that
	// replaced it, which is the evidence. A round that lost nothing — every member of it pushed
	// its work before the pod went — is written on no issue, and ends below with the drift or
	// the replacement.
	//
	// What was lost is counted here exactly as the message counts it (lostClaims), so the branch
	// is taken when, and only when, there is a role to name. Counted as the claims the
	// replacement waits for (waitingClaims), it would take this branch for a claim of the issue
	// that is in no round at all — one an earlier sweep's end has already taken out of this
	// round, or a role first started in the pod that replaced this one — and write a message
	// naming either nobody or a worker that lost nothing.
	if open != "" && open != supervise.PodRound(pod) && len(lostClaims(group, open)) > 0 {
		s.podWentWithoutItsRound(ctx, issue, group, open, pod)
		return nil
	}
	reasons, reading := s.groupDrift(ctx, issue, group)
	if reading == podGone {
		s.endRoundOverAGonePod(ctx, issue, group, open, pod)
		return nil
	}
	if reading != podRead {
		return nil
	}
	if len(reasons) == 0 && open != supervise.PodRound(pod) {
		// The round's own pod is gone: the claims record another one in its place, and that pod
		// has drifted from nothing. A round that still had a worker to lose work with the pod
		// was taken by the branch above, so this one lost nothing and ends here, over the pod
		// that replaced its own.
		if open != "" {
			s.log.Info("supervise: the pod this issue's update round was opened over is gone, and the pod in its place has drifted from nothing; the round is over",
				"issue", issue, "round", open, "pod", pod)
			s.endPodUpdates(group)
		}
		return nil
	}
	// The drift this tick read opens a round and decides nothing of a round already open: a
	// round's terms are fixed at its open (dispatch://INFRA-2981 R9), so what an open round's
	// notices and its replacement name is its own recorded reasons (roundReasons). That holds
	// where the drift has gone away too — an operator rolled the release back — and the round
	// replaces the pod all the same. The round has already asked every role in that pod to push
	// its work and to start no new work, and no path is left that lets a role carry on in the pod
	// it was told would be replaced. Replacing it is one restart of each role in its own session,
	// which the alternative — a release of the answered roles that the daemon alone knows about —
	// is not.
	round := supervise.PodRound(pod)
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
		live = s.liveGroup(group)
		if replace, decided := s.completePodUpdate(ctx, issue, group, live, open, pod, roundReasons(group, open), now); decided {
			return replace
		}
		if open == round {
			terms = roundReasons(group, open)
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
	opening := open != round && !sweep.reposts(issue, round)
	if opening && sweep.holds(issue) {
		return nil
	}
	if open == "" {
		live = s.liveGroup(group)
	}
	if s.openPodUpdate(ctx, issue, live, pod, terms) && opening {
		s.recordPodUpdateOpening(sweep, issue, round, now)
	}
	return nil
}

// completePodUpdate decides a round that is over. Before the round's bound that is every claim
// the round asks (askedClaims) having answered and no claim of the group running a turn: waiting
// for no claim to be working keeps the promise that no worker is stopped mid-turn before the
// bound — a worker answers from inside a turn, and that turn ends a moment later. At the bound
// (roundBound) neither an answer nor a turn holds the round any more, and the pod is replaced
// whatever the roles said (dispatch://INFRA-2981 R2). It reports whether it decided the round,
// and with it the pod replacement that decision leaves the sweep to make.
//
// The replacement is handed back rather than made here because it waits its pod out, and the
// sweep makes every one of this tick's replacements after its whole walk over the issues
// (reconcilePodUpdates, replacePodForRound). Everything else this round is owed — the log line,
// the notice of a round that has outlived its bound — is decided here, where the claims the
// sweep walked are.
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
// Even that read is older than the act by the time the act is made, because the replacement
// runs after the walk. The replacement reads the claims once more immediately before it calls
// the runtime, and carries the daemon's own last word into the call behind that
// (replacePodForRound, replacementDue).
//
// A round no live claim records a process in the pod of replaces nothing, and ends on nothing
// the claims say: the runtime is asked about the pod itself, and only a pod it answers gone ends
// the round (roundWithNoProcessInItsPod). Once opened, a round ends by replacing its pod or
// because that pod is gone, and no locator is no evidence either way. That is asked before an
// operator's suspension is weighed: a gone pod ends its round whatever the hold, and the hold
// keeps its place wherever the pod is there.
//
// Three readings hold a round past its bound, and each is told to the operator's side once, at
// the bound, naming the reading (tellLateRound): an operator's suspension, no process in the
// round's pod, and two pods at once. A round that waits for an answer or a turn before its bound
// tells nobody: the bound ends that wait by itself.
func (s *supervision) completePodUpdate(ctx context.Context, issue string, snapshot, live []supervise.Claim, round, pod, reasons string, now time.Time) (*podReplacement, bool) {
	reading := readReplacement(live, round, roundBound(live, round, s.cfg.PodUpdateGrace), now)
	switch reading.wait {
	case waitsForOperator:
		s.waitForOperator(ctx, issue, live, round, pod, reasons, now, reading.held)
		// The round is not decided: the post below keeps it recorded on every claim, and asks
		// a claim it has not asked yet, as it does for a round that waits for an answer.
		return nil, false
	case waitsForAnswer:
		if len(waitingClaims(snapshot, round)) == 0 {
			s.log.Info("supervise: every worker of this update round had answered in the sweep's snapshot, and the fresh read finds one unanswered; the replacement waits for the next sweep or for the round's bound",
				"issue", issue, "round", round, "waiting", namesOf(reading.waiting))
		}
		return nil, false
	case waitsForProcessInItsPod:
		return nil, s.roundWithNoProcessInItsPod(ctx, issue, snapshot, live, round, pod, reasons, now, reading.held)
	case waitsForTurn:
		s.log.Info("supervise: every worker of this update round has answered; the replacement waits for the turns still running, or for the round's bound",
			"issue", issue, "round", round, "working", reading.working)
		return nil, true
	case waitsForPodInFlight:
		s.log.Info("supervise: the claims of this issue run in two pods now; this round's replacement waits for the one in flight",
			"issue", issue, "round", round, "pod", pod)
		s.tellLateRound(ctx, issue, live, round, pod, reasons, now, twoPodsReason)
		return nil, true
	case replacementReady:
		return &podReplacement{issue: issue, round: round, pod: pod, reasons: reasons}, true
	default:
		s.log.Error("supervise: this update round's replacement was held back by a reading this sweep cannot name; nothing is replaced and the round stays open for the next sweep",
			"issue", issue, "round", round, "pod", pod, "reading", int(reading.wait))
		return nil, true
	}
}

// replacementWait is the one reading that holds a round's pod replacement back, and
// replacementReady where none of the five does. The zero value is a wait rather than the
// go-ahead, because what these readings decide is the deletion of a pod's disk: a reading nobody
// took may hold a replacement for a sweep, and may not make one.
//
// The same rule is what both switches over it are written to (completePodUpdate,
// replacePodForRound): replacementReady is the one case that reaches the replacement, and every
// other value waits, the ones below named each with the line its caller logs and whatever else
// it decides, and anything else in a default that logs and replaces nothing. A sixth reading
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
	// holds, if any: that hold is read only once the pod is known to be there
	// (roundWithNoProcessInItsPod).
	waitsForProcessInItsPod
	// waitsForTurn is a worker of the issue running a turn (workingClaims), read only before the
	// round's bound.
	waitsForTurn
	// waitsForPodInFlight is the claims recording two pods (groupPod), which is a replacement
	// already in flight, read whatever the clock says.
	waitsForPodInFlight
	// replacementReady is every reading passed: the round's pod may be replaced, through the
	// process the reading carries.
	replacementReady
)

// replacementReading is what the five readings answered over one live group: the process the
// round's pod is replaced through, or the first reading that holds the replacement back with
// what its caller has to name of it — the claims that have not answered, the claims an operator
// holds (for waitsForOperator, and for waitsForProcessInItsPod where a hold was read), or the
// roles whose turns run. Nothing but the fields the wait names is set, and process is set for
// replacementReady alone.
type replacementReading struct {
	wait    replacementWait
	process runtime.Known
	waiting []supervise.Claim
	held    []supervise.Claim
	working []string
}

// roundEnd is what a round's end reads of its claims: whether the round's bound has come, the
// claims the round asks that answered it and those that did not, and the roles whose turns run.
// It is what the line of a replacement the bound made names (replacePodForRound).
type roundEnd struct {
	past       bool
	answered   []supervise.Claim
	unanswered []supervise.Claim
	working    []string
}

// holding is what holds the round's pod replacement at this reading of the round's end: before
// the round's bound, the claims the round asks that have not answered it and the roles whose
// turns run, and past the bound nothing. It is the one statement of that rule
// (dispatch://INFRA-2981 R2). The walk's reading (readReplacement) and the last word under the
// pod's launch turn (replacementDue) read it here. The line of a replacement the bound made
// (bounded) reads the same two fields, unanswered and working, past the bound.
func (e roundEnd) holding() (unanswered []supervise.Claim, working []string) {
	if e.past {
		return nil, nil
	}
	return e.unanswered, e.working
}

// bounded is whether the bound is what let the replacement through: the bound has come, and the
// same reading taken before the bound would have held the replacement (holding).
func (e roundEnd) bounded() bool {
	return e.past && (len(e.unanswered) > 0 || len(e.working) > 0)
}

// readRoundEnd is the round's end as group reads at now, under the round's bound (roundBound).
func readRoundEnd(group []supervise.Claim, round string, bound *time.Time, now time.Time) roundEnd {
	return roundEnd{
		past:       pastBound(bound, now),
		answered:   answeredClaims(group, round),
		unanswered: waitingClaims(group, round),
		working:    workingClaims(group),
	}
}

// roundBound is the moment the round ends whatever its roles say: its opening, the earliest its
// claims record (roundOpenedAt), plus pod_update_grace_minutes. It is nil where no claim of group
// records the opening, which no round's end is read past.
func roundBound(group []supervise.Claim, round string, grace time.Duration) *time.Time {
	opened := roundOpenedAt(group, round)
	if opened == nil {
		return nil
	}
	bound := opened.Add(grace)
	return &bound
}

// pastBound is whether now is at or past bound (roundBound). No bound — no claim of the group
// records when the round opened — is a bound that has not come.
func pastBound(bound *time.Time, now time.Time) bool {
	return bound != nil && !now.Before(*bound)
}

// readReplacement takes the five readings that hold a round's pod replacement back over one live
// group, at now and under the round's bound, and answers either the process the round's pod is
// replaced through or the first reading that holds it. Both the walk that finds a round complete
// (completePodUpdate) and the replacement that walk leaves for afterwards (replacePodForRound)
// read it over claims each of them read live, so when a replacement waits is decided in one
// place; what either of them does with a wait — the line it logs, the notice it writes, whether
// it reports the round decided — is its own. It reads the claims and nothing else, so neither
// reading costs a call to the runtime or the store.
//
// The order the readings are taken in is the rule. An operator's suspension comes first, because
// it is the one wait that is a person's and the one the bound never steps over
// (dispatch://INFRA-2981 R5): a person's hold does not yield to a clock. It holds a pod that is
// there, and only that: a hold over a round no live claim records a process in the pod of is
// read as no process in the round's pod, carrying the claims the operator holds, so that its
// caller asks first whether the pod is there at all. A round whose pod is gone ends whatever
// the operator's suspension says (endRoundOverAGonePod): there is no pod left to replace, and a
// wait for the resume of a claim on a pod that no longer exists is a wait for nothing. A claim
// that has not answered and a turn that runs hold the replacement only before the bound
// (roundEnd.holding), which is what makes the bound the end of every round nobody answers (R2).
// No process in the round's pod and two pods at once are facts about the pod, not about any
// answer, so they hold the replacement whatever the clock says: there is nothing in such a pod
// to replace it through, or a replacement is already in flight.
//
// Gone comes before the turn: whether this round's pod is still there is asked before the round
// waits on a turn or on a replacement in flight, because each of those is a wait on that pod.
// The wait for an answer above it belongs to no pod: a round nobody has answered has lost nothing
// to tell anyone of, whether its pod is there or not. A round whose pod no live claim records has
// nothing in that pod to cut and nothing there to replace, so waiting for a turn — which by then
// is a turn in the pod that replaced this one — would hold every answered worker's next task, and
// the notice would name a pod that is gone. What is left to decide of such a round is read from
// the round's own record of its members (roundWithNoProcessInItsPod, lostClaims).
func readReplacement(live []supervise.Claim, round string, bound *time.Time, now time.Time) replacementReading {
	process, running := podProcess(live, round)
	if held := heldByOperator(live, round); len(held) > 0 {
		if !running {
			return replacementReading{wait: waitsForProcessInItsPod, held: held}
		}
		return replacementReading{wait: waitsForOperator, held: held}
	}
	unanswered, working := readRoundEnd(live, round, bound, now).holding()
	if len(unanswered) > 0 {
		return replacementReading{wait: waitsForAnswer, waiting: unanswered}
	}
	if !running {
		return replacementReading{wait: waitsForProcessInItsPod}
	}
	if len(working) > 0 {
		return replacementReading{wait: waitsForTurn, working: working}
	}
	if _, split := groupPod(live); split {
		return replacementReading{wait: waitsForPodInFlight}
	}
	return replacementReading{wait: replacementReady, process: process}
}

// podReplacement is the one step the sweep's walk over the issues leaves for after the walk: the
// pod of a round that is over is to be replaced (completePodUpdate). It carries what the step
// cannot read again — the issue, the round, the pod the round was opened over and the reasons
// the round records — and nothing of the claims or the clock, which the step reads afresh.
type podReplacement struct {
	issue   string
	round   string
	pod     string
	reasons string
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
// same sweep decided before this one has run since, each of them a pod boot long. The five
// readings that hold a replacement back are taken again over that read and at the clock as it
// stands now, by the one predicate the walk itself asked (readReplacement), and each of them
// gives this replacement up: an operator's suspension, a claim of the round that has not
// answered before the bound, no live claim recording a process in the round's own pod, a turn
// running in the issue before the bound, and claims recording two pods. None of them ends
// anything: the round stays open, and the next sweep decides it afresh from claims it reads
// then. An operator's suspension and two pods at once are not stepped over by the bound, so each
// is told to the operator's side once the bound has come (tellLateRound), as the walk tells
// them. No process in the round's pod is told by the walk alone, which asks the runtime first
// whether the pod is there at all (roundWithNoProcessInItsPod), and that includes a round an
// operator's suspension holds with no live process in its pod: the hold is weighed only once
// that read finds the pod there, and a pod it finds gone ends the round whatever the hold.
//
// That read is no replacement for the daemon's last word inside the runtime (replacementDue),
// which it stands in front of rather than in place of: ReplacePod takes the pod's launch turn
// and waits it out, so a round complete at this read is still a round a worker can start a turn
// in, or an operator can suspend a claim of, while the call waits. The runtime asks that last
// word one statement before the write that ends every process in the pod, and a replacement no
// longer due is refused there, ends nothing and leaves the round open for the next sweep. The
// push check of the pod's disk runs there too, last (checkUnpushed): a check that fails refuses
// the replacement the same way, and is told to the operator's side once the bound has come.
//
// The last word is asked only of a pod that is there. The Sandbox runtime asks it after it has
// found the pod the locator records still the Sandbox's own, and answers a pod that is gone by
// replacing nothing and returning nil, which ends the round here as a replacement made does: no
// operator's hold is read over a pod that is not there.
//
// ReplacePod is called once, with the process of a claim that records this round's own pod. A
// failed ReplacePod is logged and retried at the next sweep: nothing is ended, so the round
// still holds every answered worker's next task. A push check that keeps failing and a ReplacePod
// that keeps failing each hold the round past its bound, which only a person can end, so each is
// told to the operator's side once the bound has come (tellLateRound) and names what holds the
// round. One round is told once.
//
// A replacement made is logged on one line that holds for a round that ended either way, by its
// answers or by its bound. One that the bound made — a claim had not answered, or a turn ran,
// when the last word let it through — is logged on a second line of its own, `supervise pod
// update replaced at the bound`, naming the claims that answered and those that did not
// (dispatch://INFRA-2981 R8, whose production check reads those words). That line is written
// only where the last word ran and let the replacement through. A runtime that found no pod to
// replace and returned nil without asking the last word replaced nothing, so the bound
// replaced nothing either.
//
// The call is bounded to one sweep interval and the push check's own bound (rescueTimeout, and
// the margin its answer crosses the stream in). That bound costs the round nothing. The
// suspension that displaces the pod has already landed when it hits, so the next sweep reads the
// Sandbox suspended and naming that very pod as the one it displaced, which is this round's own
// replacement in flight: PodDrift answers for it as it does for a pod that is there and has not
// drifted, and the round is finished at that sweep, where only the pod's end is left to wait
// for. Once the pod itself is gone, that sweep reads it gone and the round ends there instead,
// over a pod whose disk the push check has already read, so nothing is deleted earlier than it
// would have been.
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
	now := s.plan.clock.Now()
	reading := readReplacement(live, round, roundBound(live, round, s.cfg.PodUpdateGrace), now)
	switch reading.wait {
	case waitsForOperator:
		s.log.Info("supervise: an operator's suspension holds this update round now the sweep has reached its replacement; nothing was replaced and the round stays open",
			"issue", issue, "round", round, "pod", pod, "held", namesOf(reading.held))
		s.tellLateRound(ctx, issue, live, round, pod, reasons, now, operatorHoldReason(reading.held))
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
	case waitsForPodInFlight:
		s.log.Info("supervise: the claims of this issue run in two pods now the sweep has reached this update round's replacement; nothing was replaced and the round stays open",
			"issue", issue, "round", round, "pod", pod)
		s.tellLateRound(ctx, issue, live, round, pod, reasons, now, twoPodsReason)
		return
	case replacementReady:
		// The replacement below is this one reading's; every other one has returned by here.
	default:
		s.log.Error("supervise: this update round's replacement was held back by a reading this sweep cannot name; nothing was replaced and the round stays open",
			"issue", issue, "round", round, "pod", pod, "reading", int(reading.wait))
		return
	}
	// The limit is a sweep interval plus the push check's own bound (rescueTimeout, and the margin
	// its answer crosses the stream in), which runs under the pod's launch turn just before the
	// write that ends the pod (replacementDue).
	limited, done := context.WithTimeout(ctx, s.plan.orphanSweep+rescueTimeout+rescueAnswerMargin)
	defer done()
	var checked pushCheck
	// ended is what the last word read of the round's end under the launch turn, which is the
	// read the replacement is made on. It stays the zero roundEnd, which no bound made, where the
	// runtime returns nil without asking the last word: the Sandbox runtime does that for a
	// Sandbox that is absent or suspended, or whose pod is no longer the locator's, and replaces
	// nothing there.
	var ended roundEnd
	if err := s.runtime.ReplacePod(limited, reading.process, s.replacementDue(issue, round, pod, &checked, &ended)); err != nil {
		if errors.Is(err, errReplacementNotDue) {
			s.log.Info("supervise: this update round's replacement was no longer due when its pod's launch turn came; nothing was replaced and the round stays open",
				"issue", issue, "round", round, "pod", pod, "reason", err)
			return
		}
		if errors.Is(err, errPushCheckFailed) {
			s.log.Error("supervise: the push check before this update round's replacement failed; nothing was replaced and the round stays open",
				"issue", issue, "round", round, "pod", pod, "rescued", checked.rescued(), "error", err)
			s.tellLateRound(ctx, issue, live, round, pod, reasons, now,
				fmt.Sprintf(boundPastReason+"The check for unpushed work before the replacement failed, so the daemon did not replace the pod: %v", err))
			return
		}
		if ctx.Err() == nil {
			s.log.Error("supervise: replace the pod of a finished update round; retrying at the next sweep",
				"issue", issue, "round", round, "pod", pod, "error", err)
			s.tellLateRound(ctx, issue, live, round, pod, reasons, now,
				fmt.Sprintf(boundPastReason+"The runtime refused the pod replacement: %v", err))
		}
		return
	}
	// A failed claim was not waited for (askedClaims): the push check is what carried whatever it
	// left unpushed on the disk off the pod, and the line names it beside what the check pushed.
	failed := slices.DeleteFunc(blockingClaims(live), func(c supervise.Claim) bool { return c.State != supervise.StateFailed })
	bounded := ended.bounded()
	s.log.Info("supervise: the pod of this update round is replaced and the round is over",
		"issue", issue, "round", round, "pod", pod, "atTheBound", bounded, "failedNotWaitedFor", namesOf(failed),
		"rescued", checked.rescued(), "unchecked", checked.unchecked)
	if bounded {
		s.log.Info("supervise pod update replaced at the bound",
			"issue", issue, "round", round, "pod", pod,
			"answered", namesOf(ended.answered), "unanswered", namesOf(ended.unanswered), "working", ended.working)
	}
	if post := rescueMessage(issue, round, checked); post.Body != "" {
		if err := s.writeOnIssue(ctx, issue, post); err != nil && ctx.Err() == nil {
			s.log.Error("supervise: write on the issue what the push check before its pod's replacement found",
				"issue", issue, "round", round, "rescued", checked.rescued(), "unchecked", checked.unchecked, "error", err)
		}
	}
	s.endPodUpdates(live)
}

// boundPastReason opens every reason the late-round notice gives (tellLateRound,
// tellPodlessRound): the notice is told only once the round's bound has come.
const boundPastReason = "The bound of the update round is past. "

// The reasons the late-round notice gives for the two readings that hold a round past its bound
// and name no claim of their own (tellLateRound). The controller reads them, so they follow
// ASD-STE100.
const (
	noProcessReason = boundPastReason + "No claim of the issue records a process in the pod of the round. The daemon has no process to replace the pod through."
	twoPodsReason   = boundPastReason + "The claims of the issue record two pods. A replacement is in flight, and the round waits for it."
)

// operatorHoldReason is the reason the late-round notice gives for a round an operator's
// suspension holds past its bound, naming each claim the suspension holds.
func operatorHoldReason(held []supervise.Claim) string {
	return fmt.Sprintf(boundPastReason+"An operator suspended %s. The daemon does not replace the pod until the operator resumes each of them.",
		roleStates(held))
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
// records it on, and only runtime.ErrPodGone ends the round, as every reading of a gone pod ends
// one (endRoundOverAGonePod). What the pod has drifted to says nothing here: the round is open,
// and its recorded reasons are what its ask and its notice state.
//
// Any other answer, a read that could not be made included, leaves the round where it stands for
// the next sweep to decide from claims it reads afresh: nothing is ended, and nothing is
// replaced. A pod that is there with no claim recording a process in it holds the round past its
// bound, so the operator's side is told of it once the bound has come (tellLateRound).
//
// held is the claims of the round an operator's suspension holds (readReplacement), and it is
// weighed here, after the pod is read, never before. A gone pod ends its round whatever the
// hold: the operator's suspension holds a pod that is there, and there is no pod left to wait
// for the resume of. A pod that is there keeps the hold in its place: the round waits for the
// operator as waitsForOperator's does (waitForOperator), the notice names the suspension rather
// than the missing process, and the round is not decided, so the post keeps it recorded and asks
// a claim it has not asked yet. A read that could not be made leaves such a round undecided too,
// and tells nobody of a hold over a pod it could not read.
//
// The snapshot recording no process in that pod either is the claims recording another pod in
// its place, which is the one thing that says this pod went (reconcileIssuePodUpdate): no pod of
// the round is left to ask about, and the round ends over it with the report its answers decide,
// whatever the hold.
//
// It reports whether it decided the round (completePodUpdate).
func (s *supervision) roundWithNoProcessInItsPod(ctx context.Context, issue string, snapshot, live []supervise.Claim, round, pod, reasons string, now time.Time, held []supervise.Claim) bool {
	recorded, ok := podProcess(snapshot, round)
	if !ok {
		s.endRoundOverAGonePod(ctx, issue, snapshot, round, pod)
		return true
	}
	switch _, err := s.runtime.PodDrift(ctx, recorded); {
	case errors.Is(err, runtime.ErrPodGone):
		s.endRoundOverAGonePod(ctx, issue, snapshot, round, pod)
		return true
	case err != nil:
		if ctx.Err() == nil {
			s.log.Error("supervise: read whether the pod of an update round is still there; the round stays open and the next sweep reads it again",
				"issue", issue, "round", round, "pod", pod, "error", err)
		}
	case len(held) > 0:
		s.waitForOperator(ctx, issue, live, round, pod, reasons, now, held)
	default:
		s.log.Info("supervise: no claim of this update round records a process in its pod now; the pod is there, and the round waits for a claim to record it again",
			"issue", issue, "round", round, "pod", pod)
		s.tellLateRound(ctx, issue, live, round, pod, reasons, now, noProcessReason)
	}
	return len(held) == 0
}

// waitForOperator is the walk's part of a round an operator's suspension holds over a pod that
// is there (completePodUpdate's waitsForOperator, and roundWithNoProcessInItsPod once it has read
// the pod there): the line, and the notice naming the suspension once the bound has come. The
// replacement waits for the operator's resume, whatever the round's bound.
func (s *supervision) waitForOperator(ctx context.Context, issue string, live []supervise.Claim, round, pod, reasons string, now time.Time, held []supervise.Claim) {
	s.log.Info("supervise: an operator's suspension holds this update round; the replacement waits for the operator's resume, whatever the round's bound",
		"issue", issue, "round", round, "pod", pod, "held", namesOf(held))
	s.tellLateRound(ctx, issue, live, round, pod, reasons, now, operatorHoldReason(held))
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
// refuses the replacement when one of three readings holds it: an operator's suspension of a
// claim the round asks (heldByOperator), whatever the clock says; and, only before the round's
// bound, a claim the round asks that has not answered or a claim of the issue that runs a turn
// (roundEnd.holding). It does not call readReplacement: it takes the three of that predicate's
// readings that are about the claims, through the same helpers, because the runtime asks it only
// of a pod it found there. What it read of the round's end is left in ended, which the
// replacement's log lines name. Then it runs the push check of the pod's disk (checkUnpushed),
// which checked records, and refuses the replacement when that check fails. The bound steps over
// none of that check (dispatch://INFRA-2981 R3). The Sandbox runtime asks this only of a pod that
// is there (replacePodForRound), so the operator's hold it reads first is a hold over a pod there
// is to replace.
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
// daemon holds.
//
// The push check is here, after every reading and under the pod's launch turn, because nothing
// else guards what is on the disk at the moment it is deleted. A worker's answer says it pushed
// when it answered, and the answer stands for the round through any turn after it; a failed
// claim is asked nothing; and at the bound a worker may not have answered at all. Checked any
// earlier, a turn of any of these could leave work behind after the check. A pod whose shim
// cannot run the check at all is replaced unchecked, and the issue is told so (checkUnpushed).
func (s *supervision) replacementDue(issue, round, pod string, checked *pushCheck, ended *roundEnd) func(context.Context) error {
	return func(ctx context.Context) error {
		claims, err := s.supervisor.Claims(ctx)
		if err != nil {
			return fmt.Errorf("%w: the claims of %s cannot be read: %w", errReplacementNotDue, issue, err)
		}
		group := s.viewedGroup(claims, issue)
		if held := heldByOperator(group, round); len(held) > 0 {
			return fmt.Errorf("%w: an operator's suspension holds %s", errReplacementNotDue, strings.Join(namesOf(held), ", "))
		}
		*ended = readRoundEnd(group, round, roundBound(group, round, s.cfg.PodUpdateGrace), s.plan.clock.Now())
		unanswered, working := ended.holding()
		if len(unanswered) > 0 {
			return fmt.Errorf("%w: %s has not answered it", errReplacementNotDue, strings.Join(namesOf(unanswered), ", "))
		}
		if len(working) > 0 {
			return fmt.Errorf("%w: the turn of %s runs now", errReplacementNotDue, strings.Join(working, ", "))
		}
		return s.checkUnpushed(ctx, issue, round, pod, group, checked)
	}
}

// The push check's bounds: one workspace's check and push, and the whole check of a pod's disk.
// Both are internal/rescue's own, derived there from the reserve the guard leaves the push
// (rescue.GuardReserve, which holds the second run of the shared check a pod's pre-push hook
// makes) — so a reserve that grows moves them, which a literal here would not. The whole bound
// is what replacePodForRound adds to the replacement's own, since the check runs inside it.
// rescueAnswerMargin is what the shim's bound leaves of the time the replacement has left, for
// the answer to cross the stream: a check bounded at the whole of that time would be read as a
// failure the moment it finished, and would run on in the pod beside the next one.
const (
	rescueWorkspaceTimeout = rescue.WorkspaceTimeout
	rescueTimeout          = rescue.Timeout
	rescueAnswerMargin     = 30 * time.Second
)

// errPushCheckFailed is a push check that could not show every workspace of the pod safe: a
// workspace whose check or push failed, or no process in the pod that could run the check. The
// runtime hands it back from inside ReplacePod having written nothing, and replacePodForRound
// holds the round on it and tells the operator's side through the late-round notice.
var errPushCheckFailed = errors.New("the push check of the pod's workspaces failed")

// pushCheck is what the push check before a replacement found: the role it ran as, each
// workspace's result, and, for a pod whose shim predates the check, why nothing was checked.
type pushCheck struct {
	role       claim.Role
	workspaces []runtime.WorkspaceRescue
	unchecked  string
}

// rescued is every rescue branch the check reports, as `<branch>@<commit> in <owner>/<repo>`, each
// once for each repository (branches).
func (p pushCheck) rescued() []string {
	var named []string
	for _, b := range p.branches() {
		named = append(named, b.Name+"@"+b.Commit+" in "+b.repository)
	}
	return named
}

// rescueBranch is the prefix of every rescue branch of a pod's replacement:
// `legion/<KEY>-rescue-<first 8 of the pod's uid>`, which a shim that predates the issue's one
// rescue branch (rescue.Target) numbers from 1. It is no issue's branch (claim.BranchIssue), so
// nothing the daemon keeps follows it. The request names the issue's one branch in a field of its
// own, beside this prefix rather than in its place: an older shim reports every remote branch that
// starts with `<branch>-` as its own, so the one branch's name there would have it report every
// earlier pod's rescue branches (shimwire.RescueUnpushed).
func rescueBranch(issue, pod string) string {
	return rescue.Target(issue) + "-" + pod[:min(8, len(pod))]
}

// checkUnpushed runs the push check of the round's pod (runtime.Runtime.RescueUnpushed) through a
// process of group that runs in it, and logs one line per workspace: clean, rescued with each
// branch and commit, or the error. It prefers a role that acts as the implement App, which holds
// the push rights on the issue's repository, and asks the next process when one cannot be asked
// at all. Any workspace whose check failed, and a pod no process of which could be asked, is
// errPushCheckFailed naming each claim, workspace and error.
//
// It runs before every replacement of a round's pod (replacementDue). The round asks each role,
// and replaces the pod once every asked role has answered and no turn runs, or at the round's
// bound (pod_update_grace_minutes) whatever the roles answered, an operator's suspension
// excepted. So the check, and not any answer, is what guards the disk (dispatch://INFRA-2981 R3).
//
// A pod whose shim predates the check (runtime.ErrRescueUnsupported) holds nothing: every pod
// started from an image before this check has such a shim, and refusing them all would hold
// every such pod until its tree ends. The pod is replaced unchecked, and the line and the issue's
// message say so (checked's unchecked).
func (s *supervision) checkUnpushed(ctx context.Context, issue, round, pod string, group []supervise.Claim, checked *pushCheck) error {
	timeout := rescueTimeout
	if deadline, ok := ctx.Deadline(); ok {
		timeout = min(timeout, time.Until(deadline)-rescueAnswerMargin)
	}
	if timeout <= 0 {
		return fmt.Errorf("%w: the replacement has no time left to run it in", errPushCheckFailed)
	}
	request := runtime.RescueRequest{Branch: rescueBranch(issue, pod), Target: rescue.Target(issue),
		WorkspaceTimeout: min(rescueWorkspaceTimeout, timeout), Timeout: timeout}
	var refused []string
	for _, c := range rescuers(group, round) {
		report, err := s.runtime.RescueUnpushed(ctx, runtime.Known{Claim: c.Token, Locator: c.Locator}, request)
		if errors.Is(err, runtime.ErrRescueUnsupported) {
			*checked = pushCheck{role: c.Role, unchecked: "the pod's shim predates the push check"}
			s.log.Warn("supervise: the pod's shim predates the push check; its workspaces are not checked before the replacement",
				"issue", issue, "round", round, "pod", pod, "claim", c.Token, "error", err)
			return nil
		}
		if err != nil {
			refused = append(refused, fmt.Sprintf("claim %s (%s): %v", c.Token, c.Role, err))
			continue
		}
		*checked = pushCheck{role: c.Role, workspaces: report}
		var failures []string
		for _, w := range report {
			switch w.Status {
			case runtime.RescueClean:
				s.log.Info("supervise: push check of a workspace before its pod is replaced: clean",
					"issue", issue, "round", round, "pod", pod, "claim", c.Token, "workspace", w.Dir)
			case runtime.RescueRescued:
				s.log.Info("supervise: push check of a workspace before its pod is replaced: rescued",
					"issue", issue, "round", round, "pod", pod, "claim", c.Token, "workspace", w.Dir,
					"rescued", pushCheck{workspaces: []runtime.WorkspaceRescue{w}}.rescued())
			default:
				s.log.Error("supervise: push check of a workspace before its pod is replaced: error",
					"issue", issue, "round", round, "pod", pod, "claim", c.Token, "workspace", w.Dir, "error", w.Error)
				failures = append(failures, fmt.Sprintf("claim %s (%s), workspace %s: %s", c.Token, c.Role, w.Dir, w.Error))
			}
		}
		if len(failures) > 0 {
			if rescued := checked.rescued(); len(rescued) > 0 {
				failures = append(failures, "rescued before the failure: "+strings.Join(rescued, ", "))
			}
			return fmt.Errorf("%w: %s", errPushCheckFailed, strings.Join(failures, "; "))
		}
		return nil
	}
	if len(refused) == 0 {
		return fmt.Errorf("%w: no claim of %s runs a process in pod %s to run the check through", errPushCheckFailed, issue, pod)
	}
	return fmt.Errorf("%w: %s", errPushCheckFailed, strings.Join(refused, "; "))
}

// rescuers is every claim of group with a process in the round's own pod, in the order the push
// check asks them: the roles that act as the implement App first, then the others, each in the
// group's order.
func rescuers(group []supervise.Claim, round string) []supervise.Claim {
	var implement, review []supervise.Claim
	for _, c := range group {
		if c.Locator == nil || supervise.PodRound(podOf(c.Locator)) != round {
			continue
		}
		if appauth.AppRoleFor(c.Role) == appauth.Implement {
			implement = append(implement, c)
		} else {
			review = append(review, c)
		}
	}
	return append(implement, review...)
}

// viewedGroup is every claim of the issue a round weighs, as the store names them and as each
// machine last published it: the store says which claims there are, and roundGroup says what
// each holds now, with the answer of a claim whose machine is deciding masked, as the decision
// the replacement rests on needs it.
func (s *supervision) viewedGroup(claims []supervise.Claim, issue string) []supervise.Claim {
	var of []supervise.Claim
	for _, c := range claims {
		if c.Issue == issue {
			of = append(of, c)
		}
	}
	return s.roundGroup(of, true)
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
// each role, and replaces the pod once every asked role has answered and no turn runs, or at the
// round's bound (pod_update_grace_minutes) whatever the roles answered, an operator's suspension
// excepted; the push check runs before every replacement (checkUnpushed).
//
// What it reads is each machine's published view (Machine.View), never Machine.Claim: Claim
// waits out the decision in flight, and that decision can be a Sandbox relaunch that holds the
// machine for minutes. This runs for every drifted issue on every tick, so one relaunching claim
// would otherwise stall the whole sweep behind it — the orphan reaping, the stale-plugin posts
// and every later issue's round. The view is at least as fresh as the store for a claim a
// machine holds, and the store's row stands for a claim no machine of this daemon holds.
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
// suspension, no process in the round's pod, two pods at once — and the two acts of the
// replacement itself that hold it, a push check that failed and a replacement the runtime
// refused. why is the reason the controller reads, naming that reading. The issue is written
// nothing: there is no instruction for its roles here, and the operator reads the round through
// the controller's notice.
//
// The bound runs from the round's opening and from nothing else, so one round is told at one
// moment whatever its roles did before it: an answer moves the round's end earlier and never its
// notice later.
//
// The notice lists every claim the round asks (askedClaims), each with whether it answered
// (record.UpdateWaiting.Answered), read unmasked (noticeGroup): past the bound no answer holds
// the round, so the list says what each role said rather than whom the round waits for. It is
// never empty while the round asks a claim, which the store requires of the notice
// (record.UpdateStalled). A round that asks no claim at all is told nothing: there is no role to
// name. A round that group already records as told is told nothing again and read no further.
func (s *supervision) tellLateRound(ctx context.Context, issue string, group []supervise.Claim, round, pod, reasons string, now time.Time, why string) {
	if !pastBound(roundBound(group, round, s.cfg.PodUpdateGrace), now) || roundTold(group, round) {
		return
	}
	told := s.noticeGroup(group)
	waiting := waitingOf(round, askedClaims(told))
	if len(waiting) == 0 {
		return
	}
	s.tellUpdateStalled(ctx, updateStall{
		issue: issue, round: round, pod: pod, reasons: reasons, group: told,
		waiting: waiting,
		reason:  why,
	})
}

// tellPodlessRound tells the operator's side of a round whose issue records no pod at all, once
// the round's bound has come (roundBound), the same clock as every other late-round notice
// (tellLateRound). Nothing of such a round can move: no drift can be read, no pod can be
// replaced, and it waits for one of the issue's workers to run again. That is the reading of a
// claim that records no process in the round's pod, and the notice names it so. Where an
// operator's suspension holds a claim of the round (heldByOperator) — a lone worker an operator
// suspended is the common shape, since a suspension lets the process go (letGo clears the
// locator) — only that operator can end the wait, and the notice names the suspension instead.
// No other path here would ever tell them: this round reaches no replacement decision.
//
// A round every claim of which has answered, with no claim an operator holds, is told nothing,
// and that is the ordinary end of a phase rather than a hold: the workers answered, the daemon
// suspended each of them, and letGo took the last locator with the last process. Nobody has to
// do anything about such a round — it ends once a role of the issue runs in a pod again — so
// telling the controller only opens a finding ask over a round that needs no one.
//
// The sweep's snapshot decides first whether the bound has come and whether the round was told,
// so a round before its bound costs no read of the machines. The rest is tellLateRound's, with no
// pod to name.
func (s *supervision) tellPodlessRound(ctx context.Context, issue string, group []supervise.Claim, round string, now time.Time) {
	if !pastBound(roundBound(group, round, s.cfg.PodUpdateGrace), now) || roundTold(group, round) {
		return
	}
	told := s.noticeGroup(group)
	held := heldByOperator(told, round)
	if len(held) == 0 && len(waitingClaims(told, round)) == 0 {
		return
	}
	why := boundPastReason + "No claim of this issue records a process in a pod. The round waits for a claim of the issue to run again."
	if len(held) > 0 {
		why = fmt.Sprintf(boundPastReason+"No claim of this issue records a process in a pod. An operator suspended %s. The round waits for the operator to resume a claim.",
			roleStates(held))
	}
	s.tellLateRound(ctx, issue, group, round, "", roundReasons(group, round), now, why)
}

// podWentWithoutItsRound is the round's pod gone before the round could finish: the message the
// issue's spec requires is written on the issue, naming what was lost, and the round ends.
// Nothing here can bring the work back — the disk went with the pod — so the message is the whole
// of it.
//
// Two readings say the pod went, and each calls this with the claims it decided from. The claims
// recording a pod other than the round's own is one (reconcileIssuePodUpdate), and the runtime
// answering that the pod is not there is the other (endRoundOverAGonePod). What was lost is read
// from the group this is given, so the caller's own reading of the claims is the one the issue
// is told of and the one the round ends on.
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
	if err := s.writeOnIssue(ctx, issue, record.MessagePost{Body: podWentMessage(round, reasons, lost)}); err != nil {
		s.log.Error("supervise: write on the issue that its pod went without its update round",
			"issue", issue, "round", round, "error", err)
	}
	s.endPodUpdates(group)
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
// record no pod at all does: there is no round to end, the drift of a pod that is not there says
// nothing, and the next launch into the issue builds a pod from the configuration the runtime
// has now.
func (s *supervision) endRoundOverAGonePod(ctx context.Context, issue string, group []supervise.Claim, round, pod string) {
	if round == "" {
		s.log.Info("supervise: the pod this issue's claims record is not there, and no update round is open over it; nothing is decided of this issue",
			"issue", issue, "pod", pod)
		return
	}
	told := s.noticeGroup(group)
	if len(lostClaims(told, round)) > 0 {
		s.podWentWithoutItsRound(ctx, issue, told, round, pod)
		return
	}
	s.log.Info("supervise: the pod of this issue's update round is not there, and every claim of the round had answered; nothing was lost and the round is over",
		"issue", issue, "round", round, "pod", pod)
	s.endPodUpdates(told)
}

// openPodUpdate opens the round, or keeps it open, and asks each role of it once. Every claim that
// blocks the round is posted PodUpdateOpen at every tick while the round runs: the event is
// idempotent, and it is what records the round on the claim (supervise's openPodUpdate), with its
// reasons and its clock fixed at the post that opens it.
//
// The ask is the sweep's own, published on the reading that finds the round recorded
// (askRound). The post is asynchronous (supervisor.post), so the tick that opens a round asks
// nothing, and a later tick asks each claim whose record shows this round and no publish mark,
// once its session is ready to take the ask. A claim that left the round's pod and a failed
// claim are asked nothing.
//
// The ask is made before this tick's posts, never after them. It reads the group the tick
// already read, so the order changes nothing it decides, and it skips a claim whose machine is
// deciding something (Machine.Deciding). Made after the posts, it would find the machine deciding
// the post this very tick has just made — a no-op for a claim that records the round already —
// and skip that claim at every tick the race went that way, so a role could go unasked for many
// sweeps of a round whose bound runs all the while.
//
// A claim that blocks the round has a session — an agent has run for it, and may hold unpushed
// work. A claim with no session has never had a worker and blocks nothing. An operator-suspended
// claim holds the round until the operator acts, whatever the round's bound, which is this
// issue's own rule (readReplacement's waitsForOperator), as long as the round's pod is there: a
// gone pod ends its round whatever the hold (roundWithNoProcessInItsPod). The late-round notice
// is what tells the operator (tellLateRound, tellPodlessRound). A failed claim runs no turn on
// the pod and is asked nothing, so the round does not wait for it (askedClaims). A worker whose
// plugin has no ready_for_update op is asked like any other.
//
// It tells nobody. A round that waits on its roles is no stall: its bound ends the wait whatever
// they say, and the readings that hold a round past its bound are told where they are read.
//
// It reports whether this ask can reach a claim whose machine records the round, which is what
// the project's limits charge an opening for (reconcileIssuePodUpdate). Those are the claims the
// round asks (askedClaims) rather than every claim it posts to: a failed claim is posted like
// any other and its machine ignores the event (supervise's table), so an ask whose blocking
// claims have all failed is recorded by no claim at all. An issue with no claim the round can
// reach records no round and so holds no slot: charged an opening all the same, it would be
// read as an opening of this daemon's still in flight at every later sweep (podUpdateOpenings),
// hold that slot for ever, and leave every other drifted issue waiting until the daemon
// restarts.
func (s *supervision) openPodUpdate(ctx context.Context, issue string, group []supervise.Claim, pod, reasons string) bool {
	round := supervise.PodRound(pod)
	blocking := blockingClaims(group)
	reached := len(askedClaims(group)) > 0
	s.askRound(ctx, issue, group, round, pod)
	for _, c := range blocking {
		s.supervisor.post(c.Token, supervise.PodUpdateOpen{Claim: c.Token, Round: round, Reasons: reasons})
	}
	return reached
}

// updateStall is one round that has outlived its bound, as the operator's side is told of it:
// the issue and round it is of, the pod it was opened over (none where no claim of the issue
// records one, tellPodlessRound), what the round was opened for, every claim the round asks with
// whether it answered, the reason that names what holds the round, and the group whose claims
// mark the round told. Nothing is written on the issue: a round held past its bound asks nothing
// of its workers that they have not already been asked, and the operator reads it through the
// controller.
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
	if err := s.enqueue(ctx, stall.issue, rows...); err != nil {
		if ctx.Err() == nil {
			s.log.Error("supervise: tell the operator's side that an update round is held past its bound; telling again at the next sweep",
				"issue", stall.issue, "round", stall.round, "error", err)
		}
		return
	}
	s.log.Warn("supervise: an update round is held past its bound; the controller is told", "issue", stall.issue, "round", stall.round,
		"pod", stall.pod, "reason", stall.reason, "waiting", stall.waiting)
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

// askRound publishes the round's ask to each claim the round asks that records this round and
// has not been asked it yet (unaskedClaims): one record.UpdateAsk row per claim, each to the
// claim's own role on the issue, the architect's included, since every role whose process runs
// in the pod loses it when the round replaces the pod (dispatch://INFRA-2981 R1).
//
// The text is the round's own terms, as its claims recorded them: the reasons the round opened
// with (roundReasons), never the drift this tick read, and the moment the round ends at the
// latest, its recorded opening plus pod_update_grace_minutes (supervise.UpdateTask). It names
// the issue's one rescue branch as well (rescue.Target), which is the branch the push check is
// told to push to (checkUnpushed). So every role of one round reads one text, and the record an
// operator reads names the same reasons (dispatch://INFRA-2981 R9).
//
// Every row of the round goes in one enqueue, so the round's asks land together or not at all
// (workflowRuntime.enqueueOutbox), and each claim is marked afterwards (supervise's
// Machine.PodUpdateAsked), which is the order tellUpdateStalled and markUpdateTold keep for the
// notice. An enqueue that fails marks nothing, and the next tick asks again. A mark that fails
// leaves that claim unmarked, and the next tick asks it a second time, which costs one message
// rather than an ask never published.
//
// A claim whose machine is deciding something now (Machine.Deciding, which waits on nothing) is
// asked at a later tick. The mark is written under the machine's lock, and the decision in flight
// can be a Sandbox relaunch that holds that lock for minutes: a mark made over it would hold the
// whole sweep behind one claim, which is what roundGroup reads the view for. The ask is a sweep
// late for that claim. The decision is never this tick's own post of the round, because
// openPodUpdate asks before it posts. A claim no machine of this daemon holds is asked nothing:
// its ask could not be marked, so every tick would publish it again.
//
// A claim whose agent has not said it is ready (supervise.SaidReady) is asked at a later tick
// too, and is not marked. Such a claim is queued, launching, registered but not ready, or
// suspended, and no live session of it holds the role's topic, so the publish would end at
// notify.ErrNoHolder. Marked all the same, the claim would never be asked: a relaunch of it
// could then run a turn that the round cuts at its bound, with no ask before it. The first tick
// after the agent says it is ready asks it, once. A ready session claims its topic inside its
// first turn or at the first-turn bound (supervise.SaidReady), so an ask in that short window can
// still end at notify.ErrNoHolder.
//
// The ask states the round's bound (roundBound). A round no claim of group records the opening
// of has no bound to state, and its ask would state a wrong end, so the sweep logs an error and
// publishes nothing. The one write of a round on a claim records its opening in the same
// statement (supervise's openPodUpdate), so this is never expected.
//
// A deployment with no outbox (enqueue nil) publishes nothing. The round still runs: it records
// itself on the claims, and its roles can answer it all the same.
func (s *supervision) askRound(ctx context.Context, issue string, group []supervise.Claim, round, pod string) {
	var asking []supervise.Claim
	var machines []*supervise.Machine
	for _, c := range unaskedClaims(group, round) {
		if !supervise.SaidReady(c.State) {
			continue
		}
		m, ok := s.supervisor.Machine(c.Token)
		if !ok || m.Deciding() != nil {
			continue
		}
		asking, machines = append(asking, c), append(machines, m)
	}
	if len(asking) == 0 {
		return
	}
	if s.enqueue == nil {
		s.log.Debug("supervise: this deployment has no outbox, so the update round's ask is published to no role",
			"issue", issue, "round", round, "roles", namesOf(asking))
		return
	}
	bound := roundBound(group, round, s.cfg.PodUpdateGrace)
	if bound == nil {
		s.log.Error("supervise: no claim of this update round records when the round opened, so its ask has no end to state; the ask is published to no role",
			"issue", issue, "round", round, "roles", namesOf(asking))
		return
	}
	text := supervise.UpdateTask(round, roundReasons(group, round), *bound, rescue.Target(issue))
	rows := make([]record.OutboxPayload, len(asking))
	for i, c := range asking {
		rows[i] = record.UpdateAsk{Role: c.Role, Round: round, Pod: pod, Text: text}
	}
	if err := s.enqueue(ctx, issue, rows...); err != nil {
		if ctx.Err() == nil {
			s.log.Error("supervise: enqueue the update round's ask to its roles; asking again at the next sweep",
				"issue", issue, "round", round, "roles", namesOf(asking), "error", err)
		}
		return
	}
	s.log.Info("supervise: the update round's ask is enqueued for each role of the pod",
		"issue", issue, "round", round, "pod", pod, "roles", namesOf(asking))
	for i, m := range machines {
		if _, err := m.PodUpdateAsked(ctx, round); err != nil && ctx.Err() == nil {
			s.log.Error("supervise: record that this claim was asked its update round", "claim", asking[i].Token, "round", round, "error", err)
		}
	}
}

// unaskedClaims is every claim the round asks (askedClaims) whose own record shows this round,
// no answer to it and no publish mark (supervise.Claim.UpdateTaskedAt). A claim that has not
// recorded the round yet is asked at the tick after its post lands, so the mark and the round it
// marks are one claim's record (supervise's Machine.PodUpdateAsked). A claim that left the
// round's pod is asked nothing (supervise's Claim.LeftPodRound): the ask names work that died
// with that pod, and no process the claim runs now holds any of it.
func unaskedClaims(group []supervise.Claim, round string) []supervise.Claim {
	var asking []supervise.Claim
	for _, c := range askedClaims(group) {
		if c.UpdateRound == round && c.UpdateTaskedAt == nil && !answered(c, round) && !c.LeftPodRound(round) {
			asking = append(asking, c)
		}
	}
	return asking
}

// writeOnIssue posts one message on the issue, with the document it links when it links one,
// through the outbox's Dispatch message row.
func (s *supervision) writeOnIssue(ctx context.Context, issue string, post record.MessagePost) error {
	if s.enqueue == nil {
		return fmt.Errorf("write on %s: this deployment has no workflow to write through", issue)
	}
	return s.enqueue(ctx, issue, post)
}

// endPodUpdates ends the round on every claim of the group that is in one. A round ends only
// where the pod it was opened over is gone — replaced by the round itself, replaced under it, or
// gone for a failure of its own — so the post carries nothing of that pod's fate
// (supervise.PodUpdateEnded).
func (s *supervision) endPodUpdates(group []supervise.Claim) {
	for _, c := range group {
		if c.UpdateRound == "" {
			continue
		}
		s.supervisor.post(c.Token, supervise.PodUpdateEnded{Claim: c.Token, Round: c.UpdateRound})
	}
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

// answeredClaims is every claim the round asks (askedClaims) that has answered it.
func answeredClaims(group []supervise.Claim, round string) []supervise.Claim {
	var of []supervise.Claim
	for _, c := range askedClaims(group) {
		if answered(c, round) {
			of = append(of, c)
		}
	}
	return of
}

// heldByOperator is every claim the round asks (askedClaims) that an operator's suspension holds
// (supervise's Claim.RuleHolds): one the operator suspended, or one whose operator suspension
// waits for its agent's turn to end. It is the one wait of a round that outlasts the round's
// bound, because it is a person's (dispatch://INFRA-2981 R5): where a suspended claim is the
// pod's only role its process is gone and nothing else would hold the pod, and where another
// role still runs, nothing but this reading would hold it once the bound steps over the answers.
//
// A claim that left the round's pod (supervise's Claim.LeftPodRound) holds nothing of it: its
// process and its work are in another pod, and the round asks it nothing.
func heldByOperator(group []supervise.Claim, round string) []supervise.Claim {
	var held []supervise.Claim
	for _, c := range askedClaims(group) {
		if c.RuleHolds() && !c.LeftPodRound(round) {
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

// blockingClaims is every claim of the group the round waits on: one with a session, whose agent
// has run and may hold unpushed work. Every claim the sweep groups is live already
// (reconcilePodUpdates drops the retired ones), and a claim with no session has never had a
// worker.
func blockingClaims(group []supervise.Claim) []supervise.Claim {
	var blocking []supervise.Claim
	for _, c := range group {
		if c.Session != "" {
			blocking = append(blocking, c)
		}
	}
	return blocking
}

// waitingClaims is whom the replacement waits for before the round's bound, and whom the line of
// a replacement the bound made names as not having answered: every claim the round asks
// (askedClaims) that has not answered this round and has not left it. It is deliberately the
// conservative of the round's two questions, because what it decides before the bound is the
// deletion of a disk. A claim the round has not reached yet — one whose recorded round is another,
// or none — has not answered it either, and it holds the pod until it does or the bound comes:
// the round asks it at the next tick, and a replacement made before that would end a turn of a
// worker the round asked nothing of.
//
// A claim that has left *this* round waits for nothing (supervise's Claim.LeftPodRound, which is
// the one statement of that rule and is asked here rather than restated). The pod it was in is
// gone, the round asks it nothing more, and a replacement held for its answer would be held for
// an answer no process will give.
//
// A claim that left some earlier round is not that claim. It still records the round it left,
// this round's open has not reached it yet, and it holds this round's pod exactly as any claim
// the round has not reached does, which is why the predicate is asked about the round being
// decided rather than about the claim's own: read of the claim's own round, such a claim would
// hold the pod no longer, and its disk could be deleted with work on it the round had asked
// nobody for.
//
// What a claim lost is the other question (lostClaims), and the two are never the same set.
func waitingClaims(group []supervise.Claim, round string) []supervise.Claim {
	var waiting []supervise.Claim
	for _, c := range askedClaims(group) {
		if !answered(c, round) && !c.LeftPodRound(round) {
			waiting = append(waiting, c)
		}
	}
	return waiting
}

// askedClaims is every blocking claim the round can ask: all but a failed one. A failed claim's
// launches or prompts ran out, so it runs no process and no turn of it runs on the pod; the round
// asks it nothing (unaskedClaims reads this set, and its machine ignores the round's post), and
// nothing but an operator's or an architect's retry brings it back. Holding
// the round for its answer held the pod, and every answered worker's next task with it, for an
// answer it can never give: on the first contract-46 roll a planner that had failed at contract 41
// recorded no round at all, and held its issue's round open behind the three workers that had
// answered. A retry starts it again and it is asked like any other claim from the next tick. Work
// it left on the pod's disk and did not push is carried off it by the push check before the
// replacement (checkUnpushed), which reads the disk itself rather than any claim's answer; the
// replacement's log line names each failed claim it did not wait for.
func askedClaims(group []supervise.Claim) []supervise.Claim {
	var asked []supervise.Claim
	for _, c := range blockingClaims(group) {
		if c.State != supervise.StateFailed {
			asked = append(asked, c)
		}
	}
	return asked
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
	for _, c := range blockingClaims(group) {
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

// podProcess is the process the round's pod is ended through: the first claim of the group that
// records a process in that round's own pod. The pod is what the filter is on, not merely a
// recorded process: a claim the sweep relaunched while the round ran records the pod that
// replaced this one, and ending that pod would delete a pod no round covers, which has drifted
// from nothing and which the round's own replacement has just built.
func podProcess(group []supervise.Claim, round string) (runtime.Known, bool) {
	for _, c := range group {
		if c.Locator != nil && supervise.PodRound(podOf(c.Locator)) == round {
			return runtime.Known{Claim: c.Token, Locator: c.Locator}, true
		}
	}
	return runtime.Known{}, false
}

// waitingOf is the claims as the controller's notice lists them, so it can retry one claim or ask
// the operator to act on one. Each is marked with whether it has already answered this round.
//
// Every late-round notice lists every claim the round asks with this mark (tellLateRound,
// tellPodlessRound), so the list is never empty while the round asks a claim, which the store
// requires of the notice (record.UpdateStalled). Past the bound no answer holds the round, so the
// mark tells the controller what each role said, not whom to prompt: the daemon replaces the pod
// at the bound whether or not a role answers, and the notice is told only for what holds the
// round past that. The mark is the round's own: an answer a claim gave to another round says
// nothing here.
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

// podWentMessage is what the issue is told when its pod went before its round was over: the pod
// was replaced where it stood for a failure of its own, and the disk went with it. The issue's
// spec requires this message. Another agent reads it, so it follows ASD-STE100.
func podWentMessage(round, reasons string, lost []supervise.Claim) string {
	opened := "The daemon did not record why."
	if reasons != "" {
		opened = "The reasons were: " + reasons + "."
	}
	return fmt.Sprintf(`The pod that ran this issue's roles is gone. The daemon opened an update round over that pod (%s). %s The pod went before the round was over.

These roles did not answer the update ask before the pod went: %s. The pod's disk went with the pod. Work that these roles did not push is lost.

The daemon starts each role again, in the same session, on a new pod. Each role must read its issue branch at origin to see the work that it pushed.`,
		round, opened, roleStates(lost))
}

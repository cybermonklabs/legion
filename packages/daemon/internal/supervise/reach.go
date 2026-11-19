package supervise

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/timewords"
)

// UnreachableAfter is how long a claim's session may go without registering with Envoy before the
// claim is out of reach (R5): Envoy's measured bound, 52 s past the longest heartbeat gap of a
// production sample (1,148 s), and the age after which Envoy lets another session take an
// unresponsive holder's issue claim. The relaunch guard reads the same bound for the worker
// stream's last step and for the last activity report of the turn the claim is in, so a turn that
// still takes steps, or whose process still reports, is never cut.
const UnreachableAfter = 20 * time.Minute

// Reach is the daemon's record of whether Envoy can reach a claim's session (R5), kept on the
// claim's own row (store's PutClaimReach) because three of its facts must outlive a daemon restart:
// a claim reported before a boot is not reported again after it, a second relaunch within 6 hours
// is refused whatever the claim did between them, and the daemon relaunches its own controller at
// most once in 6 hours. No write a machine makes carries it, as none carries the step or the
// activity report: the daemon's minute sweep and the relaunch write it between a machine's writes
// (internal/daemon's reach keeper). The one exception is LastRelaunch, which only a relaunch makes
// and which a machine's write alone carries.
//
// Its fields fall in four groups. Session and FirstReady are of the session the claim records,
// which is what the 20 minutes are counted from. Since through ExitTold are one episode of the
// claim out of reach, cleared when its session registers again or the claim stops recording that
// session (EndEpisode). RelaunchedAt is of the keeper's record of the claim, and no episode clears
// it. LastRelaunch is of the claim's own row, and no part of the keeper's record.
type Reach struct {
	// Session is the session the record speaks of: the one the claim's `claims/register` recorded
	// when the daemon last read it. A claim that records another session — a fresh session — is
	// a record begun anew.
	Session string `json:"session,omitempty"`
	// FirstReady is when Session first said ready (Claim.LastReady as the daemon first read it
	// beside that session). A relaunch keeps the session, so its ready does not move this, and the
	// 20 minutes are not counted again from it.
	FirstReady time.Time `json:"firstReady,omitzero"`
	// Since is the moment the 20 minutes were counted from when the daemon marked the claim out of
	// reach, and the zero time while it is not marked: the latest of the session's last
	// registration, the daemon's boot, FirstReady, the end of the daemon's last failed read of
	// Envoy's list and its last read that found no live session.
	Since time.Time `json:"since,omitzero"`
	// LastSeen is the last registration of Session the daemon had read from Envoy's list when it
	// marked the claim, and the zero time where it had read none since its boot.
	LastSeen time.Time `json:"lastSeen,omitzero"`
	// Generation is the launch the daemon last read of the claim while it was marked: a later one
	// is a process the claim was relaunched into since, which UnreachableExitNotice reads.
	Generation uint64 `json:"generation,omitempty"`
	// Reported is whether the controller has been told of this episode in an unreachable-claims
	// notice (record.UnreachableClaimsNotice), and ArchitectTold whether the tree's architect has
	// been told in its own (record.ClaimOutOfReachNotice). They are two because the controller's
	// list is one wake per project, which a list still waiting unsent holds back, while the
	// architect's notice is a row of its own.
	Reported      bool `json:"reported,omitempty"`
	ArchitectTold bool `json:"architectTold,omitempty"`
	// GuardRefusedAt is when the relaunch guard first refused a relaunch of the claim in this
	// episode (RelaunchGuard). ReadyTold is whether the controller has been told the guard would
	// now let it through, and HeldTold whether it has been told the guard has refused it for 60
	// minutes. A refusal after ReadyTold starts a refusal of its own.
	GuardRefusedAt time.Time `json:"guardRefusedAt,omitzero"`
	ReadyTold      bool      `json:"readyTold,omitempty"`
	HeldTold       bool      `json:"heldTold,omitempty"`
	// Relaunched is whether a relaunch reached the claim in this episode, and NotBackTold whether
	// the controller has been told it is still out of reach 20 minutes after it.
	Relaunched  bool `json:"relaunched,omitempty"`
	NotBackTold bool `json:"notBackTold,omitempty"`
	// ExitTold is whether the controller has been told of a marked claim that owes no task and
	// whose process exits at its Envoy role claim (record.UnreachableExitNotice).
	ExitTold bool `json:"exitTold,omitempty"`
	// RelaunchedAt is the claim's last relaunch this record has taken in (Claim.LastRelaunch, as
	// the keeper last read it), whatever the episode: a later Claim.LastRelaunch is one the keeper
	// has yet to take into the episode, which it does at its next read of the claim.
	RelaunchedAt time.Time `json:"relaunchedAt,omitzero"`
}

// Unreachable is whether the claim is marked out of reach.
func (r Reach) Unreachable() bool { return !r.Since.IsZero() }

// EndEpisode is the record once the episode of the claim out of reach is over: its session
// registered again, or the claim no longer records that session. What is of the session and of
// the claim stays.
func (r Reach) EndEpisode() Reach {
	return Reach{Session: r.Session, FirstReady: r.FirstReady, RelaunchedAt: r.RelaunchedAt}
}

// RelaunchAgainAfter is how long after a relaunch a second one of the same claim is refused (R5):
// long enough that a cause that returns the same day goes to a person, short enough that an
// unrelated end later gets a relaunch of its own. It is a choice, not a measurement.
const RelaunchAgainAfter = 6 * time.Hour

// relaunchWaitTool is the one call a relaunch may cut open: a `wait`, which a turn sits in on a
// background job, a subagent, a named service or a message. The activity report says whether a
// job or a subagent it waits on still runs, and a report made in the turn says whether the
// process still runs the turn (RelaunchGuard).
const relaunchWaitTool = "wait"

// The refusals of a relaunch the daemon gives before the machine is asked (internal/daemon's reach
// keeper) and the machine's own (RelaunchGuard, the tree lifecycle's recheck), each a sentinel the
// API names a code for. The three a runtime answers are runtime's (ErrLauncherNotConnected,
// ErrRelaunchReplacesPod, ErrProcessOutlivedStop).
var (
	// ErrClaimReachable: the claim is not marked out of reach, so Envoy can still reach its
	// session, or has registered it within the 20 minutes.
	ErrClaimReachable = errors.New("the claim is not out of reach")
	// ErrRelaunchGuard: a step of the claim's worker stream in the last 20 minutes, an activity
	// report in the last 20 minutes of the turn the claim is in, an open call other than a wait,
	// or an activity report since its launch that shows background work or a subagent running, or
	// no report at all.
	ErrRelaunchGuard = errors.New("the relaunch guard refuses the claim")
	// ErrRelaunchedRecently: a relaunch reached the claim less than 6 hours ago
	// (RelaunchAgainAfter).
	ErrRelaunchedRecently = errors.New("the claim was relaunched less than " + timewords.Words(RelaunchAgainAfter) + " ago")
	// ErrPodUnreachable: every claim of the pod the claim runs in, two or more, is out of reach,
	// so a relaunch into it would recover none of them.
	ErrPodUnreachable = errors.New("every claim of the claim's pod is out of reach")
	// ErrTreeRefusesLaunch: the tree lifecycle's recheck, which every launch makes first
	// (checkLaunch), refuses the claim's launch with an answer of the tree's own (treeRefuses) —
	// its cleanup is reserved or confirmed, its lifecycle is absent or held under another authority,
	// or the epoch the claim is bound to is no longer its tree's open one — so the relaunch stops
	// nothing. The tree's close or its next start decides that claim, not a relaunch.
	ErrTreeRefusesLaunch = errors.New("the claim's tree allows it no launch, so nothing was stopped")
	// ErrTreeCheckFailed: the tree lifecycle's recheck before the stop got no answer of the tree's
	// — the store did not answer it, or the request's time ran out waiting on it — so the relaunch
	// stopped nothing and may be asked for again.
	ErrTreeCheckFailed = errors.New("the daemon could not check the claim's tree lifecycle, so nothing was stopped; relaunch it again")
	// ErrReachUnwritten: the daemon could not write the record a relaunch's notices are told from
	// — the guard's refusal, which the notice that the guard would now let the claim through
	// counts from — so no such notice may follow, and the relaunch is answered as a failure.
	ErrReachUnwritten = errors.New("the daemon could not write the claim's reach record")
)

// RelaunchGuard is why a relaunch must not cut c's process now, and "" when it may (R5): the worker
// stream showed a step in the last 20 minutes, the claim is in a turn whose session sent an
// activity report in that turn in the last 20 minutes, its open call, if any, is not a `wait`, or
// its last activity report since its launch shows a background job or a subagent running, or
// there is no such report. steppedAt is a later step than c's record holds, where the caller knows
// one (the machine's own, Machine.steppedAt); the zero time adds nothing. The daemon's sweep asks
// it of the stored claim for the notice that the guard would now let a refused claim through, and
// for its own relaunch of its controller, and the machine asks it again under its own lock with
// what it read since.
//
// A session sends its report again every 5 minutes while a turn is open, whatever it says
// (pi-legion's TURN_ACTIVITY_HEARTBEAT_MS), from a timer of its process's own, which a frozen or
// hung process does not run. So a report whose time is after the turn began and less than 20
// minutes old is that process still running the turn, though its worker stream shows no step: a
// `wait` on a named `bash` service, which is in no activity report, blocks with no step on the
// pinned Oh My Pi until the service ends. The turn and its start are the claim's own record
// (inTurn, Claim.LastTurnStart), and the 20 minutes the step's bound, on the same clock. The guard
// reads this rule last, so a claim another rule refuses is refused for its own reason. A claim in
// no turn has no turn to cut, and is judged by the other rules alone, so an idle claim with a fresh
// report is let through. A host that sends no such report (a Claude Code worker reports at its Stop
// hook alone) is judged by those rules too.
//
// A launch clears the claim's report (store's putClaim), so a report read here is of the process
// running now.
func RelaunchGuard(c Claim, steppedAt, now time.Time) string {
	last := c.Step.LastStep()
	if steppedAt.After(last) {
		last = steppedAt
	}
	if !last.IsZero() && now.Sub(last) < UnreachableAfter {
		return fmt.Sprintf("its worker stream showed a step at %s, less than %s ago", last.UTC().Format(time.RFC3339), timewords.Words(UnreachableAfter))
	}
	// A call is open while the record holds its start and no end at or after it, the shape the
	// daemon's stall notice reads a call by too (internal/daemon's stallRecordInWords).
	if open := c.Step; open.CallID != "" && (open.EndedAt.IsZero() || open.EndedAt.Before(open.StartedAt)) && open.Tool != relaunchWaitTool {
		return fmt.Sprintf("its open call is %s (%s), not a %s", open.Tool, open.CallID, relaunchWaitTool)
	}
	if c.ActivityAt.IsZero() {
		return "its session has sent no activity report since its launch, so nothing says what it is waiting on"
	}
	if c.ActivityBackground {
		kinds := "background work of no named kind"
		if len(c.ActivityKinds) > 0 {
			kinds = strings.Join(c.ActivityKinds, ", ")
		}
		return fmt.Sprintf("its last activity report, at %s, shows background work running: %s", c.ActivityAt.UTC().Format(time.RFC3339), kinds)
	}
	// Last, so a claim an older rule refuses is refused for its own reason, and this rule catches
	// only a claim every older rule lets through: in no call or in a `wait`, its report naming
	// nothing running.
	if c.inTurn() && c.ActivityAt.After(c.LastTurnStart) && now.Sub(c.ActivityAt) < UnreachableAfter {
		return fmt.Sprintf("its session sent an activity report at %s, less than %s ago, in the turn it began at %s, so its process still runs that turn",
			c.ActivityAt.UTC().Format(time.RFC3339), timewords.Words(UnreachableAfter), c.LastTurnStart.UTC().Format(time.RFC3339))
	}
	return ""
}

// inTurn is whether c's agent is in a turn as the claim's own record says it: working, with a turn
// start (Claim.LastTurnStart) later than its last turn's end. A claim whose record holds no turn
// start has none a report can be dated against, so it is in none. A turn begun under a compaction
// moves the turn start too (turnStartedWhileBusy), so a claim the compaction's end leaves working
// for that turn is in it; one working for a compaction with no turn begun under it is in none,
// and the machine refuses a relaunch under a compaction whatever the guard says
// (relaunchUnreachable).
func (c Claim) inTurn() bool {
	return c.State == StateWorking && c.LastTurnStart.After(c.LastTurnEnd)
}

// RequestRelaunch is a relaunch of a claim Envoy cannot reach, on its same session (R5): the
// controller's `relaunch`, or the daemon's own of its controller, once the daemon has checked what
// the machine does not hold — that the claim is marked out of reach, its last relaunch is 6 hours
// old, and its pod holds a claim Envoy can reach (internal/daemon's reach keeper). Step is the step
// record the store holds as the daemon read it before posting, the freshest durable reading of the
// worker stream (the machine adds what it read since). Cause is the let-go's cause in the daemon's
// words, which reads after "because" and which the claim records and the task it owed comes back
// behind. MayReplacePod is whether the runtime may replace the claim's pod, which only the daemon's
// relaunch of its own controller may. At is the moment the daemon asked for the relaunch on its own
// clock, the one its 6 hours are counted on, which the claim records as its last relaunch once the
// stop has ended its process (Claim.LastRelaunch); the zero time is the machine's clock.
type RequestRelaunch struct {
	Claim         claim.Token
	Step          StepRecord
	Cause         string
	MayReplacePod bool
	At            time.Time
}

// relaunchUnreachable is a claim's relaunch on its same session, in the order the requirement
// sets: the guard, read again under the machine's lock with every step this machine has read and
// the activity report the store holds for this launch; then, before anything is stopped, the
// refusals that need nothing stopped: the runtime's own — a launcher that is not connected, or a
// launch that would replace the pod and end every other role in it (runtime.InPlaceRelauncher) —
// and the tree lifecycle's recheck every launch makes first (checkLaunch: the tree's cleanup
// reserved, or the claim's bound epoch no longer the open one); then the stop of the process,
// after which a process that outlived it ends the relaunch with nothing more done; and only then
// the replacement every daemon-made relaunch makes (replaceProcess's steps): the task the turn ran
// goes back to waiting and comes back behind the cause, and the recorded session is resumed.
//
// The stop has ended the process, so the process is let go there, before the launch and before
// any write that can fail, as a retirement's stop lets its process go (suspendProcess), and what
// it ended is recorded under the relaunch's cause (letGo, recordEnd); the launch lets nothing more
// go. Nothing after the stop returns with the stopped process still recorded: the sweep's next
// report of it would find it gone, charge a death and relaunch it under the death's cause. So a
// refusal the launch's own recheck still meets while the stop waited on the runtime finds a claim
// that records no process, which is read by the same rule as before the stop (treeRefuses,
// relaunchRefused): the tree's own refusal — its cleanup reserved, or its next lifetime opened, so
// the claim's epoch is stale — suspends the claim for the tree's close or its next start, and only
// a store that did not answer is launched again at the probe interval.
//
// The task the cut turn ran goes back to waiting in memory at the stop, decided before the let-go
// forgets the send and the prompts outstanding it reads (decideTakeBack, tookBack), and the claim
// is then written launching, with no process and the task taken back, in one write
// (putClaimAndPending): the shape a relaunch waiting for its launch is stored in, which a restart
// launches again. A store that refuses that write leaves the take-back unsaved and the launch
// waiting for it at the probe interval (relaunchLater, launchOnceTakenBack), since a launch
// written while the store still holds the task confirmed would have a restart read the cut turn
// as a finished one and retire its task.
//
// A refusal before the stop is a refusal of the claim, as the guard's and the runtime's are
// (RefusedError), and the tree lifecycle's recheck answers one of two: the tree's own refusal
// (treeRefuses) is ErrTreeRefusesLaunch, which its tree's close or next start decides, and any
// other error of that check — a store that did not answer — is ErrTreeCheckFailed, written to the
// log, which says nothing was stopped and the relaunch may be asked for again. A launch left to be
// tried again after the stop (relaunchLater, whose error says so: errLaunchWaits) is no refusal:
// the process is stopped, the relaunch is recorded and the daemon makes the launch itself, so the
// relaunch answers as one that ran, with the error the launch met written to the log (launchMade).
// Every other error of the launch after the stop — a refusal of the tree's, or a claim failed at
// its launch budget — is answered as it is.
//
// Once the stop has ended the process the claim records the relaunch (Claim.LastRelaunch), which
// its next write carries: the take-back's, the launch's own, which moves the generation, or that
// of the refusal the launch met, at the latest. So the record the next relaunch within 6 hours is
// refused by is durable with the launch it refuses a repeat of, and a daemon that ends after that
// write loses none of it, whatever the keeper had written by then. A relaunch refused before the
// stop records nothing, and uses no part of the 6 hours.
//
// It charges nothing: no death, and no launch failure for the stop, since the process did nothing
// wrong; a start the runtime refuses inside the launch is charged as any is (launchNow). The
// launch is exempt from the wake pacing, as an operator's own request is: the process is stopped
// already, so a launch held for its turn would keep the claim's session down for the whole wait.
//
// A runtime whose processes share no place (tmux) answers no refusal, and its stop is Suspend:
// an error from it is a process the runtime would not say it stopped, which is refused as one
// that outlived its stop.
//
// The stop is no stop for good: the process is ended only to be started again on the same
// session in this same decision, as a process the registration deadline retires is, so the claim
// keeps its role throughout and View never says its process is stopping (ClaimView.Stopping). A
// role message or a hook prompt the daemon holds for the claim while Envoy cannot reach it waits
// through the stop for the session this relaunch brings back, rather than being finished as one
// whose claim ended (the daemon's outbox, keepsItsSession).
//
// A suspension held over the claim, a wake waiting over it and a compaction in flight each own
// what becomes of its process already, so each refuses the relaunch as a claim between states.
func (m *Machine) relaunchUnreachable(ctx context.Context, ev RequestRelaunch) error {
	refuse := func(err error) error {
		return &RefusedError{State: m.claim.State, Request: "relaunch", Reason: err.Error(), Err: err}
	}
	switch {
	case m.held != nil:
		return refuse(errors.New("a suspension is held over the claim"))
	case m.wake != nil:
		return refuse(fmt.Errorf("a %s of the claim waits its turn in the wake pacing", m.wake.kind))
	case m.compacting != nil:
		return refuse(errors.New("the claim's session is being compacted"))
	case m.claim.Locator == nil:
		return refuse(errors.New("the claim records no process"))
	}
	checked := m.claim
	checked.Step = ev.Step
	report, reported := m.launchActivity(ctx)
	checked.ActivityAt, checked.ActivityBackground, checked.ActivityKinds = time.Time{}, false, nil
	if reported {
		checked.ActivityAt, checked.ActivityBackground, checked.ActivityKinds = report.At, report.Background, report.Kinds
	}
	if why := RelaunchGuard(checked, m.steppedAt, m.deps.Clock.Now()); why != "" {
		return refuse(fmt.Errorf("%w: %s", ErrRelaunchGuard, why))
	}
	loc := *m.claim.Locator
	inPlace, _ := m.deps.Runtime.(runtime.InPlaceRelauncher)
	if inPlace != nil {
		if err := inPlace.RelaunchRefusal(ctx, loc, ev.MayReplacePod); err != nil {
			return refuse(err)
		}
	}
	if err := m.checkLaunch(ctx); err != nil {
		if treeRefuses(err) {
			return refuse(fmt.Errorf("%w: %w", ErrTreeRefusesLaunch, err))
		}
		m.log.Warn("supervise: the tree lifecycle's check of the relaunch got no answer; nothing is stopped, and the relaunch may be asked for again",
			"incarnation", loc.Incarnation, "error", err)
		return refuse(fmt.Errorf("%w: %w", ErrTreeCheckFailed, err))
	}
	m.log.Warn("supervise: relaunching a claim Envoy cannot reach on its same session; the stop charges nothing",
		"incarnation", loc.Incarnation, "cause", ev.Cause, "mayReplacePod", ev.MayReplacePod,
		"lastStep", ev.Step.LastStep(), "tool", ev.Step.Tool, "callId", ev.Step.CallID)
	stop := func() error { return m.deps.Runtime.Suspend(ctx, loc) }
	if inPlace != nil {
		stop = func() error { return inPlace.StopForRelaunch(ctx, loc, ev.MayReplacePod) }
	}
	if err := stop(); err != nil {
		// A runtime that relaunches in place says itself which failure is a process that outlived
		// its stop (StopForRelaunch): a refused suspension of the controller's pod ended nothing
		// and is no such failure. Suspend says only that the stop failed, so its error is read as
		// one, since nothing says the process ended.
		if inPlace == nil && !errors.Is(err, runtime.ErrProcessOutlivedStop) {
			err = fmt.Errorf("%w: %w", runtime.ErrProcessOutlivedStop, err)
		}
		m.log.Error("supervise: the relaunch's stop did not end the claim's process; nothing more is done",
			"incarnation", loc.Incarnation, "error", err)
		return refuse(err)
	}
	at := ev.At
	if at.IsZero() {
		at = m.deps.Clock.Now()
	}
	// A timestamp column holds microseconds, so the moment is recorded as the store reads it back.
	// Its launch is still to be made, so no generation is recorded beside it yet.
	m.claim.LastRelaunch, m.claim.LastRelaunchGeneration = at.UTC().Truncate(time.Microsecond), 0
	back, taking := m.decideTakeBack("the claim was relaunched because "+ev.Cause, false)
	if taking {
		// In memory only: the write comes once the process is let go, and settle writes it with
		// the claim at the claim's next decision if this one's does not land.
		m.tookBack(back)
		m.unsaved = true
	}
	if m.held != nil {
		// The relaunch's stop keeps the session's Envoy registration for the launch it makes, so
		// no clean stop has been sent; the suspension that runs instead owes it (stopOwed).
		return m.endHeld(ctx, stopOwed)
	}
	m.letGo(ctx, letGoCause{cause: ev.Cause, relaunch: true})
	if !taking {
		// The launch that starts writes the generation it left the claim at beside the relaunch,
		// in its own write (launchNow), whether it runs now or is tried again at the probe interval.
		return m.launchMade(m.launchAfter(ctx, false, true, noEnd))
	}
	m.claim.State = StateLaunching
	return m.launchMade(m.launchOnceTakenBack(ctx))
}

// launchOnceTakenBack is the relaunch's launch where its stop took the cut turn's task back: the
// take-back is written first, with the claim launching and recording no process, and the launch
// follows once that write has landed (relaunchUnreachable). A write the store refuses is tried
// again at the probe interval, and the launch with it (relaunchLater); the relaunch is made
// meanwhile, as one whose launch waits for the store is (launchMade), and nothing more is charged.
func (m *Machine) launchOnceTakenBack(ctx context.Context) error {
	if m.unsaved {
		if err := m.saved(m.putClaimAndPending(ctx)); err != nil {
			m.log.Warn("supervise: the relaunch's take-back of its task was not written; its launch waits for it at the probe interval",
				"delivery", pendingID(m.claim.Pending), "error", err)
			return m.relaunchLater(m.deps.Timeouts.Probe, func(ctx context.Context) error {
				return m.launchMade(m.launchOnceTakenBack(ctx))
			}, err)
		}
	}
	return m.launchAfter(ctx, false, true, noEnd)
}

// launchMade is the relaunch's answer once its stop has let the process go: err, the answer of its
// launch, except a launch left to be tried again (errLaunchWaits), which the daemon makes itself,
// so the relaunch is made and answers nil, with that launch's error written to the log. Every
// other error — a refusal of the tree's after the stop, or the claim failed at its launch budget —
// arms no retry and is answered.
func (m *Machine) launchMade(err error) error {
	if !errors.Is(err, errLaunchWaits) {
		return err
	}
	m.log.Warn("supervise: the relaunch's launch waits for the store and is tried again at the probe interval; the relaunch is made",
		"error", err)
	return nil
}

// relaunchClaim is the RequestRelaunch event's row, for the three states a settled agent stands in.
func relaunchClaim(m *Machine, ctx context.Context, ev Event) error {
	return m.relaunchUnreachable(ctx, ev.(RequestRelaunch))
}

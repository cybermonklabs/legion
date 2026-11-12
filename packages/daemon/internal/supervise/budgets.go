package supervise

import (
	"context"
	"fmt"
	"slices"
)

// Budgets are a claim's retry counters, each a separate policy with its own reset point.
//
//   - LaunchFailures: launches that never became a working agent — a spawn or resume the runtime
//     refused, a process that died, one whose agent never registered or registered and never said
//     it was ready. Reset only by the agent's ready: a registration alone proves nothing about the
//     next launch. A launch this daemon's own refusal or its own start ended is relaunched at no
//     charge, which is bounded by that same limit (LaunchEnding, chargesLaunchBudget).
//   - Deaths: processes that died after their agent was ready and while it had work outstanding —
//     a pending task, whose turn was running or which was sent and not yet begun. The relaunch
//     reaches ready again whatever killed the last process, so ready cannot bound these; an agent
//     that dies before it completes a turn each time it is given the task would otherwise be
//     relaunched for ever. The count is charged against the pending task and reset only when that
//     task ends (retirePending): its turn ends, which a killed Oh My Pi never reports, or a
//     suspension or a phase change retires it. A turn that never ran the task — a notice's —
//     clears nothing. A death with nothing pending is not counted: a parked agent — idle, holding
//     no task — that the environment kills now and then is relaunched however often it happens, and
//     so is one killed in a turn no delivery started, a notice's, whose work waits for whatever
//     next wakes the relaunched agent. Bounded by the launch failure limit.
//   - PromptFailures: prompts the agent refused, acknowledged prompts that started no turn, and
//     prompts refused after their acknowledgement. A prompt lost to the transport, not sent for
//     want of a connection, or refused because the agent is in a turn of its own (Oh My Pi's busy
//     answer, before or after its acknowledgement), is re-queued at no charge. Reset by a started
//     turn.
//   - PromptRetires: processes retired because PromptFailures ran out. Reset by a started turn,
//     never by the relaunch itself — a claim that keeps acknowledging and never turning runs out.
type Budgets struct {
	LaunchFailures int
	Deaths         int
	PromptFailures int
	PromptRetires  int
}

// Limits are where each budget runs out: three launch failures, three prompt failures, and two
// prompt retirements in the shipped daemon. The caller reads them from its configuration.
// LaunchFailures bounds Deaths as well: both count processes that did not survive.
type Limits struct {
	LaunchFailures int
	PromptFailures int
	PromptRetires  int
}

// LaunchEnding is how one launch of a claim ended, as the daemon that watched it reads that
// ending: the closed set the launch budget is decided over (chargesLaunchBudget). Three of its
// values say what became of the launch and the fourth says the waiver is spent, and together
// they are every outcome this package knows; an ending outside the set is charged, which is what
// the decision's default is for.
//
// The set is the daemon's to read and this package's to decide over: supervise cannot see what
// the daemon's own API refused or when its routes first served, so the reading arrives through
// Deps.LaunchEnded and only the answer below is made here (dispatch://INFRA-2883 R6).
type LaunchEnding int

const (
	// LaunchEndedOrdinary is a launch that ended with no refusal of this daemon's behind it and
	// outside its own start: a process that died, one whose agent never registered, one whose
	// registration or ready this daemon answered. It is charged, as every launch failure was
	// before the waiver existed, and it is the zero value, so a daemon that answers nothing
	// about its endings charges every one of them.
	LaunchEndedOrdinary LaunchEnding = iota
	// LaunchEndedAfterRefusal is a launch whose registration or ready this daemon itself refused
	// with its start-or-busy answer — the refusal it gives while it starts, and the one a route
	// gives a caller it could not answer inside its bound. The daemon decides it from the memory
	// of its own refusals, by the boot token a registration carried or by the claim and
	// generation a ready named (api.Answering.Refused). Its process then ended because it had no
	// registration to live by, so the daemon's own slowness ended it and must not spend the
	// claim's launch budget: it is charged nothing.
	LaunchEndedAfterRefusal
	// LaunchEndedBeforeFirstRoute is a launch whose end this daemon observed before it had served
	// its first route (api.Answering.FirstServed): there was no answer for the launch to get,
	// whatever it asked. It is read from the daemon's own record of its start rather than from
	// the exit, which carries no time of its own, and it is charged nothing for the same reason
	// as the refusal above.
	LaunchEndedBeforeFirstRoute
	// LaunchWaiverSpent is a claim whose generations this daemon has waived Limits.LaunchFailures
	// times in a row with no registration accepted between them: the waiver is bounded, so from
	// here the charge stands again however the launch ended, and the claim fails at its limit as
	// for any other fault. The count is the daemon's, kept beside the refusals themselves and
	// cleared with the daemon (api.Answering.LaunchWaived, Registered).
	LaunchWaiverSpent
)

// String is the outcome as the launch's log line names it.
func (e LaunchEnding) String() string {
	switch e {
	case LaunchEndedOrdinary:
		return "ordinary"
	case LaunchEndedAfterRefusal:
		return "refused by this daemon"
	case LaunchEndedBeforeFirstRoute:
		return "before this daemon served a route"
	case LaunchWaiverSpent:
		return "waiver spent"
	default:
		return fmt.Sprintf("unknown(%d)", int(e))
	}
}

// chargesLaunchBudget is the one decision over that set: does a launch that ended this way cost
// the claim a launch failure? It is the whole of the waiver policy, read in one place rather
// than as an exemption at the site where the charge happens to be made.
//
// The default charges. A policy that exempted what it could not name would waive the endings a
// later change invents, and a crash-looping process would then be relaunched for ever — which is
// worse than the defect the waiver fixes, so an unnamed ending is charged and its name goes in
// the launch's log line (dispatch://INFRA-2883/comment/2a5ed273-3c5c-4860-9b39-7690718ce9ad).
func chargesLaunchBudget(ending LaunchEnding) bool {
	switch ending {
	case LaunchEndedAfterRefusal, LaunchEndedBeforeFirstRoute:
		return false
	case LaunchEndedOrdinary, LaunchWaiverSpent:
		return true
	default:
		return true
	}
}

// launchEnding is how the daemon reads the ending of the launch this claim now stands over, and
// LaunchEndedOrdinary for a daemon that reads nothing (Deps.LaunchEnded nil), which charges every
// launch failure. It is asked at the charge and nowhere else, with the claim as it stands: its
// generation and its boot token hash are the launch that ended, since nothing has let the process
// go yet (launchNow).
func (m *Machine) launchEnding() LaunchEnding {
	if m.deps.LaunchEnded == nil {
		return LaunchEndedOrdinary
	}
	return m.deps.LaunchEnded(m.claim, m.deps.Limits.LaunchFailures)
}

// relaunchAfterFailure charges one launch failure for a process that did not survive, and
// relaunches the same session after it, once the backoff for the failures charged so far has
// passed (backOff) and its turn in the wake pacing has come (reserveWake) — or fails the claim
// when the budget is spent. A relaunch the tree lifecycle refuses (its cleanup reserved) is
// checked before the charge, so it costs none; one whose process is already stopped is
// relaunchRefused's.
//
// Whether the charge is made at all is chargesLaunchBudget's, over the ending the daemon reads
// (launchEnding): a launch this daemon's own refusal or its own start ended is relaunched for
// free, told to the daemon so the run of waived generations is counted (Deps.LaunchWaived), and
// logged by the outcome's own name. Both paths relaunch after the backoff: a waived relaunch
// costs nothing but still waits, since what it waits out is a daemon that is slow or coming up.
// It is the only site this waiver touches — every ending it adds happens in a state chargeDeath
// charges nothing for (unready), so they all reach the launch budget here — and the launch a
// runtime itself refused is nobody's refusal and keeps its charge (launchNow).
func (m *Machine) relaunchAfterFailure(ctx context.Context) error {
	if err := m.checkLaunch(ctx); err != nil {
		return m.relaunchRefused(ctx, m.relaunchAfterFailure, err)
	}
	ending := m.launchEnding()
	if !chargesLaunchBudget(ending) {
		if m.deps.LaunchWaived != nil {
			m.deps.LaunchWaived(m.claim)
		}
		m.log.Warn("supervise: waived the launch failure of a launch this daemon did not answer",
			"ending", ending.String(), "claim", m.claim.Token, "generation", m.claim.Generation,
			"launchFailures", m.claim.Budgets.LaunchFailures, "limit", m.deps.Limits.LaunchFailures)
		return m.launchAfter(ctx, true, false)
	}
	m.claim.Budgets.LaunchFailures++
	m.log.Warn("supervise: charged a launch failure", "ending", ending.String(), "claim", m.claim.Token,
		"generation", m.claim.Generation, "launchFailures", m.claim.Budgets.LaunchFailures,
		"limit", m.deps.Limits.LaunchFailures)
	if m.claim.Budgets.LaunchFailures >= m.deps.Limits.LaunchFailures {
		return m.fail(ctx, "launch failures ran out")
	}
	return m.launchAfter(ctx, true, false)
}

// chargeDeath counts a process that died after its agent was ready while it had work
// outstanding, and reports whether those deaths have reached the limit.
func (m *Machine) chargeDeath() bool {
	if m.claim.Pending == nil || slices.Contains(unready, m.claim.State) {
		return false
	}
	m.claim.Budgets.Deaths++
	m.log.Warn("supervise: the agent died with work outstanding", "deaths", m.claim.Budgets.Deaths,
		"limit", m.deps.Limits.LaunchFailures)
	return m.claim.Budgets.Deaths >= m.deps.Limits.LaunchFailures
}

// chargePrompt counts one prompt failure. At the limit the process is retired — suspended, the
// session kept — and the same session relaunched, or, once retirements run out too, the claim
// fails with its prompt count left at the limit. A suspension that fails charges nothing: the next
// failure reaches the limit again and retries the retirement. So does a relaunch the tree lifecycle
// refuses, checked before the suspension so the process is kept (checkLaunch). A retirement while a
// suspension is held is that suspension's stop instead, charged nothing (suspendHeld).
func (m *Machine) chargePrompt(ctx context.Context, why string) error {
	failures := m.claim.Budgets.PromptFailures + 1
	if failures < m.deps.Limits.PromptFailures {
		m.claim.Budgets.PromptFailures = failures
		m.log.Warn("supervise: prompt failure", "why", why, "promptFailures", failures,
			"limit", m.deps.Limits.PromptFailures)
		return m.persist(ctx)
	}
	if m.held != nil {
		return m.suspendHeld(ctx)
	}
	retires := m.claim.Budgets.PromptRetires + 1
	last := retires >= m.deps.Limits.PromptRetires
	retirement := m.suspendProcess
	if last {
		retirement = m.endProcess // the last retirement fails the claim, which does not run again
	} else if err := m.checkLaunch(ctx); err != nil {
		return fmt.Errorf("retire %s after %d prompt failures: %w", m.claim.Token, failures, err)
	}
	if err := retirement(ctx); err != nil {
		return fmt.Errorf("retire %s after %d prompt failures: suspend: %w", m.claim.Token, failures, err)
	}
	m.claim.Budgets.PromptRetires = retires
	m.log.Warn("supervise: retired after prompt failures", "why", why, "promptFailures", failures,
		"relaunchCycle", retires, "limit", m.deps.Limits.PromptRetires)
	if last {
		m.claim.Budgets.PromptFailures = failures
		return m.fail(ctx, "prompt retirements ran out")
	}
	m.claim.Budgets.PromptFailures = 0
	return m.launch(ctx)
}

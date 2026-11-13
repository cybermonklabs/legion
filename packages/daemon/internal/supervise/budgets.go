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
//     next launch.
//   - Deaths: processes that died after their agent was ready with work outstanding — a pending
//     task, or a turn the agent had begun and not ended, whether or not a task was behind it
//     (chargeDeath). The relaunch reaches ready again whatever killed the last process, so ready
//     cannot bound these; an agent that dies in its turn each time it is woken would otherwise be
//     relaunched for ever. The count ends with the work it was charged against: the task's retire
//     (retirePending) or a turn that ends normally (turnEnded), either of which says the agent got
//     through what it was doing. A death with no turn open and nothing pending is not counted: a
//     parked agent — idle, holding no task — that the environment kills now and then is relaunched
//     however often it happens. Bounded by the launch failure limit.
//   - PromptFailures: prompts the agent refused, acknowledged prompts that started no turn, and
//     prompts refused after their acknowledgement. A prompt lost to the transport, not sent for
//     want of a connection, or refused because the agent is in a turn of its own (Oh My Pi's busy
//     answer or the Claude Code bridge's, before or after its acknowledgement, or as a result that
//     ran no turn), is re-queued at no charge. Which events charge this budget, and the other two,
//     is one policy's (Charges, outcome.go). Reset by a started turn.
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

// FailureCause is the one word for what failed a claim, chosen by the site that fails it rather
// than read back out of the sentence that site writes (Claim.Failure): one sentence covers four
// different events today, and a reader of it cannot tell a dead process from an agent that never
// came up. The set is closed, so an agent choosing between a retry and an escalation reads a word
// it can decide on and the sentence stays for the person behind it.
//
// It answers a different question than Outcome, which is why the two are two types. An Outcome is
// one event and whether it charges a budget (Charges), and it is never stored; a FailureCause is
// what ended a claim whose budget ran out, stored on the claim (Claim.FailureCause,
// Claim.PendingCause), served as `failure.cause` and `pendingCause`, and pinned to the contracts
// suite's enum. Many outcomes fail a claim under one cause — every prompt outcome under
// prompt_retires_exhausted — and some charge nothing and fail none. A charge site names its
// Outcome, and every cause but the prompt budget's is derived from it in one place (failureCause).
type FailureCause string

const (
	// CauseLaunchRefused: the runtime refused the launch until the launch budget ran out
	// (launchNow), with no other cause pending for the relaunch it refused.
	CauseLaunchRefused FailureCause = "launch_refused"
	// CauseDied: the process died until the deaths or the launch budget ran out (died, directly
	// and through relaunchAfterFailure).
	CauseDied FailureCause = "died"
	// CauseNeverRegistered: the agent did not register before its deadline, or registered and
	// never said it was ready, until the launch budget ran out (registrationDeadline). The one
	// word covers both, which is what the deadline itself does.
	CauseNeverRegistered FailureCause = "never_registered"
	// CauseStalePlugin: a relaunch onto the daemon's own plugin contract that ran out
	// (staleRegistration).
	CauseStalePlugin FailureCause = "stale_plugin"
	// CausePromptRetiresExhausted: the prompt retirements ran out (chargePrompt).
	CausePromptRetiresExhausted FailureCause = "prompt_retires_exhausted"
	// CauseProviderError: a provider error that cannot clear ended the claim's turn
	// (providerErrored), or the claim's producer reported a run of refused turns that reached the
	// launch failure limit (turnOutcome). Both are the provider refusing to run this session's
	// turns, which is the one thing a reader of the word has to act on, and the sentence beside
	// it says which of the two it was.
	CauseProviderError FailureCause = "provider_error"
	// CauseUnrecorded: the claim failed before the daemon recorded a cause. No live path writes
	// it; the migration that adds the column writes it for every stored failure whose sentence
	// does not say which event wrote it.
	CauseUnrecorded FailureCause = "unrecorded"
)

// FailureCauses is the closed set the constants above are, in the order they are declared: the
// roster of the set, which nothing in Go can read off the constants themselves. A cause added
// here with no word added to the `legion-api.ts` enum of the same set would make every strict
// plugin refuse the whole state body, so the two are pinned to each other through one file both
// suites read: `internal/api`'s TestFailureCausesGolden writes this list to
// `packages/contracts/fixtures/daemon-api/failure-causes.json`, and
// `packages/contracts/src/legion-api.test.ts` holds that file equal to `LEGION_FAILURE_CAUSES`.
// The roster itself is kept by hand beside the constants: a constant added above and left out of
// this list fails no test, so add every new cause here as well. What the fixture ties is this
// list to the enum, so a word added to one of those two alone fails one of the suites.
var FailureCauses = []FailureCause{
	CauseLaunchRefused,
	CauseDied,
	CauseNeverRegistered,
	CauseStalePlugin,
	CausePromptRetiresExhausted,
	CauseProviderError,
	CauseUnrecorded,
}

// relaunchAfterFailure charges one launch failure for a process that did not survive, and
// relaunches the same session after it, once the backoff for the failures charged so far has
// passed (backOff) and its turn in the wake pacing has come (reserveWake) — or fails the claim
// when the budget is spent. Whether o charges is the policy's (Charges, through chargeLaunch); an
// outcome it waives relaunches with no charge and no backoff.
//
// A relaunch the tree lifecycle refuses (its cleanup reserved) is checked before the charge, so
// it costs none; one whose process is already stopped is relaunchRefused's. That check is a guard
// of this site's and no outcome question: it asks whether the tree may launch at all, which no
// event of the claim's answers.
//
// o is what failed the process this relaunch replaces, from the caller that saw it, and the cause
// derived from it (failureCause) is recorded on the claim before the charge: it is what the claim
// fails under when this budget runs out, and what a launch the runtime refuses on the way fails
// under too (launchNow, Claim.PendingCause). The retry of a relaunch the lifecycle check refused
// carries the same outcome, which is why it is handed over in a closure rather than as the method
// value.
func (m *Machine) relaunchAfterFailure(ctx context.Context, o Outcome) error {
	if err := m.checkLaunch(ctx); err != nil {
		return m.relaunchRefused(ctx, func(ctx context.Context) error {
			return m.relaunchAfterFailure(ctx, o)
		}, err)
	}
	cause := failureCause(o)
	m.claim.PendingCause = cause
	if !Charges(o) {
		m.log.Info("supervise: relaunching after an outcome the policy charges nothing", "outcome", string(o))
		return m.launchAfter(ctx, false, false)
	}
	if m.chargeLaunch(o) {
		return m.fail(ctx, cause, "launch failures ran out")
	}
	return m.launchAfter(ctx, true, false)
}

// chargeLaunch counts one launch failure where the policy charges o (Charges), and reports whether
// the launch budget is spent. It is the count both launch-budget sites make: a process that did not
// survive (relaunchAfterFailure) and a launch the runtime refused (launchNow, which retries in its
// own loop rather than relaunching).
func (m *Machine) chargeLaunch(o Outcome) bool {
	if !Charges(o) {
		return false
	}
	m.claim.Budgets.LaunchFailures++
	return m.claim.Budgets.LaunchFailures >= m.deps.Limits.LaunchFailures
}

// chargeDeath counts a process that died after its agent was ready with work outstanding, and
// reports whether those deaths have reached the limit. Whether the death charges is the policy's
// (Charges, asked of o); whether there is anything to charge it against is this function's guard,
// which is no outcome question: a claim not yet ready owes nothing a death could
// cost, and neither does one with no work outstanding. Work is outstanding when the claim holds a
// task, and also when a turn of the agent's was open at the death — the claim's last turn started
// and has not ended — since a turn an Envoy message began does the issue's work with no task
// behind it, and a root architect killed six times inside its turns was relaunched for ever
// because each death found nothing pending. A death with no turn open and nothing pending is a
// parked agent the environment killed, which is not counted however often it happens.
//
// The death ends the turn it interrupted (Claim.LastTurnEnd), which no other path does: a turn
// records its end only when the agent ends it, so a turn whose agent died would otherwise stay
// open for ever and every later death of the relaunched agent — idle, with nothing pending —
// would be charged against it, failing a claim that is doing nothing wrong. Each death after
// this one is judged on the turn it finds, as the first was.
func (m *Machine) chargeDeath(o Outcome) bool {
	if slices.Contains(unready, m.claim.State) {
		return false
	}
	open := m.turnOpen()
	if m.claim.Pending == nil && !open {
		return false
	}
	if !Charges(o) {
		return false
	}
	m.claim.Budgets.Deaths++
	m.claim.LastTurnEnd = m.deps.Clock.Now()
	m.log.Warn("supervise: the agent died with work outstanding", "outcome", string(o), "deaths", m.claim.Budgets.Deaths,
		"limit", m.deps.Limits.LaunchFailures, "pending", pendingID(m.claim.Pending), "turnOpen", open)
	return m.claim.Budgets.Deaths >= m.deps.Limits.LaunchFailures
}

// turnOpen is whether the agent began a turn it has not ended: the claim is working, which is the
// state a turn's start puts it in and its end takes it out of, or the claim's recorded turn times
// say so. Both are persisted, so the answer survives the restart that rebuilt the machine, and
// the times answer for a claim whose state has already moved on.
func (m *Machine) turnOpen() bool {
	return m.claim.State == StateWorking || m.claim.LastTurnStart.After(m.claim.LastTurnEnd)
}

// chargePrompt counts one prompt failure where the policy charges o (Charges), and charges
// nothing where it does not. At the limit the process is retired — suspended, the session kept —
// and the same session relaunched, or, once retirements run out too, the claim fails with its
// prompt count left at the limit, under the prompt budget's own word: what ran out is the
// retirements, whichever event spent the last of them (CausePromptRetiresExhausted).
//
// Three guards here are not outcome questions, and stay this function's. A
// relaunch the tree lifecycle refuses is checked before the suspension so the process is kept
// (checkLaunch): it asks whether the tree may launch at all. A retirement while an operator's
// suspension is held is that suspension's stop instead, charged nothing (suspendHeld): the
// operator decided what becomes of the process. And the last retirement fails the claim (fail)
// rather than relaunching it, which is the budget running out, not an event's charge. A suspension
// that fails charges nothing: the next failure reaches the limit again and retries the retirement.
func (m *Machine) chargePrompt(ctx context.Context, o Outcome) error {
	if !Charges(o) {
		m.log.Info("supervise: the prompt's outcome charges nothing", "outcome", string(o))
		return nil
	}
	failures := m.claim.Budgets.PromptFailures + 1
	if failures < m.deps.Limits.PromptFailures {
		m.claim.Budgets.PromptFailures = failures
		m.log.Warn("supervise: prompt failure", "outcome", string(o), "promptFailures", failures,
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
	m.log.Warn("supervise: retired after prompt failures", "outcome", string(o), "promptFailures", failures,
		"relaunchCycle", retires, "limit", m.deps.Limits.PromptRetires)
	if last {
		m.claim.Budgets.PromptFailures = failures
		return m.fail(ctx, CausePromptRetiresExhausted, "prompt retirements ran out")
	}
	m.claim.Budgets.PromptFailures = 0
	return m.launch(ctx)
}

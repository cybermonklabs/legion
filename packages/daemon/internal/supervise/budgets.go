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

// FailureCause is the one word for what failed a claim, chosen by the site that fails it rather
// than read back out of the sentence that site writes (Claim.Failure): one sentence covers four
// different events today, and a reader of it cannot tell a dead process from an agent that never
// came up. The set is closed, so an agent choosing between a retry and an escalation reads a word
// it can decide on and the sentence stays for the person behind it.
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
	// (providerErrored).
	CauseProviderError FailureCause = "provider_error"
	// CauseUnrecorded: the claim failed before the daemon recorded a cause. No live path writes
	// it; the migration that adds the column writes it for every stored failure whose sentence
	// does not say which event wrote it.
	CauseUnrecorded FailureCause = "unrecorded"
)

// relaunchAfterFailure charges one launch failure for a process that did not survive, and
// relaunches the same session after it, once the backoff for the failures charged so far has
// passed (backOff) and its turn in the wake pacing has come (reserveWake) — or fails the claim
// when the budget is spent. A relaunch the tree lifecycle refuses (its cleanup reserved) is
// checked before the charge, so it costs none; one whose process is already stopped is
// relaunchRefused's.
//
// cause is what failed the process this relaunch replaces, from the caller that saw it, and it is
// recorded on the claim before the charge: it is what the claim fails under when this budget runs
// out, and what a launch the runtime refuses on the way fails under too (launchNow,
// Claim.PendingCause). The retry of a relaunch the lifecycle check refused carries the same
// cause, which is why it is handed over in a closure rather than as the method value.
func (m *Machine) relaunchAfterFailure(ctx context.Context, cause FailureCause) error {
	if err := m.checkLaunch(ctx); err != nil {
		return m.relaunchRefused(ctx, func(ctx context.Context) error {
			return m.relaunchAfterFailure(ctx, cause)
		}, err)
	}
	m.claim.PendingCause = cause
	m.claim.Budgets.LaunchFailures++
	if m.claim.Budgets.LaunchFailures >= m.deps.Limits.LaunchFailures {
		return m.fail(ctx, cause, "launch failures ran out")
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
		return m.fail(ctx, CausePromptRetiresExhausted, "prompt retirements ran out")
	}
	m.claim.Budgets.PromptFailures = 0
	return m.launch(ctx)
}

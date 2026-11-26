package supervise

import (
	"context"
	"fmt"
	"slices"

	"github.com/sjawhar/legion/daemon/internal/wakepace"
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

// Spent is whether a failure that left the claim's budgets at b spent any of them: one at or past
// its limit in l, deaths measured against the launch failure limit as chargeDeath measures them. A
// failure that spent none, a provider error's run of refused turns (turnOutcome) or a prompt
// refused at launch (promptRefused), failed the claim on its own account.
func (b Budgets) Spent(l Limits) bool {
	return spentAt(b.LaunchFailures, l.LaunchFailures) || spentAt(b.Deaths, l.LaunchFailures) ||
		spentAt(b.PromptFailures, l.PromptFailures) || spentAt(b.PromptRetires, l.PromptRetires)
}

// spentAt is whether count has reached limit, a limit of 0 being none.
func spentAt(count, limit int) bool { return limit > 0 && count >= limit }

// OneAttemptLeft is what b, the budgets a claim failed with, leaves the half-open retry of a
// production check's look (RequestRetry.OneAttemptLeft): each budget the failure spent (Spent) set
// one attempt short of its limit in l, and every other count as the failure left it, so one more
// failure of the same kind fails the claim again. A failure that spent none of them leaves nothing
// to keep, and the retry starts a fresh run, as every other retry does: the zero Budgets.
func (b Budgets) OneAttemptLeft(l Limits) Budgets {
	if !b.Spent(l) {
		return Budgets{}
	}
	short := func(count *int, limit int) {
		if spentAt(*count, limit) {
			*count = limit - 1
		}
	}
	short(&b.LaunchFailures, l.LaunchFailures)
	short(&b.Deaths, l.LaunchFailures)
	short(&b.PromptFailures, l.PromptFailures)
	short(&b.PromptRetires, l.PromptRetires)
	return b
}

// FailureCause is the one word for what failed a claim, chosen by the site that fails it rather
// than read back out of the sentence that site writes (Claim.Failure): one sentence covers four
// different events today, and a reader of it cannot tell a dead process from an agent that never
// came up. The set is closed, so an agent choosing between a retry and an escalation reads a word
// it can decide on and the sentence stays for the person behind it.
type FailureCause string

const (
	// CauseLaunchRefused: the runtime refused the launch until the launch budget ran out, or
	// refused it in a way no try changes, a system prompt past the start frame (launchTries,
	// promptRefused), with no other cause pending for the relaunch it refused.
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
	// CauseProviderError: a provider error that cannot clear ended the claim's turn, as the
	// result of the prompt that turn ran says (providerErrored), or as the report of a turn that
	// ran no prompt of the daemon's says (unpromptedTurnEnded), which is the provider refusing to
	// run this session's turns: the one thing a reader of the word has to act on. A claim an
	// earlier daemon failed can carry it for a run of refused turns that reached the launch failure
	// limit, which fails no claim now (turnOutcome); the sentence beside the word says which it was.
	CauseProviderError FailureCause = "provider_error"
	// CauseCyberPolicy: OpenAI refused the session's content for cyber policy, or Oh My Pi held a
	// request after such a refusal, as the result of the prompt the turn ran says, or as the
	// report of a turn that ran no prompt of the daemon's says (providerErrorTurnEnded, Oh My Pi's
	// class `cyber_policy`). It is a provider error that cannot clear, and a word of its own because
	// it is the one failure after which no retry of the daemon's own may launch the claim's agent
	// again (Claim.HeldForCyberPolicy): every prompt the daemon sends is input from outside the
	// session, which ends Oh My Pi's hold, so the refused content would go to OpenAI again and add
	// a refusal to the organization's record. The architect's retry of the held phase and the
	// operator's retry launch the agent again, each in a new session (freshSession), which holds
	// none of that content.
	CauseCyberPolicy FailureCause = "cyber_policy"
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
	CauseCyberPolicy,
	CauseUnrecorded,
}

// relaunchAfterFailure charges one launch failure for a process that did not survive, and
// relaunches the same session after it, once the backoff for the failures charged so far has
// passed (backOff) and its turn in the wake pacing has come (reserveWake) — or fails the claim
// when the budget is spent. A relaunch the tree lifecycle refuses (its cleanup reserved) is
// checked before the charge, so it costs none; one whose process is already stopped is
// relaunchRefused's. A death is charged once however often its relaunch is raised
// (deathLaunchFailureCharged).
//
// cause is what failed the process this relaunch replaces, from the caller that saw it, and it is
// recorded on the claim before the charge: it is what the claim fails under when this budget runs
// out, and what a launch the runtime refuses on the way fails under too (launchTries,
// Claim.PendingCause). The retry of a relaunch the lifecycle check refused carries the same
// cause, which is why it is handed over in a closure rather than as the method value.
//
// ending is why the process the claim still records is let go, in the daemon's words, which the
// launch records with what that process ended (launch, letGo); noEnd where the caller let it go
// already (registrationDeadline).
//
// place is the place the caller already took for this relaunch in the wake pacing, as launchAfter
// takes it, and nil for every caller but the boot's replay of a recorded death
// (RecordedDeath.Place). The retry of a refused relaunch passes nil: it is raised anew, so it
// takes a place afresh.
func (m *Machine) relaunchAfterFailure(ctx context.Context, cause FailureCause, ending string, place *wakepace.Place) error {
	if err := m.checkLaunch(ctx); err != nil {
		return m.relaunchRefused(ctx, func(ctx context.Context) error {
			return m.relaunchAfterFailure(ctx, cause, ending, nil)
		}, err)
	}
	if !m.deathLaunchFailureCharged(cause) {
		m.claim.PendingCause = cause
		m.claim.Budgets.LaunchFailures++
	}
	if m.claim.Budgets.LaunchFailures >= m.deps.Limits.LaunchFailures {
		return m.fail(ctx, cause, "launch failures ran out")
	}
	return m.launchAfter(ctx, true, false, false, place, ending)
}

// deathLaunchFailureCharged is whether cause is a death whose launch failure an earlier raise of
// the same relaunch has already charged. A death's relaunch is raised as often as that one death
// is: by the report that decided it (died), by the sweep's next report of the same death, where
// the lifecycle check refused the relaunch before (relaunchRefused keeps such a claim over its
// dead process and arms no retry), and by the boot's replay of the death after a restart
// (relaunchRecordedDeath). The first of them that passes the lifecycle check charges the launch
// failure, and the others charge none. What says the charge was made is the cause it records
// (Claim.PendingCause), over an end the claim holds decided (Claim.DeathCharged): died clears
// the cause in the decision that charges the death, and the charge sets it to died in the same
// claim write as the count. So a write the claim makes while the relaunch waits — a task a start
// queues meanwhile — stores the two together, and a restart reads either both or neither.
func (m *Machine) deathLaunchFailureCharged(cause FailureCause) bool {
	return cause == CauseDied && m.claim.DeathCharged && m.claim.PendingCause == CauseDied
}

// chargeDeath counts a process that died after its agent was ready with work outstanding, and
// reports whether those deaths have reached the limit. Work is outstanding when the claim holds a
// task, and also when a turn of the agent's was open at the death — the claim's last turn started
// and has not ended — since a turn an Envoy message began does the issue's work with no task
// behind it, and a root architect killed six times inside its turns was relaunched for ever
// because each death found nothing pending. A death with no turn open and nothing pending is a
// parked agent the environment killed, which is not counted however often it happens.
//
// The daemon's own delivery of a relaunch's record is no task (HoldsTask): it is the sentence of
// what the last let-go ended, which a parked agent is given at every ready that finds it holding
// nothing, so a parked agent killed again before it read it holds that and nothing else, and its
// death is not counted either.
//
// The death ends the turn it interrupted (Claim.LastTurnEnd), as the controller keeper's end of a
// turn open past its bound does (endTurnPastBound) and a compaction's start does for a turn whose
// end was never dated (compactStart): a turn whose agent died would otherwise stay open until one
// of those, and every later death of the relaunched agent — idle, with nothing pending — would be
// charged against it, failing a claim that is doing nothing wrong. Each death after this one is
// judged on the turn it finds, as the first was.
func (m *Machine) chargeDeath() bool {
	if slices.Contains(unready, m.claim.State) {
		return false
	}
	open := m.turnOpen()
	task := HoldsTask(m.claim.Pending)
	if !task && !open {
		return false
	}
	m.claim.Budgets.Deaths++
	m.claim.LastTurnEnd = m.deps.Clock.Now()
	m.log.Warn("supervise: the agent died with work outstanding", "deaths", m.claim.Budgets.Deaths,
		"limit", m.deps.Limits.LaunchFailures, "pending", pendingID(m.claim.Pending), "turnOpen", open)
	return m.claim.Budgets.Deaths >= m.deps.Limits.LaunchFailures
}

// turnOpen is whether the agent began a turn it has not ended: the claim is working, which is the
// state a turn's start puts it in and its end takes it out of, or the claim's recorded turn times
// say so. Both are persisted, so the answer survives the restart that rebuilt the machine, and
// the times answer for a claim whose state has already moved on.
func (m *Machine) turnOpen() bool {
	_, open := m.claim.OpenTurn()
	return m.claim.State == StateWorking || open
}

// chargePrompt counts one prompt failure. At the limit the process is retired — suspended, the
// session kept — and the same session relaunched, or, once retirements run out too, the claim
// fails with its prompt count left at the limit. A suspension that fails charges nothing: the next
// failure reaches the limit again and retries the retirement. So does a relaunch the tree lifecycle
// refuses, checked before the suspension so the process is kept (checkLaunch). A retirement while a
// suspension is held is that suspension's stop instead, charged nothing (suspendHeld).
//
// The retirement's stop is where the process is let go, so that is where what it ended is
// recorded, under the retirement's own cause (endRetired, letGo): the last retirement as well,
// whose failure then finds no process left to record. The launch that follows lets nothing go.
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
	if err := retirement(ctx, endRetired); err != nil {
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
	return m.launch(ctx, noEnd)
}

package supervise

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	"github.com/sjawhar/legion/daemon/internal/redact"
	"github.com/sjawhar/legion/daemon/internal/store/storetext"
)

// TurnOutcome is one turn of a claim's agent as the backend's producer reported it when that turn
// ended: the launch generation it ran at, the producer's own sequence within that generation,
// whether the provider refused the turn, and what the provider said about a refusal.
//
// The producer is the one counter. Oh My Pi's turns are reported by pi-legion's agent_end hook and
// Claude Code's by the Claude bridge, each over POST /legion/v1/claims/turn-outcome; the turn's end
// on the worker stream (StreamTurnEnd) and every turn ordinal the daemon keeps take no part in
// this, so no two counters can drift. A turn no report arrived for — the process died before it
// could send one — is a key the window never holds, which counts neither way.
//
// The tags are the shape the store keeps the window in (the claims table's turn_outcomes), which
// is the one place a report is written.
type TurnOutcome struct {
	Generation uint64 `json:"generation"`
	Sequence   uint64 `json:"sequence"`
	Refused    bool   `json:"refused"`
	Message    string `json:"message,omitempty"`
}

// compareTurnOutcomes orders reports as their turns ran: by generation, and within a generation by
// the producer's own sequence. It is the order the window is kept in and the run is derived in, so
// a report that arrives late or out of order lands in its own place rather than at the end.
func compareTurnOutcomes(a, b TurnOutcome) int {
	if by := cmp.Compare(a.Generation, b.Generation); by != 0 {
		return by
	}
	return cmp.Compare(a.Sequence, b.Sequence)
}

// maxRefusalMessage bounds, in runes, the provider's message a report carries. The message is
// kept with its report in the claim's window, which the store holds, so a provider that returns a
// page of diagnostics leaves a sentence rather than a page in the store. A message past the bound
// keeps its first maxRefusalMessage-1 runes and "…" (redact.Runes).
const maxRefusalMessage = 500

// turnOutcomeWindow is how many reports a claim keeps: the launch failure limit
// (launch_failure_limit, 3 by default), which is the run of refused turns at which a retry of the
// claim starts a new session (freshSession). That is the one thing the run decides, and it asks
// only whether the run has reached the limit, so the newest limit reports are everything it reads:
// a longer run reads as the limit, and a report older than all of them cannot shorten a run
// derived over them, and is dropped. That bound is why a claim whose agent works for a week
// carries a handful of rows rather than a turn-by-turn history nothing reads.
func (m *Machine) turnOutcomeWindow() int { return m.deps.Limits.LaunchFailures }

// recordTurnOutcome puts a report in the claim's window, in (generation, sequence) order, and
// trims the window to its bound (turnOutcomeWindow). A report whose key the window already holds
// replaces it, so a producer that repeats one changes nothing. No count is kept in the window:
// every reader derives what it needs from it as it stands (RefusedRun), which is what makes a
// late report and an in-order one leave the same answer.
//
// It reports whether the report is new to the window and kept in it. A repeat of a report the
// window holds is not — pi-legion sends a report again when the daemon restarted under its first
// attempt (retryWhileDaemonRestarts) — and nor is one older than everything a full window keeps,
// so what a report decides (turnOutcome) is decided once for its turn.
//
// The message is the provider's text, so it is kept as the daemon keeps a far side's text
// (storetext.FarSideText: made valid, then read whole by the URL rule) before redact.Runes cuts
// it, so the window in memory is the window the store holds, and the cut cannot leave half a
// credential.
func (m *Machine) recordTurnOutcome(o TurnOutcome) bool {
	o.Message = redact.Runes(storetext.FarSideText(o.Message), maxRefusalMessage)
	at, found := slices.BinarySearchFunc(m.claim.TurnOutcomes, o, compareTurnOutcomes)
	kept := !found
	if found {
		m.claim.TurnOutcomes[at] = o
	} else {
		m.claim.TurnOutcomes = slices.Insert(m.claim.TurnOutcomes, at, o)
	}
	if extra := len(m.claim.TurnOutcomes) - m.turnOutcomeWindow(); extra > 0 {
		m.claim.TurnOutcomes = slices.Delete(m.claim.TurnOutcomes, 0, extra)
		kept = kept && at >= extra
	}
	return kept
}

// RefusedRun is the run of refused turns the claim's reports end with, in the order the turns ran,
// counted up to the window's bound (turnOutcomeWindow). It is derived on every read and stored
// nowhere, so no count is ever mutated and a report that arrives late lands in its place and is
// counted there. The run carries across generations: a session the provider refused is refused
// again when it is resumed, which is why the retry of a claim whose run reached the launch failure
// limit starts a new session (freshSession), and a new session starts a fresh run (freshRun).
func (c Claim) RefusedRun() int {
	run := 0
	for i := len(c.TurnOutcomes) - 1; i >= 0 && c.TurnOutcomes[i].Refused; i-- {
		run++
	}
	return run
}

// LastRefusal is what the provider said about the newest refused turn the claim keeps, and "" when
// it keeps none or the provider said nothing. The retry that leaves a refused session behind logs
// it (retry). The words are stored for more than that line: a turn that ran no prompt of the
// daemon's, a phase reminder among them, has no prompt result, so its report is the daemon's one
// record of what the provider answered that turn. The report's status, never these words, is what
// judges such a turn (turnOutcome).
func (c Claim) LastRefusal() string {
	for i := len(c.TurnOutcomes) - 1; i >= 0; i-- {
		if c.TurnOutcomes[i].Refused {
			return c.TurnOutcomes[i].Message
		}
	}
	return ""
}

// freshRun drops every turn the claim has reports for, so its refused run starts again. The run
// belongs to the session whose turns were refused, and a claim leaves a session only by forgetting
// it (forgetSession): every relaunch resumes the session the claim recorded, and a registration
// naming another is refused, so the forgetting is the one moment a fresh run begins.
func (m *Machine) freshRun() { m.claim.TurnOutcomes = nil }

// turnOutcome records the turn its producer reported and derives the claim's trailing run of
// refused turns from the window that leaves. The run fails no claim, however long
// (dispatch://INFRA-4508): it decides only whether a retry starts a new session (freshSession).
//
// What a refused turn's provider error does is decided once for each turn, by the one record of
// that turn that can say: the result of the prompt the turn ran (providerErrored), or, for a turn
// that ran no prompt of the daemon's, this report (unpromptedTurnEnded). The producer says which
// (RequestTurnOutcome.Unprompted), because only it sees what started the turn: Oh My Pi's
// agent_end and the worker stream's turn end name no prompt, and the report and the prompt's
// result reach the daemon on two channels, in either order. So the report of a turn a prompt ran
// is never judged, whether it arrives before that prompt's result or after it, and a turn that ran
// none is judged from its report alone. The judgment is made for a report new to the window
// (recordTurnOutcome) and of the generation the claim runs now: a report of a generation the claim
// has left is a turn of a process that is gone, recorded in its place and judged by nothing.
//
// A turn that ran no prompt of the daemon's — a phase reminder the plugin sent (pi.sendMessage),
// an Envoy message, a background job's result — is held to dispatch://INFRA-4508's R1 as a
// prompt's turn is. A refusal whose status cannot clear (holdsTheIssue) fails the claim at once
// with CauseProviderError, so the workflow holds the issue for its architect, and the plugin's
// reminders end: their record check drops every reminder of a failed claim. A refusal that can
// clear, or whose report carries no status, fails nothing, and the reminders keep their pace; it
// counts in the claim's one run of provider-error turns (ProviderErrorRun) beside refused prompts,
// so the project's controller is told once when that run reaches providerErrorRunTold. A clean
// such turn ends the run, as a clean prompt's turn does.
func turnOutcome(m *Machine, ctx context.Context, ev Event) error {
	r := ev.(RequestTurnOutcome)
	kept := m.recordTurnOutcome(TurnOutcome{Generation: r.Generation, Sequence: r.Sequence, Refused: r.Refused, Message: r.Message})
	m.log.Info("supervise: a turn ended", "generation", r.Generation, "sequence", r.Sequence,
		"refused", r.Refused, "httpStatus", r.HTTPStatus, "unprompted", r.Unprompted,
		"refusedRun", m.claim.RefusedRun(), "freshSessionAt", m.deps.Limits.LaunchFailures)
	if !r.Unprompted || !kept || r.Generation != m.claim.Generation {
		return m.persist(ctx)
	}
	return m.unpromptedTurnEnded(ctx, r)
}

// unpromptedTurnEnded judges a turn that ran no prompt of the daemon's from its report, as
// providerErrored judges a prompt's turn from that prompt's result, with one difference: no task
// is the turn's to keep or send again, since whatever the claim owes, this turn did not carry it,
// and the reminder that started it keeps its own pace (packages/pi-shared/src/phase-reminder.ts).
// The cause is built from the status alone (providerErrorCause), and the controller's notice names
// no model, since a report carries none.
func (m *Machine) unpromptedTurnEnded(ctx context.Context, r RequestTurnOutcome) error {
	if !r.Refused {
		if m.claim.ProviderErrorRun != 0 {
			m.log.Info("supervise: a turn that ran no prompt of the daemon's ended with no provider error; the run of provider errors is over",
				"generation", r.Generation, "sequence", r.Sequence, "providerErrorRun", m.claim.ProviderErrorRun)
			m.claim.ProviderErrorRun = 0
		}
		return m.persist(ctx)
	}
	cause := providerErrorCause(r.HTTPStatus)
	m.claim.ProviderErrorRun++
	m.log.Warn("supervise: a turn that ran no prompt of the daemon's ended in a provider error",
		"generation", r.Generation, "sequence", r.Sequence, "httpStatus", r.HTTPStatus,
		"providerErrorRun", m.claim.ProviderErrorRun)
	m.tellControllerOfProviderErrors("", cause)
	if holdsTheIssue(r.HTTPStatus) {
		return m.fail(ctx, CauseProviderError,
			"a provider error that cannot clear ended a turn this claim's agent ran with no prompt of the daemon's: "+cause)
	}
	return m.persist(ctx)
}

// freshSession is whether a retry starts a new session rather than relaunching the recorded one,
// and why, for the line it logs. An operator asks for one with `legion claims retry
// --fresh-session`. A claim whose provider refused the recorded session's last limit turns in a
// row takes one whoever retried it — its architect's retry_or_escalate, the next start of its
// role, or that operator — because resuming the session those turns were refused in is what would
// be refused again. Such a claim failed for a reason of its own, since a run of refused turns
// fails none (turnOutcome).
func freshSession(ev Event, c Claim, limit int) (bool, string) {
	asked := false
	switch ev := ev.(type) {
	case RequestRetry:
		asked = ev.FreshSession
	case RequestOperatorRetry:
		asked = ev.FreshSession
	}
	switch {
	case asked:
		return true, "the retry asked for a new session"
	case c.RefusedRun() >= limit:
		return true, fmt.Sprintf("the provider refused the recorded session's last %d turns in a row", limit)
	}
	return false, ""
}

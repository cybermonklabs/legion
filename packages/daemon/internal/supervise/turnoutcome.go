package supervise

import (
	"cmp"
	"context"
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
// (launch_failure_limit, 3 by default). RefusedRun reads only the newest of them and counts up to
// that bound, so a longer run reads as the limit, and a report older than all of them cannot
// shorten a run derived over them, and is dropped. The run is what the daemon answers each report
// with (claim.TurnOutcomeResponse's RefusedRun, api's turnOutcome), for the producer's log, and it
// decides nothing: no claim fails on it, and no retry reads it (freshSession). That bound is why a
// claim whose agent works for a week carries a handful of rows rather than a turn-by-turn history
// nothing reads.
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
// counted there. It is what the daemon answers each report with (claim.TurnOutcomeResponse's
// RefusedRun, api's turnOutcome), which the producer logs, and the retry that leaves a session
// behind logs it too (retry); it decides nothing. The run carries across generations, as a resumed
// session's turns follow the ones before them, and a new session starts a fresh run (freshRun).
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
// (dispatch://INFRA-4508), and decides nothing: it is what the daemon answers the report with
// (RefusedRun).
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
// with CauseProviderError, and one the producer classed as OpenAI's cyber-policy refusal
// (RequestTurnOutcome.Code) with CauseCyberPolicy whatever its status, so the workflow holds the
// issue for its architect, and the plugin's reminders end: their record check drops every
// reminder of a failed claim. A refusal that can
// clear, or whose report carries no status, fails nothing, and the reminders keep their pace; it
// counts in the claim's one run of provider-error turns (ProviderErrorRun) beside refused prompts,
// so the project's controller is told once when that run reaches providerErrorRunTold.
//
// A clean report ends that run whether or not it is marked unprompted (providerErrorRunOver), as
// a clean prompt result does: the provider answered that turn either way. A turn the plugin reads
// as a prompt's can have no prompt result of its own — the run after one that started in the
// window between Oh My Pi's acceptance of a prompt and its `input` (that run itself is reported
// unprompted), or a run after a prompt the host refused as busy
// (packages/pi-legion/src/turn-outcome.ts) — and a clean such run that ended nothing would leave
// the run counting across a turn the provider took. It ends the run only when it is the newest
// report the window holds (newestTurnOutcome): a clean report that reaches the daemon after a later
// turn's report is of a turn before that one, and ends no later run whose report has arrived. A
// prompt's result never enters the window, though, so a clean report that arrives after a later
// prompt's error result, and before that prompt's own report, still ends the run that error began.
// The reverse holds too: a clean prompt result ends the run with no such guard (promptResulted), so
// one that arrives after a later plugin turn's refused report was counted ends the run that refusal
// began. Either way the controller's notice comes later, or not at all for a run of exactly six,
// and no claim fails or is held for it. A refused report marked prompted is still judged by
// nothing: its prompt's result counts it. The same two bounds hold for every judgment: new to the
// window, and of the generation the claim runs now.
func turnOutcome(m *Machine, ctx context.Context, ev Event) error {
	r := ev.(RequestTurnOutcome)
	m.turnOutcomeFailed = false
	o := TurnOutcome{Generation: r.Generation, Sequence: r.Sequence, Refused: r.Refused, Message: r.Message}
	kept := m.recordTurnOutcome(o)
	m.log.Info("supervise: a turn ended", "generation", r.Generation, "sequence", r.Sequence,
		"refused", r.Refused, "httpStatus", r.HTTPStatus, "code", r.Code, "unprompted", r.Unprompted,
		"refusedRun", m.claim.RefusedRun())
	switch {
	case !kept || r.Generation != m.claim.Generation:
	case !r.Refused:
		if m.newestTurnOutcome(o) {
			m.providerErrorRunOver("supervise: a turn ended with no provider error; the run of provider errors is over",
				"generation", r.Generation, "sequence", r.Sequence, "unprompted", r.Unprompted)
		}
	case r.Unprompted:
		return m.unpromptedTurnEnded(ctx, r)
	}
	return m.persist(ctx)
}

// newestTurnOutcome is whether o is the newest report the claim's window holds, in the order the
// turns ran (compareTurnOutcomes): no report of a later turn has reached the daemon before it.
func (m *Machine) newestTurnOutcome(o TurnOutcome) bool {
	window := m.claim.TurnOutcomes
	return len(window) > 0 && compareTurnOutcomes(window[len(window)-1], o) == 0
}

// freshSession is whether a retry starts a new session rather than relaunching the recorded one,
// and why, for the line it logs: only when the retry asks for one, with `legion claims retry
// --fresh-session` or the controller's fresh_session, since a new session is an operator's or the
// controller's choice (dispatch://INFRA-956's Design). Every other retry — its architect's
// retry_or_escalate, the next start of its role, an operator's retry that asks for nothing —
// resumes the recorded session with fresh budgets, whatever the claim's run of refused turns
// (dispatch://INFRA-4508's R5). That run is no sign that the session is bad: a refusal that cannot
// clear fails the claim on the turn it ends, and holds the issue, so a run is mostly refusals that
// can clear or carry no status, such as a team limit's 429, which refuses every session of the
// team alike. A claim failed on a refusal that cannot clear resumes too; if its session is refused
// again, it fails again at once.
func freshSession(ev Event) (bool, string) {
	asked := false
	switch ev := ev.(type) {
	case RequestRetry:
		asked = ev.FreshSession
	case RequestOperatorRetry:
		asked = ev.FreshSession
	}
	if asked {
		return true, "the retry asked for a new session"
	}
	return false, ""
}

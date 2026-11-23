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
// replaces it, so a producer that repeats one changes nothing. No count is kept anywhere: every
// reader derives what it needs from the window as it stands (RefusedRun), which is what makes a
// late report and an in-order one leave the same answer.
//
// The message is the provider's text, so it is kept as the daemon keeps a far side's text
// (storetext.FarSideText: made valid, then read whole by the URL rule) before redact.Runes cuts
// it, so the window in memory is the window the store holds, and the cut cannot leave half a
// credential.
func (m *Machine) recordTurnOutcome(o TurnOutcome) {
	o.Message = redact.Runes(storetext.FarSideText(o.Message), maxRefusalMessage)
	at, found := slices.BinarySearchFunc(m.claim.TurnOutcomes, o, compareTurnOutcomes)
	if found {
		m.claim.TurnOutcomes[at] = o
	} else {
		m.claim.TurnOutcomes = slices.Insert(m.claim.TurnOutcomes, at, o)
	}
	if extra := len(m.claim.TurnOutcomes) - m.turnOutcomeWindow(); extra > 0 {
		m.claim.TurnOutcomes = slices.Delete(m.claim.TurnOutcomes, 0, extra)
	}
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
// it (retry). The words are stored for more than that line: a turn the plugin starts itself, a
// phase reminder among them, has no prompt result, so its report is the daemon's one record of
// what the provider answered that turn, and dispatch://INFRA-4508's R4 is to judge such a turn by
// that report.
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
// refused turns from the window that leaves. It fails no claim, however long the run
// (dispatch://INFRA-4508). A turn the provider refused ended in a provider error, and the result
// of the prompt that turn ran decides what that error does (providerErrored): an error that can
// clear sends the task to the same session again on the ladder, with no attempt limit, and tells
// the project's controller once at providerErrorRunTold turns in a row; one that cannot clear
// fails the claim at once and holds the issue for its architect. A report cannot tell the two
// apart, so it decides neither. The run the report leaves decides only whether a retry starts a
// new session (freshSession).
func turnOutcome(m *Machine, ctx context.Context, ev Event) error {
	r := ev.(RequestTurnOutcome)
	m.recordTurnOutcome(TurnOutcome{Generation: r.Generation, Sequence: r.Sequence, Refused: r.Refused, Message: r.Message})
	m.log.Info("supervise: a turn ended", "generation", r.Generation, "sequence", r.Sequence,
		"refused", r.Refused, "refusedRun", m.claim.RefusedRun(), "freshSessionAt", m.deps.Limits.LaunchFailures)
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

package daemon

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// controllerSessionReplacedLine is the keeper's Warn line at the replacement of a controller
// session older than its age (replaceOld, dispatch://INFRA-2045 R3 and R4): the claim, the old
// session, the requirement and its reason. An operator and a production check read it whole.
const controllerSessionReplacedLine = "controller: the daemon's controller session is older than controller_session_max_age_hours; the keeper replaces it with a fresh session between turns"

// mind is the keeper's look at a controller that runs: ready, working or idle. For a working
// claim, a turn open longer than turnBound is ended (TurnPastBound, dispatch://INFRA-2045 R2); for
// one between turns, an idle session older than maxAge is replaced by a fresh one (replaceOld,
// R3). Each rule is off at 0, and neither acts over the operator's own decisions: a turn is ended
// through the machine's own row for it, and a session is replaced only between turns, with no task
// waiting.
func (k *controllerKeeper) mind(ctx context.Context, m *supervise.Machine) error {
	c := m.Claim()
	now := k.now()
	switch {
	case c.State == supervise.StateWorking:
		// A working claim never reaches replaceOld: the age rule interrupts neither a turn nor a
		// compaction, and a claim working for a compaction with no turn under it is between turns
		// but compacting (replaceOld's NeverHold suspension is refused for a working claim too).
		// The machine is asked only about a turn the claim's times show open (Claim.OpenTurn): a
		// claim working for a compaction with no turn under it has its turn closed (compactStart),
		// and is asked nothing at each look of the compaction. Whether the turn is the one read
		// here is the machine's to say (supervise's Machine.endTurnPastBound).
		start, open := c.OpenTurn()
		if k.turnBound <= 0 || !open || !start.Add(k.turnBound).Before(now) {
			return nil
		}
		return m.Handle(ctx, supervise.TurnPastBound{Claim: c.Token, Bound: k.turnBound, TurnStart: start})
	case c.Pending != nil:
		// A task waiting is work the controller has not done yet; its session is kept until it
		// has, and the next look between turns asks again.
		return nil
	}
	return k.replaceOld(ctx, m, c, now)
}

// replaceOld replaces the controller's idle session with a fresh one where the session is older
// than maxAge (dispatch://INFRA-2045 R3): its start is the time its id records (sessionStarted, a
// UUIDv7), so it survives a daemon restart and a new pod. The keeper suspends the claim as the
// daemon, not as the operator, since a resume by rule refuses an operator's suspension
// (supervise.Claim.RuleMayResume), and between turns (RequestSuspend.BetweenTurns): the machine,
// under its own lock, refuses the suspension unless the claim is ready or idle with no task
// waiting, holds the session it records, has its shim connected to this daemon, and its agent says
// it is not streaming. This keeper's read of the claim says only what the store held: after a
// daemon restart a turn an Envoy message started while the daemon was down is read only from the
// frames the shim replays after its hello, and a task can arrive between this read and the
// suspension. A claim that does not hold its session is one whose session the forget could clear
// only on the Envoy listener's word (mayForget), and whose resume no launch could run, so the
// keeper does not stop it. A refusal keeps the session until the next look; then forgetOld.
func (k *controllerKeeper) replaceOld(ctx context.Context, m *supervise.Machine, c supervise.Claim, now time.Time) error {
	if k.maxAge <= 0 || c.Session == "" {
		return nil
	}
	started, ok := sessionStarted(c.Session)
	if !ok {
		// The rule reads no age off an id of another form, so it cannot act on this session; one
		// line says so, once for each such session, while the rule is on.
		if k.unaged != c.Session {
			k.unaged = c.Session
			k.log.Warn("controller: the controller's session id is no UUIDv7, so controller_session_max_age_hours cannot read its age and the session is not replaced for it",
				"claim", c.Token, "session", c.Session)
		}
		return nil
	}
	if !started.Add(k.maxAge).Before(now) {
		return nil
	}
	why := fmt.Sprintf("the session is older than controller_session_max_age_hours (%s): it started at %s, %s before the keeper's look",
		k.maxAge, started.UTC().Format(time.RFC3339), now.Sub(started).Round(time.Minute))
	suspend := supervise.RequestSuspend{Claim: c.Token, Reason: why, By: supervise.SuspendedByDaemon, NeverHold: true, BetweenTurns: true}
	if err := m.Handle(ctx, suspend); err != nil {
		var refused *supervise.RefusedError
		if errors.As(err, &refused) {
			k.log.Info("controller: the old controller session is not replaced at this look: the claim is not known to be between turns",
				"claim", c.Token, "session", c.Session, "reason", err)
			return nil
		}
		return fmt.Errorf("suspend the controller to replace its old session: %w", err)
	}
	return k.forgetOld(ctx, m, c.Token, c.Session, why, started)
}

// forgetOld is the half of replaceOld after the keeper's suspension: the forget of the old
// session, with the rule's reason the fresh session is told in its first prompt, and the resume
// by rule that launches the fresh agent, in the order api's freshSession follows for a live claim.
// The forget names the daemon's suspension the keeper made (RequestForgetSession.SuspendedBy), and
// the keeper decides on the forget's typed answer alone, reading the claim no second time:
//
//   - Forgotten: one Warn line names the claim, the old session and the reason (R4), and the
//     resume by rule launches the fresh agent.
//   - Forgotten in memory, its write refused (*supervise.ForgetUnwrittenError): the same Warn
//     line, and the write's error, which the keeper's loop logs at Error. The machine writes the
//     dropped session at its next decision, and the next look's resume by rule launches the fresh
//     agent. A daemon restarted before that write reads the old session back, suspended by the
//     daemon, resumes it by rule and replaces it again at its next idle look, so the first line
//     named a replacement that did not happen and a second follows: one log line, for a failed
//     store write and a restart in the same minute.
//   - Not recorded (supervise.ErrForgetSessionNotRecorded): another decision cleared the session
//     between the suspension and the forget — an operator's `legion claims forget-session` — and
//     that is the operator's replacement: one Info line, no R4 line, no error.
//   - Not suspended by the daemon (supervise.ErrForgetNotSuspendedBy): an operator's suspension
//     took the claim over by the time of the forget, in one of the three ways
//     RequestForgetSession.SuspendedBy's doc names, or a resume came between the two. The
//     operator's resume brings back the same session: one Info line, no error.
//   - Any other refusal changed nothing, so the claim still stands suspended by the daemon, and
//     the keeper resumes the old session by rule in this same look, so a suspension the keeper made
//     never leaves the controller stopped. The refusal returns as the error it is. The between-turns
//     suspension held the session's names in the machine (supervise's Machine.betweenTurns), so the
//     forget asks the Envoy listener nothing and is not refused for the hold, and the resume is not
//     either: no branch here waits on an operator.
//
// The old session's transcript stays in its store.
func (k *controllerKeeper) forgetOld(ctx context.Context, m *supervise.Machine, token claim.Token, old, why string, started time.Time) error {
	forgot := m.Handle(ctx, supervise.RequestForgetSession{Claim: token, Session: old, Why: why, SuspendedBy: supervise.SuspendedByDaemon})
	var unwritten *supervise.ForgetUnwrittenError
	switch {
	case forgot == nil, errors.As(forgot, &unwritten):
		k.log.Warn(controllerSessionReplacedLine, "claim", token, "session", old, "requirement", "R3", "reason", why,
			"sessionStartedAt", started)
		if forgot != nil {
			return fmt.Errorf("write the forget of the controller's old session %s: %w", old, forgot)
		}
		return m.Handle(ctx, supervise.RequestRuleResume{Claim: token, Reason: "the keeper replaced the controller's old session"})
	case errors.Is(forgot, supervise.ErrForgetSessionNotRecorded):
		k.log.Info("controller: the old controller session is not replaced by the keeper: another decision cleared it after the keeper's suspension, an operator's forget-session among them",
			"claim", token, "session", old, "reason", forgot)
		return nil
	case errors.Is(forgot, supervise.ErrForgetNotSuspendedBy):
		k.log.Info("controller: the old controller session is not replaced at this look: the claim no longer stands under the keeper's suspension",
			"claim", token, "session", old, "reason", forgot)
		return nil
	}
	resumed := m.Handle(ctx, supervise.RequestRuleResume{Claim: token, Reason: "the keeper's forget of the controller's old session was refused, so the keeper resumes it"})
	return fmt.Errorf("forget the controller's old session %s, refused, so the keeper resumed it by rule in this look (resume: %v): %w", old, resumed, forgot)
}

// sessionStarted is the time an Oh My Pi session id records, which is a UUIDv7: its first 48 bits
// are the milliseconds since the Unix epoch at which the session began. An id of any other form —
// another version, or no UUID at all — says nothing of its age, and answers false.
func sessionStarted(id string) (time.Time, bool) {
	parsed, err := uuid.Parse(id)
	if err != nil || parsed.Version() != 7 {
		return time.Time{}, false
	}
	sec, nsec := parsed.Time().UnixTime()
	return time.Unix(sec, nsec), true
}

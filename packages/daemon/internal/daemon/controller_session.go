package daemon

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

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
		// Whether it is in a turn, and that turn the one read here, is the machine's to say
		// (supervise's endTurnPastBound): it leaves alone a compaction with no turn under it, a
		// turn that ended and a new one.
		if k.turnBound <= 0 || c.LastTurnStart.IsZero() || !c.LastTurnStart.Add(k.turnBound).Before(now) {
			return nil
		}
		return m.Handle(ctx, supervise.TurnPastBound{Claim: c.Token, Bound: k.turnBound, TurnStart: c.LastTurnStart})
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
// (supervise.Claim.RuleMayResume), and with NeverHold, so a turn that began since the keeper's read
// refuses the suspension and the session is kept until the next look; it then forgets the session
// with the rule's reason, which the fresh session is told in its first prompt, and resumes the
// claim by rule, which launches a fresh agent. That is the order api's freshSession follows for a
// live claim. One Warn line names the claim, the old session and the reason (R4); the old
// session's transcript stays in its store. A forget the machine refuses leaves the claim
// suspended by the daemon, which this keeper's next look resumes by rule on the old session.
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
	suspend := supervise.RequestSuspend{Claim: c.Token, Reason: why, By: supervise.SuspendedByDaemon, NeverHold: true}
	if err := m.Handle(ctx, suspend); err != nil {
		var refused *supervise.RefusedError
		if errors.As(err, &refused) {
			k.log.Info("controller: the old controller session is not replaced at this look: the claim moved before the keeper's suspension",
				"claim", c.Token, "session", c.Session, "reason", err)
			return nil
		}
		return fmt.Errorf("suspend the controller to replace its old session: %w", err)
	}
	if err := m.Handle(ctx, supervise.RequestForgetSession{Claim: c.Token, Session: c.Session, Why: why}); err != nil {
		return fmt.Errorf("forget the controller's old session %s: %w", c.Session, err)
	}
	k.log.Warn(controllerSessionReplacedLine, "claim", c.Token, "session", c.Session, "requirement", "R3", "reason", why,
		"sessionStartedAt", started)
	return m.Handle(ctx, supervise.RequestRuleResume{Claim: c.Token, Reason: "the keeper replaced the controller's old session"})
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

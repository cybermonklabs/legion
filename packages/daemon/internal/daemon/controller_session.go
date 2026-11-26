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
// waiting, its shim has connected to this daemon, and its agent says it is not streaming. This
// keeper's read of the claim says only what the store held: after a daemon restart a turn an Envoy
// message started while the daemon was down is read only from the frames the shim replays after
// its hello, and a task can arrive between this read and the suspension. A refusal keeps the
// session until the next look. The keeper then forgets the session with the rule's reason, which
// the fresh session is told in its first prompt, naming the daemon's suspension it made
// (RequestForgetSession.SuspendedBy). A claim an operator suspended by the time of the forget
// refuses it, and the operator's resume brings back the same session: an operator's suspension
// held through a turn whose stop the runtime refused, which the keeper's suspension ran as the
// operator's; one decided before the keeper's, which its suspension answered with nil; and one
// decided after it, which takes the daemon's suspension over on rank. The first two are settled
// before the keeper's suspension returns; only the forget's own check closes the third. So does
// a resume that came between the two, which leaves the claim running and the forget ignored. The
// keeper reads the claim after a refusal and logs it at Info only where the claim no longer
// stands suspended by the daemon; every other refusal — the Envoy listener's answer about the
// role, which the forget asks before it clears a session the claim does not hold (mayForget) —
// returns as the error it is, which the keeper's loop logs at Error. Last it resumes the claim by
// rule, which launches a fresh agent. That is the order api's freshSession follows for a live
// claim.
//
// One Warn line names the claim, the old session and the reason (R4), once the machine has dropped
// the session. Where the forget's write fails after that, the line is written all the same: the
// machine keeps the dropped session in memory and writes it at its next decision, and the next
// look's resume by rule launches the fresh agent. A daemon restarted before that write reads the
// old session back, suspended by the daemon, resumes it by rule and replaces it again at its next
// idle look, so the first line named a replacement that did not happen and a second follows: that
// costs one log line, and needs a failed store write and a restart in the same minute. The old
// session's transcript stays in its store. A forget the machine refuses changes nothing: the claim
// stays as the refusal found it, and a daemon's suspension is resumed by rule on the old session
// at this keeper's next look.
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
	forgot := m.Handle(ctx, supervise.RequestForgetSession{Claim: c.Token, Session: c.Session, Why: why, SuspendedBy: supervise.SuspendedByDaemon})
	if m.Claim().HoldsSession(c.Session) {
		if after := m.Claim(); after.State != supervise.StateSuspended || after.SuspendedBy != supervise.SuspendedByDaemon {
			k.log.Info("controller: the old controller session is not replaced at this look: the claim no longer stands under the keeper's suspension",
				"claim", c.Token, "session", c.Session, "state", after.State, "suspendedBy", after.SuspendedBy, "reason", forgot)
			return nil
		}
		return fmt.Errorf("forget the controller's old session %s: %w", c.Session, forgot)
	}
	k.log.Warn(controllerSessionReplacedLine, "claim", c.Token, "session", c.Session, "requirement", "R3", "reason", why,
		"sessionStartedAt", started)
	if forgot != nil {
		return fmt.Errorf("write the forget of the controller's old session %s: %w", c.Session, forgot)
	}
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

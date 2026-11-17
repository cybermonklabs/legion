package daemon

import (
	"context"
	"errors"
	"fmt"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// holdRecordedSessions and suspendSessionConflicts run once at boot, before any claim is relaunched
// (plan E's t-e-54). Each name of a session (supervise.Hold) is held to the
// first claim of the project that recorded it (supervise's holdRecorded), and a claim records a
// name without holding it only where a daemon from before the hold ran on this database: before
// the claim_sessions migration, or rolled back to an earlier release after it, which applies only
// the migrations it has and never writes the table. So the boot first holds every recorded name no
// row holds yet (store.HoldRecordedSessions, the migration's backfill again), before restore builds
// the first machine, since a restored machine's timer could otherwise take such a name for its
// claim first (start); and then, once the machines exist, logs at Error, and suspends,
// every claim whose current session records a name it does not hold (store.SessionConflicts): one
// held for no claim, which several claims recorded and nothing says which first, and one held for
// another claim, which a rolled-back daemon let this claim take. Each such claim is suspended once,
// whatever number of names it shares, by the operator's source so that no rule of the daemon's
// resumes it; relaunched, it would be refused before it starts, again and again until its launch
// budget ran out, and a claim that took another's name in the window would keep the grants it
// adopted then. On a database only daemons with the hold have run, both steps find nothing.
//
// A name only an abandoned session records suspends nothing, and is logged at Warn alone: nothing
// resumes or enrolls an abandoned session (start resumes the current file, ensureEnrolled enrolls
// the current session), and the operator's way out of a conflict is to give the claim that lost it
// a new session, which makes the lost one its abandoned session. Counting it would suspend that
// claim again at every boot (review R2-2 of #4220).
//
// The steps an operator takes: give each suspended claim that is not the rightful holder a new
// session (`legion claims forget-session`, then `legion claims resume`); and for a name held for no
// claim, record the rightful holder with the line's statement and resume that claim. A name held
// for another claim stays that claim's: the line gives no statement for it, since the holder's
// machine keeps the answer it was given (holdRecorded) and moving the row would not move it.
//
// A claim whose state the transition table refuses a suspension (today queued, launch_uncertain,
// failed and retired) is named and left as it is: a retired claim launches nothing, and a queued,
// uncertain or failed one that launches again is refused each time and fails at its launch budget.
// The table decides (a *supervise.RefusedError), so a state it later lets a suspension reach is
// suspended here with no change to this list. A store that refuses a read,
// a write or a suspension refuses the boot, naming what failed, as the controller's boot stop does
// (stopLaunchedController): the claims it names would otherwise be relaunched into that refusal. So
// does a stop the runtime refuses, and a run of stops past the boot's budget: the suspensions run
// one after another on that budget. A suspension the claim holds for later is no failure: a
// working claim's is held for its turn and stops the process at the operator's stop timeout
// (holdSuspension), and a launching claim's is held until its agent is ready, which an agent
// refused its registration never is, so its registration deadline suspends it
// (registrationDeadline). Neither is relaunched meanwhile, and a restart before either completes
// asks again.
//
// The settle statement is a statement an operator runs, and a session's names are the agent's own
// text, so each is written into it as a SQL literal (sqlLiteral), never as it came.
func (s *supervision) holdRecordedSessions(ctx context.Context) error {
	held, err := s.supervisor.store.HoldRecordedSessions(ctx, s.plan.project)
	if err != nil {
		return err
	}
	if held > 0 {
		s.log.Warn("supervise: held session names claims recorded while a daemon before the hold ran on this database",
			"names", held)
	}
	return nil
}

// suspendSessionConflicts is the boot's second step (holdRecordedSessions).
func (s *supervision) suspendSessionConflicts(ctx context.Context) error {
	conflicts, err := s.supervisor.store.SessionConflicts(ctx, s.plan.project)
	if err != nil {
		return err
	}
	suspended := map[claim.Token]bool{}
	for _, c := range conflicts {
		holder := string(c.Holder)
		if holder == "" {
			holder = "none"
		}
		if len(c.Claims) == 0 {
			s.log.Warn("supervise: a session name only an abandoned session records is held for another claim or for none; nothing resumes or enrolls an abandoned session, so no claim is suspended",
				"kind", c.Hold.Kind, "name", c.Hold.Name, "holder", holder, "abandoned", c.Abandoned)
			continue
		}
		line := []any{"kind", c.Hold.Kind, "name", c.Hold.Name, "holder", holder, "claims", c.Claims}
		if len(c.Abandoned) > 0 {
			line = append(line, "abandoned", c.Abandoned)
		}
		if c.Holder == "" {
			line = append(line, "settle", "update claim_sessions set claim = '<token>' where project = "+supervise.SQLLiteral(s.plan.project)+
				" and kind = "+supervise.SQLLiteral(string(c.Hold.Kind))+" and name = "+supervise.SQLLiteral(c.Hold.Name))
		}
		s.log.Error("supervise: a session name is recorded by claims that do not hold it; each of them this daemon supervises is suspended until an operator gives it a new session, or records it as the rightful holder of a name held for none, and resumes it", line...)
		for _, token := range c.Claims {
			m, ok := s.supervisor.Machine(token)
			if !ok || suspended[token] {
				continue
			}
			suspended[token] = true
			err := m.Handle(ctx, supervise.RequestSuspend{
				Claim: token, Reason: fmt.Sprintf("session name not its own: %s %s", c.Hold.Kind, c.Hold.Name), By: supervise.SuspendedByOperator,
			})
			var refused *supervise.RefusedError
			switch {
			case errors.As(err, &refused):
				s.log.Warn("supervise: a claim recording a session name it does not hold is in no state a suspension reaches; it is left as it is",
					"claim", token, "kind", c.Hold.Kind, "name", c.Hold.Name, "state", refused.State)
			case errors.Is(err, supervise.ErrSuspendHeld):
				s.log.Warn("supervise: the suspension of a claim recording a session name it does not hold waits for its turn or its launch",
					"claim", token, "kind", c.Hold.Kind, "name", c.Hold.Name)
			case err != nil:
				return fmt.Errorf("suspend %s, which records the %s %s it does not hold: %w", token, c.Hold.Kind, c.Hold.Name, err)
			}
		}
	}
	return nil
}

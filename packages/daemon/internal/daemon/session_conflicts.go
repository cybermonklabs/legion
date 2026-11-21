package daemon

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/store"
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
// adopted then. The suspension sends no clean stop of the claim's Envoy session (supervise's
// sessionGone): the listener's registration under that id can be the live process of the claim
// that holds it, which the stop would delete. On a database only daemons with the hold have run,
// both steps find nothing.
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
// machine keeps the answer it was given (holdRecorded) and moving the row would not move it. A
// name no row can hold (store.SessionConflict.Unholdable: past claim.MaxSessionNameBytes, or a
// session file that is not a clean path) gets no statement either: every claim recording it is
// suspended, and each is given a new session.
//
// A claim whose state the transition table refuses a suspension (today queued, launch_uncertain,
// failed and retired) is named and left as it is: a retired claim launches nothing, and a queued,
// uncertain or failed one that launches again is refused each time and fails at its launch budget.
// The table decides (a *supervise.RefusedError), so a state it later lets a suspension reach is
// suspended here with no change to this list. A suspension that fails for any other reason is
// logged at Error naming the claim, and the boot goes on: a stop the runtime refuses leaves the
// process running, and a write the store refuses after the stop leaves the process stopped and
// the claim not yet recorded suspended (review round 13 of #4220, note 37). Either way the claim
// is contained, and where its process runs on it is not stopped: its hold refuses every relaunch,
// resume and new enrollment of it before anything starts; the enrollment its process holds is
// revoked once the broker confirms the revocation, the claim keeping it recorded, handed to no
// connection, and asking again at each observation until then, and recorded no longer, logged at
// Error, where the broker refuses the revocation for good, while the process renews it until it
// stops (revokeUnheld); and the daemon publishes nothing to its Envoy role, whatever the claim's
// state, or, for the daemon's own controller under `controller: daemon`, to the controller topic,
// and sends that process no task and no compaction (holdRecorded, ensureEnrolled, outbox.publish,
// sendPending, compactStart). The process runs on under the session until it is stopped, and its
// session claimed the claim's Envoy role: the daemon could free the role, but the process claims a
// free role again at its next heartbeat. So the boot asks the listener to withhold the role from
// that session (withholdRole, notify.RoleWithholds): the listener frees the session's claim of the
// role and refuses each claim of it the session makes, soft or hard, so no message to the role
// reaches a process of that session, from any sender, another claim's process registered under the
// same id included. The withhold stands until the claim holds that session again, when the boot
// or the operator's resume lifts it (liftRoleWithholds); a claim given a new session records the
// old one no longer, so its role stays withheld from the old session, which no process of the
// claim runs under again. While it stands the listener names no holder for the role, so the clear
// of the session, which first asks the listener who holds the claim's role (supervise's
// mayForget), is not refused for that session.
//
// A withhold the listener refuses, as it refuses every caller but the daemon's own broker token,
// or one that does not reach it, is logged at Error, and the claim is contained as above without
// it: a message another session sends to that role reaches the session's subscriber until the
// session ends. Once no process registers the session id, the listener frees the role at the role
// reaper's first pass after the session's record is gone and the role claim's last renewal is a
// live window old: the record leaves the session bucket at the bucket's maximum age
// (`ENVOY_SESSION_MAX_AGE`, five minutes on a bucket created with it unset) or reads gone at the
// gone horizon (an hour) where that age is longer, and the reaper runs every
// `ENVOY_ROLE_CLAIM_REAP_INTERVAL`, five minutes by default, so the role is free at most
// `max(min(ENVOY_SESSION_MAX_AGE, ENVOY_SESSION_GONE_HORIZON), ENVOY_SESSION_LIVE_WINDOW) +
// ENVOY_ROLE_CLAIM_REAP_INTERVAL` after the last registration (ten minutes with the defaults,
// about 65 minutes where the bucket keeps records past the gone horizon), and later across a
// listener restart or a reaper read that fails (supervise.roleFreedBound). That holds only while
// no other live process registers the same session id: where the name is held for another claim
// whose live process runs under the same id, as after a rollback, the listener keeps the role
// with that session while it lives, and the role's messages reach that process. That is so for
// every claim this step names whose session another claim's live process runs under, suspended
// here, or not stopped and its withhold refused. The daemon withholds its own messages from such
// a claim's role only while the claim records that session, so the one decision that would open
// the role topic, the clear of that session, first asks the listener who holds the claim's role
// and is refused while the listener names that session in any state (supervise's mayForget). The
// operator stops the process under the session: it suspends the claim holding the session if that
// claim runs, and otherwise only the wait helps, since the process may be the contained claim's
// own. It forgets once the listener names no holder or another session, within that bound of that
// process's last registration, resumes the contained claim, and resumes the claim it suspended
// only once the listener names the contained claim's new session as its role's holder: a holder
// resumed before then can take that role from a transcript the two claims share (review round 16
// of #4220, #59-#61). The contained claim takes no forget while it is live, so the operator stops
// it (`legion claims suspend`, once its runtime answers, or a stop of its pod or pane by hand)
// before the steps above, which need a daemon that serves (review rounds 11 to 18 of #4220). A
// statement that makes such a claim the holder of its session writes no claim, and the
// publication of an issue's role holders follows claim writes, so Dispatch is shown that claim as
// its role's holder at the next boot, or once the claim is suspended and resumed. A boot whose
// budget runs out on the suspensions still fails, since every later step would too. A store that
// refuses the conflicts' read refuses the boot, naming what failed, as the controller's boot stop
// does (stopLaunchedController). A suspension the claim holds for later is no failure: a working
// claim's is held for its turn and stops the process at the operator's stop timeout
// (holdSuspension), and a launching claim's is held until its agent is ready, which an agent
// refused its registration never is, so its registration deadline suspends it
// (registrationDeadline). Neither is relaunched meanwhile, and a restart before either completes
// asks again.
//
// The lift is asked of each claim holding a name of its current session that another claim's
// current or abandoned session records (liftRoleWithholds), lifted from that session. A withhold
// is written only for a claim recording a name it does not hold, and such a claim comes to hold
// the name in one of two ways: an operator records it as the name's holder with the settle
// statement, when the claims it shared the name with record it still; or the name leaves every
// other claim's records, by a forget and a release of their names, and the boot's hold gives it to
// the one claim recording it (store.HoldRecordedSessions). The conflict read names the claims of
// the first way with no record of what earlier boots withheld, and a project with no conflict
// sends the listener nothing. A lift of a role not withheld answers 204 and changes nothing. The
// boot asks it of every such claim after its suspensions, and the operator's resume of a claim
// asks it of that claim alone, before the claim's machine is asked to relaunch it and whatever
// that answers, though not where the project's pause refuses the resume: a claim the boot could
// not stop is not suspended, so its resume is refused, though its process is the session's
// rightful one once the name is settled. A claim of the second way, or one settled after the name
// left the other claims' records, is named by no conflict, so its withhold stands; giving it a new
// session (`legion claims forget-session`, then `legion claims resume`) lets its process claim the
// role under that session. A lift the listener refuses, or that does not reach it, is logged at
// Error, and the next boot or resume asks again.
//
// The settle statement is a statement an operator runs, and a session's names are the agent's own
// text, so each is written into it as an escape string literal (supervise.SQLLiteral), never as it
// came, which reads the same whatever standard_conforming_strings the operator's session sets. It
// moves only a name still held for no claim, and only to one of the claims the line names, so a
// second run answers UPDATE 0 and a token that names none of them moves nothing.
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

// suspendSessionConflicts is the boot's second step (holdRecordedSessions), and the boot's pass of
// the role withholds: it withholds the role of each claim it could not stop, and lifts that of
// each claim that holds its session again (liftRoleWithholds).
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
		switch {
		case c.Unholdable:
			s.log.Error("supervise: a session name this daemon cannot hold, past the bound or not a clean path, is recorded by claims; each of them this daemon supervises is suspended until an operator gives it a new session",
				append(line, "bytes", len(c.Hold.Name))...)
		default:
			if c.Holder == "" {
				claims := make([]string, len(c.Claims))
				for i, token := range c.Claims {
					claims[i] = supervise.SQLLiteral(string(token))
				}
				line = append(line, "settle", "update claim_sessions set claim = '<token>' where project = "+supervise.SQLLiteral(s.plan.project)+
					" and kind = "+supervise.SQLLiteral(string(c.Hold.Kind))+" and name = "+supervise.SQLLiteral(c.Hold.Name)+" and claim = ''"+
					" and '<token>' in ("+strings.Join(claims, ", ")+")")
			}
			s.log.Error("supervise: a session name is recorded by claims that do not hold it; each of them this daemon supervises is suspended until an operator gives it a new session, or records it as the rightful holder of a name held for none, and resumes it", line...)
		}
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
			case err == nil:
			case errors.As(err, &refused):
				s.log.Warn("supervise: the suspension of a claim recording a session name it does not hold was refused; it is left as it is",
					"claim", token, "kind", c.Hold.Kind, "name", c.Hold.Name, "state", refused.State, "reason", refused.Reason, "error", err)
			case errors.Is(err, supervise.ErrSuspendHeld):
				s.log.Warn("supervise: the suspension of a claim recording a session name it does not hold waits for its turn or its launch",
					"claim", token, "kind", c.Hold.Kind, "name", c.Hold.Name)
			case ctx.Err() != nil:
				return fmt.Errorf("suspend %s, which records the %s %s it does not hold: %w", token, c.Hold.Kind, c.Hold.Name, err)
			default:
				s.log.Error("supervise: the suspension of a claim recording a session name it does not hold failed; it is left as it is, contained: its hold refuses every relaunch, resume and new enrollment of it, its process's enrollment is revoked once the broker confirms the revocation (or recorded no longer if the broker refuses that for good), and the daemon publishes nothing to its role and sends it no task and no compaction; where the runtime refused the stop the process runs on until it is stopped (legion claims suspend, or a stop of its pod or pane by hand), and the Envoy listener is asked to withhold its role from its session; the next boot asks again",
					"claim", token, "kind", c.Hold.Kind, "name", c.Hold.Name, "error", err)
				s.withholdRole(ctx, token, m.View().Claim.Session)
			}
		}
	}
	s.liftRoleWithholds(ctx, conflicts, "")
	return nil
}

// withholdRole asks the Envoy listener to withhold the role of the claim of token from session,
// the session that claim records and does not hold, after the boot's suspension of the claim
// failed (suspendSessionConflicts, notify.RoleWithholds). The listener frees the session's claim
// of the role and refuses each claim of it the session makes, so the process the runtime would
// not stop is sent nothing published to the role. Where the failure was a write the store refused
// after the stop, no process runs under the session, and the withhold keeps the role from it as
// well. A withhold refused, or one that does not reach the listener, is logged at Error and leaves
// the claim as #4220 leaves it, contained; the next boot asks again. A claim recording no session
// id has registered with no listener under one, so nothing is asked.
func (s *supervision) withholdRole(ctx context.Context, token claim.Token, session string) {
	if s.withholds == nil || session == "" {
		return
	}
	if err := s.withholds.WithholdRole(ctx, string(token), session); err != nil {
		s.log.Error("supervise: the Envoy listener refused to withhold the role of a claim the boot could not stop from the session it does not hold, or could not be asked; the claim stays contained, and its process keeps the role its session claimed until it is stopped, so a message another session sends to the role reaches it; the next boot asks again",
			"role", token, "session", session, "error", err)
		return
	}
	s.log.Info("supervise: the Envoy listener withholds the role of a claim the boot could not stop from the session it does not hold, until the claim holds that session again",
		"role", token, "session", session)
}

// liftRoleWithholds asks the Envoy listener to lift the withhold of the role of each claim that
// holds its session again from that session (notify.RoleWithholds.LiftRoleWithhold), from
// conflicts, the boot's read of every name claims record without holding it
// (store.SessionConflicts): each claim that holds such a name, holds every name of its own current
// session, and records the name in that session. The package comment says why those are every
// claim a withhold can have been written for that holds its session now. only, when set, is the
// one claim to lift, the claim the operator resumes (liftRoleWithholdOf); the boot lifts every
// such claim. A lift the listener refuses, or that does not reach it, is logged at Error.
func (s *supervision) liftRoleWithholds(ctx context.Context, conflicts []store.SessionConflict, only claim.Token) {
	if s.withholds == nil {
		return
	}
	unheld := map[claim.Token]bool{}
	for _, c := range conflicts {
		for _, token := range c.Claims {
			unheld[token] = true
		}
	}
	asked := map[claim.Token]bool{}
	for _, c := range conflicts {
		holder := c.Holder
		if holder == "" || unheld[holder] || asked[holder] || (only != "" && holder != only) {
			continue
		}
		m, ok := s.supervisor.Machine(holder)
		if !ok {
			continue
		}
		held := m.View().Claim
		if held.Session == "" || !slices.Contains(supervise.RecordedHolds(held.Session, held.SessionFile), c.Hold) {
			continue
		}
		asked[holder] = true
		if err := s.withholds.LiftRoleWithhold(ctx, string(holder), held.Session); err != nil {
			s.log.Error("supervise: the Envoy listener refused to lift the withhold of the role of a claim that holds its session again, or could not be asked; its process cannot claim the role until a lift lands, and the next boot or resume of the claim asks again",
				"role", holder, "session", held.Session, "error", err)
			continue
		}
		s.log.Info("supervise: the Envoy listener lifted the withhold of the role of a claim that holds its session again",
			"role", holder, "session", held.Session)
	}
}

// liftRoleWithholdOf lifts the withhold of the role of the claim of token from its session where
// the claim holds that session again (liftRoleWithholds), for the operator's resume of the claim
// (api.Options.LiftRoleWithhold), which calls it before the claim's machine is asked. A claim
// recording no session asks nothing. A store that does not answer the conflicts' read lifts
// nothing, logged at Warn: the resume goes on, and the next boot asks again.
func (s *supervision) liftRoleWithholdOf(ctx context.Context, token claim.Token) {
	if s.withholds == nil {
		return
	}
	if m, ok := s.supervisor.Machine(token); !ok || m.View().Claim.Session == "" {
		return
	}
	conflicts, err := s.supervisor.store.SessionConflicts(ctx, s.plan.project)
	if err != nil {
		s.log.Warn("supervise: the store did not answer which session names claims record without holding them, so the resume lifts no withhold of the claim's Envoy role; the next boot asks again",
			"claim", token, "error", err)
		return
	}
	s.liftRoleWithholds(ctx, conflicts, token)
}

package requests

import (
	"context"
	"log/slog"
	"time"

	"github.com/sjawhar/envoy/internal/broker/enroll"
	"github.com/sjawhar/envoy/internal/broker/machine"
	"github.com/sjawhar/envoy/internal/broker/oauthstore"
)

// SweepRecoveredMessage is logged, at INFO, by Tick for one of its sweeps on the first run of that
// sweep that succeeds after one that failed, followed by sweep=<the sweep's name in sweeps>. One
// message names the recovery of all seven sweeps, the sweep field saying which, so a reader
// filters one text for every recovery.
//
// A sweep's failure line is its own message in sweeps, at WARN, followed by sweep= and error=. It
// is written on the first failure of a run of failures and not again until this line has ended
// that run, as enroll.ServingClock.Run logs its beats: a database that stays away for an hour
// writes two lines for each sweep, not one every BROKER_SWEEP_SECONDS.
const SweepRecoveredMessage = "broker sweeper: a sweep that failed succeeded again"

// sweep is one of the seven sweeps Tick runs, in Tick's order: an index into sweeps and into
// Sweeper.failing.
type sweep int

const (
	sweepLapsedEnrollments sweep = iota
	sweepDormantEnrollments
	sweepPendingRequests
	sweepPendingMachineLogins
	sweepSpentOAuthState
	sweepExpiredBrowserSessions
	sweepEndedGrantPlaceholders
)

// sweeps names each sweep: name is the sweep field of both of its lines, and failed the message
// of its failure line.
var sweeps = [...]struct{ name, failed string }{
	sweepLapsedEnrollments:      {"lapsed-enrollments", "broker sweeper: end lapsed enrollments"},
	sweepDormantEnrollments:     {"dormant-enrollments", "broker sweeper: end dormant enrollments"},
	sweepPendingRequests:        {"pending-requests", "broker sweeper: expire pending requests"},
	sweepPendingMachineLogins:   {"pending-machine-logins", "broker sweeper: expire pending machine logins"},
	sweepSpentOAuthState:        {"spent-oauth-state", "broker sweeper: delete spent OAuth state"},
	sweepExpiredBrowserSessions: {"expired-browser-sessions", "broker sweeper: delete expired browser sessions"},
	sweepEndedGrantPlaceholders: {"ended-grant-placeholders", "broker sweeper: delete the placeholders of grants that have ended"},
}

// Sweeper is the one thing that ends what no human decides: every tick it ends the enrollments
// whose lease lapsed (revoking their grants and cancelling their pending requests), ends the
// dormant enrollments whose carry window passed, expires overdue
// pending agent_secret requests (waking each one's owner) and overdue pending machine logins,
// deletes spent OAuth state (oauthstore.Store.Sweep), deletes expired browser sessions, and
// deletes the airlock placeholders of grants that have ended — all
// read fresh from Postgres, never from memory, so a restart resumes exactly where the rows are. A
// lease that ran out while no broker was serving is not lapsed (enroll.Service.EndLapsed), so the
// first sweep after an outage never ends it for that: the broker's serving clock
// (enroll.ServingClock) gives it the time back instead.
type Sweeper struct {
	Enrollments   *enroll.Service
	Machine       *Machine
	MachineLogins *machine.Service
	OAuth         *oauthstore.Store
	Interval      time.Duration
	// SessionCarry is BROKER_SESSION_CARRY_SECONDS: how long a dormant enrollment's grants and
	// pending requests wait for the next enrollment of its session name to take them before this
	// sweep revokes them (enroll.Service.EndCarried). 0 ends every dormant enrollment the next
	// tick finds, which is the broker's behaviour before the carry.
	SessionCarry time.Duration
	Wake         func(ctx context.Context, enrollmentID, requestID, state string)
	// failing is, for each sweep, whether its last run failed (settle), which is the whole of
	// what this Sweeper remembers between ticks: the rows are read fresh every tick.
	failing [len(sweeps)]bool
}

func (s *Sweeper) Run(ctx context.Context) {
	ticker := time.NewTicker(s.Interval)
	defer ticker.Stop()
	for {
		s.Tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Tick runs one sweep: lapsed enrollments, then the dormant enrollments whose carry window
// passed, then agent_secret requests, then machine logins, then
// spent OAuth state, then expired browser sessions, then the airlock placeholders of grants that
// have ended. Each
// sweep's own failure does not stop the others — they share nothing but the ticker — and is
// logged when it starts a run of failures, not at every tick of that run (settle).
// Lapsed enrollments go first, so a request whose session is gone is cancelled with it rather
// than expired and its owner woken; the carry sweep follows them, so a lapse that leaves an
// enrollment dormant with the carry turned off (SessionCarry 0) is finished in the same tick.
// One Sweeper's ticks run one at a time, as Run's do: Tick reads and writes the state settle
// keeps, under no lock.
func (s *Sweeper) Tick(ctx context.Context) {
	ended, err := s.Enrollments.EndLapsed(ctx)
	for _, e := range ended {
		slog.Info("broker sweeper: ended an enrollment whose lease lapsed", "enrollment_id", e.ID, "kind", e.Kind,
			"runtime_id", e.RuntimeID, "slot", e.Slot, "lease_expired_at", e.LeaseExpires,
			"grants_revoked", e.GrantsRevoked, "requests_cancelled", e.RequestsCancelled, "left_dormant", e.Dormant)
	}
	s.settle(ctx, sweepLapsedEnrollments, err)
	carried, err := s.Enrollments.EndCarried(ctx, s.SessionCarry)
	for _, e := range carried {
		slog.Info("broker sweeper: ended a dormant enrollment whose carry window passed", "enrollment_id", e.ID,
			"kind", e.Kind, "runtime_id", e.RuntimeID, "slot", e.Slot, "session_id", e.SessionID,
			"dormant_since", e.DormantSince, "grants_revoked", e.GrantsRevoked, "requests_cancelled", e.RequestsCancelled)
	}
	s.settle(ctx, sweepDormantEnrollments, err)
	now := time.Now()
	expired, err := s.Machine.expirePending(ctx, now)
	s.settle(ctx, sweepPendingRequests, err)
	for _, r := range expired {
		if s.Wake != nil {
			s.Wake(ctx, r.EnrollmentID, r.ID, "expired")
		}
	}
	s.settle(ctx, sweepPendingMachineLogins, s.MachineLogins.ExpirePending(ctx, now))
	s.settle(ctx, sweepSpentOAuthState, s.OAuth.Sweep(ctx))
	deleted, err := s.Machine.Store.DeleteExpiredBrowserSessions(ctx, now)
	s.settle(ctx, sweepExpiredBrowserSessions, err)
	if err == nil && deleted > 0 {
		slog.Info("broker sweeper: deleted expired browser sessions", "count", deleted)
	}
	// The placeholders of a revoked or expired grant, which the table would otherwise keep for
	// as long as the grant row lives. Deleting them is safe at the moment the grant ends,
	// whatever still holds one: every read of a placeholder checks its grant's liveness first
	// (AirlockConnection), so such a placeholder already opens nothing, and the only thing the
	// rows still do is answer a 407 a little more slowly. A deleted grant takes its rows with
	// it through the foreign key's on delete cascade; this is for the far commoner grant that
	// ends without being deleted.
	deleted, err = s.Machine.deleteSpentPlaceholders(ctx)
	s.settle(ctx, sweepEndedGrantPlaceholders, err)
	if err == nil && deleted > 0 {
		slog.Info("broker sweeper: deleted the placeholders of grants that have ended", "count", deleted)
	}
}

// settle logs a change in one sweep's state, keyed on whether its last run failed and never on
// an error's text, as enroll.ServingClock.Run logs its beats: the sweep's failure line at the
// first failure of a run, SweepRecoveredMessage at the first success after one, and nothing for a
// run that goes on as the one before it went. A run that ends once ctx has, the broker shutting
// down, says nothing of the database: it logs nothing and leaves the state as it was.
func (s *Sweeper) settle(ctx context.Context, sw sweep, err error) {
	switch {
	case ctx.Err() != nil:
	case err != nil && !s.failing[sw]:
		slog.Warn(sweeps[sw].failed, "sweep", sweeps[sw].name, "error", err)
		s.failing[sw] = true
	case err == nil && s.failing[sw]:
		slog.Info(SweepRecoveredMessage, "sweep", sweeps[sw].name)
		s.failing[sw] = false
	}
}

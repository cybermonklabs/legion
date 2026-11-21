package textversions

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/sessionanswers"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// holdOptions are the fill's hold's TTL, renewal and margin: the delivery intakes' own
// (delivery.intakeHoldTTL, intakeHoldRenew and intakeHoldMargin). The hold lasts 15 s on the
// database's clock after each renewal, is renewed every 5 s, and reports itself lost 2 s before it
// lapses, so the holder stops writing before any standby can take it. The renewal is also the
// bound on the holder's own statements of the hold: the take and the release.
var holdOptions = duty.HoldOptions{TTL: 15 * time.Second, Renew: 5 * time.Second, Margin: 2 * time.Second}

const (
	// standbyRetry is how often a process that does not hold the fill's hold tries for it with no
	// wake: a holder that dies releases nothing, so its hold is taken within holdOptions.TTL and
	// one standbyRetry, 25 s, of its last renewal, and a release the standby's listener missed
	// within one standbyRetry.
	standbyRetry = 10 * time.Second
	// failRetryMax bounds the wait after a pass that failed: 1 s after the first failure, doubling
	// to a minute, so a failure that stays, an event whose payload the rules cannot read, logs its
	// error each minute, and one that passes costs the fill a second.
	failRetryMax = time.Minute
	// caughtUpEvery bounds the line a pass logs of where the fill stands to one a minute.
	caughtUpEvery = time.Minute
)

// The lease pool's callers the fill reserves (ReserveLeases): its own goroutine, which takes and
// releases its hold and runs its batches and its reads one at a time, and the hold's renewal
// goroutine, which renews the hold beside them.
const (
	fillCaller        = "search text versions fill"
	fillRenewalCaller = "search text versions fill hold renewal"
)

// ReserveLeases reserves the fill's two connections of the lease pool, its own goroutine's and its
// hold's renewal's (store.LeaseCallers.ForAll), and answers the pool they are taken from, which is
// the Config.Leases Run takes. The server reserves them while it wires itself, as every caller of
// the lease pool does, so the count it checks against store.LeasePoolSize is the one the pool is
// sized for.
func ReserveLeases(callers *store.LeaseCallers) (*pgxpool.Pool, error) {
	return callers.ForAll(fillCaller, fillRenewalCaller)
}

// Config is what a serving process runs the fill with (Run).
type Config struct {
	// Leases is the lease pool the fill takes its hold on, renews it on and writes its batches
	// through (ReserveLeases): on a pool of its own, a batch waits for no request and a renewal
	// waits for no batch.
	Leases *pgxpool.Pool
	// Horizon is the process's one event horizon, which every pass reads the log up to.
	Horizon *events.Horizon
	// Latency is the statistic of the answers reads this process serves, which the server
	// records and the fill pauses on before each batch (pauser).
	Latency *sessionanswers.Latency
	// Serving is closed once this process serves: it listens and the registry of live server
	// processes admitted it (cmd/dispatch). Run takes nothing and writes nothing before, and
	// nothing at all in a process the registry refuses.
	Serving <-chan struct{}
	// Events is signalled after each commit that wrote an event (the listener hub's wake on the
	// events channel, api.EventsChannel): the holder's passes run on it. Nil wakes no pass but the
	// first of each hold and the horizon's own re-reads.
	Events <-chan struct{}
	// Released is signalled after a hold is released (the listener hub's wake on
	// duty.HoldReleasedChannel): a standby tries for the hold on it, as well as every
	// standbyRetry. Nil leaves the retry alone.
	Released <-chan struct{}
	// Logger is where the fill logs; nil is slog.Default.
	Logger *slog.Logger
}

// Run fills the keyword table in a serving process until ctx ends, under the fill's hold
// (fillHoldName), so one process of all those serving the database writes it at a time. It waits
// for cfg.Serving first, and takes nothing and writes nothing before.
//
// A process that does not hold the hold stands by: it tries for it at once, each time
// cfg.Released is signalled and every standbyRetry, so it takes a released hold at once and a dead
// holder's within 25 s. The holder runs a pass at once, and then each time cfg.Events is
// signalled, each up to one read of the horizon (filler.pass); while the horizon stands below an
// id a read of it saw, it reads it again 1 s and then up to 8 s later, and otherwise it holds no
// timer, so a fill caught up with nothing written sends no statement but its hold's renewal.
// Before each batch it asks the answers reads' pause (pauser), and each batch checks in its own
// transaction that the hold is still this holder's (filler.batch). Once the hold is lost it
// writes nothing more and stands by again. A pass that fails is tried again 1 s later, doubling
// to a minute, with its error logged each time.
//
// When ctx ends the batch in flight commits or rolls back, and the holder releases its hold, so
// a standby takes it at once, before Run returns. Run answers an error only for a Config it cannot
// run with, at once and with nothing done.
func Run(ctx context.Context, cfg Config) error {
	r, err := newRunner(cfg)
	if err != nil {
		return err
	}
	r.run(ctx)
	return nil
}

// runner is one process's fill under the hold: its Config, the filler its passes run, whose
// holder it takes the hold as and whose logger it logs to, and when it last logged where the fill
// stands.
type runner struct {
	cfg        Config
	fill       *filler
	caughtUpAt time.Time
}

// newRunner is the fill cfg describes, under a holder of its own, or the error naming what cfg
// lacks.
func newRunner(cfg Config) (*runner, error) {
	switch {
	case cfg.Leases == nil:
		return nil, errors.New("keyword fill: no lease pool to take the hold on")
	case cfg.Horizon == nil:
		return nil, errors.New("keyword fill: no event horizon to read the log up to")
	case cfg.Latency == nil:
		return nil, errors.New("keyword fill: no answers read latency to pause on")
	case cfg.Serving == nil:
		return nil, errors.New("keyword fill: no channel that says the process serves")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	fill := newFiller(cfg.Leases, cfg.Horizon)
	fill.logger = logger
	fill.holder = uuid.New()
	fill.pause = newPauser(cfg.Latency, logger).waitUntilFast
	return &runner{cfg: cfg, fill: fill}, nil
}

// The lines the hold logs.
const (
	tookHoldMessage     = "dispatch: the keyword fill took its hold; this process fills the keyword table"
	standingByMessage   = "dispatch: another process holds the keyword fill's hold; standing by"
	lostHoldMessage     = "dispatch: the keyword fill's hold is lost; it writes nothing more and stands by"
	releasedHoldMessage = "dispatch: the keyword fill released its hold"
	caughtUpMessage     = "dispatch: the keyword fill is caught up"
)

// run waits for the process to serve, then stands by and fills under the hold in turn until ctx
// ends.
func (r *runner) run(ctx context.Context) {
	select {
	case <-ctx.Done():
		return
	case <-r.cfg.Serving:
	}
	for {
		hold := r.standBy(ctx)
		if hold == nil {
			return
		}
		r.fill.logger.Info(tookHoldMessage, "holder", r.fill.holder)
		r.fillUnder(ctx, hold)
		if ctx.Err() != nil {
			r.release(hold)
			return
		}
		r.fill.logger.Warn(lostHoldMessage, "holder", r.fill.holder)
		// The renewal stops, and a row that still names this holder, lapsed or not, is freed,
		// which is the state the next try for the hold reads.
		r.end(hold)
	}
}

// standBy tries for the hold until it takes it, and answers it, or until ctx ends, and answers
// nil: at once, then each time cfg.Released is signalled and every retry. It says it stands by
// once, at the first refusal.
func (r *runner) standBy(ctx context.Context) *duty.Hold {
	retry := time.NewTicker(standbyRetry)
	defer retry.Stop()
	said := false
	for {
		hold, err := r.tryHold(ctx)
		switch {
		case hold != nil:
			return hold
		case err != nil:
			if ctx.Err() == nil {
				r.fill.logger.Error("dispatch: the keyword fill could not try for its hold", "holder", r.fill.holder, "error", err)
			}
		case !said:
			r.fill.logger.Info(standingByMessage, "holder", r.fill.holder)
			said = true
		}
		select {
		case <-ctx.Done():
			return nil
		case <-r.cfg.Released:
		case <-retry.C:
		}
	}
}

// tryHold takes the hold when no other holder holds it live, within one renewal interval.
func (r *runner) tryHold(ctx context.Context) (*duty.Hold, error) {
	bounded, cancel := context.WithTimeout(ctx, holdOptions.Renew)
	defer cancel()
	return duty.TryHold(bounded, r.cfg.Leases, fillHoldName, r.fill.holder, holdOptions)
}

// fillUnder runs the fill's passes under hold until ctx ends or the hold is lost: a pass at once,
// then one each time cfg.Events is signalled, and while the horizon stands below an id a read of
// it saw (filler.heldBelow) one 1 s later, doubling to 8 s, as FillToHorizon reads it. A pass that
// fails is followed by another 1 s later, doubling to failRetryMax, and the events do not hurry
// it. Everything runs on a context the hold's loss ends, so a batch in flight as the hold is lost
// rolls back, and a pause or a rest ends there.
func (r *runner) fillUnder(ctx context.Context, hold *duty.Hold) {
	held, stop := context.WithCancel(ctx)
	defer stop()
	go func() {
		select {
		case <-hold.Lost():
			stop()
		case <-held.Done():
		}
	}()
	// A wake that came while this process stood by is answered by the pass that starts now.
	select {
	case <-r.cfg.Events:
	default:
	}
	reread, failWait := rereadFirst, rereadFirst
	for {
		safe, err := r.fill.pass(held)
		if ctx.Err() != nil || r.lost(held, hold, err) {
			return
		}
		var due time.Duration
		woken := true
		if err != nil {
			r.fill.logger.Error("dispatch: the keyword fill's pass failed; it tries again", "retry_in", failWait, "error", err)
			due, woken = failWait, false
			failWait = min(2*failWait, failRetryMax)
		} else {
			failWait = rereadFirst
			r.caughtUp(held)
			if r.fill.heldBelow(safe) {
				due = reread
				reread = min(2*reread, rereadMax)
			} else {
				reread = rereadFirst
			}
		}
		if !r.await(held, due, woken) {
			return
		}
	}
}

// lost is whether the pass that answered err ended because the hold is no longer this holder's:
// its loss closed held, a batch found duty_holds naming another holder or none
// (duty.ErrHoldLost), or the hold's loss deadline passed while the pass ran - a process frozen
// past it finds its connection's transaction ended when it resumes, before the hold's own timer
// has told it.
func (r *runner) lost(held context.Context, hold *duty.Hold, err error) bool {
	return held.Err() != nil || errors.Is(err, duty.ErrHoldLost) ||
		(err != nil && !time.Now().Before(hold.Deadline()))
}

// await waits for the next pass: due later when due is not zero, at cfg.Events when woken, and
// answers false once held ends instead.
func (r *runner) await(held context.Context, due time.Duration, woken bool) bool {
	var timer <-chan time.Time
	if due > 0 {
		t := time.NewTimer(due)
		defer t.Stop()
		timer = t.C
	}
	var events <-chan struct{}
	if woken {
		events = r.cfg.Events
	}
	select {
	case <-held.Done():
		return false
	case <-timer:
	case <-events:
	}
	return true
}

// caughtUpQuery reads where the fill stands: its cursor, its reach, how far the reach stands
// behind the database's now() in milliseconds, and the newest event of the log.
const caughtUpQuery = `select f.event_id, f.reach, (extract(epoch from now() - f.reach) * 1000)::bigint,
       coalesce((select max(e.id) from public.events e), 0)
  from public.search_text_versions_fill f
 where f.id = 1`

// caughtUp logs, from a pass that ended with the fill caught up to the horizon it read, at most
// once each caughtUpEvery, the fill's reach, how far it stands behind now (its lag), and how many
// ids the cursor stands below the newest event. A fill with nothing to do runs no pass, and so
// writes no line and sends no statement for it.
func (r *runner) caughtUp(ctx context.Context) {
	now := time.Now()
	if !r.caughtUpAt.IsZero() && now.Sub(r.caughtUpAt) < caughtUpEvery {
		return
	}
	var cursor, newest int64
	var reach *time.Time
	var lagMillis *int64
	if err := r.cfg.Leases.QueryRow(ctx, caughtUpQuery).Scan(&cursor, &reach, &lagMillis, &newest); err != nil {
		if ctx.Err() == nil {
			r.fill.logger.Warn("dispatch: the keyword fill could not read where it stands", "error", err)
		}
		return
	}
	r.caughtUpAt = now
	attributes := []any{"cursor", cursor, "newest_event", newest, "events_behind", newest - cursor}
	if reach != nil && lagMillis != nil {
		attributes = append(attributes, "reach", *reach, "lag", time.Duration(*lagMillis)*time.Millisecond)
	}
	r.fill.logger.Info(caughtUpMessage, attributes...)
}

// release ends the holder's hold as ctx ends, freeing it at once for a standby, and says so. A
// hold already lost is ended without the line.
func (r *runner) release(hold *duty.Hold) {
	select {
	case <-hold.Lost():
		r.end(hold)
		return
	default:
	}
	if r.end(hold) {
		r.fill.logger.Info(releasedHoldMessage, "holder", r.fill.holder)
	}
}

// end releases hold (duty.Hold.Release): it stops the renewal and deletes the hold's row while it
// still names this holder, notifying the standbys. It runs on a context of its own, bounded by
// one renewal interval, since it must reach the database when the process's context has ended.
// It answers whether the release reached the database.
func (r *runner) end(hold *duty.Hold) bool {
	releasing, cancel := context.WithTimeout(context.Background(), holdOptions.Renew)
	defer cancel()
	if err := hold.Release(releasing); err != nil {
		r.fill.logger.Error("dispatch: the keyword fill could not release its hold", "holder", r.fill.holder, "error", err)
		return false
	}
	return true
}

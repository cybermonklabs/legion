// Package hold is the broker's one check of whether Dispatch holds a person's credentials until
// they sign in again: Gate.Decide, which each point that serves a person's credential asks, and
// whose Verdict it switches on. It asks Dispatch's person route through
// internal/cipher/projectaccess and keeps every answer it gets in
// person_standing (internal/cipher/store), so that while Dispatch cannot be asked it answers from
// the last answer Dispatch gave: a person last answered held stays held, since only Dispatch ends
// a hold, and everyone else is served, with one WARN line a minute per person and
// broker_hold_unchecked_total. A Dispatch older than the hold answers with no held field
// (projectaccess.ErrNoHeldField); the gate answers each person from the last answer the same way,
// with one WARN line a minute for every person at once and no count. The answer is kept in the
// database rather than in this process, so a restart or a second broker process answers alike,
// and each process keeps the latest answer by Dispatch's own clock (the answer's as_of) whatever
// order the writes arrive in.
//
// An IAP deployment's Dispatch puts a deadline on every answer that a person is not held
// (not_held_until). The gate serves such an answer, fresh, cached or kept, until its deadline and
// not at it, by its own clock, and never Dispatch's unavailable answer, whose deadline is its own
// as_of, whatever its clock says; it keeps the deadline in person_standing with its answer. Its
// broker sets BROKER_HOLD_REQUIRES_DEADLINE (Config.RequireDeadline), under which a not-held
// answer naming no deadline is not served, and nor is a person of whom no answer is kept while
// Dispatch cannot be asked.
//
// A caller asks before it opens a database transaction or takes a lock, as projectaccess
// requires of every caller: Decide asks Dispatch and writes person_standing itself.
package hold

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/dispatchread"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/cipher/store"
	"github.com/sjawhar/envoy/internal/metrics"
)

// ErrHeld is a person whose credentials Dispatch holds until they sign in again.
var ErrHeld = errors.New("hold: Dispatch holds this person's credentials until they sign in again")

// ErrUncheckable is a person whose standing the gate cannot decide: Dispatch refused the broker's
// own account (dispatchread.ErrBrokerRefused), Dispatch could not be asked and person_standing
// could not be read, or a held answer could not be written.
var ErrUncheckable = errors.New("hold: whether Dispatch holds this person cannot be read")

// ErrDeadlinePassed is a person whose last not-held answer from Dispatch has passed the deadline
// an IAP deployment's Dispatch put on it (not_held_until). It always comes with ErrUncheckable,
// since whether Dispatch holds the person can no longer be read, and is named apart from it
// because, unlike Dispatch refusing the broker's own account, it is a bound the broker serves the
// person to: the airlock's watch ends a call it carries once the person's deadline has passed, as
// every new request is refused then.
var ErrDeadlinePassed = errors.New("hold: the answer that the person is not held has passed its deadline")

// The other ways a deadline decides nothing, each ErrUncheckable beside it and named in Reason's
// fixed words: under RequireDeadline, a not-held answer naming no deadline; and, under
// RequireDeadline, a person of whom the broker keeps no answer while Dispatch cannot be asked.
var (
	errNoDeadline = errors.New("hold: the answer that the person is not held names no deadline, which this broker requires")
	errNoAnswer   = errors.New("hold: no answer is kept for the person, which this broker requires while Dispatch cannot be asked")
)

// askTimeout bounds one ask of Dispatch, and writeTimeout the write to person_standing after it,
// together inside ciphertoken's 5-second introspection bound.
const askTimeout = 3 * time.Second
const writeTimeout = 1 * time.Second

// readReserve is what checkUntil leaves a caller that has a deadline, out of the time it has left,
// for reading the answer person_standing keeps once Dispatch cannot be asked: the ask ends at
// askTimeout or at the caller's deadline less this, whichever comes first. The NATS auth service's
// decision has 1.5 s for everything (natscallout's decideBudget), so an ask bounded by the caller
// alone would end only as the decision did, and the kept answer could no longer be read.
const readReserve = 500 * time.Millisecond

// errNoTimeToAsk is the cause of a check whose caller had no more time than readReserve left and
// whose person the client holds no young answer for: it asks Dispatch nothing and answers from the
// kept row, and remembers no failure of Dispatch's.
var errNoTimeToAsk = errors.New("hold: the caller had no time left to ask Dispatch")

// failureMemory is how long a failure to ask is remembered per person.
const failureMemory = 5 * time.Second

// logEvery is how often the gate logs the same thing about one person while it goes on: a person
// it serves while Dispatch cannot be asked, and, for every person at once, Dispatch refusing the
// broker's own account and Dispatch answering with no held field.
const logEvery = time.Minute

// maxRemembered bounds the gate's per-person memories of failures, warnings and writes before it
// sweeps the ones whose time has passed. It never forgets a held answer it could not write.
const maxRemembered = 4096

// Config is what a Gate is built from.
type Config struct {
	// Access asks Dispatch's person route (BROKER_PROJECT_ACCESS_URL): the client the token
	// exchange asks too, so the two share one cache of person answers and one call per person.
	Access *projectaccess.Client
	// Store keeps person_standing, the last answer Dispatch gave for each person.
	Store *store.Store
	// Log is where the gate's WARN and ERROR lines go; nil is slog.Default().
	Log *slog.Logger
	// Metrics is the registry broker_hold_unchecked_total is counted on; nil counts nothing.
	Metrics *metrics.Registry
	// Now is the clock the gate's memories are timed by; nil is time.Now.
	Now func() time.Time
	// RequireDeadline is BROKER_HOLD_REQUIRES_DEADLINE, which an IAP deployment's broker sets:
	// Dispatch's answer that a person is not held then serves only with a deadline
	// (not_held_until), and a person of whom the broker keeps no answer is not served while
	// Dispatch cannot be asked.
	RequireDeadline bool
}

// Gate is the check. A nil *Gate is a broker with no Dispatch to ask, and holds nobody.
type Gate struct {
	access    *projectaccess.Client
	store     *store.Store
	log       *slog.Logger
	unchecked *metrics.Counter
	now       func() time.Time
	// requireDeadline is Config.RequireDeadline.
	requireDeadline bool

	mu sync.Mutex
	// failures is each person's last failure to ask and when it happened, which spares Dispatch
	// a second ask, and the caller a second wait, for failureMemory.
	failures map[string]failure
	// written is the AskedAt of the last answer for each person this gate wrote, so an answer the
	// client answers again from its cache is written once.
	written map[string]time.Time
	// unwritten is each held answer this gate could not write, by its as_of: written again at
	// each check of its person until a write takes it or a later answer from Dispatch replaces
	// it, and answered from while Dispatch cannot be asked.
	unwritten map[string]time.Time
	// warned is when the gate last logged serving each person while Dispatch could not be asked.
	warned map[string]time.Time
	// refusalLogged is when the gate last logged Dispatch refusing the broker's own account.
	refusalLogged time.Time
	// noHeldLogged is when the gate last logged Dispatch answering with no held field.
	noHeldLogged time.Time
}

type failure struct {
	at  time.Time
	err error
}

// New builds the gate.
func New(cfg Config) *Gate {
	g := &Gate{
		access: cfg.Access, store: cfg.Store, log: cfg.Log, now: cfg.Now, requireDeadline: cfg.RequireDeadline,
		failures: map[string]failure{}, written: map[string]time.Time{},
		unwritten: map[string]time.Time{}, warned: map[string]time.Time{},
	}
	if g.log == nil {
		g.log = slog.Default()
	}
	if g.now == nil {
		g.now = time.Now
	}
	if cfg.Metrics != nil {
		g.unchecked = cfg.Metrics.NewCounter("broker_hold_unchecked_total",
			"Checks of a person the broker served while Dispatch could not be asked whether it holds them, at most one a minute per person, by the last answer Dispatch gave for them: not_held or none.")
	}
	return g
}

// checkUntil is the check Decide reads (kindOf): ErrHeld for a held person and ErrUncheckable when
// it cannot decide, each wrapped with the person, and nil for everyone else, with beside a person
// it serves the deadline of the not-held answer it served them on (not_held_until), which
// introspection carries: the zero time for an answer naming none, as every answer outside IAP, and
// for a person served with no answer kept. A held answer it cannot write is
// errors.Join(ErrUncheckable, ErrHeld), and a passed deadline ErrUncheckable and ErrDeadlinePassed.
// An empty person is an error. A nil *Gate answers nil.
//
// It asks Dispatch about person as record.CanonicalLogin spells them, bounded by askTimeout and,
// for a caller with a deadline, by that deadline less readReserve, and answers from Dispatch's
// answer, which it writes to person_standing, deadline included, unless it already wrote that
// answer; the write runs apart from ctx, bounded by writeTimeout, so a caller that hangs up does
// not stop it. Dispatch refusing the broker's own account is ErrUncheckable for every person,
// logged at ERROR once a minute. Dispatch's answer naming no held, a Dispatch older than the hold
// (projectaccess.ErrNoHeldField), cached or fresh, is answered as a failure to ask is, logged at
// WARN once a minute for every person at once rather than per person, and not counted. Every
// other failure to ask, the ask's own bound running out among them, is answered from the later,
// by as_of, of the kept row and a held answer the gate could not write, read in the time the bound
// left the caller, and is remembered failureMemory unless the caller itself hung up. A caller with
// no more than readReserve left asks nothing: it is answered from the client's young cached answer
// when there is one (CacheTTL), and else from the kept row.
//
// A held answer, fresh or kept, is ErrHeld whatever deadline it carries. A not-held answer, fresh,
// cached or kept, whose deadline is not after the gate's clock is ErrUncheckable, and so is
// Dispatch's unavailable answer, whose deadline is its own as_of, whatever the gate's clock says;
// under RequireDeadline, so is a not-held answer naming no deadline and a person of whom no answer
// is kept while Dispatch cannot be asked; none of these is logged or counted as served.
func (g *Gate) checkUntil(ctx context.Context, person string) (time.Time, error) {
	if g == nil {
		return time.Time{}, nil
	}
	person = record.CanonicalLogin(person)
	if person == "" {
		return time.Time{}, errors.New("hold: a person's hold is checked, and none was named")
	}
	now := g.now()
	g.mu.Lock()
	remembered, failed := g.failures[person]
	g.mu.Unlock()
	if failed && now.Sub(remembered.at) < failureMemory {
		return g.unasked(ctx, person, remembered.err)
	}
	bound := askTimeout
	if deadline, ok := ctx.Deadline(); ok {
		if left := time.Until(deadline) - readReserve; left < bound {
			bound = left
		}
	}
	if bound <= 0 {
		// No time to ask, but a young answer the client already holds is Dispatch's answer, served
		// as one, with no WARN line and no count; only a person it holds none for is unasked.
		answer, err := g.access.Person(projectaccess.CachedOnly(ctx), person)
		switch {
		case err == nil:
			return g.answered(ctx, person, answer)
		case errors.Is(err, projectaccess.ErrNoHeldField):
			g.logNoHeld(person, err)
			return g.unasked(ctx, person, err)
		}
		return g.unasked(ctx, person, errNoTimeToAsk)
	}
	askCtx, cancel := context.WithTimeout(ctx, bound)
	answer, err := g.access.Person(askCtx, person)
	cancel()
	switch {
	case err == nil:
		g.mu.Lock()
		delete(g.failures, person)
		g.mu.Unlock()
		return g.answered(ctx, person, answer)
	case errors.Is(err, dispatchread.ErrBrokerRefused):
		g.logRefusal(person, err)
		return time.Time{}, fmt.Errorf("%w: %s: %w", ErrUncheckable, person, err)
	case errors.Is(err, projectaccess.ErrNoHeldField):
		// Dispatch answered, and its answer says nothing of a hold: the deployment's state, logged
		// once a minute for everyone, and answered below as a failure to ask is.
		g.logNoHeld(person, err)
	}
	if ctx.Err() != nil {
		// The caller hung up, which is no failure of Dispatch's to remember for the next one. An ask
		// whose own bound ran out ends while the caller still has its readReserve, so it is
		// Dispatch's failure, and is remembered below.
		return g.unasked(ctx, person, err)
	}
	g.mu.Lock()
	g.sweepLocked(now)
	g.failures[person] = failure{at: now, err: err}
	g.mu.Unlock()
	return g.unasked(ctx, person, err)
}

// notHeld is the check of a not-held answer for person whose deadline is until (the zero time for
// none), given as of asOf, the database time of Dispatch's read the answer rests on: served until
// until by the gate's clock, or ErrUncheckable at or past it; ErrUncheckable for Dispatch's
// unavailable answer, until equal to asOf (as an IAP deployment's Dispatch is to answer a person it
// cannot confirm; Dispatch in this tree names no deadline yet, access.PersonAnswer),
// whatever the gate's clock says, so a broker whose clock is behind Dispatch's database serves no
// such person; and, under RequireDeadline, ErrUncheckable for an answer naming none.
func (g *Gate) notHeld(person string, until, asOf time.Time) (time.Time, error) {
	switch {
	case until.IsZero() && g.requireDeadline:
		return time.Time{}, fmt.Errorf("%w: %s: %w", ErrUncheckable, person, errNoDeadline)
	case !until.IsZero() && (until.Equal(asOf) || !until.After(g.now())):
		return time.Time{}, fmt.Errorf("%w: %s: %w, %s", ErrUncheckable, person, ErrDeadlinePassed, until.UTC().Format(time.RFC3339Nano))
	}
	return until, nil
}

// answered is the check answered from Dispatch's answer. Each answer is written once, the first
// time the gate sees it, whichever caller's ask fetched it. A held answer that write fails is
// remembered and written again at each check until a write takes it or a later answer from
// Dispatch replaces it; a not-held one is served, and the next answer is written as any is.
func (g *Gate) answered(ctx context.Context, person string, answer projectaccess.PersonAnswer) (time.Time, error) {
	g.mu.Lock()
	if asOf, ok := g.unwritten[person]; ok && answer.AsOf.After(asOf) {
		delete(g.unwritten, person)
	}
	write := answer.AskedAt.After(g.written[person])
	if write {
		g.sweepLocked(g.now())
		g.written[person] = answer.AskedAt
	}
	g.mu.Unlock()
	var writeErr error
	if write {
		writeErr = g.keep(ctx, person, answer.Held, answer.AsOf, deadlineOf(answer))
		g.mu.Lock()
		switch asOf, remembered := g.unwritten[person]; {
		case writeErr == nil && remembered && !asOf.After(answer.AsOf):
			// The row now holds this answer or a later one, so the remembered one is written.
			delete(g.unwritten, person)
		case writeErr != nil && answer.Held && (!remembered || answer.AsOf.After(asOf)):
			g.unwritten[person] = answer.AsOf
		}
		g.mu.Unlock()
		if writeErr != nil && !answer.Held {
			g.log.Warn("broker: Dispatch's answer that a person is not held could not be kept; serving on it",
				"person", person, "error", writeErr)
		}
	}
	if writeErr == nil {
		writeErr = g.writeUnwritten(ctx, person)
	}
	if !answer.Held {
		return g.notHeld(person, answer.NotHeldUntil, answer.AsOf)
	}
	g.mu.Lock()
	_, unkept := g.unwritten[person]
	g.mu.Unlock()
	if unkept {
		return time.Time{}, heldUnwritten(person, writeErr)
	}
	return time.Time{}, fmt.Errorf("%w: %s", ErrHeld, person)
}

// deadlineOf is the deadline answer writes to person_standing beside it: its not_held_until when
// it is not held and names one, and none otherwise, so a held answer keeps no deadline whatever it
// carried.
func deadlineOf(answer projectaccess.PersonAnswer) *time.Time {
	if answer.Held || answer.NotHeldUntil.IsZero() {
		return nil
	}
	return &answer.NotHeldUntil
}

// unasked is the check answered while Dispatch cannot be asked (cause): from the later, by as_of,
// of the row person_standing keeps and a held answer this gate could not write. A kept not-held
// answer is judged by its kept deadline as a fresh one is, and under RequireDeadline a person of
// whom no answer is kept is not served.
func (g *Gate) unasked(ctx context.Context, person string, cause error) (time.Time, error) {
	writeErr := g.writeUnwritten(ctx, person)
	held, answeredAt, keptUntil, found, readErr := g.store.PersonStanding(ctx, person)
	g.mu.Lock()
	unwrittenAsOf, unwritten := g.unwritten[person]
	g.mu.Unlock()
	switch {
	case unwritten && (readErr != nil || !found || !unwrittenAsOf.Before(answeredAt)):
		return time.Time{}, heldUnwritten(person, errors.Join(writeErr, fmt.Errorf("Dispatch could not be asked: %w", cause)))
	case readErr != nil:
		return time.Time{}, fmt.Errorf("%w: %s: Dispatch could not be asked (%w), and its last answer could not be read: %w", ErrUncheckable, person, cause, readErr)
	case found && held:
		return time.Time{}, fmt.Errorf("%w: %s, by Dispatch's last answer, as of %s", ErrHeld, person, answeredAt.UTC().Format(time.RFC3339Nano))
	case !found && g.requireDeadline:
		return time.Time{}, fmt.Errorf("%w: %s: Dispatch could not be asked (%w): %w", ErrUncheckable, person, cause, errNoAnswer)
	}
	var until time.Time
	if found && keptUntil != nil {
		until = *keptUntil
	}
	if found {
		var err error
		if until, err = g.notHeld(person, until, answeredAt); err != nil {
			return time.Time{}, fmt.Errorf("%w, by Dispatch's last answer while it could not be asked (%w)", err, cause)
		}
	}
	if errors.Is(cause, projectaccess.ErrNoHeldField) {
		// logNoHeld says this once a minute for every person at once.
		return until, nil
	}
	lastAnswer := "none"
	if found {
		lastAnswer = "not_held"
	}
	now := g.now()
	g.mu.Lock()
	last, logged := g.warned[person]
	due := !logged || now.Sub(last) >= logEvery
	if due {
		g.sweepLocked(now)
		g.warned[person] = now
	}
	g.mu.Unlock()
	if due {
		g.log.Warn("broker: Dispatch could not be asked whether a person is held; serving on its last answer",
			"person", person, "last_answer", lastAnswer, "error", cause)
		g.unchecked.Inc([2]string{"last_answer", lastAnswer})
	}
	return until, nil
}

// writeUnwritten writes again the held answer for person this gate could not write, if there is
// one, forgets it once a write takes it, and answers the write's failure.
func (g *Gate) writeUnwritten(ctx context.Context, person string) error {
	g.mu.Lock()
	asOf, ok := g.unwritten[person]
	g.mu.Unlock()
	if !ok {
		return nil
	}
	if err := g.keep(ctx, person, true, asOf, nil); err != nil {
		return err
	}
	g.mu.Lock()
	if current, still := g.unwritten[person]; still && current.Equal(asOf) {
		delete(g.unwritten, person)
	}
	g.mu.Unlock()
	return nil
}

// keep writes Dispatch's answer for person, with its deadline until (nil for none), to
// person_standing, apart from the caller's request and bounded by writeTimeout.
func (g *Gate) keep(ctx context.Context, person string, held bool, asOf time.Time, until *time.Time) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
	defer cancel()
	return g.store.KeepPersonStanding(ctx, person, held, asOf, until)
}

// logRefusal logs Dispatch refusing the broker's own account at ERROR, once a minute for every
// person at once: it is the deployment's misconfiguration, not this person's.
func (g *Gate) logRefusal(person string, err error) {
	now := g.now()
	g.mu.Lock()
	due := g.refusalLogged.IsZero() || now.Sub(g.refusalLogged) >= logEvery
	if due {
		g.refusalLogged = now
	}
	g.mu.Unlock()
	if due {
		g.log.Error("broker: Dispatch refused the broker's own account, so whether a person is held cannot be read",
			"person", person, "error", err)
	}
}

// logNoHeld logs Dispatch answering with no held field (err) at WARN, once a minute for every
// person at once: a Dispatch older than the hold answers every person so, which is the
// deployment's state during a roll, not this person's.
func (g *Gate) logNoHeld(person string, err error) {
	now := g.now()
	g.mu.Lock()
	due := g.noHeldLogged.IsZero() || now.Sub(g.noHeldLogged) >= logEvery
	if due {
		g.noHeldLogged = now
	}
	g.mu.Unlock()
	if due {
		g.log.Warn("broker: Dispatch answers no held, as a Dispatch older than the hold does; serving each person on the last answer Dispatch gave for them",
			"person", person, "error", err)
	}
}

// heldUnwritten is a held answer for person that no write has kept yet, for cause:
// ErrUncheckable and ErrHeld at once.
func heldUnwritten(person string, cause error) error {
	if cause == nil {
		cause = errors.New("no write has kept it yet")
	}
	return errors.Join(
		fmt.Errorf("%w: %s: Dispatch's answer that holds them could not be kept: %w", ErrUncheckable, person, cause),
		fmt.Errorf("%w: %s", ErrHeld, person))
}

// Reason is why the gate could not decide a person (err, a check's error that matches
// ErrUncheckable), in words a refused caller may read: one of six fixed reasons, never the gate's
// error, which can carry a Postgres error's database user, name and address. Dispatch refusing the
// broker's own account is the deployment's to fix; a held answer the broker could not keep still
// holds the person; a not-held answer past its deadline, a not-held answer naming no deadline where
// the broker requires one, and no answer kept where the broker requires one while Dispatch cannot
// be asked are each named; and otherwise Dispatch could not be asked while the broker could not read
// the last answer Dispatch gave either. The words of a passed deadline also say Dispatch may never
// have confirmed the person: an IAP Dispatch answers a person with no row as it answers one whose
// last confirmation lapsed (not_held_until equal to as_of), and the broker cannot tell the two apart.
// Every point that refuses 503 HOLD_UNCHECKABLE, or server_error at the token endpoint, names one
// of them (Verdict.Words), and logs err itself.
func Reason(err error) string {
	switch {
	case errors.Is(err, dispatchread.ErrBrokerRefused):
		return "Dispatch refused the broker's own account"
	case errors.Is(err, ErrHeld):
		return "the broker could not keep Dispatch's answer that holds them"
	case errors.Is(err, ErrDeadlinePassed):
		return "Dispatch's answer that they are not held has passed its deadline, or Dispatch has never confirmed them"
	case errors.Is(err, errNoDeadline):
		return "Dispatch's answer that they are not held names no deadline, which this broker requires"
	case errors.Is(err, errNoAnswer):
		return "the broker could not ask Dispatch, and keeps no answer Dispatch gave for them"
	}
	return "the broker could not ask Dispatch, nor read the last answer Dispatch gave"
}

// sweepLocked forgets the failures, warnings and writes whose time has passed once the gate
// remembers maxRemembered of them. A write is forgotten once its answer is older than the client's
// cache keeps one, after which the client answers it no more. g.mu is held.
func (g *Gate) sweepLocked(now time.Time) {
	if len(g.failures)+len(g.warned)+len(g.written) < maxRemembered {
		return
	}
	for person, f := range g.failures {
		if now.Sub(f.at) >= failureMemory {
			delete(g.failures, person)
		}
	}
	for person, at := range g.warned {
		if now.Sub(at) >= logEvery {
			delete(g.warned, person)
		}
	}
	for person, asked := range g.written {
		if now.Sub(asked) >= projectaccess.CacheTTL {
			delete(g.written, person)
		}
	}
}

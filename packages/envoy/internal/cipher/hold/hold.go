// Package hold is the broker's one check of whether Dispatch holds a person's credentials until
// they sign in again: Gate.Decide, which each point that serves a person's credential asks, and
// whose Verdict it switches on. It asks Dispatch's person route through
// internal/cipher/projectaccess, whose client keeps every answer it fetches in person_standing
// (internal/cipher/store, projectaccess.Client.Keep), so that while Dispatch cannot be asked the
// gate answers from the last answer Dispatch gave (projectaccess.Client.KeptHold): a person last
// answered held stays held, since only Dispatch ends a hold, and everyone else is served, with one
// WARN line a minute per person and broker_hold_unchecked_total. A Dispatch older than the hold
// answers with no held field (projectaccess.ErrNoHeldField); the gate answers each person from the
// last answer the same way, with one WARN line a minute for every person at once and no count. The
// answer is kept in the database rather than in this process, so a restart or a second broker
// process answers alike, and each process keeps the latest answer by Dispatch's own clock (the
// answer's as_of) whatever order the writes arrive in.
//
// An IAP deployment's Dispatch puts a deadline on every answer that a person is not held
// (not_held_until). The gate serves such an answer, fresh, cached or kept, until its deadline and
// not at it, by its own clock, and never Dispatch's unavailable answer, whose deadline is its own
// as_of, whatever its clock says; the client keeps the deadline in person_standing with its
// answer. Its broker sets BROKER_HOLD_REQUIRES_DEADLINE (Config.RequireDeadline), under which a
// not-held answer naming no deadline is not served, and nor is a person of whom no answer is kept
// while Dispatch cannot be asked.
//
// A caller asks before it opens a database transaction or takes a lock, as projectaccess
// requires of every caller: Decide asks Dispatch, and its client writes person_standing.
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
	"github.com/sjawhar/envoy/internal/metrics"
)

// errHeld is a person whose credentials Dispatch holds until they sign in again.
var errHeld = errors.New("hold: Dispatch holds this person's credentials until they sign in again")

// errUncheckable is a person whose standing the gate cannot decide: Dispatch refused the broker's
// own account (dispatchread.ErrBrokerRefused), Dispatch could not be asked and person_standing
// could not be read, or a held answer could not be kept.
var errUncheckable = errors.New("hold: whether Dispatch holds this person cannot be read")

// errDeadlinePassed is a person whose last not-held answer from Dispatch has passed the deadline
// an IAP deployment's Dispatch put on it (not_held_until). It always comes with errUncheckable,
// since whether Dispatch holds the person can no longer be read, and is named apart from it
// because, unlike Dispatch refusing the broker's own account, it is a bound the broker serves the
// person to: the airlock's watch ends a call it carries once the person's deadline has passed, as
// every new request is refused then.
var errDeadlinePassed = errors.New("hold: the answer that the person is not held has passed its deadline")

// The other ways a deadline decides nothing, each errUncheckable beside it and named by the
// verdict's fixed words: under RequireDeadline, a not-held answer naming no deadline; and, under
// RequireDeadline, a person of whom the broker keeps no answer while Dispatch cannot be asked.
var (
	errNoDeadline = errors.New("hold: the answer that the person is not held names no deadline, which this broker requires")
	errNoAnswer   = errors.New("hold: no answer is kept for the person, which this broker requires while Dispatch cannot be asked")
)

// askTimeout bounds one ask of Dispatch, inside ciphertoken's 5-second introspection bound beside
// the client's write of the answer it fetched and the read of the kept one.
const askTimeout = 3 * time.Second

// readReserve is what checkUntil leaves a caller that has a deadline, out of the time it has left,
// for reading the answer person_standing keeps once Dispatch cannot be asked: the ask ends at
// askTimeout or at the caller's deadline less this, whichever comes first. The NATS auth service's
// decision has 1.5 s for everything (natscallout's decideBudget), so an ask bounded by the caller
// alone would end only as the decision did, and the kept answer could no longer be read.
const readReserve = 500 * time.Millisecond

// errNoTimeToAsk is the cause of a check whose caller had no more time than readReserve left and
// whose person the client holds no young answer for: it asks Dispatch nothing and answers from the
// kept row.
var errNoTimeToAsk = errors.New("hold: the caller had no time left to ask Dispatch")

// logEvery is how often the gate logs the same thing about one person while it goes on: a person
// it serves while Dispatch cannot be asked, and, for every person at once, Dispatch refusing the
// broker's own account and Dispatch answering with no held field.
const logEvery = time.Minute

// maxRemembered bounds the gate's per-person WARN memory before it sweeps the entries whose time
// has passed.
const maxRemembered = 4096

// Config is what a Gate is built from.
type Config struct {
	// Access asks Dispatch's person route (BROKER_PROJECT_ACCESS_URL): the client the token
	// exchange asks too, so the two share one cache of person answers and one call per person.
	// It keeps each answer it fetches (projectaccess.Client.Keep), which the gate answers from
	// while Dispatch cannot be asked; a client that keeps nothing holds every such check
	// uncheckable.
	Access *projectaccess.Client
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
	log       *slog.Logger
	unchecked *metrics.Counter
	now       func() time.Time
	// requireDeadline is Config.RequireDeadline.
	requireDeadline bool

	mu sync.Mutex
	// warned is when the gate last logged serving each person while Dispatch could not be asked.
	warned map[string]time.Time
	// refusalLogged is when the gate last logged Dispatch refusing the broker's own account.
	refusalLogged time.Time
	// noHeldLogged is when the gate last logged Dispatch answering with no held field.
	noHeldLogged time.Time
}

// New builds the gate.
func New(cfg Config) *Gate {
	g := &Gate{
		access: cfg.Access, log: cfg.Log, now: cfg.Now, requireDeadline: cfg.RequireDeadline,
		warned: map[string]time.Time{},
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

// checkUntil is the check Decide reads (kindOf): errHeld for a held person and errUncheckable when
// it cannot decide, each wrapped with the person, and nil for everyone else, with beside a person
// it serves the deadline of the not-held answer it served them on (not_held_until), which
// introspection carries: the zero time for an answer naming none, as every answer outside IAP, and
// for a person served with no answer kept. A held answer its client cannot keep is
// errors.Join(errUncheckable, errHeld), and a passed deadline errUncheckable and
// errDeadlinePassed. An empty person is an error. A nil *Gate answers nil.
//
// It asks Dispatch about person as record.CanonicalLogin spells them, bounded by askTimeout and,
// for a caller with a deadline, by that deadline less readReserve, and answers from Dispatch's
// answer, which its client keeps in person_standing, deadline included, whichever read fetched it
// (projectaccess.Client.Keep). Dispatch refusing the broker's own account is errUncheckable for
// every person, logged at ERROR once a minute. Dispatch's answer naming no held, a Dispatch older
// than the hold (projectaccess.ErrNoHeldField), cached or fresh, is answered as a failure to ask
// is, logged at WARN once a minute for every person at once rather than per person, and not
// counted. Every other failure to ask, the ask's own bound running out among them, is answered
// from the later, by as_of, of the kept row and a held answer the client could not keep
// (projectaccess.Client.KeptHold), read in the time the bound left the caller. The client's one
// failure memory (projectaccess.failureTTL) is shared with MemberKind, so a trade's membership
// decision and its hold retry together; the gate keeps no second failure memory. A caller with no
// more than readReserve left asks nothing: it is answered from the client's young cached answer
// when there is one (CacheTTL), and else from the kept row.
//
// A held answer, fresh or kept, is errHeld whatever deadline it carries. A not-held answer, fresh,
// cached or kept, whose deadline is not after the gate's clock is errUncheckable, and so is
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
			return g.answered(person, answer)
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
		return g.answered(person, answer)
	case errors.Is(err, dispatchread.ErrBrokerRefused):
		g.logRefusal(person, err)
		return time.Time{}, fmt.Errorf("%w: %s: %w", errUncheckable, person, err)
	case errors.Is(err, projectaccess.ErrNoHeldField):
		// Dispatch answered, and its answer says nothing of a hold: the deployment's state, logged
		// once a minute for everyone, and answered below as a failure to ask is.
		g.logNoHeld(person, err)
	}
	// Every failure to ask is answered from the kept row. A hang is no special case: the client
	// shares one call per person and answers a later check errStillAsking once that call has run
	// projectaccess.failureTTL, so a check inside that window reads the kept row at once rather than
	// waiting its own bound (projectaccess.personAnswer).
	return g.unasked(ctx, person, err)
}

// notHeld is the check of a not-held answer for person whose deadline is until (the zero time for
// none), given as of asOf, the database time of Dispatch's read the answer rests on: served until
// until by the gate's clock, or errUncheckable at or past it; errUncheckable for Dispatch's
// unavailable answer, until equal to asOf (as an IAP deployment's Dispatch is to answer a person it
// cannot confirm; Dispatch in this tree names no deadline yet, access.PersonAnswer), whatever the
// gate's clock says, so a broker whose clock is behind Dispatch's database serves no such person;
// and, under RequireDeadline, errUncheckable for an answer naming none.
func (g *Gate) notHeld(person string, until, asOf time.Time) (time.Time, error) {
	switch {
	case until.IsZero() && g.requireDeadline:
		return time.Time{}, fmt.Errorf("%w: %s: %w", errUncheckable, person, errNoDeadline)
	case !until.IsZero() && (until.Equal(asOf) || !until.After(g.now())):
		return time.Time{}, fmt.Errorf("%w: %s: %w, %s", errUncheckable, person, errDeadlinePassed, until.UTC().Format(time.RFC3339Nano))
	}
	return until, nil
}

// answered is the check answered from Dispatch's answer, which the client has kept. A held answer
// it could not keep refuses the person as both errUncheckable and errHeld (Unkept), since a later
// outage of Dispatch could otherwise serve them; the client writes it again at each check until a
// write takes it or a later answer from Dispatch replaces it.
func (g *Gate) answered(person string, answer projectaccess.PersonAnswer) (time.Time, error) {
	if !answer.Held {
		return g.notHeld(person, answer.NotHeldUntil, answer.AsOf)
	}
	if answer.Unkept != nil {
		return time.Time{}, heldUnwritten(person, answer.Unkept)
	}
	return time.Time{}, fmt.Errorf("%w: %s", errHeld, person)
}

// unasked is the check answered while Dispatch cannot be asked (cause): from the later, by as_of,
// of the row person_standing keeps and a held answer the client could not keep
// (projectaccess.Client.KeptHold). A kept not-held answer is judged by its kept deadline as a
// fresh one is, and under RequireDeadline a person of whom no answer is kept is not served.
func (g *Gate) unasked(ctx context.Context, person string, cause error) (time.Time, error) {
	kept, found, readErr := g.access.KeptHold(ctx, person)
	switch {
	case kept.Unkept != nil:
		return time.Time{}, heldUnwritten(person, errors.Join(kept.Unkept, fmt.Errorf("Dispatch could not be asked: %w", cause)))
	case readErr != nil:
		return time.Time{}, fmt.Errorf("%w: %s: Dispatch could not be asked (%w), and its last answer could not be read: %w", errUncheckable, person, cause, readErr)
	case found && kept.Held:
		return time.Time{}, fmt.Errorf("%w: %s, by Dispatch's last answer, as of %s", errHeld, person, kept.AsOf.UTC().Format(time.RFC3339Nano))
	case !found && g.requireDeadline:
		return time.Time{}, fmt.Errorf("%w: %s: Dispatch could not be asked (%w): %w", errUncheckable, person, cause, errNoAnswer)
	}
	var until time.Time
	if found && kept.NotHeldUntil != nil {
		until = *kept.NotHeldUntil
	}
	if found {
		var err error
		if until, err = g.notHeld(person, until, kept.AsOf); err != nil {
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
	last := g.warned[person]
	due := g.due(&last, now)
	if due {
		g.sweepLocked(now)
		g.warned[person] = last
	}
	g.mu.Unlock()
	if due {
		g.log.Warn("broker: Dispatch could not be asked whether a person is held; serving on its last answer",
			"person", person, "last_answer", lastAnswer, "error", cause)
		g.unchecked.Inc([2]string{"last_answer", lastAnswer})
	}
	return until, nil
}

// logRefusal logs Dispatch refusing the broker's own account at ERROR, once a minute for every
// person at once: it is the deployment's misconfiguration, not this person's.
func (g *Gate) logRefusal(person string, err error) {
	now := g.now()
	g.mu.Lock()
	due := g.due(&g.refusalLogged, now)
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
	due := g.due(&g.noHeldLogged, now)
	g.mu.Unlock()
	if due {
		g.log.Warn("broker: Dispatch answers no held, as a Dispatch older than the hold does; serving each person on the last answer Dispatch gave for them",
			"person", person, "error", err)
	}
}

// due says whether a line last logged at last is due again. It stamps last with now when it is. The
// gate holds g.mu, which serializes every caller's one-minute decision and stamp.
func (g *Gate) due(last *time.Time, now time.Time) bool {
	if !last.IsZero() && now.Sub(*last) < logEvery {
		return false
	}
	*last = now
	return true
}

// heldUnwritten is a held answer for person that no write has kept yet, for cause:
// errUncheckable and errHeld at once.
func heldUnwritten(person string, cause error) error {
	return errors.Join(
		fmt.Errorf("%w: %s: Dispatch's answer that holds them could not be kept: %w", errUncheckable, person, cause),
		fmt.Errorf("%w: %s", errHeld, person))
}

// sweepLocked forgets the warnings whose time has passed once the gate remembers maxRemembered of
// them. g.mu is held.
func (g *Gate) sweepLocked(now time.Time) {
	if len(g.warned) < maxRemembered {
		return
	}
	for person, at := range g.warned {
		if now.Sub(at) >= logEvery {
			delete(g.warned, person)
		}
	}
}

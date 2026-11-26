package hold

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/dispatchread"
)

// Kind is what one check of a person decided.
type Kind int

const (
	// Served is a person the gate serves.
	Served Kind = iota
	// Held is a person Dispatch holds, by an answer the broker keeps.
	Held
	// HeldUnkept is a person Dispatch holds by an answer the broker could not keep. A route
	// refuses them as a check that decides nothing, since the broker cannot keep them held through
	// a later outage of Dispatch; the airlock's watch of a carried call cuts the call as for Held,
	// since the answer still says Dispatch holds them.
	HeldUnkept
	// DeadlinePassed is a person whose last not-held answer has passed the deadline an IAP
	// deployment's Dispatch put on it: a check that decides nothing, and the bound the broker
	// serves the person to.
	DeadlinePassed
	// Uncheckable is every other check that decides nothing: an answer the broker can neither ask
	// for nor read.
	Uncheckable
	// BrokerRefused is a Dispatch refusal of the broker's own account, a misconfiguration that the
	// token endpoint answers with refusedByDispatch rather than an ordinary HOLD_UNCHECKABLE error.
	BrokerRefused
)

// The codes a JSON route answers a refusal with: 403 MEMBERSHIP_HELD for Held, and 503
// HOLD_UNCHECKABLE for every kind that decides nothing.
const (
	CodeHeld        = "MEMBERSHIP_HELD"
	CodeUncheckable = "HOLD_UNCHECKABLE"
)

// Verdict is what one check of a person decided (Gate.Decide). The check's error is decoded once,
// in decode, in the one order every point that serves a credential needs: a held answer the broker
// could not keep matches both internal sentinels, and a passed deadline another pair, so a point
// that tested errors itself in another order would answer a hold as a 503, or a check that decides
// nothing as a 403. A point switches on Kind; Err is only for its log, and the words it reads were
// decided by decode, never by a caller's switch. The two reasons no caller reads apart (a not-held
// answer naming no deadline, a person of whom no answer is kept) stay Uncheckable, so no caller's
// switch can miss them; decode still gives each its own words.
type Verdict struct {
	// Kind is what the check decided.
	Kind Kind
	// Person is the person checked, as the caller named them.
	Person string
	// Until is the deadline of the not-held answer a Served person was served on (not_held_until),
	// the zero time for none.
	Until time.Time
	// Err is the check's error, nil for Served. It is for the log alone: it can carry a Postgres
	// error's database user, name and address. Words say what a refused caller may read.
	Err error
	// words is the fixed reason a refused caller reads, decided by decode; empty for Served and for
	// a Held person, whose words are HeldWords. It never carries Err's text.
	words string
	// requireDeadline is the gate's Config.RequireDeadline, which HeldWords reads.
	requireDeadline bool
}

// Decide is the gate's check of person as a Verdict: what every point that serves a person's
// credential asks. A nil *Gate serves everyone.
func (g *Gate) Decide(ctx context.Context, person string) Verdict {
	until, err := g.checkUntil(ctx, person)
	kind, words := decode(err)
	return Verdict{Kind: kind, Person: person, Until: until, Err: err, words: words, requireDeadline: g != nil && g.requireDeadline}
}

// decode is the gate's one reading of a check's error (checkUntil's): the Kind a caller switches on
// and the fixed reason a refused caller reads, in the one order. A held answer the broker could not
// keep matches both errHeld and errUncheckable, and a passed deadline both errDeadlinePassed and
// errUncheckable, so order decides; the two reasons no caller reads apart (a required deadline
// absent, no kept answer) are Uncheckable with their own words. The words never carry Err's text,
// which can hold a database user, name and address.
func decode(err error) (Kind, string) {
	switch held := errors.Is(err, errHeld); {
	case err == nil:
		return Served, ""
	case held && errors.Is(err, errUncheckable):
		return HeldUnkept, "the broker could not keep Dispatch's answer that holds them"
	case held:
		return Held, ""
	case errors.Is(err, errDeadlinePassed):
		return DeadlinePassed, "Dispatch's answer that they are not held has passed its deadline, or Dispatch has never confirmed them"
	case errors.Is(err, dispatchread.ErrBrokerRefused):
		return BrokerRefused, "Dispatch refused the broker's own account"
	case errors.Is(err, errNoDeadline):
		return Uncheckable, "Dispatch's answer that they are not held names no deadline, which this broker requires"
	case errors.Is(err, errNoAnswer):
		return Uncheckable, "the broker could not ask Dispatch, and keeps no answer Dispatch gave for them"
	}
	return Uncheckable, "the broker could not ask Dispatch, nor read the last answer Dispatch gave"
}

// Words is what a refused caller reads: for Held the gate's words for a held person (HeldWords),
// and for every kind that decides nothing, HeldUnkept among them, that whether Dispatch holds the
// person cannot be read and why (the reason decode chose). Served says nothing.
func (v Verdict) Words() string {
	switch v.Kind {
	case Served:
		return ""
	case Held:
		return v.HeldWords()
	}
	return "whether Dispatch holds " + v.Person + " cannot be read: " + v.words
}

// HeldWords is what a point that refuses the person as held says: who is held and what ends the
// hold. Under RequireDeadline, which only an IAP deployment's broker sets, a sign-in releases
// nothing and the Workspace Directory's next active answer does, so the words say that; elsewhere
// the person's next sign-in to Dispatch, or Dispatch confirming them with Google again, ends it.
// The airlock's watch says it for HeldUnkept too.
func (v Verdict) HeldWords() string {
	if v.requireDeadline {
		return fmt.Sprintf("Google's Workspace Directory no longer confirms %s; their credentials work again once it does", v.Person)
	}
	return fmt.Sprintf("%[1]s's credentials are held by Dispatch; they work again when %[1]s signs in to Dispatch, or when Dispatch can confirm %[1]s with Google again", v.Person)
}

// Refusal is how a route refuses the person: 0, "" and "" for Served; 403 CodeHeld for Held; and
// 503 CodeUncheckable for every kind that decides nothing, HeldUnkept among them; each with Words.
func (v Verdict) Refusal() (status int, code, words string) {
	switch v.Kind {
	case Served:
		return 0, "", ""
	case Held:
		return http.StatusForbidden, CodeHeld, v.Words()
	}
	return http.StatusServiceUnavailable, CodeUncheckable, v.Words()
}

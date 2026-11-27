// person.go is the Client's reader of Dispatch's person route: what kind of member a person is
// (MemberKind) and whether Dispatch holds them (Person), both read from one cached answer per
// person, one call in flight per person, and one memory per person of a call that failed.
package projectaccess

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/redact"
)

// failureTTL is how long a read of the person route that failed is the answer to the next reads
// of the same person, MemberKind's, Person's and the hold gate's alike, counted from when the
// failure came back: while Dispatch does not answer, those reads fail at once rather than each
// waiting up to requestTimeout for a call of its own. It is also how long a call may be in flight
// before a later read of the same person stops joining it and answers errStillAsking at once, so a
// Dispatch that hangs does not make every check wait its own bound for the one ten-second call. One
// person route answers a trade's membership decision and its hold, so both retry at this one short
// bound. A failure is no answer, and the read is asked again once it passes.
const failureTTL = 2 * time.Second

// cachedPerson is one answer of the person route, kept as words and not as another party's text:
// the member kind read from the answer with the question's record (readPerson) and kept both ways
// a reader needs it, so nothing this client holds between calls holds a redact.RemoteText. known
// is the one of MemberFull, MemberRestricted and MemberNone the field was, told apart as it came
// (knownKind), and "" for any other kind; kind is the field as the rule reads it
// (redact.RemoteText.String), which is Dispatch's own spelling for every kind of plain words, the
// three this broker knows among them, and holds no run of the account token the question carried,
// so an unknown kind prints only as the rule read it (MemberKind's ErrUnknownMemberKind) and is
// kept and passed on only that way (Keeper.KeepPersonAnswer, PersonAnswer.MemberKind). Beside
// them: whether Dispatch holds the person, as it answered it (nil for an answer with no held
// field, from a Dispatch older than the hold), the as_of it answered with (zero when it named
// none), the not_held_until it answered with (zero when it named none), and when it was asked.
type cachedPerson struct {
	kind  string
	known string
	held  *bool
	asOf  time.Time
	until time.Time
	at    time.Time
}

// failedRead is a read of the person route that failed, and when the failure came back.
type failedRead struct {
	err error
	at  time.Time
}

// personFlight is one call of the person route for a person, which every read of that person made
// while it is in flight waits on: answer and err are its outcome, set before done closes. startedAt
// is when the call began, by the client's clock, so a reader that joins a call already older than
// failureTTL answers at once rather than waiting its own bound on a Dispatch that hangs.
type personFlight struct {
	done      chan struct{}
	startedAt time.Time
	answer    cachedPerson
	err       error
}

// cachedOnlyKey marks a context whose person questions are answered from the cache alone
// (CachedOnly).
type cachedOnlyKey struct{}

// ErrNotCached is a question asked under CachedOnly that the cache holds no young answer to: no
// call to Dispatch was made, and nothing is known of the person.
var ErrNotCached = errors.New("projectaccess: no young answer is cached")

// CachedOnly is ctx for person questions the cache alone answers: Person and MemberKind under it
// answer a cached answer while it is young, as without it, and ErrNotCached where they would ask
// Dispatch, which they neither ask nor wait on. It is for a caller with no time left to ask, as
// the hold gate is for a check whose caller's deadline leaves it none (hold.Gate.Decide).
// A ctx marked Fresh as well asks Dispatch even so, since personAnswer reads Fresh first. No
// caller marks both: the hold gate is the one caller that marks CachedOnly, and the request
// machine's project reads, the ones that mark Fresh (requests' atDecision and decisionFacts),
// never check a hold.
func CachedOnly(ctx context.Context) context.Context {
	return context.WithValue(ctx, cachedOnlyKey{}, true)
}

// cachedOnly reports whether ctx is one CachedOnly made, or derived from one.
func cachedOnly(ctx context.Context) bool {
	marked, _ := ctx.Value(cachedOnlyKey{}).(bool)
	return marked
}

// MemberKind is the person's member kind, from the cached person answer while it is younger than
// CacheTTL, unless ctx is one Fresh made, whether or not that answer has held (Person), so a
// Dispatch older than the hold still answers it: one of MemberFull, MemberRestricted and
// MemberNone. Any other answer, an empty one included, is ErrUnknownMemberKind naming it; such an
// answer is read for failureTTL from when it was asked, as a failure would be, and asked again
// after, so a Dispatch that starts answering a kind the broker knows is read soon.
//
// Reads of one person in flight together, MemberKind's and Person's alike, share one call to
// Dispatch, which runs on none of their contexts, so a reader that gives up ends only its own
// wait; and a read that failed is the answer to that person's next reads, MemberKind's and
// Person's alike, for failureTTL from when it came back. So while Dispatch does not answer, a
// person's reads wait on one call at a time, each for no longer than that call's requestTimeout,
// rather than each making a call of its own. A read under Fresh, or one under AskedNow, joins no
// call already in flight, which may have been asked before the change it must see, and takes no
// kept failure: it asks Dispatch itself (personAnswerNow). Under CachedOnly it answers a young
// cached answer or ErrNotCached, and asks nothing.
//
// A client given a Keeper (Keep) answers a read Dispatch did not answer (Unanswered: no answer, a
// timeout, the caller's own bound included, or Dispatch's own outage status) from the member kind
// it kept for the person (keptKind): MemberFull for a person last kept full, and the read's failure
// for everyone else, as with no Keeper. Every other failure is returned as it is: Dispatch refusing
// the broker's own account, a kind this broker does not know, an answer it cannot read, and a read
// whose caller hung up (context.Canceled), which says nothing of Dispatch.
func (c *Client) MemberKind(ctx context.Context, person string) (string, error) {
	person = record.CanonicalLogin(person)
	if person == "" {
		return "", errors.New("projectaccess: a member kind is asked of a person, and none was named")
	}
	answer, err := c.personAnswer(ctx, person, func(cached cachedPerson) time.Duration {
		if cached.known == "" {
			return failureTTL
		}
		return CacheTTL
	})
	if err != nil {
		if c.kept != nil && Unanswered(err) {
			return c.keptKind(ctx, person, err)
		}
		return "", err
	}
	if answer.known == "" {
		return "", fmt.Errorf("%w: %q for %s", ErrUnknownMemberKind, answer.kind, person)
	}
	return answer.known, nil
}

// knownKind is the one of MemberFull, MemberRestricted and MemberNone that kind, the member kind
// as the answer carried it, is, compared as it came (RemoteText.OneOf), and "" for any other
// kind. It is read once, where the answer is read (readPerson), and kept beside the rule's reading
// of the field (cachedPerson), so no reader of a kept answer holds the field itself.
func knownKind(kind redact.RemoteText) string {
	known, _ := kind.OneOf(MemberFull, MemberRestricted, MemberNone)
	return known
}

// PersonAnswer is Dispatch's answer on its person route for one person, as Person reads it.
type PersonAnswer struct {
	// MemberKind is the member kind Dispatch answered, passed on whatever it is, as the rule reads
	// it (cachedPerson.kind): MemberKind is the check of it.
	MemberKind string
	// Held is whether Dispatch holds the person's credentials until they sign in again.
	Held bool
	// AsOf is the database time of the read Dispatch's answer rests on (as_of): the clock by
	// which a later answer is told from an earlier one, whichever broker process asked.
	AsOf time.Time
	// NotHeldUntil is the deadline an IAP deployment's Dispatch puts on an answer that is not held
	// (not_held_until): the last active Workspace Directory answer plus 24 hours, or AsOf itself
	// once that passed. Past it the answer is no longer served (hold.Gate.Decide). It is the
	// zero time for an answer naming none, as every answer outside IAP and every held one.
	NotHeldUntil time.Time
	// AskedAt is when the question this answers was asked, by the client's clock, read before it
	// was sent. Every read of one cached answer carries the same AskedAt.
	AskedAt time.Time
	// Unkept is, for a held answer, why the broker's database keeps no answer yet that Dispatch
	// holds the person: the failure of the last write of the held answer the client could not
	// keep (keepHeldAgain), or errKeepsNothing for a client never given a keeper (Keep). It is nil
	// for an answer that is not held, and once a write keeps the held answer.
	Unkept error
}

// Person is Dispatch's answer for person: their member kind, whether Dispatch holds them, the
// time its answer rests on, and the deadline an IAP deployment's Dispatch puts on a not-held
// answer, as Dispatch spelled it. It reads the cached person answer MemberKind reads, while it is
// younger than CacheTTL, whatever its deadline: the gate judges that at each use
// (hold.Gate.Decide). An answer with no held, from a Dispatch older than the hold, is
// ErrNoHeldField, answered from the cache for CacheTTL as every answer is; an answer with held and
// no as_of is a failure too, since no later answer could be told from it. Neither is ever read as
// not held. A member kind the broker does not know is still an answer here, since its held
// decodes. Dispatch refusing the broker's own account is dispatchread.ErrBrokerRefused, as for
// MemberKind, and every other failure is a failure to ask. It shares a call in flight, and a
// failure kept, as MemberKind does. A held answer the client could not keep is written again
// before it is answered, and carries Unkept while no write takes it.
func (c *Client) Person(ctx context.Context, person string) (PersonAnswer, error) {
	person = record.CanonicalLogin(person)
	if person == "" {
		return PersonAnswer{}, errors.New("projectaccess: a person's answer is asked of a person, and none was named")
	}
	answer, err := c.personAnswer(ctx, person, func(cachedPerson) time.Duration { return CacheTTL })
	switch {
	case err != nil:
		return PersonAnswer{}, err
	case answer.held == nil:
		return PersonAnswer{}, fmt.Errorf("%w: %s", ErrNoHeldField, person)
	case answer.asOf.IsZero():
		return PersonAnswer{}, fmt.Errorf("projectaccess: dispatch's answer for %s names held and no as_of", person)
	}
	read := PersonAnswer{MemberKind: answer.kind, Held: *answer.held, AsOf: answer.asOf, NotHeldUntil: answer.until, AskedAt: answer.at}
	if read.Held {
		read.Unkept = c.keepHeldAgain(ctx, person)
	}
	return read, nil
}

// personAnswer is the cached answer for person, a canonical login, while it is younger than keep
// says for it, and else the one a call of the person route answers. Under Fresh or AskedNow it is
// personAnswerNow. Under CachedOnly a cache that holds no young answer is ErrNotCached, and nothing
// is asked. A failure an earlier read kept for person within failureTTL is the answer, with no
// call. Otherwise the read joins the person's call in flight, or starts one, which runs on none of
// its readers' contexts (askPerson): a reader whose own context ends stops waiting, and the call
// goes on for every reader still waiting and for the cache. A reader that joins a call already
// older than failureTTL does not wait: a Dispatch that hangs holds its call open requestTimeout, so
// without this every check in those ten seconds would wait its own bound; instead, once a call has
// run failureTTL with no answer, later reads of that person, MemberKind's and the hold gate's
// alike, answer errStillAsking at once, as a kept failure would, until the call settles.
func (c *Client) personAnswer(ctx context.Context, person string, keep func(cachedPerson) time.Duration) (cachedPerson, error) {
	if isAskedNow(ctx) {
		return c.personAnswerNow(ctx, person, keep)
	}
	c.mu.Lock()
	if cached, ok := c.people[person]; ok && c.now().Sub(cached.at) < keep(cached) {
		c.mu.Unlock()
		return cached, nil
	}
	if cachedOnly(ctx) {
		c.mu.Unlock()
		return cachedPerson{}, fmt.Errorf("%w: %s", ErrNotCached, person)
	}
	if failed, ok := c.failures[person]; ok && c.now().Sub(failed.at) < failureTTL {
		c.mu.Unlock()
		return cachedPerson{}, failed.err
	}
	flight, joined := c.flights[person]
	switch {
	case !joined:
		flight = &personFlight{done: make(chan struct{}), startedAt: c.now()}
		c.flights[person] = flight
		go c.askPerson(context.WithoutCancel(ctx), person, flight)
	case c.now().Sub(flight.startedAt) >= failureTTL:
		c.mu.Unlock()
		return cachedPerson{}, fmt.Errorf("%w: dispatch has been asked about %s for over %s with no answer", errStillAsking, person, failureTTL)
	}
	hook := c.joined
	c.mu.Unlock()
	if joined && hook != nil {
		hook()
	}
	select {
	case <-flight.done:
		return flight.answer, flight.err
	case <-ctx.Done():
		return cachedPerson{}, fmt.Errorf("projectaccess: ask dispatch about %s: %w", person, ctx.Err())
	}
}

// personAnswerNow is personAnswer with no failure an earlier read kept and no call already in
// flight: an answer from the cache younger than keep says unless ctx is one Fresh made, else a
// call of its own on ctx, settled as any other. A read under Fresh asks it, whatever CachedOnly
// says (no caller marks both: the hold gate is the one caller that marks CachedOnly, and the
// request machine's project reads, the ones that mark Fresh, never check a hold), and so does a
// read whose failure lets something through (ListAllowed, and an approver's revoke,
// requests.Machine.RevokeByApprover), so that what it lets through lasts no longer than Dispatch
// does not answer. The call runs on the caller's own context, so a call that failed because that
// context ended, its deadline passed or its caller cancelled, keeps no failure for the person's
// next reads: that ending says nothing of Dispatch. An answer is kept (keepPerson) before it is
// settled.
func (c *Client) personAnswerNow(ctx context.Context, person string, keep func(cachedPerson) time.Duration) (cachedPerson, error) {
	c.mu.Lock()
	if cached, ok := c.people[person]; ok && !fresh(ctx) && c.now().Sub(cached.at) < keep(cached) {
		c.mu.Unlock()
		return cached, nil
	}
	c.mu.Unlock()
	answer, err := c.readPerson(ctx, person, c.now())
	if err != nil && ctx.Err() != nil {
		return cachedPerson{}, err
	}
	if err == nil {
		c.keepPerson(ctx, person, answer)
	}
	c.mu.Lock()
	c.settleLocked(person, answer, err)
	c.mu.Unlock()
	return answer, err
}

// askPerson makes flight's one call of the person route for person, keeps an answer (keepPerson)
// and settles it (settleLocked). The flight leaves c.flights under the same lock, so a read that
// finds no flight finds what this one settled, and every reader of the flight returns after its
// answer is kept.
func (c *Client) askPerson(ctx context.Context, person string, flight *personFlight) {
	answer, err := c.readPerson(ctx, person, c.now())
	if err == nil {
		c.keepPerson(ctx, person, answer)
	}
	c.mu.Lock()
	delete(c.flights, person)
	c.settleLocked(person, answer, err)
	c.mu.Unlock()
	flight.answer, flight.err = answer, err
	close(flight.done)
}

// settleLocked keeps the outcome of a call of the person route for person: an answer is kept,
// aged from before its question, unless one to a question asked later is already held, and clears
// a kept failure; a failure is kept failureTTL from when it came back. A caller whose own context
// ended settles nothing (personAnswerNow), and the shared call runs on no caller's context
// (askPerson). c.mu is held.
func (c *Client) settleLocked(person string, answer cachedPerson, err error) {
	if len(c.people) >= maxCached || len(c.failures) >= maxCached {
		c.sweepLocked()
	}
	if err != nil {
		if len(c.failures) < maxCached {
			c.failures[person] = failedRead{err: err, at: c.now()}
		}
		return
	}
	delete(c.failures, person)
	if kept, ok := c.people[person]; (!ok || kept.at.Before(answer.at)) && len(c.people) < maxCached {
		c.people[person] = answer
	}
}

// readPerson asks Dispatch's person route about person, a call bounded by requestTimeout, and
// answers what it said, stamped asked. The member kind is decoded as redact.RemoteText, which get
// fills with the question's record (redact.Answer.Decode), and read here, where that record is at
// hand, into the two words a cachedPerson keeps: the kind of the closed set it came as
// (knownKind), and the rule's reading of the field. The field itself is left here, so no answer
// this client keeps holds it. A 404, whatever its code, is no answer about a person.
func (c *Client) readPerson(ctx context.Context, person string, asked time.Time) (cachedPerson, error) {
	var answer struct {
		MemberKind   redact.RemoteText `json:"member_kind"`
		Held         *bool             `json:"held"`
		AsOf         time.Time         `json:"as_of"`
		NotHeldUntil time.Time         `json:"not_held_until"`
	}
	status, err := c.get(ctx, personPath, url.Values{"person": {person}}, &answer)
	if err != nil {
		return cachedPerson{}, err
	}
	if status == http.StatusNotFound {
		return cachedPerson{}, fmt.Errorf("projectaccess: dispatch answered %d with no answer for %s", status, person)
	}
	return cachedPerson{kind: answer.MemberKind.String(), known: knownKind(answer.MemberKind), held: answer.Held, asOf: answer.AsOf, until: answer.NotHeldUntil, at: asked}, nil
}

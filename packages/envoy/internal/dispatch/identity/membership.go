package identity

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/cipher"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/oidc"
)

const (
	// MembershipMaxAge is how long a confirmation of a person's membership stands: the first
	// request after it lapses refreshes the person's sign-in with the issuer, so someone the rule
	// no longer admits loses Dispatch within the hour.
	MembershipMaxAge = time.Hour
	// refreshTimeout bounds a refresh, which runs on a context of its own.
	refreshTimeout = 15 * time.Second
)

// standingWait bounds how long Standing waits on a refresh before it answers by the 24-hour rule.
const standingWait = 2 * time.Second

// graceWithoutGoogle is how long a person stays not held while Google cannot be reached.
const graceWithoutGoogle = 24 * time.Hour

// unavailablePause is how long Standing starts no new refresh, and answers as that refresh's outcome did, after a
// refresh of a person in this process ended with Google not reached or access lists that could not be read.
const unavailablePause = 60 * time.Second

// errOtherAccount is a refreshed ID token that names another Google account or another address
// than the row the refresh read: under a Google rule refresh compares both before the rule runs,
// and the rule's own ConfirmSubject refusal wraps it too. It holds the person (endModeFor).
var errOtherAccount = errors.New("the refreshed sign-in names another Google account or another address")

// Refresher renews a sign-in with its refresh token; *oidc.CodeFlow is the one Dispatch uses. Its
// error matches oidc.ErrGrantRefused when the issuer refused the refresh token, and
// oidc.ErrIssuerUnavailable when the issuer could not be asked or did not answer with a verified
// ID token.
type Refresher interface {
	Refresh(ctx context.Context, refreshToken string) (oidc.Session, error)
}

// Membership confirms that a person signed in through the sign-in issuer may still use Dispatch:
// a confirmation younger than MembershipMaxAge stands, and an older one is renewed by refreshing
// the person's sign-in. The confirmation's age is the people store's (the database's clock, as
// auth.PersonMembership.ConfirmedAge says), never this process's clock, so server processes whose
// clocks disagree still agree on when a confirmation lapses. A refresh the issuer refuses
// (oidc.ErrGrantRefused), or one whose ID token no longer names this person under Rule, ends
// every session the person holds: it advances their session generation and, under a Google rule
// (Rule.Holds), records a hold or, for a restricted member's list refusals, keeps the refresh
// token with the row signed out (endModeFor); under the sign-in pool's rule it forgets the refresh
// token. Either way they sign in again or not at all. Under a domain the refreshed token must also
// name the Google account and the address the row records: an address Google gave to another
// account ends the same way, and a row no domain sign-in recorded ends forgetting its token. A
// refresh that fails for any other reason (the issuer is down, slow or answers what does not
// verify: oidc.ErrIssuerUnavailable) ends nothing: the request that found the confirmation lapsed
// is refused with ErrSignInUnavailable, since no one is served on a lapsed confirmation, and the
// session generation and refresh token stay, so the first request after the issuer answers again
// refreshes and is served. Under OutsideRule, access lists that cannot be read
// (ErrMembershipUnreadable) end nothing either: the request is refused with that error and the
// sign-in kept, so a database blip signs nobody out.
//
// Standing answers whether a person's personal tokens and broker credentials are held. Under IAP
// identity (Directory set) no refresh token is kept, and Standing and the membership pass follow the
// person's Google Workspace account in the Directory instead (directoryStanding, ConfirmDirectory).
type Membership struct {
	People auth.PeopleStore
	SignIn Refresher
	Rule   SignInRule
	// Directory checks each person's Workspace account through the Directory API under IAP
	// identity (DISPATCH_IDENTITY=iap): nil outside IAP. With it set, nothing is refreshed, and
	// People must also be a DirectoryPeople, which records each check's outcome.
	Directory *DirectoryReader
	// Outliving answers whether an address still has access that outlives the browser, the
	// question a sign-out asks (api.Deps.Outliving, which cmd/dispatch hands over once the router
	// is built). Under IAP identity a Directory answer for an address deleted and created again
	// confirms the new account only while it answers no (checkDirectory). Its zero value has no
	// database and answers an error, so it confirms nobody.
	Outliving cipher.OutlivingAccess

	// refreshes holds one refresh per person in flight in this process: a page load's many
	// requests and Standing's calls share it. Another server process refreshes on its own, so two
	// processes that find one confirmation lapsed at once may each refresh it; each records the
	// refresh token its issuer answered, and the later write stands.
	refreshes singleflight.Group
	// checks holds one Directory check per person in flight in this process, which Standing and
	// the membership pass share, as refreshes does for a refresh.
	checks singleflight.Group
	// unavailable maps an email to the unavailable outcome (its time and error) of the last
	// refresh of that person in this process, while that refresh ended with ErrSignInUnavailable
	// or ErrMembershipUnreadable, or of the last Directory check while it was Failed: Standing
	// starts no refresh or check for unavailablePause after it.
	unavailable sync.Map
	// started maps an email to the start time of the refresh, or the Directory check, running for
	// that person in this process, so Standing does not wait on one that has already run past
	// standingWait.
	started sync.Map
}

// unavailableOutcome is what refresh stores in Membership.unavailable.
type unavailableOutcome struct {
	at  time.Time
	err error
}

// RefreshOutcome is what a refresh of a person did, for the membership pass's counts.
type RefreshOutcome int

const (
	// RefreshNone is no refresh: the confirmation was fresh, or a failure came before Google was
	// asked.
	RefreshNone RefreshOutcome = iota
	// RefreshConfirmed is Google's confirmation of the person, whether or not ConfirmSubject's
	// guard missed.
	RefreshConfirmed
	// RefreshHeld is an end with auth.EndHold whose EndSubject answered true. Under IAP identity it
	// is a Held Directory outcome, and an Active one that wrote nothing because another check recorded
	// a hold after this one read the row, so the person stays held (ConfirmDirectory).
	RefreshHeld
	// RefreshKept is an end with auth.EndKeep whose EndSubject answered true.
	RefreshKept
	// RefreshForgot is an end with auth.EndForget whose EndSubject answered true.
	RefreshForgot
	// RefreshEndMissed is an end whose EndSubject answered false: the row was signed in to again
	// since the refresh read it.
	RefreshEndMissed
	// RefreshFailed is ErrSignInUnavailable, a token that does not open, ErrMembershipUnreadable,
	// or another error.
	RefreshFailed
)

// endOutcomes is the outcome of an end in each mode whose EndSubject answered true.
var endOutcomes = map[auth.EndMode]RefreshOutcome{
	auth.EndHold:   RefreshHeld,
	auth.EndKeep:   RefreshKept,
	auth.EndForget: RefreshForgot,
}

// Standing is whether Dispatch holds a person's personal tokens and broker credentials
// (Membership.Standing). ByGrace is true when the 24-hour rule or the unopenable rule answered
// held, and Unopenable when the unopenable rule did. AsOf is the database time of the read the
// answer rests on: the first read, or the re-read after a refresh or a Directory check.
//
// NotHeldUntil is set under IAP identity alone, on an answer that is not held: the last Active
// Directory answer plus 24 hours, after which the answer is no longer served, or AsOf itself for an
// answer that is unavailable (Unavailable). It is zero on a held answer and outside IAP.
type Standing struct {
	Held         bool
	ByGrace      bool
	Unopenable   bool
	AsOf         time.Time
	NotHeldUntil time.Time
}

// Unavailable reports an answer under IAP identity that is neither held nor served: no Active
// Directory answer is younger than 24 hours. Standing writes it as NotHeldUntil equal to AsOf, and
// a not-held answer always carries a NotHeldUntil later than its AsOf, so it is judged on the
// database's clock, as every confirmation is. A personal token is then 503 IDENTITY_UNAVAILABLE,
// never served and never 401 MEMBERSHIP_HELD.
func (s Standing) Unavailable() bool {
	return !s.Held && !s.NotHeldUntil.IsZero() && !s.NotHeldUntil.After(s.AsOf)
}

// ChecksDirectory reports whether m follows the Workspace Directory (IAP identity, Directory set),
// where no refresh token or sign-in confirmation is kept: a caller's own read of those columns then
// answers nothing, and only Standing does.
func (m *Membership) ChecksDirectory() bool {
	return m.Directory != nil
}

// Confirm returns nil when email may still use Dispatch, ErrNoIdentity when their sign-in has
// ended (now or earlier), an error matching ErrSignInUnavailable, the refresh's own failure in its
// chain, when the confirmation has lapsed and the issuer could not renew it now, and any other
// error when the membership could not be read or recorded.
func (m *Membership) Confirm(ctx context.Context, email string) error {
	_, err := m.ConfirmFor(ctx, email)
	return err
}

// ConfirmFor is Confirm with what its refresh did (RefreshOutcome), for the membership pass's
// counts: RefreshNone when the confirmation was fresh, or a failure came before Google was asked.
func (m *Membership) ConfirmFor(ctx context.Context, email string) (RefreshOutcome, error) {
	membership, found, err := m.People.Membership(ctx, email)
	if err != nil {
		return RefreshNone, err
	}
	if found && m.fresh(membership) {
		return RefreshNone, nil
	}
	return outcomeOf(<-m.flight(ctx, email))
}

// flight starts, or joins, the one refresh of email in flight in this process. The refresh
// belongs to the person, not to the request that found it due: a browser hanging up must neither
// cancel it nor have its cancellation read as the issuer's refusal, and a caller that stops
// waiting (Standing) leaves it to run to its own bound. It runs on detached(ctx).
func (m *Membership) flight(ctx context.Context, email string) <-chan singleflight.Result {
	return m.refreshes.DoChan(email, func() (any, error) {
		refreshCtx, cancel := context.WithTimeout(detached(ctx), refreshTimeout)
		defer cancel()
		outcome, err := m.refresh(refreshCtx, email)
		return outcome, err
	})
}

// detached is the context a flight - a refresh, or a Directory check - runs on, from ctx, the
// context of the request or pass that started it: ctx's values, none of its cancellation or
// deadline, and none of the request's own pool bookkeeping. A flight outlives the request and
// writes the person's state, never the request's work, so it takes no commit step
// (store.WithoutBeforeCommit): a keyed request's step taken by the flight's transaction would mark
// the request's Idempotency-Key committed for a write it never made. And it holds a hold mark of
// its own (store.ForConcurrentWorker): sharing the request's would have the pool read the
// request's held connection as the flight's, and the flight's as the request's, and refuse the
// second one (store.ErrNestedAcquire), once the request goes on after Standing stops waiting.
func detached(ctx context.Context) context.Context {
	return store.ForConcurrentWorker(store.WithoutBeforeCommit(context.WithoutCancel(ctx)))
}

// outcomeOf is the RefreshOutcome and error of a finished flight.
func outcomeOf(result singleflight.Result) (RefreshOutcome, error) {
	outcome, _ := result.Val.(RefreshOutcome)
	return outcome, result.Err
}

// refresh renews email's confirmation with the issuer and answers what it did. It records its
// start in m.started while it runs, and, as it ends, stores its outcome in m.unavailable when it
// is ErrSignInUnavailable or ErrMembershipUnreadable, and deletes any entry otherwise.
func (m *Membership) refresh(ctx context.Context, email string) (outcome RefreshOutcome, err error) {
	m.started.Store(email, time.Now())
	defer func() {
		m.started.Delete(email)
		if errors.Is(err, ErrSignInUnavailable) || errors.Is(err, ErrMembershipUnreadable) {
			m.unavailable.Store(email, unavailableOutcome{at: time.Now(), err: err})
		} else {
			m.unavailable.Delete(email)
		}
	}()
	// Read again under the flight: a refresh that finished just before this one began has
	// already confirmed the person.
	membership, found, err := m.People.Membership(ctx, email)
	if err != nil {
		return RefreshNone, err
	}
	if !found || membership.RefreshToken == "" {
		if found && membership.Unopenable {
			return RefreshFailed, ErrNoIdentity
		}
		return RefreshNone, ErrNoIdentity
	}
	if m.fresh(membership) {
		return RefreshNone, nil
	}
	if !m.Rule.CanRefresh(membership.Subject) {
		return m.end(ctx, email, membership, "the address's sign-in recorded no Google account to confirm", auth.EndForget)
	}
	session, err := m.SignIn.Refresh(ctx, membership.RefreshToken)
	if errors.Is(err, oidc.ErrGrantRefused) {
		return m.end(ctx, email, membership, err.Error(), m.endMode(membership, err))
	}
	if err != nil {
		slog.Warn("dispatch: the sign-in issuer could not confirm a lapsed membership; the request is refused and the sign-in kept", "email", email, "error", err)
		return RefreshFailed, fmt.Errorf("%w: %w", ErrSignInUnavailable, err)
	}
	// Under a Google rule the refreshed token must name the address and the account the row
	// records, compared before the rule runs: a restricted member's new address that no list
	// names is then held, not kept as a list refusal. The sign-in pool's rule takes the address
	// from the pool's username and records no subject, so it compares neither.
	if m.Rule.Holds() {
		if login := model.CanonicalLogin(session.Claims.Email); login != email || session.Claims.Subject != membership.Subject {
			err := fmt.Errorf("%w: the refreshed sign-in is %q under Google account %q, and the address's sign-in recorded %q", errOtherAccount, login, session.Claims.Subject, membership.Subject)
			return m.end(ctx, email, membership, err.Error(), auth.EndHold)
		}
	}
	person, err := m.Rule.Person(ctx, session.Claims)
	if errors.Is(err, ErrMembershipUnreadable) {
		slog.Warn("dispatch: the access lists could not be read to confirm a lapsed membership; the request is refused and the sign-in kept", "email", email, "error", err)
		return RefreshFailed, err
	}
	if err != nil {
		return m.end(ctx, email, membership, err.Error(), m.endMode(membership, err))
	}
	if person.Email != email {
		err := fmt.Errorf("%w: the refreshed sign-in names %s", errOtherAccount, person.Email)
		return m.end(ctx, email, membership, err.Error(), m.endMode(membership, err))
	}
	subject, err := m.Rule.ConfirmSubject(session.Claims, membership.Subject)
	if err != nil {
		return m.end(ctx, email, membership, err.Error(), m.endMode(membership, err))
	}
	if err := m.Rule.WriteConfirm(ctx, m.People, membership, person, subject, session.RefreshToken); err != nil {
		return RefreshFailed, err
	}
	return RefreshConfirmed, nil
}

// end ends email's sign-in in mode under m.Rule's end policy (SignInRule.End), naming read, the
// membership this confirmation read, so a sign-in recorded since it read is not the one it ends,
// and answers what the end did.
func (m *Membership) end(ctx context.Context, email string, read auth.PersonMembership, reason string, mode auth.EndMode) (RefreshOutcome, error) {
	ended, err := m.Rule.End(ctx, m.People, email, read, reason, mode)
	switch {
	case ended:
		return endOutcomes[mode], err
	case errors.Is(err, ErrNoIdentity):
		return RefreshEndMissed, err
	default:
		return RefreshFailed, err
	}
}

// endMode is how a refresh's end for err ends the sign-in read: endModeFor under a Google rule,
// and auth.EndForget under the sign-in pool's, which holds nobody.
func (m *Membership) endMode(read auth.PersonMembership, err error) auth.EndMode {
	if !m.Rule.Holds() {
		return auth.EndForget
	}
	return endModeFor(read, err)
}

// endModeFor is how an end of the sign-in read, for err, ends it. Google's own
// refusal of the account holds anyone. A restricted member's admission refusals are the restricted
// admission rule's (RestrictedAdmission), which admits them again with no sign-in, so they keep the
// token for membership checks and hold nobody. Any other refusal holds a full member.
func endModeFor(read auth.PersonMembership, err error) auth.EndMode {
	switch {
	case errors.Is(err, oidc.ErrGrantRefused), errors.Is(err, errOtherAccount),
		errors.Is(err, ErrNoPerson), errors.Is(err, ErrNoEmail), errors.Is(err, ErrEmailNotVerified):
		return auth.EndHold
	case read.Kind == model.MemberRestricted &&
		(errors.Is(err, ErrNotInDomain) || errors.Is(err, ErrNotListed) || errors.Is(err, ErrWorkspaceAddress)):
		return auth.EndKeep
	default:
		return auth.EndHold
	}
}

// Standing answers whether Dispatch holds email's personal tokens and broker credentials. It reads
// email as model.CanonicalLogin spells it, as people.email holds it, reads the row and the
// database's time in one statement, and takes these steps in order, the first that applies
// answering:
//
//  1. under a rule other than the two Google rules (Rule.Holds false): not held, nothing asked;
//  2. a recorded hold: held;
//  3. no row, or no stored refresh token: not held, except that a stored token that does not open,
//     on a row that records a Google account, answers by the unopenable rule;
//  4. a confirmation younger than MembershipMaxAge: not held;
//  5. within unavailablePause after a refresh of this person in this process ended with Google not
//     reached or with access lists that could not be read: no new refresh, and the answer that
//     outcome gives in step 6; and while a refresh of this person in this process has run for more
//     than standingWait, the 24-hour rule at once;
//  6. otherwise it starts, or joins, the person's refresh (the flight Confirm shares) and waits for
//     it at most standingWait from its own call. A refresh that confirmed the person or ended the
//     sign-in re-reads the row and answers its recorded hold, since another process can record a
//     hold while this refresh's guarded write misses; Google not reached answers the 24-hour rule;
//     any other failure, ErrMembershipUnreadable among them, is returned while the confirmation is
//     younger than graceWithoutGoogle, and the 24-hour rule answers after; and the wait running out
//     answers the 24-hour rule, and starts no pause.
//
// The 24-hour rule is not held while the last confirmation is younger than graceWithoutGoogle, and
// held, ByGrace, after; the unopenable rule is the same, with Unopenable set. Neither records
// anything. AsOf is the ReadAt of the read the answer rests on: the first read, or the re-read.
//
// Under IAP identity (Directory set) step 1 treats the rule as one that holds, and the steps after
// the read are directoryStanding's: the Workspace Directory, never a refresh, answers.
func (m *Membership) Standing(ctx context.Context, email string) (Standing, error) {
	begun := time.Now()
	email = model.CanonicalLogin(email)
	membership, found, err := m.People.Membership(ctx, email)
	if err != nil {
		return Standing{}, err
	}
	if m.Directory != nil {
		return m.directoryStanding(ctx, email, membership, found, begun)
	}
	answer := Standing{AsOf: membership.ReadAt}
	if !m.Rule.Holds() {
		return answer, nil
	}
	if membership.Held {
		answer.Held = true
		return answer, nil
	}
	if !found || membership.RefreshToken == "" {
		if found && membership.Unopenable && m.Rule.CanRefresh(membership.Subject) {
			answer = graceRule(answer, membership)
			answer.Unopenable = answer.Held
		}
		return answer, nil
	}
	if m.fresh(membership) {
		return answer, nil
	}
	if paused, ok := m.unavailable.Load(email); ok {
		if outcome := paused.(unavailableOutcome); time.Since(outcome.at) < unavailablePause {
			return failureAnswer(answer, membership, outcome.err)
		}
	}
	if started, ok := m.started.Load(email); ok && time.Since(started.(time.Time)) > standingWait {
		return graceRule(answer, membership), nil
	}
	flight := m.flight(ctx, email)
	wait := time.NewTimer(max(standingWait-time.Since(begun), 0))
	defer wait.Stop()
	select {
	case result := <-flight:
		if result.Err == nil || errors.Is(result.Err, ErrNoIdentity) {
			reread, _, err := m.People.Membership(ctx, email)
			if err != nil {
				return Standing{}, err
			}
			return Standing{Held: reread.Held, AsOf: reread.ReadAt}, nil
		}
		return failureAnswer(answer, membership, result.Err)
	case <-wait.C:
		return graceRule(answer, membership), nil
	}
}

// failureAnswer is Standing's answer, on answer and read, the first read, to a refresh that failed
// with err: the 24-hour rule for Google not reached (ErrSignInUnavailable); for any other failure,
// ErrMembershipUnreadable among them, err while read's confirmation is younger than
// graceWithoutGoogle, and the 24-hour rule after.
func failureAnswer(answer Standing, read auth.PersonMembership, err error) (Standing, error) {
	if !errors.Is(err, ErrSignInUnavailable) && read.ConfirmedAge < graceWithoutGoogle {
		return Standing{}, err
	}
	return graceRule(answer, read), nil
}

// graceRule is the 24-hour rule on answer: held, ByGrace, once read's confirmation is
// graceWithoutGoogle old; answer as it is before.
func graceRule(answer Standing, read auth.PersonMembership) Standing {
	if read.ConfirmedAge >= graceWithoutGoogle {
		answer.Held, answer.ByGrace = true, true
	}
	return answer
}

// signedOut is ErrNoIdentity for email, whose session has ended, told as SignedOut when Rule names
// the reason (SignInRule.SignedOutReason). A reason Rule cannot read (the database or the access
// lists not answering) is no reason: the request is refused either way, so it is ErrNoIdentity
// alone, logged.
func (m *Membership) signedOut(ctx context.Context, email string) error {
	reason, err := m.Rule.SignedOutReason(ctx, m.People, email)
	if err != nil {
		slog.Warn("dispatch: could not read why a person's session ended; the request is refused without a reason", "email", email, "error", err)
		return ErrNoIdentity
	}
	if reason == "" {
		return ErrNoIdentity
	}
	return &SignedOut{Email: email, Reason: reason}
}

// fresh reports whether membership's confirmation stands: it exists and the store measures it
// younger than MembershipMaxAge.
func (m *Membership) fresh(membership auth.PersonMembership) bool {
	return membership.Confirmed && membership.ConfirmedAge < MembershipMaxAge
}

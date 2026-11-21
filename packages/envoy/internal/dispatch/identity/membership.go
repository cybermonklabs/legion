package identity

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/sjawhar/envoy/internal/dispatch/admission"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
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
// every session the person holds: it advances their session generation and forgets their refresh
// token, so they sign in again or not at all. Under a domain the refreshed token must also name the
// Google account the address's row records (Subject): an address Google gave to another account,
// or a row no domain sign-in recorded, ends the same way. A refresh that fails for any other
// reason (the issuer is down, slow or answers what does not verify: oidc.ErrIssuerUnavailable)
// ends nothing: the request that found the confirmation lapsed is refused with
// ErrSignInUnavailable, since no one is served on a lapsed confirmation, and the session
// generation and refresh token stay, so the first request after the issuer answers again
// refreshes and is served.
type Membership struct {
	People   auth.PeopleStore
	Sessions auth.SessionStore
	SignIn   Refresher
	Rule     SignInRule

	// refreshes holds one refresh per person in flight in this process: a page load's many
	// requests share it. Another server process refreshes on its own, so two processes that find
	// one confirmation lapsed at once may each refresh it; each records the refresh token its
	// issuer answered, and the later write stands.
	refreshes singleflight.Group
}

// Confirm returns nil when email may still use Dispatch, ErrNoIdentity when their sign-in has
// ended (now or earlier), an error matching ErrSignInUnavailable, the refresh's own failure in its
// chain, when the confirmation has lapsed and the issuer could not renew it now, and any other
// error when the membership could not be read or recorded.
//
// Work outside admission that holds a place of the request pool's share for its reads (a
// document socket's admission, a heartbeat check: admission.PlaceFrom) holds it here for the
// reads alone. A confirmation that has lapsed is renewed under a flight of the person's, which
// waits on the sign-in issuer for up to refreshTimeout, so Confirm gives the place back before it
// joins the flight, whether it leads the flight or waits on another request's, and the flight's
// refresh holds the place of the work that leads it for its own reads and writes alone (refresh).
// A Confirm that refreshes returns with the place given back, and the work takes it again for
// what it reads next (admission.Place.Retake). A request inside admission carries no place, and
// its refresh runs under its admission unit.
func (m *Membership) Confirm(ctx context.Context, email string) error {
	membership, found, err := m.People.Membership(ctx, email)
	if err != nil {
		return err
	}
	if found && m.fresh(membership) {
		return nil
	}
	place := admission.PlaceFrom(ctx)
	place.Release()
	// The refresh belongs to the person, not to the request that found it due: a browser
	// hanging up must neither cancel it nor have its cancellation read as the issuer's refusal.
	_, err, _ = m.refreshes.Do(email, func() (any, error) {
		refreshCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), refreshTimeout)
		defer cancel()
		return nil, m.refresh(refreshCtx, place, email)
	})
	return err
}

// refresh renews email's lapsed confirmation with the sign-in issuer, for the flight Confirm
// shares among the person's requests. place is the place of the work that leads the flight, which
// Confirm gave back, nil for a request inside admission: the refresh holds it for its reads and
// writes of the person's row, and not while it waits on the issuer, and gives it back as it
// returns.
func (m *Membership) refresh(ctx context.Context, place *admission.Place, email string) error {
	defer place.Release()
	if err := retake(ctx, place, "a sign-in's refresh"); err != nil {
		return err
	}
	// Read again under the flight: a refresh that finished just before this one began has
	// already confirmed the person.
	membership, found, err := m.People.Membership(ctx, email)
	if err != nil {
		return err
	}
	if !found || membership.RefreshToken == "" {
		return ErrNoIdentity
	}
	if m.fresh(membership) {
		return nil
	}
	if !m.Rule.CanRefresh(membership.Subject) {
		return m.end(ctx, email, membership.Subject, "the address's sign-in recorded no Google account to confirm")
	}
	place.Release()
	session, err := m.SignIn.Refresh(ctx, membership.RefreshToken)
	refused := errors.Is(err, oidc.ErrGrantRefused)
	if err != nil && !refused {
		slog.Warn("dispatch: the sign-in issuer could not confirm a lapsed membership; the request is refused and the sign-in kept", "email", email, "error", err)
		return fmt.Errorf("%w: %w", ErrSignInUnavailable, err)
	}
	// What follows writes the person's row: the end of the sign-in the issuer refused, or the
	// confirmation it gave.
	if busy := retake(ctx, place, "a sign-in's refresh"); busy != nil {
		return busy
	}
	if refused {
		return m.end(ctx, email, membership.Subject, err.Error())
	}
	person, err := m.Rule.Person(session.Claims)
	if err != nil {
		return m.end(ctx, email, membership.Subject, err.Error())
	}
	if person != email {
		return m.end(ctx, email, membership.Subject, "the refreshed sign-in names "+person)
	}
	subject, err := m.Rule.ConfirmSubject(session.Claims, membership.Subject)
	if err != nil {
		return m.end(ctx, email, membership.Subject, err.Error())
	}
	return m.Rule.WriteConfirm(ctx, m.People, email, subject, session.RefreshToken)
}

// retake takes place again for what, the work that reads and writes the person's row once the
// place was given back for a wait outside the database: a lapsed sign-in's refresh after the
// issuer's (refresh), an IAP person's record after the read of IAP's key set (IAPIdentity.Login).
// A place refused - every place held and none free within the wait, or the queue for one full - or
// not free before the work's own bound passed is a bound of this server's own that ended the work
// before it asked the database anything, so nothing of it was written. It is answered as a pool
// with no connection free is (store.ErrPoolBusy): a request told so tries again as a busy pool
// tells it to - every request waiting on a refresh's flight, and the next one refreshes. A refusal
// after the issuer answered drops that answer, as a write the busy pool refused would.
func retake(ctx context.Context, place *admission.Place, what string) error {
	if err := place.Retake(ctx); err != nil {
		return fmt.Errorf("%w: no place of the request pool's share outside admission came free for %s: %w", store.ErrPoolBusy, what, err)
	}
	return nil
}

// end ends email's sign-in under m.Rule's end policy (SignInRule.End).
func (m *Membership) end(ctx context.Context, email, subject, reason string) error {
	return m.Rule.End(ctx, m.People, m.Sessions, email, subject, reason)
}

// fresh reports whether membership's confirmation stands: it exists and the store measures it
// younger than MembershipMaxAge.
func (m *Membership) fresh(membership auth.PersonMembership) bool {
	return membership.Confirmed && membership.ConfirmedAge < MembershipMaxAge
}

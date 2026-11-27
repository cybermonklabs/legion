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
// A confirmation that has lapsed is renewed under a flight of the person's, which waits on the
// sign-in issuer for up to refreshTimeout and reads nothing of the pool while it does. So Confirm
// gives back the unit of the server's room ctx's request holds before it joins the flight, whether
// it leads the flight or waits on another request's - an admitted request's admission units, the
// place of the request pool's share outside admission a document socket's admission or a heartbeat
// check holds, and a pinned read's connection (admission.GiveBack) - and takes it back once the
// flight has answered, whatever it answered (admission.Away.TakeBack), so it returns holding what
// it was called holding, or refused. The flight's refresh holds the unit of the request that leads
// it for its own reads and writes of the person's row alone, and not while it waits on the issuer
// (refresh). A take-back refused is Confirm's answer: admission's refusal for an admitted request,
// a busy pool (store.ErrPoolBusy) for a place, after which nothing of the refresh is written.
func (m *Membership) Confirm(ctx context.Context, email string) error {
	membership, found, err := m.People.Membership(ctx, email)
	if err != nil {
		return err
	}
	if found && m.fresh(membership) {
		return nil
	}
	away := admission.GiveBack(ctx)
	// The refresh belongs to the person, not to the request that found it due: a browser
	// hanging up must neither cancel it nor have its cancellation read as the issuer's refusal.
	_, err, _ = m.refreshes.Do(email, func() (any, error) {
		refreshCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), refreshTimeout)
		defer cancel()
		return nil, m.refresh(refreshCtx, away, email)
	})
	if back := away.TakeBack(ctx); back != nil {
		return back
	}
	return err
}

// refresh renews email's lapsed confirmation with the sign-in issuer, for the flight Confirm
// shares among the person's requests. away is what the request that leads the flight gave back
// before it joined it (Confirm): the refresh takes it back for its reads and writes of the
// person's row, gives it back again while it waits on the issuer, and takes it back for what it
// writes once the issuer has answered (admission.Away.TakeBack), so the issuer's round trip holds
// no unit. A take-back refused ends the refresh before it asks the database anything, so nothing
// of it is written, and every request waiting on the flight is told so and tries again.
func (m *Membership) refresh(ctx context.Context, away admission.Away, email string) error {
	if err := away.TakeBack(ctx); err != nil {
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
	issuer := admission.GiveBack(ctx)
	session, err := m.SignIn.Refresh(ctx, membership.RefreshToken)
	refused := errors.Is(err, oidc.ErrGrantRefused)
	if err != nil && !refused {
		slog.Warn("dispatch: the sign-in issuer could not confirm a lapsed membership; the request is refused and the sign-in kept", "email", email, "error", err)
		return fmt.Errorf("%w: %w", ErrSignInUnavailable, err)
	}
	// What follows writes the person's row: the end of the sign-in the issuer refused, or the
	// confirmation it gave.
	if busy := issuer.TakeBack(ctx); busy != nil {
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

// end ends email's sign-in under m.Rule's end policy (SignInRule.End).
func (m *Membership) end(ctx context.Context, email, subject, reason string) error {
	return m.Rule.End(ctx, m.People, m.Sessions, email, subject, reason)
}

// fresh reports whether membership's confirmation stands: it exists and the store measures it
// younger than MembershipMaxAge.
func (m *Membership) fresh(membership auth.PersonMembership) bool {
	return membership.Confirmed && membership.ConfirmedAge < MembershipMaxAge
}

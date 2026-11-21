package identity

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"golang.org/x/sync/singleflight"

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
// refreshes and is served. Under OutsideRule, access lists that cannot be read
// (ErrMembershipUnreadable) end nothing either: the request is refused with that error and the
// sign-in kept, so a database blip signs nobody out.
//
// A passkey's sign-in (its row's account of the outside origin, auth.PersonMembership.Issuer)
// has no issuer to refresh with: Passkeys confirms it. It stands only while Passkeys is set and
// names that issuer, so turning passkey sign-in off (or moving the outside origin) refuses its
// cookie at its next use, writing nothing. A confirmation older than MembershipMaxAge asks Rule's
// AdmitsRestricted again and finds a passkey: a person no list names directly any more, or whose
// address is on a domain the Workspace owns, has their sign-in ended and their passkeys deleted in
// one transaction (PasskeySignIns.End), and a person with no passkey left has their sign-in ended
// (PasskeySignIns.Confirm); lists that cannot be read end nothing, as above.
type Membership struct {
	People auth.PeopleStore
	SignIn Refresher
	Rule   SignInRule
	// Passkeys confirms a passkey's sign-in hourly; nil where passkey sign-in is off.
	Passkeys PasskeySignIns

	// refreshes holds one refresh per person in flight in this process: a page load's many
	// requests share it. Another server process refreshes on its own, so two processes that find
	// one confirmation lapsed at once may each refresh it; each records the refresh token its
	// issuer answered, and the later write stands.
	refreshes singleflight.Group
}

// PasskeySignIns is the hourly confirmation's store for a passkey's sign-in (store.PgPasskeys).
type PasskeySignIns interface {
	// Issuer is the issuer a passkey's subject is recorded under, the outside origin.
	Issuer() string
	// Confirm stamps the confirmation of email's sign-in under subject while it stands and the
	// person holds a passkey, ending it when they hold none, and reports whether it stands.
	Confirm(ctx context.Context, email, subject string) (bool, error)
	// End ends email's sign-in under subject and deletes their passkeys, in one transaction.
	End(ctx context.Context, email, subject string) (bool, error)
}

// Confirm returns nil when email may still use Dispatch, ErrNoIdentity when their sign-in has
// ended (now or earlier), an error matching ErrSignInUnavailable, the refresh's own failure in its
// chain, when the confirmation has lapsed and the issuer could not renew it now, and any other
// error when the membership could not be read or recorded.
func (m *Membership) Confirm(ctx context.Context, email string) error {
	membership, found, err := m.People.Membership(ctx, email)
	if err != nil {
		return err
	}
	if found && membership.Issuer != "" {
		return m.confirmPasskey(ctx, email, membership)
	}
	if found && m.fresh(membership) {
		return nil
	}
	// The refresh belongs to the person, not to the request that found it due: a browser
	// hanging up must neither cancel it nor have its cancellation read as the issuer's refusal.
	_, err, _ = m.refreshes.Do(email, func() (any, error) {
		refreshCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), refreshTimeout)
		defer cancel()
		return nil, m.refresh(refreshCtx, email)
	})
	return err
}

// confirmPasskey is Confirm for a passkey's sign-in (Membership's own paragraph on it), membership
// its row as read: one flight per person in this process, on a context of its own, as a refresh.
func (m *Membership) confirmPasskey(ctx context.Context, email string, membership auth.PersonMembership) error {
	if m.Passkeys == nil || membership.Issuer != m.Passkeys.Issuer() || !membership.Confirmed {
		return ErrNoIdentity
	}
	if membership.ConfirmedAge < MembershipMaxAge {
		return nil
	}
	_, err, _ := m.refreshes.Do(email, func() (any, error) {
		confirmCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), refreshTimeout)
		defer cancel()
		return nil, m.confirmLapsedPasskey(confirmCtx, email, membership.Subject)
	})
	return err
}

// confirmLapsedPasskey asks Rule again whether it admits email, whose passkey sign-in under
// subject is past MembershipMaxAge, and ends or confirms it (confirmPasskey).
func (m *Membership) confirmLapsedPasskey(ctx context.Context, email, subject string) error {
	err := m.Rule.AdmitsRestricted(ctx, email)
	if errors.Is(err, ErrMembershipUnreadable) {
		slog.Warn("dispatch: the access lists could not be read to confirm a lapsed passkey sign-in; the request is refused and the sign-in kept", "email", email, "error", err)
		return err
	}
	if err != nil {
		if _, endErr := m.Passkeys.End(ctx, email, subject); endErr != nil {
			return endErr
		}
		slog.Warn("dispatch: sign-in ended: the hourly confirmation no longer admits the person", "email", email, "reason", err.Error())
		return ErrNoIdentity
	}
	confirmed, err := m.Passkeys.Confirm(ctx, email, subject)
	if err != nil {
		return err
	}
	if !confirmed {
		return ErrNoIdentity
	}
	return nil
}

func (m *Membership) refresh(ctx context.Context, email string) error {
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
		return m.end(ctx, email, membership, "the address's sign-in recorded no Google account to confirm")
	}
	session, err := m.SignIn.Refresh(ctx, membership.RefreshToken)
	if errors.Is(err, oidc.ErrGrantRefused) {
		return m.end(ctx, email, membership, err.Error())
	}
	if err != nil {
		slog.Warn("dispatch: the sign-in issuer could not confirm a lapsed membership; the request is refused and the sign-in kept", "email", email, "error", err)
		return fmt.Errorf("%w: %w", ErrSignInUnavailable, err)
	}
	person, err := m.Rule.Person(ctx, session.Claims)
	if errors.Is(err, ErrMembershipUnreadable) {
		slog.Warn("dispatch: the access lists could not be read to confirm a lapsed membership; the request is refused and the sign-in kept", "email", email, "error", err)
		return err
	}
	if err != nil {
		return m.end(ctx, email, membership, err.Error())
	}
	if person.Email != email {
		return m.end(ctx, email, membership, "the refreshed sign-in names "+person.Email)
	}
	subject, err := m.Rule.ConfirmSubject(session.Claims, membership.Subject)
	if err != nil {
		return m.end(ctx, email, membership, err.Error())
	}
	return m.Rule.WriteConfirm(ctx, m.People, membership, person, subject, session.RefreshToken)
}

// end ends email's sign-in under m.Rule's end policy (SignInRule.End), naming read, the membership
// this confirmation read, so a sign-in recorded since it read is not the one it ends.
func (m *Membership) end(ctx context.Context, email string, read auth.PersonMembership, reason string) error {
	return m.Rule.End(ctx, m.People, email, read, reason)
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

package identity

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/errkind"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/oidc"
)

// DomainRule signs in the people of Domain, a Google Workspace domain.
type DomainRule struct {
	Domain string
}

var _ SignInRule = DomainRule{}

// Person names the person by the ID token's `email` claim, which Google writes, and checks the
// account is of the domain by `hd`, the account's Workspace domain, never by the email's own
// domain: the Workspace rule the secrets broker's sign-in applies too (oidc.WorkspaceLogin), each
// of its refusals worded as Dispatch's own. The person is a full member, with the token's `hd`. A
// token with no subject is ErrNoPerson, one with no email of an address's shape ErrNoEmail, one of
// another domain or none ErrNotInDomain, and one whose email Google has not verified
// ErrEmailNotVerified.
func (r DomainRule) Person(_ context.Context, claims oidc.Claims) (SignedIn, error) {
	email, err := oidc.WorkspaceLogin(claims, r.Domain)
	if err == nil {
		return SignedIn{Email: email, Kind: model.MemberFull, HostedDomain: claims.HostedDomain}, nil
	}
	return SignedIn{Email: email}, googleRefusal(email, claims, err)
}

// AdmitsRestricted is ErrOutsideSignInOff: DomainRule admits nobody from outside the domain, so no
// restricted member's credential acts under it. OutsideRule, which embeds this rule, answers for
// itself.
func (r DomainRule) AdmitsRestricted(_ context.Context, email string) error {
	return fmt.Errorf("%s: %w", email, ErrOutsideSignInOff)
}

// googleRefusal words a refusal of oidc.WorkspaceLogin for email as Dispatch's own: ErrNoPerson,
// ErrNoEmail, ErrNotInDomain or ErrEmailNotVerified, naming email when there is one.
func googleRefusal(email string, claims oidc.Claims, err error) error {
	switch {
	case errors.Is(err, oidc.ErrNoSubject):
		return fmt.Errorf("%w: the Google ID token carries no subject", ErrNoPerson)
	case errors.Is(err, oidc.ErrNoEmail):
		return ErrNoEmail
	case errors.Is(err, oidc.ErrNotInDomain):
		return fmt.Errorf("%s (hd %q): %w", email, claims.HostedDomain, ErrNotInDomain)
	case errors.Is(err, oidc.ErrEmailNotVerified):
		return fmt.Errorf("%s: %w", email, ErrEmailNotVerified)
	default:
		return err
	}
}

// RefusalLogFields names the person Google's token carried by their login's hash (`login_hash`,
// empty for a token with no email), and its hd.
func (r DomainRule) RefusalLogFields(claims oidc.Claims) []any {
	return []any{"login_hash", refusedLoginHash(claims.Email), "hd", claims.HostedDomain}
}

// refusedLoginHash is login's hash for a refusal's line (model.LoginHash), or "" for no login,
// whose hash would name nobody.
func refusedLoginHash(login string) string {
	if strings.TrimSpace(login) == "" {
		return ""
	}
	return model.LoginHash(login)
}

// Subject is the Google account (claims.Subject) a domain sign-in records with the person.
func (r DomainRule) Subject(claims oidc.Claims) string {
	return claims.Subject
}

// BootLog logs the issuer, client and Workspace domain boot is signing people in under.
func (r DomainRule) BootLog(issuer, clientID string) {
	slog.Info("dispatch: signing people in with Google for a Workspace domain", "issuer", issuer, "client_id", clientID, "domain", r.Domain)
}

// CanRefresh is true only while stored, the account the address's row recorded, is set: a row
// that records none (a sign-in under a group, from before the column, or any other way) has
// nothing to check a refresh against, so the caller ends without asking Google.
func (r DomainRule) CanRefresh(stored string) bool {
	return stored != ""
}

// Holds is true: Google's refusal of a domain sign-in holds the person (Membership.Standing).
// OutsideRule, which embeds this rule, holds too.
func (r DomainRule) Holds() bool {
	return true
}

// ConfirmSubject requires the refreshed token to still name the account stored records; a
// refreshed token naming another account is an error matching errOtherAccount whose text names
// both, for the caller to end the sign-in with.
func (r DomainRule) ConfirmSubject(claims oidc.Claims, stored string) (string, error) {
	if claims.Subject != stored {
		return "", fmt.Errorf("%w: the refreshed sign-in is Google account %q, and the address's sign-in recorded %q", errOtherAccount, claims.Subject, stored)
	}
	return stored, nil
}

// End ends email's sign-in in mode only while the address's row still records the Google account
// this refresh read and still holds the refresh token it read (endSubject).
func (r DomainRule) End(ctx context.Context, people auth.PeopleStore, email string, read auth.PersonMembership, reason string, mode auth.EndMode) (bool, error) {
	return endSubject(ctx, people, email, read, reason, mode, "dispatch: sign-in ended: Google did not confirm the person's membership of the domain")
}

// endSubject ends email's sign-in in mode only while the address's row still records read's
// subject and holds read's refresh token (auth.PeopleStore.EndSubject), logs ended, the rule's own
// words for what ended it, and answers ended, whether the store wrote. When another account has
// signed in to the address since, or the same account again with a new refresh token, the failure
// was the refresh token the confirmation read, which no longer stands for the address, so it
// writes nothing and the request is simply not signed in. A write that fails ends nothing and is
// logged at WARN, as a Directory check's failed write is: the row keeps its sign-in and its lapsed
// confirmation, so the request is refused with the write's error and the next confirmation tries
// again. Each line names the person by their login's hash and a failed write by its kind, never
// the address or the store's error, which quotes it (model.LoginHash, errkind.Of); reason names
// no address either (Membership.refresh).
func endSubject(ctx context.Context, people auth.PeopleStore, email string, read auth.PersonMembership, reason string, mode auth.EndMode, ended string) (bool, error) {
	done, err := people.EndSubject(ctx, email, read.Subject, read.RefreshToken, mode)
	if err != nil {
		slog.Warn("dispatch: a failed confirmation could not end the sign-in, as its write failed; the request is refused and the next confirmation tries again",
			"login_hash", model.LoginHash(email), "reason", reason, "error_kind", errkind.Of(err))
		return false, err
	}
	if !done {
		slog.Info("dispatch: a failed confirmation ended nothing: the address was signed in to again since it read the sign-in", "login_hash", model.LoginHash(email), "reason", reason)
		return false, ErrNoIdentity
	}
	slog.Warn(ended, "login_hash", model.LoginHash(email), "reason", reason)
	return true, ErrNoIdentity
}

// SignedOutReason is SignedOutOutsideOff for a restricted member whose sign-in no longer stands
// (endedRestricted), or whose account is a passkey's (auth.PersonMembership.Issuer): DomainRule
// admits nobody from outside the domain, so only a deployment that once admitted restricted
// members (PasskeyRule, or an earlier build's Google outside rule) holds such a row. Its next
// hourly confirmation under this rule ends a Google account's sign-in, while a passkey's cookie is
// refused at its next use with nothing written (Membership.confirmPasskey), so its row still reads
// as standing and is told by its account instead. Everyone else is told no reason, so a deployment
// that never admitted one answers as it always has.
func (r DomainRule) SignedOutReason(ctx context.Context, people auth.PeopleStore, email string) (string, error) {
	membership, found, err := people.Membership(ctx, email)
	if err != nil || !found || membership.Kind != model.MemberRestricted {
		return "", err
	}
	if membership.Issuer == "" && membership.SignInStands() {
		return "", nil
	}
	return SignedOutOutsideOff, nil
}

// endedRestricted reads email's row (auth.PeopleStore.Membership) and reports whether it is a
// restricted member's whose sign-in no longer stands (auth.PersonMembership.SignInStands): the row
// holds no refresh token, or a passkey's row no confirmation, which the hourly confirmation's end
// and a sign-out both forget, or reads signed out, which a sign-out that kept the token for
// membership checks marks, and a sign-in by anyone since would have written again.
func endedRestricted(ctx context.Context, people auth.PeopleStore, email string) (bool, error) {
	membership, found, err := people.Membership(ctx, email)
	if err != nil {
		return false, err
	}
	return found && !membership.SignInStands() && membership.Kind == model.MemberRestricted, nil
}

// ConfirmCookie reads the address's row (auth.PeopleStore.Membership) and refuses with
// ErrSignInSuperseded when it no longer names subject: a sign-in by another Google account
// committed in between and has already taken the address.
func (r DomainRule) ConfirmCookie(ctx context.Context, people auth.PeopleStore, email, subject string) error {
	membership, _, err := people.Membership(ctx, email)
	if err != nil {
		return err
	}
	if membership.Subject != subject {
		return ErrSignInSuperseded
	}
	return nil
}

// WriteSignIn records a Google sign-in as the member kind Person named, with the `hd` its token
// carried (auth.PeopleStore.SignInSubject).
func (r DomainRule) WriteSignIn(ctx context.Context, people auth.PeopleStore, person SignedIn, subject, refreshToken string) error {
	return people.SignInSubject(ctx, person.Email, subject, refreshToken, person.HostedDomain, person.Kind)
}

// WriteConfirm records a refresh that confirmed a Google sign-in's membership, as the member kind
// Person named again with the `hd` the refreshed token carried, only while the row still holds the
// sign-in read names (auth.PeopleStore.ConfirmSubject).
func (r DomainRule) WriteConfirm(ctx context.Context, people auth.PeopleStore, read auth.PersonMembership, person SignedIn, subject, refreshToken string) error {
	return people.ConfirmSubject(ctx, person.Email, subject, read.RefreshToken, refreshToken, person.HostedDomain, person.Kind)
}

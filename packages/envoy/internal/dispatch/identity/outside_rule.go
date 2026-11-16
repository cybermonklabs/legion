package identity

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/oidc"
)

// Lister answers whether some project names email directly: as its owner, or on its access list as
// a person, never through a group. In production it is access.Listed over the database
// (cmd/dispatch builds it with ListerFunc).
type Lister interface {
	Listed(ctx context.Context, email string) (bool, error)
}

// ListerFunc is a Lister written as a function.
type ListerFunc func(ctx context.Context, email string) (bool, error)

// Listed calls f.
func (f ListerFunc) Listed(ctx context.Context, email string) (bool, error) {
	return f(ctx, email)
}

// OutsideRule signs in Domain's people as DomainRule does, and also a verified Google account
// outside the domain while some project names its email directly (People), as a restricted member.
// Every method but Person, End and SignedOutReason is Domain's: the sign-in records the Google
// account's subject, and the hourly confirmation runs Person again, so an account no list names
// any more is signed out there, and told so.
type OutsideRule struct {
	Domain DomainRule
	People Lister
}

var _ SignInRule = OutsideRule{}

// Person admits an account of Domain's Workspace as a full member without reading the lists, as
// DomainRule.Person does, and refuses it as DomainRule does for every reason but being outside the
// domain. An account outside the domain must have a subject and an email Google has verified
// (oidc.VerifiedLogin), and an address outside the sign-in domain and every subdomain of it: an
// address of the domain that is not of its Workspace (a personal Google account) is ErrNotInDomain,
// else it would read every project that lists the employee. Then it is a restricted member while
// People names it; ErrNotListed, naming the address, once no project does; and an error matching
// ErrMembershipUnreadable, which is no refusal, when the lists cannot be read. An empty Domain
// admits nobody from outside, as it admits nobody from inside.
func (r OutsideRule) Person(ctx context.Context, claims oidc.Claims) (SignedIn, error) {
	email, err := oidc.WorkspaceLogin(claims, r.Domain.Domain)
	switch {
	case err == nil:
		return SignedIn{Email: email, Kind: model.MemberFull}, nil
	case !errors.Is(err, oidc.ErrNotInDomain):
		return SignedIn{Email: email}, googleRefusal(email, claims, err)
	}
	email, err = oidc.VerifiedLogin(claims)
	if err != nil {
		return SignedIn{Email: email}, googleRefusal(email, claims, err)
	}
	if r.ofTheSignInDomain(email) {
		return SignedIn{Email: email}, fmt.Errorf("%s (hd %q): %w", email, claims.HostedDomain, ErrNotInDomain)
	}
	listed, err := r.People.Listed(ctx, email)
	if err != nil {
		return SignedIn{Email: email}, fmt.Errorf("%w: %w", ErrMembershipUnreadable, err)
	}
	if !listed {
		return SignedIn{Email: email}, fmt.Errorf("%s: %w", email, ErrNotListed)
	}
	return SignedIn{Email: email, Kind: model.MemberRestricted}, nil
}

// ofTheSignInDomain reports whether login, a lowercased address, is at the sign-in domain or a
// subdomain of it, or whether there is no sign-in domain at all.
func (r OutsideRule) ofTheSignInDomain(login string) bool {
	domain := strings.ToLower(r.Domain.Domain)
	at := login[strings.LastIndexByte(login, '@')+1:]
	return domain == "" || at == domain || strings.HasSuffix(at, "."+domain)
}

// RefusalLogFields names the email and hd Google's token carried (DomainRule.RefusalLogFields).
func (r OutsideRule) RefusalLogFields(claims oidc.Claims) []any {
	return r.Domain.RefusalLogFields(claims)
}

// Subject is the Google account a sign-in records with the person (DomainRule.Subject).
func (r OutsideRule) Subject(claims oidc.Claims) string {
	return r.Domain.Subject(claims)
}

// BootLog logs the issuer, client and Workspace domain boot is signing people in under, and that
// accounts outside the domain that a project names sign in too.
func (r OutsideRule) BootLog(issuer, clientID string) {
	slog.Info("dispatch: signing people in with Google for a Workspace domain, and the accounts outside it that a project names", "issuer", issuer, "client_id", clientID, "domain", r.Domain.Domain, "outside", true)
}

// DomainHint is false: an account outside the domain signs in too, and a chooser narrowed to the
// domain would hide it.
func (r OutsideRule) DomainHint() bool {
	return false
}

// CanRefresh is DomainRule.CanRefresh: only a row that records a Google account can be confirmed.
func (r OutsideRule) CanRefresh(stored string) bool {
	return r.Domain.CanRefresh(stored)
}

// ConfirmSubject is DomainRule.ConfirmSubject: the refreshed token must name the account that
// signed in.
func (r OutsideRule) ConfirmSubject(claims oidc.Claims, stored string) (string, error) {
	return r.Domain.ConfirmSubject(claims, stored)
}

// End is DomainRule.End, logged in words that fit both kinds of person this rule signs in: one
// from outside the domain was never admitted by a Workspace membership, so "Google did not confirm
// the person's membership of the domain" would misname why it ended. The reason names what the
// hourly confirmation found: most often, for such a person, that no project names them any more.
func (r OutsideRule) End(ctx context.Context, people auth.PeopleStore, sessions auth.SessionStore, email, subject, reason string) error {
	return endSubject(ctx, people, email, subject, reason, "dispatch: sign-in ended: the hourly confirmation no longer admits the person")
}

// SignedOutReason is SignedOutNotListed for a restricted member whose sign-in no longer stands
// (endedRestricted) while no project names them (People): the reason is read when the person asks,
// not kept from the request that ended the sign-in, so it holds whichever request ran the hourly
// confirmation. A person a list still names (one who signed out, or whose Google grant lapsed) is
// told none, as is a full member; one who signed out and whom no list has named since is told it
// too, at another browser still holding the session the sign-out ended (the browser that signed
// out holds no cookie). Lists that cannot be read are an error matching ErrMembershipUnreadable.
func (r OutsideRule) SignedOutReason(ctx context.Context, people auth.PeopleStore, email string) (string, error) {
	ended, err := endedRestricted(ctx, people, email)
	if err != nil || !ended {
		return "", err
	}
	listed, err := r.People.Listed(ctx, email)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrMembershipUnreadable, err)
	}
	if listed {
		return "", nil
	}
	return SignedOutNotListed, nil
}

// ConfirmCookie is DomainRule.ConfirmCookie.
func (r OutsideRule) ConfirmCookie(ctx context.Context, people auth.PeopleStore, email, subject string) error {
	return r.Domain.ConfirmCookie(ctx, people, email, subject)
}

// WriteSignIn is DomainRule.WriteSignIn: it records the member kind Person named.
func (r OutsideRule) WriteSignIn(ctx context.Context, people auth.PeopleStore, person SignedIn, subject, refreshToken string) error {
	return r.Domain.WriteSignIn(ctx, people, person, subject, refreshToken)
}

// WriteConfirm is DomainRule.WriteConfirm: it records the member kind Person named again.
func (r OutsideRule) WriteConfirm(ctx context.Context, people auth.PeopleStore, person SignedIn, subject, refreshToken string) error {
	return r.Domain.WriteConfirm(ctx, people, person, subject, refreshToken)
}

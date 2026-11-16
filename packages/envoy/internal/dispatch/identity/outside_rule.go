package identity

import (
	"context"
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
// It is the DomainRule it embeds but for Person, BootLog, DomainHint, OffersAccountChooser, End and
// SignedOutReason: the sign-in records the Google account's subject, and the hourly confirmation
// runs Person again, so an account no list names any more is signed out there, and told so.
type OutsideRule struct {
	DomainRule
	People Lister
}

var _ SignInRule = OutsideRule{}

// Person requires of every account a subject and an email Google has verified
// (oidc.VerifiedLogin), refusing one without them as DomainRule does, and admits an account of
// Domain's Workspace (hd) as a full member without reading the lists, as DomainRule.Person does.
// An account outside the domain must also have an address outside the sign-in domain and every
// subdomain of it: an address of the domain that is not of its Workspace (a personal Google
// account) is ErrNotInDomain, else it would read every project that lists the employee. Then it is
// a restricted member while People names it; ErrNotListed, naming the address, once no project
// does; and an error matching ErrMembershipUnreadable, which is no refusal, when the lists cannot
// be read. An empty Domain admits nobody from outside, as it admits nobody from inside.
func (r OutsideRule) Person(ctx context.Context, claims oidc.Claims) (SignedIn, error) {
	email, err := oidc.VerifiedLogin(claims)
	if err != nil {
		return SignedIn{Email: email}, googleRefusal(email, claims, err)
	}
	if r.Domain != "" && claims.HostedDomain == r.Domain {
		return SignedIn{Email: email, Kind: model.MemberFull}, nil
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
	domain := strings.ToLower(r.Domain)
	at := login[strings.LastIndexByte(login, '@')+1:]
	return domain == "" || at == domain || strings.HasSuffix(at, "."+domain)
}

// BootLog logs the issuer, client and Workspace domain boot is signing people in under, and that
// accounts outside the domain that a project names sign in too.
func (r OutsideRule) BootLog(issuer, clientID string) {
	slog.Info("dispatch: signing people in with Google for a Workspace domain, and the accounts outside it that a project names", "issuer", issuer, "client_id", clientID, "domain", r.Domain, "outside", true)
}

// DomainHint is false: an account outside the domain signs in too, and a chooser narrowed to the
// domain would hide it.
func (r OutsideRule) DomainHint() bool {
	return false
}

// OffersAccountChooser is true: the account a refusal names is most often one outside the domain
// that no list names, and a browser holding that one Google session would otherwise be sent
// straight back with it.
func (r OutsideRule) OffersAccountChooser() bool {
	return true
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

package identity

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"golang.org/x/net/idna"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
)

// ErrWorkspaceAddress means a restricted member's address is on a domain the Google Workspace owns
// (WorkspaceAddress): such an address signs in only as its Workspace account, else a person from
// outside holding it would read every project that lists the member.
var ErrWorkspaceAddress = errors.New("an address on a domain the Google Workspace owns signs in only as a Workspace account")

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

// OutsideRule signs in Domain's people with Google as DomainRule does, and no Google account
// outside the domain: its Person is DomainRule's, so such an account is ErrNotInDomain whatever a
// list names. Beside them it admits restricted members, people from outside the domain who sign in
// another way, while some project names their email directly (People) and their address is on no
// domain the Workspace owns (AdmitsRestricted). It is the DomainRule it embeds but for
// AdmitsRestricted, End and SignedOutReason, so a restricted member's credentials stop once no list
// names them, and the member is told why.
type OutsideRule struct {
	DomainRule
	People Lister
	// WorkspaceDomains are the other domains the Workspace owns (DISPATCH_SIGNIN_WORKSPACE_DOMAINS),
	// each a lowercase DNS name, an internationalized label written as its A-label; Domain is
	// always one of the Workspace's, listed here or not.
	WorkspaceDomains []string
}

var _ SignInRule = OutsideRule{}

// AdmitsRestricted answers nil while a restricted member at email may act, and otherwise why not:
// ErrNotInDomain under an empty Domain, which admits nobody from outside as it admits nobody from
// inside; ErrWorkspaceAddress for an address on a domain the Workspace owns, Domain or one of
// WorkspaceDomains, or a subdomain of one (WorkspaceAddress), without reading the lists, since such
// an address signs in only as its Workspace account, else a person from outside holding it would
// read every project that lists the member; ErrNotListed once no project names it (People); and an
// error matching ErrMembershipUnreadable, which is no refusal, when the lists cannot be read. Every
// credential acting for a restricted member asks it at each use, so it stops the moment the rule
// would no longer admit the member.
func (r OutsideRule) AdmitsRestricted(ctx context.Context, email string) error {
	if r.Domain == "" {
		return fmt.Errorf("%s: %w", email, ErrNotInDomain)
	}
	if WorkspaceAddress(email, r.Domain, r.WorkspaceDomains) {
		return fmt.Errorf("%s: %w", email, ErrWorkspaceAddress)
	}
	return admitsListed(ctx, r.People, email)
}

// admitsListed is the outside rule's list arm, which ListedAdmission asks too: nil while people
// says some project names email directly, ErrNotListed once none does, and an error matching
// ErrMembershipUnreadable when the lists cannot be read.
func admitsListed(ctx context.Context, people Lister, email string) error {
	listed, err := people.Listed(ctx, email)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrMembershipUnreadable, err)
	}
	if !listed {
		return fmt.Errorf("%s: %w", email, ErrNotListed)
	}
	return nil
}

// ListedAdmission is what a dev sign-in server (cmd/dispatch's DISPATCH_DEV_SIGNIN=1) holds a
// restricted member's credentials to: the outside rule's list arm alone (admitsListed), with no
// Workspace-domain arm, since a dev sign-in server names no Workspace and signs anyone in by email.
// Such a server has no sign-in rule, and without this a restricted member's token there would act
// for nobody, which no deployment that holds a restricted member does; with it the token acts
// while some project names the member directly, by the very check outside sign-in makes.
type ListedAdmission struct {
	People Lister
}

var _ RestrictedAdmission = ListedAdmission{}

// AdmitsRestricted is admitsListed over People.
func (a ListedAdmission) AdmitsRestricted(ctx context.Context, email string) error {
	return admitsListed(ctx, a.People, email)
}

// WorkspaceAddress reports whether login, an address with its ASCII letters lowered, is on a domain
// the Google Workspace owns, or on a subdomain of one: domain (DISPATCH_SIGNIN_DOMAIN) or any of
// otherDomains (DISPATCH_SIGNIN_WORKSPACE_DOMAINS), each a lowercase ASCII DNS name with an
// internationalized label written as its A-label (cmd/dispatch's checkSignInSettings). Such an
// address signs in only as its Workspace account, so no person from outside may hold it. The
// address's domain is compared without one trailing dot, which names the same domain, and, when it
// holds a character outside ASCII, also as IDNA lookup maps it (idna.Lookup.ToASCII), the A-label
// form a mail server resolves it in: so bob@bücher.example is on xn--bcher-kva.example, and so is
// that domain written with a capital Ü or the sign-in domain written in fullwidth letters. An ASCII
// domain is its own A-label form.
//
// The mapped form is trimmed of one trailing dot too: IDNA maps U+3002, U+FF0E and U+FF61 onto
// the full stop, so example.com followed by one of them maps to "example.com." and would
// otherwise be no domain the Workspace owns, which is the check this exists to make.
//
// A domain still holding an empty label after that trim — example.com.。, example.com。。, or a
// plain example.com.. — is answered as the Workspace's, which refuses it (ErrWorkspaceAddress)
// rather than leaving the lists to decide. The lookup profile sets no verifyDNSLength, so it maps
// such a domain through without complaint, and one trim cannot reach what a second dot hides. No
// mail server resolves a domain with an empty label, so this refuses nothing legitimate, and what
// it stops is a spelling of the Workspace's own domain that the check would otherwise not see.
func WorkspaceAddress(login, domain string, otherDomains []string) bool {
	at := strings.TrimSuffix(login[strings.LastIndexByte(login, '@')+1:], ".")
	if onAnyDomain(at, domain, otherDomains) || hasEmptyLabel(at) {
		return true
	}
	if !hasNonASCII(at) {
		return false
	}
	// The lookup maps every label it can even when it refuses another (one with -- as its third and
	// fourth characters, say), and answers that mapping beside its error, so the error is not read:
	// a domain it maps onto one the Workspace owns is refused whatever else the domain holds.
	ascii, _ := idna.Lookup.ToASCII(at)
	ascii = strings.TrimSuffix(ascii, ".")
	return onAnyDomain(ascii, domain, otherDomains) || hasEmptyLabel(ascii)
}

// onAnyDomain reports whether at, an address's domain, is domain, any of otherDomains, or a
// subdomain of one, compared as text (onDomain).
func onAnyDomain(at, domain string, otherDomains []string) bool {
	if onDomain(at, domain) {
		return true
	}
	for _, other := range otherDomains {
		if onDomain(at, other) {
			return true
		}
	}
	return false
}

// hasEmptyLabel reports whether at, a domain already trimmed of one trailing dot, is empty or
// holds a label with nothing in it: a leading dot, a trailing one a second dot left, or two dots
// together.
func hasEmptyLabel(at string) bool {
	return at == "" || strings.HasPrefix(at, ".") || strings.HasSuffix(at, ".") || strings.Contains(at, "..")
}

// hasNonASCII reports whether s holds a byte outside ASCII, so a character outside it.
func hasNonASCII(s string) bool {
	for i := range len(s) {
		if s[i] >= utf8.RuneSelf {
			return true
		}
	}
	return false
}

// onDomain reports whether at, an address's lowercased domain, is domain or a subdomain of it,
// compared without regard to case.
func onDomain(at, domain string) bool {
	tail := len(at) - len(domain)
	switch {
	case domain == "" || tail < 0:
		return false
	case tail == 0:
		return strings.EqualFold(at, domain)
	default:
		return at[tail-1] == '.' && strings.EqualFold(at[tail:], domain)
	}
}

// End is DomainRule.End, logged in this rule's own words. It ends a refused refresh of a full
// member's Google sign-in (Membership.end) under PasskeyRule, which embeds this rule and which
// cmd/dispatch's signInRule builds for DISPATCH_SIGNIN_PASSKEY=1: no Google sign-in admits a
// restricted member, and a passkey's sign-in is never refreshed (Membership.ConfirmFor), so a
// full member is the one person it reaches. The reason names what the hourly confirmation found.
func (r OutsideRule) End(ctx context.Context, people auth.PeopleStore, email string, read auth.PersonMembership, reason string, mode auth.EndMode) (bool, error) {
	return endSubject(ctx, people, email, read, reason, mode, "dispatch: sign-in ended: the hourly confirmation no longer admits the person")
}

// SignedOutReason is the reason a restricted member whose sign-in no longer stands
// (endedRestricted) is told, in the order AdmitsRestricted decides: SignedOutWorkspaceAddress while
// their address is on a domain the Workspace owns (WorkspaceAddress, ErrWorkspaceAddress), without
// reading the lists; else SignedOutNotListed while no project names them (People). The reason is
// read when the person asks, not kept from the request that ended the sign-in, so it holds
// whichever request ran the hourly confirmation. A person a list still names, at an address on no
// domain the Workspace owns (one who signed out, or whose sign-in lapsed), is told none, as is a
// full member; one who signed out and whom the rule would refuse now is told why too, at another
// browser still holding the session the sign-out ended (the browser that signed out holds no
// cookie). Lists that cannot be read are an error matching ErrMembershipUnreadable.
func (r OutsideRule) SignedOutReason(ctx context.Context, people auth.PeopleStore, email string) (string, error) {
	ended, err := endedRestricted(ctx, people, email)
	if err != nil || !ended {
		return "", err
	}
	if WorkspaceAddress(email, r.Domain, r.WorkspaceDomains) {
		return SignedOutWorkspaceAddress, nil
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

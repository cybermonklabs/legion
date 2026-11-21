package identity

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"unicode/utf8"

	"golang.org/x/net/idna"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/oidc"
)

// ErrWorkspaceAddress means a Google account outside the Workspace carries an address on a domain
// the Workspace owns (OutsideRule): such an address signs in only as its Workspace account, else a
// personal Google account holding it would read every project that lists the member.
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

// OutsideRule signs in Domain's people as DomainRule does, and also a verified Google account
// outside the domain while some project names its email directly (People), as a restricted member.
// It is the DomainRule it embeds but for Person, BootLog, DomainHint, OffersAccountChooser, End and
// SignedOutReason: the sign-in records the Google account's subject, and the hourly confirmation
// runs Person again, so an account no list names any more is signed out there, and told so.
type OutsideRule struct {
	DomainRule
	People Lister
	// WorkspaceDomains are the other domains the Workspace owns (DISPATCH_SIGNIN_WORKSPACE_DOMAINS),
	// each a lowercase DNS name, an internationalized label written as its A-label; Domain is
	// always one of the Workspace's, listed here or not.
	WorkspaceDomains []string
}

var _ SignInRule = OutsideRule{}

// Person requires of every account a subject and an email Google has verified
// (oidc.VerifiedLogin), refusing one without them as DomainRule does, and admits an account of
// Domain's Workspace (hd) as a full member without reading the lists, as DomainRule.Person does,
// whatever domain its address is on. An account outside the Workspace must also have an address
// on no domain the Workspace owns (ofTheWorkspace: Domain, each of WorkspaceDomains, and every
// subdomain of them): such an address that is not of the Workspace (a personal Google account) is
// ErrWorkspaceAddress, without reading the lists, else it would read every project that lists the
// member. Then it is a restricted member while People names it; ErrNotListed, naming the address,
// once no project does; and an error matching ErrMembershipUnreadable, which is no refusal, when
// the lists cannot be read. An empty Domain admits nobody from outside (ErrNotInDomain), as it
// admits nobody from inside. An address on a domain the Workspace owns that WorkspaceDomains does
// not name is guarded where the sign-in is recorded (WriteSignIn): when the address's row is a
// full member's under another account, the transaction that would take the address refuses it
// (auth.ErrAddressHeldByMember).
func (r OutsideRule) Person(ctx context.Context, claims oidc.Claims) (SignedIn, error) {
	email, err := oidc.VerifiedLogin(claims)
	if err != nil {
		return SignedIn{Email: email}, googleRefusal(email, claims, err)
	}
	if r.Domain != "" && claims.HostedDomain == r.Domain {
		return SignedIn{Email: email, Kind: model.MemberFull}, nil
	}
	if err := r.admitsOutsider(ctx, email, claims.HostedDomain); err != nil {
		return SignedIn{Email: email}, err
	}
	return SignedIn{Email: email, Kind: model.MemberRestricted}, nil
}

// AdmitsRestricted asks of email, a restricted member's address, the question Person asks of an
// account outside the Workspace (admitsOutsider), the one decision both make, so a credential
// acting for a restricted member stops the moment their sign-in would be refused: once no project
// names them (ErrNotListed), and once their address is on a domain the Workspace owns
// (ErrWorkspaceAddress), say after WorkspaceDomains came to list it.
func (r OutsideRule) AdmitsRestricted(ctx context.Context, email string) error {
	return r.admitsOutsider(ctx, email, "")
}

// admitsOutsider decides an account outside the Workspace (hosted, its own hd, not Domain) by its
// address email: ErrNotInDomain under an empty Domain, which admits nobody from outside;
// ErrWorkspaceAddress for an address on a domain the Workspace owns (ofTheWorkspace), without
// reading the lists; and then the lists' answer (admitsListed). Person and AdmitsRestricted both
// ask it.
func (r OutsideRule) admitsOutsider(ctx context.Context, email, hosted string) error {
	if r.Domain == "" {
		return fmt.Errorf("%s (hd %q): %w", email, hosted, ErrNotInDomain)
	}
	if r.ofTheWorkspace(email) {
		return fmt.Errorf("%s (hd %q): %w", email, hosted, ErrWorkspaceAddress)
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

// ofTheWorkspace reports whether login, an address with its ASCII letters lowered, is on a domain
// the Workspace owns or a subdomain of one: Domain, or any of WorkspaceDomains, each a lowercase
// ASCII name with an internationalized label written as its A-label (cmd/dispatch's
// checkSignInSettings). The address's domain is compared without one trailing dot, which names
// the same domain, and, when it holds a character outside ASCII, also as IDNA lookup maps it
// (idna.Lookup.ToASCII), the A-label form a mail server resolves it in: so bob@bücher.example is
// on xn--bcher-kva.example, and so is that domain written with a capital Ü or the sign-in domain
// written in fullwidth letters. An ASCII domain is its own A-label form.
//
// The mapped form is trimmed of one trailing dot too: IDNA maps U+3002, U+FF0E and U+FF61 onto
// the full stop, so example.com followed by one of them maps to "example.com." and would
// otherwise be no domain the Workspace owns, which is the check this exists to make.
//
// A domain still holding an empty label after that trim — example.com.。, example.com。。, or a
// plain example.com.. — is answered as the Workspace's, which refuses the sign-in
// (ErrWorkspaceAddress) rather than leaving the lists to decide. The lookup profile sets no
// verifyDNSLength, so it maps such a domain through without complaint, and one trim cannot reach
// what a second dot hides. No mail server resolves a domain with an empty label, so this refuses
// nothing legitimate, and what it stops is a spelling of the Workspace's own domain that the
// check would otherwise not see.
func (r OutsideRule) ofTheWorkspace(login string) bool {
	at := strings.TrimSuffix(login[strings.LastIndexByte(login, '@')+1:], ".")
	if r.onAnyDomain(at) || hasEmptyLabel(at) {
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
	return r.onAnyDomain(ascii) || hasEmptyLabel(ascii)
}

// hasEmptyLabel reports whether at, a domain already trimmed of one trailing dot, is empty or
// holds a label with nothing in it: a leading dot, a trailing one a second dot left, or two dots
// together.
func hasEmptyLabel(at string) bool {
	return at == "" || strings.HasPrefix(at, ".") || strings.HasSuffix(at, ".") || strings.Contains(at, "..")
}

// onAnyDomain reports whether at, an address's domain, is Domain, any of WorkspaceDomains, or a
// subdomain of one, compared as text (onDomain).
func (r OutsideRule) onAnyDomain(at string) bool {
	if onDomain(at, r.Domain) {
		return true
	}
	for _, domain := range r.WorkspaceDomains {
		if onDomain(at, domain) {
			return true
		}
	}
	return false
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

// BootLog logs the issuer, client and Workspace domain boot is signing people in under, the other
// domains the Workspace owns, and that accounts outside the domain that a project names sign in
// too.
func (r OutsideRule) BootLog(issuer, clientID string) {
	slog.Info("dispatch: signing people in with Google for a Workspace domain, and the accounts outside it that a project names", "issuer", issuer, "client_id", clientID, "domain", r.Domain, "workspace_domains", r.WorkspaceDomains, "outside", true)
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
func (r OutsideRule) End(ctx context.Context, people auth.PeopleStore, email string, read auth.PersonMembership, reason string) error {
	return endSubject(ctx, people, email, read, reason, "dispatch: sign-in ended: the hourly confirmation no longer admits the person")
}

// SignedOutReason is the reason a restricted member whose sign-in no longer stands
// (endedRestricted) is told, in the order Person decides: SignedOutWorkspaceAddress while their
// address is on a domain the Workspace owns (ofTheWorkspace, ErrWorkspaceAddress), without reading
// the lists; else SignedOutNotListed while no project names them (People). The reason is read when
// the person asks, not kept from the request that ended the sign-in, so it holds whichever request
// ran the hourly confirmation. A person a list still names, at an address on no domain the
// Workspace owns (one who signed out, or whose Google grant lapsed), is told none, as is a full
// member; one who signed out and whom the rule would refuse now is told why too, at another
// browser still holding the session the sign-out ended (the browser that signed out holds no
// cookie). Lists that cannot be read are an error matching ErrMembershipUnreadable.
func (r OutsideRule) SignedOutReason(ctx context.Context, people auth.PeopleStore, email string) (string, error) {
	ended, err := endedRestricted(ctx, people, email)
	if err != nil || !ended {
		return "", err
	}
	if r.ofTheWorkspace(email) {
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

package identity

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/oidc"
)

// stubLister is a Lister a test answers for, recording each email it was asked about.
type stubLister struct {
	mu     sync.Mutex
	listed map[string]bool
	err    error
	asked  []string
}

func (l *stubLister) Listed(_ context.Context, email string) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.asked = append(l.asked, email)
	return l.listed[email], l.err
}

func (l *stubLister) calls() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.asked...)
}

// outsideRule is the outside rule over workspace's domain, example.com, reading lister.
func outsideRule(lister Lister) OutsideRule {
	return OutsideRule{DomainRule: workspace, People: lister}
}

// outsideAccount is a verified Google ID token's claims for an account of no Workspace, whose
// email is outside the sign-in domain.
func outsideAccount() oidc.Claims {
	return oidc.Claims{Subject: "g-1", Email: "Outsider@Other.example", EmailVerified: true}
}

// Only a sign-in of the domain's people alone names the domain to Google as hd: the outside rule
// admits accounts of no Workspace, which a chooser narrowed to the domain would hide, and a
// Cognito group has no domain to name.
func TestOutsideSignInRuleAsksGoogleForNoDomainHint(t *testing.T) {
	for _, tc := range []struct {
		rule SignInRule
		want bool
	}{
		{outsideRule(&stubLister{}), false},
		{workspace, true},
		{GroupRule{Group: "people"}, false},
	} {
		if got := tc.rule.DomainHint(); got != tc.want {
			t.Errorf("%T.DomainHint() = %t, want %t", tc.rule, got, tc.want)
		}
	}
}

// Only the outside rule's refusal page offers Google's account chooser: a browser holding the one
// Google session just refused would otherwise be sent straight back with it. A refusal under the
// domain's people alone, or under a Cognito group, keeps its plain link.
func TestOutsideSignInRuleAloneOffersGooglesAccountChooser(t *testing.T) {
	for _, tc := range []struct {
		rule SignInRule
		want bool
	}{
		{outsideRule(&stubLister{}), true},
		{workspace, false},
		{GroupRule{Group: "people"}, false},
	} {
		if got := tc.rule.OffersAccountChooser(); got != tc.want {
			t.Errorf("%T.OffersAccountChooser() = %t, want %t", tc.rule, got, tc.want)
		}
	}
}

// Each rule tells a reason only for a restricted member whose sign-in no longer stands, and only
// for what it no longer admits: the outside rule once no list names them, the domain rule always,
// since it admits nobody from outside, and a group never. A full member, a restricted member signed
// in (another browser, or another account, holds the address's refresh token), and an address that
// never signed in are told none, and the lists are read only for a restricted sign-in that ended.
func TestOutsideSignInRuleSaysWhyARestrictedSessionEnded(t *testing.T) {
	ctx := context.Background()
	people := newTestPeopleStore(&testSessionStore{generations: map[string]int64{}})
	people.people["ended@other.example"] = testPerson{Subject: "g-1", Kind: model.MemberRestricted}
	people.people["listed@other.example"] = testPerson{Subject: "g-2", Kind: model.MemberRestricted}
	people.people["signed-in@other.example"] = testPerson{Subject: "g-3", RefreshToken: "refresh", ConfirmedAt: time.Now(), Kind: model.MemberRestricted}
	people.people["full@example.com"] = testPerson{Subject: "g-4", Kind: model.MemberFull}
	lister := &stubLister{listed: map[string]bool{"listed@other.example": true}}
	for _, tc := range []struct{ email, outside, domain string }{
		{"ended@other.example", SignedOutNotListed, SignedOutOutsideOff},
		{"listed@other.example", "", SignedOutOutsideOff},
		{"signed-in@other.example", "", ""},
		{"full@example.com", "", ""},
		{"never@other.example", "", ""},
	} {
		for _, rule := range []struct {
			rule SignInRule
			want string
		}{{outsideRule(lister), tc.outside}, {workspace, tc.domain}, {dispatchMembers, ""}} {
			if reason, err := rule.rule.SignedOutReason(ctx, people, tc.email); err != nil || reason != rule.want {
				t.Errorf("%T.SignedOutReason(%s) = %q, %v; want %q", rule.rule, tc.email, reason, err, rule.want)
			}
		}
	}
	if asked := lister.calls(); !slices.Equal(asked, []string{"ended@other.example", "listed@other.example"}) {
		t.Errorf("the lists were asked about %v, want only the two ended restricted sign-ins", asked)
	}
	failure := errors.New("connection refused")
	if reason, err := outsideRule(&stubLister{err: failure}).SignedOutReason(ctx, people, "ended@other.example"); reason != "" || !errors.Is(err, ErrMembershipUnreadable) || !errors.Is(err, failure) {
		t.Errorf("SignedOutReason while the lists do not answer = %q, %v; want no reason and ErrMembershipUnreadable carrying the failure", reason, err)
	}
}

// A restricted member whose address is now on a domain the Workspace owns (WorkspaceDomains, or a
// subdomain of one) is refused at the hourly confirmation (ErrWorkspaceAddress), and once that has
// ended their sign-in they are told so: WORKSPACE_ADDRESS, whether or not a list still names them,
// and without reading the lists, in the order Person decides. A restricted sign-in that still
// stands is told none, and an address on no such domain is told NOT_LISTED as before. The same
// rule listing no such domain answers as before, and a deployment with outside sign-in unset
// (DomainRule) answers as it always has: OUTSIDE_SIGNIN_OFF for every ended restricted sign-in.
func TestOutsideSignInRuleSaysARestrictedSessionEndedOnAWorkspaceDomain(t *testing.T) {
	ctx := context.Background()
	people := newTestPeopleStore(&testSessionStore{generations: map[string]int64{}})
	people.people["listed@second.example"] = testPerson{Subject: "g-1", Kind: model.MemberRestricted}
	people.people["unlisted@team.second.example"] = testPerson{Subject: "g-2", Kind: model.MemberRestricted}
	people.people["signed-in@second.example"] = testPerson{Subject: "g-3", RefreshToken: "refresh", ConfirmedAt: time.Now(), Kind: model.MemberRestricted}
	people.people["ended@other.example"] = testPerson{Subject: "g-4", Kind: model.MemberRestricted}
	listed := map[string]bool{"listed@second.example": true, "signed-in@second.example": true}
	owningLister, otherLister := &stubLister{listed: listed}, &stubLister{listed: listed}
	owning := OutsideRule{DomainRule: workspace, People: owningLister, WorkspaceDomains: []string{"second.example"}}
	for _, tc := range []struct{ email, owning, other, domain string }{
		{"listed@second.example", SignedOutWorkspaceAddress, "", SignedOutOutsideOff},
		{"unlisted@team.second.example", SignedOutWorkspaceAddress, SignedOutNotListed, SignedOutOutsideOff},
		{"signed-in@second.example", "", "", ""},
		{"ended@other.example", SignedOutNotListed, SignedOutNotListed, SignedOutOutsideOff},
	} {
		for _, rule := range []struct {
			name string
			rule SignInRule
			want string
		}{
			{"outside sign-in with second.example a Workspace domain", owning, tc.owning},
			{"outside sign-in with no other Workspace domain", outsideRule(otherLister), tc.other},
			{"outside sign-in unset", workspace, tc.domain},
		} {
			if reason, err := rule.rule.SignedOutReason(ctx, people, tc.email); err != nil || reason != rule.want {
				t.Errorf("%s: SignedOutReason(%s) = %q, %v; want %q", rule.name, tc.email, reason, err, rule.want)
			}
		}
	}
	if asked := owningLister.calls(); !slices.Equal(asked, []string{"ended@other.example"}) {
		t.Errorf("with second.example a Workspace domain the lists were asked about %v, want only ended@other.example", asked)
	}
}

// A verified account outside the domain that an access list names signs in as a restricted
// member, and the list is asked about its lowercased email.
func TestOutsideSignInRuleAdmitsAListedVerifiedAccountAsRestricted(t *testing.T) {
	lister := &stubLister{listed: map[string]bool{"outsider@other.example": true}}
	person, err := outsideRule(lister).Person(context.Background(), outsideAccount())
	if err != nil || person != (SignedIn{Email: "outsider@other.example", Kind: model.MemberRestricted}) {
		t.Fatalf("Person = %+v, %v; want outsider@other.example as a restricted member", person, err)
	}
	if asked := lister.calls(); len(asked) != 1 || asked[0] != "outsider@other.example" {
		t.Fatalf("the lister was asked %q, want only the lowercased outsider@other.example", asked)
	}
}

// A verified account of the domain's Workspace is a full member, and the lists are never read for
// it.
func TestOutsideSignInRuleKeepsTheDomainsPeopleFullWithoutReadingTheList(t *testing.T) {
	lister := &stubLister{}
	person, err := outsideRule(lister).Person(context.Background(), googleAccount())
	if err != nil || person != (SignedIn{Email: "a.person@example.com", Kind: model.MemberFull}) {
		t.Fatalf("Person = %+v, %v; want a.person@example.com as a full member", person, err)
	}
	if asked := lister.calls(); len(asked) != 0 {
		t.Fatalf("the lister was asked %q for a member of the domain, want no call", asked)
	}
}

// A verified account no list names is refused, and the refusal names the account: the negative
// control of c-outside-sign-in, since a rule admitting it fails here naming the account.
func TestOutsideSignInRuleRefusesAnUnlistedAccountByName(t *testing.T) {
	person, err := outsideRule(&stubLister{}).Person(context.Background(), outsideAccount())
	if !errors.Is(err, ErrNotListed) {
		t.Fatalf("Person of outsider@other.example, which no list names = %+v, %v; want ErrNotListed", person, err)
	}
	if !strings.Contains(err.Error(), "outsider@other.example") || person.Email != "outsider@other.example" || person.Kind != model.MemberNone {
		t.Fatalf("refusal = %+v, %q; want it to name outsider@other.example with no member kind", person, err)
	}
}

// An account whose email Google has not verified is refused before any list is read, and so is
// an account with no subject or no email.
func TestOutsideSignInRuleRefusesAnUnverifiedAccountBeforeReadingTheList(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*oidc.Claims)
		want   error
	}{
		{"an unverified email", func(c *oidc.Claims) { c.EmailVerified = false }, ErrEmailNotVerified},
		{"no email", func(c *oidc.Claims) { c.Email = "" }, ErrNoEmail},
		{"no subject", func(c *oidc.Claims) { c.Subject = "" }, ErrNoPerson},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lister := &stubLister{listed: map[string]bool{"outsider@other.example": true}}
			claims := outsideAccount()
			tc.change(&claims)
			if person, err := outsideRule(lister).Person(context.Background(), claims); !errors.Is(err, tc.want) || person.Kind != model.MemberNone {
				t.Fatalf("Person = %+v, %v; want %v and no member kind", person, err, tc.want)
			}
			if asked := lister.calls(); len(asked) != 0 {
				t.Fatalf("the lister was asked %q, want no call", asked)
			}
		})
	}
}

// An address of the sign-in domain, or of a subdomain of it, that is not of its Workspace (a
// personal Google account carrying the address) is refused as an address of the Workspace's,
// whatever the lists say and without reading them: else it would read every project that lists the
// employee. With no other domain listed (DISPATCH_SIGNIN_WORKSPACE_DOMAINS unset) the sign-in
// domain is the only one refused.
func TestOutsideSignInRuleRefusesAnAddressOfTheSignInDomainWithoutItsWorkspace(t *testing.T) {
	for _, email := range []string{"alice@example.com", "Alice@EU.Example.com"} {
		t.Run(email, func(t *testing.T) {
			refusedAsAWorkspaceAddress(t, outsideRule, oidc.Claims{Subject: "g-2", Email: email, EmailVerified: true})
		})
	}
	for _, email := range []string{"bob@notexample.com", "bob@second.example"} {
		t.Run(email+", off the sign-in domain", func(t *testing.T) {
			admittedAsRestricted(t, outsideRule, oidc.Claims{Subject: "g-3", Email: email, EmailVerified: true})
		})
	}
}

// The other domains the Workspace owns are its addresses too: an account outside the Workspace (no
// hd, or another Workspace's) at an address on one of them, or on a subdomain of one, is refused
// without reading the lists, however a list names the address, and so is one at the sign-in
// domain, which is the Workspace's whether listed or not. An address on any other domain, one that
// only ends in a listed domain's name included, signs in while a list names it.
func TestOutsideSignInRuleRefusesAnAddressOnAnyDomainTheWorkspaceOwns(t *testing.T) {
	rule := func(lister Lister) OutsideRule {
		return OutsideRule{DomainRule: workspace, People: lister, WorkspaceDomains: []string{"second.example", "third.example"}}
	}
	for _, tc := range []struct{ name, email, hd string }{
		{"a listed domain, with no hd", "alice@second.example", ""},
		{"the second listed domain", "carol@third.example", ""},
		{"a listed domain, under another Workspace's hd", "alice@second.example", "other.example"},
		{"a subdomain of a listed domain", "Alice@EU.Second.example", ""},
		{"the sign-in domain, which the list need not name", "alice@example.com", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			refusedAsAWorkspaceAddress(t, rule, oidc.Claims{Subject: "g-4", Email: tc.email, EmailVerified: true, HostedDomain: tc.hd})
		})
	}
	for _, email := range []string{"bob@fourth.example", "bob@notsecond.example"} {
		t.Run(email+", on no domain the Workspace owns", func(t *testing.T) {
			admittedAsRestricted(t, rule, oidc.Claims{Subject: "g-5", Email: email, EmailVerified: true})
		})
	}
	// A Workspace account (hd the sign-in domain) at an address on a listed domain is the member
	// itself: a full member, and the lists are never read.
	lister := &stubLister{}
	person, err := rule(lister).Person(context.Background(), oidc.Claims{Subject: "g-6", Email: "alice@second.example", EmailVerified: true, HostedDomain: "example.com"})
	if err != nil || person != (SignedIn{Email: "alice@second.example", Kind: model.MemberFull}) || len(lister.calls()) != 0 {
		t.Fatalf("Person of the Workspace account at alice@second.example = %+v, %v, the lists asked %q; want a full member, no list read", person, err, lister.calls())
	}
}

// refusedAsAWorkspaceAddress requires the rule built over a list naming the claims' address to
// refuse it with ErrWorkspaceAddress, naming the lowercased address and why, with no member kind,
// before reading the lists.
func refusedAsAWorkspaceAddress(t *testing.T, rule func(Lister) OutsideRule, claims oidc.Claims) {
	t.Helper()
	login := strings.ToLower(claims.Email)
	lister := &stubLister{listed: map[string]bool{login: true}}
	person, err := rule(lister).Person(context.Background(), claims)
	if !errors.Is(err, ErrWorkspaceAddress) || person.Kind != model.MemberNone || person.Email != login ||
		!strings.Contains(err.Error(), login) || !strings.Contains(err.Error(), "signs in only as a Workspace account") {
		t.Fatalf("Person of %s (hd %q) = %+v, %v; want ErrWorkspaceAddress naming %s and why", claims.Email, claims.HostedDomain, person, err, login)
	}
	if asked := lister.calls(); len(asked) != 0 {
		t.Fatalf("the lister was asked %q, want no call", asked)
	}
}

// admittedAsRestricted requires the rule built over a list naming the claims' address to admit it
// as a restricted member.
func admittedAsRestricted(t *testing.T, rule func(Lister) OutsideRule, claims oidc.Claims) {
	t.Helper()
	login := strings.ToLower(claims.Email)
	person, err := rule(&stubLister{listed: map[string]bool{login: true}}).Person(context.Background(), claims)
	if err != nil || person != (SignedIn{Email: login, Kind: model.MemberRestricted}) {
		t.Fatalf("Person of %s, which a list names = %+v, %v; want a restricted member: its domain is none the Workspace owns", claims.Email, person, err)
	}
}

// A list that cannot be read is no refusal: the error matches ErrMembershipUnreadable, so a caller
// keeps the sign-in it has rather than ending it on a database blip.
func TestOutsideSignInRuleWrapsAListFailureAsUnreadable(t *testing.T) {
	failure := errors.New("connection refused")
	person, err := outsideRule(&stubLister{err: failure}).Person(context.Background(), outsideAccount())
	if !errors.Is(err, ErrMembershipUnreadable) || !errors.Is(err, failure) || errors.Is(err, ErrNotListed) {
		t.Fatalf("Person while the lists cannot be read = %+v, %v; want ErrMembershipUnreadable carrying the failure", person, err)
	}
	if person.Kind != model.MemberNone {
		t.Fatalf("Person while the lists cannot be read named member kind %q, want none", person.Kind)
	}
}

// An address whose domain is written in Unicode is on a domain the Workspace owns exactly when its
// A-label form (IDNA lookup) is: an account outside the Workspace at bücher.example, at a subdomain
// of it (one separated by an ideographic full stop among them, and one beside a label the lookup
// refuses), or at the sign-in domain spelled in fullwidth letters, is refused as the same address
// in A-labels is, without reading the lists, and so is a credential acting for it; so is an address
// whose domain ends in a dot, which names the same domain, and one ending in a character IDNA
// maps onto the full stop (U+3002, U+FF0E, U+FF61), which the mapping turns into that same dot.
// A domain left with an empty label by a second such character, or written with one, is refused
// the same way, mapped or not: the mapping passes it through, no mail server resolves it, and
// what it would otherwise be is a spelling of the Workspace's own domain the check does not see.
// An address on another Unicode domain signs in while a list names it.
func TestOutsideSignInRuleRefusesAUnicodeSpellingOfADomainTheWorkspaceOwns(t *testing.T) {
	rule := func(lister Lister) OutsideRule {
		return OutsideRule{DomainRule: workspace, People: lister, WorkspaceDomains: []string{"xn--bcher-kva.example"}}
	}
	for _, email := range []string{
		"bob@xn--bcher-kva.example",
		"bob@bücher.example",
		"Bob@EU.bücher.example",
		"bob@eu\u3002bücher.example",
		"carol@ab--cd.bücher.example",
		"alice@\uff45\uff58\uff41\uff4d\uff50\uff4c\uff45.com",
		"alice@example.com.",
		"bob@bücher.example.",
		"alice@example.com\u3002",
		"alice@example.com\uff0e",
		"alice@example.com\uff61",
		"bob@bücher.example\u3002",
		"alice@example.com.\u3002",
		"alice@example.com\u3002\u3002",
		"alice@example.com..",
		"alice@.example.com",
		"bob@bücher.example\u3002\u3002",
	} {
		t.Run(email, func(t *testing.T) {
			refusedAsAWorkspaceAddress(t, rule, oidc.Claims{Subject: "g-7", Email: email, EmailVerified: true})
			if err := rule(&stubLister{listed: map[string]bool{strings.ToLower(email): true}}).AdmitsRestricted(context.Background(), strings.ToLower(email)); !errors.Is(err, ErrWorkspaceAddress) {
				t.Fatalf("AdmitsRestricted(%s) = %v, want ErrWorkspaceAddress", email, err)
			}
		})
	}
	if err := rule(&stubLister{}).AdmitsRestricted(context.Background(), "bob@b\u00dccher.example"); !errors.Is(err, ErrWorkspaceAddress) {
		t.Fatalf("AdmitsRestricted(bob@b\u00dccher.example), a capital Ü the login keeps = %v, want ErrWorkspaceAddress", err)
	}
	for _, email := range []string{"bob@bücherei.example", "bob@xn--bcherei-zxa.example", "bob@notbücher.example"} {
		t.Run(email+", on no domain the Workspace owns", func(t *testing.T) {
			admittedAsRestricted(t, rule, oidc.Claims{Subject: "g-8", Email: email, EmailVerified: true})
		})
	}
}

// A credential acting for a restricted member asks the rule the question that member's sign-in
// asked (AdmitsRestricted), by address alone: for each address and list, the rule admits it exactly
// when Person admits an account outside the Workspace (no hd) at that address as a restricted
// member, and refuses it with the same error. A rule that signs in nobody from outside (the
// domain's people alone, a Cognito group) admits none: ErrOutsideSignInOff.
func TestOutsideSignInRuleAdmitsARestrictedCredentialAsItAdmitsTheSignIn(t *testing.T) {
	ctx := context.Background()
	failure := errors.New("connection refused")
	rule := func(lister Lister) OutsideRule {
		return OutsideRule{DomainRule: workspace, People: lister, WorkspaceDomains: []string{"second.example"}}
	}
	for _, tc := range []struct {
		name, email string
		lister      *stubLister
	}{
		{"listed, outside every Workspace domain", "outsider@other.example", &stubLister{listed: map[string]bool{"outsider@other.example": true}}},
		{"named by no list", "outsider@other.example", &stubLister{}},
		{"listed, on a domain the Workspace owns", "bob@second.example", &stubLister{listed: map[string]bool{"bob@second.example": true}}},
		{"listed, on the sign-in domain", "alice@example.com", &stubLister{listed: map[string]bool{"alice@example.com": true}}},
		{"while the lists cannot be read", "outsider@other.example", &stubLister{err: failure}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			person, signInErr := rule(tc.lister).Person(ctx, oidc.Claims{Subject: "g-1", Email: tc.email, EmailVerified: true})
			credentialErr := rule(tc.lister).AdmitsRestricted(ctx, tc.email)
			if (signInErr == nil) != (credentialErr == nil) || (signInErr == nil && person.Kind != model.MemberRestricted) {
				t.Fatalf("%s: the sign-in answered %+v, %v and the credential %v; want both admitted as a restricted member or both refused", tc.email, person, signInErr, credentialErr)
			}
			for _, refusal := range []error{ErrNotInDomain, ErrWorkspaceAddress, ErrNotListed, ErrMembershipUnreadable} {
				if errors.Is(signInErr, refusal) != errors.Is(credentialErr, refusal) {
					t.Fatalf("%s: the sign-in answered %v and the credential %v; want the same refusal (%v)", tc.email, signInErr, credentialErr, refusal)
				}
			}
		})
	}
	for _, off := range []SignInRule{workspace, GroupRule{Group: "dispatch"}} {
		if err := off.AdmitsRestricted(ctx, "outsider@other.example"); !errors.Is(err, ErrOutsideSignInOff) {
			t.Errorf("%T admits a restricted member's credential: %v, want ErrOutsideSignInOff", off, err)
		}
	}
}

// The outside rule records a sign-in as the rule named it: a restricted member's sign-in and
// confirmation write restricted, a full member's full, each under the Google account's subject.
func TestOutsideSignInRuleWritesTheMemberKindItNamed(t *testing.T) {
	ctx := context.Background()
	people := newTestPeopleStore(&testSessionStore{generations: map[string]int64{}})
	rule := outsideRule(&stubLister{})
	outsider := SignedIn{Email: "outsider@other.example", Kind: model.MemberRestricted}
	if err := rule.WriteSignIn(ctx, people, outsider, "g-1", "refresh-1"); err != nil {
		t.Fatal(err)
	}
	if got, _ := people.get(outsider.Email); got.Kind != model.MemberRestricted || got.Subject != "g-1" {
		t.Fatalf("after the outsider's sign-in: %+v, want restricted under g-1", got)
	}
	member := SignedIn{Email: "a.person@example.com", Kind: model.MemberFull}
	if err := rule.WriteSignIn(ctx, people, member, "g-2", "refresh-2"); err != nil {
		t.Fatal(err)
	}
	read, _ := people.get(outsider.Email)
	if err := rule.WriteConfirm(ctx, people, auth.PersonMembership{Subject: read.Subject, RefreshToken: read.RefreshToken}, outsider, "g-1", "refresh-3"); err != nil {
		t.Fatal(err)
	}
	if got, _ := people.get(member.Email); got.Kind != model.MemberFull {
		t.Fatalf("after the member's sign-in: %+v, want full", got)
	}
	if got, _ := people.get(outsider.Email); got.Kind != model.MemberRestricted || got.RefreshToken != "refresh-3" {
		t.Fatalf("after the outsider's confirmation: %+v, want restricted with refresh-3", got)
	}
}

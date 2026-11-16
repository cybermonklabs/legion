package identity

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

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
	return OutsideRule{Domain: workspace, People: lister}
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
	for rule, want := range map[SignInRule]bool{
		outsideRule(&stubLister{}): false,
		workspace:                  true,
		GroupRule{Group: "people"}: false,
	} {
		if got := rule.DomainHint(); got != want {
			t.Errorf("%T.DomainHint() = %t, want %t", rule, got, want)
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
// personal Google account carrying the address) is refused as outside the domain, whatever the
// lists say and without reading them: else it would read every project that lists the employee.
func TestOutsideSignInRuleRefusesAnAddressOfTheSignInDomainWithoutItsWorkspace(t *testing.T) {
	for _, email := range []string{"alice@example.com", "Alice@EU.Example.com"} {
		t.Run(email, func(t *testing.T) {
			login := strings.ToLower(email)
			lister := &stubLister{listed: map[string]bool{login: true}}
			claims := oidc.Claims{Subject: "g-2", Email: email, EmailVerified: true}
			person, err := outsideRule(lister).Person(context.Background(), claims)
			if !errors.Is(err, ErrNotInDomain) || person.Kind != model.MemberNone || person.Email != login {
				t.Fatalf("Person of %s with no hd = %+v, %v; want ErrNotInDomain naming %s", email, person, err, login)
			}
			if asked := lister.calls(); len(asked) != 0 {
				t.Fatalf("the lister was asked %q, want no call", asked)
			}
		})
	}
	t.Run("an address that only ends in the domain's name", func(t *testing.T) {
		lister := &stubLister{listed: map[string]bool{"bob@notexample.com": true}}
		claims := oidc.Claims{Subject: "g-3", Email: "bob@notexample.com", EmailVerified: true}
		if person, err := outsideRule(lister).Person(context.Background(), claims); err != nil || person.Kind != model.MemberRestricted {
			t.Fatalf("Person of bob@notexample.com = %+v, %v; want a restricted member: its domain is not example.com's", person, err)
		}
	})
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
	if err := rule.WriteConfirm(ctx, people, outsider, "g-1", "refresh-3"); err != nil {
		t.Fatal(err)
	}
	if got, _ := people.get(member.Email); got.Kind != model.MemberFull {
		t.Fatalf("after the member's sign-in: %+v, want full", got)
	}
	if got, _ := people.get(outsider.Email); got.Kind != model.MemberRestricted || got.RefreshToken != "refresh-3" {
		t.Fatalf("after the outsider's confirmation: %+v, want restricted with refresh-3", got)
	}
}

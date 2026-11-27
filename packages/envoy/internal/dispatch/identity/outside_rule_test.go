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

// A restricted member whose address is on a domain the Workspace owns (the sign-in domain, one
// WorkspaceDomains lists, or a subdomain of either) is told so once their sign-in has ended:
// WORKSPACE_ADDRESS, whether or not a list still names them, and without reading the lists, in the
// order AdmitsRestricted decides. A restricted sign-in that still stands is told none, and an
// address on no such domain is told NOT_LISTED. The same rule listing no other domain tells an
// address on second.example NOT_LISTED or none, as its list says, and a deployment that admits no
// restricted member (DomainRule) answers OUTSIDE_SIGNIN_OFF for every ended restricted sign-in.
func TestOutsideSignInRuleSaysARestrictedSessionEndedOnAWorkspaceDomain(t *testing.T) {
	ctx := context.Background()
	people := newTestPeopleStore(&testSessionStore{generations: map[string]int64{}})
	people.people["listed@example.com"] = testPerson{Subject: "g-1", Kind: model.MemberRestricted}
	people.people["listed@second.example"] = testPerson{Subject: "g-2", Kind: model.MemberRestricted}
	people.people["unlisted@team.second.example"] = testPerson{Subject: "g-3", Kind: model.MemberRestricted}
	people.people["signed-in@second.example"] = testPerson{Subject: "g-4", RefreshToken: "refresh", ConfirmedAt: time.Now(), Kind: model.MemberRestricted}
	people.people["ended@other.example"] = testPerson{Subject: "g-5", Kind: model.MemberRestricted}
	listed := map[string]bool{"listed@example.com": true, "listed@second.example": true, "signed-in@second.example": true}
	owningLister, otherLister := &stubLister{listed: listed}, &stubLister{listed: listed}
	owning := OutsideRule{DomainRule: workspace, People: owningLister, WorkspaceDomains: []string{"second.example"}}
	for _, tc := range []struct{ email, owning, other, domain string }{
		{"listed@example.com", SignedOutWorkspaceAddress, SignedOutWorkspaceAddress, SignedOutOutsideOff},
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
			{"second.example a Workspace domain", owning, tc.owning},
			{"no other Workspace domain", outsideRule(otherLister), tc.other},
			{"no restricted member admitted", workspace, tc.domain},
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

// A verified account of the domain's Workspace is a full member, and the lists are never read for
// it.
func TestOutsideSignInRuleKeepsTheDomainsPeopleFullWithoutReadingTheList(t *testing.T) {
	lister := &stubLister{}
	person, err := outsideRule(lister).Person(context.Background(), googleAccount())
	if err != nil || person != (SignedIn{Email: "a.person@example.com", Kind: model.MemberFull, HostedDomain: "example.com"}) {
		t.Fatalf("Person = %+v, %v; want a.person@example.com as a full member of example.com", person, err)
	}
	if asked := lister.calls(); len(asked) != 0 {
		t.Fatalf("the lister was asked %q for a member of the domain, want no call", asked)
	}
}

// No Google sign-in admits an account outside the domain's Workspace, whatever a list names: the
// rule signs in with Google only an account whose hd is the domain, and refuses every other one
// ErrNotInDomain, naming its lowercased address, with no member kind and without reading the
// lists: an account of no Workspace, one of another Workspace, and a personal account at an address
// on the sign-in domain. The control: the same rule over the same list admits that listed address as
// a restricted member's credential (AdmitsRestricted), since a person from outside signs in another
// way.
func TestOutsideRuleSignsInNoGoogleAccountOutsideTheWorkspace(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct{ name, email, hd string }{
		{"an account of no Workspace", "Outsider@Other.example", ""},
		{"an account of another Workspace", "outsider@other.example", "other.example"},
		{"a personal account at an address on the sign-in domain", "alice@example.com", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			login := strings.ToLower(tc.email)
			lister := &stubLister{listed: map[string]bool{login: true}}
			person, err := outsideRule(lister).Person(ctx, oidc.Claims{Subject: "g-1", Email: tc.email, EmailVerified: true, HostedDomain: tc.hd})
			if !errors.Is(err, ErrNotInDomain) || person != (SignedIn{Email: login}) || !strings.Contains(err.Error(), login) {
				t.Fatalf("Person of %s (hd %q), which a list names = %+v, %v; want ErrNotInDomain naming %s, with no member kind", tc.email, tc.hd, person, err, login)
			}
			if asked := lister.calls(); len(asked) != 0 {
				t.Fatalf("the lister was asked %q for a Google sign-in, want no call", asked)
			}
		})
	}
	if err := outsideRule(&stubLister{listed: map[string]bool{"outsider@other.example": true}}).AdmitsRestricted(ctx, "outsider@other.example"); err != nil {
		t.Fatalf("AdmitsRestricted(outsider@other.example), which a list names = %v, want it admitted", err)
	}
}

// A restricted member a list names is admitted, and the list is asked about their address; one no
// list names is refused ErrNotListed, naming the address: the negative control of
// c-outside-sign-in, since a rule admitting it fails here naming the address.
func TestOutsideRuleAdmitsARestrictedMemberWhileAListNamesThem(t *testing.T) {
	ctx := context.Background()
	lister := &stubLister{listed: map[string]bool{"outsider@other.example": true}}
	if err := outsideRule(lister).AdmitsRestricted(ctx, "outsider@other.example"); err != nil {
		t.Fatalf("AdmitsRestricted(outsider@other.example), which a list names = %v, want it admitted", err)
	}
	if asked := lister.calls(); !slices.Equal(asked, []string{"outsider@other.example"}) {
		t.Fatalf("the lister was asked %q, want only outsider@other.example", asked)
	}
	err := outsideRule(&stubLister{}).AdmitsRestricted(ctx, "outsider@other.example")
	if !errors.Is(err, ErrNotListed) || !strings.Contains(err.Error(), "outsider@other.example") {
		t.Fatalf("AdmitsRestricted(outsider@other.example), which no list names = %v; want ErrNotListed naming it", err)
	}
}

// An address on a domain the Workspace owns, or on a subdomain of one, is refused as an address of
// the Workspace's, whatever the lists say and without reading them: else a person from outside
// holding it would read every project that lists the employee. The sign-in domain is the
// Workspace's whether WorkspaceDomains lists it or not, and each domain WorkspaceDomains lists is
// the Workspace's too. An address on any other domain, one that only ends in a listed domain's name
// included, is admitted while a list names it, and so is an address on second.example under the
// rule that lists no other domain.
func TestOutsideRuleRefusesARestrictedMemberAtAnAddressOnAnyDomainTheWorkspaceOwns(t *testing.T) {
	owning := OutsideRule{DomainRule: workspace, WorkspaceDomains: []string{"second.example", "third.example"}}
	for _, email := range []string{"alice@example.com", "alice@eu.example.com", "alice@second.example", "carol@third.example", "alice@eu.second.example"} {
		t.Run(email, func(t *testing.T) {
			refusedAsAWorkspaceAddress(t, owning, email)
		})
	}
	for _, rule := range []struct {
		name   string
		rule   OutsideRule
		emails []string
	}{
		{"second.example and third.example listed", owning, []string{"bob@notexample.com", "bob@fourth.example", "bob@notsecond.example"}},
		{"no other domain listed", OutsideRule{DomainRule: workspace}, []string{"bob@second.example"}},
	} {
		for _, email := range rule.emails {
			t.Run(email+", "+rule.name, func(t *testing.T) {
				admitted := rule.rule
				admitted.People = &stubLister{listed: map[string]bool{email: true}}
				if err := admitted.AdmitsRestricted(context.Background(), email); err != nil {
					t.Fatalf("AdmitsRestricted(%s), which a list names = %v; want it admitted: its domain is not the Workspace's", email, err)
				}
			})
		}
	}
}

// refusedAsAWorkspaceAddress requires rule over a list naming email to refuse it with
// ErrWorkspaceAddress, naming email and why, before reading the lists.
func refusedAsAWorkspaceAddress(t *testing.T, rule OutsideRule, email string) {
	t.Helper()
	lister := &stubLister{listed: map[string]bool{email: true}}
	rule.People = lister
	err := rule.AdmitsRestricted(context.Background(), email)
	if !errors.Is(err, ErrWorkspaceAddress) || !strings.Contains(err.Error(), email) || !strings.Contains(err.Error(), "signs in only as a Workspace account") {
		t.Fatalf("AdmitsRestricted(%s) = %v; want ErrWorkspaceAddress naming %s and why", email, err, email)
	}
	if asked := lister.calls(); len(asked) != 0 {
		t.Fatalf("the lister was asked %q, want no call", asked)
	}
}

// A list that cannot be read is no refusal: the error matches ErrMembershipUnreadable, so a caller
// keeps the sign-in it has rather than ending it on a database blip.
func TestOutsideRuleWrapsAListFailureAsUnreadable(t *testing.T) {
	failure := errors.New("connection refused")
	err := outsideRule(&stubLister{err: failure}).AdmitsRestricted(context.Background(), "outsider@other.example")
	if !errors.Is(err, ErrMembershipUnreadable) || !errors.Is(err, failure) || errors.Is(err, ErrNotListed) {
		t.Fatalf("AdmitsRestricted while the lists cannot be read = %v; want ErrMembershipUnreadable carrying the failure", err)
	}
}

// An address whose domain is written in Unicode is on a domain the Workspace owns exactly when its
// A-label form (IDNA lookup) is: an address at bücher.example, a domain WorkspaceDomains lists as
// its A-labels, at a subdomain of it (one separated by an ideographic full stop among them, and one
// beside a label the lookup refuses), or at the sign-in domain spelled in fullwidth letters, is the
// Workspace's as the same address in A-labels is; so is an address whose domain ends in a dot,
// which names the same domain, and one ending in a character IDNA maps onto the full stop (U+3002,
// U+FF0E, U+FF61), which the mapping turns into that same dot. A domain left with an empty label by
// a second such character, or written with one, is the Workspace's the same way, mapped or not: the
// mapping passes it through, no mail server resolves it, and what it would otherwise be is a
// spelling of the Workspace's own domain the check does not see. An address on another Unicode
// domain is not, and a restricted member's credential at each address is refused or not with it.
func TestWorkspaceAddressTakesEverySpellingOfTheDomain(t *testing.T) {
	others := []string{"xn--bcher-kva.example"}
	for _, tc := range []struct {
		login string
		want  bool
	}{
		{"bob@xn--bcher-kva.example", true},
		{"bob@bücher.example", true},
		{"bob@eu.bücher.example", true},
		{"bob@eu\u3002bücher.example", true},
		{"carol@ab--cd.bücher.example", true},
		{"bob@b\u00dccher.example", true},
		{"bob@bücher.example.", true},
		{"bob@bücher.example\u3002", true},
		{"bob@bücher.example\u3002\u3002", true},
		{"alice@\uff45\uff58\uff41\uff4d\uff50\uff4c\uff45.com", true},
		{"alice@example.com.", true},
		{"alice@example.com\u3002", true},
		{"alice@example.com\uff0e", true},
		{"alice@example.com\uff61", true},
		{"alice@example.com.\u3002", true},
		{"alice@example.com\u3002\u3002", true},
		{"alice@example.com..", true},
		{"alice@.example.com", true},
		{"bob@bücherei.example", false},
		{"bob@xn--bcherei-zxa.example", false},
		{"bob@notbücher.example", false},
		{"bob@notexample.com", false},
	} {
		if got := WorkspaceAddress(tc.login, "example.com", others); got != tc.want {
			t.Errorf("WorkspaceAddress(%q, example.com, %q) = %t, want %t", tc.login, others, got, tc.want)
		}
		rule := OutsideRule{DomainRule: workspace, People: &stubLister{listed: map[string]bool{tc.login: true}}, WorkspaceDomains: others}
		if err := rule.AdmitsRestricted(context.Background(), tc.login); errors.Is(err, ErrWorkspaceAddress) != tc.want || (!tc.want && err != nil) {
			t.Errorf("AdmitsRestricted(%q), which a list names = %v; want ErrWorkspaceAddress %t", tc.login, err, tc.want)
		}
	}
}

// A rule that admits nobody from outside (the domain's people alone, a Cognito group) admits no
// restricted member's credential: ErrOutsideSignInOff. The outside rule over an empty domain admits
// nobody from outside, as it admits nobody from inside: ErrNotInDomain, without reading the lists.
func TestOnlyTheOutsideRuleAdmitsARestrictedCredential(t *testing.T) {
	ctx := context.Background()
	for _, off := range []SignInRule{workspace, GroupRule{Group: "dispatch"}} {
		if err := off.AdmitsRestricted(ctx, "outsider@other.example"); !errors.Is(err, ErrOutsideSignInOff) {
			t.Errorf("%T admits a restricted member's credential: %v, want ErrOutsideSignInOff", off, err)
		}
	}
	lister := &stubLister{listed: map[string]bool{"outsider@other.example": true}}
	if err := (OutsideRule{People: lister}).AdmitsRestricted(ctx, "outsider@other.example"); !errors.Is(err, ErrNotInDomain) || len(lister.calls()) != 0 {
		t.Errorf("the outside rule over no domain: %v, the lists asked %q; want ErrNotInDomain and no call", err, lister.calls())
	}
}

// The outside rule records a sign-in as it is named: a restricted member's sign-in and
// confirmation write restricted, a full member's full, each under its subject.
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

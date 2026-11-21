package identity

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/admission"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

const (
	memberEmail    = "a.b+c@d.example"
	memberGroup    = "dispatch-members"
	memberRedirect = "https://dispatch.example/auth/callback"
)

func memberClaims(groups ...string) map[string]any {
	return map[string]any{
		"sub":              "person-subject",
		"cognito:username": "ExampleIdP_" + memberEmail,
		"cognito:groups":   groups,
		"email":            "not-the-person@d.example",
		"identities":       []map[string]any{{"providerName": "ExampleIdP"}},
	}
}

type membershipRig struct {
	issuer     *oidctest.Issuer
	people     *testPeopleStore
	sessions   *testSessionStore
	membership *Membership
	now        time.Time
}

// newMembershipRig signs memberEmail in through a fake pool at rig.now, the people store's clock,
// as the sign-in callback does, and returns the membership that renews it.
func newMembershipRig(t *testing.T) *membershipRig {
	t.Helper()
	return newMembershipRigFor(t, GroupRule{Group: memberGroup}, "", memberClaims(memberGroup), "")
}

// memberDomain is the Workspace domain of memberEmail, the domain rule's.
const memberDomain = "d.example"

// googleMemberClaims is a Google ID token's claims for memberEmail, a verified account of
// memberDomain.
func googleMemberClaims() map[string]any {
	return map[string]any{
		"sub":            "google-subject",
		"email":          "A.B+C@d.example",
		"email_verified": true,
		"hd":             memberDomain,
	}
}

// newDomainMembershipRig signs memberEmail in with Google for memberDomain at rig.now, the people
// store's clock, as the sign-in callback does, recording the account's subject, and returns the
// membership that renews it.
func newDomainMembershipRig(t *testing.T) *membershipRig {
	t.Helper()
	return newMembershipRigFor(t, DomainRule{Domain: memberDomain}, memberDomain, googleMemberClaims(), "google-subject")
}

func newMembershipRigFor(t *testing.T, rule SignInRule, hostedDomain string, claims map[string]any, subject string) *membershipRig {
	t.Helper()
	issuer := oidctest.New(t)
	issuer.EnableCodeFlow(issuer.PublishKey(t, "signing-key"), "dispatch-client", "dispatch-secret")
	flow := oidc.NewCodeFlow(issuer.URL(), "dispatch-client", "dispatch-secret", hostedDomain)
	issuer.SignInAs(claims)
	authURL, err := flow.AuthURL(context.Background(), memberRedirect, "state", "nonce")
	if err != nil {
		t.Fatalf("AuthURL: %v", err)
	}
	code, _ := issuer.Authorize(t, authURL)
	session, err := flow.Exchange(context.Background(), memberRedirect, code, "nonce")
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	sessions := &testSessionStore{generations: map[string]int64{memberEmail: 0}}
	rig := &membershipRig{
		issuer:   issuer,
		people:   newTestPeopleStore(sessions),
		sessions: sessions,
		now:      time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC),
	}
	// rig.now is the people store's clock, which stands in for the database's: it dates each
	// confirmation and measures its age, and Membership reads no clock of its own.
	rig.people.now = func() time.Time { return rig.now }
	if err := rule.WriteSignIn(context.Background(), rig.people, memberEmail, subject, session.RefreshToken); err != nil {
		t.Fatalf("record sign-in: %v", err)
	}
	rig.membership = &Membership{
		People:   rig.people,
		Sessions: rig.sessions,
		SignIn:   flow,
		Rule:     rule,
	}
	return rig
}

// Under the domain rule the hourly confirmation refreshes with Google and keeps the sessions of a
// verified account of the domain under the subject that signed in.
func TestDomainMembershipKeepsAMemberOfTheDomain(t *testing.T) {
	rig := newDomainMembershipRig(t)
	rig.now = rig.now.Add(time.Hour)
	if err := rig.membership.Confirm(context.Background(), memberEmail); err != nil {
		t.Fatalf("Confirm after an hour: %v", err)
	}
	if refreshes := rig.issuer.Refreshes(); refreshes != 1 {
		t.Fatalf("refreshes after an hour = %d, want 1", refreshes)
	}
	if membership, _ := rig.people.get(memberEmail); !membership.ConfirmedAt.Equal(rig.now) || membership.Subject != "google-subject" {
		t.Fatalf("membership after the refresh = %#v, want confirmed at %s under google-subject", membership, rig.now)
	}
	if generation := rig.sessions.generation(memberEmail); generation != 0 {
		t.Fatalf("a successful refresh moved the session generation to %d", generation)
	}
}

// The domain rule runs again on the refreshed token, and the refreshed account must be the one
// that signed in: an account that left the domain, lost its verified email, changed its email or
// is another account (another subject), or a refresh Google refuses, ends every session.
func TestDomainMembershipEndsTheSessionWhenTheRefreshedAccountNoLongerQualifies(t *testing.T) {
	for name, change := range map[string]func(claims map[string]any){
		"another domain":         func(claims map[string]any) { claims["hd"] = "other.example" },
		"no domain":              func(claims map[string]any) { delete(claims, "hd") },
		"an unverified email":    func(claims map[string]any) { claims["email_verified"] = "false" },
		"another email":          func(claims map[string]any) { claims["email"] = "someone-else@d.example" },
		"another Google account": func(claims map[string]any) { claims["sub"] = "another-google-subject" },
	} {
		t.Run(name, func(t *testing.T) {
			rig := newDomainMembershipRig(t)
			rig.issuer.UpdateGrants(change)
			rig.now = rig.now.Add(2 * time.Hour)
			if err := rig.membership.Confirm(context.Background(), memberEmail); !errors.Is(err, ErrNoIdentity) {
				t.Fatalf("Confirm: err = %v, want ErrNoIdentity", err)
			}
			assertSessionEnded(t, rig)
		})
	}
	t.Run("a refresh Google refuses", func(t *testing.T) {
		rig := newDomainMembershipRig(t)
		rig.issuer.RevokeGrants()
		rig.now = rig.now.Add(2 * time.Hour)
		if err := rig.membership.Confirm(context.Background(), memberEmail); !errors.Is(err, ErrNoIdentity) {
			t.Fatalf("Confirm: err = %v, want ErrNoIdentity", err)
		}
		assertSessionEnded(t, rig)
	})
}

// slowAnswer is how long the slow token endpoint below waits before each answer. One answer fits
// both Dispatch's refresh bound (refreshTimeout) and the bound oidc gives one request
// (oidc.TokenTimeout); two answers do not fit refreshTimeout. oidc.TokenTimeout less slowAnswer
// is the row's margin under load.
const slowAnswer = 7600 * time.Millisecond

// A refresh Google refuses from a slow token endpoint still ends the sign-in when one request fits
// the refresh's bound, in a process that restarted since the person signed in: its code flow is
// new, so the refused refresh is that flow's first grant, and only the style the discovery
// document names makes it one request. A client that guessed would ask by HTTP Basic and, refused,
// again in the form, as nothing has taught it the style. The endpoint waits slowAnswer before each
// answer, so the second answer could never come in time, and the sign-in of a person Google
// refused would be kept. The row fails first, naming the three durations, when they no longer let
// one answer fit both bounds and keep two from fitting refreshTimeout.
func TestDomainMembershipEndsTheSignInASlowTokenEndpointRefusesAfterARestart(t *testing.T) {
	if slowAnswer >= oidc.TokenTimeout || slowAnswer >= refreshTimeout || 2*slowAnswer <= refreshTimeout {
		t.Fatalf("slowAnswer is %s, refreshTimeout %s and oidc.TokenTimeout %s: this row needs one answer under both bounds and two answers over refreshTimeout", slowAnswer, refreshTimeout, oidc.TokenTimeout)
	}
	rig := newDomainMembershipRig(t)
	rig.membership.SignIn = oidc.NewCodeFlow(rig.issuer.URL(), "dispatch-client", "dispatch-secret", memberDomain)
	rig.issuer.RevokeGrants()
	rig.issuer.DelayToken(slowAnswer)
	rig.now = rig.now.Add(2 * time.Hour)
	before := len(rig.issuer.TokenRequests())
	if err := rig.membership.Confirm(context.Background(), memberEmail); !errors.Is(err, ErrNoIdentity) {
		t.Fatalf("Confirm: err = %v (token requests %q), want ErrNoIdentity", err, rig.issuer.TokenRequests()[before:])
	}
	if got := rig.issuer.TokenRequests()[before:]; !slices.Equal(got, []string{"client_secret_basic"}) {
		t.Fatalf("the refused refresh's token requests = %q, want one, by HTTP Basic", got)
	}
	assertSessionEnded(t, rig)
}

// A row whose refresh token no domain sign-in recorded holds no subject to compare: the domain rule
// ends its sessions at the confirmation without asking Google, rather than refreshing with a token
// whose account it cannot check.
func TestDomainMembershipEndsTheSessionOfARowWithNoSubject(t *testing.T) {
	rig := newDomainMembershipRig(t)
	membership, _ := rig.people.get(memberEmail)
	if err := rig.people.End(context.Background(), memberEmail); err != nil {
		t.Fatal(err)
	}
	rig.people.mu.Lock()
	rig.people.people[memberEmail] = testPerson{RefreshToken: membership.RefreshToken, ConfirmedAt: membership.ConfirmedAt}
	rig.people.mu.Unlock()
	rig.now = rig.now.Add(2 * time.Hour)
	refreshes := rig.issuer.Refreshes()
	if err := rig.membership.Confirm(context.Background(), memberEmail); !errors.Is(err, ErrNoIdentity) {
		t.Fatalf("Confirm of a row with no subject: err = %v, want ErrNoIdentity", err)
	}
	if got := rig.issuer.Refreshes(); got != refreshes {
		t.Fatalf("Confirm of a row with no subject refreshed at the issuer (%d refreshes, want %d)", got, refreshes)
	}
	assertSessionEnded(t, rig)
}

func TestMembershipConfirmedWithinTheHourAsksTheIssuerNothing(t *testing.T) {
	rig := newMembershipRig(t)
	rig.now = rig.now.Add(59 * time.Minute)
	if err := rig.membership.Confirm(context.Background(), memberEmail); err != nil {
		t.Fatalf("Confirm within the hour: %v", err)
	}
	if refreshes := rig.issuer.Refreshes(); refreshes != 0 {
		t.Fatalf("refreshes within the hour = %d, want 0", refreshes)
	}
}

// Membership is renewed at least hourly: once the last confirmation is an hour old, the next
// request refreshes with the stored refresh token and records the new confirmation.
func TestMembershipRefreshesOnceTheConfirmationIsAnHourOld(t *testing.T) {
	rig := newMembershipRig(t)
	rig.now = rig.now.Add(time.Hour)
	if err := rig.membership.Confirm(context.Background(), memberEmail); err != nil {
		t.Fatalf("Confirm after an hour: %v", err)
	}
	if refreshes := rig.issuer.Refreshes(); refreshes != 1 {
		t.Fatalf("refreshes after an hour = %d, want 1", refreshes)
	}
	membership, _ := rig.people.get(memberEmail)
	if !membership.ConfirmedAt.Equal(rig.now) || membership.RefreshToken == "" {
		t.Fatalf("membership after the refresh = %#v, want confirmed at %s with its refresh token", membership, rig.now)
	}
	if generation := rig.sessions.generation(memberEmail); generation != 0 {
		t.Fatalf("a successful refresh moved the session generation to %d", generation)
	}
	if err := rig.membership.Confirm(context.Background(), memberEmail); err != nil || rig.issuer.Refreshes() != 1 {
		t.Fatalf("Confirm right after the refresh: err=%v refreshes=%d, want nil and still 1", err, rig.issuer.Refreshes())
	}
}

func TestMembershipEndsTheSessionWhenTheRefreshFails(t *testing.T) {
	rig := newMembershipRig(t)
	rig.issuer.RevokeGrants()
	rig.now = rig.now.Add(2 * time.Hour)
	if err := rig.membership.Confirm(context.Background(), memberEmail); !errors.Is(err, ErrNoIdentity) {
		t.Fatalf("Confirm with a revoked refresh token: err = %v, want ErrNoIdentity", err)
	}
	assertSessionEnded(t, rig)
}

// A refresh the issuer cannot answer now (an outage, a 5xx, a 429, the client refused, a broken
// connection) ends nothing: Confirm refuses the request with ErrSignInUnavailable, the issuer's
// oidc.ErrIssuerUnavailable kept in its chain, which every caller answers 503, and leaves the
// session generation, the refresh token and the lapsed confirmation as they were, so the first
// request after the issuer answers again refreshes and is served. Only the issuer's invalid_grant
// ends the sign-in.
func TestMembershipKeepsTheSignInWhenTheIssuerCannotAnswerTheRefresh(t *testing.T) {
	for _, tc := range []struct {
		name string
		fail func(issuer *oidctest.Issuer)
	}{
		{"503", func(issuer *oidctest.Issuer) { issuer.FailToken(http.StatusServiceUnavailable, "") }},
		{"500", func(issuer *oidctest.Issuer) { issuer.FailToken(http.StatusInternalServerError, "") }},
		{"429", func(issuer *oidctest.Issuer) { issuer.FailToken(http.StatusTooManyRequests, "") }},
		{"401 invalid_client", func(issuer *oidctest.Issuer) { issuer.FailToken(http.StatusUnauthorized, "invalid_client") }},
		{"a broken connection", func(issuer *oidctest.Issuer) { issuer.DropToken() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newMembershipRig(t)
			signedIn, _ := rig.people.get(memberEmail)
			tc.fail(rig.issuer)
			rig.now = rig.now.Add(2 * time.Hour)
			for range 2 {
				if err := rig.membership.Confirm(context.Background(), memberEmail); !errors.Is(err, ErrSignInUnavailable) || !errors.Is(err, oidc.ErrIssuerUnavailable) || errors.Is(err, ErrNoIdentity) {
					t.Fatalf("Confirm while the issuer answers %s: err = %v, want ErrSignInUnavailable carrying oidc.ErrIssuerUnavailable and not ErrNoIdentity", tc.name, err)
				}
			}
			if generation := rig.sessions.generation(memberEmail); generation != 0 {
				t.Fatalf("session generation after the failed refresh = %d, want 0 (no cookie revoked)", generation)
			}
			if kept, _ := rig.people.get(memberEmail); kept.RefreshToken != signedIn.RefreshToken || !kept.ConfirmedAt.Equal(signedIn.ConfirmedAt) {
				t.Fatalf("membership after the failed refresh = %#v, want the sign-in's %#v unchanged", kept, signedIn)
			}
			login := CookieIdentity{SigningKey: testSigningKey, Sessions: rig.sessions, Membership: rig.membership}
			if _, err := login.Login(cookieRequest(t, memberEmail, 0)); !errors.Is(err, ErrSignInUnavailable) {
				t.Fatalf("Login while the issuer answers %s: err = %v, want ErrSignInUnavailable", tc.name, err)
			}

			rig.issuer.FailToken(0, "")
			if err := rig.membership.Confirm(context.Background(), memberEmail); err != nil {
				t.Fatalf("Confirm once the issuer answers again: %v", err)
			}
			if membership, _ := rig.people.get(memberEmail); !membership.ConfirmedAt.Equal(rig.now) {
				t.Fatalf("membership after the issuer answered = %#v, want confirmed at %s", membership, rig.now)
			}
			if generation := rig.sessions.generation(memberEmail); generation != 0 {
				t.Fatalf("session generation after the issuer answered = %d, want 0", generation)
			}
		})
	}
	t.Run("400 invalid_grant", func(t *testing.T) {
		rig := newMembershipRig(t)
		rig.issuer.FailToken(http.StatusBadRequest, "invalid_grant")
		rig.now = rig.now.Add(2 * time.Hour)
		if err := rig.membership.Confirm(context.Background(), memberEmail); !errors.Is(err, ErrNoIdentity) {
			t.Fatalf("Confirm when the issuer refuses the refresh token: err = %v, want ErrNoIdentity", err)
		}
		assertSessionEnded(t, rig)
	})
}

func TestMembershipEndsTheSessionWhenThePersonLeavesTheGroup(t *testing.T) {
	rig := newMembershipRig(t)
	rig.issuer.UpdateGrants(func(claims map[string]any) { claims["cognito:groups"] = []string{"other-group"} })
	rig.now = rig.now.Add(2 * time.Hour)
	if err := rig.membership.Confirm(context.Background(), memberEmail); !errors.Is(err, ErrNoIdentity) {
		t.Fatalf("Confirm after leaving the group: err = %v, want ErrNoIdentity", err)
	}
	assertSessionEnded(t, rig)
}

// A refreshed ID token naming another person is not this person's membership, whatever its groups.
func TestMembershipEndsTheSessionWhenTheRefreshNamesSomeoneElse(t *testing.T) {
	rig := newMembershipRig(t)
	rig.issuer.UpdateGrants(func(claims map[string]any) { claims["cognito:username"] = "ExampleIdP_other@d.example" })
	rig.now = rig.now.Add(2 * time.Hour)
	if err := rig.membership.Confirm(context.Background(), memberEmail); !errors.Is(err, ErrNoIdentity) {
		t.Fatalf("Confirm with a refresh naming someone else: err = %v, want ErrNoIdentity", err)
	}
	assertSessionEnded(t, rig)
}

// A person Dispatch knows but holds no refresh token for (recorded by the people migration, or
// whose session already ended) has no membership to confirm.
func TestMembershipWithoutARefreshTokenIsNoIdentity(t *testing.T) {
	rig := newMembershipRig(t)
	if err := rig.people.Record(context.Background(), "migrated@d.example"); err != nil {
		t.Fatalf("record: %v", err)
	}
	for _, email := range []string{"migrated@d.example", "never-seen@d.example"} {
		if err := rig.membership.Confirm(context.Background(), email); !errors.Is(err, ErrNoIdentity) {
			t.Errorf("Confirm %s: err = %v, want ErrNoIdentity", email, err)
		}
	}
	if refreshes := rig.issuer.Refreshes(); refreshes != 0 {
		t.Fatalf("refreshes with no refresh token = %d, want 0", refreshes)
	}
}

// The refresh belongs to the person, not to the request that noticed it was due: a browser that
// hangs up mid-refresh neither cancels it nor ends the session.
func TestMembershipRefreshOutlivesTheRequestThatStartedIt(t *testing.T) {
	rig := newMembershipRig(t)
	rig.now = rig.now.Add(2 * time.Hour)
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	if err := rig.membership.Confirm(ended, memberEmail); err != nil {
		t.Fatalf("Confirm on an ended request: %v", err)
	}
	if generation := rig.sessions.generation(memberEmail); generation != 0 {
		t.Fatalf("an ended request ended the session (generation %d)", generation)
	}
}

// A page load sends many requests at once; one refresh answers them all, so a pool that rotates
// refresh tokens is never handed the same one twice.
func TestMembershipRefreshesOnceForConcurrentRequests(t *testing.T) {
	rig := newMembershipRig(t)
	rig.now = rig.now.Add(2 * time.Hour)
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for range 16 {
		wg.Go(func() { errs <- rig.membership.Confirm(context.Background(), memberEmail) })
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Confirm: %v", err)
		}
	}
	if refreshes := rig.issuer.Refreshes(); refreshes != 1 {
		t.Fatalf("refreshes for 16 concurrent requests = %d, want 1", refreshes)
	}
}

// placedPeopleStore is a people store that records, at each read of a person's row and each
// confirmation it writes, how many places of outside are held.
type placedPeopleStore struct {
	auth.PeopleStore
	outside *admission.Outside
	mu      sync.Mutex
	held    []int
}

func (s *placedPeopleStore) record() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.held = append(s.held, s.outside.Stats().InUse)
}

func (s *placedPeopleStore) recorded() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int(nil), s.held...)
}

func (s *placedPeopleStore) Membership(ctx context.Context, email string) (auth.PersonMembership, bool, error) {
	s.record()
	return s.PeopleStore.Membership(ctx, email)
}

func (s *placedPeopleStore) Confirm(ctx context.Context, email, refreshToken string) error {
	s.record()
	return s.PeopleStore.Confirm(ctx, email, refreshToken)
}

// Work outside admission holds its place of the request pool's share for a sign-in's reads alone
// (Confirm): a lapsed confirmation's refresh holds the place of the work that leads it while it
// reads the person's row again and writes the renewed confirmation, and none while the issuer
// answers, and another work that finds the same confirmation lapsed meanwhile holds none while it
// waits on that refresh. Each read and write of the row is made under one place, and each Confirm
// returns with its place given back; the issuer is asked once.
func TestALapsedConfirmationsRefreshHoldsAPlaceForThePersonsRowAlone(t *testing.T) {
	rig := newMembershipRig(t)
	rig.now = rig.now.Add(2 * time.Hour)
	outside := admission.New(admission.DefaultLimits()).Outside()
	people := &placedPeopleStore{PeopleStore: rig.people, outside: outside}
	rig.membership.People = people
	issuer := rig.membership.SignIn
	entered, release := make(chan int, 1), make(chan struct{})
	rig.membership.SignIn = refresherFunc(func(ctx context.Context, refreshToken string) (oidc.Session, error) {
		entered <- outside.Stats().InUse
		<-release
		return issuer.Refresh(ctx, refreshToken)
	})
	confirm := func(user admission.OutsideUser) (*admission.Place, <-chan error) {
		t.Helper()
		place, err := outside.Hold(context.Background(), user)
		if err != nil {
			t.Fatalf("a place for a %s: %v", user, err)
		}
		confirmed := make(chan error, 1)
		go func() {
			confirmed <- rig.membership.Confirm(admission.WithPlace(context.Background(), place), memberEmail)
		}()
		return place, confirmed
	}

	leader, led := confirm(admission.HeartbeatCheck)
	if held := <-entered; held != 0 {
		t.Errorf("%d places held while the refresh waits on the issuer, want none", held)
	}
	follower, followed := confirm(admission.SocketAdmission)
	deadline := time.Now().Add(5 * time.Second)
	for len(people.recorded()) < 3 || outside.Stats().InUse != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("the second work's read and its place given back: %d reads made and %d places held, want 3 and none", len(people.recorded()), outside.Stats().InUse)
		}
		time.Sleep(time.Millisecond)
	}
	close(release)
	for _, confirmed := range []<-chan error{led, followed} {
		if err := <-confirmed; err != nil {
			t.Fatalf("Confirm once the issuer renewed the confirmation: %v", err)
		}
	}
	if held := outside.Stats().InUse; held != 0 {
		t.Errorf("%d places held once both Confirms returned, want none: each gave its place back", held)
	}
	leader.Release()
	follower.Release()
	if held := outside.Stats().InUse; held != 0 {
		t.Errorf("%d places held once the works released what they held, want none", held)
	}
	for i, held := range people.recorded() {
		if held != 1 {
			t.Errorf("read or write %d of the person's row was made with %d places held, want 1: the work's own", i+1, held)
		}
	}
	if confirmation, _ := rig.people.get(memberEmail); confirmation.ConfirmedAt != rig.now {
		t.Errorf("the confirmation after the refresh is dated %s, want %s: the renewal written", confirmation.ConfirmedAt, rig.now)
	}
	if refreshes := rig.issuer.Refreshes(); refreshes != 1 {
		t.Errorf("refreshes for two works that found one confirmation lapsed = %d, want 1", refreshes)
	}
}

func assertSessionEnded(t *testing.T, rig *membershipRig) {
	t.Helper()
	if generation := rig.sessions.generation(memberEmail); generation != 1 {
		t.Fatalf("session generation after the membership ended = %d, want 1 (every cookie revoked)", generation)
	}
	membership, found := rig.people.get(memberEmail)
	if !found || membership.RefreshToken != "" || !membership.ConfirmedAt.IsZero() {
		t.Fatalf("person after the membership ended = %#v (found %t), want kept with no refresh token or confirmation", membership, found)
	}
	refreshes := rig.issuer.Refreshes()
	if err := rig.membership.Confirm(context.Background(), memberEmail); !errors.Is(err, ErrNoIdentity) {
		t.Fatalf("Confirm after the membership ended: err = %v, want ErrNoIdentity", err)
	}
	if rig.issuer.Refreshes() != refreshes {
		t.Fatal("an ended membership asked the issuer again")
	}
}

// refresherFunc is a Refresher a test writes inline.
type refresherFunc func(ctx context.Context, refreshToken string) (oidc.Session, error)

func (f refresherFunc) Refresh(ctx context.Context, refreshToken string) (oidc.Session, error) {
	return f(ctx, refreshToken)
}

// The old account's refresh is in flight when the address's new account signs in and is given its
// cookie, and then the refresh fails: the failure belongs to the old account, so it ends nothing of
// the new one's. The new account's cookie still signs in, and its row keeps its refresh token.
func TestDomainMembershipWhoseRefreshFailsAfterAnotherAccountSignedInLeavesThatAccount(t *testing.T) {
	rig := newDomainMembershipRig(t)
	rig.now = rig.now.Add(2 * time.Hour)
	var newGeneration int64
	rig.membership.SignIn = refresherFunc(func(ctx context.Context, _ string) (oidc.Session, error) {
		if err := rig.people.SignInSubject(ctx, memberEmail, "google-subject-B", "refresh-B"); err != nil {
			t.Errorf("the new account's sign-in: %v", err)
		}
		generation, err := rig.sessions.EnsureSession(ctx, memberEmail)
		if err != nil {
			t.Errorf("the new account's session: %v", err)
		}
		newGeneration = generation
		return oidc.Session{}, fmt.Errorf(`oidc: refresh: %w: oauth2: "invalid_grant"`, oidc.ErrGrantRefused)
	})
	if err := rig.membership.Confirm(context.Background(), memberEmail); !errors.Is(err, ErrNoIdentity) {
		t.Fatalf("the old account's failed confirmation: err = %v, want ErrNoIdentity", err)
	}
	login := CookieIdentity{SigningKey: testSigningKey, Sessions: rig.sessions, Membership: rig.membership}
	if email, err := login.Login(cookieRequest(t, memberEmail, newGeneration)); err != nil || email != memberEmail {
		t.Fatalf("the new account's cookie: login %q, %v; want %s still signed in", email, err, memberEmail)
	}
	if membership, _ := rig.people.get(memberEmail); membership.Subject != "google-subject-B" || membership.RefreshToken != "refresh-B" {
		t.Fatalf("the row after the old account's failure: %#v, want google-subject-B with refresh-B", membership)
	}
}

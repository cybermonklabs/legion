package identity

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// The people of the hold tests: ada, a full member of example.com; and erin, a full member of the
// example.com Workspace whose address is on a domain the Workspace does not own.
const (
	adaEmail    = "ada@example.com"
	erinEmail   = "erin@other.example"
	holdDomain  = "example.com"
	holdSubject = "g-ada"
)

// adaClaims is a Google ID token's claims for ada, a verified account of example.com.
func adaClaims() map[string]any {
	return map[string]any{"sub": holdSubject, "email": adaEmail, "email_verified": true, "hd": holdDomain}
}

// erinClaims is a Google ID token's claims for erin, a verified account of the example.com
// Workspace at an address on other.example.
func erinClaims() map[string]any {
	return map[string]any{"sub": "g-erin", "email": erinEmail, "email_verified": true, "hd": holdDomain}
}

// holdRig is the real people store on a database of its own, a fake Google whose code flow signs
// people in and refreshes them, and a Membership under a rule, as one Dispatch process holds it.
type holdRig struct {
	t          *testing.T
	database   *store.Store
	people     *store.PgPeopleStore
	sessions   *store.PgSessionStore
	issuer     *oidctest.Issuer
	flow       *oidc.CodeFlow
	membership *Membership
}

func newHoldRig(t *testing.T, rule SignInRule) *holdRig {
	t.Helper()
	database := storetest.Open(t)
	issuer := oidctest.New(t)
	issuer.EnableCodeFlow(issuer.PublishKey(t, "signing-key"), "dispatch-client", "dispatch-secret", "https://dispatch.example/auth/callback")
	// A domain-mode flow reads Google's Workspace claims (email, hd); the sign-in pool's reads none.
	hostedDomain := ""
	if rule.Holds() {
		hostedDomain = holdDomain
	}
	flow := oidc.NewCodeFlow(issuer.URL(), "dispatch-client", "dispatch-secret", hostedDomain)
	people := store.NewPgPeopleStore(database.Pool, testSigningKey, nil)
	return &holdRig{
		t:          t,
		database:   database,
		people:     people,
		sessions:   store.NewPgSessionStore(database.Pool),
		issuer:     issuer,
		flow:       flow,
		membership: &Membership{People: people, SignIn: flow, Rule: rule},
	}
}

// signIn signs claims in through the fake Google as the sign-in callback does: the rule names the
// person, and WriteSignIn records them with the account and the refresh token, and a session.
func (r *holdRig) signIn(claims map[string]any) string {
	r.t.Helper()
	ctx := context.Background()
	session := r.exchange(claims)
	rule := r.membership.Rule
	person, err := rule.Person(ctx, session.Claims)
	if err != nil {
		r.t.Fatalf("the rule refused the sign-in: %v", err)
	}
	if err := rule.WriteSignIn(ctx, r.people, person, rule.Subject(session.Claims), session.RefreshToken); err != nil {
		r.t.Fatalf("record the sign-in: %v", err)
	}
	if _, err := r.sessions.EnsureSession(ctx, person.Email); err != nil {
		r.t.Fatal(err)
	}
	return person.Email
}

// exchange runs claims through the fake Google's code flow and answers the session it issued.
func (r *holdRig) exchange(claims map[string]any) oidc.Session {
	r.t.Helper()
	ctx := context.Background()
	r.issuer.SignInAs(claims)
	authURL, err := r.flow.AuthURL(ctx, memberRedirect, "state", "nonce")
	if err != nil {
		r.t.Fatalf("AuthURL: %v", err)
	}
	code, _ := r.issuer.Authorize(r.t, authURL)
	session, err := r.flow.Exchange(ctx, memberRedirect, code, "nonce")
	if err != nil {
		r.t.Fatalf("Exchange: %v", err)
	}
	return session
}

// set makes l answer listed for email from now on.
func (l *stubLister) set(email string, listed bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.listed = map[string]bool{email: listed}
}

// confirmedAgo dates email's confirmation age back, on the database's clock.
func (r *holdRig) confirmedAgo(email string, age time.Duration) {
	r.t.Helper()
	if _, err := r.database.Pool.Exec(context.Background(), `update people set confirmed_at = now() - make_interval(secs => $2) where email = $1`, email, age.Seconds()); err != nil {
		r.t.Fatalf("date the confirmation of %s %s back: %v", email, age, err)
	}
}

// newProcess is another Dispatch process over the same database and Google: a Membership with no
// refresh in flight and no pause.
func (r *holdRig) newProcess() {
	r.membership = &Membership{People: r.people, SignIn: r.membership.SignIn, Rule: r.membership.Rule}
}

// peopleRow is what the people table holds of a hold for one row.
type peopleRow struct {
	RefreshToken *string
	ConfirmedAt  *time.Time
	HeldSince    *time.Time
	SignedOutAt  *time.Time
	Kind         string
}

func (r *holdRig) row(email string) peopleRow {
	r.t.Helper()
	var row peopleRow
	if err := r.database.Pool.QueryRow(context.Background(), `
		select refresh_token, confirmed_at, held_since, signed_out_at, member_kind from people where email = $1
	`, email).Scan(&row.RefreshToken, &row.ConfirmedAt, &row.HeldSince, &row.SignedOutAt, &row.Kind); err != nil {
		r.t.Fatalf("read the people row of %s: %v", email, err)
	}
	return row
}

// databaseNow is the database's clock_timestamp().
func (r *holdRig) databaseNow() time.Time {
	r.t.Helper()
	var now time.Time
	if err := r.database.Pool.QueryRow(context.Background(), `select clock_timestamp()`).Scan(&now); err != nil {
		r.t.Fatal(err)
	}
	return now
}

// generation is email's session generation.
func (r *holdRig) generation(email string) int64 {
	r.t.Helper()
	generation, _, err := r.sessions.CurrentSessionGeneration(context.Background(), email)
	if err != nil {
		r.t.Fatal(err)
	}
	return generation
}

// countingRefresher counts the refreshes it is asked for, waits delay before each, and then
// answers as next does.
type countingRefresher struct {
	next    Refresher
	delay   time.Duration
	calls   atomic.Int32
	started chan time.Time
}

func (c *countingRefresher) Refresh(ctx context.Context, token string) (oidc.Session, error) {
	c.calls.Add(1)
	if c.started != nil {
		select {
		case c.started <- time.Now():
		default:
		}
	}
	time.Sleep(c.delay)
	return c.next.Refresh(ctx, token)
}

// waitForFlight waits until no refresh of email runs in m, failing after within.
func waitForFlight(t *testing.T, m *Membership, email string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		if _, running := m.started.Load(email); !running {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the refresh of %s still runs after %s", email, within)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestHoldTriggerRefusals (c-trigger): each refusal of a refresh, against the real people store.
// These hold the person: invalid_grant; a refreshed ID token naming another Google account; one
// naming another address; one with no subject, no email, or an email Google has not verified; and
// one outside the Workspace domain, under OutsideRule too, whatever a project's list names: Google
// signs in no account outside the Workspace (OutsideRule's Person is DomainRule's), so a former
// member is held, never confirmed as a restricted member. These hold nobody: a row with no
// recorded Google account, and any refusal under the sign-in pool's rule. Every end forgets the
// token, leaves the row not signed out and a full member's, and advances the session generation,
// and no row's refresh asks the lists.
//
// Controls: make endMode answer auth.EndForget under a Google rule: the test fails naming every
// row that holds. Give OutsideRule a Person that asks its lister once before DomainRule's: it
// fails naming the former member's row, on the lists' question.
func TestHoldTriggerRefusals(t *testing.T) {
	outside := func(lister *stubLister) SignInRule {
		return OutsideRule{DomainRule: DomainRule{Domain: holdDomain}, People: lister}
	}
	domain := func(*stubLister) SignInRule { return DomainRule{Domain: holdDomain} }
	group := func(*stubLister) SignInRule { return GroupRule{Group: memberGroup} }
	for _, tc := range []struct {
		name string
		rule func(*stubLister) SignInRule
		// signIn signs the person in and answers their address; change is what changes before
		// the refresh.
		signIn func(r *holdRig, lister *stubLister) string
		change func(r *holdRig, lister *stubLister)
		// The refresh's outcome, and whether the row then records a hold.
		outcome RefreshOutcome
		held    bool
	}{
		{name: "invalid_grant", rule: domain, signIn: signInAda,
			change:  func(r *holdRig, _ *stubLister) { r.issuer.RevokeGrants() },
			outcome: RefreshHeld, held: true},
		{name: "a refreshed token naming another Google account", rule: domain, signIn: signInAda,
			change:  updateClaims(func(c map[string]any) { c["sub"] = "g-someone-else" }),
			outcome: RefreshHeld, held: true},
		{name: "a refreshed token naming another address", rule: domain, signIn: signInAda,
			change:  updateClaims(func(c map[string]any) { c["email"] = "someone@example.com" }),
			outcome: RefreshHeld, held: true},
		{name: "a refreshed token with no subject", rule: domain, signIn: signInAda,
			change:  updateClaims(func(c map[string]any) { delete(c, "sub") }),
			outcome: RefreshHeld, held: true},
		{name: "a refreshed token with no email", rule: domain, signIn: signInAda,
			change:  updateClaims(func(c map[string]any) { delete(c, "email") }),
			outcome: RefreshHeld, held: true},
		{name: "a refreshed token whose email Google has not verified", rule: domain, signIn: signInAda,
			change:  updateClaims(func(c map[string]any) { c["email_verified"] = false }),
			outcome: RefreshHeld, held: true},
		{name: "a full member's refreshed token outside the Workspace domain", rule: domain, signIn: signInAda,
			change:  updateClaims(func(c map[string]any) { c["hd"] = "elsewhere.example" }),
			outcome: RefreshHeld, held: true},
		{name: "a former member whom a project's list names, under OutsideRule", rule: outside, signIn: signInErin,
			change: func(r *holdRig, lister *stubLister) {
				lister.set(erinEmail, true)
				updateClaims(func(c map[string]any) { c["hd"] = "elsewhere.example" })(r, lister)
			},
			outcome: RefreshHeld, held: true},
		{name: "a row with no recorded Google account", rule: domain,
			signIn: func(r *holdRig, _ *stubLister) string {
				if err := r.people.SignIn(context.Background(), adaEmail, "refresh-no-account"); err != nil {
					r.t.Fatal(err)
				}
				return adaEmail
			},
			outcome: RefreshForgot},
		{name: "the sign-in pool's invalid_grant", rule: group, signIn: signInPoolMember,
			change:  func(r *holdRig, _ *stubLister) { r.issuer.RevokeGrants() },
			outcome: RefreshForgot},
		{name: "the sign-in pool's refusal of a person outside the group", rule: group, signIn: signInPoolMember,
			change:  updateClaims(func(c map[string]any) { c["cognito:groups"] = []string{"other-group"} }),
			outcome: RefreshForgot},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			lister := &stubLister{}
			r := newHoldRig(t, tc.rule(lister))
			email := tc.signIn(r, lister)
			if tc.change != nil {
				tc.change(r, lister)
			}
			r.confirmedAgo(email, 2*time.Hour)
			generation := r.generation(email)
			outcome, err := r.membership.ConfirmFor(ctx, email)
			if outcome != tc.outcome || !errors.Is(err, ErrNoIdentity) {
				t.Fatalf("%s: ConfirmFor = %d, %v; want %d, %v", tc.name, outcome, err, tc.outcome, ErrNoIdentity)
			}
			if asked := lister.calls(); len(asked) != 0 {
				t.Errorf("%s: the refresh asked the lists about %q; want no question, Google's refresh reads no list", tc.name, asked)
			}
			row := r.row(email)
			if (row.HeldSince != nil) != tc.held {
				t.Errorf("%s: held_since = %v, want held %t", tc.name, row.HeldSince, tc.held)
			}
			if row.RefreshToken != nil {
				t.Errorf("%s: a refresh token is stored, want it forgotten", tc.name)
			}
			if row.SignedOutAt != nil {
				t.Errorf("%s: signed_out_at = %v, want the row not signed out", tc.name, row.SignedOutAt)
			}
			if row.Kind != string(model.MemberFull) {
				t.Errorf("%s: member_kind = %s, want %s", tc.name, row.Kind, model.MemberFull)
			}
			if r.generation(email) != generation+1 {
				t.Errorf("%s: the end left the session generation at %d, want %d", tc.name, r.generation(email), generation+1)
			}
			if standing, err := r.membership.Standing(ctx, email); err != nil || standing.Held != tc.held || standing.ByGrace {
				t.Errorf("%s: Standing = %+v, %v; want held %t, by no grace rule", tc.name, standing, err, tc.held)
			}
		})
	}
}

func signInAda(r *holdRig, _ *stubLister) string  { return r.signIn(adaClaims()) }
func signInErin(r *holdRig, _ *stubLister) string { return r.signIn(erinClaims()) }

func signInPoolMember(r *holdRig, _ *stubLister) string {
	return r.signIn(memberClaims(memberGroup))
}

// updateClaims is a change that makes every refresh token Google issued so far answer with
// claims changed by change.
func updateClaims(change func(claims map[string]any)) func(*holdRig, *stubLister) {
	return func(r *holdRig, _ *stubLister) { r.issuer.UpdateGrants(change) }
}

// TestStandingAfterGoogleCannotBeReached (c-trigger): with Google answering 503, Standing answers
// not held at 2 hours after the last confirmation and held, by the 24-hour rule, at 25 hours, both
// from a refresh and from the pause the refresh started, and writes nothing.
//
// Control: remove the 24-hour ceiling from Standing (graceRule never holds): the 25-hour case
// answers not held, and the test fails naming it.
func TestStandingAfterGoogleCannotBeReached(t *testing.T) {
	ctx := context.Background()
	r := newHoldRig(t, DomainRule{Domain: holdDomain})
	email := r.signIn(adaClaims())
	// The refreshes Dispatch starts: the code flow itself may ask the token endpoint again on a 5xx.
	refresher := &countingRefresher{next: r.flow}
	r.membership.SignIn = refresher
	r.issuer.FailToken(http.StatusServiceUnavailable, "")
	r.confirmedAgo(email, 2*time.Hour)
	before := r.row(email)
	generation := r.generation(email)
	if standing, err := r.membership.Standing(ctx, email); err != nil || standing.Held || standing.ByGrace {
		t.Fatalf("Standing at 2 hours with Google answering 503 = %+v, %v; want not held", standing, err)
	}
	if refreshes := refresher.calls.Load(); refreshes != 1 {
		t.Fatalf("refreshes at 2 hours = %d, want 1", refreshes)
	}
	r.confirmedAgo(email, 25*time.Hour)
	if standing, err := r.membership.Standing(ctx, email); err != nil || !standing.Held || !standing.ByGrace || standing.Unopenable {
		t.Fatalf("Standing at 25 hours with Google answering 503, served by the pause = %+v, %v; want held by the 24-hour rule", standing, err)
	}
	r.newProcess()
	if standing, err := r.membership.Standing(ctx, email); err != nil || !standing.Held || !standing.ByGrace || standing.Unopenable {
		t.Fatalf("Standing at 25 hours with Google answering 503, after a refresh = %+v, %v; want held by the 24-hour rule", standing, err)
	}
	if refreshes := refresher.calls.Load(); refreshes != 2 {
		t.Fatalf("refreshes after the second process's call = %d, want 2: the pause served the first process's second call", refreshes)
	}
	after := r.row(email)
	if after.HeldSince != nil || after.SignedOutAt != nil || *after.RefreshToken != *before.RefreshToken ||
		r.generation(email) != generation {
		t.Fatalf("the row after Google answered 503 = %+v, generation %d; want nothing written (before %+v, generation %d)", after, r.generation(email), before, generation)
	}
}

// TestStandingAnswersTheRuleAfterTwoSecondsPastTwentyFourHours (c-trigger): with a confirmation 25
// hours old and a refresh that Google confirms after 4 seconds, Standing answers held by the 24-hour
// rule within 3 seconds, a second caller that arrives 3 seconds into the refresh is answered held at
// once, and a call after the refresh has confirmed answers not held. No Google refresh reads the
// access lists (OutsideRule's Person is DomainRule's), so none fails on them.
//
// Control: make Standing wait for the refresh's own end past 24 hours: the test fails naming the
// 4-second wait.
func TestStandingAnswersTheRuleAfterTwoSecondsPastTwentyFourHours(t *testing.T) {
	t.Parallel()
	t.Run("a refresh that confirms after 4 seconds", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		r := newHoldRig(t, DomainRule{Domain: holdDomain})
		email := r.signIn(adaClaims())
		refresher := &countingRefresher{next: r.flow, delay: 4 * time.Second, started: make(chan time.Time, 1)}
		r.membership.SignIn = refresher
		r.confirmedAgo(email, 25*time.Hour)
		begun := time.Now()
		standing, err := r.membership.Standing(ctx, email)
		if took := time.Since(begun); err != nil || !standing.Held || !standing.ByGrace || took >= 3*time.Second {
			t.Fatalf("Standing at 25 hours while a refresh takes 4 seconds = %+v, %v after %s; want held by the 24-hour rule within 3 seconds, not after the 4-second wait", standing, err, took)
		}
		started := <-refresher.started
		time.Sleep(time.Until(started.Add(3 * time.Second)))
		second := time.Now()
		standing, err = r.membership.Standing(ctx, email)
		if took := time.Since(second); err != nil || !standing.Held || !standing.ByGrace || took >= 500*time.Millisecond {
			t.Fatalf("a second call 3 seconds into the refresh = %+v, %v after %s; want held at once", standing, err, took)
		}
		waitForFlight(t, r.membership, email, 5*time.Second)
		if standing, err := r.membership.Standing(ctx, email); err != nil || standing.Held {
			t.Fatalf("a call after the refresh confirmed = %+v, %v; want not held", standing, err)
		}
		if calls := refresher.calls.Load(); calls != 1 {
			t.Fatalf("refreshes = %d, want the one", calls)
		}
	})
}

// A refresh that takes 8 seconds and then is refused: Standing answers by the 24-hour rule within
// 3 seconds, and the refresh, which goes on in the background, then records the hold.
func TestStandingWaitsAtMostTwoSeconds(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newHoldRig(t, DomainRule{Domain: holdDomain})
	email := r.signIn(adaClaims())
	r.membership.SignIn = &countingRefresher{next: r.flow, delay: 8 * time.Second}
	r.issuer.RevokeGrants()
	r.confirmedAgo(email, 2*time.Hour)
	begun := time.Now()
	standing, err := r.membership.Standing(ctx, email)
	if took := time.Since(begun); err != nil || standing.Held || took >= 3*time.Second {
		t.Fatalf("Standing while the refresh takes 8 seconds = %+v, %v after %s; want not held by the 24-hour rule within 3 seconds", standing, err, took)
	}
	waitForFlight(t, r.membership, email, 12*time.Second)
	if row := r.row(email); row.HeldSince == nil {
		t.Fatalf("the refresh refused after 8 seconds recorded no hold: %+v", row)
	}
	if standing, err := r.membership.Standing(ctx, email); err != nil || !standing.Held || standing.ByGrace {
		t.Fatalf("Standing after the refresh recorded the hold = %+v, %v; want held, by the recorded hold", standing, err)
	}
}

// After a refresh ends with Google not reached, a second call within 60 seconds starts no refresh
// and answers by the 24-hour rule; and a 2-second timeout starts no pause.
func TestStandingPausesAfterGoogleCannotBeReached(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	t.Run("Google not reached", func(t *testing.T) {
		r := newHoldRig(t, DomainRule{Domain: holdDomain})
		email := r.signIn(adaClaims())
		refresher := &countingRefresher{next: r.flow}
		r.membership.SignIn = refresher
		r.issuer.FailToken(http.StatusServiceUnavailable, "")
		r.confirmedAgo(email, 2*time.Hour)
		for _, call := range []string{"the first call", "the second call"} {
			if standing, err := r.membership.Standing(ctx, email); err != nil || standing.Held {
				t.Fatalf("%s at 2 hours = %+v, %v; want not held", call, standing, err)
			}
		}
		r.confirmedAgo(email, 25*time.Hour)
		if standing, err := r.membership.Standing(ctx, email); err != nil || !standing.Held || !standing.ByGrace {
			t.Fatalf("the paused call at 25 hours = %+v, %v; want held by the 24-hour rule", standing, err)
		}
		if refreshes := refresher.calls.Load(); refreshes != 1 {
			t.Fatalf("refreshes = %d, want 1: the pause starts none", refreshes)
		}
	})
	t.Run("a 2-second timeout", func(t *testing.T) {
		r := newHoldRig(t, DomainRule{Domain: holdDomain})
		email := r.signIn(adaClaims())
		release := make(chan struct{})
		var once sync.Once
		var calls atomic.Int32
		r.membership.SignIn = refresherFunc(func(ctx context.Context, token string) (oidc.Session, error) {
			if calls.Add(1) == 1 {
				<-release
			}
			return r.flow.Refresh(ctx, token)
		})
		t.Cleanup(func() {
			once.Do(func() { close(release) })
			waitForFlight(t, r.membership, email, 5*time.Second)
		})
		r.confirmedAgo(email, 2*time.Hour)
		if standing, err := r.membership.Standing(ctx, email); err != nil || standing.Held {
			t.Fatalf("the call that waited 2 seconds = %+v, %v; want not held by the 24-hour rule", standing, err)
		}
		if _, paused := r.membership.unavailable.Load(email); paused {
			t.Fatal("the 2-second wait started a pause")
		}
		once.Do(func() { close(release) })
		waitForFlight(t, r.membership, email, 5*time.Second)
		r.confirmedAgo(email, 2*time.Hour)
		if standing, err := r.membership.Standing(ctx, email); err != nil || standing.Held {
			t.Fatalf("the call after the slow refresh = %+v, %v; want not held", standing, err)
		}
		if got := calls.Load(); got != 2 {
			t.Fatalf("refreshes = %d, want 2: the timeout paused nothing", got)
		}
	})
}

// While a refresh of the person has run 3 seconds, a new call answers by the 24-hour rule within
// 100 ms rather than wait on it.
func TestStandingDoesNotWaitOnALongRefresh(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newHoldRig(t, DomainRule{Domain: holdDomain})
	email := r.signIn(adaClaims())
	release := make(chan struct{})
	started := make(chan time.Time, 1)
	r.membership.SignIn = refresherFunc(func(ctx context.Context, token string) (oidc.Session, error) {
		started <- time.Now()
		<-release
		return r.flow.Refresh(ctx, token)
	})
	t.Cleanup(func() {
		close(release)
		waitForFlight(t, r.membership, email, 5*time.Second)
	})
	r.confirmedAgo(email, 25*time.Hour)
	go func() { _, _ = r.membership.Standing(ctx, email) }()
	time.Sleep(time.Until((<-started).Add(3 * time.Second)))
	begun := time.Now()
	standing, err := r.membership.Standing(ctx, email)
	if took := time.Since(begun); err != nil || !standing.Held || !standing.ByGrace || took >= 100*time.Millisecond {
		t.Fatalf("a call while the refresh has run 3 seconds = %+v, %v after %s; want held by the 24-hour rule within 100 ms", standing, err, took)
	}
}

// Standing reads the address as people.email holds it: "  Ada@Example.com " reads ada's row, and a
// refreshed ID token whose email claim is Ada@Example.com confirms ada and holds nobody.
func TestStandingReadsTheCanonicalAddress(t *testing.T) {
	ctx := context.Background()
	r := newHoldRig(t, DomainRule{Domain: holdDomain})
	claims := adaClaims()
	claims["email"] = "Ada@Example.com"
	email := r.signIn(claims)
	if email != adaEmail {
		t.Fatalf("the sign-in recorded %q, want %s", email, adaEmail)
	}
	r.confirmedAgo(email, 2*time.Hour)
	if standing, err := r.membership.Standing(ctx, "  Ada@Example.com "); err != nil || standing.Held {
		t.Fatalf("Standing of the spelled address after a refresh naming Ada@Example.com = %+v, %v; want not held", standing, err)
	}
	if row := r.row(email); row.HeldSince != nil || row.RefreshToken == nil || time.Since(*row.ConfirmedAt) > time.Minute {
		t.Fatalf("ada's row after the refresh = %+v, want confirmed just now and not held", row)
	}
	if r.issuer.Refreshes() != 1 {
		t.Fatalf("refreshes = %d, want 1", r.issuer.Refreshes())
	}
	read, _, err := r.people.Membership(ctx, email)
	if err != nil {
		t.Fatal(err)
	}
	if ended, err := r.people.EndSubject(ctx, email, read.Subject, read.RefreshToken, auth.EndHold); err != nil || !ended {
		t.Fatalf("hold ada: %t, %v", ended, err)
	}
	if standing, err := r.membership.Standing(ctx, "  Ada@Example.com "); err != nil || !standing.Held {
		t.Fatalf("Standing of the spelled address once ada is held = %+v, %v; want held", standing, err)
	}
}

// A stored refresh token that does not open, on a row that records a Google account, answers by
// the unopenable rule: held at 25 hours after the last confirmation, with Unopenable set, and not
// held at 2, and Google is never asked. A row that records no Google account is not held.
func TestStandingOfAnUnopenableTokenIsTheGraceRule(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		spoil func(r *holdRig, email string)
	}{
		{"a token stored in plain text", func(r *holdRig, email string) {
			if _, err := r.database.Pool.Exec(ctx, `update people set refresh_token = 'plain' where email = $1`, email); err != nil {
				r.t.Fatal(err)
			}
		}},
		{"a token sealed under a key no longer in the key set", func(r *holdRig, email string) {
			retired := store.NewPgPeopleStore(r.database.Pool, auth.SingleSigningKey("a-retired-key"), nil)
			if err := retired.SignInSubject(ctx, email, holdSubject, "refresh-under-the-retired-key", holdDomain, model.MemberFull); err != nil {
				r.t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newHoldRig(t, DomainRule{Domain: holdDomain})
			email := r.signIn(adaClaims())
			tc.spoil(r, email)
			r.confirmedAgo(email, 2*time.Hour)
			if standing, err := r.membership.Standing(ctx, email); err != nil || standing.Held || standing.Unopenable {
				t.Fatalf("Standing at 2 hours = %+v, %v; want not held", standing, err)
			}
			r.confirmedAgo(email, 25*time.Hour)
			if standing, err := r.membership.Standing(ctx, email); err != nil || !standing.Held || !standing.ByGrace || !standing.Unopenable {
				t.Fatalf("Standing at 25 hours = %+v, %v; want held by the unopenable rule", standing, err)
			}
			if refreshes := r.issuer.Refreshes(); refreshes != 0 {
				t.Fatalf("refreshes = %d, want none: the token does not open", refreshes)
			}
			// A row that records no Google account: the trigger refuses clearing sub, so the test
			// stands for such a row through the store's test-only helper, as #4199's tests do.
			storetest.TestOnlyClearAccount(t, r.database, email)
			if standing, err := r.membership.Standing(ctx, email); err != nil || standing.Held {
				t.Fatalf("Standing at 25 hours of a row that records no Google account = %+v, %v; want not held", standing, err)
			}
		})
	}
}

// A not-held answer by the 24-hour rule carries the time of the first read, which is earlier than
// the moment the refresh began, and a held answer after the refresh carries the re-read's, which is
// later than it.
func TestStandingAsOfIsTheReadItRestsOn(t *testing.T) {
	ctx := context.Background()
	r := newHoldRig(t, DomainRule{Domain: holdDomain})
	email := r.signIn(adaClaims())
	var asked time.Time
	r.membership.SignIn = refresherFunc(func(ctx context.Context, token string) (oidc.Session, error) {
		asked = r.databaseNow()
		time.Sleep(50 * time.Millisecond)
		return r.flow.Refresh(ctx, token)
	})
	r.issuer.FailToken(http.StatusServiceUnavailable, "")
	r.confirmedAgo(email, 2*time.Hour)
	standing, err := r.membership.Standing(ctx, email)
	if err != nil || standing.Held || standing.AsOf.IsZero() || !standing.AsOf.Before(asked) {
		t.Fatalf("the not-held answer = %+v, %v; want not held, as of the first read, before the refresh began at %s", standing, err, asked)
	}
	r.newProcess()
	r.issuer.FailToken(0, "")
	r.issuer.RevokeGrants()
	held, err := r.membership.Standing(ctx, email)
	if err != nil || !held.Held || !held.AsOf.After(asked) || !held.AsOf.After(standing.AsOf) {
		t.Fatalf("the held answer = %+v, %v; want held, as of the re-read, after the refresh began at %s and after %s", held, err, asked, standing.AsOf)
	}
}

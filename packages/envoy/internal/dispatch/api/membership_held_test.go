// membership_held_test.go holds the API to a hold (K1, K2, K3, K5): when Google refuses a person's
// Dispatch sign-in, or no refresh has confirmed them for 24 hours, their personal tokens and the
// broker tokens naming them are refused 401 MEMBERSHIP_HELD until they sign in again, after every
// revocation and admission check, on a request, at a live socket's heartbeat and at a machine
// login's decision; and the person route the broker's hold gate asks answers held and as_of.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/ciphertoken/ciphertokentest"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/cipher"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/oidc"
)

// standingFake is a Deps.Standing answering what answer says (not held while it is nil), and
// keeping each email it was asked about, so a test counts what a check cost. directory is its
// ChecksDirectory: false, cookie identity, unless a test of IAP identity sets it.
type standingFake struct {
	mu        sync.Mutex
	asked     []string
	answer    func(email string) (identity.Standing, error)
	directory bool
}

func (f *standingFake) Standing(_ context.Context, email string) (identity.Standing, error) {
	f.mu.Lock()
	f.asked = append(f.asked, email)
	answer := f.answer
	f.mu.Unlock()
	if answer == nil {
		return identity.Standing{}, nil
	}
	return answer(email)
}

func (f *standingFake) ChecksDirectory() bool { return f.directory }

// answers makes every later call answer standing and err.
func (f *standingFake) answers(standing identity.Standing, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answer = func(string) (identity.Standing, error) { return standing, err }
}

// askedSoFar is every email asked about so far, in order.
func (f *standingFake) askedSoFar() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.asked)
}

// refusingRefresher is Google refusing every refresh (invalid_grant), counting the refreshes.
type refusingRefresher struct{ calls atomic.Int64 }

func (r *refusingRefresher) Refresh(context.Context, string) (oidc.Session, error) {
	r.calls.Add(1)
	return oidc.Session{}, fmt.Errorf("%w: invalid_grant", oidc.ErrGrantRefused)
}

// heldClock is the broker-token check's clock in a test that outlives an answer the check keeps
// (ciphertoken.AffirmativeCacheTTL) without sleeping through it.
type heldClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *heldClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *heldClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// servedWith is the API over deps, as another process of the deployment newTestServer built deps
// for serves it: the same database and broker, with the fields a test changed.
func servedWith(t *testing.T, deps Deps) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	served := register(mux, t.Context(), deps)
	t.Cleanup(func() { waitBackgroundOf(t, served) })
	return mux
}

// peopleOf is the people store over database that the server under test reads.
func peopleOf(database *store.Store) *store.PgPeopleStore {
	return store.NewPgPeopleStore(database.Pool, testSigningKey, nil)
}

// signInFor records email's Google sign-in under subject with refresh, as kind, which releases any
// recorded hold, and dates its confirmation age back.
func signInFor(t *testing.T, database *store.Store, email, subject, refresh string, kind model.MemberKind, age time.Duration) {
	t.Helper()
	ctx := context.Background()
	if err := peopleOf(database).SignInSubject(ctx, email, subject, refresh, "", kind); err != nil {
		t.Fatalf("sign %s in: %v", email, err)
	}
	if _, err := database.Pool.Exec(ctx, `update people set confirmed_at = now() - make_interval(secs => $2) where email = $1`, email, age.Seconds()); err != nil {
		t.Fatalf("date %s's confirmation %s back: %v", email, age, err)
	}
}

// holdPerson records a hold of email as Google's refusal of their refresh does
// (store.PgPeopleStore.EndSubject with auth.EndHold), for the sign-in under subject with refresh.
func holdPerson(t *testing.T, database *store.Store, email, subject, refresh string) {
	t.Helper()
	ended, err := peopleOf(database).EndSubject(context.Background(), email, subject, refresh, auth.EndHold)
	if err != nil || !ended {
		t.Fatalf("record a hold of %s: ended %t, %v", email, ended, err)
	}
}

// recordedHold is whether people records a hold of email.
func recordedHold(t *testing.T, database *store.Store, email string) bool {
	t.Helper()
	var held bool
	if err := database.Pool.QueryRow(context.Background(), `select coalesce(held_since > signed_in_at, false) from people where email = $1`, email).Scan(&held); err != nil {
		t.Fatalf("read whether %s is held: %v", email, err)
	}
	return held
}

// wantHeldRefusal fails the test unless response is 401 MEMBERSHIP_HELD saying text.
func wantHeldRefusal(t *testing.T, response *httptest.ResponseRecorder, text, what string) {
	t.Helper()
	var refusal refusalAnswer
	_ = json.Unmarshal(response.Body.Bytes(), &refusal)
	if response.Code != http.StatusUnauthorized || refusal.Code != codeMembershipHeld || refusal.Error != text {
		t.Fatalf("%s = %d %s, want 401 %s saying %q", what, response.Code, strings.TrimSpace(response.Body.String()), codeMembershipHeld, text)
	}
}

// brokerHeldText is the broker's words for a hold of person, which Dispatch and the listener give a
// broker token whose introspection answered held.
func brokerHeldText(person string) string {
	return person + "'s credentials are held by Dispatch; they work again when " + person +
		" signs in to Dispatch, or when Dispatch can confirm " + person + " with Google again"
}

// heldPersonToken is the broker's answer for a dev box session's token acting for person, bound
// to jkt, held as held says.
func heldPersonToken(jkt, person string, held bool) ciphertoken.Introspection {
	answer := liveNamedToken(jkt)
	answer.Person, answer.ProjectScope, answer.Projects, answer.Held = person, ciphertoken.ProjectScopePerson, nil, held
	return answer
}

// TestAPersonalTokenOfAHeldPersonIsRefused: a personal token whose owner Dispatch holds is 401
// MEMBERSHIP_HELD, in words for the rule that holds them. A recorded hold is read in the token's
// own statement and asks Standing nothing, and tells the owner to sign in again; once their
// confirmation is older than an hour the token asks Standing, whose 24-hour rule says no refresh
// confirmed the owner in 24 hours and whose unopenable rule, and a hold its refresh recorded, take
// the recorded hold's words. A Standing that fails is 503 IDENTITY_UNAVAILABLE, never an accept.
// Where no Google rule signs people in (Standing nil) a recorded hold holds nobody, and the owner's
// next sign-in releases it.
func TestAPersonalTokenOfAHeldPersonIsRefused(t *testing.T) {
	_, database, deps := newTestServer(t, testServerOptions{})
	signInFor(t, database, accessCarol, "carol-account", "carol-refresh", model.MemberFull, time.Minute)
	token := personalTokenOwnedBy(t, database, accessCarol, model.ScopePerson)
	standing := &standingFake{}
	deps.Standing = standing
	held := servedWith(t, deps)
	withoutRule := deps
	withoutRule.Standing = nil
	unruled := servedWith(t, withoutRule)
	whoami := func(handler http.Handler) *httptest.ResponseRecorder {
		t.Helper()
		return agentRequest(t, handler, http.MethodGet, "/api/v1/whoami", nil, token)
	}
	if response := whoami(held); response.Code != http.StatusOK {
		t.Fatalf("carol's token before any hold = %d %s, want 200", response.Code, response.Body.String())
	}

	holdPerson(t, database, accessCarol, "carol-account", "carol-refresh")
	wantHeldRefusal(t, whoami(held), membershipHeldText, "carol's token once Dispatch records her hold")
	if asked := standing.askedSoFar(); len(asked) != 0 {
		t.Fatalf("the token of a person whose hold is recorded asked Standing about %v, want nothing: its own statement reads the hold", asked)
	}
	if response := whoami(unruled); response.Code != http.StatusOK {
		t.Fatalf("carol's token with her hold recorded where no Google rule signs people in = %d %s, want 200", response.Code, response.Body.String())
	}

	signInFor(t, database, accessCarol, "carol-account", "carol-refresh-again", model.MemberFull, 2*time.Hour)
	for _, test := range []struct {
		rule     string
		standing identity.Standing
		err      error
		status   int
		code     string
		text     string
	}{
		{"the 24-hour rule", identity.Standing{Held: true, ByGrace: true}, nil, http.StatusUnauthorized, codeMembershipHeld, membershipHeldGraceText},
		{"the unopenable rule", identity.Standing{Held: true, ByGrace: true, Unopenable: true}, nil, http.StatusUnauthorized, codeMembershipHeld, membershipHeldText},
		{"a hold Standing's refresh recorded", identity.Standing{Held: true}, nil, http.StatusUnauthorized, codeMembershipHeld, membershipHeldText},
		{"a Standing that fails", identity.Standing{}, identity.ErrMembershipUnreadable, http.StatusServiceUnavailable, "IDENTITY_UNAVAILABLE", ""},
		{"a Standing that answers not held", identity.Standing{}, nil, http.StatusOK, "", ""},
	} {
		standing.answers(test.standing, test.err)
		before := len(standing.askedSoFar())
		response := whoami(held)
		var refusal refusalAnswer
		_ = json.Unmarshal(response.Body.Bytes(), &refusal)
		if response.Code != test.status || refusal.Code != test.code || (test.text != "" && refusal.Error != test.text) {
			t.Errorf("carol's token, her confirmation two hours old, under %s = %d %s, want %d %s %q",
				test.rule, response.Code, strings.TrimSpace(response.Body.String()), test.status, test.code, test.text)
		}
		if asked := standing.askedSoFar()[before:]; len(asked) != 1 || asked[0] != accessCarol {
			t.Errorf("carol's token under %s asked Standing about %v, want %s once", test.rule, asked, accessCarol)
		}
	}
}

// TestAPersonalTokenOfASpelledOwnerIsHeld: a token whose row names its owner in other letters, or
// with spaces around the address, than the owner's people row is held when the owner is. Erin's
// confirmation is two hours old and Google refuses her refresh, and nothing records a hold of her
// yet, so the token's first request is refused only through Standing (a real identity.Membership),
// asked with erin's address as her row holds it, whose refresh records the hold.
//
// Control: with both canonicalisations removed, the token check's and Standing's own, Standing
// reads no row for the address the token spells, erin's first request is served, and the test
// fails naming the owner the token spells.
func TestAPersonalTokenOfASpelledOwnerIsHeld(t *testing.T) {
	for _, spelled := range []string{"Erin@Other.Example", "  erin@other.example  "} {
		t.Run(strings.TrimSpace(spelled), func(t *testing.T) {
			if spelled == accessErin || model.CanonicalLogin(spelled) != accessErin {
				t.Fatalf("%q must differ from erin's row %q only in case or surrounding space", spelled, accessErin)
			}
			handler, database, deps := newTestServer(t, testServerOptions{admitsRestricted: true})
			seedPeople(t, database, accessErin)
			markRestricted(t, database, accessErin)
			if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{"key": "ACC", "name": "Access project"}, "alice"); response.Code != http.StatusCreated {
				t.Fatalf("create ACC: %d %s", response.Code, response.Body.String())
			}
			if listed := dispatchRequest(t, handler, http.MethodPut, "/api/v1/projects/ACC/access/people/"+accessErin, nil, "alice"); listed.Code != http.StatusOK {
				t.Fatalf("alice names erin on ACC's list: %d %s", listed.Code, listed.Body.String())
			}
			signInFor(t, database, accessErin, "erin-account", "erin-refresh", model.MemberRestricted, 2*time.Hour)
			token := personalTokenOwnedBy(t, database, spelled, model.ScopePerson)
			google := &refusingRefresher{}
			deps.Standing = &identity.Membership{People: peopleOf(database), SignIn: google, Rule: testOutsideRule(database.Pool)}
			if recordedHold(t, database, accessErin) {
				t.Fatal("people records a hold of erin before her token's first request")
			}
			response := agentRequest(t, servedWith(t, deps), http.MethodGet, "/api/v1/whoami", nil, token)
			if response.Code != http.StatusUnauthorized || !strings.Contains(response.Body.String(), `"code":"`+codeMembershipHeld+`"`) {
				t.Fatalf("the first request of the token owned by %q, erin's confirmation two hours old and Google refusing her refresh = %d %s, want 401 %s",
					spelled, response.Code, strings.TrimSpace(response.Body.String()), codeMembershipHeld)
			}
			if google.calls.Load() != 1 || !recordedHold(t, database, accessErin) {
				t.Fatalf("the token owned by %q was refused after %d refreshes, erin's hold recorded %t: want it refused through Standing's one refresh, which records the hold",
					spelled, google.calls.Load(), recordedHold(t, database, accessErin))
			}
		})
	}
}

// TestABrokerTokenOfAHeldPersonIsRefused: a broker token naming a person the broker's
// introspection answers held is 401 MEMBERSHIP_HELD in the broker's own words, naming the person,
// whatever rule signs people in. A hold Dispatch records is read in the admission's one statement
// where a Google rule signs people in, and refuses a token the introspection answers not held, in
// a recorded hold's words; where none does it holds nobody. A broker token asks Standing nothing,
// and the person's next sign-in releases the recorded hold.
func TestABrokerTokenOfAHeldPersonIsRefused(t *testing.T) {
	rig := newBrokerTokenRig(t)
	seedPeople(t, rig.database, accessCarol)
	signInFor(t, rig.database, accessCarol, "carol-account", "carol-refresh", model.MemberFull, time.Minute)
	caller := newBrokerCaller(t)
	const heldToken, unheldToken = "carols-token-the-broker-answers-held", "carols-token-the-broker-answers-not-held"
	rig.broker.Says(heldToken, heldPersonToken(caller.jkt, accessCarol, true))
	rig.broker.Says(unheldToken, heldPersonToken(caller.jkt, accessCarol, false))
	standing := &standingFake{}
	ruled := rig.deps
	ruled.Standing = standing
	servers := map[string]http.Handler{"with a Google rule": servedWith(t, ruled), "with no Google rule": rig.handler}
	whoami := func(handler http.Handler, token string) *httptest.ResponseRecorder {
		t.Helper()
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, caller.presenting(t, http.MethodGet, "/api/v1/whoami", token, nil))
		return response
	}
	for name, handler := range servers {
		wantHeldRefusal(t, whoami(handler, heldToken), brokerHeldText(accessCarol), "carol's broker token answered held, "+name)
		if response := whoami(handler, unheldToken); response.Code != http.StatusOK {
			t.Fatalf("carol's broker token answered not held, %s = %d %s, want 200", name, response.Code, response.Body.String())
		}
	}

	holdPerson(t, rig.database, accessCarol, "carol-account", "carol-refresh")
	wantHeldRefusal(t, whoami(servers["with a Google rule"], unheldToken), membershipHeldText,
		"carol's broker token answered not held, with her hold recorded and a Google rule")
	if response := whoami(servers["with no Google rule"], unheldToken); response.Code != http.StatusOK {
		t.Fatalf("carol's broker token answered not held, with her hold recorded and no Google rule = %d %s, want 200", response.Code, response.Body.String())
	}
	signInFor(t, rig.database, accessCarol, "carol-account", "carol-refresh-again", model.MemberFull, time.Minute)
	if response := whoami(servers["with a Google rule"], unheldToken); response.Code != http.StatusOK {
		t.Fatalf("carol's broker token once she signed in again = %d %s, want 200", response.Code, response.Body.String())
	}
	if asked := standing.askedSoFar(); len(asked) != 0 {
		t.Fatalf("broker tokens asked Standing about %v, want nothing: the broker's gate asked the person route", asked)
	}
}

// TestAFreshConfirmationCostsNoStandingCall: a personal token whose owner's confirmation is younger
// than an hour (identity.MembershipMaxAge) asks Standing nothing and costs the two statements it
// always did, the token's lookup and the record of its use; at 61 minutes it asks Standing once,
// about the owner.
func TestAFreshConfirmationCostsNoStandingCall(t *testing.T) {
	_, database, deps := newTestServer(t, testServerOptions{})
	ctx := context.Background()
	signInFor(t, database, accessCarol, "carol-account", "carol-refresh", model.MemberFull, 59*time.Minute)
	token := personalTokenOwnedBy(t, database, accessCarol, model.ScopePerson)
	standing := &standingFake{}
	recorder := &storetest.StatementRecorder{}
	deps.Store = &store.Store{Pool: storetest.Traced(t, database.Pool, recorder, nil)}
	deps.Standing = standing
	served := directServer(deps)
	before := len(recorder.Statements())
	actor, err := served.personalTokenActor(ctx, token)
	statements := recorder.Statements()[before:]
	if err != nil || actor.Owner == nil || len(standing.askedSoFar()) != 0 || len(statements) != 2 {
		t.Fatalf("carol's token, her confirmation 59 minutes old: %+v, %v, asking Standing about %v, in %d statements; want her token, no Standing call, 2 statements:\n%s",
			actor, err, standing.askedSoFar(), len(statements), strings.Join(statements, "\n---\n"))
	}
	if _, err := database.Pool.Exec(ctx, `update people set confirmed_at = now() - interval '61 minutes' where email = $1`, accessCarol); err != nil {
		t.Fatal(err)
	}
	if actor, err := served.personalTokenActor(ctx, token); err != nil || actor.Owner == nil {
		t.Fatalf("carol's token, her confirmation 61 minutes old and Standing answering not held: %+v, %v; want her token", actor, err)
	}
	if asked := standing.askedSoFar(); len(asked) != 1 || asked[0] != accessCarol {
		t.Fatalf("carol's token, her confirmation 61 minutes old, asked Standing about %v, want %s once", asked, accessCarol)
	}
}

// openHeldSocket opens a connection held to its caller (whileCallerResolves with the live socket's
// resolution, callerStillResolves, and its grant compare) for request on served, and answers once
// it is open a channel that says, once it ends, whether the heartbeat closed it within ten seconds.
func openHeldSocket(t *testing.T, served *server, request *http.Request) <-chan bool {
	t.Helper()
	opened, ended := make(chan struct{}), make(chan bool, 1)
	connection := served.whileCallerResolves(served.callerStillResolves, true, func(_ http.ResponseWriter, r *http.Request) {
		close(opened)
		select {
		case <-r.Context().Done():
			ended <- true
		case <-time.After(10 * time.Second):
			ended <- false
		}
	})
	go connection.ServeHTTP(httptest.NewRecorder(), request)
	<-opened
	time.Sleep(4 * served.deps.StreamHeartbeat)
	select {
	case closed := <-ended:
		t.Fatalf("the socket ended (closed %t) before its person was held", closed)
	default:
	}
	return ended
}

// TestALiveSocketOfAHeldPersonCloses: a live socket closes at its next heartbeat after its person
// is held. One opened with a personal token closes once Dispatch records the hold. One opened with
// a broker token closes once the broker's introspection answers held, with no hold recorded, at the
// first heartbeat after the check's kept answer lapses (ciphertoken.AffirmativeCacheTTL).
//
// Control: callerStillResolves without its Held check; the broker token's socket stays open and
// the test fails naming it.
func TestALiveSocketOfAHeldPersonCloses(t *testing.T) {
	const heartbeat = 50 * time.Millisecond
	t.Run("a personal token", func(t *testing.T) {
		_, database, deps := newTestServer(t, testServerOptions{})
		signInFor(t, database, accessCarol, "carol-account", "carol-refresh", model.MemberFull, time.Minute)
		token := personalTokenOwnedBy(t, database, accessCarol, model.ScopePerson)
		deps.Standing, deps.StreamHeartbeat = &standingFake{}, heartbeat
		request := httptest.NewRequest(http.MethodGet, "/ws/live", nil)
		request.Header.Set("Authorization", "Bearer "+token)
		ended := openHeldSocket(t, directServer(deps), request)
		holdPerson(t, database, accessCarol, "carol-account", "carol-refresh")
		if !<-ended {
			t.Fatal("the socket carol's personal token opened was still open 10 s after Dispatch recorded her hold")
		}
	})
	t.Run("a broker token", func(t *testing.T) {
		broker := ciphertokentest.NewStubBroker(t)
		clock := &heldClock{at: time.Now()}
		_, database, deps := newTestServer(t, testServerOptions{brokerTokens: stubBrokerTokens(t, broker, func(o *ciphertoken.Options) { o.Now = clock.now })})
		seedPeople(t, database, accessCarol)
		caller := newBrokerCaller(t)
		const token = "carols-broker-token-holding-a-socket"
		broker.Says(token, heldPersonToken(caller.jkt, accessCarol, false))
		deps.Standing, deps.StreamHeartbeat = &standingFake{}, heartbeat
		ended := openHeldSocket(t, directServer(deps), caller.presenting(t, http.MethodGet, "/ws/live", token, nil))
		broker.Says(token, heldPersonToken(caller.jkt, accessCarol, true))
		clock.advance(ciphertoken.AffirmativeCacheTTL)
		if !<-ended {
			t.Fatal("the socket carol's broker token opened was still open 10 s after the broker's introspection answered her held: the heartbeat read no held answer")
		}
		if recordedHold(t, database, accessCarol) {
			t.Fatal("people records a hold of carol: the broker token's socket had to close on the introspection's answer alone")
		}
	})
}

// TestABrokerTokenOfAReplacedAccountAnswersRevokedNotHeld (c-held-token-verifier): a broker token
// the broker's introspection answers held gets #4199's refusals first, at brokerTokenActor, with a
// Google rule signing people in (Standing set). Another account's sign-in replaced carol's
// first account while the revocation of it is owed: the replaced account's token is 401
// BROKER_TOKEN_NOT_ACTIVE, owed and once settled; a token of an account that has not signed in
// since Google gave it the address is 401 BROKER_ACCOUNT_NOT_SIGNED_IN; a token recording no
// account made before the recording is 401 BROKER_TOKEN_RECORDS_NO_ACCOUNT; the holder's own token,
// which #4199 lets act while the revocation is owed, is 401 MEMBERSHIP_HELD. The same holds with
// carol's hold recorded too. And erin, a restricted member held with a recorded hold, is 401
// MEMBERSHIP_HELD while ACC's list names her and 401 TOKEN_OWNER_NOT_ADMITTED once no list does.
//
// Controls: brokerTokenActor's Held check moved before brokerPersonAdmitted (the replaced account's
// token answers MEMBERSHIP_HELD, and the test fails naming it); the recorded hold read before
// restrictedOwnerAdmitted (the delisted erin answers MEMBERSHIP_HELD, and the test fails naming her).
func TestABrokerTokenOfAReplacedAccountAnswersRevokedNotHeld(t *testing.T) {
	const google = "https://accounts.google.com"
	rig := newOutsideBrokerRig(t)
	ctx := context.Background()
	standing := &standingFake{}
	deps := rig.offDeps
	deps.SignInIssuer, deps.Standing, deps.RestrictedAdmission = google, standing, testOutsideRule(rig.database.Pool)
	served := directServer(deps)
	exec := func(statement string, args ...any) {
		t.Helper()
		if _, err := rig.database.Pool.Exec(ctx, statement, args...); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}

	signInFor(t, rig.database, accessCarol, "member-account", "carol-refresh", model.MemberFull, time.Minute)
	exec(`insert into broker_revocations (email, issuer, subject, holder_subject, recorded_at, session_generation)
		values ($1, $2, 'outsider-account', 'member-account', now(), (select generation from user_sessions where login = $1))`, accessCarol, google)
	var recordedAt time.Time
	if err := rig.database.Pool.QueryRow(ctx, `select sub_recorded_at from people where email = $1`, accessCarol).Scan(&recordedAt); err != nil {
		t.Fatal(err)
	}
	// held is a token of carol's that the broker answers held, recording subject (none for "") and
	// made at created (an unrecorded time for the zero time).
	held := func(name, subject string, created time.Time) string {
		answer := heldPersonToken(rig.caller.jkt, accessCarol, true)
		if subject != "" {
			answer.AccountIssuer, answer.AccountSubject = google, subject
		}
		if !created.IsZero() {
			answer.CredentialCreatedAt = created.Unix()
		}
		rig.broker.Says(name, answer)
		return name
	}
	cases := []struct {
		name, token, code, text string
	}{
		{"the replaced account's held token", held("the-replaced-accounts-token", "outsider-account", time.Time{}), "BROKER_TOKEN_NOT_ACTIVE", ""},
		{"the held token of an account not signed in since Google gave it the address", held("a-new-accounts-token", "new-account", time.Time{}), "BROKER_ACCOUNT_NOT_SIGNED_IN", ""},
		{"a held token recording no account, made before the recording", held("a-token-recording-no-account", "", recordedAt.Add(-time.Hour)), "BROKER_TOKEN_RECORDS_NO_ACCOUNT", ""},
		{"the holder's own held token", held("the-holders-token", "member-account", time.Time{}), codeMembershipHeld, brokerHeldText(accessCarol)},
	}
	refused := func(when string) {
		t.Helper()
		for _, c := range cases {
			_, _, err := served.brokerTokenActor(rig.caller.presenting(t, http.MethodGet, "/api/v1/whoami", c.token, nil))
			wantAPIError(t, err, http.StatusUnauthorized, c.code, c.name+", "+when)
			if c.text != "" && !strings.Contains(err.Error(), c.text) {
				t.Fatalf("%s, %s: %v, want it to say %q", c.name, when, err, c.text)
			}
		}
	}
	refused("while the revocation of the replaced account is owed")
	exec(`update broker_revocations set done_at = now() where email = $1`, accessCarol)
	refused("once the revocation settled")
	holdPerson(t, rig.database, accessCarol, "member-account", "carol-refresh")
	cases[3].text = membershipHeldText
	refused("with carol's hold recorded too")

	signInFor(t, rig.database, accessErin, "erin-account", "erin-refresh", model.MemberRestricted, time.Minute)
	holdPerson(t, rig.database, accessErin, "erin-account", "erin-refresh")
	erins := heldPersonToken(rig.caller.jkt, accessErin, true)
	erins.AccountIssuer, erins.AccountSubject = google, "erin-account"
	rig.broker.Says("erins-held-token", erins)
	erin := func() error {
		_, _, err := served.brokerTokenActor(rig.caller.presenting(t, http.MethodGet, "/api/v1/whoami", "erins-held-token", nil))
		return err
	}
	wantAPIError(t, erin(), http.StatusUnauthorized, codeMembershipHeld, "the held token of "+accessErin+", a restricted member ACC's list names, with a recorded hold")
	if unlisted := dispatchRequest(t, rig.on, http.MethodDelete, "/api/v1/projects/ACC/access/people/"+accessErin, nil, "alice"); unlisted.Code != http.StatusOK {
		t.Fatalf("alice takes erin off ACC's list: %d %s", unlisted.Code, unlisted.Body.String())
	}
	wantAPIError(t, erin(), http.StatusUnauthorized, codeTokenOwnerNotAdmitted, "the held token of "+accessErin+", a restricted member whom no list names, with a recorded hold")
	if asked := standing.askedSoFar(); len(asked) != 0 {
		t.Fatalf("broker tokens asked Standing about %v, want nothing", asked)
	}
}

// heldRequestBody is a request body whose first read waits, after saying it began, until the test
// lets it go: a client that sent its headers and holds its body back.
type heldRequestBody struct {
	body             io.Reader
	reading, release chan struct{}
	once             sync.Once
}

func newHeldRequestBody(body string) *heldRequestBody {
	return &heldRequestBody{body: strings.NewReader(body), reading: make(chan struct{}), release: make(chan struct{})}
}

func (b *heldRequestBody) Read(p []byte) (int, error) {
	b.once.Do(func() {
		close(b.reading)
		<-b.release
	})
	return b.body.Read(p)
}

// serveWithBodyHeld serves request through handler while the client holds body back: once the
// handler waits on the body it runs whileHeld, then lets the body go and answers the response.
func serveWithBodyHeld(t *testing.T, handler http.Handler, request *http.Request, body *heldRequestBody, whileHeld func()) *httptest.ResponseRecorder {
	t.Helper()
	recorder, answered := httptest.NewRecorder(), make(chan struct{})
	go func() {
		defer close(answered)
		handler.ServeHTTP(recorder, request)
	}()
	release := sync.OnceFunc(func() { close(body.release) })
	defer release()
	select {
	case <-body.reading:
	case <-answered:
		t.Fatalf("%s %s answered %d %s before reading its body", request.Method, request.URL.Path, recorder.Code, recorder.Body.String())
	case <-time.After(30 * time.Second):
		t.Fatalf("%s %s did not begin reading its body within 30 s", request.Method, request.URL.Path)
	}
	whileHeld()
	release()
	<-answered
	return recorder
}

// TestAHeldApproverRelaysNoApproval: a machine login's approve or deny by a personal token whose
// owner was held while its client held the body back is 401 MEMBERSHIP_HELD once the body arrives,
// and the broker is not called, not even for its settings: the decision's re-read of its caller
// (approverStanding) reads the hold, as the token's own check does, before the approve reads
// whether the broker takes the approver fields. The held decisions come first, while Dispatch's
// client has read no settings, so a settings read would be one more. The control, the same
// decisions with no hold, reaches the broker stand-in, which shows it records a call, and its
// approve reads the settings once. The personal token proves the held approver, since it is a
// credential that may approve.
//
// A broker token approves and denies nothing: no broker-issued token holds the approve scope
// (brokerPersonScopes), so with no hold its decisions are 403 OAUTH_SCOPE_REQUIRED, and with its
// person held the hold Dispatch records refuses them first, at the token's admission
// (brokerPersonAdmitted), 401 MEMBERSHIP_HELD. Neither reaches the broker.
func TestAHeldApproverRelaysNoApproval(t *testing.T) {
	relay := newFakeBrokerRig(t)
	introspection := ciphertokentest.NewStubBroker(t)
	_, database, deps := newTestServer(t, testServerOptions{brokerTokens: stubBrokerTokens(t, introspection)})
	deps.AgentSecrets = cipher.New(relay.server.URL, accountTokenFile(t, credentialTestAccountToken))
	deps.Standing = &standingFake{}
	handler := servedWith(t, deps)
	seedPeople(t, database, accessCarol)
	signInFor(t, database, accessCarol, "carol-account", "carol-refresh", model.MemberFull, time.Minute)
	personal := personalTokenOwnedBy(t, database, accessCarol, model.ScopePerson, model.ScopeApprove)
	caller := newBrokerCaller(t)
	const brokerToken = "carols-machine-broker-token"
	introspection.Says(brokerToken, heldPersonToken(caller.jkt, accessCarol, false))
	byPersonalToken := func(path string, body io.Reader) *http.Request {
		request := httptest.NewRequest(http.MethodPost, path, body)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer "+personal)
		return request
	}
	byBrokerToken := func(path string, body io.Reader) *http.Request {
		signed := caller.caller.Signed(t, http.MethodPost, testServerURL+path, brokerToken)
		request := httptest.NewRequest(http.MethodPost, path, body)
		request.RemoteAddr = brokerTestAddress
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", signed.Header.Get("Authorization"))
		request.Header.Set("DPoP", signed.Header.Get("DPoP"))
		return request
	}
	serve := func(request *http.Request) *httptest.ResponseRecorder {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	const decision = `{"code":"ABCD-1234"}`
	routes := []string{"approve", "deny"}
	for _, route := range routes {
		path := "/api/v1/credential-requests/rec1/" + route
		refresh := "carol-refresh-a-personal-token-" + route
		signInFor(t, database, accessCarol, "carol-account", refresh, model.MemberFull, time.Minute)
		before := relay.calls
		body := newHeldRequestBody(decision)
		response := serveWithBodyHeld(t, handler, byPersonalToken(path, body), body, func() {
			holdPerson(t, database, accessCarol, "carol-account", refresh)
		})
		wantHeldRefusal(t, response, membershipHeldText, "carol's "+route+" by a personal token whose body arrived after she was held")
		if relay.calls != before || relay.settingsReads != 0 {
			t.Fatalf("carol's %s by a personal token, held while its body was held back, reached the broker %d times and read its settings %d times, want neither",
				route, relay.calls-before, relay.settingsReads)
		}
		// With carol held, her broker token's decision is refused at its admission, which answers
		// before the route's scope and reads no body, so the body is not held back. Until the base
		// made a broker token decide nothing, this row held the body back as the personal token's
		// does.
		wantHeldRefusal(t, serve(byBrokerToken(path, strings.NewReader(decision))), membershipHeldText, "carol's "+route+" by her broker token while she is held")
		if relay.calls != before || relay.settingsReads != 0 {
			t.Fatalf("carol's %s by her broker token while she is held reached the broker %d times and read its settings %d times, want neither", route, relay.calls-before, relay.settingsReads)
		}
	}
	// The control: carol signs in again, which ends her hold.
	signInFor(t, database, accessCarol, "carol-account", "carol-refresh-the-control", model.MemberFull, time.Minute)
	for _, route := range routes {
		path := "/api/v1/credential-requests/rec1/" + route
		before, readsBefore := relay.calls, relay.settingsReads
		if response := serve(byPersonalToken(path, strings.NewReader(decision))); response.Code != http.StatusOK || relay.calls != before+1 {
			t.Fatalf("the control: carol's %s by a personal token, not held = %d %s with %d broker calls, want 200 and one call",
				route, response.Code, response.Body.String(), relay.calls-before)
		}
		// The approve reads the settings once, on a client that has read none; the deny reads none.
		wantReads := readsBefore
		if route == "approve" {
			wantReads++
		}
		if relay.settingsReads != wantReads {
			t.Fatalf("the control: carol's %s by a personal token read the broker's settings %d times in all, want %d", route, relay.settingsReads, wantReads)
		}
		// A broker token neither approves nor mints, whatever its person's standing: the route
		// refuses it 403 OAUTH_SCOPE_REQUIRED, since no broker-issued token holds approve. This row
		// wanted 200 and a broker call until the base made that rule.
		before = relay.calls
		response := serve(byBrokerToken(path, strings.NewReader(decision)))
		if refusal := decodeBody[refusalAnswer](t, response); response.Code != http.StatusForbidden || refusal.Code != codeScopeRequired || relay.calls != before {
			t.Fatalf("carol's %s by her broker token, not held = %d %s with %d broker calls, want 403 %s and none",
				route, response.Code, response.Body.String(), relay.calls-before, codeScopeRequired)
		}
	}
}

// personAnswerOf is GET /api/v1/access/person?person= on handler under bearer: its status, its
// fields as sent, and its decoded answer.
func personAnswerOf(t *testing.T, handler http.Handler, bearer, person string) (int, map[string]json.RawMessage, model.PersonAnswer) {
	t.Helper()
	response := agentRequest(t, handler, http.MethodGet, "/api/v1/access/person?person="+person, nil, bearer)
	var fields map[string]json.RawMessage
	var answer model.PersonAnswer
	if err := json.Unmarshal(response.Body.Bytes(), &fields); err != nil {
		t.Fatalf("GET the person answer of %s = %d %s, which is no JSON object: %v", person, response.Code, response.Body.String(), err)
	}
	if err := json.Unmarshal(response.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode the person answer of %s: %v", person, err)
	}
	return response.Code, fields, answer
}

// TestThePersonAnswerCarriesHeld (K1): the person route answers held and as_of beside member_kind.
// With Standing, held is its answer and as_of the time of the read it rests on, a person no row
// holds included (held false); with no Google rule (Standing nil) held is false and as_of the
// database's now(); and a Standing that fails leaves held out, keeps member_kind and as_of, and
// answers 200.
func TestThePersonAnswerCarriesHeld(t *testing.T) {
	issuer := newServiceTokenIssuer(t)
	_, database, deps := newTestServer(t, testServerOptions{oidc: issuer.verifier(t), accessAccounts: []string{listedServiceAccount}})
	bearer := issuer.Mint(t, issuer.signing, issuer.Claims(listedServiceAccount, serviceTokenAudience))
	ctx := context.Background()
	seedPeople(t, database, accessCarol, accessDave)
	signInFor(t, database, accessCarol, "carol-account", "carol-refresh", model.MemberFull, time.Minute)
	holdPerson(t, database, accessCarol, "carol-account", "carol-refresh")
	const nobody = "nobody@example.com"
	databaseNow := func() time.Time {
		t.Helper()
		var now time.Time
		if err := database.Pool.QueryRow(ctx, `select now()`).Scan(&now); err != nil {
			t.Fatal(err)
		}
		return now
	}
	// wantAnswer asks for person on handler and wants 200, kind, held (nil for left out) and an
	// as_of of asOf, or, for the zero time, one the database's clock read during the request.
	wantAnswer := func(what string, handler http.Handler, person, kind string, held *bool, asOf time.Time) {
		t.Helper()
		from := databaseNow()
		code, fields, answer := personAnswerOf(t, handler, bearer, person)
		to := databaseNow()
		if !asOf.IsZero() {
			from, to = asOf, asOf
		}
		_, carriesHeld := fields["held"]
		heldOK := (held == nil && !carriesHeld) || (held != nil && carriesHeld && answer.Held != nil && *answer.Held == *held)
		if code != http.StatusOK || answer.MemberKind != kind || !heldOK || answer.AsOf.Before(from) || answer.AsOf.After(to) {
			t.Errorf("%s: the person answer of %s = %d %v, want 200, member_kind %s, held %v and as_of within [%s, %s]",
				what, person, code, fields, kind, describeHeld(held), from.Format(time.RFC3339Nano), to.Format(time.RFC3339Nano))
		}
	}
	yes, no := true, false

	wantAnswer("with no Google rule", servedWith(t, deps), accessCarol, model.MemberAnswerFull, &no, time.Time{})

	ruled := deps
	ruled.Standing = &identity.Membership{People: peopleOf(database), SignIn: &refusingRefresher{}, Rule: identity.DomainRule{Domain: "example.com"}}
	standing := servedWith(t, ruled)
	wantAnswer("with Standing, carol's hold recorded", standing, accessCarol, model.MemberAnswerFull, &yes, time.Time{})
	wantAnswer("with Standing, dave stored no refresh token", standing, accessDave, model.MemberAnswerFull, &no, time.Time{})
	wantAnswer("with Standing, a person no row holds", standing, nobody, model.MemberAnswerNone, &no, time.Time{})

	asOf := time.Date(2026, 11, 17, 9, 30, 0, 0, time.UTC)
	fixed := &standingFake{}
	fixed.answers(identity.Standing{Held: true, AsOf: asOf}, nil)
	ruled.Standing = fixed
	wantAnswer("with Standing answering a held read of its own time", servedWith(t, ruled), accessDave, model.MemberAnswerFull, &yes, asOf)

	failing := &standingFake{}
	failing.answers(identity.Standing{}, errors.New("read membership: the database did not answer"))
	ruled.Standing = failing
	wantAnswer("with Standing failing", servedWith(t, ruled), accessCarol, model.MemberAnswerFull, nil, time.Time{})
}

// describeHeld is a wanted held as a failure names it.
func describeHeld(held *bool) string {
	if held == nil {
		return "left out"
	}
	return fmt.Sprint(*held)
}

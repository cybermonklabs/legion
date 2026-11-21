package main

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/wire"
	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/ciphertoken/ciphertokentest"
	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/api"
	"github.com/sjawhar/envoy/internal/dispatch/approvetimingtest"
	"github.com/sjawhar/envoy/internal/dispatch/routes"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/metrics"
)

// With approvetimingtest.Env set, this package's tests time each approve's relay; the package
// reads its own variable here, in a test file, as Dispatch reads its environment only through
// the settings table.
func init() {
	if os.Getenv(approvetimingtest.Env) != "" {
		approvetimingtest.Enable()
	}
}

// The credentials a person holds at the secrets broker end with their hold on the address, as their
// sessions and personal tokens do: once another Google account takes the address, Dispatch tells
// the broker which account it replaced, and the broker ends everything that account holds under the
// address and leaves the new holder's.

// brokerOrigin is the origin the broker-token tests' Dispatch serves: the origin a DPoP proof's
// target is checked against (brokerTokenVerifier).
const brokerOrigin = "http://dispatch.test"

// fakeBroker is the secrets broker as Dispatch reaches it: the introspection route answers what
// each test says each token is (ciphertokentest.StubBroker), each token of a person's credential
// naming the account the credential records as the broker's introspection does, POST
// /v1/person-subjects/revoke ends the credentials of the account it names as the broker's does and
// records, as the broker's replaced_person_subjects does (internal/cipher/store's
// RecordReplacedSubjectTx), that the account no longer holds the address, so it is refused there
// from then on, and that the holder does, deleting any earlier record of the holder; an approve
// answers that it minted a machine login, and a deny that it denied one. Each credential holds the
// access tokens minted under it, and the revoke ends them with it. Every other call - the
// per-credential lists and revokes an earlier Dispatch made among them - is answered 404, and
// every call but an introspection is recorded as "METHOD path body".
type fakeBroker struct {
	stub   *ciphertokentest.StubBroker
	server *httptest.Server
	// issuer is the issuer the broker records each credential's account under, Dispatch's own
	// sign-in issuer (brokerRouter sets it), unless a credential names another.
	issuer string

	mu          sync.Mutex
	credentials []*fakeCredential
	called      []string
	// replaced is each account, by address and `sub`, a revoke recorded as no longer holding the
	// address and no later revoke named as its holder: the accounts the broker refuses there.
	replaced map[[2]string]bool
	// refuse, when not zero, is the status the subject revoke answers instead, ending nothing.
	refuse int
	// supersededBy, when not zero, is the generation the subject revoke answers it was superseded
	// by (state "superseded"), ending and recording nothing, as the broker answers a revoke that
	// reached it after a later replacement at the address.
	supersededBy int64
	// duringRevoke, when set, runs inside each subject revoke before the broker answers, as
	// whatever else happens while a call is in flight: another process's pass, or time passing.
	duringRevoke func()
}

// fakeCredential is one machine login (signIn false) or one sign-in on a machine (signIn true) of
// person, or a service's login (service set, person the login that approved it). subject is the
// Google account the broker recorded for it - the person token's of a sign-in, the approving
// account's of a login - and "" for one made before the broker recorded any; issuer is that
// account's issuer when it is not the broker's own (fakeBroker.issuer). createdAt is when the
// broker made it, which its tokens answer (credential_created_at), and the zero time for one whose
// time the broker did not record.
type fakeCredential struct {
	id, person, subject, issuer, service string
	signIn                               bool
	tokens                               []string
	revoked                              bool
	createdAt                            time.Time
}

func newFakeBroker(t *testing.T) *fakeBroker {
	t.Helper()
	broker := &fakeBroker{stub: ciphertokentest.NewStubBroker(t), replaced: map[[2]string]bool{}}
	broker.server = httptest.NewServer(approvetimingtest.Broker(func() string { return broker.server.URL }, http.HandlerFunc(broker.serve)))
	t.Cleanup(broker.server.Close)
	return broker
}

// holds gives the broker a live credential and answers each of tokens as a live token of person
// bound to caller's key, as the exchange mints a person's session token, naming the account the
// credential records, none for one that records none.
func (b *fakeBroker) holds(credential *fakeCredential, caller *ciphertokentest.Caller) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.credentials = append(b.credentials, credential)
	var issuer string
	if credential.subject != "" {
		issuer = cmp.Or(credential.issuer, b.issuer)
	}
	for i, token := range credential.tokens {
		if credential.service != "" {
			b.stub.Says(token, ciphertoken.Introspection{
				Active: true, Session: credential.id + "-session", Service: credential.service, ProjectScope: ciphertoken.ProjectScopePerson,
				Scope: "dispatch.write", ExpiresAt: time.Now().Add(time.Hour).Unix(), ClientID: ciphertoken.ExchangeClientID, Thumbprint: caller.JKT(),
			})
			continue
		}
		var createdAt int64
		if !credential.createdAt.IsZero() {
			createdAt = credential.createdAt.Unix()
		}
		b.stub.Says(token, ciphertoken.Introspection{
			Active: true, Session: credential.id + "-session-" + string(rune('a'+i)), Person: credential.person, ProjectScope: ciphertoken.ProjectScopePerson,
			AccountIssuer: issuer, AccountSubject: credential.subject, CredentialCreatedAt: createdAt,
			Scope: "dispatch.write", ExpiresAt: time.Now().Add(time.Hour).Unix(), ClientID: ciphertoken.ExchangeClientID, Thumbprint: caller.JKT(),
		})
	}
}

// calls is every call Dispatch made to the broker but an introspection, in order.
func (b *fakeBroker) calls() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.called)
}

// refusing makes the subject revoke answer status, or answer again for 0.
func (b *fakeBroker) refusing(status int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refuse = status
}

// superseding makes the subject revoke answer superseded by generation, or answer again for 0.
func (b *fakeBroker) superseding(generation int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.supersededBy = generation
}

// whileRevoking makes each subject revoke run during before it answers.
func (b *fakeBroker) whileRevoking(during func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.duringRevoke = during
}

// live reports whether the broker still holds the credential id unrevoked.
func (b *fakeBroker) live(id string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, c := range b.credentials {
		if c.id == id {
			return !c.revoked
		}
	}
	return false
}

// refuses reports whether the broker records the account subject as one that no longer holds
// person's address, so it refuses that account's sign-in trades, approvals and approval-page
// sign-ins there.
func (b *fakeBroker) refuses(person, subject string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.replaced[[2]string{person, subject}]
}

func (b *fakeBroker) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == ciphertoken.IntrospectPath {
		b.stub.Server.Config.Handler.ServeHTTP(w, r)
		return
	}
	body, _ := io.ReadAll(r.Body)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.called = append(b.called, r.Method+" "+r.URL.Path+" "+strings.TrimSpace(string(body)))
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/person-subjects/revoke":
		if b.duringRevoke != nil {
			b.duringRevoke()
		}
		if b.refuse != 0 {
			w.WriteHeader(b.refuse)
			_, _ = w.Write([]byte(`{"code":"REFUSED","error":"the test refuses it"}`))
			return
		}
		if b.supersededBy != 0 {
			_ = json.NewEncoder(w).Encode(wire.PersonSubjectRevokedResponse{State: "superseded", SupersededBy: new(b.supersededBy)})
			return
		}
		var revocation wire.RevokePersonSubjectRequest
		_ = json.Unmarshal(body, &revocation)
		answer := wire.PersonSubjectRevokedResponse{State: "revoked"}
		for _, c := range b.credentials {
			if c.revoked || c.service != "" || c.person != revocation.Person || c.subject != revocation.Subject && c.subject != "" {
				continue
			}
			c.revoked = true
			if c.signIn {
				answer.SignIns++
			} else {
				answer.MachineLogins++
			}
			answer.TokensEnded += len(c.tokens)
			for _, token := range c.tokens {
				b.stub.Says(token, ciphertoken.Introspection{})
			}
		}
		b.replaced[[2]string{revocation.Person, revocation.Subject}] = true
		delete(b.replaced, [2]string{revocation.Person, revocation.HolderSubject})
		_ = json.NewEncoder(w).Encode(answer)
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/credential-requests/") && strings.HasSuffix(r.URL.Path, "/approve"):
		_, _ = w.Write([]byte(`{"state":"approved","credential_id":"login-approved"}`))
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/credential-requests/") && strings.HasSuffix(r.URL.Path, "/deny"):
		_, _ = w.Write([]byte(`{"state":"denied"}`))
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"code":"NOT_FOUND","error":"not found"}`))
	}
}

// brokerRouter is the router main serves for s's boot with the secrets broker configured
// (DISPATCH_AGENT_SECRETS_URL naming broker) and brokerOrigin as DISPATCH_SERVER_URL, its
// broker-token check built as main builds it (brokerTokenVerifier) over s's database, its people
// store with the options main gives one for that boot (peopleOptions), which s.people then is, and
// the database's own session store, as routerWithSessions has it; it answers the API server's
// background work too, whose revocation pass a test runs as the server's loop does. The broker
// records accounts under s's sign-in issuer, the one Dispatch's approves name. Each approve it
// serves is timed to the broker's receipt of its relay (approvetimingtest).
func (s *outsideSignIn) brokerRouter(t *testing.T, broker *fakeBroker) (http.Handler, api.Background) {
	t.Helper()
	broker.issuer = s.pool.URL()
	accountFile := filepath.Join(t.TempDir(), "account-token")
	if err := os.WriteFile(accountFile, []byte("dispatch-account-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	boot := s.boot
	boot.AgentSecretsURL = broker.server.URL
	boot.AgentSecretsTokenFile = accountFile
	verifier, err := brokerTokenVerifier(boot, s.database, brokerOrigin)
	if err != nil {
		t.Fatalf("build the broker-token check: %v", err)
	}
	s.people = testPeople{PgPeopleStore: store.NewPgPeopleStore(s.database.Pool, testSigningKey, nil, peopleOptions(boot)...), pool: s.database.Pool}
	s.metrics = metrics.New()
	built := routes.AppContextOptions{
		Lifetime: t.Context(), SigningKey: testSigningKey, SignIn: signInFlow(boot), People: s.people, ServerURL: brokerOrigin,
		Store: s.database, Sessions: store.NewPgSessionStore(s.database.Pool), BrokerTokens: verifier,
		Metrics: s.metrics, BrokerRevocationsSuperseded: api.NewBrokerRevocationsSuperseded(s.metrics),
	}
	rule, err := signInRule(boot, accessLister(built.Store))
	if err != nil {
		t.Fatal(err)
	}
	built.Identity = requestIdentityFor(boot, built.SigningKey, built.People, built.Sessions, built.SignIn, rule, nil)
	appCtx, err := routes.BuildAppContext(appContextOptions(boot, rule, built))
	if err != nil {
		t.Fatalf("build the router: %v", err)
	}
	handler, background := routes.New(appCtx)
	t.Cleanup(background.Wait)
	return approvetimingtest.Approves(broker.server.URL, handler), background
}

// brokerTokenRequest answers method path from handler with body, presenting token under the DPoP
// scheme with a fresh proof of caller's key for that target, as an agent holding a broker token
// sends it.
func brokerTokenRequest(t *testing.T, handler http.Handler, caller *ciphertokentest.Caller, method, path, body, token string) *httptest.ResponseRecorder {
	t.Helper()
	signed := caller.Signed(t, method, brokerOrigin+path, token)
	request := httptest.NewRequest(method, brokerOrigin+path, strings.NewReader(body))
	request.Header.Set("Authorization", signed.Header.Get("Authorization"))
	request.Header.Set("DPoP", signed.Header.Get("DPoP"))
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

// brokerRevocation is the broker_revocations rows of email: whether any is recorded, and whether
// the broker confirmed every one.
func (s *outsideSignIn) brokerRevocation(t *testing.T, email string) (recorded, done bool) {
	t.Helper()
	err := s.database.Pool.QueryRow(context.Background(), `
		select count(*) > 0, coalesce(bool_and(done_at is not null), false) from broker_revocations where email = $1
	`, email).Scan(&recorded, &done)
	if err != nil {
		t.Fatal(err)
	}
	return recorded, done
}

// brokerRevocationSessionGeneration is the session generation email's revocation of subject
// recorded.
func (s *outsideSignIn) brokerRevocationSessionGeneration(t *testing.T, email, subject string) int64 {
	t.Helper()
	var generation int64
	if err := s.database.Pool.QueryRow(context.Background(),
		`select session_generation from broker_revocations where email = $1 and subject = $2`, email, subject).Scan(&generation); err != nil {
		t.Fatalf("read the session generation of %s's revocation of %s: %v", email, subject, err)
	}
	return generation
}

// nullBrokerRevocationSessionGeneration makes email's revocation of subject hold no session
// generation, as one an earlier head recorded holds none, and restoreBrokerRevocationSessionGeneration
// puts back the address's generation; both write with the guard off, which refuses either.
func (s *outsideSignIn) nullBrokerRevocationSessionGeneration(t *testing.T, email, subject string) {
	t.Helper()
	s.writeBrokerRevocationSessionGeneration(t, email, subject, `null`)
}

func (s *outsideSignIn) restoreBrokerRevocationSessionGeneration(t *testing.T, email, subject string) {
	t.Helper()
	s.writeBrokerRevocationSessionGeneration(t, email, subject, `(select generation from user_sessions where login = broker_revocations.email)`)
}

func (s *outsideSignIn) writeBrokerRevocationSessionGeneration(t *testing.T, email, subject, value string) {
	t.Helper()
	for _, statement := range []string{
		`alter table broker_revocations disable trigger broker_revocations_binding_columns`,
		`update broker_revocations set session_generation = ` + value + ` where email = '` + email + `' and subject = '` + subject + `'`,
		`alter table broker_revocations enable trigger broker_revocations_binding_columns`,
	} {
		if _, err := s.database.Pool.Exec(context.Background(), statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
}

// ageBrokerRevocation moves the last claim of each of email's revocations a minute back, as the
// server's loop waits out its retry window before it claims one of the address again.
func (s *outsideSignIn) ageBrokerRevocation(t *testing.T, email string) {
	t.Helper()
	if _, err := s.database.Pool.Exec(context.Background(), `
		update broker_revocations set tried_at = tried_at - interval '1 minute' where email = $1
	`, email); err != nil {
		t.Fatal(err)
	}
}

// settledAgo moves the moment every settled revocation of email was settled back to ago before now.
// A settlement never moves (migration 20261118152422), so it writes with the guard off.
func (s *outsideSignIn) settledAgo(t *testing.T, email string, ago time.Duration) {
	t.Helper()
	storetest.TestOnlyWriteBrokerRevocations(t, s.database, `
		update broker_revocations set done_at = now() - make_interval(secs => $2) where email = $1 and done_at is not null
	`, email, ago.Seconds())
}

// reassignedEmail is an address of the Workspace that Google gives from one account to another:
// firstHolder signs in to it first, as a full member, and secondHolder after.
const (
	reassignedEmail = "alice@example.com"
	firstHolder     = "first-holder"
	secondHolder    = "second-holder"
)

// workspaceSignIn runs a whole sign-in through handler as the Workspace account subject (hd
// example.com) at email, a full member, and requires it to be signed in.
func (s *outsideSignIn) workspaceSignIn(t *testing.T, handler http.Handler, email, subject string) *httptest.ResponseRecorder {
	t.Helper()
	signedIn := codeFlowSignIn(t, handler, s.pool, map[string]any{"sub": subject, "email": email, "email_verified": true, "hd": "example.com"})
	if signedIn.Code != http.StatusFound || !sessionCookieSet(signedIn) {
		t.Fatalf("the Workspace account %s at %s: the callback answered %d %s, want a signed-in redirect", subject, email, signedIn.Code, signedIn.Body.String())
	}
	return signedIn
}

// subjectRevoke is the one call a revocation pass makes to the broker for the account replaced of
// email, of s's sign-in issuer, which holder took the address from, carrying the session generation
// that revocation records (broker_revocations.session_generation, which a recording always names:
// the generation its replacing sign-in left, which store.TestARecordingNamesTheSessionGenerationItsReplacementLeft
// holds the recording to).
func (s *outsideSignIn) subjectRevoke(t *testing.T, email, replaced, holder string) string {
	t.Helper()
	var generation *int64
	if err := s.database.Pool.QueryRow(context.Background(), `select session_generation from broker_revocations where email = $1 and subject = $2`,
		email, replaced).Scan(&generation); err != nil || generation == nil {
		t.Fatalf("the session generation the revocation of %s at %s records: %v, %v; want one", replaced, email, generation, err)
	}
	body, err := json.Marshal(wire.RevokePersonSubjectRequest{Person: email, Issuer: s.pool.URL(), Subject: replaced, HolderSubject: holder, SessionGeneration: generation})
	if err != nil {
		t.Fatal(err)
	}
	return "POST /v1/person-subjects/revoke " + string(body)
}

// sessionGeneration is email's session generation as its session store reads it
// (store.PgSessionStore.CurrentSessionGeneration), 0 when user_sessions holds no row for email.
func (s *outsideSignIn) sessionGeneration(t *testing.T, email string) int64 {
	t.Helper()
	generation, _, err := store.NewPgSessionStore(s.database.Pool).CurrentSessionGeneration(context.Background(), email)
	if err != nil {
		t.Fatalf("read the session generation of %s: %v", email, err)
	}
	return generation
}

// requireCalls requires the broker to have been told exactly want, in order.
func requireCalls(t *testing.T, broker *fakeBroker, what string, want ...string) {
	t.Helper()
	if got := broker.calls(); !slices.Equal(got, want) {
		t.Fatalf("%s, Dispatch told the broker:\n%s\nwant:\n%s", what, strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// A full member holds alice@example.com, and at the secrets broker a machine login they approved,
// a sign-in on a machine, a machine login from before the broker recorded any account, each holding
// a broker token of the address, and the Legion daemon's login, which the service's. Then Google
// gives the address to another Workspace account, which signs in. One revocation pass makes one call
// to the broker, naming the address, Dispatch's sign-in issuer, the replaced account, the account
// that took it and the address's session generation that sign-in left; the broker ends the replaced
// member's credentials and the unrecorded login with their tokens, and leaves the daemon's; the pass
// settles the revocation, and a pass after that tells the broker nothing.
func TestOutsideSignInReassignEndsTheReplacedAccountsBrokerCredentials(t *testing.T) {
	ctx := context.Background()
	s := newOutsideSignIn(t, nil)
	broker := newFakeBroker(t)
	handler, background := s.brokerRouter(t, broker)
	s.workspaceSignIn(t, handler, reassignedEmail, firstHolder)
	machine, box, older, daemon := ciphertokentest.NewCaller(t), ciphertokentest.NewCaller(t), ciphertokentest.NewCaller(t), ciphertokentest.NewCaller(t)
	broker.holds(&fakeCredential{id: "login-first", person: reassignedEmail, subject: firstHolder, tokens: []string{"token-of-the-first-holders-machine"}}, machine)
	broker.holds(&fakeCredential{id: "signin-first", person: reassignedEmail, subject: firstHolder, signIn: true, tokens: []string{"token-of-the-first-holders-sign-in"}}, box)
	broker.holds(&fakeCredential{id: "login-unrecorded", person: reassignedEmail, tokens: []string{"token-of-a-login-from-before"}}, older)
	broker.holds(&fakeCredential{id: "login-daemon", person: reassignedEmail, service: "legion-daemon", tokens: []string{"token-of-the-daemon"}}, daemon)
	requireTokenActs(t, "GET /api/v1/whoami with the first holder's machine's broker token, before the address passes on",
		brokerTokenRequest(t, handler, machine, http.MethodGet, "/api/v1/whoami", "", "token-of-the-first-holders-machine"))

	s.workspaceSignIn(t, handler, reassignedEmail, secondHolder)
	if recorded, done := s.brokerRevocation(t, reassignedEmail); !recorded || done {
		t.Fatalf("the second holder's sign-in recorded a broker revocation of %s: recorded %t, done %t; want one owed", reassignedEmail, recorded, done)
	}
	left := s.sessionGeneration(t, reassignedEmail)
	background.RevokeBrokerCredentials(ctx)
	revoke := s.subjectRevoke(t, reassignedEmail, firstHolder, secondHolder)
	if want := fmt.Sprintf(`"session_generation":%d}`, left); !strings.HasSuffix(revoke, want) {
		t.Fatalf("the revocation's call %s does not carry %s, the session generation the second holder's sign-in left", revoke, want)
	}
	requireCalls(t, broker, "after the revocation pass", revoke)
	if recorded, done := s.brokerRevocation(t, reassignedEmail); !recorded || !done {
		t.Fatalf("after the broker confirmed it, the broker revocation of %s reads recorded %t, done %t; want it settled", reassignedEmail, recorded, done)
	}
	for id, want := range map[string]bool{"login-first": false, "signin-first": false, "login-unrecorded": false, "login-daemon": true} {
		if got := broker.live(id); got != want {
			t.Errorf("after the revocation, the broker holds %s live %t, want %t", id, got, want)
		}
	}
	s.ageBrokerRevocation(t, reassignedEmail)
	background.RevokeBrokerCredentials(ctx)
	requireCalls(t, broker, "after a pass with nothing owed", revoke)
}

// Two replacements of one address before the first is paid are both paid, in the order they
// committed: the first account, which the second took, then the second, which the third took. The
// second is paid by the pass the retry window after the first's claim, which the loop's timer asks
// for (Background.RevokeBrokerCredentials answers when).
func TestOutsideSignInTwoReassignmentsBeforeAPassArePaidInTurn(t *testing.T) {
	ctx := context.Background()
	s := newOutsideSignIn(t, nil)
	broker := newFakeBroker(t)
	handler, background := s.brokerRouter(t, broker)
	for _, subject := range []string{firstHolder, secondHolder, "third-holder"} {
		s.workspaceSignIn(t, handler, reassignedEmail, subject)
	}
	first, second := s.subjectRevoke(t, reassignedEmail, firstHolder, secondHolder), s.subjectRevoke(t, reassignedEmail, secondHolder, "third-holder")
	next := background.RevokeBrokerCredentials(ctx)
	requireCalls(t, broker, "after the first pass", first)
	if wait := time.Until(next); wait < 25*time.Second || wait > 31*time.Second {
		t.Fatalf("the first pass asks for its next %s from now, want the retry window after its claim", wait)
	}
	s.ageBrokerRevocation(t, reassignedEmail)
	background.RevokeBrokerCredentials(ctx)
	requireCalls(t, broker, "after the second pass", first, second)
	if recorded, done := s.brokerRevocation(t, reassignedEmail); !recorded || !done {
		t.Fatalf("after both passes the revocations of %s read recorded %t, done %t; want both settled", reassignedEmail, recorded, done)
	}
}

// An account that takes its address back while the revocation of it is still owed is not revoked:
// the first holder signs in, Google gives the address to another Workspace account, which signs in,
// and gives it back to the first holder, who signs in again, all before a revocation pass runs. The
// first holder then signs in at the broker on a box. Paying the revocation of the first holder now
// would end that sign-in, and have the broker refuse the first holder until the revocation of the
// second holder was paid, the retry window later. So the first holder's reclaim settles the
// revocation of the first holder in its own transaction: the one pass tells the broker only to
// revoke the second holder, which the first holder took the address from, the first holder's
// sign-in at the broker stays live, and once the refusal by address is over its broker token acts,
// as the first holder's Dispatch session does throughout.
func TestOutsideSignInAnAccountThatTakesItsAddressBackIsNotRevoked(t *testing.T) {
	ctx := context.Background()
	s := newOutsideSignIn(t, nil)
	broker := newFakeBroker(t)
	handler, background := s.brokerRouter(t, broker)
	s.workspaceSignIn(t, handler, reassignedEmail, firstHolder)
	s.workspaceSignIn(t, handler, reassignedEmail, secondHolder)
	reclaimed := s.workspaceSignIn(t, handler, reassignedEmail, firstHolder)
	box := ciphertokentest.NewCaller(t)
	const signInToken = "token-of-the-first-holders-sign-in-after-the-reclaim"
	broker.holds(&fakeCredential{id: "signin-after-the-reclaim", person: reassignedEmail, subject: firstHolder, signIn: true, tokens: []string{signInToken}}, box)

	next := background.RevokeBrokerCredentials(ctx)
	requireCalls(t, broker, "after the pass", s.subjectRevoke(t, reassignedEmail, secondHolder, firstHolder))
	if !next.IsZero() {
		t.Errorf("the pass asks for its next %s from now, want none: nothing of %s is owed", time.Until(next), reassignedEmail)
	}
	if recorded, done := s.brokerRevocation(t, reassignedEmail); !recorded || !done {
		t.Fatalf("after the pass the revocations of %s read recorded %t, done %t; want both settled", reassignedEmail, recorded, done)
	}
	if !broker.live("signin-after-the-reclaim") {
		t.Errorf("the first holder's sign-in at the broker, made after it took %s back, was ended", reassignedEmail)
	}
	requireTokenActs(t, "GET /api/v1/whoami with the first holder's session from its reclaim",
		cookieRequest(handler, http.MethodGet, "/api/v1/whoami", "", reclaimed))
	s.settledAgo(t, reassignedEmail, 31*time.Second)
	requireTokenActs(t, "GET /api/v1/whoami with the broker token of the first holder's sign-in after the reclaim, the refusal by address over",
		brokerTokenRequest(t, handler, box, http.MethodGet, "/api/v1/whoami", "", signInToken))
}

// A broker that answers the revoke anything but 200 - a 404, which the route never answers (a wrong
// DISPATCH_AGENT_SECRETS_URL, a proxy in the way, a broker without the route), or a 500 - has ended
// nothing, so the revocation stays owed however many passes run, and the address's broker tokens
// stay refused; once the broker answers, the next pass settles it.
func TestOutsideSignInReassignLeavesTheRevocationOwedWhileTheBrokerRefuses(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			ctx := context.Background()
			s := newOutsideSignIn(t, nil)
			broker := newFakeBroker(t)
			handler, background := s.brokerRouter(t, broker)
			s.workspaceSignIn(t, handler, reassignedEmail, firstHolder)
			machine := ciphertokentest.NewCaller(t)
			broker.holds(&fakeCredential{id: "login-first", person: reassignedEmail, subject: firstHolder, tokens: []string{"token-of-the-first-holders-machine"}}, machine)
			broker.refusing(status)

			s.workspaceSignIn(t, handler, reassignedEmail, secondHolder)
			for pass := 1; pass <= 3; pass++ {
				background.RevokeBrokerCredentials(ctx)
				if recorded, done := s.brokerRevocation(t, reassignedEmail); !recorded || done {
					t.Fatalf("after pass %d against a broker answering %d, the revocation of %s reads recorded %t, done %t; want it owed", pass, status, reassignedEmail, recorded, done)
				}
				s.ageBrokerRevocation(t, reassignedEmail)
			}
			requireBrokerCredentialRevoked(t, "GET /api/v1/whoami with the first holder's machine's broker token, the revocation owed",
				brokerTokenRequest(t, handler, machine, http.MethodGet, "/api/v1/whoami", "", "token-of-the-first-holders-machine"))

			broker.refusing(0)
			background.RevokeBrokerCredentials(ctx)
			if recorded, done := s.brokerRevocation(t, reassignedEmail); !recorded || !done || broker.live("login-first") {
				t.Fatalf("once the broker answers, the revocation of %s reads recorded %t, done %t, login-first live %t; want it settled and the login ended", reassignedEmail, recorded, done, broker.live("login-first"))
			}
		})
	}
}

// A broker that answers the revoke 200 superseded (it applied a later replacement at the address
// first, so this revoke changed nothing there) leaves the revocation owed, where the owed gauges
// show it, and the pass counts the answer in envoy_dispatch_broker_revocations_superseded_total,
// since the loop hears that answer only when Dispatch's generation for the address is below the
// broker's and the replaced account's credentials were not ended: the fake ended nothing, so the
// first holder's login is still live there. Once the broker answers revoked, a pass after the retry
// window settles it and the login ends. The cmd rig's broker answers each call before Dispatch's
// client gives up, so it cannot deliver a revoke late; the broker's own store and enroll tests
// replay that (enroll.TestALateRevokeBelowTheAppliedGenerationChangesNothing).
//
// Controls: settle a superseded answer in the loop, and the revocation reads settled; drop the
// loop's count, and the counter reads nothing.
func TestOutsideSignInReassignLeavesARevocationTheBrokerAnswersSupersededOwed(t *testing.T) {
	ctx := context.Background()
	s := newOutsideSignIn(t, nil)
	broker := newFakeBroker(t)
	handler, background := s.brokerRouter(t, broker)
	s.workspaceSignIn(t, handler, reassignedEmail, firstHolder)
	machine := ciphertokentest.NewCaller(t)
	broker.holds(&fakeCredential{id: "login-first", person: reassignedEmail, subject: firstHolder, tokens: []string{"token-of-the-first-holders-machine"}}, machine)
	broker.superseding(99)

	s.workspaceSignIn(t, handler, reassignedEmail, secondHolder)
	background.RevokeBrokerCredentials(ctx)
	if recorded, done := s.brokerRevocation(t, reassignedEmail); !recorded || done || !broker.live("login-first") {
		t.Fatalf("after a pass the broker answered superseded, the revocation of %s reads recorded %t, done %t, login-first live %t; want it owed, the broker having ended nothing", reassignedEmail, recorded, done, broker.live("login-first"))
	}
	if superseded, ok := metricSample(t, scrapeMetrics(t, s.metrics), "envoy_dispatch_broker_revocations_superseded_total"); !ok || superseded != 1 {
		t.Fatalf("after the broker answered superseded, envoy_dispatch_broker_revocations_superseded_total reads %v (%t), want 1", superseded, ok)
	}
	revoke := s.subjectRevoke(t, reassignedEmail, firstHolder, secondHolder)

	broker.superseding(0)
	s.ageBrokerRevocation(t, reassignedEmail)
	background.RevokeBrokerCredentials(ctx)
	if recorded, done := s.brokerRevocation(t, reassignedEmail); !recorded || !done || broker.live("login-first") {
		t.Fatalf("once the broker answers revoked, the revocation of %s reads recorded %t, done %t, login-first live %t; want it settled and the login ended", reassignedEmail, recorded, done, broker.live("login-first"))
	}
	if calls := broker.calls(); !slices.Equal(calls, []string{revoke, revoke}) {
		t.Fatalf("the broker was told %q, want the revoke %q twice: once answered superseded, once revoked", calls, revoke)
	}
}

// A broker that answers superseded for a revocation another process's pass settled while this
// call was in flight (the call ran late, and that pass paid the address meanwhile) is not counted:
// the pass re-reads the row with Settle's condition, finds it settled, and logs WARN. The counter
// writes no sample.
//
// Control: drop the re-read (store.BrokerRevocations.StillOwed), and the counter reads 1.
func TestOutsideSignInASupersededAnswerForARevocationAnotherPassSettledIsNotCounted(t *testing.T) {
	ctx := context.Background()
	s := newOutsideSignIn(t, nil)
	broker := newFakeBroker(t)
	handler, background := s.brokerRouter(t, broker)
	s.workspaceSignIn(t, handler, reassignedEmail, firstHolder)
	s.workspaceSignIn(t, handler, reassignedEmail, secondHolder)
	broker.superseding(99)
	broker.whileRevoking(func() {
		if _, err := s.database.Pool.Exec(ctx, `update broker_revocations set done_at = now() where email = $1 and done_at is null`, reassignedEmail); err != nil {
			t.Errorf("another pass's settlement: %v", err)
		}
	})
	background.RevokeBrokerCredentials(ctx)
	if recorded, done := s.brokerRevocation(t, reassignedEmail); !recorded || !done {
		t.Fatalf("the revocation another pass settled reads recorded %t, done %t; want it settled", recorded, done)
	}
	if superseded, ok := metricSample(t, scrapeMetrics(t, s.metrics), "envoy_dispatch_broker_revocations_superseded_total"); ok && superseded != 0 {
		t.Errorf("envoy_dispatch_broker_revocations_superseded_total reads %v after a superseded answer for a settled revocation, want no count", superseded)
	}
}

// A claim takes the owed revocations of two addresses, and the pass calls the broker for one after
// the other. While the first call is in flight, the second row's claim ages past the retry window
// less the client's bound on a call (its tried_at set back 26 s, as a slow first call would leave
// it), so the pass does not send it: another process may claim that address's next revocation
// before such a call is over. It stays owed, and the next claim, once the retry window has passed,
// sends it.
//
// Control: make store.BrokerRevocations.Sendable read no claim age, and the pass sends both.
func TestOutsideSignInAClaimedRevocationPastItsSendWindowIsNotSent(t *testing.T) {
	ctx := context.Background()
	s := newOutsideSignIn(t, nil)
	broker := newFakeBroker(t)
	handler, background := s.brokerRouter(t, broker)
	const otherEmail = "bob@example.com"
	for _, email := range []string{reassignedEmail, otherEmail} {
		s.workspaceSignIn(t, handler, email, firstHolder+"-"+email)
		s.workspaceSignIn(t, handler, email, secondHolder+"-"+email)
	}
	var calls int
	broker.whileRevoking(func() {
		calls++
		if calls == 1 {
			if _, err := s.database.Pool.Exec(ctx, `update broker_revocations set tried_at = tried_at - interval '26 seconds' where done_at is null`); err != nil {
				t.Errorf("age the claims: %v", err)
			}
		}
	})
	background.RevokeBrokerCredentials(ctx)
	if got := broker.calls(); len(got) != 1 {
		t.Fatalf("the pass told the broker %q, want one call: the second row's claim was past its send window", got)
	}
	owed := 0
	for _, email := range []string{reassignedEmail, otherEmail} {
		if _, done := s.brokerRevocation(t, email); !done {
			owed++
		}
	}
	if owed != 1 {
		t.Fatalf("after the pass %d of the two revocations are owed, want 1: the one not sent", owed)
	}
	for _, email := range []string{reassignedEmail, otherEmail} {
		s.ageBrokerRevocation(t, email)
	}
	background.RevokeBrokerCredentials(ctx)
	if got := broker.calls(); len(got) != 2 {
		t.Fatalf("after the retry window the broker was told %q, want the second call too", got)
	}
}

// The hourly confirmation's end of a full member's sign-in, Google refusing their refresh, revokes
// nothing (R9), so nothing a person made is lost: no personal token of the member's is revoked, no
// broker revocation is recorded and no pass tells the broker anything, and once Google confirms the
// member again and they sign in, the personal token they made before acts again. Whether a person's
// tokens and broker credentials are held while their sign-in is lapsed is separate work, decided
// as a hold for everyone.
func TestOutsideSignInHourlyEndOfAFullMemberRevokesNothing(t *testing.T) {
	const memberEmail = "alice@example.com"
	ctx := context.Background()
	s := newOutsideSignIn(t, nil)
	broker := newFakeBroker(t)
	handler, background := s.brokerRouter(t, broker)
	signedIn := googleSignIn(t, handler, s.pool, "example.com")
	if signedIn.Code != http.StatusFound || !sessionCookieSet(signedIn) {
		t.Fatalf("the Workspace account %s: the callback answered %d %s, want a signed-in redirect", memberEmail, signedIn.Code, signedIn.Body.String())
	}
	token := mintTokenOn(t, handler, signedIn)
	machine := ciphertokentest.NewCaller(t)
	const machineToken = "token-of-the-members-machine"
	broker.holds(&fakeCredential{id: "login-member", person: memberEmail, subject: "google-subject", tokens: []string{machineToken}}, machine)
	requireTokenActs(t, "GET /api/v1/whoami with the member's personal token, before the hourly check ends the sign-in",
		bearerRequest(handler, http.MethodGet, "/api/v1/whoami", "", token))

	s.pool.FailToken(http.StatusBadRequest, "invalid_grant")
	ageConfirmation(t, s.people, memberEmail)
	if ended := requestWith(handler, http.MethodGet, "/auth/whoami", signedIn); ended.Code != http.StatusUnauthorized {
		t.Fatalf("the whoami that ran the hourly check of %s, whom Google no longer confirms, answered %d %s, want 401", memberEmail, ended.Code, ended.Body.String())
	}
	var revoked int
	if err := s.database.Pool.QueryRow(ctx, `select count(*) from agent_tokens where revoked_at is not null`).Scan(&revoked); err != nil {
		t.Fatal(err)
	}
	if revoked != 0 {
		t.Errorf("the hourly check's end of %s's sign-in revoked %d personal tokens, want none: a hold, not a revocation, is how a lapsed sign-in stops a token", memberEmail, revoked)
	}
	if recorded, _ := s.brokerRevocation(t, memberEmail); recorded {
		t.Errorf("the hourly check's end of %s's sign-in recorded a broker revocation, want none", memberEmail)
	}
	background.RevokeBrokerCredentials(ctx)
	if got := broker.calls(); len(got) != 0 {
		t.Errorf("after the hourly check's end of %s's sign-in, a revocation pass told the broker %q, want nothing", memberEmail, got)
	}

	s.pool.FailToken(0, "")
	if again := googleSignIn(t, handler, s.pool, "example.com"); again.Code != http.StatusFound || !sessionCookieSet(again) {
		t.Fatalf("the Workspace account %s signing in again: the callback answered %d %s, want a signed-in redirect", memberEmail, again.Code, again.Body.String())
	}
	requireTokenActs(t, "GET /api/v1/whoami with the member's personal token, once they signed in again",
		bearerRequest(handler, http.MethodGet, "/api/v1/whoami", "", token))
}

// A member who reclaims an address an account outside the Workspace took keeps their own broker
// credentials. The member's sign-in records a revocation of the outside account, naming the member
// as the holder, since a restricted row can name an account that made broker credentials while it
// was full under the same `sub` (the next test): the one pass tells the broker to revoke the
// outside account, and the machine login and sign-in the member made at the broker before
// reclaiming the address, which record the member's account, stay live, and their broker tokens
// act throughout, the revocation owed included, since they record the account the address's row
// records now and nothing can make them the outside account's.
func TestOutsideSignInReclaimKeepsTheMembersOwnBrokerCredentials(t *testing.T) {
	const memberEmail = "alice@second.example"
	ctx := context.Background()
	s := newOutsideSignIn(t, nil)
	broker := newFakeBroker(t)
	handler, background := s.brokerRouter(t, broker)
	s.listOn(t, "PRIV", access.PrincipalPerson, memberEmail)
	if outsider := codeFlowSignIn(t, handler, s.pool, map[string]any{"sub": "outsider", "email": memberEmail, "email_verified": true}); outsider.Code != http.StatusFound {
		t.Fatalf("the account outside the Workspace at %s: the callback answered %d %s, want a signed-in redirect", memberEmail, outsider.Code, outsider.Body.String())
	}
	machine, box := ciphertokentest.NewCaller(t), ciphertokentest.NewCaller(t)
	broker.holds(&fakeCredential{id: "login-member", person: memberEmail, subject: "member", tokens: []string{"token-of-the-members-machine"}}, machine)
	broker.holds(&fakeCredential{id: "signin-member", person: memberEmail, subject: "member", signIn: true, tokens: []string{"token-of-the-members-sign-in"}}, box)

	s.workspaceSignIn(t, handler, memberEmail, "member")
	if recorded, done := s.brokerRevocation(t, memberEmail); !recorded || done {
		t.Fatalf("the member's reclaim of %s from an outside account: a broker revocation recorded %t, done %t; want the outside account's owed", memberEmail, recorded, done)
	}
	requireTokenActs(t, "GET /api/v1/whoami with the member's machine's broker token, the outside account's revocation owed",
		brokerTokenRequest(t, handler, machine, http.MethodGet, "/api/v1/whoami", "", "token-of-the-members-machine"))
	background.RevokeBrokerCredentials(ctx)
	requireCalls(t, broker, "after the pass that follows the member's reclaim", s.subjectRevoke(t, memberEmail, "outsider", "member"))
	if recorded, done := s.brokerRevocation(t, memberEmail); !recorded || !done {
		t.Fatalf("after the pass the revocation of %s reads recorded %t, done %t; want it settled", memberEmail, recorded, done)
	}
	for _, id := range []string{"login-member", "signin-member"} {
		if !broker.live(id) {
			t.Errorf("the revocation of the outside account ended the member's own %s", id)
		}
	}
	s.settledAgo(t, memberEmail, 31*time.Second)
	requireTokenActs(t, "GET /api/v1/whoami with the member's machine's broker token, after the reclaim",
		brokerTokenRequest(t, handler, machine, http.MethodGet, "/api/v1/whoami", "", "token-of-the-members-machine"))
	requireTokenActs(t, "GET /api/v1/whoami with the member's sign-in's broker token, after the reclaim",
		brokerTokenRequest(t, handler, box, http.MethodGet, "/api/v1/whoami", "", "token-of-the-members-sign-in"))
}

// A full member's address that another Workspace account took, and that account restricted since
// under the same `sub`, is taken back with that account revoked at the broker. The first holder
// signs in to alice@second.example, a domain the Workspace does not list, which PRIV's list names;
// the second holder, a Workspace account too, takes it, and the pass pays the first holder's
// revocation, after which the broker refuses the first holder at the address. Google then moves
// the second holder's account out of the Workspace: its refresh carries no hd, so the hourly
// confirmation keeps it signed in as a restricted member. When the first holder takes the address
// back, its sign-in records the revocation of the second holder naming the first as the holder,
// and the next pass tells the broker exactly that: the broker ends what the second holder made and
// deletes its refusal of the first holder, which no other call lifts.
func TestOutsideSignInTakingTheAddressBackFromAnAccountRestrictedSinceRevokesIt(t *testing.T) {
	const email = "alice@second.example"
	ctx := context.Background()
	s := newOutsideSignIn(t, nil)
	broker := newFakeBroker(t)
	handler, background := s.brokerRouter(t, broker)
	s.listOn(t, "PRIV", access.PrincipalPerson, email)
	s.workspaceSignIn(t, handler, email, firstHolder)
	second := s.workspaceSignIn(t, handler, email, secondHolder)
	box := ciphertokentest.NewCaller(t)
	broker.holds(&fakeCredential{id: "signin-second", person: email, subject: secondHolder, signIn: true, tokens: []string{"token-of-the-second-holders-sign-in"}}, box)
	background.RevokeBrokerCredentials(ctx)
	first := s.subjectRevoke(t, email, firstHolder, secondHolder)
	requireCalls(t, broker, "after the pass that follows the second holder's sign-in", first)
	if !broker.refuses(email, firstHolder) {
		t.Fatalf("after the first holder's revocation the broker does not refuse the first holder at %s", email)
	}

	s.pool.UpdateGrants(func(claims map[string]any) { delete(claims, "hd") })
	ageConfirmation(t, s.people, email)
	if whoami := requestWith(handler, http.MethodGet, "/auth/whoami", second); whoami.Code != http.StatusOK || !strings.Contains(whoami.Body.String(), `"member_kind":"restricted"`) {
		t.Fatalf("the second holder's whoami that ran the hourly check, its account out of the Workspace: %d %s, want 200 as a restricted member", whoami.Code, whoami.Body.String())
	}
	if kind := s.memberKind(t, email); kind != "restricted" {
		t.Fatalf("people.member_kind of %s after the hourly check = %q, want restricted", email, kind)
	}

	s.workspaceSignIn(t, handler, email, firstHolder)
	s.ageBrokerRevocation(t, email)
	background.RevokeBrokerCredentials(ctx)
	if broker.refuses(email, firstHolder) {
		t.Errorf("the broker still refuses the first holder at %s, which it holds again: no revoke named it as the holder", email)
	}
	if !broker.refuses(email, secondHolder) {
		t.Errorf("the broker does not refuse the second holder at %s, which it no longer holds", email)
	}
	if broker.live("signin-second") {
		t.Errorf("the second holder's sign-in at the broker, made while it was full, outlived the first holder's return")
	}
	requireCalls(t, broker, "after the pass that follows the first holder's return", first, s.subjectRevoke(t, email, secondHolder, firstHolder))
	if recorded, done := s.brokerRevocation(t, email); !recorded || !done {
		t.Fatalf("after the passes the revocations of %s read recorded %t, done %t; want both settled", email, recorded, done)
	}
}

// tokenRowsNamed is how many personal token rows are named name.
func (s *outsideSignIn) tokenRowsNamed(t *testing.T, name string) int {
	t.Helper()
	var rows int
	if err := s.database.Pool.QueryRow(context.Background(), `select count(*) from agent_tokens where name = $1`, name).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	return rows
}

// requireBrokerCredentialRevoked requires recorder to be the 401 BROKER_TOKEN_NOT_ACTIVE a broker
// token whose credential is revoked, or owed a revocation, is answered.
func requireBrokerCredentialRevoked(t *testing.T, what string, recorder *httptest.ResponseRecorder) {
	t.Helper()
	if recorder.Code != http.StatusUnauthorized || errorCode(recorder) != "BROKER_TOKEN_NOT_ACTIVE" {
		t.Errorf("%s answered %d %s, want 401 BROKER_TOKEN_NOT_ACTIVE", what, recorder.Code, recorder.Body.String())
	}
}

// requireBrokerTokenRecordsNoAccount requires recorder to be the refusal of a broker token whose
// credential records no account and was made before people recorded the address's account, or
// within brokerCredentialMargin after, or at a time the broker did not record: 401
// BROKER_TOKEN_RECORDS_NO_ACCOUNT, never that its credentials are revoked.
func requireBrokerTokenRecordsNoAccount(t *testing.T, what string, recorder *httptest.ResponseRecorder) {
	t.Helper()
	if recorder.Code != http.StatusUnauthorized || errorCode(recorder) != "BROKER_TOKEN_RECORDS_NO_ACCOUNT" ||
		strings.Contains(recorder.Body.String(), "issued for the address is revoked") {
		t.Errorf("%s answered %d %s, want 401 BROKER_TOKEN_RECORDS_NO_ACCOUNT", what, recorder.Code, recorder.Body.String())
	}
}

// From the moment the second holder's sign-in commits, the address's broker tokens are held by
// the account each records, whether or not the broker has paid the revocation: the first holder's,
// whose account people no longer records, is refused 401 BROKER_TOKEN_NOT_ACTIVE, at a read and at
// a personal token's mint, which writes no row, before the pass, after it, an hour after the broker
// confirmed, and still once the affirmative read a process keeps for ciphertoken.AffirmativeCacheTTL
// has passed and the broker, which ended it, is asked again; the second holder's own, recording the
// account people records now, reads throughout and mints nothing (403 OAUTH_SCOPE_REQUIRED: no
// broker token holds approve); and a token recording no account whose credential was made before
// the recording, which might be either's, is refused 401 BROKER_TOKEN_RECORDS_NO_ACCOUNT
// throughout, never admitted again once the revocation settles. Before the address passed on, the
// first holder's token read, and minted nothing. The pass ends the unnamed credential that existed
// before it, as it ends every row of the address recording no account, so after the pass the test
// asks with one the broker still holds live (made before the recording, which an older broker, or
// a revoke the broker answered before it saw the row, would leave): Dispatch refuses it by its
// creation time alone. A credential recording no account that the broker made one minute after the
// recording is refused too (inside brokerCredentialMargin), and one made ten minutes after acts.
func TestOutsideSignInReassignHoldsEachBrokerTokenByItsAccount(t *testing.T) {
	ctx := context.Background()
	s := newOutsideSignIn(t, nil)
	broker := newFakeBroker(t)
	handler, background := s.brokerRouter(t, broker)
	s.workspaceSignIn(t, handler, reassignedEmail, firstHolder)
	machine := ciphertokentest.NewCaller(t)
	const replacedToken = "token-of-the-first-holders-machine"
	broker.holds(&fakeCredential{id: "login-first", person: reassignedEmail, subject: firstHolder, tokens: []string{replacedToken}}, machine)
	mint := func(caller *ciphertokentest.Caller, token, name string) *httptest.ResponseRecorder {
		return brokerTokenRequest(t, handler, caller, http.MethodPost, "/api/v1/me/agent-tokens", `{"name":"`+name+`"}`, token)
	}
	requireTokenActs(t, "GET /api/v1/whoami with the first holder's broker token, before the address passes on",
		brokerTokenRequest(t, handler, machine, http.MethodGet, "/api/v1/whoami", "", replacedToken))
	requireNoApproveScope(t, "the first holder's broker token's mint, before the address passes on",
		mint(machine, replacedToken, "minted before the address passed on"))
	unnamedMachine := ciphertokentest.NewCaller(t)
	unnamedToken := "token-of-a-machine-recording-no-account"
	broker.holds(&fakeCredential{id: "login-unnamed", person: reassignedEmail, tokens: []string{unnamedToken}, createdAt: time.Now().Add(-time.Hour)}, unnamedMachine)

	s.workspaceSignIn(t, handler, reassignedEmail, secondHolder)
	var recordedAt time.Time
	if err := s.database.Pool.QueryRow(ctx, `select sub_recorded_at from people where email = $1`, reassignedEmail).Scan(&recordedAt); err != nil {
		t.Fatalf("read when the second holder's sign-in recorded its account: %v", err)
	}
	holderMachine := ciphertokentest.NewCaller(t)
	const holderToken = "token-of-the-second-holders-machine"
	broker.holds(&fakeCredential{id: "login-second", person: reassignedEmail, subject: secondHolder, tokens: []string{holderToken}}, holderMachine)
	phase := func(when string) {
		t.Helper()
		for _, caller := range []struct {
			who, token string
			machine    *ciphertokentest.Caller
			require    func(*testing.T, string, *httptest.ResponseRecorder)
		}{
			{"the first holder's", replacedToken, machine, requireBrokerCredentialRevoked},
			{"an unnamed", unnamedToken, unnamedMachine, requireBrokerTokenRecordsNoAccount},
		} {
			caller.require(t, "GET /api/v1/whoami with "+caller.who+" broker token, "+when,
				brokerTokenRequest(t, handler, caller.machine, http.MethodGet, "/api/v1/whoami", "", caller.token))
			name := "minted by " + caller.who + " broker token " + when
			caller.require(t, "POST /api/v1/me/agent-tokens with "+caller.who+" broker token, "+when, mint(caller.machine, caller.token, name))
			if rows := s.tokenRowsNamed(t, name); rows != 0 {
				t.Errorf("a refused mint by %s broker token, %s, wrote %d token rows, want none", caller.who, when, rows)
			}
		}
		requireTokenActs(t, "GET /api/v1/whoami with the second holder's broker token, "+when,
			brokerTokenRequest(t, handler, holderMachine, http.MethodGet, "/api/v1/whoami", "", holderToken))
		requireNoApproveScope(t, "the second holder's broker token's mint, "+when, mint(holderMachine, holderToken, "the second holder's own, "+when))
		if rows := s.tokenRowsNamed(t, "the second holder's own, "+when); rows != 0 {
			t.Errorf("the second holder's refused mint, %s, wrote %d token rows, want none", when, rows)
		}
	}
	phase("before the revocation pass")
	background.RevokeBrokerCredentials(ctx)
	if recorded, done := s.brokerRevocation(t, reassignedEmail); !recorded || !done {
		t.Fatalf("the revocation pass left the revocation of %s recorded %t, done %t; want it settled", reassignedEmail, recorded, done)
	}
	if broker.live("login-unnamed") || broker.live("login-first") {
		t.Fatal("the broker still holds a credential of the address recording no account, or the first holder's, after the revocation")
	}
	unnamedToken = "token-of-a-machine-recording-no-account-the-revocation-missed"
	broker.holds(&fakeCredential{id: "login-unnamed-missed", person: reassignedEmail, tokens: []string{unnamedToken}, createdAt: recordedAt.Add(-time.Hour)}, unnamedMachine)
	s.settledAgo(t, reassignedEmail, time.Hour)
	time.Sleep(ciphertoken.AffirmativeCacheTTL + 100*time.Millisecond)
	phase("an hour after the broker confirmed, past the process's affirmative read")

	insideMachine, laterMachine := ciphertokentest.NewCaller(t), ciphertokentest.NewCaller(t)
	const insideToken, laterToken = "token-made-a-minute-after-the-recording", "token-made-ten-minutes-after-the-recording"
	broker.holds(&fakeCredential{id: "login-inside-the-margin", person: reassignedEmail, tokens: []string{insideToken}, createdAt: recordedAt.Add(time.Minute)}, insideMachine)
	broker.holds(&fakeCredential{id: "login-after-the-margin", person: reassignedEmail, tokens: []string{laterToken}, createdAt: recordedAt.Add(10 * time.Minute)}, laterMachine)
	requireBrokerTokenRecordsNoAccount(t, "GET /api/v1/whoami with a broker token recording no account made a minute after the recording",
		brokerTokenRequest(t, handler, insideMachine, http.MethodGet, "/api/v1/whoami", "", insideToken))
	requireTokenActs(t, "GET /api/v1/whoami with a broker token recording no account made ten minutes after the recording",
		brokerTokenRequest(t, handler, laterMachine, http.MethodGet, "/api/v1/whoami", "", laterToken))
	requireNoApproveScope(t, "the mint by a broker token recording no account made ten minutes after the recording",
		mint(laterMachine, laterToken, "minted ten minutes after the recording"))
}

// A revocation names the replaced account, not a moment: the second holder signs in at the broker
// on a box two seconds after taking the address, before the revocation pass runs - inside the ten
// seconds an earlier Dispatch revoked everything the broker listed in, as the replaced account's.
// The pass names the first holder alone, the broker ends only the first holder's login, the
// second holder's sign-in is never named, and once the refusal by address is over, that sign-in's
// broker token acts. The broker's per-credential lists and revokes, which that earlier pass made,
// answer 404 here: a pass that made them would fail the call list this test requires.
func TestOutsideSignInReassignKeepsTheNewHoldersSignInMadeAfterTheReplacement(t *testing.T) {
	ctx := context.Background()
	s := newOutsideSignIn(t, nil)
	broker := newFakeBroker(t)
	handler, background := s.brokerRouter(t, broker)
	s.workspaceSignIn(t, handler, reassignedEmail, firstHolder)
	broker.holds(&fakeCredential{id: "login-first", person: reassignedEmail, subject: firstHolder, tokens: []string{"token-of-the-first-holders-machine"}}, ciphertokentest.NewCaller(t))
	s.workspaceSignIn(t, handler, reassignedEmail, secondHolder)
	time.Sleep(2 * time.Second)
	box := ciphertokentest.NewCaller(t)
	const boxToken = "token-of-the-second-holders-sign-in"
	broker.holds(&fakeCredential{id: "signin-second", person: reassignedEmail, subject: secondHolder, signIn: true, tokens: []string{boxToken}}, box)

	background.RevokeBrokerCredentials(ctx)
	requireCalls(t, broker, "after the revocation pass", s.subjectRevoke(t, reassignedEmail, firstHolder, secondHolder))
	if recorded, done := s.brokerRevocation(t, reassignedEmail); !recorded || !done {
		t.Fatalf("after the pass the revocation of %s reads recorded %t, done %t; want it settled", reassignedEmail, recorded, done)
	}
	if broker.live("login-first") || !broker.live("signin-second") {
		t.Fatalf("after the revocation, login-first live %t and signin-second live %t; want the first holder's login ended and the second holder's sign-in kept", broker.live("login-first"), broker.live("signin-second"))
	}
	s.settledAgo(t, reassignedEmail, time.Minute)
	requireTokenActs(t, "GET /api/v1/whoami with the second holder's sign-in's broker token, past the refusal",
		brokerTokenRequest(t, handler, box, http.MethodGet, "/api/v1/whoami", "", boxToken))
}

// A deployment with no secrets broker configured records no broker revocation when a full member's
// address passes to another account: no pass of it would pay one. The router is the one main serves
// for a boot with no DISPATCH_AGENT_SECRETS_URL, its people store built with the options main gives
// that boot.
func TestOutsideSignInReassignWithNoBrokerRecordsNoBrokerRevocation(t *testing.T) {
	s := newOutsideSignIn(t, nil)
	if s.boot.AgentSecretsURL != "" {
		t.Fatalf("the harness boot names a broker, %q", s.boot.AgentSecretsURL)
	}
	handler := s.routerWithSessions(t)
	s.workspaceSignIn(t, handler, reassignedEmail, firstHolder)
	s.workspaceSignIn(t, handler, reassignedEmail, secondHolder)
	if recorded, _ := s.brokerRevocation(t, reassignedEmail); recorded {
		t.Fatalf("a deployment with no broker recorded a broker revocation of %s when another account took it, want none", reassignedEmail)
	}
}

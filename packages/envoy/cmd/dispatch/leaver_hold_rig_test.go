package main

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/sjawhar/envoy/internal/cipher/ciphertest"
	brokerstoretest "github.com/sjawhar/envoy/internal/cipher/store/storetest"
	"github.com/sjawhar/envoy/internal/cipherclient"
	"github.com/sjawhar/envoy/internal/dispatch/api"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/identity/directorytest"
	"github.com/sjawhar/envoy/internal/dispatch/routes"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// The leaver rig: the real Dispatch router and the real secrets broker (ciphertest.NewRig), each
// on Postgres, wired to each other as a deployment wires them, for the tests of a hold. Dispatch
// signs people in with Google, the fake Google (signInPool) standing in for it, under
// DISPATCH_SIGNIN_DOMAIN=example.com, which signs in the Workspace domain's people alone, keeps
// its sessions in its database (store.PgSessionStore), relays machine-login decisions to the
// broker and checks broker-issued tokens against it (DISPATCH_AGENT_SECRETS_URL), and answers the
// broker's own service account on its person route (DISPATCH_ACCESS_ACCOUNTS). A person the rig
// signs in holds the credentials of the measurement table a leaver's hold is read on (person). A
// rig built withIAP runs Dispatch as an Identity-Aware Proxy deployment runs it instead (R9).

const (
	// leaverDomain is the Workspace domain DISPATCH_SIGNIN_DOMAIN names: an address on it signs in
	// as a full member.
	leaverDomain = "example.com"
	// leaverProject is a project of privOwner's, as newOutsideSignIn's PRIV is.
	leaverProject = "PRIV"
	// leaverBrokerAccount is the secrets broker's Kubernetes service account, the one account
	// DISPATCH_OIDC_ACCOUNTS admits and DISPATCH_ACCESS_ACCOUNTS lets read the person answer, and
	// leaverAccountAudience is DISPATCH_OIDC_AUDIENCE, the audience its projected token names.
	leaverBrokerAccount   = "system:serviceaccount:dispatch:envoy-broker"
	leaverAccountAudience = "dispatch"
	// leaverHumanSecret is the broker rig's human-tier secret, which its operator, ada, owns: a
	// session's ask for it is an approval request to her (ciphertest.NewRig).
	leaverHumanSecret = "TEST_SECRET"
	// leaverMachineAccount is the fake Google service account for a person's broker sign-in.
	leaverMachineAccount = "leaver-machine@example.iam.gserviceaccount.com"
)

// The people of the tests: ada, a full member whom the fake Google refuses; bob, a full member
// whom it confirms; carol, a full member who holds only a machine login. Google signs in no
// restricted member: a person from outside signs in with a passkey, which Google never refuses.
const (
	adaEmail   = "ada@example.com"
	bobEmail   = "bob@example.com"
	carolEmail = "carol@example.com"
)

// leaverRig is one Dispatch and one secrets broker.
type leaverRig struct {
	// google is the fake Google every sign-in and refresh of Dispatch's goes to. googleKey signs
	// the metadata and person identity tokens the broker's person-trade configuration accepts.
	google    *oidctest.Issuer
	googleKey *oidctest.Key
	// accounts issues the projected service-account tokens Dispatch admits (DISPATCH_OIDC_ISSUER),
	// signing them with accountKey.
	accounts   *oidctest.Issuer
	accountKey *oidctest.Key
	// broker is the secrets broker, and client a machine's client of it.
	broker *ciphertest.Rig
	client *ciphertest.Client
	// brokerStopped, while set, has the server DISPATCH_AGENT_SECRETS_URL names, through which each
	// of Dispatch's calls reaches the broker, cut each call unanswered, as a stopped broker leaves
	// it, and count it in cutCalls.
	brokerStopped atomic.Bool
	cutCalls      atomic.Int64
	// brokerDispatch is the server the broker's project-access client reaches when the fixture
	// enables that client. It has a URL before the broker starts and its handler once Dispatch does.
	brokerDispatch *httptest.Server
	// brokerAhead is how far, in nanoseconds, the clock of the broker's project-access client and
	// hold gate runs ahead of the wall clock (brokerNow): advanceBrokerClock moves it on.
	brokerAhead atomic.Int64
	// outside holds Dispatch's database, its people store and the boot the router serves, with
	// PRIV and its access list, as newOutsideSignIn builds them.
	outside *outsideSignIn
	// app is the app context the router serves, handler the router, and background its
	// background work, whose revocation pass a test runs as the server's loop does. membership is
	// the router's identity.Membership, which its Standing asks and a membership pass a test runs
	// asks too, as main hands both the one Membership.
	app        *routes.AppContext
	handler    http.Handler
	background api.Background
	membership *identity.Membership
	// persons is each person the rig signed in, by email.
	persons map[string]*leaverPerson
	// On a rig built withIAP, iapKeys is the key set served in Identity-Aware Proxy's place, which
	// Dispatch verifies each browser's assertion against, iapKey the key it signs them with
	// (iapAssertion), and directory the Google hosts Dispatch's Workspace Directory reader reaches
	// (directorytest), which answer each person's account as the test sets it. All three are nil
	// on any other rig.
	iapKeys   *oidctest.Issuer
	iapKey    *oidctest.Key
	directory *directorytest.Google
	// metadataHost is the host of the rig's metadata machine on a rig built withPersonSignIns
	// (startLeaverMetadataServer), and "" on any other: what googleidentity.MetadataHostVar names
	// to a client of that machine, passed to each explicitly. machineToken is the identity token
	// that machine answers for the broker's machine audience, which a sign-in trade presents as
	// its actor (signInTrade).
	metadataHost string
	machineToken string
}

// leaverOption changes how newLeaverRig builds its rig, by the hooks it adds to the config.
type leaverOption func(*leaverConfig)

// leaverConfig is the hooks newLeaverRig runs, each kind in the order the options added them.
type leaverConfig struct {
	// broker hooks run once the fake Google and the service-account issuer exist, before the
	// broker is built, and give ciphertest.NewRig its options.
	broker []func(t *testing.T, r *leaverRig) []ciphertest.Option
	// toBroker hooks wrap the handler that carries each of Dispatch's calls to the broker, the
	// first given innermost.
	toBroker []func(t *testing.T, r *leaverRig, next http.Handler) http.Handler
	// built hooks run once the router is built and r.handler serves it.
	built []func(t *testing.T, r *leaverRig)
	// iap is withIAP: Dispatch runs under DISPATCH_IDENTITY=iap.
	iap bool
	// refreshFlow is withRefreshFlowOfItsOwn: Dispatch's Membership refreshes through a code flow
	// of its own.
	refreshFlow bool
}

// withIAP runs Dispatch as an Identity-Aware Proxy deployment runs it (R9): under
// DISPATCH_IDENTITY=iap for DISPATCH_SIGNIN_DOMAIN=example.com, with no sign-in of its own and no
// refresh token kept, naming each browser's person from the assertion IAP signs for each request
// (iapAssertion) against a key set the rig serves in IAP's place, and checking each full member's
// Workspace account through the Directory reader the boot builds from DISPATCH_DIRECTORY_ACCOUNT
// and DISPATCH_DIRECTORY_ADMIN, against Google hosts the rig serves (r.directory). The broker is an
// IAP deployment's broker, whose BROKER_HOLD_REQUIRES_DEADLINE sets the gate's deadline rule
// (ciphertest.WithHoldRequiresDeadline), so it needs withBrokerGate, whose gate that rule rules:
// without it the broker rig refuses to build.
func withIAP() leaverOption {
	return func(c *leaverConfig) {
		c.iap = true
		c.broker = append(c.broker, func(*testing.T, *leaverRig) []ciphertest.Option {
			return []ciphertest.Option{ciphertest.WithHoldRequiresDeadline()}
		})
	}
}

// withRefreshFlowOfItsOwn has Dispatch's Membership, which Standing and the membership pass ask,
// refresh through a code flow of its own (signInFlow), not the one the sign-in callback exchanges
// codes through: the code flow of a Dispatch process started after the person signed in, which has
// made no grant. A code flow left to x/oauth2's AuthStyleAutoDetect (oidc.tokenAuthStyle) keeps
// the auth style its first successful grant found, so a refresh through the flow the rig signed
// the person in through makes one token request where that process makes two: a refused refresh
// there asks by HTTP Basic and again in the form.
func withRefreshFlowOfItsOwn() leaverOption {
	return func(c *leaverConfig) { c.refreshFlow = true }
}

// withPersonPod has the broker map the Kubernetes service account account, logging in as service,
// to person in BROKER_POD_OPERATORS (ciphertest.WithPersonPod): a pod of the account logs in as
// person at any time with its own token. r.broker.SetPodOperator changes or drops the mapping.
func withPersonPod(account, service, person string) leaverOption {
	return func(c *leaverConfig) {
		c.broker = append(c.broker, func(*testing.T, *leaverRig) []ciphertest.Option {
			return []ciphertest.Option{ciphertest.WithPersonPod(account, service, person)}
		})
	}
}

// withMembershipPass runs Dispatch's maintenance sweep as main runs it under a Google rule
// (api.RunMaintenanceSweep, every api.MaintenanceSweepInterval, under its lease), from the moment
// the router is built until the test ends, with sweeps in place of its delivery sweep, so a test
// sets how long a run's other sweeps take, and with the membership pass last, the rig's own
// (membershipPass), told of each claim attempt through observe. The first run comes one interval
// after the router is built, as in a process that has just started.
func withMembershipPass(sweeps func(context.Context) error, observe func(identity.MembershipPassEvent)) leaverOption {
	return func(c *leaverConfig) {
		c.built = append(c.built, func(t *testing.T, r *leaverRig) {
			leases := storetest.Leases(t, r.outside.database.Pool)
			ctx, cancel := context.WithCancel(context.Background())
			stopped := make(chan struct{})
			go func() {
				defer close(stopped)
				api.RunMaintenanceSweep(ctx, r.outside.database.Pool, api.MaintenanceSweepInterval, leases, sweeps,
					r.outside.boot.PrivateProjects, r.outside.boot.PrivateBeforeFullSwitch, r.membershipPass(observe).Run)
			}()
			// Stopped before the databases are dropped, which cleanups registered before this one do.
			t.Cleanup(func() {
				cancel()
				<-stopped
			})
		})
	}
}

// withPersonAccessDelay makes each of Dispatch's GET /v1/person-access asks of the broker, which a
// sign-out and the membership pass's check of a signed-out row make, wait the nanoseconds d holds
// before it is carried to the broker, or until Dispatch hangs up, when it is carried nowhere.
func withPersonAccessDelay(d *atomic.Int64) leaverOption {
	return func(c *leaverConfig) {
		c.toBroker = append(c.toBroker, func(_ *testing.T, _ *leaverRig, next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if wait := time.Duration(d.Load()); wait > 0 && req.Method == http.MethodGet && req.URL.Path == "/v1/person-access" {
					timer := time.NewTimer(wait)
					defer timer.Stop()
					select {
					case <-timer.C:
					case <-req.Context().Done():
						return
					}
				}
				next.ServeHTTP(w, req)
			})
		})
	}
}

// leaverPerson is one person's credentials.
type leaverPerson struct {
	email string
	// signedIn is the answer of the person's last sign-in callback: its session cookie is the one
	// their browser holds.
	signedIn *httptest.ResponseRecorder
	// dsp is the person's personal token.
	dsp leaverToken
	// machineKey holds the machine login credential, whose OAuth token pair is machineToken.
	machineKey   *ecdsa.PrivateKey
	machineToken *oauth2.Token
	credential   string
	// signInKey and signInCredential belong to the Google person trade on the fixture's metadata
	// machine, so the release test can age and re-trade a real sign-in credential.
	signInKey        *ecdsa.PrivateKey
	signInCredential string
	// enrollment is the session enrolled under the machine login, signing with sessionKey.
	enrollment string
	sessionKey *ecdsa.PrivateKey
	// enrolments counts the sessions enrol asked for, so each names a runtime of its own.
	enrolments int
	// agentSecret is the agent-tier secret the person owns, which their own sessions get at once;
	// agentGrant is the setup session's grant of it, and humanGrant its grant of
	// leaverHumanSecret, which ada approved.
	agentSecret string
	agentGrant  string
	humanGrant  string
	// brokerToken is a Dispatch access token the broker minted from the machine login's token
	// (RFC 8693), bound to machineKey.
	brokerToken string
}

// leaverToken is one personal token: its id and its plaintext.
type leaverToken struct {
	id, plaintext string
}

// newLeaverRig builds the rig over fresh databases on DISPATCH_TEST_DATABASE_URL and
// BROKER_TEST_DATABASE_URL. It fails t when either is unset: a skipped run is no evidence.
func newLeaverRig(t *testing.T, opts ...leaverOption) *leaverRig {
	t.Helper()
	for _, name := range []string{storetest.DatabaseURLEnv, brokerstoretest.DatabaseURLEnv} {
		if os.Getenv(name) == "" {
			t.Fatalf("%s is unset: the leaver rig runs the real Dispatch and the real broker on Postgres, and refuses to skip without it", name)
		}
	}
	var config leaverConfig
	for _, opt := range opts {
		opt(&config)
	}
	r := &leaverRig{google: signInPool(t, "dispatch-client", "client-secret"), accounts: oidctest.New(t), persons: map[string]*leaverPerson{}}
	r.googleKey = r.google.PublishKey(t, "broker-person-sign-in-key")
	r.accountKey = r.accounts.PublishKey(t, "account-key")

	var brokerOptions []ciphertest.Option
	for _, hook := range config.broker {
		brokerOptions = append(brokerOptions, hook(t, r)...)
	}
	r.broker = ciphertest.NewRig(t, brokerOptions...)
	r.client = ciphertest.NewClient(t, r.broker.URL, strings.TrimPrefix(r.broker.URL, "http://"))
	r.client.UIToken, r.client.DPoP = r.broker.UIToken, cipherclient.NewDPoPTransport

	target, err := url.Parse(r.broker.URL)
	if err != nil {
		t.Fatal(err)
	}
	var toBroker http.Handler = httputil.NewSingleHostReverseProxy(target)
	for _, hook := range config.toBroker {
		toBroker = hook(t, r, toBroker)
	}
	brokerProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if r.brokerStopped.Load() {
			r.cutCalls.Add(1)
			panic(http.ErrAbortHandler)
		}
		toBroker.ServeHTTP(w, req)
	}))
	t.Cleanup(brokerProxy.Close)

	// Dispatch presents the broker rig's own Dispatch account token, as it presents its projected
	// token in a deployment.
	accountFile := filepath.Join(t.TempDir(), "account-token")
	if err := os.WriteFile(accountFile, []byte(r.broker.UIToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	settings := map[string]string{
		"DISPATCH_OIDC_ISSUER":                      r.accounts.URL(),
		"DISPATCH_OIDC_AUDIENCE":                    leaverAccountAudience,
		"DISPATCH_OIDC_ACCOUNTS":                    leaverBrokerAccount,
		"DISPATCH_ACCESS_ACCOUNTS":                  leaverBrokerAccount,
		"DISPATCH_AGENT_SECRETS_URL":                brokerProxy.URL,
		"DISPATCH_AGENT_SECRETS_ACCOUNT_TOKEN_FILE": accountFile,
	}
	var boot bootConfig
	if config.iap {
		// IAP signs people in for leaverDomain (iapIdentityEnvironment's DISPATCH_SIGNIN_DOMAIN), and
		// the boot reads IAP's keys and reaches Google's Directory hosts where no setting points it:
		// here, the key set and the fake hosts the rig serves.
		r.iapKeys, r.iapKey = iapKeySet(t)
		r.directory = directorytest.ServeGoogle(t)
		boot = iapBoot(t, r.iapKeys, r.directory, settings)
	} else {
		maps.Copy(settings, map[string]string{
			"DISPATCH_SIGNIN_ISSUER": "https://accounts.google.com",
			"DISPATCH_SIGNIN_GROUP":  "",
			"DISPATCH_SIGNIN_DOMAIN": leaverDomain,
		})
		boot = resolveWith(t, settings)
		// Resolved under Google's issuer, then pointed at the fake one, so no test calls Google.
		boot.SignInIssuer = r.google.URL()
	}

	database := storetest.Open(t)
	r.outside = &outsideSignIn{pool: r.google, database: database, boot: boot}
	r.outside.people = testPeople{PgPeopleStore: store.NewPgPeopleStore(database.Pool, testSigningKey, nil, peopleOptions(boot)...), pool: database.Pool}
	if _, err := database.Pool.Exec(context.Background(), `insert into projects (key, name, default_assignee) values ($1, 'Private', $2)`, leaverProject, privOwner); err != nil {
		t.Fatalf("seed %s: %v", leaverProject, err)
	}

	brokerTokens, err := brokerTokenVerifier(boot, database, brokerOrigin)
	if err != nil {
		t.Fatalf("build the broker-token check: %v", err)
	}
	serviceTokens := serviceTokenVerifier(boot)
	if err := serviceTokens.Discover(t.Context()); err != nil {
		t.Fatalf("discover the service-account token issuer: %v", err)
	}
	built := routes.AppContextOptions{
		Lifetime: t.Context(), SigningKey: testSigningKey, SignIn: signInFlow(boot), People: r.outside.people,
		Store: database, Sessions: store.NewPgSessionStore(database.Pool), BrokerTokens: brokerTokens, OIDC: serviceTokens,
	}
	rule, err := signInRule(boot, accessLister(built.Store))
	if err != nil {
		t.Fatal(err)
	}
	refresh := built.SignIn
	if config.refreshFlow {
		refresh = signInFlow(boot)
	}
	built.Identity, r.membership, err = requestIdentityFor(t.Context(), boot, built.SigningKey, built.People, built.Sessions, refresh, rule)
	if err != nil {
		t.Fatalf("build the request identity: %v", err)
	}
	r.app, err = buildAppContext(boot, rule, withDocumentService(t, boot, built), r.membership)
	if err != nil {
		t.Fatalf("build the router: %v", err)
	}
	r.handler, r.background = routes.New(r.app)
	t.Cleanup(r.background.Wait)
	r.outside.handler = r.handler

	// The broker's account reads the person answer, which a broker pointed at this Dispatch asks:
	// a rig whose service-account settings did not take fails here, saying so.
	asked := httptest.NewRequest(http.MethodGet, brokerOrigin+"/api/v1/access/person?person="+url.QueryEscape(privOwner), nil)
	asked.Header.Set("Authorization", "Bearer "+r.brokerAccountToken(t))
	answered := httptest.NewRecorder()
	r.handler.ServeHTTP(answered, asked)
	if answered.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/access/person as the broker's account %s answered %d %s, want 200", leaverBrokerAccount, answered.Code, answered.Body.String())
	}

	for _, hook := range config.built {
		hook(t, r)
	}
	return r
}

// brokerAccountToken is a projected token of the broker's service account for Dispatch's
// audience, good for an hour: the bearer the broker presents on Dispatch's person route.
func (r *leaverRig) brokerAccountToken(t *testing.T) string {
	t.Helper()
	return r.accounts.Mint(t, r.accountKey, r.accounts.Claims(leaverBrokerAccount, leaverAccountAudience))
}

// person signs email in and gives them every credential of the measurement table: a browser
// credential (a cookie, or on a rig built withIAP the assertion IAP signs for each request), a
// dsp_ token, a machine login approved through Dispatch, a session enrolled under it, an
// agent-tier grant and a human-tier grant, and a broker token minted from the machine login.
func (r *leaverRig) person(t *testing.T, email string) *leaverPerson {
	t.Helper()
	p := r.signIn(t, email)
	p.dsp = r.mintToken(t, p, "leaver hold")
	r.machineLogin(t, p)
	r.brokerCredentials(t, p)
	return p
}

// signIn signs email in to Dispatch through the fake Google as the Google account
// "google-<email>", and records the person, whose browser then holds the callback's cookie. On a
// rig built withIAP, Dispatch runs no sign-in of its own: the person's first request through IAP,
// GET /auth/whoami carrying the assertion IAP signed for them, records them, as IAP's sign-in
// does, and each later request of their browser carries a fresh assertion (browser).
func (r *leaverRig) signIn(t *testing.T, email string) *leaverPerson {
	t.Helper()
	p := &leaverPerson{email: email}
	if r.iapKeys == nil {
		p.signedIn = r.signInAs(t, email, "google-"+email)
	} else if admitted := r.browser(t, p, http.MethodGet, "/auth/whoami", ""); admitted.Code != http.StatusOK {
		t.Fatalf("GET /auth/whoami with the assertion IAP signed for %s answered %d %s, want 200: IAP admits them", email, admitted.Code, admitted.Body.String())
	}
	r.persons[email] = p
	return p
}

// signInAgain signs a person the rig signed in before in again through the fake Google, as the
// same Google account, and their browser holds the new cookie. It changes nothing the fake Google
// refuses (refuse).
func (r *leaverRig) signInAgain(t *testing.T, email string) {
	t.Helper()
	p := r.persons[email]
	if p == nil {
		t.Fatalf("the rig never signed %s in, so there is no sign-in to make again", email)
	}
	if r.iapKeys != nil {
		t.Fatalf("a rig built withIAP runs no sign-in of its own for %s to make again: IAP signs people in, and only the Workspace Directory's active answer ends a hold", email)
	}
	p.signedIn = r.signInAs(t, email, "google-"+email)
}

// signInAs runs a whole sign-in to Dispatch as the Google account subject at email, a Workspace
// account (hd example.com) at an address on leaverDomain and an account with no Workspace
// elsewhere, and requires it to be signed in.
func (r *leaverRig) signInAs(t *testing.T, email, subject string) *httptest.ResponseRecorder {
	t.Helper()
	claims := map[string]any{"sub": subject, "email": email, "email_verified": true}
	if strings.HasSuffix(email, "@"+leaverDomain) {
		claims["hd"] = leaverDomain
	}
	signedIn := codeFlowSignIn(t, r.handler, r.google, claims)
	if signedIn.Code != http.StatusFound || !sessionCookieSet(signedIn) {
		t.Fatalf("the sign-in of the Google account %s at %s: the callback answered %d %s, want a signed-in redirect", subject, email, signedIn.Code, signedIn.Body.String())
	}
	return signedIn
}

// browser answers method path from Dispatch with body, sent from p's browser as the dashboard's
// own script sends it (cookieRequest): carrying the session cookie of p's last sign-in, or, on a
// rig built withIAP, the fresh assertion IAP signs for each request it lets through
// (iapAssertion).
func (r *leaverRig) browser(t *testing.T, p *leaverPerson, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	if r.iapKeys == nil {
		return cookieRequest(r.handler, method, path, body, p.signedIn)
	}
	recorder := httptest.NewRecorder()
	r.handler.ServeHTTP(recorder, r.iapRequest(t, p, method, path, strings.NewReader(body)))
	return recorder
}

// iapRequest is method path with body as it leaves p's browser through IAP, on a rig built
// withIAP: the dashboard's own headers and the fresh assertion IAP signs for the request
// (iapAssertion). A body of a kind httptest.NewRequest cannot measure sends no length.
func (r *leaverRig) iapRequest(t *testing.T, p *leaverPerson, method, path string, body io.Reader) *http.Request {
	t.Helper()
	request := httptest.NewRequest(method, "http://dispatch.test"+path, body)
	request.Header.Set("Origin", "http://dispatch.test")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(identity.IAPAssertionHeader, r.iapAssertion(t, p.email))
	return request
}

// iapAssertion is the assertion IAP signs, for the audience the IAP boot names (testIAPAudience),
// for the Google account of leaverDomain's Workspace at email, "google-<email>" as signIn names
// that account: the claims of main_test.go's iapAssertion, for the person the rig names.
func (r *leaverRig) iapAssertion(t *testing.T, email string) string {
	t.Helper()
	now := time.Now()
	return r.iapKeys.Mint(t, r.iapKey, map[string]any{
		"iss": oidc.IAPIssuer, "aud": testIAPAudience, "sub": "accounts.google.com:google-" + email,
		"email": email, "hd": leaverDomain, "iat": now.Add(-time.Minute).Unix(), "exp": now.Add(9 * time.Minute).Unix(),
	})
}

// signOut signs p out of Dispatch with their cookie, as the dashboard's Sign out does.
func (r *leaverRig) signOut(t *testing.T, p *leaverPerson) {
	t.Helper()
	if out := r.browser(t, p, http.MethodPost, "/auth/logout", ""); out.Code != http.StatusOK {
		t.Fatalf("POST /auth/logout on %s's cookie answered %d %s, want 200", p.email, out.Code, out.Body.String())
	}
}

// mintToken mints a dsp_ token named name on p's browser, as the agent tokens page does.
func (r *leaverRig) mintToken(t *testing.T, p *leaverPerson, name string) leaverToken {
	t.Helper()
	minted := r.browser(t, p, http.MethodPost, "/api/v1/me/agent-tokens", leaverBody(t, map[string]string{"name": name}))
	var token struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	if minted.Code != http.StatusCreated || json.Unmarshal(minted.Body.Bytes(), &token) != nil || token.ID == "" || !strings.HasPrefix(token.Token, "dsp_") {
		t.Fatalf("POST /api/v1/me/agent-tokens on %s's browser answered %d %s, want 201 with a token", p.email, minted.Code, minted.Body.String())
	}
	return leaverToken{id: token.ID, plaintext: token.Token}
}

// revokeToken revokes p's token on their browser, as the agent tokens page does.
func (r *leaverRig) revokeToken(t *testing.T, p *leaverPerson, token leaverToken) {
	t.Helper()
	if revoked := r.browser(t, p, http.MethodDelete, "/api/v1/me/agent-tokens/"+token.id, ""); revoked.Code != http.StatusNoContent {
		t.Fatalf("DELETE /api/v1/me/agent-tokens/%s on %s's browser answered %d %s, want 204", token.id, p.email, revoked.Code, revoked.Body.String())
	}
}

// refuse makes the fake Google answer each refresh of email's grants invalid_grant, as Google
// answers one for an account it no longer confirms.
func (r *leaverRig) refuse(email string) {
	r.google.RefuseRefresh(email, true)
}

// ageConfirmation dates email's last confirmation d back by the database's clock
// (people.confirmed_at = now() - d). A row that holds no confirmation, which is a row with no
// refresh token, has none to date, and the log says so.
func (r *leaverRig) ageConfirmation(t *testing.T, email string, d time.Duration) {
	t.Helper()
	aged, err := r.outside.database.Pool.Exec(context.Background(), `
		update people set confirmed_at = now() - make_interval(secs => $2) where email = $1 and confirmed_at is not null
	`, email, d.Seconds())
	if err != nil {
		t.Fatalf("date %s's confirmation %s back: %v", email, d, err)
	}
	if aged.RowsAffected() == 0 {
		t.Logf("no people row of %s holds a confirmation to date %s back: it holds no refresh token", email, d)
	}
}

// storesRefreshToken reports whether email's people row holds a refresh token.
func (r *leaverRig) storesRefreshToken(t *testing.T, email string) bool {
	t.Helper()
	var stored bool
	if err := r.outside.database.Pool.QueryRow(context.Background(), `select refresh_token is not null from people where email = $1`, email).Scan(&stored); err != nil {
		t.Fatalf("read %s's people row: %v", email, err)
	}
	return stored
}

// stopBroker cuts each of Dispatch's calls to the broker unanswered from now on, as a stopped
// broker leaves them, and startBroker carries them again.
func (r *leaverRig) stopBroker()  { r.brokerStopped.Store(true) }
func (r *leaverRig) startBroker() { r.brokerStopped.Store(false) }

// The credential calls. Each makes the one request a credential of the measurement table is used
// in and answers its status and, for a refusal, its code: the JSON code a Dispatch or broker
// route refuses with, or the OAuth error a token endpoint answers. A success answers no code.
// Dispatch's calls are below; the broker's are in leaver_hold_broker_rig_test.go.

// cookie is GET /api/v1/whoami from p's browser (browser): on their session cookie, as the
// dashboard's script sends it, or on a rig built withIAP with the assertion IAP signs for it.
func (r *leaverRig) cookie(t *testing.T, p *leaverPerson) (int, string) {
	t.Helper()
	return dispatchAnswer(r.browser(t, p, http.MethodGet, "/api/v1/whoami", ""))
}

// personalToken is GET /api/v1/whoami with token as the bearer, as an agent sends it.
func (r *leaverRig) personalToken(t *testing.T, token leaverToken) (int, string) {
	t.Helper()
	return dispatchAnswer(bearerRequest(r.handler, http.MethodGet, "/api/v1/whoami", "", token.plaintext))
}

// brokerTokenAtDispatch is GET /api/v1/whoami presenting p's broker token under the DPoP scheme,
// with a proof of the machine key the token is bound to, as an agent on p's machine sends it.
func (r *leaverRig) brokerTokenAtDispatch(t *testing.T, p *leaverPerson) (int, string) {
	t.Helper()
	capture := &leaverCapture{}
	signing, err := http.NewRequest(http.MethodGet, brokerOrigin+"/api/v1/whoami", nil)
	if err != nil {
		t.Fatal(err)
	}
	signing.Header.Set("Authorization", "DPoP "+p.brokerToken)
	response, err := (&http.Client{Transport: cipherclient.NewDPoPTransport(p.machineKey, capture)}).Do(signing)
	if err != nil {
		t.Fatalf("sign %s's broker token request: %v", p.email, err)
	}
	response.Body.Close()
	request := httptest.NewRequest(http.MethodGet, brokerOrigin+"/api/v1/whoami", nil)
	request.Header.Set("Authorization", capture.last.Header.Get("Authorization"))
	request.Header.Set("DPoP", capture.last.Header.Get("DPoP"))
	recorder := httptest.NewRecorder()
	r.handler.ServeHTTP(recorder, request)
	return dispatchAnswer(recorder)
}

// dispatchAnswer is a Dispatch answer as a credential call answers it, read as a broker answer is.
func dispatchAnswer(response *httptest.ResponseRecorder) (int, string) {
	return brokerAnswer(response.Code, response.Body.Bytes())
}

// brokerAnswer is a broker answer as a credential call answers it.
func brokerAnswer(status int, body []byte) (int, string) {
	if status < http.StatusMultipleChoices {
		return status, ""
	}
	var refusal struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(body, &refusal)
	return status, refusal.Code
}

// leaverWant is the answer a credential call must give: what was asked, named by the person and
// the credential, and the status and code (none for a success) it must answer.
type leaverWant struct {
	t      *testing.T
	what   string
	status int
	code   string
}

// expectAnswer is the answer what must give.
func expectAnswer(t *testing.T, what string, status int, code string) leaverWant {
	return leaverWant{t: t, what: what, status: status, code: code}
}

// got checks the answer a credential call gave: another fails the test, naming what was asked,
// the answer and the one wanted, and the one wanted is logged, so a run shows each row it read.
func (w leaverWant) got(status int, code string) {
	w.t.Helper()
	if status != w.status || code != w.code {
		w.t.Errorf("%s answered %d %q, want %d %q", w.what, status, code, w.status, w.code)
		return
	}
	w.t.Logf("%s answered %d %q, as wanted", w.what, status, code)
}

// leaverCapture keeps the request the DPoP transport signed, instead of sending it.
type leaverCapture struct{ last *http.Request }

func (c *leaverCapture) RoundTrip(r *http.Request) (*http.Response, error) {
	c.last = r
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
}

// leaverBody is value as a JSON request body. (twobuild_harness_test.go's jsonBody is the same, but
// only under the twobuild tag.)
func leaverBody(t *testing.T, value any) string {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// localPart is the part of email before its @.
func localPart(email string) string {
	local, _, _ := strings.Cut(email, "@")
	return local
}

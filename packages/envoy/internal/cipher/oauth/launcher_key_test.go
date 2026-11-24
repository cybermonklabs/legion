package oauth_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/cipher/api"
	"github.com/sjawhar/envoy/internal/cipher/ciphertest"
	"github.com/sjawhar/envoy/internal/cipher/enroll"
	"github.com/sjawhar/envoy/internal/cipher/machine"
	"github.com/sjawhar/envoy/internal/cipher/oauth"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/proof"
	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/cipher/requests"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/aws/awstest"
	"github.com/sjawhar/envoy/internal/cipher/store"
	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
	"github.com/sjawhar/envoy/internal/ratelimit"
)

// credentialOn inserts a live launcher credential bound to key, as a device login's approval
// mints one, and answers its id: the credential a machine login's token names, whose key is the
// key of that machine's machine.json.
func (f *fixture) credentialOn(t *testing.T, key *ecdsa.PrivateKey) string {
	t.Helper()
	id := uuid.NewString()
	if _, err := f.pool.Exec(context.Background(), `insert into launcher_credentials (id, operator, host, key_thumbprint, public_jwk, expires_at)
		values ($1, $2, 'example-host', $3, $4, now() + interval '30 days')`, id, person, thumbprint(t, key), publicJWK(t, key)); err != nil {
		t.Fatalf("insert launcher_credentials: %v", err)
	}
	return id
}

// publicJWK is key's public half as a JWK, as a launcher credential records it.
func publicJWK(t *testing.T, key *ecdsa.PrivateKey) json.RawMessage {
	t.Helper()
	raw, err := jose.JSONWebKey{Key: &key.PublicKey}.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// tokenLogin is a Kubernetes service's token login proving key, under a subject of its own, as
// POST /v1/launcher-credentials/token records one (enroll.TokenLogin).
func tokenLogin(t *testing.T, key *ecdsa.PrivateKey) enroll.TokenLogin {
	t.Helper()
	name := "example-launcher-" + uuid.NewString()[:8]
	expires := time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond)
	return enroll.TokenLogin{
		Subject: "system:serviceaccount:example-ns:" + name, Service: "example-launcher", Host: "example-ns/" + name,
		TokenExpires: expires, Thumbprint: thumbprint(t, key), JWK: publicJWK(t, key), Expires: expires,
	}
}

// credentialsOn is how many launcher credentials, live or ended, are bound to key.
func (f *fixture) credentialsOn(t *testing.T, key *ecdsa.PrivateKey) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(), `select count(*) from launcher_credentials where key_thumbprint = $1`, thumbprint(t, key)).Scan(&n); err != nil {
		t.Fatalf("count the credentials of a key: %v", err)
	}
	return n
}

// readTokensOn is how many access tokens holding read, live or ended, are bound to key, in either
// table.
func (f *fixture) readTokensOn(t *testing.T, key *ecdsa.PrivateKey) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(), `select (select count(*) from oauth_narrowed_access_tokens where jwk_thumbprint = $1)
		+ (select count(*) from oauth_access_tokens where request #>> '{session,jwk_thumbprint}' = $1 and request #>> '{session,extra,project_scope}' = 'none')`,
		thumbprint(t, key)).Scan(&n); err != nil {
		t.Fatalf("count the read tokens of a key: %v", err)
	}
	return n
}

// endReadTokens ends every read token bound to key, in either table: revoked, as a revocation
// deactivates one, or expired. A read token of oauth_access_tokens is a read entry's, whose
// project scope is none, as readTokensOn counts it.
func (f *fixture) endReadTokens(t *testing.T, key *ecdsa.PrivateKey, revoked bool) {
	t.Helper()
	set := `active = false`
	if !revoked {
		set = `expires_at = now() - interval '1 second'`
	}
	for _, statement := range []string{
		`update oauth_narrowed_access_tokens set ` + set + ` where jwk_thumbprint = $1`,
		`update oauth_access_tokens set ` + set + ` where request #>> '{session,jwk_thumbprint}' = $1 and request #>> '{session,extra,project_scope}' = 'none'`,
	} {
		if _, err := f.pool.Exec(context.Background(), statement, thumbprint(t, key)); err != nil {
			t.Fatalf("end the read tokens of a key: %v", err)
		}
	}
}

// keyRuleRefusal is what the store's refusal of a read token on a key a live launcher credential
// is bound to says, in each of its two sentences (oauthstore's launcherKeyRefusal).
const keyRuleRefusal = "signs launcher proofs"

// launcherAnswer is what a path that writes a launcher credential's key answered: its status; for
// a refusal, its code (the broker API's) or its OAuth error (the exchange's), and its message or
// its OAuth description, which carries the hint; and for a credential minted, the credential's id.
type launcherAnswer struct {
	status                    int
	code, message, credential string
}

// apiAnswer is an answer of the broker's API: a refusal's code and message, or the credential_id
// of an approval or a token login.
func apiAnswer(status int, body map[string]any) launcherAnswer {
	code, _ := body["code"].(string)
	message, _ := body["error"].(string)
	credential, _ := body["credential_id"].(string)
	return launcherAnswer{status: status, code: code, message: message, credential: credential}
}

// tradeAnswer is an answer of the exchange: a refusal's OAuth error and description, or the
// credential the trade minted.
func tradeAnswer(status int, body map[string]any) launcherAnswer {
	code, _ := body["error"].(string)
	message, _ := body["error_description"].(string)
	credential, _ := body[oauthwire.ExtraCredentialID].(string)
	return launcherAnswer{status: status, code: code, message: message, credential: credential}
}

// refusal is what a refusal of a launcher credential's key reads: its status, its code or OAuth
// error, and the words its message or hint carries.
type refusal struct {
	status int
	code   string
	says   []string
}

// refused fails the test unless answer, what a path answered when, is want and names no
// credential.
func refused(t *testing.T, when string, answer launcherAnswer, want refusal) {
	t.Helper()
	ok := answer.status == want.status && answer.code == want.code && answer.credential == ""
	for _, words := range want.says {
		ok = ok && strings.Contains(answer.message, words)
	}
	if !ok {
		t.Fatalf("%s, the path answered %d %s %q, want %d %s saying %q", when, answer.status, answer.code, answer.message, want.status, want.code, want.says)
	}
}

// mints fails the test unless answer, what a path answered when, names the credential it minted.
func mints(t *testing.T, when string, answer launcherAnswer) {
	t.Helper()
	if answer.status != http.StatusOK || answer.credential == "" {
		t.Fatalf("%s, the path answered %d %s %q, want 200 naming the credential it minted", when, answer.status, answer.code, answer.message)
	}
}

// The refusals of a launcher credential's key. A key a live read token is bound to is refused
// with a code or a hint of its own, naming the read token and the cure, a new key: 409
// KEY_BINDS_READ_TOKEN from the broker's API, and invalid_grant from the exchange. A key another
// live launcher credential holds is refused as it was before read tokens: 409
// KEY_HOLDS_LIVE_CREDENTIAL, and invalid_grant naming another live machine login.
var (
	apiReadTokenKey   = refusal{http.StatusConflict, "KEY_BINDS_READ_TOKEN", []string{"read token", "new key"}}
	apiHeldKey        = refusal{http.StatusConflict, "KEY_HOLDS_LIVE_CREDENTIAL", []string{machine.ErrKeyHoldsLiveCredential.Error()}}
	tradeReadTokenKey = refusal{http.StatusBadRequest, "invalid_grant", []string{"A live read token is bound to the key this proof was signed with. Make a new key."}}
)

// launcherAPI is the broker's API (api.Register) on a fixture's database, wired as cmd/cipherd
// wires the two routes that write a launcher credential's key for a person or a service: the
// approval of a device login (POST /v1/credential-requests/{record}/approve), after the device
// authorization that opens the login and the lookup by its code that finds it, as Dispatch's
// machine-login page makes them, and a token login (POST /v1/launcher-credentials/token). Its
// issuer mints the ServiceAccount tokens both routes take.
type launcherAPI struct {
	url    string
	issuer *oidctest.Issuer
	key    *oidctest.Key
}

// launcherAPIAudience is the audience the API's ServiceAccount tokens are minted for and verified
// at.
const launcherAPIAudience = "legion-broker-pod"

// newLauncherAPI mounts the API on pool's database. ciphertest.DaemonAccount logs in as
// legion-daemon with its token, and ciphertest.APIAccount calls as Dispatch, which relays a
// person's decision.
func newLauncherAPI(t *testing.T, pool *pgxpool.Pool) *launcherAPI {
	t.Helper()
	st := &store.Store{Pool: pool}
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	issuer := oidctest.New(t)
	key := issuer.PublishKey(t, "launcher-api-key")
	keys, err := oidc.NewKeys(context.Background(), issuer.URL())
	if err != nil {
		t.Fatalf("oidc.NewKeys: %v", err)
	}
	accounts := enroll.K8sPodVerifier{Verifier: oidc.NewWithKeys(issuer.URL(), launcherAPIAudience, keys)}
	enr := &enroll.Service{Store: st, Lease: time.Hour, Pod: accounts, ServiceLogins: map[string]string{ciphertest.DaemonAccount: "legion-daemon"}}
	rules := policytest.Current(t, awstest.NewLocal())
	provider := ciphertest.NewOAuthProvider(t, st, issuer)
	logins := &machine.Service{
		Store: st, Enroll: enr, Policy: rules, Audience: srv.URL, Skew: time.Minute,
		PendingTTL: 15 * time.Minute, CredentialLifetime: time.Hour, OAuth: provider, Replay: enr.Replay,
	}
	// Every row logs one person and one service in from one address many times, which the
	// launcher limits' buckets would refuse 429 long before the rows are done.
	unbounded := ratelimit.Limit{Every: time.Nanosecond, Burst: 1000}
	api.Register(mux, api.Deps{
		PublicURL:      srv.URL,
		APIAccounts:    map[string]api.Account{ciphertest.APIAccount: ciphertest.Caller(t, ciphertest.APICaller)},
		AccountTokens:  accounts,
		Enroll:         enr,
		Machine:        &requests.Machine{Store: st, Policy: rules, Audience: srv.URL, Skew: time.Minute},
		MachineLogin:   logins,
		OAuth:          provider,
		DeviceLogin:    logins.LoginDevice,
		TokenHooks:     logins.TokenHooks(),
		Proof:          &proof.Verifier{Skew: time.Minute, Lookup: enr.Lookup, LookupLauncher: enr.AuthenticateLauncher, Replay: enr.Replay},
		LauncherLimits: &api.LauncherLimits{PerAddress: unbounded, PerLogin: unbounded},
	})
	return &launcherAPI{url: srv.URL, issuer: issuer, key: key}
}

// token is account's projected ServiceAccount token for the API's audience, issued a moment ago
// and living half an hour, inside the hour the broker holds such a token to.
func (a *launcherAPI) token(t *testing.T, account string) string {
	t.Helper()
	claims := a.issuer.Claims(account, launcherAPIAudience)
	now := time.Now()
	claims["iat"], claims["exp"] = now.Add(-5*time.Second).Unix(), now.Add(30*time.Minute).Unix()
	return a.issuer.Mint(t, a.key, claims)
}

// call posts body to path as JSON, with bearer as its Authorization where it is set, and answers
// the status and the decoded answer.
func (a *launcherAPI) call(t *testing.T, path, bearer string, body any) (int, map[string]any) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, a.url+path, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	var answer map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&answer); err != nil {
		t.Fatalf("decode the answer of POST %s (%d): %v", path, resp.StatusCode, err)
	}
	return resp.StatusCode, answer
}

// approve opens a device login of person's machine signed with key, looks it up by its code and
// approves it as that person, as Dispatch's machine-login page relays a person's approval, and
// answers what the approval answered.
func (a *launcherAPI) approve(t *testing.T, key *ecdsa.PrivateKey) launcherAnswer {
	t.Helper()
	request, err := record.Sign(key, a.url, []record.AuthorizationDetail{{Type: record.KindLauncherCredential, Identifier: "example-host"}}, "", person, nil, time.Now())
	if err != nil {
		t.Fatalf("sign the machine login's request: %v", err)
	}
	resp, err := http.PostForm(a.url+"/v1/oauth/device_authorization", url.Values{
		"client_id": {oauthwire.ClientID}, "scope": {"machine"}, "dpop_jkt": {thumbprint(t, key)}, "request": {request},
	})
	if err != nil {
		t.Fatalf("POST /v1/oauth/device_authorization: %v", err)
	}
	defer resp.Body.Close()
	var opened struct {
		UserCode string `json:"user_code"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&opened); err != nil || resp.StatusCode != http.StatusOK || opened.UserCode == "" {
		t.Fatalf("POST /v1/oauth/device_authorization = %d (%v), want 200 and a user code", resp.StatusCode, err)
	}
	dispatch := a.token(t, ciphertest.APIAccount)
	status, looked := a.call(t, "/v1/machine-logins/lookup", dispatch, map[string]any{"code": opened.UserCode})
	recordID, _ := looked["record_id"].(string)
	if status != http.StatusOK || recordID == "" {
		t.Fatalf("POST /v1/machine-logins/lookup = %d %v, want 200 and the login's record", status, looked)
	}
	return apiAnswer(a.call(t, "/v1/credential-requests/"+recordID+"/approve", dispatch, map[string]any{"approver": person, "code": opened.UserCode}))
}

// tokenLogin logs legion-daemon in with ciphertest.DaemonAccount's token and a request signed with
// key, and answers what the login answered.
func (a *launcherAPI) tokenLogin(t *testing.T, key *ecdsa.PrivateKey) launcherAnswer {
	t.Helper()
	request, err := record.Sign(key, a.url, []record.AuthorizationDetail{
		{Type: record.KindLauncherCredential, Identifier: "example-host-legion-daemon", Service: "legion-daemon"},
	}, "", "", nil, time.Now())
	if err != nil {
		t.Fatalf("sign the token login's request: %v", err)
	}
	return apiAnswer(a.call(t, "/v1/launcher-credentials/token", "", map[string]any{"token": a.token(t, ciphertest.DaemonAccount), "request": request}))
}

// launcherPath is one of the paths that write a launcher credential's key, tried with a key, and
// the refusals it gives: readToken for a key a live read token is bound to, and heldKey for a key
// another live launcher credential holds.
type launcherPath struct {
	name               string
	try                func(t *testing.T, f *signInFixture, routes *launcherAPI, key *ecdsa.PrivateKey) launcherAnswer
	readToken, heldKey refusal
}

// launcherPaths is every path that writes a launcher credential's key: a device login's approval
// (POST /v1/credential-requests/{record}/approve, whose credential machine.Service.ApplyDecision
// mints in the approval's transaction, enroll.Service.MintLauncherCredentialTx), a token login
// (POST /v1/launcher-credentials/token, machine.Service.TokenLogin and
// enroll.Service.IssueTokenLogin), a Google machine's trade that asks for its credential, and a
// person's trade, both through the exchange's credential step.
var launcherPaths = []launcherPath{
	{"a device login approved for the key", func(t *testing.T, _ *signInFixture, routes *launcherAPI, key *ecdsa.PrivateKey) launcherAnswer {
		return routes.approve(t, key)
	}, apiReadTokenKey, apiHeldKey},
	{"a token login with the key", func(t *testing.T, _ *signInFixture, routes *launcherAPI, key *ecdsa.PrivateKey) launcherAnswer {
		return routes.tokenLogin(t, key)
	}, apiReadTokenKey, apiHeldKey},
	{"a Google machine's trade asking for its credential with the key", func(t *testing.T, f *signInFixture, _ *launcherAPI, key *ecdsa.PrivateKey) launcherAnswer {
		return tradeAnswer(f.exchange(t, machineForm(t, f, "box-session-key"), key))
	}, tradeReadTokenKey, refusal{http.StatusBadRequest, "invalid_grant", []string{"Another live machine login holds the key this proof was signed with. A machine makes a key of its own."}}},
	{"a person's trade proving the key", func(t *testing.T, f *signInFixture, _ *launcherAPI, key *ecdsa.PrivateKey) launcherAnswer {
		return tradeAnswer(f.exchange(t, f.personTrade(t, "ada-box-key"), key))
	}, tradeReadTokenKey, refusal{http.StatusBadRequest, "invalid_grant", []string{"Another live machine login holds the key this proof was signed with. A sign-in makes a box key of its own."}}},
}

// TestNoKeyIsBothAReadTokensAndALauncherCredentials holds the two-sided key rule: no key is both a
// live read token's and a live launcher credential's, a read token being any token granted read,
// a narrowed one or the read entry's own.
//
// Every read trade whose proof key can hold a launcher credential, tried binding that key with no
// holder proof, is refused and stores no token: a person's sign-in with its box key, a machine
// login with machine.json's key, a Google machine with the key its earlier trade put a credential
// on, a pod whose key holds a token login's credential, and the read entry's pod with such a key,
// asking for read and asking for no scope. The person's sign-in is refused for sending no holder
// proof, which its read trade sends whatever its box key holds; every other trade is refused by the
// key rule. The same trade with a holder proof, or with a key that holds no credential, mints.
//
// With a live read token bound to a key, of either kind, every path that writes a launcher
// credential's key is refused, and no credential is bound to the key. Each refusal reads the
// status, the code or OAuth error, and the message or hint its path answers: a device login's
// approval and a token login are 409 KEY_BINDS_READ_TOKEN, whose message names the read token and
// the cure, a new key, and the two trades are invalid_grant with a hint that says the same. Once
// the read token is revoked, and with another key once its read token has expired, each mints.
// With another live launcher credential on the key, each path is refused as it was before read
// tokens: 409 KEY_HOLDS_LIVE_CREDENTIAL, and invalid_grant naming another live machine login.
//
// Two transactions run at once, one storing a read token bound to a key and one minting a
// credential on it, end with exactly one of them committed, fifty times over.
func TestNoKeyIsBothAReadTokensAndALauncherCredentials(t *testing.T) {
	f := newNarrowingFixture(t, func(*oauth.Config) {})
	routes := newLauncherAPI(t, f.pool)
	ctx := context.Background()

	withTokenLogin := func(t *testing.T) *ecdsa.PrivateKey {
		key := newKey(t)
		if _, err := f.enrollments().IssueTokenLogin(ctx, tokenLogin(t, key)); err != nil {
			t.Fatalf("IssueTokenLogin: %v", err)
		}
		return key
	}
	for _, c := range []struct {
		name string
		// trade is the read trade, proved with a key a live launcher credential is bound to.
		trade func(t *testing.T) narrowingTrade
		scope string
		// says is what the refusal's description holds: the key rule's sentence, but for a
		// person's sign-in, whose read trade with no holder proof is refused before the key rule
		// is read (it must send one whatever its box key holds).
		says string
	}{
		{"a person's sign-in, with its box key", func(t *testing.T) narrowingTrade {
			return narrowingRows["person-sign-in"].trade(t, f)
		}, oauthwire.ScopeRead, "A person's read trade sends a DPoP-Holder proof"},
		{"a machine login, with machine.json's key", func(t *testing.T) narrowingTrade {
			machine := newKey(t)
			subject := f.devBoxTokenOf(t, machine, person, f.credentialOn(t, machine))
			return narrowingTrade{form: exchangeForm(subject, oauthwire.SubjectTokenTypeAccessToken, narrowedDevBoxSession), key: machine}
		}, oauthwire.ScopeRead, keyRuleRefusal},
		{"a Google machine, with the key its earlier trade put a credential on", func(t *testing.T) narrowingTrade {
			tr := narrowingRows["google-identity-no-person"].trade(t, f)
			mints(t, "the machine's earlier trade", tradeAnswer(f.send(t, tr, "", "")))
			tr.form = exchangeForm(f.machineToken(t, signInMachine, machineAudience, true), oauthwire.SubjectTokenTypeGoogle, narrowedFleetSession)
			return tr
		}, oauthwire.ScopeRead, keyRuleRefusal},
		{"a pod whose key holds a token login's credential", func(t *testing.T) narrowingTrade {
			tr := narrowingRows["pod-entry"].trade(t, f)
			tr.key = withTokenLogin(t)
			return tr
		}, oauthwire.ScopeRead, keyRuleRefusal},
		{"the read entry's pod asking for read, with such a key", func(t *testing.T) narrowingTrade {
			return narrowingTrade{form: f.readPodForm(t), key: withTokenLogin(t), session: readSession}
		}, oauthwire.ScopeRead, keyRuleRefusal},
		{"the read entry's pod asking for no scope, with such a key", func(t *testing.T) narrowingTrade {
			return narrowingTrade{form: f.readPodForm(t), key: withTokenLogin(t), session: readSession}
		}, "", keyRuleRefusal},
	} {
		t.Run("a read trade of "+c.name+" binding that key is refused", func(t *testing.T) {
			tr := c.trade(t)
			tr.holder = nil
			before := f.tokenRows(t)
			status, body := f.send(t, tr, c.scope, "")
			refusedSaying(t, status, body, http.StatusBadRequest, "invalid_request", c.says)
			if after := f.tokenRows(t); after != before {
				t.Errorf("the refused trade left %d access tokens, want the %d there were", after, before)
			}
			if bound := f.readTokensOn(t, tr.key); bound != 0 {
				t.Errorf("%d read tokens are bound to the key a launcher credential holds", bound)
			}
			// With a holder key, or for the read entry's trade asking for no scope, which may
			// send no holder proof, with a key that holds no credential, the same trade mints.
			again := c.trade(t)
			if c.scope == "" {
				again.key = newKey(t)
			} else {
				again.holder = newKey(t)
			}
			status, body = f.send(t, again, c.scope, "")
			if answer := f.introspected(t, accessTokenOf(t, status, body)); answer.Thumbprint != thumbprint(t, again.readKey()) {
				t.Errorf("the trade with a key of its own introspects bound to %q, want %q", answer.Thumbprint, thumbprint(t, again.readKey()))
			}
		})
	}

	readTokens := []struct {
		name string
		bind func(t *testing.T, key *ecdsa.PrivateKey)
	}{
		{"a narrowed token", func(t *testing.T, key *ecdsa.PrivateKey) {
			tr := narrowingRows["pod-entry"].trade(t, f)
			tr.key = key
			status, body := f.send(t, tr, oauthwire.ScopeRead, "")
			accessTokenOf(t, status, body)
		}},
		{"the read entry's token", func(t *testing.T, key *ecdsa.PrivateKey) {
			status, body := f.send(t, narrowingTrade{form: f.readPodForm(t), key: key}, "", "")
			accessTokenOf(t, status, body)
		}},
	}
	for _, token := range readTokens {
		for _, path := range launcherPaths {
			t.Run(path.name+", with "+token.name+" live on the key, is refused", func(t *testing.T) {
				key := newKey(t)
				token.bind(t, key)
				refused(t, "with "+token.name+" live on the key", path.try(t, f, routes, key), path.readToken)
				if bound := f.credentialsOn(t, key); bound != 0 {
					t.Fatalf("launcher_credentials holds %d rows on the key %s is bound to, want none", bound, token.name)
				}
				f.endReadTokens(t, key, true)
				mints(t, "once "+token.name+" on the key was revoked", path.try(t, f, routes, key))
				expired := newKey(t)
				token.bind(t, expired)
				f.endReadTokens(t, expired, false)
				mints(t, "on a key whose "+token.name+" has expired", path.try(t, f, routes, expired))
			})
		}
	}
	for _, path := range launcherPaths {
		t.Run(path.name+", with another live launcher credential on the key, is refused as before", func(t *testing.T) {
			key := newKey(t)
			f.credentialOn(t, key)
			refused(t, "with another live launcher credential on the key", path.try(t, f, routes, key), path.heldKey)
			if bound := f.credentialsOn(t, key); bound != 1 {
				t.Fatalf("launcher_credentials holds %d rows on the key, want the one that held it", bound)
			}
		})
	}

	t.Run("a read token and a credential stored at once on one key end with one of them", func(t *testing.T) {
		for i := range 50 {
			key := newKey(t)
			form := f.readPodForm(t)
			form.Set("scope", oauthwire.ScopeRead)
			headers := http.Header{"DPoP": {dpopProof(t, key, f.url+"/token")}}
			login := tokenLogin(t, key)
			start := make(chan struct{})
			var wg sync.WaitGroup
			var answer tokenAnswer
			var minted error
			wg.Add(2)
			go func() {
				defer wg.Done()
				<-start
				answer = postTokenWith(f.url+"/token", form, headers)
			}()
			go func() {
				defer wg.Done()
				<-start
				_, minted = f.enrollments().IssueTokenLogin(ctx, login)
			}()
			close(start)
			wg.Wait()
			if answer.err != nil {
				t.Fatalf("round %d: the read trade: %v", i, answer.err)
			}
			if minted != nil && !errors.Is(minted, enroll.ErrKeyBindsReadToken) {
				t.Fatalf("round %d: the token login failed with %v, which is not the key rule's refusal", i, minted)
			}
			tokenStored, credentialStored := answer.status == http.StatusOK, minted == nil
			if tokenStored == credentialStored {
				t.Fatalf("round %d: the read trade answered %d %v and the token login %v: want exactly one of them stored", i, answer.status, answer.body, minted)
			}
			if description, _ := answer.body["error_description"].(string); !tokenStored &&
				(answer.status != http.StatusBadRequest || answer.body["error"] != "invalid_request" || !strings.Contains(description, keyRuleRefusal)) {
				t.Fatalf("round %d: the read trade answered %d %v, want 200 or the key rule's 400 invalid_request", i, answer.status, answer.body)
			}
			if got := f.readTokensOn(t, key) + f.credentialsOn(t, key); got != 1 {
				t.Fatalf("round %d: the key holds %d read tokens and credentials together, want 1", i, got)
			}
		}
		t.Log("50 rounds each stored exactly one of the read token and the credential")
	})
}

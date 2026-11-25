package main

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/sjawhar/envoy/internal/cipher/ciphertest"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/internal/cipher/proof"
	"github.com/sjawhar/envoy/internal/cipherclient"
	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// The leaver rig's broker half (leaver_hold_rig_test.go builds the rig): the options that wire the
// secrets broker (withBrokerGate, withPersonSignIns), the credentials a person the rig signs in
// holds at the broker (machineLogin, brokerCredentials, personSignIn), and the credential calls
// that use them there, each answering as every credential call of the rig answers.

// withBrokerGate puts the broker's real project-access client in front of a Dispatch server, and
// with it the broker's real hold gate, which ciphertest.WithDispatchAccess builds from that client
// as cmd/cipherd/main.go does.
func withBrokerGate() leaverOption {
	return func(c *leaverConfig) {
		// The router is set once it is built, by the test goroutine, and read by the server's
		// handler goroutines, so it is held atomically.
		var dispatchHandler atomic.Pointer[http.Handler]
		c.broker = append(c.broker, func(t *testing.T, r *leaverRig) []ciphertest.Option {
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				handler := dispatchHandler.Load()
				if handler == nil {
					http.Error(w, "Dispatch router is not built", http.StatusServiceUnavailable)
					return
				}
				(*handler).ServeHTTP(w, req)
			}))
			server.Start()
			t.Cleanup(server.Close)
			r.brokerDispatch = server
			accountFile := filepath.Join(t.TempDir(), "broker-dispatch-account")
			if err := os.WriteFile(accountFile, []byte(r.brokerAccountToken(t)+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			return []ciphertest.Option{ciphertest.WithDispatchAccess(server.URL, ciphertoken.NewAccountToken(accountFile), r.brokerNow)}
		})
		c.built = append(c.built, func(t *testing.T, r *leaverRig) {
			if r.brokerDispatch == nil {
				t.Fatal("withBrokerGate did not start its Dispatch server")
			}
			dispatchHandler.Store(&r.handler)
		})
	}
}

// R4's release bound, in the spec's own numbers: a person's credentials are served again within 40
// seconds of their sign-in to Dispatch, of which the broker keeps Dispatch's answer about them 30
// seconds (leaverSpecBrokerCache) and Dispatch keeps the broker's introspection answer the other 10,
// 5 cached and 5 in flight (leaverSpecIntrospection). They are the spec's, not the code's
// (projectaccess.CacheTTL), so a test that measures the release by them fails on a broker that keeps
// its answer longer than the spec allows.
const (
	leaverSpecBrokerCache   = 30 * time.Second
	leaverSpecIntrospection = 10 * time.Second
)

// brokerNow is the clock the broker's project-access client and hold gate read: the wall clock,
// ahead by what advanceBrokerClock and pastBrokerCache have moved it.
func (r *leaverRig) brokerNow() time.Time { return time.Now().Add(time.Duration(r.brokerAhead.Load())) }

// advanceBrokerClock moves the clock of the broker's project-access client and hold gate d ahead,
// as d passing would. It fails t on a rig built without withBrokerGate, whose broker asks Dispatch
// nothing.
func (r *leaverRig) advanceBrokerClock(t *testing.T, d time.Duration) {
	t.Helper()
	if r.brokerDispatch == nil {
		t.Fatal("moving the broker's clock needs a rig built withBrokerGate: its broker asks Dispatch nothing")
	}
	r.brokerAhead.Add(int64(d))
}

// pastBrokerCache moves the clock of the broker's project-access client and hold gate past the
// 30 seconds the client keeps Dispatch's answer about a person (projectaccess.CacheTTL), so the
// broker's next check of each person asks Dispatch rather than reading an answer it was given
// before. A test calls it where the answer the broker keeps predates a hold whose effect it
// measures at once; a test that measures that cache never calls it: TestLeaverHoldBound keeps the
// wall clock, and TestLeaverHoldRelease moves the clock by the spec's 30 seconds at a sign-in
// (advanceBrokerClock, leaverSpecBrokerCache). It fails t on a rig built without withBrokerGate.
func (r *leaverRig) pastBrokerCache(t *testing.T) {
	t.Helper()
	r.advanceBrokerClock(t, projectaccess.CacheTTL+time.Second)
}

// withPersonSignIns configures the broker to accept a person's Google trade with the machine's
// metadata identity as its actor. Removing that production exchange wiring makes personSignIn
// fail at the real token endpoint rather than merely changing a fixture-local value. The rig's
// metadata machine is its own (metadataHost, and the token it answers, machineToken), named to each
// client explicitly rather than through the process's environment, so rigs of tests that run in
// parallel do not share it.
func withPersonSignIns() leaverOption {
	return func(c *leaverConfig) {
		c.broker = append(c.broker, func(t *testing.T, r *leaverRig) []ciphertest.Option {
			r.metadataHost, r.machineToken = startLeaverMetadataServer(t, r.google, r.googleKey, leaverMachineAccount)
			return []ciphertest.Option{ciphertest.WithPersonSignIns(r.google, leaverMachineAccount)}
		})
	}
}

// startLeaverMetadataServer is the small boundary fake for a Compute Engine metadata server, and
// answers its host, as googleidentity.MetadataHostVar names one, and the identity token it
// answers. Its request and response shape are startMetadataServer's: only Google-flavoured
// identity requests for the broker's machine audience receive the fake Google's signed token.
func startLeaverMetadataServer(t *testing.T, google *oidctest.Issuer, key *oidctest.Key, machineAccount string) (host, token string) {
	t.Helper()
	claims := google.Claims(machineAccount, ciphertest.GoogleMachineAudience)
	claims["email"] = machineAccount
	claims["email_verified"] = true
	token = google.Mint(t, key, claims)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet ||
			req.URL.Path != "/computeMetadata/v1/instance/service-accounts/default/identity" ||
			req.Header.Get("Metadata-Flavor") != "Google" ||
			req.URL.Query().Get("audience") != ciphertest.GoogleMachineAudience ||
			req.URL.Query().Get("format") != "full" {
			http.NotFound(w, req)
			return
		}
		w.Header().Set("Metadata-Flavor", "Google")
		_, _ = io.WriteString(w, token)
	}))
	t.Cleanup(server.Close)
	return strings.TrimPrefix(server.URL, "http://"), token
}

// machineLogin logs a machine of p's in at the broker as the helper's `machine login` does: p
// approves it on Dispatch's machine-login page in their browser, Dispatch relays the approval to
// the broker, and the machine redeems its device code for its token pair.
func (r *leaverRig) machineLogin(t *testing.T, p *leaverPerson) {
	t.Helper()
	p.machineKey = ciphertest.NewSigningKey(t)
	device := r.client.DeviceLogin(t, p.machineKey, p.email, localPart(p.email)+"-box")
	code := leaverBody(t, map[string]string{"code": device.UserCode})
	looked := r.browser(t, p, http.MethodPost, "/api/v1/credential-requests/machine-lookup", code)
	var lookup struct {
		RecordID string `json:"record_id"`
	}
	if err := json.Unmarshal(looked.Body.Bytes(), &lookup); err != nil || lookup.RecordID == "" {
		t.Fatalf("Dispatch's machine-lookup of %s's machine login answered %d %s, want its record", p.email, looked.Code, looked.Body.String())
	}
	approved := r.browser(t, p, http.MethodPost, "/api/v1/credential-requests/"+lookup.RecordID+"/approve", code)
	var decided struct {
		State        string `json:"state"`
		CredentialID string `json:"credential_id"`
	}
	if err := json.Unmarshal(approved.Body.Bytes(), &decided); err != nil || decided.State != "approved" || decided.CredentialID == "" {
		t.Fatalf("Dispatch's approve of %s's machine login answered %d %s, want it approved with a credential", p.email, approved.Code, approved.Body.String())
	}
	p.credential = decided.CredentialID
	token, err := r.client.DeviceToken(t, p.machineKey, device)
	if err != nil {
		t.Fatalf("the device token of %s's machine login: %v", p.email, err)
	}
	p.machineToken = token
}

// pendingMachineLogin opens a new machine-login decision for p and leaves it pending. The held
// approval assertion calls Dispatch's real decision route with p's personal token after the hold.
func (r *leaverRig) pendingMachineLogin(t *testing.T, p *leaverPerson) (recordID, code string) {
	t.Helper()
	device := r.client.DeviceLogin(t, ciphertest.NewSigningKey(t), p.email, localPart(p.email)+"-pending-box")
	code = device.UserCode
	looked := r.browser(t, p, http.MethodPost, "/api/v1/credential-requests/machine-lookup",
		leaverBody(t, map[string]string{"code": code}))
	var lookup struct {
		RecordID string `json:"record_id"`
	}
	if err := json.Unmarshal(looked.Body.Bytes(), &lookup); err != nil || lookup.RecordID == "" {
		t.Fatalf("Dispatch's machine-lookup of %s's pending login answered %d %s, want its record", p.email, looked.Code, looked.Body.String())
	}
	return lookup.RecordID, code
}

// brokerCredentials gives p, whose machine is logged in, a session enrolled under the machine
// login, that session's grant of an agent-tier secret p owns and of the human-tier secret, which
// ada approves on the broker's approval page, and a broker token minted from the machine login.
func (r *leaverRig) brokerCredentials(t *testing.T, p *leaverPerson) {
	t.Helper()
	ctx := context.Background()
	p.enrollment, p.sessionKey = r.client.EnrollSession(t, p.machineKey, p.credential, p.email+"-session")
	p.agentSecret = strings.ToUpper(localPart(p.email)) + "_AGENT_SECRET"
	r.broker.Secrets.Put(policytest.Secret(p.agentSecret, p.email, policy.TierAgent, localPart(p.email)+"-agent-secret-v1"))
	if err := r.broker.Policy.Refresh(ctx); err != nil {
		t.Fatalf("reload the broker's policy with %s: %v", p.agentSecret, err)
	}
	p.agentGrant = r.client.RequestAgentSecret(t, p.sessionKey, p.enrollment, "leaver hold", p.agentSecret)

	asked := r.client.AskForAgentSecret(t, p.sessionKey, p.enrollment, "leaver hold", leaverHumanSecret)
	var pending struct {
		RequestID string  `json:"request_id"`
		RecordID  *string `json:"record_id"`
	}
	if asked.Status != http.StatusOK || asked.State != "pending" || json.Unmarshal([]byte(asked.Body), &pending) != nil || pending.RecordID == nil {
		t.Fatalf("%s's session asks %s: %d %s, want 200 pending with an approval record", p.email, leaverHumanSecret, asked.Status, asked.Body)
	}
	r.broker.Decide(t, *pending.RecordID, true)
	status, body := r.client.Session(t, p.sessionKey, p.enrollment, http.MethodGet, "/v1/requests/"+pending.RequestID, nil)
	var read struct {
		State   string `json:"state"`
		GrantID string `json:"grant_id"`
	}
	if status != http.StatusOK || json.Unmarshal(body, &read) != nil || read.State != "granted" || read.GrantID == "" {
		t.Fatalf("%s's request for %s after ada approved it: %d %s, want 200 granted", p.email, leaverHumanSecret, status, body)
	}
	p.humanGrant = read.GrantID

	token, status, refusal := r.exchange(t, p)
	if token == "" {
		t.Fatalf("the exchange of %s's machine login's token answered %d %s, want a broker token", p.email, status, refusal)
	}
	p.brokerToken = token
}

// personSignIn trades p's Google identity on the fixture's metadata machine through the real
// token endpoint. The broker must record the sign-in and mint its launcher credential in one
// transaction; otherwise the release snapshot below catches the missing row. Every trade of p's
// signs with the one box key the first made, as a machine's agent-secrets login keeps its key, so
// a later trade renews the launcher credential the first minted rather than mint a second.
func (r *leaverRig) personSignIn(t *testing.T, p *leaverPerson) {
	t.Helper()
	status, refusal, credential := r.signInTrade(t, p)
	if status != http.StatusOK || credential == "" {
		t.Fatalf("%s's Google sign-in trade answered %d %q with launcher credential %q, want 200 with a launcher credential", p.email, status, refusal, credential)
	}
	p.signInCredential = credential
}

// signInTrade is one trade of p's Google identity on the fixture's metadata machine (personSignIn),
// answering its status, its OAuth error for a refusal, and the launcher credential the broker
// minted or renewed for it.
func (r *leaverRig) signInTrade(t *testing.T, p *leaverPerson) (status int, refusal, credential string) {
	t.Helper()
	if p.signInKey == nil {
		p.signInKey = ciphertest.NewSigningKey(t)
	}
	if r.machineToken == "" {
		t.Fatal("the rig has no metadata machine: build it withPersonSignIns")
	}
	claims := r.google.Claims("google-"+p.email, ciphertest.PersonSignInAudience)
	claims["email"] = p.email
	claims["email_verified"] = true
	claims["hd"] = leaverDomain
	personToken := r.google.Mint(t, r.googleKey, claims)
	form := url.Values{
		"grant_type":          {cipherclient.GrantTypeTokenExchange},
		"client_id":           {oauthwire.ExchangeClientID},
		"subject_token":       {personToken},
		"subject_token_type":  {oauthwire.SubjectTokenTypePerson},
		"actor_token":         {r.machineToken},
		"actor_token_type":    {oauthwire.ActorTokenTypeMachine},
		oauthwire.FormSession: {localPart(p.email) + "-sign-in"},
	}
	transport := &http.Transport{}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: cipherclient.NewDPoPTransport(p.signInKey, transport), Timeout: 30 * time.Second}
	response, err := client.PostForm(r.broker.URL+cipherclient.TokenPath, form)
	if err != nil {
		t.Fatalf("trade %s's Google sign-in: %v", p.email, err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read %s's Google sign-in trade: %v", p.email, err)
	}
	var answer struct {
		CredentialID string `json:"credential_id"`
		Error        string `json:"error"`
	}
	if err := json.Unmarshal(raw, &answer); err != nil {
		t.Fatalf("%s's Google sign-in trade answered %d %s, which does not decode: %v", p.email, response.StatusCode, raw, err)
	}
	return response.StatusCode, answer.Error, answer.CredentialID
}

// refreshGrant redeems the machine login's refresh token at the broker (POST /v1/oauth/token), as
// the helper does once its access token expires, and keeps the pair it answers.
func (r *leaverRig) refreshGrant(t *testing.T, p *leaverPerson) (int, string) {
	t.Helper()
	refreshed, err := r.client.Refresh(t, p.machineKey, p.machineToken)
	if err != nil {
		var refused *oauth2.RetrieveError
		if errors.As(err, &refused) && refused.Response != nil {
			return refused.Response.StatusCode, refused.ErrorCode
		}
		t.Fatalf("the refresh grant of %s's machine login got no answer: %v", p.email, err)
	}
	p.machineToken = refreshed
	return http.StatusOK, ""
}

// enrol asks the broker for a new session under p's machine login (POST /v1/enrollments).
func (r *leaverRig) enrol(t *testing.T, p *leaverPerson) (int, string) {
	t.Helper()
	return brokerAnswer(r.enrolment(t, p, p.machineKey, p.credential))
}

// enrolUnderSignIn asks the broker for a new session under the launcher credential of p's sign-in
// on the fixture's metadata machine (personSignIn), as a session launched under that sign-in
// enrols (POST /v1/enrollments).
func (r *leaverRig) enrolUnderSignIn(t *testing.T, p *leaverPerson) (int, string) {
	t.Helper()
	return brokerAnswer(r.enrolment(t, p, p.signInKey, p.signInCredential))
}

// enrolment is POST /v1/enrollments for a new session of p's, each naming a runtime of its own,
// under the launcher credential credential, whose key is key: the status and the body it answers.
func (r *leaverRig) enrolment(t *testing.T, p *leaverPerson, key *ecdsa.PrivateKey, credential string) (int, []byte) {
	t.Helper()
	session := ciphertest.NewSigningKey(t)
	thumbprint, err := proof.Thumbprint(&session.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	p.enrolments++
	return r.client.Launcher(t, key, credential, http.MethodPost, "/v1/enrollments",
		map[string]any{"kind": "session", "runtime_id": fmt.Sprintf("%s-session-%d", p.email, p.enrolments), "thumbprint": thumbprint})
}

// renewLease renews the lease of the session enrolled at setup (POST /v1/enrollments/{id}/renew).
func (r *leaverRig) renewLease(t *testing.T, p *leaverPerson) (int, string) {
	t.Helper()
	return brokerAnswer(r.client.Session(t, p.sessionKey, p.enrollment, http.MethodPost, "/v1/enrollments/"+p.enrollment+"/renew", map[string]any{}))
}

// mintBrokerToken asks the broker for a new broker token from the machine login's token (the RFC
// 8693 exchange).
func (r *leaverRig) mintBrokerToken(t *testing.T, p *leaverPerson) (int, string) {
	t.Helper()
	_, status, code := r.exchange(t, p)
	return status, code
}

// askAgentSecret is the setup session's ask for the agent-tier secret p owns (POST /v1/requests).
func (r *leaverRig) askAgentSecret(t *testing.T, p *leaverPerson) (int, string) {
	t.Helper()
	asked := r.client.AskForAgentSecret(t, p.sessionKey, p.enrollment, "leaver hold", p.agentSecret)
	return brokerAnswer(asked.Status, []byte(asked.Body))
}

// askHumanSecret is the setup session's ask for the human-tier secret (POST /v1/requests).
func (r *leaverRig) askHumanSecret(t *testing.T, p *leaverPerson) (int, string) {
	t.Helper()
	asked := r.client.AskForAgentSecret(t, p.sessionKey, p.enrollment, "leaver hold", leaverHumanSecret)
	return brokerAnswer(asked.Status, []byte(asked.Body))
}

// readAgentGrant reads the values of the agent-tier grant from setup (POST /v1/grants/{id}/values).
func (r *leaverRig) readAgentGrant(t *testing.T, p *leaverPerson) (int, string) {
	t.Helper()
	return brokerAnswer(r.client.Session(t, p.sessionKey, p.enrollment, http.MethodPost, "/v1/grants/"+p.agentGrant+"/values", nil))
}

// readHumanGrant reads the values of the human-tier grant ada approved at setup.
func (r *leaverRig) readHumanGrant(t *testing.T, p *leaverPerson) (int, string) {
	t.Helper()
	return brokerAnswer(r.client.Session(t, p.sessionKey, p.enrollment, http.MethodPost, "/v1/grants/"+p.humanGrant+"/values", nil))
}

// exchange trades p's machine login's access token for a broker token bound to the machine key
// (the RFC 8693 exchange), answering the token, or none with the status and the OAuth error.
func (r *leaverRig) exchange(t *testing.T, p *leaverPerson) (token string, status int, code string) {
	t.Helper()
	form := url.Values{
		"grant_type": {cipherclient.GrantTypeTokenExchange}, "client_id": {oauthwire.ExchangeClientID},
		"subject_token": {p.machineToken.AccessToken}, "subject_token_type": {oauthwire.SubjectTokenTypeAccessToken},
		oauthwire.FormSession: {localPart(p.email) + "-leaver-hold"},
	}
	transport := &http.Transport{}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: cipherclient.NewDPoPTransport(p.machineKey, transport), Timeout: 30 * time.Second}
	response, err := client.PostForm(r.broker.URL+cipherclient.TokenPath, form)
	if err != nil {
		t.Fatalf("the exchange of %s's machine login's token got no answer: %v", p.email, err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read the exchange's answer for %s: %v", p.email, err)
	}
	var answer struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	_ = json.Unmarshal(raw, &answer)
	return answer.AccessToken, response.StatusCode, answer.Error
}

// introspection is the broker's real token readback under its listener account. Its held field is
// a broker row of c-bound: dropping the production gate would leave it false while still serving
// every other request.
func (r *leaverRig) introspection(t *testing.T, p *leaverPerson) (status int, held bool, code string) {
	t.Helper()
	form := url.Values{"token": {p.brokerToken}}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, r.broker.URL+"/v1/oauth/introspect", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+r.broker.ListenerToken)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("introspect %s's broker token: %v", p.email, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read %s's introspection answer: %v", p.email, err)
	}
	if response.StatusCode >= http.StatusMultipleChoices {
		status, code := brokerAnswer(response.StatusCode, body)
		return status, false, code
	}
	var answer struct {
		Held bool `json:"held"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		t.Fatalf("decode %s's introspection answer %s: %v", p.email, body, err)
	}
	return response.StatusCode, answer.Held, ""
}

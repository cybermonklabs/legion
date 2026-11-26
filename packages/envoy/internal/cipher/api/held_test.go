// held_test.go: the broker's hold of a person Dispatch refused (internal/cipher/hold), at each point
// of the broker's HTTP API that serves a person's credential. Each test runs newTestServer's broker
// with a real hold.Gate, wired as cmd/cipherd wires one where BROKER_PROJECT_ACCESS_URL is set: one
// gate on the API's routes and on the OAuth provider, over one Dispatch client of the stand-in in
// internal/cipher/projectaccess/projectaccesstest, keeping Dispatch's answers in the broker's own
// database. The stand-in answers ada held once a test holds her and bob never; refusing the
// broker's own account is how a test makes the gate unable to decide.
package api_test

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/sjawhar/envoy/internal/cipher/answer"
	"github.com/sjawhar/envoy/internal/cipher/api"
	"github.com/sjawhar/envoy/internal/cipher/ciphertest"
	"github.com/sjawhar/envoy/internal/cipher/hold"
	"github.com/sjawhar/envoy/internal/cipher/oauth"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess/projectaccesstest"
	"github.com/sjawhar/envoy/internal/cipherclient"
	"github.com/sjawhar/envoy/internal/logtest"
)

// heldAda is the person the Dispatch stand-in answers held once a test holds her; servedBob is the
// person it never holds, the control every path serves.
const (
	heldAda   = "ada@example.com"
	servedBob = "bob@example.com"
)

// heldWords is the broker's refusal of a held person's credential, naming person: the text of 403
// MEMBERSHIP_HELD and of the token endpoint's membership_held.
func heldWords(person string) string {
	return person + "'s credentials are held by Dispatch; they work again when " + person +
		" signs in to Dispatch, or when Dispatch can confirm " + person + " with Google again"
}

// refusedAccountWords is the text of 503 HOLD_UNCHECKABLE for person while Dispatch refuses the
// broker's own account: a fixed reason, never the error behind it.
func refusedAccountWords(person string) string {
	return "whether Dispatch holds " + person + " cannot be read: Dispatch refused the broker's own account"
}

// brokerAccount is the bearer the broker presents to the Dispatch stand-in: the stand-in's own, or,
// once refused is set, one the stand-in answers 403, as a Dispatch whose DISPATCH_ACCESS_ACCOUNTS
// does not name the broker's account answers it.
type brokerAccount struct {
	bearer  string
	refused atomic.Bool
}

func (a *brokerAccount) Read() (string, error) {
	if a.refused.Load() {
		return "a bearer Dispatch does not take", nil
	}
	return a.bearer, nil
}

// gateClock is the clock the gate and its Dispatch client read: the wall clock, moved on by hand
// past the client's cache of an answer (projectaccess.CacheTTL), so a test reads Dispatch's new
// answer at once rather than waiting the cache out.
type gateClock struct{ ahead atomic.Int64 }

func (c *gateClock) now() time.Time { return time.Now().Add(time.Duration(c.ahead.Load())) }

func (c *gateClock) pastCache() { c.ahead.Add(int64(projectaccess.CacheTTL + time.Second)) }

// holdBroker is newTestServer's broker with the hold of refused people: gate is the one gate
// every point of the broker asks, and client drives its machine logins, enrollments and grants
// over HTTP as a machine does.
type holdBroker struct {
	*testServer
	dispatch *projectaccesstest.Server
	account  *brokerAccount
	clock    *gateClock
	gate     *hold.Gate
	client   *ciphertest.Client
}

func newHoldBroker(t *testing.T) *holdBroker {
	t.Helper()
	dispatch := projectaccesstest.NewServer(t, nil, map[string]string{heldAda: projectaccess.MemberFull, servedBob: projectaccess.MemberFull})
	account := &brokerAccount{bearer: dispatch.Token}
	clock := &gateClock{}
	access := projectaccess.New(dispatch.URL, account, nil)
	access.UseClock(clock.now)
	var gate *hold.Gate
	ts := newTestServerWith(t, func(d *api.Deps) {
		gate = hold.New(hold.Config{Access: access, Store: d.Enroll.Store, Now: clock.now})
		d.Hold = gate
		withProvider(t, func(c *oauth.Config) { c.Exchange.Projects, c.Exchange.Hold = access, gate })(d)
		// One provider, as cmd/cipherd builds one: the machine login's approval records the device
		// code that this provider's token endpoint redeems.
		d.MachineLogin.OAuth = d.OAuth
	})
	client := ciphertest.NewClient(t, ts.URL, strings.TrimPrefix(ts.URL, "http://"))
	client.UIToken, client.DPoP = ts.uiToken, cipherclient.NewDPoPTransport
	return &holdBroker{testServer: ts, dispatch: dispatch, account: account, clock: clock, gate: gate, client: client}
}

// hold has Dispatch answer ada held, past every answer the gate's client keeps.
func (b *holdBroker) hold() {
	b.dispatch.SetHeld(heldAda, true)
	b.clock.pastCache()
}

// release has Dispatch answer ada not held again, as her next sign-in does.
func (b *holdBroker) release() {
	b.dispatch.SetHeld(heldAda, false)
	b.clock.pastCache()
}

// refuseAccount has Dispatch refuse the broker's own account from now on, past every answer the
// gate's client keeps, so the gate can decide nobody's hold.
func (b *holdBroker) refuseAccount() {
	b.account.refused.Store(true)
	b.clock.pastCache()
}

// heldMachine is one person's machine login: its key, its launcher credential and, when the test
// redeemed its device code, its token pair.
type heldMachine struct {
	person     string
	key        *ecdsa.PrivateKey
	credential string
	token      *oauth2.Token
}

// login logs a machine of person's on host in: the device authorization, the person's approval
// as Dispatch relays it, and the token pair its device code redeems.
func (b *holdBroker) login(t *testing.T, person, host string) heldMachine {
	t.Helper()
	key := newSigningKey(t)
	authorization := b.client.DeviceLogin(t, key, person, host)
	credential := b.client.ApproveMachineLogin(t, person, authorization.UserCode)
	token, err := b.client.DeviceToken(t, key, authorization)
	if err != nil {
		t.Fatalf("redeem %s's device code on %s: %v", person, host, err)
	}
	return heldMachine{person: person, key: key, credential: credential, token: token}
}

// ask has the session POST /v1/requests for WORKER_TOKEN, the agent-tier secret every session gets
// at once.
func (b *holdBroker) ask(t *testing.T, enrollmentID string, key *ecdsa.PrivateKey) (int, []byte) {
	t.Helper()
	return b.session(t, key, enrollmentID, http.MethodPost, "/v1/requests",
		map[string]any{"request": signAgentSecretRequest(t, key, b.URL, "the hold of refused people", "WORKER_TOKEN"), "session_id": nil})
}

// count answers a count(*) query over the broker's database.
func (b *holdBroker) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := b.Store.Pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

// introspectAs posts token to the introspection route as Dispatch's server does, answering the
// status, the answer of a 200 and the refusal of anything else.
func (b *holdBroker) introspectAs(t *testing.T, token string) (int, oauthwire.Introspection, wireError) {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, b.URL+"/v1/oauth/introspect", strings.NewReader(url.Values{"token": {token}}.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Authorization", "Bearer "+b.uiToken)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("POST /v1/oauth/introspect: %v", err)
	}
	defer response.Body.Close()
	var answer oauthwire.Introspection
	var refusal wireError
	if response.StatusCode == http.StatusOK {
		err = json.NewDecoder(response.Body).Decode(&answer)
	} else {
		err = json.NewDecoder(response.Body).Decode(&refusal)
	}
	if err != nil {
		t.Fatalf("decode the introspection's %d answer: %v", response.StatusCode, err)
	}
	return response.StatusCode, answer, refusal
}

// exchangeFor trades m's machine access token for a session's token naming session, over a DPoP
// proof of m's key, as a dev box session does, and answers the token it was issued.
func (b *holdBroker) exchangeFor(t *testing.T, m heldMachine, session string) string {
	t.Helper()
	client := &http.Client{Transport: cipherclient.NewDPoPTransport(m.key, http.DefaultTransport)}
	response, err := client.PostForm(b.URL+"/v1/oauth/token", url.Values{
		"grant_type": {"urn:ietf:params:oauth:grant-type:token-exchange"}, "client_id": {oauthwire.ExchangeClientID},
		"subject_token": {m.token.AccessToken}, "subject_token_type": {oauthwire.SubjectTokenTypeAccessToken}, "session": {session},
	})
	if err != nil {
		t.Fatalf("exchange %s's machine token: %v", m.person, err)
	}
	defer response.Body.Close()
	var answer map[string]any
	if err := json.NewDecoder(response.Body).Decode(&answer); err != nil {
		t.Fatalf("decode %s's exchange answer: %v", m.person, err)
	}
	token, _ := answer["access_token"].(string)
	if response.StatusCode != http.StatusOK || token == "" {
		t.Fatalf("exchange %s's machine token for %s = %d %v, want 200 and a token", m.person, session, response.StatusCode, answer)
	}
	return token
}

// refusal decodes a refusal's body, the zero refusal for a body that is none.
func refusal(body []byte) wireError {
	var r wireError
	_ = json.Unmarshal(body, &r)
	return r
}

// expectRefused fails t unless status and body are 403 MEMBERSHIP_HELD naming person.
func expectRefused(t *testing.T, what, person string, status int, body []byte) {
	t.Helper()
	if r := refusal(body); status != http.StatusForbidden || r.Code != "MEMBERSHIP_HELD" || r.Error != heldWords(person) {
		t.Errorf("%s answered %d %s, want 403 MEMBERSHIP_HELD saying %q", what, status, body, heldWords(person))
	}
}

// expectUncheckable fails t unless status and body are 503 HOLD_UNCHECKABLE naming person and the
// refusal of the broker's account.
func expectUncheckable(t *testing.T, what, person string, status int, body []byte) {
	t.Helper()
	if r := refusal(body); status != http.StatusServiceUnavailable || r.Code != "HOLD_UNCHECKABLE" || r.Error != refusedAccountWords(person) {
		t.Errorf("%s answered %d %s, want 503 HOLD_UNCHECKABLE saying %q", what, status, body, refusedAccountWords(person))
	}
}

// expectServed fails t unless status is one of the 2xx a served call answers.
func expectServed(t *testing.T, what string, status int, body []byte) {
	t.Helper()
	if status/100 != 2 {
		t.Errorf("%s answered %d %s, want it served", what, status, body)
	}
}

// expectOAuthRefusal fails t unless err is the token endpoint's refusal status, code, with a
// description holding says.
func expectOAuthRefusal(t *testing.T, what string, err error, status int, code, says string) {
	t.Helper()
	var refused *oauth2.RetrieveError
	if !errors.As(err, &refused) {
		t.Errorf("%s answered %v, want %d %s", what, err, status, code)
		return
	}
	if refused.Response.StatusCode != status || refused.ErrorCode != code || !strings.Contains(refused.ErrorDescription, says) {
		t.Errorf("%s answered %d %s %q, want %d %s saying %q", what, refused.Response.StatusCode, refused.ErrorCode, refused.ErrorDescription, status, code, says)
	}
}

// expectRecordNames fails t unless the broker wrote one record of the refusal whose answer body
// is, naming the caller class and id: a hold refuses a caller its credential proved, so its
// record names that caller rather than an unauthenticated one.
func expectRecordNames(t *testing.T, capture *logtest.Recorder, what string, body []byte, class, id string) {
	t.Helper()
	var refused struct {
		AnswerID string `json:"answer_id"`
	}
	if err := json.Unmarshal(body, &refused); err != nil || refused.AnswerID == "" {
		t.Errorf("the refusal of %s carries no answer id: %s", what, body)
		return
	}
	var named [][2]any
	for _, fields := range refusalRecords(capture, refused.AnswerID) {
		named = append(named, [2]any{fields["caller_class"], fields["caller"]})
	}
	if len(named) != 1 || named[0] != [2]any{class, id} {
		t.Errorf("the records of the refusal of %s name the callers %v, want one naming %s %s", what, named, class, id)
	}
}

// TestHeldSessionRouteIsRefused: a session whose enrollment names a person Dispatch holds is
// refused 403 MEMBERSHIP_HELD on a session route, before the route runs, and the broker's record of
// the refusal names the session; bob's is served. While Dispatch refuses the broker's own account
// every such session is 503 HOLD_UNCHECKABLE, and a pod's, whose enrollment names no person, is
// asked nothing and served.
func TestHeldSessionRouteIsRefused(t *testing.T) {
	capture := &logtest.Recorder{}
	logtest.SetDefault(t, capture)
	b := newHoldBroker(t)
	ada, adaKey := b.newSessionEnrollment(t, "session-"+t.Name()+"-ada", heldAda)
	bob, bobKey := b.newSessionEnrollment(t, "session-"+t.Name()+"-bob", servedBob)
	pod, podKey := b.newServicePod(t)
	b.hold()

	status, body := b.ask(t, ada, adaKey)
	expectRefused(t, "ada's secret request while Dispatch holds her", heldAda, status, body)
	expectRecordNames(t, capture, "ada's secret request", body, answer.CallerSession, ada)
	if n := b.count(t, `select count(*) from requests where enrollment_id = $1`, ada); n != 0 {
		t.Errorf("ada's refused secret request opened %d requests, want none: the hold is asked before the route runs", n)
	}
	status, body = b.session(t, adaKey, ada, http.MethodGet, "/v1/enrollments/self", nil)
	expectRefused(t, "ada's read of her own enrollment while Dispatch holds her", heldAda, status, body)
	status, body = b.ask(t, bob, bobKey)
	expectServed(t, "bob's secret request", status, body)

	b.refuseAccount()
	status, body = b.ask(t, ada, adaKey)
	expectUncheckable(t, "ada's secret request while Dispatch refuses the broker's account", heldAda, status, body)
	status, body = b.ask(t, bob, bobKey)
	expectUncheckable(t, "bob's secret request while Dispatch refuses the broker's account", servedBob, status, body)
	status, body = b.session(t, podKey, pod, http.MethodGet, "/v1/enrollments/self", nil)
	expectServed(t, "a pod's read of its own enrollment, which names no person, while Dispatch refuses the broker's account", status, body)
}

// TestHeldSessionRereadIsRefused: POST /v1/secrets/{name}/reread, which authenticates a session
// that signs it itself (rereadSecret), refuses a session whose enrollment names a person Dispatch
// holds 403 MEMBERSHIP_HELD, as every session route does, and the broker's record of the refusal
// names the session; bob's session is answered what the broker serves, and a caller with no proof
// is answered 202 as before, the same for every name. While Dispatch refuses the broker's own
// account, ada's session is 503 HOLD_UNCHECKABLE.
//
// Control: rereadSecret asking no hold after the proof verifies, as it did: ada's reread answers
// 200 with whether the broker serves the name, and the test fails there.
func TestHeldSessionRereadIsRefused(t *testing.T) {
	capture := &logtest.Recorder{}
	logtest.SetDefault(t, capture)
	b := newHoldBroker(t)
	ada, adaKey := b.newSessionEnrollment(t, "session-"+t.Name()+"-ada", heldAda)
	bob, bobKey := b.newSessionEnrollment(t, "session-"+t.Name()+"-bob", servedBob)
	b.hold()
	reread := func(enrollment string, key *ecdsa.PrivateKey) (int, []byte) {
		t.Helper()
		return b.session(t, key, enrollment, http.MethodPost, "/v1/secrets/WORKER_TOKEN/reread", nil)
	}

	status, body := reread(ada, adaKey)
	expectRefused(t, "ada's session's reread while Dispatch holds her", heldAda, status, body)
	expectRecordNames(t, capture, "ada's session's reread", body, answer.CallerSession, ada)
	status, body = reread(bob, bobKey)
	if got := decode[wireReread](t, body); status != http.StatusOK || got.Name != "WORKER_TOKEN" || !got.Served {
		t.Errorf("bob's session's reread answered %d %s, want 200 serving WORKER_TOKEN", status, body)
	}
	if status, body := b.req(t, http.MethodPost, "/v1/secrets/WORKER_TOKEN/reread", nil, nil); status != http.StatusAccepted {
		t.Errorf("a reread with no proof answered %d %s, want 202", status, body)
	}

	b.refuseAccount()
	status, body = reread(ada, adaKey)
	expectUncheckable(t, "ada's session's reread while Dispatch refuses the broker's account", heldAda, status, body)
}

// TestHeldLauncherRouteIsRefused: a machine login whose credential names a person Dispatch holds
// is refused 403 MEMBERSHIP_HELD on a launcher route, enrolling nothing, and the broker's record
// of the refusal names the machine login; bob's is served. While Dispatch refuses the broker's
// own account, each is 503 HOLD_UNCHECKABLE.
func TestHeldLauncherRouteIsRefused(t *testing.T) {
	capture := &logtest.Recorder{}
	logtest.SetDefault(t, capture)
	b := newHoldBroker(t)
	adaCredential, adaKey := b.mintLauncherCredential(t, heldAda, "ada-box")
	bobCredential, bobKey := b.mintLauncherCredential(t, servedBob, "bob-box")
	b.hold()
	enrol := func(credential string, key *ecdsa.PrivateKey) (int, []byte) {
		t.Helper()
		sessionThumbprint := thumbprintOf(t, newSigningKey(t))
		return b.launcher(t, key, credential, http.MethodPost, "/v1/enrollments", map[string]any{
			"kind": "session", "runtime_id": "session-" + sessionThumbprint, "thumbprint": sessionThumbprint,
		})
	}

	status, body := enrol(adaCredential, adaKey)
	expectRefused(t, "ada's enrollment while Dispatch holds her", heldAda, status, body)
	expectRecordNames(t, capture, "ada's enrollment", body, answer.CallerLauncher, adaCredential)
	if n := b.count(t, `select count(*) from enrollments where launcher_credential_id = $1`, adaCredential); n != 0 {
		t.Errorf("ada's refused enrollment made %d enrollments, want none", n)
	}
	status, body = enrol(bobCredential, bobKey)
	expectServed(t, "bob's enrollment", status, body)

	b.refuseAccount()
	status, body = enrol(adaCredential, adaKey)
	expectUncheckable(t, "ada's enrollment while Dispatch refuses the broker's account", heldAda, status, body)
	status, body = enrol(bobCredential, bobKey)
	expectUncheckable(t, "bob's enrollment while Dispatch refuses the broker's account", servedBob, status, body)
}

// upkeep is what one person's machine holds for TestHeldUpkeepIsServed to end: login with its
// token pair, a second login to revoke, a session to renew, one to delete and one holding the
// grant the operator revokes, the first session's grant and pending request.
type upkeep struct {
	login, second  heldMachine
	renewed        string
	renewedKey     *ecdsa.PrivateKey
	deleted        string
	grant, granted string
	pending        string
}

func (b *holdBroker) upkeep(t *testing.T, person string) upkeep {
	t.Helper()
	host := strings.Split(person, "@")[0]
	login := b.login(t, person, host+"-box")
	secondKey := newSigningKey(t)
	authorization := b.client.DeviceLogin(t, secondKey, person, host+"-second-box")
	second := heldMachine{person: person, key: secondKey, credential: b.client.ApproveMachineLogin(t, person, authorization.UserCode)}
	renewed, renewedKey := b.client.EnrollSession(t, login.key, login.credential, host+"-renewed")
	deleted, _ := b.client.EnrollSession(t, login.key, login.credential, host+"-deleted")
	holder, holderKey := b.client.EnrollSession(t, login.key, login.credential, host+"-holder")
	pending := b.client.AskForAgentSecret(t, renewedKey, renewed, "a request the session cancels", "DEEL_API_KEY")
	if pending.Status != http.StatusOK || pending.State != "pending" {
		t.Fatalf("%s's request for DEEL_API_KEY = %d %s, want it pending", person, pending.Status, pending.Body)
	}
	created := decode[wireCreateRequestResponse](t, []byte(pending.Body))
	return upkeep{
		login: login, second: second, renewed: renewed, renewedKey: renewedKey, deleted: deleted,
		grant:   b.client.RequestAgentSecret(t, renewedKey, renewed, "a grant the session ends", "WORKER_TOKEN"),
		granted: b.client.RequestAgentSecret(t, holderKey, holder, "a grant the operator ends", "WORKER_TOKEN"),
		pending: created.RequestID,
	}
}

// serves drives every call of u that only keeps up or ends access, each of which must be served:
// the refresh grant, the lease renewal, and each route that only ends access or lists what to end.
func (b *holdBroker) serves(t *testing.T, whose string, u upkeep) {
	t.Helper()
	if _, err := b.client.Refresh(t, u.login.key, u.login.token); err != nil {
		t.Errorf("%s's refresh grant answered %v, want it served", whose, err)
	}
	session := func(method, path string) (int, []byte) {
		return b.session(t, u.renewedKey, u.renewed, method, path, nil)
	}
	launcher := func(method, path string) (int, []byte) {
		return b.launcher(t, u.login.key, u.login.credential, method, path, nil)
	}
	for _, row := range []struct {
		what string
		call func() (int, []byte)
	}{
		{"lease renewal", func() (int, []byte) { return session(http.MethodPost, "/v1/enrollments/"+u.renewed+"/renew") }},
		{"session's revoke of its grant", func() (int, []byte) { return session(http.MethodPost, "/v1/grants/"+u.grant+"/revoke") }},
		{"session's cancel of its request", func() (int, []byte) { return session(http.MethodPost, "/v1/requests/"+u.pending+"/cancel") }},
		{"delete of an enrollment", func() (int, []byte) { return launcher(http.MethodDelete, "/v1/enrollments/"+u.deleted) }},
		{"list of the operator's machines", func() (int, []byte) { return launcher(http.MethodGet, "/v1/operator/machines") }},
		{"list of the operator's grants", func() (int, []byte) { return launcher(http.MethodGet, "/v1/operator/grants") }},
		{"operator's revoke of a grant", func() (int, []byte) { return launcher(http.MethodPost, "/v1/operator/grants/"+u.granted+"/revoke") }},
		{"operator's revoke of a machine login", func() (int, []byte) {
			return launcher(http.MethodPost, "/v1/operator/machines/"+u.second.credential+"/revoke")
		}},
		{"retire of the calling machine login", func() (int, []byte) {
			return launcher(http.MethodPost, "/v1/operator/machines/"+u.login.credential+"/retire")
		}},
	} {
		status, body := row.call()
		expectServed(t, whose+"'s "+row.what, status, body)
	}
}

// TestHeldUpkeepIsServed: a hold ends no machine login and no session, and leaves a person able to
// end or list what they hold. The machine login's refresh grant, the session's lease renewal and
// the eight routes that only end access or list what to end are each served for ada while the
// broker's own gate answers her held; and each of bob's is served while the gate cannot decide
// anyone, Dispatch refusing the broker's own account, so none of them asks the gate at all.
func TestHeldUpkeepIsServed(t *testing.T) {
	b := newHoldBroker(t)
	ada := b.upkeep(t, heldAda)
	bob := b.upkeep(t, servedBob)

	b.hold()
	if err := b.gate.Check(context.Background(), heldAda); !errors.Is(err, hold.ErrHeld) {
		t.Fatalf("the broker's gate answered ada %v, want hold.ErrHeld: the rows below would prove nothing", err)
	}
	b.serves(t, "ada", ada)

	b.refuseAccount()
	if err := b.gate.Check(context.Background(), servedBob); !errors.Is(err, hold.ErrUncheckable) {
		t.Fatalf("the broker's gate answered bob %v, want hold.ErrUncheckable: the rows below would prove nothing", err)
	}
	b.serves(t, "bob", bob)
}

// TestHeldDeviceCodeRedemptionIsRefused: the device code of a machine login a held person
// approved is refused membership_held, issuing nothing, and the same code is redeemed once
// Dispatch releases her; a login revoked after its approval gets its own refusal first; bob's is
// redeemed; and while Dispatch refuses the broker's own account, a redemption is server_error.
func TestHeldDeviceCodeRedemptionIsRefused(t *testing.T) {
	b := newHoldBroker(t)
	b.hold()
	adaKey := newSigningKey(t)
	adaLogin := b.client.DeviceLogin(t, adaKey, heldAda, "ada-box")
	b.client.ApproveMachineLogin(t, heldAda, adaLogin.UserCode)
	_, err := b.client.DeviceToken(t, adaKey, adaLogin)
	expectOAuthRefusal(t, "ada's device code while Dispatch holds her", err, http.StatusBadRequest, oauthwire.MembershipHeld, heldWords(heldAda))

	endedKey := newSigningKey(t)
	endedLogin := b.client.DeviceLogin(t, endedKey, heldAda, "ada-ended-box")
	ended := b.client.ApproveMachineLogin(t, heldAda, endedLogin.UserCode)
	if status, body := b.ui(t, http.MethodPost, "/v1/launcher-credentials/"+ended+"/revoke-by-approver", map[string]any{"approver": heldAda}); status != http.StatusOK {
		t.Fatalf("revoke ada's second machine login = %d %s", status, body)
	}
	_, err = b.client.DeviceToken(t, endedKey, endedLogin)
	expectOAuthRefusal(t, "the device code of ada's revoked login while Dispatch holds her", err, http.StatusBadRequest, "invalid_grant", "The machine login has ended.")

	bob := b.login(t, servedBob, "bob-box")
	if bob.token.AccessToken == "" {
		t.Errorf("bob's device code redeemed no access token")
	}

	b.release()
	if token, err := b.client.DeviceToken(t, adaKey, adaLogin); err != nil || token.AccessToken == "" {
		t.Errorf("ada's device code once Dispatch released her answered %v, want her token pair: the refusal ended nothing", err)
	}

	b.refuseAccount()
	bobKey := newSigningKey(t)
	bobLogin := b.client.DeviceLogin(t, bobKey, servedBob, "bob-second-box")
	b.client.ApproveMachineLogin(t, servedBob, bobLogin.UserCode)
	_, err = b.client.DeviceToken(t, bobKey, bobLogin)
	expectOAuthRefusal(t, "bob's device code while Dispatch refuses the broker's account", err, http.StatusInternalServerError, "server_error", "Dispatch refused the broker's own account token")
}

// TestHeldIntrospectionReportsHeld: introspection answers a live token that names a held person
// active and held, both a session's token, which names her as its person, and a machine login's own
// token, which names her as the login's operator; bob's answer active and not held; and while
// Dispatch refuses the broker's own account, each is 503 HOLD_UNCHECKABLE.
func TestHeldIntrospectionReportsHeld(t *testing.T) {
	b := newHoldBroker(t)
	tokens := map[string]map[string]string{}
	for _, person := range []string{heldAda, servedBob} {
		m := b.login(t, person, strings.Split(person, "@")[0]+"-box")
		tokens[person] = map[string]string{
			"machine login's own token": m.token.AccessToken,
			"session's token":           b.exchangeFor(t, m, person+"-session"),
		}
	}
	b.hold()
	for person, held := range map[string]bool{heldAda: true, servedBob: false} {
		for kind, token := range tokens[person] {
			status, answer, refused := b.introspectAs(t, token)
			if status != http.StatusOK || !answer.Active || answer.Held != held {
				t.Errorf("the introspection of %s's %s = %d %+v %+v, want 200, active and held=%t", person, kind, status, answer, refused, held)
			}
		}
	}
	b.refuseAccount()
	for _, person := range []string{heldAda, servedBob} {
		for kind, token := range tokens[person] {
			status, _, refused := b.introspectAs(t, token)
			if status != http.StatusServiceUnavailable || refused.Code != "HOLD_UNCHECKABLE" || refused.Error != refusedAccountWords(person) {
				t.Errorf("the introspection of %s's %s while Dispatch refuses the broker's account = %d %+v, want 503 HOLD_UNCHECKABLE saying %q",
					person, kind, status, refused, refusedAccountWords(person))
			}
		}
	}
}

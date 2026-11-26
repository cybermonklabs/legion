// dispatch_signin_test.go holds a Dispatch sign-in to what it may do at the broker: the helpers
// that open, decide and trade one; the refusals of its request and its decision; which routes its
// credential reaches, which trades it makes, what each refresh answers, what a revoke ends; and
// the switch that serves it, on every step. oauth_login_test.go holds its lookup, approval and
// listing beside a machine login's device grant.
package api_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/google/uuid"
	"golang.org/x/oauth2"

	"github.com/sjawhar/envoy/internal/cipher/api"
	"github.com/sjawhar/envoy/internal/cipher/ciphertest"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/cipherclient"
)

// routeParameter is a wildcard of a route's pattern, which a call of the route names a value of.
var routeParameter = regexp.MustCompile(`\{[^}]+\}`)

// signInOff is the description of every refusal of a step of a Dispatch sign-in on a broker that
// serves none (oauth.DispatchSignInOff).
const signInOff = "DISPATCH_SIGNIN_OFF: this broker serves no Dispatch sign-in: BROKER_DISPATCH_SIGNIN is off"

// dispatchSignInRequest signs the launcher credential detail of a Dispatch sign-in.
func (m *deviceMachine) dispatchSignInRequest(t *testing.T, actions []string) string {
	t.Helper()
	return m.dispatchSignInRequestFor(t, "a Dispatch session", actions)
}

// dispatchSignInRequestFor is dispatchSignInRequest signing reason as the request's reason.
func (m *deviceMachine) dispatchSignInRequestFor(t *testing.T, reason string, actions []string) string {
	t.Helper()
	compact, err := record.Sign(m.key, m.rig.URL, []record.AuthorizationDetail{{
		Type: "launcher_credential", Identifier: "example-host-devbox", Actions: actions,
	}}, reason, m.rig.Operator, nil, time.Now())
	if err != nil {
		t.Fatalf("record.Sign: %v", err)
	}
	return compact
}

// requestWithEmptyActions signs a machine login's request object whose launcher_credential detail
// carries an actions member holding the empty list, which record.Sign leaves out.
func (m *deviceMachine) requestWithEmptyActions(t *testing.T) string {
	t.Helper()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: m.key}, &jose.SignerOptions{
		EmbedJWK: true, ExtraHeaders: map[jose.HeaderKey]any{jose.HeaderType: record.RequestTyp},
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	payload, err := json.Marshal(map[string]any{
		"iss": m.thumbprint(t), "aud": m.rig.URL, "jti": uuid.NewString(), "iat": now, "exp": now + 600,
		"authorization_details": []map[string]any{{"type": "launcher_credential", "identifier": "example-host-devbox", "actions": []string{}}},
		"login_hint":            m.rig.Operator,
	})
	if err != nil {
		t.Fatal(err)
	}
	signed, err := signer.Sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	compact, err := signed.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return compact
}

// dispatchScopes is the dispatch_scopes field of a device login's token answer, failing the test
// when the answer carries none.
func dispatchScopes(t *testing.T, tok *oauth2.Token) []string {
	t.Helper()
	value := tok.Extra(oauthwire.ExtraDispatchScopes)
	if value == nil {
		t.Fatalf("token has no %q field", oauthwire.ExtraDispatchScopes)
	}
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal token %q: %v", oauthwire.ExtraDispatchScopes, err)
	}
	var scopes []string
	if err := json.Unmarshal(raw, &scopes); err != nil {
		t.Fatalf("decode token %q: %v", oauthwire.ExtraDispatchScopes, err)
	}
	return scopes
}

// openDispatchSignIn opens a Dispatch sign-in of a new machine asking for asked, its request
// object signing reason, and looks it up as its person's page does: the machine, its device
// authorization and the record the lookup answers.
func openDispatchSignIn(t *testing.T, rig *ciphertest.Rig, reason string, asked []string) (*deviceMachine, *oauth2.DeviceAuthResponse, wireRecord) {
	t.Helper()
	m := newDeviceMachine(t, rig)
	m.config.Scopes = []string{oauthwire.ScopeDispatch}
	da, err := m.authorize(m.dispatchSignInRequestFor(t, reason, asked), m.thumbprint(t))
	if err != nil {
		t.Fatalf("Dispatch device authorization: %v", err)
	}
	status, body := rig.UI(t, http.MethodPost, "/v1/machine-logins/lookup", map[string]any{"code": da.UserCode})
	if status != http.StatusOK {
		t.Fatalf("Dispatch sign-in lookup = %d: %s", status, body)
	}
	return m, da, decode[wireRecord](t, body)
}

// approvedDispatchSignIn logs a machine in as a Dispatch sign-in asking for asked, has the rig's
// operator approve it with approved (an empty, non-nil slice approves no scope), and polls its
// tokens: the machine, its first token answer and the credential the approval minted.
func approvedDispatchSignIn(t *testing.T, rig *ciphertest.Rig, asked, approved []string) (*deviceMachine, *oauth2.Token, string) {
	t.Helper()
	return approvedDispatchSignInFor(t, rig, "a Dispatch session", asked, approved)
}

// approvedDispatchSignInFor is approvedDispatchSignIn with its request object signing reason.
func approvedDispatchSignInFor(t *testing.T, rig *ciphertest.Rig, reason string, asked, approved []string) (*deviceMachine, *oauth2.Token, string) {
	t.Helper()
	m, da, looked := openDispatchSignIn(t, rig, reason, asked)
	credential := approveSignIn(t, rig, rig.URL, looked.RecordID, da.UserCode, approved)
	return m, m.poll(t, da), credential
}

// approveSignIn has the rig's operator approve the sign-in recordID at base, a broker's address,
// with scopes, failing the test unless a credential is minted, and answers its id.
func approveSignIn(t *testing.T, rig *ciphertest.Rig, base, recordID, code string, scopes []string) string {
	t.Helper()
	status, body := decideAt(t, rig, base, recordID, "approve", map[string]any{"approver": rig.Operator, "code": code, "scopes": scopes})
	credential := decode[struct {
		CredentialID string `json:"credential_id"`
	}](t, body).CredentialID
	if status != http.StatusOK || credential == "" {
		t.Fatalf("approve the Dispatch sign-in at %s = %d %s, want 200 with a credential id", base, status, body)
	}
	return credential
}

// decideAt sends decision ("approve" or "deny") of recordID with body to base, a broker's address,
// as Dispatch's server relays one with the rig's own ServiceAccount token.
func decideAt(t *testing.T, rig *ciphertest.Rig, base, recordID, decision string, body map[string]any) (int, []byte) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	got := send(t, http.DefaultClient, request(t, http.MethodPost, base+"/v1/credential-requests/"+recordID+"/"+decision, bytes.NewReader(raw),
		map[string]string{"Authorization": "Bearer " + rig.UIToken, "Content-Type": "application/json"}))
	return got.status, got.body
}

// at is m with its token endpoint at base, a broker's address: its polls and refreshes go there,
// each with a DPoP proof of that address.
func (m *deviceMachine) at(base string) *deviceMachine {
	config := *m.config
	config.Endpoint.TokenURL = base + "/v1/oauth/token"
	moved := *m
	moved.config = &config
	return &moved
}

// sessionName is the session this machine's trades name, its key's thumbprint telling it from
// every other machine's: a machine login and each Dispatch sign-in are principals of their own
// (oauthwire.ConsentedPrincipal), and one session name belongs to one principal
// (oauthstore.Store.BindSession), so two machines of one person trading under one name would be
// refused the second trade. A machine moved to another address (at) keeps its key, so it keeps
// its name.
func (m *deviceMachine) sessionName(t *testing.T) string {
	t.Helper()
	return "device-session-" + m.thumbprint(t)
}

// trade trades token, a device login's access token, at base for a token of this machine's own
// session (sessionName), with scope and header as tradeAt sends them and the machine's own DPoP
// proof, and answers the broker's answer with its body read as a JSON object.
func (m *deviceMachine) trade(t *testing.T, base, token, scope string, header map[string]string) (int, map[string]any) {
	t.Helper()
	got := tradeAt(t, base, cipherclient.NewDPoPTransport(m.key, http.DefaultTransport), token, m.sessionName(t), scope, header)
	var body map[string]any
	if err := json.Unmarshal(got.body, &body); err != nil {
		t.Fatalf("the trade's answer %s is not a JSON object: %v", got.body, err)
	}
	return got.status, body
}

// traded trades token at the rig's own address, failing the test unless the broker mints a token,
// and answers the token and what introspection says of it.
func (m *deviceMachine) traded(t *testing.T, token string) (string, wireIntrospection) {
	t.Helper()
	status, body := m.trade(t, m.rig.URL, token, "", nil)
	minted, _ := body["access_token"].(string)
	if status != http.StatusOK || minted == "" {
		t.Fatalf("trade = %d %v, want 200 with an access token", status, body)
	}
	status, answer := introspect(t, m.rig, minted, m.rig.UIToken)
	if status != http.StatusOK {
		t.Fatalf("introspect the traded token = %d %+v", status, answer)
	}
	return minted, answer
}

// requireConsented fails t unless answer is a live consented token for the rig's operator holding
// exactly scope.
func requireConsented(t *testing.T, what string, rig *ciphertest.Rig, answer wireIntrospection, scope string) {
	t.Helper()
	if !answer.Active || answer.ProjectScope != oauthwire.ProjectScopeConsented || answer.Scope != scope || answer.Person != rig.Operator {
		t.Fatalf("%s = %+v, want a live consented token for %s holding scope %q", what, answer, rig.Operator, scope)
	}
}

// countRows is how many rows `select count(*) from <from>` counts on rig's database, args filling
// the clause's parameters.
func countRows(t *testing.T, rig *ciphertest.Rig, from string, args ...any) int {
	t.Helper()
	var n int
	if err := rig.Store.Pool.QueryRow(t.Context(), `select count(*) from `+from, args...).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", from, err)
	}
	return n
}

// launcherRouteCall calls the launcher route pattern names under credential with a launcher proof
// the machine's key signs (launcherCall), each of the pattern's wildcards filled with a fresh UUID
// and an empty JSON object as the body of a POST.
func launcherRouteCall(t *testing.T, rig *ciphertest.Rig, machine *deviceMachine, credential, method, pattern string) (int, []byte) {
	t.Helper()
	path := routeParameter.ReplaceAllStringFunc(pattern, func(string) string { return uuid.NewString() })
	var body any
	if method == http.MethodPost {
		body = map[string]any{}
	}
	return launcherCall(t, rig, machine.key, credential, method, path, body)
}

// TestADispatchSignInRefusesScopesOutsideItsConsent holds a sign-in's request and decision to its
// consent at their public boundaries: each refusal answers its own code and words and mints
// nothing, and a refused decision leaves its record pending. Each row is a permissive branch
// away from minting a credential or a record where it must leave none.
func TestADispatchSignInRefusesScopesOutsideItsConsent(t *testing.T) {
	// refusedWithoutLogin fails t unless rig holds no credential and no machine-login record.
	refusedWithoutLogin := func(t *testing.T, rig *ciphertest.Rig) {
		t.Helper()
		if n, m := countRows(t, rig, "launcher_credentials"), countRows(t, rig, "credential_requests"); n != 0 || m != 0 {
			t.Fatalf("after the refusal rig holds %d credentials and %d records, want none", n, m)
		}
	}
	// refusedPending fails t unless the record is still pending and no credential was minted.
	refusedPending := func(t *testing.T, rig *ciphertest.Rig, recordID string) {
		t.Helper()
		if n := countRows(t, rig, "launcher_credentials"); n != 0 {
			t.Fatalf("launcher credentials after the refusal = %d, want 0", n)
		}
		if n := countRows(t, rig, "credential_request_events where record_id = $1", recordID); n != 0 {
			t.Fatalf("decisions of the record after the refusal = %d, want 0: it stays pending", n)
		}
	}
	requestRefused := func(t *testing.T, err error, code, description string) {
		t.Helper()
		if gotCode, got := oauthErrorCode(t, err); gotCode != code || got != description {
			t.Fatalf("the device authorization = %s %q, want %s %q", gotCode, got, code, description)
		}
	}

	t.Run("a non-consentable requested action", func(t *testing.T) {
		rig := ciphertest.NewRig(t)
		m := newDeviceMachine(t, rig)
		m.config.Scopes = []string{oauthwire.ScopeDispatch}
		_, err := m.authorize(m.dispatchSignInRequest(t, []string{oauthwire.ScopeApprove}), m.thumbprint(t))
		requestRefused(t, err, "invalid_scope", `SCOPE_NOT_CONSENTABLE: the scope "approve" is not consentable; a Dispatch sign-in may approve only person, p0`)
		refusedWithoutLogin(t, rig)
	})

	// D6: a machine login carries no actions member at all, not even the empty list.
	for name, request := range map[string]func(m *deviceMachine) string{
		"a machine login asking a scope":             func(m *deviceMachine) string { return m.dispatchSignInRequest(t, []string{oauthwire.ScopePerson}) },
		"a machine login with an empty actions list": func(m *deviceMachine) string { return m.requestWithEmptyActions(t) },
	} {
		t.Run(name, func(t *testing.T) {
			rig := ciphertest.NewRig(t)
			m := newDeviceMachine(t, rig)
			_, err := m.authorize(request(m), m.thumbprint(t))
			requestRefused(t, err, "invalid_request", "ACTIONS_ON_MACHINE_LOGIN: a machine login's launcher_credential detail carries no actions member, not even an empty list")
			refusedWithoutLogin(t, rig)
			// The control: the same broker opens a machine login whose detail names no actions.
			newDeviceMachine(t, rig).login(t)
		})
	}

	// A scope the device client is not registered for the library refuses in its own words before
	// the broker reads the form, so the broker's own rule is reached by the two scopes together.
	for name, scopes := range map[string][]string{
		"both device scopes": {oauthwire.Scope, oauthwire.ScopeDispatch},
	} {
		t.Run(name, func(t *testing.T) {
			rig := ciphertest.NewRig(t)
			m := newDeviceMachine(t, rig)
			m.config.Scopes = scopes
			_, err := m.authorize(m.request(t, rig.Operator, ""), m.thumbprint(t))
			requestRefused(t, err, "invalid_scope", "SCOPE_INVALID: a device login names exactly one scope, machine or dispatch")
			refusedWithoutLogin(t, rig)
		})
	}

	decisions := []struct {
		name, decision string
		asked          []string
		body           func(rig *ciphertest.Rig, code string) map[string]any
		status         int
		code, words    string
	}{
		{"approval without scopes", "approve", []string{oauthwire.ScopePerson},
			func(rig *ciphertest.Rig, code string) map[string]any {
				return map[string]any{"approver": rig.Operator, "code": code}
			}, http.StatusBadRequest, "SCOPES_REQUIRED", "a Dispatch sign-in approval requires scopes"},
		{"approval of a scope not asked", "approve", nil,
			func(rig *ciphertest.Rig, code string) map[string]any {
				return map[string]any{"approver": rig.Operator, "code": code, "scopes": []string{oauthwire.ScopePerson}}
			}, http.StatusBadRequest, "SCOPE_NOT_ASKED", "the decision names a scope the sign-in did not ask for"},
		{"approval of a scope no sign-in holds", "approve", []string{oauthwire.ScopePerson},
			func(rig *ciphertest.Rig, code string) map[string]any {
				return map[string]any{"approver": rig.Operator, "code": code, "scopes": []string{oauthwire.ScopeApprove}}
			}, http.StatusBadRequest, "SCOPE_NOT_CONSENTABLE", `the scope "approve" is not consentable; a Dispatch sign-in may approve only person, p0`},
		{"a denial carrying scopes", "deny", []string{oauthwire.ScopePerson},
			func(rig *ciphertest.Rig, code string) map[string]any {
				return map[string]any{"approver": rig.Operator, "code": code, "scopes": []string{oauthwire.ScopePerson}}
			}, http.StatusBadRequest, "SCOPES_ON_DENIAL", "a denial takes no scopes"},
		{"a denial carrying the empty list", "deny", []string{oauthwire.ScopePerson},
			func(rig *ciphertest.Rig, code string) map[string]any {
				return map[string]any{"approver": rig.Operator, "code": code, "scopes": []string{}}
			}, http.StatusBadRequest, "SCOPES_ON_DENIAL", "a denial takes no scopes"},
	}
	for _, c := range decisions {
		t.Run(c.name, func(t *testing.T) {
			rig := ciphertest.NewRig(t)
			_, da, looked := openDispatchSignIn(t, rig, "a Dispatch session", c.asked)
			status, body := decideAt(t, rig, rig.URL, looked.RecordID, c.decision, c.body(rig, da.UserCode))
			if refusal := decode[wireError](t, body); status != c.status || refusal.Code != c.code || refusal.Error != c.words {
				t.Fatalf("%s = %d %+v, want %d %s %q", c.name, status, refusal, c.status, c.code, c.words)
			}
			refusedPending(t, rig, looked.RecordID)
		})
	}

	t.Run("the control: a denial carrying no scopes is decided", func(t *testing.T) {
		rig := ciphertest.NewRig(t)
		_, da, looked := openDispatchSignIn(t, rig, "a Dispatch session", []string{oauthwire.ScopePerson})
		if status, body := decideAt(t, rig, rig.URL, looked.RecordID, "deny", map[string]any{"approver": rig.Operator, "code": da.UserCode}); status != http.StatusOK {
			t.Fatalf("a denial with no scopes = %d %s, want 200", status, body)
		}
	})

	// A machine login's lookup answers dispatch_scopes null, and any scopes on its approval, the
	// empty list included, are scopes it never asked for; on its denial they are a denial's.
	for name, scopes := range map[string][]string{"scopes": {oauthwire.ScopePerson}, "an empty scope list": {}} {
		t.Run("a machine login decided with "+name, func(t *testing.T) {
			rig := ciphertest.NewRig(t)
			da := newDeviceMachine(t, rig).login(t)
			status, body := rig.UI(t, http.MethodPost, "/v1/machine-logins/lookup", map[string]any{"code": da.UserCode})
			if status != http.StatusOK {
				t.Fatalf("lookup = %d %s", status, body)
			}
			looked := decode[wireRecord](t, body)
			if looked.DispatchScopes != nil {
				t.Fatalf("a machine login's lookup dispatch_scopes = %v, want null", *looked.DispatchScopes)
			}
			for decision, want := range map[string][2]string{
				"approve": {"SCOPE_NOT_ASKED", "a machine login takes no scopes"},
				"deny":    {"SCOPES_ON_DENIAL", "a denial takes no scopes"},
			} {
				status, body := decideAt(t, rig, rig.URL, looked.RecordID, decision, map[string]any{"approver": rig.Operator, "code": da.UserCode, "scopes": scopes})
				if refusal := decode[wireError](t, body); status != http.StatusBadRequest || refusal.Code != want[0] || refusal.Error != want[1] {
					t.Fatalf("%s of a machine login with %s = %d %+v, want 400 %s %q", decision, name, status, refusal, want[0], want[1])
				}
			}
			refusedPending(t, rig, looked.RecordID)
		})
	}
}

// TestTurningTheDispatchSignInOffStopsEveryStepButADenial: a replica started with
// BROKER_DISPATCH_SIGNIN unset beside one with it on, on one database, as the setting's rollback
// rolls them, refuses each step of a Dispatch sign-in the other opened or approved, with
// DISPATCH_SIGNIN_OFF and its one sentence: the approval of a pending sign-in (400, nothing minted,
// the record still pending), the device code grant of an approved one, its refresh and its trade
// (400 invalid_scope, nothing issued). A denial is decided there, since it grants nothing. Its
// metadata names no dispatch scope. Control: on that replica a machine login is approved, polled,
// refreshed and traded as ever, and on the replica with the setting on the same sign-in refreshes
// and trades.
//
// The named reverts: drop the gate from ApplyDecision, from the token hooks or from the trade, and
// the replica approves, refreshes or trades the sign-in.
func TestTurningTheDispatchSignInOffStopsEveryStepButADenial(t *testing.T) {
	rig := ciphertest.NewRig(t)
	replica := rig.ReplicaWithoutDispatchSignIn(t)

	for base, want := range map[string][]string{rig.URL: {oauthwire.Scope, oauthwire.ScopeDispatch}, replica: {oauthwire.Scope}} {
		got := send(t, http.DefaultClient, request(t, http.MethodGet, base+"/.well-known/oauth-authorization-server", nil, nil))
		if scopes := decode[struct {
			ScopesSupported []string `json:"scopes_supported"`
		}](t, got.body).ScopesSupported; got.status != http.StatusOK || !slices.Equal(scopes, want) {
			t.Errorf("%s's metadata scopes_supported = %d %v, want %v", base, got.status, scopes, want)
		}
	}

	// A sign-in opened where sign-ins are served, approved there, holding its first tokens.
	m, tok, _ := approvedDispatchSignIn(t, rig, []string{oauthwire.ScopePerson}, []string{oauthwire.ScopePerson})

	_, pendingCode, looked := openDispatchSignIn(t, rig, "a sign-in left pending", []string{oauthwire.ScopePerson})
	status, body := decideAt(t, rig, replica, looked.RecordID, "approve", map[string]any{"approver": rig.Operator, "code": pendingCode.UserCode, "scopes": []string{oauthwire.ScopePerson}})
	if refusal := decode[wireError](t, body); status != http.StatusBadRequest || refusal.Code != "DISPATCH_SIGNIN_OFF" || refusal.Error != strings.TrimPrefix(signInOff, "DISPATCH_SIGNIN_OFF: ") {
		t.Fatalf("the replica's approval of a pending sign-in = %d %+v, want 400 DISPATCH_SIGNIN_OFF", status, refusal)
	}
	if n := countRows(t, rig, "credential_request_events where record_id = $1", looked.RecordID); n != 0 {
		t.Fatalf("the refused approval decided the record (%d events), want it pending", n)
	}

	// Approved where sign-ins are served, its device code is polled where they are not.
	unpolledMachine, unpolled, unpolledRecord := openDispatchSignIn(t, rig, "a sign-in not yet polled", []string{oauthwire.ScopePerson})
	approveSignIn(t, rig, rig.URL, unpolledRecord.RecordID, unpolled.UserCode, []string{oauthwire.ScopePerson})
	unpolled.Interval = 1
	if _, err := unpolledMachine.at(replica).config.DeviceAccessToken(unpolledMachine.ctx, unpolled); err == nil {
		t.Fatal("the replica answered the device code grant of an approved sign-in, want DISPATCH_SIGNIN_OFF")
	} else if code, description := oauthErrorCode(t, err); code != "invalid_scope" || description != signInOff {
		t.Fatalf("the replica's device code grant of a sign-in = %s %q, want invalid_scope %q", code, description, signInOff)
	}

	if _, err := m.at(replica).refresh(tok); err == nil {
		t.Fatal("the replica refreshed a Dispatch sign-in, want DISPATCH_SIGNIN_OFF")
	} else if code, description := oauthErrorCode(t, err); code != "invalid_scope" || description != signInOff {
		t.Fatalf("the replica's refresh of a sign-in = %s %q, want invalid_scope %q", code, description, signInOff)
	}
	tokens := countRows(t, rig, "oauth_access_tokens")
	if status, body := m.trade(t, replica, tok.AccessToken, "", nil); status != http.StatusBadRequest || body["error"] != "invalid_scope" ||
		body["error_description"] != signInOff || body["access_token"] != nil {
		t.Fatalf("the replica's trade of a sign-in = %d %v, want 400 invalid_scope %q and no token", status, body, signInOff)
	}
	if n := countRows(t, rig, "oauth_access_tokens"); n != tokens {
		t.Fatalf("the refused trade stored %d access tokens beside the %d before it, want none more", n, tokens)
	}

	if status, body := decideAt(t, rig, replica, looked.RecordID, "deny", map[string]any{"approver": rig.Operator, "code": pendingCode.UserCode}); status != http.StatusOK {
		t.Fatalf("the replica's denial of a pending sign-in = %d %s, want 200: a denial grants nothing", status, body)
	}

	// The controls: the replica serves a machine login whole, and the sign-in still refreshes and
	// trades where sign-ins are served.
	login := newDeviceMachine(t, rig)
	da := login.login(t)
	status, body = rig.UI(t, http.MethodPost, "/v1/machine-logins/lookup", map[string]any{"code": da.UserCode})
	if status != http.StatusOK {
		t.Fatalf("control: the machine login's lookup = %d %s", status, body)
	}
	if status, body := decideAt(t, rig, replica, decode[wireRecord](t, body).RecordID, "approve", map[string]any{"approver": rig.Operator, "code": da.UserCode}); status != http.StatusOK {
		t.Fatalf("control: the replica's approval of a machine login = %d %s, want 200", status, body)
	}
	loginToken := login.at(replica).poll(t, da)
	refreshed, err := login.at(replica).refresh(loginToken)
	if err != nil {
		t.Fatalf("control: the replica's refresh of a machine login: %v", err)
	}
	if status, body := login.trade(t, replica, refreshed.AccessToken, "", nil); status != http.StatusOK || body["access_token"] == nil {
		t.Fatalf("control: the replica's trade of a machine login = %d %v, want a token", status, body)
	}
	again, err := m.refresh(tok)
	if err != nil {
		t.Fatalf("control: the refresh of the sign-in where sign-ins are served: %v", err)
	}
	_, answer := m.traded(t, again.AccessToken)
	requireConsented(t, "control: the trade of the sign-in where sign-ins are served", rig, answer, oauthwire.ScopePerson)
	if _, err := unpolledMachine.config.DeviceAccessToken(unpolledMachine.ctx, unpolled); err != nil {
		t.Fatalf("control: the device code grant of the same sign-in where sign-ins are served: %v", err)
	}
}

// TestADispatchSignInsCredentialAuthenticatesNoLauncherRoute: a Dispatch sign-in's credential is no
// launcher's, so a launcher proof its own key signs naming it is 401 LAUNCHER_INVALID on every route
// the broker's table serves behind launcher authentication, read from the table itself
// (api.LauncherRoutes), and changes nothing: no enrollment is made and no credential revoked. The
// same calls with a machine login's credential, approved by the same person on the same broker,
// are each answered past 401, so what refused the sign-in was its credential.
//
// The named revert: let AuthenticateLauncher take a credential whose dispatch_scopes is set, and
// every row of the sign-in is answered past 401.
func TestADispatchSignInsCredentialAuthenticatesNoLauncherRoute(t *testing.T) {
	routes := api.LauncherRoutes()
	if len(routes) == 0 {
		t.Fatal("the route table lists no launcher route, so this test would call nothing")
	}
	rig := ciphertest.NewRig(t)
	signIn, _, signInCredential := approvedDispatchSignIn(t, rig, []string{oauthwire.ScopePerson}, []string{oauthwire.ScopePerson})
	login, _, loginCredential := approvedLogin(t, rig)
	state := func() string {
		t.Helper()
		return fmt.Sprintf("%d enrollments, %d revoked credentials", countRows(t, rig, "enrollments"), countRows(t, rig, "launcher_credentials where revoked_at is not null"))
	}

	before := state()
	for _, route := range routes {
		status, body := launcherRouteCall(t, rig, signIn, signInCredential, route[0], route[1])
		if status != http.StatusUnauthorized || decode[wireError](t, body).Code != "LAUNCHER_INVALID" {
			t.Errorf("%s %s under a Dispatch sign-in = %d %s, want 401 LAUNCHER_INVALID", route[0], route[1], status, body)
		}
	}
	if after := state(); after != before {
		t.Errorf("the refused calls changed the broker: before %s, after %s", before, after)
	}
	for _, route := range routes {
		if status, body := launcherRouteCall(t, rig, login, loginCredential, route[0], route[1]); status == http.StatusUnauthorized {
			t.Errorf("%s %s under a machine login = %d %s, want an answer past 401", route[0], route[1], status, body)
		}
	}
}

// TestADispatchSignInHasNoReadTrade: a Dispatch sign-in's token is minted whole with the scopes
// its person approved, so its read trade is refused 400 invalid_request naming the sign-in even
// with a DPoP-Holder proof by a key of its own, and mints no read token. Control: a machine login
// approved by the same person on the same broker, trading for read with a holder key of the same
// kind, is minted a read-person token.
//
// The named revert: drop the read refusal of a consented claim, and the sign-in's trade is minted.
func TestADispatchSignInHasNoReadTrade(t *testing.T) {
	rig := ciphertest.NewRig(t)
	tokenURL := rig.URL + "/v1/oauth/token"
	signIn, signInToken, _ := approvedDispatchSignIn(t, rig, []string{oauthwire.ScopePerson}, []string{oauthwire.ScopePerson})
	login, loginToken, _ := approvedLogin(t, rig)

	status, body := signIn.trade(t, rig.URL, signInToken.AccessToken, oauthwire.ScopeRead,
		map[string]string{oauthwire.HeaderDPoPHolder: signedDPoPProof(t, newSigningKey(t), tokenURL)})
	description, _ := body["error_description"].(string)
	if status != http.StatusBadRequest || body["error"] != "invalid_request" || body["access_token"] != nil || !strings.Contains(description, "Dispatch sign-in") {
		t.Fatalf("a Dispatch sign-in's read trade = %d %v, want 400 invalid_request naming the Dispatch sign-in", status, body)
	}
	if n := countRows(t, rig, "oauth_narrowed_access_tokens"); n != 0 {
		t.Fatalf("the refused read trade left %d read tokens, want none", n)
	}

	status, body = login.trade(t, rig.URL, loginToken.AccessToken, oauthwire.ScopeRead,
		map[string]string{oauthwire.HeaderDPoPHolder: signedDPoPProof(t, newSigningKey(t), tokenURL)})
	minted, _ := body["access_token"].(string)
	if status != http.StatusOK || minted == "" {
		t.Fatalf("control: a machine login's read trade = %d %v, want 200 with a token", status, body)
	}
	if status, answer := introspect(t, rig, minted, rig.UIToken); status != http.StatusOK || !answer.Active ||
		answer.ProjectScope != oauthwire.ProjectScopeReadPerson || answer.Person != rig.Operator {
		t.Fatalf("control: the machine login's read token = %d %+v, want a live read-person token for %s", status, answer, rig.Operator)
	}
}

// TestEachRefreshOfADispatchSignInIsADispatchSignIn: the device code answer and each of two
// refreshes after it carry the scope dispatch and the approved dispatch_scopes, and the access
// token of each refresh trades for a consented token holding the approved scope and naming, as its
// sign-in, the credential the approval minted, so a refresh keeps the sign-in's principal
// (oauthwire.ConsentedPrincipal). Control: a machine login refreshed twice on the same broker
// carries the scope machine and no dispatch_scopes.
//
// The named reverts: grant the scope machine on a Dispatch sign-in's approval, or leave
// dispatch_scopes out of the refresh grant's answer, and a row fails naming the answer; answer a
// consented token's sign_in as anything but its credential, and a refresh's row fails naming it.
func TestEachRefreshOfADispatchSignInIsADispatchSignIn(t *testing.T) {
	rig := ciphertest.NewRig(t)
	m, tok, credential := approvedDispatchSignIn(t, rig, []string{oauthwire.ScopePerson}, []string{oauthwire.ScopePerson})
	requireAnswer := func(what string, tok *oauth2.Token) {
		t.Helper()
		if scope, _ := tok.Extra("scope").(string); scope != oauthwire.ScopeDispatch {
			t.Fatalf("%s scope = %v, want %q", what, tok.Extra("scope"), oauthwire.ScopeDispatch)
		}
		if got := dispatchScopes(t, tok); !slices.Equal(got, []string{oauthwire.ScopePerson}) {
			t.Fatalf("%s dispatch_scopes = %v, want [person]", what, got)
		}
	}
	requireAnswer("the device code answer", tok)
	for i := 1; i <= 2; i++ {
		next, err := m.refresh(tok)
		if err != nil {
			t.Fatalf("refresh %d of the Dispatch sign-in: %v", i, err)
		}
		requireAnswer(fmt.Sprintf("refresh %d", i), next)
		_, answer := m.traded(t, next.AccessToken)
		requireConsented(t, fmt.Sprintf("the trade of refresh %d", i), rig, answer, oauthwire.ScopePerson)
		if answer.SignIn != credential {
			t.Fatalf("the trade of refresh %d names the sign-in %q, want %q, the credential the approval minted", i, answer.SignIn, credential)
		}
		tok = next
	}

	login, loginToken, _ := approvedLogin(t, rig)
	for i := 1; i <= 2; i++ {
		next, err := login.refresh(loginToken)
		if err != nil {
			t.Fatalf("control: refresh %d of the machine login: %v", i, err)
		}
		if scope, _ := next.Extra("scope").(string); scope != oauthwire.Scope || next.Extra(oauthwire.ExtraDispatchScopes) != nil {
			t.Fatalf("control: refresh %d of the machine login = scope %v, dispatch_scopes %v; want %q and none",
				i, next.Extra("scope"), next.Extra(oauthwire.ExtraDispatchScopes), oauthwire.Scope)
		}
		loginToken = next
	}
}

// TestRevokingADispatchSignInEndsItsTokens: before its approver revokes it, a Dispatch sign-in's
// traded token introspects live, its refresh token is answered and a refreshed access token
// trades. Once its approver revokes it, the traded token introspects not active, the refresh
// token is refused invalid_grant, and the access token's trade is refused and mints nothing.
//
// The named revert: end no OAuth token in the credential's revoke, and the traded token stays live.
func TestRevokingADispatchSignInEndsItsTokens(t *testing.T) {
	rig := ciphertest.NewRig(t)
	m, tok, credential := approvedDispatchSignIn(t, rig, []string{oauthwire.ScopePerson}, []string{oauthwire.ScopePerson})
	exchanged, answer := m.traded(t, tok.AccessToken)
	requireConsented(t, "before the revoke, the traded token", rig, answer, oauthwire.ScopePerson)
	refreshed, err := m.refresh(tok)
	if err != nil {
		t.Fatalf("before the revoke, refresh: %v", err)
	}
	m.traded(t, refreshed.AccessToken)

	if status, body := rig.UI(t, http.MethodPost, "/v1/launcher-credentials/"+credential+"/revoke-by-approver",
		map[string]any{"approver": rig.Operator}); status != http.StatusOK {
		t.Fatalf("revoke the Dispatch sign-in = %d: %s", status, body)
	}
	if status, answer := introspect(t, rig, exchanged, rig.UIToken); status != http.StatusOK || answer.Active {
		t.Fatalf("after the revoke, the traded token = %d %+v, want not active", status, answer)
	}
	if _, err := m.refresh(refreshed); err == nil {
		t.Fatal("after the revoke, the refresh token is answered, want invalid_grant")
	} else if code, _ := oauthErrorCode(t, err); code != "invalid_grant" {
		t.Fatalf("after the revoke, the refresh = %s, want invalid_grant", code)
	}
	if status, body := m.trade(t, rig.URL, refreshed.AccessToken, "", nil); status == http.StatusOK || body["access_token"] != nil {
		t.Fatalf("after the revoke, the access token's trade = %d %v, want a refusal minting nothing", status, body)
	}
}

// TestADispatchSignInApprovedWithNoScopeTradesForAnEmptyConsent: a person may approve a Dispatch
// sign-in with none of the scopes it asked for. The token answer's dispatch_scopes is the empty
// list, and the trade mints a consented token whose scope is empty, not a person-scoped one.
//
// The named revert: store no dispatch_scopes for an empty approval, and the trade mints a
// person-scoped token.
func TestADispatchSignInApprovedWithNoScopeTradesForAnEmptyConsent(t *testing.T) {
	rig := ciphertest.NewRig(t)
	m, tok, _ := approvedDispatchSignIn(t, rig, []string{oauthwire.ScopePerson}, []string{})
	if got := dispatchScopes(t, tok); len(got) != 0 {
		t.Fatalf("dispatch_scopes = %v, want the empty list", got)
	}
	_, answer := m.traded(t, tok.AccessToken)
	requireConsented(t, "the traded token", rig, answer, "")
}

// TestADispatchSignInAskingP0TradesForTheScopesItsPersonTicked: p0 is a scope a person may tick
// for a Dispatch sign-in (dispatch://INFRA-5943/ask/3dfbf22b-74af-4e6d-af17-3bf368aac9fa). A
// sign-in that asks for person and p0, approved with both, answers both as its dispatch_scopes,
// sorted, and trades for a consented token holding both; the same sign-in approved with person
// alone, its person leaving p0 unticked, answers and trades for person alone.
//
// The named reverts: take p0 out of the consentable list, and the request is refused
// SCOPE_NOT_CONSENTABLE; grant a sign-in its asked scopes in place of its approved ones, and the
// row approved with person alone holds p0.
func TestADispatchSignInAskingP0TradesForTheScopesItsPersonTicked(t *testing.T) {
	asked := []string{oauthwire.ScopeP0, oauthwire.ScopePerson}
	for _, row := range []struct {
		name     string
		approved []string
		scope    string
	}{
		{"person and p0 ticked", []string{oauthwire.ScopePerson, oauthwire.ScopeP0}, oauthwire.ScopeP0 + " " + oauthwire.ScopePerson},
		{"person alone ticked", []string{oauthwire.ScopePerson}, oauthwire.ScopePerson},
	} {
		t.Run(row.name, func(t *testing.T) {
			rig := ciphertest.NewRig(t)
			m, tok, _ := approvedDispatchSignIn(t, rig, asked, row.approved)
			want := slices.Sorted(slices.Values(row.approved))
			if got := dispatchScopes(t, tok); !slices.Equal(got, want) {
				t.Fatalf("dispatch_scopes = %v, want %v", got, want)
			}
			_, answer := m.traded(t, tok.AccessToken)
			requireConsented(t, "the traded token", rig, answer, row.scope)
		})
	}
}

// TestABrokerWithTheDispatchSignInOffOpensNone: a broker started with BROKER_DISPATCH_SIGNIN unset
// refuses a device authorization naming the scope dispatch 400 invalid_scope DISPATCH_SIGNIN_OFF
// and records no login. Control: the same broker opens a machine login, and its person approves
// it.
//
// The named revert: leave the switch out of the device login, and the Dispatch sign-in opens.
func TestABrokerWithTheDispatchSignInOffOpensNone(t *testing.T) {
	rig := ciphertest.NewRig(t, ciphertest.WithoutDispatchSignIn())
	m := newDeviceMachine(t, rig)
	m.config.Scopes = []string{oauthwire.ScopeDispatch}
	_, err := m.authorize(m.dispatchSignInRequest(t, []string{oauthwire.ScopePerson}), m.thumbprint(t))
	if code, description := oauthErrorCode(t, err); code != "invalid_scope" || description != signInOff {
		t.Fatalf("a Dispatch sign-in on a broker with it off = %s %q, want invalid_scope %q", code, description, signInOff)
	}
	if n := countRows(t, rig, "credential_requests"); n != 0 {
		t.Fatalf("credential requests after the refusal = %d, want 0", n)
	}
	approvedLogin(t, rig)
}

// TestThePendingListNamesWhatADispatchSignInAsks: GET /v1/pending answers each undecided Dispatch
// sign-in's dispatch_scopes as its lookup does, the scopes it asks for, and the empty list for
// one asking none, and each row's reason; an undecided machine login on the same list answers
// null. Once a sign-in is decided and its device code swept, its lookup still answers the scopes
// it asked, since the record keeps them, and never reads as a machine login.
//
// The named reverts: leave dispatch_scopes off the pending rows, and both sign-ins answer null;
// read them from the device code, and the swept sign-in's lookup answers null.
func TestThePendingListNamesWhatADispatchSignInAsks(t *testing.T) {
	rig := ciphertest.NewRig(t)
	_, _, asking := openDispatchSignIn(t, rig, "a session asking person", []string{oauthwire.ScopePerson})
	_, askingNoneCode, askingNone := openDispatchSignIn(t, rig, "a session asking nothing", nil)
	da := newDeviceMachine(t, rig).login(t)
	status, body := rig.UI(t, http.MethodPost, "/v1/machine-logins/lookup", map[string]any{"code": da.UserCode})
	if status != http.StatusOK {
		t.Fatalf("machine login lookup = %d: %s", status, body)
	}
	login := decode[wireRecord](t, body).RecordID

	status, body = rig.UI(t, http.MethodGet, "/v1/pending?approver="+rig.Operator, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /v1/pending = %d: %s", status, body)
	}
	rows := map[string]wirePendingEntry{}
	for _, row := range decode[struct {
		Pending []wirePendingEntry `json:"pending"`
	}](t, body).Pending {
		rows[row.RecordID] = row
	}
	if len(rows) != 3 {
		t.Fatalf("GET /v1/pending = %s, want the two sign-ins and the machine login", body)
	}
	if got := rows[asking.RecordID]; got.DispatchScopes == nil || !slices.Equal(*got.DispatchScopes, []string{oauthwire.ScopePerson}) || got.Reason != "a session asking person" {
		t.Errorf("the sign-in asking person lists %+v, want dispatch_scopes [person] and its reason", got)
	}
	if got := rows[askingNone.RecordID]; got.DispatchScopes == nil || len(*got.DispatchScopes) != 0 || got.Reason != "a session asking nothing" {
		t.Errorf("the sign-in asking nothing lists %+v, want the empty list and its reason", got)
	}
	if got := rows[login]; got.DispatchScopes != nil {
		t.Errorf("the machine login lists dispatch_scopes %v, want null", *got.DispatchScopes)
	}

	if status, body := decideAt(t, rig, rig.URL, askingNone.RecordID, "deny", map[string]any{"approver": rig.Operator, "code": askingNoneCode.UserCode}); status != http.StatusOK {
		t.Fatalf("deny the sign-in asking nothing = %d %s", status, body)
	}
	if _, err := rig.Store.Pool.Exec(t.Context(), `delete from oauth_device_codes`); err != nil {
		t.Fatalf("sweep the device codes: %v", err)
	}
	status, body = rig.UI(t, http.MethodPost, "/v1/machine-logins/lookup", map[string]any{"code": askingNoneCode.UserCode})
	if got := decode[wireRecord](t, body); status != http.StatusOK || got.State != "denied" || got.DispatchScopes == nil || len(*got.DispatchScopes) != 0 {
		t.Fatalf("the denied sign-in's lookup once its device code is swept = %d %+v, want denied and still a sign-in asking none", status, got)
	}
}

// TestTwoDispatchSignInsOfOneHostAreToldApartByTheirReasons: two Dispatch sign-ins approved on
// one host are listed on their person's machine-login list each with the reason its request object
// signed.
//
// The named revert: list no reason, and both rows answer "".
func TestTwoDispatchSignInsOfOneHostAreToldApartByTheirReasons(t *testing.T) {
	rig := ciphertest.NewRig(t)
	_, _, first := approvedDispatchSignInFor(t, rig, "the first session", []string{oauthwire.ScopePerson}, []string{oauthwire.ScopePerson})
	_, _, second := approvedDispatchSignInFor(t, rig, "the second session", []string{oauthwire.ScopePerson}, []string{})
	status, body := rig.UI(t, http.MethodGet, "/v1/launcher-credentials?approver="+rig.Operator, nil)
	if status != http.StatusOK {
		t.Fatalf("list the machine logins = %d: %s", status, body)
	}
	reasons := map[string]string{}
	for _, row := range decode[struct {
		Credentials []wireLauncherCredential `json:"credentials"`
	}](t, body).Credentials {
		if row.Host != "example-host-devbox" {
			t.Fatalf("a listed login's host = %q, want both on example-host-devbox", row.Host)
		}
		reasons[row.CredentialID] = row.Reason
	}
	if reasons[first] != "the first session" || reasons[second] != "the second session" || len(reasons) != 2 {
		t.Fatalf("the listed reasons = %v, want the first session for %s and the second session for %s", reasons, first, second)
	}
}

// TestOnlyThePersonADispatchSignInNamesDecidesIt: an approval of a Dispatch sign-in by a person
// other than the one its request names is 403 NOT_APPROVER and mints no credential. Control: the
// person it names approves the same login with the same code, and a credential is minted.
//
// The named revert: take any signed-in person's approval, and the other person's is issued.
func TestOnlyThePersonADispatchSignInNamesDecidesIt(t *testing.T) {
	rig := ciphertest.NewRig(t)
	_, da, looked := openDispatchSignIn(t, rig, "a session of its own person", []string{oauthwire.ScopePerson})
	status, body := decideAt(t, rig, rig.URL, looked.RecordID, "approve",
		map[string]any{"approver": "bob@example.com", "code": da.UserCode, "scopes": []string{oauthwire.ScopePerson}})
	if refusal := decode[wireError](t, body); status != http.StatusForbidden || refusal.Code != "NOT_APPROVER" {
		t.Fatalf("another person's approval = %d %+v, want 403 NOT_APPROVER", status, refusal)
	}
	if n := countRows(t, rig, "launcher_credentials"); n != 0 {
		t.Fatalf("launcher credentials after the refused approval = %d, want 0", n)
	}
	approveSignIn(t, rig, rig.URL, looked.RecordID, da.UserCode, []string{oauthwire.ScopePerson})
	if n := countRows(t, rig, "launcher_credentials"); n != 1 {
		t.Fatalf("launcher credentials after the named person's approval = %d, want 1", n)
	}
}

// TestEachDispatchSignInTakesNoSessionNameAnotherCredentialOfItsPersonHolds: each sign-in's claims
// name a principal of its own (oauthwire.ConsentedPrincipal of its person and its credential),
// and the broker holds a session name to one principal (oauthstore.Store.BindSession). So sign-in
// X's trade under a name its person's machine login wrote under is refused invalid_grant, naming
// the session and minting nothing, and X under a name nobody holds binds, minting a consented
// token whose introspection names X's credential as its sign-in. Sign-in Y of the same person,
// approved with no scope, is refused X's name the same way, so it cannot act on Envoy as X's
// session, and binds a name of its own; and X trades under its own name again, since a trade of
// the same sign-in is the same principal.
//
// The named reverts: give a consented claim PersonPrincipal, and X is minted a token naming the
// machine login's session; give it one principal per person, and Y is minted a token naming X's.
func TestEachDispatchSignInTakesNoSessionNameAnotherCredentialOfItsPersonHolds(t *testing.T) {
	rig := ciphertest.NewRig(t)
	const held, xName, yName = "ada-machine-session", "ada-sign-in-x", "ada-sign-in-y"
	trade := func(m *deviceMachine, token, session string) answered {
		return tradeAt(t, rig.URL, cipherclient.NewDPoPTransport(m.key, http.DefaultTransport), token, session, "", nil)
	}
	minted := func(what string, got answered) string {
		t.Helper()
		token := decode[struct {
			AccessToken string `json:"access_token"`
		}](t, got.body).AccessToken
		if got.status != http.StatusOK || token == "" {
			t.Fatalf("%s = %d %s, want a token", what, got.status, got.body)
		}
		return token
	}
	refusedHeld := func(what, session string, got answered) {
		t.Helper()
		if got.status != http.StatusBadRequest || strings.Contains(string(got.body), `"access_token"`) {
			t.Fatalf("%s = %d %s, want 400 and no token", what, got.status, got.body)
		}
		for _, names := range []string{`"invalid_grant"`, session, "another identity"} {
			if !strings.Contains(string(got.body), names) {
				t.Errorf("the refusal of %s reads %s, want it to name %s", what, got.body, names)
			}
		}
	}

	login, loginToken, _ := approvedLogin(t, rig)
	minted(rig.Operator+"'s machine login trading under "+held, trade(login, loginToken.AccessToken, held))

	x, xToken, xCredential := approvedDispatchSignIn(t, rig, []string{oauthwire.ScopePerson}, []string{oauthwire.ScopePerson})
	refusedHeld("sign-in X's trade under "+held+", the name the machine login holds", held, trade(x, xToken.AccessToken, held))
	xTraded := minted("sign-in X's trade under "+xName+", a name nobody holds", trade(x, xToken.AccessToken, xName))
	status, answer := introspect(t, rig, xTraded, rig.UIToken)
	if status != http.StatusOK || answer.SignIn != xCredential {
		t.Fatalf("introspect sign-in X's token = %d %+v, want it to name X's credential %s as its sign-in", status, answer, xCredential)
	}
	requireConsented(t, "the token sign-in X traded under its own name", rig, answer, oauthwire.ScopePerson)

	y, yToken, yCredential := approvedDispatchSignIn(t, rig, []string{oauthwire.ScopePerson}, []string{})
	if yCredential == xCredential {
		t.Fatalf("sign-ins X and Y share the credential %s, want one each", xCredential)
	}
	refusedHeld("sign-in Y's trade under "+xName+", the name sign-in X holds", xName, trade(y, yToken.AccessToken, xName))
	minted("sign-in Y's trade under "+yName+", a name nobody holds", trade(y, yToken.AccessToken, yName))
	minted("sign-in X's trade under "+xName+" again, its own name", trade(x, xToken.AccessToken, xName))
}

package api

import (
	"crypto/ecdsa"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/sjawhar/envoy/internal/cipher/ciphertest"
	"github.com/sjawhar/envoy/internal/cipher/exchange"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess/projectaccesstest"
	"github.com/sjawhar/envoy/internal/cipherclient"
	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/ciphertoken/ciphertokentest"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// restrictedTokenRig is one deployment end to end: the real broker (ciphertest.Rig, on
// BROKER_TEST_DATABASE_URL), whose token exchange asks a stand-in of Dispatch's project-access
// routes what kind of member each person is, behind Dispatch's own routes, which relay the
// credential pages to that broker and decide every broker token by asking it. Dispatch holds ACC,
// which alice owns and whose access list names erin, a restricted member; carol is a full member.
// server is Dispatch's registered server, whose broker revocation pass a row runs itself
// (revokeBrokerDebts), as the loop runs it after a removal commits.
type restrictedTokenRig struct {
	broker   *ciphertest.Rig
	client   *ciphertest.Client
	dispatch http.Handler
	database *store.Store
	server   *server
}

// newRestrictedTokenRig builds the rig, each of configure applied to Dispatch's options (a row
// that needs passkey sign-in on sets its outside origin).
func newRestrictedTokenRig(t *testing.T, configure ...func(*testServerOptions)) restrictedTokenRig {
	t.Helper()
	access := projectaccesstest.NewServer(t,
		map[string]projectaccesstest.Row{"ACC": {Visibility: "private", Owner: "alice", Listed: []string{accessErin}}},
		map[string]string{accessErin: projectaccess.MemberRestricted, accessCarol: projectaccess.MemberFull})
	options := testServerOptions{}
	for _, apply := range configure {
		apply(&options)
	}
	return newRestrictedTokenRigWith(t, projectaccess.New(access.URL, access.Account, nil), options)
}

// newRestrictedTokenRigWith is the rig with the broker's project check asking access, and Dispatch
// built with options beside what the rig sets itself: outside sign-in on, its broker tokens checked
// at the rig's broker, and that broker as its secrets broker.
func newRestrictedTokenRigWith(t *testing.T, access projectaccess.Access, options testServerOptions) restrictedTokenRig {
	t.Helper()
	broker := ciphertest.NewRig(t, ciphertest.WithProjectAccess(access))
	client := ciphertest.NewClient(t, broker.URL, strings.TrimPrefix(broker.URL, "http://"))
	client.DPoP, client.UIToken = cipherclient.NewDPoPTransport, broker.UIToken
	dispatchAccount := accountTokenFile(t, broker.UIToken)
	options.signInOutside = true
	options.brokerTokens = func(database *store.Store) *ciphertoken.Verifier {
		verifier, err := ciphertoken.New(ciphertoken.Options{
			BrokerURL: broker.URL, Account: ciphertoken.NewAccountToken(dispatchAccount), Origins: []string{testServerURL},
			Replay: store.NewDPoPProofs(database.Pool), Address: auth.ForwardedAddress,
		})
		if err != nil {
			t.Fatalf("build the broker token verifier: %v", err)
		}
		return verifier
	}
	options.agentSecretsURL, options.agentSecretsTokenFile = broker.URL, dispatchAccount
	served, database, registered := newRegisteredAccessServer(t, options)
	if listed := dispatchRequest(t, served, http.MethodPut, "/api/v1/projects/ACC/access/people/"+accessErin, nil, "alice"); listed.Code != http.StatusOK {
		t.Fatalf("alice names erin on ACC's list: status=%d body=%s", listed.Code, listed.Body.String())
	}
	return restrictedTokenRig{broker: broker, client: client, dispatch: served, database: database, server: registered}
}

// machineLogin is one machine's pending device login for person: the key it holds and the device
// authorization the broker answered, whose user code the person types at Dispatch.
type machineLogin struct {
	key    *ecdsa.PrivateKey
	device *oauth2.DeviceAuthResponse
}

func (r restrictedTokenRig) startLogin(t *testing.T, person, host string) machineLogin {
	t.Helper()
	key := ciphertest.NewSigningKey(t)
	return machineLogin{key: key, device: r.client.DeviceLogin(t, key, person, host)}
}

// lookUp is the machine-lookup of login's code through Dispatch, by login at a browser.
func (r restrictedTokenRig) lookUp(t *testing.T, login string, machine machineLogin) *httptest.ResponseRecorder {
	t.Helper()
	return dispatchRequest(t, r.dispatch, http.MethodPost, "/api/v1/credential-requests/machine-lookup", map[string]any{"code": machine.device.UserCode}, login)
}

// recordOf is the record the broker holds for machine's login, read through Dispatch by its own
// person, which a full member's lookup and a restricted member's of their own both answer.
func (r restrictedTokenRig) recordOf(t *testing.T, person string, machine machineLogin) (id, state string) {
	t.Helper()
	looked := r.lookUp(t, person, machine)
	var record struct {
		RecordID string `json:"record_id"`
		State    string `json:"state"`
	}
	if looked.Code != http.StatusOK || json.Unmarshal(looked.Body.Bytes(), &record) != nil || record.RecordID == "" {
		t.Fatalf("%s's lookup of their own machine login through Dispatch = %d %s, want 200 and its record", person, looked.Code, looked.Body.String())
	}
	return record.RecordID, record.State
}

// approveAtBrowser is login's approve of machine's login through Dispatch, at a browser, with the
// code the machine shows.
func (r restrictedTokenRig) approveAtBrowser(t *testing.T, login, recordID string, machine machineLogin) *httptest.ResponseRecorder {
	t.Helper()
	return dispatchRequest(t, r.dispatch, http.MethodPost, "/api/v1/credential-requests/"+recordID+"/approve", map[string]any{"code": machine.device.UserCode}, login)
}

// trade is the machine's trade of its approved login's token for a session's, naming session and
// asking for projects, with a DPoP proof of its own key: the device-code exchange first, then the
// RFC 8693 exchange of that token at the broker, as a dev box session's client makes it. It
// answers the trade's status and body.
func (r restrictedTokenRig) trade(t *testing.T, machine machineLogin, session, projects string) (int, map[string]any) {
	t.Helper()
	token, err := r.client.DeviceToken(t, machine.key, machine.device)
	if err != nil {
		t.Fatalf("the machine's device-code exchange: %v", err)
	}
	return r.exchange(t, machine, token.AccessToken, session, projects)
}

// exchange is the RFC 8693 exchange at the broker of machineToken, the machine's own login token,
// for a session's, naming session and asking for projects, with a DPoP proof of the machine's key.
// It answers the trade's status and body.
func (r restrictedTokenRig) exchange(t *testing.T, machine machineLogin, machineToken, session, projects string) (int, map[string]any) {
	t.Helper()
	form := url.Values{
		"grant_type":           {"urn:ietf:params:oauth:grant-type:token-exchange"},
		"client_id":            {oauthwire.ExchangeClientID},
		"subject_token":        {machineToken},
		"subject_token_type":   {oauthwire.SubjectTokenTypeAccessToken},
		oauthwire.FormSession:  {session},
		oauthwire.FormProjects: {projects},
	}
	client := &http.Client{Transport: cipherclient.NewDPoPTransport(machine.key, http.DefaultTransport), Timeout: 30 * time.Second}
	response, err := client.PostForm(r.broker.URL+"/v1/oauth/token", form)
	if err != nil {
		t.Fatalf("POST /v1/oauth/token: %v", err)
	}
	defer response.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("decode the trade's answer: %v", err)
	}
	return response.StatusCode, body
}

// introspect is the broker's own answer about token, asked as Dispatch asks it.
func (r restrictedTokenRig) introspect(t *testing.T, token string) ciphertoken.Introspection {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, r.broker.URL+ciphertoken.IntrospectPath, strings.NewReader(url.Values{"token": {token}}.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Authorization", "Bearer "+r.broker.UIToken)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("POST %s: %v", ciphertoken.IntrospectPath, err)
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(response.Body)
	var answer ciphertoken.Introspection
	if response.StatusCode != http.StatusOK || json.Unmarshal(raw, &answer) != nil {
		t.Fatalf("POST %s = %d %s, want the broker's answer", ciphertoken.IntrospectPath, response.StatusCode, raw)
	}
	return answer
}

// presenting is token presented to Dispatch by the machine that holds machine's key, under the
// DPoP scheme with a fresh proof of that key, on method and path with body.
func (r restrictedTokenRig) presenting(t *testing.T, machine machineLogin, token, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	caller := ciphertokentest.NewCallerWith(t, machine.key)
	presented := (&brokerCaller{caller: caller, jkt: caller.JKT()}).presenting(t, method, path, token, body)
	response := httptest.NewRecorder()
	r.dispatch.ServeHTTP(response, presented)
	return response
}

// TestARestrictedMembersAgentTokenIsNamedAndNeitherMintsNorApproves runs an outside person's agent
// on one deployment, end to end. erin, a restricted member, approves her own machine's device
// login at her browser through Dispatch, which reaches the broker; her machine trades its token
// for a session's naming ACC, and the broker answers a token of the named scope listing exactly
// ACC that lives at most exchange.RestrictedLifetime. Presented to Dispatch, that token acts for
// erin in ACC, and holds no scope of a person's own: it reads no person-scope route (GET
// /api/v1/inbox), mints no personal token (POST /api/v1/me/agent-tokens) and approves no machine
// login, each 403 OAUTH_SCOPE_REQUIRED, the login staying pending. Her browser approve of carol's
// pending machine login is 403 RESTRICTED_MEMBER and leaves it pending.
//
// The control runs on the same rig: carol, a full member, approves her own login at her browser;
// her machine's trade, naming no project, is the person scope, and with that token GET
// /api/v1/inbox answers 200, so erin's refusal there is her token's scope and not the rig's; and
// carol's personal token mints a token (201) and approves her second machine login (200), so the
// rig mints and approves for a credential that holds the approve scope. A broker token acting for
// its person is no such credential wherever Dispatch gives it no approve scope, so the control
// asks neither of it.
//
// Controls: mint the person scope for a restricted member's trade, and the first row fails naming
// the scope; refuse a restricted member's browser decision as before, and erin's approve of her
// own login fails naming RESTRICTED_MEMBER.
func TestARestrictedMembersAgentTokenIsNamedAndNeitherMintsNorApproves(t *testing.T) {
	r := newRestrictedTokenRig(t)

	erins := r.startLogin(t, accessErin, "erin-laptop")
	erinsRecord, _ := r.recordOf(t, accessErin, erins)
	if approved := r.approveAtBrowser(t, accessErin, erinsRecord, erins); approved.Code != http.StatusOK || !strings.Contains(approved.Body.String(), `"state":"approved"`) {
		t.Fatalf("erin's browser approve of her own machine login through Dispatch = %d %s, want 200 approved", approved.Code, approved.Body.String())
	}
	status, body := r.trade(t, erins, "erin-agent", "ACC")
	if status != http.StatusOK {
		t.Fatalf("erin's machine's trade naming ACC = %d %v, want 200", status, body)
	}
	token, _ := body["access_token"].(string)
	answer := r.introspect(t, token)
	if !answer.Active || answer.ProjectScope != ciphertoken.ProjectScopeNamed || !slices.Equal(answer.Projects, []string{"ACC"}) || answer.Person != accessErin {
		t.Fatalf("erin's agent's token introspects as %+v, want a live token of project scope %s listing exactly [ACC], acting for %s",
			answer, ciphertoken.ProjectScopeNamed, accessErin)
	}
	if expiresIn, _ := body["expires_in"].(float64); expiresIn <= 0 || time.Duration(expiresIn)*time.Second > exchange.RestrictedLifetime {
		t.Fatalf("erin's agent's token expires_in %v, want at most %s", body["expires_in"], exchange.RestrictedLifetime)
	}
	if lifetime := time.Until(time.Unix(answer.ExpiresAt, 0)); lifetime > exchange.RestrictedLifetime {
		t.Fatalf("erin's agent's token is live for %s more, want at most %s", lifetime, exchange.RestrictedLifetime)
	}

	if acts := r.presenting(t, erins, token, http.MethodGet, "/api/v1/whoami", nil); acts.Code != http.StatusOK ||
		!strings.Contains(acts.Body.String(), `"owner":"erin@other.example"`) || !strings.Contains(acts.Body.String(), `"project":"ACC"`) {
		t.Fatalf("whoami under erin's agent's token = %d %s, want it acting for erin in ACC", acts.Code, acts.Body.String())
	}
	if minted := r.presenting(t, erins, token, http.MethodPost, "/api/v1/me/agent-tokens", map[string]any{"name": "minted by erin's agent"}); minted.Code != http.StatusForbidden ||
		!strings.Contains(minted.Body.String(), `"code":"OAUTH_SCOPE_REQUIRED"`) {
		t.Errorf("POST /api/v1/me/agent-tokens under erin's agent's token = %d %s, want 403 OAUTH_SCOPE_REQUIRED", minted.Code, minted.Body.String())
	}
	if read := r.presenting(t, erins, token, http.MethodGet, "/api/v1/inbox", nil); read.Code != http.StatusForbidden ||
		!strings.Contains(read.Body.String(), `"code":"OAUTH_SCOPE_REQUIRED"`) {
		t.Errorf("GET /api/v1/inbox under erin's agent's token = %d %s, want 403 OAUTH_SCOPE_REQUIRED", read.Code, read.Body.String())
	}
	second := r.startLogin(t, accessErin, "erin-desktop")
	secondRecord, _ := r.recordOf(t, accessErin, second)
	if decided := r.presenting(t, erins, token, http.MethodPost, "/api/v1/credential-requests/"+secondRecord+"/approve", map[string]any{"code": second.device.UserCode}); decided.Code != http.StatusForbidden ||
		!strings.Contains(decided.Body.String(), `"code":"OAUTH_SCOPE_REQUIRED"`) {
		t.Errorf("an approve under erin's agent's token = %d %s, want 403 OAUTH_SCOPE_REQUIRED", decided.Code, decided.Body.String())
	}
	if _, state := r.recordOf(t, accessErin, second); state != "pending" {
		t.Errorf("erin's second machine login after her agent's approve is %s, want pending", state)
	}

	carols := r.startLogin(t, accessCarol, "carol-laptop")
	carolsRecord, _ := r.recordOf(t, accessCarol, carols)
	if refused := r.approveAtBrowser(t, accessErin, carolsRecord, carols); refused.Code != http.StatusForbidden || !strings.Contains(refused.Body.String(), `"code":"RESTRICTED_MEMBER"`) {
		t.Errorf("erin's browser approve of carol's machine login = %d %s, want 403 RESTRICTED_MEMBER", refused.Code, refused.Body.String())
	}
	if _, state := r.recordOf(t, accessCarol, carols); state != "pending" {
		t.Fatalf("carol's machine login after erin's approve is %s, want pending", state)
	}

	// The control, on the same rig: a full member's agent.
	if approved := r.approveAtBrowser(t, accessCarol, carolsRecord, carols); approved.Code != http.StatusOK {
		t.Fatalf("carol's browser approve of her own machine login = %d %s, want 200", approved.Code, approved.Body.String())
	}
	status, body = r.trade(t, carols, "carol-agent", "")
	if status != http.StatusOK {
		t.Fatalf("carol's machine's trade = %d %v, want 200", status, body)
	}
	carolsToken, _ := body["access_token"].(string)
	if answer := r.introspect(t, carolsToken); !answer.Active || answer.ProjectScope != ciphertoken.ProjectScopePerson || answer.Projects != nil {
		t.Fatalf("carol's agent's token introspects as %+v, want a live token of project scope %s naming no project", answer, ciphertoken.ProjectScopePerson)
	}
	if read := r.presenting(t, carols, carolsToken, http.MethodGet, "/api/v1/inbox", nil); read.Code != http.StatusOK {
		t.Errorf("GET /api/v1/inbox under carol's agent's token = %d %s, want 200", read.Code, read.Body.String())
	}
	minted := dispatchRequest(t, r.dispatch, http.MethodPost, "/api/v1/me/agent-tokens", map[string]string{"name": "carol's token"}, accessCarol)
	if minted.Code != http.StatusCreated {
		t.Fatalf("carol mints a personal token at her browser: %d %s", minted.Code, minted.Body.String())
	}
	personal := decodeBody[agentTokenResponse](t, minted).Token
	if again := agentRequest(t, r.dispatch, http.MethodPost, "/api/v1/me/agent-tokens", map[string]any{"name": "minted by carol's token"}, personal); again.Code != http.StatusCreated {
		t.Errorf("POST /api/v1/me/agent-tokens under carol's personal token = %d %s, want 201", again.Code, again.Body.String())
	}
	carolsSecond := r.startLogin(t, accessCarol, "carol-desktop")
	carolsSecondRecord, _ := r.recordOf(t, accessCarol, carolsSecond)
	if decided := agentRequest(t, r.dispatch, http.MethodPost, "/api/v1/credential-requests/"+carolsSecondRecord+"/approve", map[string]any{"code": carolsSecond.device.UserCode}, personal); decided.Code != http.StatusOK {
		t.Errorf("an approve of carol's second machine login under carol's personal token = %d %s, want 200", decided.Code, decided.Body.String())
	}
}

// approvedLogin is a machine login of person's, approved at person's browser through Dispatch, and
// the device login's own token, which the machine refreshes and trades: the start of each row
// below. It answers the launcher credential the approval minted.
func (r restrictedTokenRig) approvedLogin(t *testing.T, person, host string) (machineLogin, *oauth2.Token, string) {
	t.Helper()
	machine := r.startLogin(t, person, host)
	recordID, _ := r.recordOf(t, person, machine)
	approved := r.approveAtBrowser(t, person, recordID, machine)
	var decided struct {
		CredentialID string `json:"credential_id"`
	}
	if approved.Code != http.StatusOK || json.Unmarshal(approved.Body.Bytes(), &decided) != nil || decided.CredentialID == "" {
		t.Fatalf("%s's browser approve of their own machine login = %d %s, want 200 and its credential", person, approved.Code, approved.Body.String())
	}
	device, err := r.client.DeviceToken(t, machine.key, machine.device)
	if err != nil {
		t.Fatalf("%s's machine's device-code exchange: %v", person, err)
	}
	return machine, device, decided.CredentialID
}

// sessionToken is the session token a trade of machine's device token answered, failing the test
// for any other answer.
func (r restrictedTokenRig) sessionToken(t *testing.T, machine machineLogin, device *oauth2.Token, session, projects string) string {
	t.Helper()
	status, body := r.exchange(t, machine, device.AccessToken, session, projects)
	token, _ := body["access_token"].(string)
	if status != http.StatusOK || token == "" {
		t.Fatalf("the trade for session %s naming %q = %d %v, want 200 and a token", session, projects, status, body)
	}
	return token
}

// TestADelistedMembersTokenIsNotRenewed: once ACC's owner takes erin, a restricted member, off
// ACC's access list, Dispatch owes the broker the revocation of every machine login she approved,
// recorded in the removal's transaction, and a pass of the broker revocation loop a minute on
// (machineLoginSettleAfter) pays it. Then her agent's session token introspects not active, the
// broker refuses her device login's refresh and every further trade of its token, the login is
// revoked in the broker's own rows, and the debt is settled. A device login is not per project, so
// the removal from one project's list ends all of hers, and she approves again for a project that
// still lists her.
//
// The control runs on the same rig: carol, a full member, is put on ACC's list and taken off it
// again, owes nothing, and her token stays active and her refresh is answered, so each refusal
// above is the delisting's and not the rig's.
//
// Control: record nothing on the removal (recordMachineLoginRevocation answering nil at once), and
// the row fails naming the revocation not owed; at the head that revoked inline, a removal from a
// list that never named erin ended her logins (TestARemovalFromAnotherProjectLeavesTheMembersLogins).
func TestADelistedMembersTokenIsNotRenewed(t *testing.T) {
	r := newRestrictedTokenRig(t)
	ctx := t.Context()

	erins, erinsDevice, erinsCredential := r.approvedLogin(t, accessErin, "erin-laptop")
	erinsToken := r.sessionToken(t, erins, erinsDevice, "erin-agent", "ACC")
	if !r.introspect(t, erinsToken).Active {
		t.Fatalf("erin's agent's token introspects not active before the removal")
	}
	if listed := dispatchRequest(t, r.dispatch, http.MethodPut, "/api/v1/projects/ACC/access/people/"+accessCarol, nil, "alice"); listed.Code != http.StatusOK {
		t.Fatalf("alice names carol on ACC's list: %d %s", listed.Code, listed.Body.String())
	}
	carols, carolsDevice, _ := r.approvedLogin(t, accessCarol, "carol-laptop")
	carolsToken := r.sessionToken(t, carols, carolsDevice, "carol-agent", "")

	for _, person := range []string{accessErin, accessCarol} {
		if removed := dispatchRequest(t, r.dispatch, http.MethodDelete, "/api/v1/projects/ACC/access/people/"+person, nil, "alice"); removed.Code != http.StatusOK {
			t.Fatalf("alice takes %s off ACC's list: %d %s, want 200", person, removed.Code, removed.Body.String())
		}
	}
	revocations := store.NewMachineLoginRevocations(r.database.Pool)
	for person, want := range map[string]bool{accessErin: true, accessCarol: false} {
		if owed, _, err := revocations.Owing(ctx, person); err != nil || owed != want {
			t.Fatalf("the revocation of %s's machine logins is owed %t (%v) after the removal, want %t", person, owed, err, want)
		}
	}
	removalAged(t, r.database, machineLoginSettleAfter)
	r.server.revokeBrokerDebts(ctx)
	if owed, _, err := revocations.Owing(ctx, accessErin); err != nil || owed {
		t.Errorf("the revocation of erin's machine logins is owed %t (%v) after a pass, want it settled", owed, err)
	}

	if r.introspect(t, erinsToken).Active {
		t.Errorf("erin's agent's token introspects active after her removal from ACC's list, want it ended at once")
	}
	// The trade first: a refresh ends the device login's earlier access tokens, so a trade after it
	// would be refused whatever the removal did.
	if status, body := r.exchange(t, erins, erinsDevice.AccessToken, "erin-agent-2", "ACC"); status == http.StatusOK {
		t.Errorf("a trade of erin's device token after her removal = 200 %v, want it refused", body)
	}
	if refreshed, err := r.client.Refresh(t, erins.key, erinsDevice); err == nil {
		t.Errorf("erin's device login refreshed after her removal, answering a token expiring %s; want it refused", refreshed.Expiry)
	}
	var revoked bool
	if err := r.broker.Store.Pool.QueryRow(ctx, `select revoked_at is not null from launcher_credentials where id = $1::uuid`, erinsCredential).Scan(&revoked); err != nil || !revoked {
		t.Errorf("erin's machine login %s is revoked %t (%v) after her removal, want revoked", erinsCredential, revoked, err)
	}

	// The control: carol's login is not a restricted member's, and nothing of hers ended.
	if !r.introspect(t, carolsToken).Active {
		t.Errorf("carol's agent's token introspects not active after her removal, want a full member's token untouched")
	}
	if _, err := r.client.Refresh(t, carols.key, carolsDevice); err != nil {
		t.Errorf("carol's device login's refresh after her removal: %v, want it answered", err)
	}
}

// TestARemovalFromAnotherProjectLeavesTheMembersLogins: an owner's removal of a person from a list
// that does not name them leaves the list as it was, and owes the broker nothing. carol owns OTH,
// which never named erin; her removal of erin from OTH answers 200, records no revocation of
// erin's machine logins, and after the broker revocation loop's pass erin's ACC token still
// introspects active and her device login's refresh is answered. So no project's owner ends the
// logins of a person another project lists.
//
// Control: record the revocation on every removal, as the head before this row did, and erin's ACC
// token introspects not active and her refresh is refused, each named.
func TestARemovalFromAnotherProjectLeavesTheMembersLogins(t *testing.T) {
	r := newRestrictedTokenRig(t)
	ctx := t.Context()
	if created := dispatchRequest(t, r.dispatch, http.MethodPost, "/api/v1/projects", map[string]string{"key": "OTH", "name": "Another project"}, accessCarol); created.Code != http.StatusCreated {
		t.Fatalf("carol creates OTH: %d %s", created.Code, created.Body.String())
	}
	erins, erinsDevice, _ := r.approvedLogin(t, accessErin, "erin-laptop")
	erinsToken := r.sessionToken(t, erins, erinsDevice, "erin-agent", "ACC")

	if removed := dispatchRequest(t, r.dispatch, http.MethodDelete, "/api/v1/projects/OTH/access/people/"+accessErin, nil, accessCarol); removed.Code != http.StatusOK {
		t.Fatalf("carol's removal of erin from OTH, which never named her: %d %s, want 200", removed.Code, removed.Body.String())
	}
	if owed, _, err := store.NewMachineLoginRevocations(r.database.Pool).Owing(ctx, accessErin); err != nil || owed {
		t.Errorf("the revocation of erin's machine logins is owed %t (%v) after a removal from a list that never named her, want nothing owed", owed, err)
	}
	r.server.revokeBrokerDebts(ctx)
	if !r.introspect(t, erinsToken).Active {
		t.Errorf("erin's ACC token introspects not active after carol's removal of erin from OTH, which never named her")
	}
	if _, err := r.client.Refresh(t, erins.key, erinsDevice); err != nil {
		t.Errorf("erin's device login's refresh after carol's removal of erin from OTH: %v, want it answered", err)
	}
}

// TestAnotherOwnersRemovalRevokesTheMembersLogins: every removal that takes a restricted member off
// a project's access list owes the broker the revocation of the machine logins they approved, as
// R16's approved words read, whatever other list still names them. carol owns OTH; she names erin,
// whom ACC's list names, on OTH's list and takes her off it again. Both answer 200, the removal
// records the revocation, and after the broker revocation loop's pass erin's ACC token introspects
// not active and her device login's refresh is refused, though ACC's list still names her: she
// approves a new login for ACC.
//
// Control: record the revocation only when the removal leaves the person on no list, as the head
// before this row did, and nothing is owed, erin's ACC token stays active and her refresh is
// answered, each named.
func TestAnotherOwnersRemovalRevokesTheMembersLogins(t *testing.T) {
	r := newRestrictedTokenRig(t)
	ctx := t.Context()
	if created := dispatchRequest(t, r.dispatch, http.MethodPost, "/api/v1/projects", map[string]string{"key": "OTH", "name": "Another project"}, accessCarol); created.Code != http.StatusCreated {
		t.Fatalf("carol creates OTH: %d %s", created.Code, created.Body.String())
	}
	erins, erinsDevice, _ := r.approvedLogin(t, accessErin, "erin-laptop")
	erinsToken := r.sessionToken(t, erins, erinsDevice, "erin-agent", "ACC")

	for _, write := range []struct{ method, does string }{{http.MethodPut, "names erin on"}, {http.MethodDelete, "takes erin off"}} {
		if response := dispatchRequest(t, r.dispatch, write.method, "/api/v1/projects/OTH/access/people/"+accessErin, nil, accessCarol); response.Code != http.StatusOK {
			t.Fatalf("carol %s OTH's list: %d %s, want 200", write.does, response.Code, response.Body.String())
		}
	}
	if owed, _, err := store.NewMachineLoginRevocations(r.database.Pool).Owing(ctx, accessErin); err != nil || !owed {
		t.Errorf("the revocation of erin's machine logins is owed %t (%v) after carol took her off OTH while ACC's list names her, want it owed", owed, err)
	}
	r.server.revokeBrokerDebts(ctx)
	if r.introspect(t, erinsToken).Active {
		t.Errorf("erin's ACC token introspects active after carol took her off OTH, want it ended")
	}
	if _, err := r.client.Refresh(t, erins.key, erinsDevice); err == nil {
		t.Errorf("erin's device login's refresh after carol took her off OTH was answered, want it refused")
	}
}

// TestAnApprovalWaitsForTheOwedRevocationAndItsLoginSurvives: while a revocation of a restricted
// member's machine logins is owed, Dispatch relays no approval of theirs, so every login the pass
// finds was approved before the removal, and one approved after the pass survives. alice takes erin
// off ACC's list and puts her back before any pass of the broker revocation loop; erin's browser
// approve of a new machine login is 409 MACHINE_LOGIN_REVOCATION_OWED and its login stays pending;
// the pass a minute on (machineLoginSettleAfter) revokes her login from before the removal, its
// session token introspecting not active, and settles; her approve then is 200, and that login's
// token stays active and its refresh is answered.
//
// Control: relay a restricted member's approval while a revocation is owed (the owed read dropped
// from relayMachineLoginApproval), and her first approve answers 200, named.
func TestAnApprovalWaitsForTheOwedRevocationAndItsLoginSurvives(t *testing.T) {
	r := newRestrictedTokenRig(t)
	ctx := t.Context()
	before, beforeDevice, _ := r.approvedLogin(t, accessErin, "erin-laptop")
	beforeToken := r.sessionToken(t, before, beforeDevice, "erin-agent", "ACC")

	if removed := dispatchRequest(t, r.dispatch, http.MethodDelete, "/api/v1/projects/ACC/access/people/"+accessErin, nil, "alice"); removed.Code != http.StatusOK {
		t.Fatalf("alice takes erin off ACC's list: %d %s, want 200", removed.Code, removed.Body.String())
	}
	if listed := dispatchRequest(t, r.dispatch, http.MethodPut, "/api/v1/projects/ACC/access/people/"+accessErin, nil, "alice"); listed.Code != http.StatusOK {
		t.Fatalf("alice puts erin back on ACC's list: %d %s, want 200", listed.Code, listed.Body.String())
	}
	pending := r.startLogin(t, accessErin, "erin-desktop")
	pendingRecord, _ := r.recordOf(t, accessErin, pending)
	if refused := r.approveAtBrowser(t, accessErin, pendingRecord, pending); refused.Code != http.StatusConflict ||
		!strings.Contains(refused.Body.String(), `"code":"MACHINE_LOGIN_REVOCATION_OWED"`) {
		t.Fatalf("erin's approve while the revocation of her machine logins is owed = %d %s, want 409 MACHINE_LOGIN_REVOCATION_OWED", refused.Code, refused.Body.String())
	}
	if _, state := r.recordOf(t, accessErin, pending); state != "pending" {
		t.Errorf("erin's machine login whose approve was refused is %q, want pending", state)
	}

	removalAged(t, r.database, machineLoginSettleAfter)
	r.server.revokeBrokerDebts(ctx)
	if r.introspect(t, beforeToken).Active {
		t.Errorf("erin's token from the login she approved before her removal introspects active after the pass, want it ended")
	}
	if owed, _, err := store.NewMachineLoginRevocations(r.database.Pool).Owing(ctx, accessErin); err != nil || owed {
		t.Fatalf("the revocation of erin's machine logins is owed %t (%v) after the pass, want it settled", owed, err)
	}
	after, afterDevice, _ := r.approvedLogin(t, accessErin, "erin-desktop-2")
	afterToken := r.sessionToken(t, after, afterDevice, "erin-agent-2", "ACC")
	r.server.revokeBrokerDebts(ctx)
	if !r.introspect(t, afterToken).Active {
		t.Errorf("erin's token from the login she approved after the pass introspects not active, want it untouched")
	}
	if _, err := r.client.Refresh(t, after.key, afterDevice); err != nil {
		t.Errorf("the refresh of the login erin approved after the pass: %v, want it answered", err)
	}
}

// TestARestrictedMemberRevokesTheirOwnMachineLogin runs the member-served list and revoke on the
// real broker. erin, a restricted member, lists at her browser her one machine login, and carol's
// is not in her list. Her revoke of carol's login is 403 RESTRICTED_MEMBER and carol's agent's
// token stays active. Her agent's named token lists and revokes nothing, each 403
// OAUTH_SCOPE_REQUIRED: it holds no scope, so the scope gate refuses it first. Her browser's
// revoke of her own login is answered by the broker, which takes her as its approver: her agent's
// session token then introspects not active and her device login's refresh is refused, and her
// list is empty.
//
// Control: answer a restricted member as every other caller (the filter removed), and erin's
// revoke of carol's login reaches the broker, which refuses it with its own answer, not 403
// RESTRICTED_MEMBER, and the row fails naming it.
func TestARestrictedMemberRevokesTheirOwnMachineLogin(t *testing.T) {
	r := newRestrictedTokenRig(t)
	erins, erinsDevice, erinsCredential := r.approvedLogin(t, accessErin, "erin-laptop")
	erinsToken := r.sessionToken(t, erins, erinsDevice, "erin-agent", "ACC")
	carols, carolsDevice, carolsCredential := r.approvedLogin(t, accessCarol, "carol-laptop")
	carolsToken := r.sessionToken(t, carols, carolsDevice, "carol-agent", "")
	listed := func() []string {
		t.Helper()
		response := dispatchRequest(t, r.dispatch, http.MethodGet, "/api/v1/machine-logins", nil, accessErin)
		if response.Code != http.StatusOK {
			t.Fatalf("erin's browser on GET /api/v1/machine-logins = %d %s, want 200", response.Code, response.Body.String())
		}
		return credentialIDs(t, response)
	}

	if got := listed(); !slices.Equal(got, []string{erinsCredential}) {
		t.Errorf("erin's list = %v, want her own login %s alone", got, erinsCredential)
	}
	if refused := dispatchRequest(t, r.dispatch, http.MethodPost, "/api/v1/machine-logins/"+carolsCredential+"/revoke", nil, accessErin); refused.Code != http.StatusForbidden ||
		!strings.Contains(refused.Body.String(), `"code":"RESTRICTED_MEMBER"`) {
		t.Errorf("erin's revoke of carol's login = %d %s, want 403 RESTRICTED_MEMBER", refused.Code, refused.Body.String())
	}
	if !r.introspect(t, carolsToken).Active {
		t.Errorf("carol's agent's token introspects not active after erin's refused revoke of carol's login")
	}
	for _, call := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/machine-logins"},
		{http.MethodPost, "/api/v1/machine-logins/" + erinsCredential + "/revoke"},
	} {
		if byAgent := r.presenting(t, erins, erinsToken, call.method, call.path, nil); byAgent.Code != http.StatusForbidden ||
			!strings.Contains(byAgent.Body.String(), `"code":"OAUTH_SCOPE_REQUIRED"`) {
			t.Errorf("%s %s with erin's agent's token = %d %s, want 403 OAUTH_SCOPE_REQUIRED", call.method, call.path, byAgent.Code, byAgent.Body.String())
		}
	}
	if !r.introspect(t, erinsToken).Active {
		t.Fatalf("erin's agent's token introspects not active before her browser's revoke")
	}

	revoked := dispatchRequest(t, r.dispatch, http.MethodPost, "/api/v1/machine-logins/"+erinsCredential+"/revoke", nil, accessErin)
	if revoked.Code != http.StatusOK {
		t.Fatalf("erin's browser revoke of her own login = %d %s, want 200: the broker takes her as its approver", revoked.Code, revoked.Body.String())
	}
	if r.introspect(t, erinsToken).Active {
		t.Errorf("erin's agent's token introspects active after she revoked her login, want it ended")
	}
	if _, err := r.client.Refresh(t, erins.key, erinsDevice); err == nil {
		t.Errorf("erin's device login refreshed after she revoked it, want it refused")
	}
	if got := listed(); len(got) != 0 {
		t.Errorf("erin's list after she revoked her login = %v, want none", got)
	}
}

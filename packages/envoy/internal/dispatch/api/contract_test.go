// contract_test.go is the contract layer (built because every fake had been built from the
// client's assumption): it mounts the real broker handlers
// (brokerapi.Register with real services on
// BROKER_TEST_DATABASE_URL, exactly as the cipher/api tests do)
// behind an httptest.Server, wires Dispatch's own routes to relay to it, and drives every UI
// action through DISPATCH's routes as a signed-in human — proving the proxy round-trips a real
// broker, which lists each person's pending requests and the expired ones nobody has dismissed,
// decides a machine login, a dismissal and a revoke by the login Dispatch names, and refuses
// every decision of a secret request with the page where a person signed in to the broker
// decides it. A test needing a secret request decided decides it on
// that page, signed in to the broker (ciphertest.Decide), as a person does. Skips without
// BROKER_TEST_DATABASE_URL (via brokerstoretest.Open) or DISPATCH_TEST_DATABASE_URL (via
// storetest.Open, already required by every other test in this package).
package api

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	brokerapi "github.com/sjawhar/envoy/internal/cipher/api"
	"github.com/sjawhar/envoy/internal/cipher/ciphertest"
	"github.com/sjawhar/envoy/internal/cipher/enroll"
	"github.com/sjawhar/envoy/internal/cipher/machine"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/proof"
	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/cipher/requests"
	"github.com/sjawhar/envoy/internal/cipher/secrets"
	"github.com/sjawhar/envoy/internal/cipher/store"
	brokerstoretest "github.com/sjawhar/envoy/internal/cipher/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

const (
	contractApprover = "sami@example.com"
	// contractOther is a second allowed Dispatch login, neither any record's approver nor any
	// enrollment's operator.
	contractOther = "mallory@example.com"
	// contractDispatchAccount is the ServiceAccount the broker's BROKER_API_ACCOUNTS names as the
	// caller "dispatch": the identity Dispatch's own server presents on the broker's routes.
	contractDispatchAccount = "system:serviceaccount:example-ns:example-dispatch"
	// contractDaemonAccount is the ServiceAccount the rig's BROKER_SERVICE_LOGINS lets log in as
	// legion-daemon, and contractTokenAudience the audience its tokens carry.
	contractDaemonAccount = "system:serviceaccount:legion-system:legion-daemon-contract"
	contractTokenAudience = "agent-secrets"
)

// contractRig is the real broker (real Postgres) behind Dispatch's own mounted routes. Every UI
// action a test drives goes through rig.Dispatch; rig's direct broker helpers exist only to seed
// fixtures Dispatch has no route to create (a pending request comes from an agent's session
// proof, a machine login from the machine's own key — neither is a Dispatch UI action), to decide
// a secret request on the broker's own page, where alone one is decided, and to read a request's
// grant and its value back as the session that holds it.
type contractRig struct {
	Dispatch    http.Handler
	BrokerURL   string
	brokerStore *store.Store
	issuer      *oidctest.Issuer
	issuerKey   *oidctest.Key
	// requestMachine and machineLogins are the broker's own services behind its routes, held so
	// a test runs their expiry sweeps (requests.Machine.ExpirePending and
	// machine.Service.ExpirePending) as the broker's sweeper does.
	requestMachine *requests.Machine
	machineLogins  *machine.Service
}

func newContractRig(t *testing.T) *contractRig {
	t.Helper()
	brokerStore := brokerstoretest.Open(t)

	local := secrets.NewLocal(
		policytest.Secret("DEEL_API_KEY", contractApprover, policy.TierHuman, "deel-v1"),
		// contractApprover's own sessions get it without asking.
		policytest.Secret("AUTO_TOKEN", contractApprover, policy.TierAgent, "auto-v1"),
		// Any signed-in person approves a request for it.
		policytest.Secret("SHARED_KEY", policy.OwnerShared, policy.TierHuman, "shared-v1"),
	)
	cur := policytest.Current(t, local)

	brokerMux := http.NewServeMux()
	brokerServer := httptest.NewServer(brokerMux)
	t.Cleanup(brokerServer.Close)

	issuer := oidctest.New(t)
	issuerKey := issuer.PublishKey(t, "signing-key")
	// One issuer, as a deployment's BROKER_K8S_OIDC_ISSUER names one cluster: a bare URL, so the
	// issuer has no name, and the daemon's entry names it by that empty name.
	tokenIssuers := []oidc.NamedIssuer{{URL: issuer.URL()}}
	tokenVerifier := enroll.K8sPodVerifier{Issuers: oidc.NewIssuerSet(tokenIssuers, contractTokenAudience, 0)}
	enr := &enroll.Service{
		Store: brokerStore, Lease: time.Hour, Pod: tokenVerifier, Issuers: tokenIssuers,
		ServiceLogins: map[string]enroll.ServiceLogin{contractDaemonAccount: {Service: "legion-daemon", Issuers: []string{""}}},
	}
	enr.Chain = enroll.NewChainVerifier(brokerStore, brokerServer.URL, time.Minute)
	reqMachine := &requests.Machine{
		Store: brokerStore, Policy: cur, Secrets: policytest.Store(local),
		PendingTTL: 12 * time.Hour,
		Audience:   brokerServer.URL, Skew: time.Minute, Replay: enr.Replay,
	}
	reqMachine.Chain = requests.NewChainVerifier(brokerStore, brokerServer.URL, time.Minute, reqMachine.Airlock)
	provider := ciphertest.NewOAuthProvider(t, brokerStore, issuer)
	mach := &machine.Service{
		Store: brokerStore, Enroll: enr, Policy: cur,
		Audience: brokerServer.URL, Skew: time.Minute, PendingTTL: 15 * time.Minute, CredentialLifetime: 7 * 24 * time.Hour,
		OAuth: provider, Replay: enr.Replay,
	}
	// Dispatch calls the broker as its own listed caller: a projected ServiceAccount token of the
	// same issuer the broker verifies a pod's with, minted to live well inside the hour a caller's
	// token may.
	dispatchClaims := issuer.Claims(contractDispatchAccount, contractTokenAudience)
	dispatchClaims["iat"] = time.Now().Add(-5 * time.Second).Unix()
	dispatchToken := issuer.Mint(t, issuerKey, dispatchClaims)
	brokerapi.Register(brokerMux, brokerapi.Deps{
		PublicURL:     brokerServer.URL,
		APIAccounts:   map[string]brokerapi.Account{contractDispatchAccount: ciphertest.Caller(t, "dispatch")},
		AccountTokens: tokenVerifier,
		Enroll:        enr, Machine: reqMachine, MachineLogin: mach,
		OAuth: provider, DeviceLogin: mach.LoginDevice, TokenHooks: mach.TokenHooks(),
		Proof:  &proof.Verifier{Skew: time.Minute, Lookup: enr.Lookup, LookupLauncher: enr.AuthenticateLauncher, Replay: enr.Replay},
		Policy: cur, SecretsStore: secrets.StoreAWS, SecretsPrefix: policytest.Prefix, SecretsKMSKey: policytest.KeyARN,
		// People sign in to the broker's approval page at the same local issuer, which stands in for
		// every provider.
		Approval: ciphertest.NewApprovalPage(t, ciphertest.SignIn(issuer, issuerKey), brokerServer.URL, brokerStore, reqMachine),
	})

	dispatchDB := storetest.Open(t)
	documentService := docs.New(docs.Deps{
		Store: dispatchDB, Events: events.NewBroker(), ServerURL: "https://dispatch.example", Settle: 20 * time.Millisecond,
	})
	t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
	seedPeople(t, dispatchDB, contractApprover, contractOther)
	deps, err := NewDeps(DepsInput{
		Store: dispatchDB, Identity: headerIdentity(dispatchDB),
		ServerURL: "https://dispatch.example", Docs: documentService, Events: events.NewBroker(),
		AgentSecretsURL: brokerServer.URL, AgentSecretsTokenFile: accountTokenFile(t, dispatchToken),
	})
	if err != nil {
		t.Fatalf("new dispatch API dependencies: %v", err)
	}
	dispatchMux := http.NewServeMux()
	t.Cleanup(Register(dispatchMux, t.Context(), deps).Wait)

	return &contractRig{
		Dispatch: dispatchMux, BrokerURL: brokerServer.URL, brokerStore: brokerStore, issuer: issuer, issuerKey: issuerKey,
		requestMachine: reqMachine, machineLogins: mach,
	}
}

// --- fixture seeding: direct broker access, mirroring what an agent or a machine would do ---

func contractSigningKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return key
}

// newSessionEnrollment inserts a live box enrollment directly (mirroring cipher/api's own
// api_test.go newSessionEnrollment): a session route is out of Dispatch's UI scope entirely, so
// there is no proxy path that could create one.
func (rig *contractRig) newSessionEnrollment(t *testing.T, operator string) (enrollmentID string, key *ecdsa.PrivateKey) {
	t.Helper()
	ctx := context.Background()
	key = contractSigningKey(t)
	thumbprint, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		t.Fatalf("thumbprint: %v", err)
	}
	credentialID := uuid.New()
	if _, err := rig.brokerStore.Pool.Exec(ctx, `insert into launcher_credentials (id, operator, host, key_thumbprint, public_jwk, expires_at)
		values ($1,$2,'test-host',$3,'{}'::jsonb, now() + interval '30 days')`, credentialID, operator, thumbprint); err != nil {
		t.Fatalf("insert launcher_credentials: %v", err)
	}
	enrollmentID = uuid.NewString()
	if _, err := rig.brokerStore.Pool.Exec(ctx, `insert into enrollments (id, kind, runtime_id, operator, thumbprint, launcher_credential_id, lease_expires_at)
		values ($1,'session',$2,$3,$4,$5, now() + interval '1 hour')`, enrollmentID, "session-"+t.Name(), operator, thumbprint, credentialID); err != nil {
		t.Fatalf("insert enrollments: %v", err)
	}
	return enrollmentID, key
}

// brokerReq talks to the broker directly (never through Dispatch): only what an agent's session
// proof or a machine's own request could produce.
func (rig *contractRig) brokerReq(t *testing.T, method, path string, headers map[string]string, body any) (int, []byte) {
	t.Helper()
	request, err := http.NewRequest(method, rig.BrokerURL+path, nil)
	if body != nil {
		encoded, marshalErr := json.Marshal(body)
		if marshalErr != nil {
			t.Fatalf("marshal request body: %v", marshalErr)
		}
		request, err = http.NewRequest(method, rig.BrokerURL+path, strings.NewReader(string(encoded)))
		if err == nil {
			request.Header.Set("Content-Type", "application/json")
		}
	}
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	for k, v := range headers {
		request.Header.Set(k, v)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer response.Body.Close()
	respBody, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read %s %s response: %v", method, path, err)
	}
	return response.StatusCode, respBody
}

// contractSession is an agent's enrolled session, which alone can ask for secrets and read a
// grant's value back, and RequestID and RecordID, the request its last request made and the
// pending record it opened, if it made one.
type contractSession struct {
	RequestID    string
	RecordID     string
	EnrollmentID string
	Key          *ecdsa.PrivateKey
}

// contractRequested is POST /v1/requests's answer, and GET /v1/requests/{id}'s, as these tests
// read it.
type contractRequested struct {
	RequestID string  `json:"request_id"`
	State     string  `json:"state"`
	GrantID   *string `json:"grant_id"`
	RecordID  *string `json:"record_id"`
}

// newSession enrolls a fresh session operator's machine runs.
func (rig *contractRig) newSession(t *testing.T, operator string) contractSession {
	t.Helper()
	enrollmentID, key := rig.newSessionEnrollment(t, operator)
	return contractSession{EnrollmentID: enrollmentID, Key: key}
}

// request signs a real request object with session's key and posts it straight to the broker's own
// session route — the only way a request, and a pending agent_secret record, comes to exist.
func (rig *contractRig) request(t *testing.T, session contractSession, reason string, names ...string) contractRequested {
	t.Helper()
	details := make([]record.AuthorizationDetail, len(names))
	for i, name := range names {
		details[i] = record.AuthorizationDetail{Type: "agent_secret", Identifier: name, Actions: []string{"inject"}}
	}
	compact, err := record.Sign(session.Key, rig.BrokerURL, details, reason, "", nil, time.Now())
	if err != nil {
		t.Fatalf("record.Sign: %v", err)
	}
	p, err := proof.Sign(session.Key, session.EnrollmentID, http.MethodPost, rig.BrokerURL+"/v1/requests", time.Now())
	if err != nil {
		t.Fatalf("proof.Sign: %v", err)
	}
	status, body := rig.brokerReq(t, http.MethodPost, "/v1/requests", map[string]string{"Proof": p},
		map[string]any{"request": compact, "session_id": nil})
	if status != http.StatusOK {
		t.Fatalf("POST /v1/requests = %d: %s", status, body)
	}
	var created contractRequested
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode create-request response: %v (body: %s)", err, body)
	}
	return created
}

// createPendingAgentSecretRecord asks for names from a fresh session of contractApprover's,
// returning the session with the request it made and the pending record that request opened.
func (rig *contractRig) createPendingAgentSecretRecord(t *testing.T, reason string, names ...string) contractSession {
	t.Helper()
	session := rig.newSession(t, contractApprover)
	created := rig.request(t, session, reason, names...)
	if created.State != "pending" || created.RecordID == nil {
		t.Fatalf("create-request = %+v, want pending with a record", created)
	}
	session.RequestID, session.RecordID = created.RequestID, *created.RecordID
	return session
}

// approveOnBrokerPage approves recordID as login on the broker's own approval page, signed in to
// the broker as that person: where a secret request is decided, since no Dispatch route decides one.
func (rig *contractRig) approveOnBrokerPage(t *testing.T, login, recordID string) {
	t.Helper()
	ciphertest.Decide(t, rig.BrokerURL, rig.issuer, login, recordID, true)
}

// approveOnBrokerPageRefused is approveOnBrokerPage for an approval the broker refuses with a
// reason the page names, code being the page's fixed refusal code (ciphertest.DecideRefused).
func (rig *contractRig) approveOnBrokerPageRefused(t *testing.T, login, recordID, code string) {
	t.Helper()
	ciphertest.DecideRefused(t, rig.BrokerURL, rig.issuer, login, recordID, true, code)
}

// grantOf reads requestID as session, which made it, straight from the broker, and answers its
// grant, failing t unless the request was granted.
func (rig *contractRig) grantOf(t *testing.T, session contractSession, requestID string) string {
	t.Helper()
	path := "/v1/requests/" + requestID
	p, err := proof.Sign(session.Key, session.EnrollmentID, http.MethodGet, rig.BrokerURL+path, time.Now())
	if err != nil {
		t.Fatalf("proof.Sign: %v", err)
	}
	status, body := rig.brokerReq(t, http.MethodGet, path, map[string]string{"Proof": p}, nil)
	var read contractRequested
	if err := json.Unmarshal(body, &read); status != http.StatusOK || err != nil || read.State != "granted" || read.GrantID == nil {
		t.Fatalf("GET %s = %d %s (%v), want a granted request with a grant_id", path, status, body, err)
	}
	return *read.GrantID
}

// values reads a grant's released values as the session that holds it, straight from the broker.
func (rig *contractRig) values(t *testing.T, pending contractSession, grantID string) (int, map[string]string) {
	t.Helper()
	path := "/v1/grants/" + grantID + "/values"
	p, err := proof.Sign(pending.Key, pending.EnrollmentID, http.MethodPost, rig.BrokerURL+path, time.Now())
	if err != nil {
		t.Fatalf("proof.Sign: %v", err)
	}
	status, body := rig.brokerReq(t, http.MethodPost, path, map[string]string{"Proof": p}, nil)
	var released struct {
		Values map[string]string `json:"values"`
	}
	_ = json.Unmarshal(body, &released)
	return status, released.Values
}

type contractRecord struct {
	RecordID string `json:"record_id"`
	Kind     string `json:"kind"`
	State    string `json:"state"`
}

// contractError is a broker refusal Dispatch relays; ApproveURL is set on a DECIDE_ON_BROKER_PAGE.
type contractError struct {
	Code       string `json:"code"`
	ApproveURL string `json:"approve_url"`
}

// decisionEvents counts recordID's approved and denied events on the broker.
func (rig *contractRig) decisionEvents(t *testing.T, recordID string) int {
	t.Helper()
	var n int
	if err := rig.brokerStore.Pool.QueryRow(context.Background(),
		`select count(*) from credential_request_events where record_id=$1 and event in ('approved', 'denied')`, recordID).Scan(&n); err != nil {
		t.Fatalf("count %s's decisions: %v", recordID, err)
	}
	return n
}

// --- tests ---

// TestContractDispatchDecidesNoSecretRequest: Dispatch's approve and deny of a pending secret
// request, by its approver or anyone else and whatever the browser's body names, each relay the
// broker's 403 DECIDE_ON_BROKER_PAGE carrying the record's page on the broker, and decide nothing:
// the record stays pending in its approver's inbox. Approved on that page by its approver, signed
// in to the broker, the request releases its value, and Dispatch's approve still decides nothing.
func TestContractDispatchDecidesNoSecretRequest(t *testing.T) {
	rig := newContractRig(t)
	pending := rig.createPendingAgentSecretRecord(t, "need it for the demo", "DEEL_API_KEY")
	page := rig.BrokerURL + "/approve/" + pending.RecordID
	refused := func(action, login string, body any) {
		t.Helper()
		resp := dispatchRequest(t, rig.Dispatch, http.MethodPost, "/api/v1/credential-requests/"+pending.RecordID+"/"+action, body, login)
		if refusal := decodeBody[contractError](t, resp); resp.Code != http.StatusForbidden || refusal.Code != "DECIDE_ON_BROKER_PAGE" || refusal.ApproveURL != page {
			t.Fatalf("%s as %s = %d %s, want 403 DECIDE_ON_BROKER_PAGE naming %s", action, login, resp.Code, resp.Body.String(), page)
		}
	}
	for _, action := range []string{"approve", "deny"} {
		for _, login := range []string{contractApprover, contractOther} {
			refused(action, login, map[string]any{"approver": contractApprover, "code": "ABCD-1234"})
		}
	}
	if n := rig.decisionEvents(t, pending.RecordID); n != 0 {
		t.Fatalf("the record has %d decisions after Dispatch's approves and denies, want none", n)
	}
	if rows := rig.pendingFor(t, contractApprover); len(rows) != 1 || rows[0].RecordID != pending.RecordID {
		t.Fatalf("%s's inbox = %+v, want the record %s still pending", contractApprover, rows, pending.RecordID)
	}

	rig.approveOnBrokerPage(t, contractApprover, pending.RecordID)
	var decidedBy string
	if err := rig.brokerStore.Pool.QueryRow(context.Background(),
		`select login from credential_request_events where record_id=$1 and event='approved'`, pending.RecordID).Scan(&decidedBy); err != nil || decidedBy != contractApprover {
		t.Fatalf("approved event login = %q (%v), want %s", decidedBy, err, contractApprover)
	}
	if status, released := rig.values(t, pending, rig.grantOf(t, pending, pending.RequestID)); status != http.StatusOK || released["DEEL_API_KEY"] != "deel-v1" {
		t.Fatalf("values = %d %v, want DEEL_API_KEY=deel-v1", status, released)
	}
	refused("approve", contractApprover, map[string]any{})
}

// TestContractPendingListShowsCreatedRecordThroughDispatch drives the inbox's own query: a pending
// record shows in Dispatch's proxied ?approver=me list with its minimal facts and the broker's
// page where it is decided.
func TestContractPendingListShowsCreatedRecordThroughDispatch(t *testing.T) {
	rig := newContractRig(t)
	recordID := rig.createPendingAgentSecretRecord(t, "checking the inbox lists a pending record", "DEEL_API_KEY").RecordID

	rows := rig.pendingFor(t, contractApprover)
	if len(rows) != 1 || rows[0].RecordID != recordID || rows[0].Kind != "agent_secret" || rows[0].ApproveURL != rig.BrokerURL+"/approve/"+recordID {
		t.Fatalf("pending list = %+v, want the agent_secret record %s linking its page on the broker", rows, recordID)
	}
}

// TestContractMachineLoginLookupAndApproveThroughDispatch drives the typed-code machine flow
// (approveMachineLogin): a machine's signed login request produces a code; Dispatch's lookup route
// resolves it to the record (decision 13's one selector); approving through Dispatch with the same
// code mints a launcher credential, while without the code the broker refuses CODE_REQUIRED and
// with another code CODE_MISMATCH.
func TestContractMachineLoginLookupAndApproveThroughDispatch(t *testing.T) {
	newContractRig(t).approveMachineLogin(t, contractApprover, "contract-test-host")
}

// TestContractMachineLoginDeniedThroughDispatch: a person denies their machine's login through
// Dispatch with its code, as they approve one: another code is refused CODE_MISMATCH and leaves it
// pending, its own code denies it, and a later approve is the broker's 409 RECORD_TERMINAL.
func TestContractMachineLoginDeniedThroughDispatch(t *testing.T) {
	rig := newContractRig(t)
	machineKey := contractSigningKey(t)
	compact, err := record.Sign(machineKey, rig.BrokerURL,
		[]record.AuthorizationDetail{{Type: "launcher_credential", Identifier: "contract-test-host"}}, "", contractApprover, nil, time.Now())
	if err != nil {
		t.Fatalf("record.Sign: %v", err)
	}
	code, recordID := rig.startMachineLogin(t, machineKey, compact, contractApprover)
	denyPath := "/api/v1/credential-requests/" + recordID + "/deny"
	wrong := dispatchRequest(t, rig.Dispatch, http.MethodPost, denyPath, map[string]any{"code": "WRONG-CODE"}, contractApprover)
	if wrong.Code != http.StatusForbidden || decodeBody[contractError](t, wrong).Code != "CODE_MISMATCH" {
		t.Fatalf("deny with another code = %d %s, want 403 CODE_MISMATCH", wrong.Code, wrong.Body.String())
	}
	if looked := rig.lookupMachineLogin(t, code, contractApprover); looked.State != "pending" {
		t.Fatalf("the login after the refused deny = %+v, want pending", looked)
	}
	denied := dispatchRequest(t, rig.Dispatch, http.MethodPost, denyPath, map[string]any{"code": code}, contractApprover)
	if denied.Code != http.StatusOK || decodeBody[contractRecord](t, denied).State != "denied" {
		t.Fatalf("deny with its code = %d %s, want 200 denied", denied.Code, denied.Body.String())
	}
	again := dispatchRequest(t, rig.Dispatch, http.MethodPost, "/api/v1/credential-requests/"+recordID+"/approve", map[string]any{"code": code}, contractApprover)
	if again.Code != http.StatusConflict || decodeBody[contractError](t, again).Code != "RECORD_TERMINAL" {
		t.Fatalf("approve after the denial = %d %s, want 409 RECORD_TERMINAL", again.Code, again.Body.String())
	}
}

// TestContractGrantsListAndRevokeByApproverThroughDispatch approves a request on the broker's page
// to mint a grant, lists it through Dispatch's own grants route, then revokes it as the signed-in
// approver — refused for another login — the human path that ends a grant, distinct from a session
// ending its own.
func TestContractGrantsListAndRevokeByApproverThroughDispatch(t *testing.T) {
	rig := newContractRig(t)
	pending := rig.createPendingAgentSecretRecord(t, "checking a grant lists and its approver revokes it", "DEEL_API_KEY")
	rig.approveOnBrokerPage(t, contractApprover, pending.RecordID)
	grantID := rig.grantOf(t, pending, pending.RequestID)

	listed := rig.grantsOf(t, contractApprover)
	if len(listed) != 1 || listed[0].GrantID != grantID || listed[0].Granted != "approval" || listed[0].Approver == nil || *listed[0].Approver != contractApprover {
		t.Fatalf("grants list = %s, want the grant %s approved by %s", asJSON(t, listed), grantID, contractApprover)
	}

	revokePath := "/api/v1/credential-grants/" + grantID + "/revoke"
	otherResp := dispatchRequest(t, rig.Dispatch, http.MethodPost, revokePath, map[string]any{}, contractOther)
	if otherResp.Code != http.StatusForbidden || decodeBody[contractError](t, otherResp).Code != "NOT_APPROVER" {
		t.Fatalf("revoke as %s = %d %s, want 403 NOT_APPROVER", contractOther, otherResp.Code, otherResp.Body.String())
	}
	revokeResp := dispatchRequest(t, rig.Dispatch, http.MethodPost, revokePath, map[string]any{}, contractApprover)
	if revokeResp.Code != http.StatusOK {
		t.Fatalf("revoke = %d: %s", revokeResp.Code, revokeResp.Body.String())
	}

	if listed := rig.grantsOf(t, contractApprover); len(listed) != 0 {
		t.Fatalf("grants after the revoke = %s, want none", asJSON(t, listed))
	}
}

// contractPending is one row of Dispatch's pending list.
type contractPending struct {
	RecordID    string    `json:"record_id"`
	Kind        string    `json:"kind"`
	Identifiers []string  `json:"identifiers"`
	ApproveURL  string    `json:"approve_url"`
	State       string    `json:"state"`
	ExpiresAt   time.Time `json:"expires_at"`
}

// pendingFor reads login's credential inbox through Dispatch.
func (rig *contractRig) pendingFor(t *testing.T, login string) []contractPending {
	t.Helper()
	resp := dispatchRequest(t, rig.Dispatch, http.MethodGet, "/api/v1/credential-requests?approver=me", nil, login)
	if resp.Code != http.StatusOK {
		t.Fatalf("GET pending as %s = %d: %s", login, resp.Code, resp.Body.String())
	}
	return decodeBody[struct {
		Pending []contractPending `json:"pending"`
	}](t, resp).Pending
}

// TestContractASharedSecretsRequestIsInItsOperatorsInboxAndAnyoneApprovesIt drives a request
// whose approver is anyone (a shared human-tier secret): through Dispatch it is in the credential
// inbox of its session's operator, the person responsible for the agent that asked, and not in the
// second signed-in person's; that second person, who neither owns nor operates anything, still
// approves it on the broker's page, the broker records that person's email as the deciding login,
// the session gets the value, and the request leaves the inbox.
func TestContractASharedSecretsRequestIsInItsOperatorsInboxAndAnyoneApprovesIt(t *testing.T) {
	rig := newContractRig(t)
	pending := rig.createPendingAgentSecretRecord(t, "deploy the preview", "SHARED_KEY")

	rows := rig.pendingFor(t, contractApprover)
	if len(rows) != 1 || rows[0].RecordID != pending.RecordID || rows[0].Kind != "agent_secret" || len(rows[0].Identifiers) != 1 || rows[0].Identifiers[0] != "SHARED_KEY" {
		t.Fatalf("%s's inbox = %+v, want the SHARED_KEY request %s", contractApprover, rows, pending.RecordID)
	}
	if rows := rig.pendingFor(t, contractOther); len(rows) != 0 {
		t.Fatalf("%s's inbox = %+v, want empty: the request is its session's operator's to be asked", contractOther, rows)
	}

	rig.approveOnBrokerPage(t, contractOther, pending.RecordID)
	var decidedBy string
	if err := rig.brokerStore.Pool.QueryRow(context.Background(),
		`select login from credential_request_events where record_id=$1 and event='approved'`, pending.RecordID).Scan(&decidedBy); err != nil || decidedBy != contractOther {
		t.Fatalf("approved event login = %q (%v), want %s", decidedBy, err, contractOther)
	}
	if status, released := rig.values(t, pending, rig.grantOf(t, pending, pending.RequestID)); status != http.StatusOK || released["SHARED_KEY"] != "shared-v1" {
		t.Fatalf("values = %d %v, want SHARED_KEY=shared-v1", status, released)
	}
	for _, login := range []string{contractApprover, contractOther} {
		if rows := rig.pendingFor(t, login); len(rows) != 0 {
			t.Fatalf("%s's inbox after the approval = %+v, want empty", login, rows)
		}
	}
}

// contractGrant is one row of Dispatch's Live grants list.
type contractGrant struct {
	GrantID  string   `json:"grant_id"`
	Granted  string   `json:"granted"`
	RecordID *string  `json:"record_id"`
	Approver *string  `json:"approver"`
	Names    []string `json:"names"`
}

// grantsOf reads login's Live grants through Dispatch.
func (rig *contractRig) grantsOf(t *testing.T, login string) []contractGrant {
	t.Helper()
	resp := dispatchRequest(t, rig.Dispatch, http.MethodGet, "/api/v1/credential-grants?approver=me", nil, login)
	if resp.Code != http.StatusOK {
		t.Fatalf("GET grants as %s = %d: %s", login, resp.Code, resp.Body.String())
	}
	return decodeBody[struct {
		Grants []contractGrant `json:"grants"`
	}](t, resp).Grants
}

// asJSON renders v for a failure message, so a pointer field reads as its value or null rather
// than as an address.
func asJSON(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %T: %v", v, err)
	}
	return string(data)
}

// TestContractAnAutomaticGrantIsListedRevokedAndThenAsksItsOwner drives an automatic grant through
// Dispatch's Live grants: the person whose session holds it sees it listed as automatic, with no
// approver and no record, and nobody else does; another person cannot revoke it and its operator
// can, through the same route as an approved grant; after that, the same session's next request for
// the secret is an approval request to its owner, while another of the person's sessions still
// gets it at once.
func TestContractAnAutomaticGrantIsListedRevokedAndThenAsksItsOwner(t *testing.T) {
	rig := newContractRig(t)
	session := rig.newSession(t, contractApprover)
	auto := rig.request(t, session, "", "AUTO_TOKEN")
	if auto.State != "granted" || auto.GrantID == nil || auto.RecordID != nil {
		t.Fatalf("request = %s, want an automatic grant", asJSON(t, auto))
	}
	grantID := *auto.GrantID

	listed := rig.grantsOf(t, contractApprover)
	if len(listed) != 1 || listed[0].GrantID != grantID || listed[0].Granted != "automatic" || listed[0].Approver != nil || listed[0].RecordID != nil ||
		len(listed[0].Names) != 1 || listed[0].Names[0] != "AUTO_TOKEN" {
		t.Fatalf("%s's grants = %s, want the AUTO_TOKEN grant %s listed as automatic", contractApprover, asJSON(t, listed), grantID)
	}
	if others := rig.grantsOf(t, contractOther); len(others) != 0 {
		t.Fatalf("%s's grants = %s, want none", contractOther, asJSON(t, others))
	}

	revokePath := "/api/v1/credential-grants/" + grantID + "/revoke"
	otherRevoke := dispatchRequest(t, rig.Dispatch, http.MethodPost, revokePath, map[string]any{}, contractOther)
	if otherRevoke.Code != http.StatusForbidden || decodeBody[contractError](t, otherRevoke).Code != "NOT_APPROVER" {
		t.Fatalf("revoke as %s = %d %s, want 403 NOT_APPROVER", contractOther, otherRevoke.Code, otherRevoke.Body.String())
	}
	if resp := dispatchRequest(t, rig.Dispatch, http.MethodPost, revokePath, map[string]any{}, contractApprover); resp.Code != http.StatusOK {
		t.Fatalf("revoke = %d: %s", resp.Code, resp.Body.String())
	}
	if status, _ := rig.values(t, session, grantID); status != http.StatusForbidden {
		t.Fatalf("values after the revoke = %d, want 403", status)
	}
	if listed := rig.grantsOf(t, contractApprover); len(listed) != 0 {
		t.Fatalf("grants after the revoke = %s, want none", asJSON(t, listed))
	}

	again := rig.request(t, session, "need it again", "AUTO_TOKEN")
	if again.State != "pending" || again.RecordID == nil || again.GrantID != nil {
		t.Fatalf("the same session's next request = %s, want an approval request", asJSON(t, again))
	}
	if rows := rig.pendingFor(t, contractApprover); len(rows) != 1 || rows[0].RecordID != *again.RecordID {
		t.Fatalf("the owner's inbox = %+v, want the request %s", rows, *again.RecordID)
	}
	if rows := rig.pendingFor(t, contractOther); len(rows) != 0 {
		t.Fatalf("%s's inbox = %+v, want none: the request waits on its owner alone", contractOther, rows)
	}

	elsewhere := rig.request(t, rig.newSession(t, contractApprover), "", "AUTO_TOKEN")
	if elsewhere.State != "granted" || elsewhere.GrantID == nil {
		t.Fatalf("another session's request = %s, want an automatic grant", asJSON(t, elsewhere))
	}
}

// approveMachineLogin logs approver's own machine in as a person does: the machine asks the
// broker's device authorization for its signed login, naming approver in its login_hint; approver looks its code
// up through Dispatch (decision 13's one selector), which answers the pending launcher_credential
// record; an approval without the code is refused CODE_REQUIRED, and one with another code
// CODE_MISMATCH; and approver's approval with the same code mints a launcher credential. It returns
// the launcher credential the approval minted and the machine's key. A service logs in with its own
// token instead (tokenLogin).
func (rig *contractRig) approveMachineLogin(t *testing.T, approver, host string) (string, *ecdsa.PrivateKey) {
	t.Helper()
	machineKey := contractSigningKey(t)
	compact, err := record.Sign(machineKey, rig.BrokerURL,
		[]record.AuthorizationDetail{{Type: "launcher_credential", Identifier: host}}, "", approver, nil, time.Now())
	if err != nil {
		t.Fatalf("record.Sign: %v", err)
	}
	code, recordID := rig.startMachineLogin(t, machineKey, compact, approver)
	approvePath := "/api/v1/credential-requests/" + recordID + "/approve"
	noCode := dispatchRequest(t, rig.Dispatch, http.MethodPost, approvePath, map[string]any{}, approver)
	if noCode.Code != http.StatusBadRequest || decodeBody[contractError](t, noCode).Code != "CODE_REQUIRED" {
		t.Fatalf("machine approve without its code = %d %s, want 400 CODE_REQUIRED", noCode.Code, noCode.Body.String())
	}
	wrongCode := dispatchRequest(t, rig.Dispatch, http.MethodPost, approvePath, map[string]any{"code": "WRONG-CODE"}, approver)
	if wrongCode.Code != http.StatusForbidden || decodeBody[contractError](t, wrongCode).Code != "CODE_MISMATCH" {
		t.Fatalf("machine approve with another code = %d %s, want 403 CODE_MISMATCH", wrongCode.Code, wrongCode.Body.String())
	}
	approveResp := dispatchRequest(t, rig.Dispatch, http.MethodPost, approvePath, map[string]any{"code": code}, approver)
	approved := decodeBody[struct {
		State        string  `json:"state"`
		CredentialID *string `json:"credential_id"`
	}](t, approveResp)
	if approveResp.Code != http.StatusOK || approved.State != "approved" || approved.CredentialID == nil || *approved.CredentialID == "" {
		t.Fatalf("approve the machine login = %d %s, want state=approved with a credential_id", approveResp.Code, approveResp.Body.String())
	}
	return *approved.CredentialID, machineKey
}

// startMachineLogin asks the broker's device authorization for the machine's signed login, with
// dpop_jkt key's thumbprint as the helper sends it, and has looker look its user code up through
// Dispatch, which must answer the pending launcher_credential record. It returns the code and the
// record's id.
func (rig *contractRig) startMachineLogin(t *testing.T, key *ecdsa.PrivateKey, compact, looker string) (code, recordID string) {
	t.Helper()
	status, body := rig.deviceAuthorize(t, key, compact)
	var created struct {
		UserCode string `json:"user_code"`
	}
	if err := json.Unmarshal(body, &created); status != http.StatusOK || err != nil || created.UserCode == "" {
		t.Fatalf("POST /v1/oauth/device_authorization = %d %s (%v), want a user code", status, body, err)
	}
	looked := rig.lookupMachineLogin(t, created.UserCode, looker)
	if looked.Kind != "launcher_credential" || looked.State != "pending" {
		t.Fatalf("machine-lookup = %+v, want a pending launcher_credential record", looked)
	}
	return created.UserCode, looked.RecordID
}

// deviceAuthorize posts compact, signed by key, to the broker's POST /v1/oauth/device_authorization
// as the helper's `machine login` does, answering the status and body.
func (rig *contractRig) deviceAuthorize(t *testing.T, key *ecdsa.PrivateKey, compact string) (int, []byte) {
	t.Helper()
	jkt, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{"client_id": {oauthwire.ClientID}, "scope": {oauthwire.Scope}, "request": {compact}, "dpop_jkt": {jkt}}
	response, err := http.Post(rig.BrokerURL+"/v1/oauth/device_authorization", "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("POST /v1/oauth/device_authorization: %v", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, body
}

// lookupMachineLogin looks a machine login up by its code through Dispatch, as looker.
func (rig *contractRig) lookupMachineLogin(t *testing.T, code, looker string) contractRecord {
	t.Helper()
	lookupResp := dispatchRequest(t, rig.Dispatch, http.MethodPost, "/api/v1/credential-requests/machine-lookup",
		map[string]any{"code": code}, looker)
	if lookupResp.Code != http.StatusOK {
		t.Fatalf("machine-lookup = %d: %s", lookupResp.Code, lookupResp.Body.String())
	}
	return decodeBody[contractRecord](t, lookupResp)
}

// enrollWithLauncher enrolls a session under credentialID straight on the broker, as the machine's
// launcher does with a launcher proof signed by machineKey, and answers the status, body and session.
func (rig *contractRig) enrollWithLauncher(t *testing.T, machineKey *ecdsa.PrivateKey, credentialID, runtimeID string) (int, []byte, contractSession) {
	t.Helper()
	key := contractSigningKey(t)
	thumbprint, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		t.Fatalf("thumbprint: %v", err)
	}
	p, err := proof.SignLauncher(machineKey, credentialID, http.MethodPost, rig.BrokerURL+"/v1/enrollments", time.Now())
	if err != nil {
		t.Fatalf("proof.SignLauncher: %v", err)
	}
	status, body := rig.brokerReq(t, http.MethodPost, "/v1/enrollments", map[string]string{"Proof": p},
		map[string]any{"kind": "session", "runtime_id": runtimeID, "thumbprint": thumbprint})
	var enrolled struct {
		EnrollmentID string `json:"enrollment_id"`
	}
	_ = json.Unmarshal(body, &enrolled)
	return status, body, contractSession{EnrollmentID: enrolled.EnrollmentID, Key: key}
}

// contractMachineLogin is one row of Dispatch's machine-login list.
type contractMachineLogin struct {
	CredentialID string    `json:"credential_id"`
	Host         string    `json:"host"`
	Service      *string   `json:"service"`
	Subject      *string   `json:"subject"`
	ApprovedBy   string    `json:"approved_by"`
	IssuedAt     time.Time `json:"issued_at"`
	ExpiresAt    time.Time `json:"expires_at"`
}

// machineLoginsOf reads login's machine logins through Dispatch.
func (rig *contractRig) machineLoginsOf(t *testing.T, login string) []contractMachineLogin {
	t.Helper()
	resp := dispatchRequest(t, rig.Dispatch, http.MethodGet, "/api/v1/machine-logins", nil, login)
	if resp.Code != http.StatusOK {
		t.Fatalf("GET machine logins as %s = %d: %s", login, resp.Code, resp.Body.String())
	}
	return decodeBody[struct {
		Credentials []contractMachineLogin `json:"credentials"`
	}](t, resp).Credentials
}

// TestContractAPersonRevokesTheirMachineLoginThroughDispatch drives the machine-login page's
// routes against the real broker: the person who approved their machine's login sees it listed,
// and nobody else does; another person's revoke is refused whatever their browser's body names;
// the person's own revoke ends it, and with it the session the machine enrolled, whose grant then
// releases nothing and whose machine enrolls no one more.
func TestContractAPersonRevokesTheirMachineLoginThroughDispatch(t *testing.T) {
	rig := newContractRig(t)
	credentialID, machineKey := rig.approveMachineLogin(t, contractApprover, "contract-test-host")
	status, body, session := rig.enrollWithLauncher(t, machineKey, credentialID, "box-"+t.Name())
	if status != http.StatusCreated {
		t.Fatalf("enroll under the machine login = %d: %s", status, body)
	}
	auto := rig.request(t, session, "", "AUTO_TOKEN")
	if auto.State != "granted" || auto.GrantID == nil {
		t.Fatalf("request = %+v, want an automatic grant", auto)
	}

	if logins := rig.machineLoginsOf(t, contractApprover); len(logins) != 1 || logins[0].CredentialID != credentialID || logins[0].Host != "contract-test-host" || logins[0].Service != nil {
		t.Fatalf("%s's machine logins = %+v, want the one approved for contract-test-host, no service", contractApprover, logins)
	}
	if logins := rig.machineLoginsOf(t, contractOther); len(logins) != 0 {
		t.Fatalf("%s's machine logins = %+v, want none", contractOther, logins)
	}

	revokePath := "/api/v1/machine-logins/" + credentialID + "/revoke"
	resp := dispatchRequest(t, rig.Dispatch, http.MethodPost, revokePath, map[string]any{"approver": contractApprover}, contractOther)
	if resp.Code != http.StatusForbidden || decodeBody[contractError](t, resp).Code != "NOT_APPROVER" {
		t.Fatalf("revoke as %s = %d %s, want 403 NOT_APPROVER", contractOther, resp.Code, resp.Body.String())
	}
	if status, released := rig.values(t, session, *auto.GrantID); status != http.StatusOK || released["AUTO_TOKEN"] != "auto-v1" {
		t.Fatalf("values after the refused revoke = %d %v, want AUTO_TOKEN released", status, released)
	}

	resp = dispatchRequest(t, rig.Dispatch, http.MethodPost, revokePath, map[string]any{}, contractApprover)
	if resp.Code != http.StatusOK || decodeBody[struct {
		State string `json:"state"`
	}](t, resp).State != "revoked" {
		t.Fatalf("revoke as %s = %d %s, want 200 revoked", contractApprover, resp.Code, resp.Body.String())
	}
	if status, _ := rig.values(t, session, *auto.GrantID); status != http.StatusUnauthorized {
		t.Fatalf("values after the revoke = %d, want 401: the session ended", status)
	}
	if status, body, _ := rig.enrollWithLauncher(t, machineKey, credentialID, "box-after-"+t.Name()); status != http.StatusUnauthorized || !strings.Contains(string(body), "LAUNCHER_INVALID") {
		t.Fatalf("enroll after the revoke = %d %s, want 401 LAUNCHER_INVALID", status, body)
	}
	if logins := rig.machineLoginsOf(t, contractApprover); len(logins) != 0 {
		t.Fatalf("%s's machine logins after the revoke = %+v, want none", contractApprover, logins)
	}
}

// tokenLogin logs the daemon in on the broker with a fresh token for contractDaemonAccount and a
// request object a fresh key signed, as the Legion daemon does at boot, and answers the credential
// id and the key. No Dispatch route is involved: no person approves a token login.
func (rig *contractRig) tokenLogin(t *testing.T) (string, *ecdsa.PrivateKey) {
	t.Helper()
	claims := rig.issuer.Claims(contractDaemonAccount, contractTokenAudience)
	claims["iat"] = time.Now().Add(-5 * time.Second).Unix()
	claims["exp"] = time.Now().Add(time.Hour).Unix()
	key := contractSigningKey(t)
	compact, err := record.Sign(key, rig.BrokerURL,
		[]record.AuthorizationDetail{{Type: "launcher_credential", Identifier: "legion-daemon", Service: "legion-daemon"}}, "", "", nil, time.Now())
	if err != nil {
		t.Fatalf("record.Sign: %v", err)
	}
	status, body := rig.brokerReq(t, http.MethodPost, "/v1/launcher-credentials/token", nil,
		map[string]any{"token": rig.issuer.Mint(t, rig.issuerKey, claims), "request": compact})
	var login struct {
		CredentialID string `json:"credential_id"`
	}
	if err := json.Unmarshal(body, &login); status != http.StatusOK || err != nil || login.CredentialID == "" {
		t.Fatalf("POST /v1/launcher-credentials/token = %d %s (%v), want a credential", status, body, err)
	}
	return login.CredentialID, key
}

// TestContractAServicesTokenLoginIsListedAndRevokedByAnyoneThroughDispatch: the Legion daemon's
// token login, which no person approves and which has no operator, is listed through Dispatch for
// every signed-in person, naming its service and its ServiceAccount and no approver, and its
// launcher authenticates; another person's revoke, whoever their browser's body names, ends it,
// after which its launcher proofs authenticate nothing; and a device-code login naming the
// service, which an older daemon would open, is refused SERVICE_LOGS_IN_WITH_TOKEN with nothing
// for Dispatch to show.
func TestContractAServicesTokenLoginIsListedAndRevokedByAnyoneThroughDispatch(t *testing.T) {
	rig := newContractRig(t)
	credentialID, machineKey := rig.tokenLogin(t)

	for _, person := range []string{contractOther, contractApprover} {
		logins := rig.machineLoginsOf(t, person)
		if len(logins) != 1 || logins[0].CredentialID != credentialID || logins[0].Host != "legion-system/legion-daemon-contract" ||
			logins[0].Service == nil || *logins[0].Service != "legion-daemon" || logins[0].ApprovedBy != "" ||
			logins[0].Subject == nil || *logins[0].Subject != contractDaemonAccount {
			t.Fatalf("%s's machine logins = %s, want legion-daemon's token login for %s, approved by no one", person, asJSON(t, logins), contractDaemonAccount)
		}
	}
	// A service's credential enrolls pods only, so a box is refused; what matters is that its
	// launcher proof authenticates.
	if status, body, _ := rig.enrollWithLauncher(t, machineKey, credentialID, "box-"+t.Name()); status != http.StatusForbidden || !strings.Contains(string(body), "OPERATOR_MISMATCH") {
		t.Fatalf("enroll under the token login = %d %s, want 403 OPERATOR_MISMATCH from a launcher that authenticates", status, body)
	}

	revokePath := "/api/v1/machine-logins/" + credentialID + "/revoke"
	resp := dispatchRequest(t, rig.Dispatch, http.MethodPost, revokePath, map[string]any{"approver": contractOther}, contractApprover)
	if resp.Code != http.StatusOK {
		t.Fatalf("revoke as %s = %d %s, want 200", contractApprover, resp.Code, resp.Body.String())
	}
	if status, body, _ := rig.enrollWithLauncher(t, machineKey, credentialID, "box-after-"+t.Name()); status != http.StatusUnauthorized || !strings.Contains(string(body), "LAUNCHER_INVALID") {
		t.Fatalf("enroll after the revoke = %d %s, want 401 LAUNCHER_INVALID", status, body)
	}
	for _, person := range []string{contractOther, contractApprover} {
		if logins := rig.machineLoginsOf(t, person); len(logins) != 0 {
			t.Fatalf("%s's machine logins after the revoke = %s, want none", person, asJSON(t, logins))
		}
	}

	daemonKey := contractSigningKey(t)
	compact, err := record.Sign(daemonKey, rig.BrokerURL,
		[]record.AuthorizationDetail{{Type: "launcher_credential", Identifier: "cluster.example", Service: "legion-daemon"}}, "", "", nil, time.Now())
	if err != nil {
		t.Fatalf("record.Sign: %v", err)
	}
	status, body := rig.deviceAuthorize(t, daemonKey, compact)
	if status != http.StatusBadRequest || !strings.Contains(string(body), "SERVICE_LOGS_IN_WITH_TOKEN") {
		t.Fatalf("a service's device-code login = %d %s, want 400 SERVICE_LOGS_IN_WITH_TOKEN", status, body)
	}
}

// TestContractAPersonsMachineLoginIsTheirsAloneToApproveThroughDispatch: a person's own machine
// login is refused NOT_APPROVER to every other signed-in person, and stays pending for the person
// it names.
func TestContractAPersonsMachineLoginIsTheirsAloneToApproveThroughDispatch(t *testing.T) {
	rig := newContractRig(t)
	machineKey := contractSigningKey(t)
	compact, err := record.Sign(machineKey, rig.BrokerURL,
		[]record.AuthorizationDetail{{Type: "launcher_credential", Identifier: "contract-test-host"}}, "", contractApprover, nil, time.Now())
	if err != nil {
		t.Fatalf("record.Sign: %v", err)
	}
	code, recordID := rig.startMachineLogin(t, machineKey, compact, contractOther)
	resp := dispatchRequest(t, rig.Dispatch, http.MethodPost, "/api/v1/credential-requests/"+recordID+"/approve", map[string]any{"code": code}, contractOther)
	if resp.Code != http.StatusForbidden || decodeBody[contractError](t, resp).Code != "NOT_APPROVER" {
		t.Fatalf("approve %s's machine login as %s = %d %s, want 403 NOT_APPROVER", contractApprover, contractOther, resp.Code, resp.Body.String())
	}
	if looked := rig.lookupMachineLogin(t, code, contractApprover); looked.State != "pending" {
		t.Fatalf("the login after the refused approval = %+v, want pending", looked)
	}
}

// TestContractAnyoneApprovalCannotReleaseAPersonsWithheldSecret drives a request that was pending
// when its session's operator withheld one of its names: the session's {AUTO_TOKEN, SHARED_KEY}
// request waits on anyone and is in its operator's Inbox through Dispatch, the operator revokes
// the session's automatic AUTO_TOKEN grant from Live grants through Dispatch, and a second
// person, who owns nothing, approving it on the broker's page decides nothing; the operator,
// AUTO_TOKEN's owner, approves it there and the session reads AUTO_TOKEN on that approval.
func TestContractAnyoneApprovalCannotReleaseAPersonsWithheldSecret(t *testing.T) {
	rig := newContractRig(t)
	session := rig.newSession(t, contractApprover)
	auto := rig.request(t, session, "", "AUTO_TOKEN")
	if auto.State != "granted" || auto.GrantID == nil {
		t.Fatalf("request = %s, want an automatic grant", asJSON(t, auto))
	}
	mixed := rig.request(t, session, "deploy", "AUTO_TOKEN", "SHARED_KEY")
	if mixed.State != "pending" || mixed.RecordID == nil {
		t.Fatalf("mixed request = %s, want pending", asJSON(t, mixed))
	}
	if rows := rig.pendingFor(t, contractApprover); len(rows) != 1 || rows[0].RecordID != *mixed.RecordID {
		t.Fatalf("%s's inbox = %+v, want the mixed request", contractApprover, rows)
	}
	if resp := dispatchRequest(t, rig.Dispatch, http.MethodPost, "/api/v1/credential-grants/"+*auto.GrantID+"/revoke", map[string]any{}, contractApprover); resp.Code != http.StatusOK {
		t.Fatalf("revoke = %d: %s", resp.Code, resp.Body.String())
	}
	// The current policy has the operator approve AUTO_TOKEN now that it is withheld, so the
	// broker refuses the second person's approval and the page names that reason ("policy").
	rig.approveOnBrokerPageRefused(t, contractOther, *mixed.RecordID, "policy")
	if n := rig.decisionEvents(t, *mixed.RecordID); n != 0 {
		t.Fatalf("the request has %d decisions after %s approved it once %s withheld AUTO_TOKEN, want none", n, contractOther, contractApprover)
	}
	rig.approveOnBrokerPage(t, contractApprover, *mixed.RecordID)
	if status, released := rig.values(t, session, rig.grantOf(t, session, mixed.RequestID)); status != http.StatusOK || released["AUTO_TOKEN"] == "" {
		t.Fatalf("values of the owner's approval = %d %v, want AUTO_TOKEN released", status, released)
	}
}

// --- expired requests: check c-expired-request-listed through Dispatch (dispatch://INFRA-3921) ---

// contractDismissal is the dismiss route's answer.
type contractDismissal struct {
	RecordID    string    `json:"record_id"`
	DismissedBy string    `json:"dismissed_by"`
	DismissedAt time.Time `json:"dismissed_at"`
}

// inboxFor reads login's credential inbox through Dispatch and fails t unless it lists exactly
// want, each row as "<record id>=<state>" in the list's own order. It answers the rows.
func (rig *contractRig) inboxFor(t *testing.T, login string, want ...string) []contractPending {
	t.Helper()
	rows := rig.pendingFor(t, login)
	got := make([]string, len(rows))
	for i, row := range rows {
		got[i] = row.RecordID + "=" + row.State
	}
	if !slices.Equal(got, want) {
		t.Fatalf("%s's inbox = %v, want %v", login, got, want)
	}
	return rows
}

// overdue moves requestID's deadline an hour into the past, as time passing does, and runs no
// sweep, so its request row still reads pending while every decision of it already answers that
// it expired. It answers the new deadline.
func (rig *contractRig) overdue(t *testing.T, requestID string) time.Time {
	t.Helper()
	var deadline time.Time
	if err := rig.brokerStore.Pool.QueryRow(context.Background(), `update requests set pending_expires_at = now() - interval '1 hour'
		where id=$1 returning pending_expires_at`, requestID).Scan(&deadline); err != nil {
		t.Fatalf("move request %s past its deadline: %v", requestID, err)
	}
	return deadline
}

// expiredMachineLogin writes a machine login of contractApprover's for host whose deadline
// passed an hour ago, with the typed code code, as the broker's device authorization writes one.
// A machine login's record is append-only, so its deadline is set as it is written. It answers
// the record's id and its deadline.
func (rig *contractRig) expiredMachineLogin(t *testing.T, host, code string) (string, time.Time) {
	t.Helper()
	compact, err := record.Sign(contractSigningKey(t), rig.BrokerURL,
		[]record.AuthorizationDetail{{Type: record.KindLauncherCredential, Identifier: host}}, "", contractApprover, nil, time.Now())
	if err != nil {
		t.Fatalf("record.Sign: %v", err)
	}
	body := record.Body{
		Request:         compact,
		Approver:        contractApprover,
		Enrollment:      record.Enrollment{Kind: "-", RuntimeID: "-"},
		LifetimeSeconds: new(3600),
		RulesVersion:    "rules-v1",
		ExpiresAt:       time.Now().Add(-time.Hour).UTC().Truncate(time.Second),
		Code:            code,
	}
	if _, err := rig.brokerStore.Pool.Exec(context.Background(), `insert into credential_requests (id, body, kind, approver, code, expires_at)
		values ($1,$2,'launcher_credential',$3,$4,$5)`, body.ID(), body.Canonical(), body.Approver, body.Code, body.ExpiresAt); err != nil {
		t.Fatalf("insert the expired machine login for %s: %v", host, err)
	}
	return body.ID(), body.ExpiresAt
}

// dismissThroughDispatch dismisses recordID through Dispatch as login, whose browser sends body,
// and fails t unless Dispatch relays the broker's 200 naming login as the one who dismissed it.
func (rig *contractRig) dismissThroughDispatch(t *testing.T, recordID, login string, body any) {
	t.Helper()
	resp := dispatchRequest(t, rig.Dispatch, http.MethodPost, "/api/v1/credential-requests/"+recordID+"/dismiss", body, login)
	dismissal := decodeBody[contractDismissal](t, resp)
	if resp.Code != http.StatusOK || dismissal.RecordID != recordID || dismissal.DismissedBy != login || dismissal.DismissedAt.IsZero() {
		t.Fatalf("dismiss %s as %s = %d %s, want 200 naming it, dismissed by %s, with its time", recordID, login, resp.Code, resp.Body.String(), login)
	}
}

// TestContractExpiredSecretRequestStaysInTheInboxUntilDismissed is check
// c-expired-request-listed's sequence for a secret request, through Dispatch's own routes and the
// real broker. Two requests wait on one approver, both sessions live, and one is moved past its
// deadline with no sweep run: the approver's inbox holds both, the live one pending and first, the
// overdue one expired with its deadline. Dispatch's approve and deny of the overdue one are still
// the broker's 403 DECIDE_ON_BROKER_PAGE. After the broker's sweep ends the overdue request the
// inbox still holds both, as they were. Another person's dismissal is refused whatever their
// browser's body names; the approver's dismissal answers 200, and the inbox then holds the live
// request alone.
func TestContractExpiredSecretRequestStaysInTheInboxUntilDismissed(t *testing.T) {
	rig := newContractRig(t)
	late := rig.createPendingAgentSecretRecord(t, "needed before its deadline", "DEEL_API_KEY")
	live := rig.createPendingAgentSecretRecord(t, "needed now", "DEEL_API_KEY")
	deadline := rig.overdue(t, late.RequestID)
	both := []string{live.RecordID + "=pending", late.RecordID + "=expired"}

	rows := rig.inboxFor(t, contractApprover, both...)
	if !rows[0].ExpiresAt.After(time.Now()) || rows[0].ApproveURL != rig.BrokerURL+"/approve/"+live.RecordID {
		t.Fatalf("the live row = %+v, want a deadline still ahead and its page on the broker", rows[0])
	}
	if !rows[1].ExpiresAt.Equal(deadline) || rows[1].Kind != "agent_secret" || !slices.Equal(rows[1].Identifiers, []string{"DEEL_API_KEY"}) {
		t.Fatalf("the overdue row = %+v, want the DEEL_API_KEY request with its deadline %s", rows[1], deadline)
	}
	page := rig.BrokerURL + "/approve/" + late.RecordID
	for _, action := range []string{"approve", "deny"} {
		resp := dispatchRequest(t, rig.Dispatch, http.MethodPost, "/api/v1/credential-requests/"+late.RecordID+"/"+action, map[string]any{}, contractApprover)
		if refusal := decodeBody[contractError](t, resp); resp.Code != http.StatusForbidden || refusal.Code != "DECIDE_ON_BROKER_PAGE" || refusal.ApproveURL != page {
			t.Fatalf("%s the overdue request = %d %s, want 403 DECIDE_ON_BROKER_PAGE naming %s", action, resp.Code, resp.Body.String(), page)
		}
	}
	if n := rig.decisionEvents(t, late.RecordID); n != 0 {
		t.Fatalf("the overdue request has %d decisions after Dispatch's approve and deny, want none", n)
	}

	if n, err := rig.requestMachine.ExpirePending(context.Background(), time.Now()); err != nil || n != 1 {
		t.Fatalf("the broker's sweep = %d, %v; want the overdue request alone expired", n, err)
	}
	rig.inboxFor(t, contractApprover, both...)

	refused := dispatchRequest(t, rig.Dispatch, http.MethodPost, "/api/v1/credential-requests/"+late.RecordID+"/dismiss",
		map[string]any{"approver": contractApprover}, contractOther)
	if refused.Code != http.StatusForbidden || decodeBody[contractError](t, refused).Code != "NOT_APPROVER" {
		t.Fatalf("dismiss as %s = %d %s, want 403 NOT_APPROVER", contractOther, refused.Code, refused.Body.String())
	}
	rig.inboxFor(t, contractApprover, both...)

	rig.dismissThroughDispatch(t, late.RecordID, contractApprover, nil)
	rig.inboxFor(t, contractApprover, live.RecordID+"=pending")
}

// TestContractExpiredMachineLoginStaysInTheInboxUntilDismissed is the same sequence for a machine
// login: one written with a deadline already passed beside a live one the machine opened through
// the broker's device authorization. The approver's inbox holds both, the live one pending and
// first, the overdue one expired with its machine and its deadline and no page. Dispatch's approve
// and deny of the overdue one, with its own code, are the broker's 409 RECORD_TERMINAL and write
// nothing. After the broker's machine-login sweep records it expired, the inbox still holds both;
// the approver's dismissal answers 200, and the inbox then holds the live login alone.
func TestContractExpiredMachineLoginStaysInTheInboxUntilDismissed(t *testing.T) {
	rig := newContractRig(t)
	const expiredCode = "BCDF-GHJK"
	expired, deadline := rig.expiredMachineLogin(t, "contract-expired-host", expiredCode)
	machineKey := contractSigningKey(t)
	compact, err := record.Sign(machineKey, rig.BrokerURL,
		[]record.AuthorizationDetail{{Type: record.KindLauncherCredential, Identifier: "contract-live-host"}}, "", contractApprover, nil, time.Now())
	if err != nil {
		t.Fatalf("record.Sign: %v", err)
	}
	_, live := rig.startMachineLogin(t, machineKey, compact, contractApprover)
	both := []string{live + "=pending", expired + "=expired"}

	rows := rig.inboxFor(t, contractApprover, both...)
	if !rows[0].ExpiresAt.After(time.Now()) {
		t.Fatalf("the live row = %+v, want a deadline still ahead", rows[0])
	}
	if !rows[1].ExpiresAt.Equal(deadline) || rows[1].Kind != record.KindLauncherCredential ||
		!slices.Equal(rows[1].Identifiers, []string{"contract-expired-host"}) || rows[1].ApproveURL != "" {
		t.Fatalf("the overdue row = %+v, want the machine contract-expired-host with its deadline %s and no page", rows[1], deadline)
	}
	for _, action := range []string{"approve", "deny"} {
		resp := dispatchRequest(t, rig.Dispatch, http.MethodPost, "/api/v1/credential-requests/"+expired+"/"+action, map[string]any{"code": expiredCode}, contractApprover)
		if resp.Code != http.StatusConflict || decodeBody[contractError](t, resp).Code != "RECORD_TERMINAL" {
			t.Fatalf("%s the overdue machine login = %d %s, want 409 RECORD_TERMINAL", action, resp.Code, resp.Body.String())
		}
	}
	if n := rig.decisionEvents(t, expired); n != 0 {
		t.Fatalf("the overdue machine login has %d decisions after Dispatch's approve and deny, want none", n)
	}

	if err := rig.machineLogins.ExpirePending(context.Background(), time.Now()); err != nil {
		t.Fatalf("the broker's machine-login sweep: %v", err)
	}
	var swept int
	if err := rig.brokerStore.Pool.QueryRow(context.Background(),
		`select count(*) from credential_request_events where record_id=$1 and event='expired'`, expired).Scan(&swept); err != nil || swept != 1 {
		t.Fatalf("the sweep wrote %d expired events for the overdue login (%v), want one", swept, err)
	}
	rig.inboxFor(t, contractApprover, both...)

	rig.dismissThroughDispatch(t, expired, contractApprover, map[string]any{"approver": contractOther})
	rig.inboxFor(t, contractApprover, live+"=pending")
}

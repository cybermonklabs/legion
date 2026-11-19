package brokertest

import (
	"bytes"
	"crypto/ecdsa"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"slices"
	"time"

	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// TB is the part of testing.TB a Pods calls, so a command drives a running broker with the same
// code a test does: Dispatch's browser suite seeds its pending requests through ./requestseed.
type TB interface {
	Helper()
	Fatalf(format string, args ...any)
	Logf(format string, args ...any)
}

// Pods speaks HTTP to a running broker binary at Base as a service and its pods do: a service
// logs in with its token, its launcher credential enrolls pods that prove their accounts with
// projected tokens, and a pod asks for a secret, reads its request's state and a grant's values,
// and cancels a request. Each step logs its request and response, and fails T when the broker
// answers outside the protocol; both name a body that is not the broker's own with every
// credential the request carried redacted (answered.text).
type Pods struct {
	T    TB
	Base string
	// Token is account's projected ServiceAccount token for the audience the broker takes pod
	// tokens for (BROKER_K8S_OIDC_AUDIENCE), bound to the pod podUID, or to no pod when podUID is
	// empty: IssuerTokens for an issuer in the test's own process.
	Token func(account, podUID string) string
}

// IssuerTokens is a Pods' Token, minted by issuer with key for audience.
func IssuerTokens(t oidctest.TB, issuer *oidctest.Issuer, key *oidctest.Key, audience string) func(account, podUID string) string {
	return func(account, podUID string) string {
		claims := issuer.Claims(account, audience)
		if podUID != "" {
			claims["kubernetes.io"] = map[string]any{"pod": map[string]any{"uid": podUID}}
		}
		return issuer.Mint(t, key, claims)
	}
}

// Launcher is a service's launcher credential and the key that proves it.
type Launcher struct {
	Key          *ecdsa.PrivateKey
	CredentialID string
}

// Pod is an enrolled pod's runtime ID, enrollment and the key that proves it.
type Pod struct {
	RuntimeID    string
	Key          *ecdsa.PrivateKey
	EnrollmentID string
}

// RequestReply is the broker's answer to POST /v1/requests and GET /v1/requests/{id}.
type RequestReply struct {
	RequestID  string          `json:"request_id"`
	State      string          `json:"state"`
	GrantID    *string         `json:"grant_id"`
	RecordID   *string         `json:"record_id"`
	Approver   json.RawMessage `json:"approver"`
	ApproveURL string          `json:"approve_url"`
	// Detail is the broker's own one sentence about why it denied the request, empty when it
	// denied none.
	Detail string `json:"detail"`
}

// LogIn logs service in with a token for account and returns its launcher credential.
func (b Pods) LogIn(service, account string) Launcher {
	b.T.Helper()
	key := b.newKey()
	loginRequest, err := record.Sign(key, b.Base, []record.AuthorizationDetail{
		{Type: record.KindLauncherCredential, Identifier: "example-host-" + service, Service: service},
	}, "", "", nil, time.Now())
	if err != nil {
		b.T.Fatalf("sign %s's login request: %v", service, err)
	}
	token := b.Token(account, "")
	got := b.send(http.MethodPost, "/v1/launcher-credentials/token", nil, map[string]any{
		"token":   token,
		"request": loginRequest,
	}, token, loginRequest)
	var login struct {
		CredentialID string `json:"credential_id"`
	}
	b.decode(got, &login)
	if got.status != http.StatusOK || login.CredentialID == "" {
		b.T.Fatalf("%s's token login = %d %s, want 200 with a credential_id", service, got.status, got.text())
	}
	return Launcher{Key: key, CredentialID: login.CredentialID}
}

// Enroll has by's credential enroll the pod runtimeID, proving with its projected token that it
// runs as account.
func (b Pods) Enroll(by Launcher, runtimeID, account string) Pod {
	b.T.Helper()
	key := b.newKey()
	thumbprint, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		b.T.Fatalf("thumbprint %s's key: %v", runtimeID, err)
	}
	signed, err := proof.SignLauncher(by.Key, by.CredentialID, http.MethodPost, b.Base+"/v1/enrollments", time.Now())
	if err != nil {
		b.T.Fatalf("sign %s's enrollment: %v", runtimeID, err)
	}
	podToken := b.Token(account, runtimeID)
	got := b.send(http.MethodPost, "/v1/enrollments", map[string]string{"Proof": signed}, map[string]any{
		"kind": "pod", "runtime_id": runtimeID, "thumbprint": thumbprint, "pod_token": podToken,
	}, podToken)
	var enrolled struct {
		EnrollmentID string `json:"enrollment_id"`
	}
	b.decode(got, &enrolled)
	if got.status != http.StatusCreated || enrolled.EnrollmentID == "" {
		b.T.Fatalf("enroll %s as %s = %d %s, want 201 with an enrollment_id", runtimeID, account, got.status, got.text())
	}
	return Pod{RuntimeID: runtimeID, Key: key, EnrollmentID: enrolled.EnrollmentID}
}

// EnrollRefused is Enroll for an enrollment the broker is expected to refuse: it answers the
// status and the refusal's code, and fails only when the broker created the enrollment instead.
func (b Pods) EnrollRefused(by Launcher, runtimeID, account string) (int, string) {
	b.T.Helper()
	key := b.newKey()
	thumbprint, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		b.T.Fatalf("thumbprint %s's key: %v", runtimeID, err)
	}
	signed, err := proof.SignLauncher(by.Key, by.CredentialID, http.MethodPost, b.Base+"/v1/enrollments", time.Now())
	if err != nil {
		b.T.Fatalf("sign %s's enrollment: %v", runtimeID, err)
	}
	podToken := b.Token(account, runtimeID)
	got := b.send(http.MethodPost, "/v1/enrollments", map[string]string{"Proof": signed}, map[string]any{
		"kind": "pod", "runtime_id": runtimeID, "thumbprint": thumbprint, "pod_token": podToken,
	}, podToken)
	if got.status == http.StatusCreated {
		b.T.Fatalf("enroll %s as %s = 201 %s, want a refusal", runtimeID, account, got.text())
	}
	var refusal struct {
		Code string `json:"code"`
	}
	b.decode(got, &refusal)
	return got.status, refusal.Code
}

// Ask has p request the agent secret secretName, naming the reason a pod names, and returns the
// broker's reply.
func (b Pods) Ask(p Pod, secretName string) RequestReply {
	b.T.Helper()
	return b.AskWithReason(p, secretName, p.RuntimeID+" needs "+secretName)
}

// AskWithReason has p request the agent secret secretName for reason, which is "" for a request
// that names none, and returns the broker's reply.
func (b Pods) AskWithReason(p Pod, secretName, reason string) RequestReply {
	b.T.Helper()
	request, err := record.Sign(p.Key, b.Base, []record.AuthorizationDetail{
		{Type: record.KindAgentSecret, Identifier: secretName, Actions: []string{"inject"}},
	}, reason, "", nil, time.Now())
	if err != nil {
		b.T.Fatalf("sign %s's request for %s: %v", p.RuntimeID, secretName, err)
	}
	got := b.signed(p, http.MethodPost, "/v1/requests", map[string]any{"request": request, "session_id": nil}, request)
	if got.status != http.StatusOK {
		b.T.Fatalf("%s's POST /v1/requests for %s = %d %s, want 200", p.RuntimeID, secretName, got.status, got.text())
	}
	var reply RequestReply
	b.decode(got, &reply)
	return reply
}

// State reads request id's state as p, the pod that made it.
func (b Pods) State(p Pod, id string) RequestReply {
	b.T.Helper()
	path := "/v1/requests/" + id
	got := b.signed(p, http.MethodGet, path, nil)
	if got.status != http.StatusOK {
		b.T.Fatalf("%s's GET %s = %d %s, want 200", p.RuntimeID, path, got.status, got.text())
	}
	var reply RequestReply
	b.decode(got, &reply)
	return reply
}

// Cancel has p, the pod that made request id, withdraw it while it is pending.
func (b Pods) Cancel(p Pod, id string) {
	b.T.Helper()
	path := "/v1/requests/" + id + "/cancel"
	if got := b.signed(p, http.MethodPost, path, nil); got.status != http.StatusOK {
		b.T.Fatalf("%s's POST %s = %d %s, want 200", p.RuntimeID, path, got.status, got.text())
	}
}

// Values has p read the values of its grant grantID.
func (b Pods) Values(p Pod, grantID string) map[string]string {
	b.T.Helper()
	got := b.signed(p, http.MethodPost, "/v1/grants/"+grantID+"/values", nil)
	if got.status != http.StatusOK {
		b.T.Fatalf("%s's grant values = %d %s, want 200", p.RuntimeID, got.status, got.text())
	}
	var values struct {
		Values map[string]string `json:"values"`
	}
	b.decode(got, &values)
	return values.Values
}

// newKey is a fresh key for a launcher credential or a pod to prove itself with.
func (b Pods) newKey() *ecdsa.PrivateKey {
	b.T.Helper()
	key, err := proof.NewKey()
	if err != nil {
		b.T.Fatalf("generate key: %v", err)
	}
	return key
}

// signed sends body to path with p's proof of its enrollment; inBody is every credential body
// carries.
func (b Pods) signed(p Pod, method, path string, body any, inBody ...string) answered {
	b.T.Helper()
	proofHeader, err := proof.Sign(p.Key, p.EnrollmentID, method, b.Base+path, time.Now())
	if err != nil {
		b.T.Fatalf("sign %s's proof for %s %s: %v", p.RuntimeID, method, path, err)
	}
	return b.send(method, path, map[string]string{"Proof": proofHeader}, body, inBody...)
}

// send sends body as JSON to path with the auth headers, logs the exchange with neither the
// headers nor the body, since each can carry a credential, and with the answer named as text
// names it, and returns the answer. inBody is every credential body carries.
func (b Pods) send(method, path string, auth map[string]string, body any, inBody ...string) answered {
	b.T.Helper()
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			b.T.Fatalf("encode the body of %s %s: %v", method, path, err)
		}
	}
	request, err := http.NewRequest(method, b.Base+path, bytes.NewReader(payload))
	if err != nil {
		b.T.Fatalf("%s %s: %v", method, path, err)
	}
	request.Header.Set("Content-Type", "application/json")
	for name, value := range auth {
		request.Header.Set(name, value)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		b.T.Fatalf("%s %s: %v", method, path, err)
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(response.Body)
	if err != nil {
		b.T.Fatalf("read the answer to %s %s: %v", method, path, err)
	}
	got := answered{status: response.StatusCode, body: answer, carried: append(slices.Collect(maps.Values(auth)), inBody...)}
	b.T.Logf("> %s %s\n< %d %s", method, path, got.status, got.text())
	return got
}

// decode unmarshals the JSON answer into into, failing T if it is not, naming the answer as
// answered.text does.
func (b Pods) decode(got answered, into any) {
	b.T.Helper()
	if err := json.Unmarshal(got.body, into); err != nil {
		b.T.Fatalf("decode %s: %v", got.text(), err)
	}
}

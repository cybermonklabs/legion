// contract_test.go drives Client against the real broker (packages/envoy's own httptest server,
// mounted through its brokertest.NewRig) instead of client_test.go's fakeBroker: this is the
// byte-compatibility proof for jws.go and the token login, judged by the broker's own
// record.VerifyRequestObject, its Kubernetes token verifier against the rig's local OIDC issuer, and
// its proof.Verifier, not by a reimplementation of the wire contract by inspection. It skips without
// BROKER_TEST_DATABASE_URL, except on GitHub Actions, where ci.yaml's daemon-go job sets it and a
// skip would hide that it stopped (newBrokerRig).
//
// This is the only file in this package that imports anything under github.com/sjawhar/envoy/...:
// those imports are test-only, never leaking into the agent-secrets client's build. It imports
// packages/envoy/brokertest, a thin public facade over the internal rig,
// packages/envoy/brokerclient, the broker clients' own trade, and packages/envoy/testprereq, the
// prerequisite rule both modules' tests follow, because Go's internal-package rule refuses this
// module an import under github.com/sjawhar/envoy/internal/...
package agentsecrets

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/brokeraddr"
	"github.com/sjawhar/envoy/brokerclient"
	"github.com/sjawhar/envoy/brokertest"
	"github.com/sjawhar/envoy/testprereq"
)

// newBrokerRig is brokertest.NewRig behind the repository's prerequisite rule
// (testprereq.Require): without BROKER_TEST_DATABASE_URL it skips, and on GitHub Actions it fails
// naming ci.yaml's daemon-go job, which names its Postgres service there, so the contract tests
// cannot stop running unnoticed.
func newBrokerRig(t *testing.T) *brokertest.Rig {
	t.Helper()
	testprereq.Require(t, os.Getenv("BROKER_TEST_DATABASE_URL") != "", "daemon-go", "BROKER_TEST_DATABASE_URL must be set to run the broker contract tests")
	return brokertest.NewRig(t)
}

// daemonTokenFile writes a fresh ServiceAccount token for account, living lifetime, where the
// kubelet would project it, and returns its path.
func daemonTokenFile(t *testing.T, rig *brokertest.Rig, account string, lifetime time.Duration) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(rig.MintAccountToken(t, account, lifetime)), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// count answers one count(*) query against the rig's broker database.
func count(t *testing.T, rig *brokertest.Rig, query string) int {
	t.Helper()
	var n int
	if err := rig.Store.Pool.QueryRow(context.Background(), query).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

// enrollPod enrolls a fresh pod under c, answering its enrollment.
func enrollPod(t *testing.T, rig *brokertest.Rig, c *Client, podUID string) Enrollment {
	t.Helper()
	e, err := c.Enroll(context.Background(), PodEnrollment{PodUID: podUID, Thumbprint: "tp-" + podUID, PodToken: rig.MintPodToken(t, podUID)})
	if err != nil || e.ID == "" {
		t.Fatalf("Enroll %s = %+v, %v", podUID, e, err)
	}
	return e
}

// TestContractARestartedDaemonLogsInWithItsTokenAndAsksNoOne is a daemon's restart against the real
// broker: the first boot logs in with its ServiceAccount token and enrolls a pod; the restart (a
// new Client, as a new process, with the kubelet's next token) logs in again and enrolls a pod with
// no machine login opened at any point, so nothing waits for a person. The broker holds the
// account's one credential, which the restart's key now signs for: it ends the pod the first boot
// enrolled, and the first boot's key is refused.
func TestContractARestartedDaemonLogsInWithItsTokenAndAsksNoOne(t *testing.T) {
	rig := newBrokerRig(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	first := &Client{URL: rig.URL, TokenFile: daemonTokenFile(t, rig, brokertest.DaemonAccount, time.Hour)}
	if err := first.Start(ctx); err != nil {
		t.Fatalf("first boot's Start: %v", err)
	}
	earlier := enrollPod(t, rig, first, "pod-uid-1")
	cancel()

	ctx2, cancel2 := context.WithCancel(context.Background())
	t.Cleanup(cancel2)
	restarted := &Client{URL: rig.URL, TokenFile: daemonTokenFile(t, rig, brokertest.DaemonAccount, time.Hour)}
	if err := restarted.Start(ctx2); err != nil {
		t.Fatalf("restart's Start: %v", err)
	}
	if got := restarted.LoginStatus().State; got != "issued" {
		t.Fatalf("after the restart: LoginStatus = %q, want issued", got)
	}
	enrollPod(t, rig, restarted, "pod-uid-2")
	if err := restarted.Revoke(ctx2, earlier.ID); err != nil {
		t.Fatalf("the restart ends the pod the first boot enrolled: %v", err)
	}
	if _, err := first.Enroll(ctx2, PodEnrollment{PodUID: "pod-uid-3", Thumbprint: "tp-pod-uid-3", PodToken: rig.MintPodToken(t, "pod-uid-3")}); !isNoCredential(err, "") {
		t.Fatalf("the first boot's key after the restart = %v, want the broker's refusal", err)
	}
	if n := count(t, rig, `select count(*) from credential_requests`); n != 0 {
		t.Fatalf("the broker holds %d machine-login records, want none: no person approves a token login", n)
	}
	if n := count(t, rig, `select count(*) from launcher_credentials`); n != 1 {
		t.Fatalf("the broker holds %d launcher credentials, want the account's one", n)
	}
}

// TestContractARefusedCredentialLogsInAgain: once the broker refuses the daemon's credential (here
// SQL-expired, as a credential the daemon could not renew in time), the next enrollment answers
// NO_MACHINE_CREDENTIAL and the login loop logs in again with the token, after which a pod enrolls,
// with no restart and no person.
func TestContractARefusedCredentialLogsInAgain(t *testing.T) {
	rig := newBrokerRig(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	c := &Client{URL: rig.URL, TokenFile: daemonTokenFile(t, rig, brokertest.DaemonAccount, time.Hour)}
	if err := c.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := rig.Store.Pool.Exec(ctx, `update launcher_credentials set expires_at = now() - interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Enroll(ctx, PodEnrollment{PodUID: "pod-uid-1", Thumbprint: "tp-1", PodToken: rig.MintPodToken(t, "pod-uid-1")}); !isNoCredential(err, "") {
		t.Fatalf("Enroll with an expired credential = %v, want NO_MACHINE_CREDENTIAL", err)
	}
	eventually(t, func() bool {
		_, err := c.Enroll(ctx, PodEnrollment{PodUID: "pod-uid-2", Thumbprint: "tp-2", PodToken: rig.MintPodToken(t, "pod-uid-2")})
		return err == nil
	}, "the login loop to log in again and a pod to enroll")
	if n := count(t, rig, `select count(*) from credential_requests`); n != 0 {
		t.Fatalf("the broker holds %d machine-login records, want none", n)
	}
}

// TestContractARenewalKeepsTheCredentialAndItsPods: a renewal proves the key the credential is
// already bound to, so the real broker keeps the credential's id and key: a pod enrolled before the
// renewal is still the daemon's to end after it, a proof signed before the renewal still
// authenticates, and the broker holds one credential throughout.
func TestContractARenewalKeepsTheCredentialAndItsPods(t *testing.T) {
	rig := newBrokerRig(t)
	ctx := context.Background()
	c := &Client{URL: rig.URL, TokenFile: daemonTokenFile(t, rig, brokertest.DaemonAccount, time.Hour)}
	first, err := c.Login(ctx)
	if err != nil {
		t.Fatal(err)
	}
	before := c.snapshot().cred
	enrolled := enrollPod(t, rig, c, "pod-uid-1")
	time.Sleep(10 * time.Millisecond)
	// The kubelet's next token, which outlives the first, so the renewed credential expires later.
	c.TokenFile = daemonTokenFile(t, rig, brokertest.DaemonAccount, time.Hour+30*time.Second)
	renewed, err := c.Login(ctx)
	if err != nil {
		t.Fatal(err)
	}
	after := c.snapshot().cred
	if after.id != before.id || after.key != before.key || !renewed.After(first) {
		t.Fatalf("renewal moved the credential from %s to %s (same key %v), expiry %v to %v; want the same credential, later", before.id, after.id, after.key == before.key, first, renewed)
	}
	c.state.Store(&loginState{cred: before})
	if err := c.Revoke(ctx, enrolled.ID); err != nil {
		t.Fatalf("a proof under the credential as it was before the renewal: %v", err)
	}
	if n := count(t, rig, `select count(*) from launcher_credentials`); n != 1 {
		t.Fatalf("the broker holds %d launcher credentials, want the account's one", n)
	}
}

// TestContractOnlyTheListedAccountWithALiveTokenLogsIn: a token for an account the broker does not
// list is refused ACCOUNT_NOT_LISTED and an expired token TOKEN_EXPIRED, each a failed login and no
// credential; once BROKER_SERVICE_LOGINS drops the daemon's own account, its live credential stops
// authenticating and its next login is refused.
func TestContractOnlyTheListedAccountWithALiveTokenLogsIn(t *testing.T) {
	rig := newBrokerRig(t)
	ctx := context.Background()
	for _, c := range []struct {
		name, account string
		lifetime      time.Duration
		code          string
	}{
		{"an unlisted account", "system:serviceaccount:legion-system:someone-else", time.Hour, "ACCOUNT_NOT_LISTED"},
		{"an expired token", brokertest.DaemonAccount, -time.Minute, "TOKEN_EXPIRED"},
	} {
		client := &Client{URL: rig.URL, TokenFile: daemonTokenFile(t, rig, c.account, c.lifetime)}
		_, err := client.Login(ctx)
		var api *APIError
		if !errors.As(err, &api) || api.Code != c.code || client.LoginStatus().State != "failed" {
			t.Fatalf("%s: Login = %v, state %q; want %s and failed", c.name, err, client.LoginStatus().State, c.code)
		}
	}

	c := &Client{URL: rig.URL, TokenFile: daemonTokenFile(t, rig, brokertest.DaemonAccount, time.Hour)}
	if _, err := c.Login(ctx); err != nil {
		t.Fatal(err)
	}
	enrollPod(t, rig, c, "pod-uid-1")
	rig.DropServiceLogin(brokertest.DaemonAccount)
	if _, err := c.Enroll(ctx, PodEnrollment{PodUID: "pod-uid-2", Thumbprint: "tp-2", PodToken: rig.MintPodToken(t, "pod-uid-2")}); !isNoCredential(err, "") {
		t.Fatalf("Enroll once the account is unlisted = %v, want the broker's refusal", err)
	}
	_, err := c.Login(ctx)
	var api *APIError
	if !errors.As(err, &api) || api.Code != "ACCOUNT_NOT_LISTED" {
		t.Fatalf("Login once the account is unlisted = %v, want ACCOUNT_NOT_LISTED", err)
	}
}

// TestContractAPodEnrollmentStatesItsClaim: the real broker reads the claim the daemon sends under
// the name the daemon sends it, and files the pod under that claim of the account the daemon's
// own token login proved (claim:<login subject>/<claim>), so the claim's next pod reads the
// requests this one makes. A field the broker did not declare would be refused, retried without
// it, and leave the row with no owner.
func TestContractAPodEnrollmentStatesItsClaim(t *testing.T) {
	rig := newBrokerRig(t)
	ctx := context.Background()
	c := &Client{URL: rig.URL, TokenFile: daemonTokenFile(t, rig, brokertest.DaemonAccount, time.Hour)}
	if _, err := c.Login(ctx); err != nil {
		t.Fatal(err)
	}
	const claim = "legion-acme-acme-12-planner"
	enrolled, err := c.Enroll(ctx, PodEnrollment{PodUID: "pod-uid-1", Thumbprint: "tp-1", PodToken: rig.MintPodToken(t, "pod-uid-1"), Session: "planner-session-1", Claim: claim})
	if err != nil {
		t.Fatalf("Enroll with a claim: %v", err)
	}
	var owner, stated *string
	if err := rig.Store.Pool.QueryRow(ctx, `select owner, claim from enrollments where id=$1`, enrolled.ID).Scan(&owner, &stated); err != nil {
		t.Fatal(err)
	}
	if want := "claim:" + brokertest.DaemonAccount + "/" + claim; owner == nil || *owner != want || stated == nil || *stated != claim {
		t.Fatalf("the enrollment records owner %v and claim %v, want %s and %s", deref(owner), deref(stated), want, claim)
	}
}

// deref is *s, or "<null>" for nil.
func deref(s *string) string {
	if s == nil {
		return "<null>"
	}
	return *s
}

// TestContractARegistrationIsAuthenticatedByTheDaemonsOwnToken drives RegisterPodSession against
// the real broker's POST /v1/oauth/exchange/pod-sessions. Its class is serviceAuth
// (routes_table.go), which authenticates the bearer with the token login's own check
// (server.go's serviceCaller: MachineLogin.VerifyServiceToken(bearer(r))). With the daemon's own
// ServiceAccount token as that bearer, the write passes the route's authentication, its body
// decodes and its pod token verifies, and the broker's exchange policy decides it: the rig's pods
// run as an account its BROKER_EXCHANGE_ACCOUNTS does not list, so the answer is 403
// POD_ACCOUNT_NOT_LISTED, which no earlier step can give. The control is the same write under
// the token of an account BROKER_SERVICE_LOGINS does not list, which the route's authentication
// refuses before the body is read.
func TestContractARegistrationIsAuthenticatedByTheDaemonsOwnToken(t *testing.T) {
	rig := newBrokerRig(t)
	ctx := context.Background()
	podToken := rig.MintPodToken(t, "pod-uid-registered")
	for _, c := range []struct {
		name, account string
		status        int
		code          string
	}{
		{"the daemon's own token", brokertest.DaemonAccount, http.StatusForbidden, "POD_ACCOUNT_NOT_LISTED"},
		{"an unlisted account's token", "system:serviceaccount:legion-system:someone-else", http.StatusForbidden, "ACCOUNT_NOT_LISTED"},
	} {
		client := &Client{URL: rig.URL, TokenFile: daemonTokenFile(t, rig, c.account, time.Hour)}
		_, err := client.RegisterPodSession(ctx, "pod-uid-registered", podToken, "ses_contract", testThumbprint, 1)
		var api *APIError
		if !errors.As(err, &api) || api.Status != c.status || api.Code != c.code {
			t.Fatalf("%s: RegisterPodSession = %v, want %d %s", c.name, err, c.status, c.code)
		}
	}
}

// rolesSubject is one issue pod's exchange token as a role's broker client presents it: under the
// broker's Kubernetes subject token type (oauthwire.SubjectTokenTypeKubernetes, which this module
// cannot import).
type rolesSubject struct{ token string }

func (s rolesSubject) Token(context.Context) (string, string, error) {
	return s.token, "urn:envoy:params:oauth:token-type:kubernetes-service-account", nil
}

func (rolesSubject) Describe() string { return "the issue pod's exchange token" }

// TestContractASiblingRoleIsRefusedAnotherRolesSession: two roles of one issue pod read the same
// exchange token, and the daemon registers each role's session for that pod bound to that role's
// own key. Against the real broker (the exchange's registeredPodSession and the pod-session
// route), a trade holding the pod's token is refused for role B's session when its DPoP proof is
// role A's key — invalid_grant, naming the session — and admitted for B's session with B's key
// and for A's with A's.
func TestContractASiblingRoleIsRefusedAnotherRolesSession(t *testing.T) {
	rig := newBrokerRig(t)
	ctx := context.Background()
	const (
		pod      = "pod-uid-siblings"
		sessionA = "ses-role-a"
		sessionB = "ses-role-b"
	)
	keyA, keyB := mustTestKey(t), mustTestKey(t)
	daemon := &Client{URL: rig.URL, TokenFile: daemonTokenFile(t, rig, brokertest.DaemonAccount, time.Hour)}
	podToken := rig.MintAccountPodToken(t, brokertest.ExchangeRolesAccount, pod, time.Hour)
	for session, key := range map[string]*ecdsa.PrivateKey{sessionA: keyA, sessionB: keyB} {
		if _, err := daemon.RegisterPodSession(ctx, pod, podToken, session, keyThumbprint(t, key), 1); err != nil {
			t.Fatalf("the daemon's registration of %s for %s: %v", session, pod, err)
		}
	}

	exchangeToken := rig.MintSubjectToken(t, brokertest.ExchangeRolesAccount, pod)
	trade := func(key *ecdsa.PrivateKey, session string) error {
		t.Helper()
		return tradeAs(t, rig, exchangeToken, key, session)
	}
	err := trade(keyA, sessionB)
	var refused *brokerclient.RefusedError
	if !errors.As(err, &refused) || refused.Code != "invalid_grant" || !strings.Contains(refused.Description, sessionB) {
		t.Fatalf("role A, holding the pod's exchange token and its own key, traded for role B's session %s: %v; want it refused invalid_grant naming %s", sessionB, err, sessionB)
	}
	for _, c := range []struct {
		key     *ecdsa.PrivateKey
		session string
	}{{keyB, sessionB}, {keyA, sessionA}} {
		if err := trade(c.key, c.session); err != nil {
			t.Errorf("a trade for %s proved with that role's own key: %v, want it admitted", c.session, err)
		}
	}
}

// keyThumbprint is key's RFC 7638 thumbprint, as a role's shim hello names it.
func keyThumbprint(t *testing.T, key *ecdsa.PrivateKey) string {
	t.Helper()
	tp, err := thumbprint(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return tp
}

// tradeAs is one role's trade at the rig's broker: the pod's exchange token, proved with key, for
// session.
func tradeAs(t *testing.T, rig *brokertest.Rig, exchangeToken string, key *ecdsa.PrivateKey, session string) error {
	t.Helper()
	client, err := brokerclient.New(brokerclient.Config{BrokerURL: brokeraddr.New(rig.URL), Key: key, Subject: rolesSubject{token: exchangeToken}, Session: session})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Token(context.Background())
	return err
}

// TestContractALateOldIncarnationWriteKeepsTheNewKey: a role relaunched in its pod registers its
// session at the new process's launch generation with the new process's key, and a write of the
// earlier process that the broker commits after it — the late renewal a stop's bound gave up on —
// carries the earlier, lower generation. Against the real broker, that late write changes nothing:
// it is answered with the registration the broker holds, the later generation and key, which the
// daemon's client refuses as superseded; a trade proved with the new key is admitted, and one
// proved with the old key is refused, naming the session.
func TestContractALateOldIncarnationWriteKeepsTheNewKey(t *testing.T) {
	rig := newBrokerRig(t)
	ctx := context.Background()
	const (
		pod     = "pod-uid-relaunched"
		session = "ses-relaunched"
	)
	oldKey, newKey := mustTestKey(t), mustTestKey(t)
	daemon := &Client{URL: rig.URL, TokenFile: daemonTokenFile(t, rig, brokertest.DaemonAccount, time.Hour)}
	podToken := rig.MintAccountPodToken(t, brokertest.ExchangeRolesAccount, pod, time.Hour)

	if _, err := daemon.RegisterPodSession(ctx, pod, podToken, session, keyThumbprint(t, newKey), 2); err != nil {
		t.Fatalf("the new process's registration of %s at generation 2: %v", session, err)
	}
	_, err := daemon.RegisterPodSession(ctx, pod, podToken, session, keyThumbprint(t, oldKey), 1)
	var superseded *SupersededError
	if !errors.As(err, &superseded) || superseded.Sent != 1 || superseded.Answered != 2 {
		t.Fatalf("the earlier process's late write of %s at generation 1 = %v, want it answered as superseded by generation 2", session, err)
	}

	exchangeToken := rig.MintSubjectToken(t, brokertest.ExchangeRolesAccount, pod)
	if err := tradeAs(t, rig, exchangeToken, newKey, session); err != nil {
		t.Errorf("a trade for %s proved with the new process's key: %v, want it admitted: the late write put the old key back", session, err)
	}
	err = tradeAs(t, rig, exchangeToken, oldKey, session)
	var refused *brokerclient.RefusedError
	if !errors.As(err, &refused) || refused.Code != "invalid_grant" || !strings.Contains(refused.Description, session) {
		t.Errorf("a trade for %s proved with the earlier process's key: %v, want it refused invalid_grant naming %s", session, err, session)
	}
}

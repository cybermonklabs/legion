// restricted_member_test.go drives a restricted member's machine login through the real broker
// binary: the person an access list admitted from outside the sign-in domain, whom Dispatch calls
// a restricted member, logs a machine in and approves that login, and the broker enrolls no
// session under it and releases no secret to any session that acts for that person.
package main

import (
	"crypto/ecdsa"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/ciphertest"
	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess/projectaccesstest"
	"github.com/sjawhar/envoy/internal/cipher/proof"
	"github.com/sjawhar/envoy/internal/cipher/store/storetest"
	"github.com/sjawhar/envoy/internal/cipherclient"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
	"github.com/sjawhar/envoy/internal/testwait"
)

const (
	// outsideMember is a person an access list admitted from outside the sign-in domain, whom the
	// Dispatch stand-in calls a restricted member.
	outsideMember = "outsider@outside.example"
	// insideMember is a full member, the control: what that person's machine login got before
	// this change it gets still.
	insideMember = "ines@example.com"
	// unaskedMember is a full member the stand-in is first asked about while it is down, so no
	// answer the broker kept stands in for the one it could not get.
	unaskedMember = "uma@example.com"
	// restrictedMemberDenial is how a denied request's detail names the rule that denied a
	// secret to a restricted member's session (policy.ReasonRestrictedMember), spelled here as
	// the session reads it rather than through the constant, so a change of the wire is a
	// failure of this test and not a change it follows.
	restrictedMemberDenial = "restricted-member"
	// uncheckableCode and uncheckableWords are how the broker refuses, 503, what it would do for
	// a person while it cannot learn from Dispatch whether that person is a restricted member
	// (projectaccess.UncheckableCode and MemberKindUncheckable), spelled as the caller reads them;
	// uncheckableLine is the line it logs each time.
	uncheckableCode  = "MEMBER_KIND_UNCHECKABLE"
	uncheckableWords = "whether Dispatch calls " + unaskedMember + " a restricted member cannot be read: the broker could not ask Dispatch"
	uncheckableLine  = "broker: whether Dispatch calls a person a restricted member cannot be read, so "
	// restrictedPublicURL is the one BROKER_PUBLIC_URL both brokers of the test carry, a name
	// under the reserved .test domain that resolves nowhere: every proof and request object
	// names it, so a session enrolled through one broker is served by the other
	// (ciphertest.NewClient).
	restrictedPublicURL = "http://broker-restricted.test"
)

// TestARestrictedMembersMachineLoginEnrollsNothing drives two broker processes on one database
// and one BROKER_PUBLIC_URL: one with no project-access check, as every broker ran before this
// change and as one runs while BROKER_PROJECT_ACCESS_URL is unset, and one that asks the Dispatch
// stand-in what kind of member each person is. It proves, through the binary's own routes:
//
//   - a full member's machine login enrolls a session that is given and released the shared
//     agent-tier secret at once, as it was before (the control);
//   - a restricted member approves their own machine login, and the broker that asks refuses
//     every enrollment under it 403 RESTRICTED_MEMBER_ENROLLS_NOTHING;
//   - a restricted member's session enrolled on the broker that did not ask, with the shared
//     secret granted there, is released nothing by the broker that asks, and its next ask for the
//     secret is denied restricted-member, while a full member's session enrolled the same way is
//     released its grant;
//   - while Dispatch cannot be asked, the broker that asks refuses an enrollment under a person's
//     machine login, a session's ask and the release of a grant it already holds, each 503
//     MEMBER_KIND_UNCHECKABLE with nothing written and nothing released; once Dispatch answers,
//     the same release and the same enrollment go through (the control).
func TestARestrictedMembersMachineLoginEnrollsNothing(t *testing.T) {
	databaseURL := storetest.DatabaseURL(t)
	cluster := oidctest.New(t)
	uiToken := accountToken(t, cluster, cluster.PublishKey(t, "cluster-key"), apiAccount)
	dispatch := projectaccesstest.NewServer(t, nil, map[string]string{
		outsideMember: projectaccess.MemberRestricted,
		insideMember:  projectaccess.MemberFull,
		unaskedMember: projectaccess.MemberFull,
	})
	env := restrictedMemberEnv(t, databaseURL, cluster)
	binary := buildBroker(t)
	unchecked := startBroker(t, brokerOptions{Binary: binary, Env: env})
	unchecked.AwaitRecord(t, "project access check", map[string]any{"state": "off"})
	checked := startBroker(t, brokerOptions{Binary: binary, Env: append(slices.Clone(env), projectAccessSettings(t, dispatch)...)})
	checked.AwaitRecord(t, "project access check", map[string]any{"state": "on"})
	before := restrictedMemberClient(t, unchecked.Addr, uiToken)
	after := restrictedMemberClient(t, checked.Addr, uiToken)

	t.Run("a full member's machine login enrolls and is released the shared agent-tier secret", func(t *testing.T) {
		key, credential := machineLogin(t, after, insideMember, "ines-devbox")
		enrollment, sessionKey := after.EnrollSession(t, key, credential, "ines-devbox-1")
		grant := after.RequestAgentSecret(t, sessionKey, enrollment, "the control", sharedSecret)
		if got := after.GrantValues(t, sessionKey, enrollment, grant)[sharedSecret]; got != sharedSecretValue {
			t.Fatalf("%s's session was released %s = %q, want %q", insideMember, sharedSecret, got, sharedSecretValue)
		}
	})

	t.Run("a restricted member's machine login enrolls nothing", func(t *testing.T) {
		key, credential := machineLogin(t, after, outsideMember, "outsider-laptop")
		sessionKey := ciphertest.NewSigningKey(t)
		status, body := enrollSessionKey(t, after, key, credential, "outsider-laptop-1", sessionKey)
		if code, _ := brokerRefusal(body); status == http.StatusForbidden && code == "RESTRICTED_MEMBER_ENROLLS_NOTHING" {
			return
		}
		// What the enrollment the broker should have refused went on to reach, so the failure
		// names the secret it released rather than only the status.
		var enrolled struct {
			EnrollmentID string `json:"enrollment_id"`
		}
		if status == http.StatusCreated && json.Unmarshal(body, &enrolled) == nil && enrolled.EnrollmentID != "" {
			answer := after.AskForAgentSecret(t, sessionKey, enrolled.EnrollmentID, "an outside member's agent", sharedSecret)
			if answer.State == "granted" && answer.GrantID != nil {
				values := after.GrantValues(t, sessionKey, enrolled.EnrollmentID, *answer.GrantID)
				t.Fatalf("%s's machine login enrolled a session (%d %s) and it was released %s = %q without anyone's approval; want 403 RESTRICTED_MEMBER_ENROLLS_NOTHING",
					outsideMember, status, strings.TrimSpace(string(body)), sharedSecret, values[sharedSecret])
			}
		}
		t.Fatalf("%s's enrollment = %d %s, want 403 RESTRICTED_MEMBER_ENROLLS_NOTHING", outsideMember, status, strings.TrimSpace(string(body)))
	})

	t.Run("a session enrolled before the broker asked is released nothing", func(t *testing.T) {
		key, credential := machineLogin(t, before, outsideMember, "outsider-desktop")
		enrollment, sessionKey := before.EnrollSession(t, key, credential, "outsider-desktop-1")
		grant := before.RequestAgentSecret(t, sessionKey, enrollment, "an outside member's agent", sharedSecret)

		status, body := after.Session(t, sessionKey, enrollment, http.MethodPost, "/v1/grants/"+grant+"/values", nil)
		if code, _ := brokerRefusal(body); status != http.StatusForbidden || code != "GRANT_NOT_LIVE" {
			t.Fatalf("the release of %s's grant %s = %d %s, want 403 GRANT_NOT_LIVE", outsideMember, grant, status, strings.TrimSpace(string(body)))
		}
		answer := after.AskForAgentSecret(t, sessionKey, enrollment, "an outside member's agent, again", sharedSecret)
		if detail := requestDetail(t, answer); answer.Status != http.StatusOK || answer.State != "denied" || answer.GrantID != nil ||
			!strings.Contains(detail, sharedSecret+": "+restrictedMemberDenial) {
			t.Fatalf("%s's session's next ask for %s = %d %s, want 200 denied naming %s: %s", outsideMember, sharedSecret, answer.Status, answer.Body, sharedSecret, restrictedMemberDenial)
		}

		// The control: a full member's session enrolled the same way is released its grant.
		key, credential = machineLogin(t, before, insideMember, "ines-laptop")
		enrollment, sessionKey = before.EnrollSession(t, key, credential, "ines-laptop-1")
		grant = before.RequestAgentSecret(t, sessionKey, enrollment, "the control", sharedSecret)
		if got := after.GrantValues(t, sessionKey, enrollment, grant)[sharedSecret]; got != sharedSecretValue {
			t.Fatalf("%s's session was released %s = %q by the broker that asks, want %q", insideMember, sharedSecret, got, sharedSecretValue)
		}
	})

	t.Run("while Dispatch cannot be asked nothing is enrolled, decided or released", func(t *testing.T) {
		// The session and its grant come from the broker that does not ask, so the broker that
		// does has kept no answer about this person when Dispatch goes down.
		key, credential := machineLogin(t, before, unaskedMember, "uma-devbox")
		enrollment, sessionKey := before.EnrollSession(t, key, credential, "uma-devbox-1")
		grant := before.RequestAgentSecret(t, sessionKey, enrollment, "before Dispatch goes down", sharedSecret)
		dispatch.SetDown(true)
		t.Cleanup(func() { dispatch.SetDown(false) })
		uncheckable := func(what string, status int, body []byte) {
			t.Helper()
			if code, words := brokerRefusal(body); status != http.StatusServiceUnavailable || code != uncheckableCode || words != uncheckableWords {
				t.Fatalf("%s while Dispatch is down = %d %s, want 503 %s %q", what, status, strings.TrimSpace(string(body)), uncheckableCode, uncheckableWords)
			}
		}

		status, body := enrollSessionKey(t, after, key, credential, "uma-devbox-2", ciphertest.NewSigningKey(t))
		uncheckable(unaskedMember+"'s enrollment", status, body)
		asked := after.AskForAgentSecret(t, sessionKey, enrollment, "while Dispatch is down", sharedSecret)
		uncheckable(unaskedMember+"'s ask for "+sharedSecret, asked.Status, []byte(asked.Body))
		status, body = after.Session(t, sessionKey, enrollment, http.MethodPost, "/v1/grants/"+grant+"/values", nil)
		uncheckable("the release of "+unaskedMember+"'s grant", status, body)
		if strings.Contains(string(body), sharedSecretValue) {
			t.Fatalf("the refused release carried the secret's value: %s", body)
		}
		for _, what := range []string{"the enrollment", "the secret request", "the release"} {
			checked.AwaitRecord(t, uncheckableLine+what+" is refused", map[string]any{"person": unaskedMember})
		}

		// The control: once Dispatch answers, the same release and the same enrollment go
		// through, so each refusal above was the outage's and nothing else's. The broker keeps a
		// person's failed read 2 s before it asks Dispatch again, so the release is asked until
		// it is no longer refused 503.
		dispatch.SetDown(false)
		testwait.UntilPolling(t, 250*time.Millisecond, "the release of "+unaskedMember+"'s grant to stop being refused 503 once Dispatch answers", func() (int, bool) {
			status, _ := after.Session(t, sessionKey, enrollment, http.MethodPost, "/v1/grants/"+grant+"/values", nil)
			return status, status != http.StatusServiceUnavailable
		})
		if got := after.GrantValues(t, sessionKey, enrollment, grant)[sharedSecret]; got != sharedSecretValue {
			t.Fatalf("%s's session was released %s = %q once Dispatch answered, want %q", unaskedMember, sharedSecret, got, sharedSecretValue)
		}
		if status, body := enrollSessionKey(t, after, key, credential, "uma-devbox-2", ciphertest.NewSigningKey(t)); status != http.StatusCreated {
			t.Fatalf("%s's enrollment once Dispatch answered = %d %s, want 201", unaskedMember, status, strings.TrimSpace(string(body)))
		}
	})
}

// restrictedMemberEnv is what both brokers of the test run with: the one database, the one public
// URL, a store holding the shared agent-tier secret alone, the device grant, and cluster as the
// issuer of the account the routes Dispatch's server calls take (BROKER_API_ACCOUNTS).
func restrictedMemberEnv(t *testing.T, databaseURL string, cluster *oidctest.Issuer) []string {
	t.Helper()
	secretsFile := filepath.Join(t.TempDir(), "memory-secrets.json")
	content := `{"secrets": [` + fakeSecret(testSecretsPrefix+"shared-secret", policy.OwnerShared, sharedSecretValue) + `]}`
	if err := os.WriteFile(secretsFile, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return []string{
		"BROKER_MACHINE_LOGIN_URL=" + testMachineLoginURL,
		"BROKER_DATABASE_URL=" + databaseURL,
		"BROKER_LISTEN_ADDR=127.0.0.1:0",
		"BROKER_PUBLIC_URL=" + restrictedPublicURL,
		"BROKER_SECRETS_STORE=memory",
		"BROKER_MEMORY_SECRETS_FILE=" + secretsFile,
		"BROKER_K8S_OIDC_ISSUER=" + cluster.URL(),
		"BROKER_K8S_OIDC_AUDIENCE=" + enrolmentAudience,
		"BROKER_API_ACCOUNTS=dispatch=" + apiAccount,
		"BROKER_DRAIN_SECONDS=0",
	}
}

// restrictedMemberClient drives the broker at addr as restrictedPublicURL, presenting uiToken on
// the routes Dispatch's server calls and signing its OAuth steps' DPoP proofs as the helper does.
func restrictedMemberClient(t *testing.T, addr, uiToken string) *ciphertest.Client {
	t.Helper()
	client := ciphertest.NewClient(t, restrictedPublicURL, addr)
	client.UIToken = uiToken
	client.DPoP = cipherclient.NewDPoPTransport
	return client
}

// projectAccessSettings points a broker's project-access check at server: its URL, and a file
// holding the one bearer it takes, as a projected token is mounted.
func projectAccessSettings(t *testing.T, server *projectaccesstest.Server) []string {
	t.Helper()
	tokenFile := filepath.Join(t.TempDir(), "dispatch-access-token")
	if err := os.WriteFile(tokenFile, []byte(server.Token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return []string{"BROKER_PROJECT_ACCESS_URL=" + server.URL, "BROKER_PROJECT_ACCESS_TOKEN_FILE=" + tokenFile}
}

// machineLogin is person's machine login on host through client, approved by that person as
// Dispatch relays an approval: the machine's key and the launcher credential the approval minted.
func machineLogin(t *testing.T, client *ciphertest.Client, person, host string) (*ecdsa.PrivateKey, string) {
	t.Helper()
	key := ciphertest.NewSigningKey(t)
	login := client.DeviceLogin(t, key, person, host)
	return key, client.ApproveMachineLogin(t, person, login.UserCode)
}

// enrollSessionKey enrolls sessionKey as a session of runtimeID under the launcher credential the
// machine key holds, answering the broker's status and body, so a row reads a refusal as well as
// an enrollment.
func enrollSessionKey(t *testing.T, client *ciphertest.Client, machineKey *ecdsa.PrivateKey, credential, runtimeID string, sessionKey *ecdsa.PrivateKey) (int, []byte) {
	t.Helper()
	thumbprint, err := proof.Thumbprint(&sessionKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return client.Launcher(t, machineKey, credential, http.MethodPost, "/v1/enrollments",
		map[string]any{"kind": "session", "runtime_id": runtimeID, "thumbprint": thumbprint})
}

// requestDetail is the detail POST /v1/requests answered, which names each denied secret and the
// rule that denied it; "" for an answer that carries none.
func requestDetail(t *testing.T, answer ciphertest.AgentSecretAnswer) string {
	t.Helper()
	var created struct {
		Detail *string `json:"detail"`
	}
	if err := json.Unmarshal([]byte(answer.Body), &created); err != nil || created.Detail == nil {
		return ""
	}
	return *created.Detail
}

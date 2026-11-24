// private_secret_test.go is dispatch://INFRA-3862's R11 through the two real binaries: the broker
// this package builds, with its project-access client pointed at a Dispatch stand-in, and the
// agent-secrets client, asking for a private project's secret as the sessions and the pod that
// name the project and the session that does not.
package main

import (
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/ciphertest"
	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess/projectaccesstest"
	"github.com/sjawhar/envoy/internal/cipher/proof"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/aws/awstest"
	"github.com/sjawhar/envoy/internal/cipher/store/storetest"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

const (
	// privSecret is PRIV's deploy key: shared and agent tier, so owner and tier alone would give
	// it to every session at once, and only its dispatch-project tag keeps it to PRIV's.
	privSecret = "PRIV_DEPLOY_KEY"
	privValue  = "priv-deploy-value"
	// plainSecret is the same secret with no tag, the control: every session gets it as today.
	plainSecret = "PLAIN_SHARED_KEY"
	plainValue  = "plain-shared-value"
	// lateSecret is a secret of LATE, a public project the run asks Dispatch about only while
	// Dispatch is down.
	lateSecret  = "LATE_KEY"
	lateProject = "LATE"

	// The pod of PRIV's service: the service it is registered as, the account its pods run as
	// (which BROKER_EXCHANGE_ACCOUNTS maps to PRIV for its owner), and the account its daemon
	// logs in as to enrol them.
	privService      = "priv-daemon"
	privPodAccount   = "system:serviceaccount:priv:priv-worker"
	privLoginAccount = "system:serviceaccount:priv:priv-daemon"
)

// TestThePrivateSecretGoesOnlyToTheSessionThatNamesIt: PRIV's owner enrols two sessions on her
// own machine, one naming PRIV and one naming no project, and a pod of PRIV's service enrols under
// the exchange entry that maps it to PRIV. The session naming PRIV and the pod read PRIV's secret;
// the session naming none is denied project-not-named, told how to name the project, and its
// command never runs; every one of them reads the untagged secret as before.
func TestThePrivateSecretGoesOnlyToTheSessionThatNamesIt(t *testing.T) {
	databaseURL := storetest.URL(t)
	issuer := oidctest.New(t)
	clusterKey := issuer.PublishKey(t, "cluster-key")
	ciphertest.SignIn(issuer, issuer.PublishKey(t, "signin-key"))
	dispatch := projectaccesstest.NewServer(t, map[string]projectaccesstest.Row{
		privateProject: {Visibility: "private", Owner: privateOwner},
		lateProject:    {Visibility: "public", Owner: privateOwner},
	}, map[string]string{privateOwner: projectaccess.MemberFull})
	tokenFile := filepath.Join(t.TempDir(), "dispatch-access-token")
	if err := os.WriteFile(tokenFile, []byte(dispatch.Token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := &requesterStore{path: filepath.Join(t.TempDir(), "fake-secrets.json"), rows: []awstest.LocalSecret{
		requesterRow(privSecret, policy.OwnerShared, policy.TierAgent, privValue, map[string]string{policy.TagProject: privateProject}),
		requesterRow(plainSecret, policy.OwnerShared, policy.TierAgent, plainValue, nil),
		requesterRow(lateSecret, policy.OwnerShared, policy.TierAgent, "late-value", map[string]string{policy.TagProject: lateProject}),
	}}
	store.write(t)
	cli := buildAgentSecrets(t)

	broker := startBroker(t, brokerOptions{Env: []string{
		"BROKER_DATABASE_URL=" + databaseURL,
		"BROKER_LISTEN_ADDR=127.0.0.1:0",
		"BROKER_PUBLIC_URL=http://127.0.0.1:0",
		"BROKER_FAKE_SECRETS_FILE=" + store.path,
		"BROKER_MACHINE_LOGIN_URL=" + testMachineLoginURL,
		"BROKER_K8S_OIDC_ISSUER=" + issuer.URL(),
		"BROKER_K8S_OIDC_AUDIENCE=" + enrolmentAudience,
		"BROKER_EXCHANGE_K8S_AUDIENCE=" + exchangeAudience,
		"BROKER_EXCHANGE_ACCOUNTS=" + privPodAccount + "=" + privateProject + "=" + privateOwner,
		"BROKER_SIGNIN_ISSUER=" + issuer.URL(),
		"BROKER_SIGNIN_CLIENT_ID=" + ciphertest.SignInClientID,
		"BROKER_SIGNIN_CLIENT_SECRET_FILE=" + signInClientSecretFile(t),
		"BROKER_SIGNIN_DOMAIN=" + ciphertest.SignInDomain,
		"BROKER_SERVICES=" + privService + "=" + privPodAccount,
		"BROKER_SERVICE_LOGINS=" + privService + "=" + privLoginAccount,
		"BROKER_API_ACCOUNTS=" + dispatchCaller + "=" + apiAccount,
		"BROKER_PROJECT_ACCESS_URL=" + dispatch.URL,
		"BROKER_PROJECT_ACCESS_TOKEN_FILE=" + tokenFile,
		"BROKER_DRAIN_SECONDS=0",
	}})
	base := "http://" + broker.Addr
	requireHealthy(t, broker.Addr)
	broker.AwaitRecord(t, "project access check", map[string]any{"state": "on"})
	if strings.Contains(broker.Boot, "agent secret policy refused") {
		t.Fatalf("the broker refused one of this run's secrets at boot; a broker with project access serves a tagged secret:\n%s", broker.Boot)
	}

	carol := machineLoginFor(t, newPlainBroker(broker.Addr), base, privateOwner, "carol-devbox", accountToken(t, issuer, clusterKey, apiAccount))
	named := enrolProjectSession(t, cli, base, carol, privateOwner, "carol-devbox-priv", "carol-priv-1", privateProject)
	unnamed := enrolProjectSession(t, cli, base, carol, privateOwner, "carol-devbox-plain", "carol-plain-1")
	pods := podBroker{t: t, base: base, issuer: issuer, key: clusterKey, audience: enrolmentAudience}
	pod := enrolCLIPod(t, cli, pods, pods.logIn(privService, privLoginAccount), privService, privPodAccount, "priv-pod-1")

	reads := func(t *testing.T, s requesterSession, secret, want string) {
		t.Helper()
		code, stdout, stderr := s.run(t, cli, base, secret, "--", "sh", "-c", `printf %s "$`+secret+`"`)
		if code != 0 || stdout != want {
			t.Fatalf("agent-secrets %s as %s (%s) exited %d with %q, want 0 and its value\nstderr: %s", secret, s.actor(), s.name, code, stdout, stderr)
		}
	}
	t.Run("the session naming PRIV reads PRIV's secret", func(t *testing.T) { reads(t, named, privSecret, privValue) })
	t.Run("the pod its exchange entry maps to PRIV reads PRIV's secret", func(t *testing.T) { reads(t, pod, privSecret, privValue) })
	t.Run("the session naming no project is denied PRIV's secret and told how to name it", func(t *testing.T) {
		code, stdout, stderr := unnamed.run(t, cli, base, privSecret, "--", "sh", "-c", "echo ran")
		if code != cliExitDenied || strings.Contains(stdout, "ran") {
			t.Fatalf("agent-secrets %s as %s's session naming no project exited %d with stdout %q, want %d (denied) and nothing run\nstderr: %s",
				privSecret, privateOwner, code, stdout, cliExitDenied, stderr)
		}
		for _, want := range []string{privSecret + ": " + policy.ReasonProjectNotNamed, "AGENT_SECRETS_PROJECTS"} {
			if !strings.Contains(stderr, want) {
				t.Fatalf("the client's denial does not carry %q:\n%s", want, stderr)
			}
		}
		if strings.Contains(stderr, privateProject+" ") || strings.Contains(stderr, "="+privateProject) {
			t.Fatalf("the denial names the project %s; it names none, since the asker may be one who may not see it:\n%s", privateProject, stderr)
		}
	})
	for _, s := range []requesterSession{named, unnamed, pod} {
		t.Run("the untagged secret goes to "+s.name+" as today", func(t *testing.T) { reads(t, s, plainSecret, plainValue) })
	}
	if dispatch.Calls() == 0 {
		t.Fatal("the broker asked Dispatch nothing, so no decision above read the project: the run is blind")
	}

	t.Run("while Dispatch cannot answer for a project a tagged secret is refused 503 and an untagged one is served", func(t *testing.T) {
		// The project route alone goes down: the person route goes on answering, so carol's
		// member kind is read as ever, and no answer the broker's client cached under
		// projectaccess.CacheTTL decides this row.
		dispatch.SetProjectsDown(true)
		defer dispatch.SetProjectsDown(false)
		// LATE_KEY's project was never asked about, so no answer the broker's client still holds
		// (projectaccess.CacheTTL) stands in for Dispatch.
		code, stdout, stderr := named.run(t, cli, base, lateSecret, "--", "sh", "-c", "echo ran")
		if code == 0 || strings.Contains(stdout, "ran") || !strings.Contains(stderr, "PROJECT_ACCESS_UNAVAILABLE") {
			t.Fatalf("agent-secrets %s while Dispatch is down exited %d with stdout %q, want a failure naming PROJECT_ACCESS_UNAVAILABLE and nothing run\nstderr: %s",
				lateSecret, code, stdout, stderr)
		}
		reads(t, named, plainSecret, plainValue)
	})
}

// enrolProjectSession is enrolCLISession for a session naming projects: the real client's keygen
// writes the key, by's launcher credential enrols its thumbprint as person's session naming
// projects, and the enrollment id is written beside the key.
func enrolProjectSession(t *testing.T, cli, base string, by launcher, person, runtimeID, sessionName string, projects ...string) requesterSession {
	t.Helper()
	keyDir := filepath.Join(t.TempDir(), runtimeID)
	keygen := runKeygen(t, cli, base, keyDir)
	b := podBroker{t: t, base: base}
	signed, err := proof.SignLauncher(by.key, by.credentialID, http.MethodPost, base+"/v1/enrollments", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if projects == nil {
		projects = []string{}
	}
	status, answer := b.post("/v1/enrollments", map[string]string{"Proof": signed}, map[string]any{
		"kind": "session", "runtime_id": runtimeID, "operator": person,
		"thumbprint": keygen, "session_id": sessionName, "projects": projects,
	})
	var enrolled struct {
		EnrollmentID string `json:"enrollment_id"`
	}
	b.decode(answer, &enrolled)
	if status != http.StatusCreated || enrolled.EnrollmentID == "" {
		t.Fatalf("enrol %s's session %s naming %v = %d %s, want 201 with an enrollment_id", person, sessionName, projects, status, answer)
	}
	writeWhole(t, filepath.Join(keyDir, "enrollment"), enrolled.EnrollmentID+"\n")
	return requesterSession{keyDir: keyDir, enrollmentID: enrolled.EnrollmentID, name: sessionName, runtimeID: runtimeID, person: person}
}

// runKeygen runs the real client's keygen into keyDir and answers the thumbprint it printed.
func runKeygen(t *testing.T, cli, base, keyDir string) string {
	t.Helper()
	cmd := exec.Command(cli, "keygen", "--out", keyDir)
	cmd.Env = agentSecretsEnv(base, keyDir)
	thumbprint, err := cmd.Output()
	if err != nil {
		t.Fatalf("agent-secrets keygen --out %s: %v", keyDir, err)
	}
	return strings.TrimSpace(string(thumbprint))
}

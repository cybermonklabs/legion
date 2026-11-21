// person_pod_test.go drives a pod whose Kubernetes account BROKER_POD_OPERATORS maps to a person
// through the compiled broker and the compiled agent-secrets. Such a pod asks for secrets as its
// person asks from the person's dev box, so the person's own agent-tier secret is granted to it
// with no approval. A pod of an account the setting does not map gets that secret only when the
// person approves it. It also pins that the login service of a mapped account is no service at
// the policy: a secret that names that service as its owner is refused at load.
package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/ciphertest"
	"github.com/sjawhar/envoy/internal/cipher/config"
	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess/projectaccesstest"
	"github.com/sjawhar/envoy/internal/cipher/secrets"
	"github.com/sjawhar/envoy/internal/cipher/store/storetest"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

const (
	// personPodPerson is the person the deployment maps ada's account to. The address is at the
	// Workspace domain the broker signs people in from (ciphertest.SignInDomain).
	personPodPerson = "ada@example.com"
	// personPodAdaAccount is the Kubernetes ServiceAccount that BROKER_POD_OPERATORS maps to
	// personPodPerson. personPodAdaService is the login service it logs in under
	// (BROKER_SERVICE_LOGINS). No BROKER_SERVICES entry names that service.
	personPodAdaAccount = "system:serviceaccount:people:ada"
	personPodAdaService = "person-ada"
	// personPodRobotAccount is an account that BROKER_POD_OPERATORS does not map. It logs in
	// under a login service of its own, personPodRobotService, and BROKER_SERVICES binds it to
	// no service, so its pod acts for no person and for no service.
	personPodRobotAccount = "system:serviceaccount:people:robot"
	personPodRobotService = "person-robot"
	// personPodAudience is the audience of the projected tokens both pods present
	// (BROKER_K8S_OIDC_AUDIENCE).
	personPodAudience = "example-broker-pod"
	// personPodSecret is personPodPerson's own secret at the agent tier, and personPodSecretValue
	// its value.
	personPodSecret      = "ADA_AGENT_TOKEN"
	personPodSecretValue = "ada-agent-token-value"
)

// personPodMapping is the deployment that both tests of this file read. Ada's account and robot's
// account each log in under a login service of its own, and BROKER_POD_OPERATORS maps ada's
// account, and only ada's, to ada.
var personPodMapping = []string{
	"BROKER_SERVICE_LOGINS=" + personPodAdaService + "=" + personPodAdaAccount + " " + personPodRobotService + "=" + personPodRobotAccount,
	"BROKER_POD_OPERATORS=" + personPodPerson + "=" + personPodAdaAccount,
}

// TestAPersonsPodReadsThePersonsAgentSecretWithNoApproval drives the REAL compiled broker on a
// real Postgres. A local OIDC issuer stands in for the cluster's ServiceAccount token issuer
// (BROKER_K8S_OIDC_ISSUER). The fake secrets store holds one secret, owned by ada at the agent
// tier. Each pod is the REAL compiled agent-secrets, which runs as a pod runs it: `enroll --pod
// --service <its login service>` with the projected token of its own account, bound to its own
// pod.
//
// Ada's pod runs as the account that BROKER_POD_OPERATORS maps to ada. Its enrolment names ada as
// its operator, and `agent-secrets request` of ada's secret answers granted with no approval,
// exit 0. The exec form then gives the secret's value to the command it runs. The command writes
// the value into a file of the test's own, so no log of this test holds the value.
//
// The negative control is in the same run. Robot's pod runs as an account the setting does not
// map, and it enrols the same way under its own login service. Its enrolment names no operator,
// and the same request answers pending for ada, exit 75. So the grant that ada's pod got is the
// mapping's, and not something that any pod of a login service gets.
//
// It skips when BROKER_TEST_DATABASE_URL is unset, as every Postgres-backed broker test does.
func TestAPersonsPodReadsThePersonsAgentSecretWithNoApproval(t *testing.T) {
	databaseURL := storetest.URL(t)
	issuer := oidctest.New(t)
	clusterKey := issuer.PublishKey(t, "cluster-key")
	store := &requesterStore{
		path: filepath.Join(t.TempDir(), "fake-secrets.json"),
		rows: []secrets.LocalSecret{requesterRow(personPodSecret, personPodPerson, policy.TierAgent, personPodSecretValue, nil)},
	}
	store.write(t)
	cli := buildAgentSecrets(t)

	broker := startBroker(t, brokerOptions{Env: append([]string{
		"BROKER_DATABASE_URL=" + databaseURL,
		"BROKER_LISTEN_ADDR=127.0.0.1:0",
		"BROKER_PUBLIC_URL=http://127.0.0.1:0",
		"BROKER_FAKE_SECRETS_FILE=" + store.path,
		"BROKER_K8S_OIDC_ISSUER=" + issuer.URL(),
		"BROKER_K8S_OIDC_AUDIENCE=" + personPodAudience,
		// Nothing is left in flight when the test ends, so the broker exits at once.
		"BROKER_DRAIN_SECONDS=0",
	}, personPodMapping...)})
	base := "http://" + broker.Addr
	if refused := recordsWith(broker.Boot, policy.RefusedMessage, nil); len(refused) != 0 {
		t.Fatalf("the broker refused a secret at boot, %v; it must serve %s, ada's own secret at the agent tier:\n%s", refused, personPodSecret, broker.Boot)
	}

	// enrol is one pod as its operator launches it. Its projected token, of account and bound to
	// the pod whose UID is session.runtimeID, is mounted as a file, and the real client enrols the
	// pod into a key directory of its own with `enroll --pod`. A pod states no session name, so
	// its pod UID is its runtime id and the name this test calls it by.
	enrol := func(session requesterSession, account, service string) requesterSession {
		t.Helper()
		tokenFile := filepath.Join(t.TempDir(), session.runtimeID+"-token")
		writeWhole(t, tokenFile, mintPodToken(t, issuer, clusterKey, account, personPodAudience, session.runtimeID))
		session.keyDir = filepath.Join(t.TempDir(), session.runtimeID)
		code, stdout, stderr := session.run(t, cli, base, "enroll", "--pod", "--service", service, "--pod-token-file", tokenFile, "--out", session.keyDir)
		session.enrollmentID = strings.TrimSpace(stdout)
		if code != 0 || session.enrollmentID == "" {
			t.Fatalf("agent-secrets enroll --pod --service %s with a token of %s bound to pod %s exited %d, want 0 and the enrollment id on stdout\nstdout: %s\nstderr: %s",
				service, account, session.runtimeID, code, stdout, stderr)
		}
		return session
	}
	// operatorOf is the operator that `agent-secrets self` reads for the pod's enrolment
	// (GET /v1/enrollments/self), "" for none.
	operatorOf := func(session requesterSession) string {
		t.Helper()
		code, stdout, stderr := session.run(t, cli, base, "self")
		if code != 0 {
			t.Fatalf("agent-secrets self as %s exited %d, want 0\nstdout: %s\nstderr: %s", session.actor(), code, stdout, stderr)
		}
		return cliField(t, stdout, "operator")
	}

	t.Log("--- ada's pod: a pod-bound token of the account BROKER_POD_OPERATORS maps to ada enrols under ada's login service")
	adaPod := enrol(requesterSession{name: "ada-pod-1", runtimeID: "ada-pod-1", person: personPodPerson}, personPodAdaAccount, personPodAdaService)
	if operator := operatorOf(adaPod); operator != personPodPerson {
		t.Fatalf("ada's pod enrolment %s names operator %q, want %s: the token login of a mapped account's pod writes its person as the credential's operator, and the enrolment takes it",
			adaPod.enrollmentID, operator, personPodPerson)
	}

	t.Log("--- ada's pod asks for ada's agent-tier secret and is granted it with no approval")
	code, stdout, stderr := adaPod.run(t, cli, base, "request", personPodSecret, "--reason", "test")
	if state := cliField(t, stdout, "state"); code != 0 || state != "granted" {
		t.Fatalf("agent-secrets request %s as ada's pod (enrolment %s) exited %d with state %q, want 0 and granted: the pod acts as ada, and ada's own agent-tier secret needs no approval\nstdout: %s\nstderr: %s",
			personPodSecret, adaPod.enrollmentID, code, state, stdout, stderr)
	}

	t.Log("--- the exec form gives the value to the command it runs")
	seen := filepath.Join(t.TempDir(), "seen")
	code, stdout, stderr = adaPod.run(t, cli, base, "--wait", "0s", "--reason", "test", personPodSecret, "--",
		"sh", "-c", `printf %s "$`+personPodSecret+`" > "$0"`, seen)
	if code != 0 {
		t.Fatalf("agent-secrets %s -- sh as ada's pod (enrolment %s) exited %d, want 0 with the command run\nstdout: %s\nstderr: %s",
			personPodSecret, adaPod.enrollmentID, code, stdout, stderr)
	}
	got, err := os.ReadFile(seen)
	if err != nil {
		t.Fatalf("the command that agent-secrets ran as ada's pod wrote no value: %v\nstderr: %s", err, stderr)
	}
	if string(got) != personPodSecretValue {
		t.Fatalf("the command that agent-secrets ran as ada's pod saw %d bytes in $%s, which is not the secret's value (%d bytes)",
			len(got), personPodSecret, len(personPodSecretValue))
	}

	t.Log("--- the negative control: robot's pod, an account the setting does not map, enrols the same way under its own login service")
	robotPod := enrol(requesterSession{name: "robot-pod-1", runtimeID: "robot-pod-1", service: personPodRobotService}, personPodRobotAccount, personPodRobotService)
	if operator := operatorOf(robotPod); operator != "" {
		t.Fatalf("robot's pod enrolment %s names operator %q, want none: BROKER_POD_OPERATORS does not map %s", robotPod.enrollmentID, operator, personPodRobotAccount)
	}
	code, stdout, stderr = robotPod.run(t, cli, base, "request", personPodSecret, "--reason", "test")
	if state := cliField(t, stdout, "state"); code != cliExitPending || state != "pending" {
		t.Fatalf("agent-secrets request %s as robot's pod (enrolment %s) exited %d with state %q, want %d and pending: a pod that acts for no person gets ada's secret only when ada approves it\nstdout: %s\nstderr: %s",
			personPodSecret, robotPod.enrollmentID, code, state, cliExitPending, stdout, stderr)
	}
	if want := "waiting for " + personPodPerson + " to approve it"; !strings.Contains(stdout, want) {
		t.Fatalf("robot's pending request does not wait on %s, the secret's owner; want %q in:\n%s", personPodPerson, want, stdout)
	}
}

// TestAPersonsPodNamesItsProjectsThroughTheBinaries drives a person's pod's projects through the
// REAL compiled broker, its project-access client pointed at a Dispatch stand-in, and the REAL
// compiled agent-secrets. Dispatch calls ada a full member: PRIV's access list names her, and
// ELSE's does not. Each project holds a shared agent-tier secret, which owner and tier alone would
// give her pod at once, so only the project rule keeps it.
//
// Ada's pod run as `enroll --pod` with AGENT_SECRETS_PROJECTS=PRIV enrols, `agent-secrets self`
// prints PRIV, and the pod reads PRIV's secret. It is denied ELSE's secret project-not-named, and
// the command it would run never runs. So the pod sees what her own session naming PRIV sees, and
// no more. A second broker, whose Dispatch calls ada a restricted member, enrolls no pod of hers,
// naming PRIV or naming no project: the CLI prints RESTRICTED_MEMBER_ENROLLS_NOTHING, as her
// machine login gets. The other refusals of a pod's list are enroll.Service.Create's
// (internal/cipher/enroll/person_pod_test.go), each answered by its own code
// (internal/cipher/api/enroll_projects_test.go).
//
// It skips when BROKER_TEST_DATABASE_URL is unset, as every Postgres-backed broker test does.
func TestAPersonsPodNamesItsProjectsThroughTheBinaries(t *testing.T) {
	const (
		privProject, elseProject = "PRIV", "ELSE"
		privKey, privValue       = "ADA_PRIV_KEY", "ada-priv-value"
		elseKey                  = "ELSE_KEY"
	)
	databaseURL := storetest.URL(t)
	issuer := oidctest.New(t)
	clusterKey := issuer.PublishKey(t, "cluster-key")
	rows := map[string]projectaccesstest.Row{
		privProject: {Visibility: "private", Owner: privateOwner, Listed: []string{personPodPerson}},
		elseProject: {Visibility: "private", Owner: privateOwner},
	}
	store := &requesterStore{path: filepath.Join(t.TempDir(), "fake-secrets.json"), rows: []secrets.LocalSecret{
		requesterRow(privKey, policy.OwnerShared, policy.TierAgent, privValue, map[string]string{policy.TagProject: privProject}),
		requesterRow(elseKey, policy.OwnerShared, policy.TierAgent, "else-value", map[string]string{policy.TagProject: elseProject}),
	}}
	store.write(t)
	cli := buildAgentSecrets(t)

	// brokerAsking starts a broker whose project-access client asks a Dispatch stand-in that
	// calls ada a member of kind kind, and answers the broker's base URL and the stand-in.
	brokerAsking := func(kind string) (string, *projectaccesstest.Server) {
		t.Helper()
		dispatch := projectaccesstest.NewServer(t, rows, map[string]string{personPodPerson: kind})
		tokenFile := filepath.Join(t.TempDir(), "dispatch-access-token")
		writeWhole(t, tokenFile, dispatch.Token+"\n")
		broker := startBroker(t, brokerOptions{Env: append([]string{
			"BROKER_DATABASE_URL=" + databaseURL,
			"BROKER_LISTEN_ADDR=127.0.0.1:0",
			"BROKER_PUBLIC_URL=http://127.0.0.1:0",
			"BROKER_FAKE_SECRETS_FILE=" + store.path,
			"BROKER_K8S_OIDC_ISSUER=" + issuer.URL(),
			"BROKER_K8S_OIDC_AUDIENCE=" + personPodAudience,
			"BROKER_PROJECT_ACCESS_URL=" + dispatch.URL,
			"BROKER_PROJECT_ACCESS_TOKEN_FILE=" + tokenFile,
			"BROKER_DRAIN_SECONDS=0",
		}, personPodMapping...)})
		requireHealthy(t, broker.Addr)
		broker.AwaitRecord(t, "project access check", map[string]any{"state": "on"})
		if refused := recordsWith(broker.Boot, policy.RefusedMessage, nil); len(refused) != 0 {
			t.Fatalf("the broker refused a secret at boot, %v; a broker with project access serves a tagged secret:\n%s", refused, broker.Boot)
		}
		return "http://" + broker.Addr, dispatch
	}
	base, dispatch := brokerAsking(projectaccess.MemberFull)

	// enrol runs the real client as ada's pod podUID against the broker at base: `enroll --pod`
	// with the pod's projected token mounted as a file, into a key directory of its own, with env
	// added to the pod's environment. It answers the pod's session and the run's exit code and
	// stderr.
	enrol := func(base, podUID string, env []string) (requesterSession, int, string) {
		t.Helper()
		tokenPath := filepath.Join(t.TempDir(), podUID+"-token")
		writeWhole(t, tokenPath, mintPodToken(t, issuer, clusterKey, personPodAdaAccount, personPodAudience, podUID))
		session := requesterSession{name: podUID, runtimeID: podUID, person: personPodPerson, keyDir: filepath.Join(t.TempDir(), podUID)}
		cmd := exec.Command(cli, "enroll", "--pod", "--service", personPodAdaService, "--pod-token-file", tokenPath, "--out", session.keyDir)
		cmd.Env = append(agentSecretsEnv(base, session.keyDir), env...)
		var out, errs strings.Builder
		cmd.Stdout, cmd.Stderr = &out, &errs
		var exit *exec.ExitError
		if err := cmd.Run(); err != nil && !errors.As(err, &exit) {
			t.Fatalf("agent-secrets enroll --pod as %s's pod %s: %v", personPodPerson, podUID, err)
		}
		t.Logf("$ %s agent-secrets %s\n< exit %d\n< stdout: %s\n< stderr: %s", strings.Join(env, " "), strings.Join(cmd.Args[1:], " "), cmd.ProcessState.ExitCode(), out.String(), errs.String())
		session.enrollmentID = strings.TrimSpace(out.String())
		return session, cmd.ProcessState.ExitCode(), errs.String()
	}

	t.Log("--- ada's pod with AGENT_SECRETS_PROJECTS=PRIV enrols naming PRIV")
	pod, code, stderr := enrol(base, "ada-pod-1", []string{"AGENT_SECRETS_PROJECTS=" + privProject})
	if code != 0 || pod.enrollmentID == "" {
		t.Fatalf("enroll --pod as ada's pod under AGENT_SECRETS_PROJECTS=%s exited %d, want 0 and the enrollment id\nstderr: %s", privProject, code, stderr)
	}
	code, stdout, stderr := pod.run(t, cli, base, "self")
	if code != 0 || cliField(t, stdout, "projects") != privProject || cliField(t, stdout, "operator") != personPodPerson {
		t.Fatalf("agent-secrets self as ada's pod exited %d, want 0 naming operator %s and projects %s\nstdout: %s\nstderr: %s", code, personPodPerson, privProject, stdout, stderr)
	}

	t.Log("--- the pod reads PRIV's secret, and is denied ELSE's")
	code, stdout, stderr = pod.run(t, cli, base, privKey, "--", "sh", "-c", `printf %s "$`+privKey+`"`)
	if code != 0 || stdout != privValue {
		t.Fatalf("agent-secrets %s as ada's pod naming PRIV exited %d with %q, want 0 and its value\nstderr: %s", privKey, code, stdout, stderr)
	}
	code, stdout, stderr = pod.run(t, cli, base, elseKey, "--", "sh", "-c", "echo ran")
	if code != cliExitDenied || strings.Contains(stdout, "ran") || !strings.Contains(stderr, elseKey+": "+policy.ReasonProjectNotNamed) {
		t.Fatalf("agent-secrets %s as ada's pod naming PRIV exited %d with stdout %q, want %d (denied %s) and nothing run\nstderr: %s",
			elseKey, code, stdout, cliExitDenied, policy.ReasonProjectNotNamed, stderr)
	}
	if dispatch.ProjectCalls() == 0 {
		t.Fatal("the broker asked Dispatch nothing of a project, so no answer above read one: the run is blind")
	}

	t.Log("--- a broker whose Dispatch calls ada a restricted member enrols no pod of hers, naming PRIV or no project")
	restrictedBase, _ := brokerAsking(projectaccess.MemberRestricted)
	for i, env := range [][]string{{"AGENT_SECRETS_PROJECTS=" + privProject}, nil} {
		if _, code, stderr := enrol(restrictedBase, fmt.Sprintf("ada-pod-restricted-%d", i), env); code == 0 || !strings.Contains(stderr, "RESTRICTED_MEMBER_ENROLLS_NOTHING") {
			t.Fatalf("enroll --pod as ada's pod (%v) on a broker that calls her restricted exited %d, want a refusal RESTRICTED_MEMBER_ENROLLS_NOTHING\nstderr: %s", env, code, stderr)
		}
	}
}

// TestASecretOwnedByAMappedAccountsLoginServiceIsRefusedAtLoad pins that the login service of an
// account BROKER_POD_OPERATORS maps is no owner at the policy. The test loads this file's
// deployment with config.Load, and loads the policy with the services that cmd/cipherd gives the
// loader (serviceNames). A secret whose owner tag is person-ada, ada's account's login service, is
// not served and is refused owner-tag-malformed, because that tag names no person, no service and
// not shared. A secret owned by the one service that BROKER_SERVICES registers is served beside
// it, which shows that the loader reads the configured services: the refusal is the login
// service's, not an empty service list. It needs no database.
func TestASecretOwnedByAMappedAccountsLoginServiceIsRefusedAtLoad(t *testing.T) {
	const (
		loginOwnedSecret   = "PERSON_ADA_KEY"
		serviceOwnedSecret = "SERVICE_KEY"
		registeredService  = "example-service"
	)
	env := map[string]string{
		"BROKER_DATABASE_URL":        "postgres://unused",
		"BROKER_PUBLIC_URL":          "https://secrets.internal.example",
		"BROKER_SECRETS_PREFIX":      testSecretsPrefix,
		"BROKER_SECRETS_KMS_KEY_ARN": testSecretsKMSKeyARN,
		"BROKER_K8S_OIDC_ISSUER":     "https://cluster.example",
		"BROKER_K8S_OIDC_AUDIENCE":   personPodAudience,
		"BROKER_SERVICES":            registeredService + "=system:serviceaccount:example:example-sa",
		// The four that the approval page's sign-in needs. Every broker refuses to start without
		// them, so this configuration is a whole one, as a deployment's is.
		"BROKER_SIGNIN_ISSUER":             testSignInIssuer,
		"BROKER_SIGNIN_CLIENT_ID":          ciphertest.SignInClientID,
		"BROKER_SIGNIN_CLIENT_SECRET_FILE": signInClientSecretFile(t),
		"BROKER_SIGNIN_DOMAIN":             ciphertest.SignInDomain,
	}
	for _, setting := range personPodMapping {
		name, value, _ := strings.Cut(setting, "=")
		env[name] = value
	}
	cfg, err := config.Load(func(name string) string { return env[name] })
	if err != nil {
		t.Fatalf("config.Load of the person-pod deployment: %v", err)
	}
	if cfg.PodOperators[personPodAdaAccount] != personPodPerson || cfg.ServiceLogins[personPodAdaAccount] != personPodAdaService {
		t.Fatalf("config.Load maps %s to %q under login service %q, want %s under %s: the owner tag below must name a mapped account's login service",
			personPodAdaAccount, cfg.PodOperators[personPodAdaAccount], cfg.ServiceLogins[personPodAdaAccount], personPodPerson, personPodAdaService)
	}

	set := policytest.Current(t, secrets.NewLocal(
		policytest.Secret(loginOwnedSecret, personPodAdaService, policy.TierAgent, "login-owned-value"),
		policytest.Secret(serviceOwnedSecret, registeredService, policy.TierAgent, "service-owned-value"),
	), serviceNames(cfg)...).Get()
	if _, served := set.Secrets[serviceOwnedSecret]; !served {
		t.Fatalf("the policy does not serve %s, owned by %s, which BROKER_SERVICES registers (services %v, refused %+v): the loader must read the configured services for the refusal below to be the login service's",
			serviceOwnedSecret, registeredService, serviceNames(cfg), set.Refused)
	}
	if secret, served := set.Secrets[loginOwnedSecret]; served {
		t.Fatalf("the policy serves %s, whose owner tag is %s, the login service of the account that BROKER_POD_OPERATORS maps to %s: %+v",
			loginOwnedSecret, personPodAdaService, personPodPerson, secret)
	}
	if got, want := set.Refused[policytest.ID(loginOwnedSecret)], (policy.Refusal{Reason: policy.ReasonOwnerTagMalformed}); got != want {
		t.Fatalf("%s, owned by %s, is refused as %+v, want %+v (all refused: %+v)", loginOwnedSecret, personPodAdaService, got, want, set.Refused)
	}
}

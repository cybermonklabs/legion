package main

//ci:builds packages/envoy ./cmd/cipher cmdtest.BuildCommand . in ../cipher

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/envoy/internal/cipher/leasetime"
	"github.com/sjawhar/envoy/internal/cipher/proof"
	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/cipher/store/storetest"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// TestABrokerOutageLongerThanALease reproduces dispatch INFRA-487, the 2026-10-26 broker cutover:
// no broker served for longer than a pod's enrollment lease, and the first sweep of the broker
// that came back ended every enrollment whose lease ran out while none was serving, so each pod's
// `agent-secrets renew` was refused ENROLLMENT_NOT_LIVE and exited 76, and the pod was refused
// every secret until it was relaunched. It runs the REAL broker binary twice on one database and
// the REAL agent-secrets binary, with one local OIDC issuer standing in for the cluster's token
// issuer the whole time, at a sweep of one second, the shortest lease config.Load accepts at that
// sweep, and no drain. Both brokers are reached at one address a TCP forwarder the test owns
// holds across the restart, as a Service's is: BROKER_PUBLIC_URL and AGENT_SECRETS_URL both name
// it, so a proof's htu is as good after the restart as before, and while no broker runs the
// forwarder refuses every connection at once. The outage runs from the first broker's exit until
// a lease, the three sweeps the broker tolerates between two records that it serves, and the
// renewer's longest retry wait have all passed.
//
// Four pods of one service, enrolled by its launcher and granted its agent-tier secret with no
// approver, prove one thing each. The survivor is the outage itself: keyed by `agent-secrets
// keygen` and enrolled into its key dir as the worker shim's pods are, it renews through
// `agent-secrets renew --pod-token-file` from before the outage to the end, once a third of its
// lease has passed, which two of its renewals before the outage show. Once the second broker
// listens its first fetch through the CLI is served, its renewer, which never exited, renews its
// lease to a whole one, and the second broker logs that it gave back the time no broker was
// serving. The revoked pod, ended by its launcher, and the lapsed pod, whose
// lease the first broker's sweeper ended while that broker served, prove the time given back
// revives nothing that had ended: each is refused a fetch, PROOF_INVALID not live, and a renewal,
// ENROLLMENT_NOT_LIVE. The mismatch pod, whose lease ran out in the outage as the survivor's did,
// proves a lease given back renews for no one else: a renewal carrying another pod's token, or
// another service account's, is refused POD_IDENTITY_MISMATCH and renews its lease not at all. The
// broker's log line is read last, so that against a broker without the fix the test fails on the
// refusal the pods met rather than on a missing line.
//
// A renewal is read from the database's record of each update of a lease (recordLeaseWrites),
// never from a lease moving: the serving clock's give-back moves every live lease too, and not
// only after an outage. A broker that serves while its beats fail for more than NotServingSweeps
// sweeps, three seconds of a slow database at this row's sweep, gives that gap back at its next
// beat (enroll.ServingClock.Beat), and a row that read such a move as a renewal read the renewer
// as renewing seconds after the renewal before it.
func TestABrokerOutageLongerThanALease(t *testing.T) {
	databaseURL := storetest.URL(t)
	issuer := oidctest.New(t)
	podKey := issuer.PublishKey(t, "signing-key")
	const (
		podAudience  = "example-broker-pod"
		service      = "example-service"
		loginAccount = "system:serviceaccount:example-system:example-service-login"
		podAccount   = "system:serviceaccount:example:example-worker"
		otherAccount = "system:serviceaccount:example:example-other-worker"
		secretName   = "OUTAGE_TOKEN"
		secretValue  = "outage-token-value"
	)
	// sweep is BROKER_SWEEP_SECONDS and lease is BROKER_LEASE_SECONDS: the shortest lease that
	// outlives a gap no broker gives back and the retries its renewer then takes, which is what
	// config.Load refuses below (internal/cipher/leasetime). Both are rendered into the
	// environment below, so this pair cannot drift from that refusal again. A renewer renews
	// once a third of the lease has passed, so a pod that renews holds at least two thirds of it.
	sweep := time.Second
	lease := leasetime.Production().Shortest(sweep)
	// notServing is the longest gap between two records that a broker serves which still
	// counts as one serving: leasetime.NotServingSweeps sweeps.
	notServing := leasetime.NotServingSweeps * sweep
	// renewerLongestWait is agent-secrets renew's longest wait between two failed attempts
	// (nextBackoff's cap), so a renewer whose broker is back renews within it.
	renewerLongestWait := leasetime.RenewBackoffMax
	fakeSecretsFile := filepath.Join(t.TempDir(), "fake-secrets.json")
	fakeSecrets := `{"secrets": [
		{"name": "` + testSecretsPrefix + `outage-token", "kms_key_id": "` + testSecretsKMSKeyARN + `", "tags": {"owner": "` + service + `", "tier": "agent"}, "value": "` + secretValue + `"}
	]}`
	if err := os.WriteFile(fakeSecretsFile, []byte(fakeSecrets), 0o600); err != nil {
		t.Fatal(err)
	}
	brokerBinary := buildBroker(t)
	cli := buildAgentSecrets(t)
	// Started before either broker, so its cleanup runs after theirs and never waits on a
	// connection to a broker still running.
	front := startForwarder(t)
	env := []string{
		"BROKER_DATABASE_URL=" + databaseURL,
		"BROKER_LISTEN_ADDR=127.0.0.1:0",
		"BROKER_PUBLIC_URL=" + front.url,
		"BROKER_FAKE_SECRETS_FILE=" + fakeSecretsFile,
		"BROKER_K8S_OIDC_ISSUER=" + issuer.URL(),
		"BROKER_K8S_OIDC_AUDIENCE=" + podAudience,
		"BROKER_SERVICE_LOGINS=" + service + "=" + loginAccount,
		"BROKER_SERVICES=" + service + "=" + podAccount,
		"BROKER_LEASE_SECONDS=" + strconv.Itoa(int(lease/time.Second)),
		"BROKER_SWEEP_SECONDS=" + strconv.Itoa(int(sweep/time.Second)),
		"BROKER_DRAIN_SECONDS=0",
	}
	serve := func() *brokerProcess {
		t.Helper()
		process := startBroker(t, brokerOptions{Binary: brokerBinary, Env: env})
		front.route(process.Addr)
		return process
	}
	first := serve()
	broker := podBroker{t: t, base: front.url, issuer: issuer, key: podKey, audience: podAudience}

	ctx := context.Background()
	db := connectToBroker(t, databaseURL)
	// leaseOf answers an enrollment's lease_expires_at and the database's now(), so every
	// comparison of the two is on Postgres's clock, the one the broker's liveness reads.
	leaseOf := func(enrollmentID string) (expires, now time.Time) {
		t.Helper()
		if err := db.QueryRow(ctx, `select lease_expires_at, now() from enrollments where id = $1`, enrollmentID).Scan(&expires, &now); err != nil {
			t.Fatalf("read the lease of enrollment %s: %v", enrollmentID, err)
		}
		return expires, now
	}
	recordLeaseWrites(t, db)
	podToken := func(uid, account string) string {
		claims := issuer.Claims(account, podAudience)
		claims["kubernetes.io"] = map[string]any{"pod": map[string]any{"uid": uid}}
		return issuer.Mint(t, podKey, claims)
	}

	t.Log("--- the service logs in with its token")
	launcher := broker.logIn(service, loginAccount)

	t.Log("--- the survivor: a key from keygen, enrolled by the launcher into its key dir, a renewer, and a fetch through the CLI")
	keyDir := filepath.Join(t.TempDir(), "survivor")
	keygen := exec.Command(cli, "keygen", "--out", keyDir)
	keygen.Env = agentSecretsEnv(front.url, keyDir)
	thumbprint, err := keygen.Output()
	if err != nil {
		t.Fatalf("agent-secrets keygen --out %s: %v", keyDir, err)
	}
	const survivorUID = "survivor-pod"
	survivorID := enrollKey(broker, launcher, survivorUID, strings.TrimSpace(string(thumbprint)), podToken(survivorUID, podAccount))
	writeWhole(t, filepath.Join(keyDir, "enrollment"), survivorID+"\n")
	tokenFile := filepath.Join(t.TempDir(), "token")
	writeWhole(t, tokenFile, podToken(survivorUID, podAccount))
	renew := exec.Command(cli, "renew", "--pod-token-file", tokenFile)
	renew.Env = agentSecretsEnv(front.url, keyDir)
	renewer := cmdtest.Start(t, "agent-secrets renew", renew)
	renewerRunning := func(when string) {
		t.Helper()
		select {
		case <-renewer.Exited:
			t.Fatalf("the survivor's renewer exited %s (%v):\n%s", when, renewer.Cmd.ProcessState, renewer.Output.String())
		default:
		}
	}
	fetch := func(when string) {
		t.Helper()
		started := time.Now()
		command := exec.Command(cli, secretName, "--", "sh", "-c", `printf %s "$`+secretName+`"`)
		command.Env = agentSecretsEnv(front.url, keyDir)
		var stdout, stderr strings.Builder
		command.Stdout, command.Stderr = &stdout, &stderr
		if err := command.Start(); err != nil {
			t.Fatalf("start the survivor's fetch %s: %v", when, err)
		}
		// A fetch the wait below gives up on is killed as the row ends; one that has exited
		// is no process to kill, and the kill's error says only that.
		defer func() { _ = command.Process.Kill() }()
		ran := make(chan error, 1)
		go func() { ran <- command.Wait() }()
		err := testwait.Receive(t, "the survivor's fetch "+when+" to finish", ran)
		took := time.Since(started)
		if err != nil || stdout.String() != secretValue {
			t.Fatalf("the survivor's fetch %s = %v, stdout %q, want %q; its stderr:\n%s\nits renewer's:\n%s",
				when, err, stdout.String(), secretValue, stderr.String(), renewer.Output.String())
		}
		t.Logf("the survivor's fetch %s was served in %s: %s", when, took.Round(time.Millisecond), strings.TrimSpace(stderr.String()))
	}
	fetch("before the outage")

	t.Log("--- the revoked pod: enrolled, then ended by its launcher")
	revoked := broker.enroll(launcher, "revoked-pod", podAccount, "")
	if status := deleteEnrollment(t, front.url, launcher, revoked.enrollmentID); status != http.StatusNoContent {
		t.Fatalf("the launcher's DELETE of the revoked pod's enrollment = %d, want 204", status)
	}

	t.Log("--- the lapsed pod: enrolled, its lease set in the past, then ended by the sweeper of a broker serving")
	lapsed := broker.enroll(launcher, "lapsed-pod", podAccount, "")
	if _, err := db.Exec(ctx, `update enrollments set lease_expires_at = now() - interval '1 second' where id = $1`, lapsed.enrollmentID); err != nil {
		t.Fatalf("set the lapsed pod's lease in the past: %v", err)
	}
	first.AwaitRecord(t, "broker sweeper: ended an enrollment whose lease lapsed", map[string]any{"enrollment_id": lapsed.enrollmentID})

	t.Log("--- the mismatch pod: enrolled, and served its secret once")
	mismatch := broker.enroll(launcher, "mismatch-pod", podAccount, "")
	reply := broker.ask(mismatch, secretName)
	if reply.State != "granted" || reply.GrantID == nil {
		t.Fatalf("the mismatch pod's request = %+v, want granted with a grant", reply)
	}
	if values := broker.values(mismatch, *reply.GrantID); values[secretName] != secretValue {
		t.Fatalf("the mismatch pod's grant values = %v, want %s=%s", values, secretName, secretValue)
	}

	// The renewer's cadence, read from the renewals the broker wrote (renewalsOf): a renewal sets
	// the lease to run out a whole lease after the broker's moment of renewal
	// (internal/cipher/enroll), so two renewals one after the other are as far apart as the
	// expiries they wrote. The renewer renews once a third of what it holds has passed
	// (leasetime.RenewAfter), so that gap is a third of a lease and the request that carried it,
	// and the row reads it as the third, nearer that than a quarter or a half of one.
	t.Log("--- the survivor's renewer renews at a third of its lease")
	seen := len(renewalsOf(t, db, survivorID))
	renewals := testwait.UntilPolling(t, 250*time.Millisecond, "the survivor's renewer to renew its lease twice", func() ([]leaseWrite, bool) {
		renewerRunning("before the outage")
		renewals := renewalsOf(t, db, survivorID)
		return renewals, len(renewals) >= seen+2
	})
	gap := renewals[seen+1].expires.Sub(renewals[seen].expires)
	if promised := lease / leasetime.RenewDivisor; gap <= lease*7/24 || gap >= lease*5/12 {
		t.Fatalf("the survivor's renewer renewed its lease %s after the renewal before it; want a third of its %s lease, %s (leasetime.RenewAfter), nearer that than a quarter or a half of one:\n%s",
			gap, lease, promised, renewer.Output.String())
	}
	t.Logf("the survivor's renewer renewed its lease %s after the renewal before it: a third of its %s lease", gap, lease)

	// The margin: the broker is stopped while the survivor holds more than two thirds of its
	// lease, between two of its renewals. The time given back restores the lease the survivor held
	// at the first broker's last record that it served, no later than the stop, so the restored
	// survivor holds more than two thirds of a lease, far more than its renewer's longest retry
	// wait, and it renews well inside that.
	t.Log("--- the outage: the broker is stopped between two of the survivor's renewals")
	atStop := testwait.UntilPolling(t, 250*time.Millisecond, "the survivor to hold more than two thirds of its lease", func() (time.Duration, bool) {
		renewerRunning("before the outage")
		expires, now := leaseOf(survivorID)
		left := expires.Sub(now)
		return left, left > lease*2/3+time.Second
	})
	t.Logf("the survivor holds %s of its lease at the stop", atStop.Round(time.Millisecond))
	front.route("")
	first.Terminate(t)
	if err := first.AwaitExit(t); err != nil {
		t.Fatalf("the first broker's exit = %v, want a clean one:\n%s", err, first.Log())
	}
	stopped := time.Now()
	// No broker serves from here until the second one listens, so each renewal of the survivor's
	// lease recorded after these is one the second broker wrote.
	renewedBefore := len(renewalsOf(t, db, survivorID))
	outage := lease + notServing + renewerLongestWait
	t.Logf("the first broker exited; no broker serves for %s", outage)
	time.Sleep(time.Until(stopped.Add(outage)))
	renewerRunning("during the outage")

	t.Log("--- a second broker serves the same database at the same address")
	restarted := serve()
	restartedAt := time.Now()
	t.Logf("the second broker listens %s after the first exited", restartedAt.Sub(stopped).Round(time.Millisecond))

	t.Log("--- (1) the survivor's first fetch through the CLI is served")
	fetch("after the restart")
	t.Logf("(1) served %s after the second broker listened", time.Since(restartedAt).Round(time.Millisecond))

	t.Log("--- (2) the survivor's renewer, which never exited, renews a whole lease")
	renewed := testwait.UntilPolling(t, 100*time.Millisecond, "the survivor's renewer to renew its lease against the second broker", func() (leaseWrite, bool) {
		if renewals := renewalsOf(t, db, survivorID); len(renewals) > renewedBefore {
			return renewals[renewedBefore], true
		}
		renewerRunning("after the restart")
		return leaseWrite{}, false
	})
	if left := renewed.expires.Sub(renewed.at); left < lease-2*time.Second || left > lease+time.Second {
		t.Fatalf("the survivor's renewal against the second broker set its lease to run out %s after the renewal, want about %s", left, lease)
	}
	t.Logf("(2) renewed %s after the second broker listened, to %s past the renewal", time.Since(restartedAt).Round(time.Millisecond), renewed.expires.Sub(renewed.at).Round(time.Millisecond))
	renewerRunning("after it renewed")

	for _, ended := range []struct {
		name string
		pod  pod
	}{{"revoked", revoked}, {"lapsed", lapsed}} {
		t.Logf("--- (3) the %s pod stays ended: refused a fetch and a renewal", ended.name)
		request, err := record.Sign(ended.pod.key, front.url, []record.AuthorizationDetail{
			{Type: record.KindAgentSecret, Identifier: secretName, Actions: []string{"inject"}},
		}, ended.pod.runtimeID+" needs "+secretName, "", nil, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		status, answer := broker.signed(ended.pod, "/v1/requests", map[string]any{"request": request, "session_id": nil})
		if code, message := brokerRefusal(answer); status != http.StatusUnauthorized || code != "PROOF_INVALID" || !strings.Contains(message, "not live") {
			t.Fatalf("the %s pod's fetch = %d %s, want 401 PROOF_INVALID not live", ended.name, status, answer)
		}
		status, answer = broker.signed(ended.pod, "/v1/enrollments/"+ended.pod.enrollmentID+"/renew", nil)
		if code, _ := brokerRefusal(answer); status != http.StatusUnauthorized || code != "ENROLLMENT_NOT_LIVE" {
			t.Fatalf("the %s pod's renewal = %d %s, want 401 ENROLLMENT_NOT_LIVE", ended.name, status, answer)
		}
	}

	t.Log("--- (4) the mismatch pod's lease renews for no other pod and no other account")
	before := len(renewalsOf(t, db, mismatch.enrollmentID))
	for _, other := range []struct{ what, token string }{
		{"another pod's UID", podToken("another-pod", podAccount)},
		{"another service account", podToken(mismatch.runtimeID, otherAccount)},
	} {
		status, answer := broker.signed(mismatch, "/v1/enrollments/"+mismatch.enrollmentID+"/renew", map[string]any{"pod_token": other.token})
		if code, _ := brokerRefusal(answer); status != http.StatusForbidden || code != "POD_IDENTITY_MISMATCH" {
			t.Fatalf("the mismatch pod's renewal with a token naming %s = %d %s, want 403 POD_IDENTITY_MISMATCH", other.what, status, answer)
		}
	}
	if after := renewalsOf(t, db, mismatch.enrollmentID); len(after) != before {
		t.Fatalf("the mismatch pod's lease was renewed to %s on two refused renewals", after[len(after)-1].expires)
	}

	t.Log("--- (5) the second broker logged that it gave back the time no broker was serving")
	const gaveBack = "gave back the time no broker was serving"
	for _, line := range strings.Split(restarted.Await(t, gaveBack), "\n") {
		if strings.Contains(line, gaveBack) {
			t.Logf("(5) %s", line)
		}
	}
	renewerRunning("at the end")
}

// leaseWrite is one update of an enrollment's lease that recordLeaseWrites recorded: the
// lease_expires_at it wrote, and when, on Postgres's clock.
type leaseWrite struct{ expires, at time.Time }

// recordLeaseWrites has the database db is connected to record each update of an enrollment's
// lease, and each transaction that writes a broker_serving row, for renewalsOf. The enrollment's
// insert writes its first lease, which no update trigger sees; two of the broker's statements
// update one: a renewal (enroll.Service.Renew) and the serving clock's give-back
// (enroll.ServingClock.Beat), and the give-back is written in a beat, the one transaction that
// also writes broker_serving. The row's own update, the lapsed pod's lease set in the past, is the
// one other.
func recordLeaseWrites(t *testing.T, db *pgx.Conn) {
	t.Helper()
	for _, statement := range []string{
		`create table outage_test_lease_writes (enrollment_id uuid not null, lease_expires_at timestamptz not null,
			xact xid8 not null default pg_current_xact_id(), written_at timestamptz not null default clock_timestamp())`,
		`create table outage_test_beats (xact xid8 primary key)`,
		`create function outage_test_lease_write() returns trigger language plpgsql as $$
			begin insert into outage_test_lease_writes (enrollment_id, lease_expires_at) values (new.id, new.lease_expires_at); return null; end $$`,
		`create trigger outage_test_lease_write after update of lease_expires_at on enrollments
			for each row execute function outage_test_lease_write()`,
		`create function outage_test_beat() returns trigger language plpgsql as $$
			begin insert into outage_test_beats values (pg_current_xact_id()) on conflict do nothing; return null; end $$`,
		`create trigger outage_test_beat after insert or update on broker_serving
			for each row execute function outage_test_beat()`,
	} {
		if _, err := db.Exec(context.Background(), statement); err != nil {
			t.Fatalf("record the writes of the enrollments' leases: %v", err)
		}
	}
}

// renewalsOf answers the renewals of enrollmentID's lease that recordLeaseWrites recorded, oldest
// first: each update of it but those made in a transaction that wrote broker_serving, which are
// the serving clock's give-backs.
func renewalsOf(t *testing.T, db *pgx.Conn, enrollmentID string) []leaseWrite {
	t.Helper()
	rows, err := db.Query(context.Background(), `select lease_expires_at, written_at from outage_test_lease_writes w
		where enrollment_id = $1 and not exists (select from outage_test_beats b where b.xact = w.xact)
		order by written_at`, enrollmentID)
	if err != nil {
		t.Fatalf("read the renewals of enrollment %s: %v", enrollmentID, err)
	}
	renewals, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (leaseWrite, error) {
		var renewal leaseWrite
		err := row.Scan(&renewal.expires, &renewal.at)
		return renewal, err
	})
	if err != nil {
		t.Fatalf("read the renewals of enrollment %s: %v", enrollmentID, err)
	}
	return renewals
}

// buildAgentSecrets builds cmd/cipher into a temporary directory and returns its path, as
// buildBroker builds this command.
func buildAgentSecrets(t *testing.T) string {
	t.Helper()
	return cmdtest.BuildCommand(t, cmdtest.Command{
		Name: "agent-secrets",
		Dir:  filepath.Join("..", "cipher"),
		Env:  append(os.Environ(), "GOTOOLCHAIN=go1.27.3"),
	})
}

// agentSecretsEnv is the environment an agent-secrets process of a pod runs with: the test's own,
// less every AGENT_SECRETS_* variable and every agent session id (requestSessionID's), which name
// the session running the test rather than the pod, plus the broker's address and the pod's key dir.
func agentSecretsEnv(brokerURL, keyDir string) []string {
	var env []string
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		switch {
		case strings.HasPrefix(name, "AGENT_SECRETS_"), name == "OMP_SESSION_ID", name == "ENVOY_SESSION_ID", name == "CLAUDE_CODE_SESSION_ID":
			continue
		}
		env = append(env, entry)
	}
	return append(env, "AGENT_SECRETS_URL="+brokerURL, "AGENT_SECRETS_KEY_DIR="+keyDir)
}

// writeWhole writes content to path whole and renames it into place, as the worker shim writes a
// pod's enrollment file, so no reader ever sees half of it.
func writeWhole(t *testing.T, path, content string) {
	t.Helper()
	partial := path + ".partial"
	if err := os.WriteFile(partial, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(partial, path); err != nil {
		t.Fatal(err)
	}
}

// enrollKey is podBroker.enroll for a key the pod made itself: by's credential enrolls the pod
// runtimeID for the key whose thumbprint is thumbprint, proving its account with token, and
// answers the enrollment's id.
func enrollKey(b podBroker, by launcher, runtimeID, thumbprint, token string) string {
	b.t.Helper()
	signed, err := proof.SignLauncher(by.key, by.credentialID, http.MethodPost, b.base+"/v1/enrollments", time.Now())
	if err != nil {
		b.t.Fatal(err)
	}
	status, answer := b.post("/v1/enrollments", map[string]string{"Proof": signed}, map[string]any{
		"kind": "pod", "runtime_id": runtimeID, "thumbprint": thumbprint, "pod_token": token,
	})
	var enrolled struct {
		EnrollmentID string `json:"enrollment_id"`
	}
	b.decode(answer, &enrolled)
	if status != http.StatusCreated || enrolled.EnrollmentID == "" {
		b.t.Fatalf("enroll %s = %d %s, want 201 with an enrollment_id", runtimeID, status, answer)
	}
	return enrolled.EnrollmentID
}

// deleteEnrollment has by's credential end the enrollment enrollmentID at the broker at base, as a
// launcher ends a pod it lets go, and answers the status.
func deleteEnrollment(t *testing.T, base string, by launcher, enrollmentID string) int {
	t.Helper()
	path := "/v1/enrollments/" + enrollmentID
	signed, err := proof.SignLauncher(by.key, by.credentialID, http.MethodDelete, base+path, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodDelete, base+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Proof", signed)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("DELETE %s: %v", path, err)
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("> DELETE %s\n< %d %s", path, response.StatusCode, strings.TrimSpace(string(answer)))
	return response.StatusCode
}

// brokerRefusal answers the code and message of the broker's error answer, empty for an answer
// that is not one.
func brokerRefusal(answer []byte) (code, message string) {
	var refusal struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	_ = json.Unmarshal(answer, &refusal)
	return refusal.Code, refusal.Error
}

// forwarder is the one address every broker process of a test is reached at, as a Service's
// address is in production: it holds a loopback port for the whole test and passes each
// connection it accepts on to the process it routes to, and while it routes to none it closes the
// connection at once, as a Service with no ready endpoint refuses one.
type forwarder struct {
	url     string
	mu      sync.Mutex
	backend string
	closed  bool
	open    map[net.Conn]struct{}
}

// startForwarder starts a forwarder routing to no process, and stops it when t ends, closing
// every connection it holds.
func startForwarder(t *testing.T) *forwarder {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &forwarder{url: "http://" + listener.Addr().String(), open: map[net.Conn]struct{}{}}
	var passing sync.WaitGroup
	passing.Add(1)
	go func() {
		defer passing.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			passing.Add(1)
			go func() {
				defer passing.Done()
				f.pass(conn)
			}()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		f.mu.Lock()
		f.closed = true
		for conn := range f.open {
			_ = conn.Close()
		}
		f.mu.Unlock()
		passing.Wait()
	})
	return f
}

// route sends every connection accepted from now on to addr, or refuses each one when addr is "".
func (f *forwarder) route(addr string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.backend = addr
}

// pass copies client to the process the forwarder routes to and back until either side closes,
// then closes both; with no process routed to, or none answering, it closes client at once.
func (f *forwarder) pass(client net.Conn) {
	defer client.Close()
	f.mu.Lock()
	backend := f.backend
	f.mu.Unlock()
	if backend == "" || !f.hold(client) {
		return
	}
	defer f.release(client)
	upstream, err := net.DialTimeout("tcp", backend, 5*time.Second)
	if err != nil {
		return
	}
	defer upstream.Close()
	if !f.hold(upstream) {
		return
	}
	defer f.release(upstream)
	copied := make(chan struct{})
	go func() {
		defer close(copied)
		_, _ = io.Copy(upstream, client)
		_ = upstream.Close()
	}()
	_, _ = io.Copy(client, upstream)
	_ = client.Close()
	<-copied
}

// hold records conn as open, for the cleanup to close, and answers false once the forwarder has
// stopped.
func (f *forwarder) hold(conn net.Conn) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return false
	}
	f.open[conn] = struct{}{}
	return true
}

func (f *forwarder) release(conn net.Conn) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.open, conn)
}

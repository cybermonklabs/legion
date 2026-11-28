package daemon

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/envoy/testnatscallout"

	"github.com/sjawhar/legion/daemon/internal/natsauth"
	"github.com/sjawhar/legion/daemon/internal/testnkeys"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// tokenAccount is the ServiceAccount whose projected token the daemon presents in these tests;
// tokenServices is the hub's BROKER_NATS_SERVICES, that account under a daemon/-/* entry, which the
// auth service answers with natsgrant.Daemon() whole; and tokenUser is the user the service admits
// the daemon's connection as (its decision's name), which /connz?auth=true reports as that
// connection's authorized user. internal/natsauth's tests of the connection itself
// (service_test.go) name the same three.
const (
	tokenAccount  = "system:serviceaccount:legion-example:legion-daemon"
	tokenServices = "daemon/-/*=" + tokenAccount
	tokenUser     = "daemon/" + tokenAccount
)

// readServiceToken is these tests' verifier of the daemon's token in the broker's place
// (testnatscallout.Verifier): the account, issue and expiry the unsigned token names, read
// unverified, as natscred's rig reads a platform token (natscred/rig_test.go, readPlatformToken)
// and as internal/natsauth's tests read it, which this package cannot import.
func readServiceToken(_ context.Context, raw string) (string, bool, time.Time, time.Time, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return "", false, time.Time{}, time.Time{}, errors.New("no JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", false, time.Time{}, time.Time{}, err
	}
	var claims struct {
		Sub      string `json:"sub"`
		Iat, Exp int64
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", false, time.Time{}, time.Time{}, fmt.Errorf("claims: %w", err)
	}
	return claims.Sub, false, time.Unix(claims.Iat, 0), time.Unix(claims.Exp, 0), nil
}

// refusingVerifier is readServiceToken behind a count of refusals still to make: it refuses that
// many of the tokens it is given next, as the broker refuses a token while it cannot read the key
// set of the token's issuer, and reads every other one. The auth service asks it on goroutines of
// its own.
type refusingVerifier struct {
	mu     sync.Mutex
	refuse int
}

// refuseNext makes the verifier refuse the next n tokens it is given.
func (v *refusingVerifier) refuseNext(n int) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.refuse = n
}

func (v *refusingVerifier) verify(ctx context.Context, raw string) (string, bool, time.Time, time.Time, error) {
	v.mu.Lock()
	refuse := v.refuse > 0
	if refuse {
		v.refuse--
	}
	v.mu.Unlock()
	if refuse {
		return "", false, time.Time{}, time.Time{}, errors.New("the key set of the token's issuer could not be read")
	}
	return readServiceToken(ctx, raw)
}

// writeToken writes token to path as the kubelet writes a projected token: whole, to a file of its
// own beside path, renamed over the one before, so no reader reads a part of it. The mode is a
// projected token's, 0644, which nothing that reads the token holds against it.
func writeToken(t *testing.T, path, token string) {
	t.Helper()
	next := path + ".next"
	if err := os.WriteFile(next, []byte(token+"\n"), 0o644); err != nil {
		t.Fatalf("write the token beside its file: %v", err)
	}
	if err := os.Rename(next, path); err != nil {
		t.Fatalf("rename the token over its file: %v", err)
	}
}

// tokenFile is a file of its own under t's temporary directory holding token, written as
// writeToken writes it.
func tokenFile(t *testing.T, token string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	writeToken(t, path, token)
	return path
}

// tokenHub is a hub whose clients the real NATS auth service admits (testnatscallout.Start),
// listing the daemon's account (tokenServices) and verifying each token with verify, with
// ENVOY_NOTIFICATIONS created by its admin as the daemon's grant tests create it
// (createDaemonNotificationStream), and the admin's connection, which the hub admits by its own
// configuration.
func tokenHub(t *testing.T, verify testnatscallout.Verifier) (*testnatscallout.Hub, *nats.Conn, jetstream.Stream) {
	t.Helper()
	hub := testnatscallout.Start(t, tokenServices, verify)
	admin := hub.Admin(t)
	js, err := jetstream.New(admin)
	if err != nil {
		t.Fatalf("open JetStream as the hub's admin: %v", err)
	}
	return hub, admin, createDaemonNotificationStream(t, js)
}

// hubConnection is one client connection as the hub's monitoring endpoint reports it with
// auth=true: the user it was authorized as, which for a client the auth service admitted is the
// name its decision gave (daemon/<account> for a daemon entry), and for a static user its public
// key (testnatscallout.Hub.StaticKeys).
type hubConnection struct {
	CID            uint64 `json:"cid"`
	Name           string `json:"name"`
	AuthorizedUser string `json:"authorized_user"`
}

// hubConnections is every client connection the hub's monitoring endpoint lists at /connz?query.
func hubConnections(t *testing.T, hub *testnatscallout.Hub, query string) []hubConnection {
	t.Helper()
	monitor := hub.MonitorURL()
	if monitor == "" {
		t.Fatal("the hub's server is stopped: it has no monitoring endpoint to read")
	}
	response, err := http.Get(monitor + "/connz?" + query)
	if err != nil {
		t.Fatalf("read the hub's /connz?%s: %v", query, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("the hub's /connz?%s answered %s", query, response.Status)
	}
	var connz struct {
		Connections []hubConnection `json:"connections"`
	}
	if err := json.NewDecoder(response.Body).Decode(&connz); err != nil {
		t.Fatalf("decode the hub's /connz?%s: %v", query, err)
	}
	return connz.Connections
}

// decidedConnections is each connection the hub lists at /connz?auth=true&state=all, open or
// closed, but its two static users' (testnatscallout.Hub.StaticKeys: the admin's and the auth
// service's own): every connection the auth service decided, by the client id the server gave it.
func decidedConnections(t *testing.T, hub *testnatscallout.Hub) map[uint64]hubConnection {
	t.Helper()
	static := hub.StaticKeys()
	decided := map[uint64]hubConnection{}
	for _, each := range hubConnections(t, hub, "auth=true&state=all") {
		if !slices.Contains(static, each.AuthorizedUser) {
			decided[each.CID] = each
		}
	}
	return decided
}

// linesNaming is each line of text that holds every one of words, case aside.
func linesNaming(text string, words ...string) []string {
	var lines []string
	for line := range strings.SplitSeq(text, "\n") {
		lower := strings.ToLower(line)
		if !slices.ContainsFunc(words, func(word string) bool { return !strings.Contains(lower, strings.ToLower(word)) }) {
			lines = append(lines, line)
		}
	}
	return lines
}

// tokenPermissionRefusals is every line in which the daemon's log (natsauth.LogEvents) or the
// hub's server log names a permission the server refused the daemon's connection, the subject
// among them, joined: "" when the grant refused nothing.
func tokenPermissionRefusals(logs *syncBuffer, hub *testnatscallout.Hub) string {
	lines := linesNaming(logs.String(), "NATS refused the daemon a permission")
	lines = append(lines, linesNaming(hub.ServerLog(), "Publish Violation")...)
	lines = append(lines, linesNaming(hub.ServerLog(), "Subscription Violation")...)
	return strings.Join(lines, "\n")
}

// requireLogsNoToken fails t when the daemon's log holds token or its claims, the part between its
// dots that names the account and its lapse: the daemon logs the account it connects as, never the
// credential that proves it.
func requireLogsNoToken(t *testing.T, logged, token string) {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("the test's token has %d parts, want a JWT's three", len(parts))
	}
	for name, credential := range map[string]string{"the service token": token, "the service token's claims": parts[1]} {
		if strings.Contains(logged, credential) {
			t.Errorf("the daemon's log holds %s:\n%s", name, logged)
		}
	}
}

// requirePresentedTheFilesToken fails t unless the auth service was given a token (given) at least
// once and every one it was given is token, the one the daemon's file holds: each CONNECT
// presented the file's token, and none presented another credential in its place.
func requirePresentedTheFilesToken(t *testing.T, given []string, token string) {
	t.Helper()
	if len(given) == 0 {
		t.Fatal("the auth service was given no token: no CONNECT presented the daemon's service token")
	}
	for i, each := range given {
		if each != token {
			t.Errorf("CONNECT %d of %d presented a token the file never held", i+1, len(given))
		}
	}
}

// The workflow's own NATS connection presents the daemon's service token through the real NATS
// auth service and does every NATS call the workflow's connect makes under the grant the service
// answers, natsgrant.Daemon() under a daemon/-/* entry (the shape of
// TestDaemonPermissionsOnRealServers' connectDaemonGrant, on testnatscallout's hub rather than a
// server whose static user holds the grant). The credential nats_daemon_service_token_file names
// (natsauth.DaemonCredential) is the connection's (chooseNATSConnection), and workflow.connect
// dials it through natsauth.ConnectService: the auth service is given the file's token at every
// CONNECT and admits it as daemon/<account>, under the name the daemon's connection carries
// (natsauth.ConnectionName). On that connection the connect creates the project's three durable
// consumers, subscribeNoticeExceptions receives the admin's role-lane exception, and neither the
// daemon's log nor the server's names a refused permission.
//
// The control is a connection with no credential (natsConnection{}): the hub sends it to the auth
// service too, which refuses it as no_credential, and workflow.connect fails with the
// authorization violation, as it does today.
func TestTheWorkflowConnectsThroughTheAuthServiceByItsServiceToken(t *testing.T) {
	t.Parallel()
	hub, admin, stream := tokenHub(t, readServiceToken)
	token := testnkeys.ServiceToken(tokenAccount, time.Now().Add(time.Hour))
	cfg := daemonGrantConfig(hub.URL())
	credential, err := natsauth.DaemonCredential("", tokenFile(t, token), cfg.NatsURLs, "", lookup(nil))
	if err != nil {
		t.Fatalf("DaemonCredential of nats_daemon_service_token_file: %v", err)
	}
	nc, err := chooseNATSConnection(credential, "")
	if err != nil {
		t.Fatalf("chooseNATSConnection of the service token: %v", err)
	}
	logs := &syncBuffer{}
	w := grantWorkflow(t, logs)
	if err := w.connect(t.Context(), cfg, nc); err != nil {
		t.Fatalf("connect the daemon through the auth service: %v\nthe auth service's log:\n%s", err, hub.ServiceLog())
	}
	defer w.closeNATS()
	conn := w.conn.Load()

	requirePresentedTheFilesToken(t, hub.Tokens(), token)
	cid, err := conn.GetClientID()
	if err != nil {
		t.Fatalf("the workflow connection's client id: %v", err)
	}
	name := natsauth.ConnectionName("legion-daemon "+cfg.Project, cfg.Revision)
	if listed := hubConnections(t, hub, "auth=true&cid="+strconv.FormatUint(cid, 10)); len(listed) != 1 || listed[0].AuthorizedUser != tokenUser || listed[0].Name != name {
		t.Fatalf("the hub lists the workflow connection as %+v, want one connection named %q authorized as %s", listed, name, tokenUser)
	}

	for _, consumer := range []string{dispatchGrantConsumer, githubGrantConsumer, wakeGrantConsumer} {
		if _, err := stream.Consumer(t.Context(), consumer); err != nil {
			t.Fatalf("the workflow's connect through the auth service created no %s: %v\n%s", consumer, err, tokenPermissionRefusals(logs, hub))
		}
	}

	exception := make(chan []byte, 1)
	subscription, err := subscribeNoticeExceptions(conn, func(data []byte) {
		select {
		case exception <- append([]byte(nil), data...):
		default:
		}
	})
	if err != nil {
		t.Fatalf("subscribe to role-lane exceptions: %v", err)
	}
	defer subscription.Unsubscribe()
	if err := conn.Flush(); err != nil {
		t.Fatalf("flush the exceptions subscription: %v", err)
	}
	if err := admin.Publish("notifications.envoy.exceptions.notifications.role.example", []byte("retry this notice")); err != nil {
		t.Fatalf("publish the role-lane exception as the admin: %v", err)
	}
	if err := admin.Flush(); err != nil {
		t.Fatalf("flush the role-lane exception: %v", err)
	}
	var got []byte
	testwait.Eventually(t, "the daemon receiving the role-lane exception, or a refusal of its subscription", func() bool {
		select {
		case got = <-exception:
			return true
		default:
			return tokenPermissionRefusals(logs, hub) != ""
		}
	})
	if got == nil {
		t.Fatalf("the auth service's grant refused the daemon's role-lane exceptions:\n%s", tokenPermissionRefusals(logs, hub))
	}
	if string(got) != "retry this notice" {
		t.Fatalf("the role-lane exception = %q, want %q", got, "retry this notice")
	}
	if refusals := tokenPermissionRefusals(logs, hub); refusals != "" {
		t.Fatalf("the daemon's connection was refused a permission:\n%s", refusals)
	}
	requireLogsNoToken(t, logs.String(), token)

	t.Run("control: no credential", func(t *testing.T) {
		control := grantWorkflow(t, &syncBuffer{})
		attempt, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		err := control.connect(attempt, cfg, natsConnection{})
		if err == nil {
			control.closeNATS()
			t.Fatal("the hub admitted the workflow's connection with no credential")
		}
		if !errors.Is(err, nats.ErrAuthorization) {
			t.Fatalf("the connect with no credential failed with %v, want the authorization violation", err)
		}
		testwait.Eventually(t, "the auth service counting a no_credential refusal", func() bool {
			return hub.Refused(t, "no_credential") >= 1
		})
	})
}

// A daemon booted with nats_daemon_service_token_file presents its service token on every NATS
// connection it opens: the class "every NATS connection the daemon opens" (R1) at the booted
// daemon, whose one dial is workflow.connect's, made by the boot's "connect Envoy NATS" probe. The
// daemon boots against a hub whose clients the real NATS auth service admits and serves; the first
// token the service was given is the one the file holds, and so is every other; the boot logs once
// that it connects as the account the token names, as the token's user and not the pane user, from
// the service token (`user=<account> paneUser=false seed=service-token`), and no line of its log
// holds the token. Every connection the hub lists at /connz?auth=true&state=all, open or closed,
// but its two static users' (the admin, and the auth service's own) is authorized as
// daemon/<account>, one for each CONNECT that presented the token, and the service refused none as
// no_credential: no connection of the daemon's presented no credential, or a seed in the token's
// place.
func TestABootedDaemonPresentsItsServiceTokenOnEveryConnectionThroughTheAuthService(t *testing.T) {
	t.Parallel()
	calls := quickAppMints()
	hub, _, _ := tokenHub(t, readServiceToken)
	token := testnkeys.ServiceToken(tokenAccount, time.Now().Add(time.Hour))
	cfg := workflowConfig(t, hub.URL())
	cfg.NatsDaemonServiceTokenFile = tokenFile(t, token)
	var out syncBuffer
	tokens, _ := appTokens(t, func(w http.ResponseWriter, _ *http.Request, _ string, _ int) { installed(w) })
	if err := bootsOn(t, cfg, tokens, calls, 60*time.Second, slog.New(slog.NewTextHandler(&out, nil))); err != nil {
		t.Fatalf("boot with the service token = %v, want the daemon serving\nthe auth service's log:\n%s", err, hub.ServiceLog())
	}

	given := hub.Tokens()
	if len(given) == 0 || given[0] != token {
		t.Fatalf("the auth service was given %d tokens, the first of them not the file's; want the token the file holds first", len(given))
	}
	requirePresentedTheFilesToken(t, given, token)
	logged := out.String()
	want := `level=INFO msg="legion daemon connects to NATS" user=` + tokenAccount + ` paneUser=false seed=service-token`
	if strings.Count(logged, "legion daemon connects to NATS") != 1 || !strings.Contains(logged, want) {
		t.Errorf("log =\n%s\nwant exactly one line containing %s", logged, want)
	}
	requireLogsNoToken(t, logged, token)

	if refused := hub.Refused(t, "no_credential"); refused != 0 {
		t.Errorf("the auth service refused %d connections as no_credential, want none\n%s", refused, hub.ServiceLog())
	}
	decided := decidedConnections(t, hub)
	if len(decided) != len(given) {
		t.Errorf("the hub lists %d connections the auth service decided, open or closed, and the service was given %d tokens: one connection for each CONNECT that presented one",
			len(decided), len(given))
	}
	for _, each := range decided {
		if each.AuthorizedUser != tokenUser {
			t.Errorf("the hub lists connection %d %q authorized as %q; every connection but the hub's two static users' should be the daemon's, authorized as %s",
				each.CID, each.Name, each.AuthorizedUser, tokenUser)
		}
	}
}

// The boot waits out the NATS auth service's refusal of the daemon's service token (R4, at its
// start), on the boot's own wait: the "connect Envoy NATS" probe gives bootprobe.Run the decision
// of the credential the boot resolved (natsauth.Credential.WaitsOut) in place of
// natsauth.Unreachable, which reads an authorization violation as a misconfiguration and stops the
// boot. The service refuses a token while it cannot read the key set of the token's issuer, and
// the server answers that refusal as every other, an authorization violation. Here the verifier
// refuses the first three CONNECTs: the boot logs each refused attempt with the refusal, waits
// readinessRetry (one second, two, four; the package's own, which this test leaves as it is) and
// tries again, and /readyz answers no 200 before the fourth CONNECT has been presented, since the
// daemon serves nothing while its boot runs; then the same boot, with no exit and no restart,
// serves once the service admits the token. The control is no subtest but the R4 negative control
// (a change never pushed): with the probe given natsauth.Unreachable alone, this boot exits at
// the first refusal naming the authorization violation, and the test fails there.
func TestTheBootWaitsOutARefusalOfItsServiceTokenThroughTheAuthService(t *testing.T) {
	t.Parallel()
	calls := quickAppMints()
	verifier := &refusingVerifier{}
	verifier.refuseNext(3)
	hub, _, _ := tokenHub(t, verifier.verify)
	token := testnkeys.ServiceToken(tokenAccount, time.Now().Add(time.Hour))
	cfg := workflowConfig(t, hub.URL())
	cfg.NatsDaemonServiceTokenFile = tokenFile(t, token)
	var out syncBuffer
	tokens, _ := appTokens(t, func(w http.ResponseWriter, _ *http.Request, _ string, _ int) { installed(w) })

	// While the token is refused the daemon is not ready: /readyz answers nothing, or 503, until
	// the fourth CONNECT, the admitted one, has been presented. The count is read after each
	// answer, so a 200 read with fewer than four presented is a daemon ready before it was
	// admitted. The poll stops, and is waited for, before its finding is read.
	readyEarly, stop, polled := make(chan int, 1), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(polled)
		for len(hub.Tokens()) < 4 {
			if response, err := pollClient.Get("http://127.0.0.1:" + strconv.Itoa(cfg.Port) + "/readyz"); err == nil {
				response.Body.Close()
				if presented := len(hub.Tokens()); response.StatusCode == http.StatusOK && presented < 4 {
					readyEarly <- presented
					return
				}
			}
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
	}()
	var once sync.Once
	stopPolling := func() { once.Do(func() { close(stop); <-polled }) }
	defer stopPolling()

	err := bootsOn(t, cfg, tokens, calls, 60*time.Second, slog.New(slog.NewTextHandler(&out, nil)))
	stopPolling()
	if err != nil {
		t.Fatalf("boot whose token the auth service refuses three times = %v, want the daemon serving once it is admitted\n%s", err, out.String())
	}
	select {
	case presented := <-readyEarly:
		t.Fatalf("/readyz answered 200 with %d CONNECTs presented, before the token was admitted", presented)
	default:
	}
	given := hub.Tokens()
	if len(given) < 4 {
		t.Fatalf("the daemon presented its token %d times, want at least four: three refused, then admitted", len(given))
	}
	requirePresentedTheFilesToken(t, given, token)
	if refused := hub.Refused(t, "service_token_not_verified"); refused != 3 {
		t.Errorf("the auth service refused the token %d times, want the verifier's three\n%s", refused, hub.ServiceLog())
	}
	if waited := linesNaming(out.String(), "boot probe failed transiently", "connect Envoy NATS", "authorization violation"); len(waited) != 3 {
		t.Errorf("the boot logged %d refused attempts of its NATS connect naming the refusal, want 3\n%s", len(waited), out.String())
	}
	requireLogsNoToken(t, out.String(), token)
}

// `legion config check`'s reading of what boot reads (CheckStart) names whom the daemon's own NATS
// credential authenticates as, as the boot line does, and never the credential: for the service
// token, by nats_daemon_service_token_file or by NATS_DAEMON_SERVICE_TOKEN_FILE, the account the
// token names, beside the pane seed's user, which the pane seed's settings still give beside the
// token; for a daemon seed, that seed's user by public key, beside the pane seed's user, as before.
// The configuration has no project mapped and runs on tmux with LEGION_OMP_PATH naming an
// executable, so it gets past every other read.
func TestCheckStartNamesTheAccountOfTheDaemonsServiceToken(t *testing.T) {
	// Not parallel: it calls t.Setenv (LEGION_OMP_PATH), since CheckStart resolves Oh My Pi from the process's own environment (os.Getenv), as `legion config check` does.
	omp, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("LEGION_OMP_PATH", omp)
	paneSeed, paneUser := testnkeys.User(t)
	daemonSeed, daemonUser := testnkeys.User(t)
	token := testnkeys.ServiceToken(tokenAccount, time.Now().Add(time.Hour))
	file := tokenFile(t, token)
	for _, tc := range []struct {
		name                  string
		tokenFile, daemonSeed string
		env                   map[string]string
		want                  CheckedNATS
	}{
		{name: "the service token by its key", tokenFile: file,
			want: CheckedNATS{PaneUser: paneUser, DaemonServiceAccount: tokenAccount}},
		{name: "the service token by its variable", env: map[string]string{"NATS_DAEMON_SERVICE_TOKEN_FILE": file},
			want: CheckedNATS{PaneUser: paneUser, DaemonServiceAccount: tokenAccount}},
		{name: "a daemon seed", daemonSeed: daemonSeed,
			want: CheckedNATS{PaneUser: paneUser, DaemonUser: daemonUser}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig(t)
			cfg.NatsNkeySeedFile = testnkeys.SeedFile(t, paneSeed+"\n")
			cfg.NatsDaemonServiceTokenFile = tc.tokenFile
			if tc.daemonSeed != "" {
				cfg.NatsDaemonNkeySeedFile = testnkeys.SeedFile(t, tc.daemonSeed+"\n")
			}

			checked, err := CheckStart(cfg, lookup(tc.env))

			printed := fmt.Sprintf("%+v", checked)
			for name, credential := range map[string]string{"the service token": token, "the daemon seed": daemonSeed, "the pane seed": paneSeed} {
				if strings.Contains(printed, credential) {
					t.Fatalf("CheckStart answered %s", name)
				}
			}
			if err != nil || checked != tc.want {
				t.Fatalf("CheckStart = %+v, %v; want %+v", checked, err, tc.want)
			}
		})
	}
}

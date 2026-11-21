//go:build natslive

package gcp_test

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/gcp"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
	"github.com/sjawhar/envoy/internal/testnats"
)

// The tests in this file run the listener binary of this checkout in the environment
// gcp:ClusteredNatsHub renders for its listener pod, so they carry the natslive tag. The unit
// tests prove that rendering (clusteredhub_listener_test.go's
// TestClusteredNatsHubListenerEnvironment and the pod's golden
// testdata/clustered-hub-listener-pod.golden); these prove the binary behaves as the rendering
// assumes it does: a tailnet.listener with no clientNamespaces starts bound off loopback because
// its broker settings are a credential it verifies, and refuses every call that presents none
// (t-e-19); a listener that pods of the cluster reach refuses every call that presents no token of a
// listed subject; and a second pod of the same machine id is ready while the first holds the
// listener's durable and binds it once the first stops, which is the whole of a rolling update of
// that pod.
//
// They need no container runtime: the one server they run is the nats-server this binary links
// in, started in the test process by testnats.Start and shut down with the test. Its version is
// not what these tests read — they read the listener's own behaviour against a bus with
// JetStream — and the versions the hub actually deploys are TestClusteredNatsHubChart's and
// TestClusteredNatsHubCluster's to prove. The listener is built from packages/envoy/cmd/listener
// under the repository's root go.work, as core/permissions_live_test.go's buildEnvoyCommand
// builds it, and a build that fails fails the test the same way.
const (
	// clusteredHubListenerLiveHost is the one name ENVOY_ALLOWED_HOSTS carries here: the tailnet
	// address the component renders from its hostname and tailnetDnsName, the address its clients
	// reach the listener by.
	clusteredHubListenerLiveHost = "envoy-hub.example-tailnet.ts.net"

	// clusteredHubListenerOtherHost is a Host this listener was never told, the one a page that
	// rebound its own name to the pod's address would carry.
	clusteredHubListenerOtherHost = "envoy.example.invalid"

	// clusteredHubListenerHealthWindow is how long a listener gets to reach "healthy" on a bus
	// whose durable nobody holds: it warms its caches first, up to 30 s apiece
	// (cmd/listener/main.go).
	clusteredHubListenerHealthWindow = 2 * time.Minute

	// clusteredHubListenerBindDeadline is cmd/listener/durable.go's durableBindDeadline, where a
	// listener that still cannot bind the durable ends its start, and so the time the hand-over
	// below has to land inside.
	clusteredHubListenerBindDeadline = 135 * time.Second

	// clusteredHubListenerHandoverWindow is the window the hand-over is measured in. It is longer
	// than the deadline on purpose, so a hand-over that took too long is read from the assertion
	// that measured it rather than from a timeout that measured nothing.
	clusteredHubListenerHandoverWindow = 150 * time.Second
)

// liveListenerClient reads the listener's HTTP answers. Every read is one answer counted by its
// caller, so no assertion here passes on an empty set.
var liveListenerClient = &http.Client{Timeout: 10 * time.Second}

// buildLiveListener builds the listener of this checkout, as core/permissions_live_test.go's
// buildEnvoyCommand does; gcp and core are the same two directories below packages/envoy.
func buildLiveListener(t *testing.T) string {
	t.Helper()
	return cmdtest.BuildCommand(t, cmdtest.Command{Name: "envoy-listener", Dir: filepath.Join("..", "..", "cmd", "listener")})
}

// liveListenerEnv is the whole environment the component renders for the listener container of
// args, as a process on this machine takes it: read from the rendered Deployment, with PORT,
// NATS_URLS (the test's server) and ENVOY_AGENT_SECRETS_ACCOUNT_TOKEN_FILE replaced, the last by a
// file of the test's holding a placeholder token, for the projected token the kubelet would mount
// (the listener reads it only when it introspects a token, so no broker is called here);
// ENVOY_OIDC_ISSUER replaced by issuer when issuer is set; NATS_NKEY_SEED_FILE left out, since this
// server carries no authorization; and each of drop left out, which is a control's doing. It is the
// process's whole environment, so there is no ENVOY_API_TOKEN, no ENVOY_API_ALLOW_UNAUTHENTICATED
// and no ENVOY_ALLOWED_HOSTS in it, which is what the pod renders too.
func liveListenerEnv(t *testing.T, args *gcp.ClusteredNatsHubArgs, port int, natsURL, issuer string, drop ...string) []string {
	t.Helper()
	tokenFile := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(tokenFile, []byte("placeholder-projected-token\n"), 0o600))
	rendered := containerEnv(t, podContainer(t, deploymentPodTemplate(t, runClusteredHub(t, args), clusteredListenerName), "listener"))
	require.NotEmpty(t, rendered["ENVOY_AGENT_SECRETS_URL"], "the rendered listener carries no broker settings")
	var env []string
	for name, value := range rendered {
		text, ok := value.(string)
		require.True(t, ok, "%s is rendered from a Secret, which this machine cannot read", name)
		switch {
		case slices.Contains(drop, name), name == "NATS_NKEY_SEED_FILE":
			continue
		case name == "PORT":
			text = strconv.Itoa(port)
		case name == "NATS_URLS":
			text = natsURL
		case name == "ENVOY_AGENT_SECRETS_ACCOUNT_TOKEN_FILE":
			text = tokenFile
		case name == "ENVOY_OIDC_ISSUER" && issuer != "":
			text = issuer
		}
		env = append(env, name+"="+text)
	}
	return env
}

// liveBrokerSettings are the variables of a listener's broker check, which a control drops to show
// they are the credential that lets a tailnet listener start off loopback.
var liveBrokerSettings = []string{
	"ENVOY_AGENT_SECRETS_URL", "ENVOY_AGENT_SECRETS_ACCOUNT_TOKEN_FILE", "ENVOY_PUBLIC_URL",
	"ENVOY_AGENT_SECRETS_INTROSPECTIONS_PER_SECOND", "ENVOY_AGENT_SECRETS_INTROSPECTION_BURST",
}

// liveListenerRefusal is the line the listener's own API check refuses a start off loopback with
// when it takes no credential it verifies (cmd/listener/apiauth.go, validateListenerAPIAuth).
const liveListenerRefusal = "off loopback /v1 serves only a caller that presents a credential this listener verifies"

// startLiveListener starts a listener of the tailnet listener pod's environment
// (clusteredArgs' tailnet.listener) on a port reservePorts reserved and waits for its /healthz to
// answer 200, so it returns a process that is serving. It returns the process and the port it
// serves on. The listener binds ENVOY_LISTEN_HOST=0.0.0.0 as the pod does, so it binds the port
// itself rather than inherit a cmdtest.HoldPort socket, which listens on loopback alone.
func startLiveListener(t *testing.T, name, binary, natsURL string) (*cmdtest.Process, int) {
	t.Helper()
	port := reservePorts(t, 1)[0]
	cmd := exec.Command(binary)
	cmd.Env = liveListenerEnv(t, clusteredArgs(), port, natsURL, "")
	process := startReady(t, name, cmd, func(*cmdtest.Process) error {
		status, body, err := liveListenerHealthz(port)
		if err != nil {
			return err
		}
		if status != http.StatusOK {
			return fmt.Errorf("/healthz answered %d: %s", status, body)
		}
		return nil
	})
	return process, port
}

// liveListenerHealthz is the status and body /healthz answers on loopback at port. It is a copy of
// core/permissions_live_test.go's healthz, which belongs to another package.
func liveListenerHealthz(port int) (int, string, error) {
	resp, err := liveListenerClient.Get("http://127.0.0.1:" + strconv.Itoa(port) + "/healthz")
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body), err
}

// awaitListenerStatus waits up to window for /healthz to answer 200 with a body holding want, and
// returns how long that took, the body and how many answers it read on the way. A caller asserts
// that count, so a wait that read nothing at all cannot stand for a listener that answered.
func awaitListenerStatus(t *testing.T, process *cmdtest.Process, port int, want string, window time.Duration) (time.Duration, string, int) {
	t.Helper()
	var body string
	answers := 0
	waited := within(t, window, fmt.Sprintf("the listener's /healthz answers %s", want), func() error {
		select {
		case <-process.Exited:
			return fmt.Errorf("the listener exited before /healthz answered %s:\n%s", want, process.Output.String())
		default:
		}
		status, read, err := liveListenerHealthz(port)
		if err != nil {
			return err
		}
		answers++
		if status != http.StatusOK {
			return fmt.Errorf("/healthz answered %d: %s", status, read)
		}
		if !strings.Contains(read, want) {
			return fmt.Errorf("/healthz answered %s", read)
		}
		body = read
		return nil
	})
	return waited, body, answers
}

// liveListenerAnswer is the status and body the listener at addr answers a GET of path carrying
// host as its Host and no Authorization header at all. The request is built with http.NewRequest
// and its Host set, since Go otherwise sends the dialled address as the Host and the fence under
// test would never see the name.
func liveListenerAnswer(t *testing.T, addr, path, host string) (int, string) {
	t.Helper()
	return liveListenerAnswerAs(t, addr, path, host, "")
}

// liveListenerAnswerAs is liveListenerAnswer presenting token as a bearer, or no Authorization
// header when token is empty.
func liveListenerAnswerAs(t *testing.T, addr, path, host, token string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "http://"+addr+path, nil)
	require.NoError(t, err)
	req.Host = host
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := liveListenerClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(body)
}

// liveListenerLogLines is how many lines of the process's output hold text, counted so that an
// assertion over a log fails on a log that holds no such line rather than passing on an empty
// read.
func liveListenerLogLines(process *cmdtest.Process, text string) int {
	lines := 0
	for _, line := range strings.Split(process.Output.String(), "\n") {
		if strings.Contains(line, text) {
			lines++
		}
	}
	return lines
}

// nonLoopbackIPv4 is an IPv4 address of this machine that is not loopback, which stands for the
// pod's own address: the listener binds every interface, and only an address that is not loopback
// shows that a request reaching it from outside the pod is served on the Host fence alone. A
// machine with no such address fails the test naming the problem, since a skip here would leave
// the binding the component renders untested.
func nonLoopbackIPv4(t *testing.T) string {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	require.NoError(t, err)
	examined := 0
	for _, addr := range addrs {
		ipnet, ok := addr.(*net.IPNet)
		if !ok {
			continue
		}
		ip := ipnet.IP.To4()
		if ip == nil {
			continue
		}
		examined++
		if ip.IsLoopback() {
			continue
		}
		return ip.String()
	}
	t.Fatalf("none of the %d IPv4 interface addresses of this machine is off loopback, so nothing here can reach a listener the way a pod's clients do: %v", examined, addrs)
	return ""
}

// The tailnet listener pod gcp:ClusteredNatsHub renders starts on this machine because its broker
// settings are a credential the listener verifies: the listener's own API check refuses a listener
// bound off loopback that takes none (cmd/listener/apiauth.go, validateListenerAPIAuth), so the same
// environment without the broker settings exits non-zero naming the rule and serves nothing at all.
// The listener that did start then refuses GET /v1/sessions over this machine's own non-loopback
// address 401 when the call presents no credential, whatever Host it carries, the tailnet name the
// component renders included: there is no open /v1 and no Host fence left (t-e-19). What a session
// presents is a broker token under DPoP, which cmd/listener's own tests prove; this proves the
// rendered pod admits nothing else.
func TestTheTailnetSessionListenerRefusesACallWithoutABrokerToken(t *testing.T) {
	binary := buildLiveListener(t)
	_, natsURL := testnats.Start(t)

	listeners := 0
	process, port := startLiveListener(t, "the tailnet listener", binary, natsURL)
	listeners++

	answers := 0
	status, body, err := liveListenerHealthz(port)
	require.NoError(t, err)
	answers++
	require.Equal(t, http.StatusOK, status, "/healthz answered %d: %s\n%s", status, body, process.Output.String())
	t.Logf("the listener's first /healthz: %d %s", status, strings.TrimSpace(body))
	hosts := "listener API hosts: any (a credential is required)"
	require.NotZero(t, liveListenerLogLines(process, hosts),
		"the listener logged no %q line:\n%s", hosts, process.Output.String())

	// Nothing of /v1 is read from a listener still answering "starting": the read routes answer
	// 503 until the caches behind them have filled, and a refusal then would be no credential's.
	waited, healthy, reads := awaitListenerStatus(t, process, port, `"status":"healthy"`, clusteredHubListenerHealthWindow)
	answers += reads
	require.NotZero(t, reads, "no /healthz answer was read while waiting for the listener to hold its durable")
	t.Logf("the listener held its durable after %.1fs: %s", waited.Seconds(), strings.TrimSpace(healthy))

	addr := net.JoinHostPort(nonLoopbackIPv4(t), strconv.Itoa(port))
	for _, host := range []string{clusteredHubListenerLiveHost, clusteredHubListenerOtherHost} {
		status, body := liveListenerAnswer(t, addr, "/v1/sessions", host)
		answers++
		require.Equal(t, http.StatusUnauthorized, status,
			"GET /v1/sessions at %s as %s with no credential answered %d: %s\n%s", addr, host, status, body, process.Output.String())
		t.Logf("GET /v1/sessions at %s as %s with no credential: %d %s", addr, host, status, strings.TrimSpace(body))
	}

	// The negative control. The same environment with the broker settings removed, on a port
	// reserved for it (reservePorts), so no other program is listening there: the API check runs
	// before the socket is bound (cmd/listener/main.go), so nothing listens on the port throughout
	// and a listener that served would answer on it.
	controlPort := reservePorts(t, 1)[0]
	control := exec.Command(binary)
	control.Env = liveListenerEnv(t, clusteredArgs(), controlPort, natsURL, "", liveBrokerSettings...)
	refused := startLogged(t, "the tailnet listener without its broker settings", control)
	listeners++
	refused.WaitExit(t, "its own API check")
	require.NotNil(t, refused.Cmd.ProcessState, "the refused listener left no exit status")
	require.NotZero(t, refused.Cmd.ProcessState.ExitCode(),
		"the listener started off loopback with no credential:\n%s", refused.Output.String())
	named := liveListenerLogLines(refused, liveListenerRefusal)
	require.NotZero(t, named, "the refusal does not name the rule:\n%s", refused.Output.String())
	t.Logf("the refusal exited %d and named the rule on %d line(s): %s",
		refused.Cmd.ProcessState.ExitCode(), named, strings.TrimSpace(refused.Output.String()))
	_, _, err = liveListenerHealthz(controlPort)
	require.Error(t, err, "something answered /healthz on the refused listener's port %d", controlPort)

	t.Logf("%d listener process(es) run, %d HTTP answer(s) read", listeners, answers)
}

// A listener that requires a credential — here the hub's own listener input with no tailnet,
// which the pods of a client namespace reach — runs on this machine in the very environment the
// component renders for its container (liveListenerEnv), with ENVOY_OIDC_ISSUER replaced by a local
// issuer standing in for the cluster's, which mints the projected tokens. Over this machine's own
// non-loopback address, as a pod of the cluster reaches it, GET /v1/sessions with no Authorization
// header is refused 401, so is one presenting a token of a subject ENVOY_OIDC_ACCOUNTS does not
// list, and one presenting a listed subject's token is served 200.
//
// The control is the same environment with its credentials taken out, the three ENVOY_OIDC_*
// variables and the broker settings: the listener then refuses to start, naming the rule, which
// shows the rendered environment carries no open flag and that its credentials are what let it bind
// off loopback at all.
func TestClusteredNatsHubListenerRefusesAnInClusterCallWithoutAToken(t *testing.T) {
	binary := buildLiveListener(t)
	_, natsURL := testnats.Start(t)
	issuer := oidctest.New(t)
	key := issuer.PublishKey(t, "clustered-hub-listener")

	args := clusteredArgs()
	standaloneListener(args)
	rendered := containerEnv(t, podContainer(t, deploymentPodTemplate(t, runClusteredHub(t, args), clusteredListenerName), "listener"))
	require.Empty(t, listenerCredentialProblem(rendered, clusteredListenerAccount))
	require.Equal(t, clusteredListenerIssuer, rendered["ENVOY_OIDC_ISSUER"])

	envFor := func(port int, drop ...string) []string {
		return liveListenerEnv(t, args, port, natsURL, issuer.URL(), drop...)
	}

	answers, listeners := 0, 0
	port := reservePorts(t, 1)[0]
	cmd := exec.Command(binary)
	cmd.Env = envFor(port)
	process := startReady(t, "the listener that requires a token", cmd, func(*cmdtest.Process) error {
		status, body, err := liveListenerHealthz(port)
		if err != nil {
			return err
		}
		if status != http.StatusOK {
			return fmt.Errorf("/healthz answered %d: %s", status, body)
		}
		return nil
	})
	listeners++
	require.NotZero(t, liveListenerLogLines(process, "oidc "+issuer.URL()+" for "+clusteredListenerAccount),
		"the listener logged no line saying it takes the issuer's tokens of the listed subject alone:\n%s", process.Output.String())
	waited, healthy, reads := awaitListenerStatus(t, process, port, `"status":"healthy"`, clusteredHubListenerHealthWindow)
	answers += reads
	require.NotZero(t, reads, "no /healthz answer was read while waiting for the listener to hold its durable")
	t.Logf("the listener held its durable after %.1fs: %s", waited.Seconds(), strings.TrimSpace(healthy))

	addr := net.JoinHostPort(nonLoopbackIPv4(t), strconv.Itoa(port))
	host := "envoy-nats-listener.envoy.svc.cluster.local"
	for _, call := range []struct {
		name, token string
		want        int
	}{
		{"no token", "", http.StatusUnauthorized},
		{"an unlisted subject's token", issuer.Mint(t, key, issuer.Claims("system:serviceaccount:legion:another-worker", clusteredListenerAudience)), http.StatusUnauthorized},
		{"another audience's token", issuer.Mint(t, key, issuer.Claims(clusteredListenerAccount, "dispatch")), http.StatusUnauthorized},
		{"the listed subject's token", issuer.Mint(t, key, issuer.Claims(clusteredListenerAccount, clusteredListenerAudience)), http.StatusOK},
	} {
		status, body := liveListenerAnswerAs(t, addr, "/v1/sessions", host, call.token)
		answers++
		require.Equal(t, call.want, status, "GET /v1/sessions at %s with %s answered %d: %s\n%s", addr, call.name, status, body, process.Output.String())
		t.Logf("GET /v1/sessions at %s with %s: %d %s", addr, call.name, status, strings.TrimSpace(body))
	}

	controlPort := reservePorts(t, 1)[0]
	control := exec.Command(binary)
	control.Env = envFor(controlPort, append([]string{"ENVOY_OIDC_ISSUER", "ENVOY_OIDC_AUDIENCE", "ENVOY_OIDC_ACCOUNTS"}, liveBrokerSettings...)...)
	refused := startLogged(t, "the same listener without its credentials", control)
	listeners++
	refused.WaitExit(t, "its own API check")
	require.NotNil(t, refused.Cmd.ProcessState, "the refused listener left no exit status")
	require.NotZero(t, refused.Cmd.ProcessState.ExitCode(),
		"the listener started off loopback with no credential:\n%s", refused.Output.String())
	require.NotZero(t, liveListenerLogLines(refused, liveListenerRefusal),
		"the refusal does not name the rule:\n%s", refused.Output.String())
	_, _, err := liveListenerHealthz(controlPort)
	require.Error(t, err, "something answered /healthz on the refused listener's port %d", controlPort)

	t.Logf("%d listener process(es) run, %d HTTP answer(s) read", listeners, answers)
}

// A rolling update of the listener pod runs two listeners of one machine id, and they share one
// durable consumer (bus.ListenerDurable): the second is ready, answering /healthz 200 "starting",
// for as long as the first holds it, and binds it once the first stops. That is why the pod's
// readiness probe reads the status alone — a probe that passed on the 200 would take the new pod
// for healthy and let the deployment stop the one still serving — and why the hand-over has to
// land inside durableBindDeadline, the 135 s at which the second listener would give up its start
// instead (cmd/listener/durable.go).
func TestClusteredNatsHubListenerHandsOverItsDurable(t *testing.T) {
	binary := buildLiveListener(t)
	_, natsURL := testnats.Start(t)

	listeners := 0
	answers := 0
	first, firstPort := startLiveListener(t, "the first listener", binary, natsURL)
	listeners++
	waited, healthy, reads := awaitListenerStatus(t, first, firstPort, `"status":"healthy"`, clusteredHubListenerHealthWindow)
	answers += reads
	require.NotZero(t, reads, "no /healthz answer was read while waiting for the first listener")
	t.Logf("the first listener held its durable after %.1fs: %s", waited.Seconds(), strings.TrimSpace(healthy))

	second, secondPort := startLiveListener(t, "the second listener", binary, natsURL)
	listeners++

	// The second pod is up and serving while the first still holds the durable: 200, and a body
	// that says "starting". Both are asserted, because the two say different things and the
	// readiness probe reads only the second.
	status, body, err := liveListenerHealthz(secondPort)
	require.NoError(t, err)
	answers++
	require.Equal(t, http.StatusOK, status,
		"the second listener's /healthz answered %d while the first held the durable: %s\n%s", status, body, second.Output.String())
	require.Contains(t, body, `"status":"starting"`,
		"the second listener claimed a status other than \"starting\" while the first held the durable: %s\n%s", body, second.Output.String())
	select {
	case <-second.Exited:
		t.Fatalf("the second listener exited while the first held the durable:\n%s", second.Output.String())
	default:
	}
	t.Logf("the second listener's /healthz while the first holds the durable: %d %s", status, strings.TrimSpace(body))

	// The hand-over: the first stops as a deploy stops it, and the second binds the durable it
	// let go.
	first.Terminate(t)
	first.WaitExit(t, "SIGTERM")
	handover, body, reads := awaitListenerStatus(t, second, secondPort, `"status":"healthy"`, clusteredHubListenerHandoverWindow)
	answers += reads
	require.NotZero(t, reads, "no /healthz answer was read while waiting for the hand-over")
	t.Logf("the hand-over took %.1fs: %s", handover.Seconds(), strings.TrimSpace(body))
	require.Less(t, handover, clusteredHubListenerBindDeadline,
		"the hand-over took %.1fs, at or past the %s at which the second listener gives up its start:\n%s",
		handover.Seconds(), clusteredHubListenerBindDeadline, second.Output.String())

	t.Logf("%d listener process(es) run, %d HTTP answer(s) read", listeners, answers)
}

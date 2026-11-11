//go:build natslive

package gcp_test

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/core"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/testnats"
)

// The tests in this file run the listener binary of this checkout in the environment
// gcp:ClusteredNatsHub renders for its listener pod, so they carry the natslive tag. The unit
// tests prove that rendering (clusteredhub_listener_test.go's
// TestClusteredNatsHubListenerEnvironment and the pod's golden
// testdata/clustered-hub-listener-pod.golden); these prove the binary behaves as the rendering
// assumes it does: it starts bound off loopback with no bearer only because
// ENVOY_API_ALLOW_UNAUTHENTICATED is set, it serves /v1 off loopback with no credential for the
// one Host ENVOY_ALLOWED_HOSTS names and answers 421 to every other, and a second pod of the same
// machine id is ready while the first holds the listener's durable and binds it once the first
// stops, which is the whole of a rolling update of that pod.
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

	// clusteredHubListenerLiveMachineID is the machine id every listener here runs as, and so the
	// one durable consumer they contend for (bus.ListenerDurable).
	clusteredHubListenerLiveMachineID = "clustered-hub-live"

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

// liveListenerEnv is the whole environment of a listener pod of gcp:ClusteredNatsHub on this
// machine: core.ListenerConfig.Env for the config the component resolves — PORT,
// ENVOY_LISTEN_HOST=0.0.0.0, ENVOY_ALLOWED_HOSTS, ENVOY_MACHINE_ID and NATS_URLS — with
// ENVOY_API_ALLOW_UNAUTHENTICATED=1 after it, as clusteredHubListenerEnv adds it
// (clusteredhub_listener.go), unless allowUnauthenticated is false, which is the negative control.
// It is the process's whole environment, so there is no ENVOY_API_TOKEN and no OIDC pair in it,
// which is what the pod renders too. The pod's NATS_NKEY_SEED_FILE is left out: this server
// carries no authorization, so there is no seed to mount.
func liveListenerEnv(t *testing.T, port int, natsURL string, allowUnauthenticated bool) []string {
	t.Helper()
	env, err := core.ListenerConfig{
		MachineID:    clusteredHubListenerLiveMachineID,
		Port:         port,
		ListenHost:   "0.0.0.0",
		AllowedHosts: []string{clusteredHubListenerLiveHost},
	}.Env([]string{natsURL})
	require.NoError(t, err)
	if allowUnauthenticated {
		env = append(env, "ENVOY_API_ALLOW_UNAUTHENTICATED=1")
	}
	return env
}

// startLiveListener starts a listener of the pod's environment on a port drawn from ports and
// waits for its /healthz to answer 200, so it returns a process that is serving. It returns the
// process and the port it serves on, which is drawn again on each retry a taken port costs
// (startBound).
func startLiveListener(t *testing.T, name, binary, natsURL string, ports *loopbackPorts) (*cmdtest.Process, int) {
	t.Helper()
	var port int
	process := startBound(t, name, func() *exec.Cmd {
		port = ports.draw(t, 1)[0]
		cmd := exec.Command(binary)
		cmd.Env = liveListenerEnv(t, port, natsURL, true)
		return cmd
	}, func(*cmdtest.Process) error {
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
	req, err := http.NewRequest(http.MethodGet, "http://"+addr+path, nil)
	require.NoError(t, err)
	req.Host = host
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

// The listener pod gcp:ClusteredNatsHub renders starts on this machine, and
// ENVOY_API_ALLOW_UNAUTHENTICATED is what lets it: the listener's own API check refuses a listener
// bound off loopback with neither bearer nor OIDC pair without it (cmd/listener/apiauth.go,
// validateListenerAPIAuth), so the same environment without the flag exits non-zero naming the
// flag and serves nothing at all. The listener that did start then serves /v1 over this machine's
// own non-loopback address with no Authorization header for the one Host ENVOY_ALLOWED_HOSTS
// names, answering GET /v1/sessions 200, and answers 421 to the same request carrying any other
// Host (cmd/listener/apiauth.go, servesHost). That pair is what the NetworkPolicy the component
// renders fences: the listener itself serves whoever reaches it with the right Host, so what keeps
// the open port to the tailnet entry's pods is the policy, and what keeps a rebound name from
// using it is this fence.
func TestClusteredNatsHubListenerServesItsPodEnvironment(t *testing.T) {
	binary := buildLiveListener(t)
	ports := newLoopbackPorts()
	_, natsURL := testnats.Start(t)

	listeners := 0
	process, port := startLiveListener(t, "the listener", binary, natsURL, ports)
	listeners++

	answers := 0
	status, body, err := liveListenerHealthz(port)
	require.NoError(t, err)
	answers++
	require.Equal(t, http.StatusOK, status, "/healthz answered %d: %s\n%s", status, body, process.Output.String())
	require.NotEmpty(t, body, "/healthz answered an empty body")
	t.Logf("the listener's first /healthz: %d %s", status, strings.TrimSpace(body))

	// The boot lines that say the listener is open and fenced, so a later 200 is read against the
	// configuration it was actually given.
	require.NotZero(t, liveListenerLogLines(process, "listener API auth: disabled"),
		"the listener logged no line saying /v1 takes no credential:\n%s", process.Output.String())
	hosts := "listener API hosts: loopback, " + clusteredHubListenerLiveHost
	require.NotZero(t, liveListenerLogLines(process, hosts),
		"the listener logged no %q line:\n%s", hosts, process.Output.String())

	// Nothing of /v1 is read from a listener still answering "starting": the read routes answer
	// 503 until the caches behind them have filled, and a refusal then would be no fence's doing.
	waited, healthy, reads := awaitListenerStatus(t, process, port, `"status":"healthy"`, clusteredHubListenerHealthWindow)
	answers += reads
	require.NotZero(t, reads, "no /healthz answer was read while waiting for the listener to hold its durable")
	t.Logf("the listener held its durable after %.1fs: %s", waited.Seconds(), strings.TrimSpace(healthy))

	// The negative control. The same environment with ENVOY_API_ALLOW_UNAUTHENTICATED removed, on
	// a port of its own so that nothing answering there is this process's own doing: the API check
	// runs before the socket is bound (cmd/listener/main.go), so the port is free throughout and a
	// listener that served would answer on it.
	controlPort := ports.draw(t, 1)[0]
	control := exec.Command(binary)
	control.Env = liveListenerEnv(t, controlPort, natsURL, false)
	refused := startLogged(t, "the listener without ENVOY_API_ALLOW_UNAUTHENTICATED", control)
	listeners++
	refused.WaitExit(t, "its own API check")
	require.NotNil(t, refused.Cmd.ProcessState, "the refused listener left no exit status")
	require.NotZero(t, refused.Cmd.ProcessState.ExitCode(),
		"the listener started with no bearer off loopback and no flag:\n%s", refused.Output.String())
	named := liveListenerLogLines(refused, "ENVOY_API_ALLOW_UNAUTHENTICATED")
	require.NotZero(t, named, "the refusal does not name the flag that would have allowed it:\n%s", refused.Output.String())
	t.Logf("the refusal exited %d and named the flag on %d line(s): %s",
		refused.Cmd.ProcessState.ExitCode(), named, strings.TrimSpace(refused.Output.String()))
	_, _, err = liveListenerHealthz(controlPort)
	require.Error(t, err, "something answered /healthz on the refused listener's port %d", controlPort)

	// The pod's own address, with no credential: the Host the hub told it is served, every other
	// is not.
	addr := net.JoinHostPort(nonLoopbackIPv4(t), strconv.Itoa(port))
	served, body := liveListenerAnswer(t, addr, "/v1/sessions", clusteredHubListenerLiveHost)
	answers++
	require.Equal(t, http.StatusOK, served,
		"the listener refused GET /v1/sessions at %s for the Host it was told, %s: %s\n%s",
		addr, clusteredHubListenerLiveHost, body, process.Output.String())
	t.Logf("GET /v1/sessions at %s as %s: %d %s", addr, clusteredHubListenerLiveHost, served, strings.TrimSpace(body))

	misdirected, body := liveListenerAnswer(t, addr, "/v1/sessions", clusteredHubListenerOtherHost)
	answers++
	require.Equal(t, http.StatusMisdirectedRequest, misdirected,
		"the listener served a Host it was never told, %s: %s\n%s", clusteredHubListenerOtherHost, body, process.Output.String())
	require.Contains(t, body, "ENVOY_ALLOWED_HOSTS",
		"the refusal does not name the list that would have served %s", clusteredHubListenerOtherHost)
	t.Logf("GET /v1/sessions at %s as %s: %d %s", addr, clusteredHubListenerOtherHost, misdirected, strings.TrimSpace(body))

	t.Logf("%d listener process(es) run, %d HTTP answer(s) read", listeners, answers)
}

// A rolling update of the listener pod runs two listeners of one machine id, and they share one
// durable consumer (bus.ListenerDurable): only the pod holding the machine's delivery lease
// receives from it, and the second runs as a standby, answering /healthz 200 with role
// "standby" and the holder's instance for as long as the first holds the lease. It takes the
// lease over within seconds of the first pod's stop. The pod's readiness probe reads the status
// alone, so a standby is Ready, which is what lets the rolling update finish, and the hand-over
// has to land well inside durableBindDeadline, the 135 s at which a listener that can reach
// neither the lease nor the durable gives up its start (cmd/listener/durable.go).
func TestClusteredNatsHubListenerHandsOverItsDurable(t *testing.T) {
	binary := buildLiveListener(t)
	ports := newLoopbackPorts()
	_, natsURL := testnats.Start(t)

	listeners := 0
	answers := 0
	first, firstPort := startLiveListener(t, "the first listener", binary, natsURL, ports)
	listeners++
	waited, healthy, reads := awaitListenerStatus(t, first, firstPort, `"status":"healthy"`, clusteredHubListenerHealthWindow)
	answers += reads
	require.NotZero(t, reads, "no /healthz answer was read while waiting for the first listener")
	t.Logf("the first listener held its durable after %.1fs: %s", waited.Seconds(), strings.TrimSpace(healthy))

	second, secondPort := startLiveListener(t, "the second listener", binary, natsURL, ports)
	listeners++

	// The second pod is up and serving while the first holds the lease: 200, healthy, and a body
	// that names its role as the standby of the first. All three are asserted, because the
	// readiness probe reads only the status and the role is what says which pod delivers.
	status, body, err := liveListenerHealthz(secondPort)
	require.NoError(t, err)
	answers++
	require.Equal(t, http.StatusOK, status,
		"the second listener's /healthz answered %d while the first held the lease: %s\n%s", status, body, second.Output.String())
	require.Contains(t, body, `"role":"standby"`,
		"the second listener claimed a role other than \"standby\" while the first held the lease: %s\n%s", body, second.Output.String())
	require.Contains(t, body, `"active_instance"`,
		"the second listener's standby answer names no active instance: %s\n%s", body, second.Output.String())
	select {
	case <-second.Exited:
		t.Fatalf("the second listener exited while the first held the durable:\n%s", second.Output.String())
	default:
	}
	t.Logf("the second listener's /healthz while the first holds the lease: %d %s", status, strings.TrimSpace(body))

	// The hand-over: the first stops as a deploy stops it, and the second takes the lease and
	// the durable it let go.
	first.Terminate(t)
	first.WaitExit(t, "SIGTERM")
	handover, body, reads := awaitListenerStatus(t, second, secondPort, `"role":"active"`, clusteredHubListenerHandoverWindow)
	answers += reads
	require.NotZero(t, reads, "no /healthz answer was read while waiting for the hand-over")
	t.Logf("the hand-over took %.1fs: %s", handover.Seconds(), strings.TrimSpace(body))
	require.Less(t, handover, clusteredHubListenerBindDeadline,
		"the hand-over took %.1fs, at or past the %s at which the second listener gives up its start:\n%s",
		handover.Seconds(), clusteredHubListenerBindDeadline, second.Output.String())

	t.Logf("%d listener process(es) run, %d HTTP answer(s) read", listeners, answers)
}

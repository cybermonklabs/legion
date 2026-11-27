package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/bus/bustest"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// errorLine matches an ERROR record in each format the listener's process writes: its own JSON
// handler, the default slog text handler and slog's default log.Logger bridge.
var errorLine = regexp.MustCompile(`"level":"ERROR"|level=ERROR|^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2} ERROR `)

// A SIGTERM is an ordered shutdown, not a failure: the listener stops HTTP, drains NATS and exits,
// and nothing on that path is an error or a reconnect. The listener is stopped with role-lane
// traffic in flight, as a production listener is: a holder on its own connection takes each
// forward and a role message arrives every 10 ms, so at the signal one forward is in its handler
// and more are queued behind it. Each must reach the stream: the forward opens a reply
// subscription for the stream's acknowledgement, which a connection that is already draining
// refuses. The drain also closes the session registry's KV watcher, which logs at ERROR if the
// registry still expects it, and looks like a lost connection to the bus client, whose recovery
// would re-dial NATS. Whether a goroutine logs before the exit is a race, so the test stops the
// real binary several times.
//
// One failure is not counted: a role forward whose window (roleForwardTimeout) ended before it was
// published, which its line marks "window_ended_before_publish":true (finishRoleForward). The
// window runs on the listener's own wall clock, which a loaded machine spends without the listener
// running, so that failure says how the machine ran and not what the shutdown did. A drain cannot
// cause it: a forward meets a connection before its publish only in ensureConnWithContext, which
// serves a draining connection and refuses a stopped client at once, in SubscribeSync, which
// refuses a draining connection with an error of its own, counted here, and in publishConfirmed's
// read of the connection's state, which waits for nats.go's connection lock within the window. A
// drain takes that lock only for short steps, and never while it waits for the server's answer
// (nats.go's drainConnection, FlushTimeout and close). A reconnect's handshake holds it through
// its dial: a forward whose read has not answered when its window ends carries the mark, and its
// error names bus.ErrReconnecting. A drain that holds a forward after its publish fails it on its
// flush or its acknowledgement, with no mark, and is counted. What this cannot see is a shutdown
// path that holds a forward before its publish, behind a lock say: that forward would carry the
// mark. A change to the shutdown path or to bus.Client.StartRequestStoredTo is read against this
// test for that case.
func TestListenerSIGTERMIsAnOrderedShutdown(t *testing.T) {
	t.Parallel()
	_, uri := testnats.Start(t)
	testnats.Connect(t, uri).Close()

	binary := cmdtest.Build(t, "envoy-listener")
	publisher := testnats.Connect(t, uri)
	t.Cleanup(publisher.Close)

	for run := 1; run <= 10; run++ {
		listener := startListenerProcess(t, binary, uri, "sigterm-test-"+strconv.Itoa(run))
		listener.waitHealthy(t)

		role := "sigterm-test-" + strconv.Itoa(run)
		sessionID := "ses_sigterm_holder_" + strconv.Itoa(run)
		postListener(t, listener.port, "/v1/interests/subscribe", consumerRegistration(sessionID, "epoch-1"))
		postListener(t, listener.port, "/v1/roles/set", `{"session_id":"`+sessionID+`","role":"`+role+`"}`)
		holder := testnats.Connect(t, uri)
		var forwarded atomic.Uint64
		if _, err := holder.Subscribe(contracts.LegacyAgentSubject(sessionID), func(*natsgo.Msg) { forwarded.Add(1) }); err != nil {
			t.Fatalf("run %d: holder subscribe: %v", run, err)
		}
		if err := holder.Flush(); err != nil {
			t.Fatalf("run %d: holder flush: %v", run, err)
		}
		stopTraffic := make(chan struct{})
		trafficDone := make(chan struct{})
		go func() {
			defer close(trafficDone)
			for index := 0; ; index++ {
				// The cadence of a client's steady role traffic, which this stand-in sender copies:
				// it keeps a forward in flight and more queued behind it at every moment of the
				// shutdown, and bounds nothing.
				select {
				case <-stopTraffic:
					return
				case <-time.After(10 * time.Millisecond):
				}
				item := contracts.Envelope{
					EventID:        "evt-sigterm-" + strconv.Itoa(run) + "-" + strconv.Itoa(index),
					Source:         "agent",
					SourceEventID:  "source-sigterm",
					Topic:          contracts.RoleTopicPrefix + role,
					DedupeKey:      "sigterm." + strconv.Itoa(run) + "." + strconv.Itoa(index),
					IssuedAt:       contracts.NowMillis(),
					PayloadSummary: "sigterm role traffic",
					TraceID:        "trace-sigterm",
				}
				data, _ := json.Marshal(item)
				_ = publisher.Publish(item.Topic, data)
			}
		}()
		// The signal arrives while traffic is flowing through the listener, which the holder's
		// own count of the forwards it took says, rather than after a span of the machine's clock.
		testwait.Eventually(t, "the role holder to take forwards through the listener before the signal", func() bool {
			return forwarded.Load() >= 3
		})

		listener.Terminate(t)
		listener.WaitExit(t, "SIGTERM")
		close(stopTraffic)
		<-trafficDone
		holder.Close()
		output := listener.Output.String()
		if !strings.Contains(output, "envoy-listener shutdown complete") {
			t.Fatalf("run %d: the listener did not finish its ordered shutdown:\n%s", run, output)
		}
		signal := strings.Index(output, shutdownSignalLine)
		var counted []string
		start := 0
		for line := range strings.SplitSeq(output, "\n") {
			lineStart := start
			start += len(line) + 1
			if !errorLine.MatchString(line) {
				continue
			}
			side := "before the signal"
			if signal >= 0 && lineStart > signal {
				side = "after the signal"
			}
			if forwardWindowEndedBeforePublish(line) {
				t.Logf("run %d: not counted, %s: %s", run, side, line)
				continue
			}
			counted = append(counted, side+": "+line)
		}
		if len(counted) > 0 {
			t.Fatalf("run %d: a SIGTERM shutdown logged an error:\n%s\nthe listener's output from 2s before its signal on:\n%s",
				run, strings.Join(counted, "\n"), outputAroundSignal(output, signal))
		}
		_, afterSignal, _ := strings.Cut(output, shutdownSignalLine)
		if strings.Contains(afterSignal, "envoy nats recovery attempt") {
			t.Fatalf("run %d: the listener reconnected to NATS during its shutdown:\n%s", run, afterSignal)
		}
		if !strings.Contains(afterSignal, "listener role forwarded") {
			t.Fatalf("run %d: no role message in flight at the signal reached the holder:\n%s", run, afterSignal)
		}
	}
}

// shutdownSignalLine is the message the listener logs when a signal begins its shutdown.
const shutdownSignalLine = "received signal, shutting down"

// forwardWindowEndedBeforePublish reports whether line is the listener's JSON record of a role
// forward that failed because its window ended before it was published (finishRoleForward).
func forwardWindowEndedBeforePublish(line string) bool {
	var record struct {
		Msg                      string `json:"msg"`
		WindowEndedBeforePublish bool   `json:"window_ended_before_publish"`
	}
	return json.Unmarshal([]byte(line), &record) == nil && record.Msg == "listener role forward failed" && record.WindowEndedBeforePublish
}

// outputAroundSignal is the listener's output from the first JSON line whose time is no more than
// 2 s before that of its signal line, which starts at or before offset signal, to the end: what a
// failure of the shutdown shows of how the listener ran into its signal. Output with no signal
// line, or none whose time can be read, is given whole.
func outputAroundSignal(output string, signal int) string {
	type timed struct {
		Time time.Time `json:"time"`
	}
	if signal < 0 {
		return output
	}
	lineStart := strings.LastIndexByte(output[:signal], '\n') + 1
	signalLine, _, _ := strings.Cut(output[lineStart:], "\n")
	var signalled timed
	if json.Unmarshal([]byte(signalLine), &signalled) != nil || signalled.Time.IsZero() {
		return output
	}
	from := signalled.Time.Add(-2 * time.Second)
	start := 0
	for line := range strings.SplitSeq(output, "\n") {
		var record timed
		if json.Unmarshal([]byte(line), &record) == nil && !record.Time.IsZero() && !record.Time.Before(from) {
			return output[start:]
		}
		start += len(line) + 1
	}
	return output
}

// A replacement that waits out the old task's durable serves /v1 while it waits, so a SIGTERM there
// (a circuit-breaker rollback, a second deploy) is an ordered shutdown like any other: HTTP drains,
// NATS drains, nothing on the path is an error, and the durable the old task holds is left alone.
func TestASIGTERMWhileAnotherTaskHoldsTheDurableIsAnOrderedShutdown(t *testing.T) {
	t.Parallel()
	const machineID = "sigterm-during-bind"
	old, holder := durableHeldElsewhere(t, testnats.URL(t), "listener-"+machineID)

	listener := startListenerProcess(t, cmdtest.Build(t, "envoy-listener"), old.Conn.ConnectedUrl(), machineID)
	listener.WaitForOutput(t, "subscribe failed, retrying")
	listener.Terminate(t)
	listener.WaitExit(t, "SIGTERM")
	output := listener.Output.String()
	if code := listener.Cmd.ProcessState.ExitCode(); code != 1 {
		t.Fatalf("exit code = %d, want 1 (-1 is a death by the signal, with no ordered shutdown):\n%s", code, output)
	}
	for _, line := range []string{"received signal, shutting down", "envoy-listener shutdown complete"} {
		if !strings.Contains(output, line) {
			t.Fatalf("the listener's output has no %q:\n%s", line, output)
		}
	}
	for line := range strings.SplitSeq(output, "\n") {
		if errorLine.MatchString(line) {
			t.Fatalf("a SIGTERM during the durable's bind wait logged an error: %s", line)
		}
	}
	if !holder.IsValid() {
		t.Fatal("the old task's subscription to the durable is gone after the replacement's shutdown")
	}
}

// listenerTestToken is the shared /v1 bearer every test-started listener is given.
const listenerTestToken = "listener-test-token"

// listenerProcess is a listener binary a test started, serving on the loopback port port.
type listenerProcess struct {
	*cmdtest.Process
	port int
}

// startListenerProcess starts binary as machine machineID against the NATS at uri, with env added
// to its environment, and kills it when the test ends if it is still running. It provisions the
// server first (provisionListenerServer), since no listener creates a bucket it copies. It serves
// on a loopback port the kernel chose and the test holds from that moment, which the process
// inherits as ENVOY_LISTEN_FD and serves on, so no other program can take the port before it
// serves. PORT names the same port, which the listener checks against the socket.
func startListenerProcess(t *testing.T, binary, uri, machineID string, env ...string) *listenerProcess {
	t.Helper()
	provisionListenerServer(t, uri, env)
	return startListenerProcessAsIs(t, binary, uri, machineID, env...)
}

// startListenerProcessAsIs is startListenerProcess without the provisioning: the listener starts
// on the server as the test left it, for a test that provisions the server itself
// (provisionListenerServer) because the listener's own grant does not let the harness do it, or
// that means the listener to start on a server holding no bucket.
func startListenerProcessAsIs(t *testing.T, binary, uri, machineID string, env ...string) *listenerProcess {
	t.Helper()
	held := cmdtest.HoldPort(t)
	cmd := exec.Command(binary)
	cmd.Env = append([]string{
		"PORT=" + strconv.Itoa(held.Port),
		"ENVOY_LISTEN_HOST=127.0.0.1",
		"ENVOY_MACHINE_ID=" + machineID,
		"NATS_URLS=" + uri,
		"ENVOY_API_TOKEN=" + listenerTestToken,
	}, env...)
	held.Pass(cmd, "ENVOY_LISTEN_FD")
	return &listenerProcess{Process: cmdtest.Start(t, "the listener "+machineID, cmd), port: held.Port}
}

// provisionListenerServer creates, on the test's own server at uri, every bucket a listener copies
// and the marker that says the server is a fixture's (bustest.Provision), at the copies the
// listener's environment env asks for (ENVOY_NATS_REPLICAS, else one), as the operator's
// `envoy-infra buckets` does before a deployment's first listener starts: no listener creates one,
// and a listener started without them waits at its start. It acts once per server, so a test that
// deletes a bucket and starts a listener again on the same server finds the bucket still missing.
// It connects as the listener does, with the nkey seed env names (NATS_NKEY_SEED) when it names
// one, and closes that connection before it returns, so the listener finds the server's
// connections as the test left them.
func provisionListenerServer(t *testing.T, uri string, env []string) {
	t.Helper()
	replicas := 1
	var seed string
	for _, setting := range env {
		name, value, _ := strings.Cut(setting, "=")
		switch name {
		case bus.ReplicasEnvVar:
			n, err := bus.ParseNATSReplicas(value)
			if err != nil {
				t.Fatalf("%s: %v", setting, err)
			}
			replicas = n
		case bus.NkeySeedEnvVar:
			seed = value
		}
	}
	if seed == "" {
		bustest.ProvisionURL(t, uri, replicas)
		return
	}
	conn, err := testnats.ConnectSeed(t, uri, seed)
	if err != nil {
		t.Fatalf("connect to %s with the listener's seed: %v", uri, err)
	}
	defer conn.Close()
	if err := bustest.Provision(conn, replicas); err != nil {
		t.Fatalf("provision the listener's NATS server %s with the buckets it copies: %v", uri, err)
	}
}

// A harness that holds the port hands the listener the listening socket and sets neither
// ENVOY_LISTEN_HOST nor PORT: the listener serves on the socket, at the address the kernel chose
// for it, and says that address in its listening line. Without the socket it would bind
// 127.0.0.1:9020.
func TestListenerServesOnTheListenSocketItInheritsWithNoAddressSet(t *testing.T) {
	t.Parallel()
	held := cmdtest.HoldPort(t)
	uri := testnats.URL(t)
	provisionListenerServer(t, uri, nil)
	cmd := exec.Command(cmdtest.Build(t, "envoy-listener"))
	cmd.Env = []string{
		"ENVOY_MACHINE_ID=listen-fd-unset-address",
		"NATS_URLS=" + uri,
		"ENVOY_API_TOKEN=" + listenerTestToken,
	}
	held.Pass(cmd, "ENVOY_LISTEN_FD")
	listener := &listenerProcess{Process: cmdtest.Start(t, "the listener", cmd), port: held.Port}
	// waitHealthy takes the listening line only when its addr is the held socket's, 127.0.0.1 on
	// held.Port, then waits for /healthz there to report healthy.
	listener.waitHealthy(t)
}

// PORT=0 with no inherited socket still binds a port the kernel chooses, and the listening line
// names the port it got, so a harness that starts the listener this way reads its port there.
func TestListenerOnPortZeroLogsThePortTheKernelChose(t *testing.T) {
	t.Parallel()
	uri := testnats.URL(t)
	provisionListenerServer(t, uri, nil)
	cmd := exec.Command(cmdtest.Build(t, "envoy-listener"))
	cmd.Env = []string{
		"PORT=0",
		"ENVOY_LISTEN_HOST=127.0.0.1",
		"ENVOY_MACHINE_ID=listen-port-zero",
		"NATS_URLS=" + uri,
		"ENVOY_API_TOKEN=" + listenerTestToken,
	}
	process := cmdtest.Start(t, "the listener", cmd)
	addr := listeningAddr(t, process)
	host, portText, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("the listener logged that it listens on %q, which is no address: %v", addr, err)
	}
	port, err := strconv.Atoi(portText)
	if host != "127.0.0.1" || err != nil || port == 0 {
		t.Fatalf("the listener logged that it listens on %q, want 127.0.0.1 and the port the kernel chose:\n%s", addr, process.Output.String())
	}
	(&listenerProcess{Process: process, port: port}).waitHealthy(t)
}

// An inherited socket on every interface is judged by the API auth fence as ENVOY_LISTEN_HOST=0.0.0.0
// is, though ENVOY_LISTEN_HOST is unset and would mean loopback: with no bearer, no OIDC pair and
// no ENVOY_API_ALLOW_UNAUTHENTICATED, the listener refuses to start and serves nothing.
func TestListenerRefusesAnInheritedSocketOnEveryInterfaceWithoutCredentials(t *testing.T) {
	t.Parallel()
	wildcard, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("listen on every interface: %v", err)
	}
	socket, err := wildcard.(*net.TCPListener).File()
	_ = wildcard.Close()
	if err != nil {
		t.Fatalf("take the listening socket's descriptor: %v", err)
	}
	t.Cleanup(func() { _ = socket.Close() })
	cmd := exec.Command(cmdtest.Build(t, "envoy-listener"))
	cmd.Env = []string{
		"ENVOY_MACHINE_ID=listen-fd-wildcard",
		"NATS_URLS=nats://127.0.0.1:1",
		"ENVOY_LISTEN_FD=3",
	}
	cmd.ExtraFiles = []*os.File{socket}
	process := refusedStart(t, cmd)
	refusal := "ENVOY_LISTEN_FD=3, a socket listening on " + wildcard.Addr().String() + ", is not on loopback"
	if output := process.Output.String(); !strings.Contains(output, refusal) || !strings.Contains(output, "ENVOY_API_TOKEN") {
		t.Errorf("the listener did not refuse its start with %q, naming the credentials it needs:\n%s", refusal, output)
	}
}

// PORT, when set beside an inherited socket, must name the socket's port, and PORT=0 names none:
// the listener refuses to start, naming PORT and ENVOY_LISTEN_FD, and serves nothing.
func TestListenerRefusesAPortThatIsNotItsInheritedSocketsPort(t *testing.T) {
	t.Parallel()
	binary := cmdtest.Build(t, "envoy-listener")
	for name, port := range map[string]func(held *cmdtest.HeldPort) string{
		"another port": func(held *cmdtest.HeldPort) string { return strconv.Itoa(held.Port%65535 + 1) },
		"port zero":    func(*cmdtest.HeldPort) string { return "0" },
	} {
		t.Run(name, func(t *testing.T) {
			held := cmdtest.HoldPort(t)
			cmd := exec.Command(binary)
			cmd.Env = []string{
				"PORT=" + port(held),
				"ENVOY_LISTEN_HOST=127.0.0.1",
				"ENVOY_MACHINE_ID=listen-fd-port-mismatch",
				"NATS_URLS=nats://127.0.0.1:1",
				"ENVOY_API_TOKEN=" + listenerTestToken,
			}
			held.Pass(cmd, "ENVOY_LISTEN_FD")
			process := refusedStart(t, cmd)
			setting, socket := "PORT="+port(held)+" ", "ENVOY_LISTEN_FD=3, a socket listening on "+held.Addr
			if output := process.Output.String(); !strings.Contains(output, setting) || !strings.Contains(output, socket) {
				t.Errorf("the listener did not refuse its start naming %q and %q:\n%s", setting, socket, output)
			}
		})
	}
}

// An ENVOY_LISTEN_FD that names something other than a listening socket refuses the start, naming
// the variable, and the listener binds nothing in its place: not PORT, which here names a port the
// test holds, so a bind would fail on it.
func TestListenerRefusesAListenFDThatIsNotASocketAndBindsNothing(t *testing.T) {
	t.Parallel()
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("hold the port PORT names: %v", err)
	}
	t.Cleanup(func() { _ = occupied.Close() })
	notASocket, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(cmdtest.Build(t, "envoy-listener"))
	cmd.Env = []string{
		"PORT=" + strconv.Itoa(occupied.Addr().(*net.TCPAddr).Port),
		"ENVOY_LISTEN_HOST=127.0.0.1",
		"ENVOY_MACHINE_ID=listen-fd-not-a-socket",
		"NATS_URLS=nats://127.0.0.1:1",
		"ENVOY_API_TOKEN=" + listenerTestToken,
		"ENVOY_LISTEN_FD=3",
	}
	cmd.ExtraFiles = []*os.File{notASocket}
	process := refusedStart(t, cmd)
	output := process.Output.String()
	if !strings.Contains(output, "ENVOY_LISTEN_FD=3 is not a socket") {
		t.Errorf("the listener did not refuse its start naming ENVOY_LISTEN_FD=3 and what is wrong with it:\n%s", output)
	}
	if strings.Contains(output, "address already in use") {
		t.Errorf("the listener bound PORT in the descriptor's place:\n%s", output)
	}
}

// refusedStart starts the listener cmd and waits for it to exit, and fails the test unless it
// exited 1 without ever saying it listens.
func refusedStart(t *testing.T, cmd *exec.Cmd) *cmdtest.Process {
	t.Helper()
	process := cmdtest.Start(t, "the listener", cmd)
	process.WaitExit(t, "its start")
	output := process.Output.String()
	if code := cmd.ProcessState.ExitCode(); code != 1 {
		t.Errorf("the listener exited %d, want 1:\n%s", code, output)
	}
	if strings.Contains(output, "envoy-listener listening") {
		t.Errorf("the listener served despite refusing its start:\n%s", output)
	}
	return process
}

// listeningAddr waits for the listener's "envoy-listener listening" line and returns its addr.
func listeningAddr(t *testing.T, process *cmdtest.Process) string {
	t.Helper()
	process.WaitForOutput(t, `"msg":"envoy-listener listening"`)
	for line := range strings.SplitSeq(process.Output.String(), "\n") {
		var record struct {
			Msg  string `json:"msg"`
			Addr string `json:"addr"`
		}
		if json.Unmarshal([]byte(line), &record) == nil && record.Msg == "envoy-listener listening" {
			return record.Addr
		}
	}
	t.Fatalf("the listener's output has no JSON listening line:\n%s", process.Output.String())
	return ""
}

// postListener calls a listener /v1 route as the test's shared-token caller and requires a 200.
func postListener(t *testing.T, port int, path, body string) {
	t.Helper()
	if status, answer := callListener(t, port, http.MethodPost, path, body); status != http.StatusOK {
		t.Fatalf("%s: status %d: %s", path, status, answer)
	}
}

// waitHealthy waits through the shared wait for the listener to log that it listens on its own
// loopback port, then for /healthz there to report "healthy", and fails the test when it never
// does. The socket the listener inherited is not readiness: the kernel queues connections on it
// from before the process starts, so a request there waits rather than fails until the process
// serves, and only its listening line says it does. Each request carries the shared wait's context
// (testwait.Context), so one that waits on a process that does not answer ends where the wait
// does, and no request has a bound of its own. /healthz answers 200 with "starting" from the
// moment the listener serves, before NATS connects and before the listener installs its SIGTERM
// handler, and a SIGTERM in that window kills the process without an ordered shutdown.
//
// A listener that exits during its start-up fails this wait at once, naming its exit status and
// its last output line, rather than being waited for until the test binary's own deadline: a
// process that has exited answers nothing, so every further poll reads the same closed port, and
// the wait's own give-up names neither the exit nor its reason. Either failure also names the
// last /healthz answer, so a listener still binding ("starting") reads differently from one
// whose dependency is down ("unhealthy", with the error that endpoint reports).
func (p *listenerProcess) waitHealthy(t *testing.T) {
	t.Helper()
	if addr, own := listeningAddr(t, p.Process), "127.0.0.1:"+strconv.Itoa(p.port); addr != own {
		_ = p.Cmd.Process.Kill()
		t.Fatalf("the listener logged that it listens on %s, not on its own %s:\n%s", addr, own, p.Output.String())
	}
	ctx := testwait.Context(t)
	url := "http://127.0.0.1:" + strconv.Itoa(p.port) + "/healthz"
	answer := "none: no request to /healthz was answered"
	// failIfExited ends the wait the moment the process is gone, which is the misreading this
	// helper exists to remove: without it a listener that died would be waited for until the
	// test binary's deadline and then reported as still running.
	failIfExited := func() {
		t.Helper()
		select {
		case <-p.Exited:
			// Exited closes only once the output is whole, so this read is final.
			output := p.Output.String()
			t.Fatalf("the listener exited during its start-up with %s; its last line was %q, and the last /healthz answer was %s:\n%s",
				p.Cmd.ProcessState, lastOutputLine(output), answer, output)
		default:
		}
	}
	testwait.Until(t, "the listener to answer /healthz healthy", func() (string, bool) {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			t.Fatalf("build the /healthz request: %v", err)
		}
		response, err := http.DefaultClient.Do(request)
		if err == nil {
			var health struct {
				Status string `json:"status"`
				Error  string `json:"error"`
			}
			decodeErr := json.NewDecoder(response.Body).Decode(&health)
			_ = response.Body.Close()
			if decodeErr == nil && health.Status == "healthy" {
				return answer, true
			}
			if decodeErr == nil {
				answer = fmt.Sprintf("%d %q", response.StatusCode, health.Status)
				if health.Error != "" {
					answer += fmt.Sprintf(", error %q", health.Error)
				}
			} else {
				answer = fmt.Sprintf("%d, which is no health report: %v", response.StatusCode, decodeErr)
			}
		}
		failIfExited()
		return fmt.Sprintf("%s; the listener's last line was %q", answer, lastOutputLine(p.Output.String())), false
	})
}

// lastOutputLine is the final non-empty line of a process's output, the one a start-up that ended
// in log.Fatal wrote.
func lastOutputLine(output string) string {
	trimmed := strings.TrimRight(output, "\n")
	return trimmed[strings.LastIndexByte(trimmed, '\n')+1:]
}

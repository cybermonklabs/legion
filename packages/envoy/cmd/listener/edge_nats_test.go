package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/ciphertoken/ciphertokentest"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/config"
	"github.com/sjawhar/envoy/internal/metrics"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/natsgrant"
)

// edgeProject is the one project this package's edge listeners serve.
const edgeProject = "PRIV"

// startEdgeListenerProcess starts binary as an edge listener of projects (ENVOY_EDGE_PROJECTS),
// machine machineID, against the NATS at uri, with env added to its environment: on a loopback port
// the test holds (startListenerProcess says why), taking broker access tokens from the broker at
// brokerURL, which it asks with accountToken, and no other credential, as an edge listener takes
// them.
func startEdgeListenerProcess(t *testing.T, binary, uri, machineID, projects, brokerURL, accountToken string, env ...string) *listenerProcess {
	t.Helper()
	held := cmdtest.HoldPort(t)
	account := filepath.Join(t.TempDir(), "account-token")
	if err := os.WriteFile(account, []byte(accountToken), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary)
	cmd.Env = append([]string{
		"PORT=" + strconv.Itoa(held.Port),
		"ENVOY_LISTEN_HOST=127.0.0.1",
		"ENVOY_MACHINE_ID=" + machineID,
		"NATS_URLS=" + uri,
		config.EdgeProjectsEnvVar + "=" + projects,
		EnvBrokerURL + "=" + brokerURL,
		EnvBrokerAccountTokenFile + "=" + account,
		EnvPublicURL + "=http://127.0.0.1:" + strconv.Itoa(held.Port),
	}, env...)
	held.Pass(cmd, "ENVOY_LISTEN_FD")
	return &listenerProcess{Process: cmdtest.Start(t, "the edge listener "+machineID, cmd), port: held.Port}
}

// An edge listener reads the stream and never creates it, so its start ends, naming the stream,
// on a server that holds none, and it starts on the same server once a shared listener, which
// owns the stream, has created it.
func TestTheEdgeRefusesToStartWithoutTheStream(t *testing.T) {
	t.Parallel()
	binary := cmdtest.Build(t, "envoy-listener")
	uri := testnats.URL(t)
	broker := ciphertokentest.NewStubBroker(t)

	refused := startEdgeListenerProcess(t, binary, uri, "edge-no-stream", edgeProject, broker.Server.URL, edgeAccountToken)
	refused.WaitExit(t, "its start")
	output := refused.Output.String()
	if code := refused.Cmd.ProcessState.ExitCode(); code != 1 {
		t.Fatalf("the edge listener exited %d on a server with no stream, want 1:\n%s", code, output)
	}
	if want := "the server holds no " + bus.Stream + " stream, and an edge listener may not create it"; !strings.Contains(output, want) {
		t.Fatalf("the edge listener's start did not end naming the missing stream (%q):\n%s", want, output)
	}

	shared := startListenerProcess(t, binary, uri, "shared-makes-the-stream")
	shared.waitHealthy(t)
	shared.Terminate(t)
	shared.WaitExit(t, "SIGTERM")

	started := startEdgeListenerProcess(t, binary, uri, "edge-with-stream", edgeProject, broker.Server.URL, edgeAccountToken)
	started.waitHealthy(t)
	if output := started.Output.String(); strings.Contains(output, "role lane subscribed") || !strings.Contains(output, "role lane not subscribed") {
		t.Fatalf("the edge listener opened a role lane, or did not say it opened none:\n%s", output)
	}
}

// Check l16 of dispatch://INFRA-3589 (R18) at the edge's start: an edge listener whose machine id
// is a fully qualified host name takes the grant natsgrant.Edge builds for that id, and its start
// names every inbox under the prefix that grant subscribes, the id spelled as its durable spells it
// (bus.ListenerSpelling), each dot as %2E. The start connects as a user holding that grant, on a
// server with no stream. Its read of the stream is answered on its inbox, so the start ends naming
// the missing stream: a reply under any other prefix would be a subscription the server refuses.
// An id with a space is refused naming edge mode, since its spelling, a hyphen, is another id's.
func TestADottedEdgeMachineIDStartsUnderItsSpelledPrefix(t *testing.T) {
	// Not parallel: it changes something the whole process shares: it calls t.Setenv.
	const (
		machineID = "host.example.internal"
		prefix    = "_INBOX_envoy_edge_host%2Eexample%2Einternal"
	)
	edge, err := newEdgeGrant(machineID, []string{edgeProject}, metrics.New())
	if err != nil {
		t.Fatalf("newEdgeGrant(%q, [%s]): %v", machineID, edgeProject, err)
	}
	grant, err := natsgrant.Edge(machineID, []string{edgeProject})
	if err != nil {
		t.Fatalf("natsgrant.Edge(%q, [%s]): %v", machineID, edgeProject, err)
	}
	if edge.inboxPrefix != prefix || grant.Subscribe[0] != prefix+".>" {
		t.Fatalf("the start's inbox prefix is %q and the grant subscribes %q, want %q under both", edge.inboxPrefix, grant.Subscribe[0], prefix)
	}
	seed, public := testnats.User(t)
	_, uri := testnats.StartConfig(t, testnats.GrantedUsersConfig(testnats.StaticUser{NKey: public, Publish: grant.Publish, Subscribe: grant.Subscribe}), []string{"-js"}, false)
	t.Setenv(bus.NkeySeedEnvVar, seed)
	client, err := connectListenerBus(config.Service{NATSURLs: []string{uri}}, edge, newNATSStart(listenerCredentialWait))
	if err == nil {
		client.Close()
		t.Fatal("the edge's start connected on a server with no stream")
	}
	if want := "the server holds no " + bus.Stream + " stream"; !strings.Contains(err.Error(), want) {
		t.Fatalf("the edge's start ended with %q, want it to name the missing stream (%q): its read was not answered under %s", err, want, prefix)
	}
	if refused, err := newEdgeGrant("host example", []string{edgeProject}, metrics.New()); err == nil || refused != nil || !strings.Contains(err.Error(), "edge mode") {
		t.Fatalf("newEdgeGrant(host example, [%s]) = %v, %v; want no grant and a refusal naming edge mode", edgeProject, refused, err)
	}
}

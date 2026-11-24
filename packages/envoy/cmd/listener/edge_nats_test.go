package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/consumernames"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/ciphertoken/ciphertokentest"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/config"
	"github.com/sjawhar/envoy/internal/fixtureenv"
	"github.com/sjawhar/envoy/internal/metrics"
	"github.com/sjawhar/envoy/internal/testnats"
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

// An edge listener's main client runs the process's consumer naming mode (ENVOY_CONSUMER_NAMES),
// as every other listener's does, so its durable and its sessions' consumers, whose connection
// takes the mode from this client (connectSessionDelivery), are named by the mode its start logs.
// The client of the listener that owns the stream, which takes the mode through
// listenerStreamOptions, is read beside it under the same settings.
func TestAnEdgeListenersClientRunsTheConsumerNamingMode(t *testing.T) {
	for _, mode := range []consumernames.Mode{consumernames.ModeFixed, consumernames.ModeFresh} {
		t.Run(string(mode), func(t *testing.T) {
			t.Setenv("ENVOY_MACHINE_ID", "edge-naming-mode")
			t.Setenv("NATS_URLS", testnats.URL(t))
			t.Setenv(consumernames.ModeEnvVar, string(mode))
			t.Setenv(config.EdgeProjectsEnvVar, edgeProject)
			for _, name := range []string{bus.ReplicasEnvVar, bus.StreamMaxBytesEnvVar, bus.ListenerRoleEnvVar} {
				fixtureenv.Unset(t, name)
			}
			cfg, err := config.Load(9020)
			if err != nil {
				t.Fatalf("load the edge listener's settings: %v", err)
			}
			owning, err := connectListenerBus(cfg, nil, newNATSStart(listenerCredentialWait))
			if err != nil {
				t.Fatalf("connect the listener that owns the stream: %v", err)
			}
			t.Cleanup(owning.Close)
			edge, err := connectListenerBus(cfg, testEdgeGrant(t, metrics.New()), newNATSStart(listenerCredentialWait))
			if err != nil {
				t.Fatalf("connect the edge listener: %v", err)
			}
			t.Cleanup(edge.Close)
			if got := owning.ConsumerNames(); got != mode {
				t.Errorf("the client that owns the stream runs %s names, want %s, the mode %s sets", got, mode, consumernames.ModeEnvVar)
			}
			if got := edge.ConsumerNames(); got != mode {
				t.Errorf("the edge listener's client runs %s names, want %s, the mode %s sets", got, mode, consumernames.ModeEnvVar)
			}
		})
	}
}

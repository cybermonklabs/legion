//go:build twobuild

// The mixed-build harness: the previous release's programs beside this tree's on one writer-hold
// stack (newStack, harness_test.go), which is what a hub runs across the cutover window. The
// previous release is a directory packages/envoy/scripts/build-dispatch-build.sh writes with
// --previous-release, named by DISPATCH_PREVIOUS_BUILD, as the two-build test of Dispatch reads it
// (cmd/dispatch/twobuild_harness_test.go); its envoy-listener and envoy-dispatch run as processes.
// This tree's listener is built from cmd/listener, and its session consumer calls also run in the
// test's own process (bus.SessionConsumers), the calls the listener's relay makes.
//
// Its own build tag, twobuild, and its own CI step, in ci-job-dispatch-two-builds.yaml, which builds
// the previous release: a row skips where DISPATCH_PREVIOUS_BUILD is unset, and a skip is no pass,
// so the rows run only where that job gives them the build.

package consumernames

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"

	selection "github.com/sjawhar/envoy/consumernames"
	"github.com/sjawhar/envoy/infra/streams"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/holdrecord"
	"github.com/sjawhar/envoy/internal/streamset"
	"github.com/sjawhar/envoy/natsgrant"
)

// mixedBuildPreviousVariable names the directory build-dispatch-build.sh wrote for the previous
// release, the variable the two-build test of Dispatch reads (previousBuildVariable there).
const mixedBuildPreviousVariable = "DISPATCH_PREVIOUS_BUILD"

// mixedBuildParts are the parts of the previous release's directory these rows read.
var mixedBuildParts = []string{"COMMIT", "envoy-listener", "envoy-dispatch"}

// mixedBuildRelease is the previous release's directory and the commit it was built from.
type mixedBuildRelease struct {
	dir    string
	commit string
}

// mixedBuildPreviousRelease reads DISPATCH_PREVIOUS_BUILD. It skips the test when the variable is
// unset, naming the command that builds it, and fails it when the directory lacks a part, such as
// a build from before the script built the listener.
func mixedBuildPreviousRelease(t *testing.T) mixedBuildRelease {
	t.Helper()
	dir, set := os.LookupEnv(mixedBuildPreviousVariable)
	if !set {
		t.Skipf("%s is unset. Build it with packages/envoy/scripts/build-dispatch-build.sh --previous-release --out <dir>, then set %s=<dir>.",
			mixedBuildPreviousVariable, mixedBuildPreviousVariable)
	}
	if dir == "" {
		t.Fatalf("%s is set but empty. Set it to the directory build-dispatch-build.sh wrote.", mixedBuildPreviousVariable)
	}
	var missing []string
	for _, part := range mixedBuildParts {
		if info, err := os.Stat(filepath.Join(dir, part)); err != nil || info.IsDir() {
			missing = append(missing, part)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("%s=%s is not a complete build: it has no %s. Build it again with packages/envoy/scripts/build-dispatch-build.sh --previous-release --out <dir>, which builds the listener beside the server.",
			mixedBuildPreviousVariable, dir, strings.Join(missing, ", "))
	}
	commit, err := os.ReadFile(filepath.Join(dir, "COMMIT"))
	require.NoError(t, err, "read the previous release's COMMIT")
	return mixedBuildRelease{dir: dir, commit: strings.TrimSpace(string(commit))}
}

// listener and dispatch are the previous release's binaries.
func (b mixedBuildRelease) listener() string { return filepath.Join(b.dir, "envoy-listener") }
func (b mixedBuildRelease) dispatch() string { return filepath.Join(b.dir, "envoy-dispatch") }

// mixedBuildCurrentListener is this tree's envoy-listener, built once per test (cmdtest.BuildCommand).
func mixedBuildCurrentListener(t *testing.T) string {
	t.Helper()
	return cmdtest.BuildCommand(t, cmdtest.Command{Name: "envoy-listener", Dir: filepath.Join("..", "..", "cmd", "listener")})
}

// mixedBuildProvision makes what a hub holds before its first listener starts, as the operator
// makes it, each in a hold of its own workflow: every bucket a listener copies (bus.CopiedBuckets),
// which no listener creates, as `envoy-infra buckets create` does; then every other declared
// stream, ENVOY_NOTIFICATIONS among them, as `envoy-infra streams update` does (streams.Update,
// through bus.EnsureNotificationStream, which stamps the stream's identity and generation). The
// auth service answers a listener with no stream create or update, so a listener of either build
// starts only over a stream the operator made so. Each hold is set by h.held, whose protected
// streams take the build's own configuration, so streams update finds nothing to change in one it
// made first. Each is released with its journal complete, so the row is released again when this
// returns.
func (h *stack) mixedBuildProvision() {
	h.t.Helper()
	ctx := context.Background()
	operatorInHold(h.t, h, holdrecord.WorkflowBucketsCreate, func(nc *nats.Conn, _ jetstream.JetStream) {
		legacy, err := nc.JetStream()
		require.NoError(h.t, err)
		for _, bucket := range bus.CopiedBuckets() {
			_, err := bus.CreateCopiedBucket(ctx, nc, legacy, bucket, 1)
			require.NoError(h.t, err, "create the copied bucket %s", bucket)
		}
	})
	operatorInHold(h.t, h, holdrecord.WorkflowStreamsUpdate, func(nc *nats.Conn, _ jetstream.JetStream) {
		held := func(context.Context) (natsgrant.OperatorPhase, error) { return natsgrant.OperatorPhaseHeld, nil }
		report, err := streams.Update(ctx, nc, streamset.Settings{Replicas: 1}, held)
		require.NoError(h.t, err, "streams update")
		require.Empty(h.t, report.Unknown, "streams update read something it could not: %v", report.Unknown)
		require.Empty(h.t, report.Failures, "streams update failed: %v", report.Failures)
	})
}

// mixedBuildListenerTokenFile writes the platform token of the stack's listener entry machine (one
// of holdtest.ListenerMachines) to a file of the test's, which NATS_SERVICE_TOKEN_FILE names: the
// listener presents it as natscred.Service does, as user "service", which is the credential the
// stack's ListenerCredential sends.
func (h *stack) mixedBuildListenerTokenFile(machine string) string {
	h.t.Helper()
	var o nats.Options
	require.NoError(h.t, h.s.ListenerCredential(machine)(&o))
	require.Equal(h.t, "service", o.User, "the stack's listener credential is no service user")
	path := filepath.Join(h.t.TempDir(), "listener-"+machine+".token")
	require.NoError(h.t, os.WriteFile(path, []byte(o.Password), 0o600))
	return path
}

// mixedBuildAPIToken is the /v1 bearer of every listener the harness starts.
const mixedBuildAPIToken = "mixed-build-api-token"

// mixedBuildListener is a listener process the harness started, as the stack's listener entry
// machine, serving /v1 on the loopback port port.
type mixedBuildListener struct {
	*cmdtest.Process
	machine string
	port    int
}

// mixedBuildStartListener starts binary, either build's envoy-listener, as machine (one of
// holdtest.ListenerMachines) against the stack's cluster, with env added. It connects as that
// listener entry through NATS_SERVICE_TOKEN_FILE, and serves on a loopback port the test holds
// until it serves (cmdtest.HoldPort), as cmd/listener's own process rows start it
// (shutdown_test.go, startListenerProcessAsIs). The stack must hold what it copies already
// (mixedBuildProvision). It is killed when the test ends if it still runs.
func (h *stack) mixedBuildStartListener(binary, machine string, env ...string) *mixedBuildListener {
	h.t.Helper()
	held := cmdtest.HoldPort(h.t)
	cmd := exec.Command(binary)
	cmd.Env = append([]string{
		"PORT=" + strconv.Itoa(held.Port),
		"ENVOY_LISTEN_HOST=127.0.0.1",
		"ENVOY_MACHINE_ID=" + machine,
		"NATS_URLS=" + h.s.Cluster.URL(),
		"ENVOY_API_TOKEN=" + mixedBuildAPIToken,
		bus.ServiceTokenFileEnvVar + "=" + h.mixedBuildListenerTokenFile(machine),
	}, env...)
	held.Pass(cmd, "ENVOY_LISTEN_FD")
	name := fmt.Sprintf("the listener %s (%s)", machine, binary)
	return &mixedBuildListener{Process: cmdtest.Start(h.t, name, cmd), machine: machine, port: held.Port}
}

// mixedBuildLines are the JSON log lines of output whose msg is msg, in order: every listener build
// logs one JSON object a line.
func mixedBuildLines(output, msg string) []map[string]any {
	var lines []map[string]any
	for line := range strings.SplitSeq(output, "\n") {
		var record map[string]any
		if json.Unmarshal([]byte(line), &record) == nil && record["msg"] == msg {
			lines = append(lines, record)
		}
	}
	return lines
}

// waitLine waits up to within for the listener's first log line whose msg is msg and answers it. It
// fails the test when the process exits first or within passes, with the whole output.
func (l *mixedBuildListener) waitLine(t *testing.T, msg string, within time.Duration) map[string]any {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		if lines := mixedBuildLines(l.Output.String(), msg); len(lines) > 0 {
			return lines[0]
		}
		select {
		case <-l.Exited:
			t.Fatalf("the listener %s exited before it logged %q:\n%s", l.machine, msg, l.Output.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("the listener %s logged no %q in %s:\n%s", l.machine, msg, within, l.Output.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// stop stops the listener as a deploy does, SIGTERM and its ordered shutdown, and waits for it.
func (l *mixedBuildListener) stop(t *testing.T) {
	t.Helper()
	l.Terminate(t)
	l.WaitExit(t, "SIGTERM")
}

// register registers sessionID with the listener for consumer delivery of topics
// (POST /v1/interests/subscribe, listenerwire.SubscribeRequest), and answers the status and the
// decoded body. The body is decoded as a map, since the two builds' answers need not have one shape.
func (l *mixedBuildListener) register(t *testing.T, sessionID string, topics ...string) (int, map[string]any) {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"session_id": sessionID, "topics": topics, "capabilities": []string{},
		"delivery": map[string]any{"consumer": true, "epoch": "mixed-build"},
	})
	require.NoError(t, err)
	request, err := http.NewRequest(http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/v1/interests/subscribe", l.port), bytes.NewReader(body))
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer "+mixedBuildAPIToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err, "register %s with the listener %s", sessionID, l.machine)
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	var answer map[string]any
	require.NoError(t, json.Unmarshal(raw, &answer), "the listener %s answered the registration of %s with %q", l.machine, sessionID, raw)
	return response.StatusCode, answer
}

// mixedBuildListenerConn connects to the stack as its listener entry machine, whose grant
// (natsgrant.Listener) creates, reads, pulls and deletes any consumer of ENVOY_NOTIFICATIONS and
// publishes every notification subject: what any listener build may do on the stream.
func (h *stack) mixedBuildListenerConn(machine string) (*nats.Conn, jetstream.JetStream) {
	h.t.Helper()
	nc, err := h.s.Connect(h.s.Cluster.URL(), h.s.ListenerCredential(machine), nats.Name("mixed-build test as listener "+machine))
	require.NoError(h.t, err)
	js, err := jetstream.New(nc)
	require.NoError(h.t, err)
	return nc, js
}

// mixedBuildSessionConsumers is this tree's session consumer calls (bus.ConnectSessionConsumers),
// the ones the listener's registration and relay make, in mode, connected as the stack's listener
// entry machine through NATS_SERVICE_TOKEN_FILE as the listener process connects. It is closed when
// the test ends.
func (h *stack) mixedBuildSessionConsumers(machine string, mode selection.Mode) *bus.SessionConsumers {
	h.t.Helper()
	file := h.mixedBuildListenerTokenFile(machine)
	lookup := func(name string) (string, bool) {
		if name == bus.ServiceTokenFileEnvVar {
			return file, true
		}
		return "", false
	}
	sc, err := bus.ConnectSessionConsumers(h.s.Cluster.URLs(), 0, slog.New(slog.DiscardHandler),
		bus.WithEnvironment(lookup), bus.WithConsumerNames(mode))
	require.NoError(h.t, err, "connect this tree's session consumer calls as the listener %s in mode %s", machine, mode)
	h.t.Cleanup(sc.Close)
	return sc
}

// mixedBuildPullSeqs pulls n messages from consumer name, sessionID's, with this tree's pull
// (bus.SessionConsumers.Pull), acking each when ack is set, and answers their stream sequences in
// the order they arrived. It fails the test when n do not arrive within 30 seconds.
func mixedBuildPullSeqs(t *testing.T, sc *bus.SessionConsumers, sessionID, name string, n int, ack bool) []uint64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pull, err := sc.Pull(ctx, sessionID, name)
	require.NoError(t, err, "pull %s", name)
	defer pull.Stop()
	seqs := make([]uint64, 0, n)
	for len(seqs) < n {
		msg, err := pull.Next(ctx)
		require.NoError(t, err, "message %d of %d from %s (had %v)", len(seqs)+1, n, name, seqs)
		if ack {
			require.NoError(t, msg.Ack(), "ack %d on %s", msg.StreamSeq, name)
		}
		seqs = append(seqs, msg.StreamSeq)
	}
	return seqs
}

// mixedBuildRun runs `envoy-infra consumer-names <args>` and fails the test unless it exits want.
func (h *stack) mixedBuildRun(want int, args ...string) string {
	h.t.Helper()
	code, stdout, stderr := h.run(args...)
	require.Equal(h.t, want, code, "envoy-infra consumer-names %s exited %d:\n%s%s", strings.Join(args, " "), code, stdout, stderr)
	return stdout
}

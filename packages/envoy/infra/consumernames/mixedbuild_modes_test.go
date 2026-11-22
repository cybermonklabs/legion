//go:build twobuild

package consumernames

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/writerhold"
	"github.com/sjawhar/envoy/infra/writerhold/holdtest"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/holdrecord"
)

// mixedBuildModesMachine is the listener entry this file's listener process connects as, apart from
// the entry mixedBuildFixedSessions makes the sessions' consumers through.
var mixedBuildModesMachine = holdtest.ListenerMachines[2]

// mixedBuildModeStop is the line a fixed-mode listener logs as a connection made again finds the
// cutover done (cmd/listener/main.go, the marker gate's callback, bus.Client.GateOnMarker), before
// it exits.
const mixedBuildModeStop = "session consumer naming stops the listener"

// The two modes across the window (plan §2 "Two modes", §3 step 3), the listener's part: a creator
// in the fixed mode that only reconnects across the hold is refused while the hold is set - the hold
// set restarts every hub server, and the auth service refuses its reconnect - and at its first
// connect after convert has run and the hold is released, it reads the marker done and exits,
// naming ENVOY_CONSUMER_NAMES, so a reconnect never turns a creator to fresh and the re-read at
// every connect stops it. Control: a build that reads its marker only at start (the same listener
// with the marker gate taken out by an overlay) keeps running after the release and serves a
// fixed name, s-<id>, to a session that registers.
//
// This row is the listener's part. The previous release's listener and Dispatch after a rollback
// are TestRestoreThenThePreviousBuildDeliversEveryMessage (restore_previous_build_test.go). No row
// yet runs envoy-dispatch in both builds across the window, or a build that falls back to fixed
// when its marker is missing.
func TestTheTwoModesAcrossTheWindow(t *testing.T) {
	mixedBuildPreviousRelease(t)

	t.Run("a fixed creator that only reconnects across the hold exits at its first connect after convert", func(t *testing.T) {
		h := newStack(t)
		listener := h.mixedBuildModesListener(mixedBuildCurrentListener(t))
		h.mixedBuildModesWindow(listener)

		select {
		case <-listener.Exited:
		case <-time.After(90 * time.Second):
			t.Fatalf("the fixed listener still runs 90 s after the release, over a converted bucket:\n%s", listener.Output.String())
		}
		lines := mixedBuildModeStops(listener.Output.String())
		require.NotEmpty(t, lines, "the fixed listener exited without naming the setting:\n%s", listener.Output.String())
		require.Contains(t, lines[0]["error"], "ENVOY_CONSUMER_NAMES=fixed", "the exit names the setting")
		require.Equal(t, 1, listener.Cmd.ProcessState.ExitCode())
		t.Logf("the fixed listener's exit after the release: %v", lines[0])
	})

	t.Run("control: a build that reads its marker only at start serves fixed names after the release", func(t *testing.T) {
		h := newStack(t)
		listener := h.mixedBuildModesListener(mixedBuildStartOnlyListener(t))
		h.mixedBuildModesWindow(listener)

		select {
		case <-listener.Exited:
			t.Fatalf("the start-only build exited after the release; it reads no marker past its start:\n%s", listener.Output.String())
		case <-time.After(30 * time.Second):
		}
		session := "mixed-after-the-window"
		code, answer := listener.register(t, session, contracts.LegacyAgentSubject(session))
		require.Equal(t, 200, code, "the start-only build refused a registration after the release: %v", answer)
		nc, _ := h.mixedBuildListenerConn(mixedBuildNewMachine)
		info, err := consumerInfo(context.Background(), nc, notifications, sessionFixedPrefix+session)
		require.NoError(t, err)
		require.NotNil(t, info, "the start-only build made no fixed consumer s-%s after the window", session)
		require.Empty(t, mixedBuildModeStops(listener.Output.String()), "the start-only build logged the conversion stop")
	})
}

// mixedBuildModesListener provisions the stack and starts binary as mixedBuildModesMachine in the
// fixed mode, the default, with no selection bucket, and returns it once its /v1 is open. The window
// converts the listener's own durable, listener-<machine>. The row makes no bare session consumer:
// a session with no record in envoy_sessions is one the listener's own startup sweep deletes, in a
// race with prepare's listing (prepare's threshold raise then meets a consumer that is gone).
func (h *stack) mixedBuildModesListener(binary string) *mixedBuildListener {
	h.t.Helper()
	h.mixedBuildProvision()
	listener := h.mixedBuildStartListener(binary, mixedBuildModesMachine, "ENVOY_CONSUMER_NAMES=fixed")
	listener.waitLine(h.t, "envoy-listener /v1 open", 3*time.Minute)
	return listener
}

// mixedBuildModesWindow runs the window around a listener that keeps running through it: prepare,
// the hold set with the restart of every hub server that `hold set` makes (so the listener's
// connections drop and its reconnects meet the held row), a check that the listener is refused and
// still runs, then create-bucket, convert, the census, and the release.
func (h *stack) mixedBuildModesWindow(listener *mixedBuildListener) {
	h.t.Helper()
	id := h.prepared(holdrecord.WorkflowCutover)
	h.mixedBuildRun(0, actionPrepare, "--hold", id)
	h.held(id)
	before := len(listener.Output.String())
	for i := range h.s.Cluster.Servers {
		h.s.Cluster.Servers[i].Stop(h.t)
		h.s.Cluster.Servers[i].Start(h.t)
	}
	// testnats' AwaitReady connects with no credential, which the callout refuses, so the row waits
	// until JetStream answers the operator's info of every protected stream again, as the held
	// re-read reads them: present, or JetStream's own "not found" (the selection bucket before
	// create-bucket), and never a request that times out.
	require.Eventually(h.t, func() bool {
		nc, err := h.s.Connect(h.s.Cluster.URL(), h.s.OperatorCredential()...)
		if err != nil {
			return false
		}
		defer nc.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		for _, stream := range writerhold.ProtectedStreams() {
			var api *jetstream.APIError
			if _, err := streamInfo(ctx, nc, stream); err != nil && !errors.As(err, &api) {
				return false
			}
		}
		return true
	}, 2*time.Minute, time.Second, "JetStream never answered the operator's stream infos after the hub's restart")
	require.Eventually(h.t, func() bool {
		return strings.Contains(strings.ToLower(listener.Output.String()[before:]), "authorization violation")
	}, 60*time.Second, 200*time.Millisecond, "the held hub never refused the listener's reconnect:\n%s", listener.Output.String())
	for line := range strings.SplitSeq(listener.Output.String()[before:], "\n") {
		if strings.Contains(strings.ToLower(line), "authorization violation") {
			h.t.Logf("the held hub refused the listener's reconnect: %s", line)
			break
		}
	}
	select {
	case <-listener.Exited:
		h.t.Fatalf("the listener exited while the hold was set:\n%s", listener.Output.String())
	default:
	}
	h.mixedBuildRun(0, actionCreateBucket, "--hold", id)
	h.mixedBuildRun(0, actionConvert, "--hold", id)
	h.mixedBuildCensus(id, "--family", familySession)
	require.NoError(h.t, h.release(id), "release the cutover hold")
}

// mixedBuildModeStops are the JSON lines of output whose msg begins with mixedBuildModeStop: the
// line carries the refusal after the prefix and again as its error field.
func mixedBuildModeStops(output string) []map[string]any {
	var lines []map[string]any
	for line := range strings.SplitSeq(output, "\n") {
		var record map[string]any
		if json.Unmarshal([]byte(line), &record) != nil {
			continue
		}
		if msg, _ := record["msg"].(string); strings.HasPrefix(msg, mixedBuildModeStop) {
			lines = append(lines, record)
		}
	}
	return lines
}

// mixedBuildStartOnlyListener is this tree's listener with its marker gates taken out, so it reads
// its marker at start alone: the build the control runs. The overlay replaces
// cmd/listener/consumer_naming.go with a copy whose guardFixedNaming shuts no connection's gate
// (bus.Client.GateOnMarker, on the session consumers' connection and on the durable's) and keeps
// the start's read of each (awaitFixedNaming).
func mixedBuildStartOnlyListener(t *testing.T) string {
	t.Helper()
	source := filepath.Join("..", "..", "cmd", "listener", "consumer_naming.go")
	text, err := os.ReadFile(source)
	require.NoError(t, err)
	const gate = "\t\tgated.connection.GateOnMarker(converted)\n"
	require.Equal(t, 1, strings.Count(string(text), gate), "consumer_naming.go's guardFixedNaming no longer gates as the overlay expects")
	changed := strings.Replace(string(text), gate, "\t\t_ = converted\n", 1)
	dir := t.TempDir()
	copyPath := filepath.Join(dir, "consumer_naming.go")
	require.NoError(t, os.WriteFile(copyPath, []byte(changed), 0o600))
	absSource, err := filepath.Abs(source)
	require.NoError(t, err)
	overlay := filepath.Join(dir, "overlay.json")
	require.NoError(t, os.WriteFile(overlay, []byte(`{"Replace":{"`+absSource+`":"`+copyPath+`"}}`), 0o600))
	return cmdtest.BuildCommand(t, cmdtest.Command{Name: "envoy-listener-start-only", Dir: filepath.Join("..", "..", "cmd", "listener"),
		Flags: []string{"-overlay", overlay}})
}

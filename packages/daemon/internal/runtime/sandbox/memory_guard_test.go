package sandbox

import (
	"bufio"
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// rigLauncherHello is the hello role's launcher on the pod of sandbox name sends, with the token
// the rig's role Secret holds.
func rigLauncherHello(g *rig, name, podUID string, role claim.Role) shimwire.LauncherHello {
	return shimwire.LauncherHello{Token: string(g.secret(roleSecretName(name, role)).Data[LauncherTokenFile]),
		Sandbox: name, Role: string(role), PodUID: podUID, LauncherID: "l-" + string(role)}
}

// Every role's launcher of an issue pod is answered with the memory guard's settings the runtime
// was given, and the controller's launcher with none: its pod's guard settings are a block this
// runtime does not carry. A refused hello is answered with the zero acknowledgement.
func TestMemoryGuardTheResolverHandsTheSettingsToAnIssuePodsLaunchersAlone(t *testing.T) {
	settings := shimwire.LauncherMemoryGuard{ActPercent: 70, FloorPercent: 20, WaitSeconds: 9, IntervalMilliseconds: 250}
	g := newRig(t, nil, withOptions(func(o *Options) { o.MemoryGuard = &settings }))
	controller := g.spawn(controllerSpec(t))
	issue := g.spawn(workerSpec(t))
	accept := g.r.LauncherResolver()

	for _, role := range claim.Roles {
		handler, ack, reason := accept(rigLauncherHello(g, issue.Sandbox.Name, issue.Sandbox.PodUID, role))
		if handler == nil {
			t.Fatalf("the issue pod's %s launcher was refused: %s", role, reason)
		}
		if ack.MemoryGuard == nil || *ack.MemoryGuard != settings {
			t.Fatalf("the issue pod's %s launcher was answered %+v (memory guard %+v), want the settings %+v", role, ack, ack.MemoryGuard, settings)
		}
	}
	handler, ack, reason := accept(rigLauncherHello(g, controller.Sandbox.Name, controller.Sandbox.PodUID, claim.RoleController))
	if handler == nil {
		t.Fatalf("the controller's launcher was refused: %s", reason)
	}
	if ack.MemoryGuard != nil {
		t.Fatalf("the controller's launcher was answered the memory guard settings %+v, want none", *ack.MemoryGuard)
	}
	refused := rigLauncherHello(g, issue.Sandbox.Name, issue.Sandbox.PodUID, claim.RoleTester)
	refused.Token = "not-the-token"
	if handler, ack, reason := accept(refused); handler != nil || reason == "" || ack.MemoryGuard != nil {
		t.Fatalf("a hello with a stale token was answered handler=%v ack=%+v reason=%q, want refused with the zero acknowledgement", handler, ack, reason)
	}
}

// A deployment that turned the guard off (Options.MemoryGuard nil) answers an issue pod's launcher
// with no settings, which the launcher reads as "run no guard".
func TestMemoryGuardTheResolverHandsNoSettingsWhenTheGuardIsOff(t *testing.T) {
	g := newRig(t, nil)
	issue := g.spawn(workerSpec(t))
	handler, ack, reason := g.r.LauncherResolver()(rigLauncherHello(g, issue.Sandbox.Name, issue.Sandbox.PodUID, claim.RoleTester))
	if handler == nil {
		t.Fatalf("the issue pod's tester launcher was refused: %s", reason)
	}
	if ack.MemoryGuard != nil {
		t.Fatalf("the launcher was answered the settings %+v, want none from a runtime given none", *ack.MemoryGuard)
	}
}

// memoryKillCall is one call of a runtime's MemoryGuardKilled receiver, and whether the context it
// was called with carried a deadline.
type memoryKillCall struct {
	issue   string
	token   claim.Token
	podUID  string
	report  shimwire.LauncherMemoryKill
	bounded bool
}

// recordedMemoryKills is a MemoryGuardKilled receiver that records each call on the channel it
// answers.
func recordedMemoryKills() (runtime.MemoryGuardKilled, <-chan memoryKillCall) {
	calls := make(chan memoryKillCall, 8)
	return func(ctx context.Context, issue string, token claim.Token, podUID string, report shimwire.LauncherMemoryKill) error {
		_, bounded := ctx.Deadline()
		calls <- memoryKillCall{issue: issue, token: token, podUID: podUID, report: report, bounded: bounded}
		return nil
	}, calls
}

// memoryKillReport is a kill report the frame's own rule accepts.
func memoryKillReport(sequence uint64) shimwire.LauncherMemoryKill {
	return shimwire.LauncherMemoryKill{Guard: "guard-1", Sequence: sequence, PID: 4242, Command: "python3 grow.py",
		SizeBytes: 812 << 20, ReadingBytes: 3 << 30, CounterBytes: 3<<30 + 1<<28, LimitBytes: 4 << 30, At: rigNow}
}

// serveLauncherPipe runs session's frame loop on one end of a pipe and answers a writer on the
// other, the launcher's, and a channel closed once the loop has returned. A write to the pipe
// returns once the loop has read it, which is after the loop has handled every frame before it.
func serveLauncherPipe(t *testing.T, session *launcherSession) (*shimwire.Writer, <-chan struct{}) {
	t.Helper()
	server, peer := net.Pipe()
	t.Cleanup(func() { _ = server.Close(); _ = peer.Close() })
	done := make(chan struct{})
	go func() {
		defer close(done)
		session.ServeLauncher(server, bufio.NewReader(server), shimwire.NewWriter(server))
	}()
	return shimwire.NewWriter(peer), done
}

// sendFrame writes frame on the launcher's end, failing the test where the daemon has closed the
// connection.
func sendFrame(t *testing.T, w *shimwire.Writer, frame shimwire.Frame) {
	t.Helper()
	if err := w.WriteFrame(frame); err != nil {
		t.Fatalf("write %s: %v, want the connection open", frame.FrameType(), err)
	}
}

// nextMemoryKillCall is the receiver's next call, failing the test if none comes.
func nextMemoryKillCall(t *testing.T, calls <-chan memoryKillCall) memoryKillCall {
	t.Helper()
	select {
	case call := <-calls:
		return call
	case <-time.After(5 * time.Second):
		t.Fatal("the receiver was never called with the report")
		return memoryKillCall{}
	}
}

// A valid kill report reaches the daemon's receiver with the session's own issue, role and pod,
// under a bounded context, and a report the frame's own rule refuses is dropped with a warning
// naming the launcher: the connection stays open after it, and the report after it is read.
func TestMemoryGuardServeLauncherHandsAKillToTheReceiverWithTheSessionsOwnIssueRoleAndPod(t *testing.T) {
	var logs lockedLog
	killed, calls := recordedMemoryKills()
	launchers := newLaunchers(nil, nil, nil, killed, slogTo(&logs))
	session := launchers.accept(workerToken, testTree, rigNow, shimwire.LauncherHello{PodUID: "pod-1", LauncherID: "l-1"})
	w, done := serveLauncherPipe(t, session)

	noGuard := memoryKillReport(1)
	noGuard.Guard = ""
	sendFrame(t, w, noGuard)
	first := memoryKillReport(1)
	sendFrame(t, w, first)
	if call := nextMemoryKillCall(t, calls); call != (memoryKillCall{issue: testTree, token: workerToken, podUID: "pod-1", report: first, bounded: true}) {
		t.Fatalf("the receiver was called with %+v, want the session's %s, %s and pod-1, the report %+v, and a bounded context", call, testTree, workerToken, first)
	}

	tooLong := memoryKillReport(2)
	tooLong.Command = strings.Repeat("x", shimwire.MemoryKillCommandBytes+1)
	sendFrame(t, w, tooLong)
	second := memoryKillReport(2)
	sendFrame(t, w, second)
	if call := nextMemoryKillCall(t, calls); call.report != second || call.token != workerToken {
		t.Fatalf("the receiver was called with %+v, want the second valid report %+v", call, second)
	}
	select {
	case call := <-calls:
		t.Fatalf("the receiver was called again with %+v, want the two refused reports dropped", call)
	case <-done:
		t.Fatal("the frame loop ended, want the connection open after each report")
	default:
	}
	text := logs.String()
	if got := strings.Count(text, "the pod's launcher sent a kill report the daemon cannot apply"); got != 2 {
		t.Fatalf("log = %q, want a warning for each of the two refused reports", text)
	}
	if !strings.Contains(text, "launcher="+string(workerToken)) || !strings.Contains(text, "carries no guard") {
		t.Fatalf("log = %q, want the warning to name the launcher and the refusal", text)
	}
}

// A kill report from a launcher of no issue (the controller's pod, whose role has no issue to
// hold a note) and one a runtime with no receiver reads are dropped with a warning naming the
// launcher, and the connection stays open: the launcher's frames after them are read.
func TestMemoryGuardServeLauncherDropsAKillNoIssueOrNoReceiverCanTake(t *testing.T) {
	for _, tc := range []struct {
		name     string
		receiver bool
		token    claim.Token
		issue    string
		warning  string
	}{
		{"a launcher of no issue", true, controllerToken, "", "a launcher of no issue reported a kill"},
		{"a runtime with no receiver", false, workerToken, testTree, "this runtime has no receiver for it"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs lockedLog
			var killed runtime.MemoryGuardKilled
			var calls <-chan memoryKillCall
			if tc.receiver {
				killed, calls = recordedMemoryKills()
			}
			launchers := newLaunchers(nil, nil, nil, killed, slogTo(&logs))
			session := launchers.accept(tc.token, tc.issue, rigNow, shimwire.LauncherHello{PodUID: "pod-1", LauncherID: "l-1"})
			w, done := serveLauncherPipe(t, session)

			sendFrame(t, w, memoryKillReport(1))
			// Each write returns once the loop has read it, so the second state's write returns only
			// once the report and the first state have been handled.
			sendFrame(t, w, shimwire.LauncherState{})
			sendFrame(t, w, shimwire.LauncherState{})

			select {
			case call := <-calls:
				t.Fatalf("the receiver was called with %+v, want the report dropped", call)
			case <-done:
				t.Fatal("the frame loop ended, want the connection open after the report")
			default:
			}
			if _, connected := launchers.state(tc.token, "pod-1"); !connected {
				t.Fatal("the launcher's state after the report was not read, want the connection serving")
			}
			text := logs.String()
			if !strings.Contains(text, tc.warning) || !strings.Contains(text, "launcher="+string(tc.token)) || !strings.Contains(text, "sequence=1") {
				t.Fatalf("log = %q, want a warning %q naming the launcher and the report", text, tc.warning)
			}
		})
	}
}

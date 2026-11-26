package sandbox

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"reflect"
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

// issueGuardSettings and controllerGuardSettings are a deployment's two memory guard blocks, every
// number of one different from the other's, so an answer carrying one kind's settings is never
// mistaken for the other's.
var (
	issueGuardSettings      = shimwire.LauncherMemoryGuard{ActPercent: 70, FloorPercent: 20, WaitSeconds: 9, IntervalMilliseconds: 250}
	controllerGuardSettings = shimwire.LauncherMemoryGuard{ActPercent: 80, FloorPercent: 15, WaitSeconds: 8, IntervalMilliseconds: 400}
)

// guardText is a memory guard member as a failure names it: its settings, or none.
func guardText(g *shimwire.LauncherMemoryGuard) string {
	if g == nil {
		return "none"
	}
	return fmt.Sprintf("%+v", *g)
}

// sameGuard reports whether got carries want's settings, none for none included.
func sameGuard(got, want *shimwire.LauncherMemoryGuard) bool {
	return (got == nil) == (want == nil) && (got == nil || *got == *want)
}

// resolvedGuards are the memory guard settings g's resolver answers the launchers of issue, an
// issue pod, with, by role, and the controller's launcher on controller, the controller's pod. A
// refused hello fails the test.
func resolvedGuards(t *testing.T, g *rig, issue, controller runtime.Locator) (map[claim.Role]*shimwire.LauncherMemoryGuard, *shimwire.LauncherMemoryGuard) {
	t.Helper()
	accept := g.r.LauncherResolver()
	issueGuards := map[claim.Role]*shimwire.LauncherMemoryGuard{}
	for _, role := range claim.Roles {
		handler, ack, reason := accept(rigLauncherHello(g, issue.Sandbox.Name, issue.Sandbox.PodUID, role))
		if handler == nil {
			t.Fatalf("the issue pod's %s launcher was refused: %s", role, reason)
		}
		issueGuards[role] = ack.MemoryGuard
	}
	handler, ack, reason := accept(rigLauncherHello(g, controller.Sandbox.Name, controller.Sandbox.PodUID, claim.RoleController))
	if handler == nil {
		t.Fatalf("the controller's launcher was refused: %s", reason)
	}
	return issueGuards, ack.MemoryGuard
}

// With different numbers in the two blocks, every role's launcher of an issue pod is answered with
// the issue pods' settings and the controller's launcher with the controller pod's, and neither
// ever with the other kind's. A refused hello is answered with the zero acknowledgement.
func TestMemoryGuardTheResolverHandsEachPodKindItsOwnSettings(t *testing.T) {
	t.Parallel()
	issueSettings, controllerSettings := issueGuardSettings, controllerGuardSettings
	g := newRig(t, nil, withOptions(func(o *Options) {
		o.MemoryGuard, o.ControllerMemoryGuard = &issueSettings, &controllerSettings
	}))
	controller := g.spawn(controllerSpec(t))
	issue := g.spawn(workerSpec(t))

	issueGuards, controllerGuard := resolvedGuards(t, g, issue, controller)
	for role, got := range issueGuards {
		if !sameGuard(got, &issueGuardSettings) {
			t.Errorf("the issue pod's %s launcher was answered the memory guard %s, want the issue pods' %+v and never the controller pod's %+v",
				role, guardText(got), issueGuardSettings, controllerGuardSettings)
		}
	}
	if !sameGuard(controllerGuard, &controllerGuardSettings) {
		t.Errorf("the controller's launcher was answered the memory guard %s, want the controller pod's %+v and never the issue pods' %+v",
			guardText(controllerGuard), controllerGuardSettings, issueGuardSettings)
	}
	refused := rigLauncherHello(g, issue.Sandbox.Name, issue.Sandbox.PodUID, claim.RoleTester)
	refused.Token = "not-the-token"
	if handler, ack, reason := g.r.LauncherResolver()(refused); handler != nil || reason == "" || ack.MemoryGuard != nil {
		t.Fatalf("a hello with a stale token was answered handler=%v ack=%+v reason=%q, want refused with the zero acknowledgement", handler, ack, reason)
	}
}

// A deployment that turned one kind's guard off (its Options member nil) answers that kind's
// launchers with no settings, which they read as "run no guard", and still answers the other
// kind's launchers with that kind's own settings; with both off, no launcher is handed any.
func TestMemoryGuardTheResolverLeavesOneKindOnWhenTheOtherIsOff(t *testing.T) {
	t.Parallel()
	issueSettings, controllerSettings := issueGuardSettings, controllerGuardSettings
	for _, tc := range []struct {
		name              string
		issue, controller *shimwire.LauncherMemoryGuard
	}{
		{"the issue pods' guard off", nil, &controllerSettings},
		{"the controller pod's guard off", &issueSettings, nil},
		{"both off", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newRig(t, nil, withOptions(func(o *Options) { o.MemoryGuard, o.ControllerMemoryGuard = tc.issue, tc.controller }))
			controller := g.spawn(controllerSpec(t))
			issue := g.spawn(workerSpec(t))

			issueGuards, controllerGuard := resolvedGuards(t, g, issue, controller)
			for role, got := range issueGuards {
				if !sameGuard(got, tc.issue) {
					t.Errorf("the issue pod's %s launcher was answered the memory guard %s, want %s", role, guardText(got), guardText(tc.issue))
				}
			}
			if !sameGuard(controllerGuard, tc.controller) {
				t.Errorf("the controller's launcher was answered the memory guard %s, want %s", guardText(controllerGuard), guardText(tc.controller))
			}
		})
	}
}

// Two configurations that differ only in the controller pod's memory guard — off, on at the
// defaults, on at other numbers — build the controller's pod with the same recorded shape
// (podShape) and the same template (podTemplate), so the pod one of them built reads as no drift
// to the other: a daemon restarted on a changed runtime.kubernetes.memory_guard.controller_pod
// replaces no pod, and the settings reach the running pod's launcher on its next hello instead.
func TestMemoryGuardAControllerPodSettingsChangeReplacesNoPod(t *testing.T) {
	t.Parallel()
	built := func(guard *shimwire.LauncherMemoryGuard) (*Runtime, podTemplate) {
		t.Helper()
		opts := goldenOptions()
		opts.ControllerMemoryGuard = guard
		r, err := configure(opts)
		if err != nil {
			t.Fatal(err)
		}
		return r, r.podTemplate(launchOf(t, r, controllerSpec(t)))
	}
	off, offTemplate := built(nil)
	for _, guard := range []*shimwire.LauncherMemoryGuard{
		{ActPercent: shimwire.MemoryGuardActDefault, FloorPercent: shimwire.MemoryGuardFloorDefault,
			WaitSeconds: shimwire.MemoryGuardWaitDefault, IntervalMilliseconds: shimwire.MemoryGuardIntervalDefault},
		&controllerGuardSettings,
	} {
		on, onTemplate := built(guard)
		if got, want := podShape(on, controllerPod{}, nil), podShape(off, controllerPod{}, nil); got != want {
			t.Errorf("the controller pod's shape with the guard at %+v is\n%s\nwant the shape with it off\n%s", *guard, got, want)
		}
		if !reflect.DeepEqual(onTemplate, offTemplate) {
			t.Errorf("the controller pod's template with the guard at %+v is\n%+v\nwant the template with it off\n%+v", *guard, onTemplate, offTemplate)
		}
		for _, pair := range []struct {
			name    string
			now     *Runtime
			running podTemplate
		}{{"off, then on", on, offTemplate}, {"on, then off", off, onTemplate}} {
			s := &sandbox{}
			s.Spec.PodTemplate = pair.running
			if drifted := pair.now.shapeDrift(s, controllerInGVisor); len(drifted) > 0 {
				t.Errorf("the controller's pod built with the guard %s at %+v reads as drifted: %q, want no drift", pair.name, *guard, drifted)
			}
		}
	}
}

// Every launcher the resolver accepts is told the daemon knows the pod's kernel's kill
// (kernelKills), with the guard's settings and with the guard turned off alike: each role's
// launcher of an issue pod beside the issue pods' settings, and the controller's launcher beside
// the controller pod's own (controller_pod's), in a gVisor project and in a microVM one alike. A
// microVM pod's launcher sends the kernel's reports on kernelKills and nothing else, and a gVisor
// controller pod's launcher reads the settings and ignores kernelKills, so a controller pod built
// under either type is answered all it reads (launcherHelloAck). Each role's launcher of an issue
// pod is also told the daemon knows a report of a process of no role (noRoleKills), whatever the
// guard's settings, and the controller's launcher never is, since no kill of its pod counts. The
// controller's pod is rendered in the project's sandbox, or the microVM case would prove nothing
// of a microVM controller. A refused hello is told none of it.
func TestKernelKillsTheResolverTellsEveryLauncher(t *testing.T) {
	t.Parallel()
	issueSettings, controllerSettings := issueGuardSettings, controllerGuardSettings
	for _, project := range []SandboxConfig{{Type: SandboxGVisor}, {Type: SandboxMicroVM, MicroVM: testMicroVM()}} {
		for _, tc := range []struct {
			name              string
			issue, controller *shimwire.LauncherMemoryGuard
		}{
			{"the guards on", &issueSettings, &controllerSettings},
			{"the guards off", nil, nil},
		} {
			t.Run(string(project.Type)+"/"+tc.name, func(t *testing.T) {
				g := newRig(t, nil, withOptions(func(o *Options) {
					o.Sandbox, o.MemoryGuard, o.ControllerMemoryGuard = project, tc.issue, tc.controller
				}))
				controller := g.spawn(controllerSpec(t))
				issue := g.spawn(workerSpec(t))
				if got := renderedSandbox(t, g.sandbox(controller.Sandbox.Name).Spec.PodTemplate); got != project.Controller() {
					t.Fatalf("the controller's pod of a %s project runs in %s, want %s", project.Type, got, project.Controller())
				}
				accept := g.r.LauncherResolver()

				for _, role := range claim.Roles {
					handler, ack, reason := accept(rigLauncherHello(g, issue.Sandbox.Name, issue.Sandbox.PodUID, role))
					if handler == nil {
						t.Fatalf("the issue pod's %s launcher was refused: %s", role, reason)
					}
					if !ack.KernelKills || !ack.NoRoleKills || !sameGuard(ack.MemoryGuard, tc.issue) {
						t.Fatalf("the issue pod's %s launcher was answered kernelKills=%t, noRoleKills=%t and the memory guard %s, want kernelKills, noRoleKills and %s",
							role, ack.KernelKills, ack.NoRoleKills, guardText(ack.MemoryGuard), guardText(tc.issue))
					}
				}
				handler, ack, reason := accept(rigLauncherHello(g, controller.Sandbox.Name, controller.Sandbox.PodUID, claim.RoleController))
				if handler == nil {
					t.Fatalf("the controller's launcher was refused: %s", reason)
				}
				if !ack.KernelKills || ack.NoRoleKills || !sameGuard(ack.MemoryGuard, tc.controller) {
					t.Fatalf("the controller's launcher was answered kernelKills=%t, noRoleKills=%t and the memory guard %s, want kernelKills, no noRoleKills and the controller pod's %s",
						ack.KernelKills, ack.NoRoleKills, guardText(ack.MemoryGuard), guardText(tc.controller))
				}
				for _, refused := range []shimwire.LauncherHello{
					rigLauncherHello(g, issue.Sandbox.Name, issue.Sandbox.PodUID, claim.RoleTester),
					rigLauncherHello(g, controller.Sandbox.Name, controller.Sandbox.PodUID, claim.RoleController),
				} {
					refused.Token = "not-the-token"
					if handler, ack, reason := accept(refused); handler != nil || reason == "" || ack != (shimwire.LauncherHelloAck{}) {
						t.Fatalf("a hello of the %s launcher with a stale token was answered handler=%v ack=%+v reason=%q, want refused with the zero acknowledgement",
							refused.Role, handler, ack, reason)
					}
				}
			})
		}
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

// controllerMemoryKillCall is one call of a runtime's ControllerMemoryGuardKilled receiver, and
// whether the context it was called with carried a deadline.
type controllerMemoryKillCall struct {
	token   claim.Token
	podUID  string
	report  shimwire.LauncherMemoryKill
	bounded bool
}

// recordedControllerMemoryKills is a ControllerMemoryGuardKilled receiver that records each call
// on the channel it answers.
func recordedControllerMemoryKills() (runtime.ControllerMemoryGuardKilled, <-chan controllerMemoryKillCall) {
	calls := make(chan controllerMemoryKillCall, 8)
	return func(ctx context.Context, token claim.Token, podUID string, report shimwire.LauncherMemoryKill) error {
		_, bounded := ctx.Deadline()
		calls <- controllerMemoryKillCall{token: token, podUID: podUID, report: report, bounded: bounded}
		return nil
	}, calls
}

// memoryKillReport is a kill report the frame's own rule accepts.
func memoryKillReport(sequence uint64) shimwire.LauncherMemoryKill {
	return shimwire.LauncherMemoryKill{Guard: "guard-1", Sequence: sequence, PID: 4242, Executable: "python3",
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
	t.Parallel()
	var logs lockedLog
	killed, calls := recordedMemoryKills()
	launchers := newLaunchers(testLeakCheck, nil, nil, killed, nil, nil, slogTo(&logs))
	session := launchers.accept(workerToken, testTree, time.Time{}, shimwire.LauncherHello{PodUID: "pod-1", LauncherID: "l-1"})
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
	tooLong.Executable = strings.Repeat("x", shimwire.MemoryKillExecutableBytes+1)
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

// A kill report from the controller's launcher, a session of no issue, reaches the controller's
// receiver with the session's own token and pod, under a bounded context, and the issue pods'
// receiver is not called: the controller's pod is of no issue, so there is no issue to name.
func TestMemoryGuardServeLauncherHandsAControllerKillToTheControllerReceiverWithTheSessionsOwnTokenAndPod(t *testing.T) {
	t.Parallel()
	var logs lockedLog
	issueKilled, issueCalls := recordedMemoryKills()
	controllerKilled, controllerCalls := recordedControllerMemoryKills()
	launchers := newLaunchers(testLeakCheck, nil, nil, issueKilled, controllerKilled, nil, slogTo(&logs))
	session := launchers.accept(controllerToken, "", time.Time{}, shimwire.LauncherHello{PodUID: "pod-c", LauncherID: "l-c"})
	w, done := serveLauncherPipe(t, session)

	report := memoryKillReport(1)
	sendFrame(t, w, report)
	var call controllerMemoryKillCall
	select {
	case call = <-controllerCalls:
	case <-time.After(5 * time.Second):
		t.Fatal("the controller's receiver was never called with the report")
	}
	if call != (controllerMemoryKillCall{token: controllerToken, podUID: "pod-c", report: report, bounded: true}) {
		t.Fatalf("the controller's receiver was called with %+v, want the session's %s and pod-c, the report %+v, and a bounded context", call, controllerToken, report)
	}
	// The write returns once the loop has read it, so the state's write returns only once the
	// report has been handled.
	sendFrame(t, w, shimwire.LauncherState{})
	select {
	case call := <-issueCalls:
		t.Fatalf("the issue pods' receiver was called with %+v, want only the controller's", call)
	case <-done:
		t.Fatal("the frame loop ended, want the connection open after the report")
	default:
	}
	if text := logs.String(); strings.Contains(text, "launcher memory guard") {
		t.Fatalf("log = %q, want no warning for a report a receiver took", text)
	}
}

// A report of a process the pod's kernel stopped reaches the daemon's receiver, which applies it
// as the intake fact, with its cause as the launcher sent it; a report whose cause the frame's own
// rule does not know is dropped with a warning naming the cause, and the connection stays open.
func TestKernelKillServeLauncherHandsTheReceiverTheReportWithItsCause(t *testing.T) {
	t.Parallel()
	var logs lockedLog
	killed, calls := recordedMemoryKills()
	launchers := newLaunchers(testLeakCheck, nil, nil, killed, nil, nil, slogTo(&logs))
	session := launchers.accept(workerToken, testTree, time.Time{}, shimwire.LauncherHello{PodUID: "pod-1", LauncherID: "l-1"})
	w, done := serveLauncherPipe(t, session)

	unknown := memoryKillReport(1)
	unknown.Cause = "oom"
	sendFrame(t, w, unknown)
	kernel := memoryKillReport(1)
	kernel.Cause = shimwire.MemoryKillCauseKernel
	sendFrame(t, w, kernel)
	if call := nextMemoryKillCall(t, calls); call != (memoryKillCall{issue: testTree, token: workerToken, podUID: "pod-1", report: kernel, bounded: true}) {
		t.Fatalf("the receiver was called with %+v, want the session's %s, %s and pod-1 and the report %+v with its cause %q",
			call, testTree, workerToken, kernel, kernel.Cause)
	}
	select {
	case call := <-calls:
		t.Fatalf("the receiver was called again with %+v, want the report of an unknown cause dropped", call)
	case <-done:
		t.Fatal("the frame loop ended, want the connection open after each report")
	default:
	}
	if text := logs.String(); !strings.Contains(text, "the pod's launcher sent a kill report the daemon cannot apply") || !strings.Contains(text, `cause is \"oom\"`) {
		t.Fatalf("log = %q, want a warning naming the unknown cause", text)
	}
}

// A report of a process of no role, of the guard's cause and of each of the kernel's, reaches the
// issue pods' receiver as any report of the session's connection does: with the session's own
// issue, role and pod, never anything the report says, and with its mark, which is what keeps the
// daemon from telling that role of a kill that was not its own.
func TestMemoryGuardServeLauncherHandsAReportOfNoRoleToTheReceiverWithItsMark(t *testing.T) {
	t.Parallel()
	var logs lockedLog
	killed, calls := recordedMemoryKills()
	launchers := newLaunchers(testLeakCheck, nil, nil, killed, nil, nil, slogTo(&logs))
	session := launchers.accept(workerToken, testTree, time.Time{}, shimwire.LauncherHello{PodUID: "pod-1", LauncherID: "l-1"})
	w, _ := serveLauncherPipe(t, session)

	for i, cause := range []shimwire.MemoryKillCause{shimwire.MemoryKillCauseGuard, shimwire.MemoryKillCauseKernel, shimwire.MemoryKillCauseKernelProtected} {
		report := memoryKillReport(uint64(i + 1))
		report.Cause, report.NoRole = cause, true
		sendFrame(t, w, report)
		if call := nextMemoryKillCall(t, calls); call != (memoryKillCall{issue: testTree, token: workerToken, podUID: "pod-1", report: report, bounded: true}) {
			t.Fatalf("the receiver was called with %+v, want the session's %s, %s and pod-1 and the report of no role %+v", call, testTree, workerToken, report)
		}
	}
	if text := logs.String(); strings.Contains(text, "launcher memory guard") {
		t.Fatalf("log = %q, want no warning for a report of no role a receiver took", text)
	}
}

// A kill report a runtime with no receiver for its pod's kind reads is dropped with a warning
// naming the launcher and the line that sets the missing receiver, and the connection stays open:
// the launcher's frames after it are read. The other kind's receiver, which the runtime has, is
// not called with it.
func TestMemoryGuardServeLauncherDropsAKillNoReceiverCanTake(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		token   claim.Token
		issue   string
		warning string
	}{
		{"an issue pod's launcher", workerToken, testTree, "this runtime has no receiver for it, which is dropped; " +
			"the daemon sets one in internal/daemon/kubernetes.go's sandboxRuntime (opts.MemoryGuardKilled)"},
		{"the controller's launcher", controllerToken, "", "this runtime has no receiver for it, which is dropped; " +
			"the daemon sets one in internal/daemon/kubernetes.go's sandboxRuntime (opts.ControllerMemoryGuardKilled)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs lockedLog
			// The runtime has the receiver of the other kind alone.
			var issueKilled runtime.MemoryGuardKilled
			var controllerKilled runtime.ControllerMemoryGuardKilled
			var issueCalls <-chan memoryKillCall
			var controllerCalls <-chan controllerMemoryKillCall
			if tc.issue == "" {
				issueKilled, issueCalls = recordedMemoryKills()
			} else {
				controllerKilled, controllerCalls = recordedControllerMemoryKills()
			}
			launchers := newLaunchers(testLeakCheck, nil, nil, issueKilled, controllerKilled, nil, slogTo(&logs))
			session := launchers.accept(tc.token, tc.issue, time.Time{}, shimwire.LauncherHello{PodUID: "pod-1", LauncherID: "l-1"})
			w, done := serveLauncherPipe(t, session)

			sendFrame(t, w, memoryKillReport(1))
			// Each write returns once the loop has read it, so the second state's write returns only
			// once the report and the first state have been handled.
			sendFrame(t, w, shimwire.LauncherState{})
			sendFrame(t, w, shimwire.LauncherState{})

			select {
			case call := <-issueCalls:
				t.Fatalf("the issue pods' receiver was called with %+v, want the report dropped", call)
			case call := <-controllerCalls:
				t.Fatalf("the controller's receiver was called with %+v, want the report dropped", call)
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

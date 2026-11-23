package sandbox

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// reconnectBoot is the boot timeout of the runtimes below: the start bound a kept pod's launch
// gives its launcher (startBound), long enough that a loaded machine never outruns the reconnect a
// row makes, and with the termination grace the bound stopIssuedChild waits for a launcher to stop
// the generation an unanswered Start may have begun.
const reconnectBoot = 15 * time.Second

// A launch into a kept pod whose role launcher's connection dropped before the launch began
// starts the role on that launcher once it says hello again during the launch, from the same pod
// on a new connection: the launch waits for it (awaitRoleLauncher), and the Start goes to the
// reconnected launcher alone, never to the connection that dropped. A launcher process that
// restarted reports no child; one whose connection alone dropped reports the generation it still
// runs, which the launch stops first (TestANewGenerationStopsTheEarlierOneFirst).
func TestALaunchStartsItsRoleOnALauncherThatReconnectsDuringIt(t *testing.T) {
	for _, tc := range []struct {
		name string
		// running is the generation the reconnected launcher's first state reports running, 0 for
		// none; want is every command it receives.
		running uint64
		want    string
	}{
		{name: "a launcher process that restarted", running: 0, want: "start 2"},
		{name: "a launcher whose connection alone dropped", running: 1, want: "stop 1, start 2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newRig(t, nil, withOptions(func(o *Options) { o.BootTimeout = reconnectBoot }))
			first := g.spawn(workerSpec(t))
			dropLauncher(t, g, workerToken)
			if _, connected := g.r.launchers.state(workerToken, first.Sandbox.PodUID); connected {
				t.Fatal("the tester's launcher still reads connected after its connection dropped")
			}

			g.clearActions()
			result := spawnAsync(g, nextGeneration(workerSpec(t), 2))
			// The launch reads the role's launcher binding (launcherBound) once it has decided to
			// keep the pod and before it sends a launcher anything, so from then on it is under way
			// with the launcher away.
			binding := roleSecretName(first.Sandbox.Name, claim.RoleTester)
			testwait.Eventually(t, "the launch to read the tester's launcher binding", func() bool {
				return slices.ContainsFunc(g.requests(), func(a action) bool {
					return a.verb == "get" && a.resource == "secrets" && a.name == binding
				})
			})
			select {
			case early := <-result:
				t.Fatalf("the launch returned before the launcher reconnected: %+v, %v", early.loc, early.err)
			default:
			}
			redialed := redial(t, g, workerToken, tc.running, false)

			done := awaitLaunch(t, result)
			if done.err != nil {
				t.Fatalf("the launch failed after its launcher reconnected: %v", done.err)
			}
			if done.loc.Sandbox.PodUID != first.Sandbox.PodUID || done.loc.Sandbox.Generation != 2 {
				t.Fatalf("the launch answered %+v, want generation 2 in the kept pod %s", done.loc.Sandbox, first.Sandbox.PodUID)
			}
			if got := redialed.commands(); got != tc.want {
				t.Fatalf("the reconnected launcher was sent %q, want %q", got, tc.want)
			}
			if got := commandsOf(g.sent(workerToken)); got != "start 1" {
				t.Fatalf("the connection that dropped was sent %q, want generation 1's start alone, from before the drop", got)
			}
		})
	}
}

// A launch whose role launcher reconnects after the launch chose its session — serveLeakCheck has
// returned and the Start is not written yet — sends that Start to no launcher: the start stays on
// the session it was prepared for (launchers.start), which has closed, so Spawn fails naming the
// closed connection, having stopped the Start's generation on the reconnected launcher in case the
// Start began it (stopIssuedChild). The supervisor's next try, at the next generation, starts the
// role on the reconnected launcher. The drop comes from the store's read of the issue's saved work
// (restoreWorkspace), the one step between the session's choice and the Start's write a row can
// hold: armed, that read drops the connection, waits until the launcher has said hello again, and
// answers that there is nothing to restore.
func TestALaunchWhoseLauncherReconnectsBeforeItsStartIsWrittenFailsAndTheNextTryStarts(t *testing.T) {
	var g *rig
	var armed atomic.Bool
	dropped, redialed := make(chan struct{}), make(chan struct{})
	g = newRig(t, nil, withOptions(func(o *Options) {
		o.BootTimeout = reconnectBoot
		o.WorkspaceBundleRestored = func(context.Context, string, int64) error { return nil }
		o.SavedWorkspaceBundle = func(ctx context.Context, _, _ string, _ time.Time) (runtime.WorkspaceBundle, runtime.WorkspaceBundleTake, error) {
			if armed.CompareAndSwap(true, false) {
				if session := launcherOf(g, workerToken); session != nil {
					session.close()
				}
				close(dropped)
				select {
				case <-redialed:
				case <-ctx.Done():
				}
			}
			return runtime.WorkspaceBundle{}, runtime.WorkspaceBundleNone, nil
		}
	}))
	first := g.spawn(workerSpec(t))

	armed.Store(true)
	result := spawnAsync(g, nextGeneration(workerSpec(t), 2))
	select {
	case <-dropped:
	case early := <-result:
		t.Fatalf("the launch returned before it reached its start: %+v, %v", early.loc, early.err)
	case <-time.After(90 * time.Second):
		t.Fatal("hang guard: the launch never reached its start")
	}
	again := redial(t, g, workerToken, 0, false)
	close(redialed)

	done := awaitLaunch(t, result)
	t.Logf("Spawn of generation 2, its launcher reconnected before the Start was written: locator %+v, error %v", done.loc.Sandbox, done.err)
	if done.err == nil || !strings.Contains(done.err.Error(), "start through its authenticated launcher: launcher connection closed") {
		t.Fatalf("Spawn = %+v, %v; want it failed on the closed session its start was prepared for", done.loc.Sandbox, done.err)
	}
	if got := commandsOf(g.sent(workerToken)); got != "start 1, stop 1" {
		t.Fatalf("the connection that dropped was sent %q, want generation 1 started and stopped, and no start of generation 2", got)
	}
	if got := again.commands(); got != "stop 2" {
		t.Fatalf("the reconnected launcher was sent %q, want the stop of the generation the unwritten Start named", got)
	}

	retried := retryLaunch(t, g, first, 3)
	if got := again.commands(); got != "stop 2, start 3" {
		t.Fatalf("after the next try the reconnected launcher was sent %q, want generation 3 started (%+v)", got, retried.Sandbox)
	}
}

// A launch whose role launcher's connection drops once the Start has crossed it, before the
// launcher answers, fails naming the connection that closed before its answer. The launcher may
// have begun the generation, so the launch stops it on the launcher that says hello again
// (stopIssuedChild), which here reports that generation running, as a launcher that began it before
// its connection dropped does. The supervisor's next try, at the next generation, starts the role
// on that launcher.
func TestALaunchWhoseLauncherDropsAfterReadingItsStartFailsAndTheNextTryStarts(t *testing.T) {
	g := newRig(t, nil, withOptions(func(o *Options) { o.BootTimeout = reconnectBoot }))
	first := g.spawn(workerSpec(t))
	// The launcher process restarts: the connection of the one that ran generation 1 ends, and the
	// new process says hello with no child, to hang up on the first Start it reads.
	dropLauncher(t, g, workerToken)
	hangs := redial(t, g, workerToken, 0, true)

	result := spawnAsync(g, nextGeneration(workerSpec(t), 2))
	select {
	case <-hangs.hungUp:
	case early := <-result:
		t.Fatalf("the launch returned before its Start reached the launcher: %+v, %v", early.loc, early.err)
	case <-time.After(90 * time.Second):
		t.Fatal("hang guard: the launch never sent its Start")
	}
	again := redial(t, g, workerToken, 2, false)

	done := awaitLaunch(t, result)
	t.Logf("Spawn of generation 2, its launcher dropped after reading the Start: locator %+v, error %v", done.loc.Sandbox, done.err)
	if done.err == nil || !strings.Contains(done.err.Error(), "start through its authenticated launcher: launcher connection closed before its answer") {
		t.Fatalf("Spawn = %+v, %v; want it failed on the connection that closed before its answer", done.loc.Sandbox, done.err)
	}
	if got := hangs.commands(); got != "start 2" {
		t.Fatalf("the connection that dropped was sent %q, want generation 2's start", got)
	}
	if got := again.commands(); got != "stop 2" {
		t.Fatalf("the reconnected launcher was sent %q, want the stop of the generation the unanswered Start may have begun", got)
	}

	retried := retryLaunch(t, g, first, 3)
	if got := again.commands(); got != "stop 2, start 3" {
		t.Fatalf("after the next try the reconnected launcher was sent %q, want generation 3 started (%+v)", got, retried.Sandbox)
	}
}

// retryLaunch is the supervisor's next try of the tester at generation, which must start the role
// in first's pod; it fails the test otherwise.
func retryLaunch(t *testing.T, g *rig, first runtime.Locator, generation uint64) runtime.Locator {
	t.Helper()
	next := awaitLaunch(t, spawnAsync(g, nextGeneration(workerSpec(t), generation)))
	if next.err != nil {
		t.Fatalf("the next try at generation %d failed: %v", generation, next.err)
	}
	if next.loc.Sandbox.PodUID != first.Sandbox.PodUID || next.loc.Sandbox.Generation != generation {
		t.Fatalf("the next try answered %+v, want generation %d in the kept pod %s", next.loc.Sandbox, generation, first.Sandbox.PodUID)
	}
	return next.loc
}

// launched is what one Spawn a row runs in a goroutine returned.
type launched struct {
	loc runtime.Locator
	err error
}

// spawnAsync runs g's Spawn of spec in a goroutine and answers the channel its result arrives on.
func spawnAsync(g *rig, spec runtime.SpawnSpec) <-chan launched {
	result := make(chan launched, 1)
	go func() {
		loc, err := g.r.Spawn(g.ctx, spec)
		result <- launched{loc, err}
	}()
	return result
}

// awaitLaunch is the result of the Spawn behind result, failing the test when it has not returned
// within the hang guard: a launch that waits out its own launcher bound returns well inside it.
func awaitLaunch(t *testing.T, result <-chan launched) launched {
	t.Helper()
	select {
	case done := <-result:
		return done
	case <-time.After(90 * time.Second):
		t.Fatal("hang guard: the launch never returned")
		return launched{}
	}
}

// dropLauncher ends the connection of token's launcher as the daemon sees a dropped connection end:
// the session closes, its pending requests fail, and no launcher of token is connected until one
// says hello again.
func dropLauncher(t *testing.T, g *rig, token claim.Token) {
	t.Helper()
	session := launcherOf(g, token)
	if session == nil {
		t.Fatalf("no launcher of %s is connected to drop", token)
	}
	session.close()
}

// redialedLauncher is one connection of a role's launcher that said hello through the runtime's
// own LauncherResolver, as a launcher does on every dial. It answers a Start and a Stop as the
// rig's fake launcher does (answerGeneration) unless it hangs up, when it ends its connection on
// the first Start it reads without answering it. got is every command it read, so a row tells this
// connection's commands from those of any other connection of the same role.
type redialedLauncher struct {
	mu  sync.Mutex
	got []shimwire.Frame
	// hungUp closes once a launcher that hangs up has ended its connection on a Start.
	hungUp chan struct{}
}

// redial says hello for token's launcher from its pod as the tracker holds it, with the token of
// the role's Secret (rigLauncherHello), through the runtime's resolver, and serves the accepted
// connection as a redialedLauncher whose first state reports running (0 for no child). It fails
// the test when the resolver refuses the hello.
func redial(t *testing.T, g *rig, token claim.Token, running uint64, hangUp bool) *redialedLauncher {
	t.Helper()
	_, role, ok := token.Cut()
	if !ok {
		t.Fatalf("%s names no role", token)
	}
	name := SandboxName(token)
	pod := g.pod(name)
	if pod == nil || g.secret(roleSecretName(name, role)) == nil {
		t.Fatalf("%s has no pod or no launcher Secret to say hello from", token)
	}
	handler, _, reason := g.r.LauncherResolver()(rigLauncherHello(g, name, string(pod.UID), role))
	if handler == nil {
		t.Fatalf("the resolver refused %s's launcher saying hello again from pod %s: %s", token, pod.UID, reason)
	}
	server, client := net.Pipe()
	t.Cleanup(func() { _ = client.Close() })
	go handler.ServeLauncher(server, bufio.NewReader(server), shimwire.NewWriter(server))
	l := &redialedLauncher{hungUp: make(chan struct{})}
	go l.serve(client, running, hangUp)
	return l
}

func (l *redialedLauncher) serve(conn net.Conn, running uint64, hangUp bool) {
	writer := shimwire.NewWriter(conn)
	if writer.WriteFrame(launcherFirstState(running)) != nil {
		return
	}
	frames := shimwire.NewReader(conn)
	for {
		line, err := frames.ReadLine()
		if err != nil {
			return
		}
		frame, err := shimwire.Decode(line)
		if err != nil {
			return
		}
		l.mu.Lock()
		l.got = append(l.got, frame)
		l.mu.Unlock()
		if _, start := frame.(shimwire.LauncherStart); start && hangUp {
			_ = conn.Close()
			close(l.hungUp)
			return
		}
		answers, _ := answerGeneration(frame, &running)
		for _, answer := range answers {
			_ = writer.WriteFrame(answer)
		}
	}
}

// commands names every Start and Stop l read, in order (commandsOf).
func (l *redialedLauncher) commands() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return commandsOf(l.got)
}

// commandsOf names each Start and Stop among frames by its generation, in order: "stop 1, start 2".
func commandsOf(frames []shimwire.Frame) string {
	var named []string
	for _, frame := range frames {
		switch command := frame.(type) {
		case shimwire.LauncherStart:
			named = append(named, fmt.Sprintf("start %d", command.Generation))
		case shimwire.LauncherStop:
			named = append(named, fmt.Sprintf("stop %d", command.Generation))
		}
	}
	return strings.Join(named, ", ")
}

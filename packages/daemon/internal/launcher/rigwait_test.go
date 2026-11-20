package launcher

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/hostproc"
)

// The rig's own behaviour (acceptRole, nextFor): which empty connections it passes over, and how
// long it waits for a frame. A row the rig must fail runs in a test binary of its own
// (TestRigRowHelper), and a stranger to the rig is a process of its own (TestRigStrayHelper).
const (
	rigRowEnv   = "LEGION_LAUNCHER_RIG_ROW"
	rigStrayEnv = "LEGION_LAUNCHER_RIG_STRAY"
)

func init() {
	helperChildVariables = append(helperChildVariables, rigRowEnv, rigStrayEnv)
}

// TestRigStrayHelper is the stranger a test of the rig starts: a process of its own, as any other
// process of the machine is, that dials the address rigStrayEnv names ("<how> <address>"), says
// the address it dialed from on its standard output, and once its standard input ends closes the
// connection as how says: "eof" with nothing sent, "reset" with nothing sent and the connection
// reset (SO_LINGER 0, as a connect probe or a health check may close), "partial" after part of a
// hello.
func TestRigStrayHelper(t *testing.T) {
	spec := os.Getenv(rigStrayEnv)
	if spec == "" {
		return
	}
	how, address, _ := strings.Cut(spec, " ")
	conn, err := net.Dial("tcp", address)
	if err != nil {
		fmt.Fprintln(os.Stderr, "stray:", err)
		os.Exit(1)
	}
	fmt.Println(conn.LocalAddr().String())
	_, _ = io.Copy(io.Discard, os.Stdin)
	switch how {
	case "reset":
		_ = conn.(*net.TCPConn).SetLinger(0)
	case "partial":
		_, _ = conn.Write([]byte(`{"type":"launcher_hello","token":"`))
	}
	_ = conn.Close()
	os.Exit(0)
}

// strayFrom starts TestRigStrayHelper against listener and answers the address it dialed from.
// The stranger's connection waits in the listener's queue from then on, and it closes as how says
// reconnectDelay/4 later, so a launcher connection the case drops after this call is redialed
// behind it.
func strayFrom(t *testing.T, listener net.Listener, how string) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	stray := exec.Command(self, "-test.run=^TestRigStrayHelper$")
	stray.Env = append(os.Environ(), rigStrayEnv+"="+how+" "+listener.Addr().String(), hostproc.ChildEnv(t))
	stdin, err := stray.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := stray.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	stray.Stderr = &stderr
	if err := stray.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stdin.Close(); _ = stray.Wait() })
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatalf("the stranger dialed nothing: %v\n%s", err, stderr.String())
	}
	go func() {
		time.Sleep(reconnectDelay / 4)
		_ = stdin.Close()
	}()
	return strings.TrimSpace(line)
}

// A connection that closes without sending a byte, by an end of file or by a reset, is passed over
// when another process made it, and the launcher's reconnect behind it is taken (acceptRole). On a
// shared development machine a case failed once where it took the reconnect of a dropped
// connection, with "the launcher connection ended while the test waited for the launcher's hello:
// EOF" (TestATrimmerGivenNothingItCanRunStartsWhenSettingsArrive). Here a process of the test's
// own makes such a connection before the drop, so it waits ahead of the launcher's reconnect,
// which comes reconnectDelay after the drop.
func TestTheRigPassesOverAConnectionThatClosesWithoutAByte(t *testing.T) {
	for _, row := range []struct{ name, how string }{{"an end of file", "eof"}, {"a reset", "reset"}} {
		t.Run(row.name, func(t *testing.T) {
			g := newRig(t)
			from := strayFrom(t, g.listener, row.how)
			_ = g.conn.Close()
			g.accept()
			if want := []string{from}; !slices.Equal(g.passedOver, want) {
				t.Fatalf("the rig passed over %q before it took the launcher's reconnect, want %q, the other process's connection that sent nothing", g.passedOver, want)
			}
		})
	}
}

// TestRigRowHelper runs a row of the rig that a test below must see fail, in a test binary of its
// own (runRigRow): the row rigRowEnv names, under the -test.timeout that test gives.
func TestRigRowHelper(t *testing.T) {
	switch row := os.Getenv(rigRowEnv); row {
	case "":
		return
	case "cascade":
		t.Run("a hello that never comes", helloThatNeverComes)
		t.Run("a sound rig after it", func(t *testing.T) {
			g := newRig(t)
			_ = g.conn.Close()
			g.accept()
		})
	case "margin":
		t.Run("a hello that never comes", helloThatNeverComes)
	case "launcher-drops":
		t.Run("a launcher that drops its first connection before its hello", func(t *testing.T) {
			dial := dialDaemon
			var dropped atomic.Bool
			dialDaemon = func(ctx context.Context, address string) (net.Conn, error) {
				conn, err := dial(ctx, address)
				if err == nil && dropped.CompareAndSwap(false, true) {
					_ = conn.Close()
					return nil, errors.New("the case closed the launcher's first connection before its hello")
				}
				return conn, err
			}
			// Restored after the rig's own cleanup, which waits for Run to return, since this is
			// registered before newRig registers that one.
			t.Cleanup(func() { dialDaemon = dial })
			newRig(t)
			t.Fatal("the rig took the launcher's reconnect, past its first connection, which closed without a hello")
		})
	case "partial":
		t.Run("another process's partial hello", func(t *testing.T) {
			g := newRig(t)
			strayFrom(t, g.listener, "partial")
			_ = g.conn.Close()
			g.accept()
			t.Fatal("the rig took the launcher's reconnect, past a connection that sent part of a hello")
		})
	default:
		t.Fatalf("no row %q", row)
	}
}

// helloThatNeverComes is a fleet whose launcher's hello never comes: its first pass is held where
// it removes the clone's one lock until the case ends, where the hello row of
// TestTheRigWaitsForAFrameTheLauncherIsSlowToSend holds it for heldHello.
func helloThatNeverComes(t *testing.T) {
	c := newLockClone(t)
	c.lock("index.lock", "stale", time.Second)
	release := make(chan struct{})
	previous := gitLockRemove
	gitLockRemove = func(path string) error {
		<-release
		return os.Remove(path)
	}
	// Restored after the launcher's own cleanup, for the reason that row's is.
	t.Cleanup(func() { gitLockRemove = previous })
	// The pass is let go of before the launcher's cleanup waits for Run: a deferred call runs
	// before any cleanup of the case.
	defer close(release)
	t.Chdir(c.dir)
	useSweepingLauncher(t)
	newFleet(t, 2*time.Second, "tester")
	t.Fatal("the launcher's hello came through a first pass held until the case ends")
}

// runRigRow runs TestRigRowHelper's row in a test binary of its own under -test.timeout timeout,
// and answers what it printed.
func runRigRow(t *testing.T, row string, timeout time.Duration) string {
	t.Helper()
	if left, ok := t.Deadline(); ok && time.Until(left) < timeout+frameMargin {
		t.Fatalf("the row %q runs under a -test.timeout of %s, and this test binary has %s left", row, timeout, time.Until(left).Round(time.Second))
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout+launcherLeaveBound)
	defer cancel()
	child := exec.CommandContext(ctx, self, "-test.run=^TestRigRowHelper$", "-test.v", "-test.count=1", "-test.timeout="+timeout.String())
	child.Env = append(os.Environ(), rigRowEnv+"="+row, hostproc.ChildEnv(t))
	output, _ := child.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("the row %q ran past its -test.timeout of %s by %s:\n%s", row, timeout, launcherLeaveBound, output)
	}
	return string(output)
}

// failedAfter is how long the subtest of TestRigRowHelper named sub ran before it failed, from
// the row's output, or -1 when it did not fail.
func failedAfter(output, sub string) time.Duration {
	match := regexp.MustCompile(`--- FAIL: TestRigRowHelper/` + regexp.QuoteMeta(strings.ReplaceAll(sub, " ", "_")) + ` \(([0-9.]+)s\)`).FindStringSubmatch(output)
	if match == nil {
		return -1
	}
	seconds, _ := strconv.ParseFloat(match[1], 64)
	return time.Duration(seconds * float64(time.Second))
}

// cascadeTimeout is the -test.timeout of the binary that runs a hello that never comes and a
// sound rig after it: room for the first case's whole helloBound and frameMargin, and a helloBound
// for the second.
const cascadeTimeout = helloBound + frameMargin + helloBound

// A wait of the rig that gives up does so at the launcher's own bound for its frame, by name, and
// leaves the binary's later cases their time: a hello that never comes fails its case at
// helloBound, and a sound rig after it in the same binary passes. A wait bounded by the binary's
// deadline alone would spend the rest of that deadline, and every case after it would then fail
// at once, naming a launcher that never connected.
func TestARigWaitThatGivesUpLeavesLaterCasesTheirTime(t *testing.T) {
	output := runRigRow(t, "cascade", cascadeTimeout)
	took := failedAfter(output, "a hello that never comes")
	if took < 0 || !strings.Contains(output, "no launcher connected while the test waited for one") {
		t.Fatalf("the case whose hello never comes did not fail naming its wait:\n%s", output)
	}
	if took < helloBound || took > helloBound+launcherLeaveBound {
		t.Fatalf("the case whose hello never comes failed after %s, want it to wait out helloBound (%s) and no more than its cleanup past it:\n%s", took, helloBound, output)
	}
	if !strings.Contains(output, "--- PASS: TestRigRowHelper/a_sound_rig_after_it") {
		t.Fatalf("the sound rig after the case whose hello never came did not pass:\n%s", output)
	}
	if strings.Contains(output, "panic: test timed out") {
		t.Fatalf("the binary's own timeout ended the row:\n%s", output)
	}
}

// marginTimeout is the -test.timeout of the binary whose hello never comes in
// TestARigWaitEndsBeforeTheBinaryTimeoutPanics: shorter than helloBound, so the binary's deadline
// less frameMargin is what cuts the wait, and longer than frameMargin, so that wait is not over
// before it begins.
const marginTimeout = frameMargin + 5*time.Second

// A wait the test binary's deadline cuts short still ends frameMargin before that deadline, so the
// case fails naming what it waited for, and its launcher's cleanup is done, before the binary's own
// timeout panics, which names no wait and, without -v, prints none of the case's lines. Without
// the margin the wait runs to the deadline and the binary panics.
func TestARigWaitEndsBeforeTheBinaryTimeoutPanics(t *testing.T) {
	if marginTimeout >= helloBound {
		t.Fatalf("marginTimeout %s is not shorter than helloBound %s, so the binary's deadline would not cut the wait", marginTimeout, helloBound)
	}
	output := runRigRow(t, "margin", marginTimeout)
	if strings.Contains(output, "panic: test timed out") {
		t.Fatalf("the binary's own timeout ended the case before its wait gave up:\n%s", output)
	}
	if failedAfter(output, "a hello that never comes") < 0 || !strings.Contains(output, "no launcher connected while the test waited for one") {
		t.Fatalf("the case whose hello never comes did not fail naming its wait:\n%s", output)
	}
}

// A launcher that drops its own connection before its hello fails the rig, naming it: the rig
// passes over an empty connection only when no launcher of the test dialed it (acceptRole), and a
// pod's launcher that did this would cost the pod a reconnect on every connection.
func TestALauncherThatDropsItsConnectionBeforeItsHelloFailsTheRig(t *testing.T) {
	output := runRigRow(t, "launcher-drops", helloBound+frameMargin)
	if failedAfter(output, "a launcher that drops its first connection before its hello") < 0 ||
		!strings.Contains(output, "the launcher under test closed its connection from") {
		t.Fatalf("the rig did not fail naming the launcher's own empty connection:\n%s", output)
	}
}

// A connection that sends part of a hello and closes fails the rig by name, whoever made it: the
// pass-over is for a connection that sent nothing.
func TestAPartialHelloFailsTheRigByName(t *testing.T) {
	output := runRigRow(t, "partial", helloBound+frameMargin)
	if failedAfter(output, "another process's partial hello") < 0 ||
		!strings.Contains(output, "the launcher connection ended while the test waited for the launcher's hello") {
		t.Fatalf("the rig did not fail naming the partial hello:\n%s", output)
	}
}

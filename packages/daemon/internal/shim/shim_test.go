package shim_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/execfile"
	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/fixturegit"
	"github.com/sjawhar/legion/daemon/internal/leakguard"
	"github.com/sjawhar/legion/daemon/internal/leakguard/leakguardtest"
	"github.com/sjawhar/legion/daemon/internal/procgroup"
	"github.com/sjawhar/legion/daemon/internal/shim"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/testlog"
	"github.com/sjawhar/legion/daemon/internal/testtmp"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// The fake OMP child is this test binary, re-executed with fakeOMPEnv set: a Go program that
// speaks enough of OMP's RPC frames for the bridge to have something real on the other side of
// its pipes.
const fakeOMPEnv = "LEGION_SHIM_FAKE_OMP"

const (
	bootToken = "boot-token-for-the-pane"
	grace     = 3 * time.Second
)

// TestMain runs this binary as the fake OMP where fakeOMPEnv is set, and runs the tests otherwise.
// Only TestMain makes the fake of a copy a test starts: in a build of these tests without it, such
// a copy runs the tests, each of which starts copies that run them again. fakeOMPCopy refuses to
// start a copy in such a process (fakeOMPCopyRefusal), and
// TestACopyStartedAsTheFakeOMPRunsAsTheFake fails in such a build by name.
func TestMain(m *testing.M) {
	if os.Getenv(fakeOMPEnv) == "1" {
		os.Exit(fakeOMP())
	}
	// The tests run real git and jj, in the shim's adoption (fixtureenv.MustGuard).
	fixtureenv.MustGuard()
	os.Exit(m.Run())
}

// fakeOMPCopy is the path of this test binary, which a test runs as the fake OMP with fakeOMPEnv=1
// in the copy's environment. It fails t where fakeOMPCopyRefusal refuses.
func fakeOMPCopy(t *testing.T) string {
	t.Helper()
	if err := fakeOMPCopyRefusal(); err != nil {
		t.Fatal(err)
	}
	return os.Args[0]
}

// fakeOMPCopyRefusal is why this process may start no copy of this test binary, nil when it may. A
// process that has fakeOMPEnv=1 was started as the fake OMP, and TestMain runs such a process as
// the fake, so one that runs the tests runs in a build without TestMain, and a copy started there
// would run the tests again.
func fakeOMPCopyRefusal() error {
	if os.Getenv(fakeOMPEnv) == "1" {
		return fmt.Errorf("this process has %s=1, so a test started it as the fake OMP, but it runs the tests: this build of the tests has no TestMain that runs the fake, and a copy started here would run the tests again", fakeOMPEnv)
	}
	return nil
}

// fakeOMPCopyRefusal, which fakeOMPCopy asks before any copy starts, refuses a process that has
// fakeOMPEnv=1 and no other. While TestMain is in the build no copy reaches it, so this test is
// what holds the refusal.
func TestAProcessStartedAsTheFakeOMPStartsNoCopy(t *testing.T) {
	fixtureenv.Unset(t, fakeOMPEnv)
	if err := fakeOMPCopyRefusal(); err != nil {
		t.Fatalf("with %s unset: %v, want no refusal", fakeOMPEnv, err)
	}
	t.Setenv(fakeOMPEnv, "1")
	if err := fakeOMPCopyRefusal(); err == nil || !strings.Contains(err.Error(), fakeOMPEnv+"=1") {
		t.Fatalf("with %s=1: %v, want a refusal that names it", fakeOMPEnv, err)
	}
}

// A copy started with fakeOMPEnv=1 runs as the fake OMP: it says it is ready, with its pid. The
// fake does not read -h. In a build without TestMain, the testing package reads -h before any test
// runs, prints its own flag usage and exits, so this test fails there by name and its copy starts
// no copy of its own.
func TestACopyStartedAsTheFakeOMPRunsAsTheFake(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cmd := exec.Command(fakeOMPCopy(t), "-h")
	cmd.Env = append(os.Environ(), fakeOMPEnv+"=1", "FAKE_OMP_MARKER="+filepath.Join(dir, "marker"), "FAKE_OMP_LOG="+filepath.Join(dir, "log"))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil || string(out) != fmt.Sprintf(`{"type":"fake_ready","pid":%d}`+"\n", cmd.Process.Pid) {
		t.Fatalf("a copy started as the fake OMP with -h: %v, printed %q, want exit 0 and its ready line alone\n%s", err, out, stderr.String())
	}
}

// fakeOMP answers a prompt with OMP's own sequence — the ack, then the turn — records every line
// it reads, and reports on its stdout what the tests need to see from inside the child: that it
// started, that a SIGTERM reached it, and a burst of frames on SIGUSR1.
func fakeOMP() int {
	var mu sync.Mutex
	emit := func(line string) {
		mu.Lock()
		defer mu.Unlock()
		_, _ = os.Stdout.WriteString(line + "\n")
	}
	// The marker holds the child's whole environment, so a test can read what the shim started
	// it with (omp.environment) as well as that it started at all (omp.started).
	if err := os.WriteFile(os.Getenv("FAKE_OMP_MARKER"), []byte(strings.Join(os.Environ(), "\n")), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "fake omp: write the marker: %v\n", err)
		return 3
	}
	record, err := os.OpenFile(os.Getenv("FAKE_OMP_LOG"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fake omp: open the log: %v\n", err)
		return 3
	}

	signals := make(chan os.Signal, 4)
	signal.Notify(signals, syscall.SIGUSR1)
	if os.Getenv("FAKE_OMP_IGNORE_SIGTERM") == "1" {
		signal.Notify(signals, syscall.SIGTERM)
	}
	go func() {
		for sig := range signals {
			switch sig {
			case syscall.SIGTERM:
				emit(`{"type":"fake_sigterm"}`)
			case syscall.SIGUSR1:
				for n := range 1200 {
					emit(fmt.Sprintf(`{"type":"fake_frame","n":%d}`, n))
				}
				emit(`{"type":"agent_end"}`)
			}
		}
	}()

	// FAKE_OMP_HOLD_READY names a file the fake waits for before its first line, so a test can
	// tell what the shim does at the spawn from what it does on the child's first frame.
	if hold := os.Getenv("FAKE_OMP_HOLD_READY"); hold != "" {
		for {
			if _, err := os.Stat(hold); err == nil {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	emit(fmt.Sprintf(`{"type":"fake_ready","pid":%d}`, os.Getpid()))
	if message := os.Getenv("FAKE_OMP_EXTENSION_ERROR"); message != "" {
		emit(fmt.Sprintf(`{"type":"extension_error","extensionPath":"/opt/legion/pi-legion/dist/legion.js","event":"session_start","error":%q}`, message))
	}
	// FAKE_OMP_TOOL_ARGS is the JSON arguments of a tool call the fake starts once it is ready.
	if args := os.Getenv("FAKE_OMP_TOOL_ARGS"); args != "" {
		emit(`{"type":"tool_execution_start","toolCallId":"toolu_1","toolName":"bash","args":` + args + `}`)
	}
	if os.Getenv("FAKE_OMP_HUGE_LINE") == "1" {
		emit(`{"type":"fake_huge","pad":"` + strings.Repeat("x", 2<<20) + `"}`)
	}

	lines := bufio.NewScanner(os.Stdin)
	lines.Buffer(make([]byte, 64<<10), 2<<20)
	for lines.Scan() {
		line := lines.Bytes()
		_, _ = record.Write(append(bytes.Clone(line), '\n'))
		var frame struct{ Type, ID, Message string }
		_ = json.Unmarshal(line, &frame)
		switch frame.Type {
		case "prompt":
			emit(fmt.Sprintf(`{"type":"response","id":%q,"command":"prompt","success":true}`, frame.ID))
			emit(`{"type":"agent_start"}`)
			if frame.Message != "hold" {
				emit(`{"type":"agent_end"}`)
			}
		case "fake_end_turn":
			emit(`{"type":"agent_end"}`)
		case "get_state":
			emit(fmt.Sprintf(`{"type":"response","id":%q,"command":"get_state","success":true,"data":{"isStreaming":false}}`, frame.ID))
		// fake_orphan runs, as a tool call's bash does, a shell that starts a sleep of Message
		// seconds in the background and exits, so the sleep is orphaned while it runs, and reports
		// its pid. fake_exit exits with the status Message names, and fake_die is killed by
		// SIGKILL: the agent's own end, as its shim must report it.
		case "fake_orphan":
			out, err := exec.Command("/bin/sh", "-c", "/bin/sleep "+frame.Message+" </dev/null >/dev/null 2>&1 & echo $!").Output()
			if err != nil {
				fmt.Fprintf(os.Stderr, "fake omp: leave an orphan: %v\n", err)
				return 3
			}
			emit(fmt.Sprintf(`{"type":"fake_orphan","pid":%s}`, strings.TrimSpace(string(out))))
		case "fake_exit":
			code := 0
			_, _ = fmt.Sscan(frame.Message, &code)
			return code
		case "fake_die":
			_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
		}
	}
	return 0
}

// omp is one fake OMP child's configuration and what it leaves behind.
type omp struct {
	marker, log string
	env         []string
}

func newOMP(t *testing.T, env ...string) *omp {
	t.Helper()
	dir := t.TempDir()
	o := &omp{marker: filepath.Join(dir, "started"), log: filepath.Join(dir, "stdin.ndjson")}
	o.env = append(os.Environ(), fakeOMPEnv+"=1", "FAKE_OMP_MARKER="+o.marker, "FAKE_OMP_LOG="+o.log)
	o.env = append(o.env, env...)
	return o
}

func (o *omp) started() bool {
	_, err := os.Stat(o.marker)
	return err == nil
}

// environment is the environment the shim started the child with, as the child read it: the
// marker the child writes at its start holds it, one NAME=value a line. It waits for the child,
// since a test that reads it is asking what the spawn did.
func (o *omp) environment(t *testing.T) map[string]string {
	t.Helper()
	testwait.Eventually(t, "the fake OMP to start and record its environment", o.started)
	body, err := os.ReadFile(o.marker)
	if err != nil {
		t.Fatalf("read the fake OMP's environment: %v", err)
	}
	env := map[string]string{}
	for _, entry := range strings.Split(string(body), "\n") {
		if name, value, ok := strings.Cut(entry, "="); ok {
			env[name] = value
		}
	}
	return env
}

// received is every line the child read on its stdin, decoded.
func (o *omp) received(t *testing.T) []shimwire.Frame {
	t.Helper()
	body, err := os.ReadFile(o.log)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("read the fake OMP's stdin record: %v", err)
	}
	var frames []shimwire.Frame
	for line := range bytes.Lines(body) {
		frame, err := shimwire.Decode(line)
		if err != nil {
			t.Fatalf("decode a line the fake OMP read: %v", err)
		}
		frames = append(frames, frame)
	}
	return frames
}

// fakeClock hands every wait the shim asks for to the test, which fires it — the backoff between
// dials and the grace before a kill are then steps of the test rather than time it spends.
type fakeClock struct {
	waits    chan clockWait
	mu       sync.Mutex
	released bool
}

type clockWait struct {
	d    time.Duration
	fire chan time.Time
}

func newClock() *fakeClock { return &fakeClock{waits: make(chan clockWait, 64)} }

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	w := clockWait{d: d, fire: make(chan time.Time, 1)}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.released {
		w.fire <- time.Time{}
		return w.fire
	}
	c.waits <- w
	return w.fire
}

// nextAny is the shim's next wait whatever its span, named by what the caller expected to be
// waited for: what a test reads when the span is the thing under test rather than a constant it
// already knows (drainCommands' bound, which is what is left of a span).
func (c *fakeClock) nextAny(t *testing.T, what string) clockWait {
	t.Helper()
	return testwait.Receive(t, "the shim to wait "+what, c.waits)
}

// next is the shim's next wait, which must be of d.
func (c *fakeClock) next(t *testing.T, d time.Duration) clockWait {
	t.Helper()
	w := c.nextAny(t, fmt.Sprintf("%v", d))
	if w.d != d {
		t.Fatalf("the shim waited %v, want %v", w.d, d)
	}
	return w
}

func (w clockWait) elapse() { w.fire <- time.Time{} }

// release fires every wait, pending and future, so a test's cleanup is never stuck behind one.
func (c *fakeClock) release() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.released = true
	for {
		select {
		case w := <-c.waits:
			w.fire <- time.Time{}
		default:
			return
		}
	}
}

func (c *fakeClock) idle(t *testing.T) {
	t.Helper()
	select {
	case w := <-c.waits:
		t.Fatalf("the shim waited %v; it should not have waited at all", w.d)
	default:
	}
}

// syncBuffer is the shim's log: what the pane shows.
type syncBuffer struct {
	testlog.Buffer
}

func newSyncBuffer() *syncBuffer { return &syncBuffer{} }

// count is how many log lines contain substr.
func (b *syncBuffer) count(substr string) int {
	n := 0
	for line := range strings.Lines(b.String()) {
		if strings.Contains(line, substr) {
			n++
		}
	}
	return n
}

// await waits for n log lines that contain substr; a failure prints the log.
func (b *syncBuffer) await(t *testing.T, substr string, n int) {
	t.Helper()
	testwait.Until(t, fmt.Sprintf("the shim to log %q %d time(s)", substr, n), func() (string, bool) {
		return "log: " + b.String(), b.count(substr) >= n
	})
}

// awaitLine waits for n log lines that are exactly line: the pane's one-line frame summaries.
func (b *syncBuffer) awaitLine(t *testing.T, line string, n int) {
	t.Helper()
	testwait.Until(t, fmt.Sprintf("the shim to log the line %q %d time(s)", line, n), func() (string, bool) {
		got := 0
		for l := range strings.Lines(b.String()) {
			if strings.TrimSpace(l) == line {
				got++
			}
		}
		return "log: " + b.String(), got >= n
	})
}

// running is one shim.Run in a goroutine.
type running struct {
	cancel  context.CancelFunc
	done    chan struct{}
	code    int
	failure error
	log     *syncBuffer
}

func run(t *testing.T, cfg shim.Config, clock *fakeClock) *running {
	t.Helper()
	r := &running{done: make(chan struct{}), log: newSyncBuffer()}
	cfg.Log = r.log
	cfg.Clock = clock
	var ctx context.Context
	ctx, r.cancel = context.WithCancel(context.Background())
	go func() {
		r.code, r.failure = shim.Run(ctx, cfg)
		close(r.done)
	}()
	t.Cleanup(func() {
		clock.release()
		r.cancel()
		r.awaitExit(t)
	})
	return r
}

// awaitExit waits for the shim to exit; a failure prints its log.
func (r *running) awaitExit(t *testing.T) {
	t.Helper()
	testwait.Until(t, "the shim to exit", func() (string, bool) {
		return "log: " + r.log.String(), r.exited()
	})
}

// stop ends the shim's own context and waits for it to exit, without wait's refusal of a
// non-nil error: a shim ended by its own context — an identity or keygen failure, a signal —
// reports the child's exit or its own failure, and that is not a test failure here.
func (r *running) stop(t *testing.T) {
	t.Helper()
	r.cancel()
	r.awaitExit(t)
}

// err is r.failure's text once the shim has ended (r.done is closed by then), or "" when it
// ended with no error.
func (r *running) err() string {
	if r.failure == nil {
		return ""
	}
	return r.failure.Error()
}

func (r *running) wait(t *testing.T) int {
	t.Helper()
	r.awaitExit(t)
	if r.failure != nil {
		t.Fatalf("shim.Run: %v; log:\n%s", r.failure, r.log)
	}
	return r.code
}

func (r *running) exited() bool {
	select {
	case <-r.done:
		return true
	default:
		return false
	}
}

// stub is the daemon's worker stream listener reduced to what a test drives by hand: it accepts,
// and every frame on the connection is the test's to read or write.
type stub struct {
	ln   net.Listener
	path string
}

// socketPath answers a socket path short enough to bind wherever TMPDIR points: Linux's sun_path
// holds 107 bytes, and t.TempDir() sits under TMPDIR, which is no longer short anywhere — a
// Legion role's TMPDIR is the issue's shared scratch on its volume
// (/legion/scratch/tmp, 19 bytes), and a developer's macOS TMPDIR is a path under
// /var/folders. Under either, the test's own name pushes the socket past sun_path and the listen
// fails with "bind: invalid argument" before the test does anything. The socket's own directory
// comes from testtmp, which places it under TMPDIR and falls back to /tmp for the socket path's
// length alone, and removes it with the test; every other file a test writes stays under
// t.TempDir().
func socketPath(t *testing.T) string {
	t.Helper()
	return testtmp.Socket(t, "s")
}

func listen(t *testing.T, path string) *stub {
	t.Helper()
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen on %s: %v", path, err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return &stub{ln: ln, path: path}
}

func (s *stub) accept(t *testing.T) *peer {
	t.Helper()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := s.ln.Accept()
		if err == nil {
			accepted <- conn
		}
	}()
	conn := testwait.Receive(t, "the shim to dial the listener", accepted)
	t.Cleanup(func() { _ = conn.Close() })
	return &peer{conn: conn, r: shimwire.NewReader(conn), w: shimwire.NewWriter(conn)}
}

type peer struct {
	conn net.Conn
	r    *shimwire.Reader
	w    *shimwire.Writer
}

func (p *peer) next(t *testing.T) shimwire.Frame {
	t.Helper()
	// The read's deadline is the one every wait has: the moment the shared wait gives up.
	deadline, _ := testwait.Context(t).Deadline()
	if err := p.conn.SetReadDeadline(deadline); err != nil {
		t.Fatalf("set a read deadline: %v", err)
	}
	line, err := p.r.ReadLine()
	if err != nil {
		t.Fatalf("read a frame from the shim: %v", err)
	}
	frame, err := shimwire.Decode(line)
	if err != nil {
		t.Fatalf("decode a frame from the shim: %v", err)
	}
	return frame
}

func (p *peer) expect(t *testing.T, want shimwire.Frame) {
	t.Helper()
	if got := p.next(t); !reflect.DeepEqual(got, want) {
		t.Fatalf("the shim sent %#v, want %#v", got, want)
	}
}

// expectRaw reads the next frame, which must be an unmodelled one of type typ.
func (p *peer) expectRaw(t *testing.T, typ string) shimwire.Raw {
	t.Helper()
	frame := p.next(t)
	raw, ok := frame.(shimwire.Raw)
	if !ok || raw.Type != typ {
		t.Fatalf("the shim sent %#v, want a %s frame", frame, typ)
	}
	return raw
}

func (p *peer) send(t *testing.T, frame shimwire.Frame) {
	t.Helper()
	if err := p.w.WriteFrame(frame); err != nil {
		t.Fatalf("write a %s frame to the shim: %v", frame.FrameType(), err)
	}
}

func (p *peer) sendLine(t *testing.T, line string) {
	t.Helper()
	if err := p.w.WriteLine([]byte(line)); err != nil {
		t.Fatalf("write %s to the shim: %v", line, err)
	}
}

// expectHello reads the connection's first frame, which must be a hello2 carrying the pane's
// boot token: an enrolled shim's hello also carries an identity, which this checks nothing about.
func (p *peer) expectHello(t *testing.T) {
	t.Helper()
	frame := p.next(t)
	hello, ok := frame.(shimwire.Hello2)
	if !ok || hello.BootToken != bootToken {
		t.Fatalf("the shim's first frame was %#v, want hello2 with boot token %q", frame, bootToken)
	}
}

// open is the daemon's side of a successful connect: the hello read and acked, and the child's
// first frame — proof it was spawned and bridged — read back. It returns the child's pid.
func (p *peer) open(t *testing.T) int {
	t.Helper()
	p.expectHello(t)
	p.send(t, shimwire.HelloAck{})
	return readyPID(t, p.expectRaw(t, "fake_ready"))
}

func readyPID(t *testing.T, raw shimwire.Raw) int {
	t.Helper()
	var ready struct{ PID int }
	if err := json.Unmarshal(raw.JSON, &ready); err != nil || ready.PID == 0 {
		t.Fatalf("the fake OMP's ready frame %s carries no pid: %v", raw.JSON, err)
	}
	return ready.PID
}

func config(t *testing.T, path string, o *omp) shim.Config {
	return shim.Config{
		Network:   "unix",
		Address:   path,
		BootToken: bootToken,
		Argv:      []string{fakeOMPCopy(t)},
		Env:       o.env,
		Grace:     grace,
	}
}

func frameTypes(frames []shimwire.Frame) []string {
	types := make([]string, len(frames))
	for i, frame := range frames {
		types[i] = frame.FrameType()
	}
	return types
}

// OMP is spawned only once the daemon has accepted the pane's hello: a frame that arrives before
// the ack is ignored — never buffered for a child that does not exist yet — and the child exists
// from the ack on.
func TestOMPIsSpawnedOnlyAfterTheHelloAck(t *testing.T) {
	path := socketPath(t)
	daemon := listen(t, path)
	child := newOMP(t)
	sh := run(t, config(t, path, child), newClock())

	p := daemon.accept(t)
	p.expectHello(t)
	p.send(t, shimwire.Prompt{ID: "early", DeliveryID: "d0", Message: "not yet"})
	sh.log.await(t, "ignoring a frame received before hello_ack", 1)
	if child.started() || sh.log.count("[worker-shim] spawned ") != 0 {
		t.Fatalf("OMP was spawned before the daemon acked the hello; log:\n%s", sh.log)
	}

	p.send(t, shimwire.HelloAck{})
	p.expectRaw(t, "fake_ready")
	p.send(t, shimwire.GetState{ID: "probe"})
	if raw := p.next(t).(shimwire.Response); raw.ID != "probe" {
		t.Fatalf("the probe was answered as %#v", raw)
	}
	if got := frameTypes(child.received(t)); !reflect.DeepEqual(got, []string{shimwire.TypeGetState}) {
		t.Fatalf("OMP read %v; the pre-ack prompt must never reach it", got)
	}
}

// theTag is the pin a pod's worker image carries, as these tests spell one, and theChecker the
// deployment's manifest digest the runtime tells a role of a deployment that runs a shared leak
// check (leakguard.CheckerVar): the digest (leakguard.Manifest) of the stand-in check every install
// here holds (installedFiles), which the shim compares before it starts an agent.
const theTag = "leak-check-v15"

// installedFiles are the files of the stand-in check every install here holds: a launcher whose
// --help names both shapes, and the other files of its manifest.
var installedFiles = map[string]string{
	leakguardtest.CheckName:    "#!/bin/sh\n" + leakguardtest.AnswersHelp(os.DevNull),
	leakguardtest.ProgramName:  leakguardtest.Program,
	leakguardtest.ReadmeName:   leakguardtest.Readme,
	leakguardtest.MustPassName: leakguardtest.MustPass,
}

var theChecker = leakguard.CheckerVar + "=" + func() string {
	digest, err := leakguard.Manifest(func(name string) ([]byte, error) { return []byte(installedFiles[name]), nil })
	if err != nil {
		panic(err)
	}
	return digest
}()

// installedCheck is the directory the pod's launcher installs the check the daemon served into,
// made the way the launcher makes it: a GitHub-shaped tarball of installedFiles, packed
// (leakguard.PackTarballManifest) and installed at tag (leakguard.InstallServed). The install
// runs the check's --help, and its bound is the rest of the test binary's deadline
// (testwait.Remaining): a bound counted from before that run starts is no subject here, and a
// loaded machine can hold the run past any fixed span.
func installedCheck(t *testing.T, tag string) string {
	t.Helper()
	members := map[string]leakguardtest.Member{}
	for name, body := range installedFiles {
		mode := int64(0o644)
		if name == leakguardtest.CheckName {
			mode = 0o755
		}
		members[name] = leakguardtest.Member{Body: body, Mode: mode}
	}
	raw := leakguardtest.GitHubTarball(t, members)
	body, _, err := leakguard.PackTarballManifest(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "leak-check")
	ctx := testwait.Context(t)
	if err := leakguard.InstallServed(ctx, leakguard.ServedInstall{Dir: dir, Tag: tag, Body: body, Start: procgroup.NoReaper}); err != nil {
		t.Fatal(err)
	}
	return dir
}

// The pod's launcher installs the shared public leak check once for the pod, before any role
// starts, and the shim only reads it. An install of the pin starts the agent, and the shim names
// it in PUBLIC_LEAK_CHECK where the environment names none, leaving a runtime's own value alone
// where there is one: the daemon decides wherever it says anything.
func TestAnInstallOfThePinStartsTheAgent(t *testing.T) {
	for _, tc := range []struct {
		name    string
		already string // what the shim's own environment names, "" for none
		want    func(dir string) string
	}{
		{"the environment names none, so the installed check is what the child is told", "",
			func(dir string) string { return filepath.Join(dir, leakguardtest.CheckName) }},
		{"the environment names one, so the daemon's value is what the child is told", "/opt/another/public-leak-check.sh",
			func(string) string { return "/opt/another/public-leak-check.sh" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := socketPath(t)
			daemon := listen(t, path)
			extra := []string{leakguard.TagVariable + "=" + theTag, theChecker}
			if tc.already != "" {
				extra = append(extra, leakguard.CheckVar+"="+tc.already)
			}
			child := newOMP(t, extra...)
			cfg := config(t, path, child)
			cfg.LeakCheckDir = installedCheck(t, theTag)
			sh := run(t, cfg, newClock())

			daemon.accept(t).open(t)
			if got, want := child.environment(t)[leakguard.CheckVar], tc.want(cfg.LeakCheckDir); got != want {
				t.Errorf("the child was told %s=%q, want %q; log:\n%s", leakguard.CheckVar, got, want, sh.log)
			}
			line := "[worker-shim] the shared public leak check " + theTag + " the pod's launcher installed is at " +
				filepath.Join(cfg.LeakCheckDir, leakguardtest.CheckName)
			if sh.log.count(line) != 1 {
				t.Errorf("the shim did not log %q once; log:\n%s", line, sh.log)
			}
		})
	}
}

// A role whose environment carries the pin and the deployment's checker and whose pod holds no
// install of the pin runs no agent at all: the shim ends with exit 1 and the read's own refusal.
// Every write path of such a role would refuse anyway.
func TestAnInstallThatIsNotThePinsStartsNoAgent(t *testing.T) {
	for _, tc := range []struct {
		name string
		dir  func(t *testing.T) string
		want []string
	}{
		{"an absent install", func(t *testing.T) string { return filepath.Join(t.TempDir(), "leak-check") },
			[]string{"LEAK CHECK SETUP ERROR", leakguard.TagVariable + "=" + theTag, "installed no check"}},
		{"an install of another tag", func(t *testing.T) string { return installedCheck(t, "leak-check-v14") },
			[]string{"LEAK CHECK SETUP ERROR", leakguard.TagVariable + "=" + theTag, "is of leak-check-v14"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := socketPath(t)
			daemon := listen(t, path)
			child := newOMP(t, leakguard.TagVariable+"="+theTag, theChecker)
			cfg := config(t, path, child)
			cfg.LeakCheckDir = tc.dir(t)
			sh := run(t, cfg, newClock())

			p := daemon.accept(t)
			p.expectHello(t)
			p.send(t, shimwire.HelloAck{})
			sh.awaitExit(t)
			if sh.code != 1 {
				t.Errorf("the shim ended %d, want 1", sh.code)
			}
			for _, want := range tc.want {
				if !strings.Contains(sh.err(), want) {
					t.Errorf("the shim ended with %q, want it to name %q", sh.err(), want)
				}
			}
			if child.started() {
				t.Error("the agent was started with no install of the pin")
			}
		})
	}
}

// A tmux pane's environment carries no pin, and a role of a deployment that runs no shared leak
// check carries the image's pin and no checker (leakguard.CheckerVar): the shim of either reads no
// install at all, the agent starts where no install exists, and the shim names no check of its
// own for it.
func TestWithNoPinOrNoCheckerTheAgentStartsWithNoInstall(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  []string
	}{
		{"no pin", nil},
		{"the pin and no checker", []string{leakguard.TagVariable + "=" + theTag}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := socketPath(t)
			daemon := listen(t, path)
			child := newOMP(t, tc.env...)
			cfg := config(t, path, child)
			cfg.LeakCheckDir = filepath.Join(t.TempDir(), "leak-check")
			sh := run(t, cfg, newClock())

			daemon.accept(t).open(t)
			if got, ok := child.environment(t)[leakguard.CheckVar]; ok {
				t.Errorf("the child was told %s=%q, want no check named by the shim; log:\n%s", leakguard.CheckVar, got, sh.log)
			}
			if sh.log.count("the shared public leak check") != 0 {
				t.Errorf("the shim logged an install it should not have read; log:\n%s", sh.log)
			}
		})
	}
}

// The workspace's CodeGraph warm-up (Config.WarmCodegraph) starts on the first line the child
// writes, with the workspace LEGION_WORKSPACE names: not before the ack, not at the spawn — Oh My
// Pi's first frame is its `ready`, once its extensions are loaded — and exactly once for the shim's
// life, so neither a later frame nor a connection redialled while the child runs starts a second
// one. A Config without the hook, a tmux pane's or the controller's, runs nothing.
func TestTheCodegraphWarmUpStartsOnTheChildsFirstFrameOnce(t *testing.T) {
	const workspace = "/legion/workspaces/acme/widgets/widgets-7"
	path := socketPath(t)
	daemon := listen(t, path)
	hold := filepath.Join(t.TempDir(), "ready")
	child := newOMP(t, "LEGION_WORKSPACE="+workspace, "FAKE_OMP_HOLD_READY="+hold)
	var mu sync.Mutex
	var warmed []string
	calls := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(warmed)
	}
	cfg := config(t, path, child)
	cfg.WarmCodegraph = func(_ context.Context, _ procgroup.Starter, dir string, _ time.Time) {
		mu.Lock()
		defer mu.Unlock()
		warmed = append(warmed, dir)
	}
	sh := run(t, cfg, newClock())

	p := daemon.accept(t)
	p.expectHello(t)
	p.send(t, shimwire.HelloAck{})
	sh.log.await(t, "[worker-shim] spawned ", 1)
	testwait.Eventually(t, "the fake OMP to start, held before its first line", child.started)
	if got := calls(); len(got) != 0 {
		t.Fatalf("the warm-up started at the spawn, before the child wrote anything: %v", got)
	}
	if err := os.WriteFile(hold, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	p.expectRaw(t, "fake_ready")
	testwait.Until(t, "the warm-up to start after the child's first frame", func() (string, bool) {
		return "log: " + sh.log.String(), len(calls()) > 0
	})
	if got := calls(); !slices.Equal(got, []string{workspace}) {
		t.Fatalf("the warm-up was started with %v, want once with %q", got, workspace)
	}

	p.send(t, shimwire.Prompt{ID: "p1", DeliveryID: "d1", Message: "go"})
	p.expect(t, shimwire.Response{ID: "p1", Command: "prompt", Success: true})
	p.expect(t, shimwire.AgentStart{})
	p.expect(t, shimwire.AgentEnd{})
	_ = p.conn.Close()
	sh.log.await(t, "reconnecting", 1)
	p = daemon.accept(t)
	p.expectHello(t)
	p.send(t, shimwire.HelloAck{})
	p.send(t, shimwire.GetState{ID: "probe"})
	if raw := p.next(t).(shimwire.Response); raw.ID != "probe" {
		t.Fatalf("the probe was answered as %#v", raw)
	}
	if got := calls(); !slices.Equal(got, []string{workspace}) {
		t.Fatalf("later frames and a redial started the warm-up again: %v", got)
	}

	t.Run("nothing runs without the hook", func(t *testing.T) {
		path := socketPath(t)
		daemon := listen(t, path)
		child := newOMP(t, "LEGION_WORKSPACE="+workspace)
		sh := run(t, config(t, path, child), newClock())
		p := daemon.accept(t)
		p.open(t)
		p.send(t, shimwire.GetState{ID: "probe"})
		if raw := p.next(t).(shimwire.Response); raw.ID != "probe" {
			t.Fatalf("the probe was answered as %#v", raw)
		}
		if sh.log.count("warm-up") != 0 {
			t.Fatalf("a shim with no hook said something of a warm-up; log:\n%s", sh.log)
		}
	})
}

// The shim ends with its child, and the warm-up ends with the shim: the moment the child is told
// to stop the warm-up's context is ended — the build it started stops on that, not at the child's
// exit, so an agent that spends its whole grace ignoring SIGTERM costs the stop nothing more — and
// Run returns the child's status only once the warm-up has returned, so the lease the warm-up
// releases on its way out is gone before the launcher reads the shim as exited. A role stopped
// mid-build (the launcher's one SIGTERM to the whole group) otherwise left `.codegraph/legion-warm.lock`
// behind, and its relaunch within the minute read a live build and skipped its own.
func TestTheShimEndsOnlyOnceAnInFlightWarmUpHasReturned(t *testing.T) {
	path := socketPath(t)
	daemon := listen(t, path)
	child := newOMP(t, "LEGION_WORKSPACE=/legion/workspaces/acme/widgets/widgets-7", "FAKE_OMP_IGNORE_SIGTERM=1")
	var mu sync.Mutex
	var returned time.Time
	ended := make(chan struct{})
	cfg := config(t, path, child)
	cfg.WarmCodegraph = func(ctx context.Context, _ procgroup.Starter, _ string, _ time.Time) {
		<-ctx.Done()
		close(ended)
		// What a real warm-up does here: its codegraph child ends on the same context, it logs the
		// end and releases the lease. That takes time the shim must wait out.
		time.Sleep(300 * time.Millisecond)
		mu.Lock()
		returned = time.Now()
		mu.Unlock()
	}
	clock := newClock()
	sh := run(t, cfg, clock)
	p := daemon.accept(t)
	p.open(t)

	p.send(t, shimwire.Shutdown{})
	p.expectRaw(t, "fake_sigterm")
	expired := clock.next(t, grace)
	// The child is ignoring its SIGTERM and still running; the warm-up has already been ended.
	testwait.Receive(t, "the warm-up's context to end when the child was told to stop", ended)
	expired.elapse()
	if code := sh.wait(t); code != 128+int(syscall.SIGKILL) {
		t.Fatalf("the shim exited %d, want the child's %d", code, 128+int(syscall.SIGKILL))
	}
	exited := time.Now()
	mu.Lock()
	defer mu.Unlock()
	if returned.IsZero() || returned.After(exited) {
		t.Fatalf("Run returned at %s with the warm-up still running (it returned at %s); log:\n%s", exited.Format(time.StampMilli), returned.Format(time.StampMilli), sh.log)
	}
}

// The wait for the warm-up is warmUpDrain (pinned in drain_test.go): what the role's stop grace
// leaves once the child's own grace and the stdout drain are spent, never under MinWarmUpDrain, so
// a warm-up that never returns cannot hold the shim past the grace at which its launcher kills it
// (with the lease then left behind, the failure the wait prevents): Run returns without it,
// saying so.
func TestTheWaitForTheWarmUpIsBoundedByTheStopGrace(t *testing.T) {
	path := socketPath(t)
	daemon := listen(t, path)
	child := newOMP(t, "LEGION_WORKSPACE=/legion/workspaces/acme/widgets/widgets-7")
	cfg := config(t, path, child)
	// grace is the test's 3 s; a 3.5 s stop grace leaves half a second less the 1 s stdout drain:
	// nothing, so the wait is the floor, MinWarmUpDrain. No runtime produces a stop grace this
	// small — the daemon refuses one under MinStopGrace at configuration load — so this is the
	// floor exercised by hand, not a stop any launcher runs.
	cfg.StopGrace = grace + 500*time.Millisecond
	never := make(chan struct{})
	cfg.WarmCodegraph = func(ctx context.Context, _ procgroup.Starter, _ string, _ time.Time) { <-never }
	defer close(never)
	clock := newClock()
	sh := run(t, cfg, clock)
	p := daemon.accept(t)
	p.open(t)

	p.send(t, shimwire.Shutdown{})
	// The kill timer is taken and never fired: the fake agent exits on its SIGTERM.
	clock.next(t, grace)
	clock.next(t, shim.MinWarmUpDrain).elapse()
	if code := sh.wait(t); code != 128+int(syscall.SIGTERM) {
		t.Fatalf("the shim exited %d, want the child's %d", code, 128+int(syscall.SIGTERM))
	}
	if sh.log.count(fmt.Sprintf("the CodeGraph warm-up is still running %v", shim.MinWarmUpDrain)) != 1 {
		t.Fatalf("the shim left without saying the warm-up still ran %v; log:\n%s", shim.MinWarmUpDrain, sh.log)
	}
}

// A daemon that refuses the hello — closing the stream, as it does for an unknown or stale boot
// token — never gets an OMP spawned: the shim redials, re-sending the hello each time, with a
// backoff that doubles from 200 ms to its 5 s cap. A dial that finds no listener counts the same.
func TestARefusedHelloIsRedialledWithBackoffAndNeverSpawnsOMP(t *testing.T) {
	path := socketPath(t)
	child := newOMP(t)
	clock := newClock()
	sh := run(t, config(t, path, child), clock)

	// No listener yet: the daemon is not up.
	clock.next(t, 200*time.Millisecond).elapse()
	last := clock.next(t, 400*time.Millisecond)
	daemon := listen(t, path)
	last.elapse()
	for _, want := range []time.Duration{800, 1600, 3200, 5000, 5000, 5000} {
		p := daemon.accept(t)
		p.expectHello(t)
		_ = p.conn.Close()
		clock.next(t, want*time.Millisecond).elapse()
	}
	if child.started() || sh.log.count("[worker-shim] spawned ") != 0 {
		t.Fatalf("OMP was spawned although the daemon never acked a hello; log:\n%s", sh.log)
	}

	// The shim's own SIGTERM with nothing spawned: it exits as the signal would have.
	daemon.accept(t).expectHello(t)
	sh.cancel()
	if code := sh.wait(t); code != 128+int(syscall.SIGTERM) {
		t.Fatalf("the shim exited %d, want %d", code, 128+int(syscall.SIGTERM))
	}
	if child.started() || sh.log.count("[worker-shim] spawned ") != 0 {
		t.Fatalf("OMP was spawned by a shim told to stop before any ack; log:\n%s", sh.log)
	}
}

// An acked connection clears the backoff: a stream that drops after its ack is redialled at once,
// and the next refusal waits 200 ms again, not where the earlier failures had left the backoff.
func TestAnAckedConnectionResetsTheBackoff(t *testing.T) {
	path := socketPath(t)
	child := newOMP(t)
	clock := newClock()
	run(t, config(t, path, child), clock)

	clock.next(t, 200*time.Millisecond).elapse()
	clock.next(t, 400*time.Millisecond).elapse()
	last := clock.next(t, 800*time.Millisecond)
	daemon := listen(t, path)
	last.elapse()
	p := daemon.accept(t)
	p.open(t)

	_ = p.conn.Close()
	p = daemon.accept(t)
	p.expectHello(t)
	clock.idle(t)
	_ = p.conn.Close()
	clock.next(t, 200*time.Millisecond)
}

// What OMP writes while the daemon is away waits in a backlog of the newest 1000 frames and is
// replayed in order, ahead of anything newer, once the next connection's hello is acked. The
// hello is re-sent on that connection; a stream that dropped after its ack is redialled at once,
// with no backoff; and overflowing the backlog is logged once for the shim's life.
func TestFramesFromADroppedConnectionReplayInOrderAfterTheNextAck(t *testing.T) {
	path := socketPath(t)
	daemon := listen(t, path)
	child := newOMP(t)
	clock := newClock()
	sh := run(t, config(t, path, child), clock)
	p := daemon.accept(t)
	pid := p.open(t)

	for round := 1; round <= 2; round++ {
		_ = p.conn.Close()
		sh.log.await(t, "reconnecting", round)
		// 1200 frames and the end of a turn, all while no daemon is connected: the oldest 201
		// cannot be kept.
		if err := syscall.Kill(pid, syscall.SIGUSR1); err != nil {
			t.Fatalf("signal the fake OMP: %v", err)
		}
		sh.log.awaitLine(t, "agent_end", round)

		p = daemon.accept(t)
		p.expectHello(t)
		p.send(t, shimwire.HelloAck{})
		for want := 201; want < 1200; want++ {
			var frame struct{ N int }
			if err := json.Unmarshal(p.expectRaw(t, "fake_frame").JSON, &frame); err != nil {
				t.Fatalf("decode a replayed frame: %v", err)
			}
			if frame.N != want {
				t.Fatalf("round %d replayed frame %d where frame %d was due", round, frame.N, want)
			}
		}
		p.expect(t, shimwire.AgentEnd{})
	}
	if got := sh.log.count("dropping oldest frames beyond the last 1000"); got != 1 {
		t.Fatalf("the backlog overflow was logged %d times, want once for the shim's life; log:\n%s", got, sh.log)
	}
	clock.idle(t)
}

// The daemon's `shutdown` is the shim's to act on, never OMP's to read: the child is sent
// SIGTERM, and killed if it is still running when the grace runs out. The shim's own SIGTERM —
// the pane or the pod being stopped — ends the child the same way. The bridge keeps working
// through the grace, so whatever OMP says on its way out still reaches the daemon.
func TestShutdownSendsSIGTERMThenKillsTheChildAfterTheGrace(t *testing.T) {
	for _, tc := range []struct {
		name    string
		trigger func(*testing.T, *running, *peer)
	}{
		{"the daemon's shutdown frame", func(t *testing.T, _ *running, p *peer) { p.send(t, shimwire.Shutdown{}) }},
		{"the shim's own SIGTERM", func(_ *testing.T, sh *running, _ *peer) { sh.cancel() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := socketPath(t)
			daemon := listen(t, path)
			child := newOMP(t, "FAKE_OMP_IGNORE_SIGTERM=1")
			clock := newClock()
			sh := run(t, config(t, path, child), clock)
			p := daemon.accept(t)
			p.open(t)

			tc.trigger(t, sh, p)
			p.expectRaw(t, "fake_sigterm")
			expired := clock.next(t, grace)
			p.send(t, shimwire.GetState{ID: "during-grace"})
			if got := p.next(t).(shimwire.Response); got.ID != "during-grace" {
				t.Fatalf("the bridge answered %#v during the grace", got)
			}
			if sh.exited() {
				t.Fatal("the shim exited before the grace ran out")
			}

			expired.elapse()
			if code := sh.wait(t); code != 128+int(syscall.SIGKILL) {
				t.Fatalf("the shim exited %d, want %d (the child killed)", code, 128+int(syscall.SIGKILL))
			}
			if got := frameTypes(child.received(t)); !reflect.DeepEqual(got, []string{shimwire.TypeGetState}) {
				t.Fatalf("OMP read %v; shutdown must never be forwarded", got)
			}
		})
	}
}

// A child that honours SIGTERM ends within the grace, and the shim exits with its status.
func TestShutdownOfAChildThatHonoursSIGTERMExitsWithItsStatus(t *testing.T) {
	path := socketPath(t)
	daemon := listen(t, path)
	child := newOMP(t)
	clock := newClock()
	sh := run(t, config(t, path, child), clock)
	p := daemon.accept(t)
	p.open(t)

	p.send(t, shimwire.Shutdown{})
	clock.next(t, grace)
	if code := sh.wait(t); code != 128+int(syscall.SIGTERM) {
		t.Fatalf("the shim exited %d, want %d", code, 128+int(syscall.SIGTERM))
	}
}

// fakeJJ writes a `jj` that records how it was run and then does what mode says, in a directory
// of its own put first on PATH: the shim runs PATH's jj, the one the pane's own shell runs. In
// mode hang, before anything else, it sets a SIGTERM trap that marks the stop's arrival
// (termedMark) and exits, and marks its own start (startedMark); once its record is whole, it
// marks so (recordedMark) and runs until the test's cleanup writes its release, or until this test
// process is gone: never for a span of its own, so no answer comes, and no assertion holds, by the
// jj's own exit.
func fakeJJ(t *testing.T) (record string) {
	t.Helper()
	record = filepath.Join(t.TempDir(), "jj-invocation")
	bin := t.TempDir()
	release := filepath.Join(t.TempDir(), "release")
	t.Cleanup(func() {
		if err := os.WriteFile(release, nil, 0o600); err != nil {
			t.Errorf("release the hanging jj: %v", err)
		}
	})
	script := `#!/bin/sh
if [ "$FAKE_JJ_MODE" = hang ]; then
  trap 'read -r up _ </proc/uptime; read -r stat </proc/$$/stat; printf "%s %s\n" "$up" "$stat" >"` + termedMark("$FAKE_JJ_RECORD") + `"; exit 143' TERM
  read -r up _ </proc/uptime; read -r stat </proc/$$/stat; printf '%s %s\n' "$up" "$stat" >"` + startedMark("$FAKE_JJ_RECORD") + `"
fi
printf '%s\n' "$@" >> "$FAKE_JJ_RECORD"
printf 'JJ_USER=%s\nJJ_EMAIL=%s\n' "$JJ_USER" "$JJ_EMAIL" >> "$FAKE_JJ_RECORD"
case "$FAKE_JJ_MODE" in
fail) echo "Error: the working copy is stale" >&2; exit 3 ;;
hang)
  : >"` + recordedMark("$FAKE_JJ_RECORD") + `"
  while [ ! -e '` + release + `' ] && kill -0 ` + fmt.Sprint(os.Getpid()) + ` 2>/dev/null; do sleep 0.05; done ;;
esac
`
	if err := execfile.Write(filepath.Join(bin, "jj"), []byte(script), 0o700); err != nil {
		t.Fatalf("write the fake jj: %v", err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return record
}

// recordedMark is the file fakeJJ writes in mode hang once its record is whole.
func recordedMark(record string) string { return record + ".recorded" }

// startedMark is the file fakeJJ writes in mode hang as its first act, and termedMark the one its
// SIGTERM trap writes. Each holds the jj's reading of /proc/uptime as it wrote the mark and its own
// /proc/<pid>/stat line, whose starttime the kernel set when the shim started the jj (jjHeld).
func startedMark(record string) string { return record + ".started" }
func termedMark(record string) string  { return record + ".termed" }

// jjHeld answers how long after the shim started fakeJJ the latest mark the jj wrote came: its
// termedMark where its trap took the budget's SIGTERM, else its startedMark. Both readings are on
// the kernel's boot clock: the uptime in seconds, and the starttime (field 22 of the stat line) in
// clock ticks, 100 to the second (USER_HZ). ok is false where the jj wrote neither mark, or the
// mark does not read as one.
func jjHeld(record string) (held time.Duration, ok bool) {
	for _, mark := range []string{termedMark(record), startedMark(record)} {
		raw, err := os.ReadFile(mark)
		if err != nil {
			continue
		}
		uptime, stat, cut := strings.Cut(strings.TrimSpace(string(raw)), " ")
		end := strings.LastIndexByte(stat, ')')
		if !cut || end < 0 {
			return 0, false
		}
		// Field 3 (state) onwards, so starttime (field 22) is index 22-3.
		fields := strings.Fields(stat[end+1:])
		const starttime = 22 - 3
		if len(fields) <= starttime {
			return 0, false
		}
		seconds, err := strconv.ParseFloat(uptime, 64)
		ticks, tickErr := strconv.ParseUint(fields[starttime], 10, 64)
		if err != nil || tickErr != nil {
			return 0, false
		}
		return time.Duration(seconds*float64(time.Second)) - time.Duration(ticks)*time.Second/100, true
	}
	return 0, false
}

// farBudget is the TimeoutMs of a request whose budget is not a row's subject: the rest of the
// test binary's deadline (testwait.Remaining). A budget is counted from when the shim reads the
// request, before its commands start, so a fixed one can pass on a loaded machine before they
// have done their work; this one is far from their own end, and the answer is read as every frame
// is, under the shared wait's deadline (peer.next).
func farBudget(t *testing.T) int {
	t.Helper()
	return int(testwait.Remaining(t).Milliseconds())
}

// timedOutOnceRecorded sends request, whose budget is the row's subject, to a shim whose PATH's
// jj is fakeJJ in mode hang, and answers the shim's answer from a run it can judge. A run whose
// jj wrote its whole record (recordedMark) before the budget's stop reached it is judged, and so is
// a run whose answer is no timeout. A run whose stop cut the jj before its record is judged too, so
// a budget that cuts the jj too soon (counted from too early, or shorter than TimeoutMs) fails the
// row, unless the jj's own marks show that a loaded machine held it: its latest mark came more than
// half its budget after the shim started it (jjHeld), its record still unwritten. A budget counted
// from the shim's read passes no sooner than its TimeoutMs after the send, and a jj held that long
// from its start can be cut by such a budget before its record; a jj whose start takes the
// milliseconds a shell's start takes cannot. Only such a run is sent again, until a run of the
// right order comes, within a minute. A jj that wrote no mark, which the stop met before its first
// line or which never started, is judged: nothing of its own says it was held.
func timedOutOnceRecorded(t *testing.T, p *peer, request shimwire.AdoptWorkingCopy, record string) shimwire.Frame {
	t.Helper()
	budget := time.Duration(request.TimeoutMs) * time.Millisecond
	recorded := recordedMark(record)
	within := testwait.Bounded(t, time.Minute, budget+procgroup.StopGrace+procgroup.WaitDelay+time.Second)
	until := time.Now().Add(within)
	for runs := 1; ; runs++ {
		for _, path := range []string{record, recorded, startedMark(record), termedMark(record)} {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				t.Fatalf("clear the jj's record before run %d: %v", runs, err)
			}
		}
		p.send(t, request)
		// The shim answers once the budget's stop has ended the jj, so every mark it wrote is there.
		answer := p.next(t)
		if _, err := os.Stat(recorded); err == nil {
			return answer
		}
		if result, ok := answer.(shimwire.AdoptWorkingCopyResult); !ok || !strings.Contains(result.Error, "Command timed out") {
			return answer
		}
		held, ok := jjHeld(record)
		if !ok || held <= budget/2 {
			return answer
		}
		if time.Now().Before(until) {
			t.Logf("run %d is not judged: the budget's stop came before the jj's whole record, and the jj's own marks show it held %s after the shim started it, past half its %s budget; sent again", runs, held, budget)
			continue
		}
		t.Fatalf("no run of the right order in %d runs over %s: in each, the budget's stop came before the jj's whole record, and the jj's own marks show it held past half its %s budget after the shim started it", runs, within, budget)
	}
}

// `adopt-working-copy` is answered by the shim itself: the one shared adoption command, run by
// the jj on the shim's PATH on the workspace LEGION_WORKSPACE names — never the directory the
// shim happens to run in — under the identity the daemon named and within its budget, and OMP
// never sees the frame. A shim missing LEGION_WORKSPACE refuses, naming it (a PATH without jj is
// TestAdoptWorkingCopyRunsTheJJOnPATHWithNoLEGIONJJPATH's case). A failure answers `ok: false`
// with the command's report rather than taking the agent down. A row whose request names no
// TimeoutMs runs under farBudget, its budget no subject of it. The row whose jj outlives its
// budget is judged on every run but one in which the jj's own marks show a loaded machine held it
// past half its budget, its record unwritten (timedOutOnceRecorded).
func TestAdoptWorkingCopyRunsTheSharedMetaeditAndAnswersTheDaemon(t *testing.T) {
	for _, tc := range []struct {
		name        string
		mode        string
		noWorkspace bool
		request     shimwire.AdoptWorkingCopy
		ok          bool
		errorHas    []string
		ranJJ       bool
	}{
		{
			name:    "a clean run",
			request: shimwire.AdoptWorkingCopy{ID: "a1", JJUser: "legion-implementer[bot]", JJEmail: "implementer@example.test"},
			ok:      true,
			ranJJ:   true,
		},
		{
			name:     "jj fails",
			mode:     "fail",
			request:  shimwire.AdoptWorkingCopy{ID: "a2", JJUser: "u", JJEmail: "u@example.test"},
			errorHas: []string{"Command failed (exit 3)", "jj log", "the working copy is stale"},
			ranJJ:    true,
		},
		{
			name:     "jj outlives the budget",
			mode:     "hang",
			request:  shimwire.AdoptWorkingCopy{ID: "a3", JJUser: "u", JJEmail: "u@example.test", TimeoutMs: 200},
			errorHas: []string{"Command timed out after 0.2 s", "jj log"},
			ranJJ:    true,
		},
		{
			name:     "a request without an identity",
			request:  shimwire.AdoptWorkingCopy{ID: "a4", TimeoutMs: 5000},
			errorHas: []string{"no jj identity"},
		},
		{
			name:        "a shim with no LEGION_WORKSPACE",
			noWorkspace: true,
			request:     shimwire.AdoptWorkingCopy{ID: "a5", JJUser: "u", JJEmail: "u@example.test", TimeoutMs: 5000},
			errorHas:    []string{"LEGION_WORKSPACE is not set"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			record := fakeJJ(t)
			path := socketPath(t)
			daemon := listen(t, path)
			child := newOMP(t)
			cfg := config(t, path, child)
			// The workspace is a directory of its own: not the test process's working
			// directory, which is the shim's.
			workspace := t.TempDir()
			cfg.Env = append(fixtureenv.Isolated(t.TempDir()), fakeOMPEnv+"=1", "FAKE_OMP_MARKER="+child.marker,
				"FAKE_OMP_LOG="+child.log, "FAKE_JJ_RECORD="+record, "FAKE_JJ_MODE="+tc.mode)
			if !tc.noWorkspace {
				cfg.Env = append(cfg.Env, "LEGION_WORKSPACE="+workspace)
			}
			run(t, cfg, newClock())
			p := daemon.accept(t)
			p.open(t)

			request := tc.request
			if request.TimeoutMs == 0 {
				request.TimeoutMs = farBudget(t)
			}
			var answer shimwire.Frame
			if tc.mode == "hang" {
				answer = timedOutOnceRecorded(t, p, request, record)
			} else {
				p.send(t, request)
				answer = p.next(t)
			}
			got, ok := answer.(shimwire.AdoptWorkingCopyResult)
			if !ok || got.ID != request.ID || got.OK != tc.ok {
				t.Fatalf("the shim answered %#v, want a result for %s with ok=%v", got, request.ID, tc.ok)
			}
			for _, want := range tc.errorHas {
				if !strings.Contains(got.Error, want) {
					t.Fatalf("the result's error %q does not mention %q", got.Error, want)
				}
			}

			invocation, err := os.ReadFile(record)
			if !tc.ranJJ {
				if err == nil {
					t.Fatalf("jj ran for a request the shim should have refused: %s", invocation)
				}
			} else {
				if err != nil {
					t.Fatalf("jj never ran: %v", err)
				}
				identity := []string{"JJ_USER=" + tc.request.JJUser, "JJ_EMAIL=" + tc.request.JJEmail}
				read := append([]string{"log", "-r", `@ ~ description(exact:"")`, "--no-graph", "-T", "commit_id", "-R", workspace}, identity...)
				metaedit := append([]string{"metaedit", "--update-author", "-r", `@ & description(exact:"")`, "-R", workspace}, identity...)
				want := read
				if tc.ok {
					// The fake prints no commit id: the working copy is undescribed, so it is re-authored.
					want = append(read, metaedit...)
				}
				if got := strings.Join(want, "\n") + "\n"; string(invocation) != got {
					t.Fatalf("jj ran as\n%s\nwant the read, then the shared metaedit, under the requested identity\n%s", invocation, got)
				}
			}

			p.send(t, shimwire.GetState{ID: "after"})
			p.next(t)
			if got := frameTypes(child.received(t)); !reflect.DeepEqual(got, []string{shimwire.TypeGetState}) {
				t.Fatalf("OMP read %v; adopt-working-copy is the shim's own", got)
			}
		})
	}
}

// An older daemon still asks a pod's shim for the push check it ran before it replaced the pod,
// in a frame of a type this shim no longer knows. The shim passes that line to OMP unchanged, as
// it does every frame that is not its own. OMP refuses it as a command it does not know, and an
// older daemon reads that refusal as a shim that predates the check, so it replaces the pod on
// its workers' answers alone. A shim that dropped the line would leave that daemon with no
// answer, which it reads as a check that failed, and the pod would not be replaced.
func TestShimForwardsAnUnknownRescueFrameToItsChild(t *testing.T) {
	path := socketPath(t)
	daemon := listen(t, path)
	child := newOMP(t)
	run(t, config(t, path, child), newClock())
	p := daemon.accept(t)
	p.open(t)

	const older = `{"type":"rescue-unpushed","id":"r1"}`
	p.sendLine(t, older)
	p.send(t, shimwire.GetState{ID: "after"})
	if answer, ok := p.next(t).(shimwire.Response); !ok || answer.ID != "after" {
		t.Fatalf("the shim sent %#v, want OMP's answer to the get_state after the older daemon's frame", answer)
	}
	got := child.received(t)
	if len(got) != 2 || got[1].FrameType() != shimwire.TypeGetState {
		t.Fatalf("OMP read %v, want the older daemon's frame and then the get_state", frameTypes(got))
	}
	if raw, ok := got[0].(shimwire.Raw); !ok || string(raw.JSON) != older {
		t.Fatalf("OMP read %#v, want the older daemon's line unchanged: %s", got[0], older)
	}
}

// No runtime tells a pane or a pod LEGION_JJ_PATH (internal/api/version.go, the LEGION-631 entry;
// docs/kubernetes.md, "No container is told …"): a pane's jj is its PATH's, the one every role's
// plain `jj git push` runs, and the shim's adoption runs that same jj. A PATH without one refuses
// naming jj and the PATH it searched — never a variable no runtime sets, whose absence stopped every
// role of every pod at its first jj operation (stage 4a's adopt-working-copy at 18dc1df2,
// dispatch://LEGION-631/comment/b376473d-a4cb-492f-ac7d-e66b26f2bff1).
func TestAdoptWorkingCopyRunsTheJJOnPATHWithNoLEGIONJJPATH(t *testing.T) {
	for _, tc := range []struct {
		name     string
		jjOnPath bool
		ok       bool
	}{
		{name: "the PATH's jj adopts the working copy", jjOnPath: true, ok: true},
		{name: "a PATH without jj refuses, naming jj and the PATH"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// PATH is one directory of the test's own: no other jj (the image's, a devbox's) can
			// answer the lookup, and nothing an adoption runs needs another directory (the fake OMP
			// is os.Args[0], absolute; the fake jj's interpreter is /bin/sh).
			bin := t.TempDir()
			record := filepath.Join(t.TempDir(), "jj-invocation")
			if tc.jjOnPath {
				script := "#!/bin/sh\nprintf '%s\\n' \"$@\" >> \"$FAKE_JJ_RECORD\"\nprintf 'JJ_USER=%s\\nJJ_EMAIL=%s\\n' \"$JJ_USER\" \"$JJ_EMAIL\" >> \"$FAKE_JJ_RECORD\"\n"
				if err := execfile.Write(filepath.Join(bin, "jj"), []byte(script), 0o700); err != nil {
					t.Fatalf("write the fake jj: %v", err)
				}
			}
			t.Setenv("PATH", bin)
			path := socketPath(t)
			daemon := listen(t, path)
			child := newOMP(t)
			cfg := config(t, path, child)
			workspace := t.TempDir()
			cfg.Env = append(fixtureenv.Isolated(t.TempDir()), fakeOMPEnv+"=1", "FAKE_OMP_MARKER="+child.marker,
				"FAKE_OMP_LOG="+child.log, "FAKE_JJ_RECORD="+record, "LEGION_WORKSPACE="+workspace)
			run(t, cfg, newClock())
			p := daemon.accept(t)
			p.open(t)

			request := shimwire.AdoptWorkingCopy{ID: "path", JJUser: "legion-implementer[bot]", JJEmail: "implementer@example.test", TimeoutMs: farBudget(t)}
			p.send(t, request)
			got, ok := p.next(t).(shimwire.AdoptWorkingCopyResult)
			if !ok || got.ID != request.ID || got.OK != tc.ok {
				t.Fatalf("the shim answered %#v, want a result for %s with ok=%v", got, request.ID, tc.ok)
			}
			invocation, err := os.ReadFile(record)
			if !tc.ok {
				if strings.Contains(got.Error, "LEGION_JJ_PATH") {
					t.Fatalf("the refusal %q names LEGION_JJ_PATH, which no runtime sets", got.Error)
				}
				if !strings.Contains(got.Error, "jj") || !strings.Contains(got.Error, bin) {
					t.Fatalf("the refusal %q does not name jj and the PATH searched (%s)", got.Error, bin)
				}
				if err == nil {
					t.Fatalf("jj ran on a PATH without one: %s", invocation)
				}
				return
			}
			if err != nil {
				t.Fatalf("the PATH's jj never ran: %v", err)
			}
			identity := []string{"JJ_USER=" + request.JJUser, "JJ_EMAIL=" + request.JJEmail}
			read := append([]string{"log", "-r", `@ ~ description(exact:"")`, "--no-graph", "-T", "commit_id", "-R", workspace}, identity...)
			metaedit := append([]string{"metaedit", "--update-author", "-r", `@ & description(exact:"")`, "-R", workspace}, identity...)
			if want := strings.Join(append(read, metaedit...), "\n") + "\n"; string(invocation) != want {
				t.Fatalf("the PATH's jj ran as\n%s\nwant the read, then the shared metaedit, under the requested identity\n%s", invocation, want)
			}
		})
	}
}

// Every role of an issue shares one workspace, so a role that left its working copy described (the
// implementer pushed @ as its commit) would have the next role's work land in that commit, authored
// by the previous role's App. Before a task reaches the agent, the adoption starts the incoming role
// on a fresh working copy of its own identity, leaving the described commit and its author as they
// were; an undescribed working copy is only re-authored, which rewrites its author and nothing else:
// it stays the change it was, holding what it held. Either way the adoption is one read of the
// working copy and the one change it needs.
//
// INFRA-53's case (R3 of dispatch://INFRA-2695): an empty undescribed working copy the previous
// phase role left, as a `jj split` of its work into a described commit leaves one, becomes the
// incoming phase role's, and the described commit under it keeps its id and its author.
func TestAdoptWorkingCopyStartsTheIncomingRoleOnAFreshCommitOverADescribedOne(t *testing.T) {
	jj := fixturegit.CompiledJJ(t)
	for _, tc := range []struct {
		name       string
		leave      []string // what the previous role ran once README.md was written, nil for nothing
		pushed     string   // the previous role's described commit before the adoption, "" for none
		empty      bool     // whether the working copy the previous role left holds no change
		reauthored bool     // whether the adoption re-authors that working copy rather than start a fresh one
		ran        string
	}{
		{name: "a described working copy", leave: []string{"describe", "-m", "feat: the pushed change"}, pushed: "@", ran: "log\nnew\n"},
		{name: "an undescribed working copy", reauthored: true, ran: "log\nmetaedit\n"},
		{name: "an empty working copy a split left", leave: []string{"split", "-m", "feat: the pushed change", "README.md"}, pushed: "@-", empty: true, reauthored: true, ran: "log\nmetaedit\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The test process's environment (a devbox's jj configuration, a pod's workspace and
			// credential) must not reach the test.
			home := t.TempDir()
			workspace := t.TempDir()
			jjAs := func(user string, args ...string) string {
				t.Helper()
				command := exec.Command(jj, append(args, "-R", workspace)...)
				// -R names the repository, not a working directory: the helper runs in the
				// workspace it reads, since jj searches upward from the directory it runs in for
				// the closest .jj, and the test process stands in a real workspace in a pod.
				command.Dir = workspace
				command.Env = append(fixtureenv.Isolated(home), "JJ_USER="+user, "JJ_EMAIL="+user+"@example.test")
				output, err := command.CombinedOutput()
				if err != nil {
					t.Fatalf("jj %v: %v\n%s", args, err, output)
				}
				return strings.TrimSpace(string(output))
			}
			// The init takes no -R and opens no workspace, so with no Dir jj would walk up from
			// the test process's directory to the closest .jj and write that repository.
			fixturegit.Isolated(t, home).With("JJ_USER=implementer", "JJ_EMAIL=implementer@example.test").
				Run(workspace, append([]string{jj}, fixturegit.JJInit(workspace, true)[1:]...)...)
			if err := os.WriteFile(filepath.Join(workspace, "README.md"), []byte("smoke\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if tc.leave != nil {
				jjAs("implementer", tc.leave...)
			}
			var change string
			if tc.reauthored {
				change = jjAs("implementer", "log", "-r", "@", "--no-graph", "-T", "change_id")
			}
			var pushed string
			if tc.pushed != "" {
				pushed = jjAs("implementer", "log", "-r", tc.pushed, "--no-graph", "-T", "commit_id")
			}
			// The shim's jj, recording each command it runs: first on PATH, where the shim looks. It
			// hands the real jj the PATH past its own directory, so a jj on PATH that is itself a
			// wrapper looking jj up on PATH never finds this one again.
			record := filepath.Join(t.TempDir(), "jj-commands")
			bin := t.TempDir()
			if err := execfile.Write(filepath.Join(bin, "jj"), []byte("#!/bin/sh\nprintf '%s\\n' \"$1\" >> "+record+"\nPATH=\"${PATH#*:}\" exec "+jj+" \"$@\"\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

			path := socketPath(t)
			daemon := listen(t, path)
			child := newOMP(t)
			cfg := config(t, path, child)
			cfg.Env = append(fixtureenv.Isolated(home), fakeOMPEnv+"=1", "FAKE_OMP_MARKER="+child.marker,
				"FAKE_OMP_LOG="+child.log, "LEGION_WORKSPACE="+workspace)
			run(t, cfg, newClock())
			p := daemon.accept(t)
			p.open(t)
			p.send(t, shimwire.AdoptWorkingCopy{ID: "adopt", JJUser: "tester", JJEmail: "tester@example.test", TimeoutMs: farBudget(t)})
			if got, ok := p.next(t).(shimwire.AdoptWorkingCopyResult); !ok || !got.OK {
				t.Fatalf("the shim answered %#v, want a successful adoption", got)
			}
			if ran, err := os.ReadFile(record); err != nil || string(ran) != tc.ran {
				t.Fatalf("the adoption ran jj %q (%v), want %q", ran, err, tc.ran)
			}

			author := jjAs("implementer", "log", "-r", "@", "--no-graph", "-T", `author.name() ++ "|" ++ description`)
			if author != "tester|" {
				t.Fatalf("the working copy after adoption is %q, want an undescribed commit authored by tester", author)
			}
			if tc.reauthored {
				want := fmt.Sprintf("%s %t", change, tc.empty)
				if got := jjAs("implementer", "log", "-r", "@", "--no-graph", "-T", `change_id ++ " " ++ empty`); got != want {
					t.Fatalf("the re-authored working copy is %q (change id, empty), want %q: the change the previous role left, holding what it held", got, want)
				}
			}
			if tc.pushed == "" {
				return
			}
			parent := jjAs("implementer", "log", "-r", "@-", "--no-graph", "-T", `commit_id ++ " " ++ author.name() ++ " " ++ description.first_line()`)
			if parent != pushed+" implementer feat: the pushed change" {
				t.Fatalf("the adopted working copy's parent is %q, want the described commit %s kept, authored by implementer", parent, pushed)
			}
		})
	}
}

// jj reads the repository of the directory it runs in as well as the one -R names, and migrates
// that repository's legacy configuration (.jj/repo/config.toml) into the config home, writing a
// config-id in its place. The shim's own directory is a repository of its own (in a pod, the
// role's workspace; on a devbox, a checkout), so the adoption runs in the workspace it adopts and
// leaves the repository the shim runs in as it was.
func TestAdoptWorkingCopyLeavesTheRepositoryTheShimRunsInUntouched(t *testing.T) {
	jj := fixturegit.CompiledJJ(t)
	home := t.TempDir()
	initialize := func(dir string) {
		t.Helper()
		fixturegit.Isolated(t, home).Run(dir, append([]string{jj}, fixturegit.JJInit(dir, true)[1:]...)...)
	}
	decoy, workspace := t.TempDir(), t.TempDir()
	initialize(decoy)
	initialize(workspace)
	// The decoy keeps its configuration as a jj before the per-repository config home did, the
	// layout jj migrates on the first command that loads the repository.
	if err := os.Remove(filepath.Join(decoy, ".jj", "repo", "config-id")); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(decoy, ".jj", "repo", "config.toml"), []byte("[ui]\ncolor = \"never\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := treeOf(t, decoy)
	t.Chdir(decoy)

	path := socketPath(t)
	daemon := listen(t, path)
	child := newOMP(t)
	cfg := config(t, path, child)
	cfg.Env = append(fixtureenv.Isolated(home), fakeOMPEnv+"=1", "FAKE_OMP_MARKER="+child.marker,
		"FAKE_OMP_LOG="+child.log, "LEGION_WORKSPACE="+workspace)
	run(t, cfg, newClock())
	p := daemon.accept(t)
	p.open(t)
	p.send(t, shimwire.AdoptWorkingCopy{ID: "adopt", JJUser: "tester", JJEmail: "tester@example.test", TimeoutMs: farBudget(t)})
	if got, ok := p.next(t).(shimwire.AdoptWorkingCopyResult); !ok || !got.OK {
		t.Fatalf("the shim answered %#v, want a successful adoption", got)
	}
	after := treeOf(t, decoy)
	var changed []string
	for rel, was := range before {
		if now, ok := after[rel]; !ok {
			changed = append(changed, "removed "+rel)
		} else if now != was {
			changed = append(changed, "changed "+rel)
		}
	}
	for rel := range after {
		if _, ok := before[rel]; !ok {
			changed = append(changed, "added "+rel)
		}
	}
	if len(changed) > 0 {
		slices.Sort(changed)
		t.Fatalf("the adoption wrote the repository the shim runs in: %s", strings.Join(changed, ", "))
	}
}

// treeOf is every path under root with its content: a file's bytes, a symlink's target, or "dir".
func treeOf(t *testing.T, root string) map[string]string {
	t.Helper()
	tree := map[string]string{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(path)
			tree[rel] = "-> " + target
			return err
		case info.IsDir():
			tree[rel] = "dir"
			return nil
		}
		body, err := os.ReadFile(path)
		tree[rel] = string(body)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return tree
}

// A git lock an interrupted git or jj left in a running pod's workspace fails the adoption on
// every write of what it locks, and the role that reads the failure removes nothing: the launcher
// of the pod's roles container sweeps such a lock once no process of the container holds it open
// and it is more than 15 minutes old (internal/launcher's gitLockSweep). The failure names the
// lock and how long ago it was last written, says what that sweep does and how long it takes,
// tells the role to run the command again, and says what holds under tmux, whether jj's own git
// backend reported it (the index jj rewrites as it snapshots the working copy, as in LEGION-731)
// or a git command jj ran; and the lock stays.
func TestAdoptWorkingCopyNamesTheStaleGitLockItFailedOn(t *testing.T) {
	jj := fixturegit.CompiledJJ(t)
	for _, tc := range []struct {
		name string
		// script is the PATH's jj, given the real jj and the lock.
		script func(jj, lock string) string
	}{
		{"jj's own index write", func(jj, _ string) string { return "#!/bin/sh\nPATH=\"${PATH#*:}\" exec " + jj + " \"$@\"\n" }},
		{"a git jj ran", func(_, lock string) string {
			return "#!/bin/sh\necho \"fatal: Unable to create '" + lock + "': File exists.\" >&2\nexit 1\n"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			workspace := t.TempDir()
			// The init takes no -R and opens no workspace, so with no Dir jj would walk up from
			// the test process's directory to the closest .jj and write that repository.
			fixturegit.Isolated(t, home).With("JJ_USER=implementer", "JJ_EMAIL=implementer@example.test").
				Run(workspace, append([]string{jj}, fixturegit.JJInit(workspace, true)[1:]...)...)
			// A file the adoption's first jj snapshots, so jj rewrites the git index.
			if err := os.WriteFile(filepath.Join(workspace, "README.md"), []byte("smoke\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			lock := filepath.Join(workspace, ".git", "index.lock")
			if err := os.WriteFile(lock, nil, 0o644); err != nil {
				t.Fatal(err)
			}
			written := time.Now().Add(-31 * time.Minute)
			if err := os.Chtimes(lock, written, written); err != nil {
				t.Fatal(err)
			}
			bin := t.TempDir()
			if err := execfile.Write(filepath.Join(bin, "jj"), []byte(tc.script(jj, lock)), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

			path := socketPath(t)
			daemon := listen(t, path)
			child := newOMP(t)
			cfg := config(t, path, child)
			cfg.Env = append(fixtureenv.Isolated(home), fakeOMPEnv+"=1", "FAKE_OMP_MARKER="+child.marker,
				"FAKE_OMP_LOG="+child.log, "LEGION_WORKSPACE="+workspace)
			run(t, cfg, newClock())
			p := daemon.accept(t)
			p.open(t)
			p.send(t, shimwire.AdoptWorkingCopy{ID: "adopt", JJUser: "tester", JJEmail: "tester@example.test", TimeoutMs: farBudget(t)})
			got, ok := p.next(t).(shimwire.AdoptWorkingCopyResult)
			if !ok || got.OK {
				t.Fatalf("the shim answered %#v, want a failed adoption", got)
			}
			for _, want := range []string{
				"Command failed (exit 1)",
				"git lock " + lock + " was last written 31m",
				"The launcher of the issue's pod removes a git lock that no process of the pod holds open and that is more than 15 minutes old, and it looks again every minute.",
				"Run the command again in about 16 minutes.",
				"Under the tmux runtime nothing removes it",
				"If the lock is still there after that time, tell the issue's architect.",
			} {
				if !strings.Contains(got.Error, want) {
					t.Errorf("the result's error %q does not mention %q", got.Error, want)
				}
			}
			if _, err := os.Lstat(lock); err != nil {
				t.Fatalf("the lock after the failed adoption: %v, want it left in place", err)
			}
		})
	}
}

// An adoption that fails answers its reason to the daemon on the connection it was asked on,
// however much jj wrote to its standard error. The answer goes out as one line, and an error
// longer than one line can hold would make an answer the writer refuses: the shim would close the
// connection and send that line first again at each reconnect, so the role's stream would stay
// down (ASVS v5.0.0-15.2.2). The answer keeps the start of the reason, which names the command and
// its exit, and its end, where the note about a stale git lock goes, which tells the role what to
// do; it says how many bytes were cut between. The connection then carries the next answer.
func TestAdoptWorkingCopyFailureOverOneLineKeepsItsStartAndItsLockNote(t *testing.T) {
	workspace := t.TempDir()
	lock := filepath.Join(workspace, ".git", "index.lock")
	if err := os.MkdirAll(filepath.Dir(lock), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lock, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	written := time.Now().Add(-31 * time.Minute)
	if err := os.Chtimes(lock, written, written); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	script := fmt.Sprintf("#!/bin/sh\necho 'Error: the working copy is stale' >&2\nhead -c %d /dev/zero | tr '\\0' x >&2\n"+
		"echo >&2\necho \"fatal: Unable to create '%s': File exists.\" >&2\nexit 1\n", 2*shimwire.MaxFrameBytes, lock)
	if err := execfile.Write(filepath.Join(bin, "jj"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	path := socketPath(t)
	daemon := listen(t, path)
	child := newOMP(t)
	cfg := config(t, path, child)
	cfg.Env = append(fixtureenv.Isolated(t.TempDir()), fakeOMPEnv+"=1", "FAKE_OMP_MARKER="+child.marker,
		"FAKE_OMP_LOG="+child.log, "LEGION_WORKSPACE="+workspace)
	run(t, cfg, newClock())
	p := daemon.accept(t)
	p.open(t)
	p.send(t, shimwire.AdoptWorkingCopy{ID: "adopt", JJUser: "tester", JJEmail: "tester@example.test", TimeoutMs: farBudget(t)})
	got, ok := p.next(t).(shimwire.AdoptWorkingCopyResult)
	if !ok || got.ID != "adopt" || got.OK {
		t.Fatalf("the shim answered %#v, want the failed adoption", got)
	}
	// The shim keeps 4 KiB of a reason and a line that says what it cut (shimwire.CutAnswerError).
	if len(got.Error) > 8<<10 {
		t.Fatalf("the failed adoption's reason has %d bytes, want it cut to about 4 KiB", len(got.Error))
	}
	for _, want := range []string{
		"Command failed (exit 1)",
		"the working copy is stale",
		"bytes of this error were cut here",
		"git lock " + lock + " was last written 31m",
		"Run the command again in about 16 minutes.",
	} {
		if !strings.Contains(got.Error, want) {
			t.Errorf("the failed adoption's reason %.300q ... %.300q does not say %q", got.Error, got.Error[max(len(got.Error)-300, 0):], want)
		}
	}
	p.send(t, shimwire.GetState{ID: "after"})
	if answer, ok := p.next(t).(shimwire.Response); !ok || answer.ID != "after" {
		t.Fatalf("the shim sent %#v, want OMP's answer to the get_state after the adoption on the same connection", answer)
	}
}

// The jj every role runs, and the one CI tests with, is the pin .github/ci-tools.yaml names
// (.github/actions/install-jj installs it; the worker image's ARG JJ_TOOL names the same release):
// a release of sjawhar/jj whose vendored gix-pack maps each pack as chunks kept resident up to a
// budget, JJ_PACK_MAPPED_LIMIT, where upstream gix-pack maps it whole, and under gVisor the first
// read of a pack mapped whole counts all of it as the process's memory (dispatch://INFRA-4036). A
// pin of a release built without that patch, an older one or a later one whose gix raise leaves
// its [patch.crates-io] entry unused, maps every pack whole again, and no other test sees it: on
// the small repositories tests build, the two read alike. The variable tells them apart: the
// patched mapping refuses a value that is not a budget at every pack open
// (lib/chunked-mmap/src/chunked.rs, open), and an unpatched jj never reads it. So on a git
// repository whose one commit sits in its one pack, the pinned jj (fixturegit.PinnedJJ, the jj on
// PATH with the pin's sha256, whatever jj stands in front of it) lists the commit's file under
// JJ_PACK_MAPPED_LIMIT=512m and fails under JJ_PACK_MAPPED_LIMIT=not-a-number, where jj
// 0.45.1-sami.20260910-043938 and v0.46.0-sami.20261008-234600 list the file both times. The
// crate's own refusal names the variable, but it never reaches the user: gix-odb takes the pack it
// could not open as absent, and jj reports the commit not found, so that is what the test asserts.
// The chunked mapping is Linux's alone: on any other target the crate maps a pack whole and reads
// no variable.
func TestThePinnedJJCarriesTheChunkedPackPatch(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skipf("the chunked pack mapping is Linux's alone: on %s the patched jj reads no JJ_PACK_MAPPED_LIMIT", runtime.GOOS)
	}
	jj := fixturegit.PinnedJJ(t)
	const file = "README.md"
	home := t.TempDir()
	repository := t.TempDir()
	fixture := fixturegit.Isolated(t, home)
	fixture.Run(repository, fixturegit.InitWorkTree(repository, "main")...)
	if err := os.WriteFile(filepath.Join(repository, file), []byte("smoke\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fixture.Run(repository, "git", "add", file)
	fixture.Run(repository, "git", "commit", "--quiet", "-m", "the one commit")
	fixture.Run(repository, "git", "repack", "-ad")
	head := fixture.Run(repository, "git", "rev-parse", "HEAD")
	// jj must read the commit from the pack: the patched mapping reads the variable as it opens a
	// pack, and a loose copy of the commit is read with no pack opened at all.
	packs, err := filepath.Glob(filepath.Join(repository, ".git", "objects", "pack", "*.pack"))
	if err != nil || len(packs) != 1 {
		t.Fatalf("git repack -ad left the packs %v (%v), want exactly one", packs, err)
	}
	loose := filepath.Join(repository, ".git", "objects", head[:2], head[2:])
	if _, err := os.Lstat(loose); err == nil {
		t.Fatalf("the commit %s is still a loose object (%s) after git repack -ad, want it in the pack alone", head, loose)
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
	// The init takes no -R and opens no workspace, so with no Dir jj would walk up from the test
	// process's directory to the closest .jj and write that repository.
	fixture.Run(repository, append([]string{jj}, fixturegit.JJInit(repository, true)[1:]...)...)

	// list lists the files of @-, HEAD's commit, under one JJ_PACK_MAPPED_LIMIT, which the command's
	// own environment carries: fixtureenv.MustGuard took every JJ_ variable out of the test process.
	list := func(limit string) (stdout, stderr string, err error) {
		t.Helper()
		var out, errOut strings.Builder
		command := exec.Command(jj, "--ignore-working-copy", "file", "list", "-r", "@-")
		command.Dir = repository
		command.Env = append(fixtureenv.Isolated(home), "JJ_PACK_MAPPED_LIMIT="+limit)
		command.Stdout, command.Stderr = &out, &errOut
		err = command.Run()
		return out.String(), errOut.String(), err
	}
	if stdout, stderr, err := list("512m"); err != nil || strings.TrimSpace(stdout) != file {
		t.Fatalf("under JJ_PACK_MAPPED_LIMIT=512m, jj --ignore-working-copy file list -r @- ended %v and printed %q, on stderr %q; want exit 0 and %s listed, as every jj lists it under a budget: an unpatched jj lists it under not-a-number too, and only that run tells the two apart", err, stdout, stderr, file)
	}
	stdout, stderr, err := list("not-a-number")
	if err == nil {
		t.Fatalf("under JJ_PACK_MAPPED_LIMIT=not-a-number, jj --ignore-working-copy file list -r @- exited 0 and printed %q: this jj ignores the variable, as a jj without the chunked-pack patch does (jj 0.45.1-sami.20260910-043938 and v0.46.0-sami.20261008-234600 list %s here too), where the patched gix-pack refuses the value as it opens the pack and jj reports the commit %s not found", stdout, file, head)
	}
	if !strings.Contains(stderr, head) || !strings.Contains(stderr, "not found") {
		t.Fatalf("under JJ_PACK_MAPPED_LIMIT=not-a-number, jj --ignore-working-copy file list -r @- ended %v and printed %q, on stderr %q; want the commit %s not found, which is how the patched gix-pack's refusal of the value reaches the user (gix-odb takes the pack as absent), where an unpatched jj lists %s", err, stdout, stderr, head, file)
	}
}

// The delivery dedupe end to end: the daemon retrying a prompt with the same deliveryId reaches
// OMP once, and every repeat is answered by the shim with the replay rules of internal/shimwire
// — the ack, a start carrying the deliveryId once the turn began, and its end once it is over.
func TestARepeatedDeliveryReachesOMPOnceAndIsAnsweredLocally(t *testing.T) {
	path := socketPath(t)
	daemon := listen(t, path)
	child := newOMP(t)
	run(t, config(t, path, child), newClock())
	p := daemon.accept(t)
	p.open(t)
	ack := func(id string) shimwire.Response {
		return shimwire.Response{ID: id, Command: shimwire.TypePrompt, Success: true}
	}

	p.send(t, shimwire.Prompt{ID: "r1", DeliveryID: "d1", Message: "hold"})
	p.expect(t, ack("r1"))
	p.expect(t, shimwire.AgentStart{})

	// Mid-turn: the ack and the start, which the daemon can fence on by its deliveryId.
	p.send(t, shimwire.Prompt{ID: "r2", DeliveryID: "d1", Message: "hold"})
	p.expect(t, ack("r2"))
	p.expect(t, shimwire.AgentStart{DeliveryID: "d1"})

	p.sendLine(t, `{"type":"fake_end_turn"}`)
	p.expect(t, shimwire.AgentEnd{})

	// After the turn: the end is replayed too, or the daemon would wait for one already sent.
	p.send(t, shimwire.Prompt{ID: "r3", DeliveryID: "d1", Message: "hold"})
	p.expect(t, ack("r3"))
	p.expect(t, shimwire.AgentStart{DeliveryID: "d1"})
	p.expect(t, shimwire.AgentEnd{})

	// A new delivery is OMP's again.
	p.send(t, shimwire.Prompt{ID: "r4", DeliveryID: "d2", Message: "the next task"})
	p.expect(t, ack("r4"))
	p.expect(t, shimwire.AgentStart{})
	p.expect(t, shimwire.AgentEnd{})

	var prompts []string
	for _, frame := range child.received(t) {
		if prompt, ok := frame.(shimwire.Prompt); ok {
			prompts = append(prompts, prompt.ID)
		}
	}
	if !reflect.DeepEqual(prompts, []string{"r1", "r4"}) {
		t.Fatalf("OMP was prompted with %v, want [r1 r4]: d1 once, d2 once", prompts)
	}
}

// OMP chunks any frame over the plain-frame limit, so a longer line on its stdout is a broken
// child: nothing honest can be forwarded from mid-line. The shim says so and ends the child
// rather than bridging a stream it can no longer frame.
func TestAnOverlongLineFromOMPEndsTheChild(t *testing.T) {
	path := socketPath(t)
	daemon := listen(t, path)
	child := newOMP(t, "FAKE_OMP_HUGE_LINE=1")
	clock := newClock()
	sh := run(t, config(t, path, child), clock)
	daemon.accept(t).open(t)

	clock.next(t, grace)
	if code := sh.wait(t); code != 128+int(syscall.SIGTERM) {
		t.Fatalf("the shim exited %d, want %d", code, 128+int(syscall.SIGTERM))
	}
	if sh.log.count("exceeds the plain-frame limit") != 1 {
		t.Fatalf("the shim did not say why it ended the child; log:\n%s", sh.log)
	}
}

// The pod's log is the shim's one line per frame it summarizes, and an extension of Oh My Pi
// failing is one of them: a Legion plugin whose session_start failed leaves an agent with no
// legion tool, and the pod's log says why.
func TestTheShimLogsAnExtensionError(t *testing.T) {
	path := socketPath(t)
	daemon := listen(t, path)
	child := newOMP(t, "FAKE_OMP_EXTENSION_ERROR=LEGION_DAEMON_URL is required for Legion")
	sh := run(t, config(t, path, child), newClock())
	daemon.accept(t).open(t)

	sh.log.awaitLine(t, "extension_error /opt/legion/pi-legion/dist/legion.js session_start: LEGION_DAEMON_URL is required for Legion", 1)
}

// A tool call's summary is its arguments cut to 80 runes, the last of them "…" where the cut took
// any (redact.Runes), and the URL rule runs on them before the cut: a cut inside a userinfo, before
// its "@", would leave a start of the user name that no rule after it reads as a userinfo. Wherever
// the cut of the arguments as written falls, inside the userinfo or inside the query, the summary
// holds no part of either: it is the cut of the arguments the rule left.
func TestAToolCallSummaryCutsTheArgumentsAfterTheURLRule(t *testing.T) {
	const url = "https://legion-reader@github.com/o/r.git?token=planted-query-token-4463"
	for _, tc := range []struct {
		name string
		cut  string // what the 80-rune cut of the arguments as written leaves of url
	}{
		{"the cut inside the userinfo", "https://legion"},
		{"the cut inside the query", "https://legion-reader@github.com/o/r.git?token=planted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The command before the URL is padded so that the arguments' first 80 runes end in tc.cut.
			head := `{"command":"` + strings.Repeat("x", 80-len(`{"command":" `)-len(tc.cut)) + " "
			path := socketPath(t)
			daemon := listen(t, path)
			child := newOMP(t, "FAKE_OMP_TOOL_ARGS="+head+url+`"}`)
			sh := run(t, config(t, path, child), newClock())
			daemon.accept(t).open(t)

			sh.log.await(t, shimwire.TypeToolExecutionStart+" ", 1)
			for line := range strings.Lines(sh.log.String()) {
				if strings.HasPrefix(line, shimwire.TypeToolExecutionStart+" ") && (strings.Contains(line, "legion") || strings.Contains(line, "planted")) {
					t.Fatalf("the summary %q holds a part of the URL's user name or of its query", line)
				}
			}
			left := []rune(head + `https://[a URL with credentials]/o/r.git"}`)
			summary := string(left)
			if len(left) > 80 {
				summary = string(left[:79]) + "…"
			}
			sh.log.awaitLine(t, shimwire.TypeToolExecutionStart+" bash "+summary, 1)
		})
	}
}

// jjRun runs one jj command in the workspace it reads, as every jj a test starts must
// (docs/testing.md).
func jjRun(t *testing.T, home, dir string, args ...string) string {
	t.Helper()
	return workspaceCommand(t, home, dir, append([]string{"jj"}, args...))
}

// jjGit runs one git command in the workspace it reads, as jjRun runs a jj.
func jjGit(t *testing.T, home, dir string, args ...string) string {
	t.Helper()
	return workspaceCommand(t, home, dir, append([]string{"git"}, args...))
}

// workspaceCommand runs one jj or git of a fixture's workspace under the isolated environment
// this package's fixtures use (fixtureenv.Isolated), so no command of a test reads the git or jj
// configuration, the credentials or the LEGION_ variables of whoever runs the tests — in a pod, of
// the role whose live workspace this binary stands in. Isolated's own JJ_CONFIG names a file that
// does not exist, which is what keeps the host's jj configuration out; the fixture writes none of
// its own.
//
// home is the fixture's, and the shim under test is given the same one, so the two read one
// configuration (docs/testing.md). It is outside the workspace: a fixture asserts on what the
// workspace holds, and a home under it would put jj's and git's own files into the very change
// under test.
func workspaceCommand(t *testing.T, home, dir string, args []string) string {
	t.Helper()
	return fixturegit.Isolated(t, home).With(
		"JJ_USER=tester", "JJ_EMAIL=tester@example.test",
		"GIT_AUTHOR_NAME=tester", "GIT_AUTHOR_EMAIL=tester@example.test",
		"GIT_COMMITTER_NAME=tester", "GIT_COMMITTER_EMAIL=tester@example.test",
	).Run(dir, args...)
}

// Package cmdtest builds a command's binary and runs it as a process a test drives, for the tests
// of more than one command. It is an ordinary package rather than a _test.go file because every
// command is package main, and none can import another's test helpers.
package cmdtest

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/logtest"
)

// Build builds the main package in the test's working directory, its own command's, into a binary
// named name, as BuildCommand builds a Command naming only that binary.
func Build(t testing.TB, name string) string {
	t.Helper()
	return BuildCommand(t, Command{Name: name})
}

// BuildStamped is Build with the binary stamped with commit as the image build stamps it:
// -ldflags "-X main.buildCommit=<commit>", the variable cmd/dispatch reports on /healthz and
// registers its process under. A test that needs processes serving different builds builds one
// binary per commit.
func BuildStamped(t testing.TB, name, commit string) string {
	t.Helper()
	return BuildCommand(t, Command{Name: name, Flags: []string{"-ldflags", "-X main.buildCommit=" + commit}})
}

// Command is a binary to build: Name is what the binary is called under the directory built for
// it, Dir the directory go build runs in (the test's own when empty), Target what it builds ("."
// when empty), Flags the flags before the target, and Env the whole environment of the go command
// (the process's when nil).
type Command struct {
	Name   string
	Dir    string
	Target string
	Flags  []string
	Env    []string
}

// overlayEnv names the overlay go build is given, so that a probe builds a command from changed
// sources without editing the tree. It is part of a build's key: two calls under different
// overlays are two builds.
const overlayEnv = "ENVOY_TEST_GO_OVERLAY"

// buildKey is everything a binary's build depends on, so that calls agreeing on all of it share
// one build and a call differing in any of it gets a build of its own: the directory go build
// runs in, the binary's name, the target it builds, and the overlay.
type buildKey struct {
	dir     string
	name    string
	target  string
	overlay string
	// args holds the caller's flags and environment joined by a byte neither holds, with the
	// count of the flags standing between them, so that a value that moves from one of the two
	// to the other is another key rather than the same one.
	args string
}

var (
	buildsMu sync.Mutex
	// builds holds each key's one build: a function that runs go build on its first call and
	// answers every later call with that build's path and error. go build relinks the whole
	// binary into every -o path it is given - about a second apiece on a warm build cache,
	// minutes on a loaded machine - and a row that starts five processes of one command asked
	// for five of them (INFRA-985), which this cache answers for the whole test process rather
	// than for one test. Functions rather than results, so that a caller arriving while a build
	// runs waits for that build instead of starting a second one, and a build that failed fails
	// every caller with the one error rather than handing a later one the path of a binary that
	// was never written (dispatch://INFRA-1075).
	builds = map[buildKey]func() (string, error){}
)

// BuildCommand builds c and returns the binary's path. The test process builds each distinct
// binary once, however many of its tests ask for it, and that binary stays on disk until the
// process ends, so a later test runs the build an earlier one paid for. ENVOY_TEST_GO_OVERLAY,
// when set, is passed to go build -overlay, so a probe can build the binary from changed sources
// without editing the tree.
func BuildCommand(t testing.TB, c Command) string {
	t.Helper()
	dir := c.Dir
	if dir == "" {
		var err error
		if dir, err = os.Getwd(); err != nil {
			t.Fatalf("build %s: read the test's working directory: %v", c.Name, err)
		}
	}
	target := c.Target
	if target == "" {
		target = "."
	}
	args := append([]string{strconv.Itoa(len(c.Flags))}, c.Flags...)
	key := buildKey{
		dir:     dir,
		name:    c.Name,
		target:  target,
		overlay: os.Getenv(overlayEnv),
		args:    strings.Join(append(args, c.Env...), "\x00"),
	}
	buildsMu.Lock()
	once, keyed := builds[key]
	if !keyed {
		// The caller owns its slices, and these are read long after the call returns.
		flags, env := slices.Clone(c.Flags), slices.Clone(c.Env)
		once = sync.OnceValues(func() (string, error) { return goBuild(key, flags, env) })
		builds[key] = once
	}
	buildsMu.Unlock()
	binary, err := once()
	if err != nil {
		t.Fatal(err)
	}
	return binary
}

// goBuild runs one go build for key and returns the binary's path. Each build gets a directory of
// its own under the test process's, so that two builds of one name, from different sources or
// under different flags, each keep that name: a process is known by the name it runs under.
func goBuild(key buildKey, flags, env []string) (string, error) {
	parent, err := buildsDir()
	if err != nil {
		return "", fmt.Errorf("build %s: %w", key.name, err)
	}
	dir, err := os.MkdirTemp(parent, "")
	if err != nil {
		return "", fmt.Errorf("build %s: %w", key.name, err)
	}
	binary := filepath.Join(dir, key.name)
	args := []string{"build", "-o", binary}
	if key.overlay != "" {
		args = append(args, "-overlay", key.overlay)
	}
	args = append(args, flags...)
	args = append(args, key.target)
	cmd := exec.Command("go", args...)
	// The package built is the one the key names, whatever directory the test process stands in
	// by the time a later caller's build runs.
	cmd.Dir, cmd.Env = key.dir, env
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("build %s: %v\n%s", key.name, err, out)
	}
	return binary, nil
}

// buildsDir is the one directory this test process builds its binaries in, made on the first
// build and removed when the process ends.
var buildsDir = sync.OnceValues(makeBuildsDir)

// reaperPipe is the write end of the pipe the build directory's reaper reads. This process holds
// it for the rest of its life, in a package-level variable because the garbage collector closes
// an *os.File nothing can reach any more, and that close would have the reaper remove the
// binaries while the tests still run them.
var reaperPipe *os.File

// makeBuildsDir makes the directory this process's binaries are built in and starts the process
// that removes it.
//
// A binary one test built is run by the tests after it, so no t.Cleanup can remove it: the test
// that paid for the build ends while later tests still run its binary. Go runs no hook of its own
// when a test binary exits either, and a TestMain in every package that builds a command would be
// a rule each new caller has to know. So the directory is removed by a child process that waits
// for this one to go: it reads a pipe this process holds the only write end of, and that read
// ends the moment the kernel closes this process's descriptors, whether it returned from its
// tests, panicked, or was killed.
func makeBuildsDir() (string, error) {
	dir, err := os.MkdirTemp("", "envoy-cmdtest-")
	if err != nil {
		return "", fmt.Errorf("make the test process's build directory: %w", err)
	}
	// The reaper is housekeeping, so a machine that cannot start one still builds: refusing
	// every Build of the process to avoid leaving a directory behind would cost more than the
	// directory, which the operating system reclaims with the rest of its temporary files.
	if err := removeWhenThisProcessExits(dir); err != nil {
		fmt.Fprintf(os.Stderr, "cmdtest: %v; %s is left for the operating system to reclaim\n", err, dir)
	}
	return dir, nil
}

// removeWhenThisProcessExits starts the process that removes dir once this one has exited.
func removeWhenThisProcessExits(dir string) error {
	read, write, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("open the build directory's reaper pipe: %w", err)
	}
	defer read.Close()
	// cat returns when the write end this process holds is closed, and rm then removes the
	// directory. The reaper takes a session of its own, so that the interrupt a terminal sends to
	// its whole foreground process group does not kill it before it has removed anything, and it
	// is given no output of its own: a child holding the test binary's standard output or error
	// would keep go test waiting for output after the tests were over.
	reaper := exec.Command("/bin/sh", "-c", `cat >/dev/null; exec rm -rf -- "$1"`, "cmdtest-reaper", dir)
	reaper.Stdin = read
	reaper.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := reaper.Start(); err != nil {
		_ = write.Close()
		return fmt.Errorf("start the build directory's reaper: %w", err)
	}
	reaperPipe = write
	return nil
}

// Process is a binary a test started, with its combined output.
type Process struct {
	Cmd    *exec.Cmd
	Output *LockedBuffer
	// Exited is closed once the process has exited and Cmd.ProcessState is set.
	Exited <-chan struct{}
	// name is what failures call the process.
	name string
}

// Start starts cmd, which failures call name, with its standard output and error captured in
// Output, and kills it when the test ends if it is still running. It closes every file in
// cmd.ExtraFiles once cmd.Start returns, whether the process started or not (CloseExtraFiles).
func Start(t testing.TB, name string, cmd *exec.Cmd) *Process {
	t.Helper()
	exited := make(chan struct{})
	process := &Process{Cmd: cmd, Output: &LockedBuffer{}, Exited: exited, name: name}
	cmd.Stdout, cmd.Stderr = process.Output, process.Output
	err := cmd.Start()
	CloseExtraFiles(cmd)
	if err != nil {
		t.Fatalf("start %s: %v", name, err)
	}
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-exited
	})
	return process
}

// CloseExtraFiles closes the test's copy of every file in cmd.ExtraFiles. Call it once cmd.Start
// returns, whether the process started or not: a started process holds its own copies from then
// on, so a socket a HeldPort passed it is released when it exits.
func CloseExtraFiles(cmd *exec.Cmd) {
	for _, file := range cmd.ExtraFiles {
		_ = file.Close()
	}
}

// Terminate sends the process SIGTERM, as a deploy stops it, and returns when it was sent.
func (p *Process) Terminate(t testing.TB) time.Time {
	t.Helper()
	signalled := time.Now()
	if err := p.Cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM %s: %v", p.name, err)
	}
	return signalled
}

// WaitExit waits up to 30 s for the process to exit, and fails the test with its output when it is
// still running; after names what it was waiting on.
func (p *Process) WaitExit(t testing.TB, after string) {
	t.Helper()
	select {
	case <-p.Exited:
	case <-time.After(30 * time.Second):
		t.Fatalf("%s was still running 30s after %s:\n%s", p.name, after, p.Output.String())
	}
}

// WaitForOutput waits up to 30 s for each of lines to appear in the process's output, and fails the
// test when the process exits first or a line never appears.
func (p *Process) WaitForOutput(t testing.TB, lines ...string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for _, line := range lines {
		for !strings.Contains(p.Output.String(), line) {
			select {
			case <-p.Exited:
				// Exited closes only once the output is whole, so this read is final.
				if strings.Contains(p.Output.String(), line) {
					continue
				}
				t.Fatalf("%s exited before it logged %q:\n%s", p.name, line, p.Output.String())
			default:
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s never logged %q:\n%s", p.name, line, p.Output.String())
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
}

// HeldPort is a loopback TCP port the kernel chose, held by a listening socket from the moment
// HoldPort returns until the process the test passes it to serves on it, so no other program can
// take the port in between.
type HeldPort struct {
	// Port is the port the kernel chose.
	Port int
	// Addr is the address the socket listens on, 127.0.0.1:Port.
	Addr string
	// file holds the listening socket until Start has handed it to the process.
	file *os.File
}

// HoldPort opens a listening socket on a loopback port the kernel chooses, for the test to hand to
// the process it starts with Pass. The test's copy closes once Start has started that process, or
// when the test ends.
func HoldPort(t testing.TB) *HeldPort {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("hold a loopback port: %v", err)
	}
	// The file is a descriptor of its own for the socket, so the socket stays listening once the
	// listener is closed.
	file, err := listener.(*net.TCPListener).File()
	_ = listener.Close()
	if err != nil {
		t.Fatalf("hold a loopback port: take the listening socket's descriptor: %v", err)
	}
	// A File closed twice closes its descriptor once, so this is safe after Start has closed it.
	t.Cleanup(func() { _ = file.Close() })
	addr := listener.Addr().(*net.TCPAddr)
	return &HeldPort{Port: addr.Port, Addr: addr.String(), file: file}
}

// Pass hands the held socket to cmd: it adds the socket to cmd.ExtraFiles and sets envName to the
// descriptor number the process inherits it as. It appends to cmd.Env, or to this process's
// environment when cmd.Env is nil, so call it after cmd.Env is assigned. Start then closes the
// test's copy.
func (h *HeldPort) Pass(cmd *exec.Cmd, envName string) {
	if cmd.Env == nil {
		cmd.Env = os.Environ()
	}
	// ExtraFiles entry i is descriptor 3+i in the process.
	cmd.Env = append(cmd.Env, envName+"="+strconv.Itoa(3+len(cmd.ExtraFiles)))
	cmd.ExtraFiles = append(cmd.ExtraFiles, h.file)
}

// LockedBuffer is a buffer a process's output, or a logger, writes from more than one goroutine
// while a test reads it. Its body is logtest.Buffer's.
type LockedBuffer = logtest.Buffer

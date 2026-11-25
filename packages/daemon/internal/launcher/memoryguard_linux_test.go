//go:build linux

package launcher

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unicode"

	"github.com/sjawhar/legion/daemon/internal/memoryguard"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"golang.org/x/sys/unix"
)

// memoryGuardRoleEnv is the marker of TestMemoryGuardOnRealRoleNamespaces' two helpers: the test
// binary re-entered as a role's role-init ("init") and as the process that init starts the way
// role-init starts its shim ("hold").
const memoryGuardRoleEnv = "LEGION_MEMORY_GUARD_TEST_ROLE"

// The helpers of this file are the test binary re-executed with its own LEGION_ marker, so
// TestMain runs no Guard in them (helperChildVariables, main_test.go). The file builds only under
// Linux, where a role's namespaces exist, so it registers its marker itself, as roleinit_test.go
// does. TestControllerPodMemoryGuardEndsEachKindOfCommand's helpers take the arguments of the
// processes they stand for, with which no test can be re-entered, so TestMain runs them itself
// (mainHelpers).
func init() {
	helperChildVariables = append(helperChildVariables, memoryGuardRoleEnv)
	mainHelpers[controllerGuardEnv] = controllerGuardHelper
}

// memoryGuardHeld is what the holder writes and keeps: well above the floor of
// TestMemoryGuardOnRealRoleNamespaces' fake limit, so the guard may end it.
const memoryGuardHeld = 64 << 20

// The guard reads and ends a process in a real role's namespaces. The test process stands in for
// the launcher: it starts a child in the namespaces a role's role-init is cloned into, and that
// child starts a holder of 64 MiB in a nested user namespace, as role-init starts the shim. The
// guard reads the holder's stat, exe, cmdline, ns/pid and smaps from the test's own /proc, takes
// its role from the launcher's classifier by its role-init ancestor, and ends it through the pidfd
// kill the guard runs on. The holder is the child's own, so the child waits on it and writes the
// wait status it read to the test, which reads SIGKILL there. The guard reads a fake meminfo and
// limit at its mark, and a /proc whose only processes are the test's, the child's and the
// holder's, each a link to the process's own directory in /proc, so the guard can end nothing of
// the machine's.
func TestMemoryGuardOnRealRoleNamespaces(t *testing.T) {
	t.Parallel()
	switch os.Getenv(memoryGuardRoleEnv) {
	case "init":
		memoryGuardRoleInit(t)
	case "hold":
		memoryGuardHold(t)
	}
	requireRoleNamespaces(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	readyRead, readyWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer readyRead.Close()
	statusRead, statusWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer statusRead.Close()
	attr, err := roleNamespaceAttr()
	if err != nil {
		t.Fatal(err)
	}
	initProcess := exec.Command(self, reentry(t, "TestMemoryGuardOnRealRoleNamespaces")...)
	initProcess.Env = append(os.Environ(), memoryGuardRoleEnv+"=init")
	initProcess.ExtraFiles = []*os.File{readyWrite, statusWrite}
	initProcess.SysProcAttr = attr
	output := &lockedBuffer{}
	initProcess.Stdout, initProcess.Stderr = output, output
	if err := initProcess.Start(); err != nil {
		t.Fatalf("start the role-init: %v", err)
	}
	roleInitPID := initProcess.Process.Pid
	_ = readyWrite.Close()
	_ = statusWrite.Close()
	// Killing the role-init ends its PID namespace, and the holder with it, whatever the test
	// reached.
	waited := false
	t.Cleanup(func() {
		if !waited {
			_ = initProcess.Process.Kill()
			_ = initProcess.Wait()
		}
	})
	if err := readyRead.SetReadDeadline(time.Now().Add(testwait.Remaining(t))); err != nil {
		t.Fatal(err)
	}
	if line, err := bufio.NewReader(readyRead).ReadString('\n'); err != nil || line != "ready\n" {
		t.Fatalf("the holder said %q (%v), want ready; the role's output: %s", line, err, output.String())
	}
	holder := childOf(t, roleInitPID)
	if holder == 0 {
		t.Fatalf("the role-init %d has no child after its holder said it was ready; the role's output: %s", roleInitPID, output.String())
	}

	proc, cgroup := t.TempDir(), t.TempDir()
	for _, pid := range []int{os.Getpid(), roleInitPID, holder} {
		if err := os.Symlink(filepath.Join("/proc", strconv.Itoa(pid)), filepath.Join(proc, strconv.Itoa(pid))); err != nil {
			t.Fatal(err)
		}
	}
	const limit = 512 << 20
	writeTestFile(t, filepath.Join(proc, "meminfo"), fmt.Sprintf("AnonPages:      %d kB\nShmem:              32 kB\n", (480<<20)>>10))
	writeTestFile(t, filepath.Join(cgroup, "memory.limit_in_bytes"), strconv.Itoa(limit)+"\n")
	writeTestFile(t, filepath.Join(cgroup, "memory.usage_in_bytes"), strconv.Itoa(limit/2)+"\n")

	classifier := newPodClassifier(t.TempDir(), []*manager{{role: "tester", child: &child{pid: roleInitPID}}}, &pidGuard{})
	var mu sync.Mutex
	var read *memoryguard.Process
	var classes map[int]memoryguard.Class
	kills := make(chan memoryguard.Kill, 1)
	lines := &lockedBuffer{}
	guard := memoryguard.New(memoryguard.Config{
		Proc: proc, Cgroup: cgroup, Clock: newGuardClock(),
		Classify: func(table *memoryguard.Table) map[int]memoryguard.Class {
			answer := classifier.classify(table)
			// The hour's scan for the largest process starts after the guard's decision at a
			// reading, so it reads the pod once the kill is sent and may read the holder as a
			// zombie; the first scan that reads the holder is the one the kill was decided on.
			if p, ok := table.Get(holder); ok {
				mu.Lock()
				defer mu.Unlock()
				if read == nil {
					read, classes = p, answer
				}
			}
			return answer
		},
		Sink: func(k memoryguard.Kill) { kills <- k },
		Logf: func(format string, args ...any) { fmt.Fprintf(lines, format+"\n", args...) },
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		guard.Run(ctx)
	}()
	defer func() {
		cancel()
		<-done
	}()

	var kill memoryguard.Kill
	select {
	case kill = <-kills:
	case <-time.After(testwait.Remaining(t)):
		t.Fatalf("the guard ended nothing; its log:\n%s", lines.String())
	}
	if kill.PID != holder || kill.Role != "tester" || kill.SizeBytes < memoryGuardHeld {
		t.Fatalf("the guard ended %+v, want the holder %d of the tester, at least %d bytes; its log:\n%s", kill, holder, memoryGuardHeld, lines.String())
	}
	mu.Lock()
	holderRead, holderClass, initClass := read, classes[holder], classes[roleInitPID]
	mu.Unlock()
	switch {
	case holderRead == nil:
		t.Fatal("the guard's scan did not read the holder")
	case holderRead.PPID != roleInitPID || holderRead.Exe != self || !slices.Contains(holderRead.Args, "-test.run=^TestMemoryGuardOnRealRoleNamespaces$") ||
		!strings.HasPrefix(holderRead.PIDNamespace, "pid:["):
		t.Fatalf("the guard's scan read the holder as %+v, want the role-init %d's child running %s in a PID namespace", holderRead, roleInitPID, self)
	case holderClass != (memoryguard.Class{Role: "tester"}):
		t.Fatalf("the classifier says %+v of the holder, want the tester's by its role-init ancestor, not protected", holderClass)
	case initClass != (memoryguard.Class{Role: "tester", Protected: true, Session: true}):
		t.Fatalf("the classifier says %+v of the role-init, want the tester's, protected, a process of its session", initClass)
	}

	if err := statusRead.SetReadDeadline(time.Now().Add(testwait.Remaining(t))); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(statusRead).ReadString('\n')
	if err != nil {
		t.Fatalf("the role-init's report of the holder's wait status: %v; the role's output: %s", err, output.String())
	}
	raw, err := strconv.ParseUint(strings.TrimSpace(line), 10, 32)
	if err != nil {
		t.Fatalf("the role-init reported %q, want a wait status; the role's output: %s", line, output.String())
	}
	if status := syscall.WaitStatus(raw); !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Fatalf("the holder's wait status is %#x, want SIGKILL", raw)
	}
	waited = true
	if err := initProcess.Wait(); err != nil {
		t.Fatalf("the role-init: %v; its output: %s", err, output.String())
	}
}

// memoryGuardRoleInit is the role-init of TestMemoryGuardOnRealRoleNamespaces: in a role's
// namespaces, it mounts the role's own /proc, as role-init does before it starts the shim
// (prepareRoleNamespace), since the nested user namespace's mappings are written through the
// /proc of the PID namespace the holder is started in; then it starts the holder in that nested
// user namespace as the image's unprivileged user, as runRoleShim starts the shim, hands it the
// test's ready pipe (fd 3), waits on it, and writes the wait status it read on the test's status
// pipe (fd 4). The holder is this process's own re-entered child, so its -test.timeout ends
// before this process's own deadline (reentry).
func memoryGuardRoleInit(t *testing.T) {
	ready, status := os.NewFile(3, "ready"), os.NewFile(4, "status")
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, "role-init:", err)
		os.Exit(1)
	}
	if err := privateMounts(); err != nil {
		fmt.Fprintln(os.Stderr, "role-init:", err)
		os.Exit(1)
	}
	if err := unix.Mount("proc", "/proc", "proc", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, ""); err != nil {
		fmt.Fprintln(os.Stderr, "role-init: mount the role's own /proc:", err)
		os.Exit(1)
	}
	hold := exec.Command(self, reentry(t, "TestMemoryGuardOnRealRoleNamespaces")...)
	hold.Env = append(os.Environ(), memoryGuardRoleEnv+"=hold")
	hold.ExtraFiles = []*os.File{ready}
	hold.Stdout, hold.Stderr = os.Stdout, os.Stderr
	hold.SysProcAttr = &syscall.SysProcAttr{
		Setpgid:                    true,
		Cloneflags:                 syscall.CLONE_NEWUSER,
		UidMappings:                []syscall.SysProcIDMap{{ContainerID: roleUID, HostID: 0, Size: 1}},
		GidMappings:                []syscall.SysProcIDMap{{ContainerID: roleUID, HostID: 0, Size: 1}},
		GidMappingsEnableSetgroups: false,
	}
	if err := hold.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "role-init: start the holder:", err)
		os.Exit(1)
	}
	_ = ready.Close()
	_ = hold.Wait()
	ws, _ := hold.ProcessState.Sys().(syscall.WaitStatus)
	fmt.Fprintf(status, "%d\n", uint32(ws))
	exitHelper(t)
}

// memoryGuardHold is the holder: it writes every page of memoryGuardHeld bytes, says so on the
// ready pipe, and keeps them until it is killed, or for two minutes, so a test that fails before
// the kill leaves nothing behind for long.
func memoryGuardHold(t *testing.T) {
	held := make([]byte, memoryGuardHeld)
	for i := 0; i < len(held); i += os.Getpagesize() {
		held[i] = 1
	}
	ready := os.NewFile(3, "ready")
	fmt.Fprintln(ready, "ready")
	_ = ready.Close()
	time.Sleep(2 * time.Minute)
	runtime.KeepAlive(held)
	exitHelper(t)
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The markers of TestControllerPodMemoryGuardEndsEachKindOfCommand's helpers. controllerGuardEnv
// names the part the test binary plays: the controller's shim ("shim"), its agent ("agent") or
// the agent's JavaScript eval kernel ("eval"). The others name the directory the agent writes its
// commands' pids in and reads the Python kernel's script from, the agent's executable, and the
// python3 the agent starts that kernel with.
const (
	controllerGuardEnv       = "LEGION_CONTROLLER_GUARD_TEST"
	controllerGuardDirEnv    = "LEGION_CONTROLLER_GUARD_TEST_DIR"
	controllerGuardAgentEnv  = "LEGION_CONTROLLER_GUARD_TEST_AGENT"
	controllerGuardPythonEnv = "LEGION_CONTROLLER_GUARD_TEST_PYTHON"
)

// controllerGuardSecret is the secret-looking argument every process of the test is started with,
// which no line and no report may carry any part of.
const controllerGuardSecret = "--password=hunter2-SECRET"

// What each process of the test holds, as anonymous memory: the agent the most, so the hour's
// largest process is the agent whatever the race detector adds to a Go process, and each process
// the guard may end more than the floor of the test's limit on its own.
const (
	controllerAgentHeld  = 64 << 20
	controllerEvalHeld   = 24 << 20
	controllerPythonHeld = 16 << 20
	controllerShellHeld  = 8 << 20
)

// controllerPythonKernel is the Python kernel's script: it holds a string of its own, says it is
// ready, and reads its stdin, the agent's pipe, until the agent is gone. controllerShellCommand is
// the shell command, which does the same.
const (
	controllerPythonKernel = "import sys\nheld = b\"x\" * %d\nprint(\"ready\")\nsys.stdin.read()\n"
	controllerShellCommand = "held=$(head -c %d /dev/zero | tr '\\0' x); echo ready; read line"
)

// The controller pod's guard ends each kind of command a controller's agent runs, on real
// processes. The test process stands in for PID 1 and runs the controller's launcher, Run's
// single-role form, against a fake daemon that starts its generation: the test binary run as
// `worker-shim`, the launcher's own child, which starts the agent, the test binary copied to a path
// of its own. The agent starts its JavaScript eval kernel, its own executable run as
// `__omp_worker_js_eval_process`; a `python3 -u` kernel in a session of its own, as Oh My Pi starts
// its kernel; and a shell command. Each holds real memory and is started with a secret-looking
// argument. The guard reads a fake meminfo and limit and a /proc of links to those five processes'
// own directories, so it can end nothing of the machine's; the launcher is left out of that
// /proc, since the test process's own size is no launcher's and the hour's largest process is read
// here.
//
// The guard ends the eval kernel, the Python kernel and the shell command, one at each rise, and
// never the shim or the agent. The fake daemon reads one launcher_memory_kill frame for each, with
// its pid, its executable's base name, its size, the reading and the limit, and neither the frame
// nor the guard's ended line holds any part of an argument. The line names the rule that chose the
// process (INFRA-3531 R3 of spec version 31): each rise goes from far below the band to the mark
// within one interval, so no reading of a stay comes before the kill, every process is new to it,
// and the largest of the new ends, on the sizes of the reading the kill is sent at. On the guard's
// injected clock the hour's line carries the hour's start and end, the reading's high and low and
// the time of the high, the counter's high and low and the largest process, the agent; the next
// hour's line carries that hour's readings alone.
func TestControllerPodMemoryGuardEndsEachKindOfCommand(t *testing.T) {
	// Not parallel: it writes the package variable podTrimmer (useGuardPodKilling), which parallel
	// tests read.
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skipf("no python3 to start the agent's Python kernel with: %v", err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	launcher := launcherExe(t)
	dir := t.TempDir()
	// The agent is the test binary copied to a path of its own, whose exe link names that path.
	agentExe := filepath.Join(dir, "omp")
	if err := copyExecutable(self, agentExe); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(dir, "kernel.py"), fmt.Sprintf(controllerPythonKernel, controllerPythonHeld))

	// The fake pod: a limit of 32 MiB, whose floor of 3.2 MiB each command passes on its own, and
	// whose mark of 27.2 MiB a reading of 30 MiB passes. A step between two readings is more than
	// 1% of the limit, the rise that starts a scan for the largest process, and in whole kB, as
	// meminfo writes a reading.
	const limit = 32 << 20
	mark := uint64(limit) * uint64(memoryguard.ControllerDefaults().ActPercent) / 100
	step := uint64(640 << 10)
	low, atKill := uint64(8<<20), uint64(30<<20)
	counterLow, counterHigh := uint64(10<<20), uint64(31<<20)
	pod := newGuardPod(t)
	pod.setLimit(limit)
	pod.setCounter(counterLow)
	pod.setAnon(low)
	rig := useGuardPodKilling(t, pod, nil, nil)
	g := runControllerLauncher(t)
	rig.started()
	conn := g.acceptController(controllerDefaultsAck())
	result := conn.start(shimwire.LauncherStart{
		ID: "start-1", Generation: 1, Argv: []string{self, shimArgument, controllerGuardSecret},
		Env: []string{
			controllerGuardEnv + "=shim", controllerGuardDirEnv + "=" + dir, controllerGuardAgentEnv + "=" + agentExe,
			controllerGuardPythonEnv + "=" + python, "LEGION_WORKSPACE=" + filepath.Join(t.TempDir(), "workspace"),
		},
	})
	if !result.OK || conn.latest.Child == nil {
		t.Fatalf("the start is answered %+v with the state %+v, want the shim running; the launcher's log:\n%s", result, conn.latest, g.stderr.String())
	}
	shim := conn.latest.Child.PID
	pids := controllerHelperPids(t, dir, g.stderr)
	agent := pids["agent"]
	if exe, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(agent), "exe")); err != nil || exe == launcher {
		t.Fatalf("the agent runs %q (%v), want an executable apart from the launcher's %s", exe, err, launcher)
	}
	for _, pid := range []int{shim, agent, pids["eval"], pids["python"], pids["shell"]} {
		if err := os.Symlink(filepath.Join("/proc", strconv.Itoa(pid)), filepath.Join(pod.proc, strconv.Itoa(pid))); err != nil {
			t.Fatal(err)
		}
	}

	// What the test knows of each command before the guard ends it: its executable's base name and
	// its arguments, as the kernel shows them.
	type command struct {
		what       string
		held       uint64
		executable string
		args       []string
	}
	commands := map[int]command{}
	for _, c := range []struct {
		name, what string
		held       uint64
	}{
		{"eval", "the JavaScript eval kernel", controllerEvalHeld},
		{"python", "the Python kernel", controllerPythonHeld},
		{"shell", "the shell command", controllerShellHeld},
	} {
		pid := pids[c.name]
		exe, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "exe"))
		if err != nil {
			t.Fatal(err)
		}
		cmdline, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
		if err != nil {
			t.Fatal(err)
		}
		args := strings.Split(strings.TrimSuffix(string(cmdline), "\x00"), "\x00")
		if !slices.Contains(args, controllerGuardSecret) {
			t.Fatalf("%s (pid %d) runs %q, want the secret among its arguments", c.what, pid, args)
		}
		commands[pid] = command{what: c.what, held: c.held, executable: filepath.Base(exe), args: args}
	}

	start := newGuardClock().Now()
	scanUntilCredited(rig, low, step, mark, agent)
	var highAt time.Time
	for sequence := uint64(1); sequence <= 3; sequence++ {
		pod.setCounter(counterHigh)
		pod.setAnon(atKill)
		rig.tick()
		if sequence == 1 {
			highAt = rig.clock.Now()
		}
		frame := conn.nextMemoryKill()
		c, ok := commands[frame.PID]
		if !ok {
			t.Fatalf("the guard ended %+v, want one of the commands %v it has not ended yet; its log:\n%s",
				frame, slices.Sorted(maps.Keys(commands)), rig.log())
		}
		delete(commands, frame.PID)
		if frame.Guard != rig.id() || frame.Sequence != sequence || frame.Executable != c.executable || frame.SizeBytes < c.held ||
			frame.ReadingBytes != atKill || frame.CounterBytes != counterHigh || frame.LimitBytes != limit || frame.At.IsZero() {
			t.Errorf("the report of %s is %+v, want guard %s, sequence %d, executable %q, at least %d bytes, the reading %d, the counter %d and the limit %d",
				c.what, frame, rig.id(), sequence, c.executable, c.held, atKill, counterHigh, limit)
		}
		ended := endedLine(g.stderr, frame.PID)
		if !strings.Contains(ended, " role=controller ") || !strings.HasSuffix(ended, ` rule=new sizes=reading less_parent=0 new_read=- executable="`+c.executable+`"`) {
			t.Errorf("the guard's ended line for %s (pid %d) is %q, want the controller's, chosen as the largest of the new by the reading's sizes, naming %q",
				c.what, frame.PID, ended, c.executable)
		}
		encoded, err := json.Marshal(frame)
		if err != nil {
			t.Fatal(err)
		}
		for _, text := range []string{string(encoded), ended} {
			for _, part := range argumentParts(c.args[1:]) {
				if strings.Contains(text, part) {
					t.Errorf("%q holds %q of the arguments %q of %s", text, part, c.args, c.what)
				}
			}
		}
		if !gone(t, frame.PID) {
			t.Fatalf("%s (pid %d) still runs after the guard ended it", c.what, frame.PID)
		}
		pod.setCounter(counterLow)
		pod.setAnon(low)
		rig.tick()
	}
	for _, pid := range []int{shim, agent} {
		if err := syscall.Kill(pid, 0); err != nil || zombie(pid) {
			t.Fatalf("the controller's own pid %d is gone (%v); the guard's log:\n%s", pid, err, rig.log())
		}
	}

	// The next hour, from its first reading, which the closing tick of the first one is.
	nextLow, nextFirst, nextLater := uint64(10<<20), uint64(14<<20), uint64(18<<20)
	pod.setCounter(nextFirst)
	pod.setAnon(12 << 20)
	rig.tickAfter(time.Hour)
	nextHigh, nextHighAt := scanUntilCredited(rig, 12<<20, step, mark, agent)
	pod.setCounter(nextLater)
	pod.setAnon(nextLow)
	rig.tickAfter(10 * time.Minute)
	rig.tickAfter(time.Hour)

	hours := guardLines(g.stderr, "hour ")
	if len(hours) != 2 {
		t.Fatalf("the hourly lines are %q, want two", hours)
	}
	requireHourLine(t, hours[0], start, atKill, highAt, low, counterHigh, counterLow, agent)
	requireHourLine(t, hours[1], start.Add(time.Hour), nextHigh, nextHighAt, nextLow, nextLater, nextFirst, agent)
}

// scanUntilCredited has the guard start scans for its largest process, each at a new high of the
// hour's reading under mark, until three scans that read pid have started, and then takes one more
// reading. The guard runs one scan at a time (startScan) and gives a scan's result to the hour's
// window at the first reading after it is done (takeScan), so once a third has started the second
// is done, and the reading after gives the window the second's result or a later one's: a scan of
// this hour that read pid, whatever hour the first scan counted was started in. It answers the
// reading it left the pod at and the time of the reading that first took it, the hour's high and
// its time where no reading of the hour was higher.
func scanUntilCredited(rig *guardRig, reading, step, mark uint64, pid int) (uint64, time.Time) {
	rig.t.Helper()
	target := rig.classifiedWith(pid) + 3
	var at time.Time
	for rig.classifiedWith(pid) < target {
		if reading+step >= mark {
			rig.t.Fatalf("no third scan that read pid %d started under the mark; the guard's log:\n%s", pid, rig.log())
		}
		reading += step
		before := rig.classifiedWith(pid)
		rig.pod.setAnon(reading)
		rig.tick()
		at = rig.clock.Now()
		// The scan runs on a goroutine of its own and asks the classifier when it starts.
		testwait.Holds(time.Second, testwait.Poll, func() bool { return rig.classifiedWith(pid) > before })
	}
	rig.tick()
	return reading, at
}

// requireHourLine fails unless line is the hour line of the hour from start with these readings
// and counters, whose largest process is the controller's agent, named by its executable's base
// name, at least as large as what it holds.
func requireHourLine(t *testing.T, line string, start time.Time, high uint64, highAt time.Time, low, counterHigh, counterLow uint64, agent int) {
	t.Helper()
	stamp := func(at time.Time) string { return at.UTC().Format(time.RFC3339) }
	head := fmt.Sprintf("launcher: memory guard: hour start=%s end=%s reading_high=%d reading_high_at=%s reading_low=%d counter_high=%d counter_low=%d largest_pid=%d largest_role=controller largest_size=",
		stamp(start), stamp(start.Add(time.Hour)), high, stamp(highAt), low, counterHigh, counterLow, agent)
	const tail = ` largest_executable="omp"`
	size, ok := strings.CutPrefix(line, head)
	if ok {
		size, ok = strings.CutSuffix(size, tail)
	}
	if n, err := strconv.ParseUint(size, 10, 64); !ok || err != nil || n < controllerAgentHeld {
		t.Fatalf("the hour line is\n%s\nwant\n%s<at least %d>%s", line, head, controllerAgentHeld, tail)
	}
}

// endedLine is the guard's ended line for pid in log, "" where it wrote none.
func endedLine(log *lockedBuffer, pid int) string {
	prefix := fmt.Sprintf("launcher: memory guard: ended pid=%d ", pid)
	for _, line := range strings.Split(log.String(), "\n") {
		if strings.HasPrefix(line, prefix) {
			return line
		}
	}
	return ""
}

// argumentParts are the parts of args that a line or a report could carry: each argument of more
// than three bytes whole, and each run of letters and digits in one of five bytes or more that is
// not a number, which no line or report holds by chance.
func argumentParts(args []string) []string {
	var parts []string
	for _, arg := range args {
		if len(arg) > 3 {
			parts = append(parts, arg)
		}
		for _, word := range strings.FieldsFunc(arg, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
			if len(word) >= 5 && strings.ContainsFunc(word, unicode.IsLetter) {
				parts = append(parts, word)
			}
		}
	}
	return parts
}

// controllerHelperPids waits for the pids the agent writes once each of its commands holds its
// memory, its own among them, and answers them by name.
func controllerHelperPids(t *testing.T, dir string, log *lockedBuffer) map[string]int {
	t.Helper()
	var body []byte
	if !testwait.Holds(beforeDeadline(t), testwait.Poll, func() bool {
		var err error
		body, err = os.ReadFile(filepath.Join(dir, "pids"))
		return err == nil
	}) {
		t.Fatalf("the agent never said its commands were ready; the launcher's log:\n%s", log.String())
	}
	pids := map[string]int{}
	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		name, value, _ := strings.Cut(line, " ")
		pid, err := strconv.Atoi(value)
		if err != nil {
			t.Fatalf("the agent wrote %q: %v", body, err)
		}
		pids[name] = pid
	}
	for _, name := range []string{"agent", "eval", "python", "shell"} {
		if pids[name] == 0 {
			t.Fatalf("the agent wrote %q, with no pid for its %s", body, name)
		}
	}
	return pids
}

// controllerGuardHelper is the part of TestControllerPodMemoryGuardEndsEachKindOfCommand that
// controllerGuardEnv names, which TestMain runs in the test binary's place.
func controllerGuardHelper() {
	part := os.Getenv(controllerGuardEnv)
	var err error
	switch part {
	case "shim":
		err = controllerGuardShim()
	case "agent":
		err = controllerGuardAgent()
	case "eval":
		err = controllerGuardEval()
	default:
		err = fmt.Errorf("no part %q", part)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "controller guard test %s: %v\n", part, err)
		os.Exit(1)
	}
}

// controllerGuardShim is the shim, the launcher's own child run as `worker-shim`: it starts the
// agent and lives as long as the agent does. The agent reads its stdin, the shim's pipe, until the
// shim is gone, as each of the agent's commands reads the agent's.
func controllerGuardShim() error {
	agent := exec.Command(os.Getenv(controllerGuardAgentEnv), "--mode", "rpc", controllerGuardSecret)
	agent.Env = append(os.Environ(), controllerGuardEnv+"=agent")
	agent.Stderr = os.Stderr
	if _, err := agent.StdinPipe(); err != nil {
		return err
	}
	if err := agent.Start(); err != nil {
		return err
	}
	_ = agent.Wait()
	return nil
}

// controllerGuardAgent is the agent, Oh My Pi at a path of its own: it holds the most memory of the
// test's processes and starts the three commands the guard may end, each with the secret among its
// arguments: its JavaScript eval kernel, its own executable run as `__omp_worker_js_eval_process`; a
// `python3 -u` kernel in a session of its own, as Oh My Pi starts its kernel; and a shell command.
// Once each has said it holds its memory, the agent writes its own pid and theirs for the test, and
// lives until the shim is gone. It reaps each command, so one the guard ends leaves no zombie.
func controllerGuardAgent() error {
	held, err := holdAnonymous(controllerAgentHeld)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Munmap(held) }()
	self, err := os.Executable()
	if err != nil {
		return err
	}
	dir := os.Getenv(controllerGuardDirEnv)
	eval := exec.Command(self, ompEvalKernel, controllerGuardSecret)
	eval.Env = append(os.Environ(), controllerGuardEnv+"=eval")
	python := exec.Command(os.Getenv(controllerGuardPythonEnv), "-u", filepath.Join(dir, "kernel.py"), controllerGuardSecret)
	python.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	shell := exec.Command("/bin/sh", "-c", fmt.Sprintf(controllerShellCommand, controllerShellHeld), controllerGuardSecret)
	pids := fmt.Sprintf("agent %d\n", os.Getpid())
	for _, c := range []struct {
		name string
		cmd  *exec.Cmd
	}{{"eval", eval}, {"python", python}, {"shell", shell}} {
		if _, err := c.cmd.StdinPipe(); err != nil {
			return err
		}
		out, err := c.cmd.StdoutPipe()
		if err != nil {
			return err
		}
		if err := c.cmd.Start(); err != nil {
			return fmt.Errorf("start the %s: %w", c.name, err)
		}
		if line, err := bufio.NewReader(out).ReadString('\n'); line != "ready\n" {
			return fmt.Errorf("the %s said %q (%v), want ready", c.name, line, err)
		}
		pids += fmt.Sprintf("%s %d\n", c.name, c.cmd.Process.Pid)
		go func() { _ = c.cmd.Wait() }()
	}
	if err := controllerLiveWrite(filepath.Join(dir, "pids"), pids); err != nil {
		return err
	}
	untilStdinEnds()
	return nil
}

// controllerGuardEval is the agent's JavaScript eval kernel, Oh My Pi's own executable run as
// `__omp_worker_js_eval_process`: it holds its memory, says so, and lives until the agent is gone.
func controllerGuardEval() error {
	held, err := holdAnonymous(controllerEvalHeld)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Munmap(held) }()
	fmt.Println("ready")
	untilStdinEnds()
	return nil
}

// holdAnonymous maps n bytes of anonymous memory outside the Go heap and writes every page of it,
// so the process's smaps counts them as Anonymous, and the race detector, which shadows the heap,
// adds nothing for them.
func holdAnonymous(n int) ([]byte, error) {
	held, err := unix.Mmap(-1, 0, n, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_ANON|unix.MAP_PRIVATE)
	if err != nil {
		return nil, err
	}
	for i := 0; i < len(held); i += os.Getpagesize() {
		held[i] = 1
	}
	return held, nil
}

// untilStdinEnds returns once stdin, the parent's pipe, reaches its end, which it does when the
// parent is gone, or after two minutes, so a test that fails leaves nothing behind for long.
func untilStdinEnds() {
	ended := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, os.Stdin)
		close(ended)
	}()
	select {
	case <-ended:
	case <-time.After(2 * time.Minute):
	}
}

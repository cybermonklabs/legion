//go:build linux

package launcher

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"golang.org/x/sys/unix"
)

// The live test's environment. TestControllerPodMemoryGuardLiveSandbox runs only where
// memoryGuardLiveEnv is 1, the switch of the issue pod's live test (internal/memoryguard's
// TestMemoryGuardLiveSandbox), which the measurement Jobs of INFRA-3531 set and CI does not, and
// controllerLiveScenarioEnv names its scenario. TestMain's guard would take both out of the test
// process, as it takes every LEGION_ variable, so this file has it keep them (guardKeep).
//
// controllerLiveEnv is the marker of the test's two helpers: the test binary run as the
// controller's shim ("shim") and as its agent ("agent"). Each takes the arguments of the process
// it stands for, with which no test can be re-entered, so TestMain runs them itself
// (mainHelpers). The others carry what the agent needs: the directory it writes its pids and its
// command's wait status in, its executable, the bytes it holds, and its command's argv as JSON.
const (
	memoryGuardLiveEnv        = "LEGION_MEMORY_GUARD_LIVE"
	controllerLiveScenarioEnv = "LEGION_CONTROLLER_LIVE_SCENARIO"
	controllerLiveEnv         = "LEGION_CONTROLLER_LIVE_TEST"
	controllerLiveDirEnv      = "LEGION_CONTROLLER_LIVE_DIR"
	controllerLiveAgentEnv    = "LEGION_CONTROLLER_LIVE_AGENT"
	controllerLiveHoldEnv     = "LEGION_CONTROLLER_LIVE_HOLD"
	controllerLiveRunEnv      = "LEGION_CONTROLLER_LIVE_RUN"
)

func init() {
	mainHelpers[controllerLiveEnv] = controllerLiveHelper
	guardKeep = append(guardKeep, memoryGuardLiveEnv, controllerLiveScenarioEnv)
}

const (
	controllerLiveProc   = "/proc"
	controllerLiveCgroup = "/sys/fs/cgroup/memory"
	// controllerLiveNoLimit is the smallest limit that is no limit at all, memoryguard's noLimit:
	// cgroup v1 writes an unlimited container's limit as the largest page-aligned int64.
	controllerLiveNoLimit = 1 << 62
	// controllerLiveSampleEvery is how often the test reads AnonPages and the cgroup's counter
	// itself, and controllerLivePrintEvery how many of those readings make one printed sample: one
	// line a second.
	controllerLiveSampleEvery = 100 * time.Millisecond
	controllerLivePrintEvery  = 10
	// controllerLiveStartBound bounds the agent's start: it writes a quarter of the limit, a few
	// GiB, before it starts its command.
	controllerLiveStartBound = 2 * time.Minute
	// controllerLiveKillBound bounds the wait for the guard's kill of the grower, which reaches
	// the mark from a quarter of the limit in about 20 seconds at 8 GiB.
	controllerLiveKillBound = 5 * time.Minute
	// controllerLiveExitBound bounds how long a process sent SIGKILL takes to exit.
	controllerLiveExitBound = time.Minute
	// controllerLiveFallBound bounds the wait for AnonPages to fall after a kill: the longest wait
	// the settings allow, so a fall slower than the guard's own wait is still measured.
	controllerLiveFallBound = shimwire.MemoryGuardWaitMax * time.Second
	// controllerLiveSettle is how long after its kill the daemon end keeps reading before it counts
	// the reports: twice the controller pod's guard's wait after a kill, in which it ends nothing
	// more where nothing it may end grows.
	controllerLiveSettle = 2 * shimwire.MemoryGuardControllerWaitDefault * time.Second
	// controllerLiveReportGrace is how long the test waits for a report once its command has
	// exited: the guard reads the cgroup's counter between its kill and its report.
	controllerLiveReportGrace = 5 * time.Second
	// controllerLiveMappedBound bounds the mapped file's reader, which writes and reads about
	// 10 GiB at 8 GiB before its 60 seconds.
	controllerLiveMappedBound = 30 * time.Minute
	// controllerLiveStopGrace is the launcher's stop grace, and controllerLiveStopBound bounds its
	// return once its context has ended.
	controllerLiveStopGrace = 5 * time.Second
	controllerLiveStopBound = 30 * time.Second
	// controllerLiveDeadlineMargin is what the test leaves of the test binary's deadline for its
	// own end, so a -test.timeout shorter than the run ends in a verdict line.
	controllerLiveDeadlineMargin = 15 * time.Second
	// controllerLiveLineBytes bounds one relayed line of the launcher's output, which a writer
	// that never ends its line cannot grow past.
	controllerLiveLineBytes = 64 << 10
	// controllerLiveStamp is RFC 3339 with milliseconds, the time every line carries.
	controllerLiveStamp = "2006-01-02T15:04:05.000Z07:00"
	// controllerLiveOffPrefix starts each line of the guard that says it has turned off, and
	// controllerLiveStartedPrefix the line that says it started.
	controllerLiveOffPrefix     = "launcher: memory guard: off"
	controllerLiveStartedPrefix = "launcher: memory guard: started "

	controllerLiveDeadlineReason = "the test binary's deadline came first: run it with a longer -test.timeout"
)

// The scenarios' commands, each run by python3 as the agent's child. controllerLiveGrowScript adds
// 64 MiB every quarter second until it is killed, INFRA-3557's grow-64. controllerLiveMappedScript
// writes a file of the bytes its second argument names at the path its first names, maps it
// shared and read-only, reads one byte of every page so each is resident, keeps it mapped for 60
// seconds and exits 0: its pages are the file's, which the cgroup's counter counts and AnonPages
// does not.
const (
	controllerLiveGrowScript   = "import time; b=[]; [(b.append(bytearray(64<<20)), time.sleep(0.25)) for _ in iter(int, 1)]"
	controllerLiveMappedScript = `import mmap, os, sys, time
path, n = sys.argv[1], int(sys.argv[2])
fd = os.open(path, os.O_RDWR | os.O_CREAT | os.O_TRUNC, 0o600)
chunk = memoryview(bytes(64 << 20))
written = 0
while written < n:
    written += os.write(fd, chunk[:min(len(chunk), n - written)])
print("mapped: wrote", written, "bytes to", path, flush=True)
m = mmap.mmap(fd, n, mmap.MAP_SHARED, mmap.PROT_READ)
total = 0
for offset in range(0, n, mmap.PAGESIZE):
    total += m[offset]
print("mapped: read", n // mmap.PAGESIZE, "pages, byte sum", total, "- keeping them mapped for 60 s", flush=True)
time.sleep(60)
m.close()
os.close(fd)
print("mapped: done", flush=True)
`
)

// TestControllerPodMemoryGuardLiveSandbox runs the controller pod's memory guard for real, in the
// launcher that is this container's first process, on the container's own /proc and memory
// cgroup, against one scenario (controllerLiveScenarioEnv). INFRA-3531's Jobs run it in gVisor
// pods of 4 GiB and 8 GiB, whose script builds the test binary with `go test -c` and execs it, so
// the binary is PID 1 and containerInit holds of it as it holds of a deployed launcher: nothing
// here fakes the guard's /proc, cgroup, clock, kill or classifier.
//
// The test runs launcher.Run's single-role form against a daemon end of its own on a TCP
// listener, which answers the hello with the guard's default settings, reads the launcher's
// state, and starts one generation: the test binary run as `worker-shim`, the shim, whose child
// is the agent, the test binary copied to a path of its own and run as Oh My Pi is. The agent
// holds a quarter of the limit in anonymous memory, every page written, and starts the
// scenario's command as its own child, as Oh My Pi starts a command it runs, and waits on it.
// The daemon end reads every launcher_memory_kill frame the launcher sends.
//
// `grow` is a python3 that adds 64 MiB every quarter second. It passes where the command ends by
// SIGKILL (the agent's wait reads the status), the shim and the agent live, and exactly one
// launcher_memory_kill reaches the daemon, naming the grower. `mapped` is a python3 that writes,
// maps and reads a file on the pod's disk (TMPDIR) of 1.5 times the act share of the limit and
// keeps it mapped for 60 seconds. It passes where the guard said it started and nothing ends: no
// report, and the reader exits 0 on its own.
//
// Every line goes straight to stdout, one write each, starts `live: ` and ends with its time
// (`at=`), so a pod the kernel ends keeps every line written before the end: the launcher's own
// lines (the guard's `launcher: memory guard: …` among them) and its generation's output,
// relayed; each frame the daemon end reads but the generation's states; AnonPages and the
// cgroup's counter once a second from the test's own readings, which it takes every 100 ms; and,
// for `grow`, the reading at the kill and the time AnonPages took to fall below it from the kill.
// The last is `live: verdict PASS <reason>` or `live: verdict FAIL <reason>`. Once
// LEGION_MEMORY_GUARD_LIVE=1 the test fails, and never skips, where it is not PID 1, finds no
// python3, or cannot read the cgroup's files or meminfo.
func TestControllerPodMemoryGuardLiveSandbox(t *testing.T) {
	if os.Getenv(memoryGuardLiveEnv) != "1" {
		t.Skip("the live test runs only where " + memoryGuardLiveEnv + "=1: it runs the controller pod's guard on this machine's own memory, as the container's first process")
	}
	out := &controllerLiveLog{}
	judged := false
	// A helper's t.Fatal ends the test before run returns; its verdict line is still written.
	defer func() {
		if !judged {
			out.printf("verdict FAIL the test stopped before its verdict: the test's own failure above says why")
		}
	}()
	r := &controllerLiveRun{t: t, out: out}
	pass, fail := r.run()
	judged = true
	if fail != "" {
		out.printf("verdict FAIL %s", fail)
		t.Fatal(fail)
	}
	out.printf("verdict PASS %s", pass)
}

// controllerLiveRun is one run of the live test, its steps in the order run takes them.
type controllerLiveRun struct {
	t   *testing.T
	out *controllerLiveLog
	ctx context.Context

	scenario string
	limit    uint64
	mark     uint64
	// hold is what the agent holds, mappedBytes the mapped file's size, and commandArgv the
	// command the agent runs, the scenario's python3.
	hold        int
	mappedBytes int
	commandArgv []string
	// dir holds the agent's executable, the files it writes for the test, and the mapped file.
	dir      string
	launcher string
	agentExe string

	frames  *controllerLiveFrames
	sampler *controllerLiveSampler

	mu sync.Mutex
	// guardOff is the guard's first line that says it turned off, "" while it has not, and
	// guardStarted whether the guard has said it started.
	guardOff     string
	guardStarted bool

	shim, agent, command int
}

// run takes the run's steps and answers why it passes, or why it fails. The launcher, and with it
// every process of the generation, is ended by the test's cleanup, after the verdict line.
func (r *controllerLiveRun) run() (pass, fail string) {
	ctx, cancel := controllerLiveDeadline(r.t)
	defer cancel()
	r.ctx = ctx
	if reason := r.prepare(); reason != "" {
		return "", reason
	}
	if reason := r.startGeneration(); reason != "" {
		return "", reason
	}
	r.sampler = startControllerLiveSampler(r.out, r.limit)
	defer r.finish()
	if reason := r.awaitAgent(); reason != "" {
		return "", reason
	}
	if r.scenario == "grow" {
		return r.awaitGrow()
	}
	return r.awaitMapped()
}

// controllerLiveDeadline is a context that ends controllerLiveDeadlineMargin before the test
// binary's own deadline.
func controllerLiveDeadline(t *testing.T) (context.Context, context.CancelFunc) {
	if deadline, ok := t.Deadline(); ok {
		return context.WithDeadline(context.Background(), deadline.Add(-controllerLiveDeadlineMargin))
	}
	return context.WithCancel(context.Background())
}

// prepare reads what the run needs before it starts anything, and prints the start line: the
// scenario, the guard's default settings, the limit, its mark, the cgroup's counter and AnonPages
// read once, what the agent holds, the command, the test's pid and the kernel's version (gVisor's
// own under gVisor).
func (r *controllerLiveRun) prepare() string {
	switch r.scenario = os.Getenv(controllerLiveScenarioEnv); r.scenario {
	case "grow", "mapped":
	default:
		return fmt.Sprintf("%s is %q, want grow or mapped", controllerLiveScenarioEnv, r.scenario)
	}
	if pid := os.Getpid(); pid != 1 {
		return fmt.Sprintf("the test binary is pid %d, not the container's first process: exec it as PID 1, so the launcher runs its guard as a deployed one does (containerInit)", pid)
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		return fmt.Sprintf("no python3 to run the scenario with: %v", err)
	}
	limit, err := controllerLiveCount("memory.limit_in_bytes")
	switch {
	case err != nil:
		return err.Error()
	case limit == 0 || limit >= controllerLiveNoLimit:
		return fmt.Sprintf("the container's limit reads %d: the live test needs a memory limit", limit)
	}
	counter, err := controllerLiveCount("memory.usage_in_bytes")
	if err != nil {
		return err.Error()
	}
	anon, err := controllerLiveAnonPages()
	if err != nil {
		return err.Error()
	}
	version, err := os.ReadFile(filepath.Join(controllerLiveProc, "version"))
	if err != nil {
		return err.Error()
	}
	page := uint64(os.Getpagesize())
	r.limit = limit
	r.mark = controllerLiveShare(limit, shimwire.MemoryGuardActDefault)
	r.hold = int(limit / 4 / page * page)
	r.mappedBytes = int(r.mark * 3 / 2 / page * page)
	r.dir = r.t.TempDir()
	if r.scenario == "grow" {
		r.commandArgv = []string{python, "-c", controllerLiveGrowScript}
	} else {
		r.commandArgv = []string{python, "-c", controllerLiveMappedScript, filepath.Join(r.dir, "mapped.bin"), strconv.Itoa(r.mappedBytes)}
	}
	r.out.printf("start scenario=%s settings=defaults act_percent=%d floor_percent=%d wait_seconds=%d interval_ms=%d limit=%d mark=%d counter=%d anon_pages=%d agent_holds=%d mapped_bytes=%d pid=%d version=%q command=%q",
		r.scenario, shimwire.MemoryGuardActDefault, shimwire.MemoryGuardFloorDefault, shimwire.MemoryGuardControllerWaitDefault,
		shimwire.MemoryGuardIntervalDefault, limit, r.mark, counter, anon, r.hold, r.mappedBytes, os.Getpid(),
		strings.TrimSpace(string(version)), r.commandArgv)
	return ""
}

// controllerLiveShare is percent of limit, as the guard reckons its mark (memoryguard's share).
func controllerLiveShare(limit uint64, percent int) uint64 {
	p := uint64(percent)
	return limit/100*p + limit%100*p/100
}

// startGeneration runs the controller's launcher, Run's single-role form, until the test's end,
// answers its hello with the guard's default settings, reads its state, and starts its one
// generation, the shim, whose pid the launcher's state names. From there the daemon end reads the
// connection on a goroutine of its own (controllerLiveFrames).
func (r *controllerLiveRun) startGeneration() string {
	self, err := os.Executable()
	if err != nil {
		return err.Error()
	}
	r.launcher = launcherExe(r.t)
	r.agentExe = filepath.Join(r.dir, "omp")
	if err := copyExecutable(self, r.agentExe); err != nil {
		return fmt.Sprintf("copy the test binary for the agent: %v", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err.Error()
	}
	r.t.Cleanup(func() { _ = listener.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	cfg := Config{
		Connect: "tcp://" + listener.Addr().String(), Token: "launcher-token", Sandbox: "legion-legion-controller",
		Role: "controller", PodUID: "pod-live", PrivateDir: shortTempDir(r.t),
		Stdout:    &controllerLiveLines{out: r.out, name: "stdout"},
		Stderr:    &controllerLiveLines{out: r.out, name: "stderr", onLine: r.noteLine},
		StopGrace: controllerLiveStopGrace,
	}
	go func() {
		result <- Run(ctx, cfg)
	}()
	r.t.Cleanup(func() {
		cancel()
		select {
		case err := <-result:
			if !errors.Is(err, context.Canceled) {
				r.t.Errorf("the controller's launcher returned %v, want its context's end", err)
			}
		case <-time.After(controllerLiveStopBound):
			r.t.Error("the controller's launcher did not return after its context ended")
		}
	})
	conn := acceptRole(r.t, listener)
	if conn.hello.Role != "controller" || conn.hello.Token != "launcher-token" {
		return fmt.Sprintf("the launcher's hello is %#v, want the controller's", conn.hello)
	}
	conn.acknowledgeWith(controllerDefaultsAck())
	run, err := json.Marshal(r.commandArgv)
	if err != nil {
		return err.Error()
	}
	started := conn.start(shimwire.LauncherStart{
		ID: "start-1", Generation: 1, Argv: []string{self, shimArgument},
		Env: []string{
			controllerLiveEnv + "=shim", controllerLiveDirEnv + "=" + r.dir, controllerLiveAgentEnv + "=" + r.agentExe,
			controllerLiveHoldEnv + "=" + strconv.Itoa(r.hold), controllerLiveRunEnv + "=" + string(run),
			"LEGION_WORKSPACE=" + filepath.Join(r.t.TempDir(), "workspace"),
		},
	})
	if !started.OK || conn.latest.Child == nil {
		return fmt.Sprintf("the start is answered %+v with the state %+v, want the shim running", started, conn.latest)
	}
	r.shim = conn.latest.Child.PID
	r.frames = &controllerLiveFrames{out: r.out, notify: make(chan struct{}, 1)}
	go r.frames.read(conn)
	return ""
}

// noteLine keeps the guard's first line that says it turned off, which ends every wait of the run,
// and whether the guard said it started.
func (r *controllerLiveRun) noteLine(line string) {
	started, off := strings.HasPrefix(line, controllerLiveStartedPrefix), strings.HasPrefix(line, controllerLiveOffPrefix)
	if !started && !off {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if started {
		r.guardStarted = true
	}
	if off && r.guardOff == "" {
		r.guardOff = line
	}
}

// awaitAgent waits for the pids the agent writes once it holds its memory and has started its
// command, and checks that the agent runs an executable apart from the launcher's, as Oh My Pi
// does.
func (r *controllerLiveRun) awaitAgent() string {
	var body []byte
	if reason := r.await(controllerLiveStartBound, "the agent's word that it holds its memory and runs its command", func() bool {
		var err error
		body, err = os.ReadFile(filepath.Join(r.dir, "pids"))
		return err == nil
	}); reason != "" {
		return reason
	}
	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		name, value, _ := strings.Cut(line, " ")
		pid, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Sprintf("the agent wrote %q: %v", body, err)
		}
		switch name {
		case "agent":
			r.agent = pid
		case "command":
			r.command = pid
		}
	}
	if r.agent == 0 || r.command == 0 {
		return fmt.Sprintf("the agent wrote %q, want its own pid and its command's", body)
	}
	exe, err := os.Readlink(filepath.Join(controllerLiveProc, strconv.Itoa(r.agent), "exe"))
	if err != nil || exe == r.launcher {
		return fmt.Sprintf("the agent runs %q (%v), want an executable apart from the launcher's %s", exe, err, r.launcher)
	}
	r.out.printf("helpers launcher_exe=%s shim_pid=%d agent_pid=%d agent_exe=%s agent_holds=%d command_pid=%d",
		r.launcher, r.shim, r.agent, exe, r.hold, r.command)
	return ""
}

// awaitGrow waits for the guard to end the grower, and passes where the grower ended by SIGKILL,
// exactly one report reached the daemon and named it, and the shim and the agent live. It prints
// the reading at the kill and the time AnonPages took to fall below it from the kill, by the
// test's own readings, whenever the guard reported a kill.
func (r *controllerLiveRun) awaitGrow() (pass, fail string) {
	if reason := r.await(controllerLiveKillBound, "the guard's kill of the grower", func() bool {
		_, exited := r.commandStatus()
		return len(r.frames.kills()) > 0 || exited
	}); reason != "" {
		return "", reason
	}
	if len(r.frames.kills()) == 0 {
		status, _ := r.commandStatus()
		if !controllerLiveSIGKILL(status) {
			return "", fmt.Sprintf("the grower pid=%d ended by itself (%s) and the guard ended nothing", r.command, controllerLiveStatus(status))
		}
		if reason := r.await(controllerLiveReportGrace, "a report of the grower's kill", func() bool {
			return len(r.frames.kills()) > 0
		}); reason != "" {
			return "", fmt.Sprintf("the grower pid=%d ended by SIGKILL, and %s", r.command, reason)
		}
	}
	k := r.frames.kills()[0]
	if reason := r.await(controllerLiveExitBound, "the exit of the process the guard ended", func() bool {
		_, exited := r.commandStatus()
		return exited || k.PID != r.command
	}); reason != "" {
		return "", reason
	}
	var fall controllerLiveSample
	fell := false
	if reason := r.await(time.Until(k.At.Add(controllerLiveFallBound)), "AnonPages' fall below the reading at the kill", func() bool {
		fall, fell = r.sampler.fallAfter(k.At, k.ReadingBytes)
		return fell
	}); reason == controllerLiveDeadlineReason {
		return "", reason
	}
	if reason := r.pause(time.Until(k.At.Add(controllerLiveSettle))); reason != "" {
		return "", reason
	}

	r.out.printf("reading at kill %d share_of_limit=%.2f%% mark=%d counter=%d limit=%d pid=%d executable=%q size=%d sequence=%d kill_at=%s",
		k.ReadingBytes, controllerLivePercent(k.ReadingBytes, k.LimitBytes), r.mark, k.CounterBytes, k.LimitBytes, k.PID,
		k.Executable, k.SizeBytes, k.Sequence, controllerLiveTime(k.At))
	if fell {
		r.out.printf("fall %s sample_at=%s sample_anon_pages=%d reading_at_kill=%d",
			fall.at.Sub(k.At), controllerLiveTime(fall.at), fall.anon, k.ReadingBytes)
	} else {
		r.out.printf("fall none: no reading of the test's within %s of the kill was below %d", controllerLiveFallBound, k.ReadingBytes)
	}
	kills := r.frames.kills()
	status, exited := r.commandStatus()
	switch {
	case k.PID != r.command:
		return "", fmt.Sprintf("the guard ended pid %d (executable=%q) and not the grower %d", k.PID, k.Executable, r.command)
	case !exited:
		return "", fmt.Sprintf("the grower pid=%d had not exited %s after the guard's kill", r.command, controllerLiveExitBound)
	case !controllerLiveSIGKILL(status):
		return "", fmt.Sprintf("the grower pid=%d ended with %s and not by SIGKILL", r.command, controllerLiveStatus(status))
	case len(kills) != 1:
		return "", fmt.Sprintf("%d launcher_memory_kill frames reached the daemon, want exactly one: %s", len(kills), controllerLiveKills(kills))
	case !strings.HasPrefix(k.Executable, "python3"):
		return "", fmt.Sprintf("the report names the executable %q, want python3's", k.Executable)
	case !controllerLiveAlive(r.shim):
		return "", fmt.Sprintf("the shim pid=%d is gone", r.shim)
	case !controllerLiveAlive(r.agent):
		return "", fmt.Sprintf("the agent pid=%d is gone", r.agent)
	}
	return fmt.Sprintf("the guard ended the grower pid=%d (executable=%q) by SIGKILL at a reading of %d (%.2f%% of the limit), one launcher_memory_kill reached the daemon, and the shim pid=%d and the agent pid=%d live",
		k.PID, k.Executable, k.ReadingBytes, controllerLivePercent(k.ReadingBytes, k.LimitBytes), r.shim, r.agent), ""
}

// awaitMapped waits for the mapped file's reader to exit, and passes where the guard said it
// started, the reader exited 0 on its own, no report reached the daemon, and the shim and the agent
// live. A report fails the run at once: what it ended held the file's pages, which are no
// anonymous memory.
func (r *controllerLiveRun) awaitMapped() (pass, fail string) {
	if reason := r.await(controllerLiveMappedBound, "the reader's exit", func() bool {
		_, exited := r.commandStatus()
		return len(r.frames.kills()) > 0 || exited
	}); reason != "" {
		return "", reason
	}
	if len(r.frames.kills()) == 0 {
		if reason := r.pause(controllerLiveReportGrace); reason != "" {
			return "", reason
		}
	}
	kills := r.frames.kills()
	status, _ := r.commandStatus()
	r.mu.Lock()
	started := r.guardStarted
	r.mu.Unlock()
	switch {
	case !started:
		return "", "the guard never started"
	case len(kills) > 0:
		return "", fmt.Sprintf("the guard ended %s while the reader held a mapped file, whose pages are no anonymous memory", controllerLiveKills(kills))
	case !status.Exited() || status.ExitStatus() != 0:
		return "", fmt.Sprintf("the reader pid=%d ended with %s and not exit status 0", r.command, controllerLiveStatus(status))
	case !controllerLiveAlive(r.shim):
		return "", fmt.Sprintf("the shim pid=%d is gone", r.shim)
	case !controllerLiveAlive(r.agent):
		return "", fmt.Sprintf("the agent pid=%d is gone", r.agent)
	}
	return fmt.Sprintf("the reader pid=%d wrote, mapped and read %d bytes (1.5 times the act share of the limit), kept them mapped for 60 s and exited 0, and the guard ended nothing",
		r.command, r.mappedBytes), ""
}

// await waits until done holds, asking it at each of the test's readings and at each frame the
// daemon end reads, and answers why it stopped where done never held: bound passed, the run can
// no longer go on (stopped), or the test's deadline came.
func (r *controllerLiveRun) await(bound time.Duration, what string, done func() bool) string {
	timer := time.NewTimer(bound)
	defer timer.Stop()
	tick := time.NewTicker(controllerLiveSampleEvery)
	defer tick.Stop()
	for {
		if done() {
			return ""
		}
		if reason := r.stopped(); reason != "" {
			return fmt.Sprintf("%s before %s", reason, what)
		}
		select {
		case <-r.frames.notify:
		case <-tick.C:
		case <-timer.C:
			if done() {
				return ""
			}
			return fmt.Sprintf("%s did not come within %s", what, bound)
		case <-r.ctx.Done():
			return controllerLiveDeadlineReason
		}
	}
}

// pause waits d, or answers that the test's deadline came first.
func (r *controllerLiveRun) pause(d time.Duration) string {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return ""
	case <-r.ctx.Done():
		return controllerLiveDeadlineReason
	}
}

// stopped is why the run cannot go on, "" while it can: the generation or the launcher's
// connection ended, the guard turned off, or one of the test's readings failed.
func (r *controllerLiveRun) stopped() string {
	if reason := r.frames.stopped(); reason != "" {
		return reason
	}
	r.mu.Lock()
	off := r.guardOff
	r.mu.Unlock()
	if off != "" {
		return fmt.Sprintf("the guard turned off (%s)", off)
	}
	if err := r.sampler.failed(); err != nil {
		return fmt.Sprintf("a reading of the test's failed: %v", err)
	}
	return ""
}

// commandStatus is the wait status the agent read of its command, once it has exited.
func (r *controllerLiveRun) commandStatus() (syscall.WaitStatus, bool) {
	body, err := os.ReadFile(filepath.Join(r.dir, "exit"))
	if err != nil {
		return 0, false
	}
	raw, err := strconv.ParseUint(strings.TrimSpace(string(body)), 10, 32)
	if err != nil {
		return 0, false
	}
	return syscall.WaitStatus(raw), true
}

// finish stops the test's readings and prints the end line: how many it took, and the highest
// AnonPages and counter among them.
func (r *controllerLiveRun) finish() {
	samples := r.sampler.end()
	var highest controllerLiveSample
	var counter uint64
	for _, s := range samples {
		if s.anon > highest.anon {
			highest = s
		}
		counter = max(counter, s.counter)
	}
	r.out.printf("end samples=%d highest_anon_pages=%d highest_anon_share=%.2f%% highest_anon_at=%s highest_counter=%d highest_counter_share=%.2f%% kills=%d",
		len(samples), highest.anon, controllerLivePercent(highest.anon, r.limit), controllerLiveTime(highest.at), counter,
		controllerLivePercent(counter, r.limit), len(r.frames.kills()))
}

// controllerLiveAlive is whether pid runs: it takes a signal and is no zombie.
func controllerLiveAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil && !zombie(pid)
}

// controllerLiveSIGKILL is whether status is a process's end by SIGKILL.
func controllerLiveSIGKILL(status syscall.WaitStatus) bool {
	return status.Signaled() && status.Signal() == syscall.SIGKILL
}

// controllerLiveStatus is status as os.ProcessState words it.
func controllerLiveStatus(status syscall.WaitStatus) string {
	switch {
	case status.Exited():
		return fmt.Sprintf("exit status %d", status.ExitStatus())
	case status.Signaled():
		return "signal: " + status.Signal().String()
	}
	return fmt.Sprintf("wait status %#x", uint32(status))
}

// controllerLiveKills is each report as a failure names it.
func controllerLiveKills(kills []shimwire.LauncherMemoryKill) string {
	parts := make([]string, 0, len(kills))
	for _, k := range kills {
		parts = append(parts, fmt.Sprintf("pid=%d executable=%q size=%d reading=%d", k.PID, k.Executable, k.SizeBytes, k.ReadingBytes))
	}
	return strings.Join(parts, "; ")
}

func controllerLivePercent(n, limit uint64) float64 { return 100 * float64(n) / float64(limit) }

func controllerLiveTime(t time.Time) string { return t.UTC().Format(controllerLiveStamp) }

// controllerLiveCount is the one number the cgroup's file name holds: its limit or its counter.
func controllerLiveCount(name string) (uint64, error) {
	path := filepath.Join(controllerLiveCgroup, name)
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseUint(string(bytes.TrimSpace(data)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s reads %q", path, bytes.TrimSpace(data))
	}
	return n, nil
}

// controllerLiveAnonPages is the pod's AnonPages from /proc/meminfo, in bytes, the reading the
// guard acts on.
func controllerLiveAnonPages() (uint64, error) {
	path := filepath.Join(controllerLiveProc, "meminfo")
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	for line := range strings.Lines(string(data)) {
		rest, ok := strings.CutPrefix(line, "AnonPages:")
		if !ok {
			continue
		}
		if fields := strings.Fields(rest); len(fields) == 2 && fields[1] == "kB" {
			if kb, err := strconv.ParseUint(fields[0], 10, 64); err == nil {
				return kb << 10, nil
			}
		}
		return 0, fmt.Errorf("%s holds the line %q, want AnonPages in kB", path, strings.TrimSpace(line))
	}
	return 0, errors.New("no AnonPages line in " + path)
}

// controllerLiveLog writes the live test's lines to stdout, each whole line in one write and none
// buffered, so a pod the kernel ends keeps every line written before the end. A line starts
// `live: ` and ends with its time.
type controllerLiveLog struct{ mu sync.Mutex }

// printf writes one line, stamped with the time it is written.
func (l *controllerLiveLog) printf(format string, args ...any) {
	l.write(fmt.Sprintf(format, args...) + " at=" + controllerLiveTime(time.Now()))
}

// write writes text, which carries its own time, as one line.
func (l *controllerLiveLog) write(text string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = os.Stdout.WriteString("live: " + text + "\n")
}

// controllerLiveLines is one of the launcher's writers, Config.Stdout or Config.Stderr: each line
// written to it is printed as the test's own, behind name, and handed to onLine where that is
// set. The launcher writes its own lines to Stderr, the guard's among them, and its generation
// writes to both, so the Job's log carries every one in the order it was written.
type controllerLiveLines struct {
	out    *controllerLiveLog
	name   string
	onLine func(string)

	mu      sync.Mutex
	partial []byte
}

func (w *controllerLiveLines) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.partial = append(w.partial, p...)
	for {
		line, rest, found := bytes.Cut(w.partial, []byte("\n"))
		if !found {
			break
		}
		w.emit(string(line))
		w.partial = rest
	}
	if len(w.partial) >= controllerLiveLineBytes {
		w.emit(string(w.partial))
		w.partial = w.partial[:0]
	}
	return len(p), nil
}

func (w *controllerLiveLines) emit(line string) {
	w.out.printf("%s: %s", w.name, line)
	if w.onLine != nil {
		w.onLine(line)
	}
}

// controllerLiveFrames is the daemon end of the launcher's connection once its generation has
// started: it reads every frame the launcher sends until the connection ends, prints each but
// the generation's running states, keeps each launcher_memory_kill, and notes the generation's
// end, a state that names no child. notify wakes the test's wait at each frame.
type controllerLiveFrames struct {
	out    *controllerLiveLog
	notify chan struct{}

	mu      sync.Mutex
	reports []shimwire.LauncherMemoryKill
	ended   bool
	exit    *shimwire.LauncherExit
	err     error
}

// read reads conn, whose earlier frames the test read itself, with no deadline from here.
func (f *controllerLiveFrames) read(conn *roleConn) {
	err := conn.conn.SetReadDeadline(time.Time{})
	for err == nil {
		var line []byte
		if line, err = conn.reader.ReadBytes('\n'); err != nil {
			break
		}
		var frame shimwire.Frame
		if frame, err = shimwire.Decode(line); err != nil {
			break
		}
		text := string(bytes.TrimSpace(line))
		f.mu.Lock()
		switch frame := frame.(type) {
		case shimwire.LauncherMemoryKill:
			f.reports = append(f.reports, frame)
			f.out.printf("daemon: read %s", text)
		case shimwire.LauncherState:
			if frame.Child == nil {
				f.ended, f.exit = true, frame.LastExit
				f.out.printf("daemon: read %s", text)
			}
		default:
			f.out.printf("daemon: read %s", text)
		}
		f.mu.Unlock()
		f.poke()
	}
	f.mu.Lock()
	f.err = err
	f.mu.Unlock()
	f.poke()
}

func (f *controllerLiveFrames) poke() {
	select {
	case f.notify <- struct{}{}:
	default:
	}
}

// kills are the reports read so far, in the order the launcher sent them.
func (f *controllerLiveFrames) kills() []shimwire.LauncherMemoryKill {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]shimwire.LauncherMemoryKill(nil), f.reports...)
}

// stopped is why the daemon end can no longer stand for the run, "" while it can: the generation
// ended, or the connection did.
func (f *controllerLiveFrames) stopped() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case f.ended && f.exit != nil:
		return fmt.Sprintf("the shim's generation ended (%+v)", *f.exit)
	case f.ended:
		return "the shim's generation ended"
	case f.err != nil:
		return fmt.Sprintf("the launcher's connection ended (%v)", f.err)
	}
	return ""
}

// controllerLiveSample is one of the test's own readings: AnonPages and the cgroup's counter.
type controllerLiveSample struct {
	at            time.Time
	anon, counter uint64
}

// controllerLiveSampler takes one reading every controllerLiveSampleEvery until end, keeps each,
// and prints one sample line every controllerLivePrintEvery readings: once a second.
type controllerLiveSampler struct {
	out   *controllerLiveLog
	limit uint64
	stop  chan struct{}
	done  chan struct{}

	mu      sync.Mutex
	samples []controllerLiveSample
	err     error
}

func startControllerLiveSampler(out *controllerLiveLog, limit uint64) *controllerLiveSampler {
	s := &controllerLiveSampler{out: out, limit: limit, stop: make(chan struct{}), done: make(chan struct{})}
	go s.run()
	return s
}

func (s *controllerLiveSampler) run() {
	defer close(s.done)
	tick := time.NewTicker(controllerLiveSampleEvery)
	defer tick.Stop()
	for n := 0; ; n++ {
		sample, err := takeControllerLiveSample()
		s.mu.Lock()
		if err != nil {
			s.err = err
		} else {
			s.samples = append(s.samples, sample)
		}
		s.mu.Unlock()
		switch {
		case err != nil:
			s.out.printf("sample failed reason=%v", err)
			return
		case n%controllerLivePrintEvery == 0:
			s.out.write(fmt.Sprintf("sample anon_pages=%d anon_share=%.2f%% counter=%d counter_share=%.2f%% at=%s",
				sample.anon, controllerLivePercent(sample.anon, s.limit), sample.counter,
				controllerLivePercent(sample.counter, s.limit), controllerLiveTime(sample.at)))
		}
		select {
		case <-s.stop:
			return
		case <-tick.C:
		}
	}
}

func takeControllerLiveSample() (controllerLiveSample, error) {
	at := time.Now()
	anon, err := controllerLiveAnonPages()
	if err != nil {
		return controllerLiveSample{}, err
	}
	counter, err := controllerLiveCount("memory.usage_in_bytes")
	if err != nil {
		return controllerLiveSample{}, err
	}
	return controllerLiveSample{at: at, anon: anon, counter: counter}, nil
}

// end stops the readings and answers every one taken. It is called once.
func (s *controllerLiveSampler) end() []controllerLiveSample {
	close(s.stop)
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.samples
}

func (s *controllerLiveSampler) failed() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// fallAfter is the first reading taken after at whose AnonPages is below reading.
func (s *controllerLiveSampler) fallAfter(at time.Time, reading uint64) (controllerLiveSample, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sample := range s.samples {
		if sample.at.After(at) && sample.anon < reading {
			return sample, true
		}
	}
	return controllerLiveSample{}, false
}

// controllerLiveHelper is the part of TestControllerPodMemoryGuardLiveSandbox that
// controllerLiveEnv names, which TestMain runs in the test binary's place.
func controllerLiveHelper() {
	part := os.Getenv(controllerLiveEnv)
	var err error
	switch part {
	case "shim":
		err = controllerLiveShim()
	case "agent":
		err = controllerLiveAgent()
	default:
		err = fmt.Errorf("no part %q", part)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "controller live test %s: %v\n", part, err)
		os.Exit(1)
	}
}

// controllerLiveShim is the shim, the launcher's own child run as `worker-shim`: it starts the
// agent, Oh My Pi at a path of its own run as `--mode rpc`, and lives as long as the agent does.
// The agent reads its stdin, the shim's pipe, until the shim is gone.
func controllerLiveShim() error {
	agent := exec.Command(os.Getenv(controllerLiveAgentEnv), "--mode", "rpc")
	agent.Env = append(os.Environ(), controllerLiveEnv+"=agent")
	agent.Stdout, agent.Stderr = os.Stdout, os.Stderr
	if _, err := agent.StdinPipe(); err != nil {
		return err
	}
	if err := agent.Start(); err != nil {
		return err
	}
	_ = agent.Wait()
	return nil
}

// controllerLiveAgent is the agent: it holds the bytes controllerLiveHoldEnv names in anonymous
// memory, every page written, starts the command controllerLiveRunEnv names as its own child,
// writes its own pid and the command's for the test, and lives until the shim is gone. It waits
// on its command and writes the wait status it read for the test, as Oh My Pi reads the end of a
// command it ran.
func controllerLiveAgent() error {
	hold, err := strconv.Atoi(os.Getenv(controllerLiveHoldEnv))
	if err != nil {
		return fmt.Errorf("%s: %w", controllerLiveHoldEnv, err)
	}
	var argv []string
	if err := json.Unmarshal([]byte(os.Getenv(controllerLiveRunEnv)), &argv); err != nil || len(argv) == 0 {
		return fmt.Errorf("%s holds no command (%v)", controllerLiveRunEnv, err)
	}
	held, err := holdAnonymous(hold)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Munmap(held) }()
	dir := os.Getenv(controllerLiveDirEnv)
	command := exec.Command(argv[0], argv[1:]...)
	command.Stdout, command.Stderr = os.Stdout, os.Stderr
	command.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	if err := command.Start(); err != nil {
		return fmt.Errorf("start the command: %w", err)
	}
	pid := command.Process.Pid
	fmt.Fprintf(os.Stderr, "controller live agent: pid=%d holds %d bytes and runs the command pid=%d\n", os.Getpid(), hold, pid)
	if err := controllerLiveWrite(filepath.Join(dir, "pids"), fmt.Sprintf("agent %d\ncommand %d\n", os.Getpid(), pid)); err != nil {
		return err
	}
	go func() {
		_ = command.Wait()
		status, _ := command.ProcessState.Sys().(syscall.WaitStatus)
		fmt.Fprintf(os.Stderr, "controller live agent: the command pid=%d ended: %s\n", pid, command.ProcessState)
		if err := controllerLiveWrite(filepath.Join(dir, "exit"), fmt.Sprintf("%d\n", uint32(status))); err != nil {
			fmt.Fprintf(os.Stderr, "controller live agent: %v\n", err)
		}
	}()
	_, _ = io.Copy(io.Discard, os.Stdin)
	return nil
}

// controllerLiveWrite writes content to path whole: the test reads the file only once it is there.
func controllerLiveWrite(path, content string) error {
	if err := os.WriteFile(path+".tmp", []byte(content), 0o644); err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}

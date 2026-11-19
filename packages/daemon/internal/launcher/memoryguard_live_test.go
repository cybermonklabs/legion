//go:build linux

package launcher

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
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

	"github.com/sjawhar/legion/daemon/internal/memoryguard"
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
	// controllerLivePoolBound bounds the wait for the exit of every grower of the pool and of each
	// of the tester's other probes of six growers, and of the probe itself (awaitGrowers): their
	// growers reach the mark in a few seconds, and the guard ends one at a reading, or at a reading
	// at or above the urgent share every one that grew over its window and, where the decision holds
	// a process that is not new to the stay, every one new to it with memory of its own (R1 of spec
	// versions 39, 41 and 42), or after its wait of 1 second, until none is left.
	controllerLivePoolBound = 5 * time.Minute
	// controllerLivePoolGrowers is how many growers the pool starts at once: six of `grow`'s rise
	// about 1.5 GiB a second together, the fastest rise INFRA-3531's tester measured.
	controllerLivePoolGrowers = 6
	// controllerLiveGrowerStep is the rise in the guard's reading that the traced scenarios' verdict
	// reads as a grower's growth (judgeTrace): a grower adds 64 MiB a step, where the protected
	// processes and the Go runtimes of the launcher, the shim and the agent move a few MiB, so a
	// reading 16 MiB or more above the reading at a kill means a grower grew since that kill, and a
	// smaller rise may be no one's growth at all.
	controllerLiveGrowerStep = 16 << 20
	// controllerLiveBandPercent is memoryguard's bandPercent: an armed guard reads the sizes of
	// what it may end at each reading from this share of the limit below the mark up to the mark.
	controllerLiveBandPercent = 10
	// controllerLiveBandBelowMark is the share of the limit below the mark at which the band
	// scenario holds the reading: the middle of the band.
	controllerLiveBandBelowMark = 5
	// controllerLiveBandWindow is how long the band scenario measures the guard: a minute, as
	// TestMemoryGuardLiveSandbox measures its own CPU time by default.
	controllerLiveBandWindow = time.Minute
	// controllerLiveBandHold is how long the band's holder holds once it has said it holds: the
	// window, and ten seconds for the test to start the window after the holder's word and to end
	// it before the holder lets go.
	controllerLiveBandHold = controllerLiveBandWindow + 10*time.Second
	// controllerLiveExitSlice is how long one poll of a killed process's pidfd waits while a traced
	// scenario measures the process's exit (controllerLiveWatch.timeExit). The poll is taken again
	// until the process has exited, so its exit is seen within one slice whether or not the sandbox
	// wakes a poll that waits on a pidfd, which the guard itself never does: it asks without
	// waiting (Handle.Exited).
	controllerLiveExitSlice = 5 * time.Millisecond
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
	// controllerLiveOffPrefix starts each line of the guard that says it has turned off,
	// controllerLiveStartedPrefix the line that says it started, and controllerLiveEndedPrefix each
	// line that says it ended a process (memoryguard's endedLine).
	controllerLiveOffPrefix     = "launcher: memory guard: off"
	controllerLiveStartedPrefix = "launcher: memory guard: started "
	controllerLiveEndedPrefix   = "launcher: memory guard: ended "
	// controllerLiveFirstKillIntervals is how many of the guard's intervals c-gvisor-real (its
	// verification version 20) allows in each run from the guard's first reading at or above the
	// mark to its first kill (markToKill): two. R1's one interval it counts across a wave, in 6 of
	// the 7 runs of each pool at 4 GiB, from each run's `live: first kill` line (printFirstKill), so
	// a run past one interval and within two is printed and not failed.
	controllerLiveFirstKillIntervals = 2

	controllerLiveDeadlineReason = "the test binary's deadline came first: run it with a longer -test.timeout"
)

// The scenarios' commands, each run by python3 as the agent's child. controllerLiveGrowScript adds
// 64 MiB every quarter second until it is killed, INFRA-3557's grow-64. controllerLiveMappedScript
// writes a file of the bytes its second argument names at the path its first names, maps it
// shared and read-only, reads one byte of every page so each is resident, keeps it mapped for 60
// seconds and exits 0: its pages are the file's, which the cgroup's counter counts and AnonPages
// does not.
//
// controllerLiveSharedScript is the shared-page probe of INFRA-3531's tester (2026-11-17): it
// writes every page of a block of the bytes its first argument names and forks three children
// that only sleep, which share the block's pages and under gVisor each read them as their own,
// then starts the grower its third argument holds (controllerLiveGrowScript), writes its own pid,
// the sleepers' and the grower's to the path its second argument names, and waits on the grower;
// once the grower has ended it ends the sleepers and exits 0. controllerLivePoolScript is the
// tester's pool probe: it starts as many growers as its first argument names at once, writes its
// own pid and theirs to the path its second names, and exits 0 once every grower has ended.
// controllerLiveBandScript allocates, 64 MiB at a time with every page written, until the pod's
// AnonPages is within 1 MiB of the bytes its first argument names, writes the AnonPages it then
// reads to the path its second names, holds it for the seconds its third names with nothing
// growing, and exits 0. Each writes its file whole, so the test reads it only once it is there.
//
// controllerLiveSharedPoolScript and controllerLiveLatePoolScript are the tester's probes of a
// fast rise over a high baseline, of the same day, their text as the tester wrote it
// (dispatch://INFRA-3531/artifact/tester-evidence-a97e6a44-2026-11-17-tar-gz), so a run measures
// what the tester measured. Each starts six growers at once, each the grow one-liner it holds
// itself (controllerLiveGrowScript's). controllerLiveSharedPoolScript runs protpool and
// sharedpool: where its second argument names a block of bytes, it writes every page of one and
// forks three children that only sleep, as controllerLiveSharedScript does, and protpool names 0,
// so it forks none. It then starts its growers, writes its own pid, the sleepers' and the
// growers' to the path its first argument names, waits on every grower, ends the sleepers and
// exits 0. controllerLiveLatePoolScript runs latepool. It takes the paths of its pids, of its word
// that it holds and of the test's word to go, then four numbers: a block's bytes, which it writes
// and shares with three sleeping forks as above, how many idle children of how many bytes each it
// starts, and an AnonPages it fills the pod to; latepool names the block alone and 0 for the
// other three, as the tester did. Once it holds, it writes the AnonPages it reads to the path of
// its word, waits for the path to go, starts its growers, writes its pids by kind (parent,
// sleeper, idle, grower), waits on every grower, ends the sleepers and the idle children and
// exits 0.
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
	controllerLiveSharedScript = `import os, signal, subprocess, sys, time
n, pids, grow = int(sys.argv[1]), sys.argv[2], sys.argv[3]
b = bytearray(n)
for i in range(0, n, 4096):
    b[i] = 1
kids = []
for _ in range(3):
    pid = os.fork()
    if pid == 0:
        time.sleep(3600)
        os._exit(0)
    kids.append(pid)
print("shared: parent", os.getpid(), "sleepers", kids, "shared_bytes", n, flush=True)
g = subprocess.Popen([sys.executable, "-c", grow])
print("shared: grower", g.pid, flush=True)
with open(pids + ".tmp", "w") as f:
    f.write("parent %d\nsleeper %s\ngrower %d\n" % (os.getpid(), " ".join(map(str, kids)), g.pid))
os.replace(pids + ".tmp", pids)
g.wait()
print("shared: grower returncode", g.returncode, flush=True)
for k in kids:
    try:
        os.kill(k, signal.SIGKILL)
    except ProcessLookupError:
        print("shared: sleeper", k, "already gone", flush=True)
print("shared: done", flush=True)
`
	controllerLivePoolScript = `import os, subprocess, sys
n, pids, grow = int(sys.argv[1]), sys.argv[2], sys.argv[3]
kids = [subprocess.Popen([sys.executable, "-c", grow]) for _ in range(n)]
print("pool: parent", os.getpid(), "growers", " ".join(str(k.pid) for k in kids), flush=True)
with open(pids + ".tmp", "w") as f:
    f.write("parent %d\ngrower %s\n" % (os.getpid(), " ".join(str(k.pid) for k in kids)))
os.replace(pids + ".tmp", pids)
for k in kids:
    k.wait()
    print("pool: grower", k.pid, "returncode", k.returncode, flush=True)
print("pool: done", flush=True)
`
	controllerLiveBandScript = `import os, sys, time
target, held, hold = int(sys.argv[1]), sys.argv[2], float(sys.argv[3])
def anon_pages():
    with open("/proc/meminfo") as f:
        for line in f:
            if line.startswith("AnonPages:"):
                return int(line.split()[1]) << 10
    sys.exit("band: no AnonPages line in /proc/meminfo")
blocks = []
while True:
    short = target - anon_pages()
    if short < 1 << 20:
        break
    b = bytearray(min(short, 64 << 20))
    for i in range(0, len(b), 4096):
        b[i] = 1
    blocks.append(b)
reading = anon_pages()
print("band: holds", sum(map(len, blocks)), "bytes at AnonPages", reading, "for the target", target, "-", hold, "s with nothing growing", flush=True)
with open(held + ".tmp", "w") as f:
    f.write("%d\n" % reading)
os.replace(held + ".tmp", held)
time.sleep(hold)
print("band: done", flush=True)
`
	controllerLiveSharedPoolScript = `import os, signal, subprocess, sys, time
pids, shared_n = sys.argv[1], int(sys.argv[2])
grow = "import time; b=[]; [(b.append(bytearray(64<<20)), time.sleep(0.25)) for _ in iter(int, 1)]"
sleepers = []
if shared_n:
    b = bytearray(shared_n)
    for i in range(0, shared_n, 4096):
        b[i] = 1
    for _ in range(3):
        pid = os.fork()
        if pid == 0:
            time.sleep(3600)
            os._exit(0)
        sleepers.append(pid)
growers = [subprocess.Popen([sys.executable, "-c", grow]) for _ in range(6)]
with open(pids + ".tmp", "w") as f:
    f.write("parent %d\nsleeper %s\ngrower %s\n" % (os.getpid(), " ".join(map(str, sleepers)), " ".join(str(g.pid) for g in growers)))
os.replace(pids + ".tmp", pids)
print("trace: growers", [g.pid for g in growers], "sleepers", sleepers, flush=True)
for g in growers:
    g.wait()
for p in sleepers:
    try:
        os.kill(p, signal.SIGKILL)
    except ProcessLookupError:
        pass
print("trace: done", flush=True)
`
	controllerLiveLatePoolScript = `import os, signal, subprocess, sys, time
pids, held, go = sys.argv[1], sys.argv[2], sys.argv[3]
shared_n, idle_n, idle_bytes, target = int(sys.argv[4]), int(sys.argv[5]), int(sys.argv[6]), int(sys.argv[7])
grow = "import time; b=[]; [(b.append(bytearray(64<<20)), time.sleep(0.25)) for _ in iter(int, 1)]"
def anon():
    with open("/proc/meminfo") as f:
        for l in f:
            if l.startswith("AnonPages:"):
                return int(l.split()[1]) << 10
sleepers = []
if shared_n:
    b = bytearray(shared_n)
    for i in range(0, shared_n, 4096):
        b[i] = 1
    for _ in range(3):
        pid = os.fork()
        if pid == 0:
            time.sleep(3600)
            os._exit(0)
        sleepers.append(pid)
idle_src = "import time; b=bytearray(%d)\nfor i in range(0, len(b), 4096): b[i] = 1\ntime.sleep(3600)" % idle_bytes
idle = [subprocess.Popen([sys.executable, "-c", idle_src]) for _ in range(idle_n)]
if idle:
    time.sleep(5)
blocks = []
while target:
    short = target - anon()
    if short < (1 << 20):
        break
    x = bytearray(min(short, 64 << 20))
    for i in range(0, len(x), 4096):
        x[i] = 1
    blocks.append(x)
with open(held + ".tmp", "w") as f:
    f.write("%d\n" % anon())
os.replace(held + ".tmp", held)
print("probe: held at AnonPages", anon(), "sleepers", sleepers, "idle", len(idle), flush=True)
while not os.path.exists(go):
    time.sleep(0.05)
growers = [subprocess.Popen([sys.executable, "-c", grow]) for _ in range(6)]
with open(pids + ".tmp", "w") as f:
    f.write("parent %d\nsleeper %s\nidle %s\ngrower %s\n" % (os.getpid(), " ".join(map(str, sleepers)),
            " ".join(str(p.pid) for p in idle), " ".join(str(g.pid) for g in growers)))
os.replace(pids + ".tmp", pids)
print("probe: growers", [g.pid for g in growers], flush=True)
for g in growers:
    g.wait()
    print("probe: grower", g.pid, "returncode", g.returncode, flush=True)
for p in sleepers:
    try:
        os.kill(p, signal.SIGKILL)
    except ProcessLookupError:
        print("probe: sleeper", p, "already gone", flush=True)
for p in idle:
    p.kill()
print("probe: done", flush=True)
`
	// controllerLiveCommandPIDs is the file in the run's directory where the shared and the pool
	// probes and the tester's protpool, sharedpool and latepool write their own pids by what each
	// process is, controllerLiveBandHeld the one where the band's holder writes the AnonPages it
	// holds the pod at, and controllerLiveLateHeld and controllerLiveLateGo latepool's word that its
	// block holds the pod and the test's word that its growers may start.
	controllerLiveCommandPIDs = "command-pids"
	controllerLiveBandHeld    = "band-held"
	controllerLiveLateHeld    = "late-held"
	controllerLiveLateGo      = "late-go"
)

// TestControllerPodMemoryGuardLiveSandbox runs the controller pod's memory guard for real, in the
// launcher that is this container's first process, on the container's own /proc and memory
// cgroup, against one scenario (controllerLiveScenarioEnv). INFRA-3531's Jobs run it in gVisor
// pods of 4 GiB and 8 GiB, whose script builds the test binary with `go test -c` and execs it, so
// the binary is PID 1 and containerInit holds of it as it holds of a deployed launcher: nothing
// here fakes the guard's /proc, cgroup, clock, kill or classifier.
//
// The test runs launcher.Run's single-role form against a daemon end of its own on a TCP
// listener, which answers the hello with the controller pod's default settings
// (memoryguard.ControllerDefaults), reads the launcher's state, and starts one generation: the
// test binary run as `worker-shim`, the shim, whose child is the agent, the test binary copied to
// a path of its own and run as Oh My Pi is. The agent holds a quarter of the limit in anonymous
// memory, every page written, and starts the scenario's command as its own child, as Oh My Pi
// starts a command it runs, and waits on it. The daemon end reads every launcher_memory_kill frame
// the launcher sends. The guard the launcher builds also hands the test each reading it takes and
// each kill, on its own loop (controllerLiveWatch): every scenario prints the time from the
// guard's first reading at or above the mark to its first kill, or that none came, and one that
// kills passes only where that time is within two intervals, the settings' Interval twice, and
// prints whether it came within one; beside it the line prints the kill's own processing, from the
// reading it acted on to the kill (markToKill).
//
// `grow` is a python3 that adds 64 MiB every quarter second. It passes where the command ends by
// SIGKILL (the agent's wait reads the status), the shim and the agent live, and exactly one
// launcher_memory_kill reaches the daemon, naming the grower. `mapped` is a python3 that writes,
// maps and reads a file on the pod's disk (TMPDIR) of 1.5 times the act share of the limit and
// keeps it mapped for 60 seconds. It passes where the guard said it started and nothing ends: no
// report, and the reader exits 0 on its own.
//
// `shared` is the shared-page probe of INFRA-3531's tester: a python3 parent writes a block of 3/8
// of the limit and forks three children that only sleep, each of which, as the parent does, reads
// under gVisor larger than the grower it starts next, `grow`'s. It passes where the first process
// the guard ends is the grower, the one that grew the most and not the largest, the pod lived to
// the verdict line, and the shim and the agent live, and it prints every kill by what the probe
// says the process is.
//
// `pool`, `protpool`, `sharedpool` and `latepool` are the tester's probes of a fast rise, six of
// `grow`'s growers started at once, about 1.5 GiB a second together. In `pool` a python3 starts
// them beside the agent's usual quarter of the limit. In `protpool` the agent holds 60% of the
// limit, protected as Oh My Pi is. In `sharedpool` a python3 parent writes a block of 3/8 of the
// limit and forks three children that only sleep, then starts the growers, beside the agent's
// usual quarter. In `latepool` such a block holds the pod's AnonPages at about 83% of the limit,
// in the band and below the mark, and once the test has read that it holds there, six fresh
// growers start, whose first reads come at or near the mark. Each passes where all of these hold,
// and fails naming the first that does not (awaitTraced): every grower the probe started exited
// within controllerLivePoolBound, a grower still alive there failing the run by its pid, so the pod
// lived through the guard's end of the last of them, also where the guard ended the probe's parent
// first (awaitGrowers); the pod lived to the verdict line and the shim and the agent live; the
// guard's first kill came within two intervals of its first reading at or above the mark
// (markToKill); the guard went on as R1 says (judgeTrace): for the kills of each reading, one, or
// at a reading at or above the urgent share every process the guard may end that grew over its
// window and, where the decision holds one that is not new to the stay, every one new to it with
// memory of its own (R1 of spec versions 39, 41 and 42), whose processes had all exited before a
// later reading of the
// guard's at or above the mark and controllerLiveGrowerStep or more above the reading at those
// kills, while a grower lived, the guard's next kill acted on the first such reading; and the first
// kill of each reading that followed a kill of the same rise (no reading of the guard's below the
// mark between them), at a reading controllerLiveGrowerStep or more above that kill's while a
// grower lived, ended a grower. What the first kill of a run ended fails no run, latepool's
// included: c-gvisor-real counts across a wave the latepool runs at 4 GiB whose first kill ended a
// grower within one interval (6 of 7), and asks the rule of each other's, so each traced scenario
// prints its first kill, whether it came within one interval, what it ended and the rule and the
// sizes the guard's ended line names (`live: first kill …`, printFirstKill). The guard ranks the
// processes new to this stay in the band, those it has read at most once since the band's
// second-to-last reading, right after growth over its window, the largest first, and at the mark
// takes as new those its reads there reached, the band's processes it read once and those started
// since the band's last reading (R1 of spec version 32), so latepool's new growers go before its
// sleeping forks and its parent, which read larger. Otherwise the first kill of a rise may end a
// sleeping fork or the parent: the guard orders by growth over its window, then by being new to the
// stay, then by growth since the first kill of its stay in the band, which a dip below the mark that
// stays in the band does not end (R1 of spec version 37), then by size, and a stay's first kill has
// no kill before it, so where no grower grew over the window and none is new, the largest goes
// first. Each prints every kill by what the probe says the process is and, beside the verdict, the
// kills by kind, with each that ended no grower while a grower lived marked first-of-rise,
// after-a-kill or same-reading (`live: kinds …`, printKinds), the first kill (`live: first kill
// …`) and the run's highest readings (`live: highest …`, printHighest).
//
// `band` is a python3 that allocates until the pod's AnonPages is 5% of the limit below the mark,
// in the middle of the band where the guard reads the sizes of what it may end at each reading,
// and holds it there with nothing growing. The test measures a minute of it: the test process's
// CPU time over the minute, as TestMemoryGuardLiveSandbox measures its own, which is the guard's
// with the launcher's and the idle rest of the test process (its own readings are held for the
// minute), the same per reading of the guard's, and the longest time between two of the guard's
// readings in a row in the band. It passes where every reading of the minute was in the band, the
// guard ended nothing, and the holder exited 0 on its own.
//
// Every line goes straight to stdout, one write each, starts `live: ` and ends with its time
// (`at=`), so a pod the kernel ends keeps every line written before the end: the launcher's own
// lines (the guard's `launcher: memory guard: …` among them) and its generation's output,
// relayed; each frame the daemon end reads but the generation's states; AnonPages and the
// cgroup's counter once a second from the test's own readings, which it takes every 100 ms; for
// `grow`, the reading at the kill and the time AnonPages took to fall below it from the kill; for
// `shared` and the tester's four, each kill; for the tester's four, the guard's trace as it goes
// (`live: trace …`, printTrace): each of the guard's readings at or above the band's start, each
// kill marked first of its rise or after a kill of it, each killed process's exit with the time to
// it from the kill (`live: trace exit pid=… after=…`), and at the end the longest of those times
// beside the settings' wait, the kills by kind (`live: kinds …`) and the run's highest readings of
// the guard's and of the test's own (`live: highest …`); for `latepool`, the AnonPages its block
// holds the pod at (`live: late held …`); for `band`, its measure (`live: band …`); and for each,
// the time from the mark to the first kill (`live: mark_to_kill …`). The last is `live: verdict
// PASS <reason>` or `live: verdict FAIL <reason>`. Once LEGION_MEMORY_GUARD_LIVE=1 the test fails,
// and never skips, where it is not PID 1, finds no python3, or cannot read the cgroup's files or
// meminfo.
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
	// settings are what the controller pod's guard runs by, its defaults, which the daemon end's
	// acknowledgement carries too.
	settings memoryguard.Settings
	limit    uint64
	mark     uint64
	// hold is what the agent holds, mappedBytes the mapped file's size, sharedBytes the block the
	// parent of shared, sharedpool or latepool writes, bandTarget the AnonPages the band's holder
	// holds the pod at, and commandArgv the command the agent runs, the scenario's python3.
	hold        int
	mappedBytes int
	sharedBytes int
	bandTarget  uint64
	commandArgv []string
	// dir holds the agent's executable, the files it writes for the test, and the mapped file.
	dir      string
	launcher string
	agentExe string

	frames  *controllerLiveFrames
	sampler *controllerLiveSampler
	watch   *controllerLiveWatch

	mu sync.Mutex
	// guardOff is the guard's first line that says it turned off, "" while it has not, and
	// guardStarted whether the guard has said it started.
	guardOff     string
	guardStarted bool
	// ended holds, by pid, the rule and the sizes of the guard's first ended line of that pid
	// (noteLine), which the first kill's line reads (printFirstKill).
	ended map[int]controllerLiveEnded

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
	if r.watch.traced {
		stop := r.printTrace()
		defer stop()
	}
	if reason := r.awaitAgent(); reason != "" {
		return "", reason
	}
	switch r.scenario {
	case "grow":
		return r.awaitGrow()
	case "mapped":
		return r.awaitMapped()
	case "shared":
		return r.awaitShared()
	case "pool", "protpool", "sharedpool", "latepool":
		return r.awaitTraced()
	}
	return r.awaitBand()
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
// scenario, the controller pod's default settings, the limit, its mark and its band's start, the
// cgroup's counter and AnonPages read once, what the agent holds, the scenario's sizes, the
// command, the test's pid and the kernel's version (gVisor's own under gVisor).
func (r *controllerLiveRun) prepare() string {
	switch r.scenario = os.Getenv(controllerLiveScenarioEnv); r.scenario {
	case "grow", "mapped", "shared", "pool", "protpool", "sharedpool", "latepool", "band":
	default:
		return fmt.Sprintf("%s is %q, want grow, mapped, shared, pool, protpool, sharedpool, latepool or band", controllerLiveScenarioEnv, r.scenario)
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
	r.settings = memoryguard.ControllerDefaults()
	r.limit = limit
	r.mark = controllerLiveShare(limit, r.settings.ActPercent)
	r.hold = int(limit / 4 / page * page)
	r.dir = r.t.TempDir()
	pids := filepath.Join(r.dir, controllerLiveCommandPIDs)
	switch r.scenario {
	case "grow":
		r.commandArgv = []string{python, "-c", controllerLiveGrowScript}
	case "mapped":
		r.mappedBytes = int(r.mark * 3 / 2 / page * page)
		r.commandArgv = []string{python, "-c", controllerLiveMappedScript, filepath.Join(r.dir, "mapped.bin"), strconv.Itoa(r.mappedBytes)}
	case "shared":
		// 3/8 of the limit: beside the agent's quarter, the grower reaches the mark at about a
		// fifth of the limit, so the parent and each sleeper read larger than it.
		r.sharedBytes = int(limit * 3 / 8 / page * page)
		r.commandArgv = []string{python, "-c", controllerLiveSharedScript, strconv.Itoa(r.sharedBytes), pids, controllerLiveGrowScript}
	case "pool":
		r.commandArgv = []string{python, "-c", controllerLivePoolScript, strconv.Itoa(controllerLivePoolGrowers), pids, controllerLiveGrowScript}
	case "protpool":
		// The tester's protpool: the agent, protected as Oh My Pi is, holds 60% of the limit, and
		// the probe forks no sleeper.
		r.hold = int(controllerLiveShare(limit, 60) / page * page)
		r.commandArgv = []string{python, "-c", controllerLiveSharedPoolScript, pids, "0"}
	case "sharedpool":
		r.sharedBytes = int(limit * 3 / 8)
		r.commandArgv = []string{python, "-c", controllerLiveSharedPoolScript, pids, strconv.Itoa(r.sharedBytes)}
	case "latepool":
		// The tester's reckoning: beside the agent's quarter and 2% of the limit more, the block
		// brings the pod's AnonPages to about 83% of the limit, in the band and below the mark.
		r.sharedBytes = int(controllerLiveShare(limit, 83) - uint64(r.hold) - controllerLiveShare(limit, 2))
		r.commandArgv = []string{python, "-c", controllerLiveLatePoolScript, pids, filepath.Join(r.dir, controllerLiveLateHeld),
			filepath.Join(r.dir, controllerLiveLateGo), strconv.Itoa(r.sharedBytes), "0", "0", "0"}
	case "band":
		r.bandTarget = r.mark - controllerLiveShare(limit, controllerLiveBandBelowMark)
		r.commandArgv = []string{python, "-c", controllerLiveBandScript, strconv.FormatUint(r.bandTarget, 10),
			filepath.Join(r.dir, controllerLiveBandHeld), strconv.FormatFloat(controllerLiveBandHold.Seconds(), 'f', -1, 64)}
	}
	r.out.printf("start scenario=%s settings=controller_defaults act_percent=%d floor_percent=%d wait_seconds=%d interval_ms=%d limit=%d mark=%d band_start=%d counter=%d anon_pages=%d agent_holds=%d mapped_bytes=%d shared_bytes=%d band_target=%d pid=%d version=%q command=%q",
		r.scenario, r.settings.ActPercent, r.settings.FloorPercent, int(r.settings.Wait/time.Second), r.settings.Interval.Milliseconds(),
		limit, r.mark, controllerLiveBandStart(limit, r.settings.ActPercent), counter, anon, r.hold, r.mappedBytes, r.sharedBytes,
		r.bandTarget, os.Getpid(), strings.TrimSpace(string(version)), r.commandArgv)
	return ""
}

// controllerLiveShare is percent of limit, as the guard reckons its mark (memoryguard's share).
func controllerLiveShare(limit uint64, percent int) uint64 {
	p := uint64(percent)
	return limit/100*p + limit%100*p/100
}

// controllerLiveBandStart is where the band below the mark at act percent of limit starts, as
// the guard reckons it (memoryguard's bandStart): controllerLiveBandPercent of limit below the
// mark, or 0 for a mark below it.
func controllerLiveBandStart(limit uint64, act int) uint64 {
	mark, band := controllerLiveShare(limit, act), controllerLiveShare(limit, controllerLiveBandPercent)
	if band > mark {
		return 0
	}
	return mark - band
}

// startGeneration runs the controller's launcher, Run's single-role form, until the test's end,
// answers its hello with the controller pod's default settings (controllerDefaultsAck, which
// carries memoryguard.ControllerDefaults), reads its state, and starts its one generation, the
// shim, whose pid the launcher's state names. From there the daemon end reads the connection on a
// goroutine of its own (controllerLiveFrames). The guard the launcher builds is handed the run's
// watch through newMemoryGuard: its Config.Observe, the guard's measurement seam, and its sink,
// which goes on to the launcher's own, so the watch takes each reading and each kill on the
// guard's own loop (controllerLiveWatch), and keeps the guard's trace in a scenario that prints it
// (controllerLiveTraced).
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
	r.watch = &controllerLiveWatch{act: r.settings.ActPercent, traced: controllerLiveTraced(r.scenario)}
	previous := newMemoryGuard
	newMemoryGuard = func(guard memoryguard.Config) *memoryguard.Guard {
		guard.Observe = r.watch.observe
		sink := guard.Sink
		guard.Sink = func(k memoryguard.Kill) {
			r.watch.sink(k)
			if sink != nil {
				sink(k)
			}
		}
		return previous(guard)
	}
	r.t.Cleanup(func() { newMemoryGuard = previous })
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
// whether the guard said it started, and the rule and the sizes of each of its ended lines.
func (r *controllerLiveRun) noteLine(line string) {
	started, off := strings.HasPrefix(line, controllerLiveStartedPrefix), strings.HasPrefix(line, controllerLiveOffPrefix)
	pid, ended, isEnded := controllerLiveEndedOf(line)
	if !started && !off && !isEnded {
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
	if _, seen := r.ended[pid]; isEnded && !seen {
		if r.ended == nil {
			r.ended = map[int]controllerLiveEnded{}
		}
		r.ended[pid] = ended
	}
}

// controllerLiveEnded is what the guard's ended line says of why it ended a process: rule, the
// tier of its order of ends that put the process first (grew, new, grew-since-kill or largest), or
// urgent for a process ended beside the first at a reading at or above the urgent share, and
// sizes, where the sizes it chose the process by came from (band, reread, search or reading), as
// memoryguard's lines.go names them.
type controllerLiveEnded struct{ rule, sizes string }

// controllerLiveEndedOf is the pid, the rule and the sizes the guard's ended line names, and false
// for any other line. It reads only the fields before `executable="`: the executable comes last and
// quoted, and its name may hold any text, a `rule=` among it.
func controllerLiveEndedOf(line string) (int, controllerLiveEnded, bool) {
	rest, ok := strings.CutPrefix(line, controllerLiveEndedPrefix)
	if !ok {
		return 0, controllerLiveEnded{}, false
	}
	rest, _, _ = strings.Cut(rest, ` executable="`)
	pid := 0
	var e controllerLiveEnded
	for _, field := range strings.Fields(rest) {
		key, value, _ := strings.Cut(field, "=")
		switch key {
		case "pid":
			pid, _ = strconv.Atoi(value)
		case "rule":
			e.rule = value
		case "sizes":
			e.sizes = value
		}
	}
	return pid, e, pid != 0
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
// exactly one report reached the daemon and named it, the time from the guard's first reading at
// or above the mark to the kill is within its bound (markToKill), and the shim and the agent live.
// It prints the reading at the kill and the time AnonPages took to fall below it from the kill, by
// the test's own readings, whenever the guard reported a kill.
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
	took, late := r.markToKill()
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
	case late != "":
		return "", late
	case !controllerLiveAlive(r.shim):
		return "", fmt.Sprintf("the shim pid=%d is gone", r.shim)
	case !controllerLiveAlive(r.agent):
		return "", fmt.Sprintf("the agent pid=%d is gone", r.agent)
	}
	return fmt.Sprintf("the guard ended the grower pid=%d (executable=%q) by SIGKILL at a reading of %d (%.2f%% of the limit), %s after its first reading at or above the mark, one launcher_memory_kill reached the daemon, and the shim pid=%d and the agent pid=%d live",
		k.PID, k.Executable, k.ReadingBytes, controllerLivePercent(k.ReadingBytes, k.LimitBytes), took, r.shim, r.agent), ""
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
	r.markToKill()
	kills := r.frames.kills()
	status, _ := r.commandStatus()
	started := r.guardHasStarted()
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

// awaitShared waits for the guard's first kill in the shared-page probe and, where it ended the
// grower, for the probe's exit, which comes once the grower is gone. It passes where the first
// process the guard ended is the grower, which grew the most where the parent and each sleeper
// read larger, the time from the guard's first reading at or above the mark to that kill is within
// its bound (markToKill), and the shim and the agent live. It prints every kill, each by what the
// probe says the process is.
func (r *controllerLiveRun) awaitShared() (pass, fail string) {
	if reason := r.awaitFirstKill("the guard's first kill in the shared-page probe"); reason != "" {
		return "", reason
	}
	names, err := controllerLivePIDs(filepath.Join(r.dir, controllerLiveCommandPIDs))
	if err != nil {
		return "", fmt.Sprintf("the probe's pids: %v", err)
	}
	first := r.frames.kills()[0]
	grower := names[first.PID] == "grower"
	if grower {
		if reason := r.await(controllerLiveExitBound, "the probe's exit once its grower is gone", func() bool {
			_, exited := r.commandStatus()
			return exited
		}); reason != "" {
			return "", reason
		}
		if reason := r.pause(time.Until(first.At.Add(controllerLiveSettle))); reason != "" {
			return "", reason
		}
	}
	kills := r.frames.kills()
	r.printKills(kills, names)
	took, late := r.markToKill()
	switch {
	case !grower:
		return "", fmt.Sprintf("the guard ended first pid=%d, the probe's %s (executable=%q, size=%d), and not its grower pid=%d",
			first.PID, controllerLiveName(names, first.PID), first.Executable, first.SizeBytes, controllerLivePIDOf(names, "grower"))
	case late != "":
		return "", late
	case !controllerLiveAlive(r.shim):
		return "", fmt.Sprintf("the shim pid=%d is gone", r.shim)
	case !controllerLiveAlive(r.agent):
		return "", fmt.Sprintf("the agent pid=%d is gone", r.agent)
	}
	return fmt.Sprintf("the guard ended the grower pid=%d first (size=%d at a reading of %d, %.2f%% of the limit), %s after its first reading at or above the mark, beside the probe's parent pid=%d and its three sleeping forks of a %d-byte block, %d kills in all, and the shim pid=%d and the agent pid=%d live",
		first.PID, first.SizeBytes, first.ReadingBytes, controllerLivePercent(first.ReadingBytes, first.LimitBytes), took,
		controllerLivePIDOf(names, "parent"), r.sharedBytes, len(kills), r.shim, r.agent), ""
}

// awaitTraced runs the tester's probes of a fast rise, pool, protpool, sharedpool and latepool,
// which the guard's trace judges. For latepool it first waits for the probe's block to hold the pod
// in the band and starts the probe's growers (awaitLateHold). It waits for the guard's first kill,
// then for the exit of every grower the probe started and of the probe itself (awaitGrowers), and
// prints every kill, each by what the probe says the process is, and, beside the verdict, the
// kills by kind (printKinds) and, whatever the verdict, the run's first kill (printFirstKill) and
// highest readings (printHighest). It passes where all of these hold, and fails naming the first
// that does not: every grower exited within controllerLivePoolBound, so the pod lived through the
// guard's end of the last of them, and lived to the verdict line, and the shim and the agent live;
// the guard's first kill came within two intervals of its first reading at or above the mark
// (markToKill); and the guard's trace holds, its go-on and its order after a kill of the same rise
// (judgeTrace). What the first kill ended fails no run, latepool's included.
func (r *controllerLiveRun) awaitTraced() (pass, fail string) {
	defer func() {
		r.printFirstKill()
		r.printHighest()
	}()
	if r.scenario == "latepool" {
		if reason := r.awaitLateHold(); reason != "" {
			return "", reason
		}
	}
	if reason := r.awaitFirstKill("the guard's first kill in " + r.scenario); reason != "" {
		return "", reason
	}
	var names map[int]string
	var err error
	if reason := r.await(controllerLiveReportGrace, "the probe's pids", func() bool {
		names, err = controllerLivePIDs(filepath.Join(r.dir, controllerLiveCommandPIDs))
		return err == nil
	}); reason != "" {
		return "", fmt.Sprintf("%s (%v)", reason, err)
	}
	if reason := r.awaitGrowers(names); reason != "" {
		return "", reason
	}
	if reason := r.pause(controllerLiveSettle); reason != "" {
		return "", reason
	}
	r.printKills(r.frames.kills(), names)
	took, late := r.markToKill()
	wentOn, wrong := r.judgeTrace(names)
	kinds := r.printKinds(names)
	t := r.watch.traceCopy()
	first := t.kills[0]
	switch {
	case !controllerLiveAlive(r.shim):
		return "", fmt.Sprintf("the shim pid=%d is gone", r.shim)
	case !controllerLiveAlive(r.agent):
		return "", fmt.Sprintf("the agent pid=%d is gone", r.agent)
	case late != "":
		return "", late
	case wrong != "":
		return "", wrong
	}
	growers, ended := 0, 0
	for _, name := range names {
		if name == "grower" {
			growers++
		}
	}
	for _, k := range t.kills {
		if names[k.PID] == "grower" {
			ended++
		}
	}
	longest, _, measured := controllerLiveLongestExit(t.kills)
	return fmt.Sprintf("the guard ended %d processes of %s, %d of its %d growers, %s, and every grower and the probe exited; its first kill pid=%d, the probe's %s, at a reading of %d (%.2f%% of the limit) came %s after its first reading at or above the mark; %d times every process the guard killed at a reading had exited and a later reading at or above the mark rose %d MiB or more above that reading while a grower lived, and each time the guard's next kill acted on the first such reading; the first kill of each reading after a kill of the same rise at a reading %d MiB or more above that kill's, while a grower lived, ended a grower; the longest of %d measured times from a kill to its process's exit was %s, against the wait of %s; and the shim pid=%d and the agent pid=%d live",
		len(t.kills), r.scenario, ended, growers, kinds, first.PID, controllerLiveName(names, first.PID), first.ReadingBytes, controllerLivePercent(first.ReadingBytes, first.LimitBytes),
		took, wentOn, controllerLiveGrowerStep>>20, controllerLiveGrowerStep>>20, measured, longest, r.settings.Wait, r.shim, r.agent), ""
}

// awaitGrowers waits, within controllerLivePoolBound, for every grower the probe's pids name
// (names) to have exited and for the probe itself to have exited, and answers why not, naming each
// grower that still lived. The probe's exit alone does not do: where the guard ends the probe's
// parent before its last grower, as it did in three sharedpool runs of INFRA-3531 T17, the
// parent's exit comes while that grower still grows, and the run has still to show the guard
// ending it and the pod living through that. Each grower is held by a pidfd opened here, right
// after the guard's first kill, and has exited once that pidfd reads readable, or once the pidfd
// the watch's sink opened at its kill did (its exit in the trace), which holds the grower itself
// even where its pid has since passed to another process; a grower whose pid names no process at
// the open has exited and been reaped already.
func (r *controllerLiveRun) awaitGrowers(names map[int]string) string {
	var growers []int
	for pid, name := range names {
		if name == "grower" {
			growers = append(growers, pid)
		}
	}
	if len(growers) == 0 {
		return fmt.Sprintf("the probe's pids name no grower: %v", names)
	}
	slices.Sort(growers)
	held := map[int]int{}
	defer func() {
		for _, fd := range held {
			_ = unix.Close(fd)
		}
	}()
	for _, pid := range growers {
		fd, err := unix.PidfdOpen(pid, 0)
		switch {
		case err == nil:
			held[pid] = fd
		case !errors.Is(err, unix.ESRCH):
			return fmt.Sprintf("pidfd_open of the probe's grower pid=%d: %v", pid, err)
		}
	}
	living := func() []string {
		exited := map[int]bool{}
		for _, k := range r.watch.traceCopy().kills {
			if !k.exitAt.IsZero() {
				exited[k.PID] = true
			}
		}
		var left []string
		for _, pid := range growers {
			if fd, ok := held[pid]; ok && !exited[pid] && !controllerLivePidfdExited(fd) {
				left = append(left, strconv.Itoa(pid))
			}
		}
		return left
	}
	reason := r.await(controllerLivePoolBound, "the exit of every grower of the probe's and of the probe", func() bool {
		_, exited := r.commandStatus()
		return exited && len(living()) == 0
	})
	if left := living(); reason != "" && len(left) > 0 {
		return fmt.Sprintf("the probe's grower pid=%s still lived: %s", strings.Join(left, ","), reason)
	}
	return reason
}

// controllerLivePidfdExited asks without waiting whether the pidfd fd reads readable, which is its
// process having exited, as the guard's own Handle.Exited asks.
func controllerLivePidfdExited(fd int) bool {
	fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	n, err := unix.Poll(fds, 0)
	return err == nil && n > 0 && fds[0].Revents&unix.POLLIN != 0
}

// awaitLateHold waits for latepool's word that its block holds the pod's AnonPages in the band,
// prints what it holds (`live: late held …`), and tells the probe to start its growers, by its go
// file. The growers must start from below the mark with nothing ended, so it fails where the guard
// ended a process, or the probe ended, before the hold.
func (r *controllerLiveRun) awaitLateHold() string {
	var held []byte
	if reason := r.await(controllerLiveStartBound, "the probe's word that its block holds the pod in the band", func() bool {
		var err error
		held, err = os.ReadFile(filepath.Join(r.dir, controllerLiveLateHeld))
		_, exited := r.commandStatus()
		return err == nil || exited || r.watch.snapshot().firstKill != nil
	}); reason != "" {
		return reason
	}
	if k := r.watch.snapshot().firstKill; k != nil {
		r.markToKill()
		return fmt.Sprintf("the guard ended pid=%d (executable=%q, size=%d) at a reading of %d (%.2f%% of the limit) before the probe's growers started",
			k.PID, k.Executable, k.SizeBytes, k.ReadingBytes, controllerLivePercent(k.ReadingBytes, k.LimitBytes))
	}
	if status, exited := r.commandStatus(); exited {
		return fmt.Sprintf("the probe pid=%d ended with %s before its block held the pod in the band", r.command, controllerLiveStatus(status))
	}
	r.out.printf("late held anon_pages=%s shared_bytes=%d band_start=%d mark=%d",
		strings.TrimSpace(string(held)), r.sharedBytes, controllerLiveBandStart(r.limit, r.settings.ActPercent), r.mark)
	if err := controllerLiveWrite(filepath.Join(r.dir, controllerLiveLateGo), "go\n"); err != nil {
		return fmt.Sprintf("the probe's go file: %v", err)
	}
	return ""
}

// awaitBand waits for the holder's word that it holds the pod's AnonPages in the band, measures
// the guard over controllerLiveBandWindow from there, and waits for the holder's exit. It prints
// the measure, `live: band cpu=… per_tick=… longest_gap=… ticks=…`: the CPU time the test process
// used over the window, read as TestMemoryGuardLiveSandbox reads its own (getrusage of the whole
// process: the guard's loop and its hourly scans, with the rest of the launcher and the idle rest
// of the test, which one process cannot tell apart; the test's own readings are held for the
// window, since one read of the cgroup's counter costs about 18 ms under gVisor), that time over
// the guard's readings in the window, the longest time between two of the guard's readings in a
// row that were both in the band, each of which read the sizes of what it may end, and the
// readings' count. It passes where the guard took readings in the window, every one of them in the
// band, ended nothing, the holder exited 0 on its own, and the shim and the agent live.
func (r *controllerLiveRun) awaitBand() (pass, fail string) {
	var held []byte
	if reason := r.await(controllerLiveStartBound, "the holder's word that it holds the pod in the band", func() bool {
		var err error
		held, err = os.ReadFile(filepath.Join(r.dir, controllerLiveBandHeld))
		_, exited := r.commandStatus()
		return err == nil || exited || len(r.frames.kills()) > 0
	}); reason != "" {
		return "", reason
	}
	if kills := r.frames.kills(); len(kills) > 0 {
		r.markToKill()
		return "", fmt.Sprintf("the guard ended %s before the holder held the pod in the band", controllerLiveKills(kills))
	}
	if status, exited := r.commandStatus(); exited {
		return "", fmt.Sprintf("the holder pid=%d ended with %s before it held the pod in the band", r.command, controllerLiveStatus(status))
	}
	bandStart := controllerLiveBandStart(r.limit, r.settings.ActPercent)
	r.out.printf("band held anon_pages=%s target=%d band_start=%d mark=%d window=%s",
		strings.TrimSpace(string(held)), r.bandTarget, bandStart, r.mark, controllerLiveBandWindow)

	r.sampler.hold(true)
	userBefore, sysBefore, errBefore := controllerLiveCPU()
	r.watch.startBand()
	start := time.Now()
	reason := r.pause(controllerLiveBandWindow)
	band := r.watch.endBand()
	userAfter, sysAfter, errAfter := controllerLiveCPU()
	elapsed := time.Since(start)
	r.sampler.hold(false)
	switch {
	case reason != "":
		return "", reason
	case errBefore != nil || errAfter != nil:
		return "", fmt.Sprintf("getrusage: %v", errors.Join(errBefore, errAfter))
	}
	user, sys := userAfter-userBefore, sysAfter-sysBefore
	cpu := user + sys
	var perTick time.Duration
	if band.ticks > 0 {
		perTick = cpu / time.Duration(band.ticks)
	}
	r.out.printf("band cpu=%s per_tick=%s longest_gap=%s ticks=%d user=%s sys=%s elapsed=%s percent_of_core=%.3f outside_band=%d reading_low=%d reading_high=%d band_start=%d mark=%d",
		cpu, perTick, band.longest, band.ticks, user, sys, elapsed, 100*cpu.Seconds()/elapsed.Seconds(), band.outside,
		band.low, band.high, bandStart, r.mark)

	if reason := r.await(controllerLiveBandHold-controllerLiveBandWindow+controllerLiveExitBound, "the holder's exit", func() bool {
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
	r.markToKill()
	kills := r.frames.kills()
	status, _ := r.commandStatus()
	switch {
	case !r.guardHasStarted():
		return "", "the guard never started"
	case len(kills) > 0:
		return "", fmt.Sprintf("the guard ended %s while the holder held the pod in the band, below the mark, with nothing growing", controllerLiveKills(kills))
	case band.ticks == 0:
		return "", fmt.Sprintf("the guard took no reading in the %s window", controllerLiveBandWindow)
	case band.outside > 0:
		return "", fmt.Sprintf("%d of the guard's %d readings in the window were outside the band from %d to the mark %d (lowest %d, highest %d): the holder did not hold the pod there",
			band.outside, band.ticks, bandStart, r.mark, band.low, band.high)
	case !status.Exited() || status.ExitStatus() != 0:
		return "", fmt.Sprintf("the holder pid=%d ended with %s and not exit status 0", r.command, controllerLiveStatus(status))
	case !controllerLiveAlive(r.shim):
		return "", fmt.Sprintf("the shim pid=%d is gone", r.shim)
	case !controllerLiveAlive(r.agent):
		return "", fmt.Sprintf("the agent pid=%d is gone", r.agent)
	}
	return fmt.Sprintf("the holder pid=%d held the pod's AnonPages in the band (%d to %d, the band from %d to the mark %d) for %s with nothing growing, the guard took %d readings there with %s of the test process's CPU time (%s a reading, the longest gap %s) and ended nothing, and the holder exited 0",
		r.command, band.low, band.high, bandStart, r.mark, controllerLiveBandWindow, band.ticks, cpu, perTick, band.longest), ""
}

// awaitFirstKill waits for the first report of a kill the daemon end reads, or for the command's
// exit. A command that exited first is given controllerLiveReportGrace for a report, since its
// exit can reach the test before the report of the kill that ended it or its child: the guard
// reads the cgroup's counter between its kill and its report.
func (r *controllerLiveRun) awaitFirstKill(what string) string {
	if reason := r.await(controllerLiveKillBound, what, func() bool {
		_, exited := r.commandStatus()
		return len(r.frames.kills()) > 0 || exited
	}); reason != "" {
		return reason
	}
	if len(r.frames.kills()) > 0 {
		return ""
	}
	status, _ := r.commandStatus()
	if reason := r.await(controllerLiveReportGrace, what, func() bool {
		return len(r.frames.kills()) > 0
	}); reason != "" {
		return fmt.Sprintf("the command pid=%d ended with %s, and %s", r.command, controllerLiveStatus(status), reason)
	}
	return ""
}

// markToKill prints the time from the guard's first reading at or above the mark to its first
// kill (`live: mark_to_kill …`), from the guard's own readings and the kill's At as its sink took
// them (controllerLiveWatch), or that no kill came, and answers that time and why it fails the
// run, "" where it does not.
//
// The bound is controllerLiveFirstKillIntervals of the guard's intervals (its settings' Interval,
// 100 ms), from that first reading to the first kill's At, as c-gvisor-real bounds each run. The
// line says too whether the kill came within one interval (`within_one=`), R1's bound, which the
// check counts across a wave rather than failing a run on. The kill's own processing, the time
// from the reading it acted on to its At, in which the guard took that reading, scanned the
// processes, read the sizes of those it may end, and opened, checked and signalled the one it
// ended, counts within the bound, and the line prints it beside the bound, measured and not
// assumed, since under gVisor and six growers the guard's work can take longer than at rest. The
// guard waits one interval after a reading's work before it takes its next reading, so two of its
// readings are always more than one interval apart: a first kill within one interval is one on the
// first reading at or above the mark whose processing took less than one interval, and one within
// two acted on that reading or on the next.
func (r *controllerLiveRun) markToKill() (took time.Duration, fail string) {
	seen := r.watch.snapshot()
	k := seen.firstKill
	switch {
	case k == nil && seen.firstAtMark.IsZero():
		r.out.printf("mark_to_kill none: no reading of the guard's reached the mark %d; its highest was %d (%.2f%% of the limit) at %s",
			r.mark, seen.highest, controllerLivePercent(seen.highest, r.limit), controllerLiveTime(seen.highestAt))
		return 0, ""
	case k == nil:
		r.out.printf("mark_to_kill none: the guard's reading reached the mark %d at %s (%d, %.2f%% of the limit) and it ended nothing",
			r.mark, controllerLiveTime(seen.firstAtMark), seen.firstAtMarkReading, controllerLivePercent(seen.firstAtMarkReading, r.limit))
		return 0, ""
	case seen.firstAtMark.IsZero():
		r.out.printf("mark_to_kill none: the guard ended pid=%d at %s before any reading of its reached the mark %d",
			k.PID, controllerLiveTime(k.At), r.mark)
		return 0, fmt.Sprintf("the guard ended pid=%d with no reading of its at or above the mark %d", k.PID, r.mark)
	}
	took = k.At.Sub(seen.firstAtMark)
	processing := k.At.Sub(seen.killReadingAt)
	bound := controllerLiveFirstKillIntervals * r.settings.Interval
	r.out.printf("mark_to_kill %s within_one=%v bound=%s interval=%s processing=%s first_at_mark=%s first_at_mark_reading=%d first_at_mark_share=%.2f%% kill_reading_at=%s kill_reading=%d readings_at_mark=%d kill_at=%s pid=%d",
		took, took <= r.settings.Interval, bound, r.settings.Interval, processing, controllerLiveTime(seen.firstAtMark), seen.firstAtMarkReading,
		controllerLivePercent(seen.firstAtMarkReading, r.limit), controllerLiveTime(seen.killReadingAt), k.ReadingBytes,
		seen.atMark, controllerLiveTime(k.At), k.PID)
	if took > bound {
		return took, fmt.Sprintf("the guard's first kill came %s after its first reading at or above the mark, past %d intervals (%s): it acted at its reading number %d at or above the mark, and its own processing, from that reading to the kill, took %s",
			took, controllerLiveFirstKillIntervals, bound, seen.atMark, processing)
	}
	return took, ""
}

// printFirstKill prints the run's first kill in one line, before a traced scenario's verdict:
// `live: first kill +<ms>ms within_one=<true|false> kind=<kind> rule=<rule> sizes=<sizes>
// pid=<pid>`, its time as the trace counts it (printTrace), whether it came within one interval of
// the guard's first reading at or above the mark (markToKill), what the probe says the process is
// (grower, sleeper, parent, or other where the probe's pids name it not), and the rule and the
// sizes of the guard's ended line of it (noteLine), `-` where none came; or `live: first kill
// none`. c-gvisor-real counts across a wave the runs whose first kill came within one interval,
// the latepool runs at 4 GiB whose first kill ended a grower within one, and the rule of each that
// did not, from these lines, so no run fails on them.
func (r *controllerLiveRun) printFirstKill() {
	seen := r.watch.snapshot()
	k := seen.firstKill
	if k == nil {
		r.out.printf("first kill none")
		return
	}
	names, _ := controllerLivePIDs(filepath.Join(r.dir, controllerLiveCommandPIDs))
	r.mu.Lock()
	ended, ok := r.ended[k.PID]
	r.mu.Unlock()
	if !ok {
		ended = controllerLiveEnded{rule: "-", sizes: "-"}
	}
	within := !seen.firstAtMark.IsZero() && k.At.Sub(seen.firstAtMark) <= r.settings.Interval
	r.out.printf("first kill +%dms within_one=%v kind=%s rule=%s sizes=%s pid=%d",
		k.At.Sub(r.watch.traceCopy().first).Milliseconds(), within, controllerLiveName(names, k.PID), ended.rule, ended.sizes, k.PID)
}

// printKills prints each report the daemon end read, the process it names by what the scenario's
// probe wrote of its own pids (names), `other` where it wrote nothing of that pid.
func (r *controllerLiveRun) printKills(kills []shimwire.LauncherMemoryKill, names map[int]string) {
	for i, k := range kills {
		r.out.printf("kill %d/%d pid=%d is=%s executable=%q size=%d reading=%d share_of_limit=%.2f%% counter=%d sequence=%d kill_at=%s",
			i+1, len(kills), k.PID, controllerLiveName(names, k.PID), k.Executable, k.SizeBytes, k.ReadingBytes,
			controllerLivePercent(k.ReadingBytes, k.LimitBytes), k.CounterBytes, k.Sequence, controllerLiveTime(k.At))
	}
}

// controllerLiveTraced is whether scenario prints the guard's trace (printTrace): the tester's four
// probes of a fast rise (awaitTraced), each of which ends process after process in one rise.
func controllerLiveTraced(scenario string) bool {
	switch scenario {
	case "pool", "protpool", "sharedpool", "latepool":
		return true
	}
	return false
}

// printTrace prints the guard's trace (controllerLiveTrace) as it grows, at each of the test's own
// readings: one `live: trace reading …` line for each of the guard's readings at or above the
// band's start, with its time since the first of them and its share of the limit; one `live: trace
// kill …` for each kill, with its time, the pid, what the probe says the process is, its size, the
// reading it acted on, and whether it was the first kill of its rise, followed a kill of it at an
// earlier reading, or was sent beside the kill before it at the same reading, at or above the
// urgent share (`rise=first-of-rise`, `rise=after-a-kill` or `rise=same-reading`,
// controllerLiveRise); and one `live: trace exit pid=… after=…` for each killed process's exit,
// with the time from the kill to the exit. A kill's line names the process by the probe's pids,
// which the probe writes once its growers have started, so the lines from the first kill on wait
// for them. It answers the stop that prints the rest, the probe's pids written or not, and then
// the longest time from a kill to its process's exit beside the settings' wait, which
// c-gvisor-real asks be longer than every such time (`live: trace longest_exit=…`).
func (r *controllerLiveRun) printTrace() (stop func()) {
	path := filepath.Join(r.dir, controllerLiveCommandPIDs)
	var names map[int]string
	next := 0
	flush := func(end bool) {
		if names == nil {
			names, _ = controllerLivePIDs(path)
		}
		var lines []string
		lines, next = r.watch.traceLines(next, names, !end)
		for _, line := range lines {
			r.out.printf("%s", line)
		}
	}
	quit, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(controllerLiveSampleEvery)
		defer tick.Stop()
		for {
			select {
			case <-quit:
				flush(true)
				return
			case <-tick.C:
				flush(false)
			}
		}
	}()
	return func() {
		close(quit)
		<-done
		kills := r.watch.traceCopy().kills
		longest, pid, measured := controllerLiveLongestExit(kills)
		if measured == 0 {
			r.out.printf("trace longest_exit=none wait=%s exits=0 kills=%d", r.settings.Wait, len(kills))
			return
		}
		r.out.printf("trace longest_exit=%s pid=%d is=%s wait=%s wait_longer=%v exits=%d kills=%d",
			longest, pid, controllerLiveName(names, pid), r.settings.Wait, r.settings.Wait > longest, measured, len(kills))
	}
}

// judgeTrace judges the guard's trace by the probe's pids (names), and answers how many times its
// go-on rule applied and held, and why the trace fails, "" where it holds.
//
// First the go-on rule of R1 (spec version 26): once a killed process has exited, where the
// reading has not fallen below the reading at its kill and a process the guard may end has grown
// since that kill, the guard ends the next in its order at once. It is judged for the kills of
// each reading together (controllerLiveReadingKills): one kill, or at a reading at or above the
// urgent share every process the guard may end that grew over its window and, where the decision
// holds one that is not new to the stay, every one new to it with memory of its own, each a kill
// of its own at that same reading (spec versions 39, 41 and 42), after which the guard waits until
// every one of them has
// exited. For the kills of a reading whose processes its pidfds showed all exited before a later
// reading of the guard's, the first such reading after the last exit, at or above the mark and
// controllerLiveGrowerStep or more above the reading at those kills while a grower lived
// (controllerLiveGoOnReading), is one at which a grower has grown since them, since the killed
// processes' memory is gone and the reading is higher by more than anything but a grower moves, so
// the guard's next kill must have acted on that reading.
//
// Then the order after a kill: the first kill of each reading that follows a kill of the same rise
// at an earlier reading (controllerLiveRise, `after-a-kill`), made at a reading
// controllerLiveGrowerStep or more above that kill's while a grower lived (controllerLiveGrowerAt,
// at the kill), ended a grower, since a grower has grown since the previous kill, and so since the
// stay's first kill, by which the guard's order ranks before size. A first kill of a rise is not
// judged here by what it ended: where no grower grew over the guard's window, none is new to the
// stay with memory of its own and none grew since the stay's first kill, its order puts the
// largest first, a sleeping fork or the parent under gVisor; what a run's first kill ended fails
// no run (printFirstKill). A kill after one, at a reading less than that above the kill's, is not
// judged either, since the reading may have fallen and no grower need have grown; nor is a kill
// sent beside the first of its reading (`same-reading`), which shares that kill's reading and ended
// a process that grew over the window or, where the decision held one that is not new to the stay,
// one new to it with memory of its own.
func (r *controllerLiveRun) judgeTrace(names map[int]string) (wentOn int, fail string) {
	t := r.watch.traceCopy()
	for i := 0; i < len(t.kills); {
		end, last := controllerLiveReadingKills(t.kills, i)
		reading, grower, found := controllerLiveGoOnReading(t, i, end, last, names, r.settings.ActPercent)
		if !found {
			i = end
			continue
		}
		next := "it ended nothing more"
		if end < len(t.kills) {
			n := t.kills[end]
			if n.readingAt.Equal(reading.at) {
				wentOn++
				i = end
				continue
			}
			next = fmt.Sprintf("its next kill, pid=%d, acted on its reading at +%dms (%.2f%% of the limit)",
				n.PID, n.readingAt.Sub(t.first).Milliseconds(), controllerLivePercent(n.ReadingBytes, n.LimitBytes))
		}
		k := t.kills[last]
		return wentOn, fmt.Sprintf("the process of the guard's kill %d of %d (pid=%d, on its reading of %.2f%% of the limit at +%dms), the last to exit of the %d it killed at that reading, exited %s after the kill, and the guard's reading at +%dms was %.2f%%, at or above the mark and %d MiB above the reading at that kill, while its grower pid=%d lived, but the guard did not act on that reading: %s",
			last+1, len(t.kills), k.PID, controllerLivePercent(k.ReadingBytes, k.LimitBytes), k.readingAt.Sub(t.first).Milliseconds(), end-i,
			k.exitAt.Sub(k.At), reading.at.Sub(t.first).Milliseconds(), controllerLivePercent(reading.reading, reading.limit),
			(reading.reading-k.ReadingBytes)>>20, grower, next)
	}
	for i, k := range t.kills {
		if i == 0 || k.firstOfRise || k.sameReading || names[k.PID] == "grower" {
			continue
		}
		previous := t.kills[i-1]
		if k.ReadingBytes < previous.ReadingBytes+controllerLiveGrowerStep {
			continue
		}
		if grower := controllerLiveGrowerAt(names, t.kills, k.At); grower != 0 {
			return wentOn, fmt.Sprintf("the guard's kill %d of %d at +%dms followed its kill %d of the same rise and acted on a reading of %.2f%% of the limit, %d MiB above that kill's %.2f%%, while its grower pid=%d lived, but it ended pid=%d, the probe's %s (executable=%q, size=%d), and not a grower",
				i+1, len(t.kills), k.At.Sub(t.first).Milliseconds(), i, controllerLivePercent(k.ReadingBytes, k.LimitBytes),
				(k.ReadingBytes-previous.ReadingBytes)>>20, controllerLivePercent(previous.ReadingBytes, previous.LimitBytes), grower,
				k.PID, controllerLiveName(names, k.PID), k.Executable, k.SizeBytes)
		}
	}
	return wentOn, ""
}

// printKinds prints, beside the verdict, the trace's kills by what the probe says each process is
// (names): one `live: kinds non_grower …` line for each kill that ended a process other than a
// grower while a grower lived (controllerLiveGrowerAt, at the kill), marked first-of-rise,
// after-a-kill or same-reading (controllerLiveRise), and then `live: kinds …`, the count of each
// kind and of those kills, all and by their place in the rise. It answers the counts in words, for
// the pass reason. Such a kill fails a run only where judgeTrace says so: a first kill of a rise
// may end a sleeping fork or the parent, and so may a kill after one at a reading less than
// controllerLiveGrowerStep above that kill's; a kill sent beside another at the same reading ended
// a process that grew over the guard's window or, where the decision held one that is not new to
// the stay, one new to it with memory of its own (INFRA-3531 R1 of spec versions 39, 41 and 42),
// and is counted apart, since it is neither the first of its rise nor the guard's answer to a
// later reading.
func (r *controllerLiveRun) printKinds(names map[int]string) string {
	t := r.watch.traceCopy()
	counts := map[string]int{}
	for _, name := range names {
		counts[name] = 0
	}
	firstOfRise, afterAKill, sameReading := 0, 0, 0
	for i, k := range t.kills {
		name := controllerLiveName(names, k.PID)
		counts[name]++
		if name == "grower" {
			continue
		}
		grower := controllerLiveGrowerAt(names, t.kills, k.At)
		if grower == 0 {
			continue
		}
		switch {
		case k.sameReading:
			sameReading++
		case k.firstOfRise:
			firstOfRise++
		default:
			afterAKill++
		}
		r.out.printf("kinds non_grower kill=%d/%d pid=%d is=%s rise=%s size=%d reading_share=%.2f%% kill_at=+%dms grower_lived=%d",
			i+1, len(t.kills), k.PID, name, controllerLiveRise(k), k.SizeBytes, controllerLivePercent(k.ReadingBytes, k.LimitBytes),
			k.At.Sub(t.first).Milliseconds(), grower)
	}
	parts := make([]string, 0, len(counts))
	for _, name := range slices.Sorted(maps.Keys(counts)) {
		parts = append(parts, fmt.Sprintf("%s=%d", name, counts[name]))
	}
	byKind := strings.Join(parts, " ")
	nonGrower := firstOfRise + afterAKill + sameReading
	r.out.printf("kinds kills=%d %s non_grower_while_grower_lived=%d first_of_rise=%d after_a_kill=%d same_reading=%d",
		len(t.kills), byKind, nonGrower, firstOfRise, afterAKill, sameReading)
	return fmt.Sprintf("by kind %s, %d of them of no grower while a grower lived (%d first-of-rise, %d after-a-kill, %d same-reading)",
		byKind, nonGrower, firstOfRise, afterAKill, sameReading)
}

// controllerLiveRise is where k stands in its rise, as its trace line and printKinds mark it:
// `same-reading` for a kill sent beside the one before it at the same reading, at or above the
// urgent share (INFRA-3531 R1 of spec version 39); else `first-of-rise` for the trace's first kill
// and a kill after a reading of the guard's below the mark, and `after-a-kill` for one that
// follows a kill of the same rise at a later reading.
func controllerLiveRise(k controllerLiveTraceKill) string {
	switch {
	case k.sameReading:
		return "same-reading"
	case k.firstOfRise:
		return "first-of-rise"
	}
	return "after-a-kill"
}

// controllerLiveReadingKills is, for the trace's kill i, the first of the kills the guard sent at
// one reading, end, the index after the last of them (each after the first marked sameReading),
// and last, the one of them whose process exited last, -1 where the exit of one of them was not
// measured. At a reading at or above the urgent share the guard ends every process it may end that
// grew over its window and, where the decision holds one that is not new to the stay, every one
// new to it with memory of its own, and it waits after them until every one has exited (INFRA-3531
// R1 of spec versions 39, 41 and 42), so its go-on is judged from the last of those exits; below
// that share a reading's kills are one, and end is i+1.
func controllerLiveReadingKills(kills []controllerLiveTraceKill, i int) (end, last int) {
	last = i
	for end = i; end < len(kills) && (end == i || kills[end].sameReading); end++ {
		switch {
		case last < 0:
		case kills[end].exitAt.IsZero():
			last = -1
		case kills[end].exitAt.After(kills[last].exitAt):
			last = end
		}
	}
	return end, last
}

// controllerLiveGoOnReading is, for the kills the guard sent at one reading, kills i to end of the
// trace (controllerLiveReadingKills), the first of the guard's readings after the one they acted
// on, and up to the one the next kill acted on, that was taken after the exit of every process
// they killed, the last of which is kill last's, is at or above the mark of act percent and
// controllerLiveGrowerStep or more above the reading at those kills, and at which a grower of names
// lived, with that grower, and false where none is. A reading after the next kill's is the next
// kill's to answer for, so a next kill that came earlier, on a reading that had fallen or risen
// less, holds the rule; and kills of which one's exit was not measured (last -1) have none.
func controllerLiveGoOnReading(t controllerLiveTrace, i, end, last int, names map[int]string, act int) (controllerLiveReading, int, bool) {
	if last < 0 {
		return controllerLiveReading{}, 0, false
	}
	k, exit := t.kills[i], t.kills[last].exitAt
	for _, rd := range t.readings {
		switch {
		case !rd.at.After(k.readingAt):
			continue
		case end < len(t.kills) && rd.at.After(t.kills[end].readingAt):
			return controllerLiveReading{}, 0, false
		case !exit.Before(rd.at) || rd.reading < controllerLiveShare(rd.limit, act) || rd.reading < k.ReadingBytes+controllerLiveGrowerStep:
			continue
		}
		if grower := controllerLiveGrowerAt(names, t.kills, rd.at); grower != 0 {
			return rd, grower, true
		}
	}
	return controllerLiveReading{}, 0, false
}

// controllerLiveGrowerAt is the lowest pid of a grower of names that lived at at, 0 where none
// did. A grower grows until it is ended, and in the probes only the guard ends one, so it lived at
// at unless the guard had ended it and the pidfd its sink opened had shown it exited by then; an
// exit that was not measured counts as none.
func controllerLiveGrowerAt(names map[int]string, kills []controllerLiveTraceKill, at time.Time) int {
	found := 0
	for pid, name := range names {
		if name != "grower" || (found != 0 && pid > found) {
			continue
		}
		exited := false
		for _, k := range kills {
			if k.PID == pid && !k.exitAt.IsZero() && !k.exitAt.After(at) {
				exited = true
			}
		}
		if !exited {
			found = pid
		}
	}
	return found
}

// controllerLiveLongestExit is the longest time from a kill of kills to its process's exit, the
// pid it was of, and how many of the exits were measured.
func controllerLiveLongestExit(kills []controllerLiveTraceKill) (longest time.Duration, pid, measured int) {
	for _, k := range kills {
		if k.exitAt.IsZero() {
			continue
		}
		measured++
		if took := k.exitAt.Sub(k.At); measured == 1 || took > longest {
			longest, pid = took, k.PID
		}
	}
	return longest, pid, measured
}

// guardHasStarted is whether the guard has said it started.
func (r *controllerLiveRun) guardHasStarted() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.guardStarted
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
	highest, counter := controllerLiveHighest(samples)
	r.out.printf("end samples=%d highest_anon_pages=%d highest_anon_share=%.2f%% highest_anon_at=%s highest_counter=%d highest_counter_share=%.2f%% kills=%d",
		len(samples), highest.anon, controllerLivePercent(highest.anon, r.limit), controllerLiveTime(highest.at), counter.counter,
		controllerLivePercent(counter.counter, r.limit), len(r.frames.kills()))
}

// printHighest prints the run's highest readings in one line (`live: highest …`), each in bytes,
// as a share of the limit and with its time: the guard's own highest reading (controllerLiveWatch),
// and the highest AnonPages and the highest counter of the test's own readings, which it takes
// every 100 ms beside the guard's (controllerLiveSampler). c-gvisor-real asks that no run's highest
// reading come within 50 MiB of the kill point, the last AnonPages before the host's kill that the
// guard-off grow-64 Job of the same limit measures and this test cannot read, so the line carries
// what that margin is judged from, offline. The traced scenarios print it before the verdict.
func (r *controllerLiveRun) printHighest() {
	seen := r.watch.snapshot()
	anon, counter, samples := r.sampler.highest()
	r.out.printf("highest guard_reading=%d guard_share=%.2f%% guard_at=%s anon_pages=%d anon_share=%.2f%% anon_at=%s counter=%d counter_share=%.2f%% counter_at=%s samples=%d limit=%d",
		seen.highest, controllerLivePercent(seen.highest, r.limit), controllerLiveTime(seen.highestAt),
		anon.anon, controllerLivePercent(anon.anon, r.limit), controllerLiveTime(anon.at),
		counter.counter, controllerLivePercent(counter.counter, r.limit), controllerLiveTime(counter.at), samples, r.limit)
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

// controllerLiveCPU is the user and system CPU time the test process has used so far, every
// thread of it, as TestMemoryGuardLiveSandbox's liveCPU reads its own.
func controllerLiveCPU() (user, sys time.Duration, err error) {
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		return 0, 0, err
	}
	return time.Duration(usage.Utime.Nano()), time.Duration(usage.Stime.Nano()), nil
}

// controllerLivePIDs reads the pids a probe wrote at path, one line for each kind of process it
// starts, the kind's name and then its pids, none where it started none of that kind (the tester's
// probes write `sleeper` alone where they fork no sleeper), and answers each pid by its kind's
// name.
func controllerLivePIDs(path string) (map[int]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	names := map[int]string{}
	for line := range strings.Lines(string(data)) {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			return nil, fmt.Errorf("%s holds an empty line, want a name and its pids", path)
		}
		for _, field := range fields[1:] {
			pid, err := strconv.Atoi(field)
			if err != nil {
				return nil, fmt.Errorf("%s holds the line %q: %w", path, strings.TrimSpace(line), err)
			}
			names[pid] = fields[0]
		}
	}
	return names, nil
}

// controllerLiveName is what names says pid is, `other` where it says nothing of it.
func controllerLiveName(names map[int]string, pid int) string {
	if name, ok := names[pid]; ok {
		return name
	}
	return "other"
}

// controllerLivePIDOf is the lowest pid names calls name, 0 where it calls none so.
func controllerLivePIDOf(names map[int]string, name string) int {
	found := 0
	for pid, n := range names {
		if n == name && (found == 0 || pid < found) {
			found = pid
		}
	}
	return found
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

// controllerLiveWatch is what the test learns from the guard's own loop: each reading through
// Config.Observe and each kill through the guard's sink, both called on that loop and read by the
// test under mu (startGeneration hands both to the guard the launcher builds). act is the guard's
// act share, by which the watch reckons the mark and the band's start from each reading's limit,
// and traced whether it keeps the guard's trace (controllerLiveTrace) and holds each process the
// guard ends by a pidfd until it exits.
type controllerLiveWatch struct {
	act    int
	traced bool

	mu    sync.Mutex
	seen  controllerLiveSeen
	band  controllerLiveBand
	trace controllerLiveTrace
}

// controllerLiveSeen is what the watch has taken of the guard's readings and kills so far.
type controllerLiveSeen struct {
	// highest is the guard's highest reading, and highestAt its time; lastAt is the time of its
	// latest reading.
	highest   uint64
	highestAt time.Time
	lastAt    time.Time
	// firstAtMark is the time of the guard's first reading at or above the mark, zero while none
	// has been, firstAtMarkReading that reading, and atMark how many of its readings were at or
	// above the mark until its first kill, the one that kill acted at included.
	firstAtMark        time.Time
	firstAtMarkReading uint64
	atMark             int
	// firstKill is the guard's first kill as its sink took it, nil while it has ended nothing,
	// and killReadingAt the time of the reading it acted at: the guard's latest reading when its
	// sink took the kill, since the guard ends a process on the loop that took that reading,
	// after Observe and before its next reading.
	firstKill     *shimwire.LauncherMemoryKill
	killReadingAt time.Time
}

// controllerLiveBand is the band scenario's count of the guard's readings while it is on
// (startBand to endBand): how many the guard took, how many were outside the band (below its
// start, or at or above the mark), the lowest and the highest, and the longest time between two
// readings in a row that were both in the band, each of which read the sizes of what the guard
// may end.
type controllerLiveBand struct {
	on             bool
	ticks, outside int
	low, high      uint64
	longest        time.Duration
	prevAt         time.Time
	prevIn         bool
}

// controllerLiveTrace is the guard's trace in a traced scenario, from which the test prints one
// line for each entry (printTrace) and judges the guard (judgeTrace): each of its readings at or
// above the band's start, each kill as its sink took it with the exit of the process it ended, and
// events, every one of those in the order it came. first is the time of the first reading at or
// above the band's start, from which each line counts its time, and belowMark whether a reading of
// the guard's, of any height, was below the mark since its latest kill: the kill after such a
// reading is the first of a new rise.
type controllerLiveTrace struct {
	first     time.Time
	readings  []controllerLiveReading
	kills     []controllerLiveTraceKill
	events    []controllerLiveEvent
	belowMark bool
}

// controllerLiveReading is one of the guard's readings as Observe handed it: the time its clock
// read as it took the reading, AnonPages and the limit.
type controllerLiveReading struct {
	at             time.Time
	reading, limit uint64
}

// controllerLiveTraceKill is one kill as the guard's sink took it. readingAt is the time of the
// reading it acted on, the guard's latest when its sink took the kill; firstOfRise whether it is
// the first kill of its rise, the trace's first or one after a reading below the mark, and not one
// that follows a kill of the same rise; sameReading whether the kill before it acted on the same
// reading, a process the guard ended beside the first of that reading at a reading at or above
// the urgent share, where it ends every process it may end that grew over its window and, where
// the decision holds one that is not new to the stay, every one new to it with memory of its own
// (INFRA-3531 R1 of spec versions 39, 41 and 42), which keeps the first's firstOfRise; and exitAt
// the time the pidfd the sink opened on the process it ended first read readable, the process's
// exit, zero while it has not or where it was not measured; exitNote says why it was not, or how
// it was where that was not the pidfd's own word (sink).
type controllerLiveTraceKill struct {
	shimwire.LauncherMemoryKill
	readingAt   time.Time
	firstOfRise bool
	sameReading bool
	exitAt      time.Time
	exitNote    string
}

// controllerLiveEvent is one entry of a trace, in the order it came: a reading, a kill or a
// killed process's exit, by its index in the trace's readings or kills.
type controllerLiveEvent struct {
	kind  controllerLiveEventKind
	index int
}

// controllerLiveEventKind is what one entry of a trace is.
type controllerLiveEventKind int

const (
	controllerLiveEventReading controllerLiveEventKind = iota
	controllerLiveEventKill
	controllerLiveEventExit
)

// observe is the guard's Config.Observe.
func (w *controllerLiveWatch) observe(now time.Time, reading, limit uint64) {
	mark, bandStart := controllerLiveShare(limit, w.act), controllerLiveBandStart(limit, w.act)
	w.mu.Lock()
	defer w.mu.Unlock()
	s := &w.seen
	s.lastAt = now
	if reading > s.highest {
		s.highest, s.highestAt = reading, now
	}
	if reading >= mark && s.firstKill == nil {
		if s.firstAtMark.IsZero() {
			s.firstAtMark, s.firstAtMarkReading = now, reading
		}
		s.atMark++
	}
	if w.traced {
		t := &w.trace
		t.belowMark = t.belowMark || reading < mark
		if reading >= bandStart {
			if t.first.IsZero() {
				t.first = now
			}
			t.readings = append(t.readings, controllerLiveReading{at: now, reading: reading, limit: limit})
			t.events = append(t.events, controllerLiveEvent{kind: controllerLiveEventReading, index: len(t.readings) - 1})
		}
	}
	b := &w.band
	if !b.on {
		return
	}
	if b.ticks == 0 {
		b.low, b.high = reading, reading
	}
	b.low, b.high = min(b.low, reading), max(b.high, reading)
	b.ticks++
	in := reading >= bandStart && reading < mark
	switch {
	case !in:
		b.outside++
	case b.prevIn:
		b.longest = max(b.longest, now.Sub(b.prevAt))
	}
	b.prevAt, b.prevIn = now, in
}

// sink takes the guard's kills before the launcher's own sink does, and keeps the first. A traced
// watch keeps each in its trace, marked the first of its rise where it is the trace's first or a
// reading below the mark came since the kill before, and marked sent at the same reading where the
// kill before acted on the guard's same latest reading, and holds the process it ended by a pidfd,
// opened here on the guard's loop right after the kill, before the process's parent can reap it
// and its pid pass to another, and waited on off that loop (timeExit). A process its parent reaped
// before the open is gone by then, and the open's time is taken as its exit: later than the real
// one, never earlier.
func (w *controllerLiveWatch) sink(k memoryguard.Kill) {
	var fd int
	var err error
	var opened time.Time
	if w.traced {
		fd, err = unix.PidfdOpen(k.PID, 0)
		opened = time.Now()
	}
	w.mu.Lock()
	s, t := &w.seen, &w.trace
	if s.firstKill == nil {
		s.firstKill, s.killReadingAt = &k.LauncherMemoryKill, s.lastAt
	}
	i := len(t.kills)
	if w.traced {
		same := i > 0 && t.kills[i-1].readingAt.Equal(s.lastAt)
		first := i == 0 || t.belowMark || (same && t.kills[i-1].firstOfRise)
		t.belowMark = false
		t.kills = append(t.kills, controllerLiveTraceKill{LauncherMemoryKill: k.LauncherMemoryKill, readingAt: s.lastAt, firstOfRise: first, sameReading: same})
		t.events = append(t.events, controllerLiveEvent{kind: controllerLiveEventKill, index: i})
	}
	w.mu.Unlock()
	switch {
	case !w.traced:
	case err == nil:
		go w.timeExit(i, fd)
	case errors.Is(err, unix.ESRCH):
		w.noteExit(i, opened, "reaped before the sink held it: the exit is taken at that pidfd_open")
	default:
		w.noteExit(i, time.Time{}, fmt.Sprintf("pidfd_open: %v", err))
	}
}

// snapshot is what the watch has seen so far.
func (w *controllerLiveWatch) snapshot() controllerLiveSeen {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.seen
}

// traceCopy is the trace so far, its readings and its kills copied, without its events.
func (w *controllerLiveWatch) traceCopy() controllerLiveTrace {
	w.mu.Lock()
	defer w.mu.Unlock()
	t := w.trace
	t.readings = append([]controllerLiveReading(nil), t.readings...)
	t.kills = append([]controllerLiveTraceKill(nil), t.kills...)
	t.events = nil
	return t
}

// traceLines is the trace's lines from its event from on, one for each reading, kill and exit
// (printTrace), and the event the next call starts at. A kill's line names the process by names,
// `other` where it names nothing of the pid; held is whether the lines from a kill on wait while
// names is nil, the probe's pids not yet written.
func (w *controllerLiveWatch) traceLines(from int, names map[int]string, held bool) ([]string, int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	t := &w.trace
	var lines []string
	for ; from < len(t.events); from++ {
		e := t.events[from]
		switch e.kind {
		case controllerLiveEventReading:
			rd := t.readings[e.index]
			lines = append(lines, fmt.Sprintf("trace reading +%dms share=%.2f%% reading=%d",
				rd.at.Sub(t.first).Milliseconds(), controllerLivePercent(rd.reading, rd.limit), rd.reading))
		case controllerLiveEventKill:
			if names == nil && held {
				return lines, from
			}
			k := t.kills[e.index]
			lines = append(lines, fmt.Sprintf("trace kill +%dms pid=%d is=%s rise=%s size=%d reading_share=%.2f%% reading=%d reading_at=+%dms sequence=%d",
				k.At.Sub(t.first).Milliseconds(), k.PID, controllerLiveName(names, k.PID), controllerLiveRise(k), k.SizeBytes,
				controllerLivePercent(k.ReadingBytes, k.LimitBytes), k.ReadingBytes, k.readingAt.Sub(t.first).Milliseconds(), k.Sequence))
		case controllerLiveEventExit:
			k := t.kills[e.index]
			switch {
			case k.exitAt.IsZero():
				lines = append(lines, fmt.Sprintf("trace exit pid=%d after=none reason=%q", k.PID, k.exitNote))
			case k.exitNote != "":
				lines = append(lines, fmt.Sprintf("trace exit pid=%d after=%s note=%q", k.PID, k.exitAt.Sub(k.At), k.exitNote))
			default:
				lines = append(lines, fmt.Sprintf("trace exit pid=%d after=%s", k.PID, k.exitAt.Sub(k.At)))
			}
		}
	}
	return lines, from
}

// noteExit keeps the exit of the process the trace's kill i ended, at at, zero where it was not
// measured, with note, and adds it to the trace's events.
func (w *controllerLiveWatch) noteExit(i int, at time.Time, note string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	t := &w.trace
	t.kills[i].exitAt, t.kills[i].exitNote = at, note
	t.events = append(t.events, controllerLiveEvent{kind: controllerLiveEventExit, index: i})
}

// timeExit waits, off the guard's loop, for the process the trace's kill i ended to exit, which
// its pidfd fd shows by reading readable, as the guard's own Handle.Exited reads it, and keeps the
// time it first read so, or why it did not within controllerLiveExitBound (noteExit). It polls in
// slices of controllerLiveExitSlice, and closes fd.
func (w *controllerLiveWatch) timeExit(i, fd int) {
	defer func() { _ = unix.Close(fd) }()
	fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	for deadline := time.Now().Add(controllerLiveExitBound); time.Now().Before(deadline); {
		n, err := unix.Poll(fds, int(controllerLiveExitSlice/time.Millisecond))
		switch {
		case errors.Is(err, unix.EINTR):
		case err != nil:
			w.noteExit(i, time.Time{}, fmt.Sprintf("poll of its pidfd: %v", err))
			return
		case n > 0 && fds[0].Revents&unix.POLLIN != 0:
			w.noteExit(i, time.Now(), "")
			return
		case n > 0:
			w.noteExit(i, time.Time{}, fmt.Sprintf("its pidfd polled %#x, not readable", fds[0].Revents))
			return
		}
	}
	w.noteExit(i, time.Time{}, fmt.Sprintf("it had not exited %s after the kill", controllerLiveExitBound))
}

// startBand starts the band's count afresh, and endBand ends it and answers it.
func (w *controllerLiveWatch) startBand() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.band = controllerLiveBand{on: true}
}

func (w *controllerLiveWatch) endBand() controllerLiveBand {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.band.on = false
	return w.band
}

// controllerLiveSample is one of the test's own readings: AnonPages and the cgroup's counter.
type controllerLiveSample struct {
	at            time.Time
	anon, counter uint64
}

// controllerLiveSampler takes one reading every controllerLiveSampleEvery until end, keeps each,
// and prints one sample line every controllerLivePrintEvery readings: once a second. It takes none
// while it is held (hold).
type controllerLiveSampler struct {
	out   *controllerLiveLog
	limit uint64
	stop  chan struct{}
	done  chan struct{}

	mu      sync.Mutex
	samples []controllerLiveSample
	err     error
	held    bool
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
		if !s.isHeld() && !s.take(n) {
			return
		}
		select {
		case <-s.stop:
			return
		case <-tick.C:
		}
	}
}

// take takes reading n and keeps it, prints it where it is a printed one, and answers false where
// it failed, which ends the readings.
func (s *controllerLiveSampler) take(n int) bool {
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
		return false
	case n%controllerLivePrintEvery == 0:
		s.out.write(fmt.Sprintf("sample anon_pages=%d anon_share=%.2f%% counter=%d counter_share=%.2f%% at=%s",
			sample.anon, controllerLivePercent(sample.anon, s.limit), sample.counter,
			controllerLivePercent(sample.counter, s.limit), controllerLiveTime(sample.at)))
	}
	return true
}

// hold stops the readings while held is true, and takes them again once it is false: the band
// scenario measures the test process's CPU time with none of the test's own readings in it,
// since one read of the cgroup's counter costs about 18 ms under gVisor (memoryguard's package
// doc), far more than the guard's own reading.
func (s *controllerLiveSampler) hold(held bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.held = held
}

func (s *controllerLiveSampler) isHeld() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.held
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

// highest is the test's reading with the highest AnonPages so far and the one with the highest
// counter (controllerLiveHighest), and how many it has taken.
func (s *controllerLiveSampler) highest() (anon, counter controllerLiveSample, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	anon, counter = controllerLiveHighest(s.samples)
	return anon, counter, len(s.samples)
}

// controllerLiveHighest is the sample of samples with the highest AnonPages and the one with the
// highest counter, the earliest of each where several are equal, and zero ones for none.
func controllerLiveHighest(samples []controllerLiveSample) (anon, counter controllerLiveSample) {
	for _, s := range samples {
		if s.anon > anon.anon {
			anon = s
		}
		if s.counter > counter.counter {
			counter = s
		}
	}
	return anon, counter
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

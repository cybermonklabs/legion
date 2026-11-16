package memoryguard

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// fakeCgroup is a container's memory cgroup directory of either version, written in place as the
// kernel writes it, since the guest watch holds its files open and reads them from their start.
type fakeCgroup struct {
	t       *testing.T
	version string
	dir     string
}

// fakeStat is a memory.stat's figures, each written under its version's key; noShmem leaves the
// shared memory key out.
type fakeStat struct {
	anon, file, inactiveFile, shmem uint64
	noShmem                         bool
}

// newFakeCgroup is a cgroup of version under t.TempDir(): no kill yet, a usage of a GiB, the fake
// pod's limit, and a memory.stat; v2's memory.oom.group off.
func newFakeCgroup(t *testing.T, version string) *fakeCgroup {
	t.Helper()
	return newFakeCgroupIn(t, version, t.TempDir())
}

func newFakeCgroupIn(t *testing.T, version, dir string) *fakeCgroup {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	c := &fakeCgroup{t: t, version: version, dir: dir}
	c.setKills(0)
	c.setUsage(gib)
	c.setLimit(strconv.FormatUint(podLimit, 10))
	c.setStat(fakeStat{anon: 700 * mib, file: 300 * mib, inactiveFile: 100 * mib, shmem: 16 * mib})
	if version == cgroupV2 {
		c.write("memory.oom.group", "0\n")
	}
	return c
}

func (c *fakeCgroup) write(name, content string) {
	c.t.Helper()
	if err := os.WriteFile(filepath.Join(c.dir, name), []byte(content), 0o644); err != nil {
		c.t.Fatal(err)
	}
}

func (c *fakeCgroup) path(name string) string { return filepath.Join(c.dir, name) }

// setKills writes the kill counter: v2's memory.events, v1's memory.oom_control.
func (c *fakeCgroup) setKills(n uint64) {
	if c.version == cgroupV2 {
		c.write("memory.events", fmt.Sprintf("low 0\nhigh 0\nmax 41\noom 3\noom_kill %d\noom_group_kill 0\n", n))
		return
	}
	c.write("memory.oom_control", fmt.Sprintf("oom_kill_disable 0\nunder_oom 0\noom_kill %d\n", n))
}

func (c *fakeCgroup) setUsage(n uint64) {
	name := "memory.usage_in_bytes"
	if c.version == cgroupV2 {
		name = "memory.current"
	}
	c.write(name, strconv.FormatUint(n, 10)+"\n")
}

func (c *fakeCgroup) setLimit(text string) {
	name := "memory.limit_in_bytes"
	if c.version == cgroupV2 {
		name = "memory.max"
	}
	c.write(name, text+"\n")
}

// setStat writes memory.stat as its version does: v2's own figures, and v1's figures of the
// cgroup alone beside the totals of its whole subtree, which are the ones the watch reads.
func (c *fakeCgroup) setStat(s fakeStat) {
	var b strings.Builder
	if c.version == cgroupV2 {
		fmt.Fprintf(&b, "anon %d\nfile %d\nkernel 8388608\nkernel_stack 1048576\nsock 0\n", s.anon, s.file)
		if !s.noShmem {
			fmt.Fprintf(&b, "shmem %d\n", s.shmem)
		}
		fmt.Fprintf(&b, "file_mapped 1048576\nanon_thp 0\ninactive_anon 0\nactive_anon %d\ninactive_file %d\nactive_file %d\n",
			s.anon, s.inactiveFile, s.file-s.inactiveFile)
	} else {
		fmt.Fprintf(&b, "cache 4096\nrss 4096\nrss_huge 0\nshmem 4096\nmapped_file 0\ninactive_file 4096\nhierarchical_memory_limit %d\n", podLimit)
		fmt.Fprintf(&b, "total_cache %d\ntotal_rss %d\n", s.file, s.anon)
		if !s.noShmem {
			fmt.Fprintf(&b, "total_shmem %d\n", s.shmem)
		}
		fmt.Fprintf(&b, "total_inactive_anon 0\ntotal_active_anon %d\ntotal_inactive_file %d\ntotal_active_file %d\n",
			s.anon, s.inactiveFile, s.file-s.inactiveFile)
	}
	c.write("memory.stat", b.String())
}

// guestProcess is a fakeProcess with its oom_score_adj, which the guest watch reads and raises, and
// its oom_score, the kernel's badness of it, which the watch does not read.
type guestProcess struct {
	fakeProcess
	adj   int
	score int
}

// addGuest adds each process with its oom_score_adj, its oom_score and a status as every process
// in /proc has one: SIGTERM pending for its thread group and SIGKILL in no pending mask, so only the
// status a test writes with showKill or reaped shows the kernel's kill.
func (p *fakePod) addGuest(gps ...guestProcess) {
	p.t.Helper()
	for _, gp := range gps {
		p.add(gp.fakeProcess)
		p.setOOMScore(gp.pid, gp.score)
		p.setScore(gp.pid, gp.adj)
		if gp.threads == 0 {
			p.writeWhole(filepath.Join(p.proc, strconv.Itoa(gp.pid), "status"), fmt.Sprintf(
				"Name:\t%s\nState:\tS (sleeping)\nTgid:\t%d\nPid:\t%d\nThreads:\t1\nSigQ:\t1/63451\nSigPnd:\t0000000000000000\nShdPnd:\t0000000000004000\nSigBlk:\t0000000000000100\n",
				gp.name, gp.pid, gp.pid))
		}
	}
}

// setScore writes pid's oom_score_adj.
func (p *fakePod) setScore(pid, adj int) {
	p.t.Helper()
	p.writeWhole(filepath.Join(p.proc, strconv.Itoa(pid), "oom_score_adj"), fmt.Sprintf("%d\n", adj))
}

// setOOMScore writes pid's oom_score, the kernel's badness of it, by which the kernel picks the
// process it kills; the watch does not read it.
func (p *fakePod) setOOMScore(pid, score int) {
	p.t.Helper()
	p.writeWhole(filepath.Join(p.proc, strconv.Itoa(pid), "oom_score"), fmt.Sprintf("%d\n", score))
}

// setSelfScore writes the oom_score_adj of the process that reads the pod's /proc, the watch's
// starting score.
func (p *fakePod) setSelfScore(adj int) {
	p.t.Helper()
	if err := os.MkdirAll(filepath.Join(p.proc, "self"), 0o755); err != nil {
		p.t.Fatal(err)
	}
	p.writeWhole(filepath.Join(p.proc, "self", "oom_score_adj"), fmt.Sprintf("%d\n", adj))
}

// score is pid's oom_score_adj as its file reads now.
func (p *fakePod) score(pid int) int {
	p.t.Helper()
	data, err := os.ReadFile(filepath.Join(p.proc, strconv.Itoa(pid), "oom_score_adj"))
	if err != nil {
		p.t.Fatal(err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		p.t.Fatal(err)
	}
	return n
}

// The pids of guestTree: a tester's role-init, shim and agent; the agent's broker helper, the
// LSP multiplexer helper the broker started, the gopls the multiplexer runs and the eval kernel
// the broker runs; a shell command the agent runs; and two processes of no role, one the
// classifier protects (the test database's) and one it does not (a kubectl exec's shell).
const (
	guestInit = 100 + iota
	guestShim
	guestAgent
	guestBroker
	guestMux
	guestGopls
	guestEval
	guestBash
	guestOutsider
	guestTestDB
)

// guestTree adds guestTree's processes to pod, each at oom_score_adj start, and answers what a
// classifier says of them: the role-init, the shim, the agent and its helpers are the tester's and
// protected, gopls, the eval kernel and the command are the tester's, the test database's process
// is protected and of no role, and the exec's shell is neither. No process here has an oom_score
// above 1150, so a test's process above it is the first the kernel would pick.
func guestTree(pod *fakePod, start int) map[int]Class {
	const omp = "/opt/omp/bin/omp"
	const legion = "/usr/local/bin/legion"
	at := func(pid, ppid int, exe string, args []string, score int, resident uint64) guestProcess {
		return guestProcess{fakeProcess: fakeProcess{pid: pid, ppid: ppid, exe: exe, args: args, resident: resident}, adj: start, score: score}
	}
	pod.addGuest(
		at(guestInit, 1, legion, []string{"legion", "launcher", "role-init", "--role", "tester"}, 2, 8*mib),
		at(guestShim, guestInit, legion, []string{"legion", "worker-shim"}, 3, 30*mib),
		at(guestAgent, guestShim, omp, []string{omp, "--mode", "rpc"}, 260, 2*gib),
		at(guestBroker, guestAgent, omp, []string{omp, "__omp_worker_daemon_broker"}, 40, 200*mib),
		at(guestMux, guestBroker, omp, []string{omp, "__omp_worker_lsp_mux"}, 10, 60*mib),
		at(guestGopls, guestMux, "/usr/local/bin/gopls", []string{"/usr/local/bin/gopls", "serve"}, 1150, 600*mib),
		at(guestEval, guestBroker, omp, []string{omp, "__omp_worker_js_eval_process"}, 1050, 100*mib),
		at(guestBash, guestAgent, "/usr/bin/bash", []string{"bash", "-c", "make"}, 1001, 4*mib),
		at(guestOutsider, 1, "/usr/bin/bash", []string{"bash"}, 5, 4*mib),
		at(guestTestDB, 1, "/usr/lib/postgresql/17/bin/postgres", []string{"postgres", "-D", "/roles/test-pg/data"}, 90, 300*mib),
	)
	tester := func(protected bool) Class { return Class{Role: "tester", Protected: protected} }
	return map[int]Class{
		guestInit: tester(true), guestShim: tester(true), guestAgent: tester(true), guestBroker: tester(true), guestMux: tester(true),
		guestGopls: tester(false), guestEval: tester(false), guestBash: tester(false),
		guestTestDB: {Protected: true},
	}
}

// command is a process of the tester's that the agent's command runs, under the shell command.
func command(pid int, start uint64, name string, score int, resident uint64) guestProcess {
	return guestProcess{fakeProcess: fakeProcess{pid: pid, ppid: guestBash, start: start, args: []string{name}, resident: resident}, score: score}
}

// showKill writes pid's status as the kernel's out-of-memory kill leaves a process of the pod: its
// SIGKILL pending for the whole thread group (ShdPnd), where the kill sends it, and for the thread
// as well (SigPnd) until the thread takes it on its way out. A zombie's thread has taken it: its
// status reads state Z, with SIGKILL in ShdPnd alone.
func showKill(pod *fakePod, pid int, name string, zombie bool) {
	pod.t.Helper()
	state, thread := "R (running)", "0000000000000100"
	if zombie {
		state, thread = "Z (zombie)", "0000000000000000"
	}
	pod.writeWhole(filepath.Join(pod.proc, strconv.Itoa(pid), "status"), fmt.Sprintf(
		"Name:\t%s\nState:\t%s\nTgid:\t%d\nPid:\t%d\nThreads:\t1\nSigQ:\t1/63451\nSigPnd:\t%s\nShdPnd:\t0000000000000100\n",
		name, state, pid, pid, thread))
}

// unnamedLine is the watch's line of count kills, of a counter at total, that no process was named
// for: with the pod's memory at the burst's first rise, the anonymous memory, the usage and the
// shared memory the test's cgroup read then, and the fake pod's limit.
func unnamedLine(count, total, anon, usage, shmem uint64) string {
	return fmt.Sprintf("launcher: guest memory: killed unnamed count=%d oom_kill=%d reading=%d counter=%d shmem=%d limit=%d",
		count, total, anon, usage, shmem, podLimit)
}

// guestHarness runs one guest watch over a /proc and a cgroup directory with a fake clock, one
// scan at a time.
type guestHarness struct {
	t       *testing.T
	clock   *fakeClock
	log     *lineLog
	g       *Guest
	mu      sync.Mutex
	kills   []Kill
	pending fakeWait
	cancel  context.CancelFunc
	done    chan struct{}
	stopped sync.Once
}

// newGuestHarness is a watch of proc and the cgroup at cgroupDir whose classifier answers classes
// for every scan.
func newGuestHarness(t *testing.T, proc, cgroupDir string, classes map[int]Class) *guestHarness {
	t.Helper()
	h := &guestHarness{t: t, clock: newFakeClock(), log: &lineLog{}}
	h.g = NewGuest(GuestConfig{
		Proc: proc, Cgroup: cgroupDir, Logf: h.log.logf, Clock: h.clock,
		Classify: func(*Table) map[int]Class { return classes },
		Sink: func(k Kill) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.kills = append(h.kills, k)
		},
	})
	return h
}

// run starts the watch and returns once its start and its first scan are done.
func (h *guestHarness) run() *guestHarness {
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel, h.done = cancel, make(chan struct{})
	go func() {
		defer close(h.done)
		h.g.Run(ctx)
	}()
	h.t.Cleanup(h.stop)
	h.pending = h.nextWait()
	return h
}

func (h *guestHarness) nextWait() fakeWait {
	h.t.Helper()
	select {
	case w := <-h.clock.waits:
		return w
	case <-h.done:
		h.t.Fatalf("Run returned before ctx ended; the log:\n%s", strings.Join(h.log.all(), "\n"))
	case <-time.After(testwait.Bounded(h.t, time.Minute, time.Second)):
		h.t.Fatalf("the watch never waited for its next scan; the log:\n%s", strings.Join(h.log.all(), "\n"))
	}
	return fakeWait{}
}

// tick lets the watch take its next scan, and returns once that scan is done.
func (h *guestHarness) tick() { h.tickAfter(0) }

// tickAfter lets the watch take its next scan extra after the interval it waits for.
func (h *guestHarness) tickAfter(extra time.Duration) {
	h.t.Helper()
	w := h.pending
	w.fire <- h.clock.advance(w.d + extra)
	h.pending = h.nextWait()
}

func (h *guestHarness) stop() {
	h.stopped.Do(func() {
		if h.cancel == nil {
			return
		}
		h.cancel()
		select {
		case <-h.done:
		case <-time.After(10 * time.Second):
			h.t.Errorf("Run did not return after ctx ended")
		}
	})
}

func (h *guestHarness) reported() []Kill {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.kills)
}

// lines are the watch's own lines that start with what, after the prefix every one carries.
func (h *guestHarness) lines(what string) []string {
	var out []string
	for _, line := range h.log.all() {
		if strings.HasPrefix(line, guestPrefix+what) {
			out = append(out, line)
		}
	}
	return out
}

func (h *guestHarness) logged() string { return strings.Join(h.log.all(), "\n") }

// One scan raises every process of the role the classifier does not protect, a shell command the
// agent runs and what Oh My Pi's helpers run alike (gopls, the eval kernel), to 1000; each
// protected process keeps the starting score, and one that reads above it, the agent that raised
// itself and a protected process of no role, is set back to it. A process of no role that the
// classifier does not protect is left as it reads. A process already at 1000 is not written again,
// and no score is written again at a later scan: the hour counts the first scan's five writes.
func TestGuestRaisesARolesCommandsAndKeepsItsOwnProcessesAtTheStartingScore(t *testing.T) {
	const start = 300
	pod := newFakePod(t)
	pod.setSelfScore(start)
	classes := guestTree(pod, start)
	pod.setScore(guestAgent, raisedScore)
	pod.setScore(guestTestDB, raisedScore)
	pod.setScore(guestOutsider, 0)
	h := newGuestHarness(t, pod.proc, newFakeCgroup(t, cgroupV2).dir, classes).run()
	for _, tc := range []struct {
		pid, want int
		what      string
	}{
		{guestBash, raisedScore, "a shell command, the agent's child"},
		{guestGopls, raisedScore, "gopls under the multiplexer the broker helper started"},
		{guestEval, raisedScore, "the eval kernel"},
		{guestInit, start, "the role-init"},
		{guestShim, start, "the shim"},
		{guestAgent, start, "the agent, which had raised its own score"},
		{guestBroker, start, "the broker helper"},
		{guestMux, start, "the multiplexer helper"},
		{guestTestDB, start, "a protected process of no role that read raised"},
		{guestOutsider, 0, "a process of no role the classifier does not protect"},
	} {
		if got := pod.score(tc.pid); got != tc.want {
			t.Errorf("%s (pid %d) reads oom_score_adj %d after one scan, want %d", tc.what, tc.pid, got, tc.want)
		}
	}
	h.tick()
	h.tick()
	h.tickAfter(time.Hour)
	hours := h.lines("hour ")
	if len(hours) != 1 || !strings.HasSuffix(hours[0], " raised=5 unraised=0") {
		t.Fatalf("the hourly lines are %q, want one counting 5 score writes and no failed one; the log:\n%s", hours, h.logged())
	}
	if failed := h.lines("score failed"); len(failed) != 0 {
		t.Fatalf("the watch logged %q, want no failed score write", failed)
	}
}

// The counter rising by one while a command of the tester's shows the kernel's kill, still in /proc
// with SIGKILL pending while its memory is freed (reaped), names that command: the guard's own
// line with its role, its executable and its resident size at the scan before, the cgroup's
// anonymous memory, usage, shared memory and limit at the rise, and one Kill of the tester's with
// the kernel's cause, the watch's id and the first sequence. The command is the shell command's
// child, whose exit goes to the shell and never to the launcher.
func TestGuestNamesTheProcessTheKernelKilledAndTellsItsRole(t *testing.T) {
	const load = 800
	for _, version := range []string{cgroupV1, cgroupV2} {
		t.Run(version, func(t *testing.T) {
			pod := newFakePod(t)
			pod.setSelfScore(0)
			classes := guestTree(pod, 0)
			pod.addGuest(command(load, 8000, "python3", 1400, 1500*mib))
			classes[load] = Class{Role: "tester"}
			cgroup := newFakeCgroup(t, version)
			h := newGuestHarness(t, pod.proc, cgroup.dir, classes).run()

			cgroup.setUsage(3900 * mib)
			cgroup.setStat(fakeStat{anon: 3500 * mib, file: 400 * mib, inactiveFile: 120 * mib, shmem: 32 * mib})
			reaped(pod, load, 8000, "python3")
			cgroup.setKills(1)
			h.tick()
			at := h.clock.Now()

			want := fmt.Sprintf(`launcher: memory guard: ended pid=%d role=tester size=%d reading=%d counter=%d shmem=%d limit=%d executable="python3"`,
				load, 1500*mib, 3500*mib, 3900*mib, 32*mib, podLimit)
			if got := h.log.with("ended "); !slices.Equal(got, []string{want}) {
				t.Fatalf("the kill lines are %q, want %q; the log:\n%s", got, want, h.logged())
			}
			wantKill := Kill{
				LauncherMemoryKill: shimwire.LauncherMemoryKill{
					Guard: h.g.ID(), Sequence: 1, PID: load, Executable: "python3", SizeBytes: 1500 * mib,
					ReadingBytes: 3500 * mib, CounterBytes: 3900 * mib, LimitBytes: podLimit, At: at, Cause: shimwire.MemoryKillCauseKernel,
				},
				Role: "tester",
			}
			kills := h.reported()
			if len(kills) != 1 || kills[0] != wantKill {
				t.Fatalf("the sink took %+v, want the one kill %+v", kills, wantKill)
			}
			if err := kills[0].Validate(); err != nil {
				t.Fatal(err)
			}
			if unnamed := h.lines("killed unnamed"); len(unnamed) != 0 {
				t.Fatalf("the watch logged %q, want no unnamed kill", unnamed)
			}
		})
	}
}

// A rise of two while two of the tester's commands show the kernel's kill names both, in pid
// order, as the first two kills, each at its own size: cc1, then ld, the larger. The third, as,
// which read above both before the rise, exits by itself: it shows no kill and is named for none,
// and no kill is left unnamed.
func TestGuestNamesEachProcessThatShowsTheKillInPidOrderAndNoneThatExitedByItself(t *testing.T) {
	const cc1, ld, as = 810, 811, 812
	pod := newFakePod(t)
	pod.setSelfScore(0)
	classes := guestTree(pod, 0)
	pod.addGuest(
		command(cc1, 8100, "cc1", 1200, 300*mib),
		command(ld, 8110, "ld", 1200, 900*mib),
		command(as, 8120, "as", 1500, 2*gib),
	)
	for _, pid := range []int{cc1, ld, as} {
		classes[pid] = Class{Role: "tester"}
	}
	cgroup := newFakeCgroup(t, cgroupV2)
	h := newGuestHarness(t, pod.proc, cgroup.dir, classes).run()

	reaped(pod, ld, 8110, "ld")
	reaped(pod, cc1, 8100, "cc1")
	pod.remove(as)
	cgroup.setKills(2)
	h.tick()
	kills := h.reported()
	if len(kills) != 2 || kills[0].PID != cc1 || kills[0].Sequence != 1 || kills[0].SizeBytes != 300*mib ||
		kills[1].PID != ld || kills[1].Sequence != 2 || kills[1].SizeBytes != 900*mib {
		t.Fatalf("the sink took %+v, want cc1 (%d) then ld (%d), each at its size before the rise; the log:\n%s", kills, cc1, ld, h.logged())
	}
	ended := h.log.with("ended ")
	if len(ended) != 2 || !strings.HasSuffix(ended[0], `executable="cc1"`) || !strings.HasSuffix(ended[1], `executable="ld"`) {
		t.Fatalf("the kill lines are %q, want cc1's then ld's", ended)
	}
	if unnamed := h.lines("killed unnamed"); len(unnamed) != 0 {
		t.Fatalf("the watch logged %q, want no unnamed kill", unnamed)
	}
}

// A status read under a pid that another process has taken is no evidence of the process the
// watch held under that pid. The kernel kills the tester's python3 and its parent reaps it before
// the next scan; the pid then names a sleep, started since, that has SIGKILL pending. The stat read
// after that status has the sleep's start time, not python3's, so python3 is not named, and the
// sleep, which the watch did not know at that scan, is not either. The sleep exits before the next
// scan, and after the wait the rise has its unnamed line.
func TestGuestNamesNoProcessByTheStatusOfAProcessThatTookItsPid(t *testing.T) {
	const load = 820
	pod := newFakePod(t)
	pod.setSelfScore(0)
	classes := guestTree(pod, 0)
	pod.addGuest(command(load, 8200, "python3", 1400, 1500*mib))
	classes[load] = Class{Role: "tester"}
	cgroup := newFakeCgroup(t, cgroupV2)
	h := newGuestHarness(t, pod.proc, cgroup.dir, classes).run()

	pod.remove(load)
	pod.addGuest(command(load, 9100, "sleep", 1001, mib))
	showKill(pod, load, "sleep", false)
	cgroup.setKills(1)
	h.tick()
	if ended, kills := h.log.with("ended "), h.reported(); len(ended) != 0 || len(kills) != 0 {
		t.Fatalf("the watch wrote %q and reported %+v, want neither: the status under pid %d is the sleep's; the log:\n%s", ended, kills, load, h.logged())
	}
	pod.remove(load)
	h.tickAfter(guestKillWait)
	if got, want := h.lines("killed unnamed"), unnamedLine(1, 1, 700*mib, gib, 16*mib); !slices.Equal(got, []string{want}) {
		t.Fatalf("the unnamed lines are %q, want %q alone; the log:\n%s", got, want, h.logged())
	}
	if ended, kills := h.log.with("ended "), h.reported(); len(ended) != 0 || len(kills) != 0 {
		t.Fatalf("the watch wrote %q and reported %+v, want neither", ended, kills)
	}
}

// A rise that no process shows the kill of names no process, though a command of the tester's
// exits by itself at the same scan, and tells no role: the watch reads for the kill until
// guestKillWait after the rise, then writes one unnamed line with the count, the counter's total
// and the pod's memory at the rise, not at the wait's end.
func TestGuestWritesOneUnnamedLineWithTheRisesMemoryWhenNoProcessShowsTheKill(t *testing.T) {
	const load = 825
	pod := newFakePod(t)
	pod.setSelfScore(0)
	classes := guestTree(pod, 0)
	pod.addGuest(command(load, 8250, "python3", 1400, 1500*mib))
	classes[load] = Class{Role: "tester"}
	cgroup := newFakeCgroup(t, cgroupV2)
	h := newGuestHarness(t, pod.proc, cgroup.dir, classes).run()

	cgroup.setUsage(3900 * mib)
	cgroup.setStat(fakeStat{anon: 3500 * mib, file: 400 * mib, inactiveFile: 120 * mib, shmem: 32 * mib})
	pod.remove(load)
	cgroup.setKills(1)
	h.tick()
	if unnamed := h.lines("killed unnamed"); len(unnamed) != 0 {
		t.Fatalf("the watch logged %q at the rise, want it to read for the kill until the wait ends", unnamed)
	}
	cgroup.setUsage(gib)
	cgroup.setStat(fakeStat{anon: 700 * mib, file: 300 * mib, inactiveFile: 100 * mib, shmem: 16 * mib})
	h.tickAfter(guestKillWait)
	h.tick()
	if got, want := h.lines("killed unnamed"), unnamedLine(1, 1, 3500*mib, 3900*mib, 32*mib); !slices.Equal(got, []string{want}) {
		t.Fatalf("the unnamed lines are %q, want %q alone; the log:\n%s", got, want, h.logged())
	}
	if ended, kills := h.log.with("ended "), h.reported(); len(ended) != 0 || len(kills) != 0 {
		t.Fatalf("the watch wrote %q and reported %+v, want neither", ended, kills)
	}
}

// A command that runs on through a rise, its status with SIGTERM pending and SIGKILL in no pending
// mask, is no kill, though it is the only process of a role the watch knows and the largest: the
// watch names it for no kill, tells no role, and the kill has its unnamed line once the wait ends.
func TestGuestNamesNoRunningProcessWithoutTheKillThoughItIsTheOnlyCandidate(t *testing.T) {
	const load = 826
	pod := newFakePod(t)
	pod.setSelfScore(0)
	pod.addGuest(command(load, 8260, "python3", 1400, 1500*mib))
	classes := map[int]Class{load: {Role: "tester"}}
	cgroup := newFakeCgroup(t, cgroupV2)
	cgroup.setUsage(3900 * mib)
	cgroup.setStat(fakeStat{anon: 3500 * mib, file: 400 * mib, inactiveFile: 120 * mib, shmem: 32 * mib})
	h := newGuestHarness(t, pod.proc, cgroup.dir, classes).run()

	cgroup.setKills(1)
	h.tick()
	h.tickAfter(guestKillWait)
	h.tick()
	if ended, kills := h.log.with("ended "), h.reported(); len(ended) != 0 || len(kills) != 0 {
		t.Fatalf("the watch wrote %q and reported %+v, want neither for a command that runs on; the log:\n%s", ended, kills, h.logged())
	}
	if got, want := h.lines("killed unnamed"), unnamedLine(1, 1, 3500*mib, 3900*mib, 32*mib); !slices.Equal(got, []string{want}) {
		t.Fatalf("the unnamed lines are %q, want %q alone; the log:\n%s", got, want, h.logged())
	}
}

// More processes showing the kernel's kill at one scan than kills pending do not say which of them
// the kernel killed: at a rise of one, the tester's cc1 and ld both show SIGKILL pending, so the
// watch names neither, at that scan or a later one, and tells no role; after the wait the kill has
// its unnamed line.
func TestGuestNamesNoneOfMoreProcessesShowingTheKillThanKillsPending(t *testing.T) {
	const cc1, ld = 815, 816
	pod := newFakePod(t)
	pod.setSelfScore(0)
	classes := guestTree(pod, 0)
	pod.addGuest(command(cc1, 8150, "cc1", 1300, 300*mib), command(ld, 8160, "ld", 1200, 900*mib))
	classes[cc1], classes[ld] = Class{Role: "tester"}, Class{Role: "tester"}
	cgroup := newFakeCgroup(t, cgroupV2)
	h := newGuestHarness(t, pod.proc, cgroup.dir, classes).run()

	reaped(pod, cc1, 8150, "cc1")
	reaped(pod, ld, 8160, "ld")
	cgroup.setKills(1)
	h.tick()
	h.tick()
	h.tickAfter(guestKillWait)
	if got, want := h.lines("killed unnamed"), unnamedLine(1, 1, 700*mib, gib, 16*mib); !slices.Equal(got, []string{want}) {
		t.Fatalf("the unnamed lines are %q, want %q alone; the log:\n%s", got, want, h.logged())
	}
	if ended, kills := h.log.with("ended "), h.reported(); len(ended) != 0 || len(kills) != 0 {
		t.Fatalf("the watch wrote %q and reported %+v, want neither: two processes showed one kill; the log:\n%s", ended, kills, h.logged())
	}
}

// A process named for a kill is evidence for no later one: the tester's python3 shows the kernel's
// kill and is named for the first rise, and is still in /proc, SIGKILL pending, when the counter
// rises again. No other process shows a kill, so the second rise names no process, python3 is not
// named twice, and after the wait the second kill has its unnamed line with the second rise's
// memory.
func TestGuestNamesAKilledProcessOnceThoughItStillShowsTheKillAtTheNextRise(t *testing.T) {
	const python = 818
	pod := newFakePod(t)
	pod.setSelfScore(0)
	classes := guestTree(pod, 0)
	pod.addGuest(command(python, 8180, "python3", 1400, 1500*mib))
	classes[python] = Class{Role: "tester"}
	cgroup := newFakeCgroup(t, cgroupV2)
	h := newGuestHarness(t, pod.proc, cgroup.dir, classes).run()

	reaped(pod, python, 8180, "python3")
	cgroup.setKills(1)
	h.tick()
	if kills := h.reported(); len(kills) != 1 || kills[0].PID != python || kills[0].Sequence != 1 {
		t.Fatalf("the sink took %+v, want the one kill of python3 (%d); the log:\n%s", kills, python, h.logged())
	}
	h.tickAfter(guestKillWait)

	cgroup.setUsage(3900 * mib)
	cgroup.setStat(fakeStat{anon: 3500 * mib, file: 400 * mib, inactiveFile: 120 * mib, shmem: 32 * mib})
	cgroup.setKills(2)
	h.tick()
	h.tickAfter(guestKillWait)
	if kills := h.reported(); len(kills) != 1 {
		t.Fatalf("the sink took %+v, want python3's one kill alone; the log:\n%s", kills, h.logged())
	}
	if ended := h.log.with("ended "); len(ended) != 1 {
		t.Fatalf("the kill lines are %q, want python3's alone", ended)
	}
	if got, want := h.lines("killed unnamed"), unnamedLine(1, 2, 3500*mib, 3900*mib, 32*mib); !slices.Equal(got, []string{want}) {
		t.Fatalf("the unnamed lines are %q, want %q alone; the log:\n%s", got, want, h.logged())
	}
}

// A process the kernel killed that has exited, and that its parent has not reaped yet, is a
// zombie, which the scan leaves out; its status still has the kill's SIGKILL pending for its thread
// group. The evidence pass reads it before the scan's snapshot lets it go, so the zombie is named,
// at its size before the kill.
func TestGuestNamesAZombieThatShowsTheKillThoughTheScanLeavesItOut(t *testing.T) {
	const load = 830
	pod := newFakePod(t)
	pod.setSelfScore(0)
	classes := guestTree(pod, 0)
	killed := command(load, 8300, "python3", 1400, 1500*mib)
	pod.addGuest(killed)
	classes[load] = Class{Role: "tester"}
	cgroup := newFakeCgroup(t, cgroupV2)
	h := newGuestHarness(t, pod.proc, cgroup.dir, classes).run()

	killed.zombie, killed.resident = true, 0
	pod.add(killed.fakeProcess)
	showKill(pod, load, "python3", true)
	if table, err := scan(pod.proc); err != nil || table.byPID[load] != nil {
		t.Fatalf("the scan reads the zombie %d (%v), want it left out", load, err)
	}
	cgroup.setKills(1)
	h.tick()
	if kills := h.reported(); len(kills) != 1 || kills[0].PID != load || kills[0].Role != "tester" ||
		kills[0].Cause != shimwire.MemoryKillCauseKernel || kills[0].SizeBytes != 1500*mib {
		t.Fatalf("the sink took %+v, want the zombie %d of the tester's at its %d bytes before the kill; the log:\n%s", kills, load, 1500*mib, h.logged())
	}
}

// A process shows the kernel's kill by SIGKILL's bit, 0x100, in either pending mask of its status:
// SigPnd, its thread's, or ShdPnd, its thread group's. Other signals pending, SIGKILL's bit in the
// masks of signals blocked, ignored or caught, a pending mask that does not parse, and a status
// with no pending mask show nothing.
func TestGuestReadsTheKillInEitherPendingMaskOfAStatus(t *testing.T) {
	const head = "Name:\tpython3\nState:\tR (running)\nTgid:\t850\nPid:\t850\nPPid:\t108\nThreads:\t1\nSigQ:\t1/63451\n"
	const tail = "SigBlk:\t0000000000000000\nSigIgn:\t0000000001001000\nSigCgt:\t0000000180000002\n"
	for _, tc := range []struct {
		name, status string
		want         bool
	}{
		{"SIGKILL pending for the thread group alone, as a zombie's", head + "SigPnd:\t0000000000000000\nShdPnd:\t0000000000000100\n" + tail, true},
		{"SIGKILL pending for the thread alone", head + "SigPnd:\t0000000000000100\nShdPnd:\t0000000000000000\n" + tail, true},
		{"SIGKILL among other signals pending", head + "SigPnd:\t0000000000000000\nShdPnd:\t0000000000004102\n" + tail, true},
		{"SIGTERM and SIGINT pending, not SIGKILL", head + "SigPnd:\t0000000000004002\nShdPnd:\t0000000000004000\n" + tail, false},
		{"SIGKILL's bit in the masks of signals blocked, ignored and caught alone",
			head + "SigPnd:\t0000000000000000\nShdPnd:\t0000000000000000\nSigBlk:\t0000000000000100\nSigIgn:\t0000000000000100\nSigCgt:\t0000000000000100\n", false},
		{"pending masks that do not parse", head + "SigPnd:\t00000000000001zz\nShdPnd:\t-\n" + tail, false},
		{"no pending mask", head + tail, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sigkillPending([]byte(tc.status)); got != tc.want {
				t.Fatalf("sigkillPending of\n%sis %v, want %v", tc.status, got, tc.want)
			}
		})
	}
}

// A process of no role that the kernel killed has the guard's line, naming role none, and reaches
// no sink; it counts in the sequence, as the guard's kills do, so the next kill, of the tester's,
// is the second.
func TestGuestTellsNoSinkOfAKillOfAProcessOfNoRole(t *testing.T) {
	const outside, load = 840, 841
	pod := newFakePod(t)
	pod.setSelfScore(0)
	classes := guestTree(pod, 0)
	pod.addGuest(
		guestProcess{fakeProcess: fakeProcess{pid: outside, ppid: guestOutsider, args: []string{"python3", "big.py"}, resident: 700 * mib}, score: 1500},
		command(load, 8410, "python3", 1400, 1500*mib),
	)
	classes[load] = Class{Role: "tester"}
	cgroup := newFakeCgroup(t, cgroupV2)
	h := newGuestHarness(t, pod.proc, cgroup.dir, classes).run()

	showKill(pod, outside, "python3", false)
	cgroup.setKills(1)
	h.tick()
	if ended := h.log.with("ended "); len(ended) != 1 || !strings.HasPrefix(ended[0], fmt.Sprintf("launcher: memory guard: ended pid=%d role=none ", outside)) {
		t.Fatalf("the kill lines are %q, want the one of pid %d in role none; the log:\n%s", ended, outside, h.logged())
	}
	if kills := h.reported(); len(kills) != 0 {
		t.Fatalf("the sink took %+v, want nothing for a process of no role", kills)
	}

	reaped(pod, load, 8410, "python3")
	cgroup.setKills(2)
	h.tick()
	if kills := h.reported(); len(kills) != 1 || kills[0].PID != load || kills[0].Sequence != 2 || kills[0].Role != "tester" {
		t.Fatalf("the sink took %+v, want the tester's %d as the second kill", kills, load)
	}
}

// The kernel raises the counter just before it sends SIGKILL, so the scan that sees a rise may find
// no process showing the kill yet, and the watch reads for it at the scans after. At the next one
// the tester's python3 shows the kill, its memory freed, while the tester's rg exits by itself.
// python3 is named at its size at the scan before, and rg, which showed no kill, is named for none.
func TestGuestNamesAProcessThatShowsTheKillAfterTheRiseAtItsSizeBefore(t *testing.T) {
	const python, rg = 850, 851
	pod := newFakePod(t)
	pod.setSelfScore(0)
	classes := guestTree(pod, 0)
	pod.addGuest(command(python, 8500, "python3", 1400, 1500*mib), command(rg, 8510, "rg", 1340, 50*mib))
	classes[python], classes[rg] = Class{Role: "tester"}, Class{Role: "tester"}
	cgroup := newFakeCgroup(t, cgroupV2)
	h := newGuestHarness(t, pod.proc, cgroup.dir, classes).run()

	cgroup.setKills(1)
	h.tick()
	if ended, kills := h.log.with("ended "), h.reported(); len(ended) != 0 || len(kills) != 0 {
		t.Fatalf("the watch wrote %q and reported %+v before any process showed the kill, want neither; the log:\n%s", ended, kills, h.logged())
	}
	reaped(pod, python, 8500, "python3")
	pod.remove(rg)
	h.tick()
	kills := h.reported()
	if len(kills) != 1 || kills[0].PID != python || kills[0].Executable != "python3" || kills[0].SizeBytes != 1500*mib {
		t.Fatalf("the sink took %+v, want the one kill of python3 (%d) at its %d bytes before the kill; the log:\n%s", kills, python, 1500*mib, h.logged())
	}
	if ended := h.log.with("ended "); len(ended) != 1 || !strings.HasSuffix(ended[0], `executable="python3"`) {
		t.Fatalf("the kill lines are %q, want the one of python3", ended)
	}
	h.tickAfter(guestKillWait)
	if unnamed := h.lines("killed unnamed"); len(unnamed) != 0 {
		t.Fatalf("the watch logged %q, want no unnamed kill", unnamed)
	}
}

// A command that starts during the wait and exits by itself is no kill. The kernel kills the
// tester's node, and the scan that sees the rise finds no process showing the kill yet, since the
// kernel raises the counter just before it sends SIGKILL. The reviewer's rg starts, is raised and
// exits by itself. Where node then shows the kill, it is named and rg is not; where no process
// shows it, node's parent having reaped it before the next scan, the rise has its one unnamed line
// after the wait, and rg is not named in node's place.
func TestGuestNamesNoProcessThatStartedAfterTheRise(t *testing.T) {
	const killed, rg = 860, 861
	for _, tc := range []struct {
		name  string
		shows bool
	}{
		{"the killed process shows the kill after the late one exits", true},
		{"no process shows the kill", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := newFakePod(t)
			pod.setSelfScore(0)
			classes := guestTree(pod, 0)
			pod.addGuest(command(killed, 8600, "node", 1400, 2*gib))
			classes[killed], classes[rg] = Class{Role: "tester"}, Class{Role: "reviewer"}
			cgroup := newFakeCgroup(t, cgroupV2)
			h := newGuestHarness(t, pod.proc, cgroup.dir, classes).run()

			// The kernel kills the tester's node: the counter rises before node shows the kill.
			cgroup.setKills(1)
			h.tick()
			// The reviewer's rg starts during the wait, is raised and exits by itself.
			pod.addGuest(guestProcess{fakeProcess: fakeProcess{pid: rg, ppid: 1, start: 8610, args: []string{"rg"}, resident: 50 * mib}, score: 1500})
			h.tick()
			pod.remove(rg)
			if !tc.shows {
				// node's parent reaps it before the next scan.
				pod.remove(killed)
				h.tickAfter(guestKillWait)
				if got, want := h.lines("killed unnamed"), unnamedLine(1, 1, 700*mib, gib, 16*mib); !slices.Equal(got, []string{want}) {
					t.Fatalf("the unnamed lines are %q, want %q alone; the log:\n%s", got, want, h.logged())
				}
				if ended, kills := h.log.with("ended "), h.reported(); len(ended) != 0 || len(kills) != 0 {
					t.Fatalf("the watch wrote %q and reported %+v, want neither: rg exited by itself; the log:\n%s", ended, kills, h.logged())
				}
				return
			}
			h.tick()
			if ended, kills := h.log.with("ended "), h.reported(); len(ended) != 0 || len(kills) != 0 {
				t.Fatalf("the watch wrote %q and reported %+v once rg exited, want neither while no process shows the kill; the log:\n%s", ended, kills, h.logged())
			}
			reaped(pod, killed, 8600, "node")
			h.tick()
			kills := h.reported()
			if len(kills) != 1 || kills[0].PID != killed || kills[0].Executable != "node" || kills[0].Role != "tester" ||
				kills[0].SizeBytes != 2*gib || kills[0].Cause != shimwire.MemoryKillCauseKernel || kills[0].Sequence != 1 {
				t.Fatalf("the sink took %+v, want the one kill of the tester's node (%d); the log:\n%s", kills, killed, h.logged())
			}
			ended := h.log.with("ended ")
			if len(ended) != 1 || !strings.HasPrefix(ended[0], fmt.Sprintf("launcher: memory guard: ended pid=%d role=tester ", killed)) ||
				!strings.HasSuffix(ended[0], `executable="node"`) {
				t.Fatalf("the kill lines are %q, want the one of node; the log:\n%s", ended, h.logged())
			}
			h.tickAfter(guestKillWait)
			if unnamed := h.lines("killed unnamed"); len(unnamed) != 0 {
				t.Fatalf("the watch logged %q, want no unnamed kill", unnamed)
			}
		})
	}
}

// Two rises inside one wait are two kills, each of a process the kernel picked by what that
// process held when it was picked. The first kills the tester's node, which stays in /proc while
// it frees its memory. The reviewer's rg starts after that rise. The tester's python3 then grows
// past every other command and the second rise kills it, and it stays in /proc too. node leaves
// and rg exits by itself. rg never shows the kernel's kill (SIGKILL pending in its status), so it is
// the victim of neither: the watch names node and python3, each to the tester, and tells the
// reviewer nothing.
func TestGuestNamesNoProcessThatStartedAfterTheFirstOfTwoRises(t *testing.T) {
	const node, python, rg = 870, 871, 872
	pod := newFakePod(t)
	pod.setSelfScore(0)
	classes := guestTree(pod, 0)
	pod.addGuest(command(node, 8700, "node", 1400, 2*gib), command(python, 8710, "python3", 1340, 100*mib))
	classes[node], classes[python], classes[rg] = Class{Role: "tester"}, Class{Role: "tester"}, Class{Role: "reviewer"}
	cgroup := newFakeCgroup(t, cgroupV2)
	h := newGuestHarness(t, pod.proc, cgroup.dir, classes).run()

	// The kernel kills node, which stays in /proc.
	reaped(pod, node, 8700, "node")
	cgroup.setKills(1)
	h.tick()
	// rg starts during the wait, and python3 grows to 3 GiB.
	pod.addGuest(guestProcess{fakeProcess: fakeProcess{pid: rg, ppid: 1, start: 8720, args: []string{"rg"}, resident: 200 * mib}, score: 1360})
	grown := command(python, 8710, "python3", 1700, 3*gib)
	grown.adj = raisedScore
	pod.addGuest(grown)
	h.tick()
	// The kernel kills python3, which stays in /proc.
	reaped(pod, python, 8710, "python3")
	cgroup.setKills(2)
	h.tick()
	// node leaves, and rg exits by itself.
	pod.remove(node)
	pod.remove(rg)
	h.tick()
	for _, kill := range h.reported() {
		if kill.PID == rg {
			t.Fatalf("the sink took %+v: the watch named rg (%d), which started after the first rise and held less than python3 at the second, and told the reviewer; the log:\n%s",
				kill, rg, h.logged())
		}
	}
	pod.remove(python)
	h.tick()
	h.tickAfter(guestKillWait)
	kills := h.reported()
	if len(kills) != 2 || kills[0].PID != node || kills[0].Role != "tester" || kills[1].PID != python || kills[1].Role != "tester" {
		t.Fatalf("the sink took %+v, want node (%d) then python3 (%d), each the tester's; the log:\n%s", kills, node, python, h.logged())
	}
	if unnamed := h.lines("killed unnamed"); len(unnamed) != 0 {
		t.Fatalf("the watch logged %q, want no unnamed kill", unnamed)
	}
}

// Three rises inside one wait are three kills, and each is named on the kill its process shows.
// The tester's node, python3 and cc1 run before the burst. The first rise kills node, which stays
// in /proc. The reviewer's rg starts and python3 grows past every other command; the second rise
// kills python3, which stays in /proc too. The architect's make starts and cc1 grows past every
// other command; the third rise kills cc1, which stays as well. node leaves, and rg and make exit
// by themselves. Neither rg nor make shows the kernel's kill, so neither is the victim of any
// kill, though each read above python3 and cc1 before the first rise: the watch names node,
// python3 and cc1, each to the tester, and tells the reviewer and the architect nothing.
func TestGuestNamesNoProcessThatStartedAfterTheFirstOfThreeRises(t *testing.T) {
	const node, python, cc1, rg, mk = 880, 881, 882, 883, 884
	pod := newFakePod(t)
	pod.setSelfScore(0)
	classes := guestTree(pod, 0)
	pod.addGuest(command(node, 8800, "node", 1400, 2*gib), command(python, 8810, "python3", 1340, 100*mib), command(cc1, 8820, "cc1", 1330, 80*mib))
	classes[node], classes[python], classes[cc1] = Class{Role: "tester"}, Class{Role: "tester"}, Class{Role: "tester"}
	classes[rg], classes[mk] = Class{Role: "reviewer"}, Class{Role: "architect"}
	cgroup := newFakeCgroup(t, cgroupV2)
	h := newGuestHarness(t, pod.proc, cgroup.dir, classes).run()

	// The kernel kills node, which stays in /proc.
	reaped(pod, node, 8800, "node")
	cgroup.setKills(1)
	h.tick()
	// rg starts during the wait, and python3 grows to 3 GiB.
	pod.addGuest(guestProcess{fakeProcess: fakeProcess{pid: rg, ppid: 1, start: 8830, args: []string{"rg"}, resident: 200 * mib}, score: 1360})
	grownPython := command(python, 8810, "python3", 1700, 3*gib)
	grownPython.adj = raisedScore
	pod.addGuest(grownPython)
	h.tick()
	// The kernel kills python3, which stays in /proc.
	reaped(pod, python, 8810, "python3")
	cgroup.setKills(2)
	h.tick()
	// make starts during the wait, and cc1 grows to 3 GiB.
	pod.addGuest(guestProcess{fakeProcess: fakeProcess{pid: mk, ppid: 1, start: 8840, args: []string{"make"}, resident: 150 * mib}, score: 1350})
	grownCC1 := command(cc1, 8820, "cc1", 1700, 3*gib)
	grownCC1.adj = raisedScore
	pod.addGuest(grownCC1)
	h.tick()
	// The kernel kills cc1, which stays in /proc.
	reaped(pod, cc1, 8820, "cc1")
	cgroup.setKills(3)
	h.tick()
	// node leaves, and rg and make exit by themselves.
	pod.remove(node)
	pod.remove(rg)
	pod.remove(mk)
	h.tick()
	for _, kill := range h.reported() {
		if kill.PID == rg || kill.PID == mk {
			t.Fatalf("the sink took %+v: the watch named %s (%d), which started after the first rise and held less than the process the kernel killed at its own, and told the %s; the log:\n%s",
				kill, kill.Executable, kill.PID, kill.Role, h.logged())
		}
	}
	pod.remove(python)
	pod.remove(cc1)
	h.tick()
	h.tickAfter(guestKillWait)
	kills := h.reported()
	if len(kills) != 3 || kills[0].PID != node || kills[0].Role != "tester" || kills[1].PID != python || kills[1].Role != "tester" ||
		kills[2].PID != cc1 || kills[2].Role != "tester" {
		t.Fatalf("the sink took %+v, want node (%d), python3 (%d) then cc1 (%d), each the tester's; the log:\n%s", kills, node, python, cc1, h.logged())
	}
	if unnamed := h.lines("killed unnamed"); len(unnamed) != 0 {
		t.Fatalf("the watch logged %q, want no unnamed kill", unnamed)
	}
}

// reaped puts a process of the tester's command (command) in /proc as the kernel leaves a process
// it killed while its oom_reaper frees the process's memory: still there, its resident size and
// oom_score at nothing, its oom_score_adj the raise the watch gave it, and the SIGKILL the kernel
// sent it pending in its status.
func reaped(pod *fakePod, pid int, start uint64, name string) {
	pod.t.Helper()
	process := command(pid, start, name, 0, 0)
	process.adj = raisedScore
	pod.addGuest(process)
	pod.writeWhole(filepath.Join(pod.proc, strconv.Itoa(pid), "status"), fmt.Sprintf(
		"Name:\t%s\nState:\tR (running)\nTgid:\t%d\nPid:\t%d\nPPid:\t%d\nThreads:\t1\nSigQ:\t1/63451\nSigPnd:\t0000000000000100\nShdPnd:\t0000000000000100\n",
		name, pid, pid, guestBash))
}

// A command that exits by itself is no kill, though the watch read it above the process the
// kernel killed. The reviewer's cargo and the tester's python3 run before the rise, cargo read
// above python3. python3 then grows past cargo before the next scan, the kernel kills it, and it
// stays in /proc while its memory is freed. cargo exits by itself during the wait. The watch names
// python3, to the tester, and tells the reviewer nothing.
func TestGuestNamesNoProcessThatExitsByItselfWhileTheKilledOneIsReaped(t *testing.T) {
	const cargo, python = 910, 911
	pod := newFakePod(t)
	pod.setSelfScore(0)
	classes := guestTree(pod, 0)
	pod.addGuest(command(cargo, 9100, "cargo", 1380, 400*mib), command(python, 9110, "python3", 1340, 100*mib))
	classes[cargo], classes[python] = Class{Role: "reviewer"}, Class{Role: "tester"}
	cgroup := newFakeCgroup(t, cgroupV2)
	h := newGuestHarness(t, pod.proc, cgroup.dir, classes).run()

	// The kernel kills python3, which stays in /proc while its memory is freed.
	reaped(pod, python, 9110, "python3")
	cgroup.setKills(1)
	h.tick()
	// cargo exits by itself.
	pod.remove(cargo)
	h.tick()
	for _, kill := range h.reported() {
		if kill.PID == cargo {
			t.Fatalf("the sink took %+v: the watch named cargo (%d), which exited by itself, and told the reviewer; the log:\n%s", kill, cargo, h.logged())
		}
	}
	pod.remove(python)
	h.tick()
	h.tickAfter(guestKillWait)
	kills := h.reported()
	if len(kills) != 1 || kills[0].PID != python || kills[0].Role != "tester" {
		t.Fatalf("the sink took %+v, want the one kill of python3 (%d), the tester's; the log:\n%s", kills, python, h.logged())
	}
	if unnamed := h.lines("killed unnamed"); len(unnamed) != 0 {
		t.Fatalf("the watch logged %q, want no unnamed kill", unnamed)
	}
}

// The same inside a burst, where a process the second rise killed was read before the first: the
// tester's node, the reviewer's cargo and the tester's python3 run before the burst, cargo read
// above python3. The first rise kills node, which stays in /proc. python3 grows past every other
// command and the second rise kills it, and it stays in /proc too. node leaves, and cargo exits by
// itself. The watch names node and python3, each to the tester, and tells the reviewer nothing.
func TestGuestNamesNoProcessThatExitsByItselfDuringABurst(t *testing.T) {
	const node, cargo, python = 900, 901, 902
	pod := newFakePod(t)
	pod.setSelfScore(0)
	classes := guestTree(pod, 0)
	pod.addGuest(command(node, 9000, "node", 1400, 2*gib), command(cargo, 9010, "cargo", 1380, 400*mib), command(python, 9020, "python3", 1340, 100*mib))
	classes[node], classes[cargo], classes[python] = Class{Role: "tester"}, Class{Role: "reviewer"}, Class{Role: "tester"}
	cgroup := newFakeCgroup(t, cgroupV2)
	h := newGuestHarness(t, pod.proc, cgroup.dir, classes).run()

	// The kernel kills node, which stays in /proc.
	reaped(pod, node, 9000, "node")
	cgroup.setKills(1)
	h.tick()
	// python3 grows to 3 GiB.
	grown := command(python, 9020, "python3", 1700, 3*gib)
	grown.adj = raisedScore
	pod.addGuest(grown)
	h.tick()
	// The kernel kills python3, which stays in /proc.
	reaped(pod, python, 9020, "python3")
	cgroup.setKills(2)
	h.tick()
	// node leaves, and cargo exits by itself.
	pod.remove(node)
	pod.remove(cargo)
	h.tick()
	for _, kill := range h.reported() {
		if kill.PID == cargo {
			t.Fatalf("the sink took %+v: the watch named cargo (%d), which exited by itself, and told the reviewer; the log:\n%s", kill, cargo, h.logged())
		}
	}
	pod.remove(python)
	h.tick()
	h.tickAfter(guestKillWait)
	kills := h.reported()
	if len(kills) != 2 || kills[0].PID != node || kills[0].Role != "tester" || kills[1].PID != python || kills[1].Role != "tester" {
		t.Fatalf("the sink took %+v, want node (%d) then python3 (%d), each the tester's; the log:\n%s", kills, node, python, h.logged())
	}
	if unnamed := h.lines("killed unnamed"); len(unnamed) != 0 {
		t.Fatalf("the watch logged %q, want no unnamed kill", unnamed)
	}
}

// A process of a role that the classifier protects, one of those that run the role's session
// (the agent here, which Oh My Pi's in-process shell can grow past every command), that the kernel
// killed has the guard's own line as every kill has, and one Kill of its role with the kernel's
// protected cause, so its role is told what that end does to it. A protected process of no role
// (the test database's server) has the line alone, as every process of no role has, and counts in
// the sequence.
func TestGuestReportsAKillOfARolesProtectedProcessWithTheProtectedCause(t *testing.T) {
	pod := newFakePod(t)
	pod.setSelfScore(0)
	classes := guestTree(pod, 0)
	cgroup := newFakeCgroup(t, cgroupV2)
	h := newGuestHarness(t, pod.proc, cgroup.dir, classes).run()

	cgroup.setUsage(3900 * mib)
	cgroup.setStat(fakeStat{anon: 3500 * mib, file: 400 * mib, inactiveFile: 120 * mib, shmem: 32 * mib})
	showKill(pod, guestAgent, "omp", false)
	cgroup.setKills(1)
	h.tick()
	at := h.clock.Now()

	want := fmt.Sprintf(`launcher: memory guard: ended pid=%d role=tester size=%d reading=%d counter=%d shmem=%d limit=%d executable="omp"`,
		guestAgent, 2*gib, 3500*mib, 3900*mib, 32*mib, podLimit)
	if got := h.log.with("ended "); !slices.Equal(got, []string{want}) {
		t.Fatalf("the kill lines are %q, want %q; the log:\n%s", got, want, h.logged())
	}
	wantKill := Kill{
		LauncherMemoryKill: shimwire.LauncherMemoryKill{
			Guard: h.g.ID(), Sequence: 1, PID: guestAgent, Executable: "omp", SizeBytes: 2 * gib,
			ReadingBytes: 3500 * mib, CounterBytes: 3900 * mib, LimitBytes: podLimit, At: at, Cause: shimwire.MemoryKillCauseKernelProtected,
		},
		Role: "tester",
	}
	kills := h.reported()
	if len(kills) != 1 || kills[0] != wantKill {
		t.Fatalf("the sink took %+v, want the one kill %+v", kills, wantKill)
	}
	if err := kills[0].Validate(); err != nil {
		t.Fatal(err)
	}

	showKill(pod, guestTestDB, "postgres", false)
	cgroup.setKills(2)
	h.tick()
	ended := h.log.with("ended ")
	if len(ended) != 2 || !strings.HasPrefix(ended[1], fmt.Sprintf("launcher: memory guard: ended pid=%d role=none ", guestTestDB)) ||
		!strings.HasSuffix(ended[1], `executable="postgres"`) {
		t.Fatalf("the kill lines are %q, want the agent's, then the test database's in role none; the log:\n%s", ended, h.logged())
	}
	if kills := h.reported(); len(kills) != 1 {
		t.Fatalf("the sink took %+v, want nothing more for a protected process of no role", kills)
	}
}

// Each hour from its start the watch writes one line: the hour's last usage and working set, their
// highs and when they were read, the last memory.stat figures and limit, the hour's kills, unnamed
// kills, score writes and failed score writes. The working set is the usage less the inactive file
// pages, so its high is not the usage's. A figure memory.stat does not hold is written `-`.
func TestGuestHourlyLine(t *testing.T) {
	for _, version := range []string{cgroupV1, cgroupV2} {
		t.Run(version, func(t *testing.T) {
			pod := newFakePod(t)
			pod.setSelfScore(0)
			classes := guestTree(pod, 0)
			cgroup := newFakeCgroup(t, version)
			cgroup.setStat(fakeStat{anon: 700 * mib, file: 300 * mib, inactiveFile: 100 * mib, noShmem: true})
			h := newGuestHarness(t, pod.proc, cgroup.dir, classes).run()

			// The usage's high, much of it inactive file pages.
			cgroup.setUsage(3 * gib)
			cgroup.setStat(fakeStat{anon: 1400 * mib, file: 1600 * mib, inactiveFile: 1536 * mib, noShmem: true})
			h.tickAfter(10 * time.Minute)
			currentHighAt := h.clock.Now()
			// A kill no process is named for.
			cgroup.setKills(1)
			h.tick()
			h.tickAfter(guestKillWait)

			// Less usage, more of it working set: the working set's high.
			cgroup.setUsage(2560 * mib)
			cgroup.setStat(fakeStat{anon: 2300 * mib, file: 200 * mib, inactiveFile: 100 * mib, noShmem: true})
			h.tickAfter(10 * time.Minute)
			workingSetHighAt := h.clock.Now()

			// The hour's last readings.
			cgroup.setUsage(1200 * mib)
			cgroup.setStat(fakeStat{anon: 1000 * mib, file: 150 * mib, inactiveFile: 90 * mib, noShmem: true})
			h.tickAfter(10 * time.Minute)
			if hours := h.lines("hour "); len(hours) != 0 {
				t.Fatalf("the watch wrote %q before its first hour passed", hours)
			}

			// The next hour's first scan closes the hour, and its own reading is the next hour's.
			cgroup.setUsage(4 * gib)
			h.tickAfter(45 * time.Minute)
			want := fmt.Sprintf("launcher: guest memory: hour start=2026-11-14T03:00:00Z end=2026-11-14T04:00:00Z "+
				"current=%d current_high=%d current_high_at=%s working_set=%d working_set_high=%d working_set_high_at=%s "+
				"anon=%d file=%d inactive_file=%d shmem=- limit=%d oom_kills=1 unnamed=1 raised=3 unraised=0",
				1200*mib, 3*gib, currentHighAt.Format(time.RFC3339), 1110*mib, 2460*mib, workingSetHighAt.Format(time.RFC3339),
				1000*mib, 150*mib, 90*mib, podLimit)
			if got := h.lines("hour "); !slices.Equal(got, []string{want}) {
				t.Fatalf("the hourly lines are\n%s\nwant\n%s", strings.Join(got, "\n"), want)
			}
		})
	}
}

// With no kill counter it can read, the watch writes one line saying why, once, and names nothing;
// it still raises the role's commands and writes its hourly line, with the counts it cannot keep
// written `-`.
func TestGuestWithNoReadableKillCounterStillRaisesScoresAndWritesTheHour(t *testing.T) {
	for _, tc := range []struct {
		name    string
		version string
		spoil   func(c *fakeCgroup)
		reason  func(c *fakeCgroup) string
	}{
		{"a v1 kernel whose oom_control has no oom_kill", cgroupV1,
			func(c *fakeCgroup) { c.write("memory.oom_control", "oom_kill_disable 0\nunder_oom 0\n") },
			func(c *fakeCgroup) string { return c.path("memory.oom_control") + " has no oom_kill line" }},
		{"a v2 cgroup with no memory.events", cgroupV2,
			func(c *fakeCgroup) {
				if err := os.Remove(c.path("memory.events")); err != nil {
					c.t.Fatal(err)
				}
			},
			func(c *fakeCgroup) string { return "open " + c.path("memory.events") + ": no such file or directory" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := newFakePod(t)
			pod.setSelfScore(0)
			classes := guestTree(pod, 0)
			cgroup := newFakeCgroup(t, tc.version)
			tc.spoil(cgroup)
			h := newGuestHarness(t, pod.proc, cgroup.dir, classes).run()
			pod.remove(guestBash)
			h.tick()
			h.tickAfter(time.Hour)
			want := "launcher: guest memory: kills unnamed reason=" + tc.reason(cgroup)
			if got := h.lines("kills unnamed"); !slices.Equal(got, []string{want}) {
				t.Fatalf("the watch logged %q, want %q once", got, want)
			}
			for _, pid := range []int{guestGopls, guestEval} {
				if got := pod.score(pid); got != raisedScore {
					t.Errorf("pid %d reads oom_score_adj %d, want %d", pid, got, raisedScore)
				}
			}
			hours := h.lines("hour ")
			if len(hours) != 1 || !strings.HasSuffix(hours[0], " oom_kills=- unnamed=- raised=3 unraised=0") ||
				!strings.Contains(hours[0], fmt.Sprintf(" current=%d ", gib)) {
				t.Fatalf("the hourly lines are %q, want one with the usage and no kill counts", hours)
			}
			if ended := h.log.with("ended "); len(ended) != 0 {
				t.Fatalf("the watch named %q with no kill counter", ended)
			}
		})
	}
}

// A score write that fails, as one to a process that is not dumpable does, is logged once with the
// process's pid and start time, counted in the hour, and not tried again at each scan; the other
// processes are raised as ever.
func TestGuestLogsAFailedScoreWriteOnceAndCountsIt(t *testing.T) {
	pod := newFakePod(t)
	pod.setSelfScore(0)
	classes := guestTree(pod, 0)
	h := newGuestHarness(t, pod.proc, newFakeCgroup(t, cgroupV2).dir, classes)
	var mu sync.Mutex
	tries := map[int]int{}
	write := h.g.writeScore
	h.g.writeScore = func(path string, value []byte) error {
		pid, _ := strconv.Atoi(filepath.Base(filepath.Dir(path)))
		mu.Lock()
		tries[pid]++
		mu.Unlock()
		if pid == guestEval {
			return &fs.PathError{Op: "open", Path: path, Err: syscall.EACCES}
		}
		return write(path, value)
	}
	h.run()
	h.tick()
	h.tick()
	h.tickAfter(time.Hour)

	want := fmt.Sprintf("launcher: guest memory: score failed pid=%d start_time=%d score=1000 reason=open %s: permission denied",
		guestEval, guestEval*10, filepath.Join(pod.proc, strconv.Itoa(guestEval), "oom_score_adj"))
	if got := h.lines("score failed"); !slices.Equal(got, []string{want}) {
		t.Fatalf("the failed score lines are %q, want %q once", got, want)
	}
	mu.Lock()
	evalTries := tries[guestEval]
	mu.Unlock()
	if evalTries != 1 {
		t.Fatalf("the watch tried the eval kernel's score %d times, want once", evalTries)
	}
	for _, pid := range []int{guestBash, guestGopls} {
		if got := pod.score(pid); got != raisedScore {
			t.Errorf("pid %d reads oom_score_adj %d, want %d", pid, got, raisedScore)
		}
	}
	if hours := h.lines("hour "); len(hours) != 1 || !strings.HasSuffix(hours[0], " raised=2 unraised=1") {
		t.Fatalf("the hourly lines are %q, want one counting 2 writes and 1 failed one", hours)
	}
}

// With no cgroup named, the watch finds its own from the cgroup file and the mount table of the
// process that reads /proc: v2's single hierarchy, at the path under the mount's root, or at the
// mount point itself in a cgroup namespace, a path with an escaped space undone; and v1's memory
// controller where a hybrid host mounts both. Its start line names the version, the directory,
// v2's memory.oom.group and the starting score, and with a counter it reads, no other line.
func TestGuestFindsItsCgroupByItsOwnCgroupFileAndMountTable(t *testing.T) {
	for _, tc := range []struct {
		name    string
		version string
		// layout answers the membership and the mount table for a root under which the mounts lie,
		// and the cgroup's directory.
		layout   func(root string) (membership, mounts, dir string)
		oomGroup string
	}{
		{"v2 at the path under the mount's root", cgroupV2, func(root string) (string, string, string) {
			point := filepath.Join(root, "sys", "fs", "cgroup")
			return "0::/kubepods/burstable/pod1/c1\n",
				"22 1 8:1 / / rw,relatime shared:1 - ext4 /dev/sda1 rw\n" +
					"30 22 0:26 / " + point + " rw,nosuid,nodev,noexec,relatime shared:4 - cgroup2 cgroup2 rw,nsdelegate,memory_recursiveprot\n",
				filepath.Join(point, "kubepods", "burstable", "pod1", "c1")
		}, "1"},
		{"v2 in a cgroup namespace, at a mount point with a space", cgroupV2, func(root string) (string, string, string) {
			point := filepath.Join(root, "cgroup mount")
			return "0::/\n",
				"22 1 8:1 / / rw,relatime - ext4 /dev/sda1 rw\n" +
					"31 22 0:26 / " + strings.ReplaceAll(point, " ", `\040`) + " ro,nosuid,nodev,noexec,relatime - cgroup2 cgroup rw\n",
				point
		}, "1"},
		{"v1 beside an empty v2 on a hybrid host", cgroupV1, func(root string) (string, string, string) {
			point := filepath.Join(root, "sys", "fs", "cgroup", "memory")
			return "12:memory:/kubepods/pod1/c1\n11:cpu,cpuacct:/kubepods/pod1/c1\n0::/kubepods/pod1/c1\n",
				"22 1 8:1 / / rw,relatime - ext4 /dev/sda1 rw\n" +
					"33 22 0:28 / " + filepath.Join(root, "sys", "fs", "cgroup", "unified") + " rw,nosuid - cgroup2 cgroup2 rw\n" +
					"35 22 0:30 /kubepods/pod1/c1 " + point + " rw,nosuid,nodev,noexec,relatime shared:9 - cgroup cgroup rw,memory\n",
				point
		}, "-"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := newFakePod(t)
			pod.setSelfScore(0)
			membership, mounts, dir := tc.layout(t.TempDir())
			pod.writeWhole(filepath.Join(pod.proc, "self", "cgroup"), membership)
			pod.writeWhole(filepath.Join(pod.proc, "self", "mountinfo"), mounts)
			cgroup := newFakeCgroupIn(t, tc.version, dir)
			if tc.version == cgroupV2 {
				cgroup.write("memory.oom.group", "1\n")
			}
			h := newGuestHarness(t, pod.proc, "", nil).run()
			want := fmt.Sprintf("launcher: guest memory: start cgroup=%s dir=%s oom_group=%s score=0", tc.version, dir, tc.oomGroup)
			if got := h.log.all(); !slices.Equal(got, []string{want}) {
				t.Fatalf("the watch logged %q, want its start line %q alone", got, want)
			}
		})
	}
}

// With no /proc it can read, the watch writes its start line with no starting score and one off
// line, raises and names nothing, writes each rise of the counter as unnamed at once, and still
// writes its hourly line from the cgroup, the score counts written `-`.
func TestGuestIsOffWithoutAReadableProcAndStillWritesTheHour(t *testing.T) {
	cgroup := newFakeCgroup(t, cgroupV2)
	proc := filepath.Join(t.TempDir(), "no-proc")
	h := newGuestHarness(t, proc, cgroup.dir, nil).run()
	scorePath := filepath.Join(proc, "self", "oom_score_adj")
	want := []string{
		fmt.Sprintf("launcher: guest memory: start cgroup=v2 dir=%s oom_group=0 score=-", cgroup.dir),
		fmt.Sprintf("launcher: guest memory: off reason=no readable starting score in %s: open %s: no such file or directory", scorePath, scorePath),
	}
	if got := h.log.all(); !slices.Equal(got, want) {
		t.Fatalf("the watch logged %q, want %q", got, want)
	}
	cgroup.setKills(1)
	h.tick()
	if got, want := h.lines("killed unnamed"), unnamedLine(1, 1, 700*mib, gib, 16*mib); !slices.Equal(got, []string{want}) {
		t.Fatalf("the unnamed lines are %q, want %q at once, with the rise's memory", got, want)
	}
	h.tickAfter(time.Hour)
	hour := fmt.Sprintf("launcher: guest memory: hour start=2026-11-14T03:00:00Z end=2026-11-14T04:00:00Z "+
		"current=%d current_high=%d current_high_at=2026-11-14T03:00:00Z working_set=%d working_set_high=%d working_set_high_at=2026-11-14T03:00:00Z "+
		"anon=%d file=%d inactive_file=%d shmem=%d limit=%d oom_kills=1 unnamed=1 raised=- unraised=-",
		gib, gib, 924*mib, 924*mib, 700*mib, 300*mib, 100*mib, 16*mib, podLimit)
	if got := h.lines("hour "); !slices.Equal(got, []string{hour}) {
		t.Fatalf("the hourly lines are\n%s\nwant\n%s", strings.Join(got, "\n"), hour)
	}
	if ended, kills := h.log.with("ended "), h.reported(); len(ended) != 0 || len(kills) != 0 {
		t.Fatalf("the watch wrote %q and reported %+v, want neither", ended, kills)
	}
}

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

// guestProcess is a fakeProcess with the files the guest watch reads of it besides the scan's:
// its oom_score_adj and its oom_score.
type guestProcess struct {
	fakeProcess
	adj   int
	score int
}

func (p *fakePod) addGuest(gps ...guestProcess) {
	p.t.Helper()
	for _, gp := range gps {
		p.add(gp.fakeProcess)
		p.setOOMScore(gp.pid, gp.score)
		p.setScore(gp.pid, gp.adj)
	}
}

// setScore writes pid's oom_score_adj.
func (p *fakePod) setScore(pid, adj int) {
	p.t.Helper()
	p.writeWhole(filepath.Join(p.proc, strconv.Itoa(pid), "oom_score_adj"), fmt.Sprintf("%d\n", adj))
}

// setOOMScore writes pid's oom_score, the kernel's badness of it, by which the watch ranks a kill.
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

// The counter rising by one while a command of the tester's has left the scan names that command:
// the guard's own line with its role, its executable and its resident size at the last scan, the
// cgroup's anonymous memory, usage, shared memory and limit at the rise, and one Kill of the
// tester's with the kernel's cause, the watch's id and the first sequence. The command is the
// shell command's child, whose exit goes to the shell and never to the launcher.
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
			pod.remove(load)
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

// A rise of two with three of the tester's commands gone names the two highest by their last
// oom_score, and between two of the same score the larger: the first two kills, in that order.
// The third, gone as well, is named for no kill, and no kill is left unnamed.
func TestGuestNamesTwoKilledProcessesByTheirLastOOMScoreThenTheirSize(t *testing.T) {
	const cc1, ld, as = 810, 811, 812
	pod := newFakePod(t)
	pod.setSelfScore(0)
	classes := guestTree(pod, 0)
	pod.addGuest(
		command(cc1, 8100, "cc1", 1300, 300*mib),
		command(ld, 8110, "ld", 1200, 900*mib),
		command(as, 8120, "as", 1200, 200*mib),
	)
	for _, pid := range []int{cc1, ld, as} {
		classes[pid] = Class{Role: "tester"}
	}
	cgroup := newFakeCgroup(t, cgroupV2)
	h := newGuestHarness(t, pod.proc, cgroup.dir, classes).run()

	for _, pid := range []int{cc1, ld, as} {
		pod.remove(pid)
	}
	cgroup.setKills(2)
	h.tick()
	kills := h.reported()
	if len(kills) != 2 || kills[0].PID != cc1 || kills[0].Sequence != 1 || kills[1].PID != ld || kills[1].Sequence != 2 {
		t.Fatalf("the sink took %+v, want cc1 (%d) then ld (%d); the log:\n%s", kills, cc1, ld, h.logged())
	}
	ended := h.log.with("ended ")
	if len(ended) != 2 || !strings.Contains(ended[0], `executable="cc1"`) || !strings.Contains(ended[1], fmt.Sprintf(`size=%d`, 900*mib)) {
		t.Fatalf("the kill lines are %q, want cc1's then ld's", ended)
	}
	if unnamed := h.lines("killed unnamed"); len(unnamed) != 0 {
		t.Fatalf("the watch logged %q, want no unnamed kill", unnamed)
	}
}

// A process the kernel killed whose pid another process took before the next scan is the gone
// one: the pid with another start time is a process the watch never held before the rise, and the
// line and the report name the killed process's executable and size, not the new one's.
func TestGuestTakesTheProcessWhosePidCameBackWithAnotherStartTimeForTheKilledOne(t *testing.T) {
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
	cgroup.setKills(1)
	h.tick()
	kills := h.reported()
	if len(kills) != 1 || kills[0].PID != load || kills[0].Executable != "python3" || kills[0].SizeBytes != 1500*mib {
		t.Fatalf("the sink took %+v, want the killed python3 of %d bytes under pid %d; the log:\n%s", kills, 1500*mib, load, h.logged())
	}
	if ended := h.log.with("ended "); len(ended) != 1 || !strings.HasSuffix(ended[0], `executable="python3"`) {
		t.Fatalf("the kill lines are %q, want the one of python3", ended)
	}
}

// A rise with no process gone is a kill of a process the watch never held, or of one that has not
// left yet: it waits guestKillWait, then writes one unnamed line with the count and the counter's
// total, names no process and tells no role.
func TestGuestWritesOneUnnamedLineForARiseWithNothingGone(t *testing.T) {
	pod := newFakePod(t)
	pod.setSelfScore(0)
	classes := guestTree(pod, 0)
	cgroup := newFakeCgroup(t, cgroupV2)
	h := newGuestHarness(t, pod.proc, cgroup.dir, classes).run()

	cgroup.setKills(1)
	h.tick()
	if unnamed := h.lines("killed unnamed"); len(unnamed) != 0 {
		t.Fatalf("the watch logged %q at the rise, want it to wait for the killed process to leave", unnamed)
	}
	h.tickAfter(guestKillWait)
	h.tick()
	want := "launcher: guest memory: killed unnamed count=1 oom_kill=1"
	if got := h.lines("killed unnamed"); !slices.Equal(got, []string{want}) {
		t.Fatalf("the unnamed lines are %q, want %q alone; the log:\n%s", got, want, h.logged())
	}
	if ended, kills := h.log.with("ended "), h.reported(); len(ended) != 0 || len(kills) != 0 {
		t.Fatalf("the watch wrote %q and reported %+v, want neither", ended, kills)
	}
}

// A process the kernel killed whose parent has not reaped it yet is a zombie, which the scan does
// not hold: it is the gone one.
func TestGuestCountsAZombieAsGone(t *testing.T) {
	const load = 830
	pod := newFakePod(t)
	pod.setSelfScore(0)
	classes := guestTree(pod, 0)
	killed := command(load, 8300, "python3", 1400, 1500*mib)
	pod.addGuest(killed)
	classes[load] = Class{Role: "tester"}
	cgroup := newFakeCgroup(t, cgroupV2)
	h := newGuestHarness(t, pod.proc, cgroup.dir, classes).run()

	killed.zombie = true
	pod.add(killed.fakeProcess)
	cgroup.setKills(1)
	h.tick()
	if kills := h.reported(); len(kills) != 1 || kills[0].PID != load || kills[0].Role != "tester" || kills[0].Cause != shimwire.MemoryKillCauseKernel {
		t.Fatalf("the sink took %+v, want the zombie %d of the tester's; the log:\n%s", kills, load, h.logged())
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

	pod.remove(outside)
	cgroup.setKills(1)
	h.tick()
	if ended := h.log.with("ended "); len(ended) != 1 || !strings.HasPrefix(ended[0], fmt.Sprintf("launcher: memory guard: ended pid=%d role=none ", outside)) {
		t.Fatalf("the kill lines are %q, want the one of pid %d in role none; the log:\n%s", ended, outside, h.logged())
	}
	if kills := h.reported(); len(kills) != 0 {
		t.Fatalf("the sink took %+v, want nothing for a process of no role", kills)
	}

	pod.remove(load)
	cgroup.setKills(2)
	h.tick()
	if kills := h.reported(); len(kills) != 1 || kills[0].PID != load || kills[0].Sequence != 2 || kills[0].Role != "tester" {
		t.Fatalf("the sink took %+v, want the tester's %d as the second kill", kills, load)
	}
}

// A process the kernel killed is ranked and named by its reads before the rise: the kernel counts
// the kill at its SIGKILL, and its oom_reaper frees the process's memory before the process leaves
// /proc, so a read after the rise finds it at no size and no score. A smaller raised command that
// exits by itself during the wait is not taken for it, and the killed process is named, at its
// size before the rise, once it leaves.
func TestGuestNamesAReapedProcessByItsReadsBeforeTheRise(t *testing.T) {
	const python, rg = 850, 851
	pod := newFakePod(t)
	pod.setSelfScore(0)
	classes := guestTree(pod, 0)
	pod.addGuest(command(python, 8500, "python3", 1400, 1500*mib), command(rg, 8510, "rg", 1340, 50*mib))
	classes[python], classes[rg] = Class{Role: "tester"}, Class{Role: "tester"}
	cgroup := newFakeCgroup(t, cgroupV2)
	h := newGuestHarness(t, pod.proc, cgroup.dir, classes).run()

	reaped := command(python, 8500, "python3", 0, 0)
	reaped.adj = raisedScore
	pod.addGuest(reaped)
	cgroup.setKills(1)
	h.tick()
	pod.remove(rg)
	h.tick()
	if ended, kills := h.log.with("ended "), h.reported(); len(ended) != 0 || len(kills) != 0 {
		t.Fatalf("the watch wrote %q and reported %+v while the killed python3 was still in /proc, want neither; the log:\n%s", ended, kills, h.logged())
	}
	pod.remove(python)
	h.tick()
	kills := h.reported()
	if len(kills) != 1 || kills[0].PID != python || kills[0].Executable != "python3" || kills[0].SizeBytes != 1500*mib {
		t.Fatalf("the sink took %+v, want the one kill of python3 (%d) at its %d bytes before the rise; the log:\n%s", kills, python, 1500*mib, h.logged())
	}
	if ended := h.log.with("ended "); len(ended) != 1 || !strings.HasSuffix(ended[0], `executable="python3"`) {
		t.Fatalf("the kill lines are %q, want the one of python3", ended)
	}
}

// A process the watch first holds after the scan that saw the rise is no kill of that rise, since
// that scan did not read it: another role's command that starts during the wait, is raised so it
// ranks first, and exits by itself neither holds the walk nor is named. The killed process, still
// in /proc at the rise's scan, is named once it leaves; where it does not leave within the wait,
// the rise has its one unnamed line, and the late command is not named in its place.
func TestGuestNamesNoProcessThatStartedAfterTheRise(t *testing.T) {
	const killed, rg = 860, 861
	for _, tc := range []struct {
		name   string
		leaves bool
	}{
		{"the killed process leaves after the late one", true},
		{"only the late process leaves", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := newFakePod(t)
			pod.setSelfScore(0)
			classes := guestTree(pod, 0)
			pod.addGuest(command(killed, 8600, "node", 1400, 2*gib))
			classes[killed], classes[rg] = Class{Role: "tester"}, Class{Role: "reviewer"}
			cgroup := newFakeCgroup(t, cgroupV2)
			h := newGuestHarness(t, pod.proc, cgroup.dir, classes).run()

			// The kernel kills the tester's node, which stays in /proc while it frees its memory.
			cgroup.setKills(1)
			h.tick()
			// The reviewer's rg starts during the wait, is raised and exits by itself.
			pod.addGuest(guestProcess{fakeProcess: fakeProcess{pid: rg, ppid: 1, start: 8610, args: []string{"rg"}, resident: 50 * mib}, score: 1500})
			h.tick()
			pod.remove(rg)
			if !tc.leaves {
				h.tickAfter(guestKillWait)
				want := "launcher: guest memory: killed unnamed count=1 oom_kill=1"
				if got := h.lines("killed unnamed"); !slices.Equal(got, []string{want}) {
					t.Fatalf("the unnamed lines are %q, want %q alone; the log:\n%s", got, want, h.logged())
				}
				if ended, kills := h.log.with("ended "), h.reported(); len(ended) != 0 || len(kills) != 0 {
					t.Fatalf("the watch wrote %q and reported %+v, want neither: rg started after the rise; the log:\n%s", ended, kills, h.logged())
				}
				return
			}
			h.tick()
			if ended, kills := h.log.with("ended "), h.reported(); len(ended) != 0 || len(kills) != 0 {
				t.Fatalf("the watch wrote %q and reported %+v once rg exited, want neither while the killed node is in /proc; the log:\n%s", ended, kills, h.logged())
			}
			pod.remove(killed)
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
			if unnamed := h.lines("killed unnamed"); len(unnamed) != 0 {
				t.Fatalf("the watch logged %q, want no unnamed kill", unnamed)
			}
		})
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
	pod.setOOMScore(guestAgent, 1900)
	pod.setOOMScore(guestTestDB, 1800)
	cgroup := newFakeCgroup(t, cgroupV2)
	h := newGuestHarness(t, pod.proc, cgroup.dir, classes).run()

	cgroup.setUsage(3900 * mib)
	cgroup.setStat(fakeStat{anon: 3500 * mib, file: 400 * mib, inactiveFile: 120 * mib, shmem: 32 * mib})
	pod.remove(guestAgent)
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

	pod.remove(guestTestDB)
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
	if got := h.lines("killed unnamed"); !slices.Equal(got, []string{"launcher: guest memory: killed unnamed count=1 oom_kill=1"}) {
		t.Fatalf("the unnamed lines are %q, want the rise at once", got)
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

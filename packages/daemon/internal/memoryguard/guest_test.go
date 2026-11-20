package memoryguard

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
		c.setEvents(41, n)
		return
	}
	c.write("memory.oom_control", fmt.Sprintf("oom_kill_disable 0\nunder_oom 0\noom_kill %d\n", n))
}

// setEvents writes v2's memory.events: atMax times the cgroup reached its limit (max) and kills
// kills (oom_kill).
func (c *fakeCgroup) setEvents(atMax, kills uint64) {
	c.write("memory.events", fmt.Sprintf("low 0\nhigh 0\nmax %d\noom 3\noom_kill %d\noom_group_kill 0\n", atMax, kills))
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
// protected, and the role-init, the shim and the agent run the tester's session, gopls, the eval
// kernel and the command are the tester's, the test database's process is protected and of no
// role, and the exec's shell is neither. No process here has an oom_score above 1150, so a test's
// process above it is the first the kernel would pick.
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
	session := Class{Role: "tester", Protected: true, Session: true}
	return map[int]Class{
		guestInit: session, guestShim: session, guestAgent: session, guestBroker: tester(true), guestMux: tester(true),
		guestGopls: tester(false), guestEval: tester(false), guestBash: tester(false),
		guestTestDB: {Protected: true},
	}
}

// command is a process of the tester's that the agent's command runs, under the shell command.
func command(pid int, start uint64, name string, score int, resident uint64) guestProcess {
	return guestProcess{fakeProcess: fakeProcess{pid: pid, ppid: guestBash, start: start, args: []string{name}, resident: resident}, score: score}
}

// showKill writes pid's status as the kernel's out-of-memory kill leaves a process of the pod
// (killedStatus).
func showKill(pod *fakePod, pid int, name string, zombie bool) {
	pod.t.Helper()
	pod.writeWhole(filepath.Join(pod.proc, strconv.Itoa(pid), "status"), killedStatus(pid, name, zombie))
}

// killedStatus is the status the kernel's out-of-memory kill leaves a process of the pod with: its
// SIGKILL pending for the whole thread group (ShdPnd), where the kill sends it, and for the thread
// as well (SigPnd) until the thread takes it on its way out. A zombie's thread has taken it: its
// status reads state Z, with SIGKILL in ShdPnd alone.
func killedStatus(pid int, name string, zombie bool) string {
	state, thread := "R (running)", "0000000000000100"
	if zombie {
		state, thread = "Z (zombie)", "0000000000000000"
	}
	return fmt.Sprintf("Name:\t%s\nState:\t%s\nTgid:\t%d\nPid:\t%d\nThreads:\t1\nSigQ:\t1/63451\nSigPnd:\t%s\nShdPnd:\t0000000000000100\n",
		name, state, pid, pid, thread)
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
// for every scan. Its watch of the file-modified event never fires (neverModified), so each kill
// a test writes is read at the scan the test lets the watch take; a test of the wake sets one it
// fires itself (firedWatch), or the kernel's (watchModified), before run.
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
	h.g.watchModified = neverModified
	return h
}

// neverModified is a watch of a file-modified event that starts and never fires: the tests write
// memory.events in place (setKills), which the kernel's watch would wake the watch at, between two
// of the scans they let the watch take one at a time.
func neverModified(string) (*fileWatch, error) {
	return &fileWatch{modified: make(chan struct{}), stop: func() {}}, nil
}

// awaitKills waits in real time, 5 seconds at most, for the sink to have taken n kills, and
// answers them: a wake names a kill on the watch's goroutine, at no scan the test lets it take.
func (h *guestHarness) awaitKills(n int) []Kill {
	h.t.Helper()
	if !testwait.Holds(testwait.Bounded(h.t, 5*time.Second, time.Second), time.Millisecond, func() bool { return len(h.reported()) >= n }) {
		h.t.Fatalf("the sink took %+v in 5 seconds, want %d kills; the log:\n%s", h.reported(), n, h.logged())
	}
	return h.reported()
}

// start starts the watch.
func (h *guestHarness) start() {
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel, h.done = cancel, make(chan struct{})
	go func() {
		defer close(h.done)
		h.g.Run(ctx)
	}()
	h.t.Cleanup(h.stop)
}

// run starts the watch and returns once its start and its first scan are done.
func (h *guestHarness) run() *guestHarness {
	h.start()
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

// tickReadingStatus lets the watch take its next scan with pid's status a named pipe, and returns
// once that scan is done. The scan's evidence pass opens the pipe among its status reads, and it
// reads the session processes it holds once, after them; then between runs, and only then is
// status written into the pipe, so the pass reads status after between and before anything it
// reads after its status reads. pid's status is then a file holding status, for every later read.
func (h *guestHarness) tickReadingStatus(pod *fakePod, pid int, status string, between func()) {
	h.t.Helper()
	path := filepath.Join(pod.proc, strconv.Itoa(pid), "status")
	open := pod.namedPipe(path, nil)
	w := h.pending
	w.fire <- h.clock.advance(w.d)
	var o openedPipe
	select {
	case o = <-open:
	case <-time.After(testwait.Bounded(h.t, time.Minute, time.Second)):
		h.t.Fatalf("no evidence pass read pid %d's status; the log:\n%s", pid, h.logged())
	}
	if o.err != nil {
		h.t.Fatal(o.err)
	}
	// A test that fails in between still lets the pass's read of the pipe end.
	h.t.Cleanup(func() { _ = o.writer.Close() })
	between()
	_, err := o.writer.WriteString(status)
	if err := errors.Join(err, o.writer.Close()); err != nil {
		h.t.Fatal(err)
	}
	pod.writeWhole(path, status)
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

// A panic in a step of the guest watch, here its classifier's at a scan, turns the watch off for
// good with one off line that names the panic and where it was raised: the watch asks for no next
// scan, and Run returns at its context's end alone. A panic that left Run would end the launcher,
// PID 1 of the pod, which runs the watch on a goroutine of its own.
func TestGuestTurnsOffOnAPanicInAScanAndRunsOnUntilItsContextEnds(t *testing.T) {
	const start = 300
	pod := newFakePod(t)
	pod.setSelfScore(start)
	classes := guestTree(pod, start)
	h := newGuestHarness(t, pod.proc, newFakeCgroup(t, cgroupV2).dir, classes)
	var panicking atomic.Bool
	h.g.classify = func(*Table) map[int]Class {
		if panicking.Load() {
			panic("a test's classifier panics")
		}
		return classes
	}
	h.run()
	panicking.Store(true)
	w := h.pending
	w.fire <- h.clock.advance(w.d)
	if !testwait.Holds(testwait.Bounded(t, time.Minute, time.Second), time.Millisecond, func() bool { return len(h.lines("off ")) > 0 }) {
		t.Fatalf("the watch wrote no off line after its classifier panicked; the log:\n%s", h.logged())
	}
	want := "launcher: guest memory: off reason=panic: a test's classifier panics at "
	if off := h.lines("off "); len(off) != 1 || !strings.HasPrefix(off[0], want) || !strings.Contains(off[0], "guest_test.go:") {
		t.Fatalf("the watch's off lines are %q, want one starting %q that names this test's file", off, want)
	}
	h.stop()
	if len(h.clock.waits) != 0 {
		t.Fatalf("the watch waited for another scan after its classifier panicked")
	}
}

// A panic in the guest watch's first scan, which its start takes once it has started its wake on
// memory.events (watchKills), stops that wake at once, with the off line, as a panic at any later
// step does: the watch reads no file-modified event, and holds no inotify descriptor, while it
// waits for its context's end. The wake is stopped once.
func TestGuestStopsItsWakeAtOnceOnAPanicInItsFirstScan(t *testing.T) {
	const start = 300
	pod := newFakePod(t)
	pod.setSelfScore(start)
	classes := guestTree(pod, start)
	h := newGuestHarness(t, pod.proc, newFakeCgroup(t, cgroupV2).dir, classes)
	h.g.classify = func(*Table) map[int]Class { panic("a test's classifier panics") }
	var stops atomic.Int32
	h.g.watchModified = func(string) (*fileWatch, error) {
		return &fileWatch{modified: make(chan struct{}), stop: func() { stops.Add(1) }}, nil
	}
	h.start()
	if !testwait.Holds(testwait.Bounded(t, time.Minute, time.Second), time.Millisecond, func() bool { return len(h.lines("off ")) > 0 }) {
		t.Fatalf("the watch wrote no off line after its first scan panicked; the log:\n%s", h.logged())
	}
	want := "launcher: guest memory: off reason=panic: a test's classifier panics at "
	if off := h.lines("off "); len(off) != 1 || !strings.HasPrefix(off[0], want) {
		t.Fatalf("the watch's off lines are %q, want one starting %q", off, want)
	}
	if off := h.lines("wake off"); len(off) != 0 {
		t.Fatalf("the watch logged %q, want its wake started before its first scan", off)
	}
	if !testwait.Holds(testwait.Bounded(t, 5*time.Second, time.Second), time.Millisecond, func() bool { return stops.Load() > 0 }) {
		t.Fatalf("the watch's wake still ran 5 seconds after its first scan panicked, want it stopped at once; the log:\n%s", h.logged())
	}
	h.stop()
	if n := stops.Load(); n != 1 {
		t.Fatalf("the watch stopped its wake %d times, want once", n)
	}
	if len(h.clock.waits) != 0 {
		t.Fatalf("the watch waited for a scan after its first scan panicked")
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

			want := fmt.Sprintf(`launcher: memory guard: ended pid=%d role=tester size=%d reading=%d counter=%d shmem=%d limit=%d rule=kernel sizes=kernel less_parent=0 new_read=- executable="python3"`,
				load, 1500*mib, 3500*mib, 3900*mib, 32*mib, podLimit)
			if got := h.log.with("ended "); !slices.Equal(got, []string{want}) {
				t.Fatalf("the kill lines are %q, want %q; the log:\n%s", got, want, h.logged())
			}
			wantKill := Kill{
				LauncherMemoryKill: shimwire.LauncherMemoryKill{
					Guard: h.g.ID(), Sequence: 1, PID: load, Executable: "python3", SizeBytes: 1500 * mib,
					ReadingBytes: 3500 * mib, CounterBytes: 3900 * mib, LimitBytes: podLimit, At: at, Cause: shimwire.MemoryKillCauseKernel,
				},
				Role: "tester", Rule: "kernel", Sizes: "kernel",
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

// Processes a pass set aside because more showed the kill than kills were pending go on counting
// until the first scan after they have left /proc: the tester's cc1 and ld both show SIGKILL at a
// rise of one, and the pass names neither. Later in the same wait another sender's SIGKILL reaches
// the tester's sleep, which the watch holds. sleep is then one candidate beside two counted
// processes against one kill: the pass reads the counter again, finds no rise, and names none,
// and the kill has its unnamed line.
func TestGuestCountsWhatAPassSetAsideAgainstAnotherSendersSIGKILLInTheWait(t *testing.T) {
	const cc1, ld, sleep = 817, 819, 820
	pod := newFakePod(t)
	pod.setSelfScore(0)
	classes := guestTree(pod, 0)
	pod.addGuest(command(cc1, 8170, "cc1", 1300, 300*mib), command(ld, 8190, "ld", 1200, 900*mib),
		command(sleep, 8200, "sleep", 1000, 4*mib))
	classes[cc1], classes[ld], classes[sleep] = Class{Role: "tester"}, Class{Role: "tester"}, Class{Role: "tester"}
	cgroup := newFakeCgroup(t, cgroupV2)
	h := newGuestHarness(t, pod.proc, cgroup.dir, classes).run()

	reaped(pod, cc1, 8170, "cc1")
	reaped(pod, ld, 8190, "ld")
	cgroup.setKills(1)
	h.tick()
	// Another sender's SIGKILL reaches sleep in the same wait.
	showKill(pod, sleep, "sleep", false)
	h.tick()
	if ended, kills := h.log.with("ended "), h.reported(); len(ended) != 0 || len(kills) != 0 {
		t.Fatalf("the watch wrote %q and reported %+v, want neither: sleep (%d) showed another sender's SIGKILL beside two set-aside processes; the log:\n%s",
			ended, kills, sleep, h.logged())
	}
	h.tickAfter(guestKillWait)
	if got, want := h.lines("killed unnamed"), unnamedLine(1, 1, 700*mib, gib, 16*mib); !slices.Equal(got, []string{want}) {
		t.Fatalf("the unnamed lines are %q, want %q alone; the log:\n%s", got, want, h.logged())
	}
	if ended, kills := h.log.with("ended "), h.reported(); len(ended) != 0 || len(kills) != 0 {
		t.Fatalf("the watch wrote %q and reported %+v after the wait, want neither; the log:\n%s", ended, kills, h.logged())
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

// A process shows the kernel's kill by SIGKILL's bit, 0x100, in ShdPnd, the pending mask of its
// thread group, where the kill sends it, whatever SigPnd holds. SIGKILL in SigPnd alone, the
// thread's own mask, is no kill: the kernel puts it there for every thread of a process that any
// fatal signal is ending, a SIGTERM role-init's drain sends among them. Other signals pending,
// SIGKILL's bit in the masks of signals blocked, ignored or caught, a pending mask that does not
// parse, and a status with no pending mask show nothing.
func TestGuestReadsTheKillInTheThreadGroupsPendingMaskAlone(t *testing.T) {
	const head = "Name:\tpython3\nState:\tR (running)\nTgid:\t850\nPid:\t850\nPPid:\t108\nThreads:\t1\nSigQ:\t1/63451\n"
	const tail = "SigBlk:\t0000000000000000\nSigIgn:\t0000000001001000\nSigCgt:\t0000000180000002\n"
	for _, tc := range []struct {
		name, status string
		want         bool
	}{
		{"SIGKILL pending for the thread group alone, as a zombie's", head + "SigPnd:\t0000000000000000\nShdPnd:\t0000000000000100\n" + tail, true},
		{"SIGKILL pending for the thread group and the thread, as a group's SIGKILL leaves a running process",
			head + "SigPnd:\t0000000000000100\nShdPnd:\t0000000000000100\n" + tail, true},
		{"SIGKILL pending for the thread alone, as a fatal SIGTERM leaves it", head + "SigPnd:\t0000000000000100\nShdPnd:\t0000000000004000\n" + tail, false},
		{"SIGKILL pending for the thread alone, nothing for the group", head + "SigPnd:\t0000000000000100\nShdPnd:\t0000000000000000\n" + tail, false},
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

// A process of no role that the kernel killed has the guard's line, naming role none, and the sink
// takes its kill with an empty Role, as the guard's sink takes the guard's: the launcher's sink
// keeps a role's kills alone, so no role is told of it. It counts in the sequence, as the guard's
// kills do, so the next kill, of the tester's, is the second.
func TestGuestHandsTheSinkAKillOfAProcessOfNoRoleWithAnEmptyRole(t *testing.T) {
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
	if kills := h.reported(); len(kills) != 1 || kills[0].PID != outside || kills[0].Role != "" || kills[0].Sequence != 1 ||
		kills[0].Cause != shimwire.MemoryKillCauseKernel || kills[0].SizeBytes != 700*mib {
		t.Fatalf("the sink took %+v, want the one kill of pid %d with no role, the first", kills, outside)
	}

	reaped(pod, load, 8410, "python3")
	cgroup.setKills(2)
	h.tick()
	if kills := h.reported(); len(kills) != 2 || kills[1].PID != load || kills[1].Sequence != 2 || kills[1].Role != "tester" {
		t.Fatalf("the sink took %+v, want the tester's %d as the second kill", kills, load)
	}
}

// The kernel raises the counter just before it sends SIGKILL, so the scan that sees a rise may find
// no process showing the kill yet, and the watch reads for it at the scans after. At the next one
// the tester's python3 shows the kill, its memory freed, while the tester's rg exits by itself.
// python3 is named at its size before the rise, and rg, which showed no kill, is named for none.
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

// A victim is named at its size at the last scan whose snapshot came before the pass that showed
// its kill, though kills were pending through every scan before it: in a burst the kernel's next
// victim is often a process that grew after the first kill. The kernel kills the tester's cc1,
// whose parent reaps it at once, so no process shows that kill and it stays pending. The tester's
// python3 then grows from 40 MiB to 1500 MiB and node from 100 MiB to 900 MiB over five scans, and
// the next two rises of the wait kill python3, then node, each showing the kill at the scan of its
// rise with its memory freed. python3 is named at 1500 MiB and node at 900 MiB, each to the tester,
// and cc1's kill has its unnamed line once the wait ends.
func TestGuestNamesAVictimAtItsSizeAtTheLastScanWhoseSnapshotCameBeforeThePassThatShowedItsKill(t *testing.T) {
	const cc1, python, node = 990, 991, 992
	pod := newFakePod(t)
	pod.setSelfScore(0)
	classes := guestTree(pod, 0)
	pod.addGuest(command(cc1, 9900, "cc1", 1400, 300*mib), command(python, 9910, "python3", 1300, 40*mib),
		command(node, 9920, "node", 1200, 100*mib))
	classes[cc1], classes[python], classes[node] = Class{Role: "tester"}, Class{Role: "tester"}, Class{Role: "tester"}
	cgroup := newFakeCgroup(t, cgroupV2)
	h := newGuestHarness(t, pod.proc, cgroup.dir, classes).run()

	// The kernel kills cc1, and its parent reaps it at once.
	pod.remove(cc1)
	cgroup.setKills(1)
	h.tick()
	// python3 and node grow over five scans.
	for _, size := range []struct{ python, node uint64 }{{300, 260}, {600, 420}, {900, 580}, {1200, 740}, {1500, 900}} {
		grownPython := command(python, 9910, "python3", 1300+int(size.python/4), size.python*mib)
		grownNode := command(node, 9920, "node", 1200+int(size.node/4), size.node*mib)
		grownPython.adj, grownNode.adj = raisedScore, raisedScore
		pod.addGuest(grownPython, grownNode)
		h.tick()
	}
	// The kernel kills python3, then node, each still in /proc while its memory is freed.
	reaped(pod, python, 9910, "python3")
	cgroup.setKills(2)
	h.tick()
	reaped(pod, node, 9920, "node")
	cgroup.setKills(3)
	h.tick()
	h.tickAfter(guestKillWait)
	kills := h.reported()
	if len(kills) != 2 || kills[0].PID != python || kills[0].Role != "tester" || kills[0].Sequence != 1 || kills[0].SizeBytes != 1500*mib ||
		kills[1].PID != node || kills[1].Role != "tester" || kills[1].Sequence != 2 || kills[1].SizeBytes != 900*mib {
		t.Fatalf("the sink took %+v, want python3 (%d) at %d bytes, then node (%d) at %d bytes, each at its size at the last scan whose snapshot came before the pass that showed its kill; the log:\n%s",
			kills, python, 1500*mib, node, 900*mib, h.logged())
	}
	ended := h.log.with("ended ")
	if len(ended) != 2 || !strings.Contains(ended[0], fmt.Sprintf("ended pid=%d role=tester size=%d ", python, 1500*mib)) ||
		!strings.Contains(ended[1], fmt.Sprintf("ended pid=%d role=tester size=%d ", node, 900*mib)) {
		t.Fatalf("the kill lines are %q, want python3's at %d bytes, then node's at %d; the log:\n%s", ended, 1500*mib, 900*mib, h.logged())
	}
	if got, want := h.lines("killed unnamed"), unnamedLine(1, 3, 700*mib, gib, 16*mib); !slices.Equal(got, []string{want}) {
		t.Fatalf("the unnamed lines are %q, want %q alone; the log:\n%s", got, want, h.logged())
	}
}

// A process that a scan first reads after the kernel killed it is set aside, and never named at
// a size read while its memory is freed: a pass reads only the processes the watch already holds,
// so a process that started after one scan and that the kernel killed before the next is first
// read by that next scan's snapshot. The tester's cc1 starts after a scan and grows to 1200 MiB.
// The kernel kills it, and before the next scan the counter rises and cc1 shows SIGKILL pending,
// with 300 MiB of it left. cc1 is named at no pass, the tester is told nothing, and the kill has
// its unnamed line once the wait ends.
func TestGuestNamesNoProcessFirstReadAfterItsKill(t *testing.T) {
	const cc1 = 950
	pod := newFakePod(t)
	pod.setSelfScore(0)
	classes := guestTree(pod, 0)
	classes[cc1] = Class{Role: "tester"}
	cgroup := newFakeCgroup(t, cgroupV2)
	h := newGuestHarness(t, pod.proc, cgroup.dir, classes).run()

	// cc1 starts after that scan and grows to 1200 MiB. The kernel kills it, and 900 MiB of it is
	// freed before the next scan reads it.
	pod.addGuest(command(cc1, 9500, "cc1", 0, 300*mib))
	showKill(pod, cc1, "cc1", false)
	cgroup.setKills(1)
	h.tick()
	h.tick()
	if ended, kills := h.log.with("ended "), h.reported(); len(ended) != 0 || len(kills) != 0 {
		t.Fatalf("the watch wrote %q and reported %+v, want neither: cc1 (%d) was first read after its kill, with %d of its %d bytes left; the log:\n%s",
			ended, kills, cc1, 300*mib, 1200*mib, h.logged())
	}
	h.tickAfter(guestKillWait)
	if ended, kills := h.log.with("ended "), h.reported(); len(ended) != 0 || len(kills) != 0 {
		t.Fatalf("the watch wrote %q and reported %+v after the wait, want neither; the log:\n%s", ended, kills, h.logged())
	}
	if got, want := h.lines("killed unnamed"), unnamedLine(1, 1, 700*mib, gib, 16*mib); !slices.Equal(got, []string{want}) {
		t.Fatalf("the unnamed lines are %q, want %q alone; the log:\n%s", got, want, h.logged())
	}
}

// A process that a scan first reads after the kernel killed it counts against the kill pending
// until the first scan after it has left /proc, so a SIGKILL another sender sends in the same wait
// is named for no kill.
// The tester's cc1 starts after a scan, and before the next scan the kernel kills it: the counter
// reads one, and cc1 shows SIGKILL pending. That scan's pass has no candidate, since the watch does
// not know cc1 yet, and its snapshot first reads cc1 with SIGKILL pending. During the wait the
// role's own kill -9 reaches the tester's sleep, which the watch held before the rise. The pass at
// the next scan counts two processes against one kill, reads the counter again, finds no rise, and
// names neither; the tester is told nothing, and the kill has its unnamed line once the wait ends.
func TestGuestCountsAProcessFirstReadAfterItsKillAgainstAnotherSendersSIGKILLInTheWait(t *testing.T) {
	const cc1, sleep = 953, 954
	pod := newFakePod(t)
	pod.setSelfScore(0)
	classes := guestTree(pod, 0)
	pod.addGuest(command(sleep, 9540, "sleep", 1001, mib))
	classes[cc1], classes[sleep] = Class{Role: "tester"}, Class{Role: "tester"}
	cgroup := newFakeCgroup(t, cgroupV2)
	h := newGuestHarness(t, pod.proc, cgroup.dir, classes).run()

	// cc1 starts after that scan, and the kernel kills it before the next one.
	pod.addGuest(command(cc1, 9530, "cc1", 0, 300*mib))
	showKill(pod, cc1, "cc1", false)
	cgroup.setKills(1)
	h.tick()
	// The role's own kill -9 reaches sleep during the wait.
	showKill(pod, sleep, "sleep", false)
	h.tick()
	if ended, kills := h.log.with("ended "), h.reported(); len(ended) != 0 || len(kills) != 0 {
		t.Fatalf("the watch wrote %q and reported %+v, want neither: cc1 (%d), first read with SIGKILL pending, and sleep (%d) show one kill; the log:\n%s",
			ended, kills, cc1, sleep, h.logged())
	}
	h.tickAfter(guestKillWait)
	if ended, kills := h.log.with("ended "), h.reported(); len(ended) != 0 || len(kills) != 0 {
		t.Fatalf("the watch wrote %q and reported %+v after the wait, want neither; the log:\n%s", ended, kills, h.logged())
	}
	if got, want := h.lines("killed unnamed"), unnamedLine(1, 1, 700*mib, gib, 16*mib); !slices.Equal(got, []string{want}) {
		t.Fatalf("the unnamed lines are %q, want %q alone; the log:\n%s", got, want, h.logged())
	}
}

// A process that a scan first reads with SIGKILL already pending while no kill is pending is never
// named either, and counts for no burst (the architect's rule of 2026-11-16T16:14Z): its SIGKILL
// can be another sender's, or the kernel's for a rise that comes after that scan's read of the
// counter. The tester's cc1 starts after a scan, and the next scan first reads it with SIGKILL
// pending, the counter not risen yet. The kernel then kills the tester's python3, which shows the
// kill while cc1 is still in /proc, SIGKILL pending: python3 is named, alone, and no kill is left
// unnamed. python3's parent reaps it, cc1 exits, a zombie its parent never reaps, and the counter
// rises again with no other process showing the kill: cc1 is named for none, and that kill has
// its unnamed line once the wait ends.
func TestGuestNamesALaterRisesVictimBesideAProcessFirstReadWithSIGKILLWhileNoKillWasPending(t *testing.T) {
	const cc1, python = 951, 952
	pod := newFakePod(t)
	pod.setSelfScore(0)
	classes := guestTree(pod, 0)
	pod.addGuest(command(python, 9520, "python3", 1300, 900*mib))
	classes[cc1], classes[python] = Class{Role: "tester"}, Class{Role: "tester"}
	cgroup := newFakeCgroup(t, cgroupV2)
	h := newGuestHarness(t, pod.proc, cgroup.dir, classes).run()

	// cc1 starts after that scan, and the next scan first reads it with SIGKILL pending.
	cc1Process := command(cc1, 9510, "cc1", 0, 300*mib)
	pod.addGuest(cc1Process)
	showKill(pod, cc1, "cc1", false)
	h.tick()
	// The kernel kills python3, which shows the kill; cc1 is still in /proc, SIGKILL pending.
	reaped(pod, python, 9520, "python3")
	cgroup.setKills(1)
	h.tick()
	kills := h.reported()
	if len(kills) != 1 || kills[0].PID != python || kills[0].Role != "tester" || kills[0].SizeBytes != 900*mib || kills[0].Sequence != 1 {
		t.Fatalf("the sink took %+v, want python3 (%d) alone, the tester's, at %d bytes: cc1 (%d), first read with SIGKILL pending while no kill was pending, counts for no burst; the log:\n%s",
			kills, python, 900*mib, cc1, h.logged())
	}
	h.tickAfter(guestKillWait)
	if unnamed := h.lines("killed unnamed"); len(unnamed) != 0 {
		t.Fatalf("the watch logged %q after the first wait, want no unnamed kill: python3 was named for it; the log:\n%s", unnamed, h.logged())
	}

	// python3's parent reaps it; cc1 exits, a zombie its parent never reaps, and the counter rises
	// with no other process showing the kill.
	pod.remove(python)
	cc1Process.zombie, cc1Process.resident = true, 0
	pod.add(cc1Process.fakeProcess)
	showKill(pod, cc1, "cc1", true)
	cgroup.setKills(2)
	h.tick()
	h.tickAfter(guestKillWait)
	if ended, kills := h.log.with("ended "), h.reported(); len(ended) != 1 || len(kills) != 1 {
		t.Fatalf("the watch wrote %q and reported %+v, want python3's one kill alone: cc1 (%d) is never named; the log:\n%s", ended, kills, cc1, h.logged())
	}
	if got, want := h.lines("killed unnamed"), unnamedLine(1, 2, 700*mib, gib, 16*mib); !slices.Equal(got, []string{want}) {
		t.Fatalf("the unnamed lines after the second wait are %q, want %q alone; the log:\n%s", got, want, h.logged())
	}
}

// A process a pass set aside counts against the kills pending of its own burst alone, so one that
// stays in /proc, a zombie its parent never reaps, leaves no later burst's kill unnamed (the
// architect's rule of 2026-11-16T16:14Z). The tester's cc1 and ld both show SIGKILL at a rise of
// one, and the pass sets both aside. cc1's parent reaps it; ld's never does, and ld stays a zombie.
// That wait ends on its unnamed line. In a later wait the kernel kills the tester's python3, which
// shows the kill: it is named, alone, though ld is still in /proc.
func TestGuestNamesALaterBurstsVictimBesideAZombieAnEarlierPassSetAside(t *testing.T) {
	const cc1, ld, python = 958, 959, 960
	pod := newFakePod(t)
	pod.setSelfScore(0)
	classes := guestTree(pod, 0)
	ldProcess := command(ld, 9590, "ld", 1200, 900*mib)
	pod.addGuest(command(cc1, 9580, "cc1", 1300, 300*mib), ldProcess, command(python, 9600, "python3", 1100, 800*mib))
	classes[cc1], classes[ld], classes[python] = Class{Role: "tester"}, Class{Role: "tester"}, Class{Role: "tester"}
	cgroup := newFakeCgroup(t, cgroupV2)
	h := newGuestHarness(t, pod.proc, cgroup.dir, classes).run()

	reaped(pod, cc1, 9580, "cc1")
	reaped(pod, ld, 9590, "ld")
	cgroup.setKills(1)
	h.tick()
	// cc1's parent reaps it; ld's never does.
	pod.remove(cc1)
	ldProcess.zombie, ldProcess.resident = true, 0
	pod.add(ldProcess.fakeProcess)
	showKill(pod, ld, "ld", true)
	h.tick()
	h.tickAfter(guestKillWait)
	if ended, kills := h.log.with("ended "), h.reported(); len(ended) != 0 || len(kills) != 0 {
		t.Fatalf("the watch wrote %q and reported %+v in the first wait, want neither: cc1 and ld showed one kill; the log:\n%s", ended, kills, h.logged())
	}

	// A later wait: the kernel kills python3, which shows the kill.
	reaped(pod, python, 9600, "python3")
	cgroup.setKills(2)
	h.tick()
	kills := h.reported()
	if len(kills) != 1 || kills[0].PID != python || kills[0].Role != "tester" || kills[0].SizeBytes != 800*mib {
		t.Fatalf("the sink took %+v, want python3 (%d) alone, the tester's, at %d bytes: ld (%d), set aside in an earlier wait, counts for none of this one; the log:\n%s",
			kills, python, 800*mib, ld, h.logged())
	}
}

// The same for a process a scan first read with SIGKILL pending: it counts against the kills
// pending of the burst it was first read in, and is never named, but it counts at no later burst
// (the architect's rule of 2026-11-16T16:14Z). The tester's cc1 starts after a scan, and before
// the next scan the kernel kills it, so that scan first reads it with SIGKILL pending; that wait
// ends on its unnamed line. cc1 exits, a zombie its parent never reaps. In a later wait the kernel
// kills the tester's python3, which shows the kill: it is named, alone, though cc1 is still in /proc.
func TestGuestNamesALaterBurstsVictimBesideAZombieFirstReadWithSIGKILLPending(t *testing.T) {
	const cc1, python = 961, 962
	pod := newFakePod(t)
	pod.setSelfScore(0)
	classes := guestTree(pod, 0)
	pod.addGuest(command(python, 9620, "python3", 1100, 800*mib))
	classes[cc1], classes[python] = Class{Role: "tester"}, Class{Role: "tester"}
	cgroup := newFakeCgroup(t, cgroupV2)
	h := newGuestHarness(t, pod.proc, cgroup.dir, classes).run()

	// cc1 starts after that scan, and the kernel kills it before the next one.
	cc1Process := command(cc1, 9610, "cc1", 0, 300*mib)
	pod.addGuest(cc1Process)
	showKill(pod, cc1, "cc1", false)
	cgroup.setKills(1)
	h.tick()
	// cc1 exits, a zombie its parent never reaps.
	cc1Process.zombie, cc1Process.resident = true, 0
	pod.add(cc1Process.fakeProcess)
	showKill(pod, cc1, "cc1", true)
	h.tick()
	h.tickAfter(guestKillWait)
	if ended, kills := h.log.with("ended "), h.reported(); len(ended) != 0 || len(kills) != 0 {
		t.Fatalf("the watch wrote %q and reported %+v in the first wait, want neither: cc1 was first read with SIGKILL pending; the log:\n%s", ended, kills, h.logged())
	}

	// A later wait: the kernel kills python3, which shows the kill.
	reaped(pod, python, 9620, "python3")
	cgroup.setKills(2)
	h.tick()
	kills := h.reported()
	if len(kills) != 1 || kills[0].PID != python || kills[0].Role != "tester" || kills[0].SizeBytes != 800*mib {
		t.Fatalf("the sink took %+v, want python3 (%d) alone, the tester's, at %d bytes: cc1 (%d), first read with SIGKILL pending in an earlier wait, counts for none of this one; the log:\n%s",
			kills, python, 800*mib, cc1, h.logged())
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
// (the test database's server) has the line, and its Kill with no role, which the launcher's sink
// drops, as every process of no role has, with the kernel's protected cause, and counts in the
// sequence.
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

	want := fmt.Sprintf(`launcher: memory guard: ended pid=%d role=tester size=%d reading=%d counter=%d shmem=%d limit=%d rule=kernel sizes=kernel less_parent=0 new_read=- executable="omp"`,
		guestAgent, 2*gib, 3500*mib, 3900*mib, 32*mib, podLimit)
	if got := h.log.with("ended "); !slices.Equal(got, []string{want}) {
		t.Fatalf("the kill lines are %q, want %q; the log:\n%s", got, want, h.logged())
	}
	wantKill := Kill{
		LauncherMemoryKill: shimwire.LauncherMemoryKill{
			Guard: h.g.ID(), Sequence: 1, PID: guestAgent, Executable: "omp", SizeBytes: 2 * gib,
			ReadingBytes: 3500 * mib, CounterBytes: 3900 * mib, LimitBytes: podLimit, At: at, Cause: shimwire.MemoryKillCauseKernelProtected,
		},
		Role: "tester", Rule: "kernel", Sizes: "kernel",
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
	if kills := h.reported(); len(kills) != 2 || kills[1].PID != guestTestDB || kills[1].Role != "" || kills[1].Sequence != 2 ||
		kills[1].Cause != shimwire.MemoryKillCauseKernelProtected {
		t.Fatalf("the sink took %+v, want the test database's kill with no role as the second", kills)
	}
}

// A role whose session ends during the wait names nothing for the rest of the burst: role-init's
// drain ends what the role left running with a SIGKILL that shows in ShdPnd as the kernel's does
// (kill(-1) in runRoleShim). The kernel kills the tester's python3, whose parent reaps it before
// any scan reads it. The tester's agent then leaves /proc, and the drain leaves the tester's rg
// with SIGKILL pending. rg is not named, the kill has its unnamed line once the wait ends, and the
// tester is told nothing.
func TestGuestNamesNoProcessOfARoleWhoseSessionEndedDuringTheWait(t *testing.T) {
	const python, rg = 930, 931
	pod := newFakePod(t)
	pod.setSelfScore(0)
	classes := guestTree(pod, 0)
	pod.addGuest(command(python, 9300, "python3", 1400, 1500*mib), command(rg, 9310, "rg", 1340, 50*mib))
	classes[python], classes[rg] = Class{Role: "tester"}, Class{Role: "tester"}
	cgroup := newFakeCgroup(t, cgroupV2)
	h := newGuestHarness(t, pod.proc, cgroup.dir, classes).run()

	// The kernel kills python3, and its parent reaps it at once.
	pod.remove(python)
	cgroup.setKills(1)
	h.tick()
	// The tester's agent leaves, and role-init's drain kills what the role left running.
	pod.remove(guestAgent)
	showKill(pod, rg, "rg", false)
	h.tick()
	h.tickAfter(guestKillWait)
	if ended, kills := h.log.with("ended "), h.reported(); len(ended) != 0 || len(kills) != 0 {
		t.Fatalf("the watch wrote %q and reported %+v, want neither: rg shows the SIGKILL of the tester's role-init; the log:\n%s", ended, kills, h.logged())
	}
	if got, want := h.lines("killed unnamed"), unnamedLine(1, 1, 700*mib, gib, 16*mib); !slices.Equal(got, []string{want}) {
		t.Fatalf("the unnamed lines are %q, want %q alone; the log:\n%s", got, want, h.logged())
	}
}

// A role whose session ended names nothing for the burst, and every other role names its kills as
// ever. The kernel kills the tester's python3, whose parent reaps it before any scan reads it. The
// tester's agent leaves /proc, and role-init's drain leaves the tester's rg with SIGKILL pending.
// The counter rises again, and the reviewer's cargo shows the kill: cargo is named, to the
// reviewer, as the burst's one named kill, rg is named for none, and the first kill has its
// unnamed line once the wait ends.
func TestGuestNamesAnotherRolesKillInABurstWhereARolesSessionEnded(t *testing.T) {
	const python, rg, cargo = 932, 933, 934
	pod := newFakePod(t)
	pod.setSelfScore(0)
	classes := guestTree(pod, 0)
	pod.addGuest(command(python, 9320, "python3", 1400, 1500*mib), command(rg, 9330, "rg", 1340, 50*mib),
		guestProcess{fakeProcess: fakeProcess{pid: cargo, ppid: 1, start: 9340, args: []string{"cargo"}, resident: 900 * mib}, score: 1380})
	classes[python], classes[rg], classes[cargo] = Class{Role: "tester"}, Class{Role: "tester"}, Class{Role: "reviewer"}
	cgroup := newFakeCgroup(t, cgroupV2)
	h := newGuestHarness(t, pod.proc, cgroup.dir, classes).run()

	// The kernel kills python3, and its parent reaps it at once.
	pod.remove(python)
	cgroup.setKills(1)
	h.tick()
	// The tester's agent leaves, and role-init's drain kills what the role left running.
	pod.remove(guestAgent)
	showKill(pod, rg, "rg", false)
	h.tick()
	// The kernel kills the reviewer's cargo, which stays in /proc while its memory is freed.
	showKill(pod, cargo, "cargo", false)
	cgroup.setKills(2)
	h.tick()
	h.tickAfter(guestKillWait)
	kills := h.reported()
	if len(kills) != 1 || kills[0].PID != cargo || kills[0].Role != "reviewer" || kills[0].Sequence != 1 || kills[0].SizeBytes != 900*mib {
		t.Fatalf("the sink took %+v, want the one kill of the reviewer's cargo (%d); the log:\n%s", kills, cargo, h.logged())
	}
	if ended := h.log.with("ended "); len(ended) != 1 || !strings.HasSuffix(ended[0], `executable="cargo"`) {
		t.Fatalf("the kill lines are %q, want cargo's alone; the log:\n%s", ended, h.logged())
	}
	if got, want := h.lines("killed unnamed"), unnamedLine(1, 2, 700*mib, gib, 16*mib); !slices.Equal(got, []string{want}) {
		t.Fatalf("the unnamed lines are %q, want %q alone; the log:\n%s", got, want, h.logged())
	}
}

// A session process that shows the kill before it leaves /proc is named as any process is, and a
// zombie has not left. The kernel kills the tester's agent, which shows the kill as a zombie its
// shim has not reaped yet, the tester's python3, whose SIGKILL comes later, and a command whose
// parent reaps it at once. The agent is named, to the tester, with the kernel's protected cause,
// at its size before the kill. The scan leaves the zombie out, and the watch does not take that
// for a departure: python3, which shows the kill at the next scan while the agent is still a
// zombie, is named too, to the tester. The shim then reaps the agent, and role-init's drain leaves
// the tester's bash with SIGKILL pending. That departure ends the tester's session for the rest of
// the burst, so bash is named for none, and the third kill has its unnamed line once the wait
// ends.
func TestGuestNamesARolesZombieAgentAndTakesTheZombieForNoDepartureUntilItsShimReapsIt(t *testing.T) {
	const omp = "/opt/omp/bin/omp"
	const python = 938
	pod := newFakePod(t)
	pod.setSelfScore(0)
	classes := guestTree(pod, 0)
	pod.addGuest(command(python, 9380, "python3", 1400, 1500*mib))
	classes[python] = Class{Role: "tester"}
	cgroup := newFakeCgroup(t, cgroupV2)
	h := newGuestHarness(t, pod.proc, cgroup.dir, classes).run()

	// The kernel kills the agent, a zombie its shim has not reaped, python3, and a command reaped
	// at once.
	pod.add(fakeProcess{pid: guestAgent, ppid: guestShim, exe: omp, args: []string{omp, "--mode", "rpc"}, zombie: true})
	showKill(pod, guestAgent, "omp", true)
	cgroup.setKills(3)
	h.tick()
	kills := h.reported()
	if len(kills) != 1 || kills[0].PID != guestAgent || kills[0].Role != "tester" || kills[0].Sequence != 1 ||
		kills[0].Cause != shimwire.MemoryKillCauseKernelProtected || kills[0].SizeBytes != 2*gib {
		t.Fatalf("the sink took %+v, want the tester's agent (%d) with the protected cause at its %d bytes before the kill; the log:\n%s",
			kills, guestAgent, 2*gib, h.logged())
	}
	// The kernel's SIGKILL reaches python3 while the agent is still a zombie.
	reaped(pod, python, 9380, "python3")
	h.tick()
	kills = h.reported()
	if len(kills) != 2 || kills[1].PID != python || kills[1].Role != "tester" || kills[1].Sequence != 2 ||
		kills[1].Cause != shimwire.MemoryKillCauseKernel || kills[1].SizeBytes != 1500*mib {
		t.Fatalf("the sink took %+v, want the tester's python3 (%d) second, at its %d bytes before the kill: a zombie agent has not left; the log:\n%s",
			kills, python, 1500*mib, h.logged())
	}
	// The shim reaps the agent, and role-init's drain kills what the role left running.
	pod.remove(guestAgent)
	showKill(pod, guestBash, "bash", false)
	h.tick()
	h.tickAfter(guestKillWait)
	if kills := h.reported(); len(kills) != 2 {
		t.Fatalf("the sink took %+v, want the agent's and python3's kills alone: bash shows the SIGKILL of the tester's role-init; the log:\n%s", kills, h.logged())
	}
	if got, want := h.lines("killed unnamed"), unnamedLine(1, 3, 700*mib, gib, 16*mib); !slices.Equal(got, []string{want}) {
		t.Fatalf("the unnamed lines are %q, want %q alone; the log:\n%s", got, want, h.logged())
	}
}

// A pass that sees a session process leave /proc, at its read of the session processes after its
// status reads, cannot order that departure against a SIGKILL those reads saw: on the stop path
// role-init sends kill(-1) as soon as it has reaped the shim, and the shim's finish ends the
// renewer through exec's Cancel once it has reaped the agent, each just after a departure. So the
// pass records the departure, which ends the role's session at that same pass, and sets aside
// each process of the role that shows the kill. The kernel kills the tester's python3, whose
// parent reaps it at once. While the pass at the next scan waits on the status of the tester's
// rg, the tester's agent and shim leave /proc, and then rg's status shows SIGKILL pending. rg is
// named at no pass, the tester is told nothing, and the kill has its unnamed line once the wait
// ends. The same departures before that scan name nothing either.
func TestGuestNamesNoProcessOfARoleWhoseSessionLeavesWhileThePassReadsStatuses(t *testing.T) {
	const python, rg = 936, 937
	for _, tc := range []struct {
		name   string
		during bool
	}{
		{"the session ends while the pass reads rg's status", true},
		{"the session ends before the scan", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := newFakePod(t)
			pod.setSelfScore(0)
			classes := guestTree(pod, 0)
			pod.addGuest(command(python, 9360, "python3", 1400, 1500*mib), command(rg, 9370, "rg", 1340, 50*mib))
			classes[python], classes[rg] = Class{Role: "tester"}, Class{Role: "tester"}
			cgroup := newFakeCgroup(t, cgroupV2)
			h := newGuestHarness(t, pod.proc, cgroup.dir, classes).run()

			// The kernel kills python3, and its parent reaps it at once.
			pod.remove(python)
			cgroup.setKills(1)
			// The tester's agent and its shim leave, and role-init kills what the role left
			// running.
			end := func() {
				pod.remove(guestAgent)
				pod.remove(guestShim)
			}
			if tc.during {
				h.tickReadingStatus(pod, rg, killedStatus(rg, "rg", false), end)
			} else {
				end()
				showKill(pod, rg, "rg", false)
				h.tick()
			}
			if ended, kills := h.log.with("ended "), h.reported(); len(ended) != 0 || len(kills) != 0 {
				t.Fatalf("the watch wrote %q and reported %+v at that scan, want neither: rg's SIGKILL can be the tester's role-init's; the log:\n%s", ended, kills, h.logged())
			}
			h.tick()
			h.tickAfter(guestKillWait)
			if ended, kills := h.log.with("ended "), h.reported(); len(ended) != 0 || len(kills) != 0 {
				t.Fatalf("the watch wrote %q and reported %+v, want neither: rg's SIGKILL can be the tester's role-init's; the log:\n%s", ended, kills, h.logged())
			}
			if got, want := h.lines("killed unnamed"), unnamedLine(1, 1, 700*mib, gib, 16*mib); !slices.Equal(got, []string{want}) {
				t.Fatalf("the unnamed lines are %q, want %q alone; the log:\n%s", got, want, h.logged())
			}
		})
	}
}

// A departure that a pass sees after its status reads ends its own role's session alone:
// role-init's kill(-1), the renewer's Cancel and the warm-up's WaitDelay, which it can come just
// before, each reach the processes of that role and no other. The kernel kills the tester's agent,
// which shows the kill while its memory is freed. While the pass at the next scan waits on the
// status of the reviewer's cargo, the reviewer's shim leaves /proc, and then cargo's status shows
// the SIGKILL of the reviewer's role-init. That pass sets cargo aside and names the agent, to the
// tester, with the kernel's protected cause, at its size before the kill; the tester's shim then
// reaps the agent before the next pass, which leaves the kill named. The reviewer is told nothing,
// and no kill is left unnamed.
func TestGuestNamesAnotherRolesKillAtAPassThatSeesASessionLeaveAfterItsStatusReads(t *testing.T) {
	const legion = "/usr/local/bin/legion"
	const reviewerShim, cargo = 960, 961
	pod := newFakePod(t)
	pod.setSelfScore(0)
	classes := guestTree(pod, 0)
	pod.addGuest(
		guestProcess{fakeProcess: fakeProcess{pid: reviewerShim, ppid: 1, start: 9600, exe: legion, args: []string{"legion", "worker-shim"}, resident: 30 * mib}, score: 3},
		guestProcess{fakeProcess: fakeProcess{pid: cargo, ppid: reviewerShim, start: 9610, args: []string{"cargo"}, resident: 900 * mib}, score: 1380},
	)
	classes[reviewerShim], classes[cargo] = Class{Role: "reviewer", Protected: true, Session: true}, Class{Role: "reviewer"}
	cgroup := newFakeCgroup(t, cgroupV2)
	h := newGuestHarness(t, pod.proc, cgroup.dir, classes).run()

	// The kernel kills the tester's agent, which shows the kill while its memory is freed.
	showKill(pod, guestAgent, "omp", false)
	cgroup.setKills(1)
	// The reviewer's shim leaves, and the reviewer's role-init kills what the role left running.
	h.tickReadingStatus(pod, cargo, killedStatus(cargo, "cargo", false), func() { pod.remove(reviewerShim) })
	cargoAtThatPass := unshown
	if w := h.g.known[procKey{pid: cargo, start: 9610}]; w != nil {
		cargoAtThatPass = w.evidence
	}
	// The tester's shim reaps the agent before the next pass.
	pod.remove(guestAgent)
	h.tick()
	h.tickAfter(guestKillWait)
	kills := h.reported()
	if len(kills) != 1 || kills[0].PID != guestAgent || kills[0].Role != "tester" || kills[0].Sequence != 1 ||
		kills[0].Cause != shimwire.MemoryKillCauseKernelProtected || kills[0].SizeBytes != 2*gib {
		t.Fatalf("the sink took %+v, want the one kill of the tester's agent (%d) with the protected cause at its %d bytes before the kill: the reviewer's departure ends the reviewer's session alone; the log:\n%s",
			kills, guestAgent, 2*gib, h.logged())
	}
	if ended := h.log.with("ended "); len(ended) != 1 || !strings.HasSuffix(ended[0], `executable="omp"`) {
		t.Fatalf("the kill lines are %q, want the agent's alone; the log:\n%s", ended, h.logged())
	}
	if unnamed := h.lines("killed unnamed"); len(unnamed) != 0 {
		t.Fatalf("the watch logged %q, want no unnamed kill", unnamed)
	}
	if cargoAtThatPass != settled {
		t.Fatalf("cargo (%d) was %d after the pass that saw the reviewer's shim leave, want set aside at that pass (%d): its SIGKILL can be the reviewer's role-init's",
			cargo, cargoAtThatPass, settled)
	}
}

// role-init's kill(-1) lands up to its drain after the role's shim has left /proc, so a burst
// whose first rise comes within GuestConfig.SessionDrain after the watch saw a session process of
// a role leave names nothing of that role either. The tester's shim leaves at one scan, with no
// kill pending. A second later the counter rises while the drain's SIGKILL is pending in the
// ShdPnd of the tester's rg, and no other process shows the kill: rg is not named, and the kill has
// its unnamed line once the wait ends. Where the rise comes after the bound has passed, or the
// bound is zero, so only a departure during the wait counts, rg is named.
func TestGuestNamesNoProcessOfARoleWhoseShimLeftWithinTheSessionDrainBeforeTheRise(t *testing.T) {
	const rg = 935
	const drain = 3 * time.Second
	for _, tc := range []struct {
		name         string
		sessionDrain time.Duration
		after        time.Duration
		named        bool
	}{
		{"the rise comes within the bound", drain, time.Second, false},
		{"the rise comes after the bound", drain, drain, true},
		{"the bound is zero", 0, time.Second, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := newFakePod(t)
			pod.setSelfScore(0)
			classes := guestTree(pod, 0)
			pod.addGuest(command(rg, 9350, "rg", 1340, 50*mib))
			classes[rg] = Class{Role: "tester"}
			cgroup := newFakeCgroup(t, cgroupV2)
			h := newGuestHarness(t, pod.proc, cgroup.dir, classes)
			h.g.sessionDrain = tc.sessionDrain
			h.run()

			// The tester's agent has ended, and its shim leaves.
			pod.remove(guestAgent)
			pod.remove(guestShim)
			h.tick()
			// role-init's drain kills rg as the counter rises.
			showKill(pod, rg, "rg", false)
			cgroup.setKills(1)
			h.tickAfter(tc.after)
			h.tickAfter(guestKillWait)
			kills := h.reported()
			if !tc.named {
				if ended := h.log.with("ended "); len(ended) != 0 || len(kills) != 0 {
					t.Fatalf("the watch wrote %q and reported %+v, want neither: rg shows the SIGKILL of the tester's role-init; the log:\n%s", ended, kills, h.logged())
				}
				if got, want := h.lines("killed unnamed"), unnamedLine(1, 1, 700*mib, gib, 16*mib); !slices.Equal(got, []string{want}) {
					t.Fatalf("the unnamed lines are %q, want %q alone; the log:\n%s", got, want, h.logged())
				}
				return
			}
			if len(kills) != 1 || kills[0].PID != rg || kills[0].Role != "tester" {
				t.Fatalf("the sink took %+v, want the one kill of the tester's rg (%d); the log:\n%s", kills, rg, h.logged())
			}
			if unnamed := h.lines("killed unnamed"); len(unnamed) != 0 {
				t.Fatalf("the watch logged %q, want no unnamed kill", unnamed)
			}
		})
	}
}

// requireInotify skips a test of the wake where the kernel's file-modified event is not read: it
// is Linux's inotify (watchModified).
func requireInotify(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("the file-modified event the wake waits on is Linux's inotify")
	}
}

// On cgroup v2 the watch wakes at the kernel's file-modified event of memory.events, which the
// write of a rise of oom_kill raises, and names the process that shows the kernel's kill at once,
// with no scan after the rise: the tester's python3, still in /proc with SIGKILL pending while its
// memory is freed, has the guard's own line, and the tester one Kill, at the wake's time, with its
// size at the scan before and the cgroup's memory at the rise.
func TestGuestWakesAtTheFileModifiedEventOfMemoryEventsAndNamesTheKillWithNoScan(t *testing.T) {
	requireInotify(t)
	const python = 920
	pod := newFakePod(t)
	pod.setSelfScore(0)
	classes := guestTree(pod, 0)
	pod.addGuest(command(python, 9200, "python3", 1400, 1500*mib))
	classes[python] = Class{Role: "tester"}
	cgroup := newFakeCgroup(t, cgroupV2)
	h := newGuestHarness(t, pod.proc, cgroup.dir, classes)
	h.g.watchModified = watchModified
	h.run()
	if off := h.lines("wake off"); len(off) != 0 {
		t.Fatalf("the watch logged %q, want it to wake on memory.events", off)
	}

	cgroup.setUsage(3900 * mib)
	cgroup.setStat(fakeStat{anon: 3500 * mib, file: 400 * mib, inactiveFile: 120 * mib, shmem: 32 * mib})
	reaped(pod, python, 9200, "python3")
	cgroup.setKills(1)
	kills := h.awaitKills(1)
	wantKill := Kill{
		LauncherMemoryKill: shimwire.LauncherMemoryKill{
			Guard: h.g.ID(), Sequence: 1, PID: python, Executable: "python3", SizeBytes: 1500 * mib,
			ReadingBytes: 3500 * mib, CounterBytes: 3900 * mib, LimitBytes: podLimit, At: h.clock.Now(), Cause: shimwire.MemoryKillCauseKernel,
		},
		Role: "tester", Rule: "kernel", Sizes: "kernel",
	}
	if len(kills) != 1 || kills[0] != wantKill {
		t.Fatalf("the sink took %+v, want the one kill %+v; the log:\n%s", kills, wantKill, h.logged())
	}
	want := fmt.Sprintf(`launcher: memory guard: ended pid=%d role=tester size=%d reading=%d counter=%d shmem=%d limit=%d rule=kernel sizes=kernel less_parent=0 new_read=- executable="python3"`,
		python, 1500*mib, 3500*mib, 3900*mib, 32*mib, podLimit)
	if got := h.log.with("ended "); !slices.Equal(got, []string{want}) {
		t.Fatalf("the kill lines are %q, want %q; the log:\n%s", got, want, h.logged())
	}
}

// firedWatch sets h's watch of the file-modified event to one the test fires itself, and answers
// its channel: a send returns once the watch has taken the value, and so once the wake of the
// value before it has ended, and h.tick after a send returns once that send's wake has ended.
func firedWatch(h *guestHarness) chan<- struct{} {
	modified := make(chan struct{})
	h.g.watchModified = func(string) (*fileWatch, error) { return &fileWatch{modified: modified, stop: func() {}}, nil }
	return modified
}

// The kernel raises the counter just before it sends the SIGKILL, so the wake's first pass can
// read the victim before the signal. While the kill is pending the wake reads again after each
// pause of its schedule, 1, 2, 4, 8 and 16 ms. Where the tester's python3 shows the kill from the
// second read again on, the wake names it then, after pauses of 1 and 2 ms, with no scan after the
// rise. Where no process shows it, the wake pauses for the whole schedule and writes no unnamed
// line: the scan does, once the wait has passed.
func TestGuestWakeReadsAgainAfterShortPausesForAKillTheKernelHasNotSentYet(t *testing.T) {
	for _, tc := range []struct {
		name  string
		shows bool
	}{
		{"python3 shows the kill from the second read again on", true},
		{"no process shows the kill", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const python = 921
			pod := newFakePod(t)
			pod.setSelfScore(0)
			classes := guestTree(pod, 0)
			pod.addGuest(command(python, 9210, "python3", 1400, 1500*mib))
			classes[python] = Class{Role: "tester"}
			cgroup := newFakeCgroup(t, cgroupV2)
			h := newGuestHarness(t, pod.proc, cgroup.dir, classes)
			modified := firedWatch(h)
			// python3's status once the kernel's SIGKILL reaches it, which the second pause puts in
			// place of its status.
			status := filepath.Join(pod.proc, strconv.Itoa(python), "status")
			killed := status + ".killed"
			pod.writeWhole(killed, killedStatus(python, "python3", false))
			var mu sync.Mutex
			var pauses []time.Duration
			var sent error
			h.g.pause = func(d time.Duration) {
				mu.Lock()
				defer mu.Unlock()
				pauses = append(pauses, d)
				if tc.shows && len(pauses) == 2 {
					sent = os.Rename(killed, status)
				}
			}
			paused := func() []time.Duration {
				mu.Lock()
				defer mu.Unlock()
				return slices.Clone(pauses)
			}
			h.run()

			at := h.clock.Now()
			cgroup.setKills(1)
			modified <- struct{}{}
			// The watch takes a second value only once the wake of the first has ended.
			modified <- struct{}{}
			if tc.shows {
				kills := h.reported()
				if len(kills) != 1 || kills[0].PID != python || kills[0].Role != "tester" || kills[0].SizeBytes != 1500*mib ||
					!kills[0].At.Equal(at) {
					t.Fatalf("the sink took %+v, want the one kill of the tester's python3 (%d) at the wake; the log:\n%s", kills, python, h.logged())
				}
				mu.Lock()
				err := sent
				mu.Unlock()
				if err != nil {
					t.Fatal(err)
				}
				if got, want := paused(), []time.Duration{time.Millisecond, 2 * time.Millisecond}; !slices.Equal(got, want) {
					t.Fatalf("the wake paused %v, want %v: it names python3 at the read after the second pause", got, want)
				}
				return
			}
			schedule := []time.Duration{time.Millisecond, 2 * time.Millisecond, 4 * time.Millisecond, 8 * time.Millisecond, 16 * time.Millisecond}
			if got := paused(); !slices.Equal(got, schedule) {
				t.Fatalf("the wake paused %v, want %v; the log:\n%s", got, schedule, h.logged())
			}
			h.tick()
			if unnamed := h.lines("killed unnamed"); len(unnamed) != 0 {
				t.Fatalf("the watch logged %q before the wait passed, want no unnamed line yet", unnamed)
			}
			h.tickAfter(guestKillWait)
			if got, want := h.lines("killed unnamed"), unnamedLine(1, 1, 700*mib, gib, 16*mib); !slices.Equal(got, []string{want}) {
				t.Fatalf("the unnamed lines are %q, want %q alone; the log:\n%s", got, want, h.logged())
			}
			if ended, kills := h.log.with("ended "), h.reported(); len(ended) != 0 || len(kills) != 0 {
				t.Fatalf("the watch wrote %q and reported %+v, want neither", ended, kills)
			}
		})
	}
}

// A file-modified event of memory.events with no rise of oom_kill, as the kernel raises when the
// cgroup reaches its limit (max) and reclaims with no kill, costs the read of the counter alone.
// The wake scans no process, so the reviewer's rg, started since the last scan, keeps its score
// until the next scan raises it. It reads no process's status, so the tester's python3, which a
// role's own kill -9 has left in /proc with SIGKILL pending, is not marked as having shown a kill.
// It pauses for no read again, and it names nothing.
func TestGuestWakeWithNoRiseReadsTheCounterAlone(t *testing.T) {
	const python, rg = 922, 923
	pod := newFakePod(t)
	pod.setSelfScore(0)
	classes := guestTree(pod, 0)
	pod.addGuest(command(python, 9220, "python3", 1400, 1500*mib))
	classes[python], classes[rg] = Class{Role: "tester"}, Class{Role: "reviewer"}
	cgroup := newFakeCgroup(t, cgroupV2)
	h := newGuestHarness(t, pod.proc, cgroup.dir, classes)
	modified := firedWatch(h)
	var mu sync.Mutex
	var pauses []time.Duration
	h.g.pause = func(d time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		pauses = append(pauses, d)
	}
	h.run()

	pod.addGuest(guestProcess{fakeProcess: fakeProcess{pid: rg, ppid: 1, start: 9230, args: []string{"rg"}, resident: 50 * mib}, score: 1300})
	showKill(pod, python, "python3", false)
	cgroup.setEvents(42, 0)
	modified <- struct{}{}
	// The watch takes a second value only once the wake of the first has ended, with no scan
	// between.
	modified <- struct{}{}
	if got := pod.score(rg); got != 0 {
		t.Fatalf("rg (pid %d) reads oom_score_adj %d after the wake, want 0: a wake scans no process", rg, got)
	}
	mu.Lock()
	paused := slices.Clone(pauses)
	mu.Unlock()
	if len(paused) != 0 {
		t.Fatalf("the wake paused %v with no kill pending, want no pause", paused)
	}
	if ended, kills := h.log.with("ended "), h.reported(); len(ended) != 0 || len(kills) != 0 {
		t.Fatalf("the watch wrote %q and reported %+v with no rise, want neither; the log:\n%s", ended, kills, h.logged())
	}

	h.tick()
	if got := pod.score(rg); got != raisedScore {
		t.Fatalf("rg (pid %d) reads oom_score_adj %d after the next scan, want %d", rg, got, raisedScore)
	}
	if w := h.g.known[procKey{pid: python, start: 9220}]; w == nil || w.evidence != unshown {
		t.Fatalf("the watch holds python3 as %+v, want it held and not marked: no pass read its status", w)
	}
	if ended, unnamed := h.log.with("ended "), h.lines("killed unnamed"); len(ended) != 0 || len(unnamed) != 0 {
		t.Fatalf("the watch wrote %q and %q with no rise, want neither", ended, unnamed)
	}
}

// Where more processes show the kill at one pass than kills are pending, the pass reads the
// counter again, since the kernel may have killed again after the last read. At the wake of a rise
// of one no process shows the kill yet. In the first pause the kernel kills again: the counter
// reads two, and the tester's cc1 and ld both show SIGKILL pending. The pass after that pause
// reads two processes against one kill, reads the counter again, finds two, and names and settles
// neither, leaving both to the next pass as shown; the pass after the second pause names both,
// cc1 then ld, each for one kill at its size before, and no kill is left unnamed.
func TestGuestReadsTheCounterAgainWhereMoreProcessesShowTheKillThanArePending(t *testing.T) {
	const cc1, ld = 940, 941
	pod := newFakePod(t)
	pod.setSelfScore(0)
	classes := guestTree(pod, 0)
	pod.addGuest(command(cc1, 9400, "cc1", 1300, 300*mib), command(ld, 9410, "ld", 1200, 900*mib))
	classes[cc1], classes[ld] = Class{Role: "tester"}, Class{Role: "tester"}
	cgroup := newFakeCgroup(t, cgroupV2)
	h := newGuestHarness(t, pod.proc, cgroup.dir, classes)
	modified := firedWatch(h)
	// The pauses run on the watch's goroutine, so they write the pod with no t.Fatal, and read the
	// watch's state with no lock.
	var mu sync.Mutex
	var pauses []time.Duration
	var failed []error
	var shownAtSecond, settledAtSecond []int
	reportedAtSecond := -1
	h.g.pause = func(d time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		pauses = append(pauses, d)
		switch len(pauses) {
		case 1:
			for _, p := range []struct {
				pid  int
				name string
			}{{cc1, "cc1"}, {ld, "ld"}} {
				failed = append(failed, os.WriteFile(filepath.Join(pod.proc, strconv.Itoa(p.pid), "status"), []byte(killedStatus(p.pid, p.name, false)), 0o644))
			}
			failed = append(failed, os.WriteFile(cgroup.path("memory.events"),
				[]byte("low 0\nhigh 0\nmax 41\noom 3\noom_kill 2\noom_group_kill 0\n"), 0o644))
		case 2:
			for _, w := range h.g.known {
				switch w.evidence {
				case shown:
					shownAtSecond = append(shownAtSecond, w.key.pid)
				case settled:
					settledAtSecond = append(settledAtSecond, w.key.pid)
				}
			}
			reportedAtSecond = len(h.reported())
		}
	}
	h.run()

	cgroup.setKills(1)
	modified <- struct{}{}
	h.tick()
	mu.Lock()
	defer mu.Unlock()
	if err := errors.Join(failed...); err != nil {
		t.Fatal(err)
	}
	slices.Sort(shownAtSecond)
	if !slices.Equal(shownAtSecond, []int{cc1, ld}) || len(settledAtSecond) != 0 || reportedAtSecond != 0 {
		t.Fatalf("at the second pause the watch held %v shown, %v settled and had reported %d kills, want cc1 (%d) and ld (%d) shown and nothing settled or reported: the pass after the first pause read the counter again; the log:\n%s",
			shownAtSecond, settledAtSecond, reportedAtSecond, cc1, ld, h.logged())
	}
	if want := []time.Duration{time.Millisecond, 2 * time.Millisecond}; !slices.Equal(pauses, want) {
		t.Fatalf("the wake paused %v, want %v: it names both at the pass after the second pause", pauses, want)
	}
	kills := h.reported()
	if len(kills) != 2 || kills[0].PID != cc1 || kills[0].Sequence != 1 || kills[0].SizeBytes != 300*mib ||
		kills[1].PID != ld || kills[1].Sequence != 2 || kills[1].SizeBytes != 900*mib {
		t.Fatalf("the sink took %+v, want cc1 (%d) then ld (%d), each at its size before the kill; the log:\n%s", kills, cc1, ld, h.logged())
	}
	h.tickAfter(guestKillWait)
	if unnamed := h.lines("killed unnamed"); len(unnamed) != 0 {
		t.Fatalf("the watch logged %q, want no unnamed kill", unnamed)
	}
}

// The same at a scan, whose snapshot comes after its pass: that snapshot keeps what the pass left
// to the next, a zombie the scan leaves out among them, and reads no size again of a process a pass
// has shown. The kernel kills the tester's cc1, a zombie its parent has not reaped, and the counter
// reads one. While the pass at the next scan waits on the status of the tester's ld, whose memory
// the kernel has freed, the kernel kills ld too: the counter reads two, and ld's status shows
// SIGKILL pending. That pass reads two processes against one kill, reads the counter again, finds
// two, and names neither; the pass at the scan after it names both, cc1 then ld, each at its size
// at the last scan whose snapshot came before the pass that showed its kill, and no kill is left
// unnamed.
func TestGuestKeepsWhatAPassLeftAtARiseThroughTheScansSnapshot(t *testing.T) {
	const cc1, ld = 944, 945
	pod := newFakePod(t)
	pod.setSelfScore(0)
	classes := guestTree(pod, 0)
	pod.addGuest(command(cc1, 9440, "cc1", 1300, 300*mib), command(ld, 9450, "ld", 1200, 900*mib))
	classes[cc1], classes[ld] = Class{Role: "tester"}, Class{Role: "tester"}
	cgroup := newFakeCgroup(t, cgroupV2)
	h := newGuestHarness(t, pod.proc, cgroup.dir, classes).run()

	// The kernel kills cc1, a zombie its parent has not reaped, and frees ld's memory as it kills
	// ld, whose SIGKILL comes after the counter's second rise.
	zombie := command(cc1, 9440, "cc1", 0, 0)
	zombie.zombie = true
	pod.addGuest(zombie)
	showKill(pod, cc1, "cc1", true)
	if table, err := scan(pod.proc); err != nil || table.byPID[cc1] != nil {
		t.Fatalf("the scan reads the zombie %d (%v), want it left out", cc1, err)
	}
	pod.addGuest(command(ld, 9450, "ld", 0, 0))
	cgroup.setKills(1)
	h.tickReadingStatus(pod, ld, killedStatus(ld, "ld", false), func() { cgroup.setKills(2) })
	if ended, kills := h.log.with("ended "), h.reported(); len(ended) != 0 || len(kills) != 0 {
		t.Fatalf("the watch wrote %q and reported %+v at the pass whose second read of the counter rose, want neither; the log:\n%s", ended, kills, h.logged())
	}
	h.tick()
	kills := h.reported()
	if len(kills) != 2 || kills[0].PID != cc1 || kills[0].Sequence != 1 || kills[0].SizeBytes != 300*mib ||
		kills[1].PID != ld || kills[1].Sequence != 2 || kills[1].SizeBytes != 900*mib {
		t.Fatalf("the sink took %+v, want cc1 (%d) then ld (%d), each at its size before the kill; the log:\n%s", kills, cc1, ld, h.logged())
	}
	h.tickAfter(guestKillWait)
	if unnamed := h.lines("killed unnamed"); len(unnamed) != 0 {
		t.Fatalf("the watch logged %q, want no unnamed kill", unnamed)
	}
}

// A wake runs its evidence passes, and their pauses, once for each rise of the counter: at the
// first wake after the scan that read a rise, since the rise's own event can come just after that
// scan's read, and at a wake whose own read rose. Every other file-modified event while kills are
// pending, as the max and high events of a pod under pressure come, costs the read of the counter
// alone, and the scans go on reading. No process shows the kill here, so the rise a scan read and
// ten events with no rise after it pause for one whole schedule, 1, 2, 4, 8 and 16 ms, and a
// second rise, read at a wake, pauses for the schedule once more.
func TestGuestWakeRunsItsRereadsOnceForEachRiseOfTheCounter(t *testing.T) {
	pod := newFakePod(t)
	pod.setSelfScore(0)
	classes := guestTree(pod, 0)
	cgroup := newFakeCgroup(t, cgroupV2)
	h := newGuestHarness(t, pod.proc, cgroup.dir, classes)
	modified := firedWatch(h)
	var mu sync.Mutex
	var pauses []time.Duration
	h.g.pause = func(d time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		pauses = append(pauses, d)
	}
	paused := func() []time.Duration {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(pauses)
	}
	h.run()

	cgroup.setKills(1)
	h.tick()
	for i := uint64(0); i < 10; i++ {
		cgroup.setEvents(42+i, 1)
		modified <- struct{}{}
	}
	h.tick()
	schedule := []time.Duration{time.Millisecond, 2 * time.Millisecond, 4 * time.Millisecond, 8 * time.Millisecond, 16 * time.Millisecond}
	if got := paused(); !slices.Equal(got, schedule) {
		t.Fatalf("a rise a scan read and ten events with no rise paused %v, want the schedule %v once; the log:\n%s", got, schedule, h.logged())
	}
	cgroup.setKills(2)
	modified <- struct{}{}
	h.tick()
	if got, want := paused(), slices.Concat(schedule, schedule); !slices.Equal(got, want) {
		t.Fatalf("after a second rise read at a wake the wakes paused %v, want %v; the log:\n%s", got, want, h.logged())
	}
	h.tickAfter(guestKillWait)
	if got, want := h.lines("killed unnamed"), unnamedLine(2, 2, 700*mib, gib, 16*mib); !slices.Equal(got, []string{want}) {
		t.Fatalf("the unnamed lines are %q, want %q alone; the log:\n%s", got, want, h.logged())
	}
	if ended, kills := h.log.with("ended "), h.reported(); len(ended) != 0 || len(kills) != 0 {
		t.Fatalf("the watch wrote %q and reported %+v, want neither", ended, kills)
	}
}

// On cgroup v1, whose memory.oom_control raises no file-modified event, the watch starts no watch
// of one: it logs why once, and names a kill at the next scan, as ever.
func TestGuestOnCgroupV1LogsItsWakeOffOnceAndNamesTheKillAtTheNextScan(t *testing.T) {
	const python = 924
	pod := newFakePod(t)
	pod.setSelfScore(0)
	classes := guestTree(pod, 0)
	pod.addGuest(command(python, 9240, "python3", 1400, 1500*mib))
	classes[python] = Class{Role: "tester"}
	cgroup := newFakeCgroup(t, cgroupV1)
	h := newGuestHarness(t, pod.proc, cgroup.dir, classes)
	var mu sync.Mutex
	var watched []string
	h.g.watchModified = func(path string) (*fileWatch, error) {
		mu.Lock()
		watched = append(watched, path)
		mu.Unlock()
		return watchModified(path)
	}
	h.run()

	reaped(pod, python, 9240, "python3")
	cgroup.setKills(1)
	h.tick()
	at := h.clock.Now()
	if kills := h.reported(); len(kills) != 1 || kills[0].PID != python || kills[0].Role != "tester" || !kills[0].At.Equal(at) {
		t.Fatalf("the sink took %+v, want the one kill of the tester's python3 (%d) at the scan; the log:\n%s", kills, python, h.logged())
	}
	h.tick()
	h.tickAfter(guestKillWait)
	want := "launcher: guest memory: wake off reason=cgroup v1's memory.oom_control raises no file-modified event"
	if got := h.lines("wake off"); !slices.Equal(got, []string{want}) {
		t.Fatalf("the watch logged %q, want %q once; the log:\n%s", got, want, h.logged())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(watched) != 0 {
		t.Fatalf("the watch watched %q on cgroup v1, want no watch", watched)
	}
}

// On cgroup v2, where the watch of the file-modified event cannot start, or starts and later ends,
// the watch logs why once, with the error that ended the watch, and goes on with its scans, which
// name a kill at the next scan as ever.
func TestGuestWhoseWakeCannotStartOrEndsLogsWhyOnceAndNamesTheKillAtTheNextScan(t *testing.T) {
	for _, tc := range []struct {
		name   string
		start  error
		end    error
		reason func(c *fakeCgroup) string
	}{
		{"the watch cannot start", &os.SyscallError{Syscall: "inotify_init1", Err: syscall.EMFILE}, nil,
			func(*fakeCgroup) string { return "inotify_init1: too many open files" }},
		{"the watch ends", nil, &os.SyscallError{Syscall: "read", Err: syscall.EINVAL},
			func(c *fakeCgroup) string {
				return "the watch of " + c.path("memory.events") + " ended: read: invalid argument"
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const python = 925
			pod := newFakePod(t)
			pod.setSelfScore(0)
			classes := guestTree(pod, 0)
			pod.addGuest(command(python, 9250, "python3", 1400, 1500*mib))
			classes[python] = Class{Role: "tester"}
			cgroup := newFakeCgroup(t, cgroupV2)
			h := newGuestHarness(t, pod.proc, cgroup.dir, classes)
			modified := make(chan struct{})
			wakes := &fileWatch{modified: modified, stop: func() {}}
			h.g.watchModified = func(string) (*fileWatch, error) {
				if tc.start != nil {
					return nil, tc.start
				}
				return wakes, nil
			}
			h.run()
			if tc.start == nil {
				if off := h.lines("wake off"); len(off) != 0 {
					t.Fatalf("the watch logged %q while its watch ran", off)
				}
				// The watch's reader sets why it ended before it closes the channel.
				wakes.err = tc.end
				close(modified)
				if !testwait.Holds(testwait.Bounded(t, 5*time.Second, time.Second), time.Millisecond, func() bool { return len(h.lines("wake off")) > 0 }) {
					t.Fatalf("the watch logged no wake off line in 5 seconds after its watch ended; the log:\n%s", h.logged())
				}
			}

			reaped(pod, python, 9250, "python3")
			cgroup.setKills(1)
			h.tick()
			if kills := h.reported(); len(kills) != 1 || kills[0].PID != python || kills[0].Role != "tester" {
				t.Fatalf("the sink took %+v, want the one kill of the tester's python3 (%d); the log:\n%s", kills, python, h.logged())
			}
			h.tick()
			h.tickAfter(guestKillWait)
			want := "launcher: guest memory: wake off reason=" + tc.reason(cgroup)
			if got := h.lines("wake off"); !slices.Equal(got, []string{want}) {
				t.Fatalf("the watch logged %q, want %q once; the log:\n%s", got, want, h.logged())
			}
		})
	}
}

// The kernel's watch of a file's file-modified event takes a value at a write of the file in
// place, as a cgroup's counter changes, and ends, its channel closed, once the file goes away, with
// the kernel's drop of the watch (IN_IGNORED) as why, and once it is stopped, with the closed
// file's error. A file that is not there has no watch.
func TestWatchModifiedTakesAWriteInPlaceAndEndsWhenTheFileGoesOrItIsStopped(t *testing.T) {
	requireInotify(t)
	bound := testwait.Bounded(t, 5*time.Second, time.Second)
	closed := func(w *fileWatch, after string, why error) {
		t.Helper()
		deadline := time.After(bound)
		for {
			select {
			case _, open := <-w.modified:
				if !open {
					if !errors.Is(w.err, why) {
						t.Fatalf("the watch ended after %s with %v, want %v", after, w.err, why)
					}
					return
				}
			case <-deadline:
				t.Fatalf("the watch's channel is open 5 seconds after %s", after)
			}
		}
	}
	dir := t.TempDir()
	events := filepath.Join(dir, "memory.events")
	if err := os.WriteFile(events, []byte("oom_kill 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	w, err := watchModified(events)
	if err != nil {
		t.Fatal(err)
	}
	defer w.stop()
	if err := os.WriteFile(events, []byte("oom_kill 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case _, open := <-w.modified:
		if !open {
			t.Fatal("the watch ended at a write of its file")
		}
	case <-time.After(bound):
		t.Fatal("the watch took no value in 5 seconds after a write of its file")
	}
	if err := os.Remove(events); err != nil {
		t.Fatal(err)
	}
	closed(w, "its file went away", errWatchIgnored)

	other := filepath.Join(dir, "memory.oom.group")
	if err := os.WriteFile(other, []byte("0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stopped, err := watchModified(other)
	if err != nil {
		t.Fatal(err)
	}
	stopped.stop()
	closed(stopped, "it was stopped", os.ErrClosed)

	missing := filepath.Join(dir, "missing")
	if _, err := watchModified(missing); err == nil || err.Error() != "inotify_add_watch "+missing+": no such file or directory" {
		t.Fatalf("the watch of %s answers %v, want inotify_add_watch's no such file or directory", missing, err)
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
// v2's memory.oom.group and the starting score, and with a counter it reads, no other line but,
// on v1, whose memory.oom_control raises no file-modified event, the line of its wake off.
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
			want := []string{fmt.Sprintf("launcher: guest memory: start cgroup=%s dir=%s oom_group=%s score=0", tc.version, dir, tc.oomGroup)}
			if tc.version == cgroupV1 {
				want = append(want, "launcher: guest memory: wake off reason=cgroup v1's memory.oom_control raises no file-modified event")
			}
			if got := h.log.all(); !slices.Equal(got, want) {
				t.Fatalf("the watch logged %q, want %q", got, want)
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

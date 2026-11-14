package memoryguard

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

const (
	kib = uint64(1) << 10
	mib = uint64(1) << 20
	gib = uint64(1) << 30
	// podLimit is the fake container's limit. At the default act share of 85% its mark is
	// 3481.6 MiB, and at the default floor of 10% the floor is 409.6 MiB.
	podLimit = 4 * gib
)

// fakePod is a /proc and a memory cgroup directory under t.TempDir(), written the way the kernel
// writes them: meminfo, the limit and the counter in place, since the guard holds them open and
// reads them from their start, and each process's files whole.
type fakePod struct {
	t            *testing.T
	proc, cgroup string
	anon, shmem  uint64
}

func newFakePod(t *testing.T) *fakePod {
	t.Helper()
	root := t.TempDir()
	p := &fakePod{t: t, proc: filepath.Join(root, "proc"), cgroup: filepath.Join(root, "cgroup")}
	for _, dir := range []string{p.proc, p.cgroup} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	p.setLimit(strconv.FormatUint(podLimit, 10))
	p.setCounter(gib)
	p.setAnon(gib)
	return p
}

const meminfoFormat = "MemTotal:       20971520 kB\nMemFree:        10485760 kB\nMemAvailable:   12582912 kB\n" +
	"Buffers:               0 kB\nCached:          2097152 kB\nActive:          4194304 kB\nInactive:        1048576 kB\n" +
	"AnonPages:      %d kB\nMapped:          520372 kB\nShmem:          %d kB\nAnonHugePages:         0 kB\n"

func (p *fakePod) setAnon(anon uint64) {
	p.anon = anon
	p.writeInPlace(filepath.Join(p.proc, "meminfo"), fmt.Sprintf(meminfoFormat, p.anon/kib, p.shmem/kib))
}

func (p *fakePod) setShmem(shmem uint64) {
	p.shmem = shmem
	p.setAnon(p.anon)
}

func (p *fakePod) setLimit(text string) {
	p.writeInPlace(filepath.Join(p.cgroup, "memory.limit_in_bytes"), text+"\n")
}

func (p *fakePod) setCounter(n uint64) {
	p.writeInPlace(filepath.Join(p.cgroup, "memory.usage_in_bytes"), strconv.FormatUint(n, 10)+"\n")
}

func (p *fakePod) writeInPlace(path, content string) {
	p.t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		p.t.Fatal(err)
	}
}

// fakeProcess is one process of a fakePod. Its smaps holds its program's text, which adds
// nothing to its size, a heap of anon, a shared mapping of a file on disk of file (an LMDB map),
// a shared anonymous map of shared, a memfd of memfd, and a private mapping of a file under
// /dev/shm whose pages read no Anonymous, which adds nothing either.
type fakeProcess struct {
	pid, ppid int
	start     uint64
	name, exe string
	args      []string
	ns        string
	anon      uint64
	file      uint64
	shared    uint64
	memfd     uint64
	// unreadableExe makes exe a plain file, whose readlink fails as an unreadable link does.
	unreadableExe bool
}

func (p *fakePod) add(fps ...fakeProcess) {
	p.t.Helper()
	for _, fp := range fps {
		if fp.pid == os.Getpid() {
			p.t.Skipf("the test process's own pid %d is one this test fakes", fp.pid)
		}
		p.write(fp)
	}
}

// addSelf adds fp under the test process's own pid, the one the guard never ends.
func (p *fakePod) addSelf(fp fakeProcess) {
	fp.pid = os.Getpid()
	p.write(fp)
}

func (p *fakePod) write(fp fakeProcess) {
	p.t.Helper()
	if fp.start == 0 {
		fp.start = uint64(fp.pid) * 10
	}
	if fp.name == "" && len(fp.args) > 0 {
		fp.name = filepath.Base(fp.args[0])
	}
	if fp.exe == "" {
		fp.exe = "/usr/bin/" + fp.name
	}
	if fp.ns == "" {
		fp.ns = "pid:[4026531836]"
	}
	dir := filepath.Join(p.proc, strconv.Itoa(fp.pid))
	if err := os.MkdirAll(filepath.Join(dir, "ns"), 0o755); err != nil {
		p.t.Fatal(err)
	}
	p.writeWhole(filepath.Join(dir, "stat"), statLine(fp.pid, fp.name, fp.ppid, fp.start))
	cmdline := ""
	for _, arg := range fp.args {
		cmdline += arg + "\x00"
	}
	p.writeWhole(filepath.Join(dir, "cmdline"), cmdline)
	p.writeWhole(filepath.Join(dir, "smaps"), smapsOf(fp))
	for _, link := range []struct{ name, target string }{{"exe", fp.exe}, {filepath.Join("ns", "pid"), fp.ns}} {
		path := filepath.Join(dir, link.name)
		_ = os.Remove(path)
		if link.name == "exe" && fp.unreadableExe {
			p.writeWhole(path, "")
			continue
		}
		if err := os.Symlink(link.target, path); err != nil {
			p.t.Fatal(err)
		}
	}
}

// restart rewrites pid's stat with another start time, as a process that took the pid would
// read.
func (p *fakePod) restart(pid int, start uint64) {
	p.writeWhole(filepath.Join(p.proc, strconv.Itoa(pid), "stat"), statLine(pid, "reused", 1, start))
}

func (p *fakePod) remove(pid int) {
	p.t.Helper()
	if err := os.RemoveAll(filepath.Join(p.proc, strconv.Itoa(pid))); err != nil {
		p.t.Fatal(err)
	}
}

// writeWhole writes a process's file through a rename, so the guard's hourly scan, which reads
// on a goroutine of its own, never reads half of one.
func (p *fakePod) writeWhole(path, content string) {
	p.t.Helper()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		p.t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		p.t.Fatal(err)
	}
}

// statLine is a stat line of the kernel's shape: the start time is field 22.
func statLine(pid int, name string, ppid int, start uint64) string {
	return fmt.Sprintf("%d (%s) S %d %d %d 0 -1 4194304 100 0 0 0 5 3 0 0 20 0 1 0 %d 123456789 2048 "+
		"18446744073709551615 1 1 0 0 0 0 0 0 0 0 0 0 17 0 0 0 0 0 0\n", pid, name, ppid, pid, pid, start)
}

func smapsOf(fp fakeProcess) string {
	var b strings.Builder
	mapping := func(header string, rss, anon uint64) {
		fmt.Fprintf(&b, "%s\nSize:           %8d kB\nKernelPageSize:        4 kB\nMMUPageSize:           4 kB\n"+
			"Rss:            %8d kB\nPss:            %8d kB\nShared_Clean:          0 kB\nShared_Dirty:          0 kB\n"+
			"Private_Clean:         0 kB\nPrivate_Dirty:  %8d kB\nReferenced:     %8d kB\nAnonymous:      %8d kB\n"+
			"AnonHugePages:         0 kB\nSwap:                  0 kB\nLocked:                0 kB\nVmFlags: rd wr mr mw me ac\n",
			header, rss/kib, rss/kib, rss/kib, anon/kib, rss/kib, anon/kib)
	}
	mapping("00400000-00452000 r-xp 00000000 08:02 173521                             /usr/bin/"+fp.name, 300*kib, 0)
	if fp.anon > 0 {
		mapping("01a5c000-41a5c000 rw-p 00000000 00:00 0                                  [heap]", fp.anon, fp.anon)
	}
	if fp.file > 0 {
		mapping("7f0000000000-7f0100000000 rw-s 00000000 08:02 4242                       /legion/scratch/state.lmdb/data.mdb", fp.file, 0)
	}
	if fp.shared > 0 {
		mapping("7f2000000000-7f2100000000 rw-s 00000000 00:01 1025                       /dev/zero (deleted)", fp.shared, 0)
	}
	if fp.memfd > 0 {
		mapping("7f3000000000-7f3100000000 rw-s 00000000 00:01 2048                       /memfd:pool (deleted)", fp.memfd, 0)
	}
	mapping("7f4000000000-7f4000400000 rw-p 00000000 00:16 77                         /dev/shm/private", 4*mib, 0)
	return b.String()
}

// fakeClock hands each wait the guard asks for to the test, which fires it: a reading is then a
// step of the test, and an hour passes in one.
type fakeClock struct {
	mu    sync.Mutex
	now   time.Time
	waits chan fakeWait
}

type fakeWait struct {
	d    time.Duration
	fire chan time.Time
}

var testStart = time.Date(2026, 11, 14, 3, 0, 0, 0, time.UTC)

func newFakeClock() *fakeClock {
	return &fakeClock{now: testStart, waits: make(chan fakeWait, 16)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	w := fakeWait{d: d, fire: make(chan time.Time, 1)}
	c.waits <- w
	return w.fire
}

func (c *fakeClock) advance(d time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	return c.now
}

// fakeSignal is a kill that ends nothing: it records each process the guard opened, tried to kill
// and killed, and onOpen and onKill change the pod as a reused pid or a kill would, and killErr
// refuses a kill as the kernel would.
type fakeSignal struct {
	mu      sync.Mutex
	openErr error
	onOpen  func(pid int)
	// onKill changes the pod after pid's kill and answers whether pid has exited.
	onKill func(pid int) bool
	// killErr answers the error pid's kill fails with, nil for a kill that succeeds.
	killErr func(pid int) error
	tried   []int
	killed  []int
	open    int
	handles []*fakeHandle
}

type fakeHandle struct {
	s      *fakeSignal
	pid    int
	exited bool
	closed bool
}

func (s *fakeSignal) Open(pid int) (Handle, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.openErr != nil {
		return nil, s.openErr
	}
	s.open++
	if s.onOpen != nil && pid != os.Getpid() {
		s.onOpen(pid)
	}
	h := &fakeHandle{s: s, pid: pid}
	s.handles = append(s.handles, h)
	return h, nil
}

func (s *fakeSignal) kills() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.killed)
}

func (s *fakeSignal) attempts() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.tried)
}

func (s *fakeSignal) held() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.open
}

// exit marks pid as a process that has exited, as its pidfd would read.
func (s *fakeSignal) exit(pid int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, h := range s.handles {
		if h.pid == pid {
			h.exited = true
		}
	}
}

func (h *fakeHandle) Kill() error {
	h.s.mu.Lock()
	defer h.s.mu.Unlock()
	h.s.tried = append(h.s.tried, h.pid)
	if h.s.killErr != nil {
		if err := h.s.killErr(h.pid); err != nil {
			return err
		}
	}
	h.s.killed = append(h.s.killed, h.pid)
	if h.s.onKill != nil {
		h.exited = h.s.onKill(h.pid)
	}
	return nil
}

func (h *fakeHandle) Exited() bool {
	h.s.mu.Lock()
	defer h.s.mu.Unlock()
	return h.exited
}

func (h *fakeHandle) Close() error {
	h.s.mu.Lock()
	defer h.s.mu.Unlock()
	if !h.closed {
		h.closed = true
		h.s.open--
	}
	return nil
}

// lineLog keeps every line the guard logs.
type lineLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *lineLog) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *lineLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.lines)
}

// with is each line of the guard's that starts with what, after the prefix every line carries.
func (l *lineLog) with(what string) []string {
	var out []string
	for _, line := range l.all() {
		if strings.HasPrefix(line, linePrefix+what) {
			out = append(out, line)
		}
	}
	return out
}

// harness runs one guard over a fakePod with a fake clock and a fake kill, one reading at a time.
type harness struct {
	t       *testing.T
	pod     *fakePod
	clock   *fakeClock
	signal  *fakeSignal
	log     *lineLog
	g       *Guard
	mu      sync.Mutex
	kills   []Kill
	pending fakeWait
	cancel  context.CancelFunc
	done    chan struct{}
	stopped sync.Once
}

func newHarness(t *testing.T, pod *fakePod, classify Classifier) *harness {
	t.Helper()
	h := &harness{t: t, pod: pod, clock: newFakeClock(), signal: &fakeSignal{}, log: &lineLog{}}
	g, err := New(Config{
		Proc: pod.proc, Cgroup: pod.cgroup, Classify: classify, Logf: h.log.logf,
		Clock: h.clock, Signal: h.signal,
		Sink: func(k Kill) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.kills = append(h.kills, k)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	h.g = g
	return h
}

// run starts the guard and returns once its first reading, and all it led to, is done.
func (h *harness) run() *harness {
	h.start()
	h.pending = h.nextWait()
	return h
}

func (h *harness) start() {
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel, h.done = cancel, make(chan struct{})
	go func() {
		defer close(h.done)
		h.g.Run(ctx)
	}()
	h.t.Cleanup(h.stop)
}

// nextWait is the guard's next wait: the reading before it, and any scan that reading started,
// are done.
func (h *harness) nextWait() fakeWait {
	h.t.Helper()
	select {
	case w := <-h.clock.waits:
		h.g.scans.Wait()
		return w
	case <-h.done:
		h.t.Fatalf("Run returned before ctx ended; the log:\n%s", strings.Join(h.log.all(), "\n"))
	case <-time.After(testwait.Bounded(h.t, time.Minute, time.Second)):
		h.t.Fatalf("the guard never waited for its next reading; the log:\n%s", strings.Join(h.log.all(), "\n"))
	}
	return fakeWait{}
}

// tick lets the guard take its next reading, after the interval it waits for.
func (h *harness) tick() { h.tickAfter(0) }

// tickAfter lets the guard take its next reading extra after the interval it waits for, and
// returns once that reading and all it led to are done.
func (h *harness) tickAfter(extra time.Duration) {
	h.t.Helper()
	w := h.pending
	w.fire <- h.clock.advance(w.d + extra)
	h.pending = h.nextWait()
}

// stop ends Run and waits for it.
func (h *harness) stop() {
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

func (h *harness) killed() []Kill {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.kills)
}

// requireKilled fails unless the guard has killed exactly pids, in order, and reported each.
func (h *harness) requireKilled(pids ...int) {
	h.t.Helper()
	got := h.signal.kills()
	if !slices.Equal(got, pids) {
		h.t.Fatalf("the guard killed %v, want %v; the log:\n%s", got, pids, strings.Join(h.log.all(), "\n"))
	}
	reported := h.killed()
	if len(reported) != len(pids) {
		h.t.Fatalf("the guard reported %d kills, want %d", len(reported), len(pids))
	}
	for i, k := range reported {
		if k.PID != pids[i] || k.Sequence != uint64(i+1) || k.Guard != h.g.ID() {
			h.t.Fatalf("report %d is %+v, want pid %d, sequence %d, guard %s", i, k, pids[i], i+1, h.g.ID())
		}
	}
}

// protect is a classifier that protects the given pids, of role, and gives every other process
// role too.
func protect(role string, pids ...int) Classifier {
	return func(t *Table) map[int]Class {
		out := map[int]Class{}
		for _, p := range t.Processes() {
			out[p.PID] = Class{Role: role, Protected: slices.Contains(pids, p.PID)}
		}
		return out
	}
}

// exitFreeing is an onKill that frees freed of the reading and ends the process.
func exitFreeing(pod *fakePod, freed uint64) func(int) bool {
	return func(pid int) bool {
		pod.setAnon(pod.anon - freed)
		pod.remove(pid)
		return true
	}
}

// agent is a protected process that holds most of a pod's memory, as an agent does.
func agent(anon uint64) fakeProcess {
	return fakeProcess{pid: 100, ppid: 1, name: "omp", args: []string{"omp", "--mode", "rpc"}, anon: anon}
}

func TestMemoryGuardActsOnAnonPagesNotOnACounterAMappedFileRaises(t *testing.T) {
	pod := newFakePod(t)
	pod.add(agent(gib), fakeProcess{pid: 200, ppid: 100, args: []string{"python3", "load.py"}, anon: 600 * mib, file: 2560 * mib})
	// A mapped database has the counter past the mark while AnonPages is far below it.
	pod.setAnon(1700 * mib)
	pod.setCounter(3900 * mib)
	h := newHarness(t, pod, protect("tester", 100)).run()
	h.tick()
	h.tick()
	h.requireKilled()

	pod.add(fakeProcess{pid: 200, ppid: 100, args: []string{"python3", "load.py"}, anon: 2500 * mib, file: 2560 * mib})
	pod.setAnon(3600 * mib)
	h.tick()
	h.requireKilled(200)
}

func TestMemoryGuardBelowTheMarkEndsNothing(t *testing.T) {
	pod := newFakePod(t)
	pod.add(fakeProcess{pid: 200, ppid: 1, args: []string{"python3", "grow.py"}, anon: 3000 * mib})
	pod.setAnon(3481 * mib)
	h := newHarness(t, pod, nil).run()
	for range 5 {
		h.tick()
	}
	h.requireKilled()
	if lines := h.log.with("stopped"); len(lines) != 0 {
		t.Fatalf("below the mark the guard gave up a rise: %q", lines)
	}

	pod.setAnon(3482 * mib)
	h.tick()
	h.requireKilled(200)
	if lines := h.log.with("ended pid=200 role=none "); len(lines) != 1 {
		t.Fatalf("the kill of a process no classifier names is %q, want one line naming role none", h.log.with("ended"))
	}
}

func TestMemoryGuardRanksBySizeNotByRss(t *testing.T) {
	pod := newFakePod(t)
	pod.add(
		agent(gib),
		// The larger Rss, by a mapped file, and the smaller size.
		fakeProcess{pid: 300, ppid: 100, args: []string{"pivot", "track"}, anon: 600 * mib, file: 2 * gib},
		fakeProcess{pid: 301, ppid: 100, args: []string{"python3", "grow.py"}, anon: 900 * mib},
	)
	pod.setAnon(3600 * mib)
	h := newHarness(t, pod, protect("tester", 100)).run()
	h.requireKilled(301)
	if k := h.killed()[0]; k.SizeBytes != 900*mib {
		t.Fatalf("the killed process's size is %d, want %d (its Anonymous alone)", k.SizeBytes, 900*mib)
	}
}

func TestMemoryGuardSeesASharedAnonymousMapAndAMemfdAtTheirSize(t *testing.T) {
	pod := newFakePod(t)
	pod.add(
		agent(1536*mib),
		fakeProcess{pid: 400, ppid: 100, args: []string{"python3", "shared.py"}, shared: gib},
		fakeProcess{pid: 401, ppid: 100, args: []string{"python3", "memfd.py"}, memfd: 800 * mib},
		fakeProcess{pid: 402, ppid: 100, args: []string{"python3", "heap.py"}, anon: 300 * mib},
	)
	pod.setAnon(3600 * mib)
	h := newHarness(t, pod, protect("tester", 100))
	h.signal.onKill = exitFreeing(pod, gib)
	h.run()
	h.requireKilled(400)

	// The kill took the reading below the mark, so the next rise is acted on afresh.
	h.tick()
	h.signal.onKill = exitFreeing(pod, 800*mib)
	pod.setAnon(3600 * mib)
	h.tick()
	h.requireKilled(400, 401)
	kills := h.killed()
	if kills[0].SizeBytes != gib || kills[1].SizeBytes != 800*mib {
		t.Fatalf("the sizes are %d and %d, want the shared map's %d and the memfd's %d",
			kills[0].SizeBytes, kills[1].SizeBytes, gib, 800*mib)
	}
}

func TestMemoryGuardEndsNothingWhenWhatItMayEndIsUnderTheFloor(t *testing.T) {
	pod := newFakePod(t)
	pod.add(agent(3 * gib))
	for i := range 10 {
		pod.add(fakeProcess{pid: 500 + i, ppid: 100, args: []string{"sleep", "600"}, anon: 40 * mib})
	}
	pod.setAnon(3500 * mib)
	h := newHarness(t, pod, protect("tester", 100)).run()
	h.tick()
	h.tick()
	h.requireKilled()
	want := fmt.Sprintf("launcher: memory guard: stopped for this rise reason=nothing to end reading=%d endable=%d protected=%d unread=0 unheld=%d limit=%d",
		3500*mib, 400*mib, 3*gib, 28*mib, podLimit)
	if got := h.log.with("stopped"); !slices.Equal(got, []string{want}) {
		t.Fatalf("the stopped lines are %q, want one: %q", got, want)
	}
}

func TestMemoryGuardEndsTheNextLargestWhenAKillFallsAndTheReadingStaysAtTheMark(t *testing.T) {
	pod := newFakePod(t)
	pod.add(
		agent(3*gib),
		fakeProcess{pid: 600, ppid: 100, args: []string{"go", "test", "./..."}, anon: 600 * mib},
		fakeProcess{pid: 601, ppid: 100, args: []string{"python3", "grow.py"}, anon: 450 * mib},
	)
	pod.setAnon(4050 * mib)
	h := newHarness(t, pod, protect("tester", 100))
	// 550 MiB freed, and 3500 MiB is still above the mark.
	h.signal.onKill = exitFreeing(pod, 550*mib)
	h.run()
	h.requireKilled(600)
	h.tick()
	h.requireKilled(600, 601)
}

// A killed process whose memory has partly gone but which has not exited yet is waited on: the
// rest of its memory can take the reading below the mark, and a second kill then would end a
// process for nothing. Once it has exited, the guard ends the next largest.
func TestMemoryGuardWaitsForAKilledProcessToExitBeforeItEndsAnother(t *testing.T) {
	pod := newFakePod(t)
	pod.add(
		agent(3*gib),
		fakeProcess{pid: 610, ppid: 100, args: []string{"go", "test", "./..."}, anon: 600 * mib},
		fakeProcess{pid: 611, ppid: 100, args: []string{"python3", "grow.py"}, anon: 450 * mib},
	)
	pod.setAnon(4050 * mib)
	h := newHarness(t, pod, protect("tester", 100))
	h.signal.onKill = func(int) bool {
		pod.setAnon(pod.anon - 550*mib)
		return false
	}
	h.run()
	h.requireKilled(610)
	h.tick()
	h.tick()
	h.requireKilled(610)
	h.signal.exit(610)
	pod.remove(610)
	h.tick()
	h.requireKilled(610, 611)
}

// A pool of forked workers shares its parent's pages, so each worker's smaps reads the whole
// 262 MiB while its kill frees only the 4 MiB it wrote itself. That small fall is a fall: with the
// reading still at the mark, the guard ends the next worker, one at a time, until the reading is
// below the mark.
func TestMemoryGuardEndsTheNextForkedWorkerWhenAKillFreesLittle(t *testing.T) {
	pod := newFakePod(t)
	pod.add(
		agent(3214*mib),
		fakeProcess{pid: 700, ppid: 100, start: 5000, args: []string{"make", "-j4"}, anon: 262 * mib},
		fakeProcess{pid: 701, ppid: 700, start: 5100, args: []string{"make", "-j4"}, anon: 262 * mib},
		fakeProcess{pid: 702, ppid: 700, start: 5200, args: []string{"make", "-j4"}, anon: 262 * mib},
		fakeProcess{pid: 703, ppid: 700, start: 5300, args: []string{"make", "-j4"}, anon: 262 * mib},
	)
	// The agent, the pool's 262 MiB once, and the 4 MiB each worker wrote: 3488 MiB, above the
	// mark of 3481.6 MiB.
	pod.setAnon(3488 * mib)
	h := newHarness(t, pod, protect("tester", 100))
	h.signal.onKill = exitFreeing(pod, 4*mib)
	h.run()
	// Of the equal sizes, the newest worker first.
	h.requireKilled(703)
	// 3484 MiB: 4 MiB fallen and still at the mark, so the next worker.
	h.tick()
	h.requireKilled(703, 702)
	// 3480 MiB is below the mark: the rise is over.
	for range 3 {
		h.tick()
	}
	h.requireKilled(703, 702)
	if lines := h.log.with("stopped"); len(lines) != 0 {
		t.Fatalf("the guard gave up on a rise its kills lowered: %q", lines)
	}
}

// A kill that lowers the reading by nothing within the wait freed nothing: the killed child held
// only pages its parent keeps. The guard logs the stopped line once the wait passes and ends
// nothing more until the reading has gone below the mark.
func TestMemoryGuardStopsForARiseAKillDoesNotLower(t *testing.T) {
	pod := newFakePod(t)
	pod.add(
		agent(3230*mib),
		fakeProcess{pid: 700, ppid: 100, start: 5000, args: []string{"python3", "pool.py"}, anon: 262 * mib},
		fakeProcess{pid: 701, ppid: 700, start: 5100, args: []string{"python3", "pool.py"}, anon: 262 * mib},
		fakeProcess{pid: 702, ppid: 100, start: 5200, args: []string{"python3", "work.py"}, anon: 200 * mib},
	)
	pod.setAnon(3692 * mib)
	h := newHarness(t, pod, protect("tester", 100))
	h.signal.onKill = exitFreeing(pod, 0)
	h.run()
	h.requireKilled(701)
	for range 3 {
		h.tick()
	}
	h.requireKilled(701)
	if lines := h.log.with("stopped"); len(lines) != 0 {
		t.Fatalf("the guard gave up before its wait passed: %q", lines)
	}
	h.tickAfter(5 * time.Second)
	h.requireKilled(701)
	want := fmt.Sprintf("launcher: memory guard: stopped for this rise reason=not fallen reading=%d endable=%d protected=%d unread=0 unheld=%d limit=%d",
		3692*mib, 462*mib, 3230*mib, 0, podLimit)
	if got := h.log.with("stopped"); !slices.Equal(got, []string{want}) {
		t.Fatalf("the stopped lines are %q, want one: %q", got, want)
	}
	for range 3 {
		h.tick()
	}
	h.requireKilled(701)
}

func TestMemoryGuardActsOnANewRiseOnlyAfterTheReadingFellBelowTheMark(t *testing.T) {
	pod := newFakePod(t)
	pod.add(agent(3 * gib))
	for i := range 10 {
		pod.add(fakeProcess{pid: 500 + i, ppid: 100, args: []string{"sleep", "600"}, anon: 40 * mib})
	}
	pod.setAnon(3500 * mib)
	h := newHarness(t, pod, protect("tester", 100)).run()
	if len(h.log.with("stopped")) != 1 {
		t.Fatalf("the guard did not give the rise up: %q", h.log.all())
	}
	// Something it may end grows while the reading stays above the mark: still this rise.
	pod.add(fakeProcess{pid: 520, ppid: 100, args: []string{"python3", "grow.py"}, anon: 500 * mib})
	pod.setAnon(4000 * mib)
	h.tick()
	h.tick()
	h.requireKilled()

	pod.setAnon(3400 * mib)
	h.tick()
	h.requireKilled()
	pod.setAnon(4000 * mib)
	h.tick()
	h.requireKilled(520)
	if got := len(h.log.with("stopped")); got != 1 {
		t.Fatalf("%d stopped lines, want the first rise's alone", got)
	}
}

func TestMemoryGuardDoesNotKillAPidWhoseStartTimeChanged(t *testing.T) {
	pod := newFakePod(t)
	pod.add(agent(2600*mib), fakeProcess{pid: 800, ppid: 100, start: 7000, args: []string{"python3", "grow.py"}, anon: gib})
	pod.setAnon(3700 * mib)
	h := newHarness(t, pod, protect("tester", 100))
	h.signal.onOpen = func(pid int) { pod.restart(pid, 9999) }
	h.run()
	h.requireKilled()
	if lines := h.log.with("ended"); len(lines) != 0 {
		t.Fatalf("the guard logged a kill it did not send: %q", lines)
	}
	if held := h.signal.held(); held != 0 {
		t.Fatalf("the guard still holds %d processes after giving the kill up", held)
	}
}

func TestMemoryGuardParsesAStatWhoseNameHoldsSpacesAndAParenthesis(t *testing.T) {
	for _, tc := range []struct{ name string }{{"omp daemon brok"}, {"x) S 1 2 3 4 (y"}, {"a)"}} {
		p, err := parseStat([]byte(statLine(42, tc.name, 7, 4242)))
		if err != nil {
			t.Fatalf("parseStat of the name %q: %v", tc.name, err)
		}
		if p.Name != tc.name || p.PPID != 7 || p.StartTime != 4242 {
			t.Fatalf("parseStat of the name %q = %+v, want the name whole, parent 7 and start 4242", tc.name, *p)
		}
	}
	// And through the scan, where the classifier reads it.
	pod := newFakePod(t)
	pod.add(fakeProcess{pid: 900, ppid: 7, start: 4242, name: "x) S 1 2 3 4 (y", exe: "/usr/bin/x", args: []string{"x"}})
	table, err := scan(pod.proc)
	if err != nil {
		t.Fatal(err)
	}
	p, ok := table.Get(900)
	if !ok || p.Name != "x) S 1 2 3 4 (y" || p.PPID != 7 || p.StartTime != 4242 || p.Exe != "/usr/bin/x" ||
		p.PIDNamespace != "pid:[4026531836]" || !slices.Equal(p.Args, []string{"x"}) {
		t.Fatalf("the scan read %+v", p)
	}
	if kids := table.Children(7); len(kids) != 1 || kids[0].PID != 900 {
		t.Fatalf("the children of 7 are %v", kids)
	}
}

func TestMemoryGuardTurnsOffWithOneLineWhenItCannotRead(t *testing.T) {
	for _, tc := range []struct {
		name  string
		spoil func(pod *fakePod)
		want  string
	}{
		{"no AnonPages", func(pod *fakePod) {
			pod.writeInPlace(filepath.Join(pod.proc, "meminfo"), "MemTotal: 20971520 kB\nShmem: 32 kB\n")
		}, "off reason=no AnonPages line in %s/meminfo"},
		{"no limit", func(pod *fakePod) {
			if err := os.Remove(filepath.Join(pod.cgroup, "memory.limit_in_bytes")); err != nil {
				t.Fatal(err)
			}
		}, "off reason=no readable limit in %s/memory.limit_in_bytes: open "},
		{"a limit of no limit", func(pod *fakePod) {
			pod.setLimit("9223372036854771712")
		}, "off reason=the limit 9223372036854771712 in %s/memory.limit_in_bytes is no limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := newFakePod(t)
			pod.add(fakeProcess{pid: 200, ppid: 1, args: []string{"python3", "grow.py"}, anon: 3 * gib})
			pod.setAnon(3900 * mib)
			tc.spoil(pod)
			h := newHarness(t, pod, nil)
			h.start()
			testwait.Eventually(t, "the off line", func() bool { return len(h.log.all()) > 0 })
			h.stop()
			dir := pod.proc
			if tc.name != "no AnonPages" {
				dir = pod.cgroup
			}
			lines := h.log.all()
			if len(lines) != 1 || !strings.HasPrefix(lines[0], linePrefix+fmt.Sprintf(tc.want, dir)) {
				t.Fatalf("the log is %q, want one line starting %q", lines, linePrefix+fmt.Sprintf(tc.want, dir))
			}
			if len(h.clock.waits) != 0 {
				t.Fatalf("the guard waited for another reading after it turned off")
			}
			h.requireKilled()
		})
	}
}

func TestMemoryGuardTurnsOffWhenPidfdOpenIsUnsupported(t *testing.T) {
	pod := newFakePod(t)
	pod.setAnon(3900 * mib)
	h := newHarness(t, pod, nil)
	h.signal.openErr = os.NewSyscallError("pidfd_open", syscall.ENOSYS)
	h.start()
	testwait.Eventually(t, "the off line", func() bool { return len(h.log.all()) > 0 })
	h.stop()
	want := []string{"launcher: memory guard: off reason=pidfd_open is not supported here: pidfd_open: " + syscall.ENOSYS.Error()}
	if got := h.log.all(); !slices.Equal(got, want) {
		t.Fatalf("the log is %q, want %q", got, want)
	}
	h.g.Apply(&shimwire.LauncherMemoryGuard{ActPercent: 60, FloorPercent: 10, WaitSeconds: 5, IntervalMilliseconds: 100})
	if got := h.log.all(); len(got) != 1 {
		t.Fatalf("a guard that is off for good took settings: %q", got)
	}
}

func TestMemoryGuardTakesSettingsInPlace(t *testing.T) {
	pod := newFakePod(t)
	pod.add(agent(gib), fakeProcess{pid: 200, ppid: 100, args: []string{"python3", "grow.py"}, anon: 1500 * mib})
	// 63% of the limit: under the default mark, over a mark of 60%.
	pod.setAnon(2600 * mib)
	h := newHarness(t, pod, protect("tester", 100)).run()
	h.tick()
	h.requireKilled()
	if h.pending.d != 100*time.Millisecond {
		t.Fatalf("before any acknowledgement the guard waits %v, want the default 100ms", h.pending.d)
	}

	settings := &shimwire.LauncherMemoryGuard{ActPercent: 60, FloorPercent: 10, WaitSeconds: 5, IntervalMilliseconds: 200}
	h.g.Apply(settings)
	h.g.Apply(settings)
	want := []string{"launcher: memory guard: settings act_percent=60 floor_percent=10 wait=5s interval=200ms"}
	if got := h.log.with("settings"); !slices.Equal(got, want) {
		t.Fatalf("the settings lines are %q, want %q", got, want)
	}
	h.tick()
	h.requireKilled(200)
	if h.pending.d != 200*time.Millisecond {
		t.Fatalf("after the settings the guard waits %v, want 200ms", h.pending.d)
	}
	if got := len(h.log.with("started")); got != 1 {
		t.Fatalf("%d start lines, want 1", got)
	}
}

func TestMemoryGuardRefusesSettingsOutsideTheirRanges(t *testing.T) {
	pod := newFakePod(t)
	h := newHarness(t, pod, nil)
	h.g.Apply(&shimwire.LauncherMemoryGuard{ActPercent: 96, FloorPercent: 10, WaitSeconds: 5, IntervalMilliseconds: 100})
	want := []string{"launcher: memory guard: settings refused: shimwire: malformed frame: launcher_hello_ack memoryGuard.actPercent is 96, not between 50 and 95; nothing changes"}
	if got := h.log.all(); !slices.Equal(got, want) {
		t.Fatalf("the log is %q, want %q", got, want)
	}
	if s, on := h.g.current(); s != Defaults() || !on {
		t.Fatalf("after a refusal the guard runs by %+v, on %v; want the defaults, on", s, on)
	}
}

func TestMemoryGuardTurnsOffAndOnWithTheHourlyCountUnchanged(t *testing.T) {
	pod := newFakePod(t)
	pod.add(agent(gib), fakeProcess{pid: 200, ppid: 100, args: []string{"python3", "grow.py"}, anon: 2500 * mib})
	pod.setAnon(2000 * mib)
	h := newHarness(t, pod, protect("tester", 100)).run()

	h.g.Apply(nil)
	h.g.Apply(nil)
	if got := h.log.with("off"); !slices.Equal(got, []string{"launcher: memory guard: off: the daemon's acknowledgement carries no settings"}) {
		t.Fatalf("the off lines are %q, want one", got)
	}
	pod.setAnon(3700 * mib)
	h.tickAfter(10 * time.Minute)
	h.tick()
	h.requireKilled()

	h.g.Apply(&shimwire.LauncherMemoryGuard{ActPercent: 85, FloorPercent: 10, WaitSeconds: 5, IntervalMilliseconds: 100})
	if got := h.log.with("settings"); len(got) != 1 {
		t.Fatalf("the settings lines are %q, want the one that turned the guard on", got)
	}
	h.tick()
	h.requireKilled(200)

	pod.setAnon(2000 * mib)
	h.tickAfter(time.Hour)
	hours := h.log.with("hour")
	if len(hours) != 1 || !strings.HasPrefix(hours[0], "launcher: memory guard: hour start=2026-11-14T03:00:00Z end=2026-11-14T04:00:00Z ") {
		t.Fatalf("the hourly lines are %q, want one for the hour from the guard's start", hours)
	}
	if got := len(h.log.with("started")); got != 1 {
		t.Fatalf("%d start lines, want 1", got)
	}
}

func TestMemoryGuardHourlyLine(t *testing.T) {
	pod := newFakePod(t)
	python := fakeProcess{pid: 200, ppid: 100, args: []string{"python3", "-c", "work()"}, anon: 500 * mib}
	pod.add(agent(gib), python)
	pod.setAnon(2000 * mib)
	pod.setCounter(2560 * mib)
	h := newHarness(t, pod, protect("tester", 100)).run()

	python.anon = 1500 * mib
	pod.add(python)
	pod.setAnon(3000 * mib)
	pod.setCounter(3584 * mib)
	h.tickAfter(10 * time.Minute)
	highAt := h.clock.Now()

	pod.setAnon(1200 * mib)
	pod.setCounter(1536 * mib)
	h.tickAfter(10 * time.Minute)

	// The next hour: python has shrunk, and the agent is the largest.
	python.anon = 200 * mib
	pod.add(python)
	pod.setAnon(2200 * mib)
	pod.setCounter(2 * gib)
	h.tickAfter(45 * time.Minute)
	secondStart := h.clock.Now()
	if !secondStart.After(testStart.Add(time.Hour)) {
		t.Fatalf("the reading at %s is not in the second hour", secondStart)
	}

	pod.setAnon(2500 * mib)
	pod.setCounter(3 * gib)
	h.tickAfter(time.Hour)

	want := []string{
		fmt.Sprintf(`launcher: memory guard: hour start=2026-11-14T03:00:00Z end=2026-11-14T04:00:00Z reading_high=%d reading_high_at=%s reading_low=%d counter_high=%d counter_low=%d largest_pid=200 largest_role=tester largest_size=%d largest_command="python3 -c work()"`,
			3000*mib, highAt.Format(time.RFC3339), 1200*mib, 3584*mib, 1536*mib, 1500*mib),
		fmt.Sprintf(`launcher: memory guard: hour start=2026-11-14T04:00:00Z end=2026-11-14T05:00:00Z reading_high=%d reading_high_at=%s reading_low=%d counter_high=%d counter_low=%d largest_pid=100 largest_role=tester largest_size=%d largest_command="omp --mode rpc"`,
			2200*mib, secondStart.Format(time.RFC3339), 2200*mib, 2*gib, 2*gib, gib),
	}
	if got := h.log.with("hour"); !slices.Equal(got, want) {
		t.Fatalf("the hourly lines are\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestMemoryGuardLinesCarryTheirFieldsAndAnEscapedRedactedCutCommand(t *testing.T) {
	token := "ghp_" + strings.Repeat("A1", 18)
	pod := newFakePod(t)
	pod.add(agent(2*gib), fakeProcess{pid: 900, ppid: 100, anon: 1500 * mib, args: []string{
		"python3", "-c", "import os\nprint(1)", "\x1b[31mred", "--token", token, strings.Repeat("é", 400),
	}})
	pod.setShmem(64 * mib)
	pod.setAnon(3600 * mib)
	pod.setCounter(3900 * mib)
	h := newHarness(t, pod, protect("tester", 100)).run()
	h.requireKilled(900)

	started := fmt.Sprintf("launcher: memory guard: started act_percent=85 floor_percent=10 wait=5s interval=100ms limit=%d reading=%d", podLimit, 3600*mib)
	if got := h.log.with("started"); !slices.Equal(got, []string{started}) {
		t.Fatalf("the start lines are %q, want %q", got, started)
	}
	prefix := `python3 -c import os\nprint(1) \x1b[31mred --token ghp_*** `
	commandOf := func(max int) string {
		return prefix + strings.Repeat("é", (max-len(prefix)-len(ellipsis))/2) + ellipsis
	}
	ended := fmt.Sprintf(`launcher: memory guard: ended pid=900 role=tester size=%d reading=%d counter=%d shmem=%d limit=%d command="%s"`,
		1500*mib, 3600*mib, 3900*mib, 64*mib, podLimit, commandOf(logCommandBytes))
	if got := h.log.with("ended"); !slices.Equal(got, []string{ended}) {
		t.Fatalf("the kill lines are\n%q\nwant\n%q", got, ended)
	}
	k := h.killed()[0]
	if k.Command != commandOf(shimwire.MemoryKillCommandBytes) || len(k.Command) > shimwire.MemoryKillCommandBytes || !utf8.ValidString(k.Command) {
		t.Fatalf("the report's command is %q (%d bytes), want %q", k.Command, len(k.Command), commandOf(shimwire.MemoryKillCommandBytes))
	}
	want := Kill{Guard: h.g.ID(), Sequence: 1, PID: 900, Role: "tester", Command: k.Command, SizeBytes: 1500 * mib,
		ReadingBytes: 3600 * mib, CounterBytes: 3900 * mib, ShmemBytes: 64 * mib, LimitBytes: podLimit, At: testStart}
	if k != want {
		t.Fatalf("the report is %+v, want %+v", k, want)
	}
	if !regexp.MustCompile(`^[0-9a-f]{16}$`).MatchString(k.Guard) {
		t.Fatalf("the guard id is %q, want 16 hex digits", k.Guard)
	}
	report := shimwire.LauncherMemoryKill{Guard: k.Guard, Sequence: k.Sequence, PID: k.PID, Command: k.Command,
		SizeBytes: k.SizeBytes, ReadingBytes: k.ReadingBytes, CounterBytes: k.CounterBytes, LimitBytes: k.LimitBytes, At: k.At}
	if err := report.Validate(); err != nil {
		t.Fatalf("the kill does not make a valid report: %v", err)
	}
	for _, line := range h.log.all() {
		if strings.Contains(line, token) || strings.ContainsAny(line, "\n\x1b") {
			t.Fatalf("a line holds the token or a control character: %q", line)
		}
	}
}

func TestMemoryGuardHonoursTheClassifier(t *testing.T) {
	const roleInit, shim, omp, kernel, eval = 110, 111, 112, 113, 114
	pod := newFakePod(t)
	pod.add(
		fakeProcess{pid: roleInit, ppid: 1, exe: "/usr/local/bin/legion", args: []string{"legion", "role-init"}, ns: "pid:[4026532001]"},
		fakeProcess{pid: shim, ppid: roleInit, exe: "/usr/local/bin/legion", args: []string{"legion", "worker-shim"}, ns: "pid:[4026532001]"},
		// The agent holds the most memory of all, and is spared.
		fakeProcess{pid: omp, ppid: shim, exe: "/opt/omp/bin/omp", args: []string{"omp"}, ns: "pid:[4026532001]", anon: 2 * gib},
		fakeProcess{pid: kernel, ppid: omp, exe: "/usr/bin/python3", args: []string{"python3", "-m", "kernel"}, ns: "pid:[4026532001]", anon: 700 * mib},
		fakeProcess{pid: eval, ppid: omp, exe: "/usr/bin/bun", args: []string{"bun", "eval.js"}, ns: "pid:[4026532001]", anon: 450 * mib},
	)
	// The guard's own process, which no classifier names, is never ended.
	pod.addSelf(fakeProcess{ppid: 1, name: "legion", args: []string{"legion", "launcher"}, anon: 3 * gib})
	pod.setAnon(3800 * mib)
	// A classifier of the launcher's kind: the role by the role-init ancestor, the agent and
	// Legion's own processes protected by their place in the tree and their executable.
	classify := func(t *Table) map[int]Class {
		out := map[int]Class{}
		ri, ok := t.Get(roleInit)
		if !ok {
			return out
		}
		out[roleInit] = Class{Role: "tester", Protected: true}
		for _, s := range t.Children(ri.PID) {
			if s.Exe == ri.Exe && len(s.Args) > 1 && s.Args[1] == "worker-shim" {
				out[s.PID] = Class{Role: "tester", Protected: true}
				for _, a := range t.Children(s.PID) {
					out[a.PID] = Class{Role: "tester", Protected: true}
				}
				break
			}
		}
		for _, p := range t.Processes() {
			for q := p; q != nil; {
				if q.PID == roleInit {
					if _, named := out[p.PID]; !named {
						out[p.PID] = Class{Role: "tester"}
					}
					break
				}
				q, _ = t.Get(q.PPID)
			}
		}
		return out
	}
	h := newHarness(t, pod, classify)
	h.signal.onKill = exitFreeing(pod, 700*mib)
	h.run()
	h.requireKilled(kernel)
	if k := h.killed()[0]; k.Role != "tester" {
		t.Fatalf("the report's role is %q, want the role-init ancestor's", k.Role)
	}
	// The kill took the reading below the mark; the next rise is acted on afresh.
	h.tick()
	pod.setAnon(3800 * mib)
	h.tick()
	h.requireKilled(kernel, eval)
}

func TestMemoryGuardDoesNotEndAProcessItCannotRead(t *testing.T) {
	pod := newFakePod(t)
	pod.add(
		agent(gib),
		fakeProcess{pid: 950, ppid: 1, args: []string{"mystery"}, anon: 2 * gib, unreadableExe: true},
		fakeProcess{pid: 951, ppid: 100, args: []string{"python3", "grow.py"}, anon: 600 * mib},
	)
	pod.setAnon(3700 * mib)
	h := newHarness(t, pod, protect("tester", 100))
	h.signal.onKill = exitFreeing(pod, 600*mib)
	h.run()
	h.requireKilled(951)
	h.tick()
	pod.setAnon(3700 * mib)
	h.tick()
	h.requireKilled(951)
	unread := h.log.with("unread")
	if len(unread) != 1 || !strings.HasPrefix(unread[0], "launcher: memory guard: unread pid=950 reason=read the exe: ") {
		t.Fatalf("the unread lines are %q, want one for pid 950", unread)
	}
	want := fmt.Sprintf("launcher: memory guard: stopped for this rise reason=nothing to end reading=%d endable=0 protected=%d unread=%d unheld=%d limit=%d",
		3700*mib, gib, 2*gib, 3700*mib-3*gib, podLimit)
	if got := h.log.with("stopped"); !slices.Equal(got, []string{want}) {
		t.Fatalf("the stopped lines are %q, want %q", got, want)
	}
}

// A kill the kernel refuses for a reason other than the process having ended is logged once, and
// the process counts unread from then on: the guard ends the next largest at its next reading and
// never tries the refused process again, rather than picking it at every reading.
func TestMemoryGuardMovesToTheNextLargestWhenAKillFails(t *testing.T) {
	pod := newFakePod(t)
	pod.add(
		agent(2*gib),
		fakeProcess{pid: 1000, ppid: 100, args: []string{"python3", "held.py"}, anon: gib},
		fakeProcess{pid: 1001, ppid: 100, args: []string{"python3", "grow.py"}, anon: 600 * mib},
	)
	pod.setAnon(3800 * mib)
	h := newHarness(t, pod, protect("tester", 100))
	h.signal.killErr = func(pid int) error {
		if pid == 1000 {
			return os.NewSyscallError("pidfd_send_signal", syscall.EPERM)
		}
		return nil
	}
	h.signal.onKill = exitFreeing(pod, 600*mib)
	h.run()
	h.requireKilled()
	want := []string{"launcher: memory guard: kill failed pid=1000 reason=pidfd_send_signal: " + syscall.EPERM.Error()}
	if got := h.log.with("kill failed"); !slices.Equal(got, want) {
		t.Fatalf("the kill failed lines are %q, want %q", got, want)
	}
	if held := h.signal.held(); held != 0 {
		t.Fatalf("the guard still holds %d processes after a failed kill", held)
	}

	h.tick()
	h.requireKilled(1001)
	// The kill took the reading below the mark; the next rise finds nothing it may end but the
	// refused process, which counts under unread.
	h.tick()
	pod.setAnon(3800 * mib)
	h.tick()
	h.tick()
	h.requireKilled(1001)
	if got := h.signal.attempts(); !slices.Equal(got, []int{1000, 1001}) {
		t.Fatalf("the guard tried to kill %v, want 1000 once and then 1001", got)
	}
	if got := h.log.with("kill failed"); len(got) != 1 {
		t.Fatalf("the kill failed lines are %q, want one", got)
	}
	if got := h.log.with("unread"); len(got) != 0 {
		t.Fatalf("the refused process was logged unread as well: %q", got)
	}
	stopped := fmt.Sprintf("launcher: memory guard: stopped for this rise reason=nothing to end reading=%d endable=0 protected=%d unread=%d unheld=%d limit=%d",
		3800*mib, 2*gib, gib, 3800*mib-3*gib, podLimit)
	if got := h.log.with("stopped"); !slices.Equal(got, []string{stopped}) {
		t.Fatalf("the stopped lines are %q, want %q", got, stopped)
	}
}

func TestMemoryGuardReadingAllocatesNothing(t *testing.T) {
	pod := newFakePod(t)
	h := newHarness(t, pod, nil)
	if _, _, reason := h.g.sample(); reason != "" {
		t.Fatal(reason)
	}
	allocs := testing.AllocsPerRun(100, func() {
		if _, _, reason := h.g.sample(); reason != "" {
			t.Fatal(reason)
		}
	})
	if allocs != 0 {
		t.Fatalf("a reading allocates %v times, want none", allocs)
	}
}

// The pidfd kill itself, on a real process: it holds the process, ends it with SIGKILL, and says
// when it has exited.
func TestMemoryGuardPidfdKillEndsTheProcessItHolds(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the pidfd kill is Linux's")
	}
	cmd := exec.Command("sleep", "600")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	reaped := false
	t.Cleanup(func() {
		if !reaped {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	h, err := defaultSignal().Open(cmd.Process.Pid)
	if err != nil {
		t.Fatalf("pidfd_open: %v", err)
	}
	defer h.Close()
	if h.Exited() {
		t.Fatal("a running process reads as exited")
	}
	if err := h.Kill(); err != nil {
		t.Fatalf("pidfd_send_signal: %v", err)
	}
	testwait.Eventually(t, "the pidfd to read as exited", h.Exited)
	err = cmd.Wait()
	reaped = true
	if status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Fatalf("the process ended with %v, want SIGKILL", err)
	}
}

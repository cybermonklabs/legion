package memoryguard

import (
	"context"
	"errors"
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
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"golang.org/x/sys/unix"
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
// reads them from their start, and each process's files whole. Its files directory holds the files
// its processes' descriptors lead to, a memfd among them named the way procfs names one, with the
// directory standing for the root.
type fakePod struct {
	t                   *testing.T
	proc, cgroup, files string
	anon, shmem         uint64
}

func newFakePod(t *testing.T) *fakePod {
	t.Helper()
	root := t.TempDir()
	p := &fakePod{t: t, proc: filepath.Join(root, "proc"), cgroup: filepath.Join(root, "cgroup"), files: filepath.Join(root, "files")}
	for _, dir := range []string{p.proc, p.cgroup, p.files} {
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
// a shared anonymous map of shared, a memfd of memfd, a shared mapping of the pod's memfd mapped
// with mappedRss of it resident, and a private mapping of a file under /dev/shm whose pages read
// no Anonymous, which adds nothing either.
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
	// resident is its resident size, which its stat's rss field holds in pages: the size the guest
	// watch reads of it, where the guard reads its smaps.
	resident uint64
	// unreadableExe makes exe a plain file, whose readlink fails as an unreadable link does.
	unreadableExe bool
	// unreadableSmaps makes smaps a directory, whose read fails as a smaps the guard may not
	// read does.
	unreadableSmaps bool
	// zombie writes the process's stat in state Z: it has exited and its parent has not reaped it.
	zombie bool
	// fds are the files the process holds open by a descriptor, each a path under the pod's files
	// (fakePod.memfd, fakePod.plainFile), written as fd/<n> links from descriptor 3 on.
	fds []string
	// unreadableFDs makes fd a plain file, whose listing fails as an fd directory's the guard
	// may not read does.
	unreadableFDs bool
	mapped        *fakeMemfd
	mappedRss     uint64
	// threads, when above 0, writes a status whose Threads: line counts them, with the state its
	// stat has; a process left at 0 writes no status.
	threads int
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
	stat := statLine(fp.pid, fp.name, fp.ppid, fp.start, fp.resident/uint64(os.Getpagesize()))
	if fp.zombie {
		stat = strings.Replace(stat, ") S ", ") Z ", 1)
	}
	p.writeWhole(filepath.Join(dir, "stat"), stat)
	cmdline := ""
	for _, arg := range fp.args {
		cmdline += arg + "\x00"
	}
	p.writeWhole(filepath.Join(dir, "cmdline"), cmdline)
	if smaps := filepath.Join(dir, "smaps"); fp.unreadableSmaps {
		_ = os.RemoveAll(smaps)
		if err := os.MkdirAll(smaps, 0o755); err != nil {
			p.t.Fatal(err)
		}
	} else {
		p.writeWhole(smaps, smapsOf(fp))
	}
	if fp.threads > 0 {
		state := "S (sleeping)"
		if fp.zombie {
			state = "Z (zombie)"
		}
		p.writeWhole(filepath.Join(dir, "status"), fmt.Sprintf("Name:\t%s\nState:\t%s\nTgid:\t%d\nPid:\t%d\nPPid:\t%d\nThreads:\t%d\n",
			fp.name, state, fp.pid, fp.pid, fp.ppid, fp.threads))
	}
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
	fds := filepath.Join(dir, "fd")
	switch {
	case fp.unreadableFDs:
		_ = os.RemoveAll(fds)
		p.writeWhole(fds, "no directory of open files\n")
	case len(fp.fds) > 0:
		_ = os.RemoveAll(fds)
		if err := os.MkdirAll(fds, 0o755); err != nil {
			p.t.Fatal(err)
		}
		for i, target := range fp.fds {
			if err := os.Symlink(target, filepath.Join(fds, strconv.Itoa(3+i))); err != nil {
				p.t.Fatal(err)
			}
		}
	}
}

// fakeMemfd is a memfd of a fakePod: a file under the pod's files named the way procfs names a
// memfd, so that the link of a descriptor that holds it reads as a memfd's (fakePod.memfdLink),
// with the device and inode by which a smaps line of its mapping names it.
type fakeMemfd struct {
	name, path string
	dev, ino   uint64
}

// memfd makes a memfd of size bytes named name, a sparse file so that a large one takes no room.
func (p *fakePod) memfd(name string, size uint64) *fakeMemfd {
	p.t.Helper()
	path := p.memfdPath(name)
	p.sparseFile(path, size)
	info, err := os.Stat(path)
	if err != nil {
		p.t.Fatal(err)
	}
	st := info.Sys().(*syscall.Stat_t)
	return &fakeMemfd{name: name, path: path, dev: uint64(st.Dev), ino: uint64(st.Ino)}
}

// memfdPath is the path of the pod's memfd named name, whether or not it exists: procfs's
// `/memfd:<name> (deleted)` under the pod's files.
func (p *fakePod) memfdPath(name string) string { return p.memfdLink() + name + " (deleted)" }

// memfdLink is how the link of a descriptor that holds one of the pod's memfds starts: the
// guard's memfdPrefix under the pod's files, which stand for the root.
func (p *fakePod) memfdLink() string { return p.files + memfdPrefix }

// plainFile makes an ordinary file of size bytes named name under the pod's files, a sparse one,
// and answers its path.
func (p *fakePod) plainFile(name string, size uint64) string {
	p.t.Helper()
	path := filepath.Join(p.files, name)
	p.sparseFile(path, size)
	return path
}

func (p *fakePod) sparseFile(path string, size uint64) {
	p.t.Helper()
	f, err := os.Create(path)
	if err != nil {
		p.t.Fatal(err)
	}
	defer f.Close()
	if err := f.Truncate(int64(size)); err != nil {
		p.t.Fatal(err)
	}
}

// restart rewrites pid's stat with another start time, as a process that took the pid would
// read.
func (p *fakePod) restart(pid int, start uint64) {
	p.writeWhole(filepath.Join(p.proc, strconv.Itoa(pid), "stat"), statLine(pid, "reused", 1, start, 0))
}

func (p *fakePod) remove(pid int) {
	p.t.Helper()
	if err := os.RemoveAll(filepath.Join(p.proc, strconv.Itoa(pid))); err != nil {
		p.t.Fatal(err)
	}
}

// exitAsItsCmdlineIsRead has pid exit while a scan reads it, after the scan's read of its stat and
// before its read of its exe: pid's cmdline, which the scan reads between the two, becomes a named
// pipe, and once a reader has opened the pipe, pid's stat is replaced by a zombie's before the
// arguments are written and the pipe closed. The scan so reads a running process's stat first and
// a zombie's at any later read.
func (p *fakePod) exitAsItsCmdlineIsRead(pid int) {
	p.t.Helper()
	dir := filepath.Join(p.proc, strconv.Itoa(pid))
	stat, cmdline := filepath.Join(dir, "stat"), filepath.Join(dir, "cmdline")
	running, err := os.ReadFile(stat)
	if err != nil {
		p.t.Fatal(err)
	}
	zombie := stat + ".zombie"
	if err := os.WriteFile(zombie, []byte(strings.Replace(string(running), ") S ", ") Z ", 1)), 0o644); err != nil {
		p.t.Fatal(err)
	}
	args, err := os.ReadFile(cmdline)
	if err != nil {
		p.t.Fatal(err)
	}
	done := make(chan error, 1)
	opened := p.namedPipe(cmdline, func() {
		if err := <-done; err != nil {
			p.t.Errorf("the exit of pid %d as its cmdline was read: %v", pid, err)
		}
	})
	go func() {
		o := <-opened
		if o.err != nil {
			done <- o.err
			return
		}
		renamed := os.Rename(zombie, stat)
		_, written := o.writer.Write(args)
		done <- errors.Join(renamed, written, o.writer.Close())
	}()
}

// openedPipe is the writer of a named pipe (namedPipe), opened once a reader has opened the pipe,
// or the error of its open.
type openedPipe struct {
	writer *os.File
	err    error
}

// namedPipe replaces the file at path with a named pipe and answers the channel that takes its
// writer, opened on a goroutine of its own once a reader has opened the pipe; the caller writes and
// closes it. The writer waits in its open until a reader opens the pipe, so the test's cleanup lets
// go one that no reader met, with a reader that does not wait, which it holds open while await,
// where not nil, waits for what the caller does with the writer: what the caller writes then still
// has a reader.
func (p *fakePod) namedPipe(path string, await func()) <-chan openedPipe {
	p.t.Helper()
	if err := os.Remove(path); err != nil {
		p.t.Fatal(err)
	}
	if err := unix.Mkfifo(path, 0o644); err != nil {
		p.t.Fatal(err)
	}
	opened := make(chan openedPipe, 1)
	go func() {
		writer, err := os.OpenFile(path, os.O_WRONLY, 0)
		opened <- openedPipe{writer, err}
	}()
	p.t.Cleanup(func() {
		r, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
		if err == nil {
			defer r.Close()
		}
		if await != nil {
			await()
		}
	})
	return opened
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

// statLine is a stat line of the kernel's shape: the start time is field 22, and the resident
// pages, rss, field 24.
func statLine(pid int, name string, ppid int, start, rss uint64) string {
	return fmt.Sprintf("%d (%s) S %d %d %d 0 -1 4194304 100 0 0 0 5 3 0 0 20 0 1 0 %d 123456789 %d "+
		"18446744073709551615 1 1 0 0 0 0 0 0 0 0 0 0 17 0 0 0 0 0 0\n", pid, name, ppid, pid, pid, start, rss)
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
	if fp.mapped != nil {
		mapping(fmt.Sprintf("7f5000000000-7f5040000000 rw-s 00000000 %02x:%02x %-26d /memfd:%s (deleted)",
			unix.Major(fp.mapped.dev), unix.Minor(fp.mapped.dev), fp.mapped.ino, fp.mapped.name), fp.mappedRss, 0)
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
	// exited holds each pid exit has marked: each handle of it reads exited, those Open builds
	// later as well.
	exited map[int]bool
	// sent counts the kills sent, read without mu: a hook that onOpen waits on can read it while
	// Open holds mu (recordReads).
	sent atomic.Int32
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
	h := &fakeHandle{s: s, pid: pid, exited: s.exited[pid]}
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

// exit marks pid as a process that has exited, as its pidfd would read: each handle that holds it,
// and each that Open builds for it from then on.
func (s *fakeSignal) exit(pid int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.exited == nil {
		s.exited = map[int]bool{}
	}
	s.exited[pid] = true
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
	h.s.sent.Add(1)
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
	h.g = New(Config{
		Proc: pod.proc, Cgroup: pod.cgroup, Classify: classify, Logf: h.log.logf,
		Clock: h.clock, Signal: h.signal,
		Sink: func(k Kill) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.kills = append(h.kills, k)
		},
	})
	// The pod's descriptors lead to files under its files directory, which stands for the root.
	h.g.memfdLink = pod.memfdLink()
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

// resize writes fp again with anon of anonymous memory, as a process that grew reads.
func (p *fakePod) resize(fp *fakeProcess, anon uint64) {
	p.t.Helper()
	fp.anon = anon
	p.add(*fp)
}

// countReads has h's guard count the smaps it reads of each process (size), and answers how many
// it has read of a pid so far. It is set before the guard runs.
func countReads(h *harness) func(pid int) int {
	var mu sync.Mutex
	reads := map[int]int{}
	h.g.smapsRead = func(pid int) {
		mu.Lock()
		defer mu.Unlock()
		reads[pid]++
	}
	return func(pid int) int {
		mu.Lock()
		defer mu.Unlock()
		return reads[pid]
	}
}

// smapsReadAt is one smaps the guard read: its pid, how many kills the fake signal had sent when
// the guard read it, and whether the scan for the largest process read it (scanLargest) rather
// than the guard's loop (size).
type smapsReadAt struct {
	pid, killsBefore int
	scan             bool
}

// recordReads has h's guard record each smaps it reads, its loop's and its scans' for the largest
// process, in order, with the kills sent before it, and answers the records so far. It is set
// before the guard runs.
func recordReads(h *harness) func() []smapsReadAt {
	var mu sync.Mutex
	var reads []smapsReadAt
	record := func(scan bool) func(pid int) {
		return func(pid int) {
			killed := int(h.signal.sent.Load())
			mu.Lock()
			defer mu.Unlock()
			reads = append(reads, smapsReadAt{pid: pid, killsBefore: killed, scan: scan})
		}
	}
	h.g.smapsRead, h.g.scanSmapsRead = record(false), record(true)
	return func() []smapsReadAt {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(reads)
	}
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

// A process that writes a memfd it never maps holds that memfd's pages by its descriptor alone:
// its smaps shows none of them, and its end frees them all. The guard counts the memfd at its
// size in that process's size, so it ends that process before one with more Anonymous memory, and
// its kill line, its report and the hourly line all give that size.
func TestMemoryGuardEndsTheHolderOfAnUnmappedMemfdAtItsSize(t *testing.T) {
	pod := newFakePod(t)
	pool := pod.memfd("pool", gib)
	pod.add(
		agent(gib),
		fakeProcess{pid: 1100, ppid: 100, args: []string{"python3", "memfd.py"}, anon: 20 * mib, fds: []string{pool.path}},
		fakeProcess{pid: 1101, ppid: 100, args: []string{"python3", "grow.py"}, anon: 600 * mib},
	)
	// The memfd's written pages raise Shmem by its size.
	pod.setShmem(gib)
	pod.setAnon(3700 * mib)
	h := newHarness(t, pod, protect("tester", 100)).run()
	h.requireKilled(1100)
	size := gib + 20*mib
	if k := h.killed()[0]; k.SizeBytes != size {
		t.Fatalf("the killed process's size is %d, want %d (its Anonymous and the memfd it holds)", k.SizeBytes, size)
	}
	if got := h.log.with(fmt.Sprintf("ended pid=1100 role=tester size=%d ", size)); len(got) != 1 {
		t.Fatalf("the kill lines are %q, want one with size=%d", h.log.with("ended"), size)
	}

	pod.setAnon(2000 * mib)
	h.tickAfter(time.Hour)
	largest := fmt.Sprintf(" largest_pid=1100 largest_role=tester largest_size=%d ", size)
	if hours := h.log.with("hour"); len(hours) != 1 || !strings.Contains(hours[0], largest) {
		t.Fatalf("the hourly lines are %q, want one naming%s", hours, largest)
	}
}

// A memfd made large with ftruncate and never written has its size and no pages, so ending the
// process that holds it frees nothing. In a gVisor pod on 2026-11-14, the head's launcher, run as
// the container's first process, ended first a process that held a 16 GiB memfd made that way
// (`ended pid=5584 ... size=17185095680`, twice the pod's 8 GiB limit), then ended nothing more,
// and the kernel stopped the sandbox while a process beside it grew by about 1 GiB a second;
// without the holder, the guard ended that grower and the pod lived. Under gVisor a descriptor's
// stat gives no resident size (st_blocks reads the file's size) and lseek SEEK_DATA answers
// EINVAL, while the sandbox's Shmem reading counts the pages every memfd holds. So a memfd a
// descriptor holds counts at most that reading (W1 version 8): the grower, whose anonymous memory
// is larger than Shmem, is ended first, and the holder counts its Anonymous and the Shmem reading
// at most.
func TestMemoryGuardDoesNotPickTheHolderOfAnUnwrittenMemfdBeforeALargerProcess(t *testing.T) {
	pod := newFakePod(t)
	hole := pod.memfd("hole", 16*gib)
	pod.add(
		agent(3*gib),
		fakeProcess{pid: 1150, ppid: 100, args: []string{"python3", "sparse.py"}, anon: 20 * mib, fds: []string{hole.path}},
		fakeProcess{pid: 1151, ppid: 100, args: []string{"python3", "grow.py"}, anon: 600 * mib},
	)
	pod.setShmem(32 * mib)
	pod.setAnon(4050 * mib)
	h := newHarness(t, pod, protect("tester", 100))
	// 550 MiB freed, and 3500 MiB is still above the mark: the guard decides again on what is
	// left, the holder alone, which holds less than the floor.
	h.signal.onKill = exitFreeing(pod, 550*mib)
	h.run()
	h.requireKilled(1151)
	h.tick()
	h.requireKilled(1151)
	endable := fmt.Sprintf(" endable=%d ", 20*mib+32*mib)
	if stopped := h.log.with("stopped"); len(stopped) != 1 || !strings.Contains(stopped[0], endable) {
		t.Fatalf("the stopped lines are %q, want one with%s (the holder's Anonymous and the Shmem reading at most); the log:\n%s",
			stopped, endable, strings.Join(h.log.all(), "\n"))
	}
}

// The hourly line's scan for the largest process, on a goroutine of its own, counts a memfd a
// descriptor holds at most at the Shmem of the reading that started it, as an assessment does: a
// process that holds an unwritten memfd larger than the pod's limit is named at its Anonymous and
// that reading, and not at the memfd's size.
func TestMemoryGuardHourlyLineCountsAnUnwrittenMemfdAtMostAtShmem(t *testing.T) {
	pod := newFakePod(t)
	hole := pod.memfd("hole", 16*gib)
	pod.add(
		agent(gib),
		fakeProcess{pid: 1160, ppid: 100, args: []string{"python3", "sparse.py"}, anon: 1500 * mib, fds: []string{hole.path}},
	)
	pod.setShmem(32 * mib)
	pod.setAnon(2000 * mib)
	h := newHarness(t, pod, protect("tester", 100)).run()
	h.tickAfter(time.Hour)
	largest := fmt.Sprintf(" largest_pid=1160 largest_role=tester largest_size=%d ", 1500*mib+32*mib)
	if hours := h.log.with("hour"); len(hours) != 1 || !strings.Contains(hours[0], largest) {
		t.Fatalf("the hourly lines are %q, want one naming%s (its Anonymous and the Shmem reading at most)", hours, largest)
	}
}

// A memfd that a process maps counts through that mapping's Rss alone, whichever process holds it
// by a descriptor: a process that maps the memfd it holds counts it once, and a process that holds
// by a descriptor alone a memfd another process maps counts nothing for it, so neither is ranked
// above its due and the processes the guard may end sum to each memfd once.
func TestMemoryGuardCountsAMappedMemfdThroughItsMappingAlone(t *testing.T) {
	pod := newFakePod(t)
	own := pod.memfd("own", gib)
	theirs := pod.memfd("theirs", gib)
	pod.add(
		agent(2*gib),
		// Its Anonymous and its own memfd once, through its mapping: 1074 MiB.
		fakeProcess{pid: 1200, ppid: 100, args: []string{"python3", "shm.py"}, anon: 50 * mib, fds: []string{own.path}, mapped: own, mappedRss: gib},
		// Its Anonymous alone, 100 MiB: 1203 maps the memfd it holds.
		fakeProcess{pid: 1202, ppid: 100, args: []string{"python3", "reader.py"}, anon: 100 * mib, fds: []string{theirs.path}},
		// Its Anonymous and what of the memfd it maps is resident: 522 MiB.
		fakeProcess{pid: 1203, ppid: 100, args: []string{"python3", "writer.py"}, anon: 10 * mib, mapped: theirs, mappedRss: 512 * mib},
	)
	pod.setAnon(3800 * mib)
	h := newHarness(t, pod, protect("tester", 100)).run()
	h.requireKilled(1200)
	if k := h.killed()[0]; k.SizeBytes != gib+50*mib {
		t.Fatalf("the killed process's size is %d, want %d (its Anonymous and its memfd once)", k.SizeBytes, gib+50*mib)
	}
	// The kill lowers nothing, and the stop's line sums the sizes of the processes it may end.
	h.tickAfter(5 * time.Second)
	endable := gib + 50*mib + 100*mib + 522*mib
	want := fmt.Sprintf("launcher: memory guard: stopped for this rise reason=not fallen reading=%d endable=%d protected=%d unread=0 unheld=%d limit=%d left_out=0 unread_processes=none",
		3800*mib, endable, 2*gib, 3800*mib-endable-2*gib, podLimit)
	if got := h.log.with("stopped"); !slices.Equal(got, []string{want}) {
		t.Fatalf("the stopped lines are %q, want %q", got, want)
	}
}

// A memfd that two processes hold by a descriptor, and that none maps, counts once, in the size of
// the larger of the two by their sizes without it, however many of its descriptors hold it: the
// larger is ended first, at its size with the memfd, and the processes the guard may end sum to
// the memfd once.
func TestMemoryGuardCountsAMemfdTwoProcessesHoldOnceForTheLarger(t *testing.T) {
	pod := newFakePod(t)
	shared := pod.memfd("shared", gib)
	pod.add(
		agent(gib),
		fakeProcess{pid: 1300, ppid: 100, args: []string{"python3", "parent.py"}, anon: 300 * mib, fds: []string{shared.path, shared.path}},
		fakeProcess{pid: 1301, ppid: 1300, args: []string{"python3", "child.py"}, anon: 200 * mib, fds: []string{shared.path}},
		fakeProcess{pid: 1302, ppid: 100, args: []string{"python3", "grow.py"}, anon: 1100 * mib},
	)
	// The memfd's written pages raise Shmem by its size.
	pod.setShmem(gib)
	pod.setAnon(3700 * mib)
	h := newHarness(t, pod, protect("tester", 100)).run()
	h.requireKilled(1300)
	if k := h.killed()[0]; k.SizeBytes != gib+300*mib {
		t.Fatalf("the killed process's size is %d, want %d (its Anonymous and the memfd once)", k.SizeBytes, gib+300*mib)
	}
	h.tickAfter(5 * time.Second)
	endable := gib + 300*mib + 200*mib + 1100*mib
	want := fmt.Sprintf("launcher: memory guard: stopped for this rise reason=not fallen reading=%d endable=%d protected=%d unread=0 unheld=%d limit=%d left_out=0 unread_processes=none",
		3700*mib, endable, gib, 3700*mib-endable-gib, podLimit)
	if got := h.log.with("stopped"); !slices.Equal(got, []string{want}) {
		t.Fatalf("the stopped lines are %q, want %q", got, want)
	}
}

// A descriptor adds to a process's size only when it holds a memfd: one that holds an ordinary
// file adds nothing however large the file is, and a descriptor closed under the read (a memfd
// link that stat finds nothing at) or an fd directory the guard cannot list adds nothing either,
// and leaves its process read, ranked by its smaps, and not unread.
func TestMemoryGuardCountsNothingForAnOrdinaryFileOrADescriptorItCannotRead(t *testing.T) {
	pod := newFakePod(t)
	data := pod.plainFile("data.mdb", 2*gib)
	pod.add(
		agent(gib),
		fakeProcess{pid: 1400, ppid: 100, args: []string{"python3", "load.py"}, anon: 100 * mib, fds: []string{data, pod.memfdPath("closed")}},
		fakeProcess{pid: 1401, ppid: 100, args: []string{"python3", "grow.py"}, anon: 600 * mib},
		fakeProcess{pid: 1402, ppid: 100, args: []string{"python3", "hidden.py"}, anon: 300 * mib, unreadableFDs: true},
	)
	pod.setAnon(3700 * mib)
	h := newHarness(t, pod, protect("tester", 100)).run()
	h.requireKilled(1401)
	if k := h.killed()[0]; k.SizeBytes != 600*mib {
		t.Fatalf("the killed process's size is %d, want %d (its Anonymous alone)", k.SizeBytes, 600*mib)
	}
	h.tickAfter(5 * time.Second)
	if unread := h.log.with("unread"); len(unread) != 0 {
		t.Fatalf("the unread lines are %q, want none", unread)
	}
	endable := 100*mib + 600*mib + 300*mib
	want := fmt.Sprintf("launcher: memory guard: stopped for this rise reason=not fallen reading=%d endable=%d protected=%d unread=0 unheld=%d limit=%d left_out=0 unread_processes=none",
		3700*mib, endable, gib, 3700*mib-endable-gib, podLimit)
	if got := h.log.with("stopped"); !slices.Equal(got, []string{want}) {
		t.Fatalf("the stopped lines are %q, want %q", got, want)
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
	want := fmt.Sprintf("launcher: memory guard: stopped for this rise reason=nothing to end reading=%d endable=%d protected=%d unread=0 unheld=%d limit=%d left_out=0 unread_processes=none",
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
	want := fmt.Sprintf("launcher: memory guard: stopped for this rise reason=not fallen reading=%d endable=%d protected=%d unread=0 unheld=%d limit=%d left_out=0 unread_processes=none",
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

// Below the band, the 10% of the limit below the mark (3072 to 3481.6 MiB in the fake pod), the
// guard reads no size. At each reading in the band it reads the size of each process it may end
// once, and of no protected one, and keeps those reads by process for the window, at most one more
// than the readings the window holds; a reading below the band drops them. After a kill it reads
// the sizes at each reading of the wait.
func TestMemoryGuardReadsTheSizesAtEachReadingInTheBandAndAfterAKillAndNoneBelowTheBand(t *testing.T) {
	pod := newFakePod(t)
	pod.add(
		agent(2*gib),
		fakeProcess{pid: 300, ppid: 100, args: []string{"python3", "grow.py"}, anon: 500 * mib},
		fakeProcess{pid: 301, ppid: 100, args: []string{"python3", "idle.py"}, anon: 600 * mib},
	)
	pod.setAnon(3000 * mib)
	h := newHarness(t, pod, protect("tester", 100))
	readsOf := countReads(h)
	h.run()
	for range 3 {
		h.tick()
	}
	if got := readsOf(100) + readsOf(300) + readsOf(301); got != 0 {
		t.Fatalf("below the band the guard read %d sizes, want none", got)
	}

	pod.setAnon(3100 * mib)
	for i := range 5 {
		h.tick()
		if grower, idle, agent := readsOf(300), readsOf(301), readsOf(100); grower != i+1 || idle != i+1 || agent != 0 {
			t.Fatalf("after %d readings in the band the guard read pid 300 %d times, pid 301 %d and the agent %d; want %d, %d and none",
				i+1, grower, idle, agent, i+1, i+1)
		}
	}
	if len(h.g.sizes) != 2 {
		t.Fatalf("the guard keeps the reads of %d processes, want the 2 it may end", len(h.g.sizes))
	}
	for key, reads := range h.g.sizes {
		if most := int(growthWindow/Defaults().Interval) + 1; len(reads) > most {
			t.Fatalf("the guard keeps %d reads of pid %d, want at most %d, the window's", len(reads), key.pid, most)
		}
	}

	pod.setAnon(3000 * mib)
	h.tick()
	if got := readsOf(300); got != 5 {
		t.Fatalf("below the band the guard read pid 300 again (%d reads), want the band's 5", got)
	}
	if len(h.g.sizes) != 0 {
		t.Fatalf("below the band the guard keeps the reads of %d processes, want none", len(h.g.sizes))
	}

	// pid 301, the largest, is killed and has not exited: the wait goes on.
	pod.setAnon(3600 * mib)
	h.tick()
	h.requireKilled(301)
	for i := range 3 {
		h.tick()
		if grower, killed := readsOf(300), readsOf(301); grower != 6+i+1 || killed != 6+i+1 {
			t.Fatalf("at reading %d of the wait after the kill the guard has read pid 300 %d times and pid 301 %d, want %d each",
				i+1, grower, killed, 6+i+1)
		}
	}
	h.requireKilled(301)
}

// Once the guard has given a rise up it reads no size, however long the reading stays at or above
// the mark, and keeps none. The reading that ends the stopped phase, below the mark and in the
// band, is an armed guard's, and reads them again.
func TestMemoryGuardReadsNoSizeWhileARiseIsGivenUp(t *testing.T) {
	h := newHarness(t, underTheFloor(t), protect("tester", 100))
	readsOf := countReads(h)
	h.run()
	if got := h.log.with("stopped"); len(got) != 1 {
		t.Fatalf("the stopped lines are %q, want the rise given up with nothing to end", got)
	}
	endable, agent := readsOf(500), readsOf(100)
	for _, reading := range []uint64{3500, 3700, 3900, 3482} {
		h.pod.setAnon(reading * mib)
		h.tick()
	}
	if got, protected := readsOf(500), readsOf(100); got != endable || protected != agent {
		t.Fatalf("while the rise was given up the guard read pid 500 %d times and the agent %d, want %d and %d, the stop's alone",
			got, protected, endable, agent)
	}
	if len(h.g.sizes) != 0 {
		t.Fatalf("while the rise was given up the guard keeps the reads of %d processes, want none", len(h.g.sizes))
	}
	h.pod.setAnon(3400 * mib)
	h.tick()
	if got := readsOf(500); got != endable+1 {
		t.Fatalf("the reading of 3400 MiB that ended the stopped phase read pid 500 %d times in all, want %d", got, endable+1)
	}
}

// The first kill of a rise comes at its first reading at or above the mark, on the reads the band
// holds (INFRA-3531 T16): where one of the held processes grew by them, the guard sends it before
// it reads any smaps at that reading, its own or the hour's scan for the largest process's, and
// only then assesses the reading, whose sizes are every process's size as of the kill and the
// window's newest reads. Pid 301 grows 20 MiB between the two readings in the band and pid 300
// none, so by the band's reads pid 301 is the one that grew; pid 300 then grows 60 MiB as the
// reading reaches the mark, which a read there would rank first. The guard ends pid 301, at the
// size the band read, and reads pid 300's growth only after the kill. The reading at the mark is a
// new high of the hour 200 MiB above the high the last scan started at, more than 1% of the limit,
// so a scan starts at that reading, and it reads every process's smaps, the agent's among them,
// only after the kill. Where a scan runs as the guard opens its target, the rig lets it finish
// first, as a pod whose CPU the host throttles may, so a scan started before the kill shows all
// its reads before it.
func TestMemoryGuardEndsAtTheFirstReadingAtTheMarkOnTheBandsReadsBeforeItReadsAnySmaps(t *testing.T) {
	pod := newFakePod(t)
	grower := fakeProcess{pid: 300, ppid: 100, args: []string{"python3", "grow.py"}, anon: 400 * mib}
	idle := fakeProcess{pid: 301, ppid: 100, args: []string{"python3", "idle.py"}, anon: 580 * mib}
	pod.add(agent(2*gib), grower, idle)
	pod.setAnon(3100 * mib)
	h := newHarness(t, pod, protect("tester", 100))
	reads := recordReads(h)
	h.signal.onOpen = func(int) { h.g.scans.Wait() }
	h.run()
	pod.resize(&idle, 600*mib)
	pod.setAnon(3300 * mib)
	h.tick()
	h.requireKilled()

	inTheBand := len(reads())
	pod.resize(&grower, 460*mib)
	pod.setAnon(3500 * mib)
	h.tick()
	h.requireKilled(301)
	ended := fmt.Sprintf("ended pid=301 role=tester size=%d reading=%d ", 600*mib, 3500*mib)
	if got := h.log.with(ended); len(got) != 1 {
		t.Fatalf("the ended lines are %q, want one starting %q", h.log.with("ended"), linePrefix+ended)
	}
	if high := h.g.window.scannedHigh; high != 3500*mib {
		t.Fatalf("the last scan for the largest process started at the reading %d, want the one at the mark", high)
	}
	atTheMark := reads()[inTheBand:]
	var loop []smapsReadAt
	scanned := map[int]bool{}
	for _, r := range atTheMark {
		if r.killsBefore != 1 {
			t.Fatalf("at the reading at the mark the guard read the smaps %+v (each with the kills sent before it, and whether the scan read it), want none before the kill",
				atTheMark)
		}
		if r.scan {
			scanned[r.pid] = true
		} else {
			loop = append(loop, r)
		}
	}
	if want := []smapsReadAt{{pid: 300, killsBefore: 1}, {pid: 301, killsBefore: 1}}; !slices.Equal(loop, want) {
		t.Fatalf("at the reading at the mark the guard's loop read the smaps %+v, want %+v: each it may end once, after the kill", loop, want)
	}
	if !scanned[100] || !scanned[300] || !scanned[301] {
		t.Fatalf("at the reading at the mark the scan for the largest process read the smaps of %v, want the agent's and both others'", scanned)
	}
	now := h.clock.Now()
	for _, want := range []struct {
		key  procKey
		size uint64
	}{{procKey{pid: 300, start: 3000}, 460 * mib}, {procKey{pid: 301, start: 3010}, 600 * mib}} {
		if got, ok := h.g.atKill[want.key]; !ok || got.size != want.size || !got.seen.Equal(now) {
			t.Fatalf("the size as of the kill of pid %d is %+v (kept %t), want %d read at the reading at the mark", want.key.pid, got, ok, want.size)
		}
		if reads := h.g.sizes[want.key]; len(reads) == 0 || reads[len(reads)-1] != (sizeRead{at: now, size: want.size}) {
			t.Fatalf("the reads of pid %d are %+v, want the newest %d at the reading at the mark", want.key.pid, reads, want.size)
		}
	}
	if len(h.g.atKill) != 2 {
		t.Fatalf("the guard keeps the sizes as of the kill of %d processes, want the 2 it may end", len(h.g.atKill))
	}
}

// A read older than the window, 300 ms, counts for nothing where neither the window at the mark nor
// the band's own last window holds it. Pid 300 grows 100 MiB between the band's two readings. Read
// 300 ms before the reading at the mark, its first read is in the window at the mark, and pid 300
// ends first. Read 350 ms before it but 100 ms before the band's last reading, the window at the
// mark holds one read of each process while the band's own last window holds two, which then orders
// the held processes (orderHeld, INFRA-3531 T17), and pid 300 still ends first. Read 350 ms before
// the band's last reading, no window holds two reads and the first read counts for nothing: pid 300
// is read again at the mark, since it grew in the band, shows no growth over the window there, and
// the guard ends the larger pid 301.
func TestMemoryGuardCountsNothingForAReadOlderThanTheWindow(t *testing.T) {
	for _, tc := range []struct {
		name string
		// band is the time between the band's two readings, and mark the time from the second to
		// the reading at the mark.
		band, mark time.Duration
		want       int
	}{
		{"the first read 300 ms before the mark", 100 * time.Millisecond, 200 * time.Millisecond, 300},
		{"the first read 350 ms before the mark and in the band's last window", 100 * time.Millisecond, 250 * time.Millisecond, 300},
		{"the band's reads 350 ms apart", growthWindow + 50*time.Millisecond, 100 * time.Millisecond, 301},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := newFakePod(t)
			grower := fakeProcess{pid: 300, ppid: 100, args: []string{"python3", "grow.py"}, anon: 400 * mib}
			pod.add(agent(2*gib), grower, fakeProcess{pid: 301, ppid: 100, args: []string{"python3", "idle.py"}, anon: 600 * mib})
			pod.setAnon(3100 * mib)
			h := newHarness(t, pod, protect("tester", 100)).run()
			pod.resize(&grower, 500*mib)
			pod.setAnon(3300 * mib)
			h.tickAfter(tc.band - Defaults().Interval)
			pod.setAnon(3500 * mib)
			h.tickAfter(tc.mark - Defaults().Interval)
			h.requireKilled(tc.want)
		})
	}
}

// A step older than the window at the reading at the mark is no growth, though the band's last
// reading counted it: the guard reckons the order at the mark at that reading's own time, from each
// process's held reads (endHeld), and does not take it from the band. Pid 300 steps from 400 to 500
// MiB between the band's first two readings and holds there at the third, whose window still holds
// the first read, 200 ms back, and ranks pid 300 first. With the reading at the mark 300 ms after
// the first read, that read is in the window, pid 300 has grown by the held reads, and it ends
// first, at the 500 MiB the band read, with no smaps read before the kill. 350 ms after it, pid
// 300's oldest read in the window is its second, the window holds two reads of it, and by the held
// reads nothing grew, so the guard reads again at the mark the smaps of pid 300 alone, which grew
// between its band reads (rereadHeld): it has not grown there either. Pid 301, flat through its
// three band reads, is not read again, and the guard ends it, the larger, at the 600 MiB the band
// read, though it reads 596 MiB at the mark.
func TestMemoryGuardCountsAStepOlderThanTheWindowAtTheReadingAtTheMarkAsNoGrowth(t *testing.T) {
	for _, tc := range []struct {
		name string
		gap  time.Duration
		want int
		size uint64
		// reread are the pids whose smaps the guard reads before the kill.
		reread []int
	}{
		{"300 ms", growthWindow, 300, 500 * mib, nil},
		{"350 ms", growthWindow + 50*time.Millisecond, 301, 600 * mib, []int{300}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := newFakePod(t)
			grower := fakeProcess{pid: 300, ppid: 100, args: []string{"python3", "grow.py"}, anon: 400 * mib}
			idle := fakeProcess{pid: 301, ppid: 100, args: []string{"python3", "idle.py"}, anon: 600 * mib}
			pod.add(agent(2*gib), grower, idle)
			pod.setAnon(3100 * mib)
			h := newHarness(t, pod, protect("tester", 100))
			reads := recordReads(h)
			h.run()
			pod.resize(&grower, 500*mib)
			pod.setAnon(3200 * mib)
			h.tick()
			pod.setAnon(3300 * mib)
			h.tick()
			if held := h.g.held; len(held) != 2 || held[0].PID != 300 || held[0].growth != 100*mib {
				t.Fatalf("the band's last reading holds %+v, want pid 300 first, grown 100 MiB over its window", held)
			}
			inTheBand := len(reads())
			pod.resize(&idle, 596*mib)
			pod.setAnon(3500 * mib)
			// The first read, at the guard's start, is gap before the reading at the mark.
			h.tickAfter(tc.gap - 3*Defaults().Interval)
			h.requireKilled(tc.want)
			ended := fmt.Sprintf("ended pid=%d role=tester size=%d reading=%d ", tc.want, tc.size, 3500*mib)
			if got := h.log.with(ended); len(got) != 1 {
				t.Fatalf("the ended lines are %q, want one starting %q", h.log.with("ended"), linePrefix+ended)
			}
			if got := readBeforeTheKill(reads()[inTheBand:]); !slices.Equal(got, tc.reread) {
				t.Fatalf("before the kill the guard read the smaps of %v, want %v", got, tc.reread)
			}
		})
	}
}

// readBeforeTheKill is the pids of reads, a guard's smaps reads at one reading (recordReads), that
// came before its first kill, sorted, the hour's scan's among them.
func readBeforeTheKill(reads []smapsReadAt) []int {
	var pids []int
	for _, r := range reads {
		if r.killsBefore == 0 {
			pids = append(pids, r.pid)
		}
	}
	slices.Sort(pids)
	return pids
}

// The shared-page probe where the grower's step that takes the reading to the mark falls between
// the band's last reading and the reading at the mark, as the `shared` probe's did under gVisor at
// 4 GiB (INFRA-3531 T17): a parent holds a block of 1.5 GiB and three children it forked sleep,
// each reporting the block as its own, beside a grower that steps 64 MiB at a time. Its step before
// the crossing one comes 400 ms before the reading at the mark, older than the window, so by the
// held reads nothing grew and the largest, the newest fork, would end first, freeing nothing. The
// guard reads again the smaps of the grower alone, which grew between its band reads, and not
// those of the forks, flat through the band's five readings, nor the agent's; it sees the grower's
// crossing step and ends it, at the size it reads there, before it scans the process table or
// starts the hour's scan; it keeps one read of it at that reading.
func TestMemoryGuardEndsAGrowerWhoseStepToTheMarkComesAfterTheBandsLastReading(t *testing.T) {
	const block = 1536 * mib
	pod := newFakePod(t)
	pod.add(
		agent(1100*mib),
		fakeProcess{pid: 700, ppid: 100, start: 5000, args: []string{"python3", "pool.py"}, anon: block},
		fakeProcess{pid: 701, ppid: 700, start: 5100, args: []string{"python3", "pool.py"}, anon: block},
		fakeProcess{pid: 702, ppid: 700, start: 5200, args: []string{"python3", "pool.py"}, anon: block},
		fakeProcess{pid: 703, ppid: 700, start: 5300, args: []string{"python3", "pool.py"}, anon: block},
	)
	grower := fakeProcess{pid: 710, ppid: 100, start: 5400, args: []string{"python3", "grow.py"}, anon: 736 * mib}
	pod.add(grower)
	// The agent, the block once and the grower: 3372 MiB in the band, 3436 MiB after its step at
	// the first reading, and 3500 MiB at the mark after the crossing one.
	reading := func() uint64 { return 1100*mib + block + grower.anon }
	pod.setAnon(reading())
	h := newHarness(t, pod, protect("tester", 100))
	reads := recordReads(h)
	h.signal.onOpen = func(int) { h.g.scans.Wait() }
	h.run()
	for i := range 4 {
		if i == 0 {
			pod.resize(&grower, grower.anon+64*mib)
			pod.setAnon(reading())
		}
		h.tick()
	}
	h.requireKilled()
	if held := h.g.held; len(held) != 5 || held[0].PID != 703 || held[0].growth != 0 {
		t.Fatalf("the band's last reading holds %+v, want the five pool and grower processes, the newest fork first and none grown", held)
	}

	inTheBand := len(reads())
	pod.resize(&grower, grower.anon+64*mib)
	pod.setAnon(reading())
	h.tick()
	h.requireKilled(710)
	ended := fmt.Sprintf("ended pid=710 role=tester size=%d reading=%d ", 864*mib, 3500*mib)
	if got := h.log.with(ended); len(got) != 1 {
		t.Fatalf("the ended lines are %q, want one starting %q", h.log.with("ended"), linePrefix+ended)
	}
	for _, r := range reads()[inTheBand:] {
		if r.killsBefore == 0 && r.scan {
			t.Fatalf("the hour's scan read the smaps of pid %d before the kill", r.pid)
		}
	}
	if got, want := readBeforeTheKill(reads()[inTheBand:]), []int{710}; !slices.Equal(got, want) {
		t.Fatalf("before the kill the guard read the smaps of %v, want the grower's alone: %v", got, want)
	}
	now := h.clock.Now()
	if got := h.g.sizes[procKey{pid: 710, start: 5400}]; len(got) == 0 || got[len(got)-1] != (sizeRead{at: now, size: 864 * mib}) ||
		len(got) > 1 && got[len(got)-2].at.Equal(now) {
		t.Fatalf("the grower's reads are %+v, want one read of 864 MiB at the reading at the mark, the newest", got)
	}
}

// A held process the guard cannot read again at the mark is left out of the decision on the reads
// again (rereadHeld), which is among the held processes alone. Pids 301 and 302 start at the band's
// last reading, one band read each, so the guard reads both again; pid 303 sleeps through both band
// readings at 450 MiB and is not read again. Pid 302, the largest the band held, has gone by the
// reading at the mark, and pid 303's smaps reads 1200 MiB there, which an assessment of the reading
// would count as grown. No process has started since the band's last reading. The guard ends pid
// 301, the largest held process left, before it scans the process table: had it kept pid 302 at its
// held size, its kill would have failed and the assessment of the reading would have ended pid 303.
func TestMemoryGuardLeavesAHeldProcessGoneAtTheMarkOutOfTheDecisionOnTheReadsAgain(t *testing.T) {
	pod := newFakePod(t)
	flat := fakeProcess{pid: 303, ppid: 100, args: []string{"python3", "idle.py"}, anon: 450 * mib}
	pod.add(agent(2*gib), flat)
	pod.setAnon(3200 * mib)
	h := newHarness(t, pod, protect("tester", 100))
	reads := recordReads(h)
	h.run()
	pod.add(
		fakeProcess{pid: 301, ppid: 100, args: []string{"python3", "idle.py"}, anon: 500 * mib},
		fakeProcess{pid: 302, ppid: 100, args: []string{"python3", "pool.py"}, anon: 900 * mib},
	)
	pod.setAnon(3300 * mib)
	h.tick()
	if got := len(h.g.held); got != 3 {
		t.Fatalf("the band holds %d processes, want 3", got)
	}
	inTheBand := len(reads())
	pod.remove(302)
	pod.resize(&flat, 1200*mib)
	pod.setAnon(3500 * mib)
	h.tick()
	requireMarkKill(h, 301, 500*mib)
	if got, want := readBeforeTheKill(reads()[inTheBand:]), []int{301, 302}; !slices.Equal(got, want) {
		t.Fatalf("before the kill the guard read the smaps of %v, want those of the held processes it reads again alone: %v", got, want)
	}
}

// Where the window at the reading at the mark holds two held reads of no held process, while the
// band's own last window holds two of one, the guard orders the held processes over that last
// window (orderHeld, INFRA-3531 T17): throttled band readings about 200 ms apart leave one held read
// in the window at the mark, which would show no growth however much the band saw. Pid 710 grows 40
// MiB between the band's two readings, 200 ms apart, beside pid 700, a larger sleeper, and the
// reading at the mark comes 200 ms after the second. As it does, the rig rewrites both smaps so
// that a read there would name the sleeper: pid 710 back at 320 MiB and pid 700 at 1200 MiB. The
// guard ends pid 710 on its held reads, at the 340 MiB the band read, with no smaps read before the
// kill. With the band's readings 350 ms apart, no window holds two reads: the guard reads pid 710
// again, which grew in the band, finds it no larger, and ends the larger sleeper, which it does not
// read again, at the size the band read.
func TestMemoryGuardOrdersTheHeldReadsOverTheBandsLastWindowWhereTheWindowAtTheMarkHoldsOneReadOfEach(t *testing.T) {
	for _, tc := range []struct {
		name   string
		band   time.Duration
		want   int
		size   uint64
		reread []int
	}{
		{"band readings 200 ms apart", 200 * time.Millisecond, 710, 340 * mib, nil},
		{"band readings 350 ms apart", growthWindow + 50*time.Millisecond, 700, 900 * mib, []int{710}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := newFakePod(t)
			sleeper := fakeProcess{pid: 700, ppid: 100, args: []string{"python3", "pool.py"}, anon: 900 * mib}
			grower := fakeProcess{pid: 710, ppid: 100, args: []string{"python3", "grow.py"}, anon: 300 * mib}
			pod.add(agent(2*gib), sleeper, grower)
			pod.setAnon(3200 * mib)
			h := newHarness(t, pod, protect("tester", 100))
			reads := recordReads(h)
			h.run()
			pod.resize(&grower, 340*mib)
			pod.setAnon(3300 * mib)
			h.tickAfter(tc.band - Defaults().Interval)
			inTheBand := len(reads())
			pod.resize(&grower, 320*mib)
			pod.resize(&sleeper, 1200*mib)
			pod.setAnon(3500 * mib)
			h.tickAfter(200*time.Millisecond - Defaults().Interval)
			requireMarkKill(h, tc.want, tc.size)
			if got := readBeforeTheKill(reads()[inTheBand:]); !slices.Equal(got, tc.reread) {
				t.Fatalf("before the kill the guard read the smaps of %v, want %v", got, tc.reread)
			}
		})
	}
}

// Where no held process grew by the held reads, the guard reads again at the mark the smaps of the
// held processes that grew between two of their band reads of this stay in the band, or that have a
// single one, and keeps every other at its held size (rereadHeld). Pid 700 sleeps through the
// band's four readings; pid 710 grows 40 MiB between the first two and then holds; pid 720 starts
// at the band's last reading. As the reading reaches the mark pid 710 grows 50 MiB, pid 720 100
// MiB, and the rig rewrites pid 700's smaps 300 MiB larger, which a read there would rank first.
// The guard reads pids 710 and 720 again, not pid 700, and ends pid 720, the most grown of them.
// Where the reading has gone below the band between the band's readings, the stay starts again at
// the next reading in the band, where each of them has a single read: the guard reads all three
// again, and ends pid 700, which grew the most.
func TestMemoryGuardReadsAgainAtTheMarkOnlyTheHeldProcessesThatGrewInTheBandOrHaveOneBandRead(t *testing.T) {
	for _, tc := range []struct {
		name string
		// readings are the band's readings before the mark, in MiB, 100 ms apart.
		readings []uint64
		want     int
		size     uint64
		reread   []int
	}{
		{"one stay in the band", []uint64{3100, 3200, 3300, 3400}, 720, 300 * mib, []int{710, 720}},
		{"a stay that starts again below the band", []uint64{3100, 3200, 3000, 3400}, 700, 1200 * mib, []int{700, 710, 720}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := newFakePod(t)
			sleeper := fakeProcess{pid: 700, ppid: 100, args: []string{"python3", "pool.py"}, anon: 900 * mib}
			grown := fakeProcess{pid: 710, ppid: 100, args: []string{"python3", "grow.py"}, anon: 300 * mib}
			late := fakeProcess{pid: 720, ppid: 100, args: []string{"python3", "late.py"}, anon: 200 * mib}
			pod.add(agent(2*gib), sleeper, grown)
			pod.setAnon(tc.readings[0] * mib)
			h := newHarness(t, pod, protect("tester", 100))
			reads := recordReads(h)
			h.run()
			for i, reading := range tc.readings[1:] {
				switch i {
				case 0:
					pod.resize(&grown, 340*mib)
				case len(tc.readings) - 2:
					pod.add(late)
				}
				pod.setAnon(reading * mib)
				h.tick()
			}
			h.requireKilled()
			inTheBand := len(reads())
			pod.resize(&grown, 390*mib)
			pod.resize(&late, 300*mib)
			pod.resize(&sleeper, 1200*mib)
			pod.setAnon(3500 * mib)
			h.tick()
			requireMarkKill(h, tc.want, tc.size)
			if got := readBeforeTheKill(reads()[inTheBand:]); !slices.Equal(got, tc.reread) {
				t.Fatalf("before the kill the guard read the smaps of %v, want %v", got, tc.reread)
			}
		})
	}
}

// The guard's reads again at the mark (rereadHeld) take at most half the interval, 50 ms at the
// defaults, from the reading at the mark by its clock, which the rig moves on by some time at each
// smaps read there, as a pod whose CPU the host throttles does (INFRA-3531 T17, `4g-latepool-lmnt5`).
// Six growers each have one band read, so the guard would read all six again: it reads them the
// smallest held first, and once the budget has passed it stops after the read in hand and decides
// on what it read, the rest at their held sizes and no growth. Pids 805 to 800 are held at 100 up
// to 150 MiB, and as the reading reaches the mark they grow 10, 20, 30, 40, 50 and 200 MiB, the
// most grown read last. At 20 ms a read, it reads pids 805, 804 and 803 and ends pid 803, the most
// grown of those; at 60 ms a read, it reads pid 805 alone and ends it; where the reading at the
// mark itself took 80 ms, more than the budget, it still reads pid 805, the first, and ends it;
// where the reads take no time, the budget is never reached, it reads all six, and ends pid 800,
// the most grown, as with no budget.
func TestMemoryGuardReadsAgainAtTheMarkForHalfTheIntervalAndDecidesOnWhatItRead(t *testing.T) {
	growth := map[int]uint64{805: 10 * mib, 804: 20 * mib, 803: 30 * mib, 802: 40 * mib, 801: 50 * mib, 800: 200 * mib}
	for _, tc := range []struct {
		name string
		// mark is how long the reading at the mark takes before the reads again, and perRead how
		// long each smaps read at it takes, by the guard's clock.
		mark, perRead time.Duration
		reread        []int
		want          int
	}{
		{"20 ms a read", 0, 20 * time.Millisecond, []int{803, 804, 805}, 803},
		{"60 ms a read", 0, 60 * time.Millisecond, []int{805}, 805},
		{"the reading at the mark past the budget", 80 * time.Millisecond, 0, []int{805}, 805},
		{"no time a read", 0, 0, []int{800, 801, 802, 803, 804, 805}, 800},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := newFakePod(t)
			pod.add(agent(2 * gib))
			held := map[int]uint64{}
			growers := map[int]*fakeProcess{}
			for i := range 6 {
				pid := 800 + i
				held[pid] = (150 - 10*uint64(i)) * mib
				growers[pid] = &fakeProcess{pid: pid, ppid: 100, start: uint64(6000 + i), args: []string{"python3", "-c", "grow"}, anon: held[pid]}
				pod.add(*growers[pid])
			}
			pod.setAnon(3300 * mib)
			h := newHarness(t, pod, protect("tester", 100))
			reads := recordReads(h)
			atMark, perRead := slowMark(h)
			h.run()
			inTheBand := len(reads())
			for pid, g := range growers {
				pod.resize(g, g.anon+growth[pid])
			}
			pod.setAnon(3500 * mib)
			atMark.Store(int64(tc.mark))
			perRead.Store(int64(tc.perRead))
			h.tick()
			requireMarkKill(h, tc.want, held[tc.want]+growth[tc.want])
			if got := readBeforeTheKill(reads()[inTheBand:]); !slices.Equal(got, tc.reread) {
				t.Fatalf("before the kill the guard read the smaps of %v, want %v", got, tc.reread)
			}
		})
	}
}

// slowMark has h's guard's clock move on, at each reading, by the first answer's duration, before
// the guard decides on it (its observe), and at each smaps it reads, by the second's (its
// smapsRead, after any set before it): a reading and reads that take that long, as on a pod whose
// CPU the host throttles. Both start at none. It is set before the guard runs.
func slowMark(h *harness) (atMark, perRead *atomic.Int64) {
	atMark, perRead = &atomic.Int64{}, &atomic.Int64{}
	record := h.g.smapsRead
	h.g.smapsRead = func(pid int) {
		if record != nil {
			record(pid)
		}
		h.clock.advance(time.Duration(perRead.Load()))
	}
	h.g.observe = func(time.Time, uint64, uint64) { h.clock.advance(time.Duration(atMark.Load())) }
	return atMark, perRead
}

// Of the held processes with one band read, the reads again at the mark (rereadHeld) take the
// smallest first: a fresh grower starts small, while a fresh fork under gVisor reports its parent's
// shared pages as its own and reads large, and a read of it changes no answer. A parent holds a
// block of 1.5 GiB through the band's two readings; at the second, it has forked pid 701, which
// reports the block as its own, and pid 710, older, has started at 50 MiB. Each has that one band
// read. As the reading reaches the mark pid 710 grows 64 MiB, and the reads take 60 ms each, so the
// budget allows one: the guard reads pid 710 alone and ends it.
func TestMemoryGuardReadsAgainAtTheMarkASmallFreshGrowerBeforeAFreshForkOfALargeBlock(t *testing.T) {
	const block = 1536 * mib
	pod := newFakePod(t)
	pod.add(agent(gib), fakeProcess{pid: 700, ppid: 100, start: 5000, args: []string{"python3", "pool.py"}, anon: block})
	pod.setAnon(3200 * mib)
	h := newHarness(t, pod, protect("tester", 100))
	reads := recordReads(h)
	_, perRead := slowMark(h)
	h.run()
	grower := fakeProcess{pid: 710, ppid: 100, start: 5050, args: []string{"python3", "grow.py"}, anon: 50 * mib}
	pod.add(fakeProcess{pid: 701, ppid: 700, start: 5100, args: []string{"python3", "pool.py"}, anon: block}, grower)
	pod.setAnon(3300 * mib)
	h.tick()
	inTheBand := len(reads())
	pod.resize(&grower, grower.anon+64*mib)
	pod.setAnon(3500 * mib)
	perRead.Store(int64(60 * time.Millisecond))
	h.tick()
	requireMarkKill(h, 710, 114*mib)
	if got, want := readBeforeTheKill(reads()[inTheBand:]), []int{710}; !slices.Equal(got, want) {
		t.Fatalf("before the kill the guard read the smaps of %v, want the small grower's alone: %v", got, want)
	}
}

// The reads again at the mark (rereadHeld) take the held processes in a fixed order: first those
// that grew in the band, the most grown across the stay first, then those with one band read, the
// smallest held first and the newest by start time among equal sizes; one flat through two band
// reads or more is not read. Pid 710 grows 50 MiB and pid 711 20 MiB between the band's first two
// readings, and pid 700 sleeps through all four; pid 722, of 200 MiB, and pids 720 and 721, both of
// 300 MiB, pid 721 the newer, start at the band's last reading. Nothing grows at the mark, and the
// guard reads pids 710, 711, 722, 721 and 720 in that order, and ends pid 700, the largest.
func TestMemoryGuardReadsAgainAtTheMarkTheBandsGrowersFirstThenTheSingleReadsSmallestFirst(t *testing.T) {
	pod := newFakePod(t)
	most := fakeProcess{pid: 710, ppid: 100, args: []string{"python3", "grow.py"}, anon: 300 * mib}
	less := fakeProcess{pid: 711, ppid: 100, args: []string{"python3", "grow.py"}, anon: 300 * mib}
	pod.add(agent(2*gib), most, less, fakeProcess{pid: 700, ppid: 100, args: []string{"python3", "pool.py"}, anon: 900 * mib})
	pod.setAnon(3100 * mib)
	h := newHarness(t, pod, protect("tester", 100))
	reads := recordReads(h)
	h.run()
	pod.resize(&most, 350*mib)
	pod.resize(&less, 320*mib)
	pod.setAnon(3200 * mib)
	h.tick()
	pod.setAnon(3300 * mib)
	h.tick()
	pod.add(
		fakeProcess{pid: 720, ppid: 100, start: 7000, args: []string{"python3", "late.py"}, anon: 300 * mib},
		fakeProcess{pid: 721, ppid: 100, start: 7100, args: []string{"python3", "late.py"}, anon: 300 * mib},
		fakeProcess{pid: 722, ppid: 100, start: 6900, args: []string{"python3", "late.py"}, anon: 200 * mib},
	)
	pod.setAnon(3400 * mib)
	h.tick()
	inTheBand := len(reads())
	pod.setAnon(3500 * mib)
	h.tick()
	requireMarkKill(h, 700, 900*mib)
	var order []int
	for _, r := range reads()[inTheBand:] {
		if r.killsBefore == 0 {
			order = append(order, r.pid)
		}
	}
	if want := []int{710, 711, 722, 721, 720}; !slices.Equal(order, want) {
		t.Fatalf("before the kill the guard read the smaps of %v in that order, want %v", order, want)
	}
}

// A process's reads of its stay in the band keep their storage from one reading to the next:
// counting the band reads of processes read before allocates nothing, and keeps one stay for each.
func TestMemoryGuardCountsBandReadsAllocatingNothingForAProcessReadBefore(t *testing.T) {
	h := newHarness(t, newFakePod(t), nil)
	cs := make([]candidate, 50)
	for i := range cs {
		cs[i] = candidate{Process: &Process{PID: 1000 + i, StartTime: uint64(i)}, size: uint64(i) * mib}
	}
	now := testStart
	h.g.keepStays(now, cs)
	allocs := testing.AllocsPerRun(100, func() {
		now = now.Add(Defaults().Interval)
		h.g.keepStays(now, cs)
	})
	if allocs != 0 {
		t.Fatalf("a reading's count of band reads allocates %v times, want none", allocs)
	}
	if len(h.g.stays) != len(cs) {
		t.Fatalf("the guard keeps the stays of %d processes, want %d", len(h.g.stays), len(cs))
	}
	for key, stay := range h.g.stays {
		if stay.reads < 2 || stay.grew {
			t.Fatalf("the stay of pid %d is %+v, want two reads or more and no growth", key.pid, stay)
		}
	}
}

// requireMarkKill fails unless the guard's one kill, at the reading at the mark of 3500 MiB, ended
// pid at size, and it went on with the rise: it wrote no stopped line and holds the killed process
// alone.
func requireMarkKill(h *harness, pid int, size uint64) {
	h.t.Helper()
	h.requireKilled(pid)
	ended := fmt.Sprintf("ended pid=%d role=tester size=%d reading=%d ", pid, size, 3500*mib)
	if got := h.log.with(ended); len(got) != 1 {
		h.t.Fatalf("the ended lines are %q, want one starting %q; the log:\n%s", h.log.with("ended"), linePrefix+ended, strings.Join(h.log.all(), "\n"))
	}
	if stopped := h.log.with("stopped"); len(stopped) != 0 {
		h.t.Fatalf("the guard gave the rise up at the reading at the mark: %q", stopped)
	}
	if held := h.signal.held(); held != 1 {
		h.t.Fatalf("the guard holds %d processes, want the one it killed alone", held)
	}
}

// Where it cannot end the first of what the band holds, the guard assesses the reading at the mark
// and acts on it, as at any reading of a rise, and goes on with the rise (INFRA-3531 T16). The band
// holds pid 301 first, which grew 20 MiB over its two readings, with pid 300 behind it, which did
// not; as the reading reaches the mark pid 300 grows 300 MiB, so that reading's own order has pid
// 300 first. Where pid 301 runs, the guard ends it on the band's reads, at the size the band read.
// Where it has exited (a zombie its parent has not reaped, whose handle reads exited), where its
// pid names a process started since (its start time changed), or where its kill is refused, the
// guard ends pid 300 at the size the reading at the mark read, and sends no kill to a process that
// exited or took the pid.
func TestMemoryGuardAssessesTheReadingAtTheMarkWhereItCannotEndTheHeldFirst(t *testing.T) {
	idle := fakeProcess{pid: 301, ppid: 100, args: []string{"python3", "idle.py"}, anon: 600 * mib}
	refused := func(pid int) error {
		if pid == 301 {
			return os.NewSyscallError("pidfd_send_signal", syscall.EPERM)
		}
		return nil
	}
	for _, tc := range []struct {
		name string
		// atTheMark changes pid 301 as the reading reaches the mark.
		atTheMark func(h *harness)
		want      int
		size      uint64
		tried     []int
	}{
		{"the held first runs", func(*harness) {}, 301, 600 * mib, []int{301}},
		{"the held first has exited", func(h *harness) {
			zombie := idle
			zombie.zombie = true
			h.pod.add(zombie)
			h.signal.exit(301)
		}, 300, 700 * mib, []int{300}},
		{"its pid names a process started since", func(h *harness) {
			h.pod.add(fakeProcess{pid: 301, ppid: 1, start: 9999, args: []string{"sleep", "60"}, anon: 10 * mib})
		}, 300, 700 * mib, []int{300}},
		{"its kill is refused", func(h *harness) {
			h.signal.mu.Lock()
			defer h.signal.mu.Unlock()
			h.signal.killErr = refused
		}, 300, 700 * mib, []int{301, 300}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := newFakePod(t)
			grower := fakeProcess{pid: 300, ppid: 100, args: []string{"python3", "grow.py"}, anon: 400 * mib}
			smaller := idle
			smaller.anon = 580 * mib
			pod.add(agent(2*gib), grower, smaller)
			pod.setAnon(3200 * mib)
			h := newHarness(t, pod, protect("tester", 100)).run()
			pod.add(idle)
			pod.setAnon(3300 * mib)
			h.tick()
			pod.resize(&grower, 700*mib)
			tc.atTheMark(h)
			pod.setAnon(3500 * mib)
			h.tick()
			requireMarkKill(h, tc.want, tc.size)
			if tried := h.signal.attempts(); !slices.Equal(tried, tc.tried) {
				t.Fatalf("the guard tried to kill %v, want %v", tried, tc.tried)
			}
		})
	}
}

// Where nothing is held at the reading at the mark, the guard assesses that reading and acts on it,
// as at any reading of a rise (INFRA-3531 T16): where the reading crossed the whole band within one
// interval, from below the band to the mark, and where the guard was off at the reading before, in
// the band, and so read and held nothing. Where the reading before was an armed guard's in the
// band, it ends the first of what that reading held, pid 301, which grew 20 MiB over the band's
// reads, though pid 300 grew 300 MiB as the reading reached the mark. Elsewhere it ends pid 300,
// the larger at the reading at the mark, at the size that reading read.
func TestMemoryGuardAssessesTheReadingAtTheMarkWhereNothingIsHeld(t *testing.T) {
	for _, tc := range []struct {
		name string
		// before is the reading before the one at the mark, and off whether the guard was off at
		// it; held is how many processes it holds after it.
		before uint64
		off    bool
		held   int
		want   int
		size   uint64
	}{
		{"an armed guard's reading in the band before", 3300 * mib, false, 2, 301, 600 * mib},
		{"a reading below the band before", 3000 * mib, false, 0, 300, 700 * mib},
		{"the guard off at the reading in the band before", 3300 * mib, true, 0, 300, 700 * mib},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := newFakePod(t)
			grower := fakeProcess{pid: 300, ppid: 100, args: []string{"python3", "grow.py"}, anon: 400 * mib}
			idle := fakeProcess{pid: 301, ppid: 100, args: []string{"python3", "idle.py"}, anon: 580 * mib}
			pod.add(agent(2*gib), grower, idle)
			pod.setAnon(3200 * mib)
			h := newHarness(t, pod, protect("tester", 100)).run()
			if tc.off {
				h.g.Apply(nil)
			}
			pod.resize(&idle, 600*mib)
			pod.setAnon(tc.before)
			h.tick()
			if got := len(h.g.held); got != tc.held {
				t.Fatalf("after the reading before the mark the guard holds %d processes, want %d", got, tc.held)
			}
			if tc.off {
				h.g.Apply(&shimwire.LauncherMemoryGuard{ActPercent: 85, FloorPercent: 10, WaitSeconds: 5, IntervalMilliseconds: 100})
			}
			pod.resize(&grower, 700*mib)
			pod.setAnon(3500 * mib)
			h.tick()
			requireMarkKill(h, tc.want, tc.size)
		})
	}
}

// Where the held sizes are under the floor, or name no process, the guard assesses the reading at
// the mark and acts on it, and never gives the rise up on the held sizes alone (INFRA-3531 T16).
// Pid 300 grows to 700 MiB as the reading reaches the mark, so that reading's own sizes hold the
// floor of 409.6 MiB. Where the band held pid 300 at 300 MiB and pid 301 at 600 MiB, which hold the
// floor, and pid 301 grew over the band's reads, the guard ends the held first, pid 301. Where it
// held them at 100 and 200 MiB, under the floor, though pid 301 grew, and where it held no process,
// neither yet started, the guard ends pid 300 at the size the reading at the mark read, and writes
// no stopped line.
func TestMemoryGuardAssessesTheReadingAtTheMarkWhereTheHeldSizesAreUnderTheFloorOrNameNoProcess(t *testing.T) {
	for _, tc := range []struct {
		name string
		// first are the processes at the band's first reading, by pid, at their sizes there, and
		// next is pid 301's size at its second, which it keeps at the mark.
		first map[int]uint64
		next  uint64
		want  int
		size  uint64
	}{
		{"the held sizes hold the floor", map[int]uint64{300: 300 * mib, 301: 580 * mib}, 600 * mib, 301, 600 * mib},
		{"the held sizes are under the floor", map[int]uint64{300: 100 * mib, 301: 180 * mib}, 200 * mib, 300, 700 * mib},
		{"the band held no process", nil, 0, 300, 700 * mib},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := newFakePod(t)
			pod.add(agent(2 * gib))
			grower := fakeProcess{pid: 300, ppid: 100, args: []string{"python3", "grow.py"}}
			if size, ok := tc.first[300]; ok {
				pod.resize(&grower, size)
			}
			idle := fakeProcess{pid: 301, ppid: 100, args: []string{"python3", "idle.py"}}
			if size, ok := tc.first[301]; ok {
				pod.resize(&idle, size)
			}
			pod.setAnon(3200 * mib)
			h := newHarness(t, pod, protect("tester", 100)).run()
			if tc.next != 0 {
				pod.resize(&idle, tc.next)
			}
			pod.setAnon(3300 * mib)
			h.tick()
			if got := len(h.g.held); got != len(tc.first) {
				t.Fatalf("the band holds %d processes, want %d", got, len(tc.first))
			}
			pod.resize(&grower, 700*mib)
			pod.setAnon(3500 * mib)
			h.tick()
			requireMarkKill(h, tc.want, tc.size)
		})
	}
}

// Of the processes it may end, the guard ends first the one that grew the most over the window,
// whatever their sizes: pid 301, which grew 80 MiB at each reading, before pid 300, which grew 20
// MiB, and before pid 302, the largest, which did not grow. At the next rise nothing grows, and
// it ends the largest, pid 302.
func TestMemoryGuardEndsTheMostGrownFirstAndTheLargestWhereNoneGrew(t *testing.T) {
	pod := newFakePod(t)
	slow := fakeProcess{pid: 300, ppid: 100, args: []string{"python3", "slow.py"}, anon: 200 * mib}
	fast := fakeProcess{pid: 301, ppid: 100, args: []string{"python3", "fast.py"}, anon: 300 * mib}
	pod.add(agent(2*gib), slow, fast, fakeProcess{pid: 302, ppid: 100, args: []string{"python3", "idle.py"}, anon: 900 * mib})
	pod.setAnon(3100 * mib)
	h := newHarness(t, pod, protect("tester", 100))
	// Each kill takes the reading below the band, which ends the rise.
	h.signal.onKill = exitFreeing(pod, 700*mib)
	h.run()
	for i, reading := range []uint64{3300, 3500} {
		pod.resize(&slow, slow.anon+20*mib)
		pod.resize(&fast, fast.anon+80*mib)
		pod.setAnon(reading * mib)
		h.tick()
		if i == 0 {
			h.requireKilled()
		}
	}
	h.requireKilled(301)
	h.tick()

	for _, reading := range []uint64{3100, 3300, 3500} {
		pod.setAnon(reading * mib)
		h.tick()
	}
	h.requireKilled(301, 302)
}

// A process with no earlier read in the window has not grown, however large its one read: a fresh
// fork under gVisor reads its parent's shared pages as its own. Pid 310, forked at the band's last
// reading before the mark and read there at 900 MiB, is held with that one read, and at the reading
// at the mark it is not ended before pid 300, which grew 50 MiB over the band's reads, as it would
// be if a missing read counted as a size of 0.
func TestMemoryGuardCountsAProcessWithNoEarlierReadAsNotGrown(t *testing.T) {
	pod := newFakePod(t)
	grower := fakeProcess{pid: 300, ppid: 100, args: []string{"python3", "grow.py"}, anon: 300 * mib}
	pod.add(agent(2*gib), grower)
	pod.setAnon(3100 * mib)
	h := newHarness(t, pod, protect("tester", 100)).run()
	pod.resize(&grower, 350*mib)
	pod.add(fakeProcess{pid: 310, ppid: 100, args: []string{"python3", "worker.py"}, anon: 900 * mib})
	pod.setAnon(3300 * mib)
	h.tick()
	pod.resize(&grower, 400*mib)
	pod.setAnon(3500 * mib)
	h.tick()
	h.requireKilled(300)
}

// Growth under growthNoise, 1 MiB, over the window is none: pid 300, 1020 KiB larger at the band's
// second reading than at its first, ranks by its size behind the larger pid 301 at the reading at
// the mark, and 1 MiB larger it ends first.
func TestMemoryGuardCountsGrowthUnderOneMebibyteAsNone(t *testing.T) {
	for _, tc := range []struct {
		name   string
		growth uint64
		want   int
	}{
		{"1020 KiB", 1020 * kib, 301},
		{"1 MiB", mib, 300},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := newFakePod(t)
			grower := fakeProcess{pid: 300, ppid: 100, args: []string{"python3", "grow.py"}, anon: 400 * mib}
			pod.add(agent(2*gib), grower, fakeProcess{pid: 301, ppid: 100, args: []string{"python3", "idle.py"}, anon: 600 * mib})
			pod.setAnon(3100 * mib)
			h := newHarness(t, pod, protect("tester", 100)).run()
			pod.resize(&grower, grower.anon+tc.growth)
			pod.setAnon(3300 * mib)
			h.tick()
			pod.setAnon(3500 * mib)
			h.tick()
			h.requireKilled(tc.want)
		})
	}
}

// The shared-page probe: a parent holds a block of 800 MiB, and three children it forked only
// sleep, each reporting the block as its own, as a forked process does under gVisor, beside a
// separate grower smaller than the block. By size alone the newest child would end first, and its
// end would free nothing; the guard ends the grower first, the one process that grew, at the first
// reading at the mark and at the 500 MiB the band's last reading read of it (endHeld).
func TestMemoryGuardEndsAGrowerBeforeForksThatReportTheirParentsSharedBlock(t *testing.T) {
	const block = 800 * mib
	pod := newFakePod(t)
	pod.add(
		agent(2*gib),
		fakeProcess{pid: 700, ppid: 100, start: 5000, args: []string{"python3", "pool.py"}, anon: block},
		fakeProcess{pid: 701, ppid: 700, start: 5100, args: []string{"python3", "pool.py"}, anon: block},
		fakeProcess{pid: 702, ppid: 700, start: 5200, args: []string{"python3", "pool.py"}, anon: block},
		fakeProcess{pid: 703, ppid: 700, start: 5300, args: []string{"python3", "pool.py"}, anon: block},
	)
	grower := fakeProcess{pid: 710, ppid: 100, start: 5400, args: []string{"python3", "grow.py"}, anon: 300 * mib}
	pod.add(grower)
	// The agent, the block once and the grower.
	reading := func() uint64 { return 2*gib + block + grower.anon }
	pod.setAnon(reading())
	h := newHarness(t, pod, protect("tester", 100))
	h.signal.onKill = exitFreeing(pod, 640*mib)
	h.run()
	for _, size := range []uint64{400 * mib, 500 * mib, 640 * mib} {
		pod.resize(&grower, size)
		pod.setAnon(reading())
		h.tick()
	}
	h.requireKilled(710)
	if got := h.log.with(fmt.Sprintf("ended pid=710 role=tester size=%d reading=%d ", 500*mib, reading())); len(got) != 1 {
		t.Fatalf("the ended lines are %q, want the grower's at 500 MiB, its size at the band's last reading, and the reading at the mark", h.log.with("ended"))
	}
	h.tick()
	h.requireKilled(710)
}

// After a fall that stays at the mark the guard ends the next in the same order, by the sizes of
// that reading: pid 301, which still grows, before pid 302, the largest, which does not.
func TestMemoryGuardEndsTheNextInTheOrderAfterAFall(t *testing.T) {
	pod := newFakePod(t)
	first := fakeProcess{pid: 300, ppid: 100, args: []string{"python3", "fast.py"}, anon: 300 * mib}
	next := fakeProcess{pid: 301, ppid: 100, args: []string{"python3", "slow.py"}, anon: 200 * mib}
	pod.add(agent(2*gib), first, next, fakeProcess{pid: 302, ppid: 100, args: []string{"python3", "idle.py"}, anon: 700 * mib})
	pod.setAnon(3100 * mib)
	h := newHarness(t, pod, protect("tester", 100))
	// 10 MiB freed: 3490 MiB has fallen and is still at the mark.
	h.signal.onKill = exitFreeing(pod, 10*mib)
	h.run()
	for _, reading := range []uint64{3300, 3500} {
		pod.resize(&first, first.anon+100*mib)
		pod.resize(&next, next.anon+30*mib)
		pod.setAnon(reading * mib)
		h.tick()
	}
	h.requireKilled(300)
	pod.resize(&next, next.anon+30*mib)
	h.tick()
	h.requireKilled(300, 301)
}

// When the wait after a kill passes and the reading has not fallen, the guard ends the next in
// the order only while a process it may end has grown since the kill and those processes hold the
// floor between them; otherwise it gives the rise up with reason not fallen, either way. The first
// kill, at the first reading, is of the largest, pid 302, a fork whose end frees nothing.
func TestMemoryGuardAfterAWaitWithNoFallEndsTheNextOnlyWhileOneGrowsAndTheFloorHolds(t *testing.T) {
	for _, tc := range []struct {
		name string
		// grows is whether pid 301 grows over the readings before the wait passes, and idle
		// whether pid 303 is there, which with pid 301 holds the floor.
		grows, idle bool
		want        []int
		endable     uint64
	}{
		{"one grows and the floor holds", true, true, []int{302, 301}, 0},
		{"none grows", false, true, []int{302}, 300*mib + 200*mib},
		{"one grows under the floor", true, false, []int{302}, 320 * mib},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := newFakePod(t)
			grower := fakeProcess{pid: 301, ppid: 100, args: []string{"python3", "grow.py"}, anon: 300 * mib}
			pod.add(agent(2*gib), grower, fakeProcess{pid: 302, ppid: 100, args: []string{"python3", "pool.py"}, anon: 900 * mib})
			if tc.idle {
				pod.add(fakeProcess{pid: 303, ppid: 100, args: []string{"python3", "idle.py"}, anon: 200 * mib})
			}
			pod.setAnon(3600 * mib)
			h := newHarness(t, pod, protect("tester", 100))
			h.signal.onKill = exitFreeing(pod, 0)
			h.run()
			h.requireKilled(302)
			// The reading 100 ms before the wait passes, and the one at which it has passed.
			h.tickAfter(Defaults().Wait - 2*Defaults().Interval)
			if tc.grows {
				pod.resize(&grower, grower.anon+20*mib)
			}
			h.tick()
			h.requireKilled(tc.want...)
			stopped := h.log.with("stopped")
			if len(tc.want) == 2 {
				if len(stopped) != 0 {
					t.Fatalf("the guard gave the rise up while pid 301 grew: %q", stopped)
				}
				return
			}
			want := fmt.Sprintf("launcher: memory guard: stopped for this rise reason=not fallen reading=%d endable=%d ", 3600*mib, tc.endable)
			if len(stopped) != 1 || !strings.HasPrefix(stopped[0], want) {
				t.Fatalf("the stopped lines are %q, want one starting %q", stopped, want)
			}
		})
	}
}

// A rise that is a protected process's own, Oh My Pi's, beside two idle kernels that hold more
// than the floor, each on its own too, costs one kernel: at the mark nothing the guard may end has
// grown, so it ends the larger kernel. That kill lowers nothing the agent's growth does not make
// up, and when the wait passes with no fall and nothing it may end grown since the kill, the guard
// gives the rise up with reason not fallen and never ends the other kernel, however far the agent
// grows.
func TestMemoryGuardEndsOneKernelForARiseThatIsAProtectedProcesssOwn(t *testing.T) {
	pod := newFakePod(t)
	omp := agent(2000 * mib)
	pod.add(
		omp,
		fakeProcess{pid: 200, ppid: 100, args: []string{"python3", "-m", "kernel"}, anon: 600 * mib},
		fakeProcess{pid: 201, ppid: 100, args: []string{"bun", "eval.js"}, anon: 500 * mib},
	)
	pod.setAnon(3100 * mib)
	h := newHarness(t, pod, protect("tester", 100))
	h.signal.onKill = exitFreeing(pod, 0)
	h.run()
	grow := func(reading uint64) {
		t.Helper()
		pod.resize(&omp, omp.anon+100*mib)
		pod.setAnon(reading * mib)
	}
	for _, reading := range []uint64{3200, 3300, 3400, 3500} {
		grow(reading)
		h.tick()
	}
	h.requireKilled(200)
	grow(3600)
	h.tickAfter(Defaults().Wait - 2*Defaults().Interval)
	grow(3700)
	h.tick()
	for _, reading := range []uint64{3800, 3900, 4000} {
		grow(reading)
		h.tick()
	}
	h.requireKilled(200)
	want := fmt.Sprintf("launcher: memory guard: stopped for this rise reason=not fallen reading=%d endable=%d ", 3700*mib, 500*mib)
	if stopped := h.log.with("stopped"); len(stopped) != 1 || !strings.HasPrefix(stopped[0], want) {
		t.Fatalf("the stopped lines are %q, want one starting %q", stopped, want)
	}
}

// protpoolReading is one reading of the tester's protpool probe at 4 GiB, in hundredths of a
// percent of the limit, and how long after the reading before it the guard took it.
type protpoolReading struct {
	hundredths uint64
	after      time.Duration
}

// The tester's protpool probe of 2026-11-17 under gVisor at 4 GiB, replayed with its recorded
// readings (dispatch://INFRA-3531/artifact/tester-evidence-a97e6a44-2026-11-17-tar-gz, logs3,
// infra-3531-t-adv-4g-protpool-sxs4j): the agent, protected as Oh My Pi is, holds 60% of the limit,
// and six growers rise about 1.5 GiB a second together. The first kill, at the first reading at
// the mark, 85.54%, freed less than the rise of the next interval, so the readings went on rising,
// 86.19%, 86.55%, 89.68% and 92.87%, and the host ended the pod at about 97% while the guard still
// waited. Here each killed grower exits at its kill and the readings are the recorded ones. Once
// a killed process has exited and the reading has not fallen below the reading at the kill, the
// guard ends the next in the order at the next reading, while a process it may end has grown since
// that kill and those processes hold the floor (INFRA-3531 R1, spec version 25): a grower at each
// of the four readings after the first kill, and no stopped line. While the killed grower has not
// exited, the guard waits for it, up to its wait, and ends nothing more.
func TestMemoryGuardEndsAGrowerAtEachReadingAfterAnExitWhileTheReadingStillRises(t *testing.T) {
	const agentBytes = podLimit / 100 * 60
	first := protpoolReading{7535, 0}
	toTheMark := []protpoolReading{{7935, 203 * time.Millisecond}, {8193, 117 * time.Millisecond},
		{8194, 105 * time.Millisecond}, {8554, 104 * time.Millisecond}}
	afterTheKill := []protpoolReading{{8619, 298 * time.Millisecond}, {8655, 105 * time.Millisecond},
		{8968, 105 * time.Millisecond}, {9287, 105 * time.Millisecond}}
	for _, tc := range []struct {
		name   string
		exits  bool
		killed int
	}{
		{"each killed grower exits", true, 1 + len(afterTheKill)},
		{"the killed grower has not exited", false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := newFakePod(t)
			pod.add(agent(agentBytes))
			growers := map[int]*fakeProcess{}
			for i := range 6 {
				pid := 800 + i
				growers[pid] = &fakeProcess{pid: pid, ppid: 100, start: uint64(6000 + i), args: []string{"python3", "-c", "grow"}}
			}
			// The pod at a reading: the agent, and the growers left sharing the rest, each one MiB apart.
			at := func(r protpoolReading) {
				t.Helper()
				reading := podLimit / 10000 * r.hundredths
				share := (reading - agentBytes) / uint64(len(growers))
				for pid, g := range growers {
					pod.resize(g, share+uint64(pid-800)*mib)
				}
				pod.setAnon(reading)
			}
			at(first)
			h := newHarness(t, pod, protect("tester", 100))
			h.signal.onKill = func(pid int) bool {
				if !tc.exits {
					return false
				}
				delete(growers, pid)
				pod.remove(pid)
				return true
			}
			h.run()
			for _, r := range toTheMark {
				at(r)
				h.tickAfter(r.after - Defaults().Interval)
			}
			if kills := h.signal.kills(); len(kills) != 1 {
				t.Fatalf("at the first reading at the mark the guard killed %v, want one grower; the log:\n%s", kills, strings.Join(h.log.all(), "\n"))
			}
			for _, r := range afterTheKill {
				at(r)
				h.tickAfter(r.after - Defaults().Interval)
			}
			kills := h.signal.kills()
			if len(kills) != tc.killed {
				t.Fatalf("over the four readings after the first kill the guard killed %v, want %d kills in all; the log:\n%s",
					kills, tc.killed, strings.Join(h.log.all(), "\n"))
			}
			seen := map[int]bool{}
			for _, pid := range kills {
				if pid < 800 || pid > 805 || seen[pid] {
					t.Fatalf("the guard killed %v, want each a different grower", kills)
				}
				seen[pid] = true
			}
			if stopped := h.log.with("stopped"); len(stopped) != 0 {
				t.Fatalf("the guard gave the rise up while growers grew: %q", stopped)
			}
		})
	}
}

// A grower whose steps are wider than the ordering window still counts as grown since the
// previous kill (INFRA-3531 R1, spec version 25). At the reading at the mark, which the agent's
// own growth reaches, pid 301's last step of 64 MiB is 300 ms old, so over the window nothing grew
// and the guard ends the largest, pid 302, a fork whose end frees nothing. It exits. At the next
// reading pid 301 steps again and the reading has not fallen, and the guard ends it; it never
// gives the rise up while pid 301 runs, though by the time the wait has passed that step too is
// older than the window.
func TestMemoryGuardCountsAGrowerWhoseStepsAreWiderThanTheWindowAsGrownSinceTheKill(t *testing.T) {
	pod := newFakePod(t)
	omp := agent(2 * gib)
	grower := fakeProcess{pid: 301, ppid: 100, args: []string{"python3", "grow.py"}, anon: 300 * mib}
	pod.add(omp, grower, fakeProcess{pid: 302, ppid: 100, args: []string{"python3", "pool.py"}, anon: 900 * mib})
	reading := func() uint64 { return omp.anon + 900*mib + grower.anon }
	pod.setAnon(reading())
	h := newHarness(t, pod, protect("tester", 100))
	h.signal.onKill = func(pid int) bool {
		pod.remove(pid)
		return true
	}
	h.run()
	// Readings 1 to 8: pid 301 steps at readings 1 and 5; at reading 8 the agent takes the pod
	// past the mark, 300 ms after pid 301's last step.
	for i := 1; i <= 8; i++ {
		if i == 1 || i == 5 {
			pod.resize(&grower, grower.anon+64*mib)
		}
		if i == 8 {
			pod.resize(&omp, omp.anon+200*mib)
		}
		pod.setAnon(reading())
		h.tick()
	}
	h.requireKilled(302)
	// Pid 302's end frees nothing: the reading stays where pid 301's next step leaves it.
	pod.resize(&grower, grower.anon+64*mib)
	pod.setAnon(reading())
	h.tick()
	if stopped := h.log.with("stopped"); len(stopped) != 0 {
		t.Fatalf("the guard gave the rise up while pid 301 ran: %q", stopped)
	}
	// The reading at which the wait has passed, pid 301's step now older than the window.
	h.tickAfter(Defaults().Wait)
	h.requireKilled(302, 301)
}

// The tester's latepool probe of 2026-11-17 under gVisor at 4 GiB, replayed with its recorded
// readings (dispatch://INFRA-3531/artifact/tester-evidence-e3a1923c-2026-11-17-tar-gz, logs4,
// infra-3531-t-adv-4g-latepool-gk8ht): a parent and three children it forked share a block of 56%
// of the limit, each reporting the block as its own, beside the agent's quarter, and hold the pod
// in the band below the mark, at 84.17%. Six growers then start, and the next reading, the first at
// the mark, is 88.37%. None of them has an earlier read, and over the window nothing grew. At
// e3a1923c98d7 the guard ended a sleeping fork there, whose end freed nothing, and its next readings
// rose to 96.82%, 2.4 MiB below the pod's measured kill point. Where nothing grew over the window,
// the largest process with no earlier read, one started since the reading before, comes first
// (INFRA-3531 R1, spec version 27): the guard ends a grower at the first reading at the mark.
func TestMemoryGuardEndsAFreshGrowerAtTheMarkBeforeASleepingForkOfASharedBlock(t *testing.T) {
	block := podLimit / 100 * 56
	pod := newFakePod(t)
	pod.add(
		agent(podLimit/4),
		fakeProcess{pid: 700, ppid: 100, start: 5000, args: []string{"python3", "latepool.py"}, anon: block},
		fakeProcess{pid: 701, ppid: 700, start: 5100, args: []string{"python3", "latepool.py"}, anon: block},
		fakeProcess{pid: 702, ppid: 700, start: 5200, args: []string{"python3", "latepool.py"}, anon: block},
		fakeProcess{pid: 703, ppid: 700, start: 5300, args: []string{"python3", "latepool.py"}, anon: block},
	)
	at := func(hundredths uint64) { pod.setAnon(podLimit / 10000 * hundredths) }
	at(7676)
	h := newHarness(t, pod, protect("tester", 100))
	h.signal.onKill = exitFreeing(pod, 0)
	h.run()
	for _, hundredths := range []uint64{8013, 8375, 8375, 8375, 8417, 8417, 8417, 8417, 8417} {
		at(hundredths)
		h.tick()
	}
	if kills := h.signal.kills(); len(kills) != 0 {
		t.Fatalf("in the band the guard killed %v, want nothing; the log:\n%s", kills, strings.Join(h.log.all(), "\n"))
	}
	// Six growers start, each a python3 child of the parent holding its first allocation, and the
	// reading is at the mark: 172 MiB of rise among them.
	for i, size := range []uint64{20, 24, 28, 30, 34, 36} {
		pod.add(fakeProcess{pid: 800 + i, ppid: 700, start: uint64(6000 + i), args: []string{"python3", "-c", "grow"}, anon: size * mib})
	}
	at(8837)
	h.tick()
	kills := h.signal.kills()
	if len(kills) != 1 || kills[0] < 800 || kills[0] > 805 {
		t.Fatalf("at the first reading at the mark the guard killed %v, want one of the fresh growers 800 to 805 and no fork of the block; the log:\n%s",
			kills, strings.Join(h.log.all(), "\n"))
	}
}

// After a kill, a process started since the reading before is ended before one that only grew
// since the kill, where nothing grew over the window (INFRA-3531 R1 of spec version 27). At the
// first reading, at the mark, the guard read nothing before, every process is new, and it ends the
// largest, pid 302, a fork whose end frees nothing. The next reading comes 400 ms later: pid 301
// has grown 64 MiB since the kill, which has the guard go on, and pid 310 has started, smaller than
// pid 301. Over the window nothing grew, and the guard ends pid 310, the one started since.
func TestMemoryGuardEndsAProcessStartedSinceTheReadingBeforeBeforeOneThatOnlyGrewSinceTheKill(t *testing.T) {
	pod := newFakePod(t)
	grower := fakeProcess{pid: 301, ppid: 100, args: []string{"python3", "grow.py"}, anon: 300 * mib}
	pod.add(
		agent(2*gib),
		grower,
		fakeProcess{pid: 302, ppid: 100, args: []string{"python3", "pool.py"}, anon: 900 * mib},
		fakeProcess{pid: 303, ppid: 100, args: []string{"python3", "idle.py"}, anon: 200 * mib},
	)
	pod.setAnon(3600 * mib)
	h := newHarness(t, pod, protect("tester", 100))
	h.signal.onKill = exitFreeing(pod, 0)
	h.run()
	h.requireKilled(302)

	pod.resize(&grower, grower.anon+64*mib)
	pod.add(fakeProcess{pid: 310, ppid: 100, args: []string{"python3", "worker.py"}, anon: 100 * mib})
	h.tickAfter(growthWindow)
	h.requireKilled(302, 310)
	if stopped := h.log.with("stopped"); len(stopped) != 0 {
		t.Fatalf("the guard gave the rise up: %q", stopped)
	}
}

// protectHelpers is a classifier that protects the agent, pid 100, and each child of it whose
// second argument is an Oh My Pi helper's selector, as the launcher's protects Oh My Pi's helpers
// by their place in the tree, and gives every process role tester.
func protectHelpers(t *Table) map[int]Class {
	out := map[int]Class{}
	for _, p := range t.Processes() {
		out[p.PID] = Class{Role: "tester", Protected: p.PID == 100}
	}
	for _, child := range t.Children(100) {
		if len(child.Args) > 1 && strings.HasPrefix(child.Args[1], "__omp_worker_") {
			out[child.PID] = Class{Role: "tester", Protected: true}
		}
	}
	return out
}

// A process started since the band's last reading is classified as any process is: one the
// classifier protects is never ended at the mark, however large. Pid 700 sleeps through the band's
// two readings. As the reading reaches the mark, Oh My Pi starts a helper under the agent, pid 820,
// of 1200 MiB, which the classifier protects by its place in the tree, and a command, pid 821, of
// 150 MiB. Nothing held grew, and the guard ends pid 821, which it reads before the kill, and
// neither reads nor ends the helper.
func TestMemoryGuardNeverEndsAProcessStartedSinceTheBandsLastReadingThatTheClassifierProtects(t *testing.T) {
	pod := newFakePod(t)
	pod.add(agent(2*gib), fakeProcess{pid: 700, ppid: 100, args: []string{"python3", "pool.py"}, anon: 900 * mib})
	pod.setAnon(3200 * mib)
	h := newHarness(t, pod, protectHelpers)
	reads := recordReads(h)
	h.run()
	pod.setAnon(3300 * mib)
	h.tick()
	inTheBand := len(reads())
	pod.add(
		fakeProcess{pid: 820, ppid: 100, name: "omp", exe: "/opt/omp/bin/omp", args: []string{"omp", "__omp_worker_daemon_broker"}, anon: 1200 * mib},
		fakeProcess{pid: 821, ppid: 100, args: []string{"python3", "worker.py"}, anon: 150 * mib},
	)
	pod.setAnon(3500 * mib)
	h.tick()
	requireMarkKill(h, 821, 150*mib)
	if got, want := readBeforeTheKill(reads()[inTheBand:]), []int{821}; !slices.Equal(got, want) {
		t.Fatalf("before the kill the guard read the smaps of %v, want the command's alone: %v", got, want)
	}
}

// The reads of the processes started since the band's last reading share the budget of the reads
// again at the mark, half the interval from the reading at the mark by the guard's clock, and take
// the newest first: it reads one at least, however much of the budget has passed. Pid 700 sleeps
// through the band's two readings and is not read again; pids 810, 811 and 812, of 300, 200 and
// 100 MiB, the newest last, start as the reading reaches the mark. At 60 ms a read, it reads pid
// 812 alone and ends it; where the reading at the mark itself took 80 ms, it still reads pid 812
// and ends it; where the reads take no time, it reads all three and ends pid 810, the largest.
func TestMemoryGuardReadsTheProcessesStartedSinceTheBandsLastReadingWithinTheBudgetTheNewestFirst(t *testing.T) {
	for _, tc := range []struct {
		name          string
		mark, perRead time.Duration
		read          []int
		want          int
		size          uint64
	}{
		{"60 ms a read", 0, 60 * time.Millisecond, []int{812}, 812, 100 * mib},
		{"the reading at the mark past the budget", 80 * time.Millisecond, 0, []int{812}, 812, 100 * mib},
		{"no time a read", 0, 0, []int{810, 811, 812}, 810, 300 * mib},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := newFakePod(t)
			pod.add(agent(2*gib), fakeProcess{pid: 700, ppid: 100, args: []string{"python3", "pool.py"}, anon: 900 * mib})
			pod.setAnon(3200 * mib)
			h := newHarness(t, pod, protect("tester", 100))
			reads := recordReads(h)
			atMark, perRead := slowMark(h)
			h.run()
			pod.setAnon(3300 * mib)
			h.tick()
			inTheBand := len(reads())
			for i, size := range []uint64{300, 200, 100} {
				pod.add(fakeProcess{pid: 810 + i, ppid: 100, start: uint64(6000 + i), args: []string{"python3", "-c", "grow"}, anon: size * mib})
			}
			pod.setAnon(3500 * mib)
			atMark.Store(int64(tc.mark))
			perRead.Store(int64(tc.perRead))
			h.tick()
			requireMarkKill(h, tc.want, tc.size)
			if got := readBeforeTheKill(reads()[inTheBand:]); !slices.Equal(got, tc.read) {
				t.Fatalf("before the kill the guard read the smaps of %v, want %v", got, tc.read)
			}
		})
	}
}

// Growth over the window comes before a process started since the reading before (INFRA-3531 R1
// of spec version 27): where a held process grew by the band's reads, the guard ends it on the held
// reads, with no smaps read and no process looked for before the kill, though a process larger than
// it started as the reading reached the mark. Pid 710 grows 40 MiB between the band's two readings
// beside pid 700, a sleeper; pid 820, of 1 GiB, starts at the mark.
func TestMemoryGuardEndsAHeldGrowerBeforeAProcessStartedSinceTheBandsLastReading(t *testing.T) {
	pod := newFakePod(t)
	grower := fakeProcess{pid: 710, ppid: 100, args: []string{"python3", "grow.py"}, anon: 300 * mib}
	pod.add(agent(2*gib), grower, fakeProcess{pid: 700, ppid: 100, args: []string{"python3", "pool.py"}, anon: 900 * mib})
	pod.setAnon(3200 * mib)
	h := newHarness(t, pod, protect("tester", 100))
	reads := recordReads(h)
	h.run()
	pod.resize(&grower, 340*mib)
	pod.setAnon(3300 * mib)
	h.tick()
	inTheBand := len(reads())
	pod.add(fakeProcess{pid: 820, ppid: 100, args: []string{"python3", "worker.py"}, anon: gib})
	pod.setAnon(3500 * mib)
	h.tick()
	requireMarkKill(h, 710, 340*mib)
	if got := readBeforeTheKill(reads()[inTheBand:]); len(got) != 0 {
		t.Fatalf("before the kill the guard read the smaps of %v, want none", got)
	}
}

// After a kill, the next in the order is the one that grew the most since that kill where nothing
// grew over the window (INFRA-3531 R1 of spec version 26): a throttled pod's readings come more
// than the window apart, so the window holds one read of each process and shows no growth. A
// parent holds a block of 1.5 GiB and three children it forked sleep, each reporting the block as
// its own, beside a grower smaller than the block. At the first reading, at the mark, nothing has
// grown and the guard ends the largest, the newest fork, which exits and frees nothing. The next
// reading comes 400 ms later, the grower 64 MiB larger, and the reading has not fallen: over the
// window nothing grew, but the grower grew since the kill, and the guard ends it, not the next fork.
func TestMemoryGuardEndsTheMostGrownSinceTheKillNextWhereReadingsComeWiderApartThanTheWindow(t *testing.T) {
	const block = 1536 * mib
	pod := newFakePod(t)
	pod.add(
		agent(gib),
		fakeProcess{pid: 700, ppid: 100, start: 5000, args: []string{"python3", "pool.py"}, anon: block},
		fakeProcess{pid: 701, ppid: 700, start: 5100, args: []string{"python3", "pool.py"}, anon: block},
		fakeProcess{pid: 702, ppid: 700, start: 5200, args: []string{"python3", "pool.py"}, anon: block},
		fakeProcess{pid: 703, ppid: 700, start: 5300, args: []string{"python3", "pool.py"}, anon: block},
	)
	grower := fakeProcess{pid: 710, ppid: 100, start: 5400, args: []string{"python3", "grow.py"}, anon: 1000 * mib}
	pod.add(grower)
	// The agent, the block once and the grower: 3560 MiB, at the mark.
	reading := func() uint64 { return gib + block + grower.anon }
	pod.setAnon(reading())
	h := newHarness(t, pod, protect("tester", 100))
	h.signal.onKill = exitFreeing(pod, 0)
	h.run()
	h.requireKilled(703)

	pod.resize(&grower, grower.anon+64*mib)
	pod.setAnon(reading())
	h.tickAfter(growthWindow)
	h.requireKilled(703, 710)
	if reads := h.g.sizes[procKey{pid: 710, start: 5400}]; len(reads) != 1 {
		t.Fatalf("the guard keeps %d reads of the grower in the window, want the one of this reading", len(reads))
	}
	ended := fmt.Sprintf("ended pid=710 role=tester size=%d reading=%d ", 1064*mib, reading())
	if got := h.log.with(ended); len(got) != 1 {
		t.Fatalf("the ended lines are %q, want one starting %q", h.log.with("ended"), linePrefix+ended)
	}
	if stopped := h.log.with("stopped"); len(stopped) != 0 {
		t.Fatalf("the guard gave the rise up while the grower grew: %q", stopped)
	}
}

// After a kill whose process exits at once, a reading that has not fallen below the reading at the
// kill, with nothing the guard may end grown since the kill, ends nothing and gives nothing up:
// the guard decides again at each reading, and only at the reading at which the wait has passed
// does it give the rise up, with reason not fallen (INFRA-3531 R1, spec version 25). Pid 302, the
// largest, is a fork whose end frees nothing. The sizes as of the kill it keeps are those of the
// processes it may end, and none once the rise is given up.
func TestMemoryGuardEndsNothingAfterAnExitWithNoFallAndNothingGrownUntilTheWaitPassesAndThenStops(t *testing.T) {
	pod := newFakePod(t)
	pod.add(
		agent(2*gib),
		fakeProcess{pid: 301, ppid: 100, args: []string{"python3", "idle.py"}, anon: 300 * mib},
		fakeProcess{pid: 302, ppid: 100, args: []string{"python3", "pool.py"}, anon: 900 * mib},
		fakeProcess{pid: 303, ppid: 100, args: []string{"python3", "idle.py"}, anon: 200 * mib},
	)
	pod.setAnon(3600 * mib)
	h := newHarness(t, pod, protect("tester", 100))
	h.signal.onKill = exitFreeing(pod, 0)
	h.run()
	h.requireKilled(302)
	readings := int(Defaults().Wait / Defaults().Interval)
	for i := 1; i < readings; i++ {
		h.tick()
		h.requireKilled(302)
		if stopped := h.log.with("stopped"); len(stopped) != 0 {
			t.Fatalf("at reading %d after the exit, before the wait passed, the guard gave the rise up: %q", i, stopped)
		}
		if len(h.g.atKill) != 2 {
			t.Fatalf("at reading %d after the exit the guard keeps the sizes as of the kill of %d processes, want the 2 it may end", i, len(h.g.atKill))
		}
	}
	h.tick()
	h.requireKilled(302)
	want := fmt.Sprintf("launcher: memory guard: stopped for this rise reason=not fallen reading=%d endable=%d ", 3600*mib, 500*mib)
	if stopped := h.log.with("stopped"); len(stopped) != 1 || !strings.HasPrefix(stopped[0], want) {
		t.Fatalf("at the reading at which the wait passed the stopped lines are %q, want one starting %q", stopped, want)
	}
	if len(h.g.atKill) != 0 {
		t.Fatalf("once the rise is given up the guard keeps the sizes as of the kill of %d processes, want none", len(h.g.atKill))
	}
}

// A process first read after a kill, pid 310, forked after it, has that first read kept as its
// size as of the kill: at that read it has not grown, however large, and it counts as grown once
// its size is growthNoise past that first read, here after two steps of 600 KiB, neither of which
// is growthNoise on its own (INFRA-3531 R1, spec version 25). Pid 302, the largest at the mark,
// exits at its kill and frees nothing.
func TestMemoryGuardCountsAProcessFirstReadAfterAKillAsGrownOncePastItsFirstRead(t *testing.T) {
	pod := newFakePod(t)
	pod.add(
		agent(2*gib),
		fakeProcess{pid: 302, ppid: 100, args: []string{"python3", "pool.py"}, anon: 900 * mib},
		fakeProcess{pid: 303, ppid: 100, args: []string{"python3", "idle.py"}, anon: 200 * mib},
	)
	pod.setAnon(3600 * mib)
	h := newHarness(t, pod, protect("tester", 100))
	h.signal.onKill = exitFreeing(pod, 0)
	h.run()
	h.requireKilled(302)
	fresh := fakeProcess{pid: 310, ppid: 100, args: []string{"python3", "worker.py"}, anon: 400 * mib}
	pod.add(fresh)
	h.tick()
	h.requireKilled(302)
	pod.resize(&fresh, fresh.anon+600*kib)
	h.tick()
	h.requireKilled(302)
	pod.resize(&fresh, fresh.anon+600*kib)
	h.tick()
	h.requireKilled(302, 310)
	if stopped := h.log.with("stopped"); len(stopped) != 0 {
		t.Fatalf("the guard gave the rise up while pid 310 grew: %q", stopped)
	}
}

// A killed process that has not exited when the wait passes is waited on no longer. At the reading
// at which the wait has passed, the reading not fallen, the guard ends the next in the order where
// a process it may end has grown since the kill, and otherwise gives the rise up with reason not
// fallen (INFRA-3531 R1, spec version 25). Pid 302, which grew over the band's readings before the
// mark, is killed first and stays. Pid 301 grows 20 MiB at the first reading after the kill and no
// more: the guard ends nothing while it waits for pid 302's exit, and when the wait has passed that
// step is far older than the window, so it counts only as growth since the kill, and pid 301 is
// ended. Pid 302, which has not exited, is none of what the guard may end, though its stopped line
// still counts it in endable, since it still holds its memory.
func TestMemoryGuardEndsAProcessGrownSinceTheKillOrStopsAtAWaitThatPassesBeforeTheExit(t *testing.T) {
	for _, tc := range []struct {
		name  string
		grows bool
		want  []int
	}{
		{"one grown since the kill", true, []int{302, 301}},
		{"none grown since the kill", false, []int{302}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := newFakePod(t)
			fast := fakeProcess{pid: 302, ppid: 100, args: []string{"python3", "fast.py"}, anon: 300 * mib}
			grower := fakeProcess{pid: 301, ppid: 100, args: []string{"python3", "grow.py"}, anon: 600 * mib}
			pod.add(agent(2*gib), fast, grower)
			pod.setAnon(3300 * mib)
			h := newHarness(t, pod, protect("tester", 100))
			// Pid 302's exit never comes.
			h.signal.onKill = func(int) bool { return false }
			h.run()
			pod.resize(&fast, fast.anon+40*mib)
			pod.setAnon(3400 * mib)
			h.tick()
			pod.setAnon(3600 * mib)
			h.tick()
			h.requireKilled(302)
			readings := int(Defaults().Wait / Defaults().Interval)
			for i := 1; i < readings; i++ {
				if i == 1 && tc.grows {
					pod.resize(&grower, grower.anon+20*mib)
				}
				h.tick()
				h.requireKilled(302)
			}
			h.tick()
			h.requireKilled(tc.want...)
			stopped := h.log.with("stopped")
			if tc.grows {
				if len(stopped) != 0 {
					t.Fatalf("the guard gave the rise up though pid 301 grew since the kill: %q", stopped)
				}
				return
			}
			want := fmt.Sprintf("launcher: memory guard: stopped for this rise reason=not fallen reading=%d endable=%d ", 3600*mib, 940*mib)
			if len(stopped) != 1 || !strings.HasPrefix(stopped[0], want) {
				t.Fatalf("the stopped lines are %q, want one starting %q", stopped, want)
			}
		})
	}
}

// A process the guard has killed is never ended or reported again while it has not exited, even
// where it is the largest and still first in the order when the wait passes: pid 302, a fork of
// 900 MiB whose end frees nothing, never exits. While it lingers it is left out of what the guard
// may end, of their sum against the floor and of the growth since the kill. Where pid 301 has
// grown since the kill and pids 301 and 303 hold the floor, the guard ends pid 301 at the reading
// at which the wait has passed. Where only pid 302 itself has grown, or pid 301 has but what the
// guard may still end, without pid 303, is under the floor, it gives the rise up with reason not
// fallen, and its stopped line counts pid 302, which still holds its memory, in endable and not in
// what it protects.
func TestMemoryGuardNeverEndsAKilledProcessAgainWhileItHasNotExited(t *testing.T) {
	for _, tc := range []struct {
		name string
		// grower is the pid that grows 20 MiB at the first reading after the kill, and idle
		// whether pid 303 is there.
		grower int
		idle   bool
		want   []int
		// endable is what the stopped line counts in endable, pid 302 among them, 0 where the
		// guard does not stop.
		endable uint64
	}{
		{"another grown since the kill", 301, true, []int{302, 301}, 0},
		{"only the killed process grown", 302, true, []int{302}, 920*mib + 500*mib},
		{"another grown, under the floor without the killed process", 301, false, []int{302}, 900*mib + 320*mib},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := newFakePod(t)
			procs := map[int]*fakeProcess{
				301: {pid: 301, ppid: 100, args: []string{"python3", "grow.py"}, anon: 300 * mib},
				302: {pid: 302, ppid: 100, args: []string{"python3", "pool.py"}, anon: 900 * mib},
			}
			if tc.idle {
				procs[303] = &fakeProcess{pid: 303, ppid: 100, args: []string{"python3", "idle.py"}, anon: 200 * mib}
			}
			pod.add(agent(2 * gib))
			for _, fp := range procs {
				pod.add(*fp)
			}
			pod.setAnon(3600 * mib)
			h := newHarness(t, pod, protect("tester", 100))
			// Pid 302's exit never comes.
			h.signal.onKill = func(int) bool { return false }
			h.run()
			h.requireKilled(302)
			readings := int(Defaults().Wait / Defaults().Interval)
			for i := 1; i < readings; i++ {
				if i == 1 {
					pod.resize(procs[tc.grower], procs[tc.grower].anon+20*mib)
				}
				h.tick()
				h.requireKilled(302)
			}
			h.tick()
			h.requireKilled(tc.want...)
			if tried := h.signal.attempts(); !slices.Equal(tried, tc.want) {
				t.Fatalf("the guard tried to kill %v, want %v, each once", tried, tc.want)
			}
			stopped := h.log.with("stopped")
			if tc.endable == 0 {
				if len(stopped) != 0 {
					t.Fatalf("the guard gave the rise up though pid 301 grew since the kill: %q", stopped)
				}
				return
			}
			want := fmt.Sprintf("launcher: memory guard: stopped for this rise reason=not fallen reading=%d endable=%d protected=%d unread=0 ",
				3600*mib, tc.endable, 2*gib)
			if len(stopped) != 1 || !strings.HasPrefix(stopped[0], want) {
				t.Fatalf("the stopped lines are %q, want one starting %q", stopped, want)
			}
		})
	}
}

// Growth since a kill under growthNoise, 1 MiB, is none: after the kill of pid 302, which exits
// and frees nothing, pid 301 grows 1020 KiB, and the guard ends nothing and, once the wait has
// passed, gives the rise up with reason not fallen; 1 MiB larger, pid 301 is ended at once.
func TestMemoryGuardCountsGrowthSinceAKillUnderOneMebibyteAsNone(t *testing.T) {
	for _, tc := range []struct {
		name   string
		growth uint64
		want   []int
	}{
		{"1020 KiB", 1020 * kib, []int{302}},
		{"1 MiB", mib, []int{302, 301}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := newFakePod(t)
			grower := fakeProcess{pid: 301, ppid: 100, args: []string{"python3", "grow.py"}, anon: 300 * mib}
			pod.add(
				agent(2*gib),
				grower,
				fakeProcess{pid: 302, ppid: 100, args: []string{"python3", "pool.py"}, anon: 900 * mib},
				fakeProcess{pid: 303, ppid: 100, args: []string{"python3", "idle.py"}, anon: 200 * mib},
			)
			pod.setAnon(3600 * mib)
			h := newHarness(t, pod, protect("tester", 100))
			h.signal.onKill = exitFreeing(pod, 0)
			h.run()
			h.requireKilled(302)
			pod.resize(&grower, grower.anon+tc.growth)
			h.tick()
			h.requireKilled(tc.want...)
			if stopped := h.log.with("stopped"); len(stopped) != 0 {
				t.Fatalf("the guard gave the rise up before the wait passed: %q", stopped)
			}
			if len(tc.want) == 2 {
				return
			}
			h.tickAfter(Defaults().Wait)
			h.requireKilled(tc.want...)
			want := fmt.Sprintf("launcher: memory guard: stopped for this rise reason=not fallen reading=%d endable=%d ",
				3600*mib, 500*mib+tc.growth)
			if stopped := h.log.with("stopped"); len(stopped) != 1 || !strings.HasPrefix(stopped[0], want) {
				t.Fatalf("the stopped lines are %q, want one starting %q", stopped, want)
			}
		})
	}
}

// A rise that is a protected process's own still costs at most one kernel at the controller pod's
// settings, though the guard decides at each reading once the killed kernel has exited: Oh My Pi
// grows at every reading, the reading never falls, and neither kernel grows, so at none of the
// readings of the controller's wait of 1 second does the guard end the other kernel or give up,
// and at the reading at which the wait has passed it gives the rise up with reason not fallen and
// ends nothing more, however far Oh My Pi grows.
func TestMemoryGuardEndsOneKernelForAProtectedRiseThoughItDecidesAtEachReadingAfterTheExit(t *testing.T) {
	pod := newFakePod(t)
	omp := agent(2000 * mib)
	pod.add(
		omp,
		fakeProcess{pid: 200, ppid: 100, args: []string{"python3", "-m", "kernel"}, anon: 600 * mib},
		fakeProcess{pid: 201, ppid: 100, args: []string{"bun", "eval.js"}, anon: 500 * mib},
	)
	pod.setAnon(3100 * mib)
	h := newHarness(t, pod, protect("tester", 100))
	settings := ControllerDefaults()
	h.g.Apply(&shimwire.LauncherMemoryGuard{
		ActPercent: settings.ActPercent, FloorPercent: settings.FloorPercent,
		WaitSeconds: int(settings.Wait / time.Second), IntervalMilliseconds: int(settings.Interval / time.Millisecond),
	})
	h.signal.onKill = exitFreeing(pod, 0)
	h.run()
	grow := func(by uint64) {
		t.Helper()
		pod.resize(&omp, omp.anon+by)
		pod.setAnon(pod.anon + by)
	}
	for range 4 {
		grow(100 * mib)
		h.tick()
	}
	h.requireKilled(200)
	readings := int(settings.Wait / settings.Interval)
	for i := 1; i < readings; i++ {
		grow(40 * mib)
		h.tick()
		h.requireKilled(200)
		if stopped := h.log.with("stopped"); len(stopped) != 0 {
			t.Fatalf("at reading %d after the kernel's exit, before the wait passed, the guard gave the rise up: %q", i, stopped)
		}
	}
	grow(40 * mib)
	h.tick()
	want := fmt.Sprintf("launcher: memory guard: stopped for this rise reason=not fallen reading=%d endable=%d ", 3900*mib, 500*mib)
	for range 3 {
		grow(10 * mib)
		h.tick()
	}
	h.requireKilled(200)
	if stopped := h.log.with("stopped"); len(stopped) != 1 || !strings.HasPrefix(stopped[0], want) {
		t.Fatalf("the stopped lines are %q, want one starting %q", stopped, want)
	}
}

// The sizes as of a kill keep their storage from one reading to the next: measuring the growth
// since the kill of processes it has read since the kill allocates nothing, and keeps one size for
// each of them.
func TestMemoryGuardMeasuresGrowthSinceAKillAllocatingNothingForAProcessReadBefore(t *testing.T) {
	h := newHarness(t, newFakePod(t), nil)
	cs := make([]candidate, 50)
	for i := range cs {
		cs[i] = candidate{Process: &Process{PID: 1000 + i, StartTime: uint64(i)}, size: uint64(i) * mib}
	}
	now := testStart
	h.g.keepKillSizes(now, cs)
	allocs := testing.AllocsPerRun(100, func() {
		now = now.Add(Defaults().Interval)
		if h.g.grownSinceKill(now, cs) {
			t.Fatal("a process whose size has not changed since the kill counts as grown")
		}
	})
	if allocs != 0 {
		t.Fatalf("a reading's measure of growth since the kill allocates %v times, want none", allocs)
	}
	if len(h.g.atKill) != len(cs) {
		t.Fatalf("the guard keeps the sizes as of the kill of %d processes, want %d", len(h.g.atKill), len(cs))
	}
}

// At an interval above the window, 500 ms, no read has an earlier one within 300 ms of it: no
// process counts as grown, and the guard ends the largest first, though pid 300 grew 50 MiB at
// each reading.
func TestMemoryGuardEndsTheLargestFirstAtAnIntervalAboveTheWindow(t *testing.T) {
	pod := newFakePod(t)
	grower := fakeProcess{pid: 300, ppid: 100, args: []string{"python3", "grow.py"}, anon: 400 * mib}
	pod.add(agent(2*gib), grower, fakeProcess{pid: 301, ppid: 100, args: []string{"python3", "idle.py"}, anon: 800 * mib})
	pod.setAnon(3100 * mib)
	h := newHarness(t, pod, protect("tester", 100))
	h.g.Apply(&shimwire.LauncherMemoryGuard{ActPercent: 85, FloorPercent: 10, WaitSeconds: 5, IntervalMilliseconds: 500})
	h.run()
	if h.pending.d != 500*time.Millisecond {
		t.Fatalf("the guard waits %v between readings, want 500ms", h.pending.d)
	}
	for _, reading := range []uint64{3300, 3500} {
		pod.resize(&grower, grower.anon+50*mib)
		pod.setAnon(reading * mib)
		h.tick()
	}
	h.requireKilled(301)
}

// underTheFloor is a pod at 3500 MiB, above the mark, whose processes the guard may end hold 400
// MiB between them, under the floor: each crossing of the mark is a stop with nothing to end. Its
// agent, pid 100, is the one protected process.
func underTheFloor(t *testing.T) *fakePod {
	t.Helper()
	pod := newFakePod(t)
	pod.add(agent(3 * gib))
	for i := range 10 {
		pod.add(fakeProcess{pid: 500 + i, ppid: 100, args: []string{"sleep", "600"}, anon: 40 * mib})
	}
	pod.setAnon(3500 * mib)
	return pod
}

// A stop that repeats writes its line at most once a minute by the guard's clock. The reading
// crosses the mark up and down within the minute with nothing to end: the first stop writes its
// line and each later one is left out, and the first stop after the minute writes the line again,
// counting the stops left out. Only a written line reads the smaps of the processes the guard may
// not end; every crossing reads the smaps of those it may end twice, at the reading of 3400 MiB in
// the band below the mark (3072 to 3481.6 MiB), which ends the stopped phase, and at the reading
// at the mark, which decides whether it acts: the sizes the band held are under the floor, so that
// reading assesses its own (decide).
func TestMemoryGuardWritesARepeatingStopsLineAtMostOnceAMinute(t *testing.T) {
	h := newHarness(t, underTheFloor(t), protect("tester", 100))
	var mu sync.Mutex
	reads := map[int]int{}
	h.g.smapsRead = func(pid int) {
		mu.Lock()
		defer mu.Unlock()
		reads[pid]++
	}
	readsOf := func(pid int) int {
		mu.Lock()
		defer mu.Unlock()
		return reads[pid]
	}
	stopped := func(leftOut int) string {
		return fmt.Sprintf("launcher: memory guard: stopped for this rise reason=nothing to end reading=%d endable=%d protected=%d unread=0 unheld=%d limit=%d left_out=%d unread_processes=none",
			3500*mib, 400*mib, 3*gib, 28*mib, podLimit, leftOut)
	}
	h.run()
	if got := h.log.with("stopped"); !slices.Equal(got, []string{stopped(0)}) {
		t.Fatalf("the stopped lines are %q, want %q", got, stopped(0))
	}
	if agent, endable := readsOf(100), readsOf(500); agent != 1 || endable != 1 {
		t.Fatalf("the first stop read the agent's smaps %d times and an endable process's %d, want once each", agent, endable)
	}

	const crossings = 4
	for i := range crossings {
		h.pod.setAnon(3400 * mib)
		h.tick()
		h.pod.setAnon(3500 * mib)
		h.tick()
		if got := h.log.with("stopped"); len(got) != 1 {
			t.Fatalf("after crossing %d within the minute the stopped lines are %q, want the first alone", i+1, got)
		}
		if agent, endable := readsOf(100), readsOf(500); agent != 1 || endable != 1+2*(i+1) {
			t.Fatalf("after crossing %d the agent's smaps were read %d times and an endable process's %d, want 1 and %d",
				i+1, agent, endable, 1+2*(i+1))
		}
	}
	if spent := h.clock.Now().Sub(testStart); spent >= stoppedEvery {
		t.Fatalf("the crossings took %s, not within the minute", spent)
	}

	h.pod.setAnon(3400 * mib)
	h.tick()
	h.pod.setAnon(3500 * mib)
	h.tickAfter(stoppedEvery)
	if got, want := h.log.with("stopped"), []string{stopped(0), stopped(crossings)}; !slices.Equal(got, want) {
		t.Fatalf("after the minute the stopped lines are\n%q\nwant\n%q", got, want)
	}
	if agent, endable := readsOf(100), readsOf(500); agent != 2 || endable != 1+2*(crossings+1) {
		t.Fatalf("after the minute the agent's smaps were read %d times and an endable process's %d, want 2 and %d",
			agent, endable, 1+2*(crossings+1))
	}
	h.requireKilled()
}

// A stop left out of the log changes nothing the guard does: within the minute of a written
// stopped line, after a stop it left out, a crossing at which the processes it may end hold the
// floor ends the largest of them at once.
func TestMemoryGuardEndsAtOnceOnACrossingWithinTheMinuteOfAStoppedLine(t *testing.T) {
	pod := underTheFloor(t)
	h := newHarness(t, pod, protect("tester", 100)).run()
	pod.setAnon(3400 * mib)
	h.tick()
	pod.setAnon(3500 * mib)
	h.tick()
	if got := h.log.with("stopped"); len(got) != 1 {
		t.Fatalf("the stopped lines are %q, want the first alone", got)
	}

	// Something the guard may end grows below the mark, and the next crossing ends it.
	pod.setAnon(3400 * mib)
	pod.add(fakeProcess{pid: 520, ppid: 100, args: []string{"python3", "grow.py"}, anon: 500 * mib})
	h.tick()
	h.requireKilled()
	pod.setAnon(4000 * mib)
	h.tick()
	h.requireKilled(520)
	if spent := h.clock.Now().Sub(testStart); spent >= stoppedEvery {
		t.Fatalf("the crossings took %s, not within the minute", spent)
	}
	if got := h.log.with("stopped"); len(got) != 1 {
		t.Fatalf("the stopped lines are %q, want the first alone", got)
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

// A process can exit between the scan's read of its stat and the kill, and until its parent reaps
// it, it keeps its stat and its start time: Open holds it, the start time reads unchanged, and the
// kill answers no error. In a gVisor pod on 2026-11-14, pidfd_send_signal to a child that had
// exited and that its parent had not reaped answered no error, and the child's pidfd read readable
// about 1 ms after its exit. A guard that sent the kill would tell the role it had ended with
// SIGKILL a process that ended on its own. The guard asks the handle first: it sends no kill, logs
// no ended line, reports nothing and takes no sequence, and the next reading, whose scan leaves
// the zombie out, ends the next largest as the first kill it reports.
func TestMemoryGuardDoesNotKillOrReportAProcessThatExitedBeforeTheKill(t *testing.T) {
	pod := newFakePod(t)
	grow := fakeProcess{pid: 810, ppid: 100, start: 7000, args: []string{"python3", "grow.py"}, anon: gib}
	load := fakeProcess{pid: 811, ppid: 100, start: 7100, args: []string{"python3", "load.py"}, anon: 600 * mib}
	pod.add(agent(2600*mib), grow, load)
	pod.setAnon(3700 * mib)
	h := newHarness(t, pod, protect("tester", 100))
	// Pid 810 exits once the scan has read it: at its Open its stat reads a zombie's, with the
	// same start time, and its handle reads exited.
	h.signal.exit(810)
	h.signal.onOpen = func(pid int) {
		if pid == 810 {
			zombie := grow
			zombie.zombie = true
			pod.add(zombie)
		}
	}
	h.run()
	if tried := h.signal.attempts(); len(tried) != 0 {
		t.Fatalf("the guard tried to kill %v, a process that had exited; the log:\n%s", tried, strings.Join(h.log.all(), "\n"))
	}
	h.requireKilled()
	if lines := h.log.with("ended"); len(lines) != 0 {
		t.Fatalf("the guard logged a kill it did not send: %q", lines)
	}
	if held := h.signal.held(); held != 0 {
		t.Fatalf("the guard still holds %d processes after giving the kill up", held)
	}

	// Pid 810's memory went with its exit, and pid 811 grows the pod back to the mark.
	load.anon = 1100 * mib
	pod.add(load)
	pod.setAnon(3700 * mib)
	h.tick()
	h.requireKilled(811)
	if lines := h.log.with("ended"); len(lines) != 1 {
		t.Fatalf("the ended lines are %q, want pid 811's alone", lines)
	}
}

func TestMemoryGuardParsesAStatWhoseNameHoldsSpacesAndAParenthesis(t *testing.T) {
	for _, tc := range []struct{ name string }{{"omp daemon brok"}, {"x) S 1 2 3 4 (y"}, {"a)"}} {
		p, err := parseStat([]byte(statLine(42, tc.name, 7, 4242, 0)))
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
		fmt.Sprintf(`launcher: memory guard: hour start=2026-11-14T03:00:00Z end=2026-11-14T04:00:00Z reading_high=%d reading_high_at=%s reading_low=%d counter_high=%d counter_low=%d largest_pid=200 largest_role=tester largest_size=%d largest_executable="python3"`,
			3000*mib, highAt.Format(time.RFC3339), 1200*mib, 3584*mib, 1536*mib, 1500*mib),
		fmt.Sprintf(`launcher: memory guard: hour start=2026-11-14T04:00:00Z end=2026-11-14T05:00:00Z reading_high=%d reading_high_at=%s reading_low=%d counter_high=%d counter_low=%d largest_pid=100 largest_role=tester largest_size=%d largest_executable="omp"`,
			2200*mib, secondStart.Format(time.RFC3339), 2200*mib, 2*gib, 2*gib, gib),
	}
	if got := h.log.with("hour"); !slices.Equal(got, want) {
		t.Fatalf("the hourly lines are\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// The start line and the kill's line carry their fields, and the kill's line and its report name
// the process by the base name of its executable alone: no argument shows in any line, a token
// among them, and neither does a control character or a character that does not print.
func TestMemoryGuardLinesCarryTheirFieldsAndTheExecutableWithNoArgument(t *testing.T) {
	token := "ghp_" + strings.Repeat("A1", 18)
	pod := newFakePod(t)
	pod.add(agent(2*gib), fakeProcess{pid: 900, ppid: 100, exe: "/usr/bin/python3.13", anon: 1500 * mib, args: []string{
		"python3", "-c", "import os\nprint(1)", "\x1b[31mred", "\u202eabc", "--token", token, strings.Repeat("é", 400),
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
	ended := fmt.Sprintf(`launcher: memory guard: ended pid=900 role=tester size=%d reading=%d counter=%d shmem=%d limit=%d executable="python3.13"`,
		1500*mib, 3600*mib, 3900*mib, 64*mib, podLimit)
	if got := h.log.with("ended"); !slices.Equal(got, []string{ended}) {
		t.Fatalf("the kill lines are\n%q\nwant\n%q", got, ended)
	}
	k := h.killed()[0]
	want := Kill{LauncherMemoryKill: shimwire.LauncherMemoryKill{Guard: h.g.ID(), Sequence: 1, PID: 900, Executable: "python3.13",
		SizeBytes: 1500 * mib, ReadingBytes: 3600 * mib, CounterBytes: 3900 * mib, LimitBytes: podLimit, At: testStart}, Role: "tester"}
	if k != want {
		t.Fatalf("the report is %+v, want %+v", k, want)
	}
	if !regexp.MustCompile(`^[0-9a-f]{16}$`).MatchString(k.Guard) {
		t.Fatalf("the guard id is %q, want 16 hex digits", k.Guard)
	}
	if err := k.LauncherMemoryKill.Validate(); err != nil {
		t.Fatalf("the kill does not make a valid report: %v", err)
	}
	for _, line := range h.log.all() {
		if strings.Contains(line, token) || strings.Contains(line, "import os") || strings.Contains(line, "é") || strings.ContainsAny(line, "\n\t\x1b\u202e") {
			t.Fatalf("a line holds an argument, a control character or a rune that does not print: %q", line)
		}
	}
}

// requireKillNamesAlone has the guard end fp as pid 900, a child of a protected agent that holds
// most of pod's memory, at a reading of 3600 MiB and a counter of 3900 MiB, and fails, naming
// label, unless the kill's line and its report name the process by want alone. The line is matched
// whole, so it holds none of fp's arguments; hidden are texts of those arguments that no other line
// of the guard's may hold either.
func requireKillNamesAlone(t *testing.T, label string, pod *fakePod, fp fakeProcess, want string, hidden ...string) {
	t.Helper()
	fp.pid, fp.ppid, fp.anon = 900, 100, 1500*mib
	pod.add(agent(2*gib), fp)
	pod.setAnon(3600 * mib)
	pod.setCounter(3900 * mib)
	h := newHarness(t, pod, protect("tester", 100)).run()
	defer h.stop()
	h.requireKilled(900)
	ended := fmt.Sprintf(`launcher: memory guard: ended pid=900 role=tester size=%d reading=%d counter=%d shmem=%d limit=%d executable="%s"`,
		1500*mib, 3600*mib, 3900*mib, pod.shmem, podLimit, want)
	if got := h.log.with("ended"); !slices.Equal(got, []string{ended}) {
		t.Fatalf("%s: the kill lines are\n%q\nwant\n%q", label, got, ended)
	}
	if k := h.killed()[0]; k.Executable != want {
		t.Fatalf("%s: the report names %q, want %q", label, k.Executable, want)
	}
	for _, line := range h.log.all() {
		for _, text := range hidden {
			if strings.Contains(line, text) {
				t.Fatalf("%s: a line shows %q: %q", label, text, line)
			}
		}
	}
}

// No argument of a process shows in its kill's line or its report: the guard names the process by
// its executable alone (processName), which reads none of them (dispatch://INFRA-3557, W4). The
// input is the tester's, a token that is a URL with no `@` after a bearer label: measured in a
// gVisor issue pod on 2026-11-15 at 63bf5151 by the implementer (its handoff, version 8), the
// masking rules the guard then ran over its arguments gave `Bearer ***://u:SECRETPASS` for it.
func TestMemoryGuardKillShowsNoArgumentWithATokenThatIsAURLWithNoAtSignAfterALabel(t *testing.T) {
	requireKillNamesAlone(t, "curl -H Bearer http://u:SECRETPASS", newFakePod(t),
		fakeProcess{args: []string{"curl", "-H", "Bearer", "http://u:SECRETPASS"}}, "curl", "SECRETPASS", "Bearer", "-H")
}

// No argument of a process shows in its kill's line or its report however far its list runs: not
// a secret within the classifierArgsBytes the scan reads of it, and not one past them.
func TestMemoryGuardKillShowsNoArgumentWithASecretInsideOrPastClassifierArgsBytes(t *testing.T) {
	args := slices.Concat([]string{"curl", "--password=INSIDEPASS"}, slices.Repeat([]string{"w"}, 3000), []string{"--password=PASTPASS"})
	// The second secret must start past the bound, or the list proves nothing of what the scan
	// leaves unread.
	if at := strings.Index(strings.Join(args, "\x00"), "PASTPASS"); at < classifierArgsBytes {
		t.Fatalf("the last secret starts at byte %d of the arguments, within classifierArgsBytes (%d)", at, classifierArgsBytes)
	}
	requireKillNamesAlone(t, "a list past classifierArgsBytes", newFakePod(t), fakeProcess{args: args},
		"curl", "INSIDEPASS", "PASTPASS", "--password")
}

// A process is named by the base name of its executable where the scan read its `exe` link, and by
// its stat name where it could not, and never by an argument. Each character of either that does
// not print is escaped, and a name longer than shimwire.MemoryKillExecutableBytes, its escapes
// counted, is cut after the last character or escape that leaves room for the ellipsis.
func TestMemoryGuardNamesAProcessByItsExecutablesBaseNameOrItsStatName(t *testing.T) {
	longest := strings.Repeat("a", 255)
	for _, tc := range []struct {
		name string
		p    Process
		want string
	}{
		{"the executable's base name", Process{Name: "python3", Exe: "/usr/bin/python3.13", Args: []string{"python3", "-c", "SECRETPASS"}}, "python3.13"},
		{"the stat name where the exe link is unread", Process{Name: "grow", Args: []string{"/opt/bin/grow", "--password", "SECRETPASS"}}, "grow"},
		{"a control character in the executable", Process{Name: "grow", Exe: "/tmp/gr\x1b[31mow\n"}, `gr\x1b[31mow\n`},
		{"a control character in the stat name", Process{Name: "gr\tow\u202e"}, `gr\tow\u202e`},
		{"a byte that is not UTF-8", Process{Name: "grow", Exe: "/tmp/gr\xffow"}, `gr\xffow`},
		{"the longest base name, whole", Process{Name: "aaa", Exe: "/usr/bin/" + longest}, longest},
		{"an overlong name, cut between characters", Process{Name: "é", Exe: "/tmp/" + strings.Repeat("é", 200)}, strings.Repeat("é", 126) + ellipsis},
		{"an overlong name, cut between escapes", Process{Name: "x", Exe: "/tmp/" + strings.Repeat("\x01", 100)}, strings.Repeat(`\x01`, 63) + ellipsis},
	} {
		got := processName(&tc.p)
		if got != tc.want || len(got) > shimwire.MemoryKillExecutableBytes || !utf8.ValidString(got) {
			t.Errorf("%s: the name is %q (%d bytes), want %q", tc.name, got, len(got), tc.want)
		}
	}

	// The scan gives each its source: the link it read, and the stat of a process whose link it
	// could not read.
	pod := newFakePod(t)
	pod.add(
		fakeProcess{pid: 900, ppid: 1, exe: "/opt/tool/bin/gr\now", args: []string{"grow", "SECRETPASS"}},
		fakeProcess{pid: 901, ppid: 1, name: "gr\x1bow", unreadableExe: true, args: []string{"grow", "SECRETPASS"}},
	)
	table, err := scan(pod.proc)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		pid  int
		want string
	}{{900, `gr\now`}, {901, `gr\x1bow`}} {
		p, ok := table.Get(tc.pid)
		if !ok {
			t.Fatalf("the scan did not read pid %d", tc.pid)
		}
		if got := processName(p); got != tc.want {
			t.Errorf("pid %d is named %q, want %q", tc.pid, got, tc.want)
		}
	}
}

// The scan reads a process's arguments up to classifierArgsBytes and no further. Of a linker's
// mebibyte of arguments it reads the first ones intact, cuts the one the bound falls in, and reads
// none after it: the guard's loop runs a scan before its next reading after a kill, so the bound is
// what keeps a process's arguments from delaying that reading.
func TestMemoryGuardScanReadsAtMostClassifierArgsBytesOfAMebibyteOfArguments(t *testing.T) {
	pod := newFakePod(t)
	pod.add(fakeProcess{pid: 900, ppid: 1, args: []string{"/usr/bin/ld"}})
	var b strings.Builder
	b.WriteString("/usr/bin/ld\x00-o\x00app\x00")
	for i := 0; b.Len() < 1<<20; i++ {
		fmt.Fprintf(&b, "build/obj/module%05d.o\x00", i)
	}
	cmdline := b.String()
	pod.writeWhole(filepath.Join(pod.proc, "900", "cmdline"), cmdline)
	table, err := scan(pod.proc)
	if err != nil {
		t.Fatal(err)
	}
	p, ok := table.Get(900)
	if !ok {
		t.Fatal("the scan did not read pid 900")
	}
	// Joined by NUL, the arguments are the bytes of cmdline the scan read, less a NUL that ends them.
	read := strings.Join(p.Args, "\x00")
	if len(read) > classifierArgsBytes {
		t.Fatalf("the scan read %d bytes of the arguments, want at most classifierArgsBytes, %d", len(read), classifierArgsBytes)
	}
	if read == strings.TrimSuffix(cmdline, "\x00") {
		t.Fatalf("the scan read all %d bytes of a linker's mebibyte of arguments, want classifierArgsBytes (%d) to cut them", len(read), classifierArgsBytes)
	}
	if want := strings.TrimSuffix(cmdline[:min(len(cmdline), classifierArgsBytes)], "\x00"); read != want {
		t.Fatalf("the scan read the arguments as %.80q… (%d bytes), want the first %d bytes of cmdline, %.80q…", read, len(read), len(want), want)
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

// When the guard stops for a rise, its line names each process it could not read: the pid, and the
// size where its memory map reads; it ends neither (INFRA-3531 R3 of spec version 29). Pid 960's
// exe and smaps do not read, as a non-dumpable process's in the container's own user namespace did
// under gVisor; pid 961's exe does not read and its smaps reads 500 MiB, as a process whose main
// thread exited while another holds its memory. Beside the agent, the processes the guard may end
// hold 200 MiB, under the floor, so at the mark it gives the rise up with reason nothing to end,
// and its line reads unread_processes=960,961:524288000 after every field it carried before.
func TestMemoryGuardNamesInItsStoppedLineEachProcessItCouldNotReadAndEndsNone(t *testing.T) {
	pod := newFakePod(t)
	pod.add(agent(3 * gib))
	for i := range 5 {
		pod.add(fakeProcess{pid: 500 + i, ppid: 100, args: []string{"sleep", "600"}, anon: 40 * mib})
	}
	pod.add(
		fakeProcess{pid: 960, ppid: 100, args: []string{"python3", "private.py"}, anon: gib, unreadableExe: true, unreadableSmaps: true},
		fakeProcess{pid: 961, ppid: 100, args: []string{"python3", "threads.py"}, anon: 500 * mib, unreadableExe: true},
	)
	pod.setAnon(3500 * mib)
	h := newHarness(t, pod, protect("tester", 100))
	h.run()
	h.tick()
	h.requireKilled()
	if tried := h.signal.attempts(); len(tried) != 0 {
		t.Fatalf("the guard tried to kill %v, want none", tried)
	}
	endable, unread := 200*mib, 500*mib
	want := fmt.Sprintf("launcher: memory guard: stopped for this rise reason=nothing to end reading=%d endable=%d protected=%d unread=%d unheld=%d limit=%d left_out=0 unread_processes=960,961:%d",
		3500*mib, endable, 3*gib, unread, int64(3500*mib)-int64(endable+3*gib+unread), podLimit, unread)
	if got := h.log.with("stopped"); !slices.Equal(got, []string{want}) {
		t.Fatalf("the stopped lines are %q, want %q", got, want)
	}
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
	want := fmt.Sprintf("launcher: memory guard: stopped for this rise reason=nothing to end reading=%d endable=0 protected=%d unread=%d unheld=%d limit=%d left_out=0 unread_processes=950:%d",
		3700*mib, gib, 2*gib, 3700*mib-3*gib, podLimit, 2*gib)
	if got := h.log.with("stopped"); !slices.Equal(got, []string{want}) {
		t.Fatalf("the stopped lines are %q, want %q", got, want)
	}
}

// A zombie has exited and holds no memory, whatever its exe link answers. Linux answers a zombie's
// exe link with ENOENT, which the scan already takes for an ended process. gVisor answers EACCES,
// because its procfs refuses the exe link of a task with no memory manager: in a gVisor pod on
// 2026-11-14, the head's launcher, run as the container's first process, logged
// `unread pid=5684 reason=read the exe: readlink /proc/5684/exe: permission denied` right after it
// ended pid 5684, a pool worker whose parent had not reaped it yet, and the same for the next two
// workers it ended. The guard leaves a zombie out of its scan whatever its exe link answers (a
// plain file stands in for gVisor's refusal here), so it logs no unread line for one, and the
// process it may end is still ended.
func TestMemoryGuardLeavesAZombieOutWhateverItsExeLinkAnswers(t *testing.T) {
	pod := newFakePod(t)
	pod.add(
		agent(gib),
		fakeProcess{pid: 961, ppid: 100, args: []string{"python3", "pool.py"}, anon: 600 * mib},
		fakeProcess{pid: 960, ppid: 961, args: []string{"python3", "pool.py"}, zombie: true, unreadableExe: true},
	)
	pod.setAnon(3700 * mib)
	h := newHarness(t, pod, protect("tester", 100))
	h.run()
	h.requireKilled(961)
	if unread := h.log.with("unread"); len(unread) != 0 {
		t.Fatalf("the guard logged %q for a zombie, which holds no memory; the log:\n%s", unread, strings.Join(h.log.all(), "\n"))
	}
}

// A process whose main thread alone has exited (pthread_exit) while its other threads run reads
// `Z` in its stat, its main thread's state, as a zombie does, yet it is alive and holds all of its
// memory; its status counts the threads left. The guard reads a process in state Z whose status
// counts more than one thread as a live one: the scan lists it, and the guard ends it as the
// largest it may end.
func TestMemoryGuardEndsAProcessInStateZWhoseOtherThreadsRun(t *testing.T) {
	pod := newFakePod(t)
	pod.add(
		agent(gib),
		fakeProcess{pid: 970, ppid: 100, args: []string{"python3", "threads.py"}, anon: 1500 * mib, zombie: true, threads: 3},
		fakeProcess{pid: 971, ppid: 100, args: []string{"python3", "grow.py"}, anon: 600 * mib},
	)
	table, err := scan(pod.proc)
	if err != nil {
		t.Fatal(err)
	}
	if p, ok := table.Get(970); !ok || p.Exe != "/usr/bin/python3" {
		t.Fatalf("the scan read pid 970 as %+v, listed %v; want it listed with its exe", p, ok)
	}
	pod.setAnon(3700 * mib)
	h := newHarness(t, pod, protect("tester", 100)).run()
	h.requireKilled(970)
	if k := h.killed()[0]; k.SizeBytes != 1500*mib {
		t.Fatalf("the killed process's size is %d, want %d", k.SizeBytes, 1500*mib)
	}
	if unread := h.log.with("unread"); len(unread) != 0 {
		t.Fatalf("the unread lines are %q, want none", unread)
	}
}

// A process can exit between the scan's read of its stat and a later read of its files, and under
// gVisor a zombie answers the read of its exe link with EACCES. The guard reads the stat of a
// process whose exe, ns/pid or smaps it could not read again before it counts the process unread,
// and leaves out one that has exited since: no unread line, and not among the processes it may
// end. Here pid 980's stat reads S at the scan's first read and Z at any later one, and either its
// exe link cannot be read (a plain file stands in for gVisor's refusal) or its smaps cannot (a
// directory, whose read answers EISDIR). A process whose exe cannot be read while it runs still
// counts unread (TestMemoryGuardDoesNotEndAProcessItCannotRead).
func TestMemoryGuardLeavesOutAProcessThatExitsWhileItIsRead(t *testing.T) {
	for _, tc := range []struct {
		name          string
		unreadableExe bool
		// listed is whether the scan lists pid 980: it does when the read that fails is assess's
		// of the smaps, after the scan.
		listed bool
		exits  func(pod *fakePod, h *harness)
	}{
		{"its exe", true, false, func(pod *fakePod, _ *harness) { pod.exitAsItsCmdlineIsRead(980) }},
		{"its smaps", false, true, func(pod *fakePod, h *harness) {
			h.g.smapsRead = func(pid int) {
				if pid != 980 {
					return
				}
				dir := filepath.Join(pod.proc, strconv.Itoa(pid))
				stat, err := os.ReadFile(filepath.Join(dir, "stat"))
				if err != nil {
					pod.t.Fatal(err)
				}
				pod.writeWhole(filepath.Join(dir, "stat"), strings.Replace(string(stat), ") S ", ") Z ", 1))
				if err := os.Remove(filepath.Join(dir, "smaps")); err != nil {
					pod.t.Fatal(err)
				}
				if err := os.Mkdir(filepath.Join(dir, "smaps"), 0o755); err != nil {
					pod.t.Fatal(err)
				}
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := newFakePod(t)
			pod.add(
				agent(gib),
				fakeProcess{pid: 980, ppid: 100, args: []string{"python3", "pool.py"}, anon: 600 * mib, unreadableExe: tc.unreadableExe},
				fakeProcess{pid: 981, ppid: 100, args: []string{"python3", "grow.py"}, anon: 500 * mib},
			)
			h := newHarness(t, pod, protect("tester", 100))
			tc.exits(pod, h)
			a, err := h.g.assess(h.clock.Now())
			if err != nil {
				t.Fatal(err)
			}
			if unread := h.log.with("unread"); len(unread) != 0 {
				t.Fatalf("the guard logged %q for a process that exited while it was read", unread)
			}
			if err, unread := a.table.unread[980]; unread {
				t.Fatalf("pid 980 counts unread (%v), want it left out", err)
			}
			if _, listed := a.table.Get(980); listed != tc.listed {
				t.Fatalf("pid 980 listed %v, want %v", listed, tc.listed)
			}
			var endable []int
			for _, c := range a.endable {
				endable = append(endable, c.PID)
			}
			if !slices.Equal(endable, []int{981}) {
				t.Fatalf("the processes the guard may end are %v, want 981 alone", endable)
			}
		})
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
	stopped := fmt.Sprintf("launcher: memory guard: stopped for this rise reason=nothing to end reading=%d endable=0 protected=%d unread=%d unheld=%d limit=%d left_out=0 unread_processes=1000:%d",
		3800*mib, 2*gib, gib, 3800*mib-3*gib, podLimit, gib)
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

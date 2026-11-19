package memoryguard

import (
	"context"
	"errors"
	"fmt"
	"maps"
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
	// noExe leaves exe out, whose readlink fails with ENOENT, as the exited leader of a process
	// whose other threads run answers on Linux, its memory map cleared.
	noExe bool
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
		// A smaps an earlier write made unreadable is a directory, which no file replaces.
		if info, err := os.Lstat(smaps); err == nil && info.IsDir() {
			_ = os.RemoveAll(smaps)
		}
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
		switch {
		case link.name == "exe" && fp.noExe:
			continue
		case link.name == "exe" && fp.unreadableExe:
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
// 262 MiB while its kill frees only the 4 MiB it wrote itself. Each worker runs its parent's
// program, so while the guard reads the parent each ranks by what it holds over the parent's size,
// nothing (INFRA-3531 R1 of spec version 33), and the parent, the largest, ends first. That small
// fall is a fall: with the reading still at the mark, the guard ends the next, the newest worker,
// which no read parent discounts any longer, one at a time, until the reading is below the mark.
func TestMemoryGuardEndsAPoolsParentAndThenTheNextForkedWorkerWhenAKillFreesLittle(t *testing.T) {
	pod := newFakePod(t)
	pod.add(
		agent(3214*mib),
		fakeProcess{pid: 700, ppid: 100, start: 5000, args: []string{"make", "-j4"}, anon: 262 * mib},
		fakeProcess{pid: 701, ppid: 700, start: 5100, args: []string{"make", "-j4"}, anon: 262 * mib},
		fakeProcess{pid: 702, ppid: 700, start: 5200, args: []string{"make", "-j4"}, anon: 262 * mib},
		fakeProcess{pid: 703, ppid: 700, start: 5300, args: []string{"make", "-j4"}, anon: 262 * mib},
	)
	// The agent, the pool's 262 MiB once, and the 4 MiB each process wrote: 3488 MiB, above the
	// mark of 3481.6 MiB.
	pod.setAnon(3488 * mib)
	h := newHarness(t, pod, protect("tester", 100))
	h.signal.onKill = exitFreeing(pod, 4*mib)
	h.run()
	h.requireKilled(700)
	requireChosenBy(h, 700, "new", "reading")
	// 3484 MiB: 4 MiB fallen and still at the mark, so the next: of the workers' equal sizes, the
	// newest.
	h.tick()
	h.requireKilled(700, 703)
	// 3480 MiB is below the mark: the rise is over.
	for range 3 {
		h.tick()
	}
	h.requireKilled(700, 703)
	if lines := h.log.with("stopped"); len(lines) != 0 {
		t.Fatalf("the guard gave up on a rise its kills lowered: %q", lines)
	}
}

// A kill that lowers the reading by nothing within the wait freed nothing: the killed parent held
// only pages its forked child keeps. Its child runs its program, so while the guard reads the
// parent the child ranks by what it holds over the parent's size, nothing, and the parent ends
// first (INFRA-3531 R1 of spec version 33). The guard logs the stopped line once the wait passes
// and ends nothing more until the reading has gone below the mark.
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
	h.requireKilled(700)
	for range 3 {
		h.tick()
	}
	h.requireKilled(700)
	if lines := h.log.with("stopped"); len(lines) != 0 {
		t.Fatalf("the guard gave up before its wait passed: %q", lines)
	}
	h.tickAfter(5 * time.Second)
	h.requireKilled(700)
	want := fmt.Sprintf("launcher: memory guard: stopped for this rise reason=not fallen reading=%d endable=%d protected=%d unread=0 unheld=%d limit=%d left_out=0 unread_processes=none",
		3692*mib, 462*mib, 3230*mib, 0, podLimit)
	if got := h.log.with("stopped"); !slices.Equal(got, []string{want}) {
		t.Fatalf("the stopped lines are %q, want one: %q", got, want)
	}
	for range 3 {
		h.tick()
	}
	h.requireKilled(700)
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
	for key, r := range h.g.sizes {
		if most := int(growthWindow/Defaults().Interval) + 1; len(r.window) > most || r.readings != 5 {
			t.Fatalf("the guard keeps %d reads of pid %d over %d readings, want at most %d, the window's, over the band's 5",
				len(r.window), key.pid, r.readings, most)
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
// only then assesses the reading, whose sizes are the window's newest reads; each process's size
// as of the kill is the one the decision read, the band's (INFRA-3531 R1: its size at the reading
// the kill was sent at). Pid 301 grows 20 MiB between the two readings in the band and pid 300
// none, so by the band's reads pid 301 is the one that grew; pid 300 then grows 60 MiB as the
// reading reaches the mark, which a read there would rank first. The guard ends pid 301, at the
// size the band read, and reads pid 300's growth only after the kill, where it counts as growth
// since the kill. The reading at the mark is a new high of the hour 200 MiB above the high the last
// scan started at, more than 1% of the limit, so a scan starts at that reading, and it reads every
// process's smaps, the agent's among them, only after the kill. Where a scan runs as the guard opens
// its target, the rig lets it finish first, as a pod whose CPU the host throttles may, so a scan
// started before the kill shows all its reads before it.
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
		key          procKey
		atKill, read uint64
	}{{procKey{pid: 300, start: 3000}, 400 * mib, 460 * mib}, {procKey{pid: 301, start: 3010}, 600 * mib, 600 * mib}} {
		if got, ok := h.g.atKill[want.key]; !ok || got != want.atKill {
			t.Fatalf("the size as of the kill of pid %d is %d (kept %t), want %d, the band's read the decision ended on", want.key.pid, got, ok, want.atKill)
		}
		if reads := h.g.sizes[want.key].window; len(reads) == 0 || reads[len(reads)-1] != (sizeRead{at: now, size: want.read}) {
			t.Fatalf("the reads of pid %d are %+v, want the newest %d at the reading at the mark", want.key.pid, reads, want.read)
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
// held reads nothing grew and the largest would end first: the parent, the forks running its
// program ranking by what they hold over its size (INFRA-3531 R1 of spec version 33), and its end
// freeing nothing the forks keep. The guard reads again the smaps of the grower alone, which grew
// between its band reads, and not those of the pool, flat through the band's five readings, nor
// the agent's; it sees the grower's crossing step and ends it, at the size it reads there, before
// it scans the process table or starts the hour's scan; it keeps one read of it at that reading.
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
	held := h.g.held
	if len(held) != 5 || held[0].PID != 700 || held[0].growth != 0 || held[1].PID != 710 {
		t.Fatalf("the band's last reading holds %+v, want the five pool and grower processes, the parent first, then the grower, and none grown", held)
	}
	for _, c := range held[2:] {
		if c.PPID != 700 || c.lessParent != block {
			t.Fatalf("the band's last reading holds %+v, want each fork less its parent's %d bytes", held, block)
		}
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
	if got := h.g.sizes[procKey{pid: 710, start: 5400}].window; len(got) == 0 || got[len(got)-1] != (sizeRead{at: now, size: 864 * mib}) ||
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
// Six growers each have one band read, so each is new to the stay and the guard would read all six
// again: it reads them the newest first, and once the budget has passed it stops after the read in
// hand and decides on what it read, the rest at their held sizes and no growth. Pids 805 to 800,
// the newest first, are held at 100 up to 150 MiB, and as the reading reaches the mark they grow
// 10, 20, 30, 40, 50 and 200 MiB, the most grown read last. At 20 ms a read, it reads pids 805, 804
// and 803 and ends pid 803, the most grown of those; at 60 ms a read, it reads pid 805 alone and
// ends it; where the reading at the mark itself took 80 ms, more than the budget, it still reads
// pid 805, the first, and ends it; where the reads take no time, the budget is never reached, it
// reads all six, and ends pid 800, the most grown, as with no budget. Each line says the process
// grew, by sizes read again.
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
			requireChosenBy(h, tc.want, "grew", "reread")
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

// requireChosenBy fails unless the guard's one ended line for pid says rule put it first, names
// sizes as where the sizes it was chosen by came from and takes no parent's size off, just before
// the executable (INFRA-3531 R3 of spec version 31, R1 of spec version 33).
func requireChosenBy(h *harness, pid int, rule, sizes string) {
	h.t.Helper()
	requireRankedBy(h, pid, rule, sizes, 0)
}

// requireRankedBy is requireChosenBy with lessParent the bytes the line says the process's rank
// did not count.
func requireRankedBy(h *harness, pid int, rule, sizes string, lessParent uint64) {
	h.t.Helper()
	requireEndedWith(h, pid, fmt.Sprintf(" rule=%s sizes=%s less_parent=%d new_read=", rule, sizes, lessParent))
}

// requireNewRead fails unless the guard's one ended line for pid says newRead of what the reads at
// the mark read of the processes new to the stay, just before the executable (lines.go).
func requireNewRead(h *harness, pid int, newRead string) {
	h.t.Helper()
	requireEndedWith(h, pid, " new_read="+newRead+" executable=")
}

// requireEndedWith fails unless the guard has one ended line for pid, and it holds fields.
func requireEndedWith(h *harness, pid int, fields string) {
	h.t.Helper()
	lines := h.log.with(fmt.Sprintf("ended pid=%d ", pid))
	if len(lines) != 1 || !strings.Contains(lines[0], fields) {
		h.t.Fatalf("the ended lines of pid %d are %q, want one with %q; the log:\n%s", pid, lines, fields, strings.Join(h.log.all(), "\n"))
	}
}

// Of the held processes new to the stay, the reads again at the mark (rereadHeld) take the newest
// by start time first (INFRA-3531 R1 of spec version 31): a runaway that began just before the
// band's last reading is among the newest. A parent holds a block of 1.5 GiB through the band's
// two readings; at the second, it has forked pid 701, which reports the block as its own, and pid
// 710, newer, has started at 50 MiB. Each has that one band read. As the reading reaches the mark
// pid 710 grows 64 MiB, and the reads take 60 ms each, so the budget allows one: the guard reads
// pid 710 alone and ends it, the one that grew.
func TestMemoryGuardReadsAgainAtTheMarkTheNewestProcessNewToTheStayFirst(t *testing.T) {
	const block = 1536 * mib
	pod := newFakePod(t)
	pod.add(agent(gib), fakeProcess{pid: 700, ppid: 100, start: 5000, args: []string{"python3", "pool.py"}, anon: block})
	pod.setAnon(3200 * mib)
	h := newHarness(t, pod, protect("tester", 100))
	reads := recordReads(h)
	_, perRead := slowMark(h)
	h.run()
	grower := fakeProcess{pid: 710, ppid: 100, start: 5100, args: []string{"python3", "grow.py"}, anon: 50 * mib}
	pod.add(fakeProcess{pid: 701, ppid: 700, start: 5050, args: []string{"python3", "pool.py"}, anon: block}, grower)
	pod.setAnon(3300 * mib)
	h.tick()
	inTheBand := len(reads())
	pod.resize(&grower, grower.anon+64*mib)
	pod.setAnon(3500 * mib)
	perRead.Store(int64(60 * time.Millisecond))
	h.tick()
	requireMarkKill(h, 710, 114*mib)
	requireChosenBy(h, 710, "grew", "reread")
	if got, want := readBeforeTheKill(reads()[inTheBand:]), []int{710}; !slices.Equal(got, want) {
		t.Fatalf("before the kill the guard read the smaps of %v, want the newest's alone: %v", got, want)
	}
}

// The reads again at the mark (rereadHeld) take the held processes in a fixed order: first those
// that grew in the band, the most grown across the stay first, then those new to the stay, the
// newest by start time first; one flat through two band reads or more is not read. Pid 710 grows
// 50 MiB and pid 711 20 MiB between the band's first two readings, and pid 700 sleeps through all
// four; pid 722, of 200 MiB, and pids 720 and 721, both of 300 MiB, pid 721 the newest and pid 722
// the oldest, start at the band's last reading. Nothing grows at the mark, and the guard reads pids
// 710, 711, 721, 720 and 722 in that order. No process has started since, and it ends pid 721, the
// newer of the two largest that are new to the stay, before pid 700, older and three times as large
// (INFRA-3531 R1 of spec version 31).
func TestMemoryGuardReadsAgainAtTheMarkTheBandsGrowersFirstThenTheProcessesNewToTheStayNewestFirst(t *testing.T) {
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
	requireMarkKill(h, 721, 300*mib)
	requireChosenBy(h, 721, "new", "reread")
	if got, want := readOrderBeforeTheKill(reads()[inTheBand:]), []int{710, 711, 721, 720, 722}; !slices.Equal(got, want) {
		t.Fatalf("before the kill the guard read the smaps of %v in that order, want %v", got, want)
	}
}

// At the reading at the mark, a held process new to the stay that the reads again reach, one the
// band read at its last reading alone, ends before an older, larger held process where nothing
// grew over the window, though no process has started since the band's last reading (INFRA-3531
// R1 of spec version 31); one the band read at its last two readings is not new, and the largest
// ends. Pid 700 sleeps at 900 MiB through every band reading. Pid 710, of 200 MiB, starts before
// the band's last reading, or before its last two, and has not grown at the mark: read once, it is
// read again there and ended; read twice, it is not read again, and pid 700 ends on the band's
// reads alone.
func TestMemoryGuardEndsAHeldProcessNewToTheStayBeforeAnOlderLargerOneWhereNoneStartedSince(t *testing.T) {
	for _, tc := range []struct {
		name        string
		bandReads   int
		want        int
		size        uint64
		rule, sizes string
		reread      []int
	}{
		{"read at the band's last reading alone", 1, 710, 200 * mib, "new", "reread", []int{710}},
		{"read at the band's last two readings", 2, 700, 900 * mib, "largest", "band", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var identities func() []int
			var reads func() []smapsReadAt
			pod, h := heldSleeper(t, protect("tester", 100), func(h *harness) {
				reads = recordReads(h)
				identities = slowIdentities(h, 0)
			})
			pod.add(fakeProcess{pid: 710, ppid: 100, args: []string{"python3", "late.py"}, anon: 200 * mib})
			for range tc.bandReads {
				pod.setAnon(3400 * mib)
				h.tick()
			}
			inTheBand := len(reads())
			pod.setAnon(3500 * mib)
			h.tick()
			requireMarkKill(h, tc.want, tc.size)
			requireChosenBy(h, tc.want, tc.rule, tc.sizes)
			if got := readBeforeTheKill(reads()[inTheBand:]); !slices.Equal(got, tc.reread) {
				t.Fatalf("before the kill the guard read the smaps of %v, want %v", got, tc.reread)
			}
			if got := identities(); len(got) != 0 {
				t.Fatalf("at the mark the guard read the identities of %v, want none: no process started since", got)
			}
		})
	}
}

// readOrderBeforeTheKill is the pids of reads whose smaps the guard's loop read before its first
// kill, in the order it read them.
func readOrderBeforeTheKill(reads []smapsReadAt) []int {
	var order []int
	for _, r := range reads {
		if r.killsBefore == 0 && !r.scan {
			order = append(order, r.pid)
		}
	}
	return order
}

// Whatever the budget, the guard reads at least one process new to the stay at the mark where one
// exists (INFRA-3531 R1 of spec version 31: "and at least one of them"): where the reads again of
// the band's growers spent the half interval and the listing finds no process started since the
// band's last reading, it reads the newest held process new to the stay before it decides, and
// only it. Pids 710 and 711 grow between the band's first two readings and sleep from then on;
// pid 720, of 300 MiB, and pid 721, newer, of 100 MiB, start at the band's last reading. Nothing
// grows at the mark, and each smaps read takes 60 ms, more than the budget: the guard reads pid
// 710, the band's most grown, passes over pid 711, lists the processes, and then reads pid 721.
// Pid 721 is the one process new to the stay that it read, and it ends, before pid 720, which no
// read reached, and before the larger pids 710 and 711.
func TestMemoryGuardReadsTheNewestHeldProcessNewToTheStayWhereTheBandsGrowersSpentTheBudget(t *testing.T) {
	pod := newFakePod(t)
	most := fakeProcess{pid: 710, ppid: 100, args: []string{"python3", "grow.py"}, anon: 300 * mib}
	less := fakeProcess{pid: 711, ppid: 100, args: []string{"python3", "grow.py"}, anon: 300 * mib}
	pod.add(agent(2*gib), most, less)
	pod.setAnon(3100 * mib)
	h := newHarness(t, pod, protect("tester", 100))
	reads := recordReads(h)
	identities := slowIdentities(h, 0)
	_, perRead := slowMark(h)
	h.run()
	pod.resize(&most, 350*mib)
	pod.resize(&less, 320*mib)
	pod.setAnon(3200 * mib)
	h.tick()
	pod.setAnon(3300 * mib)
	h.tick()
	pod.add(
		fakeProcess{pid: 720, ppid: 100, start: 7000, args: []string{"python3", "late.py"}, anon: 300 * mib},
		fakeProcess{pid: 721, ppid: 100, start: 7100, args: []string{"python3", "late.py"}, anon: 100 * mib},
	)
	pod.setAnon(3400 * mib)
	h.tick()
	inTheBand := len(reads())
	pod.setAnon(3500 * mib)
	perRead.Store(int64(60 * time.Millisecond))
	h.tick()
	requireMarkKill(h, 721, 100*mib)
	requireChosenBy(h, 721, "new", "reread")
	if got, want := readOrderBeforeTheKill(reads()[inTheBand:]), []int{710, 721}; !slices.Equal(got, want) {
		t.Fatalf("before the kill the guard read the smaps of %v in that order, want %v", got, want)
	}
	if got := identities(); len(got) != 0 {
		t.Fatalf("at the mark the guard read the identities of %v, want none: no process started since", got)
	}
}

// A held process new to the stay that no read at the mark reached is not among the new ones the
// order takes there (INFRA-3531 R1 of spec version 31): the largest of the ones the guard read
// ends. Pid 700 sleeps at 900 MiB through every band reading; pid 720, of 300 MiB, and pid 721,
// newer, of 100 MiB, start at the band's last reading. Each smaps read at the mark takes 60 ms,
// more than the budget: the guard reads pid 721 alone. It has read a process new to the stay that
// it may end, so it reads no other, and pid 721 ends, though pid 720 is larger.
func TestMemoryGuardLeavesAHeldProcessNewToTheStayThatNoReadReachedOutOfTheNewOnes(t *testing.T) {
	var reads func() []smapsReadAt
	var perRead *atomic.Int64
	pod, h := heldSleeper(t, protect("tester", 100), func(h *harness) {
		reads = recordReads(h)
		_, perRead = slowMark(h)
	})
	pod.add(
		fakeProcess{pid: 720, ppid: 100, start: 7000, args: []string{"python3", "late.py"}, anon: 300 * mib},
		fakeProcess{pid: 721, ppid: 100, start: 7100, args: []string{"python3", "late.py"}, anon: 100 * mib},
	)
	pod.setAnon(3400 * mib)
	h.tick()
	inTheBand := len(reads())
	pod.setAnon(3500 * mib)
	perRead.Store(int64(60 * time.Millisecond))
	h.tick()
	requireMarkKill(h, 721, 100*mib)
	requireChosenBy(h, 721, "new", "reread")
	if got, want := readOrderBeforeTheKill(reads()[inTheBand:]), []int{721}; !slices.Equal(got, want) {
		t.Fatalf("before the kill the guard read the smaps of %v, want %v", got, want)
	}
}

// spentBudget is a pod at the band's third reading whose one band grower spends, at the mark, the
// budget of the reads there: the agent holds 2 GiB, pid 700 sleeps at 900 MiB, and pid 705 grows
// 50 MiB between the band's first two readings and sleeps from then on, so the reads again at the
// mark take it first.
type spentBudget struct {
	pod        *fakePod
	h          *harness
	reads      func() []smapsReadAt
	identities func() []int
	perRead    *atomic.Int64
}

func newSpentBudget(t *testing.T, classify Classifier) *spentBudget {
	t.Helper()
	pod := newFakePod(t)
	grower := fakeProcess{pid: 705, ppid: 100, args: []string{"python3", "grow.py"}, anon: 300 * mib}
	pod.add(agent(2*gib), grower, fakeProcess{pid: 700, ppid: 100, args: []string{"python3", "pool.py"}, anon: 900 * mib})
	pod.setAnon(3100 * mib)
	r := &spentBudget{pod: pod, h: newHarness(t, pod, classify)}
	r.reads = recordReads(r.h)
	r.identities = slowIdentities(r.h, 0)
	_, r.perRead = slowMark(r.h)
	r.h.run()
	pod.resize(&grower, 350*mib)
	pod.setAnon(3200 * mib)
	r.h.tick()
	pod.setAnon(3300 * mib)
	r.h.tick()
	return r
}

// atTheMark takes the band's last reading, and then the reading at the mark once started, the
// processes that start between the two, are there, each smaps read at the mark taking 60 ms, more
// than the budget. It answers the pids whose smaps the guard read at the mark before its kill, in
// order.
func (r *spentBudget) atTheMark(started ...fakeProcess) []int {
	r.h.t.Helper()
	r.pod.setAnon(3400 * mib)
	r.h.tick()
	inTheBand := len(r.reads())
	r.pod.add(started...)
	r.pod.setAnon(3500 * mib)
	r.perRead.Store(int64(60 * time.Millisecond))
	r.h.tick()
	return readOrderBeforeTheKill(r.reads()[inTheBand:])
}

// The reads of the processes new to the stay go first to those with one band read, and then to
// those with no earlier read, the ones started since the band's last reading (INFRA-3531 R1 of
// spec version 32). The read of pid 705, the band's grower, spends the budget, which leaves the one
// read the guard makes whatever the budget: it goes to pid 720, of 100 MiB, which the band read
// once, before pid 830, newer and larger, which started since. The guard has then read a process
// new to the stay that it may end, and reads nothing more: it lists no process, and pid 720 ends.
func TestMemoryGuardReadsPastTheBudgetAProcessWithOneBandReadBeforeANewerOneStartedSince(t *testing.T) {
	r := newSpentBudget(t, protect("tester", 100))
	r.pod.add(fakeProcess{pid: 720, ppid: 100, start: 7000, args: []string{"python3", "late.py"}, anon: 100 * mib})
	order := r.atTheMark(fakeProcess{pid: 830, ppid: 100, start: 8000, args: []string{"python3", "new.py"}, anon: 300 * mib})
	requireMarkKill(r.h, 720, 100*mib)
	requireChosenBy(r.h, 720, "new", "reread")
	if want := []int{705, 720}; !slices.Equal(order, want) {
		t.Fatalf("before the kill the guard read the smaps of %v in that order, want %v", order, want)
	}
	if got := r.identities(); len(got) != 0 {
		t.Fatalf("at the mark the guard read the identities of %v, want none", got)
	}
}

// The read the guard makes past the budget goes to the newest process with one band read that it
// may end (INFRA-3531 R1 of spec version 32). Pid 730, an Oh My Pi helper of 1200 MiB that the
// classifier protects, and pid 720, older, of 100 MiB, start at the band's last reading. The read
// of pid 705 spends the budget, and the guard then reads pid 720, and at no reading pid 730, and
// ends pid 720.
func TestMemoryGuardReadsPastTheBudgetTheNewestProcessWithOneBandReadThatItMayEnd(t *testing.T) {
	r := newSpentBudget(t, protectHelpers)
	r.pod.add(
		fakeProcess{pid: 720, ppid: 100, start: 7000, args: []string{"python3", "late.py"}, anon: 100 * mib},
		fakeProcess{pid: 730, ppid: 100, start: 7100, name: "omp", exe: "/opt/omp/bin/omp", args: []string{"omp", "__omp_worker_daemon_broker"}, anon: 1200 * mib},
	)
	order := r.atTheMark()
	requireMarkKill(r.h, 720, 100*mib)
	requireChosenBy(r.h, 720, "new", "reread")
	if want := []int{705, 720}; !slices.Equal(order, want) {
		t.Fatalf("before the kill the guard read the smaps of %v in that order, want %v", order, want)
	}
	for _, read := range r.reads() {
		if read.pid == 730 && !read.scan {
			t.Fatalf("the guard read the smaps of the protected pid 730")
		}
	}
}

// Where no held process is new to the stay and the budget is spent, the search reads the processes
// started since the band's last reading past the budget, the newest first, until it has read one
// the guard may end, and then stops (INFRA-3531 R1 of spec version 32). The read of pid 705 spends
// the budget. Pid 820, of 300 MiB, pid 821, of 100 MiB, and pid 830, the newest, an Oh My Pi
// helper that the classifier protects, start as the reading reaches the mark. The guard reads the
// identity of pid 830, which it may not end, then that of pid 821 and its smaps, and stops: pid
// 821 ends, and pid 820, larger, is not read.
func TestMemoryGuardSearchReadsPastTheBudgetUntilOneProcessItMayEnd(t *testing.T) {
	r := newSpentBudget(t, protectHelpers)
	order := r.atTheMark(
		fakeProcess{pid: 820, ppid: 100, start: 8000, args: []string{"python3", "new.py"}, anon: 300 * mib},
		fakeProcess{pid: 821, ppid: 100, start: 8100, args: []string{"python3", "new.py"}, anon: 100 * mib},
		fakeProcess{pid: 830, ppid: 100, start: 8200, name: "omp", exe: "/opt/omp/bin/omp", args: []string{"omp", "__omp_worker_daemon_broker"}, anon: 1200 * mib},
	)
	requireMarkKill(r.h, 821, 100*mib)
	requireChosenBy(r.h, 821, "new", "search")
	if want := []int{705, 821}; !slices.Equal(order, want) {
		t.Fatalf("before the kill the guard read the smaps of %v in that order, want %v", order, want)
	}
	if got, want := r.identities(), []int{830, 821}; !slices.Equal(got, want) {
		t.Fatalf("at the mark the guard read the identities of %v, want %v", got, want)
	}
}

// A process's reads of its stay in the band keep their storage from one reading to the next:
// keeping the band stays of processes read before allocates nothing, and keeps one stay for each.
func TestMemoryGuardKeepsBandStaysAllocatingNothingForAProcessReadBefore(t *testing.T) {
	h := newHarness(t, newFakePod(t), nil)
	cs := make([]candidate, 50)
	table := &Table{byPID: make(map[int]*Process, len(cs))}
	for i := range cs {
		cs[i] = candidate{Process: &Process{PID: 1000 + i, StartTime: uint64(i)}, size: uint64(i) * mib}
		table.byPID[cs[i].PID] = cs[i].Process
	}
	h.g.keepStays(cs, table)
	allocs := testing.AllocsPerRun(100, func() {
		h.g.keepStays(cs, table)
	})
	if allocs != 0 {
		t.Fatalf("a reading's band stays allocate %v times, want none", allocs)
	}
	if len(h.g.stays) != len(cs) {
		t.Fatalf("the guard keeps the stays of %d processes, want %d", len(h.g.stays), len(cs))
	}
	for key, stay := range h.g.stays {
		if stay.least != uint64(key.start)*mib || stay.grew {
			t.Fatalf("the stay of pid %d is %+v, want its one size and no growth", key.pid, stay)
		}
	}
}

// A process's reads keep their storage from one reading to the next: keeping the reads of
// processes read before, with their count of the stay's readings, allocates nothing, and keeps one
// count for each, of every reading in a row.
func TestMemoryGuardCountsTheReadingsOfAStayAllocatingNothingForAProcessReadBefore(t *testing.T) {
	h := newHarness(t, newFakePod(t), nil)
	cs := make([]candidate, 50)
	table := &Table{byPID: make(map[int]*Process, len(cs))}
	for i := range cs {
		cs[i] = candidate{Process: &Process{PID: 1000 + i, StartTime: uint64(i)}, size: uint64(i) * mib}
		table.byPID[cs[i].PID] = cs[i].Process
	}
	now := testStart
	for range int(growthWindow/Defaults().Interval) + 1 {
		now = now.Add(Defaults().Interval)
		h.g.keepSizes(now, cs, table)
	}
	const runs = 100
	allocs := testing.AllocsPerRun(runs, func() {
		now = now.Add(Defaults().Interval)
		h.g.keepSizes(now, cs, table)
	})
	if allocs != 0 {
		t.Fatalf("a reading's reads and counts allocate %v times, want none", allocs)
	}
	if len(h.g.sizes) != len(cs) {
		t.Fatalf("the guard keeps the reads of %d processes, want %d", len(h.g.sizes), len(cs))
	}
	// AllocsPerRun runs the function once more before it counts.
	want := int(growthWindow/Defaults().Interval) + 1 + runs + 1
	for key, r := range h.g.sizes {
		if r.readings != want || cs[key.start].newToStay {
			t.Fatalf("pid %d has %d readings of the stay and is new %t, want %d and not new", key.pid, r.readings, cs[key.start].newToStay, want)
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

// urgentBand is a pod whose agent holds 2 GiB beside pids 300, 301 and 302, which grow 20, 40 and
// 60 MiB at each reading (grow), and pid 303, the largest, which sleeps at 700 MiB; the guard has
// read the band's three readings, 3200 to 3400 MiB, and ended nothing. The fake pod's urgent share
// is 3788.8 MiB, 92.5% of its 4 GiB. grow leaves out each process the guard has killed: a killed
// process grows no more, whether or not it has exited.
func urgentBand(t *testing.T, onKill func(pod *fakePod) func(int) bool) (*fakePod, *harness, func(), *fakeProcess) {
	t.Helper()
	pod := newFakePod(t)
	growers := []*fakeProcess{
		{pid: 300, ppid: 100, args: []string{"python3", "grow.py"}, anon: 200 * mib},
		{pid: 301, ppid: 100, args: []string{"python3", "grow.py"}, anon: 300 * mib},
		{pid: 302, ppid: 100, args: []string{"python3", "grow.py"}, anon: 400 * mib},
	}
	idle := &fakeProcess{pid: 303, ppid: 100, args: []string{"python3", "idle.py"}, anon: 700 * mib}
	pod.add(agent(2*gib), *growers[0], *growers[1], *growers[2], *idle)
	pod.setAnon(3100 * mib)
	h := newHarness(t, pod, protect("tester", 100))
	if onKill != nil {
		h.signal.onKill = onKill(pod)
	}
	h.run()
	grow := func() {
		t.Helper()
		killed := h.signal.kills()
		for i, g := range growers {
			if !slices.Contains(killed, g.pid) {
				pod.resize(g, g.anon+uint64(i+1)*20*mib)
			}
		}
	}
	for _, reading := range []uint64{3200, 3300, 3400} {
		grow()
		pod.setAnon(reading * mib)
		h.tick()
	}
	h.requireKilled()
	return pod, h, grow, idle
}

// At a reading at or above the urgent share, the mark plus half the distance from the mark to the
// limit, the guard ends every process it may end that grew over the window, each with an ended line
// of its own, and not only the first in its order: near the limit one kill a reading cannot absorb
// a burst of several growers (INFRA-3531 R1 of spec version 39). At the first reading at the mark,
// 3800 MiB, it ends pid 302, the most grown, by its tier, and then pids 301 and 300, the most grown
// first, by the urgent rule, and not pid 303, the largest, which did not grow. It reads the band's
// growers again there (rereadHeld) and each line carries the sizes and the new_read of that one
// decision. At 3780 MiB, below the urgent share, it ends pid 302 alone, on the band's held reads.
func TestMemoryGuardEndsEveryProcessThatGrewOverTheWindowAtAReadingAtOrAboveTheUrgentShare(t *testing.T) {
	for _, tc := range []struct {
		name    string
		reading uint64
		want    []endedChoice
		newRead string
	}{
		{"at 3800 MiB, at or above the urgent share", 3800,
			[]endedChoice{{302, "grew", "reread", 0}, {301, "urgent", "reread", 0}, {300, "urgent", "reread", 0}}, "0:0"},
		{"at 3780 MiB, below the urgent share", 3780, []endedChoice{{302, "grew", "band", 0}}, "-"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod, h, grow, _ := urgentBand(t, nil)
			grow()
			pod.setAnon(tc.reading * mib)
			h.tick()
			if got := endedChoices(h); !slices.Equal(got, tc.want) {
				t.Fatalf("at the reading at the mark the guard ended %+v, want %+v; the log:\n%s", got, tc.want, strings.Join(h.log.all(), "\n"))
			}
			for _, c := range tc.want {
				requireNewRead(h, c.pid, tc.newRead)
			}
			for _, k := range h.killed() {
				if k.ReadingBytes != tc.reading*mib {
					t.Fatalf("the kill of pid %d reports the reading %d, want %d", k.PID, k.ReadingBytes, tc.reading*mib)
				}
			}
		})
	}
}

// At a reading at or above the urgent share at the mark, a held process that grew over the window
// and that the reads again did not reach within half an interval is ended at that reading too: it
// keeps its growth by the held reads (INFRA-3531 R1 of spec version 39, "every process it may end
// that grew over the window"). Here each smaps read at the mark takes 60 ms, as under a pod's CPU
// throttle, so the reads again read pid 302, the most grown, alone, and pids 301 and 300 keep the
// growth the band read. A guard that took from them the growth of a read it did not make ended pid
// 302 alone at 3800 MiB and left the two others to the readings after it.
func TestMemoryGuardEndsAtAnUrgentReadingTheGrowersItDidNotReadAgainWithinHalfAnInterval(t *testing.T) {
	pod := newFakePod(t)
	growers := []*fakeProcess{
		{pid: 300, ppid: 100, args: []string{"python3", "grow.py"}, anon: 200 * mib},
		{pid: 301, ppid: 100, args: []string{"python3", "grow.py"}, anon: 300 * mib},
		{pid: 302, ppid: 100, args: []string{"python3", "grow.py"}, anon: 400 * mib},
	}
	idle := fakeProcess{pid: 303, ppid: 100, args: []string{"python3", "idle.py"}, anon: 700 * mib}
	pod.add(agent(2*gib), *growers[0], *growers[1], *growers[2], idle)
	pod.setAnon(3100 * mib)
	h := newHarness(t, pod, protect("tester", 100))
	_, perRead := slowMark(h)
	h.run()
	grow := func() {
		for i, g := range growers {
			pod.resize(g, g.anon+uint64(i+1)*20*mib)
		}
	}
	for _, reading := range []uint64{3200, 3300, 3400} {
		grow()
		pod.setAnon(reading * mib)
		h.tick()
	}
	h.requireKilled()
	grow()
	perRead.Store(int64(60 * time.Millisecond))
	pod.setAnon(3800 * mib)
	h.tick()
	got := endedChoices(h)
	var ended []int
	for _, c := range got {
		ended = append(ended, c.pid)
	}
	if len(got) == 0 || got[0].pid != 302 || got[0].rule != "grew" || got[0].sizes != "reread" {
		t.Fatalf("at the urgent reading at the mark the guard ended %+v first, want pid 302 by rule=grew sizes=reread; the log:\n%s", got, strings.Join(h.log.all(), "\n"))
	}
	slices.Sort(ended)
	if want := []int{300, 301, 302}; !slices.Equal(ended, want) {
		t.Fatalf("at the urgent reading at the mark the guard ended %v, want %v, the two growers it did not read again among them; the log:\n%s", ended, want, strings.Join(h.log.all(), "\n"))
	}
	for _, c := range got[1:] {
		if c.rule != "urgent" {
			t.Fatalf("the guard ended pid %d by rule=%s at the urgent reading, want rule=urgent; the log:\n%s", c.pid, c.rule, strings.Join(h.log.all(), "\n"))
		}
	}
}

// After the kills of a reading at or above the urgent share the guard waits until every process it
// killed there has exited, and no longer than the wait setting; a fall and growth since the kill
// are measured from that one reading (INFRA-3531 R1 of spec version 39). At 3800 MiB it ends pids
// 302, 301 and 300. Where pid 300 lingers and pid 303 grows 100 MiB at each reading after, the
// reading not falling, the guard ends nothing while pid 300 has not exited; once it has, the next
// reading ends pid 303, and where it never does, the reading at which the wait has passed ends pid
// 303, pid 300 left out. Where all three exit and the next reading falls below 3800 MiB but stays
// at the mark, the guard ends pid 303 though nothing grew.
func TestMemoryGuardWaitsAfterTheKillsOfAnUrgentReadingUntilEveryKilledProcessHasExited(t *testing.T) {
	lingers := func(pod *fakePod) func(int) bool {
		exit := exitFreeing(pod, 0)
		return func(pid int) bool { return pid != 300 && exit(pid) }
	}
	for _, tc := range []struct {
		name   string
		onKill func(*fakePod) func(int) bool
		// then runs the readings after the mark's, given its idle pid 303 to grow.
		then func(t *testing.T, pod *fakePod, h *harness, idle *fakeProcess)
	}{
		{"pid 300 exits after two readings", lingers, func(t *testing.T, pod *fakePod, h *harness, idle *fakeProcess) {
			for _, reading := range []uint64{3850, 3900} {
				pod.resize(idle, idle.anon+100*mib)
				pod.setAnon(reading * mib)
				h.tick()
				h.requireKilled(302, 301, 300)
			}
			h.signal.exit(300)
			pod.remove(300)
			pod.resize(idle, idle.anon+100*mib)
			pod.setAnon(3950 * mib)
			h.tick()
			h.requireKilled(302, 301, 300, 303)
		}},
		{"pid 300 never exits", lingers, func(t *testing.T, pod *fakePod, h *harness, idle *fakeProcess) {
			pod.resize(idle, idle.anon+100*mib)
			pod.setAnon(3850 * mib)
			h.tick()
			h.requireKilled(302, 301, 300)
			pod.resize(idle, idle.anon+100*mib)
			pod.setAnon(3900 * mib)
			h.tickAfter(Defaults().Wait)
			h.requireKilled(302, 301, 300, 303)
		}},
		{"all exit and the reading falls", func(pod *fakePod) func(int) bool { return exitFreeing(pod, 0) },
			func(t *testing.T, pod *fakePod, h *harness, _ *fakeProcess) {
				pod.setAnon(3790 * mib)
				h.tick()
				h.requireKilled(302, 301, 300, 303)
				requireChosenBy(h, 303, "largest", "reading")
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod, h, grow, idle := urgentBand(t, tc.onKill)
			grow()
			pod.setAnon(3800 * mib)
			h.tick()
			h.requireKilled(302, 301, 300)
			tc.then(t, pod, h, idle)
			if stopped := h.log.with("stopped"); len(stopped) != 0 {
				t.Fatalf("the guard gave the rise up: %q", stopped)
			}
		})
	}
}

// The urgent share holds at every reading the guard ends something at, the go-on after a kill
// included (INFRA-3531 R1 of spec version 39). At the first reading at the mark, 3600 MiB, below
// the urgent share, the guard ends pid 302 alone, which exits. The next reading, 3850 MiB, has not
// fallen and pids 301 and 300 have grown since the kill: the guard ends pid 301 by its tier and pid
// 300 by the urgent rule, each on that reading's own sizes, and not pid 303, which did not grow.
func TestMemoryGuardEndsEveryProcessThatGrewAtAnUrgentReadingAfterAKill(t *testing.T) {
	pod, h, grow, _ := urgentBand(t, func(pod *fakePod) func(int) bool { return exitFreeing(pod, 0) })
	grow()
	pod.setAnon(3600 * mib)
	h.tick()
	h.requireKilled(302)
	grow()
	pod.setAnon(3850 * mib)
	h.tick()
	want := []endedChoice{{302, "grew", "band", 0}, {301, "grew", "reading", 0}, {300, "urgent", "reading", 0}}
	if got := endedChoices(h); !slices.Equal(got, want) {
		t.Fatalf("the guard ended %+v, want %+v; the log:\n%s", got, want, strings.Join(h.log.all(), "\n"))
	}
	requireNewRead(h, 301, "-")
	requireNewRead(h, 300, "-")
}

// Each kill of an urgent reading is a kill of its own: its own pidfd and its own start-time check
// (signalKill), and one that fails is noted against its process (noteKillFailure) and leaves the
// others to go on (INFRA-3531 R1 of spec version 39). Where pid 301's kill fails, pids 302 and 300
// still end at 3800 MiB. Where pid 302's, the first, fails, the guard sends none on the band's held
// reads, assesses that reading and acts on it, with pid 302 counted unread: it ends pid 301 and
// pid 300 at that same reading.
func TestMemoryGuardGoesOnWithTheOtherKillsOfAnUrgentReadingWhereOneFails(t *testing.T) {
	for _, tc := range []struct {
		name   string
		failed int
		want   []endedChoice
	}{
		{"an urgent kill fails", 301, []endedChoice{{302, "grew", "reread", 0}, {300, "urgent", "reread", 0}}},
		{"the first kill fails", 302, []endedChoice{{301, "grew", "reading", 0}, {300, "urgent", "reading", 0}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod, h, grow, _ := urgentBand(t, func(pod *fakePod) func(int) bool { return exitFreeing(pod, 0) })
			h.signal.mu.Lock()
			h.signal.killErr = func(pid int) error {
				if pid == tc.failed {
					return os.NewSyscallError("pidfd_send_signal", syscall.EPERM)
				}
				return nil
			}
			h.signal.mu.Unlock()
			grow()
			pod.setAnon(3800 * mib)
			h.tick()
			if got := endedChoices(h); !slices.Equal(got, tc.want) {
				t.Fatalf("the guard ended %+v, want %+v; the log:\n%s", got, tc.want, strings.Join(h.log.all(), "\n"))
			}
			if got := h.signal.attempts(); !slices.Contains(got, tc.failed) {
				t.Fatalf("the guard tried the kills of %v, want pid %d's among them", got, tc.failed)
			}
			want := fmt.Sprintf("launcher: memory guard: kill failed pid=%d ", tc.failed)
			if failed := h.log.with("kill failed"); len(failed) != 1 || !strings.HasPrefix(failed[0], want) {
				t.Fatalf("the kill-failed lines are %q, want one starting %q", failed, want)
			}
		})
	}
}

// Every kill of an urgent reading is sent before the guard reads the counter, which costs about
// 18 ms under gVisor, and before any of their ended lines and reports, so a grower's kill waits
// behind nothing it needs. The counter here moves by 1 MiB at each kill, and the reports, which
// carry the counter read once for the kills of a reading, carry its value after the last of the
// three; the lines and reports come in the order of the kills, with their sequences.
func TestMemoryGuardSendsEveryKillOfAnUrgentReadingBeforeItReadsTheCounter(t *testing.T) {
	const counter = 3 * gib
	sent := 0
	pod, h, grow, _ := urgentBand(t, func(pod *fakePod) func(int) bool {
		exit := exitFreeing(pod, 0)
		return func(pid int) bool {
			sent++
			pod.setCounter(counter + uint64(sent)*mib)
			return exit(pid)
		}
	})
	grow()
	pod.setAnon(3800 * mib)
	h.tick()
	h.requireKilled(302, 301, 300)
	kills := h.killed()
	for i, k := range kills {
		if k.CounterBytes != counter+3*mib {
			t.Fatalf("the report of pid %d carries the counter %d, want %d, read once every kill of the reading was sent", k.PID, k.CounterBytes, counter+3*mib)
		}
		if k.Sequence != uint64(i+1) {
			t.Fatalf("the report of pid %d has the sequence %d, want %d, in the order of the kills", k.PID, k.Sequence, i+1)
		}
	}
	want := []endedChoice{{302, "grew", "reread", 0}, {301, "urgent", "reread", 0}, {300, "urgent", "reread", 0}}
	if got := endedChoices(h); !slices.Equal(got, want) {
		t.Fatalf("the guard's ended lines are %+v, want %+v in the order of the kills", got, want)
	}
}

// newTierAtTheMark runs to the reading at the mark, of reading MiB, a pod whose agent holds 2 GiB
// beside pid 700, python3 pool.py of 1 GiB, and pid 303, python3 idle.py of 300 MiB, each read at
// the band's three readings, 3200 to 3400 MiB, so neither is new to the stay; and, where grower is
// set, pid 300, python3 grow.py, which grows 40 MiB at each reading. After the band's last reading
// three processes start: pid 701, a fork of pid 700 that runs its program and reports its 1 GiB as
// its own, so it ranks at nothing; pid 710, python3 worker.py of 300 MiB; and pid 711, the newest,
// of 200 MiB. Each smaps read at the mark takes perRead by the guard's clock. It answers the time of
// the reading at the mark by the guard's clock too.
func newTierAtTheMark(t *testing.T, grower bool, perRead time.Duration, reading uint64) (*harness, time.Time) {
	t.Helper()
	pod := newFakePod(t)
	parent := fakeProcess{pid: 700, ppid: 100, start: 5000, exe: "/usr/bin/python3.13", args: []string{"python3", "pool.py"}, anon: gib}
	pod.add(agent(2*gib), parent, fakeProcess{pid: 303, ppid: 100, start: 5100, args: []string{"python3", "idle.py"}, anon: 300 * mib})
	g := fakeProcess{pid: 300, ppid: 100, start: 5200, args: []string{"python3", "grow.py"}, anon: 200 * mib}
	if grower {
		pod.add(g)
	}
	pod.setAnon(3100 * mib)
	h := newHarness(t, pod, protect("tester", 100))
	h.signal.onKill = exitFreeing(pod, 0)
	_, read := slowMark(h)
	h.run()
	grow := func() {
		if grower {
			pod.resize(&g, g.anon+40*mib)
		}
	}
	for _, r := range []uint64{3200, 3300, 3400} {
		grow()
		pod.setAnon(r * mib)
		h.tick()
	}
	h.requireKilled()
	pod.add(
		fakeProcess{pid: 701, ppid: 700, start: 7000, exe: "/usr/bin/python3.13", args: []string{"python3", "pool.py"}, anon: gib},
		fakeProcess{pid: 710, ppid: 100, start: 7100, args: []string{"python3", "worker.py"}, anon: 300 * mib},
		fakeProcess{pid: 711, ppid: 100, start: 7200, args: []string{"python3", "worker.py"}, anon: 200 * mib},
	)
	grow()
	pod.setAnon(reading * mib)
	read.Store(int64(perRead))
	markAt := h.clock.Now().Add(h.pending.d)
	h.tick()
	read.Store(0)
	return h, markAt
}

// At a reading at or above the urgent share, where the decision holds a process that is not new to
// the stay, the guard ends every process new to it that holds memory of its own, beside the first,
// and not a fork whose parent's size covers its own, which ranks at nothing (INFRA-3531 R1 of spec
// versions 41 and 42). At the reading at the mark, 3800 MiB, the search finds pids 701, 710 and
// 711 started since the band's last reading: it ends pid 710, the largest of the new tier, by that
// tier, and pid 711 by the urgent rule, each line with the decision's new_read, and not pid 701,
// nor pid 700 or pid 303, which are not new.
func TestMemoryGuardEndsAtAnUrgentReadingEveryNewProcessWithMemoryOfItsOwnAndNotAForkThatRanksAtNothing(t *testing.T) {
	h, _ := newTierAtTheMark(t, false, 0, 3800)
	want := []endedChoice{{710, "new", "search", 0}, {711, "urgent", "search", 0}}
	if got := endedChoices(h); !slices.Equal(got, want) {
		t.Fatalf("at the urgent reading at the mark the guard ended %+v, want %+v; the log:\n%s", got, want, strings.Join(h.log.all(), "\n"))
	}
	for _, c := range want {
		requireNewRead(h, c.pid, "3:2")
	}
}

// Below the urgent share a process new to the stay is ended only by the order of ends, one process
// a reading (INFRA-3531 R1 of spec version 41): at the reading at the mark, 3700 MiB, of the pod
// newTierAtTheMark builds with no grower, the guard ends pid 710, the largest of the new tier,
// alone, and not pid 711, new with memory of its own too.
func TestMemoryGuardEndsOneProcessNewToTheStayBelowTheUrgentShare(t *testing.T) {
	h, _ := newTierAtTheMark(t, false, 0, 3700)
	want := []endedChoice{{710, "new", "search", 0}}
	if got := endedChoices(h); !slices.Equal(got, want) {
		t.Fatalf("below the urgent share the guard ended %+v, want %+v; the log:\n%s", got, want, strings.Join(h.log.all(), "\n"))
	}
}

// At an urgent reading at the mark the guard searches for the processes started since the band's
// last reading though a held process grew, since, pids 700 and 303 not new to the stay, it ends
// every one new to it with memory of its own there (INFRA-3531 R1 of spec versions 41 and 42),
// and the window's growers come first: it ends pid 300, grown in the band and since, by its tier,
// and pids 710 and 711, found by the search, by the urgent rule. It sends pid 300's kill before
// the search, since a process the search finds has no growth over the window and cannot go before
// it: the first kill waits for no search it does not need (INFRA-3531 R1 of spec version 43). The
// search has no budget of its own: where each smaps read at the mark takes 60 ms, the read again
// of pid 300 spends the half interval, and pid 300's kill goes 60 ms after the reading at the
// mark, one read; the search, having read no process new to the stay with memory of its own,
// reads on, the newest first, and stops after the read in hand, pid 711's: pid 710, unread at this
// reading, is not ended at it. At 15b73ba6, which searched before that kill, pid 300's kill went
// 120 ms after the reading at the mark.
func TestMemoryGuardSearchesAtAnUrgentReadingAtTheMarkThoughAHeldProcessGrew(t *testing.T) {
	for _, tc := range []struct {
		name    string
		perRead time.Duration
		want    []endedChoice
		newRead string
		// firstKill is the time from the reading at the mark to the first kill.
		firstKill time.Duration
	}{
		{"reads that take no time", 0, []endedChoice{{300, "grew", "search", 0}, {710, "urgent", "search", 0}, {711, "urgent", "search", 0}}, "3:2", 0},
		{"each smaps read at the mark taking 60 ms", 60 * time.Millisecond, []endedChoice{{300, "grew", "search", 0}, {711, "urgent", "search", 0}}, "1:1", 60 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, markAt := newTierAtTheMark(t, true, tc.perRead, 3800)
			if got := endedChoices(h); !slices.Equal(got, tc.want) {
				t.Fatalf("at the urgent reading at the mark the guard ended %+v, want %+v; the log:\n%s", got, tc.want, strings.Join(h.log.all(), "\n"))
			}
			for _, c := range tc.want {
				requireNewRead(h, c.pid, tc.newRead)
			}
			if got := h.killed()[0].At.Sub(markAt); got != tc.firstKill {
				t.Fatalf("pid 300's kill went %s after the reading at the mark, want %s, before the search", got, tc.firstKill)
			}
		})
	}
}

// A fork the search finds started since the band's last reading, running the program of a parent
// the search found too, ranks by its size less its parent's read by that search, since the guard
// has read that parent in this stay (INFRA-3531 R1 of spec version 43): a fork whose parent's size
// covers its own ranks at nothing and is not ended for urgency. The band reads pid 700, python3
// pool.py of 1 GiB, and pid 303, idle, at its three readings, so neither is new to the stay. After
// the band's last reading three processes start: pid 720, python3 job.py of 400 MiB; pid 721, its
// fork, which runs its program and reports its 400 MiB as its own; and pid 730, python3 worker.py
// of 200 MiB. The search reads them the newest first, the fork before its parent. At the reading at
// the mark, 3800 MiB, at or above the urgent share, nothing has grown: the guard ends pid 720 by the
// new tier and pid 730 by the urgent rule, and not pid 721 (new_read=3:2). At 15b73ba6 the fork
// ranked by its whole size, and the guard ended it first and pids 720 and 730 beside it.
func TestMemoryGuardRanksAForkTheSearchFoundBesideItsFoundParentAtNothingAndDoesNotEndItForUrgency(t *testing.T) {
	pod := newFakePod(t)
	pod.add(agent(2*gib),
		fakeProcess{pid: 700, ppid: 100, start: 5000, exe: "/usr/bin/python3.13", args: []string{"python3", "pool.py"}, anon: gib},
		fakeProcess{pid: 303, ppid: 100, start: 5100, args: []string{"python3", "idle.py"}, anon: 300 * mib})
	pod.setAnon(3100 * mib)
	h := newHarness(t, pod, protect("tester", 100))
	h.signal.onKill = exitFreeing(pod, 0)
	h.run()
	for _, r := range []uint64{3200, 3300, 3400} {
		pod.setAnon(r * mib)
		h.tick()
	}
	h.requireKilled()
	job := []string{"python3", "job.py"}
	pod.add(
		fakeProcess{pid: 720, ppid: 100, start: 7000, exe: "/usr/bin/python3.13", args: job, anon: 400 * mib},
		fakeProcess{pid: 721, ppid: 720, start: 7100, exe: "/usr/bin/python3.13", args: job, anon: 400 * mib},
		fakeProcess{pid: 730, ppid: 100, start: 7200, args: []string{"python3", "worker.py"}, anon: 200 * mib},
	)
	pod.setAnon(3800 * mib)
	h.tick()
	want := []endedChoice{{720, "new", "search", 0}, {730, "urgent", "search", 0}}
	if got := endedChoices(h); !slices.Equal(got, want) {
		t.Fatalf("at the urgent reading at the mark the guard ended %+v, want %+v; the log:\n%s", got, want, strings.Join(h.log.all(), "\n"))
	}
	for _, c := range want {
		requireNewRead(h, c.pid, "3:2")
	}
}

// Where every process the guard may end is new to the stay, being new tells no burst from the
// rest of the pod, and an urgent reading adds the window's growers alone (INFRA-3531 R1 of spec
// version 42). The band holds one reading, 3400 MiB, after one below it, so each process has one
// read; at the reading at the mark, 3800 MiB, the reads again show pid 300 grown 60 MiB and pid
// 301 40 MiB, and pid 303, of 700 MiB, flat. The guard ends pid 300 by its tier and pid 301 by
// the urgent rule, and not pid 303, new to the stay with memory of its own as they are.
func TestMemoryGuardEndsAtAnUrgentReadingWhereEveryProcessIsNewTheFirstAndTheWindowsGrowersAlone(t *testing.T) {
	pod := newFakePod(t)
	g300 := fakeProcess{pid: 300, ppid: 100, start: 3000, args: []string{"python3", "grow.py"}, anon: 200 * mib}
	g301 := fakeProcess{pid: 301, ppid: 100, start: 3010, args: []string{"python3", "grow.py"}, anon: 300 * mib}
	pod.add(agent(2*gib), g300, g301, fakeProcess{pid: 303, ppid: 100, start: 3030, args: []string{"python3", "idle.py"}, anon: 700 * mib})
	pod.setAnon(3000 * mib)
	h := newHarness(t, pod, protect("tester", 100))
	h.signal.onKill = exitFreeing(pod, 0)
	h.run()
	pod.setAnon(3400 * mib)
	h.tick()
	h.requireKilled()
	pod.resize(&g300, g300.anon+60*mib)
	pod.resize(&g301, g301.anon+40*mib)
	pod.setAnon(3800 * mib)
	h.tick()
	want := []endedChoice{{300, "grew", "reread", 0}, {301, "urgent", "reread", 0}}
	if got := endedChoices(h); !slices.Equal(got, want) {
		t.Fatalf("at the urgent reading at the mark the guard ended %+v, want %+v; the log:\n%s", got, want, strings.Join(h.log.all(), "\n"))
	}
}

// Where every process the guard may end is new to the stay, an urgent reading at the mark adds
// the window's growers alone beside its first kill, also where the reads at the mark reach only
// some of them in their half interval (INFRA-3531 R1 of spec version 42). The band holds one
// reading, 3400 MiB, after one below it, so pids 300 to 303, flat at 300 to 330 MiB, each have
// one read and are new to the stay. At the reading at the mark, 3800 MiB, each smaps read takes
// 30 ms, so the reads at the mark reach two of them before the half interval passes. The two they
// did not reach rank by their held sizes as not new for the order (settleNew), and they are no
// history of the stay: the guard ends one process at that reading, and none for urgency. A guard
// that read whether the stay has history after those reads ended pid 303 and, by the urgent rule,
// pid 302 too.
func TestMemoryGuardEndsOneProcessAtAnUrgentReadingWhereEveryProcessIsNewThoughTheReadsAtTheMarkReachOnlySome(t *testing.T) {
	pod := newFakePod(t)
	pod.add(agent(2*gib),
		fakeProcess{pid: 300, ppid: 100, start: 3000, args: []string{"python3", "a.py"}, anon: 300 * mib},
		fakeProcess{pid: 301, ppid: 100, start: 3010, args: []string{"python3", "b.py"}, anon: 310 * mib},
		fakeProcess{pid: 302, ppid: 100, start: 3020, args: []string{"python3", "c.py"}, anon: 320 * mib},
		fakeProcess{pid: 303, ppid: 100, start: 3030, args: []string{"python3", "d.py"}, anon: 330 * mib})
	pod.setAnon(3000 * mib)
	h := newHarness(t, pod, protect("tester", 100))
	h.signal.onKill = exitFreeing(pod, 0)
	_, perRead := slowMark(h)
	h.run()
	pod.setAnon(3400 * mib)
	h.tick()
	h.requireKilled()
	perRead.Store(int64(30 * time.Millisecond))
	pod.setAnon(3800 * mib)
	h.tick()
	if got := endedChoices(h); len(got) != 1 {
		t.Fatalf("at the urgent reading at the mark where every process is new the guard ended %+v, want one process; the log:\n%s", got, strings.Join(h.log.all(), "\n"))
	}
}

// Whether an urgent reading also ends every process new to the stay with memory of its own is
// taken on the processes the decision holds, once the reads at the mark have left out those they
// could not read (INFRA-3531 R1 of spec version 43: "where the decision also holds a process it may
// end that is not new to the stay"). The band holds two readings, 3200 and 3400 MiB: pid 300 is read
// at both and grows 20 MiB between them, and pids 310 and 311 start between them, so each has one
// read and is new to the stay. Pid 300 is gone by the reading at the mark, 3800 MiB, at or above
// the urgent share, and the reads at the mark leave it out: the decision holds no process that is
// not new, and the guard ends pid 310, the larger, alone. At 15b73ba6, which took this on the
// band's held processes, pid 300 among them, the guard ended pid 311 too, by the urgent rule.
func TestMemoryGuardEndsOneProcessAtAnUrgentReadingWhereTheOneProcessNotNewIsGoneAtTheMark(t *testing.T) {
	pod := newFakePod(t)
	grower := fakeProcess{pid: 300, ppid: 100, start: 3000, args: []string{"python3", "grow.py"}, anon: 300 * mib}
	pod.add(agent(2*gib), grower)
	pod.setAnon(3000 * mib)
	h := newHarness(t, pod, protect("tester", 100))
	h.signal.onKill = exitFreeing(pod, 0)
	h.run()
	pod.setAnon(3200 * mib)
	h.tick()
	pod.resize(&grower, grower.anon+20*mib)
	pod.add(
		fakeProcess{pid: 310, ppid: 100, start: 3100, args: []string{"python3", "a.py"}, anon: 300 * mib},
		fakeProcess{pid: 311, ppid: 100, start: 3110, args: []string{"python3", "b.py"}, anon: 250 * mib},
	)
	pod.setAnon(3400 * mib)
	h.tick()
	h.requireKilled()
	pod.remove(grower.pid)
	pod.setAnon(3800 * mib)
	h.tick()
	want := []endedChoice{{310, "new", "reread", 0}}
	if got := endedChoices(h); !slices.Equal(got, want) {
		t.Fatalf("at the urgent reading at the mark the guard ended %+v, want %+v; the log:\n%s", got, want, strings.Join(h.log.all(), "\n"))
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
// of the four readings after the first kill, and at the last, 92.87%, at or above the urgent share,
// the other grower left too, since it grew over the window (spec version 39), and no stopped line.
// While the killed grower has not exited, the guard waits for it, up to its wait, and ends nothing
// more.
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
		{"each killed grower exits", true, 1 + len(afterTheKill) + 1},
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
// the largest of the processes new to this stay in the band comes first, here the ones the reading
// at the mark finds started since the band's last reading (INFRA-3531 R1 of spec version 32): the
// guard ends a grower at the first reading at the mark.
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

// The tester's latepool run zrr4d of 2026-11-17 under gVisor at 4 GiB, at 778c42de, replayed with
// the readings its trace recorded and their times (infra-3531-live-4g-latepool-zrr4d.log, among
// the tester's logs of that head): the agent holds a quarter of the limit; the probe's parent
// fills its block to 83.75% over the band's first five readings, about 100 ms apart, and forks
// three sleepers, each reporting the block as its own, and the reading stays there through +1021
// ms. Six growers then start, 5 ms before the band's last reading, at +1173 ms and 84.91%, which
// reads each of them once, at a few MiB, and the next reading, at +1300 ms, is the first at the
// mark, 87.60%, where no read of a grower shows its first step yet. The sizes are modelled from
// the readings as in the protpool replay: the parent holds what the agent does not, and the
// growers share the rise over the flat run, each one MiB apart. At 778c42de the guard ended a
// sleeping fork there, pid 6756 of 2.4 GB: a process the reading before had read was not new,
// nothing had grown, and the largest came first. A process read at most once since the band's
// second-to-last reading is new to the stay (INFRA-3531 R1 of spec version 31): the guard reads
// the growers again, the newest first, and ends one, its line naming the new tier. Where each
// read at the mark takes 60 ms, as under the pod's CPU throttle, the read of the parent, which
// grew in the band, spends the budget, and the guard still reads the newest grower and ends it.
func TestMemoryGuardEndsAGrowerReadOnceAtTheBandsLastReadingBeforeASleepingForkOfASharedBlock(t *testing.T) {
	const agentBytes = podLimit / 4
	// A reading, and a size, in whole kB, as meminfo and smaps write them.
	kb := func(bytes uint64) uint64 { return bytes >> 10 << 10 }
	at := func(hundredths uint64) uint64 { return kb(podLimit / 10000 * hundredths) }
	// The band's readings after the first, at 76.64%, up to the flat run's last, in hundredths of a
	// percent of the limit, each with how long after the reading before it the guard took it.
	band := []struct {
		hundredths uint64
		after      time.Duration
	}{
		{7844, 103 * time.Millisecond}, {8018, 102 * time.Millisecond}, {8235, 102 * time.Millisecond},
		{8375, 104 * time.Millisecond}, {8375, 102 * time.Millisecond}, {8375, 101 * time.Millisecond},
		{8375, 102 * time.Millisecond}, {8375, 101 * time.Millisecond}, {8375, 102 * time.Millisecond},
		{8375, 102 * time.Millisecond},
	}
	for _, tc := range []struct {
		name    string
		perRead time.Duration
		reread  []int
	}{
		{"the reads at the mark take no time", 0, []int{700, 805, 804, 803, 802, 801, 800}},
		{"each read at the mark takes 60 ms", 60 * time.Millisecond, []int{700, 805}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := newFakePod(t)
			parent := fakeProcess{pid: 700, ppid: 100, start: 5000, args: []string{"python3", "latepool.py"}, anon: at(7664) - agentBytes}
			pod.add(agent(agentBytes), parent)
			pod.setAnon(at(7664))
			h := newHarness(t, pod, protect("tester", 100))
			h.signal.onKill = exitFreeing(pod, 0)
			reads := recordReads(h)
			_, perRead := slowMark(h)
			h.run()
			forked := false
			for _, r := range band {
				if !forked {
					pod.resize(&parent, at(r.hundredths)-agentBytes)
				}
				if !forked && r.hundredths == 8375 {
					forked = true
					for i := range 3 {
						pod.add(fakeProcess{pid: 701 + i, ppid: 700, start: uint64(5100 + 100*i), args: []string{"python3", "latepool.py"}, anon: parent.anon})
					}
				}
				pod.setAnon(at(r.hundredths))
				h.tickAfter(r.after - Defaults().Interval)
			}
			// Six growers start, each a python3 child of the parent, and the band's last reading
			// reads each once.
			share := kb((at(8491) - at(8375)) / 6)
			for i := range 6 {
				pod.add(fakeProcess{pid: 800 + i, ppid: 700, start: uint64(6000 + i), args: []string{"python3", "-c", "grow"}, anon: share + uint64(i)*mib})
			}
			pod.setAnon(at(8491))
			h.tickAfter(152*time.Millisecond - Defaults().Interval)
			if kills := h.signal.kills(); len(kills) != 0 {
				t.Fatalf("in the band the guard killed %v, want nothing; the log:\n%s", kills, strings.Join(h.log.all(), "\n"))
			}
			inTheBand := len(reads())

			pod.setAnon(at(8760))
			perRead.Store(int64(tc.perRead))
			h.tickAfter(127*time.Millisecond - Defaults().Interval)
			h.requireKilled(805)
			requireChosenBy(h, 805, "new", "reread")
			ended := fmt.Sprintf("ended pid=805 role=tester size=%d reading=%d ", share+5*mib, at(8760))
			if got := h.log.with(ended); len(got) != 1 {
				t.Fatalf("the ended lines are %q, want one starting %q", h.log.with("ended"), linePrefix+ended)
			}
			if got := readOrderBeforeTheKill(reads()[inTheBand:]); !slices.Equal(got, tc.reread) {
				t.Fatalf("before the kill the guard read the smaps of %v in that order, want %v", got, tc.reread)
			}
		})
	}
}

// replayReading is one reading of a recorded run, in hundredths of a percent of the limit, and how
// long after the reading before it the guard took it, by the run's own trace.
type replayReading struct {
	hundredths uint64
	after      time.Duration
}

// The probe's command lines in the tester's latepool runs: the parent's, which its forks keep, and
// each grower's, which runs a program of its own under the same executable.
var (
	latepoolArgs = []string{"/usr/bin/python3", "-c", "import os, signal, subprocess, sys, time; latepool", "pids", "held", "go", "2405181686", "0", "0", "0"}
	growArgs     = []string{"/usr/bin/python3", "-c", "import time; b=[]; [(b.append(bytearray(64<<20)), time.sleep(0.25)) for _ in iter(int, 1)]"}
)

// endedChoice is one of the guard's ended lines: the process it ended, the rule that put it first,
// where the sizes it was chosen by came from, and how much of its size its rank did not count.
type endedChoice struct {
	pid         int
	rule, sizes string
	lessParent  uint64
}

// endedChoices are the guard's ended lines in h's log, read as endedChoices, in order.
func endedChoices(h *harness) []endedChoice {
	h.t.Helper()
	var out []endedChoice
	for _, line := range h.log.with("ended ") {
		var c endedChoice
		fields := strings.Fields(strings.TrimPrefix(line, linePrefix+"ended "))
		for _, field := range fields {
			key, value, _ := strings.Cut(field, "=")
			switch key {
			case "pid":
				c.pid, _ = strconv.Atoi(value)
			case "rule":
				c.rule = value
			case "sizes":
				c.sizes = value
			case "less_parent":
				c.lessParent, _ = strconv.ParseUint(value, 10, 64)
			}
		}
		out = append(out, c)
	}
	return out
}

// The tester's latepool runs under gVisor at 4 GiB, replayed with the readings their traces
// recorded and their times: `4g-latepool-6gd5t` and `4g-latepool-v5s8l` at 23fdd92f,
// `4g-latepool-54nzl`, `4g-latepool-kxv5k` and `4g-latepool-zxhnw` at d22d8da3, and
// `4g-latepool-djcvt` and `4g-latepool-rjm8m` at 18c3c5bb, all of 2026-11-18
// (infra-3531-live-4g-latepool-<run>.log, among the tester's logs of those heads). The agent holds
// a quarter of the limit; the probe's parent, a python3 command, fills a block of 2.4 GB over the
// band's first readings and holds it, read at each. It then forks three sleepers, each running its
// executable with its arguments and reporting its block as its own: after the band's last reading
// in 6gd5t, so the reading at the mark finds them started since; between the band's last two
// readings in v5s8l, 54nzl and zxhnw, so the band reads each once; and before them in kxv5k, so
// the band reads each twice. Six growers then start after the band's last reading, each a python3
// child of the parent that runs a program of its own, and the reading at the mark finds them
// started since. At 23fdd92f the new tier ended a sleeping fork first in both runs, at its full 2.4
// GB, and after the kills of two growers, at a reading that fell but stayed above the mark with no
// grower's step inside the window, the largest, the second fork (6gd5t at +1621 ms, v5s8l at +2372
// ms). At d22d8da3, where each fork reported its parent's whole size and so ranked at nothing, the
// reads at the mark of 54nzl and zxhnw had reached one fork when the half interval had passed, and
// that fork ended first, 58.9 and 84.8 ms after the reading at the mark; and after a fall, with
// growers that step every 250 ms in step, so that no read 100 ms after a kill shows a step, kxv5k
// and zxhnw ended the parent and then a sleeping fork while the growers lived. In djcvt and rjm8m
// the growers start just before the band's last reading, which reads each between its fork and its
// exec (Python's subprocess.Popen), with its parent's executable and arguments: in djcvt at a few
// MiB, in rjm8m at its parent's 2.4 GB, as a fork reports its parent's pages under gVisor. At
// 18c3c5bb the fork discount took them for forks: djcvt's first kill, a grower that grew, had its
// whole size taken off (`less_parent=31051776`), and rjm8m's reading at the mark, stalled 115.1 ms,
// read one grower, ranked it at nothing, and ended the parent.
//
// A fork that runs its parent's program, its parent read in this stay, ranks by its size less its
// parent's (INFRA-3531 R1 of spec version 33): at 1 MiB in the runs of 23fdd92f, and at nothing in
// those of d22d8da3, where it is not in the tier of the processes new to the stay (spec version
// 34). In 54nzl and zxhnw the time from the reading at the mark to the kill at d22d8da3 passes at
// that reading, before the guard reads anything, as a stall of the pod's CPU throttle: the reads
// again take the parent, the first of them always, and, having read no process new to the stay
// with memory of its own, the guard reads on past the half interval, the forks and then the
// processes started since, up to one interval from the reading at the mark (spec version 34). At
// the mark it ends the largest grower, found by the search, new to the stay, its line taking no
// parent's size off. The growers are modelled on the grow program and the traces' ended sizes,
// each 1 MiB apart. The guard then ends a grower at each reading: where one grew over the window,
// as new to the stay, or, at a reading with no grower's step inside the window, by its growth since
// the stay's first kill, which the parent and its forks show none of (spec version 34); and at a
// reading at or above the urgent share, 92.5% of the limit, every grower that grew over the window
// and, since the parent and its forks are not new to the stay, every grower new to it, the first by
// its tier and each other by the urgent rule (spec versions 39, 41 and 42). With the six growers
// ended, at the readings left, which fall, it ends the largest: the parent, whose own program is
// not its parent's, the agent's, at its whole 2.4 GB, and then, no read parent discounting the
// forks any longer, the forks, the newest first, each at its whole size. No fork, and not the
// parent, ends while a grower lives. The reading at the mark reads again the executable and
// arguments of a held process whose smaps it reads where the band read it running its parent's
// program (reidentify), so in djcvt and rjm8m the grower it reads ranks by its whole size and ends
// first; a grower the band read before its exec and the mark did not read again has its first read
// after the kill as its size as of the kill (keepKillSizes), so in rjm8m the guard goes on after
// the first kill while the growers grow, and after a fall ends one by its growth since that kill.
// In `4g-t4latepool-r2`, at 8023d298 on a 1.5-CPU pod, the band's last reading read the six growers
// at about 1.5 MiB each, modelled as djcvt's read before their exec, and a reading 148 ms late let
// each one's first 64 MiB step land before the first reading at the mark, 93.88%, at or above the
// urgent share: one kill a reading left the pod 2.8 MiB from the kill point, and the guard now ends
// all six at that reading. In `4g-latepool-4tpqx`, at a82a9f2c, six growers started between the
// band's last reading and the mark, so the search found each with one read and no growth over the
// window at the first reading at the mark, 93.95%, and one kill there left the pod 26.5 MiB from
// the kill point: each is new to the stay with memory of its own, beside a parent and forks that
// are not, and the guard ends all six at that reading (INFRA-3531 R1 of spec versions 41 and 42).
// Each run's first kill says what the reads at the mark read of the processes new to the stay
// (new_read).
func TestMemoryGuardEndsAGrowerBeforeTheSleepingForksOfAParentThatRunItsProgram(t *testing.T) {
	for _, run := range []latepoolRun{
		{
			name: "6gd5t, the forks started after the band's last reading",
			band: []replayReading{{7661, 0}, {7891, 103 * time.Millisecond}, {8108, 103 * time.Millisecond},
				{8329, 102 * time.Millisecond}, {8385, 102 * time.Millisecond}, {8385, 101 * time.Millisecond},
				{8385, 101 * time.Millisecond}, {8385, 101 * time.Millisecond}, {8385, 101 * time.Millisecond},
				{8385, 101 * time.Millisecond}},
			forksAt: 10,
			mark:    replayReading{8533, 156 * time.Millisecond},
			after: []replayReading{{9024, 242 * time.Millisecond}, {9232, 202 * time.Millisecond}, {9073, 106 * time.Millisecond},
				{9370, 105 * time.Millisecond}, {9365, 105 * time.Millisecond}, {9201, 104 * time.Millisecond},
				{8882, 105 * time.Millisecond}},
			own:     mib,
			grown:   []uint64{4, 50, 72, 72, 136, 136, 200, 200},
			newRead: "9:9",
			want: []endedChoice{
				{805, "new", "search", 0}, {804, "grew", "reading", 0}, {803, "grew", "reading", 0},
				{802, "grew-since-kill", "reading", 0}, {801, "grew", "reading", 0}, {800, "urgent", "reading", 0},
				{700, "largest", "reading", 0}, {703, "largest", "reading", 0}, {702, "largest", "reading", 0},
			},
		},
		{
			// v5s8l's next reading comes 315 ms after the mark, so its window holds no read
			// before: the growers, read once, are new; and the one after 317 ms later, by when the
			// growers have two reads and their window none, goes by growth since the rise's first
			// kill.
			name: "v5s8l, the forks started between the band's last two readings",
			band: []replayReading{{7601, 0}, {7727, 105 * time.Millisecond}, {7855, 105 * time.Millisecond},
				{7979, 104 * time.Millisecond}, {8114, 103 * time.Millisecond}, {8260, 104 * time.Millisecond},
				{8381, 113 * time.Millisecond}, {8381, 103 * time.Millisecond}, {8381, 102 * time.Millisecond},
				{8381, 102 * time.Millisecond}, {8381, 106 * time.Millisecond}, {8381, 103 * time.Millisecond},
				{8381, 103 * time.Millisecond}, {8381, 103 * time.Millisecond}, {8424, 151 * time.Millisecond}},
			forksAt: 14,
			mark:    replayReading{8520, 123 * time.Millisecond},
			after: []replayReading{{9024, 315 * time.Millisecond}, {9227, 317 * time.Millisecond}, {9063, 110 * time.Millisecond},
				{9476, 107 * time.Millisecond}, {9360, 117 * time.Millisecond}, {9197, 108 * time.Millisecond},
				{8765, 110 * time.Millisecond}},
			own:     mib,
			grown:   []uint64{4, 50, 72, 72, 136, 136, 200, 200},
			newRead: "9:9",
			want: []endedChoice{
				{805, "new", "search", 0}, {804, "new", "reading", 0}, {803, "grew-since-kill", "reading", 0},
				{802, "grew-since-kill", "reading", 0}, {801, "grew", "reading", 0}, {800, "urgent", "reading", 0},
				{700, "largest", "reading", 0}, {703, "largest", "reading", 0}, {702, "largest", "reading", 0},
			},
		},
		{
			// 54nzl's reading rose 94 MiB from +2174 to +2280 ms, so the growers are partway
			// through their second step at +2174 ms and finish it by +2280 ms. Its forks ranked at
			// nothing at their band read, so the search reads a grower before them, and the mark
			// reads no fork (INFRA-3531 R1 of spec version 38): new_read=1:1.
			name: "54nzl, the forks ranking at nothing and the reading at the mark stalled 58.9 ms",
			band: []replayReading{{7590, 0}, {7751, 103 * time.Millisecond}, {7929, 104 * time.Millisecond},
				{8100, 106 * time.Millisecond}, {8260, 104 * time.Millisecond}, {8366, 104 * time.Millisecond},
				{8366, 111 * time.Millisecond}, {8366, 103 * time.Millisecond}, {8366, 102 * time.Millisecond},
				{8366, 101 * time.Millisecond}, {8366, 101 * time.Millisecond}, {8366, 102 * time.Millisecond},
				{8366, 102 * time.Millisecond}, {8366, 101 * time.Millisecond}, {8407, 154 * time.Millisecond}},
			forksAt: 14,
			mark:    replayReading{8590, 108 * time.Millisecond},
			after: []replayReading{{8904, 193 * time.Millisecond}, {9211, 267 * time.Millisecond}, {9048, 108 * time.Millisecond},
				{9282, 106 * time.Millisecond}, {9039, 111 * time.Millisecond}, {8719, 105 * time.Millisecond}},
			stall:   58900 * time.Microsecond,
			grown:   []uint64{4, 56, 70, 128, 133, 134, 134},
			newRead: "1:1",
			want: []endedChoice{
				{805, "new", "search", 0}, {804, "grew", "reading", 0}, {803, "grew", "reading", 0},
				{802, "grew", "reading", 0}, {801, "grew", "reading", 0}, {800, "urgent", "reading", 0},
				{700, "largest", "reading", 0}, {703, "largest", "reading", 0},
			},
		},
		{
			// At +1501 and +1605 ms, each 105 ms after a kill and with the reading fallen, no
			// grower shows a step inside the window; at +1923 ms, 214 ms after the reading before,
			// neither.
			name: "kxv5k, the forks read twice by the band and the growers stepping in step after a fall",
			band: []replayReading{{7693, 0}, {7932, 102 * time.Millisecond}, {8138, 104 * time.Millisecond},
				{8364, 103 * time.Millisecond}, {8371, 103 * time.Millisecond}, {8371, 101 * time.Millisecond},
				{8371, 101 * time.Millisecond}, {8371, 102 * time.Millisecond}, {8371, 101 * time.Millisecond},
				{8401, 102 * time.Millisecond}, {8412, 105 * time.Millisecond}},
			forksAt: 9,
			mark:    replayReading{8707, 141 * time.Millisecond},
			after: []replayReading{{9222, 231 * time.Millisecond}, {9070, 105 * time.Millisecond}, {9057, 104 * time.Millisecond},
				{9594, 104 * time.Millisecond}, {9349, 214 * time.Millisecond}, {9249, 106 * time.Millisecond},
				{8866, 106 * time.Millisecond}},
			grown:   []uint64{45, 70, 70, 70, 134, 134, 197, 197},
			newRead: "6:6",
			want: []endedChoice{
				{805, "new", "search", 0}, {804, "grew", "reading", 0}, {803, "grew-since-kill", "reading", 0},
				{802, "grew-since-kill", "reading", 0}, {801, "grew", "reading", 0}, {800, "urgent", "reading", 0},
				{700, "largest", "reading", 0}, {703, "largest", "reading", 0}, {702, "largest", "reading", 0},
			},
		},
		{
			// zxhnw stalled 84.8 ms at the mark, like 54nzl, and after the kills at +2018 and
			// +2124 ms, which fell, no grower shows a step inside the window, like kxv5k.
			name: "zxhnw, the reading at the mark stalled 84.8 ms and the growers stepping in step after a fall",
			band: []replayReading{{7606, 0}, {7799, 102 * time.Millisecond}, {7990, 102 * time.Millisecond},
				{8185, 103 * time.Millisecond}, {8342, 102 * time.Millisecond}, {8371, 106 * time.Millisecond},
				{8371, 101 * time.Millisecond}, {8371, 102 * time.Millisecond}, {8371, 102 * time.Millisecond},
				{8371, 101 * time.Millisecond}, {8371, 101 * time.Millisecond}, {8371, 103 * time.Millisecond},
				{8371, 102 * time.Millisecond}, {8414, 173 * time.Millisecond}},
			forksAt: 13,
			mark:    replayReading{8563, 103 * time.Millisecond},
			after: []replayReading{{8974, 206 * time.Millisecond}, {9209, 201 * time.Millisecond}, {9053, 108 * time.Millisecond},
				{9040, 106 * time.Millisecond}, {9548, 104 * time.Millisecond}, {9331, 182 * time.Millisecond},
				{9168, 103 * time.Millisecond}, {8848, 103 * time.Millisecond}},
			stall:   84800 * time.Microsecond,
			grown:   []uint64{4, 53, 68, 68, 68, 133, 133, 197, 197},
			newRead: "1:1",
			want: []endedChoice{
				{805, "new", "search", 0}, {804, "grew", "reading", 0}, {803, "grew", "reading", 0},
				{802, "grew-since-kill", "reading", 0}, {801, "grew-since-kill", "reading", 0}, {800, "grew", "reading", 0},
				{700, "largest", "reading", 0}, {703, "largest", "reading", 0}, {702, "largest", "reading", 0},
			},
		},
		{
			// The reading at the mark reads the newest grower again, which has grown 33 MiB since
			// the band's read of it before its exec.
			name: "djcvt, the growers read by the band's last reading before their exec at a few MiB",
			band: []replayReading{{7531, 0}, {7695, 102 * time.Millisecond}, {7891, 105 * time.Millisecond},
				{8080, 102 * time.Millisecond}, {8271, 104 * time.Millisecond}, {8371, 101 * time.Millisecond},
				{8371, 102 * time.Millisecond}, {8371, 102 * time.Millisecond}, {8371, 101 * time.Millisecond},
				{8371, 101 * time.Millisecond}, {8371, 102 * time.Millisecond}, {8371, 101 * time.Millisecond},
				{8414, 101 * time.Millisecond}, {8428, 111 * time.Millisecond}},
			forksAt: 12,
			mark:    replayReading{8648, 137 * time.Millisecond},
			after: []replayReading{{8909, 192 * time.Millisecond}, {9067, 194 * time.Millisecond}, {9060, 109 * time.Millisecond},
				{9053, 109 * time.Millisecond}, {8734, 109 * time.Millisecond}},
			stall:   72400 * time.Microsecond,
			preExec: 2 * mib,
			grown:   []uint64{30, 52, 68, 134, 134, 134},
			newRead: "1:1",
			want: []endedChoice{
				{805, "grew", "reread", 0}, {804, "grew", "reading", 0}, {803, "grew", "reading", 0},
				{802, "grew", "reading", 0}, {801, "grew", "reading", 0}, {800, "grew-since-kill", "reading", 0},
			},
		},
		{
			// rjm8m's next reading comes 306 ms after the mark, so the window holds no read before
			// it and the growers, read twice, are not new: the guard goes on by their growth since
			// the first kill, and ends one by it again at +2139 ms, after a fall.
			name: "rjm8m, the growers read by the band's last reading before their exec at the parent's size and the reading at the mark stalled 115.1 ms",
			band: []replayReading{{7565, 0}, {7736, 105 * time.Millisecond}, {7893, 103 * time.Millisecond},
				{8053, 104 * time.Millisecond}, {8232, 103 * time.Millisecond}, {8371, 102 * time.Millisecond},
				{8371, 102 * time.Millisecond}, {8371, 102 * time.Millisecond}, {8376, 106 * time.Millisecond},
				{8376, 102 * time.Millisecond}, {8376, 102 * time.Millisecond}, {8376, 102 * time.Millisecond},
				{8376, 102 * time.Millisecond}, {8419, 155 * time.Millisecond}},
			forksAt: 13,
			mark:    replayReading{8664, 137 * time.Millisecond},
			after: []replayReading{{9099, 306 * time.Millisecond}, {9222, 195 * time.Millisecond}, {9058, 111 * time.Millisecond},
				{9364, 104 * time.Millisecond}, {9044, 105 * time.Millisecond}, {8877, 106 * time.Millisecond}},
			stall:   115100 * time.Microsecond,
			preExec: latepoolBlock,
			grown:   []uint64{30, 60, 70, 70, 134, 134, 197},
			newRead: "1:1",
			want: []endedChoice{
				{805, "new", "reread", 0}, {804, "grew-since-kill", "reading", 0}, {803, "grew", "reading", 0},
				{802, "grew-since-kill", "reading", 0}, {801, "grew", "reading", 0}, {800, "urgent", "reading", 0},
				{700, "largest", "reading", 0}, {703, "largest", "reading", 0},
			},
		},
		{
			// rnr56's three forks start between the band's last two readings, which read each
			// once at its parent's size, so each ranked at nothing at that read; the growers start
			// after the band's last reading. Each smaps read at the mark takes 30 ms, as a 2.4 GB
			// read did under the pod's CPU throttle: at 43213c99 the reads again took the parent
			// and then the forks, the newest first, read on to one interval with no process with
			// memory of its own read, the search read nothing, and the parent ended 120.1 ms
			// after the reading at the mark (new_read=3:0). The search now reads a grower right
			// after the parent, and it ends within one interval; the forks are not read.
			name: "rnr56, the forks ranking at nothing at their band read and each read at the mark 30 ms",
			band: []replayReading{{7754, 0}, {8129, 102 * time.Millisecond}, {8368, 103 * time.Millisecond},
				{8368, 101 * time.Millisecond}, {8368, 102 * time.Millisecond}, {8368, 104 * time.Millisecond},
				{8411, 102 * time.Millisecond}},
			forksAt: 6,
			mark:    replayReading{8643, 121 * time.Millisecond},
			after: []replayReading{{9378, 266 * time.Millisecond}, {9219, 108 * time.Millisecond}, {9456, 104 * time.Millisecond},
				{9361, 106 * time.Millisecond}, {9198, 103 * time.Millisecond}, {8878, 104 * time.Millisecond}},
			block:          2412277760,
			perRead:        30 * time.Millisecond,
			withinInterval: true,
			grown:          []uint64{4, 70, 70, 134, 134, 198, 198},
			newRead:        "1:1",
			want: []endedChoice{
				{805, "new", "search", 0}, {804, "grew", "reading", 0}, {803, "urgent", "reading", 0},
				{802, "urgent", "reading", 0}, {801, "urgent", "reading", 0}, {800, "urgent", "reading", 0},
				{700, "largest", "reading", 0}, {703, "largest", "reading", 0}, {702, "largest", "reading", 0},
			},
		},
		{
			// t4latepool-r2's band reads the growers once, at its last reading, 142 ms after the
			// one before; the reading at the mark comes 148 ms after it, past the growers' first
			// step.
			name: "4g-t4latepool-r2, six growers' first steps in one late gap and the first reading at the mark at 93.88%",
			band: []replayReading{{7887, 0}, {8174, 102 * time.Millisecond}, {8368, 111 * time.Millisecond},
				{8368, 103 * time.Millisecond}, {8368, 103 * time.Millisecond}, {8368, 101 * time.Millisecond},
				{8408, 135 * time.Millisecond}, {8408, 102 * time.Millisecond}, {8408, 102 * time.Millisecond},
				{8408, 101 * time.Millisecond}, {8408, 103 * time.Millisecond}, {8408, 101 * time.Millisecond},
				{8408, 102 * time.Millisecond}, {8408, 101 * time.Millisecond}, {8408, 103 * time.Millisecond},
				{8408, 101 * time.Millisecond}, {8408, 102 * time.Millisecond}, {8408, 101 * time.Millisecond},
				{8408, 103 * time.Millisecond}, {8408, 101 * time.Millisecond}, {8408, 102 * time.Millisecond},
				{8408, 102 * time.Millisecond}, {8408, 101 * time.Millisecond}, {8408, 101 * time.Millisecond},
				{8408, 102 * time.Millisecond}, {8408, 102 * time.Millisecond}, {8429, 142 * time.Millisecond}},
			forksAt:        7,
			mark:           replayReading{9388, 148 * time.Millisecond},
			preExec:        3 * mib / 2,
			withinInterval: true,
			grown:          []uint64{64},
			newRead:        "6:6",
			atMark:         6,
			want: []endedChoice{
				{805, "grew", "reread", 0}, {804, "urgent", "reread", 0}, {803, "urgent", "reread", 0},
				{802, "urgent", "reread", 0}, {801, "urgent", "reread", 0}, {800, "urgent", "reread", 0},
			},
		},
		{
			// 4tpqx's three sleeping forks start before the band's last reading, which reads each
			// once at its parent's size; the six growers start after it, and the first reading at
			// the mark, 101 ms later, is at or above the urgent share. The search finds the six,
			// each with one read and so no growth over the window, and the reads at the mark take
			// the forks too (new_read=9:6, as the run's line read). At a82a9f2c the guard ended
			// one of the six there, `rule=new sizes=search`, and the pod came 26.5 MiB from the
			// kill point; each is new to a stay whose parent and forks are not, and all six end.
			name: "4g-latepool-4tpqx, six growers started between the band's last reading and a first reading at the mark at 93.95%",
			band: []replayReading{{7937, 0}, {8364, 102 * time.Millisecond}, {8373, 100 * time.Millisecond},
				{8373, 101 * time.Millisecond}, {8373, 101 * time.Millisecond}, {8415, 111 * time.Millisecond}},
			forksAt:        5,
			mark:           replayReading{9395, 101 * time.Millisecond},
			withinInterval: true,
			grown:          []uint64{65},
			newRead:        "9:6",
			atMark:         6,
			want: []endedChoice{
				{805, "new", "search", 0}, {804, "urgent", "search", 0}, {803, "urgent", "search", 0},
				{802, "urgent", "search", 0}, {801, "urgent", "search", 0}, {800, "urgent", "search", 0},
			},
		},
	} {
		t.Run(run.name, func(t *testing.T) { replayLatepool(t, run) })
	}
}

// latepoolBlock is the size each sleeping fork of the latepool probe reported at 4 GiB, its
// parent's block.
const latepoolBlock = 2412785664

// latepoolRun is one of the tester's latepool runs as replayLatepool replays it.
type latepoolRun struct {
	name string
	// band is the run's readings from the first in the band, the first's after unused, and forksAt
	// the index of the band reading before which the forks start, len(band) where they start after
	// the band's last reading.
	band    []replayReading
	forksAt int
	mark    replayReading
	after   []replayReading
	// limit is the pod's limit, podLimit where it is 0, and block the size each sleeping fork
	// reports, latepoolBlock where it is 0; own is what each fork holds that its parent does not:
	// the parent holds block less own once full.
	limit, block, own uint64
	// stall is how long the reading at the mark takes before the guard reads anything, and
	// perRead how long each smaps read at the mark takes until the kill; withinInterval asserts
	// that the first kill comes within one interval of the reading at the mark.
	stall, perRead time.Duration
	withinInterval bool
	// preExec, where set, has the growers start before the band's last reading, which reads each
	// before its exec, with its parent's executable and arguments, at preExec bytes; they run the
	// grow program from the reading at the mark on.
	preExec uint64
	// forkGrowers has the growers be forks that run their parent's program and grow, each holding
	// the block besides what it grows (latefork2); lateForks has three more sleeping forks start
	// with the growers, after the band's last reading.
	forkGrowers, lateForks bool
	// grown is each grower's size, less its 1 MiB offset and any block, in MiB, at the reading at
	// the mark and at each reading after it.
	grown []uint64
	// newRead is the first kill's new_read, and lastNewRead, where set, the last kill's; atMark is
	// how many kills the reading at the mark sends, one where it is 0: at or above the urgent share,
	// every process that grew over the window and, where the decision holds a process that is not
	// new to the stay, every one new to it with memory of its own (INFRA-3531 R1 of spec versions
	// 39, 41 and 42).
	newRead, lastNewRead string
	atMark               int
	want                 []endedChoice
}

// replayLatepool replays run: the agent holds a quarter of the limit, the probe's parent, pid 700,
// fills its block over the band's readings, three sleeping forks, pids 701 to 703, start as forksAt
// says, and six growers, pids 800 to 805, start after the band's last reading (or before it, read
// before their exec, where preExec is set), each 1 MiB apart. It asserts the first kill, at the
// reading at the mark, and every kill to the run's last reading, and that the guard gave no rise
// up.
func replayLatepool(t *testing.T, run latepoolRun) {
	t.Helper()
	limit, block := run.limit, run.block
	if limit == 0 {
		limit = podLimit
	}
	if block == 0 {
		block = latepoolBlock
	}
	kb := func(bytes uint64) uint64 { return bytes >> 10 << 10 }
	at := func(hundredths uint64) uint64 { return kb(limit / 10000 * hundredths) }
	plateau := run.band[len(run.band)-1].hundredths
	if run.forksAt < len(run.band) {
		plateau = run.band[run.forksAt-1].hundredths
	}
	// The parent fills its block as the band's readings rise, and holds it from the plateau on.
	parentAt := func(hundredths uint64) uint64 {
		return block - run.own - (at(plateau) - min(at(hundredths), at(plateau)))
	}
	pod := newFakePod(t)
	pod.setLimit(strconv.FormatUint(limit, 10))
	parent := fakeProcess{pid: 700, ppid: 100, start: 5000, exe: "/usr/bin/python3.13", args: latepoolArgs, anon: parentAt(run.band[0].hundredths)}
	pod.add(agent(limit/4), parent)
	pod.setAnon(at(run.band[0].hundredths))
	h := newHarness(t, pod, protect("tester", 100))
	stall, perRead := slowMark(h)
	exit := exitFreeing(pod, 0)
	h.signal.onKill = func(pid int) bool {
		// The reads after a kill cost nothing: the read costs model the reads before it.
		perRead.Store(0)
		return exit(pid)
	}
	h.run()
	fork := func(first int) {
		for i := range 3 {
			pod.add(fakeProcess{pid: first + i, ppid: 700, start: uint64(5000 + 100*(first-700+i)), exe: "/usr/bin/python3.13", args: latepoolArgs, anon: block})
		}
	}
	growers := make([]*fakeProcess, 6)
	for i := range growers {
		growers[i] = &fakeProcess{pid: 800 + i, ppid: 700, start: uint64(6000 + i), exe: "/usr/bin/python3.13", args: growArgs}
		if run.forkGrowers {
			growers[i].args = latepoolArgs
		}
	}
	for i, r := range run.band[1:] {
		if i+1 == run.forksAt {
			fork(701)
		}
		if run.preExec > 0 && i+2 == len(run.band) {
			// The growers start, and the band's last reading reads each before its exec.
			for _, g := range growers {
				pod.add(fakeProcess{pid: g.pid, ppid: g.ppid, start: g.start, exe: g.exe, args: latepoolArgs, anon: run.preExec})
			}
		}
		pod.resize(&parent, parentAt(r.hundredths))
		pod.setAnon(at(r.hundredths))
		h.tickAfter(r.after - Defaults().Interval)
	}
	if run.forksAt == len(run.band) {
		fork(701)
	}
	if run.lateForks {
		fork(704)
	}
	// grow sets each grower that lives to its size at the reading of index i, 0 the mark's.
	grow := func(i int) {
		killed := h.signal.kills()
		for j, g := range growers {
			if slices.Contains(killed, g.pid) {
				continue
			}
			size := (run.grown[i] + uint64(j)) * mib
			if run.forkGrowers {
				size += block
			}
			pod.resize(g, size)
		}
	}
	grow(0)
	pod.setAnon(at(run.mark.hundredths))
	stall.Store(int64(run.stall))
	perRead.Store(int64(run.perRead))
	markAt := h.clock.Now().Add(run.mark.after)
	h.tickAfter(run.mark.after - Defaults().Interval)
	stall.Store(0)
	perRead.Store(0)
	// spent is how long the reading at the mark took by the guard's clock.
	spent := h.clock.Now().Sub(markAt)
	if run.withinInterval {
		if kills := h.killed(); len(kills) == 0 || kills[0].At.Sub(markAt) > Defaults().Interval {
			t.Fatalf("the first kill %+v came after one interval from the reading at the mark, at %s", kills, markAt)
		}
	}
	if got, want := endedChoices(h), run.want[:max(run.atMark, 1)]; !slices.Equal(got, want) {
		t.Fatalf("at the first reading at the mark the guard ended %+v, want %+v; the log:\n%s", got, want, strings.Join(h.log.all(), "\n"))
	}
	// The kills of the reading at the mark are one decision's, and each line carries its new_read.
	for _, c := range run.want[:max(run.atMark, 1)] {
		requireNewRead(h, c.pid, run.newRead)
	}
	for i, r := range run.after {
		grow(i + 1)
		pod.setAnon(at(r.hundredths))
		// The guard waits its interval from the end of its tick, so the stall at the mark comes
		// off the time to the reading after it.
		extra := r.after - Defaults().Interval
		if i == 0 {
			extra -= spent
		}
		h.tickAfter(extra)
	}
	if got := endedChoices(h); !slices.Equal(got, run.want) {
		t.Fatalf("the guard ended %+v, want %+v; the log:\n%s", got, run.want, strings.Join(h.log.all(), "\n"))
	}
	if run.lastNewRead != "" {
		requireNewRead(h, run.want[len(run.want)-1].pid, run.lastNewRead)
	}
	if stopped := h.log.with("stopped"); len(stopped) != 0 {
		t.Fatalf("the guard gave the rise up: %q", stopped)
	}
}

// The tester's run `4g-latepool-r5` at bd8bb44c under gVisor at 4 GiB (infra-3531-live-4g-latepool-r5.log
// in dispatch://INFRA-3531/artifact/tester-evidence-bd8bb44c-2026-11-18-tar-gz), replayed with the
// readings its trace recorded and their times. The probe's parent holds its 2.4 GB block from the
// band's third reading on, and its three sleeping forks start before the band's seventh reading.
// Its first kill, at the first reading at the mark, 93.96%, at or above the urgent share, ended the
// parent by rule=largest with sizes=reread and new_read=0:0, while six growers that had each taken a
// 64 MiB step lived: the reads at the mark reached no process new to the stay, and no held process
// grew over the window there. The guard ended the six at the next reading, 110 ms later. One way to
// that line, modelled here: a held process that grew between the band's last two readings is gone
// by the reading at the mark (it exited, or its smaps no longer reads), so the reads at the mark
// leave it out; no held process grows over the window any longer, yet the held growth keeps the
// reading from searching for the processes started since, and the growers, started after the
// band's last reading, are not read. R1 (spec version 39) ends, where none grew over the window,
// the largest process new to this stay in the band that the guard reads within half an interval,
// reading at least one: here a grower, found started since, and never the parent first.
func TestMemoryGuardSearchesAtAnUrgentReadingAtTheMarkWhereTheHeldGrowthIsGoneOnTheReadAgain(t *testing.T) {
	limit := uint64(podLimit)
	at := func(hundredths uint64) uint64 { return limit / 10000 * hundredths >> 10 << 10 }
	band := []replayReading{{7572, 0}, {7905, 101 * time.Millisecond}, {8373, 101 * time.Millisecond},
		{8373, 101 * time.Millisecond}, {8373, 101 * time.Millisecond}, {8373, 101 * time.Millisecond},
		{8416, 101 * time.Millisecond}, {8457, 102 * time.Millisecond}, {8457, 104 * time.Millisecond}}
	pod := newFakePod(t)
	pod.setLimit(strconv.FormatUint(limit, 10))
	parent := fakeProcess{pid: 700, ppid: 100, start: 5000, exe: "/usr/bin/python3.13", args: latepoolArgs, anon: latepoolBlock / 2}
	pod.add(agent(limit/4), parent)
	pod.setAnon(at(band[0].hundredths))
	h := newHarness(t, pod, protect("tester", 100))
	h.signal.onKill = exitFreeing(pod, 0)
	h.run()
	// A python3 child of the parent, started before the band's seventh reading, grows by 5 MiB
	// between the band's last two readings and is gone by the reading at the mark.
	brief := fakeProcess{pid: 750, ppid: 700, start: 5500, exe: "/usr/bin/python3.13", args: []string{"/usr/bin/python3", "-c", "brief"}, anon: mib}
	for i, r := range band[1:] {
		reading := i + 1
		if reading >= 2 {
			pod.resize(&parent, latepoolBlock)
		}
		if reading == 6 {
			for j := range 3 {
				pod.add(fakeProcess{pid: 701 + j, ppid: 700, start: uint64(5100 + 100*j), exe: "/usr/bin/python3.13", args: latepoolArgs, anon: latepoolBlock})
			}
			pod.add(brief)
		}
		if reading == len(band)-1 {
			pod.resize(&brief, 6*mib)
		}
		pod.setAnon(at(r.hundredths))
		h.tickAfter(r.after - Defaults().Interval)
	}
	pod.remove(brief.pid)
	growers := []int{800, 801, 802, 803, 804, 805}
	for j, pid := range growers {
		pod.add(fakeProcess{pid: pid, ppid: 700, start: uint64(6000 + j), exe: "/usr/bin/python3.13", args: growArgs, anon: (64 + 1 + uint64(j)) * mib})
	}
	pod.setAnon(at(9396))
	h.tickAfter(104*time.Millisecond - Defaults().Interval)
	got := endedChoices(h)
	if len(got) == 0 || !slices.Contains(growers, got[0].pid) {
		t.Fatalf("at the urgent reading at the mark the guard ended %+v first, want a grower; the log:\n%s", got, strings.Join(h.log.all(), "\n"))
	}
	for _, c := range got {
		if !slices.Contains(growers, c.pid) {
			t.Fatalf("at the urgent reading at the mark the guard ended %+v, which is no grower; the log:\n%s", c, strings.Join(h.log.all(), "\n"))
		}
	}
}

// After a kill, a dip below the mark that stays in the band ends the rise and not the stay, and
// the order's third tier counts growth since the stay's first kill across it (INFRA-3531 R1 of
// spec version 37). The tester's runs `8g-latepool-r2` and `8g-latepool-r3` (the latepool probe)
// and `8g-latefork2-r1` (its growers forks of the parent that run its program and grow) of
// 2026-11-18 at 15bdbd3c under gVisor at 8 GiB, replayed with the readings their traces recorded
// and their times, through the first kill after the dip (infra-3531-live-8g-latepool-r2.log, -r3
// and the probe's 8g-latefork2-r1.log, in dispatch://INFRA-3531/artifact/tester-evidence-15bdbd3c-
// 2026-11-18-tar-gz): the agent holds 2 GiB, the probe's parent a block of about 4.8 GB, three
// sleeping forks report the block as their own, and six growers step 64 MiB about every 250 ms
// together. The guard ends five growers in the first rise, and the reading then sits below the mark
// in the band, by 2.6 MB in r2 and 1.7 MB in r3 for three readings, and by 52 MB for one in
// latefork2, and comes back. The grower left reads the same at each reading of the dip, and steps
// just before the reading at the mark, so no read since the dip shows it growing: the window at
// the mark holds two flat reads of it, its stay since the dip never grew, so the mark does not read
// it again, and nothing is new. At 15bdbd3c a guard that let the first kill's sizes go below the
// mark ended the probe's parent there by its whole 4.8 GB (`rule=largest sizes=reread
// new_read=0:0`), whose end freed about 6 MB, and the grower at the next reading. The guard ends
// the grower there, by its growth since the stay's first kill, where the search read it partway
// into its first step.
func TestMemoryGuardEndsTheGrowerLeftAfterADipBelowTheMarkByItsGrowthSinceTheStaysFirstKill(t *testing.T) {
	const limit = 8 * gib
	// latepool is the sequence of r2 and r3: the largest grower by the search at the mark, four
	// by their growth over the window, and the one left by its growth since the stay's first kill.
	latepool := []endedChoice{
		{805, "new", "search", 0}, {804, "grew", "reading", 0}, {803, "grew", "reading", 0},
		{802, "grew", "reading", 0}, {801, "grew", "reading", 0}, {800, "grew-since-kill", "reread", 0},
	}
	for _, run := range []latepoolRun{
		{
			name: "8g-latepool-r2, three readings 2.6 MB below the mark",
			band: []replayReading{{7562, 0}, {7738, 101 * time.Millisecond}, {7945, 101 * time.Millisecond},
				{8137, 102 * time.Millisecond}, {8235, 101 * time.Millisecond}, {8235, 101 * time.Millisecond},
				{8257, 102 * time.Millisecond}},
			forksAt: 6,
			mark:    replayReading{8571, 156 * time.Millisecond},
			after: []replayReading{{8665, 129 * time.Millisecond}, {8584, 103 * time.Millisecond}, {8739, 105 * time.Millisecond},
				{8579, 102 * time.Millisecond}, {8466, 103 * time.Millisecond}, {8497, 103 * time.Millisecond},
				{8497, 103 * time.Millisecond}, {8575, 103 * time.Millisecond}},
			limit:       limit,
			block:       4817903616,
			grown:       []uint64{30, 70, 70, 134, 134, 198, 198, 198, 262},
			newRead:     "9:6",
			want:        latepool,
			lastNewRead: "0:0",
		},
		{
			name: "8g-latepool-r3, the forks read twice by the band, three readings 1.7 MB below the mark",
			band: []replayReading{{7521, 0}, {7693, 102 * time.Millisecond}, {7873, 101 * time.Millisecond},
				{8059, 102 * time.Millisecond}, {8238, 102 * time.Millisecond}, {8238, 101 * time.Millisecond},
				{8248, 103 * time.Millisecond}, {8259, 106 * time.Millisecond}},
			forksAt: 6,
			mark:    replayReading{8648, 179 * time.Millisecond},
			after: []replayReading{{8668, 119 * time.Millisecond}, {8586, 106 * time.Millisecond}, {8739, 104 * time.Millisecond},
				{8579, 103 * time.Millisecond}, {8498, 102 * time.Millisecond}, {8498, 103 * time.Millisecond},
				{8498, 102 * time.Millisecond}, {8576, 102 * time.Millisecond}},
			limit:       limit,
			block:       4818214912,
			grown:       []uint64{30, 70, 70, 134, 134, 198, 198, 198, 262},
			newRead:     "6:6",
			want:        latepool,
			lastNewRead: "0:0",
		},
		{
			// The growers run the parent's program, so each ranks by what it holds over the
			// parent's block (less_parent), and three more sleeping forks start with them.
			name: "8g-latefork2-r1, the growers forks of the parent, one reading 52 MB below the mark",
			band: []replayReading{{7568, 0}, {7842, 101 * time.Millisecond}, {8109, 102 * time.Millisecond},
				{8237, 101 * time.Millisecond}, {8237, 101 * time.Millisecond}, {8256, 101 * time.Millisecond},
				{8256, 102 * time.Millisecond}, {8256, 102 * time.Millisecond}, {8256, 101 * time.Millisecond},
				{8256, 102 * time.Millisecond}, {8256, 102 * time.Millisecond}, {8256, 102 * time.Millisecond},
				{8256, 102 * time.Millisecond}, {8256, 102 * time.Millisecond}, {8256, 102 * time.Millisecond},
				{8256, 101 * time.Millisecond}, {8256, 102 * time.Millisecond}, {8256, 102 * time.Millisecond},
				{8256, 102 * time.Millisecond}, {8256, 102 * time.Millisecond}, {8256, 102 * time.Millisecond},
				{8256, 101 * time.Millisecond}, {8256, 102 * time.Millisecond}, {8256, 102 * time.Millisecond},
				{8256, 101 * time.Millisecond}},
			forksAt: 5,
			mark:    replayReading{8551, 102 * time.Millisecond},
			after: []replayReading{{8697, 117 * time.Millisecond}, {8613, 106 * time.Millisecond}, {8687, 105 * time.Millisecond},
				{8602, 103 * time.Millisecond}, {8440, 104 * time.Millisecond}, {8518, 102 * time.Millisecond}},
			limit:       limit,
			block:       4817117184,
			forkGrowers: true,
			lateForks:   true,
			grown:       []uint64{30, 64, 64, 128, 128, 128, 192},
			newRead:     "9:6",
			want: []endedChoice{
				{805, "new", "search", 4817117184}, {804, "grew", "reading", 4817117184}, {803, "grew", "reading", 4817117184},
				{802, "grew", "reading", 4817117184}, {801, "grew", "reading", 4817117184},
				{800, "grew-since-kill", "reread", 4817117184},
			},
			lastNewRead: "0:0",
		},
	} {
		t.Run(run.name, func(t *testing.T) { replayLatepool(t, run) })
	}
}

// markReads runs a guard on a pod whose agent holds 2 GiB and whose pid 700, python3 pool.py of 1
// GiB, and pid 720, python3 idle.py of 300 MiB, sleep through the band's readings: the processes
// held start before the band's last reading, which reads each once, and the processes started
// start after it. At the reading at the mark, where nothing has grown, the guard's clock moves on
// by stall before it reads anything and by perRead at each smaps it reads. It answers the guard
// and the pids whose smaps the guard's loop read at the mark before the kill, in order.
func markReads(t *testing.T, held, started []fakeProcess, stall, perRead time.Duration) (*harness, []int) {
	t.Helper()
	pod := newFakePod(t)
	pod.add(agent(2*gib),
		fakeProcess{pid: 700, ppid: 100, start: 5000, exe: "/usr/bin/python3.13", args: []string{"python3", "pool.py"}, anon: gib},
		fakeProcess{pid: 720, ppid: 100, start: 5020, args: []string{"python3", "idle.py"}, anon: 300 * mib})
	pod.setAnon(3200 * mib)
	h := newHarness(t, pod, protect("tester", 100))
	reads := recordReads(h)
	atMark, each := slowMark(h)
	h.run()
	pod.setAnon(3300 * mib)
	h.tick()
	pod.add(held...)
	pod.setAnon(3400 * mib)
	h.tick()
	inTheBand := len(reads())

	pod.add(started...)
	pod.setAnon(3500 * mib)
	atMark.Store(int64(stall))
	each.Store(int64(perRead))
	h.tick()
	return h, readOrderBeforeTheKill(reads()[inTheBand:])
}

// A fork whose parent's size covers its own ranks at nothing and is not in the tier of the
// processes new to the stay (INFRA-3531 R1 of spec version 34): the reads at the mark read on past
// half the interval while they have read no process new to the stay with memory of its own, up to
// one interval from the reading at the mark, stopping after the read in hand once it has passed,
// and where only such forks were read, the tiers below the new decide (markReads). Before the
// band's last reading pid 700 forks pids 701 and 702, which run its program and report its 1 GiB
// as their own, so each ranked at nothing at its one band read; nothing starts after it. The
// reading at the mark takes as long as each case gives before the guard reads anything, as under
// the pod's CPU throttle. Where that is 60 ms, past half the interval, the guard reads on to both
// forks, the newest first; where it is one interval, it reads the newest alone, the one at least.
// Either way it ends the largest by its rank size: pid 700, whose program is not its parent's, at
// its whole 1 GiB, before pid 720.
func TestMemoryGuardReadsOnPastAForkThatRanksAtNothingForOneIntervalAndThenEndsByTheTiersBelowTheNew(t *testing.T) {
	forks := []fakeProcess{
		{pid: 701, ppid: 700, start: 7000, exe: "/usr/bin/python3.13", args: []string{"python3", "pool.py"}, anon: gib},
		{pid: 702, ppid: 700, start: 7010, exe: "/usr/bin/python3.13", args: []string{"python3", "pool.py"}, anon: gib},
	}
	for _, tc := range []struct {
		name    string
		stall   time.Duration
		read    []int
		newRead string
	}{
		{"a reading at the mark of 60 ms", 60 * time.Millisecond, []int{702, 701}, "2:0"},
		{"a reading at the mark of one interval", Defaults().Interval, []int{702}, "1:0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, read := markReads(t, forks, nil, tc.stall, 0)
			requireMarkKill(h, 700, gib)
			requireChosenBy(h, 700, "largest", "reread")
			requireNewRead(h, 700, tc.newRead)
			if !slices.Equal(read, tc.read) {
				t.Fatalf("before the kill the guard read the smaps of %v in that order, want %v", read, tc.read)
			}
		})
	}
}

// At the reading at the mark, where none of the held processes grew, the guard reads the processes
// new to the stay in three groups, each the newest first (INFRA-3531 R1 of spec version 38): those
// with one band read that held memory of their own at it (rereadHeld), then those with no earlier
// read, started since the band's last reading (startedSince), then those with one band read that
// ranked at nothing at it, forks whose parent's size covered their own (readHeldNew), so the reads
// of sleeping forks do not spend the interval before a new process is read: at 43213c99, in
// `4g-latepool-rnr56`, the reads of three sleeping forks started inside the band spent the
// interval and the first kill went to their 2.4 GB parent. Before the band's last reading pid 710,
// python3 worker.py of 100 MiB, starts, and pid 700 forks pid 701, newer, which runs its program
// and reports its 1 GiB as its own; after it pid 730, python3 grow.py of 50 MiB, starts
// (markReads). Where the reads take no time the guard reads pid 710, pid 730 and pid 701 in that
// order, and ends pid 710, the largest new process with memory of its own. Where each read takes
// 30 ms, the half interval has passed once pid 730 is read, and pid 701 is not read. Without pid
// 710, the guard reads pid 730 before pid 701, ends pid 730, and reads pid 701 within the half
// interval.
func TestMemoryGuardReadsAtTheMarkTheNewWithMemoryOfTheirOwnThenTheStartedSinceThenTheForks(t *testing.T) {
	withOwn := fakeProcess{pid: 710, ppid: 100, start: 7000, args: []string{"python3", "worker.py"}, anon: 100 * mib}
	fork := fakeProcess{pid: 701, ppid: 700, start: 7010, exe: "/usr/bin/python3.13", args: []string{"python3", "pool.py"}, anon: gib}
	started := []fakeProcess{{pid: 730, ppid: 100, start: 7030, args: []string{"python3", "grow.py"}, anon: 50 * mib}}
	for _, tc := range []struct {
		name    string
		held    []fakeProcess
		perRead time.Duration
		read    []int
		want    int
		size    uint64
		newRead string
	}{
		{"each group, the reads taking no time", []fakeProcess{withOwn, fork}, 0, []int{710, 730, 701}, 710, 100 * mib, "3:2"},
		{"each group, each read 30 ms", []fakeProcess{withOwn, fork}, 30 * time.Millisecond, []int{710, 730}, 710, 100 * mib, "2:2"},
		{"no held process with memory of its own, each read 30 ms", []fakeProcess{fork}, 30 * time.Millisecond, []int{730, 701}, 730, 50 * mib, "2:1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, read := markReads(t, tc.held, started, 0, tc.perRead)
			if !slices.Equal(read, tc.read) {
				t.Fatalf("before the kill the guard read the smaps of %v in that order, want %v", read, tc.read)
			}
			requireMarkKill(h, tc.want, tc.size)
			requireChosenBy(h, tc.want, "new", "search")
			requireNewRead(h, tc.want, tc.newRead)
		})
	}
}

// heldBeside is a pod whose agent holds 2 GiB and whose pid 700, the parent of what is added, a
// python3 program of 1 GiB, python3 pool.py, sleeps through the band's readings, the guard h on it
// with classify; started start before the band's third and last reading, which reads each of them
// once, and the reading after is at the mark, where nothing has grown.
func heldBeside(t *testing.T, classify Classifier, started ...fakeProcess) *harness {
	t.Helper()
	return heldBesideRunning(t, classify, []string{"python3", "pool.py"}, started...)
}

// heldBesideRunning is heldBeside whose pid 700 runs python3 with args.
func heldBesideRunning(t *testing.T, classify Classifier, args []string, started ...fakeProcess) *harness {
	t.Helper()
	pod := newFakePod(t)
	pod.add(agent(2*gib), fakeProcess{pid: 700, ppid: 100, exe: "/usr/bin/python3.13", args: args, anon: gib})
	pod.setAnon(3200 * mib)
	h := newHarness(t, pod, classify)
	h.run()
	pod.setAnon(3300 * mib)
	h.tick()
	pod.add(started...)
	pod.setAnon(3400 * mib)
	h.tick()
	pod.setAnon(3500 * mib)
	h.tick()
	return h
}

// The scan reads the first classifierArgsBytes of a process's arguments alone, and the discount
// sets no bound on their length (INFRA-3531 R1 of spec version 33): where a process's first 4,096
// bytes of arguments are its parent's and either list fills them, of 4,096 bytes or more, the guard
// reads both argument lists whole and compares them. Pid 700 runs python3 -c with a program, 1
// GiB. Before the band's last reading pid 701, of 1 GiB and 2 MiB, starts as its child, with the
// arguments each case gives, and pid 720, of 250 MiB, under the agent. At the mark nothing has
// grown. A fork with its parent's whole list ranks at 2 MiB, and pid 720 ends; one whose list
// differs from its parent's past the first 4,096 bytes runs a program of its own, ranks whole, and
// ends itself. A list of exactly 4,096 bytes may be cut as well as whole by the scan's read: it is
// read whole, so one that is its parent's whole list is discounted, and one that is only the head
// of its parent's longer list is not.
func TestMemoryGuardComparesArgumentListsLongerThanTheScanReadsWhole(t *testing.T) {
	program := strings.Repeat("x", 5000)
	// exact is a program whose list, python3 -c and it, each argument ended by its NUL, is 4,096
	// bytes.
	exact := strings.Repeat("x", 4096-len("python3\x00-c\x00\x00"))
	for _, tc := range []struct {
		name         string
		parent, args []string
		want         int
		size         uint64
	}{
		{"the parent's whole list of 5,000 bytes", []string{"python3", "-c", program}, []string{"python3", "-c", program}, 720, 250 * mib},
		{"a list that differs past its first 4096 bytes", []string{"python3", "-c", program},
			[]string{"python3", "-c", program[:4500] + "y" + program[4501:]}, 701, gib + 2*mib},
		{"the parent's whole list of exactly 4096 bytes", []string{"python3", "-c", exact}, []string{"python3", "-c", exact}, 720, 250 * mib},
		{"a list of exactly 4096 bytes that begins its parent's longer one", []string{"python3", "-c", exact, "more"},
			[]string{"python3", "-c", exact}, 701, gib + 2*mib},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := heldBesideRunning(t, protect("tester", 100), tc.parent,
				fakeProcess{pid: 701, ppid: 700, start: 7000, exe: "/usr/bin/python3.13", args: tc.args, anon: gib + 2*mib},
				fakeProcess{pid: 720, ppid: 100, start: 7020, args: []string{"python3", "idle.py"}, anon: 250 * mib},
			)
			requireMarkKill(h, tc.want, tc.size)
			requireRankedBy(h, tc.want, "new", "reread", 0)
		})
	}
}

// A decision reads a parent's whole argument list once for all its forks (sameProgram). Pid 700
// runs python3 -c with a program of 5,000 bytes, and before the band's second reading it forks
// pids 701, 702 and 703, which run its program; at that reading, one assessment, the guard reads
// each fork's list whole once and pid 700's once.
func TestMemoryGuardReadsAParentsWholeArgumentsOnceForAllItsForks(t *testing.T) {
	args := []string{"python3", "-c", strings.Repeat("x", 5000)}
	pod := newFakePod(t)
	pod.add(agent(2*gib), fakeProcess{pid: 700, ppid: 100, exe: "/usr/bin/python3.13", args: args, anon: gib})
	pod.setAnon(3200 * mib)
	h := newHarness(t, pod, protect("tester", 100))
	var mu sync.Mutex
	read := map[int]int{}
	h.g.argsRead = func(pid int) {
		mu.Lock()
		defer mu.Unlock()
		read[pid]++
	}
	h.run()
	for i := range 3 {
		pod.add(fakeProcess{pid: 701 + i, ppid: 700, start: uint64(7000 + i), exe: "/usr/bin/python3.13", args: args, anon: gib + mib})
	}
	pod.setAnon(3300 * mib)
	h.tick()
	mu.Lock()
	defer mu.Unlock()
	if want := map[int]int{700: 1, 701: 1, 702: 1, 703: 1}; !maps.Equal(read, want) {
		t.Fatalf("at one reading the guard read the whole argument lists of %v, by pid, want %v", read, want)
	}
}

// A new process that runs its parent's executable with other arguments has run a program of its
// own, and ranks by its whole size (INFRA-3531 R1 of spec version 33). Pid 700 runs python3
// pool.py at 1 GiB. Before the band's last reading it forks pid 701, which runs the same program
// and reports 1 GiB and 2 MiB, and starts pid 702, python3 worker.py of 300 MiB; pid 720 of 250
// MiB starts under the agent. At the mark nothing has grown, and of the three new to the stay the
// guard ends pid 702, at its whole size, before pid 720 and before the fork, which ranks at 2 MiB.
func TestMemoryGuardRanksANewProcessThatRunsItsParentsExecutableWithOtherArgumentsByItsWholeSize(t *testing.T) {
	h := heldBeside(t, protect("tester", 100),
		fakeProcess{pid: 701, ppid: 700, start: 7000, exe: "/usr/bin/python3.13", args: []string{"python3", "pool.py"}, anon: gib + 2*mib},
		fakeProcess{pid: 702, ppid: 700, start: 7010, exe: "/usr/bin/python3.13", args: []string{"python3", "worker.py"}, anon: 300 * mib},
		fakeProcess{pid: 720, ppid: 100, start: 7020, args: []string{"python3", "idle.py"}, anon: 250 * mib},
	)
	requireMarkKill(h, 702, 300*mib)
	requireRankedBy(h, 702, "new", "reread", 0)
}

// A new process that runs another executable than its parent's, with its parent's arguments, has
// run a program of its own, and ranks by its whole size (INFRA-3531 R1 of spec version 33). Pid 700
// runs python3 pool.py at 1 GiB. Before the band's last reading it forks pid 701, which runs the
// same program and reports 1 GiB and 2 MiB, and starts pid 702, whose executable is node's and whose
// arguments are pid 700's, of 300 MiB; pid 720 of 250 MiB starts under the agent. At the mark
// nothing has grown, and the guard ends pid 702, at its whole size, before pid 720 and the fork.
func TestMemoryGuardRanksANewProcessThatRunsAnotherExecutableThanItsParentByItsWholeSize(t *testing.T) {
	h := heldBeside(t, protect("tester", 100),
		fakeProcess{pid: 701, ppid: 700, start: 7000, exe: "/usr/bin/python3.13", args: []string{"python3", "pool.py"}, anon: gib + 2*mib},
		fakeProcess{pid: 702, ppid: 700, start: 7010, exe: "/usr/bin/node", args: []string{"python3", "pool.py"}, anon: 300 * mib},
		fakeProcess{pid: 720, ppid: 100, start: 7020, args: []string{"python3", "idle.py"}, anon: 250 * mib},
	)
	requireMarkKill(h, 702, 300*mib)
	requireRankedBy(h, 702, "new", "reread", 0)
}

// A fork whose parent the guard has not read in this stay ranks by its whole size (INFRA-3531 R1
// of spec version 33): the classifier protects pid 700, so the guard reads no size of it. Its fork
// pid 701, which runs its program at 1 GiB and 2 MiB, and pid 720, of 250 MiB, start before the
// band's last reading; at the mark nothing has grown, and the guard ends the fork at its whole
// size, its line taking no parent's size off.
func TestMemoryGuardRanksAForkWhoseParentItHasNotReadByItsWholeSize(t *testing.T) {
	h := heldBeside(t, protect("tester", 100, 700),
		fakeProcess{pid: 701, ppid: 700, start: 7000, exe: "/usr/bin/python3.13", args: []string{"python3", "pool.py"}, anon: gib + 2*mib},
		fakeProcess{pid: 720, ppid: 100, start: 7020, args: []string{"python3", "idle.py"}, anon: 250 * mib},
	)
	requireMarkKill(h, 701, gib+2*mib)
	requireRankedBy(h, 701, "new", "reread", 0)
}

// A fork that runs its parent's program ends where it holds more over its parent's size than any
// other process holds (INFRA-3531 R1 of spec version 33), and its line takes its parent's size,
// 1 GiB, off: pid 701 has written 100 MiB of its own over pid 700's 1 GiB, and pid 720 holds 50
// MiB; both start before the band's last reading, and at the mark nothing has grown.
func TestMemoryGuardEndsAForkThatHoldsMoreThanItsParentAndSaysWhatItsRankDidNotCount(t *testing.T) {
	h := heldBeside(t, protect("tester", 100),
		fakeProcess{pid: 701, ppid: 700, start: 7000, exe: "/usr/bin/python3.13", args: []string{"python3", "pool.py"}, anon: gib + 100*mib},
		fakeProcess{pid: 720, ppid: 100, start: 7020, args: []string{"python3", "idle.py"}, anon: 50 * mib},
	)
	requireMarkKill(h, 701, gib+100*mib)
	requireRankedBy(h, 701, "new", "reread", gib)
}

// A fork that runs its parent's program and reads less than its parent's latest read ranks at
// nothing, its lessParent its own size (discountForks), and never past every other process, as
// its size less its parent's would wrap to (INFRA-3531 R1 of spec version 33, verification version
// 27). Pid 700 runs python3 pool.py at 1 GiB; before the band's last reading it forks pid 701,
// which runs the same program and reads 600 MiB. At the mark nothing has grown. Beside pid 720,
// python3 idle.py of 250 MiB started under the agent, the guard ends pid 720, the one process new
// to the stay with memory of its own; with no other, it ends the largest, pid 700, by its whole
// size.
func TestMemoryGuardRanksAForkSmallerThanItsParentAtNothing(t *testing.T) {
	fork := fakeProcess{pid: 701, ppid: 700, start: 7000, exe: "/usr/bin/python3.13", args: []string{"python3", "pool.py"}, anon: 600 * mib}
	for _, tc := range []struct {
		name    string
		started []fakeProcess
		want    int
		size    uint64
		rule    string
	}{
		{"beside a process new to the stay", []fakeProcess{fork,
			{pid: 720, ppid: 100, start: 7020, args: []string{"python3", "idle.py"}, anon: 250 * mib}}, 720, 250 * mib, "new"},
		{"alone", []fakeProcess{fork}, 700, gib, "largest"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := heldBeside(t, protect("tester", 100), tc.started...)
			requireMarkKill(h, tc.want, tc.size)
			requireChosenBy(h, tc.want, tc.rule, "reread")
		})
	}
}

// The reading at the mark reads again the executable and arguments of a held process whose smaps
// it reads where the band read it running its parent's program (reidentify): a process the band
// read between its fork and its exec carries its parent's identity there, and one that has run
// another program since ranks by its whole size. Pid 700 runs python3 pool.py at 1 GiB through
// the band. Before the band's last reading it starts pid 701, which the band reads with pid 700's
// executable and arguments, reporting pid 700's 1 GiB as its own. By the reading at the mark pid
// 701 runs python3 grow.py at 300 MiB, or still runs pool.py, a fork. Nothing has grown, and the
// mark reads pid 701 again. Running grow.py, it is new to the stay with memory of its own and ends,
// its line saying the mark read one process new to the stay that held memory of its own
// (new_read=1:1); a fork ranks at nothing, and the guard ends the largest, pid 700 (new_read=1:0).
func TestMemoryGuardRanksAHeldProcessThatHasRunAnotherProgramSinceTheBandReadItByItsWholeSize(t *testing.T) {
	for _, tc := range []struct {
		name    string
		args    []string
		anon    uint64
		want    int
		size    uint64
		rule    string
		newRead string
	}{
		{"one that runs grow.py since", []string{"python3", "grow.py"}, 300 * mib, 701, 300 * mib, "new", "1:1"},
		{"a fork that still runs pool.py", []string{"python3", "pool.py"}, gib, 700, gib, "largest", "1:0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := newFakePod(t)
			pod.add(agent(2*gib), fakeProcess{pid: 700, ppid: 100, start: 5000, exe: "/usr/bin/python3.13", args: []string{"python3", "pool.py"}, anon: gib})
			pod.setAnon(3200 * mib)
			h := newHarness(t, pod, protect("tester", 100))
			h.run()
			pod.setAnon(3300 * mib)
			h.tick()
			child := fakeProcess{pid: 701, ppid: 700, start: 7000, exe: "/usr/bin/python3.13", args: []string{"python3", "pool.py"}, anon: gib}
			pod.add(child)
			pod.setAnon(3400 * mib)
			h.tick()
			h.requireKilled()

			child.args = tc.args
			pod.resize(&child, tc.anon)
			pod.setAnon(3500 * mib)
			h.tick()
			requireMarkKill(h, tc.want, tc.size)
			requireChosenBy(h, tc.want, tc.rule, "reread")
			requireNewRead(h, tc.want, tc.newRead)
		})
	}
}

// After a kill, a process new to the stay ends before one that only grew since the kill, and
// before a larger one, where nothing grew over the window (INFRA-3531 R1 of spec version 31): at
// a reading of the rise after the kill, a process the guard has read at one reading before it at
// most is new, and one it has read at two is not. The band reads pids 301, 302 and 303 twice; at
// the mark nothing grew and none is new, and the guard ends the largest, pid 302, on the band's
// reads. Pid 302 lingers, and pid 310, of 100 MiB, starts and is read at the next reading, or at
// the next two, while the guard waits. Pid 302 then exits and frees nothing, and the reading after
// comes 400 ms later, pid 301 grown 64 MiB since the kill. Read once, pid 310 is new and ends,
// before pid 301, larger and grown since the kill; read twice, it is not, and pid 301 ends.
func TestMemoryGuardEndsAProcessNewToTheStayAfterAKillBeforeOneThatOnlyGrewSinceTheKill(t *testing.T) {
	for _, tc := range []struct {
		name  string
		reads int
		want  int
		size  uint64
		rule  string
	}{
		{"read at one reading before the one decided on", 1, 310, 100 * mib, "new"},
		{"read at two readings before the one decided on", 2, 301, 364 * mib, "grew-since-kill"},
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
			pod.setAnon(3300 * mib)
			h := newHarness(t, pod, protect("tester", 100))
			h.run()
			pod.setAnon(3400 * mib)
			h.tick()
			pod.setAnon(3600 * mib)
			h.tick()
			h.requireKilled(302)
			requireChosenBy(h, 302, "largest", "band")

			pod.add(fakeProcess{pid: 310, ppid: 100, args: []string{"python3", "worker.py"}, anon: 100 * mib})
			for range tc.reads {
				h.tick()
			}
			h.requireKilled(302)
			pod.remove(302)
			h.signal.exit(302)
			pod.resize(&grower, grower.anon+64*mib)
			h.tickAfter(growthWindow)
			h.requireKilled(302, tc.want)
			requireChosenBy(h, tc.want, tc.rule, "reading")
			ended := fmt.Sprintf("ended pid=%d role=tester size=%d reading=%d ", tc.want, tc.size, 3600*mib)
			if got := h.log.with(ended); len(got) != 1 {
				t.Fatalf("the ended lines are %q, want one starting %q", h.log.with("ended"), linePrefix+ended)
			}
			if stopped := h.log.with("stopped"); len(stopped) != 0 {
				t.Fatalf("the guard gave the rise up: %q", stopped)
			}
		})
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
// neither reads nor ends the helper; the line says pid 821 is new, by a size the search read.
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
	requireChosenBy(h, 821, "new", "search")
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

// slowIdentities has h's guard's clock move on by per at each identity it reads of a process
// started since the band's last reading (identityRead), as on a pod whose CPU the host throttles,
// and answers the pids it read, in order. It is set before the guard runs.
func slowIdentities(h *harness, per time.Duration) func() []int {
	var mu sync.Mutex
	var read []int
	h.g.identityRead = func(pid int) {
		mu.Lock()
		read = append(read, pid)
		mu.Unlock()
		h.clock.advance(per)
	}
	return func() []int {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(read)
	}
}

// heldSleeper is a pod whose agent holds 2 GiB and whose pid 700 sleeps at 900 MiB through the
// band's two readings, 3200 and 3300 MiB, the guard h on it run to the second: at the mark nothing
// held grows and pid 700 is not read again. before has the harness set up before the guard runs.
func heldSleeper(t *testing.T, classify Classifier, before func(h *harness)) (*fakePod, *harness) {
	t.Helper()
	pod := newFakePod(t)
	pod.add(agent(2*gib), fakeProcess{pid: 700, ppid: 100, args: []string{"python3", "pool.py"}, anon: 900 * mib})
	pod.setAnon(3200 * mib)
	h := newHarness(t, pod, classify)
	if before != nil {
		before(h)
	}
	h.run()
	pod.setAnon(3300 * mib)
	h.tick()
	return pod, h
}

// The reading at the mark reads the identities of the processes started since the band's last
// reading the highest pid first, the newest, within half an interval of the reading by the
// guard's clock, one at least, and stops after the read in hand once that has passed: at f949e5ad
// two latepool runs put the first kill 196.3 and 128.2 ms after the reading at the mark while it read
// every new process's identity (INFRA-3531). Pids 810, of 300 MiB, and 811, of 100 MiB, start as
// the reading reaches the mark, and each identity read takes 60 ms: the guard reads pid 811's
// alone and ends it, passing over the larger, older pid 810.
func TestMemoryGuardReadsTheIdentityOfTheNewestProcessStartedSinceTheBandWithinTheBudget(t *testing.T) {
	var identities func() []int
	pod, h := heldSleeper(t, protect("tester", 100), func(h *harness) { identities = slowIdentities(h, 60*time.Millisecond) })
	pod.add(
		fakeProcess{pid: 810, ppid: 100, args: []string{"python3", "big.py"}, anon: 300 * mib},
		fakeProcess{pid: 811, ppid: 100, args: []string{"python3", "small.py"}, anon: 100 * mib},
	)
	pod.setAnon(3500 * mib)
	h.tick()
	requireMarkKill(h, 811, 100*mib)
	if got, want := identities(), []int{811}; !slices.Equal(got, want) {
		t.Fatalf("at the mark the guard read the identities of %v, want %v", got, want)
	}
}

// A process started since the band's last reading whose parent is neither one of the band's
// processes nor a new process the reading at the mark read is left out of the decision there, since
// the classifier cannot place it: neither ended nor read. As the reading reaches the mark, pid 810
// starts under the agent and forks pid 821, of 400 MiB, and pid 815, of 150 MiB, starts under the
// agent too; each identity read takes 60 ms, more than the budget. No held process is new to the
// stay, so the search reads on past the budget until it has read a process the guard may end: pid
// 821's identity, which it cannot place, then pid 815's, and pid 815's smaps, and it stops there,
// pid 810 unread. Pid 815 ends, though pid 821 is larger. The assessment of that reading after the
// kill scans every process and places pid 821. Pid 815 exits at its kill and frees nothing; at the
// next reading, which has not fallen, pid 821 has grown 64 MiB, and the guard ends it there.
func TestMemoryGuardLeavesOutAtTheMarkAProcessWhoseNewParentItDidNotRead(t *testing.T) {
	var identities func() []int
	var reads func() []smapsReadAt
	pod, h := heldSleeper(t, protect("tester", 100), func(h *harness) {
		reads = recordReads(h)
		identities = slowIdentities(h, 60*time.Millisecond)
	})
	h.signal.onKill = exitFreeing(pod, 0)
	inTheBand := len(reads())
	child := fakeProcess{pid: 821, ppid: 810, args: []string{"python3", "work.py"}, anon: 400 * mib}
	pod.add(
		fakeProcess{pid: 810, ppid: 100, args: []string{"bash", "-c", "work"}, anon: 50 * mib},
		fakeProcess{pid: 815, ppid: 100, args: []string{"python3", "small.py"}, anon: 150 * mib},
		child,
	)
	pod.setAnon(3500 * mib)
	h.tick()
	requireMarkKill(h, 815, 150*mib)
	requireChosenBy(h, 815, "new", "search")
	if got, want := identities(), []int{821, 815}; !slices.Equal(got, want) {
		t.Fatalf("at the mark the guard read the identities of %v, want %v", got, want)
	}
	if got, want := readBeforeTheKill(reads()[inTheBand:]), []int{815}; !slices.Equal(got, want) {
		t.Fatalf("before the kill the guard read the smaps of %v, want %v", got, want)
	}

	pod.resize(&child, child.anon+64*mib)
	h.tick()
	h.requireKilled(815, 821)
	ended := fmt.Sprintf("ended pid=821 role=tester size=%d reading=%d ", 464*mib, 3500*mib)
	if got := h.log.with(ended); len(got) != 1 {
		t.Fatalf("the ended lines are %q, want one starting %q", h.log.with("ended"), linePrefix+ended)
	}
	if stopped := h.log.with("stopped"); len(stopped) != 0 {
		t.Fatalf("the guard gave the rise up: %q", stopped)
	}
}

// The reading at the mark takes the band's processes as the band read them and reads no stat of
// them: a pid another process took within one interval is improbable, and the kill reads its
// target's start time before it signals it. Pid 710, a held process of 100 MiB that the band's
// last two readings read, so one not new to the stay, reads another start time at the mark, as a
// process that took its pid would; the guard does not take it for a process started since, reads
// no identity, and ends pid 700, the largest held process.
func TestMemoryGuardTakesTheBandsProcessesAsTheBandReadThemAtTheMark(t *testing.T) {
	var identities func() []int
	pod, h := heldSleeper(t, protect("tester", 100), func(h *harness) { identities = slowIdentities(h, 0) })
	// Pid 710 is held too: it starts before the band's last two readings.
	pod.add(fakeProcess{pid: 710, ppid: 100, args: []string{"python3", "idle.py"}, anon: 100 * mib})
	for range 2 {
		pod.setAnon(3300 * mib)
		h.tick()
	}
	pod.restart(710, 99999)
	pod.setAnon(3500 * mib)
	h.tick()
	requireMarkKill(h, 700, 900*mib)
	if got := identities(); len(got) != 0 {
		t.Fatalf("at the mark the guard read the identities of %v, want none", got)
	}
}

// Growth over the window comes before a process new to the stay (INFRA-3531 R1 of spec version
// 31): where a held process grew by the band's reads, the guard ends it on the held reads, with no
// smaps read and no process looked for before the kill, though a process larger than it started as
// the reading reached the mark. Pid 710 grows 40 MiB between the band's two readings beside pid
// 700, a sleeper; pid 820, of 1 GiB, starts at the mark. The line says the process grew, by the
// band's reads.
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
	requireChosenBy(h, 710, "grew", "band")
	if got := readBeforeTheKill(reads()[inTheBand:]); len(got) != 0 {
		t.Fatalf("before the kill the guard read the smaps of %v, want none", got)
	}
}

// After a kill, the next in the order is the one that grew the most since that kill where nothing
// grew over the window and no process is new to the stay (INFRA-3531 R1 of spec version 31): a
// throttled pod's readings come more than the window apart, so the window holds one read of each
// process and shows no growth. A parent holds a block of 1.5 GiB and three children it forked
// sleep, each reporting the block as its own, beside a grower smaller than the block; the band
// reads each of them twice. At the first reading at the mark nothing has grown and none is new,
// and the guard ends the largest on the band's reads: the parent, since each fork runs its program
// and ranks by what it holds over the parent's size (INFRA-3531 R1 of spec version 33). It exits
// and frees nothing, its forks keeping its pages. The next reading comes 400 ms later, the grower
// 64 MiB larger, and the reading has not fallen: over the window nothing grew, but the grower grew
// since the kill, and the guard ends it, not a fork, which no read parent discounts any longer.
// Each line names its rule and where its sizes came from.
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
	// The agent, the block once and the grower: 3560 MiB, at the mark, after two readings in the
	// band.
	reading := func() uint64 { return gib + block + grower.anon }
	pod.setAnon(3300 * mib)
	h := newHarness(t, pod, protect("tester", 100))
	h.signal.onKill = exitFreeing(pod, 0)
	h.run()
	pod.setAnon(3400 * mib)
	h.tick()
	pod.setAnon(reading())
	h.tick()
	h.requireKilled(700)
	requireChosenBy(h, 700, "largest", "band")

	pod.resize(&grower, grower.anon+64*mib)
	pod.setAnon(reading())
	h.tickAfter(growthWindow)
	h.requireKilled(700, 710)
	requireChosenBy(h, 710, "grew-since-kill", "reading")
	if reads := h.g.sizes[procKey{pid: 710, start: 5400}].window; len(reads) != 1 {
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

// A kill on the band's held reads has as each process's size as of it the size the decision read,
// and not a read made after the kill (INFRA-3531 R1: a process's growth since the kill is its size
// at this reading less its size at the reading the kill was sent at). The shape of
// `8g-latepool-q56hm` at 7356e0f6: three growers, pids 800 to 802, step 64 MiB at each reading of
// the band, and at the first reading at the mark, 3500 MiB, the guard ends pid 802, the largest of
// equal growth, on the band's held sizes. The two left step 64 MiB between that reading and the
// guard's read after the kill, and then not again; the next reading, 100 ms later, is 3528 MiB, not
// fallen. At that reading, pid 802 having exited at its kill, each of them has grown 64 MiB since
// the kill, and the guard ends the larger, pid 801. A guard that kept the read after the kill as
// their sizes as of it saw no growth there and waited, as q56hm's did for two readings.
func TestMemoryGuardMeasuresGrowthSinceAKillOnHeldSizesFromTheSizesTheDecisionRead(t *testing.T) {
	pod := newFakePod(t)
	growers := []*fakeProcess{
		{pid: 800, ppid: 100, start: 6000, args: []string{"python3", "grow.py"}, anon: 200 * mib},
		{pid: 801, ppid: 100, start: 6001, args: []string{"python3", "grow.py"}, anon: 210 * mib},
		{pid: 802, ppid: 100, start: 6002, args: []string{"python3", "grow.py"}, anon: 220 * mib},
	}
	pod.add(agent(2 * gib))
	for _, g := range growers {
		pod.add(*g)
	}
	pod.setAnon(3200 * mib)
	h := newHarness(t, pod, protect("tester", 100))
	exit := exitFreeing(pod, 0)
	h.signal.onKill = func(pid int) bool {
		// The growers left step between the reading at the mark and the guard's read after the
		// kill.
		for _, g := range growers {
			if g.pid != pid {
				pod.resize(g, g.anon+64*mib)
			}
		}
		return exit(pid)
	}
	h.run()
	for _, inTheBand := range []uint64{3300, 3400} {
		for _, g := range growers {
			pod.resize(g, g.anon+64*mib)
		}
		pod.setAnon(inTheBand * mib)
		h.tick()
	}
	h.requireKilled()

	pod.setAnon(3500 * mib)
	h.tick()
	h.requireKilled(802)
	requireChosenBy(h, 802, "grew", "band")

	pod.setAnon(3528 * mib)
	h.tick()
	h.requireKilled(802, 801)
	ended := fmt.Sprintf("ended pid=801 role=tester size=%d reading=%d ", 402*mib, 3528*mib)
	if got := h.log.with(ended); len(got) != 1 {
		t.Fatalf("the ended lines are %q, want one starting %q", h.log.with("ended"), linePrefix+ended)
	}
	if stopped := h.log.with("stopped"); len(stopped) != 0 {
		t.Fatalf("the guard gave the rise up: %q", stopped)
	}
}

// A process the reading's table lists but whose smaps the reading could not read keeps its sizes
// as of the kill and as of the stay's first kill (dropEnded, INFRA-3531 R1): only a process the
// table no longer lists by its pid and start time has them dropped. Readings come 400 ms apart,
// wider than the window, so no read in the window shows growth. At the first reading at the mark
// the guard ends pid 300, of 900 MiB, the largest, which lingers. Pid 301, python3 grow.py of 300
// MiB, grows 64 MiB while the next reading cannot read its smaps, and reads 364 MiB at the two
// after, while the guard waits for pid 300; pid 302, python3 idle.py of 600 MiB, never grows. Pid
// 300 then exits, freeing nothing, and at the reading after, not fallen, pid 301 has grown 64 MiB
// since the kill: the guard goes on, and ends it by its growth since the stay's first kill, before
// the larger pid 302. A guard that dropped its sizes at the reading that did not read it took 364
// MiB, read after its step, as its size as of the kill, saw nothing grown and waited.
func TestMemoryGuardMeasuresGrowthSinceAKillAcrossAReadingThatCouldNotReadTheProcess(t *testing.T) {
	pod := newFakePod(t)
	grower := fakeProcess{pid: 301, ppid: 100, start: 3010, args: []string{"python3", "grow.py"}, anon: 300 * mib}
	pod.add(agent(2*gib), grower,
		fakeProcess{pid: 300, ppid: 100, start: 3000, args: []string{"python3", "big.py"}, anon: 900 * mib},
		fakeProcess{pid: 302, ppid: 100, start: 3020, args: []string{"python3", "idle.py"}, anon: 600 * mib})
	pod.setAnon(3600 * mib)
	h := newHarness(t, pod, protect("tester", 100))
	h.signal.onKill = func(int) bool { return false }
	h.run()
	h.requireKilled(300)

	const apart = 400*time.Millisecond - 100*time.Millisecond
	grower.unreadableSmaps = true
	pod.resize(&grower, 364*mib)
	h.tickAfter(apart)
	grower.unreadableSmaps = false
	pod.add(grower)
	h.tickAfter(apart)
	h.tickAfter(apart)
	h.requireKilled(300)

	h.signal.exit(300)
	pod.remove(300)
	h.tickAfter(apart)
	h.requireKilled(300, 301)
	requireChosenBy(h, 301, "grew-since-kill", "reading")
	if stopped := h.log.with("stopped"); len(stopped) != 0 {
		t.Fatalf("the guard gave the rise up while pid 301 grew: %q", stopped)
	}
}

// At the reading at the mark of a rise after a dip, a held process whose smaps the reads again
// cannot read is left out of that decision and keeps its size as of the stay's first kill, since
// the band's table still lists it (dropEnded, INFRA-3531 R1). Readings come 400 ms apart. At the
// first reading at the mark the guard ends pid 300, of 900 MiB, the largest, which exits; two
// readings follow in the band, below the mark, and pid 301, python3 grow.py of 300 MiB, grows 64
// MiB between them. At the next reading at the mark its smaps cannot be read: the guard ends the
// largest held process, pid 302, and pid 301's size as of the stay's first kill is still 300 MiB.
func TestMemoryGuardKeepsTheSizeAsOfTheStaysFirstKillOfAHeldProcessItCannotReadAgainAtTheMark(t *testing.T) {
	pod := newFakePod(t)
	grower := fakeProcess{pid: 301, ppid: 100, start: 3010, args: []string{"python3", "grow.py"}, anon: 300 * mib}
	pod.add(agent(2*gib), grower,
		fakeProcess{pid: 300, ppid: 100, start: 3000, args: []string{"python3", "big.py"}, anon: 900 * mib},
		fakeProcess{pid: 302, ppid: 100, start: 3020, args: []string{"python3", "idle.py"}, anon: 700 * mib},
		fakeProcess{pid: 303, ppid: 100, start: 3030, args: []string{"python3", "idle.py"}, anon: 500 * mib})
	pod.setAnon(3600 * mib)
	h := newHarness(t, pod, protect("tester", 100))
	h.signal.onKill = exitFreeing(pod, 0)
	h.run()
	h.requireKilled(300)

	const apart = 400*time.Millisecond - 100*time.Millisecond
	pod.setAnon(3400 * mib)
	h.tickAfter(apart)
	pod.resize(&grower, 364*mib)
	h.tickAfter(apart)
	grower.unreadableSmaps = true
	pod.add(grower)
	pod.setAnon(3600 * mib)
	h.tickAfter(apart)
	h.requireKilled(300, 302)
	key := procKey{pid: 301, start: 3010}
	if got, kept := h.g.atFirstKill[key]; !kept || got != 300*mib {
		t.Fatalf("pid 301's size as of the stay's first kill is %d (kept %t), want %d", got, kept, 300*mib)
	}
}

// A band reading that cannot read a process its table still lists leaves its stay in the band as
// it was (keepStays, INFRA-3531 R1: "grew in the band" is by two of its reads in the band). Band
// readings come 200 ms apart. Pid 310, python3 slow.py, reads 300 MiB at the band's first reading,
// cannot be read at the second, and reads 302 MiB at the third, 2 MiB above its least, as pid 320,
// python3 new.py of 500 MiB, starts; pid 330 sleeps at 600 MiB. At the reading at the mark, where
// each smaps read takes 60 ms, pid 310 has stepped 64 MiB: the guard reads it first, as the band's
// grower, before pid 320, the larger and newer process new to the stay, sees its step and ends it.
// A guard that dropped its stay at the reading that could not read it read pid 320 first, spent
// the half interval on it, and ended it.
func TestMemoryGuardReadsAgainFirstAtTheMarkABandGrowerOneBandReadingCouldNotRead(t *testing.T) {
	pod := newFakePod(t)
	slow := fakeProcess{pid: 310, ppid: 100, start: 3010, args: []string{"python3", "slow.py"}, anon: 300 * mib}
	pod.add(agent(2*gib), slow, fakeProcess{pid: 330, ppid: 100, start: 3030, args: []string{"python3", "idle.py"}, anon: 600 * mib})
	pod.setAnon(3200 * mib)
	h := newHarness(t, pod, protect("tester", 100))
	reads := recordReads(h)
	_, perRead := slowMark(h)
	h.run()
	const apart = 200*time.Millisecond - 100*time.Millisecond

	slow.unreadableSmaps = true
	pod.resize(&slow, 301*mib)
	pod.setAnon(3300 * mib)
	h.tickAfter(apart)
	slow.unreadableSmaps = false
	pod.resize(&slow, 302*mib)
	pod.add(fakeProcess{pid: 320, ppid: 100, start: 3040, args: []string{"python3", "new.py"}, anon: 500 * mib})
	pod.setAnon(3400 * mib)
	h.tickAfter(apart)
	h.requireKilled()
	inTheBand := len(reads())

	pod.resize(&slow, 366*mib)
	pod.setAnon(3500 * mib)
	perRead.Store(int64(60 * time.Millisecond))
	h.tickAfter(apart)
	if got, want := readOrderBeforeTheKill(reads()[inTheBand:]), []int{310}; !slices.Equal(got, want) {
		t.Fatalf("before the kill the guard read the smaps of %v in that order, want %v", got, want)
	}
	requireMarkKill(h, 310, 366*mib)
	requireChosenBy(h, 310, "grew", "reread")
}

// A reading that cannot read a process its table still lists keeps its reads within the window,
// so its growth over the window counts once it is read again within growthWindow of them, and
// restarts its count of readings, so it is new to the stay again (keepSizes, INFRA-3531 R1).
// Readings come 100 ms apart. At the first reading, at the mark, the guard ends pid 300, of 900
// MiB, the largest, which exits and frees nothing. At the next reading pid 301, python3 grow.py of
// 300 MiB, cannot be read, and pid 303, python3 new.py of 700 MiB, starts. At the reading after,
// not fallen, pid 301 reads 364 MiB, 64 MiB above its read 200 ms before: it ranks by its growth
// over the window ahead of pid 303, the larger process new to the stay, and ends. A guard that
// dropped its reads at the reading that could not read it saw no growth over the window and ended
// pid 303.
func TestMemoryGuardRanksByItsGrowthOverTheWindowAGrowerOneReadingCouldNotRead(t *testing.T) {
	pod := newFakePod(t)
	grower := fakeProcess{pid: 301, ppid: 100, start: 3010, args: []string{"python3", "grow.py"}, anon: 300 * mib}
	pod.add(agent(2*gib), grower,
		fakeProcess{pid: 300, ppid: 100, start: 3000, args: []string{"python3", "big.py"}, anon: 900 * mib},
		fakeProcess{pid: 302, ppid: 100, start: 3020, args: []string{"python3", "idle.py"}, anon: 600 * mib})
	pod.setAnon(3600 * mib)
	h := newHarness(t, pod, protect("tester", 100))
	h.signal.onKill = exitFreeing(pod, 0)
	h.run()
	h.requireKilled(300)

	grower.unreadableSmaps = true
	pod.add(grower, fakeProcess{pid: 303, ppid: 100, start: 3030, args: []string{"python3", "new.py"}, anon: 700 * mib})
	h.tick()
	h.requireKilled(300)
	grower.unreadableSmaps = false
	pod.resize(&grower, 364*mib)
	h.tick()
	h.requireKilled(300, 301)
	requireChosenBy(h, 301, "grew", "reading")
	r := h.g.sizes[procKey{pid: 301, start: 3010}]
	if len(r.window) != 2 || r.window[0].size != 300*mib || r.readings != 1 {
		t.Fatalf("pid 301's reads are %+v, want its read before the reading that could not read it, its read since, and one reading of the stay in a row", r)
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
	table := &Table{byPID: make(map[int]*Process, len(cs))}
	for i := range cs {
		cs[i] = candidate{Process: &Process{PID: 1000 + i, StartTime: uint64(i)}, size: uint64(i) * mib}
		table.byPID[cs[i].PID] = cs[i].Process
	}
	h.g.keepKillSizes(cs, nil)
	allocs := testing.AllocsPerRun(100, func() {
		if h.g.grownSinceKill(cs, table) {
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
// among them, and neither does a control character or a character that does not print. The guard
// starts at the mark, so no reading of a stay came before the kill and every process is new to it:
// the line says the new tier chose pid 900, on the sizes of the reading it is sent at.
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
	ended := fmt.Sprintf(`launcher: memory guard: ended pid=900 role=tester size=%d reading=%d counter=%d shmem=%d limit=%d rule=new sizes=reading less_parent=0 new_read=- executable="python3.13"`,
		1500*mib, 3600*mib, 3900*mib, 64*mib, podLimit)
	if got := h.log.with("ended"); !slices.Equal(got, []string{ended}) {
		t.Fatalf("the kill lines are\n%q\nwant\n%q", got, ended)
	}
	k := h.killed()[0]
	want := Kill{LauncherMemoryKill: shimwire.LauncherMemoryKill{Guard: h.g.ID(), Sequence: 1, PID: 900, Executable: "python3.13",
		SizeBytes: 1500 * mib, ReadingBytes: 3600 * mib, CounterBytes: 3900 * mib, LimitBytes: podLimit, At: testStart},
		Role: "tester", Rule: "new", Sizes: "reading"}
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
	ended := fmt.Sprintf(`launcher: memory guard: ended pid=900 role=tester size=%d reading=%d counter=%d shmem=%d limit=%d rule=new sizes=reading less_parent=0 new_read=- executable="%s"`,
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

// A live process whose exe is missing counts unread, is never ended, and the stopped line names it
// (INFRA-3531 plan version 31): the scan asks whether the process has ended since its stat was read
// before it takes a missing exe for its end. Pid 970's main thread has exited while another thread
// holds its memory: its stat reads Z with two threads, its exe is missing, as Linux answers for such
// a leader, and its smaps reads no Anonymous, as gVisor's does. Pid 971 runs and its exe is missing;
// its smaps reads 1 GiB. Pid 972, 600 MiB, may be ended. At the mark the guard ends pid 972, though
// pid 971 is larger, and logs an unread line for each of the other two. At the next rise nothing it
// may end is left, and its stopped line names pid 970 by its pid alone, a Z whose smaps would read
// as a size of 0 (INFRA-3531 R3 of spec version 30), and pid 971 with its size.
func TestMemoryGuardCountsUnreadAndNeverEndsALiveProcessWhoseExeIsMissing(t *testing.T) {
	pod := newFakePod(t)
	pod.add(
		agent(2*gib),
		fakeProcess{pid: 970, ppid: 100, args: []string{"python3", "threads.py"}, zombie: true, threads: 2, noExe: true},
		fakeProcess{pid: 971, ppid: 100, args: []string{"python3", "running.py"}, anon: gib, noExe: true},
		fakeProcess{pid: 972, ppid: 100, args: []string{"python3", "grow.py"}, anon: 600 * mib},
	)
	pod.setAnon(3700 * mib)
	h := newHarness(t, pod, protect("tester", 100))
	h.signal.onKill = exitFreeing(pod, 600*mib)
	h.run()
	h.requireKilled(972)
	unread := h.log.with("unread")
	if len(unread) != 2 || !strings.HasPrefix(unread[0], "launcher: memory guard: unread pid=970 reason=read the exe: ") ||
		!strings.HasPrefix(unread[1], "launcher: memory guard: unread pid=971 reason=read the exe: ") {
		t.Fatalf("the unread lines are %q, want one for pid 970 and one for pid 971", unread)
	}

	h.tick()
	pod.setAnon(3700 * mib)
	h.tick()
	h.requireKilled(972)
	want := fmt.Sprintf("launcher: memory guard: stopped for this rise reason=nothing to end reading=%d endable=0 protected=%d unread=%d unheld=%d limit=%d left_out=0 unread_processes=970,971:%d",
		3700*mib, 2*gib, gib, 3700*mib-3*gib, podLimit, gib)
	if got := h.log.with("stopped"); !slices.Equal(got, []string{want}) {
		t.Fatalf("the stopped lines are %q, want %q", got, want)
	}
}

// A process whose stat reads Z is named in the stopped line by its pid alone, and adds nothing to
// its unread sum, even where its own smaps reads a size (INFRA-3531 R3 of spec version 30): the
// memory of a process whose main thread has exited is its live threads', which the leader's smaps
// does not show the same under every kernel. Pid 975's stat reads Z with two threads, its exe does
// not read, as gVisor answers EACCES, and its smaps reads 300 MiB. Beside the agent, what the guard
// may end holds 200 MiB, under the floor: at the mark it gives the rise up, ends nothing, and its
// line reads unread=0 and unread_processes=975.
func TestMemoryGuardNamesAZombieLeaderWithThreadsLeftByItsPidAloneThoughItsSmapsReadsASize(t *testing.T) {
	pod := newFakePod(t)
	pod.add(agent(2 * gib))
	for i := range 5 {
		pod.add(fakeProcess{pid: 500 + i, ppid: 100, args: []string{"sleep", "600"}, anon: 40 * mib})
	}
	pod.add(fakeProcess{pid: 975, ppid: 100, args: []string{"python3", "threads.py"}, anon: 300 * mib, zombie: true, threads: 2, unreadableExe: true})
	pod.setAnon(3500 * mib)
	h := newHarness(t, pod, protect("tester", 100))
	h.run()
	h.requireKilled()
	if unread := h.log.with("unread"); len(unread) != 1 || !strings.HasPrefix(unread[0], "launcher: memory guard: unread pid=975 reason=read the exe: ") {
		t.Fatalf("the unread lines are %q, want one for pid 975", unread)
	}
	endable := 200 * mib
	want := fmt.Sprintf("launcher: memory guard: stopped for this rise reason=nothing to end reading=%d endable=%d protected=%d unread=0 unheld=%d limit=%d left_out=0 unread_processes=975",
		3500*mib, endable, 2*gib, 3500*mib-endable-2*gib, podLimit)
	if got := h.log.with("stopped"); !slices.Equal(got, []string{want}) {
		t.Fatalf("the stopped lines are %q, want %q", got, want)
	}
}

// A zombie with no thread left holds no memory, and a missing exe of it leaves it out of the scan as
// before: pid 980's stat reads Z with one thread and its exe is missing. The guard ends pid 981,
// logs no unread line, and the stopped line of the next rise names no process it could not read.
func TestMemoryGuardLeavesOutAZombieWithNoThreadLeftWhoseExeIsMissing(t *testing.T) {
	pod := newFakePod(t)
	pod.add(
		agent(2*gib),
		fakeProcess{pid: 980, ppid: 100, args: []string{"python3", "done.py"}, anon: gib, zombie: true, threads: 1, noExe: true},
		fakeProcess{pid: 981, ppid: 100, args: []string{"python3", "grow.py"}, anon: 600 * mib},
	)
	pod.setAnon(3700 * mib)
	h := newHarness(t, pod, protect("tester", 100))
	h.signal.onKill = exitFreeing(pod, 600*mib)
	h.run()
	h.requireKilled(981)
	h.tick()
	pod.setAnon(3700 * mib)
	h.tick()
	h.requireKilled(981)
	if unread := h.log.with("unread"); len(unread) != 0 {
		t.Fatalf("the unread lines are %q, want none", unread)
	}
	if stopped := h.log.with("stopped"); len(stopped) != 1 || !strings.HasSuffix(stopped[0], " unread=0 unheld="+strconv.FormatUint(3700*mib-2*gib, 10)+
		" limit="+strconv.FormatUint(podLimit, 10)+" left_out=0 unread_processes=none") {
		t.Fatalf("the stopped lines are %q, want one naming no process it could not read", stopped)
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

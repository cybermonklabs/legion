package launcher

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/memoryguard"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// useContainerInit has every launcher this test starts take its test process for the container's
// first process, or not, until the test's end (containerInit). No test of this package runs as
// PID 1, so every other test's launcher runs no memory guard, no git lock sweep and no cache
// trimmer at all; a test of the guard stands its process in for PID 1, and points the guard at a
// pod of its own (useGuardPod), and a test of the sweep and one of the trimmer as the launcher
// runs it do the same (useSweepingLauncher, useLauncherTrimmer).
func useContainerInit(t *testing.T, first bool) {
	t.Helper()
	previous := containerInit
	containerInit = func() bool { return first }
	t.Cleanup(func() { containerInit = previous })
}

const (
	guardMiB = uint64(1) << 20
	guardGiB = uint64(1) << 30
	// guardLimit is the fake container's limit: at the default act share of 85% its mark is
	// 3481.6 MiB, and at the default floor of 10% the floor is 409.6 MiB. guardHigh is a reading
	// at the mark and guardLow one well below it.
	guardLimit   = 4 * guardGiB
	guardHigh    = 3600 * guardMiB
	guardLow     = guardGiB
	guardCounter = 3 * guardGiB
)

// The PID namespaces of the fake pod: the container's, each role's, and one a process made for
// itself inside a role.
const (
	containerNamespace = "pid:[4026531836]"
	testerNamespace    = "pid:[4026532100]"
	reviewerNamespace  = "pid:[4026532200]"
	sandboxNamespace   = "pid:[4026532199]"
)

// guardPod is a /proc and a memory cgroup directory under t.TempDir(), written as the kernel writes
// them: meminfo in place, since the guard holds it open and reads it from its start, and each
// process's files whole, through a rename, since the guard's hourly scan reads them on a goroutine
// of its own.
type guardPod struct {
	t            *testing.T
	proc, cgroup string
}

func newGuardPod(t *testing.T) *guardPod {
	t.Helper()
	root := t.TempDir()
	p := &guardPod{t: t, proc: filepath.Join(root, "proc"), cgroup: filepath.Join(root, "cgroup")}
	for _, dir := range []string{p.proc, p.cgroup} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	p.setLimit(guardLimit)
	p.setCounter(guardCounter)
	p.setAnon(guardLow)
	return p
}

// setAnon writes the pod's AnonPages.
func (p *guardPod) setAnon(anon uint64) {
	p.writeFile(filepath.Join(p.proc, "meminfo"), fmt.Sprintf(
		"MemTotal:       20971520 kB\nMemFree:        10485760 kB\nAnonPages:      %d kB\nMapped:          520372 kB\nShmem:              32 kB\n", anon>>10))
}

// setCounter writes the cgroup's memory counter, which counts the pages of the files a process
// maps as well as its anonymous ones. It is written in place, as meminfo is: the guard holds it
// open.
func (p *guardPod) setCounter(counter uint64) {
	p.writeFile(filepath.Join(p.cgroup, "memory.usage_in_bytes"), strconv.FormatUint(counter, 10)+"\n")
}

// setLimit writes the container's memory limit, in place.
func (p *guardPod) setLimit(limit uint64) {
	p.writeFile(filepath.Join(p.cgroup, "memory.limit_in_bytes"), strconv.FormatUint(limit, 10)+"\n")
}

func (p *guardPod) writeFile(path, content string) {
	p.t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		p.t.Fatal(err)
	}
}

// guardProcess is one process of a guardPod: its place in the tree, its executable and arguments,
// its PID namespace, its working directory when it matters, the anonymous memory its smaps reads,
// the resident pages of a file on disk it maps shared, a database's say, which raise the cgroup's
// counter and not AnonPages, and which no size the guard ranks by counts, and its resident size,
// which its stat's rss field holds in pages, the size the guest watch reads of it. unreadableExe
// makes its exe a plain file, whose readlink fails as an unreadable link does, and unreadableSmaps
// its smaps a directory, whose read fails as a smaps the guard may not read does.
type guardProcess struct {
	pid, ppid       int
	exe             string
	args            []string
	ns              string
	cwd             string
	anon            uint64
	mapped          uint64
	resident        uint64
	unreadableExe   bool
	unreadableSmaps bool
}

// add writes each process. The test process holds a pid of its own, which stands for the
// launcher's (addLauncher), so a fake process of that pid would be written over it: the test
// skips.
func (p *guardPod) add(processes ...guardProcess) {
	p.t.Helper()
	for _, gp := range processes {
		if gp.pid == os.Getpid() {
			p.t.Skipf("the test process's own pid %d is one this test fakes", gp.pid)
		}
		p.write(gp)
	}
}

// addLauncher writes the launcher, which is the test process: the guard and the classifier both
// know the launcher by their own pid.
func (p *guardPod) addLauncher(exe string, anon uint64) {
	p.t.Helper()
	p.write(guardProcess{pid: os.Getpid(), exe: exe, args: []string{"legion", "launcher"}, anon: anon})
}

func (p *guardPod) write(gp guardProcess) {
	p.t.Helper()
	if gp.ns == "" {
		gp.ns = containerNamespace
	}
	name := filepath.Base(gp.args[0])
	dir := filepath.Join(p.proc, strconv.Itoa(gp.pid))
	if err := os.MkdirAll(filepath.Join(dir, "ns"), 0o755); err != nil {
		p.t.Fatal(err)
	}
	// The start time is the pid's, so a role-init's children are oldest first by pid.
	p.writeWhole(filepath.Join(dir, "stat"), fmt.Sprintf("%d (%s) S %d %d %d 0 -1 4194304 100 0 0 0 5 3 0 0 20 0 1 0 %d 123456789 %d "+
		"18446744073709551615 1 1 0 0 0 0 0 0 0 0 0 0 17 0 0 0 0 0 0\n", gp.pid, name, gp.ppid, gp.pid, gp.pid, gp.pid*10, gp.resident/uint64(os.Getpagesize())))
	p.writeWhole(filepath.Join(dir, "cmdline"), strings.Join(gp.args, "\x00")+"\x00")
	smaps := fmt.Sprintf(
		"01a5c000-41a5c000 rw-p 00000000 00:00 0                                  [heap]\nSize:           %d kB\nRss:            %d kB\nAnonymous:      %d kB\n",
		gp.anon>>10, gp.anon>>10, gp.anon>>10)
	if gp.mapped > 0 {
		smaps += fmt.Sprintf(
			"7f3a00000000-7f3b00000000 rw-s 00000000 08:01 1310722                    /var/lib/legion/big.db\nSize:           %d kB\nRss:            %d kB\nAnonymous:      0 kB\n",
			gp.mapped>>10, gp.mapped>>10)
	}
	if path := filepath.Join(dir, "smaps"); gp.unreadableSmaps {
		_ = os.RemoveAll(path)
		if err := os.MkdirAll(path, 0o755); err != nil {
			p.t.Fatal(err)
		}
	} else {
		// A smaps an earlier write made unreadable is a directory, which no file replaces.
		if info, err := os.Lstat(path); err == nil && info.IsDir() {
			_ = os.RemoveAll(path)
		}
		p.writeWhole(path, smaps)
	}
	links := []struct{ name, target string }{{"exe", gp.exe}, {filepath.Join("ns", "pid"), gp.ns}}
	if gp.cwd != "" {
		links = append(links, struct{ name, target string }{"cwd", gp.cwd})
	}
	for _, link := range links {
		path := filepath.Join(dir, link.name)
		_ = os.Remove(path)
		if link.name == "exe" && gp.unreadableExe {
			p.writeWhole(path, "")
			continue
		}
		if err := os.Symlink(link.target, path); err != nil {
			p.t.Fatal(err)
		}
	}
}

func (p *guardPod) writeWhole(path, content string) {
	p.t.Helper()
	tmp := path + ".tmp"
	p.writeFile(tmp, content)
	if err := os.Rename(tmp, path); err != nil {
		p.t.Fatal(err)
	}
}

// setScore writes pid's oom_score_adj, which the guest watch reads and raises, and its oom_score,
// the kernel's badness of it, which the watch does not read.
func (p *guardPod) setScore(pid, adj, score int) {
	p.t.Helper()
	dir := filepath.Join(p.proc, strconv.Itoa(pid))
	p.writeWhole(filepath.Join(dir, "oom_score"), fmt.Sprintf("%d\n", score))
	p.writeWhole(filepath.Join(dir, "oom_score_adj"), fmt.Sprintf("%d\n", adj))
}

// setSelfScore writes the oom_score_adj of the process that reads the pod's /proc, the guest
// watch's starting score.
func (p *guardPod) setSelfScore(adj int) {
	p.t.Helper()
	if err := os.MkdirAll(filepath.Join(p.proc, "self"), 0o755); err != nil {
		p.t.Fatal(err)
	}
	p.writeWhole(filepath.Join(p.proc, "self", "oom_score_adj"), fmt.Sprintf("%d\n", adj))
}

// score is pid's oom_score_adj as its file reads now.
func (p *guardPod) score(pid int) int {
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

// setKernelKills writes the cgroup's kill counter as a v1 kernel does, in memory.oom_control, with
// the memory.stat whose totals a kill's line reads: anon of anonymous memory and shmem of shared
// memory.
func (p *guardPod) setKernelKills(n, anon, shmem uint64) {
	p.writeFile(filepath.Join(p.cgroup, "memory.oom_control"), fmt.Sprintf("oom_kill_disable 0\nunder_oom 0\noom_kill %d\n", n))
	p.writeFile(filepath.Join(p.cgroup, "memory.stat"), fmt.Sprintf(
		"cache 4096\nrss 4096\ntotal_cache %d\ntotal_rss %d\ntotal_shmem %d\ntotal_inactive_file %d\n", 400*guardMiB, anon, shmem, 100*guardMiB))
}

// showKernelKill writes pid's status as the guest kernel's out-of-memory kill leaves the process
// while its memory is freed: still in /proc, with SIGKILL pending for its thread and its thread
// group, the evidence by which the guest watch names a kill.
func (p *guardPod) showKernelKill(pid int) {
	p.t.Helper()
	p.writeWhole(filepath.Join(p.proc, strconv.Itoa(pid), "status"), fmt.Sprintf(
		"State:\tR (running)\nTgid:\t%d\nPid:\t%d\nThreads:\t1\nSigQ:\t1/63451\nSigPnd:\t0000000000000100\nShdPnd:\t0000000000000100\n", pid, pid))
}

// guardClock hands each wait the guard asks for to the test, which fires it: a reading is then a
// step of the test.
type guardClock struct {
	mu    sync.Mutex
	now   time.Time
	waits chan guardWait
}

type guardWait struct {
	d    time.Duration
	fire chan time.Time
}

func newGuardClock() *guardClock {
	return &guardClock{now: time.Date(2026, 11, 14, 3, 0, 0, 0, time.UTC), waits: make(chan guardWait, 16)}
}

func (c *guardClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *guardClock) After(d time.Duration) <-chan time.Time {
	w := guardWait{d: d, fire: make(chan time.Time, 1)}
	c.waits <- w
	return w.fire
}

func (c *guardClock) advance(d time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	return c.now
}

// guardSignal is a kill that ends nothing of the machine's: it records each process the guard
// killed, and the process leaves the fake /proc at once, as a killed process does. It runs on the
// guard's loop, so it fails nothing itself: a process it cannot take out of the fake /proc is a
// kill that failed, which the guard logs. afterKill, where a test sets it before the guard runs,
// changes the pod after each kill, before the guard reads anything after it.
type guardSignal struct {
	proc      string
	mu        sync.Mutex
	killed    []int
	afterKill func(pid int)
}

type guardHandle struct {
	s      *guardSignal
	pid    int
	exited bool
}

func (s *guardSignal) Open(pid int) (memoryguard.Handle, error) {
	return &guardHandle{s: s, pid: pid}, nil
}

func (s *guardSignal) kills() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.killed)
}

func (h *guardHandle) Kill() error {
	h.s.mu.Lock()
	defer h.s.mu.Unlock()
	if err := os.RemoveAll(filepath.Join(h.s.proc, strconv.Itoa(h.pid))); err != nil {
		return err
	}
	h.s.killed = append(h.s.killed, h.pid)
	h.exited = true
	if h.s.afterKill != nil {
		h.s.afterKill(h.pid)
	}
	return nil
}

func (h *guardHandle) Exited() bool {
	h.s.mu.Lock()
	defer h.s.mu.Unlock()
	return h.exited
}

func (h *guardHandle) Close() error { return nil }

// guardRig is the guard or the guest watch a launcher builds, pointed at a guardPod with a
// guardClock and, for the guard, a guardSignal, one reading or scan at a time. classified counts,
// for each pid, the scans the guard asked its classifier about that read that pid.
type guardRig struct {
	t          *testing.T
	pod        *guardPod
	clock      *guardClock
	signal     *guardSignal
	mu         sync.Mutex
	guard      *memoryguard.Guard
	guest      *memoryguard.Guest
	lines      []string
	classified map[int]int
	pending    guardWait
}

// useGuardPod has every launcher this test starts run a memory guard, as the container's first
// process does, and has every guard it builds from here to the test's end read pod, wait on the
// rig's clock and kill with its signal, with the launcher's own classifier and sink. roles names
// the role of each process the test gives one, over the classifier's answer: a launcher started
// with no role's generation has no role-init the classifier could find a role by, and what the
// classifier answers on its own is TestMemoryGuardGivesEachProcessItsRoleByAncestorThenByNamespace's.
//
// A launcher taken for the container's first process builds the pod's cache trimmer too
// (serveVolumeTrim), over VolumeRoot and the machine's /proc, and an answer carrying trimmer
// settings would set it sweeping the volume this test runs on, which in an issue pod is the
// roles' own. So the trimmer's loop is held to the launcher's context and runs nothing, unless
// the case puts a fixture into it (useLauncherTrimmer, which calls this first and sets the loop
// after it). Measured at e8356e5d in an issue pod without this: a guard case answered with
// trimmer settings logged "volume trim: started: … volume /legion" and read the pod's volume
// once a second.
func useGuardPod(t *testing.T, pod *guardPod, roles map[int]string) *guardRig {
	t.Helper()
	return useGuardPodKilling(t, pod, roles, &guardSignal{proc: pod.proc})
}

// useGuardPodKilling is useGuardPod whose guards end a process with signal, or with their own
// pidfd kill where signal is nil: a pod whose /proc leads to real processes
// (TestControllerPodMemoryGuardEndsEachKindOfCommand) has them ended for real.
func useGuardPodKilling(t *testing.T, pod *guardPod, roles map[int]string, signal *guardSignal) *guardRig {
	t.Helper()
	useContainerInit(t, true)
	previousTrimmer := podTrimmer
	podTrimmer = func(ctx context.Context, _ *trimmer) { <-ctx.Done() }
	t.Cleanup(func() { podTrimmer = previousTrimmer })
	rig := &guardRig{t: t, pod: pod, clock: newGuardClock(), signal: signal, classified: map[int]int{}}
	previous := newMemoryGuard
	newMemoryGuard = func(cfg memoryguard.Config) *memoryguard.Guard {
		cfg.Proc, cfg.Cgroup, cfg.Clock = pod.proc, pod.cgroup, rig.clock
		if signal != nil {
			cfg.Signal = signal
		}
		cfg.Classify, cfg.Logf = rig.counting(withRoles(cfg.Classify, roles)), rig.keep(cfg.Logf)
		guard := memoryguard.New(cfg)
		rig.mu.Lock()
		rig.guard = guard
		rig.mu.Unlock()
		return guard
	}
	t.Cleanup(func() { newMemoryGuard = previous })
	return rig
}

// classifiedWith is how many of the scans the guard asked its classifier about read pid.
func (r *guardRig) classifiedWith(pid int) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.classified[pid]
}

// useGuestPod is useGuardPod for a microVM pod's launcher: every guest watch a launcher builds from
// here to the test's end reads pod and its cgroup directory and waits on the rig's clock, with the
// launcher's own classifier and sink, roles named over the classifier's answer. The launcher is
// taken for the container's first process here too, so the pod's cache trimmer it builds is held
// to the launcher's context and runs nothing, for useGuardPod's reason.
func useGuestPod(t *testing.T, pod *guardPod, roles map[int]string) *guardRig {
	t.Helper()
	useContainerInit(t, true)
	previousTrimmer := podTrimmer
	podTrimmer = func(ctx context.Context, _ *trimmer) { <-ctx.Done() }
	t.Cleanup(func() { podTrimmer = previousTrimmer })
	rig := &guardRig{t: t, pod: pod, clock: newGuardClock()}
	previous := newGuest
	newGuest = func(cfg memoryguard.GuestConfig) *memoryguard.Guest {
		cfg.Proc, cfg.Cgroup, cfg.Clock = pod.proc, pod.cgroup, rig.clock
		cfg.Classify, cfg.Logf = withRoles(cfg.Classify, roles), rig.keep(cfg.Logf)
		guest := memoryguard.NewGuest(cfg)
		rig.mu.Lock()
		rig.guest = guest
		rig.mu.Unlock()
		return guest
	}
	t.Cleanup(func() { newGuest = previous })
	return rig
}

// withRoles is classify with the role roles names for a pid put over its answer for that pid.
func withRoles(classify memoryguard.Classifier, roles map[int]string) memoryguard.Classifier {
	if len(roles) == 0 {
		return classify
	}
	return func(table *memoryguard.Table) map[int]memoryguard.Class {
		classes := classify(table)
		for pid, role := range roles {
			class := classes[pid]
			class.Role = role
			classes[pid] = class
		}
		return classes
	}
}

// counting is classify, which also counts, for each pid of each scan it is asked about, that the
// guard asked about it (classifiedWith), or nil for none.
func (r *guardRig) counting(classify memoryguard.Classifier) memoryguard.Classifier {
	if classify == nil {
		return nil
	}
	return func(table *memoryguard.Table) map[int]memoryguard.Class {
		classes := classify(table)
		r.mu.Lock()
		defer r.mu.Unlock()
		for _, p := range table.Processes() {
			r.classified[p.PID]++
		}
		return classes
	}
}

// keep is logf, which also keeps each line in the rig's log, or nil for none.
func (r *guardRig) keep(logf func(format string, args ...any)) func(format string, args ...any) {
	if logf == nil {
		return nil
	}
	return func(format string, args ...any) {
		logf(format, args...)
		r.mu.Lock()
		defer r.mu.Unlock()
		r.lines = append(r.lines, fmt.Sprintf(format, args...))
	}
}

// id is the id of the guard or the guest watch the launcher built.
func (r *guardRig) id() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case r.guard != nil:
		return r.guard.ID()
	case r.guest != nil:
		return r.guest.ID()
	}
	r.t.Fatal("the launcher built no memory guard and no guest watch")
	return ""
}

// log is every line the guard has logged.
func (r *guardRig) log() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.lines, "\n")
}

// started returns once the guard's first reading, and all it led to, is done.
func (r *guardRig) started() {
	r.t.Helper()
	r.pending = r.nextWait()
}

func (r *guardRig) nextWait() guardWait {
	r.t.Helper()
	select {
	case w := <-r.clock.waits:
		return w
	case <-time.After(testwait.Remaining(r.t)):
		r.t.Fatalf("the guard never waited for its next reading; its log:\n%s", r.log())
	}
	return guardWait{}
}

// tick lets the guard take its next reading, and returns once that reading and all it led to,
// a kill and its report to the role's manager among them, are done.
func (r *guardRig) tick() {
	r.t.Helper()
	r.tickAfter(0)
}

// tickAfter is tick with extra on the clock beyond the wait the guard asked for: a wait after a
// kill that passes, a stop a minute after the last, an hour's end.
func (r *guardRig) tickAfter(extra time.Duration) {
	r.t.Helper()
	w := r.pending
	w.fire <- r.clock.advance(w.d + extra)
	r.pending = r.nextWait()
}

// kill raises the reading to the mark, which ends pid when pid is the largest process the guard
// may end, and lowers it again, which ends that rise.
func (r *guardRig) kill(pid int) {
	r.t.Helper()
	r.pod.setAnon(guardHigh)
	r.tick()
	if kills := r.signal.kills(); len(kills) == 0 || kills[len(kills)-1] != pid {
		r.t.Fatalf("the guard killed %v, want %d last; its log:\n%s", kills, pid, r.log())
	}
	r.pod.setAnon(guardLow)
	r.tick()
}

// launcherExe is this test process's executable as the classifier reads its own: the launcher's,
// which a role's role-init and shim run.
func launcherExe(t *testing.T) string {
	t.Helper()
	exe, err := os.Readlink("/proc/self/exe")
	if err != nil {
		t.Skipf("no /proc/self/exe to read the launcher's executable from: %v", err)
	}
	return exe
}

// scanTable is one scan of pod, as memoryguard reads it and hands it to a classifier: the guard's
// first reading starts the hour's scan for its largest process, which asks the classifier.
func scanTable(t *testing.T, pod *guardPod) *memoryguard.Table {
	t.Helper()
	tables := make(chan *memoryguard.Table, 1)
	guard := memoryguard.New(memoryguard.Config{
		Proc: pod.proc, Cgroup: pod.cgroup, Clock: newGuardClock(), Signal: &guardSignal{proc: pod.proc},
		Classify: func(table *memoryguard.Table) map[int]memoryguard.Class {
			select {
			case tables <- table:
			default:
			}
			return nil
		},
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
	select {
	case table := <-tables:
		return table
	case <-time.After(testwait.Remaining(t)):
		t.Fatal("the guard never scanned the fake pod")
	}
	return nil
}

// The pids of podTree.
const (
	treeHeldScript = 50 + iota
	treeHeldChild
	treePostmaster
	treeBackend
	treeOtherPostgres
	treeExecShell
	treeExecChild
)

const (
	treeTesterInit = 100 + iota
	treeTesterShim
	treeTesterOmp
	treeTesterRenewer
	treeTesterHelper
	treeTesterEval
	treeTesterKernel
	treeTesterCommand
	treeTesterTest
	treeTesterFakeOmp
	treeTesterFakeShim
	treeTesterOrphanShim
	treeTesterSandbox
	treeTesterSandboxChild
	treeTesterReparented
	treeTesterPostgres
)

const (
	treeReviewerInit = 200 + iota
	treeReviewerShim
	treeReviewerBridge
	treeReviewerClaude
	treeReviewerMCP
	treeReviewerCommand
	treeReviewerFakeMCP
	treeReviewerCrossed
)

// podTree is an issue pod's processes in a fake /proc: the launcher, which is the test process,
// with the test database's script it holds and the server that script left running, and two
// roles' generations, an Oh My Pi role (tester) and a Claude Code one (reviewer), each with the
// agent's own processes and the commands the agent started. The classifier is the one the
// launcher builds for those roles, whose role-inits its managers hold, and a third role with no
// generation running.
func podTree(t *testing.T) (*guardPod, *podClassifier) {
	t.Helper()
	exe := launcherExe(t)
	launcher := os.Getpid()
	rolesDir := t.TempDir()
	data := filepath.Join(rolesDir, TestPGDirName, "data")
	const postgres = "/usr/lib/postgresql/17/bin/postgres"
	const omp = "/usr/local/bin/omp"
	pod := newGuardPod(t)
	pod.addLauncher(exe, 0)
	pod.add(
		guardProcess{pid: treeHeldScript, ppid: launcher, exe: "/usr/bin/dash", args: []string{"/bin/sh", "/usr/local/bin/legion-testpg", "ensure"}},
		guardProcess{pid: treeHeldChild, ppid: treeHeldScript, exe: "/usr/lib/postgresql/17/bin/pg_ctl", args: []string{"pg_ctl", "start"}},
		guardProcess{pid: treePostmaster, ppid: launcher, exe: postgres, args: []string{postgres, "-D", data}, cwd: data, anon: 300 * guardMiB},
		guardProcess{pid: treeBackend, ppid: treePostmaster, exe: postgres, args: []string{"postgres: legion_test legion_test 127.0.0.1(40000) idle"}, cwd: data, anon: 200 * guardMiB},
		// A postgres of the launcher's that runs elsewhere is no test database of the pod's.
		guardProcess{pid: treeOtherPostgres, ppid: launcher, exe: postgres, args: []string{postgres, "-D", "/tmp/other"}, cwd: "/tmp/other"},
		// A `kubectl exec` into the container, and what it ran: no role's.
		guardProcess{pid: treeExecShell, ppid: launcher, exe: "/usr/bin/bash", args: []string{"bash"}},
		guardProcess{pid: treeExecChild, ppid: treeExecShell, exe: "/usr/bin/python3.13", args: []string{"python3", "big.py"}},

		guardProcess{pid: treeTesterInit, ppid: launcher, exe: exe, args: []string{"legion", "launcher", "role-init", "--role", "tester"}, ns: testerNamespace},
		guardProcess{pid: treeTesterShim, ppid: treeTesterInit, exe: exe, args: []string{"legion", "worker-shim", "--connect", "tcp://daemon"}, ns: testerNamespace},
		guardProcess{pid: treeTesterOmp, ppid: treeTesterShim, exe: omp, args: []string{"omp", "--mode", "rpc"}, ns: testerNamespace, anon: 2 * guardGiB},
		guardProcess{pid: treeTesterRenewer, ppid: treeTesterShim, exe: "/usr/local/bin/agent-secrets", args: []string{"agent-secrets", "renew"}, ns: testerNamespace},
		guardProcess{pid: treeTesterHelper, ppid: treeTesterOmp, exe: omp, args: []string{"omp", "__omp_worker_broker"}, ns: testerNamespace},
		guardProcess{pid: treeTesterEval, ppid: treeTesterOmp, exe: omp, args: []string{"omp", ompEvalKernel}, ns: testerNamespace},
		guardProcess{pid: treeTesterKernel, ppid: treeTesterOmp, exe: "/usr/bin/python3.13", args: []string{"python3", "-m", "ipykernel_launcher"}, ns: testerNamespace},
		guardProcess{pid: treeTesterCommand, ppid: treeTesterOmp, exe: "/usr/bin/bash", args: []string{"bash", "-c", "go test ./..."}, ns: testerNamespace},
		guardProcess{pid: treeTesterTest, ppid: treeTesterCommand, exe: "/tmp/go-build1/b001/launcher.test", args: []string{"/tmp/go-build1/b001/launcher.test", "-test.v"}, ns: testerNamespace},
		// Programs that copy the names of the agent's own, at places in the tree theirs are not.
		guardProcess{pid: treeTesterFakeOmp, ppid: treeTesterCommand, exe: omp, args: []string{"omp", "__omp_worker_broker"}, ns: testerNamespace},
		guardProcess{pid: treeTesterFakeShim, ppid: treeTesterCommand, exe: exe, args: []string{"legion", "worker-shim"}, ns: testerNamespace},
		// An orphan the role-init adopted, younger than the shim, that calls itself one.
		guardProcess{pid: treeTesterOrphanShim, ppid: treeTesterInit, exe: exe, args: []string{"legion", "worker-shim"}, ns: testerNamespace},
		// A sandboxed browser makes a PID namespace of its own inside the role.
		guardProcess{pid: treeTesterSandbox, ppid: treeTesterCommand, exe: "/opt/chrome/chrome", args: []string{"chrome", "--type=zygote"}, ns: sandboxNamespace},
		guardProcess{pid: treeTesterSandboxChild, ppid: treeTesterSandbox, exe: "/opt/chrome/chrome", args: []string{"chrome", "--type=renderer"}, ns: sandboxNamespace},
		// A process of the role's namespace whose parent chain no longer reaches its role-init.
		guardProcess{pid: treeTesterReparented, ppid: launcher, exe: "/usr/bin/sleep", args: []string{"sleep", "600"}, ns: testerNamespace},
		// A postgres the agent started in the test database's directory is the agent's command.
		guardProcess{pid: treeTesterPostgres, ppid: treeTesterCommand, exe: postgres, args: []string{postgres, "-D", data}, cwd: data, ns: testerNamespace},

		guardProcess{pid: treeReviewerInit, ppid: launcher, exe: exe, args: []string{"legion", "launcher", "role-init", "--role", "reviewer"}, ns: reviewerNamespace},
		guardProcess{pid: treeReviewerShim, ppid: treeReviewerInit, exe: exe, args: []string{"legion", "worker-shim", "--connect", "tcp://daemon"}, ns: reviewerNamespace},
		guardProcess{pid: treeReviewerBridge, ppid: treeReviewerShim, exe: exe, args: []string{"legion", "claude-bridge", "--", "/usr/local/bin/claude", "-p"}, ns: reviewerNamespace},
		guardProcess{pid: treeReviewerClaude, ppid: treeReviewerBridge, exe: "/usr/local/bin/claude", args: []string{"/usr/local/bin/claude", "-p", "--input-format", "stream-json"}, ns: reviewerNamespace, anon: guardGiB},
		guardProcess{pid: treeReviewerMCP, ppid: treeReviewerClaude, exe: "/usr/local/bin/bun", args: []string{"/usr/local/bin/bun", "/opt/legion/claude-envoy/dist/envoy-channel.js"}, ns: reviewerNamespace},
		guardProcess{pid: treeReviewerCommand, ppid: treeReviewerClaude, exe: "/usr/bin/bash", args: []string{"bash", "-c", "npm test"}, ns: reviewerNamespace},
		guardProcess{pid: treeReviewerFakeMCP, ppid: treeReviewerCommand, exe: "/usr/local/bin/bun", args: []string{"bun", "/work/claude-envoy/dist/envoy-channel.js"}, ns: reviewerNamespace},
		// A process under the reviewer's role-init that names the tester's namespace: the two rules
		// disagree only in a table no kernel writes, and the ancestor is the one that decides.
		guardProcess{pid: treeReviewerCrossed, ppid: treeReviewerCommand, exe: "/usr/bin/sleep", args: []string{"sleep", "600"}, ns: testerNamespace},
	)
	managers := []*manager{
		{role: "tester", child: &child{pid: treeTesterInit}},
		{role: "reviewer", child: &child{pid: treeReviewerInit}},
		{role: "planner"},
	}
	return pod, newPodClassifier(rolesDir, managers, &pidGuard{pids: map[int]bool{treeHeldScript: true}})
}

// The issue pod's classifier spares the launcher, the processes it holds and the test database
// with its backends; in each role the role-init, the shim, every direct child of the shim, Oh My
// Pi's helpers but its eval kernel, and a Claude Code role's claude and MCP server. It spares
// neither the commands the agents started, a Python kernel or a JavaScript eval kernel, nor a
// program that copies the name of an agent's own process, or a test database's, at a place in
// the tree that process does not hold. It marks as a session process, whose end ends its role's
// session, each role's role-init, its shim and every direct child of the shim: Oh My Pi, the
// Claude Code bridge, and the secrets renewer, which no place in the tree tells from the agent. It
// marks no other process: no Oh My Pi helper or eval kernel, no Python kernel, nothing under the
// bridge, no command or what a command runs, no orphan the role-init adopted, and none of the
// launcher's own processes or the test database's.
func TestMemoryGuardSparesTheLaunchersAndTheAgentsOwnProcessesByTheirPlaceInTheTree(t *testing.T) {
	t.Parallel()
	pod, classifier := podTree(t)
	classes := classifier.classify(scanTable(t, pod))
	for _, tc := range []struct {
		pid       int
		protected bool
		session   bool
		what      string
	}{
		{os.Getpid(), true, false, "the launcher"},
		{treeHeldScript, true, false, "the test database's script, which the launcher holds"},
		{treeHeldChild, true, false, "a child of the script the launcher holds"},
		{treePostmaster, true, false, "the test database's postmaster"},
		{treeBackend, true, false, "a backend of the test database"},
		{treeOtherPostgres, false, false, "a postgres of the launcher's outside the test database's directory"},
		{treeExecShell, false, false, "a shell a kubectl exec started"},
		{treeExecChild, false, false, "what that shell ran"},
		{treeTesterInit, true, true, "the Oh My Pi role's role-init"},
		{treeTesterShim, true, true, "the Oh My Pi role's shim"},
		{treeTesterOmp, true, true, "Oh My Pi, the shim's child"},
		{treeTesterRenewer, true, true, "the secrets renewer, the shim's child, which no place in the tree tells from the agent"},
		{treeTesterHelper, true, false, "an Oh My Pi helper"},
		{treeTesterEval, false, false, "Oh My Pi's JavaScript eval kernel"},
		{treeTesterKernel, false, false, "a Python kernel"},
		{treeTesterCommand, false, false, "a command Oh My Pi started"},
		{treeTesterTest, false, false, "what that command ran"},
		{treeTesterFakeOmp, false, false, "an omp helper's name under the agent's command"},
		{treeTesterFakeShim, false, false, "a worker-shim under the agent's command"},
		{treeTesterOrphanShim, false, false, "a worker-shim the role-init adopted after its shim"},
		{treeTesterSandbox, false, false, "a sandbox in a PID namespace of its own"},
		{treeTesterReparented, false, false, "a role's process reparented out of its role-init's subtree"},
		{treeTesterPostgres, false, false, "a postgres the agent started in the test database's directory"},
		{treeReviewerInit, true, true, "the Claude Code role's role-init"},
		{treeReviewerShim, true, true, "the Claude Code role's shim"},
		{treeReviewerBridge, true, true, "the Claude Code bridge, the shim's child"},
		{treeReviewerClaude, true, false, "claude, the bridge's child"},
		{treeReviewerMCP, true, false, "claude's MCP server"},
		{treeReviewerCommand, false, false, "a command claude started"},
		{treeReviewerFakeMCP, false, false, "an MCP server's arguments under claude's command"},
	} {
		if got := classes[tc.pid].Protected; got != tc.protected {
			t.Errorf("%s (pid %d): protected = %v, want %v", tc.what, tc.pid, got, tc.protected)
		}
		if got := classes[tc.pid].Session; got != tc.session {
			t.Errorf("%s (pid %d): session = %v, want %v", tc.what, tc.pid, got, tc.session)
		}
	}
}

// Oh My Pi's helpers start helpers of their own. In an INFRA issue pod on 2026-11-14 the agent's
// `__omp_worker_daemon_broker` was the parent of `__omp_worker_lsp_mux`, the same executable,
// and that helper was the parent of gopls; the head's classifier, run over that pod's real /proc,
// spared the broker and not the multiplexer. INFRA-3531's rule, which W1 takes, protects every Oh
// My Pi helper, the Oh My Pi executable started with an `__omp_worker_` selector other than its
// eval kernel, known by its place in the tree: a helper that a helper of the agent started is as
// much Oh My Pi's own as one the agent started. What a helper runs that is not Oh My Pi (gopls),
// the eval kernel wherever it runs, and a copy of a helper's name under the agent's command stay
// endable.
func TestMemoryGuardSparesAnOhMyPiHelperThatAnotherHelperStarted(t *testing.T) {
	t.Parallel()
	exe := launcherExe(t)
	launcher := os.Getpid()
	const omp = "/opt/omp/bin/omp"
	const roleInit, shim, agent, broker, mux, gopls, eval, command, fakeMux = 710, 711, 712, 713, 714, 715, 716, 717, 718
	pod := newGuardPod(t)
	pod.addLauncher(exe, 0)
	pod.add(
		guardProcess{pid: roleInit, ppid: launcher, exe: exe, args: []string{"legion", "launcher", "role-init", "--role", "tester"}, ns: testerNamespace},
		guardProcess{pid: shim, ppid: roleInit, exe: exe, args: []string{"legion", "worker-shim", "--connect", "tcp://daemon"}, ns: testerNamespace},
		guardProcess{pid: agent, ppid: shim, exe: omp, args: []string{omp, "--extension", "/opt/legion/pi-envoy"}, ns: testerNamespace, anon: 2 * guardGiB},
		guardProcess{pid: broker, ppid: agent, exe: omp, args: []string{omp, "__omp_worker_daemon_broker"}, ns: testerNamespace},
		guardProcess{pid: mux, ppid: broker, exe: omp, args: []string{omp, "__omp_worker_lsp_mux"}, ns: testerNamespace},
		guardProcess{pid: gopls, ppid: mux, exe: "/usr/local/bin/gopls", args: []string{"/usr/local/bin/gopls", "serve"}, ns: testerNamespace},
		guardProcess{pid: eval, ppid: broker, exe: omp, args: []string{omp, ompEvalKernel}, ns: testerNamespace},
		guardProcess{pid: command, ppid: agent, exe: "/usr/bin/bash", args: []string{"bash", "-c", "make"}, ns: testerNamespace},
		guardProcess{pid: fakeMux, ppid: command, exe: omp, args: []string{omp, "__omp_worker_lsp_mux"}, ns: testerNamespace},
	)
	classifier := newPodClassifier(t.TempDir(), []*manager{{role: "tester", child: &child{pid: roleInit}}}, &pidGuard{})
	classes := classifier.classify(scanTable(t, pod))
	for _, tc := range []struct {
		pid       int
		protected bool
		what      string
	}{
		{agent, true, "Oh My Pi, the shim's child"},
		{broker, true, "the broker helper the agent started"},
		{mux, true, "the LSP multiplexer helper the broker started"},
		{gopls, false, "the language server the multiplexer runs"},
		{eval, false, "an eval kernel a helper started"},
		{command, false, "a command the agent started"},
		{fakeMux, false, "a helper's name under the agent's command"},
	} {
		if got := classes[tc.pid].Protected; got != tc.protected {
			t.Errorf("%s (pid %d): protected = %v, want %v", tc.what, tc.pid, got, tc.protected)
		}
	}
}

// The shim starts the CodeGraph warm-up during its agent's session load, beside the agent, and its
// end ends no session, so the guard may end it and all it starts. In INFRA-2709's pod after its
// relaunch at 23:42Z on 2026-11-22 one Oh My Pi held 1.34 GB and the warm-up 523 MB, and the pod
// was killed twice with the guard able to end nothing: the classifier then spared every direct
// child of the shim. The warm-up is told by its command, the image's `codegraph` script under its
// shell before the script's exec and its bundled node on codegraph.js after it, never by an
// agent's argument that only names the CodeGraph plugin. It stays the role's, and a session
// process like every direct child of the shim, for the naming of a kill.
func TestMemoryGuardMayEndTheCodeGraphWarmUp(t *testing.T) {
	t.Parallel()
	exe := launcherExe(t)
	launcher := os.Getpid()
	const omp = "/opt/omp/bin/omp"
	const node = "/opt/codegraph/node"
	const script = "/opt/codegraph/bin/codegraph"
	const (
		testerInit, testerShim, testerAgent, testerHelper, warmUp, warmUpGit = 730, 731, 732, 733, 734, 735
		reviewerInit, reviewerShim, reviewerAgent, warmUpShell               = 740, 741, 742, 743
	)
	pod := newGuardPod(t)
	pod.addLauncher(exe, 0)
	pod.add(
		guardProcess{pid: testerInit, ppid: launcher, exe: exe, args: []string{"legion", "launcher", "role-init", "--role", "tester"}, ns: testerNamespace},
		guardProcess{pid: testerShim, ppid: testerInit, exe: exe, args: []string{"legion", "worker-shim", "--connect", "tcp://daemon"}, ns: testerNamespace},
		guardProcess{pid: testerAgent, ppid: testerShim, exe: omp, args: []string{omp, "--extension", "/opt/omp-plugins/node_modules/@bopstack/pi-codegraph"}, ns: testerNamespace, anon: 3 * guardGiB},
		guardProcess{pid: testerHelper, ppid: testerAgent, exe: omp, args: []string{omp, "__omp_worker_daemon_broker"}, ns: testerNamespace},
		guardProcess{pid: warmUp, ppid: testerShim, exe: node, args: []string{node, "--liftoff-only", "--disable-warning=ExperimentalWarning", "/opt/codegraph/lib/dist/bin/codegraph.js", "index"}, ns: testerNamespace, anon: 523 * guardMiB},
		guardProcess{pid: warmUpGit, ppid: warmUp, exe: "/usr/bin/git", args: []string{"git", "ls-files"}, ns: testerNamespace},
		guardProcess{pid: reviewerInit, ppid: launcher, exe: exe, args: []string{"legion", "launcher", "role-init", "--role", "reviewer"}, ns: reviewerNamespace},
		guardProcess{pid: reviewerShim, ppid: reviewerInit, exe: exe, args: []string{"legion", "worker-shim", "--connect", "tcp://daemon"}, ns: reviewerNamespace},
		guardProcess{pid: reviewerAgent, ppid: reviewerShim, exe: omp, args: []string{omp, "--mode", "rpc"}, ns: reviewerNamespace, anon: guardGiB},
		guardProcess{pid: warmUpShell, ppid: reviewerShim, exe: "/usr/bin/dash", args: []string{"/bin/sh", script, "init", "-i"}, ns: reviewerNamespace},
	)
	managers := []*manager{{role: "tester", child: &child{pid: testerInit}}, {role: "reviewer", child: &child{pid: reviewerInit}}}
	classes := newPodClassifier(t.TempDir(), managers, &pidGuard{}).classify(scanTable(t, pod))
	for _, tc := range []struct {
		pid  int
		want memoryguard.Class
		what string
	}{
		{testerAgent, memoryguard.Class{Role: "tester", Protected: true, Session: true}, "Oh My Pi, whose arguments name the CodeGraph plugin"},
		{testerHelper, memoryguard.Class{Role: "tester", Protected: true}, "an Oh My Pi helper"},
		{warmUp, memoryguard.Class{Role: "tester", Session: true}, "the warm-up, node on codegraph.js"},
		{warmUpGit, memoryguard.Class{Role: "tester"}, "a process the warm-up started"},
		{reviewerAgent, memoryguard.Class{Role: "reviewer", Protected: true, Session: true}, "the reviewer's Oh My Pi"},
		{warmUpShell, memoryguard.Class{Role: "reviewer", Session: true}, "the warm-up, the codegraph script under its shell"},
	} {
		if got := classes[tc.pid]; got != tc.want {
			t.Errorf("%s (pid %d): %+v, want %+v", tc.what, tc.pid, got, tc.want)
		}
	}
}

// Each process is the role's whose role-init is its ancestor, then the role's whose role-init
// shares its PID namespace, and no role's otherwise: a process in a PID namespace of its own under
// a role-init is that role's by its ancestor, and one reparented out of the role-init's subtree is
// the role's by its namespace.
func TestMemoryGuardGivesEachProcessItsRoleByAncestorThenByNamespace(t *testing.T) {
	t.Parallel()
	pod, classifier := podTree(t)
	classes := classifier.classify(scanTable(t, pod))
	for _, tc := range []struct {
		pid  int
		role string
		what string
	}{
		{os.Getpid(), "", "the launcher"},
		{treeHeldScript, "", "the script the launcher holds"},
		{treePostmaster, "", "the test database"},
		{treeExecChild, "", "what a kubectl exec ran"},
		{treeTesterInit, "tester", "the tester's role-init"},
		{treeTesterOmp, "tester", "the tester's agent"},
		{treeTesterTest, "tester", "a command under the tester's agent"},
		{treeTesterOrphanShim, "tester", "an orphan the tester's role-init adopted"},
		{treeTesterSandbox, "tester", "a PID namespace of its own under the tester's role-init, by its ancestor"},
		{treeTesterSandboxChild, "tester", "a process in that namespace, by its ancestor"},
		{treeTesterReparented, "tester", "a process reparented out of the tester's subtree, by its namespace"},
		{treeReviewerInit, "reviewer", "the reviewer's role-init"},
		{treeReviewerMCP, "reviewer", "the reviewer's MCP server"},
		{treeReviewerFakeMCP, "reviewer", "a command under the reviewer's claude"},
		{treeReviewerCrossed, "reviewer", "a process whose ancestor and namespace disagree, by its ancestor"},
	} {
		if got := classes[tc.pid].Role; got != tc.role {
			t.Errorf("%s (pid %d): role = %q, want %q", tc.what, tc.pid, got, tc.role)
		}
	}
}

// The agent holding the most memory of the pod is spared, and the command it started is ended:
// the pod's guard ends the role's process its classifier lets it end, and gives the kill to that
// role's manager. Without the rule that spares the shim's children, the guard would end the agent.
func TestMemoryGuardSparesTheAgentAndEndsItsCommand(t *testing.T) {
	// Not parallel: it writes the package variable containerInit (useContainerInit), which every
	// launcher reads to tell whether it is the container's first process.
	exe := launcherExe(t)
	launcher := os.Getpid()
	const roleInit, shim, agent, command, test = 300, 301, 302, 303, 304
	pod := newGuardPod(t)
	pod.addLauncher(exe, 50*guardMiB)
	pod.add(
		guardProcess{pid: roleInit, ppid: launcher, exe: exe, args: []string{"legion", "launcher", "role-init"}, ns: testerNamespace},
		guardProcess{pid: shim, ppid: roleInit, exe: exe, args: []string{"legion", "worker-shim"}, ns: testerNamespace, anon: 30 * guardMiB},
		guardProcess{pid: agent, ppid: shim, exe: "/usr/local/bin/omp", args: []string{"omp", "--mode", "rpc"}, ns: testerNamespace, anon: 2 * guardGiB},
		guardProcess{pid: command, ppid: agent, exe: "/usr/bin/bash", args: []string{"bash", "-c", "go test ./..."}, ns: testerNamespace, anon: 8 * guardMiB},
		guardProcess{pid: test, ppid: command, exe: "/tmp/go-build1/b001/x.test", args: []string{"/tmp/go-build1/b001/x.test", "-test.v"}, ns: testerNamespace, anon: 600 * guardMiB},
	)
	pod.setAnon(guardHigh)
	rig := useGuardPod(t, pod, nil)
	log := &roleLog{stderr: &lockedBuffer{}}
	_, own := log.writers("tester")
	tester := &manager{role: "tester", log: log, stderr: own, child: &child{pid: roleInit}}
	ctx, cancel := context.WithCancel(context.Background())
	done := serveMemoryGuard(ctx, Config{RolesDir: t.TempDir()}, []*manager{tester}, &pidGuard{})
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(cleanupBeforeDeadline(t)):
			t.Error("the memory guard did not return after its context ended")
		}
	})
	rig.started()
	if kills := rig.signal.kills(); !slices.Equal(kills, []int{test}) {
		t.Fatalf("the guard killed %v, want the agent's command %d alone and never the agent %d; its log:\n%s", kills, test, agent, rig.log())
	}
	tester.mu.Lock()
	waiting := slices.Clone(tester.memoryKills)
	tester.mu.Unlock()
	if len(waiting) != 1 || waiting[0].PID != test || waiting[0].Sequence != 1 || waiting[0].Guard != rig.id() {
		t.Fatalf("the tester's manager holds %+v, want the one report of pid %d by guard %s", waiting, test, rig.id())
	}
}

// memoryGuardAck is an acknowledgement that carries memory guard settings.
func memoryGuardAck(act, floor, wait, interval int) shimwire.LauncherHelloAck {
	return shimwire.LauncherHelloAck{MemoryGuard: &shimwire.LauncherMemoryGuard{
		ActPercent: act, FloorPercent: floor, WaitSeconds: wait, IntervalMilliseconds: interval,
	}}
}

// defaultsAck is an acknowledgement that carries the settings an issue pod's guard starts with.
func defaultsAck() shimwire.LauncherHelloAck {
	return memoryGuardAck(shimwire.MemoryGuardActDefault, shimwire.MemoryGuardFloorDefault,
		shimwire.MemoryGuardWaitDefault, shimwire.MemoryGuardIntervalDefault)
}

// controllerDefaultsAck is an acknowledgement that carries the settings the controller pod's guard
// starts with, which the daemon sends the controller's pod by default: a wait of 1 second.
func controllerDefaultsAck() shimwire.LauncherHelloAck {
	return memoryGuardAck(shimwire.MemoryGuardActDefault, shimwire.MemoryGuardFloorDefault,
		shimwire.MemoryGuardControllerWaitDefault, shimwire.MemoryGuardIntervalDefault)
}

// guardLines are the guard's lines in a container's log that start with what: the launcher's own,
// behind no role's prefix.
func guardLines(log *lockedBuffer, what string) []string {
	var lines []string
	for _, line := range strings.Split(log.String(), "\n") {
		if strings.HasPrefix(line, "launcher: memory guard: "+what) {
			lines = append(lines, line)
		}
	}
	return lines
}

// Before any acknowledgement the pod's guard runs at the defaults; each role's acknowledgement
// hands it the daemon's settings through the wire's own encoder and decoder, and the newest one
// is what it runs by, changed in place: one line for each change and no second start line.
func TestMemoryGuardRunsAtTheDefaultsUntilAnAcknowledgementsSettings(t *testing.T) {
	// Not parallel: it writes the package variable roleHome (newFleetDialedOf), which parallel
	// tests read.
	const early, held = 400, 401
	pod := newGuardPod(t)
	pod.add(
		guardProcess{pid: early, ppid: 1, exe: "/usr/bin/python3.13", args: []string{"python3", "early.py"}, anon: 900 * guardMiB},
		guardProcess{pid: held, ppid: 1, exe: "/usr/bin/python3.13", args: []string{"python3", "held.py"}, anon: 800 * guardMiB},
	)
	rig := useGuardPod(t, pod, nil)
	f := newFleetDialedOf(t, Config{StopGrace: 2 * time.Second}, "tester", "reviewer")
	rig.started()
	defaults := memoryguard.Defaults()
	want := fmt.Sprintf("launcher: memory guard: started act_percent=%d floor_percent=%d wait=%s interval=%s limit=%d reading=%d",
		defaults.ActPercent, defaults.FloorPercent, defaults.Wait, defaults.Interval, guardLimit, guardLow)
	if got := guardLines(f.stderr, "started "); !slices.Equal(got, []string{want}) {
		t.Fatalf("the guard's start lines are %q, want %q", got, want)
	}
	// At the defaults the guard acts at 85% of the limit, with no acknowledgement yet.
	pod.setAnon(guardLimit / 100 * 86)
	rig.tick()
	if kills := rig.signal.kills(); !slices.Equal(kills, []int{early}) {
		t.Fatalf("before any acknowledgement the guard killed %v at 86%% of the limit, want %d by the defaults; its log:\n%s", kills, early, rig.log())
	}
	pod.setAnon(guardLow)
	rig.tick()

	f.role("tester").acknowledgeWith(memoryGuardAck(90, 20, 7, 250))
	first := "launcher: memory guard: settings act_percent=90 floor_percent=20 wait=7s interval=250ms"
	if got := guardLines(f.stderr, "settings "); !slices.Equal(got, []string{first}) {
		t.Fatalf("after the tester's acknowledgement the guard's settings lines are %q, want %q", got, first)
	}
	pod.setAnon(guardLimit / 100 * 88)
	rig.tick()
	if kills := rig.signal.kills(); !slices.Equal(kills, []int{early}) {
		t.Fatalf("at an act share of 90%% the guard killed %v at 88%% of the limit, want nothing more; its log:\n%s", kills, rig.log())
	}

	f.role("reviewer").acknowledgeWith(memoryGuardAck(60, 5, 3, 500))
	second := "launcher: memory guard: settings act_percent=60 floor_percent=5 wait=3s interval=500ms"
	if got := guardLines(f.stderr, "settings "); !slices.Equal(got, []string{first, second}) {
		t.Fatalf("after the reviewer's acknowledgement the guard's settings lines are %q, want %q", got, []string{first, second})
	}
	rig.tick()
	if kills := rig.signal.kills(); !slices.Equal(kills, []int{early, held}) {
		t.Fatalf("at an act share of 60%% the guard killed %v at 88%% of the limit, want %d next; its log:\n%s", kills, held, rig.log())
	}
	if got := guardLines(f.stderr, "started "); len(got) != 1 {
		t.Fatalf("the guard logged %q, want its one start line and no other", got)
	}
}

// A launcher that is not its container's first process runs no memory guard: it neither sees nor
// owns every process of the container, so the classifier's rules do not hold for what its /proc
// shows, and a guard there would end processes it knows nothing of. Run's roles branch logs one
// line saying so and builds no guard, and its managers hold none, so an acknowledgement's settings
// reach no guard and no report is queued.
func TestMemoryGuardRunsOnlyInTheContainersFirstProcess(t *testing.T) {
	// Not parallel: it writes the package variable containerInit (useContainerInit), which every
	// launcher reads to tell whether it is the container's first process.
	useContainerInit(t, false)
	previous := newMemoryGuard
	newMemoryGuard = func(memoryguard.Config) *memoryguard.Guard {
		panic("a launcher that is not the container's first process built a memory guard")
	}
	t.Cleanup(func() { newMemoryGuard = previous })
	f := newFleetDialedOf(t, Config{StopGrace: 2 * time.Second}, "tester", "reviewer")
	f.role("tester").acknowledgeWith(memoryGuardAck(90, 20, 7, 250))
	f.role("reviewer").acknowledgeWith(defaultsAck())
	want := fmt.Sprintf("launcher: memory guard: off reason=the launcher is pid %d, not the container's first process, so it neither sees nor owns every process of the container",
		os.Getpid())
	if got := guardLines(f.stderr, ""); !slices.Equal(got, []string{want}) {
		t.Fatalf("the launcher's memory guard lines are %q, want %q alone", got, want)
	}

	log := &roleLog{stderr: &lockedBuffer{}}
	managers := []*manager{{role: "tester", log: log}, {role: "reviewer", log: log}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	select {
	case <-serveMemoryGuard(ctx, Config{RolesDir: t.TempDir()}, managers, &pidGuard{}):
	default:
		t.Fatal("serveMemoryGuard left something running in a launcher that runs no guard")
	}
	for _, m := range managers {
		if m.memoryGuard != nil || m.memoryWake != nil {
			t.Fatalf("the %s's manager holds the guard %v and the wake %v, want neither", m.role, m.memoryGuard, m.memoryWake)
		}
	}
}

// A launcher that is not its container's first process runs neither the memory guard nor the
// clone's git lock sweep. Both decide by what the container's /proc shows, and such a launcher
// (one run by hand inside a role) sees only part of it: its guard would end processes it knows
// nothing of, and a pass of its sweep would remove a lock a process of another role holds open.
// Run's roles branch logs one off line for each and starts neither: it builds no guard, and the
// stale lock of the clone its working directory is, which a sweeping launcher's first pass
// removes before it serves any role (TestTheLauncherTakesTheWorkspaceFromItsWorkingDirectory),
// stays where it is. Its managers hold no sweep, so a role start learns nothing.
func TestMemoryGuardAndTheGitLockSweepRunOnlyInTheContainersFirstProcess(t *testing.T) {
	// Not parallel: it writes the package variable containerInit (useContainerInit), which every
	// launcher reads to tell whether it is the container's first process.
	useContainerInit(t, false)
	previous := newMemoryGuard
	newMemoryGuard = func(memoryguard.Config) *memoryguard.Guard {
		panic("a launcher that is not the container's first process built a memory guard")
	}
	t.Cleanup(func() { newMemoryGuard = previous })
	c := newLockClone(t)
	stale := c.lock("index.lock", "stale", time.Hour)
	t.Chdir(c.dir)
	f := newFleet(t, 2*time.Second, "tester", "reviewer")
	guardOff := fmt.Sprintf("launcher: memory guard: off reason=the launcher is pid %d, not the container's first process, so it neither sees nor owns every process of the container",
		os.Getpid())
	if got := guardLines(f.stderr, ""); !slices.Equal(got, []string{guardOff}) {
		t.Fatalf("the launcher's memory guard lines are %q, want %q alone", got, guardOff)
	}
	sweepOff := fmt.Sprintf("launcher: git lock sweep: off reason=the launcher is pid %d, not the container's first process, so it cannot see every process that may hold a lock",
		os.Getpid())
	// Every line of the sweep names a git lock: its off line, the workspace it works and each
	// lock it removes or keeps.
	var sweepLines []string
	for _, line := range strings.Split(f.stderr.String(), "\n") {
		if strings.Contains(line, "git lock") {
			sweepLines = append(sweepLines, line)
		}
	}
	if !slices.Equal(sweepLines, []string{sweepOff}) {
		t.Fatalf("the launcher's git lock sweep lines are %q, want %q alone", sweepLines, sweepOff)
	}
	if swept(stale) {
		t.Fatalf("a launcher that is not the container's first process removed %s; its log:\n%s", stale, f.stderr.String())
	}

	log := &roleLog{stderr: &lockedBuffer{}}
	managers := []*manager{{role: "tester", log: log}, {role: "reviewer", log: log}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	select {
	case <-serveGitLockSweep(ctx, managers):
	default:
		t.Fatal("serveGitLockSweep left something running in a launcher that runs no sweep")
	}
	for _, m := range managers {
		if m.gitLocks != nil {
			t.Fatalf("the %s's manager holds the sweep %v, want none", m.role, m.gitLocks)
		}
	}
}

// nextMemoryKill is the next report the launcher sends on g, past its state frames.
func (g *roleConn) nextMemoryKill() shimwire.LauncherMemoryKill {
	g.t.Helper()
	for {
		switch frame := g.next().(type) {
		case shimwire.LauncherState:
		case shimwire.LauncherMemoryKill:
			return frame
		default:
			g.t.Fatalf("the launcher sent %#v, want a launcher_memory_kill", frame)
		}
	}
}

// noMemoryKill fails if the launcher sends a report on g before its answer to a stop sent now:
// the connection's frames are in order, so a report already given to the role's manager is
// written before that answer, or not at all.
func (g *roleConn) noMemoryKill(what string) {
	g.t.Helper()
	g.send(shimwire.LauncherStop{ID: "no-report-" + strconv.FormatInt(time.Now().UnixNano(), 10), Generation: 1, GraceMs: 1000})
	for {
		switch frame := g.next().(type) {
		case shimwire.LauncherState:
		case shimwire.LauncherStopResult:
			return
		case shimwire.LauncherMemoryKill:
			g.t.Fatalf("%s: the launcher sent the report %+v", what, frame)
		default:
			g.t.Fatalf("%s: the launcher sent %#v, want the stop's answer", what, frame)
		}
	}
}

// A kill of a role's process is reported on that role's connection, once, with what the guard
// saw and the process named by its executable's base name alone; another role's connection
// carries none of it. A kill of a process in no role goes on neither connection, since neither
// acknowledgement says the daemon knows a report of no role (noRoleKills): the launcher drops its
// report with a line of its own.
func TestMemoryGuardReportsAKillOnTheConnectionOfTheProcesssRoleAlone(t *testing.T) {
	// Not parallel: it writes the package variable roleHome (newFleetDialedOf), which parallel
	// tests read.
	const none, testers, reviewers = 700, 701, 702
	pod := newGuardPod(t)
	pod.add(
		guardProcess{pid: none, ppid: 1, exe: "/usr/bin/python3.13", args: []string{"python3", "outside.py"}, anon: 900 * guardMiB},
		guardProcess{pid: testers, ppid: 1, exe: "/usr/bin/python3.13", args: []string{"python3", "grow.py", "--tester"}, anon: 800 * guardMiB},
		guardProcess{pid: reviewers, ppid: 1, exe: "/usr/bin/python3.13", args: []string{"python3", "grow.py", "--reviewer"}, anon: 700 * guardMiB},
	)
	rig := useGuardPod(t, pod, map[int]string{testers: "tester", reviewers: "reviewer"})
	f := newFleetDialedOf(t, Config{StopGrace: 2 * time.Second}, "tester", "reviewer")
	rig.started()
	for _, role := range []string{"tester", "reviewer"} {
		f.role(role).acknowledgeWith(defaultsAck())
	}
	// Largest first: the process of no role, then the tester's, then the reviewer's. A report of
	// one sent on the wrong connection would come before that connection's own, since each role
	// sends its reports in the order the guard made them.
	rig.kill(none)
	rig.kill(testers)
	rig.kill(reviewers)

	at := rig.clock.Now()
	report := f.role("tester").nextMemoryKill()
	want := shimwire.LauncherMemoryKill{
		Guard: rig.id(), Sequence: 2, PID: testers, Executable: "python3.13", SizeBytes: 800 * guardMiB,
		ReadingBytes: guardHigh, CounterBytes: guardCounter, LimitBytes: guardLimit,
	}
	if report.At.IsZero() || report.At.After(at) {
		t.Fatalf("the tester's report is at %s, want the guard's clock at its kill, no later than %s", report.At, at)
	}
	report.At = time.Time{}
	if report != want {
		t.Fatalf("the tester's report is %+v, want %+v", report, want)
	}
	if err := report.Validate(); err != nil {
		t.Fatal(err)
	}
	if report := f.role("reviewer").nextMemoryKill(); report.PID != reviewers || report.Sequence != 3 || report.Guard != rig.id() {
		t.Fatalf("the reviewer's report is %+v, want its own process %d, the guard's third kill", report, reviewers)
	}
	for _, role := range []string{"tester", "reviewer"} {
		f.role(role).noMemoryKill("after its own report the " + role + "'s connection")
	}
	line := noRoleDropped(1, none)
	testwait.Eventually(t, "the launcher's line "+line, func() bool {
		return slices.Contains(guardLines(f.stderr, "report dropped"), line)
	})
	if got := guardLines(f.stderr, "report dropped"); !slices.Equal(got, []string{line}) {
		t.Fatalf("the launcher's dropped lines are %q, want %q alone", got, line)
	}
}

// dropConnection ends role's connection from the daemon's end, and returns once the launcher has
// seen it go: it logs the reconnect only after the connection's end has taken it out of use.
func (f *fleet) dropConnection(role string) {
	f.t.Helper()
	ended := func() int { return strings.Count(f.stderr.String(), role+": launcher: daemon connection: ") }
	before := ended()
	_ = f.role(role).conn.Close()
	if !testwait.Holds(testwait.Remaining(f.t), testwait.Poll, func() bool { return ended() > before }) {
		f.t.Fatalf("the launcher never saw the %s's connection end; its log: %s", role, f.stderr.String())
	}
}

// A report made while the role's connection is down waits for the next one, and is sent on it
// after its state when its acknowledgement carries settings; a report waiting when an
// acknowledgement without settings arrives is dropped with a line, since the daemon behind it may
// not know the report and would close the connection on it.
func TestMemoryGuardHoldsAReportWhileTheConnectionIsDown(t *testing.T) {
	// Not parallel: it writes the package variable containerInit (useContainerInit), which every
	// launcher reads to tell whether it is the container's first process.
	const sent, dropped = 800, 801
	pod := newGuardPod(t)
	pod.add(
		guardProcess{pid: sent, ppid: 1, exe: "/usr/bin/python3.13", args: []string{"python3", "sent.py"}, anon: 900 * guardMiB},
		guardProcess{pid: dropped, ppid: 1, exe: "/usr/bin/python3.13", args: []string{"python3", "dropped.py"}, anon: 800 * guardMiB},
	)
	rig := useGuardPod(t, pod, map[int]string{sent: "tester", dropped: "tester"})
	f := newFleetDialedOf(t, Config{StopGrace: 2 * time.Second}, "tester")
	rig.started()
	f.role("tester").acknowledgeWith(defaultsAck())

	f.dropConnection("tester")
	rig.kill(sent)
	again := acceptRole(t, f.listener)
	f.conns["tester"] = again
	again.acknowledgeWith(defaultsAck())
	if report := again.nextMemoryKill(); report.PID != sent || report.Sequence != 1 || report.Guard != rig.id() {
		t.Fatalf("the next connection's report is %+v, want the one made while it was down, pid %d", report, sent)
	}
	again.noMemoryKill("after the report that waited for it, the next connection")

	f.dropConnection("tester")
	rig.kill(dropped)
	older := acceptRole(t, f.listener)
	f.conns["tester"] = older
	older.acknowledgeWith(shimwire.LauncherHelloAck{})
	older.noMemoryKill("a connection whose acknowledgement carries no settings")
	line := fmt.Sprintf("tester: launcher: memory guard: report dropped sequence=2 pid=%d reason=this connection's acknowledgement carries no memory guard settings", dropped)
	if !strings.Contains(f.stderr.String(), line) {
		t.Fatalf("the launcher logged %s, want the line %q", f.stderr.String(), line)
	}
}

// A role keeps at most 32 reports while its connection is down: the 33rd drops the oldest, with a
// line, and the next connection carries the 32 newest in the order the guard made them.
func TestMemoryGuardKeepsThirtyTwoWaitingReportsAndDropsTheOldest(t *testing.T) {
	// Not parallel: it writes the package variable containerInit (useContainerInit), which every
	// launcher reads to tell whether it is the container's first process.
	const first = 900
	kills := maxWaitingMemoryKills + 1
	roles := map[int]string{}
	for i := range kills {
		roles[first+i] = "tester"
	}
	pod := newGuardPod(t)
	rig := useGuardPod(t, pod, roles)
	f := newFleetDialedOf(t, Config{StopGrace: 2 * time.Second}, "tester")
	rig.started()
	f.role("tester").acknowledgeWith(defaultsAck())

	f.dropConnection("tester")
	for i := range kills {
		pid := first + i
		pod.add(guardProcess{pid: pid, ppid: 1, exe: "/usr/bin/python3.13", args: []string{"python3", "grow.py", strconv.Itoa(i)}, anon: 600 * guardMiB})
		rig.kill(pid)
	}
	line := fmt.Sprintf("tester: launcher: memory guard: report dropped sequence=1 pid=%d reason=%d reports already wait for a connection", first, maxWaitingMemoryKills)
	if !strings.Contains(f.stderr.String(), line) {
		t.Fatalf("the launcher logged %s, want the line %q", f.stderr.String(), line)
	}
	again := acceptRole(t, f.listener)
	f.conns["tester"] = again
	again.acknowledgeWith(defaultsAck())
	for sequence := uint64(2); sequence <= uint64(kills); sequence++ {
		if report := again.nextMemoryKill(); report.Sequence != sequence || report.PID != first+int(sequence)-1 {
			t.Fatalf("the next connection's report is %+v, want sequence %d of pid %d", report, sequence, first+int(sequence)-1)
		}
	}
	again.noMemoryKill("after the 32 reports that waited for it, the next connection")
}

// heldConn is a connection whose first write waits until release is closed, having closed writing
// to say it has begun; every write lands in out.
type heldConn struct {
	writing, release chan struct{}
	first            sync.Once
	out              lockedBuffer
}

func (c *heldConn) Write(p []byte) (int, error) {
	c.first.Do(func() {
		close(c.writing)
		<-c.release
	})
	return c.out.Write(p)
}

// While the oldest of a role's 32 waiting reports is being written, a 33rd drops the next oldest
// and never the one being written: that write goes on to succeed, so a line saying it was dropped
// would be false. The connection carries the oldest and then the 31 newest, in order.
func TestMemoryGuardDropsTheNextOldestWhileTheOldestIsBeingWritten(t *testing.T) {
	t.Parallel()
	const first = 900
	report := func(sequence int) shimwire.LauncherMemoryKill {
		return shimwire.LauncherMemoryKill{Guard: "0123456789abcdef", Sequence: uint64(sequence), PID: first + sequence - 1,
			Executable: "python3", SizeBytes: 600 * guardMiB, ReadingBytes: 3500 * guardMiB, LimitBytes: 4096 * guardMiB, At: time.Now()}
	}
	stderr := &lockedBuffer{}
	m := &manager{role: "tester", stderr: stderr}
	for sequence := 1; sequence <= maxWaitingMemoryKills; sequence++ {
		m.queueMemoryKill(report(sequence))
	}
	conn := &heldConn{writing: make(chan struct{}), release: make(chan struct{})}
	sent := make(chan struct{})
	go func() {
		defer close(sent)
		m.openMemoryReports(shimwire.NewWriter(conn), true, false)
	}()
	select {
	case <-conn.writing:
	case <-time.After(testwait.Remaining(t)):
		t.Fatal("the role's sender never began to write the oldest report")
	}
	m.queueMemoryKill(report(maxWaitingMemoryKills + 1))
	close(conn.release)
	select {
	case <-sent:
	case <-time.After(testwait.Remaining(t)):
		t.Fatal("the role's sender never finished writing the waiting reports")
	}

	want := fmt.Sprintf("launcher: memory guard: report dropped sequence=2 pid=%d reason=%d reports already wait for a connection, the most a role keeps",
		first+1, maxWaitingMemoryKills)
	if got := guardLines(stderr, "report dropped"); !slices.Equal(got, []string{want}) {
		t.Fatalf("the dropped lines are %q, want %q alone", got, want)
	}
	wantSequences := []uint64{1}
	for sequence := uint64(3); sequence <= maxWaitingMemoryKills+1; sequence++ {
		wantSequences = append(wantSequences, sequence)
	}
	var sequences []uint64
	for _, line := range strings.Split(strings.TrimSuffix(conn.out.String(), "\n"), "\n") {
		frame, err := shimwire.Decode([]byte(line))
		if err != nil {
			t.Fatalf("the connection carries %q: %v", line, err)
		}
		kill, ok := frame.(shimwire.LauncherMemoryKill)
		if !ok {
			t.Fatalf("the connection carries %#v, want only reports", frame)
		}
		sequences = append(sequences, kill.Sequence)
	}
	if !slices.Equal(sequences, wantSequences) {
		t.Fatalf("the connection carries the reports %v, want %v", sequences, wantSequences)
	}
	m.mu.Lock()
	left := len(m.memoryKills)
	m.mu.Unlock()
	if left != 0 {
		t.Fatalf("%d reports still wait after every write succeeded", left)
	}
}

// noRoleKillsAck is ack with noRoleKills set too, as a daemon that knows a report of a process of
// no role answers an issue pod's launcher.
func noRoleKillsAck(ack shimwire.LauncherHelloAck) shimwire.LauncherHelloAck {
	ack.NoRoleKills = true
	return ack
}

// noRoleDropped is the launcher's own line, behind no role's prefix, for a report of a process of
// no role it drops because no connection up can carry it.
func noRoleDropped(sequence uint64, pid int) string {
	return fmt.Sprintf("launcher: memory guard: report dropped sequence=%d pid=%d role=none "+
		"reason=no connection's acknowledgement says the daemon behind it knows a report of a process of no role (noRoleKills)", sequence, pid)
}

// A kill of a process of no role is reported once, on the connection of the first role of the
// pod's role list whose acknowledgement carries noRoleKills, marked as of no role and with what
// the guard saw; the other role's connection carries none of it. A kill of a role's process still
// goes on that role's own connection alone, unmarked.
func TestMemoryGuardReportsAKillOfNoRoleOnceOnTheFirstConnectionThatKnowsIt(t *testing.T) {
	// Not parallel: it writes the package variable containerInit (useContainerInit), which every
	// launcher reads to tell whether it is the container's first process.
	const none, reviewers = 710, 711
	pod := newGuardPod(t)
	pod.add(
		guardProcess{pid: none, ppid: 1, exe: "/usr/bin/python3.13", args: []string{"python3", "outside.py"}, anon: 900 * guardMiB},
		guardProcess{pid: reviewers, ppid: 1, exe: "/usr/bin/python3.13", args: []string{"python3", "grow.py", "--reviewer"}, anon: 700 * guardMiB},
	)
	rig := useGuardPod(t, pod, map[int]string{reviewers: "reviewer"})
	f := newFleetDialedOf(t, Config{StopGrace: 2 * time.Second}, "tester", "reviewer")
	rig.started()
	for _, role := range []string{"tester", "reviewer"} {
		f.role(role).acknowledgeWith(noRoleKillsAck(defaultsAck()))
		// A connection answers a stop only once it serves commands, which is after its reports
		// opened on it: both connections can carry a report before the first kill.
		f.role(role).noMemoryKill("before any kill, the " + role + "'s connection")
	}
	rig.kill(none)
	rig.kill(reviewers)

	at := rig.clock.Now()
	report := f.role("tester").nextMemoryKill()
	want := shimwire.LauncherMemoryKill{
		Guard: rig.id(), Sequence: 1, PID: none, Executable: "python3.13", SizeBytes: 900 * guardMiB,
		ReadingBytes: guardHigh, CounterBytes: guardCounter, LimitBytes: guardLimit, NoRole: true,
	}
	if report.At.IsZero() || report.At.After(at) {
		t.Fatalf("the report of no role is at %s, want the guard's clock at its kill, no later than %s", report.At, at)
	}
	report.At = time.Time{}
	if report != want {
		t.Fatalf("the tester's connection carries %+v, want the report of no role %+v", report, want)
	}
	if err := report.Validate(); err != nil {
		t.Fatal(err)
	}
	if report := f.role("reviewer").nextMemoryKill(); report.PID != reviewers || report.Sequence != 2 || report.NoRole {
		t.Fatalf("the reviewer's connection carries %+v, want its own process %d, the guard's second kill, unmarked", report, reviewers)
	}
	for _, role := range []string{"tester", "reviewer"} {
		f.role(role).noMemoryKill("after the two reports, the " + role + "'s connection")
	}
	if got := guardLines(f.stderr, "report dropped"); len(got) != 0 {
		t.Fatalf("the launcher logged %q, want no report dropped", got)
	}
}

// A report of no role is never written on a connection whose acknowledgement lacks noRoleKills: a
// kill made while the only connection up is such a one is dropped with the launcher's own line.
// One made while no connection is up waits, and the next connection whose acknowledgement carries
// noRoleKills carries it.
func TestMemoryGuardDropsAKillOfNoRoleNoConnectionKnowsAndHoldsOneWhileNoneIsUp(t *testing.T) {
	// Not parallel: it writes the package variable containerInit (useContainerInit), which every
	// launcher reads to tell whether it is the container's first process.
	const dropped, held = 720, 721
	pod := newGuardPod(t)
	pod.add(
		guardProcess{pid: dropped, ppid: 1, exe: "/usr/bin/python3.13", args: []string{"python3", "dropped.py"}, anon: 900 * guardMiB},
		guardProcess{pid: held, ppid: 1, exe: "/usr/bin/python3.13", args: []string{"python3", "held.py"}, anon: 800 * guardMiB},
	)
	rig := useGuardPod(t, pod, nil)
	f := newFleetDialedOf(t, Config{StopGrace: 2 * time.Second}, "tester")
	rig.started()
	f.role("tester").acknowledgeWith(defaultsAck())
	f.role("tester").noMemoryKill("before any kill, the tester's connection")

	rig.kill(dropped)
	line := noRoleDropped(1, dropped)
	testwait.Eventually(t, "the launcher's line "+line, func() bool {
		return slices.Contains(guardLines(f.stderr, "report dropped"), line)
	})
	f.role("tester").noMemoryKill("a connection whose acknowledgement carries no noRoleKills")

	f.dropConnection("tester")
	rig.kill(held)
	again := acceptRole(t, f.listener)
	f.conns["tester"] = again
	again.acknowledgeWith(noRoleKillsAck(defaultsAck()))
	if report := again.nextMemoryKill(); report.PID != held || report.Sequence != 2 || report.Guard != rig.id() || !report.NoRole {
		t.Fatalf("the next connection carries %+v, want the report of no role made while none was up, pid %d", report, held)
	}
	again.noMemoryKill("after the report of no role that waited for it, the next connection")
	if got := guardLines(f.stderr, "report dropped"); !slices.Equal(got, []string{line}) {
		t.Fatalf("the launcher's dropped lines are %q, want %q alone", got, line)
	}
}

// The pod keeps at most 32 reports of no role while no connection can carry them: the 33rd drops
// the oldest, with the launcher's own line, and the first connection that can carries the 32
// newest, each marked as of no role, in the order the guard made them, and keeps none back.
func TestMemoryGuardKeepsThirtyTwoWaitingReportsOfNoRoleAndDropsTheOldest(t *testing.T) {
	t.Parallel()
	const first = 900
	stderr := &lockedBuffer{}
	m := &manager{role: "tester", stderr: stderr}
	reports := newNoRoleReports([]*manager{m}, func(format string, args ...any) { fmt.Fprintf(stderr, format+"\n", args...) })
	m.noRoleReports = reports
	for sequence := 1; sequence <= maxWaitingMemoryKills+1; sequence++ {
		reports.queue(shimwire.LauncherMemoryKill{Guard: "0123456789abcdef", Sequence: uint64(sequence), PID: first + sequence - 1,
			Executable: "python3", SizeBytes: 600 * guardMiB, ReadingBytes: 3500 * guardMiB, LimitBytes: 4096 * guardMiB, At: time.Now()})
	}
	want := fmt.Sprintf("launcher: memory guard: report dropped sequence=1 pid=%d role=none reason=%d reports of no role already wait for a connection, the most the pod keeps",
		first, maxWaitingMemoryKills)
	if got := guardLines(stderr, "report dropped"); !slices.Equal(got, []string{want}) {
		t.Fatalf("the dropped lines are %q, want %q alone", got, want)
	}

	var out lockedBuffer
	m.openMemoryReports(shimwire.NewWriter(&out), true, true)
	var sequences []uint64
	for _, line := range strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n") {
		frame, err := shimwire.Decode([]byte(line))
		if err != nil {
			t.Fatalf("the connection carries %q: %v", line, err)
		}
		kill, ok := frame.(shimwire.LauncherMemoryKill)
		if !ok || !kill.NoRole {
			t.Fatalf("the connection carries %#v, want only reports of no role", frame)
		}
		sequences = append(sequences, kill.Sequence)
	}
	var wantSequences []uint64
	for sequence := uint64(2); sequence <= maxWaitingMemoryKills+1; sequence++ {
		wantSequences = append(wantSequences, sequence)
	}
	if !slices.Equal(sequences, wantSequences) {
		t.Fatalf("the connection carries the reports %v, want %v", sequences, wantSequences)
	}
	reports.mu.Lock()
	left := len(reports.kills)
	reports.mu.Unlock()
	if left != 0 {
		t.Fatalf("%d reports of no role still wait after every write succeeded", left)
	}
}

// The pids of controllerTree, the controller pod's processes besides the launcher, which is the
// test process.
const (
	ctlHeld = 1500 + iota
	ctlHeldChild
	ctlShim
	ctlOmp
	ctlRenewer
	ctlKeygen
	ctlBroker
	ctlMux
	ctlEval
	ctlKernel
	ctlCommand
	ctlCommandHelper
	ctlOrphan
	ctlExec
	ctlOmpCopy
	ctlStrayShim
	ctlFakeHelper
)

// controllerOmp is the Oh My Pi executable of controllerTree's agent.
const controllerOmp = "/opt/omp/bin/omp"

// controllerEndable are the processes of controllerTree the guard may end: Oh My Pi's JavaScript
// eval kernel, its Python kernel and its shell command, an orphan under PID 1, a `kubectl exec`
// shell, and each impostor.
var controllerEndable = []int{ctlEval, ctlKernel, ctlCommand, ctlCommandHelper, ctlOrphan, ctlExec, ctlOmpCopy, ctlStrayShim, ctlFakeHelper}

// controllerTree is the controller pod's processes in a fake /proc, each with the anonymous memory
// sizes gives it, none where it names none. The launcher is the test process, which stands for PID
// 1 of the controller's container: the guard and the classifier know the launcher by their own
// pid. Its pid guard may hold a jj, which runs a git (ctlHeld). Its generation is the shim, its own
// child, which runs Oh My Pi, the secrets renewer and a keygen. Oh My Pi runs its daemon broker,
// which runs the LSP multiplexer, both of them helpers; its JavaScript eval kernel, the same
// executable; a Python kernel; and a shell command, under which Oh My Pi's executable runs with a
// helper's argument. Under PID 1 are an orphan a command left behind and a worker-shim of this
// binary that is not the manager's generation, and beside them a `kubectl exec` shell, whose
// parent is outside the container's PID namespace. Under Oh My Pi are an `omp` copied to a path of
// its own and another executable, each run with a helper's argument.
func controllerTree(t *testing.T, sizes map[int]uint64) *guardPod {
	t.Helper()
	exe := launcherExe(t)
	launcher := os.Getpid()
	pod := newGuardPod(t)
	pod.addLauncher(exe, sizes[launcher])
	for _, gp := range []guardProcess{
		{pid: ctlHeld, ppid: launcher, exe: "/usr/local/bin/jj", args: []string{"jj", "workspace", "update-stale"}},
		{pid: ctlHeldChild, ppid: ctlHeld, exe: "/usr/bin/git", args: []string{"git", "gc", "--auto"}},
		{pid: ctlShim, ppid: launcher, exe: exe, args: []string{"legion", shimArgument, "--connect", "tcp://daemon", "--", controllerOmp}},
		{pid: ctlOmp, ppid: ctlShim, exe: controllerOmp, args: []string{controllerOmp, "--mode", "rpc"}},
		{pid: ctlRenewer, ppid: ctlShim, exe: "/usr/local/bin/agent-secrets", args: []string{"agent-secrets", "renew"}},
		{pid: ctlKeygen, ppid: ctlShim, exe: "/usr/local/bin/agent-secrets", args: []string{"agent-secrets", "keygen"}},
		{pid: ctlBroker, ppid: ctlOmp, exe: controllerOmp, args: []string{controllerOmp, "__omp_worker_daemon_broker"}},
		{pid: ctlMux, ppid: ctlBroker, exe: controllerOmp, args: []string{controllerOmp, "__omp_worker_lsp_mux"}},
		{pid: ctlEval, ppid: ctlOmp, exe: controllerOmp, args: []string{controllerOmp, ompEvalKernel}},
		{pid: ctlKernel, ppid: ctlOmp, exe: "/usr/bin/python3.13", args: []string{"python3", "-u", "/tmp/omp-python-kernel.py"}},
		{pid: ctlCommand, ppid: ctlOmp, exe: "/usr/bin/bash", args: []string{"bash", "-c", "make -j8"}},
		{pid: ctlCommandHelper, ppid: ctlCommand, exe: controllerOmp, args: []string{controllerOmp, "__omp_worker_lsp_mux"}},
		{pid: ctlOrphan, ppid: launcher, exe: "/usr/bin/node", args: []string{"node", "dev-server.js"}},
		{pid: ctlExec, ppid: 0, exe: "/usr/bin/bash", args: []string{"bash"}},
		{pid: ctlOmpCopy, ppid: ctlOmp, exe: "/home/legion/.cache/omp", args: []string{"omp", "__omp_worker_daemon_broker"}},
		{pid: ctlStrayShim, ppid: launcher, exe: exe, args: []string{"legion", shimArgument, "--connect", "tcp://daemon"}},
		{pid: ctlFakeHelper, ppid: ctlOmp, exe: "/usr/local/bin/bun", args: []string{"bun", "__omp_worker_daemon_broker"}},
	} {
		gp.anon = sizes[gp.pid]
		pod.add(gp)
	}
	return pod
}

// controllerManager is the controller pod's one role as newManagers builds it, with its generation,
// controllerTree's shim, running: it has no roles log, and its own lines, the guard's among them,
// go to the log it answers.
func controllerManager() (*manager, *lockedBuffer) {
	log := &lockedBuffer{}
	return &manager{role: "controller", stderr: log, child: &child{pid: ctlShim}}, log
}

// serveControllerGuard runs the controller pod's guard for m, with the pid guard pids, as Run's
// single-role branch does, until the test's end: serveControllerGuardOf a gVisor pod's zero Config.
func serveControllerGuard(t *testing.T, m *manager, pids *pidGuard) {
	t.Helper()
	serveControllerGuardOf(t, Config{}, m, pids)
}

// serveControllerGuardOf runs the controller pod's memory watch of cfg (serveMemoryGuard) for m,
// with the pid guard pids, as Run's single-role branch does, until the test's end.
func serveControllerGuardOf(t *testing.T, cfg Config, m *manager, pids *pidGuard) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := serveMemoryGuard(ctx, cfg, []*manager{m}, pids)
	t.Cleanup(func() {
		cancel()
		testwait.Receive(t, "the memory guard's return after its context ended", done)
	})
}

// waitingReports are the guard's reports m holds for a connection, oldest first.
func waitingReports(m *manager) []shimwire.LauncherMemoryKill {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.memoryKills)
}

// guestControllerTree is controllerTree, with no sizes, as a microVM controller pod's guest watch
// reads it: the container's starting score of 0, and every process of the tree at it.
func guestControllerTree(t *testing.T) *guardPod {
	t.Helper()
	pod := controllerTree(t, nil)
	pod.setSelfScore(0)
	pod.setScore(os.Getpid(), 0, 0)
	for pid := ctlHeld; pid <= ctlFakeHelper; pid++ {
		pod.setScore(pid, 0, 0)
	}
	return pod
}

// askedGuest has every guest watch a launcher builds from here to the test's end built as the
// test's newGuest already builds it (useGuestPod), and answers a function that answers the
// configuration the launcher asked for, its own classifier and SessionDrain, failing the test
// where the launcher built none, or a second.
func askedGuest(t *testing.T) func() memoryguard.GuestConfig {
	t.Helper()
	asked := make(chan memoryguard.GuestConfig, 1)
	built := newGuest
	newGuest = func(cfg memoryguard.GuestConfig) *memoryguard.Guest {
		select {
		case asked <- cfg:
		default:
			t.Error("the launcher built a second guest watch")
		}
		return built(cfg)
	}
	t.Cleanup(func() { newGuest = built })
	return func() memoryguard.GuestConfig {
		t.Helper()
		select {
		case cfg := <-asked:
			return cfg
		default:
			t.Fatal("the launcher built no guest watch")
			return memoryguard.GuestConfig{}
		}
	}
}

// runControllerLauncher runs the controller pod's launcher, Run's single-role form, for the role
// "controller" until the test's end, when Run must return the end of its context and nothing else.
// The test takes each of its connections itself (acceptController), since what it drives is the
// acknowledgement.
func runControllerLauncher(t *testing.T) *rig {
	t.Helper()
	return runControllerLauncherOf(t, Config{StopGrace: 2 * time.Second})
}

// runControllerLauncherOf is runControllerLauncher whose launcher carries cfg's fields besides the
// ones every controller launcher of these tests is given.
func runControllerLauncherOf(t *testing.T, cfg Config) *rig {
	t.Helper()
	token := ownToken("launcher-token")
	listener := listenForLauncher(t, token)
	ctx, cancel := context.WithCancel(context.Background())
	g := &rig{t: t, listener: listener, private: shortTempDir(t), cancel: cancel, result: make(chan error, 1), stderr: &lockedBuffer{}}
	cfg.Connect, cfg.Token, cfg.Sandbox = "tcp://"+listener.Addr().String(), token, "legion-legion-controller"
	cfg.Role, cfg.PodUID, cfg.PrivateDir, cfg.Stderr = "controller", "pod-1", g.private, g.stderr
	go func() {
		err := Run(ctx, cfg)
		listener.codeReturned(err)
		g.result <- err
	}()
	t.Cleanup(func() {
		cancel()
		if err := testwait.Receive(t, "the controller's launcher returning after its context ended", g.result); !errors.Is(err, context.Canceled) {
			t.Errorf("the controller's launcher returned %v, want its context's end", err)
		}
	})
	return g
}

// acceptController takes the controller launcher's next connection, admits its hello with ack,
// whatever memory guard settings it carries, and reads the launcher's first state on it. The
// daemon end takes only a hello under the rig's own token (listenForLauncher).
func (g *rig) acceptController(ack shimwire.LauncherHelloAck) *roleConn {
	g.t.Helper()
	conn := acceptRole(g.t, g.listener)
	if conn.hello.Role != "controller" {
		g.t.Fatalf("hello = %#v, want the controller's", conn.hello)
	}
	g.roleConn = conn
	conn.acknowledgeWith(ack)
	return conn
}

// dropConnection ends the launcher's connection from the daemon's end, and returns once the
// launcher has seen it go: it logs the reconnect only after the connection's end has taken it out
// of use.
func (g *rig) dropConnection() {
	g.t.Helper()
	ended := func() int { return strings.Count(g.stderr.String(), "launcher: daemon connection: ") }
	before := ended()
	_ = g.roleConn.conn.Close()
	if !testwait.Holds(beforeDeadline(g.t), testwait.Poll, func() bool { return ended() > before }) {
		g.t.Fatalf("the launcher never saw its connection end; its log: %s", g.stderr.String())
	}
}

// The controller pod's classifier spares the launcher, the jj its pid guard holds with the git
// under it, the shim, every direct child of the shim (Oh My Pi, the secrets renewer, its keygen)
// and Oh My Pi's two helpers, one started by the other. It spares neither the JavaScript eval
// kernel, the Python kernel or the shell command Oh My Pi started, nor an orphan under PID 1 or a
// `kubectl exec` shell, nor an impostor: a helper's argument under a copy of Oh My Pi, under
// another executable or under the shell command, and a worker-shim of this binary that is not the
// manager's generation. Every process is the controller's, spared or not. The shim and each of its
// direct children are its session's processes (memoryguard.Class.Session), as a role's are in an
// issue pod, and nothing else is. A pid the manager holds is the shim only while it names this
// binary run as worker-shim: with no generation, or with one whose pid now names another process,
// the launcher and what its pid guard holds alone are spared.
func TestControllerPodMemoryGuardSparesItsOwnProcessesByTheirPlaceInTheTree(t *testing.T) {
	t.Parallel()
	pod := controllerTree(t, nil)
	pids := &pidGuard{pids: map[int]bool{ctlHeld: true}}
	m, _ := controllerManager()
	table := scanTable(t, pod)
	classes := newControllerClassifier(m, pids).classify(table)
	for _, tc := range []struct {
		pid       int
		protected bool
		session   bool
		what      string
	}{
		{os.Getpid(), true, false, "the launcher"},
		{ctlHeld, true, false, "the jj the launcher's pid guard holds"},
		{ctlHeldChild, true, false, "the git under that jj"},
		{ctlShim, true, true, "the shim, the manager's generation"},
		{ctlOmp, true, true, "Oh My Pi, the shim's child"},
		{ctlRenewer, true, true, "the secrets renewer, the shim's child"},
		{ctlKeygen, true, true, "the secrets keygen, the shim's child"},
		{ctlBroker, true, false, "the daemon broker, a helper Oh My Pi started"},
		{ctlMux, true, false, "the LSP multiplexer, a helper the broker started"},
		{ctlEval, false, false, "Oh My Pi's JavaScript eval kernel"},
		{ctlKernel, false, false, "a Python kernel"},
		{ctlCommand, false, false, "a shell command Oh My Pi started"},
		{ctlCommandHelper, false, false, "Oh My Pi's executable with a helper's argument under the shell command"},
		{ctlOrphan, false, false, "an orphan under PID 1"},
		{ctlExec, false, false, "a kubectl exec shell"},
		{ctlOmpCopy, false, false, "a copy of omp with a helper's argument under Oh My Pi"},
		{ctlStrayShim, false, false, "a worker-shim under PID 1 that is not the manager's generation"},
		{ctlFakeHelper, false, false, "a helper's argument under another executable"},
	} {
		if got, want := classes[tc.pid], (memoryguard.Class{Role: "controller", Protected: tc.protected, Session: tc.session}); got != want {
			t.Errorf("%s (pid %d): %+v, want %+v", tc.what, tc.pid, got, want)
		}
	}
	if got, want := len(classes), len(table.Processes()); got != want {
		t.Errorf("the classifier gives %d processes a class, want each of the scan's %d the controller's", got, want)
	}

	launcherAndHeld := []int{os.Getpid(), ctlHeld, ctlHeldChild}
	slices.Sort(launcherAndHeld)
	for _, tc := range []struct {
		name       string
		generation *child
	}{
		{"no generation", nil},
		{"a generation whose pid names another process", &child{pid: ctlOrphan}},
	} {
		var spared []int
		for pid, class := range newControllerClassifier(&manager{role: "controller", child: tc.generation}, pids).classify(table) {
			if class.Protected {
				spared = append(spared, pid)
			}
		}
		slices.Sort(spared)
		if !slices.Equal(spared, launcherAndHeld) {
			t.Errorf("with %s the classifier spares %v, want the launcher and what its pid guard holds, %v", tc.name, spared, launcherAndHeld)
		}
	}
}

// The launcher, the shim, Oh My Pi, its two helpers, the secrets renewer and keygen and what the
// launcher's pid guard holds are never ended, though each is larger than anything the guard may
// end: the guard ends what it may, largest first, one rise at a time, the kernels, the shell
// command, the orphan under PID 1, the kubectl exec shell and each impostor, and with only spared
// processes left it ends nothing more and writes its stopped line. Every kill is the controller's:
// each is reported to its one manager, and each ended line reads role=controller.
func TestControllerPodMemoryGuardNeverEndsItsOwnProcessesEvenWhereOneIsTheLargest(t *testing.T) {
	// Not parallel: it writes the package variable containerInit (useContainerInit), which every
	// launcher reads to tell whether it is the container's first process.
	sizes := map[int]uint64{
		os.Getpid(): 2000 * guardMiB, ctlShim: 1950 * guardMiB, ctlOmp: 1900 * guardMiB, ctlRenewer: 1850 * guardMiB,
		ctlKeygen: 1800 * guardMiB, ctlBroker: 1750 * guardMiB, ctlMux: 1700 * guardMiB, ctlHeld: 1650 * guardMiB,
		ctlHeldChild: 1600 * guardMiB,
	}
	for i, pid := range controllerEndable {
		sizes[pid] = uint64(900-50*i) * guardMiB
	}
	pod := controllerTree(t, sizes)
	rig := useGuardPod(t, pod, nil)
	m, log := controllerManager()
	serveControllerGuard(t, m, &pidGuard{pids: map[int]bool{ctlHeld: true}})
	rig.started()
	for _, pid := range controllerEndable {
		rig.kill(pid)
	}
	pod.setAnon(guardHigh)
	rig.tick()
	if kills := rig.signal.kills(); !slices.Equal(kills, controllerEndable) {
		t.Fatalf("the guard killed %v, want %v and none of the controller's own; its log:\n%s", kills, controllerEndable, rig.log())
	}
	if stopped := guardLines(log, "stopped for this rise reason=nothing to end "); len(stopped) != 1 || !strings.Contains(stopped[0], " endable=0 ") {
		t.Fatalf("the stopped lines are %q, want one with nothing it may end; its log:\n%s", stopped, rig.log())
	}
	reports, ended := waitingReports(m), guardLines(log, "ended ")
	if len(reports) != len(controllerEndable) || len(ended) != len(controllerEndable) {
		t.Fatalf("the controller's manager holds %d reports and its log %d ended lines, want %d of each; its log:\n%s",
			len(reports), len(ended), len(controllerEndable), log.String())
	}
	for i, pid := range controllerEndable {
		if r := reports[i]; r.PID != pid || r.Sequence != uint64(i+1) || r.Guard != rig.id() {
			t.Errorf("report %d is %+v, want pid %d, sequence %d of guard %s", i, r, pid, i+1, rig.id())
		}
		if want := fmt.Sprintf("launcher: memory guard: ended pid=%d role=controller ", pid); !strings.HasPrefix(ended[i], want) {
			t.Errorf("ended line %d is %q, want it to start %q", i, ended[i], want)
		}
	}
}

// The guard's reading is AnonPages, and a process's size is its anonymous memory. A database file
// mapped shared raises the cgroup's counter past the mark while AnonPages stays low, and the guard
// ends nothing; at the mark it ends the Python kernel and not the reader that maps the file, whose
// resident pages are the file's; and at the next rise, where what is left to end is the reader's
// few anonymous pages, it ends nothing more. The kill's report carries the counter it read beside
// the reading it acted on.
func TestControllerPodMemoryGuardReadsAnonPagesAndNotAMappedFile(t *testing.T) {
	// Not parallel: it writes the package variable containerInit (useContainerInit), which every
	// launcher reads to tell whether it is the container's first process.
	const reader = 1550
	pod := controllerTree(t, map[int]uint64{ctlOmp: guardGiB, ctlKernel: 600 * guardMiB})
	pod.add(guardProcess{pid: reader, ppid: ctlCommand, exe: "/usr/bin/sqlite3", args: []string{"sqlite3", "/var/lib/legion/big.db"},
		anon: 10 * guardMiB, mapped: 3 * guardGiB})
	counter := 3900 * guardMiB
	pod.setCounter(counter)
	rig := useGuardPod(t, pod, nil)
	m, log := controllerManager()
	serveControllerGuard(t, m, &pidGuard{})
	rig.started()
	rig.tick()
	if kills := rig.signal.kills(); len(kills) != 0 {
		t.Fatalf("with the counter at %d and AnonPages at %d of a limit of %d the guard killed %v, want nothing; its log:\n%s",
			counter, guardLow, guardLimit, kills, rig.log())
	}
	pod.setAnon(guardHigh)
	rig.tick()
	if kills := rig.signal.kills(); !slices.Equal(kills, []int{ctlKernel}) {
		t.Fatalf("at the mark the guard killed %v, want the Python kernel %d and not the reader %d of a mapped file; its log:\n%s",
			kills, ctlKernel, reader, rig.log())
	}
	if reports := waitingReports(m); len(reports) != 1 || reports[0].ReadingBytes != guardHigh || reports[0].CounterBytes != counter {
		t.Fatalf("the controller's reports are %+v, want one at the reading %d with the counter %d", reports, guardHigh, counter)
	}
	pod.setAnon(guardLow)
	rig.tick()
	pod.setAnon(guardHigh)
	rig.tick()
	if kills := rig.signal.kills(); !slices.Equal(kills, []int{ctlKernel}) {
		t.Fatalf("at the next rise the guard killed %v, want nothing more than %d; its log:\n%s", kills, ctlKernel, rig.log())
	}
	want := fmt.Sprintf("launcher: memory guard: stopped for this rise reason=nothing to end reading=%d endable=%d ", guardHigh, 10*guardMiB)
	if stopped := guardLines(log, "stopped "); len(stopped) != 1 || !strings.HasPrefix(stopped[0], want) {
		t.Fatalf("the stopped lines are %q, want one starting %q", stopped, want)
	}
}

// Below the mark the guard ends nothing; at the mark, where what it may end holds at least the
// floor between them and none of it grows, it ends the largest; after a fall that stays at the
// mark it ends the next largest; a reading that has not fallen when the controller's wait of 1
// second has passed, with nothing it may end growing, stops it for the rise; and it acts on no new
// rise, however high, until the reading has been below the mark.
func TestControllerPodMemoryGuardEndsTheLargestAtTheMarkAndTheNextAfterAFall(t *testing.T) {
	// Not parallel: it writes the package variable containerInit (useContainerInit), which every
	// launcher reads to tell whether it is the container's first process.
	pod := controllerTree(t, map[int]uint64{ctlOmp: 2 * guardGiB, ctlEval: 900 * guardMiB, ctlKernel: 700 * guardMiB, ctlCommand: 500 * guardMiB})
	rig := useGuardPod(t, pod, nil)
	m, log := controllerManager()
	serveControllerGuard(t, m, &pidGuard{})
	rig.started()
	step := func(what string, reading uint64, extra time.Duration, want ...int) {
		t.Helper()
		pod.setAnon(reading)
		rig.tickAfter(extra)
		if kills := rig.signal.kills(); !slices.Equal(kills, want) {
			t.Fatalf("%s (reading %d MiB): the guard killed %v, want %v; its log:\n%s", what, reading>>20, kills, want, rig.log())
		}
	}
	step("below the mark", 3400*guardMiB, 0)
	step("at the mark", guardHigh, 0, ctlEval)
	step("after a fall that stays at the mark", 3550*guardMiB, 0, ctlEval, ctlKernel)
	step("while the wait lasts with no fall", 3550*guardMiB, 0, ctlEval, ctlKernel)
	step("once the wait has passed with no fall", 3550*guardMiB, memoryguard.ControllerDefaults().Wait, ctlEval, ctlKernel)
	if stopped := guardLines(log, "stopped for this rise reason=not fallen "); len(stopped) != 1 {
		t.Fatalf("the stopped lines for a reading that has not fallen are %q, want one; its log:\n%s", stopped, rig.log())
	}
	step("at a new high before the reading has been below the mark", 3800*guardMiB, 0, ctlEval, ctlKernel)
	step("at the mark before the reading has been below it", guardHigh, 0, ctlEval, ctlKernel)
	step("below the mark", guardLow, 0, ctlEval, ctlKernel)
	step("at the mark after the reading has been below it", guardHigh, 0, ctlEval, ctlKernel, ctlCommand)
}

// The controller's launcher in a microVM pod keeps the memory rule of a microVM pod rather than
// the gVisor guard (INFRA-4567). As the container's first process, the single-role form with
// MicroVM logs the one off line through the role's own log, builds no guard, so its manager holds
// none, and runs the guest watch over the controller's classifier (controllerClassifier): one scan
// raises what the guard may end of the controller's tree and keeps the launcher, what its pid
// guard holds, the shim and the agent's own processes at the container's starting score. The
// watch's SessionDrain is that form's own drain of an exited generation, its StopGrace, and a
// second (controllerSessionDrain), which through Run counts from Run's default of 30 seconds where
// the configuration names none; an acknowledgement's guard settings reach no guard there, so no
// settings line follows the off line. The kill reports of its one role go only on a connection
// whose acknowledgement carried kernelKills, whatever it says of the guard, as the daemon's answer
// to the controller does beside its guard settings; one of guard settings alone, an earlier
// daemon's answer, or of neither drops them with the line a microVM issue pod's launcher writes.
// Without MicroVM the controller's launcher runs the guard, as INFRA-3531 has it: the guard ends
// the largest at the mark, its manager holds the guard, the guest watch is never built, and its
// reports go where guard settings came and are dropped where kernelKills alone did.
func TestTheControllersLauncherInAMicroVMRunsTheGuestWatch(t *testing.T) {
	// Not parallel: it writes the package variable containerInit (useContainerInit), which every
	// launcher reads to tell whether it is the container's first process.
	const off = "launcher: memory guard: off reason=the pod runs in a microVM, whose own kernel stops one process"
	t.Run("in a microVM", func(t *testing.T) {
		pod := guestControllerTree(t)
		noGuard(t, "a microVM controller pod's launcher built a memory guard")
		rig := useGuestPod(t, pod, nil)
		asked := askedGuest(t)
		m, log := controllerManager()
		pids := &pidGuard{pids: map[int]bool{ctlHeld: true}}
		serveControllerGuardOf(t, Config{MicroVM: true, StopGrace: 5 * time.Second}, m, pids)
		rig.started()
		if got := guardLines(log, ""); !slices.Equal(got, []string{off}) {
			t.Fatalf("the controller's memory guard lines are %q, want %q alone", got, off)
		}
		if !strings.Contains(log.String(), "\nlauncher: guest memory: start ") {
			t.Fatalf("the controller's log holds no start line of the guest watch, behind no prefix: %s", log.String())
		}
		if m.memoryGuard != nil {
			t.Fatalf("the controller's manager holds the guard %v in a microVM pod's launcher, want none", m.memoryGuard)
		}
		cfg := asked()
		if want := 6 * time.Second; cfg.SessionDrain != want {
			t.Errorf("the guest watch's SessionDrain is %s, want the controller's StopGrace of 5s and a second, %s", cfg.SessionDrain, want)
		}
		table := scanTable(t, pod)
		if got, want := cfg.Classify(table), newControllerClassifier(m, pids).classify(table); !maps.Equal(got, want) {
			t.Errorf("the guest watch classifies the controller's tree as %v, want the controller's classifier's %v", got, want)
		}
		for _, pid := range []int{os.Getpid(), ctlHeld, ctlHeldChild, ctlShim, ctlOmp, ctlRenewer, ctlKeygen, ctlBroker, ctlMux} {
			if got := pod.score(pid); got != 0 {
				t.Errorf("pid %d, which the controller's classifier spares, reads oom_score_adj %d after one scan, want the starting 0; the watch's log:\n%s", pid, got, rig.log())
			}
		}
		for _, pid := range controllerEndable {
			if got := pod.score(pid); got != 1000 {
				t.Errorf("pid %d, which the guard may end, reads oom_score_adj %d after one scan, want 1000; the watch's log:\n%s", pid, got, rig.log())
			}
		}
	})
	t.Run("through Run", func(t *testing.T) {
		pod := guestControllerTree(t)
		noGuard(t, "a microVM controller pod's launcher built a memory guard")
		rig := useGuestPod(t, pod, nil)
		asked := askedGuest(t)
		g := runControllerLauncherOf(t, Config{MicroVM: true})
		rig.started()
		if cfg := asked(); cfg.SessionDrain != 31*time.Second {
			t.Errorf("the guest watch's SessionDrain is %s, want Run's default StopGrace of 30s and a second, 31s", cfg.SessionDrain)
		}
		ack := controllerDefaultsAck()
		ack.KernelKills = true
		g.acceptController(ack)
		if got := guardLines(g.stderr, ""); !slices.Equal(got, []string{off}) {
			t.Fatalf("the launcher's memory guard lines are %q, want %q alone", got, off)
		}
	})
	t.Run("under gVisor", func(t *testing.T) {
		pod := controllerTree(t, map[int]uint64{ctlOmp: 2 * guardGiB, ctlEval: 900 * guardMiB, ctlKernel: 700 * guardMiB})
		rig := useGuardPod(t, pod, nil)
		previous := newGuest
		newGuest = func(memoryguard.GuestConfig) *memoryguard.Guest {
			panic("a gVisor controller pod's launcher built the guest watch")
		}
		t.Cleanup(func() { newGuest = previous })
		m, _ := controllerManager()
		serveControllerGuard(t, m, &pidGuard{})
		rig.started()
		pod.setAnon(3400 * guardMiB)
		rig.tick()
		pod.setAnon(guardHigh)
		rig.tick()
		if kills := rig.signal.kills(); !slices.Equal(kills, []int{ctlEval}) {
			t.Fatalf("the guard killed %v at the mark, want %d alone; its log:\n%s", kills, ctlEval, rig.log())
		}
		if m.memoryGuard == nil {
			t.Fatal("the controller's manager holds no guard under gVisor, want the guard the launcher built")
		}
		if reports := waitingReports(m); len(reports) != 1 || reports[0].PID != ctlEval || reports[0].Cause != shimwire.MemoryKillCauseGuard {
			t.Fatalf("the controller's manager holds the reports %+v, want the guard's one report of pid %d", reports, ctlEval)
		}
	})
	t.Run("its reports", func(t *testing.T) {
		const noKernelKills = "reason=this connection's acknowledgement carries no kernelKills, so the daemon behind it may not know the report"
		const noSettings = "reason=this connection's acknowledgement carries no memory guard settings, so the daemon behind it may not know the report"
		both := controllerDefaultsAck()
		both.KernelKills = true
		for _, tc := range []struct {
			name    string
			microVM bool
			ack     shimwire.LauncherHelloAck
			dropped string
		}{
			{"a microVM controller on kernelKills and guard settings", true, both, ""},
			{"a microVM controller on kernelKills and no guard settings", true, shimwire.LauncherHelloAck{KernelKills: true}, ""},
			{"a microVM controller on guard settings and no kernelKills", true, controllerDefaultsAck(), noKernelKills},
			{"a microVM controller on an older daemon's acknowledgement", true, shimwire.LauncherHelloAck{}, noKernelKills},
			{"a gVisor controller on kernelKills and guard settings", false, both, ""},
			{"a gVisor controller on kernelKills and no guard settings", false, shimwire.LauncherHelloAck{KernelKills: true}, noSettings},
		} {
			t.Run(tc.name, func(t *testing.T) {
				report := shimwire.LauncherMemoryKill{Guard: "0123456789abcdef", Sequence: 1, PID: 900, Executable: "python3",
					SizeBytes: 600 * guardMiB, ReadingBytes: 3500 * guardMiB, CounterBytes: 3900 * guardMiB, LimitBytes: 4096 * guardMiB,
					At: time.Date(2026, 11, 19, 6, 0, 0, 0, time.UTC)}
				if tc.microVM {
					report.Cause = shimwire.MemoryKillCauseKernel
				}
				conn, stderr := waitingControllerKillReport(t, tc.microVM, report)
				conn.acknowledgeWith(tc.ack)
				if tc.dropped == "" {
					if got := conn.nextMemoryKill(); got != report {
						t.Fatalf("the connection carries the report %+v, want the one that waited for it, %+v", got, report)
					}
					conn.noMemoryKill("after the report that waited for it, the connection")
					if strings.Contains(stderr.String(), "report dropped") {
						t.Fatalf("the launcher logged %s, want no report dropped", stderr.String())
					}
					return
				}
				conn.noMemoryKill("a connection whose acknowledgement does not say the daemon knows the report")
				line := "launcher: memory guard: report dropped sequence=1 pid=900 " + tc.dropped
				if !slices.Contains(strings.Split(stderr.String(), "\n"), line) {
					t.Fatalf("the launcher logged %s, want the line %q, behind no prefix", stderr.String(), line)
				}
			})
		}
	})
}

// When the guard stops for a rise in the controller's pod, its line names each process it could not
// read, the pid, and the size where its memory map reads, and it ends neither (INFRA-3531 R3 of
// spec version 29). The shell command Oh My Pi ran has started pid 1760, whose executable and smaps
// do not read, and pid 1761, whose executable does not read and whose smaps reads 500 MiB; what the
// guard may end of the controller's tree holds 200 MiB, under the floor. At the mark it gives the
// rise up with reason nothing to end, ends no process, and its line ends with
// unread_processes=1760,1761:524288000.
func TestControllerPodMemoryGuardNamesInItsStoppedLineEachProcessItCouldNotReadAndEndsNone(t *testing.T) {
	// Not parallel: it writes the package variable containerInit (useContainerInit), which every
	// launcher reads to tell whether it is the container's first process.
	const private, threads = 1760, 1761
	pod := controllerTree(t, map[int]uint64{ctlOmp: 3 * guardGiB, ctlEval: 100 * guardMiB, ctlKernel: 100 * guardMiB})
	pod.add(
		guardProcess{pid: private, ppid: ctlCommand, exe: "/usr/bin/python3.13", args: []string{"python3", "private.py"}, anon: guardGiB,
			unreadableExe: true, unreadableSmaps: true},
		guardProcess{pid: threads, ppid: ctlCommand, exe: "/usr/bin/python3.13", args: []string{"python3", "threads.py"}, anon: 500 * guardMiB,
			unreadableExe: true},
	)
	rig := useGuardPod(t, pod, nil)
	m, log := controllerManager()
	serveControllerGuard(t, m, &pidGuard{})
	rig.started()
	pod.setAnon(guardHigh)
	rig.tick()
	if kills := rig.signal.kills(); len(kills) != 0 {
		t.Fatalf("the guard killed %v, want none; its log:\n%s", kills, rig.log())
	}
	stopped := guardLines(log, "stopped for this rise reason=nothing to end ")
	want := fmt.Sprintf(" unread=%d ", 500*guardMiB)
	named := fmt.Sprintf(" left_out=0 unread_processes=%d,%d:%d", private, threads, 500*guardMiB)
	if len(stopped) != 1 || !strings.Contains(stopped[0], want) || !strings.HasSuffix(stopped[0], named) {
		t.Fatalf("the stopped lines are %q, want one with%sending%q; its log:\n%s", stopped, want, named, rig.log())
	}
	if reports := waitingReports(m); len(reports) != 0 {
		t.Fatalf("the controller's manager holds the reports %+v, want none", reports)
	}
}

// The guard ends nothing more for a rise, and writes its stopped line, where the reading has not
// fallen when the wait after a kill has passed, where what it may end holds less than the floor
// between them, and where it may end nothing at all. A stop writes its line at most once a minute,
// so each rise here comes a minute after the one before.
func TestControllerPodMemoryGuardStopsWhereNothingFallsOrNothingMayBeEnded(t *testing.T) {
	// Not parallel: it writes the package variable containerInit (useContainerInit), which every
	// launcher reads to tell whether it is the container's first process.
	pod := controllerTree(t, map[int]uint64{ctlOmp: 2 * guardGiB, ctlEval: 900 * guardMiB, ctlKernel: 300 * guardMiB, ctlCommand: 100 * guardMiB})
	rig := useGuardPod(t, pod, nil)
	m, log := controllerManager()
	serveControllerGuard(t, m, &pidGuard{})
	rig.started()

	pod.setAnon(guardHigh)
	rig.tick()
	rig.tick()
	rig.tickAfter(memoryguard.ControllerDefaults().Wait)
	pod.setAnon(guardLow)
	rig.tick()

	// What is left to end, the Python kernel and the shell command, is 400 MiB, under the floor of
	// 409.6 MiB.
	pod.setAnon(guardHigh)
	rig.tickAfter(time.Minute)
	pod.setAnon(guardLow)
	rig.tick()

	for _, pid := range controllerEndable {
		if err := os.RemoveAll(filepath.Join(pod.proc, strconv.Itoa(pid))); err != nil {
			t.Fatal(err)
		}
	}
	pod.setAnon(guardHigh)
	rig.tickAfter(time.Minute)

	if kills := rig.signal.kills(); !slices.Equal(kills, []int{ctlEval}) {
		t.Fatalf("the guard killed %v, want the eval kernel %d alone; its log:\n%s", kills, ctlEval, rig.log())
	}
	want := []string{
		"launcher: memory guard: stopped for this rise reason=not fallen ",
		fmt.Sprintf("launcher: memory guard: stopped for this rise reason=nothing to end reading=%d endable=%d ", guardHigh, 400*guardMiB),
		fmt.Sprintf("launcher: memory guard: stopped for this rise reason=nothing to end reading=%d endable=0 ", guardHigh),
	}
	stopped := guardLines(log, "stopped ")
	if len(stopped) != len(want) {
		t.Fatalf("the stopped lines are %q, want %d starting %q", stopped, len(want), want)
	}
	for i := range want {
		if !strings.HasPrefix(stopped[i], want[i]) {
			t.Errorf("stopped line %d is %q, want it to start %q", i, stopped[i], want[i])
		}
	}
}

// Ten programs of 395 MiB, each under the floor of 409.6 MiB in a 4 GiB pod, fill it from a make
// the agent ran: what the guard may end holds far more than the floor between them, so it ends
// them one at a time, the newest of equal sizes first, one at each reading that has fallen and
// stays at the mark, as the pages one kill of a forked pool frees are few. A guard with a floor on
// each process ends none of them. Once the make is all there is left to end, under the floor, it
// ends nothing more.
func TestControllerPodMemoryGuardEndsTenProgramsUnderTheFloorOneAtATime(t *testing.T) {
	// Not parallel: it writes the package variable containerInit (useContainerInit), which every
	// launcher reads to tell whether it is the container's first process.
	const firstProgram, programs = 1600, 10
	const program = 395 * guardMiB
	if floor := guardLimit / 100 * uint64(memoryguard.ControllerDefaults().FloorPercent); program >= floor {
		t.Fatalf("a program of %d bytes is not under the floor of %d", program, floor)
	}
	pod := controllerTree(t, map[int]uint64{ctlOmp: 300 * guardMiB, ctlCommand: 20 * guardMiB})
	for i := range programs {
		pod.add(guardProcess{pid: firstProgram + i, ppid: ctlCommand, exe: "/usr/libexec/gcc/x86_64-linux-gnu/14/cc1plus",
			args: []string{"cc1plus", "-quiet", fmt.Sprintf("unit%d.cc", i)}, anon: program})
	}
	rig := useGuardPod(t, pod, nil)
	m, log := controllerManager()
	serveControllerGuard(t, m, &pidGuard{})
	rig.started()
	var want []int
	reading := 4000 * guardMiB
	for i := range programs {
		pod.setAnon(reading)
		rig.tick()
		want = append(want, firstProgram+programs-1-i)
		if kills := rig.signal.kills(); !slices.Equal(kills, want) {
			t.Fatalf("at the reading %d MiB the guard has killed %v, want %v, one program at each reading; its log:\n%s",
				reading>>20, kills, want, rig.log())
		}
		reading -= 10 * guardMiB
	}
	pod.setAnon(reading)
	rig.tick()
	if kills := rig.signal.kills(); !slices.Equal(kills, want) {
		t.Fatalf("with the make alone left the guard has killed %v, want %v; its log:\n%s", kills, want, rig.log())
	}
	stopped := fmt.Sprintf("launcher: memory guard: stopped for this rise reason=nothing to end reading=%d endable=%d ", reading, 20*guardMiB)
	if got := guardLines(log, "stopped "); len(got) != 1 || !strings.HasPrefix(got[0], stopped) {
		t.Fatalf("the stopped lines are %q, want one starting %q", got, stopped)
	}
}

// A rise that is Oh My Pi's own ends nothing: its memory grows past the mark while what the guard
// may end, its kernels and its command, holds less than the floor between them, so at every
// reading of the rise the guard ends nothing, and it writes one stopped line, which counts Oh My
// Pi's memory as protected.
func TestControllerPodMemoryGuardEndsNothingForARiseThatIsOhMyPisOwn(t *testing.T) {
	// Not parallel: it writes the package variable containerInit (useContainerInit), which every
	// launcher reads to tell whether it is the container's first process.
	pod := controllerTree(t, map[int]uint64{ctlOmp: guardGiB, ctlEval: 120 * guardMiB, ctlKernel: 100 * guardMiB, ctlCommand: 80 * guardMiB})
	rig := useGuardPod(t, pod, nil)
	m, log := controllerManager()
	serveControllerGuard(t, m, &pidGuard{})
	rig.started()
	grown := guardProcess{pid: ctlOmp, ppid: ctlShim, exe: controllerOmp, args: []string{controllerOmp, "--mode", "rpc"}, anon: 3300 * guardMiB}
	pod.add(grown)
	for _, reading := range []uint64{3500 * guardMiB, 3700 * guardMiB, 3900 * guardMiB} {
		pod.setAnon(reading)
		rig.tick()
		if kills := rig.signal.kills(); len(kills) != 0 {
			t.Fatalf("at the reading %d MiB of Oh My Pi's own rise the guard killed %v, want nothing; its log:\n%s", reading>>20, kills, rig.log())
		}
	}
	want := fmt.Sprintf("launcher: memory guard: stopped for this rise reason=nothing to end reading=%d endable=%d ", 3500*guardMiB, 300*guardMiB)
	stopped := guardLines(log, "stopped ")
	if len(stopped) != 1 || !strings.HasPrefix(stopped[0], want) {
		t.Fatalf("the stopped lines are %q, want one starting %q", stopped, want)
	}
	_, rest, _ := strings.Cut(stopped[0], " protected=")
	value, _, _ := strings.Cut(rest, " ")
	if protected, err := strconv.ParseUint(value, 10, 64); err != nil || protected < grown.anon {
		t.Fatalf("the stopped line %q counts %q protected, want at least Oh My Pi's %d", stopped[0], value, grown.anon)
	}
}

// The shared-page probe in the controller's tree: the shell command Oh My Pi ran has started a
// Python pool, whose parent holds a block of 800 MiB and whose three forked workers only sleep,
// each reporting the block as its own, as a forked process does under gVisor; beside them the
// Python kernel grows, and stays smaller than the block. By size the newest worker would end
// first, and its end would free nothing; the guard ends the kernel, the one process that grew, at
// the first reading at the mark on its reads in the band, at the 500 MiB the band's last reading
// read of it.
func TestControllerPodMemoryGuardEndsTheGrowerBeforeForksThatReportTheirParentsSharedBlock(t *testing.T) {
	// Not parallel: it writes the package variable containerInit (useContainerInit), which every
	// launcher reads to tell whether it is the container's first process.
	const pool, block = 1700, 800 * guardMiB
	pod := controllerTree(t, map[int]uint64{ctlOmp: 2 * guardGiB})
	for i := range 4 {
		parent := ctlCommand
		if i > 0 {
			parent = pool
		}
		pod.add(guardProcess{pid: pool + i, ppid: parent, exe: "/usr/bin/python3.13", args: []string{"python3", "pool.py"}, anon: block})
	}
	rig := useGuardPod(t, pod, nil)
	m, log := controllerManager()
	serveControllerGuard(t, m, &pidGuard{})
	rig.started()
	kernel := guardProcess{pid: ctlKernel, ppid: ctlOmp, exe: "/usr/bin/python3.13", args: []string{"python3", "-u", "/tmp/omp-python-kernel.py"}}
	// Oh My Pi, the block once and the kernel: 3148 MiB in the band, then 3248 and 3348, and
	// 3488 MiB at the mark.
	for _, size := range []uint64{300, 400, 500, 640} {
		kernel.anon = size * guardMiB
		pod.add(kernel)
		pod.setAnon(2*guardGiB + block + kernel.anon)
		rig.tick()
	}
	if kills := rig.signal.kills(); !slices.Equal(kills, []int{ctlKernel}) {
		t.Fatalf("the guard killed %v, want the growing Python kernel %d before the pool's workers; its log:\n%s", kills, ctlKernel, rig.log())
	}
	ended := fmt.Sprintf("launcher: memory guard: ended pid=%d role=controller size=%d reading=%d ", ctlKernel, 500*guardMiB, 3488*guardMiB)
	if got := guardLines(log, "ended "); len(got) != 1 || !strings.HasPrefix(got[0], ended) {
		t.Fatalf("the ended lines are %q, want one starting %q", got, ended)
	}
}

// The first kill comes at the first reading at the mark on the reads the band holds, before the
// guard reads any smaps at that reading (INFRA-3531 T16). In the band the Python kernel grows 100
// MiB at each reading beside the shell command, which holds at 400 MiB, so by the band's reads the
// kernel has grown and the guard decides on the held reads alone: where none of them had grown, it
// would read the held processes' smaps again at the mark (memoryguard's rereadHeld), which this
// test does not reach. As the reading reaches the mark the rig rewrites both smaps so that a read
// there would name the command: the kernel back at 300 MiB, no growth over the window, and the
// command 900 MiB larger, the most grown and the largest. The guard still ends the kernel, which
// the band's reads name, and reports it at its size at the band's last reading and at the reading
// at the mark. It reads the command's 1300 MiB only after the kill, and the command's size as of
// the kill is the one the decision held, 400 MiB (INFRA-3531 R1: its size at the reading the kill
// was sent at), so at the next reading, which has not fallen, the command has grown 900 MiB since
// the kill and the guard ends it there.
func TestControllerPodMemoryGuardEndsAtTheMarkTheProcessTheBandsReadsNameThoughAReadThereWouldNameAnother(t *testing.T) {
	// Not parallel: it writes the package variable containerInit (useContainerInit), which every
	// launcher reads to tell whether it is the container's first process.
	pod := controllerTree(t, map[int]uint64{ctlOmp: 2 * guardGiB, ctlCommand: 400 * guardMiB})
	rig := useGuardPod(t, pod, nil)
	m, log := controllerManager()
	serveControllerGuard(t, m, &pidGuard{})
	rig.started()
	kernel := guardProcess{pid: ctlKernel, ppid: ctlOmp, exe: "/usr/bin/python3.13", args: []string{"python3", "-u", "/tmp/omp-python-kernel.py"}}
	for i, reading := range []uint64{3100, 3200, 3300} {
		kernel.anon = (300 + 100*uint64(i)) * guardMiB
		pod.add(kernel)
		pod.setAnon(reading * guardMiB)
		rig.tick()
	}
	if kills := rig.signal.kills(); len(kills) != 0 {
		t.Fatalf("in the band the guard killed %v, want nothing; its log:\n%s", kills, rig.log())
	}

	kernel.anon = 300 * guardMiB
	pod.add(kernel, guardProcess{pid: ctlCommand, ppid: ctlOmp, exe: "/usr/bin/bash", args: []string{"bash", "-c", "make -j8"}, anon: 1300 * guardMiB})
	pod.setAnon(guardHigh)
	rig.tick()
	if kills := rig.signal.kills(); !slices.Equal(kills, []int{ctlKernel}) {
		t.Fatalf("at the mark the guard killed %v, want the Python kernel %d the band's reads name, and not the command %d a read at the mark names; its log:\n%s",
			kills, ctlKernel, ctlCommand, rig.log())
	}
	ended := fmt.Sprintf("launcher: memory guard: ended pid=%d role=controller size=%d reading=%d ", ctlKernel, 500*guardMiB, guardHigh)
	if got := guardLines(log, "ended "); len(got) != 1 || !strings.HasPrefix(got[0], ended) {
		t.Fatalf("the ended lines are %q, want one starting %q", got, ended)
	}
	if reports := waitingReports(m); len(reports) != 1 || reports[0].PID != ctlKernel || reports[0].SizeBytes != 500*guardMiB || reports[0].ReadingBytes != guardHigh {
		t.Fatalf("the controller's manager holds the reports %+v, want the kernel's alone, at 500 MiB and the reading at the mark", reports)
	}

	rig.tick()
	if kills := rig.signal.kills(); !slices.Equal(kills, []int{ctlKernel, ctlCommand}) {
		t.Fatalf("at the reading after the kill, not fallen and with the command 900 MiB above the size the decision held, the guard killed %v, want the kernel and then the command %d; its log:\n%s",
			kills, ctlCommand, rig.log())
	}
	ended = fmt.Sprintf("launcher: memory guard: ended pid=%d role=controller size=%d reading=%d ", ctlCommand, 1300*guardMiB, guardHigh)
	if got := guardLines(log, "ended "); len(got) != 2 || !strings.HasPrefix(got[1], ended) {
		t.Fatalf("the ended lines are %q, want the second starting %q", got, ended)
	}
}

// Where no held process grew over the window at the mark, the guard reads again there the smaps of
// the held processes that grew in the band or have a single band read, and of no other (INFRA-3531
// T17): the shell command Oh My Pi ran has started a Python pool before the band, whose parent holds
// a block of 1.5 GiB and whose three forked workers sleep through every band reading, each reporting
// the block as its own. As the reading reaches the mark the rig rewrites the newest worker's smaps
// 200 MiB larger, which a read there would rank first; the guard does not read the workers again,
// and ends the grower at the size it reads there. The grower is a command that grew 64 MiB between
// the band's first two readings and steps again only as the reading reaches the mark, its step
// before that 400 ms old, so the window at the mark shows no growth; or one that started at the
// band's last reading, with that one band read.
func TestControllerPodMemoryGuardReadsAgainAtTheMarkTheGrowersTheBandSawAndNotTheSleepingWorkers(t *testing.T) {
	// Not parallel: it writes the package variable containerInit (useContainerInit), which every
	// launcher reads to tell whether it is the container's first process.
	const pool, block, grower = 1700, 1536 * guardMiB, 1710
	for _, tc := range []struct {
		name string
		// band is the grower's size at each band reading, 0 before it starts, and mark its size
		// at the reading at the mark, in MiB.
		band []uint64
		mark uint64
	}{
		{"a grower whose crossing step comes after the band's last reading", []uint64{736, 800, 800, 800, 800}, 864},
		{"a grower with one band read", []uint64{0, 0, 0, 700}, 764},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := controllerTree(t, map[int]uint64{ctlOmp: guardGiB})
			worker := func(i int, anon uint64) guardProcess {
				parent := ctlCommand
				if i > 0 {
					parent = pool
				}
				return guardProcess{pid: pool + i, ppid: parent, exe: "/usr/bin/python3.13", args: []string{"python3", "pool.py"}, anon: anon}
			}
			for i := range 4 {
				pod.add(worker(i, block))
			}
			rig := useGuardPod(t, pod, nil)
			m, log := controllerManager()
			serveControllerGuard(t, m, &pidGuard{})
			rig.started()
			command := guardProcess{pid: grower, ppid: ctlCommand, exe: "/usr/bin/python3.13", args: []string{"python3", "grow.py"}}
			for i, size := range tc.band {
				if size > 0 {
					command.anon = size * guardMiB
					pod.add(command)
				}
				pod.setAnon((3300 + 20*uint64(i)) * guardMiB)
				rig.tick()
			}
			if kills := rig.signal.kills(); len(kills) != 0 {
				t.Fatalf("in the band the guard killed %v, want nothing; its log:\n%s", kills, rig.log())
			}

			command.anon = tc.mark * guardMiB
			pod.add(command, worker(3, block+200*guardMiB))
			pod.setAnon(guardHigh)
			rig.tick()
			if kills := rig.signal.kills(); !slices.Equal(kills, []int{grower}) {
				t.Fatalf("at the mark the guard killed %v, want the grower %d and not the worker %d it does not read again; its log:\n%s",
					kills, grower, pool+3, rig.log())
			}
			ended := fmt.Sprintf("launcher: memory guard: ended pid=%d role=controller size=%d reading=%d ", grower, tc.mark*guardMiB, guardHigh)
			if got := guardLines(log, "ended "); len(got) != 1 || !strings.HasPrefix(got[0], ended) {
				t.Fatalf("the ended lines are %q, want one starting %q", got, ended)
			}
		})
	}
}

// With the band's readings 200 ms apart, as a throttled pod's come, the window at the reading at
// the mark, 200 ms after the band's last, holds one read of each process and shows no growth, and
// the guard orders the held processes over the band's own last window (INFRA-3531 T17): the grower
// a command started grew 64 MiB between the band's two readings, beside a Python pool's three
// sleeping workers, each reporting a block of 1.5 GiB as its own. As the reading reaches the mark
// the rig rewrites the grower's smaps 24 MiB smaller and the newest worker's 200 MiB larger, so
// that a read there would name the worker. The guard ends the grower on its held reads, at the 764
// MiB the band's last reading read.
func TestControllerPodMemoryGuardOrdersByTheBandsLastWindowWhereItsReadingsCome200MillisecondsApart(t *testing.T) {
	// Not parallel: it writes the package variable containerInit (useContainerInit), which every
	// launcher reads to tell whether it is the container's first process.
	const pool, block, grower = 1700, 1536 * guardMiB, 1710
	pod := controllerTree(t, map[int]uint64{ctlOmp: guardGiB})
	worker := func(i int, anon uint64) guardProcess {
		parent := ctlCommand
		if i > 0 {
			parent = pool
		}
		return guardProcess{pid: pool + i, ppid: parent, exe: "/usr/bin/python3.13", args: []string{"python3", "pool.py"}, anon: anon}
	}
	for i := range 4 {
		pod.add(worker(i, block))
	}
	command := guardProcess{pid: grower, ppid: ctlCommand, exe: "/usr/bin/python3.13", args: []string{"python3", "grow.py"}, anon: 700 * guardMiB}
	pod.add(command)
	rig := useGuardPod(t, pod, nil)
	m, log := controllerManager()
	serveControllerGuard(t, m, &pidGuard{})
	rig.started()
	pod.setAnon(3300 * guardMiB)
	rig.tick()
	command.anon = 764 * guardMiB
	pod.add(command)
	pod.setAnon(3400 * guardMiB)
	interval := memoryguard.ControllerDefaults().Interval
	rig.tickAfter(200*time.Millisecond - interval)
	if kills := rig.signal.kills(); len(kills) != 0 {
		t.Fatalf("in the band the guard killed %v, want nothing; its log:\n%s", kills, rig.log())
	}

	command.anon = 740 * guardMiB
	pod.add(command, worker(3, block+200*guardMiB))
	pod.setAnon(guardHigh)
	rig.tickAfter(200*time.Millisecond - interval)
	if kills := rig.signal.kills(); !slices.Equal(kills, []int{grower}) {
		t.Fatalf("at the mark the guard killed %v, want the grower %d the band's last window names; its log:\n%s", kills, grower, rig.log())
	}
	ended := fmt.Sprintf("launcher: memory guard: ended pid=%d role=controller size=%d reading=%d ", grower, 764*guardMiB, guardHigh)
	if got := guardLines(log, "ended "); len(got) != 1 || !strings.HasPrefix(got[0], ended) {
		t.Fatalf("the ended lines are %q, want one starting %q", got, ended)
	}
}

// The latepool shape in the controller's tree (INFRA-3531 R1 of spec version 27): the shell
// command Oh My Pi ran has started a Python pool before the band, whose parent holds a block of 1.5
// GiB and whose three forked workers sleep through the band's three readings, each reporting the
// block as its own. A grower the command starts between the band's last reading and the mark is in
// no held read, and nothing held grew: the guard finds it as started since the band's last reading,
// and ends it at the first reading at the mark, at its 64 MiB, and not a worker.
func TestControllerPodMemoryGuardEndsAGrowerStartedSinceTheBandsLastReadingBeforeTheSleepingWorkers(t *testing.T) {
	// Not parallel: it writes the package variable containerInit (useContainerInit), which every
	// launcher reads to tell whether it is the container's first process.
	const pool, block, grower = 1700, 1536 * guardMiB, 1710
	pod := controllerTree(t, map[int]uint64{ctlOmp: guardGiB})
	for i := range 4 {
		parent := ctlCommand
		if i > 0 {
			parent = pool
		}
		pod.add(guardProcess{pid: pool + i, ppid: parent, exe: "/usr/bin/python3.13", args: []string{"python3", "pool.py"}, anon: block})
	}
	rig := useGuardPod(t, pod, nil)
	m, log := controllerManager()
	serveControllerGuard(t, m, &pidGuard{})
	rig.started()
	for _, reading := range []uint64{3300, 3320, 3340} {
		pod.setAnon(reading * guardMiB)
		rig.tick()
	}
	if kills := rig.signal.kills(); len(kills) != 0 {
		t.Fatalf("in the band the guard killed %v, want nothing; its log:\n%s", kills, rig.log())
	}

	pod.add(guardProcess{pid: grower, ppid: ctlCommand, exe: "/usr/bin/python3.13", args: []string{"python3", "grow.py"}, anon: 64 * guardMiB})
	pod.setAnon(guardHigh)
	rig.tick()
	if kills := rig.signal.kills(); !slices.Equal(kills, []int{grower}) {
		t.Fatalf("at the mark the guard killed %v, want the grower %d started since the band's last reading and no worker; its log:\n%s",
			kills, grower, rig.log())
	}
	ended := fmt.Sprintf("launcher: memory guard: ended pid=%d role=controller size=%d reading=%d ", grower, 64*guardMiB, guardHigh)
	if got := guardLines(log, "ended "); len(got) != 1 || !strings.HasPrefix(got[0], ended) {
		t.Fatalf("the ended lines are %q, want one starting %q", got, ended)
	}
}

// guardEnded is one of the guard's ended lines as a test reads it: the process it ended, the rule
// that put it first, where the sizes it was chosen by came from, and how much of its size its rank
// did not count (INFRA-3531 R3 of spec version 31, R1 of spec version 33).
type guardEnded struct {
	pid         int
	rule, sizes string
	lessParent  uint64
}

// guardEndedLines are the guard's ended lines in log, read as guardEndeds, in order.
func guardEndedLines(log *lockedBuffer) []guardEnded {
	var out []guardEnded
	for _, line := range guardLines(log, "ended ") {
		var e guardEnded
		for _, field := range strings.Fields(strings.TrimPrefix(line, "launcher: memory guard: ended ")) {
			key, value, _ := strings.Cut(field, "=")
			switch key {
			case "pid":
				e.pid, _ = strconv.Atoi(value)
			case "rule":
				e.rule = value
			case "sizes":
				e.sizes = value
			case "less_parent":
				e.lessParent, _ = strconv.ParseUint(value, 10, 64)
			}
		}
		out = append(out, e)
	}
	return out
}

// guardReplayReading is one reading of a recorded run, in hundredths of a percent of the limit,
// and how long after the reading before it the guard took it, by the run's trace.
type guardReplayReading struct {
	hundredths uint64
	after      time.Duration
}

// The latepool probe's command lines in the controller's tree: the parent's, which its forks keep,
// and each grower's, a program of its own under the same executable.
var (
	guardLatepoolArgs = []string{"/usr/bin/python3", "-c", "import os, signal, subprocess, sys, time; latepool", "pids", "held", "go", "2405181686", "0", "0", "0"}
	guardGrowArgs     = []string{"/usr/bin/python3", "-c", "import time; b=[]; [(b.append(bytearray(64<<20)), time.sleep(0.25)) for _ in iter(int, 1)]"}
)

// The tester's latepool runs at 4 GiB in the controller's tree, replayed with their traces'
// readings and times: `4g-latepool-6gd5t` and `4g-latepool-v5s8l` at 23fdd92f,
// `4g-latepool-54nzl`, `4g-latepool-kxv5k` and `4g-latepool-zxhnw` at d22d8da3, and
// `4g-latepool-djcvt` and `4g-latepool-rjm8m` at 18c3c5bb (INFRA-3531 R1 of spec versions 33 and
// 34, verification version 24). The shell command Oh My Pi ran starts the probe, a python3 parent
// that fills a block of 2.4 GB over the band's first readings, beside Oh My Pi at a quarter of the
// limit.
// It forks three sleepers, each running its program and reporting its block as its own: between
// the band's last two readings, so the band reads each once (v5s8l, 54nzl, zxhnw), after the
// band's last reading, so the reading at the mark finds them started since (6gd5t), or before the
// band's last two, so the band reads each twice (kxv5k). Six growers, python3 children of the
// parent that run a program of their own, start after the band's last reading, read at the mark at
// a few MiB each, 1 MiB apart, and step every 250 ms together. At 23fdd92f the guard ended a
// sleeping fork first in both runs, and a second one at the reading after two growers' kills that
// fell but stayed above the mark with no grower's step inside the window. At d22d8da3, where each
// fork reported its parent's whole size and ranked at nothing, the reads at the mark of 54nzl and
// zxhnw had reached one fork when the half interval had passed, and it ended first; and kxv5k and
// zxhnw ended the parent and a sleeping fork after a fall while the growers lived. In djcvt and
// rjm8m the growers start just before the band's last reading, which reads each between its fork
// and its exec with its parent's executable and arguments, in djcvt at a few MiB and in rjm8m at
// its parent's 2.4 GB; at 18c3c5bb the fork discount took them for forks, and rjm8m's reading at
// the mark, stalled 115.1 ms, ended the parent.
//
// Each fork ranks by its size less its parent's: 1 MiB at 23fdd92f and nothing at d22d8da3, where
// it is not in the tier of the processes new to the stay. In 54nzl and zxhnw the time from the
// reading at the mark to the kill at d22d8da3, 58.9 and 84.8 ms, passes at that reading (the
// guard's Observe, the one seam of its loop a launcher test reaches), before the guard reads
// anything, as a stall of the pod's CPU throttle; having read no process new to the stay with
// memory of its own, the guard reads on, up to one interval from the reading at the mark. At the
// mark it ends the largest grower, found by the search, new to the stay, its line taking nothing
// off. It ends a grower at each reading, where one grew over the window, as new to the stay, or by
// its growth since the stay's first kill, at a reading with no grower's step inside the window;
// the parent and its forks show no growth. At a reading at or above the urgent share, 92.5% of the
// limit, it ends every grower that grew over the window and, since the parent and its forks are not
// new to the stay, every grower new to it, the first by its tier and each other by the urgent rule
// (INFRA-3531 R1 of spec versions 39, 41 and 42). With the six growers ended, at the readings
// left, which fall, it ends the largest: the probe's parent, at its whole 2.4 GB, its own program
// not its parent's, the shell's; then, no read parent discounting the forks any longer, the forks,
// the newest first, each at its whole size. Neither the parent nor a fork ends while a grower
// lives. The reading at the mark reads again the executable and arguments of a held process whose
// smaps it reads where the band read it running its parent's program, so in djcvt and rjm8m the
// grower it reads ranks by its whole size; a grower the band read before its exec that the mark did
// not read again has its first read after the kill as its size as of the kill, so in rjm8m the
// guard goes on while the growers grow and after a fall ends one by its growth since the first
// kill. In `4g-t4latepool-r2` (8023d298) the band's last reading read the six growers at about
// 1.5 MiB each, as djcvt's before their exec, and a reading 148 ms late let each one's first 64 MiB
// step land before the first reading at the mark, 93.88%: the guard ends all six there. Each first
// kill's line says what the reads at the mark read of the processes new to the stay (new_read).
func TestControllerPodMemoryGuardEndsAGrowerBeforeTheSleepingForksOfAParentThatRunItsProgram(t *testing.T) {
	// Not parallel: it writes the package variable containerInit (useContainerInit), which every
	// launcher reads to tell whether it is the container's first process.
	const parent = 1700
	for _, run := range []guardLatepoolRun{
		{
			name: "the forks started between the band's last two readings, read once by the band (v5s8l)",
			band: []guardReplayReading{{7601, 0}, {7727, 105 * time.Millisecond}, {7855, 105 * time.Millisecond},
				{7979, 104 * time.Millisecond}, {8114, 103 * time.Millisecond}, {8260, 104 * time.Millisecond},
				{8381, 113 * time.Millisecond}, {8381, 103 * time.Millisecond}, {8381, 102 * time.Millisecond},
				{8381, 102 * time.Millisecond}, {8381, 106 * time.Millisecond}, {8381, 103 * time.Millisecond},
				{8381, 103 * time.Millisecond}, {8381, 103 * time.Millisecond}, {8424, 151 * time.Millisecond}},
			forksAt: 14,
			mark:    guardReplayReading{8520, 123 * time.Millisecond},
			after: []guardReplayReading{{9024, 315 * time.Millisecond}, {9227, 317 * time.Millisecond}, {9063, 110 * time.Millisecond},
				{9476, 107 * time.Millisecond}, {9360, 117 * time.Millisecond}, {9197, 108 * time.Millisecond},
				{8765, 110 * time.Millisecond}},
			own:     guardMiB,
			grown:   []uint64{4, 50, 72, 72, 136, 136, 200, 200},
			newRead: "9:9",
			want: []guardEnded{
				{1715, "new", "search", 0}, {1714, "new", "reading", 0}, {1713, "grew-since-kill", "reading", 0},
				{1712, "grew-since-kill", "reading", 0}, {1711, "grew", "reading", 0}, {1710, "urgent", "reading", 0},
				{parent, "largest", "reading", 0}, {1703, "largest", "reading", 0}, {1702, "largest", "reading", 0},
			},
		},
		{
			name: "the forks started after the band's last reading, found by the search (6gd5t)",
			band: []guardReplayReading{{7661, 0}, {7891, 103 * time.Millisecond}, {8108, 103 * time.Millisecond},
				{8329, 102 * time.Millisecond}, {8385, 102 * time.Millisecond}, {8385, 101 * time.Millisecond},
				{8385, 101 * time.Millisecond}, {8385, 101 * time.Millisecond}, {8385, 101 * time.Millisecond},
				{8385, 101 * time.Millisecond}},
			forksAt: 10,
			mark:    guardReplayReading{8533, 156 * time.Millisecond},
			after: []guardReplayReading{{9024, 242 * time.Millisecond}, {9232, 202 * time.Millisecond}, {9073, 106 * time.Millisecond},
				{9370, 105 * time.Millisecond}, {9365, 105 * time.Millisecond}, {9201, 104 * time.Millisecond},
				{8882, 105 * time.Millisecond}},
			own:     guardMiB,
			grown:   []uint64{4, 50, 72, 72, 136, 136, 200, 200},
			newRead: "9:9",
			want: []guardEnded{
				{1715, "new", "search", 0}, {1714, "grew", "reading", 0}, {1713, "grew", "reading", 0},
				{1712, "grew-since-kill", "reading", 0}, {1711, "grew", "reading", 0}, {1710, "urgent", "reading", 0},
				{parent, "largest", "reading", 0}, {1703, "largest", "reading", 0}, {1702, "largest", "reading", 0},
			},
		},
		{
			name: "the forks ranking at nothing and the reading at the mark stalled 58.9 ms (54nzl)",
			band: []guardReplayReading{{7590, 0}, {7751, 103 * time.Millisecond}, {7929, 104 * time.Millisecond},
				{8100, 106 * time.Millisecond}, {8260, 104 * time.Millisecond}, {8366, 104 * time.Millisecond},
				{8366, 111 * time.Millisecond}, {8366, 103 * time.Millisecond}, {8366, 102 * time.Millisecond},
				{8366, 101 * time.Millisecond}, {8366, 101 * time.Millisecond}, {8366, 102 * time.Millisecond},
				{8366, 102 * time.Millisecond}, {8366, 101 * time.Millisecond}, {8407, 154 * time.Millisecond}},
			forksAt: 14,
			mark:    guardReplayReading{8590, 108 * time.Millisecond},
			after: []guardReplayReading{{8904, 193 * time.Millisecond}, {9211, 267 * time.Millisecond}, {9048, 108 * time.Millisecond},
				{9282, 106 * time.Millisecond}, {9039, 111 * time.Millisecond}, {8719, 105 * time.Millisecond}},
			stall:   58900 * time.Microsecond,
			grown:   []uint64{4, 56, 70, 128, 133, 134, 134},
			newRead: "1:1",
			want: []guardEnded{
				{1715, "new", "search", 0}, {1714, "grew", "reading", 0}, {1713, "grew", "reading", 0},
				{1712, "grew", "reading", 0}, {1711, "grew", "reading", 0}, {1710, "urgent", "reading", 0},
				{parent, "largest", "reading", 0}, {1703, "largest", "reading", 0},
			},
		},
		{
			name: "the forks read twice by the band and the growers stepping in step after a fall (kxv5k)",
			band: []guardReplayReading{{7693, 0}, {7932, 102 * time.Millisecond}, {8138, 104 * time.Millisecond},
				{8364, 103 * time.Millisecond}, {8371, 103 * time.Millisecond}, {8371, 101 * time.Millisecond},
				{8371, 101 * time.Millisecond}, {8371, 102 * time.Millisecond}, {8371, 101 * time.Millisecond},
				{8401, 102 * time.Millisecond}, {8412, 105 * time.Millisecond}},
			forksAt: 9,
			mark:    guardReplayReading{8707, 141 * time.Millisecond},
			after: []guardReplayReading{{9222, 231 * time.Millisecond}, {9070, 105 * time.Millisecond}, {9057, 104 * time.Millisecond},
				{9594, 104 * time.Millisecond}, {9349, 214 * time.Millisecond}, {9249, 106 * time.Millisecond},
				{8866, 106 * time.Millisecond}},
			grown:   []uint64{45, 70, 70, 70, 134, 134, 197, 197},
			newRead: "6:6",
			want: []guardEnded{
				{1715, "new", "search", 0}, {1714, "grew", "reading", 0}, {1713, "grew-since-kill", "reading", 0},
				{1712, "grew-since-kill", "reading", 0}, {1711, "grew", "reading", 0}, {1710, "urgent", "reading", 0},
				{parent, "largest", "reading", 0}, {1703, "largest", "reading", 0}, {1702, "largest", "reading", 0},
			},
		},
		{
			name: "the reading at the mark stalled 84.8 ms and the growers stepping in step after a fall (zxhnw)",
			band: []guardReplayReading{{7606, 0}, {7799, 102 * time.Millisecond}, {7990, 102 * time.Millisecond},
				{8185, 103 * time.Millisecond}, {8342, 102 * time.Millisecond}, {8371, 106 * time.Millisecond},
				{8371, 101 * time.Millisecond}, {8371, 102 * time.Millisecond}, {8371, 102 * time.Millisecond},
				{8371, 101 * time.Millisecond}, {8371, 101 * time.Millisecond}, {8371, 103 * time.Millisecond},
				{8371, 102 * time.Millisecond}, {8414, 173 * time.Millisecond}},
			forksAt: 13,
			mark:    guardReplayReading{8563, 103 * time.Millisecond},
			after: []guardReplayReading{{8974, 206 * time.Millisecond}, {9209, 201 * time.Millisecond}, {9053, 108 * time.Millisecond},
				{9040, 106 * time.Millisecond}, {9548, 104 * time.Millisecond}, {9331, 182 * time.Millisecond},
				{9168, 103 * time.Millisecond}, {8848, 103 * time.Millisecond}},
			stall:   84800 * time.Microsecond,
			grown:   []uint64{4, 53, 68, 68, 68, 133, 133, 197, 197},
			newRead: "1:1",
			want: []guardEnded{
				{1715, "new", "search", 0}, {1714, "grew", "reading", 0}, {1713, "grew", "reading", 0},
				{1712, "grew-since-kill", "reading", 0}, {1711, "grew-since-kill", "reading", 0}, {1710, "grew", "reading", 0},
				{parent, "largest", "reading", 0}, {1703, "largest", "reading", 0}, {1702, "largest", "reading", 0},
			},
		},
		{
			name: "the growers read by the band's last reading before their exec at a few MiB (djcvt)",
			band: []guardReplayReading{{7531, 0}, {7695, 102 * time.Millisecond}, {7891, 105 * time.Millisecond},
				{8080, 102 * time.Millisecond}, {8271, 104 * time.Millisecond}, {8371, 101 * time.Millisecond},
				{8371, 102 * time.Millisecond}, {8371, 102 * time.Millisecond}, {8371, 101 * time.Millisecond},
				{8371, 101 * time.Millisecond}, {8371, 102 * time.Millisecond}, {8371, 101 * time.Millisecond},
				{8414, 101 * time.Millisecond}, {8428, 111 * time.Millisecond}},
			forksAt: 12,
			mark:    guardReplayReading{8648, 137 * time.Millisecond},
			after: []guardReplayReading{{8909, 192 * time.Millisecond}, {9067, 194 * time.Millisecond}, {9060, 109 * time.Millisecond},
				{9053, 109 * time.Millisecond}, {8734, 109 * time.Millisecond}},
			stall:   72400 * time.Microsecond,
			preExec: 2 * guardMiB,
			grown:   []uint64{30, 52, 68, 134, 134, 134},
			newRead: "1:1",
			want: []guardEnded{
				{1715, "grew", "reread", 0}, {1714, "grew", "reading", 0}, {1713, "grew", "reading", 0},
				{1712, "grew", "reading", 0}, {1711, "grew", "reading", 0}, {1710, "grew-since-kill", "reading", 0},
			},
		},
		{
			name: "the growers read by the band's last reading before their exec at the parent's size and the reading at the mark stalled 115.1 ms (rjm8m)",
			band: []guardReplayReading{{7565, 0}, {7736, 105 * time.Millisecond}, {7893, 103 * time.Millisecond},
				{8053, 104 * time.Millisecond}, {8232, 103 * time.Millisecond}, {8371, 102 * time.Millisecond},
				{8371, 102 * time.Millisecond}, {8371, 102 * time.Millisecond}, {8376, 106 * time.Millisecond},
				{8376, 102 * time.Millisecond}, {8376, 102 * time.Millisecond}, {8376, 102 * time.Millisecond},
				{8376, 102 * time.Millisecond}, {8419, 155 * time.Millisecond}},
			forksAt: 13,
			mark:    guardReplayReading{8664, 137 * time.Millisecond},
			after: []guardReplayReading{{9099, 306 * time.Millisecond}, {9222, 195 * time.Millisecond}, {9058, 111 * time.Millisecond},
				{9364, 104 * time.Millisecond}, {9044, 105 * time.Millisecond}, {8877, 106 * time.Millisecond}},
			stall:   115100 * time.Microsecond,
			preExec: guardLatepoolBlock,
			grown:   []uint64{30, 60, 70, 70, 134, 134, 197},
			newRead: "1:1",
			want: []guardEnded{
				{1715, "new", "reread", 0}, {1714, "grew-since-kill", "reading", 0}, {1713, "grew", "reading", 0},
				{1712, "grew-since-kill", "reading", 0}, {1711, "grew", "reading", 0}, {1710, "urgent", "reading", 0},
				{parent, "largest", "reading", 0}, {1703, "largest", "reading", 0},
			},
		},
		{
			// rnr56's three forks start between the band's last two readings, which read each once
			// at its parent's size, so each ranked at nothing at that read; the growers start after
			// the band's last reading. At 43213c99 the reads of the parent and the three forks
			// spent the interval, the search read nothing, and the parent ended 120.1 ms after the
			// reading at the mark (new_read=3:0). A launcher test reaches no read of the guard's but
			// Observe, so the interval the reads took passes there: the guard reads the parent, the
			// first of its reads always, and then a grower, which the search takes before the forks
			// (INFRA-3531 R1 of spec version 38), and ends it.
			name: "the forks ranking at nothing at their band read and the reads at the mark taking the interval (rnr56)",
			band: []guardReplayReading{{7754, 0}, {8129, 102 * time.Millisecond}, {8368, 103 * time.Millisecond},
				{8368, 101 * time.Millisecond}, {8368, 102 * time.Millisecond}, {8368, 104 * time.Millisecond},
				{8411, 102 * time.Millisecond}},
			forksAt: 6,
			mark:    guardReplayReading{8643, 121 * time.Millisecond},
			after: []guardReplayReading{{9378, 266 * time.Millisecond}, {9219, 108 * time.Millisecond}, {9456, 104 * time.Millisecond},
				{9361, 106 * time.Millisecond}, {9198, 103 * time.Millisecond}, {8878, 104 * time.Millisecond}},
			block:   2412277760,
			stall:   memoryguard.ControllerDefaults().Interval,
			grown:   []uint64{4, 70, 70, 134, 134, 198, 198},
			newRead: "1:1",
			want: []guardEnded{
				{1715, "new", "search", 0}, {1714, "grew", "reading", 0}, {1713, "urgent", "reading", 0},
				{1712, "urgent", "reading", 0}, {1711, "urgent", "reading", 0}, {1710, "urgent", "reading", 0},
				{parent, "largest", "reading", 0}, {1703, "largest", "reading", 0}, {1702, "largest", "reading", 0},
			},
		},
		{
			// t4latepool-r2's band reads the growers once, at its last reading, 142 ms after the
			// one before; the reading at the mark comes 148 ms after it, past the growers' first
			// step.
			name: "six growers' first steps in one late gap and the first reading at the mark at 93.88% (4g-t4latepool-r2)",
			band: []guardReplayReading{{7887, 0}, {8174, 102 * time.Millisecond}, {8368, 111 * time.Millisecond},
				{8368, 103 * time.Millisecond}, {8368, 103 * time.Millisecond}, {8368, 101 * time.Millisecond},
				{8408, 135 * time.Millisecond}, {8408, 102 * time.Millisecond}, {8408, 102 * time.Millisecond},
				{8408, 101 * time.Millisecond}, {8408, 103 * time.Millisecond}, {8408, 101 * time.Millisecond},
				{8408, 102 * time.Millisecond}, {8408, 101 * time.Millisecond}, {8408, 103 * time.Millisecond},
				{8408, 101 * time.Millisecond}, {8408, 102 * time.Millisecond}, {8408, 101 * time.Millisecond},
				{8408, 103 * time.Millisecond}, {8408, 101 * time.Millisecond}, {8408, 102 * time.Millisecond},
				{8408, 102 * time.Millisecond}, {8408, 101 * time.Millisecond}, {8408, 101 * time.Millisecond},
				{8408, 102 * time.Millisecond}, {8408, 102 * time.Millisecond}, {8429, 142 * time.Millisecond}},
			forksAt: 7,
			mark:    guardReplayReading{9388, 148 * time.Millisecond},
			preExec: 3 * guardMiB / 2,
			grown:   []uint64{64},
			newRead: "6:6",
			atMark:  6,
			want: []guardEnded{
				{1715, "grew", "reread", 0}, {1714, "urgent", "reread", 0}, {1713, "urgent", "reread", 0},
				{1712, "urgent", "reread", 0}, {1711, "urgent", "reread", 0}, {1710, "urgent", "reread", 0},
			},
		},
	} {
		t.Run(run.name, func(t *testing.T) { replayGuardLatepool(t, run) })
	}
}

// guardLatepoolBlock is the size each sleeping fork of the latepool probe reported at 4 GiB, its
// parent's block.
const guardLatepoolBlock = 2412785664

// guardLatepoolRun is one of the tester's latepool runs as replayGuardLatepool replays it in the
// controller's tree.
type guardLatepoolRun struct {
	name    string
	band    []guardReplayReading
	forksAt int
	mark    guardReplayReading
	after   []guardReplayReading
	// limit is the pod's limit, guardLimit where it is 0, and block the size each sleeping fork
	// reports, guardLatepoolBlock where it is 0; own is what each fork holds that its parent does
	// not, and stall how long the reading at the mark takes before the guard reads anything.
	limit, block, own uint64
	stall             time.Duration
	// preExec, where set, has the growers start before the band's last reading, which reads each
	// before its exec, with its parent's executable and arguments, at preExec bytes.
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
	want                 []guardEnded
}

// replayGuardLatepool replays run in the controller's tree: Oh My Pi holds a quarter of the limit,
// the shell command it ran starts the probe's parent, pid 1700, which fills its block over the
// band's readings, three sleeping forks, pids 1701 to 1703, start as forksAt says, and six growers,
// pids 1710 to 1715, start after the band's last reading (or before it, read before their exec,
// where preExec is set), each 1 MiB apart. The reading at the mark takes run.stall at the guard's
// Observe, the one seam of its loop a launcher test reaches. It asserts the first kill and every
// kill to the run's last reading, and that the guard gave no rise up.
func replayGuardLatepool(t *testing.T, run guardLatepoolRun) {
	t.Helper()
	const parent = 1700
	limit, block := run.limit, run.block
	if limit == 0 {
		limit = guardLimit
	}
	if block == 0 {
		block = guardLatepoolBlock
	}
	at := func(hundredths uint64) uint64 { return limit / 10000 * hundredths >> 10 << 10 }
	interval := memoryguard.ControllerDefaults().Interval
	plateau := run.band[len(run.band)-1].hundredths
	if run.forksAt < len(run.band) {
		plateau = run.band[run.forksAt-1].hundredths
	}
	probe := func(hundredths uint64) guardProcess {
		anon := block - run.own - (at(plateau) - min(at(hundredths), at(plateau)))
		return guardProcess{pid: parent, ppid: ctlCommand, exe: "/usr/bin/python3.13", args: guardLatepoolArgs, anon: anon}
	}
	pod := controllerTree(t, map[int]uint64{ctlOmp: limit / 4})
	pod.setLimit(limit)
	pod.add(probe(run.band[0].hundredths))
	pod.setAnon(at(run.band[0].hundredths))
	rig := useGuardPod(t, pod, nil)
	var stall atomic.Int64
	built := newMemoryGuard
	newMemoryGuard = func(cfg memoryguard.Config) *memoryguard.Guard {
		cfg.Observe = func(time.Time, uint64, uint64) { rig.clock.advance(time.Duration(stall.Load())) }
		return built(cfg)
	}
	t.Cleanup(func() { newMemoryGuard = built })
	m, log := controllerManager()
	serveControllerGuard(t, m, &pidGuard{})
	rig.started()
	fork := func(first int) {
		for i := range 3 {
			pod.add(guardProcess{pid: first + i, ppid: parent, exe: "/usr/bin/python3.13", args: guardLatepoolArgs, anon: block})
		}
	}
	growArgs := guardGrowArgs
	if run.forkGrowers {
		growArgs = guardLatepoolArgs
	}
	for i, r := range run.band[1:] {
		if i+1 == run.forksAt {
			fork(parent + 1)
		}
		if run.preExec > 0 && i+2 == len(run.band) {
			// The growers start, and the band's last reading reads each before its exec.
			for j := range 6 {
				pod.add(guardProcess{pid: parent + 10 + j, ppid: parent, exe: "/usr/bin/python3.13", args: guardLatepoolArgs, anon: run.preExec})
			}
		}
		pod.add(probe(r.hundredths))
		pod.setAnon(at(r.hundredths))
		rig.tickAfter(r.after - interval)
	}
	if run.forksAt == len(run.band) {
		fork(parent + 1)
	}
	if run.lateForks {
		fork(parent + 4)
	}
	// grow sets each grower that lives to its size at the reading of index i, 0 the mark's.
	grow := func(i int) {
		killed := rig.signal.kills()
		for j := range 6 {
			if pid := parent + 10 + j; !slices.Contains(killed, pid) {
				size := (run.grown[i] + uint64(j)) * guardMiB
				if run.forkGrowers {
					size += block
				}
				pod.add(guardProcess{pid: pid, ppid: parent, exe: "/usr/bin/python3.13", args: growArgs, anon: size})
			}
		}
	}
	grow(0)
	pod.setAnon(at(run.mark.hundredths))
	stall.Store(int64(run.stall))
	rig.tickAfter(run.mark.after - interval)
	stall.Store(0)
	if got, want := guardEndedLines(log), run.want[:max(run.atMark, 1)]; !slices.Equal(got, want) {
		t.Fatalf("at the first reading at the mark the guard ended %+v, want %+v; its log:\n%s", got, want, rig.log())
	}
	// The kills of the reading at the mark are one decision's, and each line carries its new_read.
	for _, e := range run.want[:max(run.atMark, 1)] {
		if got := guardNewRead(log, e.pid); got != run.newRead {
			t.Fatalf("the line of pid %d, killed at the reading at the mark, says new_read=%s, want %s", e.pid, got, run.newRead)
		}
	}
	for i, r := range run.after {
		grow(i + 1)
		pod.setAnon(at(r.hundredths))
		// The guard waits its interval from the end of its tick, so the stall at the mark comes
		// off the time to the reading after it.
		extra := r.after - interval
		if i == 0 {
			extra -= run.stall
		}
		rig.tickAfter(extra)
	}
	if got := guardEndedLines(log); !slices.Equal(got, run.want) {
		t.Fatalf("the guard ended %+v, want %+v; its log:\n%s", got, run.want, rig.log())
	}
	if last := run.want[len(run.want)-1].pid; run.lastNewRead != "" {
		if got := guardNewRead(log, last); got != run.lastNewRead {
			t.Fatalf("the last kill's line, of pid %d, says new_read=%s, want %s", last, got, run.lastNewRead)
		}
	}
	if stopped := guardLines(log, "stopped "); len(stopped) != 0 {
		t.Fatalf("the guard gave the rise up: %q", stopped)
	}
}

// In the controller's tree, after a kill, a dip below the mark that stays in the band ends the rise
// and not the stay, and the order's third tier counts growth since the stay's first kill across it
// (INFRA-3531 R1 of spec version 37). The tester's runs `8g-latepool-r2`, `8g-latepool-r3` and
// `8g-latefork2-r1` at 15bdbd3c under gVisor at 8 GiB, replayed with their traces' readings and
// times through the first kill after the dip, as the guard's core replays them
// (TestMemoryGuardEndsTheGrowerLeftAfterADipBelowTheMarkByItsGrowthSinceTheStaysFirstKill): the
// shell command Oh My Pi ran starts the probe, a python3 parent of a 4.8 GB block, beside Oh My Pi
// at 2 GiB, with three sleeping forks and six growers that step together (in latefork2 forks of the
// parent that run its program and grow, beside three more sleeping forks). The guard ends five
// growers, the reading sits below the mark in the band and comes back, and no read since the dip
// shows the grower left growing. At 15bdbd3c the guard ended the parent there by its whole size
// (`rule=largest sizes=reread new_read=0:0`), which freed about 6 MB; the guard ends the grower,
// by its growth since the stay's first kill.
func TestControllerPodMemoryGuardEndsTheGrowerLeftAfterADipBelowTheMarkByItsGrowthSinceTheStaysFirstKill(t *testing.T) {
	// Not parallel: it writes the package variable containerInit (useContainerInit), which every
	// launcher reads to tell whether it is the container's first process.
	const limit = 8 * guardGiB
	latepool := []guardEnded{
		{1715, "new", "search", 0}, {1714, "grew", "reading", 0}, {1713, "grew", "reading", 0},
		{1712, "grew", "reading", 0}, {1711, "grew", "reading", 0}, {1710, "grew-since-kill", "reread", 0},
	}
	for _, run := range []guardLatepoolRun{
		{
			name: "three readings 2.6 MB below the mark (8g-latepool-r2)",
			band: []guardReplayReading{{7562, 0}, {7738, 101 * time.Millisecond}, {7945, 101 * time.Millisecond},
				{8137, 102 * time.Millisecond}, {8235, 101 * time.Millisecond}, {8235, 101 * time.Millisecond},
				{8257, 102 * time.Millisecond}},
			forksAt: 6,
			mark:    guardReplayReading{8571, 156 * time.Millisecond},
			after: []guardReplayReading{{8665, 129 * time.Millisecond}, {8584, 103 * time.Millisecond}, {8739, 105 * time.Millisecond},
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
			name: "the forks read twice by the band, three readings 1.7 MB below the mark (8g-latepool-r3)",
			band: []guardReplayReading{{7521, 0}, {7693, 102 * time.Millisecond}, {7873, 101 * time.Millisecond},
				{8059, 102 * time.Millisecond}, {8238, 102 * time.Millisecond}, {8238, 101 * time.Millisecond},
				{8248, 103 * time.Millisecond}, {8259, 106 * time.Millisecond}},
			forksAt: 6,
			mark:    guardReplayReading{8648, 179 * time.Millisecond},
			after: []guardReplayReading{{8668, 119 * time.Millisecond}, {8586, 106 * time.Millisecond}, {8739, 104 * time.Millisecond},
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
			name: "the growers forks of the parent, one reading 52 MB below the mark (8g-latefork2-r1)",
			band: []guardReplayReading{{7568, 0}, {7842, 101 * time.Millisecond}, {8109, 102 * time.Millisecond},
				{8237, 101 * time.Millisecond}, {8237, 101 * time.Millisecond}, {8256, 101 * time.Millisecond},
				{8256, 102 * time.Millisecond}, {8256, 102 * time.Millisecond}, {8256, 101 * time.Millisecond},
				{8256, 102 * time.Millisecond}, {8256, 102 * time.Millisecond}, {8256, 102 * time.Millisecond},
				{8256, 102 * time.Millisecond}, {8256, 102 * time.Millisecond}, {8256, 102 * time.Millisecond},
				{8256, 101 * time.Millisecond}, {8256, 102 * time.Millisecond}, {8256, 102 * time.Millisecond},
				{8256, 102 * time.Millisecond}, {8256, 102 * time.Millisecond}, {8256, 102 * time.Millisecond},
				{8256, 101 * time.Millisecond}, {8256, 102 * time.Millisecond}, {8256, 102 * time.Millisecond},
				{8256, 101 * time.Millisecond}},
			forksAt: 5,
			mark:    guardReplayReading{8551, 102 * time.Millisecond},
			after: []guardReplayReading{{8697, 117 * time.Millisecond}, {8613, 106 * time.Millisecond}, {8687, 105 * time.Millisecond},
				{8602, 103 * time.Millisecond}, {8440, 104 * time.Millisecond}, {8518, 102 * time.Millisecond}},
			limit:       limit,
			block:       4817117184,
			forkGrowers: true,
			lateForks:   true,
			grown:       []uint64{30, 64, 64, 128, 128, 128, 192},
			newRead:     "9:6",
			want: []guardEnded{
				{1715, "new", "search", 4817117184}, {1714, "grew", "reading", 4817117184}, {1713, "grew", "reading", 4817117184},
				{1712, "grew", "reading", 4817117184}, {1711, "grew", "reading", 4817117184},
				{1710, "grew-since-kill", "reread", 4817117184},
			},
			lastNewRead: "0:0",
		},
	} {
		t.Run(run.name, func(t *testing.T) { replayGuardLatepool(t, run) })
	}
}

// guardHeldBeside is the controller's tree with Oh My Pi at 2 GiB and a python3 program the shell
// command Oh My Pi ran started, pid 1700, python3 pool.py, of 1 GiB, which sleeps through the
// band's readings; started start before the band's last reading, which reads each of them once,
// and the reading after is at the mark, where nothing has grown. It answers the guard's rig and
// log.
func guardHeldBeside(t *testing.T, started ...guardProcess) (*guardRig, *lockedBuffer) {
	t.Helper()
	pod := controllerTree(t, map[int]uint64{ctlOmp: 2 * guardGiB})
	pod.add(guardProcess{pid: 1700, ppid: ctlCommand, exe: "/usr/bin/python3.13", args: []string{"python3", "pool.py"}, anon: guardGiB})
	rig := useGuardPod(t, pod, nil)
	m, log := controllerManager()
	serveControllerGuard(t, m, &pidGuard{})
	rig.started()
	for _, reading := range []uint64{3300, 3350} {
		pod.setAnon(reading * guardMiB)
		rig.tick()
	}
	pod.add(started...)
	pod.setAnon(3400 * guardMiB)
	rig.tick()
	pod.setAnon(3500 * guardMiB)
	rig.tick()
	return rig, log
}

// In the controller's tree, at the reading at the mark, where none of the held processes grew, the
// guard reads the processes new to the stay in three groups (INFRA-3531 R1 of spec version 38):
// those with one band read that held memory of their own at it, then those started since the
// band's last reading, then those with one band read that ranked at nothing at it, forks whose
// parent's size covered their own. The python3 program pid 1700, pool.py of 1 GiB, runs through
// the band; before the band's last reading pid 1710, python3 worker.py of 100 MiB, starts, and
// pid 1700 forks pid 1701, which runs its program and reports its 1 GiB as its own; after it pid
// 1730, python3 grow.py of 50 MiB, starts. A launcher test reaches no read of the guard's but
// Observe, so in the cases where the reading at the mark takes one interval there, before the guard
// reads anything, the first group a case holds is the one read: the guard reads one process new to
// the stay and no more where it holds memory of its own. Pid 1710 ends where it is held; without
// it pid 1730 ends, found by the search, before pid 1701 is read; with pid 1701 alone the guard
// reads it, it ranks at nothing, and the largest, pid 1700, ends. Where the reading takes no time
// the guard reads all three and ends pid 1710. Each line says what the reads at the mark read
// (new_read).
func TestControllerPodMemoryGuardReadsAtTheMarkTheNewWithMemoryOfTheirOwnThenTheStartedSinceThenTheForks(t *testing.T) {
	// Not parallel: it writes the package variable containerInit (useContainerInit), which every
	// launcher reads to tell whether it is the container's first process.
	withOwn := guardProcess{pid: 1710, ppid: ctlCommand, exe: "/usr/bin/python3.13", args: []string{"python3", "worker.py"}, anon: 100 * guardMiB}
	fork := guardProcess{pid: 1701, ppid: 1700, exe: "/usr/bin/python3.13", args: []string{"python3", "pool.py"}, anon: guardGiB}
	started := guardProcess{pid: 1730, ppid: ctlCommand, exe: "/usr/bin/python3.13", args: []string{"python3", "grow.py"}, anon: 50 * guardMiB}
	interval := memoryguard.ControllerDefaults().Interval
	for _, tc := range []struct {
		name    string
		held    []guardProcess
		started []guardProcess
		stall   time.Duration
		want    guardEnded
		newRead string
	}{
		{"each group, the reads taking no time", []guardProcess{withOwn, fork}, []guardProcess{started}, 0,
			guardEnded{1710, "new", "search", 0}, "3:2"},
		{"each group, the reading at the mark one interval", []guardProcess{withOwn, fork}, []guardProcess{started}, interval,
			guardEnded{1710, "new", "reread", 0}, "1:1"},
		{"no held process with memory of its own, the reading at the mark one interval", []guardProcess{fork}, []guardProcess{started}, interval,
			guardEnded{1730, "new", "search", 0}, "1:1"},
		{"a fork alone, the reading at the mark one interval", []guardProcess{fork}, nil, interval,
			guardEnded{1700, "largest", "reread", 0}, "1:0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := controllerTree(t, map[int]uint64{ctlOmp: 2 * guardGiB})
			pod.add(guardProcess{pid: 1700, ppid: ctlCommand, exe: "/usr/bin/python3.13", args: []string{"python3", "pool.py"}, anon: guardGiB})
			rig := useGuardPod(t, pod, nil)
			var stall atomic.Int64
			built := newMemoryGuard
			newMemoryGuard = func(cfg memoryguard.Config) *memoryguard.Guard {
				cfg.Observe = func(time.Time, uint64, uint64) { rig.clock.advance(time.Duration(stall.Load())) }
				return built(cfg)
			}
			t.Cleanup(func() { newMemoryGuard = built })
			m, log := controllerManager()
			serveControllerGuard(t, m, &pidGuard{})
			rig.started()
			for _, reading := range []uint64{3300, 3350} {
				pod.setAnon(reading * guardMiB)
				rig.tick()
			}
			pod.add(tc.held...)
			pod.setAnon(3400 * guardMiB)
			rig.tick()
			pod.add(tc.started...)
			pod.setAnon(3500 * guardMiB)
			stall.Store(int64(tc.stall))
			rig.tick()
			if got := guardEndedLines(log); !slices.Equal(got, []guardEnded{tc.want}) {
				t.Fatalf("at the mark the guard ended %+v, want %+v; its log:\n%s", got, tc.want, rig.log())
			}
			if got := guardNewRead(log, tc.want.pid); got != tc.newRead {
				t.Fatalf("the ended line of pid %d says new_read=%s, want %s", tc.want.pid, got, tc.newRead)
			}
		})
	}
}

// In the controller's tree, a new process that runs its parent's executable with other arguments,
// or another executable, has run a program of its own, and ranks by its whole size (INFRA-3531 R1
// of spec version 33). The python3 program pid 1700, of 1 GiB, forks before the band's last reading
// pid 1701, which runs its program and reports 1 GiB and 2 MiB, and starts pid 1702, of 300 MiB,
// with each case's executable and arguments; the shell command starts pid 1720, of 250 MiB. At the
// mark nothing has grown, and of the three new to the stay the guard ends pid 1702, at its whole
// size, before pid 1720 and before the fork, which ranks at 2 MiB, its line taking nothing off.
func TestControllerPodMemoryGuardRanksANewProcessThatRunsAProgramOfItsOwnByItsWholeSize(t *testing.T) {
	// Not parallel: it writes the package variable containerInit (useContainerInit), which every
	// launcher reads to tell whether it is the container's first process.
	for _, tc := range []struct {
		name string
		exe  string
		args []string
	}{
		{"its parent's executable with other arguments", "/usr/bin/python3.13", []string{"python3", "worker.py"}},
		{"another executable with its parent's arguments", "/usr/bin/node", []string{"python3", "pool.py"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig, log := guardHeldBeside(t,
				guardProcess{pid: 1701, ppid: 1700, exe: "/usr/bin/python3.13", args: []string{"python3", "pool.py"}, anon: guardGiB + 2*guardMiB},
				guardProcess{pid: 1702, ppid: 1700, exe: tc.exe, args: tc.args, anon: 300 * guardMiB},
				guardProcess{pid: 1720, ppid: ctlCommand, exe: "/usr/bin/python3.13", args: []string{"python3", "idle.py"}, anon: 250 * guardMiB},
			)
			if kills := rig.signal.kills(); !slices.Equal(kills, []int{1702}) {
				t.Fatalf("at the mark the guard killed %v, want pid 1702 at its whole size; its log:\n%s", kills, rig.log())
			}
			if got, want := guardEndedLines(log), []guardEnded{{1702, "new", "reread", 0}}; !slices.Equal(got, want) {
				t.Fatalf("the guard ended %+v, want %+v", got, want)
			}
		})
	}
}

// guardNewRead is the new_read of the guard's ended line for pid in log, what the reads at the mark
// read of the processes new to the stay, "" where it wrote no such line.
func guardNewRead(log *lockedBuffer, pid int) string {
	for _, line := range guardLines(log, fmt.Sprintf("ended pid=%d ", pid)) {
		for _, field := range strings.Fields(line) {
			if value, ok := strings.CutPrefix(field, "new_read="); ok {
				return value
			}
		}
	}
	return ""
}

// In the controller's tree, the reading at the mark reads again the executable and arguments of a
// held process whose smaps it reads where the band read it running its parent's program
// (INFRA-3531 R1 of spec version 34, verification version 24): a process the band read between its
// fork and its exec carries its parent's identity there. The python3 program pid 1700, of 1 GiB,
// starts pid 1701 before the band's last reading, which reads it with pid 1700's executable and
// arguments, reporting pid 1700's 1 GiB as its own. By the reading at the mark pid 1701 runs
// python3 grow.py at 300 MiB, or still runs pool.py, a fork; nothing has grown. Running grow.py it
// ends at its whole size, new to the stay, its line saying the mark read one process new to the
// stay with memory of its own (new_read=1:1); a fork ranks at nothing, and the guard ends the
// largest, pid 1700 (new_read=1:0).
func TestControllerPodMemoryGuardRanksAHeldProcessThatHasRunAnotherProgramSinceTheBandReadItByItsWholeSize(t *testing.T) {
	// Not parallel: it writes the package variable containerInit (useContainerInit), which every
	// launcher reads to tell whether it is the container's first process.
	for _, tc := range []struct {
		name    string
		args    []string
		anon    uint64
		want    guardEnded
		newRead string
	}{
		{"one that runs grow.py since", []string{"python3", "grow.py"}, 300 * guardMiB, guardEnded{1701, "new", "reread", 0}, "1:1"},
		{"a fork that still runs pool.py", []string{"python3", "pool.py"}, guardGiB, guardEnded{1700, "largest", "reread", 0}, "1:0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := controllerTree(t, map[int]uint64{ctlOmp: 2 * guardGiB})
			pod.add(guardProcess{pid: 1700, ppid: ctlCommand, exe: "/usr/bin/python3.13", args: []string{"python3", "pool.py"}, anon: guardGiB})
			rig := useGuardPod(t, pod, nil)
			m, log := controllerManager()
			serveControllerGuard(t, m, &pidGuard{})
			rig.started()
			for _, reading := range []uint64{3300, 3350} {
				pod.setAnon(reading * guardMiB)
				rig.tick()
			}
			pod.add(guardProcess{pid: 1701, ppid: 1700, exe: "/usr/bin/python3.13", args: []string{"python3", "pool.py"}, anon: guardGiB})
			pod.setAnon(3400 * guardMiB)
			rig.tick()
			if kills := rig.signal.kills(); len(kills) != 0 {
				t.Fatalf("in the band the guard killed %v, want nothing; its log:\n%s", kills, rig.log())
			}

			pod.add(guardProcess{pid: 1701, ppid: 1700, exe: "/usr/bin/python3.13", args: tc.args, anon: tc.anon})
			pod.setAnon(3500 * guardMiB)
			rig.tick()
			if got := guardEndedLines(log); !slices.Equal(got, []guardEnded{tc.want}) {
				t.Fatalf("at the mark the guard ended %+v, want %+v; its log:\n%s", got, tc.want, rig.log())
			}
			if got := guardNewRead(log, tc.want.pid); got != tc.newRead {
				t.Fatalf("the ended line of pid %d says new_read=%s, want %s", tc.want.pid, got, tc.newRead)
			}
		})
	}
}

// The zrr4d shape in the controller's tree (INFRA-3531 R1 of spec version 31, the tester's run
// `4g-latepool-zrr4d` at 778c42de): the probe's parent holds a block of 1.5 GiB and has forked three
// sleepers that run its program before the band, each reporting the block as its own; six growers,
// its children that run a program of their own, start before the band's last reading, which reads
// each once at a few MiB, and have not stepped by the mark. Nothing grew, and the growers are new
// to the stay beside an older, larger sleeper, the parent: the guard ends the largest grower, its
// line naming the new tier and taking nothing off.
func TestControllerPodMemoryGuardEndsAGrowerReadOnceAtTheBandsLastReadingBeforeAnOlderLargerSleeper(t *testing.T) {
	// Not parallel: it writes the package variable containerInit (useContainerInit), which every
	// launcher reads to tell whether it is the container's first process.
	const parent, block = 1700, 1536 * guardMiB
	pod := controllerTree(t, map[int]uint64{ctlOmp: guardGiB})
	pod.add(guardProcess{pid: parent, ppid: ctlCommand, exe: "/usr/bin/python3.13", args: guardLatepoolArgs, anon: block})
	for i := range 3 {
		pod.add(guardProcess{pid: parent + 1 + i, ppid: parent, exe: "/usr/bin/python3.13", args: guardLatepoolArgs, anon: block + guardMiB})
	}
	rig := useGuardPod(t, pod, nil)
	m, log := controllerManager()
	serveControllerGuard(t, m, &pidGuard{})
	rig.started()
	for _, reading := range []uint64{3300, 3350} {
		pod.setAnon(reading * guardMiB)
		rig.tick()
	}
	for j := range 6 {
		pod.add(guardProcess{pid: parent + 10 + j, ppid: parent, exe: "/usr/bin/python3.13", args: guardGrowArgs, anon: uint64(4+j) * guardMiB})
	}
	pod.setAnon(3400 * guardMiB)
	rig.tick()
	if kills := rig.signal.kills(); len(kills) != 0 {
		t.Fatalf("in the band the guard killed %v, want nothing; its log:\n%s", kills, rig.log())
	}
	pod.setAnon(guardHigh)
	rig.tick()
	if got, want := guardEndedLines(log), []guardEnded{{parent + 15, "new", "reread", 0}}; !slices.Equal(got, want) {
		t.Fatalf("at the mark the guard ended %+v, want the largest grower %+v and no sleeper; its log:\n%s", got, want, rig.log())
	}
}

// At the first reading at the mark, the guard reads the processes started since the band's last
// reading the newest first, within half an interval of the reading at the mark by its clock, one at
// least, and ends the largest of those it read (INFRA-3531 R1 of spec version 28). A Python pool's
// workers sleep through the band beside Oh My Pi; as the reading reaches the mark the shell command
// starts pid 1710, of 300 MiB, and then pid 1711, of 100 MiB. Where the reading at the mark itself
// takes 80 ms by the guard's clock, past the budget of 50 ms, as on a pod whose CPU the host
// throttles, the guard reads pid 1711 alone and ends it, passing over the larger, older pid 1710;
// where it takes no time, the guard reads both and ends pid 1710. The rig moves the clock on at the
// reading (the guard's Observe), the one seam of the guard's loop a launcher test reaches.
func TestControllerPodMemoryGuardReadsTheNewestStartedSinceTheBandWithinHalfAnIntervalAndEndsTheLargestItRead(t *testing.T) {
	// Not parallel: it writes the package variable containerInit (useContainerInit), which every
	// launcher reads to tell whether it is the container's first process.
	const pool, block, older, newer = 1700, 1536 * guardMiB, 1710, 1711
	for _, tc := range []struct {
		name string
		mark time.Duration
		want int
		size uint64
	}{
		{"the reading at the mark past the budget", 80 * time.Millisecond, newer, 100 * guardMiB},
		{"no time at the reading at the mark", 0, older, 300 * guardMiB},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := controllerTree(t, map[int]uint64{ctlOmp: guardGiB})
			for i := range 4 {
				parent := ctlCommand
				if i > 0 {
					parent = pool
				}
				pod.add(guardProcess{pid: pool + i, ppid: parent, exe: "/usr/bin/python3.13", args: []string{"python3", "pool.py"}, anon: block})
			}
			rig := useGuardPod(t, pod, nil)
			var atMark atomic.Int64
			built := newMemoryGuard
			newMemoryGuard = func(cfg memoryguard.Config) *memoryguard.Guard {
				cfg.Observe = func(time.Time, uint64, uint64) { rig.clock.advance(time.Duration(atMark.Load())) }
				return built(cfg)
			}
			t.Cleanup(func() { newMemoryGuard = built })
			m, log := controllerManager()
			serveControllerGuard(t, m, &pidGuard{})
			rig.started()
			for _, reading := range []uint64{3300, 3320} {
				pod.setAnon(reading * guardMiB)
				rig.tick()
			}

			command := func(pid int, anon uint64) guardProcess {
				return guardProcess{pid: pid, ppid: ctlCommand, exe: "/usr/bin/python3.13", args: []string{"python3", "grow.py"}, anon: anon}
			}
			pod.add(command(older, 300*guardMiB), command(newer, 100*guardMiB))
			pod.setAnon(guardHigh)
			atMark.Store(int64(tc.mark))
			rig.tick()
			if kills := rig.signal.kills(); !slices.Equal(kills, []int{tc.want}) {
				t.Fatalf("at the mark the guard killed %v, want %d; its log:\n%s", kills, tc.want, rig.log())
			}
			ended := fmt.Sprintf("launcher: memory guard: ended pid=%d role=controller size=%d reading=%d ", tc.want, tc.size, guardHigh)
			if got := guardLines(log, "ended "); len(got) != 1 || !strings.HasPrefix(got[0], ended) {
				t.Fatalf("the ended lines are %q, want one starting %q", got, ended)
			}
		})
	}
}

// After a kill in the controller's tree, the next in the order is the one that grew the most since
// that kill where nothing grew over the window and no process is new to the stay (INFRA-3531 R1 of
// spec version 31): readings more than the window apart, as a throttled pod's come, leave one read
// of each process in the window. The shell command Oh My Pi ran has started a Python pool, whose
// parent holds a block of 1.5 GiB and whose three forked workers sleep, each reporting the block
// as its own, and a grower smaller than the block; the band reads each of them twice. At the first
// reading at the mark nothing has grown and none is new, and the guard ends the largest: the
// parent, since each worker runs its program and ranks by what it holds over the parent's size
// (INFRA-3531 R1 of spec version 33); its end frees nothing, its workers keeping its pages. The
// next reading comes 400 ms later with the grower 64 MiB larger and the reading not fallen: over
// the window nothing grew, but the grower grew since the kill, and the guard ends it, not a worker.
func TestControllerPodMemoryGuardEndsTheGrowerNextAfterAKillWhereReadingsComeWiderApartThanTheWindow(t *testing.T) {
	// Not parallel: it writes the package variable containerInit (useContainerInit), which every
	// launcher reads to tell whether it is the container's first process.
	const pool, block = 1700, 1536 * guardMiB
	pod := controllerTree(t, map[int]uint64{ctlOmp: guardGiB})
	for i := range 4 {
		parent := ctlCommand
		if i > 0 {
			parent = pool
		}
		pod.add(guardProcess{pid: pool + i, ppid: parent, exe: "/usr/bin/python3.13", args: []string{"python3", "pool.py"}, anon: block})
	}
	grower := guardProcess{pid: pool + 10, ppid: ctlCommand, exe: "/usr/bin/python3.13", args: []string{"python3", "grow.py"}, anon: 1000 * guardMiB}
	pod.add(grower)
	rig := useGuardPod(t, pod, nil)
	m, log := controllerManager()
	serveControllerGuard(t, m, &pidGuard{})
	rig.started()
	for _, inTheBand := range []uint64{3300, 3400} {
		pod.setAnon(inTheBand * guardMiB)
		rig.tick()
	}
	// Oh My Pi, the block once and the grower: 3560 MiB, at the mark.
	reading := func() uint64 { return guardGiB + block + grower.anon }
	pod.setAnon(reading())
	rig.tick()
	if kills := rig.signal.kills(); !slices.Equal(kills, []int{pool}) {
		t.Fatalf("at the mark the guard killed %v, want the pool's parent %d, the largest where nothing grew; its log:\n%s", kills, pool, rig.log())
	}

	grower.anon += 64 * guardMiB
	pod.add(grower)
	pod.setAnon(reading())
	rig.tickAfter(300 * time.Millisecond)
	if kills := rig.signal.kills(); !slices.Equal(kills, []int{pool, grower.pid}) {
		t.Fatalf("400 ms after the kill the guard killed %v, want the grower %d next and not another worker; its log:\n%s",
			kills, grower.pid, rig.log())
	}
	ended := fmt.Sprintf("launcher: memory guard: ended pid=%d role=controller size=%d reading=%d ", grower.pid, 1064*guardMiB, reading())
	if got := guardLines(log, "ended "); len(got) != 2 || !strings.HasPrefix(got[1], ended) {
		t.Fatalf("the ended lines are %q, want the second starting %q", got, ended)
	}
	if stopped := guardLines(log, "stopped "); len(stopped) != 0 {
		t.Fatalf("the guard gave the rise up while the grower grew: %q", stopped)
	}
	if reports := waitingReports(m); len(reports) != 2 || reports[1].PID != grower.pid {
		t.Fatalf("the controller's manager holds the reports %+v, want the parent's and then the grower's", reports)
	}
}

// A kill on the band's held reads in the controller's tree has as each process's size as of it the
// size the decision read, and not a read made after the kill (INFRA-3531 R1), in the shape of
// `8g-latepool-q56hm` at 7356e0f6. The shell command Oh My Pi ran has started three growers, pids
// 1710 to 1712, which step 64 MiB at each reading of the band; at the first reading at the mark,
// 3500 MiB, the guard ends pid 1712, the largest of equal growth, on the band's held sizes. The two
// left step 64 MiB between that reading and the guard's read after the kill, and then not again;
// at the next reading, 3528 MiB and not fallen, each has grown 64 MiB since the kill, and the guard
// ends pid 1711 there, where a guard that kept the read after the kill waited.
func TestControllerPodMemoryGuardMeasuresGrowthSinceAKillOnHeldSizesFromTheSizesTheDecisionRead(t *testing.T) {
	// Not parallel: it writes the package variable containerInit (useContainerInit), which every
	// launcher reads to tell whether it is the container's first process.
	pod := controllerTree(t, map[int]uint64{ctlOmp: 2 * guardGiB})
	sizes := map[int]uint64{1710: 200 * guardMiB, 1711: 210 * guardMiB, 1712: 220 * guardMiB}
	// put writes each grower at its size.
	put := func() {
		for pid, size := range sizes {
			pod.add(guardProcess{pid: pid, ppid: ctlCommand, exe: "/usr/bin/python3.13", args: []string{"python3", "grow.py"}, anon: size})
		}
	}
	// step has each grower step 64 MiB.
	step := func() {
		for pid := range sizes {
			sizes[pid] += 64 * guardMiB
		}
		put()
	}
	put()
	pod.setAnon(3200 * guardMiB)
	rig := useGuardPod(t, pod, nil)
	// The growers left step between the reading at the mark and the guard's read after its kill.
	rig.signal.afterKill = func(pid int) {
		delete(sizes, pid)
		if pid == 1712 {
			step()
		}
	}
	m, log := controllerManager()
	serveControllerGuard(t, m, &pidGuard{})
	rig.started()
	for _, inTheBand := range []uint64{3300, 3400} {
		step()
		pod.setAnon(inTheBand * guardMiB)
		rig.tick()
	}
	if kills := rig.signal.kills(); len(kills) != 0 {
		t.Fatalf("in the band the guard killed %v, want nothing; its log:\n%s", kills, rig.log())
	}

	pod.setAnon(3500 * guardMiB)
	rig.tick()
	if got, want := guardEndedLines(log), []guardEnded{{1712, "grew", "band", 0}}; !slices.Equal(got, want) {
		t.Fatalf("at the first reading at the mark the guard ended %+v, want %+v; its log:\n%s", got, want, rig.log())
	}

	pod.setAnon(3528 * guardMiB)
	rig.tick()
	if kills := rig.signal.kills(); !slices.Equal(kills, []int{1712, 1711}) {
		t.Fatalf("at the first reading after the exit the guard killed %v, want [1712 1711]; its log:\n%s", kills, rig.log())
	}
	ended := fmt.Sprintf("launcher: memory guard: ended pid=1711 role=controller size=%d reading=%d ", 402*guardMiB, 3528*guardMiB)
	if got := guardLines(log, "ended "); len(got) != 2 || !strings.HasPrefix(got[1], ended) {
		t.Fatalf("the ended lines are %q, want the second starting %q", got, ended)
	}
	if stopped := guardLines(log, "stopped "); len(stopped) != 0 {
		t.Fatalf("the guard gave the rise up: %q", stopped)
	}
}

// In the controller's tree, a process the reading's table lists but whose smaps the reading could
// not read keeps its size as of the kill (INFRA-3531 R1): its growth since the kill counts once it
// is read again. The shell command Oh My Pi ran runs three Python programs; at the first reading,
// at the mark, the guard ends the largest, pid 1700, of 900 MiB, which exits and frees nothing.
// Pid 1701, python3 grow.py of 300 MiB, grows 64 MiB at the next reading, 400 ms later, which
// cannot read its smaps, and the reading has not fallen: nothing the guard read has grown, and it
// ends nothing. At the reading after, 400 ms later, pid 1701 reads 364 MiB, grown since the kill,
// and the guard goes on and ends it, before pid 1702, python3 idle.py of 600 MiB, which never grew.
// A guard that dropped its size as of the kill at the reading that did not read it took 364 MiB as
// that size, saw nothing grown, and gave the rise up once the wait had passed.
func TestControllerPodMemoryGuardGoesOnForAGrowerItCouldNotReadAtAReadingAfterTheKill(t *testing.T) {
	// Not parallel: it writes the package variable containerInit (useContainerInit), which every
	// launcher reads to tell whether it is the container's first process.
	pod := controllerTree(t, map[int]uint64{ctlOmp: 2 * guardGiB})
	command := func(pid int, program string, anon uint64) guardProcess {
		return guardProcess{pid: pid, ppid: ctlCommand, exe: "/usr/bin/python3.13", args: []string{"python3", program}, anon: anon}
	}
	grower := command(1701, "grow.py", 300*guardMiB)
	pod.add(command(1700, "big.py", 900*guardMiB), grower, command(1702, "idle.py", 600*guardMiB))
	pod.setAnon(guardHigh)
	rig := useGuardPod(t, pod, nil)
	m, log := controllerManager()
	serveControllerGuard(t, m, &pidGuard{})
	rig.started()
	if kills := rig.signal.kills(); !slices.Equal(kills, []int{1700}) {
		t.Fatalf("at the first reading, at the mark, the guard killed %v, want the largest, 1700; its log:\n%s", kills, rig.log())
	}

	grower.anon, grower.unreadableSmaps = 364*guardMiB, true
	pod.add(grower)
	rig.tickAfter(300 * time.Millisecond)
	if kills := rig.signal.kills(); !slices.Equal(kills, []int{1700}) {
		t.Fatalf("at the reading that could not read pid 1701 the guard killed %v, want nothing more; its log:\n%s", kills, rig.log())
	}
	grower.unreadableSmaps = false
	pod.add(grower)
	rig.tickAfter(300 * time.Millisecond)
	if kills := rig.signal.kills(); !slices.Equal(kills, []int{1700, 1701}) {
		t.Fatalf("at the reading after, the guard killed %v, want pid 1701, grown since the kill, next; its log:\n%s", kills, rig.log())
	}
	if stopped := guardLines(log, "stopped "); len(stopped) != 0 {
		t.Fatalf("the guard gave the rise up while pid 1701 grew: %q", stopped)
	}
}

// After a kill of the rise, the largest process new to the stay is ended before a larger process
// that is not and did not grow, and before one that grew only since the stay's first kill; then
// the one that grew since that kill before the largest; and where nothing grew and nothing is new,
// the largest is ended (INFRA-3531 R1 of spec versions 31 and 34). The shell command Oh My Pi ran
// runs three Python processes, which the band reads twice. At the first reading at the mark none is
// new and none grew, and the guard ends the largest, pid 1700. 400 ms later, the reading not
// fallen, pid 1710 has grown 64 MiB since that kill, which has the guard go on, and two processes
// have started, pids 1720 and 1721, of 200 and 100 MiB; pid 1701 sleeps at 900 MiB. Over the
// window nothing grew, and the guard ends pid 1720, the larger of the two new ones. At the next
// reading, fallen but still at the mark, pid 1721 has one read before it and is new still, and
// ends. At the reading after, fallen again, nothing grew over the window or since the previous
// kill and nothing is new, and the guard ends pid 1710, which grew since the stay's first kill,
// before pid 1701, the largest; at the one after, fallen again, it ends pid 1701. Each ended line
// names its rule.
func TestControllerPodMemoryGuardEndsTheLargestNewProcessAfterAKillBeforeALargerIdleOneOrAGrowerAndTheLargestWhereNoneApplies(t *testing.T) {
	// Not parallel: it writes the package variable containerInit (useContainerInit), which every
	// launcher reads to tell whether it is the container's first process.
	const first, idle, grower, newer, newest = 1700, 1701, 1710, 1720, 1721
	pod := controllerTree(t, map[int]uint64{ctlOmp: 2 * guardGiB})
	command := func(pid int, anon uint64) guardProcess {
		return guardProcess{pid: pid, ppid: ctlCommand, exe: "/usr/bin/python3.13", args: []string{"python3", "work.py"}, anon: anon}
	}
	pod.add(command(first, 1000*guardMiB), command(idle, 900*guardMiB), command(grower, 300*guardMiB))
	rig := useGuardPod(t, pod, nil)
	m, log := controllerManager()
	serveControllerGuard(t, m, &pidGuard{})
	rig.started()
	step := func(what string, reading uint64, extra time.Duration, want ...int) {
		t.Helper()
		pod.setAnon(reading)
		rig.tickAfter(extra)
		if kills := rig.signal.kills(); !slices.Equal(kills, want) {
			t.Fatalf("%s: the guard killed %v, want %v; its log:\n%s", what, kills, want, rig.log())
		}
	}
	step("at the band's first reading", 3300*guardMiB, 0)
	step("at the band's second reading", 3400*guardMiB, 0)
	step("at the first reading at the mark", guardHigh, 0, first)

	pod.add(command(grower, 364*guardMiB), command(newer, 200*guardMiB), command(newest, 100*guardMiB))
	step("400 ms after the kill", guardHigh, 300*time.Millisecond, first, newer)
	step("at the next reading, fallen, with one read of the newest before it", 3550*guardMiB, 0, first, newer, newest)
	step("at the reading after, fallen, with pid 1710 grown since the stay's first kill", 3540*guardMiB, 0, first, newer, newest, grower)
	step("at the reading after, fallen and with nothing grown or new", 3530*guardMiB, 0, first, newer, newest, grower, idle)
	if stopped := guardLines(log, "stopped "); len(stopped) != 0 {
		t.Fatalf("the guard gave the rise up: %q", stopped)
	}
	ended := guardLines(log, "ended ")
	rules := []string{" rule=largest sizes=band ", " rule=new sizes=reading ", " rule=new sizes=reading ", " rule=grew-since-kill sizes=reading ",
		" rule=largest sizes=reading "}
	if len(ended) != len(rules) {
		t.Fatalf("the ended lines are %q, want %d", ended, len(rules))
	}
	for i, rule := range rules {
		if !strings.Contains(ended[i], rule) {
			t.Errorf("ended line %d is %q, want it to hold %q", i, ended[i], rule)
		}
	}
}

// Oh My Pi growing past the mark beside an idle Python kernel and an idle JavaScript kernel, which
// hold more than the floor between them and each on its own, costs one kernel: at the mark nothing
// the guard may end has grown, so it ends the larger, the JavaScript kernel. That kill lowers
// nothing Oh My Pi's growth does not make up, and once the controller's wait of 1 second passes
// with no fall and no kernel growing, the guard writes its stopped line with reason not fallen
// and never ends the Python kernel, however far Oh My Pi grows.
func TestControllerPodMemoryGuardEndsOneKernelForARiseThatIsOhMyPisOwn(t *testing.T) {
	// Not parallel: it writes the package variable containerInit (useContainerInit), which every
	// launcher reads to tell whether it is the container's first process.
	pod := controllerTree(t, map[int]uint64{ctlOmp: 2000 * guardMiB, ctlEval: 600 * guardMiB, ctlKernel: 500 * guardMiB})
	rig := useGuardPod(t, pod, nil)
	m, log := controllerManager()
	serveControllerGuard(t, m, &pidGuard{})
	rig.started()
	omp := guardProcess{pid: ctlOmp, ppid: ctlShim, exe: controllerOmp, args: []string{controllerOmp, "--mode", "rpc"}, anon: 2000 * guardMiB}
	grow := func(reading uint64) {
		omp.anon += 100 * guardMiB
		pod.add(omp)
		pod.setAnon(reading * guardMiB)
	}
	for _, reading := range []uint64{3200, 3300, 3400, 3500} {
		grow(reading)
		rig.tick()
	}
	if kills := rig.signal.kills(); !slices.Equal(kills, []int{ctlEval}) {
		t.Fatalf("at the mark the guard killed %v, want the larger idle kernel %d; its log:\n%s", kills, ctlEval, rig.log())
	}
	// The reading 100 ms before the wait passes, and the one at which it has passed.
	settings := memoryguard.ControllerDefaults()
	grow(3600)
	rig.tickAfter(settings.Wait - 2*settings.Interval)
	grow(3700)
	rig.tick()
	for _, reading := range []uint64{3800, 3900, 4000} {
		grow(reading)
		rig.tick()
	}
	if kills := rig.signal.kills(); !slices.Equal(kills, []int{ctlEval}) {
		t.Fatalf("the guard killed %v, want the JavaScript kernel %d alone and not the Python kernel %d; its log:\n%s",
			kills, ctlEval, ctlKernel, rig.log())
	}
	want := fmt.Sprintf("launcher: memory guard: stopped for this rise reason=not fallen reading=%d endable=%d ", 3700*guardMiB, 500*guardMiB)
	if stopped := guardLines(log, "stopped "); len(stopped) != 1 || !strings.HasPrefix(stopped[0], want) {
		t.Fatalf("the stopped lines are %q, want one starting %q", stopped, want)
	}
	if reports := waitingReports(m); len(reports) != 1 || reports[0].PID != ctlEval {
		t.Fatalf("the controller's manager holds the reports %+v, want the JavaScript kernel's alone", reports)
	}
}

// A guard that cannot read the pod turns off for good with one off line and ends nothing: with no
// readable AnonPages, no readable limit, or a limit that reads as no limit, though the pod is at the
// mark with a process the guard may end above the floor.
func TestControllerPodMemoryGuardTurnsOffWhereItCannotReadThePod(t *testing.T) {
	// Not parallel: it writes the package variable containerInit (useContainerInit), which every
	// launcher reads to tell whether it is the container's first process.
	for _, tc := range []struct {
		name   string
		spoil  func(pod *guardPod) error
		reason string
	}{
		{"no meminfo", func(pod *guardPod) error { return os.Remove(filepath.Join(pod.proc, "meminfo")) },
			"off reason=no readable AnonPages in "},
		{"a meminfo with no AnonPages", func(pod *guardPod) error {
			return os.WriteFile(filepath.Join(pod.proc, "meminfo"), []byte("MemTotal:       20971520 kB\nShmem:              32 kB\n"), 0o644)
		}, "off reason=no AnonPages line in "},
		{"no limit", func(pod *guardPod) error { return os.Remove(filepath.Join(pod.cgroup, "memory.limit_in_bytes")) },
			"off reason=no readable limit in "},
		{"a limit that reads as no limit", func(pod *guardPod) error { pod.setLimit(9223372036854771712); return nil },
			"off reason=the limit 9223372036854771712 in "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := controllerTree(t, map[int]uint64{ctlOmp: 2 * guardGiB, ctlKernel: 900 * guardMiB})
			pod.setAnon(guardHigh)
			if err := tc.spoil(pod); err != nil {
				t.Fatal(err)
			}
			rig := useGuardPod(t, pod, nil)
			m, log := controllerManager()
			serveControllerGuard(t, m, &pidGuard{})
			if !testwait.Holds(beforeDeadline(t), testwait.Poll, func() bool { return len(guardLines(log, "off ")) > 0 }) {
				t.Fatalf("the guard wrote no off line; its log:\n%s", log.String())
			}
			if lines := guardLines(log, ""); len(lines) != 1 || !strings.HasPrefix(lines[0], "launcher: memory guard: "+tc.reason) {
				t.Fatalf("the guard's lines are %q, want one off line starting %q", lines, tc.reason)
			}
			if kills, reports := rig.signal.kills(), waitingReports(m); len(kills) != 0 || len(reports) != 0 {
				t.Fatalf("a guard that is off killed %v and reported %+v, want nothing", kills, reports)
			}
		})
	}
}

// A single-role launcher that is not its container's first process, a controller's launcher run
// by hand or by a test, runs no memory guard: Run writes one off line through the role's own log
// and builds no guard, though its manager has no roles log, and serves on: an acknowledgement's
// settings reach no guard, a stop is answered, and Run returns at its context's end with no panic.
// serveMemoryGuard's single-role form answers at once and leaves the manager holding no guard.
func TestControllerPodMemoryGuardRunsOnlyInTheContainersFirstProcess(t *testing.T) {
	// Not parallel: it writes the package variable containerInit (useContainerInit), which every
	// launcher reads to tell whether it is the container's first process.
	useContainerInit(t, false)
	previous := newMemoryGuard
	newMemoryGuard = func(memoryguard.Config) *memoryguard.Guard {
		panic("a launcher that is not the container's first process built a memory guard")
	}
	t.Cleanup(func() { newMemoryGuard = previous })
	g := runControllerLauncher(t)
	g.acceptController(memoryGuardAck(70, 10, 5, 100))
	g.stop(1)
	want := fmt.Sprintf("launcher: memory guard: off reason=the launcher is pid %d, not the container's first process, so it neither sees nor owns every process of the container",
		os.Getpid())
	if got := guardLines(g.stderr, ""); !slices.Equal(got, []string{want}) {
		t.Fatalf("the launcher's memory guard lines are %q, want %q alone", got, want)
	}

	log := &lockedBuffer{}
	m := &manager{role: "controller", stderr: log}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	select {
	case <-serveMemoryGuard(ctx, Config{}, []*manager{m}, &pidGuard{}):
	default:
		t.Fatal("serveMemoryGuard left something running in a launcher that runs no guard")
	}
	if m.memoryGuard != nil || m.memoryWake != nil {
		t.Fatalf("the controller's manager holds the guard %v and the wake %v, want neither", m.memoryGuard, m.memoryWake)
	}
	if got := guardLines(log, ""); !slices.Equal(got, []string{want}) {
		t.Fatalf("the controller's memory guard lines are %q, want %q alone", got, want)
	}
}

// Before any acknowledgement the controller pod's guard runs at its own defaults, those of an issue
// pod's but a wait of 1 second after a kill (memoryguard.ControllerDefaults), which its start line
// says. An acknowledgement at 70% with the controller pod's other defaults, through the wire's own
// encoder and decoder, is taken in place: one settings line and no second start line, and the
// report of the kill made before it goes on its connection. After the connection drops, the next
// acknowledgement's other numbers are taken in place too, with no second start line, and the
// hour's window keeps the start it took at the guard's first reading.
func TestControllerPodMemoryGuardRunsAtTheDefaultsUntilAnAcknowledgementsSettings(t *testing.T) {
	// Not parallel: it writes the package variable containerInit (useContainerInit), which every
	// launcher reads to tell whether it is the container's first process.
	const first, second, third = 1650, 1651, 1652
	pod := newGuardPod(t)
	pod.add(
		guardProcess{pid: first, ppid: os.Getpid(), exe: "/usr/bin/python3.13", args: []string{"python3", "first.py"}, anon: 900 * guardMiB},
		guardProcess{pid: second, ppid: os.Getpid(), exe: "/usr/bin/python3.13", args: []string{"python3", "second.py"}, anon: 800 * guardMiB},
		guardProcess{pid: third, ppid: os.Getpid(), exe: "/usr/bin/python3.13", args: []string{"python3", "third.py"}, anon: 700 * guardMiB},
	)
	rig := useGuardPod(t, pod, nil)
	g := runControllerLauncher(t)
	rig.started()
	started := fmt.Sprintf("launcher: memory guard: started act_percent=85 floor_percent=10 wait=1s interval=100ms limit=%d reading=%d", guardLimit, guardLow)
	if got := guardLines(g.stderr, "started "); !slices.Equal(got, []string{started}) {
		t.Fatalf("the guard's start lines are %q, want %q", got, started)
	}
	controller := memoryguard.Defaults()
	controller.Wait = time.Second
	if got := memoryguard.ControllerDefaults(); got != controller {
		t.Fatalf("the controller pod's defaults are %+v, want an issue pod's with a wait of 1s, %+v", got, controller)
	}
	kill := func(what string, reading uint64, want ...int) {
		t.Helper()
		pod.setAnon(reading)
		rig.tick()
		if kills := rig.signal.kills(); !slices.Equal(kills, want) {
			t.Fatalf("%s the guard killed %v at %d%% of the limit, want %v; its log:\n%s", what, kills, reading*100/guardLimit, want, rig.log())
		}
		pod.setAnon(guardLow)
		rig.tick()
	}
	report := func(conn *roleConn, pid int, sequence uint64) {
		t.Helper()
		if r := conn.nextMemoryKill(); r.PID != pid || r.Sequence != sequence || r.Guard != rig.id() {
			t.Fatalf("the controller's connection carries %+v, want the report of pid %d, sequence %d of guard %s", r, pid, sequence, rig.id())
		}
	}
	kill("before any acknowledgement, at the default act share of 85%,", guardLimit/100*86, first)

	conn := g.acceptController(memoryGuardAck(70, shimwire.MemoryGuardFloorDefault, shimwire.MemoryGuardControllerWaitDefault, shimwire.MemoryGuardIntervalDefault))
	at70 := "launcher: memory guard: settings act_percent=70 floor_percent=10 wait=1s interval=100ms"
	if got := guardLines(g.stderr, "settings "); !slices.Equal(got, []string{at70}) {
		t.Fatalf("after the acknowledgement the guard's settings lines are %q, want %q", got, at70)
	}
	report(conn, first, 1)
	kill("at an act share of 70%", guardLimit/100*75, first, second)
	report(conn, second, 2)

	g.dropConnection()
	again := g.acceptController(memoryGuardAck(60, 5, 3, 500))
	at60 := "launcher: memory guard: settings act_percent=60 floor_percent=5 wait=3s interval=500ms"
	if got := guardLines(g.stderr, "settings "); !slices.Equal(got, []string{at70, at60}) {
		t.Fatalf("after the next connection's acknowledgement the guard's settings lines are %q, want %q", got, []string{at70, at60})
	}
	kill("at an act share of 60%", guardLimit/100*65, first, second, third)
	report(again, third, 3)
	if got := guardLines(g.stderr, "started "); !slices.Equal(got, []string{started}) {
		t.Fatalf("the guard's start lines are %q, want its one start line %q", got, started)
	}

	rig.tickAfter(time.Hour)
	hours := guardLines(g.stderr, "hour ")
	if len(hours) != 1 || !strings.HasPrefix(hours[0], "launcher: memory guard: hour start=2026-11-14T03:00:00Z end=2026-11-14T04:00:00Z ") {
		t.Fatalf("the hourly lines are %q, want one for the hour from the guard's first reading", hours)
	}
}

// An acknowledgement that carries no settings, a daemon's that does not know the guard or a
// deployment's that turned it off, turns the controller pod's guard off with its line, and the
// guard ends nothing at the mark.
func TestControllerPodMemoryGuardTurnsOffOnAnAcknowledgementWithNoSettings(t *testing.T) {
	// Not parallel: it writes the package variable containerInit (useContainerInit), which every
	// launcher reads to tell whether it is the container's first process.
	const grower = 1660
	pod := newGuardPod(t)
	pod.add(guardProcess{pid: grower, ppid: os.Getpid(), exe: "/usr/bin/python3.13", args: []string{"python3", "grow.py"}, anon: 900 * guardMiB})
	rig := useGuardPod(t, pod, nil)
	g := runControllerLauncher(t)
	rig.started()
	g.acceptController(shimwire.LauncherHelloAck{})
	want := "launcher: memory guard: off: the daemon's acknowledgement carries no settings"
	if got := guardLines(g.stderr, "off"); !slices.Equal(got, []string{want}) {
		t.Fatalf("the guard's off lines are %q, want %q", got, want)
	}
	pod.setAnon(guardHigh)
	rig.tick()
	rig.tick()
	if kills := rig.signal.kills(); len(kills) != 0 {
		t.Fatalf("a guard the acknowledgement turned off killed %v at the mark, want nothing; its log:\n%s", kills, rig.log())
	}
}

// The pids of TestControllerPodMemoryGuardRunsInTheRolesDirFormAtTheControllerPodsSettings: the
// controller's generation under a role-init of its own, in the roles-dir form.
const (
	rolesDirCtlInit = 1680 + iota
	rolesDirCtlShim
	rolesDirCtlOmp
	rolesDirCtlKernel
	rolesDirCtlCommand
)

// Where the session database names a writer audience the controller's pod runs the launcher's
// roles-dir form, and the controller's generation runs under a role-init of its own. In gVisor
// that launcher, the container's first process, runs that form's guard over the controller's
// role-init (podClassifier), and the daemon's first answer to the controller's hello hands it the
// settings of runtime.kubernetes.memory_guard.controller_pod, here an act share of 70% with that
// block's other defaults, its wait of 1 second among them. The guard takes them in place, with one
// settings line, and acts at 75% of the limit, below the 85% it starts at. It ends, largest first,
// the Python kernel and then the shell command the controller's Oh My Pi started, and never the
// role-init, the shim or Oh My Pi, though each is larger than both; with only those three left it
// ends nothing more and writes its stopped line. Each kill is the controller's: its ended line
// reads role=controller, and its report goes on the controller's connection, in the guard's order.
func TestControllerPodMemoryGuardRunsInTheRolesDirFormAtTheControllerPodsSettings(t *testing.T) {
	// Not parallel: it writes the package variable containerInit (useContainerInit), which every
	// launcher reads to tell whether it is the container's first process.
	exe := launcherExe(t)
	launcher := os.Getpid()
	pod := newGuardPod(t)
	pod.addLauncher(exe, 0)
	pod.add(
		guardProcess{pid: rolesDirCtlInit, ppid: launcher, exe: exe, args: []string{"legion", "launcher", "role-init", "--role", "controller"}, anon: 1900 * guardMiB},
		guardProcess{pid: rolesDirCtlShim, ppid: rolesDirCtlInit, exe: exe, args: []string{"legion", shimArgument, "--connect", "tcp://daemon", "--", controllerOmp}, anon: 1800 * guardMiB},
		guardProcess{pid: rolesDirCtlOmp, ppid: rolesDirCtlShim, exe: controllerOmp, args: []string{controllerOmp, "--mode", "rpc"}, anon: 1700 * guardMiB},
		guardProcess{pid: rolesDirCtlKernel, ppid: rolesDirCtlOmp, exe: "/usr/bin/python3.13", args: []string{"python3", "-u", "/tmp/omp-python-kernel.py"}, anon: 900 * guardMiB},
		guardProcess{pid: rolesDirCtlCommand, ppid: rolesDirCtlOmp, exe: "/usr/bin/bash", args: []string{"bash", "-c", "make -j8"}, anon: 800 * guardMiB},
	)
	rig := useGuardPod(t, pod, nil)

	// The roles-dir form for the controller alone, built as Run builds it (newManagers), with the
	// controller's generation running: its role-init is the manager's child.
	rolesDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rolesDir, "controller", RoleLauncherDir), 0o755); err != nil {
		t.Fatal(err)
	}
	token := ownToken("launcher-token-controller")
	if err := os.WriteFile(filepath.Join(rolesDir, "controller", RoleLauncherDir, LauncherTokenFile), []byte(token+"\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	listener := listenForLauncher(t, token)
	stderr := &lockedBuffer{}
	cfg := Config{
		Connect: "tcp://" + listener.Addr().String(), Sandbox: "legion-legion-controller", PodUID: "pod-1",
		RolesDir: rolesDir, Roles: []string{"controller"}, Stdout: stderr, Stderr: stderr, StopGrace: 2 * time.Second,
	}
	pids := &pidGuard{}
	managers, err := newManagers(cfg, pids)
	if err != nil {
		t.Fatal(err)
	}
	m := managers[0]
	m.mu.Lock()
	m.child = &child{generation: 1, pid: rolesDirCtlInit}
	m.mu.Unlock()
	// The guard and the controller's connection, as Run's roles-dir branch serves them. The
	// connection is served once (serve, not serveUntil), so its end stops no generation: this one
	// is no process of the test's.
	ctx, cancel := context.WithCancel(context.Background())
	guarded := serveMemoryGuard(ctx, cfg, managers, pids)
	served := make(chan struct{})
	go func() {
		defer close(served)
		listener.codeReturned(m.serve(ctx, listener.Addr().String()))
	}()
	t.Cleanup(func() {
		cancel()
		for what, done := range map[string]<-chan struct{}{"the controller's connection": served, "the memory guard": guarded} {
			select {
			case <-done:
			case <-time.After(cleanupBeforeDeadline(t)):
				t.Errorf("%s did not return after its context ended; the launcher's log: %s", what, stderr.String())
			}
		}
	})
	rig.started()
	conn := acceptRole(t, listener)
	if conn.hello.Role != "controller" || conn.hello.Token != token {
		t.Fatalf("hello = %#v, want the controller's from its roles directory", conn.hello)
	}
	conn.acknowledgeWith(memoryGuardAck(70, shimwire.MemoryGuardFloorDefault, shimwire.MemoryGuardControllerWaitDefault, shimwire.MemoryGuardIntervalDefault))
	at70 := "launcher: memory guard: settings act_percent=70 floor_percent=10 wait=1s interval=100ms"
	if got := guardLines(stderr, "settings "); !slices.Equal(got, []string{at70}) {
		t.Fatalf("after the controller's first acknowledgement the guard's settings lines are %q, want %q; the launcher's log:\n%s", got, at70, stderr.String())
	}

	rise := func(want ...int) {
		t.Helper()
		pod.setAnon(guardLimit / 100 * 75)
		rig.tick()
		if kills := rig.signal.kills(); !slices.Equal(kills, want) {
			t.Fatalf("at 75%% of the limit the guard killed %v, want %v and never the role-init, the shim or Oh My Pi; its log:\n%s", kills, want, rig.log())
		}
		pod.setAnon(guardLow)
		rig.tick()
	}
	rise(rolesDirCtlKernel)
	rise(rolesDirCtlKernel, rolesDirCtlCommand)
	rise(rolesDirCtlKernel, rolesDirCtlCommand)
	if stopped := guardLines(stderr, "stopped for this rise reason=nothing to end "); len(stopped) != 1 || !strings.Contains(stopped[0], " endable=0 ") {
		t.Fatalf("the stopped lines are %q, want one with nothing it may end; its log:\n%s", stopped, rig.log())
	}
	ended := guardLines(stderr, "ended ")
	for i, pid := range []int{rolesDirCtlKernel, rolesDirCtlCommand} {
		if r := conn.nextMemoryKill(); r.PID != pid || r.Sequence != uint64(i+1) || r.Guard != rig.id() {
			t.Errorf("the controller's connection carries %+v as its report %d, want pid %d, sequence %d of guard %s", r, i+1, pid, i+1, rig.id())
		}
		if want := fmt.Sprintf("launcher: memory guard: ended pid=%d role=controller ", pid); i >= len(ended) || !strings.HasPrefix(ended[i], want) {
			t.Errorf("the guard's ended lines are %q, want line %d to start %q", ended, i, want)
		}
	}
}

// A panic on the controller pod's guard's loop, here in its Config.Observe at a reading at the
// mark, turns the guard off for good with one off line that names the panic and where it was
// raised, and the launcher, its container's first process, serves on: the guard ends nothing and
// asks for no next reading, the launcher takes its next connection, whose acknowledgement's
// settings reach no guard, and Run returns at its context's end (runControllerLauncher). A panic
// that left the guard's goroutine would end this test's process, as it ends a pod's PID 1.
func TestControllerPodMemoryGuardTurnsOffOnAPanicInItsLoopAndTheLauncherServesOn(t *testing.T) {
	// Not parallel: it writes the package variable containerInit (useContainerInit), which every
	// launcher reads to tell whether it is the container's first process.
	const grower = 1670
	pod := newGuardPod(t)
	pod.add(guardProcess{pid: grower, ppid: os.Getpid(), exe: "/usr/bin/python3.13", args: []string{"python3", "grow.py"}, anon: 900 * guardMiB})
	rig := useGuardPod(t, pod, nil)
	var panicking atomic.Bool
	built := newMemoryGuard
	newMemoryGuard = func(cfg memoryguard.Config) *memoryguard.Guard {
		cfg.Observe = func(time.Time, uint64, uint64) {
			if panicking.Load() {
				panic("a test's Observe panics")
			}
		}
		return built(cfg)
	}
	t.Cleanup(func() { newMemoryGuard = built })
	g := runControllerLauncher(t)
	rig.started()
	g.acceptController(controllerDefaultsAck())
	panicking.Store(true)
	pod.setAnon(guardHigh)
	// The guard takes the reading and turns off; it asks the clock for no next reading, which
	// rig.tick waits for, so the test lets it take this one reading alone.
	w := rig.pending
	w.fire <- rig.clock.advance(w.d)
	if !testwait.Holds(beforeDeadline(t), testwait.Poll, func() bool { return len(guardLines(g.stderr, "off ")) > 0 }) {
		t.Fatalf("the guard wrote no off line after its Observe panicked; the launcher's log:\n%s", g.stderr.String())
	}
	want := "launcher: memory guard: off reason=panic: a test's Observe panics at "
	if off := guardLines(g.stderr, "off "); len(off) != 1 || !strings.HasPrefix(off[0], want) || !strings.Contains(off[0], "memoryguard_test.go:") {
		t.Fatalf("the guard's off lines are %q, want one starting %q that names this test's file", off, want)
	}
	if kills := rig.signal.kills(); len(kills) != 0 {
		t.Fatalf("a guard whose loop panicked killed %v at the mark, want nothing; its log:\n%s", kills, rig.log())
	}
	g.dropConnection()
	g.acceptController(memoryGuardAck(70, 10, 1, 100))
	if got := guardLines(g.stderr, "settings "); len(got) != 0 {
		t.Fatalf("after the panic the next acknowledgement logged the settings lines %q, want none: the guard is off for good", got)
	}
	select {
	case next := <-rig.clock.waits:
		t.Fatalf("the guard asked for its next reading in %s after its loop panicked, want none", next.d)
	default:
	}
}

// waitingKillReport builds the tester's manager of an issue pod's launcher as Run builds it
// (newManagers), from a configuration that says whether the pod is a microVM's, and queues report
// on it as the pod's watch does while no connection is up (queueMemoryKill). The manager then
// serves its connections to a listener of the test's; the test gets the daemon's end of the
// first, up to its hello, and the launcher's log.
func waitingKillReport(t *testing.T, microVM bool, report shimwire.LauncherMemoryKill) (*roleConn, *lockedBuffer) {
	t.Helper()
	rolesDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rolesDir, "tester", RoleLauncherDir), 0o755); err != nil {
		t.Fatal(err)
	}
	token := ownToken("launcher-token-tester")
	if err := os.WriteFile(filepath.Join(rolesDir, "tester", RoleLauncherDir, LauncherTokenFile), []byte(token+"\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	return servedKillReport(t, token, Config{Sandbox: "legion-legion-legion-208", RolesDir: rolesDir, Roles: []string{"tester"}, MicroVM: microVM}, report)
}

// waitingControllerKillReport is waitingKillReport for the controller pod's one role: the
// single-role form's manager as Run builds it, whose lines carry no role's prefix.
func waitingControllerKillReport(t *testing.T, microVM bool, report shimwire.LauncherMemoryKill) (*roleConn, *lockedBuffer) {
	t.Helper()
	token := ownToken("launcher-token")
	return servedKillReport(t, token, Config{
		Sandbox: "legion-legion-controller", Token: token, Role: "controller", PrivateDir: shortTempDir(t), MicroVM: microVM,
	}, report)
}

// servedKillReport is the rest of waitingKillReport and waitingControllerKillReport: it builds
// cfg's first manager, with a daemon end that takes its hello under token, the pod, the log and the
// stop grace every such test's is given, queues report on it and serves it until the test's end.
func servedKillReport(t *testing.T, token string, cfg Config, report shimwire.LauncherMemoryKill) (*roleConn, *lockedBuffer) {
	t.Helper()
	listener := listenForLauncher(t, token)
	stderr := &lockedBuffer{}
	cfg.Connect, cfg.PodUID, cfg.Stdout, cfg.Stderr, cfg.StopGrace = "tcp://"+listener.Addr().String(), "pod-1", stderr, stderr, 2*time.Second
	managers, err := newManagers(cfg, &pidGuard{})
	if err != nil {
		t.Fatal(err)
	}
	m := managers[0]
	m.queueMemoryKill(report)
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan struct{})
	go func() {
		defer close(served)
		listener.codeReturned(m.serveUntil(ctx, listener.Addr().String()))
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-served:
		case <-time.After(cleanupBeforeDeadline(t)):
			t.Errorf("the %s's manager did not return after its context ended; its log: %s", m.role, stderr.String())
		}
	})
	return acceptRole(t, listener), stderr
}

// A microVM pod's launcher opens its kill reports on a connection whose acknowledgement carries
// kernelKills, whatever it says of the guard, so a deployment that turned the guard off still
// tells its microVM roles of the kernel's kills; it drops them on one without kernelKills, a
// daemon that does not know the report or, with guard settings, one that would tell a role of the
// kernel's kill in the guard's words. A gVisor pod's launcher keeps the guard's rule on the same
// acknowledgements: it sends its report where guard settings came and drops it where only
// kernelKills did, with the line it always wrote.
func TestAMicroVMLauncherSendsItsKillReportsOnKernelKillsAndAGVisorLauncherOnGuardSettings(t *testing.T) {
	t.Parallel()
	const noKernelKills = "reason=this connection's acknowledgement carries no kernelKills, so the daemon behind it may not know the report"
	const noSettings = "reason=this connection's acknowledgement carries no memory guard settings, so the daemon behind it may not know the report"
	both := defaultsAck()
	both.KernelKills = true
	for _, tc := range []struct {
		name    string
		microVM bool
		ack     shimwire.LauncherHelloAck
		dropped string
	}{
		{"a microVM launcher on kernelKills and no guard settings", true, shimwire.LauncherHelloAck{KernelKills: true}, ""},
		{"a microVM launcher on kernelKills and guard settings", true, both, ""},
		{"a microVM launcher on guard settings and no kernelKills", true, defaultsAck(), noKernelKills},
		{"a microVM launcher on an older daemon's acknowledgement", true, shimwire.LauncherHelloAck{}, noKernelKills},
		{"a gVisor launcher on kernelKills and no guard settings", false, shimwire.LauncherHelloAck{KernelKills: true}, noSettings},
		{"a gVisor launcher on kernelKills and guard settings", false, both, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report := shimwire.LauncherMemoryKill{Guard: "0123456789abcdef", Sequence: 1, PID: 900, Executable: "python3",
				SizeBytes: 600 * guardMiB, ReadingBytes: 3500 * guardMiB, CounterBytes: 3900 * guardMiB, LimitBytes: 4096 * guardMiB,
				At: time.Date(2026, 11, 15, 6, 0, 0, 0, time.UTC)}
			if tc.microVM {
				report.Cause = shimwire.MemoryKillCauseKernel
			}
			conn, stderr := waitingKillReport(t, tc.microVM, report)
			conn.acknowledgeWith(tc.ack)
			if tc.dropped == "" {
				if got := conn.nextMemoryKill(); got != report {
					t.Fatalf("the connection carries the report %+v, want the one that waited for it, %+v", got, report)
				}
				conn.noMemoryKill("after the report that waited for it, the connection")
				if strings.Contains(stderr.String(), "report dropped") {
					t.Fatalf("the launcher logged %s, want no report dropped", stderr.String())
				}
				return
			}
			conn.noMemoryKill("a connection whose acknowledgement does not say the daemon knows the report")
			line := "tester: launcher: memory guard: report dropped sequence=1 pid=900 " + tc.dropped
			if !strings.Contains(stderr.String(), line) {
				t.Fatalf("the launcher logged %s, want the line %q", stderr.String(), line)
			}
		})
	}
}

// noGuard has every launcher this test starts fail it if it builds the memory guard.
func noGuard(t *testing.T, why string) {
	t.Helper()
	previous := newMemoryGuard
	newMemoryGuard = func(memoryguard.Config) *memoryguard.Guard { panic(why) }
	t.Cleanup(func() { newMemoryGuard = previous })
}

// In a microVM pod's launcher the guest watch runs over the pod's processes with the launcher's own
// classifier, the guard's: one scan raises to 1000 the language server the LSP multiplexer runs
// under the broker helper, the eval kernel a helper started, the agent's command and a helper's
// name under that command, and the launcher, the role-init, the shim, the agent and Oh My Pi's two
// helpers keep the container's starting score. The launcher builds no guard, and its manager holds
// none.
func TestAMicroVMLauncherRaisesTheAgentsWorkAndKeepsItsOwnProcessesAtTheStartingScore(t *testing.T) {
	// Not parallel: it writes the package variable containerInit (useContainerInit), which every
	// launcher reads to tell whether it is the container's first process.
	exe := launcherExe(t)
	launcher := os.Getpid()
	const omp = "/opt/omp/bin/omp"
	const start = 200
	const roleInit, shim, agent, broker, mux, gopls, eval, command, fakeMux = 730, 731, 732, 733, 734, 735, 736, 737, 738
	pod := newGuardPod(t)
	pod.addLauncher(exe, 0)
	pod.add(
		guardProcess{pid: roleInit, ppid: launcher, exe: exe, args: []string{"legion", "launcher", "role-init", "--role", "tester"}, ns: testerNamespace},
		guardProcess{pid: shim, ppid: roleInit, exe: exe, args: []string{"legion", "worker-shim", "--connect", "tcp://daemon"}, ns: testerNamespace},
		guardProcess{pid: agent, ppid: shim, exe: omp, args: []string{omp, "--extension", "/opt/legion/pi-envoy"}, ns: testerNamespace, anon: 2 * guardGiB},
		guardProcess{pid: broker, ppid: agent, exe: omp, args: []string{omp, "__omp_worker_daemon_broker"}, ns: testerNamespace},
		guardProcess{pid: mux, ppid: broker, exe: omp, args: []string{omp, "__omp_worker_lsp_mux"}, ns: testerNamespace},
		guardProcess{pid: gopls, ppid: mux, exe: "/usr/local/bin/gopls", args: []string{"/usr/local/bin/gopls", "serve"}, ns: testerNamespace},
		guardProcess{pid: eval, ppid: broker, exe: omp, args: []string{omp, ompEvalKernel}, ns: testerNamespace},
		guardProcess{pid: command, ppid: agent, exe: "/usr/bin/bash", args: []string{"bash", "-c", "make"}, ns: testerNamespace},
		guardProcess{pid: fakeMux, ppid: command, exe: omp, args: []string{omp, "__omp_worker_lsp_mux"}, ns: testerNamespace},
	)
	pod.setSelfScore(start)
	for _, pid := range []int{launcher, roleInit, shim, agent, broker, mux, gopls, eval, command, fakeMux} {
		pod.setScore(pid, start, 0)
	}
	noGuard(t, "a microVM pod's launcher built a memory guard")
	rig := useGuestPod(t, pod, nil)
	log := &roleLog{stderr: &lockedBuffer{}}
	_, own := log.writers("tester")
	tester := &manager{role: "tester", log: log, stderr: own, child: &child{pid: roleInit}}
	ctx, cancel := context.WithCancel(context.Background())
	done := serveMemoryGuard(ctx, Config{RolesDir: t.TempDir(), MicroVM: true}, []*manager{tester}, &pidGuard{})
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(cleanupBeforeDeadline(t)):
			t.Error("the guest watch did not return after its context ended")
		}
	})
	rig.started()
	for _, tc := range []struct {
		pid, want int
		what      string
	}{
		{gopls, 1000, "the language server the multiplexer runs"},
		{eval, 1000, "an eval kernel a helper started"},
		{command, 1000, "a command the agent started"},
		{fakeMux, 1000, "a helper's name under the agent's command"},
		{launcher, start, "the launcher"},
		{roleInit, start, "the role-init"},
		{shim, start, "the shim"},
		{agent, start, "Oh My Pi, the shim's child"},
		{broker, start, "the broker helper the agent started"},
		{mux, start, "the LSP multiplexer helper the broker started"},
	} {
		if got := pod.score(tc.pid); got != tc.want {
			t.Errorf("%s (pid %d) reads oom_score_adj %d after one scan, want %d; the watch's log:\n%s", tc.what, tc.pid, got, tc.want, rig.log())
		}
	}
	if tester.memoryGuard != nil {
		t.Fatalf("the tester's manager holds the guard %v in a microVM pod's launcher, want none", tester.memoryGuard)
	}
}

// A microVM pod's launcher never builds the memory guard, whatever its acknowledgements say: it
// logs one off line, and an acknowledgement's guard settings reach no guard, so no settings line
// follows. Its guest watch reports the kernel's kill of the tester's process, which shows the kill
// with SIGKILL pending in its status, to the tester's manager, which sends it, with the kernel's
// cause, on the tester's connection, whose acknowledgement carries kernelKills; the reviewer's
// connection, which carries kernelKills too, gets nothing. The kill of a process of no role in the
// same rise, which the watch hands the launcher's sink with no role, has its line and reaches no
// connection: neither acknowledgement carries noRoleKills, so the launcher drops its report with
// a line of its own.
func TestAMicroVMLauncherRunsNoGuardAndTellsTheKilledProcesssRoleAlone(t *testing.T) {
	// Not parallel: it writes the package variable containerInit (useContainerInit), which every
	// launcher reads to tell whether it is the container's first process.
	const testers, reviewers, outside = 750, 751, 752
	pod := newGuardPod(t)
	pod.add(
		guardProcess{pid: testers, ppid: 1, exe: "/usr/bin/python3.13", args: []string{"python3", "grow.py"}, resident: 900 * guardMiB},
		guardProcess{pid: reviewers, ppid: 1, exe: "/usr/bin/python3.13", args: []string{"python3", "lint.py"}, resident: 300 * guardMiB},
		guardProcess{pid: outside, ppid: 1, exe: "/usr/bin/python3.13", args: []string{"python3", "outside.py"}, resident: 200 * guardMiB},
	)
	pod.setSelfScore(0)
	pod.setScore(testers, 0, 1400)
	pod.setScore(reviewers, 0, 1100)
	pod.setScore(outside, 0, 1000)
	pod.setKernelKills(0, guardGiB, 32*guardMiB)
	noGuard(t, "a microVM pod's launcher built a memory guard")
	rig := useGuestPod(t, pod, map[int]string{testers: "tester", reviewers: "reviewer"})
	f := newFleetDialedOf(t, Config{StopGrace: 2 * time.Second, MicroVM: true}, "tester", "reviewer")
	rig.started()
	kernelKills := func(ack shimwire.LauncherHelloAck) shimwire.LauncherHelloAck {
		ack.KernelKills = true
		return ack
	}
	f.role("tester").acknowledgeWith(kernelKills(memoryGuardAck(90, 20, 7, 250)))
	f.role("reviewer").acknowledgeWith(kernelKills(defaultsAck()))
	off := "launcher: memory guard: off reason=the pod runs in a microVM, whose own kernel stops one process"
	if got := guardLines(f.stderr, ""); !slices.Equal(got, []string{off}) {
		t.Fatalf("the launcher's memory guard lines are %q, want %q alone", got, off)
	}
	if !strings.Contains(f.stderr.String(), "\nlauncher: guest memory: start cgroup=v1 dir="+pod.cgroup+" ") {
		t.Fatalf("the launcher logged %s, want the guest watch's start line", f.stderr.String())
	}

	pod.showKernelKill(testers)
	pod.showKernelKill(outside)
	pod.setKernelKills(2, 3500*guardMiB, 64*guardMiB)
	rig.tick()
	at := rig.clock.Now()
	report := f.role("tester").nextMemoryKill()
	want := shimwire.LauncherMemoryKill{
		Guard: rig.id(), Sequence: 1, PID: testers, Executable: "python3.13", SizeBytes: 900 * guardMiB,
		ReadingBytes: 3500 * guardMiB, CounterBytes: guardCounter, LimitBytes: guardLimit, Cause: shimwire.MemoryKillCauseKernel,
	}
	if !report.At.Equal(at) {
		t.Fatalf("the tester's report is at %s, want the scan that saw the kill, at %s", report.At, at)
	}
	report.At = time.Time{}
	if report != want {
		t.Fatalf("the tester's report is %+v, want %+v", report, want)
	}
	if err := report.Validate(); err != nil {
		t.Fatal(err)
	}
	f.role("reviewer").noMemoryKill("after the kernel killed the tester's process and one of no role, the reviewer's connection")
	f.role("tester").noMemoryKill("after its one report, the tester's connection")
	dropped := noRoleDropped(2, outside)
	testwait.Eventually(t, "the launcher's line "+dropped, func() bool {
		return slices.Contains(guardLines(f.stderr, "report dropped"), dropped)
	})
	ended := []string{off,
		fmt.Sprintf(`launcher: memory guard: ended pid=%d role=tester size=%d reading=%d counter=%d shmem=%d limit=%d rule=kernel sizes=kernel less_parent=0 new_read=- executable="python3.13"`,
			testers, 900*guardMiB, 3500*guardMiB, guardCounter, 64*guardMiB, guardLimit),
		fmt.Sprintf(`launcher: memory guard: ended pid=%d role=none size=%d reading=%d counter=%d shmem=%d limit=%d rule=kernel sizes=kernel less_parent=0 new_read=- executable="python3.13"`,
			outside, 200*guardMiB, 3500*guardMiB, guardCounter, 64*guardMiB, guardLimit),
		dropped,
	}
	if got := guardLines(f.stderr, ""); !slices.Equal(got, ended) {
		t.Fatalf("the launcher's memory guard lines are %q, want %q", got, ended)
	}
}

// A microVM pod's launcher reports the kernel's kill of a process of no role, with the kernel's
// cause and marked as of no role, on a connection whose acknowledgement carries noRoleKills beside
// kernelKills, and never on one whose acknowledgement carries kernelKills alone: in one rise the
// kernel stops a process of the tester's and one of no role, the tester's connection, which lacks
// noRoleKills, carries the tester's own report alone, and the reviewer's carries the one of no
// role. Both kills have the watch's own `ended` line as before, and no report is dropped.
func TestAMicroVMLauncherReportsTheKernelsKillOfNoRoleOnAConnectionThatKnowsIt(t *testing.T) {
	// Not parallel: it writes the package variable containerInit (useContainerInit), which every
	// launcher reads to tell whether it is the container's first process.
	const testers, outside = 760, 761
	pod := newGuardPod(t)
	pod.add(
		guardProcess{pid: testers, ppid: 1, exe: "/usr/bin/python3.13", args: []string{"python3", "grow.py"}, resident: 900 * guardMiB},
		guardProcess{pid: outside, ppid: 1, exe: "/usr/bin/python3.13", args: []string{"python3", "outside.py"}, resident: 200 * guardMiB},
	)
	pod.setSelfScore(0)
	pod.setScore(testers, 0, 1400)
	pod.setScore(outside, 0, 1000)
	pod.setKernelKills(0, guardGiB, 32*guardMiB)
	noGuard(t, "a microVM pod's launcher built a memory guard")
	rig := useGuestPod(t, pod, map[int]string{testers: "tester"})
	f := newFleetDialedOf(t, Config{StopGrace: 2 * time.Second, MicroVM: true}, "tester", "reviewer")
	rig.started()
	f.role("tester").acknowledgeWith(shimwire.LauncherHelloAck{KernelKills: true})
	f.role("reviewer").acknowledgeWith(shimwire.LauncherHelloAck{KernelKills: true, NoRoleKills: true})
	for _, role := range []string{"tester", "reviewer"} {
		// A connection answers a stop only once it serves commands, which is after its reports
		// opened on it: both connections are up before the kernel stops anything.
		f.role(role).noMemoryKill("before any kill, the " + role + "'s connection")
	}

	pod.showKernelKill(testers)
	pod.showKernelKill(outside)
	pod.setKernelKills(2, 3500*guardMiB, 64*guardMiB)
	rig.tick()
	at := rig.clock.Now()
	if report := f.role("tester").nextMemoryKill(); report.PID != testers || report.Sequence != 1 || report.NoRole ||
		report.Cause != shimwire.MemoryKillCauseKernel {
		t.Fatalf("the tester's connection carries %+v, want its own process %d with the kernel's cause, unmarked", report, testers)
	}
	report := f.role("reviewer").nextMemoryKill()
	want := shimwire.LauncherMemoryKill{
		Guard: rig.id(), Sequence: 2, PID: outside, Executable: "python3.13", SizeBytes: 200 * guardMiB,
		ReadingBytes: 3500 * guardMiB, CounterBytes: guardCounter, LimitBytes: guardLimit, At: at,
		Cause: shimwire.MemoryKillCauseKernel, NoRole: true,
	}
	if !report.At.Equal(at) {
		t.Fatalf("the report of no role is at %s, want the scan that saw the kill, at %s", report.At, at)
	}
	report.At = at
	if report != want {
		t.Fatalf("the reviewer's connection carries %+v, want the report of no role %+v", report, want)
	}
	if err := report.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"tester", "reviewer"} {
		f.role(role).noMemoryKill("after the two reports, the " + role + "'s connection")
	}
	if got := guardLines(f.stderr, "report dropped"); len(got) != 0 {
		t.Fatalf("the launcher logged %q, want no report dropped", got)
	}
	if got := guardLines(f.stderr, "ended "); len(got) != 2 || !strings.Contains(got[1], fmt.Sprintf("ended pid=%d role=none ", outside)) {
		t.Fatalf("the launcher's kill lines are %q, want the tester's and then the one of pid %d in role none", got, outside)
	}
}

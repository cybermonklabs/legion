package launcher

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
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
	p.writeFile(filepath.Join(p.cgroup, "memory.limit_in_bytes"), strconv.FormatUint(guardLimit, 10)+"\n")
	p.writeFile(filepath.Join(p.cgroup, "memory.usage_in_bytes"), strconv.FormatUint(guardCounter, 10)+"\n")
	p.setAnon(guardLow)
	return p
}

// setAnon writes the pod's AnonPages.
func (p *guardPod) setAnon(anon uint64) {
	p.writeFile(filepath.Join(p.proc, "meminfo"), fmt.Sprintf(
		"MemTotal:       20971520 kB\nMemFree:        10485760 kB\nAnonPages:      %d kB\nMapped:          520372 kB\nShmem:              32 kB\n", anon>>10))
}

func (p *guardPod) writeFile(path, content string) {
	p.t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		p.t.Fatal(err)
	}
}

// guardProcess is one process of a guardPod: its place in the tree, its executable and arguments,
// its PID namespace, its working directory when it matters, the anonymous memory its smaps
// reads, and its resident size, which its stat's rss field holds in pages, the size the guest
// watch reads of it.
type guardProcess struct {
	pid, ppid int
	exe       string
	args      []string
	ns        string
	cwd       string
	anon      uint64
	resident  uint64
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
	p.writeWhole(filepath.Join(dir, "smaps"), fmt.Sprintf(
		"01a5c000-41a5c000 rw-p 00000000 00:00 0                                  [heap]\nSize:           %d kB\nRss:            %d kB\nAnonymous:      %d kB\n",
		gp.anon>>10, gp.anon>>10, gp.anon>>10))
	links := []struct{ name, target string }{{"exe", gp.exe}, {filepath.Join("ns", "pid"), gp.ns}}
	if gp.cwd != "" {
		links = append(links, struct{ name, target string }{"cwd", gp.cwd})
	}
	for _, link := range links {
		path := filepath.Join(dir, link.name)
		_ = os.Remove(path)
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
// kill that failed, which the guard logs.
type guardSignal struct {
	proc   string
	mu     sync.Mutex
	killed []int
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
	return nil
}

func (h *guardHandle) Exited() bool {
	h.s.mu.Lock()
	defer h.s.mu.Unlock()
	return h.exited
}

func (h *guardHandle) Close() error { return nil }

// guardRig is the guard or the guest watch a launcher builds, pointed at a guardPod with a
// guardClock and, for the guard, a guardSignal, one reading or scan at a time.
type guardRig struct {
	t       *testing.T
	pod     *guardPod
	clock   *guardClock
	signal  *guardSignal
	mu      sync.Mutex
	guard   *memoryguard.Guard
	guest   *memoryguard.Guest
	lines   []string
	pending guardWait
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
	useContainerInit(t, true)
	previousTrimmer := podTrimmer
	podTrimmer = func(ctx context.Context, _ *trimmer) { <-ctx.Done() }
	t.Cleanup(func() { podTrimmer = previousTrimmer })
	rig := &guardRig{t: t, pod: pod, clock: newGuardClock(), signal: &guardSignal{proc: pod.proc}}
	previous := newMemoryGuard
	newMemoryGuard = func(cfg memoryguard.Config) *memoryguard.Guard {
		cfg.Proc, cfg.Cgroup, cfg.Clock, cfg.Signal = pod.proc, pod.cgroup, rig.clock, rig.signal
		cfg.Classify, cfg.Logf = withRoles(cfg.Classify, roles), rig.keep(cfg.Logf)
		guard := memoryguard.New(cfg)
		rig.mu.Lock()
		rig.guard = guard
		rig.mu.Unlock()
		return guard
	}
	t.Cleanup(func() { newMemoryGuard = previous })
	return rig
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
	case <-time.After(testwait.Bounded(r.t, time.Minute, time.Second)):
		r.t.Fatalf("the guard never waited for its next reading; its log:\n%s", r.log())
	}
	return guardWait{}
}

// tick lets the guard take its next reading, and returns once that reading and all it led to,
// a kill and its report to the role's manager among them, are done.
func (r *guardRig) tick() {
	r.t.Helper()
	w := r.pending
	w.fire <- r.clock.advance(w.d)
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
	case <-time.After(testwait.Bounded(t, time.Minute, time.Second)):
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

// Each process is the role's whose role-init is its ancestor, then the role's whose role-init
// shares its PID namespace, and no role's otherwise: a process in a PID namespace of its own under
// a role-init is that role's by its ancestor, and one reparented out of the role-init's subtree is
// the role's by its namespace.
func TestMemoryGuardGivesEachProcessItsRoleByAncestorThenByNamespace(t *testing.T) {
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
		case <-time.After(10 * time.Second):
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

// defaultsAck is an acknowledgement that carries the settings the guard starts with.
func defaultsAck() shimwire.LauncherHelloAck {
	return memoryGuardAck(shimwire.MemoryGuardActDefault, shimwire.MemoryGuardFloorDefault,
		shimwire.MemoryGuardWaitDefault, shimwire.MemoryGuardIntervalDefault)
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
// carries none of it, and a kill of a process in no role is reported nowhere.
func TestMemoryGuardReportsAKillOnTheConnectionOfTheProcesssRoleAlone(t *testing.T) {
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
}

// dropConnection ends role's connection from the daemon's end, and returns once the launcher has
// seen it go: it logs the reconnect only after the connection's end has taken it out of use.
func (f *fleet) dropConnection(role string) {
	f.t.Helper()
	ended := func() int { return strings.Count(f.stderr.String(), role+": launcher: daemon connection: ") }
	before := ended()
	_ = f.role(role).conn.Close()
	if !testwait.Holds(testwait.Bounded(f.t, time.Minute, time.Second), testwait.Poll, func() bool { return ended() > before }) {
		f.t.Fatalf("the launcher never saw the %s's connection end; its log: %s", role, f.stderr.String())
	}
}

// A report made while the role's connection is down waits for the next one, and is sent on it
// after its state when its acknowledgement carries settings; a report waiting when an
// acknowledgement without settings arrives is dropped with a line, since the daemon behind it may
// not know the report and would close the connection on it.
func TestMemoryGuardHoldsAReportWhileTheConnectionIsDown(t *testing.T) {
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
		m.openMemoryReports(shimwire.NewWriter(conn), true)
	}()
	bound := testwait.Bounded(t, time.Minute, time.Second)
	select {
	case <-conn.writing:
	case <-time.After(bound):
		t.Fatal("the role's sender never began to write the oldest report")
	}
	m.queueMemoryKill(report(maxWaitingMemoryKills + 1))
	close(conn.release)
	select {
	case <-sent:
	case <-time.After(bound):
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
	if err := os.WriteFile(filepath.Join(rolesDir, "tester", RoleLauncherDir, LauncherTokenFile), []byte(roleToken("tester")+"\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	listener := listenForLauncher(t, roleToken("tester"))
	stderr := &lockedBuffer{}
	managers, err := newManagers(Config{
		Connect: "tcp://" + listener.Addr().String(), Sandbox: "legion-legion-legion-208", PodUID: "pod-1",
		RolesDir: rolesDir, Roles: []string{"tester"}, MicroVM: microVM, Stdout: stderr, Stderr: stderr, StopGrace: 2 * time.Second,
	}, &pidGuard{})
	if err != nil {
		t.Fatal(err)
	}
	tester := managers[0]
	tester.queueMemoryKill(report)
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan struct{})
	go func() {
		defer close(served)
		listener.launcherReturned(tester.serveUntil(ctx, listener.Addr().String()))
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-served:
		case <-time.After(30 * time.Second):
			t.Errorf("the tester's manager did not return after its context ended; its log: %s", stderr.String())
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
		case <-time.After(10 * time.Second):
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
// connection.
func TestAMicroVMLauncherRunsNoGuardAndTellsTheKilledProcesssRoleAlone(t *testing.T) {
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
	ended := []string{off,
		fmt.Sprintf(`launcher: memory guard: ended pid=%d role=tester size=%d reading=%d counter=%d shmem=%d limit=%d executable="python3.13"`,
			testers, 900*guardMiB, 3500*guardMiB, guardCounter, 64*guardMiB, guardLimit),
		fmt.Sprintf(`launcher: memory guard: ended pid=%d role=none size=%d reading=%d counter=%d shmem=%d limit=%d executable="python3.13"`,
			outside, 200*guardMiB, 3500*guardMiB, guardCounter, 64*guardMiB, guardLimit),
	}
	if got := guardLines(f.stderr, ""); !slices.Equal(got, ended) {
		t.Fatalf("the launcher's memory guard lines are %q, want %q", got, ended)
	}
}

package launcher

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sjawhar/legion/daemon/internal/procgroup"
)

// A deployed launcher is PID 1 in its role's private PID namespace. kill(-1) there includes
// setsid/daemonized descendants but cannot reach another role. Standalone host invocations keep
// their existing process-group scope; they must never signal the host's whole process population.
func signalGeneration(pid int, sig syscall.Signal) error {
	target := -pid
	if os.Getpid() == 1 {
		target = -1
	}
	if err := syscall.Kill(target, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("signal role processes: %w", err)
	}
	return nil
}

// procPIDs is every process the /proc mounted at root names, in the order os.ReadDir answers
// them: /proc names a process by its decimal pid, so a numeric entry is a process and nothing
// else there is one. The error is os.ReadDir's own, for each caller to word as its own failure,
// and each keeps whatever further filtering it does of the pids.
func procPIDs(root string) ([]int, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	pids := make([]int, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if name == "" || name[0] < '0' || name[0] > '9' {
			continue
		}
		pid, err := strconv.Atoi(name)
		if err != nil {
			continue
		}
		pids = append(pids, pid)
	}
	return pids, nil
}

// namespaceProcesses is one listing of the /proc of PID 1's namespace, split by the guard's
// children, so what a pass spares and what it signals or reaps come from the same read: a process
// a held child forks after it is in neither, rather than in the second and not the first.
type namespaceProcesses struct {
	// pids is every process the listing found.
	pids []int
	// spared is the guard's children and every process of theirs: a descendant by its parent
	// chain (a held child's own children, as the install's --help run starts python), or a member
	// of a process group a held child leads (procgroup.Configure gives each of the launcher's
	// guarded commands one).
	spared map[int]bool
	// others is every process of the listing but PID 1 and the spared whose stat was read; one
	// whose stat could not be read, gone in between, is in neither.
	others []int
}

// listNamespace is one listing of the /proc mounted at root, split by held (namespaceProcesses).
// With nothing held it reads no process's stat: spared is empty and others is every pid but 1.
func listNamespace(root string, held []int) (namespaceProcesses, error) {
	pids, err := procPIDs(root)
	if err != nil {
		return namespaceProcesses{}, err
	}
	listed := namespaceProcesses{pids: pids, spared: make(map[int]bool, len(held))}
	for _, pid := range held {
		listed.spared[pid] = true
	}
	if len(held) == 0 {
		for _, pid := range pids {
			if pid > 1 {
				listed.others = append(listed.others, pid)
			}
		}
		return listed, nil
	}
	parent := make(map[int]int, len(pids))
	for _, pid := range pids {
		ppid, pgid, ok := statParentAndGroup(root, pid)
		if !ok {
			continue
		}
		parent[pid] = ppid
		if listed.spared[pgid] {
			listed.spared[pid] = true
		}
	}
	for _, pid := range pids {
		for ancestor, steps := parent[pid], 0; ancestor > 1 && steps < len(pids); ancestor, steps = parent[ancestor], steps+1 {
			if listed.spared[ancestor] {
				listed.spared[pid] = true
				break
			}
		}
	}
	for _, pid := range pids {
		if _, read := parent[pid]; read && pid > 1 && !listed.spared[pid] {
			listed.others = append(listed.others, pid)
		}
	}
	return listed, nil
}

// statParentAndGroup is pid's parent and process group, as /proc/<pid>/stat holds them after the
// command name, which may itself hold spaces and parentheses, so the fields are read from after
// its last `)`: state, ppid, pgrp.
func statParentAndGroup(root string, pid int) (ppid, pgid int, ok bool) {
	stat, err := os.ReadFile(root + "/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, 0, false
	}
	end := strings.LastIndexByte(string(stat), ')')
	if end < 0 {
		return 0, 0, false
	}
	fields := strings.Fields(string(stat[end+1:]))
	if len(fields) < 3 {
		return 0, 0, false
	}
	ppid, errParent := strconv.Atoi(fields[1])
	pgid, errGroup := strconv.Atoi(fields[2])
	return ppid, pgid, errParent == nil && errGroup == nil
}

// pidGuard holds the pids of the launcher's own children that are no generation's, so a reap of
// PID 1's leaves each exit to the wait that started it (pidGuard.reap: the orphan reaper's passes,
// which skip them as they skip a manager's generation, and the controller pod's drain). A pid is
// recorded as its process starts, under the lock every reap holds for its whole pass, so no pass
// can read /proc between a child's start and its record. Every child the launcher's process starts
// and waits for itself, but a generation, starts here: the test database's script (runTestPG), the
// put-back's jj and git (bundleRestore), the leak-check install's run of the check
// (checkInstall) and each cache trimmer sweep's deleter (startContainedRemover).
// TestEveryChildTheLaunchersProcessWaitsForStartsWhereNoReapTakesIt reads every start that process
// reaches.
type pidGuard struct {
	mu   sync.Mutex
	pids map[int]bool
}

// start runs cmd and answers its pid, recorded as the launcher's own.
func (g *pidGuard) start(cmd *exec.Cmd) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	pid := cmd.Process.Pid
	if g.pids == nil {
		g.pids = map[int]bool{}
	}
	g.pids[pid] = true
	return pid, nil
}

// done forgets pid once its own wait has read its exit.
func (g *pidGuard) done(pid int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.pids, pid)
}

// starter is start and done as a procgroup.Starter, for the launcher's own children that run
// through a package that takes one: the put-back's jj and git (workspace.RunCommand, from
// restoreWorkspace) and the leak-check install's run of the check (leakguard.InstallServed). Its
// start takes the guard's lock alone, as start does.
func (g *pidGuard) starter() procgroup.Starter {
	return func(cmd *exec.Cmd) (func(), error) {
		pid, err := g.start(cmd)
		if err != nil {
			return nil, err
		}
		return func() { g.done(pid) }, nil
	}
}

// held are the pids the reaper must skip. The caller holds the guard's lock for the whole pass.
func (g *pidGuard) held() []int {
	pids := make([]int, 0, len(g.pids))
	for pid := range g.pids {
		pids = append(pids, pid)
	}
	return pids
}

// reap is one pass of reapNamespace that leaves the guard's children, and skip, to their own
// waits, under the guard's lock for the whole pass, and answers whether any other process of the
// namespace is left that is none of the guard's children's (listNamespace): their descendants and
// group members are not counted, though an exited one PID 1 adopted is reaped, since no other wait
// reads it. Every reap of the launcher's process runs here: the orphan reaper's passes
// (reapOrphans, which skips each manager's generation) and the controller pod's drain of an exited
// generation (cleanExitedGeneration), which so waits for no process of a held child's.
func (g *pidGuard) reap(skip ...int) (bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	held := g.held()
	listed, err := listNamespace("/proc", held)
	if err != nil {
		return false, fmt.Errorf("list role namespace processes: %w", err)
	}
	return reapListed(listed.pids, append(held, skip...), listed.spared)
}

// signalNamespace sends sig to every process of PID 1's namespace but PID 1 and the guard's
// children with theirs (listNamespace), which the controller pod's drain leaves to finish and to
// their own waits. While the guard holds nothing it is kill(-1), which reaches every process at
// once, a setsid or daemonized one included; while it holds a child the signal goes process by
// process to the others of one listing (signalEach), under the guard's lock, so no guarded child
// starts between the read of /proc and the signals, and a process a held child forks after the
// read is not signalled.
func (g *pidGuard) signalNamespace(sig syscall.Signal) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	held := g.held()
	if len(held) == 0 {
		return signalGeneration(0, sig)
	}
	listed, err := listNamespace("/proc", held)
	if err != nil {
		return fmt.Errorf("list role namespace processes: %w", err)
	}
	return signalEach(listed.others, sig, syscall.Kill)
}

// signalEach sends sig to each of pids with kill, as kill(-1) would: a process that is gone
// (ESRCH) or that this process may not signal (EPERM) is passed over, since kill(-1) also signals
// every process it may and leaves the rest, and any other error ends the signals with it.
func signalEach(pids []int, sig syscall.Signal, kill func(int, syscall.Signal) error) error {
	for _, pid := range pids {
		if err := kill(pid, sig); err != nil && !errors.Is(err, syscall.ESRCH) && !errors.Is(err, syscall.EPERM) {
			return fmt.Errorf("signal role process %d: %w", pid, err)
		}
	}
	return nil
}

// reapNamespace leaves each process of skip, a direct child exec.Cmd.Wait reads, to that wait.
// Every other adopted child belongs to PID 1; Wait4 with WNOHANG reaps exited orphans without
// delaying a resident worker's next turn, and never takes the exit status a generation's own wait
// reports. It answers whether any process of the namespace is left but PID 1 and skip's.
func reapNamespace(skip ...int) (bool, error) {
	pids, err := procPIDs("/proc")
	if err != nil {
		return false, fmt.Errorf("list role namespace processes: %w", err)
	}
	return reapListed(pids, skip, nil)
}

// reapListed is reapNamespace over pids, one listing of /proc, that answers whether a process is
// left that is neither in skip nor in uncounted, which it reaps all the same.
func reapListed(pids, skip []int, uncounted map[int]bool) (bool, error) {
	remaining := false
	for _, pid := range pids {
		if pid <= 1 || slices.Contains(skip, pid) {
			continue
		}
		// No __WCLONE or __WALL: a child that sends no SIGCHLD when it exits, such as the clone
		// Go's os package makes to probe for pidfd support (CLONE_VFORK|CLONE_PIDFD, no exit
		// signal, waited with WCLONE), is never one this reaps.
		reaped, err := syscall.Wait4(pid, nil, syscall.WNOHANG, nil)
		if err != nil && !errors.Is(err, syscall.ECHILD) && !errors.Is(err, syscall.ESRCH) && !errors.Is(err, syscall.EINTR) {
			return false, fmt.Errorf("reap role process %d: %w", pid, err)
		}
		remaining = remaining || (reaped != pid && !uncounted[pid])
	}
	return remaining, nil
}

// reapOrphans is PID 1's reaper: on every SIGCHLD it reaps each exited process of the container's
// PID namespace but the generations its managers run, whose exits their own waits read, and the
// launcher's own children (the test database's script, the put-back's jj and git, the leak-check
// install's run of the check and each sweep's deleter, pidGuard), whose exits their own waits read
// too. Every manager's lock and then the guard's are held across the pass (pidGuard.reap), and a
// manager records its generation's pid under its lock as it starts it, as the guard records its
// child's, so the reaper never waits on a process it has not seen yet. In the controller's pod a
// pass that fails leaves the role's namespace unobservable, so the launcher fails its container
// loudly. In an issue pod's container the container's /proc still lists every role's processes,
// since a role's PID namespace is nested in it, but none is the launcher's child except each
// role's role-init and the launcher's own held children, which the pass skips: the rest are their
// role-init's to reap, and Wait4 on them answers ECHILD. So a failed pass costs no role anything,
// and it is logged and the next pass tries again.
func reapOrphans(ctx context.Context, signals <-chan os.Signal, managers []*manager, guard *pidGuard) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-signals:
			var skip []int
			for _, m := range managers {
				m.mu.Lock()
				if m.child != nil {
					skip = append(skip, m.child.pid)
				}
			}
			_, err := guard.reap(skip...)
			for _, m := range managers {
				m.mu.Unlock()
			}
			switch {
			case err == nil:
			case !managers[0].namespaced:
				panic(err)
			default:
				managers[0].log.linef("launcher: %v", err)
			}
		}
	}
}

// namespaceReapPoll is how often a wait for a role's processes to go asks again: the span between
// two passes of reapNamespace, here and in role-init's own drain (roleNamespaceDrain,
// roleinit_linux.go). Ten milliseconds, because what either wait is there for is a git or a jj
// that leaves on its SIGTERM in milliseconds, and a pass costs one read of /proc and one Wait4
// per process: short enough that a teardown is not held past what its processes need, cheap
// enough to run for the whole of a deadline nothing answers inside.
const namespaceReapPoll = 10 * time.Millisecond

// Cleanup finishes before the exit is reported, credentials are removed or a new generation is
// admitted. A namespace whose processes cannot be observed is not safe to reuse: fail the launcher
// loudly so Kubernetes replaces this container rather than reporting a false completed stop. Only
// the single-role form runs it; an issue pod's roles end with their own namespace's init. It
// leaves the launcher's own children, with their descendants and process groups, alone: its
// SIGTERM and SIGKILL do not reach them (pidGuard.signalNamespace), its reap passes leave their
// exits to their own waits, and it waits for none of them (pidGuard.reap). None can run then
// today, since in the controller's pod the leak-check install runs before the first start and a
// restore inside a start, which is refused while a generation is still the manager's; the drain
// leaves them alone all the same, so a step that one day runs then keeps its child.
func (m *manager) cleanExitedGeneration() {
	if os.Getpid() != 1 {
		return
	}
	if err := m.guard.signalNamespace(syscall.SIGTERM); err != nil {
		panic(err)
	}
	deadline := time.Now().Add(m.cfg.StopGrace)
	for {
		remaining, err := m.guard.reap()
		if err != nil {
			panic(err)
		}
		if !remaining {
			return
		}
		// Past the grace every pass kills what is left again: a signal sent process by process
		// (signalNamespace, while the guard holds a child) misses a process forked after the read
		// of /proc it went by, which the next pass finds.
		if !time.Now().Before(deadline) {
			if err := m.guard.signalNamespace(syscall.SIGKILL); err != nil {
				panic(err)
			}
		}
		time.Sleep(namespaceReapPoll)
	}
}

// signalChild reaches one generation. In the controller's pod the generation is a process group
// of the container's own namespace, so the signal goes to the group. In an issue pod the
// generation is the init of the role's own namespaces: the signal goes to it alone, through its
// own process handle, which forwards a stop to every process of the role and, killed, takes the
// whole namespace with it. A generation its wait has already reaped is done: the handle reaches
// nothing then, where its pid could name another role's role-init by now.
func (m *manager) signalChild(active *child, sig syscall.Signal) error {
	if !m.namespaced {
		return signalGeneration(active.pid, sig)
	}
	if err := active.process.Signal(sig); err != nil && !errors.Is(err, os.ErrProcessDone) && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("signal role %s: %w", m.role, err)
	}
	return nil
}

// endGeneration stops active's child: SIGTERM, then SIGKILL once grace is over. The first stop's
// end of grace is recorded (child.stopBy), which bounds a Claude Code keeper's last save (wait).
func (m *manager) endGeneration(active *child, grace time.Duration) error {
	m.mu.Lock()
	if active.stopBy.IsZero() {
		active.stopBy = time.Now().Add(grace)
	}
	m.mu.Unlock()
	if err := m.signalChild(active, syscall.SIGTERM); err != nil {
		return err
	}
	select {
	case <-active.done:
	case <-time.After(grace):
		if err := m.signalChild(active, syscall.SIGKILL); err != nil {
			return err
		}
		<-active.done
	}
	return nil
}

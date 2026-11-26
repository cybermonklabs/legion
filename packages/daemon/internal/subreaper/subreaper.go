// Package subreaper is what a process needs once it takes in its descendants' orphans: a reaper
// for every exit it does not wait for itself, which never takes an exit it does.
//
// A process the kernel gives its descendants' orphans to, the init of a PID namespace or one that
// set PR_SET_CHILD_SUBREAPER, must reap them, or each stays a zombie. A reap that takes any exited
// child can also take the exit of a child an exec.Cmd still waits for: that Wait then fails with
// `waitid: no child processes`, or under gVisor at times `wait: no child processes` or `waitid: no
// such process`, and the caller loses the child's status. The pod's launcher, its container's
// PID 1, lost two of its own commands' exits that way before its pidGuard
// (internal/launcher/process.go). So a Guard records the pid of every child the process starts
// and waits for itself, under the lock each pass of its reaper holds for the whole pass, and the
// reaper (Serve) reaps every other exited child.
//
// `legion worker-shim --reap-orphans` uses it (internal/shim's Config.ReapOrphans): the controller
// pod's launcher passes that flag when it runs the shim with no role-init above it.
package subreaper

import (
	"os/exec"
	"sync"

	"github.com/sjawhar/legion/daemon/internal/procgroup"
)

// Guard holds the pids of the children a process starts and waits for itself. Each one starts
// through Start or Starter and is released with Done once its own wait has read its exit; until
// then no pass of the reaper reaps it. Its zero value is not ready: use NewGuard.
type Guard struct {
	mu sync.Mutex
	// pids counts, by pid, the children started here that their own waits have not released yet.
	// A count rather than a mark: once a child's wait has reaped it and before that wait's Done,
	// the kernel may give the pid to a new child started here, and the earlier Done must not take
	// the new child's hold away.
	pids map[int]int
	// proc is where a pass lists the processes, "/proc" but in a test of this package that makes a
	// pass fail.
	proc string
	// again asks the reaper for a pass: one slot, so a Done while a pass runs, or while another is
	// already asked for, adds no pass of its own.
	again chan struct{}
	// passed, when a test of this package sets it before Serve, is called after each pass with the
	// pids that pass reaped.
	passed func(reaped []int)
}

// NewGuard is a guard that holds no child yet.
func NewGuard() *Guard {
	return &Guard{pids: map[int]int{}, proc: "/proc", again: make(chan struct{}, 1)}
}

// Start starts cmd and records its pid, under the lock every pass of the reaper holds, so no pass
// can run between the child's start and its record. The caller calls Done with the pid once
// cmd.Wait has read the exit.
func (g *Guard) Start(cmd *exec.Cmd) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	pid := cmd.Process.Pid
	g.hold(pid)
	return pid, nil
}

// hold records one more hold on pid, with g.mu held: a count, never a mark, since the kernel may
// give the pid of a child whose wait has reaped it, and whose Done has not come yet, to a new child
// started here (Guard.pids).
func (g *Guard) hold(pid int) {
	g.pids[pid]++
}

// Done releases one hold on pid, whose own wait has read its exit, and asks the reaper for a pass.
// The pass covers the one gap the record leaves: once that wait has reaped the child, the kernel
// may give its pid to a new process, an orphan the reaper must take, and a pass made while the pid
// was still held passed that orphan over.
func (g *Guard) Done(pid int) {
	g.mu.Lock()
	if g.pids[pid]--; g.pids[pid] <= 0 {
		delete(g.pids, pid)
	}
	g.mu.Unlock()
	select {
	case g.again <- struct{}{}:
	default:
	}
}

// Starter is Start and Done as a procgroup.Starter, for a child started by code that takes one.
func (g *Guard) Starter() procgroup.Starter {
	return func(cmd *exec.Cmd) (func(), error) {
		pid, err := g.Start(cmd)
		if err != nil {
			return nil, err
		}
		return func() { g.Done(pid) }, nil
	}
}

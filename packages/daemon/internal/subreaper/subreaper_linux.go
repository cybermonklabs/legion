package subreaper

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// The retry of a pass that failed: after firstRetry, doubling to lastRetry, until a pass succeeds.
// A pass's failure may have used up the event that asked for it, the SIGCHLD of the last orphan to
// exit, and no later event need come, so a failed pass is tried again on a clock of its own rather
// than left waiting for one. A successful pass waits for events alone, so a held child cannot
// make the reaper spin.
const (
	firstRetry = 100 * time.Millisecond
	lastRetry  = 5 * time.Second
)

// Serve makes this process the child subreaper of its descendants (PR_SET_CHILD_SUBREAPER), so a
// descendant whose parent exits is handed to this process rather than to its PID namespace's
// init, and reaps every exited child the guard does not hold: one pass at once, then one pass
// at each SIGCHLD and at each Done, until stop is called. stop ends the reaper and returns once
// its last pass is over. A pass that fails is logged through logf and tried again after a
// backoff (firstRetry to lastRetry), or at the next event if one comes first.
//
// After a successful pass the reaper does nothing until the next event, so it cannot spin: a
// held child that has exited and whose wait has not read it yet costs the one pass its SIGCHLD
// makes, which passes it over, and nothing more until the next SIGCHLD or Done.
//
// The attribute is the process's, not the caller's thread's, and no child inherits it. A child
// that is still running when this process exits goes to the next subreaper above it, or to the
// namespace's init, as any orphan does.
func (g *Guard) Serve(logf func(format string, args ...any)) (stop func(), err error) {
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		return nil, fmt.Errorf("make this process its descendants' child subreaper: %w", err)
	}
	exits := make(chan os.Signal, 1)
	signal.Notify(exits, syscall.SIGCHLD)
	ended, finished := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(finished)
		defer signal.Stop(exits)
		var retry time.Duration
		for {
			reaped, err := g.pass()
			var again <-chan time.Time
			if err != nil {
				retry = min(max(2*retry, firstRetry), lastRetry)
				logf("reap this process's exited orphans: %v; trying again in %s", err, retry)
				// A timer nothing refers to any more is collected (Go 1.23 on), so one left
				// behind by an event that came first costs nothing.
				again = time.After(retry)
			} else {
				retry = 0
			}
			if g.passed != nil {
				g.passed(reaped)
			}
			select {
			case <-ended:
				return
			case <-exits:
			case <-g.again:
			case <-again:
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() { close(ended) })
		<-finished
	}, nil
}

// pass is one pass of the reaper, under the guard's lock for the whole of it: it waits, with
// WNOHANG, on every process /proc lists but this one, PID 1 and the guard's, and answers the pids
// it reaped. A process that is not this one's child answers ECHILD and is passed over; one still
// running answers 0. This is the pass the launcher's reapNamespace makes (internal/launcher's
// process.go), a wait on each process by its id, so a held child is never waited on at all.
func (g *Guard) pass() ([]int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	entries, err := os.ReadDir(g.proc)
	if err != nil {
		return nil, fmt.Errorf("list the processes: %w", err)
	}
	self := os.Getpid()
	var reaped []int
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 1 || pid == self || g.pids[pid] > 0 {
			continue
		}
		// No __WCLONE or __WALL: a child that sends no SIGCHLD when it exits, such as the clone
		// Go's os package makes to probe for pidfd support (CLONE_VFORK|CLONE_PIDFD, no exit
		// signal, waited with WCLONE), is never one this reaps.
		got, err := syscall.Wait4(pid, nil, syscall.WNOHANG, nil)
		if err != nil && !errors.Is(err, syscall.ECHILD) && !errors.Is(err, syscall.ESRCH) && !errors.Is(err, syscall.EINTR) {
			return reaped, fmt.Errorf("reap process %d: %w", pid, err)
		}
		if got == pid {
			reaped = append(reaped, pid)
		}
	}
	return reaped, nil
}

package exp3

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// pidLine is what cmd/sessionworker prints once its worker is polling.
var pidLine = regexp.MustCompile(`^sessionworker: pid=(\d+)$`)

// child is one cmd/sessionworker process the driver started: the pid the driver got from
// the operating system, the pid the process printed about itself, and the plumbing that
// forwards the rest of its output to stderr and reaps it exactly once.
type child struct {
	name        string
	cmd         *exec.Cmd
	pid         int // the pid exec reported
	reportedPID int // the pid the process printed
	drained     chan struct{}
	once        sync.Once
}

// startChild runs binary as a session worker on taskQueue and returns once the process
// has printed its pid, which it does only after its worker is polling. The process runs
// in its own process group so that stop can take down anything it spawned.
func startChild(ctx context.Context, name, binary, address, namespace, taskQueue, workDir string, timeout time.Duration) (*child, error) {
	readEnd, writeEnd, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("%s: pipe: %w", name, err)
	}
	cmd := exec.Command(binary,
		"-address", address,
		"-namespace", namespace,
		"-task-queue", taskQueue,
	)
	cmd.Dir = workDir
	cmd.Stdout = writeEnd
	cmd.Stderr = writeEnd
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		readEnd.Close()
		writeEnd.Close()
		return nil, fmt.Errorf("%s: start %s: %w", name, binary, err)
	}
	// The parent holds no write end, so the reader sees EOF when the process dies.
	writeEnd.Close()

	c := &child{name: name, cmd: cmd, pid: cmd.Process.Pid, drained: make(chan struct{})}

	pidCh := make(chan int, 1)
	go func() {
		defer close(c.drained)
		defer readEnd.Close()
		scanner := bufio.NewScanner(readEnd)
		for scanner.Scan() {
			line := scanner.Text()
			if m := pidLine.FindStringSubmatch(line); m != nil {
				reported, convErr := strconv.Atoi(m[1])
				if convErr == nil {
					select {
					case pidCh <- reported:
					default:
					}
				}
			}
			fmt.Fprintf(os.Stderr, "[%s] %s\n", name, line)
		}
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case reported := <-pidCh:
		c.reportedPID = reported
		if reported != c.pid {
			c.stop()
			return nil, fmt.Errorf("%s: printed pid %d but was started as pid %d", name, reported, c.pid)
		}
		return c, nil
	case <-c.drained:
		c.stop()
		return nil, fmt.Errorf("%s: exited before printing its pid", name)
	case <-timer.C:
		c.stop()
		return nil, fmt.Errorf("%s: printed no pid within %s", name, timeout)
	case <-ctx.Done():
		c.stop()
		return nil, ctx.Err()
	}
}

// alive reports whether the process still exists, by sending it the null signal.
func (c *child) alive() bool {
	return syscall.Kill(c.pid, 0) == nil
}

// signal sends sig to the exact pid the driver recorded, then reaps the process. It is
// what the experiment measures with, so it names the one pid and nothing around it.
func (c *child) signal(sig syscall.Signal) error {
	if err := syscall.Kill(c.pid, sig); err != nil {
		return fmt.Errorf("%s: signal %v to pid %d: %w", c.name, sig, c.pid, err)
	}
	c.reap()
	return nil
}

// stop takes the process group down and reaps it. It is safe to call on a process that
// is already gone, and safe to call more than once.
func (c *child) stop() {
	if c.cmd.Process != nil {
		// Negative pid: the process group started by Setpgid, so nothing survives.
		_ = syscall.Kill(-c.pid, syscall.SIGKILL)
	}
	c.reap()
}

// reap waits for the process exactly once and for its output to run out, so no zombie
// and no goroutine outlives the experiment.
func (c *child) reap() {
	c.once.Do(func() {
		_ = c.cmd.Wait()
		<-c.drained
	})
}

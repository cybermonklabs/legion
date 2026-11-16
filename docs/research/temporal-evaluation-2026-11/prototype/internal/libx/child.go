package libx

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

// fieldLine matches what a worker process tells its driver: one `<prefix>: key=value`
// line per thing the driver has to know, the first of them always `pid`.
var fieldLine = regexp.MustCompile(`^([a-z0-9-]+): ([a-z0-9-]+)=(.*)$`)

// Child is one worker process the driver started: the pid the operating system gave
// it, the fields it has printed so far, and the plumbing that forwards the rest of its
// output to stderr and reaps it exactly once.
type Child struct {
	name    string
	prefix  string
	cmd     *exec.Cmd
	pid     int
	drained chan struct{}
	once    sync.Once

	mu      sync.Mutex
	fields  map[string]string
	updated chan struct{}
}

// StartChild runs binary with args and returns once the process has printed its pid,
// which it does only after the library under test is running in it. prefix is the word
// that process puts in front of every field line. The process runs in its own process
// group so that Stop can take down anything it spawned.
func StartChild(ctx context.Context, name, prefix, binary string, args []string, workDir string, timeout time.Duration) (*Child, error) {
	readEnd, writeEnd, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("%s: pipe: %w", name, err)
	}
	cmd := exec.Command(binary, args...)
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

	c := &Child{
		name:    name,
		prefix:  prefix,
		cmd:     cmd,
		pid:     cmd.Process.Pid,
		drained: make(chan struct{}),
		fields:  map[string]string{},
		updated: make(chan struct{}),
	}
	go func() {
		defer close(c.drained)
		defer readEnd.Close()
		scanner := bufio.NewScanner(readEnd)
		for scanner.Scan() {
			line := scanner.Text()
			if m := fieldLine.FindStringSubmatch(line); m != nil && m[1] == prefix {
				c.setField(m[2], m[3])
			}
			fmt.Fprintf(os.Stderr, "[%s] %s\n", name, line)
		}
	}()

	reported, err := c.Field(ctx, "pid", timeout)
	if err != nil {
		c.Stop()
		return nil, err
	}
	if reported != strconv.Itoa(c.pid) {
		c.Stop()
		return nil, fmt.Errorf("%s: printed pid %s but was started as pid %d", name, reported, c.pid)
	}
	return c, nil
}

// PID is the process id the operating system gave the child, which is the pid the
// driver signals and the pid the experiment's facts name.
func (c *Child) PID() int { return c.pid }

// Name is how the experiment's facts refer to this process.
func (c *Child) Name() string { return c.name }

func (c *Child) setField(key, value string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fields[key] = value
	close(c.updated)
	c.updated = make(chan struct{})
}

// Field waits until the child has printed key, and returns its value. It fails when
// the child exits without printing it, or when timeout passes first.
func (c *Child) Field(ctx context.Context, key string, timeout time.Duration) (string, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		c.mu.Lock()
		value, ok := c.fields[key]
		updated := c.updated
		c.mu.Unlock()
		if ok {
			return value, nil
		}
		select {
		case <-updated:
		case <-c.drained:
			c.mu.Lock()
			value, ok := c.fields[key]
			c.mu.Unlock()
			if ok {
				return value, nil
			}
			return "", fmt.Errorf("%s: exited before printing %s", c.name, key)
		case <-timer.C:
			return "", fmt.Errorf("%s: printed no %s within %s", c.name, key, timeout)
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
}

// Alive reports whether the process still exists, by sending it the null signal.
func (c *Child) Alive() bool {
	return syscall.Kill(c.pid, 0) == nil
}

// Signal sends sig to the exact pid the driver recorded, then reaps the process. It is
// what the experiment measures with, so it names the one pid and nothing around it.
func (c *Child) Signal(sig syscall.Signal) error {
	if err := syscall.Kill(c.pid, sig); err != nil {
		return fmt.Errorf("%s: signal %v to pid %d: %w", c.name, sig, c.pid, err)
	}
	c.reap()
	return nil
}

// Stop takes the process group down and reaps it. It is safe to call on a process that
// is already gone, and safe to call more than once.
func (c *Child) Stop() {
	if c.cmd.Process != nil {
		// Negative pid: the process group started by Setpgid, so nothing survives.
		_ = syscall.Kill(-c.pid, syscall.SIGKILL)
	}
	c.reap()
}

// reap waits for the process exactly once and for its output to run out, so no zombie
// and no goroutine outlives the experiment.
func (c *Child) reap() {
	c.once.Do(func() {
		_ = c.cmd.Wait()
		<-c.drained
	})
}

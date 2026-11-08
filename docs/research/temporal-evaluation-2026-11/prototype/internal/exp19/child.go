package exp19

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/harness"
)

// pidLine is what a worker helper prints once its worker is polling.
var pidLine = regexp.MustCompile(`^` + workerPIDPrefix + `: pid=(\d+)$`)

// Child is one worker process the driver started: the pid the driver got from the
// operating system, the pid the process printed about itself, and the plumbing that
// forwards the rest of its output to stderr and reaps it exactly once.
type Child struct {
	// PID is the pid exec reported, and the one pid the experiment signals.
	PID int

	name        string
	cmd         *exec.Cmd
	reportedPID int // the pid the process printed
	drained     chan struct{}
	once        sync.Once
}

// helperCommand is this same experiments binary, invoked again as one of experiment
// 19's helper processes: the same server, namespace and scratch directory, with the
// role and its settings in the environment. It is `-experiment 19` whichever
// experiment started it, because the roles are this package's: experiment 20 hands
// its work out with the same worker and the same completer.
func helperCommand(ctx context.Context, cfg harness.Config, workDir, role string, env ...string) (*exec.Cmd, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("this binary's own path is unknown, so the handout experiments cannot start their helper processes: %w", err)
	}
	cmd := exec.CommandContext(ctx, self,
		"-experiment", "19",
		"-address", cfg.Address,
		"-namespace", cfg.Namespace,
		"-work-dir", workDir,
	)
	cmd.Dir = workDir
	cmd.Env = append(append(os.Environ(), roleEnv+"="+role), env...)
	return cmd, nil
}

// StartWorkerChild runs a worker helper on taskQueue and returns once the process has
// printed its pid, which it does only after its worker is polling. The process runs in
// its own process group so that Stop can take down anything it spawned.
func StartWorkerChild(ctx context.Context, cfg harness.Config, name, workDir, taskQueue string, timeout time.Duration) (*Child, error) {
	readEnd, writeEnd, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("%s: pipe: %w", name, err)
	}
	cmd, err := helperCommand(ctx, cfg, workDir, workerRole, queueEnv+"="+taskQueue)
	if err != nil {
		readEnd.Close()
		writeEnd.Close()
		return nil, err
	}
	cmd.Stdout = writeEnd
	cmd.Stderr = writeEnd
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		readEnd.Close()
		writeEnd.Close()
		return nil, fmt.Errorf("%s: start %s: %w", name, cmd.Path, err)
	}
	// The parent holds no write end, so the reader sees EOF when the process dies.
	writeEnd.Close()

	c := &Child{PID: cmd.Process.Pid, name: name, cmd: cmd, drained: make(chan struct{})}

	pidCh := make(chan int, 1)
	go func() {
		defer close(c.drained)
		defer readEnd.Close()
		scanner := bufio.NewScanner(readEnd)
		for scanner.Scan() {
			line := scanner.Text()
			if m := pidLine.FindStringSubmatch(line); m != nil {
				if reported, convErr := strconv.Atoi(m[1]); convErr == nil {
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
		if reported != c.PID {
			c.Stop()
			return nil, fmt.Errorf("%s: printed pid %d but was started as pid %d", name, reported, c.PID)
		}
		return c, nil
	case <-c.drained:
		c.Stop()
		return nil, fmt.Errorf("%s: exited before printing its pid", name)
	case <-timer.C:
		c.Stop()
		return nil, fmt.Errorf("%s: printed no pid within %s", name, timeout)
	case <-ctx.Done():
		c.Stop()
		return nil, ctx.Err()
	}
}

// Alive reports whether the process still exists, by sending it the null signal.
func (c *Child) Alive() bool {
	return syscall.Kill(c.PID, 0) == nil
}

// Signal sends sig to the exact pid the driver recorded, then reaps the process. It is
// what the experiment measures with, so it names the one pid and nothing around it.
func (c *Child) Signal(sig syscall.Signal) error {
	if err := syscall.Kill(c.PID, sig); err != nil {
		return fmt.Errorf("%s: signal %v to pid %d: %w", c.name, sig, c.PID, err)
	}
	c.reap()
	return nil
}

// Stop takes the process group down and reaps it. It is safe to call on a process that
// is already gone, and safe to call more than once.
func (c *Child) Stop() {
	if c.cmd.Process != nil {
		// Negative pid: the process group started by Setpgid, so nothing survives.
		_ = syscall.Kill(-c.PID, syscall.SIGKILL)
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

// CompleteFromAnotherProcess runs a completer helper to the end and returns what the
// server answered it. The helper holds the token and nothing else, so an accepted
// completion is one any process with the token could have made, and a refusal is the
// server's own words, carried back through the answer file.
func CompleteFromAnotherProcess(ctx context.Context, cfg harness.Config, workDir, answerFile string, token []byte, result string, timeout time.Duration) (Completion, error) {
	if err := os.Remove(answerFile); err != nil && !os.IsNotExist(err) {
		return Completion{}, fmt.Errorf("remove %s: %w", answerFile, err)
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd, err := helperCommand(runCtx, cfg, workDir, completerRole,
		tokenEnv+"="+base64.StdEncoding.EncodeToString(token),
		resultEnv+"="+result,
		answerEnv+"="+answerFile,
	)
	if err != nil {
		return Completion{}, err
	}
	// Everything a helper prints goes to stderr, so nothing it says can land among
	// this experiment's measured lines on stdout.
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return Completion{}, fmt.Errorf("the completing process failed: %w", err)
	}

	data, err := os.ReadFile(answerFile)
	if err != nil {
		return Completion{}, fmt.Errorf("the completing process wrote no answer: %w", err)
	}
	var answer Completion
	if err := json.Unmarshal(data, &answer); err != nil {
		return Completion{}, fmt.Errorf("the completing process wrote %q, which is not its answer: %w", data, err)
	}
	return answer, nil
}

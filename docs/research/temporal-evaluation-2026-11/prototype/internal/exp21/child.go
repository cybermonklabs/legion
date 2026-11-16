package exp21

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
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

// readyLine is what a holder prints once it holds its working copy and its
// credential, and -- in queue mode -- once its worker is polling the queue it named
// for itself. The driver reads the queue and the identity out of this line, because
// the holder makes them and nothing else knows them.
var readyLine = regexp.MustCompile(`^` + readyPrefix +
	`: pid=(\d+) host=(\S+) mode=(\S+) queue=(\S+) identity=(\S+) ran-worker=(\S+) repository-ready=(\S+) credential-ready=(\S+)$`)

// holderChild is one holder process the driver started: the pid the operating system
// reported, what the holder said about itself, and the plumbing that forwards the
// rest of its output to stderr and reaps it exactly once.
type holderChild struct {
	// PID is the pid exec reported, and the one pid the driver names.
	PID int
	// Host, Queue and Identity are what the holder reported about itself. Queue and
	// Identity are "none" for a holder that runs no worker.
	Host     string
	Queue    string
	Identity string
	// ReportFile is where the holder writes what it did.
	ReportFile string

	name    string
	cmd     *exec.Cmd
	drained chan struct{}
	once    sync.Once
}

// startHolder runs a holder process and returns once it has printed its ready line,
// which it does only after its working copy, its credential and -- in queue mode --
// its worker are all up. The process runs in its own process group so that Stop can
// take down anything it spawned.
func startHolder(
	ctx context.Context,
	cfg harness.Config,
	name, workDir, dir, mode, tokenFile, reportFile string,
	timeout time.Duration,
) (*holderChild, error) {
	if err := os.Remove(reportFile); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("remove %s: %w", reportFile, err)
	}
	self, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("this binary's own path is unknown, so experiment 21 cannot start its holder: %w", err)
	}
	readEnd, writeEnd, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("%s: pipe: %w", name, err)
	}

	cmd := exec.CommandContext(ctx, self,
		"-experiment", "21",
		"-address", cfg.Address,
		"-namespace", cfg.Namespace,
		"-work-dir", workDir,
	)
	cmd.Dir = workDir
	cmd.Env = append(os.Environ(),
		roleEnv+"="+holderRole,
		modeEnv+"="+mode,
		dirEnv+"="+dir,
		tokenFileEnv+"="+tokenFile,
		reportEnv+"="+reportFile,
	)
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

	h := &holderChild{PID: cmd.Process.Pid, ReportFile: reportFile, name: name, cmd: cmd, drained: make(chan struct{})}

	ready := make(chan []string, 1)
	go func() {
		defer close(h.drained)
		defer readEnd.Close()
		scanner := bufio.NewScanner(readEnd)
		for scanner.Scan() {
			line := scanner.Text()
			if m := readyLine.FindStringSubmatch(line); m != nil {
				select {
				case ready <- m:
				default:
				}
			}
			fmt.Fprintf(os.Stderr, "[%s] %s\n", name, line)
		}
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case m := <-ready:
		reported, convErr := strconv.Atoi(m[1])
		if convErr != nil || reported != h.PID {
			h.Stop()
			return nil, fmt.Errorf("%s: printed pid %q but was started as pid %d", name, m[1], h.PID)
		}
		h.Host, h.Queue, h.Identity = m[2], m[4], m[5]
		return h, nil
	case <-h.drained:
		h.Stop()
		return nil, fmt.Errorf("%s: exited before it was ready", name)
	case <-timer.C:
		h.Stop()
		return nil, fmt.Errorf("%s: was not ready within %s", name, timeout)
	case <-ctx.Done():
		h.Stop()
		return nil, ctx.Err()
	}
}

// Wait blocks until the holder has written its report and returns what it says. A
// holder that exits without one, or writes none in time, is an error: the driver
// would then have no reading of what the process it handed the work to did with it.
func (h *holderChild) Wait(ctx context.Context, timeout time.Duration) (Report, error) {
	deadline := time.Now().Add(timeout)
	for {
		report, found, err := h.readReport()
		if err != nil {
			return Report{}, err
		}
		if found {
			return report, nil
		}
		select {
		case <-h.drained:
			// It is gone; one last look, in case it wrote the report as it ended.
			report, found, err := h.readReport()
			if err != nil {
				return Report{}, err
			}
			if found {
				return report, nil
			}
			return Report{}, fmt.Errorf("%s exited without writing a report to %s", h.name, h.ReportFile)
		case <-ctx.Done():
			return Report{}, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
		if !time.Now().Before(deadline) {
			return Report{}, fmt.Errorf("%s wrote no report to %s within %s", h.name, h.ReportFile, timeout)
		}
	}
}

// readReport reads the holder's report, and reports a file that is not there yet as
// absent rather than as an error: it does not exist until the holder has finished.
func (h *holderChild) readReport() (Report, bool, error) {
	data, err := os.ReadFile(h.ReportFile)
	if errors.Is(err, os.ErrNotExist) {
		return Report{}, false, nil
	}
	if err != nil {
		return Report{}, false, fmt.Errorf("read %s: %w", h.ReportFile, err)
	}
	var report Report
	if err := json.Unmarshal(data, &report); err != nil {
		return Report{}, false, fmt.Errorf("%s wrote %q, which is not its report: %w", h.name, data, err)
	}
	return report, true, nil
}

// Alive reports whether the process still exists, by sending it the null signal.
func (h *holderChild) Alive() bool {
	return syscall.Kill(h.PID, 0) == nil
}

// Stop takes the process group down and reaps it. It is safe to call on a process
// that is already gone, and safe to call more than once.
func (h *holderChild) Stop() {
	if h.cmd.Process != nil {
		// Negative pid: the process group started by Setpgid, so nothing survives.
		_ = syscall.Kill(-h.PID, syscall.SIGKILL)
	}
	h.reap()
}

// reap waits for the process exactly once and for its output to run out, so no
// zombie and no goroutine outlives the experiment.
func (h *holderChild) reap() {
	h.once.Do(func() {
		_ = h.cmd.Wait()
		<-h.drained
	})
}

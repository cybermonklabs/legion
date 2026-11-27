package replay

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// DefaultGitMemoryCap is the memory guard's cap on the git processes collect-git runs: 1.5 GiB of
// the summed resident memory of one command's process group, the guard the feasibility reading of
// git's verdicts ran under.
const DefaultGitMemoryCap int64 = 1536 << 20

// gitPoll is how often the guard reads the resident memory of the process group it watches.
const gitPoll = 200 * time.Millisecond

// The guard's time of each git command collect-git runs: the clone of the base branch, a fetch of a
// batch of commits, a fetch of one commit, a merge-tree, and any other command. A command past its
// time is killed, as one past the memory cap is. A merge-tree's time is that of one merge: a
// merge-tree --stdin's is counted from the last record it wrote (gitClone.merge).
const (
	gitCloneTime  = 15 * time.Minute
	gitBranchTime = 180 * time.Second
	gitFetchTime  = 90 * time.Second
	gitMergeTime  = 120 * time.Second
	gitOtherTime  = 60 * time.Second
)

// gitGuard runs commands under a memory cap and a time, as the feasibility reading's guard did,
// each command on its own and any number of them at once: the command runs as the leader of a
// process group of its own, the summed resident memory of that group is read as it starts and
// every gitPoll after (rss, groupRSS where nil), and the whole group is killed once that memory
// passes limit or the command outlives its time (run). Peak is the most memory it read of any
// command it ran, under mu. Linux alone holds the /proc it reads.
type gitGuard struct {
	limit int64
	rss   func(pgid int) int64
	mu    sync.Mutex
	peak  int64
}

// guardedRun is what one command run under the guard did: its exit code (-1 where it did not exit
// by itself), what it wrote on stdout and stderr, the most memory its group held as the guard read
// it, why the guard killed it (GitMemoryCap, GitTimeout, "" for no kill), and the error that kept
// it from starting or the context's end that stopped it.
type guardedRun struct {
	code           int
	stdout, stderr []byte
	peak           int64
	killed         string
	err            error
}

// run runs cmd under g, its time timeout: counted from its start, or, where sinceOutput, from the
// last time it wrote on stdout, so a command that goes on writing is killed only once it has
// written nothing for timeout. It sets cmd's stdout, stderr and process group.
func (g *gitGuard) run(ctx context.Context, cmd *exec.Cmd, timeout time.Duration, sinceOutput bool) guardedRun {
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	// Each write on stdout is noted on wrote and moves the deadline; a nil wrote is never ready.
	var wrote chan struct{}
	if sinceOutput {
		wrote = make(chan struct{}, 1)
		cmd.Stdout = notedWriter{&stdout, wrote}
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// A process that left the group yet holds the output open does not hold the run past this.
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Start(); err != nil {
		return guardedRun{code: -1, err: err}
	}
	pgid := cmd.Process.Pid
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	rss := g.rss
	if rss == nil {
		rss = groupRSS
	}
	tick := time.NewTicker(gitPoll)
	defer tick.Stop()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	var run guardedRun
	kill := func(why string) {
		if run.killed == "" && run.err == nil {
			run.killed = why
			syscall.Kill(-pgid, syscall.SIGKILL)
		}
	}
	measure := func() {
		held := rss(pgid)
		run.peak = max(run.peak, held)
		if held > g.limit {
			kill(GitMemoryCap)
		}
	}
	// The group is read as the command starts, as the feasibility reading's guard read it, so a
	// command that ends before the first tick is read too.
	measure()
	var waited error
	for exited := false; !exited; {
		select {
		case waited = <-done:
			exited = true
		case <-tick.C:
			measure()
		case <-wrote:
			deadline.Reset(timeout)
		case <-deadline.C:
			kill(GitTimeout)
		case <-ctx.Done():
			if run.killed == "" && run.err == nil {
				run.err = ctx.Err()
				syscall.Kill(-pgid, syscall.SIGKILL)
			}
		}
	}
	g.mu.Lock()
	g.peak = max(g.peak, run.peak)
	g.mu.Unlock()
	run.stdout, run.stderr = stdout.Bytes(), stderr.Bytes()
	run.code = cmd.ProcessState.ExitCode()
	var exit *exec.ExitError
	if waited != nil && !errors.As(waited, &exit) && run.err == nil {
		run.err = waited
	}
	return run
}

// notedWriter writes into w and notes each write on wrote, where the note before has been taken,
// so a write never waits on the guard, which needs to know only that one came.
type notedWriter struct {
	w     io.Writer
	wrote chan<- struct{}
}

func (n notedWriter) Write(p []byte) (int, error) {
	written, err := n.w.Write(p)
	select {
	case n.wrote <- struct{}{}:
	default:
	}
	return written, err
}

// groupRSS is the summed resident memory of the processes of the process group pgid, in bytes, as
// /proc/<pid>/stat gives each: its fifth field the group, its 24th the resident pages. A process
// that ends while it is read is left out.
func groupRSS(pgid int) int64 {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0
	}
	page := int64(os.Getpagesize())
	var total int64
	for _, entry := range entries {
		if _, err := strconv.Atoi(entry.Name()); err != nil {
			continue
		}
		stat, err := os.ReadFile("/proc/" + entry.Name() + "/stat")
		if err != nil {
			continue
		}
		// The command's name, in parentheses, can hold spaces: the fields after it are read.
		end := bytes.LastIndexByte(stat, ')')
		if end < 0 {
			continue
		}
		fields := strings.Fields(string(stat[end+1:]))
		if len(fields) < 22 {
			continue
		}
		if group, err := strconv.Atoi(fields[2]); err != nil || group != pgid {
			continue
		}
		if pages, err := strconv.ParseInt(fields[21], 10, 64); err == nil {
			total += pages * page
		}
	}
	return total
}

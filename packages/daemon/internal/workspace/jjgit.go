package workspace

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/procgroup"
)

// The jj and git plumbing the shim runs over an issue workspace (internal/shim), the idle release's
// read of the workspace's unpushed work among it: one runner, so every command of it stops the same
// way at its budget's end and every failure names the stale git locks it hit.

// OutputLines is out's non-empty lines, trimmed: what a jj or git command's output is read as,
// and what a multi-line report is joined back from for a notice that must be one line.
func OutputLines(out string) []string {
	var kept []string
	for line := range strings.SplitSeq(out, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			kept = append(kept, line)
		}
	}
	return kept
}

// RunCommand runs one jj or git command of the workspace within the caller's budget and returns
// its trimmed standard output. start starts the command (procgroup.Starter, never nil: the shim's
// is its subreaper.Guard's).
// dir is the directory the command runs in, which is the workspace its -R or --git-dir names: jj
// searches upward from the directory it runs in for the closest .jj and reads that repository's
// configuration before it applies -R, so a command left standing wherever the caller's process
// stands would read a repository nobody named (docs/testing.md). stdin is fed to the command, and
// nil is an empty standard input. started is when the caller's budget began, which a timeout's
// report names. A failure's report ends with a note on each stale git lock it names
// (StaleGitLockNote). The command timed out only when the budget ended it: once the command has
// exited on its own, procgroup.WaitDelay can hold the wait open past the budget for a child still
// holding the command's pipes, and the command's own exit is then its answer. A command that
// never started because an earlier one on the same budget spent it timed out too, while a command
// the caller stopped by cancelling its context, rather than the budget ending it, reports the
// signal its stop sent (procgroup.Err), not a timeout.
func RunCommand(ctx context.Context, start procgroup.Starter, budget time.Duration, started time.Time, env []string, dir string, stdin []byte, args []string) (string, error) {
	// PLANT (never merged, review 6085965957's two-stall fault): a 0.6 s prefix inside the
	// command's own process before the command's first line.
	cmd := exec.CommandContext(ctx, "sh", append([]string{"-c", `sleep 0.6; exec "$@"`, "sh"}, args...)...)
	cmd.Env = env
	cmd.Dir = dir
	if len(stdin) > 0 {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	// The budget's end stops the command the way every other git and jj of this package is
	// stopped: SIGTERM to the command's own process group, a grace for a git that handles it to
	// remove the lock files it holds, then SIGKILL to the group, and procgroup.WaitDelay as the
	// bound on the wait for a child that still holds the pipes once the command itself has gone
	// (procgroup.Configure, which also answers whether that stop is what ended the command).
	killed := procgroup.Configure(cmd)
	// A start that failed leaves cmd.Process nil (procgroup.Run), so a budget already spent before
	// the start still reads as a timeout below.
	// PLANT (never merged): the runner spends 0.6 s of CPU before Start, a busy loop.
	for spin := time.Now(); time.Since(spin) < 600*time.Millisecond; {
	}
	err := procgroup.Run(cmd, start)
	command := strings.Join(args, " ")
	// The budget ended the command only when that stop is what ended it (procgroup.Configure,
	// never the deadline having passed by the time the wait returned) and the budget is what ended
	// ctx, which is how execRunner reads its own TimedOut (config.go). It is read before the run's
	// own error is propagated: exec reports a command whose Cancel succeeded and which then exited
	// cleanly as the context's error, which is what a git that handles its SIGTERM and exits 0
	// inside the grace does.
	timedOut := (killed() && errors.Is(ctx.Err(), context.DeadlineExceeded)) ||
		(cmd.Process == nil && errors.Is(err, context.DeadlineExceeded))
	runErr := procgroup.Err(err)
	if runErr == nil && !timedOut && cmd.ProcessState.ExitCode() == 0 {
		return strings.TrimSpace(stdout.String()), nil
	}
	report := strings.TrimSpace(stderr.String())
	if note := StaleGitLockNote(report, time.Now()); note != "" {
		report += "\n" + note
	}
	if runErr != nil && !timedOut {
		return "", fmt.Errorf("run %s: %w", command, procgroup.Said(runErr, report))
	}
	if timedOut {
		message := fmt.Sprintf("Command timed out after %s s (ran %.1f s): %s",
			strconv.FormatFloat(budget.Seconds(), 'f', -1, 64), time.Since(started).Seconds(), command)
		if report != "" {
			message += "\n" + report
		}
		return "", errors.New(message)
	}
	return "", fmt.Errorf("Command failed (exit %d): %s\n%s", cmd.ProcessState.ExitCode(), command, report)
}

// gitLockReports are the ways a git or jj failure names the lock file it could not take: jj's git
// backend (gitoxide), and git's own lockfile API, which every git jj runs reports through. Both
// print the path between single quotes without escaping it, so a path may hold a quote of its own:
// each pattern takes the path up to the first quote that the message's fixed text follows, never
// up to the first quote alone. `.` stops at the end of the line, so an opening cut off at a line's
// end takes nothing from the next line. That is a trade: `'([^']+)'`, which these replace, matched
// a newline, so a lock whose path holds a newline got its note then and gets none now; a quote in a
// path is far likelier than a newline, and only the advisory line is lost. pi-shared's
// git-lock.ts reads the same paths.
var gitLockReports = []*regexp.Regexp{
	regexp.MustCompile(`The lockfile at '(.+?)' might need manual deletion`),
	regexp.MustCompile(`Unable to create '(.+?)': File exists`),
}

// staleGitLockNoteFile is the note's wording, held in a data file beside this one rather than in
// a string literal here because packages/pi-shared's own copy of this note is held to that
// wording by a test that reads it and compares the two (git-lock.test.ts): a data file is one
// text both languages read as it stands, where a Go literal is one this package could rewrap or
// split over a const block at any time, breaking a test in another package.
//
// It holds the note's one line and the newline that ends that line, which the note itself does
// not carry: the template below is this text without that newline, and the other side strips
// the same one byte, so the two notes are equal byte for byte.
//
// The numbers in it are internal/launcher's gitLockGuard (15 minutes) and gitLockInterval (a
// minute), which this package cannot read: internal/launcher imports this one. A test there
// renders this note and asserts both numbers, so a change of either without a change of this
// wording fails (TestStaleGitLockNoteCarriesTheLaunchersSweepNumbers).
//
//go:embed stale-git-lock-note.txt
var staleGitLockNoteFile string

// staleGitLockNoteText is that wording as a format template, with the lock's path and its age in
// that order: the data file without the newline that ends its only line.
var staleGitLockNoteText = strings.TrimSuffix(staleGitLockNoteFile, "\n")

// StaleGitLockNote is one line for each git lock file output names that is still on disk: its
// path, how long ago it was last written, and what becomes of it. A git or jj interrupted
// mid-write leaves its lock, and every later write of what it locks fails on it. In an issue pod
// the launcher of the roles container removes such a lock once no process of the container holds
// it open and it is more than 15 minutes old, and looks again every minute
// (internal/launcher's gitLockSweep), and the pod's `workspace-init provision` removes every lock
// it finds before any container starts (RemoveStaleGitLocks, stalelock.go): the role's answer is
// to run its command again, never to remove the file, which another role's live git may hold.
// Under tmux nothing removes it, and the last sentence is what stands where a pod's image
// predates that sweep. The Legion tools' staleGitLockNote
// (packages/pi-shared/src/git-lock.ts) words the same note for its own jj calls. Empty when
// output names no lock, or each one it names is already gone.
func StaleGitLockNote(output string, now time.Time) string {
	var locks, notes []string
	for _, report := range gitLockReports {
		for _, match := range report.FindAllStringSubmatch(output, -1) {
			if !slices.Contains(locks, match[1]) {
				locks = append(locks, match[1])
			}
		}
	}
	for _, lock := range locks {
		info, err := os.Lstat(lock)
		if err != nil {
			continue
		}
		notes = append(notes, fmt.Sprintf(staleGitLockNoteText, lock, now.Sub(info.ModTime()).Round(time.Second)))
	}
	return strings.Join(notes, "\n")
}

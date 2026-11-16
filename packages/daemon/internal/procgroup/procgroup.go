// Package procgroup makes an exec.Cmd die as a unit when its context ends, instead of leaving a
// child's own child (a helper it spawned, reparented once the direct child is gone) holding its
// pipes open forever. The stop it gives every such command is SIGTERM to the whole group, a grace
// in which a command that handles SIGTERM can clean up after itself, and SIGKILL to the group only
// where something of it is still there when the grace is over.
package procgroup

import (
	"errors"
	"os"
	"os/exec"
	"sync/atomic"
	"syscall"
	"time"
)

// WaitDelay is the one value Configure sets: how long Wait keeps waiting, once the command's own
// leader process has exited, for whatever still holds its output pipes open to close them on its
// own, before giving up and forcing the pipes closed itself. Its clock starts when Cancel returns
// (watchCtx, os/exec), which is after the group's SIGTERM and the grace that follows it, and after
// the group's SIGKILL where anything outlived that grace (Configure): every process of the group
// has had both signals by then and is already dying, so the wait is usually brief — except for a
// descendant that escaped the group itself by calling setsid (never git's or jj's own background
// maintenance, which daemonizes with its descriptors pointed elsewhere, but nothing stops another
// command from spawning one): neither signal reaches it, since both are sent to the group alone,
// so that descendant survives them, and Run then ends only at the command's own deadline plus the
// grace plus WaitDelay. That escaped descendant is WaitDelay's one remaining job. After the
// leader's own clean exit, Cancel never ran at all: a child the leader started earlier is
// untouched and keeps running on its own schedule, holding the pipe open for as long as it likes,
// the same as an escaped descendant. WaitDelay does not stop any of these processes — only Go's
// own wait for them, and exec's own kill at the end of it, which reaches the leader alone — so it
// needs no margin for slow work, only enough to not block on one indefinitely; one second serves a
// probe and a git clone alike.
const WaitDelay = time.Second

// StopGrace is how long Cancel gives the group between its SIGTERM and its SIGKILL: the time a
// command that handles SIGTERM has to clean up after itself. git removes its own lock files from
// its signal handler (its lockfile API), and SIGKILL runs no handler, so a git killed outright can
// leave an index.lock or a ref lock behind that stops every later git and jj of that repository
// (dispatch://INFRA-3098, measured in an issue pod on 2026-11-11 with git 2.47.3: SIGTERM removed
// the lock, SIGKILL left it).
//
// The grace is spent inside Cancel rather than left to os/exec's own wait delay, because that
// delay's clock starts only once Cancel returns and its kill reaches the leader alone (watchCtx),
// so a group killed after it would leave the helper the leader forked. It ends as soon as the
// group is gone rather than being slept in full, because Cmd.Wait blocks until Cancel returns, so
// a blind sleep would add a whole second to every timed-out probe, token command, image probe and
// provisioning command.
//
// One second: the grace and WaitDelay together stay inside the three seconds
// TestFetchKillsGitsWholeProcessTreeWhenItsTimeoutFires (internal/workspace) allows a killed fetch
// past its own timeout, which is what bounds the value, and no constant the tree pins grows for it.
//
// It is exported beside WaitDelay because the rows of internal/workspace bound their own stops by
// it rather than by a copy of their own (stopsignal_test.go, realtools_test.go).
const StopGrace = time.Second

// stopPoll is how often the grace asks whether the group is gone. Short enough that a command
// which leaves on its SIGTERM ends the grace about as soon as the kernel has reported its exit,
// long enough that a command which ignores SIGTERM costs fifty questions over the grace rather
// than thousands.
const stopPoll = 20 * time.Millisecond

// Configure sets cmd's own process group (SysProcAttr.Setpgid), a Cancel that stops the whole
// group instead of the single process exec.CommandContext would kill alone — SIGTERM to the group,
// StopGrace for it to leave on its own, then SIGKILL to the group where anything of it is still
// there — and WaitDelay as the backstop that stops Wait from blocking forever on a pipe something
// still holds open once cmd's own process has exited, cleanly or by that stop. Without this, a
// helper a command's own child spawns (git's git-remote-https, under an https remote) can keep a
// pipe open forever, reparented, after exec.CommandContext's own kill ends only the direct child —
// leaving Wait, and so Run, never returning. The SIGTERM is what lets a command that handles it
// remove the lock files it holds before it goes (StopGrace).
//
// Cancel reports the SIGTERM's own result and never the SIGKILL's. A SIGTERM that answers ESRCH
// found nothing left to stop: Cancel then sends nothing more and answers os.ErrProcessDone, which
// keeps killed false for a command that had already finished. Any other SIGTERM error is returned
// as it is, and the escalation still runs behind it.
//
// It returns killed, KilledByContext's answer for cmd: whether the end of cmd's context made
// Cancel signal the group. A caller reads a timeout from killed, never from the context's deadline
// having passed by the time Run returned: once Wait has seen cmd's own process exit, WaitDelay can
// hold Run open up to WaitDelay longer for a child still holding cmd's pipes, and a deadline that
// passes then ends nothing. The command exited on its own, and its exit is its answer. Wait sees
// an exit only once the kernel has reported it and Go has reaped it, so an exit that comes within
// that latency of the deadline can still meet Cancel, whose signals then reach the unreaped leader
// or a child still in its group: killed is true for such a run. A caller also reads killed before
// it propagates the run's own error, because exec reports a command whose Cancel succeeded and
// which then exited cleanly as the context's error (watchCtx), which is what a command that
// handles its SIGTERM and exits 0 inside the grace does.
//
// Call Configure before setting any of the caller's own SysProcAttr fields (a terminal foreground
// job's Ctty, for one): it replaces cmd.SysProcAttr whole, and the caller's own fields need to
// land on the same struct afterward.
func Configure(cmd *exec.Cmd) (killed func() bool) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		group := -cmd.Process.Pid
		err := syscall.Kill(group, syscall.SIGTERM)
		if errors.Is(err, syscall.ESRCH) {
			// The group was already gone by the time the signal reached it — cmd's own process
			// exited in the race window between ctx firing and this call, and no child of it is
			// left either. Nothing more is sent. exec's watchCtx goroutine treats any other error
			// here as a real cancellation failure, wrapping it as "exec: canceling Cmd: <err>" and
			// returning that from Wait even when the command's own exit was a clean success; it
			// special-cases exactly one answer to mean "nothing to cancel, the process already
			// finished on its own" — an error equivalent to os.ErrProcessDone.
			return os.ErrProcessDone
		}
		if groupOutlivedGrace(group) {
			// The SIGKILL's own error is dropped rather than returned: an ESRCH here is the group
			// having gone between the last question and this signal, and a Cancel that answered it
			// would read as "nothing was killed", which turns a timed-out clone into a retryable
			// network failure (transientCloneFailure, internal/workspace/fetch.go). The SIGTERM's
			// answer is the one this Cancel reports.
			_ = syscall.Kill(group, syscall.SIGKILL)
		}
		return err
	}
	cmd.WaitDelay = WaitDelay
	return KilledByContext(cmd)
}

// groupOutlivedGrace reports whether the process group still answers once StopGrace is over. group
// is the negated leader pid, as kill takes a group. It asks every stopPoll with signal 0, which
// signals nothing and answers ESRCH once no member of the group is left, and stops at the first
// such answer, so a command that leaves on its SIGTERM costs the grace nothing; any other answer
// (EPERM for a member this process may not signal) counts as still there. The window between the
// last question and the caller's SIGKILL is the one a cancel that kills unconditionally has too,
// so the poll can only send fewer signals than the kill alone did.
func groupOutlivedGrace(group int) bool {
	for deadline := time.Now().Add(StopGrace); ; time.Sleep(stopPoll) {
		if errors.Is(syscall.Kill(group, 0), syscall.ESRCH) {
			return false
		}
		if !time.Now().Before(deadline) {
			return true
		}
	}
}

// KilledByContext wraps cmd.Cancel, which exec runs once cmd's context ends before Wait has seen
// cmd's own process exit, and returns killed: whether, once Run or Wait has returned, the end of
// cmd's context ended the command. A Cancel that answers os.ErrProcessDone found the process
// already finished, and does not count. Any other answer does, a failed signal included: exec
// then kills the process itself once WaitDelay has passed (watchCtx), so the context's end still
// ends a command that has not exited by then.
//
// Call it once cmd has the Cancel it runs with: exec.CommandContext's own, which kills cmd's
// process alone, or the caller's.
func KilledByContext(cmd *exec.Cmd) (killed func() bool) {
	var ended atomic.Bool
	cancel := cmd.Cancel
	cmd.Cancel = func() error {
		err := cancel()
		ended.Store(!errors.Is(err, os.ErrProcessDone))
		return err
	}
	return ended.Load
}

// Err turns cmd.Run's error into what a caller should propagate: nil whenever cmd has its own
// real exit to report through cmd.ProcessState.ExitCode() instead — whether that is an ordinary
// *exec.ExitError or exec.ErrWaitDelay (a clean exit whose I/O draining outlived WaitDelay; Go
// returns this only once the process itself already exited, Cmd.Run's doc). Any other error (the
// command never started, or Run failed some other way) is returned unchanged, for the caller to
// propagate as its own.
func Err(err error) error {
	if err == nil {
		return nil
	}
	var exited *exec.ExitError
	if errors.As(err, &exited) || errors.Is(err, exec.ErrWaitDelay) {
		return nil
	}
	return err
}

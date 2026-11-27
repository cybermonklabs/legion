// Package procgroup makes an exec.Cmd die as a unit when its context ends, instead of leaving a
// child's own child (a helper it spawned, reparented once the direct child is gone) holding its
// pipes open forever. The stop it gives every such command is SIGTERM to the whole group, a grace
// in which a command that handles SIGTERM can clean up after itself, and SIGKILL to the group only
// where something of it is still there when the grace is over. It also runs a command through the
// start its caller chose (Starter, Run), which is how a process that reaps children it does not
// hold, a container's PID 1, keeps each exit of its own commands for the wait that reads it.
package procgroup

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/sjawhar/legion/daemon/internal/programbound"
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

// WaitDelayBound is WaitDelay as Configure reads it, through programbound (WaitDelayNow): a variable
// so that cmd/legion's tests, whose commands copy their stand-ins' output on a loaded machine, can
// give it their own deadline, since a copy a loaded machine left unscheduled past WaitDelay after
// the command's exit loses what the command printed.
var WaitDelayBound = WaitDelay

// StopGraceBound is StopGrace as the grace reads it, through programbound (StopGraceNow): a
// variable so that cmd/legion's tests, whose stand-ins answer a stop's SIGTERM on a loaded machine,
// can give it their own deadline, since a stand-in a loaded machine left unscheduled past StopGrace
// after its SIGTERM is SIGKILLed and its own answer lost. StopGrace stays the constant the rows of
// internal/workspace bound their stops by.
var StopGraceBound = StopGrace

func init() {
	programbound.Register(&WaitDelayBound, &StopGraceBound)
}

// WaitDelayNow is the WaitDelay a command configured now gets: WaitDelay in a legion binary, and in
// cmd/legion's tests what is left of the test binary's deadline (programbound).
func WaitDelayNow() time.Duration { return programbound.Of(&WaitDelayBound) }

// StopGraceNow is the grace a group stopped now gets between its SIGTERM and its SIGKILL:
// StopGrace in a legion binary, and in cmd/legion's tests what is left of the test binary's
// deadline (programbound).
func StopGraceNow() time.Duration { return programbound.Of(&StopGraceBound) }

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
		time.Sleep(600 * time.Millisecond) // PLANT (never merged): the SIGTERM goes 600 ms after the context ended.
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
	cmd.WaitDelay = WaitDelayNow()
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
	for deadline := time.Now().Add(StopGraceNow()); ; time.Sleep(stopPoll) {
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
// returns this only once the process itself already exited, Cmd.Run's doc). A command a signal
// ended has no exit of its own (ExitCode answers -1), so its *exec.ExitError (`signal: killed`)
// is returned unchanged: the kill is what happened, and a caller that read the exit code instead
// would report a killed command as one that ran and printed nothing. Any other error (the command
// never started, or Run failed some other way) is returned unchanged too, for the caller to
// propagate as its own.
func Err(err error) error {
	if err == nil || Ended(err).Exited != nil || errors.Is(err, exec.ErrWaitDelay) {
		return nil
	}
	return err
}

// Ending is how a command ended, read out of its run's error (Ended).
type Ending struct {
	// Exited is the run's *exec.ExitError when the command exited on its own, so that its
	// ExitCode is the status the command chose, and nil otherwise.
	Exited *exec.ExitError
	// Signal is the signal's own words (`signal: killed`) when a signal ended the command, which
	// leaves it no status: its ExitCode answers -1.
	Signal string
}

// String is the ending as a failure's message names it: `exit 3`, the signal's words, or "" for
// a run's error that says neither.
func (e Ending) String() string {
	if e.Exited != nil {
		return fmt.Sprintf("exit %d", e.Exited.ExitCode())
	}
	return e.Signal
}

// Ran is whether the command reached an end of its own: an exit or a signal's death. A run's error
// that says neither (a start that failed) is false.
func (e Ending) Ran() bool {
	return e.Exited != nil || e.Signal != ""
}

// ExitedWith is whether the command exited with code, an exit of its own: a signal's death, whose
// ExitCode answers -1, never is.
func (e Ending) ExitedWith(code int) bool {
	return e.Exited != nil && e.Exited.ExitCode() == code
}

// Ended reads err, the error a command's run answered, as how the command ended. Both of the
// Ending's fields are empty for any err that says neither: nil, a start that failed,
// exec.ErrWaitDelay. Code that reads a status or a signal out of a run's error reads it here, so
// a signal's death is never read as the status -1; a caller that reads the status from
// cmd.ProcessState passes the run's error through Err instead.
func Ended(err error) Ending {
	var ended *exec.ExitError
	if !errors.As(err, &ended) || ended.ProcessState == nil {
		return Ending{}
	}
	if ended.Exited() {
		return Ending{Exited: ended}
	}
	return Ending{Signal: ended.String()}
}

// Said is err with output beneath it, on a line of its own, when err is a signal's death (Ended)
// and output, trimmed, says anything: a command a signal ended answered no status, but what it
// printed before the signal may say why it was failing, and a caller that propagates err alone
// would drop it, where the same command's failed exit carries it. Any other err is returned
// unchanged: an exit's caller reads the output beside its status itself, and a command that never
// started printed nothing. A caller whose command can print a credential scrubs output first.
func Said(err error, output string) error {
	if trimmed := strings.TrimSpace(output); trimmed != "" && Ended(err).Signal != "" {
		return fmt.Errorf("%w\n%s", err, trimmed)
	}
	return err
}

// Starter starts cmd and answers done, which the caller calls once cmd.Wait has read cmd's exit,
// and only when the start succeeded. The pod's launcher is its container's PID 1 and reaps every
// exited child it does not hold, so it passes one that holds the child out of that reaper until
// its own wait has read the exit (internal/launcher's pidGuard): a child that reaper took first
// fails its own wait with `waitid: no child processes`, or under gVisor at times `wait: no child
// processes` or `waitid: no such process`. `legion worker-shim` passes its subreaper.Guard's
// (internal/subreaper) for the same reason: under --reap-orphans it reaps every exited child it
// does not hold. Every other process passes NoReaper. There is no default: every caller names the
// one it runs under, so code the launcher or the shim reaches cannot start a child through one it
// did not choose (internal/launcher's
// TestEveryChildTheLaunchersProcessWaitsForStartsWhereNoReapTakesIt and internal/shim's
// TestEveryChildTheShimWaitsForStartsThroughItsGuard read every start each process reaches).
type Starter func(cmd *exec.Cmd) (done func(), err error)

// NoReaper is the Starter of a process that reaps no child it did not start itself: cmd.Start,
// and nothing to do once the wait has read the exit.
func NoReaper(cmd *exec.Cmd) (func(), error) {
	return func() {}, cmd.Start()
}

// Run is cmd.Run with the start the caller chose (Starter, which may not be nil): a start that
// fails is answered as it is, with cmd.Process left nil as Run's own leaves it, and otherwise
// cmd.Wait's answer, with done called once the wait has read the exit.
func Run(cmd *exec.Cmd, start Starter) error {
	if start == nil {
		panic("procgroup.Run: no Starter for " + cmd.Path + "; pass NoReaper outside a process that reaps")
	}
	done, err := start(cmd)
	if err != nil {
		return err
	}
	defer done()
	return cmd.Wait()
}

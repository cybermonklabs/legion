package main

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"sync"
	"syscall"
)

// mainNotified are the signals main's context is cancelled by (main.go's signal.NotifyContext),
// which are the only ones a cause of this process's context can name.
var mainNotified = []syscall.Signal{syscall.SIGINT, syscall.SIGTERM}

// endedBySignal is the status for a context whose cause is one of those signals, or the SIGHUP or
// SIGQUIT hangupOrQuitCancels names (errHangup, errQuit), and whether it is one: 128+<signal>, the
// status a shell reads a death by that signal as, which is what runGhChild answers for a gh a
// signal killed and what this command answers for a signal that came before gh was started at all.
//
// The cause is where that signal is left: signal.NotifyContext's own goroutine takes the signal,
// cancels with a cause naming it and raises it no further. Each name is matched against the
// cause os/signal makes of that signal, the signal's own name and " signal received" (its
// signalCtx), built here from the signal rather than written out again: the error it makes
// carries no signal to read, so the words are the only way back to the name, and the two
// spellings of them are one edit of os/signal apart.
func endedBySignal(cause error) (status int, ok bool) {
	if sig, ok := hangupOrQuit(cause); ok {
		return 128 + int(sig), true
	}
	for _, sig := range mainNotified {
		if cause.Error() == sig.String()+" signal received" {
			return 128 + int(sig), true
		}
	}
	return 0, false
}

var (
	errHangup = errors.New(syscall.SIGHUP.String() + " signal received")
	errQuit   = errors.New(syscall.SIGQUIT.String() + " signal received")
)

// hangupOrQuit is the signal cause names where it is one hangupOrQuitCancels cancels with.
func hangupOrQuit(cause error) (syscall.Signal, bool) {
	switch {
	case errors.Is(cause, errHangup):
		return syscall.SIGHUP, true
	case errors.Is(cause, errQuit):
		return syscall.SIGQUIT, true
	}
	return 0, false
}

// hangupOrQuitCancels is ctx, cancelled as well when a SIGHUP or a SIGQUIT reaches this process
// before stop is called, with errHangup or errQuit. It is for a command's own work that runs in a
// process group of its own: `legion gh`'s help reads (readGhHelp), the leak guard's children,
// which `legion gh` and `legion leak-check` both run, and the curl of the credential helper's fork
// read (readForkUpstream) (leakguard's Guard.run starts each with procgroup.Configure under the
// context it is given), and the work of each command stoppingOnHangupOrQuit runs. A pane that
// closes sends its SIGHUP, and a terminal's quit key its SIGQUIT, to the terminal's foreground
// group, which that work is not in, and main catches neither, so without this either signal would
// kill this process where it stood and leave the group running, reparented and waited for by
// nobody. A SIGQUIT caught here ends the command as a SIGTERM does, so it prints no goroutine dump
// while this watches. stop ends the watch and cancels the context, and a call after the first does
// nothing. A signal that arrived just before stop is still read as one, so a caller reads the
// context's cause after stop, and finds errHangup or errQuit there whenever this took one.
//
// A SIGHUP this process was started with ignored is left alone, for the reason runGhChild arms no
// forwarding of it there: arming it would undo a `nohup gh …`. SIGQUIT is watched whatever this
// process was started with: the Go runtime keeps an inherited ignore of SIGHUP and SIGINT alone,
// and puts its own handler over one of SIGQUIT, which ends the process with a goroutine dump
// (sigInstallGoHandler, runtime/signal_unix.go; runGit's note in git.go), so a process started
// with SIGQUIT ignored would die of it all the same without this watch.
func hangupOrQuitCancels(ctx context.Context) (context.Context, func()) {
	watched := []os.Signal{syscall.SIGQUIT}
	if !signal.Ignored(syscall.SIGHUP) {
		watched = append(watched, syscall.SIGHUP)
	}
	cancelled, cancel := context.WithCancelCause(ctx)
	cause := func(sig os.Signal) error {
		if sig == syscall.SIGQUIT {
			return errQuit
		}
		return errHangup
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, watched...)
	done, ended := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(ended)
		select {
		case sig := <-signals:
			cancel(cause(sig))
		case <-done:
		}
	}()
	return cancelled, sync.OnceFunc(func() {
		signal.Stop(signals)
		close(done)
		<-ended
		// The watch may have taken done over a signal already in the channel; Stop has made
		// sure no other comes.
		select {
		case sig := <-signals:
			cancel(cause(sig))
		default:
		}
		cancel(nil)
	})
}

// signalled is the status a signal that cancelled ctx ends this command with (endedBySignal), and
// whether one did.
func signalled(ctx context.Context) (int, bool) {
	if cause := context.Cause(ctx); cause != nil {
		return endedBySignal(cause)
	}
	return 0, false
}

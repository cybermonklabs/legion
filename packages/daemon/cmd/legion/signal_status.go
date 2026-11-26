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

// endedBySignal is the status for a context whose cause is one of those signals, or the SIGHUP
// hangupCancels names (errHangup), and whether it is one: 128+<signal>, the status a shell reads a
// death by that signal as, which is what runGhChild answers for a gh a signal killed and what this
// command answers for a signal that came before gh was started at all.
//
// The cause is where that signal is left: signal.NotifyContext's own goroutine takes the signal,
// cancels with a cause naming it and raises it no further. Each name is matched against the
// cause os/signal makes of that signal, the signal's own name and " signal received" (its
// signalCtx), built here from the signal rather than written out again: the error it makes
// carries no signal to read, so the words are the only way back to the name, and the two
// spellings of them are one edit of os/signal apart.
func endedBySignal(cause error) (status int, ok bool) {
	if errors.Is(cause, errHangup) {
		return 128 + int(syscall.SIGHUP), true
	}
	for _, sig := range mainNotified {
		if cause.Error() == sig.String()+" signal received" {
			return 128 + int(sig), true
		}
	}
	return 0, false
}

var errHangup = errors.New(syscall.SIGHUP.String() + " signal received")

// hangupCancels is ctx, cancelled with errHangup as well when a SIGHUP reaches this process before
// stop is called. It is for a command's own work that runs in a process group of its own: `legion
// gh`'s git reads of the repository's configuration and remotes (ghRepoGitConfigRefusal,
// ghroute.ReadGitRemotes) and its help reads (ghroute.ReadHelp), the leak guard's children, which
// `legion gh` and `legion leak-check` both run, and the curl of the credential helper's fork read
// (readForkUpstream) (each started with procgroup.Configure under the context it is given). A
// pane that closes sends its SIGHUP to the terminal's foreground group,
// which that work is not in, and main catches no SIGHUP, so without this the signal would kill
// this process where it stood and leave the group running, reparented and waited for by nobody.
// stop ends the watch and cancels the context, and a call after the first does nothing. A SIGHUP
// that arrived just before stop is still read as one, so a caller reads the context's cause after
// stop, and finds errHangup there whenever this took one.
//
// Where this process was started with SIGHUP ignored it is ctx itself and arms nothing, for the
// reason runGhChild arms no forwarding of it there: arming it would undo a `nohup gh …`.
func hangupCancels(ctx context.Context) (context.Context, func()) {
	if signal.Ignored(syscall.SIGHUP) {
		return ctx, func() {}
	}
	hup, cancel := context.WithCancelCause(ctx)
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGHUP)
	done, watched := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(watched)
		select {
		case <-signals:
			cancel(errHangup)
		case <-done:
		}
	}()
	return hup, sync.OnceFunc(func() {
		signal.Stop(signals)
		close(done)
		<-watched
		// The watch may have taken done over a SIGHUP already in the channel; Stop has made
		// sure no other comes.
		select {
		case <-signals:
			cancel(errHangup)
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

// packages/envoy/cmd/agent-secrets/secret_prompt_watch_unix.go
//go:build (linux || darwin) && (amd64 || arm64)

// The value prompt's signal watcher on Linux and macOS (secret_prompt_unix.go holds its reader): it
// reports each signal to the reader through a wake pipe, and before a stop discards what the
// terminal holds unread. Its wait for an event keeps the reader's deadline rule (waitBound).
package main

import (
	"errors"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type promptSignal struct {
	sig syscall.Signal
	err error
}

// promptWatch is the prompt's signal watcher. It never changes the terminal's settings: for each
// signal it writes one byte, the signal's number, to the wake pipe, so an event and the wake-up that
// announces it are one thing, taken only by next. A stop's byte is written before the stop, under
// stopping, which the watcher holds until the process resumes: the reader can keep a stop off its
// baseline read and label (whileHeld), and a reader that takes the byte waits the stop out.
type promptWatch struct {
	fd           int // the wake pipe's read end, polled with the terminal
	wake, notify *os.File
	signals      chan os.Signal
	done, joined chan struct{}
	flush        func() error // discards the terminal's unread input before a stop (flushInput)
	stopping     sync.Mutex
	stopErr      error // why a stop failed, written under stopping
}

// watchPromptSignals starts the watcher. SIGCONT is a wake only; SIGTTIN and SIGTTOU retain their
// default actions, and a signal whose kernel disposition is SIG_IGN stays ignored, the terminal
// control character of its key answered in ignored.
func watchPromptSignals(flush func() error) (*promptWatch, []int, error) {
	watched := []os.Signal{syscall.SIGCONT}
	var ignored []int
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT, syscall.SIGHUP, syscall.SIGTSTP} {
		isIgnored, err := promptSignalIgnored(sig)
		if err != nil {
			return nil, nil, err
		}
		if !isIgnored {
			watched = append(watched, sig)
		} else {
			switch sig {
			case syscall.SIGINT:
				ignored = append(ignored, unix.VINTR)
			case syscall.SIGQUIT:
				ignored = append(ignored, unix.VQUIT)
			case syscall.SIGTSTP:
				ignored = append(ignored, unix.VSUSP)
			}
		}
	}
	wake, notify, err := os.Pipe()
	if err != nil {
		return nil, nil, err
	}
	w := &promptWatch{fd: int(wake.Fd()), wake: wake, notify: notify, signals: make(chan os.Signal, 8),
		done: make(chan struct{}), joined: make(chan struct{}), flush: flush}
	signal.Notify(w.signals, watched...)
	go w.run()
	return w, ignored, nil
}

func (w *promptWatch) run() {
	defer close(w.joined)
	for {
		select {
		case s := <-w.signals:
			sig := s.(syscall.Signal)
			if !promptStopSignal(sig) {
				_, _ = w.notify.Write([]byte{byte(sig)})
				continue
			}
			w.stopping.Lock()
			// The byte goes first, so the reader never touches the terminal while the stop
			// takes effect: a poll in flight can report the terminal ready for input the flush
			// below discards, and a read on that readiness can be caught mid-syscall by the
			// stop, then raise SIGTTIN once bg leaves the job in the background.
			_, _ = w.notify.Write([]byte{byte(sig)})
			// What the terminal holds unread is the entry the stop discards; left there it
			// would reach the shell once the stop gives it the terminal. A failed flush and a
			// failed stop are both recorded here, under stopping where next reads stopErr, so
			// the refused entry reports the first of them.
			if err := w.flush(); err != nil && w.stopErr == nil {
				w.stopErr = err
			}
			if err := stopBy(sig); err != nil && w.stopErr == nil {
				w.stopErr = err
			}
			// On Darwin stopBy must clear os/signal's handler bookkeeping before Notify can
			// reinstall it. On Linux it is still installed.
			signal.Notify(w.signals, sig)
			w.stopping.Unlock()
		case <-w.done:
			return
		}
	}
}

// pollWatch is the poll ready waits on the wake pipe with: unix.Poll, which a test replaces to
// interrupt that wait (EINTR) as signals would.
var pollWatch = unix.Poll

// ready reports whether the watcher has an event the reader has not taken, waiting up to timeout
// milliseconds for one, and never past until (waitBound, counted again at each attempt; the zero
// time sets no moment): signals that keep interrupting the wait cannot hold it there, and one that
// interrupts it once until has passed ends it with no event.
func (w *promptWatch) ready(timeout int, until time.Time) (bool, error) {
	fds := []unix.PollFd{{Fd: int32(w.fd), Events: unix.POLLIN}}
	for {
		n, err := pollWatch(fds, waitBound(timeout, until))
		if errors.Is(err, unix.EINTR) {
			if passed(until) {
				return false, nil
			}
			continue
		}
		return n > 0, err
	}
}

// next takes the watcher's next event, if one comes within timeout milliseconds. A stop's event is
// answered only once the stop is over, with its failure if it failed.
func (w *promptWatch) next(timeout int, until time.Time) (promptSignal, bool, error) {
	if ready, err := w.ready(timeout, until); err != nil || !ready {
		return promptSignal{}, false, err
	}
	var b [1]byte
	if _, err := unix.Read(w.fd, b[:]); err != nil {
		return promptSignal{}, false, err
	}
	event := promptSignal{sig: syscall.Signal(b[0])}
	if promptStopSignal(event.sig) {
		w.stopping.Lock()
		event.err, w.stopErr = w.stopErr, nil
		w.stopping.Unlock()
	}
	return event, true, nil
}

// stop joins the watcher, so no late resume can race the reader's final restore, and answers
// what the reader has not taken: a death arriving at the read's end, which still takes effect
// after restoration, including one Notify queued but the watcher did not receive; else a stop.
func (w *promptWatch) stop() promptSignal {
	close(w.done)
	<-w.joined
	signal.Stop(w.signals)
	defer w.wake.Close()
	defer w.notify.Close()
	var pending promptSignal
	note := func(sig syscall.Signal) {
		if sig != syscall.SIGCONT && (pending.sig == 0 || !promptStopSignal(sig)) {
			pending.sig = sig
		}
	}
	for {
		event, took, err := w.next(0, time.Time{})
		if err != nil || !took {
			break
		}
		if event.err != nil {
			pending.err = event.err
		}
		note(event.sig)
	}
	for len(w.signals) != 0 {
		note((<-w.signals).(syscall.Signal))
	}
	return pending
}

func promptStopSignal(sig os.Signal) bool {
	return sig == syscall.SIGTSTP
}

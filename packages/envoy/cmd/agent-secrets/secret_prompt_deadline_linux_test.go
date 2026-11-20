// packages/envoy/cmd/agent-secrets/secret_prompt_deadline_linux_test.go
//go:build linux && (amd64 || arm64)

// The value prompt's reader (readHidden) and its value deadline at a real pseudo-terminal: a
// deadline that passes with nothing typed, with an entry under way, inside a paste, at a wait an
// interrupt cut short, and in the drain after the line; the rule every wait the deadline bounds
// keeps (waitBound); and a create whose value ends past its window. The pseudo-terminal helpers
// these tests share with the rest of the reader's tests (openPTY, promptRead, awaitEchoOff, unread,
// shown) are in secret_tty_linux_test.go. Linux-only, as that file is.
package main

import (
	"bytes"
	"errors"
	"io"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestPromptEndsAtTheValueDeadline: a prompt handed a deadline (a create's value read) that gets
// nothing gives up at the deadline, with errValueTimedOut and no line, and gives the terminal back
// with echo on, as any other end of the prompt does. The deadline's clock stands still until the
// prompt's first wait for input and runs from there (clockFromFirstWait), where the reader's own
// goroutine reads that echo is off: a machine that holds this process longer than the deadline,
// before that wait or after it, neither uses the deadline up before the prompt waits nor ends the
// prompt before echo off is seen.
func TestPromptEndsAtTheValueDeadline(t *testing.T) {
	_, terminal := openPTY(t) // openPTY's cleanup holds the controller open for the whole test
	deadline := time.Now().Add(500 * time.Millisecond)
	answer := make(chan promptRead, 1)
	waited := clockFromFirstWait(t, func() time.Time { return deadline.Add(-500 * time.Millisecond) }, nil)
	go func() {
		line, err := readHidden(terminal, deadline, nil, nil)
		answer <- promptRead{line, err}
	}()
	awaitFirstWait(t, waited, answer)
	select {
	case got := <-answer:
		if got.line != nil || !errors.Is(got.err, errValueTimedOut) {
			t.Fatalf("readHidden = %q, %v; want no line and errValueTimedOut", got.line, got.err)
		}
	case <-time.After(promptJobWait):
		t.Fatalf("the prompt was still reading %s after its first wait began, past its 500ms deadline", promptJobWait)
	}
	if lflag(t, terminal)&unix.ECHO == 0 {
		t.Fatal("the prompt left echo off at its deadline")
	}
}

// TestPromptAtTheValueDeadlineMidEntryReadsTheEntryToItsEnd: a deadline that passes once part of the
// entry has been typed refuses the entry, but the prompt keeps the terminal: it says so at once
// (onDiscard, with errValueTimedOut), reads the rest of the entry to the line's end, and only then
// answers errValueTimedOut, so what is typed after the deadline is neither echoed nor left for the
// shell. The entry's first part is queued at the prompt's first wait for input, before the
// deadline's clock starts there (clockFromFirstWait), so the entry is under way when the deadline
// passes however long the machine holds this process. The rest is typed once the prompt has either
// said so or answered, whichever comes first, so a prompt that gives the entry up at the deadline
// shows the rest reaching the shell. The pseudo-terminal is not this process's controlling
// terminal, so no key of it sends a signal: the reason the notice reads names no interrupt key, and
// no open paste.
func TestPromptAtTheValueDeadlineMidEntryReadsTheEntryToItsEnd(t *testing.T) {
	controller, terminal := openPTY(t)
	deadline := time.Now().Add(2 * time.Second)
	noticed := make(chan error, 1)
	answer := make(chan promptRead, 1)
	const head = "fakehead-"
	waited := clockFromFirstWait(t, func() time.Time { return deadline.Add(-2 * time.Second) }, func(fd int) {
		if _, err := controller.Write([]byte(head)); err != nil {
			t.Error(err)
		}
		awaitQueued(t, fd, len(head))
	})
	go func() {
		line, err := readHidden(terminal, deadline, nil, func(reason error) { noticed <- reason })
		answer <- promptRead{line, err}
	}()
	awaitFirstWait(t, waited, answer)
	var early *promptRead
	select {
	case reason := <-noticed:
		var notice valueDeadlineNotice
		if !errors.Is(reason, errValueTimedOut) || !errors.As(reason, &notice) || notice.interrupt != 0 || notice.inPaste {
			t.Fatalf("the prompt refused the entry for %#v; want a valueDeadlineNotice naming no interrupt key and no open paste, on a terminal that is not the controlling one", reason)
		}
	case got := <-answer:
		early = &got
	case <-time.After(promptJobWait):
		t.Fatalf("the prompt neither refused the entry nor answered %s after its first wait began, past its 2s deadline", promptJobWait)
	}
	if _, err := controller.Write([]byte("FAKETAIL\r")); err != nil {
		t.Fatal(err)
	}
	got := early
	if got == nil {
		select {
		case read := <-answer:
			got = &read
		case <-time.After(promptJobWait):
			t.Fatalf("the prompt had not answered %s after the entry's line ended", promptJobWait)
		}
	}
	if got.line != nil || !errors.Is(got.err, errValueTimedOut) {
		t.Fatalf("readHidden = %q, %v; want no line and errValueTimedOut", got.line, got.err)
	}
	if n := unread(t, terminal); n != 0 || bytes.Contains(shown(t, controller), []byte("FAKETAIL")) {
		t.Fatalf("the rest of the entry reached the shell: %d bytes left unread, or it was echoed", n)
	}
}

// firstWait is what clockFromFirstWait saw at the reader's first wait for input: the moment it
// began, from which the prompt's clock runs, and whether the reader had turned echo off by then.
type firstWait struct {
	at      time.Time
	echoOff bool
}

// clockFromFirstWait replaces the prompt's clock (promptNow) for the rest of t with one that stands
// at start() until the reader's first wait for input (readTerminal), and runs as the machine's own
// clock from the moment that wait begins: a value deadline set from start then counts from the
// prompt's first wait, so a machine that holds the process between the setting of the deadline and
// that wait uses none of it. typed, unless it is nil, runs at that wait on the reader's goroutine,
// with the terminal's fd, before the clock starts: what it writes is queued before the deadline can
// pass. It answers a channel that gets the first wait's facts once. Only the reader's goroutine
// calls promptNow, start and readTerminal.
func clockFromFirstWait(t *testing.T, start func() time.Time, typed func(fd int)) <-chan firstWait {
	t.Helper()
	restoreNow, real := promptNow, readTerminal
	t.Cleanup(func() { promptNow, readTerminal = restoreNow, real })
	var began time.Time // the zero time until the reader's first wait
	promptNow = func() time.Time {
		if began.IsZero() {
			return start()
		}
		return start().Add(time.Since(began))
	}
	waited := make(chan firstWait, 1)
	readTerminal = func(fd, wake int, buf []byte, timeout int) (int, error) {
		if began.IsZero() {
			if typed != nil {
				typed(fd)
			}
			mode, err := unix.IoctlGetTermios(fd, unix.TCGETS)
			began = time.Now()
			waited <- firstWait{at: began, echoOff: err == nil && mode.Lflag&unix.ECHO == 0}
		}
		return real(fd, wake, buf, timeout)
	}
	return waited
}

// awaitFirstWait waits for the reader's first wait for input (clockFromFirstWait) and answers when
// it began. It fails t where the reader answered before it (the clock stands still until then, so
// no deadline can have passed), never reached it within promptJobWait, or had not turned echo off
// by then.
func awaitFirstWait[T any](t *testing.T, waited <-chan firstWait, answer <-chan T) time.Time {
	t.Helper()
	select {
	case w := <-waited:
		if !w.echoOff {
			t.Fatal("the reader began to wait for input with echo still on")
		}
		return w.at
	case got := <-answer:
		t.Fatalf("the reader answered %+v before its first wait for input", got)
	case <-time.After(promptJobWait):
		t.Fatalf("the reader had not begun to wait for input %s on", promptJobWait)
	}
	return time.Time{}
}

// stillPromptClock replaces the prompt's clock (promptNow) for the rest of t with one that stands
// at start until the test moves it, through the function it answers: the test, not the machine's
// load, then decides at which of the reader's steps the value deadline passes. Once the reader
// runs, only its goroutine (a readTerminal or pollWatch the test wraps) may move the clock.
func stillPromptClock(t *testing.T, start time.Time) func(time.Time) {
	t.Helper()
	restore := promptNow
	now := start
	promptNow = func() time.Time { return now }
	t.Cleanup(func() { promptNow = restore })
	return func(at time.Time) { now = at }
}

// awaitQueued waits until the terminal fd holds at least n bytes unread, so a write to its
// controller has reached the reader's side before the reader polls for it; it reports through
// t.Error, for it runs on the reader's goroutine.
func awaitQueued(t *testing.T, fd, n int) {
	for budget := time.Now().Add(promptJobWait); ; {
		queued, err := unix.IoctlGetInt(fd, unix.TIOCINQ)
		if err != nil {
			t.Error(err)
			return
		}
		if queued >= n {
			return
		}
		if time.Now().After(budget) {
			t.Errorf("the terminal held %d of %d bytes written to it %s on", queued, n, promptJobWait)
			return
		}
		_, _ = unix.Poll([]unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}, 50)
	}
}

// pasteHeadThenRest runs readHidden at a fresh terminal with a value deadline of two seconds on a
// clock that stands still (stillPromptClock), and types the head of a bracketed paste at the
// prompt's first wait, which it lets begin only once the terminal holds the head: a machine that
// delivers the head late delays that wait, rather than leave it to time out before the head comes.
// atWait runs at each of the reader's waits once it has read the whole head, on the reader's
// goroutine, with the wait's bound, the deadline, the clock's mover and a write to the terminal
// that returns once the bytes are queued there; it answers whether it took the wait in hand, and
// if so what the wait returns. The prompt's notice sets *noticed, where noticed is not nil.
// pasteHeadThenRest answers what readHidden returned, and the reason of the notice it gave, nil
// for none, once it has checked that nothing of the paste reached the shell.
func pasteHeadThenRest(t *testing.T, atWait func(timeout int, deadline time.Time, move func(time.Time), write func(string)) (bool, int, error), noticed *bool) (promptRead, error) {
	t.Helper()
	controller, terminal := openPTY(t)
	start := time.Now()
	deadline := start.Add(2 * time.Second)
	move := stillPromptClock(t, start)
	real := readTerminal
	t.Cleanup(func() { readTerminal = real })
	const head = "\x1b[200~pastehead"
	write := func(text string) {
		if _, err := controller.Write([]byte(text)); err != nil {
			t.Error(err)
		}
	}
	// Only the reader's goroutine touches these until it answers.
	headSent, read := false, 0
	var notice error
	readTerminal = func(fd, wake int, buf []byte, timeout int) (int, error) {
		switch {
		case !headSent:
			headSent = true
			write(head)
			awaitQueued(t, fd, len(head))
		case read == len(head):
			if took, n, err := atWait(timeout, deadline, move, func(text string) {
				write(text)
				awaitQueued(t, fd, len(text))
			}); took {
				return n, err
			}
		}
		n, err := real(fd, wake, buf, timeout)
		if n > 0 && read < len(head) {
			read += n
		}
		return n, err
	}
	answer := make(chan promptRead, 1)
	go func() {
		line, err := readHidden(terminal, deadline, nil, func(reason error) {
			notice = reason
			if noticed != nil {
				*noticed = true
			}
		})
		answer <- promptRead{line, err}
	}()
	var got promptRead
	select {
	case got = <-answer:
	case <-time.After(promptJobWait):
		t.Fatalf("the prompt had not answered %s on", promptJobWait)
	}
	if n := unread(t, terminal); n != 0 || bytes.Contains(shown(t, controller), []byte("PASTE_")) {
		t.Fatalf("the paste's rest reached the shell: %d bytes left unread, or it was echoed", n)
	}
	return got, notice
}

// TestPromptAtTheValueDeadlineAnInterruptedWaitStillEndsThere: a wait for input that ends with
// nothing read is tried again, and the retry still ends at the value deadline, so the deadline is
// acted on there as it is when nothing ends the wait early: a paste open at the deadline is refused
// at once (onDiscard, naming the open paste), and its rest, which comes at the prompt's next wait,
// is read through its closing mark, so none of it reaches the shell, and is refused with the
// entry, errValueTimedOut. Each way promptTerminal.read tries a wait again has its case: a signal
// interrupts the poll (EINTR), the signal watcher has an event the reader takes first, and the
// input poll reported is gone by the time it is read. The first wait after the paste's head, a
// wait that begins before the deadline, ends that way as the deadline passes: a retry that waited
// that wait's whole bound again read the rest as if it had come in time, and refused the entry as
// more than one line, its deadline never acted on. The rest goes at the first wait after that one
// that can block (a bound other than zero). The clock stands still until the ended wait moves it
// to the deadline, so no step of the machine's own decides which wait the deadline falls in.
func TestPromptAtTheValueDeadlineAnInterruptedWaitStillEndsThere(t *testing.T) {
	for _, tc := range []struct {
		name string
		n    int
		err  error
	}{
		{"poll interrupted by a signal (EINTR)", 0, unix.EINTR},
		{"the signal watcher's event", -2, nil},
		{"input gone before the read", -1, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const rest = "\recho PASTE_$((2+3))\r\x1b[201~"
			ended, beganInTime, restSent, noticeFirst := false, false, false, false
			var noticed bool
			got, notice := pasteHeadThenRest(t, func(timeout int, deadline time.Time, move func(time.Time), write func(string)) (bool, int, error) {
				switch {
				case !ended:
					ended, beganInTime = true, promptNow().Before(deadline)
					move(deadline)
					return true, tc.n, tc.err
				case !restSent && timeout != 0:
					restSent, noticeFirst = true, noticed
					write(rest)
				}
				return false, 0, nil
			}, &noticed)
			if !ended || !beganInTime {
				t.Fatalf("readHidden = %q, %v; no wait that began before the deadline ended with nothing read (ended: %t, begun in time: %t), so this run says nothing of a retry", got.line, got.err, ended, beganInTime)
			}
			if got.line != nil || !errors.Is(got.err, errValueTimedOut) {
				t.Fatalf("readHidden = %q, %v; want no line and errValueTimedOut, for a paste open when the deadline passed", got.line, got.err)
			}
			var said valueDeadlineNotice
			if !restSent || !noticeFirst || !errors.As(notice, &said) || !said.inPaste {
				t.Fatalf("the prompt's notice %#v (the paste's rest sent: %t, after the notice: %t); want a valueDeadlineNotice naming the open paste, given at the deadline, before the rest came", notice, restSent, noticeFirst)
			}
		})
	}
}

// TestPromptRefusesInputReadPastTheValueDeadlineAsLate: input a read returns once the value
// deadline has passed came late, whatever it holds. The paste's rest is queued at the terminal as
// the deadline passes, while the prompt waits after the paste's head, and the read that returns it
// closes the paste and the line at once: the entry is refused as late (errValueTimedOut), not as
// the more than one line its bytes hold, and none of it reaches the shell. The clock stands still
// until the wait moves it to the deadline, once the rest is queued, so that read ends past the
// deadline however slowly this test runs.
func TestPromptRefusesInputReadPastTheValueDeadlineAsLate(t *testing.T) {
	const rest = "\recho PASTE_$((2+3))\r\x1b[201~"
	queued := false
	got, _ := pasteHeadThenRest(t, func(_ int, deadline time.Time, move func(time.Time), write func(string)) (bool, int, error) {
		if !queued {
			queued = true
			write(rest)
			move(deadline)
		}
		return false, 0, nil
	}, nil)
	if !queued {
		t.Fatalf("readHidden = %q, %v before the paste's rest was queued", got.line, got.err)
	}
	if got.line != nil || !errors.Is(got.err, errValueTimedOut) {
		t.Fatalf("readHidden = %q, %v; want no line and errValueTimedOut, for a paste whose rest was read past the deadline", got.line, got.err)
	}
}

// TestPromptRefusesInputItsDrainReadsPastTheValueDeadlineAsLate: the drain after the line makes
// the read loop's check (drainAfterTheLine): input it reads once the value deadline has passed came
// late, so the entry is refused as late (errValueTimedOut), not as the more than one line a second
// line makes it, and the drain reads that line, so none of it reaches the shell. The line is typed
// in time; the second line is queued at the drain's first wait, and the clock, which stands still
// until then, moves to the deadline there, so the drain's read ends past it however slowly this
// test runs.
func TestPromptRefusesInputItsDrainReadsPastTheValueDeadlineAsLate(t *testing.T) {
	controller, terminal := openPTY(t)
	start := time.Now()
	deadline := start.Add(2 * time.Second)
	move := stillPromptClock(t, start)
	real := readTerminal
	t.Cleanup(func() { readTerminal = real })
	write := func(fd int, text string) {
		if _, err := controller.Write([]byte(text)); err != nil {
			t.Error(err)
		}
		awaitQueued(t, fd, len(text))
	}
	// Only the reader's goroutine touches these until it answers.
	typed, lineRead, moreSent := false, false, false
	readTerminal = func(fd, wake int, buf []byte, timeout int) (int, error) {
		switch {
		case !typed:
			typed = true
			write(fd, "value\r")
		case lineRead && !moreSent && timeout > 0:
			moreSent = true
			write(fd, "more\r")
			move(deadline)
		}
		n, err := real(fd, wake, buf, timeout)
		if n > 0 && bytes.ContainsAny(buf[:n], "\r\n") {
			lineRead = true
		}
		return n, err
	}
	answer := make(chan promptRead, 1)
	go func() {
		line, err := readHidden(terminal, deadline, nil, func(error) {})
		answer <- promptRead{line, err}
	}()
	var got promptRead
	select {
	case got = <-answer:
	case <-time.After(promptJobWait):
		t.Fatalf("the prompt had not answered %s on", promptJobWait)
	}
	if !moreSent {
		t.Fatalf("readHidden = %q, %v before the drain read anything", got.line, got.err)
	}
	if got.line != nil || !errors.Is(got.err, errValueTimedOut) {
		t.Fatalf("readHidden = %q, %v; want no line and errValueTimedOut, for a second line the drain read past the deadline", got.line, got.err)
	}
	if n := unread(t, terminal); n != 0 || bytes.Contains(shown(t, controller), []byte("more")) {
		t.Fatalf("the second line reached the shell: %d bytes left unread, or it was echoed", n)
	}
}

// TestPromptWatchWaitEndsAtItsDeadlineThoughSignalsKeepInterruptingIt: the watcher's wait for an
// event (promptWatch.ready, which the wait for the foreground repeats every 50 ms) keeps the value
// deadline across the signals that interrupt it, by the read's own rule (waitBound): each attempt
// counts the time left again, none asks to wait past the deadline, and one interrupted once the
// deadline has passed ends the wait with no event, where waitForeground's check of the deadline
// ends the prompt. Every attempt here is interrupted (EINTR), and the clock moves 30 ms at each, as
// a stream of signals would have it; an attempt that the rule would never reach ends the wait. The
// wait must have been interrupted on both sides of the deadline, so a wait that polled something
// else, or gave up at its first interruption, does not pass.
func TestPromptWatchWaitEndsAtItsDeadlineThoughSignalsKeepInterruptingIt(t *testing.T) {
	start := time.Now()
	deadline := start.Add(100 * time.Millisecond)
	move := stillPromptClock(t, start)
	restore := pollWatch
	t.Cleanup(func() { pollWatch = restore })
	const enough = 100
	now := start
	var asked []int
	pastDeadline := 0
	pollWatch = func(_ []unix.PollFd, timeout int) (int, error) {
		asked = append(asked, timeout)
		if timeout < 0 || now.Add(time.Duration(timeout)*time.Millisecond).After(deadline.Add(time.Millisecond)) {
			pastDeadline++
		}
		if len(asked) == enough {
			return 0, nil
		}
		now = now.Add(30 * time.Millisecond)
		move(now)
		return 0, unix.EINTR
	}
	ready, err := (&promptWatch{fd: -1}).ready(50, deadline)
	if len(asked) < 2 || now.Before(deadline) {
		t.Fatalf("ready = %t, %v after %d interrupted attempts, the clock %s past its start; want attempts on both sides of the %s deadline", ready, err, len(asked), now.Sub(start), deadline.Sub(start))
	}
	if ready || err != nil || pastDeadline > 0 || len(asked) == enough {
		t.Fatalf("ready = %t, %v after %d attempts, asking %v ms, %d of them to wait past the deadline; want no event and no error, and no attempt past the deadline", ready, err, len(asked), asked, pastDeadline)
	}
}

// TestWaitBoundEndsAWaitAtItsMomentNeverBefore: waitBound cuts a wait's bound to the time left
// before its moment, rounded up past the next whole millisecond, so a poll given that bound ends at
// the moment or after it, never before, however little time is left: a wait that ended before the
// moment would find the deadline not yet passed, and wait again. It keeps a shorter bound, and a
// bound of -1 (none) where the moment is the zero time, and answers 0 once the moment has passed.
// The clock stands still, so the time left is exactly what each case sets.
func TestWaitBoundEndsAWaitAtItsMomentNeverBefore(t *testing.T) {
	start := time.Now()
	stillPromptClock(t, start)
	for _, left := range []time.Duration{time.Nanosecond, 400 * time.Microsecond, time.Millisecond, 1500 * time.Microsecond, 2 * time.Second} {
		bound := waitBound(-1, start.Add(left))
		if time.Duration(bound)*time.Millisecond < left || bound > int(left.Milliseconds())+1 {
			t.Errorf("waitBound(-1, %s from now) = %d ms; want a bound that reaches the moment, at most a millisecond past it", left, bound)
		}
	}
	for _, tc := range []struct {
		wait  int
		until time.Time
		want  int
	}{
		{-1, time.Time{}, -1},
		{50, time.Time{}, 50},
		{50, start.Add(time.Second), 50},
		{50, start, 0},
		{-1, start.Add(-time.Second), 0},
	} {
		if got := waitBound(tc.wait, tc.until); got != tc.want {
			t.Errorf("waitBound(%d, %s from now) = %d; want %d", tc.wait, tc.until.Sub(start), got, tc.want)
		}
	}
}

// TestGCPSecretCreateStoresNoValueThatEndsPastItsWindow: a value typed at the prompt inside the
// create's window, whose drain then runs past it - a bracketed paste that opens after the line and
// closes only once the window is over - is not stored: the prompt reads that paste through its
// closing mark, and the create then refuses the value as one that did not come in time, never
// calling AddSecretVersion, deletes the empty secret it made, asks the broker to reread the name,
// and exits 1 naming the window and the command to run again. The window's clock stands still
// until the prompt's first wait for input and runs from there (clockFromFirstWait), so a machine
// that holds this process between the create's asking the store and that wait uses none of the
// window; the line is queued at that wait, and the paste opens at the drain's first wait, so each
// comes where the test places it however slowly this test runs. The create counts its window from
// before the store is asked to make the secret (runCreate), so the test learns where the window
// began from the deadline the create hands the prompt.
func TestGCPSecretCreateStoresNoValueThatEndsPastItsWindow(t *testing.T) {
	controller, terminal := openPTY(t)
	const window = 2 * time.Second
	restoreWindow := valueReadWindow
	valueReadWindow = window
	t.Cleanup(func() { valueReadWindow = restoreWindow })
	fake := newFakeSecretManager()
	startFakeSecretManager(t, fake)
	useGCPIdentity(t, "authorized_user", "ada@example.com")
	broker := startSecretBrokerWith(t, gcpTestSettings(), gcpServedBy(fake, gcpTestSettings()))
	useTerminal(t)
	stdinTerminal = func(io.Reader) (int, bool) { return terminal, true }
	var asked time.Time // where the window began; set and read on the create's goroutine, the reader's
	readHidden = func(fd int, deadline time.Time, prompt func(), onDiscard func(error)) ([]byte, error) {
		asked = deadline.Add(-window)
		return readHiddenAtTerminal(fd, deadline, prompt, onDiscard)
	}
	write := func(fd int, text string) {
		if _, err := controller.Write([]byte(text)); err != nil {
			t.Error(err)
		}
		awaitQueued(t, fd, len(text))
	}
	real := readTerminal
	t.Cleanup(func() { readTerminal = real })
	// draining closes at the reader's first wait after the read that ended the line, the drain of
	// what follows the line, once the paste's opening mark is queued there. The terminal turns the
	// typed carriage return into a line feed.
	draining := make(chan struct{})
	lineEnded, drainSeen := false, false
	readTerminal = func(fd, wake int, buf []byte, timeout int) (int, error) {
		if lineEnded && !drainSeen {
			drainSeen = true
			write(fd, "\x1b[200~")
			close(draining)
		}
		n, err := real(fd, wake, buf, timeout)
		if n > 0 && bytes.ContainsAny(buf[:n], "\r\n") {
			lineEnded = true
		}
		return n, err
	}
	waited := clockFromFirstWait(t, func() time.Time { return asked }, func(fd int) { write(fd, "typed-value\r") })
	type result struct {
		stderr string
		code   int
	}
	done := make(chan result, 1)
	go func() {
		_, stderr, code := runSecret("create", "NEW_TEST", "--owner", "me", "--tier", "agent")
		done <- result{stderr, code}
	}()
	// The window ends, on the prompt's clock, window after its first wait began.
	windowEnds := awaitFirstWait(t, waited, done).Add(window)
	select {
	case <-draining:
	case <-time.After(promptJobWait):
		t.Fatalf("the prompt had not drained what follows the line %s on", promptJobWait)
	}
	time.Sleep(time.Until(windowEnds) + 300*time.Millisecond)
	if _, err := controller.Write([]byte("\x1b[201~")); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		if got.code != 1 || !strings.Contains(got.stderr, "no value came in time, within 2s") || !strings.Contains(got.stderr, "agent-secrets secret create NEW_TEST") {
			t.Fatalf("exit %d, stderr %q; want 1 naming the window and the command to run again", got.code, got.stderr)
		}
		if !regexp.MustCompile(`Enter the value by \d\d:\d\d:\d\d \S+, 2s after the create, or nothing is stored\.\nValue for NEW_TEST: `).MatchString(got.stderr) {
			t.Fatalf("stderr %q; want the prompt to name the time its value must come by, then its label", got.stderr)
		}
	case <-time.After(promptJobWait):
		t.Fatalf("the create had not ended %s after the paste closed", promptJobWait)
	}
	if calls := fake.called(); slices.Contains(calls, "AddSecretVersion") {
		t.Fatalf("Secret Manager calls %v; want no AddSecretVersion for a value that ended past its window", calls)
	}
	requireNothingLeft(t, fake, broker, "NEW_TEST")
}

// packages/envoy/cmd/cipher/secret_tty_linux_test.go
//go:build linux && (amd64 || arm64)

// The value prompt's reader (readHidden) at a real pseudo-terminal: what a person types or pastes
// at it, Ctrl-D, and what it leaves for the shell once it returns. Linux-only: the pseudo-terminal
// pair is opened with Linux's /dev/ptmx ioctls.
package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// openPTY opens a pseudo-terminal: the controller, the side a terminal emulator writes what a
// person types or pastes to, and the terminal a program reads, as a blocking descriptor.
func openPTY(t *testing.T) (controller *os.File, terminal int) {
	t.Helper()
	ptmx, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatalf("open /dev/ptmx: %v", err)
	}
	t.Cleanup(func() { ptmx.Close() })
	raw, err := ptmx.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var number uint32
	var ioctlErr error
	if err := raw.Control(func(fd uintptr) {
		if ioctlErr = unix.IoctlSetPointerInt(int(fd), unix.TIOCSPTLCK, 0); ioctlErr == nil {
			number, ioctlErr = unix.IoctlGetUint32(int(fd), unix.TIOCGPTN)
		}
	}); err != nil || ioctlErr != nil {
		t.Fatalf("unlock the pseudo-terminal: %v %v", err, ioctlErr)
	}
	fd, err := unix.Open(fmt.Sprintf("/dev/pts/%d", number), unix.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatalf("open /dev/pts/%d: %v", number, err)
	}
	t.Cleanup(func() { unix.Close(fd) })
	return ptmx, fd
}

// lflag is the terminal's local mode flags.
func lflag(t *testing.T, fd int) uint32 {
	t.Helper()
	tio, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}
	return tio.Lflag
}

// testBinary is the running test binary's absolute path, for a helper started in another
// directory, where os.Args[0] as `go test -c` and a relative path run it would name nothing.
func testBinary(t *testing.T) string {
	t.Helper()
	path, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return path
}

// promptRead is what readHidden returned.
type promptRead struct {
	line []byte
	err  error
}

// typeAtPrompt starts readHidden at a fresh terminal and, once it has turned echo off, writes typed
// to the controller as a terminal emulator does for a person's keys or a paste. It answers the
// pseudo-terminal and the reader's answer, failing t if the reader has not answered when the
// waits of these tests give up (waitsGaveUp).
func typeAtPrompt(t *testing.T, typed string) (*os.File, int, promptRead) {
	t.Helper()
	return typeAtPromptInWrites(t, 0, typed)
}

// typeAtPromptInWrites is typeAtPrompt with what the person types or pastes reaching the terminal
// in several writes, gap apart, as a paste over a slow link does.
func typeAtPromptInWrites(t *testing.T, gap time.Duration, writes ...string) (*os.File, int, promptRead) {
	t.Helper()
	controller, terminal := openPTY(t)
	answer := make(chan promptRead, 1)
	go func() {
		line, err := readHidden(terminal, time.Time{}, nil, nil)
		answer <- promptRead{line, err}
	}()
	awaitEchoOff(t, terminal)
	for i, w := range writes {
		if i > 0 {
			time.Sleep(gap)
		}
		if _, err := controller.Write([]byte(w)); err != nil {
			t.Fatal(err)
		}
	}
	got, ok := received(t, answer)
	if !ok {
		t.Fatalf("the reader had not returned after %q was typed, %s before the test binary's deadline", writes, waitMargin)
	}
	return controller, terminal, got
}

// awaitEchoOff waits until the prompt has turned the terminal's echo off. Its one bound is the
// moment the waits of these tests give up (waitsGaveUp): a prompt in a process of its own may take
// a while to start on a loaded machine.
func awaitEchoOff(t *testing.T, terminal int) {
	t.Helper()
	for ; lflag(t, terminal)&unix.ECHO != 0; time.Sleep(5 * time.Millisecond) {
		if waitsGaveUp(t) {
			t.Fatal("the reader never turned echo off")
		}
	}
}

// TestPromptEndsAtTheValueDeadline: a prompt handed a deadline (a create's value read) that gets
// nothing gives up at the deadline, with errValueTimedOut and no line, and gives the terminal back
// with echo on, as any other end of the prompt does. The prompt is timed against its deadline,
// less the time this process was held up meanwhile (endsByItsDeadline).
func TestPromptEndsAtTheValueDeadline(t *testing.T) {
	_, terminal := openPTY(t) // openPTY's cleanup holds the controller open for the whole test
	meter := startStallMeter(t)
	start := time.Now()
	deadline := start.Add(500 * time.Millisecond)
	answer := make(chan promptRead, 1)
	go func() {
		line, err := readHidden(terminal, deadline, nil, nil)
		answer <- promptRead{line, err}
	}()
	awaitEchoOff(t, terminal)
	got := endsByItsDeadline(t, meter, "the prompt", start, deadline.Sub(start), answer)
	if got.line != nil || !errors.Is(got.err, errValueTimedOut) {
		t.Fatalf("readHidden = %q, %v; want no line and errValueTimedOut", got.line, got.err)
	}
	if lflag(t, terminal)&unix.ECHO == 0 {
		t.Fatal("the prompt left echo off at its deadline")
	}
}

// TestPromptAtTheValueDeadlineMidEntryReadsTheEntryToItsEnd: a deadline that passes once part of the
// entry has been typed refuses the entry, but the prompt keeps the terminal: it says so at once
// (onDiscard, with errValueTimedOut), reads the rest of the entry to the line's end, and only then
// answers errValueTimedOut, so what is typed after the deadline is neither echoed nor left for the
// shell. The rest is typed once the prompt has either said so or answered, whichever comes first,
// so a prompt that gives the entry up at the deadline shows the rest reaching the shell. That first
// end is timed against the deadline, less the time this process was held up (endsByItsDeadline).
// The pseudo-terminal is not this process's controlling terminal, so no key of it sends a signal:
// the reason the notice reads names no interrupt key, and no open paste.
func TestPromptAtTheValueDeadlineMidEntryReadsTheEntryToItsEnd(t *testing.T) {
	controller, terminal := openPTY(t)
	meter := startStallMeter(t)
	start := time.Now()
	deadline := start.Add(2 * time.Second)
	// first takes whichever comes first, the notice or the answer; answer keeps the answer.
	type firstEnd struct {
		reason error
		read   *promptRead
	}
	first := make(chan firstEnd, 2)
	answer := make(chan promptRead, 1)
	go func() {
		line, err := readHidden(terminal, deadline, nil, func(reason error) { first <- firstEnd{reason: reason} })
		read := promptRead{line, err}
		answer <- read
		first <- firstEnd{read: &read}
	}()
	awaitEchoOff(t, terminal)
	if _, err := controller.Write([]byte("fakehead-")); err != nil {
		t.Fatal(err)
	}
	var early *promptRead
	if end := endsByItsDeadline(t, meter, "the prompt's refusal of the entry", start, deadline.Sub(start), first); end.read != nil {
		early = end.read
	} else {
		var notice valueDeadlineNotice
		if !errors.Is(end.reason, errValueTimedOut) || !errors.As(end.reason, &notice) || notice.interrupt != 0 || notice.inPaste {
			t.Fatalf("the prompt refused the entry for %#v; want a valueDeadlineNotice naming no interrupt key and no open paste, on a terminal that is not the controlling one", end.reason)
		}
	}
	if _, err := controller.Write([]byte("FAKETAIL\r")); err != nil {
		t.Fatal(err)
	}
	got := early
	if got == nil {
		read, ok := received(t, answer)
		if !ok {
			t.Fatalf("the prompt had not answered after the entry's line ended, %s before the test binary's deadline", waitMargin)
		}
		got = &read
	}
	if got.line != nil || !errors.Is(got.err, errValueTimedOut) {
		t.Fatalf("readHidden = %q, %v; want no line and errValueTimedOut", got.line, got.err)
	}
	if n := unread(t, terminal); n != 0 || bytes.Contains(shown(t, controller), []byte("FAKETAIL")) {
		t.Fatalf("the rest of the entry reached the shell: %d bytes left unread, or it was echoed", n)
	}
}

// TestGCPSecretCreateStoresNoValueThatEndsPastItsWindow: a value typed at the prompt inside the
// create's window, whose drain then runs past it - a bracketed paste that opens after the line and
// closes only once the window is over - is not stored: the prompt reads that paste through its
// closing mark, and the create then refuses the value as one that did not come in time, never
// calling AddSecretVersion, deletes the empty secret it made, asks the broker to reread the name,
// and exits 1 naming the window and the command to run again.
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
	readHidden = readHiddenAtTerminal
	real := readTerminal
	t.Cleanup(func() { readTerminal = real })
	// draining closes at the reader's first wait after the read that ended the line: the drain of
	// what follows the line. The terminal turns the typed carriage return into a line feed.
	draining := make(chan struct{})
	lineEnded, drainSeen := false, false
	readTerminal = func(fd, wake int, buf []byte, timeout int) (int, error) {
		if lineEnded && !drainSeen {
			drainSeen = true
			close(draining)
		}
		n, err := real(fd, wake, buf, timeout)
		if n > 0 && bytes.ContainsAny(buf[:n], "\r\n") {
			lineEnded = true
		}
		return n, err
	}
	type result struct {
		stderr string
		code   int
	}
	done := make(chan result, 1)
	go func() {
		_, stderr, code := runSecret("create", "NEW_TEST", "--owner", "me", "--tier", "agent")
		done <- result{stderr, code}
	}()
	awaitEchoOff(t, terminal)
	// The create asked the store for the secret before the prompt turned echo off, so its window
	// is over by then.
	windowEnds := time.Now().Add(window)
	if _, err := controller.Write([]byte("typed-value\r")); err != nil {
		t.Fatal(err)
	}
	if _, ok := received(t, draining); !ok {
		t.Fatalf("the prompt never drained what follows the line, %s before the test binary's deadline", waitMargin)
	}
	if _, err := controller.Write([]byte("\x1b[200~")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Until(windowEnds) + 300*time.Millisecond)
	if _, err := controller.Write([]byte("\x1b[201~")); err != nil {
		t.Fatal(err)
	}
	got, ok := received(t, done)
	if !ok {
		t.Fatalf("the create had not ended after the paste closed, %s before the test binary's deadline", waitMargin)
	}
	if got.code != 1 || !strings.Contains(got.stderr, "no value came in time, within 2s") || !strings.Contains(got.stderr, "agent-secrets secret create NEW_TEST") {
		t.Fatalf("exit %d, stderr %q; want 1 naming the window and the command to run again", got.code, got.stderr)
	}
	if !regexp.MustCompile(`Enter the value by \d\d:\d\d:\d\d \S+, 2s after the create, or nothing is stored\.\nValue for NEW_TEST: `).MatchString(got.stderr) {
		t.Fatalf("stderr %q; want the prompt to name the time its value must come by, then its label", got.stderr)
	}
	if calls := fake.called(); slices.Contains(calls, "AddSecretVersion") {
		t.Fatalf("Secret Manager calls %v; want no AddSecretVersion for a value that ended past its window", calls)
	}
	requireNothingLeft(t, fake, broker, "NEW_TEST")
}

// unread is how many bytes the terminal holds unread, which the shell would read once the form
// exits: with canonical mode turned off, so a last line without a newline counts too.
func unread(t *testing.T, fd int) int {
	t.Helper()
	tio, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}
	tio.Lflag &^= unix.ICANON
	tio.Cc[unix.VMIN], tio.Cc[unix.VTIME] = 0, 0
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, tio); err != nil {
		t.Fatal(err)
	}
	n, err := unix.IoctlGetInt(fd, unix.TIOCINQ)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// shown is what the terminal wrote back to the controller, which the person would see: an echo.
func shown(t *testing.T, controller *os.File) []byte {
	t.Helper()
	if err := controller.SetReadDeadline(time.Now().Add(300 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	buf := make([]byte, 4096)
	for {
		n, err := controller.Read(buf)
		out.Write(buf[:n])
		if err != nil {
			return out.Bytes()
		}
	}
}

// TestPromptReadsOneTypedLineWithEchoOff: Enter ends the value, which is never echoed; the reader
// asks the terminal to bracket pastes while it reads and to stop once it is done, and leaves it as
// it was, echo on.
func TestPromptReadsOneTypedLineWithEchoOff(t *testing.T) {
	controller, terminal, got := typeAtPrompt(t, "typed value\r")
	if got.err != nil || string(got.line) != "typed value" {
		t.Fatalf("readHidden = %q, %v; want %q", got.line, got.err, "typed value")
	}
	echoed := shown(t, controller)
	if bytes.Contains(echoed, []byte("typed")) {
		t.Fatalf("the terminal showed %q: the value was echoed", echoed)
	}
	on, off := bytes.Index(echoed, []byte("\x1b[?2004h")), bytes.LastIndex(echoed, []byte("\x1b[?2004l"))
	if on < 0 || off < on {
		t.Fatalf("the terminal was sent %q, want bracketed paste turned on and then off", echoed)
	}
	if flags := lflag(t, terminal); flags&unix.ECHO == 0 || flags&unix.ICANON == 0 {
		t.Fatalf("lflag after the read = %#x, want echo and canonical mode back on", flags)
	}
}

// TestPromptClearsIEXTENWhileItReads: the prompt's mode clears IEXTEN, under which macOS's terminal
// acts on Ctrl-V and Ctrl-O whatever ICANON and ISIG say (bsd/kern/tty.c, ttyinput), so a pasted
// Ctrl-V would vanish from the value and Ctrl-O would toggle output discard. The restore puts it
// back. On Linux n_tty lowercases input under IUCLC only while IEXTEN is set, even outside
// canonical mode, so this checks the mode itself.
func TestPromptClearsIEXTENWhileItReads(t *testing.T) {
	controller, terminal := openPTY(t)
	if lflag(t, terminal)&unix.IEXTEN == 0 {
		t.Fatal("a fresh pseudo-terminal has IEXTEN off; the test needs it on")
	}
	answer := make(chan promptRead, 1)
	go func() {
		line, err := readHidden(terminal, time.Time{}, nil, nil)
		answer <- promptRead{line, err}
	}()
	awaitEchoOff(t, terminal)
	if flags := lflag(t, terminal); flags&unix.IEXTEN != 0 {
		t.Fatalf("lflag while the prompt reads = %#x, want IEXTEN off", flags)
	}
	if _, err := controller.Write([]byte("value\r")); err != nil {
		t.Fatal(err)
	}
	got, ok := received(t, answer)
	if !ok {
		t.Fatalf("the reader had not returned, %s before the test binary's deadline", waitMargin)
	}
	if got.err != nil || string(got.line) != "value" {
		t.Fatalf("readHidden = %q, %v; want %q", got.line, got.err, "value")
	}
	if lflag(t, terminal)&unix.IEXTEN == 0 {
		t.Fatal("IEXTEN is off after the prompt returned")
	}
}

// TestPromptCtrlDEndsTheValue: Ctrl-D ends the read, with nothing typed (an empty value) and after
// typing (the value typed).
func TestPromptCtrlDEndsTheValue(t *testing.T) {
	for _, tc := range []struct{ typed, want string }{{"\x04", ""}, {"typed\x04", "typed"}} {
		_, terminal, got := typeAtPrompt(t, tc.typed)
		if got.err != nil || string(got.line) != tc.want {
			t.Fatalf("after %q: readHidden = %q, %v; want %q", tc.typed, got.line, got.err, tc.want)
		}
		if flags := lflag(t, terminal); flags&unix.ECHO == 0 {
			t.Fatalf("after %q: lflag = %#x, want echo back on", tc.typed, flags)
		}
	}
}

// TestPromptRefusesAPasteOfMoreThanOneLineAndLeavesNothingForTheShell: a paste of a value of more
// than one line, as a terminal sends it (each line ended by a carriage return, the last one with
// or without), is refused rather than cut at its first line, and none of it is left for the shell
// to read once the form exits, nor echoed.
func TestPromptRefusesAPasteOfMoreThanOneLineAndLeavesNothingForTheShell(t *testing.T) {
	for _, paste := range []string{
		"-----BEGIN KEY-----\rline two\recho line three",
		"-----BEGIN KEY-----\nline two\nline three\n",
	} {
		controller, terminal, got := typeAtPrompt(t, paste)
		if !errors.Is(got.err, errMoreThanOneLine) {
			t.Fatalf("paste %q: readHidden = %q, %v; want errMoreThanOneLine", paste, got.line, got.err)
		}
		if n := unread(t, terminal); n != 0 {
			t.Fatalf("paste %q: the terminal holds %d bytes unread for the shell, want none", paste, n)
		}
		if echoed := shown(t, controller); bytes.Contains(echoed, []byte("line")) {
			t.Fatalf("paste %q: the terminal showed %q", paste, echoed)
		}
	}
}

// TestPromptReadsABracketedPasteThroughItsEnd: a bracketed paste of more than one line is read
// through its closing mark however long a gap splits it, as one lost packet over ssh can, so it is
// refused whole and nothing of it is left for the shell or echoed.
func TestPromptReadsABracketedPasteThroughItsEnd(t *testing.T) {
	controller, terminal, got := typeAtPromptInWrites(t, 300*time.Millisecond, "\x1b[200~line1\n", "line2\x1b[201~")
	if !errors.Is(got.err, errMoreThanOneLine) {
		t.Fatalf("readHidden = %q, %v; want errMoreThanOneLine", got.line, got.err)
	}
	if n := unread(t, terminal); n != 0 {
		t.Fatalf("the terminal holds %d bytes unread for the shell, want none", n)
	}
	if echoed := shown(t, controller); bytes.Contains(echoed, []byte("line")) {
		t.Fatalf("the terminal showed %q", echoed)
	}
}

// TestPromptAcceptsABracketedPasteOfOneLine: a bracketed paste of one line is the value, never its
// marks: ending in a line ending, which is dropped, or followed by Enter, with its marks whole or
// split across reads.
func TestPromptAcceptsABracketedPasteOfOneLine(t *testing.T) {
	for _, writes := range [][]string{
		{"\x1b[200~value\n\x1b[201~"},
		{"\x1b[200~value\r\n\x1b[201~"},
		{"\x1b[200~value\x1b[201~", "\r"},
		{"\x1b[20", "0~value\n\x1b[2", "01~"},
	} {
		_, terminal, got := typeAtPromptInWrites(t, 50*time.Millisecond, writes...)
		if got.err != nil || string(got.line) != "value" {
			t.Fatalf("%q: readHidden = %q, %v; want %q", writes, got.line, got.err, "value")
		}
		if n := unread(t, terminal); n != 0 {
			t.Fatalf("%q: the terminal holds %d bytes unread, want none", writes, n)
		}
	}
}

// TestPromptWordEraseAndControlBytes: the terminal's configured word erase removes the word it
// would in canonical mode, and another control byte cannot become an invisible byte in a secret.
func TestPromptWordEraseAndControlBytes(t *testing.T) {
	_, terminal, got := typeAtPrompt(t, "abc def\x17ghi\r")
	if got.err != nil || string(got.line) != "abc ghi" {
		t.Fatalf("word erase: readHidden = %q, %v; want %q", got.line, got.err, "abc ghi")
	}
	if n := unread(t, terminal); n != 0 {
		t.Fatalf("word erase: the terminal holds %d bytes unread, want none", n)
	}
	_, _, got = typeAtPrompt(t, "value\x01\r")
	if got.err == nil || !strings.Contains(got.err.Error(), "the control byte 0x01 cannot be typed at the prompt") {
		t.Fatalf("control byte: readHidden = %q, %v; want refusal naming 0x01", got.line, got.err)
	}
}

// TestPromptNamesATabAPasteEndsWith: a key pasted at a real terminal with a Tab after it, as a copy
// out of a web page's table often carries, is refused, neither stored nor trimmed, with a refusal
// that names the Tab, says it is at the end of the value, and says that a paste often adds one
// there; the terminal is left with nothing unread.
func TestPromptNamesATabAPasteEndsWith(t *testing.T) {
	_, terminal, got := typeAtPromptInWrites(t, 50*time.Millisecond, "\x1b[200~sk-live-key\t\x1b[201~", "\r")
	var refused promptControlByteError
	if got.line != nil || !errors.As(got.err, &refused) || refused.c != '\t' || !refused.textBefore || refused.textAfter {
		t.Fatalf("readHidden = %q, %v; want no line and a refusal of the Tab at the value's end", got.line, got.err)
	}
	if text := got.err.Error(); !strings.Contains(text, "0x09") || !strings.Contains(text, "Tab") || !strings.Contains(text, "end of the value") || !strings.Contains(text, "paste") {
		t.Fatalf("refusal %q; want it to name 0x09, the Tab, the value's end and the paste", text)
	}
	if n := unread(t, terminal); n != 0 {
		t.Fatalf("the terminal holds %d bytes unread, want none", n)
	}
}

// TestPromptRefusesFlowControlBytes: Ctrl-S and Ctrl-Q reach the reader as control bytes rather
// than stopping and starting the terminal's output, so a paste holding one is refused, never stored
// without it, and never leaves the prompt waiting with its output suspended.
func TestPromptRefusesFlowControlBytes(t *testing.T) {
	for _, c := range []byte{0x13, 0x11} {
		_, _, got := typeAtPrompt(t, "val"+string(c)+"ue\r")
		want := fmt.Sprintf("the control byte 0x%02x cannot be typed at the prompt", c)
		if got.line != nil || got.err == nil || !strings.Contains(got.err.Error(), want) {
			t.Fatalf("control 0x%02x: readHidden = %q, %v; want the refusal naming it", c, got.line, got.err)
		}
	}
}

// TestPromptRefusesAPasteCutShortByAHangUp: when the terminal hangs up inside a bracketed paste, as
// when its window or ssh session closes mid-paste, the reader answers an error rather than what it
// has read, so nothing is stored. The hang-up lands while the reader is between reads, where its
// next read of the hung-up terminal answers end of input.
func TestPromptRefusesAPasteCutShortByAHangUp(t *testing.T) {
	for _, tc := range []struct {
		paste string
		want  error
	}{
		{"\x1b[200~-----BEGIN KEY-----\nline2", errMoreThanOneLine},
		{"\x1b[200~value\n", errPasteCutShort},
		{"\x1b[200~val", errPasteCutShort},
		{"partial-secr", errValueCutShort},
	} {
		t.Run(fmt.Sprintf("%q", tc.paste), func(t *testing.T) {
			controller, terminal := openPTY(t)
			real := readTerminal
			t.Cleanup(func() { readTerminal = real })
			consumed, hungUp := make(chan struct{}), make(chan struct{})
			var once sync.Once
			read := 0
			readTerminal = func(fd, wake int, buf []byte, timeout int) (int, error) {
				n, err := real(fd, wake, buf, timeout)
				if n > 0 {
					read += n
				}
				if read >= len(tc.paste) {
					once.Do(func() { close(consumed) })
					<-hungUp
				}
				return n, err
			}
			answer := make(chan promptRead, 1)
			go func() {
				line, err := readHidden(terminal, time.Time{}, nil, nil)
				answer <- promptRead{line, err}
			}()
			awaitEchoOff(t, terminal)
			if _, err := controller.Write([]byte(tc.paste)); err != nil {
				t.Fatal(err)
			}
			if _, ok := received(t, consumed); !ok {
				t.Fatalf("the reader had not read the paste, %s before the test binary's deadline", waitMargin)
			}
			if err := controller.Close(); err != nil {
				t.Fatal(err)
			}
			close(hungUp)
			got, ok := received(t, answer)
			if !ok {
				t.Fatalf("the reader had not returned after the hang-up, %s before the test binary's deadline", waitMargin)
			}
			if !errors.Is(got.err, tc.want) || got.line != nil {
				t.Fatalf("readHidden = %q, %v; want no value and %v", got.line, got.err, tc.want)
			}
		})
	}
}

// TestPromptSignalHelper is the process the signal tests start: the value prompt's reader on its
// standard input, its controlling terminal. It names only the reader's error, never its value.
func TestPromptSignalHelper(t *testing.T) {
	if os.Getenv("AGENT_SECRETS_PROMPT_HELPER") == "" {
		t.Skip("subprocess helper")
	}
	_, err := readHidden(0, time.Time{}, nil, nil)
	fmt.Printf("RETURNED %v\n", err)
}

// TestPromptCtrlCEndsTheProcessBySIGINT: Ctrl-C at the prompt ends the process by SIGINT, echo back
// on, so a calling shell stops its list rather than running the next command. The prompt runs in a
// process of its own, the session leader whose controlling terminal sends the Ctrl-C, started by
// `env --default-signal`, so it does not inherit a SIGINT this test process ignores (as a job run in
// the background of a script does), at which the prompt rightly goes on reading.
func TestPromptCtrlCEndsTheProcessBySIGINT(t *testing.T) {
	reset := []string{"env", "--default-signal=INT,TERM"}
	helper := shellWord(os.Args[0]) + " -test.run=^TestPromptSignalHelper$"
	for _, tc := range []struct {
		name    string
		command *exec.Cmd
	}{
		{"the prompt alone", exec.Command(reset[0], reset[1], os.Args[0], "-test.run=^TestPromptSignalHelper$")},
		{"a bash list", exec.Command(reset[0], reset[1], "bash", "-c", helper+"; echo SECOND")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			controller, terminal := openPTY(t)
			fd, err := unix.Dup(terminal)
			if err != nil {
				t.Fatal(err)
			}
			tty := os.NewFile(uintptr(fd), "terminal")
			cmd := tc.command
			cmd.Env = append(os.Environ(), "AGENT_SECRETS_PROMPT_HELPER=1")
			cmd.Stdin, cmd.Stdout, cmd.Stderr = tty, tty, tty
			cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
			err = cmd.Start()
			tty.Close()
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			awaitEchoOff(t, terminal)
			if _, err := controller.Write([]byte{0x03}); err != nil {
				t.Fatal(err)
			}
			if _, ok := received(t, done); !ok {
				_ = cmd.Process.Kill()
				t.Fatalf("the process had not ended after Ctrl-C, %s before the test binary's deadline; the terminal showed %q", waitMargin, shown(t, controller))
			}
			status := cmd.ProcessState.Sys().(syscall.WaitStatus)
			out := shown(t, controller)
			if tc.name == "the prompt alone" && (!status.Signaled() || status.Signal() != syscall.SIGINT) {
				t.Fatalf("wait status %v (%s); want ended by SIGINT. The terminal showed %q", status, cmd.ProcessState, out)
			}
			if bytes.Contains(out, []byte("SECOND")) || bytes.Contains(out, []byte("RETURNED")) {
				t.Fatalf("the terminal showed %q (%s): the list went on past the Ctrl-C", out, cmd.ProcessState)
			}
			if lflag(t, terminal)&unix.ECHO == 0 {
				t.Fatal("echo is off after the process ended")
			}
		})
	}
}

// TestPromptQuitEndsTheProcessBySIGQUITWithNoCore: Ctrl-\ at the prompt restores echo and ends the
// process by SIGQUIT, which a shell reads as 131, with no goroutine dump from Go's runtime and no
// core, which would hold what was typed. The prompt runs with its core limit raised to the hard
// limit, in a directory of its own, so a core the kernel would write shows in its wait status and
// in that directory.
func TestPromptQuitEndsTheProcessBySIGQUITWithNoCore(t *testing.T) {
	controller, terminal := openPTY(t)
	fd, err := unix.Dup(terminal)
	if err != nil {
		t.Fatal(err)
	}
	tty := os.NewFile(uintptr(fd), "terminal")
	dir := t.TempDir()
	cmd := exec.Command("bash", "-c",
		`ulimit -c "$(ulimit -Hc)" && exec env --default-signal=QUIT "$0" -test.run='^TestPromptSignalHelper$'`,
		testBinary(t))
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "AGENT_SECRETS_PROMPT_HELPER=1")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = tty, tty, tty
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		tty.Close()
		t.Fatal(err)
	}
	tty.Close()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	awaitEchoOff(t, terminal)
	if _, err := controller.Write([]byte{0x1c}); err != nil {
		t.Fatal(err)
	}
	if _, ok := received(t, done); !ok {
		_ = cmd.Process.Kill()
		t.Fatalf("the process had not quit after Ctrl-\\, %s before the test binary's deadline", waitMargin)
	}
	status := cmd.ProcessState.Sys().(syscall.WaitStatus)
	out := shown(t, controller)
	if !status.Signaled() || status.Signal() != syscall.SIGQUIT {
		t.Fatalf("wait status %v (%s); want ended by SIGQUIT. The terminal showed %q", status, cmd.ProcessState, out)
	}
	if status.CoreDump() {
		t.Fatal("the kernel dumped core")
	}
	left, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range left {
		t.Errorf("the prompt left %s in its directory", entry.Name())
	}
	if bytes.Contains(out, []byte("goroutine")) || bytes.Contains(out, []byte("RETURNED")) {
		t.Fatalf("the terminal showed %q: want no goroutine dump and no return from the read", out)
	}
	if lflag(t, terminal)&unix.ECHO == 0 {
		t.Fatalf("echo is off after Ctrl-\\; the terminal showed %q", out)
	}
}

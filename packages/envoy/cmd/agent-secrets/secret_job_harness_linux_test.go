//go:build linux && (amd64 || arm64)

// packages/envoy/cmd/agent-secrets/secret_job_harness_linux_test.go
//
// The harness the job-control tests of secret_job_linux_test.go run the value prompt in: an
// interactive bash on a pseudo-terminal of its own (promptShell), and TestPromptJobHelper, the
// process that bash starts, which reads a value at the prompt and prints what it read and how it
// left the terminal. The tests themselves are in secret_job_linux_test.go.
package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/sjawhar/envoy/internal/testwait"
)

// promptShell gives the prompt a parent interactive shell in the same session, so
// its process group is not orphaned and the kernel really stops it on Ctrl-Z.
type promptShell struct {
	t          *testing.T
	controller *os.File
	ctlFd      int // controller's fd, read once: (*os.File).Fd() would switch it to
	// blocking mode, silently breaking every later SetReadDeadline on controller.
	terminal int
	bash     *exec.Cmd
	out      bytes.Buffer
	pid      int
	history  string
	typed    []string // the commands start and foregroundState typed, as the history holds them
	env      string   // variables start sets for the helper, each followed by a space
	// wrapperIgnores is the signals start's wrapper shell ignores (`trap '' <signals>`), which the
	// helper it starts inherits as ignored; "TSTP" when empty.
	wrapperIgnores string
}

// rawFd answers f's file descriptor through its SyscallConn, which, unlike
// (*os.File).Fd(), never switches f to blocking mode.
func rawFd(f *os.File) (int, error) {
	raw, err := f.SyscallConn()
	if err != nil {
		return 0, err
	}
	var fd int
	if ctlErr := raw.Control(func(sysfd uintptr) { fd = int(sysfd) }); ctlErr != nil {
		return 0, ctlErr
	}
	return fd, nil
}

func newPromptShell(t *testing.T) *promptShell {
	t.Helper()
	controller, terminal := openPTY(t)
	ctlFd, err := rawFd(controller)
	if err != nil {
		t.Fatal(err)
	}
	fd, err := unix.Dup(terminal)
	if err != nil {
		t.Fatal(err)
	}
	tty := os.NewFile(uintptr(fd), "terminal")
	defer tty.Close()
	home := t.TempDir()
	cmd := exec.Command("bash", "--norc", "--noprofile", "-i")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "HISTFILE=" + filepath.Join(home, "history"), "PS1=PROMPT$ ", "TERM=dumb"}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = tty, tty, tty
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	s := &promptShell{t: t, controller: controller, ctlFd: ctlFd, terminal: terminal, bash: cmd, history: filepath.Join(home, "history")}
	t.Cleanup(func() {
		if s.pid != 0 {
			_ = syscall.Kill(s.pid, syscall.SIGKILL)
		}
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	s.wait("PROMPT$ ")
	return s
}

func (s *promptShell) send(text string) {
	s.t.Helper()
	if _, err := s.controller.Write([]byte(text)); err != nil {
		s.t.Fatal(err)
	}
}

// setupOutranDeadline is what the helper prints when its prompt, given a deadline inside the
// entry (AGENT_SECRETS_JOB_DEADLINE_MID_ENTRY), reached its first check of that deadline only
// after it had passed, before it read anything: the machine stalled across the moment between
// the helper setting the deadline and the prompt's first read.
const setupOutranDeadline = "SETUP_OUTRAN_DEADLINE"

// stallRetries runs scenario in a new shell, and again in another while it answers false: it
// does so when the helper printed setupOutranDeadline, a run that says nothing of the prompt. It
// runs it again for as long as the shared wait waits (testwait), so a machine that stalls past
// the value deadline in every run fails the test at the test binary's deadline, naming the run
// it waited for.
func stallRetries(t *testing.T, scenario func(s *promptShell) bool) {
	t.Helper()
	run := 0
	testwait.Eventually(t, "a run whose prompt reached its first read before the value deadline", func() bool {
		run++
		if scenario(newPromptShell(t)) {
			return true
		}
		t.Logf("run %d: the machine stalled past the value deadline before the prompt's first read; running it again", run)
		return false
	})
}

// wait waits until the terminal has shown want (waitFirst).
func (s *promptShell) wait(want string) {
	s.t.Helper()
	s.waitFirst(want)
}

// waitFirst waits until the terminal has shown one of wants, and answers the one it showed first.
func (s *promptShell) waitFirst(wants ...string) string {
	s.t.Helper()
	first := ""
	s.waitShown(fmt.Sprintf("the terminal to show one of %q", wants), func(shown string) bool {
		at := -1
		for _, want := range wants {
			if i := strings.Index(shown, want); i >= 0 && (at < 0 || i < at) {
				first, at = want, i
			}
		}
		return at >= 0
	})
	return first
}

// bound waits until the helper has said it is about to make the prompt's k-th wait with a bound
// (WAIT k <ms>, TestPromptJobHelper), and answers that bound in milliseconds.
func (s *promptShell) bound(k int) int {
	s.t.Helper()
	line := regexp.MustCompile(fmt.Sprintf(`WAIT %d (\d+)\r\n`, k))
	ms := 0
	s.waitShown(fmt.Sprintf("the prompt's wait %d with a bound (WAIT %d <ms>)", k, k), func(shown string) bool {
		said := line.FindStringSubmatch(shown)
		if said == nil {
			return false
		}
		var err error
		if ms, err = strconv.Atoi(said[1]); err != nil {
			s.t.Fatalf("the helper said %q for the prompt's wait %d: %v", said[0], k, err)
		}
		return true
	})
	return ms
}

// requireBound fails the test unless the prompt bounded its k-th wait with a bound (bound) at want
// milliseconds: the bound of what, which the behaviour under test rests on, read as data.
func (s *promptShell) requireBound(k, want int, what string) {
	s.t.Helper()
	if got := s.bound(k); got != want {
		s.t.Fatalf("the prompt bounded its wait %d, %s, at %d ms; want %d ms", k, what, got, want)
	}
}

// waitShown waits until holds is true of what the terminal has shown, reading on from it until then.
func (s *promptShell) waitShown(what string, holds func(shown string) bool) {
	s.t.Helper()
	buf := make([]byte, 4096)
	testwait.Until(s.t, what, func() (string, bool) {
		if holds(s.out.String()) {
			return s.out.String(), true
		}
		// The read deadline is how a pty read is made to come back at all, not a bound on the
		// wait: what it does not find this time the next read finds.
		_ = s.controller.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
		n, _ := s.controller.Read(buf)
		s.out.Write(buf[:n])
		return s.out.String(), false
	})
}

func (s *promptShell) start(wrapper bool, background bool) {
	s.t.Helper()
	command := s.env + "AGENT_SECRETS_JOB_HELPER=1 " + shellWord(os.Args[0]) + " -test.run='^TestPromptJobHelper$'"
	if background {
		// fg hands the job the terminal in the state bash hands every foreground
		// job, which is what the prompt must restore: not what the terminal holds
		// while the job waits behind bash's line editor.
		command = "AGENT_SECRETS_JOB_REFERENCE=" + s.foregroundState() + " " + command
	}
	if wrapper {
		ignores := s.wrapperIgnores
		if ignores == "" {
			ignores = "TSTP"
		}
		command = "bash -c " + shellWord("trap '' "+ignores+"; "+command+"; echo WRAPPER_DONE")
	}
	if background {
		command += " &"
	}
	s.out.Reset()
	s.typed = append(s.typed, command)
	s.send(command + "\r")
	s.wait("HELPER_PID=")
	s.wait("\r\nHELPER_READY\r\n")
	start := strings.Index(s.out.String(), "HELPER_PID=") + len("HELPER_PID=")
	pid, err := strconv.Atoi(strings.Fields(s.out.String()[start:])[0])
	if err != nil {
		s.t.Fatal(err)
	}
	s.pid = pid
	// The label shows once the prompt holds the terminal in its own mode: a stop
	// before it discards nothing, so each test's stop comes after it.
	marker := "Value for DEMO_KEY: "
	if background {
		marker = "PROMPT_STARTING"
	}
	s.wait(marker)
	// What the terminal showed after the marker is kept: on a loaded machine one read can carry
	// the marker and what the prompt did next, which may be the line the test waits for.
	shown := s.out.String()
	rest := shown[strings.Index(shown, marker)+len(marker):]
	s.out.Reset()
	s.out.WriteString(rest)
}

// foregroundState is the terminal state, as termiosKey writes it, that bash hands a
// foreground job: what a fresh foreground command reads at its start.
func (s *promptShell) foregroundState() string {
	s.t.Helper()
	s.out.Reset()
	probe := "AGENT_SECRETS_JOB_HELPER=1 AGENT_SECRETS_JOB_PROBE=1 " + shellWord(os.Args[0]) + " -test.run='^TestPromptJobHelper$'"
	s.typed = append(s.typed, probe)
	s.send(probe + "\r")
	s.wait("FOREGROUND_STATE=")
	s.wait(" END")
	s.wait("PROMPT$ ")
	start := strings.Index(s.out.String(), "FOREGROUND_STATE=") + len("FOREGROUND_STATE=")
	return strings.Fields(s.out.String()[start:])[0]
}

// termiosKey is every field of t, as one shell-safe word.
func termiosKey(t *unix.Termios) string {
	return fmt.Sprintf("%x.%x.%x.%x.%x.%x.%x.%x", t.Iflag, t.Oflag, t.Cflag, t.Lflag, t.Line, t.Cc[:], t.Ispeed, t.Ospeed)
}

func (s *promptShell) waitForeground(pid int) {
	s.t.Helper()
	testwait.Until(s.t, fmt.Sprintf("the terminal's foreground group to be %d", pid), func() (int, bool) {
		foreground, err := unix.IoctlGetInt(s.ctlFd, unix.TIOCGPGRP)
		if err != nil {
			s.t.Fatal(err)
		}
		return foreground, foreground == pid
	})
}

func (s *promptShell) noShellValue(markers ...string) {
	s.t.Helper()
	s.wait("PROMPT$ ")
	s.wait("RESTORED=true LABEL_HELD=true")
	s.send("history -w; printf 'HISTORY_%s\\n' SAVED\r")
	s.wait("HISTORY_SAVED")
	history, err := os.ReadFile(s.history)
	if err != nil {
		s.t.Fatal(err)
	}
	// The commands this test typed to start the helper are in the history too, and bash shows a
	// job's command again at fg and at a stop, with the test binary's path and the helper's
	// settings in them: a marker that only those hold (a path under a directory named "head") is
	// no leak, so they are taken out before the check.
	terminal := s.out.String()
	for _, command := range s.typed {
		for _, shown := range []string{command, strings.TrimSuffix(command, " &")} {
			history = bytes.ReplaceAll(history, []byte(shown), nil)
			terminal = strings.ReplaceAll(terminal, shown, "")
		}
	}
	for _, marker := range markers {
		if strings.Contains(terminal, marker) || bytes.Contains(history, []byte(marker)) {
			s.t.Fatalf("value reached the screen or shell history: terminal=%q history=%q", s.out.String(), history)
		}
	}
}

// labelCheck passes the prompt's writes to stderr and records whether its label
// reached the terminal only while the prompt held it: in the foreground, echo off.
type labelCheck struct {
	shown, held bool
}

func (c *labelCheck) Write(p []byte) (int, error) {
	if bytes.HasPrefix(p, []byte("Value for ")) {
		foreground, fgErr := unix.IoctlGetInt(0, unix.TIOCGPGRP)
		tio, tioErr := unix.IoctlGetTermios(0, unix.TCGETS)
		c.held = (!c.shown || c.held) && fgErr == nil && tioErr == nil &&
			foreground == unix.Getpgrp() && tio.Lflag&unix.ECHO == 0
		c.shown = true
	}
	return os.Stderr.Write(p)
}

func TestPromptJobHelper(t *testing.T) {
	if os.Getenv("AGENT_SECRETS_JOB_HELPER") == "" {
		t.Skip("subprocess helper")
	}
	if os.Getenv("AGENT_SECRETS_JOB_PROBE") != "" {
		tio, err := unix.IoctlGetTermios(0, unix.TCGETS)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Printf("FOREGROUND_STATE=%s END\n", termiosKey(tio))
		return
	}
	fmt.Printf("HELPER_PID=%d\nHELPER_READY\n", os.Getpid())
	if bound := os.Getenv("AGENT_SECRETS_JOB_PASTE_BOUND"); bound != "" {
		d, err := time.ParseDuration(bound)
		if err != nil {
			t.Fatal(err)
		}
		maxPasteDrain = d
	}
	if os.Getenv("AGENT_SECRETS_JOB_FAIL_DISCARD") != "" {
		discardInput = func(int) error { return errors.New("TIOCFLUSH failed") }
	}
	if hook := os.Getenv("AGENT_SECRETS_JOB_DEATH_HOOK"); hook != "" {
		// What create sets while the secret it made is empty (onSignalDeath, onSecondSignal),
		// reduced to lines the test can see on the terminal, with create's own watch on the
		// signals running beside the prompt's, as it does in a create. AGENT_SECRETS_JOB_DEATH_HOOK
		// =hang makes the cleanup wait an hour, as one waiting on a store call that never answers.
		onSignalDeath = func() {
			fmt.Println("DEATH_HOOK_RAN")
			if hook == "hang" {
				time.Sleep(time.Hour)
			}
		}
		onSecondSignal = func(sig os.Signal) { fmt.Println("SECOND_SIGNAL_LINE", sig) }
		defer watchCreateSignals()()
	}
	reference := os.Getenv("AGENT_SECRETS_JOB_REFERENCE")
	if reference == "" {
		// Started in the foreground: bash has already handed it the terminal.
		saved, err := unix.IoctlGetTermios(0, unix.TCGETS)
		if err != nil {
			t.Fatal(err)
		}
		reference = termiosKey(saved)
	} else {
		// Started with &: start the prompt only once bash's line editor holds the
		// terminal (canonical mode off), which a prompt in the background must
		// neither take as its baseline nor show its label over.
		testwait.Eventually(t, "bash's line editor to take the terminal", func() bool {
			tio, err := unix.IoctlGetTermios(0, unix.TCGETS)
			if err != nil {
				t.Fatal(err)
			}
			return tio.Lflag&unix.ICANON == 0
		})
		fmt.Println("PROMPT_STARTING")
		if os.Getenv("AGENT_SECRETS_JOB_AFTER_SHELL_EXIT") != "" {
			// Reach the prompt only once the shell's exit has taken the terminal from the
			// session, as a prompt descheduled that long on a loaded machine would.
			testwait.Eventually(t, "the shell's exit to take the terminal", func() bool {
				_, err := unix.IoctlGetInt(0, unix.TIOCGPGRP)
				return errors.Is(err, unix.ENOTTY)
			})
		}
	}
	// AGENT_SECRETS_JOB_DEADLINE gives the read a deadline that long after it starts, as a create's
	// window does, and names it so in the timeout's message. AGENT_SECRETS_JOB_DEADLINE_MID_ENTRY
	// places that deadline inside the entry: the reader's first wait lasts until input comes,
	// however late, so the entry is under way when the deadline passes; its first wait after that
	// input lasts until the deadline has passed, where it has not, and then ends with nothing read,
	// as an entry, typed or pasted, that falls quiet across the deadline does. The helper decides
	// that by the deadline, not by the wait's bound. It prints DEADLINE_PASSED then, and READING_ON
	// at the reader's next wait, if it waits again; where the input itself came after the
	// deadline, the reader is already reading on, and it prints both at once. With
	// AGENT_SECRETS_JOB_HOLD_READING_ON it holds the wait it prints READING_ON at open until input
	// comes, so what the test sends after READING_ON arrives inside that wait however slowly this
	// process runs, whichever of the two ways the deadline passed. With
	// AGENT_SECRETS_JOB_ANNOUNCE_DEADLINE it prints DEADLINE_REACHED once the deadline has passed,
	// so a test can send its input after it.
	var window time.Duration
	if d := os.Getenv("AGENT_SECRETS_JOB_DEADLINE"); d != "" {
		var err error
		if window, err = time.ParseDuration(d); err != nil {
			t.Fatal(err)
		}
		valueReadWindow = window
	}
	// Before each wait of the reader's that has a bound (a poll with a timeout), the helper prints
	// WAIT <k> <ms>: k counts those waits from 1, and ms is the bound the reader gave the wait, so a
	// test reads the bound its behaviour rests on as data (promptShell.bound). With
	// AGENT_SECRETS_JOB_HOLD_WAITS=<n> it holds each of the first n such waits open until input
	// comes, so what the test sends after a held wait's WAIT line arrives inside that wait however
	// slowly this process runs. Later waits run with their own bound, so the reader still ends a
	// quiet window, or gives a paste up, by its own clock where a test needs that. One wait can take
	// several calls: the reader calls again with the same bound after a watcher event (-2), input
	// gone before its read (-1), or a signal that interrupted the poll (EINTR, which the Go runtime's
	// own signals cause too), so only another answer ends the wait, and its hold with it.
	holds := 0
	if n := os.Getenv("AGENT_SECRETS_JOB_HOLD_WAITS"); n != "" {
		var err error
		if holds, err = strconv.Atoi(n); err != nil {
			t.Fatal(err)
		}
	}
	var deadline time.Time
	midEntry, held, readOn := os.Getenv("AGENT_SECRETS_JOB_DEADLINE_MID_ENTRY") != "", false, false
	real := readTerminal
	waits, waiting, readSome := 0, false, false
	// readingOnWait is the wait (k of WAIT k) READING_ON was printed at, which
	// AGENT_SECRETS_JOB_HOLD_READING_ON holds; 0 while there is none.
	holdReadingOn, readingOnWait := os.Getenv("AGENT_SECRETS_JOB_HOLD_READING_ON") != "", 0
	// readingOn prints READING_ON at the wait whose bound is timeout, and answers the bound that
	// wait runs with: held open until input comes where AGENT_SECRETS_JOB_HOLD_READING_ON asks
	// for it and the wait has a bound at all.
	readingOn := func(timeout int) int {
		fmt.Println("READING_ON")
		if !holdReadingOn || timeout < 0 {
			return timeout
		}
		readingOnWait = waits
		return -1
	}
	announceIgnoredStop := os.Getenv("AGENT_SECRETS_JOB_ANNOUNCE_IGNORED_STOP") != ""
	stopRead := false
	reads := 0
	readTerminal = func(fd, wake int, buf []byte, timeout int) (int, error) {
		reads++
		if stopRead {
			// The preceding read included Ctrl-Z. Reaching the next read means feed has
			// handled it, unlike READ_SOME, which is printed before feed sees the bytes.
			fmt.Println("IGNORED_STOP_HANDLED")
			stopRead = false
		}
		if midEntry && !readSome {
			timeout = -1
		}
		if timeout >= 0 {
			if !waiting {
				waits, waiting = waits+1, true
				fmt.Printf("WAIT %d %d\n", waits, timeout)
			}
			if waits <= holds || waits == readingOnWait {
				timeout = -1
			}
		}
		if held && !readOn {
			readOn = true
			timeout = readingOn(timeout)
		}
		if midEntry && readSome && !held {
			// The reader's first wait after input: decided by the deadline itself, not by the
			// wait's bound, which inside a paste that began after the deadline is maxPasteDrain.
			held = true
			if time.Until(deadline) > 0 {
				time.Sleep(time.Until(deadline))
				timeout = 0
				fmt.Println("DEADLINE_PASSED")
			} else {
				readOn = true
				fmt.Println("DEADLINE_PASSED")
				timeout = readingOn(timeout)
			}
		}
		n, err := real(fd, wake, buf, timeout)
		waiting = waiting && (n == -2 || (n == -1 && err == nil) || errors.Is(err, unix.EINTR))
		if announceIgnoredStop && n > 0 && bytes.IndexByte(buf[:n], '\x1a') >= 0 {
			stopRead = true
		}
		if n > 0 && !readSome {
			readSome = true
			fmt.Println("READ_SOME")
		}
		return n, err
	}
	label := &labelCheck{}
	if window != 0 {
		deadline = time.Now().Add(window)
		if os.Getenv("AGENT_SECRETS_JOB_ANNOUNCE_DEADLINE") != "" {
			time.AfterFunc(time.Until(deadline), func() { fmt.Println("DEADLINE_REACHED") })
		}
	}
	value, err := readSecretValue("DEMO_KEY", "agent-secrets secret set DEMO_KEY", deadline, label)
	if midEntry && reads == 0 && errors.Is(err, errValueTimedOut) {
		// The prompt reached its first check of the deadline only after the deadline had passed,
		// before it read at all: the machine stalled between the helper setting the deadline and
		// the prompt's first read, and this run says nothing of the entry (stallRetries).
		fmt.Println(setupOutranDeadline)
	}
	restored, restoreErr := unix.IoctlGetTermios(0, unix.TCGETS)
	fmt.Printf("RETURNED %v MATCH=%t EMPTY=%t RESTORED=%t LABEL_HELD=%t\n", err, value == "headtail", value == "",
		restoreErr == nil && termiosKey(restored) == reference, label.shown && label.held)
}

// jobStopped waits until bash's job table shows job 1 stopped: fg sends SIGCONT only to a
// job bash has already seen stop.
func (s *promptShell) jobStopped() {
	s.t.Helper()
	testwait.Until(s.t, "bash's job table to show job 1 stopped", func() (string, bool) {
		s.out.Reset()
		s.send("jobs %1\r")
		s.wait("PROMPT$ ")
		shown := s.out.String()
		return shown, strings.Contains(shown, "Stopped")
	})
}

// allInputRead waits until the terminal holds nothing unread: the prompt has read all that was
// sent, so none of it is left for the shell.
func (s *promptShell) allInputRead() {
	s.t.Helper()
	testwait.Until(s.t, "the prompt to read what was typed at it", func() (int, bool) {
		queued, err := unix.IoctlGetInt(s.terminal, unix.TIOCINQ)
		if err != nil {
			s.t.Fatal(err)
		}
		return queued, queued == 0
	})
}

// processState is the state letter /proc gives process pid ("T" while stopped).
func processState(t *testing.T, pid int) string {
	t.Helper()
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(stat[bytes.LastIndexByte(stat, ')')+2:]))[0]
}

// catches reports whether process pid has a handler of its own installed for sig.
func catches(t *testing.T, pid int, sig syscall.Signal) bool {
	t.Helper()
	return signalMaskHas(t, pid, "SigCgt", sig)
}

// ignores reports whether process pid ignores sig (SIG_IGN).
func ignores(t *testing.T, pid int, sig syscall.Signal) bool {
	t.Helper()
	return signalMaskHas(t, pid, "SigIgn", sig)
}

// signalMaskHas reports whether the signal mask field (SigCgt, SigIgn) of /proc/<pid>/status holds
// sig.
func signalMaskHas(t *testing.T, pid int, field string, sig syscall.Signal) bool {
	t.Helper()
	status, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(status), "\n") {
		if mask, ok := strings.CutPrefix(line, field+":"); ok {
			bits, err := strconv.ParseUint(strings.TrimSpace(mask), 16, 64)
			if err != nil {
				t.Fatal(err)
			}
			return bits&(1<<(uint(sig)-1)) != 0
		}
	}
	t.Fatalf("no %s line in /proc/%d/status", field, pid)
	return false
}

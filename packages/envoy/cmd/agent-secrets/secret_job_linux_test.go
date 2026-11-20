//go:build linux && (amd64 || arm64)

package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestPromptJobBackgroundThenForegroundKeepsValueHidden(t *testing.T) {
	for _, background := range []bool{false, true} {
		t.Run(fmt.Sprintf("started-in-background=%t", background), func(t *testing.T) {
			s := newPromptShell(t)
			s.start(false, background)
			if !background {
				s.send("\x1a")
				s.wait("Stopped")
				s.wait("PROMPT$ ")
				s.reset()
				s.send("bg\r")
				s.wait("PROMPT$ ")
			}
			// A stopped main thread is not proof that bash has reclaimed the tty.
			s.waitForeground(s.bash.Process.Pid)
			s.reset()
			s.send("fg\r")
			s.wait("\x1b[?2004h")
			s.send("headtail\r")
			if background {
				s.wait("RETURNED <nil> MATCH=true")
			} else {
				s.wait("RETURNED nothing was stored:")
				s.wait("MATCH=false EMPTY=true")
			}
			s.noShellValue("head", "tail")
		})
	}
}

// A stop the prompt takes while it waits in the background, before its label,
// discards nothing: nothing has been typed for it yet.
func TestPromptJobStopBeforeTheLabelKeepsTheEntry(t *testing.T) {
	s := newPromptShell(t)
	s.start(false, true)
	// Go leaves SIGTSTP at its default action until the prompt's watcher asks
	// for it, so a stop sent once the watcher catches it is the prompt's to handle.
	for budget := newWaitBudget(t); !catches(t, s.pid, syscall.SIGTSTP); time.Sleep(time.Millisecond) {
		if budget.spent() {
			t.Fatal("the prompt never caught SIGTSTP")
		}
	}
	s.send("kill -TSTP %1\r")
	// The job is in the background, so bash never takes the terminal back for it, and
	// fg does not resume a job bash has not yet seen stop: wait for its job table.
	s.jobStopped()
	s.waitForeground(s.bash.Process.Pid)
	s.reset()
	s.send("fg\r")
	s.wait("\x1b[?2004h")
	s.send("headtail\r")
	s.wait("RETURNED <nil> MATCH=true")
	s.noShellValue("head", "tail")
}

// A prompt whose process group is orphaned (the subshell that started it has exited) can
// never be brought to the foreground: it refuses, naming the pipe, rather than waiting forever,
// and shows no label over the shell's line.
func TestPromptJobOrphanedBackgroundPromptRefuses(t *testing.T) {
	s := newPromptShell(t)
	// Without job control a background command's standard input is /dev/null, so
	// the subshell names the terminal for it.
	command := "AGENT_SECRETS_JOB_REFERENCE=" + s.foregroundState() + " AGENT_SECRETS_JOB_HELPER=1 " +
		shellWord(os.Args[0]) + " -test.run='^TestPromptJobHelper$' </dev/tty"
	s.reset()
	s.send("(" + command + " &)\r")
	s.wait("HELPER_PID=")
	s.wait("\r\nHELPER_READY\r\n") // the whole pid line has come
	start := strings.Index(s.out.String(), "HELPER_PID=") + len("HELPER_PID=")
	pid, err := strconv.Atoi(strings.Fields(s.out.String()[start:])[0])
	if err != nil {
		t.Fatal(err)
	}
	s.pid = pid
	s.wait("RETURNED no shell can bring the value prompt to the foreground of this terminal; pipe the value in: agent-secrets secret set DEMO_KEY < FILE")
	s.wait("LABEL_HELD=false")
	if strings.Contains(s.out.String(), "Value for") {
		t.Fatalf("an orphaned prompt showed its label: %q", s.out.String())
	}
	// printf, not echo: the terminal's echo of `echo SHELL_ALIVE` would show the word before the
	// shell had run anything.
	s.send("printf 'SHELL_%s\\n' ALIVE\r")
	s.wait("SHELL_ALIVE\r\n")
}

// A signal that ends the prompt while it waits in the background, after Ctrl-Z and bg, ends it by
// that signal at once, without fg: the terminal is the shell's then, and a restore from the
// background would stop the job until fg.
func TestPromptJobSignalInTheBackgroundEndsItWithoutFg(t *testing.T) {
	for _, tc := range []struct{ signal, report string }{{"TERM", "Terminated"}, {"HUP", "Hangup"}} {
		t.Run(tc.signal, func(t *testing.T) {
			s := newPromptShell(t)
			s.start(false, false)
			s.send("\x1a")
			s.wait("Stopped")
			s.wait("PROMPT$ ")
			s.reset()
			s.send("bg\r")
			s.wait("PROMPT$ ")
			s.waitForeground(s.bash.Process.Pid)
			s.reset()
			s.send("kill -" + tc.signal + " %1\r")
			// bash reaps a job that ends; one a restore stopped stays in /proc in state T.
			for budget := newWaitBudget(t); ; time.Sleep(time.Millisecond) {
				stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", s.pid))
				if os.IsNotExist(err) {
					break
				}
				if budget.spent() {
					t.Fatalf("the prompt was still there after SIG%s in the background, past this wait's bound of %s: %q; terminal: %q", tc.signal, promptJobWait, stat, s.out.String())
				}
			}
			s.pid = 0
			// bash prints a background job's end before its next prompt.
			s.send("printf 'SHELL_%s\\n' ALIVE\r")
			s.wait("SHELL_ALIVE\r\n")
			s.wait(tc.report)
			if strings.Contains(s.out.String(), "Exit") || strings.Contains(s.out.String(), "RETURNED") {
				t.Fatalf("the job ended by exiting, not by SIG%s: %q", tc.signal, s.out.String())
			}
			if strings.Contains(s.out.String(), "\x1b[?2004l") {
				t.Fatalf("the prompt wrote to the shell's terminal from the background: %q", s.out.String())
			}
		})
	}
}

// A prompt sent to the background with Ctrl-Z and bg, whose shell then exits, is orphaned: it
// refuses, naming the pipe command, and leaves the terminal to the shell that holds it now, with
// no write of its own (a bracketed-paste-off mark would land on that shell's line, and its line
// editor would read its next paste unbracketed).
func TestPromptJobOrphanedByItsShellsExitLeavesTheTerminalAlone(t *testing.T) {
	s := newPromptShell(t)
	s.reset() // the outer shell's prompt is no sign the inner one is up
	s.send("bash --norc --noprofile -i\r")
	s.wait("PROMPT$ ")
	s.reset()
	s.send("printf 'NESTED_%s\\n' READY\r")
	s.wait("NESTED_READY\r\n")
	s.wait("PROMPT$ ")
	s.start(false, false)
	s.send("\x1a")
	s.wait("Stopped")
	s.wait("PROMPT$ ")
	s.reset() // so the prompt waited for next is the one after bg
	s.send("bg\r")
	s.wait("PROMPT$ ")
	s.reset()
	s.send("exit\r")
	s.wait("RETURNED no shell can bring the value prompt to the foreground of this terminal; pipe the value in: agent-secrets secret set DEMO_KEY < FILE")
	s.pid = 0
	s.send("printf 'SHELL_%s\\n' ALIVE\r")
	s.wait("SHELL_ALIVE\r\n")
	if strings.Contains(s.out.String(), "\x1b[?2004l") {
		t.Fatalf("the orphaned prompt wrote to the terminal the outer shell holds: %q", s.out.String())
	}
}

func TestPromptJobFastForegroundDoesNotRestopOrExposeValue(t *testing.T) {
	for _, background := range []bool{false, true} {
		t.Run(fmt.Sprintf("bg-before-fg=%t", background), func(t *testing.T) {
			s := newPromptShell(t)
			s.start(false, false)
			s.send("\x1a")
			// Queue fg at the first stopped thread, without waiting for all of the
			// job's threads or bash to reclaim the terminal.
			for budget := newWaitBudget(t); processState(t, s.pid) != "T"; time.Sleep(time.Millisecond) {
				if budget.spent() {
					t.Fatal("prompt did not stop")
				}
			}
			if background {
				s.send("bg\r")
			}
			s.send("fg\r")
			s.wait("\x1b[?2004h")
			s.reset()
			s.send("headtail\r")
			s.wait("RETURNED nothing was stored:")
			s.wait("MATCH=false EMPTY=true")
			// Bash's own "Stopped" notice can lag arbitrarily far behind a real
			// stop-then-resume it already handled (it only prints it once its own
			// job-table check next runs), so it is not evidence of a second stop by
			// itself. A second bracketed-paste-on write is: the reader emits
			// exactly one, from apply(), each time it (re)gains the terminal, so a
			// second one here means a second, unwanted stop-and-resume cycle.
			if strings.Contains(s.out.String(), "\x1b[?2004h") {
				t.Fatalf("prompt reapplied terminal settings after fg (stopped again): %q", s.out.String())
			}
			s.noShellValue("head", "tail")
		})
	}
}

func TestPromptJobControlByteDrainsBeforeReturningToShell(t *testing.T) {
	for _, control := range []string{"\t", "\x1b[D", "\x00"} {
		t.Run(fmt.Sprintf("%x", control), func(t *testing.T) {
			s := newPromptShell(t)
			s.start(false, false)
			s.send("prefix" + control)
			for _, c := range "private-tail" {
				time.Sleep(80 * time.Millisecond)
				s.send(string(c))
			}
			s.send("\r")
			s.wait("RETURNED the control byte")
			s.noShellValue("private-tail")
		})
	}
}

func TestPromptJobInheritedIgnoredStopStaysIgnored(t *testing.T) {
	s := newPromptShell(t)
	s.env = "AGENT_SECRETS_JOB_ANNOUNCE_IGNORED_STOP=1 "
	s.start(true, false)
	s.send("head\x1a")
	// Wait for the reader to handle Ctrl-Z and ask for more input, not just remove the byte
	// from the kernel's queue: READ_SOME is printed before the reader handles those bytes.
	s.wait("IGNORED_STOP_HANDLED")
	if lflag(t, s.terminal)&unix.ECHO != 0 {
		t.Fatal("ignored Ctrl-Z restored echo")
	}
	s.send("tail\r")
	s.wait("RETURNED <nil> MATCH=true")
	s.wait("WRAPPER_DONE")
	s.noShellValue("head", "tail")
}

func TestPromptControlByteInsideBracketedPasteDrainsThroughEnd(t *testing.T) {
	for _, control := range []string{"\x00", "\t", "\x01", "\x7f"} {
		controller, terminal, got := typeAtPromptInWrites(t, 300*time.Millisecond,
			"\x1b[200~prefix"+control, "private-tail\n\x1b[201~")
		if got.err == nil || !strings.Contains(got.err.Error(), "control byte") || got.line != nil {
			t.Fatalf("control %q: got %q, %v", control, got.line, got.err)
		}
		if unread(t, terminal) != 0 || bytes.Contains(shown(t, controller), []byte("private-tail")) {
			t.Fatal("paste tail leaked")
		}
	}
}

func TestPromptJobStopDuringQuietWindowDoesNotHang(t *testing.T) {
	s := newPromptShell(t)
	// The helper holds the quiet window open until the Ctrl-Z comes, so the stop lands inside it
	// however slowly this test runs; the window after the resume is not held.
	s.env = "AGENT_SECRETS_JOB_HOLD_QUIET=1 "
	s.start(false, false)
	s.send("headtail\r")
	s.wait("QUIET_READY")
	s.send("\x1a")
	s.wait("Stopped")
	s.reset()
	s.send("fg\r")
	// No further input is needed to finish the quiet window after a resume.
	s.wait("RETURNED nothing was stored:")
	s.wait("MATCH=false EMPTY=true")
	s.noShellValue("headtail")
}

func TestPromptJobStopDiscardsQueuedInputWithoutLeaking(t *testing.T) {
	s := newPromptShell(t)
	s.start(false, false)
	// A single write exercises the prompt's input flush without a scheduling gap.
	s.send("head\x1a")
	s.wait("Stopped")
	s.reset()
	s.send("fg\r")
	s.wait("Nothing was stored. Press Enter")
	s.wait("run agent-secrets secret set DEMO_KEY again and type the whole value")
	s.send("tail\r")
	s.wait("RETURNED nothing was stored:")
	s.wait("MATCH=false EMPTY=true")
	s.noShellValue("head", "tail")

	s.start(false, false)
	s.send("headtail\r")
	s.wait("RETURNED <nil> MATCH=true")
	s.noShellValue("head", "tail")
}

// Ctrl-Z discards whatever follows it in the same write, as a fast typist's or a paste's keys
// arrive: the prompt stops with the entry refused, and none of it reaches the shell.
func TestPromptJobStopKeyDiscardsWhatFollowsIt(t *testing.T) {
	s := newPromptShell(t)
	s.start(false, false)
	s.send("partone\x1aparttwo\r")
	s.wait("Stopped")
	s.wait("PROMPT$ ")
	s.reset()
	s.send("fg\r")
	s.wait("Nothing was stored. Press Enter")
	s.send("\r")
	s.wait("RETURNED nothing was stored:")
	s.wait("MATCH=false EMPTY=true")
	s.noShellValue("partone", "parttwo")
}

// A signal key inside a bracketed paste is pasted text, not a keypress: the prompt refuses it as a
// control byte and discards the paste through its end, in one write or with the rest arriving in a
// later read, so none of the paste reaches the shell.
func TestPromptJobSignalKeyInsideAPasteDrainsThroughItsEnd(t *testing.T) {
	for _, key := range []string{"\x03", "\x1a", "\x1c"} {
		for _, later := range []bool{false, true} {
			name := fmt.Sprintf("%x/one-write", key)
			if later {
				name = fmt.Sprintf("%x/rest-in-a-later-read", key)
			}
			t.Run(name, func(t *testing.T) {
				s := newPromptShell(t)
				var release func()
				if later {
					release = s.holdReads()
				}
				s.start(false, false)
				head, tail := "\x1b[200~pastehead"+key, "pastetail\r\x1b[201~"
				if !later {
					s.send(head + tail)
				} else {
					s.send(head)
					// The prompt waits for the rest at the open paste's bound, held, so the rest
					// comes in a later read however slowly this test runs.
					s.nextReadWait(maxPasteDrain.Milliseconds())
					s.send(tail)
					release()
				}
				s.wait(fmt.Sprintf("RETURNED the control byte 0x%02x cannot be typed at the prompt", key[0]))
				s.noShellValue("pastehead", "pastetail")
			})
		}
	}
}

// A paste that begins after the typed line, while the prompt drains what follows it, is read
// through its closing mark like one in the line: a signal key inside it is pasted text, and the
// paste's rest, arriving in a later read, is discarded rather than run by the shell. The entry is
// refused as more than one line. The helper holds each timed read until the next part comes, so
// the paste starts inside the quiet window, and its rest inside the paste's bound, however slowly
// this test runs.
func TestPromptJobPasteAfterTheLineDrainsThroughItsEnd(t *testing.T) {
	for _, key := range []string{"\x03", "\x1c", "\x1a", ""} {
		name := fmt.Sprintf("%x", key)
		if key == "" {
			name = "no-key"
		}
		t.Run(name, func(t *testing.T) {
			s := newPromptShell(t)
			release := s.holdReads()
			s.start(false, false)
			s.send("headtail\r")
			s.nextReadWait(pasteGapDeciseconds * 100)
			s.send("\x1b[200~pastelate" + key)
			s.nextReadWait(maxPasteDrain.Milliseconds())
			s.send("echo PASTE_$((2+3))\r\x1b[201~")
			release()
			s.wait("RETURNED a value of more than one line must be piped in")
			// Clear the shell's line, where a leaked paste mark would leave text that breaks the
			// history check's own command, so a leak shows as the markers below.
			s.send("\x15")
			s.noShellValue("pastelate", "PASTE_$((", "PASTE_5")
		})
	}
}

// A bracketed paste that begins in the same read as the line's end is read on through its closing
// mark, not given back to the shell after the line: the prompt keeps the terminal, so the paste's
// rest, arriving in a later read, is discarded rather than run by the shell. The entry is refused
// as more than one line.
func TestPromptJobPasteInTheLineEndReadDrainsThroughItsEnd(t *testing.T) {
	for _, key := range []string{"\x03", "\x1c", "\x1a", ""} {
		name := fmt.Sprintf("%x", key)
		if key == "" {
			name = "no-key"
		}
		t.Run(name, func(t *testing.T) {
			s := newPromptShell(t)
			release := s.holdReads()
			s.start(false, false)
			// One write carries the line's end and the paste's start; the prompt reads both at
			// once, so a paste the prompt only watches for after the line would be missed.
			s.send("headtail\r\x1b[200~pastelate" + key)
			// A prompt that saw the paste waits for its rest at the paste's bound, held until the
			// rest comes; one that missed it would wait out the quiet window instead, and this
			// bound would never show.
			s.nextReadWait(maxPasteDrain.Milliseconds())
			s.send("echo PASTE_$((2+3))\r\x1b[201~")
			release()
			s.wait("RETURNED a value of more than one line must be piped in")
			// Clear the shell's line, where a leaked paste mark would leave text that breaks
			// the history check's own command, so a leak shows as the markers below.
			s.send("\x15")
			s.noShellValue("headtail", "pastelate", "PASTE_$((", "PASTE_5")
		})
	}
}

// A paste-start mark split so that only its first bytes arrive in the read that ends the line is
// held as pending and completed from the next read: the paste opens, so a Ctrl-C after its content
// is pasted text, not a keypress, and the entry is refused as more than one line rather than its
// content run by the shell. The helper shortens the paste bound so the refusal does not wait the
// full bound for a close mark that never comes.
func TestPromptJobSplitPasteMarkAfterTheLineIsRead(t *testing.T) {
	s := newPromptShell(t)
	s.env = "AGENT_SECRETS_JOB_HOLD_QUIET=1 AGENT_SECRETS_JOB_PASTE_BOUND=1s "
	s.start(false, false)
	s.send("headtail\r\x1b[20")
	s.wait("QUIET_READY")
	s.send("0~pastelate\x03")
	s.wait("RETURNED a value of more than one line must be piped in")
	s.noShellValue("headtail", "pastelate")
}

// A paste-start mark split so that only its first bytes arrive in the read that ends the line, with
// the rest arriving in a later read, still opens a paste: while a partial mark is pending the drain
// waits a second for the rest, longer than the quiet window, so a terminal that splits those six
// bytes across writes that far apart does not give the terminal back before the mark completes and
// the paste's content runs in the shell. The test checks that the drain asks for that second, and
// the helper holds the read until the rest comes. The entry is refused as more than one line. The
// helper shortens the paste bound so the refusal does not wait the full bound for a close mark
// that never comes.
func TestPromptJobSplitPasteMarkRestAfterTheQuietWindowIsRead(t *testing.T) {
	s := newPromptShell(t)
	s.env = "AGENT_SECRETS_JOB_PASTE_BOUND=1s "
	release := s.holdReads()
	s.start(false, false)
	s.send("headtail\r\x1b[20")
	s.nextReadWait(1000) // a second for the mark's rest, not the 200 ms quiet window
	s.send("0~pastelate\x03")
	s.nextReadWait(1000) // the paste's shortened bound
	s.send("echo PASTE_$((2+3))\r\x1b[201~")
	release()
	s.wait("RETURNED a value of more than one line must be piped in")
	s.send("\x15")
	s.noShellValue("headtail", "pastelate", "PASTE_$((", "PASTE_5")
}

// A second bracketed paste that begins in the same read that closes a pasted line opens like any
// other: the prompt keeps reading, so its rest, arriving in a later read, is discarded rather than
// run by the shell, and the pasted line is refused as more than one line.
func TestPromptJobSecondPasteInThePastedLinesCloseIsRead(t *testing.T) {
	s := newPromptShell(t)
	release := s.holdReads()
	s.start(false, false)
	s.send("\x1b[200~firstpaste\r\x1b[201~\x1b[200~pastelate")
	s.nextReadWait(maxPasteDrain.Milliseconds())
	s.send("echo PASTE_$((2+3))\r\x1b[201~")
	release()
	s.wait("RETURNED a value of more than one line must be piped in")
	s.send("\x15")
	s.noShellValue("firstpaste", "pastelate", "PASTE_$((", "PASTE_5")
}

// A paste whose closing mark never comes, as from a terminal that sends only the start mark, is
// given up once it has fallen quiet for the whole bound without closing: the prompt ends, refusing
// the entry, though the signal keys pressed meanwhile are pasted text. The helper shortens the bound.
func TestPromptJobPasteThatNeverEndsIsGivenUp(t *testing.T) {
	for _, key := range []string{"\x03", "\x1c", "\x1a"} {
		t.Run(fmt.Sprintf("%x", key), func(t *testing.T) {
			s := newPromptShell(t)
			s.env = "AGENT_SECRETS_JOB_PASTE_BOUND=1s "
			s.start(false, false)
			s.send("\x1b[200~pasteopen" + key)
			s.wait("RETURNED read the value at the terminal: " + errPasteCutShort.Error())
			s.noShellValue("pasteopen")
		})
	}
}

// A paste that keeps arriving past the bound is read on, not given up: the bound counts the quiet
// since the last input, restarted at each read, so a slow paste is drained however long it takes and
// its lines never reach the shell. The helper shortens the bound. Each line goes once the prompt
// waits for it with the whole bound again, held until the line comes, and the lines keep coming for
// longer than the bound in all; once they stop, the bound runs out and the paste is given up.
func TestPromptJobLongPasteDrainsWhileInputKeepsArriving(t *testing.T) {
	s := newPromptShell(t)
	s.env = "AGENT_SECRETS_JOB_PASTE_BOUND=1s "
	release := s.holdReads()
	s.start(false, false)
	s.send("\x1b[200~")
	for range 20 {
		s.nextReadWait(1000)
		s.send("echo LEAKED_$((9+9))\r")
		time.Sleep(100 * time.Millisecond)
	}
	release()
	s.wait("RETURNED read the value at the terminal: " + errPasteCutShort.Error())
	s.noShellValue("LEAKED_18", "LEAKED_$((")
}

// A value deadline that passes while a bracketed paste is open refuses the entry, but the prompt
// keeps the terminal and reads the paste on through its closing mark before it gives the terminal
// back, so the paste's rest, arriving after the deadline, is discarded rather than run by the
// shell. The helper places the deadline inside the paste (AGENT_SECRETS_JOB_DEADLINE_MID_ENTRY):
// the paste begins before the deadline, or only after it (sent once the helper has said the
// deadline passed), and the paste's rest goes only once the prompt has either read on past it or
// given the terminal back to the shell, so a prompt that gives the paste up shows that rest
// running there. The notice says the paste must end first: inside it Ctrl-C is pasted text, which
// deletes nothing. In the third case a signal interrupts the reader's zero read at the deadline
// (AGENT_SECRETS_JOB_INTERRUPT_DEADLINE_READ): the retry must still end at the deadline, where a
// retry that waited the bound its read began with took the paste's rest as if it had come in time
// and refused the entry as more than one line, the notice never given.
func TestPromptJobValueDeadlineInsideAPasteDrainsItThroughItsEnd(t *testing.T) {
	for _, tc := range []struct {
		name               string
		after, interrupted bool
	}{
		{"the paste begun after the deadline=false", false, false},
		{"the paste begun after the deadline=true", true, false},
		{"the zero read at the deadline interrupted", false, true},
	} {
		after := tc.after
		t.Run(tc.name, func(t *testing.T) {
			stallRetries(t, func(s *promptShell) bool {
				s.env = "AGENT_SECRETS_JOB_DEADLINE=2s AGENT_SECRETS_JOB_DEADLINE_MID_ENTRY=1 AGENT_SECRETS_JOB_ANNOUNCE_DEADLINE=1 "
				if tc.interrupted {
					s.env += "AGENT_SECRETS_JOB_INTERRUPT_DEADLINE_READ=1 "
				}
				// The paste's read after the deadline is held until its rest comes, so the rest
				// arrives inside the paste's bound however slowly this test runs.
				release := s.holdReads()
				s.start(false, false)
				// The helper's clock stands still until the prompt's first wait, so the deadline
				// passes after the label, which start keeps what follows.
				if after {
					s.wait("DEADLINE_REACHED")
				}
				s.send("\x1b[200~pastehead")
				if s.waitFirst("DEADLINE_PASSED", setupOutranDeadline) == setupOutranDeadline {
					return false
				}
				if tc.interrupted {
					s.wait("DEADLINE_READ_INTERRUPTED")
				}
				if s.waitFirst("READING_ON", "RETURNED") == "RETURNED" {
					s.wait("PROMPT$ ")
				}
				s.send("\recho PASTE_$((2+3))\r\x1b[201~")
				release()
				s.wait("RETURNED nothing was stored: no value came in time, within 2s")
				// The prompt said at the deadline that the entry is refused, and that the empty
				// secret stays until the paste and then the entry end, so the person knows to stop.
				notice := s.out.String()
				if !strings.Contains(notice, "The value deadline passed with an entry under way: no value came within 2s of the create, so none is stored. The empty secret DEMO_KEY stays in the store until this entry ends") ||
					!strings.Contains(notice, "finish the paste, then press Enter") || strings.Contains(notice, "Ctrl-C deletes it now") {
					t.Fatalf("the prompt's notice at its deadline inside a paste %q; want it to say the paste must end first, and never that Ctrl-C deletes the secret", notice)
				}
				// Clear the shell's line, where a leaked paste mark would leave text that breaks
				// the history check's own command, so a leak shows as the markers below.
				s.send("\x15")
				s.noShellValue("pastehead", "PASTE_$((", "PASTE_5")
				return true
			})
		})
	}
}

// TestPromptJobValueDeadlineMidEntryNoticeNamesWhatEndsTheEntry: the notice a value deadline
// gives with a typed entry under way says what ends the entry, which is when a create deletes its
// empty secret: Enter, and, at once, the interrupt key where it ends the process. Where SIGINT is
// live, the notice says Ctrl-C deletes the secret now, and a Ctrl-C ends the process by SIGINT,
// running the cleanup that deletes it (DEATH_HOOK_RAN). With SIGINT inherited as ignored the key
// does nothing, so the notice names Enter and ending the process instead, never Ctrl-C, and a
// Ctrl-C leaves the entry open until Enter. The helper's clock stands still until the prompt's
// first wait, which lasts until the entry's first input comes: a stall before that wait uses none
// of the deadline, and input that comes after the deadline is refused as late, with the same
// notice. (The paste case is TestPromptJobValueDeadlineInsideAPasteDrainsItThroughItsEnd.)
func TestPromptJobValueDeadlineMidEntryNoticeNamesWhatEndsTheEntry(t *testing.T) {
	const notice, live, end = "The value deadline passed with an entry under way", "Ctrl-C deletes it now", "Then run agent-secrets secret set DEMO_KEY again"
	t.Run("the interrupt key live", func(t *testing.T) {
		s := newPromptShell(t)
		s.env = "AGENT_SECRETS_JOB_DEADLINE=2s AGENT_SECRETS_JOB_DEADLINE_MID_ENTRY=1 AGENT_SECRETS_JOB_DEATH_HOOK=1 "
		s.start(false, false)
		s.send("head")
		s.wait(notice)
		s.wait(end)
		if !strings.Contains(s.out.String(), "until you press Enter to end this entry, and the create deletes it then; "+live) {
			t.Fatalf("the notice %q; want it to say Enter ends the entry and Ctrl-C deletes the secret now", s.out.String())
		}
		s.send("\x03")
		s.wait("DEATH_HOOK_RAN")
		s.wait("PROMPT$ ")
		s.send("echo STATUS_$?\r")
		s.wait("STATUS_130")
	})
	t.Run("SIGINT inherited ignored", func(t *testing.T) {
		s := newPromptShell(t)
		s.env = "AGENT_SECRETS_JOB_DEADLINE=2s AGENT_SECRETS_JOB_DEADLINE_MID_ENTRY=1 "
		s.wrapperIgnores = "INT"
		s.start(true, false)
		s.send("head")
		s.wait(notice)
		s.wait(end)
		if said := s.out.String(); strings.Contains(said, live) || !strings.Contains(said, "until you press Enter to end this entry, or end this process (close this terminal, or send it SIGTERM)") {
			t.Fatalf("the notice %q; want it to name Enter and ending the process, and never Ctrl-C, which does nothing with SIGINT ignored", said)
		}
		s.send("\x03")
		s.send("tail\r")
		s.wait("RETURNED nothing was stored: no value came in time, within 2s")
		s.wait("WRAPPER_DONE")
		s.noShellValue("head", "tail")
	})
}

// A prompt started with & that is never brought to the foreground gives up at its value deadline,
// rather than waiting for fg past the time the broker holds a create's secret for: it ends with the
// timeout, having shown no label and never taken the terminal. The helper's clock stands still
// until the prompt's first wait for the watcher's event, so the deadline passes while it waits.
func TestPromptJobValueDeadlineEndsAPromptWaitingForTheForeground(t *testing.T) {
	s := newPromptShell(t)
	s.env = "AGENT_SECRETS_JOB_DEADLINE=1s "
	s.start(false, true)
	s.wait("RETURNED nothing was stored: no value came in time, within 1s")
	s.wait("LABEL_HELD=false")
	if strings.Contains(s.out.String(), "Value for") {
		t.Fatalf("a prompt that never held the terminal showed its label: %q", s.out.String())
	}
}

// A prompt waiting in the background for the foreground gives up at its value deadline however
// often signals interrupt its waits for the watcher's event (AGENT_SECRETS_JOB_INTERRUPT_WATCH):
// the wait for the foreground hands each of them the deadline (waitForeground), so one interrupted
// once the deadline has passed ends, where a wait handed no deadline was tried again for as long as
// the signals kept coming, and the prompt never answered. The deadline's clock stands still until
// the prompt's first wait, so however long the machine holds the helper before that wait, the
// deadline passes only while the prompt waits; the helper's own run fails where the prompt
// answered the deadline before it passed on that clock, as one that gives up before its first
// wait or at an interrupted one does, and the test reads that run's end (helperFailure). The
// count of the waits the helper interrupted, which shows the signals reached them, is logged.
func TestPromptJobValueDeadlineEndsAForegroundWaitSignalsKeepInterrupting(t *testing.T) {
	s := newPromptShell(t)
	s.env = "AGENT_SECRETS_JOB_DEADLINE=1s AGENT_SECRETS_JOB_INTERRUPT_WATCH=1 "
	s.start(false, true)
	s.wait("RETURNED nothing was stored: no value came in time, within 1s")
	s.wait("LABEL_HELD=false")
	if failure := s.helperFailure(); failure != "" {
		t.Fatalf("the helper's own run failed: %q", failure)
	}
	_, rest, _ := strings.Cut(s.out.String(), "WATCH_INTERRUPTED=")
	interrupted, _ := strconv.Atoi(rest[:len(rest)-len(strings.TrimLeft(rest, "0123456789"))])
	t.Logf("the helper interrupted %d waits for the watcher's event", interrupted)
}

// A signal key that follows a paste's closing mark in the same read is a keypress: it acts as it
// would in a read of its own, so the pasted value is not stored. Ctrl-C and Ctrl-\ end the
// process by their signals; Ctrl-Z stops it, and after fg the entry is refused.
func TestPromptJobKeyAfterAPasteInOneWriteActs(t *testing.T) {
	paste := "\x1b[200~pastevalue\r\x1b[201~"
	for _, tc := range []struct{ key, status string }{{"\x03", "130"}, {"\x1c", "131"}} {
		t.Run(fmt.Sprintf("%x", tc.key), func(t *testing.T) {
			s := newPromptShell(t)
			s.start(false, false)
			s.send(paste + tc.key)
			s.wait("PROMPT$ ")
			s.send("echo STATUS_$?\r")
			s.wait("STATUS_" + tc.status)
			if strings.Contains(s.out.String(), "RETURNED") {
				t.Fatalf("want the process ended by its signal (status %s), not a returned value: %q", tc.status, s.out.String())
			}
		})
	}
	t.Run("1a", func(t *testing.T) {
		s := newPromptShell(t)
		s.start(false, false)
		s.send(paste + "\x1a")
		s.wait("Stopped")
		s.wait("PROMPT$ ")
		s.reset()
		s.send("fg\r")
		s.wait("Nothing was stored. Press Enter")
		s.send("\r")
		s.wait("RETURNED nothing was stored:")
		s.wait("MATCH=false EMPTY=true")
		s.noShellValue("pastevalue")
	})
}

// A signal that ends the prompt runs onSignalDeath first, which create sets while the secret it
// made is empty: Ctrl-C and Ctrl-\ at the prompt, and a termination sent to the process, each run
// it once, after the terminal is restored - the restore's bracketed-paste-off mark reaches the
// terminal before the cleanup's line - and the shell still reports the signal's own status.
// Create's own watch on the signals runs beside the prompt's (watchCreateSignals) and leaves a
// signal that comes during the prompt to the prompt, so the cleanup runs once, and only once the
// prompt has restored the terminal.
func TestPromptJobSignalRunsTheDeathCleanupFirst(t *testing.T) {
	for _, tc := range []struct {
		name, key string
		sig       syscall.Signal
		status    string
	}{{"ctrl-c", "\x03", 0, "130"}, {"ctrl-backslash", "\x1c", 0, "131"}, {"sigterm", "", syscall.SIGTERM, "143"}} {
		t.Run(tc.name, func(t *testing.T) {
			s := newPromptShell(t)
			s.env = "AGENT_SECRETS_JOB_DEATH_HOOK=1 "
			s.start(false, false)
			if tc.sig != 0 {
				if err := syscall.Kill(s.pid, tc.sig); err != nil {
					t.Fatal(err)
				}
			} else {
				s.send(tc.key)
			}
			s.wait("DEATH_HOOK_RAN")
			s.wait("PROMPT$ ")
			s.send("echo STATUS_$?\r")
			s.wait("STATUS_" + tc.status)
			out := s.out.String()
			if n := strings.Count(out, "DEATH_HOOK_RAN"); n != 1 || strings.Contains(out, "RETURNED") {
				t.Fatalf("the cleanup ran %d times, terminal %q; want once, and the process ended by its signal", n, out)
			}
			if restored := strings.Index(out, string(bracketedPasteOff)); restored < 0 || restored > strings.Index(out, "DEATH_HOOK_RAN") {
				t.Fatalf("terminal %q: want the prompt's restore before the cleanup ran", out)
			}
		})
	}
}

// A second signal while the cleanup a signal at the prompt ran is still waiting - a store call in
// flight that does not answer - ends the process at once, by that second signal, once the prompt
// has restored the terminal, and prints create's line for it first: Ctrl-C at the prompt, then a
// termination, and the shell reports the termination's status.
func TestPromptJobSecondSignalEndsAWaitingCleanupAtOnce(t *testing.T) {
	s := newPromptShell(t)
	s.env = "AGENT_SECRETS_JOB_DEATH_HOOK=hang "
	s.start(false, false)
	s.send("\x03")
	s.wait("DEATH_HOOK_RAN")
	if err := syscall.Kill(s.pid, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	s.wait("SECOND_SIGNAL_LINE terminated")
	s.wait("PROMPT$ ")
	s.send("echo STATUS_$?\r")
	s.wait("STATUS_143")
	out := s.out.String()
	if restored := strings.Index(out, string(bracketedPasteOff)); restored < 0 || restored > strings.Index(out, "SECOND_SIGNAL_LINE") || strings.Contains(out, "RETURNED") {
		t.Fatalf("terminal %q: want the prompt's restore before the second signal's line, and the process ended by that signal", out)
	}
}

// A prompt started with & whose shell then exits has no shell to bring it forward, though the
// terminal stays open: it refuses, naming the pipe, rather than showing its label to nobody and
// reading a value there.
func TestPromptJobShellGoneRefuses(t *testing.T) {
	s := newPromptShell(t)
	s.start(false, true)
	s.send("exit\r")
	s.wait("RETURNED no shell can bring the value prompt to the foreground of this terminal; pipe the value in: agent-secrets secret set DEMO_KEY < FILE")
	s.wait("LABEL_HELD=false")
	if strings.Contains(s.out.String(), "Value for") {
		t.Fatalf("a prompt whose shell had gone showed its label: %q", s.out.String())
	}
}

// A prompt started with & that reaches the terminal only after its shell has exited finds a
// terminal that is no longer its controlling one: it refuses as when the shell exits under it,
// rather than take the terminal for one job control never applied to.
func TestPromptJobShellGoneBeforeThePromptStartsRefuses(t *testing.T) {
	s := newPromptShell(t)
	s.env = "AGENT_SECRETS_JOB_AFTER_SHELL_EXIT=1 "
	s.start(false, true)
	s.send("exit\r")
	s.wait("RETURNED no shell can bring the value prompt to the foreground of this terminal; pipe the value in: agent-secrets secret set DEMO_KEY < FILE")
	s.wait("LABEL_HELD=false")
	if strings.Contains(s.out.String(), "Value for") {
		t.Fatalf("a prompt that started after its shell had gone showed its label: %q", s.out.String())
	}
}

// A stop no handler sees (SIGSTOP from another process) lets the shell put its own mode back,
// echo on, while the job is stopped. After fg the prompt puts its own mode back before it reads
// again, so the rest of the value stays hidden.
func TestPromptJobExternalStopKeepsTheValueHidden(t *testing.T) {
	s := newPromptShell(t)
	s.start(false, false)
	s.send("head")
	// The prompt must have read the bytes first: the shell would read any still queued.
	s.wait("READ_SOME")
	s.allInputRead()
	if err := syscall.Kill(s.pid, syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	s.wait("Stopped")
	s.wait("PROMPT$ ")
	s.reset()
	s.send("fg\r")
	// The prompt's mode is back once it turns bracketed paste on again.
	s.wait("\x1b[?2004h")
	s.send("tail\r")
	s.wait("RETURNED <nil> MATCH=true")
	s.noShellValue("head", "tail")
}

func TestPromptHangupRestoresAndAbortCannotDumpTheValue(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGHUP, syscall.SIGTERM, syscall.SIGABRT} {
		t.Run(sig.String(), func(t *testing.T) {
			controller, terminal := openPTY(t)
			fd, err := unix.Dup(terminal)
			if err != nil {
				t.Fatal(err)
			}
			tty := os.NewFile(uintptr(fd), "terminal")
			defer tty.Close()
			dir := t.TempDir()
			cmd := exec.Command("bash", "-c",
				`ulimit -c "$(ulimit -Hc)" && exec env --default-signal=HUP,TERM,ABRT "$0" -test.run='^TestPromptSignalHelper$'`,
				testBinary(t))
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "AGENT_SECRETS_PROMPT_HELPER=1", "GOTRACEBACK=crash")
			cmd.Stdin, cmd.Stdout, cmd.Stderr = tty, tty, tty
			cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = cmd.Process.Kill() })
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			awaitEchoOff(t, terminal)
			if _, err := controller.Write([]byte("private-value")); err != nil {
				t.Fatal(err)
			}
			if err := cmd.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			// Keep consuming until the process exits; a fixed read deadline can fill
			// the pty halfway through Go's SIGABRT diagnostics and block the crash.
			output := make(chan []byte, 1)
			go func() {
				var out bytes.Buffer
				buf := make([]byte, 4096)
				for {
					n, err := controller.Read(buf)
					out.Write(buf[:n])
					if err != nil {
						output <- out.Bytes()
						return
					}
				}
			}()
			exitWithin := 10 * time.Second
			if sig == syscall.SIGABRT {
				// Go's GOTRACEBACK=crash relays SIGQUIT between threads. A
				// SIGQUIT subscriber can make that relay wait for its 10s
				// watchdog before the runtime raises the fatal SIGABRT.
				exitWithin = 30 * time.Second
			}
			select {
			case <-done:
			case <-time.After(exitWithin):
				_ = controller.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
				state, _ := os.ReadFile(fmt.Sprintf("/proc/%d/status", cmd.Process.Pid))
				t.Fatalf("signal did not end the prompt; process: %s; terminal: %q", state, <-output)
			}
			if err := controller.SetReadDeadline(time.Now().Add(300 * time.Millisecond)); err != nil {
				t.Fatal(err)
			}
			out := <-output
			status := cmd.ProcessState.Sys().(syscall.WaitStatus)
			if !status.Signaled() || status.Signal() != sig || status.CoreDump() {
				t.Fatalf("status=%v; want signal %v without a core", status, sig)
			}
			left, err := os.ReadDir(dir)
			if err != nil || len(left) != 0 {
				t.Fatalf("core directory=%v, err=%v", left, err)
			}
			if bytes.Contains(out, []byte("private-value")) {
				t.Fatal("signal output exposed the value")
			}
			if sig != syscall.SIGABRT && lflag(t, terminal)&(unix.ECHO|unix.ICANON) != unix.ECHO|unix.ICANON {
				t.Fatal("signal did not restore the terminal")
			}
		})
	}
}

// A failed input flush on the stop path is reported: Ctrl-Z discards what the terminal holds unread
// to keep it from the shell, and when that discard fails the refused entry says so. discardInput has
// never failed on a held terminal in practice; the helper makes it fail.
func TestPromptJobStopDiscardFailureIsReported(t *testing.T) {
	s := newPromptShell(t)
	s.env = "AGENT_SECRETS_JOB_FAIL_DISCARD=1 "
	s.start(false, false)
	s.send("head\x1a")
	s.wait("Stopped")
	s.wait("PROMPT$ ")
	s.reset()
	s.send("fg\r")
	s.wait("RETURNED read the value at the terminal: discard the terminal's unread input")
}

package testbin

import (
	"bytes"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// homeStopGrace is how long the end of a test waits for a process still running under its Oh My
// Pi HOME to exit after SIGTERM, and again after SIGKILL. Oh My Pi's daemon broker ends on SIGTERM
// by stopping each daemon it runs, with 2 s for each (launch/broker.ts's shutdown at the pin), so
// the first wait holds the broker's whole shutdown with room for a loaded machine.
const homeStopGrace = 15 * time.Second

// homeStopRounds bounds how many times the end of a test lists the processes under its HOME
// again after it has ended the ones it found: a process that one of them started between the list
// and its signal is found by the next list.
const homeStopRounds = 3

// homeProcess is one process whose environment names a test's HOME: its pid, its start ticks
// (field 22 of /proc/<pid>/stat), which tell it from a later process given the same pid, and its
// command line, for the report.
type homeProcess struct {
	pid     int
	ticks   uint64
	command string
}

func (p homeProcess) String() string {
	command := p.command
	if len(command) > 160 {
		command = command[:160] + "..."
	}
	return "pid " + strconv.Itoa(p.pid) + " (" + command + ")"
}

// stopAtEnd registers the cleanup that ends every process still running under home when the test
// ends, and waits until each has gone, before the test's TempDir holding home is removed.
//
// A process group does not hold everything Oh My Pi starts. Its daemon broker is spawned detached
// (launch/client.ts at the pin), so it leads a session of its own, and so does the headless
// Chromium the broker runs for the browser tool (omp.browser.headless). Neither dies with the
// process group a test signals, and both keep writing under home: the broker, as it shuts down
// once its clients have gone or its runtime directory was removed, writes each daemon's
// meta.json again with Bun.write, which makes the parent directories again, and Chromium writes
// its profile as it exits. A TempDir removal racing those writes fails the test with
// `unlinkat …/run/daemons: directory not empty`, after every step of it passed.
//
// The processes are found by their environment, which records the HOME each one started with:
// every process Oh My Pi starts takes its whole environment (workerEnvFromParent, at the pin),
// however it detaches. A process whose environment cannot be read is another user's, or a
// sandboxed Chromium helper, which ends with the browser process that started it.
//
// The cleanup is registered when home is readied, after the TempDir that holds home was made,
// so it runs before that TempDir's removal; and after nothing the test registers later, so a
// runtime that stops its own panes at the end has stopped them first.
func stopAtEnd(t *testing.T, home string, grace time.Duration) {
	t.Cleanup(func() {
		if runtime.GOOS != "linux" {
			t.Logf("testbin: %s has no /proc, so no process still running under %s could be listed, and none was ended", runtime.GOOS, home)
			return
		}
		stopped, err := stopUnder(home, grace)
		if len(stopped) > 0 {
			t.Logf("testbin: ended %d processes still running under the Oh My Pi HOME %s: %s", len(stopped), home, describe(stopped))
		}
		if err != nil {
			t.Error(err)
		}
	})
}

// stopUnder ends every process running under home, SIGTERM first and SIGKILL for one that has not
// gone within grace, and answers the processes it ended. It lists them again after each round,
// and fails when one is still running after SIGKILL, or when processes are still found after
// homeStopRounds rounds.
func stopUnder(home string, grace time.Duration) ([]homeProcess, error) {
	var stopped []homeProcess
	for range homeStopRounds {
		found, err := processesUnder(home)
		if err != nil {
			return stopped, err
		}
		if len(found) == 0 {
			return stopped, nil
		}
		stopped = append(stopped, found...)
		signal(found, syscall.SIGTERM)
		left := awaitEnd(found, grace)
		if len(left) == 0 {
			continue
		}
		signal(left, syscall.SIGKILL)
		if left = awaitEnd(left, grace); len(left) > 0 {
			return stopped, fmt.Errorf("testbin: %s still ran under %s %s after its SIGKILL", describe(left), home, grace)
		}
	}
	found, err := processesUnder(home)
	if err != nil {
		return stopped, err
	}
	if len(found) > 0 {
		return stopped, fmt.Errorf("testbin: processes still start under %s after %d rounds of ending them: %s", home, homeStopRounds, describe(found))
	}
	return stopped, nil
}

// processesUnder lists every running process whose environment holds HOME=home.
func processesUnder(home string) ([]homeProcess, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, fmt.Errorf("testbin: list the processes under %s: %w", home, err)
	}
	want := []byte("HOME=" + home)
	var found []homeProcess
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		environ, err := os.ReadFile("/proc/" + entry.Name() + "/environ")
		if err != nil || !holdsEntry(environ, want) {
			continue
		}
		ticks, running := startOf(pid)
		if !running {
			continue
		}
		command, _ := os.ReadFile("/proc/" + entry.Name() + "/cmdline")
		found = append(found, homeProcess{
			pid: pid, ticks: ticks,
			command: strings.TrimSpace(string(bytes.ReplaceAll(command, []byte{0}, []byte{' '}))),
		})
	}
	return found, nil
}

// holdsEntry is whether environ, a /proc/<pid>/environ block of NUL-ended entries, holds want.
func holdsEntry(environ, want []byte) bool {
	for len(environ) > 0 {
		entry, rest, _ := bytes.Cut(environ, []byte{0})
		if bytes.Equal(entry, want) {
			return true
		}
		environ = rest
	}
	return false
}

// startOf answers pid's start ticks, and whether it still runs. A zombie runs no code and writes
// nothing, so it has ended for this file's purposes whoever reaps it, and so has a process whose
// record cannot be read.
func startOf(pid int) (ticks uint64, running bool) {
	state, ticks, gone, err := ProcessStat(pid)
	if err != nil || gone || state == "Z" || state == "X" {
		return 0, false
	}
	return ticks, true
}

// ProcessStat decodes pid's record in /proc/<pid>/stat: its state (field 3) and its start ticks
// (field 22), which tell it from a later process given the same pid, or gone when no process has
// the pid. The command name, field 2, is in parentheses and may hold spaces and either
// parenthesis, so the fields are counted from after the last ")". It is the one decoder of that
// record the tests share (this file's cleanup, and internal/daemon's local runtime for the live
// proofs), and each caller keeps its own policy for a zombie and for a record it cannot read.
func ProcessStat(pid int) (state string, ticks uint64, gone bool, err error) {
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		if os.IsNotExist(err) {
			return "", 0, true, nil
		}
		return "", 0, false, err
	}
	end := bytes.LastIndexByte(stat, ')')
	if end < 0 {
		return "", 0, false, fmt.Errorf("malformed /proc/%d/stat line: %s", pid, strings.TrimSpace(string(stat)))
	}
	fields := strings.Fields(string(stat[end+1:]))
	const stateField, starttime = 0, 22 - 3
	if len(fields) <= starttime {
		return "", 0, false, fmt.Errorf("malformed /proc/%d/stat line (no starttime field)", pid)
	}
	if ticks, err = strconv.ParseUint(fields[starttime], 10, 64); err != nil {
		return "", 0, false, fmt.Errorf("malformed /proc/%d/stat starttime: %w", pid, err)
	}
	return fields[stateField], ticks, false, nil
}

// ended is whether p has gone: no process has its pid, or the one that has it started later.
func (p homeProcess) ended() bool {
	ticks, running := startOf(p.pid)
	return !running || ticks != p.ticks
}

// signal sends sig to each process that is still the one listed, read again just before. One
// the signal does not reach or does not end is still running when awaitEnd reads it.
func signal(procs []homeProcess, sig syscall.Signal) {
	for _, p := range procs {
		if !p.ended() {
			_ = syscall.Kill(p.pid, sig)
		}
	}
}

// awaitEnd waits up to within for every one of procs to have ended, and answers those that have
// not.
func awaitEnd(procs []homeProcess, within time.Duration) []homeProcess {
	deadline := time.Now().Add(within)
	for {
		var left []homeProcess
		for _, p := range procs {
			if !p.ended() {
				left = append(left, p)
			}
		}
		if len(left) == 0 || !time.Now().Before(deadline) {
			return left
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func describe(procs []homeProcess) string {
	parts := make([]string, len(procs))
	for i, p := range procs {
		parts[i] = p.String()
	}
	return strings.Join(parts, "; ")
}

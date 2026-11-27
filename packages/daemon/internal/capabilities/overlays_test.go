package capabilities

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/testexec"
	"github.com/sjawhar/legion/daemon/internal/procgroup"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"golang.org/x/sys/unix"
)

// OverlayEntries leaves out the empty entries Oh My Pi leaves out.
func TestOverlayEntriesSkipsEmptyEntries(t *testing.T) {
	for value, want := range map[string][]string{
		"":                  nil,
		"/a.yml":            {"/a.yml"},
		"/a.yml:":           {"/a.yml"},
		":/a.yml":           {"/a.yml"},
		"/a.yml::/b.yml":    {"/a.yml", "/b.yml"},
		"/a.yml:/b.yml:::/": {"/a.yml", "/b.yml", "/"},
	} {
		if got := OverlayEntries(value); !slices.Equal(got, want) {
			t.Errorf("OverlayEntries(%q) = %q, want %q", value, got, want)
		}
	}
}

// CheckOverlays refuses only an overlay a run of Oh My Pi exits non-zero on, or one it cannot read,
// naming the file and Oh My Pi's own error line however long the output before it; an answer it
// cannot read as JSON, on exit 0, is no refusal, since the overlays started.
func TestCheckOverlaysRefusesOnlyANonZeroExit(t *testing.T) {
	dir := t.TempDir()
	omp := filepath.Join(dir, "omp")
	script := "#!/bin/sh\nIFS=:\nfor f in $PI_CONFIG_FILES; do case \"$f\" in *bad*)\n" +
		"  echo '" + strings.Repeat("y", 500) + "' >&2; echo \"error: Failed to parse config overlay $f: SyntaxError\" >&2; exit 1;; esac; done\n" +
		"echo not-json\n"
	if err := os.WriteFile(omp, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	good, bad, missing := filepath.Join(dir, "good.yml"), filepath.Join(dir, "bad.yml"), filepath.Join(dir, "missing.yml")
	for _, file := range []string{good, bad} {
		if err := os.WriteFile(file, []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	img := Image{Launch: omp, Env: []string{"PATH=" + os.Getenv("PATH"), "PI_CONFIG_FILES=/elsewhere.yml"}, WorkDir: dir}
	for _, tc := range []struct {
		name          string
		overlays, own []string
		want          string
	}{
		{"a file that starts", []string{good}, []string{good}, ""},
		{"a missing file", []string{good, missing}, []string{missing}, missing + " cannot be read: stat " + missing + ": no such file or directory"},
		{"a file Oh My Pi refuses", []string{good, bad}, []string{bad}, "Oh My Pi refuses " + bad + ": error: Failed to parse config overlay " + bad + ": SyntaxError"},
		{"the list Oh My Pi refuses", []string{bad, good}, []string{good}, "Oh My Pi refuses PI_CONFIG_FILES " + bad + ":" + good + ": error: Failed to parse config overlay " + bad + ": SyntaxError"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := CheckOverlays(context.Background(), img, tc.overlays, tc.own)
			if err != nil || got != tc.want {
				t.Fatalf("CheckOverlays = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

// A run that never reaches an exit of its own (here a timeout) does not stop the check: it is
// returned as err, and a role file Oh My Pi refuses after it is still refused. Both files run
// under main's 300 ms commandTimeout. The slow file's run waits for a release, which the row writes
// stopBound, main's 5 s, after that run's Oh My Pi wrote its start mark, as main's `sleep 5` let it
// go then: a timeout that has not stopped it by then finds it exited 0 on the release, and the row
// fails for want of the slow file's timeout. The refused file's run answers at once, and must
// answer inside its 300 ms, as on main. Its Oh My Pi logs its overlays as its first act, once its
// SIGTERM trap is set. A call in which the refused file's run timed out before that first act is
// judged, and fails. A call in which it timed out after that first act is told apart by what reached
// the refused file's Oh My Pi:
//   - its trap recorded a SIGTERM: the timeout's stop reached a live Oh My Pi. Either a loaded
//     machine slowed its own run past its 300 ms, or the check held it there, as a check does that
//     leaves its output pipe unread until the timeout. The kernel tells the two apart. The refused
//     file's Oh My Pi reads its own /proc/<pid>/schedstat line just before its first act and again
//     first thing in its trap, each beside a mark of the time since boot (firstActToTrap), and it
//     forks nothing between the two. Over that span it ran, waited for a CPU (the line's run_delay),
//     or slept, and it has nothing of its own to sleep on there: it writes a few bytes to files and
//     to a pipe the check reads. A loaded machine leaves its hold as run-queue wait, and such a call
//     is not judged. A span in which it slept asleepBound or more past what the two marks can be off
//     by (clockSlack) was the check's hold, and the row fails it. A kernel that keeps no schedstat
//     (gVisor) cannot tell the two apart, and such a call is not judged either.
//   - it recorded none: Oh My Pi exited on its own, a refusal. A correct check reads that as its own
//     exit, with one exception: kill succeeds on a zombie, so a refusal whose reap os/exec took only
//     after the timeout passed is stopped "as a timeout" with no SIGTERM delivered, which looks exactly
//     like a check that misreads a refusal. The row orders that window from the run's own marks. The
//     refused file's run starts after the slow file's run returned, which is after the slow file's
//     Oh My Pi recorded its SIGTERM, so its timeout passes no sooner than that mark plus 300 ms. A
//     refusal that exited reapBound or more before that earliest moment would have needed os/exec to
//     reap it that late, and the row fails it as a refusal read as a timeout. One that exited closer
//     than that lies in the window no fact outside os/exec orders, and its exit is not judged.
//
// Either way, no correct check reports the refused file's timeout before the earliest moment it could
// pass, the slow file's SIGTERM mark plus 300 ms: only the end of the refused file's own budget makes
// its run time out, and that budget starts after the mark. The row reads the time since boot as soon
// as CheckOverlays returns, and a call that returned two ticks or more before that moment, more than
// the two readings together can be off by (testexec.ClockTick), read a refusal as a timeout and
// fails, however loaded the machine. Any other such call is not judged, and the row calls again,
// within a minute.
//
// A check that stops at the first timeout never runs the refused file at all, and fails the row at once.
func TestCheckOverlaysKeepsCheckingAfterATimeout(t *testing.T) {
	was := commandTimeout
	commandTimeout = 300 * time.Millisecond
	t.Cleanup(func() { commandTimeout = was })
	const stopBound, reapBound, asleepBound = 5 * time.Second, 200 * time.Millisecond, 100 * time.Millisecond
	// Each time since boot the row reads, its own and a stand-in's mark, is off by less than a tick.
	const clockSlack = 2 * testexec.ClockTick
	dir := t.TempDir()
	// CONTROL ONLY (r63 starve): a machine that keeps the refused Oh My Pi of call 1 waiting for a
	// CPU after its first act. ctlHogs busy loops run pinned to the last CPU this process may use;
	// once that Oh My Pi has written its pid, after its first act, the row pins it there too, and it
	// spins (CPU, no sleep) until it is pinned and for 20 ms of CPU more, so the hogs hold it there.
	const ctlHogs = 50
	var ctlSet unix.CPUSet
	if err := unix.SchedGetaffinity(0, &ctlSet); err != nil {
		t.Fatalf("CONTROL: read this process's CPUs: %v", err)
	}
	ctlCPU := -1
	for cpu := range 1024 {
		if ctlSet.IsSet(cpu) {
			ctlCPU = cpu
		}
	}
	for i := range ctlHogs {
		hog := exec.Command("taskset", "-c", strconv.Itoa(ctlCPU), "sh", "-c", "while :; do :; done")
		if err := hog.Start(); err != nil {
			t.Fatalf("CONTROL: start hog %d: %v", i, err)
		}
		t.Cleanup(func() { _ = hog.Process.Kill(); _ = hog.Wait() })
	}
	t.Logf("CONTROL: %d busy loops pinned to CPU %d", ctlHogs, ctlCPU)
	ctlStarve := "case \"$LEGION_TEST_RUN\" in */1) echo $$ >\"$LEGION_TEST_RUN/ctl.pid\"; while [ ! -e \"$LEGION_TEST_RUN/ctl.pinned\" ]; do :; done; " +
		"read -r cs </proc/self/schedstat; c0=${cs%% *}; while :; do read -r cs </proc/self/schedstat; [ $(( ${cs%% *} - c0 )) -ge 20000000 ] && break; done ;; esac; "
	omp := filepath.Join(dir, "omp")
	// Each call's files are in the directory LEGION_TEST_RUN names. The slow file's run waits until
	// the row releases it or this test process is gone, never on a timer of its own. The refused
	// file's run alone reads its schedstat line before its first mark's time and after its trap's,
	// so the span between the two counts no more sleep than there was (firstActToTrap).
	alone := "[ \"$PI_CONFIG_FILES\" = \"$LEGION_TEST_BAD\" ]"
	schedstat := `ss=; [ -r /proc/self/schedstat ] && read -r ss </proc/self/schedstat`
	script := "#!/bin/sh\n" +
		"onslowterm() { " + testexec.MarkNow(`"$LEGION_TEST_RUN/slow.termed"`) + "; exit 143; }\n" +
		"onbadterm() { if " + alone + "; then " + testexec.MarkNow(`"$LEGION_TEST_RUN/bad.trapped"`) + "; " + schedstat + "; printf '%s\\n' \"$ss\" >\"$LEGION_TEST_RUN/bad.trapped.waits\"; fi; " +
		"echo \"$PI_CONFIG_FILES\" >>\"$LEGION_TEST_RUN/bad.termed\"; exit 143; }\n" +
		"case \"$PI_CONFIG_FILES\" in\n" +
		"*slow.yml) trap onslowterm TERM; : >\"$LEGION_TEST_RUN/slow.started\"; while [ ! -e \"$LEGION_TEST_RUN/release\" ] && kill -0 " + strconv.Itoa(os.Getpid()) + " 2>/dev/null; do sleep 0.05; done ;;\n" +
		"*bad.yml) trap onbadterm TERM; if " + alone + "; then " + schedstat + "; printf '%s\\n' \"$ss\" >\"$LEGION_TEST_RUN/bad.first.waits\"; " + testexec.MarkNow(`"$LEGION_TEST_RUN/bad.first"`) + "; fi; " +
		"echo \"$PI_CONFIG_FILES\" >>\"$LEGION_TEST_RUN/bad.ran\"; if " + alone + "; then " + ctlStarve + "fi; echo \"error: Failed to parse config overlay $PI_CONFIG_FILES\" >&2; " +
		"if " + alone + "; then " + testexec.MarkNow(`"$LEGION_TEST_RUN/bad.exiting"`) + "; fi; exit 1 ;;\n" +
		"esac\necho '{\"value\": false}'\n"
	if err := testexec.WriteFile(omp, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	slow, bad := filepath.Join(dir, "slow.yml"), filepath.Join(dir, "bad.yml")
	for _, file := range []string{slow, bad} {
		if err := os.WriteFile(file, []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	within := testwait.Bounded(t, time.Minute, stopBound+2*commandTimeout+procgroup.StopGrace+procgroup.WaitDelay)
	until := time.Now().Add(within)
	for calls := 1; ; calls++ {
		run := filepath.Join(dir, strconv.Itoa(calls))
		if err := os.Mkdir(run, 0o755); err != nil {
			t.Fatal(err)
		}
		slowStarted, release, badRan := filepath.Join(run, "slow.started"), filepath.Join(run, "release"), filepath.Join(run, "bad.ran")
		t.Cleanup(func() {
			if err := os.WriteFile(release, nil, 0o600); err != nil {
				t.Errorf("release call %d's slow file: %v", calls, err)
			}
		})
		img := Image{Launch: omp, Env: []string{"PATH=" + os.Getenv("PATH"), "LEGION_TEST_RUN=" + run, "LEGION_TEST_BAD=" + bad}, WorkDir: dir}
		returned := make(chan struct{})
		go func() {
			var mark time.Time
			for mark.IsZero() {
				select {
				case <-returned:
					return
				case <-time.After(testwait.Poll):
				}
				if info, err := os.Stat(slowStarted); err == nil {
					mark = info.ModTime()
				}
			}
			select {
			case <-returned:
			case <-time.After(time.Until(mark.Add(stopBound))):
				_ = os.WriteFile(release, nil, 0o600)
			}
		}()
		if calls == 1 { // CONTROL ONLY: pin call 1's refused Oh My Pi to the hogs' CPU once it has written its pid
			go func() {
				pidFile, pinned := filepath.Join(run, "ctl.pid"), filepath.Join(run, "ctl.pinned")
				for {
					select {
					case <-returned:
						return
					case <-time.After(time.Millisecond):
					}
					raw, err := os.ReadFile(pidFile)
					if err != nil || !strings.HasSuffix(string(raw), "\n") {
						continue
					}
					out, err := exec.Command("taskset", "-p", "-c", strconv.Itoa(ctlCPU), strings.TrimSpace(string(raw))).CombinedOutput()
					t.Logf("CONTROL: pinned call 1's refused Oh My Pi (pid %s) to CPU %d: %v %s", strings.TrimSpace(string(raw)), ctlCPU, err, strings.TrimSpace(string(out)))
					_ = os.WriteFile(pinned, nil, 0o600)
					return
				}
			}()
		}

		got, err := CheckOverlays(context.Background(), img, []string{slow, bad}, []string{slow, bad})
		answered := testexec.SinceBoot(t)
		close(returned)

		// The refused file's own run started where its Oh My Pi logged that file alone.
		badStarted, badTermed := false, false
		if raw, readErr := os.ReadFile(badRan); readErr == nil {
			badStarted = slices.Contains(strings.Split(string(raw), "\n"), bad)
		}
		if raw, readErr := os.ReadFile(filepath.Join(run, "bad.termed")); readErr == nil {
			badTermed = slices.Contains(strings.Split(string(raw), "\n"), bad)
		}
		if err != nil && strings.Contains(err.Error(), "under PI_CONFIG_FILES "+bad+": timed out") {
			if !badStarted {
				t.Fatalf("call %d: the refused file's run timed out before its Oh My Pi's first act: it must answer inside its %s timeout (CheckOverlays = %q, %v)", calls, commandTimeout, got, err)
			}
			slowTermed, slowErr := testexec.ReadNowMark(filepath.Join(run, "slow.termed"))
			var why string
			if badTermed {
				span := readFirstActToTrap(t, run)
				if span.known && span.asleep() >= clockSlack+asleepBound {
					t.Fatalf("call %d: the refused file's run read as timed out, and its Oh My Pi recorded the timeout's SIGTERM, but over the %s from just before its first act to that SIGTERM it ran %s and waited %s for a CPU, so it slept %s with nothing of its own to wait on: the check held it past its %s timeout, which a loaded machine does only as run-queue wait (CheckOverlays = %q, %v)", calls, span.span, span.ran, span.waited, span.asleep(), commandTimeout, got, err)
				}
				why = span.String()
			} else {
				exited, exitErr := testexec.ReadNowMark(filepath.Join(run, "bad.exiting"))
				if slowErr != nil || exitErr != nil {
					t.Fatalf("call %d: the refused file's run read as timed out, its Oh My Pi passed its first act and recorded no SIGTERM, and the row cannot read the marks that order its exit against its timeout (slow file's SIGTERM mark: %v; refused file's exit mark: %v): CheckOverlays = %q, %v", calls, slowErr, exitErr, got, err)
				}
				earliest := slowTermed + commandTimeout
				if reap := earliest - exited; reap >= reapBound {
					t.Fatalf("call %d: the refused file's run read as timed out, but its Oh My Pi passed its first act, recorded no SIGTERM, and exited %s before the earliest moment its %s timeout could pass: the check read a refusal as a timeout (CheckOverlays = %q, %v)", calls, reap, commandTimeout, got, err)
				}
				why = fmt.Sprintf("its Oh My Pi recorded no SIGTERM and exited only %s before the earliest moment its timeout could pass, inside the window from a zombie's exit to os/exec's reap, which no fact outside os/exec orders", earliest-exited)
			}
			if early := slowTermed + commandTimeout - answered; slowErr == nil && early >= clockSlack {
				t.Fatalf("call %d: the refused file's run read as timed out, but CheckOverlays returned %s before the earliest moment its %s timeout could pass, the slow file's SIGTERM mark plus that timeout: no timeout had ended the run, so the check read a refusal as a timeout (CheckOverlays = %q, %v)", calls, early, commandTimeout, got, err)
			}
			if time.Now().Before(until) {
				t.Logf("call %d is not judged: %s (%v)", calls, why, err)
				continue
			}
			t.Fatalf("no call of %d over %s in which the refused file's run answered before its %s timeout: in each, %s: %v", calls, within, commandTimeout, why, err)
		}
		if !badStarted {
			t.Fatalf("call %d: the refused file's run never started: the check stopped at the slow file's timeout (CheckOverlays = %q, %v)", calls, got, err)
		}
		if want := "Oh My Pi refuses " + bad + ": error: Failed to parse config overlay " + bad; got != want {
			t.Errorf("call %d: CheckOverlays refused %q, want %q", calls, got, want)
		}
		if err == nil || !strings.Contains(err.Error(), "under PI_CONFIG_FILES "+slow+": timed out") {
			t.Errorf("call %d: CheckOverlays err = %v, want the slow file's timeout, which must stop its run within %s of its start, before the row releases it", calls, err, stopBound)
		}
		return
	}
}

// firstActToTrap is what the refused file's Oh My Pi recorded of itself from just before its first
// act to first thing in its SIGTERM trap: the span between the two moments it marked, and the CPU
// time it ran and the time the kernel kept it waiting for a CPU over that span, the differences of
// the first two fields of the /proc/<pid>/schedstat lines it read at the two (the kernel's
// Documentation/scheduler/sched-stats.rst: time on the CPU, then run_delay, in nanoseconds). known
// is false where this kernel keeps no such line, as gVisor's.
type firstActToTrap struct {
	span, ran, waited time.Duration
	known             bool
}

// asleep is the span less what the refused Oh My Pi ran and waited for a CPU over it: the time it
// slept, give or take what the two marks can be off by. It reads its first line before its first
// moment and its second after its trap's, so a wait or a run just outside the span is counted in it,
// which makes asleep less, never more, than the sleep in the span.
func (s firstActToTrap) asleep() time.Duration { return s.span - s.ran - s.waited }

// String says what the span shows of the hold that kept the refused Oh My Pi past its timeout.
func (s firstActToTrap) String() string {
	if !s.known {
		return "its Oh My Pi recorded the timeout's SIGTERM, and this kernel keeps no /proc/<pid>/schedstat to tell a loaded machine's hold of it from the check's"
	}
	return fmt.Sprintf("its Oh My Pi recorded the timeout's SIGTERM, and over the %s from just before its first act to it the kernel kept it waiting %s for a CPU and it ran %s, so it slept only %s: a loaded machine held its own run past its timeout", s.span, s.waited, s.ran, s.asleep())
}

// readFirstActToTrap reads, from the call's directory run, what the refused file's Oh My Pi
// recorded just before its first act (bad.first) and first thing in its trap (bad.trapped): each
// moment, and beside it (.waits) the schedstat line it read there, empty where it had none. It
// fails the row on a record it cannot read: the stand-in writes both before the lines that tell
// the row it passed its first act and took a SIGTERM.
func readFirstActToTrap(t *testing.T, run string) firstActToTrap {
	t.Helper()
	read := func(name string) (time.Duration, []string) {
		at, err := testexec.ReadNowMark(filepath.Join(run, name))
		if err != nil {
			t.Fatalf("read the moment the refused file's Oh My Pi recorded at %s: %v", name, err)
		}
		raw, err := os.ReadFile(filepath.Join(run, name+".waits"))
		if err != nil {
			t.Fatalf("read the schedstat line the refused file's Oh My Pi recorded at %s: %v", name, err)
		}
		return at, strings.Fields(string(raw))
	}
	first, before := read("bad.first")
	trapped, after := read("bad.trapped")
	s := firstActToTrap{span: trapped - first}
	if len(before) < 2 || len(after) < 2 {
		return s
	}
	field := func(fields []string, i int) time.Duration {
		ns, err := strconv.ParseInt(fields[i], 10, 64)
		if err != nil || ns < 0 {
			t.Fatalf("field %d of the refused file's Oh My Pi's schedstat line %q: %v", i+1, strings.Join(fields, " "), err)
		}
		return time.Duration(ns)
	}
	s.ran, s.waited, s.known = field(after, 0)-field(before, 0), field(after, 1)-field(before, 1), true
	return s
}

package capabilities

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/testexec"
	"github.com/sjawhar/legion/daemon/internal/procgroup"
	"github.com/sjawhar/legion/daemon/internal/testwait"
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
//   - its trap recorded a SIGTERM: the timeout's stop reached a live Oh My Pi, whose own run a loaded
//     machine slowed past its 300 ms. That call is not judged, and the row calls again, within a minute.
//   - it recorded none: Oh My Pi exited on its own, a refusal. A correct check reads that as its own
//     exit, with one exception: kill succeeds on a zombie, so a refusal whose reap os/exec took only
//     after the timeout passed is stopped "as a timeout" with no SIGTERM delivered, which looks exactly
//     like a check that misreads a refusal. The row orders that window from the run's own marks. The
//     refused file's run starts after the slow file's run returned, which is after the slow file's
//     Oh My Pi recorded its SIGTERM, so its timeout passes no sooner than that mark plus 300 ms. A
//     refusal that exited reapBound or more before that earliest moment would have needed os/exec to
//     reap it that late, and the row fails it as a refusal read as a timeout. One that exited closer
//     than that lies in the window no fact outside os/exec orders, and is not judged.
//
// A check that stops at the first timeout never runs the refused file at all, and fails the row at once.
func TestCheckOverlaysKeepsCheckingAfterATimeout(t *testing.T) {
	was := commandTimeout
	commandTimeout = 300 * time.Millisecond
	t.Cleanup(func() { commandTimeout = was })
	const stopBound, reapBound = 5 * time.Second, 200 * time.Millisecond
	dir := t.TempDir()
	omp := filepath.Join(dir, "omp")
	// Each call's files are in the directory LEGION_TEST_RUN names. The slow file's run waits until
	// the row releases it or this test process is gone, never on a timer of its own.
	script := "#!/bin/sh\n" +
		"onslowterm() { " + testexec.MarkNow(`"$LEGION_TEST_RUN/slow.termed"`) + "; exit 143; }\n" +
		"onbadterm() { echo \"$PI_CONFIG_FILES\" >>\"$LEGION_TEST_RUN/bad.termed\"; exit 143; }\n" +
		"case \"$PI_CONFIG_FILES\" in\n" +
		"*slow.yml) trap onslowterm TERM; : >\"$LEGION_TEST_RUN/slow.started\"; while [ ! -e \"$LEGION_TEST_RUN/release\" ] && kill -0 " + strconv.Itoa(os.Getpid()) + " 2>/dev/null; do sleep 0.05; done ;;\n" +
		"*bad.yml) trap onbadterm TERM; echo \"$PI_CONFIG_FILES\" >>\"$LEGION_TEST_RUN/bad.ran\"; echo \"error: Failed to parse config overlay $PI_CONFIG_FILES\" >&2; " +
		"if [ \"$PI_CONFIG_FILES\" = \"$LEGION_TEST_BAD\" ]; then " + testexec.MarkNow(`"$LEGION_TEST_RUN/bad.exiting"`) + "; fi; exit 1 ;;\n" +
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

		got, err := CheckOverlays(context.Background(), img, []string{slow, bad}, []string{slow, bad})
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
			why := "its Oh My Pi recorded the timeout's SIGTERM: the stop reached a live Oh My Pi whose own run outlasted its timeout"
			if !badTermed {
				slowTermed, slowErr := testexec.ReadNowMark(filepath.Join(run, "slow.termed"))
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

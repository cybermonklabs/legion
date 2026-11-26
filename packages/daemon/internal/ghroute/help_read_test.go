package ghroute

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/ghroute/ghroutetest"
	"github.com/sjawhar/legion/daemon/internal/runtime/shellprefix"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// A help read that outlives its timeout stops everything gh started, not gh alone. A timeout that
// killed gh alone, as exec.CommandContext's own does, left what gh started running, reparented and
// waited for by nobody; under a gh that led back to `legion gh`, every level of the chain was such
// a helper.
//
// The row reads that in three reads, so that no verdict of it is a race with the clock. In the
// first, gh never answers, and the read's deadline passes once gh has recorded its pid: ReadHelp
// gives up naming the timeout and the stop of gh's process group, which it says only where
// procgroup's stop ended the read. The deadline is passed by hand (testwait.PassedDeadline), since a
// timeout of a second could pass before a loaded machine started gh, which is the second read's
// case: under a timeout that has already passed, gh never starts and the read says so. In the
// third, gh starts a helper of its own, detached from gh's output so nothing holds the read's
// pipes, and never answers, and the read's context ends once gh has recorded that helper:
// ReadHelp gives up naming the stop of gh's group, and the helper is gone too. exec runs the one
// stop procgroup gives the read whichever end of its context comes, the timeout's or this one. Each
// wait has the test binary's deadline as its only bound (testwait.Remaining), and so has the read's
// own timeout (TestMain), and each stand-in sleeps about a day, so a stop that never comes fails
// the row by name and no sleep ends by itself before it.
func TestAHelpReadThatTimesOutStopsWhatGhStarted(t *testing.T) {
	if _, err := os.ReadFile("/proc/self/cmdline"); err != nil {
		t.Fatalf("/proc does not answer (%v): this row reads from it whether gh's helper is still running", err)
	}
	// read starts ReadHelp of gh's `pr` help under parent and answers the channel its error comes
	// on and the cancel of its context. The end of t cancels that context and waits for the read
	// before the cleanups registered earlier run, so a read the row gave up on is stopped, and never
	// reads helpTimeout while a cleanup puts it back.
	read := func(t *testing.T, parent context.Context, gh string) (<-chan error, context.CancelFunc) {
		ctx, cancel := context.WithCancel(parent)
		answered, ended := make(chan error, 1), make(chan struct{})
		go func() {
			defer close(ended)
			_, err := ReadHelp(ctx, gh, os.Environ(), []string{"pr"})
			answered <- err
		}()
		t.Cleanup(func() {
			cancel()
			<-ended
		})
		return answered, cancel
	}
	t.Run("a gh that never answers is stopped at the timeout", func(t *testing.T) {
		dir := t.TempDir()
		gh, ghPid := filepath.Join(dir, "gh"), filepath.Join(dir, "gh.pid")
		const ghSeconds = "86398"
		if err := os.WriteFile(gh, []byte("#!/bin/sh\n"+ghroutetest.HoldsStandIn(ghPid, ghSeconds)), 0o700); err != nil {
			t.Fatal(err)
		}
		ghroutetest.KillRecordedHelper(t, ghPid, ghSeconds)
		deadline := testwait.NewPassedDeadline()
		answered, _ := read(t, deadline, gh)
		testwait.WithinPolling(t, testwait.Remaining(t), testwait.Poll, "gh to start", func() bool {
			_, running := ghroutetest.RecordedHelper(ghPid, ghSeconds)
			return running
		})
		deadline.Pass()
		err := testwait.Receive(t, "the help read of a gh that never answers to end once its deadline passed", answered)
		if err == nil || !helpTimedOutStopped.MatchString(err.Error()) {
			t.Fatalf("ReadHelp of a gh that never answers, its deadline passed = %v; want it to match %q", err, helpTimedOutStopped)
		}
	})
	t.Run("a timeout that passed before gh started is said as the timeout", func(t *testing.T) {
		dir := t.TempDir()
		gh, ran := filepath.Join(dir, "gh"), filepath.Join(dir, "ran")
		if err := os.WriteFile(gh, []byte("#!/bin/sh\n: >'"+ran+"'\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		timeout := helpTimeout
		helpTimeout = 0
		t.Cleanup(func() { helpTimeout = timeout })
		answered, _ := read(t, context.Background(), gh)
		err := testwait.Receive(t, "the help read whose timeout has passed to end", answered)
		if want := "gh did not answer its help within 0s: the time passed before gh started"; err == nil || err.Error() != want {
			t.Fatalf("ReadHelp under a timeout that passed before gh started = %v; want %q", err, want)
		}
		if _, err := os.Stat(ran); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("gh ran (%v) under a timeout that had passed", err)
		}
	})
	t.Run("the stop reaches the helper gh started", func(t *testing.T) {
		dir := t.TempDir()
		gh, ghPid, helperPid := filepath.Join(dir, "gh"), filepath.Join(dir, "gh.pid"), filepath.Join(dir, "helper.pid")
		const ghSeconds, helperSeconds = "86400", "86399"
		script := "#!/bin/sh\nsleep " + helperSeconds + " </dev/null >/dev/null 2>&1 &\necho $! >" + shellprefix.Literal(helperPid) + "\n" + ghroutetest.HoldsStandIn(ghPid, ghSeconds)
		if err := os.WriteFile(gh, []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
		ghroutetest.KillRecordedHelper(t, helperPid, helperSeconds)
		ghroutetest.KillRecordedHelper(t, ghPid, ghSeconds)
		answered, stop := read(t, context.Background(), gh)
		var pid int
		testwait.WithinPolling(t, testwait.Remaining(t), testwait.Poll, "gh to record the helper it starts", func() bool {
			var running bool
			pid, running = ghroutetest.RecordedHelper(helperPid, helperSeconds)
			return running
		})
		stop()
		err := testwait.Receive(t, "the help read to end once its context ended", answered)
		if want := "gh's help read was stopped, with its process group: context canceled"; err == nil || err.Error() != want {
			t.Fatalf("ReadHelp of a gh that never answers, its context ended = %v; want %q", err, want)
		}
		// The group's stop is SIGTERM, then SIGKILL after procgroup.StopGrace; the helper, a
		// sleep, ends on the SIGTERM.
		testwait.WithinPolling(t, testwait.Remaining(t), testwait.Poll,
			fmt.Sprintf("gh's helper (pid %d) to end once the help read was stopped: want the stop to reach gh's whole process group", pid),
			func() bool { return !ghroutetest.HelperRunning(pid, helperSeconds) })
	})
}

// helpTimedOutStopped is how ReadHelp says a read whose deadline passed while gh ran: the timeout,
// whatever bound the read took (programbound), and the stop of gh's process group.
var helpTimedOutStopped = regexp.MustCompile(`^gh did not answer its help within \S+, and its process group was stopped$`)

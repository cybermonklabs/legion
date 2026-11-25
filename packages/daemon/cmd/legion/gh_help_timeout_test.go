package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"syscall"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/leakguard/leakguardtest"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// A help read that outlives its timeout stops everything gh started, not gh alone. A timeout that
// killed gh alone, as exec.CommandContext's own does, left what gh started running, reparented and
// waited for by nobody; under a gh that led back to `legion gh`, every level of the chain was such
// a helper.
//
// The row reads that in three reads, so that no verdict of it is a race with the clock. In the
// first, gh never answers, and the read's deadline passes once gh has recorded its pid: readGhHelp
// gives up naming the timeout and the stop of gh's process group, which it says only where
// procgroup's stop ended the read. The deadline is passed by hand (passedDeadline), since a
// timeout of a second could pass before a loaded machine started gh, which is the second read's
// case: under a timeout that has already passed, gh never starts and the read says so. In the
// third, gh starts a helper of its own, detached from gh's output so nothing holds the read's
// pipes, and never answers, and the read's context ends once gh has recorded that helper:
// readGhHelp gives up naming the stop of gh's group, and the helper is gone too. exec runs the one
// stop procgroup gives the read whichever end of its context comes, the timeout's or this one. Each
// wait has the test binary's deadline as its only bound, and each stand-in sleeps about a day, so a
// stop that never comes fails the row by name and no sleep ends by itself before it.
func TestAHelpReadThatTimesOutStopsWhatGhStarted(t *testing.T) {
	if _, err := os.ReadFile("/proc/self/cmdline"); err != nil {
		t.Fatalf("/proc does not answer (%v): this row reads from it whether gh's helper is still running", err)
	}
	// read starts readGhHelp of gh's `pr` help under parent and answers the channel its error comes
	// on and the cancel of its context. The end of t cancels that context and waits for the read
	// before the cleanups registered earlier run, so a read the row gave up on is stopped, and never
	// reads ghHelpTimeout while a cleanup puts it back.
	read := func(t *testing.T, parent context.Context, gh string) (<-chan error, context.CancelFunc) {
		ctx, cancel := context.WithCancel(parent)
		answered, ended := make(chan error, 1), make(chan struct{})
		go func() {
			defer close(ended)
			_, err := readGhHelp(ctx, gh, os.Environ(), []string{"pr"})
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
		if err := os.WriteFile(gh, []byte("#!/bin/sh\n"+holdsStandIn(ghPid, ghSeconds)), 0o700); err != nil {
			t.Fatal(err)
		}
		killRecordedHelper(t, ghPid, ghSeconds)
		deadline := newPassedDeadline()
		answered, _ := read(t, deadline, gh)
		testwait.WithinPolling(t, testwait.Bounded(t, 1<<60, testBoundMargin), testwait.Poll, "gh to start", func() bool {
			_, running := recordedHelper(ghPid, ghSeconds)
			return running
		})
		deadline.pass()
		err := testwait.Receive(t, "the help read of a gh that never answers to end once its deadline passed", answered)
		if err == nil || !helpTimedOutStopped.MatchString(err.Error()) {
			t.Fatalf("readGhHelp of a gh that never answers, its deadline passed = %v; want it to match %q", err, helpTimedOutStopped)
		}
	})
	t.Run("a timeout that passed before gh started is said as the timeout", func(t *testing.T) {
		dir := t.TempDir()
		gh, ran := filepath.Join(dir, "gh"), filepath.Join(dir, "ran")
		if err := os.WriteFile(gh, []byte("#!/bin/sh\n: >'"+ran+"'\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		timeout := ghHelpTimeout
		ghHelpTimeout = 0
		t.Cleanup(func() { ghHelpTimeout = timeout })
		answered, _ := read(t, context.Background(), gh)
		err := testwait.Receive(t, "the help read whose timeout has passed to end", answered)
		if want := "gh did not answer its help within 0s: the time passed before gh started"; err == nil || err.Error() != want {
			t.Fatalf("readGhHelp under a timeout that passed before gh started = %v; want %q", err, want)
		}
		if _, err := os.Stat(ran); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("gh ran (%v) under a timeout that had passed", err)
		}
	})
	t.Run("the stop reaches the helper gh started", func(t *testing.T) {
		dir := t.TempDir()
		gh, ghPid, helperPid := filepath.Join(dir, "gh"), filepath.Join(dir, "gh.pid"), filepath.Join(dir, "helper.pid")
		const ghSeconds, helperSeconds = "86400", "86399"
		script := "#!/bin/sh\nsleep " + helperSeconds + " </dev/null >/dev/null 2>&1 &\necho $! >'" + helperPid + "'\n" + holdsStandIn(ghPid, ghSeconds)
		if err := os.WriteFile(gh, []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
		killRecordedHelper(t, helperPid, helperSeconds)
		killRecordedHelper(t, ghPid, ghSeconds)
		answered, stop := read(t, context.Background(), gh)
		var pid int
		testwait.WithinPolling(t, testwait.Bounded(t, 1<<60, testBoundMargin), testwait.Poll, "gh to record the helper it starts", func() bool {
			var running bool
			pid, running = recordedHelper(helperPid, helperSeconds)
			return running
		})
		stop()
		err := testwait.Receive(t, "the help read to end once its context ended", answered)
		if want := "gh's help read was stopped, with its process group: context canceled"; err == nil || err.Error() != want {
			t.Fatalf("readGhHelp of a gh that never answers, its context ended = %v; want %q", err, want)
		}
		// The group's stop is SIGTERM, then SIGKILL after procgroup.StopGrace; the helper, a
		// sleep, ends on the SIGTERM.
		testwait.WithinPolling(t, testwait.Bounded(t, 1<<60, testBoundMargin), testwait.Poll,
			fmt.Sprintf("gh's helper (pid %d) to end once the help read was stopped: want the stop to reach gh's whole process group", pid),
			func() bool { return !helperRunning(pid, helperSeconds) })
	})
}

// helpTimedOutStopped is how readGhHelp says a read whose deadline passed while gh ran: the
// timeout, whatever bound the read took (programbound), and the stop of gh's process group.
var helpTimedOutStopped = regexp.MustCompile(`^gh did not answer its help within \S+, and its process group was stopped$`)

// passedDeadline is a context whose deadline a test passes by hand, once what it waits for has
// happened: Done closes, and Err answers context.DeadlineExceeded, from pass on. A context derived
// from it ends then with that error, as one whose deadline passed does.
type passedDeadline struct {
	context.Context
	done chan struct{}
	once sync.Once
}

func newPassedDeadline() *passedDeadline {
	return &passedDeadline{Context: context.Background(), done: make(chan struct{})}
}

func (d *passedDeadline) pass() { d.once.Do(func() { close(d.done) }) }

func (d *passedDeadline) Done() <-chan struct{} { return d.done }

func (d *passedDeadline) Err() error {
	select {
	case <-d.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

// A signal the terminal sends `legion gh`'s process group stops everything `legion gh` started
// before gh too, though each of those runs in a process group of its own, which no such signal
// reaches: the resolver's help read, the shared check the leak guard runs, and the guard's read of
// the destination's visibility, which goes back through `legion gh` to the real gh. `legion gh pr
// create -R acme/widgets` is started here as the leader of a group of its own, and in each row one
// stand-in holds that work (signalWhileHeld).
//
// A SIGINT to the group, a Ctrl-C, cancels the command's context (main's signal.NotifyContext),
// which stops the held work's group, and `legion gh` ends with 130. A SIGHUP
// to the group, a pane that closes, is caught until gh would start (hangupCancels) and stops the
// held work's group the same way, and `legion gh` ends with 129.
//
// Every row runs with the role's state directory set, so `legion gh` keeps the helps gh prints
// (ghHelpKeep). The help read's stand-in prints gh's whole help before it holds the read, and the
// read the signal stopped leaves no copy: a stopped read is an error, whatever gh printed before
// the stop, so it is never kept and no later call is answered with it.
//
// On main the help read shared `legion gh`'s own process group, so the group's signal reached the
// stand-in itself: a SIGINT ended it and `legion gh` refused the command with a help error, exit 2,
// and a SIGHUP ended both. The guard's children already ran in groups of their own: a SIGINT
// stopped one and `legion gh` answered with the guard's setup error, exit 2, and a SIGHUP killed
// `legion gh` where it stood and left the child running, reparented and waited for by nobody. The
// group of its own this branch gives the help read, so that a timed-out read stops whole
// (TestAHelpReadThatTimesOutStopsWhatGhStarted), would put the help read in that last case too
// without the handling these rows read.
func TestASignalToLegionGhsGroupStopsWhatItStartedBeforeGh(t *testing.T) {
	if _, err := os.ReadFile("/proc/self/cmdline"); err != nil {
		t.Fatalf("/proc does not answer (%v): this row reads from it whether the stand-in is still running", err)
	}
	root, defaultDir := gitCredentialBundle(t)
	help, err := filepath.Abs(ghHelpDir)
	if err != nil {
		t.Fatal(err)
	}
	passes := func(string, string) string { return leakguardtest.Passes }
	for _, held := range []struct {
		name string
		// seconds is the holding stand-in's sleep, one per row (holdsStandIn).
		seconds string
		// gh and check are the stand-in gh and the stand-in shared check, given the file the
		// holding one records its pid in and its sleep.
		gh, check func(pidFile, seconds string) string
		// keepsNothing is whether the one help the call reads is the one held, so that no copy may
		// be kept.
		keepsNothing bool
	}{
		{
			name: "the help read", seconds: "86397", check: passes, keepsNothing: true,
			gh: func(pidFile, seconds string) string {
				return "#!/bin/sh\nfor last; do :; done\nif [ \"$last\" = --help ]; then\n  cat '" + filepath.Join(help, "gh.txt") + "'\n" +
					holdsStandIn(pidFile, seconds) + "fi\nexit 0\n"
			},
		},
		{
			name: "the shared check", seconds: "86396", check: holdsStandIn,
			gh: func(string, string) string { return ghStandInPrefix(help) + "exit 0\n" },
		},
		{
			// The stand-in gh answers the outer `legion gh`'s help reads, and holds the guard's
			// read, which reads no help of its own. It tells that read by its argv
			// (ghVisibilityReadArgv): gh is handed no Legion variable naming it (ghChildEnviron).
			name: "the guard's visibility read", seconds: "86395", check: passes,
			gh: func(pidFile, seconds string) string {
				return ghStandInHelp(help) +
					"case \" $* \" in\n" + ghVisibilityReadArgv + ")\n" + holdsStandIn(pidFile, seconds) + ";;\nesac\nexit 0\n"
			},
		},
	} {
		for _, tc := range []struct {
			name   string
			signal syscall.Signal
		}{
			{"SIGINT", syscall.SIGINT},
			{"SIGHUP", syscall.SIGHUP},
		} {
			t.Run(held.name+" "+tc.name, func(t *testing.T) {
				dir := t.TempDir()
				pidFile := filepath.Join(dir, "stand-in")
				stub := filepath.Join(dir, "gh")
				if err := os.WriteFile(stub, []byte(held.gh(pidFile, held.seconds)), 0o700); err != nil {
					t.Fatal(err)
				}
				state := t.TempDir()
				command := legionGhCommand(t, t.TempDir(), map[string]string{
					"LEGION_GH_PATH": stub, "GH_CONFIG_DIR": defaultDir, "LEGION_GH_OWNERS_DIR": root,
					"PUBLIC_LEAK_CHECK": leakguardtest.Write(t, t.TempDir(), held.check(pidFile, held.seconds)),
					"XDG_STATE_HOME":    state,
				}, "pr", "create", "-R", "acme/widgets", "--title", "a title", "--body", "a body")
				signalWhileHeld(t, command, "legion gh", pidFile, held.seconds, held.name, tc.signal)
				kept, err := os.ReadDir(filepath.Join(state, "legion-gh", "help"))
				if err != nil && !errors.Is(err, fs.ErrNotExist) {
					t.Fatal(err)
				}
				if held.keepsNothing && len(kept) > 0 {
					t.Errorf("legion gh kept %d help copies after a %s stopped its only help read; want none", len(kept), tc.name)
				}
			})
		}
	}
}

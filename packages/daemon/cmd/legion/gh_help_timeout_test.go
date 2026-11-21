package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/leakguard/leakguardtest"
)

// A signal the terminal sends `legion gh`'s process group stops everything `legion gh` started
// before gh too, though each of those runs in a process group of its own, which no such signal
// reaches: the resolver's help read, the shared check the leak guard runs, and the guard's read of
// the destination's visibility, which goes back through `legion gh` to the real gh. `legion gh pr
// create -R acme/widgets` is started here as the leader of a group of its own, and in each row one
// stand-in holds that work (signalWhileHeld).
//
// A SIGINT to the group, a Ctrl-C, cancels the command's context (main's signal.NotifyContext),
// which stops the held work's group, and `legion gh` ends with 130 within a few seconds. A SIGHUP
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
// (ghroute's TestAHelpReadThatTimesOutStopsWhatGhStarted), would put the help read in that last
// case too without the handling these rows read.
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
		// seconds is the holding stand-in's sleep, one per row.
		seconds string
		// gh and check are the stand-in gh and the stand-in shared check, given the file the
		// holding one records its pid in and its sleep.
		gh, check func(pidFile, seconds string) string
		// keepsNothing is whether the one help the call reads is the one held, so that no copy may
		// be kept.
		keepsNothing bool
	}{
		{
			name: "the help read", seconds: "297", check: passes, keepsNothing: true,
			gh: func(pidFile, seconds string) string {
				return "#!/bin/sh\nfor last; do :; done\nif [ \"$last\" = --help ]; then\n  cat '" + filepath.Join(help, "gh.txt") + "'\n" +
					holdsStandIn(pidFile, seconds) + "fi\nexit 0\n"
			},
		},
		{
			name: "the shared check", seconds: "296", check: holdsStandIn,
			gh: func(string, string) string { return ghStandInPrefix(help) + "exit 0\n" },
		},
		{
			// The stand-in gh answers the outer `legion gh`'s help reads, and holds the guard's
			// read, which reads no help of its own. It tells that read by its argv
			// (ghVisibilityReadArgv): gh is handed no Legion variable naming it (ghChildEnviron).
			name: "the guard's visibility read", seconds: "295", check: passes,
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

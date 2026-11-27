package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/testnkeys"
)

// A file that names no NATS has `legion config check` print, after Config OK, the line a
// booting daemon logs for it (daemon.NoNATSLine): such a daemon boots and serves, and launches no
// worker, since it has no task bucket to hand one its task through. A file that names NATS prints
// no such line. The check exits 0 either way: the line is a report, and nothing is refused.
func TestCheckConfigSaysNoNATSLaunchesNoWorker(t *testing.T) {
	// Not parallel: its rows set environment variables of the whole process (legionState, projectsConfig).
	for _, row := range []struct {
		name     string
		projects string
		want     string
	}{
		{"a file that names no NATS", "projects:\n  DEMO: {}\n", demoCheckConfigOKNoNATS + tmuxCapabilityGaps + tmuxPauseUnavailable},
		{"a file that names NATS", "nats_urls: [nats://127.0.0.1:4222]\nprojects:\n  DEMO: {}\n", demoCheckConfigOK + tmuxCapabilityGaps + tmuxPauseUnavailable},
	} {
		t.Run(row.name, func(t *testing.T) {
			legionState(t)
			config := projectsConfig(t, row.projects)

			var out, errb bytes.Buffer
			code := run(context.Background(), []string{"legion", "config", "check", "--config", config}, &out, &errb)

			if code != 0 || out.String() != row.want || errb.Len() != 0 {
				t.Fatalf("exit code = %d, stdout %q, stderr %q; want 0 and %q", code, out.String(), errb.String(), row.want)
			}
		})
	}
}

// paneSeedLine is the line `legion config check` prints after Config OK for a file whose
// daemon would connect to NATS as the pane seed's user, written out here for the reason noNATSLine
// is: what an operator reads is the point of the line.
const paneSeedLine = "nats: the daemon connects as the pane seed's user (NATS_NKEY_SEED_FILE), the session user every pane shares, " +
	"which holds no grant on the task bucket: give the daemon a user of its own (nats_daemon_nkey_seed_file); " +
	"until it has one, its boot is refused at its open of the task bucket (legion_tasks)\n"

// A daemon that connects to NATS as the pane seed's user — the pane seed alone, or a daemon seed
// that is the pane seed — has its boot refused at its open of the task bucket, since the user
// every pane shares holds no grant on it. That is known offline, so `legion config check`
// prints the boot's own sentence after its OK line, as it prints the no-NATS line, and exits 0. A
// daemon seed of its own prints no such line, and neither does a file that names no NATS, whose
// daemon connects to none and gets the no-NATS line instead.
//
// The negative control is the check without the line (main's checkConfig printing nothing
// of CheckStart's paneSeed): the first two rows then read Config OK straight into the
// production-check line.
func TestCheckConfigSaysADaemonOnThePaneSeedIsRefusedItsBoot(t *testing.T) {
	// Not parallel: its rows set environment variables of the whole process (legionState, projectsConfig and the two seeds).
	paneSeed, paneUser := testnkeys.User(t)
	daemonSeed, daemonUser := testnkeys.User(t)
	const withNATS, withoutNATS = "nats_urls: [nats://127.0.0.1:4222]\nprojects:\n  DEMO: {}\n", "projects:\n  DEMO: {}\n"
	ok := "Config OK: project=DEMO design-gate-steps=spec nats-nkey-user=" + paneUser
	for _, row := range []struct {
		name, projects, daemonSeed, want string
	}{
		{"the pane seed alone", withNATS, "", ok + "\n" + paneSeedLine + demoProjectLines},
		{"a daemon seed that is the pane seed", withNATS, paneSeed, ok + " nats-daemon-nkey-user=" + paneUser + "\n" + paneSeedLine + demoProjectLines},
		{"a daemon seed of its own", withNATS, daemonSeed, ok + " nats-daemon-nkey-user=" + daemonUser + "\n" + demoProjectLines},
		{"the pane seed and no NATS", withoutNATS, "", ok + "\n" + noNATSLine + demoProjectLines},
	} {
		t.Run(row.name, func(t *testing.T) {
			legionState(t)
			config := projectsConfig(t, row.projects)
			t.Setenv("NATS_NKEY_SEED", paneSeed)
			if row.daemonSeed != "" {
				t.Setenv("NATS_DAEMON_NKEY_SEED", row.daemonSeed)
			}

			var out, errb bytes.Buffer
			code := run(context.Background(), []string{"legion", "config", "check", "--config", config}, &out, &errb)

			if printed := out.String() + errb.String(); strings.Contains(printed, paneSeed) || strings.Contains(printed, daemonSeed) {
				t.Fatalf("the check printed a seed: stdout %q stderr %q", out.String(), errb.String())
			}
			if want := row.want + tmuxCapabilityGaps + tmuxPauseUnavailable; code != 0 || out.String() != want || errb.Len() != 0 {
				t.Fatalf("exit code = %d, stdout %q, stderr %q; want 0 and %q", code, out.String(), errb.String(), want)
			}
		})
	}
}

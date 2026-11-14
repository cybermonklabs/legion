package main

import (
	"bytes"
	"context"
	"testing"
)

// A file that names no NATS has `legion start --check-config` print, after Config OK, the line a
// booting daemon logs for it (daemon.NoNATSLine): such a daemon boots and serves, and launches no
// worker, since it has no task bucket to hand one its task through. A file that names NATS prints
// no such line. The check exits 0 either way: the line is a report, and nothing is refused.
func TestCheckConfigSaysNoNATSLaunchesNoWorker(t *testing.T) {
	for _, row := range []struct {
		name     string
		projects string
		want     string
	}{
		{"a file that names no NATS", "projects:\n  DEMO: {}\n", demoCheckConfigOKNoNATS + tmuxCapabilityGaps},
		{"a file that names NATS", "nats_urls: [nats://127.0.0.1:4222]\nprojects:\n  DEMO: {}\n", demoCheckConfigOK + tmuxCapabilityGaps},
	} {
		t.Run(row.name, func(t *testing.T) {
			legionState(t)
			config := projectsConfig(t, row.projects)

			var out, errb bytes.Buffer
			code := run(context.Background(), []string{"legion", "start", "--config", config, "--check-config"}, &out, &errb)

			if code != 0 || out.String() != row.want || errb.Len() != 0 {
				t.Fatalf("exit code = %d, stdout %q, stderr %q; want 0 and %q", code, out.String(), errb.String(), row.want)
			}
		})
	}
}

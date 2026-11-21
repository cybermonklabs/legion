package ghroute

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/ghroute/ghroutetest"
	"github.com/sjawhar/legion/daemon/internal/procgroup"
)

// A help read that outlives its timeout stops everything gh started, not gh alone. The gh here
// starts a helper of its own, detached from gh's output so nothing holds the read's pipes, and
// then never answers. Once ReadHelp has given up, the helper is gone too. A timeout that killed
// gh alone, as exec.CommandContext's own does, left that helper running, reparented and waited
// for by nobody; under a gh that led back to `legion gh`, every level of the chain was such a
// helper.
func TestAHelpReadThatTimesOutStopsWhatGhStarted(t *testing.T) {
	if _, err := os.ReadFile("/proc/self/cmdline"); err != nil {
		t.Fatalf("/proc does not answer (%v): this row reads from it whether gh's helper is still running", err)
	}
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "helper")
	const helperSeconds = "299"
	gh := filepath.Join(dir, "gh")
	script := "#!/bin/sh\nsleep " + helperSeconds + " </dev/null >/dev/null 2>&1 &\necho $! >'" + pidFile + "'\nexec sleep 300\n"
	if err := os.WriteFile(gh, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	timeout := helpTimeout
	helpTimeout = time.Second
	t.Cleanup(func() { helpTimeout = timeout })
	ghroutetest.KillRecordedHelper(t, pidFile, helperSeconds)

	_, err := ReadHelp(context.Background(), gh, os.Environ(), []string{"pr"})
	if err == nil || !strings.Contains(err.Error(), "did not answer its help within") {
		t.Fatalf("ReadHelp of a gh that never answers = %v; want it to give up naming the timeout", err)
	}
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("gh recorded no helper: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("gh recorded helper pid %q: %v", raw, err)
	}
	// The group's stop is SIGTERM, then SIGKILL after procgroup.StopGrace; the helper, a sleep,
	// ends on the SIGTERM, and the bound leaves room for a loaded machine to reap it.
	for deadline := time.Now().Add(procgroup.StopGrace + 5*time.Second); ghroutetest.HelperRunning(pid, helperSeconds); time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("gh's helper (pid %d) is still running after the help read gave up; want the read's timeout to stop gh's whole process group", pid)
		}
	}
}

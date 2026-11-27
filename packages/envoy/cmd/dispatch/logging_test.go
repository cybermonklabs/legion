package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A boot that cannot go on exits with 1, and the reason it logged reaches stderr first. Log lines
// are written by a goroutine of their own (logging.Handler), which os.Exit does not wait for, so
// an exit that did not flush them could end the process with its reason still buffered. This boot
// fails late, after it has connected, migrated and logged other lines, on a web dist whose
// index.html cannot be read: the dashboard publish refuses it, and the boot does not serve.
func TestABootFailureWritesItsReasonBeforeTheProcessExits(t *testing.T) {
	database := storetest.Open(t)
	dist := t.TempDir()
	page := filepath.Join(dist, "index.html")
	if err := os.Mkdir(page, 0o755); err != nil {
		t.Fatalf("make index.html a directory: %v", err)
	}
	process := startDispatchProcessWith(t, database.Pool.Config().ConnString(), dispatchOptions{WebDist: dist})
	process.WaitExit(t, "a boot whose web dist's index.html cannot be read")
	if code := process.Cmd.ProcessState.ExitCode(); code != 1 {
		t.Errorf("the refused boot exited %d, want 1", code)
	}
	want := fmt.Sprintf(`level=ERROR msg="dispatch: publish dashboard assets" error="read the dashboard's index.html: read %s: is a directory"`, page)
	if output := process.Output.String(); !strings.Contains(output, want) {
		t.Errorf("the refused boot's output lacks its reason %s:\n%s", want, output)
	}
}

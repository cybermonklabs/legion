package workspace

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/envlisting"
)

// git and jj, which run in a managed repository's checkout, start without either raw NATS seed the
// daemon holds.
func TestWorkspaceCommandsGetNoNATSSeed(t *testing.T) {
	t.Setenv("NATS_NKEY_SEED", "pane-seed-value")
	t.Setenv("NATS_DAEMON_NKEY_SEED", "daemon-seed-value")
	git := filepath.Join(t.TempDir(), "git")
	if err := os.WriteFile(git, []byte("#!/bin/sh\nenv\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	result, err := NewRunner(10*time.Second, map[string]string{"git": git}).Run(context.Background(), Command{Argv: []string{"git", "status"}, Dir: t.TempDir(), Timeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	vars := envlisting.Vars(result.Stdout)
	if name, leaked := envlisting.HoldingValue(vars, "pane-seed-value", "daemon-seed-value"); leaked || vars["PATH"] == "" {
		t.Errorf("git's environment carries a NATS seed in %s (or is not the daemon's):\n%s", name, result.Stdout)
	}
}

package workspace

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/envlisting"
	"github.com/sjawhar/legion/daemon/internal/execfile"
)

// git and jj, which run in a managed repository's checkout, start without either raw NATS seed the
// daemon holds.
func TestWorkspaceCommandsGetNoNATSSeed(t *testing.T) {
	// Unique values, so that only a variable a seed reached can hold one (envlisting).
	paneSeed, daemonSeed := envlisting.Unique("pane-seed-"), envlisting.Unique("daemon-seed-")
	t.Setenv("NATS_NKEY_SEED", paneSeed)
	t.Setenv("NATS_DAEMON_NKEY_SEED", daemonSeed)
	git := filepath.Join(t.TempDir(), "git")
	if err := execfile.Write(git, []byte("#!/bin/sh\nenv\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	result, err := NewRunner(10*time.Second, map[string]string{"git": git}).Run(context.Background(), Command{Argv: []string{"git", "status"}, Dir: t.TempDir(), Timeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	vars := envlisting.Vars(result.Stdout)
	if name, leaked := envlisting.Carrying(vars, paneSeed, daemonSeed); leaked {
		t.Errorf("git's environment carries a NATS seed in %s:\n%s", name, result.Stdout)
	}
	if vars["PATH"] == "" {
		t.Errorf("git's environment is not the daemon's (no PATH):\n%s", result.Stdout)
	}
}

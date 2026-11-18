package omplaunch

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/envlisting"
)

// `mise where` starts without either raw NATS seed the daemon holds.
func TestMiseWhereGetsNoNATSSeed(t *testing.T) {
	// Unique values, so that only a variable a seed reached can hold one (envlisting).
	paneSeed, daemonSeed := envlisting.Unique("pane-seed-"), envlisting.Unique("daemon-seed-")
	t.Setenv("NATS_NKEY_SEED", paneSeed)
	t.Setenv("NATS_DAEMON_NKEY_SEED", daemonSeed)
	dir := t.TempDir()
	dump := filepath.Join(dir, "env")
	mise := filepath.Join(dir, "mise")
	if err := os.WriteFile(mise, []byte("#!/bin/sh\nenv >"+dump+"\necho "+dir+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, _ = resolveMiseOmp(mise, "github:acme/omp@1") // the tool dir has no omp; the environment is the point
	seen, err := os.ReadFile(dump)
	if err != nil {
		t.Fatalf("mise never ran: %v", err)
	}
	vars := envlisting.Vars(string(seen))
	if name, leaked := envlisting.Carrying(vars, paneSeed, daemonSeed); leaked {
		t.Errorf("mise where's environment carries a NATS seed in %s:\n%s", name, seen)
	}
	if vars["PATH"] == "" {
		t.Errorf("mise where's environment is not the daemon's (no PATH):\n%s", seen)
	}
}

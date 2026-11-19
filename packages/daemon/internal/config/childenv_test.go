package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/envlisting"
)

// A key command, and the secrets command a provider key runs, start without either raw NATS seed
// the daemon holds.
func TestTheDaemonsKeyCommandsGetNoNATSSeed(t *testing.T) {
	// Unique values, so that only a variable a seed reached can hold one (envlisting).
	paneSeed, daemonSeed := envlisting.Unique("pane-seed-"), envlisting.Unique("daemon-seed-")
	t.Setenv("NATS_NKEY_SEED", paneSeed)
	t.Setenv("NATS_DAEMON_NKEY_SEED", daemonSeed)
	out, err := executePrivateKeyCommand("env", "github_apps.implement.private_key_command")
	if err != nil {
		t.Fatalf("executePrivateKeyCommand: %v", err)
	}
	vars := envlisting.Vars(out)
	if name, leaked := envlisting.Carrying(vars, paneSeed, daemonSeed); leaked {
		t.Errorf("the key command's environment carries a NATS seed in %s:\n%s", name, out)
	}
	if vars["PATH"] == "" {
		t.Errorf("the key command's environment is not the daemon's (no PATH):\n%s", out)
	}

	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "secrets"), []byte("#!/bin/sh\nenv\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(), "PATH="+bin+":/usr/bin:/bin")
	got, err := runSecretsGet(ProviderKey{Env: "X_KEY", Secret: "X_KEY"}, "--raw", env)
	if err != nil {
		t.Fatalf("runSecretsGet: %v", err)
	}
	if name, leaked := envlisting.Carrying(envlisting.Vars(string(got)), paneSeed, daemonSeed); leaked {
		t.Errorf("the secrets command's environment carries a NATS seed in %s:\n%s", name, got)
	}
}

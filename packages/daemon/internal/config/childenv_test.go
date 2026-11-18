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
	t.Setenv("NATS_NKEY_SEED", "pane-seed-value")
	t.Setenv("NATS_DAEMON_NKEY_SEED", "daemon-seed-value")
	out, err := executePrivateKeyCommand("env", "github_apps.implement.private_key_command")
	if err != nil {
		t.Fatalf("executePrivateKeyCommand: %v", err)
	}
	vars := envlisting.Vars(out)
	if name, leaked := envlisting.HoldingValue(vars, "pane-seed-value", "daemon-seed-value"); leaked || vars["PATH"] == "" {
		t.Errorf("the key command's environment carries a NATS seed in %s (or is not the daemon's):\n%s", name, out)
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
	if name, leaked := envlisting.HoldingValue(envlisting.Vars(string(got)), "pane-seed-value", "daemon-seed-value"); leaked {
		t.Errorf("the secrets command's environment carries a NATS seed in %s:\n%s", name, got)
	}
}

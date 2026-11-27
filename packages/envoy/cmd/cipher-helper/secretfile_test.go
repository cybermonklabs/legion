//go:build linux

package main

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipherclient"
	"github.com/sjawhar/envoy/internal/redact"
)

// wantNamedNotPrinted fails t unless text names each of named and holds none of secrets.
func wantNamedNotPrinted(t *testing.T, what, text string, named []string, secrets ...string) {
	t.Helper()
	for _, secret := range secrets {
		if strings.Contains(text, secret) {
			t.Errorf("%s holds %q: %s", what, secret, text)
		}
	}
	for _, name := range named {
		if !strings.Contains(text, name) {
			t.Errorf("%s does not name %q: %s", what, name, text)
		}
	}
}

// underAFile is a path whose parent directory is a regular file, so every call on it fails
// "not a directory".
func underAFile(t *testing.T, dir, name string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "a-file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(file, name)
}

// captureStderr runs run with os.Stderr a pipe, and answers what it wrote there.
func captureStderr(t *testing.T, run func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	old := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = old }()
	run()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// TestClaimMachineFileNamesTheSettingNeverThePath: the machine file's claim is a lock beside it,
// whose path is the machine file's own, so neither a claim another helper holds nor a directory
// the claim cannot make prints either path: each names AGENT_SECRETS_MACHINE_FILE and says why.
func TestClaimMachineFileNamesTheSettingNeverThePath(t *testing.T) {
	const secret = "hunter2pw-claim-T6y2"
	path := filepath.Join(t.TempDir(), secret, "machine.json")
	first, err := claimMachineFile(path)
	if err != nil {
		t.Fatalf("the first claim: %v", err)
	}
	_, err = claimMachineFile(path)
	if err == nil {
		t.Fatal("a second claim on a held machine file = nil")
	}
	wantNamedNotPrinted(t, "the held claim's refusal", err.Error(), []string{"AGENT_SECRETS_MACHINE_FILE", "stop it first"}, secret)

	_, err = claimMachineFile(underAFile(t, filepath.Join(t.TempDir(), secret), "machine.json"))
	if err == nil {
		t.Fatal("a claim under a file = nil")
	}
	wantNamedNotPrinted(t, "the directory error", err.Error(), []string{"AGENT_SECRETS_MACHINE_FILE", "not a directory"}, secret)

	// The control: once the first claim is released, the same path is claimed again.
	first.Close()
	again, err := claimMachineFile(path)
	if err != nil {
		t.Fatalf("a claim after the first was released: %v", err)
	}
	again.Close()
}

// TestAMachineFileLockThatCannotBeOpenedNamesTheSetting: the lock beside the machine file is opened
// at the machine file's own path with ".lock" after it, so when it cannot be opened the refusal
// names AGENT_SECRETS_MACHINE_FILE and the system's reason, never the path. With the way clear the
// same path is claimed: the control.
func TestAMachineFileLockThatCannotBeOpenedNamesTheSetting(t *testing.T) {
	const secret = "hunter2pw-lock-R4m8"
	path := filepath.Join(t.TempDir(), secret, "machine.json")
	// A directory where the lock goes, which no open for writing takes.
	if err := os.MkdirAll(path+".lock", 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := claimMachineFile(path)
	if err == nil {
		t.Fatal("a claim whose lock is a directory = nil")
	}
	wantNamedNotPrinted(t, "the lock's refusal", err.Error(), []string{"open the lock beside", "AGENT_SECRETS_MACHINE_FILE", "open: is a directory"}, secret)

	if err := os.Remove(path + ".lock"); err != nil {
		t.Fatal(err)
	}
	claim, err := claimMachineFile(path)
	if err != nil {
		t.Fatalf("a claim with the way clear: %v", err)
	}
	claim.Close()
}

// TestTheIdentityKeyLogLineNamesNoPath: the line the helper logs about the identity key it
// trades with, and the refusal of one others can read, name the key by where it lives under
// HOME, never by its path; the line still carries the key's public thumbprint.
func TestTheIdentityKeyLogLineNamesNoPath(t *testing.T) {
	const secret = "hunter2pw-home-J8d5"
	home := filepath.Join(t.TempDir(), secret)
	var out bytes.Buffer
	log := slog.New(slog.NewTextHandler(&out, nil))
	cfg := identityHelperConfig(t, home)
	if _, _, err := identityClient(cfg, log); err != nil {
		t.Fatal(err)
	}
	wantNamedNotPrinted(t, "the identity key line", out.String(), []string{"identity key", "thumbprint=", "made=true"}, secret)

	if err := os.Chmod(cfg.IdentityKeyFile, 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := identityClient(cfg, log)
	if err == nil {
		t.Fatal("a world-readable identity key = nil")
	}
	wantNamedNotPrinted(t, "the refusal", err.Error(), []string{"identity key", "chmod 600"}, secret)
}

// TestMigrateRecordsLogsNoPath: the records hold named sessions' signing keys, and their paths
// follow the machine file's and the socket's settings, so a migration that fails logs both files
// by their settings and the failure by its operation and reason, never by a path; one that works
// moves the records and logs nothing.
func TestMigrateRecordsLogsNoPath(t *testing.T) {
	const legacySecret, stateSecret = "hunter2pw-legacy-N2b7", "hunter2pw-state-F9k3"
	legacy := filepath.Join(t.TempDir(), legacySecret, "sessions.json")
	if err := os.MkdirAll(filepath.Dir(legacy), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, []byte("[]"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	log := slog.New(slog.NewTextHandler(&out, nil))
	migrateRecords(config{LegacyStatePath: legacy, StatePath: underAFile(t, filepath.Join(t.TempDir(), stateSecret), "sessions.json")}, log)
	wantNamedNotPrinted(t, "the migration's line", out.String(),
		[]string{"could not move the session records", "AGENT_SECRETS_STATE_FILE", "AGENT_SECRETS_HELPER_SOCK", "not a directory"}, legacySecret, stateSecret)

	// The control: a migration to a path it can write moves the records and logs nothing.
	out.Reset()
	durable := filepath.Join(t.TempDir(), stateSecret, "sessions.json")
	migrateRecords(config{LegacyStatePath: legacy, StatePath: durable}, log)
	if out.Len() != 0 {
		t.Errorf("a migration that worked logged: %s", out.String())
	}
	if data, err := os.ReadFile(durable); err != nil || string(data) != "[]" {
		t.Errorf("the durable records after the migration: %q %v", data, err)
	}
}

// TestInstallRefusesAValueWithoutPrintingIt: a unit line install cannot write is refused naming
// the variable and the rule it broke, never its value, which may be a secret pasted in place of
// a path or an account; a value it can write is written whole.
func TestInstallRefusesAValueWithoutPrintingIt(t *testing.T) {
	for _, c := range []struct{ name, value, secret, rule string }{
		{cipherclient.PoolAccountVar, "hunter2pw tok-acct-5Lm1", "tok-acct-5Lm1", "escaped"},
		{cipherclient.MachineTokenFileVar, "hunter2pw-rel-3Qw7", "hunter2pw-rel-3Qw7", "absolute"},
	} {
		r := newInstallRig(t)
		r.env[c.name] = c.value
		err := r.run("--url", "https://secrets.example")
		if err == nil {
			t.Fatalf("install with %s=%q = nil", c.name, c.value)
		}
		wantNamedNotPrinted(t, "the refusal of "+c.name, err.Error(), []string{c.name, c.rule}, c.secret, "hunter2pw")
		if _, err := os.Stat(r.unit); !os.IsNotExist(err) {
			t.Errorf("a refused install wrote the unit: %v", err)
		}
	}
	// The control: an absolute path is written into the unit as it is.
	r := newInstallRig(t)
	clean := filepath.Join(r.dir, "machine.json")
	r.env["AGENT_SECRETS_MACHINE_FILE"] = clean
	if err := r.run("--url", "https://secrets.example"); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(r.unit); !strings.Contains(string(data), "\nEnvironment=AGENT_SECRETS_MACHINE_FILE="+clean+"\n") {
		t.Errorf("the unit lacks the machine file:\n%s", data)
	}
}

// TestInstallFlagErrorsPrintNoArgument: Go's flag package writes a failure to stderr and returns
// it with the argument it could not read in its text (bad flag syntax: <arg>, flag provided but
// not defined: -<name>, invalid value "<v>"), and a --url's credential, or a secret pasted where a
// flag goes, is such an argument. install prints nothing of it, on stderr or in its error, and
// names the flag of its own the argument was for, where there was one.
func TestInstallFlagErrorsPrintNoArgument(t *testing.T) {
	for _, c := range []struct {
		args  []string
		named []string
	}{
		{[]string{"--restart-with-sessions=hunter2pw-flag-A1"}, []string{"--restart-with-sessions"}},
		{[]string{"--https://" + fixtureUserinfo("B2") + "@broker.invalid"}, []string{"--url", "--restart-with-sessions"}},
		{[]string{"---url=https://" + fixtureUserinfo("C3") + "@broker.invalid"}, []string{"--url", "--restart-with-sessions"}},
		{[]string{"--url"}, []string{"--url", "needs a value"}},
		// A made-up secret given to the bool flag, whose text names the other flag after a quote:
		// the flag that refused it is named, whatever its value says.
		{[]string{"--restart-with-sessions=hunter2pw-flag-D4\" for -url: x"}, []string{"--restart-with-sessions", "was given a value it does not take"}},
		// A flag install does not define whose name ends as a refused value's text does: it is no
		// flag of install's, so neither flag is named as the one it was for.
		{[]string{"--hunter2pw-flag-E5\" for -url: x"}, []string{"--url", "--restart-with-sessions", "neither"}},
		{[]string{"-url"}, []string{"--url", "needs a value"}},
		// --url last, after an argument the parse already stopped at, is not what failed.
		{[]string{"--hunter2pw-flag-F6", "--url"}, []string{"--url", "--restart-with-sessions", "neither"}},
	} {
		r := newInstallRig(t)
		var err error
		stderr := captureStderr(t, func() { err = r.run(c.args...) })
		if err == nil {
			t.Fatalf("install %q = nil", c.args)
		}
		wantNamedNotPrinted(t, "the error of install "+strings.Join(c.args, " "), err.Error(), c.named, "hunter2pw", "tokuser9f3")
		if stderr != "" {
			t.Errorf("install %q wrote to stderr: %q", c.args, stderr)
		}
	}
	// The control: the flags install takes still parse.
	r := newInstallRig(t)
	if err := r.run("--url", "https://secrets.example", "--restart-with-sessions=false"); err != nil {
		t.Fatalf("install with its own flags: %v", err)
	}
}

// TestInstallNamesTheURLsSetting: the refusal of a broker URL a unit line cannot hold names where
// the URL came from, --url or AGENT_SECRETS_URL, beside the URL as redact.URL prints it; a clean
// URL is written into the unit whole.
func TestInstallNamesTheURLsSetting(t *testing.T) {
	credentialed := "https://" + fixtureUserinfo("%40x") + "@broker.invalid"
	r := newInstallRig(t)
	err := r.run("--url", credentialed)
	if err == nil {
		t.Fatal("install of a URL that needs escaping = nil")
	}
	wantNamedNotPrinted(t, "the --url refusal", err.Error(), []string{"--url", redact.NotPrinted}, "hunter2pw", "tokuser9f3")

	r = newInstallRig(t)
	r.env["AGENT_SECRETS_URL"] = credentialed
	err = r.run()
	if err == nil {
		t.Fatal("install of a URL that needs escaping = nil")
	}
	wantNamedNotPrinted(t, "the AGENT_SECRETS_URL refusal", err.Error(), []string{"AGENT_SECRETS_URL", redact.NotPrinted}, "hunter2pw", "tokuser9f3")

	// The control: a clean URL goes into the unit as it is.
	r = newInstallRig(t)
	if err := r.run("--url", "https://secrets.example"); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(r.unit); !strings.Contains(string(data), "\nEnvironment=AGENT_SECRETS_URL=https://secrets.example\n") {
		t.Errorf("the unit lacks the URL:\n%s", data)
	}
}

// fixtureUserinfo is a made-up user name and password for a URL probe, joined here rather than
// written as one URL literal, so no line of this file reads as a URL carrying a credential.
func fixtureUserinfo(tag string) string { return "tokuser9f3" + ":" + "hunter2pw-flag-" + tag }

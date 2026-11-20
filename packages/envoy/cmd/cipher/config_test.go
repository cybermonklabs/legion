// packages/envoy/cmd/cipher/config_test.go
//
// ~/.config/agent-secrets/config: the settings a process started with no environment of its own
// (a systemd unit, a cron job) reads in place of the variables it was not given, as the AWS CLI
// reads ~/.aws/config. A variable that is set wins over the file, and an empty one counts as unset.
package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeClientConfig points HOME at a directory of the test's own holding content as its
// agent-secrets config file, and answers the file's path. The settings run read stay loaded for
// the test alone.
func writeClientConfig(t *testing.T, content string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, ".config", "agent-secrets", "config")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { clientConfig = nil })
	return path
}

// unsetenv unsets name for the rest of t, restoring it after.
func unsetenv(t *testing.T, name string) {
	t.Helper()
	t.Setenv(name, "")
	if err := os.Unsetenv(name); err != nil {
		t.Fatal(err)
	}
}

func TestTheConfigFileFillsAnUnsetVariable(t *testing.T) {
	path := writeClientConfig(t, "# the secrets broker\nAGENT_SECRETS_URL=https://secrets.example/\n\nAGENT_SECRETS_SIGNIN_CLIENT_ID = device-client\n")
	unsetenv(t, "AGENT_SECRETS_URL")
	unsetenv(t, "AGENT_SECRETS_SIGNIN_CLIENT_ID")
	if err := loadClientConfig(); err != nil {
		t.Fatalf("load %s: %v", path, err)
	}
	if got := brokerBaseURL().Raw(); got != "https://secrets.example" {
		t.Fatalf("brokerBaseURL().Raw() = %q; want the file's https://secrets.example", got)
	}
	if got := clientGetenv("AGENT_SECRETS_SIGNIN_CLIENT_ID"); got != "device-client" {
		t.Fatalf("AGENT_SECRETS_SIGNIN_CLIENT_ID = %q; want the file's device-client", got)
	}
	if got := clientGetenv("AGENT_SECRETS_MACHINE_IDENTITY"); got != "" {
		t.Fatalf("AGENT_SECRETS_MACHINE_IDENTITY = %q; want nothing, since neither the file nor the environment sets it", got)
	}
}

func TestAVariableWinsOverTheConfigFile(t *testing.T) {
	writeClientConfig(t, "AGENT_SECRETS_URL=https://from-the-file.example\n")
	t.Setenv("AGENT_SECRETS_URL", "https://from-the-environment.example")
	if err := loadClientConfig(); err != nil {
		t.Fatal(err)
	}
	if got := brokerBaseURL().Raw(); got != "https://from-the-environment.example" {
		t.Fatalf("brokerBaseURL().Raw() = %q; want the environment's https://from-the-environment.example", got)
	}
}

func TestAnEmptyVariableReadsTheConfigFile(t *testing.T) {
	writeClientConfig(t, "AGENT_SECRETS_URL=https://from-the-file.example\n")
	t.Setenv("AGENT_SECRETS_URL", "")
	if err := loadClientConfig(); err != nil {
		t.Fatal(err)
	}
	if got := brokerBaseURL().Raw(); got != "https://from-the-file.example" {
		t.Fatalf("brokerBaseURL().Raw() = %q; want the file's https://from-the-file.example, since an empty variable is unset", got)
	}
}

func TestAnUnknownKeyInTheConfigFileIsAUsageError(t *testing.T) {
	for name, line := range map[string]string{
		"a key the file does not take": "AWS_PROFILE=operator",
		"a shell export":               "export AGENT_SECRETS_URL=https://secrets.example",
		"no equals sign":               "AGENT_SECRETS_URL",
	} {
		t.Run(name, func(t *testing.T) {
			path := writeClientConfig(t, "# settings\n\n"+line+"\n")
			var stdout, stderr bytes.Buffer
			code := run([]string{"identity"}, &stdout, &stderr)
			if code != exitUsageError {
				t.Fatalf("exit %d, want %d (stderr %q)", code, exitUsageError, stderr.String())
			}
			if !strings.Contains(stderr.String(), path) || !strings.Contains(stderr.String(), "line 3") {
				t.Fatalf("stderr %q; want it to name %s and line 3", stderr.String(), path)
			}
		})
	}
}

// TestAConfigFileOthersCanWriteIsRefused: the file names the broker this machine proves its
// identity to and the identity it proves, so a file its group or others can write is a usage error
// naming the file and the fix, and nothing in it is read. A file others can only read is taken.
func TestAConfigFileOthersCanWriteIsRefused(t *testing.T) {
	for name, mode := range map[string]os.FileMode{"its group can write it": 0o620, "anyone can write it": 0o602} {
		t.Run(name, func(t *testing.T) {
			path := writeClientConfig(t, "AGENT_SECRETS_URL=https://secrets.example\n")
			if err := os.Chmod(path, mode); err != nil {
				t.Fatal(err)
			}
			unsetenv(t, "AGENT_SECRETS_URL")
			var stdout, stderr bytes.Buffer
			if code := run([]string{"identity"}, &stdout, &stderr); code != exitUsageError {
				t.Fatalf("exit %d, want %d (stderr %q)", code, exitUsageError, stderr.String())
			}
			if !strings.Contains(stderr.String(), path) || !strings.Contains(stderr.String(), "chmod go-w") {
				t.Fatalf("stderr %q; want it to name %s and chmod go-w", stderr.String(), path)
			}
			if clientConfig != nil {
				t.Fatalf("the settings %v were read from a file others can write", clientConfig)
			}
		})
	}

	path := writeClientConfig(t, "AGENT_SECRETS_URL=https://secrets.example\n")
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	unsetenv(t, "AGENT_SECRETS_URL")
	if err := loadClientConfig(); err != nil {
		t.Fatalf("a file others can only read: %v", err)
	}
	if got := brokerBaseURL().Raw(); got != "https://secrets.example" {
		t.Fatalf("brokerBaseURL().Raw() = %q; want the file's", got)
	}
}

// TestAConfigFileAnotherUserOwnsIsRefused: a file this user does not own can be changed by its
// owner whatever its mode, so it is a usage error naming the file and its owner, and nothing in it
// is read. A file root owns is taken, as OpenSSH takes one: root can change any file anyway.
func TestAConfigFileAnotherUserOwnsIsRefused(t *testing.T) {
	owner := func(t *testing.T, uid int) {
		t.Helper()
		original := fileOwner
		fileOwner = func(os.FileInfo) (int, bool) { return uid, true }
		t.Cleanup(func() { fileOwner = original })
	}
	stranger := os.Getuid() + 4242

	path := writeClientConfig(t, "AGENT_SECRETS_URL=https://secrets.example\n")
	unsetenv(t, "AGENT_SECRETS_URL")
	owner(t, stranger)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"identity"}, &stdout, &stderr); code != exitUsageError {
		t.Fatalf("exit %d, want %d (stderr %q)", code, exitUsageError, stderr.String())
	}
	if want := fmt.Sprintf("is owned by uid %d", stranger); !strings.Contains(stderr.String(), path) || !strings.Contains(stderr.String(), want) {
		t.Fatalf("stderr %q; want it to name %s and say it %s", stderr.String(), path, want)
	}
	if clientConfig != nil {
		t.Fatalf("the settings %v were read from a file another user owns", clientConfig)
	}

	owner(t, 0)
	if err := loadClientConfig(); err != nil {
		t.Fatalf("a file root owns: %v", err)
	}
	if got := brokerBaseURL().Raw(); got != "https://secrets.example" {
		t.Fatalf("brokerBaseURL().Raw() = %q; want the file's", got)
	}
}

func TestNoConfigFileIsNoError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AGENT_SECRETS_URL", "https://secrets.example")
	t.Cleanup(func() { clientConfig = nil })
	if err := loadClientConfig(); err != nil {
		t.Fatalf("loadClientConfig with no file: %v", err)
	}
	if got := brokerBaseURL().Raw(); got != "https://secrets.example" {
		t.Fatalf("brokerBaseURL().Raw() = %q; want the environment's", got)
	}
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("AGENT_SECRETS_KEY_DIR", "")
	t.Setenv("AGENT_SECRETS_HELPER_SOCK", "")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"identity"}, &stdout, &stderr); code == exitUsageError {
		t.Fatalf("identity with no config file exited %d: %q", code, stderr.String())
	}
}

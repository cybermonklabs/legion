// packages/envoy/cmd/cipher/redact_core_test.go
//
// What the machine, enroll and airlock code prints in place of a value a person could have put a
// secret in: AGENT_SECRETS_APPROVE_URL only as internal/redact's URL prints it, enroll's help
// without the directory AGENT_SECRETS_KEY_DIR names, and the airlock's refusals without the
// broker's proxy URL or the WGETRC a command inherits, whose file can hold a password; the
// airlock's own paths still print. Every secret here is made up.
package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

// TestMachineLoginPrintsTheApproveURLOnlyAsRedactPrintsIt: an AGENT_SECRETS_APPROVE_URL that
// carries a credential, or that is no plain http(s) URL, is not printed, and the line names the
// setting in its place; a plain one still prints as the page under it, and names the setting too.
func TestMachineLoginPrintsTheApproveURLOnlyAsRedactPrintsIt(t *testing.T) {
	binary := buildAgentSecrets(t)
	sock := fakeLoginHelper(t, "KQ7M-X4PZ", []string{"issued"})
	for _, row := range []struct {
		name, approve, want string
		secrets             []string
	}{
		// The user name and password are joined to the URL here so no line reads as a URL that
		// carries a credential.
		{"a credential in the URL", "https://" + "tokuser9f3" + ":" + "hunter2pw-approve-4c8a" + "@dispatch.example", redact.NotPrinted, []string{"tokuser9f3", "hunter2pw-approve-4c8a"}},
		{"a token pasted in place of the URL", "hunter2pw-approve-token-71bd", redact.NotPrinted, []string{"hunter2pw-approve-token-71bd"}},
		{"a query in the URL", "https://dispatch.example/?token=hunter2pw-approve-q-0e3f", redact.NotPrinted, []string{"hunter2pw-approve-q-0e3f"}},
		{"control: a plain URL", "https://dispatch.example/", "enter it at https://dispatch.example/credentials/machine", nil},
	} {
		t.Run(row.name, func(t *testing.T) {
			stdout, stderr, exit := runAgentSecrets(t, binary, "http://unused", t.TempDir(),
				[]string{"AGENT_SECRETS_HELPER_SOCK=" + sock, "AGENT_SECRETS_APPROVE_URL=" + row.approve}, "machine", "login")
			if exit != 0 {
				t.Fatalf("exit %d, want 0 (stdout %q, stderr %q)", exit, stdout, stderr)
			}
			wantNamedNotPrinted(t, "stdout and stderr", stdout+stderr, nil, row.secrets...)
			wantNamedNotPrinted(t, "stdout", stdout, []string{row.want, "AGENT_SECRETS_APPROVE_URL"})
		})
	}
}

// TestEnrollHelpPrintsNoKeyDirectory: enroll's help, and the usage a flag it does not take
// prints, never print the directory AGENT_SECRETS_KEY_DIR names as --out's default: a person's
// key dir is where key.pem is, and a help text is pasted wherever a person asks for help. The
// help still says AGENT_SECRETS_KEY_DIR is --out's default, and still prints the default of
// --pod-token-file, which is this program's own constant.
func TestEnrollHelpPrintsNoKeyDirectory(t *testing.T) {
	const secret = "hunter2pw-keydir-6e2f18"
	t.Setenv("AGENT_SECRETS_KEY_DIR", filepath.Join(t.TempDir(), secret))
	for _, row := range []struct {
		args []string
		exit int
	}{
		{[]string{"-h"}, 0},
		{[]string{"--no-such-flag"}, exitUsageError},
	} {
		var stdout, stderr bytes.Buffer
		if code := cmdEnroll(row.args, &stdout, &stderr); code != row.exit {
			t.Fatalf("enroll %v: exit %d, want %d (stderr %q)", row.args, code, row.exit, stderr.String())
		}
		enroll := "enroll " + strings.Join(row.args, " ")
		wantNamedNotPrinted(t, enroll+"'s stdout and stderr", stdout.String()+stderr.String(), nil, secret)
		wantNamedNotPrinted(t, enroll+"'s stderr", stderr.String(), []string{"AGENT_SECRETS_KEY_DIR when not given", `(default "` + defaultPodTokenFile + `")`})
	}
}

// TestAnAirlockProxyURLThatCannotBeUsedIsNotPrinted: a proxy URL in the broker's answer that
// does not parse, or names no host, is refused without the URL or the parser's words about it,
// which quote it whole; a proxy URL that names a host is still used.
func TestAnAirlockProxyURLThatCannotBeUsedIsNotPrinted(t *testing.T) {
	for _, row := range []struct {
		name, proxy, secret string
	}{
		{"a URL that does not parse", "http://" + "agent-secrets" + ":" + "hunter2pw-proxy-9d0b33" + "@[::1", "hunter2pw-proxy-9d0b33"},
		{"a URL that names no host", "hunter2pw-proxy-nohost-1a7c", "hunter2pw-proxy-nohost-1a7c"},
	} {
		t.Run(row.name, func(t *testing.T) {
			release := testRelease("AIRLOCK_ME", nil, nil)
			release.ProxyURL = row.proxy
			_, err := airlockProxyURL(release)
			if err == nil {
				t.Fatalf("airlockProxyURL accepted %q", row.proxy)
			}
			wantNamedNotPrinted(t, "the refusal", err.Error(), []string{"airlock proxy URL"}, row.secret)
		})
	}
	t.Run("control: a URL that names a host", func(t *testing.T) {
		if _, err := airlockProxyURL(testRelease("AIRLOCK_ME", nil, nil)); err != nil {
			t.Fatalf("airlockProxyURL: %v", err)
		}
	})
}

// inheritedWgetrc is a WGETRC a command could inherit: a file at a path a person chose, holding a
// password for wget as a wgetrc may (http_password). It answers the environment's WGETRC entry
// and the made-up secrets no airlock output may hold: a part of that path, and the password.
func inheritedWgetrc(t *testing.T) (string, []string) {
	t.Helper()
	const dirSecret, httpPass = "hunter2pw-wgetrc-dir-1c5e", "hunter2pw-wgetrc-pass-9a07"
	dir := filepath.Join(t.TempDir(), dirSecret)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "wgetrc")
	// wget's key for a password, joined from parts so that no line here reads as one set to a value.
	key := "http_" + "pass" + "word"
	content := "http_user = agent\n" + key + " = " + httpPass + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return "WGETRC=" + path, []string{dirSecret, httpPass}
}

// TestAnAirlockDirectoryThatCannotBePutInPlaceNamesItsPathButNotWGETRCs: every path the airlock
// prints is its root - XDG_RUNTIME_DIR, else TMPDIR, else the system's temporary directory - and
// names this CLI made below it, none of which a person sets to a secret's file, so when a grant's
// directory cannot take its final name the refusal names that path and the system's reason. The
// WGETRC the command inherits names a file that can hold http_password, so neither its path nor
// what it holds prints. With nothing in the way the root takes the grant's files, and the wgetrc
// there carries the inherited lines: the control, which shows the run read that WGETRC.
func TestAnAirlockDirectoryThatCannotBePutInPlaceNamesItsPathButNotWGETRCs(t *testing.T) {
	wgetrc, secrets := inheritedWgetrc(t)
	base := filepath.Join(t.TempDir(), "runtime")
	root := filepath.Join(base, "agent-secrets", "airlock")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	environ := []string{"XDG_RUNTIME_DIR=" + base, wgetrc}
	// A file where the grant's directory goes: the rename of a directory onto it is refused.
	if err := os.WriteFile(filepath.Join(root, "grant-one"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := prepareAirlock(environ, testRelease("AIRLOCK_ME", nil, nil), "grant-one", nil)
	if err == nil {
		t.Fatal("prepareAirlock put its directory in place over a file")
	}
	wantNamedNotPrinted(t, "the refusal", err.Error(), []string{"put the airlock directory in place", filepath.Join(root, "grant-one") + ": not a directory"}, secrets...)

	t.Run("control: nothing in the way", func(t *testing.T) {
		if err := os.Remove(filepath.Join(root, "grant-one")); err != nil {
			t.Fatal(err)
		}
		files, err := prepareAirlock(environ, testRelease("AIRLOCK_ME", nil, nil), "grant-one", nil)
		if err != nil {
			t.Fatalf("prepareAirlock: %v", err)
		}
		if data, err := os.ReadFile(files.wgetrc); err != nil || !strings.Contains(string(data), "http_password") {
			t.Fatalf("the grant's wgetrc: %v; want it to carry the inherited lines", err)
		}
	})
}

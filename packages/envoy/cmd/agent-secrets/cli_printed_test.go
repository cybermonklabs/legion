// packages/envoy/cmd/agent-secrets/cli_printed_test.go
//
// What the forms print about a value a person typed or set: a flag's value that does not parse,
// the default a flag reads from AGENT_SECRETS_KEY_DIR, AGENT_SECRETS_ENROLL_WAIT, and key.pem, the
// file that holds a session key. Each names the flag, the variable or the setting that names the
// file, and never the value or the path: a URL flag's value can carry a credential, and a setting
// meant for a path is where a person who pasted the credential itself puts it. Each test runs the
// form in-process with a made-up secret where a person could put one, and a control row that
// shows the clean case still works.
package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/envoy/brokeraddr"
	"github.com/sjawhar/envoy/internal/broker/helper"
	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/brokerclient"
)

// signArgs is a use command that asks nothing of the network once it has a signer.
var signArgs = []string{"sign", "--method", "GET", "--url", "https://broker.example/v1/enrollments/self"}

// useBareEnvironment gives the rest of t an environment of its own: HOME, XDG_STATE_HOME and
// XDG_RUNTIME_DIR empty directories (so no settings file, sign-in, machine cache or helper socket
// of the developer's is read), AGENT_SECRETS_KEY_DIR an empty directory, no home from the password
// database (so a row that empties HOME reaches none of the developer's files either), and no
// broker, helper socket, machine identity, person or enrollment wait named.
func useBareEnvironment(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("AGENT_SECRETS_KEY_DIR", t.TempDir())
	for _, name := range []string{"AGENT_SECRETS_URL", "AGENT_SECRETS_HELPER_SOCK", "AGENT_SECRETS_SIGNIN_DIR", "AGENT_SECRETS_MACHINE_IDENTITY", "AGENT_SECRETS_PERSON", "AGENT_SECRETS_ENROLL_WAIT"} {
		t.Setenv(name, "")
	}
	original := passwdHome
	passwdHome = func() string { return "" }
	t.Cleanup(func() { passwdHome = original })
}

// runIn runs `agent-secrets args...` in-process.
func runIn(args ...string) (stdout, stderr string, code int) {
	var out, errOut bytes.Buffer
	code = run(args, &out, &errOut)
	return out.String(), errOut.String(), code
}

// TestAFlagValueThatDoesNotParseIsNotPrinted: a flag whose value does not parse is refused naming
// the flag, and an argument whose flag syntax does not parse, or that is no flag of the form, is
// refused as one, never quoting the value or the argument, which Go's flag package does: an
// argument that starts with '-' can be a token pasted in the wrong place. Each prints the form's
// usage and exits 2, as before. A flag given no value is still named as Go names it, and values
// that parse still parse (the controls).
func TestAFlagValueThatDoesNotParseIsNotPrinted(t *testing.T) {
	const secret = "hunter2pw-flagvalue-5e0b32"
	for _, row := range []struct {
		name string
		args []string
		want []string
	}{
		{"a duration given as the next argument", []string{"request", "GRANT_ME", "--ttl", secret}, []string{"invalid value for flag -ttl", "usage: agent-secrets request"}},
		{"a duration given after =", []string{"request", "GRANT_ME", "--ttl=" + secret}, []string{"invalid value for flag -ttl", "usage: agent-secrets request"}},
		{"a boolean", []string{"request", "GRANT_ME", "--json=" + secret}, []string{"invalid boolean value for flag -json", "usage: agent-secrets request"}},
		{"a boolean of machine list", []string{"machine", "list", "--json=" + secret}, []string{"invalid boolean value for flag -json", "usage: agent-secrets machine list"}},
		{"a number", []string{"register", "--wait=" + secret}, []string{"invalid value for flag -wait", "usage: agent-secrets register"}},
		{"a URL flag of three dashes", []string{"sign", "---url=https://tokuser9f3:" + secret + "@broker.example/v1/x"}, []string{"bad flag syntax: a flag that does not parse", "usage: agent-secrets sign"}},
		{"an argument that is no flag", []string{"request", "GRANT_ME", "--" + secret}, []string{"an argument that is not a flag of this form (not printed)", "usage: agent-secrets request"}},
		{"an argument that is no flag, with a value", []string{"secret", "list", "-" + secret + "=x"}, []string{"an argument that is not a flag of this form (not printed)", "usage: agent-secrets secret list"}},
		{"control: a flag given no value", []string{"request", "GRANT_ME", "--ttl"}, []string{"flag needs an argument: -ttl"}},
		{"control: values that parse", []string{"request", "GRANT_ME", "--ttl", "1h", "--json=true"}, []string{"AGENT_SECRETS_URL is required"}},
	} {
		t.Run(row.name, func(t *testing.T) {
			useBareEnvironment(t)
			stdout, stderr, code := runIn(row.args...)
			if code != exitUsageError {
				t.Fatalf("exit %d, stderr %q; want %d", code, stderr, exitUsageError)
			}
			wantNamedNotPrinted(t, "stderr", stdout+stderr, row.want, []string{secret, "tokuser9f3"}...)
			if strings.HasPrefix(row.name, "control: values") && strings.Contains(stderr, "invalid") {
				t.Errorf("stderr %q refuses a value that parses", stderr)
			}
		})
	}
	t.Run("the error Parse answers", func(t *testing.T) {
		var stderr bytes.Buffer
		flags := newFlagSet("request", &stderr)
		flags.Duration("ttl", 0, "a lifetime")
		err := flags.Parse([]string{"--ttl=" + secret})
		if err == nil || err.Error() != "invalid value for flag -ttl" {
			t.Fatalf("Parse = %v, want the fixed refusal naming -ttl", err)
		}
		if strings.Contains(stderr.String(), secret) {
			t.Errorf("stderr %q holds the value", stderr.String())
		}
		if err := flags.Parse([]string{"--ttl=90s"}); err != nil || flags.Lookup("ttl").Value.String() != "1m30s" {
			t.Fatalf("Parse(--ttl=90s) = %v with -ttl %q, want 1m30s", err, flags.Lookup("ttl").Value)
		}
		if _, ok := flags.Lookup("ttl").Value.(flag.Getter); !ok {
			t.Errorf("-ttl's value is %T after Parse, not the flag package's own", flags.Lookup("ttl").Value)
		}
	})
}

// TestHelpNamesTheKeyDirVariableNotItsValue: keygen's --out defaults to AGENT_SECRETS_KEY_DIR, the
// directory of key.pem, and a help text is pasted wherever a person asks for help, so -h and the
// usage a parse failure prints name the variable beside the flag, never its value. A default
// that is the program's own constant still prints as it is (the control).
func TestHelpNamesTheKeyDirVariableNotItsValue(t *testing.T) {
	const secret = "hunter2pw-helpdir-80c4f1"
	for _, row := range []struct {
		args []string
		code int
		want []string
	}{
		{[]string{"keygen", "-h"}, 0, []string{`-out string`, "AGENT_SECRETS_KEY_DIR when not given"}},
		{[]string{"keygen", "--no-such-flag"}, exitUsageError, []string{"an argument that is not a flag of this form (not printed)", "AGENT_SECRETS_KEY_DIR when not given"}},
		{[]string{"enroll", "-h"}, 0, []string{`(default "` + defaultPodTokenFile + `")`}},
	} {
		t.Run(strings.Join(row.args, " "), func(t *testing.T) {
			useBareEnvironment(t)
			t.Setenv("AGENT_SECRETS_KEY_DIR", filepath.Join(t.TempDir(), secret))
			stdout, stderr, code := runIn(row.args...)
			if code != row.code {
				t.Fatalf("exit %d, stderr %q; want %d", code, stderr, row.code)
			}
			wantNamedNotPrinted(t, "the help", stdout+stderr, row.want, []string{secret}...)
		})
	}
}

// TestEnrollWaitRefusalNamesTheRuleNotTheValue: an AGENT_SECRETS_ENROLL_WAIT that is no duration
// is refused naming the variable and the rule it broke, never its value, by a use command and by
// `identity --source` alike, exit 2; a duration still bounds the wait (the control).
func TestEnrollWaitRefusalNamesTheRuleNotTheValue(t *testing.T) {
	const secret = "hunter2pw-enrollwait-6a2d90"
	for _, row := range []struct {
		name, wait string
		args       []string
		code       int
	}{
		{"a use command", secret, signArgs, exitUsageError},
		{"identity --source", secret, []string{"identity", "--source"}, exitUsageError},
		{"control: a duration", "3s", signArgs, 0},
	} {
		t.Run(row.name, func(t *testing.T) {
			useBareEnvironment(t)
			t.Setenv("AGENT_SECRETS_KEY_DIR", newKeyDir(t))
			t.Setenv("AGENT_SECRETS_ENROLL_WAIT", row.wait)
			_, stderr, code := runIn(row.args...)
			if code != row.code {
				t.Fatalf("exit %d, stderr %q; want %d", code, stderr, row.code)
			}
			if row.code == 0 {
				if strings.Contains(stderr, "AGENT_SECRETS_ENROLL_WAIT") {
					t.Errorf("stderr %q refuses a duration", stderr)
				}
				return
			}
			wantNamedNotPrinted(t, "stderr", stderr, []string{"AGENT_SECRETS_ENROLL_WAIT", "Go duration"}, []string{secret}...)
		})
	}
}

// keyPemFaults are the key.pem contents a loader refuses, each with the reason it names: a
// directory where the file should be (the system's reason, through redact.File), bytes that are
// no PEM, and a PEM block that holds no PKCS#8 key (fixed text, never x509's or asn1's words).
var keyPemFaults = []struct {
	name  string
	write func(t *testing.T, path string)
	want  string
}{
	{"key.pem is a directory", func(t *testing.T, path string) {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}, "read: is a directory"},
	{"key.pem is not PEM", func(t *testing.T, path string) {
		if err := os.WriteFile(path, []byte("hunter2pw-keybytes-1e8f60\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}, "not a PEM file"},
	{"key.pem is no PKCS#8 key", func(t *testing.T, path string) {
		block := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("hunter2pw-keybytes-1e8f60")})
		if err := os.WriteFile(path, block, 0o600); err != nil {
			t.Fatal(err)
		}
	}, "not a PKCS#8 private key"},
}

// keyPemLeaks are what no refusal of a key.pem prints: the key's bytes and the parsers' words.
var keyPemLeaks = []string{"hunter2pw-keybytes-1e8f60", "x509:", "asn1:"}

// TestKeyPemInTheKeyDirIsNamedByItsSettingNeverItsPath: a key.pem in AGENT_SECRETS_KEY_DIR that
// cannot be read, or holds no session key, is refused naming key.pem in AGENT_SECRETS_KEY_DIR,
// with the system's reason or fixed text, by the chain's key-dir source and by sign --enrollment
// alike, and neither the directory's path nor the key's bytes print. A key dir that holds a key
// still signs (the control).
func TestKeyPemInTheKeyDirIsNamedByItsSettingNeverItsPath(t *testing.T) {
	const secret = "hunter2pw-keydir-b51e07"
	for _, fault := range keyPemFaults {
		for _, form := range []struct {
			name string
			args []string
			code int
		}{
			{"the key-dir source", signArgs, 1},
			{"sign --enrollment", slices.Concat(signArgs, []string{"--enrollment", "enr-other"}), exitUsageError},
		} {
			t.Run(fault.name+", "+form.name, func(t *testing.T) {
				useBareEnvironment(t)
				dir := filepath.Join(t.TempDir(), secret)
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				fault.write(t, filepath.Join(dir, keyFile))
				t.Setenv("AGENT_SECRETS_KEY_DIR", dir)
				stdout, stderr, code := runIn(form.args...)
				if code != form.code {
					t.Fatalf("exit %d, stderr %q; want %d", code, stderr, form.code)
				}
				wantNamedNotPrinted(t, "the output", stdout+stderr, []string{"key.pem in AGENT_SECRETS_KEY_DIR", fault.want}, append([]string{secret}, keyPemLeaks...)...)
			})
		}
	}
	t.Run("control: a key dir that holds a key", func(t *testing.T) {
		useBareEnvironment(t)
		t.Setenv("AGENT_SECRETS_KEY_DIR", newKeyDir(t))
		stdout, stderr, code := runIn(signArgs...)
		if code != 0 || strings.Count(strings.TrimSpace(stdout), ".") != 2 {
			t.Fatalf("exit %d, stdout %q, stderr %q; want 0 and a compact JWS", code, stdout, stderr)
		}
	})
}

// TestKeygenNamesKeyPemByItsFlagNeverItsPath: a key keygen cannot write, into the directory --out
// names or, without --out, AGENT_SECRETS_KEY_DIR, is refused naming that directory by the flag and
// the variable, with the system's reason, and never its path. A directory keygen can write into
// still gets the key (the control).
func TestKeygenNamesKeyPemByItsFlagNeverItsPath(t *testing.T) {
	const secret = "hunter2pw-keygen-3c7a55"
	blockedKey := func(t *testing.T) string {
		t.Helper()
		dir := filepath.Join(t.TempDir(), secret)
		if err := os.MkdirAll(filepath.Join(dir, keyFile, "taken"), 0o700); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	underAFile := func(t *testing.T) string {
		t.Helper()
		file := filepath.Join(t.TempDir(), secret)
		if err := os.WriteFile(file, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		return filepath.Join(file, "keys")
	}
	for _, row := range []struct {
		name  string
		dir   func(t *testing.T) string
		byEnv bool
		want  []string
	}{
		{"key.pem cannot be put in place in --out", blockedKey, false, []string{"write key.pem in the directory --out (or AGENT_SECRETS_KEY_DIR) names", "rename: "}},
		{"key.pem cannot be put in place in AGENT_SECRETS_KEY_DIR", blockedKey, true, []string{"write key.pem in the directory --out (or AGENT_SECRETS_KEY_DIR) names", "rename: "}},
		{"--out cannot be made", underAFile, false, []string{"the directory --out (or AGENT_SECRETS_KEY_DIR) names", "mkdir: not a directory"}},
	} {
		t.Run(row.name, func(t *testing.T) {
			useBareEnvironment(t)
			dir := row.dir(t)
			args := []string{"keygen", "--out", dir}
			if row.byEnv {
				t.Setenv("AGENT_SECRETS_KEY_DIR", dir)
				args = []string{"keygen"}
			}
			stdout, stderr, code := runIn(args...)
			if code != 1 {
				t.Fatalf("exit %d, stderr %q; want 1", code, stderr)
			}
			wantNamedNotPrinted(t, "the output", stdout+stderr, row.want, []string{secret}...)
		})
	}
	t.Run("control: a directory keygen can write into", func(t *testing.T) {
		useBareEnvironment(t)
		dir := filepath.Join(t.TempDir(), "keys")
		stdout, stderr, code := runIn("keygen", "--out", dir)
		if code != 0 {
			t.Fatalf("exit %d, stderr %q; want 0", code, stderr)
		}
		data, err := os.ReadFile(filepath.Join(dir, keyFile))
		if err != nil {
			t.Fatal(err)
		}
		block, _ := pem.Decode(data)
		if block == nil {
			t.Fatalf("key.pem %q is not PEM", data)
		}
		key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		public, ok := key.(*ecdsa.PrivateKey)
		if !ok {
			t.Fatalf("key.pem holds a %T, want an ECDSA key", key)
		}
		if thumb, err := proof.Thumbprint(&public.PublicKey); err != nil || strings.TrimSpace(stdout) != thumb {
			t.Fatalf("keygen printed %q, want the thumbprint of the key it wrote, %q (%v)", stdout, thumb, err)
		}
	})
}

// TestTheMachineCacheKeyIsNamedByTheCacheNeverItsPath: the machine cache's key.pem, in
// ${XDG_STATE_HOME:-$HOME/.local/state}/agent-secrets/machine, holds the machine session's key, so
// a key there that cannot be read or holds no key, and a key that cannot be written there, are
// refused naming key.pem in the machine cache, never its path. A cache that holds a session still
// signs, and one that can be written still keeps the key (the controls).
func TestTheMachineCacheKeyIsNamedByTheCacheNeverItsPath(t *testing.T) {
	const secret = "hunter2pw-statehome-9d61c2"
	useCache := func(t *testing.T) string {
		t.Helper()
		useBareEnvironment(t)
		t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), secret))
		dir := machineCacheDir()
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	for _, fault := range keyPemFaults {
		t.Run("read: "+fault.name, func(t *testing.T) {
			dir := useCache(t)
			fault.write(t, filepath.Join(dir, keyFile))
			if err := os.WriteFile(filepath.Join(dir, enrollmentFile), []byte("enr-machine\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			stdout, stderr, code := runIn(signArgs...)
			if code != 1 {
				t.Fatalf("exit %d, stderr %q; want 1", code, stderr)
			}
			wantNamedNotPrinted(t, "the output", stdout+stderr, []string{"key.pem in the machine cache", fault.want}, append([]string{secret}, keyPemLeaks...)...)
		})
	}
	t.Run("write: key.pem cannot be put in place", func(t *testing.T) {
		dir := useCache(t)
		if err := os.MkdirAll(filepath.Join(dir, keyFile, "taken"), 0o700); err != nil {
			t.Fatal(err)
		}
		key, err := proof.NewKey()
		if err != nil {
			t.Fatal(err)
		}
		err = writeMachineCache(dir, key, "enr-machine")
		if err == nil {
			t.Fatal("writeMachineCache put a key where a directory is")
		}
		wantNamedNotPrinted(t, "the error", err.Error(), []string{"write key.pem in the machine cache", "rename: "}, []string{secret}...)
	})
	t.Run("control: a cache that holds a session", func(t *testing.T) {
		useCache(t)
		key, err := proof.NewKey()
		if err != nil {
			t.Fatal(err)
		}
		if err := writeMachineCache(machineCacheDir(), key, "enr-machine"); err != nil {
			t.Fatal(err)
		}
		started := time.Now()
		stdout, stderr, code := runIn(signArgs...)
		if code != 0 || strings.Count(strings.TrimSpace(stdout), ".") != 2 || time.Since(started) > 10*time.Second {
			t.Fatalf("exit %d, stdout %q, stderr %q; want 0 and a compact JWS", code, stdout, stderr)
		}
	})
}

// machineCacheWords is how every line names the machine cache, written out by hand.
const machineCacheWords = "the machine cache (${XDG_STATE_HOME:-$HOME/.local/state}/agent-secrets/machine)"

// TestTheKeyDirIsNamedByItsSettingNeverItsPath: the key dir holds key.pem, so no line about it, or
// about a file beside key.pem, spells its path: the chain's refusal of a key dir with no key.pem,
// an enrollment file that cannot be read or is empty, a launcher's enrollment.error, a wait on a
// pending marker nothing follows, and the source `identity --source` names each say
// AGENT_SECRETS_KEY_DIR, or its default's own expression where the variable is unset, with the
// system's reason where a file call failed. What a launcher wrote in enrollment.error still
// prints, and a key dir that holds a session still signs (the controls).
func TestTheKeyDirIsNamedByItsSettingNeverItsPath(t *testing.T) {
	const secret = "hunter2pw-keydirpath-7e3b40"
	writeEnrollment := func(t *testing.T, dir string, data []byte, asDirectory bool) {
		t.Helper()
		writeSessionFiles(t, dir, "enr-launcher")
		path := filepath.Join(dir, enrollmentFile)
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		switch {
		case asDirectory:
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
		case data != nil:
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, row := range []struct {
		name      string
		fill      func(t *testing.T, dir string)
		byDefault bool
		wait      string
		args      []string
		code      int
		want      []string
	}{
		{name: "no key.pem", fill: func(*testing.T, string) {}, args: signArgs, code: 1,
			want: []string{"key-dir: no key.pem in AGENT_SECRETS_KEY_DIR (a launcher puts one there"}},
		{name: "the enrollment file is a directory", fill: func(t *testing.T, dir string) { writeEnrollment(t, dir, nil, true) }, args: signArgs, code: 1,
			want: []string{"read the enrollment file in AGENT_SECRETS_KEY_DIR: read: is a directory"}},
		{name: "the enrollment file is empty", fill: func(t *testing.T, dir string) { writeEnrollment(t, dir, []byte("\n"), false) }, args: signArgs, code: 1,
			want: []string{"the enrollment file in AGENT_SECRETS_KEY_DIR is empty"}},
		{name: "a launcher's enrollment.error", fill: func(t *testing.T, dir string) {
			writeEnrollment(t, dir, nil, false)
			if err := os.WriteFile(filepath.Join(dir, enrollmentErrorFile), []byte("broker 503 DATABASE: postgres unreachable\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, args: signArgs, code: 1,
			want: []string{"read the enrollment file in AGENT_SECRETS_KEY_DIR: open: no such file or directory", "enrollment.error: broker 503 DATABASE: postgres unreachable"}},
		{name: "a pending marker nothing follows", fill: func(t *testing.T, dir string) { writePendingMarker(t, dir, 0) }, wait: "100ms", args: signArgs, code: 1,
			want: []string{"waited 100ms while AGENT_SECRETS_KEY_DIR held enrollment.pending"}},
		{name: "identity --source", fill: func(t *testing.T, dir string) { writeSessionFiles(t, dir, "enr-launcher") }, args: []string{"identity", "--source"},
			want: []string{"key-dir: AGENT_SECRETS_KEY_DIR\n"}},
		{name: "identity --source, the default key dir", fill: func(t *testing.T, dir string) { writeSessionFiles(t, dir, "enr-launcher") }, byDefault: true, args: []string{"identity", "--source"},
			want: []string{"key-dir: $XDG_RUNTIME_DIR/agent-secrets (AGENT_SECRETS_KEY_DIR is unset)\n"}},
		{name: "control: a key dir that holds a session", fill: func(t *testing.T, dir string) { writeSessionFiles(t, dir, "enr-launcher") }, args: signArgs,
			want: []string{"."}},
	} {
		t.Run(row.name, func(t *testing.T) {
			useBareEnvironment(t)
			dir := filepath.Join(t.TempDir(), secret)
			if row.byDefault {
				runtime := filepath.Join(t.TempDir(), secret)
				t.Setenv("XDG_RUNTIME_DIR", runtime)
				t.Setenv("AGENT_SECRETS_KEY_DIR", "")
				dir = filepath.Join(runtime, "agent-secrets")
			} else {
				t.Setenv("AGENT_SECRETS_KEY_DIR", dir)
			}
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			row.fill(t, dir)
			t.Setenv("AGENT_SECRETS_ENROLL_WAIT", row.wait)
			stdout, stderr, code := runIn(row.args...)
			if code != row.code {
				t.Fatalf("exit %d, stdout %q, stderr %q; want %d", code, stdout, stderr, row.code)
			}
			wantNamedNotPrinted(t, "the output", stdout+stderr, row.want, []string{secret}...)
		})
	}
}

// TestTheKeyDirsNameIsWhereTheKeyDirIs: keyDirName names the key dir by the expression keyDir
// resolves it by, never its path: AGENT_SECRETS_KEY_DIR, else under XDG_RUNTIME_DIR, else under
// /run/user/$UID, where helper.DefaultSocket looks when XDG_RUNTIME_DIR is unset. The last row
// shows keyDir is there, so the name is not a place the key dir is not.
func TestTheKeyDirsNameIsWhereTheKeyDirIs(t *testing.T) {
	for _, row := range []struct{ keyDir, runtime, want, dir string }{
		{"/elsewhere/keys", "/run/elsewhere", "AGENT_SECRETS_KEY_DIR", "/elsewhere/keys"},
		{"", "/run/elsewhere", "$XDG_RUNTIME_DIR/agent-secrets (AGENT_SECRETS_KEY_DIR is unset)", "/run/elsewhere/agent-secrets"},
		{"", "", "/run/user/$UID/agent-secrets (AGENT_SECRETS_KEY_DIR and XDG_RUNTIME_DIR are unset)", filepath.Join("/run/user", strconv.Itoa(os.Getuid()), "agent-secrets")},
	} {
		t.Setenv("AGENT_SECRETS_KEY_DIR", row.keyDir)
		t.Setenv("XDG_RUNTIME_DIR", row.runtime)
		if got := keyDirName(); got != row.want {
			t.Errorf("keyDirName() = %q with AGENT_SECRETS_KEY_DIR %q and XDG_RUNTIME_DIR %q; want %q", got, row.keyDir, row.runtime, row.want)
		}
		if got := keyDir(); got != row.dir {
			t.Errorf("keyDir() = %q; want %q", got, row.dir)
		}
	}
}

// TestAKeyDirMachineSessionMovesWithoutItsPaths: a machine session an older client left in the key
// dir, beside machine-session, moves to the machine cache, and each refusal of the move names the
// key dir by AGENT_SECRETS_KEY_DIR and the cache by where it is, with the system's reason, never a
// path: a cache whose key.pem cannot be put in place, a key dir copy that cannot be removed, a
// marker that cannot, a cache that cannot be made, and no directory for one. A move that can be
// made still moves the session, which then signs (the control).
func TestAKeyDirMachineSessionMovesWithoutItsPaths(t *testing.T) {
	const secret = "hunter2pw-migrate-0f4c9a"
	blockWithADirectory := func(t *testing.T, path string) {
		t.Helper()
		if err := os.RemoveAll(path); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(path, "taken"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, row := range []struct {
		name string
		fill func(t *testing.T, keyDir, cache string)
		want []string
	}{
		{"the cache's key.pem cannot be put in place", func(t *testing.T, _, cache string) {
			blockWithADirectory(t, filepath.Join(cache, keyFile))
		}, []string{"move this machine's session from AGENT_SECRETS_KEY_DIR to " + machineCacheWords + ": rename: "}},
		{"the key dir's copy cannot be removed", func(t *testing.T, keyDir, cache string) {
			writeSessionFiles(t, cache, "enr-cache")
			blockWithADirectory(t, filepath.Join(keyDir, keyFile))
		}, []string{"remove the machine session AGENT_SECRETS_KEY_DIR held, which " + machineCacheWords + " replaces: remove: directory not empty"}},
		{"the marker cannot be removed", func(t *testing.T, keyDir, _ string) {
			blockWithADirectory(t, filepath.Join(keyDir, machineSessionMarker))
		}, []string{"remove the machine-session marker in AGENT_SECRETS_KEY_DIR: remove: directory not empty"}},
		{"the cache cannot be made", func(t *testing.T, _, cache string) {
			if err := os.WriteFile(filepath.Dir(cache), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}, []string{"make " + machineCacheWords + ": mkdir: not a directory"}},
		{"no directory for the cache", func(t *testing.T, _, _ string) {
			t.Setenv("XDG_STATE_HOME", "")
			t.Setenv("HOME", "")
		}, []string{"AGENT_SECRETS_KEY_DIR holds a machine session to move to the machine cache, and neither XDG_STATE_HOME nor HOME names a directory for it, nor does the password database give this user a home"}},
		{"control: a move that can be made", func(*testing.T, string, string) {}, []string{"."}},
	} {
		t.Run(row.name, func(t *testing.T) {
			useBareEnvironment(t)
			keyDir := filepath.Join(t.TempDir(), secret)
			writeSessionFiles(t, keyDir, "enr-machine")
			if err := os.WriteFile(filepath.Join(keyDir, machineSessionMarker), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("AGENT_SECRETS_KEY_DIR", keyDir)
			state := filepath.Join(t.TempDir(), secret)
			t.Setenv("XDG_STATE_HOME", state)
			cache := filepath.Join(state, "agent-secrets", "machine")
			if err := os.MkdirAll(filepath.Dir(filepath.Dir(cache)), 0o700); err != nil {
				t.Fatal(err)
			}
			row.fill(t, keyDir, cache)
			stdout, stderr, code := runIn(signArgs...)
			control := strings.HasPrefix(row.name, "control: ")
			if want := map[bool]int{true: 0, false: 1}[control]; code != want {
				t.Fatalf("exit %d, stdout %q, stderr %q; want %d", code, stdout, stderr, want)
			}
			wantNamedNotPrinted(t, "the output", stdout+stderr, row.want, []string{secret}...)
			if control && (!machineCacheHolds(cache) || exists(filepath.Join(keyDir, keyFile))) {
				t.Fatal("the move left the session where it was")
			}
		})
	}
}

// TestTheMachineCacheIsNamedByWhereItIsNeverItsPath: the machine cache holds key.pem, so the source
// `identity --source` names, an enrollment file there that cannot be read or is empty, and a cached
// session the broker no longer holds with no identity to register it again each name the cache by
// where it is, never by its path.
func TestTheMachineCacheIsNamedByWhereItIsNeverItsPath(t *testing.T) {
	const secret = "hunter2pw-cachename-c4a917"
	useCache := func(t *testing.T) string {
		t.Helper()
		useBareEnvironment(t)
		t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), secret))
		dir := machineCacheDir()
		writeSessionFiles(t, dir, "enr-machine")
		return dir
	}
	t.Run("identity --source", func(t *testing.T) {
		useCache(t)
		stdout, stderr, code := runIn("identity", "--source")
		if code != 0 {
			t.Fatalf("exit %d, stderr %q; want 0", code, stderr)
		}
		wantNamedNotPrinted(t, "the output", stdout+stderr, []string{"machine: the cached enrollment in " + machineCacheWords + "\n"}, []string{secret}...)
	})
	for _, row := range []struct {
		name  string
		write func(t *testing.T, path string)
		want  string
	}{
		{"the enrollment file is a directory", func(t *testing.T, path string) {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
		}, "read the enrollment file in " + machineCacheWords + ": read: is a directory"},
		{"the enrollment file is empty", func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte("\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, "the enrollment file in " + machineCacheWords + " is empty"},
	} {
		t.Run(row.name, func(t *testing.T) {
			dir := useCache(t)
			row.write(t, filepath.Join(dir, enrollmentFile))
			stdout, stderr, code := runIn(signArgs...)
			if code != 1 {
				t.Fatalf("exit %d, stderr %q; want 1", code, stderr)
			}
			wantNamedNotPrinted(t, "the output", stdout+stderr, []string{row.want}, []string{secret}...)
		})
	}
	t.Run("a session the broker no longer holds, and no identity to register it again", func(t *testing.T) {
		useCache(t)
		_, _, err := machineReregister(context.Background(), "enr-machine")
		if err == nil {
			t.Fatal("the session was registered again with no identity to register it under")
		}
		wantNamedNotPrinted(t, "the error", err.Error(), []string{"this machine's session in " + machineCacheWords + " is not live"}, []string{secret}...)
	})
	t.Run("an enrollment file that cannot be put in place", func(t *testing.T) {
		dir := useCache(t)
		path := filepath.Join(dir, enrollmentFile)
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(path, "taken"), 0o700); err != nil {
			t.Fatal(err)
		}
		key, err := proof.NewKey()
		if err != nil {
			t.Fatal(err)
		}
		err = writeMachineCache(dir, key, "enr-machine")
		if err == nil {
			t.Fatal("writeMachineCache put an enrollment where a directory is")
		}
		wantNamedNotPrinted(t, "the error", err.Error(), []string{"write the enrollment file in " + machineCacheWords + ": rename: "}, []string{secret}...)
	})
	t.Run("the cache's lock cannot be opened", func(t *testing.T) {
		dir := useCache(t)
		if err := os.Mkdir(filepath.Join(dir, machineCacheLock), 0o700); err != nil {
			t.Fatal(err)
		}
		stdout, stderr, code := runIn(signArgs...)
		if code != 1 {
			t.Fatalf("exit %d, stderr %q; want 1", code, stderr)
		}
		wantNamedNotPrinted(t, "the output", stdout+stderr, []string{"open a lock in " + machineCacheWords + ": open: is a directory"}, []string{secret}...)
	})
	t.Run("the kept Google audience cannot be read", func(t *testing.T) {
		dir := useCache(t)
		if err := os.Mkdir(filepath.Join(dir, googleAudienceFile), 0o700); err != nil {
			t.Fatal(err)
		}
		err := checkGoogleAudience(brokeraddr.New("https://broker.example"), "agent-secrets-vm")
		if err == nil {
			t.Fatal("checkGoogleAudience read an audience from a directory")
		}
		wantNamedNotPrinted(t, "the error", err.Error(), []string{"read google-exchange-audience in " + machineCacheWords + ": read: is a directory"}, []string{secret}...)
	})
	t.Run("the Google audience cannot be kept", func(t *testing.T) {
		dir := useCache(t)
		if err := os.Mkdir(filepath.Join(dir, googleAudienceFile+".lock"), 0o700); err != nil {
			t.Fatal(err)
		}
		err := keepGoogleAudience(brokeraddr.New("https://broker.example"), "agent-secrets-vm")
		if err == nil {
			t.Fatal("keepGoogleAudience kept an audience under a lock it could not open")
		}
		wantNamedNotPrinted(t, "the error", err.Error(), []string{"keep the broker's Google exchange audience in google-exchange-audience in " + machineCacheWords + ": open: is a directory"}, []string{secret}...)
	})
	t.Run("another Google audience than the one kept", func(t *testing.T) {
		useCache(t)
		base := brokeraddr.New("https://broker.example")
		if err := keepGoogleAudience(base, "agent-secrets-vm"); err != nil {
			t.Fatal(err)
		}
		err := checkGoogleAudience(base, "another-audience")
		if err == nil {
			t.Fatal("checkGoogleAudience passed an audience other than the one kept")
		}
		wantNamedNotPrinted(t, "the error", err.Error(), []string{"(kept in google-exchange-audience in " + machineCacheWords + ")", "remove google-exchange-audience in " + machineCacheWords + " and run"}, []string{secret}...)
	})
	t.Run("control: a kept Google audience the broker still names", func(t *testing.T) {
		useCache(t)
		base := brokeraddr.New("https://broker.example")
		for range 2 {
			if err := keepGoogleAudience(base, "agent-secrets-vm"); err != nil {
				t.Fatal(err)
			}
			if err := checkGoogleAudience(base, "agent-secrets-vm"); err != nil {
				t.Fatal(err)
			}
		}
	})
}

// TestTheSignInStoreIsNamedByItsSettingNeverItsPath: the sign-in store holds each person's refresh
// token and box key, so the chain's refusal of a machine nobody has signed in on names the store
// as internal/brokerclient names it, by AGENT_SECRETS_SIGNIN_DIR or its default's expression, never
// by the directory, whether the variable names it or XDG_STATE_HOME places it. A store that holds a
// sign-in still signs (the control).
func TestTheSignInStoreIsNamedByItsSettingNeverItsPath(t *testing.T) {
	const secret = "hunter2pw-signindir-5b08e3"
	const store = "the sign-in store (AGENT_SECRETS_SIGNIN_DIR, or $XDG_STATE_HOME/agent-secrets/signins where it is unset)"
	for _, row := range []struct {
		name  string
		place func(t *testing.T)
	}{
		{"AGENT_SECRETS_SIGNIN_DIR names it", func(t *testing.T) {
			t.Setenv("AGENT_SECRETS_SIGNIN_DIR", filepath.Join(t.TempDir(), secret))
		}},
		{"XDG_STATE_HOME places it", func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), secret))
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			useBareEnvironment(t)
			row.place(t)
			stdout, stderr, code := runIn(signArgs...)
			if code != 1 {
				t.Fatalf("exit %d, stdout %q, stderr %q; want 1", code, stdout, stderr)
			}
			wantNamedNotPrinted(t, "the output", stdout+stderr, []string{"sign-in: nobody has signed in on this machine with `agent-secrets login`", store}, []string{secret}...)
		})
	}
	t.Run("control: a store that holds a sign-in", func(t *testing.T) {
		useBareEnvironment(t)
		t.Setenv("AGENT_SECRETS_SIGNIN_DIR", filepath.Join(t.TempDir(), secret))
		key, err := proof.NewKey()
		if err != nil {
			t.Fatal(err)
		}
		thumbprint, err := proof.Thumbprint(&key.PublicKey)
		if err != nil {
			t.Fatal(err)
		}
		if err := brokerclient.SignIns(os.Getenv).Write(brokerclient.SignInRecord{
			Person: "ada@example.com", Issuer: "https://accounts.google.com", ClientID: "device-client", RefreshToken: "not-a-secret-refresh-token",
			BoxKey: string(keyPEM(key)), Thumbprint: thumbprint, EnrollmentID: "signin-enrollment", SignedInAt: time.Now().UTC(),
		}); err != nil {
			t.Fatal(err)
		}
		stdout, stderr, code := runIn(signArgs...)
		if code != 0 || strings.Count(strings.TrimSpace(stdout), ".") != 2 {
			t.Fatalf("exit %d, stdout %q, stderr %q; want 0 and a compact JWS", code, stdout, stderr)
		}
	})
}

// wantUnexpectedArgument is the refusal of an argument a form does not take, as every form prints
// it, written out by hand.
const wantUnexpectedArgument = "unexpected argument (not printed: an argument this form does not take may be a credential pasted in the wrong place)"

// TestAnUnexpectedArgumentIsNotPrinted: a form that takes no such argument refuses it, exit 2,
// without quoting it: an argument a form does not take is where a credential pasted in the wrong
// place lands. Each form run without it goes on past the check (the control).
func TestAnUnexpectedArgumentIsNotPrinted(t *testing.T) {
	const secret = "hunter2pw-positional-4e7a19"
	for _, form := range [][]string{
		{"grant", "list"}, {"identity"}, {"login"}, {"keygen"}, {"self"}, {"whoami"}, {"register"}, {"secret", "list"}, {"sign"},
	} {
		name := strings.Join(form, " ")
		t.Run(name, func(t *testing.T) {
			useBareEnvironment(t)
			t.Setenv("AGENT_SECRETS_HELPER_SOCK", filepath.Join(t.TempDir(), "none.sock"))
			stdout, stderr, code := runIn(append(append([]string{}, form...), secret)...)
			if code != exitUsageError {
				t.Fatalf("exit %d, stderr %q; want %d", code, stderr, exitUsageError)
			}
			wantNamedNotPrinted(t, "the output", stdout+stderr, []string{"agent-secrets " + name + ": " + wantUnexpectedArgument}, []string{secret}...)
		})
		if name == "register" {
			continue // register with nothing after it waits on the helper; its row above is the check
		}
		t.Run("control: "+name+" with no such argument", func(t *testing.T) {
			useBareEnvironment(t)
			t.Setenv("AGENT_SECRETS_HELPER_SOCK", filepath.Join(t.TempDir(), "none.sock"))
			stdout, stderr, _ := runIn(form...)
			if strings.Contains(stdout+stderr, "unexpected argument") {
				t.Errorf("the output %q refuses an argument it was not given", stdout+stderr)
			}
		})
	}
}

// TestASettingsFileKeyItDoesNotTakeIsNotPrinted: a line of the settings file whose key the file
// does not take is refused naming the file and the line, never the key: a token pasted into the
// file as a line of its own reads as a key up to its first '='. A line whose key the file takes is
// still read (the control).
func TestASettingsFileKeyItDoesNotTakeIsNotPrinted(t *testing.T) {
	const secret = "eyJhbGciOiJSUzI1NiJ9.hunter2pw-settingsline-0b6c33"
	for _, line := range []string{secret + "==", secret + "=x"} {
		t.Run(line, func(t *testing.T) {
			path := writeClientConfig(t, "# settings\n\n"+line+"\n")
			stdout, stderr, code := runIn("identity")
			if code != exitUsageError {
				t.Fatalf("exit %d, stderr %q; want %d", code, stderr, exitUsageError)
			}
			wantNamedNotPrinted(t, "the output", stdout+stderr, []string{path + " line 3 sets a key this file does not take (not printed)"}, []string{"hunter2pw-settingsline"}...)
		})
	}
	t.Run("control: a key the file takes", func(t *testing.T) {
		unsetenv(t, "AGENT_SECRETS_URL")
		writeClientConfig(t, "AGENT_SECRETS_URL=https://secrets.example\n")
		if _, stderr, code := runIn("identity"); code == exitUsageError {
			t.Fatalf("exit %d, stderr %q: a key the file takes was refused", code, stderr)
		}
		if got := clientGetenv("AGENT_SECRETS_URL"); got != "https://secrets.example" {
			t.Fatalf("the file's AGENT_SECRETS_URL read as %q", got)
		}
	})
}

// TestAnIdThatCannotBeInAPathIsNotPrinted: an id a form would put in a call's path, which a URL
// path would change, is refused before any call naming the kind of id and never the argument, which
// is where a credential pasted in the wrong place lands. An id that can be in a path goes on to the
// call (the control).
func TestAnIdThatCannotBeInAPathIsNotPrinted(t *testing.T) {
	const secret = "hunter2pw/pathid-91de07"
	for _, row := range []struct {
		args []string
		kind string
	}{
		{[]string{"status", secret}, "a request id"},
		{[]string{"cancel", secret}, "a request id"},
		{[]string{"revoke", secret}, "a grant id"},
		{[]string{"machine", "revoke", secret}, "a machine id"},
		{[]string{"grant", "revoke", secret}, "a grant id"},
	} {
		for _, id := range []string{secret, "plain-id-0001"} {
			args := append(append([]string{}, row.args[:len(row.args)-1]...), id)
			name := strings.Join(args, " ")
			if id != secret {
				name = "control: " + name
			}
			t.Run(name, func(t *testing.T) {
				useBareEnvironment(t)
				t.Setenv("AGENT_SECRETS_URL", "http://127.0.0.1:1")
				t.Setenv("AGENT_SECRETS_KEY_DIR", newKeyDir(t))
				sock := filepath.Join(t.TempDir(), "helper.sock")
				if err := os.WriteFile(sock, nil, 0o600); err != nil {
					t.Fatal(err)
				}
				t.Setenv("AGENT_SECRETS_HELPER_SOCK", sock)
				stdout, stderr, code := runIn(args...)
				if code == 0 {
					t.Fatalf("exit 0, stdout %q; want a failure", stdout)
				}
				refusal := "not " + row.kind + " (the argument is not printed: it holds a character a call's path would change)"
				if id != secret {
					if strings.Contains(stderr, "not "+row.kind) {
						t.Errorf("stderr %q refuses an id that can be in a path", stderr)
					}
					return
				}
				wantNamedNotPrinted(t, "the output", stdout+stderr, []string{refusal}, []string{"hunter2pw"}...)
			})
		}
	}
}

// TestAnUnknownFormIsNotPrinted: a group of forms - machine, grant, secret - refuses a form it does
// not have naming the forms it has, never the argument, which is where a credential pasted in the
// wrong place lands. A group given no form still says one is required (the control).
func TestAnUnknownFormIsNotPrinted(t *testing.T) {
	const secret = "hunter2pw-form-2a5c70"
	for _, row := range []struct{ group, forms string }{
		{"machine", "login, login-status, list, revoke"},
		{"grant", "list, revoke"},
		{"secret", "list, show, create, set, retag, delete, restore"},
	} {
		t.Run(row.group, func(t *testing.T) {
			useBareEnvironment(t)
			stdout, stderr, code := runIn(row.group, secret)
			if code != exitUsageError {
				t.Fatalf("exit %d, stderr %q; want %d", code, stderr, exitUsageError)
			}
			wantNamedNotPrinted(t, "the output", stdout+stderr, []string{"agent-secrets " + row.group + ": unknown form (not printed); the forms are " + row.forms}, []string{secret}...)
		})
		t.Run("control: "+row.group+" with no form", func(t *testing.T) {
			useBareEnvironment(t)
			_, stderr, code := runIn(row.group)
			if code != exitUsageError || !strings.Contains(stderr, "agent-secrets "+row.group+": a form is required: "+row.forms) {
				t.Fatalf("exit %d, stderr %q; want %d naming the forms", code, stderr, exitUsageError)
			}
		})
	}
}

// useUnreachableBroker leaves the rest of t with a bare environment whose broker is url, a key dir
// that holds a session to sign with, and a helper that signs every call this machine's login makes.
func useUnreachableBroker(t *testing.T, url string) {
	t.Helper()
	useBareEnvironment(t)
	t.Setenv("AGENT_SECRETS_URL", url)
	t.Setenv("AGENT_SECRETS_KEY_DIR", newKeyDir(t))
	sock, _ := fakeHelper(t, helper.Response{OK: true, Proof: "eyJ.fake.proof"})
	t.Setenv("AGENT_SECRETS_HELPER_SOCK", sock)
}

// TestAnIdAPersonTypedIsLeftOutOfAFailedCallsRoute: a form that puts an id the person typed in a
// call's path, and whose call fails before the broker answers it - nothing answers, or the answer
// is cut off - names the route with the id left out, as `(the machine id, not printed)`: the
// broker never accepted the id, and an argument that is no id is where a credential pasted in the
// wrong place lands. A call whose path holds no typed id still prints its URL whole (the control),
// and the failure still classifies as it did (errors.Is).
func TestAnIdAPersonTypedIsLeftOutOfAFailedCallsRoute(t *testing.T) {
	const secret = "hunter2pw-typedid-60b1e4"
	forms := []struct {
		args  []string
		route string
	}{
		{[]string{"status", secret}, "GET %s/v1/requests/(the request id, not printed)"},
		{[]string{"cancel", secret}, "POST %s/v1/requests/(the request id, not printed)/cancel"},
		{[]string{"revoke", secret}, "POST %s/v1/grants/(the grant id, not printed)/revoke"},
		{[]string{"machine", "revoke", secret}, "POST %s/v1/operator/machines/(the machine id, not printed)/revoke"},
		// In a session (the key dir here holds one), grant revoke is the session's own revoke.
		{[]string{"grant", "revoke", secret}, "POST %s/v1/grants/(the grant id, not printed)/revoke"},
	}
	cutOff := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "4096")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"partial":`))
	}))
	t.Cleanup(cutOff.Close)
	for _, broker := range []struct{ name, url, says string }{
		{"nothing answers", "http://127.0.0.1:1", "the connection was refused"},
		{"the answer is cut off", cutOff.URL, "read response"},
	} {
		for _, form := range forms {
			t.Run(broker.name+", "+strings.Join(form.args[:len(form.args)-1], " "), func(t *testing.T) {
				useUnreachableBroker(t, broker.url)
				stdout, stderr, code := runIn(form.args...)
				if code == 0 {
					t.Fatalf("exit 0, stdout %q; want a failure", stdout)
				}
				wantNamedNotPrinted(t, "the output", stdout+stderr, []string{fmt.Sprintf(form.route, broker.url), broker.says}, []string{secret}...)
			})
		}
	}
	t.Run("control: a call whose path holds no typed id", func(t *testing.T) {
		useUnreachableBroker(t, "http://127.0.0.1:1")
		_, stderr, code := runIn("grant", "list")
		if code == 0 || !strings.Contains(stderr, "GET http://127.0.0.1:1/v1/operator/grants") || strings.Contains(stderr, "not printed)") {
			t.Fatalf("exit %d, stderr %q; want a failure naming the call's URL whole", code, stderr)
		}
	})
	t.Run("the failure still classifies", func(t *testing.T) {
		key, err := proof.NewKey()
		if err != nil {
			t.Fatal(err)
		}
		c := newClient(brokeraddr.New("http://127.0.0.1:1"))
		signer := &fileSigner{key: key, enrollmentID: "enr-test"}
		for route, err := range map[string]error{
			"POST http://127.0.0.1:1/v1/operator/machines/(the machine id, not printed)/revoke": c.RevokeOperatorMachine(context.Background(), signer, secret),
			"POST http://127.0.0.1:1/v1/operator/grants/(the grant id, not printed)/revoke":     c.RevokeOperatorGrant(context.Background(), signer, secret),
		} {
			var unreachable *unreachableError
			if !errors.Is(err, syscall.ECONNREFUSED) || !errors.As(err, &unreachable) {
				t.Fatalf("%s = %v; want an unreachableError that is ECONNREFUSED", route, err)
			}
			wantNamedNotPrinted(t, "the error", err.Error(), []string{route}, []string{secret}...)
		}
	})
}

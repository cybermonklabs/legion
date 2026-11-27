// packages/envoy/cmd/cipher/keydir_printed_test.go
//
// What the forms print about a file that holds a session's key or a person's sign-in: key.pem in
// the key dir or the machine cache, the files beside it, keygen's --out, the machine cache's move
// from the key dir, and the sign-in store. Each is named by the setting that names its directory,
// or by where it is, never by its path, which is where a person who pasted the credential itself
// puts it. Each test runs a form in-process (runIn), or the function that words the line where no
// form reaches it alone (keyDirName, lockFile, keepGoogleAudience), with a made-up secret where a
// person could put one, and a control row that shows the clean case still works.
package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/cipheraddr"
	"github.com/sjawhar/envoy/internal/cipher/proof"
	"github.com/sjawhar/envoy/internal/cipherclient"
)

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
	// Not parallel: it sets environment variables of the whole process (t.Setenv at keydir_printed_test.go:83); it signals the test process or changes its signal handling (main.runIn, main.run, main.cmdLogin, signal.NotifyContext at login.go:48); it sets a package variable the package's other tests read (main.useBareEnvironment, passwdHome set at cli_printed_test.go:51).
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
	// Not parallel: it sets environment variables of the whole process (t.Setenv at keydir_printed_test.go:140); it signals the test process or changes its signal handling (main.runIn, main.run, main.cmdLogin, signal.NotifyContext at login.go:48); it sets a package variable the package's other tests read (main.useBareEnvironment, passwdHome set at cli_printed_test.go:51).
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
	// Not parallel: it sets environment variables of the whole process (t.Setenv at keydir_printed_test.go:190); it signals the test process or changes its signal handling (main.runIn, main.run, main.cmdLogin, signal.NotifyContext at login.go:48); it sets a package variable the package's other tests read (main.useBareEnvironment, passwdHome set at cli_printed_test.go:51); it holds code to an upper bound on a measured time, which the load of the package's parallel tests could push past (main.requireOnTime, onTimeWithin at waits_test.go:239).
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
		meter := startStallMeter(t)
		started := time.Now()
		stdout, stderr, code := runIn(signArgs...)
		// A cached session signs with no network call, so the sign waits for nothing: it is timed
		// against no deadline, allowing for the time this process was held up (requireOnTime).
		requireOnTime(t, meter, "the sign of a cached session, which makes no network call", started, 0)
		if code != 0 || strings.Count(strings.TrimSpace(stdout), ".") != 2 {
			t.Fatalf("exit %d, stdout %q, stderr %q; want 0 and a compact JWS", code, stdout, stderr)
		}
	})
}

// machineCacheWords is how every line names the machine cache, written out by hand.
const machineCacheWords = "the machine cache (${XDG_STATE_HOME:-$HOME/.local/state}/agent-secrets/machine)"

// TestTheKeyDirIsNamedByItsSettingNeverItsPath: the key dir holds key.pem, so no line about it, or
// about a file beside key.pem, spells its path: the chain's refusal of a key dir with no key.pem,
// an enrollment file that cannot be read or is empty, a launcher's enrollment.error, a wait on a
// pending marker nothing follows (the line it says as it starts to wait, which it says before it
// reads its deadline, and its refusal), and the source `identity --source` names each say
// AGENT_SECRETS_KEY_DIR, or its default's own expression where the variable is unset, with the
// system's reason where a file call failed. What a launcher wrote in enrollment.error still
// prints, and a key dir that holds a session still signs (the controls).
func TestTheKeyDirIsNamedByItsSettingNeverItsPath(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (t.Setenv at keydir_printed_test.go:315); it signals the test process or changes its signal handling (main.runIn, main.run, main.cmdLogin, signal.NotifyContext at login.go:48); it sets a package variable the package's other tests read (main.useBareEnvironment, passwdHome set at cli_printed_test.go:51).
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
			want: []string{launcherWaitLine("AGENT_SECRETS_KEY_DIR", "100ms"), ", its bound 100ms, while AGENT_SECRETS_KEY_DIR held enrollment.pending)"}},
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
	// Not parallel: it sets environment variables of the whole process (t.Setenv at keydir_printed_test.go:346).
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
	// Not parallel: it sets environment variables of the whole process (t.Setenv at keydir_printed_test.go:399); it signals the test process or changes its signal handling (main.runIn, main.run, main.cmdLogin, signal.NotifyContext at login.go:48); it sets a package variable the package's other tests read (main.useBareEnvironment, passwdHome set at cli_printed_test.go:51).
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
		// moves is a row whose move can be made, so the command signs and the session is in the
		// cache.
		moves bool
	}{
		{"the cache's key.pem cannot be put in place", func(t *testing.T, _, cache string) {
			blockWithADirectory(t, filepath.Join(cache, keyFile))
		}, []string{"move this machine's session from AGENT_SECRETS_KEY_DIR to " + machineCacheWords + ": rename: "}, false},
		{"the key dir's copy cannot be removed", func(t *testing.T, keyDir, cache string) {
			writeSessionFiles(t, cache, "enr-cache")
			blockWithADirectory(t, filepath.Join(keyDir, keyFile))
		}, []string{"remove the machine session AGENT_SECRETS_KEY_DIR held, which " + machineCacheWords + " replaces: remove: directory not empty"}, false},
		{"the marker cannot be removed", func(t *testing.T, keyDir, _ string) {
			blockWithADirectory(t, filepath.Join(keyDir, machineSessionMarker))
		}, []string{"remove the machine-session marker in AGENT_SECRETS_KEY_DIR: remove: directory not empty"}, false},
		{"the cache cannot be made", func(t *testing.T, _, cache string) {
			if err := os.WriteFile(filepath.Dir(cache), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}, []string{"make " + machineCacheWords + ": mkdir: not a directory"}, false},
		{"no directory for the cache", func(t *testing.T, _, _ string) {
			t.Setenv("XDG_STATE_HOME", "")
			t.Setenv("HOME", "")
		}, []string{"AGENT_SECRETS_KEY_DIR holds a machine session to move to the machine cache, and neither XDG_STATE_HOME nor HOME names a directory for it, nor does the password database give this user a home"}, false},
		{"control: a move that can be made", func(*testing.T, string, string) {}, []string{"."}, true},
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
			if want := map[bool]int{true: 0, false: 1}[row.moves]; code != want {
				t.Fatalf("exit %d, stdout %q, stderr %q; want %d", code, stdout, stderr, want)
			}
			wantNamedNotPrinted(t, "the output", stdout+stderr, row.want, []string{secret}...)
			if row.moves && (!machineCacheHolds(cache) || exists(filepath.Join(keyDir, keyFile))) {
				t.Fatal("the move left the session where it was")
			}
		})
	}
}

// TestTheMachineCacheIsNamedByWhereItIsNeverItsPath: the machine cache holds key.pem, so the source
// `identity --source` names, an enrollment file there that cannot be read or is empty, a lock
// there that cannot be opened, and a cached session the broker no longer holds with no identity to
// register it again each name the cache by where it is, never by its path. A key.pem there that
// is no PEM file is TestTheMachineCacheKeyIsNamedByTheCacheNeverItsPath's (keyPemFaults).
func TestTheMachineCacheIsNamedByWhereItIsNeverItsPath(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (t.Setenv at keydir_printed_test.go:442); it signals the test process or changes its signal handling (main.runIn, main.run, main.cmdLogin, signal.NotifyContext at login.go:48); it sets a package variable the package's other tests read (main.useBareEnvironment, passwdHome set at cli_printed_test.go:51).
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
		file  string
		write func(t *testing.T, path string)
		want  string
	}{
		{"the enrollment file is a directory", enrollmentFile, func(t *testing.T, path string) {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
		}, "read the enrollment file in " + machineCacheWords + ": read: is a directory"},
		{"the enrollment file is empty", enrollmentFile, func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte("\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, "the enrollment file in " + machineCacheWords + " is empty"},
	} {
		t.Run(row.name, func(t *testing.T) {
			dir := useCache(t)
			row.write(t, filepath.Join(dir, row.file))
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
		err := checkGoogleAudience(cipheraddr.New("https://broker.example"), "agent-secrets-vm")
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
		err := keepGoogleAudience(cipheraddr.New("https://broker.example"), "agent-secrets-vm")
		if err == nil {
			t.Fatal("keepGoogleAudience kept an audience under a lock it could not open")
		}
		wantNamedNotPrinted(t, "the error", err.Error(), []string{"keep the broker's Google exchange audience in google-exchange-audience in " + machineCacheWords + ": open: is a directory"}, []string{secret}...)
	})
	t.Run("another Google audience than the one kept", func(t *testing.T) {
		useCache(t)
		base := cipheraddr.New("https://broker.example")
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
		base := cipheraddr.New("https://broker.example")
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
// as internal/cipherclient names it, by AGENT_SECRETS_SIGNIN_DIR or its default's expression, never
// by the directory, whether the variable names it or XDG_STATE_HOME places it. A store that holds a
// sign-in still signs (the control).
func TestTheSignInStoreIsNamedByItsSettingNeverItsPath(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (t.Setenv at keydir_printed_test.go:585); it signals the test process or changes its signal handling (main.runIn, main.run, main.cmdLogin, signal.NotifyContext at login.go:48); it sets a package variable the package's other tests read (main.useBareEnvironment, passwdHome set at cli_printed_test.go:51).
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
		if err := cipherclient.SignIns(os.Getenv).Write(cipherclient.SignInRecord{
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

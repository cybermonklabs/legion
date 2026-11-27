// packages/envoy/cmd/cipher/proxy_trust_key_test.go
//
// The key a grant's directory name is keyed with (proxyTrustDirKey): no name, trust variable or
// printed line holds the digits a plain hash of the trust gives, so none lets a reader test a guess
// of a password the inherited wgetrc holds; runs read back the key their root holds, so runs of one
// trust share one directory; and a key file this CLI did not write is refused, not used.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// unkeyedTrustDigest is the hex digits a plain SHA-256 of the trust would give the directory name:
// what proxyTrustDirName hashes (writeProxyTrustDirInput: the bundle the command's clients read
// and the inherited wgetrc lines), but with no key. All of it but the wgetrc lines is public or
// printed beside the name, so a name holding these digits would let a reader test guesses of a
// password in those lines.
func unkeyedTrustDigest(environ []string, caPEM string) string {
	h := sha256.New()
	writeProxyTrustDirInput(h, trustBundle(environ, caPEM), commandWgetrcLines(environ))
	return hex.EncodeToString(h.Sum(nil))[:proxyTrustDirHashLen]
}

// TestProxyTrustDirNameLetsNoPrintedNameTestAPasswordGuess: the directory's name leaves the root,
// in every trust variable the command gets and in a refusal that names the directory, while the
// inherited wgetrc it is named for can hold http_password and the rest of what it is named for is
// public. So the name is keyed: neither it, the command's variables nor what the run prints holds
// the digits a plain hash of the same trust gives. The wgetrc lines are checked to hold the
// planted password first, so the plain hash is one a guess of that password would reproduce.
func TestProxyTrustDirNameLetsNoPrintedNameTestAPasswordGuess(t *testing.T) {
	wgetrc, secrets := inheritedWgetrc(t)
	environ := []string{"XDG_RUNTIME_DIR=" + t.TempDir(), wgetrc}
	release := testRelease("AIRLOCK_ME", []string{"api.example.com"}, nil)
	if lines := commandWgetrcLines(environ); !bytes.Contains(lines, []byte(secrets[1])) {
		t.Fatalf("the inherited wgetrc lines %q do not hold the planted password, so nothing here tests a guess of it", lines)
	}
	plain := unkeyedTrustDigest(environ, release.CACertificate)

	var stderr strings.Builder
	files, err := prepareProxyTrustForCommand(environ, nil, release, "grant-one", nil, &stderr)
	if err != nil {
		t.Fatalf("prepareProxyTrustForCommand: %v", err)
	}
	if files.bundle == "" {
		t.Fatal("the run wrote no bundle, so it named no directory to check")
	}
	vars, err := airlockVars(environ, release, files, releaseNames(nil, release))
	if err != nil {
		t.Fatalf("airlockVars: %v", err)
	}
	seen := map[string]string{"the directory name": filepath.Base(filepath.Dir(files.bundle)), "stderr": stderr.String()}
	for key, value := range vars {
		seen["the command's "+key] = value
	}
	for what, text := range seen {
		if strings.Contains(text, plain) {
			t.Errorf("%s holds %s, the plain SHA-256 of the trust, which a guess of the wgetrc's password can reproduce: %q", what, plain, text)
		}
	}
}

// TestProxyTrustDirNameTellsTheBundleFromTheWgetrc: the name is of two inputs, the bundle and the
// wgetrc lines, and a trust file a command names can hold any byte, a zero among them. So the
// bundle's length is part of what is hashed: a bundle that runs on into what is another command's
// wgetrc is other trust, and gets another name. One pair of rows splits the same bytes at another
// point, which no framing at all tells apart; the other puts a zero byte where a separator would
// go, which a zero byte between the two inputs does not tell apart. The control: the same pair
// takes the same name.
func TestProxyTrustDirNameTellsTheBundleFromTheWgetrc(t *testing.T) {
	key := bytes.Repeat([]byte{0x5a}, proxyTrustKeyLen)
	for _, pair := range [][2][2]string{
		{{"ab", "c"}, {"a", "bc"}},
		{{"a", "b\x00c"}, {"a\x00b", "c"}},
	} {
		first := proxyTrustDirName(key, "grant-one", []byte(pair[0][0]), []byte(pair[0][1]))
		if other := proxyTrustDirName(key, "grant-one", []byte(pair[1][0]), []byte(pair[1][1])); other == first {
			t.Errorf("the bundle %q with the wgetrc %q and the bundle %q with the wgetrc %q both take %s", pair[0][0], pair[0][1], pair[1][0], pair[1][1], first)
		}
		if again := proxyTrustDirName(key, "grant-one", []byte(pair[0][0]), []byte(pair[0][1])); again != first {
			t.Errorf("the same trust took %s and then %s", first, again)
		}
	}
}

// TestProxyTrustDirKeyIsTheKeyItsRootHolds: a run reads the key its root holds, each run in a
// process of its own, so two runs of one trust share one directory and the key stays as it was. A
// root with no key gets one this user alone can read, which names the same trust otherwise than
// another root's key does: the name follows the key, not the trust alone.
func TestProxyTrustDirKeyIsTheKeyItsRootHolds(t *testing.T) {
	release := testRelease("AIRLOCK_ME", nil, nil)
	planted := bytes.Repeat([]byte{0x5a}, proxyTrustKeyLen)
	runtimeDir := t.TempDir()
	environ := []string{"XDG_RUNTIME_DIR=" + runtimeDir}
	root := filepath.Join(runtimeDir, "agent-secrets", "airlock")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, proxyTrustKeyFile), planted, 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(t *testing.T, environ []string) airlockFiles {
		t.Helper()
		files, err := prepareAirlock(environ, release, "grant-one", nil)
		if err != nil {
			t.Fatalf("prepareAirlock: %v", err)
		}
		return files
	}

	first := run(t, environ)
	want := proxyTrustDirName(planted, "grant-one", trustBundle(environ, release.CACertificate), commandWgetrcLines(environ))
	if got := filepath.Base(filepath.Dir(first.bundle)); got != want {
		t.Errorf("the directory is %s, want %s, the name the root's key gives this trust", got, want)
	}
	if second := run(t, environ); filepath.Dir(second.bundle) != filepath.Dir(first.bundle) {
		t.Errorf("a second run of the same trust got %s, want the first run's directory %s", filepath.Dir(second.bundle), filepath.Dir(first.bundle))
	}
	if data, err := os.ReadFile(filepath.Join(root, proxyTrustKeyFile)); err != nil || !bytes.Equal(data, planted) {
		t.Errorf("the root's key changed or went (read error %v)", err)
	}

	otherDir := t.TempDir()
	other := []string{"XDG_RUNTIME_DIR=" + otherDir}
	made := run(t, other)
	keyPath := filepath.Join(otherDir, "agent-secrets", "airlock", proxyTrustKeyFile)
	info, err := os.Lstat(keyPath)
	if err != nil {
		t.Fatalf("a root with no key got none: %v", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() != proxyTrustKeyLen {
		t.Errorf("the key made is %v of %d bytes, want a %d-byte regular file only this user can read", info.Mode(), info.Size(), proxyTrustKeyLen)
	}
	if filepath.Base(filepath.Dir(made.bundle)) == want {
		t.Errorf("a root with a key of its own named the same trust %s, as the planted key does", want)
	}
	if again := run(t, other); filepath.Dir(again.bundle) != filepath.Dir(made.bundle) {
		t.Errorf("a second run under the root that made its key got %s, want %s", filepath.Dir(again.bundle), filepath.Dir(made.bundle))
	}
}

// TestProxyTrustDirKeyRefusesAKeyItDidNotWrite: a key file this CLI did not write - one its group or
// others can read, one of another length, a directory, or a symbolic link - is refused with its
// path, and nothing is built on it: no grant directory is made and the file stays as it is. The
// rule is checkAirlockRoot's, so the controls, a 32-byte file of mode 0600 and one of mode 0400,
// are used.
func TestProxyTrustDirKeyRefusesAKeyItDidNotWrite(t *testing.T) {
	good := bytes.Repeat([]byte{0x5a}, proxyTrustKeyLen)
	elsewhere := writeLoginFile(t, "key", string(good))
	for _, row := range []struct {
		name    string
		plant   func(t *testing.T, path string)
		refused bool
	}{
		{"readable by others", writeKey(good, 0o644), true},
		{"31 bytes", writeKey(good[:proxyTrustKeyLen-1], 0o600), true},
		{"33 bytes", writeKey(append(bytes.Clone(good), 0x5a), 0o600), true},
		{"a directory", func(t *testing.T, path string) {
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"a symbolic link to a good key", func(t *testing.T, path string) {
			if err := os.Symlink(elsewhere, path); err != nil {
				t.Skipf("no symbolic link here: %v", err)
			}
		}, true},
		{"control: 32 bytes of mode 0600", writeKey(good, 0o600), false},
		{"control: 32 bytes of mode 0400", writeKey(good, 0o400), false},
	} {
		t.Run(row.name, func(t *testing.T) {
			runtimeDir := t.TempDir()
			root := filepath.Join(runtimeDir, "agent-secrets", "airlock")
			if err := os.MkdirAll(root, 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, proxyTrustKeyFile)
			row.plant(t, path)
			before, _ := os.Lstat(path)

			_, err := prepareAirlock([]string{"XDG_RUNTIME_DIR=" + runtimeDir}, testRelease("AIRLOCK_ME", nil, nil), "grant-one", nil)
			if !row.refused {
				if err != nil {
					t.Fatalf("prepareAirlock refused a key this CLI could have written: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("prepareAirlock used a key this CLI did not write")
			}
			if !strings.Contains(err.Error(), "the airlock key "+path) {
				t.Errorf("the refusal %q does not name the key %s", err, path)
			}
			entries, _ := os.ReadDir(root)
			for _, entry := range entries {
				if _, ok := proxyTrustDirGrant(entry.Name()); ok {
					t.Errorf("the refused run made the grant directory %s", entry.Name())
				}
			}
			if after, err := os.Lstat(path); err != nil || after.Mode() != before.Mode() || after.Size() != before.Size() {
				t.Errorf("the refused key changed or went (stat error %v)", err)
			}
		})
	}
}

// writeKey plants data at a test's key path with mode perm, which os.WriteFile's umask would not
// widen but could narrow, so the mode is set after the write.
func writeKey(data []byte, perm os.FileMode) func(t *testing.T, path string) {
	return func(t *testing.T, path string) {
		t.Helper()
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, perm); err != nil {
			t.Fatal(err)
		}
	}
}

// TestProxyTrustDirKeyRefusesAKeyAnotherUserOwns: the root is this user's alone, so a key file
// another user owns is one this CLI did not write, and it is refused with its path and its owner;
// the control, the same file owned by this user, is used. The owner is one a test cannot make,
// so fileOwner stands it in for the key file alone.
func TestProxyTrustDirKeyRefusesAKeyAnotherUserOwns(t *testing.T) {
	stranger := os.Getuid() + 4242
	for _, row := range []struct {
		name    string
		owner   int
		refused bool
	}{
		{"owned by another user", stranger, true},
		{"control: owned by this user", os.Getuid(), false},
	} {
		t.Run(row.name, func(t *testing.T) {
			runtimeDir := t.TempDir()
			root := filepath.Join(runtimeDir, "agent-secrets", "airlock")
			if err := os.MkdirAll(root, 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, proxyTrustKeyFile)
			writeKey(bytes.Repeat([]byte{0x5a}, proxyTrustKeyLen), 0o600)(t, path)
			original := fileOwner
			fileOwner = func(info os.FileInfo) (int, bool) {
				if info.Name() == proxyTrustKeyFile {
					return row.owner, true
				}
				return original(info)
			}
			t.Cleanup(func() { fileOwner = original })

			_, err := prepareAirlock([]string{"XDG_RUNTIME_DIR=" + runtimeDir}, testRelease("AIRLOCK_ME", nil, nil), "grant-one", nil)
			if !row.refused {
				if err != nil {
					t.Fatalf("prepareAirlock refused a key this user owns: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("prepareAirlock used a key another user owns")
			}
			if want := fmt.Sprintf("uid %d", stranger); !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal %q does not name the key %s and its owner (%s)", err, path, want)
			}
		})
	}
}

// TestProxyTrustDirKeyNamesTheReasonWhereTheRootTakesNoHardLink: the key is put in place with a hard
// link, which never replaces a key another run put there first. A root on a file system that takes
// no hard link (some FUSE mounts answer EPERM or ENOTSUP) gets a refusal that names the root, the
// reason and the way out, and leaves no key, no temporary key and no grant directory behind.
// proxyTrustKeyLink stands the file system in.
func TestProxyTrustDirKeyNamesTheReasonWhereTheRootTakesNoHardLink(t *testing.T) {
	for _, row := range []struct {
		name string
		err  error
	}{
		{"not supported", errors.ErrUnsupported},
		{"not permitted", fs.ErrPermission},
	} {
		t.Run(row.name, func(t *testing.T) {
			original := proxyTrustKeyLink
			proxyTrustKeyLink = func(oldname, newname string) error {
				return &os.LinkError{Op: "link", Old: oldname, New: newname, Err: row.err}
			}
			t.Cleanup(func() { proxyTrustKeyLink = original })
			runtimeDir := t.TempDir()
			root := filepath.Join(runtimeDir, "agent-secrets", "airlock")

			_, err := prepareAirlock([]string{"XDG_RUNTIME_DIR=" + runtimeDir}, testRelease("AIRLOCK_ME", nil, nil), "grant-one", nil)
			if err == nil {
				t.Fatal("prepareAirlock answered files with no key in place")
			}
			for _, want := range []string{root, "hard link", "XDG_RUNTIME_DIR", "TMPDIR"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal %q does not name %q", err, want)
				}
			}
			entries, _ := os.ReadDir(root)
			for _, entry := range entries {
				t.Errorf("the refused run left %s in the root", entry.Name())
			}
		})
	}
}

// TestTidyAirlockDirsSweepsAKeyLeftOverByAKilledRun: a run killed between writing its temporary
// key and linking it leaves that file in the root. The tidy removes one that has stood longer than
// a build can take, as it does a build directory, and leaves a young one, which may be a run in
// flight, the key itself, and a file it does not recognize.
func TestTidyAirlockDirsSweepsAKeyLeftOverByAKilledRun(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	long := now.Add(-2 * airlockBuildLifetime)
	plant := func(name string, at time.Time) string {
		t.Helper()
		path := filepath.Join(root, name)
		writeKey(bytes.Repeat([]byte{0x5a}, proxyTrustKeyLen), 0o600)(t, path)
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
		return path
	}
	left := plant(proxyTrustKeyTempPrefix+"123", long)
	young := plant(proxyTrustKeyTempPrefix+"456", now)
	key := plant(proxyTrustKeyFile, long)
	other := plant("something-else", long)

	tidyAirlockDirs(root, "grant-one", now)

	if _, err := os.Lstat(left); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the temporary key a killed run left is still there (stat error %v)", err)
	}
	for _, path := range []string{young, key, other} {
		if _, err := os.Lstat(path); err != nil {
			t.Errorf("%s is gone, want it kept: %v", filepath.Base(path), err)
		}
	}
}

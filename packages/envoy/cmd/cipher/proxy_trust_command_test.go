// packages/envoy/cmd/cipher/proxy_trust_command_test.go
//
// What the exec form's command gets from the airlock: the skip decision and the bundle read the
// environment the command runs with, released values included; a relative trust path is read from
// the directory the command starts in and named on stderr; and each trust the command's settings
// name gets its own directory under the grant, so a later command of the same grant never gets an
// earlier command's bundle.
package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// grantDirOf fails the test unless path is a file in one of grant's directories under root, and
// answers that directory.
func grantDirOf(t *testing.T, root, grant, path string) string {
	t.Helper()
	dir := filepath.Dir(path)
	if got, ok := proxyTrustDirGrant(filepath.Base(dir)); filepath.Dir(dir) != root || !ok || got != grant {
		t.Fatalf("%s is not in a directory of %s under %s", path, grant, root)
	}
	return dir
}

// TestPrepareProxyTrustForCommandDecidesOnTheReleasedValues: the exec form adds the released values to
// the command's environment after the inherited ones (buildChildEnv), so a released value of a
// trust setting, of HOME or of SSL_CERT_DIR is one the command's clients read. The decision reads
// that environment: such a value that takes a root away gets the bundle, where the inherited
// environment alone would have skipped it.
func TestPrepareProxyTrustForCommandDecidesOnTheReleasedValues(t *testing.T) {
	root, other := proxyTestRoot(t, "test proxy root"), proxyTestRoot(t, "test private authority")
	system := writeLoginFile(t, "ca-certificates.crt", root)
	useSystemTrustFiles(t, system)
	without := writeLoginFile(t, "private.pem", other)
	homeEmptyDir := filepath.Dir(writeLoginFile(t, ".wgetrc", "ca_directory =\n"))
	for _, row := range []struct {
		name       string
		released   map[string]string
		wantBundle bool
	}{
		{name: "nothing released beside the airlock name"},
		{name: "a released value of another name", released: map[string]string{"SOME_TOKEN": "a value"}},
		{name: "a released empty GIT_SSL_CAINFO", released: map[string]string{"GIT_SSL_CAINFO": ""}, wantBundle: true},
		{name: "a released CURL_CA_BUNDLE naming a file without the root", released: map[string]string{"CURL_CA_BUNDLE": without}, wantBundle: true},
		{name: "a released SSL_CERT_DIR", released: map[string]string{"SSL_CERT_DIR": t.TempDir()}, wantBundle: true},
		{name: "a released HOME whose .wgetrc has an empty ca_directory", released: map[string]string{"HOME": homeEmptyDir}, wantBundle: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			environ := append([]string{"XDG_RUNTIME_DIR=" + t.TempDir()}, proxyStoreSettings(t, system)...)
			release := testRelease("AIRLOCK_ME", []string{"api.example.com"}, nil)
			release.CACertificate = root
			var stderr strings.Builder
			files, err := prepareProxyTrustForCommand(environ, row.released, release, "grant-one", nil, &stderr)
			if err != nil {
				t.Fatalf("prepareProxyTrustForCommand: %v", err)
			}
			if got := files.bundle != ""; got != row.wantBundle {
				t.Errorf("prepareProxyTrustForCommand wrote a bundle: %v, want %v", got, row.wantBundle)
			}
		})
	}
}

// TestPrepareProxyTrustForCommandPlacesTheRootFromItsOwnEnvironment: the broker's released values
// reach the command's environment, and a released XDG_RUNTIME_DIR or TMPDIR is the broker's text,
// not agent-secrets' choice. So the root, where agent-secrets writes and tidies, comes from the
// environment agent-secrets inherited alone: the bundle goes under the inherited directory, and
// the released ones stay empty.
func TestPrepareProxyTrustForCommandPlacesTheRootFromItsOwnEnvironment(t *testing.T) {
	for _, row := range []struct {
		name      string
		inherited func(dir string) (environ []string, root string)
	}{
		{"an inherited XDG_RUNTIME_DIR", func(dir string) ([]string, string) {
			return []string{"XDG_RUNTIME_DIR=" + dir}, filepath.Join(dir, "agent-secrets", "airlock")
		}},
		{"an inherited TMPDIR and no XDG_RUNTIME_DIR", func(dir string) ([]string, string) {
			return []string{"TMPDIR=" + dir}, filepath.Join(dir, "agent-secrets", "airlock")
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			environ, root := row.inherited(t.TempDir())
			releasedRuntime, releasedTemp := t.TempDir(), t.TempDir()
			released := map[string]string{"XDG_RUNTIME_DIR": releasedRuntime, "TMPDIR": releasedTemp}
			var stderr strings.Builder
			files, err := prepareProxyTrustForCommand(environ, released, testRelease("AIRLOCK_ME", nil, nil), "grant-one", nil, &stderr)
			if err != nil {
				t.Fatalf("prepareProxyTrustForCommand: %v", err)
			}
			if files.bundle == "" || filepath.Dir(filepath.Dir(files.bundle)) != root {
				t.Errorf("the bundle is %q, want it in a grant directory under the inherited root %s", files.bundle, root)
			}
			for _, dir := range []string{releasedRuntime, releasedTemp} {
				if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
					t.Errorf("the released directory %s holds %d entries (read error %v), want none", dir, len(entries), err)
				}
			}
		})
	}
}

// TestPrepareProxyTrustForCommandReadsARelativeTrustPathFromTheStartDirectory: agent-secrets runs the
// command in its own process (syscall.Exec), so the command starts in this directory, and a
// relative trust path names here the file the command itself would read at its start. The bundle
// carries what that file holds, under an absolute path that still holds after the command changes
// directory, and one stderr line names each variable so read. The decision counts no relative path,
// so such a command never skips the bundle: the control names the same file by its absolute path
// and skips it.
func TestPrepareProxyTrustForCommandReadsARelativeTrustPathFromTheStartDirectory(t *testing.T) {
	root, other := proxyTestRoot(t, "test proxy root"), proxyTestRoot(t, "test private authority")
	system := writeLoginFile(t, "ca-certificates.crt", root)
	useSystemTrustFiles(t, system)
	start := t.TempDir()
	t.Chdir(start)
	if err := os.WriteFile(filepath.Join(start, "corp.pem"), []byte(other+root), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(start, "wgetrc"), []byte("tries = 7\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	release := testRelease("AIRLOCK_ME", []string{"api.example.com"}, nil)
	release.CACertificate = root
	run := func(t *testing.T, more ...string) (airlockFiles, string) {
		t.Helper()
		environ := withSettings(append([]string{"XDG_RUNTIME_DIR=" + t.TempDir()}, proxyStoreSettings(t, system)...), more...)
		var stderr strings.Builder
		files, err := prepareProxyTrustForCommand(environ, nil, release, "grant-one", nil, &stderr)
		if err != nil {
			t.Fatalf("prepareProxyTrustForCommand: %v", err)
		}
		return files, stderr.String()
	}

	t.Run("control: the same file by its absolute path", func(t *testing.T) {
		if files, stderr := run(t, "CURL_CA_BUNDLE="+filepath.Join(start, "corp.pem")); files.bundle != "" || stderr != "" {
			t.Fatalf("an absolute CURL_CA_BUNDLE holding the root: bundle %q, stderr %q; want no bundle and nothing said", files.bundle, stderr)
		}
	})
	t.Run("a relative CURL_CA_BUNDLE holding the root", func(t *testing.T) {
		files, stderr := run(t, "CURL_CA_BUNDLE=corp.pem")
		if files.bundle == "" {
			t.Fatalf("a relative CURL_CA_BUNDLE: no bundle, want one")
		}
		bundle, err := os.ReadFile(files.bundle)
		if err != nil {
			t.Fatalf("read the bundle: %v", err)
		}
		if !strings.Contains(string(bundle), other) {
			t.Errorf("the bundle lacks what the relative CURL_CA_BUNDLE names in the start directory")
		}
		if want := "agent-secrets: CURL_CA_BUNDLE is a relative path, resolved for the trust bundle from the directory the command starts in\n"; stderr != want {
			t.Errorf("stderr is %q, want %q", stderr, want)
		}
	})
	t.Run("a relative WGETRC", func(t *testing.T) {
		files, stderr := run(t, "WGETRC=wgetrc")
		if files.wgetrc == "" {
			t.Fatalf("a relative WGETRC: no wgetrc, want one")
		}
		if data, err := os.ReadFile(files.wgetrc); err != nil || !strings.Contains(string(data), "tries = 7\n") {
			t.Errorf("the wgetrc lacks the relative WGETRC's lines (read error %v)", err)
		}
		if want := "agent-secrets: WGETRC is a relative path, resolved for the wgetrc from the directory the command starts in\n"; stderr != want {
			t.Errorf("stderr is %q, want %q", stderr, want)
		}
	})
}

// TestExecFormNamesARelativeTrustPathItReadFromTheStartDirectory is the stderr line end to end:
// the built binary, run in this package's directory with CURL_CA_BUNDLE naming a file relative to
// it, puts that file in the bundle and names the variable on stderr before the line that names the
// released secrets.
func TestExecFormNamesARelativeTrustPathItReadFromTheStartDirectory(t *testing.T) {
	binary := buildAgentSecrets(t)
	broker, _ := fakeBroker(t)
	defer broker.Close()
	keyDir := newKeyDir(t)
	other := proxyTestRoot(t, "test private authority")
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(cwd, writeLoginFile(t, "corp.pem", other))
	if err != nil || filepath.IsAbs(relative) {
		t.Fatalf("no relative path from %s to the test's file: %q, %v", cwd, relative, err)
	}
	stdout, stderr, exit := runAgentSecrets(t, binary, broker.URL, keyDir,
		[]string{"XDG_RUNTIME_DIR=" + t.TempDir(), "AGENT_SECRETS_HELPER_SOCK=", "CURL_CA_BUNDLE=" + relative},
		"AIRLOCK_ME", "--", "sh", "-c", `cat "$CURL_CA_BUNDLE"`)
	if exit != 0 {
		t.Fatalf("exit = %d, want 0: stdout=%q stderr=%q", exit, stdout, stderr)
	}
	if !strings.Contains(stdout, other) {
		t.Errorf("the command's bundle lacks what the relative CURL_CA_BUNDLE names")
	}
	lines := strings.Split(strings.TrimSuffix(stderr, "\n"), "\n")
	want := []string{
		"agent-secrets: CURL_CA_BUNDLE is a relative path, resolved for the trust bundle from the directory the command starts in",
		"agent-secrets: AIRLOCK_ME from " + fakeSourceText(broker),
	}
	if len(lines) < 2 || lines[0] != want[0] || lines[1] != want[1] {
		t.Errorf("stderr is %q, want its first two lines %q", stderr, want)
	}
}

// TestPrepareAirlockGivesEachTrustItsOwnDirectory: agent-secrets reuses a live grant across the
// commands of a session, and two commands of one grant can name different trust. The directory a
// run's files go in is named for the grant and for what its clients will read (proxyTrustDirName),
// so a run whose trust differs gets a directory and a bundle of its own, and the earlier run's
// stays as its command may still be reading it. The name is of what was read, not of the
// settings: the same path holding other bytes is other trust, and two paths holding the same bytes
// are the same trust.
func TestPrepareAirlockGivesEachTrustItsOwnDirectory(t *testing.T) {
	runtimeDir := t.TempDir()
	airlockRoot := filepath.Join(runtimeDir, "agent-secrets", "airlock")
	expires := time.Now().Add(time.Hour)
	privateA, privateB := proxyTestRoot(t, "test private authority A"), proxyTestRoot(t, "test private authority B")
	named := writeLoginFile(t, "corp.pem", privateA)
	sameBytes := writeLoginFile(t, "corp-copy.pem", privateA)
	run := func(t *testing.T, more ...string) (airlockFiles, string) {
		t.Helper()
		files, err := prepareAirlock(append([]string{"XDG_RUNTIME_DIR=" + runtimeDir}, more...), testRelease("AIRLOCK_ME", nil, nil), "grant-one", &expires)
		if err != nil {
			t.Fatalf("prepareAirlock: %v", err)
		}
		data, err := os.ReadFile(files.bundle)
		if err != nil {
			t.Fatalf("read the bundle: %v", err)
		}
		return files, string(data)
	}

	first, firstBundle := run(t)
	firstDir := grantDirOf(t, airlockRoot, "grant-one", first.bundle)
	second, secondBundle := run(t, "CURL_CA_BUNDLE="+named)
	secondDir := grantDirOf(t, airlockRoot, "grant-one", second.bundle)
	if secondDir == firstDir {
		t.Fatalf("a run whose CURL_CA_BUNDLE names another authority got the first run's directory %s", firstDir)
	}
	if !strings.Contains(secondBundle, privateA) || strings.Contains(firstBundle, privateA) {
		t.Errorf("the second run's bundle lacks its CURL_CA_BUNDLE's authority, or the first run's holds it")
	}
	if data, err := os.ReadFile(first.bundle); err != nil || string(data) != firstBundle {
		t.Errorf("the first run's bundle changed or went (read error %v)", err)
	}
	if again, _ := run(t, "CURL_CA_BUNDLE="+sameBytes); filepath.Dir(again.bundle) != secondDir {
		t.Errorf("another path holding the same bytes got %s, want the second run's directory %s", filepath.Dir(again.bundle), secondDir)
	}
	if err := os.WriteFile(named, []byte(privateB), 0o600); err != nil {
		t.Fatal(err)
	}
	third, thirdBundle := run(t, "CURL_CA_BUNDLE="+named)
	if thirdDir := grantDirOf(t, airlockRoot, "grant-one", third.bundle); thirdDir == secondDir {
		t.Errorf("the same path holding other bytes got the second run's directory %s", secondDir)
	}
	if !strings.Contains(thirdBundle, privateB) || strings.Contains(thirdBundle, privateA) {
		t.Errorf("the third run's bundle does not hold what its CURL_CA_BUNDLE holds now")
	}
	for _, files := range []airlockFiles{first, second, third} {
		if data, err := os.ReadFile(files.wgetrc); err != nil || !strings.Contains(string(data), "ca_certificate = "+files.bundle+"\n") {
			t.Errorf("the wgetrc %s does not name its own directory's bundle (read error %v)", files.wgetrc, err)
		}
	}
}

// TestPrepareAirlockRunsOfOneTrustShareOneFinishedDirectory: runs of one grant and one trust,
// parallel tool calls of one agent among them, take one directory, and each answers it only once
// it holds every file under its final name (the rename): no run reads a half-written one. Runs
// that start together on a root with no key yet race to make the key the name is keyed with
// (proxyTrustDirKey), and each ends with the key the winner put in place: the root holds that one
// key, the one directory, and nothing a run left behind. Each round is a fresh root, so each
// round races for the key again.
func TestPrepareAirlockRunsOfOneTrustShareOneFinishedDirectory(t *testing.T) {
	const rounds, runs = 8, 12
	expires := time.Now().Add(time.Hour)
	for round := range rounds {
		runtimeDir := t.TempDir()
		environ := []string{"XDG_RUNTIME_DIR=" + runtimeDir}
		var wg sync.WaitGroup
		start := make(chan struct{})
		answers := make([]airlockFiles, runs)
		// Each worker reports with t.Errorf, which may be called from any goroutine, so no failure
		// waits on a reader: a run that answers nothing fails here rather than stalling the test.
		for i := range runs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				files, err := prepareAirlock(environ, testRelease("AIRLOCK_ME", nil, nil), "grant-one", &expires)
				if err != nil {
					t.Errorf("round %d run %d: %v", round, i, err)
					return
				}
				answers[i] = files
				for _, path := range []string{files.bundle, files.wgetrc, filepath.Join(filepath.Dir(files.bundle), "expires")} {
					if _, err := os.Stat(path); err != nil {
						t.Errorf("round %d run %d answered a directory without %s: %v", round, i, filepath.Base(path), err)
					}
				}
				if data, err := os.ReadFile(files.bundle); err != nil || !strings.Contains(string(data), testAirlockCertificate) {
					t.Errorf("round %d run %d answered a bundle without the proxy's certificate (read error %v)", round, i, err)
				}
			}()
		}
		close(start)
		wg.Wait()
		for i, files := range answers {
			if files.bundle != answers[0].bundle || files.wgetrc != answers[0].wgetrc {
				t.Errorf("round %d run %d answered %q and %q, want the one directory %q", round, i, files.bundle, files.wgetrc, filepath.Dir(answers[0].bundle))
			}
		}
		entries, err := os.ReadDir(filepath.Join(runtimeDir, "agent-secrets", "airlock"))
		if err != nil {
			t.Fatal(err)
		}
		names := make([]string, len(entries))
		for i, entry := range entries {
			names[i] = entry.Name()
		}
		want := []string{proxyTrustKeyFile, filepath.Base(filepath.Dir(answers[0].bundle))}
		if !slices.Equal(names, want) {
			t.Errorf("round %d: the airlock root holds %v, want the one key and the one directory of the one trust %v", round, names, want)
		}
	}
}

// TestTidyAirlockDirsSweepsAnEndedGrantByItsName: each directory of a grant carries the grant's
// expiry, so once the grant ends a tidy removes every one of them, whatever trust each holds. The
// running grant's own directories stay whatever their expires files say, as before, and so does
// every directory of a grant still live. A grant whose id begins with the running one's is a grant
// of its own: ended, it goes.
func TestTidyAirlockDirsSweepsAnEndedGrantByItsName(t *testing.T) {
	root := t.TempDir()
	leave := func(grant, trust, expires string) string {
		t.Helper()
		dir := filepath.Join(root, proxyTrustDirName([]byte("a key"), grant, []byte(trust), nil))
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "expires"), []byte(expires), 0o600); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	past, ahead := time.Now().Add(-time.Minute).Format(time.RFC3339), time.Now().Add(time.Hour).Format(time.RFC3339)
	endedA, endedB := leave("grant-ended", "trust A", past), leave("grant-ended", "trust B", past)
	endedAlike := leave("grant-one-old", "trust A", past)
	alike := leave("grant-ended-later", "trust A", ahead)
	live := leave("grant-live", "trust A", ahead)
	ownA, ownB := leave("grant-one", "trust A", past), leave("grant-one", "trust B", ahead)

	tidyAirlockDirs(root, "grant-one", time.Now())
	for _, dir := range []string{endedA, endedB, endedAlike} {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("%s of an ended grant is still there, want it removed", filepath.Base(dir))
		}
	}
	for _, dir := range []string{alike, live, ownA, ownB} {
		if _, err := os.Stat(dir); err != nil {
			t.Errorf("%s is gone, want it kept: %v", filepath.Base(dir), err)
		}
	}
}

// TestProxyTrustDirGrantReadsTheNameProxyTrustDirNameGives: the grant id comes back from the
// directory name whatever it holds, a dot among it, and a name of another form names no grant.
func TestProxyTrustDirGrantReadsTheNameProxyTrustDirNameGives(t *testing.T) {
	for _, grant := range []string{"grant-one", "grant.with.dots", "0193f6a2-7d1e-7c3b-9a4f-2b1c0d9e8f7a"} {
		if got, ok := proxyTrustDirGrant(proxyTrustDirName([]byte("a key"), grant, []byte("a bundle"), []byte("a wgetrc"))); !ok || got != grant {
			t.Errorf("proxyTrustDirGrant(proxyTrustDirName(%q)) = %q, %v; want %q, true", grant, got, ok, grant)
		}
	}
	for _, name := range []string{"grant-one", "grant-one.0123", "grant-one.0123456789abcdeg", ".0123456789abcdef", airlockBuildPrefix + "123"} {
		if got, ok := proxyTrustDirGrant(name); ok {
			t.Errorf("proxyTrustDirGrant(%q) = %q, true; want no grant", name, got)
		}
	}
}

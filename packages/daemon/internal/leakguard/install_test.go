package leakguard

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/leakguard/leakguardtest"
	"github.com/sjawhar/legion/daemon/internal/procgroup"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// theTag is the pin these tests install, spelled as the worker image spells one.
const theTag = "leak-check-v15"

// The members the test's own tarball carries under its leak-check directory. They are the shape
// the real one has at leak-check-v15, which this package measured: the launcher, the python file
// it runs from beside itself, a fixture its self-test reads, and a nested directory. A file
// outside that directory is in the tarball too, to show that the rest of the repository is left
// where it is.
const (
	theNeighbour = leakguardtest.ProgramName
	theFixture   = "must-pass-lines.txt"
	theNested    = "revert-gate/run.py"
)

// answersItsHelp is a stand-in launcher that answers as the real one does: it finds its python
// file beside itself, prints the usage header naming the two shapes this package calls, and exits
// 1, which is what `public-leak-check.sh --help` exits with at leak-check-v15. A caller that read
// the status rather than the answer would call this a failure.
const answersItsHelp = `#!/bin/sh
beside="$(dirname "$0")/` + theNeighbour + `"
if [ ! -f "$beside" ]; then
  echo "LEAK CHECK ERROR: $beside is missing, so the check could not start" >&2
  exit 2
fi
echo '# Usage: check-public-text FILE..., --commits REV..., --tree DIR, gh-guard ARGS, or REMOTE URL.'
exit 1
`

// answersNothing is a stand-in launcher that cannot start: the shape the real launcher takes when
// /usr/bin/python3 is missing or the python file did not land beside it.
const answersNothing = `#!/bin/sh
echo "LEAK CHECK ERROR: /usr/bin/python3 is missing or not executable, so the check could not start" >&2
exit 2
`

// theCheck is the members of a check that runs: the launcher, its python file, its README, its
// fixture and a nested directory — every file of its manifest (ManifestFiles) and two it does not
// cover.
func theCheck(launcher string) map[string]leakguardtest.Member {
	return map[string]leakguardtest.Member{
		checkName:                {Body: launcher, Mode: 0o755},
		theNeighbour:             {Body: leakguardtest.Program, Mode: 0o644},
		leakguardtest.ReadmeName: {Body: leakguardtest.Readme, Mode: 0o644},
		theFixture:               {Body: "a line the self-test must pass\n", Mode: 0o600},
		theNested:                {Body: "# the revert gate\n", Mode: 0o644},
	}
}

// wholeCheck is a tarball of a check that runs.
func wholeCheck(t *testing.T, launcher string) []byte {
	t.Helper()
	return leakguardtest.GitHubTarball(t, theCheck(launcher))
}

// served is what the daemon serves of a tarball: its leak-check directory, packed. The manifest
// digest the pack answers beside it is the source's to read (TestTheServedBodyIsTheDirectoryAloneWithItsDigest).
func served(t *testing.T, raw []byte) []byte {
	t.Helper()
	body, _, err := PackTarballManifest(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("pack: %v", err)
	}
	return body
}

// marked is answersItsHelp printing tag in its usage header, so a test reads which of two installs
// is in place from the installed check itself.
func marked(tag string) string {
	return strings.Replace(answersItsHelp, "# Usage:", "# "+tag+" Usage:", 1)
}

// helpOf is what the check at dir prints for `--help`, which exits 1 as the real one does.
func helpOf(t *testing.T, dir string) string {
	t.Helper()
	printed, err := exec.Command(filepath.Join(dir, checkName), "--help").Output()
	if err == nil {
		t.Fatalf("the installed check exited 0 printing %q, want the usage header and exit 1", printed)
	}
	return string(printed)
}

// recordedTag is the tag file of the install at dir.
func recordedTag(t *testing.T, dir string) string {
	t.Helper()
	recorded, err := os.ReadFile(filepath.Join(dir, tagFile))
	if err != nil {
		t.Fatalf("read the install's tag: %v", err)
	}
	return string(recorded)
}

// leftovers is every entry beside the install directory: an install leaves none, since the
// directory is unpacked under a name of its own, renamed into place, and what it replaced removed.
func leftovers(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(dir))
	if err != nil {
		t.Fatal(err)
	}
	var left []string
	for _, entry := range entries {
		if entry.Name() != filepath.Base(dir) {
			left = append(left, entry.Name())
		}
	}
	return left
}

// install is InstallServed of body at tag into dir, with every line it logs collected.
func install(t *testing.T, dir, tag string, body []byte) ([]string, error) {
	t.Helper()
	var logged []string
	err := InstallServed(bound(t), ServedInstall{Dir: dir, Tag: tag, Body: body, Start: procgroup.NoReaper, Log: func(format string, args ...any) {
		logged = append(logged, format)
	}})
	return logged, err
}

// packedMember is one member of a packed body, as read back.
type packedMember struct {
	typeflag byte
	mode     int64
	body     string
}

func membersOf(t *testing.T, body []byte) map[string]packedMember {
	t.Helper()
	zipped, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("the packed body is no gzip stream: %v", err)
	}
	archive := tar.NewReader(zipped)
	members := map[string]packedMember{}
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			return members
		}
		if err != nil {
			t.Fatalf("read the packed body: %v", err)
		}
		content, err := io.ReadAll(archive)
		if err != nil {
			t.Fatal(err)
		}
		members[header.Name] = packedMember{typeflag: header.Typeflag, mode: header.Mode, body: string(content)}
	}
}

// The daemon serves the check's directory alone: the files of leak-check/ at the directory's own
// paths, with their bytes and modes, and nothing of the rest of the repository nor its prefix.
func TestThePackHoldsTheDirectoryAlone(t *testing.T) {
	files := theCheck(answersItsHelp)
	body, _, err := PackTarballManifest(bytes.NewReader(leakguardtest.GitHubTarball(t, files)))
	if err != nil {
		t.Fatalf("pack: %v", err)
	}
	members := membersOf(t, body)
	var names []string
	for name := range members {
		names = append(names, name)
	}
	slices.Sort(names)
	want := []string{checkName, theFixture, theNeighbour, theNested, leakguardtest.ReadmeName}
	slices.Sort(want)
	if !slices.Equal(names, want) {
		t.Fatalf("the pack holds %q, want the directory's own files alone, %q", names, want)
	}
	for name, m := range files {
		got := members[name]
		if got.typeflag != tar.TypeReg || got.body != m.Body || got.mode != m.Mode {
			t.Errorf("the pack holds %s as (type %q, mode %o, %q), want a regular file of mode %o holding %q",
				name, got.typeflag, got.mode, got.body, m.Mode, m.Body)
		}
	}
}

// A member of the directory that is neither a directory nor a regular file, one whose path leaves
// the directory, a tarball that holds no such directory, one whose directory holds no file, one
// whose directory misses a file of the manifest, one whose manifest file is past the read limit and
// one that holds a manifest file twice are each refused by name, with no body and no digest. A
// missing manifest file is refused rather than hashed as a digest with a gap, and a second copy of
// one rather than hashed as whichever copy came last.
func TestThePackRefusesWhatIsNotTheCheck(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  func(t *testing.T) []byte
		want string
	}{
		{
			name: "a member outside the directory",
			raw: func(t *testing.T) []byte {
				files := theCheck(answersItsHelp)
				files["../escaped.sh"] = leakguardtest.Member{Body: "#!/bin/sh\n", Mode: 0o755}
				return leakguardtest.GitHubTarball(t, files)
			},
			want: `holds the member "` + leakguardtest.TarballPrefix + `leak-check/../escaped.sh", which is no path inside leak-check/`,
		},
		{
			name: "a symbolic link",
			raw: func(t *testing.T) []byte {
				files := theCheck(answersItsHelp)
				files["linked"] = leakguardtest.Member{Link: "/etc/passwd", Mode: 0o777}
				return leakguardtest.GitHubTarball(t, files)
			},
			want: `holds the member "` + leakguardtest.TarballPrefix + `leak-check/linked", which is neither a directory nor a regular file`,
		},
		{
			name: "no leak-check directory",
			raw: func(t *testing.T) []byte {
				return leakguardtest.Tarball(t, map[string]leakguardtest.Member{
					leakguardtest.TarballPrefix + "README.md":          {Body: "the rest of the repository\n", Mode: 0o644},
					leakguardtest.TarballPrefix + "other/" + checkName: {Body: answersItsHelp, Mode: 0o755},
				})
			},
			want: "holds no leak-check directory",
		},
		{
			name: "a leak-check directory with no file",
			raw: func(t *testing.T) []byte {
				return leakguardtest.GitHubTarball(t, nil)
			},
			want: "holds a leak-check directory with no file",
		},
		{
			name: "a missing manifest file",
			raw: func(t *testing.T) []byte {
				files := theCheck(answersItsHelp)
				delete(files, leakguardtest.ReadmeName)
				return leakguardtest.GitHubTarball(t, files)
			},
			want: "holds a leak-check directory without a file of its manifest: " + leakguardtest.ReadmeName + ": file does not exist",
		},
		{
			name: "a manifest file past the read limit",
			raw: func(t *testing.T) []byte {
				files := theCheck(answersItsHelp)
				files[leakguardtest.ReadmeName] = leakguardtest.Member{Body: strings.Repeat("r", manifestFileLimit+1), Mode: 0o644}
				return leakguardtest.GitHubTarball(t, files)
			},
			want: `holds the manifest file "` + leakguardtest.ReadmeName + `", which is over the 8 MiB read limit`,
		},
		{
			name: "a manifest file held twice",
			raw: func(t *testing.T) []byte {
				// A raw tar, since GitHubTarball's map holds each name once: every file of the
				// check under its directory, then a second README.md of other bytes.
				var raw bytes.Buffer
				zipped := gzip.NewWriter(&raw)
				archive := tar.NewWriter(zipped)
				add := func(name, body string) {
					t.Helper()
					if err := archive.WriteHeader(&tar.Header{Name: leakguardtest.TarballPrefix + "leak-check/" + name, Typeflag: tar.TypeReg,
						Mode: 0o644, Size: int64(len(body))}); err != nil {
						t.Fatal(err)
					}
					if _, err := archive.Write([]byte(body)); err != nil {
						t.Fatal(err)
					}
				}
				files := theCheck(answersItsHelp)
				for _, name := range slices.Sorted(maps.Keys(files)) {
					add(name, files[name].Body)
				}
				add(leakguardtest.ReadmeName, "a second README of other bytes\n")
				if err := archive.Close(); err != nil {
					t.Fatal(err)
				}
				if err := zipped.Close(); err != nil {
					t.Fatal(err)
				}
				return raw.Bytes()
			},
			want: `holds the manifest file "` + leakguardtest.ReadmeName + `" more than once`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, manifest, err := PackTarballManifest(bytes.NewReader(tc.raw(t)))
			if err == nil || err.Error() != tc.want {
				t.Fatalf("pack = %v, want %q", err, tc.want)
			}
			if body != nil || manifest != "" {
				t.Errorf("a refused pack answered %d bytes and the digest %q, want neither", len(body), manifest)
			}
		})
	}
}

// The launcher's install lands the served files and the tag beside them, logs nothing of a fetch,
// and replaces what the directory held: an install of another tag, and one of the same tag, which
// is what a restarted launcher finds and installs over.
func TestTheServedInstallLandsTheCheckAndReplacesWhatWasThere(t *testing.T) {
	const newTag = "leak-check-v16"
	dir := filepath.Join(t.TempDir(), "leak-check")

	logged, err := install(t, dir, theTag, served(t, wholeCheck(t, marked(theTag))))
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if logged != nil {
		t.Errorf("an install into an empty place logged %q, want nothing", logged)
	}
	for name, m := range theCheck(marked(theTag)) {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Errorf("the install is missing %s: %v", name, err)
			continue
		}
		if got := int64(info.Mode().Perm()); got != m.Mode {
			t.Errorf("the install holds %s with mode %o, want %o", name, got, m.Mode)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "CONTRIBUTING.md")); !os.IsNotExist(err) {
		t.Errorf("stat the repository's own CONTRIBUTING.md in the install = %v, want nothing of the repository outside %s", err, checkDirectory)
	}
	if got := recordedTag(t, dir); got != theTag+"\n" {
		t.Errorf("the install recorded %q, want %q", got, theTag+"\n")
	}
	if got := helpOf(t, dir); !strings.Contains(got, theTag) || !strings.Contains(got, "gh-guard") {
		t.Errorf("the installed check printed %q, want the first tag's usage header", got)
	}

	for _, tc := range []struct {
		name, tag, marker string
	}{
		{"another tag", newTag, newTag},
		{"the same tag", newTag, newTag + " again"},
	} {
		t.Run("over an install of "+tc.name, func(t *testing.T) {
			logged, err := install(t, dir, tc.tag, served(t, wholeCheck(t, marked(tc.marker))))
			if err != nil {
				t.Fatalf("install: %v", err)
			}
			if got := helpOf(t, dir); !strings.Contains(got, "# "+tc.marker+" Usage:") {
				t.Errorf("the installed check printed %q, want the served one's header", got)
			}
			if got := recordedTag(t, dir); got != tc.tag+"\n" {
				t.Errorf("the install recorded %q, want %q", got, tc.tag+"\n")
			}
			if len(logged) != 1 {
				t.Errorf("the replacing install logged %q, want its one line", logged)
			}
			for _, name := range []string{theNeighbour, theFixture, theNested} {
				if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
					t.Errorf("the install that replaced another is missing %s: %v", name, err)
				}
			}
			if left := leftovers(t, dir); left != nil {
				t.Errorf("the replacing install left %v beside the install path, want the install alone", left)
			}
		})
	}
}

// GitHub's tarball of a tag carries the modes git archive gives its members under its tar.umask of
// 002: 0664 for a file and 0775 for a program. The install takes group and other write off each
// member's mode, so the installed check's files have the modes they had when the umask of 022
// made them, 0644 and 0755, whatever umask the launcher runs under: under 002 as well. The test
// sets the process's umask, so it does not run beside another.
func TestTheInstalledCheckHasNoGroupOrOtherWriteUnderAnyUmask(t *testing.T) {
	members := theCheck(answersItsHelp)
	want := map[string]os.FileMode{}
	for name, archived := range map[string]int64{checkName: 0o775, theNeighbour: 0o664, leakguardtest.ReadmeName: 0o664, theFixture: 0o600, theNested: 0o664} {
		m := members[name]
		m.Mode = archived
		members[name] = m
		want[name] = os.FileMode(archived) &^ 0o022
	}
	body := served(t, leakguardtest.GitHubTarball(t, members))
	dir := filepath.Join(t.TempDir(), "leak-check")
	umask := syscall.Umask(0o002)
	_, err := install(t, dir, theTag, body)
	syscall.Umask(umask)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	for name, mode := range want {
		info, err := os.Lstat(filepath.Join(dir, name))
		if err != nil {
			t.Errorf("the install is missing %s: %v", name, err)
			continue
		}
		if got := info.Mode(); got != mode {
			t.Errorf("the install holds %s as %s from the archive's %o, want %s", name, got, members[name].Mode, mode)
		}
	}
}

// A served check that does not answer its --help, a body holding no file and a body that is no
// tarball each fail the install, name what happened, and leave the directory as it was — an
// absent one absent, and one holding an install with that install — with nothing beside it.
func TestAServedCheckThatDoesNotAnswerLeavesTheDirectoryAsItWas(t *testing.T) {
	refused := []struct {
		name string
		body func(t *testing.T) []byte
		want string
	}{
		{
			name: "a check whose --help names no shape",
			body: func(t *testing.T) []byte { return served(t, wholeCheck(t, answersNothing)) },
			want: "the served check did not answer: its --help names no check-public-text shape: " +
				"LEAK CHECK ERROR: /usr/bin/python3 is missing or not executable, so the check could not start",
		},
		{
			name: "a body holding no file",
			body: func(t *testing.T) []byte { return leakguardtest.Tarball(t, nil) },
			want: "the pod's launcher could not unpack the check the daemon served: it holds no file",
		},
		{
			name: "a body that is no tarball",
			body: func(*testing.T) []byte { return []byte("not a tarball") },
			want: "the pod's launcher could not unpack the check the daemon served: gzip: invalid header",
		},
	}
	for _, tc := range refused {
		t.Run(tc.name+", into an empty place", func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "leak-check")
			if _, err := install(t, dir, theTag, tc.body(t)); err == nil || err.Error() != tc.want {
				t.Fatalf("install = %v, want %q", err, tc.want)
			}
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Errorf("stat %s = %v, want the install path absent after a failed install", dir, err)
			}
			if left := leftovers(t, dir); left != nil {
				t.Errorf("a failed install left %v beside the install path", left)
			}
		})
		t.Run(tc.name+", over an install", func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "leak-check")
			if _, err := install(t, dir, theTag, served(t, wholeCheck(t, marked(theTag)))); err != nil {
				t.Fatalf("the first install: %v", err)
			}
			if _, err := install(t, dir, "leak-check-v16", tc.body(t)); err == nil || err.Error() != tc.want {
				t.Fatalf("install = %v, want %q", err, tc.want)
			}
			if got := helpOf(t, dir); !strings.Contains(got, theTag) {
				t.Errorf("the installed check printed %q, want the install that was there", got)
			}
			if got := recordedTag(t, dir); got != theTag+"\n" {
				t.Errorf("the install records %q, want the one that was there, %q", got, theTag+"\n")
			}
			if left := leftovers(t, dir); left != nil {
				t.Errorf("a failed install left %v beside the install path", left)
			}
		})
	}
}

// An install that names no directory is refused before anything is written: the caller names
// where the check goes.
func TestAnInstallNamingNoDirectoryIsRefused(t *testing.T) {
	t.Chdir(t.TempDir())
	_, err := install(t, "", theTag, served(t, wholeCheck(t, answersItsHelp)))
	if want := "the pod's launcher named no directory to install the check into"; err == nil || err.Error() != want {
		t.Fatalf("install = %v, want %q", err, want)
	}
	if entries, err := os.ReadDir("."); err != nil || len(entries) != 0 {
		t.Fatalf("the refused install left %v (%v) in the working directory, want nothing", entries, err)
	}
}

// The launcher that runs the install is the pod's first process and holds every role's launcher
// token, so the one --help run is handed PATH, and HOME and TMPDIR set to the install's parent,
// and nothing else of the process's environment. The stand-in check writes the environment it was
// handed into its TMPDIR, and answers its help as the real one does.
func TestTheHelpRunSeesOnlyPathHomeAndTmpdir(t *testing.T) {
	t.Setenv("LEGION_LAUNCHER_TOKEN", "a token the check must never see")
	t.Setenv("GH_TOKEN", "another")
	printsItsEnvironment := strings.Replace(answersItsHelp, "echo '# Usage:", "env > \"$TMPDIR/help-env\"\necho '# Usage:", 1)
	parent := t.TempDir()
	dir := filepath.Join(parent, "leak-check")
	if _, err := install(t, dir, theTag, served(t, wholeCheck(t, printsItsEnvironment))); err != nil {
		t.Fatalf("install: %v", err)
	}
	printed, err := os.ReadFile(filepath.Join(parent, "help-env"))
	if err != nil {
		t.Fatalf("the check wrote no environment under the install's parent: %v", err)
	}
	seen := map[string]string{}
	for line := range strings.Lines(string(printed)) {
		name, value, _ := strings.Cut(strings.TrimSuffix(line, "\n"), "=")
		seen[name] = value
	}
	// The shell the stand-in is sets these of its own: dash PWD, bash SHLVL and _ too.
	for _, own := range []string{"PWD", "OLDPWD", "SHLVL", "_"} {
		delete(seen, own)
	}
	want := map[string]string{"PATH": os.Getenv("PATH"), "HOME": parent, "TMPDIR": parent}
	if len(seen) != len(want) {
		t.Errorf("the --help run saw %q, want %q alone", seen, want)
	}
	for name, value := range want {
		if seen[name] != value {
			t.Errorf("the --help run saw %s=%q, want %q", name, seen[name], value)
		}
	}
}

// A --help run that ends with no exit of its own has answered no shape, but the install's refusal
// names why it ended rather than a check that answered nothing. One that something else killed
// (another drain's signal, the kernel's out-of-memory kill) is refused naming the kill, with what
// it printed before it beneath (procgroup.Said), so the person reading the claim's cause looks for
// what killed it and sees what the check said first. One the install's own context ended is
// refused naming the context, since that end is what killed the run (verifyServed reads it before
// the run's error). The stand-in prints a line, writes its pid and waits; each row ends it its own
// way.
func TestAHelpRunThatEndsWithNoExitIsRefusedNamingWhy(t *testing.T) {
	for _, row := range []struct {
		name string
		end  func(t *testing.T, pid int, cancel context.CancelFunc)
		want string
	}{
		{
			name: "killed by another process",
			end: func(t *testing.T, pid int, _ context.CancelFunc) {
				if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
					t.Fatalf("kill the --help run %d: %v", pid, err)
				}
			},
			want: "the served check could not be run: signal: terminated\nLEAK CHECK: reading the rules",
		},
		{
			name: "the install's context ended",
			end:  func(_ *testing.T, _ int, cancel context.CancelFunc) { cancel() },
			want: "the served check could not be run: context canceled",
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			parent := t.TempDir()
			pidFile := filepath.Join(parent, "help-pid")
			body := served(t, wholeCheck(t, "#!/bin/sh\necho 'LEAK CHECK: reading the rules' >&2\necho $$ >'"+pidFile+"'\nexec sleep 30\n"))
			dir := filepath.Join(parent, "leak-check")
			ctx, cancel := context.WithCancel(bound(t))
			defer cancel()
			done := make(chan error, 1)
			go func() {
				done <- InstallServed(ctx, ServedInstall{Dir: dir, Tag: theTag, Body: body, Start: procgroup.NoReaper, Log: func(string, ...any) {}})
			}()
			var pid int
			testwait.WithinPolling(t, 10*time.Second, testwait.Poll, "the --help run to write its pid", func() bool {
				written, err := os.ReadFile(pidFile)
				n, convErr := strconv.Atoi(strings.TrimSpace(string(written)))
				pid = n
				return err == nil && convErr == nil && pid > 0
			})
			row.end(t, pid, cancel)
			if err := <-done; err == nil || err.Error() != row.want {
				t.Fatalf("install with its --help run ended = %v, want %q", err, row.want)
			}
			if _, statErr := os.Lstat(dir); !errors.Is(statErr, fs.ErrNotExist) {
				t.Errorf("the refused install left %s (%v), want nothing there", dir, statErr)
			}
		})
	}
}

// A role's shim reads the install and starts no agent unless it is the pin's and its manifest
// digest is the deployment's: an absent check, an install that records no tag, one of another
// tag, one whose program is another, one whose launcher is edited beside an untouched program,
// and one missing a file of the manifest are each refused naming the pin and what is there. With
// no pin, which is every tmux pane, nothing is read and nothing refused; and with the pin but no
// deployment digest (CheckerVar), which is every role of a deployment that runs no shared leak
// check, nothing is read either, even where there is no install at all.
func TestInstalledAnswersTheCheckOfThePinAlone(t *testing.T) {
	installedAt := func(t *testing.T) string {
		t.Helper()
		dir := filepath.Join(t.TempDir(), "leak-check")
		if _, err := install(t, dir, theTag, served(t, wholeCheck(t, answersItsHelp))); err != nil {
			t.Fatalf("install: %v", err)
		}
		return dir
	}
	deployments, err := manifestSHA256(filepath.Join(installedAt(t), checkName))
	if err != nil {
		t.Fatal(err)
	}
	pinned := []string{"PATH=/usr/bin:/bin", TagVariable + "=" + theTag, CheckerVar + "=" + deployments}

	t.Run("the install of the pin", func(t *testing.T) {
		dir := installedAt(t)
		check, err := Installed(pinned, dir)
		if err != nil || check != filepath.Join(dir, checkName) {
			t.Fatalf("Installed = %q, %v; want %q", check, err, filepath.Join(dir, checkName))
		}
	})
	t.Run("no pin", func(t *testing.T) {
		check, err := Installed([]string{"PATH=/usr/bin:/bin"}, filepath.Join(t.TempDir(), "absent"))
		if err != nil || check != "" {
			t.Fatalf("Installed = %q, %v; want no check and no refusal", check, err)
		}
	})
	t.Run("the pin and no checker", func(t *testing.T) {
		check, err := Installed([]string{"PATH=/usr/bin:/bin", TagVariable + "=" + theTag}, filepath.Join(t.TempDir(), "absent"))
		if err != nil || check != "" {
			t.Fatalf("Installed = %q, %v; want no check and no refusal", check, err)
		}
	})
	edit := func(name, body string, mode os.FileMode) func(t *testing.T, dir string) {
		return func(t *testing.T, dir string) {
			path := filepath.Join(dir, name)
			if err := os.Chmod(path, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(body), mode); err != nil {
				t.Fatal(err)
			}
		}
	}
	differs := []string{TagVariable + "=" + theTag, "has the manifest digest ", "not the deployment's " + CheckerVar + "=" + deployments}
	for _, tc := range []struct {
		name  string
		spoil func(t *testing.T, dir string)
		want  []string
	}{
		{
			name:  "an absent check",
			spoil: func(t *testing.T, dir string) { remove(t, filepath.Join(dir, checkName)) },
			want:  []string{TagVariable + "=" + theTag, "installed no check"},
		},
		{
			name:  "an absent tag file",
			spoil: func(t *testing.T, dir string) { remove(t, filepath.Join(dir, tagFile)) },
			want:  []string{TagVariable + "=" + theTag, "records no tag"},
		},
		{
			name: "another tag",
			spoil: func(t *testing.T, dir string) {
				if err := os.WriteFile(filepath.Join(dir, tagFile), []byte("leak-check-v14\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			want: []string{TagVariable + "=" + theTag, "is of leak-check-v14"},
		},
		{
			name:  "a program of another version",
			spoil: edit(theNeighbour, "# a check of another version\n", 0o644),
			want:  differs,
		},
		{
			name:  "a launcher edited to exit 0 beside an untouched program",
			spoil: edit(checkName, "#!/bin/sh\nexit 0\n", 0o755),
			want:  differs,
		},
		{
			name:  "no program beside the check",
			spoil: func(t *testing.T, dir string) { remove(t, filepath.Join(dir, theNeighbour)) },
			want:  []string{TagVariable + "=" + theTag, "the manifest of the install at ", "cannot be read to compare with " + CheckerVar},
		},
		{
			name:  "no must-pass lines beside the check",
			spoil: func(t *testing.T, dir string) { remove(t, filepath.Join(dir, theFixture)) },
			want:  []string{TagVariable + "=" + theTag, "cannot be read to compare with " + CheckerVar, theFixture},
		},
		{
			// A role swaps the shared install's launcher for a link to a pass.sh it wrote beside
			// copies of the four real files: the file that runs is not the one the manifest holds.
			name: "a launcher linked out beside copies",
			spoil: func(t *testing.T, dir string) {
				copies := t.TempDir()
				for _, name := range ManifestFiles {
					body, err := os.ReadFile(filepath.Join(dir, name))
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(copies, name), body, 0o755); err != nil {
						t.Fatal(err)
					}
				}
				pass := filepath.Join(copies, "pass.sh")
				if err := os.WriteFile(pass, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
					t.Fatal(err)
				}
				remove(t, filepath.Join(dir, checkName))
				if err := os.Symlink(pass, filepath.Join(dir, checkName)); err != nil {
					t.Fatal(err)
				}
			},
			want: []string{TagVariable + "=" + theTag, "cannot be read to compare with " + CheckerVar, "which is not the " + checkName + " its manifest covers"},
		},
		{
			// A file the manifest covers that is a link is not followed, even to the real bytes.
			name: "a program that is a link to the real one",
			spoil: func(t *testing.T, dir string) {
				real := filepath.Join(t.TempDir(), theNeighbour)
				body, err := os.ReadFile(filepath.Join(dir, theNeighbour))
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(real, body, 0o644); err != nil {
					t.Fatal(err)
				}
				remove(t, filepath.Join(dir, theNeighbour))
				if err := os.Symlink(real, filepath.Join(dir, theNeighbour)); err != nil {
					t.Fatal(err)
				}
			},
			want: []string{TagVariable + "=" + theTag, "cannot be read to compare with " + CheckerVar, "is a symlink"},
		},
		{
			// A fifo nobody writes, where a plain read would wait for ever, holding the role's start.
			name: "a README that is a fifo",
			spoil: func(t *testing.T, dir string) {
				remove(t, filepath.Join(dir, leakguardtest.ReadmeName))
				if err := syscall.Mkfifo(filepath.Join(dir, leakguardtest.ReadmeName), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			want: []string{TagVariable + "=" + theTag, "cannot be read to compare with " + CheckerVar, "is a fifo"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := installedAt(t)
			tc.spoil(t, dir)
			done := make(chan struct{})
			var check string
			var err error
			go func() { check, err = Installed(pinned, dir); close(done) }()
			// A read that waits on the fifo fails by name before the test binary's deadline.
			testwait.Receive(t, "Installed of "+dir, done)
			failed := setupFailure(t, err)
			for _, want := range tc.want {
				if !strings.Contains(failed.Error(), want) {
					t.Errorf("Installed = %q, want it to name %q", failed, want)
				}
			}
			if check != "" {
				t.Errorf("a refused read answered %q, want no check", check)
			}
		})
	}
}

// The tag file is under the issue's scratch, which every role writes, and the pod's launcher,
// its container's PID 1, reads it as it replaces an install. So what a role leaves there is read
// without following a link and without waiting on a fifo, and bounded: a symlink to /dev/zero, a
// symlink to a file that holds the pin, a fifo nobody writes and a file past the bound each answer
// as no install, the shim's read (Installed) refuses naming why, and a new install replaces it. A
// read that followed the link to /dev/zero would read it to the end, one that followed the link
// to the pin would take it for an install, and one that opened the fifo plainly would wait for
// ever, and fails the row by name before the test binary's deadline (testwait.Receive).
func TestTheInstalledTagIsReadWithoutFollowingASymlinkOrWaitingOnAFifo(t *testing.T) {
	// A role of a deployment that runs a shared leak check, which is told a manifest digest; the
	// tag file's refusal comes before any digest is compared, so any digest serves.
	pinned := []string{"PATH=/usr/bin:/bin", TagVariable + "=" + theTag, CheckerVar + "=" + strings.Repeat("5e", 32)}
	for _, tc := range []struct {
		name    string
		plant   func(t *testing.T, path string)
		refusal string
	}{
		{"a symlink to /dev/zero", func(t *testing.T, path string) {
			if err := os.Symlink("/dev/zero", path); err != nil {
				t.Fatal(err)
			}
		}, "is a symlink"},
		{"a symlink to a file that holds the pin", func(t *testing.T, path string) {
			pin := filepath.Join(t.TempDir(), "pin")
			if err := os.WriteFile(pin, []byte(theTag+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(pin, path); err != nil {
				t.Fatal(err)
			}
		}, "is a symlink"},
		{"a fifo", func(t *testing.T, path string) {
			if err := syscall.Mkfifo(path, 0o644); err != nil {
				t.Fatal(err)
			}
		}, "is a fifo"},
		{"a file past the bound", func(t *testing.T, path string) {
			if err := os.WriteFile(path, bytes.Repeat([]byte("x"), tagLimit+1), 0o644); err != nil {
				t.Fatal(err)
			}
		}, "is larger than 256 bytes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "leak-check")
			if _, err := install(t, dir, theTag, served(t, wholeCheck(t, answersItsHelp))); err != nil {
				t.Fatalf("install: %v", err)
			}
			tag := filepath.Join(dir, tagFile)
			remove(t, tag)
			tc.plant(t, tag)
			type read struct {
				tag     string
				there   bool
				refused error
			}
			done := make(chan read, 1)
			go func() {
				got, there, refused := installedTag(dir)
				done <- read{got, there, refused}
			}()
			got := testwait.Receive(t, "installedTag of "+tag, done)
			if got.there || got.tag != "" || got.refused == nil || got.refused.Error() != tag+" "+tc.refusal {
				t.Fatalf("installedTag = %q, %v, %v; want no install, refused as %q", got.tag, got.there, got.refused, tag+" "+tc.refusal)
			}
			failed := setupFailure(t, func() error { _, err := Installed(pinned, dir); return err }())
			if !strings.Contains(failed.Error(), "its tag file is refused ("+tag+" "+tc.refusal+")") {
				t.Errorf("Installed = %q, want it to name the refusal", failed)
			}
			if _, err := install(t, dir, theTag, served(t, wholeCheck(t, answersItsHelp))); err != nil {
				t.Fatalf("the install over %s: %v", tc.name, err)
			}
			if got := recordedTag(t, dir); got != theTag+"\n" {
				t.Errorf("the install over %s records %q, want %q", tc.name, got, theTag+"\n")
			}
		})
	}
}

// The install unpacks the body and writes its tag into a staging directory under the scratch every
// role writes, so a role can plant a file at a member's path or the tag's first. Each is written
// with os.O_EXCL: a file already there refuses the install rather than being opened and written
// over, a fifo refuses it rather than holding the write until a reader comes, and a symlink
// refuses it rather than sending the write to the link's target. A served body that carries the
// tag file stands in for a file a role planted at the tag's path, since it puts one there every
// time; a fifo or a symlink is planted at the tag's path and at the launcher member's once the
// install has made its staging directory and before it writes (beforeTheFirstWrite).
func TestAMemberIsNeverWrittenThroughWhatARoleLeftAtItsPath(t *testing.T) {
	t.Run("the staged tag, through the install", func(t *testing.T) {
		members := theCheck(answersItsHelp)
		members[tagFile] = leakguardtest.Member{Body: "leak-check-v1\n", Mode: 0o644}
		dir := filepath.Join(t.TempDir(), "leak-check")
		_, err := install(t, dir, theTag, served(t, leakguardtest.GitHubTarball(t, members)))
		if !errors.Is(err, fs.ErrExist) || !strings.HasPrefix(err.Error(), "the installed tag could not be recorded: ") {
			t.Fatalf("install with a file already at the staged tag's path = %v, want it refused as existing", err)
		}
		if _, err := os.Lstat(dir); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("the refused install left %s (%v), want nothing there", dir, err)
		}
	})
	body := served(t, wholeCheck(t, answersItsHelp))
	for _, tc := range []struct {
		name  string
		plant func(path, target string) error
	}{
		{"a symlink", func(path, target string) error { return os.Symlink(target, path) }},
		{"a fifo", func(path, _ string) error { return syscall.Mkfifo(path, 0o644) }},
	} {
		for _, at := range []struct{ name, path, refusal string }{
			{"the tag's path", tagFile, "the installed tag could not be recorded: "},
			{"the launcher's path", checkName, "the pod's launcher could not unpack the check the daemon served: "},
		} {
			t.Run(tc.name+" at "+at.name, func(t *testing.T) {
				dir := filepath.Join(t.TempDir(), "leak-check")
				target := filepath.Join(t.TempDir(), "target")
				err := beforeTheFirstWrite(t, filepath.Join(filepath.Dir(dir), ".leak-check-install-*"), func(staging string) (string, error) {
					path := filepath.Join(staging, at.path)
					return path, tc.plant(path, target)
				}, "the install beside "+tc.name, func() error {
					_, err := install(t, dir, theTag, body)
					return err
				})
				if !errors.Is(err, fs.ErrExist) || !strings.HasPrefix(err.Error(), at.refusal) {
					t.Fatalf("install with %s at %s = %v, want it refused as existing", tc.name, at.name, err)
				}
				if _, err := os.Lstat(target); !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("the install wrote through the symlink to its target (%v)", err)
				}
				if _, err := os.Lstat(dir); !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("the refused install left %s (%v), want nothing there", dir, err)
				}
				if left := leftovers(t, dir); left != nil {
					t.Errorf("the refused install left %v beside the install path", left)
				}
			})
		}
	}
}

func remove(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
}

// The install's one run of the check starts through the caller's starter (ServedInstall.Start),
// which is how the pod's launcher, its container's PID 1, holds the run out of its own reaper:
// the starter is given the staged check's --help, and its done is called only once the run's own
// wait has read the exit. A start the starter refuses refuses the install, and an install named no
// starter at all is refused before it writes anything, rather than run the check under a start
// nobody chose; either leaves nothing behind.
func TestTheInstallsRunOfTheCheckStartsThroughItsStarter(t *testing.T) {
	t.Run("a starter", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "leak-check")
		var started []*exec.Cmd
		var exitAtDone *os.ProcessState
		err := InstallServed(bound(t), ServedInstall{Dir: dir, Tag: theTag, Body: served(t, wholeCheck(t, answersItsHelp)),
			Start: func(cmd *exec.Cmd) (func(), error) {
				started = append(started, cmd)
				return func() { exitAtDone = cmd.ProcessState }, cmd.Start()
			}})
		if err != nil {
			t.Fatalf("install: %v", err)
		}
		if len(started) != 1 || filepath.Base(started[0].Path) != checkName || !slices.Equal(started[0].Args[1:], []string{"--help"}) {
			t.Fatalf("the starter was given %d commands (%v), want the one run of the check's --help", len(started), started)
		}
		if exitAtDone == nil || exitAtDone.ExitCode() != 1 {
			t.Errorf("done ran with the exit %v, want it called once the run's wait had read its exit 1", exitAtDone)
		}
		if got := recordedTag(t, dir); got != theTag+"\n" {
			t.Errorf("the install records %q, want %q", got, theTag+"\n")
		}
	})
	t.Run("a start the starter refuses", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "leak-check")
		refused := errors.New("the guard refused the start")
		err := InstallServed(bound(t), ServedInstall{Dir: dir, Tag: theTag, Body: served(t, wholeCheck(t, answersItsHelp)),
			Start: func(*exec.Cmd) (func(), error) { return nil, refused }})
		if want := "the served check could not be run: " + refused.Error(); err == nil || err.Error() != want || !errors.Is(err, refused) {
			t.Fatalf("install = %v, want %q", err, want)
		}
		if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the refused install left %s (%v), want nothing there", dir, err)
		}
		if left := leftovers(t, dir); len(left) != 0 {
			t.Errorf("the refused install left %v beside %s, want nothing", left, dir)
		}
	})
	t.Run("no starter", func(t *testing.T) {
		parent := t.TempDir()
		dir := filepath.Join(parent, "leak-check")
		err := InstallServed(bound(t), ServedInstall{Dir: dir, Tag: theTag, Body: served(t, wholeCheck(t, answersItsHelp))})
		if want := "the pod's launcher named no starter for the check's run"; err == nil || err.Error() != want {
			t.Fatalf("install = %v, want %q", err, want)
		}
		if entries, err := os.ReadDir(parent); err != nil || len(entries) != 0 {
			t.Errorf("the refused install left %v (%v) in %s, want nothing", entries, err, parent)
		}
	})
}

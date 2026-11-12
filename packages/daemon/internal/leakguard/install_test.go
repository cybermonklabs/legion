package leakguard

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// theTag is the pin these tests install, spelled as the worker image spells one.
const theTag = "leak-check-v15"

// The members the test's own tarball carries under its leak-check directory. They are the shape
// the real one has at leak-check-v15, which this package measured: the launcher, the python file
// it runs from beside itself, a fixture its self-test reads, and a nested directory. A file
// outside that directory is in the tarball too, to show that the rest of the repository is left
// where it is.
const (
	theNeighbour = "public_leak_check.py"
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

// member is one file of the test's tarball: a regular file, or a symbolic link where link is set.
type member struct {
	body string
	mode int64
	link string
}

// repositoryPrefix is the one directory GitHub's tarball of a repository holds every member under.
const repositoryPrefix = "theorem-labs-agent-infra-84d835ba5d00f97212253ffa221898f1f234a772/"

// gzipTar writes a gzipped tarball whose members write adds, and answers its bytes.
func gzipTar(t *testing.T, write func(*tar.Writer)) []byte {
	t.Helper()
	var raw bytes.Buffer
	zipped := gzip.NewWriter(&raw)
	archive := tar.NewWriter(zipped)
	write(archive)
	for _, closer := range []interface{ Close() error }{archive, zipped} {
		if err := closer.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return raw.Bytes()
}

// writeFile adds one member to a test tarball.
func writeFile(t *testing.T, archive *tar.Writer, name string, m member) {
	t.Helper()
	header := &tar.Header{Name: name, Mode: m.mode, Size: int64(len(m.body))}
	if m.link != "" {
		header = &tar.Header{Name: name, Mode: m.mode, Typeflag: tar.TypeSymlink, Linkname: m.link}
	}
	if err := archive.WriteHeader(header); err != nil {
		t.Fatal(err)
	}
	if m.link == "" {
		if _, err := archive.Write([]byte(m.body)); err != nil {
			t.Fatal(err)
		}
	}
}

// tarball writes a gzipped tarball shaped like GitHub's tarball of the check's repository: every
// member under one `<owner>-<repo>-<sha>` directory, the check's own files under that directory's
// `leak-check`, and one file of the repository outside it. It answers its bytes.
func tarball(t *testing.T, files map[string]member) []byte {
	t.Helper()
	return gzipTar(t, func(archive *tar.Writer) {
		if err := archive.WriteHeader(&tar.Header{Name: repositoryPrefix + checkDirectory + "/", Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
			t.Fatal(err)
		}
		writeFile(t, archive, repositoryPrefix+"README.md", member{body: "the rest of the repository\n", mode: 0o644})
		for _, name := range slices.Sorted(maps.Keys(files)) {
			writeFile(t, archive, repositoryPrefix+checkDirectory+"/"+name, files[name])
		}
	})
}

// theCheck is the members of a check that runs: the launcher, its python file, its fixture and a
// nested directory.
func theCheck(launcher string) map[string]member {
	return map[string]member{
		checkName:    {body: launcher, mode: 0o755},
		theNeighbour: {body: "# the check itself\n", mode: 0o644},
		theFixture:   {body: "a line the self-test must pass\n", mode: 0o600},
		theNested:    {body: "# the revert gate\n", mode: 0o644},
	}
}

// wholeCheck is a tarball of a check that runs.
func wholeCheck(t *testing.T, launcher string) []byte {
	t.Helper()
	return tarball(t, theCheck(launcher))
}

// served is what the daemon serves of a tarball: its leak-check directory, packed.
func served(t *testing.T, raw []byte) []byte {
	t.Helper()
	body, _, err := PackTarball(bytes.NewReader(raw))
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
func install(t *testing.T, dir, tag string, body []byte) (string, []string, error) {
	t.Helper()
	var logged []string
	check, err := InstallServed(bound(t), ServedInstall{Dir: dir, Tag: tag, Body: body, Log: func(format string, args ...any) {
		logged = append(logged, format)
	}})
	return check, logged, err
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

// The tag shape is written twice, here and in internal/shimwire, so the wire imports nothing of
// this package; the two must be one shape.
func TestTheTagShapeIsTheWiresOwn(t *testing.T) {
	if tagPattern != shimwire.LeakCheckTagPattern {
		t.Fatalf("leakguard's tag pattern is %q and shimwire's %q, want one pattern", tagPattern, shimwire.LeakCheckTagPattern)
	}
	for tag, want := range map[string]bool{
		"leak-check-v15":       true,
		"leak-check-v1":        true,
		"leak-check-v0":        false,
		"leak-check-v015":      false,
		"leak-check-v":         false,
		"":                     false,
		"v15":                  false,
		"leak-check-v15/../x":  false,
		"leak-check-v15\n":     false,
		"leak-check-v15?x=1":   false,
		"refs/leak-check-v15":  false,
		" leak-check-v15":      false,
		"leak-check-v15-extra": false,
	} {
		if got := ValidTag(tag); got != want {
			t.Errorf("ValidTag(%q) = %v, want %v", tag, got, want)
		}
		if got := shimwire.ValidLeakCheckTag(tag); got != want {
			t.Errorf("shimwire.ValidLeakCheckTag(%q) = %v, want %v", tag, got, want)
		}
	}
}

// The daemon serves the check's directory alone: the files of leak-check/ at the directory's own
// paths, with their bytes and modes, and nothing of the rest of the repository nor its prefix.
func TestThePackHoldsTheDirectoryAlone(t *testing.T) {
	files := theCheck(answersItsHelp)
	body, count, err := PackTarball(bytes.NewReader(tarball(t, files)))
	if err != nil {
		t.Fatalf("pack: %v", err)
	}
	if count != len(files) {
		t.Errorf("the pack counted %d files, want %d", count, len(files))
	}
	members := membersOf(t, body)
	var names []string
	for name := range members {
		names = append(names, name)
	}
	slices.Sort(names)
	want := []string{checkName, theFixture, theNeighbour, theNested}
	slices.Sort(want)
	if !slices.Equal(names, want) {
		t.Fatalf("the pack holds %q, want the directory's own files alone, %q", names, want)
	}
	for name, m := range files {
		got := members[name]
		if got.typeflag != tar.TypeReg || got.body != m.body || got.mode != m.mode {
			t.Errorf("the pack holds %s as (type %q, mode %o, %q), want a regular file of mode %o holding %q",
				name, got.typeflag, got.mode, got.body, m.mode, m.body)
		}
	}
}

// A member of the directory that is neither a directory nor a regular file, one whose path leaves
// the directory, and a tarball that holds no such directory are each refused by name.
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
				files["../escaped.sh"] = member{body: "#!/bin/sh\n", mode: 0o755}
				return tarball(t, files)
			},
			want: `holds the member "` + repositoryPrefix + `leak-check/../escaped.sh", which is no path inside leak-check/`,
		},
		{
			name: "a symbolic link",
			raw: func(t *testing.T) []byte {
				files := theCheck(answersItsHelp)
				files["linked"] = member{link: "/etc/passwd", mode: 0o777}
				return tarball(t, files)
			},
			want: `holds the member "` + repositoryPrefix + `leak-check/linked", which is neither a directory nor a regular file`,
		},
		{
			name: "no leak-check directory",
			raw: func(t *testing.T) []byte {
				return gzipTar(t, func(archive *tar.Writer) {
					writeFile(t, archive, repositoryPrefix+"README.md", member{body: "the rest of the repository\n", mode: 0o644})
					writeFile(t, archive, repositoryPrefix+"other/"+checkName, member{body: answersItsHelp, mode: 0o755})
				})
			},
			want: "holds no leak-check directory",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, files, err := PackTarball(bytes.NewReader(tc.raw(t)))
			if err == nil || err.Error() != tc.want {
				t.Fatalf("pack = %v, want %q", err, tc.want)
			}
			if body != nil || files != 0 {
				t.Errorf("a refused pack answered %d bytes and %d files, want none", len(body), files)
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
	check := filepath.Join(dir, checkName)

	landed, logged, err := install(t, dir, theTag, served(t, wholeCheck(t, marked(theTag))))
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if landed != check {
		t.Errorf("the install answered %q, want the check it landed, %q", landed, check)
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
		if got := int64(info.Mode().Perm()); got != m.mode {
			t.Errorf("the install holds %s with mode %o, want %o", name, got, m.mode)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "README.md")); !os.IsNotExist(err) {
		t.Errorf("stat the repository's own README in the install = %v, want nothing of the repository outside %s", err, checkDirectory)
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
			landed, logged, err := install(t, dir, tc.tag, served(t, wholeCheck(t, marked(tc.marker))))
			if err != nil {
				t.Fatalf("install: %v", err)
			}
			if landed != check {
				t.Errorf("the install answered %q, want %q", landed, check)
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
			body: func(t *testing.T) []byte { return served(t, tarball(t, nil)) },
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
			landed, _, err := install(t, dir, theTag, tc.body(t))
			if err == nil || err.Error() != tc.want {
				t.Fatalf("install = %v, want %q", err, tc.want)
			}
			if landed != "" {
				t.Errorf("a failed install answered %q, want no check", landed)
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
			if _, _, err := install(t, dir, theTag, served(t, wholeCheck(t, marked(theTag)))); err != nil {
				t.Fatalf("the first install: %v", err)
			}
			if _, _, err := install(t, dir, "leak-check-v16", tc.body(t)); err == nil || err.Error() != tc.want {
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
	if _, _, err := install(t, dir, theTag, served(t, wholeCheck(t, printsItsEnvironment))); err != nil {
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

// A role's shim reads the install and starts no agent unless it is the pin's: an absent check, an
// install that records no tag and one of another tag are each refused naming the pin and what is
// there. With no pin, which is every tmux pane, nothing is read and nothing refused.
func TestInstalledAnswersTheCheckOfThePinAlone(t *testing.T) {
	pinned := []string{"PATH=/usr/bin:/bin", TagVariable + "=" + theTag}
	installedAt := func(t *testing.T) string {
		t.Helper()
		dir := filepath.Join(t.TempDir(), "leak-check")
		if _, _, err := install(t, dir, theTag, served(t, wholeCheck(t, answersItsHelp))); err != nil {
			t.Fatalf("install: %v", err)
		}
		return dir
	}

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
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := installedAt(t)
			tc.spoil(t, dir)
			check, err := Installed(pinned, dir)
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

func remove(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
}

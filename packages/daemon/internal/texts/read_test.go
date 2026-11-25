package texts_test

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/texts"
)

// legionText stands for Legion's own text at a path: a folder's manifest records its digest, and
// the folder's own file at the path is other bytes.
func legionText(path string) string { return "Legion's text at " + path + ".\n" }

// folderText is the folder's own file at a path.
func folderText(path string) string { return "The arm's text at " + path + ".\n" }

// writeFolder makes a folder of texts holding a file at each of paths, the folder's own text, and
// a manifest recording, for each, the digest of Legion's text at that path.
func writeFolder(t *testing.T, paths ...string) string {
	t.Helper()
	dir := t.TempDir()
	digests := map[string]string{}
	for _, p := range paths {
		writeFile(t, filepath.Join(dir, filepath.FromSlash(p)), folderText(p))
		digests[p] = texts.Digest([]byte(legionText(p)))
	}
	if err := texts.WriteManifest(dir, digests); err != nil {
		t.Fatal(err)
	}
	return dir
}

func writeFile(t *testing.T, file, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fault is the *texts.Fault err is, or a failure saying what err is instead.
func fault(t *testing.T, err error) *texts.Fault {
	t.Helper()
	var f *texts.Fault
	if !errors.As(err, &f) {
		t.Fatalf("got %v, want a *texts.Fault", err)
	}
	return f
}

// A ConfigMap or Secret volume that projects keys through `items` with nested paths, the only way
// a folder with roles/… and skills/<skill>/… mounts, shows the folder as the kubelet lays it out:
// a timestamped directory that holds every file, a `..data` link to it, and each top-level entry a
// link into `..data`. testdata/volume-layout.txt is that layout as `find` listed it in a Legion
// pod. This builds the same layout, line for line, with the fixture's entries renamed to a
// folder's (its owner directories become roles/, skills/ and agents/, and each of their files a
// text at a nested path), and Read takes it: each file once, by its path in the folder, and no
// `..` name.
func TestReadTakesTheKubernetesVolumeLayout(t *testing.T) {
	listing, err := os.ReadFile(filepath.Join("testdata", "volume-layout.txt"))
	if err != nil {
		t.Fatal(err)
	}
	renamed := map[string]string{
		"renames.txt":             texts.ManifestName,
		"acme":                    "roles",
		"acme/config.yml":         "roles/reviewer.md",
		"acme/hosts.yml":          "roles/core/common.md",
		"example-user":            "skills",
		"example-user/config.yml": "skills/legion-worker/SKILL.md",
		"example-user/hosts.yml":  "skills/legion-worker/references/plan-tests.md",
		"example-org":             "agents",
		"example-org/hosts.yml":   "agents/goal-red-team.md",
		"example-org/config.yml":  "agents/oracle.md",
	}
	// rename is a path of the fixture as the folder's: a `..` element (the kubelet's own) stays,
	// and the path after it is the folder's name for it.
	var rename func(p string) string
	rename = func(p string) string {
		if p == "" {
			return ""
		}
		head, rest, _ := strings.Cut(p, "/")
		if strings.HasPrefix(head, "..") {
			if rest == "" {
				return head
			}
			return head + "/" + rename(rest)
		}
		name, ok := renamed[p]
		if !ok {
			t.Fatalf("testdata/volume-layout.txt lists %s, which this test renames to nothing: rename it to a path of a folder", p)
		}
		return name
	}

	mount := t.TempDir()
	var dataDir string
	links, files := 0, map[string]string{}
	for _, line := range strings.Split(strings.TrimSuffix(string(listing), "\n"), "\n") {
		kind, rest, _ := strings.Cut(line, " ")
		p, target, _ := strings.Cut(rest, " ")
		at := filepath.Join(mount, filepath.FromSlash(rename(p)))
		switch kind {
		case "d":
			if err := os.MkdirAll(at, 0o755); err != nil {
				t.Fatal(err)
			}
		case "f":
			if filepath.Base(at) != texts.ManifestName {
				name := strings.SplitN(rename(p), "/", 2)[1]
				files[name] = folderText(name)
				writeFile(t, at, files[name])
			}
			dataDir = strings.SplitN(rename(p), "/", 2)[0]
		case "l":
			if err := os.Symlink(filepath.FromSlash(rename(target)), at); err != nil {
				t.Fatal(err)
			}
			links++
		default:
			t.Fatalf("testdata/volume-layout.txt line %q is of a type (%q) this test does not build", line, kind)
		}
	}
	if links != 5 || len(files) != 6 || !strings.HasPrefix(dataDir, "..") {
		t.Fatalf("the fixture built %d links, %d texts and the data directory %q; want the kubelet's ..data and four entry links, six texts, and a `..` directory", links, len(files), dataDir)
	}
	digests := map[string]string{}
	for name := range files {
		digests[name] = texts.Digest([]byte(legionText(name)))
	}
	if err := texts.WriteManifest(filepath.Join(mount, dataDir), digests); err != nil {
		t.Fatal(err)
	}
	manifest, err := os.ReadFile(filepath.Join(mount, dataDir, texts.ManifestName))
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(filepath.Join(mount, texts.ManifestName)); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the folder's manifest.json is %v, %v; want the kubelet's link into ..data", info, err)
	}

	folder, err := texts.Read(mount)
	if err != nil {
		t.Fatalf("Read(the volume layout) = %v, want it read", err)
	}
	want := maps.Clone(files)
	want[texts.ManifestName] = string(manifest)
	got := map[string]string{}
	for p, body := range folder.Files {
		got[p] = string(body)
	}
	if !maps.Equal(got, want) {
		t.Errorf("Read(the volume layout) holds %q, want each text once by its path in the folder: %q", slices.Sorted(maps.Keys(got)), slices.Sorted(maps.Keys(want)))
	}
	if !maps.Equal(folder.Manifest.Files, digests) {
		t.Errorf("Read(the volume layout) read the manifest %v, want %v", folder.Manifest.Files, digests)
	}
	if folder.Dir != mount {
		t.Errorf("Read(%s).Dir = %s, want the folder as named", mount, folder.Dir)
	}
}

// A folder Read cannot take as one checked version is refused with its cause and the file it found
// it at ("" being the folder itself), and the refusal names both. The first row is the valid
// folder every other row breaks one way, which Read takes, so each refusal is the row's own.
func TestReadRefusesEachBrokenFolder(t *testing.T) {
	paths := []string{"roles/reviewer.md", "go/reviewer.md", "agents/goal-red-team.md", "skills/legion-worker/references/plan-tests.md"}
	manifestWith := func(dir string, entries map[string]string) {
		t.Helper()
		digests := map[string]string{}
		for _, p := range paths {
			digests[p] = texts.Digest([]byte(legionText(p)))
		}
		maps.Copy(digests, entries)
		if err := texts.WriteManifest(dir, digests); err != nil {
			t.Fatal(err)
		}
	}
	good := "sha256:" + strings.Repeat("ab", 32)
	for _, tc := range []struct {
		name   string
		break_ func(t *testing.T, dir string) string
		// root is whether the row cannot run as root, which reads a mode 000 file.
		root  bool
		cause texts.Cause
		path  string
	}{
		{name: "the valid folder", break_: func(*testing.T, string) string { return "" }},
		{name: "gone", break_: func(_ *testing.T, dir string) string { return filepath.Join(dir, "absent") },
			cause: texts.Gone},
		{name: "a file in place of the folder", break_: func(t *testing.T, dir string) string {
			writeFile(t, filepath.Join(dir, "folder"), "not a folder\n")
			return filepath.Join(dir, "folder")
		}, cause: texts.Unreadable},
		{name: "the folder at mode 000", root: true, break_: func(t *testing.T, dir string) string {
			chmod(t, dir, 0o000)
			return ""
		}, cause: texts.Unreadable},
		{name: "a file at mode 000", root: true, break_: func(t *testing.T, dir string) string {
			chmod(t, filepath.Join(dir, "roles", "reviewer.md"), 0o000)
			return ""
		}, cause: texts.Unreadable, path: "roles/reviewer.md"},
		{name: "no manifest", break_: func(t *testing.T, dir string) string {
			if err := os.Remove(filepath.Join(dir, texts.ManifestName)); err != nil {
				t.Fatal(err)
			}
			return ""
		}, cause: texts.NoManifest, path: texts.ManifestName},
		{name: "a malformed manifest", break_: func(t *testing.T, dir string) string {
			writeFile(t, filepath.Join(dir, texts.ManifestName), `{"files": {"roles/reviewer.md": "sha256:`)
			return ""
		}, cause: texts.ManifestUnreadable, path: texts.ManifestName},
		{name: "a manifest that is no object", break_: func(t *testing.T, dir string) string {
			writeFile(t, filepath.Join(dir, texts.ManifestName), "null\n")
			return ""
		}, cause: texts.ManifestUnreadable, path: texts.ManifestName},
		{name: "a manifest with two objects", break_: func(t *testing.T, dir string) string {
			manifestWith(dir, nil)
			body, _ := os.ReadFile(filepath.Join(dir, texts.ManifestName))
			writeFile(t, filepath.Join(dir, texts.ManifestName), string(body)+"{}\n")
			return ""
		}, cause: texts.ManifestUnreadable, path: texts.ManifestName},
		{name: "an unknown manifest key", break_: func(t *testing.T, dir string) string {
			writeFile(t, filepath.Join(dir, texts.ManifestName), `{"files": {}, "version": 1}`)
			return ""
		}, cause: texts.ManifestUnreadable, path: texts.ManifestName},
		{name: "a manifest key in another case", break_: func(t *testing.T, dir string) string {
			writeFile(t, filepath.Join(dir, texts.ManifestName), `{"Files": {}}`)
			return ""
		}, cause: texts.ManifestUnreadable, path: texts.ManifestName},
		{name: "a digest in upper case", break_: func(_ *testing.T, dir string) string {
			manifestWith(dir, map[string]string{"roles/reviewer.md": strings.ToUpper(good)})
			return ""
		}, cause: texts.ManifestUnreadable, path: texts.ManifestName},
		{name: "a digest of another algorithm", break_: func(_ *testing.T, dir string) string {
			manifestWith(dir, map[string]string{"roles/reviewer.md": "md5:" + strings.Repeat("ab", 16)})
			return ""
		}, cause: texts.ManifestUnreadable, path: texts.ManifestName},
		{name: "a short digest", break_: func(_ *testing.T, dir string) string {
			manifestWith(dir, map[string]string{"roles/reviewer.md": good[:len(good)-1]})
			return ""
		}, cause: texts.ManifestUnreadable, path: texts.ManifestName},
		{name: "a listed path that climbs out", break_: func(_ *testing.T, dir string) string {
			manifestWith(dir, map[string]string{"../x": good})
			return ""
		}, cause: texts.BadPath, path: "../x"},
		{name: "a listed absolute path", break_: func(_ *testing.T, dir string) string {
			manifestWith(dir, map[string]string{"/x": good})
			return ""
		}, cause: texts.BadPath, path: "/x"},
		{name: "a listed path that is not clean", break_: func(_ *testing.T, dir string) string {
			manifestWith(dir, map[string]string{"x/./y": good})
			return ""
		}, cause: texts.BadPath, path: "x/./y"},
		{name: "a listed path under a tree that is not clean", break_: func(_ *testing.T, dir string) string {
			manifestWith(dir, map[string]string{"roles/./reviewer.md": good})
			return ""
		}, cause: texts.BadPath, path: "roles/./reviewer.md"},
		{name: "a listed path with an empty element", break_: func(_ *testing.T, dir string) string {
			manifestWith(dir, map[string]string{"roles//reviewer.md": good})
			return ""
		}, cause: texts.BadPath, path: "roles//reviewer.md"},
		{name: "a listed path outside the four trees", break_: func(t *testing.T, dir string) string {
			writeFile(t, filepath.Join(dir, "notes", "x.md"), "notes\n")
			manifestWith(dir, map[string]string{"notes/x.md": good})
			return ""
		}, cause: texts.BadPath, path: "notes/x.md"},
		{name: "a file outside the four trees", break_: func(t *testing.T, dir string) string {
			writeFile(t, filepath.Join(dir, "README.md"), "a note\n")
			return ""
		}, cause: texts.BadPath, path: "README.md"},
		{name: "an unlisted file", break_: func(t *testing.T, dir string) string {
			writeFile(t, filepath.Join(dir, "roles", "core", "common.md"), folderText("roles/core/common.md"))
			return ""
		}, cause: texts.Unlisted, path: "roles/core/common.md"},
		{name: "a listed file the folder lacks", break_: func(_ *testing.T, dir string) string {
			manifestWith(dir, map[string]string{"roles/architect.md": good})
			return ""
		}, cause: texts.Missing, path: "roles/architect.md"},
		{name: "a link to a file out of the folder", break_: func(t *testing.T, dir string) string {
			outside := filepath.Join(t.TempDir(), "reviewer.md")
			writeFile(t, outside, folderText("roles/reviewer.md"))
			replaceWithLink(t, filepath.Join(dir, "roles", "reviewer.md"), outside)
			return ""
		}, cause: texts.LinkEscapes, path: "roles/reviewer.md"},
		{name: "a link to a directory out of the folder", break_: func(t *testing.T, dir string) string {
			outside := t.TempDir()
			writeFile(t, filepath.Join(outside, "reviewer.md"), folderText("roles/reviewer.md"))
			if err := os.RemoveAll(filepath.Join(dir, "roles")); err != nil {
				t.Fatal(err)
			}
			replaceWithLink(t, filepath.Join(dir, "roles"), outside)
			return ""
		}, cause: texts.LinkEscapes, path: "roles"},
		{name: "a link up the folder by a relative target", break_: func(t *testing.T, dir string) string {
			writeFile(t, filepath.Join(filepath.Dir(dir), "sibling.md"), "beside the folder\n")
			replaceWithLink(t, filepath.Join(dir, "go", "reviewer.md"), filepath.Join("..", "..", "sibling.md"))
			return ""
		}, cause: texts.LinkEscapes, path: "go/reviewer.md"},
		{name: "a link back to a directory that holds it", break_: func(t *testing.T, dir string) string {
			replaceWithLink(t, filepath.Join(dir, "roles", "loop"), "..")
			return ""
		}, cause: texts.Unreadable, path: "roles/loop"},
		{name: "a dangling link", break_: func(t *testing.T, dir string) string {
			replaceWithLink(t, filepath.Join(dir, "roles", "reviewer.md"), "absent.md")
			return ""
		}, cause: texts.Unreadable, path: "roles/reviewer.md"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.root && os.Geteuid() == 0 {
				t.Skip("as root a mode 000 file or folder still reads, so this refusal cannot be made")
			}
			dir := writeFolder(t, paths...)
			if named := tc.break_(t, dir); named != "" {
				dir = named
			}
			folder, err := texts.Read(dir)
			if tc.cause == "" {
				if err != nil {
					t.Fatalf("Read(the valid folder) = %v, want it read", err)
				}
				if len(folder.Files) != len(paths)+1 {
					t.Errorf("Read(the valid folder) holds %q, want its %d texts and its manifest", slices.Sorted(maps.Keys(folder.Files)), len(paths))
				}
				return
			}
			if err == nil {
				t.Fatalf("Read(%s) took the folder, want the cause %s at %q", tc.name, tc.cause, tc.path)
			}
			got := fault(t, err)
			if got.Cause != tc.cause || got.Path != tc.path || got.Dir != dir {
				t.Errorf("Read(%s) = %+v, want the cause %s at %q in %s", tc.name, *got, tc.cause, tc.path, dir)
			}
			for _, word := range []string{dir, tc.path, "[" + string(tc.cause) + "]"} {
				if !strings.Contains(err.Error(), word) {
					t.Errorf("Read(%s)'s refusal %q does not name %q", tc.name, err, word)
				}
			}
		})
	}

	// A named pipe in the folder is refused at once: a read of it would wait for a writer that
	// never comes.
	t.Run("a named pipe", func(t *testing.T) {
		dir := writeFolder(t, paths...)
		pipe := filepath.Join(dir, "roles", "reviewer.md")
		if err := os.Remove(pipe); err != nil {
			t.Fatal(err)
		}
		if err := syscall.Mkfifo(pipe, 0o644); err != nil {
			t.Fatal(err)
		}
		read := make(chan error, 1)
		go func() {
			_, err := texts.Read(dir)
			read <- err
		}()
		err := testwait.Receive(t, "Read of a folder holding a named pipe", read)
		if got := fault(t, err); got.Cause != texts.Unreadable || got.Path != "roles/reviewer.md" {
			t.Errorf("Read(a folder holding a named pipe) = %+v, want unreadable at roles/reviewer.md", *got)
		}
	})
}

func chmod(t *testing.T, file string, mode os.FileMode) {
	t.Helper()
	if err := os.Chmod(file, mode); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(file, 0o755) })
}

func replaceWithLink(t *testing.T, file, target string) {
	t.Helper()
	if err := os.Remove(file); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if err := os.Symlink(target, file); err != nil {
		t.Fatal(err)
	}
}

// The fingerprint is how the launcher tells that the folder it reads is the one the daemon read,
// and how a notice tells one cut of a folder from the next: SHA-256 over one line `<path>\t<sha256
// of its bytes>\n` for each file, manifest.json included, in the order of their paths. So it is the
// same for the same files wherever they sit and whatever their modes and times, and it changes
// with every byte of every file, the manifest's own included, and with a file's path.
func TestTheFingerprintChangesWithAnyFileAndOnlyThen(t *testing.T) {
	paths := []string{"roles/reviewer.md", "go/reviewer.md", "agents/goal-red-team.md", "skills/legion-worker/SKILL.md"}
	dir := writeFolder(t, paths...)
	first := read(t, dir)

	var lines strings.Builder
	for _, p := range slices.Sorted(maps.Keys(first.Files)) {
		sum := sha256.Sum256(first.Files[p])
		fmt.Fprintf(&lines, "%s\t%s\n", p, hex.EncodeToString(sum[:]))
	}
	if sum := sha256.Sum256([]byte(lines.String())); first.Fingerprint != hex.EncodeToString(sum[:]) {
		t.Errorf("the fingerprint is %s, want the SHA-256 of\n%s", first.Fingerprint, lines.String())
	}

	// The same files: read again, at another path, at other modes and times.
	again := t.TempDir()
	for p, body := range first.Files {
		writeFile(t, filepath.Join(again, filepath.FromSlash(p)), string(body))
		if err := os.Chmod(filepath.Join(again, filepath.FromSlash(p)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, same := range []texts.Folder{read(t, dir), read(t, again)} {
		if same.Fingerprint != first.Fingerprint {
			t.Errorf("the same files at %s read as the fingerprint %s, want %s", same.Dir, same.Fingerprint, first.Fingerprint)
		}
	}

	// Each change to one file, the manifest's own bytes and a file's path among them, is a new
	// fingerprint, and no two of them are the same.
	seen := map[string]string{first.Fingerprint: "the folder as cut"}
	change := func(what string, edit func(dir string)) {
		t.Helper()
		changed := t.TempDir()
		for p, body := range first.Files {
			writeFile(t, filepath.Join(changed, filepath.FromSlash(p)), string(body))
		}
		edit(changed)
		got := read(t, changed).Fingerprint
		if earlier, ok := seen[got]; ok {
			t.Errorf("%s reads as the fingerprint of %s, %s", what, earlier, got)
		}
		seen[got] = what
	}
	for _, p := range paths {
		change("one byte more in "+p, func(dir string) {
			writeFile(t, filepath.Join(dir, filepath.FromSlash(p)), string(first.Files[p])+" ")
		})
	}
	change("the manifest re-indented", func(dir string) {
		writeFile(t, filepath.Join(dir, texts.ManifestName), strings.ReplaceAll(string(first.Files[texts.ManifestName]), "  ", "\t"))
	})
	change("a file's bytes at another path", func(dir string) {
		if err := os.Rename(filepath.Join(dir, "roles", "reviewer.md"), filepath.Join(dir, "roles", "tester.md")); err != nil {
			t.Fatal(err)
		}
		digests := maps.Clone(first.Manifest.Files)
		digests["roles/tester.md"] = digests["roles/reviewer.md"]
		delete(digests, "roles/reviewer.md")
		if err := texts.WriteManifest(dir, digests); err != nil {
			t.Fatal(err)
		}
	})
}

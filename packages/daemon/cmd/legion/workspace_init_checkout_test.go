package main

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/execfile"
	"github.com/sjawhar/legion/daemon/internal/fixturegit"
)

// fixtureTreeFile is the path and the content withFixtureTree gives the tree's i-th nested file.
func fixtureTreeFile(i int) (rel, body string) {
	return fmt.Sprintf("d%02d/s%02d/f%04d.txt", i/100, (i/10)%10, i), fmt.Sprintf("file %04d body line\n", i)
}

// withFixtureTree gives the volume a bare remote whose defaultBranch holds one commit containing
// nestedCount files in nested directories, an executable file, a symlink, an empty file and a
// 5 MiB file. It writes the commit directly through `git fast-import`, so 3,000+ files commit in
// one process. It returns the total number of tree entries and the 5 MiB file's bytes.
func (v *issueVolume) withFixtureTree(t *testing.T, defaultBranch string, nestedCount int) (int, []byte) {
	t.Helper()
	dir := t.TempDir()
	remote := filepath.Join(dir, "remote.git")
	v.runner(t).Run(dir, fixturegit.InitBare(remote, defaultBranch)...)

	var stream bytes.Buffer
	fmt.Fprintf(&stream, "commit refs/heads/%s\n", defaultBranch)
	stream.WriteString("committer Legion test <legion-test@example.invalid> 1700000000 +0000\n")
	stream.WriteString("data 12\nfixture tree\n")

	writeInline := func(mode, path string, content []byte) {
		fmt.Fprintf(&stream, "M %s inline %s\ndata %d\n", mode, path, len(content))
		stream.Write(content)
		stream.WriteByte('\n')
	}

	for i := range nestedCount {
		rel, body := fixtureTreeFile(i)
		writeInline("100644", rel, []byte(body))
	}
	writeInline("100755", "bin/run.sh", []byte("#!/bin/sh\necho ok\n"))
	writeInline("120000", "links/to-first", []byte("../d00/s00/f0000.txt"))
	writeInline("100644", "empty.txt", nil)

	large := make([]byte, 5<<20)
	for i := range large {
		large[i] = byte((i*31 + 17) & 0xff)
	}
	writeInline("100644", "large.bin", large)
	stream.WriteString("done\n")

	cmd := exec.Command(v.env["WINIT_REAL_GIT"], "--git-dir="+remote, "fast-import", "--quiet")
	cmd.Dir = dir
	cmd.Env = v.runner(t).Env
	cmd.Stdin = &stream
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git fast-import: %v\n%s", err, out)
	}

	v.env["WINIT_REMOTE"] = remote
	v.defaultBranch = defaultBranch
	return nestedCount + 4, large
}

type treeEntry struct {
	mode string
	sha  string
	path string
}

// assertWholeCheckout holds every R2-5 assertion on the checked-out workspace ws: every path of
// `git ls-tree -r HEAD` is on disk with its blob's content (`git hash-object`) and mode, `find`
// counts as many files and links, `jj diff --summary` and `git status --porcelain
// --untracked-files=no` print nothing, `jj sparse list` prints `.`, and no `.jj/legion-checkout`
// marker remains.
func assertWholeCheckout(t *testing.T, v *issueVolume, ws string, wantCount int) {
	t.Helper()
	rawTree := v.git(t, "-C", ws, "ls-tree", "-r", "HEAD")
	if rawTree == "" {
		t.Fatal("git ls-tree -r HEAD printed nothing")
	}
	lines := strings.Split(rawTree, "\n")
	if len(lines) != wantCount {
		t.Fatalf("git ls-tree -r HEAD listed %d entries, want %d", len(lines), wantCount)
	}

	var regularPaths strings.Builder
	regularEntries := make([]treeEntry, 0, len(lines))
	for _, line := range lines {
		meta, path, ok := strings.Cut(line, "\t")
		if !ok {
			t.Fatalf("unexpected ls-tree line: %q", line)
		}
		fields := strings.Fields(meta)
		if len(fields) != 3 || fields[1] != "blob" {
			t.Fatalf("unexpected ls-tree metadata: %q", line)
		}
		e := treeEntry{mode: fields[0], sha: fields[2], path: path}
		full := filepath.Join(ws, filepath.FromSlash(e.path))
		info, err := os.Lstat(full)
		if err != nil {
			t.Fatalf("missing path %s: %v", e.path, err)
		}
		switch e.mode {
		case "120000":
			if info.Mode()&os.ModeSymlink == 0 {
				t.Errorf("%s: mode %v, want symlink", e.path, info.Mode())
				continue
			}
			target, err := os.Readlink(full)
			if err != nil {
				t.Fatalf("readlink %s: %v", e.path, err)
			}
			gotSHA := v.gitInput(t, target, "-C", ws, "hash-object", "--stdin")
			if gotSHA != e.sha {
				t.Errorf("symlink %s hash-object = %s (target %q), want %s", e.path, gotSHA, target, e.sha)
			}
		case "100755":
			if !info.Mode().IsRegular() {
				t.Errorf("%s: mode %v, want regular executable file", e.path, info.Mode())
			}
			if info.Mode().Perm()&0o100 == 0 {
				t.Errorf("%s: perm %o has no user execute bit", e.path, info.Mode().Perm())
			}
			regularPaths.WriteString(e.path)
			regularPaths.WriteByte('\n')
			regularEntries = append(regularEntries, e)
		case "100644":
			if !info.Mode().IsRegular() {
				t.Errorf("%s: mode %v, want regular file", e.path, info.Mode())
			}
			if info.Mode().Perm()&0o111 != 0 {
				t.Errorf("%s: perm %o has execute bits, want 100644", e.path, info.Mode().Perm())
			}
			regularPaths.WriteString(e.path)
			regularPaths.WriteByte('\n')
			regularEntries = append(regularEntries, e)
		default:
			t.Fatalf("unexpected mode %q for %s", e.mode, e.path)
		}
	}

	gotSHAs := strings.Split(v.gitInput(t, regularPaths.String(), "-C", ws, "hash-object", "--stdin-paths"), "\n")
	if len(gotSHAs) != len(regularEntries) {
		t.Fatalf("git hash-object returned %d hashes for %d regular files", len(gotSHAs), len(regularEntries))
	}
	for i, e := range regularEntries {
		if gotSHAs[i] != e.sha {
			t.Errorf("%s: git hash-object = %s, want %s", e.path, gotSHAs[i], e.sha)
		}
	}

	findCmd := exec.Command("find", ws,
		"(", "-path", filepath.Join(ws, ".git"), "-o", "-path", filepath.Join(ws, ".jj"), ")", "-prune",
		"-o", "(", "-type", "f", "-o", "-type", "l", ")", "-print",
	)
	findOut, err := findCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("find: %v\n%s", err, findOut)
	}
	found := 0
	for _, line := range strings.Split(strings.TrimSpace(string(findOut)), "\n") {
		if line != "" {
			found++
		}
	}
	if found != len(lines) {
		t.Errorf("find counted %d files and links outside .git/.jj, want %d", found, len(lines))
	}

	if diff := v.jj(t, "diff", "--summary", "-R", ws); diff != "" {
		t.Errorf("jj diff --summary = %q, want empty", diff)
	}
	if status := v.git(t, "-C", ws, "status", "--porcelain", "--untracked-files=no"); status != "" {
		t.Errorf("git status --porcelain --untracked-files=no = %q, want empty", status)
	}
	if sparse := v.jj(t, "sparse", "list", "-R", ws); sparse != "." {
		t.Errorf("jj sparse list = %q, want .", sparse)
	}
	if _, err := os.Lstat(filepath.Join(ws, ".jj", "legion-checkout")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("after a finished checkout, .jj/legion-checkout lstat = %v, want ErrNotExist", err)
	}
}

// R2-5: provisioning checks out every file of a 3,000-file tree with nested directories, an
// executable file, a symlink, an empty file and a 5 MiB file, matching every blob and mode.
func TestWorkspaceInitChecksOutEveryFileOfTheTree(t *testing.T) {
	t.Parallel()
	v := newIssueVolume(t)
	wantCount, _ := v.withFixtureTree(t, "main", 3000)
	v.provisionEnv()

	code, stdout, stderr := v.answered(t, "provision 3000-file tree", v.args("LEGION-42"))
	if code != 0 {
		t.Fatalf("provision exited %d:\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	assertWholeCheckout(t, v, v.workspace("LEGION-42"), wantCount)
}

// R3-2: when a first provision's `jj sparse reset` is cut short (exiting 137 after writing half
// of the tree and half of one file's bytes), `.jj/legion-checkout` is present while it runs, and
// a second provision with the real jj removes the workspace and adds it again (workspace-reset),
// checking out every file cleanly.
func TestWorkspaceInitRedoesACheckoutThatWasCutShort(t *testing.T) {
	t.Parallel()
	v := newIssueVolume(t)
	const nestedCount = 40
	wantCount, large := v.withFixtureTree(t, "main", nestedCount)
	v.provisionEnv()

	// Stage half of the tree's files plus the first half of large.bin's bytes.
	partial := t.TempDir()
	for i := range nestedCount / 2 {
		rel, body := fixtureTreeFile(i)
		full := filepath.Join(partial, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(partial, "large.bin"), large[:len(large)/2], 0o644); err != nil {
		t.Fatal(err)
	}

	record := filepath.Join(t.TempDir(), "marker.txt")
	cutScript := filepath.Join(t.TempDir(), "cut-checkout.sh")
	body := "#!/bin/sh\n" +
		"if [ -f .jj/legion-checkout ]; then printf 'present\\n' > '" + record + "'; else printf 'absent\\n' > '" + record + "'; fi\n" +
		"cp -RP '" + partial + "/.' .\n" +
		"exit 137\n"
	if err := execfile.Write(cutScript, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}

	v.env["WINIT_CUT_CHECKOUT"] = cutScript
	code, stdout, stderr := v.answered(t, "cut-short first provision", v.args("LEGION-42"))
	if code == 0 {
		t.Fatalf("first provision succeeded despite cut-short stand-in:\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
	}

	recorded, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("read marker record: %v", err)
	}
	if got := strings.TrimSpace(string(recorded)); got != "present" {
		t.Errorf("the checkout stand-in recorded .jj/legion-checkout as %q, want present", got)
	}

	// Confirm the truncated file is currently on disk in the workspace.
	ws := v.workspace("LEGION-42")
	cutInfo, err := os.Stat(filepath.Join(ws, "large.bin"))
	if err != nil {
		t.Fatalf("stat cut large.bin: %v", err)
	}
	if cutInfo.Size() != int64(len(large)/2) {
		t.Fatalf("cut large.bin size = %d, want %d", cutInfo.Size(), len(large)/2)
	}

	// Second provision with the real jj must remove the half-written workspace and add it again.
	v.env["WINIT_CUT_CHECKOUT"] = ""
	code, stdout, stderr = v.answered(t, "second provision after cut-short checkout", v.args("LEGION-42"))
	if code != 0 {
		t.Fatalf("second provision exited %d:\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	assertWholeCheckout(t, v, ws, wantCount)
}

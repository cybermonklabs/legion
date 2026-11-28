package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cmdtest"
)

// TestChanged runs changed in a scratch git repository: commit A holds a/data.json and a/a.go, B is
// A with `git mv a/data.json docs/data.json`, C adds to B a file whose name holds a line break, and
// D adds to B a directory whose tree object the clone then loses. Each git command of the test
// reads no configuration of this machine (gitIn), and each run of the command has gitEnv in its
// environment too; the command drops each variable GIT_* before it runs git (specgraph.GitHistory),
// so its own git commands read this machine's configuration. C and D are made with git's plumbing
// (update-index, write-tree, commit-tree), since a git wrapper that checks a commit's text can
// refuse such a name, and the test is about what changed prints.
//
// Each run over A and B is a parallel subtest over the one clone, which none of them changes:
// changed's git commands only read it. Making C and D changes the clone they are made in (a file of
// its work tree, its index, the object that the test removes), so they are made in a copy of the
// clone at B, which the test takes before any subtest starts, and their two runs are one subtest,
// in order, whose expected messages name the copy.
func TestChanged(t *testing.T) {
	t.Parallel()
	binary := cmdtest.Build(t, "spec-check")
	repo := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		return gitIn(t, repo, args...)
	}
	git("init", "-q")
	if err := os.MkdirAll(filepath.Join(repo, "a"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(repo, "a", "data.json"), []byte("{\"answer\": 42}\n"))
	writeFile(t, filepath.Join(repo, "a", "a.go"), []byte("package a\n"))
	git("add", ".")
	git("commit", "-q", "-m", "A")
	a := git("rev-parse", "HEAD")
	if err := os.Mkdir(filepath.Join(repo, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	git("mv", "a/data.json", "docs/data.json")
	git("commit", "-q", "-m", "B")
	b := git("rev-parse", "HEAD")

	// The control: git's own diff reads the move as a rename and names the new path alone, so a
	// command that left rename detection on would miss a/data.json.
	if got := git("diff", "--name-only", a, b); got != "docs/data.json" {
		t.Fatalf("git diff --name-only A B = %q; the control wants docs/data.json alone", got)
	}
	// The copy of the clone at B, in which C and D are made, so that the runs over A and B read a
	// clone that nothing changes while they run. copyOf (os.CopyFS) copies a symbolic link as a link
	// to the same target, which could lead back into the clone, so the clone must hold none.
	if err := filepath.WalkDir(repo, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.Type()&fs.ModeSymlink != 0 {
			t.Fatalf("the clone holds the symbolic link %s, which its copy would share", path)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	copied := copyOf(t, repo)

	// The first line names the base by its full object name, whatever name --base gave it.
	t.Run("the base by its full name", func(t *testing.T) {
		t.Parallel()
		expect(t, runBinaryIn(t, binary, "", gitEnv, "changed", "--repo", repo, "--base", a, "--head", b), result{
			stdout: "base " + a + "\na/data.json\ndocs/data.json\n",
			stderr: "spec-check: " + repo + ": 2 paths changed from " + a + " to " + b + "\n"})
	})
	// --head defaults to HEAD, which is B.
	t.Run("head defaults to HEAD", func(t *testing.T) {
		t.Parallel()
		expect(t, runBinaryIn(t, binary, "", gitEnv, "changed", "--repo", repo, "--base", a), result{
			stdout: "base " + a + "\na/data.json\ndocs/data.json\n",
			stderr: "spec-check: " + repo + ": 2 paths changed from " + a + " to HEAD\n"})
	})
	t.Run("a base of HEAD~1", func(t *testing.T) {
		t.Parallel()
		expect(t, runBinaryIn(t, binary, "", gitEnv, "changed", "--repo", repo, "--base", "HEAD~1"), result{
			stdout: "base " + a + "\na/data.json\ndocs/data.json\n",
			stderr: "spec-check: " + repo + ": 2 paths changed from HEAD~1 to HEAD\n"})
	})
	t.Run("no path from B", func(t *testing.T) {
		t.Parallel()
		expect(t, runBinaryIn(t, binary, "", gitEnv, "changed", "--repo", repo, "--base", b), result{
			stdout: "base " + b + "\n",
			stderr: "spec-check: " + repo + ": 0 paths changed from " + b + " to HEAD\n"})
	})

	unknown := strings.Repeat("0", 39) + "1"
	t.Run("an unknown base", func(t *testing.T) {
		t.Parallel()
		expect(t, runBinaryIn(t, binary, "", gitEnv, "changed", "--repo", repo, "--base", unknown), result{code: 2,
			stderr: "spec-check: " + repo + ": git cannot read commit " + unknown + "\n"})
	})
	t.Run("a head of --output=x", func(t *testing.T) {
		t.Parallel()
		expect(t, runBinaryIn(t, binary, "", gitEnv, "changed", "--repo", repo, "--base", a, "--head", "--output=x"), result{code: 2,
			stderr: "spec-check: " + repo + ": git cannot read commit --output=x\n"})
		if _, err := os.Stat(filepath.Join(repo, "x")); err == nil {
			t.Error("git read --output=x as an option and wrote a file")
		}
	})
	t.Run("a missing repository", func(t *testing.T) {
		t.Parallel()
		missing := filepath.Join(t.TempDir(), "missing")
		expect(t, runBinaryIn(t, binary, "", gitEnv, "changed", "--repo", missing, "--base", a), result{code: 2, stderr: "spec-check: " + missing + ": does not exist\n"})
	})

	// --repo is the top of its clone, from which changed names each path: a directory below the top
	// is refused before git reads a commit, with the top that git names to pass instead, and so is
	// a directory where git finds no work tree, such as the clone's .git. The top passes, also
	// through a link to it.
	t.Run("a directory below the top", func(t *testing.T) {
		t.Parallel()
		top := gitIn(t, repo, "rev-parse", "--show-toplevel")
		below := filepath.Join(repo, "a")
		expect(t, runBinaryIn(t, binary, "", gitEnv, "changed", "--repo", below, "--base", unknown), result{code: 2,
			stderr: "spec-check: " + below + ": is not the top of its git clone; pass the top, --repo " + top + "\n"})
	})
	t.Run("the .git directory", func(t *testing.T) {
		t.Parallel()
		gitDir := filepath.Join(repo, ".git")
		if got, line := runBinaryIn(t, binary, "", gitEnv, "changed", "--repo", gitDir, "--base", a), "spec-check: "+gitDir+": git finds no work tree there: "; got.code != 2 ||
			got.stdout != "" || !strings.HasPrefix(got.stderr, line) || strings.Count(got.stderr, "\n") != 1 {
			t.Errorf("changed --repo %s: exit %d, stdout %q, stderr %q; want exit 2 and one line that starts %q", gitDir, got.code, got.stdout, got.stderr, line)
		}
	})
	t.Run("the top through a link", func(t *testing.T) {
		t.Parallel()
		link := filepath.Join(t.TempDir(), "link")
		if err := os.Symlink(repo, link); err != nil {
			t.Fatal(err)
		}
		expect(t, runBinaryIn(t, binary, "", gitEnv, "changed", "--repo", link, "--base", a), result{
			stdout: "base " + a + "\na/data.json\ndocs/data.json\n",
			stderr: "spec-check: " + link + ": 2 paths changed from " + a + " to HEAD\n"})
	})

	// C and D change the clone they are made in, so they are made in the copy, in order: D's
	// read-tree puts back B's index, to which C added the line break's path.
	t.Run("C and D, in a copy of the clone", func(t *testing.T) {
		t.Parallel()
		git := func(args ...string) string {
			t.Helper()
			return gitIn(t, copied, args...)
		}
		writeFile(t, filepath.Join(copied, "a", "line\nbreak.go"), []byte("package a\n"))
		blob := git("hash-object", "-w", "a/line\nbreak.go")
		git("update-index", "--add", "--cacheinfo", "100644,"+blob+",a/line\nbreak.go")
		c := git("commit-tree", git("write-tree"), "-p", b, "-m", "C")
		expect(t, runBinaryIn(t, binary, "", gitEnv, "changed", "--repo", copied, "--base", b, "--head", c), result{code: 2,
			stderr: "spec-check: " + copied + `: the path "a/line\nbreak.go" holds a line break, which a list of one path a line cannot hold` + "\n"})

		// The copy holds D, so git reads the commit, but git diff cannot read the tree of gone:
		// changed prints the fault of the history, which names the two commits, the clone and git's
		// reason, and gives no list, where a list of the paths would read as a diff that git made.
		git("read-tree", b)
		git("update-index", "--add", "--cacheinfo", "100644,"+blob+",gone/x")
		d := git("commit-tree", git("write-tree"), "-p", b, "-m", "D")
		gone := git("rev-parse", d+":gone")
		if err := os.Remove(filepath.Join(copied, ".git", "objects", gone[:2], gone[2:])); err != nil {
			t.Fatal(err)
		}
		line := "spec-check: specgraph: history: git cannot read the commits " + strconv.Quote(b) + " and " + strconv.Quote(d) + " of the clone at "
		if got := runBinaryIn(t, binary, "", gitEnv, "changed", "--repo", copied, "--base", b, "--head", d); got.code != 2 || got.stdout != "" ||
			!strings.HasPrefix(got.stderr, line) || strings.Count(got.stderr, "\n") != 1 {
			t.Errorf("changed --base B --head D: exit %d, stdout %q, stderr %q; want exit 2 and one line that starts %q", got.code, got.stdout, got.stderr, line)
		}
	})
}

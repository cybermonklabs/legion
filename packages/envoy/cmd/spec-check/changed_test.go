package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cmdtest"
)

// TestChanged runs changed in a scratch git repository: commit A holds a/data.json and a/a.go, B is
// A with `git mv a/data.json docs/data.json`, C adds to B a file whose name holds a line break, and
// D adds to B a directory whose tree object the clone then loses. Each git command, the command's
// own included, reads no configuration of this machine. C and D are made with git's plumbing
// (update-index, write-tree, commit-tree), since a git wrapper that checks a commit's text can
// refuse such a name, and the test is about what changed prints.
func TestChanged(t *testing.T) {
	binary := cmdtest.Build(t, "spec-check")
	for _, entry := range gitEnv {
		name, value, _ := strings.Cut(entry, "=")
		t.Setenv(name, value)
	}
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
	// The first line names the base by its full object name, whatever name --base gave it.
	expect(t, runBinary(t, binary, "", "changed", "--repo", repo, "--base", a, "--head", b), result{
		stdout: "base " + a + "\na/data.json\ndocs/data.json\n",
		stderr: "spec-check: " + repo + ": 2 paths changed from " + a + " to " + b + "\n"})
	// --head defaults to HEAD, which is B.
	expect(t, runBinary(t, binary, "", "changed", "--repo", repo, "--base", a), result{
		stdout: "base " + a + "\na/data.json\ndocs/data.json\n",
		stderr: "spec-check: " + repo + ": 2 paths changed from " + a + " to HEAD\n"})
	expect(t, runBinary(t, binary, "", "changed", "--repo", repo, "--base", "HEAD~1"), result{
		stdout: "base " + a + "\na/data.json\ndocs/data.json\n",
		stderr: "spec-check: " + repo + ": 2 paths changed from HEAD~1 to HEAD\n"})
	expect(t, runBinary(t, binary, "", "changed", "--repo", repo, "--base", b), result{
		stdout: "base " + b + "\n",
		stderr: "spec-check: " + repo + ": 0 paths changed from " + b + " to HEAD\n"})

	unknown := strings.Repeat("0", 39) + "1"
	expect(t, runBinary(t, binary, "", "changed", "--repo", repo, "--base", unknown), result{code: 2,
		stderr: "spec-check: " + repo + ": git cannot read commit " + unknown + "\n"})
	expect(t, runBinary(t, binary, "", "changed", "--repo", repo, "--base", a, "--head", "--output=x"), result{code: 2,
		stderr: "spec-check: " + repo + ": git cannot read commit --output=x\n"})
	if _, err := os.Stat(filepath.Join(repo, "x")); err == nil {
		t.Error("git read --output=x as an option and wrote a file")
	}
	missing := filepath.Join(t.TempDir(), "missing")
	expect(t, runBinary(t, binary, "", "changed", "--repo", missing, "--base", a), result{code: 2, stderr: "spec-check: " + missing + ": does not exist\n"})

	// --repo is the top of its clone, from which changed names each path: a directory below the top
	// is refused before git reads a commit, with the top that git names to pass instead, and so is
	// a directory where git finds no work tree, such as the clone's .git. The top passes, also
	// through a link to it.
	top := git("rev-parse", "--show-toplevel")
	below := filepath.Join(repo, "a")
	expect(t, runBinary(t, binary, "", "changed", "--repo", below, "--base", unknown), result{code: 2,
		stderr: "spec-check: " + below + ": is not the top of its git clone; pass the top, --repo " + top + "\n"})
	gitDir := filepath.Join(repo, ".git")
	if got, line := runBinary(t, binary, "", "changed", "--repo", gitDir, "--base", a), "spec-check: "+gitDir+": git finds no work tree there: "; got.code != 2 ||
		got.stdout != "" || !strings.HasPrefix(got.stderr, line) || strings.Count(got.stderr, "\n") != 1 {
		t.Errorf("changed --repo %s: exit %d, stdout %q, stderr %q; want exit 2 and one line that starts %q", gitDir, got.code, got.stdout, got.stderr, line)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(repo, link); err != nil {
		t.Fatal(err)
	}
	expect(t, runBinary(t, binary, "", "changed", "--repo", link, "--base", a), result{
		stdout: "base " + a + "\na/data.json\ndocs/data.json\n",
		stderr: "spec-check: " + link + ": 2 paths changed from " + a + " to HEAD\n"})

	writeFile(t, filepath.Join(repo, "a", "line\nbreak.go"), []byte("package a\n"))
	blob := git("hash-object", "-w", "a/line\nbreak.go")
	git("update-index", "--add", "--cacheinfo", "100644,"+blob+",a/line\nbreak.go")
	c := git("commit-tree", git("write-tree"), "-p", b, "-m", "C")
	expect(t, runBinary(t, binary, "", "changed", "--repo", repo, "--base", b, "--head", c), result{code: 2,
		stderr: "spec-check: " + repo + `: the path "a/line\nbreak.go" holds a line break, which a list of one path a line cannot hold` + "\n"})

	// The clone holds D, so git reads the commit, but git diff cannot read the tree of gone: changed
	// prints the fault of the history, which names the two commits, the clone and git's reason, and
	// gives no list, where a list of the paths would read as a diff that git made.
	git("read-tree", b)
	git("update-index", "--add", "--cacheinfo", "100644,"+blob+",gone/x")
	d := git("commit-tree", git("write-tree"), "-p", b, "-m", "D")
	gone := git("rev-parse", d+":gone")
	if err := os.Remove(filepath.Join(repo, ".git", "objects", gone[:2], gone[2:])); err != nil {
		t.Fatal(err)
	}
	line := "spec-check: specgraph: history: git cannot read the commits " + strconv.Quote(b) + " and " + strconv.Quote(d) + " of the clone at "
	if got := runBinary(t, binary, "", "changed", "--repo", repo, "--base", b, "--head", d); got.code != 2 || got.stdout != "" ||
		!strings.HasPrefix(got.stderr, line) || strings.Count(got.stderr, "\n") != 1 {
		t.Errorf("changed --base B --head D: exit %d, stdout %q, stderr %q; want exit 2 and one line that starts %q", got.code, got.stdout, got.stderr, line)
	}
}

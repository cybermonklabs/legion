package specgraph

import (
	"encoding/hex"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/specgraph/specgraphtest"
)

// treeFiles is each path under root in the form of specgraphtest.PutTree, and "dir" for an empty directory.
func treeFiles(t *testing.T, root string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || path == root {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(path)
			files[rel] = "link " + target
			return err
		case d.IsDir():
			entries, err := os.ReadDir(path)
			if len(entries) == 0 {
				files[rel] = "dir"
			}
			return err
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		info, err := d.Info()
		kind := "file "
		if err == nil && info.Mode()&0o111 != 0 {
			kind = "exec "
		}
		files[rel] = kind + string(content)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// commitIndex commits the index of the clone at dir as it is, on parent ("" for none), with git's
// plumbing (write-tree, commit-tree), makes it HEAD and answers it: a gitlink staged with
// update-index stays, which add -A would take out.
func commitIndex(t *testing.T, dir, parent, message string) string {
	t.Helper()
	args := []string{"commit-tree", runTestGit(t, dir, "write-tree"), "-m", message}
	if parent != "" {
		args = append(args, "-p", parent)
	}
	commit := runTestGit(t, dir, args...)
	runTestGit(t, dir, "update-ref", "HEAD", commit)
	return commit
}

// commitWorkTree commits every file of the work tree of the clone at dir, on parent ("" for none),
// as commitIndex does, and answers it.
func commitWorkTree(t *testing.T, dir, parent, message string) string {
	t.Helper()
	runTestGit(t, dir, "add", "-A")
	return commitIndex(t, dir, parent, message)
}

// R8, R48: the base commit of spec-check citations and the tie that its checkout gives are facts
// of the commits alone, as Changed's list is (TestGitHistoryIsTheSameUnderEachSetting): a tie at
// the base that misses a file of the base's tree would carry a result across a change of its code,
// and a commit name of another clone would judge another commit. So no setting of the repository,
// of its .gitattributes or of the process changes what CommitName names or what Checkout writes.
// Each row: a clone with commits A and B and the row's setting, then CommitName of B, which must be
// B, and the checkout of B, which must hold the tree of B as git stores it: each file's own bytes
// and its executable bit, each link as a link, and a gitlink as an empty directory. The shallow
// row's clone does not hold A: CommitName of A is false and its checkout an error. The settings of
// the process are set after the commits; the test's own git reads none of them (runTestGit).
func TestTheGitCallsOfACheckoutAreTheSameUnderEachSetting(t *testing.T) {
	const sub1, sub2 = "1111111111111111111111111111111111111111", "2222222222222222222222222222222222222222"
	tree := map[string]string{"a.go": "file package p\n", "run.sh": "exec #!/bin/sh\n", "link": "link a.go", "d/e.txt": "file e\n"}
	with := func(extra map[string]string) map[string]string {
		m := maps.Clone(tree)
		maps.Copy(m, extra)
		return m
	}
	// plain commits A, a.go alone, and B, the tree with extra, and answers both.
	plain := func(t *testing.T, dir string, extra map[string]string) (string, string) {
		specgraphtest.PutTree(t, dir, map[string]string{"a.go": tree["a.go"]})
		a := commitWorkTree(t, dir, "", "A")
		specgraphtest.PutTree(t, dir, with(extra))
		return a, commitWorkTree(t, dir, a, "B")
	}
	// submodule commits A, the tree with .gitmodules and the gitlink sub at sub1, then B with sub at
	// sub2: a gitlink names a commit of the submodule, which this clone does not need to hold.
	modules := func(ignore string) string {
		m := "[submodule \"sub\"]\n\tpath = sub\n\turl = ./sub.git\n"
		if ignore != "" {
			m += "\tignore = " + ignore + "\n"
		}
		return m
	}
	submodule := func(t *testing.T, dir, ignore string) (string, string) {
		specgraphtest.PutTree(t, dir, with(map[string]string{".gitmodules": "file " + modules(ignore)}))
		runTestGit(t, dir, "add", "-A")
		runTestGit(t, dir, "update-index", "--add", "--cacheinfo", "160000,"+sub1+",sub")
		a := commitIndex(t, dir, "", "A")
		runTestGit(t, dir, "update-index", "--cacheinfo", "160000,"+sub2+",sub")
		return a, commitIndex(t, dir, a, "B")
	}
	odd := map[string]string{"é.md": "file 1\n", "a b.md": "file 1\n", "a\tb.md": "file 1\n", `a"b.md`: "file 1\n", "a\nb.md": "file 1\n"}
	for _, row := range []struct {
		name    string
		make    func(t *testing.T, dir string) (a, b, repo string)
		want    map[string]string
		shallow bool
	}{
		{name: "control: no setting", make: func(t *testing.T, dir string) (string, string, string) {
			a, b := plain(t, dir, nil)
			return a, b, dir
		}, want: tree},
		{name: "a moved submodule whose committed .gitmodules sets ignore = all", make: func(t *testing.T, dir string) (string, string, string) {
			a, b := submodule(t, dir, "all")
			return a, b, dir
		}, want: with(map[string]string{".gitmodules": "file " + modules("all"), "sub": "dir"})},
		{name: "a moved submodule whose .git/config sets ignore = all", make: func(t *testing.T, dir string) (string, string, string) {
			a, b := submodule(t, dir, "")
			runTestGit(t, dir, "config", "submodule.sub.ignore", "all")
			return a, b, dir
		}, want: with(map[string]string{".gitmodules": "file " + modules(""), "sub": "dir"})},
		{name: "a moved submodule with diff.ignoreSubmodules = all", make: func(t *testing.T, dir string) (string, string, string) {
			a, b := submodule(t, dir, "")
			runTestGit(t, dir, "config", "diff.ignoreSubmodules", "all")
			return a, b, dir
		}, want: with(map[string]string{".gitmodules": "file " + modules(""), "sub": "dir"})},
		{name: "names with a letter that is not ASCII, a space, a tab, a quote and a line feed, with core.quotePath = true",
			make: func(t *testing.T, dir string) (string, string, string) {
				runTestGit(t, dir, "config", "core.quotePath", "true")
				a, b := plain(t, dir, odd)
				return a, b, dir
			}, want: with(odd)},
		{name: "a move with diff.renames = copies", make: func(t *testing.T, dir string) (string, string, string) {
			runTestGit(t, dir, "config", "diff.renames", "copies")
			specgraphtest.PutTree(t, dir, map[string]string{"p/data.json": `file {"a":1}` + "\n"})
			a := commitWorkTree(t, dir, "", "A")
			runTestGit(t, dir, "mv", "p/data.json", "docs.json")
			specgraphtest.PutTree(t, dir, tree)
			return a, commitWorkTree(t, dir, a, "B"), dir
		}, want: with(map[string]string{"docs.json": `file {"a":1}` + "\n"})},
		{name: "a change of a letter's case with core.ignoreCase = true", make: func(t *testing.T, dir string) (string, string, string) {
			runTestGit(t, dir, "config", "core.ignoreCase", "true")
			specgraphtest.PutTree(t, dir, map[string]string{"C.go": "file package p\n"})
			a := commitWorkTree(t, dir, "", "A")
			runTestGit(t, dir, "mv", "C.go", "c.go")
			specgraphtest.PutTree(t, dir, tree)
			return a, commitWorkTree(t, dir, a, "B"), dir
		}, want: with(map[string]string{"c.go": "file package p\n"})},
		{name: "a .gitattributes line that makes each file no text, and a textconv that gives each one text",
			make: func(t *testing.T, dir string) (string, string, string) {
				a, b := plain(t, dir, map[string]string{".gitattributes": "file * -diff\n*.txt diff=same\n"})
				runTestGit(t, dir, "config", "diff.same.textconv", "echo same")
				return a, b, dir
			}, want: with(map[string]string{".gitattributes": "file * -diff\n*.txt diff=same\n"})},
		{name: "a replace ref that puts another commit in the place of B", make: func(t *testing.T, dir string) (string, string, string) {
			a, b := plain(t, dir, nil)
			specgraphtest.PutTree(t, dir, map[string]string{"c.go": "file package p\n"})
			runTestGit(t, dir, "replace", b, commitWorkTree(t, dir, b, "C"))
			return a, b, dir
		}, want: tree},
		{name: "diff.relative = true, with the repository at a directory of the clone", make: func(t *testing.T, dir string) (string, string, string) {
			runTestGit(t, dir, "config", "diff.relative", "true")
			a, b := plain(t, dir, nil)
			return a, b, filepath.Join(dir, "d")
		}, want: tree},
		{name: "color.ui = always", make: func(t *testing.T, dir string) (string, string, string) {
			runTestGit(t, dir, "config", "color.ui", "always")
			runTestGit(t, dir, "config", "color.diff", "always")
			a, b := plain(t, dir, nil)
			return a, b, dir
		}, want: tree},
		{name: "GIT_DIR of the process naming another clone", make: func(t *testing.T, dir string) (string, string, string) {
			a, b := plain(t, dir, nil)
			other := t.TempDir()
			runTestGit(t, other, "init", "-q")
			t.Setenv("GIT_DIR", filepath.Join(other, ".git"))
			return a, b, dir
		}, want: tree},
		{name: "GIT_WORK_TREE of the process naming another directory", make: func(t *testing.T, dir string) (string, string, string) {
			a, b := plain(t, dir, nil)
			t.Setenv("GIT_WORK_TREE", t.TempDir())
			return a, b, dir
		}, want: tree},
		{name: "GIT_INDEX_FILE of the process naming another index", make: func(t *testing.T, dir string) (string, string, string) {
			a, b := plain(t, dir, nil)
			t.Setenv("GIT_INDEX_FILE", filepath.Join(t.TempDir(), "index"))
			return a, b, dir
		}, want: tree},
		{name: "GIT_OBJECT_DIRECTORY of the process naming the objects of another clone", make: func(t *testing.T, dir string) (string, string, string) {
			a, b := plain(t, dir, nil)
			other := t.TempDir()
			runTestGit(t, other, "init", "-q")
			t.Setenv("GIT_OBJECT_DIRECTORY", filepath.Join(other, ".git", "objects"))
			return a, b, dir
		}, want: tree},
		{name: "a shallow clone that does not hold A", make: func(t *testing.T, dir string) (string, string, string) {
			a, b := plain(t, dir, nil)
			shallow := filepath.Join(t.TempDir(), "shallow")
			runTestGit(t, dir, "clone", "-q", "--depth", "1", "file://"+dir, shallow)
			return a, b, shallow
		}, want: tree, shallow: true},
		{name: "a filter driver of .gitattributes with a smudge command", make: func(t *testing.T, dir string) (string, string, string) {
			a, b := plain(t, dir, map[string]string{".gitattributes": "file *.txt filter=up\n"})
			runTestGit(t, dir, "config", "filter.up.smudge", "tr a-z A-Z")
			return a, b, dir
		}, want: with(map[string]string{".gitattributes": "file *.txt filter=up\n"})},
		{name: "text eol=crlf in .gitattributes", make: func(t *testing.T, dir string) (string, string, string) {
			a, b := plain(t, dir, map[string]string{".gitattributes": "file *.txt text eol=crlf\n"})
			return a, b, dir
		}, want: with(map[string]string{".gitattributes": "file *.txt text eol=crlf\n"})},
		{name: "core.autocrlf = true", make: func(t *testing.T, dir string) (string, string, string) {
			a, b := plain(t, dir, nil)
			runTestGit(t, dir, "config", "core.autocrlf", "true")
			return a, b, dir
		}, want: tree},
		{name: "ident in .gitattributes", make: func(t *testing.T, dir string) (string, string, string) {
			a, b := plain(t, dir, map[string]string{".gitattributes": "file *.txt ident\n", "d/id.txt": "file $Id$\n"})
			return a, b, dir
		}, want: with(map[string]string{".gitattributes": "file *.txt ident\n", "d/id.txt": "file $Id$\n"})},
		{name: "core.symlinks = false", make: func(t *testing.T, dir string) (string, string, string) {
			a, b := plain(t, dir, nil)
			runTestGit(t, dir, "config", "core.symlinks", "false")
			return a, b, dir
		}, want: tree},
		{name: "core.sparseCheckout with a pattern that leaves a file of the tree out", make: func(t *testing.T, dir string) (string, string, string) {
			a, b := plain(t, dir, nil)
			runTestGit(t, dir, "config", "core.sparseCheckout", "true")
			specgraphtest.PutTree(t, filepath.Join(dir, ".git", "info"), map[string]string{"sparse-checkout": "file /a.go\n"})
			return a, b, dir
		}, want: tree},
	} {
		t.Run(row.name, func(t *testing.T) {
			dir := t.TempDir()
			runTestGit(t, dir, "init", "-q")
			a, b, repo := row.make(t, dir)
			h := GitHistory{Dir: repo}
			if name, ok := h.CommitName(b); !ok || name != b {
				t.Errorf("CommitName(B) = %q, %t; want B, %s, true", name, ok, b)
			}
			base, err := h.Checkout(b)
			if err != nil {
				t.Fatalf("Checkout(B): %v", err)
			}
			defer base.Remove()
			if got := treeFiles(t, base.Tree()); !maps.Equal(got, row.want) {
				t.Errorf("Checkout(B) wrote %q; want %q", got, row.want)
			}
			// The index of the checkout is what the base's go-test-reads.sh lists through Environ: it
			// must list each path of B's tree, and no path of a commit that a replace ref puts in B's
			// place.
			ls := exec.Command("git", "ls-files", "-z")
			ls.Dir, ls.Env = base.Tree(), base.Environ
			out, err := ls.Output()
			if err != nil {
				t.Fatalf("git ls-files in the checkout of B: %v", err)
			}
			if got, want := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00"), slices.Sorted(maps.Keys(row.want)); !slices.Equal(got, want) {
				t.Errorf("the index of the checkout of B lists %q; want %q", got, want)
			}
			if row.shallow {
				if name, ok := h.CommitName(a); ok {
					t.Errorf("CommitName(A) = %q, true; want false: the clone does not hold A", name)
				}
				if base, err := h.Checkout(a); err == nil {
					base.Remove()
					t.Error("Checkout(A) gave no error; the clone does not hold A")
				}
			}
		})
	}
}

// hashObject writes content to the objects of the clone at dir as an object of kind, with no check
// of its form (--literally), and answers its name.
func hashObject(t *testing.T, dir, kind string, content []byte) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "object")
	if err := os.WriteFile(file, content, 0o644); err != nil {
		t.Fatal(err)
	}
	return runTestGit(t, dir, "hash-object", "-w", "--literally", "-t", kind, file)
}

// rawTree is the bytes of a tree object of the entries, each "<mode> <name> <object>", in their
// order: no rule of git's checks them.
func rawTree(t *testing.T, entries ...string) []byte {
	t.Helper()
	var tree []byte
	for _, entry := range entries {
		parts := strings.SplitN(entry, " ", 3)
		object, err := hex.DecodeString(parts[2])
		if err != nil {
			t.Fatal(err)
		}
		tree = append(append(append(tree, parts[0]+" "+parts[1]...), 0), object...)
	}
	return tree
}

// A checkout writes its own files, so it keeps the rules by which git writes none outside the work
// tree: a tree that git's own commands refuse to write, which only a tree object made by hand can
// hold, is an error, and nothing is written outside the checkout's directory. Each row is a commit
// of a tree made with hash-object --literally: a part "..", which is no repository path
// (isRepoPath), a part ".git", and a link "l" to a directory outside beside a directory "l" with a
// file in it, whose file a writer that made the link first would write through the link. The
// control is a tree of a link to that directory alone, which is written as a link.
func TestACheckoutWritesNothingOutsideItsDirectory(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name    string
		entries func(link, file string) []string // the tree's entries, by the objects of the link and of a tree "f"
		fault   string                           // what the error holds, or "" for a checkout
	}{
		{"a part ..", func(_, file string) []string { return []string{"40000 .. " + file} },
			`the tree holds the path "../f", which is no repository path, whose parts "/" separates and none of which is empty, "." or ".."`},
		{"a part .git", func(_, file string) []string { return []string{"40000 .git " + file} },
			`the tree holds the path ".git/f", whose part ".git" names the git directory, which git checks out nowhere`},
		{"a link and a directory of one name", func(link, file string) []string {
			return []string{"120000 l " + link, "40000 l " + file}
		}, "l: file exists"},
		{"control: the link alone", func(link, _ string) []string { return []string{"120000 l " + link} }, ""},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			dir, outside := t.TempDir(), t.TempDir()
			runTestGit(t, dir, "init", "-q")
			link := hashObject(t, dir, "blob", []byte(outside))
			file := hashObject(t, dir, "tree", rawTree(t, "100644 f "+hashObject(t, dir, "blob", []byte("1\n"))))
			tree := hashObject(t, dir, "tree", rawTree(t, row.entries(link, file)...))
			commit := runTestGit(t, dir, "commit-tree", tree, "-m", row.name)
			base, err := GitHistory{Dir: dir}.Checkout(commit)
			if err == nil {
				defer base.Remove()
			}
			switch {
			case row.fault == "" && err != nil:
				t.Fatalf("Checkout: %v; want a checkout", err)
			case row.fault == "":
				if got := treeFiles(t, base.Tree()); !maps.Equal(got, map[string]string{"l": "link " + outside}) {
					t.Errorf("Checkout wrote %q; want the link l to %s", got, outside)
				}
			case err == nil || !strings.Contains(err.Error(), row.fault):
				t.Errorf("Checkout: %v; want an error that holds %q", err, row.fault)
			}
			if written, err := os.ReadDir(outside); err != nil || len(written) != 0 {
				t.Errorf("the directory outside holds %d entries (%v); want none", len(written), err)
			}
		})
	}
}

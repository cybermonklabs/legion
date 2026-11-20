package main

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

	"github.com/sjawhar/envoy/internal/dispatch/specgraph"
)

// putTree writes each entry of files under dir: "file <bytes>" a file, "exec <bytes>" an executable
// file, and "link <target>" a link.
func putTree(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for path, entry := range files {
		kind, value, _ := strings.Cut(entry, " ")
		full := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		var err error
		switch kind {
		case "file":
			err = os.WriteFile(full, []byte(value), 0o644)
		case "exec":
			err = os.WriteFile(full, []byte(value), 0o755)
		case "link":
			err = os.Symlink(value, full)
		default:
			t.Fatalf("an entry of kind %q", kind)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
}

// treeFiles is each path under root in the form of putTree, and "dir" for an empty directory.
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

// commitIndex commits the index of the clone at dir as it is, on parent ("" for none), makes it
// HEAD and answers it: a gitlink staged with update-index stays, which add -A would take out.
func commitIndex(t *testing.T, dir, parent, message string) string {
	t.Helper()
	args := []string{"commit-tree", gitIn(t, dir, "write-tree"), "-m", message}
	if parent != "" {
		args = append(args, "-p", parent)
	}
	commit := gitIn(t, dir, args...)
	gitIn(t, dir, "update-ref", "HEAD", commit)
	return commit
}

// R8, R48: the base commit of --changed and the tie that its checkout gives are facts of the
// commits alone, as GitHistory's list is (TestGitHistoryIsTheSameUnderEachSetting in specgraph): a
// tie at the base that misses a file of the base's tree would carry a result across a change of its
// code, and a commit name of another clone would judge another commit. So no setting of the
// repository, of its .gitattributes or of the process changes what commitName names or what
// checkout writes. Each row: a clone with commits A and B and the row's setting, then commitName of
// B, which must be B, and the checkout of B, which must hold the tree of B as git stores it: each
// file's own bytes and its executable bit, each link as a link, and a gitlink as an empty
// directory. The shallow row's clone does not hold A: commitName of A is false and its checkout an
// error. The settings of the process are set after the commits, since the test's own git reads
// them too.
func TestTheGitCallsOfTheBaseAreTheSameUnderEachSetting(t *testing.T) {
	const sub1, sub2 = "1111111111111111111111111111111111111111", "2222222222222222222222222222222222222222"
	tree := map[string]string{"a.go": "file package p\n", "run.sh": "exec #!/bin/sh\n", "link": "link a.go", "d/e.txt": "file e\n"}
	with := func(extra map[string]string) map[string]string {
		m := maps.Clone(tree)
		maps.Copy(m, extra)
		return m
	}
	// plain commits A, a.go alone, and B, the tree with extra, and answers both.
	plain := func(t *testing.T, dir string, extra map[string]string) (string, string) {
		putTree(t, dir, map[string]string{"a.go": tree["a.go"]})
		a := commitAll(t, dir, "", "A")
		putTree(t, dir, with(extra))
		return a, commitAll(t, dir, a, "B")
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
		putTree(t, dir, with(map[string]string{".gitmodules": "file " + modules(ignore)}))
		gitIn(t, dir, "add", "-A")
		gitIn(t, dir, "update-index", "--add", "--cacheinfo", "160000,"+sub1+",sub")
		a := commitIndex(t, dir, "", "A")
		gitIn(t, dir, "update-index", "--cacheinfo", "160000,"+sub2+",sub")
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
			gitIn(t, dir, "config", "submodule.sub.ignore", "all")
			return a, b, dir
		}, want: with(map[string]string{".gitmodules": "file " + modules(""), "sub": "dir"})},
		{name: "a moved submodule with diff.ignoreSubmodules = all", make: func(t *testing.T, dir string) (string, string, string) {
			a, b := submodule(t, dir, "")
			gitIn(t, dir, "config", "diff.ignoreSubmodules", "all")
			return a, b, dir
		}, want: with(map[string]string{".gitmodules": "file " + modules(""), "sub": "dir"})},
		{name: "names with a letter that is not ASCII, a space, a tab, a quote and a line feed, with core.quotePath = true",
			make: func(t *testing.T, dir string) (string, string, string) {
				gitIn(t, dir, "config", "core.quotePath", "true")
				a, b := plain(t, dir, odd)
				return a, b, dir
			}, want: with(odd)},
		{name: "a move with diff.renames = copies", make: func(t *testing.T, dir string) (string, string, string) {
			gitIn(t, dir, "config", "diff.renames", "copies")
			putTree(t, dir, map[string]string{"p/data.json": `file {"a":1}` + "\n"})
			a := commitAll(t, dir, "", "A")
			gitIn(t, dir, "mv", "p/data.json", "docs.json")
			putTree(t, dir, tree)
			return a, commitAll(t, dir, a, "B"), dir
		}, want: with(map[string]string{"docs.json": `file {"a":1}` + "\n"})},
		{name: "a change of a letter's case with core.ignoreCase = true", make: func(t *testing.T, dir string) (string, string, string) {
			gitIn(t, dir, "config", "core.ignoreCase", "true")
			putTree(t, dir, map[string]string{"C.go": "file package p\n"})
			a := commitAll(t, dir, "", "A")
			gitIn(t, dir, "mv", "C.go", "c.go")
			putTree(t, dir, tree)
			return a, commitAll(t, dir, a, "B"), dir
		}, want: with(map[string]string{"c.go": "file package p\n"})},
		{name: "a .gitattributes line that makes each file no text, and a textconv that gives each one text",
			make: func(t *testing.T, dir string) (string, string, string) {
				a, b := plain(t, dir, map[string]string{".gitattributes": "file * -diff\n*.txt diff=same\n"})
				gitIn(t, dir, "config", "diff.same.textconv", "echo same")
				return a, b, dir
			}, want: with(map[string]string{".gitattributes": "file * -diff\n*.txt diff=same\n"})},
		{name: "a replace ref that puts another commit in the place of B", make: func(t *testing.T, dir string) (string, string, string) {
			a, b := plain(t, dir, nil)
			putTree(t, dir, map[string]string{"c.go": "file package p\n"})
			gitIn(t, dir, "replace", b, commitAll(t, dir, b, "C"))
			return a, b, dir
		}, want: tree},
		{name: "diff.relative = true, with the repository at a directory of the clone", make: func(t *testing.T, dir string) (string, string, string) {
			gitIn(t, dir, "config", "diff.relative", "true")
			a, b := plain(t, dir, nil)
			return a, b, filepath.Join(dir, "d")
		}, want: tree},
		{name: "color.ui = always", make: func(t *testing.T, dir string) (string, string, string) {
			gitIn(t, dir, "config", "color.ui", "always")
			gitIn(t, dir, "config", "color.diff", "always")
			a, b := plain(t, dir, nil)
			return a, b, dir
		}, want: tree},
		{name: "GIT_DIR of the process naming another clone", make: func(t *testing.T, dir string) (string, string, string) {
			a, b := plain(t, dir, nil)
			other := t.TempDir()
			gitIn(t, other, "init", "-q")
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
			gitIn(t, other, "init", "-q")
			t.Setenv("GIT_OBJECT_DIRECTORY", filepath.Join(other, ".git", "objects"))
			return a, b, dir
		}, want: tree},
		{name: "a shallow clone that does not hold A", make: func(t *testing.T, dir string) (string, string, string) {
			a, b := plain(t, dir, nil)
			shallow := filepath.Join(t.TempDir(), "shallow")
			gitIn(t, dir, "clone", "-q", "--depth", "1", "file://"+dir, shallow)
			return a, b, shallow
		}, want: tree, shallow: true},
		{name: "a filter driver of .gitattributes with a smudge command", make: func(t *testing.T, dir string) (string, string, string) {
			a, b := plain(t, dir, map[string]string{".gitattributes": "file *.txt filter=up\n"})
			gitIn(t, dir, "config", "filter.up.smudge", "tr a-z A-Z")
			return a, b, dir
		}, want: with(map[string]string{".gitattributes": "file *.txt filter=up\n"})},
		{name: "text eol=crlf in .gitattributes", make: func(t *testing.T, dir string) (string, string, string) {
			a, b := plain(t, dir, map[string]string{".gitattributes": "file *.txt text eol=crlf\n"})
			return a, b, dir
		}, want: with(map[string]string{".gitattributes": "file *.txt text eol=crlf\n"})},
		{name: "core.autocrlf = true", make: func(t *testing.T, dir string) (string, string, string) {
			a, b := plain(t, dir, nil)
			gitIn(t, dir, "config", "core.autocrlf", "true")
			return a, b, dir
		}, want: tree},
		{name: "ident in .gitattributes", make: func(t *testing.T, dir string) (string, string, string) {
			a, b := plain(t, dir, map[string]string{".gitattributes": "file *.txt ident\n", "d/id.txt": "file $Id$\n"})
			return a, b, dir
		}, want: with(map[string]string{".gitattributes": "file *.txt ident\n", "d/id.txt": "file $Id$\n"})},
		{name: "core.symlinks = false", make: func(t *testing.T, dir string) (string, string, string) {
			a, b := plain(t, dir, nil)
			gitIn(t, dir, "config", "core.symlinks", "false")
			return a, b, dir
		}, want: tree},
		{name: "core.sparseCheckout with a pattern that leaves a file of the tree out", make: func(t *testing.T, dir string) (string, string, string) {
			a, b := plain(t, dir, nil)
			gitIn(t, dir, "config", "core.sparseCheckout", "true")
			putTree(t, filepath.Join(dir, ".git", "info"), map[string]string{"sparse-checkout": "file /a.go\n"})
			return a, b, dir
		}, want: tree},
	} {
		t.Run(row.name, func(t *testing.T) {
			dir := t.TempDir()
			gitIn(t, dir, "init", "-q")
			a, b, repo := row.make(t, dir)
			if name, ok := commitName(repo, b); !ok || name != b {
				t.Errorf("commitName(B) = %q, %t; want B, %s, true", name, ok, b)
			}
			base, err := checkout(repo, b)
			if err != nil {
				t.Fatalf("checkout(B): %v", err)
			}
			defer os.RemoveAll(base)
			if got := treeFiles(t, filepath.Join(base, "tree")); !maps.Equal(got, row.want) {
				t.Errorf("checkout(B) wrote %q; want %q", got, row.want)
			}
			if row.shallow {
				if name, ok := commitName(repo, a); ok {
					t.Errorf("commitName(A) = %q, true; want false: the clone does not hold A", name)
				}
				if base, err := checkout(repo, a); err == nil {
					os.RemoveAll(base)
					t.Error("checkout(A) gave no error; the clone does not hold A")
				}
			}
		})
	}
}

// hashObject writes content to the objects of the clone at dir as an object of kind, with no check
// of its form (--literally), and answers its name.
func hashObject(t *testing.T, dir, kind string, content []byte) string {
	t.Helper()
	cmd := exec.Command("git", "-C", dir, "hash-object", "-w", "--literally", "-t", kind, "--stdin")
	cmd.Env = append(cmd.Environ(), gitEnv...)
	cmd.Stdin = strings.NewReader(string(content))
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git hash-object -t %s: %v", kind, err)
	}
	return strings.TrimSpace(string(out))
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

// The checkout writes its own files, so it keeps the rules by which git writes none outside the
// work tree: a tree that git's own commands refuse to write, which only a tree object made by hand
// can hold, is an error, and nothing is written outside the checkout's directory. Each row is a
// commit of a tree made with hash-object --literally: a part "..", a part ".git", and a link "l"
// to a directory outside beside a directory "l" with a file in it, whose file a writer that made
// the link first would write through the link. The control is a tree of a link to that directory
// alone, which is written as a link.
func TestTheCheckoutWritesNothingOutsideItsDirectory(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name    string
		entries func(link, file string) []string // the tree's entries, by the objects of the link and of a tree "f"
		fault   string                           // what the error holds, or "" for a checkout
	}{
		{"a part ..", func(_, file string) []string { return []string{"40000 .. " + file} },
			`the tree holds the path "../f", whose part ".." git checks out nowhere`},
		{"a part .git", func(_, file string) []string { return []string{"40000 .git " + file} },
			`the tree holds the path ".git/f", whose part ".git" git checks out nowhere`},
		{"a link and a directory of one name", func(link, file string) []string {
			return []string{"120000 l " + link, "40000 l " + file}
		}, "l: file exists"},
		{"control: the link alone", func(link, _ string) []string { return []string{"120000 l " + link} }, ""},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			dir, outside := t.TempDir(), t.TempDir()
			gitIn(t, dir, "init", "-q")
			link := hashObject(t, dir, "blob", []byte(outside))
			file := hashObject(t, dir, "tree", rawTree(t, "100644 f "+hashObject(t, dir, "blob", []byte("1\n"))))
			tree := hashObject(t, dir, "tree", rawTree(t, row.entries(link, file)...))
			commit := gitIn(t, dir, "commit-tree", tree, "-m", row.name)
			base, err := checkout(dir, commit)
			if err == nil {
				defer os.RemoveAll(base)
			}
			switch {
			case row.fault == "" && err != nil:
				t.Fatalf("checkout: %v; want a checkout", err)
			case row.fault == "":
				if got := treeFiles(t, filepath.Join(base, "tree")); !maps.Equal(got, map[string]string{"l": "link " + outside}) {
					t.Errorf("checkout wrote %q; want the link l to %s", got, outside)
				}
			case err == nil || !strings.Contains(err.Error(), row.fault):
				t.Errorf("checkout: %v; want an error that holds %q", err, row.fault)
			}
			if written, err := os.ReadDir(outside); err != nil || len(written) != 0 {
				t.Errorf("the directory outside holds %d entries (%v); want none", len(written), err)
			}
		})
	}
}

// readsScripts is each file of this repository's .github/scripts that go-test-reads.sh runs: the
// script, its engine and the two modules the engine imports. ci_job_reads.py's row for this file
// names them.
var readsScripts = []string{"go-test-reads.sh", "ci_filter_coverage.py", "workflow_paths.py", "go_build_tags.py"}

// R8, R18: the tie at the base commit is what the base's own go-test-reads.sh says, and that script
// lists the repository's files with git ls-files (ci_filter_coverage.py's repo_files), as this
// repository's ci_job_reads.py does when it builds its tables (files_but_tests). So the script must
// read git's list of the base's tree in the checkout of the base. The fixture repository holds this
// repository's script and engine (readsScripts) and a table that builds its one RULES row from
// repo_files, as the repository's own table does: lib_test.go reads every file under fixtures/. The
// base commit holds fixtures/data.json; the head deletes it and adds fixtures/other.json, so a tie
// at the base that read the clone's own index or work tree would name other.json. The tie at the
// base must be known and name data.json and not other.json. The control is the tie at the head,
// from the clone itself, which names other.json.
func TestTheBaseTieReadsGitsListOfTheBase(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	files := map[string]string{
		".github/scripts/ci_job_reads.py": "file from ci_filter_coverage import Read, repo_files\n\n" +
			"RULES = {\"mod/lib/lib_test.go\": [Read(path, \"lib_test.go reads the files under ../../fixtures\")\n" +
			"                                 for path in repo_files() if path.startswith(\"fixtures/\")]}\n",
		"go.work":             "file go 1.21\n\nuse ./mod\n",
		"mod/go.mod":          "file module example.com/m\n\ngo 1.21\n",
		"mod/lib/lib.go":      "file package lib\n\nfunc Hello() {}\n",
		"mod/lib/lib_test.go": "file package lib\n\nimport (\n\t\"os\"\n\t\"testing\"\n)\n\nfunc TestData(t *testing.T) {\n\tif _, err := os.ReadDir(\"../../fixtures\"); err != nil {\n\t\tt.Fatal(err)\n\t}\n}\n",
		"fixtures/data.json":  "file {}\n",
	}
	for _, name := range readsScripts {
		content, err := os.ReadFile(filepath.Join("..", "..", "..", "..", ".github", "scripts", name))
		if err != nil {
			t.Fatal(err)
		}
		kind := "file "
		if strings.HasSuffix(name, ".sh") {
			kind = "exec "
		}
		files[filepath.Join(".github", "scripts", name)] = kind + string(content)
	}
	putTree(t, repo, files)
	gitIn(t, repo, "init", "-q")
	base := commitAll(t, repo, "", "base")
	if err := os.Remove(filepath.Join(repo, "fixtures", "data.json")); err != nil {
		t.Fatal(err)
	}
	putTree(t, repo, map[string]string{"fixtures/other.json": "file {}\n"})
	commitAll(t, repo, base, "head")
	check := specgraph.Check{ID: "the-data-is-read", Procedure: specgraph.ProcedureProgram,
		Test: &specgraph.Test{Runner: "go", Selector: "example.com/m/lib TestData", Job: "go-checks"}}

	head := newDerivedTies(repo, nil).tie(check)
	if !head.Known || !slices.Contains(head.Paths, "fixtures/other.json") || slices.Contains(head.Paths, "fixtures/data.json") {
		t.Fatalf("the control: the tie at the head = %+v; want a known tie that names fixtures/other.json and not fixtures/data.json", head)
	}
	atBase := &baseCheckout{repo: repo, commit: base}
	defer atBase.close()
	got := atBase.tie(check)
	if !got.Known || !slices.Contains(got.Paths, "fixtures/data.json") || slices.Contains(got.Paths, "fixtures/other.json") ||
		!slices.Contains(got.Paths, "mod/lib/*") {
		t.Errorf("the tie at the base = %+v; want a known tie that names fixtures/data.json and mod/lib/* and not fixtures/other.json", got)
	}
}

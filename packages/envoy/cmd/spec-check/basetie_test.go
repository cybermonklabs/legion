package main

//ci:reads .github/scripts/go-test-reads.sh readsRepo copies it into a fixture repository.
//ci:reads .github/scripts/ci_filter_coverage.py readsRepo copies it into a fixture repository.
//ci:reads .github/scripts/workflow_paths.py readsRepo copies it into a fixture repository.
//ci:reads .github/scripts/go_build_tags.py readsRepo copies it into a fixture repository.

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/specgraph"
	"github.com/sjawhar/envoy/internal/dispatch/specgraph/specgraphtest"
)

// readsScripts is each file of this repository's .github/scripts that go-test-reads.sh runs: the
// script, its engine and the two modules the engine imports. ci_job_reads.py's row for this file
// names them.
var readsScripts = []string{"go-test-reads.sh", "ci_filter_coverage.py", "workflow_paths.py", "go_build_tags.py"}

// readsRepo is a git repository, with no commit, that holds this repository's script and engine
// (readsScripts), a module example.com/m with the package lib in mod/lib, bound by a go.work, and a
// table that builds its one RULES row from repo_files, as the repository's own table does:
// lib_test.go reads every file under fixtures/, which holds data.json.
func readsRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	files := map[string]string{
		".github/scripts/ci_job_reads.py": "file from ci_filter_coverage import Read, repo_files\n\n" +
			"RULES = {\"mod/lib/lib_test.go\": [Read(\"fixtures/*.json\", \"lib_test.go reads fixture JSON\"),\n" +
			"                                 *[Read(path, \"lib_test.go reads the files under ../../fixtures\")\n" +
			"                                   for path in repo_files() if path.startswith(\"fixtures/\")]]}\n",
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
	specgraphtest.PutTree(t, repo, files)
	gitIn(t, repo, "init", "-q")
	return repo
}

// R8, R18: the tie at the base commit is what the base's own go-test-reads.sh says, and that script
// lists the repository's files with git ls-files (ci_filter_coverage.py's repo_files), as this
// repository's ci_job_reads.py does when it builds its tables (files_but_tests). So the script must
// read git's list of the base's tree in the checkout of the base. The fixture repository is
// readsRepo. The base commit holds fixtures/data.json; the head deletes it and adds
// fixtures/other.json, so a tie at the base that read the clone's own index or work tree would name
// other.json. The tie at the base must be known and name data.json and not other.json. The control
// is the tie at the head, from the clone itself, which names other.json.
func TestTheBaseTieReadsGitsListOfTheBase(t *testing.T) {
	t.Parallel()
	repo := readsRepo(t)
	base := commitAll(t, repo, "", "base")
	if err := os.Remove(filepath.Join(repo, "fixtures", "data.json")); err != nil {
		t.Fatal(err)
	}
	specgraphtest.PutTree(t, repo, map[string]string{"fixtures/other.json": "file {}\n"})
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
		!got.Covers("mod/lib/lib.go") || !got.Covers("mod/lib/lib_test.go") {
		t.Errorf("the tie at the base = %+v; want the fixture and compiled source reads, but not fixtures/other.json", got)
	}
}

// go-test-reads.sh runs go list with the packages it is given after go's own flags, so a package
// that starts with "-" would reach go as a flag: go list refuses -exec, a flag of go test alone,
// and takes -toolexec, which names a program that go runs, and then lists the package of its
// directory, as for no package. The script passes "--" before the packages, so go reads each one
// as an import path, and refuses one that starts with "-". Each row asks the script of readsRepo
// for the reads of go test of one planted package in mod/lib, the directory of a package: it must
// answer an error that names go's refusal of the import path. The control is the package of
// mod/lib by its path, whose reads the script answers.
func TestGoTestReadsPassesEachPackageToGoAsAPackage(t *testing.T) {
	t.Parallel()
	reads := specgraph.ScriptTestReads(readsRepo(t), nil)
	for _, planted := range []string{"-toolexec=/bin/false", "-exec=/bin/false"} {
		got, err := reads("mod/lib", []string{planted}, nil)
		if want := `malformed import path "` + planted + `": leading dash`; err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("the reads of go test %s in mod/lib = %q, %v; want no reads and an error that holds %q", planted, got, err, want)
		}
	}
	got, err := reads("mod/lib", []string{"example.com/m/lib"}, nil)
	if tie, tieErr := specgraph.NewTie(got); err != nil || tieErr != nil || !tie.Covers("mod/lib/lib_test.go") {
		t.Errorf("the control: the reads of go test example.com/m/lib in mod/lib = %q, %v; want reads that cover mod/lib/lib_test.go", got, err)
	}
}

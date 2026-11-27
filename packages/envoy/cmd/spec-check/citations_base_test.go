package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cmdtest"
)

// stubScript is a .github/scripts/go-test-reads.sh that answers reads for every package, as the
// repository's own script answers at one commit.
func stubScript(reads string) []byte {
	return []byte("#!/bin/sh\necho '{\"reads\":" + reads + ",\"problems\":[]}'\n")
}

// gitCase is a git repository that holds a copy of testdata/citations/repo, the spec file of the
// check on TestA and a go-test-reads.sh, and the files of a citations run over it but its list.
type gitCase struct{ root, repo, reports, needs, json, tmp string }

func newGitCase(t *testing.T, reads string) gitCase {
	t.Helper()
	root := t.TempDir()
	g := gitCase{root: root, repo: filepath.Join(root, "repo"), reports: filepath.Join(root, "reports"), needs: filepath.Join(root, "needs.json"),
		json: filepath.Join(root, "out", "faults.json"), tmp: filepath.Join(root, "tmp")}
	copyCitationsRepo(t, g.repo, citedSpec(testPart("example.com/citations/a TestA", "go-checks")))
	for _, dir := range []string{filepath.Join(g.repo, ".github", "scripts"), filepath.Join(g.reports, "spec-report--go-checks"), g.tmp} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	g.script(t, reads)
	writeFile(t, filepath.Join(g.reports, "spec-report--go-checks", "report.jsonl"), []byte(goReport("skip")))
	writeFile(t, g.needs, []byte(needsOf("success")))
	return g
}

// script writes the repository's go-test-reads.sh, which answers reads.
func (g gitCase) script(t *testing.T, reads string) {
	t.Helper()
	writeScript(t, filepath.Join(g.repo, ".github", "scripts", "go-test-reads.sh"), stubScript(reads))
}

// cite runs changed from base to HEAD, then citations over its list, as the job runs them, with
// TMPDIR at g.tmp; it fails the test unless changed exits 0 and the checkout of the base is gone
// after the run.
func (g gitCase) cite(t *testing.T, binary, workflow, base string) result {
	t.Helper()
	list := runBinary(t, binary, "", "changed", "--repo", g.repo, "--base", base)
	if list.code != 0 {
		t.Fatalf("changed: exit %d, stderr %q", list.code, list.stderr)
	}
	changed := filepath.Join(g.root, "changed.txt")
	writeFile(t, changed, []byte(list.stdout))
	got := runBinaryIn(t, binary, "", []string{"TMPDIR=" + g.tmp}, "citations", "--dir", filepath.Join(g.repo, ".dispatch", "spec"), "--repo", g.repo,
		"--workflow", workflow, "--changed", changed, "--reports", g.reports, "--needs", g.needs, "--json", g.json)
	if left, err := os.ReadDir(g.tmp); err != nil || len(left) != 0 {
		t.Errorf("the run left %v in its TMPDIR (%v); want the checkout of the base removed", left, err)
	}
	return got
}

// R8: citations derives the tie at the base commit from a checkout of that commit with the base's
// own go-test-reads.sh, so a file that the change deleted counts when the base's tie holds it. The
// head deletes a/testdata and its script answers a/* alone; at the base the script answers a/* and
// a/testdata/**. The control is the same change where the base's script answers a/* alone: the
// deleted file is then no read of the test at either end, and it is cited.
func TestCitationsReadsTheTieAtTheBaseCommit(t *testing.T) {
	t.Parallel()
	binary := cmdtest.Build(t, "spec-check")
	workflow := fixture(t, "citations", "workflow.yaml")
	for _, row := range []struct {
		name, baseReads string
		want            func(dir string) result
	}{
		{"the base's tie holds the deleted testdata", `["a/*","a/testdata/**"]`, func(dir string) result {
			return refused(dir, "test-not-run: a/testdata/answer.json changed (the tie at the base commit holds it), and go-checks did not run example.com/citations/a TestA (its report shows it skipped)")
		}},
		{"control: the base's tie does not hold it", `["a/*"]`, cited},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			g := newGitCase(t, row.baseReads)
			if err := os.MkdirAll(filepath.Join(g.repo, "a", "testdata"), 0o755); err != nil {
				t.Fatal(err)
			}
			writeFile(t, filepath.Join(g.repo, "a", "testdata", "answer.json"), []byte("{\"answer\": 42}\n"))
			base := commitAll(t, g.repo, "", "base")
			removeAll(t, filepath.Join(g.repo, "a", "testdata"))
			g.script(t, `["a/*"]`)
			commitAll(t, g.repo, base, "delete the testdata")
			expect(t, g.cite(t, binary, workflow, base), row.want(filepath.Join(g.repo, ".dispatch", "spec")))
		})
	}
}

// R8: a carried result stands at the commit that the list starts from, so the base of changed is
// the merge base with the default branch, never the branch's push before. Push 1 adds t.Skip to
// TestA, push 2 changes docs alone. From the merge base the list holds a/a_test.go, and the skipped
// test is refused at push 2; from push 1, the control, the list holds the docs alone, and the check
// reads as cited although its test has not run since a/a_test.go changed.
func TestCitationsFromTheMergeBase(t *testing.T) {
	t.Parallel()
	binary := cmdtest.Build(t, "spec-check")
	workflow := fixture(t, "citations", "workflow.yaml")
	g := newGitCase(t, `["a/*"]`)
	if err := os.MkdirAll(filepath.Join(g.repo, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(g.repo, "docs", "readme.md"), []byte("The answer.\n"))
	main := commitAll(t, g.repo, "", "main")
	gitIn(t, g.repo, "update-ref", "refs/heads/main", main)
	test := filepath.Join(g.repo, "a", "a_test.go")
	content, err := os.ReadFile(test)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, test, []byte(strings.Replace(string(content), "func TestA(t *testing.T) {\n", "func TestA(t *testing.T) {\n\tt.Skip(\"flaky\")\n", 1)))
	push1 := commitAll(t, g.repo, main, "skip TestA")
	writeFile(t, filepath.Join(g.repo, "docs", "readme.md"), []byte("The answer, again.\n"))
	push2 := commitAll(t, g.repo, push1, "docs")
	if mergeBase := gitIn(t, g.repo, "merge-base", "main", push2); mergeBase != main {
		t.Fatalf("merge base %s, want main %s", mergeBase, main)
	}
	dir := filepath.Join(g.repo, ".dispatch", "spec")
	expect(t, g.cite(t, binary, workflow, main), refused(dir, "test-not-run: a/a_test.go changed, and go-checks did not run example.com/citations/a TestA (its report shows it skipped)"))
	expect(t, g.cite(t, binary, workflow, push1), cited(dir))
}

// citations derives a tie only for a check whose test did not run: a run where each test ran starts
// no go-test-reads.sh and makes no checkout of the base. The script of the fixture writes a line to
// a file of the test each time it runs, at the head and at the base. The control is the same run
// with the test skipped, where only docs/readme.md changed: the tie at the head covers no changed
// path, so citations asks the script at the head and then at the base. A list from the head itself
// has the head as its base, whose tie is the one at the head: the script runs once, and no checkout
// is made.
func TestCitationsDerivesATieOnlyForATestThatDidNotRun(t *testing.T) {
	t.Parallel()
	binary := cmdtest.Build(t, "spec-check")
	workflow := fixture(t, "citations", "workflow.yaml")
	for _, row := range []struct {
		name, outcome string
		fromHead      bool // the list's base is the head itself
		runs          int
	}{{"pass", "pass", false, 0}, {"skip", "skip", false, 2}, {"skip, from the head itself", "skip", true, 1}} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			g := newGitCase(t, `["a/*"]`)
			runs := filepath.Join(t.TempDir(), "runs")
			writeFile(t, filepath.Join(g.repo, ".github", "scripts", "go-test-reads.sh"),
				[]byte("#!/bin/sh\necho run >> '"+runs+"'\necho '{\"reads\":[\"a/*\"],\"problems\":[]}'\n"))
			writeFile(t, filepath.Join(g.reports, "spec-report--go-checks", "report.jsonl"), []byte(goReport(row.outcome)))
			if err := os.MkdirAll(filepath.Join(g.repo, "docs"), 0o755); err != nil {
				t.Fatal(err)
			}
			writeFile(t, filepath.Join(g.repo, "docs", "readme.md"), []byte("The answer.\n"))
			base := commitAll(t, g.repo, "", "base")
			writeFile(t, filepath.Join(g.repo, "docs", "readme.md"), []byte("The answer, again.\n"))
			if head := commitAll(t, g.repo, base, "docs"); row.fromHead {
				base = head
			}
			dir := filepath.Join(g.repo, ".dispatch", "spec")
			expect(t, g.cite(t, binary, workflow, base), cited(dir))
			content, err := os.ReadFile(runs)
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				t.Fatal(err)
			}
			if got := strings.Count(string(content), "run\n"); got != row.runs {
				t.Errorf("go-test-reads.sh ran %d times, want %d", got, row.runs)
			}
		})
	}
}

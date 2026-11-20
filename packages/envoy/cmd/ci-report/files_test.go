package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cmdtest"
)

// The step reads the check nodes of the files at its commit (R12). A change of the spec files in
// the working tree that is not committed makes no check of the commit lose its result: the step
// reports the check as the commit's files name it, or it refuses the tree with no call.
// testdata/committed-spec holds two checks of job go-checks, and testdata/beta-fails.jsonl is a
// real `go test -json` report of a module example.com/acc in which TestBeta fails.
func TestTheStepReadsTheFilesAtItsCommitAndNotTheWorkingTree(t *testing.T) {
	binary := cmdtest.Build(t, "ci-report")
	dir, commit := checkout(t, filepath.Join("testdata", "committed-spec"))
	file := filepath.Join(dir, ".dispatch", "spec", "probe.md")
	text, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	const committed = `selector="example.com/acc/probe TestBeta" job="go-checks"`
	if !strings.Contains(string(text), committed) {
		t.Fatalf("the fixture holds no %s", committed)
	}
	// A step before this one rewrites the spec file and commits nothing.
	writeFile(t, file, strings.Replace(string(text), committed, `selector="example.com/acc/probe TestBeta" job="lint"`, 1))
	report, err := filepath.Abs(filepath.Join("testdata", "beta-fails.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	s := newStub(t)
	got := runReporter(t, binary, dir, s.env("store"),
		"--dir", ".dispatch/spec", "--job", "go-checks", "--report", report, "--project", "ACME", "--dispatch-cli", s.path)
	calls := s.calls(t)
	reportedAsCommitted := verdictOf(calls, "beta-passes") == "fail"
	refused := got.code != 0 && len(calls) == 0
	if !reportedAsCommitted && !refused {
		t.Errorf("at %s the commit's files give beta-passes to job go-checks and its test failed, and the step neither recorded that failure nor refused the tree: exit %d, %d calls (%v)\nstdout:\n%s\nstderr:\n%s",
			commit, got.code, len(calls), calls, got.stdout, got.stderr)
	}
}

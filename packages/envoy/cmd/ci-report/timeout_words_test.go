package main

import (
	"maps"
	"path/filepath"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/cmdtest"
)

// verdicts is each check the stub's calls name, with the --verdict of each call that names it, in
// call order: a check called twice holds two.
func verdicts(calls []call) map[string][]string {
	byCheck := map[string][]string{}
	for _, c := range calls {
		check, verdict := c.args[slices.Index(c.args, "--check")+1], c.args[slices.Index(c.args, "--verdict")+1]
		byCheck[check] = append(byCheck[check], verdict)
	}
	return byCheck
}

// A test's own pass or fail decides its result, even when its output quotes the words that go test
// writes when it kills a test at its time limit, and the test that go test really killed is
// could_not_run. testdata/timeout-words.jsonl is two real `go test -json -count=1 -trimpath` runs
// of a module example.com/acc, whole, each package's start and end events included, without which
// the reader gives no test an outcome: in ./output/, TestSaysTimedOut passes and
// TestFailsSayingTimedOut fails, each after a t.Log that quotes "panic: test timed out after", and
// in ./slow/, run with -timeout 3s, TestSlow is killed at its time limit.
// testdata/timeout-words/make-report.sh makes it again from the packages of
// testdata/timeout-words/cases, and refuses a report that names a path of the machine that ran it.
// Each check gets one call, and no other check gets one.
func TestATestThatQuotesTheTimeoutWordsKeepsItsOwnOutcome(t *testing.T) {
	binary := cmdtest.Build(t, "ci-report")
	dir, commit := checkout(t, filepath.Join("testdata", "timeout-words-spec"))
	report, err := filepath.Abs(filepath.Join("testdata", "timeout-words.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	s := newStub(t)
	got := runReporter(t, binary, dir, s.env("store"),
		"--dir", ".dispatch/spec", "--job", "go-checks", "--report", report, "--project", "ACME", "--dispatch-cli", s.path)
	if got.code != 0 {
		t.Errorf("exit %d, want 0; stderr:\n%s", got.code, got.stderr)
	}
	want := map[string][]string{"passes-saying-timeout": {"pass"}, "fails-saying-timeout": {"fail"}, "slow-test": {"could_not_run"}}
	if called := verdicts(s.calls(t)); !maps.EqualFunc(called, want, slices.Equal) {
		t.Errorf("at %s the calls give the checks the verdicts %v, want one call each: %v\nstdout:\n%s", commit, called, want, got.stdout)
	}
}

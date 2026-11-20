package main

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/cmdtest"
)

// verdictOf is the --verdict of the stub's call for check, or "" when no call names it.
func verdictOf(calls []call, check string) string {
	for _, c := range calls {
		at := slices.Index(c.args, "--check")
		if at >= 0 && at+1 < len(c.args) && c.args[at+1] == check {
			v := slices.Index(c.args, "--verdict")
			if v >= 0 && v+1 < len(c.args) {
				return c.args[v+1]
			}
		}
	}
	return ""
}

// A test's own pass or fail decides its result, even when its output quotes the words that go test
// writes when it kills a test at its time limit, and the test that go test really killed is
// could_not_run. testdata/timeout-words.jsonl holds the events of real `go test -json` runs of a
// module example.com/acc: TestSaysTimedOut passes and TestFailsSayingTimedOut fails, each after a
// t.Log that quotes "panic: test timed out after", and TestSlow is killed at its -timeout.
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
	calls := s.calls(t)
	verdicts := map[string]string{}
	for _, check := range []string{"passes-saying-timeout", "fails-saying-timeout", "slow-test"} {
		verdicts[check] = verdictOf(calls, check)
	}
	want := map[string]string{"passes-saying-timeout": "pass", "fails-saying-timeout": "fail", "slow-test": "could_not_run"}
	for check, verdict := range want {
		if verdicts[check] != verdict {
			t.Errorf("check %s at %s: verdict %q, want %q (exit %d)\nstdout:\n%s", check, commit, verdicts[check], verdict, got.code, got.stdout)
		}
	}
}

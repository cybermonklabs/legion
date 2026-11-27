package main

import (
	"fmt"
	"os"
	"testing"
)

// skipper is the part of a *testing.T that skipOutsideCI calls.
type skipper interface {
	Helper()
	Fatalf(format string, args ...any)
	Skip(args ...any)
}

// skipOutsideCI skips a row that cannot make its state here, as for root, for a process in one
// group alone, or for a system with no /dev/full or no bash on PATH, with its reason, and fails it
// with the reason in a CI job (GITHUB_ACTIONS is "true"), where each row makes its state: so the
// job passes only when each row ran, and a job whose runner loses a tool or a right fails.
func skipOutsideCI(t skipper, reason string) {
	t.Helper()
	if os.Getenv("GITHUB_ACTIONS") == "true" {
		t.Fatalf("the CI job runs each row, and this one cannot make its state here: %s", reason)
		return
	}
	t.Skip(reason)
}

// skipRecord is a skipper that records the one call that skipOutsideCI makes.
type skipRecord struct{ call string }

func (*skipRecord) Helper() {}
func (r *skipRecord) Fatalf(format string, args ...any) {
	r.call = "fail: " + fmt.Sprintf(format, args...)
}
func (r *skipRecord) Skip(args ...any) { r.call = "skip: " + fmt.Sprint(args...) }

// A row that cannot make its state here skips with its reason, except in a CI job (GITHUB_ACTIONS
// is "true"), where it fails with the reason: the CI job of this package must run each row, and go
// test prints no skip of a package that passes, so only a failure there shows a row that did not
// run.
func TestARowThatCannotMakeItsStateSkipsOutsideCIAndFailsInCI(t *testing.T) {
	for _, row := range []struct{ env, want string }{
		{"true", "fail: the CI job runs each row, and this one cannot make its state here: the reason"},
		{"", "skip: the reason"},
		{"false", "skip: the reason"},
	} {
		t.Run("GITHUB_ACTIONS="+row.env, func(t *testing.T) {
			t.Setenv("GITHUB_ACTIONS", row.env)
			var got skipRecord
			skipOutsideCI(&got, "the reason")
			if got.call != row.want {
				t.Errorf("skipOutsideCI: %q, want %q", got.call, row.want)
			}
		})
	}
}

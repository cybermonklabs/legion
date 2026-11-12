package rescuetest

import (
	"strings"
	"testing"
	"time"
)

// The steps a failed entry names, each spelled as the check spells it (rescue's own step texts).
// A case that reads a failure reads its step off the entry with Step and says which step it
// wants: an entry that only says something failed leaves a reader unable to tell this check's
// own read of a worktree's files from the push it makes. They are here with Step, and for the
// same reason: the cases that read them are in two packages.
const (
	RemoteStep = "the read of what the remote holds"
	FileStep   = "the read of the files a worktree holds that no commit does"
	PushStep   = "the push of the rescue branches to the remote"
)

// Step is the step a failed entry of the push check names, and how long the entry says that step
// ran. Every failure the check reports is written through one step — `<step> failed after
// <duration>: <error>` — so that a reader tells this check's own fault from the fault of the push
// it makes, and tells a step that ran out of time from one that answered at once. The duration is
// read with time.ParseDuration, which is what parses what the check writes: the step's own time,
// rounded to the millisecond.
//
// It is here rather than in a test file of the check's own package because the cases that read it
// are in two: the check's (internal/rescue), which drives it over a disk directly, and the
// daemon's (internal/daemon), which drives the same check through the sweep that replaces a pod
// (checkUnpushed).
func Step(t testing.TB, failure string) (string, time.Duration) {
	t.Helper()
	what, rest, ok := strings.Cut(failure, " failed after ")
	if !ok {
		t.Fatalf("the entry failed with %q, want `<step> failed after <duration>: <error>`: nothing there tells a reader which step failed", failure)
	}
	written, _, ok := strings.Cut(rest, ": ")
	if !ok {
		t.Fatalf("the entry failed with %q, want the step's own duration and then `: ` and the error it failed with", failure)
	}
	took, err := time.ParseDuration(written)
	if err != nil {
		t.Fatalf("the entry says its step ran %q, which is no duration (%v): %q", written, err, failure)
	}
	if took < 0 {
		t.Fatalf("the entry says its step ran %s, want the time that step took: %q", took, failure)
	}
	return what, took
}

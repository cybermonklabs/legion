package workflow

import (
	"slices"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The CI news of a task (record.CINews) keeps three purposes apart: a report of the GitHub
// listener's new runs and statuses at the task's head (reportRuns), a prompt to read Dispatch's
// state again when what Dispatch holds of a check at that head changed (promptReaders), and the
// fix rule's one repair assignment of a check at a head (assignRepair). None of them decides
// another: no report and no prompt assigns work, starts a fix or allows an approval, since
// Dispatch's state of each check decides and each reader reads it itself.

// ciReader is one current reader of a task's CI news: the role, the issue whose role it is (the
// owning architect's for the owner), and the work it reads for (record.CIReader), which a row
// carries so that the outbox finishes it undelivered once that work is no longer current.
type ciReader struct {
	role   claim.Role
	issue  string
	reader record.CIReader
}

// acceptedChange says whether a write of Dispatch's state of a check, after, changed what the
// daemon holds of it at its head: the store took the write (written, a higher Dispatch sequence)
// and its content differs from the state held before (before, held false for none) in its state,
// its reason, its CI case or its set of roles. A first state of the check at the head is a
// change; the same content at a higher sequence is none, and so is a lower sequence the store
// refused.
func acceptedChange(written, held bool, before, after record.CheckState) bool {
	if !written {
		return false
	}
	return !held || before.State != after.State || before.Reason != after.Reason || before.CICase != after.CICase ||
		!sameRoles(before.Roles, after.Roles)
}

// acceptedSetChange says whether a write of the head's set of checks, after, changed the set the
// daemon holds there, before (nil for none held): the store took the write and the list of checks
// differs. A first set at a head is a change, an empty one included.
func acceptedSetChange(written bool, before, after []string) bool {
	return written && (before == nil || !slices.Equal(before, after))
}

// sameRoles says whether a and b name the same roles, in whatever order.
func sameRoles(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	return slices.Equal(slices.Sorted(slices.Values(a)), slices.Sorted(slices.Values(b)))
}

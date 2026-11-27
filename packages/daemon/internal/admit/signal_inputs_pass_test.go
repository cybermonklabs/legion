package admit

import (
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/signals"
)

// Each fact that keeps an input of a signal runs the admission pass where it changed a row of the
// input (the pass rule, Apply: record.RecordsWritten), so the pass of a new kill, a new or renamed
// merge-queue event, a moved ask, a moved assignee or default assignee and a changed read outcome
// reads what it kept without waiting for a tick; and the same input brought again under another
// event id (a report read twice, a boot read that restates what the stream kept) changes no row
// and runs none. Each row applies its first fact, which must run one pass, and then its second,
// which runs one more only where it changes a row too (a pull request's merge-queue event read
// again under its repository's new name). A pass is counted where it commits (UsePassObserver).
func TestASignalInputRunsThePassWhereItChangedARow(t *testing.T) {
	enters := func(id int64, repo string) intake.QueueTimeline {
		return intake.QueueTimeline{Repo: repo, Number: 7, Events: []signals.QueueEvent{
			{ID: id, Repo: repo, PullRequest: 7, At: fixedNow.Add(-30 * time.Minute), Enters: true}}}
	}
	ask := intake.DispatchAskTurn{Key: "ACME-9", Mark: record.EventMark{Seq: 3},
		Turn: signals.AskEvent{Kind: signals.AskOpened, Ask: "ask-9", Issue: "ACME-9"}}
	read := intake.ApprovalsBootRead{Read: record.ApprovalsRead{
		Asks:            []record.ApprovalAsk{{Ask: signals.Ask{ID: "ask-9", Issue: "ACME-9", Open: true, Human: true}, Seq: 3}},
		Assignees:       []record.IssueAssignee{{Issue: "ACME-9", Assignee: "acme-lead", Seq: 3}},
		DefaultAssignee: "acme-owner", DefaultAt: fixedNow.Add(-time.Hour)}}
	for _, tc := range []struct {
		name         string
		first, again intake.Fact
		againRuns    bool
	}{
		{name: "an issue pod's kill, its report read twice", first: guardKill(t, 1), again: guardKill(t, 1)},
		{name: "a merge-queue event, read twice", first: enters(701, "acme/widgets"), again: enters(701, "acme/widgets")},
		{name: "a merge-queue event, read again under its repository's new name", first: enters(702, "acme/old-widgets"),
			again: enters(702, "acme/widgets"), againRuns: true},
		{name: "an ask's event, delivered twice", first: ask, again: ask},
		{name: "the boot's read of Dispatch, read again unchanged", first: read, again: read},
		{name: "a move of the project's default assignee, delivered twice",
			first: intake.DispatchDefaultAssignee{Assignee: "acme-lead", Seq: 5, At: fixedNow.Add(-time.Hour)},
			again: intake.DispatchDefaultAssignee{Assignee: "acme-lead", Seq: 5, At: fixedNow.Add(-time.Hour)}},
		{name: "the boot's failed read of the merge queue, failing again alike",
			first: intake.QueueBootStatus{ReadError: "GitHub answered 502"}, again: intake.QueueBootStatus{ReadError: "GitHub answered 502"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := ruledAdmission(t, 2, record.MainRule, silent())
			passes := 0
			admission.UsePassObserver(func(PassView) { passes++ })
			applyThrough(t, pool, admission, "signals", "first", tc.first)
			if passes != 1 {
				t.Fatalf("the first fact ran %d committed passes, want 1: it kept a row of a signal's input", passes)
			}
			applyThrough(t, pool, admission, "signals", "again", tc.again)
			want := 1
			if tc.againRuns {
				want = 2
			}
			if passes != want {
				t.Errorf("after the second fact %d committed passes ran, want %d (a second fact that changes a row runs one: %t)", passes, want, tc.againRuns)
			}
		})
	}
}

// guardKill is the memory guard's kill of sequence of guard-9 in ACME-9's pod, which the daemon
// records no issue of, reported on its implementer's connection a minute before the pass.
func guardKill(t *testing.T, sequence uint64) intake.MemoryGuardKill {
	t.Helper()
	return intake.MemoryGuardKill{Issue: "ACME-9", Token: acmeToken(t, "ACME-9", claim.RoleImplementer), PodUID: "pod-9",
		Report: shimwire.LauncherMemoryKill{Guard: "guard-9", Sequence: sequence, PID: 4242, Executable: "python3", LimitBytes: 4 << 30,
			At: fixedNow.Add(-time.Minute)}}
}

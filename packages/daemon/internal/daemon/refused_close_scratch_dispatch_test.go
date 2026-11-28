package daemon

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// parentRuleRefusal is the line the outbox logs for each status write Dispatch refuses with
// PARENT_STATUS_CONFLICT (outbox's parentRuleRefused).
const parentRuleRefusal = "outbox status write refused by Dispatch's parent status rule"

// A close Dispatch refuses for a child made after the daemon's decision, while the close waited
// in the outbox, changes nothing and is written again (R24 of dispatch://INFRA-3320), on the rig's
// Dispatch with its parent status rule on, the setting under which Dispatch refuses a close at
// all. The close is close_root's, held at its entry check (overrides.statusRowChecked) until the
// person's child, filed under the root after the call answered, is in the daemon's copy:
//
//   - refused, the root stays in its phase, at its run and phase entry, with its claims, its slot
//     and no production check, Dispatch still shows it in_progress, the outbox holds no close of
//     it, and its architect is told the child Dispatch's refusal body names, with its status;
//   - the person's done on that child, while a second child filed after the refusal stands open,
//     writes nothing, since the pass that reads the done sees the second child open in the copy;
//     the done of the second child is the first pass whose copy shows each child done, which
//     writes the close again, and Dispatch takes it, so the daemon's record and Dispatch both read
//     done;
//   - the person unlinks the child and leaves it open: the pass that reads the unlink writes the
//     close again, and Dispatch takes it with the child open and no longer under the root.
//
// Apart from the refused closes, no status write the daemon queues in the test is refused.
// close_root's close opens no production check when it lands either; the merge's close, which
// opens one, is queued only once no child of the merged issue stands open
// (TestAMergedIssueWaitsInMergingForEachChildNotDone).
func TestARefusedCloseChangesNothingAndIsWrittenAgain(t *testing.T) {
	t.Parallel()
	r := newStopReturnRig(t)

	t.Run("written again and taken at the first pass whose copy shows each child done", func(t *testing.T) {
		root, grant := r.runningRoot("Plover marsh survey")
		first := r.refusedClose(t, root, grant, "Plover nest count")
		second := r.create(asPerson, "Plover egg census", map[string]any{"parent": root.Key})
		r.quiet(second.Key)
		r.setStatus(first.Key, "done")
		r.quiet(first.Key)
		if pending := r.record(root.Key).PendingClose; pending == nil || !pending.Refused || statusRowsOf(t, r.pool, root.Key) != 0 {
			t.Fatalf("after the pass that read %s done, %s carries the close %+v with %d status rows, want the refused close kept and none written: %s stands open",
				first.Key, root.Key, pending, statusRowsOf(t, r.pool, root.Key), second.Key)
		}
		if shown := r.issue(root.Key).Status; shown != "in_progress" {
			t.Fatalf("Dispatch shows %s %s while %s stands open, want in_progress", root.Key, shown, second.Key)
		}
		r.setStatus(second.Key, "done")
		r.quiet(second.Key)
		if got := r.record(root.Key); got.Phase != phase.Done && (got.PendingClose == nil || got.PendingClose.Refused) {
			t.Fatalf("after the pass that read %s done, the record of %s = %+v, want its close written again", second.Key, root.Key, got)
		}
		r.closeTaken(t, root)
	})

	t.Run("written again and taken once a person unlinks the child, leaving it open", func(t *testing.T) {
		root, grant := r.runningRoot("Avocet tide log")
		child := r.refusedClose(t, root, grant, "Avocet feeding grounds")
		r.patch(child.Key, asPerson, map[string]any{"parent": nil})
		r.quiet(child.Key)
		r.closeTaken(t, root)
		shown, err := dispatch.New(r.baseURL, dispatch.BearerTransport(r.dispatch.token, nil)).GetIssue(t.Context(), child.Key)
		if err != nil || shown.Status != "triage" || shown.Parent != nil {
			t.Fatalf("Dispatch shows %s at %q under %v (%v), want it open in triage under no parent", child.Key, shown.Status, shown.Parent, err)
		}
	})

	logs := r.logs.String()
	for _, text := range []string{"outbox status write refused by Dispatch and dropped", "outbox status write refused by Dispatch; its architect is told"} {
		if lines := loggedLines(t, logs, text); len(lines) != 0 {
			t.Fatalf("the daemon logged %q: %v, want no status write of its refused but the closes", text, lines)
		}
	}
	if refused := loggedLines(t, logs, parentRuleRefusal); len(refused) != 2 {
		t.Fatalf("the daemon logged %d refusals by the parent status rule, want the two closes alone: %v", len(refused), refused)
	}
}

// refusedClose is close_root's close of root, held at its entry check while the person files a
// child titled title under root, released once the daemon's copy holds the child, so the pass
// that applies the refusal reads it, and refused by Dispatch for that child. It reads what the
// refusal leaves and answers the child.
func (r *stopReturnRig) refusedClose(t *testing.T, root scratchIssue, grant func() string, title string) scratchIssue {
	t.Helper()
	before, fence := r.record(root.Key), r.fence(root.Key)
	r.checked.arm(closeRowOf(root.Key))
	r.closeRoot(root.Key, grant, "the human decided no change is needed")
	r.checked.reached(t, "the close's check")
	child := r.create(asPerson, title, map[string]any{"parent": root.Key})
	r.mirrored(child.Key, child.LastSeq)
	r.checked.release()
	testwait.Eventually(t, "the daemon to apply Dispatch's refusal of the close of "+root.Key, func() bool {
		pending := r.record(root.Key).PendingClose
		return pending != nil && pending.Refused
	})
	r.statusRowGone(root.Key)

	if got := r.record(root.Key); got.Phase != before.Phase || got.Generation != before.Generation || !record.InLegionsWork(got) {
		t.Fatalf("the record of %s = %+v after the refused close, want it in %s at generation %d in Legion's work", root.Key, got, before.Phase, before.Generation)
	}
	if now := r.fence(root.Key); now != fence {
		t.Fatalf("%s stands at %+v after the refused close, want %+v", root.Key, now, fence)
	}
	if shown := r.issue(root.Key).Status; shown != "in_progress" {
		t.Fatalf("Dispatch shows %s %s after the refused close, want in_progress", root.Key, shown)
	}
	if stopped := r.stoppedClaims(root.Key); len(stopped) != 0 {
		t.Fatalf("the refused close of %s stopped claims %v, want none", root.Key, stopped)
	}
	if state, body := settled(t, r.d, "answer the state after the refused close of "+root.Key, func(api.State) bool { return true }); !slices.Contains(state.Admission.Active, root.Key) {
		t.Fatalf("%s holds no slot after the refused close, want its slot kept\nstate %s", root.Key, body)
	}
	var check *record.ProductionCheck
	if err := store.Transact(t.Context(), r.pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		check, err = record.NewStore().ProductionCheck(t.Context(), tx, root.Key)
		return err
	}); err != nil || check != nil {
		t.Fatalf("the production check of %s after the refused close = %+v (%v), want none", root.Key, check, err)
	}
	told := r.notices(root.Key, "message-refused")
	if len(told) != 1 || told[0].Role != claim.RoleArchitect || !strings.Contains(told[0].Reason, child.Key+" (triage)") {
		t.Fatalf("the notices of the refused close of %s = %+v, want one to its architect naming %s (triage)", root.Key, told, child.Key)
	}
	refused := slices.DeleteFunc(loggedLines(t, r.logs.String(), parentRuleRefusal), func(line map[string]any) bool {
		return line["issue"] != root.Key || line["kind"] != string(record.CloseStatusRow)
	})
	if len(refused) != 1 {
		t.Fatalf("the daemon logged %d refusals of the close of %s by the parent status rule, want one: %v", len(refused), root.Key, refused)
	}
	t.Logf("Dispatch refused the close of %s for %s, and its architect was told: %s", root.Key, child.Key, told[0].Reason)
	return child
}

// closeTaken waits until Dispatch shows root done by the daemon's close written again, and reads
// the run's end: the close queued twice (close_root's and the pass's), the record at phase done,
// the architect's claim retired and the slot given back.
func (r *stopReturnRig) closeTaken(t *testing.T, root scratchIssue) {
	t.Helper()
	awaitDispatchStatus(t, r.baseURL, root.Key, "done")
	r.statusRowGone(root.Key)
	if closed := r.newestEvent(root.Key, "issue.closed", nil); closed.Actor != (scratchActor{Kind: "session", ID: dispatch.DaemonSession(root.Key)}) {
		t.Fatalf("the close of %s was written by %+v, want the daemon", root.Key, closed.Actor)
	}
	testwait.Eventually(t, "the daemon to end the run of "+root.Key, func() bool {
		got := r.record(root.Key)
		return got.Phase == phase.Done && got.Status == "done" && got.PendingClose == nil
	})
	testwait.Eventually(t, "the daemon to retire the architect of "+root.Key, func() bool {
		return slices.Contains(r.stoppedClaims(root.Key), string(claim.RoleArchitect)+" retired")
	})
	settled(t, r.d, "give back the slot of "+root.Key, func(s api.State) bool { return !slices.Contains(s.Admission.Active, root.Key) })
	queued := slices.DeleteFunc(loggedLines(t, r.logs.String(), "workflow: the daemon's close of an issue is queued"), func(line map[string]any) bool {
		return line["issue"] != root.Key
	})
	if len(queued) != 2 {
		t.Fatalf("the daemon queued the close of %s %d times, want twice: close_root's and the pass's after the refusal: %v", root.Key, len(queued), queued)
	}
}

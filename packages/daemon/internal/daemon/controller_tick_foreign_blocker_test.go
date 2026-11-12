package daemon

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// foreignProject is the Dispatch project of the blocker these tests wait on, which is not the
// daemon's own ("CAPTURE"): a key names its project (`<PROJECT>-<number>`), as the daemon reads
// it everywhere (workflowRuntime.ownDispatchKey).
const (
	foreignProject = "OTHER"
	foreignBlocker = "OTHER-7"
	foreignOther   = "OTHER-9"
)

// The turn is the one way the daemon learns what Dispatch says of a blocker outside its own
// project: no event of another project reaches it (intake.decodeDispatchFact drops it undecoded)
// and the listing it reads is of its own project alone. So the turn reads such a key one at a
// time, in both of its branches, and the mirror row it writes carries the project the key names
// rather than the daemon's, which is what keeps the row readable by the daemon of that other
// project on a shared database.
func TestTheTurnReadsABlockerOutsideTheProject(t *testing.T) {
	t.Run("above the fan-out, beside the one project listing", func(t *testing.T) {
		pool := isolatedOutboxPool(t)
		client := &tickDispatch{issues: map[string]dispatch.Issue{
			foreignBlocker: {Key: foreignBlocker, Project: foreignProject, Status: "in_progress", LastSeq: 5},
		}}
		w := tickRuntime(t, pool, client)
		// The project's own waiting set, far enough above the bound that the turn lists the
		// project instead of reading each of those keys, and the listing says they are all open
		// so that nothing of this project competes for the one slot.
		seedBlockedRoots(t, pool, blockerReadFanout)
		for i := 1; i <= blockerReadFanout; i++ {
			client.summaries = append(client.summaries,
				dispatch.IssueSummary{Key: ownBlocker(i), Status: "in_progress", LastSeq: 1})
		}
		seedStartedWaiter(t, pool, "CAPTURE-50", foreignBlocker)
		// A row of the other project's own daemon, written before the turn: nothing this turn
		// does may touch it.
		seedForeignMirror(t, pool, foreignOther, "todo", 3)

		w.applyControllerTick(context.Background(), time.Now())

		if asked, want := client.asked(), []string{foreignBlocker}; !slices.Equal(asked, want) {
			t.Fatalf("the turn read %v one at a time, want exactly %v: the listing names no issue of another project", asked, want)
		}
		if listings := client.listings(); listings != 1 {
			t.Fatalf("the turn listed the project %d times, want one: its own waiting set is above the bound", listings)
		}
		if slotted := tickSlotted(t, pool); len(slotted) != 0 {
			t.Fatalf("slots = %v, want none: the started issue waits on an open issue of another project", slotted)
		}
		if row := foreignMirrored(t, pool)[foreignBlocker]; row.Project != foreignProject || row.Status != "in_progress" || row.Seq != 5 {
			t.Fatalf("the mirror holds %s as project %q at %q seq %d, want the turn's own reading (%s, in_progress, 5)",
				foreignBlocker, row.Project, row.Status, row.Seq, foreignProject)
		}
		if row := foreignMirrored(t, pool)[foreignOther]; row.Status != "todo" || row.Seq != 3 {
			t.Fatalf("the row of %s stands at %q seq %d, want the other project's own (todo, 3) untouched", foreignOther, row.Status, row.Seq)
		}

		// The foreign issue closes. Its close reaches this daemon on the next turn and nowhere
		// else, and the issue that waited takes a slot in that same pass.
		client.serve(dispatch.Issue{Key: foreignBlocker, Project: foreignProject, Status: "done", LastSeq: 6})

		w.applyControllerTick(context.Background(), time.Now().Add(time.Second))

		if slotted := tickSlotted(t, pool); !slices.Equal(slotted, []string{"CAPTURE-50"}) {
			t.Fatalf("slots = %v, want the waiting issue admitted once the turn saw the foreign blocker is done", slotted)
		}
		if row := foreignMirrored(t, pool)[foreignBlocker]; row.Project != foreignProject || row.Status != "done" || row.Seq != 6 {
			t.Fatalf("the mirror holds %s as project %q at %q seq %d, want (%s, done, 6)",
				foreignBlocker, row.Project, row.Status, row.Seq, foreignProject)
		}
		if row := foreignMirrored(t, pool)[foreignOther]; row.Status != "todo" || row.Seq != 3 {
			t.Fatalf("the row of %s stands at %q seq %d, want the other project's own (todo, 3) untouched", foreignOther, row.Status, row.Seq)
		}
	})

	t.Run("below the fan-out, read singly as every other key is", func(t *testing.T) {
		pool := isolatedOutboxPool(t)
		client := &tickDispatch{issues: map[string]dispatch.Issue{
			foreignBlocker: {Key: foreignBlocker, Project: foreignProject, Status: "in_progress", LastSeq: 5},
		}}
		w := tickRuntime(t, pool, client)
		seedStartedWaiter(t, pool, "CAPTURE-50", foreignBlocker)

		w.applyControllerTick(context.Background(), time.Now())

		if asked, want := client.asked(), []string{foreignBlocker}; !slices.Equal(asked, want) {
			t.Fatalf("the turn read %v one at a time, want exactly %v", asked, want)
		}
		if listings := client.listings(); listings != 0 {
			t.Fatalf("the turn listed the project %d times, want none: one waited-on key is far under the bound", listings)
		}
		if slotted := tickSlotted(t, pool); len(slotted) != 0 {
			t.Fatalf("slots = %v, want none while the foreign blocker is open", slotted)
		}

		client.serve(dispatch.Issue{Key: foreignBlocker, Project: foreignProject, Status: "done", LastSeq: 6})

		w.applyControllerTick(context.Background(), time.Now().Add(time.Second))

		if slotted := tickSlotted(t, pool); !slices.Equal(slotted, []string{"CAPTURE-50"}) {
			t.Fatalf("slots = %v, want the waiting issue admitted once the turn saw the foreign blocker is done", slotted)
		}
	})

	t.Run("a failed read of the foreign key ends no wait", func(t *testing.T) {
		pool := isolatedOutboxPool(t)
		client := &tickDispatch{err: errors.New("dispatch is unreachable")}
		w := tickRuntime(t, pool, client)
		seedStartedWaiter(t, pool, "CAPTURE-50", foreignBlocker)
		seedForeignMirror(t, pool, foreignBlocker, "in_progress", 5)

		w.applyControllerTick(context.Background(), time.Now())

		if row := foreignMirrored(t, pool)[foreignBlocker]; row.Project != foreignProject || row.Status != "in_progress" || row.Seq != 5 {
			t.Fatalf("the mirror holds %s as project %q at %q seq %d after a failed read, want the row as it stood (%s, in_progress, 5)",
				foreignBlocker, row.Project, row.Status, row.Seq, foreignProject)
		}
		if slotted := tickSlotted(t, pool); len(slotted) != 0 {
			t.Fatalf("slots = %v, want none: a failed read tells the daemon nothing new about the wait", slotted)
		}
	})
}

// seedStartedWaiter records key as an issue the daemon has started that waits on blocker: it took
// a slot, gave it back for the wait, and so stands in the project's waiting line
// (record.Issue.ReleasedForWaitAt), which is where the turn finds the key it waits on
// (workflowRuntime.waitedOn).
func seedStartedWaiter(t *testing.T, pool *pgxpool.Pool, key, blocker string) {
	t.Helper()
	seedTickIssue(t, pool, record.Issue{
		Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "CAPTURE",
		Title: key, Tree: key, Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "A",
		HandedOver: true, LastDispatchSeq: 1,
	})
	seedTickMirror(t, pool, key, "in_progress", 1, blocker)
	ctx := context.Background()
	token, err := claim.ProjectToken("CAPTURE")
	if err != nil {
		t.Fatalf("project token: %v", err)
	}
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		records := record.NewStore()
		at := time.Now().UTC()
		// The tree runs: the slot rule counts an issue of a live tree alone (record.NeedsSlot).
		if _, err := records.OpenTreeLifecycle(ctx, tx, token, key, treelifecycle.AuthorityWorkflow); err != nil {
			return err
		}
		if err := records.PutSlot(ctx, tx, record.Slot{Issue: key, Index: 0, AdmittedAt: at}); err != nil {
			return err
		}
		return records.ReleaseSlotForWait(ctx, tx, key, at)
	}); err != nil {
		t.Fatalf("seed the released slot of %s: %v", key, err)
	}
}

// seedForeignMirror writes a mirror row of the other project, as that project's own daemon
// writes one: the table is keyed by the issue alone, so a row of either project stands in the
// same table.
func seedForeignMirror(t *testing.T, pool *pgxpool.Pool, key, status string, seq int64) {
	t.Helper()
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		return record.NewStore().PutDispatchIssue(context.Background(), tx, record.DispatchIssueRow{
			Key: key, Project: foreignProject, Status: status, Seq: seq})
	}); err != nil {
		t.Fatalf("seed the mirror row of %s: %v", key, err)
	}
}

// foreignMirrored is the mirror rows of the other project, by key: tickMirrored reads this
// daemon's own project, and a row the turn wrote for a foreign key is in neither that listing nor
// that project.
func foreignMirrored(t *testing.T, pool *pgxpool.Pool) map[string]record.DispatchIssueRow {
	t.Helper()
	var mirrored []record.DispatchIssueRow
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		var err error
		mirrored, err = record.NewStore().DispatchIssues(context.Background(), tx, foreignProject)
		return err
	}); err != nil {
		t.Fatalf("read the mirror of %s: %v", foreignProject, err)
	}
	rows := make(map[string]record.DispatchIssueRow, len(mirrored))
	for _, row := range mirrored {
		rows[row.Key] = row
	}
	return rows
}

// ownBlocker is the key seedBlockedRoots gives the i-th root to wait on.
func ownBlocker(i int) string { return fmt.Sprintf("CAPTURE-B%d", i) }

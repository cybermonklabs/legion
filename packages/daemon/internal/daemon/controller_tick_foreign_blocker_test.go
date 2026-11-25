package daemon

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
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
	t.Parallel()
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
		// The one listing is the read of CAPTURE-50's children, which the turn makes for every
		// issue whose phases have started (readObserved), and which takes only the summaries of
		// this project: the waited-on key is another project's, so it is read singly all the same.
		if listings := client.listings(); listings != 1 {
			t.Fatalf("the turn listed the project %d times, want one, for the children of CAPTURE-50, an issue in implementing", listings)
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
		w, logs := rereadRuntime(t, pool, client)
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
		// The read's one line says why it failed and names the row the copy keeps.
		want := fmt.Sprintf(`blocker=%s waiting=[CAPTURE-50] error="dispatch is unreachable" copy="read failed" copy_status=in_progress copy_seq=5`, foreignBlocker)
		if lines := foreignReadLines(logs); !slices.Equal(lines, []string{want}) {
			t.Fatalf("the turn logged %q of its failed read of %s, want exactly %q", lines, foreignBlocker, want)
		}
	})
}

// A root that waits on an issue of another project that is already done is held for at most one
// turn (item 51 of dispatch://INFRA-3320's open items: OSSV-551 stood more than six hours on
// INFRA-4737, done for days). Both kinds of waiting root are read: one waiting for its first start
// (seedWaitingRoot) and one the daemon started that gave its slot back for the wait
// (seedStartedWaiter), as OSSV-551 had. The copy holds no row of the blocker, or a row of it from a
// reading before its close. The waiter's own row stands at a sequence of its own log far above
// the blocker's, as an issue at work for weeks does (1511 against 70): each value of a row is
// fenced by the sequences of that one issue's log alone (record.Store.PutDispatchIssue), so the
// waiter's sequence decides nothing of the blocker's row. The turn reads the blocker, writes its
// row at its own sequence and project, and the root takes the slot in that same turn. The control
// serves the blocker open, and the root stays held.
//
// A third copy holds the blocker open at the very sequence Dispatch serves, the shape a sequence
// of another log would leave: the fence keeps the copy's value at an equal sequence, so the root
// stays held whatever Dispatch serves. Every turn leaves one line of its read of the blocker
// (workflowRuntime.logForeignRead): the issue waiting on it, what Dispatch served, and whether the
// copy applied the reading or kept the row it held, which that line then names.
func TestARootWaitingOnADoneIssueOfAnotherProjectIsHeldForOneTurnAtMost(t *testing.T) {
	t.Parallel()
	const waiter = "CAPTURE-60"
	for _, kind := range []struct {
		name string
		seed func(t *testing.T, pool *store.Pool)
	}{
		{"a root waiting for its first start", func(t *testing.T, pool *store.Pool) {
			seedWaitingRoot(t, pool, waiter, "A")
			seedTickMirror(t, pool, waiter, "todo", 1511, foreignBlocker)
		}},
		{"a started root that gave its slot back for the wait", func(t *testing.T, pool *store.Pool) {
			seedStartedWaiter(t, pool, waiter, foreignBlocker)
			seedTickMirror(t, pool, waiter, "in_progress", 1511, foreignBlocker)
		}},
	} {
		for _, copied := range []struct {
			name string
			// status and seq are the copy's row of the blocker before the turn; "" is no row.
			status string
			seq    int64
		}{
			{"with no row of the blocker in the copy", "", 0},
			{"with a row of the blocker from before its close", "in_progress", 65},
			{"with a row of the blocker at the sequence Dispatch serves", "in_progress", 70},
		} {
			for _, served := range []string{"done", "in_progress"} {
				t.Run(kind.name+", "+copied.name+", served "+served, func(t *testing.T) {
					t.Parallel()
					pool := isolatedOutboxPool(t)
					client := &tickDispatch{issues: map[string]dispatch.Issue{
						foreignBlocker: {Key: foreignBlocker, Project: foreignProject, Status: served, LastSeq: 70},
					}}
					w, logs := rereadRuntime(t, pool, client)
					kind.seed(t, pool)
					if copied.status != "" {
						seedForeignMirror(t, pool, foreignBlocker, copied.status, copied.seq)
					}
					if slotted := tickSlotted(t, pool); len(slotted) != 0 {
						t.Fatalf("slots = %v before the turn, want %s waiting", slotted, waiter)
					}

					w.applyControllerTick(context.Background(), time.Now())

					// The copy takes the reading where it held no row of the blocker or one at a lower
					// sequence, and keeps the value it held at an equal one (record.Store.PutDispatchIssue).
					holds := served
					line := fmt.Sprintf("blocker=%s waiting=[%s] served_status=%s served_seq=70 copy=applied", foreignBlocker, waiter, served)
					if copied.status != "" && copied.seq >= 70 {
						holds = copied.status
						line = fmt.Sprintf("blocker=%s waiting=[%s] served_status=%s served_seq=70 copy=kept copy_status=%s copy_seq=%d",
							foreignBlocker, waiter, served, copied.status, copied.seq)
					}
					if asked := client.asked(); !slices.Contains(asked, foreignBlocker) {
						t.Fatalf("the turn read %v singly, want %s among them", asked, foreignBlocker)
					}
					if row := foreignMirrored(t, pool)[foreignBlocker]; row.Project != foreignProject || row.Status != holds || row.Seq != 70 {
						t.Fatalf("the copy holds %s as project %q at %q seq %d, want (%s, %s, 70)",
							foreignBlocker, row.Project, row.Status, row.Seq, foreignProject, holds)
					}
					slotted := tickSlotted(t, pool)
					if holds == "done" && !slices.Equal(slotted, []string{waiter}) {
						t.Fatalf("slots = %v after one turn, want %s admitted: the copy holds its one blocker done", slotted, waiter)
					}
					if holds != "done" && len(slotted) != 0 {
						t.Fatalf("slots = %v after one turn, want %s still held: the copy holds its one blocker open", slotted, waiter)
					}
					if lines := foreignReadLines(logs); !slices.Equal(lines, []string{line}) {
						t.Fatalf("the turn logged %q of its read of %s, want exactly %q", lines, foreignBlocker, line)
					}
				})
			}
		}
	}
}

// seedStartedWaiter records key as an issue the daemon has started that waits on blocker: it took
// a slot, gave it back for the wait, and so stands in the project's waiting line
// (record.Issue.ReleasedForWaitAt), which is where the turn finds the key it waits on
// (workflowRuntime.waitedOn).
func seedStartedWaiter(t *testing.T, pool *store.Pool, key, blocker string) {
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
	if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
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
func seedForeignMirror(t *testing.T, pool *store.Pool, key, status string, seq int64) {
	t.Helper()
	if err := store.Transact(context.Background(), pool, func(ctx context.Context, tx pgx.Tx) error {
		return record.NewStore().PutDispatchIssue(context.Background(), tx, record.DispatchReading{Key: key, Project: foreignProject, Seq: seq,
			Status: record.Carry(status), Parent: record.Carry(""), Blockers: record.Carry[[]string](nil)})
	}); err != nil {
		t.Fatalf("seed the mirror row of %s: %v", key, err)
	}
}

// foreignMirrored is the mirror rows of the other project, by key: tickMirrored reads this
// daemon's own project, and a row the turn wrote for a foreign key is in neither that listing nor
// that project.
func foreignMirrored(t *testing.T, pool *store.Pool) map[string]record.DispatchIssueRow {
	t.Helper()
	var mirrored []record.DispatchIssueRow
	if err := store.Transact(context.Background(), pool, func(ctx context.Context, tx pgx.Tx) error {
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

// foreignReadLine is the message of the one line a turn writes of each read of a key of another
// project (workflowRuntime.logForeignRead).
const foreignReadLine = "the controller tick re-read a blocker of another project"

// foreignReadLines is each line of logs, rereadRuntime's text log, that is foreignReadLine at
// info, from its first value on: what comes before the level is the clock's.
func foreignReadLines(logs *syncBuffer) []string {
	var lines []string
	for line := range strings.Lines(logs.String()) {
		if _, values, found := strings.Cut(strings.TrimSuffix(line, "\n"), ` level=INFO msg="`+foreignReadLine+`" `); found {
			lines = append(lines, values)
		}
	}
	return lines
}

// ownBlocker is the key seedBlockedRoots gives the i-th root to wait on.
func ownBlocker(i int) string { return fmt.Sprintf("CAPTURE-B%d", i) }

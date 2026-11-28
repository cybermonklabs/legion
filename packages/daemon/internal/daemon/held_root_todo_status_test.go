package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/blockers"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// A root whose plan has no acceptance step is held before its retro for the issue beneath it, and
// the board shows the status of its last step. Something moves the board, then the last issue
// beneath the root leaves, and the retro that ends the hold lands on the board: Dispatch reads
// retro. The board's move is a person's todo on the held root, which leaves the hold as it stands
// and sets the board back with the hold's why posted beside it, written for that person, so the
// person reads why and is given none of the architect's rules for ending a child; or an outside
// session's status on the root, in the workflow or out of it, which the daemon re-asserts its own
// status over, and which the status repair never puts back where it is out of the workflow; the
// control has no move, so the same check is seen to pass where the board was never moved, and
// nothing posts the sentence. The root's status rows run through the outbox's own status executor
// against the board, oldest first, as the outbox runs an issue's status writes one at a time in
// the order they were made, and the move's set-back runs on either side of the last issue's leave.
// Run before it, the set-back lands while the hold stands, and the board answers it with no
// sequence for the record to take (applyOwnWrite), so the board shows the set-back while the
// record still reads the move as what Dispatch shows. Still queued when the release starts the
// retro at a new phase entry, the set-back finishes there unwritten and posts nothing
// (record.LateAt), so the board still shows the move when the retro's write runs. Either way the
// retro lands in the outbox's own run.
func TestTheRetroThatEndsAChildrenHoldLandsOnTheBoard(t *testing.T) {
	t.Parallel()
	const rootKey, childKey = "LEGION-208", "LEGION-209"
	// heldSentence is the why of the root's hold that a person reads beside the board's move,
	// naming the issue beneath it.
	const heldSentence = "LEGION-208 waits before its retro while these issues beneath it are still in the workflow: LEGION-209. " +
		"Legion starts the retro by itself once nothing holds it any longer: no issue beneath it is still in the workflow, " +
		"no root that rests on its approval is still in the workflow, and no Dispatch child of it is open."
	for _, tc := range []struct {
		name string
		// moved is the status the board is moved to before the last issue leaves, "" for none, and
		// session the session that moves it, "" for a person. setBackFirst runs the status rows of
		// the move before the last issue leaves; otherwise they are still queued when it leaves.
		moved, session string
		setBackFirst   bool
	}{
		{name: "a person's todo on the held root, set back before the last issue beneath it leaves", moved: "todo", setBackFirst: true},
		{name: "a person's todo on the held root, still to be set back when the last issue beneath it leaves", moved: "todo"},
		{name: "an outside session's status on the held root, set back before the last issue beneath it leaves", moved: "in_progress", session: "ses-outside", setBackFirst: true},
		{name: "an outside session's status on the held root, still to be set back when the last issue beneath it leaves", moved: "in_progress", session: "ses-outside"},
		{name: "an outside session's backlog on the held root, still to be set back when the last issue beneath it leaves", moved: "backlog", session: "ses-outside"},
		{name: "control: the last issue beneath it leaves with the board never moved"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
			engine := workflow.New(records, workflow.Config{Project: "LEGION", Repositories: testRepositories,
				Clock: func() time.Time { return now }}, quietLogger())
			admission := admit.New(records, engine, 1, "LEGION", quietLogger())
			parent := rootKey
			root := record.Issue{Key: rootKey, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: rootKey, Project: "LEGION",
				Title: "root", Phase: phase.Held, Hold: &record.Hold{From: phase.Retro, Reason: record.HoldForChildren}, Generation: 1,
				Status: "needs_review", DispatchStatus: "needs_review", Rank: "A", LastDispatchSeq: 1, HandedOver: true}
			child := record.Issue{Key: childKey, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: rootKey, Project: "LEGION",
				Title: "child", Parent: &parent, Phase: phase.Reviewing, Generation: 1, Status: "needs_review", DispatchStatus: "needs_review",
				Rank: "B", LastDispatchSeq: 1, HandedOver: true}
			// The copy of Dispatch's issues holds both, the child under the root, so the root's
			// chain is known: the release of the hold reads what stands beneath the root and its
			// gate root there (workflow's startHeldRetro). The root carries the legion label there,
			// so an outside session's status on it is the daemon's to set back (R26 of
			// dispatch://INFRA-3320, workflow's sessionStatusWrite).
			if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
				version := record.PlanVersion{Issue: rootKey, Generation: 1, Version: 1, ArtifactID: "plan-artifact", Source: record.PlanSourceOwner,
					GivenBy: mustClaimToken(t, rootKey, claim.RoleArchitect), GivenAt: now.Add(-time.Hour)}
				task := record.PlanTask{Issue: rootKey, Generation: 1, Task: "a", PlanVersion: 1, BlockText: "Build the widget.",
					State: record.TaskRunning, Place: record.TaskPlace{Kind: record.PlaceRetro}}
				steps := []record.PlanStep{
					{Issue: rootKey, Generation: 1, Task: "a", Position: 1, Role: "implementer", Type: "implementation"},
					{Issue: rootKey, Generation: 1, Task: "a", Position: 2, Role: "reviewer", Type: "review"},
				}
				for _, put := range []error{
					records.PutIssue(ctx, tx, root),
					records.PutIssue(ctx, tx, child),
					records.PutPlan(ctx, tx, version, []record.PlanTask{task}, steps),
					records.PutDispatchIssue(ctx, tx, record.DispatchReading{Key: rootKey, Project: "LEGION", Seq: 1,
						Status: record.Carry(root.DispatchStatus), Parent: record.Carry(""), Labels: record.Carry([]string{"legion"})}),
				} {
					if put != nil {
						return put
					}
				}
				seedCopyRow(t, tx, childKey, rootKey, child.DispatchStatus, 1)
				return openIssueTx(ctx, tx, "legion", rootKey, treelifecycle.AuthorityWorkflow)
			}); err != nil {
				t.Fatalf("seed the held root, the child beneath it, their copy rows and the root's plan: %v", err)
			}
			board := &boardDispatch{status: "needs_review"}
			// Every fact goes through the daemon's own handlers in their order (workflow.go): the
			// mirror of Dispatch first, so the engine reads the copy row the fact writes. The hold
			// over the root's open Dispatch children (workflow's acceptanceHeld) reads the child's
			// copy row, and only the child's done written there ends it.
			handlers := []intake.Handler{blockers.NewMirror(records, "LEGION"), engine, admission}
			runner := &outbox{pool: pool, records: records, dispatch: board, dispatchProject: "LEGION",
				handlers: handlers, log: quietLogger()}
			apply := func(id string, fact intake.DispatchIssue) {
				t.Helper()
				if result, err := intake.ApplyFact(ctx, pool, "dispatch", id, fact, handlers...); err != nil || result.Refusal != nil {
					t.Fatalf("apply %s = %+v, %v; want it taken", id, result.Refusal, err)
				}
			}
			// writeRootStatuses runs each status row of the root the outbox holds, oldest first, and
			// finishes it, as the outbox's status lane does.
			writeRootStatuses := func() {
				t.Helper()
				var rows []record.OutboxRow
				if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
					var err error
					rows, err = records.PendingStatusWrites(ctx, tx, "LEGION")
					return err
				}); err != nil {
					t.Fatalf("read the status rows: %v", err)
				}
				for _, row := range rows {
					if row.Issue != rootKey {
						continue
					}
					if err := runner.execute(ctx, row); err != nil {
						t.Fatalf("run the status row %d of %s: %v", row.ID, rootKey, err)
					}
					if _, err := pool.Exec(ctx, "delete from outbox where id = $1", row.ID); err != nil {
						t.Fatalf("finish the status row %d: %v", row.ID, err)
					}
				}
			}
			stored := func(key string) record.Issue {
				t.Helper()
				return *storedIssueOf(t, ctx, pool, records, key)
			}

			if tc.moved != "" {
				board.status = tc.moved
				apply("root-moved", intake.DispatchIssue{Key: rootKey, ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated",
					Status: tc.moved, Title: "root", Rank: "A", Labels: []string{"legion"}, HandedOver: true, ActorSession: tc.session})
				if got := stored(rootKey); !record.HeldForChildren(got) {
					t.Fatalf("%s after the board moved to %s stands in %s with hold %+v, want it still held for its children", rootKey, tc.moved, got.Phase, got.Hold)
				}
				if tc.setBackFirst {
					writeRootStatuses()
					if got := stored(rootKey); board.status != "needs_review" || got.DispatchStatus != tc.moved {
						t.Fatalf("after the set-back ran, Dispatch reads %s as %q and its record reads Dispatch at %q, want needs_review on the board and %s on the record",
							rootKey, board.status, got.DispatchStatus, tc.moved)
					}
				}
			}
			apply("child-done", intake.DispatchIssue{Key: childKey, ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.closed",
				Status: "done", Title: "child", Parent: rootKey, Rank: "B", HandedOver: true})
			if got := stored(rootKey); got.Phase != phase.Retro || got.Hold != nil {
				t.Fatalf("%s after the last issue beneath it left stands in %s with hold %+v, want its retro started", rootKey, got.Phase, got.Hold)
			}
			writeRootStatuses()
			if board.status != "retro" {
				t.Fatalf("Dispatch reads %s as %q once its retro started, want retro", rootKey, board.status)
			}
			told := 0
			for _, post := range board.posted {
				if strings.HasPrefix(post, heldSentence) {
					told++
				}
				if strings.Contains(post, record.EndChildRule) || strings.Contains(post, record.StatusNotTakenYet) {
					t.Errorf("Dispatch was posted %q, which carries the architect's rules for ending a child: a person reads it", post)
				}
			}
			if want := map[bool]int{true: 1, false: 0}[tc.moved == "todo" && tc.setBackFirst]; told != want {
				t.Fatalf("Dispatch was posted the hold's retro-held sentence %d times (posts %q), want %d: a person's todo set back while the hold stands is set back with it, a set-back the retro's entry refuses posts nothing, and nothing else posts it",
					told, board.posted, want)
			}
		})
	}
}

package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// testerRow is the tester's phases row of issue as it stands now.
func testerRow(t *testing.T, h *harness, issue string) record.PhaseRow {
	t.Helper()
	var rows []record.PhaseRow
	if err := store.Transact(context.Background(), h.store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		rows, err = record.NewStore().Phases(context.Background(), tx, issue)
		return err
	}); err != nil {
		t.Fatalf("read the phases rows of %s: %v", issue, err)
	}
	for _, row := range rows {
		if row.Role == claim.RoleTester {
			return row
		}
	}
	t.Fatalf("%s has no tester's phases row in %+v", issue, rows)
	return record.PhaseRow{}
}

// A completion is checked twice: by the route, against the phase entry its claim serves, and by
// the workflow where its fact commits (decision 5 of dispatch://INFRA-4730). A tester's
// completion of its testing entry passes the route's check, and is held there (Options.CallJudged)
// while a person's backlog stops the issue, its phase kept, and their todo returns it, which
// resumes testing at a new entry raised as a move back (R20 of dispatch://INFRA-3320). Released,
// the completion is of the entry the stop ended: it is refused where it commits, the issue stays
// in testing on the new entry, and the tester's row records no handoff. So is the same completion
// naming the pull request's head, which no push has moved, since the return asks for the phase's
// work again.
//
// R12's return is the other case: a parent whose open child stops its testing moves into
// awaiting_children at the admission pass and comes back once the child is done, at a new entry
// raised as a move back, so a completion of the entry before the wait that passed the route's
// check before the wait began is refused by the same check where it commits.
func TestACompletionPausedAcrossAStopIsRefusedAtItsCommit(t *testing.T) {
	for _, tc := range []struct {
		name   string
		commit string
	}{
		{name: "naming its handoff record"},
		{name: "naming the pull request's unchanged head", commit: headSHA},
	} {
		t.Run("a stop and a return, "+tc.name, func(t *testing.T) {
			var h *harness
			var engine *workflow.Engine
			var told []string
			paused := func(ctx context.Context, op string, issues []string) {
				told = append(told, op)
				if len(told) > 1 {
					return
				}
				for i, status := range []string{"backlog", "todo"} {
					applyEntryFact(t, h, engine, "person-"+status, intake.DispatchIssue{Key: "LEGION-208", Seq: int64(10 + i), Type: "issue.updated",
						Status: status, Title: "LEGION-208", ResolvedRepo: "acme/widgets"})
				}
			}
			h, engine, _ = newWorkflowHarness(t, workflow.Config{Project: testProject, Repositories: testRepositories,
				Steps: config.DesignGateRootIssues.Steps(), ReviewRoundCap: 3}, func(opts *Options) { opts.CallJudged = paused })
			seedIssueAt(t, h, "LEGION-208", phase.Testing)
			seedPullRequest(t, h, "LEGION-208", headSHA)
			seedEntry(t, h, "LEGION-208", claim.RoleTester, 1)
			tester := newLiveClaim(t, h, "LEGION-208", claim.RoleTester)
			takeTask(t, h, tester.token, "outbox:test-1", "test LEGION-208", 1, 1)

			refused(t, h.complete(HandoffCompleteRequest{GrantID: tester.grant(t), Summary: "tests pass", Verdict: "pass", Commit: tc.commit,
				HandoffID: tester.writeHandoff("test", 1), HandoffVersion: 1}), http.StatusConflict, "HANDOFF_STALE_ENTRY")
			if len(told) != 1 || told[0] != "handoff_complete" {
				t.Fatalf("the seam was told %v, want the completion once: the route's check passed it before the stop", told)
			}
			assertIssuePhase(t, h, phase.Testing)
			if row := testerRow(t, h, "LEGION-208"); row.Entry != 2 || !row.EntryMovedBack || row.HandoffRecord != "" {
				t.Fatalf("the tester's row after the refused completion = %+v, want entry 2, raised by the return as a move back, with no handoff recorded", row)
			}
		})
	}

	t.Run("a parent's return from awaiting_children", func(t *testing.T) {
		var h *harness
		var handlers []intake.Handler
		var waited, back phase.Phase
		var told []string
		parent := func() phase.Phase {
			if issue := recordedChild(t, h, statusRoot); issue != nil {
				return issue.Phase
			}
			return ""
		}
		paused := func(ctx context.Context, op string, issues []string) {
			told = append(told, op)
			if len(told) > 1 {
				return
			}
			// The child's event reaches the admission pass, which finds it open under the parent
			// at work and moves the parent into awaiting_children; a person's done on the child
			// then ends the wait, and the parent comes back.
			for i, status := range []string{"in_progress", "done"} {
				if _, err := intake.ApplyFact(ctx, h.store.Pool(), "dispatch", "child-"+status, intake.DispatchIssue{Key: statusChild, Seq: int64(5 + i),
					Type: "issue.updated", Status: status, Title: "the child", Parent: statusRoot, Rank: "B", ResolvedRepo: "acme/widgets", ClaimKnown: true}, handlers...); err != nil {
					t.Errorf("apply the child's %s: %v", status, err)
				}
				if i == 0 {
					waited = parent()
				}
			}
			back = parent()
		}
		h, handlers, statuses := statusHarness(t, func(opts *Options) { opts.CallJudged = paused })
		h.handoffs = statuses
		setIssue(t, h, statusRoot, func(issue *record.Issue) { issue.Phase = phase.Testing })
		if err := store.Transact(context.Background(), h.store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
			if err := seedCopy(tx, statusRoot, "", "in_progress"); err != nil {
				return err
			}
			return seedCopy(tx, statusChild, statusRoot, "in_progress")
		}); err != nil {
			t.Fatalf("seed the copy: %v", err)
		}
		seedEntry(t, h, statusRoot, claim.RoleTester, 1)
		tester := newLiveClaim(t, h, statusRoot, claim.RoleTester)
		takeTask(t, h, tester.token, "outbox:test-1", "test LEGION-208", 1, 1)

		refused(t, h.complete(HandoffCompleteRequest{GrantID: tester.grant(t), Summary: "tests pass", Verdict: "pass",
			HandoffID: tester.writeHandoff("test", 1), HandoffVersion: 1}), http.StatusConflict, "HANDOFF_STALE_ENTRY")
		if len(told) != 1 || told[0] != "handoff_complete" {
			t.Fatalf("the seam was told %v, want the completion once: the route's check passed it before the wait", told)
		}
		if waited != phase.AwaitingChildren || back != phase.Testing {
			t.Fatalf("the parent waited at %q and came back at %q, want awaiting_children and then testing: this case proves nothing of R12's return", waited, back)
		}
		if row := testerRow(t, h, statusRoot); row.Entry != 2 || !row.EntryMovedBack || row.HandoffRecord != "" {
			t.Fatalf("the tester's row after the refused completion = %+v, want entry 2, raised by the return as a move back, with no handoff recorded", row)
		}
	})
}

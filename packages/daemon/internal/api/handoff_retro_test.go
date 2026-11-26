package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// retroStepStartedAt is when the retro step row of seedRetroAfterClose's run started: the moment
// the retro after the close counts its bound from (workflow.RetroAfterCloseBound). The run itself
// started a day before, and the close opened the retro a minute after its step row's start.
var retroStepStartedAt = time.Date(2026, 11, 25, 8, 0, 0, 0, time.UTC)

// retroRating and retroReason are the rating of the plan, and its reason, a retro's report in these
// tests carries.
const (
	retroRating = 4
	retroReason = "The plan named the tests the change needed before the code."
)

// seedRetroAfterClose records the run key stands on as started, its retro step row as started,
// and the retro Legion runs after key's close as open on that run, its agent the claim token
// agent: what the close leaves for a run that had no retro (workflow's openRetroAfterClose), and
// what the completion route reads to take the retro's report (INFRA-5602's P2). key is a root, its
// own tree, as seedIssueAt records it. It answers the run.
func seedRetroAfterClose(t *testing.T, h *harness, key string, agent claim.Token) uint64 {
	t.Helper()
	run := issueGeneration(t, h, key)
	records := record.NewStore()
	if err := store.Transact(context.Background(), h.store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		if err := records.MarkRunStarted(ctx, tx, key, run, key, retroStepStartedAt.Add(-24*time.Hour)); err != nil {
			return err
		}
		if err := records.MarkRetroStarted(ctx, tx, key, run, key, retroStepStartedAt); err != nil {
			return err
		}
		return records.OpenRetroAfterClose(ctx, tx, key, run, key, retroStepStartedAt.Add(time.Minute),
			retroStepStartedAt.Add(workflow.RetroAfterCloseBound), agent, 0)
	}); err != nil {
		t.Fatalf("open the retro after the close of %s: %v", key, err)
	}
	return run
}

// worksRetro hands c's claim the task of the retro after its issue's close of run and starts its
// turn, as the retro's own start row does (a task of no phase carrying the run,
// record.SuperviseRequest.RetroRun), by worksTask.
func (c liveClaim) worksRetro(t *testing.T, run uint64) {
	t.Helper()
	machine := c.worksTask(t, supervise.RequestDeliver{Task: retroTaskOf(c.issue), ID: fmt.Sprintf("outbox:retro:%s:%d", c.token, run),
		Generation: issueGeneration(t, c.h, c.issue), RetroRun: supervise.RetroRun(run)})
	if got := machine.Claim().ServingRetro(); got != supervise.RetroRun(run) {
		t.Fatalf("the claim serves retro run %d, want run %d", got, run)
	}
}

// retroTaskOf is the first line of the retro's task on key, as every retro task opens.
func retroTaskOf(key string) string {
	return fmt.Sprintf("Continue %s. Issue: %s. Phase: retro.", key, key)
}

// retroReportBy is the report of the retro after a close the plugin posts for grant: the summary
// and the rating of the plan with its reason, and no verdict, no commit and no wake.
func retroReportBy(grant string) HandoffCompleteRequest {
	rating, reason := retroRating, retroReason
	return HandoffCompleteRequest{GrantID: grant, Summary: "the retro of the closed run", PlanRating: &rating, PlanReason: &reason}
}

// newRetroAfterCloseClaim records LEGION-208 done with its retro after the close open on its run,
// the implementer's (the agent of the retro on main), and answers the implementer's live claim
// working that retro's task, and the run.
func newRetroAfterCloseClaim(t *testing.T, h *harness) (liveClaim, uint64) {
	t.Helper()
	seedIssueAt(t, h, "LEGION-208", phase.Done)
	implementer := newLiveClaim(t, h, "LEGION-208", claim.RoleImplementer)
	run := seedRetroAfterClose(t, h, "LEGION-208", implementer.token)
	implementer.worksRetro(t, run)
	return implementer, run
}

// The report of the retro Legion runs after a done issue's close (INFRA-5602's s2, s3). The
// claim whose worker took that retro's task completes with the rating of the plan and its reason
// and with no verdict: the route answers 200 and applies the one fact of that report, carrying
// the run the claim serves, and does nothing else: no status write, since the status stays as
// the close left it, and no read of GitHub, since the retro pushed nothing. The same report sent
// again is answered as already recorded, with nothing more applied.
//
// Each other shape is refused before the fact is applied, with nothing recorded, so the same
// report sent again in its right shape is then taken under the key the refused one would have
// taken: one with no rating (PLAN_RATING_REQUIRED, since the report of a retro rates the plan),
// and one that carries what no retro's report carries: a verdict, a commit, or the wake of a
// production check's pending report. Each refusal names the field.
func TestACompletionOfADoneIssuesRetroAfterItsCloseIsTakenWithItsRating(t *testing.T) {
	t.Run("a rating and its reason, and no verdict", func(t *testing.T) {
		github := newHandoffGitHub(t)
		h, facts := newHandoffHarness(t, github, nil, nil)
		implementer, run := newRetroAfterCloseClaim(t, h)

		completion(t, h.complete(retroReportBy(implementer.grant(t))))
		want := intake.RetroReport{Issue: "LEGION-208", Claim: implementer.token, Generation: run,
			PlanRating: record.PlanRating{Rating: retroRating, Reason: retroReason}}
		if got := facts.recorded(); len(got) != 1 || got[0] != want {
			t.Fatalf("facts = %#v, want the one report %#v", got, want)
		}
		if got := h.handoffs.writes; len(got) != 0 {
			t.Fatalf("status writes = %+v, want none: the retro after a close moves the issue nowhere", got)
		}
		if served := github.serves(); len(served) != 0 {
			t.Fatalf("GitHub was asked for %v, want nothing: the retro pushed nothing", served)
		}

		message := refused(t, h.complete(retroReportBy(implementer.grant(t))), http.StatusConflict, "HANDOFF_ALREADY_RECORDED")
		if !strings.Contains(message, "retro") {
			t.Fatalf("the refusal of the report sent again is %q, want it to name the retro it already recorded", message)
		}
		if got := facts.recorded(); len(got) != 1 {
			t.Fatalf("facts after the report sent again = %#v, want the first alone", got)
		}
	})

	for _, tc := range []struct {
		name   string
		change func(req *HandoffCompleteRequest)
		status int
		code   string
		field  string
	}{
		{name: "no rating", change: func(req *HandoffCompleteRequest) { req.PlanRating, req.PlanReason = nil, nil },
			status: http.StatusBadRequest, code: "PLAN_RATING_REQUIRED", field: "planRating"},
		{name: "a verdict", change: func(req *HandoffCompleteRequest) { req.Verdict = "pass" },
			status: http.StatusBadRequest, code: "RETRO_VERDICT_UNEXPECTED", field: "verdict"},
		{name: "a commit", change: func(req *HandoffCompleteRequest) { req.Commit = headSHA },
			status: http.StatusBadRequest, code: "RETRO_COMMIT_UNEXPECTED", field: "commit"},
		{name: "a wake", change: func(req *HandoffCompleteRequest) { req.WakeAt = &checkWakeAt },
			status: http.StatusBadRequest, code: "CHECK_WAKE_UNEXPECTED", field: "wakeAt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, facts := newHandoffHarness(t, newHandoffGitHub(t), nil, nil)
			implementer, run := newRetroAfterCloseClaim(t, h)

			req := retroReportBy(implementer.grant(t))
			tc.change(&req)
			if message := refused(t, h.complete(req), tc.status, tc.code); !strings.Contains(message, tc.field) {
				t.Fatalf("the %s refusal is %q, want it to name %s", tc.code, message, tc.field)
			}
			if got := facts.recorded(); len(got) != 0 {
				t.Fatalf("facts = %#v, want none: the refusal is written before any fact is applied", got)
			}

			completion(t, h.complete(retroReportBy(implementer.grant(t))))
			got := facts.recorded()
			if len(got) != 1 {
				t.Fatalf("facts after the report in its right shape = %#v, want its one report of run %d: the refusal spent nothing", got, run)
			}
			if report, ok := got[0].(intake.RetroReport); !ok || report.Generation != run {
				t.Fatalf("the fact after the report in its right shape = %#v, want the report of run %d", got[0], run)
			}
		})
	}
}

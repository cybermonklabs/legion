package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/credential"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// reviewedSHA is the head the round's recorded reviewer approved in reviewing, which the pull
// request has since moved off: headSHA is its head now.
const reviewedSHA = "c0de00000000000000000000000000000000ee01"

// reviewVerdict posts req to the review verdict route.
func (h *harness) reviewVerdict(req ReviewVerdictRequest) *httptest.ResponseRecorder {
	h.t.Helper()
	return h.request(http.MethodPost, "/legion/v1/review-verdict", req, nil)
}

// seedRecordedReviewer records reviewer as the claim whose completion ended key's review round, on
// the reviewer's phases row, as the workflow records that completion (workflow's handoff): its
// review record, the head it reviewed, its approve and the round's approval of that head.
func seedRecordedReviewer(t *testing.T, h *harness, key string, reviewer claim.Token) {
	t.Helper()
	err := store.Transact(context.Background(), h.store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		return record.NewStore().PutPhase(context.Background(), tx, record.PhaseRow{Issue: key, Role: claim.RoleReviewer, Claim: reviewer,
			HandoffCommit: reviewedSHA, HandoffRecord: "review-1", HandoffVersion: 1, Summary: "reviewed",
			Decision: &record.ReviewDecision{State: "approved", Head: reviewedSHA}})
	})
	if err != nil {
		t.Fatalf("seed the recorded reviewer of %s: %v", key, err)
	}
}

// The places of the review steps of a task of the default steps (planner, implementer, tester,
// reviewer, acceptance:owner): the reviewer's step and the owner's acceptance.
const (
	reviewerPlace   = 4
	acceptancePlace = 5
)

// seedStepAgent records agent as the agent of the latest start of the step at place of key's task
// default, as the daemon's start writes it (workflow's startStep), and ends that run where ended:
// the step has passed.
func seedStepAgent(t *testing.T, h *harness, key string, place int, agent claim.Token, ended bool) {
	t.Helper()
	at := record.StepPlace{Task: plan.DefaultTask, Kind: record.PlaceStep, Place: place}
	started := time.Date(2026, 11, 21, 22, place, 0, 0, time.UTC)
	if err := store.Transact(context.Background(), h.store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		records := record.NewStore()
		if _, err := records.RaiseStepStarts(context.Background(), tx, key, at, func(int64) record.StepRecord {
			return record.StepRecord{Task: at.Task, Step: at.Place, Kind: at.Kind, Agent: string(agent), StartedAt: &started}
		}); err != nil || !ended {
			return err
		}
		_, _, err := records.EndStepStart(context.Background(), tx, key, at)
		return err
	}); err != nil {
		t.Fatalf("record %s as the agent of step %d of %s: %v", agent, place, key, err)
	}
}

// pastReviewChild is the issue the verdict past review is sent for: LEGION-208, a child of the
// root LEGION-207, standing at at, with its pull request acme/widgets#42 at headSHA and its review
// round ended by its reviewer's completion at reviewedSHA. The step records name the agent of each
// of its review steps: its reviewer for the reviewer's step, passed, and the architect of its tree
// root, the architect above it, for the owner's acceptance (workflow's acceptingAgent), passed
// past accepting and running in accepting. It answers the claims of that reviewer and that
// architect.
func pastReviewChild(t *testing.T, h *harness, at phase.Phase) (reviewer, architect liveClaim) {
	t.Helper()
	parent := "LEGION-207"
	if err := store.Transact(context.Background(), h.store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		records := record.NewStore()
		if err := records.PutIssue(context.Background(), tx, record.Issue{Key: parent, Repo: "acme/widgets", DefaultBranch: "main",
			GitHubRepo: "acme/widgets", Tree: parent, Project: testProject, Title: parent, Phase: phase.Implementing, Generation: 1, Status: "in_progress"}); err != nil {
			return err
		}
		return records.PutIssue(context.Background(), tx, record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main",
			GitHubRepo: "acme/widgets", Tree: parent, Parent: &parent, Project: testProject, Title: "LEGION-208", Phase: at, Generation: 1, Status: "in_progress"})
	}); err != nil {
		t.Fatalf("seed LEGION-208 under %s at %s: %v", parent, at, err)
	}
	seedPullRequest(t, h, "LEGION-208", headSHA)
	reviewer = newLiveClaimIn(t, h, parent, "LEGION-208", claim.RoleReviewer)
	architect = newLiveClaimIn(t, h, parent, parent, claim.RoleArchitect)
	seedRecordedReviewer(t, h, "LEGION-208", reviewer.token)
	seedStepAgent(t, h, "LEGION-208", reviewerPlace, reviewer.token, true)
	seedStepAgent(t, h, "LEGION-208", acceptancePlace, architect.token, at != phase.Accepting)
	return reviewer, architect
}

// newVerdictWritingHarness is the daemon's routes over a real Postgres with the workflow deciding
// every fact they apply, as newWorkflowHandoffHarness serves them, and a writer of legion-verdict
// that appends the head of each write the workflow asks for to asked, once the fact's transaction
// commits (workflow.Config.WriteVerdict). The write itself is the daemon's (writeVerdict), which
// its own TestAHeadThatMovesPastReviewAsksTheRecordedReviewer drives against a fake GitHub.
func newVerdictWritingHarness(t *testing.T, asked *[]string) *harness {
	t.Helper()
	h := newHarness(t)
	records := record.NewStore()
	engine := workflow.New(records, workflow.Config{Project: testProject, Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps(),
		ReviewRoundCap: 3, WriteVerdict: func(_, _ string, _ int, head string) { *asked = append(*asked, head) }},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.handoffs = &statusRecorder{}
	h.handler = NewServer("127.0.0.1", 8437, Options{
		Steps:      config.DesignGateRootIssues.Steps(),
		Supervisor: h.supervisor, BootTokens: h.tokens, Project: testProject, OperatorToken: testOperatorToken,
		Controller: h.store, Grants: credential.New(nil), Pool: h.store.Pool(), Record: records,
		Handlers: []intake.Handler{engine}, Dispatch: h.handoffs, Tokens: handoffTokens{}, GitHubAPI: newHandoffGitHub(t).url,
	}).Handler
	return h
}

// stepVerdictAt is the verdict the store holds of the step at place of LEGION-208's task default on
// acme/widgets#42 at head (store.StepVerdictTx), the one the writer of legion-verdict reads for
// that step, and whether it holds one.
func stepVerdictAt(t *testing.T, h *harness, head string, place int) (store.Verdict, bool) {
	t.Helper()
	var given store.Verdict
	var found bool
	if err := store.Transact(context.Background(), h.store.Pool(), func(ctx context.Context, tx pgx.Tx) (err error) {
		given, found, err = store.StepVerdictTx(context.Background(), tx, ghrepo.MustParse("acme/widgets"), 42, head, plan.DefaultTask, place)
		return err
	}); err != nil {
		t.Fatalf("read the verdict of step %d at %s: %v", place, head, err)
	}
	return given, found
}

// R5 and C57: the verdict past review. With LEGION-208, a task of the default steps, in accepting,
// retro or merging, no review round is open, and `POST /legion/v1/review-verdict` takes the verdict
// of the agent of each review step of the task that has passed, as its step record names it: the
// reviewer's for the reviewer's step, and in retro and merging the owner's, from the architect above
// LEGION-208 that ran its acceptance, which names LEGION-208 as the issue it is for, since its own
// grant is its own issue's. In accepting the owner's acceptance is the step that runs, whose verdict
// is its sign_off, so its call there is refused (below). The call records that step's verdict at
// the pull request's current head, writes no handoff, leaves the issue's phase as it was, and asks
// for the write of legion-verdict there in its own pass. The same call from any other claim - the
// implementer's, the tester's, the planner's, or the architect's naming no issue, so of its own
// issue, where it ran no review step - gets VERDICT_CALLER_FORBIDDEN, and one that names another
// head VERDICT_HEAD_NOT_CURRENT; each refusal records nothing and asks for no write.
func TestReviewVerdictPastReviewTakesOnlyAReviewStepsAgent(t *testing.T) {
	for _, at := range []phase.Phase{phase.Accepting, phase.Retro, phase.Merging} {
		t.Run(string(at), func(t *testing.T) {
			var asked []string
			h := newVerdictWritingHarness(t, &asked)
			reviewer, architect := pastReviewChild(t, h, at)
			before := phaseRowOf(t, h, "LEGION-208", claim.RoleReviewer)

			for _, caller := range []struct {
				name string
				sent ReviewVerdictRequest
			}{
				{"the implementer's", ReviewVerdictRequest{GrantID: newLiveClaimIn(t, h, "LEGION-207", "LEGION-208", claim.RoleImplementer).grant(t)}},
				{"the tester's", ReviewVerdictRequest{GrantID: newLiveClaimIn(t, h, "LEGION-207", "LEGION-208", claim.RoleTester).grant(t)}},
				{"the planner's", ReviewVerdictRequest{GrantID: newLiveClaimIn(t, h, "LEGION-207", "LEGION-208", claim.RolePlanner).grant(t)}},
				{"the architect's naming no issue", ReviewVerdictRequest{GrantID: architect.grant(t)}},
			} {
				sent := caller.sent
				sent.Verdict, sent.Commit, sent.Summary = "approve", headSHA, "read the merge"
				message := refused(t, h.reviewVerdict(sent), http.StatusForbidden, "VERDICT_CALLER_FORBIDDEN")
				if !strings.Contains(message, "nothing was recorded") {
					t.Fatalf("the refusal of %s call says %q, want it to say nothing was recorded", caller.name, message)
				}
			}
			message := refused(t, h.reviewVerdict(ReviewVerdictRequest{GrantID: reviewer.grant(t), Verdict: "approve", Commit: reviewedSHA,
				Summary: "read the merge"}), http.StatusConflict, "VERDICT_HEAD_NOT_CURRENT")
			for _, want := range []string{reviewedSHA, headSHA, "nothing was recorded"} {
				if !strings.Contains(message, want) {
					t.Fatalf("the refusal of a verdict at another head says %q, want it to say %q", message, want)
				}
			}
			for _, place := range []int{reviewerPlace, acceptancePlace} {
				if given, found := stepVerdictAt(t, h, headSHA, place); found {
					t.Fatalf("after the refusals the store holds the verdict %+v of step %d, want none", given, place)
				}
			}
			if len(asked) != 0 {
				t.Fatalf("the refusals asked for writes of legion-verdict on %v, want none", asked)
			}

			for i, tc := range []struct {
				name  string
				by    liveClaim
				sent  ReviewVerdictRequest
				place int
			}{
				{"the reviewer's changes", reviewer, ReviewVerdictRequest{Verdict: "changes", Commit: headSHA, Summary: "the merge breaks the parser"}, reviewerPlace},
				{"the reviewer's approve", reviewer, ReviewVerdictRequest{Verdict: "approve", Commit: headSHA, Summary: "the merge changes no code that runs"}, reviewerPlace},
				{"the owner's approve", architect, ReviewVerdictRequest{Verdict: "approve", Commit: headSHA, Issue: "LEGION-208", Summary: "the owner read the merge"}, acceptancePlace},
			} {
				if at == phase.Accepting && tc.place == acceptancePlace {
					continue
				}
				tc.sent.GrantID = tc.by.grant(t)
				if recorder := h.reviewVerdict(tc.sent); recorder.Code != http.StatusOK {
					t.Fatalf("%s = %d: %s", tc.name, recorder.Code, recorder.Body)
				}
				if given, found := stepVerdictAt(t, h, headSHA, tc.place); !found || string(given.Verdict) != tc.sent.Verdict ||
					given.Session != string(tc.by.token) || given.Issue != "LEGION-208" {
					t.Fatalf("after %s the verdict of step %d at %s = %+v (found %t), want %s's %s", tc.name, tc.place, headSHA, given, found, tc.by.token, tc.sent.Verdict)
				}
				if want := slices.Repeat([]string{headSHA}, i+1); !slices.Equal(asked, want) {
					t.Fatalf("after %s the workflow asked for writes of legion-verdict on %v, want %v: one in each verdict's own pass", tc.name, asked, want)
				}
				assertIssuePhase(t, h, at)
			}
			after := phaseRowOf(t, h, "LEGION-208", claim.RoleReviewer)
			if decided := after.Decision; decided == nil || decided.State != "approved" || decided.Head != headSHA {
				t.Fatalf("the round's decision = %+v, want the reviewer's approve at %s", decided, headSHA)
			}
			after.Decision = before.Decision
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("the reviewer's row after the verdicts = %+v, want it as it stood but for the decision, %+v: the call writes no handoff", after, before)
			}
		})
	}

	// The reviewer's verdict in reviewing and the owner's in accepting, each while its own step runs,
	// and the reviewer's with a word or a commit missing.
	for _, tc := range []struct {
		name   string
		at     phase.Phase
		owner  bool
		sent   ReviewVerdictRequest
		status int
		code   string
	}{
		{name: "in reviewing, where its completion carries the verdict", at: phase.Reviewing,
			sent: ReviewVerdictRequest{Verdict: "approve", Commit: headSHA, Summary: "reviewed"}, status: http.StatusConflict, code: "VERDICT_NOT_PAST_REVIEW"},
		{name: "the owner's in accepting, where its sign_off carries the verdict", at: phase.Accepting, owner: true,
			sent: ReviewVerdictRequest{Verdict: "approve", Commit: headSHA, Issue: "LEGION-208", Summary: "the owner read the merge"}, status: http.StatusConflict, code: "VERDICT_NOT_PAST_REVIEW"},
		{name: "the tester's pass", at: phase.Retro,
			sent: ReviewVerdictRequest{Verdict: "pass", Commit: headSHA, Summary: "read the merge"}, status: http.StatusBadRequest, code: "REVIEWER_VERDICT_REQUIRED"},
		{name: "no commit", at: phase.Retro,
			sent: ReviewVerdictRequest{Verdict: "approve", Summary: "read the merge"}, status: http.StatusBadRequest, code: "VERDICT_COMMIT_REQUIRED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var asked []string
			h := newVerdictWritingHarness(t, &asked)
			reviewer, architect := pastReviewChild(t, h, tc.at)
			tc.sent.GrantID = reviewer.grant(t)
			if tc.owner {
				tc.sent.GrantID = architect.grant(t)
			}
			refused(t, h.reviewVerdict(tc.sent), tc.status, tc.code)
			for _, place := range []int{reviewerPlace, acceptancePlace} {
				if given, found := stepVerdictAt(t, h, headSHA, place); found || len(asked) != 0 {
					t.Fatalf("after the refusal the store holds %+v of step %d (found %t) and the workflow asked for writes on %v, want nothing", given, place, found, asked)
				}
			}
		})
	}

	// The route hands the workflow the issue the call names, and the caller's own issue where it
	// names none.
	t.Run("the fact names the issue the call names", func(t *testing.T) {
		h, facts := newHandoffHarness(t, newHandoffGitHub(t), nil, nil)
		reviewer, architect := pastReviewChild(t, h, phase.Retro)
		for _, tc := range []struct {
			by   liveClaim
			sent ReviewVerdictRequest
		}{
			{reviewer, ReviewVerdictRequest{Verdict: "approve", Commit: headSHA, Summary: "read the push"}},
			{architect, ReviewVerdictRequest{Verdict: "changes", Commit: headSHA, Issue: "LEGION-208", Summary: "the owner read the push"}},
		} {
			tc.sent.GrantID = tc.by.grant(t)
			if recorder := h.reviewVerdict(tc.sent); recorder.Code != http.StatusOK {
				t.Fatalf("the call of %s = %d: %s", tc.by.token, recorder.Code, recorder.Body)
			}
			got := facts.recorded()
			want := intake.ReviewVerdict{Issue: "LEGION-208", Claim: tc.by.token, Verdict: tc.sent.Verdict, Commit: headSHA, Summary: tc.sent.Summary}
			if len(got) == 0 || !reflect.DeepEqual(got[len(got)-1], want) {
				t.Fatalf("the facts after the call of %s = %#v, want the last %#v", tc.by.token, got, want)
			}
		}
	})
}

// The bodies of a review step's verdict as Go writes them, and the route's answer: the reviewer's
// verdict past review, the owner's, which names the issue it is for, and the reviewer's completion
// of reviewing. contracts' legion-api.test.ts parses each under the schema that mirrors it
// (LegionReviewVerdictRequest, LegionEmptyResponse, LegionHandoffCompleteRequest), and the request
// schemas are strict, so a name Go writes and the contract does not read fails there.
func TestReviewerVerdictBodiesGolden(t *testing.T) {
	golden(t, "review-verdict-request.json", ReviewVerdictRequest{GrantID: "grant-for-one-command", Verdict: "approve",
		Commit: headSHA, Summary: "The merge forward changes no code that runs."})
	golden(t, "review-verdict-request-owner.json", ReviewVerdictRequest{GrantID: "grant-for-one-command", Verdict: "changes",
		Commit: headSHA, Issue: "LEGION-208", Summary: "The owner reads a change the plan does not cover."})
	golden(t, "review-verdict.json", EmptyResponse{})
	golden(t, "handoff-complete-request-reviewer.json", HandoffCompleteRequest{GrantID: "grant-for-one-command",
		Summary: "Reviewed; no finding stands.", Verdict: "approve", Commit: headSHA,
		HandoffID: "5b1f0c2e-9d7a-4c3b-8e61-2f4a9d0c7b13", HandoffVersion: 1})
}

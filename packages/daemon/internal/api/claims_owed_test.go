package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// askOwed posts the owed route's body as the caller names it, so a refusal can be asked for.
func askOwed(h *harness, req OwedRequest) *httptest.ResponseRecorder {
	h.t.Helper()
	return h.request(http.MethodPost, "/legion/v1/claims/owed", req, nil)
}

// owed is what c's own session is told its claim owes now, asked with the secret its registration
// issued it.
func (c liveClaim) owed(t *testing.T) OwedResponse {
	t.Helper()
	recorder := askOwed(c.h, OwedRequest{ClaimToken: c.token, SessionID: c.session, Secret: c.secret})
	if recorder.Code != http.StatusOK {
		t.Fatalf("owed for %s = %d, want 200; body %s", c.token, recorder.Code, recorder.Body)
	}
	var answer OwedResponse
	decodeInto(t, recorder, &answer)
	return answer
}

// owes asserts c owes work, and that the answer names it as want.
func (c liveClaim) owes(t *testing.T, want string) {
	t.Helper()
	if got := c.owed(t); !got.Owes || got.Work != want {
		t.Fatalf("%s owes %+v, want it to owe %q", c.token, got, want)
	}
}

// owesNothing asserts c owes nothing, and that the answer names no work.
func (c liveClaim) owesNothing(t *testing.T) {
	t.Helper()
	if got := c.owed(t); got != (OwedResponse{}) {
		t.Fatalf("%s owes %+v, want nothing and no work named", c.token, got)
	}
}

// The defect this route cures (dispatch://INFRA-2746): a reminder state kept per process nagged
// every role whose phase had finished, and started again as open at every restart. The answer is
// read from the record instead, so it follows the workflow as the workflow moves the issue, and a
// restarted process reads what its predecessor would have.
//
// The issue is driven through the daemon's own workflow, one completion at a time, and each role is
// asked at each step: the worker of the current phase, with its work undone, owes that phase and
// the answer names it; a worker whose phase has finished owes nothing, whether the issue moved to
// another role's phase (the implementer and the tester) or stayed in the role's own phase with the
// completion recorded on its row (the reviewer, whose round waits for a review that decides it).
func TestClaimOwedFollowsThePhaseAsTheWorkflowMovesTheIssue(t *testing.T) {
	h, _, _ := newAssignmentHarness(t)
	ctx := context.Background()
	h.recordIssue(record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: "LEGION-208",
		Project: testProject, Title: "LEGION-208", Phase: phase.Implementing, Generation: 1, Status: "in_progress"})
	if err := h.store.Tx(ctx, func(tx pgx.Tx) error {
		return record.NewStore().PutPullRequest(ctx, tx, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "acme/widgets",
			Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head", CheckedHead: "head", Failing: []string{}})
	}); err != nil {
		t.Fatalf("record the pull request: %v", err)
	}

	implementer := newLiveClaim(t, h, "LEGION-208", claim.RoleImplementer)
	implementer.owes(t, "the implementing phase of LEGION-208")

	completion(t, h.complete(HandoffCompleteRequest{GrantID: implementer.grant(t), Summary: "implemented", Commit: "handoff-1",
		HandoffID: implementer.writeHandoff("implement", 1), HandoffVersion: 1}))
	assertIssuePhase(t, h, phase.Testing)
	implementer.owesNothing(t)

	tester := newLiveClaim(t, h, "LEGION-208", claim.RoleTester)
	takeStart(t, h, tester.token, "LEGION-208", claim.RoleTester)
	tester.owes(t, "the testing phase of LEGION-208")

	completion(t, h.complete(HandoffCompleteRequest{GrantID: tester.grant(t), Summary: "tests pass", Verdict: "pass",
		HandoffID: tester.writeHandoff("test", 1), HandoffVersion: 1}))
	assertIssuePhase(t, h, phase.Reviewing)
	tester.owesNothing(t)
	implementer.owesNothing(t)

	// The tester's process is lost and the daemon relaunches it on the same session: the new
	// process holds a new secret and remembers nothing, and it is told what its predecessor was.
	tester.secret = h.registered(h.relaunch(tester.token), tester.session).Secret
	tester.owesNothing(t)

	reviewer := newLiveClaim(t, h, "LEGION-208", claim.RoleReviewer)
	takeStart(t, h, reviewer.token, "LEGION-208", claim.RoleReviewer)
	reviewer.owes(t, "the reviewing phase of LEGION-208")

	// The reviewer completes and no review has decided its round, so the issue stays in
	// reviewing, the reviewer's phase: its row records the completion the workflow took, and that
	// is what says the work is done.
	completion(t, h.complete(HandoffCompleteRequest{GrantID: reviewer.grant(t), Summary: "reviewed",
		HandoffID: reviewer.writeHandoff("review", 1), HandoffVersion: 1}))
	assertIssuePhase(t, h, phase.Reviewing)
	reviewer.owesNothing(t)
}

// A claim of a role whose phase another role works, or whose issue stands where no role works,
// owes nothing, and the answer names no work: what each role was handed is no part of it. The
// architect works no phase of its own — accepting is its sign_off, not a phase it completes — so it
// owes nothing even in accepting.
func TestClaimOwedIsNothingForARoleWhosePhaseAnotherRoleWorks(t *testing.T) {
	h, _, _ := newAssignmentHarness(t)
	for _, tc := range []struct {
		issue string
		at    phase.Phase
		role  claim.Role
	}{
		{issue: "LEGION-301", at: phase.Reviewing, role: claim.RoleTester},
		{issue: "LEGION-302", at: phase.Done, role: claim.RoleMerger},
		{issue: "LEGION-303", at: phase.Accepting, role: claim.RoleImplementer},
		{issue: "LEGION-304", at: phase.Accepting, role: claim.RoleArchitect},
		{issue: "LEGION-305", at: phase.Done, role: claim.RoleImplementer},
	} {
		t.Run(string(tc.role)+" in "+string(tc.at), func(t *testing.T) {
			seedIssueAt(t, h, tc.issue, tc.at)
			newLiveClaim(t, h, tc.issue, tc.role).owesNothing(t)
		})
	}
}

// The merger merges (dispatch://INFRA-714): merging is its working phase, with no wait for the
// daemon's merge after it, so a merger whose issue stands in merging with its pull request open
// owes that phase, and the answer names it. The merged pull request is what finishes merging
// (workflow.PhaseFinished), and a merged root stands in merging until the rest of its tree
// leaves, so once its pull request is merged the merger owes nothing though the phase stands.
func TestClaimOwedIsTheMergeUntilTheMergersPullRequestIsMerged(t *testing.T) {
	h, _, _ := newAssignmentHarness(t)
	ctx := context.Background()
	seedIssueAt(t, h, "LEGION-208", phase.Merging)
	putPullRequest := func(state record.PullRequestState) {
		t.Helper()
		if err := h.store.Tx(ctx, func(tx pgx.Tx) error {
			return record.NewStore().PutPullRequest(ctx, tx, record.PullRequest{State: state, Issue: "LEGION-208", Repo: "acme/widgets",
				Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head", CheckedHead: "head", Failing: []string{}})
		}); err != nil {
			t.Fatalf("record the pull request %s: %v", state, err)
		}
	}
	putPullRequest(record.PullRequestOpen)

	merger := newLiveClaim(t, h, "LEGION-208", claim.RoleMerger)
	merger.owes(t, "the merging phase of LEGION-208")

	putPullRequest(record.PullRequestMerged)
	assertIssuePhase(t, h, phase.Merging)
	merger.owesNothing(t)
}

// A claim the daemon has failed owes nothing, whatever its phase: the failure is the daemon's own
// word that the session is finished. This is what ends the reminding of a session whose provider
// refuses every turn (dispatch://INFRA-264): each refused turn is reported over
// `POST /legion/v1/claims/turn-outcome`, the run reaching the launch failure limit fails the
// claim, and the one delayed reminder a refused turn arms asks this route before it goes.
func TestClaimOwedIsNothingOnceTheDaemonHasFailedTheClaim(t *testing.T) {
	h, _, _ := newAssignmentHarness(t)
	seedIssueAt(t, h, "LEGION-208", phase.Testing)
	tester := newLiveClaim(t, h, "LEGION-208", claim.RoleTester)
	tester.owes(t, "the testing phase of LEGION-208")

	generation := h.stored(tester.token).Generation
	for sequence := uint64(1); h.stored(tester.token).State != supervise.StateFailed; sequence++ {
		if sequence > 10 {
			t.Fatalf("ten refused turns left %s %s, want it failed", tester.token, h.stored(tester.token).State)
		}
		recorder, _ := h.turnOutcome(claim.TurnOutcomeRequest{
			ClaimToken: tester.token, SessionID: tester.session, Secret: tester.secret,
			Generation: generation, Sequence: sequence, Outcome: claim.TurnRefused, Message: "the provider refused the request",
		})
		if recorder.Code != http.StatusOK {
			t.Fatalf("turn outcome %d = %d, want 200; body %s", sequence, recorder.Code, recorder.Body)
		}
	}
	tester.owesNothing(t)
}

// A tester holding an open look of its closed issue's production check owes that look, and the
// answer names it; once the check closes, it owes nothing. Done runs no phase, so the look is the
// whole of what it can owe.
func TestClaimOwedIsTheProductionCheckLookItsTesterHolds(t *testing.T) {
	h, _, _ := newAssignmentHarness(t)
	seedIssueAt(t, h, "LEGION-208", phase.Done)
	seedCheck(t, h, "LEGION-208", claim.RoleTester, false, false)
	tester := newCheckLookClaim(t, h, "LEGION-208", claim.RoleTester, 2)
	tester.owes(t, "the production check of LEGION-208")

	seedCheck(t, h, "LEGION-208", claim.RoleTester, true, false)
	tester.owesNothing(t)
}

// The three sources disagree here, and the claim's own live assignment is what says which kind of
// work its turn is on, as it says for the completion route. A merged close spares the tester's
// claim and opens a look behind its running testing turn: the record holds the look open for the
// tester, while the tester still serves its testing task. A report from that turn is refused as no
// report on production (CHECK_LOOK_NOT_SERVED), so the turn owes nothing — not the look, and not
// the testing phase of an issue that is done. Once the look's own task starts a turn, it owes the
// look.
func TestClaimOwedIsNoLookItsTesterHasNotTaken(t *testing.T) {
	h, _, tester, _ := mergedCloseBehindATestingTurn(t)
	tester.owesNothing(t)

	tester.worksLook(t, 1, recordedCheck(t, h, "LEGION-209").OpenedAt)
	tester.owes(t, "the production check of LEGION-209")
}

// The route never answers another claim's work, and it refuses exactly as claims/activity does:
// another claim's token, a wrong secret and a claim the daemon does not hold are each
// claim.InvalidSecret, and a body naming no claim is a 400 naming the field. It holds the session
// too, which activity does not read: a session the claim does not record is refused the same way.
func TestClaimOwedRefusesACallerThatCannotProveItsClaim(t *testing.T) {
	h, _, _ := newAssignmentHarness(t)
	seedIssueAt(t, h, "LEGION-208", phase.Testing)
	tester := newLiveClaim(t, h, "LEGION-208", claim.RoleTester)
	implementer := newLiveClaim(t, h, "LEGION-208", claim.RoleImplementer)
	tester.owes(t, "the testing phase of LEGION-208")

	for _, tc := range []struct {
		name string
		req  OwedRequest
	}{
		{name: "another claim's token", req: OwedRequest{ClaimToken: tester.token, SessionID: implementer.session, Secret: implementer.secret}},
		{name: "a wrong secret", req: OwedRequest{ClaimToken: tester.token, SessionID: tester.session, Secret: "not-the-secret"}},
		{name: "a wrong session", req: OwedRequest{ClaimToken: tester.token, SessionID: implementer.session, Secret: tester.secret}},
		{name: "a claim the daemon does not hold", req: OwedRequest{ClaimToken: "legion-legion-legion-999-tester", SessionID: tester.session, Secret: tester.secret}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wantRefusal(t, askOwed(h, tc.req), claim.InvalidSecret.Status, claim.InvalidSecret.Message)
		})
	}
	t.Run("a body naming no claim", func(t *testing.T) {
		wantRefusal(t, askOwed(h, OwedRequest{SessionID: tester.session, Secret: tester.secret}), http.StatusBadRequest, "claimToken is required")
	})
}

// A daemon that cannot say which role works a phase answers 500, not "nothing owed": the reminder
// reading a failure still nudges and says it could not read the record, where an empty answer
// would silence it on a guess.
func TestClaimOwedIsAFailureWhereTheDaemonCannotReadWhoWorksThePhase(t *testing.T) {
	h := newHarness(t)
	seedIssueAt(t, h, "LEGION-208", phase.Testing)
	tester := newLiveClaim(t, h, "LEGION-208", claim.RoleTester)
	recorder := askOwed(h, OwedRequest{ClaimToken: tester.token, SessionID: tester.session, Secret: tester.secret})
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("owed on a daemon with no RoleFor = %d, want 500; body %s", recorder.Code, recorder.Body)
	}
}

// The answer as the route writes it, built through owedBy from a tester's claim on an issue in
// testing, so the fixture the contracts mirror is held to is the handler's own body.
func TestClaimOwedResponseGolden(t *testing.T) {
	position := completionPosition{issue: &record.Issue{Key: "LEGION-208", Phase: phase.Testing, Generation: 1}}
	golden(t, "owed.json", owedBy(supervise.Claim{Role: claim.RoleTester}, position, workflow.RoleFor))
}

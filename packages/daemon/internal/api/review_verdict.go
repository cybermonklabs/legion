package api

import (
	"cmp"
	"fmt"
	"net/http"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// ReviewerVerdictContract is the daemon contract that gave the reviewer a verdict of its own
// (version.go's 272): its completion of reviewing names approve or changes on the head its review
// record names, and `POST /legion/v1/review-verdict` takes the verdict of the agent of a review
// step on a head that moved past review. A reviewer whose plugin declared an earlier contract knows
// the tester's words alone, and the daemon still serves its claim while its turn runs and while its
// pod is in an update round (staleClaim), so its completion is read in those words
// (reviewerVerdictOf).
const ReviewerVerdictContract = 272

// reviewerVerdictOf is the verdict a reviewer's word sent is, for a claim whose plugin declared
// contract, and the sentence of its refusal REVIEWER_VERDICT_REQUIRED where the word is none of
// the verdict's. The verdict is approve (the head may merge) or changes (the head needs changes).
// A plugin before ReviewerVerdictContract offers its reviewer the tester's words alone, so its
// pass is approve and its fail is changes, and its refusal names those two words as the verdict
// they are taken as: the old reviewer learns the words from this answer, since nothing else it
// reads names a verdict for it. Its pending, a check report's word, is refused as no verdict is.
func reviewerVerdictOf(sent string, contract int) (verdict, refusal string) {
	approve, changes := string(store.VerdictApprove), string(store.VerdictChanges)
	old := contract < ReviewerVerdictContract
	switch {
	case sent == approve || sent == changes:
		return sent, ""
	case old && sent == "pass":
		return approve, ""
	case old && sent == "fail":
		return changes, ""
	}
	named := fmt.Sprintf("not %q", sent)
	if sent == "" {
		named = "and this one names none"
	}
	if !old {
		return "", fmt.Sprintf("a reviewer's verdict on the head it reviewed is approve (the head may merge) or changes (the head needs changes), %s; nothing was recorded", named)
	}
	return "", fmt.Sprintf("a reviewer's completion names its verdict on the head it reviewed, %s: this reviewer's plugin speaks daemon API contract %d, from before the reviewer's verdict, so complete again with verdict pass, which this daemon takes as approve (the head may merge), or fail, which it takes as changes (the head needs changes); nothing was recorded",
		named, contract)
}

// ReviewVerdictRequest is `POST /legion/v1/review-verdict`'s body: the verdict of the agent of a
// review step, approve or changes, on Commit, the head its issue's pull request moved to past
// review, and Summary, its two sentences. Issue is the issue the verdict is for, which the
// architect that ran a child's owner acceptance names, since its grant is of its own issue; the
// reviewer leaves it out, and the grant's issue is the one. The `legion` tool's review_verdict sends
// it (packages/pi-shared/src/legion-phase-ops.ts), and contracts' LegionReviewVerdictRequest
// mirrors it.
type ReviewVerdictRequest struct {
	GrantID string `json:"grantId"`
	Verdict string `json:"verdict"`
	Commit  string `json:"commit"`
	Issue   string `json:"issue,omitempty"`
	Summary string `json:"summary"`
}

// reviewVerdict is the verdict past review: each time the head of an issue's pull request moves
// once a review step of the issue's task has passed (workflow's reviewVerdict, by the step walk),
// and so with no review round open, the agent of each review step of that task that has passed
// sends its verdict on the new head here: the reviewer, and the architect that ran the owner's
// acceptance, which names the issue it is for (ReviewVerdictRequest.Issue). The call is no
// completion and writes no handoff: it queues one intake.ReviewVerdict fact of the issue it names,
// or of the grant's issue where it names none, which the workflow records as that step's verdict at
// that head, and it moves no phase. The route refuses what it can judge from the call alone, each
// before the fact, so a refused call records nothing:
//
//   - VERDICT_CALLER_FORBIDDEN (403): the grant is the controller's, which runs no step.
//   - REVIEWER_VERDICT_REQUIRED (400): the verdict is neither approve nor changes
//     (reviewerVerdictOf).
//   - VERDICT_COMMIT_REQUIRED (400): the call names no commit.
//   - ISSUE_NOT_FOUND (404): the daemon records no such issue.
//
// The workflow judges the rest on the record under the fact's lock (workflow's reviewVerdict),
// each refusal recording nothing: a claim the record names the agent of no review step of the
// issue's task (VERDICT_CALLER_FORBIDDEN, 403), a caller whose own step has not passed, as the
// step that runs now has not, whose verdict is the reviewer's completion or the owner's sign_off
// or move back (VERDICT_NOT_PAST_REVIEW, 409), a lingering tree (TREE_LINGERING), and a commit
// that is not the head the daemon's record holds for the issue's pull request
// (VERDICT_HEAD_NOT_CURRENT, 409): the record's own head and never GitHub's, as the entry fence
// reads it (behindEntryClause).
//
// The fact is the request's own (requestFactID): two calls are two verdicts, the later one
// standing, so an agent that changes its verdict at one head is heard however often it does.
func (s *server) reviewVerdict(w http.ResponseWriter, r *http.Request) {
	var req ReviewVerdictRequest
	if !s.readBody(w, r, &req, s.redeemIdentityCheck) || !s.requireFailureFields(w, r, field{"grantId", req.GrantID}, field{"summary", req.Summary}) {
		return
	}
	grant, ok := s.redeem(w, r, req.GrantID)
	if !ok {
		return
	}
	if grant.Controller {
		s.writeFailure(w, r, http.StatusForbidden, "VERDICT_CALLER_FORBIDDEN",
			"review_verdict is the verdict of the agent of a review step of the issue's task, and this call is the controller's, which runs no step; nothing was recorded")
		return
	}
	if s.pool == nil || s.records == nil {
		s.writeFailure(w, r, http.StatusInternalServerError, "FACTS_UNAVAILABLE", "fact intake is unavailable")
		return
	}
	// No plugin before ReviewerVerdictContract offers this call, so its verdict is read in the
	// verdict's own two words alone.
	verdict, refusal := reviewerVerdictOf(req.Verdict, ReviewerVerdictContract)
	if refusal != "" {
		s.writeFailure(w, r, http.StatusBadRequest, "REVIEWER_VERDICT_REQUIRED", refusal)
		return
	}
	if req.Commit == "" {
		s.writeFailure(w, r, http.StatusBadRequest, "VERDICT_COMMIT_REQUIRED",
			"a verdict past review is a verdict on the pull request's current head, and this call names no commit: send it again with commit set to that head; nothing was recorded")
		return
	}
	key := cmp.Or(req.Issue, grant.Issue)
	issue, err := s.recordedIssue(r.Context(), key)
	if err != nil {
		s.writeFailure(w, r, http.StatusInternalServerError, "RECORD_UNAVAILABLE", "could not read the issue record")
		return
	}
	if issue == nil {
		s.writeFailure(w, r, http.StatusNotFound, "ISSUE_NOT_FOUND", fmt.Sprintf("%s is not recorded; nothing was recorded", key))
		return
	}
	result, err := s.apply(r.Context(), "api", requestFactID("review-verdict"), intake.ReviewVerdict{Issue: issue.Key, Claim: grant.Claim,
		Verdict: verdict, Commit: req.Commit, Summary: req.Summary})
	if err != nil {
		s.writeFailure(w, r, http.StatusInternalServerError, "FACT_APPLY_FAILED", "could not apply the review verdict's fact")
		return
	}
	if result.Refusal != nil {
		s.writeFailure(w, r, result.Refusal.Status, result.Refusal.Code, result.Refusal.Message)
		return
	}
	s.log.Info("api: recorded a review verdict", "issue", issue.Key, "claim", grant.Claim, "phase", issue.Phase,
		"verdict", verdict, "commit", req.Commit)
	s.writeAnswer(w, r, AnswerEmptyObject, http.StatusOK, EmptyResponse{})
}

package api

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// MergeJumpRequest sets one issue's jump mark: the controller's instruction that the next merge
// the daemon submits for Issue goes in at the head of GitHub's merge queue (record.MergeJump).
// Reason is what the controller says the jump is for, which the record of the jump keeps after
// the mark itself is spent, so the account of a pull request that went ahead of the queue names
// the decision. An issue holds at most one mark, so a second set of the same issue replaces the
// first rather than being refused: the route takes the controller's newest word.
type MergeJumpRequest struct {
	GrantID string `json:"grantId"`
	Issue   string `json:"issue"`
	Reason  string `json:"reason"`
}

// MergeJumpClearRequest takes an issue's jump mark off again, which is how a controller undoes a
// decision before the merge it was about is submitted. It needs no reason: the mark that carried
// one is gone, and nothing keeps the account of a jump that never happened.
type MergeJumpClearRequest struct {
	GrantID string `json:"grantId"`
	Issue   string `json:"issue"`
}

// MergeHoldsRequest carries the WHOLE merge hold list of the project and replaces it
// (record.Store.ReplaceMergeHolds): a hold has no identifier of its own, so a controller that
// wants to drop one sends the list without it, and an empty list clears every hold. That is why
// there is one route and not three: with no identity to name, an add and a remove would both be
// a replacement under another name.
type MergeHoldsRequest struct {
	GrantID string             `json:"grantId"`
	Holds   []MergeHoldRequest `json:"holds"`
}

// MergeHoldRequest is one hold of that list (record.MergeHold). Paths are regular expressions
// matched, unanchored, against the paths a pull request changes, and a hold catches a merge when
// one of them matches one changed file. Repository is `owner/name` and must be one the project
// serves; "" holds the path in every repository the project serves, which is what a hold of a
// path that several repositories carry wants. Reason is what the controller says the hold is
// for, and EndsAt when it stops holding, an absolute time in UTC: a hold ends by itself, so a
// controller that forgets one stops nobody for longer than it said.
type MergeHoldRequest struct {
	Paths      []string  `json:"paths"`
	Repository string    `json:"repository"`
	Reason     string    `json:"reason"`
	EndsAt     time.Time `json:"endsAt"`
}

// mergeJump sets one issue's jump mark. The order of the checks is every controller route's: the
// body (an unknown field is refused there), the fields it must carry, then the values, each
// refused by a code of its own and before the grant is redeemed, so a caller learns what is wrong
// with its request whether or not its grant is still good. The mark is written last, under a
// controller grant alone.
func (s *server) mergeJump(w http.ResponseWriter, r *http.Request) {
	var req MergeJumpRequest
	if !s.readBody(w, r, &req, s.redeemIdentityCheck) || !requireFailureFields(w,
		field{"grantId", req.GrantID}, field{"issue", req.Issue}, field{"reason", req.Reason}) {
		return
	}
	if !s.recordedMarkIssue(w, r, req.Issue, "mark for the jump") {
		return
	}
	grant, ok := s.redeem(w, r, req.GrantID)
	if !ok {
		return
	}
	if !grant.Controller {
		writeFailure(w, http.StatusForbidden, "CONTROLLER_REQUIRED", "a jump mark requires a controller grant")
		return
	}
	session, ok := s.markSession(w, r)
	if !ok {
		return
	}
	mark := record.MergeJump{Issue: req.Issue, Reason: req.Reason, Session: session, SetAt: s.now().UTC()}
	if err := pgx.BeginFunc(r.Context(), s.pool, func(tx pgx.Tx) error {
		return s.records.SetMergeJump(r.Context(), tx, mark)
	}); err != nil {
		s.logRequestFailure(r, err, "api: set a jump mark", "issue", req.Issue)
		writeFailure(w, http.StatusInternalServerError, "RECORD_UNAVAILABLE", fmt.Sprintf("could not set the jump mark of %s", req.Issue))
		return
	}
	writeJSON(w, http.StatusOK, EmptyResponse{})
}

// mergeJumpClear takes an issue's jump mark off. It answers the same whether a mark went or none
// was there: the controller asked for an issue with no mark, and that is what it has. The
// daemon's own spending of a mark reads the deletion's answer (record.Store.ClearMergeJump),
// because it must know the mark was still there when it submitted; nothing here does.
func (s *server) mergeJumpClear(w http.ResponseWriter, r *http.Request) {
	var req MergeJumpClearRequest
	if !s.readBody(w, r, &req, s.redeemIdentityCheck) || !requireFailureFields(w,
		field{"grantId", req.GrantID}, field{"issue", req.Issue}) {
		return
	}
	if !s.recordedMarkIssue(w, r, req.Issue, "take a jump mark off") {
		return
	}
	grant, ok := s.redeem(w, r, req.GrantID)
	if !ok {
		return
	}
	if !grant.Controller {
		writeFailure(w, http.StatusForbidden, "CONTROLLER_REQUIRED", "taking a jump mark off requires a controller grant")
		return
	}
	if err := pgx.BeginFunc(r.Context(), s.pool, func(tx pgx.Tx) error {
		_, err := s.records.ClearMergeJump(r.Context(), tx, req.Issue)
		return err
	}); err != nil {
		s.logRequestFailure(r, err, "api: clear a jump mark", "issue", req.Issue)
		writeFailure(w, http.StatusInternalServerError, "RECORD_UNAVAILABLE", fmt.Sprintf("could not take the jump mark of %s off", req.Issue))
		return
	}
	writeJSON(w, http.StatusOK, EmptyResponse{})
}

// mergeHolds replaces the project's whole merge hold list, and then makes every merge the project
// is still holding back due at once.
//
// The replacement and the expediting are one transaction because they are one decision: a
// controller that takes a hold off, or shortens one, has said that the merges it was keeping back
// may go now, and a ready_merge row deferred to the old hold's end would otherwise sit until that
// moment, which the record no longer holds. Every deferred row of the project is made due, not
// only the ones a hold caught: the run that follows reads the hold list again and holds back
// whatever the new list still catches, so the cost of making a row due early is one submission
// attempt, while the cost of leaving one behind is a merge that waits on a hold nobody set. The
// commit hooks are the route's own (intake.WithCommitHooks), as every other write of this package
// that wakes a lane is: the lane of each expedited row is woken when this transaction commits.
func (s *server) mergeHolds(w http.ResponseWriter, r *http.Request) {
	var req MergeHoldsRequest
	if !s.readBody(w, r, &req, s.redeemIdentityCheck) || !requireFailureFields(w, field{"grantId", req.GrantID}) {
		return
	}
	now := s.now().UTC()
	holds, ok := s.holdList(w, req.Holds, now)
	if !ok {
		return
	}
	grant, ok := s.redeem(w, r, req.GrantID)
	if !ok {
		return
	}
	if !grant.Controller {
		writeFailure(w, http.StatusForbidden, "CONTROLLER_REQUIRED", "the merge hold list requires a controller grant")
		return
	}
	session, ok := s.markSession(w, r)
	if !ok {
		return
	}
	for i := range holds {
		holds[i].Session, holds[i].SetAt = session, now
	}
	scoped, committed := intake.WithCommitHooks(r.Context())
	if err := pgx.BeginFunc(scoped, s.pool, func(tx pgx.Tx) error {
		if err := s.records.ReplaceMergeHolds(scoped, tx, s.dispatchProject, holds); err != nil {
			return err
		}
		deferred, err := s.records.DeferredReadyMerges(scoped, tx, s.dispatchProject, now)
		if err != nil {
			return err
		}
		for _, row := range deferred {
			if err := s.records.ExpediteOutbox(scoped, tx, row, now); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		s.logRequestFailure(r, err, "api: replace the merge holds", "project", s.dispatchProject, "holds", len(holds))
		writeFailure(w, http.StatusInternalServerError, "RECORD_UNAVAILABLE",
			fmt.Sprintf("could not replace the merge hold list of %s", s.dispatchProject))
		return
	}
	committed()
	writeJSON(w, http.StatusOK, EmptyResponse{})
}

// holdList is every value check the hold list gets, and the holds the record is to take. It runs
// before the grant is redeemed and writes nothing, so a list with one bad hold in it leaves the
// list that stands exactly as it was. Each refusal names the hold by its place in the list the
// caller sent, since a hold has no identifier the two sides share.
func (s *server) holdList(w http.ResponseWriter, holds []MergeHoldRequest, now time.Time) ([]record.MergeHold, bool) {
	taken := make([]record.MergeHold, 0, len(holds))
	for index, hold := range holds {
		where := fmt.Sprintf("hold %d of the list", index+1)
		if len(hold.Paths) == 0 {
			writeFailure(w, http.StatusBadRequest, "HOLD_WITHOUT_PATH", fmt.Sprintf(
				"%s names no path: a hold keeps back a merge whose pull request changes a file one of its paths matches, so a hold with no path keeps back nothing", where))
			return nil, false
		}
		for _, path := range hold.Paths {
			if strings.TrimSpace(path) == "" {
				writeFailure(w, http.StatusBadRequest, "HOLD_WITHOUT_PATH", fmt.Sprintf(
					"%s names an empty path, which matches every file of every pull request: name the paths the hold is for", where))
				return nil, false
			}
		}
		if strings.TrimSpace(hold.Reason) == "" {
			writeFailure(w, http.StatusBadRequest, "HOLD_WITHOUT_REASON", fmt.Sprintf(
				"%s gives no reason: a hold stops other people's merges, and the architect of a held issue is told why it waits", where))
			return nil, false
		}
		if !hold.EndsAt.After(now) {
			writeFailure(w, http.StatusBadRequest, "HOLD_ALREADY_ENDED", fmt.Sprintf(
				"%s ends at %s, which is not after %s, the time of this call: a hold that has ended already holds nothing back",
				where, hold.EndsAt.UTC().Format(time.RFC3339), now.Format(time.RFC3339)))
			return nil, false
		}
		for _, path := range hold.Paths {
			if _, err := regexp.Compile(path); err != nil {
				writeFailure(w, http.StatusBadRequest, "INVALID_HOLD_PATH", fmt.Sprintf(
					"the path %q of %s is no regular expression: %v", path, where, err))
				return nil, false
			}
		}
		repository, ok := s.heldRepository(w, hold.Repository, where)
		if !ok {
			return nil, false
		}
		taken = append(taken, record.MergeHold{
			Paths: hold.Paths, Repository: repository, Reason: hold.Reason, EndsAt: hold.EndsAt.UTC(),
		})
	}
	return taken, true
}

// heldRepository is the repository one hold names, as the record is to keep it: "" for a hold of
// every repository the project serves, and otherwise the name GitHub serves it under. A name this
// project does not serve is refused, and so is one written in any shape but `owner/name`, by the
// same code: both are a hold that would hold nothing, and the controller has to be told which of
// the project's repositories it meant either way. The comparison ignores case (ghrepo.Serves),
// because GitHub's own owner and repository names are not case-sensitive, so a hold of
// "Acme/Widgets" must hold "acme/widgets".
func (s *server) heldRepository(w http.ResponseWriter, named, where string) (string, bool) {
	if strings.TrimSpace(named) == "" {
		return "", true
	}
	refuse := func(why string) {
		writeFailure(w, http.StatusBadRequest, "REPOSITORY_NOT_SERVED", fmt.Sprintf(
			"%s names the repository %q, which %s: a hold names one of the repositories this project serves, or none at all, which holds its paths in every one of them",
			where, named, why))
	}
	parsed, err := ghrepo.Parse("the repository of a merge hold", named)
	if err != nil {
		refuse(err.Error())
		return "", false
	}
	var served []ghrepo.Repository
	if s.githubRepositories != nil {
		served = s.githubRepositories()
	}
	if !ghrepo.Serves(served, parsed) {
		refuse("this project serves no repository of that name")
		return "", false
	}
	return parsed.String(), true
}

// recordedMarkIssue refuses an issue the daemon does not record, naming it and what the caller
// asked for it. A mark governs a merge the daemon submits for the issue's own ready_merge row, so
// an issue it holds no record of has no merge to mark: the mark's row is bound to the issue's
// record in the first place (migration 20261110235352's `on delete cascade`), and the write would
// be refused by the database with nothing a controller could act on.
func (s *server) recordedMarkIssue(w http.ResponseWriter, r *http.Request, issue, asked string) bool {
	recorded, err := s.recordedIssue(r.Context(), issue)
	if err != nil {
		s.logRequestFailure(r, err, "api: read the issue a merge mark names", "issue", issue)
		writeFailure(w, http.StatusInternalServerError, "RECORD_UNAVAILABLE", fmt.Sprintf("could not read the record of %s", issue))
		return false
	}
	if recorded == nil {
		writeFailure(w, http.StatusNotFound, "ISSUE_NOT_RECORDED", fmt.Sprintf(
			"the daemon records no issue %s, so there is no merge of it to %s", issue, asked))
		return false
	}
	return true
}

// markSession is the controller session a mark records as its author (record.MergeJump.Session,
// record.MergeHold.Session). A redeemed controller grant names no session of its own — the grant
// record carries the claim of a worker's grant and nothing for a controller's
// (credential.Grant) — so the session is read from the project's controller record, which is the
// session the controller-session grant was minted for and authenticated against
// (controllerSessionGrant). A grant minted from the operator's bearer records that same
// registered controller, the project's one controller either way; a project whose controller has
// not registered records no session, as the record's own empty Session says.
func (s *server) markSession(w http.ResponseWriter, r *http.Request) (string, bool) {
	if s.controller == nil {
		return "", true
	}
	stored, found, err := s.controller.Controller(r.Context(), s.project)
	if err != nil {
		s.logRequestFailure(r, err, "api: read the controller a merge mark is recorded for", "project", s.project)
		writeFailure(w, http.StatusInternalServerError, "CONTROLLER_UNAVAILABLE", "could not read the project's controller")
		return "", false
	}
	if !found {
		return "", true
	}
	return stored.Session, true
}

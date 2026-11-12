package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// MergeJumpRequest sets one issue's jump mark: the controller's instruction that the daemon ask
// GitHub to put the next merge it submits for Issue at the head of GitHub's merge queue
// (record.MergeJump). Reason is what the controller says the jump is for, which the record of
// the jump keeps after the mark itself is spent, so the account of what Legion asked for names
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
//
// Holds is a pointer because an absent list and an empty one are different decisions and a plain
// slice spells them the same: clearing every hold of the project is a thing a controller says
// with `"holds": []`, and a body that says nothing about the list is a body that forgot it
// (HOLD_LIST_REQUIRED), not one asking for the holds to go.
type MergeHoldsRequest struct {
	GrantID string              `json:"grantId"`
	Holds   *[]MergeHoldRequest `json:"holds"`
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
//
// The issue is checked twice, and the second check is the write's own. runningMarkIssue reads
// the record in a transaction of its own and refuses a controller that asked about an issue
// already out of the workflow, without writing anything; the write then takes the issue's row
// lock and refuses again where the record it locks has stopped (record.Store.SetMergeJump),
// which is the departure that committed while this request was in flight. Both refusals are the
// one 409 ISSUE_LEFT_WORKFLOW: the caller asked to mark an issue the daemon submits no merge
// for, whichever moment it stopped being one.
func (s *server) mergeJump(w http.ResponseWriter, r *http.Request) {
	var req MergeJumpRequest
	if !s.readBody(w, r, &req, s.redeemIdentityCheck) || !requireFailureFields(w,
		field{"grantId", req.GrantID}, field{"issue", req.Issue}, field{"reason", req.Reason}) {
		return
	}
	recorded, ok := s.recordedMarkIssue(w, r, req.Issue, "mark for the jump")
	if !ok || !runningMarkIssue(w, *recorded) {
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
	var wrote bool
	if err := pgx.BeginFunc(r.Context(), s.pool, func(tx pgx.Tx) error {
		var err error
		wrote, err = s.records.SetMergeJump(r.Context(), tx, mark)
		return err
	}); err != nil {
		s.logRequestFailure(r, err, "api: set a jump mark", "issue", req.Issue)
		writeFailure(w, http.StatusInternalServerError, "RECORD_UNAVAILABLE", fmt.Sprintf("could not set the jump mark of %s", req.Issue))
		return
	}
	if !wrote {
		writeFailure(w, http.StatusConflict, "ISSUE_LEFT_WORKFLOW", fmt.Sprintf(
			"%s left the workflow while its mark was being written, so the daemon submits no merge of it to mark and nothing was written; a mark it still carries can be taken off",
			req.Issue))
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
	// The clear takes no runningMarkIssue: a mark on an issue that has left the workflow is
	// exactly what a controller must be able to take off (runningMarkIssue).
	if _, ok := s.recordedMarkIssue(w, r, req.Issue, "take a jump mark off"); !ok {
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

// mergeHolds replaces the project's whole merge hold list, and then makes every merge a hold of
// the project caught when its row last ran due at once.
//
// The replacement and the expediting are one transaction because they are one decision: a
// controller that takes a hold off, or shortens one, has said that the merges it was keeping back
// may go now, and a ready_merge row deferred to the old hold's end would otherwise sit until that
// moment, which the record no longer holds. A deferred row is deferred for one of two reasons,
// and only one of them is the controller's to undo: a row the daemon put off to the end of a
// hold, which its payload's heldSince marks and which this route makes due, and a row GitHub's
// own rate limit or the submission's backoff put off, which keeps its next_at because that
// deadline is GitHub's and not the hold's (record.Store.HeldReadyMerges). Making the held rows
// due early costs at most one submission attempt, since the run that follows reads the hold list
// again and holds back whatever the new list still catches. The commit hooks are the route's own
// (intake.WithCommitHooks), as every other write of this package that wakes a lane is: the lane
// of each expedited row is woken when this transaction commits.
//
// The transaction opens by taking the project's merge-hold lock
// (record.Store.SerializeMergeHolds), which the merge run's own decision to hold a submission
// back takes before it re-reads the list (daemon's readyMergeMarks.holdAndTell). Without it the
// scan below and that write interleave: a run that read the old list, then called GitHub for the
// pull request's changed files, writes its held moment after this scan has run, so this route
// never sees the row and the merge sleeps out the whole of a hold that no longer stands. Under
// the lock the two orders are the only two, and both end with the merge woken: the replacement
// first, and the run re-reads this list; the run first, and the scan below finds its row. The
// locks already held around this one, and why none of them can close a cycle with it, are
// written where it is taken.
func (s *server) mergeHolds(w http.ResponseWriter, r *http.Request) {
	var req MergeHoldsRequest
	if !s.readBody(w, r, &req, s.redeemIdentityCheck) || !requireFailureFields(w, field{"grantId", req.GrantID}) {
		return
	}
	if req.Holds == nil {
		writeFailure(w, http.StatusBadRequest, "HOLD_LIST_REQUIRED", fmt.Sprintf(
			"holds is required: this call replaces the whole merge hold list of %s, so a body that names no list says nothing about the holds that stand, and clearing every hold is an explicit empty list", s.dispatchProject))
		return
	}
	now := s.now().UTC()
	holds, ok := s.holdList(w, *req.Holds, now)
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
		if err := s.records.SerializeMergeHolds(scoped, tx, s.dispatchProject); err != nil {
			return err
		}
		if err := s.records.ReplaceMergeHolds(scoped, tx, s.dispatchProject, holds); err != nil {
			return err
		}
		held, err := s.records.HeldReadyMerges(scoped, tx, s.dispatchProject, now)
		if err != nil {
			return err
		}
		for _, row := range held {
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
//
// The repositories the project serves are read once for the whole list and handed to each hold's
// check: the read takes a lock and builds the served list anew every time it is called
// (server.githubRepositories), and a list of fifty holds has no more to learn from fifty reads
// of it than from one — the served set cannot change inside one request.
func (s *server) holdList(w http.ResponseWriter, holds []MergeHoldRequest, now time.Time) ([]record.MergeHold, bool) {
	var served []ghrepo.Repository
	if s.githubRepositories != nil {
		served = s.githubRepositories()
	}
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
		repository, ok := heldRepository(w, served, hold.Repository, where)
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
// every repository the project serves, and otherwise the name as the controller wrote it, in the
// `owner/name` shape ghrepo.Parse admits and in the controller's own spelling, which Parse keeps.
// Nothing downcases it, and nothing has to: the submission that asks whether a hold covers its
// repository compares the two without case (strings.EqualFold, the daemon's holding), as the
// check here does (ghrepo.Serves), because GitHub's own owner and repository names are not
// case-sensitive, so a hold of "Acme/Widgets" holds "acme/widgets".
//
// A name this project does not serve is refused, and so is one written in any shape but
// `owner/name`, by the same code: both are a hold that would hold nothing, and the controller
// has to be told which of the project's repositories it meant either way. served is the
// project's repositories, read once for the whole list by the caller.
func heldRepository(w http.ResponseWriter, served []ghrepo.Repository, named, where string) (string, bool) {
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
	if !ghrepo.Serves(served, parsed) {
		refuse("this project serves no repository of that name")
		return "", false
	}
	return parsed.String(), true
}

// recordedMarkIssue is the issue both jump-mark routes check before either writes anything, and
// answers its record, which the setting route reads once more (runningMarkIssue).
//
// An issue outside this daemon's own Dispatch project is refused first, ahead of the read of the
// record and so ahead of anything that could be written. The daemons of several projects share
// one database, and a mark is kept under the issue's key alone (record.Store.SetMergeJump), so a
// key of another project names a row that project's daemon spends on its next submission,
// against a decision its controller never made, and a clear takes off one that controller did
// make. The project is the key's own prefix, which is how the daemon reads a key's project
// everywhere else — this package's own ownDispatchIssue and the state's list of the project's
// marks (record.Store.MergeJumps, `split_part(issue, '-', 1)`), which would not even show such
// a row to the controller that set it. A server given no Dispatch project — a test's — takes
// every key as its own. The status is 403 and not 404: the caller is this project's controller
// holding a good grant, and what refuses it is that the issue is not this daemon's to act on at
// all, which is the refusal an architect reaching outside its tree meets (ISSUE_OUTSIDE_TREE).
//
// Then an issue the daemon does not record is refused, naming it and what the caller asked for
// it. A mark governs a merge the daemon submits for the issue's own ready_merge row, so an issue
// it holds no record of has no merge to mark: the mark's row is bound to the issue's record in
// the first place (migration 20261110235352's `on delete cascade`), and the write would be
// refused by the database with nothing a controller could act on.
func (s *server) recordedMarkIssue(w http.ResponseWriter, r *http.Request, issue, asked string) (*record.Issue, bool) {
	if !s.ownDispatchIssue(issue) {
		writeFailure(w, http.StatusForbidden, "ISSUE_OUTSIDE_PROJECT", fmt.Sprintf(
			"%s is not an issue of %s, the Dispatch project of this daemon, so there is no merge of it to %s",
			issue, s.dispatchProject, asked))
		return nil, false
	}
	recorded, err := s.recordedIssue(r.Context(), issue)
	if err != nil {
		s.logRequestFailure(r, err, "api: read the issue a merge mark names", "issue", issue)
		writeFailure(w, http.StatusInternalServerError, "RECORD_UNAVAILABLE", fmt.Sprintf("could not read the record of %s", issue))
		return nil, false
	}
	if recorded == nil {
		writeFailure(w, http.StatusNotFound, "ISSUE_NOT_RECORDED", fmt.Sprintf(
			"the daemon records no issue %s, so there is no merge of it to %s", issue, asked))
		return nil, false
	}
	return recorded, true
}

// runningMarkIssue refuses a mark on an issue the daemon no longer runs. Only the setting route
// calls it: taking a mark off such an issue is a thing a controller may always do, and is how a
// mark that outlived its issue goes, so mergeJumpClear works on every recorded issue of the
// project.
//
// It reads the record in a transaction of its own, so it answers a controller without writing
// anything and before the grant is spent; the same rule is read again, under the issue's row
// lock, in the transaction that writes the mark (record.Store.SetMergeJump), which is what
// catches a departure that commits while this request is in flight. This check is kept for what
// it is good at: it names the state the issue was found in, which the write's own refusal
// cannot.
//
// The states refused are the ones record.Stopped names, the daemon's one statement of work that
// has stopped, so that this refusal and the departure that clears a mark read one record the same
// way: a status out of the workflow — done, backlog, icebox or triage, the issue closed or parked
// — a phase of done, and a tree lingering after its close. Such an issue submits no merge, and
// the departure already cleared the mark it carried (workflow's leave, whose comment states the
// harm): a mark set after that would stand in the record until a re-admission brought the issue
// back, and "would be spent weeks later by whatever merge a re-admission of this issue reaches,
// against a decision nobody made about that merge".
//
// The status is 409 and not 400: the request is well formed and the caller is authorized, and
// what refuses it is the state the record is in, as the lingering tree of a rerun is
// (TREE_LINGERING).
//
// The sentence says the issue's state in Dispatch's own words — done, on the backlog, in icebox,
// in triage — rather than that it has left the workflow. ISSUE_LEFT_WORKFLOW is the repository's
// own phrase and stays the code, which a controller matches on; the words beside it are read by
// a person, and the two Dispatch has are the two a person knows.
func runningMarkIssue(w http.ResponseWriter, recorded record.Issue) bool {
	if !record.Stopped(recorded) {
		return true
	}
	writeFailure(w, http.StatusConflict, "ISSUE_LEFT_WORKFLOW", fmt.Sprintf(
		"%s is %s, so the daemon submits no merge of it to mark; a mark it still carries can be taken off",
		recorded.Key, markIssueState(recorded)))
	return false
}

// markIssueState is how the refusal above names an issue the daemon no longer runs, in the words
// its Dispatch status is written in. A tree lingering after its close is named by its close,
// since that is what a reader of the issue sees, and a status this build does not know is given
// as it stands rather than guessed at.
func markIssueState(recorded record.Issue) string {
	switch recorded.Status {
	case record.DoneStatus:
		return "done"
	case "backlog":
		return "on the backlog"
	case "icebox":
		return "in icebox"
	case "triage":
		return "in triage"
	}
	if recorded.Lingers() || recorded.Phase == phase.Done {
		return "done"
	}
	return "in " + recorded.Status
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

// MergeJumpView is one issue's live jump mark as the state shows it (record.MergeJump): the
// issue, what the controller said the jump is for, the controller session that set it and when.
// It carries no state of its own, because the mark has none: a mark in the record has not been
// spent, and one that was spent or taken off is gone (record.Store.ClearMergeJump).
type MergeJumpView struct {
	Issue   string    `json:"issue"`
	Reason  string    `json:"reason"`
	Session string    `json:"session"`
	SetAt   time.Time `json:"setAt"`
}

// MergeHoldView is one live merge hold as the state shows it (record.MergeHold): the unanchored
// regular expressions it matches against the paths a pull request changes, the repository it
// holds them in, what the controller said the hold is for, when it stops holding, and the
// controller session that set the list and when.
//
// Repository is `owner/name`, and "" is every repository the project serves. It is spelled
// `repository` and not `repo`, which the issue and pull request views use, because this view is
// read beside the route that sets it (MergeHoldRequest.Repository): a controller comparing what
// it sent with what the daemon holds reads one name for one thing.
type MergeHoldView struct {
	Paths      []string  `json:"paths"`
	Repository string    `json:"repository"`
	Reason     string    `json:"reason"`
	EndsAt     time.Time `json:"endsAt"`
	Session    string    `json:"session"`
	SetAt      time.Time `json:"setAt"`
}

// MarshalJSON keeps `paths` an array on the wire: a hold the record answers always names at
// least one path, and a nil slice would still be `null`, which the plugin's reader refuses.
func (h MergeHoldView) MarshalJSON() ([]byte, error) {
	type wire MergeHoldView
	out := wire(h)
	if out.Paths == nil {
		out.Paths = []string{}
	}
	return json.Marshal(out)
}

// MergeHeldView is one issue whose merge a hold held when its ready_merge row last ran
// (record.MergeHeldIssue): the issue, and when that row runs again, which is when the daemon
// reads the holds once more. The row keeps the moment a hold caught it until its next run finds
// no hold, so the hold that caught this row may have ended or been replaced since, and the view
// is the row's last reading rather than a hold that stands now. Nothing else of the row is here:
// the row's payload is the submission's own business, and a reader of this key is asking which
// merges the holds are costing and when each is tried next.
type MergeHeldView struct {
	Issue string    `json:"issue"`
	DueAt time.Time `json:"dueAt"`
}

// MergeQueueEntryView is one entry of GitHub's merge queue as it stood before a jump. It is
// record.MergeQueueEntryRecord: the stored snapshot and the served one are the same rows spelled
// the same way, and the record's own json tags are the wire's, so the type is declared once and
// read under both names.
type MergeQueueEntryView = record.MergeQueueEntryRecord

// MergeJumpRecordView is the newest jump of one repository as the state shows it
// (record.MergeJumpRecord): the repository, the issue whose mark was spent, the pull request's
// number and head, when the daemon asked, the reason the mark carried, the queue as it stood
// before the request — oldest entry first, which is what names whose merge the jump asked to go
// ahead of — how long that queue was, whether the daemon has no reading of that queue at all,
// and what became of the jump's own request, with the words that decided it.
//
// EntriesTotal and EntriesUnknown are both here because Entries alone cannot be read: it may
// hold less of the queue than the queue had, and an empty Entries is a queue of nothing only
// when EntriesUnknown is false. EntriesUnknown is the daemon having no reading of that queue:
// the read failed — GitHub refused it, it ran out of time, or its body would not read — or
// GitHub answered that the branch has no merge queue at all. A queue GitHub answered empty was
// read and found empty, so it leaves EntriesUnknown false.
//
// Outcome is one of the four record.MergeJumpOutcome values and is never empty, so no reader
// has to tell an outcome from a field nobody wrote: `asked` is the daemon having asked and not
// learned what came of it, which is what the record says from the moment it is written until
// GitHub answers; `accepted` is GitHub taking the request; `refused` is an answer that decided
// it; and `unknown` is an answer that decided nothing or a request nothing came back for, which
// Answer's own words tell apart. Answer carries GitHub's status and words, or the transport
// failure's text, for the last two, and is empty for the first two.
//
// None of them says where the merge stands, `accepted` least of all: it is the mutation
// answering and no place in a queue. Nor does Entries say it — that is the queue as the daemon
// read it before the request, and GitHub alone knows what became of it.
type MergeJumpRecordView struct {
	Repository     string                  `json:"repository"`
	Issue          string                  `json:"issue"`
	Number         int                     `json:"number"`
	Head           string                  `json:"head"`
	At             time.Time               `json:"at"`
	Reason         string                  `json:"reason"`
	Entries        []MergeQueueEntryView   `json:"entries"`
	EntriesTotal   int                     `json:"entriesTotal"`
	EntriesUnknown bool                    `json:"entriesUnknown"`
	Outcome        record.MergeJumpOutcome `json:"outcome"`
	Answer         string                  `json:"answer"`
}

// MarshalJSON keeps `entries` an array on the wire: a jump of an empty queue, and one whose
// queue GitHub did not answer (EntriesUnknown), both record no entry, and a nil slice would be
// `null`, which the plugin's reader refuses.
func (r MergeJumpRecordView) MarshalJSON() ([]byte, error) {
	type wire MergeJumpRecordView
	out := wire(r)
	if out.Entries == nil {
		out.Entries = []MergeQueueEntryView{}
	}
	return json.Marshal(out)
}

// MergeJumpViewsOf is jumps as the state shows them, mark for mark; no marks is an empty list.
func MergeJumpViewsOf(jumps []record.MergeJump) []MergeJumpView {
	views := make([]MergeJumpView, len(jumps))
	for i, jump := range jumps {
		views[i] = MergeJumpView{Issue: jump.Issue, Reason: jump.Reason, Session: jump.Session, SetAt: jump.SetAt}
	}
	return views
}

// MergeHoldViewsOf is holds as the state shows them, hold for hold, in the list's own order; no
// holds is an empty list.
func MergeHoldViewsOf(holds []record.MergeHold) []MergeHoldView {
	views := make([]MergeHoldView, len(holds))
	for i, hold := range holds {
		views[i] = MergeHoldView{
			Paths:      hold.Paths,
			Repository: hold.Repository,
			Reason:     hold.Reason,
			EndsAt:     hold.EndsAt,
			Session:    hold.Session,
			SetAt:      hold.SetAt,
		}
	}
	return views
}

// MergeHeldViewsOf is held as the state shows it, issue for issue; no held merge is an empty
// list.
func MergeHeldViewsOf(held []record.MergeHeldIssue) []MergeHeldView {
	views := make([]MergeHeldView, len(held))
	for i, issue := range held {
		views[i] = MergeHeldView{Issue: issue.Issue, DueAt: issue.DueAt}
	}
	return views
}

// MergeJumpRecordViewsOf is records as the state shows them, record for record; no jump is an
// empty list.
func MergeJumpRecordViewsOf(records []record.MergeJumpRecord) []MergeJumpRecordView {
	views := make([]MergeJumpRecordView, len(records))
	for i, jump := range records {
		views[i] = MergeJumpRecordView{
			Repository:     jump.Repository,
			Issue:          jump.Issue,
			Number:         jump.Number,
			Head:           jump.Head,
			At:             jump.At,
			Reason:         jump.Reason,
			Entries:        jump.Entries,
			EntriesTotal:   jump.EntriesTotal,
			EntriesUnknown: jump.EntriesUnknown,
			Outcome:        jump.Outcome,
			Answer:         jump.Answer,
		}
	}
	return views
}

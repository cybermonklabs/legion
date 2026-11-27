package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// results_retract.go retracts one recorded result. A retraction is a record of its own
// (check_result_retractions): it names the result, carries the reason and the actor, and
// updates and deletes nothing, so a reader sees the result and its retraction together. A
// retracted result is no evidence, and the record says who said so and why. The store holds it as
// the result node's retracted value, appended in the transaction that writes the row, of which the
// row is the projection (writeCheckResultRetraction); the node itself never changes.
//
// It is taken on a closed issue, and for a check of any `when`, which is what the record route
// refuses (ISSUE_CLOSED in results.go). A closed issue's results are the production record,
// and a false one there is exactly the one that must be correctable.

// checkResultRetractionInput is the body POST /api/v1/results/{id}/retract takes.
type checkResultRetractionInput struct {
	Actor  *model.Actor `json:"actor"`
	Reason string       `json:"reason" required:"true"`
}

// retractedResult is what the write reads about the result it is asked to retract: the issue
// the result was recorded on, whose claim is one of the three credentials that may retract it,
// the check's issue, which the event is appended on and whose owner lock the route takes before
// it reads who may retract the result and whether a retraction stands, and the session that
// recorded it.
type retractedResult struct {
	IssueKey      string
	CheckIssueKey string
	CheckBlockID  string
	CommitSHA     string
	Session       string
}

// POST /api/v1/results/{id}/retract  (the recording session, the claimant, or a person)
//
// Records that one stored result is wrong, with the reason. Nothing about the result changes:
// the retraction is a row of its own, and both records stay readable. The answer is the
// retraction.
func (s *server) retractCheckResult(w http.ResponseWriter, r *http.Request) {
	// The one project the credential names, empty for a credential that names none, read before
	// the transaction opens, as the person scope below is. The check's issue, which the retraction
	// is appended to, can be of another project than the result's own, reached through a parent of
	// another project. The pre-handler check asked about both before any other layer ran
	// (withinProjectScope, projectOfRetractedCheck), so a check of a private project that the
	// credential does not name got the answer of a result that does not exist, whatever the
	// request holds. The transaction asks again below, once it has read the result and before it
	// takes the owner lock of the check's issue (refuseRetractionOfUncoveredCheck).
	callerProject, _ := s.callerProject(r)
	var input checkResultRetractionInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	actor, ok := s.requireActor(w, r, input.Actor)
	if !ok {
		return
	}
	reason := strings.TrimSpace(input.Reason)
	if reason == "" {
		writeError(w, "RETRACTION_INPUT", http.StatusBadRequest,
			"reason says why the result is wrong, and the retraction keeps it for every later reader")
		return
	}
	if !requireUUIDPath(w, r, "result") {
		return
	}
	resultID := r.PathValue("id")
	// Asked before the transaction opens, because a cookie caller's credential is read through
	// the pool (identity), and a caller that already holds a connection of the pool is refused
	// a second one (store/pool.go): inside the transaction every person would read as holding
	// no scope, which is the refusal this route must not make.
	person := s.callerHoldsScope(r, model.ScopePerson)
	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())

	// The read phase holds no transaction id (INFRA-636). The result's row is read plainly, for
	// the owner it names: nothing ever updates a check_results row, and its one writer, the
	// record route, inserts it under that same owner's lock (results.go, lockIssueOwners), so
	// the row read here holds still. The owner's lock comes after the check of the owner's
	// project below, and it is the lock the event's append would otherwise be the first to take
	// (store.LockOwner, events' lockEventOwner): advisory, so this route waits for it holding no
	// transaction id, where a `select ... for update` of the result taken first would have held
	// one across the wait and across every read below it.
	//
	// Who may retract the result, and whether a retraction of it stands already, are read in the
	// statements after that lock, each taking a fresh snapshot: a second retraction of this
	// result, whose only writer is this route under this lock, reads the first one as soon as
	// that one commits.
	result, err := readRetractedResult(r.Context(), tx, resultID)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// The check of the check's issue's project again, in the transaction that appends the
	// retraction to that issue, before it takes that issue's owner lock.
	if err := refuseRetractionOfUncoveredCheck(r.Context(), tx, callerProject, result.CheckIssueKey); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := store.LockOwner(r.Context(), tx, store.IssueOwner(result.CheckIssueKey)); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// The retraction moves the record of the check's issue's Verification query. This transaction
	// writes no version of that query and takes no lock of the issue's room: its batch marks the
	// issue's queries owed (writeCheckResultRetraction), and the drain writes the version once it
	// has committed, below.
	if err := s.refuseRetractionBy(r.Context(), tx, actor, person, result); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	retracted, err := resultIsRetracted(r.Context(), tx, resultID)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if retracted {
		writeError(w, "RESULT_RETRACTED", http.StatusConflict, fmt.Sprintf(
			"result %s was retracted already. Read the retraction that stands, and record a new result for the check.",
			resultID))
		return
	}
	held, createdAt, err := resultNodeHeld(r.Context(), tx, resultID)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}

	// The write phase: the event, then the retraction's row and its retracted value in one batch,
	// and the COMMIT behind them. The event is appended inside the transaction and published after
	// the commit, as the record route appends result.recorded (results.go): the Verification tab
	// refetches on it, and a reader woken before the row committed would read the result as
	// evidence still.
	event, err := s.appendEvent(r.Context(), tx, issueOwner(result.CheckIssueKey).event(
		"result.retracted", actor, model.CheckResultRetractedEventPayload{
			ResultID: resultID, Issue: result.IssueKey, CheckIssue: result.CheckIssueKey,
			CheckBlockID: result.CheckBlockID, CommitSHA: result.CommitSHA, Reason: reason,
		}))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	seq := int64(event.Seq)
	retraction := model.CheckResultRetraction{
		ResultID: resultID, Reason: reason, Session: sessionOf(actor), Actor: actor, EventSeq: &seq, CreatedAt: createdAt,
	}
	marked, err := writeCheckResultRetraction(r.Context(), tx, &retraction, result.CheckIssueKey, held, event.ID)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// The drain writes the version of the Verification query the retraction left owed, before the
	// retraction's event is published (docs.Service.DrainOwedQueries), and what it appended
	// follows that event.
	var drained []model.Event
	if marked {
		drained = s.deps.Docs.DrainOwedQueries(r.Context(), result.CheckIssueKey)
	}
	s.publish(r.Context(), append([]model.Event{event}, drained...)...)
	WriteJSON(w, http.StatusCreated, retraction)
}

// refuseRetractionBy refuses every caller but the three a retraction belongs to:
//
//   - the session that recorded the result, which check_results.session names;
//   - a session holding the Dispatch claim of the issue the result was recorded on, which is
//     issue_key and not check_issue_key: the recording issue is the one whose session wrote the
//     result, and its claimant answers for what that session recorded;
//   - a caller whose credential carries a person, which is a personal token or a signed-in
//     person's own session. That is the model.ScopePerson credential `dispatch whoami` prints
//     an owner for, and the deployment's shared token holds no scope at all.
//
// person is whether the request's own credential holds model.ScopePerson, which the handler
// read before it opened its transaction. It is read off the credential (callerHoldsScope) and
// not off actor: actorFrom drops the credential from the actor a handler records, and
// s.actingPerson writes a refusal of its own, which would answer a session this route admits.
func (s *server) refuseRetractionBy(
	ctx context.Context, q queryer, actor model.Actor, person bool, result retractedResult,
) error {
	session := ""
	if actor.Kind == "session" {
		session = actor.ID
	}
	if session != "" && session == result.Session {
		return nil
	}
	if person {
		return nil
	}
	holder, err := claimingSession(ctx, q, result.IssueKey)
	if err != nil {
		return err
	}
	if session != "" && session == holder {
		return nil
	}
	return errorf(http.StatusForbidden, "RETRACTION_FORBIDDEN",
		"three callers retract this result: the session that recorded it, the session holding the Dispatch claim of %s, and a person. Take that claim, or send the request with a personal token.",
		result.IssueKey)
}

// claimingSession is the session that holds one issue's Dispatch claim, and "" where no session
// holds it: an unclaimed issue, and one a person holds, which is no session to compare a caller
// with.
func claimingSession(ctx context.Context, q queryer, key string) (string, error) {
	var holder string
	if err := q.QueryRow(ctx, `
		select coalesce(case when claimed_by ->> 'kind' = 'session' then claimed_by ->> 'id' end, '')
		from issues where key = $1
	`, key).Scan(&holder); err != nil {
		return "", fmt.Errorf("read the claim of %s: %w", key, err)
	}
	return holder, nil
}

// readRetractedResult reads the result a retraction names, before the route takes the owner's
// lock: the result names that owner (check_issue_key). The route's one other read before that
// lock is the check of the owner's project for a credential that names one
// (refuseRetractionOfUncoveredCheck). This read takes no row lock, so it leaves the transaction
// without a transaction id for the wait on that lock. A row lock here would serialise nothing the
// owner's lock does not: check_results is written once, by the record route under the check
// issue's owner lock, and two retractions of one result are taken one after the other because
// both hold that lock while they read and write (retractCheckResult).
func readRetractedResult(ctx context.Context, tx pgx.Tx, id string) (retractedResult, error) {
	var result retractedResult
	err := tx.QueryRow(ctx, `
		select issue_key, check_issue_key, check_block_id, commit_sha, session
		from check_results where id = $1
	`, id).Scan(&result.IssueKey, &result.CheckIssueKey, &result.CheckBlockID,
		&result.CommitSHA, &result.Session)
	if errors.Is(err, pgx.ErrNoRows) {
		return retractedResult{}, errorf(http.StatusNotFound, "RESULT_NOT_FOUND", "result not found")
	}
	if err != nil {
		return retractedResult{}, fmt.Errorf("read result %s: %w", id, err)
	}
	return result, nil
}

// refuseRetractionOfUncoveredCheck asks again, in the transaction that appends a retraction to
// the result's check's issue, checkIssue, whether the request's credential covers that issue's
// project, with the visibility of that project read in the same statement (checkIssueProject).
// caller is the one project the credential names, empty for a credential that names none, which
// is refused nothing here. retractCheckResult asks it before it takes the owner lock of
// checkIssue. The pre-handler check refused a check of a private project that the credential
// does not name already (withinProjectScope, projectOfRetractedCheck), so this one refuses it
// only where its project became private between the two reads, with the same answer: the answer
// of a result that does not exist (outsideProjectScopeError).
func refuseRetractionOfUncoveredCheck(ctx context.Context, q queryer, caller, checkIssue string) error {
	if caller == "" {
		return nil
	}
	project, private, err := checkIssueProject(ctx, q, caller, checkIssue)
	if err != nil {
		return err
	}
	if !access.CredentialCovers(access.Credential{Listed: []string{caller}}, project, private) {
		return outsideProjectScopeError(caller)
	}
	return nil
}

// projectOfRetractedCheck reads, in one statement, the project of the check's issue of the result
// the path names, and whether that project is private: the project of the issue that a retraction
// of the result is appended to. An empty project with no error means no result has that id
// (scanProjectKey). withinProjectScope asks it of the retraction route beside the result's own
// issue (secondProjectResolverFor).
func projectOfRetractedCheck(ctx context.Context, q queryer, r *http.Request) (string, bool, error) {
	return scanProjectKey(q.QueryRow(ctx, `
		select i.project_key, p.visibility = 'private'
		from check_results c
		join issues i on i.key = c.check_issue_key
		join projects p on p.key = i.project_key
		where c.id = $1
	`, r.PathValue("id")))
}

// resultIsRetracted answers whether a retraction of this result stands already.
func resultIsRetracted(ctx context.Context, q queryer, id string) (bool, error) {
	var retracted bool
	err := q.QueryRow(ctx, `select true from check_result_retractions where result_id = $1`, id).Scan(&retracted)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read the retraction of result %s: %w", id, err)
	}
	return true, nil
}

// loadCheckResultRetractions is the retraction standing against each result of one
// Verification document's checks, by result id. One query serves the whole document, as
// loadCheckResults does for the results themselves: a document holding twenty checks is one
// round trip, and a retraction is read once per read of the results rather than once per
// result.
//
// A reader of the results sees a retracted result with the reason it is wrong and the actor
// who said so, and counts it as no evidence: no pass of a retracted result stands
// (results_comparison.go, passStandsOf).
func loadCheckResultRetractions(
	ctx context.Context, q queryer, artifactID string,
) (map[string]model.CheckResultRetracted, error) {
	// A document of an issue room keys its results by the room's tree artifact, so they are read
	// through its room, as loadCheckResults reads them (refs.CheckResultsOfDocumentSQL).
	rows, err := q.Query(ctx, `
		select retraction.result_id, retraction.reason, retraction.actor, retraction.created_at
		from check_result_retractions retraction
		join check_results result on result.id = retraction.result_id
		where `+refs.CheckResultsOfDocumentSQL("result", "$1::uuid")+`
	`, artifactID)
	if err != nil {
		return nil, fmt.Errorf("read the retractions of document %s: %w", artifactID, err)
	}
	defer rows.Close()
	retractions := map[string]model.CheckResultRetracted{}
	for rows.Next() {
		var id string
		var retraction model.CheckResultRetracted
		var actorJSON []byte
		if err := rows.Scan(&id, &retraction.Reason, &actorJSON, &retraction.CreatedAt); err != nil {
			return nil, fmt.Errorf("read the retractions of document %s: %w", artifactID, err)
		}
		if err := json.Unmarshal(actorJSON, &retraction.Actor); err != nil {
			return nil, fmt.Errorf("decode a retraction's actor: %w", err)
		}
		retractions[id] = retraction
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the retractions of document %s: %w", artifactID, err)
	}
	return retractions, nil
}

// resultNodeHeld reads, in the retract route's read phase and under the check issue's owner lock,
// whether the store holds the node of result id and the transaction's now(), the created_at the
// retraction row's column default would give it, in one statement. The backfill writes a result's
// node and its retracted value under that same owner lock (graphcensus' lockResultOwners), so a
// node it wrote before the lock was taken is one this read finds, and one it writes later reads
// the retraction's row this transaction commits.
func resultNodeHeld(ctx context.Context, tx pgx.Tx, id string) (bool, time.Time, error) {
	var held bool
	var at time.Time
	if err := tx.QueryRow(ctx, `select exists(select 1 from graph_nodes where kind = $1 and id = $2), now()`,
		graphstore.KindResult, id).Scan(&held, &at); err != nil {
		return false, time.Time{}, fmt.Errorf("read the node of result %s: %w", id, err)
	}
	return held, at, nil
}

// writeCheckResultRetraction writes the retraction, after the result.retracted event whose id is
// event, in one batch (store.RunWrite), its write phase's last: its row, dated at the retraction's
// created_at, the transaction's now() (resultNodeHeld), and, where the store holds the result's
// node (held), the retracted value of that result (graphstore.ResultRetracted): its reason, stated
// by the retraction's actor at that time and caused by event, of which the row is the projection;
// then the mark of the queries of the check's issue checkIssueKey owed a version, whose record the
// value moved (docs.QueueQueryMark), with that event and the value's sequence as its causes, the
// value's writer, the retraction's actor, being the one the drain names its version after, and
// last the result's head rows. A result whose node the store does not hold yet - a row an earlier
// release wrote, which the backfill has not reached - takes no value here, and so marks nothing:
// the backfill writes its node and then its retracted value from the row (graphcensus'
// census_results.go). It answers whether the batch marked the issue's queries, which it does only
// where the issue has converted.
func writeCheckResultRetraction(ctx context.Context, tx pgx.Tx, retraction *model.CheckResultRetraction, checkIssueKey string, held bool, event int64) (bool, error) {
	actorJSON, err := encodeJSON(retraction.Actor)
	if err != nil {
		return false, err
	}
	marked := false
	err = store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
		b.Queue("record the retraction", `
			insert into check_result_retractions (result_id, reason, session, actor, event_seq, created_at)
			values ($1, $2, $3, $4, $5, $6)
		`, retraction.ResultID, retraction.Reason, retraction.Session, actorJSON, retraction.EventSeq, retraction.CreatedAt)
		if !held {
			return nil
		}
		retracted := []graphstore.Tuple{graphstore.ResultRetracted(
			retraction.ResultID, retraction.Reason, retraction.Actor, retraction.CreatedAt, event)}
		graphstore.QueueWrite(b, nil, retracted)
		docs.QueueQueryMark(b, checkIssueKey, event, &marked)
		graphstore.QueueHeads(b, graphstore.HeadsOf(nil, retracted))
		return nil
	})
	return marked, err
}

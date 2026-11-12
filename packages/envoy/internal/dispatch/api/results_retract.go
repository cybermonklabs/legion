package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// results_retract.go retracts one recorded result. A retraction is a record of its own
// (check_result_retractions): it names the result, carries the reason and the actor, and
// updates and deletes nothing, so a reader sees the result and its retraction together. A
// retracted result is no evidence, and the record says who said so and why.
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
// the check's issue, which the event is appended on, and the session that recorded it.
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
	var input checkResultRetractionInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, r, err)
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
		s.writeHandlerError(w, r, err)
		return
	}
	defer tx.Rollback(r.Context())

	// The result's row is locked before anything is judged, and the retraction is looked for in
	// the statement after that lock. In READ COMMITTED each statement takes its own snapshot, so
	// a second retraction that waited here reads the first one as soon as that one commits. No
	// writer of a result ever holds this lock — nothing updates a check_results row — and the
	// record route takes its issue rows before it inserts, so the two cannot deadlock.
	result, err := lockRetractedResult(r.Context(), tx, resultID)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	if err := s.refuseRetractionBy(r.Context(), tx, actor, person, result); err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	retracted, err := resultIsRetracted(r.Context(), tx, resultID)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	if retracted {
		writeError(w, "RESULT_RETRACTED", http.StatusConflict, fmt.Sprintf(
			"result %s was retracted already. Read the retraction that stands, and record a new result for the check.",
			resultID))
		return
	}

	// The event is appended inside the transaction and published after the commit, as the record
	// route appends result.recorded (results.go): the Verification tab refetches on it, and a
	// reader woken before the row committed would read the result as evidence still.
	event, err := s.appendEvent(r.Context(), tx, issueOwner(result.CheckIssueKey).event(
		"result.retracted", actor, model.CheckResultRetractedEventPayload{
			ResultID: resultID, Issue: result.IssueKey, CheckIssue: result.CheckIssueKey,
			CheckBlockID: result.CheckBlockID, CommitSHA: result.CommitSHA, Reason: reason,
		}))
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	seq := int64(event.Seq)
	retraction := model.CheckResultRetraction{
		ResultID: resultID, Reason: reason, Session: sessionOf(actor), Actor: actor, EventSeq: &seq,
	}
	if err := insertCheckResultRetraction(r.Context(), tx, &retraction); err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	s.publish(event)
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

// lockRetractedResult reads the result a retraction names and locks its row for the length of
// the write, so two retractions of one result are taken one after the other.
func lockRetractedResult(ctx context.Context, tx pgx.Tx, id string) (retractedResult, error) {
	var result retractedResult
	err := tx.QueryRow(ctx, `
		select issue_key, check_issue_key, check_block_id, commit_sha, session
		from check_results where id = $1
		for update
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
	rows, err := q.Query(ctx, `
		select retraction.result_id, retraction.reason, retraction.actor, retraction.created_at
		from check_result_retractions retraction
		join check_results result on result.id = retraction.result_id
		where result.check_artifact_id = $1::uuid
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

// insertCheckResultRetraction writes the retraction's row and fills in the created_at the row
// takes, which the route answers with: the pointer is the caller's own value, not a copy of it.
func insertCheckResultRetraction(ctx context.Context, tx pgx.Tx, retraction *model.CheckResultRetraction) error {
	actorJSON, err := encodeJSON(retraction.Actor)
	if err != nil {
		return err
	}
	return tx.QueryRow(ctx, `
		insert into check_result_retractions (result_id, reason, session, actor, event_seq)
		values ($1, $2, $3, $4, $5)
		returning created_at
	`, retraction.ResultID, retraction.Reason, retraction.Session, actorJSON, retraction.EventSeq,
	).Scan(&retraction.CreatedAt)
}

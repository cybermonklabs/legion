package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/credential"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
)

// setDispatchStatus writes an issue's status in Dispatch as the daemon, answering a refused or
// failed write 502, and returns the issue as Dispatch answered the write
// (dispatch.Client.SetStatus), which applyOwnStatusWrite applies to the record.
func (s *server) setDispatchStatus(w http.ResponseWriter, r *http.Request, issue, status string) (dispatch.Issue, bool) {
	if s.dispatch == nil {
		s.writeFailure(w, r, http.StatusInternalServerError, "DISPATCH_UNAVAILABLE", "Dispatch is unavailable")
		return dispatch.Issue{}, false
	}
	written, err := s.dispatch.SetStatus(r.Context(), issue, status)
	var dispatchError *dispatch.Error
	switch {
	case errors.As(err, &dispatchError):
		s.writeFailure(w, r, http.StatusBadGateway, dispatchError.Code, dispatchError.Message)
		return dispatch.Issue{}, false
	case err != nil:
		s.writeFailure(w, r, http.StatusBadGateway, "DISPATCH_FAILED", "Dispatch status update failed")
		return dispatch.Issue{}, false
	}
	return written, true
}

// applyOwnStatusWrite applies the daemon's own status write to the record in the request that made
// it, as the write's event applies it when the Dispatch consumer delivers it. Without it the record
// moved only on that event, so every route that writes a status waited for the consumer: a consumer
// running half an hour behind left a released child unrecorded, and its tree's architect waiting on
// it, for that half hour.
//
// written is the issue as Dispatch answered the write, the state the write's event carries, at that
// event's sequence (intake.OwnStatusWrite). It applies through the fact path every event takes, under
// the fact lock and with the same handlers, as Dispatch's view of the issue that settles under the
// lock (intake.DispatchIssue.Observed): an issue the workflow still does not record takes the view,
// as the consumer prepares an unrecorded issue's event; one it records takes the event itself, with
// the daemon's own actor. Either way the record stands at the write's sequence, so the event, when
// the consumer delivers it, is at or below the record's sequence and changes nothing, in the
// workflow, in admission and in the Dispatch mirror alike. The event is a status move
// (intake.DispatchIssue.StatusMove): it lands ahead of events the consumer has not delivered, so the
// workflow takes its status even where the record last saw Dispatch at it. A Dispatch that
// answered no sequence gives the event no fence to find, so nothing applies here and the event
// applies the write, as before. Nor does an issue of another Dispatch project, which the
// controller's set_status can name: the consumer drops every event of another project (intake's
// decodeDispatchFact), so the record never takes one, and neither does this.
//
// Once the write has landed, a failure here leaves the record to the event, and the caller is
// answered so (recordOwnStatusWrite says which failure). A route that writes the status again on
// its retry gets the issue back at the sequence of that write's own event, and applies it then.
// requester and person are who asked the route for the write (ownWriteAsker).
func (s *server) applyOwnStatusWrite(w http.ResponseWriter, r *http.Request, written dispatch.Issue, requester claim.Role, person string) bool {
	if failure := s.recordOwnStatusWrite(r.Context(), written, requester, person); failure != nil {
		s.writeFailure(w, r, failure.status, failure.code, failure.message)
		return false
	}
	return true
}

// requesterPerson is who asked where a person did, the operator among them: the request it
// writes is a person's, which only that person ends with no pull request.
const requesterPerson claim.Role = "person"

// operatorPerson is the person a write through the operator's grant names (PersonActor): the
// operator's bearer names no one, so the write names the operator.
const operatorPerson = "operator"

// ownWriteAsker is who asked set_status for its write, by the grant it redeemed: a person, named
// operatorPerson, for the operator's grant (`legion issue status`, credential.Grant.Operator), and
// the controller for a controller session's.
func ownWriteAsker(grant credential.Grant) (claim.Role, string) {
	if grant.Operator {
		return requesterPerson, operatorPerson
	}
	return claim.RoleController, ""
}

// ownWriteFailure is why the record did not take the daemon's own status write in the request that
// made it, as a route answers it.
type ownWriteFailure struct {
	status        int
	code, message string
}

// recordOwnStatusWrite is applyOwnStatusWrite's work, answering nothing, for a route that does not
// fail its call when the record does not take a write that landed: release_children, whose write's
// event records the child when the consumer delivers it. It reports why the record did not take the
// write, or nil when it did or when there was nothing to apply. The write's event says who asked
// (intake.OwnStatusWrite): requester, and person where a person asked through the operator's grant.
func (s *server) recordOwnStatusWrite(ctx context.Context, written dispatch.Issue, requester claim.Role, person string) *ownWriteFailure {
	if written.LastSeq <= 0 || !s.ownDispatchIssue(written.Key) {
		return nil
	}
	event := intake.OwnStatusWrite(written, requester, person)
	fact := intake.ObservedIssue(written)
	fact.Delivered = &event
	if s.prepare != nil {
		prepared, err := s.prepare(ctx, fact)
		var later *intake.RetryLater
		switch {
		case errors.As(err, &later):
			return &ownWriteFailure{http.StatusConflict, "STATUS_APPLY_BACKOFF", fmt.Sprintf("%s: Dispatch shows %s; %v; retry after %s", written.Key, written.Status, err, later.After)}
		case err != nil:
			return &ownWriteFailure{http.StatusBadGateway, "REPOSITORY_READ_FAILED", fmt.Sprintf("Dispatch shows %s %s, but its repository could not be read: %v; retry", written.Key, written.Status, err)}
		}
		fact = prepared
	}
	result, err := s.apply(ctx, "api", requestFactID("status/own-write"), fact)
	if err != nil {
		return &ownWriteFailure{http.StatusInternalServerError, "FACT_APPLY_FAILED", fmt.Sprintf("Dispatch shows %s %s, but the record did not take it (%v); retry", written.Key, written.Status, err)}
	}
	if result.Refusal != nil {
		return &ownWriteFailure{result.Refusal.Status, result.Refusal.Code, result.Refusal.Message}
	}
	return nil
}

// ownDispatchIssue says whether key is an issue of the daemon's own Dispatch project, by its key's
// project as intake's decodeDispatchFact reads it. A server given no Dispatch project (a test's)
// takes every key as its own.
func (s *server) ownDispatchIssue(key string) bool {
	project, _, _ := strings.Cut(key, "-")
	return s.dispatchProject == "" || project == s.dispatchProject
}

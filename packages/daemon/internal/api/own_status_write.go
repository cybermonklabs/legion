package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
)

// OwnStatusWrite is what applies the daemon's own status write to its record once Dispatch has
// answered it (ApplyOwnStatusWrite): the daemon's Dispatch project, whose events the consumer
// takes; the preparation an issue's event takes before it is applied (Options.PrepareRepository,
// nil for none); and the application of a fact through every handler (intake.ApplyFact with the
// daemon's handlers).
type OwnStatusWrite struct {
	DispatchProject string
	Prepare         func(ctx context.Context, issue intake.DispatchIssue) (intake.DispatchIssue, error)
	Apply           func(ctx context.Context, eventID string, fact intake.Fact) (intake.Result, error)
}

// OwnWriteFailure is why the record did not take the daemon's own status write: the status, code
// and message a caller that answers it answers.
type OwnWriteFailure struct {
	Status        int
	Code, Message string
}

func (f *OwnWriteFailure) Error() string { return f.Code + ": " + f.Message }

// ApplyOwnStatusWrite applies the daemon's own status write to the record once Dispatch has
// answered it, as the write's event applies it when the Dispatch consumer delivers it. Every status
// write of the daemon's is queued as a status row (decision 5 of dispatch://INFRA-4730), and the
// outbox's status executor applies Dispatch's answer to a write of an issue the daemon does not
// record through this one function: an issue the controller's set_status took to todo, carrying
// the legion label, is recorded and stands in the waiting line before its event arrives (R19's
// handover), and an issue a status call wrote todo under a parent at work is recorded under it.
// The status a write of a recorded issue gives was counted on its record when the write was queued
// (R16 of dispatch://INFRA-3320), and its echo moves only the record's view of Dispatch, the
// workflow keeping the counted status over an earlier write's echo while a later one is queued.
//
// written is the issue as Dispatch answered the write, the state the write's event carries, at that
// event's sequence (intake.OwnStatusWrite). It applies through the fact path every event takes,
// under the fact lock and with the same handlers, as Dispatch's view of the issue that settles
// under the lock (intake.DispatchIssue.Observed): an issue the workflow still does not record takes
// the view, as the consumer prepares an unrecorded issue's event, admission among the handlers;
// one it records takes the event itself, with the daemon's own actor. Either way the record stands
// at the write's sequence, so the event, when the consumer delivers it, is at or below the record's
// sequence and changes nothing, in the workflow, in admission and in the Dispatch mirror alike. A
// Dispatch that answered no sequence gives the event no fence to find, so nothing applies here and
// the event applies the write. Nor does an issue of another Dispatch project, which the
// controller's set_status can name: the consumer drops every event of another project (intake's
// decodeDispatchFact), so the record never takes one, and neither does this.
//
// It answers why the record did not take the write, nil where it did or where there was nothing to
// apply: the write has landed, so the record is left to the write's event.
func ApplyOwnStatusWrite(ctx context.Context, own OwnStatusWrite, eventID string, written dispatch.Issue) *OwnWriteFailure {
	if written.LastSeq <= 0 || own.DispatchProject != "" && claim.KeyProject(written.Key) != own.DispatchProject {
		return nil
	}
	event := intake.OwnStatusWrite(written)
	fact := intake.ObservedIssue(written)
	fact.Delivered = &event
	if own.Prepare != nil {
		prepared, err := own.Prepare(ctx, fact)
		var later *intake.RetryLater
		switch {
		case errors.As(err, &later):
			return &OwnWriteFailure{http.StatusConflict, "STATUS_APPLY_BACKOFF", fmt.Sprintf("%s: Dispatch shows %s; %v; retry after %s", written.Key, written.Status, err, later.After)}
		case err != nil:
			return &OwnWriteFailure{http.StatusBadGateway, "REPOSITORY_READ_FAILED", fmt.Sprintf("Dispatch shows %s %s, but its repository could not be read: %v; retry", written.Key, written.Status, err)}
		}
		fact = prepared
	}
	result, err := own.Apply(ctx, eventID, fact)
	if err != nil {
		return &OwnWriteFailure{http.StatusInternalServerError, "FACT_APPLY_FAILED", fmt.Sprintf("Dispatch shows %s %s, but the record did not take it (%v); retry", written.Key, written.Status, err)}
	}
	if result.Refusal != nil {
		return &OwnWriteFailure{result.Refusal.Status, result.Refusal.Code, result.Refusal.Message}
	}
	return nil
}

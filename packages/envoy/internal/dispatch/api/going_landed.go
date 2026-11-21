package api

// going_landed.go is how a request the command bar sends in-process records that it landed. A
// request has landed when its handler's transaction committed the change it made, whatever status
// its answer then carried: a route may commit and then answer 5xx (createStoredMessage commits a
// message and then fails to read its deliveries), and an answer may be lost after its commit with
// the process that was to read it. So no answer is taken for a landing. The record is written by
// the request itself: each request of a run's call (goingRunLanding) or of a run's undo
// (goingUndoLanding) carries its landing on its context (goingLandingKey), and goingInProcess.Do
// hands its route the commit step that writes the request's going_landings row, inside the
// route's own transaction, so the change and the record of its landing commit together or not at
// all. A route that answers what its transaction found and left hands that answer to the landing
// before its commit (goingRecordAnswer), which records it beside the landing in the same
// statement, so a call whose process ended after it landed is read from the values of the change
// itself, never from a read made after another writer may have moved the item.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync/atomic"
)

// goingLandingKey carries a request's landing (goingLandingStep) on the context of the request it
// belongs to.
type goingLandingKey struct{}

// goingAnswerKey carries the goingAnswerSink of a request the command bar sends in-process with a
// landing (goingInProcess.Do), for its route to fill (goingRecordAnswer).
type goingAnswerKey struct{}

// goingAnswerSink is what a request's route hands its landing before its commit: the status it is
// about to answer and the value it answers with, which its landing records beside it, and the
// delivery recovery point of a write that delivers to sessions (goingRecordRecovery), which a run
// call's landing records beside it (goingRunLanding). A route's detached work may commit beside
// the request, so each is read and written whole, atomically.
type goingAnswerSink struct {
	answer   atomic.Pointer[goingSunkAnswer]
	recovery atomic.Pointer[json.RawMessage]
}

// recorded is the status and the encoded value the route handed the sink (goingRecordAnswer),
// built now; recorded is false where it handed none, or handed a value that does not encode,
// which is logged.
func (s *goingAnswerSink) recorded() (status int, body json.RawMessage, recorded bool) {
	sunk := s.answer.Load()
	if sunk == nil {
		return 0, nil, false
	}
	body, err := json.Marshal(sunk.answer())
	if err != nil {
		slog.Warn("dispatch: hand a going request's landing what its route answers", "error", err)
		return 0, nil, false
	}
	return sunk.status, body, true
}

// goingSunkAnswer is one answer a route handed its request's landing: the status, and the
// function that builds the value the landing encodes in the commit step.
type goingSunkAnswer struct {
	status int
	answer func() any
}

// goingRecordAnswer hands the landing of the request ctx belongs to the status its route is about
// to answer and answer, the function that builds the value it answers with, as the transaction
// about to commit computed it: the landing records them in that transaction, so the change and
// what its route answered of it commit together or not at all. The routes whose answer says what
// they found and left call it just before their commit - an issue's update, create, claim and
// release, an ask's edit and open, a thread's resolve, a follow, a document's edit and upload, an
// approval request, and a send to an agent, whose answer names the message it stored - and
// nothing else is changed of them: off the bar's in-process path it does nothing. The landing
// calls answer in the commit step, after the transaction's queued writes have run, so a value
// holding what that write phase fills in - the id of the event it queued, as an opened ask's
// opened_event_id, or the last_seq an issue update's events leave - is recorded as the route
// answers it once its Commit has run. A route whose answer holds such a value builds it in one
// local function it hands here and calls again for its own response, so the record and the
// response are one value. A value that does not encode is logged and recorded as none, which
// leaves the undo of the call with what it left unread.
func goingRecordAnswer(ctx context.Context, status int, answer func() any) {
	sink, carries := ctx.Value(goingAnswerKey{}).(*goingAnswerSink)
	if !carries {
		return
	}
	sink.answer.Store(&goingSunkAnswer{status: status, answer: answer})
}

// goingRecordRecovery hands the landing of the request ctx belongs to point, the delivery
// recovery point a write that delivers to sessions commits beside what it made
// (queueRequestKeyRecovery, bindRequestKeyRecovery): the message a send stored, the broadcast,
// the comment whose mentions deliver it. A run call's landing records it in its row's recovery
// (goingRunLanding), and the server's recovery pass replays the delivery from there as it replays
// a keyed request's (resumeOwedDeliveries). Off the bar's in-process path it does nothing.
func goingRecordRecovery[T recoveryPoint](ctx context.Context, point T) error {
	sink, carries := ctx.Value(goingAnswerKey{}).(*goingAnswerSink)
	if !carries {
		return nil
	}
	encoded, err := json.Marshal(point)
	if err != nil {
		return fmt.Errorf("encode a recovery point: %w", err)
	}
	recovery := json.RawMessage(encoded)
	sink.recovery.Store(&recovery)
	return nil
}

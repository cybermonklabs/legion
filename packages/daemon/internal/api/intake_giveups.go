package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// GiveUps is the replay and the discard of the project's give-ups, the messages its intake gave up
// on and keeps whole (intake.GiveUps, which the daemon builds from its consumers and the handlers a
// live delivery runs). The four give-up routes are its two doors: the operator's, authenticated by
// the operator's bearer, and the controller's, authenticated by a controller grant.
type GiveUps interface {
	Replay(ctx context.Context, selector record.IntakeSelector, caller string) (intake.ReplayAnswer, error)
	Discard(ctx context.Context, selector record.IntakeSelector, caller, reason string) ([]int64, error)
}

// operatorCaller is who the record keeps as having closed a give-up through the operator's door,
// or through the controller's door with a controller grant the operator's bearer minted, which
// names no controller session.
const operatorCaller = "operator"

// IntakeSelection names the give-ups a replay or a discard takes: GiveUp, one record by its id,
// or Consumer and ThroughSeq, every open record of that consumer at or below that stream sequence
// (record.IntakeSelector). The state's `intakeGiveUps` gives both: each record's id, and each
// consumer's newest sequence.
type IntakeSelection struct {
	GiveUp     int64  `json:"giveUp,omitempty"`
	Consumer   string `json:"consumer,omitempty"`
	ThroughSeq uint64 `json:"throughSeq,omitempty"`
}

func (s IntakeSelection) selector() record.IntakeSelector {
	return record.IntakeSelector{ID: s.GiveUp, Consumer: s.Consumer, ThroughSeq: s.ThroughSeq}
}

// OperatorIntakeReplayRequest is the body of `POST /legion/v1/operator/intake/replay`, which
// `legion intake replay` posts.
type OperatorIntakeReplayRequest struct {
	IntakeSelection
}

// OperatorIntakeDiscardRequest is the body of `POST /legion/v1/operator/intake/discard`, which
// `legion intake discard` posts: the selection and why its records are let go.
type OperatorIntakeDiscardRequest struct {
	IntakeSelection
	Reason string `json:"reason"`
}

// IntakeReplayRequest is the body of the controller's `POST /legion/v1/intake/replay`: the
// operator's, with the controller grant that authenticates it.
type IntakeReplayRequest struct {
	GrantID string `json:"grantId"`
	IntakeSelection
}

// IntakeDiscardRequest is the body of the controller's `POST /legion/v1/intake/discard`: the
// operator's, with the controller grant that authenticates it.
type IntakeDiscardRequest struct {
	GrantID string `json:"grantId"`
	IntakeSelection
	Reason string `json:"reason"`
}

// IntakeReplayResponse is a replay's answer through either door: each record it took, in stream
// order, with how its replay ended, and Remaining, the open records of the selection left after
// it, which a bulk replay of more than the effect takes in one call asks for again.
type IntakeReplayResponse struct {
	Replayed  []ReplayedGiveUpView `json:"replayed"`
	Remaining int                  `json:"remaining"`
}

// ReplayedGiveUpView is one record a replay took: its id, its consumer and stream sequence, how
// its replay ended (`applied`, `already applied`, `no fact` or `failed`), and for `failed` the
// error, which the record keeps as its last error.
type ReplayedGiveUpView struct {
	ID        int64                `json:"id"`
	Consumer  string               `json:"consumer"`
	StreamSeq uint64               `json:"streamSeq"`
	Outcome   intake.ReplayOutcome `json:"outcome"`
	Error     string               `json:"error,omitempty"`
}

// IntakeDiscardResponse is a discard's answer through either door: the id of each record it
// closed, in stream order.
type IntakeDiscardResponse struct {
	Discarded []int64 `json:"discarded"`
}

// The codes of the give-up routes' own refusals.
const (
	// giveUpRequestInvalid is a selection that names neither form or both, or a discard whose
	// reason is blank or too long (intake.RequestError).
	giveUpRequestInvalid = "GIVE_UP_REQUEST_INVALID"
	// giveUpNotFound is an id that names no give-up of this project: another project's record, an
	// open failure the intake has not given up, or no record.
	giveUpNotFound = "GIVE_UP_NOT_FOUND"
	// giveUpClosed is an id whose record is closed in a way the action may not take, named with how
	// and when it closed.
	giveUpClosed = "GIVE_UP_CLOSED"
	// giveUpBeingReplayed is an id whose record a replay running now holds.
	giveUpBeingReplayed = "GIVE_UP_BEING_REPLAYED"
	// noIntake is a daemon that runs no intake (Options.Intake nil), which keeps no give-ups.
	noIntake = "NO_INTAKE"
	// giveUpFailed is a replay or a discard the store did not carry out.
	giveUpFailed = "GIVE_UP_FAILED"
)

// operatorIntakeReplay is the operator's door onto the replay: `legion intake replay`. Its answer
// is the `legion` command's, which no plugin parses (answerUnparsed, writeUnparsedAnswer).
func (s *server) operatorIntakeReplay(w http.ResponseWriter, r *http.Request) {
	var req OperatorIntakeReplayRequest
	if s.readBody(w, r, &req, nil) {
		s.replayGiveUps(w, r, answerUnparsed, req.IntakeSelection, operatorCaller)
	}
}

// operatorIntakeDiscard is the operator's door onto the discard: `legion intake discard`. Its
// answer is the `legion` command's, as operatorIntakeReplay's is.
func (s *server) operatorIntakeDiscard(w http.ResponseWriter, r *http.Request) {
	var req OperatorIntakeDiscardRequest
	if s.readBody(w, r, &req, nil) {
		s.discardGiveUps(w, r, answerUnparsed, req.IntakeSelection, operatorCaller, req.Reason)
	}
}

// intakeReplay is the controller's door onto the replay: the controller tool's replay_give_up.
// Its answer is a plugin's (AnswerIntakeReplay).
func (s *server) intakeReplay(w http.ResponseWriter, r *http.Request) {
	var req IntakeReplayRequest
	if !s.readBody(w, r, &req, s.redeemIdentityCheck) {
		return
	}
	if caller, ok := s.giveUpController(w, r, req.GrantID); ok {
		s.replayGiveUps(w, r, AnswerIntakeReplay, req.IntakeSelection, caller)
	}
}

// intakeDiscard is the controller's door onto the discard: the controller tool's
// discard_give_up. Its answer is a plugin's (AnswerIntakeDiscard).
func (s *server) intakeDiscard(w http.ResponseWriter, r *http.Request) {
	var req IntakeDiscardRequest
	if !s.readBody(w, r, &req, s.redeemIdentityCheck) {
		return
	}
	if caller, ok := s.giveUpController(w, r, req.GrantID); ok {
		s.discardGiveUps(w, r, AnswerIntakeDiscard, req.IntakeSelection, caller, req.Reason)
	}
}

// giveUpController answers who the record keeps as the caller of the controller door, whose grant
// controllerActor redeems and refuses as it does the pause's: the operator for a controller grant
// the operator's bearer minted, and `controller session <id>` for one the controller's registered
// session minted.
func (s *server) giveUpController(w http.ResponseWriter, r *http.Request, grantID string) (string, bool) {
	by, ok := s.controllerActor(w, r, grantID, "a give-up's replay or discard requires a controller grant")
	if !ok {
		return "", false
	}
	if by.Kind == store.PauseByOperator {
		return operatorCaller, true
	}
	return "controller session " + by.Session, true
}

// replayGiveUps runs the replay of the give-ups selection names for caller and answers it, through
// either door. A replay runs to its end when its caller hangs up (intake.GiveUps.Replay), so its
// answer may reach no one; the log line says what it did either way. The answer is written as
// answer, the id of the door's own answer: AnswerIntakeReplay on the controller's door, and
// answerUnparsed on the operator's. Every refusal is AnswerRefusal.
func (s *server) replayGiveUps(w http.ResponseWriter, r *http.Request, answer AnswerID, selection IntakeSelection, caller string) {
	if s.refusedGiveUps(w, r, "intake replay") {
		return
	}
	replay, err := s.giveUps.Replay(r.Context(), selection.selector(), caller)
	if err != nil {
		s.refuseGiveUps(w, r, "intake replay", selection, caller, err)
		return
	}
	response := IntakeReplayResponse{Replayed: make([]ReplayedGiveUpView, len(replay.Replayed)), Remaining: replay.Remaining}
	failed := 0
	for i, replayed := range replay.Replayed {
		response.Replayed[i] = ReplayedGiveUpView{ID: replayed.ID, Consumer: replayed.Consumer, StreamSeq: replayed.StreamSeq,
			Outcome: replayed.Outcome, Error: replayed.Error}
		if replayed.Outcome == intake.ReplayFailed {
			failed++
		}
	}
	s.log.Info("api: replayed intake give-ups", "caller", caller, "giveUp", selection.GiveUp, "consumer", selection.Consumer,
		"throughSeq", selection.ThroughSeq, "replayed", len(response.Replayed), "failed", failed, "remaining", response.Remaining)
	s.writeAnswer(w, r, answer, http.StatusOK, response)
}

// discardGiveUps runs the discard of the give-ups selection names for caller, for reason, and
// answers it, through either door, written as answer as replayGiveUps writes a replay's.
func (s *server) discardGiveUps(w http.ResponseWriter, r *http.Request, answer AnswerID, selection IntakeSelection, caller, reason string) {
	if s.refusedGiveUps(w, r, "intake discard") {
		return
	}
	ids, err := s.giveUps.Discard(r.Context(), selection.selector(), caller, reason)
	if err != nil {
		s.refuseGiveUps(w, r, "intake discard", selection, caller, err)
		return
	}
	if ids == nil {
		ids = []int64{}
	}
	s.log.Info("api: discarded intake give-ups", "caller", caller, "giveUp", selection.GiveUp, "consumer", selection.Consumer,
		"throughSeq", selection.ThroughSeq, "discarded", ids)
	s.writeAnswer(w, r, answer, http.StatusOK, IntakeDiscardResponse{Discarded: ids})
}

// refusedGiveUps refuses request with 503 where the daemon runs no intake, and while its stop
// drains it (stopped), and reports whether it did.
func (s *server) refusedGiveUps(w http.ResponseWriter, r *http.Request, request string) bool {
	if s.giveUps == nil {
		s.writeFailure(w, r, http.StatusServiceUnavailable, noIntake, request+" refused: this daemon runs no intake, so it keeps no give-ups")
		return true
	}
	return s.stopped(w, r, request)
}

// refuseGiveUps answers the refusal of a replay or a discard: 400 for what the request asks
// (intake.RequestError), 404 for an id of no give-up of this project, 409 for an id whose record
// is closed, with how and when, or that a replay running now holds, and 500, logged, for a failure
// of the store (logRequestFailure, since the discard runs on r's context).
func (s *server) refuseGiveUps(w http.ResponseWriter, r *http.Request, request string, selection IntakeSelection, caller string, err error) {
	var invalid *intake.RequestError
	var closed *record.IntakeGiveUpClosedError
	switch {
	case errors.As(err, &invalid):
		s.writeFailure(w, r, http.StatusBadRequest, giveUpRequestInvalid, invalid.Error())
	case errors.Is(err, record.ErrIntakeGiveUpNotFound):
		s.writeFailure(w, r, http.StatusNotFound, giveUpNotFound, fmt.Sprintf("give-up %d: %v", selection.GiveUp, record.ErrIntakeGiveUpNotFound))
	case errors.As(err, &closed):
		s.writeFailure(w, r, http.StatusConflict, giveUpClosed, closed.Error())
	case errors.Is(err, record.ErrIntakeGiveUpBeingReplayed):
		s.writeFailure(w, r, http.StatusConflict, giveUpBeingReplayed, fmt.Sprintf("give-up %d: %v", selection.GiveUp, record.ErrIntakeGiveUpBeingReplayed))
	default:
		s.logRequestFailure(r, err, "api: a give-up request failed", "request", request, "caller", caller, "giveUp", selection.GiveUp,
			"consumer", selection.Consumer, "throughSeq", selection.ThroughSeq)
		s.writeFailure(w, r, http.StatusInternalServerError, giveUpFailed, request+" failed: the daemon could not record it")
	}
}

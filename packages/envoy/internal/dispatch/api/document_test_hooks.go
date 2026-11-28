package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
)

// The document test hooks: mounted by routes() only when Deps.TestHooksEnabled is set, so the
// multiprocess suite (cmd/dispatch) can read and steer one process's rooms. Each answers 404
// TEST_HOOKS_OFF when the document service carries no test hooks (docs.Deps.TestHooks).

// documentTestHooks is the document service's test hook surface (docs.Service). It is reached by
// assertion rather than through docs.API, so the API every handler and test double implements
// does not carry test hooks.
type documentTestHooks interface {
	ResidentRooms() []string
	AskBlockAttributes(ctx context.Context, artifactID string) (map[string]map[string]any, error)
	RoomCounters(artifactID string) (docs.RoomCounters, error)
	HoldRoomPersistence(artifactID string, duration time.Duration) error
	ArmLiveWritePause(artifactID string, point docs.LiveWritePausePoint) error
	AwaitLiveWriteParked(ctx context.Context, artifactID string, point docs.LiveWritePausePoint) error
	ReleaseLiveWritePause(artifactID string, point docs.LiveWritePausePoint) (bool, error)
}

// liveWriteParkedWait is how long GET .../_test/live-write-pauses/{point} waits for a write to
// park when the request names no ?wait.
const liveWriteParkedWait = 10 * time.Second

// requireDocumentTestHooks authenticates the caller, as every test hook does, and answers the
// document service's test hooks.
func (s *server) requireDocumentTestHooks(w http.ResponseWriter, r *http.Request) (documentTestHooks, bool) {
	if !s.requireAuthenticated(w, r) {
		return nil, false
	}
	hooks, ok := s.deps.Docs.(documentTestHooks)
	if !ok {
		writeDocumentTestHookError(w, docs.ErrTestHooksOff)
		return nil, false
	}
	return hooks, true
}

// hookRoom is the room the document {id} is edited in (docs.DocumentRoomOfStatement): its issue's
// room for a role document of a converted issue, the document itself otherwise. The room hooks
// below act on this process's rooms, which are keyed by their room's name, and a document's id is
// that name only where the document is its own room.
func (s *server) hookRoom(w http.ResponseWriter, r *http.Request) (string, bool) {
	var room string
	if err := s.deps.Store.Pool.QueryRow(r.Context(), docs.DocumentRoomOfStatement, r.PathValue("id")).Scan(&room); err != nil {
		writeDocumentTestHookError(w, fmt.Errorf("read the room of document %s: %w", r.PathValue("id"), err))
		return "", false
	}
	return room, true
}

func writeDocumentTestHookError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, docs.ErrTestHooksOff):
		writeError(w, "TEST_HOOKS_OFF", http.StatusNotFound, err.Error())
	case errors.Is(err, docs.ErrLiveWritePausePoint):
		writeError(w, "PAUSE_POINT_INPUT", http.StatusBadRequest, err.Error()+`; want "after-first-append" or "after-commit"`)
	case errors.Is(err, docs.ErrLiveWritePauseArmed):
		writeError(w, "PAUSE_ARMED", http.StatusConflict, err.Error())
	case errors.Is(err, docs.ErrLiveWritePauseNotArmed):
		writeError(w, "PAUSE_NOT_ARMED", http.StatusNotFound, err.Error())
	default:
		writeError(w, "TEST_HOOK_FAILED", http.StatusInternalServerError, err.Error())
	}
}

// residentRooms is the body GET /api/v1/artifacts/_test/rooms answers.
type residentRooms struct {
	Rooms []string `json:"rooms"`
}

// liveAskAttributes is the body GET /api/v1/artifacts/{id}/_test/ask-attributes answers: each ask
// block of the live document, by block id, with every attribute but its id.
type liveAskAttributes struct {
	Blocks map[string]map[string]any `json:"blocks"`
}

// liveWritePauseState is the body the GET and the DELETE of
// /api/v1/artifacts/{id}/_test/live-write-pauses/{point} answer.
type liveWritePauseState struct {
	Parked bool `json:"parked"`
}

// listResidentRooms answers {"rooms": [artifact id, ...]}: the documents this process holds a
// room of now.
func (s *server) listResidentRooms(w http.ResponseWriter, r *http.Request) {
	hooks, ok := s.requireDocumentTestHooks(w, r)
	if !ok {
		return
	}
	WriteJSON(w, http.StatusOK, residentRooms{Rooms: hooks.ResidentRooms()})
}

// getLiveAskAttributes answers {"blocks": {block id: {attribute: value, ...}}}: the attributes
// each ask block holds in this process's live tree of the document
// (docs.Service.AskBlockAttributes).
func (s *server) getLiveAskAttributes(w http.ResponseWriter, r *http.Request) {
	hooks, ok := s.requireDocumentTestHooks(w, r)
	if !ok {
		return
	}
	blocks, err := hooks.AskBlockAttributes(r.Context(), r.PathValue("id"))
	if err != nil {
		writeDocumentTestHookError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, liveAskAttributes{Blocks: blocks})
}

// getRoomCounters answers {"room_failures": n, "lock_waiting_compactions": n, "room_closes": n,
// "settlement_resumptions": [time, ...], "awareness_clients": n}: what this process did with the
// rooms of the room the document is edited in (hookRoom) and its owed settlement since it started,
// and how many awareness client entries its room holds now.
func (s *server) getRoomCounters(w http.ResponseWriter, r *http.Request) {
	hooks, ok := s.requireDocumentTestHooks(w, r)
	if !ok {
		return
	}
	room, ok := s.hookRoom(w, r)
	if !ok {
		return
	}
	counters, err := hooks.RoomCounters(room)
	if err != nil {
		writeDocumentTestHookError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, counters)
}

// holdPersistenceInput is the body of POST /api/v1/artifacts/{id}/_test/hold-persistence.
type holdPersistenceInput struct {
	Duration string `json:"duration" required:"true"`
}

// holdRoomPersistence takes {"duration": "<Go duration>"} and holds, for that long from now, every
// append of a browser update this process's rooms of the room the document is edited in (hookRoom)
// make. 204 once the hold is set.
func (s *server) holdRoomPersistence(w http.ResponseWriter, r *http.Request) {
	hooks, ok := s.requireDocumentTestHooks(w, r)
	if !ok {
		return
	}
	var input holdPersistenceInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	duration, err := time.ParseDuration(input.Duration)
	if err != nil || duration <= 0 {
		writeError(w, "DURATION_INPUT", http.StatusBadRequest, `duration must be a positive Go duration, such as "300ms"`)
		return
	}
	room, ok := s.hookRoom(w, r)
	if !ok {
		return
	}
	if err := hooks.HoldRoomPersistence(room, duration); err != nil {
		writeDocumentTestHookError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// armLiveWritePause arms a pause of the next live write of the room the document is edited in
// (hookRoom) at {point} (after-first-append or after-commit). 204 once armed; 409 PAUSE_ARMED when
// one already is.
func (s *server) armLiveWritePause(w http.ResponseWriter, r *http.Request) {
	hooks, ok := s.requireDocumentTestHooks(w, r)
	if !ok {
		return
	}
	room, ok := s.hookRoom(w, r)
	if !ok {
		return
	}
	if err := hooks.ArmLiveWritePause(room, docs.LiveWritePausePoint(r.PathValue("point"))); err != nil {
		writeDocumentTestHookError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// awaitLiveWriteParked waits up to ?wait (a Go duration, default liveWriteParkedWait) for a write
// to park at the armed pause and answers {"parked": true}, or {"parked": false} when none did in
// that time.
func (s *server) awaitLiveWriteParked(w http.ResponseWriter, r *http.Request) {
	hooks, ok := s.requireDocumentTestHooks(w, r)
	if !ok {
		return
	}
	room, ok := s.hookRoom(w, r)
	if !ok {
		return
	}
	wait := liveWriteParkedWait
	if raw := r.URL.Query().Get("wait"); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil || parsed <= 0 {
			writeError(w, "WAIT_INPUT", http.StatusBadRequest, `wait must be a positive Go duration, such as "10s"`)
			return
		}
		wait = parsed
	}
	ctx, cancel := context.WithTimeout(r.Context(), wait)
	defer cancel()
	err := hooks.AwaitLiveWriteParked(ctx, room, docs.LiveWritePausePoint(r.PathValue("point")))
	switch {
	case err == nil:
		WriteJSON(w, http.StatusOK, liveWritePauseState{Parked: true})
	case errors.Is(err, context.DeadlineExceeded):
		WriteJSON(w, http.StatusOK, liveWritePauseState{Parked: false})
	default:
		writeDocumentTestHookError(w, err)
	}
}

// releaseLiveWritePause disarms the pause and lets the write parked there go on, answering
// {"parked": bool}: whether a write had parked.
func (s *server) releaseLiveWritePause(w http.ResponseWriter, r *http.Request) {
	hooks, ok := s.requireDocumentTestHooks(w, r)
	if !ok {
		return
	}
	room, ok := s.hookRoom(w, r)
	if !ok {
		return
	}
	parked, err := hooks.ReleaseLiveWritePause(room, docs.LiveWritePausePoint(r.PathValue("point")))
	if err != nil {
		writeDocumentTestHookError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, liveWritePauseState{Parked: parked})
}

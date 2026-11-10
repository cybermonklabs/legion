package api

import (
	"net/http"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// Handing an ask on: PUT /api/v1/asks/{id}/asked-of.
//
// An ask waits on one person, and until somebody hands it on that person is the assignee of its
// issue (askedOfPersonExpression, the one expression every list reading an ask's person shares).
// This route writes the other half of that expression, asks.asked_of: the person a question is
// for, when that is not whoever holds the issue. The ask keeps everything else - its question,
// its thread, its followers - and stays open; only whose Inbox it sits in moves.
//
// It is a person's act, so it needs the person scope (routeScopes) and nobody else may make it:
// a credential the deployment configured holds no person to hand a question from, and handing a
// question to somebody is how one person asks another. The row records who handed it on and
// when (asked_of_by, asked_of_at) whatever else the write leaves, so the ask card can say it.

// askAskedOfInput is the body of PUT /api/v1/asks/{id}/asked-of: the person the ask waits on
// from now on. It is required - a body naming nobody is 400 INVALID_ASK - and never null,
// since an ask always waits on somebody; handing one to the assignee of its own issue is what
// gives the ask back to the issue (the empty stored field below).
type askAskedOfInput struct {
	AskedOf *string `json:"asked_of" required:"true"`
}

// putAskAskedOf writes the person an open ask waits on.
func (s *server) putAskAskedOf(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	askID, err := parseAskID(r)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	var input askAskedOfInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if input.AskedOf == nil {
		writeError(w, "INVALID_ASK", http.StatusBadRequest, "asked_of must be a person's email")
		return
	}
	// Who the ask is handed to, canonicalised and held to having signed in
	// (400 ASSIGNEE_NOT_ALLOWED), resolved before the transaction opens: allowedLogin reads the
	// shared pool, which refuses a second connection to a caller already holding one
	// (store.ErrNestedAcquire), so it cannot run under the transaction below - the reason
	// editAsk resolves its acting person before it begins. A caller naming nobody who has
	// signed in is therefore told so before the ask is read.
	askedOf, err := s.allowedLogin(r.Context(), *input.AskedOf)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}

	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	// The locks, in the order the ask writes take them: the owner row, then the ask row. A
	// closed issue is 409 ISSUE_CLOSED here; a project document is always open, so a document
	// ask passes.
	unlockedAsk, err := s.loadAsk(r.Context(), tx, askID)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := s.requireOpenOwner(r.Context(), tx, ownerOf(unlockedAsk.IssueKey, unlockedAsk.ArtifactID)); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	ask, err := s.lockAskForTransition(r.Context(), tx, askID)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if ask.State != "open" {
		writeError(w, "ASK_NOT_OPEN", http.StatusConflict, "only open asks may be handed to another person")
		return
	}
	// Whom the ask waited on before this write, read under the two locks through the expression
	// every reader of that person shares, and the assignee of its issue beside it (null for a
	// document ask, which has none).
	var waitedOn, assignee *string
	if err := tx.QueryRow(r.Context(), `
		select `+askedOfPersonExpression+`, i.assignee
		from asks a
		left join issues i on i.key = a.issue_key
		where a.id = $1
	`, ask.ID).Scan(&waitedOn, &assignee); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// Handing an ask to the assignee of its own issue empties the field instead of repeating
	// them in it: the ask follows its issue again, so a later reassignment carries it along, and
	// coalesce(asked_of, assignee) answers the same person either way. A document ask has no
	// assignee, so nothing empties there.
	stored := &askedOf
	if assignee != nil && *assignee == askedOf {
		stored = nil
	}
	handedOnBy := model.CanonicalLogin(actor.ID)
	var handedOnAt time.Time
	if err := tx.QueryRow(r.Context(), `
		update asks
		set asked_of = $2, asked_of_by = $3, asked_of_at = now()
		where id = $1
		returning asked_of_at
	`, ask.ID, stored, handedOnBy).Scan(&handedOnAt); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	ask.AskedOf = stored
	ask.AskedOfBy = &handedOnBy
	ask.AskedOfAt = &handedOnAt

	payload := model.NewAskEventPayload(ask, model.ReferenceChanges{})
	// The two people the hand-on moved, as an assignee move names them (issueWriteThreadLogins):
	// the Inbox lists an open ask under the person it waits on, so this write takes it out of
	// one person's list and puts it into the other's, and no other reader's rows move. That is
	// why the event does not carry the ask's thread logins (events.AskEventPayload): somebody
	// who replied in the thread has the same rows after this write as before it.
	payload.ThreadLogins = movedLogins(nil, waitedOn, &askedOf)
	event, err := s.appendEvent(r.Context(), tx, ownerOf(ask.IssueKey, ask.ArtifactID).event(
		"ask.asked_of", actor, payload,
	))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.publish(event)
	WriteJSON(w, http.StatusOK, ask)
}

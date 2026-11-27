package api

import (
	"context"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// Snoozing an Inbox row is a human's own view state, like the Agents page's Clear: the ask
// stays open, stays listed, and reaches everyone else unchanged, while this viewer's Inbox
// folds it away until snoozed_until. The routes need the person scope (their routes' OAuthScopes) because
// the Inbox does; a session has no inbox to snooze a row out of.
//
// No job ever wakes a snoozed row. GET /api/v1/inbox carries snoozed_until on the row, and
// /api/v2 carries it on the caller's ask mark (GET /api/v2/asks with include=mark, GET
// /api/v2/me/marks); the reader decides from it, so the moment a snooze passes the row is an
// ordinary row again on the next read.
//
// The row is keyed on the canonical login, not on the actor id: rows written before every
// identity lowercased the person it names can carry another casing of the same email, so keying
// on the actor id raw could give one person two snooze sets. model.CanonicalLogin is the form
// issues store assignees in and the form inboxAssigneeFilter compares against.
//
// A snooze or an un-snooze moves only its writer's Inbox, so it announces itself only to that
// person's other tabs and devices: it appends `user_state.updated` (askSnoozeEvent), the event a
// person's own pin, read mark and dismissal already append, with `changed: ["snoozed"]` and the
// writer's login alone, since every signed-in feed carries it. A write that moves nothing - a
// snooze to the moment the row already holds, an un-snooze of a row nobody snoozed - appends
// nothing, since each event refetches the Inbox in every tab the person has open.

// userAskSnooze is one row's snooze for the calling human: when the Inbox stops folding it
// away. The Inbox's "Until I clear it" is this timestamp set far enough out that only the
// reader's own un-snooze brings the row back.
type userAskSnooze struct {
	SnoozedUntil string `json:"snoozed_until"`
}

// askSnoozeInput is the body of PUT /api/v1/me/asks/{id}/snooze.
type askSnoozeInput struct {
	SnoozedUntil *string `json:"snoozed_until" required:"true"`
}

// PUT /api/v1/me/asks/{id}/snooze
func (s *server) putAskSnooze(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	askID, err := parseAskID(r)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	var input askSnoozeInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	if input.SnoozedUntil == nil {
		writeError(w, r, apicode.SnoozeInput, "snoozed_until is required")
		return
	}
	snoozedUntil, err := time.Parse(time.RFC3339, *input.SnoozedUntil)
	if err != nil {
		writeError(w, r, apicode.SnoozeInput, "snoozed_until must be an RFC3339 timestamp")
		return
	}
	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	defer tx.Rollback(r.Context())
	stored, event, err := s.snoozeAskIn(r.Context(), tx, actor, askID, snoozedUntil)
	if err == nil {
		err = s.commitAskSnooze(r.Context(), tx, event)
	}
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, userAskSnooze{SnoozedUntil: timestampValue(stored)})
}

// snoozeAskIn folds, in tx, the ask askID names away from actor's Inbox until snoozedUntil,
// replacing the snooze it held, and answers the snooze as stored and, where it moved, the event
// that announces it (askSnoozeEvent), for its caller to append: the work of PUT
// /api/v1/me/asks/{id}/snooze and of an ask mark's write under /api/v2 once each has read the
// moment. A moment that has passed is 400 SNOOZE_INPUT and an ask that does not exist 404
// NOT_FOUND, each writing nothing.
func (s *server) snoozeAskIn(ctx context.Context, tx pgx.Tx, actor model.Actor, askID string, snoozedUntil time.Time) (time.Time, *model.Event, error) {
	// A snooze that has already passed would fold nothing away, so it is refused. "Passed" is
	// judged against the database's now(), in the statement that stores the snooze: each
	// process's host keeps its own clock, and the Inbox compares a snooze with now when it reads
	// it. The bound allows no skew: the Inbox's presets are its own to keep clear of it, and the
	// one that can land near midnight keeps a few seconds of today in hand rather than naming a
	// moment this could refuse. The ask is selected rather than its id inserted, so an unknown ask
	// stores nothing and reads as absent - a 404 - instead of the foreign key's 500; a past moment
	// is refused first, whatever ask it names. previous is read from the statement's own snapshot,
	// before the write, so a snooze to the moment the row already holds is known to move nothing.
	var future bool
	var stored, previous *time.Time
	if err := tx.QueryRow(ctx, `
		with previous as (
			select snoozed_until from user_ask_snooze where login = $1 and ask_id = $2
		), snoozed as (
			insert into user_ask_snooze (login, ask_id, snoozed_until)
			select $1, id, $3::timestamptz from asks where id = $2 and $3::timestamptz > now()
			on conflict (login, ask_id) do update set snoozed_until = excluded.snoozed_until
			returning snoozed_until
		)
		select $3::timestamptz > now(), (select snoozed_until from snoozed), (select snoozed_until from previous)
	`, model.CanonicalLogin(actor.ID), askID, snoozedUntil).Scan(&future, &stored, &previous); err != nil {
		return time.Time{}, nil, err
	}
	if !future {
		return time.Time{}, nil, errorf(apicode.SnoozeInput, "snoozed_until must be in the future")
	}
	if stored == nil {
		return time.Time{}, nil, errorf(apicode.NotFound, "no ask %s", askID)
	}
	if previous != nil && previous.Equal(*stored) {
		return *stored, nil, nil
	}
	event, err := s.askSnoozeEvent(ctx, tx, actor, askID)
	if err != nil {
		return time.Time{}, nil, err
	}
	return *stored, &event, nil
}

// DELETE /api/v1/me/asks/{id}/snooze
func (s *server) deleteAskSnooze(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	askID, err := parseAskID(r)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	defer tx.Rollback(r.Context())
	event, err := s.unsnoozeAskIn(r.Context(), tx, actor, askID)
	if err == nil {
		err = s.commitAskSnooze(r.Context(), tx, event)
	}
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// unsnoozeAskIn ends, in tx, actor's snooze of the ask askID names, and answers the event that
// announces it where there was one: the work of DELETE /api/v1/me/asks/{id}/snooze and of an ask
// mark's write under /api/v2 that writes snoozed_until null. Un-snoozing a row that is not snoozed
// is the state the caller asked for, so it is no refusal, and it announces nothing.
func (s *server) unsnoozeAskIn(ctx context.Context, tx pgx.Tx, actor model.Actor, askID string) (*model.Event, error) {
	removed, err := tx.Exec(ctx, `
		delete from user_ask_snooze where login = $1 and ask_id = $2
	`, model.CanonicalLogin(actor.ID), askID)
	if err != nil || removed.RowsAffected() == 0 {
		return nil, err
	}
	event, err := s.askSnoozeEvent(ctx, tx, actor, askID)
	if err != nil {
		return nil, err
	}
	return &event, nil
}

// commitAskSnooze ends a snooze's or an un-snooze's own transaction: it appends event in tx where
// the write moved anything, commits, and publishes the event.
func (s *server) commitAskSnooze(ctx context.Context, tx pgx.Tx, event *model.Event) error {
	if event == nil {
		return tx.Commit(ctx)
	}
	written, err := s.appendEvent(ctx, tx, *event)
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		return err
	}
	s.publish(ctx, written)
	return nil
}

// askSnoozeEvent is the `user_state.updated` a snooze or an un-snooze of askID appends, owned
// by the ask's project (its issue's, or its document's) as every `user_state.updated` is. It
// carries the writer's login and `changed: ["snoozed"]`, and nothing else: every signed-in
// person's stream replays it, so it names neither the ask nor any issue state of the writer's,
// and the writer's other tabs refetch their whole Inbox from it.
func (s *server) askSnoozeEvent(ctx context.Context, tx pgx.Tx, actor model.Actor, askID string) (model.Event, error) {
	var project string
	if err := tx.QueryRow(ctx, `
		select coalesce(i.project_key, ar.project_key)
		from asks a
		left join issues i on i.key = a.issue_key
		left join artifacts ar on ar.id = a.artifact_id
		where a.id = $1
	`, askID).Scan(&project); err != nil {
		return model.Event{}, err
	}
	return projectOwner(project).event("user_state.updated", actor, map[string]any{
		"login":   model.CanonicalLogin(actor.ID),
		"changed": []string{"snoozed"},
	}), nil
}

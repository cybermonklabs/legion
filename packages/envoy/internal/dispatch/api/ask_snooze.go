package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/failpoint"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// Snoozing an Inbox row is a human's own view state, like the Agents page's Clear: the ask
// stays open, stays listed, and reaches everyone else unchanged, while this viewer's Inbox
// folds it away until snoozed_until. The routes need the person scope (routeScopes) because
// the Inbox does; a session has no inbox to snooze a row out of.
//
// No job ever wakes a snoozed row. GET /api/v1/inbox carries snoozed_until on the row and
// the reader decides from it, so the moment a snooze passes the row is an ordinary row again
// on the next read.
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
		s.writeHandlerError(w, err)
		return
	}
	var input askSnoozeInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if input.SnoozedUntil == nil {
		writeError(w, "SNOOZE_INPUT", http.StatusBadRequest, "snoozed_until is required")
		return
	}
	snoozedUntil, err := time.Parse(time.RFC3339, *input.SnoozedUntil)
	if err != nil {
		writeError(w, "SNOOZE_INPUT", http.StatusBadRequest, "snoozed_until must be an RFC3339 timestamp")
		return
	}
	// A snooze that has already passed would fold nothing away, so it is refused. "Passed" is
	// judged against the database's now(), in the statement that stores the snooze: each
	// process's host keeps its own clock, and the Inbox compares a snooze with now when it reads
	// it. The bound allows no skew: the Inbox's presets are its own to keep clear of it, and the
	// one that can land near midnight keeps a few seconds of today in hand rather than naming a
	// moment this could refuse. The ask is selected rather than its id inserted, so an unknown ask
	// stores nothing and reads as absent - a 404 - instead of the foreign key's 500; a past moment
	// is refused first, whatever ask it names. previous is the row as it stands once locked `for
	// no key update`, which the insert's own select reads before it writes (the join), so it is the
	// version the conflict arm then compares with and updates: a snooze another request committed
	// after this statement's snapshot was taken is waited for and read, rather than the snapshot's
	// older row, so a snooze to the moment the row already holds is known to move nothing and every
	// other one states its tuple. A snooze that moves the row marks it at its snoozed_until, the
	// moment of its snoozed tuple (decision 13), where the mark fits the store ($4, markFits); one
	// that moves nothing leaves the row's marker as it was, since it states no tuple.
	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	var future bool
	var stored, previous *time.Time
	var ask *string
	var at time.Time
	if err := tx.QueryRow(r.Context(), `
		with previous as (
			select snoozed_until from user_ask_snooze where login = $1 and ask_id = $2
			for no key update
		), snoozed as (
			insert into user_ask_snooze (login, ask_id, snoozed_until, tuples_stored_at)
			select $1, a.id, $3::timestamptz, case when $4::boolean then $3::timestamptz end
			from asks a left join previous on true
			where a.id = $2 and $3::timestamptz > now()
			on conflict (login, ask_id) do update set
				snoozed_until = excluded.snoozed_until,
				tuples_stored_at = case when user_ask_snooze.snoozed_until is distinct from excluded.snoozed_until
				                        then excluded.tuples_stored_at else user_ask_snooze.tuples_stored_at end
			returning snoozed_until, ask_id::text
		)
		select $3::timestamptz > now(), (select snoozed_until from snoozed), (select snoozed_until from previous),
		       (select ask_id from snoozed), now()
	`, model.CanonicalLogin(actor.ID), askID, snoozedUntil, markFits(actor)).Scan(&future, &stored, &previous, &ask, &at); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if !future {
		writeError(w, "SNOOZE_INPUT", http.StatusBadRequest, "snoozed_until must be in the future")
		return
	}
	if stored == nil {
		writeError(w, "NOT_FOUND", http.StatusNotFound, "no ask "+askID)
		return
	}
	if previous == nil || !previous.Equal(*stored) {
		mark := personMark(actor, graphstore.TupleSnoozed, graphstore.KindAsk, *ask, *stored, true, graphstore.NoEvent)
		if !s.announceAskSnooze(w, r, tx, actor, askID, at, mark) {
			return
		}
	} else if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, userAskSnooze{SnoozedUntil: timestampValue(*stored)})
}

// DELETE /api/v1/me/asks/{id}/snooze
func (s *server) deleteAskSnooze(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	askID, err := parseAskID(r)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// Un-snoozing a row that is not snoozed is the state the caller asked for, so it answers
	// the same 204 rather than a 404 the reader could do nothing with, and announces nothing.
	// A row it deletes is the retraction of its snoozed tuple at the moment of the delete.
	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	var ask string
	var at time.Time
	err = tx.QueryRow(r.Context(), `
		delete from user_ask_snooze where login = $1 and ask_id = $2
		returning ask_id::text, now()
	`, model.CanonicalLogin(actor.ID), askID).Scan(&ask, &at)
	if errors.Is(err, pgx.ErrNoRows) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if !s.announceAskSnooze(w, r, tx, actor, askID, at, personMark(actor, graphstore.TupleSnoozed, graphstore.KindAsk, ask, at, false, graphstore.NoEvent)) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// announceAskSnooze appends the snooze's event in tx, and mark, the snoozed tuple or its
// retraction the write states, under that event (graphstore.QueueFromPerson), in one batch whose
// person node is made at at, the transaction's now(); the failure point between the snooze's row
// and the mark (failpoint.AskSnooze) is decided before the batch is sent and queued between the
// two. It then commits and publishes the event, and answers false after writing the error
// response when any step fails.
func (s *server) announceAskSnooze(
	w http.ResponseWriter, r *http.Request, tx pgx.Tx, actor model.Actor, askID string, at time.Time, mark graphstore.Tuple,
) bool {
	event, err := s.askSnoozeEvent(r.Context(), tx, actor, askID)
	if err == nil {
		mark.SourceEvent = graphstore.LatestEvent
		fails := failpoint.Planned(r.Context(), failpoint.AskSnooze)
		event, err = s.appendEventThen(r.Context(), tx, event, func(b *store.WriteBatch) error {
			failpoint.Queue(b, fails)
			return graphstore.QueueFromPerson(b, at, []graphstore.Tuple{mark})
		})
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		s.writeHandlerError(w, err)
		return false
	}
	s.publish(r.Context(), event)
	return true
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

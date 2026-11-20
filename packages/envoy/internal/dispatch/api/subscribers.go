package api

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/envoy"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/routing"
)

// subscriber is a session whose persisted Envoy interests include an issue's
// or unlinked document's topic family, enriched with its live registry state
// when the session is currently registered. Removable is false when the only
// matching topic is broader than this owner (e.g. "notifications.dispatch.>"):
// removing that topic would silently unsubscribe the session from every
// other issue and document too, so Unsubscribe refuses it; Via then names
// one such topic for the header to explain why. LastSeen is the session's
// last heartbeat: the registered session's own, else the one the listener still
// knows for the interest row, else the row's updated_at, which can lag that
// heartbeat by up to the listener's interest refresh age.
//
// State is the registry's own state for the session, and Live is that state being `live`
// alone. The listener's list carries the unresponsive sessions too, so a row's presence in it
// is no longer what makes a subscriber live: a stalled session is listed, named unresponsive,
// and still receives - its events wait for it - which is what a reader of this list needs to
// tell from an ended one. State is empty for a session the registry holds no record of.
type subscriber struct {
	SessionID string   `json:"session_id"`
	Title     string   `json:"title"`
	Live      bool     `json:"live"`
	State     string   `json:"state,omitempty"`
	LastSeen  int64    `json:"last_seen"`
	Topics    []string `json:"topics"`
	Removable bool     `json:"removable"`
	Via       string   `json:"via,omitempty"`
}

// issueSubscriberBase is the bare NATS subject family root a session must be
// interested in (with or without a trailing ".>") to receive an issue's events.
func issueSubscriberBase(key string) string {
	return "notifications.dispatch.issue." + key
}

// documentSubscriberBase is the bare NATS subject family root for an unlinked
// project document's own topic (see model.Artifact: an issue-attached
// artifact has no topic of its own — its events route through the issue).
func documentSubscriberBase(project, slug string) string {
	return "notifications.dispatch.document." + project + "." + slug
}

// matchingTopics returns the subset of topics that would actually deliver an
// event published under base, using the listener's own wildcard semantics
// (routing.Match): a bare subject alone (no ".>") never matches, since NATS
// only delivers a typed event like "<base>.issue.created" to a subscriber of
// "<base>.>" or a broader wildcard, never to a subscriber of "<base>" alone.
// A topic is read in every form a reader takes it in (contracts.TopicForms):
// the listener filters a session's consumer on both forms of each topic, so a
// session following an issue in the form naming its project receives that
// issue's events and is its subscriber, as one following the old form is.
func matchingTopics(topics []string, base string) []string {
	candidate := base + ".marker"
	matched := make([]string, 0, len(topics))
	for _, topic := range topics {
		if slices.ContainsFunc(contracts.TopicForms(topic), func(form string) bool { return routing.Match(form, candidate) }) {
			matched = append(matched, topic)
		}
	}
	return matched
}

// exactTopics returns the subset of topics scoped exactly to base (base
// itself, or base+".>"), in either form of the name (contracts.TopicForms) —
// the only forms safe to remove without silently unsubscribing the session
// from every other issue or document a broader wildcard among its topics also
// happens to cover. Each is returned as the session wrote it, which is how the
// listener's unsubscribe finds it.
func exactTopics(topics []string, base string) []string {
	wildcard := base + ".>"
	exact := make([]string, 0, len(topics))
	for _, topic := range topics {
		if slices.ContainsFunc(contracts.TopicForms(topic), func(form string) bool { return form == base || form == wildcard }) {
			exact = append(exact, topic)
		}
	}
	return exact
}

// listIssueSubscribers answers the sessions subscribed to an issue's topics. An issue of a
// project the caller does not see answers as a key no issue carries answers: a topic scoped to
// that issue names it, so it is left out (writeSubscribers' hidden), and what a broader wildcard
// delivers is listed as for any key.
func (s *server) listIssueSubscribers(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.actingPerson(w, r); !ok {
		return
	}
	who, ok := s.readerOf(w, r)
	if !ok {
		return
	}
	key := r.PathValue("key")
	viewer := refs.VisibilityArg(who)
	var hidden bool
	err := s.deps.Store.Pool.QueryRow(r.Context(), `select not `+access.ProjectClause("project_key", "$2")+
		` from issues where key = $1`, key, viewer).Scan(&hidden)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		s.writeHandlerError(w, err)
		return
	}
	s.writeSubscribers(w, r, who, issueSubscriberBase(key), hidden)
}

// otherIssueBase is the subject root of an issue no key names ("-" is no key's spelling): a topic
// that delivers both its events and a given issue's is a broader wildcard, and one that delivers
// the given issue's alone is scoped to that issue.
var otherIssueBase = issueSubscriberBase("-")

// keyScoped reports whether topic delivers base's events and not those of every other issue: an
// exact form of base, or a narrower wildcard naming it (`<base>.*`, `<base>.issue.>`), which a
// broader wildcard (`notifications.dispatch.>`) is not. The topic is read in every form a reader
// takes it in (contracts.TopicForms), as matchingTopics reads it, so the form naming the key's
// project (`notifications.dispatch.issue.<project>.<key>.>`) is scoped to the key too.
func keyScoped(topic, base string) bool {
	forms := contracts.TopicForms(topic)
	delivers := func(subject string) bool {
		return slices.ContainsFunc(forms, func(form string) bool { return routing.Match(form, subject) })
	}
	return delivers(base+".marker") && !delivers(otherIssueBase+".marker")
}

func (s *server) listArtifactSubscribers(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.actingPerson(w, r); !ok {
		return
	}
	who, ok := s.readerOf(w, r)
	if !ok {
		return
	}
	artifact, _, err := s.documentOwnerFromRequest(r.Context(), s.deps.Store.Pool, who, r)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.writeSubscribers(w, r, who, documentSubscriberBase(artifact.Project, artifact.Slug), false)
}

// writeSubscribers answers the sessions whose interests deliver an event published under base,
// each one who may see (seenSessions). hidden says base names an owner of a project the
// viewer does not see: every topic scoped to that owner (keyScoped) names it, so it is left
// out of every row, and a row left with no topic is dropped; the answer is then the one a key
// nobody holds gets, which lists what the broader wildcards deliver.
func (s *server) writeSubscribers(w http.ResponseWriter, r *http.Request, who model.Actor, base string, hidden bool) {
	if s.deps.Envoy == nil {
		writeError(w, "ENVOY_UNAVAILABLE", http.StatusServiceUnavailable, "ENVOY_URL is not configured")
		return
	}
	interests, err := s.deps.Envoy.ListInterests(r.Context())
	if err != nil {
		s.writeEnvoyError(w, err)
		return
	}
	sessions, err := s.deps.Envoy.Sessions(r.Context())
	if err != nil {
		s.writeEnvoyError(w, err)
		return
	}
	// Every session the registry holds a record of, whatever its state; the row decides what
	// the subscriber reads as, so this is no longer the list of the live ones.
	registered := make(map[string]envoy.Session, len(sessions))
	for _, session := range sessions {
		registered[session.SessionID] = session
	}

	result := make([]subscriber, 0)
	for _, interest := range interests {
		matched := matchingTopics(interest.Topics, base)
		if hidden {
			matched = slices.DeleteFunc(matched, func(topic string) bool { return keyScoped(topic, base) })
		}
		if len(matched) == 0 {
			continue
		}
		entry := subscriber{SessionID: interest.SessionID, Topics: matched, LastSeen: interest.UpdatedAt}
		// A listener that restarted since the session died, or one that predates the field,
		// sends no last_seen; the row's own updated_at is then the closest it has.
		if interest.LastSeen > 0 {
			entry.LastSeen = interest.LastSeen
		}
		if exact := exactTopics(matched, base); len(exact) > 0 {
			entry.Removable = true
		} else {
			entry.Via = matched[0]
		}
		if session, ok := registered[interest.SessionID]; ok {
			entry.Title = session.Title
			entry.State = session.State
			entry.Live = session.State == envoy.StateLive
			entry.LastSeen = session.LastSeen
		}
		result = append(result, entry)
	}
	// A reader that sees every object passes SessionClause on every id, so the read would cut
	// nothing (R9).
	if !seesEveryObject(who) {
		ids := make([]string, len(result))
		for index, entry := range result {
			ids[index] = entry.SessionID
		}
		seen, err := s.seenSessions(r.Context(), refs.VisibilityArg(who), ids)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		result = slices.DeleteFunc(result, func(entry subscriber) bool { return !seen[entry.SessionID] })
	}
	sort.SliceStable(result, func(left, right int) bool {
		if result[left].LastSeen != result[right].LastSeen {
			return result[left].LastSeen > result[right].LastSeen
		}
		return result[left].SessionID < result[right].SessionID
	})
	WriteJSON(w, http.StatusOK, result)
}

// seenSessions is which of the sessions ids names the viewer may see (access.SessionClause), in one
// statement: its own and those that have acted in a project it sees, and, for an open viewer, every
// session but one that acted in a project it does not see. With no id it reads nothing.
func (s *server) seenSessions(ctx context.Context, viewer any, ids []string) (map[string]bool, error) {
	seen := map[string]bool{}
	if len(ids) == 0 {
		return seen, nil
	}
	rows, err := s.deps.Store.Pool.Query(ctx, `
		select named.id from unnest($1::text[]) as named(id)
		where `+access.SessionClause("named.id", "$2"), ids, viewer)
	if err != nil {
		return nil, fmt.Errorf("read which sessions the caller sees: %w", err)
	}
	visible, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("read which sessions the caller sees: %w", err)
	}
	for _, id := range visible {
		seen[id] = true
	}
	return seen, nil
}

// sessionsHiddenFromReader reports which of ids the request's reader may not see
// (access.SessionClause), read in one statement (seenSessions): sessionHiddenFromReader's rule over a
// list of sessions. Work with no reader in ctx, and a reader that sees every object - among them an
// open reader of a deployment that holds no private project (R9) - sees every session and costs no
// read.
func (s *server) sessionsHiddenFromReader(ctx context.Context, ids []string) (func(string) bool, error) {
	reader, ok := readerFromContext(ctx)
	if !ok || seesEveryObject(reader) {
		return func(string) bool { return false }, nil
	}
	seen, err := s.seenSessions(ctx, refs.VisibilityArg(reader), ids)
	if err != nil {
		return nil, err
	}
	return func(sessionID string) bool { return !seen[sessionID] }, nil
}

// unsubscribeIssueSession removes a session from an issue's topics. The issue is read first as
// the caller reads it, as unsubscribeArtifactSession reads its document: an issue of a project the
// caller does not see is a key no issue holds, before the listener is asked anything. The session
// is then read as the caller sees it (unsubscribeSession).
func (s *server) unsubscribeIssueSession(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	who, ok := s.readerOf(w, r)
	if !ok {
		return
	}
	key := r.PathValue("key")
	if _, _, err := issueStatus(r.Context(), who, key, pooledOwnerRead(s.deps.Store.Pool)); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.unsubscribeSession(w, r, actor, issueOwner(key), issueSubscriberBase(key))
}

func (s *server) unsubscribeArtifactSession(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	who, ok := s.readerOf(w, r)
	if !ok {
		return
	}
	artifact, own, err := s.documentOwnerFromRequest(r.Context(), s.deps.Store.Pool, who, r)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.unsubscribeSession(w, r, actor, own, documentSubscriberBase(artifact.Project, artifact.Slug))
}

// subscriptionRemovedPayload is the durable unsubscribe command. Pending is true
// until the listener removal succeeds; the outbox skips pending commands so the
// removed session is never notified before its Envoy interest is gone.
type subscriptionRemovedPayload struct {
	SessionID      string      `json:"session_id"`
	By             model.Actor `json:"by"`
	Topics         []string    `json:"topics"`
	Pending        bool        `json:"pending"`
	RequestEventID int64       `json:"request_event_id,omitempty"`
}

// subscriptionOwnerColumn is the events column a subscription removal's owner
// is matched on (issue_key or artifact_id) and the value it is matched against.
func subscriptionOwnerColumn(own owner) (column string, value any, err error) {
	switch {
	case own.IssueKey != nil:
		return "issue_key", *own.IssueKey, nil
	case own.ArtifactID != nil:
		return "artifact_id", *own.ArtifactID, nil
	default:
		return "", nil, errors.New("subscription removal requires an issue or artifact owner")
	}
}

func (s *server) pendingSubscriptionRemoval(
	ctx context.Context,
	q queryer,
	own owner,
	sessionID string,
	topics []string,
) (*int64, error) {
	column, value, err := subscriptionOwnerColumn(own)
	if err != nil {
		return nil, err
	}
	predicate := "e." + column + " = $1"
	arguments := []any{value, sessionID}
	if topics != nil {
		encoded, err := encodeJSON(topics)
		if err != nil {
			return nil, err
		}
		predicate += " and e.payload -> 'topics' = $3::jsonb"
		arguments = append(arguments, string(encoded))
	}
	var requestID int64
	err = q.QueryRow(ctx, `
		select e.id
		from events e
		where `+predicate+`
		  and e.type = 'subscription.remove_requested'
		  and e.payload ->> 'session_id' = $2
		  and coalesce(e.payload ->> 'pending', 'false') = 'true'
		  and not exists (
			select 1
			from events completed
			where completed.type = 'subscription.removed'
			  and (completed.payload ->> 'request_event_id')::bigint = e.id
		  )
		order by e.id desc
		limit 1
	`, arguments...).Scan(&requestID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &requestID, nil
}

func (s *server) completeSubscriptionRemoval(
	ctx context.Context,
	own owner,
	sessionID string,
	requestID int64,
	actor model.Actor,
) (*model.Event, error) {
	column, value, err := subscriptionOwnerColumn(own)
	if err != nil {
		return nil, err
	}
	arguments := []any{value, sessionID, requestID}
	tx, err := s.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if err := s.deps.Events.LockOwners(ctx, tx, own.event("", actor, nil)); err != nil {
		return nil, err
	}

	// The pending request, unless a subscription.removed already answers it: then there is
	// nothing left to complete.
	var payloadJSON []byte
	err = tx.QueryRow(ctx, `
		select payload from events
		where `+column+` = $1
		  and id = $3
		  and type = 'subscription.remove_requested'
		  and payload ->> 'session_id' = $2
		  and coalesce(payload ->> 'pending', 'false') = 'true'
		  and not exists (
			select 1 from events completed
			where completed.`+column+` = $1
			  and completed.type = 'subscription.removed'
			  and completed.payload ->> 'session_id' = $2
			  and (completed.payload ->> 'request_event_id')::bigint = $3
		  )
	`, arguments...).Scan(&payloadJSON)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			if err := tx.Commit(ctx); err != nil {
				return nil, err
			}
			return nil, nil
		}
		return nil, err
	}
	var payload subscriptionRemovedPayload
	if err := json.Unmarshal(payloadJSON, &payload); err != nil {
		return nil, err
	}
	payload.Pending = false
	payload.RequestEventID = requestID
	event, err := s.appendEvent(ctx, tx, own.event("subscription.removed", actor, payload))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &event, nil
}

// finishPendingRemoval completes a removal the session's Envoy interest no
// longer holds (the interest is gone, or none of its topics match this owner):
// 404 SUBSCRIBER_NOT_FOUND when no request is pending, otherwise the
// subscription.removed event and 204.
func (s *server) finishPendingRemoval(w http.ResponseWriter, r *http.Request, own owner, sessionID string, actor model.Actor) {
	requestID, recordErr := s.pendingSubscriptionRemoval(r.Context(), s.deps.Store.Pool, own, sessionID, nil)
	if recordErr != nil {
		s.writeHandlerError(w, recordErr)
		return
	}
	if requestID == nil {
		writeSubscriberNotFound(w)
		return
	}
	event, completeErr := s.completeSubscriptionRemoval(r.Context(), own, sessionID, *requestID, actor)
	if completeErr != nil {
		s.writeHandlerError(w, completeErr)
		return
	}
	if event != nil {
		s.publish(r.Context(), *event)
	}
	w.WriteHeader(http.StatusNoContent)
}

// unsubscribeSession removes the session the path names from own's topics, base being their root.
// A session the caller may not see (sessionHiddenFromReader, access.SessionClause) is a session
// nobody holds: it is answered 404 SUBSCRIBER_NOT_FOUND, as one the listener has no interest of
// and no removal pending for is, before the listener is asked anything, a lock is taken or a
// removal is read. While a project is private, and in every deployment for a caller that is not
// open, a session nobody holds is one the caller does not see either, so the two take this one
// path. An open caller of a deployment that holds no private project sees every session and reads
// nothing for it (R9).
func (s *server) unsubscribeSession(
	w http.ResponseWriter,
	r *http.Request,
	actor model.Actor,
	own owner,
	base string,
) {
	if s.deps.Envoy == nil {
		writeError(w, "ENVOY_UNAVAILABLE", http.StatusServiceUnavailable, "ENVOY_URL is not configured")
		return
	}
	sessionID := r.PathValue("session_id")
	hidden, err := s.sessionHiddenFromReader(r.Context(), sessionID)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if hidden {
		writeSubscriberNotFound(w)
		return
	}
	interest, err := s.deps.Envoy.Interest(r.Context(), sessionID)
	if err != nil {
		if errors.Is(err, envoy.ErrNotFound) {
			s.finishPendingRemoval(w, r, own, sessionID, actor)
			return
		}
		s.writeEnvoyError(w, err)
		return
	}
	matched := matchingTopics(interest.Topics, base)
	if len(matched) == 0 {
		s.finishPendingRemoval(w, r, own, sessionID, actor)
		return
	}
	// Only a topic scoped exactly to this owner is safe to remove: a broader
	// wildcard among the session's topics (e.g. "notifications.dispatch.>")
	// also covers every other issue and document, so removing it here would
	// silently unsubscribe the session everywhere else too.
	exact := exactTopics(matched, base)
	if len(exact) == 0 {
		writeError(
			w, "NOT_REMOVABLE", http.StatusConflict,
			"session is subscribed only via a broader topic ("+matched[0]+"); removing it would affect every other issue and document",
		)
		return
	}

	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	if err := s.deps.Events.LockOwners(r.Context(), tx, own.event("", actor, nil)); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	requestID, err := s.pendingSubscriptionRemoval(r.Context(), tx, own, sessionID, exact)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if requestID == nil {
		request, err := s.appendEvent(r.Context(), tx, own.event(
			"subscription.remove_requested",
			actor,
			subscriptionRemovedPayload{SessionID: sessionID, By: actor, Topics: exact, Pending: true},
		))
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		requestID = &request.ID
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if _, err := s.deps.Envoy.Unsubscribe(r.Context(), sessionID, exact); err != nil {
		writeError(w, "ENVOY_UNSUBSCRIBE_FAILED", http.StatusBadGateway, err.Error())
		return
	}
	event, err := s.completeSubscriptionRemoval(r.Context(), own, sessionID, *requestID, actor)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if event != nil {
		s.publish(r.Context(), *event)
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeSubscriberNotFound answers an unsubscribe of a session with no interest in the owner and no
// removal pending for it, and one the caller may not see (unsubscribeSession), in the same words.
func writeSubscriberNotFound(w http.ResponseWriter) {
	writeError(w, "SUBSCRIBER_NOT_FOUND", http.StatusNotFound, "session is not subscribed")
}

// writeEnvoyError answers a listener failure: the listener's refilling answer as 503
// ENVOY_REFILLING (writeEnvoyRefilling), any other ErrUnavailable as 503 ENVOY_UNAVAILABLE.
func (s *server) writeEnvoyError(w http.ResponseWriter, err error) {
	var refilling *envoy.RefillingError
	if errors.As(err, &refilling) {
		writeEnvoyRefilling(w, refilling.RetryAfter)
		return
	}
	if errors.Is(err, envoy.ErrUnavailable) {
		writeError(w, "ENVOY_UNAVAILABLE", http.StatusServiceUnavailable, err.Error())
		return
	}
	s.writeHandlerError(w, err)
}

// envoyRefillingRetryAfter is the Retry-After a refilling answer gives when the listener sent
// none: the listener's own (cmd/listener listRefillingRetryAfter).
const envoyRefillingRetryAfter = "5"

// writeEnvoyRefilling answers a read that needed the listener's agent list while the listener
// refused it as refilling (envoy.ErrRefilling): 503 ENVOY_REFILLING with the listener's
// Retry-After passed through. Its own code, apart from ENVOY_UNAVAILABLE, because the listener
// did answer and said the list will be back in a moment; either way a client must not read the
// missing list as "no agents".
func writeEnvoyRefilling(w http.ResponseWriter, retryAfter string) {
	w.Header().Set("Retry-After", cmp.Or(retryAfter, envoyRefillingRetryAfter))
	writeError(w, "ENVOY_REFILLING", http.StatusServiceUnavailable, "The agent list is refilling. Try again in a moment.")
}

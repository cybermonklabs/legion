package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/asks"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// Ask followers: the sessions an ask's answer, edits, resolution, and replies reach
// directly. Every session that writes to an ask follows it (asks.FollowAuthor at each
// insert site); these routes let a session leave or rejoin and a person curate the list.
//
// A credential acting as the addressed session — the session a broker-issued token proved,
// else the one its body names — follows or unfollows that session as it always did, and the
// write is recorded as that session acting for its owner: its own act is no person's. Acting on
// any other session is the person's act, which needs the `person` scope: a person's browser
// cookie, or a token of theirs that holds it, may add or remove any session and sends no body
// of its own, and the write is recorded as the person with the agent beside them. Every other
// credential is refused that, 403 FOLLOWER_FORBIDDEN.

func parseAskID(r *http.Request) (string, error) {
	id := r.PathValue("id")
	if _, err := uuid.Parse(id); err != nil {
		return "", errorf(apicode.AskIDInput, "%s", idPathProblem("ask"))
	}
	return id, nil
}

// loadAskOwner returns the owner of an ask (its issue or unlinked document) or
// pgx.ErrNoRows when the ask does not exist.
func (s *server) loadAskOwner(ctx context.Context, q queryer, askID string) (owner, error) {
	var issueKey, artifactID *string
	if err := q.QueryRow(ctx, `
		select issue_key, artifact_id::text from asks where id = $1
	`, askID).Scan(&issueKey, &artifactID); err != nil {
		return owner{}, err
	}
	return ownerOf(issueKey, artifactID), nil
}

// askFollowersRead is the body GET /api/v1/asks/{id}/followers answers.
type askFollowersRead struct {
	Followers []model.AskFollower `json:"followers"`
}

// GET /api/v1/asks/{id}/followers  (any authenticated actor)
func (s *server) listAskFollowers(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	askID, err := parseAskID(r)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	if _, err := s.loadAskOwner(r.Context(), s.deps.Store.Pool, askID); err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	followers, err := asks.Followers(r.Context(), s.deps.Store.Pool, askID)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, askFollowersRead{Followers: followers})
}

// PUT /api/v1/asks/{id}/followers/{session_id}
func (s *server) followAsk(w http.ResponseWriter, r *http.Request) {
	s.changeAskFollower(w, r, true)
}

// DELETE /api/v1/asks/{id}/followers/{session_id}
func (s *server) unfollowAsk(w http.ResponseWriter, r *http.Request) {
	s.changeAskFollower(w, r, false)
}

func (s *server) changeAskFollower(w http.ResponseWriter, r *http.Request, follow bool) {
	askID, err := parseAskID(r)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	sessionID := strings.TrimSpace(r.PathValue("session_id"))
	if sessionID == "" {
		writeError(w, r, apicode.FollowerInput, "follower session id is required")
		return
	}
	actor, human, err := s.optionalActor(r)
	if err != nil {
		s.writeAuthenticationError(w, r, err)
		return
	}
	// A person at a browser writes as themselves: the credential optionalActor resolved is the
	// request's, not the record's, and the bearer paths below take their actor from
	// bearerSessionActor and actingPerson, which record none either.
	actor = actor.Recorded()
	if !human {
		var input actorInput
		// A credential that holds no `person` scope acts only as a session, which the body has
		// to name, so the body is required of it exactly as it always was. A person's own
		// credential may send none: it acts as the person, and the body only names the agent
		// beside them.
		person := s.callerHoldsScope(r, model.ScopePerson)
		if person {
			if !s.decodeOptionalJSON(w, r, &input) {
				return
			}
		} else if err := decodeJSON(r, &input); err != nil {
			s.writeHandlerError(w, r, err)
			return
		}
		var ok bool
		if actor, ok = s.askFollowerActor(w, r, actor, person, sessionID, input.Actor); !ok {
			return
		}
	}
	if !s.writeAskFollower(w, r, actor, askID, sessionID, follow, nil) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// askFollowerActor is whom a follower write by a credential that is no person at a browser is
// recorded as, from the actor it resolved to and the session its body names: the addressed session
// itself, or a person holding the person scope acting on another session, or nobody, which it
// refuses itself and answers false. person is whether the credential holds the person scope.
func (s *server) askFollowerActor(
	w http.ResponseWriter, r *http.Request, actor model.Actor, person bool, sessionID string, named *model.Actor,
) (model.Actor, bool) {
	// The session this credential acts as, where it acts as one, is bearerSessionActor's own
	// answer: the session a broker-issued token proved, else the one the body names, as that call
	// records it. A credential that acts as the addressed session follows it as it always did,
	// recorded as that session, since that is its own act and no person's; nothing of what a
	// session reads back changes.
	asTheSession, sessionErr := bearerSessionActor(r, actor, named)
	switch {
	case sessionErr == nil && asTheSession.ID == sessionID:
		return asTheSession, true
	case person:
		// Putting another session on an ask is the person's act, so it is recorded as the person
		// with the agent beside them. actingPerson cannot refuse the scope here, which the caller
		// has just read; it still refuses a `?session=` naming a session other than the one a
		// broker-issued token proved.
		acting, ok := s.actingPerson(w, r)
		if !ok {
			return model.Actor{}, false
		}
		return actSessionFromBody(w, r, acting, named)
	default:
		// The body still has to name a session, so a bearer that names none is told that first,
		// as it always was (400 ACTOR_KIND).
		if sessionErr != nil {
			s.writeHandlerError(w, r, sessionErr)
			return model.Actor{}, false
		}
		writeError(w, r, apicode.FollowerForbidden,
			"a session may follow or unfollow only itself; acting for another session needs the person scope")
		return model.Actor{}, false
	}
}

// writeAskFollower puts sessionID on the ask askID's followers, or with follow false takes it off,
// recorded as actor: one ask.follower_added or ask.follower_removed event for a change, and none
// for a follow of a session that already follows, which changes nothing. Unfollowing a session
// that does not follow is 404 FOLLOWER_NOT_FOUND. Both versions of the follower routes write
// through it and answer in their own shape; it writes a refusal itself and answers false. readBack
// reads, on the write's own transaction and before it commits, what a /api/v2 route answers, so a
// read that fails rolls the write back (askReadBack); /api/v1's routes read nothing back (nil).
func (s *server) writeAskFollower(
	w http.ResponseWriter, r *http.Request, actor model.Actor, askID, sessionID string, follow bool,
	readBack func(ctx context.Context, q queryer) error,
) bool {
	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, r, err)
		return false
	}
	defer tx.Rollback(r.Context())
	own, err := s.loadAskOwner(r.Context(), tx, askID)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return false
	}
	if err := s.deps.Events.LockOwners(r.Context(), tx, own.event("", actor, nil)); err != nil {
		s.writeHandlerError(w, r, err)
		return false
	}
	eventType := "ask.follower_added"
	var changed bool
	if follow {
		changed, err = asks.Follow(r.Context(), tx, askID, sessionID)
	} else {
		eventType = "ask.follower_removed"
		changed, err = asks.Unfollow(r.Context(), tx, askID, sessionID)
	}
	if err != nil {
		s.writeHandlerError(w, r, err)
		return false
	}
	if !changed {
		if !follow {
			writeError(w, r, apicode.FollowerNotFound, "session does not follow this ask")
			return false
		}
		return s.readFollowerBack(w, r, tx, readBack)
	}
	event, err := s.appendEvent(r.Context(), tx, own.event(eventType, actor, model.AskFollowerEventPayload{
		AskID: askID, SessionID: sessionID, By: actor,
	}))
	if err != nil {
		s.writeHandlerError(w, r, err)
		return false
	}
	if !s.readFollowerBack(w, r, tx, readBack) {
		return false
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, r, err)
		return false
	}
	s.publish(r.Context(), event)
	return true
}

// readFollowerBack runs a follower write's readBack, where it has one, on its transaction tx,
// writing its failure and answering false.
func (s *server) readFollowerBack(w http.ResponseWriter, r *http.Request, tx queryer, readBack func(ctx context.Context, q queryer) error) bool {
	if readBack == nil {
		return true
	}
	if err := readBack(r.Context(), tx); err != nil {
		s.writeHandlerError(w, r, err)
		return false
	}
	return true
}

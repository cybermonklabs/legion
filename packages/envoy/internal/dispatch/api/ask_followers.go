package api

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/dispatch/asks"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// Ask followers: the sessions an ask's answer, edits, resolution, and replies reach
// directly. Every session that writes to an ask follows it (asks.PlanFollowAuthor at each
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
		return "", errorf(http.StatusBadRequest, "ASK_ID_INPUT", "%s", idPathProblem("ask"))
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
		s.writeHandlerError(w, err)
		return
	}
	if _, err := s.loadAskOwner(r.Context(), s.deps.Store.Pool, askID); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	followers, err := asks.Followers(r.Context(), s.deps.Store.Pool, askID)
	if err != nil {
		s.writeHandlerError(w, err)
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
		s.writeHandlerError(w, err)
		return
	}
	sessionID := strings.TrimSpace(r.PathValue("session_id"))
	if sessionID == "" {
		writeError(w, "FOLLOWER_INPUT", http.StatusBadRequest, "follower session id is required")
		return
	}
	actor, human, err := s.optionalActor(r)
	if err != nil {
		s.writeAuthenticationError(w, err)
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
			s.writeHandlerError(w, err)
			return
		}
		// The session this credential acts as, where it acts as one, is bearerSessionActor's
		// own answer: the session a broker-issued token proved, else the one the body names, as
		// that call records it. A credential that acts as the addressed session follows it as it
		// always did, recorded as that session, since that is its own act and no person's;
		// nothing of what a session reads back changes.
		asTheSession, sessionErr := bearerSessionActor(r, actor, input.Actor)
		switch {
		case sessionErr == nil && asTheSession.ID == sessionID:
			actor = asTheSession
		case person:
			// Putting another session on an ask is the person's act, so it is recorded as the
			// person with the agent beside them. actingPerson cannot refuse the scope here,
			// which the line above has just read; it still refuses a `?session=` naming a
			// session other than the one a broker-issued token proved.
			acting, ok := s.actingPerson(w, r)
			if !ok {
				return
			}
			if actor, ok = actSessionFromBody(w, acting, input.Actor); !ok {
				return
			}
		default:
			// The body still has to name a session, so a bearer that names none is told that
			// first, as it always was (400 ACTOR_KIND).
			if sessionErr != nil {
				s.writeHandlerError(w, sessionErr)
				return
			}
			writeError(w, "FOLLOWER_FORBIDDEN", http.StatusForbidden,
				"a session may follow or unfollow only itself; acting for another session needs the person scope")
			return
		}
	}

	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	own, err := s.loadAskOwner(r.Context(), tx, askID)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := s.deps.Events.LockOwners(r.Context(), tx, own.event("", actor, nil)); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	eventType := "ask.follower_added"
	var changed bool
	// What the change states on the ask's node, held for the event that records it, below: the
	// follower's node and the followed_by the follow states, or the retraction the unfollow does.
	var held graphstore.Uncaused
	if follow {
		// The follower is a node of the store from this write on, made before the event that
		// records the follow exists and moved onto it below.
		var followed asks.Followed
		followed, err = asks.Follow(r.Context(), tx, askID, sessionID, actor, time.Now(), graphstore.NoEvent)
		changed, held = followed.Added, followed.Uncaused
	} else {
		eventType = "ask.follower_removed"
		var unfollowed asks.Unfollowed
		unfollowed, err = asks.Unfollow(r.Context(), tx, askID, sessionID, actor, time.Now().UTC())
		changed, held = unfollowed.Removed, unfollowed.Uncaused
	}
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if !changed {
		if !follow {
			writeError(w, "FOLLOWER_NOT_FOUND", http.StatusNotFound, "session does not follow this ask")
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// The change's node facts wait for the event that records it: the two are one batch, the
	// facts appended right after the event, under its id (appendCausingEvent), and after them the
	// mark of the queries of the ask's issue owed (docs.QueueAskQueryMark): followed_by is a
	// relation of the ask's node, which the decisions query's record holds of a question it takes,
	// so R14 owes that query a version, which the drain writes once the batch has committed.
	var marked string
	appended, err := s.appendCausingEvent(r.Context(), tx, own.event(eventType, actor, model.AskFollowerEventPayload{
		AskID: askID, SessionID: sessionID, By: actor,
	}), held, func(b *store.WriteBatch) { docs.QueueAskQueryMark(b, askID, &marked) })
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	published := []model.Event{appended}
	if marked != "" {
		published = append(published, s.deps.Docs.DrainOwedQueries(r.Context(), marked)...)
	}
	s.publish(r.Context(), published...)
	w.WriteHeader(http.StatusNoContent)
}

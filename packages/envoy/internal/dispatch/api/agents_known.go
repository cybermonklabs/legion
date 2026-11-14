package api

import (
	"fmt"
	"maps"
	"net/http"
	"slices"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// knownSession is one session GET /api/v1/agents/known found a record of. Title is the title the
// registry copy holds for it, and null where that copy does not list the session or lists it
// with no title: no table of Dispatch's own keeps a session's title.
type knownSession struct {
	SessionID string  `json:"session_id"`
	Title     *string `json:"title"`
}

// knownSessions is GET /api/v1/agents/known's answer: the asked sessions Dispatch has any record
// of, in the order they were asked, each once.
type knownSessions struct {
	Sessions []knownSession `json:"sessions"`
}

// knownSessionsQuery answers, in one statement, which of the session ids $1 names Dispatch holds
// any record of, in the order of $1, with the title the registry copy holds. Each record is read
// through an index keyed by the session, so a batch of a hundred ids costs a hundred probes of
// each index and never a scan of a table that grows with use:
//
//   - the registry copy: the route_roster row's sessions, the listener's last list of live and
//     unresponsive sessions that GET /api/v1/agents lists (migration 0159), read whatever its age,
//     since a session it ever listed existed. It is one row of a few hundred entries, unnested
//     once.
//   - agent activity: an event a session wrote (events_session_actor_created_at_idx, 0026), which
//     is every ask, comment, reply, claim, document version and handoff a session made.
//   - the conversation's roots: a message targeted at the session (messages_session_roots, 0051).
//   - message deliveries: an attempt to deliver a message to the session, a broadcast copy among
//     them (message_deliveries_session, 0051).
//   - comment deliveries: a mention delivered to the session (comment_deliveries_session, 0146).
//   - the conversation's files and images (artifacts_session_id, 0071).
//   - ask followers: an ask the session follows, whose answers Dispatch delivers to it
//     (ask_followers_session_id).
//
// An answer delivered to a session is one of these: an ask's answer reaches the ask's followers,
// a comment its mentions and a message its deliveries. The events whose published_destinations
// name the session's topic are not read, because no index serves that array and every such event
// reached the session through one of the rows above. A person's own marks on a conversation
// (user_agent_state, user_agent_read, user_agent_reply_read) are not counted either: they are one
// viewer's, and a person may write one for any id at all.
//
// A session whose only trace is a live registration younger than the stored copy (at most
// routeRosterTTL old) has no record here yet; a caller asks this route only for the sessions the
// live registry, GET /api/v1/agents, does not list.
const knownSessionsQuery = `
	with listed as (
		select distinct on (entry->>'session_id')
		       entry->>'session_id' as id, nullif(entry->>'title', '') as title
		from route_roster roster, jsonb_array_elements(roster.sessions) entry
		order by entry->>'session_id'
	)
	select asked.id, listed.title
	from unnest($1::text[]) with ordinality as asked (id, position)
	left join listed on listed.id = asked.id
	where listed.id is not null
	   or exists (select 1 from events e
	              where e.actor->>'kind' = 'session' and e.actor->>'id' = asked.id)
	   or exists (select 1 from messages m
	              where m.in_reply_to is null and m.target = 'session:' || asked.id)
	   or exists (select 1 from message_deliveries d where d.session_id = asked.id)
	   or exists (select 1 from comment_deliveries c where c.session_id = asked.id)
	   or exists (select 1 from artifacts a where a.session_id = asked.id)
	   or exists (select 1 from ask_followers f where f.session_id = asked.id)
	order by asked.position`

// listKnownSessions answers which of the sessions a page references Dispatch holds any record of,
// so a dispatch://agent/<session id> reference links where its conversation exists - an ended
// session's included, which the live registry no longer lists - and not where no agent ever held
// the id. A page asks once for every such reference it holds (knownSessionsQuery reads them in
// one statement), naming each in `session`, 1 to contracts.KnownSessionsMax times.
//
// It answers only what its caller could already learn, so it admits exactly the callers GET
// /api/v1/agents/{session_id}/messages admits: the `person` scope its row names (routeScopes),
// and personCredential, the credential checks actingPerson makes there. That read already tells
// such a caller whether a session has a conversation, one id per request.
//
// Every parameter is held to its rule before anything is read, each refusal 400 INVALID_QUERY
// naming the parameter, as the search routes refuse theirs: a parameter other than `session`, no
// `session` at all, more than contracts.KnownSessionsMax of them (counted as sent, duplicates
// included), and a value that is no session id by text.IsSessionID, the rule every route that
// takes a session id applies.
func (s *server) listKnownSessions(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.personCredential(w, r); !ok {
		return
	}
	query := r.URL.Query()
	for _, key := range slices.Sorted(maps.Keys(query)) {
		if key != "session" {
			writeError(w, "INVALID_QUERY", http.StatusBadRequest, fmt.Sprintf(
				"%s is not a query parameter of GET /api/v1/agents/known, which takes session alone", key))
			return
		}
	}
	asked := query["session"]
	switch {
	case len(asked) == 0:
		writeError(w, "INVALID_QUERY", http.StatusBadRequest, fmt.Sprintf(
			"session is required: name each session id asked about in a session parameter of its own, 1 to %d of them",
			contracts.KnownSessionsMax))
		return
	case len(asked) > contracts.KnownSessionsMax:
		writeError(w, "INVALID_QUERY", http.StatusBadRequest, fmt.Sprintf(
			"session names at most %d session ids (%d named)", contracts.KnownSessionsMax, len(asked)))
		return
	}
	ids := make([]string, 0, len(asked))
	for _, id := range asked {
		if !text.IsSessionID(id) {
			writeError(w, "INVALID_QUERY", http.StatusBadRequest, fmt.Sprintf(
				"session %q is not a session id: one is not empty and holds no whitespace, no control "+
					"character and none of the characters that end a reference in text", id))
			return
		}
		if !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}

	rows, err := s.deps.Store.Pool.Query(r.Context(), knownSessionsQuery, ids)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer rows.Close()
	answer := knownSessions{Sessions: []knownSession{}}
	for rows.Next() {
		var session knownSession
		if err := rows.Scan(&session.SessionID, &session.Title); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		answer.Sessions = append(answer.Sessions, session)
	}
	if err := rows.Err(); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, answer)
}

package api

import (
	"cmp"
	"context"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"slices"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/envoy"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

type agentResponse struct {
	envoy.Session
	OpenAsks     int     `json:"open_asks"`
	LastActivity *string `json:"last_activity"`
	// LastActivityIssue and LastActivityType are where the session's newest event, the one
	// LastActivity dates, is: the issue it belongs to, absent for an event of a document that is
	// on no issue, and its type, such as comment.created or message.created; both absent where
	// the session has no event.
	LastActivityIssue *string `json:"last_activity_issue,omitempty"`
	LastActivityType  *string `json:"last_activity_type,omitempty"`
}

func (s *server) agentOpenAskCounts(ctx context.Context, sessionIDs []string) (map[string]int, error) {
	rows, err := s.deps.Store.Pool.Query(ctx, `
		select a.author->>'id', count(*)::int
		from asks a
		left join issues i on i.key = a.issue_key
		where a.state = 'open'
			and a.author->>'kind' = 'session'
			and a.author->>'id' = any($1::text[])
			and (i.key is null or i.closed_at is null)
		group by a.author->>'id'
	`, sessionIDs)
	if err != nil {
		return nil, fmt.Errorf("query agent open asks: %w", err)
	}
	defer rows.Close()

	counts := make(map[string]int)
	for rows.Next() {
		var sessionID string
		var count int
		if err := rows.Scan(&sessionID, &count); err != nil {
			return nil, fmt.Errorf("scan agent open asks: %w", err)
		}
		counts[sessionID] = count
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate agent open asks: %w", err)
	}
	return counts, nil
}

// agentLastActivityQuery reads one session's newest event through
// events_session_actor_created_at_idx, ((actor->>'id'), created_at desc) where the actor is a
// session, and does that once for each session asked for: an ordered read that stops at its
// first row. Grouping max(created_at) instead reads every event each session ever wrote to keep
// one of them — 1,875 rows and 470 buffers to answer five sessions on the measurement database,
// against 20 buffers here — because Postgres has no index skip scan. The lateral repeats
// actor->>'kind' = 'session' because that index is partial on it, and it is joined with `cross
// join`, not a left join: a session with no event is a session the caller's map must not hold,
// which is what the grouped read answered by returning it no row. Beside its time it answers the
// event's issue and type, which say where that activity is.
const agentLastActivityQuery = `
		select s.id, e.created_at, e.issue_key, e.type
		from unnest($1::text[]) s(id)
		cross join lateral (
			select created_at, issue_key, type
			from events
			where actor->>'kind' = 'session' and actor->>'id' = s.id
			order by created_at desc
			limit 1
		) e
	`

// agentActivity is one session's newest event: when it was written, the issue it belongs to, nil
// for an event of a document on no issue, and its type.
type agentActivity struct {
	at    time.Time
	issue *string
	kind  string
}

func (s *server) agentLastActivity(ctx context.Context, sessionIDs []string) (map[string]agentActivity, error) {
	rows, err := s.deps.Store.Pool.Query(ctx, agentLastActivityQuery, sessionIDs)
	if err != nil {
		return nil, fmt.Errorf("query agent activity: %w", err)
	}
	defer rows.Close()

	activity := make(map[string]agentActivity)
	for rows.Next() {
		var sessionID string
		var newest agentActivity
		if err := rows.Scan(&sessionID, &newest.at, &newest.issue, &newest.kind); err != nil {
			return nil, fmt.Errorf("scan agent activity: %w", err)
		}
		activity[sessionID] = newest
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate agent activity: %w", err)
	}
	return activity, nil
}

// listAgents is readable by any authenticated caller: a session picks a target by the
// capabilities it advertises. Sending to a session without an issue needs the person scope
// (routeScopes), since it writes to someone else's conversation. The list is
// the stored copy of the listener's sessions every process shares (readRouteRoster, the
// route_roster row), at most routeRosterTTL (5 s) old, so one listener call serves agents lists
// and issue reads alike; a listener that did not answer answers 503 for as long as that copy is a
// silence, a request that ends while it waits on the shared fetch answers 503 saying so, and a
// failed read of the stored copy answers as the database's error. Once the process is shutting
// down, a list the drain still serves asks the listener on its own request instead, since the
// shared fetch runs on the process's lifetime and can no longer answer.
//
// The copy carries the unresponsive sessions as well as the live ones, each row with its own
// `state`, so the Agents page shows a session that missed its heartbeats as stalled rather than
// dropping it: it is still working, still holds its claims, and is still a target a reader may
// pick (dispatch://INFRA-392).
//
// `limit` answers the newest seen sessions alone, as many as it names, and reads the open asks
// and the last activity of those alone (agentRows); the command bar's roster read sends it, so
// a run's read of a roster of any size loads no more rows than it answers.
func (s *server) listAgents(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	limit, err := parseAgentListLimit(r.URL.Query())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if s.deps.Envoy == nil {
		writeError(w, "ENVOY_UNAVAILABLE", http.StatusServiceUnavailable, "ENVOY_URL is not configured")
		return
	}

	roster, err := s.agentRoster(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if !roster.answered {
		// The roster's reason is the one every process reads, stored on the row beside the
		// answer, so a refill one process met answers ENVOY_REFILLING here too, with the
		// Retry-After the listener sent, never the unreachable listener dispatch://INFRA-261
		// forbids (listIssues refuses a judged filter the same way).
		if roster.reason == model.RouteReasonRefilling {
			writeEnvoyRefilling(w, roster.retryAfter)
			return
		}
		s.writeEnvoyError(w, roster.err)
		return
	}
	rows, err := s.agentRows(r.Context(), roster.sessions, limit)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, rows)
}

// agentRoster is the roster the agents list and the going run's roster read serve: the stored
// copy every process shares (readRouteRoster), or, once the process is shutting down and that
// copy did not answer, the listener asked on ctx. The shared fetch runs on the process's
// lifetime, so it has ended or failed at once and its silence says nothing about the listener; a
// request the drain still serves asks the listener on its own context instead, as every agents
// list did before the copy existed. With no listener configured there is none to ask, and the
// unanswered copy stands.
func (s *server) agentRoster(ctx context.Context) (routeRoster, error) {
	roster, err := s.readRouteRoster(ctx)
	if err != nil {
		return routeRoster{}, err
	}
	if !roster.answered && s.lifetime.Err() != nil && s.deps.Envoy != nil {
		roster = s.listRouteRoster(ctx)
	}
	return roster, nil
}

// agentRows is the agents list's rows for the listed sessions: newest seen first, at most limit
// of them (every one for a limit of 0), each with its open asks and its last activity, when and
// where it is, the machine standing in for a host a listener left out. Only the sessions it
// answers are looked up, so a limit bounds the rows its two reads load as it bounds the rows it
// answers. GET /api/v1/agents and the going run's roster read (going_reads.go) answer through it
// alike.
func (s *server) agentRows(ctx context.Context, listed []envoy.Session, limit int) ([]agentResponse, error) {
	sessions := newestSessions(listed, limit)
	if len(sessions) == 0 {
		// Nothing to look up: both aggregates would bind an empty id list and return no rows.
		return []agentResponse{}, nil
	}
	sessionIDs := make([]string, len(sessions))
	for index, session := range sessions {
		sessionIDs[index] = session.SessionID
	}
	openAsks, err := s.agentOpenAskCounts(ctx, sessionIDs)
	if err != nil {
		return nil, err
	}
	activity, err := s.agentLastActivity(ctx, sessionIDs)
	if err != nil {
		return nil, err
	}

	rows := make([]agentResponse, 0, len(sessions))
	for _, session := range sessions {
		row := agentResponse{OpenAsks: openAsks[session.SessionID]}
		if newest, ok := activity[session.SessionID]; ok {
			row.LastActivity, row.LastActivityIssue, row.LastActivityType = timestampPtr(&newest.at), newest.issue, &newest.kind
		}
		// A listener that predates the "host" field (packages/envoy/internal/session's
		// SessionEntry.Host) answers with none; fall back to the routing machine_id rather
		// than showing an empty machine on the Agents page during a partial rollout.
		session.Host = cmp.Or(session.Host, session.MachineID)
		row.Session = session
		rows = append(rows, row)
	}
	return rows, nil
}

// agentListLimitMost is the most sessions a `limit` of GET /api/v1/agents may name.
const agentListLimitMost = math.MaxInt32

// parseAgentListLimit reads GET /api/v1/agents' `limit`: how many of the newest seen sessions the
// list answers, 0 where the caller named none and the list answers every session.
func parseAgentListLimit(query url.Values) (int, error) {
	limit, present, valid := parseQueryInt(query, "limit", 1, agentListLimitMost)
	if present && !valid {
		return 0, errorf(http.StatusBadRequest, "INVALID_QUERY", "limit must be one integer from 1 to %d", agentListLimitMost)
	}
	return limit, nil
}

// newestSessions is a copy of the listed sessions, newest seen first and then by id, at most
// limit of them (every one for a limit of 0). The listed sessions are every read's (routeRoster),
// so it orders a copy; a limit below the list's length keeps only the newest as it reads the list,
// in a slice of the limit's size, so a roster of any size copies limit sessions.
func newestSessions(listed []envoy.Session, limit int) []envoy.Session {
	newer := func(left, right envoy.Session) int {
		if order := cmp.Compare(right.LastSeen, left.LastSeen); order != 0 {
			return order
		}
		return cmp.Compare(left.SessionID, right.SessionID)
	}
	if limit <= 0 || limit >= len(listed) {
		sessions := slices.Clone(listed)
		slices.SortStableFunc(sessions, newer)
		return sessions
	}
	kept := make([]envoy.Session, 0, limit+1)
	for _, session := range listed {
		if len(kept) == limit && newer(session, kept[limit-1]) >= 0 {
			continue
		}
		at, _ := slices.BinarySearchFunc(kept, session, newer)
		kept = slices.Insert(kept, at, session)
		if len(kept) > limit {
			kept = kept[:limit]
		}
	}
	return kept
}

package api

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/envoy"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

type agentResponse struct {
	envoy.Session
	OpenAsks     int     `json:"open_asks"`
	LastActivity *string `json:"last_activity"`
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

// visibleRosterSessions is the one place that decides which of the roster's sessions a reader
// reads (K6), in their order: those SessionClause passes on their id, in one read. That is the
// reader's own session, a session with history in a project the reader sees (session_projects)
// and none in a private project it does not see, and a session with no history judged by the
// projects its listener record names, which the clause reads from the stored roster this list was
// read from (route_roster_session_projects): for an open viewer, every project the record names is
// one it sees, none included, and for a viewer that is not open, the record names a project and
// every one it names is one the viewer sees. While a project is private, a session whose record
// names no list (the listener sent no projects key) is one nothing Dispatch holds places, and it
// reaches no reader but its own. The list decides a row and is served on none (agentResponse). A
// reader that sees every object (seesEveryObject) passes every session, so listAgents does not ask
// (R9).
func (s *server) visibleRosterSessions(ctx context.Context, viewer any, sessions []envoy.Session) ([]envoy.Session, error) {
	ids := make([]string, len(sessions))
	for index, session := range sessions {
		ids[index] = session.SessionID
	}
	seen, err := s.sessionsSeen(ctx, viewer, ids)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(sessions, func(session envoy.Session) bool { return !seen[session.SessionID] }), nil
}

// sessionsSeen is which of ids pass SessionClause for the reader whose visibility argument is
// viewer, in one read.
func (s *server) sessionsSeen(ctx context.Context, viewer any, ids []string) (map[string]bool, error) {
	seen := map[string]bool{}
	if len(ids) == 0 {
		return seen, nil
	}
	rows, err := s.deps.Store.Pool.Query(ctx, `
		select candidate.id from unnest($1::text[]) candidate(id)
		where `+access.SessionClause("candidate.id", "$2"), ids, viewer)
	if err != nil {
		return nil, fmt.Errorf("read which sessions the caller sees: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan a session the caller sees: %w", err)
		}
		seen[id] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read which sessions the caller sees: %w", err)
	}
	return seen, nil
}

// agentLastActivityQuery reads one session's newest event through
// events_session_actor_created_at_idx, ((actor->>'id'), created_at desc) where the actor is a
// session, and does that once for each session asked for: an ordered read that stops at its
// first row. Grouping max(created_at) instead reads every event each session ever wrote to keep
// one of them — 1,875 rows and 470 buffers to answer five sessions on the measurement database,
// against 20 buffers here — because Postgres has no index skip scan. The lateral repeats
// actor->>'kind' = 'session' because that index is partial on it, and it is joined with `cross
// join`, not a left join: a session with no event is a session the caller's map must not hold,
// which is what the grouped read answered by returning it no row.
const agentLastActivityQuery = `
		select s.id, e.created_at
		from unnest($1::text[]) s(id)
		cross join lateral (
			select created_at
			from events
			where actor->>'kind' = 'session' and actor->>'id' = s.id
			order by created_at desc
			limit 1
		) e
	`

func (s *server) agentLastActivity(ctx context.Context, sessionIDs []string) (map[string]time.Time, error) {
	rows, err := s.deps.Store.Pool.Query(ctx, agentLastActivityQuery, sessionIDs)
	if err != nil {
		return nil, fmt.Errorf("query agent activity: %w", err)
	}
	defer rows.Close()

	activity := make(map[string]time.Time)
	for rows.Next() {
		var sessionID string
		var createdAt time.Time
		if err := rows.Scan(&sessionID, &createdAt); err != nil {
			return nil, fmt.Errorf("scan agent activity: %w", err)
		}
		activity[sessionID] = createdAt
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
func (s *server) listAgents(w http.ResponseWriter, r *http.Request) {
	reader, ok := s.readerOf(w, r)
	if !ok {
		return
	}
	if s.deps.Envoy == nil {
		writeError(w, "ENVOY_UNAVAILABLE", http.StatusServiceUnavailable, "ENVOY_URL is not configured")
		return
	}

	roster, err := s.readRouteRoster(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if !roster.answered && s.lifetime.Err() != nil {
		// The process is shutting down: the shared fetch runs on its lifetime, so it has ended or
		// failed at once, and its silence says nothing about the listener. A request the drain
		// still serves asks the listener on its own context instead, as every agents list did
		// before the copy existed.
		roster = s.listRouteRoster(r.Context())
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
	if len(roster.sessions) == 0 {
		// Nothing to look up: both aggregates would bind an empty id list and return no rows.
		WriteJSON(w, http.StatusOK, []agentResponse{})
		return
	}
	// The roster's list is every read's, so the sort orders a copy of it.
	sessions := slices.Clone(roster.sessions)
	sort.SliceStable(sessions, func(left, right int) bool {
		if sessions[left].LastSeen != sessions[right].LastSeen {
			return sessions[left].LastSeen > sessions[right].LastSeen
		}
		return sessions[left].SessionID < sessions[right].SessionID
	})
	viewer := refs.VisibilityArg(reader)
	// A reader that sees every object passes SessionClause on every id, so the read would cut
	// nothing (R9).
	if !seesEveryObject(reader) {
		if sessions, err = s.visibleRosterSessions(r.Context(), viewer, sessions); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		// A session the reader may read can still hold a role of a project it does not see,
		// whose name spells that project's key (contracts.RoleProject).
		if err = leaveOutHiddenSessionRoles(r.Context(), s.deps.Store.Pool, reader, sessions); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	sessionIDs := make([]string, len(sessions))
	for index, session := range sessions {
		sessionIDs[index] = session.SessionID
	}
	openAsks, err := s.agentOpenAskCounts(r.Context(), sessionIDs)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	activity, err := s.agentLastActivity(r.Context(), sessionIDs)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}

	rows := make([]agentResponse, 0, len(sessions))
	for _, session := range sessions {
		var lastActivity *string
		if createdAt, ok := activity[session.SessionID]; ok {
			lastActivity = timestampPtr(&createdAt)
		}
		// A listener that predates the "host" field (packages/envoy/internal/session's
		// SessionEntry.Host) answers with none; fall back to the routing machine_id rather
		// than showing an empty machine on the Agents page during a partial rollout.
		session.Host = cmp.Or(session.Host, session.MachineID)
		rows = append(rows, agentResponse{
			Session:      session,
			OpenAsks:     openAsks[session.SessionID],
			LastActivity: lastActivity,
		})
	}
	WriteJSON(w, http.StatusOK, rows)
}

// leaveOutHiddenSessionRoles writes each session's roles as who reads them. A role's name can spell
// a project's key (legion-<project>-…, contracts.RoleProject), so a role of a project who does not
// see is left out, and a session's other roles stay, in their order. Which projects those are
// depends on the reader:
//
//   - A restricted member, or a credential acting for one (a viewer that is not open), is confined
//     to the projects it sees, public ones no more its own than private ones: it reads a role of a
//     project only where that key is one it sees (Viewer.Sees), and a role whose name spells no
//     project. A key no project holds is left out as well, so what it reads never tells a project
//     from a key nobody holds. Its viewer already holds what it sees, so this reads nothing.
//   - An open viewer reads every role but one of a private project it does not see, as
//     access.RoleProjectClause leaves out every reference to such a role, held or not. One read
//     judges every session's roles (seenReferences), made whatever they name, so a list that names
//     a hidden role costs what one that names none costs.
//
// Each session that holds a role is given a list of its own: the sessions are a copy of the
// roster's, whose lists every read shares. A reader that sees every object reads every role, with
// no read (R9), and no session left to answer reads nothing either.
func leaveOutHiddenSessionRoles(ctx context.Context, q queryer, who model.Actor, sessions []envoy.Session) error {
	if seesEveryObject(who) || len(sessions) == 0 {
		return nil
	}
	reads := func(role string) bool {
		project := routeRoleProject("role:" + role)
		return project == "" || (who.Viewer != nil && slices.Contains(who.Viewer.Sees, project))
	}
	if who.Viewer != nil && who.Viewer.Open {
		named := references{}
		for _, session := range sessions {
			for _, role := range session.Roles {
				named.name(roleProjectReference, routeRoleProject("role:"+role))
			}
		}
		seen, err := seenReferences(ctx, q, who, named)
		if err != nil {
			return err
		}
		reads = func(role string) bool { return seen.holdsRoute("role:" + role) }
	}
	for index := range sessions {
		if len(sessions[index].Roles) == 0 {
			continue
		}
		kept := make([]string, 0, len(sessions[index].Roles))
		for _, role := range sessions[index].Roles {
			if reads(role) {
				kept = append(kept, role)
			}
		}
		sessions[index].Roles = kept
	}
	return nil
}

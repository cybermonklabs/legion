package delivery

import (
	"context"

	"github.com/sjawhar/envoy/internal/dispatch/events"
)

// resolveSessionTitlesQuery answers, for each id it is given, that session's stored title: one
// read of session_titles_pkey (migration 20261117100008) for the whole list, and no row for a
// session that has none. It reads no events. Each title used to be resolved out of events on
// every page read: the read before cybermonklabs/legion#3517, a `distinct on` that sorted every
// event of the listed sessions, cost 42 to 133 ms a paint of the Delivery page over three
// projects and 812,386 events (dispatch://INFRA-2916, R13), and #3517's ordered read a session of
// events_session_actor_created_at_idx, which has no measurement at that size, still read events
// on each one. The table is written where the title is stamped instead (20261117100015's
// trigger). The plan test of this package holds the read there (session_titles_plan_test.go). It
// is the read under a rule that binds no viewer (events.EveryEvent), which every event passes:
// the stored title is the newest one the session stamped on any event.
const resolveSessionTitlesQuery = `select session_id, title from session_titles where session_id = any($1)`

// readableSessionTitlesQuery answers, for each id it is given, that session's newest event whose
// stamped title is not empty among the events readable holds the read to (events.Readable), as
// one ordered read a session that stops at that session's newest titled row it may read. It reads
// events_session_actor_created_at_idx (migration 0026), the partial index on ((actor->>'id'),
// created_at desc) for a session actor, once per id. session_titles cannot answer a rule that
// binds a viewer: a title stamped on an event of a project the reader does not see names nothing
// for that reader, which reads the newest title among the events it sees instead, and the stored
// row keeps only the newest title of all.
//
// The lateral is the shape this repository already uses for the same table and the same index,
// for the same question of the same population (agentLastActivityQuery, dispatch/api/agents.go).
// It repeats actor->>'kind' = 'session' because that index is partial on it, and it is joined
// with `cross join`, not a left join: a session with no titled event must answer no row, which is
// what the caller's map needs. The title condition is not in the index, so this is not one row
// read a session: the read skips a session's untitled rows on the way to its newest titled one,
// and a session that never stamped a title is read to the end of its own events and answers no
// row at all.
func readableSessionTitlesQuery(readable string) string {
	return `
		select s.id, e.session_title
		from unnest($1::text[]) s(id)
		cross join lateral (
			select actor->'origin'->>'session_title' as session_title
			from events
			where actor->>'kind' = 'session' and actor->>'id' = s.id
				and nullif(actor->'origin'->>'session_title', '') is not null
				and ` + readable + `
			order by created_at desc
			limit 1
		) e
	`
}

// ResolveSessionTitles answers the newest title each session stamped, which the Delivery page
// shows a session by (LEGION-567): a subagent's own commit trailer carries no parent-session
// reference, so a pull request's Parent agent is the first session its Omp-Session trailers name,
// shown by that title, unless a session group holds it (ParentAgents, parent_agent.go; LEGION-682).
//
// Every write stamps a model.Actor (including its ActorOrigin.SessionTitle) as JSONB onto the row
// it wrote and onto the matching events row, and the trigger on events (migration
// 20261117100015) records the title of each such event in session_titles, in the event's own
// transaction, with the boot's fill (store.FillSessionTitles) covering the events stored before
// it. The name is the newest title the session stamped, not the title on its newest event: a later
// write carrying none -- a delivery reply stamps no origin at all (postDeliveryReply) -- does not
// take the name away (INFRA-1368). It is durable -- it survives the session ending, unlike Envoy's
// live, TTL-expiring session registry -- for any session that ever claimed an issue, posted a
// message/comment/ask, or wrote a document under a title. A session that never stamped one is
// simply absent from the returned map, and nothing here backfills a name for it; SessionTitle
// falls back to its bare id. Each title is read among the events readable passes: a read answered
// to one caller binds that caller's rule, and so does the group rule, which holds a group's name
// to the titles its writer reads (groups.go sessionsTitled), and such a rule reads the events it
// passes (readableSessionTitlesQuery); a rule that binds no viewer (events.EveryEvent), which
// every event passes, reads session_titles by primary key (resolveSessionTitlesQuery).
func ResolveSessionTitles(ctx context.Context, q Querier, sessionIDs []string, readable events.Readable) (map[string]string, error) {
	titles := make(map[string]string, len(sessionIDs))
	if len(sessionIDs) == 0 {
		return titles, nil
	}
	// One statement for every id of the read rather than one query per id: a Delivery timeline page
	// can name hundreds of distinct sessions across its window.
	query, arguments := resolveSessionTitlesQuery, []any{sessionIDs}
	if readable.Viewer != nil {
		query, arguments = readableSessionTitlesQuery(readable.Where), readable.Args(sessionIDs)
	}
	rows, err := q.Query(ctx, query, arguments...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var sessionID, title string
		if err := rows.Scan(&sessionID, &title); err != nil {
			return nil, err
		}
		titles[sessionID] = title
	}
	return titles, rows.Err()
}

// SessionTitle is the title Dispatch knows for session, or session itself when it stamped none
// on anything it wrote. ParentAgents and the timeline's editor payload share this fallback.
func SessionTitle(titles map[string]string, session string) string {
	if title, ok := titles[session]; ok {
		return title
	}
	return session
}

// ListPopulationSessionIDs lists every session any stored delivery population pull request names.
// The group title rule reads this population, while the timeline reads only its current window.
func ListPopulationSessionIDs(ctx context.Context, q Querier) ([]string, error) {
	rows, err := q.Query(ctx, `select distinct unnest(sessions) from delivery_pull_requests order by 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

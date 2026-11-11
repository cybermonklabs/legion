package delivery

import "context"

// resolveSessionTitlesQuery answers, for each id it is given, that session's newest event whose
// stamped title is not empty, as one ordered read a session that stops at that session's newest
// titled row. It reads events_session_actor_created_at_idx (migration 0026), the partial index
// on ((actor->>'id'), created_at desc) for a session actor, once per id.
//
// It replaced `select distinct on (actor->>'id') … order by actor->>'id', created_at desc` over
// that same index: `Unique` consumes its whole input, so that read walked every event of every
// listed session to keep one of them, and this read's largest caller (sessionsTitled,
// groups.go) lists every session of the stored delivery population at once. The lateral is the
// shape this repository already uses for the same table and the same index, for the same
// question of the same population (agentLastActivityQuery, dispatch/api/agents.go; the 1,875
// rows and 470 buffers against 20 cited there were measured on that statement, not on this one).
// This statement's own measurement is the plan test of this package
// (session_titles_plan_test.go): over a fixture of 14 events across three listed sessions, the
// one node that reads events runs one loop a session -- three -- and touches no more of those 14
// rows than stopping at each session's newest titled row costs, which is 8 here. The `distinct
// on` it replaced was measured once on the same fixture when this change was made, not by that
// test: a single loop over all 14.
//
// The lateral repeats actor->>'kind' = 'session' because that index is partial on it, and it is
// joined with `cross join`, not a left join: a session with no titled event must answer no row,
// which is what the caller's map needs. The title condition is not in the index, so this is not
// one row read a session: the read skips a session's untitled rows on the way to its newest
// titled one, and a session that never stamped a title is read to the end of its own events and
// answers no row at all.
const resolveSessionTitlesQuery = `
		select s.id, e.session_title
		from unnest($1::text[]) s(id)
		cross join lateral (
			select actor->'origin'->>'session_title' as session_title
			from events
			where actor->>'kind' = 'session' and actor->>'id' = s.id
				and nullif(actor->'origin'->>'session_title', '') is not null
			order by created_at desc
			limit 1
		) e
	`

// ResolveSessionTitles answers the newest title each session stamped, which the Delivery page
// shows a session by (LEGION-567): a subagent's own commit trailer carries no parent-session
// reference, so a pull request's Parent agent is the first session its Omp-Session trailers name,
// shown by that title, unless a session group holds it (ParentAgents, parent_agent.go; LEGION-682).
//
// A session's title is never stored in a dedicated table; every write stamps a model.Actor
// (including its ActorOrigin.SessionTitle) as JSONB onto the row it wrote and onto the matching
// events row, and events_session_actor_created_at_idx (migration 0026) makes this one indexed
// read per id. The name is the newest title the session stamped, not the title on its newest
// event: a later write carrying none -- a delivery reply stamps no origin at all
// (postDeliveryReply) -- does not take the name away (INFRA-1368). It is durable -- it survives
// the session ending, unlike Envoy's live, TTL-expiring session registry -- for any session that
// ever claimed an issue, posted a message/comment/ask, or wrote a document under a title. A
// session that never stamped one is simply absent from the returned map, and nothing here
// backfills a name for it; SessionTitle falls back to its bare id.
func ResolveSessionTitles(ctx context.Context, q Querier, sessionIDs []string) (map[string]string, error) {
	titles := make(map[string]string, len(sessionIDs))
	if len(sessionIDs) == 0 {
		return titles, nil
	}
	// One statement for every id of the read, over that index, rather than one query per id: a
	// Delivery timeline page can name hundreds of distinct sessions across its window.
	rows, err := q.Query(ctx, resolveSessionTitlesQuery, sessionIDs)
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

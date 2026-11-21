package asof

import "slices"

// askStateTypes are the events whose payload is the whole ask (model.AskEventPayload, flat, and
// the edit's and the answer's payloads that embed it). The follower events carry no ask and are
// not among them.
var askStateTypes = []string{
	"ask.opened", "ask.answered", "ask.edited", "ask.resolved",
	"ask.handed_back", "ask.asked_of", "ask.anchor_refreshed",
}

// AskStateTypes are the event types the asks shadow reads an ask's state from. The predicate of
// the partial index events_ask_state (migration 20261120004316) names exactly these types, in
// this order, so that the planner can prove that the index serves that read: a change here needs
// a new index.
func AskStateTypes() []string { return slices.Clone(askStateTypes) }

// AsksChangedAfterStatement reads which asks changed after a cut: of the asks whose ids $1 holds
// (text[]), the id of each one that has an event of its state (AskStateTypes) later than the cut
// $2 (timestamptz). It names public.events, the events as they are now, by its schema, so that
// the store's binding at a cut does not read the events shadow in its place (Bind). An ask that
// no such event changed has the same parts at the cut as now, so a past-time answer gives its
// revision and its part ids from the present tables, and leaves them out for every other ask
// (reason P). The partial index events_ask_state serves the read.
var AsksChangedAfterStatement = `select a.id from unnest($1::text[]) as a(id)
	where exists (
		select from public.events e
		where e.payload->>'id' = a.id and e.type in ` + askStateList + `
			and e.created_at > $2::timestamptz)`

// commentCreatedTypes are the events that create a comment: comment.created, and comment.answered,
// which a delivery callback's reply is created with (api/comment_delivery.go).
var commentCreatedTypes = []string{"comment.created", "comment.answered"}

// The type lists of the asks' shadows and of the creating events of a comment as SQL lists
// (sqlList), each quoted once.
var (
	askStateList       = sqlList(askStateTypes)
	askFollowerList    = sqlList(askFollowerTypes)
	commentCreatedList = sqlList(commentCreatedTypes)
)

// asksShadow is decision 2's asks: the newest ask.* payload of each ask at the cut, by event id,
// the follower events excepted. An ask none of whose events is at the cut did not exist then.
// Columns no write changes after the insert (id, issue_key, author, created_at, artifact_id, kind,
// written_version, which its insert trigger sets) are the row's. Every other column is the
// payload's, a key a payload of an older shape lacks reading as the column's default (asked_of,
// asked_of_by and asked_of_at before event 438,269), with these exceptions:
//   - block_artifact_id: the payload's block_artifact.id where the payload carries one; the
//     payload carries the field only where its writer hydrated it, so elsewhere the row's.
//   - search: what the asks_search trigger computes, from the question, options and answer at
//     the cut.
//   - handed_back_reply_id: no payload carries it. A hand-back records the thread's newest reply
//     as it runs, under the owner lock every reply takes (handBackApprovalAsk, api/reviews.go), so
//     at the cut it is the reply whose creating event is the last of the ask's thread before the
//     newest ask.handed_back at the cut (one owner's events commit in id order); null when no
//     hand-back is at the cut.
//   - approval: the payload's as written. An approval written before event 186,599 holds no
//     documents, which migration 20261102110100 filled in the row with no event: the field is
//     absent at such a cut (reason B).
func asksShadow(cut string) string {
	question := `coalesce(s.payload->>'question', '')`
	options := jsonArray(`s.payload->'options'`)
	answer := jsonValue(`s.payload->'answer'`)
	return `select a.id as id,
		a.issue_key as issue_key,
		a.author as author,
		` + question + ` as question,
		` + options + ` as options,
		coalesce((s.payload->>'multiple')::boolean, false) as multiple,
		coalesce(s.payload->>'urgency', 'med') as urgency,
		` + jsonValue(`s.payload->'anchor'`) + ` as anchor,
		coalesce(s.payload->>'state', 'open') as state,
		` + answer + ` as answer,
		a.created_at as created_at,
		` + jsonValue(`s.payload->'resolution'`) + ` as resolution,
		a.artifact_id as artifact_id,
		(s.payload->>'edited_at')::timestamptz as edited_at,
		a.kind as kind,
		` + jsonValue(`s.payload->'approval'`) + ` as approval,
		s.payload->>'block_id' as block_id,
		coalesce((s.payload->'block_artifact'->>'id')::uuid, a.block_artifact_id) as block_artifact_id,
		public.search_vector('', public.search_text(` + question + ` || ' ' || public.ask_search_suffix(` + options + `, ` + answer + `))) as search,
		case when a.kind = 'approval' then (
			select (r.payload->>'id')::uuid
			from (
				select hb.id from public.events hb
				where hb.payload->>'id' = a.id::text and hb.type = 'ask.handed_back'
					and hb.created_at <= ` + cut + `
				order by hb.id desc limit 1
			) h
			cross join lateral (
				select ce.payload from public.comments c
				cross join lateral (` + commentCreatedEvent("c", "ce", "") + `) ce
				where c.ask_id = a.id and ce.id < h.id
				order by ce.id desc limit 1
			) r
		) end as handed_back_reply_id,
		a.written_version as written_version,
		s.payload->>'asked_of' as asked_of,
		s.payload->>'asked_of_by' as asked_of_by,
		(s.payload->>'asked_of_at')::timestamptz as asked_of_at
	from public.asks a
	cross join lateral (
		select e.payload from public.events e
		where e.payload->>'id' = a.id::text and e.type in ` + askStateList + `
			and e.created_at <= ` + cut + `
		order by e.id desc limit 1
	) s`
}

// askFollowersShadow is decision 2's ask_followers: of each ask, the sessions that followed it at
// the cut. Every follow is one of three writes, each in a transaction that appends the event
// read here: a session that opens the ask (ask.opened, its author), a session that writes in its
// thread (the reply's comment.created or comment.answered, its author), and a follow or an
// unfollow (ask.follower_added, ask.follower_removed; a renewed approval request appends
// ask.follower_added for its session). The insert keeps the first follow's time (on conflict do
// nothing, asks/followers.go), so since is the earliest follow after the session's last unfollow
// at the cut, and a session whose last word at the cut is an unfollow follows not.
func askFollowersShadow(cut string) string {
	return `select a.id as ask_id, f.session_id as session_id, f.since as since
	from public.asks a
	cross join lateral (
		select v.session_id, min(v.created_at) filter (where v.follows and v.id > v.left_at) as since
		from (
			select w.session_id, w.follows, w.id, w.created_at,
				coalesce(max(w.id) filter (where not w.follows) over (partition by w.session_id), 0) as left_at
			from (
				select e.payload->'author'->>'id' as session_id, true as follows, e.id, e.created_at
				from public.events e
				where e.payload->>'id' = a.id::text and e.type = 'ask.opened'
					and e.created_at <= ` + cut + `
					and e.payload->'author'->>'kind' = 'session'
				union all
				select e.payload->'author'->>'id', true, e.id, e.created_at
				from public.comments c
				cross join lateral (` + commentCreatedEvent("c", "e", cut) + `) e
				where c.ask_id = a.id and e.payload->'author'->>'kind' = 'session'
				union all
				select e.payload->>'session_id', e.type = 'ask.follower_added', e.id, e.created_at
				from public.events e
				where e.type in ` + askFollowerList + `
					and e.payload->>'ask_id' = a.id::text and e.created_at <= ` + cut + `
			) w
			where coalesce(w.session_id, '') <> ''
		) v
		group by v.session_id
	) f
	where f.since is not null`
}

// askFollowerTypes are the events of a follow and an unfollow, which name the ask as ask_id in
// their payload. The predicate of the partial index events_ask_follower names exactly these
// types, in this order, so that the planner can prove that the index serves askFollowersShadow's
// read of them: a change here needs a new index.
var askFollowerTypes = []string{"ask.follower_added", "ask.follower_removed"}

// AskFollowerTypes are the event types the ask followers shadow reads a follow and an unfollow
// from, in the order of the predicate of events_ask_follower.
func AskFollowerTypes() []string { return slices.Clone(askFollowerTypes) }

// commentCreatedEvent is the event that created the comment aliased comment, under the alias
// event: its id, its created_at and its payload, read by the comment's id in their payload
// (events_comment_state), at or before cut when cut is not empty. It is one row, for a lateral
// join: PostgreSQL keeps no statistics of payload->>'id', and a plain join of the comments to
// their events it plans as a hash of every comment.created.
func commentCreatedEvent(comment, event, cut string) string {
	bound := ""
	if cut != "" {
		bound = " and " + event + ".created_at <= " + cut
	}
	return `select ` + event + `.id, ` + event + `.created_at, ` + event + `.payload from public.events ` + event + `
					where ` + event + `.payload->>'id' = ` + comment + `.id::text and ` + event + `.type in ` + commentCreatedList + bound + `
					order by ` + event + `.id limit 1`
}

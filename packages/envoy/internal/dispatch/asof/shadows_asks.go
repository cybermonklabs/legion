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
// a new index. That predicate also holds created_at is not null, so each read of the index
// compares created_at with the cut, as asksShadow and AsksChangedAfterStatement do.
func AskStateTypes() []string { return slices.Clone(askStateTypes) }

// AsksChangedAfterStatement reads which asks changed after a cut, and which of them hold a
// wording at the cut that no record gives. Of the asks whose ids $1 holds (text[]), it answers
// the id of each ask that changed after the cut $2 (timestamptz), and beside it whether the ask's
// wording at the cut is unknown (wording_unknown). An ask changed after the cut where an event of
// its state (AskStateTypes) is later than the cut, or where its wording at the cut is unknown: a
// write with no event changed that wording, and that write can be later than the cut. An ask
// that did not change has the same parts at the cut as now, so a past-time answer gives its
// revision and its part ids from the present tables, and leaves them out for every other ask
// (reason P).
//
// The wording is the question, the options, multiple and the urgency. The restore of a decided
// ask's wording (docs' decidedWordingRestore, at boot and at a document's settlement) writes them
// back to the wording before the ask's first edit after its decision, and no event carries the
// change. So at each cut after the restore and before the ask's next event, the asks shadow gives
// the wording that the restore undid. Let w be the wording of the ask's newest state event at the
// cut, which the shadow gives. The wording just after the cut, by the record, is:
//   - that of n, the ask's first state event after the cut, when n is an ask.edited whose
//     previous wording records a question: the previous wording, where each of options, multiple
//     and urgency that it does not record counts as w's;
//   - that of any other n, its own: no other state event changes the wording;
//   - with no n, that of the present row.
//
// Where that wording is not w, a write with no event changed the wording after w's event, at a
// time the records do not give. The wording at the cut is then unknown, and the answer leaves out
// the four members (reason E), and the parts too, since the present parts are those of another
// wording. The options are compared with no part id: a payload's options carry each option's part
// id, and the row's options and an edit's previous options do not.
//
// It names public.events and public.asks by their schema, so that the store's binding at a cut
// does not read a shadow in their place (Bind). The partial index events_ask_state serves each
// read of an ask's events, and asks_pkey the read of its row. An id that is no uuid names no row.
var AsksChangedAfterStatement = `select x.id, x.wording_unknown from (
	select a.id, n.type as later,
		case
			when s.payload is null then false
			when n.type is null then r.id is not null and ` + askWordingDiffers(askWordingAt, askWordingOfTheRow) + `
			when n.type = 'ask.edited' and n.payload->'previous' ? 'question' then ` + askWordingDiffers(askWordingAt, askWordingBeforeTheEdit) + `
			else ` + askWordingDiffers(askWordingAt, askWordingOf(`n.payload`)) + `
		end as wording_unknown
	from unnest($1::text[]) as a(id)
	left join lateral (
		select e.payload from public.events e
		where e.payload->>'id' = a.id and e.type in ` + askStateList + `
			and e.created_at <= $2::timestamptz
		order by e.id desc limit 1
	) s on true
	left join lateral (
		select e.type, e.payload from public.events e
		where e.payload->>'id' = a.id and e.type in ` + askStateList + `
			and e.created_at > $2::timestamptz
		order by e.id limit 1
	) n on true
	left join public.asks r on n.type is null and s.payload is not null
		and r.id = (case when pg_input_is_valid(a.id, 'uuid') then a.id::uuid end)
) x
where x.later is not null or x.wording_unknown`

// askWording is the wording of an ask as four SQL expressions: its question, its options (a JSON
// array), whether it takes several options, and its urgency.
type askWording struct{ question, options, multiple, urgency string }

// askWordingOf is the wording that the JSON object payload holds, as the asks shadow gives it:
// each key that a payload of an older shape lacks reads as the column's default.
func askWordingOf(payload string) askWording {
	return askWording{
		question: `coalesce(` + payload + `->>'question', '')`,
		options:  jsonArray(payload + `->'options'`),
		multiple: `coalesce((` + payload + `->>'multiple')::boolean, false)`,
		urgency:  `coalesce(` + payload + `->>'urgency', 'med')`,
	}
}

// The wordings that AsksChangedAfterStatement compares: the ask's at the cut, of its newest state
// event at the cut (s), which the asks shadow gives too; its row's (r); and the previous wording
// of its first edit after the cut (n), where each part that the edit does not record is the
// wording at the cut's.
var (
	askWordingAt            = askWordingOf(`s.payload`)
	askWordingOfTheRow      = askWording{question: `r.question`, options: `r.options`, multiple: `r.multiple`, urgency: `r.urgency`}
	askWordingBeforeTheEdit = askWording{
		question: `(n.payload->'previous'->>'question')`,
		options:  `(case when jsonb_typeof(n.payload->'previous'->'options') = 'array' then n.payload->'previous'->'options' else ` + askWordingAt.options + ` end)`,
		multiple: `coalesce((n.payload->'previous'->>'multiple')::boolean, ` + askWordingAt.multiple + `)`,
		urgency:  `coalesce(n.payload->'previous'->>'urgency', ` + askWordingAt.urgency + `)`,
	}
)

// askWordingDiffers is the SQL test that wording a is not wording b, the options of each read
// with no part id (optionsWithoutIDs).
func askWordingDiffers(a, b askWording) string {
	return `((` + a.question + `, ` + optionsWithoutIDs(a.options) + `, ` + a.multiple + `, ` + a.urgency + `) is distinct from (` +
		b.question + `, ` + optionsWithoutIDs(b.options) + `, ` + b.multiple + `, ` + b.urgency + `))`
}

// optionsWithoutIDs is the JSON array of options options with no id in any option, in their
// order.
func optionsWithoutIDs(options string) string {
	return `(select coalesce(jsonb_agg(o.value - 'id' order by o.ordinality), '[]'::jsonb)
		from jsonb_array_elements(` + options + `) with ordinality o)`
}

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
	question, options := askWordingAt.question, askWordingAt.options
	answer := jsonValue(`s.payload->'answer'`)
	return `select a.id as id,
		a.issue_key as issue_key,
		a.author as author,
		` + question + ` as question,
		` + options + ` as options,
		` + askWordingAt.multiple + ` as multiple,
		` + askWordingAt.urgency + ` as urgency,
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

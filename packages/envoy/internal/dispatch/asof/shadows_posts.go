package asof

import "slices"

// commentStateTypes are the events whose payload is the whole comment (model.CommentEventPayload):
// the comment.* events but the delivery receipt, and the suggestion's accept and reject. The
// predicate of the partial index events_comment_state names exactly these types, in this order,
// so that the planner can prove that the index serves the comments shadow's reads of a comment's
// events, each of which names these types or some of them: a change here needs a new index.
var commentStateTypes = []string{
	"comment.created", "comment.answered", "comment.edited", "comment.deleted",
	"comment.resolved", "comment.reopened", "comment.anchor_refreshed",
	"suggestion.accepted", "suggestion.rejected",
}

// CommentStateTypes are the event types the comments shadow reads a comment from, in the order
// of the predicate of events_comment_state.
func CommentStateTypes() []string { return slices.Clone(commentStateTypes) }

// messageStateTypes are the events whose payload is the whole message (model.MessageEventPayload):
// message.answered creates a reply, as message.created creates every other message. The predicate
// of the partial index events_message_state names exactly these types, in this order, for the
// messages shadow's reads, as commentStateTypes do for the comments shadow's.
var messageStateTypes = []string{"message.created", "message.answered", "message.edited", "message.deleted"}

// MessageStateTypes are the event types the messages shadow reads a message from, in the order of
// the predicate of events_message_state.
func MessageStateTypes() []string { return slices.Clone(messageStateTypes) }

// commentRewriteTypes and messageRewriteTypes are the events of a post's edit and of its delete.
// From the time post_body_history began (postHistoryBegan), the transaction of each one writes one
// post_body_history row, which keeps the body that the edit or the delete replaced.
var (
	commentRewriteTypes = []string{"comment.edited", "comment.deleted"}
	messageRewriteTypes = []string{"message.edited", "message.deleted"}
)

// The type lists of the posts' shadows as SQL lists (sqlList), each quoted once.
var (
	commentStateList   = sqlList(commentStateTypes)
	messageStateList   = sqlList(messageStateTypes)
	commentRewriteList = sqlList(commentRewriteTypes)
	messageRewriteList = sqlList(messageRewriteTypes)
)

// CommentBodiesNotGivenStatement reads, of the comments whose ids $1 holds (uuid[]), the id of each
// comment that its author did not delete and whose body at the cut $2 (timestamptz) no record
// holds (reason T). The first edit of such a comment after the cut was made before
// post_body_history began, so no history row keeps the body that the edit replaced, and the newest
// event of the comment at the cut holds only the first part of the body (body_truncated). The
// comments shadow gives such a body as the empty text (postBodyAtCut), so a past-time answer
// leaves the body out. A deleted comment is not such a comment: its body is empty at all times
// (decision 14). The statement names each table by its schema, so the store's binding at a cut
// does not read a shadow in its place (Bind). It reads the events as they are now, under its own
// bound $2.
//
// No message is such a post. post_body_history began with the edit and the delete of a message
// (migration 20261110224508), so each edit of a message after the cut has its history row.
var CommentBodiesNotGivenStatement = `select c.id::text from public.comments c
	where c.id = any($1::uuid[]) and c.deleted_at is null
		and coalesce((
			select (e.payload->>'body_truncated')::boolean from public.events e
			where e.payload->>'id' = c.id::text and e.type in ` + commentStateList + `
				and e.created_at <= $2::timestamptz
			order by e.id desc limit 1), false)
		and (
			select f.created_at from public.events f
			where f.payload->>'id' = c.id::text and f.type in ` + commentRewriteList + `
				and f.created_at > $2::timestamptz
			order by f.id limit 1) < ` + postHistoryBegan

// CommentAnchorsOrphanedNotGivenStatement reads, of the comments whose ids $1 holds (uuid[]), the
// id of each comment whose anchor at the cut $2 (timestamptz) the comments shadow gives as not
// orphaned, and whose orphaned flag at the cut no record gives (reason E). The shadow gives the
// anchor of the comment's newest state event at the cut. An anchor refresh that finds the mark
// gone writes orphaned true and appends comment.anchor_refreshed, which carries the comment, in
// its own transaction, and a refresh that changes nothing appends no event. Two writes set the
// flag true with no event that dates the change: an accept or a reject of a suggestion that found
// its mark gone, before such a 409 appended a comment.anchor_refreshed of its own (api's
// recordOrphanedCommentAnchor), and the delete of a suggestion that finds its mark gone, whose
// comment.deleted carries the flag as true whether the delete set it or a 409 before it did. No
// write sets the flag false with no event.
//
// So the flag just before n, the comment's first state event after the cut, is, by the record:
//   - with no n, the present row's flag;
//   - where n is a comment.anchor_refreshed that orphans the anchor, false;
//   - where n is a comment.anchor_refreshed that leaves the anchor not orphaned with the quote it
//     had at the cut, true: that refresh changed the flag alone, so it found the mark again;
//   - where n is any other event, n's own flag: it carries the comment as its write read the row.
//
// Where that flag is true, the anchor was orphaned at a time between the newest event at the cut
// and n that the records do not give, and the answer leaves the flag out. Only a
// comment.anchor_refreshed dates an orphaning. This costs the deletes that set the flag
// themselves: at each cut before such a delete the flag is left out, though it was false. A rule
// that took the delete to date the orphaning would give false at each cut between an old 409 and
// the delete that followed it, where every read then gave true; a member left out is honest, and
// false would be wrong. A refresh that found the mark again and also moved its quote reads as a
// move of the quote alone, so the flag at a cut between an orphaning with no event and such a
// refresh reads false.
//
// The statement names each table by its schema, so the store's binding at a cut does not read a
// shadow in its place (Bind). It reads each comment's events through events_comment_state.
var CommentAnchorsOrphanedNotGivenStatement = `select c.id::text from public.comments c
	cross join lateral (
		select e.payload->'anchor' as anchor from public.events e
		where e.payload->>'id' = c.id::text and e.type in ` + commentStateList + `
			and e.created_at <= $2::timestamptz
		order by e.id desc limit 1
	) s
	left join lateral (
		select e.type, e.payload->'anchor' as anchor from public.events e
		where e.payload->>'id' = c.id::text and e.type in ` + commentStateList + `
			and e.created_at > $2::timestamptz
		order by e.id limit 1
	) n on true
	where c.id = any($1::uuid[])
		and jsonb_typeof(s.anchor) = 'object' and not coalesce((s.anchor->>'orphaned')::boolean, false)
		and case
			when n.type is null then coalesce((c.anchor->>'orphaned')::boolean, false)
			when n.type = 'comment.anchor_refreshed' then not coalesce((n.anchor->>'orphaned')::boolean, false)
				and n.anchor->>'quote' is not distinct from s.anchor->>'quote'
			else coalesce((n.anchor->>'orphaned')::boolean, false)
		end`

// ItemsChangedAfterStatement reads, of the items whose ids $1 holds (text[]), the id of each item
// that changed after the cut $2 (timestamptz): a comment or a message with an edit or a delete
// after the cut, or an ask that AsksChangedAfterStatement answers, which is one with an event of
// its state after the cut or one whose wording at the cut no record gives. Only a change of its
// item rewrites an item's blocks (refs.PlanItemIndex, refs.PlanAskParts; the restore of a decided
// ask's wording indexes its parts again), so the present rows of comment_blocks, message_blocks
// and ask_blocks give a block's text, and whether it is removed, as they stood at the cut for an
// item that did not change after it. The block index keeps no earlier state with its time, so for
// an item that changed, the answer leaves both out. The statement names public.events and
// public.asks by their schema, so the store's binding at a cut does not read a shadow in their
// place (Bind). It reads each kind of item through the partial index of its types
// (events_comment_state, events_message_state, events_ask_state), one part of a union each: one
// list of the types of every kind implies no index's predicate. The union answers each id once.
var ItemsChangedAfterStatement = `select e.payload->>'id' from public.events e
	where e.payload->>'id' = any($1::text[]) and e.type in ` + commentRewriteList + `
		and e.created_at > $2::timestamptz
	union
	select e.payload->>'id' from public.events e
	where e.payload->>'id' = any($1::text[]) and e.type in ` + messageRewriteList + `
		and e.created_at > $2::timestamptz
	union
	select changed.id from (` + AsksChangedAfterStatement + `) changed`

// CommentHistoryRowsAtCut and MessageHistoryRowsAtCut are SQL expressions for the history read of
// one post at a cut (decision 17). Each is the number of post_body_history rows that the post had
// at the cut, for the post whose row the alias p names: one row for each edit or delete event of
// the post that began at or after the time post_body_history began (historyEventsCount). So the
// history at the cut is the first rows, as many as this number. An edit made before the history
// began wrote no row, and it does not count. The expression reads the events by their bare name,
// so the binding at the cut reads the events at the cut (the events shadow).
var (
	CommentHistoryRowsAtCut = historyEventsCount("events", "p", commentRewriteList, "")
	MessageHistoryRowsAtCut = historyEventsCount("events", "p", messageRewriteList, "")
)

// commentsShadow is decision 2's comments: each comment whose creating event (comment.created, or
// the comment.answered of a delivery callback's reply) began at or before the cut. Its
// created_at is not that clock: the insert takes clock_timestamp(), later than its transaction's
// start. Columns no write changes after the insert are the row's (id, issue_key, author,
// reply_to, created_at, ask_id, artifact_id, turn, written_version, replies_to_block, answers).
//   - node_kind and node_address: the row's, the address of the comment's node in graph_nodes
//     (20261109022248). It names the comment, not anything it holds, so it is the same at every
//     time; a writer of an older build left it null, and a later pass fills it.
//   - body: the body at the cut by the one-clock rule (postBodyAtCut).
//   - anchor, resolved, resolved_by, resolved_at, suggestion and edited_at: the newest payload
//     at the cut, by event id (one owner's events commit in id order).
//   - deleted_at and deleted_by: the present values (decision 14). A comment deleted after the
//     cut reads deleted with no body, and with no suggestion, which its delete cleared, as the
//     present read gives it.
//   - search: what the comments_search trigger computes, from the body at the cut.
//
// Each read of the comment's events is a lateral join of one row, by the comment's id in their
// payload, so it reads events_comment_state for that comment. PostgreSQL keeps no statistics of
// payload->>'id' (a partial index's expression gives the table none), so it takes a comment to
// have hundreds of events; an EXISTS of the creating event, which it plans as a semi-join, it
// then read as a hash of every comment.created.
func commentsShadow(cut string) string {
	return `select c.id as id,
		c.issue_key as issue_key,
		c.author as author,
		b.body as body,
		` + jsonValue(`p.payload->'anchor'`) + ` as anchor,
		c.reply_to as reply_to,
		coalesce((p.payload->>'resolved')::boolean, false) as resolved,
		case when c.deleted_at is null then ` + jsonValue(`p.payload->'suggestion'`) + ` end as suggestion,
		c.created_at as created_at,
		c.ask_id as ask_id,
		c.artifact_id as artifact_id,
		public.search_vector('', public.search_text(b.body)) as search,
		` + jsonValue(`p.payload->'resolved_by'`) + ` as resolved_by,
		(p.payload->>'resolved_at')::timestamptz as resolved_at,
		(p.payload->>'edited_at')::timestamptz as edited_at,
		c.turn as turn,
		c.written_version as written_version,
		c.replies_to_block as replies_to_block,
		c.answers as answers,
		c.node_kind as node_kind,
		c.node_address as node_address,
		c.deleted_at as deleted_at,
		c.deleted_by as deleted_by
	from public.comments c
	cross join lateral (
		select from public.events e
		where e.payload->>'id' = c.id::text and e.type in ` + commentCreatedList + `
			and e.created_at <= ` + cut + `
		limit 1
	) created
	cross join lateral (
		select e.id, e.payload from public.events e
		where e.payload->>'id' = c.id::text and e.type in ` + commentStateList + `
			and e.created_at <= ` + cut + `
		order by e.id desc limit 1
	) p
	left join lateral (
		select e.id, e.created_at from public.events e
		where e.payload->>'id' = c.id::text and e.type in ` + commentRewriteList + `
			and e.created_at > ` + cut + `
		order by e.id limit 1
	) f on true
	cross join lateral (select ` + postBodyAtCut("c", "comment_id", "p", "f", commentRewriteList, cut) + ` as body) b`
}

// messagesShadow is decision 2's messages: each message whose creating event (message.created, or
// message.answered for a reply) began at or before the cut. A message's created_at is the
// creating transaction's now(), read before its insert (api/messages.go, message_delivery.go) or
// its column's default (api/broadcasts.go), the very value that transaction's event takes, so
// the row's created_at is that event's.
//   - body: the body at the cut by the one-clock rule (postBodyAtCut). Only an issue message is
//     edited or deleted (lockedMessageForCorrection). Its events are read by the message's id in
//     their payload, through events_message_state, as a comment's are through events_comment_state.
//   - edited_at: the newest payload at the cut; a payload written before the field existed
//     carries none, as no message was edited then.
//   - deleted_at and deleted_by: the present values (decision 14).
//   - search: what the messages_search trigger computes, from the body at the cut.
//   - Every other column: the row's, which no write changes after the insert.
func messagesShadow(cut string) string {
	return `select m.id as id,
		m.issue_key as issue_key,
		m.author as author,
		b.body as body,
		m.created_at as created_at,
		public.search_vector('', public.search_text(b.body)) as search,
		m.in_reply_to as in_reply_to,
		m.target as target,
		m.broadcast_id as broadcast_id,
		m.broadcast_position as broadcast_position,
		m.replies_to_block as replies_to_block,
		(p.payload->>'edited_at')::timestamptz as edited_at,
		m.deleted_at as deleted_at,
		m.deleted_by as deleted_by
	from public.messages m
	left join lateral (
		select e.id, e.payload from public.events e
		where e.payload->>'id' = m.id::text
			and e.type in ` + messageStateList + ` and e.created_at <= ` + cut + `
		order by e.id desc limit 1
	) p on true
	left join lateral (
		select e.id, e.created_at from public.events e
		where e.payload->>'id' = m.id::text
			and e.type in ` + messageRewriteList + ` and e.created_at > ` + cut + `
		order by e.id limit 1
	) f on true
	cross join lateral (select ` + postBodyAtCut("m", "message_id", "p", "f", messageRewriteList, cut) + ` as body) b
	where m.created_at <= ` + cut
}

// commentDeliveriesShadow is decision 2's comment_deliveries: each attempt of a mention whose
// insert began at or before the cut (created_at, its column's now()), or that an event at the cut
// dates. An attempt's state is dated by its newest comment.delivery receipt at the cut
// (model.CommentDeliveryEventPayload: its state, error, duplicate, mode, session and reply) or by
// the creating event of the reply that answered it at the cut (state sent, as the callback
// writes it, api/comment_delivery.go), and the columns its send wrote with that state
// (envelope_id, resolve_error) are the row's, which nothing writes again once the attempt is
// settled. An attempt no event at the cut dates holds a state the records cannot give (reason E):
// its state is the empty string and every column a send writes is its default (delivery the
// empty string; session_id, envelope_id, error, resolve_error and reply_id null; duplicate
// false). claimed_at is a sender's lease, which no record dates; it is null.
func commentDeliveriesShadow(cut string) string {
	return `select d.comment_id as comment_id,
		d.target as target,
		d.attempt as attempt,
		case when r.id is not null then coalesce(r.payload->>'delivery', d.delivery)
			when y.id is not null then d.delivery else '' end as delivery,
		case when r.id is not null then r.payload->>'session_id'
			when y.id is not null then d.session_id end as session_id,
		case when r.id is not null or y.id is not null then d.envelope_id end as envelope_id,
		case when y.id is not null then 'sent'
			when r.id is not null then coalesce(r.payload->>'state', '') else '' end as state,
		case when y.id is null and r.id is not null then nullif(r.payload->>'error', '') end as error,
		case when r.id is not null or y.id is not null then d.resolve_error end as resolve_error,
		case when y.id is not null then d.reply_id
			when r.id is not null then (r.payload->>'reply_id')::uuid end as reply_id,
		d.created_at as created_at,
		null::timestamptz as claimed_at,
		case when r.id is not null then coalesce((r.payload->>'duplicate')::boolean, false) else false end as duplicate
	from public.comment_deliveries d
	join public.comments c on c.id = d.comment_id
	left join lateral (
		select e.id, e.payload from public.events e
		where e.type = 'comment.delivery' and e.payload->>'comment_id' = d.comment_id::text
			and e.payload->>'target' = d.target and e.payload->>'attempt' = d.attempt::text
			and e.created_at <= ` + cut + ` and ` + ownedBy("e", "c") + `
		order by e.id desc limit 1
	) r on true
	left join lateral (
		select e.id from public.events e
		where d.reply_id is not null and e.payload->>'id' = d.reply_id::text
			and e.type in ` + commentCreatedList + `
			and e.created_at <= ` + cut + `
		limit 1
	) y on true
	where d.created_at <= ` + cut + ` or r.id is not null or y.id is not null`
}

// messageDeliveriesShadow is decision 2's message_deliveries, by the rule of
// commentDeliveriesShadow: an attempt's state is dated by its newest message.delivery receipt at
// the cut (model.MessageDeliveryEventPayload) or by the reply that answered it at the cut, whose
// created_at is its creating event's (messagesShadow). The receipt of an issue message is its
// issue's event; the receipt of a direct message is an event of no owner (seq 0).
//   - requested_by and envelope_id: the row's for a dated attempt, which its send or its insert
//     wrote; null for one no event dates (reason E).
//   - accepted_at and accepted_as: the row's when the accept is at or before the cut. The accept
//     writes accepted_at as its transaction's now(), the time its message.accepted takes
//     (acceptDelivery, api/message_delivery.go).
//   - urgency and connected_only: the row's, which the attempt's insert writes.
//   - An attempt no event dates: state, delivery and session_id the empty string (the two last
//     are not null, with no default); envelope_id, error, reply_id and requested_by null;
//     duplicate false.
//   - claimed_at: a sender's lease, null.
func messageDeliveriesShadow(cut string) string {
	return `select d.message_id as message_id,
		d.attempt as attempt,
		case when r.id is not null then coalesce(r.payload->>'delivery', d.delivery)
			when y.replied then d.delivery else '' end as delivery,
		case when r.id is not null then coalesce(r.payload->>'session_id', d.session_id)
			when y.replied then d.session_id else '' end as session_id,
		case when r.id is not null or y.replied then d.envelope_id end as envelope_id,
		case when y.replied then 'sent'
			when r.id is not null then coalesce(r.payload->>'state', '') else '' end as state,
		case when not y.replied and r.id is not null then nullif(r.payload->>'error', '') end as error,
		case when y.replied then d.reply_id end as reply_id,
		d.created_at as created_at,
		null::timestamptz as claimed_at,
		case when r.id is not null then coalesce((r.payload->>'duplicate')::boolean, false) else false end as duplicate,
		case when r.id is not null or y.replied then d.requested_by end as requested_by,
		case when d.accepted_at <= ` + cut + ` then d.accepted_at end as accepted_at,
		case when d.accepted_at <= ` + cut + ` then d.accepted_as end as accepted_as,
		d.urgency as urgency,
		d.connected_only as connected_only
	from public.message_deliveries d
	join public.messages m on m.id = d.message_id
	left join lateral (
		select e.id, e.payload from public.events e
		where e.type = 'message.delivery' and e.payload->>'message_id' = d.message_id::text
			and e.payload->>'attempt' = d.attempt::text and e.created_at <= ` + cut + `
			and (e.issue_key = m.issue_key
				or (m.issue_key is null and e.issue_key is null and e.seq = 0
					and e.artifact_id is null and e.project_key is null))
		order by e.id desc limit 1
	) r on true
	cross join lateral (
		select exists (
			select from public.messages reply where reply.id = d.reply_id and reply.created_at <= ` + cut + `
		) as replied
	) y
	where d.created_at <= ` + cut + ` or r.id is not null or y.replied`
}

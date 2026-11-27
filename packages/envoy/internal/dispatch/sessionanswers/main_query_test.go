package sessionanswers

// mainQuery is the whole text that query sent at commit cdb2baa0c3f9f6392aa3fb1aeb8cbb9c812af5b1,
// before the read found the session's events through the index of their destinations, with
// ownsAsk, declinedResolution and model.DeliveryTakenStatesSQL written out as they were at that
// commit. It is the oracle of the tests that hold the read to main's rows, held letter for letter.
// A text built from the package's own constants would move with them, and the two sides would then
// agree however the read changed.
const mainQuery = `
	with reached as (
		select e.type, e.payload->>'id' as id, e.payload->'resolution' as resolution
		from events e
		where e.actor->>'kind' = 'user'
		  and e.type in ('ask.answered', 'comment.created', 'message.created', 'message.answered',
		                 'ask.resolved')
		  and e.created_at >= $2
		  and $3 = any(e.published_destinations)
	), mentioned as (
		select cd.comment_id::text as id
		from comment_deliveries cd
		where cd.session_id = $1 and cd.state in ('sent', 'queued') and cd.created_at >= $2 - interval '1 hour'
	), answer_ids as (
		select distinct id from reached where type = 'ask.answered'
	), answer_events as materialized (
		select e.payload->>'id' as id, e.actor, e.payload->'answer' as answer
		from events e
		where e.type = 'ask.answered' and e.payload->>'id' in (select id from answer_ids)
	), decline_ids as (
		-- A decline carries its own words, so the arm keeps the resolution the event recorded.
		-- An ask holds at most one decline: a resolved ask is not resolved again (api's
		-- transitionAskTx), and the only resolutions anything takes back are the retraction a
		-- document's settlement wrote when the block left (docs.settlementRetraction) and the
		-- withdrawal an issue's close wrote, which its reopen undoes - never a decline. The
		-- distinct on is what holds the list to one item per ask, the latest decline of it,
		-- whatever the events hold.
		select distinct on (id) id, resolution
		from reached
		where type = 'ask.resolved' and resolution->>'kind' = 'declined'
		order by id, (resolution->>'at')::timestamptz desc
	), comment_ids as (
		select id, bool_or(m) as mentioned
		from (
			select id, false as m from reached where type = 'comment.created'
			union all
			select id, true from mentioned
		) s
		group by id
	), message_ids as (
		select id from reached where type in ('message.created', 'message.answered')
		union
		select md.message_id::text
		from message_deliveries md
		where md.session_id = $1 and md.state in ('sent', 'queued')
		  and md.created_at >= $2 - interval '1 hour'
	)
	select 'answer' as kind, a.id::text as id, (a.answer->>'at')::timestamptz as at,
	       null::timestamptz as edited_at, null::timestamptz as deleted_at, null::jsonb as deleted_by,
	       null::jsonb as author, null::text as body, null::text as dispatch_words, a.answer,
	       (select count(distinct ae.answer->>'at') from answer_events ae
	         where ae.id = x.id and ae.actor->>'kind' = 'user' and ae.actor->>'id' = ae.answer->>'user') > 1 as replaced,
	       a.id::text as ask_id, a.question as ask_question,
	       i.key as issue_key, i.title as issue_title, ar.project_key, ar.slug, ar.name,
	       false as mentioned, false as in_comment_thread, false as on_document,
	       false as replies_to_session,
	       ((a.author->>'kind' = 'session' and a.author->>'id' = $1) is true
	       or exists (select 1 from ask_followers f where f.ask_id = a.id and f.session_id = $1)) as owns
	from answer_ids x
	join asks a on a.id::text = x.id
	left join issues i on i.key = a.issue_key
	left join artifacts ar on ar.id = a.artifact_id
	where a.answer is not null and (a.answer->>'at')::timestamptz >= $2
	union all
	select 'comment', c.id::text, c.created_at, c.edited_at, c.deleted_at, c.deleted_by, c.author, c.body, null, null, false,
	       a.id::text, a.question,
	       i.key, i.title, ar.project_key, ar.slug, ar.name,
	       x.mentioned,
	       c.reply_to is not null,
	       c.artifact_id is not null or c.anchor->>'artifact_id' is not null,
	       false,
	       ((a.author->>'kind' = 'session' and a.author->>'id' = $1) is true
	       or exists (select 1 from ask_followers f where f.ask_id = a.id and f.session_id = $1))
	from comment_ids x
	join comments c on c.id::text = x.id
	left join asks a on a.id = c.ask_id
	left join issues i on i.key = c.issue_key
	left join artifacts ar on ar.id = c.artifact_id
	where c.author->>'kind' = 'user' and c.created_at >= $2
	union all
	select 'message', m.id::text, m.created_at, m.edited_at, m.deleted_at, m.deleted_by, m.author, m.body, null, null, false,
	       null, null,
	       i.key, i.title, null, null, null,
	       false, false, false,
	       exists (select 1 from messages p where p.id = m.in_reply_to
	               and p.author->>'kind' = 'session' and p.author->>'id' = $1),
	       false
	from message_ids x
	join messages m on m.id::text = x.id
	left join issues i on i.key = m.issue_key
	where m.author->>'kind' = 'user' and m.created_at >= $2
	union all
	-- A decline has no answer to take a time from, so it sits at the resolution's own time, and
	-- that time, not the event's, is what the window holds it to: the event is written moments
	-- after the resolution it carries.
	select 'declined', a.id::text, (x.resolution->>'at')::timestamptz, null, null, null,
	       x.resolution->'actor', x.resolution->>'reason', x.resolution->>'dispatch_words',
	       null, false,
	       a.id::text, a.question,
	       i.key, i.title, ar.project_key, ar.slug, ar.name,
	       false, false, false,
	       false,
	       ((a.author->>'kind' = 'session' and a.author->>'id' = $1) is true
	       or exists (select 1 from ask_followers f where f.ask_id = a.id and f.session_id = $1))
	from decline_ids x
	join asks a on a.id::text = x.id
	left join issues i on i.key = a.issue_key
	left join artifacts ar on ar.id = a.artifact_id
	where (x.resolution->>'at')::timestamptz >= $2
	order by at desc, id desc`

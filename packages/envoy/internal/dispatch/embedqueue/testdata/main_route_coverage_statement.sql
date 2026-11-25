
		with judged as (
			select ('{document_block,comment_block,message_block,ask_part}'::text[])[position] as kind, count(*) as items, count(*) filter (where current) as current,
			       count(*) filter (where dead and not current) as failed,
			       min(greatest(pending_since, written_at)) filter (where not current and not dead) as waited_since
			from (select array_position('{document_block,comment_block,message_block,ask_part}'::text[], kind) as position, pending_since as written_at, pending_since, dead,
			             coalesce(embedding is not null and embedded_hash = content_hash and $1::text <> '' and model = $1::text, false) as current
			      from embeddings
			      where (kind ~ '^(document_block|comment_block|message_block|ask_part)$' and text_snapshot ~ '[^[:space:]]' and embedding is not null and embedded_hash = content_hash) or (kind ~ '^(document_block|comment_block|message_block|ask_part)$' and text_snapshot ~ '[^[:space:]]' and (embedding is null or embedded_hash is distinct from content_hash))) windows
			group by position
			union all
			select kind, count(*) as items, count(*) filter (where current) as current,
			       count(*) filter (where dead and not current) as failed,
			       min(greatest(pending_since, written_at)) filter (where not current and not dead) as waited_since
			from (select 'issue'::text as kind, i.created_at as written_at, e.pending_since, coalesce(e.dead, false) as dead,
			             coalesce($1::text <> '' and e.embedding is not null and e.model = $1::text and e.embedded_hash = case when r.source = (coalesce(spec.id::text, '-') || ':' || coalesce(spec.xmin::text, '-') || ':' || coalesce(e.content_hash, '-') || ':' || i.title) then e.content_hash else encode(sha256(convert_to(issue_embedding_text(i.title, issue_embedding_spec(i.key)), 'UTF8')), 'hex') end, false) as current
			      from issues i
	left join lateral (
		select v.id, v.xmin
		from artifact_versions v
		where v.artifact_id = (select a.id from artifacts a where a.issue_key = i.key and a.is_primary)
		order by v.number desc
		limit 1
	) spec on true
	left join embeddings e on e.kind = 'issue' and e.id = i.key
	left join embedding_issue_sources r on r.key = i.key
			      where (ascii(i.title) between 33 and 126 or issue_embedding_text(i.title, issue_embedding_spec(i.key)) ~ '[^[:space:]]')) issue_items
			group by kind
		)
		select kind, true, sum(items)::bigint, sum(current)::bigint, sum(failed)::bigint,
		       floor(extract(epoch from now() - min(waited_since)))::bigint
		from judged
		group by kind
		union all select 'component'::text, false, ((select count(*) from components))::bigint, 0::bigint, 0::bigint, null::bigint
 union all select 'session'::text, false, (coalesce((select jsonb_array_length(sessions) from route_roster), 0))::bigint, 0::bigint, 0::bigint, null::bigint
 union all select 'document_version'::text, false, ((select count(*) from artifact_versions))::bigint, 0::bigint, 0::bigint, null::bigint
 union all select 'result'::text, false, ((select count(*) from check_results))::bigint, 0::bigint, 0::bigint, null::bigint
 union all select 'handoff'::text, false, ((select count(*) from handoffs))::bigint, 0::bigint, 0::bigint, null::bigint
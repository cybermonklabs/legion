
		with items as (select 'issue'::text as kind, item.id, item.hash, item.written_at from (select s.id, s.text, encode(sha256(convert_to(s.text, 'UTF8')), 'hex') as hash, s.written_at
	from (select i.key as id, issue_embedding_text(i.title, issue_embedding_spec(i.key)) as text, i.created_at as written_at from issues i) s where not (s.text !~ '[^[:space:]]')) item
 union all select 'document_block'::text as kind, item.id, item.hash, item.written_at from (select s.id, s.text, s.hash, s.written_at from (
		select e.id, e.text_snapshot as text, e.content_hash as hash, e.pending_since as written_at
		from embeddings e where e.kind = 'document_block') s where not (s.text !~ '[^[:space:]]')) item
 union all select 'comment_block'::text as kind, item.id, item.hash, item.written_at from (select s.id, s.text, s.hash, s.written_at from (
		select e.id, e.text_snapshot as text, e.content_hash as hash, e.pending_since as written_at
		from embeddings e where e.kind = 'comment_block') s where not (s.text !~ '[^[:space:]]')) item
 union all select 'message_block'::text as kind, item.id, item.hash, item.written_at from (select s.id, s.text, s.hash, s.written_at from (
		select e.id, e.text_snapshot as text, e.content_hash as hash, e.pending_since as written_at
		from embeddings e where e.kind = 'message_block') s where not (s.text !~ '[^[:space:]]')) item
 union all select 'ask_part'::text as kind, item.id, item.hash, item.written_at from (select s.id, s.text, s.hash, s.written_at from (
		select e.id, e.text_snapshot as text, e.content_hash as hash, e.pending_since as written_at
		from embeddings e where e.kind = 'ask_part') s where not (s.text !~ '[^[:space:]]')) item),
		judged as (
			select item.kind, item.written_at, e.pending_since, coalesce(e.dead, false) as dead,
			       coalesce($1::text <> '' and e.embedding is not null and e.model = $1::text and e.embedded_hash = item.hash, false) as current
			from items item
			left join embeddings e on e.kind = item.kind and e.id = item.id
		)
		select kind, true, count(*), count(*) filter (where current), count(*) filter (where dead and not current),
		       floor(extract(epoch from now() - min(greatest(pending_since, written_at)) filter (where not current and not dead)))::bigint
		from judged
		group by kind
		union all select 'component'::text, false, ((select count(*) from components))::bigint, 0::bigint, 0::bigint, null::bigint
 union all select 'session'::text, false, (coalesce((select jsonb_array_length(sessions) from route_roster), 0))::bigint, 0::bigint, 0::bigint, null::bigint
 union all select 'document_version'::text, false, ((select count(*) from artifact_versions))::bigint, 0::bigint, 0::bigint, null::bigint
 union all select 'result'::text, false, ((select count(*) from check_results))::bigint, 0::bigint, 0::bigint, null::bigint
 union all select 'handoff'::text, false, ((select count(*) from handoffs))::bigint, 0::bigint, 0::bigint, null::bigint
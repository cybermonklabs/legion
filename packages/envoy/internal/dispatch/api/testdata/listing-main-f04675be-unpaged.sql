with recursive advances_relations as (
		select newest.from_id as key, newest.to_id as priority
		from (
			select distinct on (t.from_id, t.to_id) t.from_id, t.to_id, t.asserted
		from priority_tuples t
		where t.from_kind = 'issue' and t.kind = 'advances'
			and t.to_kind = 'priority'
			and (cardinality($11::text[]) > 0 or $12::boolean or $13::boolean)
		order by t.from_id, t.to_id, t.seq desc
		) newest
		join priority_nodes n on n.kind = 'priority' and n.id = newest.to_id
			and n.state = 'live'
		where newest.asserted
	), advances_stated as (
		select key, bool_or(priority = any($11::text[])) as named
		from advances_relations
		group by key
	), advances_spread as (
		select s.key, s.named, 0 as depth from advances_stated s where not $15::boolean
		union all
		select c.key, a.named, a.depth + 1
		from advances_spread a join issues c on c.parent_key = a.key
		where a.depth < 32
	), advancing as (
		select key, bool_or(named) as named from advances_spread group by key
	), advances_origin_keys as (
		select key from advances_stated where $15::boolean and (named or $13::boolean)
	), advances_above as (
		select k.key, i.parent_key as above, 1 as depth
		from advances_origin_keys k join issues i on i.key = k.key
		where i.parent_key is not null
		union all
		select a.key, p.parent_key, a.depth + 1
		from advances_above a join issues p on p.key = a.above
		where p.parent_key is not null and a.depth < 32
	), advances_origins as (
		select r.key, r.priority
		from advances_relations r
		where r.key in (select key from advances_origin_keys)
			and not exists (
				select 1
				from advances_above a
				join advances_relations up on up.key = a.above and up.priority = r.priority
				where a.key = r.key
			)
	), advances_originating as (
		select key, bool_or(priority = any($11::text[])) as named
		from advances_origins
		group by key
	), priority_candidates as (
		select i.key from issues i
		where (cardinality($7::smallint[]) > 0 or $8::boolean) and i.priority is null
		  and ($1 = '' or i.project_key = $1) and (not $6::boolean or i.closed_at is null)
	), priority_inherited_chain as (
		select s.key as start, i.key, i.parent_key, i.priority, 0 as depth
		from priority_candidates s join issues i on i.key = s.key
		union all
		select c.start, p.key, p.parent_key, p.priority, c.depth + 1
		from priority_inherited_chain c join issues p on p.key = c.parent_key
		where c.priority is null and c.depth < 32
	), priority_inherited as (
		select c.start as key, max(c.priority) as priority,
		       max(c.key) filter (where c.depth > 0) as source
		from priority_inherited_chain c
		where c.priority is not null
		group by c.start
	), matched as (
		select i.ctid as address
		from issues i
		where ($1 = '' or i.project_key = $1)
		  and (cardinality($2::text[]) = 0 or i.status = any($2::text[]))
		  and ($3 = '' or i.parent_key = $3)
		  and ($4::timestamptz is null or i.updated_at >= $4)
		  and ($5::text[] = '{}' or (select array_agg(translate(label, $16::text, $17::text)) from unnest(i.labels) as label) @> $5)
		  and (not $6::boolean or i.closed_at is null)
		  and ((cardinality($7::smallint[]) = 0 and not $8::boolean)
		       or i.priority = any($7::smallint[])
		       or i.key in (select key from priority_inherited where priority = any($7::smallint[]))
		       or ($8::boolean and i.priority is null and i.key not in (select key from priority_inherited)))
		  and ($9::timestamptz is null or i.closed_at >= $9)
		  and ($10::timestamptz is null or i.closed_at < $10)
		  and ((cardinality($11::text[]) = 0 and not $12::boolean and not $13::boolean)
		       or (not $15::boolean and (i.key in (select key from advancing where named or $13::boolean)
		           or ($12::boolean and i.key not in (select key from advancing))))
		       or ($15::boolean and i.key in (select key from advances_originating where named or $13::boolean)))
	), filtered as (
		select i.key, i.status, i.rank, i.created_at
		from (select array_agg(address) as addresses from matched) m
		cross join generate_subscripts(m.addresses, 1) as place(n)
		join issues i on i.ctid = m.addresses[place.n]
	), subjects as (
		select key from filtered
	), advances_chain as (
		select s.key as start, s.key as issue_key, 0 as depth
		from subjects s
		union all
		select c.start, p.parent_key, c.depth + 1
		from advances_chain c join issues p on p.key = c.issue_key
		where p.parent_key is not null and c.depth < 32
	), advances_current as (
		select distinct on (t.from_id, t.to_id) t.from_id, t.to_id, t.seq, t.asserted
		from priority_tuples t
		where t.from_kind = 'issue' and t.kind = 'advances'
			and t.to_kind = 'priority'
			and t.from_id in (select issue_key from advances_chain)
		order by t.from_id, t.to_id, t.seq desc
	), advances as (
		select c.start as issue_key,
		       coalesce(array_agg(distinct cu.to_id collate "C" order by cu.to_id collate "C")
		                filter (where cu.asserted and live.id is not null and c.depth = 0), '{}') as ids,
		       coalesce(max(cu.seq) filter (where c.depth = 0), 0) as revision,
		       coalesce(array_agg(distinct cu.to_id collate "C" order by cu.to_id collate "C")
		                filter (where cu.asserted and live.id is not null and c.depth > 0), '{}') as inherited
		from advances_chain c
		left join advances_current cu on cu.from_id = c.issue_key
		left join priority_nodes live on live.kind = 'priority' and live.id = cu.to_id
			and live.state = 'live'
		group by c.start
	), comp_chain as (
		select s.key as start, i.project_key, i.key, i.parent_key, 0 as depth
		from subjects s join issues i on i.key = s.key
		union all
		select c.start, c.project_key, p.key, p.parent_key, c.depth + 1
		from comp_chain c join issues p on p.key = c.parent_key and p.project_key = c.project_key
		where c.depth < 32
	), comp_owner as (
		select distinct on (c.start) c.start as key, c.key as owner_key, ic.mode, ic.reason
		from comp_chain c join issue_components ic on ic.issue_key = c.key
		order by c.start, c.depth
	), comp as (
		select o.key, o.owner_key, o.mode, o.reason,
		       coalesce(array_agg(m.component_id order by m.component_id) filter (where x.id is not null), '{}'::text[]) as ids,
		       coalesce(array_agg(m.component_id order by m.component_id) filter (where m.component_id is not null and x.id is null), '{}'::text[]) as unknown
		from comp_owner o
		left join issue_component_members m on m.issue_key = o.owner_key
		left join components x on x.project_key = m.project_key and x.id = m.component_id
		group by o.key, o.owner_key, o.mode, o.reason
	), repo_chain as (
		select s.key as start, i.project_key, i.parent_key, i.repo, 0 as depth
		from subjects s join issues i on i.key = s.key
		union all
		select c.start, c.project_key, p.parent_key, p.repo, c.depth + 1
		from repo_chain c join issues p on p.key = c.parent_key and p.project_key = c.project_key
		where c.repo is null and c.depth < 32
	), repo as (
		select c.start as key,
		       max(c.repo) as owned,
		       bool_or(c.depth = 32 and c.repo is null and c.parent_key is not null) as too_deep
		from repo_chain c
		group by c.start
	), priority_resolved_chain as (
		select s.key as start, i.key, i.parent_key, i.priority, 0 as depth
		from subjects s join issues i on i.key = s.key
		union all
		select c.start, p.key, p.parent_key, p.priority, c.depth + 1
		from priority_resolved_chain c join issues p on p.key = c.parent_key
		where c.priority is null and c.depth < 32
	), priority_resolved as (
		select c.start as key, max(c.priority) as priority,
		       max(c.key) filter (where c.depth > 0) as source
		from priority_resolved_chain c
		where c.priority is not null
		group by c.start
	)
	select i.key, i.title, i.status, i.priority, i.rank, i.labels, i.parent_key,
	       coalesce(blockers.keys, '{}'),
	       coalesce(advances.ids, '{}'), coalesce(advances.revision, 0), coalesce(advances.inherited, '{}'),
	       moved.created_at, moved.actor,
	       priority_resolved.priority, priority_resolved.source,
	       i.assignee, i.route, i.updated_at, i.last_seq, i.created_by, i.created_at,
	       coalesce(spec.id, ''),
	       i.claimed_by, i.claimed_at,
	       case when i.closed_at is null then coalesce(open_asks.total, 0) else 0 end,
	       i.tasks_done, i.tasks_total, coalesce(progress.children_done, 0), coalesce(progress.children_total, 0),
	       comp.owner_key, comp.mode, comp.reason, comp.ids, comp.unknown,
	       i.repo, coalesce(repo.owned, repo_default.default_repo), coalesce(repo.too_deep, false),
	       i.closed_at
	from filtered join issues i on i.key = filtered.key
	left join (
		select l.issue_key, array_agg(l.target_key order by l.target_key) as keys
		from issue_links l
		where l.kind = 'blocked_by' and l.issue_key in (select key from subjects)
		group by l.issue_key
	) blockers on blockers.issue_key = i.key
	left join (
		select a.issue_key, a.id::text as id
		from artifacts a
		where a.is_primary and a.issue_key in (select key from subjects)
	) spec on spec.issue_key = i.key
	left join (
		select a.issue_key, count(*) as total
		from asks a
		where a.state = 'open' and a.issue_key in (select key from subjects)
		group by a.issue_key
	) open_asks on open_asks.issue_key = i.key
	left join (
		select c.parent_key, count(*) filter (where c.status = 'done') as children_done, count(*) as children_total
		from issues c
		where c.parent_key in (select key from subjects)
		group by c.parent_key
	) progress on progress.parent_key = i.key
	left join advances on advances.issue_key = i.key
	left join comp on comp.key = i.key
	left join repo on repo.key = i.key
	left join projects repo_default on repo_default.key = i.project_key
	left join priority_resolved on priority_resolved.key = i.key
	left join lateral (
			select e.created_at, e.actor
			from events e
			where $14::boolean and i.status in ('icebox', 'backlog')
			and e.issue_key = i.key and e.type = 'issue.updated'
				and e.payload->'changed' @> '"status"'::jsonb
				and e.payload->>'status' in ('icebox', 'backlog')
			order by e.seq desc
			limit 1
		) moved on true
	order by case i.status
		when 'triage' then 1
		when 'icebox' then 2
		when 'backlog' then 3
		when 'todo' then 4
		when 'in_progress' then 5
		when 'testing' then 6
		when 'needs_review' then 7
		when 'retro' then 8
		when 'done' then 9
		end, i.rank asc, i.created_at asc, i.key asc

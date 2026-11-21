-- asof_corpus_derive.test.sql: the present rows that a read at a cut joins on, derived from a
-- copy of production's event log, for TestAsOfCostOnACorpusCopy (as_of_cost_test.go). It is a
-- test's fixture, so the route-writer guard (TestOnlyTheIssuePatchWritesTheRoute) leaves it out.
--
-- The copy is the table corpus.events, which holds the columns of GET /api/v1/events/log: id,
-- issue_key, artifact_id, project, seq, type, actor, payload and created_at. The shadows read the
-- present tables beside events (decision 2 of INFRA-4620's plan), and an event refers to its
-- issue, its document and its project. So the file writes, in this order: projects, issues,
-- artifacts, artifact_versions and document_search, events, asks, comments and messages. Each row
-- takes the columns that the shadows and the routes read from the events of its item: its first
-- event for the columns that no write changes, its newest event for the others. No event holds the
-- text of a document, so the markdown of each document version is empty. No event holds an
-- embedding, a delivery's row, a priority tuple or a link's row, so those tables stay empty.
--
-- The test runs the file once, in one transaction, on a migrated database whose events table is
-- empty. The triggers of these tables are off while the file writes: some of them queue work for
-- the server's loops (block indexes, reference grammar, search term statistics, embeddings), and
-- the file writes the columns that the others compute (search vectors, ref_key) itself, with the
-- functions those triggers call.
begin;
set local statement_timeout = 0;
set local lock_timeout = 0;

alter table public.projects disable trigger user;
alter table public.issues disable trigger user;
alter table public.artifacts disable trigger user;
alter table public.artifact_versions disable trigger user;
alter table public.document_search disable trigger user;
alter table public.events disable trigger user;
alter table public.asks disable trigger user;
alter table public.comments disable trigger user;
alter table public.messages disable trigger user;

create index if not exists corpus_events_type on corpus.events (type, id);

-- The projects: each project that an event names, or that an issue key starts with.
insert into public.projects (key, name, next_number, created_at, last_seq, default_assignee, gated_steps)
select p.key,
       coalesce(c.payload->>'name', p.key),
       1,
       least(p.first_at, c.created_at),
       coalesce(p.own_seq, 0),
       coalesce(lower(nullif(d.payload->>'assignee', '')), 'nobody@example.invalid'),
       coalesce(array(select s.step from jsonb_array_elements_text(
                          case jsonb_typeof(g.payload->'gated_steps') when 'array' then g.payload->'gated_steps' else '[]'::jsonb end) as s(step)
                      where s.step in ('decisions', 'criteria', 'spec')), '{}')
  from (select n.key, min(n.created_at) as first_at, max(n.own_seq) as own_seq
          from (select e.project as key, e.created_at,
                       case when e.issue_key is null and e.artifact_id is null then e.seq end as own_seq
                  from corpus.events e where coalesce(e.project, '') <> ''
                union all
                select split_part(e.issue_key, '-', 1), e.created_at, null::integer
                  from corpus.events e where e.issue_key is not null) n
         where n.key ~ '^[A-Z][A-Z0-9]{1,9}$'
         group by n.key) p
  left join lateral (select e.payload, e.created_at from corpus.events e
                      where e.type = 'project.created' and e.payload->>'key' = p.key order by e.id limit 1) c on true
  left join lateral (select e.payload from corpus.events e
                      where e.type = 'settings.project_default_assignee.updated' and e.issue_key is null
                        and e.artifact_id is null and e.project = p.key
                      order by e.id desc limit 1) d on true
  left join lateral (select e.payload from corpus.events e
                      where e.type in ('project.created', 'project.updated') and e.payload->>'key' = p.key
                      order by e.id desc limit 1) g on true;

-- The issues: each issue that an event names. The columns that no write changes come from its
-- issue.created; the others from its newest whole-issue event by seq, and its claim from the newer
-- of that event and its newest issue.claimed or issue.released.
create temporary table corpus_issue_state on commit drop as
select distinct on (e.issue_key) e.issue_key, e.seq, e.payload
  from corpus.events e
 where e.type in ('issue.created', 'issue.updated', 'issue.closed') and e.issue_key is not null
 order by e.issue_key, e.seq desc;

insert into public.issues (key, project_key, number, title, status, labels, route, created_by, created_at, updated_at,
                           closed_at, last_seq, search, rank, priority, assignee, claimed_by, claimed_at, title_lexemes, repo)
select k.key,
       k.project_key,
       coalesce(case when s.payload->>'number' ~ '^[0-9]{1,9}$' then (s.payload->>'number')::integer end,
                case when k.key ~ '-[0-9]{1,9}$' then substring(k.key from '-([0-9]+)$')::integer end),
       x.title,
       coalesce(s.payload->>'status', 'triage'),
       coalesce(array(select jsonb_array_elements_text(
                          case jsonb_typeof(s.payload->'labels') when 'array' then s.payload->'labels' else '[]'::jsonb end)), '{}'),
       s.payload->>'route',
       coalesce(nullif(c.payload->'created_by', 'null'::jsonb), nullif(s.payload->'created_by', 'null'::jsonb), c.actor, '{}'::jsonb),
       coalesce(c.created_at, k.first_at),
       greatest(k.last_at, (s.payload->>'updated_at')::timestamptz),
       (s.payload->>'closed_at')::timestamptz,
       k.last_seq,
       public.issue_search_vector(k.key, x.title, null),
       coalesce(s.payload->>'rank', ''),
       case when s.payload->>'priority' ~ '^[0-3]$' then (s.payload->>'priority')::smallint end,
       s.payload->>'assignee',
       case when cl.complete then cl.claim->'actor' end,
       case when cl.complete then (cl.claim->>'at')::timestamptz end,
       public.title_lexemes(x.title),
       s.payload->>'repo'
  from (select e.issue_key as key,
               coalesce(min(e.project) filter (where coalesce(e.project, '') <> ''), split_part(e.issue_key, '-', 1)) as project_key,
               min(e.created_at) as first_at, max(e.created_at) as last_at, max(e.seq) as last_seq
          from corpus.events e where e.issue_key is not null group by e.issue_key) k
  left join (select distinct on (e.issue_key) e.issue_key, e.payload, e.created_at, e.actor
               from corpus.events e where e.type = 'issue.created' and e.issue_key is not null
              order by e.issue_key, e.id) c on c.issue_key = k.key
  left join corpus_issue_state s on s.issue_key = k.key
  left join (select distinct on (e.issue_key) e.issue_key, e.payload, e.seq
               from corpus.events e where e.type in ('issue.claimed', 'issue.released') and e.issue_key is not null
              order by e.issue_key, e.seq desc) l on l.issue_key = k.key
  cross join lateral (select coalesce(s.payload->>'title', '') as title) x
  cross join lateral (select case when l.seq > coalesce(s.seq, -1) then l.payload->'claim' else s.payload->'claim' end as claim) y
  cross join lateral (select y.claim as claim,
                             jsonb_typeof(y.claim) = 'object' and jsonb_typeof(y.claim->'actor') = 'object'
                               and y.claim->>'at' is not null as complete) cl;

update public.issues i
   set parent_key = s.payload->>'parent'
  from corpus_issue_state s
 where s.issue_key = i.key and s.payload->>'parent' <> i.key
   and exists (select from public.issues p where p.key = s.payload->>'parent');

update public.projects p
   set next_number = coalesce((select max(i.number) + 1 from public.issues i where i.project_key = p.key), 1);

-- The documents: each document that an artifact.created names, each issue's primary document,
-- and each one an event refers to that none names, with what its events give. An issue's primary
-- document, its spec, is written with the issue (api/issue_create.go: slug spec, name spec.md), and
-- the issue's events name it (primary_artifact_id); no artifact.created does, and no
-- artifact.version holds its version 1, which the issue's create writes at its own time. A slug
-- that two documents of one owner hold (a document removed and made again) takes the document's
-- id after it, so ref_key stays unique.
create temporary table corpus_artifact_created on commit drop as
select distinct on (e.payload->'artifact'->>'id')
       (e.payload->'artifact'->>'id')::uuid as id, e.payload->'artifact' as artifact, e.created_at, e.actor, e.project
  from corpus.events e
 where e.type = 'artifact.created' and pg_input_is_valid(e.payload->'artifact'->>'id', 'uuid')
 order by e.payload->'artifact'->>'id', e.id;

create temporary table corpus_artifact_versions on commit drop as
select (e.payload->>'artifact_id')::uuid as artifact_id, e.payload->'version' as version, e.payload->>'name' as name,
       e.created_at, e.id as event_id, e.project, e.issue_key
  from corpus.events e
 where e.type = 'artifact.version' and pg_input_is_valid(e.payload->>'artifact_id', 'uuid')
   and jsonb_typeof(e.payload->'version') = 'object';

create temporary table corpus_primary on commit drop as
select distinct on (p.id) p.id, p.issue_key, i.project_key, i.created_at, i.created_by
  from (select (s.payload->>'primary_artifact_id')::uuid as id, s.issue_key
          from corpus_issue_state s where pg_input_is_valid(s.payload->>'primary_artifact_id', 'uuid')) p
  join public.issues i on i.key = p.issue_key
 order by p.id, i.created_at desc;

insert into public.artifacts (id, issue_key, slug, name, kind, is_primary, created_by, created_at, project_key, ref_key,
                              last_seq, session_id)
select r.id,
       r.issue_key,
       r.slug || case when r.taken > 1 then '~' || left(r.id::text, 8) else '' end,
       r.name,
       r.kind,
       r.is_primary and r.primary_rank = 1,
       r.created_by,
       r.created_at,
       r.project_key,
       coalesce(r.issue_key, r.project_key, 'agent/' || r.session_id) || '/' || r.slug
         || case when r.taken > 1 then '~' || left(r.id::text, 8) else '' end,
       r.last_seq,
       r.session_id
  from (select o.*,
               row_number() over (partition by coalesce(o.issue_key, o.project_key, 'agent/' || o.session_id), o.slug
                                  order by o.created_at, o.id) as taken,
               row_number() over (partition by o.issue_key, o.is_primary order by o.created_at desc, o.id) as primary_rank
          from (select n.id,
                       case when n.session_id is null and exists (select from public.issues i where i.key = n.issue_key)
                            then n.issue_key end as issue_key,
                       case when n.session_id is null then n.project_key end as project_key,
                       n.session_id,
                       n.slug, coalesce(n.name, n.slug) as name, n.kind,
                       coalesce(n.is_primary, false) and n.kind = 'doc' and n.session_id is null
                         and exists (select from public.issues i where i.key = n.issue_key) as is_primary,
                       n.created_by, n.created_at, n.last_seq
                  from (select ids.id,
                               case when p.id is null then nullif(c.artifact->>'session_id', '') end as session_id,
                               coalesce(p.issue_key, c.artifact->>'issue_key', ids.issue_key) as issue_key,
                               coalesce(p.project_key, nullif(c.artifact->>'project', ''), nullif(c.project, ''), ids.project,
                                        split_part(coalesce(c.artifact->>'issue_key', ids.issue_key), '-', 1)) as project_key,
                               case when p.id is not null then 'spec'
                                    else coalesce(nullif(c.artifact->>'slug', ''), 'corpus-' || ids.id::text) end as slug,
                               case when p.id is not null then 'spec.md'
                                    else coalesce(nullif(c.artifact->>'name', ''), v.name) end as name,
                               case when p.id is not null then 'doc'
                                    when c.artifact->>'kind' in ('doc', 'image', 'file') then c.artifact->>'kind'
                                    when v.version->>'mime' like 'image/%' then 'image'
                                    when v.version->>'mime' is null or v.version->>'mime' like 'text/markdown%' then 'doc'
                                    else 'file' end as kind,
                               p.id is not null or coalesce((c.artifact->>'primary')::boolean, false) as is_primary,
                               coalesce(p.created_by, nullif(c.artifact->'created_by', 'null'::jsonb), c.actor, '{}'::jsonb) as created_by,
                               coalesce(p.created_at, c.created_at, ids.first_at) as created_at,
                               coalesce(ids.own_seq, 0) as last_seq
                          from (select r.id, min(r.created_at) as first_at,
                                       min(r.project) filter (where coalesce(r.project, '') <> '') as project,
                                       min(r.issue_key) as issue_key, max(r.own_seq) as own_seq
                                  from (select e.artifact_id as id, e.created_at, e.project, null::text as issue_key,
                                               e.seq as own_seq
                                          from corpus.events e where e.artifact_id is not null
                                        union all
                                        select v.artifact_id, v.created_at, v.project, v.issue_key, null
                                          from corpus_artifact_versions v
                                        union all
                                        select c.id, c.created_at, c.project, c.artifact->>'issue_key', null
                                          from corpus_artifact_created c
                                        union all
                                        select p.id, p.created_at, p.project_key, p.issue_key, null
                                          from corpus_primary p) r
                                 group by r.id) ids
                          left join corpus_artifact_created c on c.id = ids.id
                          left join corpus_primary p on p.id = ids.id
                          left join (select distinct on (v.artifact_id) v.artifact_id, v.version, v.name
                                       from corpus_artifact_versions v
                                      order by v.artifact_id, v.event_id desc) v on v.artifact_id = ids.id) n
                 where n.session_id is not null or n.project_key ~ '^[A-Z][A-Z0-9]{1,9}$') o) r;

-- The versions of each document: those its artifact.created holds, those each artifact.version
-- adds, each made at its event's time, and version 1 of each primary document, made with its
-- issue, where no event holds it. A document version's markdown is empty, and its vector the empty
-- vector: no event holds a document's text. document_search holds each document's newest version,
-- as its trigger keeps it.
insert into public.artifact_versions (artifact_id, number, markdown, mime, size, sha256, authors, named, summary, created_at, search)
select x.artifact_id,
       (x.version->>'number')::integer,
       case when a.kind = 'doc' then '' end,
       x.version->>'mime',
       case when x.version->>'size' ~ '^[0-9]{1,9}$' then (x.version->>'size')::integer end,
       x.version->>'sha256',
       case jsonb_typeof(x.version->'authors') when 'array' then x.version->'authors' else '[]'::jsonb end,
       coalesce((x.version->>'named')::boolean, false),
       x.version->>'summary',
       x.created_at,
       case when a.kind = 'doc' then ''::tsvector end
  from (select v.artifact_id, v.version, v.created_at, v.event_id from corpus_artifact_versions v
        union all
        select c.id, n.version, c.created_at, 0
          from corpus_artifact_created c
         cross join jsonb_array_elements(case jsonb_typeof(c.artifact->'versions') when 'array' then c.artifact->'versions'
                                              else '[]'::jsonb end) as n(version)
         where jsonb_typeof(n.version) = 'object'
        union all
        select p.id, jsonb_build_object('number', 1, 'mime', 'text/markdown'), p.created_at, 9223372036854775807
          from corpus_primary p) x
  join public.artifacts a on a.id = x.artifact_id
 where x.version->>'number' ~ '^[0-9]{1,9}$'
 order by x.artifact_id, (x.version->>'number')::integer, x.event_id
    on conflict (artifact_id, number) do nothing;

insert into public.document_search (artifact_id, number, created_at, search)
select distinct on (v.artifact_id) v.artifact_id, v.number, v.created_at, v.search
  from public.artifact_versions v
 where v.markdown is not null
 order by v.artifact_id, v.number desc;

-- The event log itself. A project's own event names its project; every other event names its
-- issue, its document or nothing, as the log answered it. Each is published, so no outbox picks it.
insert into public.events (id, issue_key, seq, type, actor, payload, notify, published_at, created_at, artifact_id, project_key)
select e.id, e.issue_key, coalesce(e.seq, 0), e.type, e.actor, e.payload, false, e.created_at, e.created_at, e.artifact_id,
       case when e.issue_key is null and e.artifact_id is null and coalesce(e.project, '') <> '' then e.project end
  from corpus.events e
 order by e.id;

select setval(pg_get_serial_sequence('public.events', 'id'), (select max(e.id) from public.events e));

-- The asks: each ask an ask.opened names, its columns from its newest event of its state.
create temporary table corpus_ask_opened on commit drop as
select distinct on (e.payload->>'id') (e.payload->>'id')::uuid as id, e.payload, e.issue_key, e.artifact_id, e.created_at, e.actor
  from corpus.events e
 where e.type = 'ask.opened' and pg_input_is_valid(e.payload->>'id', 'uuid')
 order by e.payload->>'id', e.id;

create temporary table corpus_ask_state on commit drop as
select distinct on (e.payload->>'id') (e.payload->>'id')::uuid as id, e.payload
  from corpus.events e
 where e.type in ('ask.opened', 'ask.answered', 'ask.edited', 'ask.resolved', 'ask.handed_back', 'ask.asked_of', 'ask.anchor_refreshed')
   and pg_input_is_valid(e.payload->>'id', 'uuid')
 order by e.payload->>'id', e.id desc;

create temporary table corpus_asks on commit drop as
select o.id,
       o.issue_key,
       coalesce(nullif(o.payload->'author', 'null'::jsonb), o.actor) as author,
       coalesce(s.payload->>'question', '') as question,
       case jsonb_typeof(s.payload->'options') when 'array' then s.payload->'options' else '[]'::jsonb end as options,
       coalesce((s.payload->>'multiple')::boolean, false) as multiple,
       coalesce(s.payload->>'urgency', 'med') as urgency,
       nullif(s.payload->'anchor', 'null'::jsonb) as anchor,
       t.state,
       case when t.state = 'answered' then coalesce(nullif(s.payload->'answer', 'null'::jsonb), '{}'::jsonb) end as answer,
       coalesce((o.payload->>'created_at')::timestamptz, o.created_at) as created_at,
       case when t.state = 'resolved' then coalesce(nullif(s.payload->'resolution', 'null'::jsonb), '{}'::jsonb) end as resolution,
       o.artifact_id,
       (s.payload->>'edited_at')::timestamptz as edited_at,
       t.kind,
       case when t.kind = 'approval' then s.payload->'approval' end as approval,
       s.payload->>'block_id' as block_id,
       (select a.id from public.artifacts a
         where pg_input_is_valid(s.payload->'block_artifact'->>'id', 'uuid') and a.id = (s.payload->'block_artifact'->>'id')::uuid) as block_artifact_id,
       s.payload->>'asked_of' as asked_of,
       s.payload->>'asked_of_by' as asked_of_by,
       (s.payload->>'asked_of_at')::timestamptz as asked_of_at
  from corpus_ask_opened o
  join corpus_ask_state s on s.id = o.id
 cross join lateral (select case when s.payload->>'state' in ('open', 'answered', 'resolved') then s.payload->>'state' else 'open' end as state,
                            case when coalesce(s.payload->>'kind', o.payload->>'kind') = 'approval' then 'approval' else 'question' end as kind) t
 where (o.issue_key is null) <> (o.artifact_id is null);

-- An approval of an older shape lacks the documents and the requested version that migrations
-- filled in its row; the file fills them as those migrations did (asks_default_approval_requested_version,
-- asks_normalize_approval_documents). An ask the table's checks still refuse is written as a
-- question, and one whose block another ask holds is written with no block.
update corpus_asks set approval = jsonb_set(approval, '{requested_version}', approval->'version')
 where kind = 'approval' and jsonb_typeof(approval) = 'object' and not approval ? 'requested_version';
update corpus_asks set approval = jsonb_set(approval, '{documents}', jsonb_build_array(jsonb_build_object(
           'artifact_id', approval->'artifact_id', 'name', approval->'name',
           'version', approval->'version', 'requested_version', approval->'requested_version')))
 where kind = 'approval' and jsonb_typeof(approval) = 'object' and not approval ? 'documents';

do $$
declare
  ask corpus_asks;
begin
  for ask in select * from corpus_asks order by created_at, id loop
    begin
      insert into public.asks (id, issue_key, author, question, options, multiple, urgency, anchor, state, answer, created_at,
                               resolution, artifact_id, edited_at, kind, approval, block_id, block_artifact_id, search,
                               written_version, asked_of, asked_of_by, asked_of_at)
      values (ask.id, ask.issue_key, ask.author, ask.question, ask.options, ask.multiple, ask.urgency, ask.anchor, ask.state,
              ask.answer, ask.created_at, ask.resolution, ask.artifact_id, ask.edited_at, ask.kind, ask.approval, ask.block_id,
              ask.block_artifact_id,
              public.search_vector('', public.search_text(ask.question || ' ' || public.ask_search_suffix(ask.options, ask.answer))),
              case when ask.approval->>'version' ~ '^[1-9][0-9]{0,8}$' then (ask.approval->>'version')::integer end,
              ask.asked_of, ask.asked_of_by, ask.asked_of_at);
    exception when check_violation or unique_violation then
      insert into public.asks (id, issue_key, author, question, options, multiple, urgency, anchor, state, answer, created_at,
                               resolution, artifact_id, edited_at, kind, approval, block_id, block_artifact_id, search,
                               asked_of, asked_of_by, asked_of_at)
      values (ask.id, ask.issue_key, ask.author, ask.question, ask.options, ask.multiple, ask.urgency, ask.anchor, ask.state,
              ask.answer, ask.created_at, ask.resolution, ask.artifact_id, ask.edited_at, 'question', null, null, null,
              public.search_vector('', public.search_text(ask.question || ' ' || public.ask_search_suffix(ask.options, ask.answer))),
              ask.asked_of, ask.asked_of_by, ask.asked_of_at);
    end;
  end loop;
end $$;

-- The comments: each comment a comment.created or comment.answered names, its body and its other
-- changing columns from its newest event of its state; its reply_to and answers once every
-- comment is written. A comment whose newest event is its comment.deleted reads as the delete left
-- it: no body and no suggestion, and its delete mark, from that event.
create temporary table corpus_comment_created on commit drop as
select distinct on (e.payload->>'id') (e.payload->>'id')::uuid as id, e.payload, e.issue_key, e.artifact_id, e.created_at, e.actor
  from corpus.events e
 where e.type in ('comment.created', 'comment.answered') and pg_input_is_valid(e.payload->>'id', 'uuid')
 order by e.payload->>'id', e.id;

create temporary table corpus_comment_state on commit drop as
select distinct on (e.payload->>'id') (e.payload->>'id')::uuid as id, e.payload, e.type, e.created_at, e.actor
  from corpus.events e
 where e.type in ('comment.created', 'comment.answered', 'comment.edited', 'comment.deleted', 'comment.resolved',
                  'comment.reopened', 'comment.anchor_refreshed', 'suggestion.accepted', 'suggestion.rejected')
   and pg_input_is_valid(e.payload->>'id', 'uuid')
 order by e.payload->>'id', e.id desc;

insert into public.comments (id, issue_key, author, body, anchor, resolved, suggestion, created_at, ask_id, artifact_id, search,
                             resolved_by, resolved_at, edited_at, turn, deleted_at, deleted_by)
select c.id,
       c.issue_key,
       coalesce(nullif(c.payload->'author', 'null'::jsonb), c.actor),
       b.body,
       nullif(s.payload->'anchor', 'null'::jsonb),
       coalesce((s.payload->>'resolved')::boolean, false),
       case when s.type <> 'comment.deleted' then nullif(s.payload->'suggestion', 'null'::jsonb) end,
       coalesce((c.payload->>'created_at')::timestamptz, c.created_at),
       k.ask_id,
       c.artifact_id,
       public.search_vector('', public.search_text(b.body)),
       nullif(s.payload->'resolved_by', 'null'::jsonb),
       (s.payload->>'resolved_at')::timestamptz,
       (s.payload->>'edited_at')::timestamptz,
       case when k.ask_id is not null and c.payload->>'turn' in ('human', 'agent') then c.payload->>'turn' end,
       case when s.type = 'comment.deleted' then coalesce((s.payload->>'deleted_at')::timestamptz, s.created_at) end,
       case when s.type = 'comment.deleted' then coalesce(nullif(s.payload->'deleted_by', 'null'::jsonb), s.actor) end
  from corpus_comment_created c
  join corpus_comment_state s on s.id = c.id
 cross join lateral (select case when s.type = 'comment.deleted' then ''
                                else coalesce(s.payload->>'body', c.payload->>'body', '') end as body) b
 cross join lateral (select (select a.id from public.asks a
                              where pg_input_is_valid(c.payload->>'ask_id', 'uuid') and a.id = (c.payload->>'ask_id')::uuid) as ask_id) k
 where (c.issue_key is null) <> (c.artifact_id is null);

update public.comments m
   set reply_to = (c.payload->>'reply_to')::uuid
  from corpus_comment_created c
 where c.id = m.id and pg_input_is_valid(c.payload->>'reply_to', 'uuid')
   and exists (select from public.comments p where p.id = (c.payload->>'reply_to')::uuid);

update public.comments m
   set answers = (c.payload->>'answers')::uuid
  from corpus_comment_created c
 where c.id = m.id and pg_input_is_valid(c.payload->>'answers', 'uuid')
   and exists (select from public.comments p where p.id = (c.payload->>'answers')::uuid);

-- The messages: each message a message.created or message.answered names (message.answered is the
-- reply's own event), made at its event's time, its body from its newest event of its state. A
-- message whose newest event is its message.deleted reads with no body and its delete mark.
create temporary table corpus_message_created on commit drop as
select distinct on (e.payload->>'id') (e.payload->>'id')::uuid as id, e.payload, e.issue_key, e.created_at, e.actor
  from corpus.events e
 where e.type in ('message.created', 'message.answered') and pg_input_is_valid(e.payload->>'id', 'uuid')
 order by e.payload->>'id', e.id;

create temporary table corpus_message_state on commit drop as
select distinct on (e.payload->>'id') (e.payload->>'id')::uuid as id, e.payload, e.type, e.created_at, e.actor
  from corpus.events e
 where e.type in ('message.created', 'message.answered', 'message.edited', 'message.deleted')
   and pg_input_is_valid(e.payload->>'id', 'uuid')
 order by e.payload->>'id', e.id desc;

insert into public.messages (id, issue_key, author, body, created_at, search, target, edited_at, deleted_at, deleted_by)
select m.id,
       o.issue_key,
       coalesce(nullif(m.payload->'author', 'null'::jsonb), m.actor),
       b.body,
       m.created_at,
       public.search_vector('', public.search_text(b.body)),
       nullif(m.payload->>'target', ''),
       (s.payload->>'edited_at')::timestamptz,
       case when s.type = 'message.deleted' then coalesce((s.payload->>'deleted_at')::timestamptz, s.created_at) end,
       case when s.type = 'message.deleted' then coalesce(nullif(s.payload->'deleted_by', 'null'::jsonb), s.actor) end
  from corpus_message_created m
  join corpus_message_state s on s.id = m.id
 cross join lateral (select case when s.type = 'message.deleted' then ''
                                else coalesce(s.payload->>'body', m.payload->>'body', '') end as body) b
 cross join lateral (select (select i.key from public.issues i where i.key = coalesce(m.issue_key, m.payload->>'issue_key')) as issue_key) o
 where o.issue_key is not null or nullif(m.payload->>'target', '') is not null;

update public.messages m
   set in_reply_to = (c.payload->>'in_reply_to')::uuid
  from corpus_message_created c
 where c.id = m.id and pg_input_is_valid(c.payload->>'in_reply_to', 'uuid')
   and exists (select from public.messages p where p.id = (c.payload->>'in_reply_to')::uuid);

alter table public.projects enable trigger user;
alter table public.issues enable trigger user;
alter table public.artifacts enable trigger user;
alter table public.artifact_versions enable trigger user;
alter table public.document_search enable trigger user;
alter table public.events enable trigger user;
alter table public.asks enable trigger user;
alter table public.comments enable trigger user;
alter table public.messages enable trigger user;

analyze public.projects, public.issues, public.artifacts, public.artifact_versions, public.document_search, public.events,
        public.asks, public.comments, public.messages;
commit;

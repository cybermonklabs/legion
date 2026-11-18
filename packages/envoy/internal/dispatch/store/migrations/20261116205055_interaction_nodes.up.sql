-- 20261116205055_interaction_nodes.up.sql
--
-- INFRA-1950: a run of the command bar is an `interaction` node of the store, and `going_requests`
-- and `going_events` are its projections (dispatch://INFRA-1950/spec#b-r-interaction). The going
-- POST and the frame path of this release write the node and each `frame` value before the row
-- that projects it, in one transaction (api/going.go). This migration gives every run written
-- before it the same: for each `going_requests` row whose run the store holds no interaction node
-- of, the node, its author's node and tuples, and a `frame` value for each of its `going_events`
-- rows, in the shape `graphstore.InteractionFacts`, `graphstore.AuthorFacts` and
-- `graphstore.FrameTuple` write and `graphcensus.WriteRun` writes from a row:
--
-- - The node is `interaction:<the run's uuid>`, its supertag `interaction`, reserved for its
--   author (`author`) and `live`, its project the row's where the row names one, made at the row's
--   `created_at` by the row's `author` (`made_by` is the actor the row stored), caused by no event.
--   Its attributes are its `words` and its `ends_at`, the row's, written in UTC as RFC 3339 with
--   the fraction of a second Go's `time.RFC3339Nano` writes (trailing zeros dropped, no fraction
--   where it is zero), and each is also stated as a value tuple.
-- - Its two base attributes, `visibility` and `state`, and its `project` where it has one, are
--   stated by the node's own writer at its `made_at`, as the store's writer states them of a node
--   it makes.
-- - Where `graphstore.AuthorOf` names somebody of the row's author - a `user` actor's login, a
--   `session` actor's session, or the owner of a personal token, a login canonicalised as
--   `user_threads_login` canonicalises one - and that address fits the store's bound (1024 bytes),
--   the run states `authored_by` to that author's `person` or `session` node, which is made where
--   the store holds none: project-visible, `live`, made by the actor at the run's moment, the first
--   run of each author making it. A session the credential proved its owner's (its act names that
--   same session) is tied to that owner by `alias_of`, and the owner's `person` node is made too;
--   a person whose act names a session the server proved (an act with a session and no token id)
--   is tied from it by `alias_of`, where it fits the bound; and a person recorded under a raw id
--   that is not their canonical login carries it as `recorded_as`.
-- - A row whose author names nobody gets the node, reserved for an author nobody proves, which
--   nobody reads, as the stream reads such a row, and no `authored_by`.
-- - Each `going_events` row of the run is one `frame` value, the jsonb text
--   `{"data": …, "name": …, "sequence": …}` (`graphstore.FrameValueSQL`), dated at the row's own
--   `created_at` and stated by `{"kind": "system", "id": "going"}` (`graphstore.FrameWriter`).
--
-- Every tuple carries the `source_event` sentinel 0, and none is a row's legacy sequence. A run
-- whose node the store holds already - one this release's POST wrote, or one a frame of this
-- release gave a node from its row - is left as it is, so running this file again writes nothing.
-- The nodes are inserted in ascending (kind, id), before any tuple, as every writer of the store
-- takes them. A run a process of the release before records after this applies, during the roll,
-- is given its node by the frame path's first frame of it or by `envoy-dispatch backfill-graph-store`
-- (graphcensus's backfillRuns), which find it by the same anti-join.
with runs as (
  select r.id::text as id, r.author, nullif(r.project, '') as project, r.words, r.created_at,
         to_char(r.ends_at at time zone 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS')
           || coalesce('.' || nullif(rtrim(to_char(r.ends_at at time zone 'UTC', 'US'), '0'), ''), '')
           || 'Z' as ends_at
  from going_requests r
  where not exists (select 1 from graph_nodes n where n.kind = 'interaction' and n.id = r.id::text)
),
-- The author graphstore.AuthorOf names of each run, within the store's bound.
authors as (
  select runs.id as run, named.kind, named.id, runs.author, runs.created_at
  from runs
  cross join lateral (
    select case
             when runs.author->>'kind' = 'user' and coalesce(runs.author->>'id', '') <> ''
               then 'person'
             when runs.author->>'kind' = 'session' and coalesce(runs.author->>'id', '') <> ''
               then 'session'
             when coalesce(runs.author->>'owner', '') <> ''
               then 'person'
           end,
           case
             when runs.author->>'kind' = 'user' and coalesce(runs.author->>'id', '') <> ''
               then user_threads_login(runs.author->>'id')
             when runs.author->>'kind' = 'session' and coalesce(runs.author->>'id', '') <> ''
               then runs.author->>'id'
             when coalesce(runs.author->>'owner', '') <> ''
               then user_threads_login(runs.author->>'owner')
           end
  ) as named (kind, id)
  where named.kind is not null and octet_length(named.id) <= 1024
),
-- The owner a session author's credential proved, whose node the write that states the session's
-- authorship makes beside it.
owners as (
  select authors.run, authors.id as session, user_threads_login(authors.author->>'owner') as login,
         authors.author, authors.created_at
  from authors
  where authors.kind = 'session'
    and authors.author->'act'->>'session' = authors.id
    and user_threads_login(coalesce(authors.author->>'owner', '')) <> ''
),
-- Each author's node, and each proved owner's, made by the first run that names it.
made_authors as (
  select distinct on (made.kind, made.id) made.kind, made.id, made.at, made.author
  from (
    select authors.kind, authors.id, authors.created_at as at, authors.author from authors
    union all
    select 'person', owners.login, owners.created_at, owners.author from owners
  ) made
  order by made.kind, made.id, made.at
),
nodes (kind, id, visibility, attributes, project, made_at, made_by) as (
  select 'interaction', runs.id, 'author', jsonb_build_object('words', runs.words, 'ends_at', runs.ends_at),
         runs.project, runs.created_at, runs.author
  from runs
  union all
  select made_authors.kind, made_authors.id, 'project', '{}'::jsonb, null::text, made_authors.at, made_authors.author
  from made_authors
),
written as (
  insert into graph_nodes (kind, id, supertag, visibility, state, attributes, made_at, made_by, source_event, project_key)
  select nodes.kind, nodes.id, nodes.kind, nodes.visibility, 'live', nodes.attributes, nodes.made_at, nodes.made_by, 0,
         nodes.project
  from nodes
  order by nodes.kind, nodes.id
  on conflict (kind, id) do nothing
  returning kind, id, visibility, state, project_key, made_at, made_by
),
facts (from_kind, from_id, kind, to_kind, to_id, at, who) as (
  -- The base attributes of each node this statement made.
  select written.kind, written.id, fact.kind, 'value', fact.value, written.made_at, written.made_by
  from written
  cross join lateral (values ('visibility', written.visibility), ('state', written.state), ('project', written.project_key))
    as fact (kind, value)
  where fact.value is not null
  union all
  -- Each run's words and the moment it ends.
  select 'interaction', runs.id, fact.kind, 'value', fact.value, runs.created_at, runs.author
  from runs
  cross join lateral (values ('words', runs.words), ('ends_at', runs.ends_at)) as fact (kind, value)
  union all
  -- Who wrote each run.
  select 'interaction', authors.run, 'authored_by', authors.kind, authors.id, authors.created_at, authors.author
  from authors
  union all
  -- A proved session tied to its owner.
  select 'session', owners.session, 'alias_of', 'person', owners.login, owners.created_at, owners.author
  from owners
  union all
  -- The session a person's credential proved, tied to that person.
  select 'session', authors.author->'act'->>'session', 'alias_of', 'person', authors.id, authors.created_at, authors.author
  from authors
  where authors.kind = 'person'
    and coalesce(authors.author->'act'->>'token_id', '') = ''
    and coalesce(authors.author->'act'->>'session', '') <> ''
    and octet_length(authors.author->'act'->>'session') <= 1024
  union all
  -- The raw id a person was recorded under where it is not their canonical login.
  select 'person', authors.id, 'recorded_as', 'value', authors.author->>'id', authors.created_at, authors.author
  from authors
  where authors.kind = 'person'
    and coalesce(authors.author->>'id', '') <> ''
    and authors.author->>'id' <> authors.id
  union all
  -- Each frame of each run.
  select 'interaction', runs.id, 'frame', 'value',
         jsonb_build_object('sequence', e.sequence, 'name', e.name, 'data', e.data)::text,
         e.created_at, '{"kind": "system", "id": "going"}'::jsonb
  from runs
  join going_events e on e.request_id = runs.id::uuid
)
insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, legacy_source_seq, asserted)
select facts.from_kind, facts.from_id, facts.kind, facts.to_kind, facts.to_id, facts.at, facts.who, 0, null, true
from facts
-- The written arm is read first, so every node is inserted before the first tuple.
where (select count(*) from written) >= 0
order by facts.from_kind, facts.from_id, facts.kind, facts.to_id;

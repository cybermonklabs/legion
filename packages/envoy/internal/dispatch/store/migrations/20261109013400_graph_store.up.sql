-- 20261109013400_graph_store.up.sql
--
-- The node and tuple store. A node is a thing Dispatch holds; a tuple is one fact stated about
-- it at one moment, by one writer, caused by one event. Nothing updates a tuple and nothing
-- deletes one: a relation that ends is a second tuple with `asserted` false, and an attribute
-- that changes is a second tuple carrying the new value. Every table Dispatch reads today stays
-- where it is and becomes the projection of these two.
--
-- `source_event` holds an `events.id` and is `not null`. It holds 0 for a write no event
-- recorded, the one sentinel this store has: `events.id` is a `bigserial` (`0001_init.up.sql`),
-- so no event ever carries 0, and the value reads as "no event recorded this write".
--
-- `to_kind = 'value'` marks an attribute: `to_id` then holds the value itself rather than an
-- address, and a value has no length bound (a block's text is one). A btree key is bounded at
-- about 2,704 bytes on an 8 KiB page and an insert over that bound fails the statement, so no
-- index below keys a column that may hold a value. The three indexes that carry `to_id` are
-- partial on `to_kind <> 'value'`, and the index that serves an attribute read keys the node and
-- the attribute name alone. A read of a current value binds `to_kind = 'value'`, so the planner
-- takes `graph_tuples_values` for it.
--
-- Neither table is partitioned. `create index concurrently` is reachable on a table past
-- `pgmigrate.CensusTableLimit` through a `-- pgmigrate:non-transactional` migration, which the
-- census exempts from the size limit by name (`pgmigrate/census.go`, `ConcurrentIndex`);
-- `store/README.md` holds that route beside the shape it uses. A range-partitioned log would
-- prune nothing, because a current read out of a node constrains no `seq`, and PostgreSQL
-- refuses a default partition on a hash-partitioned table.

create table graph_nodes (
  kind text not null,
  id text not null,
  supertag text not null,
  visibility text not null,
  state text not null,
  attributes jsonb not null default '{}',
  made_at timestamptz not null,
  made_by jsonb not null,
  source_event bigint not null,
  primary key (kind, id)
);

create index graph_nodes_supertag on graph_nodes (supertag, kind, id);

create table graph_tuples (
  seq bigserial primary key,
  from_kind text not null,
  from_id text not null,
  kind text not null,
  to_kind text not null,
  to_id text not null,
  at timestamptz not null,
  who jsonb not null,
  source_event bigint not null,
  legacy_source_seq bigint,
  asserted boolean not null default true
);

-- The repeat keys. One event states one fact once, so a writer that is retried appends the same
-- row again and `on conflict do nothing` drops it. Both are partial on `source_event <> 0`: at
-- the sentinel there is no event to state a fact twice, and a document re-indexed again and
-- again would otherwise lose the second retraction of one `part_of`.
create unique index graph_tuples_once on graph_tuples (from_kind, from_id, kind, to_kind, to_id, source_event, asserted)
  where to_kind <> 'value' and source_event <> 0;
create unique index graph_tuples_value_once on graph_tuples (from_kind, from_id, kind, source_event, asserted)
  where to_kind = 'value' and source_event <> 0;

-- The read indexes.
create index graph_tuples_current on graph_tuples (from_kind, from_id, kind, to_kind, to_id, seq desc)
  where to_kind <> 'value';
create index graph_tuples_values on graph_tuples (from_kind, from_id, kind, seq desc)
  where to_kind = 'value';
create index graph_tuples_inbound on graph_tuples (to_kind, to_id, kind, from_kind, from_id, seq desc)
  where to_kind <> 'value';
create index graph_tuples_as_of_out on graph_tuples (from_kind, from_id, kind, at desc, seq desc);
create index graph_tuples_as_of_in on graph_tuples (to_kind, to_id, kind, at desc, seq desc)
  where to_kind <> 'value';

-- Which kinds the store has taken over. `graph_edges` keeps its arm over each source column
-- while that kind's row reads false, and the tuple arm answers that kind once it reads true, so
-- the view answers each row exactly once at every instant. One row per kind from this migration,
-- so no arm's test reads null.
create table graph_store_cutover (
  kind text primary key,
  complete boolean not null default false
);
insert into graph_store_cutover (kind) values ('comment'), ('block'), ('block_ref');

-- What `envoy-dispatch erase` removed. An operator's audit row: nobody addresses it, and no read
-- of the graph answers it. It holds no erased text.
create table graph_erasures (
  id bigserial primary key,
  node_kind text not null,
  node_id text not null,
  removed jsonb not null,
  ran_by jsonb not null,
  ran_at timestamptz not null
);

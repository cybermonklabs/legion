-- 20261112024320_priority_nodes.up.sql — the cross-project priority, as a node and its tuples.
--
-- INFRA-2947: a priority is a thing the whole deployment holds rather than a row of one project's
-- board, and work in any project states that it advances one. The two tables are the node and
-- tuple store's own shape (legion/INFRA-1149,
-- store/migrations/20261109013400_graph_store.up.sql), less `legacy_source_seq`, which exists
-- there only for a projection's own backfill: so the day that store lands, these rows copy into
-- `graph_nodes` and `graph_tuples` column for column. They carry names of their own rather than
-- the store's, because tables named `graph_nodes` and `graph_tuples` here would collide with that
-- migration on the day it lands.
--
-- No column names a project. That is what makes a priority cross-project: an issue of any board
-- states `advances` on one, and neither table holds a row that could be narrowed to one project.
--
-- Nothing updates a tuple and nothing deletes one: a relation that ends is a second tuple with
-- `asserted` false, and an attribute that changes is a second tuple carrying the new value. The
-- node row is the projection of its own tuples — `attributes` holds the current title,
-- description, owner and order, and the tuples hold how each of them got there — and one
-- transaction moves both, so a read answers the row and a history answers the log.
--
-- `source_event` holds an `events.id` and is `not null`. It holds 0 for a write no event
-- recorded, which is every write here: `events` takes one owner, an issue or a project
-- (`0001_init.up.sql`), and a priority is neither, so a priority write appends no event row.
-- `events.id` is a `bigserial`, so no event ever carries 0, and the value reads as "no event
-- recorded this write" (the store's own `NoEvent`).
--
-- `to_kind = 'value'` marks an attribute: `to_id` then holds the value itself rather than an
-- address, and a value has no length bound (a priority's description is one). A btree key is
-- bounded at about 2,704 bytes on an 8 KiB page and an insert over that bound fails the
-- statement, so no index below keys a column that may hold a value. The two indexes that carry
-- `to_id` are partial on `to_kind <> 'value'`, and the index that serves an attribute read keys
-- the node and the attribute name alone; a read of a current value binds `to_kind = 'value'`, so
-- the planner takes `priority_tuples_values` for it.
--
-- Two new tables no earlier release names: nothing existing is read or rewritten, no foreign key
-- reaches another table, and the release before this one neither reads nor writes them, so this
-- rolls (cmd/dispatch/README.md, "Database migrations") and the census answers 0. The identifier
-- is the UTC moment this was written, as `migrationid` requires of every migration past the
-- numbered series, which closed at 0161.

create table priority_nodes (
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

create table priority_tuples (
  seq bigserial primary key,
  from_kind text not null,
  from_id text not null,
  kind text not null,
  to_kind text not null,
  to_id text not null,
  at timestamptz not null,
  who jsonb not null,
  source_event bigint not null,
  asserted boolean not null default true
);

-- The read indexes, the store's own three. `priority_tuples_current` is the current relations out
-- of a node, whose column order is the one that read's `distinct on` walks. `priority_tuples_values`
-- is the current value of an attribute. `priority_tuples_inbound` is `priority_tuples_current` with
-- the ends swapped, and is what answers "every issue that advances this priority".
create index priority_tuples_current on priority_tuples (from_kind, from_id, kind, to_kind, to_id, seq desc)
  where to_kind <> 'value';
create index priority_tuples_values on priority_tuples (from_kind, from_id, kind, seq desc)
  where to_kind = 'value';
create index priority_tuples_inbound on priority_tuples (to_kind, to_id, kind, from_kind, from_id, seq desc)
  where to_kind <> 'value';

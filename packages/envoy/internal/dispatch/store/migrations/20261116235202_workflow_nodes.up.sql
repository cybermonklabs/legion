-- 20261116235202_workflow_nodes.up.sql — the record of each step of an issue, as a node and its tuples.
--
-- Each run of each step of an issue's workflow is one node of kind `step`: who ran it, with which
-- model, when it started and ended, and its result. The Legion daemon writes it at the start of the
-- run and at its end, under one id, and a person reads an issue's steps through the `part_of`
-- relation each node states to its issue.
--
-- The two tables are the priority's shape (20261112024320_priority_nodes.up.sql), which is the shape
-- of the graph's node and tuple store, less `legacy_source_seq`: so the day that store lands, these
-- rows copy into `graph_nodes` and `graph_tuples` column for column. They carry names of their
-- own, because tables named `graph_nodes` and `graph_tuples` here would collide with that store's
-- own migration on the day it lands. The columns, the primary keys and the three read indexes are
-- the priority's exactly, which the store's test reads out of the catalogue
-- (workflowstore TestTheTablesAreThePriorityShape).
--
-- Nothing updates a tuple and nothing deletes one. The node row is the projection of its own
-- tuples: `attributes` holds each field of the step, written once, and the tuples hold who wrote
-- each and when. One transaction moves both (internal/dispatch/workflowstore, the one writer).
--
-- `source_event` is 0 on every row, `NoEvent`: a step write appends no event, since an event would
-- feed the daemon's intake with facts it caused. `events.id` is a `bigserial`, so no event carries 0.
--
-- `to_kind = 'value'` marks an attribute: `to_id` then holds the value itself. The two indexes that
-- carry `to_id` are partial on `to_kind <> 'value'`, so no index keys a value, whose length nothing
-- bounds (a btree key is bounded at about 2,704 bytes on an 8 KiB page).
--
-- Two new tables no earlier release names: nothing existing is read or rewritten, no foreign key
-- reaches another table, and the release before this one neither reads nor writes them, so this
-- rolls (cmd/dispatch/README.md, "Database migrations") and the census answers 0.

create table workflow_nodes (
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

create table workflow_tuples (
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

-- The store's three read indexes. `workflow_tuples_current` is the current relations out of a node,
-- `workflow_tuples_values` the current value of an attribute, and `workflow_tuples_inbound` the
-- relations into a node, which answers "every step that is part of this issue".
create index workflow_tuples_current on workflow_tuples (from_kind, from_id, kind, to_kind, to_id, seq desc)
  where to_kind <> 'value';
create index workflow_tuples_values on workflow_tuples (from_kind, from_id, kind, seq desc)
  where to_kind = 'value';
create index workflow_tuples_inbound on workflow_tuples (to_kind, to_id, kind, from_kind, from_id, seq desc)
  where to_kind <> 'value';

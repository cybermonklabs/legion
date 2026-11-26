-- 20261126000300_stores_moved.up.sql
--
-- The first part of the move of the priority store and the workflow store into the graph store
-- (dispatch://INFRA-1098's R32, "How the two stores move"). The boot of this build moves the rows,
-- after its migrations and before it listens; this migration writes no row of either store. It
-- writes two things.
--
-- One. The move's record, `stores_moved_seqs`: one row for each tuple the move has copied into
-- graph_tuples, named by the store it came from (`priority` or `workflow`) and the seq it held
-- there. A batch of the move writes its tuples and their rows here in one transaction, so a move
-- that stops after any batch leaves no tuple a later move copies again. The table is created
-- empty, and the move drops it once the store holds every row. It is created `if not exists`
-- only because cmd/dispatch's TestEveryBlockTypeIsASupertagNodeAndValidationReadsIt runs each
-- type migration a second time over the store the first run left (runTypeMigrationsAgain); the
-- runner applies a version once.
--
-- Two. The type nodes of the three kinds the move brings into the store: the supertag nodes
-- `priority`, `step` and `plan`, and an attribute node for each attribute the kind's writer
-- states that is no base attribute (graphstore.IsBaseAttribute: visibility, state, project and
-- parties), each tied to its supertag by an `attribute_of` tuple. None of the three declares a
-- block type: no document holds a block of one, so the block schema the server installs reads
-- past them and its version does not move. Each attribute node carries what a type migration gives
-- an attribute of a type that declares no block type, as 20261116123354 gives a lens's: its kind,
-- its name, its store name, which is its own name since none is a base attribute, and its
-- position among its type's attributes. The kind is that of the value the node row holds: `string`
-- and `timestamp` as the lens catalogue's, `integer` for a whole number the row holds as a JSON
-- number, and `json` for a plan's tasks, a JSON list the row holds as itself and its value tuple
-- as that JSON's text.
--
--   - A priority (internal/dispatch/prioritystore, its four attributes a person writes): title,
--     description, owner and rank, each text.
--   - A run of a step (internal/dispatch/workflowstore, workflowstore.go and write.go): the
--     seventeen attributes beside its project, in the order the package declares them - issue,
--     generation, task, step (its place), kind, role, type, agent, session, model, started_at,
--     ended_at, result, returned_to, report, and a retro's rating and rating_reason.
--   - A plan (workflowstore, plans.go): the six beside its project - issue, generation, source,
--     artifact_id, version and tasks.
--
-- Every statement is one of the two shapes the pre-deploy census admits of a table past
-- pgmigrate.CensusTableLimit, which graph_nodes and graph_tuples are once the node store's
-- backfill has filled them: an `insert … values` of at most 1,000 literal rows, with no `on
-- conflict` or with `on conflict do nothing` (boundedWrites in pgmigrate/exempt.go). So no
-- statement reads a row to write one: each node and each tuple is written out below as the row it
-- is, and a tuple states the value the node row above it carries, as a string itself and any other
-- value as its JSON text. Every row carries the migration's own stamp, 2026-11-23 13:43:18 UTC, and
-- a writer of its own, so the file writes the same rows on every store and a reader can tell them
-- from every other type migration's. The migration records no event, so every tuple carries the
-- `source_event` sentinel 0.
--
-- The insert into graph_nodes takes `on conflict do nothing`, so a database that already holds one
-- of the three supertags, or one of their attribute nodes, takes the migration and keeps the row it
-- holds. The runner applies each version once and records it, so nothing else here guards a second
-- run: the tuples carry the sentinel, which no repeat key of graph_tuples drops. The triggers of
-- 20261109051232 announce the supertag and attribute rows once, at commit, and the server installs
-- the block schema those nodes build, which is the one it installed before.

create table if not exists stores_moved_seqs (
  store text not null check (store in ('priority', 'workflow')),
  seq bigint not null,
  primary key (store, seq)
);

-- The nodes: the three supertags, then the attribute nodes of each, in their positions. Every node
-- is project-visible and live, in no project and between no parties.
insert into graph_nodes (kind, id, supertag, visibility, state, attributes, made_at, made_by, source_event)
values
  ('supertag', 'priority', 'supertag', 'project', 'live', '{}', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0),
  ('supertag', 'step', 'supertag', 'project', 'live', '{}', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0),
  ('supertag', 'plan', 'supertag', 'project', 'live', '{}', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0),
  ('attribute', 'priority/title', 'attribute', 'project', 'live', '{"kind": "string", "name": "title", "store_name": "title", "position": 0}', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0),
  ('attribute', 'priority/description', 'attribute', 'project', 'live', '{"kind": "string", "name": "description", "store_name": "description", "position": 1}', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0),
  ('attribute', 'priority/owner', 'attribute', 'project', 'live', '{"kind": "string", "name": "owner", "store_name": "owner", "position": 2}', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0),
  ('attribute', 'priority/rank', 'attribute', 'project', 'live', '{"kind": "string", "name": "rank", "store_name": "rank", "position": 3}', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0),
  ('attribute', 'step/issue', 'attribute', 'project', 'live', '{"kind": "string", "name": "issue", "store_name": "issue", "position": 0}', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0),
  ('attribute', 'step/generation', 'attribute', 'project', 'live', '{"kind": "integer", "name": "generation", "store_name": "generation", "position": 1}', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0),
  ('attribute', 'step/task', 'attribute', 'project', 'live', '{"kind": "string", "name": "task", "store_name": "task", "position": 2}', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0),
  ('attribute', 'step/step', 'attribute', 'project', 'live', '{"kind": "integer", "name": "step", "store_name": "step", "position": 3}', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0),
  ('attribute', 'step/kind', 'attribute', 'project', 'live', '{"kind": "string", "name": "kind", "store_name": "kind", "position": 4}', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0),
  ('attribute', 'step/role', 'attribute', 'project', 'live', '{"kind": "string", "name": "role", "store_name": "role", "position": 5}', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0),
  ('attribute', 'step/type', 'attribute', 'project', 'live', '{"kind": "string", "name": "type", "store_name": "type", "position": 6}', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0),
  ('attribute', 'step/agent', 'attribute', 'project', 'live', '{"kind": "string", "name": "agent", "store_name": "agent", "position": 7}', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0),
  ('attribute', 'step/session', 'attribute', 'project', 'live', '{"kind": "string", "name": "session", "store_name": "session", "position": 8}', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0),
  ('attribute', 'step/model', 'attribute', 'project', 'live', '{"kind": "string", "name": "model", "store_name": "model", "position": 9}', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0),
  ('attribute', 'step/started_at', 'attribute', 'project', 'live', '{"kind": "timestamp", "name": "started_at", "store_name": "started_at", "position": 10}', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0),
  ('attribute', 'step/ended_at', 'attribute', 'project', 'live', '{"kind": "timestamp", "name": "ended_at", "store_name": "ended_at", "position": 11}', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0),
  ('attribute', 'step/result', 'attribute', 'project', 'live', '{"kind": "string", "name": "result", "store_name": "result", "position": 12}', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0),
  ('attribute', 'step/returned_to', 'attribute', 'project', 'live', '{"kind": "integer", "name": "returned_to", "store_name": "returned_to", "position": 13}', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0),
  ('attribute', 'step/report', 'attribute', 'project', 'live', '{"kind": "string", "name": "report", "store_name": "report", "position": 14}', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0),
  ('attribute', 'step/rating', 'attribute', 'project', 'live', '{"kind": "integer", "name": "rating", "store_name": "rating", "position": 15}', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0),
  ('attribute', 'step/rating_reason', 'attribute', 'project', 'live', '{"kind": "string", "name": "rating_reason", "store_name": "rating_reason", "position": 16}', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0),
  ('attribute', 'plan/issue', 'attribute', 'project', 'live', '{"kind": "string", "name": "issue", "store_name": "issue", "position": 0}', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0),
  ('attribute', 'plan/generation', 'attribute', 'project', 'live', '{"kind": "integer", "name": "generation", "store_name": "generation", "position": 1}', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0),
  ('attribute', 'plan/source', 'attribute', 'project', 'live', '{"kind": "string", "name": "source", "store_name": "source", "position": 2}', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0),
  ('attribute', 'plan/artifact_id', 'attribute', 'project', 'live', '{"kind": "string", "name": "artifact_id", "store_name": "artifact_id", "position": 3}', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0),
  ('attribute', 'plan/version', 'attribute', 'project', 'live', '{"kind": "integer", "name": "version", "store_name": "version", "position": 4}', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0),
  ('attribute', 'plan/tasks', 'attribute', 'project', 'live', '{"kind": "json", "name": "tasks", "store_name": "tasks", "position": 5}', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0)
on conflict do nothing;

-- The two base attributes each node carries, as the tuples they are, at the time and by the
-- writer of the node's row.
insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, legacy_source_seq, asserted)
values
  ('supertag', 'plan', 'state', 'value', 'live', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('supertag', 'plan', 'visibility', 'value', 'project', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('supertag', 'priority', 'state', 'value', 'live', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('supertag', 'priority', 'visibility', 'value', 'project', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('supertag', 'step', 'state', 'value', 'live', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('supertag', 'step', 'visibility', 'value', 'project', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'plan/artifact_id', 'state', 'value', 'live', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'plan/artifact_id', 'visibility', 'value', 'project', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'plan/generation', 'state', 'value', 'live', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'plan/generation', 'visibility', 'value', 'project', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'plan/issue', 'state', 'value', 'live', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'plan/issue', 'visibility', 'value', 'project', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'plan/source', 'state', 'value', 'live', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'plan/source', 'visibility', 'value', 'project', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'plan/tasks', 'state', 'value', 'live', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'plan/tasks', 'visibility', 'value', 'project', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'plan/version', 'state', 'value', 'live', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'plan/version', 'visibility', 'value', 'project', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'priority/description', 'state', 'value', 'live', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'priority/description', 'visibility', 'value', 'project', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'priority/owner', 'state', 'value', 'live', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'priority/owner', 'visibility', 'value', 'project', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'priority/rank', 'state', 'value', 'live', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'priority/rank', 'visibility', 'value', 'project', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'priority/title', 'state', 'value', 'live', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'priority/title', 'visibility', 'value', 'project', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/agent', 'state', 'value', 'live', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/agent', 'visibility', 'value', 'project', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/ended_at', 'state', 'value', 'live', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/ended_at', 'visibility', 'value', 'project', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/generation', 'state', 'value', 'live', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/generation', 'visibility', 'value', 'project', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/issue', 'state', 'value', 'live', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/issue', 'visibility', 'value', 'project', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/kind', 'state', 'value', 'live', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/kind', 'visibility', 'value', 'project', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/model', 'state', 'value', 'live', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/model', 'visibility', 'value', 'project', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/rating', 'state', 'value', 'live', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/rating', 'visibility', 'value', 'project', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/rating_reason', 'state', 'value', 'live', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/rating_reason', 'visibility', 'value', 'project', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/report', 'state', 'value', 'live', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/report', 'visibility', 'value', 'project', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/result', 'state', 'value', 'live', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/result', 'visibility', 'value', 'project', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/returned_to', 'state', 'value', 'live', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/returned_to', 'visibility', 'value', 'project', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/role', 'state', 'value', 'live', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/role', 'visibility', 'value', 'project', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/session', 'state', 'value', 'live', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/session', 'visibility', 'value', 'project', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/started_at', 'state', 'value', 'live', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/started_at', 'visibility', 'value', 'project', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/step', 'state', 'value', 'live', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/step', 'visibility', 'value', 'project', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/task', 'state', 'value', 'live', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/task', 'visibility', 'value', 'project', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/type', 'state', 'value', 'live', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/type', 'visibility', 'value', 'project', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true);

-- The values each attribute node carries, as the tuples they are: its kind, its name and store
-- name as themselves, and its position as that number's text.
insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, legacy_source_seq, asserted)
values
  ('attribute', 'plan/artifact_id', 'kind', 'value', 'string', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'plan/artifact_id', 'name', 'value', 'artifact_id', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'plan/artifact_id', 'position', 'value', '3', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'plan/artifact_id', 'store_name', 'value', 'artifact_id', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'plan/generation', 'kind', 'value', 'integer', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'plan/generation', 'name', 'value', 'generation', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'plan/generation', 'position', 'value', '1', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'plan/generation', 'store_name', 'value', 'generation', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'plan/issue', 'kind', 'value', 'string', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'plan/issue', 'name', 'value', 'issue', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'plan/issue', 'position', 'value', '0', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'plan/issue', 'store_name', 'value', 'issue', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'plan/source', 'kind', 'value', 'string', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'plan/source', 'name', 'value', 'source', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'plan/source', 'position', 'value', '2', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'plan/source', 'store_name', 'value', 'source', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'plan/tasks', 'kind', 'value', 'json', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'plan/tasks', 'name', 'value', 'tasks', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'plan/tasks', 'position', 'value', '5', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'plan/tasks', 'store_name', 'value', 'tasks', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'plan/version', 'kind', 'value', 'integer', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'plan/version', 'name', 'value', 'version', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'plan/version', 'position', 'value', '4', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'plan/version', 'store_name', 'value', 'version', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'priority/description', 'kind', 'value', 'string', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'priority/description', 'name', 'value', 'description', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'priority/description', 'position', 'value', '1', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'priority/description', 'store_name', 'value', 'description', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'priority/owner', 'kind', 'value', 'string', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'priority/owner', 'name', 'value', 'owner', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'priority/owner', 'position', 'value', '2', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'priority/owner', 'store_name', 'value', 'owner', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'priority/rank', 'kind', 'value', 'string', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'priority/rank', 'name', 'value', 'rank', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'priority/rank', 'position', 'value', '3', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'priority/rank', 'store_name', 'value', 'rank', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'priority/title', 'kind', 'value', 'string', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'priority/title', 'name', 'value', 'title', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'priority/title', 'position', 'value', '0', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'priority/title', 'store_name', 'value', 'title', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/agent', 'kind', 'value', 'string', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/agent', 'name', 'value', 'agent', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/agent', 'position', 'value', '7', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/agent', 'store_name', 'value', 'agent', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/ended_at', 'kind', 'value', 'timestamp', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/ended_at', 'name', 'value', 'ended_at', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/ended_at', 'position', 'value', '11', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/ended_at', 'store_name', 'value', 'ended_at', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/generation', 'kind', 'value', 'integer', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/generation', 'name', 'value', 'generation', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/generation', 'position', 'value', '1', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/generation', 'store_name', 'value', 'generation', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/issue', 'kind', 'value', 'string', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/issue', 'name', 'value', 'issue', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/issue', 'position', 'value', '0', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/issue', 'store_name', 'value', 'issue', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/kind', 'kind', 'value', 'string', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/kind', 'name', 'value', 'kind', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/kind', 'position', 'value', '4', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/kind', 'store_name', 'value', 'kind', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/model', 'kind', 'value', 'string', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/model', 'name', 'value', 'model', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/model', 'position', 'value', '9', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/model', 'store_name', 'value', 'model', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/rating', 'kind', 'value', 'integer', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/rating', 'name', 'value', 'rating', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/rating', 'position', 'value', '15', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/rating', 'store_name', 'value', 'rating', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/rating_reason', 'kind', 'value', 'string', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/rating_reason', 'name', 'value', 'rating_reason', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/rating_reason', 'position', 'value', '16', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/rating_reason', 'store_name', 'value', 'rating_reason', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/report', 'kind', 'value', 'string', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/report', 'name', 'value', 'report', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/report', 'position', 'value', '14', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/report', 'store_name', 'value', 'report', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/result', 'kind', 'value', 'string', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/result', 'name', 'value', 'result', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/result', 'position', 'value', '12', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/result', 'store_name', 'value', 'result', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/returned_to', 'kind', 'value', 'integer', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/returned_to', 'name', 'value', 'returned_to', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/returned_to', 'position', 'value', '13', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/returned_to', 'store_name', 'value', 'returned_to', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/role', 'kind', 'value', 'string', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/role', 'name', 'value', 'role', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/role', 'position', 'value', '5', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/role', 'store_name', 'value', 'role', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/session', 'kind', 'value', 'string', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/session', 'name', 'value', 'session', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/session', 'position', 'value', '8', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/session', 'store_name', 'value', 'session', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/started_at', 'kind', 'value', 'timestamp', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/started_at', 'name', 'value', 'started_at', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/started_at', 'position', 'value', '10', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/started_at', 'store_name', 'value', 'started_at', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/step', 'kind', 'value', 'integer', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/step', 'name', 'value', 'step', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/step', 'position', 'value', '3', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/step', 'store_name', 'value', 'step', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/task', 'kind', 'value', 'string', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/task', 'name', 'value', 'task', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/task', 'position', 'value', '2', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/task', 'store_name', 'value', 'task', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/type', 'kind', 'value', 'string', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/type', 'name', 'value', 'type', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/type', 'position', 'value', '6', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/type', 'store_name', 'value', 'type', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true);

-- What ties each attribute to its type. A reader of the schema follows this tuple, never the
-- spelling of the node's id, so without it a node is no attribute of any type.
insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, legacy_source_seq, asserted)
values
  ('attribute', 'plan/artifact_id', 'attribute_of', 'supertag', 'plan', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'plan/generation', 'attribute_of', 'supertag', 'plan', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'plan/issue', 'attribute_of', 'supertag', 'plan', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'plan/source', 'attribute_of', 'supertag', 'plan', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'plan/tasks', 'attribute_of', 'supertag', 'plan', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'plan/version', 'attribute_of', 'supertag', 'plan', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'priority/description', 'attribute_of', 'supertag', 'priority', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'priority/owner', 'attribute_of', 'supertag', 'priority', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'priority/rank', 'attribute_of', 'supertag', 'priority', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'priority/title', 'attribute_of', 'supertag', 'priority', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/agent', 'attribute_of', 'supertag', 'step', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/ended_at', 'attribute_of', 'supertag', 'step', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/generation', 'attribute_of', 'supertag', 'step', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/issue', 'attribute_of', 'supertag', 'step', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/kind', 'attribute_of', 'supertag', 'step', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/model', 'attribute_of', 'supertag', 'step', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/rating', 'attribute_of', 'supertag', 'step', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/rating_reason', 'attribute_of', 'supertag', 'step', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/report', 'attribute_of', 'supertag', 'step', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/result', 'attribute_of', 'supertag', 'step', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/returned_to', 'attribute_of', 'supertag', 'step', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/role', 'attribute_of', 'supertag', 'step', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/session', 'attribute_of', 'supertag', 'step', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/started_at', 'attribute_of', 'supertag', 'step', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/step', 'attribute_of', 'supertag', 'step', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/task', 'attribute_of', 'supertag', 'step', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true),
  ('attribute', 'step/type', 'attribute_of', 'supertag', 'step', '2026-11-23 13:43:18+00', '{"kind": "system", "id": "stores-moved"}', 0, null, true);

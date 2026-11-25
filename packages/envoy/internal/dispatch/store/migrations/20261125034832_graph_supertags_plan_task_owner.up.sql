-- 20261125034832_graph_supertags_plan_task_owner.up.sql
--
-- Move the stored block schema from 11 to 12 with plan_task's fifth attribute, owner: a string,
-- the role of the task's sub-owner, read and created from version 12. The server installs the
-- schema built from these nodes, not the embedded file, so the attribute and the version must
-- reach the store together. Otherwise the page built for 12 is admitted read-only by schema 11.
--
-- These literal inserts and primary-key updates are bounded writes after the graph backfill, as
-- 20261123080000_graph_supertags_plan_task_steps and 20261125012814_graph_supertags_plan_task_merges
-- are. The runner applies the file once in one transaction; the type-node triggers announce the
-- whole change at its commit. A second manual application fails at the attribute node's primary
-- key and writes nothing.
--
-- Rollback is the older image alone. pmdoc.Install reads the newer store at that build's own
-- version and leaves out owner; a document that holds it answers DOC_NEWER_CONTENT until a build
-- that reads version 12 serves it. Returning to this image finds this migration recorded.

-- owner is the name of no base attribute, so its store name is its own name
-- (pmdoc/schemanodes/schemanodestest.StoreName). serves, needs, steps and merges hold positions 0
-- to 3, so owner holds 4.
insert into graph_nodes (kind, id, supertag, visibility, state, attributes, made_at, made_by, source_event)
values ('attribute', 'plan_task/owner', 'attribute', 'project', 'live',
        '{"kind": "string", "since": 12, "creatable_since": 12, "name": "owner", "store_name": "owner", "position": 4}'::jsonb,
        now(), '{"kind": "system", "id": "block-schema-plan-task-owner"}'::jsonb, 0);

-- Each attribute of the node is also a value tuple; attribute_of names the type it belongs to.
insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, legacy_source_seq, asserted)
values ('attribute', 'plan_task/owner', 'state', 'value', 'live', now(), '{"kind": "system", "id": "block-schema-plan-task-owner"}'::jsonb, 0, null, true),
       ('attribute', 'plan_task/owner', 'visibility', 'value', 'project', now(), '{"kind": "system", "id": "block-schema-plan-task-owner"}'::jsonb, 0, null, true),
       ('attribute', 'plan_task/owner', 'attribute_of', 'supertag', 'plan_task', now(), '{"kind": "system", "id": "block-schema-plan-task-owner"}'::jsonb, 0, null, true),
       ('attribute', 'plan_task/owner', 'creatable_since', 'value', '12', now(), '{"kind": "system", "id": "block-schema-plan-task-owner"}'::jsonb, 0, null, true),
       ('attribute', 'plan_task/owner', 'kind', 'value', 'string', now(), '{"kind": "system", "id": "block-schema-plan-task-owner"}'::jsonb, 0, null, true),
       ('attribute', 'plan_task/owner', 'name', 'value', 'owner', now(), '{"kind": "system", "id": "block-schema-plan-task-owner"}'::jsonb, 0, null, true),
       ('attribute', 'plan_task/owner', 'position', 'value', '4', now(), '{"kind": "system", "id": "block-schema-plan-task-owner"}'::jsonb, 0, null, true),
       ('attribute', 'plan_task/owner', 'since', 'value', '12', now(), '{"kind": "system", "id": "block-schema-plan-task-owner"}'::jsonb, 0, null, true),
       ('attribute', 'plan_task/owner', 'store_name', 'value', 'owner', now(), '{"kind": "system", "id": "block-schema-plan-task-owner"}'::jsonb, 0, null, true);

-- Every block type keeps the schema's version, preserving its other attributes from version 11.
update graph_nodes
set attributes = '{"block_type": true, "content": "block+", "render": "host", "version": 12, "position": 0}'::jsonb
where kind = 'supertag' and id = 'callout';

update graph_nodes
set attributes = '{"block_type": true, "content": "paragraph+ bullet_list?", "render": "host", "version": 12, "position": 1}'::jsonb
where kind = 'supertag' and id = 'ask';

update graph_nodes
set attributes = '{"block_type": true, "content": "paragraph+", "render": "host", "version": 12, "position": 2, "since": 3, "creatable_since": 3}'::jsonb
where kind = 'supertag' and id = 'requirement';

update graph_nodes
set attributes = '{"block_type": true, "content": "paragraph+", "render": "host", "version": 12, "position": 3, "since": 3, "creatable_since": 3}'::jsonb
where kind = 'supertag' and id = 'plan_task';

update graph_nodes
set attributes = '{"block_type": true, "content": "paragraph+", "render": "host", "version": 12, "position": 4, "since": 3, "creatable_since": 3}'::jsonb
where kind = 'supertag' and id = 'check';

-- Keep the previous version facts and append the value each type node now holds.
insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, legacy_source_seq, asserted)
values ('supertag', 'ask', 'version', 'value', '12', now(), '{"kind": "system", "id": "block-schema-plan-task-owner"}'::jsonb, 0, null, true),
       ('supertag', 'callout', 'version', 'value', '12', now(), '{"kind": "system", "id": "block-schema-plan-task-owner"}'::jsonb, 0, null, true),
       ('supertag', 'check', 'version', 'value', '12', now(), '{"kind": "system", "id": "block-schema-plan-task-owner"}'::jsonb, 0, null, true),
       ('supertag', 'plan_task', 'version', 'value', '12', now(), '{"kind": "system", "id": "block-schema-plan-task-owner"}'::jsonb, 0, null, true),
       ('supertag', 'requirement', 'version', 'value', '12', now(), '{"kind": "system", "id": "block-schema-plan-task-owner"}'::jsonb, 0, null, true);

-- 20261125012814_graph_supertags_plan_task_merges.up.sql
--
-- Move the stored block schema from 5 to 11 with plan_task's fourth attribute, merges: the
-- enum values "true" and "false", read and created from version 11. The server installs the
-- schema built from these nodes, not the embedded file, so the attribute and the version must
-- reach the store together. Otherwise the page built for 11 is admitted read-only by schema 5.
--
-- These literal inserts and primary-key updates are bounded writes after the graph backfill,
-- as 20261123080000_graph_supertags_plan_task_steps is. The runner applies the file once in one
-- transaction; the type-node triggers announce the whole change at its commit. A second manual
-- application fails at the attribute node's primary key and writes nothing.
--
-- Rollback is the older image alone. pmdoc.Install reads the newer store at that build's own
-- version and leaves out merges; a document that holds it answers DOC_NEWER_CONTENT until a
-- build that reads version 11 serves it. Returning to this image finds this migration recorded.

insert into graph_nodes (kind, id, supertag, visibility, state, attributes, made_at, made_by, source_event)
values ('attribute', 'plan_task/merges', 'attribute', 'project', 'live',
        '{"kind": "enum", "choices": ["true", "false"], "since": 11, "creatable_since": 11, "name": "merges", "store_name": "merges", "position": 3}'::jsonb,
        now(), '{"kind": "system", "id": "block-schema-plan-task-merges"}'::jsonb, 0);

-- Each attribute of the node is also a value tuple; attribute_of names the type it belongs to.
insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, legacy_source_seq, asserted)
values ('attribute', 'plan_task/merges', 'state', 'value', 'live', now(), '{"kind": "system", "id": "block-schema-plan-task-merges"}'::jsonb, 0, null, true),
       ('attribute', 'plan_task/merges', 'visibility', 'value', 'project', now(), '{"kind": "system", "id": "block-schema-plan-task-merges"}'::jsonb, 0, null, true),
       ('attribute', 'plan_task/merges', 'attribute_of', 'supertag', 'plan_task', now(), '{"kind": "system", "id": "block-schema-plan-task-merges"}'::jsonb, 0, null, true),
       ('attribute', 'plan_task/merges', 'choices', 'value', '["true", "false"]', now(), '{"kind": "system", "id": "block-schema-plan-task-merges"}'::jsonb, 0, null, true),
       ('attribute', 'plan_task/merges', 'creatable_since', 'value', '11', now(), '{"kind": "system", "id": "block-schema-plan-task-merges"}'::jsonb, 0, null, true),
       ('attribute', 'plan_task/merges', 'kind', 'value', 'enum', now(), '{"kind": "system", "id": "block-schema-plan-task-merges"}'::jsonb, 0, null, true),
       ('attribute', 'plan_task/merges', 'name', 'value', 'merges', now(), '{"kind": "system", "id": "block-schema-plan-task-merges"}'::jsonb, 0, null, true),
       ('attribute', 'plan_task/merges', 'position', 'value', '3', now(), '{"kind": "system", "id": "block-schema-plan-task-merges"}'::jsonb, 0, null, true),
       ('attribute', 'plan_task/merges', 'since', 'value', '11', now(), '{"kind": "system", "id": "block-schema-plan-task-merges"}'::jsonb, 0, null, true),
       ('attribute', 'plan_task/merges', 'store_name', 'value', 'merges', now(), '{"kind": "system", "id": "block-schema-plan-task-merges"}'::jsonb, 0, null, true);

-- Every block type keeps the schema's version, preserving its other attributes from version 5.
update graph_nodes
set attributes = '{"block_type": true, "content": "block+", "render": "host", "version": 11, "position": 0}'::jsonb
where kind = 'supertag' and id = 'callout';

update graph_nodes
set attributes = '{"block_type": true, "content": "paragraph+ bullet_list?", "render": "host", "version": 11, "position": 1}'::jsonb
where kind = 'supertag' and id = 'ask';

update graph_nodes
set attributes = '{"block_type": true, "content": "paragraph+", "render": "host", "version": 11, "position": 2, "since": 3, "creatable_since": 3}'::jsonb
where kind = 'supertag' and id = 'requirement';

update graph_nodes
set attributes = '{"block_type": true, "content": "paragraph+", "render": "host", "version": 11, "position": 3, "since": 3, "creatable_since": 3}'::jsonb
where kind = 'supertag' and id = 'plan_task';

update graph_nodes
set attributes = '{"block_type": true, "content": "paragraph+", "render": "host", "version": 11, "position": 4, "since": 3, "creatable_since": 3}'::jsonb
where kind = 'supertag' and id = 'check';

-- Keep the previous version facts and append the value each type node now holds.
insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, legacy_source_seq, asserted)
values ('supertag', 'ask', 'version', 'value', '11', now(), '{"kind": "system", "id": "block-schema-plan-task-merges"}'::jsonb, 0, null, true),
       ('supertag', 'callout', 'version', 'value', '11', now(), '{"kind": "system", "id": "block-schema-plan-task-merges"}'::jsonb, 0, null, true),
       ('supertag', 'check', 'version', 'value', '11', now(), '{"kind": "system", "id": "block-schema-plan-task-merges"}'::jsonb, 0, null, true),
       ('supertag', 'plan_task', 'version', 'value', '11', now(), '{"kind": "system", "id": "block-schema-plan-task-merges"}'::jsonb, 0, null, true),
       ('supertag', 'requirement', 'version', 'value', '11', now(), '{"kind": "system", "id": "block-schema-plan-task-merges"}'::jsonb, 0, null, true);

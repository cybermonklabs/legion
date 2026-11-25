-- This migration inserts plan_task/merges and its tuples, and sets the five block type
-- supertag nodes to their version-5 attributes with the version moved to 11. Only migrations
-- write type nodes (cmd/dispatch's requireNoRouteOrCommandWritesAType). The previous type
-- migration, 20261123080000_graph_supertags_plan_task_steps, left those exact attributes, and
-- no migration between it and this one changes a type node. No plan_task/merges node exists.
-- The primary-key updates change only the schema version, not stored document or event data;
-- the inserts read no table. The number of existing content rows refused or rewritten is 0.
select 0

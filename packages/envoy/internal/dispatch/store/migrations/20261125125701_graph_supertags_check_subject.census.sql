-- This migration inserts the attribute nodes check/subject and check/program and their tuples. It
-- writes no other row: the text main shipped also set the attributes of the five block type
-- supertag nodes, and the edit its replaces line declares took those writes out, so the version
-- those nodes carry is the one 20261125034832_graph_supertags_plan_task_owner wrote. No migration
-- below this one writes a node check/subject or check/program, and no route and no command writes a
-- type node (cmd/dispatch's requireNoRouteOrCommandWritesAType), so no row exists for the inserts
-- to meet. It reads no row of another table. The number of existing rows this migration would
-- refuse or rewrite is therefore 0.
select 0

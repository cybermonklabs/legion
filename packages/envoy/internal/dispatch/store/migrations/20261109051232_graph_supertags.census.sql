-- This migration replaces a trigger function, creates two row triggers on graph_nodes, and
-- writes the type nodes and their tuples. Nothing reads an existing row of another table, and
-- the only rows it rewrites are the supertag and attribute nodes it wrote itself, which no
-- release before this one writes: graph_nodes and graph_tuples arrive empty of them at
-- 20261109013400, and no code outside this file writes a node of either kind. The two triggers
-- fire on rows of this seed alone at apply time. The number of existing rows this migration
-- would refuse or rewrite is therefore 0.
select 0;

-- 20261126000100_graph_edges_retirements.up.sql
--
-- The persisted fact that the boot step has deleted a child's old arms of graph_edges
-- (dispatch://INFRA-1098's T7b; startup.AsksArmsRetirement, startup.IssueTreeArmsRetirement): one
-- row per child, named as store.GraphEdgesChildren names it, written by the step in the
-- transaction that restates graph_edges without that child's arms, and by nothing else. The step
-- reads it to know which children's arms the view still holds, and refs.BacklinkCounts reads it,
-- through the primary key, once in every count statement, consulting a child's markers exactly
-- while no row names it. A migration that states graph_edges again with a retired child's arms
-- must delete that child's row in the same migration, or the count would stop consulting markers
-- the view consults.
--
-- The table is created empty: no migration deletes an arm, so every database this runs on holds
-- every child's arms, and the step writes each row at the boot that deletes them.
create table graph_edges_retirements (
  child text primary key,
  retired_at timestamptz not null default now()
);

-- The migration drops one column of issue_head_nodes and adds one nullable column with no default
-- to it, and nothing else: it refuses no row and rewrites none. issue_head_nodes is
-- 20261127120803_issue_tree_head's, which ships in this same release, so it holds no row at
-- deploy, and the census names no table only a pending migration creates. The number of existing
-- rows this migration would refuse or rewrite is therefore 0.
select 0

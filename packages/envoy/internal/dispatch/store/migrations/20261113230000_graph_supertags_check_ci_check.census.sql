-- This migration writes one attribute node and its three tuples, and sets the version on the
-- block-type supertag nodes. Nothing reads an existing row of another table, and the only rows it
-- rewrites are type nodes 20261109051232 wrote, which ship in the same release as this one:
-- graph_nodes and graph_tuples are created by 20261109013400 of that release, so a deployment
-- takes this census before either table exists, and no code outside a migration writes a node of
-- either kind. The number of existing rows this migration would refuse or rewrite is therefore 0.
select 0;

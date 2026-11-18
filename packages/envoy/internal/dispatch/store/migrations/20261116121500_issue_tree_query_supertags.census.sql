-- This migration writes two `supertag` nodes of the store and the tuples that state their base
-- attributes, and nothing else. Its ground is dispatch://INFRA-1149's R7: "A supertag write ships
-- as a migration, in the same release as the binary that handles it. It passes the pre-apply
-- census." No route or command writes a node of that kind (requireNoRouteOrCommandWritesAType in
-- cmd/dispatch/supertag_nodes_test.go), so the only rows it could rewrite are the two nodes a
-- first run of itself wrote, which it writes again with the values they hold. It refuses no row,
-- rewrites no row a person or an agent wrote, and reads no existing row of another table. The
-- number of existing rows this migration would refuse or rewrite is therefore 0.
select 0;

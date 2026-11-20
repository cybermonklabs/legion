-- The migration creates one function, decisions_documents, and nothing else: it writes no row,
-- reads no row while it applies (a sql function's body is checked against the catalog, not run),
-- and changes no table, so it refuses no row and rewrites no value any reader had before it. The
-- number of existing rows this migration would refuse or rewrite is therefore 0.
select 0

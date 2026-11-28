-- 20261128165639_graph_edges_security_invoker.up.sql — graph_edges reads its base tables under its caller's row security (plan D4, INFRA-6168).
--
-- graph_edges is a view, and a view reads its base tables as the view's owner, past every policy,
-- unless it is a security_invoker view. With the option on, each arm reads its table under the row
-- security of whoever reads the view: dispatch_app reads only the edges whose rows it sees, and the
-- owner and every other role read every edge, as before.
--
-- ALTER VIEW … SET (security_invoker = on) rewrites the view's options in pg_class alone and reads
-- no row: it takes ACCESS EXCLUSIVE on the view for the moment it runs, bounded by the runner's
-- lock_timeout, and locks none of its base tables, so the census admits it as catalog-only. A
-- later CREATE OR REPLACE VIEW of graph_edges replaces the view's options with the ones it states,
-- so it must state security_invoker = on again. No row is read, rewritten or refused, and the
-- census answers 0. Rollback is an image rollback: the release before this one reads the view as
-- the owner, whose row security is off.

alter view public.graph_edges set (security_invoker = on);

-- pgmigrate:non-transactional
-- The store census counts the two tuple kinds no table projects, copied_from and moved_to, and
-- resolves the far end of each (graphcensus's storeOnlyRead). No index of graph_tuples
-- (20261109013400) leads with a tuple's kind, so without this one that read walks the whole
-- table, the largest of the store, inside the census's snapshot, to find two kinds that hold a
-- copy's block apiece and a moved block apiece. This is the index it walks: partial on those two
-- kinds, so it holds their tuples and no other, in the order the read takes the newest tuple of
-- each relation in, its seq last, so the planner sorts nothing. It is partial on to_kind too, as
-- every index of graph_tuples that keys to_id is: a value has no length bound and a btree key is
-- bounded, so a value in the key would refuse a long write. The far end of either kind is an ask
-- or a block and never a value, so the predicate leaves out no tuple the read wants, and the reads
-- state it so the planner can take the index.
--
-- It is the one statement of this file because pgmigrate admits exactly one concurrent index
-- build in a non-transactional migration (nontransactional.go's concurrentIndex). It is a
-- performance index: no constraint and no later migration depends on it. A failed concurrent build
-- leaves an INVALID index of this name on graph_tuples, which the runner drops with DROP INDEX
-- CONCURRENTLY and builds again on the next attempt.
create index concurrently if not exists graph_tuples_store_only
    on graph_tuples (kind, from_kind, from_id, to_kind, to_id, seq desc)
    where kind in ('copied_from', 'moved_to') and to_kind <> 'value';

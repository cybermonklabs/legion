-- pgmigrate:non-transactional
-- The keyword fill's first-body repair (textversions' repairCutFirstBodies) reads, each time a
-- process takes the fill's hold, the comment versions of the keyword table that keep their event's
-- cut text, and the store census counts the comment.created ones revision 1 proves
-- (textversions.ProvenCutFirstBodiesQuery). Both read search_text_versions by kind = 'comment'
-- and body_from = 'cut', which no index serves: through the primary key they read every comment
-- version, a read that grows with the table, about 2 GB at production's counts. This partial
-- index holds exactly the comment versions marked cut, keyed by the primary key's own order
-- after kind, so the repair walks them in (id, event_seq) order in bounded batches and a table
-- that holds none costs one read of an empty index.
--
-- Its predicate names only the table's own columns, as every index predicate must. The table
-- records no event type, so the index holds every comment version marked cut, a comment.created's
-- or a later edit's or answer's, and the two readers keep the comment.created ones by their
-- event (repairWindowQuery, ProvenCutFirstBodiesQuery). Each of those versions is one the fill
-- warned of when it kept the cut text, a comment longer than an event carries that no source held
-- whole, so the index stays small.
--
-- It is a performance index: no constraint and no later migration depends on it. The build is
-- concurrent, so it holds off no read or write of the table while it scans it, whatever the
-- table's size, and the census admits it at any size (pgmigrate's concurrentIndex). A failed
-- concurrent build leaves an INVALID index of this name, which the runner drops with DROP INDEX
-- CONCURRENTLY and builds again on the next attempt. The release before this one neither reads
-- nor writes by it; its inserts keep it, as every insert keeps every index of the table.
create index concurrently if not exists search_text_versions_cut_comments
    on search_text_versions (id, event_seq)
    where kind = 'comment' and body_from = 'cut';

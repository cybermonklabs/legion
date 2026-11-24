-- pgmigrate:non-transactional
-- The keyword fill's re-read (internal/dispatch/textversions, rereadBatch) reads the events that
-- change a post's text - comment.edited, comment.deleted, message.edited and message.deleted - in
-- id order, from the log's first event, after a fill of an earlier rules version wrote during a
-- roll. With no index on type it walks every event of the log: 7.4 s of an 8.0 s re-read at
-- 900,000 events, production's count. This index holds those events alone, in id order, so the
-- read takes them through it. Its predicate is the list the read's statement names
-- (textversions' rereadLogTypes), which TestTheReReadsLogReadUsesItsIndex holds to this file.
-- It is built `concurrently`, outside the runner's transaction, because events is the largest
-- table a Dispatch deployment holds and grows with every write Dispatch makes: a plain build holds
-- SHARE on events for the length of one read of the table, which blocks every write to it. A
-- concurrent build takes no lock that blocks a write, at the cost of two passes over the table and
-- of waiting for the transactions already open.
-- A failed concurrent build leaves an INVALID index of this name on events, which the runner drops
-- with DROP INDEX CONCURRENTLY and builds again on the next attempt.
create index concurrently if not exists events_post_text_changes on events (id)
  where type in ('comment.deleted', 'comment.edited', 'message.deleted', 'message.edited');

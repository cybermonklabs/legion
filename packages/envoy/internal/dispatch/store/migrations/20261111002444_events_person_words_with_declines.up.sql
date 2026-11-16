-- pgmigrate:non-transactional
-- GET /api/v1/agents/{session_id}/answers (sessionanswers.List) now reads a fifth type beside the
-- four that carry a person's words to a session: ask.resolved, of which it lists the declines, an
-- ask a person chose not to answer. events_person_words (0145) is partial on the other four, so a
-- read of five types cannot use it at all and would scan events. This index is that one widened:
-- the same key and the same actor test, over the five types the read names, so the planner reads
-- only the window's human events of those types through it and filters published_destinations on
-- those rows, exactly as it did through 0145.
-- It is built `concurrently`, outside the runner's transaction, because events is the largest
-- table a Dispatch deployment holds and grows with every write Dispatch makes: 0145's plain build
-- held SHARE on events for the length of one read of the table, which blocks every write to it,
-- and that wait now grows with the table. A concurrent build takes no lock that blocks a write,
-- at the cost of two passes over the table and of waiting for the transactions already open.
-- A failed concurrent build leaves an INVALID index of this name on events, which the runner drops
-- with DROP INDEX CONCURRENTLY and builds again on the next attempt.
create index concurrently if not exists events_person_words_with_declines on events (created_at desc, id desc)
  where actor->>'kind' = 'user'
    and type in ('ask.answered', 'comment.created', 'message.created', 'message.answered', 'ask.resolved');

-- pgmigrate:non-transactional
-- 20261114081435_message_deliveries_session_taken.up.sql
-- GET /api/v1/agents/{session_id}/answers (sessionanswers.List) reads one session's message
-- deliveries the listener took, created since an hour before its window: `sent`, handed to the
-- running session, and since 20261114081434 `queued`, stored for a session that is not running
-- and handed over when it runs again. Both are words a person wrote that reach the session, so the
-- read's predicate is `state in ('sent', 'queued')`. message_deliveries_session_created (0147),
-- which backed that read, holds `state = 'sent'` rows alone, so it cannot answer the wider read;
-- this index is that one widened, under a new name, and 20261114081436 drops the old one after it.
--
-- The key and the window are 0147's, (session_id, created_at desc), so the read takes only its
-- window's range of one session, and message_id, the one column the read returns, is carried. So
-- is `state`: Postgres drops an index predicate's clause from a scan only for a predicate it can
-- prove the query's own clause implies and that clause the predicate, and over two states it keeps
-- the read's own `state in ('sent', 'queued')` as a filter; a column the index did not carry would
-- turn the index-only scan into a heap read of every row it found. With `state` carried the read
-- is an index-only scan (measured on a scratch database while this change was written: cost 5.26),
-- which is also what makes a cached generic plan, knowing neither the session nor the window's
-- start, choose it over message_deliveries_session (0051, session_id alone). The release before
-- this one reads `state = 'sent'`, which implies this predicate, so it reads through this index
-- as it read through 0147's while the two releases serve together.
--
-- It is built `concurrently`, outside the runner's transaction: message_deliveries grows with
-- every message and broadcast copy Dispatch sends, and a plain build would hold SHARE on it, which
-- blocks every write, for the length of a read of the table. A concurrent build takes SHARE UPDATE
-- EXCLUSIVE, which no ordinary read or write waits on, at the cost of two passes over the table
-- and a wait for the transactions already open; the census exempts it from its table size limit on
-- that ground (pgmigrate's concurrentIndex). A failed build leaves an INVALID index of this name
-- on message_deliveries, which the runner drops with DROP INDEX CONCURRENTLY and builds again on
-- the next boot, recording the version only after a build that finished.
create index concurrently if not exists message_deliveries_session_taken
  on message_deliveries (session_id, created_at desc)
  include (message_id, state)
  where state in ('sent', 'queued');

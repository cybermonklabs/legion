-- 20261123212127_claim_start_failure_after_ready.up.sql -- a claim whose agent is up holds no
-- start failure (dispatch://INFRA-5463).
--
-- What this changes: claims.last_start_failure is cleared on every claim stored ready, working or
-- idle. Every other claim keeps the column, and nothing else of any row changes.
--
-- Why: an earlier release kept a claim's last start failure until another failed start replaced
-- it, through every later ready, and the column carries no time. A claim waiting to launch days
-- later showed that old refusal as the reason it was not running. This release drops the start
-- failure when the claim's agent says it is ready (supervise's ready), with the launch failures
-- it names, so a claim stored ready, working or idle holds none. A row the earlier release left in
-- one of those states has had an agent up since its last start failure, so the sentence it holds
-- is a refusal its running agent came after.
--
-- What stays: the start failure of a claim stored in any other state. Nothing in the row says
-- whether its agent came up after that start, so it stays until the claim's agent is next ready.
-- A daemon of the earlier release that shares the database can write a ready, working or idle row
-- with a start failure after this ran; this release's boot clears such a row (supervise's
-- DropStaleStartFailure).
--
-- There is no down file: migrations here are forward-only, and the column keeps its type and its
-- readers. An earlier release reads an empty column as no start failure recorded, as it reads a
-- claim that never failed a start, so a rollback needs nothing put back.
update claims
set last_start_failure = ''
where state in ('ready', 'working', 'idle')
  and last_start_failure <> '';

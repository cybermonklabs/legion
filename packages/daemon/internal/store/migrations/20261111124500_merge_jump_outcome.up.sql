-- 20261111124500_merge_jump_outcome.up.sql — merge_jump_records stops recording what became of a
-- jump as the absence of a refusal and records it outright (INFRA-1578).
--
-- 20261110235352 gave the table `refused boolean` and `refusal text`, written only where a jump's
-- own mutation failed. The row is inserted before GitHub is asked, and nothing wrote it again
-- when GitHub took the jump, so `refused = false` covered three states at once: Legion asked and
-- never learned, GitHub accepted, and a worker died between the mutation and the write. A reader
-- of such a row inferred the most hopeful of the three, every time. outcome says what happened
-- and answer carries the words that decided it, so no reader reads an outcome out of a field
-- nobody wrote (record.MergeJumpOutcome).
--
-- The four values the check constraint names:
--   asked     what the transaction that spends the mark writes, before the mutation is sent. A
--             lost lease or a stopped daemon leaves it standing, and it is true of both: Legion
--             asked and never learned.
--   accepted  the enqueue mutation came back with no error at all, which is GitHub taking the
--             request. It is no queue position: where the merge then stands is read on GitHub.
--   refused   an answer came back that decides it — GitHub's own GraphQL error, or the 4xx
--             GitHub decided the request itself with.
--   unknown   anything that came back and decides nothing — a 5xx of GitHub's own edge, a 2xx
--             carrying no data — and anything that did not come back at all — a dial that
--             failed, a timeout, a connection closed before any status arrived. answer tells
--             those apart in its own words, and no reader branches on the difference.
-- answer keeps GitHub's status and words, or the transport failure's own text, for refused and
-- unknown, and is empty for asked and accepted, which have nothing to quote.
--
-- Why a second migration and not an edit of 20261110235352, which this branch adds and no release
-- carries. That no release carries it is measured; that nobody pointed a hand-built binary of
-- this branch at a database that is not a test's cannot be ruled out from here. A second
-- migration is right whether or not somebody did, and costs one file; editing the first in place
-- is wrong if somebody did. So 20261110235352's own SQL stands exactly as it was written, and
-- what changes the table is here.
--
-- The mapping of a row such a database already holds:
--   refused = true  -> outcome 'refused', its words carried into answer as they stand
--   refused = false -> outcome 'asked'
-- Not 'accepted' and not 'unknown'. The old false meant no more than "no refusal recorded" and
-- covered asked, accepted and a worker that died before it wrote what happened, with nothing in
-- the row telling them apart, so 'asked' is the only value such a row supports: Legion asked and
-- never learned. Mapping it to 'accepted' would make, inside the migration written to end this
-- defect, the very inference the defect is.
--
-- There is no down file: migrations here are forward-only, as 20261110235352's own note says.
alter table merge_jump_records
  add column outcome text not null default 'asked',
  add column answer text not null default '';

update merge_jump_records set outcome = 'refused', answer = refusal where refused;

alter table merge_jump_records
  alter column outcome drop default,
  alter column answer drop default,
  add constraint merge_jump_records_outcome_check
    check (outcome in ('asked', 'accepted', 'refused', 'unknown')),
  drop column refused,
  drop column refusal;

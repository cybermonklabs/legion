-- 20261126040600_refs_decisions_forms_seed.up.sql
-- Owes an index with the reading of an issue's Decisions page (20261126040000) to every text
-- already stored: each comment, each ask, each issue message and each document with a markdown
-- version - the sources rebuild-refs reads (refs.RebuildAll), and the ones 20261117025905 seeded.
-- No build that stored them read that page as this release does, so the issue's edge of
-- `dispatch://CORE-45/decisions` was not written and the retired document's edge was written for
-- `/issues/CORE-45/decisions` in its place.
-- It enqueues every row rather than the ones whose text looks like such a link: no SQL predicate
-- finds them, since a link's address may be written percent-encoded (`/issues/CORE-45/%64ecisions`
-- reads as the same page), and an edge to the retired document is not left by every build that
-- stored the text, which is why the triggers before it enqueue on what a re-index writes rather
-- than on what a text says. The drain reads each text once and writes only what moved.
--
-- It writes refs_decisions_forms_seed_owed, the bootstrap queue, and no other table: the live
-- queue, refs_decisions_forms_owed, is the one the triggers of 20261126040100-20261126040500 write
-- for the release before, and the two share no key. The seed is one statement that inserts a
-- row for every stored text and holds them uncommitted until it commits, so a seed into the live
-- queue made every writer of a stored text wait for it, the writer of the release before when its
-- settle was relayed, and a writer that relayed two sources in the reverse of the seed's order
-- deadlocked with it (40P01), which the migration runner does not retry. Nothing but this
-- statement inserts into the bootstrap queue, so it waits for no writer and no writer waits for
-- it, and the deadlock is not there to have. A write that lands while it runs is owed by the
-- triggers, in the live queue, and a source in both is indexed once: a complete index clears its
-- row of each (refs.queueSettleSourceOwed).
-- It runs after the triggers, so no write is between a seed that has read it and a relay that has
-- not begun, and in a migration of its own: it holds no lock beyond ACCESS SHARE on comments, asks,
-- messages, artifacts and artifact_versions, which it reads, and ROW EXCLUSIVE on the table it
-- writes, so no create trigger above holds the lock of a table the writers of a text write for as
-- long as it runs.
-- The queue holds rows only from this statement, so a drain that finds it empty is finished, and
-- stops reading it (docs.formsDrain).
-- kept for one release: decisions-ref-repair
insert into refs_decisions_forms_seed_owed (kind, id)
select 'comment', id from comments
union all
select 'ask', id from asks
union all
select 'message', id from messages where issue_key is not null
union all
select 'artifact', a.id from artifacts a
where exists (select 1 from artifact_versions v where v.artifact_id = a.id and v.markdown is not null)
on conflict (kind, id) do nothing;

-- 20261116012459_claim_reach_and_prior_boot_tokens.up.sql — the daemon's record of whether Envoy
-- can reach a claim's session and the moment the claim was last relaunched on its same session
-- (R5), and the boot tokens of each claim's previous launches (R3).
--
-- claims.reach is supervise.Claim.Reach: the session it is about and when that session first said
-- ready, the mark of a claim whose session's last registration is more than 20 minutes old and
-- the moment the 20 minutes were counted from, whether the controller has been told of it, what
-- the relaunch guard and the relaunch did to it, and the last relaunch the record has taken in.
-- Three of those must outlive a daemon restart, which is the very case R5 exists for: a claim
-- reported before a boot is not reported again after it, a second relaunch within 6 hours of the
-- first is refused whatever happened between them, and the daemon relaunches its own controller at
-- most once in 6 hours. The rest rides beside them so that one notice is sent once across a restart
-- as well. jsonb on the claim's own row, written by its own narrow statement (store's
-- PutClaimReach, or in the transaction of the notice a write marks told) and by no write a machine
-- makes, as the step and activity columns are, because the daemon's minute sweep writes it between
-- a machine's writes. Every existing row takes '{}', the record of a claim nothing has been read of
-- yet.
--
-- claims.relaunched_at is supervise.Claim.LastRelaunch: when a relaunch of a claim Envoy could not
-- reach last ended its process to start it again on the same session. It is a column of the claim's
-- own row, which the claim's own write carries — its launch's write at the latest — in that write's
-- own statement, and not a field of claims.reach, which the daemon's reach keeper writes after the
-- launch: a daemon that ended between the two writes, or a reach write the store refused, would
-- otherwise lose the record a second relaunch within 6 hours is refused by. A write never moves it
-- back (store's putClaim takes the later of the two), so a writer that does not carry it leaves it
-- as it stands. Every existing row takes null: no claim has been relaunched this way.
--
-- claim_boot_tokens is the hash of the boot token of each of a claim's previous launches, with the
-- generation it was minted for. The trigger claims_archive_boot_token keeps there the hash a write
-- of a claim's row replaces, in the statement that replaces it, so a shim of an older generation
-- whose hello reaches a daemon that has restarted since is still known for what it is — a launch the
-- claim has left — and is told to stop, where the claim row alone knows only its current launch and
-- such a token read as one no launch minted. Every hash is kept, however many launches a claim has:
-- a shim isolated through any number of later launches is still told to stop. It is a trigger
-- rather than a part of the daemon's own statement so the hash is kept whatever writes the launch:
-- a daemon of an earlier release, which a rollback runs, writes its launches with statements of its
-- own and names no such table, and a roll forward still knows the shim of every launch that daemon
-- made. The claim's own deletion takes its hashes with it (the table's foreign key).
--
-- Rolling the image back needs no restore of the schema: the release before this one names none of
-- the two columns, the table or the trigger in any statement, so its upserts leave both columns as
-- they stand, the trigger keeps the hash each of its launches replaces, and a daemon rolled forward
-- again reads all of it back.
--
-- There is no down file: migrations here are forward-only.
alter table claims add column reach jsonb not null default '{}'::jsonb;

alter table claims add column relaunched_at timestamptz;

create table claim_boot_tokens (
	boot_token_hash bytea primary key,
	claim_token text not null references claims(token) on delete cascade,
	generation bigint not null
);

create index claim_boot_tokens_claim on claim_boot_tokens (claim_token, generation);

create function claims_archive_boot_token() returns trigger language plpgsql as $$
begin
	insert into claim_boot_tokens (boot_token_hash, claim_token, generation)
	values (old.boot_token_hash, old.token, old.generation)
	on conflict (boot_token_hash) do nothing;
	return null;
end $$;

create trigger claims_archive_boot_token after update of boot_token_hash on claims
	for each row
	when (old.boot_token_hash is distinct from new.boot_token_hash and length(old.boot_token_hash) > 0)
	execute function claims_archive_boot_token();

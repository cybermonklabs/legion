-- 20261119114140_provider_errors_told.up.sql — the notices the project's controller is owed of a
-- claim's runs of provider-error turns, and the mark that they were told.
--
-- After six turns of one claim in a row end in a provider error, the project's controller is told
-- once, with the issue, the role and the error. The daemon used to tell it at exactly the sixth
-- turn, and that notice queued nothing while no controller was registered, which is the default
-- (`controller: operator`), and nothing when its write failed: the run went on or ended, and
-- nothing told the controller later. A run that reached six is owed its notice whatever comes
-- after it, and so is every later run of the same claim that reaches six, so the store keeps one
-- owed notice for each such run until it is told:
--
--   claims.provider_error_run_id     the number of the run of provider-error turns the claim
--                                    stands in, or stood in last. It moves to a new number as each
--                                    run begins, and it is 0 before the claim's first run;
--   claims.provider_errors_told_run  the number of the newest run whose notice admission wrote, in
--                                    the transaction that wrote the notice (admit's
--                                    tellProviderErrors), and 0 before the first;
--   provider_errors_owed             one row for each run of the claim that reached six turns and
--                                    is not told yet: the run's number, and what its notice tells
--                                    the controller (the run, the model and the HTTP status). The
--                                    claim's write adds the row of the run it stands in, or
--                                    rewrites its words, at each turn of the run from the sixth on
--                                    (supervise's Claim.ProviderErrorsOwed, the claim store's
--                                    upsertClaim), and only while the run stands above the told
--                                    mark.
--
-- No turn, reset or restart of the claim removes an owed row, and no claim write moves the told
-- mark or names an earlier run: a claim write adds or rewrites the row of its own run alone. So a
-- second run that reaches six before the first run's notice is written owes a row of its own
-- beside the first's, and neither replaces the other. Admission writes the owed notices while a
-- controller is registered: at the claim's own offer, at the controller's registration, and at
-- the admission pass of every fact, tick and boot (admit's TellOwedProviderErrors). It writes one
-- notice for a claim, which names each of the claim's owed runs, and in the same transaction it
-- moves the claim's told mark to the newest of them and deletes their rows. Each reader and the
-- claim's write lock the claim's row before they touch its owed rows, so the notice of one run is
-- written once. The pass reads the owed rows of its project, which are almost always none. A
-- claim with no issue, the project controller's own, owes no row: the notice names a worker's
-- issue, and every reader of the owed rows reads claims with an issue alone, so such a row would be
-- read by nothing and deleted by nothing.
--
-- The machine moves the run's number as the run's first turn ends in a provider error (supervise's
-- providerErrored). An earlier release names none of these columns, so its claim write leaves each
-- of them as it stands, and a run that release begins keeps the number of the run before it: after
-- a rollback and a roll forward, that run would read as a run already told, and its notice would
-- never be written. claims_provider_error_run_begun moves the number for such a write, whichever
-- release makes it: a write that begins a run and leaves the number as the row had it. A write
-- begins a run when its run climbs from 0, or stands below the row's, which a write whose reset the
-- store did not take leaves. This release's writes move the number themselves, so the trigger does
-- nothing to them.
--
-- An earlier release writes no owed row either, so a run it takes to six, after a rollback, would
-- owe nothing: if its next turn after the roll forward were a clean one, no turn of the run would
-- be left to owe it, and it would never be told. claims_provider_error_run_owed adds the owed row
-- for such a write, whichever release makes it: a write of a claim with an issue, whatever its
-- state, whose run stands at six or more and is numbered above the told mark, adds that run's row
-- if the claim owes none for it. A retired or a failed claim owes it as a live one does, for the
-- reason the backfill below gives. Its words are the backfill's. This release's own write adds
-- or rewrites the same row with the model and the status in the same statement (the claim store's
-- upsertClaim), so its words are the ones kept; a run already told stands at or below the told
-- mark, so the trigger owes it nothing. It runs after the write, and claims_provider_error_run_begun
-- before it, so the owed row names the number that trigger gave the run.
--
-- Every claim that is in a run at this migration gets the number 1, so the run it stands in is
-- numbered above the told mark's 0. A claim with an issue whose run already stands at six or more
-- owes run 1's notice at once, whatever its state: the earlier release told such a run at exactly
-- its sixth turn, and only while a controller was registered, which this database does not record,
-- and the run's next turn can be a clean one, after which no turn of the run is left to owe it. A
-- retired or a failed claim owes it too, since either can run again: its architect's retry, the
-- next start of its role and an operator's retry each reach both (supervise's RequestRetry and
-- RequestOperatorRetry), and the retry of a claim that a run of refused turns failed starts a
-- fresh session (supervise's freshSession), whose write puts the run back to zero, so a run of six
-- that the upgrade did not owe would never be told. The notice names the run and the error, a
-- provider error, and says that the earlier release did not keep its model or its HTTP status,
-- which that release kept nowhere. So such a run can be told once more after the upgrade, but no
-- run goes untold.
--
-- Rolling the image back needs no statement. The earlier release names none of these columns and
-- this table, and reads none of them. The triggers change only provider_error_run_id and the owed
-- rows, which that release does not read, and they refuse no write. A rollback and a roll forward
-- cost one thing: a run that the earlier release took to six, and told at that sixth turn while a
-- controller was registered, is told once more after the roll forward. The earlier release keeps
-- no mark of the notices it wrote, so nothing here can read that it told one, and the cost is the
-- upgrade's own: a run can be told twice, and no run goes untold.
alter table claims
  add column provider_error_run_id bigint not null default 0,
  add column provider_errors_told_run bigint not null default 0;

create table provider_errors_owed (
  claim_token text not null references claims (token) on delete cascade,
  run bigint not null check (run > 0),
  reason text not null,
  primary key (claim_token, run)
);

update claims set provider_error_run_id = 1 where provider_error_run > 0;

insert into provider_errors_owed (claim_token, run, reason)
  select token, 1, provider_error_run || ' turns in a row ended in a provider error, whose model and HTTP status the earlier release did not keep; the daemon keeps the task and sends it again every 10m0s at most'
  from claims
  where provider_error_run >= 6 and issue <> '';

create function claims_provider_error_run_begun() returns trigger language plpgsql as $$
begin
  new.provider_error_run_id := old.provider_error_run_id + 1;
  return new;
end $$;

create trigger claims_provider_error_run_begun before update on claims
  for each row
  when (new.provider_error_run > 0
    and (old.provider_error_run = 0 or new.provider_error_run < old.provider_error_run)
    and new.provider_error_run_id = old.provider_error_run_id)
  execute function claims_provider_error_run_begun();

create function claims_provider_error_run_owed() returns trigger language plpgsql as $$
begin
  insert into provider_errors_owed (claim_token, run, reason)
    values (new.token, new.provider_error_run_id, new.provider_error_run || ' turns in a row ended in a provider error, whose model and HTTP status the earlier release did not keep; the daemon keeps the task and sends it again every 10m0s at most')
    on conflict (claim_token, run) do nothing;
  return null;
end $$;

create trigger claims_provider_error_run_owed after update on claims
  for each row
  when (new.provider_error_run >= 6
    and new.provider_error_run_id > new.provider_errors_told_run
    and new.issue <> '')
  execute function claims_provider_error_run_owed();

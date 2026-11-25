-- 20261125171320_launch_model_totals.up.sql -- what the daemon keeps to count the calls of each
-- step's agent by model: each launch of a claim, the reads a step used, and each launch's final
-- totals.
--
-- claim_launches holds one row for each launch of a claim. A launch is the generation a claim
-- write first records a process under (supervise's launchNow). That claim write adds the row in
-- the same statement (store's upsertClaim). newest is the newest read of the launch's totals that
-- a step's start or end used (record.Store.KeepRead). final is the totals the launch's shim sent
-- when its harness exited (record.Store.KeepFinal). Each holds a record.LaunchRead as JSON: the
-- shim's totals and the moment the daemon received them. So a restart of the daemon loses none of
-- them.
--
-- step_starts.start_totals is what the latest start of a place read of its agent
-- (record.StartTotals), null for a start that read nothing. The next start of the place clears it
-- (record.Store.RaiseStepStarts), so an end never counts from an earlier run's read.
--
-- Every row of the release before reads as it did. A start it wrote has no start totals, so the
-- end of that run names no counts. A launch it made gets its row at the claim's next write. An
-- outbox step_record row it queued names no set model and no answers.
--
-- The way back below runs with every daemon of the database stopped, before the earlier image is
-- pinned (docs/kubernetes.md, "Rolling back the release that counts each step's models"). The
-- release before decodes a step_record row strictly, so the update takes off the two keys it
-- refuses. The drops take the column and the table. The delete makes the roll forward apply this
-- migration again.
--
-- update outbox set payload = payload - 'set_model' - 'answers' where kind = 'step_record';
-- alter table step_starts drop column start_totals;
-- drop table claim_launches;
-- delete from schema_version where version = 20261125171320;
create table claim_launches (
  claim text not null,
  generation bigint not null check (generation > 0),
  started_at timestamptz not null default now(),
  newest jsonb,
  final jsonb,
  primary key (claim, generation)
);

alter table step_starts add column start_totals jsonb;

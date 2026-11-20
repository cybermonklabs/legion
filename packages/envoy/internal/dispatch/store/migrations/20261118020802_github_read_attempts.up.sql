-- 20261118020802_github_read_attempts.up.sql
--
-- INFRA-4324: one row per GitHub read Dispatch makes under a lease, holding that read's newest
-- attempt, so GET /healthz can name each read with whether its newest attempt succeeded, when it
-- ended and the first failure it met. Each read runs under a lease (duty.Exclusive), so one
-- process attempts it in each period, and every process must answer it: a record in the memory
-- of the process that ran the pass would leave every other process answering no attempt. The
-- loop that holds the lease writes its read's row when its pass ends
-- (internal/dispatch/githubreads, Finish; store.GitHubReadAttempts.Record), overwriting the row
-- before it, and each process keeps a copy of the rows that the probe reads (githubreads.Copy),
-- so the probe sends nothing to the database.
--
--   read        which read the row is, one of the four names and no other, which the check
--               below holds it to: 'webhook_redelivery', 'architecture_import',
--               'delivery_reconcile' and 'ci_reconcile' (githubreads.Reads).
--   at          when the attempt was recorded, by the database's clock, so two processes' clocks
--               never order one read's attempts.
--   succeeded   whether the attempt sent requests to GitHub and noted no failure.
--   cause       the text of the attempt's first failure, made storable and cut to at most 1,024
--               bytes (githubreads.Cause), which github_read_attempts_cause_fits holds it to. Null
--               exactly when succeeded is true, which github_read_attempts_cause_when_failed
--               holds the pair to.
--
-- A new table nothing before this release names: the release before this one neither reads nor
-- writes it, so the two roll side by side (cmd/dispatch/README.md, "Database migrations"), and
-- the census answers 0 because the table holds no row at this schema. Rolling back past it is an
-- image rollback alone. The identifier is the UTC moment this file was created, as every
-- migration past the numbered series is named.
create table github_read_attempts (
    read text primary key check (read in ('webhook_redelivery', 'architecture_import', 'delivery_reconcile', 'ci_reconcile')),
    at timestamptz not null,
    succeeded boolean not null,
    cause text,
    constraint github_read_attempts_cause_when_failed check ((cause is null) = succeeded),
    constraint github_read_attempts_cause_fits check (octet_length(cause) <= 1024)
);

-- Every write of a row wakes every envoy-dispatch process, each of which reads the record again
-- into its copy (githubreads.Copy, on cmd/dispatch's listener hub). The payload is the read's
-- name; the listener reads every row itself rather than trusting the notification's order.
--
-- A trigger rather than a statement beside the upsert, so every writer of the table reaches every
-- listening process. Postgres delivers the notification only when the writing transaction
-- commits, and folds repeats of one read within it. The table is new, so the trigger locks no
-- table any release already reads or writes.
create function github_read_attempts_notify() returns trigger language plpgsql as $$
begin
  perform pg_notify('dispatch_github_read_attempts', new.read);
  return null;
end $$;
create trigger github_read_attempts_notify after insert or update on github_read_attempts
  for each row execute function github_read_attempts_notify();

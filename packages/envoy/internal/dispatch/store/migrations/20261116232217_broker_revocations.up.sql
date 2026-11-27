-- 20261116232217_broker_revocations.up.sql — the revocations Dispatch owes the secrets broker.
--
-- When the Google account behind an address changes, everything the replaced account holds is to
-- end. Its machine logins and its sign-ins on a machine are the secrets broker's, which Dispatch can
-- end only by calling the broker after the sign-in's transaction commits. So that transaction is to
-- record the debt here, and a revocation loop is to pay it with one call naming the replaced
-- account, again and again until the broker confirms: a process that dies between the commit and
-- the broker call leaves the row for the next pass of any process. A later slice adds the sign-in's
-- record and the loop; nothing in this release writes or reads the table. This release uses its
-- function alone: machine_login_revocations (20261120150130) announces itself with it.
--
-- One row per replaced account of an address: `email`, `issuer` (the sign-in issuer the account is
-- of, in its canonical spelling) and `subject` (the replaced account's `sub`), with
-- `holder_subject`, the `sub` of the account that took the address from it, which the broker leaves
-- every credential of. Two replacements of one address before the first is paid are two rows, both
-- paid, oldest first; the same account replaced again is its row owed again: `generation` counts the
-- revocations recorded for it, so a pass that read an earlier one never settles one recorded since.
-- `recorded_at` is the replacing sign-in's commit, read by its last statement (clock_timestamp())
-- under the people row's lock, so the rows of one address are ordered as their sign-ins committed,
-- the order they are paid in, one at a time; `tried_at` is the last pass that claimed the row,
-- which an account replaced again keeps, so two processes do not call the broker for it at once, a
-- failed call waits before it is tried again, and no row of the address is paid while a call of
-- another may still be in flight; `done_at` is when the broker confirmed it, null while it is
-- owed.
--
-- A recorded revocation announces itself on dispatch_broker_revocations at commit, for the loop to
-- wake on in every process. The trigger fires on an insert and on a write of `generation` alone, so
-- the loop's own claims and settlements, which write neither, wake nothing.
--
-- The table is new, so the migration touches no existing row.
create table broker_revocations (
  email text not null check (email <> '' and email = lower(btrim(email))),
  issuer text not null check (issuer <> ''),
  subject text not null check (subject <> ''),
  holder_subject text not null check (holder_subject <> '' and holder_subject <> subject),
  generation bigint not null default 1,
  recorded_at timestamptz not null default now(),
  tried_at timestamptz,
  done_at timestamptz,
  primary key (email, issuer, subject)
);

create function broker_revocations_notify() returns trigger language plpgsql as $$
begin
  perform pg_notify('dispatch_broker_revocations', '');
  return null;
end $$;

create trigger broker_revocations_notify after insert or update of generation on broker_revocations
  for each row execute function broker_revocations_notify();

-- 20261120150130_machine_login_revocations.up.sql — the machine logins of restricted members that
-- Dispatch owes the secrets broker a revocation of.
--
-- When a project's owner takes a restricted member off its access list and no other project's list
-- names them any more, the member's agents must hold nothing of the deployment. Each trade of their
-- machine login names one project, which the broker checks against Dispatch every time, so no new
-- token of a project is minted past the broker's cache; the logins themselves, and the tokens
-- already traded from them, are the broker's, which Dispatch can end only by calling the broker
-- after the removal commits. So the removal records the debt here, in its own transaction, and the
-- server's revocation loop (api: revokeOwedMachineLogins) pays it after the commit by revoking the
-- machine logins the member approved for their own machines before `recorded_at`, again and again
-- until the broker confirms: a broker that does not answer, or a process that dies between the
-- commit and the call, leaves the row for the next pass of any process. The removal does not wait
-- on the broker, so an owner removes a person while the broker is down; a login the member approves
-- after an owner puts them back survives the pass. A removal that leaves the member on another
-- project's list records nothing: a machine login belongs to no project, and keeps serving those.
--
-- One row per address (`email`, as people.email holds it). A removal recorded again before or after
-- the row is paid owes it again: `generation` counts the removals recorded, so a pass that read an
-- earlier one never settles one recorded since. `recorded_at` is the removal's last statement
-- (clock_timestamp()); `tried_at` is the last pass that claimed the row, so two processes do not call
-- the broker for it at once and a failed call waits before it is tried again; `done_at` is when the
-- broker confirmed it, null while it is owed.
--
-- A recorded revocation announces itself on dispatch_broker_revocations at commit, the channel the
-- revocation loop already wakes on in every process, through the function broker_revocations
-- announces itself with (broker_revocations_notify, migration 20261116232217, its search_path set by
-- 20261119065002), which names no table. The trigger fires on an insert and on a write of
-- `generation` alone, so the loop's own claims and settlements, which write neither, wake nothing.
--
-- The table is new, so the migration touches no existing row.
create table machine_login_revocations (
  email text primary key check (email <> '' and email = lower(btrim(email))),
  generation bigint not null default 1,
  recorded_at timestamptz not null default clock_timestamp(),
  tried_at timestamptz,
  done_at timestamptz
);

create trigger machine_login_revocations_notify after insert or update of generation on machine_login_revocations
  for each row execute function broker_revocations_notify();

-- 20261119064905_earliest_head_broker_revocations_refused.up.sql — a database an earliest head of
-- cybermonklabs/legion#4199 migrated is refused here, where it used to pass and then fail at every
-- replacing sign-in.
--
-- 20261116232217 made broker_revocations. The heads cf83238fe9 through fe1d01bd2e wrote it with
-- email as its primary key and no issuer, subject or holder_subject (aa6bb03e14 added those), and
-- the migrator records a version with no checksum, so a database one of them migrated holds that
-- shape and counts 20261116232217 as run. 20261118152422 handles the later shapes alone: it adds
-- session_generation `if not exists` and creates a guard whose body names the missing columns,
-- which plpgsql does not resolve until the trigger runs. So Migrate answered no error on such a
-- database, and then every sign-in that replaced an account failed with 42703 (column "issuer"
-- does not exist) and recorded nothing — the replaced account kept its credentials, and nothing
-- said why.
--
-- A database of any head before 8db739b89a already fails loudly, at 20261118041916's
-- `drop function refuse_agent_token_account_change()`, so this window is the one that passed. The
-- rows of such a table name no account and no holder, so no revocation in it can be paid; the
-- recovery is the reset the runbook gives for a Dispatch database and the broker's together, not a
-- rewrite of the table. This refuses the migration with that message instead.
--
-- The table's name is built with format %I, as the other migrations of this change build theirs,
-- so the schema is quoted where its name needs it. Pasted together, a schema named Dispatch (with
-- a capital) folds to dispatch, and Postgres answers this block with SQLSTATE 3F000 (schema
-- "dispatch" does not exist) on every database in that schema, a healthy one included, rather
-- than the refusal (TestADatabaseOfTheEarliestHeadsShapeIsRefusedAtMigrate).
--
-- It reads the catalogue alone, writes nothing and refuses no stored row. The census exempts no DO
-- block, since it cannot read what one runs, but this one names no table, so nothing is measured
-- or refused.
do $$
begin
  if to_regclass(format('%I.broker_revocations', current_schema())) is not null
    and not exists (
      select 1 from pg_attribute
      where attrelid = format('%I.broker_revocations', current_schema())::regclass
        and attname = 'issuer' and attnum > 0 and not attisdropped
    ) then
    raise exception 'broker_revocations holds no issuer column: this database was migrated by a head of cybermonklabs/legion#4199 from 2026-11-16T23:38Z to 2026-11-17T04:05Z, whose table named one address per row and recorded neither the replaced account nor the account that took the address, so no revocation in it can be paid. Reset this database and the secrets broker''s database together, never one alone (docs/site/src/content/docs/dispatch/running-dispatch.md, "Signing people in"): drop both, create both empty, and let each server migrate its own.'
      using errcode = 'check_violation';
  end if;
end
$$;

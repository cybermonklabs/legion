-- packages/envoy/internal/cipher/store/migrations/0042_person_subjects.up.sql
-- An address is held by one account of the identity provider at a time, and Dispatch decides
-- which. When another Google account takes an address Dispatch holds for a person, Dispatch
-- names the account it replaced, by its issuer and its `sub` (POST /v1/person-subjects/revoke),
-- and the broker ends every credential that account holds under the address, whenever it was
-- issued, and every session it holds on the broker's own approval page, refuses it any new one,
-- and leaves the new holder's alone. That needs the broker to know which account made each, which
-- until now it never recorded: a sign-in on a machine named the address alone, a machine login
-- the address that approved it, and an approval-page session the address that signed in.
--
-- So each of the three a person makes records the account that made it:
--   - a sign-in on a machine, the issuer and `sub` of the person's own Google identity token the
--     trade presented (store.RecordPersonSignIn);
--   - a person's machine login, the issuer and `sub` of the account that approved it, which
--     Dispatch's approval relays beside the address (machine.Service.ApplyDecision);
--   - a session on the broker's own approval page, the issuer and `sub` of the ID token the
--     page's sign-in verified (store.CreateBrowserSession).
-- Each is null on every row written before this migration, and on a machine login whose approval
-- an older Dispatch relayed: such a row is read as possibly the replaced account's, and the revoke
-- ends it too. A sign-in row with none recorded is ended by its next trade, which writes a row of
-- its own (0043 refuses any change of a recorded account). The two columns of each pair are set
-- together or not at all, and neither is ever empty, so a row either names one account or says
-- plainly that it names none. An issuer is stored as oidc.CanonicalIssuer spells it, which is the
-- spelling Dispatch names it by.
--
-- replaced_person_subjects is what the revoke leaves behind: one row per account an address no
-- longer belongs to, which every later sign-in trade, machine-login approval and approval-page
-- sign-in of that account under that address is refused by. It is keyed by all three, so a repeat
-- revoke adds no row, and the moment it was first recorded is kept. A row ends only when a
-- revoke names that account as the holder again, which is an address coming back to an account it
-- was taken from. Nothing sweeps it: the account it names stays refused for as long as another one
-- holds the address, and a row holds no secret, a `sub` being an identifier the provider prints in
-- every token it mints.
--
-- session_generation is where the replacement happened, as Dispatch counts it: the address's
-- session generation (Dispatch's user_sessions.generation) once the replacing sign-in recorded its
-- account, which every revoke carries (session_generation; the route answers 400 without it). Every
-- recording of an account at Dispatch moves that generation in its own transaction, and Dispatch's
-- approve carries the generation its check of the approver read (approver_session_generation), so
-- an approval whose generation is below a row's was checked before that replacement, whatever
-- account it names, and is refused (machine.Service.ApplyDecision): the revoke has ended every
-- credential of the address that records no account, and would not end the one such an approval
-- mints. An approval that carries no generation (an older Dispatch's) and names no account is held
-- to a time window after replaced_at instead. A repeat raises the row's generation to the greatest
-- any revoke of the row carried, and never lowers it.
--
-- Its guard (guard_replaced_person_subjects) holds every row to those writers, whatever statement
-- is sent, and refuses any other with SQLSTATE 23514 (check_violation): a row removed stops the
-- refusal of its account, and a generation lowered, or a replaced_at moved, lets through an
-- approval checked before the replacement. person, issuer, subject and replaced_at are written
-- once, at the insert; session_generation only rises; a row is deleted only by a statement whose
-- own transaction first names exactly that row in the setting broker.replaced_subject_delete, as
-- jsonb_build_array(pg_current_xact_id()::text, person, issuer, subject)::text (set with
-- set_config, is_local true), which the revoke sets for its delete of the holder's row
-- (store.RecordReplacedSubjectTx) and the operator's clear for the one row it clears; and the
-- table is never truncated. The marker carries its transaction's id because a setting can outlive
-- its transaction: one set with is_local false stays on the connection, which a pool hands to
-- later transactions, and a marker naming the row alone would let such a transaction delete it
-- unnamed. It is a JSON array because no field excludes any character: fields joined by a
-- separator give two rows whose fields join to the same text one marker, such as
-- ('x@example.com|y', 'z') and ('x@example.com', 'y|z') of one subject, and a delete whose where
-- covers both then removes both. The table and its guard are made in this one migration, so no
-- broker holds the table without the guard.
--
-- person_replacement_generations keeps, for each address, the greatest session generation a revoke
-- carried and the broker applied. Dispatch pays an address's revocations one at a time, in the
-- order of their session generations, and claims the next only once the last is settled: by the
-- broker's answer, after which it was applied, or by the replaced account taking the address back,
-- when it must not apply. But a revoke can reach the broker late: Dispatch gives up after its
-- client's 5 s and may claim the address's next revocation 30 s after the last claim, while the
-- broker may still begin a request's transaction up to 45 s after it arrived. So when B took the
-- address from A and A took it back, the revoke of B (holder A) can commit before the first revoke
-- of A (holder B), which would then record A, the holder, replaced, and delete the record of B.
-- So the revoke reads this row first, under the person's lock: one carrying a lower generation
-- than the stored one was superseded by a later replacement and writes and ends nothing
-- (store.RecordReplacedSubjectTx answers the stored generation, and the broker answers "superseded";
-- a late revoke's answer goes to a call Dispatch gave up on, and one Dispatch still waits on is left
-- owed and reported, since Dispatch's generation for the address is then below the broker's); one
-- at or above it applies and raises the row to its generation. Every revoke carries a generation
-- (the route answers 400 without one). Its guard
-- (guard_person_replacement_generations) holds the row to that writer, refusing any other with
-- SQLSTATE 23514: person is written once, session_generation only rises, a row is never deleted,
-- and the table is never truncated, since a row removed or lowered lets a late revoke apply again.
--
-- Earlier heads of cybermonklabs/legion#4199 shipped this migration without session_generation, the
-- guards and person_replacement_generations, or (7142c3108a) with a guard whose delete marker joined
-- its fields with '|', so a broker database an earlier head migrated keeps that shape: there the
-- revoke's own delete is refused (23514), as the marker it sets is not the one that guard compares,
-- or its read of person_replacement_generations fails on the missing table. Reset it (drop it,
-- create it empty, and let the broker migrate it) together with Dispatch's database, never one
-- alone, for the reason Dispatch's runbook gives (docs/site/src/content/docs/dispatch/running-
-- dispatch.md, "Signing people in"). This file was numbered 0032, then 0033, then 0037, then 0040,
-- then 0041, until it was merged onto bases whose 0032 to 0041 are other migrations
-- (0032_oauth_pod_sessions_binding, then 0033_credential_request_dismissals to 0036_request_notify,
-- then 0037_pod_carry_launcher to 0039_pod_session_registrars, then 0040_enrollment_projects, then
-- 0041_request_secrets_target). A broker database an earlier head of this branch migrated recorded
-- this file and its followers under those numbers (0032 to 0034, 0033 to 0035, 0037 to 0039, 0040
-- to 0044, or 0041 to 0047). The migrator records each file's identity and refuses a version
-- recorded under another file (pgmigrate's identity check), so such a database refuses this head
-- at its next boot, naming version 40 or 41, the first it recorded under another file; one
-- migrated before that check arrived holds no identity to compare, takes those numbers for the
-- base's files and fails at this file's first statement with SQLSTATE 42701 (the column already
-- exists). Reset either the same way, with its Dispatch database.
alter table person_signins
  add column issuer text,
  add column subject text,
  add constraint person_signins_account check (
    (issuer is null and subject is null)
    or (issuer is not null and subject is not null and issuer <> '' and subject <> ''));
alter table launcher_credentials
  add column approver_issuer text,
  add column approver_subject text,
  add constraint launcher_credentials_approver_account check (
    (approver_issuer is null and approver_subject is null)
    or (approver_issuer is not null and approver_subject is not null and approver_issuer <> '' and approver_subject <> ''));
alter table browser_sessions
  add column issuer text,
  add column subject text,
  add constraint browser_sessions_account check (
    (issuer is null and subject is null)
    or (issuer is not null and subject is not null and issuer <> '' and subject <> ''));
create table replaced_person_subjects (
  person text not null check (person <> '' and person = lower(person)),
  issuer text not null check (issuer <> ''),
  subject text not null check (subject <> ''),
  replaced_at timestamptz not null default now(),
  session_generation bigint not null,
  primary key (person, issuer, subject)
);

create function guard_replaced_person_subjects() returns trigger language plpgsql as $$
begin
  if tg_op = 'TRUNCATE' then
    raise exception 'replaced_person_subjects records the accounts the broker refuses under an address, and it is never truncated'
      using errcode = 'check_violation';
  end if;
  if tg_op = 'DELETE' then
    if current_setting('broker.replaced_subject_delete', true) is distinct from
        jsonb_build_array(pg_current_xact_id()::text, old.person, old.issuer, old.subject)::text then
      raise exception 'replaced_person_subjects: a row is deleted only by a statement whose transaction names it in broker.replaced_subject_delete, as the revoke naming its account as the holder and the operator''s clear do'
        using errcode = 'check_violation';
    end if;
    return old;
  end if;
  if new.person is distinct from old.person or new.issuer is distinct from old.issuer or new.subject is distinct from old.subject then
    raise exception 'replaced_person_subjects.person, issuer and subject name the address and the replaced account, and they are written once'
      using errcode = 'check_violation';
  end if;
  if new.replaced_at is distinct from old.replaced_at then
    raise exception 'replaced_person_subjects.replaced_at is when the account was first recorded replaced, and it never moves'
      using errcode = 'check_violation';
  end if;
  if new.session_generation < old.session_generation then
    raise exception 'replaced_person_subjects.session_generation is the session generation the replacement happened at, and it only rises'
      using errcode = 'check_violation';
  end if;
  return new;
end
$$;

create trigger replaced_person_subjects_guarded before update or delete on replaced_person_subjects
  for each row execute function guard_replaced_person_subjects();

create trigger replaced_person_subjects_not_truncated before truncate on replaced_person_subjects
  for each statement execute function guard_replaced_person_subjects();

create table person_replacement_generations (
  person text primary key check (person <> '' and person = lower(person)),
  session_generation bigint not null
);

create function guard_person_replacement_generations() returns trigger language plpgsql as $$
begin
  if tg_op in ('DELETE', 'TRUNCATE') then
    raise exception 'person_replacement_generations holds the latest replacement the broker applied at each address, and its rows are never removed: a row removed lets a late revoke apply again'
      using errcode = 'check_violation';
  end if;
  if new.person is distinct from old.person then
    raise exception 'person_replacement_generations.person names the address, and it is written once'
      using errcode = 'check_violation';
  end if;
  if new.session_generation < old.session_generation then
    raise exception 'person_replacement_generations.session_generation is the latest replacement the broker applied, and it only rises'
      using errcode = 'check_violation';
  end if;
  return new;
end
$$;

create trigger person_replacement_generations_guarded before update or delete on person_replacement_generations
  for each row execute function guard_person_replacement_generations();

create trigger person_replacement_generations_not_truncated before truncate on person_replacement_generations
  for each statement execute function guard_person_replacement_generations();

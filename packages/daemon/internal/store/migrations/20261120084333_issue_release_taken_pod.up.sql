-- 20261120084333_issue_release_taken_pod.up.sql — a release's mark records that its teardown has
-- taken the pod, and the database refuses the writes of a claim of that issue the take forbids.
--
-- taken_pod is null until the teardown's last read, the one the runtime asks under its pod lock
-- (store.IssueRelease with IssueReleased.Takes), answers that the pod goes. That read writes it
-- in its own transaction, naming the issue's Sandbox ('' where the runtime names none), and the
-- mark keeps it until the release ends: its finish or its cancel deletes the mark. A teardown
-- that fails after that read and before the runtime touched the pod ends it sooner, in the
-- transaction that records the row's failure (store.ClearIssuePodTakeTx), since the pod then
-- stands as it stood; one that failed after the runtime touched the pod leaves it for the row's
-- retry, which ends the release. The runtime acts on that read and reads the store no more.
--
-- While a mark carries the take, two writes of a claim of its issue are refused: one that makes
-- the claim suspended by the operator, unless it already is (such a suspension would keep no pod),
-- and one that starts the claim's launch, the claim written launching where it was not (the claim
-- would stand live in a pod that is going, and the operator's suspension of it would hold
-- nothing). The trigger below is the one statement of both rules, and every statement that writes
-- claims meets it — the store's own upsert, an update through an alias or the schema's name, a
-- copy, a statement no caller of the store's makes — so no path writes either past the take. The
-- store's launch check refuses a launch before its first write (store.Store.CheckLaunch). The
-- trigger takes the shared serializer the last read takes too before it reads the take, so the
-- two are ordered: a write committed before the last read is a claim that read sees, and one
-- after it reads the take. An upsert meets it as an insert before the claim's row is locked, so
-- the serializer comes before the row, the order a fact takes them in. A write that only
-- rewrites a claim the operator's already, or a launch already under way, takes nothing. Its
-- refusals carry their own SQLSTATE, LR001 for the suspension and LR002 for the launch, and a
-- detail naming the release and the Sandbox in JSON, which the store answers as
-- supervise.PodTakenError and supervise.PodTakenLaunchError (store.takeRefusal). An insert is
-- read against the row of its token that the claims table holds already, since an upsert's
-- proposed row meets this trigger before its conflict is found.
--
-- The column is nullable with no default, so every mark standing reads as one whose teardown has
-- not taken its pod, which is what each of them is until its teardown runs again, and an earlier
-- build, which names its columns, reads and writes the table as before. No earlier build writes
-- the column, so a rollback needs no step: the trigger refuses nothing while no mark carries a
-- take, and a take standing at the rollback ends with its release. The way back below removes the
-- trigger, its function and the column, and deletes this migration's version row, so the roll
-- forward applies it again; run it with the daemon stopped.
--
-- begin;
-- drop trigger claims_refused_past_a_release_take on claims;
-- drop function claims_refused_past_a_release_take();
-- alter table issue_releases drop column taken_pod;
-- delete from schema_version where version = 20261120084333;
-- commit;
alter table issue_releases add column taken_pod text;

create function claims_refused_past_a_release_take() returns trigger language plpgsql as $$
declare
  was_state text;
  was_by text;
  launches boolean;
  suspends boolean;
  take record;
begin
  if tg_op = 'UPDATE' then
    was_state := old.state;
    was_by := old.suspended_by;
  else
    select state, suspended_by into was_state, was_by from claims where token = new.token;
  end if;
  launches := new.state = 'launching' and was_state is distinct from 'launching';
  suspends := new.suspended_by = 'operator' and was_by is distinct from 'operator';
  if not (launches or suspends) then
    return new;
  end if;
  perform pg_advisory_xact_lock(hashtext('legion.apply_fact'));
  select id, taken_pod into take from issue_releases
    where project = new.project and issue = new.issue and taken_pod is not null;
  if not found then
    return new;
  end if;
  if suspends then
    raise exception using errcode = 'LR001',
      message = format('release %s of %s is deleting the issue''s pod (Sandbox %s): a claim suspended by the operator now would keep no pod',
        take.id, new.issue, take.taken_pod),
      detail = json_build_object('release', take.id, 'sandbox', take.taken_pod)::text;
  end if;
  raise exception using errcode = 'LR002',
    message = format('release %s of %s is deleting the issue''s pod (Sandbox %s): a claim launched now would stand in a pod that is going',
      take.id, new.issue, take.taken_pod),
    detail = json_build_object('release', take.id, 'sandbox', take.taken_pod)::text;
end $$;

-- The condition reads the new row alone, which an insert and an update both have; the function
-- reads what the row was.
create trigger claims_refused_past_a_release_take before insert or update on claims
  for each row
  when (new.state = 'launching' or new.suspended_by = 'operator')
  execute function claims_refused_past_a_release_take();

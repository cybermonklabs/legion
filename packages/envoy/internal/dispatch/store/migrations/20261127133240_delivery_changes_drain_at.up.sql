-- Stamp moved rows when the drain writes them. A pending row can be older than a change already
-- in the log, and retention must not remove a newer sequence first on account of that delay.
-- The log's clock_timestamp() default runs after the drain has taken 6900. A transaction can
-- start before it waits for that lock, so its transaction timestamp would not preserve this order.
create or replace function delivery_changes_drain(p_batch integer) returns bigint language plpgsql as $$
declare
  moved bigint;
begin
  if not exists (select 1 from delivery_changes_pending) then
    return 0;
  end if;
  perform pg_advisory_xact_lock(6900, 0);
  with taken as (
    delete from delivery_changes_pending
    where id in (select id from delivery_changes_pending order by id limit p_batch)
    returning id, kind, run_kind, repo, ref, t_old, t_new
  )
  insert into delivery_changes (kind, run_kind, repo, ref, t_old, t_new)
  select kind, run_kind, repo, ref, t_old, t_new from taken order by id;
  get diagnostics moved = row_count;
  if moved > 0 then
    perform pg_notify('delivery_changes', '');
  end if;
  return moved;
end $$;

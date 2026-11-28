-- 20261120111107_delivery_settings_bookkeeping.up.sql
--
-- delivery_settings_changed (0100, 0156) logs a settings change, and announces on
-- dispatch_delivery_settings, for a write that moves only who saved the settings and when: it
-- ignored the freshness columns (last_event_at, last_reconcile_at, last_error) and updated_at, and
-- not updated_by, so the settings saved again with the same values by another person
-- (PUT /api/v1/settings/delivery, delivery.PutSettings, which writes updated_by and updated_at on
-- every save) logged a change (dispatch://INFRA-2916, R14). No reader of either reads updated_by:
-- no day row, no delivery window and no intake's routing reads who saved the settings, and the
-- settings route reads the row itself. So the function now ignores updated_by too, in the one
-- branch that logs the change and announces it: a save of the same values logs nothing and wakes
-- no settings cache, as RecordEventAt, RecordReconcileSuccess and RecordReconcileError already
-- log nothing. Every other column logs and announces as before, and every insert and update still
-- sends delivery_freshness.
--
-- The two builds of a roll read the change log as before. A settings change is still a row of kind
-- `settings` with no repository, which both read as "every window may have moved": the release
-- before this one, which keeps no eval day rows, marks every delivery view of its live hub, and
-- this one marks its views too and rewrites the evals the change moves (evals.rewriteSettings). A
-- write of updated_by and updated_at alone moved nothing either build reads, so neither misses a
-- change by its absence: the release before this one marks no view for it, and this one rewrites
-- nothing. The function is replaced in place, and 0106's trigger keeps calling it by name, so
-- every write of the settings, by either build, runs this body once it commits; a write that
-- committed before it logged its change under 0156's body, which both builds read as they read any
-- settings change.
--
-- The census answers 0: one function's body is replaced, and no table is locked or row rewritten.
create or replace function delivery_settings_changed() returns trigger language plpgsql as $$
declare
  bookkeeping constant text[] := array['last_event_at', 'last_reconcile_at', 'last_error', 'updated_at', 'updated_by'];
begin
  if tg_op <> 'UPDATE' or (to_jsonb(old) - bookkeeping) is distinct from (to_jsonb(new) - bookkeeping) then
    perform delivery_change_log('settings', null, null, null, null, null);
    perform pg_notify('dispatch_delivery_settings', '');
  end if;
  if tg_op <> 'DELETE' then
    perform pg_notify('delivery_freshness', json_build_object(
      'last_event_at', new.last_event_at, 'last_reconcile_at', new.last_reconcile_at,
      'last_error', left(new.last_error, 1000))::text);
  end if;
  return null;
end $$;

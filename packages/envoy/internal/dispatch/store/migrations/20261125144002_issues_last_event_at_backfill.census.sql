-- 20261125144002 writes issues.last_event_at and last_event_at_seq, which 20261125143901 adds in
-- the same release, on every issue whose last_seq names an event, from that issue's own event of
-- last_seq: it refuses no row and changes no value any reader had before it, so there is nothing to
-- inspect before the deploy. It cannot name the columns, which do not exist yet when the census is
-- taken.
select 0;

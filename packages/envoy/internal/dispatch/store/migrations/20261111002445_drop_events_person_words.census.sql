-- One index dropped: the drop reads no row of events, refuses none and rewrites none. The census
-- report still names events, the table behind the index, so a deployment sees what the drop locks.
select 0;

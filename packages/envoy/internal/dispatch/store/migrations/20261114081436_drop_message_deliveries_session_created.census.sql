-- One index dropped: the drop reads no row of message_deliveries, refuses none and rewrites none.
-- The census report still names message_deliveries, the table behind the index, so a deployment
-- sees what the drop locks.
select 0;

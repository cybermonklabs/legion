-- 20261115130748 widens message_deliveries_state_check to admit `kept` beside the three states it
-- already admits, added `not valid`, so no stored row can fail it, and builds
-- message_deliveries_session_kept, a partial index over kept deliveries: the build reads every row
-- of the table, which the census report counts and holds to its size limit, and nothing is refused
-- or rewritten.
select 0;

-- 20261125005345_handoffs_check_states_serial.up.sql
-- The serial of the issue's check states (check_state_serials, 20261125005338) a `test` handoff
-- was written after: the route waits until every mark of the issue due when it began is settled,
-- and stores the serial then, so each check.state_changed event the tester's results caused has a
-- serial at or below it (api/check_states.go, awaitCheckStatesSettled). Null for every other
-- phase and for a handoff written before this column, whose reader waits for nothing. Nullable
-- with no default, so the release before keeps inserting handoffs without it. The column and its
-- check are two statements: an inline check is validated at once, which reads every row of the
-- table under its ACCESS EXCLUSIVE lock, while `not valid` writes the catalog alone and still
-- refuses every row written or updated after it (0120's precedent).
alter table handoffs add column check_states_serial bigint;
alter table handoffs add constraint handoffs_check_states_serial_check check (check_states_serial >= 0) not valid;

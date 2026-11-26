-- 20261125125707_check_states_owe_check_result_retractions.up.sql
-- A retraction of a role's result marks the check states of the result's check's issue owed
-- (check_states_owe_retraction, 20261125125704). Its own migration, so the transaction holds
-- check_result_retractions' SHARE ROW EXCLUSIVE alone, for the catalog write.
create trigger check_states_owe_check_result_retractions after insert on check_result_retractions
  for each row execute function check_states_owe_retraction();

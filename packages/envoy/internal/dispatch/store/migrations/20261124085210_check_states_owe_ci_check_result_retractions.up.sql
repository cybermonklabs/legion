-- 20261124085210_check_states_owe_ci_check_result_retractions.up.sql
-- A retraction of a CI job's result marks the check states of the result's check's issue owed
-- (check_states_owe_retraction, 20261124085206, which reads the result from ci_check_results for
-- this table). Its own migration, so the transaction holds ci_check_result_retractions' SHARE ROW
-- EXCLUSIVE alone, for the catalog write.
create trigger check_states_owe_ci_check_result_retractions after insert on ci_check_result_retractions
  for each row execute function check_states_owe_retraction();

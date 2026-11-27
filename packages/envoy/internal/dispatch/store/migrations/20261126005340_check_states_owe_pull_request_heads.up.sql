-- 20261126005340_check_states_owe_pull_request_heads.up.sql
-- A pull request head inserted, moved, opened, closed, merged, given to another issue or deleted
-- marks the check states of the issue it belonged to and of the issue it belongs to now owed
-- (check_states_owe_head, 20261126005334): the open heads are the heads a state is published at.
-- The update trigger fires only where head_sha, state or issue_key moved: every writer of the row
-- names each column in its `do update set` on each write, a read-through included, so a trigger
-- on the columns alone would fire on each read, and the settle's own read-through would mark the
-- issue again for ever. Its own migration, so the transaction holds pull_request_heads' SHARE ROW
-- EXCLUSIVE alone, for the catalog write.
create trigger check_states_owe_head_insert after insert on pull_request_heads
  for each row execute function check_states_owe_head();
create trigger check_states_owe_head_update after update on pull_request_heads
  for each row when ((old.head_sha, old.state, old.issue_key) is distinct from (new.head_sha, new.state, new.issue_key))
  execute function check_states_owe_head();
create trigger check_states_owe_head_delete after delete on pull_request_heads
  for each row execute function check_states_owe_head();

-- Run by hand outside a transaction, before rolling back past 20261124060000.
drop index concurrently if exists events_issue_changes;

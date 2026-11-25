-- Run by hand outside a transaction, before rolling back past 20261124060100.
drop index concurrently if exists comments_issue_unvoiced;

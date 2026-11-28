-- 20261118215934_pull_request_reads_owed_refusal.up.sql
--
-- How GitHub refuses a read that a pull request is owed, and since when; and two indexes for the
-- readers of a head.
--
--   pull_request_reads_owed.refused_as     the word of the last refusal of an owed read:
--                                          'app_not_installed', 'repository_unreadable' or
--                                          'pull_request_not_found'. Null where the read is owed
--                                          for a failure, and on every row a release before this
--                                          one wrote.
--   pull_request_reads_owed.refused_since  the time of the first refusal of this owed read. A
--                                          writer of this release sets it where the row becomes a
--                                          refusal, keeps it while the row stays one, and clears
--                                          it where the row becomes a failure
--                                          (delivery.owePullRequestRead,
--                                          delivery.recordOwedReadOutcome). Null on a refusal that
--                                          a release before this one owed.
--
-- While a read is owed, Dispatch does not know that the stored head is the head. So each reader
-- of a head by issue reads the owed row of each pull request that it answers, through the
-- renaming rule (delivery.HeadsAttributedTo, HeadAttributedTo and NewestPullRequestHead). The
-- index pull_request_reads_owed_repo_number serves that read, and the read-through's read of
-- the owed reads of its repository. Its predicate keeps a name over 1,024 bytes out of it, as
-- each reader's own condition does, so no stored name can fail the build of an entry.
--
-- The results route reads the open and merged pull requests of one repository whose commit
-- messages are not read (delivery.PullRequestsNotKnown). The partial index
-- pull_request_texts_messages_not_read holds only those rows, and the facts line reads them at
-- the next pass.
--
-- No stored row can fail a statement here. Each column added is nullable with no default, so its
-- statement writes the catalog alone. lower(repo) of a name of at most 1,024 bytes is at most
-- 1,536 bytes in any database the guard of 20261117111040 admits, below the btree's limit of
-- 2,704. Each pull_request_texts row was written by a release that checks its name first
-- (checkPullRequestKey): at most 140 ASCII characters, which lower() does not make longer.
--
-- Locks: the two alter table statements come first, so no statement takes ACCESS EXCLUSIVE on a
-- table after a build took SHARE on it. Each index reads its table once under that lock, and
-- the census reports both tables with their sizes. The release before names neither column, so
-- it serves beside this one unchanged. Rolling back past this migration is an image rollback
-- alone.
alter table pull_request_reads_owed add column refused_as text;
alter table pull_request_reads_owed add column refused_since timestamptz;

create index pull_request_reads_owed_repo_number on pull_request_reads_owed (lower(repo), number)
  where octet_length(repo) <= 1024;
create index pull_request_texts_messages_not_read on pull_request_texts (lower(repo))
  where commit_messages is null;

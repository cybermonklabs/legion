-- 20261118011345_embedding_issue_sources.up.sql — for each issue, the inputs its embeddings row's
-- text was built from, so the search coverage statement need not build that text again.
--
-- What a row means. source is what the coverage pass's record step (internal/dispatch/embedqueue,
-- recordIssueSources) found when it checked the issue: the id and the row version (xmin) of the
-- latest version of the issue's primary document, '-' for each when it has none, the
-- content_hash of the issue's embeddings row and the issue's title, joined by ':'. The step
-- writes a row only where that content_hash is the hash of the text those inputs build
-- (issue_embedding_text(title, issue_embedding_spec(key)), 0140), so a row says that the
-- embeddings row holds the issue's text as of those inputs. The coverage statement
-- (coverageQuery) computes the same expression from the current rows: where it equals the
-- recorded source, the issue's text hashes to content_hash and the statement reads the hash from
-- there; where it differs, or no row is recorded, it builds and hashes the text, as every call
-- did before. A title edit, a new spec version, a version changed in place (a new xmin), a new
-- text the trigger wrote and a restore (every row a new xmin) each change the expression, so a
-- stale row costs time and not a wrong count, but for the one coincidence embedqueue's issueSource
-- names: a version changed in place whose new xmin repeats the recorded one 2^32 transactions on.
--
-- A table of its own rather than a column of embeddings, because the record step would then
-- write rows of embeddings the triggers already wrote, and a pass over a database the triggers
-- keep current is held to writing none of those (embedqueue's
-- TestCoverChangesNothingTheTriggersAlreadyEnqueued compares each row's xmin).
--
-- No foreign key. An issue that is deleted leaves its row behind, unmatched: no issue reads it,
-- so it matches nothing and is harmless, and a record step's upsert never needs it. Without the
-- key, neither this migration nor any write here takes a lock on issues.
--
-- A new table that no earlier release names: nothing existing is read or rewritten, and the
-- release before this one neither reads nor writes it, so it rolls (cmd/dispatch/README.md,
-- "Database migrations") and the census answers 0.
--
-- A later migration that changes issue_embedding_text or issue_embedding_spec must empty this
-- table in the same migration. A recorded source names the inputs, not the function that turned
-- them into text, so every row recorded under the old function would still match and the
-- coverage statement would read as current a vector of the old text.
create table embedding_issue_sources (
  key text primary key,
  source text not null
);

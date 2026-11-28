-- 20261124130831_going_run_posts.up.sql — the posts whose words a command-bar run was given.
--
-- going_run_posts names each comment and each issue message whose words a run of the command
-- bar's agent was given: a call of the run whose answer carried the post's words
-- (api.recordGoingRunPosts), and each post of an earlier run of its conversation whose notes the
-- run's claim handed it (api.claimGoingRun). Once one of them no longer stands - its author
-- deleted it, or its row is gone - every read of the run gives none of what the run stored that
-- may quote it (api.goingRunWithdrawnOf), as going_request_posts (20261110224508) does for the
-- short answer of a pause in typing. A row is written before the answer it names reaches the
-- run's agent, so before any step or frame that holds the post's words.
--
-- Its owner pair and exclusive check are going_request_posts'. Its foreign key to going_runs
-- cascades, so the sweep's delete of a conversation takes its runs' rows with it
-- (api.sweepGoingRuns). comment_id and message_id carry no foreign key, for going_request_posts'
-- reason: a post whose row is gone stands no more than a deleted one, and a cascade from the post
-- would take the record with the row. Its one index is unique on the run and the post: a run reads
-- one post many times, and each claim of a follow-up hands it the same earlier runs, so every
-- insert does nothing on a conflict and the table holds each post of a run once; and the verdict
-- reads a run's rows by its leading column.
--
-- going_runs is new on the branch that adds this table (20261112173940), so no build of an earlier
-- release reads or writes a run while this one rolls (cmd/dispatch/README.md, "Database
-- migrations"). The table is new, so no stored row meets its check; its foreign key takes SHARE
-- ROW EXCLUSIVE on going_runs for the migration's own transaction, which holds back a run's start,
-- claim, step and end for that moment and no read. The identifier is the UTC moment this was
-- written, as `migrationid` requires of every migration past the numbered series.
create table going_run_posts (
    run_id uuid not null references going_runs(id) on delete cascade,
    comment_id uuid,
    message_id uuid,
    constraint going_run_posts_one_post check ((comment_id is null) <> (message_id is null))
);

create unique index going_run_posts_post on going_run_posts (run_id, coalesce(comment_id, message_id));

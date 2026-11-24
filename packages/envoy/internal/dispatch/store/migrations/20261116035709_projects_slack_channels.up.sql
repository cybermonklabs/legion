-- 20261116035709_projects_slack_channels.up.sql — the Slack channels a project's credentials may
-- post to, named by its owner.
--
-- R14: the secrets broker is to release the Legion Slack bot token to a private
-- project's credentials only for the channels that project names, and for no channel where it
-- names none; no broker reads the list yet.
-- The list is the project's own, so it is a column of `projects`. In this release only
-- `access.SetSlackChannels` writes it, which no route calls; a later slice adds the owner's route
-- that writes it (`slack_channels`).
--
-- The check holds every id to Slack's own form, `^[CG][A-Z0-9]{8,20}$`, and the list to 16. An array
-- check cannot test each element on its own, so it tests the joined text: the elements are joined
-- with a comma, which no id holds, and the pattern then matches exactly a comma-separated list of
-- ids, or the empty string for the empty list. A null element makes `array_to_string` drop it, so
-- a list holding one would pass this test; `SetSlackChannels` writes no null, and a null in an
-- array is not a channel id anybody can post to.
--
-- Adding a column with a constant default writes the catalog alone (Postgres 11 on), so it holds
-- the table's ACCESS EXCLUSIVE lock for milliseconds and rewrites no row; the check is on the new
-- column alone, whose every existing row reads the empty list, which it satisfies, so it refuses
-- none. The release before this one neither reads nor writes the column, and the one that carries
-- it writes it only through `access.SetSlackChannels`.
--
-- Its identifier is the UTC moment it was written, as `migrationid` requires of every migration
-- past the numbered series, which closed at 0161.
alter table projects
  add column slack_channels text[] not null default '{}'
  check (cardinality(slack_channels) <= 16
         and array_to_string(slack_channels, ',') ~ '^([CG][A-Z0-9]{8,20}(,[CG][A-Z0-9]{8,20})*)?$');

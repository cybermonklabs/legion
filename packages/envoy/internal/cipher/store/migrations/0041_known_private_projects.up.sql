-- packages/envoy/internal/cipher/store/migrations/0041_known_private_projects.up.sql
-- The broker's stored private list: the last list of private projects it read from Dispatch, with
-- their Slack channels, beside the projects a switch to private recorded through the broker before
-- Dispatch committed it (store/known_private_projects.go). When a read of Dispatch's list fails,
-- the airlock's public Slack fence fences the channels of the rows held here; while none is held
-- it fences nothing, as a broker without this table did.
--
-- One row per project the broker has reason to hold private. Every number a row holds comes from
-- the one sequence below, so no number is used twice, even after a row is deleted and made again:
-- listed is the stamp number of the newest read that named the project, record the number of its
-- last record by a switch, and forgotten the number the last forget that took effect set. A row is
-- held while greatest(listed, record) > forgotten, nulls read as 0; only a held row fences. Every
-- time a row holds is the broker database's statement_timestamp(), never a pod's clock.
--
-- slack_channels is null while the channels are unknown (a read named the project with no
-- slack_channels key, which only a Dispatch from before projects' owners could name channels
-- answers), and channels_stored_at is null exactly then: it is the database's time the stored set
-- last gained a channel, or became known. A read replaces the set with its own list (at most 16
-- ids), and a switch's record adds its list to a known set, so a set holds at most two lists'
-- worth, 32 ids.
--
-- The checks are the Go side's own rules (store.ErrPrivateProjectInput): the key is a Dispatch
-- project key (contracts.ProjectKeyPattern, written here once more because a check constraint
-- cannot call Go), every channel is a conversation id, no element is null, and every row was
-- named by a read or recorded by a switch.
create sequence if not exists known_private_projects_seq;

create table if not exists known_private_projects (
  project text primary key check (project ~ '^[A-Z][A-Z0-9]{1,9}$'),
  slack_channels text[],
  channels_stored_at timestamptz,
  listed bigint,
  listed_at timestamptz,
  record bigint,
  recorded_at timestamptz,
  forgotten bigint,
  forgotten_at timestamptz,
  constraint known_private_projects_channels_bounded
    check (slack_channels is null or (cardinality(slack_channels) <= 32 and array_position(slack_channels, null) is null)),
  constraint known_private_projects_channels_ids
    check (slack_channels is null or array_to_string(slack_channels, ' ') ~ '^([CDG][A-Z0-9]{8,}( [CDG][A-Z0-9]{8,})*)?$'),
  constraint known_private_projects_channels_time
    check ((slack_channels is null) = (channels_stored_at is null)),
  constraint known_private_projects_listed_time check ((listed is null) = (listed_at is null)),
  constraint known_private_projects_record_time check ((record is null) = (recorded_at is null)),
  constraint known_private_projects_forgotten_time check ((forgotten is null) = (forgotten_at is null)),
  constraint known_private_projects_named check (listed is not null or record is not null)
);

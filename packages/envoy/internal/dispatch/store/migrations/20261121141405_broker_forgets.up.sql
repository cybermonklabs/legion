-- 20261121141405_broker_forgets.up.sql — the forgets Dispatch owes the secrets broker.
--
-- A switch to private first asks the broker to count a project's live developer-box tokens, and
-- the broker records the project and its Slack channels in its store of private projects before it
-- counts, answering the record's number. A switch that does not make the project private owes the
-- broker the forget of that record, and so does a project's switch to public. Each row is one
-- forget owed: written by access.SetVisibility when it writes public over a private or closing
-- project (record null: the project's record, whatever its number), by the switch's closing step
-- when it refuses after the broker answered record N, and by the switch when its transaction ends
-- with no commit sent after the broker answered N, or with a commit whose outcome is unknown
-- (record null). The sender (internal/dispatch/api's sendBrokerForgets) claims due rows with
-- `for update skip locked`, deletes a row whose project is private when it reads it, sends the
-- rest, deletes each the broker takes, and moves the others' next_attempt_at on.
--
-- A new table with no rows writes the catalog alone; the release before this one neither reads nor
-- writes it, so it serves beside it unchanged.
--
-- Its identifier is the UTC moment it was written, as `migrationid` requires of every migration
-- past the numbered series, which closed at 0161.
create table broker_forgets (
  id bigint generated always as identity primary key,
  project text not null,
  record bigint check (record > 0),
  next_attempt_at timestamptz not null default now(),
  attempts integer not null default 0,
  last_error text not null default ''
);

create index broker_forgets_due on broker_forgets (next_attempt_at);

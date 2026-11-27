-- Each approval carry records the original approval's version under the new review's id.
-- There is no foreign key to artifact_reviews: this migration creates a new table only,
-- taking no lock on artifact_reviews and rewriting no existing row. The writer and the
-- review_carried_from census keep the tables in agreement.
--
-- record_sha256 is populated when a deferred render pins the carry to its query record.
-- event_id is the id of the carry's own artifact.approved, which the writer records in the
-- statement right after that event's append. The statements that complete the carried review
-- and its event run after the version's artifact.version (R14), so they name the event by this
-- id. A row the backfill writes names no event.
-- While builds roll, older writers leave this table empty for their new carries; readers
-- fall back to the stored reason and the backfill fills the missing rows.
create table artifact_review_carries (
  review_id uuid primary key,
  carried_from_version bigint not null check (carried_from_version > 0),
  record_sha256 text check (record_sha256 ~ '^[0-9a-f]{64}$'),
  event_id bigint
);

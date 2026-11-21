-- packages/envoy/internal/cipher/store/migrations/0044_person_standing.up.sql
-- The last answer Dispatch gave the broker for each person, whether Dispatch holds
-- them, so an outage of Dispatch releases no held person and a restart or a second process
-- answers alike. Each answer is written unless the row holds a later one, by Dispatch's own clock
-- (the answer's as_of). person is a login as record.CanonicalLogin spells it.
--
-- This file and its follower were numbered 0043 and 0044 until they were merged onto a base whose
-- 0041 to 0043 are other migrations (0041_person_subjects to 0043_guards_search_path). A broker
-- database an earlier head of this branch migrated recorded the two under those numbers, and the
-- migrator refuses a version recorded under another file (pgmigrate's identity check), so such a
-- database refuses this head at its next boot, naming version 43. Reset it with its Dispatch
-- database, as 0041_person_subjects says.
create table person_standing (
  person text primary key check (person <> '' and person = btrim(person) and person !~ '[A-Z]'),
  held boolean not null,
  answered_at timestamptz not null
);

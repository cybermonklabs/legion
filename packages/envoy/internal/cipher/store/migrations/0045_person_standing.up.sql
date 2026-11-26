-- packages/envoy/internal/cipher/store/migrations/0045_person_standing.up.sql
-- The last answer Dispatch gave the broker for each person, whether Dispatch holds
-- them, so an outage of Dispatch releases no held person and a restart or a second process
-- answers alike. Each answer is written unless the row holds a later one, by Dispatch's own clock
-- (the answer's as_of). person is a login as record.CanonicalLogin spells it.
--
-- This file and its follower were numbered 0043 and 0044 until the merge of #4199 at b7bd32ae64
-- (60d79893b6) put them onto a base whose 0040 is 0040_enrollment_projects, which moved every file
-- after 0039 up by one: 0040_person_subjects to 0042_guards_search_path became 0041 to 0043, and
-- these two 0044 and 0045. A base whose 0041 is 0041_request_secrets_target then moved every file
-- after 0040 up by one again: 0041_person_subjects to 0047_project_standing became 0042 to 0048,
-- and these two 0045 and 0046. Only a broker database that a head of this branch before either
-- move migrated recorded these files under the earlier numbers. The migrator refuses a version
-- recorded under another file (pgmigrate's identity check), so such a database refuses this head
-- at its next boot, naming version 40 or 41, the first of them, which it recorded as
-- 0040_person_subjects or 0041_person_subjects. Reset it with its Dispatch database, as
-- 0042_person_subjects says. The replaces lines the file carried at 0044, which named the texts of
-- its two in-place edits, went with that number: a replaces line names a file of its own version.
create table person_standing (
  person text primary key check (person <> '' and person = btrim(person) and person !~ '[A-Z]'),
  held boolean not null,
  answered_at timestamptz not null
);

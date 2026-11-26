-- packages/envoy/internal/cipher/store/migrations/0048_person_standing.up.sql
-- The last answer Dispatch gave the broker for each person, whether Dispatch holds
-- them, so an outage of Dispatch releases no held person and a restart or a second process
-- answers alike. Each answer is written unless the row holds a later one, by Dispatch's own clock
-- (the answer's as_of). person is a login as record.CanonicalLogin spells it.
--
-- An IAP deployment's Dispatch puts a deadline on every answer that a person is not held
-- (not_held_until): the last active Workspace Directory answer plus 24 hours, or the answer's own
-- as_of once that passed. The broker keeps it with the answer, written under the same answered_at
-- rule, so a kept not-held answer serves until its deadline and not at it, at a restart and at a
-- second broker process as at the first. Null for a held answer and for every answer outside IAP.
create table person_standing (
  person text primary key check (person <> '' and person = btrim(person) and person !~ '[A-Z]'),
  held boolean not null,
  answered_at timestamptz not null,
  not_held_until timestamptz
);

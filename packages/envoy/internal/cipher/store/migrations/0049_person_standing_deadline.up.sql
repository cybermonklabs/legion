-- packages/envoy/internal/cipher/store/migrations/0049_person_standing_deadline.up.sql
-- An IAP deployment's Dispatch puts a deadline on every answer that a person is not held
-- (not_held_until): the last active Workspace Directory answer plus 24 hours, or the answer's own
-- as_of once that passed. The broker keeps it with the answer, written under the same answered_at
-- rule, so a kept not-held answer serves until its deadline and not at it, at a restart and at a
-- second broker process as at the first. Null for a held answer and for every answer outside IAP.
-- A nullable column added with no default writes the catalog alone: no existing row is read or
-- changed.
alter table person_standing add column not_held_until timestamptz;

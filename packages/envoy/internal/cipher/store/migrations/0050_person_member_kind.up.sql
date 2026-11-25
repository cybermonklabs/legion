-- packages/envoy/internal/cipher/store/migrations/0050_person_member_kind.up.sql
-- The member kind the last answer of Dispatch's person route named for each person, so that while
-- Dispatch cannot be asked the broker serves a person last answered a full member as one and
-- refuses everyone else (projectaccess.Client.MemberKind), at a restart and at a second broker
-- process as at the first. person is a login as record.CanonicalLogin spells it. member_kind is as
-- Dispatch spelled it, null for an answer that named none. as_of is the time the answer rests on
-- (its as_of), and an answer is written unless the row holds one Dispatch gave later.
--
-- A table of its own rather than columns of person_standing, ordered by its own as_of: every
-- answer names a kind, while one whose held Dispatch left out (its own read of the hold failed)
-- keeps nothing in person_standing, so the kind of such an answer, a demotion among them, is kept
-- here whatever the hold. A kept kind is served only while its as_of is no older than the
-- answered_at of the hold person_standing keeps for the person (store.KeptMemberKind): a broker
-- release that keeps no kind writes person_standing alone, and its later answer, whatever kind it
-- named, leaves the earlier kind here unserved.
create table person_member_kind (
  person text primary key check (person <> '' and person = btrim(person) and person !~ '[A-Z]'),
  member_kind text,
  as_of timestamptz not null
);

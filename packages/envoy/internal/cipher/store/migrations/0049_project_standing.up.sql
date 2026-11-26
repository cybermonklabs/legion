-- packages/envoy/internal/cipher/store/migrations/0049_project_standing.up.sql
-- The last answer Dispatch's project route gave the broker for each person and project, so that
-- while Dispatch cannot be asked the request machine's person rule (requests.Machine.maySee,
-- through projectaccess.OrKept) shows a person a project Dispatch last answered public or visible
-- to them, and no other, at a restart and at a second broker process as at the first. person is a
-- login as record.CanonicalLogin spells it, or '' for a question naming no person and for an
-- answer that Dispatch holds no such project, which is the same for everyone. visibility is
-- 'public' or 'private' as Dispatch answered it, and null for a project Dispatch does not hold.
-- Dispatch's project answer names no time of its own, so asked_at is when the broker asked, read
-- before the question was sent, and an answer is written unless the row holds one asked later.
-- A sibling of person_standing rather than columns of it: that table keeps one answer per person,
-- ordered by Dispatch's own as_of, and this one an answer per person and project, ordered by the
-- broker's question time.
create table project_standing (
  person text not null check (person = btrim(person) and person !~ '[A-Z]'),
  project text not null check (project <> ''),
  visibility text check (visibility in ('public', 'private')),
  visible boolean not null,
  asked_at timestamptz not null,
  primary key (person, project)
);

-- 20261109221325_asks_asked_of.up.sql — the person an ask waits on, and who handed it to them.
--
--   asked_of     the canonical login (model.CanonicalLogin: lower case, trimmed) of the person
--                this ask waits on, null until a person hands the ask on, which is every row
--                this migration leaves. Null means the ask waits on the assignee of its issue.
--   asked_of_by  the canonical login of the person who last handed the ask on, null until a
--                person first does. A hand-on to the assignee of the ask's own issue writes it
--                beside a null asked_of.
--   asked_of_at  when that hand-on was written, null and written as asked_of_by is.
--
-- INFRA-1576: a Dispatch ask reaches the assignee of its issue and nobody else, so a question
-- meant for one person sat in another person's Inbox and the person it was meant for never saw
-- it. The field is named asked_of and not `recipient` because `recipient` already means the
-- sessions an answer reaches (AskRecipients in the web app). One expression in the server,
-- coalesce(a.asked_of, i.assignee), gives the person an ask waits on, so an ask opened before
-- this migration, and an ask nobody hands on after it, reaches exactly the person it reaches
-- today and no backfill is owed. The pair beside it is what the ask card names: who moved the
-- ask here, and when.
--
-- The three columns are nullable with no default, so the statement writes the catalog rows and
-- nothing else: no row is rewritten and none is read, and asks is held in ACCESS EXCLUSIVE for
-- that write alone. One statement adds all three, so a write of asks waits for one lock and not
-- for three. The release before this one neither reads nor writes them and names its columns in
-- every insert of an ask (api/asks.go), so it keeps opening and editing asks against this schema
-- while it still serves (cmd/dispatch/README.md, "Database migrations").
--
-- asked_of_at is timestamptz, so the census's catalog-only exemption, which covers a nullable
-- text column alone (internal/pgmigrate/exempt.go, catalogOnlyAlterTable), does not cover this
-- migration and CensusTableLimit applies to asks: one gigabyte of pg_total_relation_size over the
-- table, its indexes and its TOAST (internal/pgmigrate/census.go). asks is far below that bound.
--
-- Its identifier is the UTC moment it was written, as `migrationid` requires of every migration
-- past the numbered series, which closed at 0161.
alter table asks
  add column asked_of text,
  add column asked_of_by text,
  add column asked_of_at timestamptz;

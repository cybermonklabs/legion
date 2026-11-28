-- A second key reading, for the case key-reading-changes of verification-results.ts. The driver
-- applies it with psql while no server runs, as a migration of a later release would apply it.
--
-- It takes the lock and makes the change that the migration 20261117122117_attribution_reading
-- names for a change of the reading. The body is that migration's body, with one change: the
-- branch arm reads the branch as it is written. The migration's arm writes each lower-case letter
-- in upper case first. So a branch such as legion/acme-12 names no key on this reading, and a
-- branch such as legion/ACME-12 still names ACME-12.
--
-- create or replace keeps the function's oid, so attribution_reading() gives a new identity at
-- once. The stored keys of each row are then on another reading. The readers compute the keys of
-- such a row from its texts, and the key remake loop of the server writes them again.
begin;
lock table pull_request_texts in share row exclusive mode;
create or replace function attribution_keys(repo text, head_ref text, title text, body text, messages text[],
	out title_keys text[], out cited_issues text[], out branch_keys text[], out commit_keys text[])
language sql immutable parallel safe
set search_path = pg_catalog
begin atomic
select
  (select coalesce(pg_catalog.array_agg(k order by n), '{}') from (select m[1] k, pg_catalog.min(n) n from pg_catalog.regexp_matches((coalesce(title, '') || E'\n' || coalesce(body, '')) collate "C", '(?<![A-Za-z0-9_])([A-Z][A-Z0-9]{1,9}-[1-9][0-9]*)(?![A-Za-z0-9_])', 'g') with ordinality r(m, n) where pg_catalog.octet_length(m[1]) <= 1024 group by m[1]) t),
  (select coalesce(pg_catalog.array_agg(u order by n), '{}') from (select u, pg_catalog.min(n) n from (
     select pg_catalog.translate((case when m[4] is null then 'https://github.com/' || m[1] || '/' || m[2] || '/issues/' || m[3]
       else 'https://github.com/' || repo || '/issues/' || m[4] end) collate "C", 'ABCDEFGHIJKLMNOPQRSTUVWXYZ', 'abcdefghijklmnopqrstuvwxyz') u, n
     from pg_catalog.regexp_matches(coalesce(body, '') collate "C", '(?<![A-Za-z0-9.-])[Gg][Ii][Tt][Hh][Uu][Bb]\.[Cc][Oo][Mm](?::[0-9]{1,252})?/([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+)/issues/([1-9][0-9]*)(?![A-Za-z0-9_])|(?:^|[^A-Za-z0-9_&/#])#([1-9][0-9]{1,5})(?![A-Za-z0-9_])', 'g') with ordinality r(m, n)) c where pg_catalog.octet_length(c.u) <= 1024 group by u) d),
  (select coalesce(pg_catalog.array_agg(k order by n), '{}') from (select m[1] k, pg_catalog.min(n) n from pg_catalog.regexp_matches(coalesce(head_ref, '') collate "C", '(?<![A-Za-z0-9_])([A-Z][A-Z0-9]{1,9}-[1-9][0-9]*)(?![A-Za-z0-9_])', 'g') with ordinality r(m, n) where pg_catalog.octet_length(m[1]) <= 1024 group by m[1]) t),
  (select coalesce(pg_catalog.array_agg(k order by n), '{}') from (select m[1] k, pg_catalog.min(n) n from pg_catalog.regexp_matches(pg_catalog.array_to_string(coalesce(messages, '{}'), E'\n') collate "C", '(?<![A-Za-z0-9_])([A-Z][A-Z0-9]{1,9}-[1-9][0-9]*)(?![A-Za-z0-9_])', 'g') with ordinality r(m, n) where pg_catalog.octet_length(m[1]) <= 1024 group by m[1]) t);
end;
commit;

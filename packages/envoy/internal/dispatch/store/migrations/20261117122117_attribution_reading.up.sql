-- 20261117122117_attribution_reading.up.sql
--
-- The database reads the keys of a pull request from its stored texts, so no stored key can
-- outlive the texts it was read from or the reading that read it.
--
--   attribution_keys(repo, head_ref, title, body, messages)
--                        the reading: the issue keys the title and the body name, the GitHub
--                        issues the body cites, the keys the head branch names and the keys the
--                        commit messages name, each in text order with the first place of each
--                        entry, the commit messages read newest first: GitHub lists a pull
--                        request's commits oldest first, and the head commit's key is the
--                        branch's word now, where an older commit's is history the branch may
--                        carry from another pull request's work (a later pull request credited
--                        to a closed issue by an older commit's `Dispatch:` trailer, event 162 of
--                        that issue, 2026-11-25). A cited issue is
--                        https://github.com/<owner>/<repo>/issues/<n>, from a path
--                        /<owner>/<repo>/issues/<n> whose host is github.com in any case
--                        with an optional port of up to 253 digits, as the Go reading credited
--                        (the byte before the host is no letter, digit, dot or hyphen), or from a
--                        bare #n of two to six digits, not after a word
--                        character, &, / or #, which names an issue of repo; each cited URL is in
--                        ASCII lower case. One pass over the body finds every path and every bare
--                        #n, each with the text since the match before it, which gives each path's
--                        byte offset. The host of a path is then decided from a slice of the body:
--                        the 507 bytes before the path, or the bytes before it where the path
--                        stands nearer the start, less the trailing bytes of a character the slice
--                        cuts at its start. 507 is 2 x 253 + 1, a port of up to 253 digits, its
--                        colon and a host name of up to 253 bytes, the bound the Go reading of the
--                        default branch gives its own host check. So the check, as it is or as a
--                        later change makes it, reads no more than 507 bytes before a path,
--                        however long the body is. The branch is upper-cased as
--                        Go's strings.ToUpper does it for a key: a to z, and the two runes outside
--                        ASCII that it maps to ASCII letters, ı to I and ſ to S. The commit keys
--                        are none where the messages are null, which is where a read did not read
--                        them. Each list keeps only entries of at most 1,024 bytes: a GIN entry
--                        holds at most 2,712 bytes, and a key or a URL of random characters does
--                        not compress. It is sql with an SQL-standard body, so its calls bind when
--                        it is created; each names pg_catalog, and its search path is pg_catalog.
--                        Every pattern, translate and concatenation takes its text under the
--                        collation "C", where only A to Z are letters and a range is a range of
--                        code points, so no locale takes part: each database the guard of
--                        20261117111040 admits gives the same keys.
--   pull_request_texts_keys()
--                        the trigger function: before each insert and each update of a texts
--                        row, it sets the four key columns from the row's own new texts and
--                        `reading` to the identity of the reading that runs now. A writer cannot
--                        store keys; the trigger overwrites any it sends. It is plpgsql, which
--                        looks a name up when it first runs in a session, so it names the schema
--                        of each function it calls and keeps the search path of this migration
--                        (set search_path from current).
--   attribution_reading()
--                        the identity of the reading: the md5 of the body of attribution_keys as
--                        the catalog stores it (prosqlbody), with every place in its source text
--                        left out (the :location, :stmt_location and :stmt_len fields, which
--                        Postgres 16 fills and Postgres 17 writes as -1), so a restore that
--                        creates the function again from its own definition keeps the identity;
--                        its strictness and its settings, the source and the settings of the
--                        trigger function, and the major version
--                        of Postgres. No session setting, locale, collation version or minor
--                        version enters it; a major version can change the engine of the
--                        patterns, and pg_upgrade creates the function again. It is sql with an
--                        SQL-standard body, so it binds to the two functions when it is created.
--
-- A reader takes the stored keys of a row only where its `reading` is attribution_reading(), and
-- computes the keys from the row's texts where it is not. So a change of the reading is one
-- migration in a later release, and it reaches every row with no read of GitHub:
--
--   lock table pull_request_texts in share row exclusive mode;
--   create or replace function attribution_keys(…) … ;
--
-- The lock keeps a write of texts from spanning the change, and the census admits both
-- statements as catalog writes. A change of a comment or of white space inside the body changes
-- the identity too: that costs a remake of the keys and gives no wrong key. A migration remakes
-- no keys, this one included: one update of every row would hold each row's lock for as long as
-- the whole statement runs.
--
-- The functions are created in the schema of the migration. The trigger function and the
-- identity name that schema, which they can only name once it is known, so the DO block writes
-- them with format('%I', current_schema()); the identity comes after the trigger function, to
-- which it binds. The create trigger statement stands outside the block, so the census reads the
-- table it names.
--
-- No stored row can fail a statement here: each statement writes the catalog alone. The trigger
-- runs on later writes only, and a row stored before it, with the defaults of 20261117111040,
-- is a row on another reading. Locks: create trigger takes SHARE ROW EXCLUSIVE on
-- pull_request_texts to the commit; the functions lock no table. The release before names
-- neither the functions nor the trigger and writes no texts row, so it serves beside this one
-- unchanged. Rolling back past this migration is an image rollback alone.
create function attribution_keys(repo text, head_ref text, title text, body text, messages text[],
	out title_keys text[], out cited_issues text[], out branch_keys text[], out commit_keys text[])
language sql immutable parallel safe
set search_path = pg_catalog
begin atomic
select
  (select coalesce(pg_catalog.array_agg(k order by n), '{}') from (select m[1] k, pg_catalog.min(n) n from pg_catalog.regexp_matches((coalesce(title, '') || E'\n' || coalesce(body, '')) collate "C", '(?<![A-Za-z0-9_])([A-Z][A-Z0-9]{1,9}-[1-9][0-9]*)(?![A-Za-z0-9_])', 'g') with ordinality r(m, n) where pg_catalog.octet_length(m[1]) <= 1024 group by m[1]) t),
  (select coalesce(pg_catalog.array_agg(u order by n), '{}') from (select u, pg_catalog.min(n) n from (
     select pg_catalog.translate((case when m[4] is null then 'https://github.com/' || m[1] || '/' || m[2] || '/issues/' || m[3]
       else 'https://github.com/' || repo || '/issues/' || m[4] end) collate "C", 'ABCDEFGHIJKLMNOPQRSTUVWXYZ', 'abcdefghijklmnopqrstuvwxyz') u, n
     from (select r.m[3:6] m, r.n, (pg_catalog.sum(pg_catalog.octet_length(r.m[1]) + pg_catalog.octet_length(r.m[2])) over (order by r.n) - pg_catalog.octet_length(r.m[2]))::integer path_at
       from pg_catalog.regexp_matches(coalesce(body, '') collate "C", '(.*?)(/([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+)/issues/([1-9][0-9]*)(?![A-Za-z0-9_])|(?:^|[^A-Za-z0-9_&/#])#([1-9][0-9]{1,5})(?![A-Za-z0-9_]))', 'g') with ordinality r(m, n)) p,
       (select pg_catalog.convert_to(coalesce(body, ''), 'UTF8') bytes offset 0) b
     where m[4] is not null or pg_catalog.convert_from(pg_catalog.ltrim(pg_catalog.substring(b.bytes, greatest(path_at - 507, 0) + 1, least(path_at, 507)), '\x808182838485868788898a8b8c8d8e8f909192939495969798999a9b9c9d9e9fa0a1a2a3a4a5a6a7a8a9aaabacadaeafb0b1b2b3b4b5b6b7b8b9babbbcbdbebf'::bytea), 'UTF8') collate "C"
       ~ '(?<![A-Za-z0-9.-])[Gg][Ii][Tt][Hh][Uu][Bb]\.[Cc][Oo][Mm](?::[0-9]{1,253})?$') c where pg_catalog.octet_length(c.u) <= 1024 group by u) d),
  (select coalesce(pg_catalog.array_agg(k order by n), '{}') from (select m[1] k, pg_catalog.min(n) n from pg_catalog.regexp_matches(pg_catalog.translate(coalesce(head_ref, '') collate "C", 'abcdefghijklmnopqrstuvwxyzıſ', 'ABCDEFGHIJKLMNOPQRSTUVWXYZIS'), '(?<![A-Za-z0-9_])([A-Z][A-Z0-9]{1,9}-[1-9][0-9]*)(?![A-Za-z0-9_])', 'g') with ordinality r(m, n) where pg_catalog.octet_length(m[1]) <= 1024 group by m[1]) t),
  (select coalesce(pg_catalog.array_agg(k order by n), '{}') from (select m[1] k, pg_catalog.min(n) n from pg_catalog.regexp_matches(pg_catalog.array_to_string(array(select u.m from pg_catalog.unnest(coalesce(messages, '{}')) with ordinality u(m, i) order by u.i desc), E'\n') collate "C", '(?<![A-Za-z0-9_])([A-Z][A-Z0-9]{1,9}-[1-9][0-9]*)(?![A-Za-z0-9_])', 'g') with ordinality r(m, n) where pg_catalog.octet_length(m[1]) <= 1024 group by m[1]) t);
end;

do $reading$
begin
  execute pg_catalog.format($function$
create function %1$I.pull_request_texts_keys() returns trigger
language plpgsql
set search_path from current
as $body$
begin
  select k.title_keys, k.cited_issues, k.branch_keys, k.commit_keys
    into new.title_keys, new.cited_issues, new.branch_keys, new.commit_keys
    from %1$I.attribution_keys(new.repo, new.head_ref, new.title, new.body, new.commit_messages) k;
  new.reading := %1$I.attribution_reading();
  return new;
end
$body$$function$, pg_catalog.current_schema());
  execute pg_catalog.format($function$
create function %1$I.attribution_reading() returns text
language sql stable parallel safe
set search_path = pg_catalog
begin atomic
  select pg_catalog.md5(pg_catalog.regexp_replace(p.prosqlbody::text,
      '[[:space:]]:(location|stmt_location|stmt_len) -?[0-9]+', '', 'g')
    || ' ' || p.proisstrict::text || ' ' || coalesce(p.proconfig::text, '')
    || ' ' || t.prosrc || ' ' || coalesce(t.proconfig::text, '')
    || ' ' || (pg_catalog.current_setting('server_version_num')::integer / 10000)::text)
  from pg_catalog.pg_proc p, pg_catalog.pg_proc t
  where p.oid = %2$L::pg_catalog.regprocedure and t.oid = %3$L::pg_catalog.regprocedure;
end$function$,
    pg_catalog.current_schema(),
    pg_catalog.format('%I.attribution_keys(text,text,text,text,text[])', pg_catalog.current_schema()),
    pg_catalog.format('%I.pull_request_texts_keys()', pg_catalog.current_schema()));
end
$reading$;

create trigger pull_request_texts_keys before insert or update on pull_request_texts
  for each row execute function pull_request_texts_keys();

-- 20261117111040_pull_request_texts.up.sql
--
-- The texts a pull request's issue is read from, stored as GitHub answers them, so that the
-- issue can be an answer of the rule over stored facts and not a verdict a writer stored. A
-- change of the rule, a new link, a moved issue or a new issue then reaches every stored pull
-- request with no read of GitHub.
--
--   pull_request_texts   one row per pull_request_heads row, keyed (repo, number) as that row
--                        is, which it follows on a rename (on update cascade) and goes with on a
--                        delete (on delete cascade): the head, the branch, the head repository
--                        ('' where GitHub names none, a deleted fork), the base, the title, the
--                        body and the commit messages of one read of GitHub, with GitHub's
--                        updated_at of that read and the moment it was stored. commit_messages is
--                        null where the read did not read the messages. The four key columns and
--                        `reading` are what a later migration's trigger computes from the row's
--                        own texts; until then they hold their defaults.
--   pull_request_heads.attribution_refused_at, .attribution_absent_at
--                        when a read of the pull request's facts was last refused, and when GitHub
--                        proved that no such pull request exists. Both null on every row stored
--                        before this migration.
--   delivery_pull_requests.facts_refused_at
--                        the same refusal stamp for a timeline row with no results row.
--
-- The guard comes first. The reading of keys runs under the collation "C", and every reader by
-- issue compares repository names and links with lower() under the database's own collation,
-- as the other readers do. The two answer alike only where the database is UTF8 and its
-- collation folds each ASCII letter as "C" does, so attribution_database_refusal() answers the
-- line that names the encoding or the collation of any other database, and null for this one;
-- the DO block raises that line, so a refused database stores nothing of this migration. Each
-- start of Dispatch calls the same function. The census file beside this one holds the same
-- test, so a refused database stops the deploy before the serving release stops.
--
-- The write of a row's issue alone is no longer a write of a fact. From this release on, the
-- issue of a pull request is the answer of one statement over its stored facts, and a pass of the
-- reconcile writes that answer to the row's issue_key and attribution_source with no read of
-- GitHub. So pull_request_heads_unread_write, as 20261116182209 left it, is replaced here with the
-- same body without issue_key and attribution_source in its two tuples: a write of the issue
-- alone neither takes the row's mark away nor owes a read, and a write of the head, its branch,
-- the base, the state, the title, the URL or the time still does both. A foreign key's
-- `on delete set null` of issue_key no longer costs the row a read either.
-- pull_request_heads_older_read does not name the issue and stays as it is. A stamp of
-- facts_refused_at is no change a page shows, so delivery_pull_requests_changed, as 0100 wrote
-- it, is replaced with facts_refused_at beside the two columns it already ignores; a write of
-- issue_key still logs a change.
--
-- The indexes each serve one comparison of the statement that reads a pull request's issue:
-- pull_request_heads_number and delivery_pull_requests_number the joins through the renaming
-- rule, which compare the number alone; pull_request_heads_url_lower the arm that a link of an
-- issue names; delivery_pull_requests_issue the arm of timeline rows by issue;
-- pull_request_texts_keys and pull_request_texts_reading the keys and the reading of the texts.
-- The existing lower() indexes stay, because the statement uses them.
--
-- No stored row can fail a statement here; only the guard can refuse the migration, over the
-- database itself. Each column added is nullable with no default, so its statement writes the
-- catalog alone. A function replaced reads no row. pull_request_texts is new and empty, and the
-- primary key (repo, number) its foreign key references exists. number is an integer, so an
-- entry of either index on it holds 4 bytes. Each issue key of a timeline row is the key of an
-- issue (0080's foreign key), which the primary key of issues already holds in a btree of the
-- same type. A URL is the one value an older row can hold beyond a btree entry (2,704 bytes):
-- each release before 20261116163242 stored the URL of an envelope with no check, and a failed
-- index build fails the whole migration. So the index on lower(url) is partial, over URLs of at
-- most 1,024 bytes: lower() makes a text at most 1.5 times as long in bytes, so each entry holds
-- at most 1,536 bytes. A row with a longer URL is no pull request of GitHub; no link matches it,
-- and the arm that reads this index carries the same condition, so the planner reads it.
--
-- Locks: the guard and the functions lock no table. The alter table statements come before the
-- index builds, so no statement takes ACCESS EXCLUSIVE on a table after a build took SHARE on it:
-- pull_request_heads and delivery_pull_requests are each held ACCESS EXCLUSIVE for the catalog
-- write from their alter table to the commit, and each index on them reads the table once under
-- that lock. The foreign key takes SHARE ROW EXCLUSIVE on pull_request_heads, which the migration
-- already holds. The census holds both tables to its 1 GiB limit (the census file says why).
--
-- The release before names neither the table nor the new columns, and its writes of a results
-- row run the narrowed trigger, so it serves beside this one unchanged. A later release drops
-- attribution_rule, its two merged-arm indexes and the timeline's attribution_* columns, once no
-- release still serving reads them. Rolling back past this migration is an image rollback alone.
create function attribution_database_refusal() returns text
language plpgsql stable
set search_path = pg_catalog
as $guard$
declare
  db pg_catalog.pg_database%rowtype;
begin
  select * into db from pg_catalog.pg_database d where d.datname = pg_catalog.current_database();
  if db.encoding <> pg_catalog.pg_char_to_encoding('UTF8') then
    return pg_catalog.format('dispatch: database %s has encoding %s, and the attribution of a pull request needs UTF8',
      pg_catalog.current_database(), pg_catalog.pg_encoding_to_char(db.encoding));
  end if;
  if pg_catalog.lower('ABCDEFGHIJKLMNOPQRSTUVWXYZ') <> 'abcdefghijklmnopqrstuvwxyz'
     or pg_catalog.upper('abcdefghijklmnopqrstuvwxyz') <> 'ABCDEFGHIJKLMNOPQRSTUVWXYZ' then
    return pg_catalog.format('dispatch: database %s has collation %s (provider %s, ctype %s, locale %s), which folds an ASCII letter in another way than "C" does',
      pg_catalog.current_database(), db.datcollate, db.datlocprovider, db.datctype, coalesce(pg_catalog.to_jsonb(db)->>'datlocale', pg_catalog.to_jsonb(db)->>'daticulocale', ''));
  end if;
  return null;
end
$guard$;

do $guard$
declare
  refusal text;
begin
  execute pg_catalog.format('select %I.attribution_database_refusal()', pg_catalog.current_schema()) into refusal;
  if refusal is not null then
    raise exception '%', refusal;
  end if;
end
$guard$;

alter table pull_request_heads
  add column attribution_refused_at timestamptz,
  add column attribution_absent_at timestamptz;

alter table delivery_pull_requests add column facts_refused_at timestamptz;

create or replace function delivery_pull_requests_changed() returns trigger language plpgsql as $$
declare
  ignored constant text[] := array['updated_at', 'attribution_checked_at', 'facts_refused_at'];
begin
  if tg_op = 'INSERT' then
    perform delivery_change_log('pull_request', null, new.repo, new.number::text, null, new.merged_at);
  elsif tg_op = 'DELETE' then
    perform delivery_change_log('pull_request', null, old.repo, old.number::text, old.merged_at, null);
  elsif (to_jsonb(old) - ignored) is distinct from (to_jsonb(new) - ignored) then
    perform delivery_change_log('pull_request', null, new.repo, new.number::text, old.merged_at, new.merged_at);
  end if;
  return null;
end $$;

create or replace function pull_request_heads_unread_write() returns trigger language plpgsql as $$
begin
  if old.github_read_at is not null
     and new.github_read_at is not distinct from old.github_read_at
     and (new.head_sha, new.head_ref, new.base_ref, new.state, new.title, new.url, new.event_at)
         is distinct from
         (old.head_sha, old.head_ref, old.base_ref, old.state, old.title, old.url, old.event_at)
  then
    new.github_read_at := null;
    new.checked_at := null;
    new.attribution_rule := 0;
    insert into pull_request_reads_owed (repo, number, reason) values (new.repo, new.number, 'failure')
    on conflict (repo, number) do update
      set reason = excluded.reason, owed_at = excluded.owed_at, revision = excluded.revision;
  end if;
  return new;
end $$;

create table pull_request_texts (
  repo text not null,
  number integer not null,
  head_sha text not null,
  head_ref text not null,
  head_repo text not null,
  base_ref text not null,
  title text not null,
  body text not null,
  commit_messages text[],
  updated_at timestamptz,
  read_at timestamptz not null default clock_timestamp(),
  title_keys text[] not null default '{}',
  cited_issues text[] not null default '{}',
  branch_keys text[] not null default '{}',
  commit_keys text[] not null default '{}',
  reading text not null default '',
  primary key (repo, number),
  foreign key (repo, number) references pull_request_heads (repo, number) on delete cascade on update cascade
);

create index pull_request_texts_keys on pull_request_texts
  using gin (branch_keys, title_keys, commit_keys, cited_issues);
create index pull_request_texts_reading on pull_request_texts (reading);
create index pull_request_heads_number on pull_request_heads (number);
create index pull_request_heads_url_lower on pull_request_heads (lower(url)) where octet_length(url) <= 1024;
create index delivery_pull_requests_number on delivery_pull_requests (number);
create index delivery_pull_requests_issue on delivery_pull_requests (issue_key) where issue_key is not null;

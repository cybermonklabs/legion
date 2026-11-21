-- 20261121060636_search_document_text.up.sql
-- A document's search text removes its ask blocks before search_text, not after, so Postgres's
-- text-search parser never gets a run search_text did not break.
--
-- search_text (0056) puts a space after every sixteenth underscore-joined segment of a run,
-- because the parser reads a run such as `a_a_a_…` again from each letter in it. The document
-- vector (0058, 0087's artifact_versions_search) and the spec's part of the issue vector
-- (20261105145242's issue_search_vector) applied search_text and then removed the ask blocks, and
-- the removal joins the two runs on each side of a block into one that nothing breaks again. 1,000
-- copies of `a_` fifteen times and `:::ask{}:::` (41,000 bytes) gave the parser one run of 15,000
-- segments: the insert of an issue's primary document version took 12 s, its document vector and
-- its issue vector 5 s each (250, 500 and 1,000 copies: 0.65, 2.6 and 12 s, 3.9 and 4.7 times for
-- each doubling), and each vector, the blocks removed first, 8 to 18 ms.
--
-- search_document_text(markdown) is the one definition of that text, the ask blocks removed and
-- then search_text: both functions below call it, and so do the related-item picker and the
-- duplicate check (api/related_terms.go relatedTermsSQL, api/duplicates.go
-- duplicateEqualLexemesQuery), which build the same text to compare with stored vectors. It is
-- STRICT, as both functions it joins are, so a version with no markdown keeps a null vector.
--
-- No stored row is written. The two orders give a different text only where text the ask-block
-- pattern matches has a run character on each side of it, so that removing it joins two runs.
-- Dispatch writes a real ask block on a line of its own, at most after a list's indentation or a
-- quote's `> `, so only `:::ask{…}:::` written inside a run, in a paragraph or a code block, is
-- indexed differently; and where the parser reads the joined run as words, as it reads `a_a_…`,
-- the two texts give the same vector. Where it reads the run as one host name, path or URL, the
-- vector differs, in that run's lexemes and in the positions of the words after it. Such a row
-- keeps its vector until its document's next version, and while the release before this one still
-- serves, its picker and duplicate check build their text in the old order. create or replace
-- keeps each trigger bound to its function, so the migration takes no lock on any table.
create function search_document_text(markdown text) returns text
language sql immutable parallel safe strict as $$
  select search_text(regexp_replace(markdown, '(?s):::ask\{.*?:::', '', 'g'))
$$;

create or replace function artifact_versions_search() returns trigger language plpgsql as $$
begin
  new.search := search_vector('', search_document_text(new.markdown));
  return new;
end $$;

create or replace function issue_search_vector(p_key text, p_title text, p_spec text) returns tsvector
language plpgsql immutable as $$
declare
  head tsvector := setweight(search_vector(search_text(p_key), search_text(p_title)), 'A');
  body text := search_document_text(coalesce(p_spec, ''));
  kept integer := length(body);
  part text := body;
begin
  loop
    begin
      return head || setweight(to_tsvector('english', part), 'C');
    exception when program_limit_exceeded then
      if kept = 0 then
        raise;
      end if;
      kept := kept / 2;
      -- One character past the cut, so a word that runs past it is dropped whole (0087).
      part := regexp_replace(left(body, kept + 1), '\S*$', '');
    end;
  end loop;
end $$;

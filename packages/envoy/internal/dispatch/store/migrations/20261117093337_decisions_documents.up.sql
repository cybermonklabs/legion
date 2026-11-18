-- 20261117093337_decisions_documents.up.sql — each issue's decisions document that stands.
--
-- The decisions step of dispatch://INFRA-1150 (docs.DecisionStep, its third phase) restores the
-- answered asks an agent moved out of an issue's spec and converts every section of the issue's
-- decisions.md into a node, then retires that document (docs.RetireDocument). It finds the
-- documents it owes that work through this one function, and so do its count of what it owes,
-- the census's lines and the two commands that run it by hand (graphcensus.ReadRestoreWork):
-- the step's Go code spells no slug and no role of the document, so the rule is written once.
--
-- decisions_documents() answers (id, issue_key, ref_key) of each document of an issue that plays
-- the role decisions, or, for a document stored before the role existed, whose role is null and
-- whose slug is decisions-md, and that is not retired (artifacts.retired_at null): a retired
-- document holds nothing the step owes, and its retirement cleared its role, so its slug alone
-- would otherwise find it again. A document a project holds, and a file, is none.
--
-- decisions_documents(include_retired => true) answers the retired ones too, found by that slug:
-- the release before gave the role decisions to an issue's document under that slug alone,
-- so every decisions document carries that slug, and the retirement leaves it. The census's
-- dangling-citation counter reads it so (graphcensus.danglingRead): a block address a retired
-- decisions document held, cited where no moved_to tuple leads from it, still dangles.
--
-- Rolling deploy. A new function and nothing else: it writes no row, reads no row while it
-- applies (a sql function's body is checked against the catalog, not run), and changes no table.
-- The release before this one names no decisions_documents, so nothing it runs changes. Rolling
-- back past this migration is an image rollback alone; the function left behind is called by
-- nothing that release runs. The root's own pull request drops it, one release later, with the
-- step that calls it (dispatch://INFRA-1150 R11).
-- kept for one release: decisions-documents-fn
create function decisions_documents(include_retired boolean default false)
  returns table (id uuid, issue_key text, ref_key text)
  language sql stable
  as $$
    select a.id, a.issue_key, a.ref_key
    from artifacts a
    where a.issue_key is not null and a.kind = 'doc' and (a.retired_at is null or include_retired)
      and (a.role = 'decisions' or (a.role is null and a.slug = 'decisions-md'))
  $$;

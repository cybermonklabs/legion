-- 20261123024730_eval_charts_answer.up.sql
--
-- An eval's declared charts and its answer (dispatch://INFRA-2918, R20; root dispatch://INFRA-979,
-- R38) are columns of its versioned definition, so a change of either writes the next version and
-- every version a reader once saw keeps its own:
--   charts   the list of chart declarations the Evals tab draws the eval's readings by, as a
--            register write gave them, each list accepted by the eval platform's validate_charts
--            rules before it is stored (evals/charts); '[]' for an eval that declares none.
--            eval_versions_charts_array holds it to a list.
--   answer   a short text that answers the eval's question, which a page draws beside its number;
--            '' for none.
-- The eval's detail read answers both; the list reads and the live evals view answer answer and
-- never charts, so no first paint carries a chart's declaration.
--
-- A version carries both forward where its insert gives neither: eval_versions_carry_forward, a
-- trigger before each insert, sets a null charts or answer from the newest earlier version of the
-- same eval, or to '[]' and '' for its first. The columns have no default, so the release before
-- this one, whose insert names neither column, writes nulls the trigger fills, and a version it
-- writes during the roll, or after a rollback, keeps the eval's charts and answer; this release
-- names both, and sends a null charts on a version whose write sets none (evals.definitionInsert).
-- A before trigger runs ahead of the not null check, so no insert stores a null.
--
-- evals_current is stated again so its rows carry the two columns: the `*` of its query was
-- expanded when 20261117100005 created it. `create or replace view` keeps every column the view
-- had, in its place, and appends the two, so the release before this one, which names the columns
-- it reads, reads the view as before.
--
-- Each column is added with a constant default, which Postgres writes into the catalog alone and
-- every existing version reads ('[]' and ''), and the default is then dropped, which changes only
-- what a later insert writes. The check is a statement of its own, added not valid: inline, it
-- would read every row of the table while the migration holds the table's ACCESS EXCLUSIVE lock,
-- and not valid still refuses every row written after it, where every row before it holds '[]'.
-- The migration takes eval_versions' and evals_current's ACCESS EXCLUSIVE for its short
-- transaction, so a registry write and read wait behind it; it refuses and rewrites no row, and its
-- census answers 0.
alter table eval_versions
    add column charts jsonb not null default '[]'::jsonb,
    add column answer text not null default '';

alter table eval_versions
    alter column charts drop default,
    alter column answer drop default;

alter table eval_versions
    add constraint eval_versions_charts_array check (jsonb_typeof(charts) = 'array') not valid;

create function eval_versions_carry_forward() returns trigger language plpgsql as $$
declare
  previous_charts jsonb;
  previous_answer text;
begin
  if new.charts is null or new.answer is null then
    select charts, answer into previous_charts, previous_answer
    from eval_versions
    where project_key = new.project_key and eval_id = new.eval_id and version < new.version
    order by version desc
    limit 1;
    new.charts := coalesce(new.charts, previous_charts, '[]'::jsonb);
    new.answer := coalesce(new.answer, previous_answer, '');
  end if;
  return new;
end $$;

create trigger eval_versions_carry_forward before insert on eval_versions
  for each row execute function eval_versions_carry_forward();

create or replace view evals_current as
    select distinct on (project_key, eval_id) *
    from eval_versions
    order by project_key, eval_id, version desc;

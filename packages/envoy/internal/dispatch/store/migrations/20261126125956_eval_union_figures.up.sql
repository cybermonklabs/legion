-- 20261126125956_eval_union_figures.up.sql
--
-- The mark of how far a project's union cut is folded (dispatch://INFRA-978, R14; plan T19 path 2;
-- evals.unionDeployRepo). This release stores, beside each deploy repository's own figures, the
-- fold of all a project's current deploy repositories, under deploy_repo '*' of eval_range_figures
-- and eval_day_figures, which a read of an eval keyed by the deploy repository looks up. The
-- release before folds no such row, and marks a project's figures built (eval_figures_built) all
-- the same, so on its database a read of '*' would find no row and answer "no data" for figures no
-- build folded. eval_figures_built.union_through is the last end day through which the project's
-- union cut is folded by a build that folds it: a read looks up the union rows of a range that ends
-- on or before it, and folds the day rows of any other (evals.ReadStoredFiguresIn). Null, as on
-- every row this migration finds, is folded through no day.
--
-- Only this release's code writes the column: the first whole figures walk of a project sets it
-- with the figures' mark, each later batch of the walk moves it on where it stood at the day before
-- the batch's first end day, and a rebuild's flip moves it to the end day the rebuild folded the
-- project's figures through (evals.markFiguresBuilt, evals' flipStatements). The release before
-- names it in no statement: its insert of a project's mark leaves it null, and its update of
-- through leaves it as it stands, so no write of the release before marks a union cut folded.
--
-- The release before deletes union rows, though: its fold of a range or a day, which builds no
-- union cut, deletes each stored row of the end days and days it folds that it did not build. So a
-- statement that deletes union rows moves each project's mark back before the earliest of them (a
-- range row's end day less one, a day row's day, which every window that holds it ends after),
-- unless its transaction says it folds union cuts: the transaction-local setting
-- dispatch.union_figures 'on', which this release's writes of the stored figures set before they
-- delete a union row (evals.writeAfterRanges, evals.flipRebuild). The refresher of this release
-- then folds the project's figures again from the mark on and moves the mark with them
-- (evals.walkFigures). The triggers read the rows the statement deleted (gone), the transition
-- table the figures tables' rebuild triggers (20261120072042) already keep of every delete.
--
-- A nullable column with no default rewrites no row and takes ACCESS EXCLUSIVE on
-- eval_figures_built for an instant; the triggers take SHARE ROW EXCLUSIVE on the two figures
-- tables for the migration's short transaction. The release before reads and writes every column
-- it names as it did. Its census answers 0: nothing is read or refused.
alter table eval_figures_built add column union_through date;

create function eval_union_ranges_gone() returns trigger language plpgsql as $$
begin
    if current_setting('dispatch.union_figures', true) is distinct from 'on' then
        update eval_figures_built b set union_through = lowest.kept
        from (select project_key, min(end_day) - 1 as kept from gone where deploy_repo = '*' group by project_key) lowest
        where b.project_key = lowest.project_key and b.union_through > lowest.kept;
    end if;
    return null;
end
$$;

create function eval_union_days_gone() returns trigger language plpgsql as $$
begin
    if current_setting('dispatch.union_figures', true) is distinct from 'on' then
        update eval_figures_built b set union_through = lowest.kept
        from (select project_key, min(day) as kept from gone where deploy_repo = '*' group by project_key) lowest
        where b.project_key = lowest.project_key and b.union_through > lowest.kept;
    end if;
    return null;
end
$$;

create trigger eval_range_figures_union_gone after delete on eval_range_figures
  referencing old table as gone for each statement execute function eval_union_ranges_gone();
create trigger eval_day_figures_union_gone after delete on eval_day_figures
  referencing old table as gone for each statement execute function eval_union_days_gone();

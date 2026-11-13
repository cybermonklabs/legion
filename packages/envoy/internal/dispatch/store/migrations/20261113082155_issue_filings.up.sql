-- 20261113082155_issue_filings.up.sql
--
-- The issues Legion sessions filed, one row per filed issue (dispatch://INFRA-2916, D5), which the
-- splitting eval's day rows count by filing day and by class. The Legion facts consumer writes a
-- row when the filing's `issue.created` event reaches it, classified by the rule legion/metrics
-- classifies a filing by, so a day row is built from stored rows and no read walks the event log.
--   issue_key       the filed issue, and the row's identity.
--   project_key     the project whose Legion session filed it: the project a phase worker's
--                   issue belongs to, or the controller's own project (legion.SessionTitle's
--                   OfProject). Splitting counts a filing for that project, wherever the filed
--                   issue lives, so an issue filed into another project is a filing of the
--                   filer's project, classified there, and no filing of the project holding it.
--   filed_at        when it was filed: the time of its `issue.created` event.
--   filer_role      the Legion role whose session filed it, or controller.
--   filer_issue     the issue that role was working, empty for the controller, which works none.
--   filer_phase     the phase the filer's issue was in when it filed, named by the bar it falls
--                   on, and empty where the issue's timeline holds no phase at that moment.
--   class           in_tree_at_design, in_tree_later, out_of_tree or by_the_controller, the four
--                   classes of legion/metrics. The check holds the column to them, so a filing
--                   counted under a class the evals do not list is refused rather than lost.
--   root_key        the root of the filed issue's tree as it stands now, which the consumer
--                   rewrites when that tree moves, as it does issue_facts.root_key, so the roots
--                   and children cuts read the tree of today and not the tree at the filing.
-- issue_filings_day, (project_key, filed_at), is the read of one project's filings of one day,
-- which is what a day row of the splitting eval counts.
--
-- One new table: nothing existing is read or rewritten, and the release before this one neither
-- reads nor writes it, so the two builds roll together (cmd/dispatch/README.md, "Database
-- migrations"). The foreign key to projects takes SHARE ROW EXCLUSIVE on projects for the length
-- of this transaction, which holds a project write and no project read. The census answers 0, and
-- rolling back past it is an image rollback alone.
create table issue_filings (
    issue_key text primary key,
    project_key text not null references projects (key) on delete cascade,
    filed_at timestamptz not null,
    filer_role text not null,
    filer_issue text not null default '',
    filer_phase text not null default '',
    class text not null check (class in ('in_tree_at_design', 'in_tree_later', 'out_of_tree', 'by_the_controller')),
    root_key text not null default ''
);
create index issue_filings_day on issue_filings (project_key, filed_at);

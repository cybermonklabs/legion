-- 20261110214252_issue_design_gates.up.sql — the document each design-gate step records on an issue.
--
-- INFRA-1727: a project's row names the steps the Legion deployment that serves it gates, in the
-- order it gates them (projects.gated_steps, 20261105204141_project_gated_steps.up.sql). This
-- table names, issue by issue, which document each of those steps records there: `decisions` the
-- issue's decisions document, `criteria` its Verification document, `spec` its primary document.
-- Dispatch's `superseded_approval` write advice reads it, so a design-gate clause is printed for
-- a document a gate on that issue really records and for no other.
--
-- The key is (issue, step): a step records one document of an issue, and the daemon publishes an
-- issue's whole set at once (PUT /api/v1/issues/{key}/design-gates, api/issue_design_gates.go).
-- The issue cascades, since the record has nothing to say once the issue it is about is gone.
-- The check holds step to the three steps Legion knows, as projects.gated_steps' own check does,
-- so a misspelt step is refused where it is written rather than read as a step nobody gates.
--
-- artifact_id carries no foreign key, because the row is no claim about this issue's own
-- documents: a hand-over records a root issue's document on a child, so the id need not be a
-- document of the issue that records it, and the table would have to hold a key that says
-- nothing about where the document belongs for the one reader that reads the id — the advice,
-- which looks the document up itself and prints the clause only when it finds the one it is
-- writing about.
--
-- The row is the daemon's own fact, not a person's: it is what that deployment's gate recorded,
-- republished whenever the daemon says so. Nothing but that daemon writes here, and the write
-- appends no issue event (api/issue_design_gates.go says why), so the table grows no event log
-- of its own.
--
-- A new table no earlier release names: nothing existing is read or rewritten, and the release
-- before this one neither reads nor writes it, so it rolls (cmd/dispatch/README.md, "Database
-- migrations") and the census answers 0. The foreign key holds issues in SHARE ROW EXCLUSIVE for
-- the migration's short transaction, so an issue write waits behind it while a read does not.
-- The identifier is the UTC moment this was written, as `migrationid` requires of every
-- migration past the numbered series, which closed at 0161 (cmd/dispatch/README.md, "Database
-- migrations").
create table design_gates (
  issue text not null references issues(key) on delete cascade,
  step text not null check (step in ('decisions', 'criteria', 'spec')),
  artifact_id text not null,
  primary key (issue, step)
);

-- The advice reads the table by document, not by issue: `superseded_approval` asks which issues'
-- gates record the document it is writing about (api/write_advice.go, supersededApprovalGates),
-- and the primary key answers nothing about artifact_id. That read runs inside the advice's
-- savepoint under a 500 ms statement_timeout, where a scan of every gate row would cost the whole
-- gate clause the advice exists to print, so the one column that reader filters by holds an index
-- of its own. The table is new and empty, so the index is built with it.
create index design_gates_artifact_id_idx on design_gates (artifact_id);

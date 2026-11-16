-- 20261116160000_spec_term_arrivals.up.sql
--
-- The move that brought a covered document under a root of The Spec, which the list of terms of
-- that root orders the document by (api/spec_form.go, approvedSpecTexts).
--
--   root         the key of the named root (DISPATCH_SPEC_ROOTS) whose list the document joined.
--   artifact_id  the spec or the Verification document of an issue that the move brought under
--                that root, one row for each root at most.
--   event_id     the id of the move's issue.updated event (events.id). The move appends it under
--                the lock of the root's list (lockSpecTerms), so the ids of moves and of the
--                approvals that the list reads are in the order in which they joined the list.
--
-- The list holds one definition of each term: the definition of the document that joined the
-- list first, at the later of its first approval and the move that brought it under the root. A
-- move inside the root writes no row, so it changes no order. A move out of the root deletes the
-- row. A document with no row joined at its first approval.
--
-- A new table: no existing row is read, refused or rewritten. The foreign key takes SHARE ROW
-- EXCLUSIVE on artifacts for the migration's short transaction, which holds artifacts writes
-- behind it for that long and no read, within pgmigrate.LockTimeout's five seconds; its on delete
-- cascade keeps that release's document deletes working, a deleted document taking its rows with
-- it. No foreign key names events, whose writes the migration does not hold.
--
-- The release before this one neither reads nor writes the table, so the two roll side by side
-- (cmd/dispatch/README.md, "Database migrations"). A move by that release writes no row, so the
-- document it moves joins the list at its first approval, as every document did before. Rolling
-- back past this migration is an image rollback alone; the table left behind is read by nothing.
create table spec_term_arrivals (
  root text not null,
  artifact_id uuid not null references artifacts(id) on delete cascade,
  event_id bigint not null,
  primary key (root, artifact_id)
);

-- 20261110235352_merge_marks.up.sql — the two durable marks a controller sets on a project's
-- merges, so that the daemon's submission at READY can jump GitHub's merge queue for one issue and
-- can be held back for a path the project is protecting (INFRA-1578).
--
-- A mark lives in the record and not in the controller's memory because the submission it governs
-- is made by an outbox row, minutes or hours after the controller set the mark, by whichever
-- daemon process holds the row then. The controller session that set it is gone by then, and a
-- restart of the daemon must change nothing. Each table below is read inside the transaction that
-- decides one submission, so a mark and the submission it governs commit or roll back together.
--
-- merge_jumps is the per-issue jump mark: at most one row per issue, which is why the issue is the
-- primary key rather than a column of a log. A jump is a decision about one issue's next merge,
-- never a history, so a second set of the mark replaces the first (record.Store.SetMergeJump) and
-- nothing accumulates. reason and session record who asked for it and why, which is what the
-- record of the jump (merge_jump_records) repeats to anybody who asks later why a pull request
-- went ahead of the queue. A row goes in three ways: the daemon spends it when it submits the
-- jumping merge (ClearMergeJump), a controller takes the mark off, and the issue's own record is
-- deleted — the `on delete cascade` above, since a mark on an issue the daemon no longer records
-- can govern no submission.
--
-- merge_holds is the project's hold list. The list is replaced whole on every write
-- (ReplaceMergeHolds), so a controller sends the list it wants to stand rather than editing one
-- row of a list it has to name. That is why a hold carries no identifier of its own: ordinal is
-- the list's own order, assigned from 0 at each replacement, and it never leaves the daemon, so no
-- controller can hold a reference to a row that the next replacement renumbers. paths holds the
-- hold's unanchored regular expressions, matched against the paths a pull request changes;
-- text[] rather than one text per row because the patterns of one hold are one decision with one
-- reason and one end time, and splitting them over rows would let a replacement land half applied.
-- repository is 'owner/name', and '' — the default — is every repository the project serves, so a
-- hold that names no repository needs no list of them. ends_at is when the hold stops: the daemon
-- deletes a row at or past its end when it reads the list (MergeHolds), so a controller that reads
-- the list before it replaces it sees only the holds still in force and never copies a dead one
-- forward. A row also goes with the next replacement of the project's list.
--
-- merge_jump_records keeps the newest jump of each repository, which is why the primary key is
-- (project, repository): what a reader wants is what jumped this repository's queue last, and a
-- record per repository answers it without a scan. entries is the queue as it stood before the
-- jump, a JSON array of entries (record.MergeQueueEntryRecord), with entries_total the queue's
-- length and entries_unknown set when GitHub did not answer the queue at all: a record that could
-- not read the queue says so rather than reading as an empty queue. refused and refusal carry a
-- jump GitHub would not take, so the mark being spent is never mistaken for the merge having
-- jumped. A jsonb array rather than a table of entries because nothing queries one entry: the
-- whole snapshot is written once and read whole.
--
-- merge_jump_records.issue deliberately carries no foreign key to issues (key). The record of what
-- a jump displaced outlives the issue record: an issue is deleted when its tree is cleaned up,
-- and the account of a queue that was jumped weeks ago must stay readable after that. The column
-- holds the issue key as text, and a reader that finds no issue of that key reads a jump of an
-- issue the daemon no longer records.
--
-- There is no down file: migrations here are forward-only. A rollback of the image leaves the
-- three tables standing and empty of meaning, which costs a release that names none of them
-- nothing.
create table merge_jumps (
  issue text primary key references issues (key) on delete cascade,
  reason text not null,
  session text not null,
  set_at timestamptz not null);

create table merge_holds (
  project text not null,
  ordinal int not null,
  paths text[] not null,
  repository text not null default '',
  reason text not null,
  ends_at timestamptz not null,
  session text not null,
  set_at timestamptz not null,
  primary key (project, ordinal));

create table merge_jump_records (
  project text not null,
  repository text not null,
  issue text not null,
  number int not null,
  head text not null,
  at timestamptz not null,
  reason text not null,
  entries jsonb not null,
  entries_total int not null,
  entries_unknown boolean not null,
  refused boolean not null,
  refusal text not null,
  primary key (project, repository));
